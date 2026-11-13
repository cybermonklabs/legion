//go:build e2e

// The live proof's driver for the completion route's entry fence on a completion behind its phase
// entry: a completion of an entry the issue has moved past is taken when the commit it names is the
// head the daemon's record holds, because a verdict is a fact about a head and the phase was
// entered again at that same head (api/handoff.go's behindEntryClause), and is refused when it
// names any other commit or none, or when the move that entered the phase again moved the issue
// back. Such a move sends the work back at the head that is already there, so the two heads read
// alike and the heads alone would take a verdict over an entry that exists to have the work done
// again. The record says which move it was (record.PhaseRow.EntryMovedBack, written by the
// transition that raises the entry), and the fence reads it. A behind completion from a role that
// no longer works the phase the issue stands in is neither taken nor refused that way: the fence
// answers it HANDOFF_NOT_CURRENT_PHASE, in the engine's words (intake.NotCurrentPhase), rather
// than tell the worker a task is owed to it that the daemon drops unsent, since the issue has left
// the phase that task was queued for.
//
// So every completion the proof drives is of the tester's first entry, reported from a turn that
// began before any later entry's task reached the tester, and each reading has its control beside
// it. Steps 3, 4 and 5 are the same behind completion behind the same forward entry, naming the
// head the tester tested, which the record has left, then no commit at all, then the head the
// record holds: only the commit differs, and only the last is taken. Steps 5 and 8 are the same
// completion at the same head, which never moves between them: only what raised the entry
// differs, a forward move for step 5 and the registered reviewer's real move back for step 8, and
// only step 5 is taken. Step 6 is what follows a taken one: step 5's completion moved the issue to
// reviewing and left the tester's claim serving the entry it was behind on, so the same completion
// sent again, and one naming a newer record of the phase, each meet the fence in a phase the
// tester no longer works. scripts/e2e/behind-entry-fence.sh builds this driver with `-tags e2e`
// and owns the nats-server and the PostgreSQL it runs against.
//
// The forward entry of step 2 is raised on the path the daemon has for it: the tester asks for the
// work back with its own backward move to implementing and ends its turn, and the implementer
// pushes a head of its own and completes, which hands testing out again. The tester's first turn
// cannot still be running then, because the daemon sends no role a task that adopts the issue's
// working copy while another role of the issue is in a turn (supervise's sendPending), so the
// implementer's task waits for that turn to end; the tester's completions come instead from a
// turn of its own, begun while the implementer works and so before the new entry's task could
// reach it. Neither of the moves the daemon makes on its own serves here. A CI red moves no issue,
// since the daemon judges no check, so the proof publishes no settlement; and the daemon reads a
// head's mergeability at its start, after a handoff, and for an approved review round in reviewing
// alone, so a head that starts conflicting with its base while the tester tests is not read until
// the next handoff (dispatch://INFRA-3164). The move back into testing is the reviewer's own
// request over POST /legion/v1/phase/backward, from reviewing, under a grant its registered
// session minted, because the engine takes a backward move from no role but the one running the
// issue's phase (workflow's Engine.backward, BACKWARD_REFUSED).
//
// The daemon is the whole daemon: run, the function `legion start` calls, through this package's
// own whole-daemon harness (startDaemonWithin), so the proof exercises the real configuration, the
// real store and its migrations, the real HTTP API, the real claim supervision and its worker
// stream, the real openWorkflow, the real JetStream consumers and the real outbox lanes. Facts
// enter only as production's do: GitHub envelopes on the ENVOY_NOTIFICATIONS stream for the
// implementer's push and its synchronize; the daemon's own HTTP routes for each role's
// registration, readiness, grants, completions and backward moves; and the daemon's worker stream
// for each role's turns, which this driver speaks as `legion worker-shim` does (shimwire frames).
// Almost every assertion is a read of GET /legion/v1/state or of the operator's claim listing.
// What neither shows is what the proof reads from the daemon's own store: the tester's phases row,
// for the fence's own input, whether the entry the row stands on was raised by a move back, and
// for what a refusal must leave unwritten there, the verdict and the handoff commit of the
// completion it refused, or, once step 5's completion stands on that row, that completion
// unchanged; and in step 6 the tester's completions the daemon recorded as processed
// (processed_events), which a refusal before the fact adds none to.
//
// What the proof stands in for lies outside the rule. The broker is a native nats-server the
// script starts and names in LEGION_E2E_NATS_URL. GitHub is the harness's REST stand-in for the
// one pull request the proof drives (liveGitHub, in live_proof_test.go), at the head the
// implementer last pushed. Dispatch is the board the package's whole-daemon live proofs share
// (liveDispatch, in live_proof_test.go), which holds the handoff record each completion names, and
// Envoy's listener is that harness's noticeListener. The two Apps' tokens are
// workflowTokenRecorder's placeholders. No agent runs: every claim is launched by the package's
// fake runtime, which says it provisions each workspace in the claim's own pod, as the Agent
// Sandbox runtime does, so the daemon clones nothing from GitHub, and each role the proof acts as
// is driven over the daemon's own worker stream and HTTP routes. The issue, its pull request, its
// slot and the implementer's claim are seeded through the store, as every daemon test seeds its
// fixture; what is driven is the change.
package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// behindFenceBase is the pull request's base branch, the repository's default.
const behindFenceBase = "main"

