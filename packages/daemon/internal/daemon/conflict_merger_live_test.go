//go:build e2e

// The live proof's driver for who gets a conflict once the work is past its review
// (cybermonklabs/legion#4979): a pull request that conflicts with its base after the review is the
// merger's to resolve, and nobody hands the issue back to the implementer for it. The issue stands
// in accepting on a head GitHub then answers CONFLICTING: the daemon reads it twice a minute apart,
// as it reads any conflicting head, records the confirmed conflict and asks nobody, the
// architect's checks-red notice included, since the next step is the merge and the merger makes
// that merge. The architect's sign-off moves the issue to retro, which asks nothing either. Retro's
// completion moves it to merging: the merger is started, and asked on its own Envoy role topic to
// merge the base forward and resolve the conflict itself (record.MergeForwardMessage, Role merger,
// Step ask); the implementer's topic takes no such message, and the architect is told once, in a
// checks-red notice naming merging and the merger. The merger's push of its merge lands, as a
// synchronize to a new head made by the implement App, which the merger merges as: the merger is
// told the merge landed and to merge only once the reviewer's approval of the new head is on the
// pull request, and the reviewer is asked to read the merge with `git show --remerge-diff` and
// approve the new head (R2). A required check that settles red on the new head is told to the
// merger and the architect, and not to the implementer. GitHub's merge of the pull request closes
// the issue. Throughout, the issue never enters implementing: every move the daemon logs for it goes
// from accepting to retro to merging to done, no read of GET /legion/v1/state shows implementing,
// and the daemon writes no in_progress status to Dispatch. Last, the merger's own
// prompt, as its launch carries it, is printed beside what the daemon sent it, for a reader to
// hold the one against the other. scripts/e2e/conflict-merger.sh builds this driver with
// `-tags e2e`, owns the nats-server it runs against, and takes the PostgreSQL from
// LEGION_TEST_PG_DSN, which it starts none of.
//
// The daemon is the whole daemon: run, the function `legion start` calls, booted as this package's
// other whole-daemon proofs boot it (startDaemonWithin, through conflictLive's bootLogged), so the
// proof exercises the real configuration, the real store and its migrations, the real HTTP API,
// the real claim supervision and its worker stream, the real openWorkflow, the real JetStream
// consumers, the real merge-state reads and the real outbox lanes — the lane that runs the second
// read (record.ConflictConfirm), the one that publishes the merge forward's messages
// (record.MergeForwardMessage) and the one that publishes a CI settlement's role messages
// (record.RoleMessage) among them — and the real prompt bundle, composed at boot, which the
// merger's launch carries. Facts enter only as production's do: GitHub envelopes on the
// ENVOY_NOTIFICATIONS stream for the push onto the base, the merger's push and its synchronize,
// CI's settlement and the merge; the daemon's own HTTP routes for each role's registration,
// readiness, grants, the architect's sign-off and the implementer's completion of retro; and the
// daemon's worker stream for each role's turns, which this driver speaks as `legion worker-shim`
// does (shimwire frames). Every assertion is a read of GET /legion/v1/state, of the operator's
// claim listing, of what the daemon published to the Envoy listener stand-in, of what it sent — or
// did not send — to a role's own connection, of the launch the fake runtime recorded, of the
// writes it made to the Dispatch stand-in, or of the lines the daemon logged for its decisions
// (recordingLog). The store is read only for what no route shows: the pull request's record of the
// conflict and of the merge forward.
//
// What the proof stands in for lies outside the change. The broker is a native nats-server the
// script starts and names in LEGION_E2E_NATS_URL. GitHub is the harness's REST stand-in for the
// issue's pull request (liveGitHub, in live_issue_proof_test.go), which answers the head
// conflicting from the moment the driver says so, with the base's rules (conflictBaseRules). The
// minute between the two reads is the daemon's own (workflow's conflictConfirmWait); no seam a
// test may set shortens it, so step 1 waits it out. Dispatch is the board the package's
// whole-daemon live proofs share (liveDispatch), which keeps every status the daemon writes, and
// Envoy's listener is that harness's noticeListener, which takes every publish whether or not a
// session holds the role. The two Apps' tokens are workflowTokenRecorder's placeholders. No agent
// runs: every claim is launched by the package's fake runtime, which says it provisions each
// workspace in the claim's own pod, so the daemon clones nothing, and each role the proof acts as is
// driven over the daemon's own worker stream and HTTP routes. The issue, its pull request, its slot
// and its architect's claim are seeded through the store, as every daemon test seeds its fixture;
// what is driven is the change.
package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
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

// The heads the proof drives: the head the review approved and the architect accepts, which the
// push onto the base leaves conflicting, and the head the merger's merge of the base pushes.
const (
	mergerAcceptedHead = "head-accepted"
	mergerMergeHead    = "head-merged"
)

// mergerPromptSections are the headings of the merger's prompt a reader holds against what the
// daemon sent the merger in the run (prompts/roles/merger.md): the conflict, and the check that
// failed beneath it.
var mergerPromptSections = []string{"## A head that conflicts with its base", "### A check that failed"}

