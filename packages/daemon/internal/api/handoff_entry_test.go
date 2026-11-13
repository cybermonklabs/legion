package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// A tester's turn outlives its phase: the daemon ends no turn when a phase ends, so when the
// head starts conflicting with its base the implementer starts at once and the tester goes on
// working. Its issue comes back to
// testing on the implementer's pass, where a second testing entry hands the tester a second
// task — and the completion the first task's turn reports is of work the issue has moved past. The
// run cannot tell the two apart, since the issue never left run 1, and neither can the phase, which
// is testing both times. The entry can: the tester's claim still serves the first task's, because
// the machine refuses a second delivery while one is pending, while the phases row is already on
// the second, raised in the transition's own transaction. The completion is refused
// HANDOFF_STALE_ENTRY before any fact is applied, and the refusal sends the tester to the
// new task's own work: that task's turn reports a handoff of its own, which is taken.
func TestAStaleEntrysCompletionDoesNotTakeTheNextTestingPhases(t *testing.T) {
	h, engine, facts := newEntryHarness(t)
	ctx := context.Background()
	h.recordIssue(record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: "LEGION-208",
		Project: testProject, Title: "LEGION-208", Phase: phase.Implementing, Generation: 1, Status: "in_progress"})
	if err := h.store.Tx(ctx, func(tx pgx.Tx) error {
		return record.NewStore().PutPullRequest(ctx, tx, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "acme/widgets",
			Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head", CheckedHead: "head", Failing: []string{}})
	}); err != nil {
		t.Fatalf("record the pull request: %v", err)
	}

	// The implementer's round ends and the issue enters testing: the tester's first entry.
	implementer := newLiveClaim(t, h, "LEGION-208", claim.RoleImplementer)
	completion(t, h.complete(HandoffCompleteRequest{GrantID: implementer.grant(t), Summary: "implemented", Commit: "handoff-1",
		HandoffID: implementer.writeHandoff("implement", 1), HandoffVersion: 1}))
	assertIssuePhase(t, h, phase.Testing)
	tester := newLiveClaim(t, h, "LEGION-208", claim.RoleTester)
	firstTesting := takeStart(t, h, tester.token, "LEGION-208", claim.RoleTester)

	// The head starts conflicting with its base while the tester works. The issue goes back to
	// implementing, the implementer starts at once, and the tester's turn, its process and its
	// claim are untouched: its delivery is still pending and confirmed.
	if result, err := intake.ApplyFact(ctx, h.store.Pool(), "github", "conflict", intake.PullRequestMergeability{Repo: "acme/widgets", Number: 42,
		HeadSHA: "head", Base: "main", Mergeable: record.MergeabilityConflicting}, engine); err != nil || result.Refusal != nil {
		t.Fatalf("apply the conflicting head = %+v, %v", result.Refusal, err)
	}
	assertIssuePhase(t, h, phase.Implementing)
	testerMachine, ok := h.supervisor.Machine(tester.token)
	if !ok {
		t.Fatal("no machine for the tester")
	}
	if p := testerMachine.Claim().Pending; p == nil || p.ConfirmedAt.IsZero() || p.Entry != firstTesting {
		t.Fatalf("the tester's delivery after the conflict is %+v, want the task of its first entry still confirmed and pending", p)
	}

	// The implementer's second round ends and the issue is in testing again: the tester's second
	// entry, which its first task's turn knows nothing of.
	takeStart(t, h, implementer.token, "LEGION-208", claim.RoleImplementer)
	completion(t, h.complete(HandoffCompleteRequest{GrantID: implementer.grant(t), Summary: "merged the base forward", Commit: "handoff-2",
		HandoffID: implementer.writeHandoff("implement", 2), HandoffVersion: 2}))
	assertIssuePhase(t, h, phase.Testing)
	_, secondStart := queuedStart(t, h, "LEGION-208", claim.RoleTester)
	if secondStart.Entry != firstTesting+1 {
		t.Fatalf("the second testing start is of entry %d, want the %d after the first: each entry of the phase raises the row by one",
			secondStart.Entry, firstTesting+1)
	}
	if run := issueGeneration(t, h, "LEGION-208"); run != 1 {
		t.Fatalf("the issue is on run %d, want 1: the run never moved, which is why it cannot refuse this completion", run)
	}

	// The old turn reports its completion. Refused, with nothing recorded, and the call made again
	// naming the same record is refused the same way rather than answered ALREADY_RECORDED.
	firstRecord := tester.writeHandoff("test", 1)
	complete := func() string {
		t.Helper()
		recorder := h.complete(HandoffCompleteRequest{GrantID: tester.grant(t), Summary: "tests pass", Verdict: "pass", HandoffID: firstRecord, HandoffVersion: 1})
		return refused(t, recorder, http.StatusConflict, "HANDOFF_STALE_ENTRY")
	}
	before := recordedCompletions(t, h, claim.RoleTester)
	message := complete()
	for _, want := range []string{
		fmt.Sprintf("LEGION-208 handed the tester phase testing again: this completion is of entry %d, which is 1 behind the entry %d the phase is on",
			firstTesting, secondStart.Entry),
		"nothing was recorded",
		// The second entry's task is queued and no delivery has carried it to this claim, which
		// is the state the daemon sends the row's own work in (workflow's SendOwedEntry).
		"The task of the entry the phase is on is owed to this claim, and the daemon sends it by itself",
	} {
		if !strings.Contains(message, want) {
			t.Fatalf("the refusal is %q, want it to say %q", message, want)
		}
	}
	if strings.Contains(message, "your turn began before") {
		t.Fatalf("the refusal is %q, want it to name no queued task: no delivery has carried entry %d's to this claim", message, secondStart.Entry)
	}
	// What the refusal tells the worker to do: the second testing entry is for the head the
	// implementer's fix pushed, so this turn's verdict on the head before it is no answer for it.
	// The worker does that task's work and writes a handoff of its own, and the refusal never
	// sends this completion's record again.
	if !strings.HasSuffix(message, ". "+staleEntryInstruction) {
		t.Fatalf("the refusal is %q, want it to end with the instruction the legion tool's description carries, %q", message, staleEntryInstruction)
	}
	if !strings.Contains(message, "a handoff of its own") {
		t.Fatalf("the refusal is %q, want it to tell the worker to complete the new task with a handoff of its own", message)
	}
	if strings.Contains(message, "at the same commit") {
		t.Fatalf("the refusal is %q, want it never to offer this completion's commit again: the new entry's work is its own", message)
	}
	complete()
	if got := recordedCompletions(t, h, claim.RoleTester); got != before {
		t.Fatalf("the refused completions left %d recorded tester completions, want the %d there were: a refusal before the fact records nothing", got, before)
	}
	for _, fact := range facts.recorded() {
		if completed, ok := fact.(intake.HandoffComplete); ok && completed.Role == claim.RoleTester {
			t.Fatalf("the workflow saw the tester's completion %#v, want the route to have refused it", completed)
		}
	}
	assertIssuePhase(t, h, phase.Testing)

	// The second entry's task arrives; the tester tests the fixed head, writes the handoff that
	// task's work calls for, and completes in its turn, which is applied and moves the issue on
	// to reviewing.
	second := takeStart(t, h, tester.token, "LEGION-208", claim.RoleTester)
	if second != secondStart.Entry {
		t.Fatalf("the tester took entry %d, want the start's %d", second, secondStart.Entry)
	}
	completion(t, h.complete(HandoffCompleteRequest{GrantID: tester.grant(t), Summary: "tests pass on the fix", Verdict: "pass",
		HandoffID: tester.writeHandoff("test", 2), HandoffVersion: 2}))
	assertIssuePhase(t, h, phase.Reviewing)
	testers := 0
	for _, fact := range facts.recorded() {
		if completed, ok := fact.(intake.HandoffComplete); ok && completed.Role == claim.RoleTester {
			testers++
		}
	}
	if testers != 1 {
		t.Fatalf("the workflow saw %d tester completions, want the one the new entry reported", testers)
	}
}