// The two heads the proof drives: the implementer's seeded head, which the tester's first turn
// tests, and the head the implementer pushes once the tester has sent the work back, which the
// record holds from step 2 to the end. Nothing is pushed after it, so steps 5 and 8 differ in
// nothing a reading of the heads can see.
const (
	behindFenceFirstHead  = "bf1"
	behindFenceSecondHead = "bf2"
)

// behindFenceTakenSummary is the summary of the behind completion step 5 takes, which step 6
// sends again word for word.
const behindFenceTakenSummary = "tested it at " + behindFenceSecondHead

// behindEntryFenceLive is one run of the proof: the booted daemon, the one issue it drives, the
// GitHub stand-in its facts are read against, and the three roles the driver acts as over the
// worker stream.
type behindEntryFenceLive struct {
	liveIssueProof
	github *liveGitHub
	// imp, test and review are the implementer, the tester and the reviewer as their agents reach
	// the daemon. The reviewer is registered and holds a grant, because the move back is its own.
	imp, test, review *liveClaimant
	// served is the entry whose task the tester's one turn took, and entry the entry the tester's
	// phases row stands on as the last step left it, so the step after it reads what the one
	// before recorded rather than a number of its own.
	served, entry int64
	// delivery is the delivery of the served entry's task, the last task the tester's turn took.
	delivery string
	// taken is the handoff record the behind completion step 5 took named, which step 6 sends again.
	taken string
}

// TestABehindCompletionIsDecidedByItsHeadAndTheMoveThatRaisedTheEntryLive drives the eight steps
// of the proof against one booted daemon, in the order each step's state follows from the last: the
// tester working its first entry, the tester's own move back and the implementer's push and
// completion that hand testing out again by a forward move, the behind completion refused at the
// head the record has left, refused naming no commit, and taken at the head the record holds, that
// completion sent again and one naming a newer record, each answered as of a phase the tester no
// longer works, the registered reviewer's own move back into testing, and the same completion at
// the same head refused because a move back raised the entry the phase now stands on.
func TestABehindCompletionIsDecidedByItsHeadAndTheMoveThatRaisedTheEntryLive(t *testing.T) {
	r := newBehindEntryFenceLive(t)
	r.step("the tester works its entry", r.testerWorksItsEntry)
	r.step("testing is entered again by a forward move", r.testingIsEnteredAgain)
	r.step("stale head refused", r.staleHeadRefused)
	r.step("behind with no head", r.behindWithNoHead)
	r.step("behind at the head", r.behindAtTheHead)
	r.step("after a taken behind completion", r.afterATakenBehindCompletion)
	r.step("the reviewer moves the work back", r.reviewerMovesTheWorkBack)
	r.step("behind after a backward move", r.behindAfterABackwardMove)
}

