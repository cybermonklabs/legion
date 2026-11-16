package delivery

// A pull request whose read of GitHub has not yet answered for a change Dispatch knows of is held
// in pull_request_reads_owed until a read of GitHub answers for it (dispatch://INFRA-3992, R1 to
// R4). R2 makes GitHub's answer the only source of a head, so an event the intake gives up on
// would otherwise leave its pull request with no head for good: the reconcile reads only rows
// that exist, a merged or closed row is never read again, and the read-through runs only for a
// result's commit. The row holds which pull request changed, why its read is owed, how often the
// reconcile has read it and when it was last owed, and nothing an envelope says: no head, no
// state, no issue.
//
// Three writers record one. Intake.deliver records a failure at each delivery of an envelope whose
// head write failed, so that one refused write at one moment is never the only record, and
// acknowledges it at the delivery cap (headWriteMaxDeliveries), which a GitHub outage of about five
// minutes reaches, and a rate limit after thirty of its waits; a later delivery that writes the
// head deletes what the earlier ones owed (settleOwedReadsBefore). recordPullRequestHead records a
// refusal for GitHub's refusal of a read, a pull request it does not know or a repository the App
// cannot read, since GitHub's answer to a read made a moment after its own webhook may not know
// the pull request yet. And upsertHeadRow records a failure in the same statement as a write of
// GitHub's answer that replaces a row no read of GitHub wrote whose time is later than the answer's:
// during a roll the release before stores GitHub's newer answer from an envelope, and a slower read
// of this build can land an older answer after it, so GitHub is read again. An envelope leaves the
// intake with no record of its pull request only where no delivery of the cap could write one.
// Every other way an envelope leaves the intake unacknowledged keeps it with NATS: a stop hands it
// back (Intake.deliver), and a hold that has lapsed leaves it outstanding for the next holder.
//
// The reconcile's head pass reads the owed pull requests from GitHub before the stored rows
// (Reconcile.readOwedPullRequests): first the failures, at most PullRequestHeadBatch of them, then
// the refusals, at most RefusedReadsPerPass, each kind the row read the fewest times first and
// then the one owed longest. An answer writes the row, creating it where none existed, and
// settles the read; a failure is read again by the next pass, so it is read every pass until
// GitHub answers; a refusal settles once it has been owed owedReadRefusalFinalAfter. A read that
// answers the other way moves the row to the other kind (recordOwedReadOutcome).
//
// The two budgets are what keep a forged envelope from costing a real one its head. Any bearer of
// Envoy's /v1 publish route can name a pull request GitHub never had, and each such envelope owes
// a refusal (dispatch://INFRA-3911 owns forged envelopes). Outside a GitHub outage, refusals take
// no read from a failure, so the head of a pull request an outage or a slow read left owed is
// written by the first pass after GitHub answers again. During an outage, a pass reads up to
// RefusedReadsPerPass refusals and each read fails, so each such row becomes a failure with fewer
// reads than the real failure and goes before it: after the outage, each PullRequestHeadBatch of
// such rows can delay the real head by one more pass. A real pull request whose own read was
// refused, because GitHub's answer had not caught up with its webhook, is a refusal itself, and
// refusals go fewest reads first, then owed longest. So the first pass after the lag reads it,
// where no pass has read it yet, only when fewer than RefusedReadsPerPass refusals with no reads
// were owed before it, and each further RefusedReadsPerPass of them delay it by one pass. Where a
// pass read it while GitHub still lagged, every later refusal with no reads goes before it, so a
// flood that continues can hold it back with no fixed bound. Its next event reads GitHub at once
// regardless.
//
// No reader of a head reads this table. The results read, the CI-proved checks and the delivery
// timeline read pull_request_heads and delivery_pull_requests alone, so an owed read is never a
// head, a basis or a merge anywhere; and a release before it neither reads nor writes the table.

