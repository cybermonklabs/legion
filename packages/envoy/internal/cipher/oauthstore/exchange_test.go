package oauthstore_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"authelia.com/provider/oauth2"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/cipher/oauthstore"
	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
	"github.com/sjawhar/envoy/internal/cipher/store/storetest"
)

// The principals a session name is claimed for here, spelled as the exchange spells them: a listed
// service account, a listed workflow and a person, the three kinds the exchange decides, and a
// second account standing for any other caller.
const (
	accountPrincipal  = "account:system:serviceaccount:example-ns:example-worker"
	daemonPrincipal   = "account:system:serviceaccount:example-ns:example-daemon"
	otherPrincipal    = "account:system:serviceaccount:other-ns:example-worker"
	workflowPrincipal = "workflow:example-org/example-repo/.github/workflows/release.yaml@refs/heads/main"
	personPrincipal   = "person:ada@example.com"
)

// exchangeRequest is an exchanged token's request: the exchange client, an ExchangeSession
// carrying the claims an exchange decided and the tokens it decided from, and no launcher
// credential, because an exchanged token follows no machine login.
func exchangeRequest(t *testing.T, s *oauthstore.Store, requestID string) *oauth2.Request {
	t.Helper()
	client, err := s.GetClient(context.Background(), oauthwire.ExchangeClientID)
	if err != nil {
		t.Fatalf("GetClient: %v", err)
	}
	session := oauthstore.NewExchangeSession()
	session.Subject = "ada@example.com"
	session.Username = "ada@example.com"
	session.JWKThumbprint = "thumbprint-of-the-callers-key"
	session.Extra["session"] = "implementer-g1"
	session.Extra["person"] = "ada@example.com"
	session.Extra["project"] = "ACME"
	session.SetExpiresAt(oauth2.AccessToken, time.Now().Add(time.Hour))
	session.SetSubjectToken(map[string]any{"sub": "system:serviceaccount:example-ns:example-worker"})
	session.SetActorToken(map[string]any{"sub": "example-actor"})
	request := oauth2.NewRequest()
	request.SetID(requestID)
	request.Client = client
	request.GrantScope(oauthwire.Scope)
	request.SetSession(session)
	return request
}

// TestASessionNameIsHeldByThePrincipalThatClaimedIt: the first principal to claim a session name
// holds it until SessionRetention past the expiry of the last token it claimed it for: re-claiming
// extends the claim and never shortens it, and another principal is refused by name meanwhile,
// even once that token has expired. Only a claim past its retention is taken over.
func TestASessionNameIsHeldByThePrincipalThatClaimedIt(t *testing.T) {
	ctx := context.Background()
	s, pool := newStore(t)
	hour := time.Now().Add(time.Hour)
	for _, c := range []struct {
		name      string
		session   string
		principal string
		expires   time.Time
		runOut    bool
		want      error
	}{
		{name: "an unheld name is claimed", session: "implementer-g1", principal: accountPrincipal, expires: hour},
		{name: "the same principal re-claims it", session: "implementer-g1", principal: accountPrincipal, expires: hour.Add(time.Hour)},
		{name: "a shorter-lived token does not shorten the claim", session: "implementer-g1", principal: accountPrincipal, expires: time.Now().Add(time.Minute)},
		{name: "another principal is refused", session: "implementer-g1", principal: personPrincipal, expires: hour, want: oauthstore.ErrSessionHeld},
		{name: "another name is free", session: "ada-devbox", principal: personPrincipal, expires: hour},
		{name: "a name whose token has expired is claimed", session: "reviewer-g1", principal: accountPrincipal, expires: time.Now().Add(-time.Hour)},
		{name: "and is still refused to another principal", session: "reviewer-g1", principal: otherPrincipal, expires: hour, want: oauthstore.ErrSessionHeld},
		{name: "a claim past its retention is taken over", session: "implementer-g1", principal: personPrincipal, expires: hour, runOut: true},
		{name: "and then holds against the principal that let it run out", session: "implementer-g1", principal: accountPrincipal, expires: hour, want: oauthstore.ErrSessionHeld},
	} {
		t.Run(c.name, func(t *testing.T) {
			if c.runOut {
				if _, err := pool.Exec(ctx, `update oauth_session_bindings set expires_at = now() - interval '1 second' where session = $1`, c.session); err != nil {
					t.Fatalf("run the claim out: %v", err)
				}
			}
			taken, err := s.BindSession(ctx, c.session, c.principal, false, c.expires)
			if !errors.Is(err, c.want) {
				t.Fatalf("BindSession(%q, %q) = %v, want %v", c.session, c.principal, err, c.want)
			}
			if c.want != nil && !strings.Contains(err.Error(), c.session) {
				t.Errorf("BindSession refused with %q, which does not name the session %q", err, c.session)
			}
			if taken != "" {
				t.Errorf("BindSession answered it took %q from %q, want nothing taken by a claim no configuration fixes", c.session, taken)
			}
		})
	}
	_, held, _ := heldBy(t, pool, "implementer-g1")
	if want := hour.Add(oauthstore.SessionRetention); held.Before(want.Add(-time.Second)) {
		t.Errorf("the claim runs out at %s, want SessionRetention past the expiry of the token it was made for, %s", held, want)
	}
}

