package api

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/plan"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// A role that stands at two steps of a task completes each, and the two completions are two facts
// even where they report the same thing in one pass: the key names the place of the step when the
// task names the role at two places. In `implementer tester:a reviewer tester:a`, the tester's
// completions of steps 2 and 4 name one handoff record and the verdict pass in pass 0, with no
// return between; both are applied, and neither is answered HANDOFF_ALREADY_RECORDED, so the
// workflow judges the second (which refuses a record named again, HANDOFF_NOT_NEW). Before, the
// key named no place, the second was taken as the first's duplicate, and the task never moved
// past step 4. A role at one place keeps the key it had (TestTheCompletionKeyNamesTheTaskOfAnIssueWithPlanRows).
func TestTwoCompletionsOfOneRoleInOnePassAreTwoFacts(t *testing.T) {
	h, _ := newHandoffHarness(t, newHandoffGitHub(t), nil, nil)
	const key = "LEGION-208"
	seedIssueAt(t, h, key, phase.Testing)
	seedStepsAt(t, h, key, []string{"implementer", "tester:a", "reviewer", "tester:a"}, 2, claim.RoleTester, "", nil)
	tester := newLiveClaim(t, h, key, claim.RoleTester)
	written := tester.writeHandoff("test", 1)
	request := HandoffCompleteRequest{GrantID: tester.grant(t), Summary: "tests pass", Verdict: "pass", HandoffID: written, HandoffVersion: 1}
	completion(t, h.complete(request))

	runTaskAt(t, h, key, 4)
	request.GrantID = tester.grant(t)
	if answer := h.complete(request); answer.Code != http.StatusOK {
		t.Fatalf("the tester's completion of step 4 = %d %s, want it applied as a fact of its own", answer.Code, answer.Body)
	}
	prefix := "handoff:" + key + ":1:1:1:tester:testing:a:0:" + written + ":pass"
	if got, want := completionKeys(t, h, key), []string{prefix + ":p2", prefix + ":p4"}; !slices.Equal(got, want) {
		t.Fatalf("the completion keys of %s = %q, want %q", key, got, want)
	}
}

// runTaskAt runs task a of key's plan at step place.
func runTaskAt(t *testing.T, h *harness, key string, place int) {
	t.Helper()
	if err := store.Transact(context.Background(), h.store.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		return record.NewStore().MoveTask(ctx, tx, key, 1, "a", record.TaskPlace{Kind: record.PlaceStep, Position: place}, record.TaskRunning)
	}); err != nil {
		t.Fatalf("run task a of %s at step %d: %v", key, place, err)
	}
}