// conflictMergerLive is one run of the proof: the booted daemon, its log and the baseline of each
// role topic (conflictLive), the three roles the driver acts as over the worker stream, and the
// watch of the issue's phase over the whole run.
type conflictMergerLive struct {
	conflictLive
	arch, imp, merger *liveClaimant
	watch             *phaseWatch
}

// TestAConflictPastTheReviewIsTheMergersLive drives the six steps of the proof against one booted
// daemon, in the order each step's state follows from the last: the conflict in accepting that is
// recorded and asks nobody, the sign-off and retro that ask nothing until retro's completion asks
// the merger in merging, the merger's merge landing with the reviewer asked to read it, a red check
// on the new head told to the merger and not the implementer, GitHub's merge closing the issue
// with no move into implementing at any point, and the merger's prompt as its launch carries it.
func TestAConflictPastTheReviewIsTheMergersLive(t *testing.T) {
	r := newConflictMergerLive(t)
	r.step("step1-a-conflict-in-accepting-is-recorded-and-asks-nobody", r.acceptingRecordsTheConflict)
	r.step("step2-sign-off-and-retro-ask-nothing-and-merging-asks-the-merger", r.mergingAsksTheMerger)
	r.step("step3-the-mergers-push-lands-and-the-reviewer-is-asked-to-read-the-merge", r.mergerMergeLands)
	r.step("step4-a-red-check-in-merging-reaches-the-merger-and-not-the-implementer", r.redCheckReachesTheMerger)
	r.step("step5-githubs-merge-closes-the-issue-which-never-entered-implementing", r.mergeClosesTheIssue)
	r.step("step6-the-mergers-prompt-as-its-launch-carries-it", r.mergerPromptAsLaunched)
}

// newConflictMergerLive boots the daemon over the script's broker with the issue already recorded
// in accepting (seed), and its architect's claim as a launch a previous daemon began and never
// recorded, which this boot relaunches, then starts the watch of the issue's phase.
func newConflictMergerLive(t *testing.T) *conflictMergerLive {
	t.Helper()
	r := &conflictMergerLive{conflictLive: conflictLive{liveIssueProof: newLiveIssueProof(t, "scripts/e2e/conflict-merger.sh",
		conflictBase, "the accepted issue whose head starts conflicting with its base")}}
	r.serveGitHub(&liveGitHub{sha: mergerAcceptedHead, also: conflictBaseRules()})
	r.seed()
	r.bootLogged()
	r.watch = watchPhase(t, r.d, r.key)
	return r
}

// seed records the issue as the daemon holds it once its review approved its head and before its
// architect signed off: a root handed to Legion, in accepting, holding its tree's admission slot,
// its lifecycle open under the workflow, on an open pull request at the accepted head onto a base
// that requires ci, where ci passed (the record's CheckRuns, stamped with that head) and GitHub
// last answered the head mergeable and clean. Its architect's claim is a launch a previous daemon
// began and never recorded, which the boot relaunches. Every other role is started by the daemon
// itself when the issue reaches the phase that needs it: the implementer by retro and the merger by
// merging. The architect's and the implementer's prompts are written to disk as a launch writes
// one; the merger's is not, so its launch carries the role prompt the daemon composed at boot
// (specs.rolePromptPaths), which step 6 reads.
func (r *conflictMergerLive) seed() {
	t := r.t
	t.Helper()
	ctx := context.Background()
	r.board.show(r.key, "needs_review")
	r.write("the issue in accepting, its pull request and its slot", func(tx pgx.Tx) error {
		if err := r.records.PutIssue(ctx, tx, record.Issue{
			Key: r.key, Repo: liveRepo, DefaultBranch: conflictBase, GitHubRepo: liveRepo, Tree: r.key, Project: r.cfg.Project,
			Title: r.title, Phase: phase.Accepting, Generation: 1, Status: "needs_review", DispatchStatus: "needs_review",
			Rank: "U", LastDispatchSeq: 1, HandedOver: true,
		}); err != nil {
			return err
		}
		if err := r.records.PutPullRequest(ctx, tx, record.PullRequest{
			State: record.PullRequestOpen, Issue: r.key, Repo: liveRepo, Number: r.number, Branch: claim.IssueBranch(r.key),
			HeadSHA: mergerAcceptedHead, Base: conflictBase, Failing: []string{}, CheckedHead: mergerAcceptedHead, Generation: 1,
			CheckRuns:    []record.AttemptRun{{Name: conflictRequiredCheck, ID: 1, Head: mergerAcceptedHead, Conclusion: record.RunSucceeded}},
			Mergeability: record.MergeabilityMergeable, MergeState: record.MergeStateClean,
		}); err != nil {
			return err
		}
		return r.admit(ctx, tx, r.key)
	})
	if _, err := r.st.OpenTreeLifecycle(ctx, r.project, r.key, treelifecycle.AuthorityWorkflow); err != nil {
		t.Fatalf("open the lifecycle of %s: %v", r.key, err)
	}
	if _, err := r.st.AdmitClaim(ctx, supervise.Claim{
		Token: r.token(claim.RoleArchitect), Project: r.project, Tree: r.key, Issue: r.key, Role: claim.RoleArchitect,
		Generation: 1, State: supervise.StateLaunching, BootTokenHash: supervise.HashBootToken("interrupted-" + randomSuffix(t)),
		ServingGeneration: 1,
	}); err != nil {
		t.Fatalf("record the architect's unfinished launch: %v", err)
	}
	for _, role := range []claim.Role{claim.RoleArchitect, claim.RoleImplementer} {
		writePrompt(t, r.cfg, r.token(role))
	}
	r.note("seeded: %s in accepting on pull request #%d at %s, base %s requiring [%s], %s passed there; its architect mid-launch",
		r.key, r.number, mergerAcceptedHead, conflictBase, conflictRequiredCheck, conflictRequiredCheck)
}

