package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// The shim's configuration as `legion worker-shim` builds it from its flags carries the two things
// internal/shim's call-graph walk takes as given (TestEveryChildTheShimWaitsForStartsThroughItsGuard):
// --warm-codegraph hands the shim workspace.WarmCodegraphIndex itself, the function that walk
// starts from as the shim's warm-up, and --reap-orphans sets Config.ReapOrphans, which the
// controller pod's single-role launcher passes (internal/launcher's reapingShim). Without either
// flag neither is set.
func TestTheWorkerShimWarmsWithTheWorkspacesWarmUp(t *testing.T) {
	token := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(token, []byte("boot-token-for-the-pane"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LEGION_WORKSPACE", t.TempDir())
	given := workerShimFlags{connect: "unix:///run/legion/stream", bootTokenFile: token}
	set := map[string]bool{"connect": true, "boot-token-file": true}

	cfg, err := workerShimConfig(set, given, []string{"omp"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.WarmCodegraph != nil || cfg.ReapOrphans {
		t.Errorf("with neither flag the shim warms %v and reaps orphans %t, want neither", cfg.WarmCodegraph != nil, cfg.ReapOrphans)
	}

	given.warmCodegraph, given.reapOrphans = true, true
	set["warm-codegraph"], set[shimwire.ReapOrphansFlag] = true, true
	cfg, err = workerShimConfig(set, given, []string{"omp"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.WarmCodegraph == nil || reflect.ValueOf(cfg.WarmCodegraph).Pointer() != reflect.ValueOf(workspace.WarmCodegraphIndex).Pointer() {
		t.Error("--warm-codegraph does not hand the shim workspace.WarmCodegraphIndex, the warm-up the shim's call-graph walk reads")
	}
	if !cfg.ReapOrphans {
		t.Error("--reap-orphans does not set Config.ReapOrphans")
	}
}