// newBehindEntryFenceLive boots the daemon over the script's broker with the issue already
// recorded: one root implementing on an open pull request, holding its tree's admission slot, and
// its implementer's claim as a launch a previous daemon began and never recorded, which this boot
// relaunches. That last one is how the proof reaches a live implementer session, serving run 1,
// without an operator's spawn.
func newBehindEntryFenceLive(t *testing.T) *behindEntryFenceLive {
	t.Helper()
	r := &behindEntryFenceLive{liveIssueProof: newLiveIssueProof(t, "scripts/e2e/behind-entry-fence.sh",
		behindFenceBase, "the issue whose tester reports behind its entry")}
	r.github = r.serveGitHub(&liveGitHub{sha: behindFenceFirstHead})
	r.seed()
	r.boot(r.github.url, nil)
	return r
}

// seed records the issue as seedImplementing does, on an open pull request at the first head. No
// phases row is seeded: every entry the fence reads here is raised live, by the starts the
// completions and the moves back make.
func (r *behindEntryFenceLive) seed() {
	r.t.Helper()
	r.seedImplementing(behindFenceFirstHead)
	r.note("seeded: %s implementing on pull request #%d at %s, base %s, its implementer mid-launch serving run 1",
		r.key, r.number, behindFenceFirstHead, behindFenceBase)
}

// moveBack is this role requesting the issue's move back to an earlier phase, from the turn it is
// in and under a grant of its own, as the `legion` tool's request_backward_move does. The engine
// takes it from the role running the issue's phase and from no other (workflow's Engine.backward,
// BACKWARD_REFUSED), which is why the tester asks for it in testing and the registered reviewer in
// reviewing.
func (c *liveSession) moveBack(to phase.Phase, reason string) (int, api.Failure) {
	t := c.p.t
	t.Helper()
	status, body := c.p.d.request(http.MethodPost, "/legion/v1/phase/backward",
		api.PhaseBackwardRequest{GrantID: c.grant(), To: to, Reason: reason}, false)
	if status == http.StatusOK {
		return status, api.Failure{}
	}
	var failure api.Failure
	if err := json.Unmarshal(body, &failure); err != nil {
		t.Fatalf("decode the %s's refused backward move %s: %v", c.role, body, err)
	}
	return status, failure
}

// Step 1. The seeded implementer completes implementing at the first head, which hands the testing
// phase to the tester with an entry of its own and starts it. The tester registers, takes that
// task and works it: the entry every completion below is of.
func (r *behindEntryFenceLive) testerWorksItsEntry() {
	t := r.t
	r.imp = r.claimant(claim.RoleImplementer, "ses_implementer")
	r.note("the implementer is registered and ready on the launch this boot made for the claim the fixture left mid-launch")
	r.imp.startTurn("")
	if status, failure := r.imp.complete("implemented it", behindFenceFirstHead, "", r.imp.handoff("implement")); status != http.StatusOK {
		t.Fatalf("the implementer's completion = %d %s %s, want 200", status, failure.Code, failure.Error)
	}
	r.imp.endTurn()
	r.note("drove POST /legion/v1/handoff/complete for the implementer at %s, then its agent_end: 200", behindFenceFirstHead)
	got := r.waitFor("the issue in testing", func(got api.Issue) bool { return got.Phase == phase.Testing })

	r.test = r.claimant(claim.RoleTester, "ses_tester")
	prompt := r.test.takeTask("Phase: testing")
	working := r.d.claim(r.test.token)
	if working.State != string(supervise.StateWorking) || working.Pending == nil || working.Pending.ConfirmedAt == nil {
		t.Fatalf("the tester is %s holding %+v, want working on a confirmed delivery of its testing task", working.State, working.Pending)
	}
	r.served, r.entry, r.delivery = got.PhaseEntry, got.PhaseEntry, working.Pending.ID
	r.note("read back: %s is in %s on entry %d at head %s, and the tester is %s on the delivery %s of that entry's task (%s)",
		r.key, got.Phase, r.entry, got.PullRequest.Head, working.State, r.delivery, firstLine(prompt.Message))
}

