//go:build e2e

// The live proof's driver for the one thing that tells a head that stopped from a head that is
// moving: the `mergeWait` object an issue's view of GET /legion/v1/state carries while its review
// round waits on GitHub's answer about merging the pull request's head (api.MergeWaitView, built
// by projection's mergeWaitView over classify.RoundWaitsOnMergeState). The object names the head
// the round waits on, the merge state the record holds for it, when that state was last read
// (record.PullRequest.StateReadAt), when GitHub last moved the pull request, how many of the
// fence's check runs are the head's own, and the head and state the architect and the reviewer
// were told the round was stuck at. scripts/e2e/merge-wait-readable.sh builds this driver with
// `-tags e2e` and owns the nats-server and the PostgreSQL it runs against.
//
// The step the proof exists for is the second: a read that only confirms the state already
// recorded still moves `stateReadAt`, so a head read on every pass reads differently from one
// nothing has read for a day. A daemon whose confirming read applied nothing would leave the two
// one picture, which is what the proof's control (the early return in applyMergeability put back)
// shows by failing at that step.
//
// The daemon is the whole daemon: run, the function `legion start` calls, through this package's
// own whole-daemon harness (startDaemonWithin), so the proof exercises the real configuration, the
// real store and its migrations, the real HTTP API, the real intake consumers, the real review
// round and its stuck notice, the real merge-state ladder (daemon's readMergeStates) and the real
// state build. Facts enter only as production's do: GitHub envelopes on the ENVOY_NOTIFICATIONS
// stream for the CI settlements, the reviews, the push, the synchronize and the pushes onto the
// base that ask the daemon to read a head again. Every assertion is a read of GET
// /legion/v1/state — of the decoded object for what it says, and of the body's own keys for what
// it leaves off, which is what the negative control holds.
//
// What the proof stands in for lies outside the rule. The broker is a native nats-server the
// script starts and names in LEGION_E2E_NATS_URL. GitHub is mergeWaitGitHub, a REST stand-in for
// the two pull requests the proof drives, whose answer for a head — `mergeable`,
// `mergeable_state`, `head.sha`, `base.ref` and `updated_at` — the steps set between them, and
// which can be made to answer nothing at all for a head, the state a round is in before its first
// answering read. Dispatch is the board the package's whole-daemon live proofs share
// (liveDispatch, in live_proof_test.go), and Envoy's listener the package's noticeListener. The
// two Apps' tokens are workflowTokenRecorder's placeholders, which is also where the review App's
// login comes from: appLoginsAtBoot asks the lease for it and gets `legion-review[bot]`, the
// reviewAppLogin this package already spells (pull_request_reads_test.go), so the reviews this
// proof publishes are the reviewer's own (workflow's byReviewer, with no reviewer session
// recorded). No agent runs: the fake runtime launches whatever claim a move starts, and the proof
// acts as no role. The three issues, their pull requests and their slots are seeded through the
// store, as every daemon test seeds its fixture; what is driven is the change.
package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

// mergeWaitBase is the base branch every pull request of the proof sits on, the repository's
// default: the branch a push onto it asks the daemon to read every open pull request's head again
// (workflow's basePush), which is how a step asks for a read without changing what the round is.
const mergeWaitBase = "main"

// The two heads the proof drives: the head the round is approved at, and the head a push moves it
// to. They are full commit SHAs because a review the review App submits decides a round only when
// the head its Legion footer names is the pull request's current head, and the daemon reads no
// shorter spelling there (intake.PullRequestReview.LegionHead, over fullCommitSHA).
const (
	mergeWaitFirstHead  = "4c1d0b9a7e52f38610d4c2b7a9e5f0318d6b2c47"
	mergeWaitSecondHead = "8b3e5f1c9d04a7268f1b3c5d7e9a0b2c4d6e8f13"
)

// The head of each issue the negative control reads: the issue whose round waits on nothing, and
// the issue whose pull request the record does not hold open. Both are full SHAs for the same
// reason the two above are.
const (
	mergeWaitMovingHead = "0d7a4e61b9c3528f0a1b2c3d4e5f60718293a4b5"
	mergeWaitClosedHead = "f3e2d1c0b9a8978685746352413f2e1d0c9b8a79"
)

// mergeWaitReadableLive is one run of the proof: the booted daemon, the three issues it reads, the
// GitHub stand-in whose answers the steps set, and the counters that keep each envelope its own
// fact.
type mergeWaitReadableLive struct {
	liveProof
	rt     *fake.Runtime
	github *mergeWaitGitHub
	// stuck is the issue whose round waits on GitHub; moving the negative control whose round
	// waits on nothing, its reviewer having requested changes; and closed the negative control
	// whose pull request the record does not hold open. Each is its own tree, and each carries
	// the run's own project, since the store a pod re-runs the proof against holds every earlier
	// run's issues.
	stuck, moving, closed string
	// The pull request numbers of those three, the run's own: a fact names a pull request by its
	// repository and number, and the store holds every earlier run's.
	stuckNumber, movingNumber, closedNumber int
	// openedAt is GitHub's clock for the pull requests as they are seeded, and pushedAt its clock
	// for the synchronize that moves the stuck issue's head: the lifecycle fence orders by GitHub's
	// own clock (classify.LateLifecycle), so the second is after the first.
	openedAt, pushedAt time.Time
	// seq numbers the envelopes, so no two carry one event id, and reviewID numbers the reviews,
	// so each is newer than the one before it (record.ReviewOrder).
	seq      int64
	reviewID int64
}

// TestTheMergeWaitSaysWhatAReviewRoundWaitsOnFromGitHubLive drives the six steps of the proof
// against one booted daemon, in the order each step's state follows from the last: the wait
// appearing on an approved head GitHub will not merge from, a read that confirms that answer and
// moves nothing but the stamp, a read that changes the answer, a push that moves the head and the
// approval that follows it, the two issues the same state carries no wait for at all, and the
// answer that ends the wait.
func TestTheMergeWaitSaysWhatAReviewRoundWaitsOnFromGitHubLive(t *testing.T) {
	r := newMergeWaitReadableLive(t)
	r.step("merge-wait-appears", r.mergeWaitAppears)
	r.step("a-confirming-read-moves-the-stamp", r.aConfirmingReadMovesTheStamp)
	r.step("a-changing-read", r.aChangingRead)
	r.step("a-push-moves-the-head", r.aPushMovesTheHead)
	r.step("the-negative-control", r.theNegativeControl)
	r.step("the-wait-ends", r.theWaitEnds)
}

