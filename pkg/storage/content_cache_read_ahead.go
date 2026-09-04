package storage

import (
	"fmt"
	"io"
	"sync"

	"golang.org/x/sync/singleflight"
)

const (
	// Windows are fetched whole: a bigger window means fewer round trips to
	// the cache host for a sequential reader, at the cost of latency and
	// waste for a reader that touches one page of it.
	DefaultContentCacheReadAheadBytes = 4 * 1024 * 1024
	DefaultContentCacheReadAheadSlots = 16
)

type ContentCacheReadAheadOptions struct {
	WindowBytes int64
	MaxWindows  int
}

type ContentCacheReadAhead struct {
	cache       ContentCache
	windowBytes int64
	maxWindows  int

	mu      sync.Mutex
	windows map[contentCacheWindowKey][]byte
	order   []contentCacheWindowKey
	group   singleflight.Group

	inflight    sync.Mutex
	prefetching map[contentCacheWindowKey]struct{}
	prefetchWG  sync.WaitGroup
}

// maxPrefetchInFlight bounds background window fetches per read-ahead cache.
const maxPrefetchInFlight = 4

type contentCacheWindowKey struct {
	hash       string
	routingKey string
	start      int64
	end        int64
}

func NewContentCacheReadAhead(cache ContentCache, opts ContentCacheReadAheadOptions) *ContentCacheReadAhead {
	if cache == nil {
		return nil
	}
	windowBytes := opts.WindowBytes
	if windowBytes <= 0 {
		windowBytes = DefaultContentCacheReadAheadBytes
	}
	maxWindows := opts.MaxWindows
	if maxWindows <= 0 {
		maxWindows = DefaultContentCacheReadAheadSlots
	}
	return &ContentCacheReadAhead{
		cache:       cache,
		windowBytes: windowBytes,
		maxWindows:  maxWindows,
		windows:     make(map[contentCacheWindowKey][]byte),
	}
}

func (r *ContentCacheReadAhead) Read(hash string, offset int64, dest []byte, opts struct{ RoutingKey string }, limit int64) (int64, error) {
	if r == nil || r.cache == nil {
		return 0, fmt.Errorf("content cache is not available")
	}
	if opts.RoutingKey == "" {
		opts.RoutingKey = hash
	}
	if offset < 0 {
		return 0, fmt.Errorf("negative content cache offset: %d", offset)
	}
	length := int64(len(dest))
	if length == 0 {
		return 0, nil
	}
	if limit > 0 && offset+length > limit {
		return 0, io.ErrUnexpectedEOF
	}
	if limit <= 0 || r.windowBytes <= length {
		return readContentCacheInto(r.cache, hash, offset, dest, opts)
	}

	start := (offset / r.windowBytes) * r.windowBytes
	end := start + r.windowBytes
	if needEnd := offset + length; end < needEnd {
		end = needEnd
	}
	if end > limit {
		end = limit
	}
	if end < offset+length || end <= start {
		return readContentCacheInto(r.cache, hash, offset, dest, opts)
	}

	key := contentCacheWindowKey{hash: hash, routingKey: opts.RoutingKey, start: start, end: end}
	// A reader in the second half of a window is likely sequential: fetch the
	// next window now so its transfer overlaps with the pages being consumed.
	if offset+length > start+r.windowBytes/2 && end < limit {
		r.prefetch(hash, end, limit, opts)
	}
	data, err := r.window(key, opts)
	if err != nil {
		return 0, err
	}
	if int64(len(data)) < offset-start+length {
		return 0, io.ErrUnexpectedEOF
	}
	copy(dest, data[offset-start:offset-start+length])
	return length, nil
}

// window returns the bytes of key, fetching them once across concurrent callers.
func (r *ContentCacheReadAhead) window(key contentCacheWindowKey, opts struct{ RoutingKey string }) ([]byte, error) {
	if data, ok := r.get(key); ok {
		return data, nil
	}
	value, err, _ := r.group.Do(key.String(), func() (any, error) {
		if data, ok := r.get(key); ok {
			return data, nil
		}
		size := key.end - key.start
		if size <= 0 || size > int64(int(size)) {
			return nil, fmt.Errorf("invalid content cache read-ahead size: %d", size)
		}
		data := make([]byte, int(size))
		n, err := readContentCacheInto(r.cache, key.hash, key.start, data, opts)
		if err != nil {
			return nil, err
		}
		if n != size {
			return nil, fmt.Errorf("content cache short read: want %d, got %d", size, n)
		}
		r.put(key, data)
		return data, nil
	})
	if err != nil {
		return nil, err
	}
	data, ok := value.([]byte)
	if !ok {
		return nil, io.ErrUnexpectedEOF
	}
	return data, nil
}

// prefetch fetches the window starting at start in the background. The
// singleflight group keeps one fetch in flight per window, and a fetch that
// is already running when the reader arrives is simply joined.
func (r *ContentCacheReadAhead) prefetch(hash string, start, limit int64, opts struct{ RoutingKey string }) {
	end := start + r.windowBytes
	if end > limit {
		end = limit
	}
	if end <= start {
		return
	}
	key := contentCacheWindowKey{hash: hash, routingKey: opts.RoutingKey, start: start, end: end}
	if _, ok := r.get(key); ok {
		return
	}
	r.inflight.Lock()
	if r.prefetching == nil {
		r.prefetching = map[contentCacheWindowKey]struct{}{}
	}
	if _, busy := r.prefetching[key]; busy || len(r.prefetching) >= maxPrefetchInFlight {
		r.inflight.Unlock()
		return
	}
	r.prefetching[key] = struct{}{}
	r.inflight.Unlock()
	r.prefetchWG.Add(1)
	go func() {
		defer func() {
			r.inflight.Lock()
			delete(r.prefetching, key)
			r.inflight.Unlock()
			r.prefetchWG.Done()
		}()
		_, _ = r.window(key, opts)
	}()
}

// WaitPrefetches blocks until no background window fetch is in flight.
func (r *ContentCacheReadAhead) WaitPrefetches() {
	if r != nil {
		r.prefetchWG.Wait()
	}
}

func (k contentCacheWindowKey) String() string {
	return fmt.Sprintf("%s\x00%s\x00%d\x00%d", k.hash, k.routingKey, k.start, k.end)
}

func (r *ContentCacheReadAhead) get(key contentCacheWindowKey) ([]byte, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	data, ok := r.windows[key]
	return data, ok
}

func (r *ContentCacheReadAhead) put(key contentCacheWindowKey, data []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.windows[key]; ok {
		return
	}
	r.windows[key] = data
	r.order = append(r.order, key)
	for len(r.order) > r.maxWindows {
		evict := r.order[0]
		copy(r.order, r.order[1:])
		r.order = r.order[:len(r.order)-1]
		delete(r.windows, evict)
	}
}

func readContentCacheInto(cache ContentCache, hash string, offset int64, dest []byte, opts struct{ RoutingKey string }) (int64, error) {
	if cache == nil {
		return 0, fmt.Errorf("content cache is not available")
	}
	if readInto, ok := cache.(ContentCacheReadInto); ok {
		return readInto.ReadContentInto(hash, offset, dest, opts)
	}

	data, err := cache.GetContent(hash, offset, int64(len(dest)), opts)
	if err != nil {
		return 0, err
	}
	n := copy(dest, data)
	if n != len(dest) {
		return int64(n), io.ErrUnexpectedEOF
	}
	return int64(n), nil
}
