package daemon

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/ghautomerge"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// autoMergeTimeout bounds one auto_merge_disable or ready_merge row's GitHub calls, its token's
// mint with them: the row runs on its issue's lifecycle lane, so one unanswered call would hold
// every later lifecycle row of the issue behind it.
const autoMergeTimeout = 30 * time.Second

// removalLogReads is how long the whole of one removal's job-log reads take before the row gives
// up on what is left of them (readyMerge's readFailures): the one deadline every read of that
// removal shares, off the row's own autoMergeTimeout, which no read of a 54 MB log finishes
// inside. Each read is bounded in itself at the log client's githubrest.Client.JobLogTimeBound,
// a minute in production, and they go eight at a time (ghautomerge's jobLogReads), so three
// minutes is three such waves: twenty-four failing jobs of a merge group read at the production
// bound. Those eight are this removal's own: the process reads no more than githubrest's
// jobLogReadsAtOnce logs at once over every issue lane together, so a removal reading
// while other lanes read waits for its places inside this deadline. A read this deadline cuts
// short is a log the daemon could not read, told in that run's own line as every other unread
// log is, and the removal is recorded either way.
const removalLogReads = 3 * time.Minute

// mergeWaitWarn is how long a ready_merge row waits before its issue's architect is told once that
// GitHub has not completed the merge Legion submitted (readyMergeWaiting). It runs from
// record.ReadyMerge.Queued, the moment the workflow queued that submission: nothing in the daemon
// records when GitHub was first asked, and the queue time lives on the payload beside the flag that
// records the telling (record.ReadyMerge.Warned), so a reopen's upsert of the same issue and head,
// which replaces the payload on purpose (readyMergeConflict), resets the two together and they can
// never disagree. Measured from created_at they would: the upsert leaves it at the original
// queueing, so a reopen of an hours-old row would warn at once about a submission seconds old.
// With the CI router (INFRA-520), main requires ci-ok, which waits for every job its change
// selected, and pr-title. One of those jobs, the Playwright browser job (dispatch), takes a median
// of 46 minutes and a 90th percentile of 50, plus queue time
// (dispatch://INFRA-490/artifact/ci-router-measurement-2026-10-26-md). A merge that waits for it
// can pass an hour, so readyMergeWaiting can fire on a merge its checks will still end. The hour
// stays as one informational notice (the INFRA-490 architect's decision). It was first set when,
// measured on 2026-10-23, main required lint, typecheck, test and pr-title, test took 12-16
// minutes, and the 90th percentile of every pull_request run was 17.
const mergeWaitWarn = time.Hour

// queueConflictBound is how long GitHub's merge queue must keep reporting a ready_merge row's
// entry UNMERGEABLE, built all that time on the same commit, before the row takes the entry's own
// head to be what the queue cannot merge and sends the issue back to implementing (queueConflict).
// The row retries on the outbox's backoff, which is capped at 64 seconds (retryBackoff), so ten
// minutes is at least nine reads of the entry: one odd read — GitHub's queue rebuilding entries
// around a merge that has just landed — decides nothing, and the return completes within twenty
// minutes of the first UNMERGEABLE read, the bound plus the one backoff that follows it.
const queueConflictBound = 10 * time.Minute

// conflictUnknownBound is how long a ready_merge row keeps asking GitHub for the merge state of a
// pull request its merge queue took out for a conflict while GitHub answers UNKNOWN, having
// computed none, before it reads that removal as any other and ends the row with the architect
// told (conflictRemoval). It answers the one question the merge state decides — whether the head
// conflicts with the base itself or with an entry ahead of it in the queue — on an answer GitHub
// gave, never on its silence. GitHub computes the state of a pull request no longer queued within
// seconds, and the row asks again on the outbox's backoff, capped at 64 seconds (retryBackoff),
// so five minutes is at least six further reads before the row gives the answer up.
const conflictUnknownBound = 5 * time.Minute

// autoMergeDisable disarms pull request payload.Number as the implement App, which submits READY's
// merge (ghautomerge.Disarm), deciding by the record when it runs, never by when the row was queued:
// while the issue awaits its merge, the merger's READY commit is the head whose arm is kept, and
// any other head is disarmed. A row queued by a withdrawal the issue has since come back from so
// leaves the READY that followed armed. The record is read after GitHub, so a READY recorded while
// the row runs keeps the arm its ready_merge row made.
func (r *outbox) autoMergeDisable(ctx context.Context, row record.OutboxRow, payload record.AutoMergeDisable) error {
	ctx, cancel := context.WithTimeout(ctx, autoMergeTimeout)
	defer cancel()
	github, repo, err := r.graphQL(ctx, row.Issue)
	if err != nil {
		return err
	}
	keep := func(ctx context.Context) (string, error) {
		head, _, err := r.awaitedREADY(ctx, row.Issue)
		return head, err
	}
	disarmed, err := ghautomerge.Disarm(ctx, github, repo, payload.Number, keep)
	if err != nil {
		return err
	}
	r.log.Info("outbox disarmed a pull request", "row", row.ID, "issue", row.Issue, "repo", repo.String(), "number", payload.Number,
		"missing", disarmed.Missing, "kept", disarmed.Kept, "autoMergeDisabled", disarmed.AutoMerge, "dequeued", disarmed.Queue)
	return nil
}

