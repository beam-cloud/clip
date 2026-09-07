package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
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
	// Every layer a container waits on is worth fanning out: a single registry
	// stream from a remote worker is bounded by one TCP flow to one blob-store
	// front-end, and some front-ends are an order of magnitude slower than
	// others from the same site.
	parallelBlobPullThreshold   int64 = 32 << 20 // 32 MiB
	parallelBlobPullConcurrency       = 8        // per layer
	parallelBlobPullPartSize    int64 = 32 << 20 // 32 MiB
	parallelBlobPullDiskReserve int64 = 1 << 30  // 1 GiB
	parallelBlobPullAttempts          = 3

	// Ranges across all layers materializing on this worker share one
	// connection budget so eight concurrent layers do not open 64 flows.
	parallelBlobPullGlobalConcurrency = 24

	// A range whose connection is starved is abandoned and its remaining bytes
	// re-fetched on a new connection (to a different front-end where possible)
	// instead of holding the whole layer hostage. See stragglerRule.
	parallelBlobStragglerGrace            = 3 * time.Second
	parallelBlobStragglerFloorPerSec      = 4 << 20 // 4 MiB/s
	parallelBlobStragglerMedianDivisor    = 6
	parallelBlobStragglerTailDivisor      = 4
	parallelBlobStragglerMaxAbortsPerPart = 3
	parallelBlobStragglerRateHistory      = 32
	parallelBlobSlowEndpointTTL           = 2 * time.Minute
)

var errParallelBlobRangeUnsupported = errors.New("registry blob range request unsupported")

// errParallelBlobStraggler marks an attempt the straggler monitor cut short.
var errParallelBlobStraggler = errors.New("registry blob range attempt abandoned as straggler")

var parallelBlobRangeSlots = make(chan struct{}, parallelBlobPullGlobalConcurrency)

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

	// stragglerGrace <= 0 disables the straggler monitor (tests).
	stragglerGrace time.Duration
	stragglerFloor int64
	// slots bounds concurrent range requests across transports; nil means the
	// package-wide budget.
	slots chan struct{}
	// markSlowEndpoint records the remote address of an abandoned attempt so
	// later dials avoid it. nil disables.
	markSlowEndpoint func(string)
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

// rangePart is one in-flight range attempt, observed by the straggler monitor.
type rangePart struct {
	start, end int64
	startedAt  time.Time
	// bytes is the progress of the current attempt; done is the total already
	// written to the file across attempts, so a resumed attempt only asks for
	// the remainder.
	bytes  atomic.Int64
	done   int64
	remote atomic.Pointer[string]
	cancel context.CancelFunc
	aborts atomic.Int32
}

func (p *rangePart) rate(now time.Time) (float64, time.Duration) {
	elapsed := now.Sub(p.startedAt)
	if elapsed <= 0 {
		return 0, 0
	}
	return float64(p.bytes.Load()) / elapsed.Seconds(), elapsed
}

// stragglerRule picks the in-flight parts to abandon. A part is a straggler
// when, past the grace period, it is below the absolute floor and far below
// the median rate of its progressing peers and recently completed parts. The
// relative test keeps a uniformly slow link from churning every connection;
// the floor keeps a lone tail part from crawling when there is nothing to
// compare against, and a part with no bytes at all past grace is always
// abandoned. In the tail (no parts left to hand out) idle workers have nothing
// better to do, so any part well below the reference rate is abandoned even
// above the floor; the remainder is resumed, so an abort costs a reconnect.
func stragglerRule(parts []*rangePart, completed []float64, tail bool, now time.Time, grace time.Duration, floor int64, maxAborts int) []*rangePart {
	rates := make([]float64, 0, len(parts)+len(completed))
	for _, part := range parts {
		if rate, elapsed := part.rate(now); elapsed >= time.Second && rate > 0 {
			rates = append(rates, rate)
		}
	}
	rates = append(rates, completed...)
	sort.Float64s(rates)
	var reference float64
	if len(rates) > 0 {
		reference = rates[len(rates)/2]
	}
	if tail {
		grace /= 2
	}
	var stragglers []*rangePart
	for _, part := range parts {
		rate, elapsed := part.rate(now)
		if elapsed < grace || int(part.aborts.Load()) >= maxAborts {
			continue
		}
		switch {
		case rate == 0:
		case tail && len(rates) >= 2 && rate < reference/parallelBlobStragglerTailDivisor:
		case rate < float64(floor) && (len(rates) < 2 || rate < reference/parallelBlobStragglerMedianDivisor):
		default:
			continue
		}
		stragglers = append(stragglers, part)
	}
	return stragglers
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
		inner:            blobRangeTransport(),
		registryHost:     normalizedRegistryHost(s.storageInfo.RegistryURL),
		digest:           digest,
		size:             size,
		tempDir:          s.diskCacheDir,
		minimumFree:      saturatingAdd(size, uncompressedSize, parallelBlobPullDiskReserve),
		threshold:        parallelBlobPullThreshold,
		partSize:         parallelBlobPullPartSize,
		concurrency:      parallelBlobPullConcurrency,
		attempts:         parallelBlobPullAttempts,
		retryBackoff:     100 * time.Millisecond,
		availableBytes:   filesystemAvailableBytes,
		stragglerGrace:   parallelBlobStragglerGrace,
		stragglerFloor:   parallelBlobStragglerFloorPerSec,
		slots:            parallelBlobRangeSlots,
		markSlowEndpoint: slowBlobEndpoints.mark,
	})
}

