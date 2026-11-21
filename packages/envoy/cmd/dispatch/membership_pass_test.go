package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/wire"
	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/cipher"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/oidc"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

// The membership pass (identity.MembershipPass), the last work of each maintenance sweep under a
// Google rule: it checks every stored refresh token about once an hour, so Google's refusal holds a
// person whether or not they make a request. Each test answers the broker's GET /v1/person-access
// with a stand-in in K8's shape (passBroker), the one broker call the pass makes.

// passBroker stands in for the secrets broker's GET /v1/person-access: it answers each person with
// one live machine login when holds names them and with none otherwise, after wait, or never while
// never is set (the ask is then cut by the pass's own bound), and records each ask. A person
// before names has that done first, once each ask of them arrives and before it is answered.
type passBroker struct {
	mu     sync.Mutex
	holds  map[string]bool
	before map[string]func()
	wait   time.Duration
	never  bool
	asks   []passBrokerAsk
}

// passBrokerAsk is one ask of the broker: the person it named, when it arrived, and when it was
// answered or cut.
type passBrokerAsk struct {
	person      string
	asked, done time.Time
}

func newPassBroker() *passBroker {
	return &passBroker{holds: map[string]bool{}, before: map[string]func(){}}
}

// beforeAnswering runs do as each later ask of person arrives, before the ask is answered.
func (b *passBroker) beforeAnswering(person string, do func()) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.before[person] = do
}

// keep makes the broker answer that each of emails has access that outlives the browser.
func (b *passBroker) keep(emails ...string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, email := range emails {
		b.holds[email] = true
	}
}

// answerNever makes every later ask wait until the asker cuts it.
func (b *passBroker) answerNever() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.never = true
}

func (b *passBroker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet || r.URL.Path != "/v1/person-access" {
		http.NotFound(w, r)
		return
	}
	person := r.URL.Query().Get("person")
	b.mu.Lock()
	index := len(b.asks)
	b.asks = append(b.asks, passBrokerAsk{person: person, asked: time.Now()})
	holds, wait, never, before := b.holds[person], b.wait, b.never, b.before[person]
	b.mu.Unlock()
	if before != nil {
		before()
	}
	defer func() {
		b.mu.Lock()
		b.asks[index].done = time.Now()
		b.mu.Unlock()
	}()
	if never {
		<-r.Context().Done()
		return
	}
	select {
	case <-time.After(wait):
	case <-r.Context().Done():
		return
	}
	answer := wire.PersonAccessResponse{Person: person}
	if holds {
		answer.MachineLogins = 1
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(answer)
}

// take is every ask since the last take, in order.
func (b *passBroker) take() []passBrokerAsk {
	b.mu.Lock()
	defer b.mu.Unlock()
	asks := b.asks
	b.asks = nil
	return asks
}

// persons is whom asks named, in order.
func persons(asks []passBrokerAsk) []string {
	named := make([]string, len(asks))
	for i, ask := range asks {
		named[i] = ask.person
	}
	return named
}

// withPersonAccess serves the leaver rig's Dispatch's GET /v1/person-access from broker, ahead of
// the rig's broker, and every other call from the rig's broker.
func withPersonAccess(broker *passBroker) leaverOption {
	return func(config *leaverConfig) {
		config.toBroker = append(config.toBroker, func(_ *testing.T, _ *leaverRig, next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/person-access" {
					broker.ServeHTTP(w, r)
					return
				}
				next.ServeHTTP(w, r)
			})
		})
	}
}

// passRefresher stands in for Google's token endpoint behind identity.Membership: each refresh
// fails as Google unreached does, at once, or, while never is set, when its own bound
// (identity's 15-second refresh bound) cuts it. It records which person each refresh was for, read
// from the refresh token passFixture stores ("refresh-<email>"), and when it started.
type passRefresher struct {
	mu     sync.Mutex
	never  bool
	starts []passRefresh
}

// passRefresh is one refresh the stand-in was asked for.
type passRefresh struct {
	email string
	at    time.Time
}

func (r *passRefresher) Refresh(ctx context.Context, refreshToken string) (oidc.Session, error) {
	r.mu.Lock()
	r.starts = append(r.starts, passRefresh{email: strings.TrimPrefix(refreshToken, "refresh-"), at: time.Now()})
	never := r.never
	r.mu.Unlock()
	if never {
		<-ctx.Done()
		return oidc.Session{}, fmt.Errorf("%w: the token endpoint never answered: %w", oidc.ErrIssuerUnavailable, ctx.Err())
	}
	return oidc.Session{}, fmt.Errorf("%w: the token endpoint answered 503", oidc.ErrIssuerUnavailable)
}

// answerNever makes every later refresh wait until its bound cuts it.
func (r *passRefresher) answerNever() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.never = true
}

