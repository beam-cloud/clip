// Test that symlinks serialize/deserialize correctly with gob
package main

import (
	"bytes"
	"encoding/gob"
	"fmt"
	"log"

	"github.com/beam-cloud/clip/pkg/common"
	"github.com/hanwen/go-fuse/v2/fuse"
)

func init() {
	gob.Register(&common.ClipNode{})
	gob.Register(&common.RemoteRef{})
}

func main() {
	// Create a symlink node
	original := &common.ClipNode{
		Path:     "/bin",
		NodeType: common.SymLinkNode,
		Target:   "usr/bin",
		Attr: fuse.Attr{
			Ino:  12345,
			Size: 7, // len("usr/bin")
			Mode: 0777 | 0120000, // S_IFLNK
		},
	}

	fmt.Printf("Original symlink:\n")
	fmt.Printf("  Path: %s\n", original.Path)
	fmt.Printf("  Target: '%s'\n", original.Target)
	fmt.Printf("  NodeType: %s\n", original.NodeType)
	fmt.Printf("  Size: %d\n", original.Attr.Size)

	// Encode with gob
	var buf bytes.Buffer
	enc := gob.NewEncoder(&buf)
	if err := enc.Encode(original); err != nil {
		log.Fatal("Encode failed:", err)
	}

	fmt.Printf("\nEncoded to %d bytes\n", buf.Len())

	// Decode
	dec := gob.NewDecoder(&buf)
	decoded := &common.ClipNode{}
	if err := dec.Decode(decoded); err != nil {
		log.Fatal("Decode failed:", err)
	}

	fmt.Printf("\nDecoded symlink:\n")
	fmt.Printf("  Path: %s\n", decoded.Path)
	fmt.Printf("  Target: '%s'\n", decoded.Target)
	fmt.Printf("  NodeType: %s\n", decoded.NodeType)
	fmt.Printf("  Size: %d\n", decoded.Attr.Size)

	// Check if they match
	if decoded.Target != original.Target {
		log.Fatalf("ERROR: Target mismatch! original='%s' decoded='%s'", original.Target, decoded.Target)
	}

	fmt.Printf("\n✓ Serialization test PASSED\n")
}