// readyMerge submits the merge of an accepted READY's head as the implement App
// (ghautomerge.Submit), deciding by GitHub and then the record when it runs: only while the issue
// still awaits that READY head on an open pull request does it ask GitHub to enqueue, merge or arm
// it, and GitHub's merge state and rules decide which. A repository that allows no auto-merge, or a
// base that requires a merge queue, leaves the row waiting until GitHub reports the pull request
// mergeable, so a merge-queue base never has auto-merge armed; an arm or a queue entry leaves it
// waiting until GitHub reports the merge (ghautomerge.Armed), and a READY the record has left ends
// the row. The first arm or entry the row sees is recorded on the row (rememberWait), so a later run
// that finds it gone reads the removal from GitHub instead of submitting again: the merge queue
// failing the merge sends the issue back to implementing (intake.MergeFailed, applied through the
// workflow), and anyone else's removal ends the row with the architect told (ghautomerge.Removed,
// merge-removed). The queue's own removal for a conflict is neither of those until the pull
// request's merge state says which conflict it was (ghautomerge.ConflictRemoval, conflictRemoval):
// a head GitHub still merges cleanly conflicted with an entry ahead of it in the queue, which is
// submitted once more, and only a head that conflicts with the base itself ends the row. A
// refusal a retry would meet again (ghautomerge.Refused) and a disagreement with
// the record that has stood on GitHub longer than ghautomerge.LagBound (ghautomerge.Lagging.Stalled)
// end it too, the architect told (permanentRefusal, finishRefused); every other failure retries. A
// merge-queue entry GitHub keeps reporting unmergeable while it is built on the base branch's own
// tip sends the issue back to implementing as well (queueConflict), since nothing ahead of such an
// entry in the queue can be what makes it unmergeable. A wait GitHub has left standing longer than
// mergeWaitWarn tells the architect once that the merge is still waiting (merge-waiting,
// rememberWait), and the row waits on.
//
// A removal the row tells of names what the log of each of its failing jobs says failed: those
// reads are made once the removal is decided and only on the paths that tell of it
// (readFailures, ghautomerge.ReadFailingTests), never while GitHub's own answer is judged and
// never for a row that only waits.
func (r *outbox) readyMerge(ctx context.Context, row record.OutboxRow, payload record.ReadyMerge) error {
	lane := ctx
	ctx, cancel := context.WithTimeout(ctx, autoMergeTimeout)
	defer cancel()
	github, repo, err := r.graphQL(ctx, row.Issue)
	if err != nil {
		return err
	}
	// The record ends the row only when it awaits no READY head or has applied a close: a close
	// GitHub shows first leaves the row waiting for the record (ghautomerge.Lagging).
	keep := func(ctx context.Context) (string, error) {
		head, open, err := r.awaitedREADY(ctx, row.Issue)
		if !open {
			head = ""
		}
		return head, err
	}
	// The further submission the queue's conflict removal can call for is this same submission at
	// the same head, with nothing armed: it is built here, where keep is, so the decision that
	// makes it takes the submission rather than the four values it is made of.
	resubmit := func(ctx context.Context) (ghautomerge.Outcome, error) {
		return ghautomerge.Submit(ctx, github, repo, payload.Number, payload.Head, false, keep)
	}
	// logs is the repository's REST base, where a failing job's log lives, under this row's own
	// token (readFailures); told is the budget everything the row does after those reads runs
	// on. The reads take up to removalLogReads, so the context this run opened with can be long
	// spent by the time they answer, and a write on it would fail after a submission that
	// succeeded.
	logs := githubrest.Client{Token: github.Token, API: githubrest.RepositoryAPI(r.githubAPI, repo)}
	told := func() (context.Context, context.CancelFunc) { return context.WithTimeout(lane, autoMergeTimeout) }
	outcome, err := ghautomerge.Submit(ctx, github, repo, payload.Number, payload.Head, payload.Armed, keep)
	// The queue's conflict removal is decided before the arms below, since what the row does with
	// it — a further submission, a wait for GitHub's merge state, or the end of the row — is read
	// off that decision's own answer, which those arms then handle as any other. The reading, the
	// message and the further submission a resubmission is made of are this row's, made here
	// where the budget they run on is.
	var conflicted *ghautomerge.ConflictRemoval
	if errors.As(err, &conflicted) {
		decided, failure := r.conflictRemoval(ctx, row, &payload, conflicted)
		switch {
		case failure != nil:
			return failure
		case decided == conflictEnds:
			err = conflicted.Ended()
		case decided == conflictWaits:
			err = conflicted
		default:
			// The message names what failed on the commit the queue built, so those logs are read
			// once the decision to submit again is made and before the message is written. What
			// the further submission then answers is handled by the arms below, which write the
			// row's payload: they run on the budget opened here, never on the one the reads
			// spent.
			if read := r.readFailures(lane, logs, conflicted.Removal.Runs); read != nil {
				return read
			}
			fresh, cancel := told()
			defer cancel()
			ctx = fresh
			outcome, err = r.resubmitAfterConflict(ctx, row, &payload, conflicted, resubmit)
		}
	}
	var armed *ghautomerge.Armed
	var failed *ghautomerge.MergeFailed
	var removed *ghautomerge.Removed
	switch {
	case errors.As(err, &armed):
		// Every arm or entry is decided, whether or not the row has seen one before: the entry
		// GitHub cannot merge is read run after run, and the marks queueConflict writes on the
		// payload stand on the copy rememberWait writes next, so neither write loses the other's.
		returned, failure := r.queueConflict(ctx, row, &payload, armed)
		if failure != nil || returned {
			return failure
		}
		if remembered := r.rememberWait(ctx, row, payload, !payload.Armed, err); remembered != nil {
			return remembered
		}
		return err
	case errors.As(err, &failed):
		// A failure already applied is one a run of this row has already told of: the issue went
		// back to implementing with the tests that run read, and this run's apply would change
		// nothing. The mark is read before the logs are, so a retry — the apply that failed
		// after its mark stood, a worker whose settle was lost — reads no log a second time for
		// a decision already made.
		key := fmt.Sprintf("merge-failed:%s:%s:%s", row.Issue, payload.Head, failed.Event)
		applied, failure := r.factApplied(ctx, key)
		if failure != nil {
			return fmt.Errorf("read whether %s was already sent back to implementing on its failed merge: %w", row.Issue, failure)
		}
		if applied {
			r.log.Info("outbox READY merge failed in the merge queue; that failure was already applied, so nothing changed",
				"row", row.ID, "issue", row.Issue, "number", payload.Number, "head", payload.Head, "reason", failed.Error())
			return nil
		}
		if read := r.readFailures(lane, logs, failed.Runs); read != nil {
			return read
		}
		ctx, cancel := told()
		defer cancel()
		fact := intake.MergeFailed{Issue: row.Issue, Number: payload.Number, Head: payload.Head, Reason: failed.Error()}
		sent, applyErr := intake.ApplyFact(ctx, r.pool, "outbox", key, fact, r.handlers...)
		if applyErr != nil {
			return fmt.Errorf("send %s back to implementing on its failed merge: %w", row.Issue, applyErr)
		}
		// The workflow moves only an issue that still awaits that READY (workflow's mergeFailed), and
		// a fact already applied moves nothing.
		message := "outbox READY merge failed in the merge queue; the workflow sends the issue back to implementing if it still awaits that READY"
		if sent.Duplicate {
			message = "outbox READY merge failed in the merge queue; that failure was already applied, so nothing changed"
		}
		r.log.Info(message, "row", row.ID, "issue", row.Issue, "number", payload.Number, "head", payload.Head, "reason", failed.Error())
		return nil
	case err != nil:
		// A removal that ends the row is told by the notice its finish writes (readyMergeEnded),
		// which renders this error: its failing runs are read here, and only here, where every
		// other answer — a wait, a refusal, a read of GitHub that failed — names none and reads
		// none.
		if errors.As(err, &removed) {
			if read := r.readFailures(lane, logs, removed.Runs); read != nil {
				return read
			}
			fresh, cancel := told()
			defer cancel()
			ctx = fresh
		}
		if remembered := r.rememberWait(ctx, row, payload, false, err); remembered != nil {
			return remembered
		}
		return err
	}
	r.log.Info("outbox submitted a READY merge", "row", row.ID, "issue", row.Issue, "repo", repo.String(), "number", payload.Number, "head", payload.Head, "outcome", outcome)
	return nil
}

