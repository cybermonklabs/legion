// The operator's limit on the messages each agent session sends through this listener: what the
// two settings read as, the start line, and that a listener with no limit takes every message.

package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/id"
	"github.com/sjawhar/envoy/internal/logging"
)

// senderLimitEnv is an environment holding values and nothing else, as os.Getenv reads one.
func senderLimitEnv(values map[string]string) func(string) string {
	return func(name string) string { return values[name] }
}

// requireSenderLimit holds limit to perMinute messages a minute and burst at once, read off the
// count itself: burst messages from one sender at one instant, then none until a minute divided
// by perMinute has passed.
func requireSenderLimit(t *testing.T, limit *senderLimit, perMinute, burst int) {
	t.Helper()
	if limit == nil {
		t.Fatalf("no limit, want %d messages a minute and %d at once", perMinute, burst)
	}
	if limit.perMinute != perMinute || limit.burst != burst {
		t.Fatalf("limit of %d messages a minute and %d at once, want %d and %d", limit.perMinute, limit.burst, perMinute, burst)
	}
	if limit.now == nil {
		t.Fatal("the limit reads no clock")
	}
	at := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	for i := range burst {
		if _, ok := limit.counts.TakeAt("ses_sender", at); !ok {
			t.Fatalf("message %d of a burst of %d was refused", i+1, burst)
		}
	}
	if _, ok := limit.counts.TakeAt("ses_sender", at); ok {
		t.Fatalf("message %d at the same instant was taken past a burst of %d", burst+1, burst)
	}
	if wait, want := limit.counts.WaitAt("ses_sender", at), time.Minute/time.Duration(perMinute); wait != want {
		t.Fatalf("after the burst the sender waits %s, want %s at %d messages a minute", wait, want, perMinute)
	}
}

// requireOneSenderAccepted publishes messages from one agent session to a team topic of its own
// through the publish route over deps, all at once, and holds each to a 200. Arriving together,
// they are a burst that any limit taking fewer at once would refuse part of. The time they
// took is logged and not held to a bound: with no limit no clock is read, so the time cannot
// change an answer, and a bound would fail a slow run (the race detector's) rather than a limit.
func requireOneSenderAccepted(t *testing.T, deps *listenerDeps, messages int) {
	t.Helper()
	handler := publishHandler(deps, logging.NewWithWriter("test", io.Discard))
	topic := "notifications.team.sender-limit-" + id.New()
	recorders := make([]*httptest.ResponseRecorder, messages)
	var sent sync.WaitGroup
	started := time.Now()
	for i := range recorders {
		body := fmt.Sprintf(`{"source":"agent","source_session":"ses_sender","topic":%q,"message":"message %d"}`, topic, i+1)
		request := httptest.NewRequest(http.MethodPost, "/v1/messages/publish", strings.NewReader(body))
		recorders[i] = httptest.NewRecorder()
		sent.Go(func() { handler.ServeHTTP(recorders[i], request) })
	}
	sent.Wait()
	elapsed := time.Since(started)
	for i, recorder := range recorders {
		if recorder.Code != http.StatusOK {
			t.Fatalf("message %d of %d from one sender: status %d, want 200 (body: %s)", i+1, messages, recorder.Code, recorder.Body.String())
		}
	}
	t.Logf("%d messages from one sender, sent at once, all taken in %s", messages, elapsed)
}