// The second entry's task is delivered to the claim and the worker has not acknowledged it: it
// still serves the entry its last confirmed task carried, and reports that entry's completion in
// a turn of its own. The refusal is the same one, and it names the task waiting on the claim
// rather than telling the worker to wait for one.
func TestACompletionIsRefusedWhileTheNextEntrysTaskWaitsUnconfirmed(t *testing.T) {
	h, facts := newHandoffHarness(t, newHandoffGitHub(t), nil, nil)
	seedIssueAt(t, h, "LEGION-208", phase.Testing)
	seedEntry(t, h, "LEGION-208", claim.RoleTester, 1)
	tester := newLiveClaim(t, h, "LEGION-208", claim.RoleTester)
	takeTask(t, h, tester.token, "outbox:77", "test LEGION-208", 1, 1)

	// The phase is entered again: the row is raised to entry 2 and that entry's task is queued on
	// the claim, where it waits unread while a turn of the worker's own runs.
	seedEntry(t, h, "LEGION-208", claim.RoleTester, 2)
	queueTask(t, h, tester.token, "outbox:78", "test the fixed head of LEGION-208", 2, 1)
	machine, ok := h.supervisor.Machine(tester.token)
	if !ok {
		t.Fatal("no machine for the tester")
	}
	c := machine.Claim()
	if p := c.Pending; p == nil || !p.ConfirmedAt.IsZero() || p.Entry != 2 {
		t.Fatalf("the tester's delivery is %+v, want entry 2's task pending and unconfirmed", p)
	}
	if c.ServingEntry() != 1 {
		t.Fatalf("the tester serves entry %d, want the 1 its confirmed task carried: an unconfirmed task is none it took", c.ServingEntry())
	}

	message := refused(t, h.complete(HandoffCompleteRequest{GrantID: tester.grant(t), Summary: "tests pass", Verdict: "pass",
		HandoffID: tester.writeHandoff("test", 1), HandoffVersion: 1}), http.StatusConflict, "HANDOFF_STALE_ENTRY")
	for _, want := range []string{
		"LEGION-208 handed the tester phase testing again: this completion is of entry 1, which is 1 behind the entry 2 the phase is on",
		"nothing was recorded",
		"your turn began before the daemon's task for entry 2 (outbox:78) reached this session",
		"complete it with a handoff of its own, never this completion's record, commit or verdict",
	} {
		if !strings.Contains(message, want) {
			t.Fatalf("the refusal = %q, want it to say %q", message, want)
		}
	}
	if !strings.HasSuffix(message, ". "+staleEntryInstruction) {
		t.Fatalf("the refusal = %q, want it to end with the instruction the legion tool's description carries, %q", message, staleEntryInstruction)
	}
	if recorded := facts.recorded(); len(recorded) != 0 {
		t.Fatalf("the refused completion reached the workflow as %+v, want no fact", recorded)
	}
}

