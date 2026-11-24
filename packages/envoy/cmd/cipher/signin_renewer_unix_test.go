//go:build unix

// packages/envoy/cmd/cipher/signin_renewer_unix_test.go
//
// The sign-in renewer's loop in this process, on a clock of the test's own (signInRenewer's after
// and now) against a broker stand-in: where it stops without a broker answer it can read, how long
// a grant with an end keeps it, a refusal for good, a sign-in leaving the machine, a request the
// broker no longer holds, and the second look at the state that keeps a command's use from being
// lost while the renewer stops. The renewer against the real broker, as the built binary, is
// signin_renewer_linux_test.go's.
package main

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/cipheraddr"
	"github.com/sjawhar/envoy/internal/cipher/leasetime"
	"github.com/sjawhar/envoy/internal/cipher/proof"
	"github.com/sjawhar/envoy/internal/cipherclient"
	"github.com/sjawhar/envoy/internal/testwait"
)

// renewerClock is a clock that moves only when the renewer sleeps on it: each after(d) moves it on
// by d and fires at once. It fails the test past maxSleeps, so a loop that never stops fails rather
// than spins.
type renewerClock struct {
	t      *testing.T
	mu     sync.Mutex
	at     time.Time
	sleeps int
}

const maxSleeps = 100_000

func (c *renewerClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *renewerClock) after(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	c.at = c.at.Add(d)
	c.sleeps++
	sleeps, at := c.sleeps, c.at
	c.mu.Unlock()
	if sleeps > maxSleeps {
		c.t.Fatalf("the renewer slept %d times, to %s, and never stopped", sleeps, at)
	}
	fired := make(chan time.Time, 1)
	fired <- at
	return fired
}

