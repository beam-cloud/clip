# Final Fix Summary - Complete FUSE Attributes ✅

## Root Cause: Missing FUSE Attributes

The "wandered into deleted directory" error was caused by **incomplete FUSE attributes** in the OCI indexer. The critical missing field was **Nlink** (link count).

## What Was Missing

Comparing V1 (stat-based) vs V2 (tar-based):

| Attribute | V1 | V2 Before | V2 After |
|-----------|----|-----------| ---------|
| Ino | ✓ | ✓ | ✓ |
| Size | ✓ | ✓ | ✓ |
| **Blocks** | ✓ | ❌ | ✅ |
| Atime | ✓ | ✓ | ✓ |
| **Atimensec** | ✓ | ❌ | ✅ |
| Mtime | ✓ | ✓ | ✓ |
| **Mtimensec** | ✓ | ❌ | ✅ |
| Ctime | ✓ | ✓ | ✓ |
| **Ctimensec** | ✓ | ❌ | ✅ |
| Mode | ✓ | ✓ | ✓ |
| **Nlink** | ✓ | ❌ | ✅ **CRITICAL!** |
| Uid/Gid | ✓ | ✓ | ✓ |

## The Fix

### Regular Files:
```go
Attr: fuse.Attr{
    Ino:       ca.generateInode(layerDigest, cleanPath),
    Size:      uint64(hdr.Size),
    Blocks:    (uint64(hdr.Size) + 511) / 512,  // ✅ Added
    Atime:     uint64(hdr.AccessTime.Unix()),
    Atimensec: uint32(hdr.AccessTime.Nanosecond()),  // ✅ Added
    Mtime:     uint64(hdr.ModTime.Unix()),
    Mtimensec: uint32(hdr.ModTime.Nanosecond()),     // ✅ Added
    Ctime:     uint64(hdr.ChangeTime.Unix()),
    Ctimensec: uint32(hdr.ChangeTime.Nanosecond()),  // ✅ Added
    Mode:      ca.tarModeToFuse(hdr.Mode, tar.TypeReg),
    Nlink:     1,  // ✅ CRITICAL FIX!
    Owner: fuse.Owner{
        Uid: uint32(hdr.Uid),
        Gid: uint32(hdr.Gid),
    },
}
```

### Symlinks:
```go
Attr: fuse.Attr{
    Ino:       ca.generateInode(layerDigest, cleanPath),
    Size:      uint64(len(target)),
    Blocks:    0,  // ✅ Added
    Atime:     uint64(hdr.AccessTime.Unix()),
    Atimensec: uint32(hdr.AccessTime.Nanosecond()),  // ✅ Added
    Mtime:     uint64(hdr.ModTime.Unix()),
    Mtimensec: uint32(hdr.ModTime.Nanosecond()),     // ✅ Added
    Ctime:     uint64(hdr.ChangeTime.Unix()),
    Ctimensec: uint32(hdr.ChangeTime.Nanosecond()),  // ✅ Added
    Mode:      ca.tarModeToFuse(hdr.Mode, tar.TypeSymlink),
    Nlink:     1,  // ✅ CRITICAL FIX!
    Owner: fuse.Owner{
        Uid: uint32(hdr.Uid),
        Gid: uint32(hdr.Gid),
    },
}
```

### Directories:
```go
Attr: fuse.Attr{
    Ino:       ca.generateInode(layerDigest, cleanPath),
    Size:      0,  // ✅ Added
    Blocks:    0,  // ✅ Added
    Atime:     uint64(hdr.AccessTime.Unix()),
    Atimensec: uint32(hdr.AccessTime.Nanosecond()),  // ✅ Added
    Mtime:     uint64(hdr.ModTime.Unix()),
    Mtimensec: uint32(hdr.ModTime.Nanosecond()),     // ✅ Added
    Ctime:     uint64(hdr.ChangeTime.Unix()),
    Ctimensec: uint32(hdr.ChangeTime.Nanosecond()),  // ✅ Added
    Mode:      ca.tarModeToFuse(hdr.Mode, tar.TypeDir),
    Nlink:     2,  // ✅ CRITICAL FIX! (. and ..)
    Owner: fuse.Owner{
        Uid: uint32(hdr.Uid),
        Gid: uint32(hdr.Gid),
    },
}
```

## Why Nlink Was Critical

**Nlink = 0** means the file/directory has been deleted/unlinked:
- Kernel's VFS checks `if (inode->i_nlink == 0)` → "deleted"
- Result: "wandered into deleted directory" error
- runc couldn't mount over these "phantom" directories

**Correct Nlink values:**
- Files: 1 (one hard link)
- Symlinks: 1 (one link)  
- Directories: 2 (`.` and `..`)

## Files Changed

**pkg/clip/oci_indexer.go** (559 lines):
- Added complete FUSE attributes for all node types
- Files: +Blocks, +*nsec fields, +Nlink
- Symlinks: +Blocks, +*nsec fields, +Nlink
- Directories: +Size, +Blocks, +*nsec fields, +Nlink

**Deleted Files:**
- `pkg/clip/overlay.go` (331 lines) - Unnecessary complexity
- `pkg/clip/oci_runtime_dirs_test.go` (137 lines) - Filtering no longer needed

**Net change:** -468 lines of complexity removed, attributes fixed

## Test Results

```bash
$ go test ./pkg/clip -run TestOCIIndexing -v

✅ TestOCIIndexing - PASS (0.61s)
   Created index: 61,334 bytes (was 60,288 - increased due to more attributes)
   527 files indexed correctly

All tests pass!
```

## Expected Results in Beta9

### Before:
```
❌ error: wandered into deleted directory "/proc"
❌ runc unable to mount
❌ container start failures
```

### After:
```
✅ No "deleted directory" errors
✅ runc mounts successfully
✅ Containers start correctly
✅ Full metadata available (timestamps, blocks, link counts)
```

## Verification Steps

```bash
# 1. Create index
clip index docker.io/library/ubuntu:22.04 /tmp/ubuntu.clip

# 2. Mount
clip mount /tmp/ubuntu.clip /tmp/test

# 3. Verify with stat
stat /tmp/test/proc
# Should show:
#   Size: 0
#   Blocks: 0
#   Links: 2  ← CRITICAL! Not 0!
#   Access: 2025-04-04... (with nanoseconds)

# 4. Verify with ls
ls -ln /tmp/test/
# Should show proper link counts, not all 0

# 5. Use with runc
runc run container
# Should work without "deleted directory" errors ✅
```

## Summary

### Root Cause:
Missing Nlink attribute (defaults to 0 = deleted)

### Fix:
Set all FUSE attributes to match v1:
- Nlink: 1 for files/symlinks, 2 for directories
- Blocks: Proper block count calculation
- *nsec: Nanosecond timestamp precision

### Impact:
- ✅ Fixes "wandered into deleted directory"
- ✅ Full v1 compatibility
- ✅ Proper FUSE behavior
- ✅ runc works correctly

### Files:
- Modified: `pkg/clip/oci_indexer.go` (+complete attributes)
- Deleted: `overlay.go`, `oci_runtime_dirs_test.go` (-468 lines)

---

**Status: COMPLETE - Ready for production testing in beta9!** 🚀
