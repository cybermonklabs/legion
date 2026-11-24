// packages/envoy/internal/cipherclient/renewal_test.go
//
// What the renewal loop does when a trade does not land, which is the half of the contract a
// corpus cannot hold: a broker that is down is waited out, because this grant has no rotation
// window to miss and an outage past one token's life is the same event as a short one; a broker
// that refuses the identity stops the loop with a plain error, because no retry cures that; and
// a caller is handed no identity it cannot use, so nothing of its writes lands unauthenticated.
// When it trades again once a trade lands is the other half, held by testdata/renewal.json
// (TestRenewalCorpus, and TestTokenTradesAgainAtTheCorpusMoment through Token itself), which the
// TypeScript client's suite replays too.

package cipherclient

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/cipheraddr"
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
			"acc" + "ess_token": "dispatch-token", "token_type": "DPoP", "expires_in": 3600,
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

	// The held token is due for renewal and still live, so the caller keeps writing while the
	// broker is down.
	client.held.Expiry, client.held.renewsAt = time.Now().Add(30*time.Second), time.Now()
	held, err := client.Token(ctx)
	if err != nil || held.AccessToken != first.AccessToken {
		t.Fatalf("Token while the broker is down = %v, %v; want the token in hand, which is still live", held, err)
	}

	// Once it has run out there is no identity to write with, and the client says so rather
	// than handing its caller a token the broker will refuse.
	client.held.Expiry, client.held.renewsAt = time.Now().Add(-time.Second), time.Now()
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

	// A 429 while the token in hand is due for renewal and still live is no reason to stop
	// writing with it.
	client.held.Expiry, client.held.renewsAt = time.Now().Add(30*time.Second), time.Now()
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

// renewalCorpusPath is the file both clients' suites replay their renewal rule against; the
// TypeScript suite reads it by relative path.
const renewalCorpusPath = "testdata/renewal.json"

// renewalCorpus is testdata/renewal.json: each row a token's life when it lands, the window, and
// how long after it lands the rule trades again, all in seconds.
type renewalCorpus struct {
	Version int `json:"version"`
	Rows    []struct {
		Name               string  `json:"name"`
		LifeSeconds        float64 `json:"life_seconds"`
		RenewBeforeSeconds float64 `json:"renew_before_seconds"`
		RenewsAfterSeconds float64 `json:"renews_after_seconds"`
	} `json:"rows"`
}

// TestRenewalCorpus replays every row of testdata/renewal.json through renewalAt: a token is
// traded again RenewBefore ahead of its expiry and never before half of what it had left when it
// landed, so a token between one and two windows renews at its half life rather than soon after
// it lands, and the TypeScript client, which replays the same file, renews at the same moment.
func TestRenewalCorpus(t *testing.T) {
	corpus := readRenewalCorpus(t)
	now := time.Now()
	for _, row := range corpus.Rows {
		t.Run(row.Name, func(t *testing.T) {
			client, err := New(Config{BrokerURL: cipheraddr.New("https://secrets.example"), Key: testKey(t), Subject: projectedStub(),
				RenewBefore: corpusSeconds(row.RenewBeforeSeconds), Now: func() time.Time { return now }})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			token := &Token{Expiry: now.Add(corpusSeconds(row.LifeSeconds))}
			if got, want := client.renewalAt(token, now).Sub(now), corpusSeconds(row.RenewsAfterSeconds); got != want {
				t.Errorf("a token of %v s with a window of %v s renews %s after it lands, want %s", row.LifeSeconds, row.RenewBeforeSeconds, got, want)
			}
		})
	}
}

// TestTokenTradesAgainAtTheCorpusMoment drives Token with a stepped clock across each row of
// testdata/renewal.json a token can land by, under the row's own window: Token trades as the
// token lands, answers that token at every call a step apart before the corpus's renewal moment,
// and trades again at exactly that moment, which is when Run renews it and when the TypeScript
// client trades again. A Token that held a token only until RenewBefore ahead of its expiry
// traded one between one and two windows again before its half life, and one of the window or
// less at every call.
func TestTokenTradesAgainAtTheCorpusMoment(t *testing.T) {
	const step = 500 * time.Millisecond
	ran := 0
	for _, row := range readRenewalCorpus(t).Rows {
		// The broker writes a token's life in whole seconds, and read refuses an answer with none
		// left, so only a row of a whole, positive life can land through Token: the rows of a life
		// of 0 s and -1 s cannot, and are left out.
		if row.LifeSeconds <= 0 || row.LifeSeconds != math.Trunc(row.LifeSeconds) {
			continue
		}
		ran++
		t.Run(row.Name, func(t *testing.T) {
			broker := newStubBroker(t, func(n int, _ url.Values) (int, map[string]any) {
				return 200, map[string]any{"access_token": "token-" + strconv.Itoa(n), "token_type": "DPoP", "expires_in": int64(row.LifeSeconds)}
			})
			landed := time.Now()
			clock := landed
			client, err := New(Config{BrokerURL: broker.Address(), Key: testKey(t), Subject: projectedStub(),
				RenewBefore: corpusSeconds(row.RenewBeforeSeconds), Now: func() time.Time { return clock }})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			ctx := context.Background()
			if token, err := client.Token(ctx); err != nil || token.AccessToken != "token-0" {
				t.Fatalf("the first Token = %+v, %v; want the trade that lands the token", token, err)
			}
			renews := corpusSeconds(row.RenewsAfterSeconds)
			for elapsed := time.Duration(0); elapsed < renews; elapsed += step {
				clock = landed.Add(elapsed)
				token, err := client.Token(ctx)
				if trades := len(broker.Requests()); trades != 1 {
					t.Fatalf("Token %s after a token of %v s landed made %d trades; want the one it landed by, and none before the corpus's moment, %s after it landed",
						elapsed, row.LifeSeconds, trades, renews)
				}
				if err != nil || token.AccessToken != "token-0" {
					t.Fatalf("Token %s after a token of %v s landed = %+v, %v; want the token in hand", elapsed, row.LifeSeconds, token, err)
				}
			}
			clock = landed.Add(renews)
			token, err := client.Token(ctx)
			if trades := len(broker.Requests()); trades != 2 {
				t.Fatalf("Token at the corpus's moment, %s after a token of %v s landed, made %d trades in all; want two, the one it landed by and one at that moment",
					renews, row.LifeSeconds, trades)
			}
			if err != nil || token.AccessToken != "token-1" {
				t.Fatalf("Token at the corpus's moment, %s after a token of %v s landed = %+v, %v; want the fresh trade", renews, row.LifeSeconds, token, err)
			}
		})
	}
	if ran == 0 {
		t.Fatalf("%s holds no row of a whole, positive life", renewalCorpusPath)
	}
}