// unitRenewer is a renewer of ada@example.com's sign-in, whose record names enrollment-1, in a
// store of the test's own, against the broker broker serves, on clock. Its trade answers a
// credential that lasts a day, as a machine that trades without fault, and a test of the trade
// replaces it. Its renewal directory is not made: the renewer makes nothing (keptSignIn.prepare
// is a command's).
func unitRenewer(t *testing.T, clock *renewerClock, broker http.Handler) (*signInRenewer, *strings.Builder) {
	t.Helper()
	server := httptest.NewServer(broker)
	t.Cleanup(server.Close)
	key, err := proof.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	thumbprint, err := proof.Thumbprint(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	store := cipherclient.SignInStore{Dir: t.TempDir()}
	const person = "ada@example.com"
	if err := store.Write(cipherclient.SignInRecord{
		Person: person, Issuer: "https://accounts.google.com", ClientID: "device-client", RefreshToken: "not-a-secret-refresh-token",
		BoxKey: string(keyPEM(key)), Thumbprint: thumbprint, EnrollmentID: "enrollment-1", SignedInAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	var log strings.Builder
	return &signInRenewer{
		renewal: keptSignIn{person: person, record: store.Path(person), dir: store.RenewalDir(person)},
		store:   store, broker: newClient(cipheraddr.New(server.URL)), stderr: &log,
		trade: func(context.Context, cipherclient.SignInRecord, *ecdsa.PrivateKey) (time.Time, error) {
			return clock.now().Add(24 * time.Hour), nil
		},
		after: clock.after, now: clock.now, release: func() {},
	}, &log
}

// TestARenewerKeepsItsSignInsCredentialLive: the broker renews a sign-in's session only while
// its credential is live, and the credential lasts an hour from each trade. The stand-in broker
// refuses a renewal, 401 ENROLLMENT_NOT_LIVE, once the credential's expiry has passed. Rows:
// every trade goes through, so the renewer keeps the enrollment for the five hours a grant keeps
// it, trading at its start and each time a third of the credential's life has passed; every
// trade after the first fails, as for a person whose Google account was suspended, so the
// credential lapses within its hour after several tries, the broker refuses the renewal, and the
// renewer exits 76; and the machine is told of no sign-in client, so it tries once, says so, and
// the enrollment ends at the expiry its registration gave it.
func TestARenewerKeepsItsSignInsCredentialLive(t *testing.T) {
	const lease, life = 15 * time.Minute, time.Hour
	for _, row := range []struct {
		name                  string
		trade                 func(n int) error
		wantCode              int
		wantLog               string
		ranAtLeast, ranAtMost time.Duration
		tradesAtLeast         int
		tradesAtMost          int
	}{
		{name: "every trade goes through", wantCode: 0, wantLog: "stopped: nothing has used it",
			trade:      func(int) error { return nil },
			ranAtLeast: 5 * time.Hour, ranAtMost: 5*time.Hour + lease, tradesAtLeast: 14, tradesAtMost: 17},
		{name: "every trade after the first fails", wantCode: exitRenewRefused, wantLog: "the trade of the person's identity failed",
			trade: func(n int) error {
				if n == 1 {
					return nil
				}
				return errors.New("renew the sign-in at https://accounts.google.com: invalid_grant")
			},
			ranAtLeast: life, ranAtMost: life + lease/3 + time.Minute, tradesAtLeast: 9, tradesAtMost: 20},
		{name: "no sign-in client", wantCode: exitRenewRefused, wantLog: "this machine cannot trade the person's identity",
			trade: func(int) error {
				return &unrenewableSignIn{reason: "this machine is told of no sign-in client", why: "no sign-in client is set"}
			},
			ranAtLeast: life, ranAtMost: life + lease/3 + time.Minute, tradesAtLeast: 1, tradesAtMost: 1},
	} {
		t.Run(row.name, func(t *testing.T) {
			clock := &renewerClock{t: t, at: time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)}
			start := clock.now()
			var mu sync.Mutex
			// expires is the credential's expiry at the broker: the registration's, an hour from
			// the start, and moved by each trade that goes through.
			expires, trades, refusals := start.Add(life), 0, 0
			mux := http.NewServeMux()
			mux.HandleFunc("POST /v1/enrollments/enrollment-1/renew", func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				lapsed := !clock.now().Before(expires)
				if lapsed {
					refusals++
				}
				mu.Unlock()
				if lapsed {
					notLive(w, r)
					return
				}
				renewsFor(clock, lease)(w, r)
			})
			mux.HandleFunc("GET /v1/enrollments/self", func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, map[string]any{"enrollment_id": "enrollment-1", "kind": "session", "lease_expires_at": clock.now().Add(lease),
					"grants": []map[string]any{{"grant_id": "grant-1", "request_id": "request-1", "expires_at": start.Add(5 * time.Hour)}}})
			})
			renewer, log := unitRenewer(t, clock, mux)
			renewer.trade = func(context.Context, cipherclient.SignInRecord, *ecdsa.PrivateKey) (time.Time, error) {
				mu.Lock()
				defer mu.Unlock()
				trades++
				if err := row.trade(trades); err != nil {
					return time.Time{}, err
				}
				expires = clock.now().Add(life)
				return expires, nil
			}
			holdLock(t, renewer)
			// No command used the sign-in after the start: the grant alone keeps the enrollment.
			if err := os.Chtimes(renewer.renewal.path(renewalUsedFile), start, start); err != nil {
				t.Fatal(err)
			}
			if code := runSignInRenewer(context.Background(), renewer); code != row.wantCode {
				t.Fatalf("run = %d, want %d (log %q)", code, row.wantCode, log.String())
			}
			if ran := clock.now().Sub(start); ran < row.ranAtLeast || ran > row.ranAtMost {
				t.Fatalf("the renewer stopped after %s, want from %s to %s (log %q)", ran, row.ranAtLeast, row.ranAtMost, log.String())
			}
			if trades < row.tradesAtLeast || trades > row.tradesAtMost {
				t.Fatalf("the renewer traded %d times, want from %d to %d (log %q)", trades, row.tradesAtLeast, row.tradesAtMost, log.String())
			}
			if row.wantCode == 0 && refusals != 0 {
				t.Fatalf("the broker refused %d renewals for a lapsed credential; want none while every trade goes through (log %q)", refusals, log.String())
			}
			if !strings.Contains(log.String(), row.wantLog) {
				t.Fatalf("the log %q; want %q", log.String(), row.wantLog)
			}
		})
	}
}

