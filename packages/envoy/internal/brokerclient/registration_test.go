// packages/envoy/internal/brokerclient/registration_test.go
//
// A pod's trade can reach the broker before the registration that lets that pod write under its
// session name does: its daemon registers at the claim, a session's watch at the pod's creation,
// and either registrar can be down for minutes. The broker refuses that trade invalid_grant with
// an error_description beginning pod_session_not_registered:, and that one refusal is never final:
// the renewal loop trades again, 15 s doubling to 5 minutes, for as long as its caller runs, and
// says so once, naming the session, and again every 30 minutes while it lasts. Every other
// refusal stays as final as it was.

package brokerclient

import (
	"context"
	"log/slog"
	"net/url"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/brokeraddr"
	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/redact"
)

// registeredSession is the session name a pod of these tests trades under, and landedToken the
// access token the stub answers once the session is registered.
const (
	registeredSession = "implementer-g1"
	landedToken       = "token-once-registered"
)

// unregisteredAnswer is the broker's refusal of a pod whose session no registration names yet,
// in the broker's own words (internal/broker/oauth, registeredPodSession).
func unregisteredAnswer(session string) (int, map[string]any) {
	return 400, map[string]any{
		"error": "invalid_grant",
		"error_description": "pod_session_not_registered: No registration says this pod may write under the session name '" +
			session + "'. A pod writes under a session name its deployment registered for it.",
	}
}

// waitingClock is the client's clock and its waits in one: every wait the loop asks for passes at
// once and moves the clock by its length, and is recorded, so a loop that waits for hours runs at
// the test's speed. Once stop has ended a wait never passes, so the loop ends on its context
// rather than trading again.
type waitingClock struct {
	now   time.Time
	waits []time.Duration
	stop  context.Context
}

func (c *waitingClock) Now() time.Time { return c.now }

func (c *waitingClock) After(d time.Duration) <-chan time.Time {
	if c.stop.Err() != nil {
		return nil
	}
	c.waits = append(c.waits, d)
	c.now = c.now.Add(d)
	passed := make(chan time.Time, 1)
	passed <- c.now
	return passed
}