// Step 2. The tester asks for the work back with its own backward move to implementing, from the
// turn it is in, and ends that turn: the daemon sends no role a task that adopts the issue's
// working copy while another role of the issue is in a turn (supervise's sendPending), so the
// implementer's task waits for it. The implementer is prompted, and while it works the tester
// begins a turn of its own, as an agent that goes on working does, before any task of a later
// entry has reached it. The implementer pushes a head of its own and completes there, which moves
// the issue forward into testing and raises the tester's second entry, at the head the implementer
// pushed and by no move back. That entry's task waits on the claim, since the tester is in a turn,
// and the claim still serves the entry before it: every completion of steps 3 to 8 is reported
// from a turn that began before its entry's task reached the session.
func (r *behindEntryFenceLive) testingIsEnteredAgain() {
	t := r.t
	const reason = "the tests show a fault in the code; back to implementing"
	if status, failure := r.test.moveBack(phase.Implementing, reason); status != http.StatusOK {
		t.Fatalf("the tester's backward move to implementing = %d %s %s, want 200", status, failure.Code, failure.Error)
	}
	r.note("drove POST /legion/v1/phase/backward for the tester, to implementing, from the turn it is in: 200")
	r.test.endTurn()
	r.note("the tester's turn on entry %d's task ended", r.served)
	fix := r.imp.takeTask("Phase: implementing")
	r.note("read back: the implementer was prompted its implementing task once the tester's turn ended (%s)", firstLine(fix.Message))
	r.test.startTurn("")
	r.note("the tester began a turn of its own while the implementer works")
	r.pushes(behindFenceFirstHead, behindFenceSecondHead)
	if status, failure := r.imp.complete("fixed the fault the tests showed", behindFenceSecondHead, "", r.imp.handoff("implement")); status != http.StatusOK {
		t.Fatalf("the implementer's second completion = %d %s %s, want 200", status, failure.Code, failure.Error)
	}
	r.imp.endTurn()
	r.note("drove POST /legion/v1/handoff/complete for the implementer at %s, then its agent_end: 200", behindFenceSecondHead)

	got := r.waitFor("the tester's row on a second entry", func(got api.Issue) bool {
		return got.Phase == phase.Testing && got.PhaseEntry > r.entry
	})
	if got.PullRequest == nil || got.PullRequest.Head != behindFenceSecondHead {
		t.Fatalf("the record holds %+v for %s, want the pull request at %s, the head the implementer pushed", got.PullRequest, r.key, behindFenceSecondHead)
	}
	if row := rolePhaseRow(t, r.st.Pool(), r.key, claim.RoleTester); row.Entry != got.PhaseEntry || row.EntryMovedBack {
		t.Fatalf("the tester's phases row = entry %d, moved back %t; want entry %d raised by no move back: implementing moves forward into testing",
			row.Entry, row.EntryMovedBack, got.PhaseEntry)
	}
	r.entry = got.PhaseEntry
	r.heldOn()
	r.note("read back: %s is in testing on entry %d, raised by no move back (phases.entry_moved_back false), with the record at head %s",
		r.key, r.entry, got.PullRequest.Head)
}

// Step 3. A behind completion at a head the record has left is refused. The tester reports from
// its own turn its verdict on the head its entry tested, which is the head before the
// implementer's push: the daemon refuses it 409 HANDOFF_STALE_ENTRY, the refusal names that head
// and the head the record holds, nothing is recorded, and the issue stays in testing.
func (r *behindEntryFenceLive) staleHeadRefused() {
	r.refused(behindFenceFirstHead,
		"this completion is at commit "+behindFenceFirstHead+" while the pull request the record holds is at head "+behindFenceSecondHead)
}

