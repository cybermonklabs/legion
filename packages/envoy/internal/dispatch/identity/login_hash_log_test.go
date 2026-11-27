package identity

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/cipher"
	"github.com/sjawhar/envoy/internal/dispatch/identity/directorytest"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/logtest"
)

// endsNothing is testPeopleStore whose EndSubject finds the row signed in to again since the
// confirmation read it, and so ends nothing.
type endsNothing struct{ *testPeopleStore }

func (endsNothing) EndSubject(context.Context, string, string, string, auth.EndMode) (bool, error) {
	return false, nil
}

// someoneElse is the address a refreshed sign-in names in place of the person's own.
const someoneElse = "someone-else@d.example"

// Each line an hourly confirmation logs names the person by their login's hash, and its reason
// names a refreshed sign-in's address by its hash too: an end Google refused, an end that ended
// nothing, a refreshed sign-in of another address under the domain rule and under a sign-in pool,
// one the rule refuses under either, and an issuer that could not answer. No line holds the
// person's address, in any letter case, or the address a refreshed sign-in named.
//
// Control: log "email", email again in endSubject or Membership.refresh, or a reason of the
// refreshed sign-in's address as written (fmt's %q of login, person.Email, or err.Error() of the
// rule's refusal), and the rows that reach that line fail on the address.
func TestAConfirmationsLinesNameThePersonByTheirLoginsHash(t *testing.T) {
	domainEnded := "dispatch: sign-in ended: Google did not confirm the person's membership of the domain"
	poolEnded := "dispatch: sign-in ended: the sign-in pool did not confirm the person's membership"
	for _, row := range []struct {
		name    string
		rig     func(t *testing.T) *membershipRig
		change  func(rig *membershipRig)
		message string
		// reason is what the line's reason must hold; "" for a line with none.
		reason string
	}{
		{"an end Google refused", newDomainMembershipRig, func(rig *membershipRig) { rig.issuer.RevokeGrants() },
			domainEnded, "invalid_grant"},
		{"an end that ended nothing", newDomainMembershipRig, func(rig *membershipRig) {
			rig.issuer.RevokeGrants()
			rig.membership.People = endsNothing{rig.people}
		}, "dispatch: a failed confirmation ended nothing", "invalid_grant"},
		{"a refreshed sign-in of another address under the domain rule", newDomainMembershipRig, func(rig *membershipRig) {
			rig.issuer.UpdateGrants(func(c map[string]any) { c["email"] = someoneElse })
		}, domainEnded, "the refreshed sign-in is login_hash " + model.LoginHash(someoneElse)},
		{"a refreshed sign-in the domain rule refuses", newDomainMembershipRig, func(rig *membershipRig) {
			rig.issuer.UpdateGrants(func(c map[string]any) { c["hd"] = "other.example" })
		}, domainEnded, "not in the domain that may use Dispatch (login_hash " + model.LoginHash(memberEmail) + `), hd \"other.example\"`},
		{"a refreshed sign-in of another address under a sign-in pool", newMembershipRig, func(rig *membershipRig) {
			rig.issuer.UpdateGrants(func(c map[string]any) { c["cognito:username"] = "ExampleIdP_" + someoneElse })
		}, poolEnded, "the refreshed sign-in names login_hash " + model.LoginHash(someoneElse)},
		{"a refreshed sign-in the pool's rule refuses", newMembershipRig, func(rig *membershipRig) {
			rig.issuer.UpdateGrants(func(c map[string]any) { c["cognito:groups"] = []string{} })
		}, poolEnded, "not a member of the group that may use Dispatch (login_hash " + model.LoginHash(memberEmail) + ")"},
		{"an issuer that could not answer", newDomainMembershipRig, func(rig *membershipRig) { rig.issuer.FailToken(500, "server_error") },
			"dispatch: the sign-in issuer could not confirm a lapsed membership", ""},
	} {
		t.Run(row.name, func(t *testing.T) {
			var logs logtest.Buffer
			logtest.SetDefault(t, slog.NewTextHandler(&logs, nil))
			rig := row.rig(t)
			row.change(rig)
			rig.now = rig.now.Add(2 * time.Hour)
			if _, err := rig.membership.ConfirmFor(context.Background(), memberEmail); err == nil {
				t.Fatalf("ConfirmFor answered no error; want the confirmation refused")
			}
			line := loggedLine(t, &logs, row.message)
			if want := "login_hash=" + model.LoginHash(memberEmail); !strings.Contains(line, want) {
				t.Errorf("the line %q does not hold %s", line, want)
			}
			if row.reason != "" && !strings.Contains(line, row.reason) {
				t.Errorf("the line %q does not hold the reason %q", line, row.reason)
			}
			holdsNone(t, logs.String(), memberEmail)
			holdsNone(t, logs.String(), someoneElse)
		})
	}
}

// directoryWrites is the people store a Directory check records its outcome in, with each write
// failing as fail says, quoting the address as the store's own errors do, or, for active, writing
// nothing as a write that finds a later hold does.
type directoryWrites struct {
	*store.PgPeopleStore
	fail            error
	failing         string
	activeUnwritten bool
}

func (s directoryWrites) RecordDirectoryActive(ctx context.Context, email, userID string, readAt time.Time) (time.Time, error) {
	switch {
	case s.failing == "active":
		return time.Time{}, fmt.Errorf("record an active Directory answer for %q: %w", email, s.fail)
	case s.activeUnwritten:
		return time.Time{}, nil
	}
	return s.PgPeopleStore.RecordDirectoryActive(ctx, email, userID, readAt)
}