// TestTheNamesTheConfigurationFixesAreHeldForTheirEntries: ReserveSessions holds every name it is
// handed for its principal unless another principal holds it, and says who does; the entry's own
// claim, which its configuration fixes the name for, takes it from that holder and says whom.
// From then on any other principal is refused it, its own principal claims it as often as it
// likes, and no claim or sweep lets it run out. A name a later configuration no longer fixes goes
// back to the ordinary rule: its principal keeps it for SessionRetention from then, and nobody
// else takes it meanwhile. A configuration re-pointing a name takes it at its new entry's claim,
// and an older configuration reserving again afterwards takes nothing back.
func TestTheNamesTheConfigurationFixesAreHeldForTheirEntries(t *testing.T) {
	ctx := context.Background()
	s, pool := newStore(t)
	hour := time.Now().Add(time.Hour)
	if _, err := s.BindSession(ctx, "legion-daemon:ACME", personPrincipal, false, hour); err != nil {
		t.Fatalf("a dev box claims the name before the configuration fixes it: %v", err)
	}
	first := map[string]string{"legion-daemon:ACME": daemonPrincipal, "ci-release": workflowPrincipal}
	left, err := s.ReserveSessions(ctx, first)
	if err != nil {
		t.Fatalf("ReserveSessions: %v", err)
	}
	if want := map[string]string{"legion-daemon:ACME": personPrincipal}; !reflect.DeepEqual(left, want) {
		t.Fatalf("ReserveSessions left %v with their holders, want %v: the dev box keeps the name until the entry claims it, and an unheld one is reserved", left, want)
	}
	for _, c := range []struct {
		name      string
		session   string
		principal string
		fixed     bool
		want      error
		taken     string
	}{
		{name: "another account is refused the name the dev box holds", session: "legion-daemon:ACME", principal: otherPrincipal, want: oauthstore.ErrSessionHeld},
		{name: "the entry's account takes it from the dev box", session: "legion-daemon:ACME", principal: daemonPrincipal, fixed: true, taken: personPrincipal},
		{name: "the dev box that held it is refused", session: "legion-daemon:ACME", principal: personPrincipal, want: oauthstore.ErrSessionHeld},
		{name: "another account is refused", session: "legion-daemon:ACME", principal: otherPrincipal, want: oauthstore.ErrSessionHeld},
		{name: "and the entry's account claims it again", session: "legion-daemon:ACME", principal: daemonPrincipal, fixed: true},
		{name: "a dev box is refused the workflow's name before the workflow has claimed it", session: "ci-release", principal: personPrincipal, want: oauthstore.ErrSessionHeld},
		{name: "the workflow claims its name", session: "ci-release", principal: workflowPrincipal, fixed: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			taken, err := s.BindSession(ctx, c.session, c.principal, c.fixed, hour)
			if !errors.Is(err, c.want) || taken != c.taken {
				t.Fatalf("BindSession(%q, %q, fixed %t) = %q, %v, want %q, %v", c.session, c.principal, c.fixed, taken, err, c.taken, c.want)
			}
		})
	}
	if err := s.Sweep(ctx); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	for session, want := range first {
		if principal, _, forever := heldBy(t, pool, session); principal != want || !forever {
			t.Errorf("after its principal's claims and a sweep, %s is held by %q for ever: %t; want %q with no expiry", session, principal, forever, want)
		}
	}

	// The next configuration fixes the daemon's name for another account and no longer fixes the
	// workflow's. Reserving takes nothing; the other account's own claim does.
	next := map[string]string{"legion-daemon:ACME": otherPrincipal}
	left, err = s.ReserveSessions(ctx, next)
	if err != nil {
		t.Fatalf("ReserveSessions(the next configuration): %v", err)
	}
	if want := map[string]string{"legion-daemon:ACME": daemonPrincipal}; !reflect.DeepEqual(left, want) {
		t.Fatalf("ReserveSessions(the next configuration) left %v with their holders, want %v", left, want)
	}
	principal, runsOut, forever := heldBy(t, pool, "ci-release")
	if principal != workflowPrincipal || forever {
		t.Errorf("the name the configuration stopped fixing is held by %q for ever: %t; want %q under the ordinary retention", principal, forever, workflowPrincipal)
	}
	if want := time.Now().Add(oauthstore.SessionRetention); runsOut.Before(want.Add(-time.Minute)) || runsOut.After(want.Add(time.Minute)) {
		t.Errorf("the released claim runs out at %s, want SessionRetention from now, about %s", runsOut, want)
	}
	if _, err := s.BindSession(ctx, "ci-release", personPrincipal, false, hour); !errors.Is(err, oauthstore.ErrSessionHeld) {
		t.Errorf("BindSession(the released name, a dev box) = %v, want ErrSessionHeld: the workflow used it within the retention", err)
	}
	if taken, err := s.BindSession(ctx, "legion-daemon:ACME", otherPrincipal, true, hour); err != nil || taken != daemonPrincipal {
		t.Fatalf("BindSession(the re-pointed name, its new entry) = %q, %v, want it taken from %q", taken, err, daemonPrincipal)
	}

	// A replica still on the first configuration reserves after the next one has: it takes the
	// re-pointed name back from nobody, and a name both hold for one principal is its again.
	left, err = s.ReserveSessions(ctx, first)
	if err != nil {
		t.Fatalf("ReserveSessions(the first configuration, again): %v", err)
	}
	if want := map[string]string{"legion-daemon:ACME": otherPrincipal}; !reflect.DeepEqual(left, want) {
		t.Fatalf("ReserveSessions(the first configuration, again) left %v with their holders, want %v", left, want)
	}
	if principal, _, forever := heldBy(t, pool, "legion-daemon:ACME"); principal != otherPrincipal || !forever {
		t.Errorf("after an older configuration reserved again, the re-pointed name is held by %q for ever: %t; want %q with no expiry", principal, forever, otherPrincipal)
	}
	if principal, _, forever := heldBy(t, pool, "ci-release"); principal != workflowPrincipal || !forever {
		t.Errorf("the workflow's name, which the older configuration fixes for it, is held by %q for ever: %t; want %q with no expiry", principal, forever, workflowPrincipal)
	}
	if _, err := s.BindSession(ctx, "legion-daemon:ACME", daemonPrincipal, false, hour); !errors.Is(err, oauthstore.ErrSessionHeld) {
		t.Errorf("BindSession(the re-pointed name, its old entry under the next configuration) = %v, want ErrSessionHeld", err)
	}
}

