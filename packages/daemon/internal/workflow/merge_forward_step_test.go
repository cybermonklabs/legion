package workflow

// A merge forward is asked at step as at the phases of Legion's own workers that read the head: an
// issue whose task runs a step of a role the deployment defines after the implementer, with its
// head confirmed in conflict with its base, keeps its step, the implementer is asked to merge the
// base forward, and the role of that step is told to hold (classify.MergeForwardStands,
// mergeForwardWorker, record.MergeForwardReaches); the landing of the merge tells that role it
// landed. Its completion at the head the merge replaced is then refused
// HANDOFF_HEAD_MERGED_FORWARD, naming the head the merge left, as a tester's is
// (mergedForwardRefusal), so no step passes on code the issue no longer stands on. The control is
// the same task's tester step, whose tester is told and refused the same way.

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/plan"
	"github.com/sjawhar/legion/daemon/internal/record"
)

func TestAMergeForwardAtAStepOfADeploymentRoleTellsThatRole(t *testing.T) {
	requiresCI := &record.BaseRules{Required: []string{"ci"}}
	for _, tc := range []struct {
		name   string
		at     phase.Phase
		status string
		steps  []string
		worker claim.Role
	}{
		{name: "a step of a role the deployment defines", at: phase.Step, status: "in_progress", steps: []string{"implementer", "checker"}, worker: "checker"},
		{name: "control: a tester's step", at: phase.Testing, status: "testing", steps: []string{"implementer", "tester"}, worker: claim.RoleTester},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := seedConflictIssue(t, tc.at, tc.status, settledGreen("lint"))
			seedStepsPlan(t, pool, "LEGION-208", 1, tc.steps...)
			seedRecord(t, pool, func(tx pgx.Tx) error {
				return record.NewStore().MoveTask(t.Context(), tx, "LEGION-208", 1, "a", record.TaskPlace{Kind: record.PlaceStep, Position: 2}, record.TaskRunning)
			})
			if tc.at == phase.Step {
				seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: tc.worker, Claim: claim.Token(string(tc.worker) + "-claim"), Entry: 1})
			}

			confirmsConflict(t, pool, requiresCI)
			askedOnce(t, pool, tc.at, tc.status, record.ConflictCheckNotGreen, tc.worker)

			asked := len(mergeForwardMessages(t, pool))
			landsMergeForward(t, pool)
			want := []string{string(tc.worker) + " landed"}
			if got := toldWho(mergeForwardMessages(t, pool)[asked:]); !reflect.DeepEqual(got, want) {
				t.Fatalf("the landing queued %v, want %v", got, want)
			}
			if got := issuePhase(t, pool); got != tc.at {
				t.Fatalf("after the merge landed the issue is in %s, want it still in %s", got, tc.at)
			}

			complete := intake.HandoffComplete{Generation: 1, Issue: "LEGION-208", Role: tc.worker, Claim: claim.Token(string(tc.worker) + "-claim"),
				Summary: "checked", Verdict: "pass", Commit: conflictHead}
			if tc.at == phase.Testing {
				complete.HandoffRecord, complete.HandoffVersion = "handoff-1", 1
			}
			result := applyFacts(t, pool, readsAfter(4*time.Minute))("at the replaced head", complete)
			if code := refusalCode(result); code != "HANDOFF_HEAD_MERGED_FORWARD" {
				t.Fatalf("the %s's completion at %s, the head the merge replaced, was answered %q (%+v), want HANDOFF_HEAD_MERGED_FORWARD",
					tc.worker, conflictHead, code, result.Refusal)
			}
			if says := "do the phase's work at head " + forwardHead; !strings.Contains(result.Refusal.Message, says) {
				t.Fatalf("the refusal reads %q, want it to say %q", result.Refusal.Message, says)
			}
			if got := issuePhase(t, pool); got != tc.at {
				t.Fatalf("after the refused completion the issue is in %s, want it still in %s", got, tc.at)
			}
		})
	}
}