// newMergeWaitReadableLive boots the daemon over the script's broker with the three issues already
// recorded, each in reviewing and holding its tree's admission slot: the one the proof drives, on
// an open pull request GitHub answers `blocked` for; the one whose reviewer requests changes in
// step 5; and the one whose pull request the record holds closed, its reviewer's approval standing
// at the head it was closed on.
func newMergeWaitReadableLive(t *testing.T) *mergeWaitReadableLive {
	t.Helper()
	proof := newLiveProof(t, "scripts/e2e/merge-wait-readable.sh")
	number, err := strconv.ParseInt(randomSuffix(t)[:6], 16, 64)
	if err != nil {
		t.Fatal(err)
	}
	opened := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	r := &mergeWaitReadableLive{
		liveProof: proof, rt: fake.NewRuntime(),
		stuck: proof.cfg.Project + "-1", moving: proof.cfg.Project + "-2", closed: proof.cfg.Project + "-3",
		stuckNumber: int(number) + 1, movingNumber: int(number) + 2, closedNumber: int(number) + 3,
		openedAt: opened, pushedAt: opened.Add(30 * time.Minute),
	}
	r.github = newMergeWaitGitHub(t)
	// GitHub answers `mergeable: true` for a head it holds on a check or a review the repository
	// requires as surely as for one it would merge now, which is what makes `mergeable_state` the
	// only answer that tells those two apart (record.PullRequest.MergeState).
	r.github.open(r.stuckNumber, "legion/"+r.stuck, mergeWaitFirstHead, true, "blocked", r.openedAt)
	r.github.open(r.movingNumber, "legion/"+r.moving, mergeWaitMovingHead, true, "blocked", r.openedAt)
	r.seed()
	// The runtime provisions each claim's workspace itself (fake.Runtime's InPod), so the start a
	// move makes reaches the fake's launch without the daemon cloning acme/widgets from github.com.
	r.rt.InPod = true
	o := fakeRuntime(r.rt, &built{})
	o.workflowTokens = &workflowTokenRecorder{}
	o.githubAPI = r.github.url
	r.d = startDaemonWithin(t, r.cfg, o, r.log(), 60*time.Second)
	return r
}

// seed records the three issues as the daemon would hold them at the moment their reviewers took
// the work: each a root handed to Legion, in reviewing, holding its tree's admission slot, its
// lifecycle open under the workflow, on a pull request of its own. The reviewer's row is left
// unwritten for the two whose reviews the proof publishes, since the review itself writes it; the
// closed issue's is seeded with the approval that stands at its head, because no review of it is
// published and its approval is the whole of what makes the rule say its round waits.
func (r *mergeWaitReadableLive) seed() {
	t := r.t
	t.Helper()
	ctx := context.Background()
	for _, key := range []string{r.stuck, r.moving, r.closed} {
		r.board.show(key, "in_progress")
	}
	r.write("the three issues, their pull requests and their slots", func(tx pgx.Tx) error {
		for i, seeded := range []struct {
			key    string
			number int
			head   string
			state  record.PullRequestState
		}{
			{r.stuck, r.stuckNumber, mergeWaitFirstHead, record.PullRequestOpen},
			{r.moving, r.movingNumber, mergeWaitMovingHead, record.PullRequestOpen},
			{r.closed, r.closedNumber, mergeWaitClosedHead, record.PullRequestClosed},
		} {
			if err := r.records.PutIssue(ctx, tx, record.Issue{
				Key: seeded.key, Repo: liveRepo, DefaultBranch: mergeWaitBase, GitHubRepo: liveRepo, Tree: seeded.key,
				Project: r.cfg.Project, Title: "the issue whose approval waits on GitHub", Phase: phase.Reviewing,
				Generation: 1, Status: "in_progress", DispatchStatus: "in_progress", Rank: fmt.Sprintf("U%d", i),
				LastDispatchSeq: 1, HandedOver: true,
			}); err != nil {
				return err
			}
			if err := r.records.PutPullRequest(ctx, tx, record.PullRequest{
				State: seeded.state, Issue: seeded.key, Repo: liveRepo, Number: seeded.number,
				Branch: "legion/" + seeded.key, HeadSHA: seeded.head, Base: mergeWaitBase,
				HeadUpdatedAt: r.openedAt, Failing: []string{},
			}); err != nil {
				return err
			}
		}
		if err := r.records.PutPhase(ctx, tx, record.PhaseRow{Issue: r.closed, Role: claim.RoleReviewer,
			Decision: &record.ReviewDecision{State: "approved", Head: mergeWaitClosedHead}}); err != nil {
			return err
		}
		return r.admit(ctx, tx, r.stuck, r.moving, r.closed)
	})
	for _, key := range []string{r.stuck, r.moving, r.closed} {
		if _, err := r.st.OpenTreeLifecycle(ctx, r.project, key, treelifecycle.AuthorityWorkflow); err != nil {
			t.Fatalf("open the lifecycle of %s: %v", key, err)
		}
		// Every role a move of these issues could launch needs its prompt on disk, as the daemon's
		// own launch writes it: step 5's request for changes starts an implementer.
		for _, role := range claim.Roles {
			writePrompt(t, r.cfg, r.token(key, role))
		}
	}
	r.note("seeded: %s in reviewing on open pull request #%d at %s, %s in reviewing on open #%d at %s, and %s in reviewing with #%d recorded closed at %s, its reviewer's approval standing there; base %s, GitHub's clock %s",
		r.stuck, r.stuckNumber, mergeWaitShort(mergeWaitFirstHead), r.moving, r.movingNumber, mergeWaitShort(mergeWaitMovingHead),
		r.closed, r.closedNumber, mergeWaitShort(mergeWaitClosedHead), mergeWaitBase, r.openedAt.Format(time.RFC3339))
}

