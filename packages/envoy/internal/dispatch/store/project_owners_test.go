package store

import (
	"context"
	"strings"
	"testing"
)

// projectAccessVersion stores who may see a project: projects.owner and projects.visibility, the
// access lists, the groups, people.member_kind, agent_tokens.projects and the sessions' projects
// (INFRA-3862).
const projectAccessVersion = 20261116015736

// The person the operator's setting names in these tests (DISPATCH_PROJECT_OWNER_FALLBACK).
const ownerFallback = "operator@example.com"

// TestEveryProjectHasAnOwner (c-owner): the migration gives each project its default assignee as
// owner where that names a person who has signed in; the boot fills every project it left with no
// owner from the operator's setting; and a project created afterwards takes its default assignee
// as it is inserted. Then no project is without an owner.
func TestEveryProjectHasAnOwner(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	if err := store.migrate(ctx, migrationsThrough(t, projectAccessVersion-1)); err != nil {
		t.Fatalf("migrate through %d: %v", projectAccessVersion-1, err)
	}
	if _, err := store.Pool.Exec(ctx, `insert into people (email) values ('carol@example.com'), ($1)`, ownerFallback); err != nil {
		t.Fatalf("seed people before %d: %v", projectAccessVersion, err)
	}
	if _, err := store.Pool.Exec(ctx, `
		insert into projects (key, name, default_assignee) values
			('SIGNED', 'Its default has signed in', 'carol@example.com'),
			('STRAY', 'Its default never signed in', 'gone@example.com')
	`); err != nil {
		t.Fatalf("seed projects before %d: %v", projectAccessVersion, err)
	}
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate to head: %v", err)
	}
	if _, err := store.Pool.Exec(ctx, `
		insert into projects (key, name, default_assignee) values ('LATER', 'Created after the migration', 'dave@example.com')
	`); err != nil {
		t.Fatalf("create a project after the migration: %v", err)
	}

	if err := EnsureProjectOwners(ctx, store.Pool, ownerFallback); err != nil {
		t.Fatalf("EnsureProjectOwners(%q): %v", ownerFallback, err)
	}

	for _, want := range []struct{ project, owner, why string }{
		{"SIGNED", "carol@example.com", "the migration's fill: its default assignee has signed in"},
		{"STRAY", ownerFallback, "the boot's fallback: its default assignee never signed in"},
		{"LATER", "dave@example.com", "the insert trigger: a project created with no owner takes its default assignee"},
	} {
		if got := projectOwner(t, store, want.project); got != want.owner {
			t.Errorf("project %s owner = %q, want %q (%s)", want.project, got, want.owner, want.why)
		}
	}
	if stranded := projectsWithNoOwner(t, store); len(stranded) != 0 {
		t.Fatalf("projects with no owner after the boot: %s", strings.Join(stranded, ", "))
	}
}

// The boot refuses a database where a project still has no owner, naming every such project and
// both repairs: when the operator's setting is empty, and when it names somebody who is not a full
// member, because an owner is always a full member. The refusal names no person's email, neither
// the default assignees' nor the setting's, since the boot writes it to its log.
func TestTheBootNamesEveryProjectItCannotGiveAnOwner(t *testing.T) {
	ctx := context.Background()
	for _, setting := range []struct{ name, fallback string }{
		{"no setting", ""},
		{"a setting that names nobody who signed in", "nobody@example.com"},
		{"a setting that names a restricted member", "outsider@other.example"},
	} {
		t.Run(setting.name, func(t *testing.T) {
			store := openEmptyTestStore(t)
			if err := store.migrate(ctx, migrationsThrough(t, projectAccessVersion-1)); err != nil {
				t.Fatalf("migrate through %d: %v", projectAccessVersion-1, err)
			}
			if _, err := store.Pool.Exec(ctx, `
				insert into projects (key, name, default_assignee) values
					('STRAY', 'Stray', 'gone@example.com'),
					('LOST', 'Lost', 'left@example.com');
			`); err != nil {
				t.Fatalf("seed projects: %v", err)
			}
			if err := store.Migrate(ctx); err != nil {
				t.Fatalf("migrate to head: %v", err)
			}
			if _, err := store.Pool.Exec(ctx, `
				insert into people (email, member_kind) values ('outsider@other.example', 'restricted')
			`); err != nil {
				t.Fatalf("seed a restricted member: %v", err)
			}

			err := EnsureProjectOwners(ctx, store.Pool, setting.fallback)
			if err == nil {
				t.Fatalf("EnsureProjectOwners(%q) = nil, want a refusal naming LOST and STRAY", setting.fallback)
			}
			for _, named := range []string{"LOST", "STRAY", "DISPATCH_PROJECT_OWNER_FALLBACK", "envoy-dispatch project-owner"} {
				if !strings.Contains(err.Error(), named) {
					t.Errorf("EnsureProjectOwners(%q) = %q, want it to name %s", setting.fallback, err, named)
				}
			}
			for _, email := range []string{"gone@example.com", "left@example.com", "nobody@example.com", "outsider@other.example", "@"} {
				if strings.Contains(err.Error(), email) {
					t.Errorf("EnsureProjectOwners(%q) = %q, which names %q: the boot logs it", setting.fallback, err, email)
				}
			}
			if stranded := projectsWithNoOwner(t, store); len(stranded) != 2 {
				t.Errorf("projects with no owner = %v, want LOST and STRAY left as they were", stranded)
			}
		})
	}
}