// queueConflict decides the one merge-queue state in which the entry's own head is what GitHub's
// queue cannot merge, and says whether it sent the issue back to implementing, which ends the row:
// the queue reports the entry UNMERGEABLE (armed.QueueState) while it has built that entry on the
// tip of the pull request's own base branch (armed.QueueBase, armed.BaseTip, both from the one
// read). An entry built on that tip has nothing ahead of it in the queue to blame — no earlier
// entry's merge is in it — so what is left is the head conflicting with the base. An entry behind
// another carries the commit the merge queue built for the entry ahead instead, whose parent is
// the base branch's tip and which is no pull request's head: measured 2026-11-08 00:43:57Z on
// this repository, entry 2 (#2859, UNMERGEABLE) carried baseCommit b66b78d6d9f3, the queue's own
// build for entry 1 (#2234, head 7c4463b8ac8b) on main's tip 1f46e1cc76f4. Every other state
// (QUEUED, AWAITING_CHECKS, MERGEABLE, LOCKED, and one this daemon does not know), an entry the
// queue has built nothing for yet, and an entry built on anything but the base branch's tip do
// nothing at all: they neither write the marks nor clear them, since the queue rebuilds its
// entries as it merges what stands before them and a state read in the middle of that decides
// nothing. GitHub's mergeable and mergeStateStatus say nothing here and are never read for it:
// while an entry is queued GitHub answers UNKNOWN for both, which is what left a conflicting pull
// request sitting in the queue for hours (dispatch://INFRA-1442).
//
// The first such read is marked on the payload (record.ReadyMerge.Unmergeable,
// UnmergeableBase), one transaction under the row's lease as rememberWait writes Warned, and a
// replacement that did not take — the row's lease gone to another worker — changes nothing, since
// that worker reads the entry itself. An entry GitHub rebuilds onto another base commit replaces
// the mark and starts the clock again. Only a mark that has stood queueConflictBound at the same
// base commit returns the issue, through the workflow (intake.MergeQueueConflict) under a key of
// this issue, head and base commit, so a later read of the same entry applies nothing twice. The
// entry is left where it is: the merge queue's own removal is the disarm's (ghautomerge.Disarm),
// which the withdrawal the workflow makes queues.
func (r *outbox) queueConflict(ctx context.Context, row record.OutboxRow, payload *record.ReadyMerge, armed *ghautomerge.Armed) (bool, error) {
	if !armed.UnmergeableOnBaseTip() {
		return false, nil
	}
	now := r.now().UTC()
	if payload.Unmergeable.IsZero() || payload.UnmergeableBase != armed.QueueBase {
		payload.Unmergeable, payload.UnmergeableBase = now, armed.QueueBase
		if _, err := r.replacePayload(ctx, row, *payload, nil); err != nil {
			return false, fmt.Errorf("record the merge-queue entry outbox row %d found unmergeable: %w", row.ID, err)
		}
		return false, nil
	}
	if now.Sub(payload.Unmergeable) < queueConflictBound {
		return false, nil
	}
	reason := fmt.Sprintf("the head conflicts with %s: GitHub's merge queue reports its entry unmergeable while the entry is built on %s's own tip %s; merge %s forward",
		armed.BaseName, armed.BaseName, armed.QueueBase, armed.BaseName)
	fact := intake.MergeQueueConflict{Issue: row.Issue, Number: payload.Number, Head: payload.Head, Reason: reason}
	key := fmt.Sprintf("merge-queue-conflict:%s:%s:%s", row.Issue, payload.Head, armed.QueueBase)
	applied, err := intake.ApplyFact(ctx, r.pool, "outbox", key, fact, r.handlers...)
	if err != nil {
		return false, fmt.Errorf("send %s back to implementing on its merge-queue conflict: %w", row.Issue, err)
	}
	// The workflow moves only an issue that still awaits that READY (workflow's
	// mergeQueueConflict), and a fact already applied moves nothing.
	message := "outbox READY merge is unmergeable in the merge queue on its base branch's tip; the workflow sends the issue back to implementing if it still awaits that READY"
	if applied.Duplicate {
		message = "outbox READY merge is unmergeable in the merge queue on its base branch's tip; that conflict was already applied, so nothing changed"
	}
	r.log.Info(message, "row", row.ID, "issue", row.Issue, "number", payload.Number, "head", payload.Head, "base", armed.BaseName, "baseCommit", armed.QueueBase)
	return true, nil
}