// Step 4. A behind completion naming no commit is refused. The same completion from the same turn
// names no commit at all, so it says nothing about a head and nothing makes it the work the current
// entry's task asks for: the daemon refuses it 409 HANDOFF_STALE_ENTRY, saying it named no commit
// and what head the record holds, and nothing is recorded.
func (r *behindEntryFenceLive) behindWithNoHead() {
	r.refused("", "this completion names no commit while the pull request the record holds is at head "+behindFenceSecondHead)
}

// Step 5. A behind completion at the head the record holds is taken. The same completion from the
// same turn names the head the record holds. Nothing moved the issue back into testing, so the work
// the standing entry's task asks for is the work this turn did: the daemon takes the completion
// (behindEntryClause) and the pass moves the issue to reviewing. Steps 3 and 4 are this same
// completion with another commit; step 6 sends it again once it is taken; step 8 is this same
// completion at this same head with one thing changed, the move that raised the entry.
func (r *behindEntryFenceLive) behindAtTheHead() {
	t := r.t
	r.taken = r.test.handoff("test")
	if status, failure := r.test.complete(behindFenceTakenSummary, behindFenceSecondHead, "pass", r.taken); status != http.StatusOK {
		t.Fatalf("the behind completion at the head the record holds = %d %s %s, want 200", status, failure.Code, failure.Error)
	}
	moved := r.waitFor("the issue in reviewing", func(got api.Issue) bool { return got.Phase == phase.Reviewing })
	r.note("read back: the completion of entry %d at %s, the head the record holds, naming record %s and reported while the row stands on entry %d, was taken 200: %s is in %s",
		r.served, behindFenceSecondHead, r.taken, r.entry, r.key, moved.Phase)
	r.heldOn()
}

// Step 6. A completion after a taken behind completion is answered as of a phase the tester no
// longer works. Step 5's completion moved the issue to reviewing, as a taken completion always
// moves its phase on, and left the tester's claim serving the entry it was behind on, so every
// completion that claim sends from here meets the entry fence in the reviewer's phase. From the
// same turn the tester sends step 5's completion again, word for word under a grant of its own,
// as a retry after a dropped connection sends it, and then one naming a newer record of the
// phase. The daemon answers each 409 HANDOFF_NOT_CURRENT_PHASE, the code every worker's prompt
// names for a completion of a phase it no longer works, in the engine's words: never
// HANDOFF_STALE_ENTRY, and with no words telling the worker a task is owed to it, since the second
// testing entry's task is queued for testing, which the issue has left, and is dropped rather than
// sent (supervise's phaseHolds). Each records nothing — the tester's phases row still holds step
// 5's completion, and the daemon has processed no completion of the tester's beyond it — and the
// issue stays in reviewing.
func (r *behindEntryFenceLive) afterATakenBehindCompletion() {
	t := r.t
	row := rolePhaseRow(t, r.st.Pool(), r.key, claim.RoleTester)
	if row.HandoffRecord != r.taken || row.Verdict != "pass" || row.HandoffCommit != behindFenceSecondHead || row.Summary != behindFenceTakenSummary {
		t.Fatalf("the tester's phases row holds record %q, verdict %q at commit %q (%q), want step 5's completion: record %s, pass at %s (%q)",
			row.HandoffRecord, row.Verdict, row.HandoffCommit, row.Summary, r.taken, behindFenceSecondHead, behindFenceTakenSummary)
	}
	r.note("read back: the tester's phases row holds step 5's completion, record %s, %s at %s, while it stands on entry %d and the claim serves entry %d",
		row.HandoffRecord, row.Verdict, row.HandoffCommit, row.Entry, r.served)
	r.notCurrentPhase(row, "step 5's completion sent again", behindFenceTakenSummary, r.taken)
	r.notCurrentPhase(row, "a completion naming a newer test record", behindFenceTakenSummary+", record corrected", r.test.handoff("test"))
	r.heldOn()
}

