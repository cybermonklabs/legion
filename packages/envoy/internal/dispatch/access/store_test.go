package access

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// inTx runs write in one transaction of database, committed when it answers no error.
func inTx(t *testing.T, database *store.Store, write func(tx pgx.Tx) error) error {
	t.Helper()
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	if err := write(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return nil
}

// TestEveryAccessWriteAppendsItsEvent: each write of a project's owner, visibility or access list
// appends one project.access_changed on that project, naming the change and carrying the access
// as the write left it; each write of a group appends one access_group.changed, and a group's
// delete also takes it off every list that named it, with that list's own event.
func TestEveryAccessWriteAppendsItsEvent(t *testing.T) {
	ctx := context.Background()
	database := seedAccess(t)
	broker := events.NewBroker()
	olgaActor := model.Actor{Kind: "user", ID: olga}

	type written struct {
		name   string
		write  func(tx pgx.Tx) ([]model.Event, error)
		checks func(t *testing.T, appended []model.Event)
	}
	projectChange := func(project, change, principal string, holds func(model.ProjectAccess) bool) func(*testing.T, []model.Event) {
		return func(t *testing.T, appended []model.Event) {
			t.Helper()
			if len(appended) != 1 {
				t.Fatalf("%d events appended, want one project.access_changed", len(appended))
			}
			event := appended[0]
			payload, ok := event.Payload.(model.ProjectAccessEventPayload)
			if event.Type != "project.access_changed" || event.ProjectKey == nil || *event.ProjectKey != project || !ok ||
				payload.Change != change || payload.Principal != principal || !holds(payload.ProjectAccess) {
				t.Fatalf("appended %s on %v payload %+v, want project.access_changed on %s with change %s naming %q", event.Type, event.ProjectKey, event.Payload, project, change, principal)
			}
		}
	}
	for _, step := range []written{
		{"the owner gives PUB to carol", func(tx pgx.Tx) ([]model.Event, error) {
			_, event, err := SetOwner(ctx, tx, broker, "PUB", carol, olgaActor)
			return []model.Event{event}, err
		}, projectChange("PUB", model.AccessOwnerChanged, "", func(a model.ProjectAccess) bool { return a.Owner == carol })},
		{"PUB is made private", func(tx pgx.Tx) ([]model.Event, error) {
			_, event, err := SetVisibility(ctx, tx, broker, "PUB", model.ProjectPrivate, olgaActor)
			return []model.Event{event}, err
		}, projectChange("PUB", model.AccessVisibilityChanged, "", func(a model.ProjectAccess) bool { return a.Visibility == model.ProjectPrivate })},
		{"alice is added to PUB", func(tx pgx.Tx) ([]model.Event, error) {
			_, event, err := AddPrincipal(ctx, tx, broker, "PUB", PrincipalPerson, " Alice@Example.com ", olgaActor)
			return []model.Event{event}, err
		}, projectChange("PUB", model.AccessPersonAdded, alice, func(a model.ProjectAccess) bool { return slices.Equal(a.People, []string{alice}) })},
		{"the group crew is added to PUB", func(tx pgx.Tx) ([]model.Event, error) {
			_, event, err := AddPrincipal(ctx, tx, broker, "PUB", PrincipalGroup, "crew", olgaActor)
			return []model.Event{event}, err
		}, projectChange("PUB", model.AccessGroupAdded, "crew", func(a model.ProjectAccess) bool { return slices.Equal(a.Groups, []string{"crew"}) })},
		{"alice is removed from PUB", func(tx pgx.Tx) ([]model.Event, error) {
			_, event, err := RemovePrincipal(ctx, tx, broker, "PUB", PrincipalPerson, alice, olgaActor)
			return []model.Event{event}, err
		}, projectChange("PUB", model.AccessPersonRemoved, alice, func(a model.ProjectAccess) bool { return len(a.People) == 0 && a.People != nil })},
		{"the group reviewers is made", func(tx pgx.Tx) ([]model.Event, error) {
			_, event, err := PutGroup(ctx, tx, broker, "reviewers", alice, []string{carol, dave, carol}, model.Actor{Kind: "user", ID: alice})
			return []model.Event{event}, err
		}, func(t *testing.T, appended []model.Event) {
			groupChange(t, appended, "reviewers", model.AccessGroupCreated)
			group, err := ReadGroup(ctx, database.Pool, "reviewers")
			if err != nil || group.Owner != alice || !slices.Equal(group.Members, []string{carol, dave}) {
				t.Fatalf("reviewers = %+v, %v, want owner alice and members carol, dave", group, err)
			}
		}},
		{"the group reviewers is written again", func(tx pgx.Tx) ([]model.Event, error) {
			_, event, err := PutGroup(ctx, tx, broker, "reviewers", alice, []string{dave}, model.Actor{Kind: "user", ID: alice})
			return []model.Event{event}, err
		}, func(t *testing.T, appended []model.Event) {
			groupChange(t, appended, "reviewers", model.AccessGroupUpdated)
			group, err := ReadGroup(ctx, database.Pool, "reviewers")
			if err != nil || !slices.Equal(group.Members, []string{dave}) {
				t.Fatalf("reviewers = %+v, %v, want the one member dave", group, err)
			}
		}},
		{"the group crew, which PUB and OTHER list, is deleted", func(tx pgx.Tx) ([]model.Event, error) {
			return DeleteGroup(ctx, tx, broker, "crew", olgaActor)
		}, func(t *testing.T, appended []model.Event) {
			if len(appended) != 3 {
				t.Fatalf("%d events appended, want one per list that named crew and the group's own", len(appended))
			}
			for index, project := range []string{"OTHER", "PUB"} {
				projectChange(project, model.AccessGroupRemoved, "crew", func(a model.ProjectAccess) bool { return len(a.Groups) == 0 })(t, appended[index:index+1])
			}
			groupChange(t, appended[2:], "crew", model.AccessGroupDeleted)
		}},
	} {
		t.Run(step.name, func(t *testing.T) {
			var appended []model.Event
			if err := inTx(t, database, func(tx pgx.Tx) error {
				var err error
				appended, err = step.write(tx)
				return err
			}); err != nil {
				t.Fatalf("write: %v", err)
			}
			step.checks(t, appended)
			for _, event := range appended {
				var stored string
				if err := database.Pool.QueryRow(ctx, `select type from events where id = $1`, event.ID).Scan(&stored); err != nil || stored != event.Type {
					t.Fatalf("event %d stored as %q, %v, want %s committed with the write", event.ID, stored, err, event.Type)
				}
			}
		})
	}
}

func groupChange(t *testing.T, appended []model.Event, name, change string) {
	t.Helper()
	if len(appended) != 1 {
		t.Fatalf("%d events appended, want one access_group.changed", len(appended))
	}
	payload, ok := appended[0].Payload.(model.AccessGroupEventPayload)
	if appended[0].Type != "access_group.changed" || !ok || payload.Name != name || payload.Change != change {
		t.Fatalf("appended %s %+v, want access_group.changed for %s, %s", appended[0].Type, appended[0].Payload, name, change)
	}
}

// Each write refuses what the rule cannot hold, and writes nothing when it does: an owner who is
// not a full member, a group member who is restricted, a group no row holds, a project no row
// holds, and a principal that is no email or no group name.
func TestAccessWritesRefuseWhatTheRuleCannotHold(t *testing.T) {
	ctx := context.Background()
	database := seedAccess(t)
	broker := events.NewBroker()
	by := model.Actor{Kind: "user", ID: olga}
	for _, refused := range []struct {
		name  string
		write func(tx pgx.Tx) error
		want  error
		names string
	}{
		{"an owner who is a restricted member", func(tx pgx.Tx) error {
			_, _, err := SetOwner(ctx, tx, broker, "PRIV", bob, by)
			return err
		}, ErrOwnerNotAllowed, bob},
		{"an owner nobody has signed in as", func(tx pgx.Tx) error {
			_, _, err := SetOwner(ctx, tx, broker, "PRIV", newbie, by)
			return err
		}, ErrOwnerNotAllowed, newbie},
		{"an unknown project", func(tx pgx.Tx) error {
			_, _, err := AddPrincipal(ctx, tx, broker, "NOPE", PrincipalPerson, alice, by)
			return err
		}, ErrProjectNotFound, "NOPE"},
		{"an unknown group on a list", func(tx pgx.Tx) error {
			_, _, err := AddPrincipal(ctx, tx, broker, "PUB", PrincipalGroup, "ghosts", by)
			return err
		}, ErrGroupNotFound, "ghosts"},
		{"a person who is no email", func(tx pgx.Tx) error {
			_, _, err := AddPrincipal(ctx, tx, broker, "PUB", PrincipalPerson, "not an email", by)
			return err
		}, ErrPrincipalInput, "not an email"},
		{"an email holding a bidi override", func(tx pgx.Tx) error {
			_, _, err := AddPrincipal(ctx, tx, broker, "PUB", PrincipalPerson, "alice\u202e@example.com", by)
			return err
		}, ErrPrincipalInput, "bidi"},
		{"an email holding a control character", func(tx pgx.Tx) error {
			_, _, err := AddPrincipal(ctx, tx, broker, "PUB", PrincipalPerson, "alice\x07@example.com", by)
			return err
		}, ErrPrincipalInput, "control"},
		// fold.ASCII lowers A-Z alone, and the people table holds only lower(email), so a capital
		// beyond A-Z names nobody who could sign in: it is refused as input, not left to the
		// column's check, which would answer 500.
		{"an email with a capital letter beyond A-Z", func(tx pgx.Tx) error {
			_, _, err := AddPrincipal(ctx, tx, broker, "PUB", PrincipalPerson, "\u00c4@example.com", by)
			return err
		}, ErrPrincipalInput, "lowercase"},
		{"a group member with a capital letter beyond A-Z", func(tx pgx.Tx) error {
			_, _, err := PutGroup(ctx, tx, broker, "accents", olga, []string{dave, "\u00c4@example.com"}, by)
			return err
		}, ErrPrincipalInput, "lowercase"},
		{"a group member who is restricted", func(tx pgx.Tx) error {
			_, _, err := PutGroup(ctx, tx, broker, "mixed", olga, []string{dave, bob}, by)
			return err
		}, ErrGroupMemberRestricted, bob},
		{"a visibility that is neither", func(tx pgx.Tx) error {
			_, _, err := SetVisibility(ctx, tx, broker, "PUB", "secret", by)
			return err
		}, ErrVisibilityInput, "secret"},
		{"a group no row holds, deleted", func(tx pgx.Tx) error {
			_, err := DeleteGroup(ctx, tx, broker, "ghosts", by)
			return err
		}, ErrGroupNotFound, "ghosts"},
	} {
		t.Run(refused.name, func(t *testing.T) {
			before := countEvents(t, database)
			err := inTx(t, database, refused.write)
			if !errors.Is(err, refused.want) || !strings.Contains(err.Error(), refused.names) {
				t.Fatalf("write = %v, want %v naming %q", err, refused.want, refused.names)
			}
			if after := countEvents(t, database); after != before {
				t.Fatalf("events moved from %d to %d under a refused write", before, after)
			}
		})
	}
	if group, err := ReadGroup(ctx, database.Pool, "mixed"); !errors.Is(err, ErrGroupNotFound) {
		t.Fatalf("the refused group was stored: %+v, %v", group, err)
	}
}

func countEvents(t *testing.T, database *store.Store) int {
	t.Helper()
	var count int
	if err := database.Pool.QueryRow(context.Background(), `select count(*) from events`).Scan(&count); err != nil {
		t.Fatalf("count events: %v", err)
	}
	return count
}

// MemberKind and the membership answer read a person as the people table holds them: a full or
// restricted member, and none for a login nobody has signed in as. The answer says whether the
// person sees the project.
func TestTheMembershipAnswerReadsTheRule(t *testing.T) {
	ctx := context.Background()
	database := seedAccess(t)
	for _, test := range []struct {
		person string
		kind   model.MemberKind
		answer string
		sees   bool
	}{
		{bob, model.MemberRestricted, model.MemberAnswerRestricted, true},
		{alice, model.MemberFull, model.MemberAnswerFull, false},
		{"Carol@Example.com", model.MemberFull, model.MemberAnswerFull, true},
		{newbie, model.MemberNone, model.MemberAnswerNone, false},
	} {
		kind, err := MemberKind(ctx, database.Pool, test.person)
		if err != nil || kind != test.kind {
			t.Errorf("MemberKind(%s) = %q, %v, want %q", test.person, kind, err, test.kind)
		}
		person, err := PersonAnswer(ctx, database.Pool, test.person)
		if err != nil || person.MemberKind != test.answer || person.Person != model.CanonicalLogin(test.person) {
			t.Errorf("PersonAnswer(%s) = %+v, %v, want member_kind %s", test.person, person, err, test.answer)
		}
		answer, err := Answer(ctx, database.Pool, "PRIV", test.person)
		if err != nil {
			t.Fatalf("Answer(PRIV, %s): %v", test.person, err)
		}
		want := model.AccessAnswer{
			Project: "PRIV", Visibility: model.ProjectPrivate, Owner: olga, Person: model.CanonicalLogin(test.person),
			MemberKind: test.answer, Visible: test.sees,
		}
		if answer.Project != want.Project || answer.Visibility != want.Visibility || answer.Owner != want.Owner || answer.Person != want.Person ||
			answer.MemberKind != want.MemberKind || answer.Visible != want.Visible {
			t.Errorf("Answer(PRIV, %s) = %+v, want %+v", test.person, answer, want)
		}
	}
	// A person with no people row reads as the broker reads them, a full member, so a public
	// project is visible to them.
	if answer, err := Answer(ctx, database.Pool, "PUB", newbie); err != nil || !answer.Visible || answer.MemberKind != model.MemberAnswerNone {
		t.Errorf("Answer(PUB, %s) = %+v, %v, want visible with member_kind none", newbie, answer, err)
	}
	if _, err := Answer(ctx, database.Pool, "NOPE", alice); !errors.Is(err, ErrProjectNotFound) {
		t.Errorf("Answer(NOPE) = %v, want ErrProjectNotFound", err)
	}
}