// The accounts the pair tests claim names for, spelled as kubernetes.Principal spells them:
// the shared account a deployment's pods run as before an operator moves them, the project's own
// account they move to, a third account each test pairs with one of those two and not the other,
// and an account paired with nobody (BROKER_SESSION_ACCOUNT_PAIRS).
const (
	sharedWorkerPrincipal  = "account:system:serviceaccount:example-ns:shared-worker"
	projectWorkerPrincipal = "account:system:serviceaccount:example-ns:project-worker"
	thirdWorkerPrincipal   = "account:system:serviceaccount:example-ns:third-worker"
	unpairedPrincipal      = "account:system:serviceaccount:example-ns:unpaired-worker"
)

// heldBy reads the row holding session: its principal, its expiry, and whether it is held with no
// expiry ('infinity', which reads as the zero time).
func heldBy(t *testing.T, pool *pgxpool.Pool, session string) (principal string, expires time.Time, forever bool) {
	t.Helper()
	var at pgtype.Timestamptz
	if err := pool.QueryRow(context.Background(), `select principal, expires_at from oauth_session_bindings where session = $1`, session).Scan(&principal, &at); err != nil {
		t.Fatalf("read the claim on %s: %v", session, err)
	}
	return principal, at.Time, at.InfinityModifier == pgtype.Infinity
}

