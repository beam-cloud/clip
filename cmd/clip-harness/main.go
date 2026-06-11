// clip-harness is a local test harness for the CLIP OCI indexer.
//
// Subcommands:
//
//	verify  Build a synthetic deterministic image in-process, push it to an
//	        in-memory registry, and assert that indexing is deterministic and
//	        correct across cold, parallel, and layer-index-cached runs.
//	bench   Index a real image cold vs warm (layer index cache) and report
//	        timings and cache hit counts.
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
	fmt.Fprintln(os.Stderr, "usage: clip-harness <verify|bench> [flags]")
	fmt.Fprintln(os.Stderr, "  verify              deterministic correctness checks against a synthetic image")
	fmt.Fprintln(os.Stderr, "  bench [-image ref]  cold vs warm indexing benchmark against a real image")
}
