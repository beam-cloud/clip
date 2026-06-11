package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
)

// referenceTime keeps every tar header in the reference image deterministic.
var referenceTime = time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

// layerEntry describes one tar entry in a reference image layer.
type layerEntry struct {
	name     string
	typeflag byte
	content  []byte
	linkname string
	mode     int64
}

// referenceLayers defines an image that exercises every node type the indexer
// handles: regular files, directories, symlinks, hard links, whiteouts,
// opaque whiteouts, and a payload large enough to span multiple gzip
// checkpoints.
func referenceLayers() [][]layerEntry {
	// Deterministic pseudo-random payload (~5 MiB) to force checkpoints
	rng := rand.New(rand.NewSource(42))
	payload := make([]byte, 5*1024*1024)
	rng.Read(payload)

	return [][]layerEntry{
		// Layer 1: base filesystem
		{
			{name: "bin/", typeflag: tar.TypeDir, mode: 0755},
			{name: "bin/tool", typeflag: tar.TypeReg, content: []byte("#!/bin/sh\necho tool\n"), mode: 0755},
			{name: "bin/tool-alias", typeflag: tar.TypeLink, linkname: "bin/tool", mode: 0755},
			{name: "etc/", typeflag: tar.TypeDir, mode: 0755},
			{name: "etc/old.conf", typeflag: tar.TypeReg, content: []byte("remove-me"), mode: 0644},
			{name: "etc/keep.conf", typeflag: tar.TypeReg, content: []byte("keep-me"), mode: 0644},
			{name: "opt/", typeflag: tar.TypeDir, mode: 0755},
			{name: "opt/stale.txt", typeflag: tar.TypeReg, content: []byte("stale"), mode: 0644},
			{name: "opt/sub/", typeflag: tar.TypeDir, mode: 0755},
			{name: "opt/sub/deep.txt", typeflag: tar.TypeReg, content: []byte("deep"), mode: 0644},
			{name: "usr/", typeflag: tar.TypeDir, mode: 0755},
			{name: "usr/lib/", typeflag: tar.TypeDir, mode: 0755},
			{name: "usr/lib/big.bin", typeflag: tar.TypeReg, content: payload, mode: 0644},
		},
		// Layer 2: whiteout, opaque whiteout, and file replacement
		{
			{name: "etc/.wh.old.conf", typeflag: tar.TypeReg, mode: 0644},
			{name: "etc/keep.conf", typeflag: tar.TypeReg, content: []byte("replaced"), mode: 0600},
			{name: "opt/.wh..wh..opq", typeflag: tar.TypeReg, mode: 0644},
			{name: "opt/fresh.txt", typeflag: tar.TypeReg, content: []byte("fresh"), mode: 0644},
		},
		// Layer 3: symlinks and additions
		{
			{name: "srv/", typeflag: tar.TypeDir, mode: 0755},
			{name: "srv/app.txt", typeflag: tar.TypeReg, content: []byte("app"), mode: 0644},
			{name: "srv/link-to-app", typeflag: tar.TypeSymlink, linkname: "app.txt", mode: 0777},
			{name: "bin/tool2", typeflag: tar.TypeReg, content: []byte("#!/bin/sh\necho tool2\n"), mode: 0755},
		},
	}
}