// write runs body in one transaction against the daemon's own database, failing the test with what
// it was seeding on an error.
func (r *mergeWaitReadableLive) write(what string, body func(tx pgx.Tx) error) {
	r.t.Helper()
	if err := r.st.Tx(context.Background(), body); err != nil {
		r.t.Fatalf("seed %s: %v", what, err)
	}
}

// token is the claim token of issue's role, as the daemon spells it.
func (r *mergeWaitReadableLive) token(issue string, role claim.Role) claim.Token {
	r.t.Helper()
	token, err := claim.NewToken(r.project, issue, role)
	if err != nil {
		r.t.Fatal(err)
	}
	return token
}

// Step 1. The round waits, and the state says what on. CI settles the head's own two checks, every
// run GitHub reported on it concluded, and the reviewer approves that head, which GitHub answers
// `blocked` for: the round is one the daemon reads GitHub for, so the issue's view carries a
// mergeWait naming the head, the state, when it was read, when GitHub last moved the pull request
// and how many of the fence's runs are this head's own; and, once the daemon has told the
// architect and the reviewer the round is stuck, the head and state it told them of.
func (r *mergeWaitReadableLive) mergeWaitAppears() {
	t := r.t
	r.publishChecks(r.stuckNumber, mergeWaitFirstHead, "lint", "unit")
	r.note("drove CI's settlement of #%d at %s: lint and unit reported, both concluded, so every check GitHub reported on the head is over (classify.ChecksConcludedOnHead)",
		r.stuckNumber, mergeWaitShort(mergeWaitFirstHead))
	r.publishReview(r.stuckNumber, "approved", mergeWaitFirstHead)
	r.note("drove the reviewer's APPROVE of %s, the head the pull request stands at, while GitHub answers that head mergeable_state %q",
		mergeWaitShort(mergeWaitFirstHead), "blocked")

	wait := r.waitForWait(r.stuck, "the merge wait of "+r.stuck+" with the stuck round told of",
		func(got *api.MergeWaitView) bool { return got != nil && got.ToldHead != "" })
	if wait.WaitsOn != mergeWaitFirstHead || wait.MergeState != "blocked" {
		t.Fatalf("%s waits on %s/%s, want the approved head %s blocked", r.stuck, wait.WaitsOn, wait.MergeState, mergeWaitFirstHead)
	}
	if wait.StateReadAt == nil {
		t.Fatalf("%s's merge wait carries no stateReadAt, want the moment of the read that answered blocked", r.stuck)
	}
	if wait.HeadUpdatedAt == nil || !wait.HeadUpdatedAt.Equal(r.openedAt) {
		t.Fatalf("%s's merge wait carries headUpdatedAt %v, want GitHub's clock for the pull request, %v", r.stuck, wait.HeadUpdatedAt, r.openedAt)
	}
	if wait.CheckRuns != 2 {
		t.Fatalf("%s's merge wait counts %d check runs of the head, want the head's own two", r.stuck, wait.CheckRuns)
	}
	if wait.ToldHead != mergeWaitFirstHead || wait.ToldState != "blocked" {
		t.Fatalf("%s's merge wait was told stuck at %s/%s, want %s blocked", r.stuck, wait.ToldHead, wait.ToldState, mergeWaitFirstHead)
	}
	issue := r.issue(r.stuck)
	r.note("read back: %s is in %s, its pull request #%d at %s with mergeState %q, and its mergeWait is %s",
		r.stuck, issue.Phase, issue.PullRequest.Number, mergeWaitShort(issue.PullRequest.Head), issue.PullRequest.MergeState, mergeWaitLine(wait))
	var notices []string
	testwait.Eventually(t, "the review-stuck notice on "+r.stuck+"'s architect's topic", func() bool {
		notices = r.stuckNotices(r.stuck)
		return len(notices) == 1
	})
	r.note("read back: the architect was told once that the round is stuck — %s", notices[0])
}

// Step 2. A read that confirms what is recorded moves the stamp and nothing else. Nothing has
// asked the daemon to read since step 1, so the stamp stands where that step left it; a
// settlement of the same head — its two checks re-run and concluded the same way — asks for a
// read of the round again (workflow's askMergeState, which Apply calls after every such fact,
// whatever the handler recorded), GitHub answers the same `blocked` about the same head, and the
// only thing the record takes from that answer is when it was read.
//
// This is the sentence the proof exists for, and the read is driven by a fact that moves nothing
// else for that reason: the head stands, the round stands, GitHub's answer stands and the base has
// not moved under the head, which is the read a daemon that applied only changing answers would
// drop. A head read on every pass would then be indistinguishable from one nothing had read for a
// day (dispatch://INFRA-3027).
//
// A moved stamp alone would not say which read moved it: a read step 1 asked for can reach the
// record after this step samples the stamp, and the move would then be that older read's. So the
// step samples the stand-in's count of the reads it has answered for this pull request
// (mergeWaitPull.answered) beside the stamp, and holds that the count rose. What that proves is
// that GitHub served the answer behind the moved stamp after the sample, so no read step 1 asked
// for and GitHub had already answered can stand for this step's assertion.
func (r *mergeWaitReadableLive) aConfirmingReadMovesTheStamp() {
	t := r.t
	// The read the review of step 1 asked for lands behind that step's own reads, so the stamp is
	// taken once the step's own facts have been answered for.
	time.Sleep(liveSettle)
	before := r.mergeWaitOf(r.stuck)
	beforeAnswered := r.github.answeredReads(r.stuckNumber)
	r.note("the stamp step 1 left: stateReadAt %s, mergeState %q, waiting on %s, with %d reads of the pull request answered so far",
		before.StateReadAt.Format(time.RFC3339Nano), before.MergeState, mergeWaitShort(before.WaitsOn), beforeAnswered)

	r.publishChecks(r.stuckNumber, mergeWaitFirstHead, "lint", "unit")
	r.note("drove CI's settlement of the same head again, lint and unit re-run and concluded the same way, which asks the daemon to read GitHub for the round again; GitHub answers the same %q about the same head, the base has not moved under it, and nothing the wait is judged by has changed",
		"blocked")
	wait := r.waitForWait(r.stuck, "a read of "+r.stuck+" later than "+before.StateReadAt.Format(time.RFC3339Nano),
		func(got *api.MergeWaitView) bool {
			return got != nil && got.StateReadAt != nil && got.StateReadAt.After(*before.StateReadAt)
		})
	answered := r.github.answeredReads(r.stuckNumber)
	if answered <= beforeAnswered {
		t.Fatalf("the stamp of %s moved while GitHub had answered %d reads, the %d it had answered before this step drove anything: the move must stand on a read answered after the stamp was sampled, not on a read of step 1's reaching the record late",
			r.stuck, answered, beforeAnswered)
	}
	if wait.MergeState != before.MergeState || wait.WaitsOn != before.WaitsOn {
		t.Fatalf("the confirming read left %s waiting on %s/%s, want the %s/%s it was waiting on: the answer did not change",
			r.stuck, wait.WaitsOn, wait.MergeState, before.WaitsOn, before.MergeState)
	}
	if wait.ToldHead != before.ToldHead || wait.ToldState != before.ToldState {
		t.Fatalf("the confirming read left %s told stuck at %s/%s, want the %s/%s it was told at: one finding is told once",
			r.stuck, wait.ToldHead, wait.ToldState, before.ToldHead, before.ToldState)
	}
	r.note("read back: stateReadAt moved from %s to %s, %s later, on a read GitHub answered after that first stamp was sampled (%d reads answered, %d before), with mergeState still %q, waitsOn still %s, checkRuns still %d and the told marks unmoved — the read confirmed the record and the stamp is the only thing it wrote",
		before.StateReadAt.Format(time.RFC3339Nano), wait.StateReadAt.Format(time.RFC3339Nano),
		wait.StateReadAt.Sub(*before.StateReadAt), answered, beforeAnswered,
		wait.MergeState, mergeWaitShort(wait.WaitsOn), wait.CheckRuns)
	if notices := r.stuckNotices(r.stuck); len(notices) != 1 {
		t.Fatalf("the architect was told %d times the round is stuck, want the one: a read that confirms the finding tells nobody again", len(notices))
	}
	r.note("read back: the architect has still been told once — the mark on the reviewer's decision is what deduplicates it, not the answer GitHub gave")
}

