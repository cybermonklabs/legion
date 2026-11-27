package daemon

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/wait"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// mergeQueueRead runs a merge-queue read (record.MergeQueueRead): the daemon's one read of a pull
// request that GitHub reported leaving the merge queue at the row's head. GitHub's event names no
// reason for the removal, and GitHub's merge queue reports a removal in the same second as the
// merge it makes, so this read is what tells the merge from a removal once GitHub reports the
// merge. A read made before GitHub reports it answers the pull request open, and the merger is
// told of a removal that was the merge: step 5 of its role prompt reads that removal's `merged`
// reason as no removal, and the issue closes on the merge's own event (plan D12 of
// dispatch://INFRA-3274, which adds no second read for it).
//
// Each attempt first reads the issue, its tree's root and its pull request record, and finishes
// the row without reading GitHub once the issue has left merging at the row's head (mergingAt,
// workflow.MergingAt): nobody would be told of what the read says. Otherwise it reads the pull
// request once, bounded by issueRead as the per-issue read bounds its own, and logs one line for
// that read with what it came to. The row's context has no deadline, and a running row renews its
// lease, so a read GitHub never answered would hold the issue's role-message lane without end; a
// read that runs out of issueRead is a failed read. A merged answer applies the merge as the
// webhook's merge would, at the head GitHub answers for (intake.PullRequestMerged, which closes the
// issue, and changes nothing when the merge is already recorded), and nothing is sent to the
// merger. A failed read is read again after the runtime's wait (workflowRuntime.mergeStateWait),
// or after GitHub's Retry-After when that is longer (readAgainAfter), as the merge-state ladder
// reads a state GitHub has not computed yet, up to mergeStateLadderReads attempts in all. The
// bound is on attempts, and each attempt makes at most one read: the row's tries
// (record.OutboxRow.Attempts) count an attempt that a database error or a failed apply ended too,
// so the log names the attempt and not a count of reads. Any other answer, or the failed read of
// the last attempt, applies intake.MergeQueueReadDone, whose transaction writes the merger's
// message (record.MergeQueueRemoval) while the issue still merges at that head, Unread when no
// read succeeded.
//
// A read that answered and is no merge first applies what it says about merging the head
// (applyMergeability, as every read of the pull request does, the base's rules read with it on a
// conflicting answer), under an event id that names the row and the attempt: mergeability moves no
// phase - a conflicting head is read a second time and merged forward in the phase the issue
// stands in (workflow's mergeability, dispatch://INFRA-4374 R1, R2) - so recording it here moves
// nothing a removal must not move (dispatch://INFRA-3274/spec R1). It applies no synchronize of
// the head, which is the per-issue read's to apply (followHead).
//
// Every fact is applied under an event id that names the row, so a row run again after its finish
// was lost applies none a second time. The read, its apply and the wait are the runtime's, which
// attach gives the outbox: an outbox a test builds by hand has none of them, and the row fails with
// an error that names the missing read rather than reading nothing.
func (r *outbox) mergeQueueRead(ctx context.Context, row record.OutboxRow, payload record.MergeQueueRead) error {
	if r.readPullRequest == nil || r.applyMergeability == nil || r.mergeStateWait == nil {
		return errors.New("merge-queue read executor has no read of a pull request from GitHub")
	}
	_, pr, left, err := r.mergingAt(ctx, row.Issue, payload.Number, payload.Head)
	if err != nil {
		return err
	}
	if left != "" {
		r.log.Info("outbox merge-queue read finished: the issue left merging at that head",
			"row", row.ID, "issue", row.Issue, "pullRequest", payload.Number, "head", payload.Head, "why", left)
		return nil
	}
	attempt := row.Attempts + 1
	bounded, cancel := context.WithTimeout(ctx, issueRead)
	github, pull, failed := r.readPullRequest(bounded, *pr)
	cancel()
	readAt := r.now().UTC()
	attributes := []any{"row", row.ID, "issue", row.Issue, "pullRequest", payload.Number, "head", payload.Head, "attempt", attempt}
	switch {
	// GitHub's merge commit and state are its words, or those of whatever answers in its place: the
	// read decides on them as they came, and its line names each only in GitHub's shape for it.
	case failed == nil && pull.Merged:
		r.log.Info("outbox merge-queue read: GitHub answered merged; the merge is applied", append(attributes, "mergeCommit", githubrest.ShownSHA(pull.MergeCommitSHA))...)
		if _, err := intake.ApplyFact(ctx, r.pool, "outbox", fmt.Sprintf("merge-queue-merged:%s:%d", row.Issue, row.ID),
			intake.PullRequestMerged{Repo: pr.Repo, Number: payload.Number, MergeSHA: pull.MergeCommitSHA, HeadSHA: pull.Head.SHA}, r.handlers...); err != nil {
			return fmt.Errorf("apply the merge of %s's pull request #%d: %w", row.Issue, payload.Number, err)
		}
		return nil
	case failed == nil:
		applied, err := r.applyMergeability(ctx, github, *pr, pull, readAt, fmt.Sprintf("merge-queue-mergeability:%s:%d:%d", row.Issue, row.ID, attempt))
		if err != nil {
			return fmt.Errorf("apply the mergeability of %s's pull request #%d: %w", row.Issue, payload.Number, err)
		}
		r.log.Info("outbox merge-queue read: GitHub answered not merged; the merger is told", append(attributes, "state", githubrest.ShownWord(pull.State, pullStates...),
			"mergeable", string(applied.Mergeable), "mergeState", string(applied.MergeState))...)
	case attempt < mergeStateLadderReads:
		delay := r.readAgainAfter(failed)
		r.log.Warn("outbox merge-queue read: the read of GitHub failed; it reads again",
			append(attributes, "attempts", mergeStateLadderReads, "wait", delay, "error", failed)...)
		return wait.Until(readAt.Add(delay), fmt.Sprintf("the merge-queue read of %s's pull request #%d reads GitHub again in %s, after the read of attempt %d of %d failed: %v",
			row.Issue, payload.Number, delay, attempt, mergeStateLadderReads, failed))
	default:
		r.log.Error("outbox merge-queue read: the read of the last attempt failed; the merger is told that no read succeeded",
			append(attributes, "attempts", mergeStateLadderReads, "error", failed)...)
	}
	if _, err := intake.ApplyFact(ctx, r.pool, "outbox", fmt.Sprintf("merge-queue-read:%s:%d", row.Issue, row.ID),
		intake.MergeQueueReadDone{Issue: row.Issue, Number: payload.Number, Head: payload.Head, Unread: failed != nil}, r.handlers...); err != nil {
		return fmt.Errorf("apply the merge-queue read of %s's pull request #%d: %w", row.Issue, payload.Number, err)
	}
	return nil
}

