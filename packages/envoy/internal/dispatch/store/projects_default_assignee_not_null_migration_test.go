package store

import (
	"context"
	"testing"
)

// projectsDefaultAssigneeNotNullVersion fills every project's default assignee from the login
// holding most of its issues, else from the person who created it, and then makes
// projects.default_assignee not null, so the last step of the chain that resolves who a question
// reaches always lands on a person (INFRA-3090).
const projectsDefaultAssigneeNotNullVersion = 20261111210103

// The migration fills a project of each kind the deployment holds: one with a majority assignee,
// one whose tie the most recent issue breaks, one whose only assignee no `people` row holds, one
// with no issue at all, one an agent session created, and one created with a person's own token.
// After it, the column refuses a project with no default.
func TestTheDefaultAssigneeBackfillFillsEveryProject(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	if err := store.migrate(ctx, migrationsThrough(t, projectsDefaultAssigneeNotNullVersion-1)); err != nil {
		t.Fatalf("migrate through %d: %v", projectsDefaultAssigneeNotNullVersion-1, err)
	}
	seedProjectsOfEveryKind(t, store)

	// The negative control of the fill's `people` test, inside this one run: before the
	// migration no project names a default at all, so a default read back afterwards is the
	// migration's doing and not a value the seed left.
	var defaults int
	if err := store.Pool.QueryRow(ctx, `select count(*) from projects where default_assignee is not null`).Scan(&defaults); err != nil {
		t.Fatalf("read the defaults at %d: %v", projectsDefaultAssigneeNotNullVersion-1, err)
	}
	if defaults != 0 {
		t.Fatalf("projects naming a default at %d = %d, want none", projectsDefaultAssigneeNotNullVersion-1, defaults)
	}

	if err := store.migrate(ctx, migrationsThrough(t, projectsDefaultAssigneeNotNullVersion)); err != nil {
		t.Fatalf("migrate to %d: %v", projectsDefaultAssigneeNotNullVersion, err)
	}

	for _, want := range []struct{ project, assignee, why string }{
		{"MAJOR", "alice@d.example", "alice holds two of the three issues; the creator, bob, is read only where no issue names a person"},
		{"TIE", "bob@d.example", "alice and bob hold one issue each, and bob's was created last"},
		{"STALE", "carol@d.example", "mallory holds every issue and no people row, so the fill passes over her and reads the creator"},
		{"NOISSUE", "dave@d.example", "no issue at all, so the project.created actor of kind user names its login in id"},
		{"AGENT", "erin@d.example", "the session that created it names nobody, and erin holds its one issue"},
		{"OWNED", "frank@d.example", "a personal token's actor names its person in owner"},
	} {
		var got string
		if err := store.Pool.QueryRow(ctx, `select default_assignee from projects where key = $1`, want.project).Scan(&got); err != nil {
			t.Errorf("read %s's default: %v", want.project, err)
			continue
		}
		if got != want.assignee {
			t.Errorf("%s's default = %q, want %q: %s", want.project, got, want.assignee, want.why)
		}
	}

	if _, err := store.Pool.Exec(ctx, `insert into projects (key, name) values ('NODEF', 'No default')`); !isSQLState(err, "23502") {
		t.Fatalf("a project inserted with no default: %v, want not_null_violation", err)
	}
}

// The census counts a project naming no default: the rows the migration rewrites, and the rows
// its `set not null` would refuse. Where every project names a person it answers 0, which is what
// the deployment takes after it has run the fill on the release still serving.
func TestTheCensusCountsAProjectWithNoDefault(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	if err := store.migrate(ctx, migrationsThrough(t, projectsDefaultAssigneeNotNullVersion-1)); err != nil {
		t.Fatalf("migrate through %d: %v", projectsDefaultAssigneeNotNullVersion-1, err)
	}
	if _, err := store.Pool.Exec(ctx, `
		insert into people (email) values ('alice@d.example');
		insert into projects (key, name, default_assignee) values
			('CORE', 'Core', 'alice@d.example'),
			('OTHER', 'Other', null);
	`); err != nil {
		t.Fatalf("seed a project with no default: %v", err)
	}
	if got := censusCount(t, store, projectsDefaultAssigneeNotNullVersion); got != 1 {
		t.Fatalf("census = %d, want 1: OTHER alone names no default", got)
	}
	if _, err := store.Pool.Exec(ctx, `update projects set default_assignee = 'alice@d.example' where default_assignee is null`); err != nil {
		t.Fatalf("name a person on every project: %v", err)
	}
	if got := censusCount(t, store, projectsDefaultAssigneeNotNullVersion); got != 0 {
		t.Fatalf("census = %d, want 0: every project names a person", got)
	}
}

