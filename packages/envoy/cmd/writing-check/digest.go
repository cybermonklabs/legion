package main

import (
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/sjawhar/envoy/cmd/writing-check/harness"
)

// The bundle's digest and its files are package harness's (harness.BundleDigest), which
// `harness score` records for change 7a, the bundle run in process.

func runDigest(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("writing-check digest", flag.ContinueOnError)
	flags.SetOutput(stderr)
	root := flags.String("root", ".", "the module root, packages/envoy")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() > 0 {
		fmt.Fprintln(stderr, "writing-check digest: --root is the only flag, and nothing else")
		return 2
	}
	digest, files, err := harness.BundleDigest(*root)
	if err != nil {
		fmt.Fprintf(stderr, "writing-check digest: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "bundle digest: %s\nfiles (%d, in digest order):\n", digest, len(files))
	for _, f := range files {
		fmt.Fprintf(stdout, "%s  %s\n", f.SHA256, f.Path)
	}
	return 0
}

// errNoDigest is the refusal of a final-test run whose bundle digest cannot be read.
var errNoDigest = errors.New("a final-test run names the bundle digest: run from packages/envoy or give --root")
