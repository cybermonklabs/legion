package record

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

const toldColumns = `issue, audience, versions, opening_owed, told_at`

// scanTold reads a design_news_told row in the shape the writers below keep it.
func scanTold(row scanner) (DesignTold, error) {
	var told DesignTold
	var audience string
	var versions []byte
	var at *time.Time
	if err := row.Scan(&told.Issue, &audience, &versions, &told.OpeningOwed, &at); err != nil {
		return DesignTold{}, err
	}
	told.Audience = DesignAudience(audience)
	if err := json.Unmarshal(versions, &told.Versions); err != nil {
		return DesignTold{}, fmt.Errorf("decode the versions %s's %s took: %w", told.Issue, audience, err)
	}
	if at != nil {
		told.ToldAt = at.UTC()
	}
	return told, nil
}

// TreeTold is every told row of the issues of tree, by issue and then audience: what OwedNews
// weighs the tree's gate against, in one read.
func (s *Postgres) TreeTold(ctx context.Context, tx pgx.Tx, tree string) ([]DesignTold, error) {
	rows, err := tx.Query(ctx, `select `+toldColumns+` from design_news_told
		where issue in (select key from issues where tree = $1) order by issue, audience`, tree)
	if err != nil {
		return nil, fmt.Errorf("read what the holders of tree %s took: %w", tree, err)
	}
	told, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (DesignTold, error) { return scanTold(row) })
	if err != nil {
		return nil, fmt.Errorf("read what the holders of tree %s took: %w", tree, err)
	}
	return told, nil
}

// BaselineTold writes issue's audience row at the approved version of each step whose gate row
// root holds an approval, when issue holds no such row yet, and reports whether it wrote one. A
// row already there is left as it stands: the baseline is the version the work started from, and
// a later baseline would hide the news of every version approved since. A step whose row holds no
// approval is left out, which is a version the record does not know (DesignTold.Took).
//
// The versions are read in the statement that writes them, from the rows the caller's transaction
// holds, so the baseline is the gate as that transaction leaves it: the transition that starts the
// work (workflow's Engine.enter), or the write that opens the tree's gate for the first time
// (workflow's putGateTold).
func (s *Postgres) BaselineTold(ctx context.Context, tx pgx.Tx, issue string, audience DesignAudience, root string, at time.Time) (bool, error) {
	tag, err := tx.Exec(ctx, `insert into design_news_told (issue, audience, versions, opening_owed, told_at)
		select $1::text, $2::text,
			coalesce(jsonb_object_agg(step, approved_version) filter (where approved_version is not null), '{}'::jsonb),
			false, $4::timestamptz
		from design_gates where issue = $3::text
		on conflict (issue, audience) do nothing`, issue, string(audience), root, at)
	if err != nil {
		return false, fmt.Errorf("write the baseline %s's %s takes from %s's gate: %w", issue, audience, root, err)
	}
	return tag.RowsAffected() == 1, nil
}

// RaiseTold raises each step of versions on issue's audience row to the greater of the version the
// row holds and the one given, writing the row when there is none. A step the row does not know
// takes the version given, and a step versions does not name keeps what the row holds, so a raise
// never lowers a version and never forgets one. The owed opening is left as it stands.
func (s *Postgres) RaiseTold(ctx context.Context, tx pgx.Tx, issue string, audience DesignAudience, versions map[DesignStep]int, at time.Time) error {
	if versions == nil {
		versions = map[DesignStep]int{}
	}
	encoded, err := json.Marshal(versions)
	if err != nil {
		return fmt.Errorf("encode the versions %s's %s took: %w", issue, audience, err)
	}
	if _, err := tx.Exec(ctx, `insert into design_news_told (issue, audience, versions, opening_owed, told_at)
		values ($1, $2, $3, false, $4)
		on conflict (issue, audience) do update set
			versions = design_news_told.versions || (
				select coalesce(jsonb_object_agg(raised.key,
					greatest(raised.value::int, coalesce((design_news_told.versions->>raised.key)::int, raised.value::int))), '{}'::jsonb)
				from jsonb_each_text(excluded.versions) raised),
			told_at = excluded.told_at`, issue, string(audience), encoded, at); err != nil {
		return fmt.Errorf("raise the versions %s's %s took: %w", issue, audience, err)
	}
	return nil
}