// Step 7. The reviewer's own move back. Its claim was started by the tester's pass, and its task
// waits while the tester's turn runs, so that turn ends first; the reviewer then registers, takes
// its review task, and the tester begins another turn of its own before the move, which no task
// of its has reached. The reviewer requests the move to testing from the turn its task began, with
// a grant its session minted. The engine takes a backward move from the role running the issue's
// phase alone, so this is the one way the proof can reach a move back into testing. Nothing is
// pushed with it: the head the record holds is the one the tester reported on in step 5, and the
// entry the move raises says it was raised by a move back.
func (r *behindEntryFenceLive) reviewerMovesTheWorkBack() {
	t := r.t
	r.test.endTurn()
	r.note("the tester's own turn ended")
	r.review = r.claimant(claim.RoleReviewer, "ses_reviewer")
	prompt := r.review.takeTask("Phase: reviewing")
	r.note("the reviewer is registered, ready and working the review task the tester's pass started it with (%s)", firstLine(prompt.Message))
	r.test.startTurn("")
	r.note("the tester began another turn of its own while the reviewer reviews")
	const reason = "the fix needs a test of its own; back to testing"
	if status, failure := r.review.moveBack(phase.Testing, reason); status != http.StatusOK {
		t.Fatalf("the reviewer's backward move to testing = %d %s %s, want 200", status, failure.Code, failure.Error)
	}
	r.note("drove POST /legion/v1/phase/backward for the reviewer, to testing: 200")

	got := r.waitFor("the issue back in testing on a third entry", func(got api.Issue) bool {
		return got.Phase == phase.Testing && got.PhaseEntry > r.entry
	})
	if got.PullRequest == nil || got.PullRequest.Head != behindFenceSecondHead {
		t.Fatalf("the record holds %+v for %s, want the pull request still at %s: nothing was pushed with the move back", got.PullRequest, r.key, behindFenceSecondHead)
	}
	row := rolePhaseRow(t, r.st.Pool(), r.key, claim.RoleTester)
	if row.Entry != got.PhaseEntry || !row.EntryMovedBack {
		t.Fatalf("the tester's phases row = entry %d, moved back %t; want entry %d raised by a move back: reviewing came back to testing",
			row.Entry, row.EntryMovedBack, got.PhaseEntry)
	}
	r.entry = got.PhaseEntry
	r.heldOn()
	r.note("read back: %s is in testing on entry %d, raised by a move back (phases.entry_moved_back true), with the record still at head %s — the head the completion step 5 took was of",
		r.key, row.Entry, got.PullRequest.Head)
}

// Step 8. A behind completion is refused whatever head it names when a move back raised the entry.
// The tester reports step 5's completion again from its own turn, at the same head, which is still
// the head the record holds — and this time the entry the phase stands on was raised by a move
// back, an entry that exists to have this phase's work done again. The daemon refuses it 409
// HANDOFF_STALE_ENTRY, the refusal says the move back is what refused it however the two heads
// read, nothing is recorded, and the issue stays in testing.
func (r *behindEntryFenceLive) behindAfterABackwardMove() {
	r.refused(behindFenceSecondHead,
		"this completion is at commit "+behindFenceSecondHead+" while the pull request the record holds is at head "+behindFenceSecondHead,
		"the move that handed the phase out again moved the issue back, which asks for this phase's work to be done again, so no verdict on either head answers for it")
}

