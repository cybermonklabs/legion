package routes

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/logtest"
)

// signOutDown is the database failure the sign-out's failing stores answer with.
var signOutDown = &pgconn.PgError{Severity: "FATAL", Code: "57P01", Message: "the database is down"}

// failingSignOutPeople is the people store, with the one write or read failing names failing, its
// error quoting the address as the store's own do.
type failingSignOutPeople struct {
	*store.PgPeopleStore
	failing string
}

func (p failingSignOutPeople) Membership(ctx context.Context, email string) (auth.PersonMembership, bool, error) {
	if p.failing == "membership" {
		return auth.PersonMembership{}, false, fmt.Errorf("read membership of %q: %w", email, signOutDown)
	}
	return p.PgPeopleStore.Membership(ctx, email)
}

func (p failingSignOutPeople) KeepAfterSignOut(ctx context.Context, email string, signedInAt time.Time) error {
	if p.failing == "keep" {
		return fmt.Errorf("keep the refresh token of %q after its sign-out: %w", email, signOutDown)
	}
	return p.PgPeopleStore.KeepAfterSignOut(ctx, email, signedInAt)
}

func (p failingSignOutPeople) ForgetAfterSignOut(ctx context.Context, email string, signedInAt time.Time) error {
	if p.failing == "forget" {
		return fmt.Errorf("forget the refresh token of %q after its sign-out: %w", email, signOutDown)
	}
	return p.PgPeopleStore.ForgetAfterSignOut(ctx, email, signedInAt)
}

func (p failingSignOutPeople) End(ctx context.Context, email string) error {
	if p.failing == "end" {
		return fmt.Errorf("end membership of %q: %w", email, signOutDown)
	}
	return p.PgPeopleStore.End(ctx, email)
}

// failingRevokes is the session store whose revoke fails, quoting the login.
type failingRevokes struct{ *store.PgSessionStore }

func (failingRevokes) RevokeSessions(_ context.Context, login string) error {
	return fmt.Errorf("revoke the sessions of %q: %w", login, signOutDown)
}

// Each line a sign-out logs names the person by their login's hash and its failure by its kind:
// the read of the sign-in it ends, the revoke of the sessions, the broker's question, and each of
// the three writes of the refresh token. No line holds the address, in any letter case, or a word
// of the store's or the broker client's error, each of which quotes it; and the sign-out answers
// what it answered before: 500 for a failed read or revoke, 200 otherwise.
//
// Control: log "email", email or "error", err again on one of authLogout's lines, and its row
// fails on the address.
func TestASignOutsLinesNameThePersonByTheirLoginsHash(t *testing.T) {
	const person = "ada@example.com"
	brokerDown := func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"code":"INTERNAL","error":"boom"}`, http.StatusInternalServerError)
	}
	noAccess := personAccess(`{"person":"ada@example.com","machine_logins":0,"signins":0,"exchange_entries":0}`)
	google := identity.DomainRule{Domain: "example.com"}
	for _, row := range []struct {
		name    string
		rule    identity.SignInRule
		broker  http.HandlerFunc
		failing string
		revoke  bool
		status  int
		body    string
		message string
		kind    string
	}{
		{name: "the read of the sign-in", rule: google, failing: "membership", status: http.StatusInternalServerError, body: "read sign-in",
			message: "dispatch: read the sign-in a sign-out ends failed", kind: "database SQLSTATE 57P01"},
		{name: "the revoke of the sessions", rule: google, revoke: true, status: http.StatusInternalServerError, body: "revoke session",
			message: "dispatch: revoke sessions failed", kind: "database SQLSTATE 57P01"},
		{name: "the broker's question", rule: google, broker: brokerDown, status: http.StatusOK, body: `"ok":true`,
			message: "dispatch: a sign-out could not ask whether the person has access that outlives the browser", kind: "broker HTTP 500"},
		{name: "the write that keeps the token", rule: google, broker: brokerDown, failing: "keep", status: http.StatusOK, body: `"ok":true`,
			message: "dispatch: keep the signed-out person's refresh token failed", kind: "database SQLSTATE 57P01"},
		{name: "the write that forgets the token", rule: google, broker: noAccess, failing: "forget", status: http.StatusOK, body: `"ok":true`,
			message: "dispatch: forget the sign-in's refresh token failed", kind: "database SQLSTATE 57P01"},
		{name: "the end under the sign-in pool's rule", rule: identity.GroupRule{Group: rigGroup}, failing: "end", status: http.StatusOK, body: `"ok":true`,
			message: "dispatch: forget the sign-in's refresh token failed", kind: "database SQLSTATE 57P01"},
	} {
		t.Run(row.name, func(t *testing.T) {
			logged := &logtest.Buffer{}
			logtest.SetDefault(t, slog.NewJSONHandler(logged, nil))
			answers := map[string]http.HandlerFunc{}
			if row.broker != nil {
				answers[person] = row.broker
			}
			people := func(p *store.PgPeopleStore) auth.PeopleStore {
				return failingSignOutPeople{PgPeopleStore: p, failing: row.failing}
			}
			var sessions func(*store.PgSessionStore) auth.SessionStore
			if row.revoke {
				sessions = func(s *store.PgSessionStore) auth.SessionStore { return failingRevokes{s} }
			}
			rig := newSignOutRigOver(t, row.rule, answers, people, sessions)
			response := rig.post(rig.signIn(t, person, "refresh-ada"))
			if response.Code != row.status || !strings.Contains(response.Body.String(), row.body) {
				t.Errorf("POST /auth/logout answered %d %s, want %d with %s", response.Code, response.Body.String(), row.status, row.body)
			}
			lines := logged.Lines(row.message)
			if strings.Count(lines, "\n") != 1 {
				t.Fatalf("logged %q; want one line holding %q", logged.String(), row.message)
			}
			for _, want := range []string{`"level":"WARN"`, `"login_hash":"` + model.LoginHash(person) + `"`, `"error_kind":"` + row.kind + `"`} {
				if !strings.Contains(lines, want) {
					t.Errorf("the line %q does not hold %s", lines, want)
				}
			}
			// Every line names the person by their login's hash alone, the request's access-log line
			// among them (accesslog).
			all := logged.String()
			if strings.Contains(strings.ToLower(all), person) {
				t.Errorf("the log holds the address %s:\n%s", person, all)
			}
			for _, word := range []string{"of \\\"ada", "the database is down", "agent-secrets", "broker returned"} {
				if strings.Contains(all, word) {
					t.Errorf("the log holds %q, a word of the error's own text:\n%s", word, all)
				}
			}
		})
	}
}