// Step 1. Another pull request lands on the base and GitHub answers the accepted head conflicting
// with it. The push onto the base asks the daemon to read every open pull request on that base
// (classify.MergeStateReadOwed: the issue stands in accepting), and this read is its first
// conflicting one: the daemon queues its second read a minute on. The second read confirms the
// conflict, and the daemon records it (record.PullRequest.ConflictingHead) and asks nobody
// (classify.ConflictNeedsMerge answers none in accepting), logging a confirmed conflict that needs
// no merge yet. No role topic of the issue takes a message, the architect's included, so it gets
// no checks-red notice; the architect's connection is sent nothing at all; the record holds no
// merge forward; and the issue stays in accepting.
func (r *conflictMergerLive) acceptingRecordsTheConflict() {
	t := r.t
	r.arch = r.claimant(claim.RoleArchitect, "ses_architect")
	r.note("the architect is registered and ready on the launch this boot made for the claim the fixture left mid-launch")
	r.published(r.key)
	r.heard = r.heardOn(r.key)
	r.github.conflicts(true)
	r.publishBasePush()
	r.note("drove a push onto %s, after which GitHub answers %s CONFLICTING", conflictBase, mergerAcceptedHead)
	first := r.awaitLogged(conflictFirstReadLine, r.key, testwait.Bounded(t, time.Minute, time.Second))
	r.note("read back: the daemon logged %q for %s, head %v against base %v", first.Message, r.key, first.Attrs["head"], first.Attrs["base"])
	bound := testwait.Bounded(t, conflictSecondReadBound, time.Second)
	second := r.awaitLogged(conflictSecondReadLine, r.key, bound)
	r.note("read back: the second read ran %s after the first was logged: GitHub answered %v for head %v",
		second.at.Sub(first.at).Round(time.Second), second.Attrs["mergeable"], second.Attrs["readHead"])
	recorded := r.awaitLogged(conflictNoMergeLine, r.key, bound)
	if recorded.Attrs["phase"] != string(phase.Accepting) || recorded.Attrs["head"] != mergerAcceptedHead {
		t.Fatalf("the daemon logged %q for %s with %v, want the confirmed conflict of %s in accepting", recorded.Message, r.key, recorded.Attrs, mergerAcceptedHead)
	}
	r.note("read back: the daemon logged %q for %s: head %v, phase %v, required %v, green %v",
		recorded.Message, r.key, recorded.Attrs["head"], recorded.Attrs["phase"], recorded.Attrs["required"], recorded.Attrs["green"])
	if asked := r.loggedFor(conflictAskedLine, r.key); len(asked) != 0 {
		t.Fatalf("the daemon logged %q for %s in accepting with %v, want no merge forward asked before merging", asked[0].Message, r.key, asked[0].Attrs)
	}

	r.arch.silent(liveSettle)
	r.note("read back over %s: the daemon sent the architect's connection nothing at all", liveSettle)
	r.published(r.key)
	if heard := r.heardOn(r.key); !maps.Equal(heard, r.heard) {
		t.Fatalf("the role topics of %s took %v messages after the confirmed conflict, want %v as before it: a conflict in accepting asks nobody", r.key, heard, r.heard)
	}
	r.note("read back: no role topic of %s took a message, the architect's included: no checks-red notice", r.key)
	pr := r.pullRequest()
	if pr.ConflictingHead != mergerAcceptedHead || pr.MergeForward != nil {
		t.Fatalf("the record of #%d holds conflicting head %q and merge forward %+v, want the conflict of %s recorded and no merge forward",
			r.number, pr.ConflictingHead, pr.MergeForward, mergerAcceptedHead)
	}
	r.note("read back: the record holds %s conflicting with %s and no merge forward", pr.ConflictingHead, pr.Base)
	if got := r.issue(r.key); got.Phase != phase.Accepting {
		t.Fatalf("%s is in %s after the confirmed conflict, want accepting: a conflict moves no issue", r.key, got.Phase)
	}
	r.note("read back: %s is still in accepting", r.key)
}