// raceForName makes first's claim of session, for an hour's token, in a transaction it holds
// open; starts second's claim of it, which runs in a transaction of BindSession's own; sees that
// claim wait on a lock first's transaction holds; and only then commits first's. It answers what
// first's claim answered and what second's answered once first's had committed, so a test reads
// the decision the waiting claim made after the row it waited on changed under it.
func raceForName(t *testing.T, s *oauthstore.Store, pool *pgxpool.Pool, session, first, second string) (firstTaken, secondTaken string, secondErr error) {
	t.Helper()
	ctx := context.Background()
	hour := time.Now().Add(time.Hour)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(ctx) })
	firstTaken, err = s.BindSession(oauthstore.WithTx(ctx, tx), session, first, false, hour)
	if err != nil {
		t.Fatalf("BindSession(%q, %q) in a transaction held open = %v, want it claimed", session, first, err)
	}
	type answer struct {
		taken string
		err   error
	}
	answers := make(chan answer, 1)
	go func() {
		taken, err := s.BindSession(ctx, session, second, false, hour)
		answers <- answer{taken, err}
	}()
	storetest.AwaitLockWaiters(t, pool, tx, 1)
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit %q's claim of %q: %v", first, session, err)
	}
	var a answer
	select {
	case a = <-answers:
	case <-time.After(10 * time.Second):
		t.Fatalf("%q's claim of %q did not end within ten seconds of %q's claim committing", second, session, first)
	}
	return firstTaken, a.taken, a.err
}

// TestAPairedAccountTakesTheNameItsPairHolds: an account paired with the holder of a live session
// name takes it, says whom it took it from, and holds it for its own claim's retention rather
// than for the longer hold it took the name from. The pair works both ways: the account the name
// was taken from takes it back the same way, again for its own claim's retention, though the
// claim it takes the name from runs out later.
func TestAPairedAccountTakesTheNameItsPairHolds(t *testing.T) {
	ctx := context.Background()
	s, pool := newStore(t)
	s.SetSessionPairs([][2]string{{sharedWorkerPrincipal, projectWorkerPrincipal}})
	const session = "implementer-g1"
	if _, err := s.BindSession(ctx, session, sharedWorkerPrincipal, false, time.Now().Add(200*24*time.Hour)); err != nil {
		t.Fatalf("the shared account's claim: %v", err)
	}
	for _, c := range []struct {
		name      string
		principal string
		expires   time.Time
		from      string
	}{
		{name: "the project's account takes the shared account's name", principal: projectWorkerPrincipal, expires: time.Now().Add(time.Hour), from: sharedWorkerPrincipal},
		{name: "and the shared account takes it back", principal: sharedWorkerPrincipal, expires: time.Now().Add(time.Minute), from: projectWorkerPrincipal},
	} {
		taken, err := s.BindSession(ctx, session, c.principal, false, c.expires)
		if err != nil || taken != c.from {
			t.Fatalf("%s: BindSession(%q, %q) = %q, %v, want it taken from %q", c.name, session, c.principal, taken, err, c.from)
		}
		want := c.expires.Add(oauthstore.SessionRetention).UTC()
		if principal, expires, _ := heldBy(t, pool, session); principal != c.principal || expires.Sub(want).Abs() > time.Second {
			t.Errorf("%s: %s is held by %q until %s, want %q until %s, SessionRetention past its own claim and not the later hold it took the name from",
				c.name, session, principal, expires, c.principal, want)
		}
	}
}