// What a worker refused HANDOFF_STALE_ENTRY is told to do is one text in two languages: the
// daemon ends every staleEntry refusal with staleEntryInstruction, and the legion tool's
// description (STALE_ENTRY_INSTRUCTION in `packages/pi-shared/src/legion-phase-ops.ts`) carries
// it after the code. This golden is where the two meet, as version.json is for the contract
// number: `packages/pi-shared/src/legion-phase-ops.test.ts` holds the description to the file,
// so a word changed on the Go side fails here until the fixture is written again
// (`go test ./internal/api/ -update`), and then fails there until the plugin's text says it too.
//
// The code is written out here beside it, so a rename of the constant's value — which would
// otherwise rewrite the fixture with nobody reading the diff — fails here by its own name.
func TestStaleEntryInstructionGolden(t *testing.T) {
	if staleEntryCode != "HANDOFF_STALE_ENTRY" {
		t.Fatalf("staleEntryCode = %q, want the wire code HANDOFF_STALE_ENTRY", staleEntryCode)
	}
	// What decided the refusal stands in the middle of it (entryHeadClause), so the instruction is
	// still the text the refusal ends with, whichever clause it carries.
	decided := entryHeadClause(true, "", headSHA, false)
	for clause, pending := range map[string]*supervise.Delivery{
		"the entry 2 the phase is on, and " + decided + "; nothing was recorded":                nil,
		"your turn began before the daemon's task for entry 2 (outbox:78) reached this session": {ID: "outbox:78", Entry: 2},
	} {
		refusal := staleEntry("LEGION-208", claim.RoleTester, phase.Testing, 2, 1, pending, decided)
		if !strings.HasSuffix(refusal, clause+". "+staleEntryInstruction) {
			t.Fatalf("staleEntry with pending %+v = %q, want %q and then exactly %q", pending, refusal, clause, staleEntryInstruction)
		}
	}
	golden(t, "stale-entry.json", map[string]string{"code": staleEntryCode, "instruction": staleEntryInstruction})
}

// The comparison is made only when both sides name an entry, which is what carries a daemon
// upgraded in place: every worker live at the release has a zero on both sides, and the first
// start each role is given stamps both at once. A row no start has stamped and a claim that has
// taken no task carrying an entry are the one-sided cases, and each is accepted — the fence is
// the pair, never one half of it. An entry above the row's is refused with one below it, naming
// both numbers: the transition raises the row in the transaction that composes the entry's task,
// so the record never produces it.
func TestACompletionIsHeldToItsEntryOnlyWhenBothSidesNameOne(t *testing.T) {
	for _, tc := range []struct {
		name    string
		row     int64
		serving int64
		refused bool
		says    []string
	}{
		{name: "a daemon upgraded in place: neither side names one"},
		{name: "the row names one, the claim's task was given before the upgrade", row: 2},
		{name: "a row no start has stamped, the claim serves its task's entry", serving: 1},
		{name: "both sides name the entry the phase is on", row: 1, serving: 1},
		{
			name: "the claim serves the entry before this phase's", row: 2, serving: 1, refused: true,
			says: []string{"this completion is of entry 1, which is 1 behind the entry 2 the phase is on"},
		},
		{
			name: "the claim serves an entry the phase has not raised to", row: 1, serving: 2, refused: true,
			says: []string{"this completion is of entry 2 of phase testing of LEGION-208, and the tester's phases row is on entry 1",
				"no task it hands out carries an entry above it"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			github := newHandoffGitHub(t)
			h, facts := newHandoffHarness(t, github, nil, nil)
			seedIssueAt(t, h, "LEGION-208", phase.Testing)
			if tc.row != 0 {
				seedEntry(t, h, "LEGION-208", claim.RoleTester, tc.row)
			}
			tester := newLiveClaim(t, h, "LEGION-208", claim.RoleTester)
			if tc.serving != 0 {
				takeTask(t, h, tester.token, "outbox:77", "test it again", tc.serving, 1)
			}
			recorder := h.complete(HandoffCompleteRequest{GrantID: tester.grant(t), Summary: "tests pass", Verdict: "pass",
				HandoffID: tester.writeHandoff("test", 1), HandoffVersion: 1})
			if tc.refused {
				message := refused(t, recorder, http.StatusConflict, "HANDOFF_STALE_ENTRY")
				for _, want := range tc.says {
					if !strings.Contains(message, want) {
						t.Fatalf("the refusal = %q, want it to say %q", message, want)
					}
				}
				if got := facts.recorded(); len(got) != 0 {
					t.Fatalf("facts after the refusal = %#v, want none applied", got)
				}
				return
			}
			if recorder.Code != http.StatusOK {
				t.Fatalf("the completion = %d, want 200; body %s", recorder.Code, recorder.Body)
			}
		})
	}
}