// TestAWriteThatClearsTheOwnerIsRefused: once a project has an owner, no write can take it away,
// so a project never goes back to having nobody who may change its access.
func TestAWriteThatClearsTheOwnerIsRefused(t *testing.T) {
	ctx := context.Background()
	store := openMigratedProjectStore(t)
	if _, err := store.Pool.Exec(ctx, `insert into projects (key, name, default_assignee) values ('KEPT', 'Kept', 'carol@example.com')`); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	_, err := store.Pool.Exec(ctx, `update projects set owner = null where key = 'KEPT'`)
	if !isSQLState(err, "23514") {
		t.Fatalf("clearing the owner: %v, want check_violation (23514)", err)
	}
	if got := projectOwner(t, store, "KEPT"); got != "carol@example.com" {
		t.Fatalf("owner after the refused write = %q, want carol@example.com", got)
	}
	// Moving the owner to another person is a write the trigger lets through.
	if _, err := store.Pool.Exec(ctx, `update projects set owner = 'dave@example.com' where key = 'KEPT'`); err != nil {
		t.Fatalf("giving the project to another person: %v", err)
	}
}

// TestAnInsertWithNoOwnerTakesTheDefaultAssignee: every insert into projects, whichever of the
// many writers makes it, stores an owner, because a project with no owner named takes its default
// assignee; an insert that names an owner keeps it.
func TestAnInsertWithNoOwnerTakesTheDefaultAssignee(t *testing.T) {
	ctx := context.Background()
	store := openMigratedProjectStore(t)
	if _, err := store.Pool.Exec(ctx, `
		insert into projects (key, name, default_assignee) values ('DEFAULT', 'Default', 'carol@example.com');
		insert into projects (key, name, default_assignee, owner) values ('NAMED', 'Named', 'carol@example.com', 'dave@example.com');
	`); err != nil {
		t.Fatalf("seed projects: %v", err)
	}
	if got := projectOwner(t, store, "DEFAULT"); got != "carol@example.com" {
		t.Errorf("owner of a project inserted with none = %q, want its default assignee carol@example.com", got)
	}
	if got := projectOwner(t, store, "NAMED"); got != "dave@example.com" {
		t.Errorf("owner of a project inserted with one = %q, want dave@example.com as named", got)
	}
}

// openMigratedProjectStore is a database of its own carrying every migration.
func openMigratedProjectStore(t *testing.T) *Store {
	t.Helper()
	store := openEmptyTestStore(t)
	if err := store.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return store
}

func projectOwner(t *testing.T, store *Store, key string) string {
	t.Helper()
	var owner *string
	if err := store.Pool.QueryRow(context.Background(), `select owner from projects where key = $1`, key).Scan(&owner); err != nil {
		t.Fatalf("read project %s owner: %v", key, err)
	}
	if owner == nil {
		return ""
	}
	return *owner
}

func projectsWithNoOwner(t *testing.T, store *Store) []string {
	t.Helper()
	rows, err := store.Pool.Query(context.Background(), `select key from projects where owner is null order by key`)
	if err != nil {
		t.Fatalf("read projects with no owner: %v", err)
	}
	defer rows.Close()
	var keys []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			t.Fatalf("scan project key: %v", err)
		}
		keys = append(keys, key)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read projects with no owner: %v", err)
	}
	return keys
}