// A merge forward is asked at a step of a role the deployment defines only where that step stands
// after the implementer on its task's line, as at a planner's step: before the implementer the
// branch holds no implementation to merge the base into. The task `checker implementer linter`
// runs two steps at step, the checker's before the implementer and the linter's after it, so its
// line reads step at place 1 and at place 3, and only the place the task stands at tells them
// apart (classify.MergeForwardStands and classify.MergeStateReadOwed, which the workflow hands
// that place). At the checker's place 1 a confirmed conflict asks nothing and tells nobody, and a
// push to the head or to the base asks no read of GitHub's merge state; at the linter's place 3 the
// conflict asks the implementer and tells the linter to hold, the role of the running step
// (mergeForwardWorker), and each push asks the read. A rule that reads where the issue stands from
// its phase finds a step after the implementer on that line at either place, and fails place 1; a
// worker read that names only Legion's own workers tells the linter nothing, and fails place 3.
func TestAMergeForwardIsAskedAtADeploymentStepOnlyAfterTheImplementer(t *testing.T) {
	requiresCI := &record.BaseRules{Required: []string{"ci"}}
	for _, tc := range []struct {
		name  string
		at    int
		role  claim.Role
		asked bool
	}{
		{name: "the checker's place 1, before the implementer", at: 1, role: "checker"},
		{name: "the linter's place 3, after the implementer", at: 3, role: "linter", asked: true},
	} {
		seed := func(t *testing.T) *pgxpool.Pool {
			t.Helper()
			pool := seedConflictIssue(t, phase.Step, "in_progress", settledGreen("lint"))
			seedStepsPlan(t, pool, "LEGION-208", 1, "checker", "implementer", "linter")
			seedRecord(t, pool, func(tx pgx.Tx) error {
				return record.NewStore().MoveTask(t.Context(), tx, "LEGION-208", 1, "a", record.TaskPlace{Kind: record.PlaceStep, Position: tc.at}, record.TaskRunning)
			})
			seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: tc.role, Claim: claim.Token(string(tc.role) + "-claim"), Entry: 1})
			return pool
		}
		reads := []string{}
		if tc.asked {
			reads = []string{"LEGION-208"}
		}

		t.Run(tc.name+", a confirmed conflict", func(t *testing.T) {
			pool := seed(t)
			confirmsConflict(t, pool, requiresCI)
			if got := issuePhase(t, pool); got != phase.Step {
				t.Fatalf("after the confirmed conflict the issue is in %s, want it still at step", got)
			}
			if !tc.asked {
				if asked := recordedPR(t, pool, "LEGION-208").MergeForward; asked != nil {
					t.Fatalf("the conflict at place %d asked a merge forward %+v, want none before the implementer", tc.at, asked)
				}
				if told := toldWho(mergeForwardMessages(t, pool)); len(told) != 0 {
					t.Fatalf("the conflict at place %d queued %v, want no merge-forward message", tc.at, told)
				}
				return
			}
			askedOnce(t, pool, phase.Step, "in_progress", record.ConflictCheckNotGreen, tc.role)
		})
		t.Run(tc.name+", a head push", func(t *testing.T) {
			pool := seed(t)
			asked := []string{}
			applyRefusingNothing(t, pool, readingEngine(&asked), intake.PullRequestSynchronized{Repo: "sjawhar/legion", Number: 42,
				Branch: "legion/LEGION-208", Base: "main", HeadSHA: headTwo, Before: conflictHead, Sender: "legion-implementer[bot]"})
			if !reflect.DeepEqual(asked, reads) {
				t.Fatalf("the head push at place %d asked reads of %v, want %v", tc.at, asked, reads)
			}
		})
		t.Run(tc.name+", a base push", func(t *testing.T) {
			pool := seed(t)
			asked := []string{}
			applyRefusingNothing(t, pool, readingEngine(&asked), basePushOntoMain)
			if !reflect.DeepEqual(asked, reads) {
				t.Fatalf("the base push at place %d asked reads of %v, want %v", tc.at, asked, reads)
			}
		})
	}
}

