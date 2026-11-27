//go:build e2e

// The live proof's driver for the rule a head that starts conflicting with its base follows
// (dispatch://INFRA-4374): the issue keeps its phase, and the daemon takes nobody's turn over. A
// first read that finds the head CONFLICTING acts on nothing, since GitHub computes mergeability in
// a background job and can answer about a base it has not caught up with: the read is logged, its
// second read is queued a minute on, and nothing is sent to any role or to its connection. The
// second read confirms the conflict, and the daemon acts on it only because the issue's next step
// needs a head that merges — here a check the base requires has not passed on the head, which
// GitHub runs no checks on while it conflicts. The issue stays in testing: the implementer is
// asked, on its own Envoy role topic, to merge the base forward, the tester is told on its own to
// hold while that runs, the architect is told once in a checks-red notice naming testing, and the
// tester's connection, in the turn its own assignment began, is sent nothing at all. Once the
// implementer's push lands, the tester is told so; a completion it reports at the head the merge
// replaced is refused HANDOFF_HEAD_MERGED_FORWARD and records nothing, and one at the new head is
// taken. The control is a second issue on the same base whose required check passed on its
// conflicting head: its second read confirms the conflict too, and gets it nothing.
// scripts/e2e/conflict-no-takeover.sh builds this driver with `-tags e2e` and owns the nats-server
// and the PostgreSQL it runs against.
//
// The daemon is the whole daemon: run, the function `legion start` calls, booted as this package's
// other whole-daemon proofs boot it (startDaemonWithin), so the proof exercises the real
// configuration, the real store and its migrations, the real HTTP API, the real claim supervision
// and its worker stream, the real openWorkflow, the real JetStream consumers, the real merge-state
// reads and the real outbox lanes — the lane that runs the second read (record.ConflictConfirm)
// and the one that publishes the merge forward's messages (record.MergeForwardMessage) among
// them. Facts enter only as production's do: GitHub envelopes on the ENVOY_NOTIFICATIONS stream
// for the push onto the base, the implementer's push and its synchronize; the daemon's own HTTP
// routes for each role's registration, readiness, grants and completions; and the daemon's worker
// stream for each role's turns, which this driver speaks as `legion worker-shim` does (shimwire
// frames). Every assertion is a read of GET /legion/v1/state, of the operator's claim listing, of
// what the daemon published to the Envoy listener stand-in, of what it sent — or did not send — to
// a role's own connection, of what the fake runtime was asked to suspend, or of the lines the
// daemon logged for its decisions (recordingLog). The store is read only for what no route shows:
// the outbox rows of the second read and of the merge forward, and the tester's phases row, which a
// refused completion must leave unwritten.
//
// The RPC carries no abort frame, and the daemon has no quiesce of a claim and no abort of a
// connection, so the proof holds the stronger fact that leaves — that the tester's connection is
// sent nothing at all while the daemon reads the conflict and acts on it, and that the tester's
// claim stays `working` on the delivery it confirmed throughout — rather than the absence of one
// frame type.
//
// What the proof stands in for lies outside the rule. The broker is a native nats-server the
// script starts and names in LEGION_E2E_NATS_URL. GitHub is the harness's REST stand-in for the
// issue's pull request (liveGitHub, in live_issue_proof_test.go), which answers the head
// conflicting from the moment the driver says so, with what conflictGitHub adds: the base branch's
// rules, which the daemon reads for a conflicting head, and the control's pull request. The minute
// between the two reads is the daemon's own (workflow's conflictConfirmWait); no seam a test may
// set shortens it, so step 3 waits it out. Dispatch is the board the package's whole-daemon live
// proofs share (liveDispatch, in live_proof_test.go), which holds the handoff record each
// completion names, and Envoy's listener is that harness's noticeListener. The two Apps' tokens are
// workflowTokenRecorder's placeholders. No agent runs: every claim is launched by the package's
// fake runtime, which says it provisions each workspace in the claim's own pod, as the Agent
// Sandbox runtime does, so the daemon clones nothing from GitHub, and each role the proof acts as
// is driven over the daemon's own worker stream and HTTP routes. The two issues, their pull
// requests, their slots and the implementer's claim are seeded through the store, as every daemon
// test seeds its fixture; what is driven is the change.
package daemon

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

