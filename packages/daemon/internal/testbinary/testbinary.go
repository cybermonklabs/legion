// Package testbinary holds what a test asserts about the test binary it runs in.
package testbinary

import (
	"runtime/debug"
	"strings"
	"testing"
)

// forbidden is the module path prefix of testcontainers-go and its modules.
const forbidden = "github.com/testcontainers/"

// LinksNoTestcontainers fails t when the test binary it runs in links a module of
// testcontainers-go, naming each, or carries no module list to read. That library's package init
// reads every process on the machine: 7.6 to 11.5 s and 600 to 910 MB of allocation per start of
// internal/daemon's test binary on a machine that ran 13,000 to 20,000 processes
// (dispatch://INFRA-3904). A test binary pays it at every start, so one that starts itself again
// pays it at each child; the test that calls this says which starts its package makes. It reads
// the modules the binary was built from, as `go version -m` prints them. CI's
// .github/scripts/testcontainers_links.py holds every such binary to the same rule from the
// outside and names the import chain.
func LinksNoTestcontainers(t testing.TB) {
	t.Helper()
	info, ok := debug.ReadBuildInfo()
	if !ok {
		t.Fatal("this test binary carries no build information to read its modules from")
	}
	if len(info.Deps) == 0 {
		t.Fatal("this test binary's build information lists no module, so it cannot say what is linked")
	}
	pkg := strings.TrimSuffix(info.Path, ".test")
	for _, dep := range info.Deps {
		if strings.HasPrefix(dep.Path, forbidden) {
			t.Errorf("the test binary of %s links %s %s, and every start of it pays that module's package init; `go list -deps -test -f '{{.ImportPath}} {{.Imports}}' %s` names the package that imports it, and .github/scripts/testcontainers_links.py the whole chain", pkg, dep.Path, dep.Version, pkg)
		}
	}
}
