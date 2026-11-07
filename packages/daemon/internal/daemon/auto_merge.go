package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"
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
	github, _, repo, err := r.graphQL(ctx, row.Issue)
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
func (r *outbox) readyMerge(ctx context.Context, row record.OutboxRow, payload record.ReadyMerge) error {
	ctx, cancel := context.WithTimeout(ctx, autoMergeTimeout)
	defer cancel()
	github, logs, repo, err := r.graphQL(ctx, row.Issue)
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
	outcome, err := ghautomerge.Submit(ctx, github, logs, repo, payload.Number, payload.Head, payload.Armed, keep)
	// The queue's conflict removal is decided before the arms below, since what the row does with
	// it — a further submission, a wait for GitHub's merge state, or the end of the row — is read
	// off that decision's own answer, which those arms then handle as any other.
	var conflicted *ghautomerge.ConflictRemoval
	if errors.As(err, &conflicted) {
		outcome, err = r.conflictRemoval(ctx, row, &payload, conflicted, github, logs, repo, keep)
	}
	var armed *ghautomerge.Armed
	var failed *ghautomerge.MergeFailed
	switch {
	case errors.As(err, &armed):
		// Every arm or entry is decided, whether or not the row has seen one before: the entry
		// GitHub cannot merge is read run after run, and the marks queueConflict writes on the
		// payload stand on the copy rememberWait writes next, so neither write loses the other's.
		returned, conflict := r.queueConflict(ctx, row, &payload, armed)
		if conflict != nil || returned {
			return conflict
		}
		if remembered := r.rememberWait(ctx, row, payload, !payload.Armed, err); remembered != nil {
			return remembered
		}
		return err
	case errors.As(err, &failed):
		fact := intake.MergeFailed{Issue: row.Issue, Number: payload.Number, Head: payload.Head, Reason: failed.Error()}
		applied, applyErr := intake.ApplyFact(ctx, r.pool, "outbox", fmt.Sprintf("merge-failed:%s:%s:%s", row.Issue, payload.Head, failed.Event), fact, r.handlers...)
		if applyErr != nil {
			return fmt.Errorf("send %s back to implementing on its failed merge: %w", row.Issue, applyErr)
		}
		// The workflow moves only an issue that still awaits that READY (workflow's mergeFailed), and
		// a fact already applied moves nothing.
		message := "outbox READY merge failed in the merge queue; the workflow sends the issue back to implementing if it still awaits that READY"
		if applied.Duplicate {
			message = "outbox READY merge failed in the merge queue; that failure was already applied, so nothing changed"
		}
		r.log.Info(message, "row", row.ID, "issue", row.Issue, "number", payload.Number, "head", payload.Head, "reason", failed.Error())
		return nil
	case err != nil:
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
// entry's merge is in it — so what is left is the head conflicting with the base. Every other
// state (QUEUED, AWAITING_CHECKS, MERGEABLE, LOCKED, and one this daemon does not know), an entry
// the queue has built nothing for yet, and an entry built on anything but the base branch's tip
// do nothing at all: they neither write the marks nor clear them, since the queue rebuilds its
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
	if armed.QueueState != "UNMERGEABLE" || armed.QueueBase == "" || armed.QueueBase != armed.BaseTip {
		return false, nil
	}
	now := r.now().UTC()
	if payload.Unmergeable.IsZero() || payload.UnmergeableBase != armed.QueueBase {
		payload.Unmergeable, payload.UnmergeableBase = now, armed.QueueBase
		if err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
			_, err := r.records.ReplaceOutboxPayload(ctx, tx, row.ID, row.LeaseToken, *payload)
			return err
		}); err != nil {
			return false, fmt.Errorf("record the merge-queue entry outbox row %d found unmergeable: %w", row.ID, err)
		}
		return false, nil
	}
	if now.Sub(payload.Unmergeable) < queueConflictBound {
		return false, nil
	}
	reason := fmt.Sprintf("the head conflicts with %s: GitHub's merge queue reports its entry unmergeable while the entry is built on %s's own tip %s; merge %s forward",
		armed.BaseName, armed.BaseName, armed.QueueBase, armed.BaseName)
	fact := intake.MergeQueueConflict{Issue: row.Issue, Number: payload.Number, Head: payload.Head, Base: armed.BaseName, BaseCommit: armed.QueueBase, Reason: reason}
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

