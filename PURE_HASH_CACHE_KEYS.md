# Pure Hash Cache Keys - Final Implementation

## Change Summary

Simplified cache keys to use **just the hex hash** (no algorithm prefix) for true content-addressed storage.

### Before
```
Layer digest: sha256:239fb06d94222b78c6bf9f52b4ef8a0a92dd49e66d7f1ea0a9ea0450a0ba738c
Cache key:    sha256_239fb06d94222b78c6bf9f52b4ef8a0a92dd49e66d7f1ea0a9ea0450a0ba738c
```

### After
```
Layer digest: sha256:239fb06d94222b78c6bf9f52b4ef8a0a92dd49e66d7f1ea0a9ea0450a0ba738c
Cache key:    239fb06d94222b78c6bf9f52b4ef8a0a92dd49e66d7f1ea0a9ea0450a0ba738c
```

## Rationale

### Why Use Layer Digest as Cache Key?

Even though we're caching the **decompressed** layer, we use the **compressed layer digest** (from OCI manifest) as the cache key:

1. **Canonical Identifier**: The layer digest is the official identifier in OCI spec
2. **Consistency**: Same layer in different images = same digest
3. **No Recomputation**: Don't need to hash the decompressed data
4. **Clear Mapping**: "Give me the decompressed version of layer sha256:abc123"
5. **Industry Standard**: Docker, containerd, and other runtimes use this approach

### Why Just the Hash (No Prefix)?

1. **Cleaner**: Simpler content-addressed storage
2. **Universal**: Hash alone is sufficient for uniqueness
3. **Shorter**: Saves space in logs and storage keys
4. **Standard**: Common pattern in content-addressed systems

## Implementation

### getContentHash Function

```go
func (s *OCIClipStorage) getContentHash(digest string) string {
    // Extract just the hex part after the colon
    // Input:  "sha256:239fb06d..."
    // Output: "239fb06d..."
    parts := strings.SplitN(digest, ":", 2)
    if len(parts) == 2 {
        return parts[1] // Just the hash
    }
    return digest // Fallback if no colon
}
```

### Usage Across System

**Disk Cache:**
```
/images/cache/239fb06d94222b78c6bf9f52b4ef8a0a92dd49e66d7f1ea0a9ea0450a0ba738c
              ↑ Just the hex hash
```

**ContentCache Store:**
```go
cacheKey := s.getContentHash("sha256:239fb06d...")
// cacheKey = "239fb06d..."
s.contentCache.StoreContent(chunks, cacheKey, ...)
```

**ContentCache Retrieve:**
```go
cacheKey := s.getContentHash("sha256:239fb06d...")
// cacheKey = "239fb06d..."
data, err := s.contentCache.GetContent(cacheKey, offset, length, ...)
```

## Benefits

### 1. True Content-Addressing ✓
```
Hash:     239fb06d94222b78c6bf9f52b4ef8a0a92dd49e66d7f1ea0a9ea0450a0ba738c
Content:  Decompressed layer data (deterministic)
Mapping:  Direct 1:1 relationship
```

### 2. Cross-Image Deduplication ✓
```
Image A: ubuntu:22.04
  Layer: sha256:239fb06d... → Cache key: 239fb06d...

Image B: ubuntu:22.04-custom  
  Layer: sha256:239fb06d... → Cache key: 239fb06d... (SAME!)
  
Result: Automatic sharing, no duplication ✓
```

### 3. Cleaner Logs ✓
```
# Before:
cache_key: sha256_239fb06d94222b78c6bf9f52b4ef8a0a92dd49e66d7f1ea0a9ea0450a0ba738c

# After:
cache_key: 239fb06d94222b78c6bf9f52b4ef8a0a92dd49e66d7f1ea0a9ea0450a0ba738c
```

### 4. Simpler Disk Structure ✓
```
/images/cache/
├── 239fb06d94222b78c6bf9f52b4ef8a0a92dd49e66d7f1ea0a9ea0450a0ba738c
├── 17113d8a7900d9e00e630fdb2795d5839fc44dc4b7c002969f39c0cd6f41a824
├── 12988d4e65587a5bf2d724b19602de581247805c1ae6298b95f29cef57aabbed
└── 4b7cba76aa7d8eda84344048fdcb1ff308af910a6fb3148926855b873e997076

Pure hex hashes, no prefixes ✓
```

## Security Considerations

### Hash Collision Risk?