// Step 3. A read that changes the answer moves both. GitHub now answers `behind`, the other state
// an issue may not advance on that a push onto the base is the natural cause of, and the next read
// writes the state and the stamp together.
func (r *mergeWaitReadableLive) aChangingRead() {
	t := r.t
	before := r.mergeWaitOf(r.stuck)
	r.github.answers(r.stuckNumber, true, "behind")
	r.publishBasePush()
	r.note("drove the proof's first push onto %s, GitHub now answering the head %q: its base moved under it, which GitHub sends no event for", mergeWaitBase, "behind")

	wait := r.waitForWait(r.stuck, "the merge wait of "+r.stuck+" reading behind",
		func(got *api.MergeWaitView) bool { return got != nil && got.MergeState == "behind" })
	if wait.StateReadAt == nil || !wait.StateReadAt.After(*before.StateReadAt) {
		t.Fatalf("the changing read left %s stamped %v, want a moment after %v", r.stuck, wait.StateReadAt, before.StateReadAt)
	}
	if wait.WaitsOn != mergeWaitFirstHead {
		t.Fatalf("%s waits on %s after the changing read, want the same head %s: only GitHub's answer about it changed", r.stuck, wait.WaitsOn, mergeWaitFirstHead)
	}
	if wait.ToldState != "behind" || wait.ToldHead != mergeWaitFirstHead {
		t.Fatalf("%s was told stuck at %s/%s after the changing read, want %s behind: a new state at the same head is a new finding",
			r.stuck, wait.ToldHead, wait.ToldState, mergeWaitFirstHead)
	}
	r.note("read back: mergeState moved %q → %q and stateReadAt %s → %s, on the same head %s, and the architect was told of the new state (toldState %q)",
		before.MergeState, wait.MergeState, before.StateReadAt.Format(time.RFC3339Nano), wait.StateReadAt.Format(time.RFC3339Nano),
		mergeWaitShort(wait.WaitsOn), wait.ToldState)
}

