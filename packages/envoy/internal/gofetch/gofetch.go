// Package gofetch is what a test's go command, run with GOPROXY=off, adds to its failure when the
// module cache lacks a module it needs: the command that fetches that module before the tests.
// A fetch while a test runs would fail the test whenever the module proxy or the checksum
// database does, on a change that touched nothing it tests, so the tests' go commands read the
// module cache alone and a job fetches what they read in a step before them
// (.github/scripts/go-fetch.sh deps).
package gofetch

import (
	"path/filepath"
	"strings"
)

// Hint is the go-fetch.sh command that downloads the modules a go command over pkgs in dir needs,
// with why it is due, when output - what that command printed - says GOPROXY=off refused a
// module lookup, and empty for any other failure. dir is made absolute, so the command runs from
// the repository root whatever directory the test ran the go command in.
func Hint(dir string, pkgs []string, output string) string {
	if !strings.Contains(output, "GOPROXY=off") {
		return ""
	}
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	return "\nThe module cache lacks a module this go command needs, and a test fetches no module while it " +
		"runs. Fetch them before the tests, from the repository root:\n  .github/scripts/go-fetch.sh deps " +
		dir + " " + strings.Join(pkgs, " ")
}
