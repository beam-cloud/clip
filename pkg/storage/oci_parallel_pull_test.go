package storage

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/beam-cloud/clip/pkg/common"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/stretchr/testify/require"
)

const parallelPullTestDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

type rangeBlobServer struct {
	data            []byte
	malformed       bool
	block           <-chan struct{}
	started         chan<- struct{}
	delay           time.Duration
	requests        atomic.Int64
	active          atomic.Int64
	maximum         atomic.Int64
	authLeaks       atomic.Int64
	transient       bool
	transientMu     sync.Mutex
	transientServed bool
}

func (s *rangeBlobServer) serve(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "" {
		s.authLeaks.Add(1)
	}
	start, end, err := parseTestRange(r.Header.Get("Range"), int64(len(s.data)))
	if err != nil {
		http.Error(w, err.Error(), http.StatusRequestedRangeNotSatisfiable)
		return
	}
	s.requests.Add(1)
	active := s.active.Add(1)
	defer s.active.Add(-1)
	for {
		maximum := s.maximum.Load()
		if active <= maximum || s.maximum.CompareAndSwap(maximum, active) {
			break
		}
	}
	if s.started != nil {
		select {
		case s.started <- struct{}{}:
		default:
		}
	}
	if s.block != nil {
		select {
		case <-s.block:
		case <-r.Context().Done():
			return
		}
	}
	if s.delay > 0 && start != 0 {
		time.Sleep(s.delay)
	}
	if s.transient {
		s.transientMu.Lock()
		served := s.transientServed
		if !served {
			s.transientServed = true
		}
		s.transientMu.Unlock()
		if !served {
			http.Error(w, "retry", http.StatusServiceUnavailable)
			return
		}
	}
	contentRange := fmt.Sprintf("bytes %d-%d/%d", start, end, len(s.data))
	if s.malformed {
		contentRange = fmt.Sprintf("bytes %d-%d/%d", start+1, end, len(s.data))
	}
	w.Header().Set("Content-Range", contentRange)
	w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
	w.WriteHeader(http.StatusPartialContent)
	_, _ = w.Write(s.data[start : end+1])
}

type registryRedirectServer struct {
	data          []byte
	redirectURL   string
	ignoreRanges  bool
	rangeRequests atomic.Int64
	fullRequests  atomic.Int64
	authFailures  atomic.Int64
}