// Step 4. A push moves the head, and the round waits again only once the reviewer has approved the
// new head: a push above the approved head ends the approval (classify.ApprovalStands), so between
// the two there is no round waiting and no object at all. At the new head the object is the new
// head's: `waitsOn` is it, `headUpdatedAt` is the synchronize's clock, `checkRuns` counts the new
// head's own runs rather than the old head's, and `stateReadAt` is absent until a read answers for
// it — which is held by making GitHub answer nothing for the head, and then released.
func (r *mergeWaitReadableLive) aPushMovesTheHead() {
	t := r.t
	r.github.withhold(r.stuckNumber, true)
	r.github.head(r.stuckNumber, mergeWaitSecondHead, r.pushedAt)
	r.publishPush(r.stuck, mergeWaitFirstHead, mergeWaitSecondHead)
	r.publishSynchronize(r.stuckNumber, r.stuck, mergeWaitSecondHead, r.pushedAt)
	r.note("drove the implementer's push of %s onto %s and GitHub's synchronize of #%d to it, with GitHub answering nothing at all for the new head",
		mergeWaitShort(mergeWaitSecondHead), mergeWaitShort(mergeWaitFirstHead), r.stuckNumber)

	moved := r.waitForIssue(r.stuck, "the pull request of "+r.stuck+" at the pushed head",
		func(got api.Issue) bool { return got.PullRequest != nil && got.PullRequest.Head == mergeWaitSecondHead })
	if moved.MergeWait != nil {
		t.Fatalf("%s carries a merge wait on %s after the push, want none: the push is above the approved head, so no approval stands and no round waits",
			r.stuck, moved.MergeWait.WaitsOn)
	}
	if carried := r.issuesJSON()[r.stuck]; keyCarried(t, carried, "mergeWait") {
		t.Fatalf("%s's view carries a mergeWait key after the push, want the key left off the object", r.stuck)
	}
	r.note("read back: the head is %s and %s carries no mergeWait key at all — the approval of %s no longer stands",
		mergeWaitShort(moved.PullRequest.Head), r.stuck, mergeWaitShort(mergeWaitFirstHead))

	r.publishChecks(r.stuckNumber, mergeWaitSecondHead, "unit")
	r.publishReview(r.stuckNumber, "approved", mergeWaitSecondHead)
	r.note("drove CI's settlement of the new head, one run of unit, and the reviewer's APPROVE of %s", mergeWaitShort(mergeWaitSecondHead))
	testwait.Eventually(t, "the daemon's read of the new head, which GitHub answers nothing for", func() bool {
		return r.github.refusals(r.stuckNumber) > 0
	})
	wait := r.waitForWait(r.stuck, "the merge wait of "+r.stuck+" at the new head",
		func(got *api.MergeWaitView) bool { return got != nil && got.WaitsOn == mergeWaitSecondHead })
	if wait.StateReadAt != nil {
		t.Fatalf("%s's merge wait at %s is stamped %v, want no stateReadAt: no read has answered for this head",
			r.stuck, mergeWaitSecondHead, wait.StateReadAt)
	}
	if wait.MergeState != "" {
		t.Fatalf("%s's merge wait at %s reads %q, want no merge state: no read has answered for this head", r.stuck, mergeWaitSecondHead, wait.MergeState)
	}
	if wait.HeadUpdatedAt == nil || !wait.HeadUpdatedAt.Equal(r.pushedAt) {
		t.Fatalf("%s's merge wait carries headUpdatedAt %v, want the synchronize's own clock %v", r.stuck, wait.HeadUpdatedAt, r.pushedAt)
	}
	if wait.CheckRuns != 1 {
		t.Fatalf("%s's merge wait counts %d check runs of %s, want the one run reported there: the old head's two are no runs of this one",
			r.stuck, wait.CheckRuns, mergeWaitSecondHead)
	}
	if wait.ToldHead != "" || wait.ToldState != "" {
		t.Fatalf("%s's merge wait was told stuck at %s/%s, want neither mark: the new approval replaced the decision the old marks lived on",
			r.stuck, wait.ToldHead, wait.ToldState)
	}
	r.note("read back, with %d read of the new head answered nothing: %s waits on %s, headUpdatedAt moved to %s, checkRuns is the new head's own %d where the old head's was 2, and there is no stateReadAt and no mergeState — nothing has answered for this head",
		r.github.refusals(r.stuckNumber), r.stuck, mergeWaitShort(wait.WaitsOn), wait.HeadUpdatedAt.Format(time.RFC3339), wait.CheckRuns)

	r.github.withhold(r.stuckNumber, false)
	r.github.answers(r.stuckNumber, true, "blocked")
	r.publishBasePush()
	answered := r.waitForWait(r.stuck, "the first answering read of "+mergeWaitSecondHead,
		func(got *api.MergeWaitView) bool { return got != nil && got.StateReadAt != nil })
	if answered.WaitsOn != mergeWaitSecondHead || answered.MergeState != "blocked" {
		t.Fatalf("the first read of the new head left %s waiting on %s/%s, want %s blocked", r.stuck, answered.WaitsOn, answered.MergeState, mergeWaitSecondHead)
	}
	r.note("read back: GitHub answered again and the stamp appeared — %s", mergeWaitLine(answered))
}

// Step 5. The negative control, read out of the same state body: an issue whose round waits on
// nothing and an issue whose pull request the record does not hold open each carry no mergeWait
// key at all, where the issue of the four steps above carries one. The absence is read from the
// body's own keys rather than from the decoded value, since a Go nil and a key written empty are
// the same value and only one of them is what the daemon writes.
func (r *mergeWaitReadableLive) theNegativeControl() {
	t := r.t
	r.publishReview(r.movingNumber, "changes_requested", mergeWaitMovingHead)
	r.note("drove the reviewer's REQUEST_CHANGES of %s at %s, which ends its round", r.moving, mergeWaitShort(mergeWaitMovingHead))
	rejected := r.waitForIssue(r.moving, "the issue "+r.moving+" back in implementing",
		func(got api.Issue) bool { return got.Phase == phase.Implementing })
	r.note("read back: %s is in %s — its round ended, so nothing of it waits on GitHub, whatever GitHub answers for its head (%q)",
		r.moving, rejected.Phase, rejected.PullRequest.MergeState)

	issues := r.issuesJSON()
	if !keyCarried(t, issues[r.stuck], "mergeWait") {
		t.Fatalf("%s carries no mergeWait key in the body the control is read from, want the wait of step 4", r.stuck)
	}
	for _, key := range []string{r.moving, r.closed} {
		if keyCarried(t, issues[key], "mergeWait") {
			t.Fatalf("%s carries a mergeWait key, want the key left off the object altogether: %s", key, issues[key]["mergeWait"])
		}
	}
	r.note("read back from one body: %s carries `mergeWait`, %s does not — its round ended — and %s does not either, though its reviewer's approval stands at the head its pull request was closed at, because the object is for an open pull request alone (projection's mergeWaitView, over record.PullRequestOpen)",
		r.stuck, r.moving, r.closed)
}

// Step 6. The wait ends when GitHub answers a state the issue may advance on. A settlement does
// not end it — on a head GitHub answers `blocked` for, the settlement that concludes the head's
// checks moves the round from waiting to stuck, and both of those wait — so what ends it is the
// read: GitHub answers `clean`, the approval that has stood throughout ends the round, and the
// issue leaves reviewing with no mergeWait key behind it.
func (r *mergeWaitReadableLive) theWaitEnds() {
	t := r.t
	before := r.mergeWaitOf(r.stuck)
	r.github.answers(r.stuckNumber, true, "clean")
	r.publishBasePush()
	r.note("drove one more push onto %s, GitHub now answering %s %q, a state record.MergeState.Mergeable holds true of",
		mergeWaitBase, mergeWaitShort(before.WaitsOn), "clean")

	advanced := r.waitForIssue(r.stuck, "the issue "+r.stuck+" out of reviewing",
		func(got api.Issue) bool { return got.Phase != phase.Reviewing })
	if advanced.Phase != phase.Accepting {
		t.Fatalf("%s is in %s after the read, want accepting: the approval that stood ends the round (TriggerReviewApproved)", r.stuck, advanced.Phase)
	}
	if advanced.MergeWait != nil {
		t.Fatalf("%s carries a merge wait on %s after the round ended, want none", r.stuck, advanced.MergeWait.WaitsOn)
	}
	if carried := r.issuesJSON()[r.stuck]; keyCarried(t, carried, "mergeWait") {
		t.Fatalf("%s's view carries a mergeWait key after the round ended, want the key left off the object", r.stuck)
	}
	r.note("read back: %s is in %s, its pull request #%d reads mergeState %q, and its view carries no mergeWait key — the round that waited on GitHub is over",
		r.stuck, advanced.Phase, advanced.PullRequest.Number, advanced.PullRequest.MergeState)
}