// A held phase's retry is the phase entry the worker is already on, resumed: the issue never left
// it, and the work is the work it had. The tester's claim fails, the issue is held with that
// claim still holding the task of its testing entry, and the architect's retry puts the issue
// back in testing. The outbox hands a relaunched claim no second copy of the task it still works
// (daemon's outbox, "not delivered again"), so the worker goes on serving the entry the held
// phase gave it: the retry's start is of that same entry, the row keeps it, and the completion
// the worker reports is applied. A retry that raised the entry would name one no start ever
// delivers, and every completion of that phase would be refused HANDOFF_STALE_ENTRY until
// something outside the workflow moved the issue.
func TestAHeldPhasesRetryKeepsTheEntryItsWorkerStillServes(t *testing.T) {
	h, engine, facts := newEntryHarness(t)
	ctx := context.Background()
	h.recordIssue(record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: "LEGION-208",
		Project: testProject, Title: "LEGION-208", Phase: phase.Implementing, Generation: 1, Status: "in_progress"})
	if err := h.store.Tx(ctx, func(tx pgx.Tx) error {
		return record.NewStore().PutPullRequest(ctx, tx, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "acme/widgets",
			Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head", CheckedHead: "head", Failing: []string{}})
	}); err != nil {
		t.Fatalf("record the pull request: %v", err)
	}

	// The implementer's round ends and the tester takes the task of the testing phase's entry.
	implementer := newLiveClaim(t, h, "LEGION-208", claim.RoleImplementer)
	completion(t, h.complete(HandoffCompleteRequest{GrantID: implementer.grant(t), Summary: "implemented", Commit: "handoff-1",
		HandoffID: implementer.writeHandoff("implement", 1), HandoffVersion: 1}))
	assertIssuePhase(t, h, phase.Testing)
	tester := newLiveClaim(t, h, "LEGION-208", claim.RoleTester)
	testing := takeStart(t, h, tester.token, "LEGION-208", claim.RoleTester)

	// The tester's claim fails — a budget ran out — and the issue is held. The claim keeps the task
	// it was working, and the turn it is in goes on serving that task's entry.
	applyEntryFact(t, h, engine, "claim-failed", intake.ClaimFailed{Issue: "LEGION-208", Role: claim.RoleTester,
		Reason: "the launch budget ran out", Cause: supervise.CauseLaunchRefused, Budgets: supervise.Budgets{LaunchFailures: 3},
		Limits: supervise.Limits{LaunchFailures: 3, PromptFailures: 3, PromptRetires: 2}})
	assertIssuePhase(t, h, phase.Held)

	// The architect retries the hold: testing again, with the same work, so the start is of the
	// entry the tester's row already names rather than one nothing would deliver.
	applyEntryFact(t, h, engine, "retry", intake.RetryOrEscalate{Issue: "LEGION-208", Decision: intake.RetryDecision})
	assertIssuePhase(t, h, phase.Testing)
	_, retry := queuedStart(t, h, "LEGION-208", claim.RoleTester)
	if retry.Entry != testing {
		t.Fatalf("the retry's start is of entry %d, want the %d the held phase handed the tester: a later one reaches the worker that still holds the old task", retry.Entry, testing)
	}
	if got := rowEntry(t, h, "LEGION-208", claim.RoleTester); got != testing {
		t.Fatalf("after the retry the tester's row is on entry %d, want the %d its phase handed out", got, testing)
	}

	// The relaunched worker finishes the task it held, and its completion is applied: the issue
	// moves on to reviewing, and the workflow saw exactly that one tester completion.
	completion(t, h.complete(HandoffCompleteRequest{GrantID: tester.grant(t), Summary: "tests pass", Verdict: "pass",
		HandoffID: tester.writeHandoff("test", 1), HandoffVersion: 1}))
	assertIssuePhase(t, h, phase.Reviewing)
	testers := 0
	for _, fact := range facts.recorded() {
		if completed, ok := fact.(intake.HandoffComplete); ok && completed.Role == claim.RoleTester {
			testers++
		}
	}
	if testers != 1 {
		t.Fatalf("the workflow saw %d tester completions, want the one the retried phase reported", testers)
	}
}

