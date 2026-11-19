package gofetch_test

import (
	"path/filepath"
	"testing"

	"github.com/sjawhar/envoy/internal/gofetch"
)

// A refusal by GOPROXY=off gets the fetch command, with the directory made absolute so it runs
// from the repository root, and every other failure gets nothing.
func TestHintNamesTheFetchOfARefusedLookupAlone(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	refused := "go: github.com/nats-io/nats.go@v1.53.1: module lookup disabled by GOPROXY=off"
	want := "\nThe module cache lacks a module this go command needs, and a test fetches no module while it " +
		"runs. Fetch them before the tests, from the repository root:\n  .github/scripts/go-fetch.sh deps " +
		filepath.Join(dir, "sub") + " ./a ./b"
	if got := gofetch.Hint("sub", []string{"./a", "./b"}, refused); got != want {
		t.Errorf("Hint of a refused lookup = %q, want %q", got, want)
	}
	if got := gofetch.Hint("sub", []string{"./a"}, "go: undefined: x"); got != "" {
		t.Errorf("Hint of another failure = %q, want none", got)
	}
}