// TestAnAccountNotPairedWithTheHolderIsRefused: a claim reads the claimant's own pairs, and every
// pair works both ways, so only an account paired with the holder takes its name. With the shared
// account paired with the project's and the project's with a third, the third is refused the
// shared account's name, since a pair does not chain, and so is an account paired with nobody;
// neither refusal moves the name or its expiry. A store given no pairs, the broker with
// BROKER_SESSION_ACCOUNT_PAIRS unset, refuses even the project's account.
func TestAnAccountNotPairedWithTheHolderIsRefused(t *testing.T) {
	ctx := context.Background()
	s, pool := newStore(t)
	s.SetSessionPairs([][2]string{{sharedWorkerPrincipal, projectWorkerPrincipal}, {projectWorkerPrincipal, thirdWorkerPrincipal}})
	const session = "implementer-g1"
	hour := time.Now().Add(time.Hour)
	if _, err := s.BindSession(ctx, session, sharedWorkerPrincipal, false, hour); err != nil {
		t.Fatalf("the shared account's claim: %v", err)
	}
	_, held, _ := heldBy(t, pool, session)
	refused := func(who, principal string) {
		t.Helper()
		taken, err := s.BindSession(ctx, session, principal, false, hour.Add(time.Hour))
		if !errors.Is(err, oauthstore.ErrSessionHeld) || taken != "" {
			t.Errorf("BindSession(%q, %s) = %q, %v, want ErrSessionHeld", session, who, taken, err)
		} else if !strings.Contains(err.Error(), session) {
			t.Errorf("BindSession(%q, %s) refused with %q, which does not name the session", session, who, err)
		}
		if holder, expires, _ := heldBy(t, pool, session); holder != sharedWorkerPrincipal || !expires.Equal(held) {
			t.Errorf("after %s was refused, %s is held by %q until %s, want %q until %s, as before", who, session, holder, expires, sharedWorkerPrincipal, held)
		}
	}
	refused("an account paired only with the holder's pair", thirdWorkerPrincipal)
	refused("an account paired with nobody", unpairedPrincipal)
	s.SetSessionPairs(nil)
	refused("the project's account, with no pairs given", projectWorkerPrincipal)
}

// TestAPairNeverTakesANameTheConfigurationFixes: a name the configuration fixes goes to its entry
// and to no pair. The account paired with the holder is refused a name held with no expiry,
// whether ReserveSessions or the entry's own fixed claim wrote it and though this replica's
// configuration fixes neither (another replica's did); and it is refused a name this replica's
// configuration fixes (Store.Reserved) that its pair still holds as an ordinary claim. Every row
// keeps its principal and its expiry.
func TestAPairNeverTakesANameTheConfigurationFixes(t *testing.T) {
	ctx := context.Background()
	s, pool := newStore(t)
	s.SetSessionPairs([][2]string{{sharedWorkerPrincipal, projectWorkerPrincipal}})
	hour := time.Now().Add(time.Hour)
	if left, err := s.ReserveSessions(ctx, map[string]string{"legion-daemon:ACME": sharedWorkerPrincipal}); err != nil || len(left) != 0 {
		t.Fatalf("ReserveSessions = %v, %v, want the unheld name reserved", left, err)
	}
	if _, err := s.BindSession(ctx, "ci-release", sharedWorkerPrincipal, true, hour); err != nil {
		t.Fatalf("the entry's fixed claim: %v", err)
	}
	if _, err := s.BindSession(ctx, "legion-daemon:EXAMPLE", sharedWorkerPrincipal, false, hour); err != nil {
		t.Fatalf("the shared account's ordinary claim: %v", err)
	}
	s.Reserved = map[string]string{"legion-daemon:EXAMPLE": daemonPrincipal}
	for _, c := range []struct {
		name    string
		session string
		forever bool
	}{
		{name: "a name ReserveSessions holds with no expiry", session: "legion-daemon:ACME", forever: true},
		{name: "a name a fixed claim holds with no expiry", session: "ci-release", forever: true},
		{name: "a name the configuration fixes for another principal, held as an ordinary claim", session: "legion-daemon:EXAMPLE"},
	} {
		_, before, _ := heldBy(t, pool, c.session)
		if taken, err := s.BindSession(ctx, c.session, projectWorkerPrincipal, false, hour); !errors.Is(err, oauthstore.ErrSessionHeld) || taken != "" {
			t.Errorf("%s: BindSession(%q, the holder's pair) = %q, %v, want ErrSessionHeld", c.name, c.session, taken, err)
		}
		if principal, expires, forever := heldBy(t, pool, c.session); principal != sharedWorkerPrincipal || forever != c.forever || !expires.Equal(before) {
			t.Errorf("%s: after the holder's pair was refused, %s is held by %q until %s (for ever: %t), want %q until %s (for ever: %t), as before",
				c.name, c.session, principal, expires, forever, sharedWorkerPrincipal, before, c.forever)
		}
	}
}