// SetOpeningOwed sets or clears whether the gate's opening is owed to issue's audience. Setting it
// on an issue with no row writes one that knows no version; clearing it where no row stands
// writes nothing.
func (s *Postgres) SetOpeningOwed(ctx context.Context, tx pgx.Tx, issue string, audience DesignAudience, owed bool) error {
	var err error
	if owed {
		_, err = tx.Exec(ctx, `insert into design_news_told (issue, audience, opening_owed) values ($1, $2, true)
			on conflict (issue, audience) do update set opening_owed = true`, issue, string(audience))
	} else {
		_, err = tx.Exec(ctx, `update design_news_told set opening_owed = false where issue = $1 and audience = $2`, issue, string(audience))
	}
	if err != nil {
		return fmt.Errorf("record whether the opening is owed to %s's %s: %w", issue, audience, err)
	}
	return nil
}

// FillTold writes each step of versions on issue's audience row where the row knows no version of
// that step, writing the row when there is none, and leaves every step the row knows as it stands:
// a known version is never lowered or overwritten, and a step versions does not name keeps what
// the row holds. It is how a version the record did not know is resolved into it — the reconcile
// reads it from Dispatch's review history (dispatch.Client.ApprovedVersionBefore) — so that every
// later reader of the row names the same pair of versions, where a version read again at each send
// could name another. A row already there keeps its owed opening and the moment it last rose.
func (s *Postgres) FillTold(ctx context.Context, tx pgx.Tx, issue string, audience DesignAudience, versions map[DesignStep]int, at time.Time) error {
	if len(versions) == 0 {
		return nil
	}
	encoded, err := json.Marshal(versions)
	if err != nil {
		return fmt.Errorf("encode the versions %s's %s is taken to have worked from: %w", issue, audience, err)
	}
	if _, err := tx.Exec(ctx, `insert into design_news_told (issue, audience, versions, opening_owed, told_at)
		values ($1, $2, $3, false, $4)
		on conflict (issue, audience) do update set versions = excluded.versions || design_news_told.versions`,
		issue, string(audience), encoded, at); err != nil {
		return fmt.Errorf("write the versions %s's %s is taken to have worked from: %w", issue, audience, err)
	}
	return nil
}

// TreeNewsFacts is what OwedNews reads of tree, as tx finds the record: the deployment's steps, the
// root's gate rows, every member with the root among them, their told rows, and whether each
// member's pull request has merged, read from the pull request's own state whatever phase the
// member stands in. Every reader of a tree's owed news reads its facts here, so the opening, a
// phase move, the reconcile, a start, the merger's hold and the sweep weigh one rule over one read.
func TreeNewsFacts(ctx context.Context, store Store, tx pgx.Tx, steps []DesignStep, tree string) (NewsFacts, error) {
	gates, err := store.GatesOf(ctx, tx, tree)
	if err != nil {
		return NewsFacts{}, err
	}
	members, err := store.TreeIssues(ctx, tx, tree)
	if err != nil {
		return NewsFacts{}, err
	}
	told, err := store.TreeTold(ctx, tx, tree)
	if err != nil {
		return NewsFacts{}, err
	}
	merged := make(map[string]bool, len(members))
	for _, member := range members {
		pr, err := store.PullRequest(ctx, tx, member.Key)
		if err != nil {
			return NewsFacts{}, err
		}
		merged[member.Key] = pr != nil && pr.State == PullRequestMerged
	}
	return NewsFacts{Steps: steps, Gates: gates, Members: members, Told: told, Merged: merged}, nil
}

// DesignNewsPending says whether the outbox holds a reconcile of tree's news (DesignNewsReconcile)
// that has not begun to run at now: one due, backing off or retrying, under no live lease. A row a
// lane runs now is not counted, since it may have read the record before the caller's write.
func (s *Postgres) DesignNewsPending(ctx context.Context, tx pgx.Tx, tree string, now time.Time) (bool, error) {
	var pending bool
	if err := tx.QueryRow(ctx, `select exists (select 1 from outbox where kind = $1 and issue = $2
		and (lease_until is null or lease_until <= $3))`, string(OutboxKindDesignChanges), tree, now).Scan(&pending); err != nil {
		return false, fmt.Errorf("read whether the news of tree %s is queued for its reconcile: %w", tree, err)
	}
	return pending, nil
}

// QueueDesignNews queues the reconcile of tree's news of its approved design, due at now, unless one
// that has not begun to run is queued already (Store.DesignNewsPending): the reconcile reads the
// record when it runs, so the row already queued does all a second would. Its writers — the write
// of a gate row that raises an approved version or opens the standing gate (workflow's
// putGateTold), a phase move that leaves its member owed news (workflow's transition), a send that
// drops an item it carried (the daemon's designNews) and the sweep — each take the lock every
// fact applies under, so no two of them can each find none and both queue one.
func QueueDesignNews(ctx context.Context, store Store, tx pgx.Tx, tree string, now time.Time) error {
	pending, err := store.DesignNewsPending(ctx, tx, tree, now)
	if err != nil || pending {
		return err
	}
	row, err := NewOutboxRow(tree, DesignNewsReconcile{}, now)
	if err != nil {
		return err
	}
	return store.Enqueue(ctx, tx, row)
}

