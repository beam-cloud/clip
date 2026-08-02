package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	log "github.com/rs/zerolog/log"
	"golang.org/x/sync/errgroup"
)

const (
	parallelBlobPullThreshold   int64 = 1 << 30 // 1 GiB
	parallelBlobPullConcurrency       = 16
	parallelBlobPullPartSize    int64 = 256 << 20 // 256 MiB
	parallelBlobPullDiskReserve int64 = 1 << 30   // 1 GiB
	parallelBlobPullAttempts          = 3
)

var errParallelBlobRangeUnsupported = errors.New("registry blob range request unsupported")

type parallelBlobPullConfig struct {
	inner          http.RoundTripper
	registryHost   string
	digest         string
	size           int64
	tempDir        string
	minimumFree    int64
	threshold      int64
	partSize       int64
	concurrency    int
	attempts       int
	retryBackoff   time.Duration
	availableBytes func(string) (int64, error)
}

// parallelBlobTransport turns one large authenticated registry blob GET into
// bounded, validated range GETs in a temporary local file. Subrequests follow
// ECR's cross-host signed redirect with Range intact; Go strips the registry
// Authorization header from that redirect. The returned file still flows
// through go-containerregistry's compressed digest verifier and CLIP's existing
// decompressed digest verifier.
type parallelBlobTransport struct {
	parallelBlobPullConfig
}

func (s *OCIClipStorage) parallelBlobTransport(digest string) http.RoundTripper {
	size := s.compressedLayerSize(digest)
	if size < parallelBlobPullThreshold {
		return nil
	}
	uncompressedSize := s.estimatedUncompressedLayerSize(digest)
	if uncompressedSize <= 0 {
		// Parallel prefetch temporarily stores both representations. Without a
		// trustworthy indexed size, preserve the single-stream disk footprint.
		return nil
	}

	return newParallelBlobTransport(parallelBlobPullConfig{
		inner:          remote.DefaultTransport,
		registryHost:   normalizedRegistryHost(s.storageInfo.RegistryURL),
		digest:         digest,
		size:           size,
		tempDir:        s.diskCacheDir,
		minimumFree:    saturatingAdd(size, uncompressedSize, parallelBlobPullDiskReserve),
		threshold:      parallelBlobPullThreshold,
		partSize:       parallelBlobPullPartSize,
		concurrency:    parallelBlobPullConcurrency,
		attempts:       parallelBlobPullAttempts,
		retryBackoff:   100 * time.Millisecond,
		availableBytes: filesystemAvailableBytes,
	})
}

func (s *OCIClipStorage) fetchLayerByDigestWithTransport(ctx context.Context, digest string, transport http.RoundTripper) (v1.Layer, error) {
	layerRef := fmt.Sprintf("%s/%s@%s", s.storageInfo.RegistryURL, s.storageInfo.Repository, digest)
	ref, err := name.NewDigest(layerRef)
	if err != nil {
		return nil, fmt.Errorf("failed to parse layer digest reference %q: %w", layerRef, err)
	}
	opts := append(s.remoteOptions(ctx), remote.WithTransport(transport))
	layer, err := remote.Layer(ref, opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch layer by digest %s: %w", digest, err)
	}
	return layer, nil
}

func newParallelBlobTransport(config parallelBlobPullConfig) http.RoundTripper {
	if config.inner == nil {
		config.inner = http.DefaultTransport
	}
	if config.threshold <= 0 {
		config.threshold = parallelBlobPullThreshold
	}
	if config.partSize <= 0 {
		config.partSize = parallelBlobPullPartSize
	}
	if config.concurrency <= 0 {
		config.concurrency = parallelBlobPullConcurrency
	}
	if config.attempts <= 0 {
		config.attempts = parallelBlobPullAttempts
	}
	if config.availableBytes == nil {
		config.availableBytes = filesystemAvailableBytes
	}
	return &parallelBlobTransport{parallelBlobPullConfig: config}
}

func (t *parallelBlobTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if !t.eligible(req) {
		return t.inner.RoundTrip(req)
	}

	available, err := t.availableBytes(t.tempDir)
	if err != nil || (t.minimumFree > 0 && available < t.minimumFree) {
		log.Debug().
			Err(err).
			Int64("available_bytes", available).
			Int64("required_bytes", t.minimumFree).
			Str("layer_digest", t.digest).
			Msg("parallel registry blob prefetch skipped: insufficient disk headroom")
		return t.inner.RoundTrip(req)
	}

	resp, err := t.prefetch(req)
	if err == nil {
		return resp, nil
	}
	if ctxErr := req.Context().Err(); ctxErr != nil {
		return nil, ctxErr
	}

	log.Warn().
		Err(err).
		Str("layer_digest", t.digest).
		Int64("compressed_bytes", t.size).
		Msg("parallel registry blob prefetch failed; falling back to single stream")
	return t.inner.RoundTrip(req)
}

