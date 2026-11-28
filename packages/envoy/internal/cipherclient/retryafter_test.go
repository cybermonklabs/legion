// packages/envoy/internal/cipherclient/retryafter_test.go
//
// The broker answers a trade 503 with Retry-After when it could not check the identity's token
// because the key set of that token's issuer could not be read, and the token may well be sound
// once that wait has passed. So the client sends no trade before the wait has passed, a call waits
// for it inside its own context, and the renewal loop waits the larger of its own step and the
// wait, read as at most a minute. Each stand-in here replays the broker's own answer, captured from
// its real server (internal/cipher/api/unreadablekeysettest), with the row's own Retry-After where
// the row is about another wait, and every wait runs on the client's injected clock
// (waitingClock), so no row sleeps.

package cipherclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/cipheraddr"
	"github.com/sjawhar/envoy/internal/cipher/api/unreadablekeysettest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// outageStart is when the clock of each of these rows starts.
var outageStart = time.Date(2026, 11, 28, 4, 0, 0, 0, time.UTC)

// outageBroker is the broker's token endpoint through a key set that cannot be read: it answers
// the nth trade, counting from zero, with the answer outage(n) gives, and with ok(n) as a 200 where
// that gives none; and it records the client's clock as each trade reaches it.
type outageBroker struct {
	url      string
	mu       sync.Mutex
	arrivals []time.Time
}

func newOutageBroker(t *testing.T, clock *waitingClock, outage func(n int) *unreadablekeysettest.Answer, ok func(n int) map[string]any) *outageBroker {
	t.Helper()
	broker := &outageBroker{}
	broker.url = newRawServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != TokenPath {
			http.Error(w, "not the token endpoint", http.StatusNotFound)
			return
		}
		broker.mu.Lock()
		n := len(broker.arrivals)
		broker.arrivals = append(broker.arrivals, clock.Now())
		broker.mu.Unlock()
		if answer := outage(n); answer != nil {
			answer.Write(w)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(ok(n))
	})
	return broker
}

// Arrivals is the client's clock as each trade reached the stand-in, in order.
func (b *outageBroker) Arrivals() []time.Time {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]time.Time(nil), b.arrivals...)
}

// sinceStart is each of arrivals as the time since outageStart, for a failure's text.
func sinceStart(arrivals []time.Time) []time.Duration {
	offsets := make([]time.Duration, len(arrivals))
	for i, at := range arrivals {
		offsets[i] = at.Sub(outageStart)
	}
	return offsets
}

// keySetOutage is the token endpoint's answer through a key set that cannot be read as the broker's
// real server wrote it: 503, OAuth's error object with the code ISSUER_UNAVAILABLE, and Retry-After
// 5, the default spacing of a failing key set's reads, which the rows about that wait take as it
// is and the rows about another wait replace (Answer.Asking).
func keySetOutage(t *testing.T) unreadablekeysettest.Answer {
	t.Helper()
	answer := unreadablekeysettest.Read(t).TokenEndpoint
	if answer.Status != http.StatusServiceUnavailable || answer.RetryAfter != "5" {
		t.Fatalf("the broker's token endpoint answer through an unreadable key set is %d with Retry-After %q; these rows read it as 503 asking for 5 s", answer.Status, answer.RetryAfter)
	}
	return answer
}

// firstOnly answers the first trade with answer and every later one with none.
func firstOnly(answer *unreadablekeysettest.Answer) func(int) *unreadablekeysettest.Answer {
	return func(n int) *unreadablekeysettest.Answer {
		if n == 0 {
			return answer
		}
		return nil
	}
}

// always answers every trade with answer.
func always(answer *unreadablekeysettest.Answer) func(int) *unreadablekeysettest.Answer {
	return func(int) *unreadablekeysettest.Answer { return answer }
}

