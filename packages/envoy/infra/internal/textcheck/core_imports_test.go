package textcheck

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestCoreAndTheListenerAgreeOnAnAllowedHost (infra/core/allowed_hosts_agreement_test.go) imports
// the listener's config package, which go list does not count: a non-test import would put it in
// `go list -deps ./cmd/...`, the path list a plugin release is cut from (AGENTS.md, "Release
// contract"), so an edit to Envoy's config would cut a plugin release.
func TestCoreCompilesNoListenerConfigPackage(t *testing.T) {
	// go list resolves a relative pattern against the directory it runs in, which is this
	// package's, so the command runs at the module root and names the package from there.
	list := exec.Command("go", "list", "-deps", "-f", "{{.ImportPath}}", "./core")
	list.Dir = filepath.Join("..", "..")
	out, err := list.CombinedOutput()
	require.NoError(t, err, string(out))
	for _, dep := range strings.Fields(string(out)) {
		require.NotEqual(t, "github.com/sjawhar/envoy/internal/config", dep,
			"the core package imports the listener's config package")
	}
}