// The entry says a completion is behind; what the worker reported says whether that matters. A
// verdict is a fact about a head, and a phase can be handed to its role again at the head the
// role is already reporting on — the issue left the phase and came back with nothing pushed in
// between — where the second entry's task asks for the very work this turn did. The phase ends
// with a handoff record, which names the phase it is of, so the completion is readable as this
// phase's answer; the commit it names is the head the record holds. It is taken, and the fact it
// reports moves the issue on to reviewing.
func TestABehindCompletionAtTheRecordedHeadIsTaken(t *testing.T) {
	h, _, facts := newEntryHarness(t)
	seedIssueAt(t, h, "LEGION-208", phase.Testing)
	seedPullRequest(t, h, "LEGION-208", headSHA)
	seedEntry(t, h, "LEGION-208", claim.RoleTester, 2)
	tester := newLiveClaim(t, h, "LEGION-208", claim.RoleTester)
	takeTask(t, h, tester.token, "outbox:77", "test LEGION-208", 1, 1)
	machine, ok := h.supervisor.Machine(tester.token)
	if !ok {
		t.Fatal("no machine for the tester")
	}
	if got := machine.Claim().ServingEntry(); got != 1 {
		t.Fatalf("the tester serves entry %d, want the 1 its task carried while the row is on 2", got)
	}

	completion(t, h.complete(HandoffCompleteRequest{GrantID: tester.grant(t), Summary: "tests pass", Verdict: "pass",
		Commit: headSHA, HandoffID: tester.writeHandoff("test", 1), HandoffVersion: 1}))

	assertIssuePhase(t, h, phase.Reviewing)
	testers := 0
	for _, fact := range facts.recorded() {
		completed, ok := fact.(intake.HandoffComplete)
		if !ok || completed.Role != claim.RoleTester {
			continue
		}
		testers++
		if completed.Commit != headSHA || completed.Verdict != "pass" {
			t.Fatalf("the completion the workflow saw = %#v, want the pass at %s this turn reported", completed, headSHA)
		}
	}
	if testers != 1 {
		t.Fatalf("the workflow saw %d tester completions, want the one this turn reported", testers)
	}
	if got := recordedCompletions(t, h, claim.RoleTester); got != 1 {
		t.Fatalf("%d tester completions are recorded, want the one that was taken", got)
	}
}

// The head takes a behind completion only where the phase was handed out again for the same
// work, and a move back is the case where it was not: the architect's move back and the
// reviewer's request for changes ask for this phase's work to be done again, at the head that is
// already there, so the commit the turn reports and the head the record holds read alike and the
// heads alone would take a verdict the new entry exists to replace. What tells the two cases
// apart is the move the transition wrote onto the entry it raised
// (record.PhaseRow.EntryMovedBack), which the fence reads: this completion is refused at the
// very head that takes the one above, the refusal says the move is what refused it, and nothing
// is recorded. Its control is TestABehindCompletionAtTheRecordedHeadIsTaken, the same completion
// at the same head whose current entry a forward move raised.
func TestABehindCompletionWhoseEntryAMoveBackRaisedIsRefused(t *testing.T) {
	h, _, facts := newEntryHarness(t)
	seedIssueAt(t, h, "LEGION-208", phase.Testing)
	seedPullRequest(t, h, "LEGION-208", headSHA)
	seedMovedBackEntry(t, h, "LEGION-208", claim.RoleTester, 2)
	tester := newLiveClaim(t, h, "LEGION-208", claim.RoleTester)
	takeTask(t, h, tester.token, "outbox:77", "test LEGION-208", 1, 1)

	message := refused(t, h.complete(HandoffCompleteRequest{GrantID: tester.grant(t), Summary: "tests pass", Verdict: "pass",
		Commit: headSHA, HandoffID: tester.writeHandoff("test", 1), HandoffVersion: 1}), http.StatusConflict, "HANDOFF_STALE_ENTRY")
	for _, want := range []string{
		"LEGION-208 handed the tester phase testing again: this completion is of entry 1, which is 1 behind the entry 2 the phase is on",
		"this completion is at commit " + headSHA + " while the pull request the record holds is at head " + headSHA,
		"the move that handed the phase out again moved the issue back, which asks for this phase's work to be done again, so no verdict on either head answers for it",
		"nothing was recorded",
	} {
		if !strings.Contains(message, want) {
			t.Fatalf("the refusal = %q, want it to say %q", message, want)
		}
	}
	assertIssuePhase(t, h, phase.Testing)
	if got := facts.recorded(); len(got) != 0 {
		t.Fatalf("facts after the refusal = %#v, want none applied: the issue stays in the phase the move back put it in", got)
	}
	if got := recordedCompletions(t, h, claim.RoleTester); got != 0 {
		t.Fatalf("%d tester completions are recorded, want none: a refusal before the fact records nothing", got)
	}
}