const changesColumns = `artifact_id, from_version, to_version, state, decision_only, sections, blocks, reason, read_at`

// DesignChangesOf is the recorded change set of each of pairs that has one, by pair, in one read;
// a pair with none recorded has no entry, and no pairs read nothing.
func (s *Postgres) DesignChangesOf(ctx context.Context, tx pgx.Tx, pairs []VersionPair) (map[VersionPair]DesignChanges, error) {
	recorded := make(map[VersionPair]DesignChanges, len(pairs))
	if len(pairs) == 0 {
		return recorded, nil
	}
	artifacts, froms, tos := make([]string, len(pairs)), make([]int, len(pairs)), make([]int, len(pairs))
	for i, pair := range pairs {
		artifacts[i], froms[i], tos[i] = pair.ArtifactID, pair.From, pair.To
	}
	rows, err := tx.Query(ctx, `select `+changesColumns+` from design_changes
		join unnest($1::text[], $2::int[], $3::int[]) as asked (artifact_id, from_version, to_version)
			using (artifact_id, from_version, to_version)`, artifacts, froms, tos)
	if err != nil {
		return nil, fmt.Errorf("read the change sets of %d pairs: %w", len(pairs), err)
	}
	defer rows.Close()
	for rows.Next() {
		var changes DesignChanges
		var state string
		var sections, blocks []byte
		if err := rows.Scan(&changes.ArtifactID, &changes.From, &changes.To, &state, &changes.DecisionOnly, &sections, &blocks,
			&changes.Reason, &changes.ReadAt); err != nil {
			return nil, fmt.Errorf("read the change sets of %d pairs: %w", len(pairs), err)
		}
		changes.State, changes.ReadAt = DesignChangesState(state), changes.ReadAt.UTC()
		if err := json.Unmarshal(sections, &changes.Sections); err != nil {
			return nil, fmt.Errorf("decode the sections of %+v: %w", changes.VersionPair, err)
		}
		if err := json.Unmarshal(blocks, &changes.Blocks); err != nil {
			return nil, fmt.Errorf("decode the blocks of %+v: %w", changes.VersionPair, err)
		}
		recorded[changes.VersionPair] = changes
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the change sets of %d pairs: %w", len(pairs), err)
	}
	return recorded, nil
}

// PutDesignChanges records the change set of one pair. A pair already recorded read keeps its
// row, since two stored versions never change and Dispatch's answer for them is final; a pair
// recorded unavailable takes the new row, so a Dispatch that has gained the route since can still
// say what changed. It refuses a set the table's shape does not hold: a state that is neither,
// a pair whose From is not below its To, and a reason given or missing against the state.
func (s *Postgres) PutDesignChanges(ctx context.Context, tx pgx.Tx, changes DesignChanges) error {
	switch {
	case changes.State != DesignChangesRead && changes.State != DesignChangesUnavailable:
		return fmt.Errorf("record the change set of %+v: state %q is neither read nor unavailable", changes.VersionPair, changes.State)
	case changes.From < 0 || changes.To <= changes.From:
		return fmt.Errorf("record the change set of %+v: from must be at least 0 and below to", changes.VersionPair)
	case (changes.State == DesignChangesUnavailable) != (changes.Reason != ""):
		return fmt.Errorf("record the change set of %+v: an unavailable pair says why, and a read one has no reason to give", changes.VersionPair)
	}
	sections, err := json.Marshal(append([]SectionChange{}, changes.Sections...))
	if err != nil {
		return fmt.Errorf("encode the sections of %+v: %w", changes.VersionPair, err)
	}
	blocks, err := json.Marshal(append([]BlockChange{}, changes.Blocks...))
	if err != nil {
		return fmt.Errorf("encode the blocks of %+v: %w", changes.VersionPair, err)
	}
	if _, err := tx.Exec(ctx, `insert into design_changes (`+changesColumns+`)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		on conflict (artifact_id, from_version, to_version) do update set state = excluded.state,
			decision_only = excluded.decision_only, sections = excluded.sections, blocks = excluded.blocks,
			reason = excluded.reason, read_at = excluded.read_at
		where design_changes.state <> 'read'`,
		changes.ArtifactID, changes.From, changes.To, string(changes.State), changes.DecisionOnly, sections, blocks,
		changes.Reason, changes.ReadAt); err != nil {
		return fmt.Errorf("record the change set of %+v: %w", changes.VersionPair, err)
	}
	return nil
}
