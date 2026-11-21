package delivery

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// One pass keeps every stored row of a pull request on the answer of the statement: the
// release before reads pull_request_heads.issue_key and delivery_pull_requests.issue_key, and
// both hold what this build's readers answer after the pass (SyncAttributions). A results row
// that takes another issue queues a move (pull_request_attribution_moves), and the pass appends
// the events of at most AttributionMovesPerPass moves (AppendAttributionEvents).

// AttributionEventType is the event that records a change of a pull request's issue: one on the
// issue the pull request left and one on the issue it reached, each carrying the same payload, so
// either issue's timeline shows the whole move. A pull request that reaches an issue from nobody
// has one event, on the issue it reached, and one that is uncredited has one on the issue it left.
const AttributionEventType = "issue.pull_request_attribution_changed"

// AttributionEventActor writes every issue.pull_request_attribution_changed: a system actor,
// since no person or session asked for the move — a link someone added, a branch GitHub
// reported, or this build's own ranking of the sources decided it. Broker.Notify is false for a
// system actor, which keeps the event out of inboxes and routed delivery; the outbox publishes
// it on the issue's subject as it does every unpublished event, and the event feed and the
// event log carry it.
var AttributionEventActor = model.Actor{Kind: "system", ID: "delivery-attribution"}

// IssuePullRequestAttributionEventPayload is an issue.pull_request_attribution_changed event's
// payload: the pull request (its repository, number and URL), the issue it left and the issue it
// reached — either is null, never both — and the attribution source that decided the new issue
// (attributionSource, "none" where the pull request is now credited to nobody).
type IssuePullRequestAttributionEventPayload struct {
	Repo   string  `json:"repo"`
	Number int     `json:"number"`
	URL    string  `json:"url"`
	From   *string `json:"from"`
	To     *string `json:"to"`
	Source string  `json:"source"`
}

// AttributionMovesPerPass is the most queued moves one pass appends the events of. One drain of
// 500 rows took 8 ms to 9 ms. So n changes need ceil(n / 500) passes for their events.
const AttributionMovesPerPass = 500

// AttributionSync is what one SyncAttributions wrote: the moves it queued, one for each pull
// request whose results row took another issue, and the timeline rows it wrote.
type AttributionSync struct{ Changes, TimelineRows int }

// syncJudged is the judged rows of SyncAttributions: every stored pull request, under each name
// it is stored under.
const syncJudged = `select repo, number from pull_request_heads union select repo, number from delivery_pull_requests`

// syncAttributionsStatement is SyncAttributions' one statement. It writes the answer to each
// results row of the pull request, under each of its names, where it differs; the source too
// where the pull request has facts. It writes the answer to each timeline row of a pull request
// that has a results row. And it queues one move for each pull request whose chosen results row
// (the row that heads takes) held another issue, keyed by the canonical name: a second move before
// the events keeps the first from_key. A pull request whose answer is not known writes nothing and
// queues nothing. All terms read one snapshot, so from_key is the stored issue before the write.
func syncAttributionsStatement(schema string) string {
	return attributedTerms(schema, syncJudged) + `, heads_written as (
		update pull_request_heads h set issue_key = a.issue_key,
			attribution_source = case when a.has_facts then a.source else h.attribution_source end
		from attributed a
		where a.known and h.number = a.number and lower(` + refs.CanonicalRepoSQL("h.repo") + `) = lower(a.repo)
			and (h.issue_key is distinct from a.issue_key or (a.has_facts and h.attribution_source is distinct from a.source))
		returning 1
	), timeline_written as (
		update delivery_pull_requests d set issue_key = a.issue_key
		from attributed a
		where a.known and a.stored_repo is not null
			and d.number = a.number and lower(` + refs.CanonicalRepoSQL("d.repo") + `) = lower(a.repo)
			and d.issue_key is distinct from a.issue_key
		returning 1
	), queued as (
		insert into pull_request_attribution_moves (repo, number, from_key, to_key, source)
		select a.repo, a.number, a.fallback, a.issue_key, a.source from attributed a
		where a.known and a.stored_repo is not null and a.fallback is distinct from a.issue_key
		on conflict (repo, number) do update set to_key = excluded.to_key, source = excluded.source, queued_at = excluded.queued_at
		returning 1
	)
	select (select count(*) from queued), (select count(*) from timeline_written), (select count(*) from heads_written)`
}

// SyncAttributions writes the answer of the statement over every stored pull request to its
// stored rows, in one statement with no cap, and queues a move for each results row that takes
// another issue (syncAttributionsStatement). It reads no GitHub. The writes of a results row's
// issue and source owe no read and keep the row's mark: migration 20261117111040 narrowed the
// trigger pull_request_heads_unread_write to the facts of the head.
func SyncAttributions(ctx context.Context, pool *store.Pool) (AttributionSync, error) {
	statements, err := attribution()
	if err != nil {
		return AttributionSync{}, err
	}
	var synced AttributionSync
	var heads int
	if err := pool.QueryRow(ctx, statements.syncAttributions).Scan(&synced.Changes, &synced.TimelineRows, &heads); err != nil {
		return AttributionSync{}, fmt.Errorf("write the issue of every stored pull request: %w", err)
	}
	return synced, nil
}

// attributionMove is one queued move whose two keys differ, as AppendAttributionEvents takes it:
// fromLive and toLive say whether each key is still an issue, which an event needs.
type attributionMove struct {
	payload          IssuePullRequestAttributionEventPayload
	fromLive, toLive bool
}