// take is every refresh since the last take, in order.
func (r *passRefresher) take() []passRefresh {
	r.mu.Lock()
	defer r.mu.Unlock()
	starts := r.starts
	r.starts = nil
	return starts
}

// refreshed is whom refreshes were for, in order.
func refreshed(starts []passRefresh) []string {
	named := make([]string, len(starts))
	for i, start := range starts {
		named[i] = start.email
	}
	return named
}

// passLog keeps each claim attempt a membership pass reports (MembershipPass.Observe), with the
// time it was reported.
type passLog struct {
	mu     sync.Mutex
	events []passEvent
}

// passEvent is one claim attempt and when it was reported.
type passEvent struct {
	identity.MembershipPassEvent
	at time.Time
}

func (l *passLog) observe(event identity.MembershipPassEvent) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, passEvent{MembershipPassEvent: event, at: time.Now()})
}

// take is every claim attempt since the last take, in order.
func (l *passLog) take() []passEvent {
	l.mu.Lock()
	defer l.mu.Unlock()
	events := l.events
	l.events = nil
	return events
}

// claimed is the emails events claimed in step, in order.
func claimed(events []passEvent, step int) []string {
	var emails []string
	for _, event := range events {
		if event.Step == step && event.Claimed {
			emails = append(emails, event.Email)
		}
	}
	return emails
}

// stepTwoBegan is the latest moment step 2 of the pass events report can have begun before: the
// report of step 1's last claim attempt, or began, the pass's start, when step 1 made none. Step 2
// begins right after that report when the attempt claimed no row.
func stepTwoBegan(events []passEvent, began time.Time) time.Time {
	at := began
	for _, event := range events {
		if event.Step == 1 {
			at = event.at
		}
	}
	return at
}

// passClock moves the database's clock, as the membership pass reads it, forward: each move dates
// every people timestamp the pass reads back by the step less the time that really passed since the
// last move, so passes a test runs a minute apart are a minute apart by the database's clock,
// however long the test takes to run them.
type passClock struct {
	pool *store.Pool
	last time.Time
}

func newPassClock(pool *store.Pool) *passClock { return &passClock{pool: pool, last: time.Now()} }

func (c *passClock) advance(t *testing.T, step time.Duration) {
	t.Helper()
	now := time.Now()
	back := step - now.Sub(c.last)
	c.last = now
	if _, err := c.pool.Exec(context.Background(), `
		update people set
			signed_in_at = signed_in_at - make_interval(secs => $1),
			confirmed_at = confirmed_at - make_interval(secs => $1),
			held_since = held_since - make_interval(secs => $1),
			signed_out_at = signed_out_at - make_interval(secs => $1),
			confirm_tried_at = confirm_tried_at - make_interval(secs => $1),
			outliving_checked_at = outliving_checked_at - make_interval(secs => $1)
	`, back.Seconds()); err != nil {
		t.Fatalf("move the database's clock %s forward: %v", step, err)
	}
}