// TestTwoClaimsRacingForAPairsNameEndWithOneHolder: a claim that waits on a name's row behind
// another claim decides on the row that claim committed, not on the one it found before waiting,
// so the two end with one holder. With the shared account paired with the project's account and
// with a third, which are not paired with each other, the project's account takes the shared
// account's name: the third, waiting behind it, is refused, though it would have taken the name
// from the shared account, and the shared account, waiting behind it, takes the name back. Two
// claims racing for an unheld name end the same way: the later takes it only from an account it
// is paired with.
func TestTwoClaimsRacingForAPairsNameEndWithOneHolder(t *testing.T) {
	ctx := context.Background()
	s, pool := newStore(t)
	s.SetSessionPairs([][2]string{{sharedWorkerPrincipal, projectWorkerPrincipal}, {sharedWorkerPrincipal, thirdWorkerPrincipal}})
	for _, c := range []struct {
		name    string
		session string
		held    string
		second  string
		err     error
		taken   string
		holder  string
	}{
		{name: "an account not paired with the taker is refused", session: "implementer-g1", held: sharedWorkerPrincipal,
			second: thirdWorkerPrincipal, err: oauthstore.ErrSessionHeld, holder: projectWorkerPrincipal},
		{name: "the account the name was taken from takes it back", session: "implementer-g2", held: sharedWorkerPrincipal,
			second: sharedWorkerPrincipal, taken: projectWorkerPrincipal, holder: sharedWorkerPrincipal},
		{name: "an unheld name's later claimant paired with the first takes it", session: "implementer-g3",
			second: sharedWorkerPrincipal, taken: projectWorkerPrincipal, holder: sharedWorkerPrincipal},
		{name: "an unheld name's later claimant paired with nobody is refused", session: "implementer-g4",
			second: unpairedPrincipal, err: oauthstore.ErrSessionHeld, holder: projectWorkerPrincipal},
	} {
		t.Run(c.name, func(t *testing.T) {
			if c.held != "" {
				if _, err := s.BindSession(ctx, c.session, c.held, false, time.Now().Add(time.Hour)); err != nil {
					t.Fatalf("the holder's claim: %v", err)
				}
			}
			first, taken, err := raceForName(t, s, pool, c.session, projectWorkerPrincipal, c.second)
			if first != c.held {
				t.Fatalf("the project account's claim answered it took %s from %q, want %q", c.session, first, c.held)
			}
			if !errors.Is(err, c.err) || taken != c.taken {
				t.Fatalf("BindSession(%q, %q), waiting behind the project account's claim = %q, %v, want %q, %v", c.session, c.second, taken, err, c.taken, c.err)
			}
			if holder, _, _ := heldBy(t, pool, c.session); holder != c.holder {
				t.Errorf("after the race, %s is held by %q, want %q", c.session, holder, c.holder)
			}
		})
	}
}

// TestAPairedTakeNamesTheHolderTheUpdateRead: a claim that waited on a name's row behind a pair's
// take answers the holder its own update took the name from, the principal whose take committed
// while it waited, and not the one that held the name when it began. With the shared account
// paired with the project's account and that one with a third, the project's account takes the
// shared account's name, and the third, waiting behind it, takes the name from the project's
// account, which is whom it must name.
func TestAPairedTakeNamesTheHolderTheUpdateRead(t *testing.T) {
	ctx := context.Background()
	s, pool := newStore(t)
	s.SetSessionPairs([][2]string{{sharedWorkerPrincipal, projectWorkerPrincipal}, {projectWorkerPrincipal, thirdWorkerPrincipal}})
	const session = "implementer-g1"
	if _, err := s.BindSession(ctx, session, sharedWorkerPrincipal, false, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("the shared account's claim: %v", err)
	}
	first, taken, err := raceForName(t, s, pool, session, projectWorkerPrincipal, thirdWorkerPrincipal)
	if first != sharedWorkerPrincipal {
		t.Fatalf("the project account's take answered it took %s from %q, want %q", session, first, sharedWorkerPrincipal)
	}
	if err != nil || taken != projectWorkerPrincipal {
		t.Fatalf("the third account's claim, which waited behind the project account's take = %q, %v, want it taken from %q, the holder its update read, not %q, which held the name when the claim began",
			taken, err, projectWorkerPrincipal, sharedWorkerPrincipal)
	}
	if holder, _, _ := heldBy(t, pool, session); holder != thirdWorkerPrincipal {
		t.Errorf("%s is held by %q, want %q", session, holder, thirdWorkerPrincipal)
	}
}

