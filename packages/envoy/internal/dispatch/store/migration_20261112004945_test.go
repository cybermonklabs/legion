package store

import (
	"context"
	"testing"
)

// issueRepoNeedsNoMappingVersion drops the foreign key 0113 put on issues.repo and the unique
// constraint on repo_projects (repo, project) that backed it, so an issue's own repository is any
// owner/name (INFRA-2543 R3).
const issueRepoNeedsNoMappingVersion = 20261112004945

// TestMigrationLetsAnIssueNameAnyRepository runs the shipped files over rows written as the
// release before this one wrote them: an issue naming a mapping of its own project, which is all
// the foreign key admitted. It proves the drop by what the schema takes afterwards, rather than
// by the catalogue alone: a repository another project maps, a repository no project maps, and
// the move and the delete of a mapping an issue names, each of which the foreign key refused.
func TestMigrationLetsAnIssueNameAnyRepository(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	if err := store.migrate(ctx, migrationsThrough(t, issueRepoNeedsNoMappingVersion-1)); err != nil {
		t.Fatalf("migrate through %d: %v", issueRepoNeedsNoMappingVersion-1, err)
	}
	if _, err := store.Pool.Exec(ctx, `
		insert into projects (key, name) values ('CORE', 'Core'), ('OTHER', 'Other');
		insert into repo_projects (repo, project, created_by) values
			('acme/widgets', 'CORE', '{"kind":"user","id":"alice"}'),
			('other/tools', 'OTHER', '{"kind":"user","id":"alice"}');
		insert into issues (key, project_key, number, title, created_by, rank, repo)
			values ('CORE-1', 'CORE', 1, 'One', '{"kind":"user","id":"alice"}', 'U', 'acme/widgets');
	`); err != nil {
		t.Fatalf("seed as the release before %d wrote: %v", issueRepoNeedsNoMappingVersion, err)
	}

	// The negative control of the three cases below, inside this one run: at the schema the
	// release before this one carried, the foreign key refuses each of them, so taking them
	// afterwards is this migration's doing and not a rule that was never there.
	for name, statement := range map[string]string{
		"a repository another project maps": `update issues set repo = 'other/tools' where key = 'CORE-1'`,
		"a repository no project maps":      `update issues set repo = 'acme/nobody' where key = 'CORE-1'`,
		"moving the mapping CORE-1 names":   `update repo_projects set project = 'OTHER' where repo = 'acme/widgets'`,
		"deleting the mapping CORE-1 names": `delete from repo_projects where repo = 'acme/widgets'`,
	} {
		if _, err := store.Pool.Exec(ctx, statement); !isSQLState(err, "23503") {
			t.Fatalf("%s at %d: %v, want foreign_key_violation", name, issueRepoNeedsNoMappingVersion-1, err)
		}
	}
	for _, constraint := range []string{"issues_repo_mapped_to_project", "repo_projects_repo_project_key"} {
		if !constraintExists(t, store, constraint) {
			t.Fatalf("%s is absent at %d, want the constraint %d drops to exist before it runs", constraint, issueRepoNeedsNoMappingVersion-1, issueRepoNeedsNoMappingVersion)
		}
	}

	if err := store.migrate(ctx, migrationsThrough(t, issueRepoNeedsNoMappingVersion)); err != nil {
		t.Fatalf("migrate to %d: %v", issueRepoNeedsNoMappingVersion, err)
	}

	for _, constraint := range []string{"issues_repo_mapped_to_project", "repo_projects_repo_project_key"} {
		if constraintExists(t, store, constraint) {
			t.Errorf("%s is still here after %d, want it dropped", constraint, issueRepoNeedsNoMappingVersion)
		}
	}
	var kept string
	if err := store.Pool.QueryRow(ctx, `select repo from issues where key = 'CORE-1'`).Scan(&kept); err != nil || kept != "acme/widgets" {
		t.Fatalf("CORE-1's repo after %d = %q (%v), want the acme/widgets the seed wrote", issueRepoNeedsNoMappingVersion, kept, err)
	}
	for name, statement := range map[string]string{
		"a repository another project maps": `update issues set repo = 'other/tools' where key = 'CORE-1'`,
		"a repository no project maps":      `update issues set repo = 'acme/nobody' where key = 'CORE-1'`,
		"moving the mapping CORE-1 names":   `update repo_projects set project = 'OTHER' where repo = 'acme/widgets'`,
		"deleting the mapping CORE-1 names": `delete from repo_projects where repo = 'acme/widgets'`,
	} {
		if _, err := store.Pool.Exec(ctx, statement); err != nil {
			t.Errorf("%s after %d: %v, want it taken", name, issueRepoNeedsNoMappingVersion, err)
		}
	}
	// An insert carries the same rule as an update, and it is the statement the server's create
	// sends: an issue of CORE naming a repository no project maps is stored.
	if _, err := store.Pool.Exec(ctx, `
		insert into issues (key, project_key, number, title, created_by, rank, repo)
			values ('CORE-2', 'CORE', 2, 'Two', '{"kind":"user","id":"alice"}', 'V', 'acme/nobody')
	`); err != nil {
		t.Errorf("insert an issue naming a repository no project maps: %v", err)
	}
	// What the dropped unique constraint said is still true, because repo_projects' primary key
	// says it: one repository maps to one project, so dropping the pair lost no rule.
	if _, err := store.Pool.Exec(ctx, `
		insert into repo_projects (repo, project, created_by)
			values ('other/tools', 'CORE', '{"kind":"user","id":"alice"}')
	`); !isSQLState(err, "23505") {
		t.Errorf("mapping other/tools to a second project: %v, want unique_violation from the primary key", err)
	}
}

// constraintExists answers whether the database holds a table constraint of that name.
func constraintExists(t *testing.T, store *Store, name string) bool {
	t.Helper()
	var exists bool
	if err := store.Pool.QueryRow(context.Background(), `
		select exists (select 1 from pg_constraint where conname = $1)
	`, name).Scan(&exists); err != nil {
		t.Fatalf("read pg_constraint for %s: %v", name, err)
	}
	return exists
}
