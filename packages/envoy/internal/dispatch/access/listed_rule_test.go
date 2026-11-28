package access

import (
	"context"
	"testing"
)

// TestListedAgreesWithTheReadRule: outside sign-in admits an email exactly when Listed says some
// project names it directly, and the person it admits is a restricted member, whom the read rule
// admits to a project only by its owner or its access list naming them. So, for every seeded
// person read as a restricted member, Listed is true exactly when the read rule shows them some
// project: the two cannot drift apart.
func TestListedAgreesWithTheReadRule(t *testing.T) {
	ctx := context.Background()
	database := seedAccess(t)
	for _, person := range []string{alice, bob, carol, dave, olga, newbie} {
		tx, err := database.Pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		if _, err := tx.Exec(ctx, `
			insert into people (email, member_kind) values ($1, 'restricted')
			on conflict (email) do update set member_kind = 'restricted'
		`, person); err != nil {
			t.Fatalf("read %s as a restricted member: %v", person, err)
		}
		listed, err := Listed(ctx, tx, person)
		if err != nil {
			t.Fatalf("Listed(%s): %v", person, err)
		}
		viewer, err := Resolve(ctx, tx, cookie(person))
		if err != nil {
			t.Fatalf("Resolve(%s): %v", person, err)
		}
		if seesAny := len(viewer.Sees) > 0; listed != seesAny {
			t.Errorf("%s: Listed = %t, and the read rule shows them %v as a restricted member", person, listed, viewer.Sees)
		}
		_ = tx.Rollback(ctx)
	}
}