// AppendAttributionEvents takes at most AttributionMovesPerPass queued moves, the oldest first,
// and deletes them, in one transaction. For each move whose two keys differ, it appends one
// issue.pull_request_attribution_changed on the issue left and one on the issue reached, each
// where it is set and still an issue. It locks every issue in one call before the first append
// (events.Broker.LockOwners), which takes them in one order. It publishes the events after the
// commit and answers how many it appended. A move whose keys are equal again, a move and the
// move back before the events, appends nothing. Two processes take disjoint moves (skip locked).
// A caller with no broker appends nothing, and the moves wait for a pass that has one.
func AppendAttributionEvents(ctx context.Context, pool *store.Pool, broker *events.Broker) (int, error) {
	if broker == nil {
		return 0, nil
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("open the append of attribution events: %w", err)
	}
	defer func() {
		rollback, cancel := context.WithTimeout(context.WithoutCancel(ctx), headWriteRollback)
		defer cancel()
		_ = tx.Rollback(rollback)
	}()
	// The take locks and deletes rows of the moves table alone, so it names no owner table that
	// a `for update` would lock (api's TestNoForUpdateOnOwnerTables); the issues and the URLs are
	// read by the next statement.
	rows, err := tx.Query(ctx, `
		with taken as (
			select repo, number from pull_request_attribution_moves
			order by queued_at, repo, number
			limit $1
			for update skip locked
		)
		delete from pull_request_attribution_moves m using taken t
		where m.repo = t.repo and m.number = t.number
		returning m.repo, m.number, m.from_key, m.to_key, m.source, m.queued_at
	`, AttributionMovesPerPass)
	if err != nil {
		return 0, fmt.Errorf("take the queued attribution moves: %w", err)
	}
	type queuedMove struct {
		payload  IssuePullRequestAttributionEventPayload
		queuedAt time.Time
	}
	var taken []queuedMove
	for rows.Next() {
		var move queuedMove
		p := &move.payload
		if err := rows.Scan(&p.Repo, &p.Number, &p.From, &p.To, &p.Source, &move.queuedAt); err != nil {
			rows.Close()
			return 0, fmt.Errorf("take the queued attribution moves: %w", err)
		}
		if !equalIssue(p.From, p.To) {
			taken = append(taken, move)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("take the queued attribution moves: %w", err)
	}
	slices.SortFunc(taken, func(a, b queuedMove) int {
		return cmp.Or(a.queuedAt.Compare(b.queuedAt), strings.Compare(a.payload.Repo, b.payload.Repo), cmp.Compare(a.payload.Number, b.payload.Number))
	})
	repos, numbers := make([]string, len(taken)), make([]int, len(taken))
	froms, tos := make([]*string, len(taken)), make([]*string, len(taken))
	for i, move := range taken {
		repos[i], numbers[i], froms[i], tos[i] = move.payload.Repo, move.payload.Number, move.payload.From, move.payload.To
	}
	rows, err = tx.Query(ctx, `
		select coalesce(u.url, ''), exists (select 1 from issues i where i.key = k.from_key), exists (select 1 from issues i where i.key = k.to_key)
		from unnest($1::text[], $2::int[], $3::text[], $4::text[]) with ordinality k(repo, number, from_key, to_key, n)
		left join lateral (
			select h.url from pull_request_heads h
			where h.number = k.number and lower(`+refs.CanonicalRepoSQL("h.repo")+`) = lower(k.repo)
			order by lower(h.repo) = lower(k.repo) desc, h.repo
			limit 1) u on true
		order by k.n
	`, repos, numbers, froms, tos)
	if err != nil {
		return 0, fmt.Errorf("read the pull requests and issues of %d attribution moves: %w", len(taken), err)
	}
	moves := make([]attributionMove, 0, len(taken))
	for rows.Next() {
		move := attributionMove{payload: taken[len(moves)].payload}
		if err := rows.Scan(&move.payload.URL, &move.fromLive, &move.toLive); err != nil {
			rows.Close()
			return 0, fmt.Errorf("read the pull requests and issues of %d attribution moves: %w", len(taken), err)
		}
		moves = append(moves, move)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("read the pull requests and issues of %d attribution moves: %w", len(taken), err)
	}
	var moved []model.Event
	for _, move := range moves {
		for _, side := range []struct {
			key  *string
			live bool
		}{{move.payload.From, move.fromLive}, {move.payload.To, move.toLive}} {
			if side.key == nil || !side.live {
				continue
			}
			key := *side.key
			moved = append(moved, model.Event{IssueKey: &key, Type: AttributionEventType, Actor: AttributionEventActor, Payload: move.payload})
		}
	}
	appended := make([]model.Event, 0, len(moved))
	if len(moved) > 0 {
		if err := broker.LockOwners(ctx, tx, moved...); err != nil {
			return 0, fmt.Errorf("append the events of %d attribution moves: %w", len(moves), err)
		}
	}
	for _, event := range moved {
		written, err := broker.Append(ctx, tx, event)
		if err != nil {
			return 0, fmt.Errorf("append the events of %d attribution moves: %w", len(moves), err)
		}
		appended = append(appended, written)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit the events of %d attribution moves: %w", len(moves), err)
	}
	for _, event := range appended {
		broker.Publish(event)
	}
	return len(appended), nil
}