// seedStepsAt gives key, at generation 1, the owner's plan of task a whose steps are words, each
// `role` or `role:key`, with the type plan.BuiltinRoles gives its role, each named as planGiven
// names a version's agents (record.NameAgents), with the task running at place, and role's row of
// each place in entries at that entry, under token.
func seedStepsAt(t *testing.T, h *harness, key string, words []string, place int, role claim.Role, token claim.Token, entries map[int]int64) {
	t.Helper()
	ctx, records := context.Background(), record.NewStore()
	builtin := plan.BuiltinRoles()
	if err := store.Transact(ctx, h.store.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		v := record.PlanVersion{Issue: key, Generation: 1, Version: 1, ArtifactID: "plan-" + key, Source: record.PlanSourceOwner,
			GivenBy: "owner", GivenAt: time.Date(2026, 11, 22, 7, 0, 0, 0, time.UTC)}
		task := record.PlanTask{Issue: key, Generation: 1, Task: "a", PlanVersion: 1, BlockText: "Task a.", State: record.TaskWaiting}
		steps := make([]record.PlanStep, 0, len(words))
		for i, word := range words {
			name, agentKey, _ := strings.Cut(word, ":")
			steps = append(steps, record.PlanStep{Issue: key, Generation: 1, Task: "a", Position: i + 1, Role: name, AgentKey: agentKey, Type: builtin[name].Type})
		}
		if err := records.PutPlan(ctx, tx, v, []record.PlanTask{task}, record.NameAgents(nil, nil, steps, nil)); err != nil {
			return err
		}
		for at := range len(words) + 1 {
			if entry, ok := entries[at]; ok {
				if err := records.PutPhase(ctx, tx, record.PhaseRow{Issue: key, Role: role, Claim: token, Entry: entry, StepTask: "a", StepPlace: at}); err != nil {
					return err
				}
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed the steps %v of %s: %v", words, key, err)
	}
	runTaskAt(t, h, key, place)
}

// The route holds a completion to the step its turn was given, as the workflow does. In
// `implementer tester:a reviewer tester:a`, a forward move entered step 4 at the head step 2 left,
// and the agent's turn of step 2, still open on step 2's entry, completes at the pull request's head
// with a record of its own: its entry is at or below step 2's row, another step of the role, so the
// route refuses it HANDOFF_STALE_ENTRY, saying so, and applies no fact. The control is a role at one
// step, `implementer tester:a reviewer`, whose step a forward move handed again at the same head: the
// same completion is taken, as it was before a role stood at two steps. Without the route's read of
// the other step (record.OtherStepsEntry) the first completion was applied, and only the
// workflow's own fence, behind this route, refused it.
func TestTheRouteRefusesATurnOfARolesOtherStep(t *testing.T) {
	const key = "LEGION-208"
	for _, tc := range []struct {
		name    string
		roles   []string
		place   int
		entries map[int]int64
		refused bool
	}{
		{"the turn of step 2 completing while step 4 runs", []string{"implementer", "tester:a", "reviewer", "tester:a"}, 4, map[int]int64{2: 1, 4: 3}, true},
		{"the control: a role at one step handed again at the same head", []string{"implementer", "tester:a", "reviewer"}, 2, map[int]int64{2: 3}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, facts := newHandoffHarness(t, newHandoffGitHub(t), nil, nil)
			seedIssueAt(t, h, key, phase.Testing)
			seedPullRequest(t, h, key, headSHA)
			tester := newLiveClaim(t, h, key, claim.RoleTester)
			seedStepsAt(t, h, key, tc.roles, tc.place, claim.RoleTester, tester.token, tc.entries)
			takeTask(t, h, tester.token, "outbox:77", "test "+key, 1, 1)

			answer := h.complete(HandoffCompleteRequest{GrantID: tester.grant(t), Summary: "tests pass", Verdict: "pass",
				Commit: headSHA, HandoffID: tester.writeHandoff("test", 1), HandoffVersion: 1})
			if !tc.refused {
				completion(t, answer)
				if got := facts.recorded(); len(got) != 1 {
					t.Fatalf("facts after the completion = %#v, want the one completion applied", got)
				}
				return
			}
			message := refused(t, answer, http.StatusConflict, intake.StaleEntryCode)
			for _, want := range []string{
				"this completion is at commit " + headSHA + " while the pull request the record holds is at head " + headSHA,
				"the entry this completion serves is of another of this role's steps or its retro, or of a run of this step that the task left for one of them",
				"nothing was recorded",
			} {
				if !strings.Contains(message, want) {
					t.Fatalf("the refusal = %q, want it to say %q", message, want)
				}
			}
			if got := facts.recorded(); len(got) != 0 {
				t.Fatalf("facts after the refusal = %#v, want none applied: the route refuses before the fact", got)
			}
		})
	}
}

// A late completion of a role's later step is held to the row of that step's run, the run whose
// entry it serves. In `implementer:a reviewer:r implementer:a reviewer:r`, the review of step 4
// requested changes before the reviewer completed, so the task stands at step 3, between the
// reviewer's two steps, and the reviewer's claim still serves step 4's entry. Its completion is
// applied, its key naming step 4 (`:p4`). The control is the same completion serving an entry no
// row of the reviewer stands on: refused HANDOFF_NOT_CURRENT_PHASE, with no fact applied.
// Before, the route read the reviewer's latest place at or before step 3, step 2's row, whose entry
// is below step 4's, and refused the completion of step 4 HANDOFF_NOT_CURRENT_PHASE too.
func TestALateCompletionOfARolesLaterStepIsHeldToThatStep(t *testing.T) {
	const key = "LEGION-208"
	for _, tc := range []struct {
		name   string
		served int64
		taken  bool
	}{
		{"serving step 4's entry", 4, true},
		{"the control: serving an entry no row stands on", 3, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, facts := newHandoffHarness(t, newHandoffGitHub(t), nil, nil)
			seedIssueAt(t, h, key, phase.Implementing)
			seedPullRequest(t, h, key, headSHA)
			reviewer := newLiveClaim(t, h, key, claim.RoleReviewer)
			seedStepsAt(t, h, key, []string{"implementer:a", "reviewer:r", "implementer:a", "reviewer:r"}, 3, claim.RoleReviewer, reviewer.token,
				map[int]int64{2: 2, 4: 4})
			takeTask(t, h, reviewer.token, "outbox:77", "review "+key, tc.served, 1)

			answer := h.complete(HandoffCompleteRequest{GrantID: reviewer.grant(t), Summary: "changes requested",
				HandoffID: reviewer.writeHandoff("review", 2), HandoffVersion: 2})
			if !tc.taken {
				refused(t, answer, http.StatusConflict, "HANDOFF_NOT_CURRENT_PHASE")
				if got := facts.recorded(); len(got) != 0 {
					t.Fatalf("facts after the refusal = %#v, want none applied", got)
				}
				return
			}
			completion(t, answer)
			if got := facts.recorded(); len(got) != 1 {
				t.Fatalf("facts after the completion = %#v, want the one completion applied", got)
			}
			if keys := completionKeys(t, h, key); len(keys) != 1 || !strings.HasSuffix(keys[0], ":p4") {
				t.Fatalf("the completion keys of %s = %q, want one, naming step 4 (:p4)", key, keys)
			}
		})
	}
}