// outageClient is a client of broker trading a pod's projected token on clock, its waits made by
// after (clock.After where nil).
func outageClient(t *testing.T, broker *outageBroker, clock *waitingClock, after func(time.Duration) <-chan time.Time) *Client {
	t.Helper()
	if after == nil {
		after = clock.After
	}
	client, err := New(Config{BrokerURL: cipheraddr.New(broker.url), Key: testKey(t), Subject: projectedStub(), Now: clock.Now, After: after})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return client
}

// noRefusal fails t unless err is the broker's outage through a key set that cannot be read, as a
// call answers it once it stops trying: an UnavailableError of the 503 and the 5 s it asked for,
// neither a RefusedError nor a refusal that ends the identity (finalRefusal), whose words name the
// outage, the status and how long the call tried.
func noRefusal(t *testing.T, what string, err error) {
	t.Helper()
	var refused *RefusedError
	if errors.As(err, &refused) || finalRefusal(err) {
		t.Fatalf("%s = %v; want the outage, which is no refusal of the identity", what, err)
	}
	var outage *UnavailableError
	if !errors.As(err, &outage) || outage.Status != http.StatusServiceUnavailable || outage.OAuthError != "temporarily_unavailable" ||
		outage.Code != "ISSUER_UNAVAILABLE" || !outage.IssuerUnavailable() || outage.RetryAfter != 5*time.Second {
		t.Fatalf("%s = %v (%+v); want the UnavailableError of a 503 temporarily_unavailable ISSUER_UNAVAILABLE asking for 5 s", what, err, outage)
	}
	for _, words := range []string{"could not check the token of this machine's identity", "because its issuer's key set could not be read", "HTTP 503", "tried for"} {
		if !strings.Contains(err.Error(), words) {
			t.Errorf("%s reads %q; want it to say %q", what, err, words)
		}
	}
}

// TestARunWaitsOutA503sRetryAfter: the renewal loop trades again after the larger of its own step,
// 15 s after its first failure, and the wait a 503 asked for, read as at most 60 s: 45 s after a 503
// asking for 45 s, at its own 15 s after one asking for 5 s, and 60 s after one asking for 90 s. A
// 503 ends the loop no more than any other outage does: it goes on trading on its own schedule,
// keeps the token in hand, and returns no error when its context ends.
func TestARunWaitsOutA503sRetryAfter(t *testing.T) {
	outage := keySetOutage(t)
	for _, row := range []struct {
		name   string
		answer unreadablekeysettest.Answer
		want   time.Duration
	}{
		{"45 s", outage.Asking("45"), 45 * time.Second},
		{"5 s at 15 s", outage, 15 * time.Second},
		{"90 s", outage.Asking("90"), 60 * time.Second},
	} {
		t.Run(row.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			clock := &waitingClock{now: outageStart, stop: ctx}
			broker := newOutageBroker(t, clock, firstOnly(&row.answer), mintedToken)
			var landed []string
			err := outageClient(t, broker, clock, nil).Run(ctx, func(_ context.Context, token *Token) error {
				landed = append(landed, token.AccessToken)
				cancel()
				return nil
			})
			if err != nil {
				t.Fatalf("Run = %v; want it to end with its context", err)
			}
			arrivals := broker.Arrivals()
			if len(arrivals) != 2 || len(landed) != 1 || landed[0] != "access-token-1" {
				t.Fatalf("the broker saw trades at %v and the loop handed on %v; want two trades, the 503 and the one after it, and that one's token", sinceStart(arrivals), landed)
			}
			if got := arrivals[1].Sub(arrivals[0]); got != row.want {
				t.Fatalf("the trade after a 503 asking for %s s came %s after it; want %s", row.answer.RetryAfter, got, row.want)
			}
		})
	}

	t.Run("loop keeps going", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		clock := &waitingClock{now: outageStart, stop: ctx}
		// The first trade lands; every renewal after it meets the outage, and the third of those
		// ends the loop's context.
		broker := newOutageBroker(t, clock, func(n int) *unreadablekeysettest.Answer {
			switch n {
			case 0:
				return nil
			case 3:
				cancel()
			}
			return &outage
		}, mintedToken)
		client := outageClient(t, broker, clock, nil)
		landed := 0
		if err := client.Run(ctx, func(context.Context, *Token) error { landed++; return nil }); err != nil {
			t.Fatalf("Run through the outage = %v; want no error once its context ends: the outage is no refusal", err)
		}
		arrivals := broker.Arrivals()
		if len(arrivals) != 4 || landed != 1 {
			t.Fatalf("the broker saw trades at %v and the loop handed on %d tokens; want the first trade's token and three renewals that met the outage", sinceStart(arrivals), landed)
		}
		if steps := [2]time.Duration{arrivals[2].Sub(arrivals[1]), arrivals[3].Sub(arrivals[2])}; steps != [2]time.Duration{15 * time.Second, 30 * time.Second} {
			t.Fatalf("the renewals after the outage came %v apart; want the loop's own steps, 15 s then 30 s, each longer than the 5 s asked for", steps)
		}
		if held := client.held.Load(); held == nil || held.AccessToken != "access-token-0" || !clock.Now().Before(held.Expiry) {
			t.Fatalf("the token in hand after the loop = %+v; want the first trade's, still live", held)
		}
	})
}