// TestRunRenewsAtTheMomentWorkedOutAsTheTokenLanded: Run trades again at the renewal moment it
// stored as the token landed, however long onToken took, and RenewalAt answers that same moment
// inside onToken, where natscred's keeper reads it. A token of 400 s under the default window of
// 300 s renews at its half life, 200 s after it lands. An onToken that takes 40 s leaves it 360 s,
// whose half, worked out again then, is 220 s after the landing: a loop that worked the moment out
// after onToken renewed 20 s late, half of what onToken took.
func TestRunRenewsAtTheMomentWorkedOutAsTheTokenLanded(t *testing.T) {
	const (
		life = 400 * time.Second
		took = 40 * time.Second
		// renews is half of life, since life is under twice DefaultRenewBefore.
		renews = 200 * time.Second
	)
	broker := newStubBroker(t, func(n int, _ url.Values) (int, map[string]any) {
		return 200, map[string]any{"access_token": "token-" + strconv.Itoa(n), "token_type": "DPoP", "expires_in": int64(life / time.Second)}
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	landed := time.Date(2026, 11, 22, 4, 0, 0, 0, time.UTC)
	clock := &waitingClock{now: landed, stop: ctx}
	client, err := New(Config{BrokerURL: broker.Address(), Key: testKey(t), Subject: projectedStub(), Now: clock.Now, After: clock.After})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// arrivals is the loop's clock as each token reached onToken.
	var arrivals []time.Time
	err = client.Run(ctx, func(_ context.Context, token *Token) error {
		arrivals = append(arrivals, clock.now)
		if len(arrivals) == 2 {
			cancel()
			return nil
		}
		clock.now = clock.now.Add(took)
		if got := client.RenewalAt(token); !got.Equal(landed.Add(renews)) {
			t.Errorf("RenewalAt %s after the token landed answers %s after the landing, want %s: the moment worked out as it landed", took, got.Sub(landed), renews)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Run = %v, want it to end with its context", err)
	}
	if len(arrivals) != 2 {
		t.Fatalf("the loop handed its caller %d tokens, want the first and its renewal", len(arrivals))
	}
	if got := arrivals[1].Sub(landed); got != renews {
		t.Errorf("the renewal landed %s after the token it renews, want %s: the moment worked out as that token landed, whatever the %s onToken took", got, renews, took)
	}
	if trades := len(broker.Requests()); trades != 2 {
		t.Errorf("the broker saw %d trades, want two: the first and its renewal", trades)
	}
}

// readRenewalCorpus is testdata/renewal.json, refused where it is not version 1 with rows.
func readRenewalCorpus(t *testing.T) renewalCorpus {
	t.Helper()
	data, err := os.ReadFile(renewalCorpusPath)
	if err != nil {
		t.Fatalf("read %s: %v", renewalCorpusPath, err)
	}
	var corpus renewalCorpus
	if err := json.Unmarshal(data, &corpus); err != nil {
		t.Fatalf("parse %s: %v", renewalCorpusPath, err)
	}
	if corpus.Version != 1 || len(corpus.Rows) == 0 {
		t.Fatalf("%s is version %d with %d rows, want version 1 with rows", renewalCorpusPath, corpus.Version, len(corpus.Rows))
	}
	return corpus
}

// corpusSeconds is one of the corpus's times, written in seconds, as a duration.
func corpusSeconds(s float64) time.Duration { return time.Duration(s * float64(time.Second)) }

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
			client, err := New(Config{BrokerURL: cipheraddr.New(broker.URL()), Key: testKey(t), Subject: projectedStub(), RenewBefore: time.Minute, Log: slog.New(logged)})
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

			client.held.Expiry, client.held.renewsAt = time.Now().Add(30*time.Second), time.Now()
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
