package storetest

//ci:builds packages/envoy/internal/cipher/store/storetest . TestStoretestLinksNoContainerRuntime go list -deps .

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/testgo"
)

// containerModule is the import prefix every testcontainers package shares.
const containerModule = "github.com/testcontainers/testcontainers-go"

// storetest links no testcontainers package. Every Postgres-backed broker test binary links this
// package, ciphertest's and cmd/cipher's among them, and testcontainers' internal/core init
// walks every process on the machine, before any test runs, to name its session: on a box running
// 26,582 processes that took 9.8 s of an agent-secrets test binary's start (GODEBUG=inittrace=1),
// past the 10 s a prompt test gives its helper process to start. storetest makes a database on a
// server it is given (testdb.DatabaseOn) and starts no container, so it needs none of it.
//
// Control: import internal/testpg here again (or in testdb) and this fails, naming the path.
func TestStoretestLinksNoContainerRuntime(t *testing.T) {
	out, err := testgo.Output(exec.Command("go", "list", "-deps", "-f", "{{.ImportPath}}", "."))
	if err != nil {
		t.Fatalf("go list -deps .: %v", err)
	}
	deps := strings.Fields(string(out))
	if len(deps) == 0 {
		t.Fatal("go list -deps . listed no package, so it read nothing this test can judge")
	}
	for _, dep := range deps {
		if dep == containerModule || strings.HasPrefix(dep, containerModule+"/") {
			t.Errorf("storetest links %s: every broker test binary would run testcontainers' process walk at start", dep)
		}
	}
}