// renewsFor answers a renewal with a lease of lease from clock's now, as the broker does.
func renewsFor(clock *renewerClock, lease time.Duration) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"lease_expires_at": clock.now().Add(lease)})
	}
}

// unavailable answers 503, a failure a retry may cure.
func unavailable(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = w.Write([]byte(`{"code":"UNAVAILABLE","error":"the broker is not serving"}`))
}

// TestARenewerThatCannotRenewStopsAtTheBlindLimit: a renewer whose renewals fail retries at renew's
// backoff and stops once none got through for the blind limit, rather than retry forever on a
// machine whose broker went away. Rows: every renewal fails, so it stops 30 minutes on; the first
// renewal granted an hour's lease, so it stops two leases after that renewal, not 30 minutes; and a
// command used the sign-in as the limit came, so the stop sees the use and the renewer tries once
// more before it stops.
func TestARenewerThatCannotRenewStopsAtTheBlindLimit(t *testing.T) {
	for _, row := range []struct {
		name         string
		firstLease   time.Duration // the first renewal's lease; 0 where it fails too
		useAtLimit   bool
		wantStop     string
		ranAtLeast   time.Duration
		ranAtMost    time.Duration
		triesAfterUs int
	}{
		{name: "every renewal fails", wantStop: "stopped: no renewal of enrollment enrollment-1 got through for 30m", ranAtLeast: renewerBlindLimit, ranAtMost: renewerBlindLimit + time.Minute},
		{name: "a lease of an hour", firstLease: time.Hour, wantStop: "stopped: no renewal of enrollment enrollment-1 got through for 2h0", ranAtLeast: 2 * time.Hour, ranAtMost: 2*time.Hour + time.Minute},
		{name: "a use as the limit comes", useAtLimit: true, wantStop: "stopped: no renewal of enrollment enrollment-1 got through for 30m", ranAtLeast: renewerBlindLimit, ranAtMost: renewerBlindLimit + time.Minute, triesAfterUs: 1},
	} {
		t.Run(row.name, func(t *testing.T) {
			clock := &renewerClock{t: t, at: time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)}
			start := clock.now()
			var renewer *signInRenewer
			tries, triesAfterUse, used := 0, 0, false
			renewer, log := unitRenewer(t, clock, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				tries++
				if used {
					triesAfterUse++
				}
				if tries == 1 && row.firstLease > 0 {
					renewsFor(clock, row.firstLease)(w, r)
					return
				}
				if row.useAtLimit && !used && clock.now().Sub(start) >= renewerBlindLimit {
					used = true
					later := time.Now().Add(time.Minute)
					_ = os.Chtimes(renewer.renewal.path(renewalUsedFile), later, later)
				}
				unavailable(w, r)
			}))
			holdLock(t, renewer)
			if err := os.Chtimes(renewer.renewal.path(renewalUsedFile), time.Unix(0, 0), time.Unix(0, 0)); err != nil {
				t.Fatal(err)
			}
			if code := runSignInRenewer(context.Background(), renewer); code != 1 {
				t.Fatalf("run = %d, want 1 (log %q)", code, log.String())
			}
			if ran := clock.now().Sub(start); ran < row.ranAtLeast || ran > row.ranAtMost {
				t.Fatalf("the renewer stopped after %s, want from %s to %s (log %q)", ran, row.ranAtLeast, row.ranAtMost, log.String())
			}
			if !strings.Contains(log.String(), row.wantStop) {
				t.Fatalf("the log %q; want %q", log.String(), row.wantStop)
			}
			if triesAfterUse != row.triesAfterUs {
				t.Fatalf("the renewer tried %d times after the use; want %d: a stop reads the state again and goes on for a use it did not see (log %q)", triesAfterUse, row.triesAfterUs, log.String())
			}
		})
	}
}