// Step 2. The architect, in a turn of its own, signs off the issue through its own route, which
// moves it to retro and starts the implementer there. Still nothing is asked: the daemon logs no
// merge forward, the record holds none, the architect takes no checks-red notice and the
// implementer's role topic takes nothing. The implementer takes its retro task and completes it at
// the accepted head, rating the plan, which moves the issue to merging. That move asks the merger,
// whose merge it is: the daemon logs the merge forward asked in merging for the merge being next
// (record.ConflictMergeNext), starts the merger, and publishes the ask to the merger's own role
// topic, which says the merge and its conflict are the merger's to resolve. The implementer's
// topic takes nothing, and the architect is told once, in a checks-red notice naming merging and
// the merger. The merger registers and takes its merging task. Nothing repeats.
func (r *conflictMergerLive) mergingAsksTheMerger() {
	t := r.t
	r.arch.startTurn("")
	status, body := r.d.request(http.MethodPost, "/legion/v1/signoff", map[string]any{"grantId": r.arch.grant(), "issue": r.key}, false)
	if status != http.StatusOK {
		t.Fatalf("the architect's sign-off of %s = %d %s, want 200", r.key, status, body)
	}
	r.arch.endTurn()
	r.note("drove POST /legion/v1/signoff for %s from the architect's own turn, then its agent_end: 200", r.key)
	retro := r.waitFor("the issue in retro", func(got api.Issue) bool { return got.Phase == phase.Retro })
	r.note("read back: %s is in %s, status %s", r.key, retro.Phase, retro.Status)

	r.imp = r.claimant(claim.RoleImplementer, "ses_implementer")
	prompt := r.imp.takeTask("Phase: retro")
	r.note("read back: the implementer was launched, registered and prompted its retro task (%s)", firstLine(prompt.Message))
	r.published(r.key)
	if asked := r.loggedFor(conflictAskedLine, r.key); len(asked) != 0 {
		t.Fatalf("the daemon logged %q for %s with %v, want no merge forward asked in retro", asked[0].Message, r.key, asked[0].Attrs)
	}
	if pr := r.pullRequest(); pr.MergeForward != nil {
		t.Fatalf("the record of #%d holds merge forward %+v in retro, want none: the conflict waits for merging", r.number, pr.MergeForward)
	}
	if notices := r.architectNoticesSince(); len(notices) != 0 {
		t.Fatalf("the architect was told %+v by retro, want nothing: nothing is asked before merging", notices)
	}
	if heard := r.heardOn(r.key); heard[claim.RoleImplementer] != r.heard[claim.RoleImplementer] {
		t.Fatalf("the implementer's role topic took %d messages by retro, want none", heard[claim.RoleImplementer]-r.heard[claim.RoleImplementer])
	}
	r.note("read back: in retro the daemon asked no merge forward, the record holds none, the architect took no notice and the implementer's role topic took nothing")

	rating, reason := 4, "The plan fit the work."
	status, failure := r.imp.completeWith(api.HandoffCompleteRequest{Summary: "retro written", Commit: mergerAcceptedHead,
		PlanRating: &rating, PlanReason: &reason})
	if status != http.StatusOK {
		t.Fatalf("the implementer's completion of retro = %d %s %s, want 200", status, failure.Code, failure.Error)
	}
	r.imp.endTurn()
	r.note("drove POST /legion/v1/handoff/complete for the implementer's retro at %s, rating the plan %d, then its agent_end: 200", mergerAcceptedHead, rating)
	merging := r.waitFor("the issue in merging", func(got api.Issue) bool { return got.Phase == phase.Merging })
	r.note("read back: %s is in %s, status %s", r.key, merging.Phase, merging.Status)

	bound := testwait.Bounded(t, time.Minute, time.Second)
	asked := r.awaitLogged(conflictAskedLine, r.key, bound)
	if asked.Attrs["phase"] != string(phase.Merging) || asked.Attrs["why"] != string(record.ConflictMergeNext) || asked.Attrs["head"] != mergerAcceptedHead {
		t.Fatalf("the daemon logged %q for %s with %v, want the merge forward of %s asked in merging for %s",
			asked.Message, r.key, asked.Attrs, mergerAcceptedHead, record.ConflictMergeNext)
	}
	r.note("read back: the daemon logged %q for %s: head %v, phase %v, why %v", asked.Message, r.key, asked.Attrs["head"], asked.Attrs["phase"], asked.Attrs["why"])
	launch := r.awaitLaunch(r.token(claim.RoleMerger))
	r.note("read back: the merger was launched, generation %d", launch.Generation)
	ask := record.MergeForwardMessage{Role: claim.RoleMerger, Step: record.MergeForwardAsk, Head: mergerAcceptedHead,
		Base: conflictBase, Phase: phase.Merging, PullRequest: r.number, Why: record.ConflictMergeNext}
	told := r.awaitHeard(claim.RoleMerger, 1, bound)
	if told[0].Message != ask.Message(r.key, record.PullRequest{Repo: liveRepo}) || !strings.Contains(told[0].Message, "The merge and its conflict are yours to resolve") {
		t.Fatalf("the merger was told %q, want the merge forward's ask to the merger %q", told[0].Message, ask.Message(r.key, record.PullRequest{Repo: liveRepo}))
	}
	r.note("read back: one message on the merger's role topic — %s", told[0].Message)
	checksRed := func() ([]record.Notice, []record.Notice) {
		var red, other []record.Notice
		for _, notice := range r.architectNoticesSince() {
			if notice.Kind == "checks-red" {
				red = append(red, notice)
			} else {
				other = append(other, notice)
			}
		}
		return red, other
	}
	var red, other []record.Notice
	testwait.WithinPolling(t, bound, testwait.Poll, "the architect's checks-red notice of the merge forward", func() bool {
		red, other = checksRed()
		return len(red) > 0
	})
	if len(red) != 1 || red[0].Phase != phase.Merging || !strings.Contains(red[0].Reason, "the merger is asked to merge "+conflictBase+" forward") {
		t.Fatalf("the architect was told the checks-red notices %+v on the move into merging, want one naming merging and the merger", red)
	}
	r.note("read back: one checks-red notice on the architect's role topic, naming %s — %s", red[0].Phase, red[0].Reason)

	r.merger = r.claimant(claim.RoleMerger, "ses_merger")
	task := r.merger.takeTask("Phase: merging")
	r.note("read back: the merger registered and was prompted its merging task — %s", task.Message)
	r.published(r.key)
	time.Sleep(liveSettle)
	red, other = checksRed()
	if len(red) != 1 {
		t.Fatalf("the architect was told the checks-red notices %+v by merging, want one: nothing repeats", red)
	}
	for _, notice := range other {
		r.note("read back: the architect's role topic also took a %s notice of the %s's %s", notice.Kind, notice.Role, notice.Phase)
	}
	want := maps.Clone(r.heard)
	want[claim.RoleMerger]++
	want[claim.RoleArchitect] += 1 + len(other)
	if heard := r.heardOn(r.key); !maps.Equal(heard, want) {
		t.Fatalf("the role topics of %s took %v messages by merging, want %v: the merger's ask once, the architect's notices, and nothing to the implementer", r.key, heard, want)
	}
	r.note("read back over %s: nothing more reached any role topic of %s; the merger's took the ask once and the implementer's took nothing", liveSettle, r.key)
	r.noMergeForwardTo(claim.RoleImplementer)
}