// Only the head the record holds takes a behind completion, because that head is what the daemon
// knows the phase's work is of. A completion at the commit before it is a verdict on a head the
// issue has left, and one naming no commit says nothing about a head at all: each is refused as
// before, nothing is recorded, and the refusal names what the completion gave against what the
// record holds.
func TestABehindCompletionAwayFromTheRecordedHeadIsRefused(t *testing.T) {
	// The commit before headSHA: the head the tester tested before a push moved the pull request on.
	const earlierSHA = "a99e0000000000000000000000000000000000ff"
	for _, tc := range []struct {
		name   string
		commit string
		says   string
	}{
		{
			name: "the commit before the head the record holds", commit: earlierSHA,
			says: "this completion is at commit " + earlierSHA + " while the pull request the record holds is at head " + headSHA,
		},
		{
			name: "no commit at all",
			says: "this completion names no commit while the pull request the record holds is at head " + headSHA,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, facts := newHandoffHarness(t, newHandoffGitHub(t), nil, nil)
			seedIssueAt(t, h, "LEGION-208", phase.Testing)
			seedPullRequest(t, h, "LEGION-208", headSHA)
			seedEntry(t, h, "LEGION-208", claim.RoleTester, 2)
			tester := newLiveClaim(t, h, "LEGION-208", claim.RoleTester)
			takeTask(t, h, tester.token, "outbox:77", "test LEGION-208", 1, 1)

			message := refused(t, h.complete(HandoffCompleteRequest{GrantID: tester.grant(t), Summary: "tests pass", Verdict: "pass",
				Commit: tc.commit, HandoffID: tester.writeHandoff("test", 1), HandoffVersion: 1}), http.StatusConflict, "HANDOFF_STALE_ENTRY")
			for _, want := range []string{
				"LEGION-208 handed the tester phase testing again: this completion is of entry 1, which is 1 behind the entry 2 the phase is on",
				tc.says,
				"nothing was recorded",
			} {
				if !strings.Contains(message, want) {
					t.Fatalf("the refusal = %q, want it to say %q", message, want)
				}
			}
			if got := facts.recorded(); len(got) != 0 {
				t.Fatalf("facts after the refusal = %#v, want none applied", got)
			}
			if got := recordedCompletions(t, h, claim.RoleTester); got != 0 {
				t.Fatalf("%d tester completions are recorded, want none: a refusal before the fact records nothing", got)
			}
		})
	}
}

// Retro and merging end with no handoff record, so a completion of theirs reports a
// commit and nothing that says which entry's work it is: the record that would have named the
// phase, and that HANDOFF_RECORD_MISMATCH and HANDOFF_NOT_NEW hold a completion to, was never
// written. Each is refused behind its entry however its commit reads — here at the very head the
// record holds, which is what takes a tester's — and the refusal says that missing record is what
// decided it and names no head: at this head, naming the two would answer the worker with the one
// reading under which a behind completion is taken.
func TestABehindCompletionOfAPhaseWithNoRecordIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name string
		at   phase.Phase
		role claim.Role
	}{
		{name: "retro", at: phase.Retro, role: claim.RoleImplementer},
		{name: "merging", at: phase.Merging, role: claim.RoleMerger},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, facts := newHandoffHarness(t, newHandoffGitHub(t), nil, nil)
			seedIssueAt(t, h, "LEGION-208", tc.at)
			seedPullRequest(t, h, "LEGION-208", headSHA)
			seedEntry(t, h, "LEGION-208", tc.role, 2)
			worker := newLiveClaim(t, h, "LEGION-208", tc.role)
			takeTask(t, h, worker.token, "outbox:77", "finish LEGION-208", 1, 1)

			message := refused(t, h.complete(HandoffCompleteRequest{GrantID: worker.grant(t), Summary: "done", Commit: headSHA}),
				http.StatusConflict, "HANDOFF_STALE_ENTRY")
			for _, want := range []string{
				fmt.Sprintf("LEGION-208 handed the %s phase %s again: this completion is of entry 1, which is 1 behind the entry 2 the phase is on", tc.role, tc.at),
				"this phase ends with no handoff record this role writes, so nothing this completion names says which entry's work it is",
				"nothing was recorded",
			} {
				if !strings.Contains(message, want) {
					t.Fatalf("the refusal = %q, want it to say %q", message, want)
				}
			}
			if got := facts.recorded(); len(got) != 0 {
				t.Fatalf("facts after the refusal = %#v, want none applied", got)
			}
		})
	}
}

// What keeps an accepted behind completion from having the worker do the entry again is the rule
// every task is sent under: a task carries the phase it was queued for, and the issue has left
// that phase, so the delivery is dropped rather than sent (supervise's phaseHolds). Without that
// the worker would be given the second entry's task after its own answer had moved the issue on,
// and would do the same work again at the same head for the same answer.
func TestTheCurrentEntrysTaskIsDroppedAfterAnAcceptedBehindCompletion(t *testing.T) {
	h, _, _ := newEntryHarness(t)
	holdPhases(h)
	seedIssueAt(t, h, "LEGION-208", phase.Testing)
	seedPullRequest(t, h, "LEGION-208", headSHA)
	seedEntry(t, h, "LEGION-208", claim.RoleTester, 2)
	tester := newLiveClaim(t, h, "LEGION-208", claim.RoleTester)
	takeTask(t, h, tester.token, "outbox:77", "test LEGION-208", 1, 1)
	completion(t, h.complete(HandoffCompleteRequest{GrantID: tester.grant(t), Summary: "tests pass", Verdict: "pass",
		Commit: headSHA, HandoffID: tester.writeHandoff("test", 1), HandoffVersion: 1}))
	assertIssuePhase(t, h, phase.Reviewing)

	// The second testing entry's task reaches the claim, queued for the phase the completion has
	// just taken the issue out of. The claim is given a connection first, since a delivery the
	// daemon has nothing to send over waits for the shim's hello and is never put to the question
	// at all (supervise's sendPending); the harness registers none of its own.
	conn := connected(h, tester.token)
	deliverPhaseTask(t, h, tester.token, "outbox:78", "test LEGION-208 again", phase.Testing, 2, 1)
	settled(h, tester.token)
	machine, ok := h.supervisor.Machine(tester.token)
	if !ok {
		t.Fatal("no machine for the tester")
	}
	if p := machine.Claim().Pending; p != nil {
		t.Fatalf("the tester holds %+v, want entry 2's task dropped: the issue has left the phase it was queued for", p)
	}
	for _, prompt := range conn.Prompts() {
		if prompt.DeliveryID == "outbox:78" {
			t.Fatalf("entry 2's task was sent as %+v, want it dropped rather than worked a second time", prompt)
		}
	}
}