func (s *registryRedirectServer) serve(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer clip-test" {
		s.authFailures.Add(1)
	}
	if r.Header.Get("Range") == "" {
		s.fullRequests.Add(1)
		w.Header().Set("Content-Length", strconv.Itoa(len(s.data)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(s.data)
		return
	}
	s.rangeRequests.Add(1)
	if s.ignoreRanges {
		w.Header().Set("Content-Length", strconv.Itoa(len(s.data)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(s.data)
		return
	}
	http.Redirect(w, r, s.redirectURL, http.StatusTemporaryRedirect)
}

func parseTestRange(value string, size int64) (int64, int64, error) {
	if !strings.HasPrefix(value, "bytes=") {
		return 0, 0, fmt.Errorf("invalid range %q", value)
	}
	bounds := strings.Split(strings.TrimPrefix(value, "bytes="), "-")
	if len(bounds) != 2 {
		return 0, 0, fmt.Errorf("invalid range %q", value)
	}
	start, err := strconv.ParseInt(bounds[0], 10, 64)
	if err != nil {
		return 0, 0, err
	}
	end, err := strconv.ParseInt(bounds[1], 10, 64)
	if err != nil {
		return 0, 0, err
	}
	if start < 0 || end < start || end >= size {
		return 0, 0, fmt.Errorf("range outside object: %d-%d/%d", start, end, size)
	}
	return start, end, nil
}

func parallelTestClient(t *testing.T, registry *httptest.Server, tempDir string, size, partSize int64, concurrency int) *http.Client {
	t.Helper()
	parsed, err := url.Parse(registry.URL)
	require.NoError(t, err)
	return &http.Client{Transport: newParallelBlobTransport(parallelBlobPullConfig{
		inner:          registry.Client().Transport,
		registryHost:   parsed.Host,
		digest:         parallelPullTestDigest,
		size:           size,
		tempDir:        tempDir,
		minimumFree:    1,
		threshold:      1,
		partSize:       partSize,
		concurrency:    concurrency,
		attempts:       3,
		retryBackoff:   time.Millisecond,
		availableBytes: func(string) (int64, error) { return math.MaxInt64, nil },
	})}
}

func parallelTestRequest(t *testing.T, client *http.Client, registryURL string, ctx context.Context) (*http.Response, error) {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, registryURL+"/v2/repo/blobs/"+parallelPullTestDigest, nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer clip-test")
	return client.Do(req)
}

func requireNoParallelTemps(t *testing.T, dir string) {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, ".clip-oci-compressed-*.tmp"))
	require.NoError(t, err)
	require.Empty(t, files)
}

func TestParallelBlobTransportReassemblesAcrossECRStyleRedirect(t *testing.T) {
	data := make([]byte, 2<<20)
	for i := range data {
		data[i] = byte((i*31 + 7) % 251)
	}
	blobState := &rangeBlobServer{data: data, delay: 3 * time.Millisecond}
	blob := httptest.NewServer(http.HandlerFunc(blobState.serve))
	defer blob.Close()
	// A distinct hostname exercises Go's cross-host redirect policy: Range is
	// preserved while the registry bearer token is stripped from the signed URL.
	signedURL := strings.Replace(blob.URL, "127.0.0.1", "localhost", 1) + "/signed-layer"
	registryState := &registryRedirectServer{data: data, redirectURL: signedURL}
	registry := httptest.NewServer(http.HandlerFunc(registryState.serve))
	defer registry.Close()
	tempDir := t.TempDir()
	const partSize int64 = 32 << 10
	client := parallelTestClient(t, registry, tempDir, int64(len(data)), partSize, 16)

	resp, err := parallelTestRequest(t, client, registry.URL, context.Background())
	require.NoError(t, err)
	actual, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, data, actual)
	expectedRanges := int64(1 + (int64(len(data))-1+partSize-1)/partSize)
	require.Equal(t, expectedRanges, registryState.rangeRequests.Load())
	require.Equal(t, expectedRanges, blobState.requests.Load())
	require.Zero(t, registryState.fullRequests.Load())
	require.Zero(t, registryState.authFailures.Load())
	require.Zero(t, blobState.authLeaks.Load())
	require.Greater(t, blobState.maximum.Load(), int64(1))
	require.LessOrEqual(t, blobState.maximum.Load(), int64(16))
	requireNoParallelTemps(t, tempDir)
}