// mergeQueueRemoval publishes the message of a merge-queue removal row (record.MergeQueueRemoval)
// to the Envoy topic of the issue's merger, and to nothing else: the pull request left the merge
// queue at the row's head, GitHub did not report it merged, and the event names no reason, so the
// merger reads the reason on GitHub and acts on it (step 5 of its role prompt). It stops nothing
// it reaches, as a hook prompt stops nothing.
//
// The row is held as an architect's notice is held (notice_exceptions.go, noticeAnswered), not
// ended as the row of a hook prompt nobody holds is (hookPrompt). That row can end because the
// listener keeps its prompt for the role's next holder (publishKeptToRole). Nothing else tells a
// merger of this removal: its pull request waits out of the queue with nobody to enqueue it again
// or send the issue back, and a merger whose pod was released would learn of it only when the
// release's backstop wakes it (backstop_minutes, an hour by default). So the row is delivered as
// deliverHeld delivers a held message: a released merger is woken, and the row waits until the
// session of the merger's current launch holds the role. A copy stored for the session of an
// earlier launch, whose stop has not reached the listener, would tell the merger of the new launch
// nothing.
//
// Each attempt first finishes the row undelivered, with one log line naming why, once the issue
// has left merging at the row's head (mergingAt, the read row's own rule); what follows - a claim
// that failed or retired, the design gate and the news the merger is owed, the wake and the
// publish - is deliverHeld's.
//
// A merger the design gate holds may not know the gate closed, and woken by this message it could
// enqueue the pull request again against the gate, which holds the merge; a merger woken before
// what changed is recorded could enqueue it again before it reads that change.
func (r *outbox) mergeQueueRemoval(ctx context.Context, row record.OutboxRow, payload record.MergeQueueRemoval) error {
	if r.notices == nil {
		return errors.New("merge-queue removal executor has no Envoy publisher")
	}
	if r.supervisor == nil {
		return errors.New("merge-queue removal executor has no claim supervisor")
	}
	token, err := roleToken(r.project, row, claim.RoleMerger)
	if err != nil {
		return err
	}
	issue, pr, left, err := r.mergingAt(ctx, row.Issue, payload.Number, payload.Head)
	if err != nil {
		return err
	}
	attributes := []any{"row", row.ID, "issue", row.Issue, "pullRequest", payload.Number, "head", payload.Head, "unread", payload.Unread}
	if left != "" {
		r.log.Info("outbox merge-queue removal finished undelivered: the issue left merging at that head", append(attributes, "why", left)...)
		return nil
	}
	return r.deliverHeld(ctx, row, heldMessage{issue: issue, token: token, role: claim.RoleMerger,
		what: "the merge-queue removal of " + row.Issue, message: payload.Message(row.Issue, pr.Repo), attributes: attributes,
		claimEnded: "outbox merge-queue removal finished undelivered: the merger's claim has failed or retired",
		delivered:  "outbox merge-queue removal delivered to the merger"})
}