// TestARetriedRenewalAsksTheBrokerNothingElse: whether the machine needs the enrollment is asked
// once a cycle, so a failed renewal is retried at once at renew's backoff, without the broker reads
// first; each of those reads may take a third of the lease. Here each renewal after the first fails
// three times before it goes through, and a grant with an end 25 minutes on keeps the enrollment.
func TestARetriedRenewalAsksTheBrokerNothingElse(t *testing.T) {
	const lease = 15 * time.Minute
	clock := &renewerClock{t: t, at: time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)}
	start := clock.now()
	var mu sync.Mutex
	attempts, renewed, selfReads := 0, 0, 0
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/enrollments/enrollment-1/renew", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		attempts++
		fail := attempts > 1 && attempts%4 != 1
		if !fail {
			renewed++
		}
		mu.Unlock()
		if fail {
			unavailable(w, r)
			return
		}
		renewsFor(clock, lease)(w, r)
	})
	mux.HandleFunc("GET /v1/enrollments/self", func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		selfReads++
		mu.Unlock()
		writeJSON(w, map[string]any{"enrollment_id": "enrollment-1", "kind": "session", "lease_expires_at": clock.now().Add(lease),
			"grants": []map[string]any{{"grant_id": "grant-1", "request_id": "request-1", "expires_at": start.Add(25 * time.Minute)}}})
	})
	renewer, log := unitRenewer(t, clock, mux)
	if code := runSignInRenewer(context.Background(), renewer); code != 0 {
		t.Fatalf("run = %d, want 0 once the grant ended (log %q)", code, log.String())
	}
	// One read for each cycle after the first: each renewal that went through but the first, and
	// the look that stopped the renewer.
	if renewed < 3 || selfReads != renewed {
		t.Fatalf("%d renewals went through, of %d attempts, and the broker answered %d reads of the grants; want a read for each renewal after the first and one to stop, %d (log %q)",
			renewed, attempts, selfReads, renewed, log.String())
	}
}

// TestARenewerStopsByWhatTheBrokerSaysOnceUseIsOld: once no command used the sign-in for a lease,
// the renewer asks the broker. Rows: no grant and no request, so it stops at once; a grant with no
// end, which lasts until its session ends, so it stops at once too; a grant with an end two hours
// on, so it renews until that end and then stops; and a broker whose answer it cannot read, so it
// renews for renewerBlindLimit and then stops.
func TestARenewerStopsByWhatTheBrokerSaysOnceUseIsOld(t *testing.T) {
	const lease = 15 * time.Minute
	rows := []struct {
		name       string
		grantFor   time.Duration // a grant whose end is this far from the start; 0 for none, -1 for one with no end
		selfFails  bool
		wantCode   int
		wantStop   string
		ranAtLeast time.Duration
		ranAtMost  time.Duration
	}{
		{name: "nothing live", wantCode: 0, wantStop: "stopped: nothing has used it for 15m0s", ranAtMost: lease},
		{name: "a grant with no end", grantFor: -1, wantCode: 0, wantStop: "stopped: nothing has used it for 15m0s", ranAtMost: lease},
		{name: "a grant with an end", grantFor: 2 * time.Hour, wantCode: 0, wantStop: "stopped: nothing has used it for 15m0s", ranAtLeast: 2 * time.Hour, ranAtMost: 2*time.Hour + lease},
		{name: "an answer it cannot read", selfFails: true, wantCode: 1, wantStop: "stopped: for 30m0s no answer of the broker said whether anything needs enrollment enrollment-1", ranAtLeast: renewerBlindLimit, ranAtMost: renewerBlindLimit + lease},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			clock := &renewerClock{t: t, at: time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)}
			start := clock.now()
			mux := http.NewServeMux()
			mux.HandleFunc("POST /v1/enrollments/enrollment-1/renew", renewsFor(clock, lease))
			mux.HandleFunc("GET /v1/enrollments/self", func(w http.ResponseWriter, r *http.Request) {
				if row.selfFails {
					unavailable(w, r)
					return
				}
				grants := []map[string]any{}
				switch {
				case row.grantFor < 0:
					grants = append(grants, map[string]any{"grant_id": "grant-1", "request_id": "request-1", "expires_at": nil})
				case row.grantFor > 0:
					grants = append(grants, map[string]any{"grant_id": "grant-1", "request_id": "request-1", "expires_at": start.Add(row.grantFor)})
				}
				writeJSON(w, map[string]any{"enrollment_id": "enrollment-1", "kind": "session", "lease_expires_at": clock.now().Add(lease), "grants": grants})
			})
			renewer, log := unitRenewer(t, clock, mux)
			if code := runSignInRenewer(context.Background(), renewer); code != row.wantCode {
				t.Fatalf("run = %d, want %d (log %q)", code, row.wantCode, log.String())
			}
			if ran := clock.now().Sub(start); ran < row.ranAtLeast || ran > row.ranAtMost {
				t.Fatalf("the renewer stopped after %s, want from %s to %s (log %q)", ran, row.ranAtLeast, row.ranAtMost, log.String())
			}
			if !strings.Contains(log.String(), row.wantStop) {
				t.Fatalf("the log %q; want %q", log.String(), row.wantStop)
			}
		})
	}
}

