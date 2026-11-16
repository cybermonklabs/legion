package access

import (
	"context"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// seedListed is a database where project PRIV names outsider@other.example directly, project
// OWNED is owned by owner@other.example, and project GROUPED lists the group crew, whose one
// member is member@other.example, and a group principal spelled exactly like an email (no group
// may be named so, but a list row is held to no group).
func seedListed(t *testing.T) *store.Store {
	t.Helper()
	database := storetest.Open(t)
	if _, err := database.Pool.Exec(context.Background(), `
		insert into projects (key, name, default_assignee) values
			('PRIV', 'Private', 'olga@example.com'),
			('OWNED', 'Owned', 'owner@other.example'),
			('GROUPED', 'Grouped', 'olga@example.com');
		insert into access_groups (name, owner) values ('crew', 'olga@example.com');
		insert into access_group_members (group_name, person) values ('crew', 'member@other.example');
		insert into project_access (project_key, principal_kind, principal, added_by) values
			('PRIV', 'person', 'outsider@other.example', '{"kind":"user","id":"olga@example.com"}'),
			('GROUPED', 'group', 'crew', '{"kind":"user","id":"olga@example.com"}'),
			('GROUPED', 'group', 'lookalike@other.example', '{"kind":"user","id":"olga@example.com"}');
	`); err != nil {
		t.Fatalf("seed access lists: %v", err)
	}
	return database
}

func listedOf(t *testing.T, database *store.Store, email string) bool {
	t.Helper()
	listed, err := Listed(context.Background(), database.Pool, email)
	if err != nil {
		t.Fatalf("Listed(%q): %v", email, err)
	}
	return listed
}

func TestListedNamesAPersonOnAnAccessList(t *testing.T) {
	if !listedOf(t, seedListed(t), "outsider@other.example") {
		t.Fatal("Listed(outsider@other.example) = false, want true: PRIV's access list names it")
	}
}

func TestListedNamesAnOwner(t *testing.T) {
	if !listedOf(t, seedListed(t), "owner@other.example") {
		t.Fatal("Listed(owner@other.example) = false, want true: it owns OWNED")
	}
}

// A group on an access list never admits an outsider: only a project's owner or a person its
// list names directly signs in from outside (v24 R2).
func TestListedIsFalseForAMemberOfAListedGroup(t *testing.T) {
	if listedOf(t, seedListed(t), "member@other.example") {
		t.Fatal("Listed(member@other.example) = true, want false: only the group crew on GROUPED's list holds it")
	}
}

func TestListedIsFalseForAnEmailNoListNames(t *testing.T) {
	if listedOf(t, seedListed(t), "stranger@other.example") {
		t.Fatal("Listed(stranger@other.example) = true, want false: no list names it")
	}
}

func TestListedIsFalseForAGroupNamedLikeTheEmail(t *testing.T) {
	if listedOf(t, seedListed(t), "lookalike@other.example") {
		t.Fatal("Listed(lookalike@other.example) = true, want false: only a group of that name is listed")
	}
}

func TestListedComparesTheEmailInLowerCase(t *testing.T) {
	if !listedOf(t, seedListed(t), " Outsider@Other.example ") {
		t.Fatal("Listed(\" Outsider@Other.example \") = false, want true: PRIV names outsider@other.example")
	}
}
