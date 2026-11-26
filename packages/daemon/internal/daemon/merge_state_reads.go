package daemon

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// mergeStateLadderReads is how many times one climb of the ladder reads GitHub's merge state for a
// head GitHub answers `unknown` for, and mergeStateLadderWait how long it waits between those
// reads. GitHub computes the merge state lazily: the first read of a head it has not looked at yet
// asks it to, and answers `unknown` while it works, which is an answer about GitHub and not about
// the head. A few seconds is the usual wait, so five reads half a minute apart cover it with room
// over.
//
// Two ladders climb. A fact of one pull request - a review round waiting on GitHub, a head move of
// an issue in implementing through merging - climbs with reads of that pull request alone
// (climbMergeState, one GET /pulls/{number} each). A push to a base climbs with reads of every pull
// request on that base the push owes a read, in one batch: GraphQL queries that ask for those pull
// requests alone, by number, baseReadChunk to a query (climbBase, readBase), which every base of
// an installation makes one at a time, paced and held by that installation's gate (queryGate).
//
// GitHub can answer `unknown` for far longer: INFRA-4391's record read `unknown` for over an hour
// while GitHub answered CONFLICTING and DIRTY for the same head (dispatch://INFRA-4374 R4). So a
// climb whose fifth read still answers `unknown` is followed by more reads of the same head, each
// after a wait twice the one before, from two of the ladder's waits up to mergeStateFollowWaits of
// them - one, two, four and eight minutes, then ten minutes each - until GitHub answers a state,
// the record's head moves off the head read, or the pull request is no longer open. Those reads
// are the base's batch's (followBase): a climb of one pull request hands the pull request, as its
// last read held it, to its base (followOnBase), each pull request keeps its own ladder of waits
// there, starting from its climb or its handover, and is read once it is due and never before, in
// the same query as those due then. A pull request's reads end once GitHub answers it a state. The
// same reads follow a second read of a conflicting head whose last attempt still read no answer
// (conflict_confirm.go's conflictConfirm, queueMergeStateFollow). A request for a read that arrives
// meanwhile ends the follow's wait and starts a climb at once. The reads are held in memory, as the
// climb is: no row waits in the outbox, and a restart ends them; the daemon's start reads every
// issue's pull request again and queues a ladder again for each one GitHub still answers `unknown`
// for (readIssuesAtStart), which climbs and follows in turn. `unknown` is no answer about the head,
// so nobody is told of it, and a `blocked` answer missing part of what the round decides it on -
// `mergeable` null, no rollup of the head's checks, no count of its review requests
// (classify.MergeAnswerIncomplete) - is read again as `unknown` is. Every other answer the ladder
// gets is recorded, and what it comes to for an approved round - it ends the round, waits
// quietly, or waits and tells the architect that owns the issue and the reviewer once for that
// head and state - is classify.MergeOutcomeOf, the one place that rule is written (workflow's
// reviewRound, stuckMergeState).
//
// mergeStateLadderReads and mergeStateLadderWait have two more users, the outbox's reads of one
// pull request: the merge-queue read (merge_queue_removal.go's mergeQueueRead), which reads a pull
// request GitHub reported leaving the merge queue (dispatch://INFRA-3274/spec R1), and the second
// read of a head a first read found conflicting (conflict_confirm.go's conflictConfirm,
// dispatch://INFRA-4374 R2), each as this ladder reads a merge state GitHub answers `unknown`
// for. Each makes at most mergeStateLadderReads attempts, each with at most one read, and reads
// again mergeStateLadderWait (mergeStateWait) later, or after GitHub's Retry-After when that is
// longer. So a change to either number changes those reads too. mergeStateFollowWaits is the
// longest wait between two reads after a climb, counted in the ladder's waits: twenty half
// minutes, ten minutes.
const (
	mergeStateLadderReads = 5
	mergeStateLadderWait  = 30 * time.Second
	mergeStateFollowWaits = 20
)