**Question**: What if two different algorithms produce the same hex hash?

**Answer**: Extremely unlikely, and acceptable risk:

1. **SHA256 space**: 2^256 possibilities (practically infinite)
2. **Layer digest is authoritative**: From OCI manifest (signed/verified)
3. **Collision requires**: Malicious or corrupt registry (bigger problem)
4. **Industry standard**: Docker/containerd use same approach

### If Concerned

The original layer digest (with algorithm) is still stored in metadata:
```go
node.Remote.LayerDigest = "sha256:239fb06d..."  // Full digest preserved
```

Cache key is just for lookup:
```go
cacheKey = "239fb06d..."  // Simplified key
```

## Expected Logs

### Node A - First Access
```
INF OCI CACHE MISS - downloading and decompressing layer from registry
  layer: sha256:239fb06d94222b78c6bf9f52b4ef8a0a92dd49e66d7f1ea0a9ea0450a0ba738c
  cache_key: 239fb06d94222b78c6bf9f52b4ef8a0a92dd49e66d7f1ea0a9ea0450a0ba738c

INF Layer decompressed and cached to disk
  cache_key: 239fb06d94222b78c6bf9f52b4ef8a0a92dd49e66d7f1ea0a9ea0450a0ba738c
  disk_path: /images/cache/239fb06d94222b78c6bf9f52b4ef8a0a92dd49e66d7f1ea0a9ea0450a0ba738c

INF Storing decompressed layer in ContentCache (async)
  cache_key: 239fb06d94222b78c6bf9f52b4ef8a0a92dd49e66d7f1ea0a9ea0450a0ba738c

INF ✓ Successfully stored decompressed layer in ContentCache
  cache_key: 239fb06d94222b78c6bf9f52b4ef8a0a92dd49e66d7f1ea0a9ea0450a0ba738c
  stored_hash: 239fb06d94222b78c6bf9f52b4ef8a0a92dd49e66d7f1ea0a9ea0450a0ba738c
```

### Node B - Subsequent Access
```
DBG Trying ContentCache range read
  layer: sha256:239fb06d94222b78c6bf9f52b4ef8a0a92dd49e66d7f1ea0a9ea0450a0ba738c
  cache_key: 239fb06d94222b78c6bf9f52b4ef8a0a92dd49e66d7f1ea0a9ea0450a0ba738c

DBG CONTENT CACHE HIT - range read from remote
  cache_key: 239fb06d94222b78c6bf9f52b4ef8a0a92dd49e66d7f1ea0a9ea0450a0ba738c
  bytes_read: 53248
```

### Node A - Local Cache Hit
```
DBG DISK CACHE HIT - using local decompressed layer
  cache_key: 239fb06d94222b78c6bf9f52b4ef8a0a92dd49e66d7f1ea0a9ea0450a0ba738c
```

## Testing

All tests updated and passing:

```bash
$ go test ./pkg/clip ./pkg/storage -short
ok  	github.com/beam-cloud/clip/pkg/clip	(cached)
ok  	github.com/beam-cloud/clip/pkg/storage	17.607s

✅ ALL TESTS PASS
```

## Files Modified

1. **`pkg/storage/oci.go`** - Changed getContentHash() to extract just hex
2. **`pkg/storage/storage_test.go`** - Updated expected values
3. **`pkg/storage/cache_sharing_test.go`** - Updated TestCacheKeyFormat

## Comparison to Industry

### Docker/containerd Approach
```
Layer SHA256: sha256:239fb06d...
Cached at:    /var/lib/docker/overlay2/239fb06d.../diff
Cache key:    239fb06d... (just the hash)
```

### Our Approach (Now)
```
Layer SHA256: sha256:239fb06d...
Cached at:    /images/cache/239fb06d...
Cache key:    239fb06d... (just the hash)
```

**Result**: Consistent with industry standards ✓

## Summary

### What Changed
- Cache keys now use just the hex hash (no "sha256_" prefix)
- Disk cache paths use just the hash
- ContentCache keys use just the hash
- All consistent and clean

### Why This Is Better
- ✅ Simpler and cleaner
- ✅ True content-addressing
- ✅ Industry-standard approach
- ✅ Shorter keys and paths
- ✅ Still fully unique and secure

### Performance
- No performance impact (string is just shorter)
- Same cache hit rates
- Same deduplication benefits
- Cleaner logs for debugging

This is the final, cleanest implementation for content-addressed caching of OCI layers!