// refused is the tester's behind completion at commit, from the turn of its first entry, refused:
// 409 HANDOFF_STALE_ENTRY, its text naming the entries, saying each of says and that nothing was
// recorded; the issue still in testing on the entry it stood on; and the tester's phases row
// holding no verdict and no handoff commit, since a refusal before the fact records nothing.
func (r *behindEntryFenceLive) refused(commit string, says ...string) {
	t := r.t
	t.Helper()
	status, failure := r.test.complete("tested it", commit, "pass", r.test.handoff("test"))
	if status != http.StatusConflict || failure.Code != "HANDOFF_STALE_ENTRY" {
		t.Fatalf("the behind completion at commit %q = %d %s %s, want 409 HANDOFF_STALE_ENTRY", commit, status, failure.Code, failure.Error)
	}
	behind := fmt.Sprintf("handed the tester phase testing again: this completion is of entry %d, which is %d behind the entry %d the phase is on",
		r.served, r.entry-r.served, r.entry)
	for _, want := range append([]string{behind, "nothing was recorded"}, says...) {
		if !strings.Contains(failure.Error, want) {
			t.Fatalf("the refusal = %q, want it to say %q", failure.Error, want)
		}
	}
	r.note("read back: the behind completion at commit %q = 409 %s — %s", commit, failure.Code, failure.Error)
	after := r.issue(r.key)
	if after.Phase != phase.Testing || after.PhaseEntry != r.entry {
		t.Fatalf("%s is in %s on entry %d after the refused completion, want testing on entry %d: a refusal records nothing",
			r.key, after.Phase, after.PhaseEntry, r.entry)
	}
	row := rolePhaseRow(t, r.st.Pool(), r.key, claim.RoleTester)
	if row.Verdict != "" || row.HandoffCommit != "" {
		t.Fatalf("the tester's phases row holds verdict %q at commit %q after the refusal, want neither: the refusal is before the fact and records nothing",
			row.Verdict, row.HandoffCommit)
	}
	r.note("read back: %s is still in testing on entry %d, and the tester's phases row holds no verdict and no commit of this completion — nothing was recorded",
		r.key, after.PhaseEntry)
}

// notCurrentPhase is the tester's completion at the head the record holds, naming handoffID and
// reported from its own turn while the issue is in reviewing, answered as of a phase the tester no
// longer works: 409 HANDOFF_NOT_CURRENT_PHASE in the engine's words, naming no task as owed; the
// issue still in reviewing; the tester's phases row still holding taken, step 5's completion,
// field for field; and as many of the tester's completions processed as before, since an answer
// before the fact records nothing. The engine's own refusal would be processed: it commits with
// its event (intake.ApplyFact).
func (r *behindEntryFenceLive) notCurrentPhase(taken record.PhaseRow, what, summary, handoffID string) {
	t := r.t
	t.Helper()
	processed := r.testerCompletionsProcessed()
	status, failure := r.test.complete(summary, behindFenceSecondHead, "pass", handoffID)
	if status != http.StatusConflict || failure.Code != "HANDOFF_NOT_CURRENT_PHASE" {
		t.Fatalf("%s, naming record %s = %d %s %s, want 409 HANDOFF_NOT_CURRENT_PHASE: step 5's completion moved %s to reviewing, a phase the tester does not work",
			what, handoffID, status, failure.Code, failure.Error, r.key)
	}
	words := fmt.Sprintf("the tester does not run phase reviewing of %s; this completion changed nothing", r.key)
	if !strings.Contains(failure.Error, words) {
		t.Fatalf("the answer = %q, want it to say %q, the engine's words for HANDOFF_NOT_CURRENT_PHASE", failure.Error, words)
	}
	if strings.Contains(failure.Error, "owed to this claim") {
		t.Fatalf("the answer = %q, want no task named as owed: the testing entry's task was dropped when the issue left testing", failure.Error)
	}
	r.note("read back: %s, naming record %s at %s = 409 %s — %s", what, handoffID, behindFenceSecondHead, failure.Code, failure.Error)
	if after := r.issue(r.key); after.Phase != phase.Reviewing {
		t.Fatalf("%s is in %s after the answer, want reviewing: an answer before the fact moves nothing", r.key, after.Phase)
	}
	row := rolePhaseRow(t, r.st.Pool(), r.key, claim.RoleTester)
	if row.HandoffRecord != taken.HandoffRecord || row.HandoffVersion != taken.HandoffVersion || row.HandoffCommit != taken.HandoffCommit ||
		row.Verdict != taken.Verdict || row.Summary != taken.Summary || !row.CompletedAt.Equal(taken.CompletedAt) || row.Entry != taken.Entry {
		t.Fatalf("the tester's phases row after the answer = record %s v%d, %q at %q (%q), completed %s, entry %d; want step 5's completion unchanged: record %s v%d, %q at %q (%q), completed %s, entry %d",
			row.HandoffRecord, row.HandoffVersion, row.Verdict, row.HandoffCommit, row.Summary, row.CompletedAt, row.Entry,
			taken.HandoffRecord, taken.HandoffVersion, taken.Verdict, taken.HandoffCommit, taken.Summary, taken.CompletedAt, taken.Entry)
	}
	if got := r.testerCompletionsProcessed(); got != processed {
		t.Fatalf("the daemon holds %d of the tester's completions processed after the answer, want the %d before it: an answer before the fact records nothing", got, processed)
	}
	r.note("read back: %s is still in reviewing, the tester's phases row still holds step 5's completion (record %s, %s at %s), and the daemon holds %d tester completions processed, as before it — nothing was recorded",
		r.key, row.HandoffRecord, row.Verdict, row.HandoffCommit, processed)
}