// nameADefaultAssigneeOnEveryProject names a person on every project the database holds,
// bringing the schema to just below the backfill first. A test that seeds a project at an older
// schema and then migrates to head calls it in between: the backfill refuses a project no
// issue's assignee and no creator names a person who has signed in for, which such a seed, older
// than `people` or than `issues.assignee`, leaves.
func nameADefaultAssigneeOnEveryProject(t *testing.T, store *Store) {
	t.Helper()
	migrateThrough(t, store, projectsDefaultAssigneeNotNullVersion-1)
	if _, err := store.Pool.Exec(context.Background(), `
		insert into people (email) values ('alice@d.example') on conflict do nothing;
		update projects set default_assignee = 'alice@d.example' where default_assignee is null;
	`); err != nil {
		t.Fatalf("name a default assignee on every project: %v", err)
	}
}

// seedCoreWithADefaultAssignee inserts project CORE naming a person, for a test at the current
// schema whose own seed helper cannot name one because it seeds at older schemas too
// (20261111210103 made projects.default_assignee not null). seedBlockIndexIssue and
// seedIssueWithSpec insert CORE only where it is not there already, so this stands in for theirs.
func seedCoreWithADefaultAssignee(t *testing.T, store *Store) {
	t.Helper()
	if _, err := store.Pool.Exec(context.Background(), `
		insert into projects (key, name, default_assignee) values ('CORE', 'Core', 'alice@d.example')
	`); err != nil {
		t.Fatalf("seed project CORE naming a default assignee: %v", err)
	}
}

// seedProjectsOfEveryKind inserts the people, projects, issues and project.created events the
// fill reads: each project is one of the kinds the two sources have to tell apart.
func seedProjectsOfEveryKind(t *testing.T, store *Store) {
	t.Helper()
	if _, err := store.Pool.Exec(context.Background(), `
		insert into people (email) values
			('alice@d.example'), ('bob@d.example'), ('carol@d.example'),
			('dave@d.example'), ('erin@d.example'), ('frank@d.example');

		insert into projects (key, name) values
			('MAJOR', 'A majority assignee'),
			('TIE', 'A tie the most recent issue breaks'),
			('STALE', 'An assignee no people row holds'),
			('NOISSUE', 'No issue at all'),
			('AGENT', 'Created by an agent session'),
			('OWNED', 'Created with a person''s own token');

		insert into issues (key, project_key, number, title, created_by, rank, assignee, created_at) values
			('MAJOR-1', 'MAJOR', 1, 'One', '{"kind":"user","id":"alice@d.example"}', '0000001', 'alice@d.example', '2026-11-01T00:00:00Z'),
			('MAJOR-2', 'MAJOR', 2, 'Two', '{"kind":"user","id":"alice@d.example"}', '0000002', 'alice@d.example', '2026-11-02T00:00:00Z'),
			('MAJOR-3', 'MAJOR', 3, 'Three', '{"kind":"user","id":"bob@d.example"}', '0000003', 'bob@d.example', '2026-11-03T00:00:00Z'),
			('TIE-1', 'TIE', 1, 'One', '{"kind":"user","id":"alice@d.example"}', '0000001', 'alice@d.example', '2026-11-01T00:00:00Z'),
			('TIE-2', 'TIE', 2, 'Two', '{"kind":"user","id":"bob@d.example"}', '0000002', 'bob@d.example', '2026-11-04T00:00:00Z'),
			('STALE-1', 'STALE', 1, 'One', '{"kind":"user","id":"carol@d.example"}', '0000001', 'mallory@d.example', '2026-11-05T00:00:00Z'),
			('STALE-2', 'STALE', 2, 'Two', '{"kind":"user","id":"carol@d.example"}', '0000002', 'mallory@d.example', '2026-11-06T00:00:00Z'),
			('AGENT-1', 'AGENT', 1, 'One', '{"kind":"session","id":"session-1"}', '0000001', 'erin@d.example', '2026-11-07T00:00:00Z');

		insert into events (issue_key, project_key, seq, type, actor, payload, notify) values
			(null, 'MAJOR', 1, 'project.created', '{"kind":"user","id":"bob@d.example"}', '{}', false),
			(null, 'STALE', 1, 'project.created', '{"kind":"user","id":"carol@d.example"}', '{}', false),
			(null, 'NOISSUE', 1, 'project.created', '{"kind":"user","id":"dave@d.example"}', '{}', false),
			(null, 'AGENT', 1, 'project.created', '{"kind":"session","id":"session-1"}', '{}', false),
			(null, 'OWNED', 1, 'project.created', '{"kind":"session","id":"","owner":"frank@d.example"}', '{}', false);
	`); err != nil {
		t.Fatalf("seed a project of every kind: %v", err)
	}
}