// slowEndpointSet remembers blob-store front-ends that starved a range so new
// dials prefer the others. Entries expire; a front-end is not slow forever.
type slowEndpointSet struct {
	mu        sync.Mutex
	ttl       time.Duration
	seen      map[string]time.Time
	closeIdle func()
}

var slowBlobEndpoints = &slowEndpointSet{ttl: parallelBlobSlowEndpointTTL, seen: map[string]time.Time{}}

func (s *slowEndpointSet) mark(remoteAddr string) {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	if host == "" {
		return
	}
	s.mu.Lock()
	s.seen[host] = time.Now().Add(s.ttl)
	s.mu.Unlock()
	if s.closeIdle != nil {
		// Keep-alive would otherwise hand the next range the same starved
		// connection. Re-dialing every pooled connection costs a handshake.
		s.closeIdle()
	}
}

func (s *slowEndpointSet) isSlow(ip string, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	until, ok := s.seen[ip]
	if !ok {
		return false
	}
	if now.After(until) {
		delete(s.seen, ip)
		return false
	}
	return true
}

// dial resolves addr and connects to a randomly chosen address that is not
// currently marked slow, so concurrent ranges spread over the blob store's
// front-ends instead of piling onto whichever one the resolver listed first.
func (s *slowEndpointSet) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	host, port, err := net.SplitHostPort(addr)
	if err != nil || net.ParseIP(host) != nil {
		return dialer.DialContext(ctx, network, addr)
	}
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil || len(ips) == 0 {
		return dialer.DialContext(ctx, network, addr)
	}
	now := time.Now()
	candidates := make([]net.IPAddr, 0, len(ips))
	for _, ip := range ips {
		if !s.isSlow(ip.String(), now) {
			candidates = append(candidates, ip)
		}
	}
	if len(candidates) == 0 {
		candidates = ips
	}
	rand.Shuffle(len(candidates), func(i, j int) { candidates[i], candidates[j] = candidates[j], candidates[i] })
	var lastErr error
	for _, ip := range candidates {
		conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		if err == nil {
			return conn, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			break
		}
	}
	return nil, lastErr
}

var (
	blobRangeTransportOnce sync.Once
	blobRangeTransportInst http.RoundTripper
)

// blobRangeTransport is the shared HTTP transport for range subrequests. It
// mirrors go-containerregistry's defaults but dials through slowBlobEndpoints
// and keeps enough idle connections for the range fan-out to reuse.
func blobRangeTransport() http.RoundTripper {
	blobRangeTransportOnce.Do(func() {
		base, ok := remote.DefaultTransport.(*http.Transport)
		if !ok {
			blobRangeTransportInst = remote.DefaultTransport
			return
		}
		transport := base.Clone()
		transport.DialContext = slowBlobEndpoints.dial
		transport.MaxIdleConnsPerHost = parallelBlobPullGlobalConcurrency * 2
		transport.MaxIdleConns = parallelBlobPullGlobalConcurrency * 4
		slowBlobEndpoints.closeIdle = transport.CloseIdleConnections
		blobRangeTransportInst = transport
	})
	return blobRangeTransportInst
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
		log.Warn().
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

	pullStart := time.Now()
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

	var (
		activeMu  sync.Mutex
		active    = map[*rangePart]struct{}{}
		completed []float64
		aborts    atomic.Int64
		retries   atomic.Int64
	)
	track := func(part *rangePart, on bool) {
		activeMu.Lock()
		if on {
			active[part] = struct{}{}
		} else {
			delete(active, part)
			if rate, elapsed := part.rate(time.Now()); elapsed > 0 && part.done == part.end-part.start+1 {
				completed = append(completed, rate)
				if len(completed) > parallelBlobStragglerRateHistory {
					completed = completed[1:]
				}
			}
		}
		activeMu.Unlock()
	}
	monitorDone := make(chan struct{})
	if t.stragglerGrace > 0 {
		go func() {
			ticker := time.NewTicker(t.stragglerGrace / 3)
			defer ticker.Stop()
			for {
				select {
				case <-monitorDone:
					return
				case <-groupCtx.Done():
					return
				case now := <-ticker.C:
					activeMu.Lock()
					parts := make([]*rangePart, 0, len(active))
					for part := range active {
						parts = append(parts, part)
					}
					tail := nextOffset.Load() >= t.size
					stragglers := stragglerRule(parts, completed, tail, now, t.stragglerGrace, t.stragglerFloor, parallelBlobStragglerMaxAbortsPerPart)
					for _, part := range stragglers {
						part.aborts.Add(1)
						part.cancel()
					}
					activeMu.Unlock()
				}
			}
		}()
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
				part := &rangePart{start: start, end: end}
				if err := t.downloadPart(groupCtx, req, tempFile, part, track, &aborts, &retries); err != nil {
					return err
				}
			}
		})
	}
	err = group.Wait()
	close(monitorDone)
	if err != nil {
		return nil, err
	}
	if err := req.Context().Err(); err != nil {
		return nil, err
	}
	elapsed := time.Since(pullStart)
	log.Info().
		Str("layer_digest", t.digest).
		Int64("compressed_bytes", t.size).
		Int64("parts", partCount).
		Int("concurrency", workers).
		Int64("straggler_aborts", aborts.Load()).
		Int64("retries", retries.Load()).
		Dur("duration", elapsed).
		Float64("mib_per_s", float64(t.size)/(1<<20)/elapsed.Seconds()).
		Msg("parallel registry blob prefetch complete")
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

