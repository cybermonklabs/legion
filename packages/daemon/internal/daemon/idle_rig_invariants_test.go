package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// idleRigCheckWatch is the release's runtime with one thing added: it reads the state of the claim
// whose shim is being asked, at the moment it is asked. Only a shim can read the workspace — it
// runs the `jj` of the workspace it already works in — and a shim dies with its role's process,
// so the one thing the check's place in the release has to hold is that the process is still
// there when the check runs.
type idleRigCheckWatch struct {
	runtime.Runtime
	reader   runtime.WorkspaceWorkReader
	releaser runtime.IssueReleaser
	pods     issuePods
	observe  func()
}

func (w *idleRigCheckWatch) WorkspaceWork(ctx context.Context, issue string, role claim.Role) (runtime.WorkspaceWork, error) {
	w.observe()
	return w.reader.WorkspaceWork(ctx, issue, role)
}

func (w *idleRigCheckWatch) ReleaseIssue(ctx context.Context, issue, tree string, authorize func(context.Context) (bool, error),
	writing func(context.Context) error) (bool, error) {
	return w.releaser.ReleaseIssue(ctx, issue, tree, authorize, writing)
}

func (w *idleRigCheckWatch) IssuePodRunsConfiguredImage(issue string) bool {
	return w.pods.IssuePodRunsConfiguredImage(issue)
}

func (w *idleRigCheckWatch) IssueSandbox(issue string) (string, bool) {
	return w.pods.IssueSandbox(issue)
}

func (w *idleRigCheckWatch) IssuePod(issue string) (string, bool) {
	return w.pods.IssuePod(issue)
}

func (w *idleRigCheckWatch) IssueKeepsVolume(issue string, since time.Time) bool {
	return w.pods.IssueKeepsVolume(issue, since)
}

// A release reads the workspace for work origin does not hold while the role whose shim reads it
// is still running, and only then stops it. The check is a row of its own
// (record.IssueWorkspaceCheck) ahead of the release's `release` rows, because the runtime asks the
// role's shim and refuses a claim with no process — "holds no role process to read it"
// (sandbox.Runtime.WorkspaceWork). With the check behind those stops, every check would fail, every
// release would be declined workspace_unread, and no pod would ever be released.
func TestTheReleaseChecksTheWorkspaceWhileTheRolesProcessIsStillRunning(t *testing.T) {
	t.Parallel()
	rig := newIdleRig(t)
	ctx := context.Background()

	inner := rig.sup.deps.Runtime
	reader, ok := inner.(runtime.WorkspaceWorkReader)
	if !ok {
		t.Fatal("the rig's runtime reads no workspace")
	}
	releaser, ok := inner.(runtime.IssueReleaser)
	if !ok {
		t.Fatal("the rig's runtime releases no issue")
	}
	podRuntime, ok := inner.(issuePods)
	if !ok {
		t.Fatal("the rig's runtime reads no issue pod")
	}
	var checks int
	var atCheck supervise.Claim
	rig.sup.deps.Runtime = &idleRigCheckWatch{Runtime: inner, reader: reader, releaser: releaser, pods: podRuntime,
		observe: func() {
			checks++
			atCheck = rig.machine.Claim()
		}}

	if got := rig.queuedBy(t, func() { rig.w.releaseIdlePods(ctx) }); len(got) != 3 || got[0] != "issue_bundle" {
		t.Fatalf("queued rows = %v, want the workspace check first", got)
	}
	rig.drain(t)

	if checks != 1 {
		t.Fatalf("the release read the workspace %d times, want exactly once", checks)
	}
	if atCheck.State == supervise.StateSuspended || atCheck.Locator == nil {
		t.Fatalf("at the check the claim was %s by %q with locator %v; want its role process still running",
			atCheck.State, atCheck.SuspendedBy, atCheck.Locator)
	}
	if got := rig.machine.Claim(); got.State != supervise.StateSuspended || got.SuspendedBy != supervise.SuspendedByReleased {
		t.Fatalf("after the release the claim = %s by %s, want suspended by released", got.State, got.SuspendedBy)
	}
	if rig.declined(t) {
		t.Fatal("the release of a clean workspace recorded a decline")
	}
}

// The workflow engine and the daemon address the pod tables of the idle release under one
// project. The engine writes `listener_reports` (workflow's listenerWake) and the daemon reads it
// (workflowRuntime.listenerLive), and the two spellings of a project are not the same string: the
// key is what an issue's row holds ("LEGION"), the claim token's is what `claims.project` and
// every table keyed on a claim hold ("legion"). With the engine on the key and the daemon on the
// token, as it was, the gate R7 reads never opened and no pod was ever released.
//
// This writes through the engine and reads through the daemon's own store, which is the only
// shape that catches a divergence: every test that picks one spelling and uses it on both sides
// passes either way, which is how it was missed.
func TestTheEngineAndTheDaemonReadThePodTablesUnderOneProject(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	st := outboxTreeStore(t, pool, "LEGION-208")
	token, err := claim.ProjectToken("LEGION")
	if err != nil {
		t.Fatal(err)
	}
	if token == "LEGION" {
		t.Fatal("the project token and the project key are the same string, so this test proves nothing")
	}
	cfg := engineConfig(idleRigProjectConfig(), "legion-reviewer[bot]", nil)
	// The engine is handed the store's own view of the project, which derives that one spelling
	// (store.Store.PodRecordsOf): the engine carries no second project of its own.
	if cfg.Pods, err = st.PodRecordsOf(cfg.Project); err != nil {
		t.Fatal(err)
	}
	engine := workflow.New(record.NewStore(), cfg, quietLogger())

	at := time.Now().UTC().Truncate(time.Millisecond)
	if _, err := intake.ApplyFact(ctx, pool, "wake", "listener-ready-1",
		intake.ListenerWake{Kind: intake.ListenerWakeReady, At: at, Report: "listener-ready-1"}, engine); err != nil {
		t.Fatalf("apply the listener's ready report: %v", err)
	}

	live, read, found, err := st.ListenerLive(ctx, token, at.Add(time.Minute), listenerReadyWindow)
	if err != nil {
		t.Fatalf("read the listener's report: %v", err)
	}
	if !found || !live || !read.Equal(at) {
		t.Fatalf("the daemon read the listener's report as live=%t found=%t at=%s, want the report the engine wrote at %s",
			live, found, read, at)
	}
	if _, _, underTheKey, err := st.ListenerLive(ctx, cfg.Project, at.Add(time.Minute), listenerReadyWindow); err != nil || underTheKey {
		t.Fatalf("the report is filed under the project key %q too (%v); the pod tables are keyed on the claim token alone", cfg.Project, err)
	}
}

// idleRigProjectConfig is the smallest configuration engineConfig reads: the project whose two
// spellings this test is about.
func idleRigProjectConfig() config.Config {
	return config.Config{Project: "LEGION"}
}