func TestSenderLimitSettings(t *testing.T) {
	rate, burst := bus.SenderMessagesPerMinuteEnvVar, bus.SenderMessageBurstEnvVar

	for _, tc := range []struct {
		name string
		env  map[string]string
	}{
		{name: "no variable set is no limit"},
		{name: "both set blank is no limit", env: map[string]string{rate: "", burst: " \t"}},
		{name: "a rate of 0 is no limit", env: map[string]string{rate: "0"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			limit, err := parseSenderLimit(senderLimitEnv(tc.env))
			if err != nil {
				t.Fatalf("refused: %v", err)
			}
			if limit != nil {
				t.Fatalf("a limit of %d messages a minute and %d at once, want none", limit.perMinute, limit.burst)
			}
		})
	}

	t.Run("a rate alone sets a burst equal to the rate", func(t *testing.T) {
		limit, err := parseSenderLimit(senderLimitEnv(map[string]string{rate: "4"}))
		if err != nil {
			t.Fatalf("refused: %v", err)
		}
		requireSenderLimit(t, limit, 4, 4)
	})

	t.Run("a rate and a burst", func(t *testing.T) {
		limit, err := parseSenderLimit(senderLimitEnv(map[string]string{rate: " 4 ", burst: "\t8\n"}))
		if err != nil {
			t.Fatalf("refused: %v", err)
		}
		requireSenderLimit(t, limit, 4, 8)
	})

	for _, tc := range []struct {
		name     string
		env      map[string]string
		variable string
	}{
		{name: "a rate above 60000 is refused naming the variable", env: map[string]string{rate: "60001"}, variable: rate},
		{name: "a negative rate is refused naming the variable", env: map[string]string{rate: "-1"}, variable: rate},
		{name: "a rate that is not a whole number is refused naming the variable", env: map[string]string{rate: "4.5"}, variable: rate},
		{name: "a burst of 0 is refused naming the variable", env: map[string]string{rate: "4", burst: "0"}, variable: burst},
		{name: "a burst with no rate is refused naming the variable", env: map[string]string{burst: "8"}, variable: burst},
		{name: "a burst with a rate of 0 is refused naming the variable", env: map[string]string{rate: "0", burst: "8"}, variable: burst},
	} {
		t.Run(tc.name, func(t *testing.T) {
			limit, err := parseSenderLimit(senderLimitEnv(tc.env))
			if err == nil {
				t.Fatalf("parsed %+v, want a refusal naming %s", limit, tc.variable)
			}
			if limit != nil {
				t.Fatalf("refused with a limit of %d messages a minute and %d at once, want none", limit.perMinute, limit.burst)
			}
			for _, want := range []string{tc.variable, strconv.Quote(tc.env[tc.variable])} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("refusal %q does not name %s", err, want)
				}
			}
		})
	}

	t.Run("start line for 4 messages a minute and 8 at once", func(t *testing.T) {
		limit, err := parseSenderLimit(senderLimitEnv(map[string]string{rate: "4", burst: "8"}))
		if err != nil {
			t.Fatalf("refused: %v", err)
		}
		const want = "listener sender limit: 4 messages a minute, 8 at once, for each agent session in this process"
		if got := describeSenderLimit(limit); got != want {
			t.Fatalf("start line %q, want %q", got, want)
		}
	})

	t.Run("start line for no limit", func(t *testing.T) {
		limit, err := parseSenderLimit(senderLimitEnv(nil))
		if err != nil {
			t.Fatalf("refused: %v", err)
		}
		const want = "listener sender limit: none"
		if got := describeSenderLimit(limit); got != want {
			t.Fatalf("start line %q, want %q", got, want)
		}
	})

	t.Run("a listener with no setting accepts 100 messages from one sender in one second", func(t *testing.T) {
		limit, err := parseSenderLimit(senderLimitEnv(nil))
		if err != nil {
			t.Fatalf("refused: %v", err)
		}
		client := setupPublishTestClient(t)
		registry, sessions := setupSessionsTest(t, nil, nil)
		requireOneSenderAccepted(t, &listenerDeps{client: client, registry: registry, sessions: sessions, senderLimit: limit}, 100)
	})

	t.Run("listenerDeps with a zero senderLimit accepts 100 messages from one sender in one second", func(t *testing.T) {
		client := setupPublishTestClient(t)
		registry, sessions := setupSessionsTest(t, nil, nil)
		requireOneSenderAccepted(t, &listenerDeps{client: client, registry: registry, sessions: sessions}, 100)
	})
}
