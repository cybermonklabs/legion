package projection

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// fixHead is the head the fixes of these tests started at, where their check failed.
const fixHead = "c0de0000000000000000000000000000000000ff"

// An issue whose task runs a fix beside its step carries the fix: its task, the step it runs
// again, the check and the head it started for, the directory of its working copy beside the
// issue's workspace, the entry the fix raised on the implementer's phases row and the entry the
// implementer's claim was last given. The plugin completes the fix by the directory, and a reader
// tells a fix whose task its claim holds from one the daemon has still to send by the two
// entries, as it reads the issue's own pair for the step the issue stands at, which stays the
// reviewer's. A fix that still reads its failed job has raised no entry, so the issue record names
// no running fix and the issue carries none; nor does an issue whose task runs no fix.
func TestTheStateServesTheRunningFixWithItsEntries(t *testing.T) {
	const project = "LEGION"
	issue := func(key string, runningFix int) record.Issue {
		return record.Issue{Key: key, Project: project, Tree: key, Phase: phase.Reviewing, Status: "needs_review", Generation: 1,
			Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Rank: key, RunningFix: runningFix}
	}
	rows := func(key string, implementer int64) []record.PhaseRow {
		return []record.PhaseRow{
			{Issue: key, Role: claim.RoleImplementer, Claim: claim.Token("legion-" + key + "-implementer"), Entry: implementer},
			{Issue: key, Role: claim.RoleReviewer, Claim: claim.Token("legion-" + key + "-reviewer"), Entry: 1},
		}
	}
	claims := func(key string, given int64) []supervise.Claim {
		return []supervise.Claim{
			{Token: claim.Token("legion-" + key + "-implementer"), Issue: key, Role: claim.RoleImplementer, State: supervise.StateWorking,
				Session: "ses_implementer_" + key, GivenEntry: given},
			{Token: claim.Token("legion-" + key + "-reviewer"), Issue: key, Role: claim.RoleReviewer, State: supervise.StateWorking,
				Session: "ses_reviewer_" + key, GivenEntry: 1},
		}
	}
	started := projectedAt.Add(-10 * time.Minute)
	fix := func(key string, state record.FixState, entry int64) record.TaskFix {
		return record.TaskFix{Issue: key, Generation: 1, Task: "a", Fix: 1, State: state, Check: "unit", Head: fixHead, Place: 1,
			Entry: entry, StartedAt: &started}
	}
	store := projectionStore{
		issues: []record.Issue{issue("LEGION-40", 1), issue("LEGION-50", 1), issue("LEGION-60", 0), issue("LEGION-70", 0)},
		phases: map[string][]record.PhaseRow{
			"LEGION-40": rows("LEGION-40", 2), "LEGION-50": rows("LEGION-50", 2),
			"LEGION-60": rows("LEGION-60", 1), "LEGION-70": rows("LEGION-70", 1),
		},
		planTasks: []record.PlanTask{
			{Issue: "LEGION-40", Generation: 1, Task: "a"}, {Issue: "LEGION-50", Generation: 1, Task: "a"},
			{Issue: "LEGION-60", Generation: 1, Task: "a"}, {Issue: "LEGION-70", Generation: 1, Task: "a"},
		},
		fixes: map[string]record.TaskFix{
			// Its claim holds the fix's task.
			"LEGION-40": fix("LEGION-40", record.FixRunning, 2),
			// The fix's task is still owed to its claim.
			"LEGION-50": fix("LEGION-50", record.FixRunning, 2),
			// The fix still reads its failed job: no entry raised, and the record names none.
			"LEGION-60": fix("LEGION-60", record.FixReading, 0),
		},
	}
	var live []supervise.Claim
	for key, given := range map[string]int64{"LEGION-40": 2, "LEGION-50": 1, "LEGION-60": 1, "LEGION-70": 1} {
		live = append(live, claims(key, given)...)
	}
	workspaces := func(issue record.Issue) string { return "/var/lib/legion/workspaces/acme/widgets/" + issue.Key }

	got, err := Project(context.Background(), Inputs{Store: store, Project: project, Claims: live, Now: projectedAt, WorkspaceDir: workspaces})
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	for key, want := range map[string]*api.FixView{
		"LEGION-40": {Task: "a", Step: 1, Check: "unit", Head: fixHead, Dir: "/var/lib/legion/workspaces/acme/widgets/LEGION-40-fix", Entry: 2, GivenEntry: 2},
		"LEGION-50": {Task: "a", Step: 1, Check: "unit", Head: fixHead, Dir: "/var/lib/legion/workspaces/acme/widgets/LEGION-50-fix", Entry: 2, GivenEntry: 1},
		"LEGION-60": nil,
		"LEGION-70": nil,
	} {
		view, known := got.Issues[key]
		if !known {
			t.Fatalf("%s is in no issue of the projected state; this test proves nothing", key)
		}
		if !reflect.DeepEqual(view.Fix, want) {
			t.Errorf("%s is projected with the fix %+v, want %+v", key, view.Fix, want)
		}
		if view.PhaseEntry != 1 || view.GivenEntry != 1 {
			t.Errorf("%s is projected with entry %d given %d, want the reviewer's 1 and 1: the fix's entries are the fix's own",
				key, view.PhaseEntry, view.GivenEntry)
		}
	}
}
