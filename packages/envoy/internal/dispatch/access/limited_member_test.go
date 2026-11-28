package access

import (
	"context"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// TestALimitedMemberIsAdmittedThroughAGroup (plan D8, R1): bob, a limited member PRIV names
// directly, is a member of the group crew, which OTHER's access list names; a group admits a
// limited member as a direct listing does, so bob sees OTHER and PRIV, by Resolve and by
// store.PersonSeesProjectSQL alike, and stays a limited member (no open viewer). His token that
// lists OTHER sees it; one that lists nothing does not, since OTHER is private.
func TestALimitedMemberIsAdmittedThroughAGroup(t *testing.T) {
	ctx := context.Background()
	database := seedAccess(t)
	viewer, err := Resolve(ctx, database.Pool, cookie(bob))
	if err != nil {
		t.Fatalf("Resolve bob's browser: %v", err)
	}
	if !slices.Equal(viewer.Sees, []string{"OTHER", "PRIV"}) || viewer.Open || !viewer.Restricted {
		t.Errorf("bob resolves %+v, want a limited member that sees OTHER, through the group crew OTHER lists, and PRIV, which lists him", viewer)
	}
	var admitted bool
	if err := database.Pool.QueryRow(ctx, `select `+store.PersonSeesProjectSQL("$1", "(select member_kind from people where email = $1)", "p")+` from projects p where p.key = 'OTHER'`,
		bob).Scan(&admitted); err != nil {
		t.Fatalf("PersonSeesProjectSQL(bob, OTHER): %v", err)
	}
	if !admitted {
		t.Error("PersonSeesProjectSQL(bob, OTHER) = false, want true: a group admits a limited member as a direct listing does")
	}
	for _, token := range []struct {
		name   string
		actor  model.Actor
		others bool
	}{
		{"bob's token that lists OTHER", personalToken(bob, "OTHER"), true},
		{"bob's token that lists nothing", personalToken(bob), false},
	} {
		viewer, err := Resolve(ctx, database.Pool, token.actor)
		if err != nil {
			t.Fatalf("%s: Resolve: %v", token.name, err)
		}
		if got := slices.Contains(viewer.Sees, "OTHER"); got != token.others {
			t.Errorf("%s sees OTHER = %t (%+v), want %t: OTHER is private and bob sees it through the group", token.name, got, viewer, token.others)
		}
	}
}

// TestAGroupWriteTakesALimitedMember (plan D8, R1): a group's write keeps a limited member among
// its members: crew written again with bob, the limited member it holds, and dave, and a new group
// mixed made with them, each answer no error and hold both. Listing mixed on PUB, a public project
// no list names bob on, admits bob to PUB, which a limited member sees only where a list names him.
func TestAGroupWriteTakesALimitedMember(t *testing.T) {
	ctx := context.Background()
	database := seedAccess(t)
	broker := events.NewBroker()
	by := model.Actor{Kind: "user", ID: olga}
	for _, group := range []string{"crew", "mixed"} {
		if err := inTx(t, database, func(tx pgx.Tx) error {
			_, _, err := PutGroup(ctx, tx, broker, group, olga, []string{dave, bob}, by)
			return err
		}); err != nil {
			t.Fatalf("PutGroup(%s) with the limited member bob: %v, want it written", group, err)
		}
		written, err := ReadGroup(ctx, database.Pool, group)
		if err != nil || !slices.Equal(written.Members, []string{bob, dave}) {
			t.Errorf("%s = %+v, %v, want the members bob and dave", group, written, err)
		}
	}
	viewer, err := Resolve(ctx, database.Pool, cookie(bob))
	if err != nil {
		t.Fatalf("Resolve bob's browser: %v", err)
	}
	if slices.Contains(viewer.Sees, "PUB") {
		t.Fatalf("bob sees PUB before any list names him, as %+v: a limited member sees no public project whose list does not name him", viewer)
	}
	if err := inTx(t, database, func(tx pgx.Tx) error {
		_, _, err := AddPrincipal(ctx, tx, broker, "PUB", PrincipalGroup, "mixed", by)
		return err
	}); err != nil {
		t.Fatalf("list mixed on PUB: %v", err)
	}
	if viewer, err = Resolve(ctx, database.Pool, cookie(bob)); err != nil {
		t.Fatalf("Resolve bob's browser: %v", err)
	}
	if !slices.Contains(viewer.Sees, "PUB") {
		t.Errorf("bob resolves %+v once PUB lists the group mixed he is a member of, want PUB among what he sees", viewer)
	}
}

// TestALimitedMemberRemovedThroughAGroupHasItsReachedProjectsRecorded (plan D8, R1): bob, a
// limited member, sees OTHER through the group crew, and his session ses_bob is tied to him. Each
// change that takes OTHER from him through the group - taking him out of crew, taking crew off
// OTHER's list, deleting crew - counts him among the members it admitted, so OTHER is recorded as
// a project ses_bob reached (session_reached_projects), as it is for a full member, and the
// content rule keeps what the session read while he saw it.
func TestALimitedMemberRemovedThroughAGroupHasItsReachedProjectsRecorded(t *testing.T) {
	ctx := context.Background()
	by := model.Actor{Kind: "user", ID: olga}
	for _, removal := range []struct {
		name  string
		write func(tx pgx.Tx, broker *events.Broker) error
	}{
		{"taken out of the group", func(tx pgx.Tx, broker *events.Broker) error {
			_, _, err := PutGroup(ctx, tx, broker, "crew", olga, []string{dave}, by)
			return err
		}},
		{"the group taken off the project's list", func(tx pgx.Tx, broker *events.Broker) error {
			_, _, err := RemovePrincipal(ctx, tx, broker, "OTHER", PrincipalGroup, "crew", by)
			return err
		}},
		{"the group deleted", func(tx pgx.Tx, broker *events.Broker) error {
			_, err := DeleteGroup(ctx, tx, broker, "crew", by)
			return err
		}},
	} {
		t.Run(removal.name, func(t *testing.T) {
			database := seedAccess(t)
			if err := TieSession(ctx, database.Pool, "ses_bob", bob); err != nil {
				t.Fatalf("tie ses_bob to bob: %v", err)
			}
			before, err := Resolve(ctx, database.Pool, cookie(bob))
			if err != nil {
				t.Fatalf("Resolve bob's browser: %v", err)
			}
			if !slices.Contains(before.Sees, "OTHER") {
				t.Errorf("before the change bob resolves %+v, want OTHER, which he sees through the group crew", before)
			}
			if err := inTx(t, database, func(tx pgx.Tx) error { return removal.write(tx, events.NewBroker()) }); err != nil {
				t.Fatalf("%s: %v", removal.name, err)
			}
			after, err := Resolve(ctx, database.Pool, cookie(bob))
			if err != nil {
				t.Fatalf("Resolve bob's browser: %v", err)
			}
			if slices.Contains(after.Sees, "OTHER") {
				t.Errorf("after the change bob still resolves %+v, want no OTHER", after)
			}
			var reached int
			if err := database.Pool.QueryRow(ctx, `select count(*) from session_reached_projects where session_id = 'ses_bob' and project_key = 'OTHER'`).Scan(&reached); err != nil {
				t.Fatalf("read ses_bob's reached projects: %v", err)
			}
			if reached != 1 {
				t.Errorf("ses_bob has %d reached pairs of OTHER after bob lost it through the group, want 1: a group change counts every member it admitted, a limited member too", reached)
			}
		})
	}
}