// waitForRegistration runs a pod's renewal loop against a broker that refuses its session
// refusals times as unregistered and then answers it, and reports the token the loop handed its
// caller, the error it ended with, every trade the broker saw, the waits between them, and what
// the loop logged.
func waitForRegistration(t *testing.T, refusals int) (*Token, error, []stubRequest, []time.Duration, []slog.Record) {
	t.Helper()
	broker := newStubBroker(t, func(n int, _ url.Values) (int, map[string]any) {
		if n < refusals {
			return unregisteredAnswer(registeredSession)
		}
		return 200, map[string]any{"access_token": landedToken, "token_type": "DPoP", "expires_in": 3600}
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	clock := &waitingClock{now: time.Date(2026, 11, 16, 4, 0, 0, 0, time.UTC), stop: ctx}
	logged := &logtest.Recorder{}
	client, err := New(Config{
		BrokerURL: brokeraddr.New(broker.URL()), Key: testKey(t), Subject: projectedStub(), Session: registeredSession,
		Log: slog.New(logged), Now: clock.Now, After: clock.After,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	var landed *Token
	err = client.Run(ctx, func(_ context.Context, token *Token) error {
		landed = token
		cancel()
		return nil
	})
	return landed, err, broker.Requests(), clock.waits, logged.Records()
}

// unregisteredLines is the records that say the broker has no registration for the session yet.
func unregisteredLines(records []slog.Record) []slog.Record {
	var lines []slog.Record
	for _, record := range records {
		session := ""
		record.Attrs(func(attr slog.Attr) bool {
			if attr.Key == "session" {
				session = attr.Value.String()
			}
			return true
		})
		if session == registeredSession && record.Message == unregisteredLogLine {
			lines = append(lines, record)
		}
	}
	return lines
}

// unregisteredLogLine is what the loop logs while the broker has no registration for its session.
const unregisteredLogLine = "the secrets broker has no registration for this pod's session yet; trading again until it lands"

// TestAPodTradesOnceItsSessionIsRegistered: the broker refuses the pod's session seven times as
// unregistered, across more than five minutes of the loop's own clock, and then answers it. The
// loop trades again after each refusal, 15 s doubling to its 5-minute cap, hands its caller the
// token the eighth trade lands, and logs the wait once, naming the session.
func TestAPodTradesOnceItsSessionIsRegistered(t *testing.T) {
	const refusals = 7
	landed, err, trades, waits, records := waitForRegistration(t, refusals)
	if err != nil || landed == nil || landed.AccessToken != landedToken {
		t.Fatalf("Run = %v and handed its caller %+v; want the pod's session %s traded once its registration landed, after %d refusals",
			err, landed, registeredSession, refusals)
	}
	if len(trades) != refusals+1 {
		t.Fatalf("the broker saw %d trades of the session %s, want %d: each refusal and the trade that landed", len(trades), registeredSession, refusals+1)
	}
	for i, trade := range trades {
		if got := trade.Form.Get("session"); got != registeredSession {
			t.Errorf("trade %d names the session %q, want %s", i, got, registeredSession)
		}
		if trade.Form.Has("projects") {
			t.Errorf("trade %d sends projects %q: a pod's trade names none, its entry deciding its project", i, trade.Form.Get("projects"))
		}
	}
	want := []time.Duration{15 * time.Second, 30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute, 5 * time.Minute, 5 * time.Minute}
	if !slices.Equal(waits, want) {
		t.Errorf("the loop waited %v between the trades of the session %s, want %v: 15 s doubling to a cap of 5 minutes", waits, registeredSession, want)
	}
	var spanned time.Duration
	for _, wait := range waits {
		spanned += wait
	}
	if spanned <= 5*time.Minute {
		t.Errorf("the refusals of the session %s spanned %s, want more than 5 minutes: the case a client that gave up after 5 minutes would lose", registeredSession, spanned)
	}
	if lines := unregisteredLines(records); len(lines) != 1 {
		t.Errorf("the loop logged %d lines naming the session %s as unregistered across %d refusals, want exactly one within half an hour", len(lines), registeredSession, refusals)
	}
}

// registeredLogLine is what the loop logs once, when a trade lands after it has said the broker
// had no registration for its session: the line that ends the wait, as the TypeScript client
// logs one too (packages/broker-client/src/token-client.ts).
const registeredLogLine = "the secrets broker answered this pod's session once it was registered"

// TestTheEndOfAWaitForARegistrationIsLoggedOnce: the wait is said once when it starts and once
// when the token lands, naming the session and how long it waited, and nothing else is said of it.
func TestTheEndOfAWaitForARegistrationIsLoggedOnce(t *testing.T) {
	const refusals = 3
	landed, err, _, _, records := waitForRegistration(t, refusals)
	if err != nil || landed == nil {
		t.Fatalf("the trade after the registration did not land: token %v, error %v", landed, err)
	}
	var ended []slog.Record
	for _, record := range records {
		if record.Message == registeredLogLine {
			ended = append(ended, record)
		}
	}
	if len(ended) != 1 {
		t.Fatalf("the loop logged %d lines saying the wait ended, want exactly one", len(ended))
	}
	session := ""
	ended[0].Attrs(func(attr slog.Attr) bool {
		if attr.Key == "session" {
			session = attr.Value.String()
		}
		return true
	})
	if session != registeredSession || waitedOf(ended[0]) <= 0 {
		t.Errorf("the line that ends the wait names session %q after %v, want %q after the refusals' waits", session, waitedOf(ended[0]), registeredSession)
	}
}

// TestAnUnregisteredSessionIsLoggedAgainEveryHalfHour: a registration that stays missing for over
// an hour is logged when it is first refused, again at the first refusal 30 minutes after that,
// and again 30 minutes after that — three lines across seventeen refusals and 3765 s, rather than
// one per trade or one for ever.
func TestAnUnregisteredSessionIsLoggedAgainEveryHalfHour(t *testing.T) {
	const refusals = 17
	landed, err, _, waits, records := waitForRegistration(t, refusals)
	if err != nil || landed == nil {
		t.Fatalf("Run = %v and handed its caller %+v; want the pod's session %s traded once its registration landed, after %d refusals",
			err, landed, registeredSession, refusals)
	}
	lines := unregisteredLines(records)
	if len(lines) != 3 {
		t.Fatalf("the loop logged %d lines naming the session %s as unregistered across %d refusals, want 3: once, then every 30 minutes (%v waits)",
			len(lines), registeredSession, refusals, waits)
	}
	for i := 1; i < len(lines); i++ {
		if apart := waitedOf(lines[i]) - waitedOf(lines[i-1]); apart < 30*time.Minute {
			t.Errorf("lines %d and %d naming the session %s are %s apart on the loop's clock, want at least 30 minutes", i-1, i, registeredSession, apart)
		}
	}
}

// waitedOf is a line's own `waited` attribute: how long the loop had been waiting for the
// registration when it wrote that line, on the loop's clock rather than the record's wall time.
func waitedOf(record slog.Record) time.Duration {
	var waited time.Duration
	record.Attrs(func(attr slog.Attr) bool {
		if attr.Key == "waited" {
			waited = attr.Value.Duration()
		}
		return true
	})
	return waited
}

// TestEveryOtherRefusalStaysPermanent pins what Permanent reads: the broker's unregistered pod
// session is the one invalid_grant a later trade can cure, read off the start of its
// error_description, and every other refusal reads as it did — the same words under another
// code, the words anywhere but at the start, and every other refusal the broker answers.
func TestEveryOtherRefusalStaysPermanent(t *testing.T) {
	_, unregistered := unregisteredAnswer(registeredSession)
	description := unregistered["error_description"].(string)
	for _, tc := range []struct {
		name        string
		status      int
		code        string
		description string
		permanent   bool
	}{
		{"an unregistered pod session", 400, "invalid_grant", description, false},
		{"the broker's rate limit", 429, "invalid_request", "too many token requests", false},
		{"a proof outside the skew window", 400, "invalid_dpop_proof", "", false},
		{"a nonce the broker wanted", 400, "use_dpop_nonce", "", false},
		{"no entry lists the account", 400, "invalid_grant", "no entry lists the account this token names", true},
		{"a session another identity holds", 400, "invalid_grant", "The session name 'implementer-g1' belongs to another identity.", true},
		{"a revoked sign-in", 400, "invalid_grant", "ada@example.com revoked this machine's sign-in", true},
		{"the unregistered words under another code", 400, "invalid_request", description, true},
		{"the unregistered words past the start", 400, "invalid_grant", "the refusal was: " + description, true},
		{"the unregistered code with no colon", 400, "invalid_grant", "pod_session_not_registered", true},
		{"an unknown client", 401, "invalid_client", "", true},
		{"a refused subject token", 400, "invalid_request", "The subject token was refused: its audience is not this broker's.", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			refused := newRefusedError("a pod's projected token", tc.status, redact.NewRemoteText(tc.code), redact.NewRemoteText(tc.description))
			if got := refused.Permanent(); got != tc.permanent {
				t.Errorf("HTTP %d %s %q reads permanent=%t, want %t", tc.status, tc.code, tc.description, got, tc.permanent)
			}
		})
	}
}