// TestASlowBrokerReadDoesNotHoldTheRenewal: a broker that holds the renewer's read of its grants
// unanswered costs each renewal a third of the lease at most, in real time, so the renewal still
// goes out before the lease lapses, and goes on going out while the read hangs. The lease here is
// 1.5 s, so the reads give up after 500 ms; the renewer's own sleeps are on the test's clock and
// take no real time, so each gap between two renewals is that read alone.
func TestASlowBrokerReadDoesNotHoldTheRenewal(t *testing.T) {
	const lease = 1500 * time.Millisecond
	// margin is what a loaded machine under -race adds to a gap: the reads' own calls and the
	// loop. A read bounded by the whole lease takes 1.5 s, three times the bound.
	const margin = 400 * time.Millisecond
	clock := &renewerClock{t: t, at: time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)}
	var mu sync.Mutex
	var renewals []time.Time
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/enrollments/enrollment-1/renew", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		renewals = append(renewals, time.Now())
		mu.Unlock()
		renewsFor(clock, lease)(w, r)
	})
	mux.HandleFunc("GET /v1/enrollments/self", func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
	renewer, log := unitRenewer(t, clock, mux)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() { done <- runSignInRenewer(ctx, renewer) }()
	testwait.WithinPolling(t, 10*time.Second, 10*time.Millisecond, "four renewals while the grants read hangs", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(renewals) >= 4
	})
	cancel()
	if code := testwait.Receive(t, "the renewer to end once cancelled", done); code != 0 {
		t.Fatalf("run = %d, want 0 once cancelled (log %q)", code, log.String())
	}
	mu.Lock()
	defer mu.Unlock()
	for i := 1; i < len(renewals); i++ {
		if gap := renewals[i].Sub(renewals[i-1]); gap > leasetime.RenewAfter(lease)+margin {
			t.Fatalf("renewal %d came %s after the one before; want a third of the lease, %s, and %s at most", i+1, gap, leasetime.RenewAfter(lease), margin)
		}
	}
}