// conflictDecision is what the merge queue's removal of a row's entry for a conflict leaves the
// row to do, as conflictRemoval reads it off the pull request's merge state and the row's own
// payload.
type conflictDecision int

const (
	// conflictEnds is the removal standing as any other does: the row ends with the architect
	// told (ghautomerge.ConflictRemoval.Ended, permanentRefusal, merge-removed).
	conflictEnds conflictDecision = iota
	// conflictWaits is GitHub having computed no merge state yet: the row asks again on its own
	// backoff, telling nobody.
	conflictWaits
	// conflictResubmits is the merge submitted once more at the same head
	// (resubmitAfterConflict).
	conflictResubmits
)

// conflictRemoval decides what the merge queue's removal of the row's entry for a conflict
// (ghautomerge.ConflictRemoval) leaves the row to do, and answers that decision and nothing
// else: it reads no log, posts nothing and submits nothing, so readyMerge makes each of those
// on a budget of its own. GitHub gives the one reason, merge_conflict, to two different things,
// and the merge state of the pull request the removal left behind, which GitHub computes once
// the entry is no longer queued, tells them apart:
//
//   - DIRTY is the head conflicting with the base itself, which no further submission can merge:
//     the row ends (conflictEnds), with the architect told of the removal as of any other
//     (ghautomerge.ConflictRemoval.Ended, permanentRefusal, merge-removed). The two-minute
//     required-checks pass reads that conflict from GitHub itself and returns the issue, naming
//     the base to merge forward.
//   - UNKNOWN is GitHub having computed no merge state yet, which decides nothing: the first such
//     read is marked on the payload (record.ReadyMerge.ConflictUnknown) and the row asks again
//     on its own backoff (conflictWaits), until conflictUnknownBound has passed from that mark
//     and the row ends as a DIRTY one does.
//   - Any other state is GitHub still merging the head cleanly, so what the queue could not merge
//     was this head against an entry ahead of it: no check failed, the removal is nobody's to
//     undo and nothing else in Legion would ever submit that merge again. The merge is submitted
//     once more at the same head (conflictResubmits) and the issue stays in awaiting_merge, with
//     its recorded head and round count untouched: no fact of this reaches the workflow, and the
//     architect's topic hears nothing.
//
// A head whose further submission is already spent (record.ReadyMerge.Resubmitted) ends the row
// whatever the merge state says: the mark is cleared only by the workflow's queue of a new READY
// at a new head, which replaces the whole payload, so a second removal of the same head, this
// row's own re-queue of the pull request included, ends it. The entry is left where it is on
// every path: taking one out is the disarm's alone (ghautomerge.Disarm).
func (r *outbox) conflictRemoval(ctx context.Context, row record.OutboxRow, payload *record.ReadyMerge, conflict *ghautomerge.ConflictRemoval) (conflictDecision, error) {
	now := r.now().UTC()
	switch {
	case payload.Resubmitted, conflict.WithBase():
		return conflictEnds, nil
	case !conflict.Uncomputed():
		return conflictResubmits, nil
	case payload.ConflictUnknown.IsZero():
		payload.ConflictUnknown = now
		if _, err := r.replacePayload(ctx, row, *payload, nil); err != nil {
			return conflictWaits, fmt.Errorf("record the merge state GitHub had not computed when outbox row %d read its conflict removal: %w", row.ID, err)
		}
		return conflictWaits, nil
	case now.Sub(payload.ConflictUnknown) >= conflictUnknownBound:
		return conflictEnds, nil
	default:
		return conflictWaits, nil
	}
}