// buildLayerBlob produces a deterministic gzipped tarball for the entries.
func buildLayerBlob(entries []layerEntry) ([]byte, error) {
	var tarBuf bytes.Buffer
	tw := tar.NewWriter(&tarBuf)
	for _, entry := range entries {
		hdr := &tar.Header{
			Name:     entry.name,
			Typeflag: entry.typeflag,
			Mode:     entry.mode,
			Linkname: entry.linkname,
			ModTime:  referenceTime,
			Format:   tar.FormatPAX,
		}
		if entry.typeflag == tar.TypeReg {
			hdr.Size = int64(len(entry.content))
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return nil, err
		}
		if entry.typeflag == tar.TypeReg && len(entry.content) > 0 {
			if _, err := tw.Write(entry.content); err != nil {
				return nil, err
			}
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}

	var gzBuf bytes.Buffer
	gzw := gzip.NewWriter(&gzBuf)
	if _, err := io.Copy(gzw, &tarBuf); err != nil {
		return nil, err
	}
	if err := gzw.Close(); err != nil {
		return nil, err
	}
	return gzBuf.Bytes(), nil
}

// buildReferenceImage composes the reference layers into an OCI image with a
// fixed created time so indexing output is fully reproducible.
func buildReferenceImage() (v1.Image, error) {
	img := empty.Image
	for i, entries := range referenceLayers() {
		blob, err := buildLayerBlob(entries)
		if err != nil {
			return nil, fmt.Errorf("failed to build layer %d: %w", i, err)
		}
		layer, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(blob)), nil
		})
		if err != nil {
			return nil, fmt.Errorf("failed to create layer %d: %w", i, err)
		}
		img, err = mutate.AppendLayers(img, layer)
		if err != nil {
			return nil, fmt.Errorf("failed to append layer %d: %w", i, err)
		}
	}
	return mutate.CreatedAt(img, v1.Time{Time: referenceTime})
}

// localRegistry serves an in-memory OCI registry on a loopback address.
type localRegistry struct {
	server   *http.Server
	listener net.Listener
}

func startLocalRegistry() (*localRegistry, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("failed to start local registry listener: %w", err)
	}

	server := &http.Server{
		Handler: registry.New(registry.Logger(log.New(io.Discard, "", 0))),
	}
	go server.Serve(listener)

	return &localRegistry{server: server, listener: listener}, nil
}

func (r *localRegistry) host() string {
	return r.listener.Addr().String()
}

func (r *localRegistry) close() {
	r.server.Close()
}

// pushReferenceImage builds the reference image and pushes it to the local
// registry, returning its reference.
func pushReferenceImage(reg *localRegistry) (string, error) {
	img, err := buildReferenceImage()
	if err != nil {
		return "", err
	}

	imageRef := fmt.Sprintf("%s/harness/reference:latest", reg.host())
	ref, err := name.ParseReference(imageRef)
	if err != nil {
		return "", err
	}
	if err := remote.Write(ref, img); err != nil {
		return "", fmt.Errorf("failed to push reference image: %w", err)
	}
	return imageRef, nil
}

// expectedNode is an independently computed expectation for one path in the
// merged image filesystem.
type expectedNode struct {
	kind   string // "dir", "file", "symlink"
	size   int64
	target string
}

// expectedTree applies OCI overlay semantics to the reference layer
// definitions using a simple map-based implementation, fully independent of
// the indexer code under test.
func expectedTree() map[string]expectedNode {
	tree := map[string]expectedNode{
		"/": {kind: "dir"},
	}

	deletePrefix := func(prefix string) {
		for p := range tree {
			if strings.HasPrefix(p, prefix) {
				delete(tree, p)
			}
		}
	}

	for _, entries := range referenceLayers() {
		for _, entry := range entries {
			clean := path.Clean("/" + strings.TrimPrefix(entry.name, "./"))
			base := path.Base(clean)
			dir := path.Dir(clean)

			if base == ".wh..wh..opq" {
				deletePrefix(dir + "/")
				continue
			}
			if strings.HasPrefix(base, ".wh.") {
				victim := path.Join(dir, strings.TrimPrefix(base, ".wh."))
				delete(tree, victim)
				deletePrefix(victim + "/")
				continue
			}

			switch entry.typeflag {
			case tar.TypeDir:
				tree[clean] = expectedNode{kind: "dir"}
			case tar.TypeReg:
				tree[clean] = expectedNode{kind: "file", size: int64(len(entry.content))}
			case tar.TypeSymlink:
				tree[clean] = expectedNode{kind: "symlink", target: entry.linkname, size: int64(len(entry.linkname))}
			case tar.TypeLink:
				target := path.Clean("/" + strings.TrimPrefix(entry.linkname, "./"))
				if targetNode, ok := tree[target]; ok {
					tree[clean] = expectedNode{kind: "file", size: targetNode.size}
				}
			}
		}
	}

	return tree
}