// stuckNotices is the reason of every review-stuck notice the daemon has published to key's
// architect. It is the harness's own architectNotices read (live_proof_test.go) with the one
// difference this proof needs: the architect's role topic also takes the plain sentence a CI
// settlement sends it (workflow's tellChecks), whose payload is a JSON string rather than a
// notice object, and a payload that is no object is passed over here rather than read as one.
func (r *mergeWaitReadableLive) stuckNotices(key string) []string {
	t := r.t
	t.Helper()
	architect, err := claim.NewToken(r.project, key, claim.RoleArchitect)
	if err != nil {
		t.Fatal(err)
	}
	var reasons []string
	for _, published := range r.notices.acceptedOn(notify.RoleTopicPrefix + string(architect)) {
		var payload struct {
			Kind   string `json:"kind"`
			Reason string `json:"reason"`
		}
		if err := json.Unmarshal([]byte(published.Payload), &payload); err != nil {
			continue
		}
		if payload.Kind == "review-stuck" {
			reasons = append(reasons, payload.Reason)
		}
	}
	return reasons
}

// mergeWaitOf is key's merge wait as the state shows it now, failing the test when the view
// carries none.
func (r *mergeWaitReadableLive) mergeWaitOf(key string) *api.MergeWaitView {
	r.t.Helper()
	issue := r.issue(key)
	if issue.MergeWait == nil {
		r.t.Fatalf("%s carries no mergeWait, want the wait its round is in", key)
	}
	return issue.MergeWait
}

// waitForWait polls the state until want is true of key's merge wait, which is nil while the view
// carries none, and returns what it last saw.
func (r *mergeWaitReadableLive) waitForWait(key, what string, want func(*api.MergeWaitView) bool) *api.MergeWaitView {
	r.t.Helper()
	var last *api.MergeWaitView
	testwait.Eventually(r.t, what, func() bool {
		last = r.issue(key).MergeWait
		return want(last)
	})
	return last
}

// waitForIssue polls the state until want is true of key, and returns what it last saw.
func (r *mergeWaitReadableLive) waitForIssue(key, what string, want func(api.Issue) bool) api.Issue {
	r.t.Helper()
	var last api.Issue
	testwait.Eventually(r.t, what, func() bool {
		last = r.issue(key)
		return want(last)
	})
	return last
}

// issuesJSON is every issue of the body GET /legion/v1/state answers now, each by its own keys.
// The negative control reads the body rather than the decoded state because api.Issue.MergeWait is
// `omitempty`: a nil pointer and a key written empty decode to the same Go value, and which of the
// two the daemon writes is the thing the control holds.
func (r *mergeWaitReadableLive) issuesJSON() map[string]map[string]json.RawMessage {
	t := r.t
	t.Helper()
	status, body := r.d.request(http.MethodGet, "/legion/v1/state", nil, false)
	if status != http.StatusOK {
		t.Fatalf("GET /legion/v1/state = %d %s, want 200", status, body)
	}
	var state struct {
		Issues map[string]map[string]json.RawMessage `json:"issues"`
	}
	if err := json.Unmarshal(body, &state); err != nil {
		t.Fatalf("decode the state body: %v", err)
	}
	return state.Issues
}

// keyCarried says whether an issue's own object in the state body carries key, failing the test
// when the body holds no such issue at all.
func keyCarried(t *testing.T, issue map[string]json.RawMessage, key string) bool {
	t.Helper()
	if issue == nil {
		t.Fatalf("the state body holds no object for the issue whose %s key was read", key)
	}
	_, carried := issue[key]
	return carried
}

// publishChecks publishes CI's settlement of the pull request at head, with one run of each name
// reported and none of them failing or cancelled, as Envoy relays the listener's settlement
// (intake's decodeChecks reads it, which takes the subject as part of the fact's identity). Each
// run reaches the fence stamped with the head it ran on and the conclusion the settlement's lists
// give it (classify's stampedRuns), which is what makes it a run of this head's own.
func (r *mergeWaitReadableLive) publishChecks(number int, head string, names ...string) {
	r.t.Helper()
	r.seq++
	// Each settlement reports runs of its own, so a head settled twice is the same checks re-run
	// rather than the same report again (classify's redundantSettlement): the fence keeps the
	// newest run of each name, so the count of the head's own runs stands where it was.
	runs := make([]map[string]any, 0, len(names))
	for i, name := range names {
		runs = append(runs, map[string]any{"name": name, "id": int64(number)*1000 + r.seq*10 + int64(i) + 1})
	}
	none := map[string]any{"count": 0, "checks": []string{}}
	payload, err := json.Marshal(map[string]any{
		"kind": "checks", "repo": liveRepo, "number": strconv.Itoa(number), "sha": head, "check_runs": runs,
		"generation": 0, "snapshot": fmt.Sprintf("snapshot-%s-%d", head[:8], r.seq), "settled_at": time.Now().UnixMilli(),
		"failed": none, "cancelled": none, "running": none, "queued": none, "skipped": none,
		"passed":         map[string]any{"count": len(names), "checks": names},
		"failing_checks": []any{},
	})
	if err != nil {
		r.t.Fatalf("encode the checks payload: %v", err)
	}
	subject := fmt.Sprintf("notifications.github.%s.pr.%d.checks", r.subjectRepo(), number)
	r.waitProcessed(r.publish(subject, "github", fmt.Sprintf("checks-%d-%d", number, r.seq), string(payload)))
}

