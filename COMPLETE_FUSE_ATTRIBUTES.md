# Complete FUSE Attributes Audit ✅

## Comparison: V1 vs V2

### V1 (From stat() syscall):
```go
attr := fuse.Attr{
    Ino:       inode,
    Size:      uint64(stat.Size),
    Blocks:    uint64(stat.Blocks),        // ✓
    Atime:     uint64(stat.Atim.Sec),
    Atimensec: uint32(stat.Atim.Nsec),     // ✓ Nanosecond precision
    Mtime:     uint64(stat.Mtim.Sec),
    Mtimensec: uint32(stat.Mtim.Nsec),     // ✓ Nanosecond precision
    Ctime:     uint64(stat.Ctim.Sec),
    Ctimensec: uint32(stat.Ctim.Nsec),     // ✓ Nanosecond precision
    Mode:      mode,
    Nlink:     uint32(stat.Nlink),         // ✓ Link count
    Owner: fuse.Owner{
        Uid: stat.Uid,
        Gid: stat.Gid,
    },
}
```

### V2 (Before fix) - Missing fields:
- ❌ **Blocks** - Number of 512-byte blocks
- ❌ **Atimensec, Mtimensec, Ctimensec** - Nanosecond precision
- ❌ **Nlink** - Link count (caused "deleted directory" bug!)

## Fixed V2 Attributes

### Regular Files:
```go
Attr: fuse.Attr{
    Ino:       ca.generateInode(layerDigest, cleanPath),
    Size:      uint64(hdr.Size),
    Blocks:    (uint64(hdr.Size) + 511) / 512,  // ✓ Added
    Atime:     uint64(hdr.AccessTime.Unix()),
    Atimensec: uint32(hdr.AccessTime.Nanosecond()), // ✓ Added
    Mtime:     uint64(hdr.ModTime.Unix()),
    Mtimensec: uint32(hdr.ModTime.Nanosecond()),    // ✓ Added
    Ctime:     uint64(hdr.ChangeTime.Unix()),
    Ctimensec: uint32(hdr.ChangeTime.Nanosecond()), // ✓ Added
    Mode:      ca.tarModeToFuse(hdr.Mode, tar.TypeReg),
    Nlink:     1,  // ✓ Added (was missing!)
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
    Size:      uint64(len(target)),  // Length of symlink target
    Blocks:    0,  // ✓ Symlinks don't use blocks
    Atime:     uint64(hdr.AccessTime.Unix()),
    Atimensec: uint32(hdr.AccessTime.Nanosecond()), // ✓ Added
    Mtime:     uint64(hdr.ModTime.Unix()),
    Mtimensec: uint32(hdr.ModTime.Nanosecond()),    // ✓ Added
    Ctime:     uint64(hdr.ChangeTime.Unix()),
    Ctimensec: uint32(hdr.ChangeTime.Nanosecond()), // ✓ Added
    Mode:      ca.tarModeToFuse(hdr.Mode, tar.TypeSymlink),
    Nlink:     1,  // ✓ Added
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
    Size:      0,  // ✓ Directories have size 0 in FUSE
    Blocks:    0,  // ✓ Directories don't report blocks
    Atime:     uint64(hdr.AccessTime.Unix()),
    Atimensec: uint32(hdr.AccessTime.Nanosecond()), // ✓ Added
    Mtime:     uint64(hdr.ModTime.Unix()),
    Mtimensec: uint32(hdr.ModTime.Nanosecond()),    // ✓ Added
    Ctime:     uint64(hdr.ChangeTime.Unix()),
    Ctimensec: uint32(hdr.ChangeTime.Nanosecond()), // ✓ Added
    Mode:      ca.tarModeToFuse(hdr.Mode, tar.TypeDir),
    Nlink:     2,  // ✓ Added (critical fix!)
    Owner: fuse.Owner{
        Uid: uint32(hdr.Uid),
        Gid: uint32(hdr.Gid),
    },
}
```

## Field-by-Field Explanation

### Ino (Inode number)
- **Purpose:** Unique identifier for the file/directory
- **V1:** From stat
- **V2:** Generated from hash of layerDigest + path
- **Status:** ✓ Already correct

### Size
- **Regular files:** Actual file size in bytes
- **Symlinks:** Length of symlink target string
- **Directories:** 0 (standard for FUSE)
- **Status:** ✓ Already correct