// queueMergeState is what the workflow asks for when a fact of one pull request owes a read of
// GitHub's merge state for an issue's open pull request (workflow's askMergeState, the engine's
// Config.ReadMergeState): a review round the fact leaves waiting on GitHub's answer for the head,
// or ended by an approval the fact did not move on (a CI settlement, which moves no issue), and,
// by the gate classify.MergeStateReadOwed, a fact that moves the head of an issue where a merge
// forward stands (classify.MergeForwardStands: its task at the implementer's place, or a place
// after it on the task's line, a step of a role the deployment defines among them), which can turn
// the head conflicting with its base (dispatch://INFRA-4374 R0). A push to a base asks for its pull
// requests' reads all at once instead (queueBaseMergeStates). The daemon's start asks for it too,
// for each issue whose start read GitHub answered `unknown` (readIssuesAtStart). It queues a ladder
// of reads for issue that starts with a climb (queueMergeStateFollow, with no pull request to
// follow). It only records the issue and wakes the reader, so the fact's own transaction waits for
// no GitHub call, and a second fact of the same issue queued before the reader takes it is one
// ladder rather than two. A runtime with no reader - the API-only daemon, and a test that drives
// the ladder itself - queues nothing.
func (w *workflowRuntime) queueMergeState(issue string) {
	w.queueMergeStateFollow(issue, nil)
}

// queueMergeStateFollow records that issue is owed a ladder (mergeStateQueued) and wakes the
// reader (readMergeStates): follow is the pull request the ladder hands to the reads of its base
// at once (followOnBase), as the read that hands it over held it - its repository, its base and
// the head that read was made at - and nil for a ladder that starts with a climb
// (queueMergeState). A pull request is what the second read of a conflicting head hands over once
// its last attempt read no answer, GitHub's UNKNOWN or a read that failed, and released the first
// read (conflict_confirm.go's conflictConfirm, which attach hands this): reads of its head that
// follow GitHub's merge state as the reads after a climb that ended on `unknown` do (followBase),
// the first of them after twice the ladder's wait, with no climb before them, since the second
// read's attempts were that climb (dispatch://INFRA-4374 R4). Their CONFLICTING answer is applied
// as every read's is, so it records a first conflicting read of the head and the second read
// starts again (R2).
//
// Asking for a climb leaves the issue in its base's reads; the climb takes it out of them, and
// when, by climbMergeState's rule. The base's reads do not wait for a climb that runs: they read
// the issue while it is theirs, and a read of the base that took the issue up before the climb
// took it can still apply its answer, since readChunk checks that its batch holds the issue just
// before it applies one. So a pull request can be read by its own climb and by its base's reads at
// once. That costs a read and nothing more: the workflow records an answer only for the head the
// record holds, and drops one read before the answer the record holds (workflow's mergeability).
// An issue whose ladder already runs takes the request as a request for a read, and its ladder
// climbs again.
//
// A climb asked for before the reader takes the issue outranks a follow asked for beside it, in
// either order: the climb reads at once and follows in turn while GitHub answers `unknown`, and
// the fact that asked for it can have moved the head off the one the follow would read. Of two
// follows, the later one's pull request stands. A runtime with no reader records nothing.
func (w *workflowRuntime) queueMergeStateFollow(issue string, follow *record.PullRequest) {
	if w.mergeStates == nil || issue == "" {
		return
	}
	w.mergeStateMu.Lock()
	if w.mergeStateQueued == nil {
		w.mergeStateQueued = map[string]*record.PullRequest{}
	}
	if queued, ok := w.mergeStateQueued[issue]; follow == nil || !ok || queued != nil {
		w.mergeStateQueued[issue] = follow
	}
	w.mergeStateMu.Unlock()
	w.mergeStates.add(issue)
}