// The route refuses a completion from a claim of the role that does not work the step before its
// entry fence, as the workflow does. In `implementer:a tester:a reviewer`, the tester's row names
// key a's agent on step 2 at entry 2, and the tester's own claim, serving entry 1, which no row of
// the role stands on, completes at a commit the pull request has left: the route answers 409
// HANDOFF_NOT_THE_STEPS_WORKER naming key a's agent and applies no fact. The control is the same
// completion where the row names the tester's own claim: the fence answers HANDOFF_STALE_ENTRY.
// Before, the route asked the fence alone, and answered the other claim HANDOFF_STALE_ENTRY too,
// whose words promise it the task of entry 2, which goes to key a's agent.
func TestTheRouteRefusesAClaimThatDoesNotWorkTheStepBeforeItsFence(t *testing.T) {
	const key = "LEGION-208"
	const keyed = claim.Token("legion-legion-legion-208-implementer")
	for _, tc := range []struct {
		name  string
		keyed bool
		code  string
	}{
		{"the row names key a's agent", true, "HANDOFF_NOT_THE_STEPS_WORKER"},
		{"the control: the row names the tester's own claim", false, intake.StaleEntryCode},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, facts := newHandoffHarness(t, newHandoffGitHub(t), nil, nil)
			seedIssueAt(t, h, key, phase.Testing)
			seedPullRequest(t, h, key, headSHA)
			tester := newLiveClaim(t, h, key, claim.RoleTester)
			holder := tester.token
			if tc.keyed {
				holder = keyed
			}
			seedStepsAt(t, h, key, []string{"implementer:a", "tester:a", "reviewer"}, 2, claim.RoleTester, holder, map[int]int64{2: 2})
			takeTask(t, h, tester.token, "outbox:77", "test "+key, 1, 1)

			message := refused(t, h.complete(HandoffCompleteRequest{GrantID: tester.grant(t), Summary: "tests pass", Verdict: "pass",
				Commit: strings.Repeat("ab", 20), HandoffID: tester.writeHandoff("test", 1), HandoffVersion: 1}), http.StatusConflict, tc.code)
			if tc.keyed && (!strings.Contains(message, string(keyed)+" works its step") || strings.Contains(message, intake.StaleEntryInstruction)) {
				t.Fatalf("the refusal = %q, want it to name %s as the step's worker and promise no task", message, keyed)
			}
			if got := facts.recorded(); len(got) != 0 {
				t.Fatalf("facts after the refusal = %#v, want none applied", got)
			}
		})
	}
}