// TestAJWTIdIsAcceptedOnce: the exchange's replay store accepts a jti once and refuses it while
// the token it came from is live. A record whose token has expired is taken over, so the sweep's
// deletions refuse nothing.
func TestAJWTIdIsAcceptedOnce(t *testing.T) {
	ctx := context.Background()
	s, pool := newStore(t)
	exp := time.Now().Add(time.Minute)
	for _, c := range []struct {
		name string
		mark func() error
		want bool
	}{
		{name: "an exchanged subject token is accepted", mark: func() error { return s.SetTokenExchangeCustomJWT(ctx, "jti-1", exp) }},
		{name: "and refused the second time", mark: func() error { return s.SetTokenExchangeCustomJWT(ctx, "jti-1", exp) }, want: true},
		{name: "another jti is its own", mark: func() error { return s.SetTokenExchangeCustomJWT(ctx, "jti-2", exp) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			if err := c.mark(); (err != nil) != c.want {
				t.Fatalf("= %v, want an error: %v", err, c.want)
			}
		})
	}
	if _, err := pool.Exec(ctx, `update oauth_exchange_jtis set expires_at = now() - interval '1 second'`); err != nil {
		t.Fatalf("expire the records: %v", err)
	}
	if err := s.SetTokenExchangeCustomJWT(ctx, "jti-1", exp); err != nil {
		t.Errorf("SetTokenExchangeCustomJWT(an expired record) = %v, want it accepted", err)
	}
}

// TestAnExchangeSessionRoundTrips: an exchanged token's session is stored and read back whole —
// the claims the exchange decided, the subject and actor tokens it decided from, the DPoP key it
// is bound to and its expiry — because an introspection of that token answers from the row.
func TestAnExchangeSessionRoundTrips(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t)
	request := exchangeRequest(t, s, "request-1")
	stored := request.Session.(*oauthstore.ExchangeSession)
	if err := s.CreateAccessTokenSession(ctx, "access-1", request); err != nil {
		t.Fatalf("CreateAccessTokenSession: %v", err)
	}
	read := oauthstore.NewExchangeSession()
	back, err := s.GetAccessTokenSession(ctx, "access-1", read)
	if err != nil {
		t.Fatalf("GetAccessTokenSession: %v", err)
	}
	if back.GetClient().GetID() != oauthwire.ExchangeClientID {
		t.Errorf("the stored request's client is %q, want the exchange client", back.GetClient().GetID())
	}
	for _, c := range []struct {
		name      string
		got, want any
	}{
		{name: "subject", got: read.GetSubject(), want: stored.GetSubject()},
		{name: "username", got: read.GetUsername(), want: stored.GetUsername()},
		{name: "DPoP thumbprint", got: read.GetDPoPJWKThumbprint(), want: stored.GetDPoPJWKThumbprint()},
		{name: "extra claims", got: read.Extra, want: stored.Extra},
		{name: "subject token", got: read.GetSubjectToken(), want: stored.GetSubjectToken()},
		{name: "actor token", got: read.GetActorToken(), want: stored.GetActorToken()},
	} {
		t.Run(c.name, func(t *testing.T) {
			if !reflect.DeepEqual(c.got, c.want) {
				t.Errorf("%s = %#v, want %#v", c.name, c.got, c.want)
			}
		})
	}
	if got, want := read.GetExpiresAt(oauth2.AccessToken), stored.GetExpiresAt(oauth2.AccessToken); !got.Equal(want) {
		t.Errorf("access token expiry = %s, want %s", got, want)
	}
	if clone, ok := read.Clone().(*oauthstore.ExchangeSession); !ok || !reflect.DeepEqual(clone.SubjectToken, read.SubjectToken) {
		t.Errorf("Clone = %#v, want a copy of the exchange session", read.Clone())
	}
}

