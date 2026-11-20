// packages/envoy/internal/brokerclient/renewal_test.go
//
// What the renewal loop does when a trade does not land, which is the half of the contract a
// corpus cannot hold: a broker that is down is waited out, because this grant has no rotation
// window to miss and an outage past one token's life is the same event as a short one; a broker
// that refuses the identity stops the loop with a plain error, because no retry cures that; and
// a caller is handed no identity it cannot use, so nothing of its writes lands unauthenticated.

package brokerclient

import (
	"context"
	"errors"
	"log/slog"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/brokeraddr"
	"github.com/sjawhar/envoy/internal/logtest"
)

// TestARefusedIdentityStopsTheRenewalLoopWithAPlainError: the broker is told to refuse, and the
// loop ends saying so rather than asking again every quarter minute for ever.
func TestARefusedIdentityStopsTheRenewalLoopWithAPlainError(t *testing.T) {
	broker := newStubBroker(t, func(int, url.Values) (int, map[string]any) {
		return 400, map[string]any{"error": "invalid_grant", "error_description": "no entry lists this account"}
	})
	client := refusalClient(t, broker)
	err := client.Run(context.Background(), func(context.Context, *Token) error {
		t.Error("the loop handed its caller a token the broker refused to mint")
		return nil
	})
	var refused *RefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("Run = %v, want the broker's refusal", err)
	}
	if !strings.Contains(err.Error(), "no entry lists this account") {
		t.Errorf("Run's error is %q, want the broker's own words in it", err)
	}
	if trades := len(broker.Requests()); trades != 1 {
		t.Errorf("the loop made %d trades, want one: a refusal is not retried", trades)
	}
}

// TestABrokerThatIsDownIsWaitedOut: the first trade lands, the broker then answers nothing an
// answer can be read out of, and the loop keeps asking until it is up again — with the caller
// handed the token it already holds while that token is live, and no token at all once it is not.
func TestABrokerThatIsDownIsWaitedOut(t *testing.T) {
	down := true
	broker := newStubBroker(t, func(n int, _ url.Values) (int, map[string]any) {
		if n > 0 && down {
			return 503, map[string]any{"error": "unavailable"}
		}
		return 200, map[string]any{
			"access_token": "dispatch-token", "token_type": "DPoP", "expires_in": 3600,
			"issued_token_type": "urn:ietf:params:oauth:token-type:access_token",
		}
	})
	client, err := New(Config{BrokerURL: broker.Address(), Key: testKey(t), Subject: projectedStub(), RenewBefore: time.Second})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()
	first, err := client.Token(ctx)
	if err != nil {
		t.Fatalf("Token: %v", err)
	}

	// The held token is still live, so the caller keeps writing while the broker is down.
	client.held.Expiry = time.Now().Add(30 * time.Second)
	held, err := client.Token(ctx)
	if err != nil || held.AccessToken != first.AccessToken {
		t.Fatalf("Token while the broker is down = %v, %v; want the token in hand, which is still live", held, err)
	}

	// Once it has run out there is no identity to write with, and the client says so rather
	// than handing its caller a token the broker will refuse.
	client.held.Expiry = time.Now().Add(-time.Second)
	if token, err := client.Token(ctx); err == nil {
		t.Fatalf("Token with nothing live left = %+v, want the failure", token)
	}

	down = false
	after, err := client.Token(ctx)
	if err != nil || after.AccessToken != "dispatch-token" {
		t.Fatalf("Token once the broker is up again = %v, %v; want a fresh trade, with no step by a person", after, err)
	}
}