// Step 3. The merger pushes its merge of the base, a new head, and GitHub's synchronize arrives,
// sent by the implement App the merger pushes as: the merge forward landed (workflow's
// mergeForwardLanded). The merger, the worker of merging, is told so on its role topic, with both
// heads, how to read what the merge changed and to merge the new head only once the reviewer's
// approval of it is on the pull request; the reviewer is asked on its own to read the merge with
// `git show --remerge-diff` and approve the new head or request changes. The record holds the
// merge forward asked of the merger, landed at the new head, with the reviewer's review asked. The
// merger's connection is sent nothing, and the issue stays in merging.
func (r *conflictMergerLive) mergerMergeLands() {
	t := r.t
	r.github.conflicts(false)
	r.pushAs(mergerAcceptedHead, mergerMergeHead, "Merge branch '"+conflictBase+"' into "+claim.IssueBranch(r.key), implementerLogin)
	r.note("drove the merger's push of %s, its merge of %s into %s, as %s, and GitHub's synchronize of #%d to it from %s, GitHub answering it mergeable",
		mergerMergeHead, conflictBase, mergerAcceptedHead, implementerLogin, r.number, mergerAcceptedHead)
	bound := testwait.Bounded(t, time.Minute, time.Second)
	landed := record.MergeForwardMessage{Role: claim.RoleMerger, Step: record.MergeForwardLanded, Head: mergerAcceptedHead,
		Landed: mergerMergeHead, Base: conflictBase, Phase: phase.Merging, PullRequest: r.number, Why: record.ConflictMergeNext}
	told := r.awaitHeard(claim.RoleMerger, 2, bound)
	// The landing asked the reviewer, since the issue's task has a reviewer step, so the merger's
	// landing holds its merge to that approval (MergeForward.ReviewAsked).
	landedPR := record.PullRequest{Repo: liveRepo, MergeForward: &record.MergeForward{Landed: mergerMergeHead, ReviewAsked: true}}
	if told[1].Message != landed.Message(r.key, landedPR) ||
		!strings.Contains(told[1].Message, "only once the reviewer's approval of that head is on the pull request") {
		t.Fatalf("the merger was told %q, want the landing %q", told[1].Message, landed.Message(r.key, landedPR))
	}
	r.note("read back: the merger's role topic took the landing — %s", told[1].Message)
	review := landed
	review.Role, review.Step = claim.RoleReviewer, record.MergeForwardReview
	asked := r.awaitHeard(claim.RoleReviewer, 1, bound)
	if asked[0].Message != review.Message(r.key, landedPR) || !strings.Contains(asked[0].Message, "git show --remerge-diff "+mergerMergeHead) ||
		!strings.Contains(asked[0].Message, mergerAcceptedHead) {
		t.Fatalf("the reviewer was told %q, want the review of the merge %q", asked[0].Message, review.Message(r.key, landedPR))
	}
	r.note("read back: the reviewer's role topic took the review ask — %s", asked[0].Message)
	logged := r.awaitLogged("workflow: the merge forward landed", r.key, bound)
	r.note("read back: the daemon logged %q for %s: head %v, landed %v, phase %v", logged.Message, r.key, logged.Attrs["head"], logged.Attrs["landed"], logged.Attrs["phase"])
	pr := r.pullRequest()
	if m := pr.MergeForward; m == nil || m.Asked() != claim.RoleMerger || m.Head != mergerAcceptedHead || m.Landed != mergerMergeHead || !m.ReviewAsked {
		t.Fatalf("the record of #%d holds merge forward %+v, want the merger's merge of %s landed at %s with the review asked", r.number, pr.MergeForward, mergerAcceptedHead, mergerMergeHead)
	}
	r.note("read back: the record holds the merge forward asked of the %s in %s, of %s, landed at %s, the reviewer's review asked", pr.MergeForward.Asked(),
		pr.MergeForward.Phase, pr.MergeForward.Head, pr.MergeForward.Landed)
	r.merger.silent(liveSettle)
	r.note("read back over %s: the daemon sent the merger's connection nothing", liveSettle)
	if got := r.issue(r.key); got.Phase != phase.Merging || got.PullRequest == nil || got.PullRequest.Head != mergerMergeHead {
		t.Fatalf("%s is in %s at %+v after the landing, want merging at %s", r.key, got.Phase, got.PullRequest, mergerMergeHead)
	}
	r.note("read back: %s is still in merging, its pull request at %s", r.key, mergerMergeHead)
}

