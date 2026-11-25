package routes

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/logtest"
)

// signInDown is the database failure the sign-in's failing stores answer with.
var signInDown = &pgconn.PgError{Severity: "FATAL", Code: "57P01", Message: "the database is down"}

// failingSignInPeople is the rig's people store with the one call failing names failing, its
// error quoting the address as the store's own do; "superseded" reads the row as another
// account's.
type failingSignInPeople struct {
	*memoryPeopleStore
	failing string
}

func (p failingSignInPeople) SignIn(ctx context.Context, email, refreshToken string) error {
	if p.failing == "signin" {
		return fmt.Errorf("record sign-in of %q: %w", email, signInDown)
	}
	return p.memoryPeopleStore.SignIn(ctx, email, refreshToken)
}

func (p failingSignInPeople) Record(ctx context.Context, email string) error {
	if p.failing == "record" {
		return fmt.Errorf("record %q: %w", email, signInDown)
	}
	return p.memoryPeopleStore.Record(ctx, email)
}

func (p failingSignInPeople) Membership(ctx context.Context, email string) (auth.PersonMembership, bool, error) {
	switch p.failing {
	case "read":
		return auth.PersonMembership{}, false, fmt.Errorf("read membership of %q: %w", email, signInDown)
	case "superseded":
		return auth.PersonMembership{Subject: "another-google-account"}, true, nil
	}
	return p.memoryPeopleStore.Membership(ctx, email)
}

// failingEnsure is the rig's session store whose EnsureSession fails, quoting the login.
type failingEnsure struct{ *memorySessionStore }

func (failingEnsure) EnsureSession(_ context.Context, login string) (int64, error) {
	return 0, fmt.Errorf("ensure session for %q: %w", login, signInDown)
}

// overStores rebuilds rig's router over its people store as people wraps it and its session store
// as sessions wraps it, either nil for the rig's own.
func overStores(t *testing.T, rig *signInRig, people func(*memoryPeopleStore) auth.PeopleStore, sessions func(*memorySessionStore) auth.SessionStore) {
	t.Helper()
	options := rig.options
	if people != nil {
		options.People = people(rig.people)
	}
	if sessions != nil {
		options.Sessions = sessions(rig.sessions)
	}
	ctx, err := BuildAppContext(withDocuments(t, options))
	if err != nil {
		t.Fatalf("build context: %v", err)
	}
	standInEventLog(ctx, 0)
	handler, background := New(ctx)
	t.Cleanup(background.Wait)
	rig.handler = handler
}