// resubmitAfterConflict submits the merge of READY's head once more after the merge queue took
// its entry out for a conflict GitHub's merge state leaves with an entry ahead of it
// (conflictResubmits), and answers the outcome and failure readyMerge's own arms then handle.
//
// The submission is spent before it is made: the mark that records it
// (record.ReadyMerge.Resubmitted) and the message that tells the issue of it are written in one
// transaction under the row's lease, as rememberWait writes its notice, and only then is GitHub
// asked. A submission that fails after the mark stands retries once and then ends the row with
// the architect told, where a mark written after it would submit again on every run — the loop
// LEGION-800 records. A replacement that did not take, the row's lease gone to another worker,
// submits nothing and leaves that worker to read the removal itself.
//
// The message names what failed on the commit the queue built, so readyMerge reads those logs
// before this runs: a merge state GitHub has not computed tells nobody anything, so the row
// that waits for one reads none, and the branch that ends the row leaves the reading to the arm
// that tells of its *Removed. resubmit is the further submission, built by readyMerge beside
// the keep both submissions ask: the same submission at the same head with nothing armed. ctx
// is the budget readyMerge opened after those reads, since a write on the one the run opened
// with would fail after a submission that succeeded.
func (r *outbox) resubmitAfterConflict(ctx context.Context, row record.OutboxRow, payload *record.ReadyMerge, conflict *ghautomerge.ConflictRemoval,
	resubmit func(context.Context) (ghautomerge.Outcome, error)) (ghautomerge.Outcome, error) {
	payload.Resubmitted = true
	told, err := record.NewOutboxRow(row.Issue, readyMergeResubmitted(*payload, conflict), r.now().UTC())
	if err != nil {
		return "", err
	}
	// The message is enqueued in the payload's own transaction (replacePayload), so its lane is
	// woken as the payload commits and a replacement that did not take writes neither.
	replaced, err := r.replacePayload(ctx, row, *payload, &told)
	if err != nil {
		return "", fmt.Errorf("record the further submission outbox row %d makes after the merge queue's conflict: %w", row.ID, err)
	}
	if !replaced {
		return "", conflict
	}
	r.log.Warn("outbox READY merge was taken out of the merge queue for a conflict GitHub's merge state leaves with an entry ahead of it; it is submitted once more",
		"row", row.ID, "issue", row.Issue, "number", payload.Number, "head", payload.Head, "mergeState", conflict.MergeState, "removal", conflict.Error())
	return resubmit(ctx)
}

// readFailures fills in what the log of each of a removal's failing jobs says failed
// (ghautomerge.ReadFailingTests), and answers the one failure of that reading the row acts on:
// the lane cancelled under it. It is called once a removal is decided and only where the row is
// about to tell of it — the fact that sends the issue back, the notice that ends the row, the
// message its further submission is posted with — so a run that waits reads no log at all, a
// decision already applied is never read for again, and a refusal or a read of GitHub that
// failed names no job to read.
//
// The reads run off the row's own 30 seconds, on the lane's context: a read the row's budget
// cut short reports "could not read" for exactly the logs this reads — job 128558893568 of this
// repository is 54,330,939 bytes — while a stop of the daemon itself must still end them, which
// the lane carries. One removal's reads share the one deadline (removalLogReads) rather than
// taking a read's own bound each, and a read that deadline ends says so in its run's line
// (ghautomerge's readsEnded). Holding the row that long costs the issue's lifecycle lane and
// nothing else: the removal path arms nothing while a read runs, and the reads go eight at a
// time (ghautomerge's jobLogReads), so a removal costs about one read's wall time rather than
// the sum of its logs'.
//
// A lane cancelled while they ran is the daemon stopping, and every line those reads wrote then
// says so rather than what the job's log held. Telling the removal with those lines would spend
// its one telling on them: a *Removed ends its row, and merge-removed is written once and never
// again. So the row fails here instead, with an error nobody is told of and no mark behind it.
// The row is due again when the daemon comes back, and reads the same removal from GitHub.
func (r *outbox) readFailures(lane context.Context, logs githubrest.Client, runs []ghautomerge.FailingRun) error {
	reads, stopReads := context.WithTimeout(lane, removalLogReads)
	defer stopReads()
	ghautomerge.ReadFailingTests(reads, logs, runs)
	if lane.Err() != nil {
		return fmt.Errorf("read what the logs of the jobs the merge queue's commit failed say failed: %w", lane.Err())
	}
	return nil
}