func TestParallelBlobTransportThroughGoContainerRegistryAuthAndVerifier(t *testing.T) {
	data := make([]byte, 512<<10)
	for i := range data {
		data[i] = byte((i*17 + 11) % 251)
	}
	digestBytes := sha256.Sum256(data)
	digest := "sha256:" + hex.EncodeToString(digestBytes[:])
	wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("clip-user:clip-pass"))
	var authFailures atomic.Int64
	var active atomic.Int64
	var maximum atomic.Int64

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/" && r.Header.Get("Authorization") == "" {
			w.Header().Set("WWW-Authenticate", `Basic realm="clip-test"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Header.Get("Authorization") != wantAuth {
			authFailures.Add(1)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Path == "/v2/" {
			w.WriteHeader(http.StatusOK)
			return
		}
		if !strings.HasSuffix(r.URL.Path, "/blobs/"+digest) {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Docker-Content-Digest", digest)
		w.Header().Set("Content-Type", "application/vnd.oci.image.layer.v1.tar+gzip")
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Length", strconv.Itoa(len(data)))
			w.WriteHeader(http.StatusOK)
			return
		}
		start, end, err := parseTestRange(r.Header.Get("Range"), int64(len(data)))
		if err != nil {
			http.Error(w, err.Error(), http.StatusRequestedRangeNotSatisfiable)
			return
		}
		current := active.Add(1)
		defer active.Add(-1)
		for {
			seen := maximum.Load()
			if current <= seen || maximum.CompareAndSwap(seen, current) {
				break
			}
		}
		if start != 0 {
			time.Sleep(2 * time.Millisecond)
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
		w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(data[start : end+1])
	})
	registry := httptest.NewTLSServer(handler)
	defer registry.Close()
	parsed, err := url.Parse(registry.URL)
	require.NoError(t, err)
	tempDir := t.TempDir()
	transport := newParallelBlobTransport(parallelBlobPullConfig{
		inner:          registry.Client().Transport,
		registryHost:   parsed.Host,
		digest:         digest,
		size:           int64(len(data)),
		tempDir:        tempDir,
		minimumFree:    1,
		threshold:      1,
		partSize:       16 << 10,
		concurrency:    16,
		attempts:       1,
		availableBytes: func(string) (int64, error) { return math.MaxInt64, nil },
	})
	ref, err := name.NewDigest(parsed.Host + "/repo@" + digest)
	require.NoError(t, err)
	layer, err := remote.Layer(
		ref,
		remote.WithTransport(transport),
		remote.WithAuth(authn.FromConfig(authn.AuthConfig{Username: "clip-user", Password: "clip-pass"})),
	)
	require.NoError(t, err)
	compressed, err := layer.Compressed()
	require.NoError(t, err)
	actual, err := io.ReadAll(compressed)
	require.NoError(t, err, "go-containerregistry must accept the reassembled compressed digest")
	require.NoError(t, compressed.Close())
	require.Equal(t, data, actual)
	require.Zero(t, authFailures.Load())
	require.Greater(t, maximum.Load(), int64(1))
	require.LessOrEqual(t, maximum.Load(), int64(16))
	requireNoParallelTemps(t, tempDir)
}

func TestParallelBlobTransportProbesBeforeUnsupportedFallback(t *testing.T) {
	data := []byte(strings.Repeat("single-fallback-", 8192))
	registryState := &registryRedirectServer{data: data, ignoreRanges: true}
	registry := httptest.NewServer(http.HandlerFunc(registryState.serve))
	defer registry.Close()
	tempDir := t.TempDir()
	client := parallelTestClient(t, registry, tempDir, int64(len(data)), 32<<10, 16)

	resp, err := parallelTestRequest(t, client, registry.URL, context.Background())
	require.NoError(t, err)
	actual, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, data, actual)
	require.Equal(t, int64(1), registryState.rangeRequests.Load(), "unsupported registry must only receive the one-byte probe")
	require.Equal(t, int64(1), registryState.fullRequests.Load())
	requireNoParallelTemps(t, tempDir)
}

func TestParallelBlobTransportMalformedContentRangeFallsBack(t *testing.T) {
	data := []byte(strings.Repeat("malformed-range-", 8192))
	blobState := &rangeBlobServer{data: data, malformed: true}
	blob := httptest.NewServer(http.HandlerFunc(blobState.serve))
	defer blob.Close()
	registryState := &registryRedirectServer{data: data, redirectURL: blob.URL + "/signed"}
	registry := httptest.NewServer(http.HandlerFunc(registryState.serve))
	defer registry.Close()
	tempDir := t.TempDir()
	client := parallelTestClient(t, registry, tempDir, int64(len(data)), 32<<10, 16)

	resp, err := parallelTestRequest(t, client, registry.URL, context.Background())
	require.NoError(t, err)
	actual, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, data, actual)
	require.Equal(t, int64(1), registryState.fullRequests.Load())
	requireNoParallelTemps(t, tempDir)
}

func TestParallelBlobTransportCancellationDoesNotFallback(t *testing.T) {
	data := []byte(strings.Repeat("cancel-range-", 32<<10))
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	blobState := &rangeBlobServer{data: data, block: release, started: started}
	blob := httptest.NewServer(http.HandlerFunc(blobState.serve))
	defer blob.Close()
	registryState := &registryRedirectServer{data: data, redirectURL: blob.URL + "/signed"}
	registry := httptest.NewServer(http.HandlerFunc(registryState.serve))
	defer registry.Close()
	tempDir := t.TempDir()
	client := parallelTestClient(t, registry, tempDir, int64(len(data)), 32<<10, 16)
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, registry.URL+"/v2/repo/blobs/"+parallelPullTestDigest, nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer clip-test")
	done := make(chan error, 1)
	go func() {
		_, err := client.Do(req)
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("range probe did not start")
	}
	cancel()
	select {
	case err := <-done:
		require.Error(t, err)
		require.True(t, errors.Is(err, context.Canceled), "got %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("canceled parallel pull did not return")
	}
	close(release)
	require.Zero(t, registryState.fullRequests.Load())
	require.Eventually(t, func() bool {
		files, _ := filepath.Glob(filepath.Join(tempDir, ".clip-oci-compressed-*.tmp"))
		return len(files) == 0
	}, time.Second, 10*time.Millisecond)
}

func TestParallelBlobTransportDiskGateUsesSingleStream(t *testing.T) {
	data := []byte(strings.Repeat("disk-gate-", 8192))
	registryState := &registryRedirectServer{data: data, ignoreRanges: true}
	registry := httptest.NewServer(http.HandlerFunc(registryState.serve))
	defer registry.Close()
	parsed, err := url.Parse(registry.URL)
	require.NoError(t, err)
	client := &http.Client{Transport: newParallelBlobTransport(parallelBlobPullConfig{
		inner:          registry.Client().Transport,
		registryHost:   parsed.Host,
		digest:         parallelPullTestDigest,
		size:           int64(len(data)),
		tempDir:        t.TempDir(),
		minimumFree:    int64(len(data)) + 1,
		threshold:      1,
		partSize:       32 << 10,
		concurrency:    16,
		availableBytes: func(string) (int64, error) { return int64(len(data)), nil },
	})}

	resp, err := parallelTestRequest(t, client, registry.URL, context.Background())
	require.NoError(t, err)
	_, err = io.Copy(io.Discard, resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, int64(1), registryState.fullRequests.Load())
	require.Zero(t, registryState.rangeRequests.Load())
}

func TestParallelBlobTransportRetriesTransientProbe(t *testing.T) {
	data := []byte(strings.Repeat("retry-probe-", 8192))
	blobState := &rangeBlobServer{data: data, transient: true}
	blob := httptest.NewServer(http.HandlerFunc(blobState.serve))
	defer blob.Close()
	registryState := &registryRedirectServer{data: data, redirectURL: blob.URL + "/signed"}
	registry := httptest.NewServer(http.HandlerFunc(registryState.serve))
	defer registry.Close()
	tempDir := t.TempDir()
	client := parallelTestClient(t, registry, tempDir, int64(len(data)), 32<<10, 4)

	resp, err := parallelTestRequest(t, client, registry.URL, context.Background())
	require.NoError(t, err)
	actual, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, data, actual)
	require.Greater(t, blobState.requests.Load(), int64(1))
	require.Zero(t, registryState.fullRequests.Load())
	requireNoParallelTemps(t, tempDir)
}

func TestParallelBlobMetadataAndRegistryNormalization(t *testing.T) {
	const compressedSize int64 = 2 << 30
	const uncompressedSize int64 = 3 << 30
	digest := parallelPullTestDigest
	storage := &OCIClipStorage{storageInfo: &common.OCIStorageInfo{
		ImageMetadata: &common.ImageMetadata{LayersData: []common.LayerMetadata{{Digest: digest, Size: compressedSize}}},
		GzipIdxByLayer: map[string]*common.GzipIndex{
			digest: {Checkpoints: []common.GzipCheckpoint{{UOff: uncompressedSize}}},
		},
	}}
	require.Equal(t, compressedSize, storage.compressedLayerSize(digest))
	require.Equal(t, uncompressedSize+(1<<20), storage.estimatedUncompressedLayerSize(digest))
	require.Equal(t, "registry.example.com:5000", normalizedRegistryHost("https://registry.example.com:5000/team"))
	require.Equal(t, "registry.example.com", normalizedRegistryHost("registry.example.com"))
}

func TestParseContentRange(t *testing.T) {
	start, end, total, err := parseContentRange("bytes 4-9/20")
	require.NoError(t, err)
	require.Equal(t, int64(4), start)
	require.Equal(t, int64(9), end)
	require.Equal(t, int64(20), total)
	for _, value := range []string{"", "items 4-9/20", "bytes */20", "bytes 4-3/20", "bytes 4-20/20", "bytes x-9/20"} {
		_, _, _, err := parseContentRange(value)
		require.Error(t, err, value)
	}
}

func TestRemoveOnCloseFileIsIdempotentForRemovedPath(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "remove-on-close-*")
	require.NoError(t, err)
	path := file.Name()
	require.NoError(t, os.Remove(path))
	require.NoError(t, (&removeOnCloseFile{File: file, path: path}).Close())
}

func TestStragglerRule(t *testing.T) {
	now := time.Now()
	mk := func(age time.Duration, bytes int64, aborts int32) *rangePart {
		p := &rangePart{startedAt: now.Add(-age)}
		p.bytes.Store(bytes)
		p.aborts.Store(aborts)
		return p
	}
	const floor = 4 << 20
	grace := 3 * time.Second
	rule := func(parts []*rangePart, completed []float64, tail bool) []*rangePart {
		return stragglerRule(parts, completed, tail, now, grace, floor, 3)
	}

	fast := mk(4*time.Second, 400<<20, 0)  // 100 MiB/s
	fast2 := mk(4*time.Second, 320<<20, 0) // 80 MiB/s
	stalled := mk(4*time.Second, 0, 0)
	slow := mk(4*time.Second, 4<<20, 0)  // 1 MiB/s: below floor and far below median
	young := mk(time.Second, 0, 0)       // inside grace
	exhausted := mk(4*time.Second, 0, 3) // hit the abort cap

	got := rule([]*rangePart{fast, fast2, stalled, slow, young, exhausted}, nil, false)
	require.ElementsMatch(t, []*rangePart{stalled, slow}, got)

	// Uniformly slow link: everything is below the floor but nothing is far
	// below the median, so nothing is abandoned.
	uniform := []*rangePart{mk(4*time.Second, 8<<20, 0), mk(4*time.Second, 9<<20, 0), mk(4*time.Second, 12<<20, 0)}
	require.Empty(t, rule(uniform, nil, false))

	// ...unless recently completed parts show the link can do far better.
	require.ElementsMatch(t, uniform, rule(uniform, []float64{60 << 20, 70 << 20, 80 << 20}, false))

	// A lone part has no peers; the floor alone decides.
	require.Equal(t, []*rangePart{slow}, rule([]*rangePart{slow}, nil, false))
	require.Empty(t, rule([]*rangePart{fast}, nil, false))

	// Tail: a part above the floor but well below the reference is abandoned,
	// and the grace period is halved.
	tailSlow := mk(2*time.Second, 20<<20, 0) // 10 MiB/s
	require.Empty(t, rule([]*rangePart{tailSlow}, []float64{60 << 20, 70 << 20}, false))
	require.Equal(t, []*rangePart{tailSlow}, rule([]*rangePart{tailSlow}, []float64{60 << 20, 70 << 20}, true))
	require.Empty(t, rule([]*rangePart{mk(2*time.Second, 60<<20, 0)}, []float64{60 << 20, 70 << 20}, true))
	// Tail with no reference falls back to the floor.
	require.Empty(t, rule([]*rangePart{tailSlow}, nil, true))
	require.Equal(t, []*rangePart{slow}, rule([]*rangePart{slow}, nil, true))
}

func TestSlowEndpointSetExpires(t *testing.T) {
	set := &slowEndpointSet{ttl: time.Minute, seen: map[string]time.Time{}}
	set.mark("52.216.1.2:443")
	set.mark("[2600::1]:443")
	now := time.Now()
	require.True(t, set.isSlow("52.216.1.2", now))
	require.True(t, set.isSlow("2600::1", now))
	require.False(t, set.isSlow("52.216.1.3", now))
	require.False(t, set.isSlow("52.216.1.2", now.Add(2*time.Minute)))
	require.False(t, set.isSlow("52.216.1.2", now), "expired entries are dropped")
}

// stallingBlobServer serves ranges normally except that the first attempt at
// each range whose start is in stallStarts sends headers and a few bytes, then
// hangs until the client abandons it.
type stallingBlobServer struct {
	data        []byte
	stallStarts map[int64]bool
	mu          sync.Mutex
	stalled     map[int64]int
	resumeFrom  map[int64]bool
	resumed     atomic.Int64
	requests    atomic.Int64
}

func (s *stallingBlobServer) serve(w http.ResponseWriter, r *http.Request) {
	start, end, err := parseTestRange(r.Header.Get("Range"), int64(len(s.data)))
	if err != nil {
		http.Error(w, err.Error(), http.StatusRequestedRangeNotSatisfiable)
		return
	}
	s.requests.Add(1)
	s.mu.Lock()
	if s.resumeFrom[start] {
		s.resumed.Add(1)
	}
	s.mu.Unlock()
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(s.data)))
	w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
	w.WriteHeader(http.StatusPartialContent)
	s.mu.Lock()
	stall := s.stallStarts[start] && s.stalled[start] == 0
	if stall {
		s.stalled[start]++
	}
	s.mu.Unlock()
	if stall {
		// Partial progress, then starvation: the resume must pick up at +16.
		s.mu.Lock()
		s.resumeFrom[start+16] = true
		s.mu.Unlock()
		_, _ = w.Write(s.data[start : start+16])
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done()
		return
	}
	_, _ = w.Write(s.data[start : end+1])
}

func TestParallelBlobTransportAbandonsStragglersAndRefetches(t *testing.T) {
	const partSize = 64 << 10
	data := make([]byte, 16*partSize+1)
	for i := range data {
		data[i] = byte((i*17 + 3) % 253)
	}
	// Parts start at 1 + k*partSize (byte 0 is the probe).
	blobState := &stallingBlobServer{
		data:        data,
		stallStarts: map[int64]bool{1 + 2*partSize: true, 1 + 9*partSize: true},
		stalled:     map[int64]int{},
		resumeFrom:  map[int64]bool{},
	}
	blob := httptest.NewServer(http.HandlerFunc(blobState.serve))
	defer blob.Close()
	registryState := &registryRedirectServer{data: data, redirectURL: blob.URL + "/signed"}
	registry := httptest.NewServer(http.HandlerFunc(registryState.serve))
	defer registry.Close()
	parsed, err := url.Parse(registry.URL)
	require.NoError(t, err)

	var marked sync.Map
	tempDir := t.TempDir()
	client := &http.Client{Transport: newParallelBlobTransport(parallelBlobPullConfig{
		inner:            registry.Client().Transport,
		registryHost:     parsed.Host,
		digest:           parallelPullTestDigest,
		size:             int64(len(data)),
		tempDir:          tempDir,
		minimumFree:      1,
		threshold:        1,
		partSize:         partSize,
		concurrency:      4,
		attempts:         3,
		retryBackoff:     time.Millisecond,
		availableBytes:   func(string) (int64, error) { return math.MaxInt64, nil },
		stragglerGrace:   150 * time.Millisecond,
		stragglerFloor:   1 << 20,
		slots:            make(chan struct{}, 3),
		markSlowEndpoint: func(addr string) { marked.Store(addr, true) },
	})}

	started := time.Now()
	resp, err := parallelTestRequest(t, client, registry.URL, context.Background())
	require.NoError(t, err)
	actual, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, data, actual, "abandoned ranges must be resumed byte-exact")
	require.Less(t, time.Since(started), 5*time.Second)
	require.Zero(t, registryState.fullRequests.Load(), "stragglers must not trigger the single-stream fallback")
	require.Equal(t, 1, blobState.stalled[1+2*partSize])
	require.Equal(t, 1, blobState.stalled[1+9*partSize])
	// 1 probe + 16 parts + 2 resumes, each resume starting after the 16 bytes
	// the starved attempt delivered.
	require.Equal(t, int64(19), blobState.requests.Load())
	require.Equal(t, int64(2), blobState.resumed.Load())
	blobHost, _, _ := net.SplitHostPort(strings.TrimPrefix(blob.URL, "http://"))
	var markedHosts []string
	marked.Range(func(k, _ any) bool {
		h, _, _ := net.SplitHostPort(k.(string))
		markedHosts = append(markedHosts, h)
		return true
	})
	require.Equal(t, []string{blobHost}, markedHosts, "the starving front-end is marked slow")
	requireNoParallelTemps(t, tempDir)
}