// TestATokenCallWaitsOutA503sRetryAfterInsideItsContext: with no live token in hand, Token waits
// out the wait a 503 asked for inside its context and trades again once it has passed. A context
// whose deadline comes before the wait ends is answered the outage at once, with no trade more; a
// context with no deadline tries for 2 minutes at most; the outage a call answers is no refusal and
// names what happened. A live token in hand is answered at once, with no trade before the wait
// ends. The wait holds no lock: while one caller waits, a second whose deadline is 1 s away is
// answered by its deadline and sends nothing, and Drop returns.
func TestATokenCallWaitsOutA503sRetryAfterInsideItsContext(t *testing.T) {
	outage := keySetOutage(t)

	t.Run("waits inside ctx", func(t *testing.T) {
		clock := &waitingClock{now: outageStart, stop: context.Background()}
		broker := newOutageBroker(t, clock, firstOnly(&outage), mintedToken)
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		token, err := outageClient(t, broker, clock, nil).Token(ctx)
		if err != nil || token.AccessToken != "access-token-1" {
			t.Fatalf("Token through a 503 asking for 5 s = %+v, %v; want the token of the trade after the wait", token, err)
		}
		if arrivals := broker.Arrivals(); len(arrivals) != 2 || arrivals[1].Sub(arrivals[0]) < 5*time.Second {
			t.Fatalf("the broker saw trades at %v; want two, the second at least 5 s after the first", sinceStart(arrivals))
		}
	})

	t.Run("short deadline", func(t *testing.T) {
		clock := &waitingClock{now: outageStart, stop: context.Background()}
		broker := newOutageBroker(t, clock, always(&outage), mintedToken)
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		token, err := outageClient(t, broker, clock, nil).Token(ctx)
		var unavailable *UnavailableError
		if token != nil || !errors.As(err, &unavailable) {
			t.Fatalf("Token under a 3 s deadline through a 503 asking for 5 s = %+v, %v; want the outage", token, err)
		}
		if trades, waits := len(broker.Arrivals()), clock.Waits(); trades != 1 || len(waits) != 0 {
			t.Fatalf("the call made %d trades and waited %v; want one trade and no wait: the 5 s asked for run past its deadline", trades, waits)
		}
	})

	t.Run("bounded", func(t *testing.T) {
		clock := &waitingClock{now: outageStart, stop: context.Background()}
		broker := newOutageBroker(t, clock, always(&outage), mintedToken)
		token, err := outageClient(t, broker, clock, nil).Token(context.Background())
		var unavailable *UnavailableError
		if token != nil || !errors.As(err, &unavailable) {
			t.Fatalf("Token with no deadline through a broker answering 503 every time = %+v, %v; want the outage", token, err)
		}
		if took := clock.Now().Sub(outageStart); took > 2*time.Minute {
			t.Fatalf("Token with no deadline answered the outage %s after it began; want 2 minutes at most", took)
		}
		arrivals := broker.Arrivals()
		for i := 1; i < len(arrivals); i++ {
			if gap := arrivals[i].Sub(arrivals[i-1]); gap != 5*time.Second {
				t.Fatalf("the broker saw trades at %v; want each 5 s after the one before, the wait each 503 asked for", sinceStart(arrivals))
			}
		}
		if last := arrivals[len(arrivals)-1].Sub(outageStart); last+5*time.Second < 2*time.Minute {
			t.Fatalf("the last trade came %s after the call began; want the call to try until no 5 s wait is left inside its 2 minutes", last)
		}
	})

	t.Run("outage error is no refusal", func(t *testing.T) {
		clock := &waitingClock{now: outageStart, stop: context.Background()}
		broker := newOutageBroker(t, clock, always(&outage), mintedToken)
		client := outageClient(t, broker, clock, nil)
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_, err := client.Token(ctx)
		noRefusal(t, "Token under a 3 s deadline", err)
		_, err = client.Token(context.Background())
		noRefusal(t, "Token with no deadline", err)
	})

	t.Run("live token", func(t *testing.T) {
		clock := &waitingClock{now: outageStart, stop: context.Background()}
		broker := newOutageBroker(t, clock, func(n int) *unreadablekeysettest.Answer {
			if n == 1 {
				return &outage
			}
			return nil
		}, mintedToken)
		client := outageClient(t, broker, clock, nil)
		ctx := context.Background()
		first, err := client.Token(ctx)
		if err != nil {
			t.Fatalf("the first trade: %v", err)
		}
		// The token in hand is due for renewal and still live: the renewal meets the outage, and the
		// token is answered at once, then again within the wait, with no trade.
		clock.Advance(client.RenewalAt(first).Sub(clock.Now()))
		for _, step := range []time.Duration{0, 4 * time.Second} {
			clock.Advance(step)
			if token, err := client.Token(ctx); err != nil || token.AccessToken != first.AccessToken {
				t.Fatalf("Token %s into the wait the 503 asked for = %+v, %v; want the live token in hand", step, token, err)
			}
		}
		if trades, waits := len(broker.Arrivals()), clock.Waits(); trades != 2 || len(waits) != 0 {
			t.Fatalf("the broker saw %d trades and the calls waited %v before the wait ended; want two, the first and the renewal the 503 answered, and no wait", trades, waits)
		}
		clock.Advance(time.Second)
		if renewed, err := client.Token(ctx); err != nil || renewed.AccessToken != "access-token-2" {
			t.Fatalf("Token once the wait has passed = %+v, %v; want the renewal's fresh token", renewed, err)
		}
		if trades := len(broker.Arrivals()); trades != 3 {
			t.Fatalf("the broker saw %d trades; want three: the renewal is traded once the wait has passed", trades)
		}
	})

	t.Run("second caller", func(t *testing.T) {
		clock := &waitingClock{now: outageStart, stop: context.Background()}
		broker := newOutageBroker(t, clock, firstOnly(&outage), mintedToken)
		// The first caller's wait holds until the test releases it, so the second caller comes
		// while it is waiting.
		waiting := make(chan time.Duration, 1)
		release := make(chan struct{})
		// A row that fails before it releases the wait releases it at its cleanup, which runs
		// before the stand-in's, so the first caller's trade finds the stand-in serving.
		releaseOnce := sync.OnceFunc(func() { close(release) })
		t.Cleanup(releaseOnce)
		client := outageClient(t, broker, clock, func(d time.Duration) <-chan time.Time {
			waiting <- d
			<-release
			return clock.After(d)
		})
		type answer struct {
			token *Token
			err   error
		}
		firstAnswer := make(chan answer, 1)
		go func() {
			token, err := client.Token(context.Background())
			firstAnswer <- answer{token, err}
		}()
		if wait := testwait.Receive(t, "the first caller's wait for the 503's Retry-After", waiting); wait != 5*time.Second {
			t.Fatalf("the first caller waits %s; want the 5 s the 503 asked for", wait)
		}

		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		var second answer
		testwait.Returns(t, "a second Token whose deadline is 1 s away, while the first waits", func() {
			second.token, second.err = client.Token(ctx)
		})
		var unavailable *UnavailableError
		if second.token != nil || !errors.As(second.err, &unavailable) || ctx.Err() != nil {
			t.Fatalf("a second Token whose deadline is 1 s away = %+v, %v (its context: %v); want the outage, answered by its deadline", second.token, second.err, ctx.Err())
		}
		firstTrade := "access-token-0"
		testwait.Returns(t, "Drop while the first caller waits", func() { client.Drop(&Token{AccessToken: firstTrade}) })
		if trades := len(broker.Arrivals()); trades != 1 {
			t.Fatalf("the broker saw %d trades while the first caller waited; want the one the 503 answered: the second caller sends nothing", trades)
		}

		releaseOnce()
		got := testwait.Receive(t, "the first caller's answer once its wait has passed", firstAnswer)
		if got.err != nil || got.token.AccessToken != "access-token-1" {
			t.Fatalf("the first caller's Token = %+v, %v; want the token of the trade after its wait", got.token, got.err)
		}
		if trades := len(broker.Arrivals()); trades != 2 {
			t.Fatalf("the broker saw %d trades; want two, the 503 and the first caller's after its wait", trades)
		}
	})
}