// replacePayload writes payload back to row under the row's lease, with told enqueued in the same
// transaction where there is one to enqueue (nil where there is none), and answers whether the
// replacement took. It is the one lease-guarded payload write a merge row makes: the marks a
// decision leaves for the row's next run (queueConflict's unmergeable entry, conflictRemoval's
// uncomputed merge state), the message the further submission is told by
// (resubmitAfterConflict) and the notice of a wait (rememberWait).
//
// A replacement that did not take — the row's lease gone to another worker — is no failure, since
// that worker reads GitHub itself; nothing at all is written then, told included, so a payload
// never records something as told without the row that tells it. A write that failed is a
// failure, and each caller says in its own sentence which write it was. The transaction owns the
// commit hook write registers (projectRecords, intake.OnCommit), so an enqueued row's lane is
// woken as the payload commits.
func (r *outbox) replacePayload(ctx context.Context, row record.OutboxRow, payload record.ReadyMerge, told *record.OutboxRow) (bool, error) {
	scoped, committed := intake.WithCommitHooks(ctx)
	var replaced bool
	if err := pgx.BeginFunc(scoped, r.pool, func(tx pgx.Tx) error {
		var err error
		if replaced, err = r.records.ReplaceOutboxPayload(scoped, tx, row.ID, row.LeaseToken, payload); err != nil || !replaced || told == nil {
			return err
		}
		return r.records.Enqueue(scoped, tx, *told)
	}); err != nil {
		return false, err
	}
	committed()
	return replaced, nil
}

// factApplied reports whether the fact under key has already been applied, the mark
// intake.ApplyFact takes for itself (record.Store.Processed, source "outbox"): the one question
// a merge row asks before it spends a removal's log reads on a decision another run of it has
// already told the workflow (readyMerge). A fact already applied is one ApplyFact would answer
// Duplicate for, whatever this row built its reason from, so the reason is never built.
func (r *outbox) factApplied(ctx context.Context, key string) (bool, error) {
	var applied bool
	if err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		var err error
		applied, err = r.records.Processed(ctx, tx, "outbox", key)
		return err
	}); err != nil {
		return false, err
	}
	return applied, nil
}

// rememberWait records on row what its wait learned that its next run must know, and tells the
// issue's architect of a merge GitHub is taking long over, in one transaction under row's lease, so
// a run writes the payload at most once. arm is GitHub having shown READY's head armed or queued
// (record.ReadyMerge.Armed): from then on the arm disappearing is a removal the row reads from
// GitHub rather than a cue to submit again. A submission queued longer ago than mergeWaitWarn
// whose cause is GitHub not having completed the merge (waitingOnGitHub), and which has not told
// its architect yet, carries readyMergeWaiting's notice, enqueued in the same transaction as the
// payload that records the telling (record.ReadyMerge.Warned), so the row is never recorded as
// warned without its notice, nor the architect told twice.
//
// The notice names one thing, GitHub not having completed the merge Legion submitted, so the only
// causes that carry it are the three that say exactly that: an arm or a merge-queue entry GitHub
// has yet to merge (ghautomerge.Armed), a pull request GitHub does not report mergeable yet
// (ghautomerge.Unmergeable), and the queue's removal for the merge itself (ghautomerge.Merging).
// readyMerge calls this for every failure its other arms leave, and each of the rest is silent
// here for its own reason. GitHub and the record disagreeing — a pull request GitHub shows closed
// or at another head (ghautomerge.Lagging), an arm gone from GitHub with nothing on its timeline
// yet to say who took it (ghautomerge.Disappeared) — is the case where something has gone wrong
// with the row, while the notice's premise is that nothing has: it would tell the architect the
// merge has not been withdrawn and the daemon goes on submitting it while quoting a cause whose
// own words are the opposite. What telling a disagreement has is finishRefused's, and it differs
// by kind: a ghautomerge.Lagging's is merge-stalled, once it has stood past ghautomerge.LagBound;
// a ghautomerge.Disappeared's is merge-removed if GitHub names what took the arm away, and nothing
// at all if it never does, since Submit then answers Disappeared on every run, the row neither
// ending nor telling anyone and the outbox's own log line for the failed row its only signal. A
// refusal a retry would meet again (ghautomerge.Refused) and a removal (ghautomerge.Removed) end
// the row, and finishRefused tells the architect of each itself. A GitHub read that failed, a
// mutation refusal a retry may not meet again and the re-read after it, a read carrying no
// updatedAt, and the record read that decides the row, which is a Postgres error, are the daemon's
// own trouble: each retries on the row's backoff and must spend neither the one notice nor render
// its own error as GitHub's last answer, and the genuine wait that follows is what the architect is
// to be told of. Naming the three that carry the notice leaves a wait type added to ghautomerge
// later silent by default rather than wrong by default. A reopen's queue that took the row since
// starts it over unarmed, unwarned and freshly queued, and its own payload stands: the replacement
// writes nothing then, and the notice rolls back with it.
func (r *outbox) rememberWait(ctx context.Context, row record.OutboxRow, payload record.ReadyMerge, arm bool, cause error) error {
	now := r.now().UTC()
	// A row a daemon before record.ReadyMerge.Queued enqueued carries no queue time: its created_at
	// is what its wait was measured from then, and reading the zero time as an infinite wait would
	// warn on the row's first run after the upgrade.
	queued := payload.Queued
	if queued.IsZero() {
		queued = row.CreatedAt
	}
	waited := now.Sub(queued)
	warn := !payload.Warned && waited > mergeWaitWarn && waitingOnGitHub(cause)
	if !arm && !warn {
		return nil
	}
	payload.Armed = payload.Armed || arm
	payload.Warned = payload.Warned || warn
	// The notice is enqueued in the payload's own transaction (replacePayload), so the row is
	// never recorded as warned without it, a replacement that did not take writes neither, and
	// the notice's lane is woken as the payload commits.
	var notice *record.OutboxRow
	if warn {
		told, err := record.NewOutboxRow(row.Issue, readyMergeWaiting(payload, waited, cause), now)
		if err != nil {
			return err
		}
		notice = &told
	}
	replaced, err := r.replacePayload(ctx, row, payload, notice)
	if err != nil {
		return fmt.Errorf("record what outbox row %d learned while it waits: %w", row.ID, err)
	}
	if warn && replaced {
		r.log.Warn("outbox READY merge is still waiting on GitHub; its architect is told once", "row", row.ID, "issue", row.Issue,
			"number", payload.Number, "head", payload.Head, "waited", waited.Round(time.Minute), "error", cause)
	}
	return nil
}