// publishReview publishes the reviewer's review of the pull request at head, as Envoy relays a
// GitHub pull-request review (intake's decodeReview reads it). It is the review App's own, with
// the Legion footer naming the head it read, which is what lets it decide the round at all
// (workflow's refusesRound); no reviewer session is recorded on these issues, so the review App's
// login decides by itself (workflow's byReviewer). Each review carries an id and a time after the
// one before it, so none is ordered behind a review already applied (record.ReviewOrder).
func (r *mergeWaitReadableLive) publishReview(number int, state, head string) {
	r.t.Helper()
	r.seq++
	r.reviewID++
	payload, err := json.Marshal(map[string]string{
		"kind": "review", "repo": liveRepo, "number": strconv.Itoa(number), "action": "submitted",
		"state": state, "commit_id": head, "head_sha": head, "author": reviewAppLogin,
		"body":           "the reviewer's " + state + " of " + head + legionFooter("ses-reviewer", head),
		"body_truncated": "false", "review_id": strconv.FormatInt(1000+r.reviewID, 10),
		"submitted_at": time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		r.t.Fatalf("encode the review payload: %v", err)
	}
	subject := fmt.Sprintf("notifications.github.%s.pr.%d.review", r.subjectRepo(), number)
	r.waitProcessed(r.publish(subject, "github", fmt.Sprintf("review-%d-%d", number, r.seq), string(payload)))
}

// publishPush publishes the implementer's push of to onto from on issue's branch, changing code, as
// Envoy relays a GitHub push (intake's decodePush reads it). It is published before the synchronize
// that moves the head, which is the order that leaves the move judged by the chain of its pushes
// (classify.AdvancePullRequestHead).
func (r *mergeWaitReadableLive) publishPush(issue, from, to string) {
	r.t.Helper()
	r.seq++
	payload, err := json.Marshal(map[string]string{
		"kind": "push", "repo": liveRepo, "ref": "refs/heads/legion/" + issue, "before": from, "after": to,
		"changed_paths": "src/widget.go", "changed_paths_truncated": "false", "forced": "false",
		"pusher": "legion-implementer[bot]", "commit_count": "1", "head_subject": "earn what the base requires",
	})
	if err != nil {
		r.t.Fatalf("encode the push payload: %v", err)
	}
	subject := fmt.Sprintf("notifications.github.%s.push.branch.legion/%s", r.subjectRepo(), issue)
	r.waitProcessed(r.publish(subject, "github", fmt.Sprintf("push-%s-%s", issue, to[:8]), string(payload)))
}

// publishSynchronize publishes GitHub's synchronize of the pull request to head at updated, as Envoy
// relays a GitHub pull-request event (intake's decodePullRequest reads it). updated is GitHub's own
// clock, which the lifecycle fence orders the observation by and the record keeps as HeadUpdatedAt.
func (r *mergeWaitReadableLive) publishSynchronize(number int, issue, head string, updated time.Time) {
	r.t.Helper()
	r.seq++
	payload, err := json.Marshal(map[string]string{
		"action": "synchronize", "kind": "pr", "repo": liveRepo, "number": strconv.Itoa(number),
		"head_sha": head, "base_ref": mergeWaitBase, "head_ref": "legion/" + issue,
		"title":      "the issue whose approval waits on GitHub",
		"url":        "https://github.com/" + liveRepo + "/pull/" + strconv.Itoa(number),
		"updated_at": updated.Format(time.RFC3339),
	})
	if err != nil {
		r.t.Fatalf("encode the synchronize payload: %v", err)
	}
	subject := fmt.Sprintf("notifications.github.%s.pr.%d", r.subjectRepo(), number)
	r.waitProcessed(r.publish(subject, "github", fmt.Sprintf("pr-%d-synchronize-%s", number, head[:8]), string(payload)))
}

// publishBasePush publishes somebody else's push onto the base branch, as Envoy relays a GitHub
// push to a branch Legion holds no pull request on (intake's decodePush makes it an intake.BasePush).
// Its handler walks the open pull requests recorded on that base and asks for a read of each whose
// round waits on GitHub (workflow's basePush, askMergeStateFor), which is how a step asks the
// daemon to read GitHub again without changing anything the round is judged by.
func (r *mergeWaitReadableLive) publishBasePush() {
	r.t.Helper()
	r.seq++
	payload, err := json.Marshal(map[string]string{
		"kind": "push", "repo": liveRepo, "ref": "refs/heads/" + mergeWaitBase,
		"before": fmt.Sprintf("base-%d", r.seq-1), "after": fmt.Sprintf("base-%d", r.seq),
		"changed_paths": "src/other.go", "changed_paths_truncated": "false", "forced": "false",
		"pusher": "someone-else", "commit_count": "1", "head_subject": "another pull request merged",
	})
	if err != nil {
		r.t.Fatalf("encode the base push payload: %v", err)
	}
	subject := fmt.Sprintf("notifications.github.%s.push.branch.%s", r.subjectRepo(), mergeWaitBase)
	r.waitProcessed(r.publish(subject, "github", fmt.Sprintf("base-push-%d", r.seq), string(payload)))
}

// mergeWaitLine is a merge wait as a note prints it: every field of the object, with the two times
// in GitHub's own spelling and "never" for a stamp the view carries none of.
func mergeWaitLine(wait *api.MergeWaitView) string {
	read, updated := "never", "none"
	if wait.StateReadAt != nil {
		read = wait.StateReadAt.Format(time.RFC3339Nano)
	}
	if wait.HeadUpdatedAt != nil {
		updated = wait.HeadUpdatedAt.Format(time.RFC3339)
	}
	return fmt.Sprintf("waitsOn %s, mergeState %q, stateReadAt %s, headUpdatedAt %s, checkRuns %d, toldHead %s, toldState %q",
		mergeWaitShort(wait.WaitsOn), wait.MergeState, read, updated, wait.CheckRuns, mergeWaitShort(wait.ToldHead), wait.ToldState)
}

// mergeWaitShort is a commit as a note names it, GitHub's own seven characters, and "" as itself.
func mergeWaitShort(sha string) string {
	if len(sha) <= 7 {
		return sha
	}
	return sha[:7]
}

// mergeWaitGitHub is GitHub's REST API for acme/widgets as this proof's daemon reads it: each pull
// request the proof drives answered as GET /pulls/{number} answers one (daemon's
// githubPullRequest) — its number, its state, the base it sits on, the head it stands at, GitHub's
// clock for it, its lazily computed `mergeable` and `mergeable_state`, GitHub's own merge state
// for that head — with its reviews as none (readReviews) and the branch's open pull requests as
// none, which is what the per-issue read lists for an issue whose record holds none open. It also
// answers what a launch reads: any repository, with default branch main, and a GraphQL root that
// knows no pull request. Anything else is answered 404, so a read this proof did not foresee fails
// the step that waited on it rather than reaching github.com.
//
// Every field a step sets is set under this mutex, which the answer is written under too, so a
// step that sets a head and a state sets them for every read after it and none before it.
type mergeWaitGitHub struct {
	t   *testing.T
	url string
	mu  sync.Mutex
	// pulls is each pull request GitHub serves, by number. A pull request the record holds closed
	// is in none of them: nothing reads it, since the merge-state ladder reads only a record held
	// open and the per-issue read lists the branch instead.
	pulls map[int]*mergeWaitPull
}

// mergeWaitPull is one pull request as the stand-in answers it, and what a step has done to that
// answer: withheld says GitHub answers nothing about this head at all, refused counts the reads it
// has turned away, which is how a step holds that a read was made and answered nothing rather than
// that no read was made yet, and answered counts the reads it has served, which is how a step holds
// that the record it reads back came from a read GitHub answered after the step began rather than
// from an earlier read that reached the record late.
type mergeWaitPull struct {
	branch    string
	sha       string
	mergeable bool
	state     string
	updatedAt time.Time
	withheld  bool
	refused   int
	answered  int
}

func newMergeWaitGitHub(t *testing.T) *mergeWaitGitHub {
	t.Helper()
	g := &mergeWaitGitHub{t: t, pulls: map[int]*mergeWaitPull{}}
	server := httptest.NewServer(g)
	t.Cleanup(server.Close)
	g.url = server.URL
	return g
}

// open puts number on GitHub, open on branch at sha with GitHub's clock updated, answering
// mergeable and state for that head.
func (g *mergeWaitGitHub) open(number int, branch, sha string, mergeable bool, state string, updated time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.pulls[number] = &mergeWaitPull{branch: branch, sha: sha, mergeable: mergeable, state: state, updatedAt: updated}
}

// head moves number to sha with GitHub's clock updated, as a push does on GitHub.
func (g *mergeWaitGitHub) head(number int, sha string, updated time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.pull(number).sha, g.pull(number).updatedAt = sha, updated
}

// answers makes GitHub answer number's head with mergeable and state, the pair one read gives:
// GitHub computes them together, and answers `mergeable: true` for a head it holds on a check the
// repository requires as surely as for one it would merge now.
func (g *mergeWaitGitHub) answers(number int, mergeable bool, state string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.pull(number).mergeable, g.pull(number).state = mergeable, state
}

// withhold makes GitHub answer every read of number 503, as it answers a read it cannot serve, so
// the daemon's read ends with nothing applied: the record then holds no answer for the head, which
// is a round waiting for its first answering read.
func (g *mergeWaitGitHub) withhold(number int, yes bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.pull(number).withheld = yes
}

// refusals is how many reads of number GitHub has turned away while withheld.
func (g *mergeWaitGitHub) refusals(number int) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.pull(number).refused
}

