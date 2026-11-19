package listenerwire_test

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// goList runs `go list` with args in this package's directory and returns what it printed on
// standard output. It fetches no module: go list reads the module cache alone (GOPROXY=off), since a
// fetch while the test runs would fail it whenever the module proxy or the checksum database does,
// on a change that touched nothing it checks. envoy-go-checks fetches the modules a listing reads
// in a step before the tests; a module the cache lacks fails the listing with the command that does
// it, which fetches the modules of fetch, the packages of packages/envoy the listing reads.
func goList(t *testing.T, fetch string, args ...string) []byte {
	t.Helper()
	list := exec.Command("go", append([]string{"list"}, args...)...)
	list.Env = append(os.Environ(), "GOPROXY=off")
	var stderr bytes.Buffer
	list.Stderr = &stderr
	out, err := list.Output()
	if err != nil {
		hint := ""
		if strings.Contains(stderr.String(), "GOPROXY=off") {
			hint = "\nThe module cache lacks a module this listing needs, and a test fetches no module while it " +
				"runs. Fetch them before the tests, from the repository root:\n  .github/scripts/go-fetch.sh deps " +
				"packages/envoy " + fetch
		}
		t.Fatalf("go list %s: %v\n%s%s", strings.Join(args, " "), err, stderr.String(), hint)
	}
	return out
}
