# Architecture Audit - ContentCache Range Reads

## Your Requirements

**Goal:** Minimize cold start across cluster nodes using lazy loading via ContentCache.

**Desired behavior:**
```
Node A (first access):
  1. Pull entire layer from OCI registry
  2. Decompress to disk cache
  3. Store decompressed layer in remote ContentCache
  4. Serve file from disk cache
  ✓ Acceptable overhead (happens once per cluster)

Node A (subsequent):
  1. Read from disk cache
  ✓ Fast (local disk)

Node B (first access):
  1. Check disk cache (miss)
  2. Fetch ONLY needed file range from remote ContentCache (lazy!)
  3. Serve file immediately
  ✓ Fast cold start (no full layer download!)
```

---

## Current Implementation Issues

### Issue 1: Wrong ContentCache Method

**ContentCache interface supports range reads:**
```go
type ContentCache interface {
    GetContent(hash string, offset int64, length int64, opts struct{ RoutingKey string }) ([]byte, error)
    //         ^^^^        ^^^^^^          ^^^^^^  ← HAS offset and length!
    StoreContent(chunks chan []byte, hash string, opts struct{ RoutingKey string }) (string, error)
}
```

**But we're using the OLD interface:**
```go
// Line 354 in oci.go
data, found, err := s.contentCache.Get(cacheKey)  // ← WRONG METHOD!
//                                   ^^^
// Should be: GetContent(hash, offset, length, opts)
```

This is calling a method that doesn't exist in the interface!

### Issue 2: Caching Entire Layers Instead of Range Reads

**Current flow:**
```go
func ReadFile(node *common.ClipNode, dest []byte, offset int64) {
    // Calculate file range we need
    wantUStart := remote.UOffset + offset
    readLen := int64(len(dest))
    
    // ❌ Ensure ENTIRE layer is cached on disk
    layerPath, err := s.ensureLayerCached(remote.LayerDigest)
    
    // Read from disk
    return s.readFromDiskCache(layerPath, wantUStart, dest[:readLen])
}
```

**What happens:**
- Node A: Decompress entire layer, store entire layer in ContentCache
- Node B: Try to fetch entire layer from ContentCache, not just the file range!

**What should happen:**
- Node A: Same (cache entire layer)
- Node B: **Fetch ONLY the file range from ContentCache** (offset, length)

---

## Checkpoints: Are They Still Useful?

### Current Role
Checkpoints enable **lazy reading from OCI registry** (gzip seeking):
- Without: Must decompress from start to reach file → slow
- With: Seek to checkpoint near file, decompress small chunk → fast

### With Your Architecture

