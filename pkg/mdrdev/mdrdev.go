// Package mdrdev runs curated monetdroid development operations. Each
// operation is a subcommand; the omnibus shape lets sibling operations share
// one binary and one freshness mechanism (the tools/mdrdev wrapper runs it
// from source).
package mdrdev

import (
	"fmt"
	"os"
)

// Run dispatches a mdrdev subcommand and returns its exit code.
func Run(args []string) int {
	if len(args) == 0 {
		usage()
		return 2
	}
	switch args[0] {
	case "test":
		return runTest(args[1:], false)
	case "record-cassette":
		return runTest(args[1:], true)
	case "help", "-h", "--help":
		usage()
		return 0
	default:
		fmt.Fprintf(os.Stderr, "mdrdev: unknown subcommand %q\n", args[0])
		usage()
		return 2
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `mdrdev: curated monetdroid development operations

Usage:
  mdrdev test [go test args...]            run go test -v with condensed, de-interleaved output (replay-only; -record rejected)
  mdrdev record-cassette [go test args...] run go test -v -record to record cassettes (real upstream calls)
`)
}
