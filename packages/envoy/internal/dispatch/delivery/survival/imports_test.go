package survival_test

import (
	"os/exec"
	"strings"
	"testing"
)

// forbiddenImports is every package the computation must not reach, at any depth: the network,
// a process, a database, and Dispatch's GitHub client and delivery store.
var forbiddenImports = []string{
	"net",
	"net/http",
	"os/exec",
	"database/sql",
	"github.com/sjawhar/envoy/internal/dispatch/githubapp",
	"github.com/sjawhar/envoy/internal/dispatch/delivery",
}

func TestTheSurvivalPackageImportsNoNetworkProcessOrStorePackage(t *testing.T) {
	t.Parallel()
	const self = "github.com/sjawhar/envoy/internal/dispatch/delivery/survival"
	command := exec.Command("go", "list", "-deps", "-f", "{{.ImportPath}} {{.Standard}}", ".")
	out, err := command.Output()
	if err != nil {
		t.Fatalf("go list -deps: %v", err)
	}
	deps := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		path, standard, found := strings.Cut(line, " ")
		if !found {
			t.Fatalf("go list wrote %q", line)
		}
		deps[path] = standard == "true"
	}
	if _, listed := deps[self]; !listed || len(deps) < 10 || !deps["crypto/sha256"] {
		t.Fatalf("go list read %d packages, not the survival package's own: %v", len(deps), deps)
	}
	for path, standard := range deps {
		if path != self && !standard {
			t.Errorf("the survival package reaches %s, which is not of Go's standard library", path)
		}
		if strings.Contains(path, "pgx") {
			t.Errorf("the survival package reaches %s, a Postgres driver", path)
		}
	}
	for _, path := range forbiddenImports {
		if _, reached := deps[path]; reached {
			t.Errorf("the survival package reaches %s", path)
		}
	}
}
