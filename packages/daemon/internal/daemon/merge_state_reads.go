package daemon

import (
	"context"
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
// are the base's batch's (followBase): a climb of one pull request hands its head to its base
// (followOnBase), each pull request keeps its own ladder of waits there, starting from its climb or
// its handover, and is read once it is due and never before, in the same query as those due then.
// A pull request's reads end once GitHub answers it a state. The
// same reads follow a second read of a conflicting head whose last attempt still read no answer
// (conflict_confirm.go's conflictConfirm, queueMergeStateFollow). A request for a read that arrives
// meanwhile ends the follow's wait and starts a climb at once. The reads are held in memory, as the
// climb is: no row waits in the outbox, and a restart ends them; the daemon's start reads every
// issue's pull request again and queues a ladder again for each one GitHub still answers `unknown`
// for (readIssuesAtStart), which climbs and follows in turn. `unknown` is no answer about the head,
// so nobody is told of it. Every other answer the ladder gets is recorded, and an approval left
// standing on a head GitHub answers with a state it would not merge from tells the architect that
// owns the issue and the reviewer, once for that head and state, in the review-stuck notice the
// round writes - but only once the CI settlement standing for the head says every check GitHub
// reported on it has concluded, since until then the state says no more than that a check is
// running (workflow's reviewRound, stuckMergeState).
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

// The lines the reads after a climb log (followBase, followOnBase): one for each pull request each
// read answers or fails for, with its wait, and one for each pull request whose reads end because
// the record no longer holds it open at the head they read.
const (
	followReadLine = "github: a merge state read after the ladder"
	followEndLine  = "github: the merge state reads after the ladder end: the pull request is no longer open at the head they read"
)

// queueMergeState is what the workflow asks for when a fact of one pull request owes a read of
// GitHub's merge state for an issue's open pull request (workflow's askMergeState, the engine's
// Config.ReadMergeState): a review round the fact leaves waiting on GitHub's answer for the head,
// and, by the gate classify.MergeStateReadOwed, a fact that moves the head of an issue in
// implementing through merging, which can turn the head conflicting with its base
// (dispatch://INFRA-4374 R0). A push to a base asks for its pull requests' reads all at once
// instead (queueBaseMergeStates). The daemon's start asks for it too, for each issue whose start
// read GitHub answered `unknown` (readIssuesAtStart). It queues a ladder of reads for issue that
// starts with a climb (queueMergeStateFollow, with no head to follow). It only records the issue
// and wakes the reader, so the fact's own transaction waits for no GitHub call, and a second fact
// of the same issue queued before the reader takes it is one ladder rather than two. A runtime
// with no reader - the API-only daemon, and a test that drives the ladder itself - queues nothing.
func (w *workflowRuntime) queueMergeState(issue string) {
	w.queueMergeStateFollow(issue, "")
}

// queueMergeStateFollow records that issue is owed a ladder (mergeStateQueued) and wakes the
// reader (readMergeStates): follow is the head the ladder hands to the reads of its base at once
// (followOnBase), "" for a ladder that starts with a climb (queueMergeState). A head is what the
// second read of a conflicting head asks for once its last attempt read no answer, GitHub's
// UNKNOWN or a read that failed, and released the first read (conflict_confirm.go's
// conflictConfirm, which attach hands this): reads of head that follow GitHub's merge state as the
// reads after a climb that ended on `unknown` do (followBase), the first of them after twice the
// ladder's wait, with no climb before them, since the second read's attempts were that climb
// (dispatch://INFRA-4374 R4). Their CONFLICTING answer is applied as every read's is, so it
// records a first conflicting read of the head and the second read starts again (R2).
//
// A climb takes the issue out of its base's reads (leaveBaseLocked): the climb reads it now, and
// hands it back to its base if GitHub still answers `unknown` at its last read, so no pull request
// is read by two ladders at once. An issue whose ladder already runs takes the request as a
// request for a read, and its ladder climbs again.
//
// A climb asked for before the reader takes the issue outranks a follow asked for beside it, in
// either order: the climb reads at once and follows in turn while GitHub answers `unknown`, and
// the fact that asked for it can have moved the head off the one the follow would read. Of two
// follows, the later one's head stands. A runtime with no reader records nothing.
func (w *workflowRuntime) queueMergeStateFollow(issue, follow string) {
	if w.mergeStates == nil || issue == "" {
		return
	}
	w.mergeStateMu.Lock()
	if w.mergeStateQueued == nil {
		w.mergeStateQueued = map[string]string{}
	}
	if queued, ok := w.mergeStateQueued[issue]; follow == "" || !ok || queued != "" {
		w.mergeStateQueued[issue] = follow
	}
	if follow == "" {
		w.leaveBaseLocked(issue)
	}
	w.mergeStateMu.Unlock()
	w.mergeStates.add(issue)
}

// readMergeStates runs a ladder for each issue queueMergeStateFollow queued, until ctx ends: each
// wake takes every issue queued since the last one, in the order of their keys, and runs its
// ladder on a goroutine of its own, so one issue's waits hold up no other's. The ladder starts as
// the issue's request asked (mergeStateQueued): with a climb, or by handing the head a second read
// named to its base. An issue whose ladder is still climbing starts no second one, and is marked
// as asked again instead, whichever kind of ladder it asked for: the running ladder climbs once
// more when its climb ends (mergeStateLadder), so the fact that asked is answered by a read made
// after it, which a ladder already past its last read would not be. Dropping the request outright
// left the issue standing at whatever the running ladder's last answer was, though the fact behind
// it - a check that concluded, a push, a review - is exactly what changes GitHub's answer
// (dispatch://INFRA-714 R4).
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
		started := make(map[string]string, len(issues))
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

// mergeStateLadder runs issue's ladder: it climbs (climbMergeState) - or, when follow names a head,
// hands that head to its base's reads at once instead (queueMergeStateFollow) - hands the head a
// climb that ended on `unknown` to its base's reads (followOnBase), and climbs again for as long as
// something asked for another climb (mergeStateEnds); it leaves the issue unmarked as running when
// it returns.
func (w *workflowRuntime) mergeStateLadder(ctx context.Context, issue, follow string) {
	head := follow
	for {
		if head == "" {
			head = w.climbMergeState(ctx, issue)
		}
		if head != "" {
			w.followOnBase(ctx, issue, head)
		}
		if w.mergeStateEnds(ctx, issue) {
			return
		}
		head = ""
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
// one, at most mergeStateLadderReads times, mergeStateLadderWait apart. GitHub's `unknown` says it
// is still computing the state, so the climb asks again; every other answer, the read finding no
// open pull request among them, is an answer, and the climb ends on it and answers "". After the
// last `unknown` it ends too, and answers the head that read was made at, which the ladder then
// hands to its base's reads (followOnBase). A failed read ends the climb with nothing to follow:
// the issue's next event, its next handoff and the daemon's next start all read again.
func (w *workflowRuntime) climbMergeState(ctx context.Context, issue string) string {
	for read := 1; ; read++ {
		state, head, err := w.readMergeState(ctx, issue)
		if err != nil {
			if ctx.Err() == nil {
				w.log.Warn("read GitHub's merge state for an issue whose read was asked for; the record stands as it was until the next read",
					"issue", issue, "read", read, "error", err)
			}
			return ""
		}
		if state != record.MergeStateUnknown {
			return ""
		}
		if read >= mergeStateLadderReads {
			return head
		}
		select {
		case <-ctx.Done():
			return ""
		case <-time.After(w.mergeStateWait()):
		}
	}
}

// followOnBase hands head, the head of issue's pull request a climb last read `unknown` for or a
// second read of a conflict read no answer for, to the reads of the base the record holds that
// pull request on (queueBaseRead, as a follow): those reads read it again on the follow's waits, in
// one query with every other pull request of that base GitHub still answers `unknown` for, until
// GitHub answers a state (dispatch://INFRA-4374 R4). A record that no longer holds the pull request
// open at head is followed no more, and says so on the follow's end line.
func (w *workflowRuntime) followOnBase(ctx context.Context, issue, head string) {
	_, pr, err := issueRecord(ctx, w.pool, w.records, issue)
	if err != nil {
		if ctx.Err() == nil {
			w.log.Warn("github: hand a head GitHub answers unknown for to the reads of its base; nothing reads it again until its next event",
				"issue", issue, "head", head, "error", err)
		}
		return
	}
	if pr == nil || pr.State != record.PullRequestOpen || pr.HeadSHA != head {
		w.log.Info(followEndLine, "issue", issue, "head", head, "attempt", 0)
		return
	}
	repository, err := ghrepo.Parse("the pull request's repository", pr.Repo)
	if err == nil && pr.Base == "" {
		err = fmt.Errorf("the record holds pull request #%d on no base", pr.Number)
	}
	if err != nil {
		w.log.Warn("github: hand a head GitHub answers unknown for to the reads of its base; nothing reads it again until its next event",
			"issue", issue, "head", head, "error", err)
		return
	}
	w.queueBaseRead(w.repositories.serving(repository), pr.Base, map[string]string{issue: head}, false)
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

// readMergeState reads issue's pull request from GitHub once (one GET /pulls/{number}) and applies
// what it says about merging that head into its base (applyMergeability, the same fact the
// per-issue read applies), answering GitHub's merge state as that read found it and the head the
// record held the pull request at when it read. An issue whose record holds no open pull request is
// read from nothing and answers record.MergeStateUnread and no head: the state belongs to a pull
// request, and there is none.
//
// The fact names the head GitHub answered for, which the engine records the answer against only
// while the record still stands at it (workflow's mergeability): this ladder reads GitHub with no
// transaction of its own, and a push can land while it waits. The ladder reads on, since what it
// answers is GitHub's state for the head it read, and the push the record took is itself a fact
// that asks for a read of the head it left (workflow's askMergeState).
func (w *workflowRuntime) readMergeState(ctx context.Context, issue string) (record.MergeState, string, error) {
	_, recorded, err := issueRecord(ctx, w.pool, w.records, issue)
	if err != nil {
		return record.MergeStateUnread, "", err
	}
	if recorded == nil || recorded.State != record.PullRequestOpen {
		return record.MergeStateUnread, "", nil
	}
	read, cancel := context.WithTimeout(ctx, issueRead)
	github, pull, err := w.readPullRequest(read, *recorded)
	answeredAt := time.Now().UTC()
	cancel()
	if err != nil {
		return record.MergeStateUnread, "", fmt.Errorf("read pull request %s#%d and its merge state: %w", recorded.Repo, recorded.Number, err)
	}
	// A failed apply is logged there, and the next read of the pull request applies it again.
	_ = w.applyMergeability(ctx, github, *recorded, pull, answeredAt, w.mergeabilityEventID(*recorded, answeredAt))
	return pull.mergeState(), recorded.HeadSHA, nil
}
