package store

import (
	"context"
	"testing"
	"time"
)

// pullRequestHeadsGitHubReadVersion is the migration that marks the pull_request_heads rows a read
// of GitHub wrote (github_read_at) and adds the trigger that takes the mark away from a row a
// write of no read of GitHub changes.
const pullRequestHeadsGitHubReadVersion = 20261116163242

// TestMigrationMarksNoStoredHeadAndAWriteOfTheReleaseBeforeTakesTheMarkAway: a row stored before
// the migration holds no mark, so its time, which an envelope a caller set may have written,
// stops no read of GitHub. After it, the release before still writes rows with a statement that
// names no github_read_at: where such a write changes a fact of a row a read of GitHub marked,
// the row loses its mark, is unread (checked_at null) and judged by no rule (attribution_rule 0),
// so the reconcile reads it again. A write of that release that changes no fact, and a stamp of
// checked_at alone, keep the mark, and so does a write that names a new mark, as this release's
// writes all do.
func TestMigrationMarksNoStoredHeadAndAWriteOfTheReleaseBeforeTakesTheMarkAway(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	if err := store.migrate(ctx, migrationsThrough(t, pullRequestHeadsGitHubReadVersion-1)); err != nil {
		t.Fatalf("migrate through %d: %v", pullRequestHeadsGitHubReadVersion-1, err)
	}
	ahead := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	if _, err := store.Pool.Exec(ctx, `
		insert into pull_request_heads (repo, number, head_sha, state, event_at, checked_at, attribution_rule)
		values ('acme/widgets', 1, 'aaaa', 'open', $1, now(), 2)
	`, ahead); err != nil {
		t.Fatalf("store a head row before the migration: %v", err)
	}
	if err := store.migrate(ctx, migrationsThrough(t, pullRequestHeadsGitHubReadVersion)); err != nil {
		t.Fatalf("migrate to %d: %v", pullRequestHeadsGitHubReadVersion, err)
	}

	type row struct {
		marked, checked bool
		rule            int
		head            string
	}
	read := func() row {
		t.Helper()
		var got row
		if err := store.Pool.QueryRow(ctx, `
			select github_read_at is not null, checked_at is not null, attribution_rule, head_sha
			from pull_request_heads where repo = 'acme/widgets' and number = 1
		`).Scan(&got.marked, &got.checked, &got.rule, &got.head); err != nil {
			t.Fatalf("read the head row: %v", err)
		}
		return got
	}
	if got := read(); got.marked {
		t.Fatalf("the row stored before the migration = %+v, want it unmarked: its time may be an envelope's", got)
	}

	// This release's write of GitHub's answer marks the row.
	mark := func(head string) {
		t.Helper()
		if _, err := store.Pool.Exec(ctx, `
			update pull_request_heads set head_sha = $1, event_at = now(), checked_at = now(),
				attribution_rule = 3, github_read_at = clock_timestamp()
			where repo = 'acme/widgets' and number = 1
		`, head); err != nil {
			t.Fatalf("write GitHub's answer as this release does: %v", err)
		}
		if got := read(); !got.marked || !got.checked || got.rule != 3 || got.head != head {
			t.Fatalf("the row after this release's write = %+v, want marked, checked and judged by rule 3 at %s", got, head)
		}
	}
	mark("bbbb")

	// The release before's upsert names no github_read_at, as its statement never did.
	releaseBefore := func(head string, at time.Time) {
		t.Helper()
		if _, err := store.Pool.Exec(ctx, `
			insert into pull_request_heads (repo, number, head_sha, state, event_at, checked_at, attribution_rule)
			values ('acme/widgets', 1, $1, 'merged', $2, now(), 2)
			on conflict (repo, number) do update set
				head_sha = excluded.head_sha, state = excluded.state,
				event_at = coalesce(excluded.event_at, pull_request_heads.event_at),
				checked_at = coalesce(excluded.checked_at, pull_request_heads.checked_at),
				attribution_rule = excluded.attribution_rule
		`, head, at); err != nil {
			t.Fatalf("write the head as the release before does: %v", err)
		}
	}
	releaseBefore("cccc", ahead)
	if got := read(); got.marked || got.checked || got.rule != 0 || got.head != "cccc" {
		t.Fatalf("the row after the release before's write of other facts = %+v, want unmarked, unchecked and judged by no rule", got)
	}

	mark("dddd")
	if _, err := store.Pool.Exec(ctx, `
		update pull_request_heads set checked_at = now(), attribution_rule = 3 where repo = 'acme/widgets' and number = 1
	`); err != nil {
		t.Fatalf("stamp the row checked: %v", err)
	}
	if got := read(); !got.marked {
		t.Fatalf("the row after a stamp of checked_at alone = %+v, want the mark kept: no fact changed", got)
	}
	var facts struct {
		state string
		at    time.Time
	}
	if err := store.Pool.QueryRow(ctx, `select state, event_at from pull_request_heads where repo = 'acme/widgets' and number = 1`).Scan(&facts.state, &facts.at); err != nil {
		t.Fatalf("read the row's facts: %v", err)
	}
	if _, err := store.Pool.Exec(ctx, `
		insert into pull_request_heads (repo, number, head_sha, state, event_at, checked_at, attribution_rule)
		values ('acme/widgets', 1, 'dddd', $1, $2, now(), 2)
		on conflict (repo, number) do update set
			head_sha = excluded.head_sha, state = excluded.state, event_at = excluded.event_at,
			checked_at = excluded.checked_at, attribution_rule = excluded.attribution_rule
	`, facts.state, facts.at); err != nil {
		t.Fatalf("write the same facts as the release before does: %v", err)
	}
	if got := read(); !got.marked || got.rule != 2 {
		t.Fatalf("the row after the release before wrote the facts it holds = %+v, want the mark kept: no fact changed", got)
	}
}