// readMergeStates runs a ladder for each issue queueMergeStateFollow queued, until ctx ends: each
// wake takes every issue queued since the last one, in the order of their keys, and runs its
// ladder on a goroutine of its own, so one issue's waits hold up no other's. The ladder starts as
// the issue's request asked (mergeStateQueued): with a climb, or by handing the pull request a
// second read handed over to its base. An issue whose ladder is still climbing starts no second
// one, and is marked as asked again instead, whichever kind of ladder it asked for: the running
// ladder climbs once more when its climb ends (mergeStateLadder), so the fact that asked is
// answered by a read made after it, which a ladder already past its last read would not be.
// Dropping the request outright left the issue standing at whatever the running ladder's last
// answer was, though the fact behind it - a check that concluded, a push, a review - is exactly
// what changes GitHub's answer (dispatch://INFRA-714 R4).
//
// mergeStateRunning is that bookkeeping: an issue is in it while its ladder runs
// (mergeStateRun). An issue a wake carries with no request standing in mergeStateQueued is skipped:
// its request was recorded while an earlier wake took the issue, and that wake took it up.
//
// It runs the reader of the bases' batches beside it for as long as it runs
// (readBaseMergeStates), since its ladders hand their follows to them.
func (w *workflowRuntime) readMergeStates(ctx context.Context) {
	go w.readBaseMergeStates(ctx)
	for {
		issues, ok := w.mergeStates.take(ctx)
		if !ok {
			return
		}
		w.mergeStateMu.Lock()
		if w.mergeStateRunning == nil {
			w.mergeStateRunning = map[string]*mergeStateRun{}
		}
		started := make(map[string]*record.PullRequest, len(issues))
		for _, issue := range issues {
			follow, queued := w.mergeStateQueued[issue]
			if !queued {
				continue
			}
			delete(w.mergeStateQueued, issue)
			if run, running := w.mergeStateRunning[issue]; running {
				run.asked = true
				continue
			}
			w.mergeStateRunning[issue] = &mergeStateRun{}
			started[issue] = follow
		}
		w.mergeStateMu.Unlock()
		for _, issue := range issues {
			if follow, start := started[issue]; start {
				go w.mergeStateLadder(store.ForConcurrentWorker(ctx), issue, follow)
			}
		}
	}
}

// mergeStateRun is one running ladder's bookkeeping (workflowRuntime.mergeStateRunning): asked is
// whether a request for a read arrived since its climb began.
type mergeStateRun struct {
	asked bool
}

// mergeStateLadder runs issue's ladder: it climbs (climbMergeState) - or, when follow names a pull
// request, hands it to its base's reads at once instead (queueMergeStateFollow) - hands the pull
// request a climb ended on `unknown` for to its base's reads (followOnBase), and climbs again for
// as long as something asked for another climb (mergeStateEnds); it leaves the issue unmarked as
// running when it returns.
func (w *workflowRuntime) mergeStateLadder(ctx context.Context, issue string, follow *record.PullRequest) {
	for {
		if follow == nil {
			follow = w.climbMergeState(ctx, issue)
		}
		if follow != nil {
			w.followOnBase(issue, *follow)
		}
		if w.mergeStateEnds(ctx, issue) {
			return
		}
		follow = nil
	}
}

// mergeStateEnds says whether issue's ladder ends here, and is the one place the issue's mark is
// taken off: it ends unless a request arrived while the climb ran and ctx is still live, and it is
// read under the lock readMergeStates marks under, so a request made during the last read is
// either marked before this reads it - and climbs - or lands after the mark is gone and starts a
// ladder of its own.
func (w *workflowRuntime) mergeStateEnds(ctx context.Context, issue string) bool {
	w.mergeStateMu.Lock()
	defer w.mergeStateMu.Unlock()
	if run := w.mergeStateRunning[issue]; run != nil && run.asked && ctx.Err() == nil {
		run.asked = false
		return false
	}
	delete(w.mergeStateRunning, issue)
	return true
}

