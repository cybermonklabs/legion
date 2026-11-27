package routes

import (
	"context"
	"crypto/sha256"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/logtest"
)

// signOutRig is a router under a sign-in rule, with the real people and session stores on a
// database of its own, whose Dispatch asks a broker stand-in for GET /v1/person-access (K8's
// shape): the stand-in answers each person with what the test gave it, and records whom it was
// asked about.
type signOutRig struct {
	handler  http.Handler
	database *store.Store
	people   *store.PgPeopleStore
	sessions *store.PgSessionStore
	rule     identity.SignInRule
	mu       sync.Mutex
	asked    []string
}

func newSignOutRig(t *testing.T, rule identity.SignInRule, answers map[string]http.HandlerFunc) *signOutRig {
	t.Helper()
	database := storetest.Open(t)
	rig := &signOutRig{
		database: database,
		people:   store.NewPgPeopleStore(database.Pool, testSigningKey, nil),
		sessions: store.NewPgSessionStore(database.Pool),
		rule:     rule,
	}
	broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		person := r.URL.Query().Get("person")
		rig.mu.Lock()
		rig.asked = append(rig.asked, person)
		rig.mu.Unlock()
		if r.Method != http.MethodGet || r.URL.Path != "/v1/person-access" || r.Header.Get("Authorization") != "Bearer dispatch-account" {
			t.Errorf("the broker stand-in was called %s %s under %q, want GET /v1/person-access under Dispatch's own token", r.Method, r.URL.RequestURI(), r.Header.Get("Authorization"))
		}
		answer, found := answers[person]
		if !found {
			t.Errorf("the broker was asked about %s, whom the test gave no answer", person)
			http.Error(w, "no answer", http.StatusTeapot)
			return
		}
		answer(w, r)
	}))
	t.Cleanup(broker.Close)
	accountFile := filepath.Join(t.TempDir(), "account-token")
	if err := os.WriteFile(accountFile, []byte("dispatch-account"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, err := BuildAppContext(withDocuments(t, AppContextOptions{
		SigningKey:            testSigningKey,
		People:                rig.people,
		Sessions:              rig.sessions,
		Identity:              identity.CookieIdentity{SigningKey: testSigningKey, Sessions: rig.sessions},
		SignInRule:            rule,
		Store:                 database,
		AgentSecretsURL:       broker.URL,
		AgentSecretsTokenFile: accountFile,
		Lifetime:              t.Context(),
	}))
	if err != nil {
		t.Fatalf("build context: %v", err)
	}
	handler, background := New(ctx)
	t.Cleanup(background.Wait)
	rig.handler = handler
	return rig
}

// signIn records email's sign-in with refreshToken under the rig's rule, as the sign-in callback
// records it, and answers the session cookie it issues.
func (rig *signOutRig) signIn(t *testing.T, email, refreshToken string) *http.Cookie {
	t.Helper()
	ctx := context.Background()
	var err error
	if rig.rule.Holds() {
		err = rig.people.SignInSubject(ctx, email, "sub-"+email, refreshToken, "example.com", model.MemberFull)
	} else {
		err = rig.people.SignIn(ctx, email, refreshToken)
	}
	if err != nil {
		t.Fatalf("sign %s in: %v", email, err)
	}
	generation, err := rig.sessions.EnsureSession(ctx, email)
	if err != nil {
		t.Fatalf("session generation of %s: %v", email, err)
	}
	cookie, err := http.ParseSetCookie(auth.IssueSessionCookie(email, generation, testSigningKey, true))
	if err != nil {
		t.Fatal(err)
	}
	return cookie
}

// personalToken stores an unrevoked dsp_ token of owner.
func (rig *signOutRig) personalToken(t *testing.T, owner string) {
	t.Helper()
	hash := sha256.Sum256([]byte("dsp_" + owner))
	if _, err := rig.database.Pool.Exec(context.Background(), `insert into agent_tokens (owner, name, token_hash, prefix) values ($1, 'agent', $2, 'test')`, owner, hash[:]); err != nil {
		t.Fatalf("store a personal token of %s: %v", owner, err)
	}
}

// post posts /auth/logout with cookie, as the dashboard's Sign out does.
func (rig *signOutRig) post(cookie *http.Cookie) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, "http://dispatch.test/auth/logout", nil)
	request.Header.Set("Origin", "http://dispatch.test")
	request.AddCookie(cookie)
	response := httptest.NewRecorder()
	rig.handler.ServeHTTP(response, request)
	return response
}

// signOut signs email out with cookie and wants 200.
func (rig *signOutRig) signOut(t *testing.T, email string, cookie *http.Cookie) {
	t.Helper()
	if response := rig.post(cookie); response.Code != http.StatusOK {
		t.Fatalf("POST /auth/logout as %s answered %d %s, want 200", email, response.Code, response.Body.String())
	}
}

// expect checks email's row after what: holding refreshToken ("" for none), and signed out or not.
func (rig *signOutRig) expect(t *testing.T, what, email, refreshToken string, signedOut bool) {
	t.Helper()
	membership, _, err := rig.people.Membership(context.Background(), email)
	if err != nil {
		t.Fatalf("read %s: %v", email, err)
	}
	if membership.RefreshToken != refreshToken || membership.SignedOut != signedOut {
		t.Errorf("%s: %s's row holds refresh token %q, signed out %v; want %q, signed out %v", what, email, membership.RefreshToken, membership.SignedOut, refreshToken, signedOut)
	}
}

// takeAsked answers whom the broker was asked about since the last take.
func (rig *signOutRig) takeAsked() []string {
	rig.mu.Lock()
	defer rig.mu.Unlock()
	asked := rig.asked
	rig.asked = nil
	return asked
}