func (t *parallelBlobTransport) eligible(req *http.Request) bool {
	if req == nil || req.URL == nil || req.Method != http.MethodGet || req.Header.Get("Range") != "" {
		return false
	}
	if t.size < t.threshold || t.digest == "" || t.tempDir == "" {
		return false
	}
	if t.registryHost != "" && !strings.EqualFold(req.URL.Host, t.registryHost) {
		return false
	}
	return strings.HasSuffix(req.URL.EscapedPath(), "/blobs/"+url.PathEscape(t.digest)) ||
		strings.HasSuffix(req.URL.Path, "/blobs/"+t.digest)
}

func (t *parallelBlobTransport) prefetch(req *http.Request) (*http.Response, error) {
	tempFile, err := os.CreateTemp(t.tempDir, ".clip-oci-compressed-*.tmp")
	if err != nil {
		return nil, fmt.Errorf("create compressed layer prefetch file: %w", err)
	}
	tempPath := tempFile.Name()
	keep := false
	defer func() {
		if !keep {
			_ = tempFile.Close()
			_ = os.Remove(tempPath)
		}
	}()

	if err := tempFile.Truncate(t.size); err != nil {
		return nil, fmt.Errorf("size compressed layer prefetch file: %w", err)
	}

	// Probe one byte before fan-out. A registry that ignores Range otherwise
	// risks sending the complete multi-gigabyte blob to every worker.
	if err := t.downloadRange(req.Context(), req, tempFile, 0, 0); err != nil {
		return nil, err
	}

	group, groupCtx := errgroup.WithContext(req.Context())
	var nextOffset atomic.Int64
	nextOffset.Store(1)
	remaining := t.size - 1
	partCount := remaining / t.partSize
	if remaining%t.partSize != 0 {
		partCount++
	}
	workers := t.concurrency
	if int64(workers) > partCount {
		workers = int(partCount)
	}
	for i := 0; i < workers; i++ {
		group.Go(func() error {
			for {
				start := nextOffset.Add(t.partSize) - t.partSize
				if start >= t.size {
					return nil
				}
				end := start + t.partSize - 1
				if end < start || end >= t.size {
					end = t.size - 1
				}
				if err := t.downloadRange(groupCtx, req, tempFile, start, end); err != nil {
					return err
				}
			}
		})
	}
	if err := group.Wait(); err != nil {
		return nil, err
	}
	if err := req.Context().Err(); err != nil {
		return nil, err
	}
	if _, err := tempFile.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("rewind compressed layer prefetch file: %w", err)
	}

	keep = true
	header := make(http.Header)
	header.Set("Content-Length", strconv.FormatInt(t.size, 10))
	header.Set("Content-Type", "application/octet-stream")
	return &http.Response{
		Status:        "200 OK",
		StatusCode:    http.StatusOK,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        header,
		Body:          &removeOnCloseFile{File: tempFile, path: tempPath},
		ContentLength: t.size,
		Request:       req,
	}, nil
}