// accountTokenFile is a file holding Dispatch's own account token for the broker, as
// DISPATCH_AGENT_SECRETS_ACCOUNT_TOKEN_FILE names one.
func accountTokenFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "account-token")
	if err := os.WriteFile(path, []byte("dispatch-account-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// numbered is n addresses on leaverDomain, prefix01@ to prefix<n>@, in the order a claim that
// breaks ties by email takes them.
func numbered(prefix string, n int) []string {
	emails := make([]string, n)
	for i := range emails {
		emails[i] = fmt.Sprintf("%s%02d@%s", prefix, i+1, leaverDomain)
	}
	return emails
}

// passFixture is the membership pass as Dispatch runs it under DISPATCH_SIGNIN_DOMAIN, over a
// people store on a fresh database: its Membership refreshes through refresher, and its check of
// access that outlives the browser asks the database's agent_tokens and then broker.
type passFixture struct {
	database  *store.Store
	people    *store.PgPeopleStore
	refresher *passRefresher
	broker    *passBroker
	log       *passLog
	clock     *passClock
	pass      identity.MembershipPass
}

func newPassFixture(t *testing.T) *passFixture {
	t.Helper()
	database := storetest.Open(t)
	people := store.NewPgPeopleStore(database.Pool, testSigningKey, nil)
	boot := resolveWith(t, map[string]string{
		"DISPATCH_SIGNIN_ISSUER": "https://accounts.google.com",
		"DISPATCH_SIGNIN_GROUP":  "",
		"DISPATCH_SIGNIN_DOMAIN": leaverDomain,
	})
	rule, err := signInRule(boot, accessLister(database))
	if err != nil {
		t.Fatal(err)
	}
	f := &passFixture{database: database, people: people, refresher: &passRefresher{}, broker: newPassBroker(), log: &passLog{}, clock: newPassClock(database.Pool)}
	server := httptest.NewServer(f.broker)
	t.Cleanup(server.Close)
	f.pass = identity.MembershipPass{
		Membership: &identity.Membership{People: people, SignIn: f.refresher, Rule: rule},
		People:     people,
		Outliving:  cipher.OutlivingAccess{Pool: database.Pool, Broker: cipher.New(server.URL, accountTokenFile(t))},
		Observe:    f.log.observe,
	}
	return f
}

// signIn records a sign-in of each of emails as a full member's by this build: a Google account,
// "google-<email>", and a refresh token, "refresh-<email>", sealed as the store seals one.
func (f *passFixture) signIn(t *testing.T, emails ...string) {
	t.Helper()
	for _, email := range emails {
		if err := f.people.SignInSubject(context.Background(), email, "google-"+email, "refresh-"+email, leaverDomain, model.MemberFull); err != nil {
			t.Fatalf("record %s's sign-in: %v", email, err)
		}
	}
}

// due dates each of emails' confirmations past the hour, each a second older than the next, so
// the first is due longest.
func (f *passFixture) due(t *testing.T, emails ...string) {
	t.Helper()
	for i, email := range emails {
		if _, err := f.database.Pool.Exec(context.Background(), `
			update people set confirmed_at = now() - interval '2 hours' - make_interval(secs => $2) where email = $1
		`, email, len(emails)-i); err != nil {
			t.Fatalf("date %s's confirmation back: %v", email, err)
		}
	}
}

// signOut marks each of emails signed out with its refresh token kept, as a sign-out of a person
// whose access outlives the browser does (KeepAfterSignOut).
func (f *passFixture) signOut(t *testing.T, emails ...string) {
	t.Helper()
	if _, err := f.database.Pool.Exec(context.Background(), `update people set signed_out_at = clock_timestamp() where email = any($1)`, emails); err != nil {
		t.Fatalf("sign %v out: %v", emails, err)
	}
}

// run runs one pass and answers its claim attempts and how long it took.
func (f *passFixture) run(t *testing.T) ([]passEvent, time.Time, time.Duration) {
	t.Helper()
	began := time.Now()
	if err := f.pass.Run(context.Background()); err != nil {
		t.Fatalf("the membership pass: %v", err)
	}
	return f.log.take(), began, time.Since(began)
}

// requireSame fails t unless got is want, naming what.
func requireSame(t *testing.T, what string, got, want []string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("%s: %q, want %q", what, got, want)
		return
	}
	t.Logf("%s: %q, as wanted", what, got)
}

// One membership pass is bounded (the spec's Limits): of 25 overdue rows it asks Google about 20,
// the row due longest first, and the next pass asks the other 5 and none it asked before, even
// though Google could not be reached for any. With a token endpoint that never answers, it starts
// no refresh 20 seconds or more after its refresh step began, so two 15-second refreshes fill it,
// and it ends within 50 seconds; even so, its first step checks the signed-out rows due in every
// pass. When the broker never answers the first step's asks, each is cut at 5 seconds and the
// step starts none 10 seconds or more after the pass began, and the refresh step, timed from its
// own start, still starts two slow refreshes. Over an hour of passes, by the clock the test moves,
// 25 rows that the hourly check kept signed out (auth.EndKeep) are each asked about once, by the
// broker and by Google.
func TestMembershipPassBoundsEachRun(t *testing.T) {
	t.Run("of 25 overdue rows a pass asks 20, the row due longest first", func(t *testing.T) {
		f := newPassFixture(t)
		rows := numbered("due", 25)
		f.signIn(t, rows...)
		f.due(t, rows...)

		events, _, _ := f.run(t)
		requireSame(t, "the first pass asked Google about", refreshed(f.refresher.take()), rows[:20])
		requireSame(t, "the first pass's refresh step claimed", claimed(events, 2), rows[:20])
		if len(events) == 0 {
			t.Fatal("the first pass reported no claim attempt")
		}
		if last := events[len(events)-1]; last.Step != 2 || !last.Claimed {
			t.Errorf("the first pass's last claim attempt was %+v, want its 20th claim: a pass makes no 21st", last.MembershipPassEvent)
		}

		f.clock.advance(t, time.Minute)
		events, _, _ = f.run(t)
		requireSame(t, "the next pass, a minute later, asked Google about", refreshed(f.refresher.take()), rows[20:])
		requireSame(t, "the next pass's refresh step claimed", claimed(events, 2), rows[20:])
	})

	t.Run("a token endpoint that never answers", func(t *testing.T) {
		t.Parallel()
		f := newPassFixture(t)
		rows := numbered("due", 25)
		f.signIn(t, rows...)
		f.due(t, rows...)
		f.refresher.answerNever()
		for pass, signedOut := range [][]string{numbered("out", 3), numbered("later", 3)} {
			if pass > 0 {
				f.clock.advance(t, time.Minute)
			}
			f.signIn(t, signedOut...)
			f.signOut(t, signedOut...)
			f.broker.keep(signedOut...)
			name := fmt.Sprintf("pass %d", pass+1)

			events, began, took := f.run(t)
			requireSame(t, name+" asked the broker about the signed-out rows", persons(f.broker.take()), signedOut)
			starts := f.refresher.take()
			requireSame(t, name+" asked Google about", refreshed(starts), rows[2*pass:2*pass+2])
			stepTwo := stepTwoBegan(events, began)
			for _, start := range starts {
				if after := start.at.Sub(stepTwo); after >= 20*time.Second {
					t.Errorf("%s started the refresh of %s %v after its refresh step began, want under 20s", name, start.email, after)
				}
			}
			if took >= 50*time.Second {
				t.Errorf("%s took %v, want under 50s", name, took)
			} else {
				t.Logf("%s took %v, under 50s as wanted", name, took)
			}
		}
	})

	t.Run("after a slow signed-out step, two slow refreshes still start", func(t *testing.T) {
		t.Parallel()
		f := newPassFixture(t)
		signedOut := numbered("out", 6)
		f.signIn(t, signedOut...)
		f.signOut(t, signedOut...)
		f.broker.answerNever()
		rows := numbered("due", 3)
		f.signIn(t, rows...)
		f.due(t, rows...)
		f.refresher.answerNever()

		events, began, took := f.run(t)
		asks := f.broker.take()
		requireSame(t, "the pass asked the broker about", persons(asks), signedOut[:2])
		for _, ask := range asks {
			if after := ask.asked.Sub(began); after >= 10*time.Second {
				t.Errorf("the pass asked the broker about %s %v after it began, want under 10s", ask.person, after)
			}
			if lasted := ask.done.Sub(ask.asked); lasted > 6*time.Second {
				t.Errorf("the pass's ask about %s lasted %v, want it cut at 5s", ask.person, lasted)
			}
		}
		if got := claimed(events, 1); len(got) != 2 {
			t.Errorf("the pass's signed-out step claimed %q, want the 2 it asked about", got)
		}
		requireSame(t, "the pass asked Google about", refreshed(f.refresher.take()), rows[:2])
		if took >= 50*time.Second {
			t.Errorf("the pass took %v, want under 50s", took)
		} else {
			t.Logf("the pass took %v, under 50s as wanted", took)
		}
		for _, email := range signedOut {
			var stored bool
			if err := f.database.Pool.QueryRow(context.Background(), `select refresh_token is not null from people where email = $1`, email).Scan(&stored); err != nil {
				t.Fatal(err)
			}
			if !stored {
				t.Errorf("%s's refresh token was forgotten while the broker could not be asked, want it kept", email)
			}
		}
	})

	t.Run("25 rows the hourly check kept are each asked once in an hour of passes", func(t *testing.T) {
		f := newPassFixture(t)
		rows := numbered("kept", 25)
		f.signIn(t, rows...)
		for _, email := range rows {
			if ended, err := f.people.EndSubject(context.Background(), email, "google-"+email, "refresh-"+email, auth.EndKeep); err != nil || !ended {
				t.Fatalf("keep %s's sign-in: ended %t, %v", email, ended, err)
			}
		}
		f.broker.keep(rows...)
		f.clock = newPassClock(f.database.Pool)
		f.clock.advance(t, time.Hour)

		askedByBroker, askedByGoogle := map[string]int{}, map[string]int{}
		for pass := range 60 {
			if pass > 0 {
				f.clock.advance(t, time.Minute)
			}
			events, _, _ := f.run(t)
			if pass == 0 && (len(claimed(events, 1)) != 20 || len(claimed(events, 2)) != 20) {
				t.Errorf("the first pass claimed %d signed-out rows and %d due rows, want 20 of each", len(claimed(events, 1)), len(claimed(events, 2)))
			}
			for _, person := range persons(f.broker.take()) {
				askedByBroker[person]++
			}
			for _, email := range refreshed(f.refresher.take()) {
				askedByGoogle[email]++
			}
		}
		for _, email := range rows {
			if askedByBroker[email] != 1 || askedByGoogle[email] != 1 {
				t.Errorf("in an hour of passes %s was asked about %d times by the broker and %d by Google, want once by each", email, askedByBroker[email], askedByGoogle[email])
			}
		}
		if !t.Failed() {
			t.Logf("in an hour of passes each of the %d kept rows was asked about once by the broker and once by Google", len(rows))
		}
	})
}

// firstPassSigningKey is DISPATCH_SIGNING_KEY for both processes of
// TestMembershipPassChecksEveryStoredToken, so the refresh tokens the first seals open in the
// second.
const firstPassSigningKey = "membership-pass-signing-key"

// startFirstPassProcess starts the compiled envoy-dispatch on database under a Google sign-in for
// leaverDomain, against google in Google's place, with env added.
func startFirstPassProcess(t *testing.T, database *store.Store, google *oidctest.Issuer, env map[string]string) *dispatchProcess {
	t.Helper()
	values := map[string]string{
		"DISPATCH_IDENTITY":                  "cookie",
		"DISPATCH_INSECURE_COOKIE":           "1",
		"DISPATCH_SIGNIN_ISSUER":             google.URL(),
		"DISPATCH_SIGNIN_CLIENT_ID":          cookieClientID,
		"DISPATCH_SIGNIN_CLIENT_SECRET":      cookieClientSecret,
		"DISPATCH_SIGNIN_DOMAIN":             leaverDomain,
		"DISPATCH_TEST_SIGNIN_DOMAIN_ISSUER": "1",
	}
	maps.Copy(values, env)
	process := startDispatchProcessWith(t, database.Pool.Config().ConnString(), dispatchOptions{
		SigningKey: firstPassSigningKey,
		Env:        values,
		Without:    []string{"DISPATCH_IDENTITY_HEADER_TRUSTED"},
	})
	if err := process.healthyWithin(2 * time.Minute); err != nil {
		t.Fatal(err)
	}
	return process
}

// workspaceClaims are the claims Google's ID token carries for email, the verified address of the
// Workspace account "google-<email>" of leaverDomain.
func workspaceClaims(email string) map[string]any {
	return map[string]any{"sub": "google-" + email, "email": email, "email_verified": true, "hd": leaverDomain}
}

// The first membership passes check everyone who signed in before this change and still has a
// stored token (c-first-pass), whether or not they make a request. People sign in through the
// compiled envoy-dispatch's own sign-in against the fake Google; that process stops; SQL makes the
// rows look as they did before this change (no hosted_domain, confirmations two hours old), and
// adds a row with a recorded Google account and no token, one with neither, and a signed-out row,
// confirmed a minute ago, whose person has no access that outlives the browser. A second process
// boots on that database with the same signing key, the broker stand-in in its environment, and
// nobody makes a request. Its boot logs the census: 4 rows it can check, 0 held, 2 it cannot. Its
// first pass, with Google refusing ada, confirming bob and answering 503 for gina, logs 1 confirmed,
// 1 held, 0 kept, 1 unasked and 1 forgotten; ada is then held, the row with no token is unchanged,
// and carol's signed-out row has no refresh token. The next pass asks Google about none of the
// three rows the first tried.
func TestMembershipPassChecksEveryStoredToken(t *testing.T) {
	ctx := context.Background()
	const (
		refused     = adaEmail
		confirmed   = bobEmail
		unreachable = "gina@example.com"
		noToken     = "hal@example.com"
		neither     = "ivy@example.com"
		signedOut   = carolEmail
	)
	database := storetest.Open(t)
	google := oidctest.New(t)
	google.EnableCodeFlow(google.PublishKey(t, "signing-key"), cookieClientID, cookieClientSecret)
	broker := newPassBroker()
	brokerServer := httptest.NewServer(broker)
	t.Cleanup(brokerServer.Close)

	first := startFirstPassProcess(t, database, google, nil)
	first.waitForHealth(t, time.Minute, "signin true", func(health healthBody) bool { return health.SignIn != nil && *health.SignIn })
	for _, email := range []string{refused, confirmed, unreachable, noToken, signedOut} {
		signInWith(t, first, google, workspaceClaims(email))
	}
	first.Terminate(t)
	first.WaitExit(t, "SIGTERM")

	for _, statement := range []struct {
		what, sql string
		args      []any
	}{
		{"date three confirmations two hours back", `update people set confirmed_at = now() - interval '2 hours' where email = any($1)`, []any{[]string{refused, confirmed, unreachable}}},
		{"forget every recorded hosted domain", `update people set hosted_domain = null`, nil},
		{"leave a row with a Google account and no token", `update people set refresh_token = null, confirmed_at = null where email = $1`, []any{noToken}},
		{"add a row with neither", `insert into people (email) values ($1)`, []any{neither}},
		{"sign a row out, confirmed a minute ago", `update people set signed_out_at = clock_timestamp(), confirmed_at = now() - interval '1 minute' where email = $1`, []any{signedOut}},
	} {
		if _, err := database.Pool.Exec(ctx, statement.sql, statement.args...); err != nil {
			t.Fatalf("%s: %v", statement.what, err)
		}
	}
	row := func(email string) string {
		t.Helper()
		var text string
		if err := database.Pool.QueryRow(ctx, `select row_to_json(p)::text from people p where email = $1`, email).Scan(&text); err != nil {
			t.Fatalf("read %s's people row: %v", email, err)
		}
		return text
	}
	noTokenRow := row(noToken)
	google.RefuseRefresh(refused, true)
	google.FailRefreshFor(unreachable, http.StatusServiceUnavailable)

	second := startFirstPassProcess(t, database, google, map[string]string{
		"DISPATCH_TEST_MAINTENANCE_SWEEP_INTERVAL":  "2s",
		"DISPATCH_AGENT_SECRETS_URL":                brokerServer.URL,
		"DISPATCH_AGENT_SECRETS_ACCOUNT_TOKEN_FILE": accountTokenFile(t),
		// The origin a broker token's DPoP proof is checked against, which a broker URL needs;
		// this process takes no request a broker token would make, and no sign-in.
		"DISPATCH_SERVER_URL": "http://127.0.0.1",
	})
	const census = `msg="dispatch: people census" checkable=4 held=0 uncheckable=2`
	if !second.logged(census) {
		t.Errorf("the second process's boot did not log %s:\n%s", census, second.Output.String())
	}

	// confirmTried is each row the first pass refreshed, by its last refresh attempt.
	confirmTried := func() map[string]string {
		t.Helper()
		rows, err := database.Pool.Query(ctx, `select email, coalesce(confirm_tried_at::text, '') from people where email = any($1)`, []string{refused, confirmed, unreachable})
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		at := map[string]string{}
		for rows.Next() {
			var email, when string
			if err := rows.Scan(&email, &when); err != nil {
				t.Fatal(err)
			}
			at[email] = when
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return at
	}
	const passLine = `msg="dispatch: membership pass"`
	leaseLine := []string{`msg="dispatch duty: leased pass ran"`, "lease=suggestion-outcomes"}
	for deadline := time.Now().Add(90 * time.Second); !second.logged(passLine) && time.Now().Before(deadline); {
		time.Sleep(50 * time.Millisecond)
	}
	// Read at once, two seconds before the next pass: the Google refreshes the first pass made
	// (oauth2 may send a refused grant twice, once per client authentication style, so this is no
	// count of rows), and each row's last attempt.
	refreshesAtFirst := google.Refreshes()
	triedAtFirst := confirmTried()
	passesAtFirst := len(second.logLines(leaseLine...))
	if !second.logged(passLine) {
		t.Errorf("no membership pass logged %s within 90s:\n%s", passLine, second.Output.String())
	} else if want := passLine + ` confirmed=1 held=1 kept=0 unasked=1 forgotten=1`; !second.logged(want) {
		t.Errorf("the first membership pass logged %q, want %s", second.logLines(passLine), want)
	} else {
		t.Logf("the first membership pass logged %s, as wanted", want)
	}

	var held, refusedStores bool
	if err := database.Pool.QueryRow(ctx, `select coalesce(held_since > signed_in_at, false), refresh_token is not null from people where email = $1`, refused).Scan(&held, &refusedStores); err != nil {
		t.Fatal(err)
	}
	if !held || refusedStores {
		t.Errorf("after the first pass %s, whom Google refuses, reads held %t with a refresh token %t; want held with none", refused, held, refusedStores)
	}
	if got := row(noToken); got != noTokenRow {
		t.Errorf("the first pass changed %s's row, which holds no token:\nbefore %s\nafter  %s", noToken, noTokenRow, got)
	}
	var signedOutStores bool
	if err := database.Pool.QueryRow(ctx, `select refresh_token is not null from people where email = $1`, signedOut).Scan(&signedOutStores); err != nil {
		t.Fatal(err)
	}
	if signedOutStores {
		t.Errorf("after the first pass %s's signed-out row, whose person has no access that outlives the browser, holds a refresh token, want none", signedOut)
	}
	if asked := persons(broker.take()); !slices.Equal(asked, []string{signedOut}) {
		t.Errorf("the first pass asked the broker about %q, want %s alone", asked, signedOut)
	}
	if t.Failed() {
		t.FailNow()
	}

	// The next pass, and the one after: neither asks Google about a row the first tried. The
	// refresh count and the pass lines say so however late this reads them; the attempt times
	// name the row when the read came before the next pass.
	for deadline := time.Now().Add(time.Minute); len(second.logLines(leaseLine...)) < passesAtFirst+2; {
		if time.Now().After(deadline) {
			t.Fatalf("the maintenance sweep ran no pass after the first membership pass within a minute:\n%s", second.Output.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
	for email, at := range confirmTried() {
		if at != triedAtFirst[email] {
			t.Errorf("a later pass asked Google about %s again (confirm_tried_at %s, then %s), want it tried again only an hour later", email, triedAtFirst[email], at)
		}
	}
	if got := google.Refreshes(); got != refreshesAtFirst {
		t.Errorf("the passes after the first asked Google for %d more refreshes, want none", got-refreshesAtFirst)
	}
	if lines := second.logLines(passLine); len(lines) != 1 {
		t.Errorf("the membership pass logged %d lines, want the first pass's alone, since a later one claims no row:\n%s", len(lines), strings.Join(lines, "\n"))
	}
}

// membershipPass is the membership pass the rig's Dispatch runs: the router's Membership, its
// people store, and the sign-out's check of access that outlives the browser.
func (r *leaverRig) membershipPass(observe func(identity.MembershipPassEvent)) identity.MembershipPass {
	return identity.MembershipPass{Membership: r.membership, People: r.outside.people.PgPeopleStore, Outliving: r.app.Outliving(), Observe: observe}
}

// runPass runs pass once and fails t on its error.
func runPass(t *testing.T, pass identity.MembershipPass) {
	t.Helper()
	if err := pass.Run(context.Background()); err != nil {
		t.Fatalf("the membership pass: %v", err)
	}
}

// A membership pass forgets the kept token of a signed-out person who no longer has access that
// outlives the browser (c-sign-out, the pass's half). Ada signs out holding a live dsp_ token, so
// her refresh token is kept; her token then revokes itself, her last. Twenty other signed-out rows
// that keep their access come before hers, and fill the first pass; she is still reached within
// the hour's passes, which forget her refresh token and keep the other twenty. Dave, signed out
// with no such access, signs in again while a pass asks the broker about him: the pass's forget
// misses its guard, so his new sign-in keeps its token, and the pass counts nobody forgotten.
func TestMembershipPassForgetsASignedOutToken(t *testing.T) {
	ctx := context.Background()
	broker := newPassBroker()
	r := newLeaverRig(t, withPersonAccess(broker))
	ada := r.signIn(t, adaEmail)
	ada.dsp = r.mintToken(t, ada, "ada's agent")
	r.signOut(t, ada)
	expectRefreshToken(t, r, "ada@example.com signed out holding a live dsp_ token", adaEmail, true)

	ahead := numbered("aa", 20)
	for _, email := range ahead {
		if err := r.outside.people.SignInSubject(ctx, email, "google-"+email, "refresh-"+email, leaverDomain, model.MemberFull); err != nil {
			t.Fatalf("record %s's sign-in: %v", email, err)
		}
	}
	if _, err := r.outside.database.Pool.Exec(ctx, `update people set signed_out_at = clock_timestamp() where email = any($1)`, ahead); err != nil {
		t.Fatal(err)
	}
	broker.keep(ahead...)
	if revoked := bearerRequest(r.handler, http.MethodDelete, "/api/v1/me/agent-tokens/"+ada.dsp.id, "", ada.dsp.plaintext); revoked.Code != http.StatusNoContent {
		t.Fatalf("ada@example.com's dsp_ token revoking itself answered %d %s, want 204", revoked.Code, revoked.Body.String())
	}
	if t.Failed() {
		t.FailNow()
	}

	log := &passLog{}
	pass := r.membershipPass(log.observe)
	clock := newPassClock(r.outside.database.Pool)
	reached := 0
	for run := 1; run <= 60 && reached == 0; run++ {
		if run > 1 {
			clock.advance(t, time.Minute)
		}
		runPass(t, pass)
		signedOutClaims := claimed(log.take(), 1)
		if run == 1 {
			requireSame(t, "the first pass's signed-out step claimed", signedOutClaims, ahead)
		}
		if !r.storesRefreshToken(t, adaEmail) {
			reached = run
		}
	}
	if reached == 0 {
		t.Errorf("an hour of passes left ada@example.com's refresh token, whose last dsp_ token is revoked, want it forgotten")
	} else {
		t.Logf("pass %d forgot ada@example.com's refresh token, whose last dsp_ token is revoked", reached)
	}
	if reached == 1 {
		t.Errorf("the first pass reached ada@example.com, want the twenty rows ahead of hers to fill it")
	}
	asked := map[string]int{}
	for _, person := range persons(broker.take()) {
		asked[person]++
	}
	for _, email := range ahead {
		if !r.storesRefreshToken(t, email) || asked[email] != 1 {
			t.Errorf("%s, signed out with access that outlives the browser: refresh token kept %t, the broker asked %d times; want kept, asked once", email, r.storesRefreshToken(t, email), asked[email])
		}
	}

	const dave = "dave@example.com"
	if err := r.outside.people.SignInSubject(ctx, dave, "google-"+dave, "refresh-"+dave, leaverDomain, model.MemberFull); err != nil {
		t.Fatalf("record %s's sign-in: %v", dave, err)
	}
	if _, err := r.outside.database.Pool.Exec(ctx, `update people set signed_out_at = clock_timestamp() where email = $1`, dave); err != nil {
		t.Fatal(err)
	}
	signedInAgain := make(chan error, 1)
	broker.beforeAnswering(dave, func() {
		signedInAgain <- r.outside.people.SignInSubject(ctx, dave, "google-"+dave, "refresh-again-"+dave, leaverDomain, model.MemberFull)
	})
	logged := &logtest.Buffer{}
	logtest.SetDefault(t, slog.NewTextHandler(logged, nil))
	runPass(t, pass)
	select {
	case err := <-signedInAgain:
		if err != nil {
			t.Fatalf("%s's sign-in while the pass asked the broker about him: %v", dave, err)
		}
	default:
		t.Fatalf("the pass never asked the broker about %s, so he never signed in during it", dave)
	}
	requireSame(t, "the pass during dave's sign-in claimed", claimed(log.take(), 1), []string{dave})
	if line := logged.Lines(`msg="dispatch: membership pass"`); !strings.Contains(line, " forgotten=0") {
		t.Errorf("the pass whose forget of %s missed its guard logged %q, want forgotten=0", dave, line)
	} else {
		t.Logf("the pass whose forget of %s missed its guard logged %q, as wanted", dave, strings.TrimSpace(line))
	}
	if membership, _, err := r.outside.people.Membership(ctx, dave); err != nil || membership.RefreshToken != "refresh-again-"+dave || membership.SignedOut {
		t.Errorf("after the pass %s, who signed in during it, reads refresh token %q, signed out %t (%v); want his new sign-in's token, signed in", dave, membership.RefreshToken, membership.SignedOut, err)
	}
}

// A restricted member whom the hourly check kept signed out is held by the membership pass when
// Google refuses her (c-refusal-order, erin's case). Erin, whom PRIV's list names, holds a dsp_
// token; the list stops naming her, and her browser's next request past the hour ends her sign-in
// and keeps her token. SQL dates her confirmation and last attempt an hour back, and at the next
// pass Google refuses her refresh. When the list names her again, her token answers 401
// MEMBERSHIP_HELD.
func TestMembershipPassHoldsAKeptRestrictedMember(t *testing.T) {
	ctx := context.Background()
	r := newLeaverRig(t, withPersonAccess(newPassBroker()))
	erin := r.signIn(t, erinEmail)
	erin.dsp = r.mintToken(t, erin, "erin's agent")
	expectAnswer(t, "erin@outside.example's dsp_ token, PRIV's list naming her", http.StatusOK, "").got(r.personalToken(t, erin.dsp))
	if t.Failed() {
		t.FailNow()
	}

	r.delist(t, erinEmail)
	r.ageConfirmation(t, erinEmail, confirmationAge)
	expectAnswer(t, "erin@outside.example's cookie (GET /api/v1/whoami), the first request past the hour since PRIV's list stopped naming her", http.StatusUnauthorized, "NO_IDENTITY").got(r.cookie(t, erin))
	expectRefreshToken(t, r, "the hourly check ended erin@outside.example's sign-in for PRIV's list", erinEmail, true)
	expectAnswer(t, "erin@outside.example's dsp_ token, PRIV's list not naming her", http.StatusUnauthorized, "TOKEN_OWNER_NOT_ADMITTED").got(r.personalToken(t, erin.dsp))
	if t.Failed() {
		t.FailNow()
	}

	if _, err := r.outside.database.Pool.Exec(ctx, `
		update people set confirmed_at = confirmed_at - interval '1 hour', confirm_tried_at = confirm_tried_at - interval '1 hour' where email = $1
	`, erinEmail); err != nil {
		t.Fatal(err)
	}
	r.refuse(erinEmail)
	log := &passLog{}
	runPass(t, r.membershipPass(log.observe))
	requireSame(t, "the pass's refresh step claimed", claimed(log.take(), 2), []string{erinEmail})
	var held bool
	if err := r.outside.database.Pool.QueryRow(ctx, `select coalesce(held_since > signed_in_at, false) from people where email = $1`, erinEmail).Scan(&held); err != nil {
		t.Fatal(err)
	}
	if !held {
		t.Errorf("after the pass at which Google refused erin@outside.example, her row records no hold")
	}

	r.list(t, erinEmail)
	expectAnswer(t, "erin@outside.example's dsp_ token, held and PRIV's list naming her again", http.StatusUnauthorized, "MEMBERSHIP_HELD").got(r.personalToken(t, erin.dsp))
}