// The heads the proof drives: the issue's seeded head, which the push onto the base leaves
// conflicting; the head the implementer's merge forward pushes; and the control's head, which
// GitHub answers conflicting from the start and on which ci passed.
const (
	conflictFirstHead   = "h1"
	conflictMergedHead  = "h2"
	conflictControlHead = "c1"
)

// conflictNoTakeoverLive is one run of the proof: the booted daemon, its log and the baseline of
// each role topic (conflictLive), the issue it drives and the control beside it, and the two roles
// the driver acts as over the worker stream.
type conflictNoTakeoverLive struct {
	conflictLive
	// imp and test are the implementer and the tester as their agents reach the daemon, and
	// delivery the delivery of the tester's testing task, whose turn runs from step 1 to step 4.
	imp, test *liveClaimant
	delivery  string
	// control is the second issue, whose required check passed on its conflicting head, and
	// controlNumber its pull request's number.
	control       string
	controlNumber int
	// suspends is how many suspensions the fake runtime had been asked for when step 2 began, which
	// sets the role topics' baseline (heard) too, so the steps after it read what the conflict's
	// reads sent apart from anything before them.
	suspends int
}

// TestAConflictKeepsThePhaseAndTakesNoTurnOverLive drives the five steps of the proof against one
// booted daemon, in the order each step's state follows from the last: the tester working the
// assignment its phase handed it, the first conflicting read that waits for a second and sends
// nothing, the second read a minute on that asks for the merge forward while the issue stays in
// testing and the tester's turn runs on, the merge landing with the tester's completion at the head
// it replaced refused and its completion at the new head taken, and last the control, a conflict
// whose required check passed, which the second read gets nothing.
func TestAConflictKeepsThePhaseAndTakesNoTurnOverLive(t *testing.T) {
	// Not parallel: a live proof on the one NATS server LEGION_E2E_NATS_URL names: it creates the stream
	// ENVOY_NOTIFICATIONS there and writes the file LEGION_E2E_DAEMON_LOG names, which a proof beside it
	// would share, and its steps watch liveSettle's 3 s for what must not happen.
	r := newConflictNoTakeoverLive(t)
	r.step("step1-the-tester-works-the-assignment-its-phase-handed-it", r.testerWorksItsAssignment)
	r.step("step2-the-first-conflicting-read-waits-for-a-second-and-sends-nothing", r.firstReadSendsNothing)
	r.step("step3-the-second-read-asks-for-the-merge-forward-and-the-issue-stays-in-testing", r.secondReadAsksTheMergeForward)
	r.step("step4-the-merge-lands-and-the-completion-at-the-replaced-head-is-refused", r.mergeLands)
	r.step("step5-a-conflict-whose-required-check-passed-gets-nothing", r.passedControlGetsNothing)
}

// newConflictNoTakeoverLive boots the daemon over the script's broker with the two issues already
// recorded (seed), and its implementer's claim as a launch a previous daemon began and never
// recorded, which this boot relaunches. That last one is how the proof reaches a live implementer
// session, serving run 1, without an operator's spawn.
func newConflictNoTakeoverLive(t *testing.T) *conflictNoTakeoverLive {
	t.Helper()
	r := &conflictNoTakeoverLive{conflictLive: conflictLive{liveIssueProof: newLiveIssueProof(t, "scripts/e2e/conflict-no-takeover.sh",
		conflictBase, "the issue whose head starts conflicting with its base")}}
	r.control, r.controlNumber = r.cfg.Project+"-2", r.number+1
	r.serveGitHub(&liveGitHub{sha: conflictFirstHead, also: r.conflictGitHub()})
	r.seed()
	r.bootLogged()
	return r
}