// TestAnAirlockPlaceholderWaitsOutA503sRetryAfter: a workflow run's trade for its proxy
// placeholder waits out the wait a 503 asked for inside its context and is answered the
// placeholder by the trade after it; with no deadline it tries for 2 minutes at most and then
// answers the outage, which is no refusal.
func TestAnAirlockPlaceholderWaitsOutA503sRetryAfter(t *testing.T) {
	outage := keySetOutage(t)
	const held = "agent-secrets-placeholder-result-QqRrSsTtUuVvWwXxYyZz0123456789ab"
	placeholder := func(int) map[string]any { return c8Answer(held) }

	t.Run("503 then a placeholder", func(t *testing.T) {
		clock := &waitingClock{now: outageStart, stop: context.Background()}
		broker := newOutageBroker(t, clock, firstOnly(&outage), placeholder)
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		grant, err := outageClient(t, broker, clock, nil).AirlockPlaceholder(ctx)
		if err != nil || grant.Placeholder != held {
			t.Fatalf("AirlockPlaceholder through a 503 asking for 5 s = %+v, %v; want the placeholder of the trade after the wait", grant, err)
		}
		if arrivals := broker.Arrivals(); len(arrivals) != 2 || arrivals[1].Sub(arrivals[0]) < 5*time.Second {
			t.Fatalf("the broker saw trades at %v; want two, the second at least 5 s after the first", sinceStart(arrivals))
		}
	})

	t.Run("bounded", func(t *testing.T) {
		clock := &waitingClock{now: outageStart, stop: context.Background()}
		broker := newOutageBroker(t, clock, always(&outage), placeholder)
		grant, err := outageClient(t, broker, clock, nil).AirlockPlaceholder(context.Background())
		if grant != nil {
			t.Fatalf("AirlockPlaceholder through a broker answering 503 every time = %+v; want the outage", grant)
		}
		noRefusal(t, "AirlockPlaceholder with no deadline", err)
		if took := clock.Now().Sub(outageStart); took > 2*time.Minute {
			t.Fatalf("AirlockPlaceholder with no deadline answered the outage %s after it began; want 2 minutes at most", took)
		}
		if arrivals := broker.Arrivals(); len(arrivals) < 2 || arrivals[1].Sub(arrivals[0]) != 5*time.Second {
			t.Fatalf("the broker saw trades at %v; want the call to trade again 5 s after each 503", sinceStart(arrivals))
		}
	})
}