// conflictRemoval decides what the merge queue's removal of the row's entry for a conflict
// (ghautomerge.ConflictRemoval) leaves the row to do, and answers the outcome and failure
// readyMerge's own arms then handle. GitHub gives the one reason, merge_conflict, to two
// different things, and the merge state of the pull request the removal left behind, which
// GitHub computes once the entry is no longer queued, tells them apart:
//
//   - DIRTY is the head conflicting with the base itself, which no further submission can merge:
//     the removal is returned as the *Removed it is (ghautomerge.ConflictRemoval.Ended), which
//     ends the row with the architect told (permanentRefusal, merge-removed). The two-minute
//     required-checks pass reads that conflict from GitHub itself and returns the issue, naming
//     the base to merge forward.
//   - UNKNOWN is GitHub having computed no merge state yet, which decides nothing: the first such
//     read is marked on the payload (record.ReadyMerge.ConflictUnknown) and the removal is
//     returned for the row to ask again on its own backoff, until conflictUnknownBound has passed
//     from that mark and the row ends as a DIRTY one does.
//   - Any other state is GitHub still merging the head cleanly, so what the queue could not merge
//     was this head against an entry ahead of it: no check failed, the removal is nobody's to
//     undo and nothing else in Legion would ever submit that merge again. The row submits it once
//     more at the same head and leaves the issue in awaiting_merge, with its recorded head and
//     round count untouched: no fact of this reaches the workflow, and the architect's topic
//     hears nothing.
//
// The further submission is spent before it is made: the mark that records it
// (record.ReadyMerge.Resubmitted) and the message that tells the issue of it are written in one
// transaction under the row's lease, as rememberWait writes its notice, and only then is GitHub
// asked. A submission that fails after the mark stands retries once and then ends the row with
// the architect told, where a mark written after it would submit again on every run — the loop
// LEGION-800 records. A replacement that did not take, the row's lease gone to another worker,
// submits nothing and leaves that worker to read the removal itself. The mark is cleared only by
// the workflow's queue of a new READY at a new head, which replaces the whole payload, so a
// second removal of the same head, this one's own re-queue of the pull request included, ends the
// row. The entry is left where it is on every path: taking one out is the disarm's alone
// (ghautomerge.Disarm).
func (r *outbox) conflictRemoval(ctx context.Context, row record.OutboxRow, payload *record.ReadyMerge, conflict *ghautomerge.ConflictRemoval,
	github, logs githubrest.Client, repo ghrepo.Repository, keep ghautomerge.Keep) (ghautomerge.Outcome, error) {
	now := r.now().UTC()
	switch {
	case payload.Resubmitted, conflict.MergeState == "DIRTY":
		return "", conflict.Ended()
	case conflict.MergeState == "UNKNOWN":
		if !payload.ConflictUnknown.IsZero() {
			if now.Sub(payload.ConflictUnknown) >= conflictUnknownBound {
				return "", conflict.Ended()
			}
			return "", conflict
		}
		payload.ConflictUnknown = now
		if err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
			_, err := r.records.ReplaceOutboxPayload(ctx, tx, row.ID, row.LeaseToken, *payload)
			return err
		}); err != nil {
			return "", fmt.Errorf("record the merge state GitHub had not computed when outbox row %d read its conflict removal: %w", row.ID, err)
		}
		return "", conflict
	}
	payload.Resubmitted = true
	told, err := record.NewOutboxRow(row.Issue, readyMergeResubmitted(*payload, conflict), now)
	if err != nil {
		return "", err
	}
	// The message is written in the payload's own transaction, which owns the commit hook that
	// write registers (projectRecords, intake.OnCommit), so its lane is woken as the payload
	// commits.
	scoped, committed := intake.WithCommitHooks(ctx)
	var replaced bool
	if err := pgx.BeginFunc(scoped, r.pool, func(tx pgx.Tx) error {
		var err error
		if replaced, err = r.records.ReplaceOutboxPayload(scoped, tx, row.ID, row.LeaseToken, *payload); err != nil || !replaced {
			return err
		}
		return r.records.Enqueue(scoped, tx, told)
	}); err != nil {
		return "", fmt.Errorf("record the further submission outbox row %d makes after the merge queue's conflict: %w", row.ID, err)
	}
	committed()
	if !replaced {
		return "", conflict
	}
	r.log.Warn("outbox READY merge was taken out of the merge queue for a conflict GitHub's merge state leaves with an entry ahead of it; it is submitted once more",
		"row", row.ID, "issue", row.Issue, "number", payload.Number, "head", payload.Head, "mergeState", conflict.MergeState, "removal", conflict.Error())
	return ghautomerge.Submit(ctx, github, logs, repo, payload.Number, payload.Head, false, keep)
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
	var notice record.OutboxRow
	if warn {
		var err error
		if notice, err = record.NewOutboxRow(row.Issue, readyMergeWaiting(payload, waited, cause), now); err != nil {
			return err
		}
	}
	// The notice is written in the payload's own transaction, which owns the commit hook that write
	// registers (projectRecords, intake.OnCommit), so its lane is woken as the payload commits.
	scoped, committed := intake.WithCommitHooks(ctx)
	var replaced bool
	if err := pgx.BeginFunc(scoped, r.pool, func(tx pgx.Tx) error {
		var err error
		if replaced, err = r.records.ReplaceOutboxPayload(scoped, tx, row.ID, row.LeaseToken, payload); err != nil || !replaced || !warn {
			return err
		}
		return r.records.Enqueue(scoped, tx, notice)
	}); err != nil {
		return fmt.Errorf("record what outbox row %d learned while it waits: %w", row.ID, err)
	}
	committed()
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
// the message says; one that does names each, with what its job's log says failed
// (ghautomerge.Removed.Runs), so the tree reads the cause here rather than fetching a
// merge-group log.
func readyMergeResubmitted(payload record.ReadyMerge, conflict *ghautomerge.ConflictRemoval) record.MessagePost {
	checks := "and no check failed"
	if runs := conflict.Removal.Runs; len(runs) > 0 {
		checks = "while these checks failed on the commit the queue built for it: " + strings.Join(runs, "; ")
	}
	return record.MessagePost{Body: fmt.Sprintf("Legion submitted the merge of pull request #%d at READY's head %s once more: GitHub's merge queue took the pull request out at %s without merging it (%s), while GitHub reports its merge state %s. That merge state leaves the conflict with an entry ahead of it in the queue rather than with the base, %s, so nothing else would ever submit this merge again. This is the one further submission this head gets: a second removal of it for a conflict ends the merge and tells the architect.",
		payload.Number, payload.Head, conflict.Removal.At.UTC().Format(time.RFC3339), conflict.Removal.Reason, conflict.MergeState, checks)}
}

