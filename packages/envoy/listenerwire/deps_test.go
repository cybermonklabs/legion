package listenerwire_test

import (
	"os/exec"
	"strings"
	"testing"
)

// listenerwire imports the standard library and nothing else, so a client links no package of the
// server by reading its bodies. A listing that names no package, this one included, has read
// nothing and fails as UNKNOWN.
func TestListenerwireImportsOnlyTheStandardLibrary(t *testing.T) {
	const self = "github.com/sjawhar/envoy/listenerwire"
	out, err := exec.Command("go", "list", "-deps", "-f", "{{.ImportPath}} {{.Standard}}", ".").Output()
	if err != nil {
		t.Fatalf("go list -deps: %v", err)
	}
	listed, found := 0, false
	for line := range strings.Lines(string(out)) {
		path, standard, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok {
			t.Fatalf("go list printed %q, want an import path and whether it is standard", line)
		}
		listed++
		switch {
		case path == self:
			found = true
		case standard != "true":
			t.Errorf("listenerwire imports %s, which is not the standard library", path)
		}
	}
	if listed == 0 || !found {
		t.Fatalf("UNKNOWN: go list -deps listed %d packages and %s among them: %t", listed, self, found)
	}
}