// Step 4. CI settles a required check red on the merger's head. In merging the CI result is the
// merger's and the architect's (workflow's tellChecks): each is told on its own role topic, and the
// implementer, whose claim is live and would take a message published to it, is told nothing, and
// the daemon logs no role message to it. The issue stays in merging.
func (r *conflictMergerLive) redCheckReachesTheMerger() {
	t := r.t
	r.published(r.key)
	r.heard = r.heardOn(r.key)
	implementer := r.d.claim(r.imp.token)
	if implementer.State != string(supervise.StateIdle) {
		t.Fatalf("the implementer's claim is %s before the settlement, want idle: a released claim would end a message to it unpublished, and the read would see nothing either way", implementer.State)
	}
	r.note("read back: the implementer's claim is %s, so a message the daemon addressed to it would be published", implementer.State)
	r.publishSettlementOf(r.number, mergerMergeHead, 1, nil, []string{conflictRequiredCheck})
	r.note("drove CI's settlement of #%d at %s: %s failed", r.number, mergerMergeHead, conflictRequiredCheck)
	bound := testwait.Bounded(t, time.Minute, time.Second)
	prefix := fmt.Sprintf("Pull request #%d: ", r.number)
	for _, role := range []claim.Role{claim.RoleMerger, claim.RoleArchitect} {
		told := r.awaitHeard(role, 1, bound)
		if !strings.HasPrefix(told[0].Message, prefix) || !strings.Contains(told[0].Message, strconv.Quote(conflictRequiredCheck)) {
			t.Fatalf("the %s was told %q after the settlement, want the settled checks of #%d naming %s", role, told[0].Message, r.number, conflictRequiredCheck)
		}
		r.note("read back: the %s's role topic took — %s", role, told[0].Message)
	}
	r.published(r.key)
	time.Sleep(liveSettle)
	want := maps.Clone(r.heard)
	want[claim.RoleMerger]++
	want[claim.RoleArchitect]++
	if heard := r.heardOn(r.key); !maps.Equal(heard, want) {
		t.Fatalf("the role topics of %s took %v messages after the settlement, want %v: the merger and the architect once each, the implementer nothing", r.key, heard, want)
	}
	implementerToken := string(r.imp.token)
	if lines := r.logged.matching(func(kept loggedRecord) bool {
		return strings.HasPrefix(kept.Message, "outbox role message") && kept.Attrs["role"] == implementerToken
	}); len(lines) != 0 {
		t.Fatalf("the daemon logged %q for the implementer's role, want no role message to it in merging", lines[0].Message)
	}
	r.note("read back over %s: the implementer's role topic took nothing and the daemon logged no role message to it; nothing else reached any role topic", liveSettle)
	if got := r.issue(r.key); got.Phase != phase.Merging {
		t.Fatalf("%s is in %s after the red settlement, want merging: a CI result moves no issue", r.key, got.Phase)
	}
	r.note("read back: %s is still in merging", r.key)
}

// Step 5. GitHub merges the pull request at the merger's head, which closes the issue: done. Then
// what held over the whole run: the moves the daemon logged for the issue went from accepting to
// retro, to merging and to done, and no other; every read of GET /legion/v1/state the watch made
// showed the issue in those phases, in that order, and never in implementing; the daemon wrote no
// in_progress status to Dispatch; and every merge-forward message it delivered or ended went to the
// merger or the reviewer, none to the implementer.
//
// The order of the phases is read from the daemon's log, not from the watch. The daemon logs each
// move its transition table makes once the move commits (phaseChangedLine). The watch reads the
// state ten times a second, and the driver completes retro at once, so retro can fall between two
// of its reads (phaseWatch). The watch still holds what the route showed.
func (r *conflictMergerLive) mergeClosesTheIssue() {
	t := r.t
	r.waitProcessed(r.publishMergeOf(r.number, r.key, mergerMergeHead, conflictBase))
	r.note("drove GitHub's merge of #%d at %s onto %s", r.number, mergerMergeHead, conflictBase)
	done := r.waitFor("the issue done", func(got api.Issue) bool { return got.Phase == phase.Done })
	r.note("read back: %s is %s, status %s", r.key, done.Phase, done.Status)

	var moves []string
	testwait.WithinPolling(t, testwait.Bounded(t, time.Minute, time.Second), 100*time.Millisecond, "the daemon's line for "+r.key+"'s move to done", func() bool {
		moves = r.phaseMoves()
		return len(moves) > 0 && strings.HasSuffix(moves[len(moves)-1], " to "+string(phase.Done))
	})
	if want := []string{"accepting to retro", "retro to merging", "merging to done"}; !slices.Equal(moves, want) {
		t.Fatalf("the daemon logged the moves %v of %s, want exactly %v: never implementing", moves, r.key, want)
	}
	reads, failed, seen := r.watch.stop()
	if reads == 0 {
		t.Fatalf("the watch of %s's phase read GET /legion/v1/state no time (%d reads failed): it saw nothing, so it says nothing", r.key, failed)
	}
	order := []phase.Phase{phase.Accepting, phase.Retro, phase.Merging, phase.Done}
	if seen[0] != phase.Accepting || seen[len(seen)-1] != phase.Done || !inOrder(seen, order) {
		t.Fatalf("the watch of %s read the phases %v over %d reads, want accepting first, done last and each in the order %v: never implementing",
			r.key, seen, reads, order)
	}
	r.note("read back over the whole run: the daemon logged the moves %v of %s, and %d reads of GET /legion/v1/state (%d failed) showed it in %v, in that order and never in implementing",
		moves, r.key, reads, failed, seen)
	var statuses []string
	for _, write := range r.board.writes() {
		if write.Issue == r.key && write.Status != "" {
			statuses = append(statuses, write.Status)
		}
	}
	if len(statuses) == 0 || slices.Contains(statuses, "in_progress") {
		t.Fatalf("the daemon wrote the statuses %v of %s to Dispatch, want some and no in_progress", statuses, r.key)
	}
	r.note("read back: the daemon wrote the statuses %v of %s to Dispatch, none of them in_progress", statuses, r.key)
	r.noMergeForwardTo(claim.RoleImplementer)
}