// graphQL is the pair of GitHub clients a merge row calls with, both carrying the implement App's
// token, the App that writes the issue branch and submits or disarms its pull request's merge,
// and the repository GitHub holds the one issue's record names under now (serving), which the
// token is minted for the owner of: GitHub's GraphQL route, which the merge itself is read and
// made on, and the repository's REST base, which the log of a run that failed in the merge queue
// is read under (ghautomerge.Submit).
// What a record's old name cannot do is carry a token: after a transfer the App installation that
// covers the repository is the new owner's. The address is the served name as well, though GitHub
// answers the old one too (measured: `GET /repos/twitter/bootstrap` answers 301 to
// `/repositories/2126244`, and GraphQL's `repository(owner:"twitter",name:"bootstrap")` answers
// `twbs/bootstrap`), because a write such as createBranch's POST should not depend on a redirect,
// and because the submission and the disarm its callers make name the repository in their log
// lines.
func (r *outbox) graphQL(ctx context.Context, issue string) (githubrest.Client, githubrest.Client, ghrepo.Repository, error) {
	recorded, err := r.issue(ctx, issue)
	if err != nil {
		return githubrest.Client{}, githubrest.Client{}, ghrepo.Repository{}, err
	}
	named, err := recorded.Repository()
	if err != nil {
		return githubrest.Client{}, githubrest.Client{}, ghrepo.Repository{}, err
	}
	repo := r.repositories.serving(named)
	lease, err := r.tokens.Token(ctx, appauth.Implement, repo)
	if err != nil {
		return githubrest.Client{}, githubrest.Client{}, ghrepo.Repository{}, fmt.Errorf("mint the implement App token for %s: %w", repo, err)
	}
	return githubrest.Client{Token: lease.Token, API: githubrest.GraphQLAPI(r.githubAPI)},
		githubrest.Client{Token: lease.Token, API: githubrest.RepositoryAPI(r.githubAPI, repo)}, repo, nil
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
