// Package state counts the attempts of a fixture test of .github/scripts/go-test.test.sh across
// the processes gotestsum starts: each call adds one to a counter file named for the test, under
// the directory GO_TEST_FIXTURE_STATE names, which the harness makes fresh for each case.
package state

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Attempt returns which attempt of the named fixture this is, 1 on the first.
func Attempt(t testing.TB, name string) int {
	t.Helper()
	dir := os.Getenv("GO_TEST_FIXTURE_STATE")
	if dir == "" {
		t.Fatal("GO_TEST_FIXTURE_STATE is unset: the harness names the directory the attempt counters live in")
	}
	path := filepath.Join(dir, name)
	count := 0
	if raw, err := os.ReadFile(path); err == nil {
		count, err = strconv.Atoi(strings.TrimSpace(string(raw)))
		if err != nil {
			t.Fatalf("%s holds no attempt count: %v", path, err)
		}
	} else if !os.IsNotExist(err) {
		t.Fatal(err)
	}
	count++
	if err := os.WriteFile(path, []byte(strconv.Itoa(count)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return count
}