// Each line a sign-in logs names the person by their login's hash and its failure by its kind: the
// write of the sign-in, the session generation, the account a sign-in recorded read again, that
// account superseded, and a dev sign-in's record of the person; and a passkey ceremony's failure,
// which names nobody, by its kind. Each failure's error quotes the address, and no line, the access
// log's among them, holds it; each answer is what it was.
//
// Control: log "email", email or "error", err again on one of those lines, and its row fails on the
// address.
func TestASignInsLinesNameThePersonByTheirLoginsHash(t *testing.T) {
	const googlePerson = "a.person@example.com"
	pool := func(t *testing.T) *signInRig { return newSignInRig(t, "") }
	for _, row := range []struct {
		name    string
		rig     func(t *testing.T) *signInRig
		claims  map[string]any
		people  string
		ensure  bool
		person  string
		status  int
		message string
		kind    string
	}{
		{name: "the write of the sign-in", rig: pool, claims: personClaims(rigGroup), people: "signin", person: rigEmail,
			status: http.StatusInternalServerError, message: "dispatch: record sign-in failed", kind: "database SQLSTATE 57P01"},
		{name: "the session generation", rig: pool, claims: personClaims(rigGroup), ensure: true, person: rigEmail,
			status: http.StatusInternalServerError, message: "dispatch: ensure session generation failed", kind: "database SQLSTATE 57P01"},
		{name: "the account the sign-in recorded, read again", rig: newDomainSignInRig, claims: googleClaims("google-subject-1"), people: "read", person: googlePerson,
			status: http.StatusInternalServerError, message: "dispatch: read the account the sign-in recorded failed", kind: "database SQLSTATE 57P01"},
		{name: "that account superseded", rig: newDomainSignInRig, claims: googleClaims("google-subject-1"), people: "superseded", person: googlePerson,
			status: http.StatusConflict, message: "dispatch: sign-in superseded"},
	} {
		t.Run(row.name, func(t *testing.T) {
			rig := row.rig(t)
			var people func(*memoryPeopleStore) auth.PeopleStore
			if row.people != "" {
				people = func(p *memoryPeopleStore) auth.PeopleStore {
					return failingSignInPeople{memoryPeopleStore: p, failing: row.people}
				}
			}
			var sessions func(*memorySessionStore) auth.SessionStore
			if row.ensure {
				sessions = func(s *memorySessionStore) auth.SessionStore { return failingEnsure{s} }
			}
			overStores(t, rig, people, sessions)
			logs := &logtest.Buffer{}
			logtest.SetDefault(t, slog.NewTextHandler(logs, nil))
			if response := rig.signIn(t, row.claims); response.Code != row.status {
				t.Fatalf("the callback answered %d %s, want %d", response.Code, response.Body.String(), row.status)
			}
			signInLineHolds(t, logs, row.message, row.person, row.kind)
		})
	}

	t.Run("a dev sign-in's record of the person", func(t *testing.T) {
		sessions := &memorySessionStore{generations: map[string]int64{}}
		ctx, err := BuildAppContext(withDocuments(t, AppContextOptions{
			SigningKey:  testSigningKey,
			People:      failingSignInPeople{memoryPeopleStore: newMemoryPeople(), failing: "record"},
			Sessions:    sessions,
			Identity:    identity.CookieIdentity{SigningKey: testSigningKey, Sessions: sessions},
			AgentTokens: sharedAgentTokens(t, "e2e-token"),
			ServerURL:   devSignInOrigin,
			DevSignIn:   true,
			Lifetime:    t.Context(),
		}))
		if err != nil {
			t.Fatalf("build context: %v", err)
		}
		standInEventLog(ctx, 0)
		handler, background := New(ctx)
		t.Cleanup(background.Wait)
		logs := &logtest.Buffer{}
		logtest.SetDefault(t, slog.NewTextHandler(logs, nil))
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, devSignInRequest(t, devSignInOrigin+"/auth/_dev/signin?login=alice@d.example&next=/issues/CORE-1"))
		if response.Code != http.StatusInternalServerError {
			t.Fatalf("the dev sign-in answered %d %s, want 500", response.Code, response.Body.String())
		}
		signInLineHolds(t, logs, "dispatch: dev sign-in could not record the person", "alice@d.example", "database SQLSTATE 57P01")
	})

	t.Run("a passkey ceremony's failure", func(t *testing.T) {
		logs := &logtest.Buffer{}
		logtest.SetDefault(t, slog.NewTextHandler(logs, nil))
		response := httptest.NewRecorder()
		writePasskeyRefusal(response, fmt.Errorf("confirm the passkey sign-in of %q: %w", outsiderLogin, signInDown), false)
		if response.Code != http.StatusInternalServerError || !strings.Contains(response.Body.String(), "PASSKEY_ERROR") {
			t.Fatalf("the ceremony's failure answered %d %s, want 500 PASSKEY_ERROR", response.Code, response.Body.String())
		}
		line := strings.TrimSpace(logs.Lines("dispatch: a passkey ceremony failed"))
		if !strings.Contains(line, `error_kind="database SQLSTATE 57P01"`) {
			t.Errorf("the line %q does not hold the failure's kind", line)
		}
		noAddressIn(t, logs.String(), outsiderLogin)
	})
}

// outsiderLogin is a restricted member's address a passkey ceremony names.
const outsiderLogin = "erin@other.example"

// signInLineHolds checks the one line of logs holding message for person's login hash and, for a
// failure, its kind, and the whole log for person's address and the failure's words.
func signInLineHolds(t *testing.T, logs *logtest.Buffer, message, person, kind string) {
	t.Helper()
	lines := logs.Lines(message)
	if strings.Count(lines, "\n") != 1 {
		t.Fatalf("logged %q; want one line holding %q", logs.String(), message)
	}
	if want := "login_hash=" + model.LoginHash(person); !strings.Contains(lines, want) {
		t.Errorf("the line %q does not hold %s", lines, want)
	}
	if kind != "" && !strings.Contains(lines, fmt.Sprintf("error_kind=%q", kind)) {
		t.Errorf("the line %q does not hold error_kind %q", lines, kind)
	}
	noAddressIn(t, logs.String(), person)
}

// noAddressIn fails t when logged holds email in any letter case, or a word of the failing stores'
// errors.
func noAddressIn(t *testing.T, logged, email string) {
	t.Helper()
	if strings.Contains(strings.ToLower(logged), strings.ToLower(email)) {
		t.Errorf("the log holds the address %s:\n%s", email, logged)
	}
	for _, word := range []string{"the database is down", "record sign-in of", "ensure session for", "read membership of"} {
		if strings.Contains(logged, word) {
			t.Errorf("the log holds %q, a word of the error's own text:\n%s", word, logged)
		}
	}
}