// A merge forward tells nothing to the role of a step the deployment no longer defines: with the
// checker dropped from the settings, the issue at the checker's step has the implementer asked to
// merge the base forward and the checker sent no hold, and the merge's landing sends the checker
// no landed message, since that role's task is stopped until the role is defined again and a
// message held for its released claim would bring the claim back (mergeForwardWorker). The
// control is the same issue under a deployment that defines the checker, which is told both.
func TestAMergeForwardTellsNoRoleTheDeploymentLost(t *testing.T) {
	requiresCI := &record.BaseRules{Required: []string{"ci"}}
	for _, tc := range []struct {
		name          string
		lost          bool
		asked, landed []string
	}{
		{name: "the checker lost", lost: true, asked: []string{"implementer ask"}, landed: []string{}},
		{name: "control: the checker defined", asked: []string{"implementer ask", "checker hold"}, landed: []string{"checker landed"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			engine := func(after time.Duration) *Engine {
				e := readsAfter(after)
				if tc.lost {
					e.cfg.Deployment = plan.Deployment{Roles: plan.BuiltinRoles()}
				}
				return e
			}
			pool := seedConflictIssue(t, phase.Step, "in_progress", settledGreen("lint"))
			seedStepsPlan(t, pool, "LEGION-208", 1, "implementer", "checker")
			seedRecord(t, pool, func(tx pgx.Tx) error {
				return record.NewStore().MoveTask(t.Context(), tx, "LEGION-208", 1, "a", record.TaskPlace{Kind: record.PlaceStep, Position: 2}, record.TaskRunning)
			})
			seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: "checker", Claim: claim.Token("checker-claim"), Entry: 1})

			applyRefusingNothing(t, pool, engine(0), conflictingRead(requiresCI))
			applyRefusingNothing(t, pool, engine(conflictConfirmWait), conflictingRead(requiresCI))
			if got := toldWho(mergeForwardMessages(t, pool)); !reflect.DeepEqual(got, tc.asked) {
				t.Fatalf("the merge forward queued %v, want %v", got, tc.asked)
			}
			asked := len(mergeForwardMessages(t, pool))
			applyRefusingNothing(t, pool, engine(3*time.Minute), intake.PullRequestSynchronized{Repo: "sjawhar/legion", Number: 42,
				Branch: "legion/LEGION-208", Base: "main", HeadSHA: forwardHead, Before: conflictHead, Sender: "legion-implementer[bot]"})
			if got := toldWho(mergeForwardMessages(t, pool)[asked:]); !reflect.DeepEqual(got, tc.landed) {
				t.Fatalf("the landing queued %v, want %v", got, tc.landed)
			}
		})
	}
}

// A merge forward tells nothing to a claim that runs in the slot of a role the deployment no longer
// defines, whatever its step's role: in `implementer checker:a tester:a` key a's agent is named
// checker (record.NameAgents) and runs the tester's step in the checker's slot, so with the checker
// dropped from the settings, the issue at that tester step has the implementer asked to merge the
// base forward and the tester's claim, key a's, sent no hold, and the landing sends it no landed
// message: its task is stopped, and a message held for it would wake a claim no pod of the
// deployment has a slot for. The control is the same issue under a deployment that defines the
// checker, whose tester is told both.
func TestAMergeForwardTellsNoClaimInTheSlotOfARoleTheDeploymentLost(t *testing.T) {
	requiresCI := &record.BaseRules{Required: []string{"ci"}}
	for _, tc := range []struct {
		name          string
		lost          bool
		asked, landed []string
	}{
		{name: "the checker lost", lost: true, asked: []string{"implementer ask"}, landed: []string{}},
		{name: "control: the checker defined", asked: []string{"implementer ask", "tester hold"}, landed: []string{"tester landed"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			engine := func(after time.Duration) *Engine {
				e := readsAfter(after)
				if tc.lost {
					e.cfg.Deployment = plan.Deployment{Roles: plan.BuiltinRoles()}
				}
				return e
			}
			pool := seedConflictIssue(t, phase.Testing, "testing", settledGreen("lint"))
			seedStepsPlan(t, pool, "LEGION-208", 1, "implementer", "checker:a", "tester:a")
			seedRecord(t, pool, func(tx pgx.Tx) error {
				return record.NewStore().MoveTask(t.Context(), tx, "LEGION-208", 1, "a", record.TaskPlace{Kind: record.PlaceStep, Position: 3}, record.TaskRunning)
			})
			seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleTester, Claim: "legion-legion-legion-208-checker", StepTask: "a",
				StepPlace: 3, Entry: 1})

			applyRefusingNothing(t, pool, engine(0), conflictingRead(requiresCI))
			applyRefusingNothing(t, pool, engine(conflictConfirmWait), conflictingRead(requiresCI))
			if got := toldWho(mergeForwardMessages(t, pool)); !reflect.DeepEqual(got, tc.asked) {
				t.Fatalf("the merge forward queued %v, want %v", got, tc.asked)
			}
			asked := len(mergeForwardMessages(t, pool))
			applyRefusingNothing(t, pool, engine(3*time.Minute), intake.PullRequestSynchronized{Repo: "sjawhar/legion", Number: 42,
				Branch: "legion/LEGION-208", Base: "main", HeadSHA: forwardHead, Before: conflictHead, Sender: "legion-implementer[bot]"})
			if got := toldWho(mergeForwardMessages(t, pool)[asked:]); !reflect.DeepEqual(got, tc.landed) {
				t.Fatalf("the landing queued %v, want %v", got, tc.landed)
			}
		})
	}
}

