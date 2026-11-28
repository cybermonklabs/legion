package daemon

// Review round 13's second finding on cybermonklabs/legion#4049, which the architect decided
// (dispatch://INFRA-3583/comment/b93427cc-8c08-4bb9-a19d-d1b74142be69, option a): a pod in which no
// role process runs has no shim to read through, so the release reads it through the pod's
// launcher, by the `workspace-work` read the idle release makes (runtime.PodWorkspaceReader), and
// records what that read found, as for a pod whose role answers: deleted_lost where it found
// work, deleted_clean where it found none. The pod's line names that read as its reason, since it
// counts the commits under a working copy or a local bookmark and not the release's own unpushed
// work. Only a launcher that does not answer or refuses leaves the pod deleted_unread, with the
// error. The dry run reads such a pod the same way.

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// launcherReadPod is the Sandbox of LEGION-208, the one pod of the release tests below.
const launcherReadPod = "legion-legion-legion-208"

// launcherReadRelease is a release, or its dry run, over LEGION-208's pod, whose one claim retired
// so that no role process runs there, with the pod's launcher answering its read as pods says.
// It answers the pod's line.
func launcherReadRelease(t *testing.T, pods *releasePods, dryRun bool) api.ReleasedSandbox {
	t.Helper()
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	token := mustClaimToken(t, "LEGION-208", claim.RoleImplementer)
	if _, _, err := sup.Create(context.Background(), supervise.Claim{Token: token, Project: "legion", Tree: "LEGION-208",
		Issue: "LEGION-208", Role: claim.RoleImplementer, Session: "ses_implementer", State: supervise.StateRetired}, ""); err != nil {
		t.Fatal(err)
	}
	pods.sandboxes = []runtime.ProjectSandbox{{Name: launcherReadPod, Kind: runtime.ProjectSandboxIssue, Issue: "LEGION-208",
		Tree: "LEGION-208", PodUID: "uid-208", ConfiguredImage: true}}
	records := &releaseRecord{claims: sup.deps.Store.(*outboxClaimStore), finished: make(chan json.RawMessage, 1)}
	if !dryRun {
		return releaseLine(t, api.ProjectReleaseResponse{Result: runRelease(t, records, sup, pods)}, launcherReadPod)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	view, started, err := testReleaser(ctx, &projectRelease{}, records, sup, pods).Start(ctx, time.Second, true)
	if err != nil || started || !view.DryRun {
		t.Fatalf("the dry run answered %+v, started %v, %v; want a dry run", view, started, err)
	}
	return releaseLine(t, view, launcherReadPod)
}

func TestProjectReleaseReadsAPodWithNoRunningRoleThroughItsLauncher(t *testing.T) {
	t.Parallel()
	refused := errors.New("read LEGION-208's workspace: no launcher of pod uid-208 that reads a workspace is connected")
	for _, tc := range []struct {
		name    string
		work    *runtime.WorkspaceWork
		err     error
		outcome api.ReleaseOutcome
		// commits is the count the line carries, -1 for none, and changed its working-copy flag.
		commits int
		changed bool
		reason  string
	}{
		{name: "the launcher's read finds work", outcome: api.ReleaseDeletedLost, commits: 2, changed: true, reason: releaseLauncherRead,
			work: &runtime.WorkspaceWork{Head: passedOverHead, Changed: true, Commits: []string{passedOverCommit, launchReadCommit}, Count: 2}},
		{name: "the launcher's read finds none", outcome: api.ReleaseDeletedClean, commits: 0, reason: releaseLauncherRead,
			work: &runtime.WorkspaceWork{Head: passedOverHead}},
		{name: "control: the launcher refuses the read", outcome: api.ReleaseDeletedUnread, commits: -1, err: refused,
			reason: releaseLauncherUnread + refused.Error()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			for _, dryRun := range []bool{false, true} {
				pods := &releasePods{launcherWork: tc.work, launcherErr: tc.err}
				line := launcherReadRelease(t, pods, dryRun)
				counted := line.Commits != nil && line.WorkingCopyChanged != nil
				switch {
				case line.Outcome != tc.outcome || line.Reason != tc.reason || line.Refusal != "" || len(line.Asked) != 0:
					t.Errorf("dry run %v: the pod's line is %s, want %s with the reason %q, no refusal and nobody asked", dryRun, lineJSON(line), tc.outcome, tc.reason)
				case tc.commits < 0 && (line.Commits != nil || line.WorkingCopyChanged != nil):
					t.Errorf("dry run %v: the pod's line is %s, want no count: its launcher read nothing", dryRun, lineJSON(line))
				case tc.commits >= 0 && (!counted || *line.Commits != tc.commits || *line.WorkingCopyChanged != tc.changed):
					t.Errorf("dry run %v: the pod's line is %s, want the launcher's count %d and changed=%v", dryRun, lineJSON(line), tc.commits, tc.changed)
				}
				pods.mu.Lock()
				launcherReads, reads, deleted := slices.Clone(pods.launcherReads), len(pods.reads), slices.Clone(pods.deleted)
				pods.mu.Unlock()
				if !slices.Equal(launcherReads, []string{"LEGION-208"}) || reads != 0 {
					t.Errorf("dry run %v: the pod's launcher read %v and a role's shim %d times, want the launcher once and no shim: no role process runs there",
						dryRun, launcherReads, reads)
				}
				if want := []string{"LEGION-208"}; dryRun {
					if len(deleted) != 0 {
						t.Errorf("the dry run deleted the pods of %v, want none", deleted)
					}
				} else if !slices.Equal(deleted, want) {
					t.Errorf("the release deleted the pods of %v, want %v", deleted, want)
				}
			}
		})
	}
}