// seed records the issue as seedImplementing does, on an open pull request at its first head onto
// a base that requires ci, and the control: a second root handed to Legion, in testing, holding its
// tree's admission slot, its lifecycle open under the workflow, on an open pull request of its own
// onto the same base, whose head CI has settled with ci passed (the record's CheckRuns, stamped
// with that head), and with no claim and no phases row, since none of its roles is driven. The
// issue's implementer's phases row is left unwritten, as a daemon that minted no assignment for that
// phase would leave it, which is why its completion is not held to one (api/handoff.go: either side
// empty is nothing to compare); the tester's assignment is minted live, by the start that
// completion makes.
func (r *conflictNoTakeoverLive) seed() {
	t := r.t
	t.Helper()
	ctx := context.Background()
	r.seedImplementing(conflictFirstHead)
	r.board.show(r.control, "testing")
	r.write("the control, its pull request and its slot", func(tx pgx.Tx) error {
		if err := r.records.PutIssue(ctx, tx, record.Issue{
			Key: r.control, Repo: liveRepo, DefaultBranch: conflictBase, GitHubRepo: liveRepo, Tree: r.control,
			Project: r.cfg.Project, Title: "the issue whose required check passed", Phase: phase.Testing, Generation: 1,
			Status: "testing", DispatchStatus: "testing", Rank: "U1", LastDispatchSeq: 1, HandedOver: true,
		}); err != nil {
			return err
		}
		if err := r.records.PutPullRequest(ctx, tx, record.PullRequest{
			State: record.PullRequestOpen, Issue: r.control, Repo: liveRepo, Number: r.controlNumber,
			Branch: claim.IssueBranch(r.control), HeadSHA: conflictControlHead, Base: conflictBase, Failing: []string{},
			CheckedHead: conflictControlHead, Generation: 1,
			CheckRuns: []record.AttemptRun{{Name: conflictRequiredCheck, ID: 1, Head: conflictControlHead, Conclusion: record.RunSucceeded}},
		}); err != nil {
			return err
		}
		return r.admit(ctx, tx, r.control)
	})
	if _, err := r.st.OpenTreeLifecycle(ctx, r.project, r.control, treelifecycle.AuthorityWorkflow); err != nil {
		t.Fatalf("open the lifecycle of %s: %v", r.control, err)
	}
	r.note("seeded: %s implementing on pull request #%d at %s, base %s requiring [%s], its implementer mid-launch serving run 1; and the control %s in testing on pull request #%d at %s, onto the same base, with %s passed there",
		r.key, r.number, conflictFirstHead, conflictBase, conflictRequiredCheck, r.control, r.controlNumber,
		conflictControlHead, conflictRequiredCheck)
}

// Step 1. The seeded implementer completes implementing, naming a handoff record of its own,
// which hands the testing phase to the tester with an assignment of its own and starts it. The
// tester registers, takes that task and works it: a claim working a delivery it confirmed, which is
// the turn the conflict of the steps after it must leave alone.
func (r *conflictNoTakeoverLive) testerWorksItsAssignment() {
	t := r.t
	r.imp = r.claimant(claim.RoleImplementer, "ses_implementer")
	r.note("the implementer is registered and ready on the launch this boot made for the claim the fixture left mid-launch")
	r.imp.startTurn("")
	if status, failure := r.imp.complete("implemented it", conflictFirstHead, "", r.imp.handoff("implement")); status != http.StatusOK {
		t.Fatalf("the implementer's completion = %d %s %s, want 200", status, failure.Code, failure.Error)
	}
	r.imp.endTurn()
	r.note("drove POST /legion/v1/handoff/complete for the implementer at %s, naming its implement record, then its agent_end: 200", conflictFirstHead)
	got := r.waitFor("the issue in testing", func(got api.Issue) bool { return got.Phase == phase.Testing })
	r.note("read back: %s is in %s, status %s", r.key, got.Phase, got.Status)

	r.test = r.claimant(claim.RoleTester, "ses_tester")
	prompt := r.test.takeTask("Phase: testing")
	r.note("read back: the tester was launched, registered and prompted its testing task (%s)", firstLine(prompt.Message))
	working := r.d.claim(r.test.token)
	if working.State != string(supervise.StateWorking) || working.Pending == nil || working.Pending.ConfirmedAt == nil {
		t.Fatalf("the tester is %s holding %+v, want working on a confirmed delivery of its testing task", working.State, working.Pending)
	}
	r.delivery = working.Pending.ID
	r.note("read back: the tester's claim is %s on delivery %s, phase %s, confirmed at %s",
		working.State, working.Pending.ID, working.Pending.Phase, working.Pending.ConfirmedAt.UTC().Format(time.RFC3339Nano))
}