// answeredReads is how many reads of number GitHub has served an answer to. It rises under the
// same mutex the answer is written under, so a step that samples it and then sees the record move
// knows GitHub answered a read after that sample.
func (g *mergeWaitGitHub) answeredReads(number int) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.pull(number).answered
}

// pull is number's pull request, failing the test when the stand-in serves none: every setter and
// every count names a pull request the run put there.
func (g *mergeWaitGitHub) pull(number int) *mergeWaitPull {
	g.t.Helper()
	pull, served := g.pulls[number]
	if !served {
		g.t.Fatalf("the GitHub stand-in serves no pull request #%d", number)
	}
	return pull
}

func (g *mergeWaitGitHub) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if req.Method == http.MethodPost && req.URL.Path == "/graphql" {
		_, _ = io.WriteString(w, `{"data":{"repository":{"pullRequest":null}}}`)
		return
	}
	if req.Method == http.MethodGet && g.answer(w, req.URL.Path) {
		return
	}

	w.WriteHeader(http.StatusNotFound)
	_, _ = io.WriteString(w, `{"message":"Not Found"}`)
}

// answer writes the stand-in's answer to a GET of path, and says whether it had one.
func (g *mergeWaitGitHub) answer(w http.ResponseWriter, path string) bool {
	rest, ours := strings.CutPrefix(path, "/repos/"+liveRepo)
	if !ours {
		// Any other repository is answered as the launch reads it: a name and its default branch.
		other, isRepository := strings.CutPrefix(path, "/repos/")
		if parts := strings.Split(other, "/"); isRepository && len(parts) == 2 {
			g.write(w, map[string]any{"full_name": other, "default_branch": mergeWaitBase})
			return true
		}
		return false
	}
	switch rest {
	case "":
		g.write(w, map[string]any{"full_name": liveRepo, "default_branch": mergeWaitBase})
		return true
	case "/pulls":
		// The listing the per-issue read makes for an issue whose record holds no open pull
		// request: GitHub holds none open on the branch, so that read applies no opened.
		g.write(w, []any{})
		return true
	}
	for number, pull := range g.pulls {
		switch rest {
		case fmt.Sprintf("/pulls/%d/reviews", number):
			g.write(w, []any{})
			return true
		case fmt.Sprintf("/pulls/%d", number):
			if pull.withheld {
				pull.refused++
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = io.WriteString(w, `{"message":"Server Error"}`)
				return true
			}
			pull.answered++
			g.write(w, map[string]any{
				"number": number, "state": "open", "mergeable": pull.mergeable, "mergeable_state": pull.state,
				"updated_at": pull.updatedAt.UTC().Format(time.RFC3339),
				"base":       map[string]string{"ref": mergeWaitBase},
				"head":       map[string]string{"ref": pull.branch, "sha": pull.sha},
			})
			return true
		}
	}
	return false
}

// write encodes value as the answer.
func (g *mergeWaitGitHub) write(w http.ResponseWriter, value any) {
	if err := json.NewEncoder(w).Encode(value); err != nil {
		g.t.Errorf("write the GitHub stand-in's answer: %v", err)
	}
}
