package delivery

import (
	"context"
	"fmt"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// AttributionCounts is what GET /api/v1/delivery/attribution answers, counted at the request.
// ReadsLeft is the entries of the facts line, the pull requests a pass still reads; FactsLeft the
// part of them that has no facts, R4's number; FactsRefused the part whose last read was refused.
// Reading is the identity of the reading that runs now, and KeysOnAnotherReading the texts rows
// whose keys another reading made. MovesQueued is the moves that wait for their events.
// LinksInTwoCases is the links that more than one issue holds for one thing: one pull request
// under any of its spellings, forms and names (linkedPullRequest, under its canonical name), or
// one other URL in two letter cases. OwedFailures and
// OwedRefusals count the pull requests whose head is not read while a read is owed: GitHub did
// not answer the read, or GitHub refuses it. OwedFailuresOldestAt is the oldest time that a
// failure is owed since. OwedRefusalsSince is the oldest time that a refusal is refused since, or
// owed since where an older release owed it. Each time is nil where its count is 0.
type AttributionCounts struct {
	FactsLeft            int        `json:"facts_left"`
	ReadsLeft            int        `json:"reads_left"`
	FactsRefused         int        `json:"facts_refused"`
	Reading              string     `json:"reading"`
	KeysOnAnotherReading int        `json:"keys_on_another_reading"`
	MovesQueued          int        `json:"moves_queued"`
	LinksInTwoCases      int        `json:"links_in_two_cases"`
	OwedFailures         int        `json:"owed_failures"`
	OwedFailuresOldestAt *time.Time `json:"owed_failures_oldest_at,omitempty"`
	OwedRefusals         int        `json:"owed_refusals"`
	OwedRefusalsSince    *time.Time `json:"owed_refusals_since,omitempty"`
}

// factsRefusedOf is the condition that an attributed pull request's last read of facts was
// refused: its results row's attribution_refused_at, or its timeline row's facts_refused_at where
// it has no results row.
const factsRefusedOf = `case when a.stored_repo is not null
		then exists (select 1 from pull_request_heads x where x.repo = a.stored_repo and x.number = a.number and x.attribution_refused_at is not null)
		else exists (select 1 from delivery_pull_requests x where x.repo = a.timeline_repo and x.number = a.number and x.facts_refused_at is not null) end`

// attributionCountsSelect is ReadAttributionCounts' select over the terms of the facts line.
var attributionCountsSelect = `
	select (select count(*) from attributed a where not a.has_facts),
		(select count(*) from attributed),
		(select count(*) from attributed a where ` + factsRefusedOf + `),
		(select r.id from now_reading r),
		(select count(*) from pull_request_texts t where t.reading <> (select r.id from now_reading r)),
		(select count(*) from pull_request_attribution_moves),
		(select count(*) from (select coalesce(
				(select lower(` + refs.CanonicalRepoSQL("lp.repo") + `) || '#' || lp.number from ` + LinkedPullRequestSQL("l.url") + ` lp),
				lower(` + LinkURLAsPullRequestSQL("l.url") + `))
			from issue_external_links l group by 1 having count(distinct l.issue_key) > 1) tie)`

// ReadAttributionCounts counts, at the call, what the attribution of the stored pull requests
// still waits on (AttributionCounts). It reads no setting and no GitHub. A pull request GitHub
// proved absent is in no count, and one whose texts are current and whose messages are not read
// is in ReadsLeft, and in FactsLeft only where its answer is not known. The owed reads are
// counted by one more statement, one scan of pull_request_reads_owed (owedReadCounts).
func ReadAttributionCounts(ctx context.Context, pool *store.Pool) (AttributionCounts, error) {
	statements, err := attribution()
	if err != nil {
		return AttributionCounts{}, err
	}
	var counts AttributionCounts
	if err := pool.QueryRow(ctx, statements.attributionCounts).Scan(&counts.FactsLeft, &counts.ReadsLeft, &counts.FactsRefused,
		&counts.Reading, &counts.KeysOnAnotherReading, &counts.MovesQueued, &counts.LinksInTwoCases); err != nil {
		return AttributionCounts{}, fmt.Errorf("count the attribution's work left: %w", err)
	}
	rows, err := pool.Query(ctx, owedReadCounts)
	if err != nil {
		return AttributionCounts{}, fmt.Errorf("count the pull requests owed a read: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var reason owedReason
		var count int
		var since *time.Time
		if err := rows.Scan(&reason, &count, &since); err != nil {
			return AttributionCounts{}, fmt.Errorf("count the pull requests owed a read: %w", err)
		}
		switch reason {
		case owedFailure:
			counts.OwedFailures, counts.OwedFailuresOldestAt = count, since
		case owedRefusal:
			counts.OwedRefusals, counts.OwedRefusalsSince = count, since
		}
	}
	if err := rows.Err(); err != nil {
		return AttributionCounts{}, fmt.Errorf("count the pull requests owed a read: %w", err)
	}
	return counts, nil
}

// owedReadCounts counts the reads owed of each kind, with the oldest time of each: when a failure
// was owed, and when a refusal was first refused (refused_since), or owed where a release before
// this one owed it and wrote no refused_since. A trigger that owes a failure over a refusal row
// leaves its refused_since, so a failure's time is its owed_at alone. A row recorded as unreadable
// at its revision is owed no read (recordOwedReadOutcome), so it is in neither count; the results
// of its issue name its link (UnreadableLinksOf).
const owedReadCounts = `select reason, count(*),
		min(case when reason = 'refusal' then coalesce(refused_since, owed_at) else owed_at end)
	from pull_request_reads_owed where unreadable_revision is distinct from revision group by reason`