// Step 2. Another pull request lands on the base and GitHub answers the issue's head conflicting
// with it, while the tester's turn runs. The push onto the base asks the daemon to read every open
// pull request on that base (workflow's basePush, mergeStateOwed: the issue stands in testing),
// and this read of the issue's head is its first conflicting one: the daemon records it, queues its
// second read a minute on (record.ConflictConfirm), logs it and acts on nothing. No role of the
// issue is sent anything on its role topic, the architect's included, and neither the tester's
// connection nor the implementer's is sent anything at all; the issue stays in testing, the
// tester's claim stays working on the delivery it confirmed, and nothing is suspended.
func (r *conflictNoTakeoverLive) firstReadSendsNothing() {
	t := r.t
	if before := r.d.claim(r.test.token); before.State != string(supervise.StateWorking) {
		t.Fatalf("the tester is %s before the conflict, want working: the proof turns on a turn that is running", before.State)
	}
	r.published(r.key)
	r.heard, r.suspends = r.heardOn(r.key), len(r.rt.CallsOf("Suspend"))
	r.github.conflicts(true)
	r.publishBasePush()
	r.note("drove a push onto %s while the tester's turn runs, after which GitHub answers %s conflicting with it; %s's ruleset requires %s, which has no run on %s",
		conflictBase, conflictFirstHead, conflictBase, conflictRequiredCheck, conflictFirstHead)
	first := r.awaitLogged(conflictFirstReadLine, r.key, testwait.Remaining(t))
	r.note("read back: the daemon logged %q for %s, head %v against base %v", first.Message, r.key, first.Attrs["head"], first.Attrs["base"])
	if rows := outboxRowsOfKind(t, r.st.Pool(), r.key, record.OutboxKindConflictConfirm); rows != 1 {
		t.Fatalf("the outbox holds %d second reads of %s, want the one the first read queued", rows, r.key)
	}
	r.note("read back: the outbox holds one second read of %s's head (conflict_confirm)", r.key)

	r.test.silent(liveSettle)
	r.imp.silent(liveSettle)
	r.note("read back over %s each: the daemon sent the tester's connection and the implementer's nothing at all", liveSettle)
	if heard := r.heardOn(r.key); !maps.Equal(heard, r.heard) {
		t.Fatalf("the role topics of %s took %v messages after the first read, want %v as before it: a first read sends nothing", r.key, heard, r.heard)
	}
	if rows := outboxRowsOfKind(t, r.st.Pool(), r.key, record.OutboxKindMergeForward); rows != 0 {
		t.Fatalf("the outbox holds %d merge-forward messages of %s after the first read, want none", rows, r.key)
	}
	r.note("read back: no role topic of %s took a message, the architect's included, and the outbox holds no merge-forward message of it", r.key)
	r.heldOn("the first read")
}