// TestAnExchangedTokenNamesNoLauncherCredential: an exchanged token follows no machine login, so
// its row holds no credential and no revoke of a credential can end it; a machine login's token
// that has lost its credential is still refused, which is the fault the check was written for.
func TestAnExchangedTokenNamesNoLauncherCredential(t *testing.T) {
	ctx := context.Background()
	s, pool := newStore(t)
	if err := s.CreateAccessTokenSession(ctx, "access-1", exchangeRequest(t, s, "request-1")); err != nil {
		t.Fatalf("CreateAccessTokenSession: %v", err)
	}
	if err := s.CreateRefreshTokenSession(ctx, "refresh-1", "access-1", exchangeRequest(t, s, "request-1")); err != nil {
		t.Fatalf("CreateRefreshTokenSession: %v", err)
	}
	for _, c := range []struct{ name, query string }{
		{name: "access token", query: `select credential_id is null from oauth_access_tokens where signature = 'access-1'`},
		{name: "refresh token", query: `select credential_id is null from oauth_refresh_tokens where signature = 'refresh-1'`},
	} {
		t.Run(c.name, func(t *testing.T) {
			var none bool
			if err := pool.QueryRow(ctx, c.query).Scan(&none); err != nil {
				t.Fatalf("read the row: %v", err)
			}
			if !none {
				t.Errorf("the %s names a launcher credential, want none", c.name)
			}
		})
	}
	if err := s.CreateAccessTokenSession(ctx, "access-2", tokenRequest(t, s, "request-2", "")); err == nil {
		t.Error("a machine login's token session with no credential_id = nil, want an error")
	}
}

// TestATokenSessionIsReadIntoEitherKind: the library reads the subject access token of an
// exchange into the exchange request's own session, so a row written from a machine login's
// *oauth2.DefaultSession must read back into an *ExchangeSession, and a row written from an
// exchange session must read back into a plain one. A third kind is still refused.
func TestATokenSessionIsReadIntoEitherKind(t *testing.T) {
	ctx := context.Background()
	s, pool := newStore(t)
	credentialID := newCredential(t, pool)
	if err := s.CreateAccessTokenSession(ctx, "access-1", tokenRequest(t, s, "request-1", credentialID)); err != nil {
		t.Fatalf("CreateAccessTokenSession: %v", err)
	}
	if err := s.CreateAccessTokenSession(ctx, "access-2", exchangeRequest(t, s, "request-2")); err != nil {
		t.Fatalf("CreateAccessTokenSession: %v", err)
	}
	intoExchange := oauthstore.NewExchangeSession()
	if _, err := s.GetAccessTokenSession(ctx, "access-1", intoExchange); err != nil {
		t.Fatalf("a machine login's token read into an exchange session: %v", err)
	}
	if intoExchange.Extra[oauthwire.ExtraCredentialID] != credentialID {
		t.Errorf("the machine login's session lost its %s: %#v", oauthwire.ExtraCredentialID, intoExchange.Extra)
	}
	intoDefault := &oauth2.DefaultSession{}
	if _, err := s.GetAccessTokenSession(ctx, "access-2", intoDefault); err != nil {
		t.Fatalf("an exchanged token read into a default session: %v", err)
	}
	if intoDefault.Extra["session"] != "implementer-g1" {
		t.Errorf("the exchanged token's claims read back as %#v, want the session it was minted for", intoDefault.Extra)
	}
	if _, err := s.GetAccessTokenSession(ctx, "access-2", &otherSession{}); err == nil {
		t.Error("a read into a third session type = nil, want an error")
	}
}

// TestTheExchangeStorageNeedsItsWiring: the store decides no policy of its own, so each seam the
// provider wiring fills is refused rather than guessed at while it is empty, and answers through
// whatever the wiring set once it is.
func TestTheExchangeStorageNeedsItsWiring(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t)
	if _, err := s.GetSubjectForTokenExchange(ctx, exchangeRequest(t, s, "request-1"), map[string]any{}); err == nil {
		t.Error("GetSubjectForTokenExchange with no decision wired = nil, want an error")
	}
	s.ExchangeSubject = func(context.Context, oauth2.Requester, map[string]any) (string, error) {
		return "ada@example.com", nil
	}
	subject, err := s.GetSubjectForTokenExchange(ctx, exchangeRequest(t, s, "request-1"), map[string]any{})
	if err != nil || subject != "ada@example.com" {
		t.Fatalf("GetSubjectForTokenExchange = (%q, %v), want the decision it was wired to", subject, err)
	}
}