func (s directoryWrites) RecordDirectoryRebound(ctx context.Context, email, userID string, readAt time.Time) (time.Time, error) {
	if s.failing == "rebound" {
		return time.Time{}, fmt.Errorf("record an active Directory answer of a new account for %q: %w", email, s.fail)
	}
	return s.PgPeopleStore.RecordDirectoryRebound(ctx, email, userID, readAt)
}

func (s directoryWrites) RecordDirectoryHeld(ctx context.Context, email string) error {
	if s.failing == "held" {
		return fmt.Errorf("record a Held Directory answer for %q: %w", email, s.fail)
	}
	return s.PgPeopleStore.RecordDirectoryHeld(ctx, email)
}

// peopleOnly is a people store that records no Directory outcome.
type peopleOnly struct{ auth.PeopleStore }

// Each line a Workspace Directory check logs names the person by their login's hash, and each
// failed write or failed question by its kind: no Directory people, an Active answer whose write
// fails or writes nothing, a Held answer whose write fails or is recorded, a Failed check, and an
// answer for the address deleted and created again whose question fails, whose write fails, or
// which confirms the new account. No line holds the address, in any letter case, or a word of the
// store's or the broker client's error, each of which quotes the address.
//
// Control: log "email", email or "error", err again on one of checkDirectory's lines, and its row
// fails on the address.
func TestADirectoryChecksLinesNameThePersonByTheirLoginsHash(t *testing.T) {
	down := &pgconn.PgError{Severity: "FATAL", Code: "57P01", Message: "the database is down"}
	suspended := directorytest.User{ID: "u-ada", PrimaryEmail: directoryAda, Suspended: true}
	for _, row := range []struct {
		name string
		// recreated starts from ada's row confirmed by u-ada, the Directory answering her
		// address under u-ada-2; otherwise from a row with no Directory answer.
		recreated bool
		answer    func(*directorytest.Google)
		people    func(*store.PgPeopleStore) auth.PeopleStore
		outliving func(*store.Store) cipher.OutlivingAccess
		message   string
		kind      string
		words     []string
	}{
		{name: "no Directory people", answer: adaActive,
			people:  func(p *store.PgPeopleStore) auth.PeopleStore { return peopleOnly{p} },
			message: "dispatch: a Workspace Directory check cannot record its outcome"},
		{name: "an Active answer whose write fails", answer: adaActive,
			people: func(p *store.PgPeopleStore) auth.PeopleStore {
				return directoryWrites{PgPeopleStore: p, fail: down, failing: "active"}
			},
			message: "dispatch: a Workspace Directory check answered Active, and its write failed", kind: "database SQLSTATE 57P01",
			words: []string{"record an active", "the database is down"}},
		{name: "an Active answer that writes nothing", answer: adaActive,
			people: func(p *store.PgPeopleStore) auth.PeopleStore {
				return directoryWrites{PgPeopleStore: p, activeUnwritten: true}
			},
			message: "dispatch: a Workspace Directory check answered Active, and wrote nothing"},
		{name: "a Held answer whose write fails", answer: func(g *directorytest.Google) { g.SetUser(directoryAda, suspended) },
			people: func(p *store.PgPeopleStore) auth.PeopleStore {
				return directoryWrites{PgPeopleStore: p, fail: down, failing: "held"}
			},
			message: "dispatch: a Workspace Directory check answered Held, and its write failed", kind: "database SQLSTATE 57P01",
			words: []string{"record a Held", "the database is down"}},
		{name: "a Held answer recorded", answer: func(g *directorytest.Google) { g.SetUser(directoryAda, suspended) },
			message: "their credentials are held"},
		{name: "a Failed check", answer: directoryRefusal(500),
			message: "dispatch: a Workspace Directory check failed"},
		{name: "a recreated address whose question fails", recreated: true,
			outliving: func(*store.Store) cipher.OutlivingAccess { return cipher.OutlivingAccess{} },
			message:   "while the address may still hold the earlier person's access", kind: "*errors.errorString",
			words: []string{"holds a personal token", "no database"}},
		{name: "a recreated address whose write fails", recreated: true,
			people: func(p *store.PgPeopleStore) auth.PeopleStore {
				return directoryWrites{PgPeopleStore: p, fail: down, failing: "rebound"}
			},
			message: "dispatch: the Workspace Directory answered a recreated address's new account, and its write failed", kind: "database SQLSTATE 57P01",
			words: []string{"of a new account", "the database is down"}},
		{name: "a recreated address confirmed", recreated: true,
			message: "the new account is confirmed"},
	} {
		t.Run(row.name, func(t *testing.T) {
			var logs logtest.Buffer
			logtest.SetDefault(t, slog.NewTextHandler(&logs, nil))
			r := newDirectoryStandingRig(t)
			if row.outliving != nil {
				r.outliving = row.outliving(r.database)
			}
			r.newProcess()
			r.record(directoryAda)
			if row.recreated {
				r.active(directoryAda, "u-ada", 2*time.Hour)
				r.google.SetUser(directoryAda, directorytest.User{ID: "u-ada-2", PrimaryEmail: directoryAda})
			} else {
				row.answer(r.google)
			}
			if row.people != nil {
				r.membership.People = row.people(r.people)
			}
			// The check's outcome is each row's own; what is read here is the line it logged.
			_, _ = r.membership.ConfirmDirectory(context.Background(), directoryAda)
			line := loggedLine(t, &logs, row.message)
			if want := "login_hash=" + model.LoginHash(directoryAda); !strings.Contains(line, want) {
				t.Errorf("the line %q does not hold %s", line, want)
			}
			if row.kind != "" && !strings.Contains(line, fmt.Sprintf("error_kind=%q", row.kind)) && !strings.Contains(line, "error_kind="+row.kind) {
				t.Errorf("the line %q does not hold error_kind %s", line, row.kind)
			}
			holdsNone(t, logs.String(), directoryAda, row.words...)
		})
	}
}