// Step 3. The second read, the row the first one queued, runs a minute on, and GitHub still
// answers the head conflicting with the same base: the conflict is confirmed, and R0 decides on the
// base's rules that read carries. ci is required, and no run of it passed on h1, which GitHub runs
// no checks on while it conflicts (record.ConflictCheckNotGreen). The issue stays in testing. The
// implementer is asked, on its own role topic, to merge the base forward (record.MergeForwardAsk),
// the tester is told, on its own, to hold while the merge runs (MergeForwardHold), and the
// architect is told once, in a checks-red notice naming testing, the phase the issue stays in. The
// tester's connection is sent nothing at all — no abort, no prompt — its claim stays working on the
// delivery it confirmed in step 1, and nothing is suspended. Nothing repeats once it is sent.
func (r *conflictNoTakeoverLive) secondReadAsksTheMergeForward() {
	t := r.t
	bound := testwait.Bounded(t, conflictSecondReadBound, time.Second)
	second := r.awaitLogged(conflictSecondReadLine, r.key, bound)
	first := r.loggedFor(conflictFirstReadLine, r.key)[0]
	r.note("read back: the second read ran %s after the first was logged: the daemon logged %q for %s, GitHub answering %v for head %v",
		second.at.Sub(first.at).Round(time.Second), second.Message, r.key, second.Attrs["mergeable"], second.Attrs["readHead"])
	asked := r.awaitLogged(conflictAskedLine, r.key, bound)
	if asked.Attrs["why"] != string(record.ConflictCheckNotGreen) || asked.Attrs["phase"] != string(phase.Testing) || asked.Attrs["head"] != conflictFirstHead {
		t.Fatalf("the daemon logged %q for %s with %v, want the merge forward of %s asked in testing for %s",
			asked.Message, r.key, asked.Attrs, conflictFirstHead, record.ConflictCheckNotGreen)
	}
	r.note("read back: the daemon logged %q for %s: head %v, phase %v, why %v", asked.Message, r.key, asked.Attrs["head"], asked.Attrs["phase"], asked.Attrs["why"])

	// The texts name the pull request by the repository the record holds it under; neither the ask
	// nor the hold reads anything else of the record.
	pull := record.PullRequest{Repo: liveRepo}
	ask := record.MergeForwardMessage{Role: claim.RoleImplementer, Step: record.MergeForwardAsk, Head: conflictFirstHead,
		Base: conflictBase, Phase: phase.Testing, PullRequest: r.number, Why: record.ConflictCheckNotGreen}
	if told := r.awaitHeard(claim.RoleImplementer, 1, bound); told[0].Message != ask.Message(r.key, pull) {
		t.Fatalf("the implementer was told %q, want the merge forward's ask %q", told[0].Message, ask.Message(r.key, pull))
	}
	r.note("read back: one message on the implementer's role topic — %s", ask.Message(r.key, pull))
	hold := ask
	hold.Role, hold.Step = claim.RoleTester, record.MergeForwardHold
	if told := r.awaitHeard(claim.RoleTester, 1, bound); told[0].Message != hold.Message(r.key, pull) {
		t.Fatalf("the tester was told %q, want the merge forward's hold %q", told[0].Message, hold.Message(r.key, pull))
	}
	r.note("read back: one message on the tester's role topic — %s", hold.Message(r.key, pull))
	var notices []record.Notice
	testwait.WithinPolling(t, bound, testwait.Poll, "the architect's notice of the merge forward", func() bool {
		notices = r.architectNoticesSince()
		return len(notices) > 0
	})
	if len(notices) != 1 || notices[0].Kind != "checks-red" || notices[0].Phase != phase.Testing {
		t.Fatalf("the architect was told %+v after the second read, want one checks-red notice naming testing", notices)
	}
	r.note("read back: one notice on the architect's role topic, checks-red naming %s — %s", notices[0].Phase, notices[0].Reason)

	r.test.silent(liveSettle)
	r.note("read back over %s: the daemon sent the tester's connection nothing at all — no abort (the frame is gone), no shutdown, no prompt", liveSettle)
	want := maps.Clone(r.heard)
	for _, role := range []claim.Role{claim.RoleImplementer, claim.RoleTester, claim.RoleArchitect} {
		want[role]++
	}
	if heard := r.heardOn(r.key); !maps.Equal(heard, want) {
		t.Fatalf("the role topics of %s took %v messages, want %v: the ask, the hold and the notice once each", r.key, heard, want)
	}
	r.note("read back: nothing more reached any role topic of %s", r.key)
	if got := r.issue(r.key); got.Phase != phase.Testing {
		t.Fatalf("%s is in %s after the confirmed conflict, want testing: a conflict moves no issue", r.key, got.Phase)
	}
	r.heldOn("the second read")
}

