package workflow

import (
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

func TestOnlyAStartedStepOfThisRunCanReportALeftPhase(t *testing.T) {
	for _, which := range []string{"no entry", "entry without task", "started", "empty token", "missing record", "cleared issue", "cleared tree"} {
		t.Run(which, func(t *testing.T) {
			pool := migratedPool(t)
			engine := testEngine(config.DesignGateRootIssues, nil)
			issue := record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Reviewing, Generation: 1, Status: "needs_review", Rank: "U"}
			seedIssue(t, pool, issue)
			token := seedPhaseWorker(t, pool, issue.Key, claim.RoleTester, "")
			row := record.PhaseRow{Issue: issue.Key, Role: claim.RoleTester, Claim: token, Entry: 1, EntryTask: "test this change"}
			switch which {
			case "no entry":
				row.Entry, row.EntryTask = 0, ""
			case "entry without task":
				row.EntryTask = ""
			}
			seedPhase(t, pool, row)
			if which == "cleared issue" || which == "cleared tree" {
				seedRecord(t, pool, func(tx pgx.Tx) error {
					if which == "cleared tree" {
						return record.NewStore().ClearTreeGeneration(t.Context(), tx, issue.Tree, time.Now())
					}
					return record.NewStore().ClearGeneration(t.Context(), tx, issue.Key, time.Now())
				})
				issue.Generation++
				seedIssue(t, pool, issue)
				if _, err := pool.Exec(t.Context(), "update claims set serving_generation = $2 where token = $1", string(token), issue.Generation); err != nil {
					t.Fatal(err)
				}
			}
			fact := intake.HandoffComplete{Generation: issue.Generation, Issue: issue.Key, Role: claim.RoleTester, Claim: token, Summary: "tests pass", Verdict: "pass", Commit: "c0ffee", HandoffRecord: "test-late-1", HandoffVersion: 1}
			if which == "empty token" {
				fact.Claim = ""
			}
			if which == "missing record" {
				fact.HandoffRecord, fact.HandoffVersion = "", 0
			}
			before := reportedOf(t, pool, engine, issue.Key, claim.RoleTester, false)
			apply := applyFacts(t, pool, engine)
			result := apply("first", fact)
			want := "HANDOFF_NOT_CURRENT_PHASE"
			if which == "started" {
				want = ""
			}
			if which == "missing record" {
				want = "HANDOFF_RECORD_REQUIRED"
			}
			if want != "" {
				if result.Refusal == nil || result.Refusal.Status != 409 || result.Refusal.Code != want {
					t.Fatalf("result = %+v, want 409 %s", result.Refusal, want)
				}
				if after := reportedOf(t, pool, engine, issue.Key, claim.RoleTester, false); !reflect.DeepEqual(after, before) {
					t.Fatalf("refused report changed records:\nbefore %+v\nafter %+v", before, after)
				}
			} else {
				if result.Refusal != nil {
					t.Fatalf("started step refused: %+v", result.Refusal)
				}
				after := reportedOf(t, pool, engine, issue.Key, claim.RoleTester, false)
				if after.issue.Phase != phase.Reviewing || !after.row.Completed() || after.row.HandoffRecord != "test-late-1" || after.row.HandoffVersion != 1 {
					t.Fatalf("late report = %+v", after)
				}
				fact.HandoffRecord = "test-late-2"
				if again := apply("second", fact); again.Refusal == nil || again.Refusal.Code != "HANDOFF_NOT_CURRENT_PHASE" {
					t.Fatalf("second report = %+v, want refused", again.Refusal)
				}
			}
			if which == "cleared issue" || which == "cleared tree" {
				row = phaseRowOf(t, pool, claim.RoleTester)
				row.Entry++
				row.EntryTask = "test the new run"
				seedPhase(t, pool, row)
				if next := apply("new-start", fact); next.Refusal != nil {
					t.Fatalf("new generation's started step refused: %+v", next.Refusal)
				}
			}
		})
	}
}

func TestAStoppedStepsLateReportNeedsItsStartedRun(t *testing.T) {
	for _, started := range []bool{true, false} {
		name := "started"
		if !started {
			name = "no task"
		}
		t.Run(name, func(t *testing.T) {
			pool := migratedPool(t)
			engine := returnEngine()
			root := stoppedAtTesting(t, pool, engine)
			token := seedPhaseWorker(t, pool, root.Key, claim.RoleTester, "")
			row := phaseRowOf(t, pool, claim.RoleTester)
			if !started {
				row.EntryTask = ""
				seedPhase(t, pool, row)
			}
			before := reportedOf(t, pool, engine, root.Key, claim.RoleTester, true)
			result := applyFacts(t, pool, engine)("late", intake.HandoffComplete{Generation: root.Generation, Issue: root.Key, Role: claim.RoleTester, Claim: token, Summary: "tests pass", Verdict: "pass", Commit: "c0ffee", HandoffRecord: "test-late-1", HandoffVersion: 1})
			want := "HANDOFF_NOT_CURRENT_PHASE"
			if started {
				want = "TASK_STOPPED"
				before.row.LastHandoff = "test-late-1"
			}
			if result.Refusal == nil || result.Refusal.Status != 409 || result.Refusal.Code != want {
				t.Fatalf("result = %+v, want 409 %s", result.Refusal, want)
			}
			if after := reportedOf(t, pool, engine, root.Key, claim.RoleTester, true); !reflect.DeepEqual(after, before) {
				t.Fatalf("late refusal changed records:\nwant %+v\ngot %+v", before, after)
			}
		})
	}
}