// climbMergeState reads GitHub's merge state for issue's pull request until GitHub answers with
// one the round can decide on, at most mergeStateLadderReads times, mergeStateLadderWait apart.
// GitHub's `unknown` says it is still computing the state, and a `blocked` answer missing part of
// what the round decides it on - mergeability not computed, no rollup of the head's checks, no
// count of its review requests (classify.MergeAnswerIncomplete) - is no more of an answer, so the
// climb asks again for each; every other answer, the read finding no open pull request among
// them, is an answer, and the climb ends on it and answers nil. Below, `unknown` stands for both.
// After the last `unknown` it ends too, and answers the pull request that read was made of, as
// readMergeState
// holds it, which the ladder then hands to its base's reads (followOnBase) with no read of the
// record between the two. A read that fails - the record's read, or GitHub's, which answers 403
// for up to an hour once the implement App's REST budget is spent - ends the climb as well. After
// an `unknown` it answers the pull request the last `unknown` was read of, so the head goes to its
// base's reads as after the last read, and they read it until GitHub answers it a state, as they
// keep a pull request whose read fails owed (readBase): a failed read says nothing of the head, and
// GitHub has already answered that it is still computing it (dispatch://INFRA-4374 R4). A read
// that fails before any `unknown` answers the pull request the record holds, on its own base,
// when no base's reads hold the issue yet, so those reads read it until GitHub answers: a round
// that waits quietly on the head has no event coming that would read it again. Where a base's
// reads already hold the issue it answers nil, and they read it on their own ladder. A read that
// fails on the record's read answers nil, as there is no pull request to hand. A read
// GitHub answered whose answer the record did not take (errUnapplied: the database failed the
// write) ends the climb and answers that read's pull request, whatever GitHub answered, so its
// base's reads read it again on its ladder: the record still holds what it held before, and
// nothing else would read the head again until its next event (dispatch://INFRA-4374 R4).
//
// The climb takes the issue out of its base's reads once: after its first read, when GitHub
// answered and the record took the answer, or the record held no open pull request to read, and
// only under the request the base held the issue under when the climb began (leaveBase). It does
// not take it out when it is asked, or before that read: a first read that fails has nothing to
// hand back, and an issue its base followed would then be read by nobody until its next event
// (dispatch://INFRA-4374 R4). A request put in while the first read ran - a push to the base,
// whose own read reads the head after that push, where the first read may have read it before -
// stands.
func (w *workflowRuntime) climbMergeState(ctx context.Context, issue string) *record.PullRequest {
	key, asked := w.heldUnder(issue)
	var unknown *record.PullRequest
	for read := 1; ; read++ {
		state, incomplete, pr, err := w.readMergeState(ctx, issue)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if errors.Is(err, errUnapplied) {
				w.log.Warn("github: the record did not take GitHub's merge state for an issue whose read was asked for; the reads of its base read it again",
					"issue", issue, "head", pr.HeadSHA, "read", read, "answered", string(state), "error", err)
				return pr
			}
			// A read that fails before any `unknown` hands the record's pull request to its base's
			// reads when no base holds the issue yet: nothing else would read the head until its
			// next event, and a round that waits quietly on it has none coming (dispatch://INFRA-4192
			// review 6049029052). A base that already holds it reads it on its own ladder.
			handed := unknown
			if handed == nil && key == "" {
				handed = pr
			}
			w.log.Warn("read GitHub's merge state for an issue whose read was asked for; the record stands as it was until the next read",
				"issue", issue, "read", read, "handedToBase", handed != nil, "error", err)
			if pr != nil {
				w.readFailed(ctx, *pr)
			}
			return handed
		}
		if read == 1 {
			w.leaveBase(key, issue, asked)
		}
		if !incomplete {
			return nil
		}
		unknown = pr
		if read >= mergeStateLadderReads {
			return unknown
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(w.mergeStateWait()):
		}
	}
}

// followOnBase hands pr, issue's pull request as the read before it held it - a climb whose last
// read GitHub answered `unknown` for, or a second read of a conflict that read no answer - to the
// reads of the base it is on (queueBaseRead, as a follow): those reads read its head again on the
// follow's waits, in one query with every other pull request of that base GitHub still answers
// `unknown` for, until GitHub answers a state (dispatch://INFRA-4374 R4). It reads no record of
// its own, since the read before it has just read one: a failed read here would leave the head
// unread until its next event, where the base's reads keep an issue whose record read fails owed
// and read it at its next turn (readBase). Their first read of the issue checks the record, and
// ends the follow on its end line once the record no longer holds the pull request open at that
// head.
func (w *workflowRuntime) followOnBase(issue string, pr record.PullRequest) {
	repository, err := ghrepo.Parse("the pull request's repository", pr.Repo)
	if err == nil && pr.Base == "" {
		err = fmt.Errorf("neither the record nor GitHub names a base of pull request #%d", pr.Number)
	}
	if err != nil {
		w.log.Warn("github: hand a head GitHub answers unknown for to the reads of its base; nothing reads it again until its next event",
			"issue", issue, "head", pr.HeadSHA, "error", err)
		return
	}
	w.queueBaseRead(w.repositories.serving(repository), pr.Base, map[string]string{issue: pr.HeadSHA}, false)
}