// newEntryHarness serves the daemon's routes with the workflow deciding every fact they apply
// (newWorkflowHarness), configured as a project that checks nothing in production.
func newEntryHarness(t *testing.T) (*harness, *workflow.Engine, *factRecorder) {
	t.Helper()
	return newWorkflowHarness(t, workflow.Config{Project: testProject, Repositories: testRepositories,
		Steps: config.DesignGateRootIssues.Steps(), ReviewRoundCap: 3})
}

// applyEntryFact applies fact to the workflow under id, as the route that carries it would, and
// fails the test when the engine refuses it: every fact these tests apply is one it takes.
func applyEntryFact(t *testing.T, h *harness, engine *workflow.Engine, id string, fact intake.Fact) {
	t.Helper()
	result, err := intake.ApplyFact(context.Background(), h.store.Pool(), "test", id, fact, engine)
	if err != nil || result.Refusal != nil {
		t.Fatalf("apply %s (%T) = %+v, %v; want it taken", id, fact, result.Refusal, err)
	}
}

// rowEntry is the phase entry role's phases row on issue is on now.
func rowEntry(t *testing.T, h *harness, issue string, role claim.Role) int64 {
	t.Helper()
	var entry int64
	if err := h.store.Pool().QueryRow(context.Background(),
		"select entry from phases where issue = $1 and role = $2", issue, string(role)).Scan(&entry); err != nil {
		t.Fatalf("read the %s's entry on %s: %v", role, issue, err)
	}
	return entry
}

// assertIssuePhase asserts where LEGION-208's record stands now.
func assertIssuePhase(t *testing.T, h *harness, want phase.Phase) {
	t.Helper()
	var got phase.Phase
	if err := h.store.Pool().QueryRow(context.Background(), "select phase from issues where key = 'LEGION-208'").Scan(&got); err != nil {
		t.Fatalf("read the phase: %v", err)
	}
	if got != want {
		t.Fatalf("the issue is in %s, want %s", got, want)
	}
}

// seedEntry puts role's phases row on issue at entry, as a transition that moved the issue
// forward into the phase and handed that role its task raised it.
func seedEntry(t *testing.T, h *harness, issue string, role claim.Role, entry int64) {
	t.Helper()
	putEntry(t, h, issue, role, entry, false)
}

// seedMovedBackEntry is seedEntry for an entry a move back to an earlier phase raised: the
// architect's move back or the reviewer's request for changes, which hand the role its phase
// again for work to be done again (record.PhaseRow.EntryMovedBack).
func seedMovedBackEntry(t *testing.T, h *harness, issue string, role claim.Role, entry int64) {
	t.Helper()
	putEntry(t, h, issue, role, entry, true)
}

// putEntry is the write both of them make, which they differ in only by the move they name.
func putEntry(t *testing.T, h *harness, issue string, role claim.Role, entry int64, movedBack bool) {
	t.Helper()
	ctx := context.Background()
	if err := h.store.Tx(ctx, func(tx pgx.Tx) error {
		return record.NewStore().PutPhase(ctx, tx, record.PhaseRow{Issue: issue, Role: role,
			Claim: claim.Token(string(role) + "-claim"), Entry: entry, EntryMovedBack: movedBack})
	}); err != nil {
		t.Fatalf("put the %s's row of %s on entry %d: %v", role, issue, entry, err)
	}
}

// queuedStart is the newest start the outbox holds for role on issue, with its row id: the row the
// outbox runner would deliver next.
// A phase worker's start is one row, which carries the task and its entry and waits for the
// issue's admission slot (workflow.StartOrOwe). A start with no task is a role admission brings
// back beside it and is of no entry, so it is not one of these.
func queuedStart(t *testing.T, h *harness, issue string, role claim.Role) (int64, record.SuperviseRequest) {
	t.Helper()
	var id int64
	var payload []byte
	err := h.store.Pool().QueryRow(context.Background(), `select id, payload from outbox
		where kind = 'supervise' and issue = $1 and payload->>'op' = 'start' and payload->>'role' = $2
			and coalesce(payload->>'task', '') <> ''
		order by id desc limit 1`, issue, string(role)).Scan(&id, &payload)
	if err != nil {
		t.Fatalf("read the %s's queued start on %s: %v", role, issue, err)
	}
	var request record.SuperviseRequest
	if err := json.Unmarshal(payload, &request); err != nil {
		t.Fatalf("decode the %s's start: %v", role, err)
	}
	return id, request
}

