package store

import (
	"context"
	"testing"
	"time"
)

// pullRequestHeadsOlderReadVersion is the migration whose trigger records a pull request as owed
// a read when a write of GitHub's answer replaces a newer row no read of GitHub wrote, or a row of
// the same GitHub second with other facts.
const pullRequestHeadsOlderReadVersion = 20261116182209

// TestMigrationOwesAReadWhereAnOlderAnswerReplacesANewerUnmarkedRow: after the migration, an
// update that writes a mark (github_read_at) over an unmarked row whose event_at is later than
// the update's own records the pull request as owed a read of kind failure. So does an update of
// the same event_at that changes a fact GitHub answers (the head, the state, the title) of a row
// GitHub's answer is in before or after it: a marked answer over a marked or an unmarked row. And
// so does a write of the release before (which keeps the mark) over a marked row, at any time:
// pull_request_heads_unread_write, as this migration replaces it, owes the read in the statement
// that takes the mark away (acceptance round 4 of cybermonklabs/legion#4209, D8). An update writing
// a mark over an older unmarked row, an update over a row that is marked already with an older
// time, an update of the same event_at that changes no fact, one that changes only the
// attribution (Dispatch's own judgement of GitHub's facts, which a later rule changes at the same
// second), and a write of the release before over its own row of the same second, record nothing.
// Each owe gives the record a new revision.
func TestMigrationOwesAReadWhereAnOlderAnswerReplacesANewerUnmarkedRow(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	if err := store.migrate(ctx, migrationsThrough(t, pullRequestHeadsOlderReadVersion)); err != nil {
		t.Fatalf("migrate to %d: %v", pullRequestHeadsOlderReadVersion, err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	owed := func(number int) (string, int64) {
		t.Helper()
		var reason string
		var revision int64
		err := store.Pool.QueryRow(ctx, `select reason, revision from pull_request_reads_owed where repo = 'acme/widgets' and number = $1`, number).Scan(&reason, &revision)
		if err != nil {
			return "", 0
		}
		return reason, revision
	}
	insert := func(number int, at time.Time, marked bool) {
		t.Helper()
		if _, err := store.Pool.Exec(ctx, `
			insert into pull_request_heads (repo, number, head_sha, state, event_at, github_read_at)
			values ('acme/widgets', $1, 'aaaa', 'open', $2, case when $3::boolean then clock_timestamp() end)
		`, number, at, marked); err != nil {
			t.Fatalf("store #%d: %v", number, err)
		}
	}
	update := func(number int, head, state string, at time.Time, marked bool) {
		t.Helper()
		if _, err := store.Pool.Exec(ctx, `
			update pull_request_heads set head_sha = $2, state = $3, event_at = $4,
				github_read_at = case when $5::boolean then clock_timestamp() else github_read_at end
			where repo = 'acme/widgets' and number = $1
		`, number, head, state, at, marked); err != nil {
			t.Fatalf("update #%d: %v", number, err)
		}
	}
	// restate writes GitHub's answer again at the row's own event_at with column set to value.
	restate := func(number int, column, value string) {
		t.Helper()
		if _, err := store.Pool.Exec(ctx, `
			update pull_request_heads set `+column+` = $2, github_read_at = clock_timestamp()
			where repo = 'acme/widgets' and number = $1
		`, number, value); err != nil {
			t.Fatalf("restate #%d: %v", number, err)
		}
	}

	insert(1, now, false)
	update(1, "bbbb", "closed", now.Add(-5*time.Minute), true)
	insert(5, now, true)
	update(5, "bbbb", "closed", now, true)
	insert(6, now, false)
	update(6, "bbbb", "closed", now, true)
	insert(7, now, true)
	update(7, "bbbb", "closed", now, false)
	insert(3, now, true)
	update(3, "bbbb", "closed", now.Add(-5*time.Minute), false)
	insert(11, now, true)
	restate(11, "title", "A title GitHub changed in the same second")
	for number, why := range map[int]string{
		1:  "an older marked answer over a newer unmarked row",
		5:  "a marked answer over a marked row of the same second with other facts",
		6:  "a marked answer over an unmarked row of the same second with other facts",
		7:  "a write of the release before over a marked row of the same second with other facts",
		3:  "a write of the release before over a marked row, at an older time (D8)",
		11: "a marked answer of the same second whose title alone changed",
	} {
		if got, _ := owed(number); got != "failure" {
			t.Errorf("#%d, %s: owed %q, want failure", number, why, got)
		}
	}

	insert(2, now, false)
	update(2, "bbbb", "closed", now.Add(5*time.Minute), true)
	insert(4, now, true)
	update(4, "bbbb", "closed", now.Add(-5*time.Minute), true)
	insert(8, now, true)
	update(8, "aaaa", "open", now, true)
	insert(9, now, false)
	update(9, "bbbb", "closed", now, false)
	insert(10, now, true)
	restate(10, "attribution_source", "branch")
	for _, number := range []int{2, 4, 8, 9, 10} {
		if got, _ := owed(number); got != "" {
			t.Errorf("#%d owed %q, want nothing", number, got)
		}
	}

	_, first := owed(5)
	update(5, "cccc", "open", now, true)
	if got, second := owed(5); got != "failure" || second == first {
		t.Errorf("#5 owed again: reason %q, revision %d then %d, want failure at a new revision", got, first, second)
	}
}

// TestAWriteOfTheIssueAloneOwesNoReadAndKeepsTheMark: from pullRequestTextsVersion on, a row's
// issue is an answer of Dispatch's statement over its stored facts, and the pass writes it to
// the row with no read of GitHub. So an update of a read row that changes issue_key and
// attribution_source alone owes no read and keeps github_read_at and checked_at, while an
// update that changes a fact (the state) still takes the mark away and owes a read. Control in
// the same test: at pullRequestHeadsOlderReadVersion the same write of the issue owes a read and
// takes the mark away, so the test reads the change of the trigger.
func TestAWriteOfTheIssueAloneOwesNoReadAndKeepsTheMark(t *testing.T) {
	ctx := context.Background()
	type marks struct {
		owed              string
		readAt, checkedAt *time.Time
	}
	writeTheIssue := func(t *testing.T, version int, set string) (before, after marks) {
		t.Helper()
		store := openEmptyTestStore(t)
		if err := store.migrate(ctx, migrationsThrough(t, version)); err != nil {
			t.Fatalf("migrate to %d: %v", version, err)
		}
		if _, err := store.Pool.Exec(ctx, `
			insert into projects (key, name, default_assignee) values ('ACME', 'Acme', 'alice@example.com');
			insert into issues (key, project_key, number, title, status, created_by, rank) values
				('ACME-1', 'ACME', 1, 'ACME-1', 'todo', '{"kind":"system","id":"test"}'::jsonb, 'ACME-1'),
				('ACME-2', 'ACME', 2, 'ACME-2', 'todo', '{"kind":"system","id":"test"}'::jsonb, 'ACME-2');
			insert into pull_request_heads (repo, number, head_sha, state, issue_key, attribution_source, event_at, checked_at, github_read_at)
			values ('acme/widgets', 1, 'aaaa', 'merged', 'ACME-1', 'branch', clock_timestamp(), clock_timestamp(), clock_timestamp());
		`); err != nil {
			t.Fatalf("store a read row of ACME-1: %v", err)
		}
		read := func() marks {
			t.Helper()
			var m marks
			if err := store.Pool.QueryRow(ctx, `
				select coalesce((select reason from pull_request_reads_owed where repo = 'acme/widgets' and number = 1), ''),
					github_read_at, checked_at
				from pull_request_heads where repo = 'acme/widgets' and number = 1
			`).Scan(&m.owed, &m.readAt, &m.checkedAt); err != nil {
				t.Fatalf("read #1's marks: %v", err)
			}
			return m
		}
		before = read()
		if _, err := store.Pool.Exec(ctx, `update pull_request_heads set `+set+` where repo = 'acme/widgets' and number = 1`); err != nil {
			t.Fatalf("update #1 (%s): %v", set, err)
		}
		return before, read()
	}
	const theIssue = `issue_key = 'ACME-2', attribution_source = 'external_link'`

	before, after := writeTheIssue(t, pullRequestTextsVersion, theIssue)
	if after.owed != "" || after.readAt == nil || !after.readAt.Equal(*before.readAt) || after.checkedAt == nil || !after.checkedAt.Equal(*before.checkedAt) {
		t.Errorf("a write of the issue alone: owed %q, github_read_at %v then %v, checked_at %v then %v; want nothing owed and both kept",
			after.owed, before.readAt, after.readAt, before.checkedAt, after.checkedAt)
	}
	if _, after := writeTheIssue(t, pullRequestTextsVersion, `state = 'closed'`); after.owed != "failure" || after.readAt != nil {
		t.Errorf("a write of the state alone: owed %q, github_read_at %v; want failure and the mark taken away", after.owed, after.readAt)
	}
	if _, after := writeTheIssue(t, pullRequestHeadsOlderReadVersion, theIssue); after.owed != "failure" || after.readAt != nil {
		t.Errorf("at %d, a write of the issue alone: owed %q, github_read_at %v; want failure and the mark taken away (the control)",
			pullRequestHeadsOlderReadVersion, after.owed, after.readAt)
	}
}
