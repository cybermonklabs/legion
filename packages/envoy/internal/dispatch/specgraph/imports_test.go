package specgraph

//ci:builds packages/envoy/internal/dispatch/specgraph . TestSpecgraphReachesNoStore go list -deps .

import (
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/testgo"
)

// specgraph is logic over bytes: it reaches no store, no route and no database, so Dispatch's
// importer, the pilot and the spec-check command can all call it. go list lists the package's
// whole import graph, its tests' imports aside.
func TestSpecgraphReachesNoStore(t *testing.T) {
	t.Parallel()
	// Standard output alone is the list: what go writes on standard error (a module it downloads)
	// is no package.
	out, err := testgo.Output(exec.Command("go", "list", "-deps", "-f", "{{.ImportPath}}", "."))
	if err != nil {
		t.Fatalf("go list -deps: %v", err)
	}
	deps := strings.Fields(string(out))
	if !slices.Contains(deps, "github.com/sjawhar/envoy/internal/dispatch/specgraph") {
		t.Fatalf("go list -deps listed %d packages and not specgraph itself: %q", len(deps), deps)
	}
	for _, dep := range deps {
		for _, banned := range []string{
			"github.com/sjawhar/envoy/internal/dispatch/store",
			"github.com/sjawhar/envoy/internal/dispatch/graphstore",
			"github.com/sjawhar/envoy/internal/dispatch/api",
		} {
			if dep == banned || strings.HasPrefix(dep, banned+"/") {
				t.Errorf("specgraph imports %s", dep)
			}
		}
		if strings.HasPrefix(dep, "github.com/jackc/pgx") {
			t.Errorf("specgraph imports %s", dep)
		}
	}
}
