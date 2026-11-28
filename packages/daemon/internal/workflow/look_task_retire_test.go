package workflow

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// TestEveryEndOfALooksTaskQueuesItsRetirement holds the R4/R5 retirement inventory: every workflow
// cause that ends a production check's look task queues its supervise `retire_look` row in that
// cause's transaction, naming the check's role, look number and check opening time. A report queues
// a row with Reported true, and each close of a check queues one without it, so a report that closes
// the check (pass, fail) queues both; every other cause queues one without it.
//
// A root's re-admission lives in internal/admit (admit/admit.go's readmit), which imports this
// package and so cannot be imported here; its row is TestAReadmissionQueuesTheRetirementOfItsLooksTask
// in internal/admit/look_task_retire_test.go.
func TestEveryEndOfALooksTaskQueuesItsRetirement(t *testing.T) {
	t.Parallel()

	type retireCase struct {
		name         string
		issue        string
		wantRole     claim.Role
		wantLook     int
		wantOpenedAt time.Time
		// wantReported lists the Reported flag of every retire_look row the cause's transaction must
		// queue, regardless of order.
		wantReported []bool
		// run prepares the fixture, records how many retire_look rows stood before the cause, drives
		// the cause through the engine, and returns the rig so the test can read the rows added by
		// that cause alone.
		run func(t *testing.T) (rig checkRig, before int)
	}

	firstOpened := checkOpened.Add(-24 * time.Hour)

	cases := []retireCase{
		{
			name:         "report of pass",
			issue:        "LEGION-209",
			wantRole:     claim.RoleTester,
			wantLook:     1,
			wantOpenedAt: checkOpened,
			wantReported: []bool{true, false},
			run: func(t *testing.T) (checkRig, int) {
				rig := childCheckRig(t)
				before := len(superviseRowsOf(t, rig.pool, "LEGION-209", "retire_look"))
				if res := rig.apply("pass", intake.CheckReport{
					Issue: "LEGION-209", Claim: "tester", Look: 1, OpenedAt: checkOpened,
					Verdict: "pass", Summary: "observed in production",
				}); res.Refusal != nil {
					t.Fatalf("report of pass refused: %+v", res.Refusal)
				}
				return rig, before
			},
		},
		{
			name:         "report of pending",
			issue:        "LEGION-209",
			wantRole:     claim.RoleTester,
			wantLook:     1,
			wantOpenedAt: checkOpened,
			wantReported: []bool{true},
			run: func(t *testing.T) (checkRig, int) {
				rig := childCheckRig(t)
				before := len(superviseRowsOf(t, rig.pool, "LEGION-209", "retire_look"))
				wake := checkOpened.Add(2 * time.Hour)
				if res := rig.apply("pending", intake.CheckReport{
					Issue: "LEGION-209", Claim: "tester", Look: 1, OpenedAt: checkOpened,
					Verdict: "pending", Summary: "not deployed yet", WakeAt: &wake,
				}); res.Refusal != nil {
					t.Fatalf("report of pending refused: %+v", res.Refusal)
				}
				return rig, before
			},
		},
		{
			name:         "report of fail",
			issue:        "LEGION-209",
			wantRole:     claim.RoleTester,
			wantLook:     1,
			wantOpenedAt: checkOpened,
			wantReported: []bool{true, false},
			run: func(t *testing.T) (checkRig, int) {
				rig := childCheckRig(t)
				before := len(superviseRowsOf(t, rig.pool, "LEGION-209", "retire_look"))
				if res := rig.apply("fail", intake.CheckReport{
					Issue: "LEGION-209", Claim: "tester", Look: 1, OpenedAt: checkOpened,
					Verdict: "fail", Summary: "defect observed in production",
				}); res.Refusal != nil {
					t.Fatalf("report of fail refused: %+v", res.Refusal)
				}
				return rig, before
			},
		},
		{
			name:         "look bound",
			issue:        "LEGION-209",
			wantRole:     claim.RoleTester,
			wantLook:     1,
			wantOpenedAt: checkOpened,
			wantReported: []bool{false},
			run: func(t *testing.T) (checkRig, int) {
				rig := childCheckRig(t)
				rig.clock.now = checkOpened.Add(CheckLookBound)
				before := len(superviseRowsOf(t, rig.pool, "LEGION-209", "retire_look"))
				if res := rig.apply("look-bound", intake.CheckLookEnded{
					Issue: "LEGION-209", Look: 1, OpenedAt: checkOpened, Cause: "look bound",
				}); res.Refusal != nil {
					t.Fatalf("look bound refused: %+v", res.Refusal)
				}
				return rig, before
			},
		},
		{
			name:         "hold met by start",
			issue:        "LEGION-209",
			wantRole:     claim.RoleTester,
			wantLook:     1,
			wantOpenedAt: checkOpened,
			wantReported: []bool{false},
			run: func(t *testing.T) (checkRig, int) {
				rig := childCheckRig(t)
				before := len(superviseRowsOf(t, rig.pool, "LEGION-209", "retire_look"))
				if res := rig.apply("operator-hold", intake.CheckLookEnded{
					Issue: "LEGION-209", Look: 1, OpenedAt: checkOpened, Cause: "operator hold",
				}); res.Refusal != nil {
					t.Fatalf("hold met by start refused: %+v", res.Refusal)
				}
				return rig, before
			},
		},
		{
			name:         "failed claim",
			issue:        "LEGION-209",
			wantRole:     claim.RoleTester,
			wantLook:     1,
			wantOpenedAt: checkOpened,
			wantReported: []bool{false},
			run: func(t *testing.T) (checkRig, int) {
				rig := childCheckRig(t)
				before := len(superviseRowsOf(t, rig.pool, "LEGION-209", "retire_look"))
				if res := rig.apply("tester-failed", failedClaim("LEGION-209", claim.RoleTester)); res.Refusal != nil {
					t.Fatalf("failed claim refused: %+v", res.Refusal)
				}
				return rig, before
			},
		},
		{
			name:         "close by check bound",
			issue:        "LEGION-208",
			wantRole:     claim.RoleTester,
			wantLook:     1,
			wantOpenedAt: checkOpened,
			wantReported: []bool{false},
			run: func(t *testing.T) (checkRig, int) {
				rig := rootCheckRig(t)
				// Root runs again while its check stays open with look 1 already ended, so the
				// check's bound closes it as not_run via closeCheck -> markCheckClosed.
				if _, err := rig.pool.Exec(context.Background(),
					"update issues set phase = 'admitted', status = 'todo', linger_until = null, generation = 8 where key = 'LEGION-208'"); err != nil {
					t.Fatalf("move root back into workflow: %v", err)
				}
				if _, err := rig.pool.Exec(context.Background(),
					"update production_checks set look_open = false where issue = 'LEGION-208'"); err != nil {
					t.Fatalf("mark check look closed for bound test: %v", err)
				}
				rig.clock.now = checkOpened.Add(checkBound)
				before := len(superviseRowsOf(t, rig.pool, "LEGION-208", "retire_look"))
				if res := rig.apply("check-bound", intake.CheckBound{Issue: "LEGION-208"}); res.Refusal != nil {
					t.Fatalf("check bound refused: %+v", res.Refusal)
				}
				if check := productionCheck(t, rig.pool, "LEGION-208"); check == nil || check.Open() || check.Outcome != record.CheckNotRun {
					t.Fatalf("check after bound while running again = %+v, want closed as not_run", check)
				}
				return rig, before
			},
		},
		{
			name:         "replacement by a later merge",
			issue:        "LEGION-209",
			wantRole:     claim.RoleTester,
			wantLook:     1,
			wantOpenedAt: firstOpened,
			wantReported: []bool{false},
			run: func(t *testing.T) (checkRig, int) {
				rig := newCheckRig(t)
				seedMergingChild(t, rig.pool, record.Issue{
					Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
					Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Implementing,
					Generation: 7, Status: "in_progress", Rank: "U", LastDispatchSeq: 1,
				})
				// Seed an open production check from an earlier merge of LEGION-209 (generation 6),
				// so merging PR #43 at generation 7 replaces it.
				putProductionCheck(t, rig.pool, record.ProductionCheck{
					Issue: "LEGION-209", Project: "LEGION", Tree: "LEGION-208", Repo: "sjawhar/legion",
					Number: 41, MergeSHA: "merge-41", Role: claim.RoleTester,
					OpenedAt: firstOpened, BoundAt: firstOpened.Add(checkBound),
					Looks: 1, LookOpen: true, LookWakeAt: &firstOpened,
					LastWakeKind: wakeMerge, LastWakeDetail: "merge-41", LastWakeAt: &firstOpened,
					LookID: "LOOKIDREPLACEDCHECK0000001", LookIDLook: 1, LookIDOpenedAt: firstOpened,
					OpenedGeneration: 6,
				})
				before := len(superviseRowsOf(t, rig.pool, "LEGION-209", "retire_look"))
				if res := rig.apply("later-merge", intake.PullRequestMerged{
					Repo: "sjawhar/legion", Number: 43, MergeSHA: "merge-43",
				}); res.Refusal != nil {
					t.Fatalf("later merge refused: %+v", res.Refusal)
				}
				if check := productionCheck(t, rig.pool, "LEGION-209"); check == nil || !check.Open() || check.MergeSHA != "merge-43" {
					t.Fatalf("check after later merge = %+v, want open replacement check on merge-43", check)
				}
				return rig, before
			},
		},
		{
			name:         "leave with no merge",
			issue:        "LEGION-209",
			wantRole:     claim.RoleTester,
			wantLook:     1,
			wantOpenedAt: checkOpened,
			wantReported: []bool{false},
			run: func(t *testing.T) (checkRig, int) {
				rig := newCheckRig(t)
				seedIssue(t, rig.pool, record.Issue{
					Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
					Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Implementing,
					Generation: 7, Status: "in_progress", Rank: "U", LastDispatchSeq: 1,
				})
				parent := "LEGION-208"
				seedIssue(t, rig.pool, record.Issue{
					Key: "LEGION-209", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
					Tree: "LEGION-208", Project: "LEGION", Title: "child", Parent: &parent,
					Phase: phase.Implementing, Generation: 8, Status: "in_progress", Rank: "V", LastDispatchSeq: 1,
				})
				// Seed an open production check opened at generation 7; LEGION-209 is now running
				// again at generation 8 and leaves with no merge (parked in backlog).
				putProductionCheck(t, rig.pool, record.ProductionCheck{
					Issue: "LEGION-209", Project: "LEGION", Tree: "LEGION-208", Repo: "sjawhar/legion",
					Number: 43, MergeSHA: "merge-43", Role: claim.RoleTester,
					OpenedAt: checkOpened, BoundAt: checkOpened.Add(checkBound),
					Looks: 1, LookOpen: true, LookWakeAt: &checkOpened,
					LastWakeKind: wakeMerge, LastWakeDetail: "merge-43", LastWakeAt: &checkOpened,
					LookID: "LOOKIDLEAVENOMERGE00000001", LookIDLook: 1, LookIDOpenedAt: checkOpened,
					OpenedGeneration: 7,
				})
				before := len(superviseRowsOf(t, rig.pool, "LEGION-209", "retire_look"))
				if res := rig.apply("parked", intake.DispatchIssue{
					Key: "LEGION-209", Seq: 2, Type: "issue.updated", Status: "backlog",
					Title: "child", Parent: "LEGION-208", Rank: "V",
				}); res.Refusal != nil {
					t.Fatalf("leave with no merge refused: %+v", res.Refusal)
				}
				return rig, before
			},
		},
		{
			name:         "child re-entry",
			issue:        "LEGION-209",
			wantRole:     claim.RoleTester,
			wantLook:     1,
			wantOpenedAt: checkOpened,
			wantReported: []bool{false},
			run: func(t *testing.T) (checkRig, int) {
				rig := childCheckRig(t)
				before := len(superviseRowsOf(t, rig.pool, "LEGION-209", "retire_look"))
				reenterChild(t, rig)
				return rig, before
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rig, before := tc.run(t)
			all := superviseRowsOf(t, rig.pool, tc.issue, "retire_look")
			got := all[before:]
			if len(got) != len(tc.wantReported) {
				t.Fatalf("%s queued %d retire_look row(s) (%+v), want %d (reported flags %v)",
					tc.name, len(got), got, len(tc.wantReported), tc.wantReported)
			}
			var gotReported []bool
			for i, row := range got {
				if row.Role != tc.wantRole {
					t.Errorf("%s: retire_look[%d].Role = %q, want %q", tc.name, i, row.Role, tc.wantRole)
				}
				if row.CheckLook != tc.wantLook {
					t.Errorf("%s: retire_look[%d].CheckLook = %d, want %d", tc.name, i, row.CheckLook, tc.wantLook)
				}
				if !row.CheckOpenedAt.Equal(tc.wantOpenedAt) {
					t.Errorf("%s: retire_look[%d].CheckOpenedAt = %s, want %s",
						tc.name, i, row.CheckOpenedAt.UTC().Format(time.RFC3339Nano), tc.wantOpenedAt.UTC().Format(time.RFC3339Nano))
				}
				gotReported = append(gotReported, row.Reported)
			}
			wantSorted := slices.Clone(tc.wantReported)
			slices.SortFunc(gotReported, compareBool)
			slices.SortFunc(wantSorted, compareBool)
			if !slices.Equal(gotReported, wantSorted) {
				t.Errorf("%s: retire_look Reported flags = %v, want %v (rows: %+v)",
					tc.name, gotReported, wantSorted, got)
			}
		})
	}
}

func compareBool(a, b bool) int {
	switch {
	case a == b:
		return 0
	case !a && b:
		return -1
	default:
		return 1
	}
}