import (
	"context"
	"fmt"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// owedReason is why a pull request is owed a read: pull_request_reads_owed.reason.
type owedReason string

const (
	// owedFailure is a read GitHub did not answer: a 5xx, a timeout, a rate limit, the intake's
	// delivery cap; or a write of GitHub's answer older than the row it replaced (upsertHeadRow).
	// GitHub will answer it once it answers at all, so it is read before any refusal.
	owedFailure owedReason = "failure"
	// owedRefusal is a read GitHub answered with a refusal: it does not have the pull request
	// (404 or 410), or the App cannot read its repository.
	owedRefusal owedReason = "refusal"
)

// RefusedReadsPerPass is how many pull requests owed a refusal one reconcile pass reads, after
// every failure it reads. Outside a GitHub outage it bounds what forged envelopes cost: at most 50
// reads of the App's installation a pass, about 600 an hour, however many are owed, and none of
// them taken from a failure. During an outage the refusals a pass reads fail and become failures,
// so they take failure reads after it. A refusal is settled at its first read past
// owedReadRefusalFinalAfter, so a run of N forged envelopes is gone after about N/50 passes.
const RefusedReadsPerPass = 50

// owedReadRefusalFinalAfter is how long a pull request must have been owed a read before
// GitHub's refusal of the reconcile's read (a pull request it does not have, a repository the App
// cannot read) settles it. A refusal earlier than that may be GitHub's answer catching up with its
// own webhook, and is read again by the next pass. It is the reconcile's own period
// (ReconcileInterval), so a pull request GitHub never had is read by the reconcile at most twice.
const owedReadRefusalFinalAfter = ReconcileInterval

// owedReadWriteTimeout bounds the write of an owed read the intake makes at each delivery whose
// head write failed (Intake.oweEnvelopesRead), which runs on a context of its own: the message's
// own deadline may be the very failure of that delivery.
const owedReadWriteTimeout = 5 * time.Second

// owePullRequestRead records that the pull request number of the repository given changed, that
// no read of GitHub has answered for that change, and why (reason). given is canonicalised first
// (refs.CanonicalRepo), so one pull request is owed under one name. Recording it again moves its
// time to now and its reason to the latest read's: the latest event is the one a later read must
// answer for, and a read the reconcile began before it settles nothing (recordOwedReadOutcome).
// It keeps the count of the reconcile's reads, so publishing the same envelope again moves no
// row ahead of the rows that have been read as often.
func owePullRequestRead(ctx context.Context, pool *store.Pool, given string, number int, reason owedReason) error {
	repoFull, err := refs.CanonicalRepo(ctx, pool, given)
	if err != nil {
		return err
	}
	if _, _, err := splitRepo(repoFull); err != nil {
		return err
	}
	if _, err := pool.Exec(ctx, `
		insert into pull_request_reads_owed (repo, number, reason) values ($1, $2, $3)
		on conflict (repo, number) do update set reason = excluded.reason, owed_at = excluded.owed_at
	`, repoFull, number, reason); err != nil {
		return fmt.Errorf("record the read %s#%d is owed: %w", repoFull, number, err)
	}
	return nil
}

// owedReadSettleMargin is how much earlier than its own read the intake's delete of an owed read
// draws its line (settleOwedReadsBefore): a record a delivery of the same envelope wrote is at
// least headWriteRedeliverAfter older, and the margin keeps a record another writer made just
// after the read began, such as the trigger pull_request_heads_older_read, from being deleted.
const owedReadSettleMargin = time.Second

// settleOwedReadsBefore deletes the read repoFull's pull request number was owed before a read
// of GitHub that began readAge ago, now that the intake wrote that read's answer. The line is the
// database's own clock less readAge and owedReadSettleMargin, so no clock of this process is
// compared with the database's; a record owed after the read began stays for the reconcile.
func settleOwedReadsBefore(ctx context.Context, pool *store.Pool, repoFull string, number int, readAge time.Duration) error {
	if _, err := pool.Exec(ctx, `
		delete from pull_request_reads_owed
		where repo = $1 and number = $2 and owed_at < now() - make_interval(secs => $3)
	`, repoFull, number, (readAge + owedReadSettleMargin).Seconds()); err != nil {
		return fmt.Errorf("settle the read %s#%d was owed: %w", repoFull, number, err)
	}
	return nil
}

// owedRead is one pull request owed a read: its repository, its number, why, and when the read
// was last owed.
type owedRead struct {
	Repo   string
	Number int
	Reason owedReason
	OwedAt time.Time
}

// listOwedPullRequestReads is at most limit pull requests owed a read for reason, the row the
// reconcile has read the fewest times first and then the one owed longest, so every row has its
// first read before any row has its second. The index pull_request_reads_owed_order serves the
// order, so the cost is the limit's rows and not the table's.
func listOwedPullRequestReads(ctx context.Context, pool *store.Pool, reason owedReason, limit int) ([]owedRead, error) {
	rows, err := pool.Query(ctx, `
		select repo, number, reason, owed_at from pull_request_reads_owed
		where reason = $1
		order by reads, owed_at, repo, number
		limit $2
	`, reason, limit)
	if err != nil {
		return nil, fmt.Errorf("list the pull requests owed a read (%s): %w", reason, err)
	}
	defer rows.Close()
	var owed []owedRead
	for rows.Next() {
		var read owedRead
		if err := rows.Scan(&read.Repo, &read.Number, &read.Reason, &read.OwedAt); err != nil {
			return nil, fmt.Errorf("list the pull requests owed a read (%s): %w", reason, err)
		}
		owed = append(owed, read)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list the pull requests owed a read (%s): %w", reason, err)
	}
	return owed, nil
}

// owedReadOutcome is how the reconcile's read of an owed pull request ended.
type owedReadOutcome int

const (
	// owedReadAnswered: GitHub answered for the pull request, and its row is written.
	owedReadAnswered owedReadOutcome = iota
	// owedReadFailed: GitHub did not answer (owedFailure).
	owedReadFailed
	// owedReadRefused: GitHub answered that it cannot give the pull request (owedRefusal).
	owedReadRefused
)

// recordOwedReadOutcome records how the reconcile's read of read ended. An answer deletes it. A
// refusal deletes it once the pull request has been owed owedReadRefusalFinalAfter. Any other
// outcome counts the read and moves the row to the kind the read gave, so a failure GitHub now
// refuses becomes a refusal and a refusal GitHub now fails becomes a failure. Every statement names
// the time it read, so an event owed again while the reconcile read GitHub keeps its record, as
// that event recorded it, for the next pass. Both times are the database's own clock.
func recordOwedReadOutcome(ctx context.Context, pool *store.Pool, read owedRead, outcome owedReadOutcome) error {
	tag, err := pool.Exec(ctx, `
		delete from pull_request_reads_owed
		where repo = $1 and number = $2 and owed_at = $3
		  and ($4::boolean or ($5::boolean and owed_at < now() - make_interval(secs => $6)))
	`, read.Repo, read.Number, read.OwedAt, outcome == owedReadAnswered, outcome == owedReadRefused,
		owedReadRefusalFinalAfter.Seconds())
	if err != nil {
		return fmt.Errorf("settle the read %s#%d is owed: %w", read.Repo, read.Number, err)
	}
	if tag.RowsAffected() > 0 || outcome == owedReadAnswered {
		return nil
	}
	reason := owedFailure
	if outcome == owedReadRefused {
		reason = owedRefusal
	}
	if _, err := pool.Exec(ctx, `
		update pull_request_reads_owed set reason = $4, reads = reads + 1
		where repo = $1 and number = $2 and owed_at = $3
	`, read.Repo, read.Number, read.OwedAt, reason); err != nil {
		return fmt.Errorf("count the read %s#%d is owed: %w", read.Repo, read.Number, err)
	}
	return nil
}
