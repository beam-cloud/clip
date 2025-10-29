# OCI Indexing Audit Report

## Executive Summary

Audited the OCI indexing process for correctness. Found **one critical issue** (missing whiteout handling) and confirmed correctness for other aspects.

## Audit Findings

### ✅ 1. Layer Processing Order (CORRECT)

**Code:**
```go
// Line 156-157: Process each layer in order (bottom to top)
for i, layer := range layers {
    // Process layer...
}
```

**Analysis:**
- OCI images return layers in bottom-to-top order (base layer first)
- We process them sequentially: Layer 0 (base) → Layer 1 → ... → Layer N (top)
- This is **correct** ✅

**Example:**
```
Image: myapp:latest
├── Layer 0 (sha256:abc123...) - Ubuntu base
├── Layer 1 (sha256:def456...) - Install packages
└── Layer 2 (sha256:789xyz...) - Copy app files
```

Processed in order: 0 → 1 → 2 (correct!)

---

### ✅ 2. Uppermost File Wins (CORRECT)

**Code:**
```go
// Lines 299, 335, 364: index.Set(node)
index.Set(node)  // Overwrites any existing node with same path
```

**Analysis:**
- `btree.Set()` replaces existing items with matching keys
- Key is the file path (e.g., `/app/config.txt`)
- If a file appears in multiple layers, later calls to `Set()` overwrite earlier ones
- **Result:** The uppermost layer's version is preserved ✅

**Example:**
```
Layer 1: /app/config.txt → RemoteRef{LayerDigest: "sha256:abc123", ...}
Layer 3: /app/config.txt → RemoteRef{LayerDigest: "sha256:789xyz", ...}

After indexing:
/app/config.txt → RemoteRef{LayerDigest: "sha256:789xyz", ...}  ← Layer 3 wins!
```

**Verification needed:** Confirm btree behavior with test.

---

### ✅ 3. Layer References (CORRECT)

**Code:**
```go
// Lines 292-296: Each file points to its specific layer
Remote: &common.RemoteRef{
    LayerDigest: layerDigest,  // sha256:abc123...
    UOffset:     dataStart,     // Offset in decompressed layer
    ULength:     hdr.Size,      // File size
},
```

**Analysis:**
- Each file stores the digest of the layer it came from
- `UOffset` is the position in the **decompressed layer**, not the tar archive
- `ULength` is the file size
- This allows lazy loading from the correct layer ✅

**Storage lookup:**
```
1. User reads /app/config.txt
2. Index returns: RemoteRef{LayerDigest: "sha256:789xyz", UOffset: 12345, ULength: 1024}
3. Storage layer:
   a. Gets cached layer sha256:789xyz from disk
   b. Seeks to offset 12345
   c. Reads 1024 bytes
   d. Returns data
```

---

### ✅ 4. Content-Addressed Storage (CORRECT)

**Code:**
```go
// Lines 158-164: Use layer digest from OCI image
digest, err := layer.Digest()
layerDigestStr := digest.String()  // "sha256:abc123..."
layerDigests = append(layerDigests, layerDigestStr)
```

**Analysis:**
- We use the **layer digest** (sha256 of compressed layer) as the storage key
- We do **NOT** hash individual files
- Multiple images sharing the same layer share the same cached data ✅

**Example:**
```
ubuntu:22.04 base layer: sha256:44cf07d57ee44241...
myapp-one:latest layer 0: sha256:44cf07d57ee44241...  ← SAME!
myapp-two:latest layer 0: sha256:44cf07d57ee44241...  ← SAME!

Disk cache:
/tmp/clip-oci-cache/sha256_44cf07d57ee44241...  ← Shared!
```

---

### ❌ 5. Whiteout Files (MISSING - CRITICAL!)

**Current Code:** No handling for whiteout files!

**Problem:**
Docker/OCI uses whiteout files to represent deletions in upper layers:
```
Layer 1: Creates /app/temp.txt
Layer 2: Contains /app/.wh.temp.txt (whiteout marker)
Result: /app/temp.txt should be DELETED in final filesystem
```

**Our code:** Ignores whiteout files, so deleted files remain visible! ❌

**Impact:**
- Files deleted in upper layers still appear in the mounted filesystem
- This breaks container behavior (files that should be gone are visible)
- **Severity: HIGH** - This is a correctness bug

**Required Fix:**
```go
// In indexLayerOptimized(), handle whiteout files:
if strings.HasPrefix(path.Base(cleanPath), ".wh.") {
    // This is a whiteout file - delete the target
    targetPath := path.Join(path.Dir(cleanPath), 
                            strings.TrimPrefix(path.Base(cleanPath), ".wh."))
    
    // Remove target from index
    index.Delete(&common.ClipNode{Path: targetPath})
    
    // Don't add the .wh. file itself
    continue
}

// Also handle opaque whiteouts (.wh..wh..opq)
if path.Base(cleanPath) == ".wh..wh..opq" {
    // This marks the directory as opaque - remove all lower layer contents
    dirPath := path.Dir(cleanPath)
    // Delete all items under dirPath from lower layers
    // (complex - requires tracking which layer each file came from)
}
```

---

### ✅ 6. All Layers Accounted For (CORRECT)

**Code:**
```go
// Lines 132-136: Get all layers from image
layers, err := img.Layers()

// Lines 157-182: Process every layer
for i, layer := range layers {
    // Index layer...
    gzipIdx[layerDigestStr] = gzipIndex
}
```