// downloadRange fetches one range with the normal retry policy and without
// straggler supervision (used for the probe).
func (t *parallelBlobTransport) downloadRange(ctx context.Context, original *http.Request, dest *os.File, start, end int64) error {
	return t.downloadPart(ctx, original, dest, &rangePart{start: start, end: end}, nil, nil, nil)
}

// downloadPart fetches one range, retrying transient failures with backoff and
// immediately re-issuing attempts the straggler monitor abandons. Straggler
// aborts do not consume the transient-failure budget; they are bounded per part
// by parallelBlobStragglerMaxAbortsPerPart inside stragglerRule.
func (t *parallelBlobTransport) downloadPart(ctx context.Context, original *http.Request, dest *os.File, part *rangePart, track func(*rangePart, bool), aborts, retries *atomic.Int64) error {
	var lastErr error
	for attempt := 0; attempt < t.attempts; {
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

		if t.slots != nil {
			select {
			case t.slots <- struct{}{}:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		partCtx, cancel := context.WithCancel(ctx)
		part.startedAt = time.Now()
		part.bytes.Store(0)
		part.remote.Store(nil)
		part.cancel = cancel
		if track != nil {
			track(part, true)
		}
		err := t.downloadRangeAttempt(partCtx, original, dest, part)
		if track != nil {
			track(part, false)
		}
		// Only the straggler monitor cancels partCtx while ctx is alive; read
		// that before our own cancel() below makes partCtx.Err() non-nil.
		straggled := partCtx.Err() != nil && ctx.Err() == nil
		cancel()
		if t.slots != nil {
			<-t.slots
		}
		if err == nil {
			return nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if straggled {
			if aborts != nil {
				aborts.Add(1)
			}
			remote := ""
			if addr := part.remote.Load(); addr != nil {
				remote = *addr
			}
			if t.markSlowEndpoint != nil && remote != "" {
				t.markSlowEndpoint(remote)
			}
			rate, elapsed := part.rate(time.Now())
			log.Debug().
				Str("layer_digest", t.digest).
				Int64("range_start", part.start).
				Int64("range_end", part.end).
				Int64("resume_at", part.start+part.done).
				Str("remote", remote).
				Float64("mib_per_s", rate/(1<<20)).
				Dur("elapsed", elapsed).
				Int32("aborts", part.aborts.Load()).
				Msg("registry blob range straggler abandoned; resuming on a new connection")
			lastErr = errParallelBlobStraggler
			continue
		}
		if errors.Is(err, errParallelBlobRangeUnsupported) {
			return err
		}
		lastErr = err
		attempt++
		if retries != nil && attempt < t.attempts {
			retries.Add(1)
		}
	}
	return fmt.Errorf("download range %d-%d after %d attempts: %w", part.start, part.end, t.attempts, lastErr)
}

// downloadRangeAttempt performs a single validated range request into dest.
func (t *parallelBlobTransport) downloadRangeAttempt(ctx context.Context, original *http.Request, dest *os.File, part *rangePart) error {
	start, end := part.start+part.done, part.end
	want := end - start + 1
	if want <= 0 {
		return nil
	}

	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		GotConn: func(info httptrace.GotConnInfo) {
			if info.Conn != nil && info.Conn.RemoteAddr() != nil {
				addr := info.Conn.RemoteAddr().String()
				part.remote.Store(&addr)
			}
		},
	})
	req := original.Clone(ctx)
	req.Header = original.Header.Clone()
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
	resp, err := (&http.Client{Transport: t.inner}).Do(req)
	if err != nil {
		return err
	}

	if resp.StatusCode != http.StatusPartialContent {
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusRequestedRangeNotSatisfiable {
			return fmt.Errorf("%w: status %s", errParallelBlobRangeUnsupported, resp.Status)
		}
		return fmt.Errorf("range %d-%d returned status %s", start, end, resp.Status)
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

	body := &rangeCountingReader{r: resp.Body, n: &part.bytes}
	written, copyErr := io.CopyN(io.NewOffsetWriter(dest, start), body, want)
	part.done += written
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
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	return fmt.Errorf("short range write: wrote %d, expected %d", written, want)
}

// rangeCountingReader adds bytes read to a shared counter (progress for stragglerRule).
type rangeCountingReader struct {
	r io.Reader
	n *atomic.Int64
}

func (c *rangeCountingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n.Add(int64(n))
	return n, err
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