func personAccess(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}
}

// warned answers whether logged holds a WARN line naming email.
func warned(logged *logtest.Buffer, email string) bool {
	for line := range strings.Lines(logged.Lines(email)) {
		if strings.Contains(line, `"level":"WARN"`) {
			return true
		}
	}
	return false
}

// Under a Google rule a sign-out keeps the refresh token, for membership checks, of a person whose
// access outlives the browser, and forgets everyone else's (c-sign-out, Dispatch's half). Ada,
// holding a dsp_ token, keeps hers, and the broker is not asked. Bob, holding nothing, has his
// forgotten. Carol, holding a machine login, keeps hers. A broker answering 500 or 404 cannot be
// asked, so erin and frank keep theirs, and the sign-out logs a WARN naming the person. A sign-in
// that commits while the broker delays its answer keeps its own token, not signed out, whether the
// answer would have forgotten the old one (dave) or kept it (gail). Under the sign-in pool's rule,
// hana's token is forgotten as before, though she holds a dsp_ token, and the broker is not asked.
func TestSignOutKeepsTheTokenOfAPersonWhoseAgentsStillAct(t *testing.T) {
	logged := &logtest.Buffer{}
	logtest.SetDefault(t, slog.NewJSONHandler(logged, nil))
	// The broker delays its answer about dave and gail until the test has signed each in again, or
	// until the sign-out's ask is given up.
	held := map[string]chan struct{}{"dave@example.com": make(chan struct{}), "gail@example.com": make(chan struct{})}
	asked := map[string]chan struct{}{"dave@example.com": make(chan struct{}), "gail@example.com": make(chan struct{})}
	delayed := func(email, body string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			close(asked[email])
			select {
			case <-held[email]:
				personAccess(body)(w, r)
			case <-r.Context().Done():
			}
		}
	}
	rig := newSignOutRig(t, identity.DomainRule{Domain: "example.com"}, map[string]http.HandlerFunc{
		"bob@example.com":   personAccess(`{"person":"bob@example.com","machine_logins":0,"signins":0,"exchange_entries":0}`),
		"carol@example.com": personAccess(`{"person":"carol@example.com","machine_logins":1,"signins":0,"exchange_entries":0}`),
		"erin@example.com": func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, `{"code":"INTERNAL","error":"boom"}`, http.StatusInternalServerError)
		},
		"frank@example.com": http.NotFound,
		"dave@example.com":  delayed("dave@example.com", `{"person":"dave@example.com","machine_logins":0,"signins":0,"exchange_entries":0}`),
		"gail@example.com":  delayed("gail@example.com", `{"person":"gail@example.com","machine_logins":1,"signins":0,"exchange_entries":0}`),
	})

	ada := rig.signIn(t, "ada@example.com", "refresh-ada")
	rig.personalToken(t, "ada@example.com")
	rig.signOut(t, "ada@example.com", ada)
	rig.expect(t, "ada signed out holding a dsp_ token", "ada@example.com", "refresh-ada", true)
	if got := rig.takeAsked(); len(got) != 0 {
		t.Errorf("ada's sign-out asked the broker about %q, want no ask: her dsp_ token answers", got)
	}

	bob := rig.signIn(t, "bob@example.com", "refresh-bob")
	rig.signOut(t, "bob@example.com", bob)
	rig.expect(t, "bob signed out holding nothing", "bob@example.com", "", false)

	carol := rig.signIn(t, "carol@example.com", "refresh-carol")
	rig.signOut(t, "carol@example.com", carol)
	rig.expect(t, "carol signed out holding a machine login", "carol@example.com", "refresh-carol", true)

	for _, email := range []string{"erin@example.com", "frank@example.com"} {
		cookie := rig.signIn(t, email, "refresh-"+email)
		rig.signOut(t, email, cookie)
		rig.expect(t, email+" signed out while the broker could not be asked", email, "refresh-"+email, true)
		if !warned(logged, email) {
			t.Errorf("%s's sign-out logged no WARN naming them while the broker could not be asked; the log:\n%s", email, logged.String())
		}
	}
	if got, want := rig.takeAsked(), []string{"bob@example.com", "carol@example.com", "erin@example.com", "frank@example.com"}; !slices.Equal(got, want) {
		t.Errorf("the broker was asked about %q, want %q", got, want)
	}

	for _, email := range []string{"dave@example.com", "gail@example.com"} {
		cookie := rig.signIn(t, email, "refresh-"+email)
		answered := make(chan *httptest.ResponseRecorder, 1)
		go func() { answered <- rig.post(cookie) }()
		select {
		case <-asked[email]:
		case response := <-answered:
			t.Fatalf("%s's sign-out answered %d %s without asking the broker, want it to ask", email, response.Code, response.Body.String())
		}
		rig.signIn(t, email, "refresh-again-"+email)
		close(held[email])
		if response := <-answered; response.Code != http.StatusOK {
			t.Fatalf("POST /auth/logout as %s answered %d %s, want 200", email, response.Code, response.Body.String())
		}
		rig.expect(t, email+" signed in again while the broker delayed its answer to the sign-out", email, "refresh-again-"+email, false)
	}

	pool := newSignOutRig(t, identity.GroupRule{Group: rigGroup}, nil)
	hana := pool.signIn(t, "hana@example.com", "refresh-hana")
	pool.personalToken(t, "hana@example.com")
	pool.signOut(t, "hana@example.com", hana)
	pool.expect(t, "hana signed out of the sign-in pool holding a dsp_ token", "hana@example.com", "", false)
	if got := pool.takeAsked(); len(got) != 0 {
		t.Errorf("a sign-out under the sign-in pool's rule asked the broker about %q, want no ask", got)
	}
}
