package daemon

import (
	"context"
	"fmt"
	"time"

	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// mergeStateLadderReads is how many times one climb of the ladder (climbMergeState) reads GitHub's
// merge state for a head GitHub answers `unknown` for, and mergeStateLadderWait how long it waits
// between those reads. GitHub computes the merge state lazily: the first read of a head it has not
// looked at yet asks it to, and answers `unknown` while it works, which is an answer about GitHub
// and not about the head. A few seconds is the usual wait, so five reads half a minute apart cover
// it with room over.
//
// GitHub can answer `unknown` for far longer: INFRA-4391's record read `unknown` for over an hour
// while GitHub answered CONFLICTING and DIRTY for the same head (dispatch://INFRA-4374 R4). So a
// climb whose fifth read still answers `unknown` is followed by more reads of the same head
// (followMergeState), each after a wait twice the one before, from two of the ladder's waits up to
// mergeStateFollowWaits of them - one, two, four and eight minutes, then ten minutes each - until
// GitHub answers a state, the record's head moves off the head read, or the pull request is no
// longer open. A request for a read that arrives meanwhile ends the follow's wait and starts a
// climb at once (mergeStateEnds). It is held in memory, as the climb is: no row waits in the
// outbox, and nothing is lost across a restart, since the daemon's start reads every issue's pull
// request again (readIssuesAtStart). `unknown` is no answer about the head, so nobody is told of
// it. Every other answer the ladder gets is recorded, and an approval left standing on a head
// GitHub answers with a state it would not merge from tells the architect that owns the issue and
// the reviewer, once for that head and state, in the review-stuck notice the round writes - but
// only once the CI settlement standing for the head says every check GitHub reported on it has
// concluded, since until then the state says no more than that a check is running (workflow's
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

// queueMergeState is what the workflow asks for when a fact owes a read of GitHub's merge state for
// an issue's open pull request (workflow's askMergeState and basePush, the engine's
// Config.ReadMergeState), by the gate classify.MergeStateReadOwed states: a review round the fact
// leaves waiting on GitHub's answer for the head, a fact that moves the head of an issue in
// implementing through merging, and a push to the base of an issue in testing through merging, each
// of which can turn the head conflicting with its base (dispatch://INFRA-4374 R0). It queues a
// ladder of reads for issue. It only records the issue and wakes the reader, so the fact's own
// transaction waits for no GitHub call, and a second fact of the same issue queued before the
// reader takes it is one ladder rather than two. A runtime with no reader - the API-only daemon,
// and a test that drives the ladder itself - queues nothing.
func (w *workflowRuntime) queueMergeState(issue string) {
	w.mergeStates.add(issue)
}

// readMergeStates runs a ladder for each issue queueMergeState queued, until ctx ends: each wake
// takes every issue queued since the last one, in the order of their keys, and runs its ladder on
// a goroutine of its own, so one issue's waits hold up no other's. An issue whose ladder is still
// running - climbing, or following a climb that ended on `unknown` - starts no second one, and is
// marked as asked again instead: the running ladder climbs once more when its climb ends, and a
// follow's wait ends at once for it (mergeStateLadder), so the fact that asked is answered by a
// read made after it, which a ladder already past its last read would not be. Dropping the request
// outright left the issue standing at whatever the running ladder's last answer was, though the
// fact behind it - a check that concluded, a push, a review - is exactly what changes GitHub's
// answer (dispatch://INFRA-714 R4).
//
// mergeStateRunning is that bookkeeping: an issue is in it while its ladder runs
// (mergeStateRun).
func (w *workflowRuntime) readMergeStates(ctx context.Context) {
	for {
		issues, ok := w.mergeStates.take(ctx)
		if !ok {
			return
		}
		w.mergeStateMu.Lock()
		if w.mergeStateRunning == nil {
			w.mergeStateRunning = map[string]*mergeStateRun{}
		}
		started := make([]string, 0, len(issues))
		for _, issue := range issues {
			if run, running := w.mergeStateRunning[issue]; running {
				run.asked = true
				select {
				case run.wake <- struct{}{}:
				default:
				}
				continue
			}
			w.mergeStateRunning[issue] = &mergeStateRun{wake: make(chan struct{}, 1)}
			started = append(started, issue)
		}
		w.mergeStateMu.Unlock()
		for _, issue := range started {
			go w.mergeStateLadder(store.ForConcurrentWorker(ctx), issue)
		}
	}
}

// mergeStateRun is one running ladder's bookkeeping (workflowRuntime.mergeStateRunning): asked is
// whether a request for a read arrived since its climb began, and wake ends a follow's wait for
// that request (followWait). wake holds one signal at most: readMergeStates sends it without
// blocking and mergeStateEnds drains it as it takes the request up, both under mergeStateMu, so a
// request a climb answers leaves no signal behind to cut a later follow's wait short.
type mergeStateRun struct {
	asked bool
	wake  chan struct{}
}

// mergeStateLadder climbs for issue, follows a climb that ended on `unknown` (followMergeState),
// and climbs again for as long as something asked for another climb (mergeStateEnds); it leaves
// the issue unmarked as running when it returns.
func (w *workflowRuntime) mergeStateLadder(ctx context.Context, issue string) {
	for {
		if head := w.climbMergeState(ctx, issue); head != "" {
			w.followMergeState(ctx, issue, head)
		}
		if w.mergeStateEnds(ctx, issue) {
			return
		}
	}
}

// mergeStateEnds says whether issue's ladder ends here, and is the one place the issue's mark is
// taken off: it ends unless a request arrived while the climb or its follow ran and ctx is still
// live, and it is read under the lock readMergeStates marks under, so a request made during the
// last read is either marked before this reads it - and climbs - or lands after the mark is gone
// and starts a ladder of its own.
func (w *workflowRuntime) mergeStateEnds(ctx context.Context, issue string) bool {
	w.mergeStateMu.Lock()
	defer w.mergeStateMu.Unlock()
	if run := w.mergeStateRunning[issue]; run != nil && run.asked && ctx.Err() == nil {
		run.asked = false
		select {
		case <-run.wake:
		default:
		}
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
// follows (followMergeState). A failed read ends the climb with nothing to follow: the issue's
// next event, its next handoff and the daemon's next start all read again.
func (w *workflowRuntime) climbMergeState(ctx context.Context, issue string) string {
	for read := 1; ; read++ {
		state, head, err := w.readMergeState(ctx, issue, "")
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

// followMergeState reads issue's pull request again after a climb whose last read, made at head,
// still answered `unknown` (dispatch://INFRA-4374 R4): GitHub can go on answering `unknown` for an
// hour while it holds the head CONFLICTING, and a ladder that stopped at its fifth read left the
// record reading `unknown` all that while. Each read waits twice as long as the one before, from
// two of the ladder's waits (mergeStateWait, which a test shortens) up to mergeStateFollowWaits of
// them, and is logged with its wait and what it answered; readMergeState applies its answer as it
// applies every read's.
//
// The follow ends when GitHub answers a state, any answer but `unknown`; when the record's pull
// request is no longer open or no longer stands at head, which reads nothing and is logged; when a
// request for a read arrives, before a wait or during it (followWait), since the ladder then climbs
// at once (mergeStateEnds); and when ctx ends. A read that fails is logged and followed by the next
// wait, since the record follows GitHub only while something reads it.
func (w *workflowRuntime) followMergeState(ctx context.Context, issue, head string) {
	wait := w.mergeStateWait()
	for attempt := 1; ; attempt++ {
		wait = min(2*wait, mergeStateFollowWaits*w.mergeStateWait())
		if !w.followWait(ctx, issue, wait) {
			return
		}
		state, read, err := w.readMergeState(ctx, issue, head)
		attributes := []any{"issue", issue, "head", head, "attempt", attempt, "wait", wait}
		switch {
		case err != nil:
			if ctx.Err() != nil {
				return
			}
			w.log.Warn("github: a merge state read after the ladder", append(attributes, "answered", "no answer", "error", err)...)
		case read == "":
			w.log.Info("github: the merge state reads after the ladder end: the pull request is no longer open at the head they read", attributes...)
			return
		default:
			w.log.Info("github: a merge state read after the ladder", append(attributes, "answered", string(state))...)
			if state != record.MergeStateUnknown {
				return
			}
		}
	}
}

// followWait waits wait before a read after the ladder (followMergeState), and says whether to make
// that read: false once a request for a read of issue has arrived, before the wait or during it,
// since the ladder then climbs at once (mergeStateEnds), and false when ctx ends.
func (w *workflowRuntime) followWait(ctx context.Context, issue string, wait time.Duration) bool {
	w.mergeStateMu.Lock()
	run := w.mergeStateRunning[issue]
	asked := run == nil || run.asked
	w.mergeStateMu.Unlock()
	if asked {
		return false
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-run.wake:
		return false
	case <-timer.C:
		return true
	}
}

// mergeStateWait is how long the ladder waits between reads of a head GitHub is still computing
// the merge state of, the unit the reads after it wait in (followMergeState), and how long the
// outbox's merge-queue read and second conflict read wait before they read again (mergeQueueRead,
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
// request, and there is none. So does one whose record stands at another head than head, when head
// is not "": a read after the ladder names the head it follows (followMergeState), and a climb
// names none and reads the head the record holds.
//
// The fact names the head GitHub answered for, which the engine records the answer against only
// while the record still stands at it (workflow's mergeability): this ladder reads GitHub with no
// transaction of its own, and a push can land while it waits. The ladder reads on, since what it
// answers is GitHub's state for the head it read, and the push the record took is itself a fact
// that asks for a read of the head it left (workflow's askMergeState).
func (w *workflowRuntime) readMergeState(ctx context.Context, issue, head string) (record.MergeState, string, error) {
	_, recorded, err := w.issueRecord(ctx, issue)
	if err != nil {
		return record.MergeStateUnread, "", err
	}
	if recorded == nil || recorded.State != record.PullRequestOpen || (head != "" && recorded.HeadSHA != head) {
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