// A merge forward whose implementer cannot be asked is asked of nobody, and the record keeps no
// ask: in `checker:a implementer:a tester` key a's agent is named checker (record.NameAgents) and
// works the implementer's step in the checker's slot, so with the checker dropped from the
// settings an ask would wake a claim no pod of the deployment has a slot for. The issue at the
// tester's step is then sent no message: no ask, and no hold, which would keep the tester waiting
// for a merge nobody makes. Its architect is told of no ask, and the record's merge forward stays
// empty, so a later confirmed read of that head asks again, under a deployment that defines the
// checker again. The control is the same issue under a deployment that defines the checker: the
// ask and the hold go, the architect is told, and the record keeps the ask.
func TestAMergeForwardWhoseImplementerRunsInALostSlotAsksNothingAndRecordsNoAsk(t *testing.T) {
	requiresCI := &record.BaseRules{Required: []string{"ci"}}
	for _, tc := range []struct {
		name string
		lost bool
	}{
		{name: "the checker lost", lost: true},
		{name: "control: the checker defined"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			engine := func(after time.Duration) *Engine {
				e := readsAfter(after)
				if tc.lost {
					e.cfg.Deployment = plan.Deployment{Roles: plan.BuiltinRoles()}
				}
				return e
			}
			pool := seedConflictIssue(t, phase.Testing, "testing", settledGreen("lint"))
			seedStepsPlan(t, pool, "LEGION-208", 1, "checker:a", "implementer:a", "tester")
			seedRecord(t, pool, func(tx pgx.Tx) error {
				return record.NewStore().MoveTask(t.Context(), tx, "LEGION-208", 1, "a", record.TaskPlace{Kind: record.PlaceStep, Position: 3}, record.TaskRunning)
			})
			seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "legion-legion-legion-208-checker", StepTask: "a",
				StepPlace: 2, Entry: 1})
			seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleTester, Claim: "legion-legion-legion-208-tester", StepTask: "a",
				StepPlace: 3, Entry: 1})

			applyRefusingNothing(t, pool, engine(0), conflictingRead(requiresCI))
			applyRefusingNothing(t, pool, engine(conflictConfirmWait), conflictingRead(requiresCI))
			told, asked, notices := toldWho(mergeForwardMessages(t, pool)), recordedPR(t, pool, "LEGION-208").MergeForward, capNotices(t, pool, "checks-red")
			if tc.lost {
				if len(told) != 0 || asked != nil || notices != 0 {
					t.Fatalf("the merge forward queued %v, recorded the ask %+v and told the architect %d times, want nothing: the implementer cannot be asked",
						told, asked, notices)
				}
				return
			}
			if want := []string{"implementer ask", "tester hold"}; !reflect.DeepEqual(told, want) || asked == nil || asked.Head != conflictHead || notices != 1 {
				t.Fatalf("the merge forward queued %v, recorded the ask %+v and told the architect %d times, want %v, the ask of %s and one notice",
					told, asked, notices, want, conflictHead)
			}
		})
	}
}