func (t *parallelBlobTransport) downloadRange(ctx context.Context, original *http.Request, dest *os.File, start, end int64) error {
	want := end - start + 1
	var lastErr error
	for attempt := 0; attempt < t.attempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		if attempt > 0 && t.retryBackoff > 0 {
			delay := t.retryBackoff * time.Duration(1<<(attempt-1))
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}

		req := original.Clone(ctx)
		req.Header = original.Header.Clone()
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
		resp, err := (&http.Client{Transport: t.inner}).Do(req)
		if err != nil {
			lastErr = err
			continue
		}

		if resp.StatusCode != http.StatusPartialContent {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusRequestedRangeNotSatisfiable {
				return fmt.Errorf("%w: status %s", errParallelBlobRangeUnsupported, resp.Status)
			}
			lastErr = fmt.Errorf("range %d-%d returned status %s", start, end, resp.Status)
			continue
		}

		gotStart, gotEnd, gotTotal, err := parseContentRange(resp.Header.Get("Content-Range"))
		if err != nil || gotStart != start || gotEnd != end || gotTotal != t.size {
			_ = resp.Body.Close()
			return fmt.Errorf("%w: requested %d-%d/%d, got %q", errParallelBlobRangeUnsupported, start, end, t.size, resp.Header.Get("Content-Range"))
		}
		if resp.ContentLength >= 0 && resp.ContentLength != want {
			_ = resp.Body.Close()
			return fmt.Errorf("%w: range %d-%d content length %d, expected %d", errParallelBlobRangeUnsupported, start, end, resp.ContentLength, want)
		}

		written, copyErr := io.CopyN(io.NewOffsetWriter(dest, start), resp.Body, want)
		if copyErr == nil {
			var extra [1]byte
			n, readErr := resp.Body.Read(extra[:])
			if n != 0 || (readErr != nil && !errors.Is(readErr, io.EOF)) {
				copyErr = fmt.Errorf("range response exceeded declared length")
			}
		}
		closeErr := resp.Body.Close()
		if copyErr == nil && closeErr == nil && written == want {
			return nil
		}
		if copyErr != nil {
			lastErr = copyErr
		} else if closeErr != nil {
			lastErr = closeErr
		} else {
			lastErr = fmt.Errorf("short range write: wrote %d, expected %d", written, want)
		}
	}
	return fmt.Errorf("download range %d-%d after %d attempts: %w", start, end, t.attempts, lastErr)
}

func parseContentRange(value string) (start, end, total int64, err error) {
	if !strings.HasPrefix(value, "bytes ") {
		return 0, 0, 0, fmt.Errorf("invalid content range %q", value)
	}
	rangeAndTotal := strings.Split(strings.TrimPrefix(value, "bytes "), "/")
	if len(rangeAndTotal) != 2 || rangeAndTotal[1] == "*" {
		return 0, 0, 0, fmt.Errorf("invalid content range %q", value)
	}
	bounds := strings.Split(rangeAndTotal[0], "-")
	if len(bounds) != 2 {
		return 0, 0, 0, fmt.Errorf("invalid content range %q", value)
	}
	start, err = strconv.ParseInt(bounds[0], 10, 64)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("invalid content range start %q: %w", value, err)
	}
	end, err = strconv.ParseInt(bounds[1], 10, 64)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("invalid content range end %q: %w", value, err)
	}
	total, err = strconv.ParseInt(rangeAndTotal[1], 10, 64)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("invalid content range total %q: %w", value, err)
	}
	if start < 0 || end < start || total <= end {
		return 0, 0, 0, fmt.Errorf("invalid content range bounds %q", value)
	}
	return start, end, total, nil
}

type removeOnCloseFile struct {
	*os.File
	path string
}

func (f *removeOnCloseFile) Close() error {
	closeErr := f.File.Close()
	removeErr := os.Remove(f.path)
	if closeErr != nil {
		return closeErr
	}
	if errors.Is(removeErr, os.ErrNotExist) {
		return nil
	}
	return removeErr
}

func (s *OCIClipStorage) estimatedUncompressedLayerSize(digest string) int64 {
	if s == nil || s.storageInfo == nil || s.storageInfo.GzipIdxByLayer == nil {
		return 0
	}
	index := s.storageInfo.GzipIdxByLayer[digest]
	if index == nil {
		return 0
	}
	var maximum int64
	for _, checkpoint := range index.Checkpoints {
		if checkpoint.UOff > maximum {
			maximum = checkpoint.UOff
		}
	}
	if maximum <= 0 {
		return 0
	}
	return saturatingAdd(maximum, 1<<20)
}

func saturatingAdd(values ...int64) int64 {
	var total int64
	for _, value := range values {
		if value <= 0 {
			continue
		}
		if total > math.MaxInt64-value {
			return math.MaxInt64
		}
		total += value
	}
	return total
}

func filesystemAvailableBytes(path string) (int64, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, err
	}
	if stat.Bsize <= 0 || uint64(stat.Bavail) > math.MaxUint64/uint64(stat.Bsize) {
		return math.MaxInt64, nil
	}
	available := uint64(stat.Bavail) * uint64(stat.Bsize)
	if available > math.MaxInt64 {
		return math.MaxInt64, nil
	}
	return int64(available), nil
}

func normalizedRegistryHost(registry string) string {
	registry = strings.TrimSpace(registry)
	if registry == "" {
		return ""
	}
	if !strings.Contains(registry, "://") {
		registry = "//" + registry
	}
	parsed, err := url.Parse(registry)
	if err != nil {
		return ""
	}
	return parsed.Host
}