// TestABusyBrokerIsAskedAgainRatherThanEndingTheLoop: the broker's token endpoint spends a
// bucket per source address, so a 429 is the one refusal a retry cures. A loop that stopped on
// it would enrol nothing from the moment the credential in hand lapsed, with only a restart to
// cure it — the failure a dev stack reproduces in seconds and a deployment meets behind one
// address.
func TestABusyBrokerIsAskedAgainRatherThanEndingTheLoop(t *testing.T) {
	// The first trade lands; the three after it find the bucket empty; then it has refilled.
	const refusals = 3
	broker := newStubBroker(t, func(n int, _ url.Values) (int, map[string]any) {
		if n > 0 && n <= refusals {
			return 429, map[string]any{"error": "invalid_request", "error_description": "too many token requests; try again later"}
		}
		return 200, map[string]any{"access_token": "token-" + strconv.Itoa(n), "token_type": "DPoP", "expires_in": 3600}
	})
	client, err := New(Config{BrokerURL: broker.Address(), Key: testKey(t), Subject: projectedStub(), RenewBefore: time.Second})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()
	first, err := client.Token(ctx)
	if err != nil {
		t.Fatalf("Token: %v", err)
	}

	// A 429 while the token in hand is live is no reason to stop writing with it.
	client.held.Expiry = time.Now().Add(30 * time.Second)
	held, err := client.Token(ctx)
	if err != nil || held.AccessToken != first.AccessToken {
		t.Fatalf("Token while the broker is busy = %v, %v; want the token in hand, which is still live", held, err)
	}

	// And the renewal loop waits the bucket out rather than ending: every wait fires at once,
	// so the three refusals and the trade that follows them run at the test's speed.
	client.after = func(time.Duration) <-chan time.Time {
		fired := make(chan time.Time, 1)
		fired <- time.Now()
		return fired
	}
	loop, cancel := context.WithCancel(ctx)
	defer cancel()
	var landed atomic.Int64
	err = client.Run(loop, func(context.Context, *Token) error {
		landed.Add(1)
		cancel()
		return nil
	})
	if err != nil {
		t.Fatalf("Run = %v; want the 429s waited out, not the end of the loop", err)
	}
	if landed.Load() != 1 {
		t.Fatalf("the loop handed its caller %d tokens; want the one after the 429s", landed.Load())
	}
	if trades := len(broker.Requests()); trades != refusals+2 {
		t.Fatalf("the broker saw %d trades, want %d: Token's first, the loop's %d refused, and the one that landed",
			trades, refusals+2, refusals)
	}
}

