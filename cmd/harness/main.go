// harness is a local validation and benchmarking tool for the CLIP OCI
// indexer.
//
// Usage:
//
//	harness verify              run determinism and correctness checks
//	harness bench [-image ref]  benchmark cold vs warm indexing of an image
//
// verify builds a reference image in-process, serves it from an in-memory
// registry, and asserts that sequential, parallel, and layer-index-cached
// indexing all produce identical output that matches independently computed
// overlay semantics.
//
// bench indexes a real image cold (sequential and parallel) and warm (layer
// index cache) and reports durations, throughput, and cache activity.
package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	var err error
	switch os.Args[1] {
	case "verify":
		err = runVerify(os.Args[2:])
	case "bench":
		err = runBench(os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "FAIL: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `usage: harness <command> [flags]

commands:
  verify              determinism and correctness checks against a reference image
  bench [-image ref]  cold vs warm indexing benchmark against a real image
`)
}
