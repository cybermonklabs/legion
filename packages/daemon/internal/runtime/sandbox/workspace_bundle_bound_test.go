package sandbox

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// The daemon's wait for a workspace bundle and the shim's build of it are one budget, never two:
// BundleWorkspace hands the far side the shorter of runtime.WorkspaceBundleTimeout, the runtimes'
// own budget for the request, and what is left of the caller's deadline. A shim still building
// after the caller stopped waiting would hold the workspace into the next ask for one, and two
// runs of the bundler in one workspace share the ref prefix `refs/legion-bundle/`, so one run's
// delete lands inside the other's `git bundle create`.
//
// The caller with no deadline is the idle release's save, which gets the whole request budget as
// it always did; the caller with one is the cadence that copies a running pod's workspace, which
// abandons a copy at runningCopyBudget, four minutes (internal/daemon).
func TestTheWorkspaceBundleBoundIsTheShorterOfTheCallersDeadlineAndTheRequestsBudget(t *testing.T) {
	if bound := workspaceBundleBound(context.Background()); bound != runtime.WorkspaceBundleTimeout {
		t.Fatalf("a caller with no deadline gives the shim %s, want the request's whole budget %s", bound, runtime.WorkspaceBundleTimeout)
	}

	const copyBudget = 4 * time.Minute
	tight, cancel := context.WithTimeout(context.Background(), copyBudget)
	defer cancel()
	bound := workspaceBundleBound(tight)
	if bound > copyBudget || bound < copyBudget-time.Second {
		t.Fatalf("a caller with %s left gives the shim %s, want what is left of its own deadline", copyBudget, bound)
	}

	wide, cancelWide := context.WithTimeout(context.Background(), time.Hour)
	defer cancelWide()
	if bound := workspaceBundleBound(wide); bound != runtime.WorkspaceBundleTimeout {
		t.Fatalf("a caller with an hour left gives the shim %s, want the request's budget %s: the wider deadline is not the shim's",
			bound, runtime.WorkspaceBundleTimeout)
	}
}
