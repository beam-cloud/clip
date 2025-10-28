# FUSE Metadata Testing Strategy

## Problem Statement

The user reported that mounted FUSE filesystems show incorrect timestamps:
```
drwxr-xr-x 0 root root  0 Jan  1  1970 usr/
```

This indicates that directory metadata (specifically timestamps) is not being properly preserved through the FUSE mount.

## Root Cause Analysis

The issue is likely NOT in the indexing (the index stores correct metadata from the tar), but in:

1. **FUSE Filesystem Implementation** (`pkg/clip/clipfs.go`)
   - How we're exposing metadata through FUSE `Getattr()` calls
   - Conversion from `ClipNode.Attr` to FUSE attributes

2. **ClipNode Attribute Storage** (`pkg/common/types.go`)
   - How timestamps are stored (uint64 Unix seconds)
   - Whether they're being set correctly during indexing

## Test Strategy

### Phase 1: Index Verification (Already Done)
- ✅ Verify tar headers have correct timestamps
- ✅ Verify ClipNode structures store timestamps correctly
- ✅ Check that index contains proper metadata

### Phase 2: FUSE Mount Verification (New Tests)

Created `pkg/clip/fuse_metadata_test.go` with comprehensive tests:

1. **TestFUSEMountMetadataPreservation**
   - Mounts Ubuntu 22.04 image
   - Verifies root directory metadata
   - Checks `/usr`, `/etc`, `/usr/local/bin` timestamps
   - Validates regular files have correct times
   - Tests symlink metadata
   - Confirms runtime dirs excluded
   - Uses syscall.Stat for detailed verification

2. **TestFUSEMountAlpineMetadata**
   - Lighter/faster test with Alpine
   - Quick verification of basic metadata

3. **TestFUSEMountReadFileContent**
   - Verifies file reads work correctly
   - Checks file metadata after reading

### What We're Testing

For each directory/file in the mounted FUSE filesystem:

```go
// Check 1: Timestamp is not Unix epoch (Jan 1 1970)
epoch := time.Unix(0, 0)
assert.True(t, info.ModTime().After(epoch.Add(time.Hour)),
    "Timestamp should not be Unix epoch")

// Check 2: Permissions are correct
assert.Equal(t, os.ModeDir, info.Mode()&os.ModeDir,
    "Should have directory bit set")

// Check 3: Detailed syscall verification
var stat syscall.Stat_t
syscall.Stat(path, &stat)
assert.NotZero(t, stat.Mtim.Sec, "mtime should not be zero")
assert.NotZero(t, stat.Atim.Sec, "atime should not be zero")
```

## Expected Behavior

### Correct (What We Want)
```
drwxr-xr-x 0 root root  0 Apr  4  2025 usr/
drwxr-xr-x 0 root root  0 Apr  4  2025 etc/
-rw-r--r-- 0 root root 12 Mar  1  2025 etc/hostname
```

### Incorrect (Current Problem)
```
drwxr-xr-x 0 root root  0 Jan  1  1970 usr/
drwxr-xr-x 0 root root  0 Jan  1  1970 etc/
```

## Debugging Path

If tests fail (timestamps are epoch 0), investigate:

### 1. ClipFS Getattr Implementation
**File:** `pkg/clip/clipfs.go`

```go
func (n *Node) Getattr(ctx context.Context, f fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
    // Check if we're copying times correctly
    out.Atime = node.Attr.Atime  // Should be > 0
    out.Mtime = node.Attr.Mtime  // Should be > 0
    out.Ctime = node.Attr.Ctime  // Should be > 0
    
    // Are these being set?
    log.Debug().Msgf("Getattr %s: mtime=%d", path, out.Mtime)
}
```

### 2. Index Construction
**File:** `pkg/clip/oci_indexer.go`

```go
case tar.TypeDir:
    node := &common.ClipNode{
        Attr: fuse.Attr{
            Mtime: uint64(hdr.ModTime.Unix()),  // From tar
            // Is this > 0?
            // Is hdr.ModTime valid?
        },
    }
```

### 3. Tar Header Reading
Are we reading tar headers correctly?

```go
// Add debug logging
log.Debug().Msgf("Dir %s: tar mtime=%v unix=%d", 
    cleanPath, hdr.ModTime, hdr.ModTime.Unix())
```

## Fixing the Issue

### If Problem is in FUSE Getattr:
```go
// Ensure we're setting all time fields
out.SetTimes(&node.Attr.Atime, &node.Attr.Mtime, &node.Attr.Ctime)
```

### If Problem is in Indexing:
```go
// Ensure tar times are being read
if hdr.ModTime.IsZero() {
    log.Warn().Msgf("Zero modtime for %s", cleanPath)
    // Use a default time instead of epoch
    hdr.ModTime = time.Now()
}
```

### If Problem is Time Conversion:
```go
// Verify uint64 conversion
unixTime := uint64(hdr.ModTime.Unix())
if unixTime == 0 {
    log.Warn().Msgf("Zero unix time for %s (time was %v)", 
        cleanPath, hdr.ModTime)
}
```

## Running Tests

```bash
# Run FUSE metadata tests (requires fusermount)
go test ./pkg/clip -run TestFUSEMount -v

# Expected output (if working):
✓ Root directory: mtime=2025-04-04 mode=drwxr-xr-x
✓ /usr directory: mtime=2025-04-04 mode=drwxr-xr-x
✓ /etc directory: mtime=2025-04-04 mode=drwxr-xr-x

# If skipped (no FUSE):
SKIP: Cannot mount FUSE (fusermount not available)
```

## Manual Testing

```bash
# 1. Create index
clip index docker.io/library/ubuntu:22.04 /tmp/ubuntu.clip

# 2. Mount
mkdir /tmp/test
clip mount /tmp/ubuntu.clip /tmp/test

# 3. Check timestamps
ls -la /tmp/test/usr/
# Should show: drwxr-xr-x ... Apr  4  2025 usr/
# NOT: drwxr-xr-x ... Jan  1  1970 usr/

# 4. Detailed stat
stat /tmp/test/usr
# Should show:
# Modify: 2025-04-04 ...
# NOT: Modify: 1970-01-01 ...

# 5. Syscall stat
python3 -c "import os; st=os.stat('/tmp/test/usr'); print(f'mtime={st.st_mtime}')"
# Should show: mtime=1712188800.0 (not 0.0)
```

## Next Steps

1. **Run tests** to identify where metadata is lost
2. **Add debug logging** to trace timestamp flow
3. **Fix the issue** in the appropriate layer (index vs FUSE)
4. **Verify fix** with both automated and manual tests

## Success Criteria

- ✅ All FUSE mount tests pass
- ✅ Mounted directories show correct timestamps (not Jan 1 1970)
- ✅ File metadata preserved through mount
- ✅ Symlinks have correct attributes
- ✅ Runtime directories (/proc, /sys, /dev) excluded
- ✅ Deep directory structures work correctly

---

**Status:** Tests created, waiting for execution in environment with FUSE support