// waitingOnGitHub says whether cause is GitHub not having completed the merge it was asked for —
// the one thing merge-waiting asserts. A Lagging or a Disappeared is a wait too, but each is
// GitHub and the record disagreeing, whose single telling is finishRefused's.
func waitingOnGitHub(cause error) bool {
	var armed *ghautomerge.Armed
	var unmergeable *ghautomerge.Unmergeable
	var merging *ghautomerge.Merging
	return errors.As(cause, &armed) || errors.As(cause, &unmergeable) || errors.As(cause, &merging)
}

// readyMergeEnded is the architect's notice of a READY merge row ended without a merge by cause:
// GitHub's refusal, which a retry would meet again (merge-refused), GitHub showing the pull
// request closed or at another head than the record awaits since a change older than
// ghautomerge.LagBound, a webhook the daemon never received (merge-stalled), or a person, or GitHub
// on its own, taking it out of the merge queue or turning its auto-merge off at READY's head, not
// because the merge failed (merge-removed).
func readyMergeEnded(cause error) record.Notice {
	var lagging *ghautomerge.Lagging
	var removed *ghautomerge.Removed
	switch {
	case errors.As(cause, &lagging):
		return record.Notice{Kind: "merge-stalled", Role: claim.RoleArchitect,
			Reason: fmt.Sprintf("GitHub has shown pull request #%d %s at %s since %s, longer than %s, while the record awaits its merge at READY's head %s: the daemon never received that change's webhook, so it stopped submitting READY's merge",
				lagging.Number, lagging.State, lagging.GitHubHead, lagging.Since.UTC().Format(time.RFC3339), ghautomerge.LagBound, lagging.Head)}
	case errors.As(cause, &removed):
		return record.Notice{Kind: "merge-removed", Role: claim.RoleArchitect, Reason: removed.Error()}
	}
	return record.Notice{Kind: "merge-refused", Role: claim.RoleArchitect, Reason: cause.Error()}
}

// readyMergeWaiting is the architect's notice of a merge the daemon submitted that GitHub has not
// completed within mergeWaitWarn of the submission's queueing (merge-waiting,
// record.ReadyMerge.Queued): the pull request, READY's head, how long it has waited and what
// GitHub last answered, cause. cause is GitHub not having completed the merge and nothing else
// (waitingOnGitHub: ghautomerge.Armed, Unmergeable or Merging), since the notice says exactly
// that and renders cause as GitHub's last answer: a disagreement between GitHub and the record,
// a refusal, a removal, a GitHub read that failed, a record read that failed or a mutation
// refusal a retry may not meet again either says nothing of what GitHub waits for or says the
// opposite of this notice, and would spend the one notice on it (rememberWait). Nothing has gone wrong with the row — the merge has not been
// withdrawn, the issue stays in awaiting_merge and the daemon goes on submitting it — so the
// architect is told once (rememberWait, record.ReadyMerge.Warned) and what is left to find is on
// GitHub: a required check that never ran or failed, a review only a person can give, a merge
// queue that has not landed it.
func readyMergeWaiting(payload record.ReadyMerge, waited time.Duration, cause error) record.Notice {
	return record.Notice{Kind: "merge-waiting", Role: claim.RoleArchitect,
		Reason: fmt.Sprintf("Legion submitted the merge of pull request #%d at READY's head %s %s ago and GitHub has not completed it; GitHub last answered: %s. The merge has not been withdrawn, the issue stays in awaiting_merge and the daemon goes on submitting it: read the pull request on GitHub to find what GitHub is waiting for",
			payload.Number, payload.Head, waited.Round(time.Minute), cause)}
}