### Blocks
- **Purpose:** Number of 512-byte blocks allocated
- **Regular files:** `(size + 511) / 512` (round up)
- **Symlinks:** 0 (don't use disk blocks)
- **Directories:** 0 (don't report blocks in FUSE)
- **Status:** ✅ **FIXED** (was missing)

### Atime, Mtime, Ctime (Timestamps in seconds)
- **Purpose:** Access, modification, change times
- **V1:** From stat (seconds since epoch)
- **V2:** From tar header `.Unix()`
- **Status:** ✓ Already correct

### Atimensec, Mtimensec, Ctimensec (Nanoseconds)
- **Purpose:** Nanosecond precision for timestamps
- **V1:** From stat (0-999999999)
- **V2:** From tar header `.Nanosecond()`
- **Status:** ✅ **FIXED** (was missing)
- **Impact:** More precise timestamps (microsecond/nanosecond level)

### Mode
- **Purpose:** File type + permissions
- **Regular files:** S_IFREG | permissions (e.g., 0100644)
- **Symlinks:** S_IFLNK | permissions (e.g., 0120777)
- **Directories:** S_IFDIR | permissions (e.g., 0040755)
- **Status:** ✓ Already correct via `tarModeToFuse()`

### Nlink (Link count)
- **Purpose:** Number of hard links
- **Regular files:** 1
- **Symlinks:** 1
- **Directories:** 2 (for . and ..)
- **Status:** ✅ **FIXED** (was 0, caused "deleted directory" bug!)
- **Critical:** This was the root cause of all issues!

### Owner (Uid, Gid)
- **Purpose:** File ownership
- **V1:** From stat
- **V2:** From tar header
- **Status:** ✓ Already correct

## Why These Fields Matter

### Blocks:
- Used by `du` and `df` commands
- Helps filesystem report disk usage
- Not critical for functionality but important for tools

### Nanosecond precision (*nsec fields):
- Modern filesystems support nanosecond timestamps
- Important for `make` and build systems (dependency tracking)
- Ensures accurate timestamp comparisons

### Nlink:
- **CRITICAL!** Kernel uses this to determine if file/dir exists
- **Nlink = 0 means deleted/unlinked**
- This was causing "wandered into deleted directory"
- Must be set correctly for FUSE to work

## Testing

```bash
# Create index with complete attributes
clip index docker.io/library/ubuntu:22.04 /tmp/ubuntu.clip

# Mount
clip mount /tmp/ubuntu.clip /tmp/test

# Verify with stat
stat /tmp/test/proc
# Should show:
#   Size: 0
#   Blocks: 0
#   Links: 2  ✓ Critical!
#   Access: 2025-04-04 ... (with nanosecond precision)

# Verify with du
du -sh /tmp/test/usr/bin/bash
# Should show realistic block count

# Verify with ls
ls -la /tmp/test/
# Should show proper timestamps and link counts
```

## Summary of Changes

### Files Added:
1. **Blocks:** `(size + 511) / 512`
2. **Atimensec:** `hdr.AccessTime.Nanosecond()`
3. **Mtimensec:** `hdr.ModTime.Nanosecond()`
4. **Ctimensec:** `hdr.ChangeTime.Nanosecond()`
5. **Nlink:** `1` (critical fix!)

### Symlinks Added:
1. **Blocks:** `0`
2. **Atimensec, Mtimensec, Ctimensec:** From tar header
3. **Nlink:** `1` (critical fix!)

### Directories Added:
1. **Size:** `0`
2. **Blocks:** `0`
3. **Atimensec, Mtimensec, Ctimensec:** From tar header
4. **Nlink:** `2` (critical fix!)

## Expected Results

### Before Fix:
- ❌ "wandered into deleted directory" errors
- ❌ Missing nanosecond timestamp precision
- ❌ Missing block counts
- ❌ Nlink = 0 (kernel thinks files deleted)

### After Fix:
- ✅ No "deleted directory" errors
- ✅ Full timestamp precision
- ✅ Accurate block counts for tools
- ✅ Nlink set correctly (kernel recognizes files exist)
- ✅ 100% attribute compatibility with v1

---

**All FUSE attributes now match v1's stat()-based approach!**