// takeStart hands the claim the newest start queued for its role, as the outbox runner does, and
// answers the entry that start is of. The turn the claim is in ends first, which retires the task
// it held: the outbox's delivery waits for that anyway, since a machine refuses a second delivery
// while one is pending.
func takeStart(t *testing.T, h *harness, token claim.Token, issue string, role claim.Role) int64 {
	t.Helper()
	row, start := queuedStart(t, h, issue, role)
	if start.Entry == 0 {
		t.Fatalf("the %s's start on %s is of no entry: %+v", role, issue, start)
	}
	takeTask(t, h, token, fmt.Sprintf("outbox:%d", row), start.Task, start.Entry, start.Generation)
	return start.Entry
}

// takeTask ends whatever turn the claim is in, which retires the task it held, and gives it task
// of entry in a turn of its own: a worker taking the next phase's work.
func takeTask(t *testing.T, h *harness, token claim.Token, id, task string, entry int64, generation uint64) {
	t.Helper()
	queueTask(t, h, token, id, task, entry, generation)
	machine, ok := h.supervisor.Machine(token)
	if !ok {
		t.Fatalf("no machine for %s", token)
	}
	if err := machine.Handle(context.Background(), supervise.StreamTurnStart{Claim: token, DeliveryID: id}); err != nil {
		t.Fatalf("start the turn of %s: %v", token, err)
	}
}

// queueTask is takeTask's delivery without the turn that confirms it: the task waits on the claim
// unacknowledged, as one the outbox sent and the worker has not read, so the claim goes on serving
// the entry its last confirmed task carried (supervise.Claim.ServingEntry). It is deliverPhaseTask
// for a delivery of no phase, which is what every task these tests queue by hand carries.
func queueTask(t *testing.T, h *harness, token claim.Token, id, task string, entry int64, generation uint64) {
	t.Helper()
	deliverPhaseTask(t, h, token, id, task, "", entry, generation)
}

// recordedCompletions is how many completions of role on LEGION-208 are recorded as processed.
func recordedCompletions(t *testing.T, h *harness, role claim.Role) int {
	t.Helper()
	var stored int
	if err := h.store.Pool().QueryRow(context.Background(),
		"select count(*) from processed_events where event_id like $1", "handoff:LEGION-208:%:"+string(role)+":%").Scan(&stored); err != nil {
		t.Fatalf("count the recorded %s completions: %v", role, err)
	}
	return stored
}

// holdPhases wires the supervisor to the issue record the way daemon.go wires
// workflowRuntime.phaseHolds in production, with the predicate that function answers with
// (record.PhaseHolds): a task carries the phase and the run it was queued for, and one the issue
// has left is dropped rather than sent. The harness wires none of its own, so every delivery
// holds until a test asks for this.
//
// It must be called before the test creates its first claim: testSupervisor.Create copies deps
// by value into each machine it makes (supervise.NewMachine), so a machine made before this call
// carries no hook of it at all and every delivery of that machine's holds (supervise's
// phaseHolds), which is a test passing with the rule it is about never asked.
func holdPhases(h *harness) {
	h.supervisor.deps.PhaseHolds = func(ctx context.Context, key string, queuedFor phase.Phase, generation uint64) (bool, error) {
		var issue *record.Issue
		if err := h.store.Tx(ctx, func(tx pgx.Tx) error {
			var err error
			issue, err = record.NewStore().Issue(ctx, tx, key)
			return err
		}); err != nil {
			return false, err
		}
		return record.PhaseHolds(issue, queuedFor, generation), nil
	}
}

// deliverPhaseTask is the delivery both helpers make, with the phase a start of a phase carries:
// naming it is what puts the task to the question a workflow task is sent under (holdPhases).
// The tasks queueTask hands out name none and are never put to it.
func deliverPhaseTask(t *testing.T, h *harness, token claim.Token, id, task string, p phase.Phase, entry int64, generation uint64) {
	t.Helper()
	ctx := context.Background()
	machine, ok := h.supervisor.Machine(token)
	if !ok {
		t.Fatalf("no machine for %s", token)
	}
	if machine.Claim().State == supervise.StateWorking {
		if err := machine.Handle(ctx, supervise.StreamTurnEnd{Claim: token}); err != nil {
			t.Fatalf("end the turn of %s: %v", token, err)
		}
	}
	if err := machine.Handle(ctx, supervise.RequestDeliver{Claim: token, Task: task, ID: id,
		Phase: p, Generation: generation, Entry: entry}); err != nil {
		t.Fatalf("deliver %s to %s: %v", id, token, err)
	}
}