// TestAStoppingRenewerTakesTheLockBackForWhatItDidNotSee: a renewer that decided to stop lets its
// lock go and reads the state again. A use a command wrote after the renewer read the state, a
// request it recorded, or another enrollment it registered and wrote to the record, makes the
// renewer take the lock back and go on, since that command found the lock held and started no
// renewer. With nothing new, the renewer stops and the lock is free.
func TestAStoppingRenewerTakesTheLockBackForWhatItDidNotSee(t *testing.T) {
	rows := []struct {
		name     string
		write    func(t *testing.T, r *signInRenewer)
		wantGoOn bool
	}{
		{name: "nothing new", write: func(*testing.T, *signInRenewer) {}},
		{name: "a later use", wantGoOn: true, write: func(t *testing.T, r *signInRenewer) {
			later := time.Now().Add(time.Minute)
			if err := os.Chtimes(r.renewal.path(renewalUsedFile), later, later); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "a new pending request", wantGoOn: true, write: func(t *testing.T, r *signInRenewer) {
			if err := os.WriteFile(r.renewal.path(renewalPendingDir)+"/request-2", nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "another enrollment", wantGoOn: true, write: func(t *testing.T, r *signInRenewer) {
			rewriteEnrollment(t, r, "enrollment-2")
		}},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			clock := &renewerClock{t: t, at: time.Now()}
			renewer, _ := unitRenewer(t, clock, http.NotFoundHandler())
			holdLock(t, renewer)
			decided := renewer.state()
			row.write(t, renewer)

			goesOn := renewer.stop(decided.movedOn)
			defer renewer.release()
			if goesOn != row.wantGoOn {
				t.Fatalf("stop = go on %v, want %v", goesOn, row.wantGoOn)
			}
			probe, held, err := tryRenewerLock(renewer.renewal.path(renewalLockFile), false)
			if err != nil {
				t.Fatal(err)
			}
			if probe != nil {
				probe()
			}
			if held != row.wantGoOn {
				t.Fatalf("after stop the lock is held %v, want %v: the renewer holds it exactly when it goes on", held, row.wantGoOn)
			}
		})
	}
}

// holdLock makes r's renewal directory as a command does, and has r hold its lock as a renewer
// does.
func holdLock(t *testing.T, r *signInRenewer) {
	t.Helper()
	if err := r.renewal.prepare(); err != nil {
		t.Fatal(err)
	}
	release, held, err := tryRenewerLock(r.renewal.path(renewalLockFile), true)
	if err != nil || held {
		t.Fatalf("take the renewer's lock: held %v, %v", held, err)
	}
	r.release = release
}

// rewriteEnrollment writes r's sign-in record again naming enrollmentID, as a command that
// registered the next enrollment does.
func rewriteEnrollment(t *testing.T, r *signInRenewer, enrollmentID string) {
	t.Helper()
	record, err := r.store.Read(r.renewal.record)
	if err != nil {
		t.Fatal(err)
	}
	record.EnrollmentID = enrollmentID
	if err := r.store.Write(record); err != nil {
		t.Fatal(err)
	}
}

// notLive answers a renewal as the broker answers one of an enrollment it ended: 401
// ENROLLMENT_NOT_LIVE, a refusal for good.
func notLive(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte(`{"code":"ENROLLMENT_NOT_LIVE","error":"enrollment not live"}`))
}