// followed is pr, the record's pull request as a read of GitHub took it up, as the reads of its
// base follow it (followOnBase): at the head the record held, which is the head that read was made
// at, and on the base GitHub's answer pull names, which that read's apply records too - or on the
// record's own base when GitHub named none, as a read that failed names none.
func followed(pr record.PullRequest, pull githubPullRequest) record.PullRequest {
	if pull.Base.Ref != "" {
		pr.Base = pull.Base.Ref
	}
	return pr
}

// mergeStateWait is how long the ladder waits between reads of a head GitHub is still computing
// the merge state of, the unit the reads after it wait in (followBase), and how long the outbox's
// merge-queue read and second conflict read wait before they read again (mergeQueueRead,
// conflictConfirm, which attach hands this wait): mergeStateLadderWait, which a test shortens
// through the runtime's own field.
func (w *workflowRuntime) mergeStateWait() time.Duration {
	if w.mergeStateRetry > 0 {
		return w.mergeStateRetry
	}
	return mergeStateLadderWait
}

// errUnapplied marks a read GitHub answered whose answer the record did not take: the fact that
// carries it failed (applyMergeability), as a write the database fails fails it. The record holds
// what it held before the read, so the pull request is owed a read still.
var errUnapplied = errors.New("the record did not take GitHub's answer")

// readMergeState reads issue's pull request from GitHub once (one GET /pulls/{number}) and applies
// what it says about merging that head into its base (applyMergeability, the same fact the
// per-issue read applies), answering GitHub's merge state as that read found it, whether that
// answer is one the readers read again (answerIncomplete: `unknown`, or `blocked` missing part of
// what the round decides it on), and the pull request the record held when it read, as the reads
// of its base follow it (followed). An issue whose record holds no open pull request is read from
// nothing and answers record.MergeStateUnread, complete, and no pull request: the state belongs to
// a pull request, and there is none. An answer whose apply failed is answered with all three, and
// an error that is errUnapplied.
//
// The fact names the head GitHub answered for, which the engine records the answer against only
// while the record still stands at it (workflow's mergeability): this ladder reads GitHub with no
// transaction of its own, and a push can land while it waits. The ladder reads on, since what it
// answers is GitHub's state for the head it read, and the push the record took is itself a fact
// that asks for a read of the head it left (workflow's askMergeState).
func (w *workflowRuntime) readMergeState(ctx context.Context, issue string) (record.MergeState, bool, *record.PullRequest, error) {
	_, recorded, err := issueRecord(ctx, w.pool, w.records, issue)
	if err != nil {
		return record.MergeStateUnread, false, nil, err
	}
	if recorded == nil || recorded.State != record.PullRequestOpen {
		return record.MergeStateUnread, false, nil, nil
	}
	read, cancel := context.WithTimeout(ctx, issueRead)
	github, pull, err := w.readPullRequest(read, *recorded)
	answeredAt := time.Now().UTC()
	cancel()
	if err != nil {
		// The pull request the record holds, on the base it records, so a climb whose first read
		// fails can hand the head to its base's reads (climbMergeState).
		return record.MergeStateUnread, false, recorded, fmt.Errorf("read pull request %s#%d and its merge state: %w", recorded.Repo, recorded.Number, err)
	}
	pr := followed(*recorded, pull)
	fact, err := w.applyMergeability(ctx, github, *recorded, pull, answeredAt, w.mergeabilityEventID(*recorded, answeredAt))
	if err != nil {
		return fact.MergeState, answerIncomplete(fact), &pr, fmt.Errorf("%w: %w", errUnapplied, err)
	}
	return fact.MergeState, answerIncomplete(fact), &pr, nil
}