// testerCompletionsProcessed is how many of the tester's completions of the proof's issue the
// daemon holds as processed: processed_events rows under the id the completion route mints for a
// completion it applies (handoff:<issue>:…:tester:…, api/handoff.go's eventID).
func (r *behindEntryFenceLive) testerCompletionsProcessed() int {
	r.t.Helper()
	var processed int
	if err := r.st.Pool().QueryRow(context.Background(),
		"select count(*) from processed_events where source = 'api' and event_id like $1", "handoff:"+r.key+":%:tester:%").Scan(&processed); err != nil {
		r.t.Fatalf("count the tester's completions of %s the daemon processed: %v", r.key, err)
	}
	return processed
}

// heldOn holds that the tester is in a turn while the phases row stands on a later entry than the
// one whose task the tester took in step 1, and that no task of a later entry has reached it: the
// task the claim holds, if any, is unconfirmed and undelivered, so the claim still serves the
// first entry. That is the state every completion of steps 3 to 8 is reported from, and it says so.
func (r *behindEntryFenceLive) heldOn() {
	t := r.t
	t.Helper()
	held := r.d.claim(r.test.token)
	if held.State != string(supervise.StateWorking) || (held.Pending != nil && (held.Pending.ConfirmedAt != nil || held.Pending.DeliveredAt != nil)) {
		t.Fatalf("the tester is %s holding %+v, want it in a turn of its own with no later task delivered: its claim serves entry %d",
			held.State, held.Pending, r.served)
	}
	waiting := "no task"
	if held.Pending != nil {
		waiting = "the task " + held.Pending.ID + ", undelivered,"
	}
	r.note("read back: the tester is %s in a turn of its own holding %s while its row stands on entry %d; the last task it took is %s, of entry %d",
		held.State, waiting, r.entry, r.delivery, r.served)
}

// pushes is the implementer's push of to onto from: GitHub's own copy of the pull request moves
// first, as it does on github.com, then the push and the synchronize Envoy relays, and it returns
// once the head the daemon's record holds is the pushed one.
func (r *behindEntryFenceLive) pushes(from, to string) {
	r.t.Helper()
	r.github.head(to)
	r.publishPush(from, to, "fix the fault the tests showed")
	r.publishSynchronize(to)
	r.note("drove the implementer's push of %s onto %s and GitHub's synchronize of #%d to it", to, from, r.number)
	r.waitFor("the pull request at the pushed head "+to, func(got api.Issue) bool {
		return got.PullRequest != nil && got.PullRequest.Head == to
	})
}