// TestARenewerRefusedForGoodRenewsThatEnrollmentOnce: the broker refuses enrollment-1 for good,
// as after a person revoked the sign-in. Rows: a command uses the sign-in between the renewer's
// decision and its stop, so the stop sees a later use; the renewer still sends the refused
// enrollment once and exits 76, since that command registers the next enrollment itself. And a
// command registered enrollment-2 meanwhile and wrote it to the record: the renewer renews that
// one. The refused enrollment is recorded in the lock file, so a renewer started later sends it
// no more, and no command starts one for it.
func TestARenewerRefusedForGoodRenewsThatEnrollmentOnce(t *testing.T) {
	t.Run("a use meanwhile", func(t *testing.T) {
		clock := &renewerClock{t: t, at: time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)}
		var renewer *signInRenewer
		var mu sync.Mutex
		refusals := 0
		mux := http.NewServeMux()
		mux.HandleFunc("POST /v1/enrollments/enrollment-1/renew", func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			refusals++
			n := refusals
			mu.Unlock()
			if n <= 3 {
				// A command used the sign-in after the renewer read the state; bounded, so a
				// renewer that goes on for it ends after three rather than spin.
				later := time.Now().Add(time.Duration(n) * time.Minute)
				_ = os.Chtimes(renewer.renewal.path(renewalUsedFile), later, later)
			}
			notLive(w, r)
		})
		renewer, log := unitRenewer(t, clock, mux)
		holdLock(t, renewer)
		if code := runSignInRenewer(context.Background(), renewer); code != exitRenewRefused {
			t.Fatalf("run = %d, want %d (log %q)", code, exitRenewRefused, log.String())
		}
		if refusals != 1 {
			t.Fatalf("the renewer sent the refused enrollment %d times; want once (log %q)", refusals, log.String())
		}
		if got := renewer.renewal.refused(); got != "enrollment-1" {
			t.Fatalf("the lock file records %q as refused; want enrollment-1", got)
		}

		// A renewer started after it sends the refused enrollment nothing.
		later, laterLog := unitRenewer(t, clock, mux)
		later.renewal, later.store = renewer.renewal, renewer.store
		holdLock(t, later)
		if code := runSignInRenewer(context.Background(), later); code != exitRenewRefused || refusals != 1 {
			t.Fatalf("a later renewer: run = %d after %d refusals; want %d with none more (log %q)", code, refusals, exitRenewRefused, laterLog.String())
		}
		// And no command starts one for it; one for the next enrollment, it does.
		launches := 0
		launch := launchRenewer
		launchRenewer = func(keptSignIn) error { launches++; return nil }
		defer func() { launchRenewer = launch }()
		if err := renewer.renewal.ensureRenewer(); err != nil || launches != 0 {
			t.Fatalf("ensureRenewer for the refused enrollment: %v, %d launches; want none", err, launches)
		}
		rewriteEnrollment(t, renewer, "enrollment-2")
		if err := renewer.renewal.ensureRenewer(); err != nil || launches != 1 {
			t.Fatalf("ensureRenewer for the next enrollment: %v, %d launches; want one", err, launches)
		}
	})
	t.Run("the next enrollment meanwhile", func(t *testing.T) {
		clock := &renewerClock{t: t, at: time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)}
		var renewer *signInRenewer
		var mu sync.Mutex
		renewed := map[string]int{}
		mux := http.NewServeMux()
		mux.HandleFunc("POST /v1/enrollments/enrollment-1/renew", func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			renewed["enrollment-1"]++
			mu.Unlock()
			rewriteEnrollment(t, renewer, "enrollment-2")
			notLive(w, r)
		})
		mux.HandleFunc("POST /v1/enrollments/enrollment-2/renew", func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			renewed["enrollment-2"]++
			mu.Unlock()
			renewsFor(clock, 15*time.Minute)(w, r)
		})
		mux.HandleFunc("GET /v1/enrollments/self", func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, map[string]any{"enrollment_id": "enrollment-2", "kind": "session", "lease_expires_at": clock.now().Add(15 * time.Minute), "grants": []any{}})
		})
		renewer, log := unitRenewer(t, clock, mux)
		holdLock(t, renewer)
		if code := runSignInRenewer(context.Background(), renewer); code != 0 {
			t.Fatalf("run = %d, want 0 once nothing needs enrollment-2 (log %q)", code, log.String())
		}
		if renewed["enrollment-1"] != 1 || renewed["enrollment-2"] < 1 {
			t.Fatalf("renewals %v; want enrollment-1 once and then enrollment-2 (log %q)", renewed, log.String())
		}
	})
}

// TestARenewerStopsWithinItsPollOfTheSignInLeavingTheMachine: a sign-in taken off the machine
// stops its renewer within renewerPoll, not a third of the lease (5 minutes here).
func TestARenewerStopsWithinItsPollOfTheSignInLeavingTheMachine(t *testing.T) {
	clock := &renewerClock{t: t, at: time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)}
	var renewer *signInRenewer
	var removedAt time.Time
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/enrollments/enrollment-1/renew", func(w http.ResponseWriter, r *http.Request) {
		renewsFor(clock, 15*time.Minute)(w, r)
		removedAt = clock.now()
		_ = os.Remove(renewer.renewal.record)
	})
	renewer, log := unitRenewer(t, clock, mux)
	if code := runSignInRenewer(context.Background(), renewer); code != 0 {
		t.Fatalf("run = %d, want 0 (log %q)", code, log.String())
	}
	if took := clock.now().Sub(removedAt); took > renewerPoll {
		t.Fatalf("the renewer stopped %s after the record went; want %s at most (log %q)", took, renewerPoll, log.String())
	}
	if !strings.Contains(log.String(), "stopped: the sign-in is no longer on this machine") {
		t.Fatalf("the log %q; want the line that says the sign-in is gone", log.String())
	}
}

