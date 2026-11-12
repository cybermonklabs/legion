package store

import (
	"context"
	"testing"

	"github.com/sjawhar/envoy/internal/pgmigrate"
)

// issueRepoAnyRepositoryVersion drops the foreign key that held an issue's own repository to a
// mapping of its project, and the unique constraint that backed it (INFRA-2543).
const issueRepoAnyRepositoryVersion = 20261112023740

// issueRepoConstraints answers which of the two constraints the migration drops the catalog
// still holds, by name.
func issueRepoConstraints(t *testing.T, ctx context.Context, store *Store) map[string]bool {
	t.Helper()
	rows, err := store.Pool.Query(ctx, `
		select conname from pg_constraint
		where conname in ('issues_repo_mapped_to_project', 'repo_projects_repo_project_key')
	`)
	if err != nil {
		t.Fatalf("read the constraints: %v", err)
	}
	defer rows.Close()
	held := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("read a constraint: %v", err)
		}
		held[name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the constraints: %v", err)
	}
	return held
}

// The migration runs over rows the release before it wrote: an issue naming its own project's
// mapping and an issue naming none. Before it, the foreign key refuses an issue repository that is
// another project's mapping or nobody's, and refuses the move and the delete of a mapping an issue
// names; that is the control, run in this same test, which shows each write below fails for the
// constraint and not for some other reason. After it, each of those writes succeeds, the issue that
// named a mapping keeps its repository, and a repository is still mapped to one project only, by
// repo_projects' primary key.
func TestMigrationLetsAnIssueNameARepositoryNoMappingOfItsProjectNames(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	migrateThrough(t, store, issueRepoAnyRepositoryVersion-1)
	if _, err := store.Pool.Exec(ctx, `
		insert into projects (key, name) values ('CORE', 'Core'), ('OTHER', 'Other');
		insert into repo_projects (repo, project, created_by, is_default) values
			('acme/widgets', 'CORE', '{"kind":"user","id":"alice"}', true),
			('acme/gadgets', 'OTHER', '{"kind":"user","id":"alice"}', false);
		insert into issues (key, project_key, number, title, created_by, rank, repo) values
			('CORE-1', 'CORE', 1, 'Names its mapping', '{"kind":"user","id":"alice"}', 'U', 'acme/widgets'),
			('CORE-2', 'CORE', 2, 'Names none', '{"kind":"user","id":"alice"}', 'V', null);
	`); err != nil {
		t.Fatalf("seed as the release before the migration wrote: %v", err)
	}

	if held := issueRepoConstraints(t, ctx, store); !held["issues_repo_mapped_to_project"] || !held["repo_projects_repo_project_key"] {
		t.Fatalf("constraints before the migration = %v, want both that it drops", held)
	}
	// The control: each write the migration is for, refused by the foreign key before it runs.
	// Each runs in a transaction rolled back after, so the rows stay as the old release left them.
	for _, write := range []struct{ name, sql string }{
		{"another project's mapping on CORE-2", `update issues set repo = 'acme/gadgets' where key = 'CORE-2'`},
		{"a repository no project maps on CORE-2", `update issues set repo = 'zed/nobody' where key = 'CORE-2'`},
		{"a new issue on a repository no project maps", `insert into issues (key, project_key, number, title, created_by, rank, repo) values ('CORE-3', 'CORE', 3, 'Elsewhere', '{"kind":"user","id":"alice"}', 'W', 'zed/nobody')`},
		{"moving the mapping CORE-1 names", `update repo_projects set project = 'OTHER', is_default = false where repo = 'acme/widgets'`},
		{"deleting the mapping CORE-1 names", `delete from repo_projects where repo = 'acme/widgets'`},
	} {
		tx, err := store.Pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		_, err = tx.Exec(ctx, write.sql)
		_ = tx.Rollback(ctx)
		if !isSQLState(err, "23503") {
			t.Fatalf("before the migration, %s: %v, want foreign_key_violation", write.name, err)
		}
	}

	migrateThrough(t, store, issueRepoAnyRepositoryVersion)

	for _, write := range []struct{ name, sql string }{
		{"another project's mapping on CORE-2", `update issues set repo = 'acme/gadgets' where key = 'CORE-2'`},
		{"a new issue on a repository no project maps", `insert into issues (key, project_key, number, title, created_by, rank, repo) values ('CORE-3', 'CORE', 3, 'Elsewhere', '{"kind":"user","id":"alice"}', 'W', 'zed/nobody')`},
		{"moving the mapping CORE-1 names", `update repo_projects set project = 'OTHER', is_default = false where repo = 'acme/widgets'`},
		{"deleting the mapping CORE-1 names", `delete from repo_projects where repo = 'acme/widgets'`},
	} {
		if _, err := store.Pool.Exec(ctx, write.sql); err != nil {
			t.Fatalf("after the migration, %s: %v", write.name, err)
		}
	}
	if held := issueRepoConstraints(t, ctx, store); len(held) != 0 {
		t.Fatalf("constraints after the migration = %v, want neither", held)
	}
	var repos [3]*string
	if err := store.Pool.QueryRow(ctx, `
		select (select repo from issues where key = 'CORE-1'),
		       (select repo from issues where key = 'CORE-2'),
		       (select repo from issues where key = 'CORE-3')
	`).Scan(&repos[0], &repos[1], &repos[2]); err != nil {
		t.Fatalf("read the issues' repositories: %v", err)
	}
	for i, want := range []string{"acme/widgets", "acme/gadgets", "zed/nobody"} {
		if repos[i] == nil || *repos[i] != want {
			t.Errorf("CORE-%d's repo = %v, want %s", i+1, repos[i], want)
		}
	}
	if _, err := store.Pool.Exec(ctx, `
		insert into repo_projects (repo, project, created_by) values ('acme/gadgets', 'CORE', '{"kind":"user","id":"alice"}')
	`); !isSQLState(err, "23505") {
		t.Errorf("a second mapping of acme/gadgets: %v, want unique_violation from the primary key", err)
	}
}