// Step 6. The merger's prompt as its launch carries it: the role prompt the daemon composed at
// boot, read from the paths the merger's launch named rather than from any prompt the driver wrote.
// Its section on a head that conflicts with its base, and the subsection on a check that failed,
// are printed whole, beside the messages step 2, 3 and 4 printed, for a reader to hold the one
// against the other.
func (r *conflictMergerLive) mergerPromptAsLaunched() {
	t := r.t
	token := r.token(claim.RoleMerger)
	launch := lastLaunch(t, r.rt, token)
	override := rolePromptPath(r.cfg.StateDir, token)
	var text strings.Builder
	for _, path := range launch.Prompt.RolePromptPaths {
		if path == override {
			t.Fatalf("the merger's launch names the driver's own prompt %s, want the role prompt the daemon composed", path)
		}
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read the merger's role prompt %s: %v", path, err)
		}
		text.Write(body)
	}
	r.note("read back: the merger's launch names %d role prompt parts the daemon composed at boot, %d bytes in all", len(launch.Prompt.RolePromptPaths), text.Len())
	composed := text.String()
	start := strings.Index(composed, mergerPromptSections[0]+"\n")
	if start < 0 {
		t.Fatalf("the merger's composed prompt has no section %q", mergerPromptSections[0])
	}
	section := composed[start:]
	if end := strings.Index(section[len(mergerPromptSections[0]):], "\n## "); end >= 0 {
		section = section[:len(mergerPromptSections[0])+end]
	}
	if !strings.Contains(section, "\n"+mergerPromptSections[1]+"\n") {
		t.Fatalf("the merger's section %q holds no subsection %q", mergerPromptSections[0], mergerPromptSections[1])
	}
	for _, line := range strings.Split(strings.TrimSpace(section), "\n") {
		r.note("prompt| %s", line)
	}
}

// noMergeForwardTo holds that no merge-forward message of the issue was addressed to role: the
// daemon logs every such message it delivers or ends undelivered with the role it names, and no
// line of the whole run names role. The control is the merger's own delivery, which must be logged.
func (r *conflictMergerLive) noMergeForwardTo(role claim.Role) {
	t := r.t
	t.Helper()
	lines := r.logged.matching(func(kept loggedRecord) bool {
		return strings.HasPrefix(kept.Message, "outbox merge-forward message") && kept.Attrs["issue"] == r.key
	})
	var roles []string
	for _, line := range lines {
		named := fmt.Sprint(line.Attrs["role"])
		if named == string(role) {
			t.Fatalf("the daemon logged %q naming the %s for %s, want no merge-forward message to it", line.Message, role, r.key)
		}
		roles = append(roles, named+" "+fmt.Sprint(line.Attrs["step"]))
	}
	if !slices.Contains(roles, string(claim.RoleMerger)+" "+string(record.MergeForwardAsk)) {
		t.Fatalf("the daemon logged the merge-forward messages %v for %s, want the merger's ask among them: a log that names none proves nothing", roles, r.key)
	}
	r.note("read back: the daemon logged merge-forward messages of %s to %v, none to the %s", r.key, roles, role)
}

// pullRequest is the issue's pull request as the store records it, which no route shows whole: its
// conflicting head and its merge forward.
func (r *conflictMergerLive) pullRequest() record.PullRequest {
	r.t.Helper()
	var pr *record.PullRequest
	r.write("nothing: a read of the pull request of "+r.key, func(tx pgx.Tx) error {
		var err error
		pr, err = r.records.MergePullRequest(context.Background(), tx, r.key)
		return err
	})
	if pr == nil {
		r.t.Fatalf("the store records no pull request for %s", r.key)
	}
	return *pr
}

