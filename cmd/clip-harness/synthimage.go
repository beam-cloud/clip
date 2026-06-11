package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http/httptest"
	"path"
	"sort"
	"strings"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
)

// synthEntry describes one tar entry in a synthetic layer.
type synthEntry struct {
	name     string
	typeflag byte
	content  []byte
	linkname string
	mode     int64
}

// fixedTime keeps all synthetic tar headers deterministic.
var fixedTime = time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

// synthLayers returns layer definitions that exercise regular files,
// directories, symlinks, hard links, whiteouts, opaque whiteouts, and a file
// large enough to force multiple gzip checkpoints.
func synthLayers() [][]synthEntry {
	// Deterministic pseudo-random payload (~5 MiB) to force checkpoints.
	rng := rand.New(rand.NewSource(42))
	bigPayload := make([]byte, 5*1024*1024)
	rng.Read(bigPayload)

	return [][]synthEntry{
		{ // layer 1: base filesystem
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
			{name: "usr/lib/big.bin", typeflag: tar.TypeReg, content: bigPayload, mode: 0644},
		},
		{ // layer 2: whiteout + opaque whiteout + replacement
			{name: "etc/.wh.old.conf", typeflag: tar.TypeReg, mode: 0644},
			{name: "etc/keep.conf", typeflag: tar.TypeReg, content: []byte("replaced"), mode: 0600},
			{name: "opt/.wh..wh..opq", typeflag: tar.TypeReg, mode: 0644},
			{name: "opt/fresh.txt", typeflag: tar.TypeReg, content: []byte("fresh"), mode: 0644},
		},
		{ // layer 3: symlinks and additions
			{name: "srv/", typeflag: tar.TypeDir, mode: 0755},
			{name: "srv/app.txt", typeflag: tar.TypeReg, content: []byte("app"), mode: 0644},
			{name: "srv/link-to-app", typeflag: tar.TypeSymlink, linkname: "app.txt", mode: 0777},
			{name: "bin/tool2", typeflag: tar.TypeReg, content: []byte("#!/bin/sh\necho tool2\n"), mode: 0755},
		},
	}
}

// buildLayerBlob produces a deterministic gzipped tarball for the entries.
func buildLayerBlob(entries []synthEntry) ([]byte, error) {
	var tarBuf bytes.Buffer
	tw := tar.NewWriter(&tarBuf)
	for _, e := range entries {
		hdr := &tar.Header{
			Name:     e.name,
			Typeflag: e.typeflag,
			Mode:     e.mode,
			Linkname: e.linkname,
			ModTime:  fixedTime,
			Format:   tar.FormatPAX,
		}
		if e.typeflag == tar.TypeReg {
			hdr.Size = int64(len(e.content))
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return nil, err
		}
		if e.typeflag == tar.TypeReg && len(e.content) > 0 {
			if _, err := tw.Write(e.content); err != nil {
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

// buildSynthImage composes the synthetic layers into an OCI image with a
// fixed created time so indexing output is fully deterministic.
func buildSynthImage() (v1.Image, error) {
	img := empty.Image
	for i, entries := range synthLayers() {
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
	return mutate.CreatedAt(img, v1.Time{Time: fixedTime})
}

// startSynthRegistry serves an in-memory OCI registry, pushes the synthetic
// image, and returns its reference plus a shutdown func.
func startSynthRegistry() (string, func(), error) {
	server := httptest.NewServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))

	img, err := buildSynthImage()
	if err != nil {
		server.Close()
		return "", nil, err
	}

	host := strings.TrimPrefix(server.URL, "http://")
	imageRef := fmt.Sprintf("%s/harness/synth:latest", host)
	ref, err := name.ParseReference(imageRef)
	if err != nil {
		server.Close()
		return "", nil, err
	}
	if err := remote.Write(ref, img); err != nil {
		server.Close()
		return "", nil, fmt.Errorf("failed to push synthetic image: %w", err)
	}

	return imageRef, server.Close, nil
}

// groundTruthNode is an independently computed expectation for one path.
type groundTruthNode struct {
	kind   string // "dir", "file", "symlink"
	size   int64
	target string
}

// computeGroundTruth applies OCI overlay semantics to the synthetic layer
// definitions using a simple map-based implementation, fully independent of
// the clip indexer code paths under test.
func computeGroundTruth() map[string]groundTruthNode {
	tree := map[string]groundTruthNode{
		"/": {kind: "dir"},
	}

	deletePrefix := func(prefix string) {
		for p := range tree {
			if strings.HasPrefix(p, prefix) {
				delete(tree, p)
			}
		}
	}

	for _, entries := range synthLayers() {
		for _, e := range entries {
			clean := path.Clean("/" + strings.TrimPrefix(e.name, "./"))
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

			switch e.typeflag {
			case tar.TypeDir:
				tree[clean] = groundTruthNode{kind: "dir"}
			case tar.TypeReg:
				tree[clean] = groundTruthNode{kind: "file", size: int64(len(e.content))}
			case tar.TypeSymlink:
				tree[clean] = groundTruthNode{kind: "symlink", target: e.linkname, size: int64(len(e.linkname))}
			case tar.TypeLink:
				target := path.Clean("/" + strings.TrimPrefix(e.linkname, "./"))
				if tn, ok := tree[target]; ok {
					tree[clean] = groundTruthNode{kind: "file", size: tn.size}
				}
			}
		}
	}

	return tree
}

func sortedPaths(m map[string]groundTruthNode) []string {
	paths := make([]string, 0, len(m))
	for p := range m {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	return paths
}
