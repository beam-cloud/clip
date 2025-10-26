package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"log"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

func main() {
	// Test reading symlinks from ubuntu image
	ref, err := name.ParseReference("docker.io/library/ubuntu:24.04")
	if err != nil {
		log.Fatal(err)
	}

	img, err := remote.Image(ref, remote.WithAuthFromKeychain(authn.DefaultKeychain), remote.WithContext(context.Background()))
	if err != nil {
		log.Fatal(err)
	}

	layers, err := img.Layers()
	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("Found %d layers\n", len(layers))

	// Check first layer for symlinks
	if len(layers) > 0 {
		layer := layers[0]
		digest, _ := layer.Digest()
		fmt.Printf("\nChecking layer: %s\n", digest.String())

		compressedRC, err := layer.Compressed()
		if err != nil {
			log.Fatal(err)
		}
		defer compressedRC.Close()

		gzr, err := gzip.NewReader(compressedRC)
		if err != nil {
			log.Fatal(err)
		}
		defer gzr.Close()

		tr := tar.NewReader(gzr)

		symlinkCount := 0
		emptyTargetCount := 0

		for {
			hdr, err := tr.Next()
			if err != nil {
				break
			}

			if hdr.Typeflag == tar.TypeSymlink {
				symlinkCount++
				if hdr.Linkname == "" {
					emptyTargetCount++
					fmt.Printf("EMPTY SYMLINK: %s -> '%s'\n", hdr.Name, hdr.Linkname)
				} else {
					fmt.Printf("Symlink: %s -> %s\n", hdr.Name, hdr.Linkname)
				}
			}
		}

		fmt.Printf("\nTotal symlinks: %d\n", symlinkCount)
		fmt.Printf("Empty targets: %d\n", emptyTargetCount)
	}
}