// TestATokenShorterThanRenewBeforeIsNotRenewedInALoop: a token that lives less than
// RenewBefore is renewed at half of what is left, so a deployment minting short tokens — or a
// dev stack proving a renewal in seconds — renews steadily rather than trading again the
// instant each token lands, which empties the broker's bucket.
func TestATokenShorterThanRenewBeforeIsNotRenewedInALoop(t *testing.T) {
	now := time.Now()
	client, err := New(Config{BrokerURL: brokeraddr.New("https://secrets.example"), Key: testKey(t), Subject: projectedStub(),
		Now: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if wait := client.untilRenewal(&Token{Expiry: now.Add(time.Hour)}); wait != time.Hour-DefaultRenewBefore {
		t.Errorf("a token of an hour is renewed in %s, want %s ahead of its expiry", wait, DefaultRenewBefore)
	}
	if wait := client.untilRenewal(&Token{Expiry: now.Add(2 * time.Second)}); wait != time.Second {
		t.Errorf("a token of two seconds is renewed in %s, want half of what is left", wait)
	}
	if wait := client.untilRenewal(&Token{Expiry: now.Add(-time.Second)}); wait != 0 {
		t.Errorf("a token already past its expiry is renewed in %s, want at once", wait)
	}
}

// TestTokenReusesAShortTokenUntilHalfwayThroughItsLife: Token reuses a token that lives less than
// RenewBefore, such as the 270 seconds the broker gives a restricted member's machine login,
// until halfway through what was left of its life when it landed, as Run renews one
// (untilRenewal), rather than trading again at every call, which empties the broker's bucket.
func TestTokenReusesAShortTokenUntilHalfwayThroughItsLife(t *testing.T) {
	broker := newStubBroker(t, func(n int, _ url.Values) (int, map[string]any) {
		return 200, map[string]any{"access_token": "token-" + strconv.Itoa(n), "token_type": "DPoP", "expires_in": 300}
	})
	now := time.Now()
	client, err := New(Config{BrokerURL: broker.Address(), Key: testKey(t), Subject: projectedStub(), Now: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()
	first, err := client.Token(ctx)
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	now = now.Add(100 * time.Second)
	if again, err := client.Token(ctx); err != nil || again.AccessToken != first.AccessToken {
		t.Fatalf("Token 100 s into a token of 300 s = %v, %v; want the token in hand", again, err)
	}
	if trades := len(broker.Requests()); trades != 1 {
		t.Fatalf("the broker saw %d trades by 100 s into a token of 300 s, want 1", trades)
	}
	now = now.Add(60 * time.Second)
	if renewed, err := client.Token(ctx); err != nil || renewed.AccessToken == first.AccessToken {
		t.Fatalf("Token 160 s into a token of 300 s = %v, %v; want a fresh token past the halfway mark", renewed, err)
	}
	if trades := len(broker.Requests()); trades != 2 {
		t.Fatalf("the broker saw %d trades by 160 s into a token of 300 s, want 2", trades)
	}
}

// TestADPoPRefusalKeepsTheRenewalLoopTrading: a proof the broker refuses for its own clock, or
// for a nonce it wanted, is nothing a restart cures and everything the next trade may. A helper
// started at boot, before NTP has corrected the host's clock, is refused `invalid_dpop_proof`
// by a broker whose skew window is 60 s; stopping there would leave it holding no launcher
// credential, answering NO_CREDENTIAL to every register and sign, until somebody restarted it.
// The loop is proved still waiting to trade again by its context ending it with no error, while
// the control — a refusal that is the broker's final word — returns at once.
func TestADPoPRefusalKeepsTheRenewalLoopTrading(t *testing.T) {
	for _, tc := range []struct {
		code  string
		stops bool
	}{
		{"invalid_dpop_proof", false},
		{"use_dpop_nonce", false},
		{"invalid_grant", true},
	} {
		t.Run(tc.code, func(t *testing.T) {
			broker := newStubBroker(t, func(int, url.Values) (int, map[string]any) {
				return 400, map[string]any{"error": tc.code, "error_description": "the proof was refused"}
			})
			ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			defer cancel()
			err := refusalClient(t, broker).Run(ctx, func(context.Context, *Token) error {
				t.Error("the loop handed its caller a token the broker refused to mint")
				return nil
			})
			var refused *RefusedError
			switch stopped := errors.As(err, &refused); {
			case tc.stops && !stopped:
				t.Fatalf("Run on %s = %v, want it to stop with the broker's refusal", tc.code, err)
			case !tc.stops && stopped:
				t.Fatalf("Run on %s stopped with %v, want it still waiting to trade again: the next trade can succeed", tc.code, err)
			case !tc.stops && err != nil:
				t.Fatalf("Run on %s = %v, want no error when its context ends", tc.code, err)
			}
		})
	}
}

const (
	retriedRefusalLine  = "the secrets broker refused a trade, which a later trade may cure"
	retriedNoAnswerLine = "a trade got no usable answer from the secrets broker"
)

func TestARetriedTradeIsLoggedAsWhatHappened(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   map[string]any
		down   bool
		code   string
	}{
		{name: "a person Dispatch holds", status: 400, code: "membership_held",
			body: map[string]any{"error": "membership_held", "error_description": heldDescription}},
		{name: "an empty bucket", status: 429, code: "invalid_request",
			body: map[string]any{"error": "invalid_request", "error_description": "too many token requests; try again later"}},
		{name: "a proof outside the skew window", status: 400, code: "invalid_dpop_proof",
			body: map[string]any{"error": "invalid_dpop_proof", "error_description": "the proof's iat is outside the broker's skew window"}},
		{name: "a proof with no nonce", status: 400, code: "use_dpop_nonce",
			body: map[string]any{"error": "use_dpop_nonce", "error_description": "the broker wants a nonce in the proof"}},
		{name: "a broker answering 503", status: 503, body: map[string]any{"error": "unavailable"}},
		// The broker's answer to a machine login's trade while Dispatch cannot be asked what kind
		// of member the login's approver is, as through a restart of Dispatch: no person acts, and
		// the loop trades again on its schedule until Dispatch answers.
		{name: "a broker that cannot ask Dispatch a member kind", status: 503, body: map[string]any{"error": "temporarily_unavailable",
			"error_description": "Dispatch could not be asked what kind of member the machine login's approver is, so this broker cannot tell whether the token is held to one project; try again."}},
		{name: "a broker answering nothing", down: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			broker := newStubBroker(t, func(n int, _ url.Values) (int, map[string]any) {
				if n == 0 {
					return 200, mintedToken(1)
				}
				return tc.status, tc.body
			})
			logged := &logtest.Recorder{}
			client, err := New(Config{BrokerURL: brokeraddr.New(broker.URL()), Key: testKey(t), Subject: projectedStub(), RenewBefore: time.Minute, Log: slog.New(logged)})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			ctx := context.Background()
			if _, err := client.Token(ctx); err != nil {
				t.Fatalf("the first trade: %v", err)
			}
			if tc.down {
				broker.server.Close()
			}

			client.held.Expiry = time.Now().Add(30 * time.Second)
			if held, err := client.Token(ctx); err != nil || held.AccessToken != mintedToken(1)["access_token"] {
				t.Fatalf("Token = %+v, %v; want the token in hand, which is still live", held, err)
			}
			if n := len(broker.Requests()); !tc.down && n != 2 {
				t.Fatalf("the broker saw %d trades; want two, the first and Token's renewal", n)
			}
			loop, cancel := context.WithCancel(ctx)
			client.after = func(time.Duration) <-chan time.Time {
				cancel()
				return nil
			}
			if err := client.Run(loop, func(context.Context, *Token) error {
				t.Error("the loop handed its caller a token from a trade that failed")
				return nil
			}); err != nil {
				t.Fatalf("Run = %v; want it waiting to ask again", err)
			}

			client.mu.Lock()
			_, tradeErr := client.trade(ctx)
			client.mu.Unlock()
			if tradeErr == nil {
				t.Fatal("a trade after the loop succeeded; want it failing as every trade after the first does")
			}

			want := retriedRefusalLine
			if tc.code == "" {
				want = retriedNoAnswerLine
			}
			for _, suffix := range []string{"; the token in hand is still live", "; asking again"} {
				var lines []map[string]string
				for _, record := range logged.Records() {
					if strings.Contains(record.Message, "did not answer") {
						t.Errorf("the client logged %q: the broker answered, or the line names what it did", record.Message)
					}
					if record.Message == want+suffix {
						lines = append(lines, attrsOf(t, record))
					}
				}
				if len(lines) != 1 {
					t.Fatalf("the client logged %d lines %q; want one (log: %v)", len(lines), want+suffix, messagesOf(logged.Records()))
				}
				line := lines[0]
				if line["code"] != tc.code {
					t.Errorf("the line %q names the code %q; want %q", want+suffix, line["code"], tc.code)
				}
				if line["error"] != tradeErr.Error() {
					t.Errorf("the line %q carries the error %q; want the trade's error whole, %q", want+suffix, line["error"], tradeErr.Error())
				}
				if tc.code != "" {
					if line["status"] != strconv.Itoa(tc.status) {
						t.Errorf("the line %q names the status %q; want %d", want+suffix, line["status"], tc.status)
					}
					if description := tc.body["error_description"].(string); !strings.Contains(line["error"], description) {
						t.Errorf("the line %q carries the error %q; want the broker's words %q", want+suffix, line["error"], description)
					}
				} else if _, named := line["status"]; named {
					t.Errorf("the line %q names a status %q; a trade with no usable answer has none", want+suffix, line["status"])
				}
			}
		})
	}
}

func attrsOf(t *testing.T, record slog.Record) map[string]string {
	t.Helper()
	attrs := map[string]string{}
	record.Attrs(func(attr slog.Attr) bool {
		if _, twice := attrs[attr.Key]; twice {
			t.Errorf("the line %q carries the key %q twice", record.Message, attr.Key)
		}
		attrs[attr.Key] = attr.Value.String()
		return true
	})
	return attrs
}

func messagesOf(records []slog.Record) []string {
	messages := make([]string, 0, len(records))
	for _, record := range records {
		messages = append(messages, record.Message)
	}
	return messages
}

func refusalClient(t *testing.T, broker *stubBroker) *Client {
	t.Helper()
	client, err := New(Config{BrokerURL: broker.Address(), Key: testKey(t), Subject: projectedStub()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return client
}

func projectedStub() Source {
	return &scriptedSource{
		token:     "projected.service.account.token",
		tokenType: "urn:envoy:params:oauth:token-type:kubernetes-service-account",
		describe:  "a pod's projected service-account token",
	}
}