// The census of the migration counts 0 and refuses nothing at a table limit of one byte, a size
// every non-empty table is over: both its statements are DROP CONSTRAINT RESTRICT, which the
// catalog-only exemption admits, so the deploy does not weigh issues or repo_projects for it.
func TestTheIssueRepoAnyRepositoryMigrationIsExemptFromTheTableLimit(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	migrateThrough(t, store, issueRepoAnyRepositoryVersion-1)
	if _, err := store.Pool.Exec(ctx, `
		insert into projects (key, name) values ('CORE', 'Core');
		insert into repo_projects (repo, project, created_by) values ('acme/widgets', 'CORE', '{"kind":"user","id":"alice"}');
		insert into issues (key, project_key, number, title, created_by, rank, repo)
			values ('CORE-1', 'CORE', 1, 'One', '{"kind":"user","id":"alice"}', 'U', 'acme/widgets');
	`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	report, err := census(ctx, store.Pool.Config().ConnString(), migrationsThrough(t, issueRepoAnyRepositoryVersion), pgmigrate.CensusOptions{TableLimit: 1})
	if err != nil {
		t.Fatalf("census: %v", err)
	}
	if report.Refused() {
		t.Fatalf("%d refused:\n%s", issueRepoAnyRepositoryVersion, reportText(report))
	}
	if len(report.Pending) != 1 || report.Pending[0].Migration.Version != issueRepoAnyRepositoryVersion {
		t.Fatalf("pending = %+v, want %d alone", report.Pending, issueRepoAnyRepositoryVersion)
	}
	entry := report.Pending[0]
	if entry.TableSizeExemption != "alter table" {
		t.Errorf("TableSizeExemption = %q, want %q", entry.TableSizeExemption, "alter table")
	}
	if entry.Count == nil || *entry.Count != 0 {
		t.Errorf("census count = %v, want 0", entry.Count)
	}
}
