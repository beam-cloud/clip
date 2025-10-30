# OCI Indexing Cache Lookup Order Fix

## Problem Summary

The OCI indexing implementation had several critical issues with cache lookup order and hash usage:

### Issues Identified

1. **Wrong Cache Lookup Order in fsnode.go**
   - FUSE read operations were attempting to use ContentCache at the file level for OCI images
   - Using incorrect ContentHash values (sha256 of layer+path) instead of layer digests
   - This bypassed the correct 3-tier cache hierarchy in oci.go

2. **Incorrect ContentHash Generation**
   - OCI indexer was computing `ContentHash = sha256(layerDigest + filePath)` 
   - This created unique hashes per file instead of per layer
   - Logs showed lookups for these wrong hashes that couldn't be found
   - Example from logs: `6e667a703fd834a0df67bb9c90eece6bc612282da3c18696fc4425cfc6186953`

3. **Duplicate Caching Logic**
   - Caching was being attempted in both fsnode.go and oci.go
   - For OCI images, all caching should be handled in the storage layer (oci.go)
   - fsnode-level caching is only for legacy archives

## Solution Implemented

### 1. Fixed fsnode.go Read Method

**Location:** `pkg/clip/fsnode.go` lines 119-159

**Changes:**
- Detect OCI mode by checking if `node.Remote != nil`
- For OCI images: delegate ALL caching to storage layer
- For legacy archives: maintain existing file-level ContentCache behavior

```go
// For OCI images (v2 with Remote), delegate ALL caching to the storage layer
// The storage layer (oci.go) handles the proper 3-tier cache hierarchy:
//   1. Disk cache (local)
//   2. ContentCache with layer digest (remote)
//   3. OCI registry (download + decompress)
if n.clipNode.Remote != nil {
    // OCI mode - storage layer handles all caching
    nRead, err = n.filesystem.storage.ReadFile(n.clipNode, dest[:readLen], off)
    // ...
} else {
    // Legacy mode - use file-level ContentCache
    // ...
}
```

### 2. Fixed ContentHash Generation in oci_indexer.go

**Location:** `pkg/clip/oci_indexer.go` lines 324-331

**Changes:**
- Removed incorrect ContentHash computation for OCI images
- ContentHash is intentionally not set for OCI images
- Caching is handled at the layer level using `Remote.LayerDigest`

```go
// For OCI images, we don't need ContentHash at the file level
// The Remote field with LayerDigest is used for content-addressed caching
// ContentHash is only used for legacy archives with file-level caching
node := &common.ClipNode{
    Path:     cleanPath,
    NodeType: common.FileNode,
    // ContentHash is intentionally not set for OCI images
    // Caching is handled at the layer level using Remote.LayerDigest
    // ...
}
```

### 3. Improved Cache Logging in oci.go

**Location:** `pkg/storage/oci.go` lines 150-190, 234-238, 357-364, 474-487

**Changes:**
- Added detailed debug logging showing which cache tier is being used
- Log both layer digest and cache key (hex hash) for debugging
- Clear messages for: DISK CACHE HIT, CONTENT CACHE HIT, OCI CACHE MISS

**Example logs:**
```
DISK CACHE HIT - using local decompressed layer
  layer: sha256:abc123...
  cache_key: abc123...
  offset: 1000
  length: 5000

CONTENT CACHE HIT - range read from remote
  layer: sha256:abc123...
  cache_key: abc123...
  offset: 1000
  length: 5000
  bytes_read: 5000

OCI CACHE MISS - downloading and decompressing layer from registry
  layer: sha256:abc123...
  cache_key: abc123...
```

## Correct Cache Hierarchy (After Fix)

### OCI Images (v2 mode - has Remote field)