// readyMergeResubmitted is the message posted on the issue when the merge queue took its pull
// request out for a conflict GitHub's merge state leaves with an entry ahead of it, and the row
// submitted the merge once more (conflictRemoval): the removal GitHub recorded, the head it was
// made at, and the submission the row has now spent on that head. It is written on the issue and
// nowhere else: nothing has gone wrong and nothing is anyone's to do — the issue stays in
// awaiting_merge at the same head and the daemon submits it — so the architect's topic is left
// alone, and the issue's own record carries why a second submission of the same head stands on
// it. The commit the queue built for such a removal usually carries no failing run at all, which
// the message says; one that does names each, with what its job's log says failed, in the one
// sentence every telling of a removal names them with (ghautomerge.Removed.Failures), so the
// tree reads the cause here rather than fetching a merge-group log.
func readyMergeResubmitted(payload record.ReadyMerge, conflict *ghautomerge.ConflictRemoval) record.MessagePost {
	checks := "No check failed on the commit the queue built for it."
	if failures := conflict.Removal.Failures(); failures != "" {
		checks = failures + "."
	}
	return record.MessagePost{Body: fmt.Sprintf("Legion submitted the merge of pull request #%d at READY's head %s once more: GitHub's merge queue took the pull request out at %s without merging it (%s), while GitHub reports its merge state %s. That merge state leaves the conflict with an entry ahead of it in the queue rather than with the base, so nothing else would ever submit this merge again. %s This is the one further submission this head gets: a second removal of it for a conflict ends the merge and tells the architect.",
		payload.Number, payload.Head, conflict.Removal.At.UTC().Format(time.RFC3339), conflict.Removal.Reason, conflict.MergeState, checks)}
}

// graphQL is the GitHub client a merge row calls with, carrying the implement App's token, the
// App that writes the issue branch and submits or disarms its pull request's merge, and the
// repository GitHub holds the one issue's record names under now (serving), which the token is
// minted for the owner of: GitHub's GraphQL route, which the merge itself is read and made on.
// A row that reads a failing job's log builds the repository's REST base where it reads it,
// from this client's own token (readyMerge's logs, readFailures), since that read is one
// answer's alone and no merge row else touches that route.
//
// What a record's old name cannot do is carry a token: after a transfer the App installation that
// covers the repository is the new owner's. The address is the served name as well, though GitHub
// answers the old one too (measured: `GET /repos/twitter/bootstrap` answers 301 to
// `/repositories/2126244`, and GraphQL's `repository(owner:"twitter",name:"bootstrap")` answers
// `twbs/bootstrap`), because a write such as createBranch's POST should not depend on a redirect,
// and because the submission and the disarm its callers make name the repository in their log
// lines.
func (r *outbox) graphQL(ctx context.Context, issue string) (githubrest.Client, ghrepo.Repository, error) {
	recorded, err := r.issue(ctx, issue)
	if err != nil {
		return githubrest.Client{}, ghrepo.Repository{}, err
	}
	named, err := recorded.Repository()
	if err != nil {
		return githubrest.Client{}, ghrepo.Repository{}, err
	}
	repo := r.repositories.serving(named)
	lease, err := r.tokens.Token(ctx, appauth.Implement, repo)
	if err != nil {
		return githubrest.Client{}, ghrepo.Repository{}, fmt.Errorf("mint the implement App token for %s: %w", repo, err)
	}
	return githubrest.Client{Token: lease.Token, API: githubrest.GraphQLAPI(r.githubAPI)}, repo, nil
}

// awaitedREADY is the READY head issue awaits the merge of, and whether the record holds its pull
// request open. The head is the merger's READY commit while the issue awaits its merge, and none in
// any other phase, where no arm is legitimate, nor for a READY a person merges (its by-hand note,
// record.PhaseRow.MergeNote), which covers no arm, nor in a tree that lingers after its root left the
// workflow, whose members beginLinger disarms. A disarm keeps the arm of that head; a READY merge
// also needs the pull request open, since a close the record has applied ends the row and the
// close already told the architect.
func (r *outbox) awaitedREADY(ctx context.Context, issue string) (string, bool, error) {
	var head string
	var open bool
	if err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		current, err := r.records.Issue(ctx, tx, issue)
		if err != nil || current == nil || current.Phase != phase.AwaitingMerge {
			return err
		}
		if lingers, err := record.TreeLingers(ctx, r.records, tx, current.Tree); err != nil || lingers {
			return err
		}
		pr, err := r.records.PullRequest(ctx, tx, issue)
		if err != nil {
			return err
		}
		open = pr != nil && pr.State == record.PullRequestOpen
		phases, err := r.records.Phases(ctx, tx, issue)
		if err != nil {
			return err
		}
		for _, row := range phases {
			if row.Role == claim.RoleMerger && row.MergeNote == "" {
				head = row.HandoffCommit
			}
		}
		return nil
	}); err != nil {
		return "", false, fmt.Errorf("read the READY head %s awaits: %w", issue, err)
	}
	return head, open, nil
}