// heldMessage is one message a row delivers to a role of its issue and holds until a session of
// that role takes it (deliverHeld): the issue, the role's claim token and the role, what names the
// message in the row's waits and errors ("the merge-queue removal of LEGION-208"), the text
// published, the attributes of the row's log lines, and the two lines the delivery itself logs: the
// row's end on a claim that failed or retired, and the delivery. Each message is for the session
// of the role's current launch: it is published only once that launch has registered its session,
// and it is delivered unless the listener names a session other than the one the claim records
// (deliverHeld). end, where set, is the row's end itself, which deletes the row: end's delivered
// argument says whether the row was delivered, and told is the launch whose session the listener
// stored the message for, 0 for the end at a claim that failed or retired, and for a delivery
// where the listener named no holder (a merge-forward ask's, endMergeForwardAsk). nil leaves the
// row's end to its settle.
type heldMessage struct {
	issue      record.Issue
	token      claim.Token
	role       claim.Role
	what       string
	message    string
	attributes []any
	claimEnded string
	delivered  string
	end        func(ctx context.Context, delivered bool, told uint64) error
}

// deliverHeld publishes m's message to the Envoy topic of m's role, and to nothing else, and holds
// the row until a session of that role takes it: the delivery a merge-queue removal, a merge holds
// change and a merge-forward message share (mergeQueueRemoval, mergeHoldsChanged,
// mergeForwardMessage), once each has read what ends its row on its own terms. It stops nothing it
// reaches, as a hook prompt stops nothing. A released role is woken (wakeForDelivery), and the row
// waits on its backoff until the woken session holds the role, or, while the wake pacing holds that
// resume, for its turn, reading the claim again at most parkedWakeBound apart (parkedWait), as it
// waits while the listener answers that nobody holds the role (notify.ErrNoHolder) or that its
// holder is unresponsive (notify.ErrHolderUnresponsive). A role with no claim yet, its start still
// owed, is no end either: the row waits for it.
//
// It reads, in this order, what ends or holds the row. The row finishes undelivered, logging
// m.claimEnded, once the role's claim has failed or retired, since that launch will not hold its
// role again. A released merger is held by the rule that holds its start
// (mergerWaitsForTheDesignGate: the design gate, and the news of the approved design it is owed),
// and a released role of another kind while the root's design gate is closed (designGateClosed):
// such a role is neither woken nor told, and the row waits, since a role the gate holds may not
// know the gate closed, and woken by this message it would work against the gate. A role whose
// session holds it is told at once, gate or none, and m.delivered is logged. The three other
// answers of the confirmed publish route keep the ordinary retry, as they do for a hook prompt: a
// holder that did not confirm in time (notify.ErrNotDelivered), one that does not advertise the
// delivery mode (notify.ErrRefusedByHolder), and a forward the listener's own NATS grant denies
// (notify.ErrPublishDenied).
//
// Each message is for the session of the role's current launch, and the listener's word is the one
// evidence of who took it: its 200 names the session it stored the message for
// (notify.Publisher.Publish). A launch still coming up (supervise.Unregistered) can resume the
// session the claim records or start a fresh one in the same launch (supervise's startFresh), and
// the listener can still name the session of an earlier launch whose clean stop has not landed,
// so the message waits while the launch has not registered its session, and once it has, it is
// delivered unless the listener names a session other than the one the claim records, as the
// daemon's forget reads the listener's holder (supervise's mayForget). A copy stored for
// another session tells this launch nothing: the row waits, and its next attempt under the same
// key is answered from the listener's record while that session still holds the role, and
// forwarded to this launch's session once it claims the role (the listener's
// settledBeforeForward).
//
// Where m has an end, both ends of the row are it: at a claim that failed or retired, undelivered,
// before the line is logged, and after the publish, delivered, with told the launch read before
// the publish where the listener stored the message for the session that launch registered, and 0
// where it named no holder. A launch's session is fixed once it registered, and a later launch is
// a later generation, so told is a launch whose session took the message. An end that fails fails
// the attempt: undelivered, the row is read again at its retry; delivered, it is published again
// and ends again, so a launch may be told twice and none is told nothing.
func (r *outbox) deliverHeld(ctx context.Context, row record.OutboxRow, m heldMessage) error {
	held := r.supervisedClaim(m.token)
	if supervise.FailedOrRetired(held.State) {
		if err := r.endUndelivered(ctx, row, m.end); err != nil {
			return err
		}
		r.log.Info(m.claimEnded, append(m.attributes, "state", held.State)...)
		return nil
	}
	if held.Released() {
		if m.role == claim.RoleMerger {
			if err := r.mergerWaitsForTheDesignGate(ctx, m.issue, m.role); err != nil {
				return err
			}
		} else if closed, err := r.designGateClosed(ctx, m.issue); err != nil {
			return err
		} else if closed {
			return wait.Errorf("%s waits for the design gate of %s to open before it wakes the released %s", m.what, m.issue.Tree, m.role)
		}
	}
	// This runs before the publish, which would otherwise answer ErrNoHolder for the released
	// role on every attempt and wait for a session nothing brings back. A launch the wake pacing
	// holds is waited for until it runs, the claim read again at its turn or parkedWakeBound on,
	// whichever comes first (parkedWait).
	if waits, turn, err := r.wakeForDelivery(ctx, m.token, m.what, row.HeldByPause); err != nil {
		return err
	} else if !turn.IsZero() {
		return r.parkedWait(turn, fmt.Sprintf("%s waits for its %s's launch, which the wake pacing holds until %s",
			m.what, m.role, turn.UTC().Format(time.RFC3339)))
	} else if waits {
		return wait.Errorf("%s waits for its %s's released pod to come back", m.what, m.role)
	}
	launch := r.supervisedClaim(m.token)
	if supervise.Unregistered(launch.State) {
		return wait.Errorf("%s waits for its %s's launch %d, which is %s, to register its session: a publish now would reach an earlier launch's session or none",
			m.what, m.role, launch.Generation, launch.State)
	}
	holder, published := r.publishTo(ctx, notify.RoleTopicPrefix+string(m.token), m.message, m.message, record.OutboxKey(row.ID))
	switch {
	case published == nil && holder != "" && launch.Session != "" && !launch.HoldsSession(holder):
		return wait.Errorf("%s was stored for session %s, which is not %s, the session of its %s's launch %d: it waits for that session to hold the role",
			m.what, holder, launch.Session, m.role, launch.Generation)
	case published == nil:
		r.log.Info(m.delivered, m.attributes...)
		if m.end != nil {
			var told uint64
			if holder != "" && launch.HoldsSession(holder) {
				told = launch.Generation
			}
			return m.end(ctx, true, told)
		}
		return nil
	case errors.Is(published, notify.ErrNoHolder):
		return wait.Errorf("%s waits for a session to hold its %s's role: %w", m.what, m.role, published)
	case errors.Is(published, notify.ErrHolderUnresponsive):
		return wait.Errorf("%s waits for its %s, whose session the listener reports %s: %w", m.what, m.role, notify.StateUnresponsive, published)
	}
	return fmt.Errorf("publish %s to its %s: %w", m.what, m.role, published)
}