```
File Read Request
    ↓
fsnode.go detects OCI mode (Remote != nil)
    ↓
Delegate to storage.ReadFile()
    ↓
oci.go ReadFile() - 3-tier cache:
    ├─ 1. Check disk cache (fastest)
    │    └─ Range read from: /tmp/clip-oci-cache/sha256_abc123...
    │
    ├─ 2. Check ContentCache (fast, network)
    │    └─ Range read with: cache_key="abc123..." (layer digest hex)
    │    └─ GetContent(cache_key, offset, length)
    │
    └─ 3. Download from OCI (slow, first time only)
         └─ Download compressed layer
         └─ Decompress entire layer
         └─ Cache to disk (for local range reads)
         └─ Cache to ContentCache (for cluster range reads)
         └─ Range read from newly cached layer
```

### Legacy Archives (has DataLen, no Remote)

```
File Read Request
    ↓
fsnode.go detects legacy mode (Remote == nil)
    ↓
Check ContentCache with file's ContentHash
    ├─ Hit: Return cached content
    └─ Miss: Read from storage + async cache file
```

## Key Differences

| Aspect | OCI Images (v2) | Legacy Archives |
|--------|----------------|-----------------|
| **Cache Level** | Layer-level (shared across files) | File-level (per file) |
| **Cache Key** | Layer digest hex (e.g., `abc123...`) | File ContentHash (sha256 of content) |
| **Handled By** | oci.go storage layer | fsnode.go + legacy storage |
| **Optimizations** | Range reads, layer sharing | Full file caching |

## Hash Usage Clarification

### Before Fix (WRONG)
```
ContentHash = sha256(layerDigest + filePath)
Example: 6e667a703fd834a0df67bb9c90eece6bc612282da3c18696fc4425cfc6186953
└─ Looking up: NOT FOUND (doesn't exist in cache)
```

### After Fix (CORRECT)
```
Layer Digest: sha256:abc123def456...
Cache Key: abc123def456... (hex only)
└─ Looking up: layer-level cache (shared across all files in layer)
```

## Benefits

1. **Correct Cache Lookups**: No more lookups for non-existent hashes
2. **Better Cache Efficiency**: Layer-level caching means better hit rates
3. **Proper Cache Priority**: 
   - Disk cache first (local, fastest)
   - ContentCache second (network, but only bytes needed)
   - OCI registry last (download + decompress, slowest)
4. **Clear Debugging**: Logs show exactly which cache tier is being used
5. **Cross-Image Sharing**: Same layer in different images uses same cache key

## Testing

All tests pass:
```bash
$ go test ./pkg/clip ./pkg/storage -short
ok  	github.com/beam-cloud/clip/pkg/clip	1.733s
ok  	github.com/beam-cloud/clip/pkg/storage	17.037s
```

## Files Changed

1. `pkg/clip/fsnode.go` - Fixed cache lookup order
2. `pkg/clip/oci_indexer.go` - Removed incorrect ContentHash generation
3. `pkg/storage/oci.go` - Improved logging and documentation
4. Removed unused imports from oci_indexer.go (crypto/sha256, encoding/hex)

## Migration Impact

**No breaking changes** - This is a bug fix that:
- Makes the implementation work as originally intended
- No API changes
- No storage format changes
- Existing .clip files work unchanged

## Performance Impact

**Positive impact:**
- Eliminates unnecessary cache lookups for non-existent keys
- Proper cache hierarchy means better hit rates
- Layer-level caching more efficient than file-level for OCI images

## Verification

To verify the fix is working, check logs for:
1. **No more** `Get - [<wrong-hash>] - content not found` messages
2. **Should see** `DISK CACHE HIT` or `CONTENT CACHE HIT` messages
3. Cache keys should be layer digests (hex), not computed hashes
4. Example good log: `cache_key: abc123def456...` (from layer digest `sha256:abc123def456...`)

## Related Documentation

- See `BRANCH_SUMMARY.md` for complete architecture overview
- See `RANGE_READ_FIX.md` for ContentCache range read implementation
- See `CONTENT_ADDRESSED_CACHE.md` for cache key design