// phaseChangedLine is the line the daemon logs for each move of an issue its transition table
// makes, once the move commits (workflow's arrive), naming the phase it left (`from`) and the
// phase it entered (`to`). A parent held before its retro (holdBeforeRetro) is logged with its
// retro as `to` while it stands held. No other write of an issue's phase logs the line: a failed
// claim's hold (claimFailed) and its release (retryPhase); a stopped task's hold (stopTask), its
// restart at its first step (restartTask) and its wait for that step again (waitForFirstStep); a
// held parent's start of its retro (startHeldRetro); a lingering tree's member closed at its merge
// (closeMerged); a leave that writes done (endTaskWithLeave) and a member an adoption parks at done
// (moveIntoTree); a root's move into awaiting_person (enterAwaiting) and admission's move of it
// back to admitted once its wait is answered; and the record of an issue at admitted: a new root's,
// a re-admitted root's and the record of a child that enters its tree (enterChild).
//
// The run moves no phase through any of them. Its root is seeded in accepting with no child, and
// the driver sends no stop, plan, retry, adoption or Dispatch event. GitHub's merge closes the root
// through the table, because its tree lingers only once the root has left. The root's leave that
// follows does reach one of them: beginLinger runs endTaskWithLeave, which writes done over the
// done the move wrote and so changes no phase. The driver scripts no failure of the fake runtime
// either. A claim that failed anyway holds the issue only where its role works the issue's phase
// and is not an architect (claimFailed): here the implementer's in retro and the merger's in
// merging. A failed claim of any other role holds nothing. A hold in retro fails the driver before
// retro ends, since the daemon refuses the retro completion of a held issue, and a hold from
// merging before step 4's last read of the issue fails a read that wants merging. The one release
// of a hold in the run is GitHub's merge of an issue held from merging (closeMerged), which the
// daemon logs as merging to done. So a hold from merging after step 4's last read leaves the three
// moves in the log, and step 5's check of the watch refuses it only where one of the watch's reads
// fell inside it.
const phaseChangedLine = "workflow: phase changed"

// phaseMoves is each move of the issue the daemon logged (phaseChangedLine), oldest first, as
// "<from> to <to>".
func (r *conflictMergerLive) phaseMoves() []string {
	var moves []string
	for _, line := range r.loggedFor(phaseChangedLine, r.key) {
		moves = append(moves, fmt.Sprintf("%v to %v", line.Attrs["from"], line.Attrs["to"]))
	}
	return moves
}

// inOrder says whether each phase of seen stands in order after the phase seen before it.
func inOrder(seen, order []phase.Phase) bool {
	rest := order
	for _, p := range seen {
		at := slices.Index(rest, p)
		if at < 0 {
			return false
		}
		rest = rest[at+1:]
	}
	return true
}

// phaseWatch reads GET /legion/v1/state ten times a second, on a goroutine of its own, from the
// boot until stop, and once more after stop, so the phase the caller saw before it stopped the
// watch is the last one kept. It keeps each phase the issue showed, in the order it showed them, a
// phase shown again in a row kept once. Its reads are samples: a phase shorter than a tenth of a
// second can fall between two of them, so it says what the route showed, not every phase the
// issue entered; the order of the moves comes from the daemon's log (phaseMoves). It reads through
// a client of its own and fails no test itself: a read that fails is counted, and stop answers how
// many reads were made and failed, so a watch that read nothing says so rather than passing.
type phaseWatch struct {
	stopping chan struct{}
	stopped  chan struct{}
	mu       sync.Mutex
	reads    int
	failed   int
	seen     []phase.Phase
}

// watchPhase starts the watch of issue's phase on d, stopped by stop or, at the latest, as the test
// ends, before the daemon stops.
func watchPhase(t *testing.T, d *daemon, issue string) *phaseWatch {
	w := &phaseWatch{stopping: make(chan struct{}), stopped: make(chan struct{})}
	client := &http.Client{Timeout: 2 * time.Second}
	go func() {
		defer close(w.stopped)
		tick := time.NewTicker(100 * time.Millisecond)
		defer tick.Stop()
		for {
			w.read(client, d.base, issue)
			select {
			case <-w.stopping:
				w.read(client, d.base, issue)
				return
			case <-tick.C:
			}
		}
	}()
	t.Cleanup(func() { w.stop() })
	return w
}

// read is one read of the state, which keeps the issue's phase when it differs from the last kept.
func (w *phaseWatch) read(client *http.Client, base, issue string) {
	var state api.State
	response, err := client.Get(base + "/legion/v1/state")
	if err == nil {
		err = json.NewDecoder(response.Body).Decode(&state)
		response.Body.Close()
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	got, ok := state.Issues[issue]
	if err != nil || !ok {
		w.failed++
		return
	}
	w.reads++
	if n := len(w.seen); n == 0 || w.seen[n-1] != got.Phase {
		w.seen = append(w.seen, got.Phase)
	}
}

// stop ends the watch, once, and answers how many reads it made, how many failed, and each phase
// it saw.
func (w *phaseWatch) stop() (int, int, []phase.Phase) {
	select {
	case <-w.stopping:
	default:
		close(w.stopping)
	}
	<-w.stopped
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.reads, w.failed, slices.Clone(w.seen)
}