// endUndelivered is the end of row, a held message's, undelivered: the test's hook between the read
// that ends it and the end (beforeUndeliveredEnd), then end, where the message has one (a
// merge-forward ask's, endMergeForwardAsk), as undelivered to no launch. Both ways a held message
// ends undelivered take it: the workflow's rule of a merge-forward message (mergeForwardMessage)
// and a claim that failed or retired (deliverHeld).
func (r *outbox) endUndelivered(ctx context.Context, row record.OutboxRow, end func(context.Context, bool, uint64) error) error {
	if r.beforeUndeliveredEnd != nil {
		r.beforeUndeliveredEnd(row)
	}
	if end == nil {
		return nil
	}
	return end(ctx, false, 0)
}

// mergingAt reads, in one transaction, what a merge-queue row reads of its issue before it acts:
// the issue, the pull request the record holds for it, and why the workflow no longer works the
// issue's merging phase at head of pull request number, "" while it does (workflow.MergingAt).
// The read row and the delivery row read it at every attempt, as every fence of a row is read just
// before the row acts, and through the read the fact between them judges the issue by (workflow's
// mergeQueueReadDone), so none of the three tells a merger of an issue another of them found gone
// from merging. An issue the record does not hold has left merging too.
func (r *outbox) mergingAt(ctx context.Context, key string, number int, head string) (record.Issue, *record.PullRequest, string, error) {
	var issue *record.Issue
	var pr *record.PullRequest
	var left string
	if err := store.TransactWith(ctx, r.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		issue, pr, left, err = workflow.MergingAt(ctx, r.records, tx, key, number, head)
		return err
	}); err != nil {
		return record.Issue{}, nil, "", fmt.Errorf("read the merging phase of %s: %w", key, err)
	}
	if issue == nil {
		return record.Issue{}, nil, "the issue is not recorded", nil
	}
	return *issue, pr, left, nil
}