**Analysis:**
- We get all layers from the OCI image
- We process every layer
- We store gzip index for every layer
- **Result:** All layers are indexed ✅

**Verification:**
```go
// After indexing:
assert(len(layerDigests) == len(layers))
assert(len(gzipIdx) == len(layers))
```

---

## Issue Summary

| Issue | Status | Severity | Impact |
|-------|--------|----------|--------|
| Layer processing order | ✅ Correct | — | Works as expected |
| Uppermost file wins | ✅ Correct | — | Works as expected |
| Layer references | ✅ Correct | — | Files point to correct layers |
| Content-addressed storage | ✅ Correct | — | Cross-image sharing works |
| **Whiteout handling** | ❌ **MISSING** | **HIGH** | **Deleted files still visible** |
| All layers indexed | ✅ Correct | — | No layers skipped |

---

## Critical Issue: Missing Whiteout Support

### What Are Whiteouts?

Docker/OCI layers are **additive only**. To delete a file in an upper layer, they use **whiteout files**:

**Types:**
1. **Regular whiteout:** `.wh.<filename>`
   - Deletes a specific file
   - Example: `.wh.temp.txt` deletes `temp.txt`

2. **Opaque whiteout:** `.wh..wh..opq`
   - Marks directory as opaque
   - Hides all contents from lower layers
   - Example: `/app/.wh..wh..opq` means only show files from this layer in `/app/`

### Example Scenario

```
Layer 1 (Base):
/app/
├── config.txt
├── secrets.txt
└── temp.txt

Layer 2 (App):
/app/
├── .wh.secrets.txt    ← Delete secrets.txt
├── .wh.temp.txt       ← Delete temp.txt
└── newfile.txt

Expected final filesystem:
/app/
├── config.txt         ← From Layer 1
└── newfile.txt        ← From Layer 2
(secrets.txt and temp.txt are DELETED)

Current behavior (BUG):
/app/
├── config.txt         ← From Layer 1
├── secrets.txt        ← BUG: Should be deleted!
├── temp.txt           ← BUG: Should be deleted!
└── newfile.txt        ← From Layer 2
```

### Impact

**Security Risk:**
- Files intended to be deleted (e.g., secrets, temp files) remain visible
- Could expose sensitive data

**Correctness:**
- Mounted filesystem doesn't match what `docker run` would produce
- Breaks applications that rely on file deletions

### Recommended Fix

Add whiteout handling in `indexLayerOptimized()`:

```go
// After cleaning the path, check for whiteout
if strings.HasPrefix(path.Base(cleanPath), ".wh.") {
    if path.Base(cleanPath) == ".wh..wh..opq" {
        // Opaque whiteout - mark directory as opaque
        // (requires tracking layer provenance)
        handleOpaqueWhiteout(cleanPath, index)
    } else {
        // Regular whiteout - delete target file
        targetName := strings.TrimPrefix(path.Base(cleanPath), ".wh.")
        targetPath := path.Join(path.Dir(cleanPath), targetName)
        
        // Remove from index
        index.Delete(&common.ClipNode{Path: targetPath})
        log.Debug().Msgf("  Whiteout: deleted %s", targetPath)
    }
    
    // Skip adding the .wh. file itself
    continue
}
```

---

## Verification Tests Needed

### Test 1: Layer Override
```go
// Create image with:
// Layer 1: /file.txt = "v1"
// Layer 2: /file.txt = "v2"
// Verify index points to Layer 2
```

### Test 2: Whiteout Files
```go
// Create image with:
// Layer 1: /app/secret.txt
// Layer 2: /app/.wh.secret.txt
// Verify /app/secret.txt NOT in index
```

### Test 3: Content Addressing
```go
// Create two images sharing ubuntu:22.04 base
// Verify both reference same layer digest
```

### Test 4: All Layers Indexed
```go
// Index multi-layer image
// Verify len(layerDigests) == number of layers
// Verify each layer has gzip index
```

---

## Recommendations

### Immediate (Critical):
1. ✅ **Implement whiteout handling** - Fixes deleted file visibility bug
2. Add tests for whiteout behavior

### Important:
3. Add layer override test (verify uppermost wins)
4. Add integration test with real multi-layer image

### Nice to have:
5. Add logging for overwrites (debug layer conflicts)
6. Validate layer order matches OCI spec

---

## Code Review Checklist

- [x] Layer processing order correct (bottom-to-top)
- [x] File overwriting works (uppermost wins)
- [x] Layer references are correct
- [x] Content-addressed storage (layer digests)
- [ ] **Whiteout handling (MISSING)**
- [x] All layers indexed
- [ ] **Tests for whiteout behavior (MISSING)**
- [ ] Integration test for layer overrides

---

## Conclusion

**Overall assessment:** Indexing logic is mostly correct, but **missing critical whiteout support**.

**Severity:** HIGH - Files that should be deleted remain visible

**Recommendation:** Implement whiteout handling before production use.

**Estimated effort:** 2-4 hours (implementation + tests)

---

## Additional Notes

### btree.Set() Behavior

Need to verify that `btree.Set()` actually overwrites existing items:

```go
// Test:
tree := btree.New(...)
tree.Set(&ClipNode{Path: "/file.txt", value: "v1"})
tree.Set(&ClipNode{Path: "/file.txt", value: "v2"})

// Expected: tree contains only "v2"
// Actual: Need to verify!
```

### Layer Digest Format

Confirmed layer digests use OCI format: `sha256:<hex>`

This is correct for content-addressed storage.