**Scenario 1: First node in cluster accesses layer**
- Must pull from OCI registry
- **With checkpoints:** Can do lazy reads (only decompress what's needed)
- **Without checkpoints:** Must decompress entire layer

**Scenario 2: Second+ node accesses layer**  
- Pulls from ContentCache (Node 1 cached it)
- **Checkpoints not used** (ContentCache has decompressed data, just use byte offsets)

### Recommendation

**Keep checkpoints for now**, but they're NOT the bottleneck. Here's why:

**Benefits:**
- ✅ Useful for first node (lazy reads from OCI registry)
- ✅ Reduce bandwidth on first pull (only decompress what you need)
- ✅ Faster cold start even on Node A

**Not critical:**
- ⚠️ Most value comes from ContentCache range reads (Node B onwards)
- ⚠️ If you always decompress entire layers on Node A, checkpoints don't help much

**Priority:**
1. **HIGH:** Fix ContentCache range reads (enables your cross-node lazy loading)
2. **MEDIUM:** Keep checkpoints (helps first node, minimal overhead)
3. **LOW:** Optimize checkpoint interval (already good at 2 MiB + content-defined)

---

## The Fix

### What needs to change:

#### 1. Fix `tryGetDecompressedFromRemoteCache` to use range reads
```go
// OLD (broken - fetches entire layer)
func (s *OCIClipStorage) tryGetDecompressedFromRemoteCache(digest string) ([]byte, bool) {
    cacheKey := s.getContentHash(digest)
    data, found, err := s.contentCache.Get(cacheKey)  // ← WRONG!
    // ...
}

// NEW (fixed - supports range reads)
func (s *OCIClipStorage) tryGetRangeFromRemoteCache(digest string, offset, length int64) ([]byte, bool) {
    cacheKey := s.getContentHash(digest)
    data, err := s.contentCache.GetContent(cacheKey, offset, length, struct{ RoutingKey string }{})
    // ...
}
```

#### 2. Update `ReadFile` to try range read before full layer download
```go
func (s *OCIClipStorage) ReadFile(node *common.ClipNode, dest []byte, offset int64) (int, error) {
    remote := node.Remote
    wantUStart := remote.UOffset + offset
    readLen := int64(len(dest))
    
    // 1. Try disk cache first (fastest)
    layerPath := s.getDiskCachePath(remote.LayerDigest)
    if _, err := os.Stat(layerPath); err == nil {
        return s.readFromDiskCache(layerPath, wantUStart, dest[:readLen])
    }
    
    // 2. Try range read from remote ContentCache (fast, no full layer download!)
    if s.contentCache != nil {
        if data, found := s.tryGetRangeFromRemoteCache(remote.LayerDigest, wantUStart, readLen); found {
            copy(dest, data)
            return len(data), nil
        }
    }
    
    // 3. Fallback: decompress from OCI registry (slow, but cache for future)
    layerPath, err := s.ensureLayerCached(remote.LayerDigest)
    if err != nil {
        return 0, err
    }
    return s.readFromDiskCache(layerPath, wantUStart, dest[:readLen])
}
```

#### 3. Keep storing entire layers to ContentCache (for range reads)
```go
// This part is already correct!
// Node A stores entire decompressed layer
// Nodes B, C, D... do range reads from it
func (s *OCIClipStorage) storeDecompressedInRemoteCache(digest string, diskPath string) {
    // Read entire decompressed layer
    data, err := os.ReadFile(diskPath)
    
    // Store entire layer (so other nodes can do range reads!)
    cacheKey := s.getContentHash(digest)
    s.contentCache.Set(cacheKey, data)
}
```

---

## New Architecture Flow

### Node A (first in cluster):
```
1. ReadFile(/bin/sh)
2. Check disk cache → MISS
3. Check remote cache (range read) → MISS
4. Pull from OCI registry
   - With checkpoints: Decompress only what's needed ← FAST
   - Without checkpoints: Decompress entire layer ← SLOWER
5. Cache entire decompressed layer:
   - Disk: /tmp/clip-oci-cache/sha256_abc...
   - Remote: ContentCache[abc...] = <entire layer>
6. Serve /bin/sh from disk cache
```

### Node B (second in cluster):
```
1. ReadFile(/bin/sh)
2. Check disk cache → MISS
3. Check remote cache (range read):
   - GetContent(abc..., offset=12345, length=98765) ← LAZY!
   - Returns ONLY /bin/sh data, not entire layer!
4. Serve /bin/sh immediately ← FAST COLD START!
5. Optional: Cache to disk for future local reads
```

---

## Performance Impact

### Without range reads (current):
```
Node A: Download layer (10 MB), decompress (2s), cache → 2.5s
Node B: Download layer from cache (10 MB), save to disk → 1s
Node C: Download layer from cache (10 MB), save to disk → 1s
Total bandwidth: 30 MB
```

### With range reads (fixed):
```
Node A: Download layer (10 MB), decompress (2s), cache → 2.5s
Node B: Range read (100 KB), serve immediately → 50ms ← FAST!
Node C: Range read (100 KB), serve immediately → 50ms ← FAST!
Total bandwidth: 10.2 MB (3× reduction!)
```

---

## Summary

### Critical Issue Found:
❌ Using wrong ContentCache method (`Get` vs `GetContent`)
❌ Not doing range reads from remote cache
❌ Fetching entire layers on every node

### Fix Priority:
1. **CRITICAL:** Implement range reads from ContentCache
2. **IMPORTANT:** Fix method calls to use `GetContent`
3. **NICE TO HAVE:** Keep checkpoints (help first node)

### Checkpoints Decision:
✅ **Keep them** - They help Node A (first pull) do lazy reads from OCI
⚠️ **Not critical** - Real value is in ContentCache range reads (Node B+)

### Expected Result After Fix:
- Node A: Same speed (or faster with checkpoint lazy reads)
- Node B+: **Massive improvement** (range reads, no full layer download)
- Bandwidth: **3-10× reduction** across cluster
- Cold start: **50ms instead of 1s+**

---

**Should I implement this fix?**