// TestARequestTheBrokerNoLongerHoldsIsForgotten: a recorded request the broker answers 404 (it
// holds none such) or 403 (not this session's), or answers decided, keeps the enrollment no
// longer: its file is removed, and with nothing else live the renewer stops at its first look.
func TestARequestTheBrokerNoLongerHoldsIsForgotten(t *testing.T) {
	const lease = 15 * time.Minute
	for _, row := range []struct {
		name   string
		answer http.HandlerFunc
	}{
		{name: "404", answer: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"code":"NOT_FOUND","error":"no such request"}`))
		}},
		{name: "403", answer: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"code":"FORBIDDEN","error":"not this session's request"}`))
		}},
		{name: "decided", answer: func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, map[string]any{"request_id": "request-1", "state": "denied"})
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			clock := &renewerClock{t: t, at: time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)}
			start := clock.now()
			mux := http.NewServeMux()
			mux.HandleFunc("POST /v1/enrollments/enrollment-1/renew", renewsFor(clock, lease))
			mux.HandleFunc("GET /v1/requests/request-1", row.answer)
			mux.HandleFunc("GET /v1/enrollments/self", func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, map[string]any{"enrollment_id": "enrollment-1", "kind": "session", "lease_expires_at": clock.now().Add(lease), "grants": []any{}})
			})
			renewer, log := unitRenewer(t, clock, mux)
			holdLock(t, renewer)
			// No command used the sign-in: only the recorded request could keep it.
			if err := os.Chtimes(renewer.renewal.path(renewalUsedFile), time.Unix(0, 0), time.Unix(0, 0)); err != nil {
				t.Fatal(err)
			}
			pending := filepath.Join(renewer.renewal.path(renewalPendingDir), "request-1")
			if err := os.WriteFile(pending, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			if code := runSignInRenewer(context.Background(), renewer); code != 0 {
				t.Fatalf("run = %d, want 0 (log %q)", code, log.String())
			}
			if ran := clock.now().Sub(start); ran > lease {
				t.Fatalf("the renewer stopped after %s, want at its first look, within %s (log %q)", ran, lease, log.String())
			}
			if _, err := os.Stat(pending); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("the request's file: %v; want it removed", err)
			}
		})
	}
}

// TestRenewerEnvironmentKeepsOnlyWhatItReads: the renewer keeps each AGENT_SECRETS_ variable and
// the listed ones, the two that say where this machine's own identity is found for the trade
// among them, names the person it renews for, and drops the rest, the stage marker and any other
// person's name among them.
func TestRenewerEnvironmentKeepsOnlyWhatItReads(t *testing.T) {
	got := renewerEnv([]string{
		"HOME=/home/ada",
		"AGENT_SECRETS_URL=https://broker.example",
		"HTTPS_PROXY=http://proxy.example:3128",
		"GOOGLE_APPLICATION_CREDENTIALS=/etc/agent-secrets/credentials.json",
		"GCE_METADATA_HOST=metadata.example",
		"EDITOR=vi",
		cipherclient.PersonVar + "=bob@example.com",
		renewerStageVar + "=" + renewerStageDetach,
	}, "ada@example.com")
	want := []string{
		"HOME=/home/ada",
		"AGENT_SECRETS_URL=https://broker.example",
		"HTTPS_PROXY=http://proxy.example:3128",
		"GOOGLE_APPLICATION_CREDENTIALS=/etc/agent-secrets/credentials.json",
		"GCE_METADATA_HOST=metadata.example",
		cipherclient.PersonVar + "=ada@example.com",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("renewerEnv = %q, want %q", got, want)
	}
}
