package access

import (
	"context"
	"slices"
	"testing"
)

// TestTheReadsNameWhoHasNotSignedInYet: a list and a group may name a person nobody has signed in
// as yet, whom the rule admits once they sign in (a group only as a full member), so the reads say
// which of their people that is, and the page can show them as not signed in yet.
func TestTheReadsNameWhoHasNotSignedInYet(t *testing.T) {
	ctx := context.Background()
	database := seedAccess(t)
	if _, err := database.Pool.Exec(ctx, `
		insert into project_access (project_key, principal_kind, principal, added_by) values
			('PRIV', 'person', 'newcomer@example.com', '{"kind":"user","id":"olga@example.com"}');
		insert into access_group_members (group_name, person) values ('crew', 'newcomer@example.com');
	`); err != nil {
		t.Fatalf("name a person nobody has signed in as: %v", err)
	}
	project, err := ReadProject(ctx, database.Pool, "PRIV")
	if err != nil || !slices.Equal(project.NotSignedIn, []string{"newcomer@example.com"}) {
		t.Fatalf("PRIV's not_signed_in = %v, %v, want newcomer@example.com alone", project.NotSignedIn, err)
	}
	if other, err := ReadProject(ctx, database.Pool, "PUB"); err != nil || other.NotSignedIn == nil || len(other.NotSignedIn) != 0 {
		t.Fatalf("PUB's not_signed_in = %#v, %v, want the empty list", other.NotSignedIn, err)
	}
	group, err := ReadGroup(ctx, database.Pool, "crew")
	if err != nil || !slices.Equal(group.NotSignedIn, []string{"newcomer@example.com"}) {
		t.Fatalf("crew's not_signed_in = %v, %v, want newcomer@example.com alone", group.NotSignedIn, err)
	}
	groups, err := ListGroups(ctx, database.Pool)
	if err != nil || len(groups) != 1 || !slices.Equal(groups[0].NotSignedIn, []string{"newcomer@example.com"}) {
		t.Fatalf("ListGroups = %+v, %v, want crew naming newcomer@example.com as not signed in", groups, err)
	}
}