// Step 4. The implementer pushes its merge of the base, h2, and GitHub's synchronize arrives: the
// merge forward landed (workflow's mergeForwardLanded), and the tester is told so on its role topic,
// with the head the merge replaced, the new one and how to read what the merge changed
// (record.MergeForwardLanded). The tester's turn is still the one its step 1 assignment began. Its
// completion at h1, the head the merge replaced, naming a handoff record of its own, is refused
// 409 HANDOFF_HEAD_MERGED_FORWARD and records nothing: the issue stays in testing and the tester's
// phases row holds no verdict and no commit. Its completion at h2, naming another record of its
// own, is taken, and moves the issue to reviewing.
func (r *conflictNoTakeoverLive) mergeLands() {
	t := r.t
	r.github.conflicts(false)
	r.push(conflictFirstHead, conflictMergedHead, "merge main forward")
	r.note("drove the implementer's push of %s, its merge of %s forward onto %s, and GitHub's synchronize of #%d to it, GitHub answering it mergeable",
		conflictMergedHead, conflictBase, conflictFirstHead, r.number)
	landed := record.MergeForwardMessage{Role: claim.RoleTester, Step: record.MergeForwardLanded, Head: conflictFirstHead,
		Landed: conflictMergedHead, Base: conflictBase, Phase: phase.Testing, PullRequest: r.number, Why: record.ConflictCheckNotGreen}
	// The tester's landed reads nothing of the record but the repository it holds the pull request under.
	pull := record.PullRequest{Repo: liveRepo}
	told := r.awaitHeard(claim.RoleTester, 2, testwait.Remaining(t))
	if told[1].Message != landed.Message(r.key, pull) {
		t.Fatalf("the tester was told %q, want the merge forward's landing %q", told[1].Message, landed.Message(r.key, pull))
	}
	r.note("read back: the tester's role topic took the landing — %s", landed.Message(r.key, pull))
	r.heldOn("the landing")

	status, failure := r.test.complete("tested it", conflictFirstHead, "pass", r.test.handoff("test"))
	if status != http.StatusConflict || failure.Code != "HANDOFF_HEAD_MERGED_FORWARD" {
		t.Fatalf("the tester's completion at %s = %d %s %s, want 409 HANDOFF_HEAD_MERGED_FORWARD", conflictFirstHead, status, failure.Code, failure.Error)
	}
	r.note("read back: the tester's completion at %s, from its step 1 turn = 409 %s — %s", conflictFirstHead, failure.Code, failure.Error)
	if after := r.issue(r.key); after.Phase != phase.Testing {
		t.Fatalf("%s is in %s after the refused completion, want testing: a refusal records nothing", r.key, after.Phase)
	}
	if row := rolePhaseRow(t, r.st.Pool(), r.key, claim.RoleTester); row.Verdict != "" || row.HandoffCommit != "" {
		t.Fatalf("the tester's phases row holds verdict %q at commit %q after the refusal, want neither: the refusal records nothing",
			row.Verdict, row.HandoffCommit)
	}
	r.note("read back: %s is still in testing, and the tester's phases row holds no verdict and no commit — nothing was recorded", r.key)

	if status, failure := r.test.complete("tested the merge", conflictMergedHead, "pass", r.test.handoff("test")); status != http.StatusOK {
		t.Fatalf("the tester's completion at %s = %d %s %s, want 200", conflictMergedHead, status, failure.Code, failure.Error)
	}
	r.test.endTurn()
	moved := r.waitFor("the issue in reviewing", func(got api.Issue) bool { return got.Phase == phase.Reviewing })
	r.note("read back: the tester's completion at %s, naming a record of its own, was taken in the same turn: %s is in %s",
		conflictMergedHead, r.key, moved.Phase)
}

// Step 5. The control. The push onto the base of step 2 asked for a read of every open pull
// request on that base, the control's among them, and GitHub answers the control's head conflicting
// too; its second read, a minute on, confirms the conflict as the issue's did. But ci, the one
// check the base requires, passed on that head, the ruleset requires no pull request to be current
// with the base, and the control's next step is no merge, so its conflict needs no merge yet
// (classify.ConflictNeedsMerge answers none): the daemon logs the conflict with the checks the base
// requires and those green on the head, and sends nothing. No role topic of the control takes a
// message, its architect's included, the outbox holds no merge-forward message of it, and it stays
// in testing.
func (r *conflictNoTakeoverLive) passedControlGetsNothing() {
	t := r.t
	confirmed := r.awaitLogged(conflictNoMergeLine, r.control, testwait.Bounded(t, conflictSecondReadBound, time.Second))
	if first := r.loggedFor(conflictFirstReadLine, r.control); len(first) == 0 {
		t.Fatalf("the daemon logged %q for %s with no first read of its head logged before it", confirmed.Message, r.control)
	}
	r.note("read back: the daemon logged a first read of %s's head, then %q: head %v, phase %v, required %v, green %v",
		r.control, confirmed.Message, confirmed.Attrs["head"], confirmed.Attrs["phase"], confirmed.Attrs["required"], confirmed.Attrs["green"])
	time.Sleep(liveSettle)
	for role, heard := range r.heardOn(r.control) {
		if heard != 0 {
			t.Fatalf("the %s's role topic of %s took %d messages, want none: a conflict that needs no merge is told to nobody", role, r.control, heard)
		}
	}
	if asked := r.loggedFor(conflictAskedLine, r.control); len(asked) != 0 {
		t.Fatalf("the daemon logged %q for %s, want no merge forward asked of a conflict that needs none", asked[0].Message, r.control)
	}
	if rows := outboxRowsOfKind(t, r.st.Pool(), r.control, record.OutboxKindMergeForward); rows != 0 {
		t.Fatalf("the outbox holds %d merge-forward messages of %s, want none", rows, r.control)
	}
	if got := r.issue(r.control); got.Phase != phase.Testing {
		t.Fatalf("%s is in %s after its confirmed conflict, want testing", r.control, got.Phase)
	}
	r.note("read back over %s: no role topic of %s took a message, the architect's included, the daemon asked no merge forward of it, the outbox holds no merge-forward message of it, and it is still in testing",
		liveSettle, r.control)
}

// heldOn holds that the tester's turn of step 1 runs on after what: its claim is still working on
// the delivery it confirmed then, and the fake runtime was asked to suspend nothing since the
// conflict began.
func (r *conflictNoTakeoverLive) heldOn(what string) {
	t := r.t
	t.Helper()
	held := r.d.claim(r.test.token)
	if held.State != string(supervise.StateWorking) || held.Pending == nil || held.Pending.ID != r.delivery {
		t.Fatalf("the tester is %s holding %+v after %s, want it still working its delivery %s", held.State, held.Pending, what, r.delivery)
	}
	if suspends := len(r.rt.CallsOf("Suspend")); suspends != r.suspends {
		t.Fatalf("the fake runtime was asked for %d suspensions after %s, want none: no role is stopped by a conflict", suspends-r.suspends, what)
	}
	r.note("read back: after %s the tester's claim is still %s on delivery %s, and the fake runtime was asked to suspend nothing", what, held.State, held.Pending.ID)
}

// conflictGitHub is what this proof's GitHub answers beyond the harness's stand-in (liveGitHub's
// also): the base's rules for a conflicting head (conflictBaseRules), then the control's pull
// request, which GitHub answers conflicting with that base from the start, and its reviews, as
// none.
func (r *conflictNoTakeoverLive) conflictGitHub() map[string]any {
	also := conflictBaseRules()
	also[fmt.Sprintf("/pulls/%d", r.controlNumber)] = map[string]any{
		"number": r.controlNumber, "state": "open", "mergeable": false,
		"base": map[string]string{"ref": conflictBase},
		"head": map[string]string{"ref": claim.IssueBranch(r.control), "sha": conflictControlHead},
	}
	also[fmt.Sprintf("/pulls/%d/reviews", r.controlNumber)] = []any{}
	return also
}
