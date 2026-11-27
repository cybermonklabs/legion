// The operator's limit on the messages each agent session sends through this listener: what the
// two settings read as, the start line, that a listener with no limit takes every message, and
// what each route answers, costs and logs for a message the limit counts.

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/bus/bustest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/id"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
	"github.com/sjawhar/envoy/internal/testnats"
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
	for i := range burst {
		if _, ok := limit.counts.TakeAt("ses_sender", senderLimitStart); !ok {
			t.Fatalf("message %d of a burst of %d was refused", i+1, burst)
		}
	}
	if _, ok := limit.counts.TakeAt("ses_sender", senderLimitStart); ok {
		t.Fatalf("message %d at the same instant was taken past a burst of %d", burst+1, burst)
	}
	if wait, want := limit.counts.WaitAt("ses_sender", senderLimitStart), time.Minute/time.Duration(perMinute); wait != want {
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
		request := postJSON("/v1/messages/publish", body)
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
	t.Parallel()
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

	t.Run("start line for 1 message a minute and 1 at once", func(t *testing.T) {
		limit, err := parseSenderLimit(senderLimitEnv(map[string]string{rate: "1"}))
		if err != nil {
			t.Fatalf("refused: %v", err)
		}
		const want = "listener sender limit: 1 message a minute, 1 at once, for each agent session in this process"
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

// senderLimitStart is the instant each rig's limit is read at until a test moves the rig's clock.
// The limit reads the test's clock and not the wall's, so no answer below turns on how long a run
// takes.
var senderLimitStart = time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)

// countedMessage is the text of every message below that names none of its own.
const countedMessage = "a message the sender limit counts"

// senderLimitSentence is the 429's error with its values filled in: what a refused agent reads.
// A value of 1 takes its noun's singular, `1 message a minute` and `after 1 second`.
func senderLimitSentence(perMinute, burst, seconds int) string {
	messages, wait := fmt.Sprintf("%d messages", perMinute), fmt.Sprintf("%d seconds", seconds)
	if perMinute == 1 {
		messages = "1 message"
	}
	if seconds == 1 {
		wait = "1 second"
	}
	return fmt.Sprintf("Your sending is rate limited. This message was not sent. This listener lets one session send %s a minute and %d at once. You can send it again after %s.",
		messages, burst, wait)
}

// senderLimitRig is a listener's /v1 routes over one bus with every store open
// (knownSessionRig), the wake reports its routes raise (newWakeReporter) read where they arrive,
// as send_state_test.go reads them, and a sender limit read at the rig's own clock, now.
type senderLimitRig struct {
	*knownSessionRig
	now   time.Time
	wakes *natsgo.Subscription
}

// newSenderLimitRig is a senderLimitRig on the package's shared server.
func newSenderLimitRig(t *testing.T) *senderLimitRig {
	t.Helper()
	return newSenderLimitRigOn(t, setupPublishTestClient(t), testnats.URL(t))
}

// newSenderLimitRigOnServer is a senderLimitRig on a server of its own, which the test can stop
// answering under live connections (testnats.Server.Freeze).
func newSenderLimitRigOnServer(t *testing.T) (*senderLimitRig, *testnats.Server) {
	t.Helper()
	server, url := testnats.Start(t)
	client, err := bus.ConnectOwningStream([]string{url}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("connect the listener: %v", err)
	}
	t.Cleanup(client.Close)
	return newSenderLimitRigOn(t, client, url), server
}

// newSenderLimitRigOn is a senderLimitRig over client, a listener's connection to the server url
// names.
func newSenderLimitRigOn(t *testing.T, client *bus.Client, url string) *senderLimitRig {
	t.Helper()
	rig := &senderLimitRig{knownSessionRig: newKnownSessionRigOn(t, client, url), now: senderLimitStart}
	wakesCtx, stopWakes := context.WithCancel(context.Background())
	t.Cleanup(stopWakes)
	rig.deps.wakes = newWakeReporter(wakesCtx, client)
	conn := testnats.Connect(t, url)
	t.Cleanup(conn.Close)
	wakes, err := conn.SubscribeSync(bus.LegionWakeTopic)
	if err != nil {
		t.Fatalf("subscribe to %s: %v", bus.LegionWakeTopic, err)
	}
	if err := conn.Flush(); err != nil {
		t.Fatalf("flush the wake subscription: %v", err)
	}
	rig.wakes = wakes
	return rig
}

// limit holds every sender to perMinute messages a minute and burst at once from here on, each
// sender's count full, read at the rig's clock.
func (rig *senderLimitRig) limit(perMinute, burst int) {
	rig.deps.senderLimit = newSenderLimit(perMinute, burst, func() time.Time { return rig.now })
}

// live gives sessionID a live entry advertising capabilities. The entry records no consumer
// delivery, so no probe judges it: a send to it is stored for it and answered connected.
func (rig *senderLimitRig) live(t *testing.T, sessionID string, capabilities []string) {
	t.Helper()
	if err := rig.deps.sessions.Put(sessionID, session.SessionEntry{
		MachineID: "test-machine", SelfSubscribed: true, Title: "a counted target", Capabilities: capabilities,
	}); err != nil {
		t.Fatalf("register %s: %v", sessionID, err)
	}
}

// recorded gives sessionID a known-session record advertising capabilities and no entry, as a
// session that stopped leaves behind: a send to it reports a wake before anything else is decided.
func (rig *senderLimitRig) recorded(t *testing.T, sessionID string, capabilities []string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := rig.known.RecordWithin(ctx, session.KnownSession{SessionID: sessionID, Title: "a stopped target", Capabilities: capabilities}); err != nil {
		t.Fatalf("record %s: %v", sessionID, err)
	}
	rig.record(t, sessionID)
}

// sendFrom sends a message from sender to target through the rig's routes. fields override or add
// to the body's, and a nil value removes a field.
func (rig *senderLimitRig) sendFrom(t *testing.T, sender, target string, fields map[string]any) limitedAnswer {
	t.Helper()
	body := map[string]any{"target_session": target, "source_session": sender, "message": countedMessage}
	return serveLimited(t, rig.mux, rig.harness.logs.String, postJSON("/v1/messages/send", messageJSON(t, body, fields)))
}

// publishFrom publishes a message from sender to topic through the rig's routes, with fields as
// sendFrom takes them.
func (rig *senderLimitRig) publishFrom(t *testing.T, sender, topic string, fields map[string]any) limitedAnswer {
	t.Helper()
	body := map[string]any{"topic": topic, "source_session": sender, "message": countedMessage}
	return serveLimited(t, rig.mux, rig.harness.logs.String, postJSON("/v1/messages/publish", messageJSON(t, body, fields)))
}

// spend takes n messages of sender's count, each a publish to a team topic of its own that the
// route answers 200.
func (rig *senderLimitRig) spend(t *testing.T, sender string, n int) {
	t.Helper()
	topic := "notifications.team.sender-limit-spent-" + id.New()
	for i := range n {
		rig.publishFrom(t, sender, topic, nil).requireStatus(t, fmt.Sprintf("%s's message %d of its burst", sender, i+1), http.StatusOK)
	}
}

// requireCount reads sender's count with its next message, a publish to a team topic of its own:
// 200 where the count still holds a message (has), and 429 where it holds none. Each case below
// ends with it, so what a message cost is read off the count itself.
func (rig *senderLimitRig) requireCount(t *testing.T, sender string, has bool) {
	t.Helper()
	next := rig.publishFrom(t, sender, "notifications.team.sender-limit-next-"+id.New(), nil)
	if has {
		next.requireStatus(t, sender+"'s next message, its count still holding one", http.StatusOK)
	} else {
		next.requireStatus(t, sender+"'s next message, its count holding none", http.StatusTooManyRequests)
	}
	next.requireNamed(t, sender+"'s next message", sender)
}

// requireNoWake reads the rig's wake reports until none has arrived for half a second, and fails
// on one of kind naming name. A report is published off the route's path, so the quiet is what
// tells a report that will never come from one still on its way.
func (rig *senderLimitRig) requireNoWake(t *testing.T, kind, name string) {
	t.Helper()
	for {
		msg, err := rig.wakes.NextMsg(500 * time.Millisecond)
		if errors.Is(err, natsgo.ErrTimeout) {
			return
		}
		if err != nil {
			t.Fatalf("read the wake reports: %v", err)
		}
		if wakeNamed(wakeReportIn(t, msg), kind, name) {
			t.Fatalf("a %s wake report named %s, want none", kind, name)
		}
	}
}

// interceptForwards has every role forward the rig's routes make go through intercept, handed the
// forward's number among them, from 1, and the forward itself, which intercept makes or does not,
// and answers in its place. It answers the count of the forwards the routes made so far.
func (rig *senderLimitRig) interceptForwards(intercept func(n int, forward func() (func() (bus.StoredAck, error), error)) (func() (bus.StoredAck, error), error)) *int {
	forwards := 0
	delivery := *rig.deps.delivery
	forwardRole := delivery.forwardRole
	delivery.forwardRole = func(subject string, item contracts.Envelope, msgID string, timeout time.Duration) (func() (bus.StoredAck, error), error) {
		forwards++
		return intercept(forwards, func() (func() (bus.StoredAck, error), error) { return forwardRole(subject, item, msgID, timeout) })
	}
	rig.deps.delivery = &delivery
	return &forwards
}

// loseFirstForwardAck has the stream store the first role forward the rig's routes make and its
// acknowledgement lost on the way back: once the stream stored it, then runs, and the forward's
// wait answers natsgo.ErrTimeout, as the wait for an acknowledgement that never arrives ends.
// Every later forward goes out as it would. It answers the count of the forwards made so far.
func (rig *senderLimitRig) loseFirstForwardAck(then func()) *int {
	return rig.interceptForwards(func(n int, forward func() (func() (bus.StoredAck, error), error)) (func() (bus.StoredAck, error), error) {
		if n > 1 {
			return forward()
		}
		// The first forward is stored, and its acknowledgement is lost on the way back.
		wait, err := forward()
		if err != nil {
			return nil, err
		}
		if _, err := wait(); err != nil {
			return nil, err
		}
		then()
		return func() (bus.StoredAck, error) { return bus.StoredAck{}, natsgo.ErrTimeout }, nil
	})
}

// messageJSON is body with fields over it, a nil value removing a field, as a request's JSON.
func messageJSON(t *testing.T, body, fields map[string]any) string {
	t.Helper()
	for key, value := range fields {
		if value == nil {
			delete(body, key)
			continue
		}
		body[key] = value
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

// limitedAnswer is one message's answer as these tests read it, with the one line the route wrote
// for it.
type limitedAnswer struct {
	status     int
	retryAfter string
	raw        string
	line       map[string]any

	Error             string `json:"error"`
	Reason            string `json:"reason"`
	RetryAfterSeconds int    `json:"retry_after_seconds"`
	MessagesPerMinute int    `json:"messages_per_minute"`
	Burst             int    `json:"burst"`
	EventID           string `json:"event_id"`
	Holder            string `json:"holder"`
	Kept              bool   `json:"kept"`
	Duplicate         bool   `json:"duplicate"`
}

// serveLimited serves request through handler and reads its answer, with the one record line the
// route wrote for it: the line of the route's name among those logs gained while it was served. A
// route writes that line as it answers, so it is there once ServeHTTP returns.
func serveLimited(t *testing.T, handler http.Handler, logs func() string, request *http.Request) limitedAnswer {
	t.Helper()
	name := "listener publish"
	if request.URL.Path == "/v1/messages/send" {
		name = "listener send"
	}
	before := len(logs())
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	answer := limitedAnswer{status: recorder.Code, retryAfter: recorder.Header().Get("Retry-After"), raw: recorder.Body.String()}
	if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil {
		t.Fatalf("decode the answer %s: %v", answer.raw, err)
	}
	written := logs()[before:]
	lines := linesIn(written, name, nil)
	if len(lines) != 1 {
		t.Fatalf("%s %s wrote %d %q lines, want one:\n%s", request.Method, request.URL.Path, len(lines), name, written)
	}
	answer.line = lines[0]
	return answer
}

// requireStatus fails unless the answer's status is status.
func (a limitedAnswer) requireStatus(t *testing.T, what string, status int) {
	t.Helper()
	if a.status != status {
		t.Fatalf("%s: status %d, want %d; body %s", what, a.status, status, a.raw)
	}
}

// requireOutcome fails unless the answer's line records outcome.
func (a limitedAnswer) requireOutcome(t *testing.T, what string, outcome requestOutcome) {
	t.Helper()
	if a.line["outcome"] != string(outcome) {
		t.Fatalf("%s: the line's outcome is %v, want %s; line %v", what, a.line["outcome"], outcome, a.line)
	}
}

// requireNamed fails unless the answer's line names sender in the field sender: the message cost
// sender one, or was refused for its rate.
func (a limitedAnswer) requireNamed(t *testing.T, what, sender string) {
	t.Helper()
	if got := a.line["sender"]; got != sender {
		t.Fatalf("%s: the line's sender is %v, want %s; line %v", what, got, sender, a.line)
	}
}

// requireUnnamed fails when the answer's line has the field sender: the message cost nothing and
// was not refused for its rate.
func (a limitedAnswer) requireUnnamed(t *testing.T, what string) {
	t.Helper()
	if got, named := a.line["sender"]; named {
		t.Fatalf("%s: the line names sender %v, want no sender field: the message cost nothing; line %v", what, got, a.line)
	}
}

// requireRefused fails unless the answer is the sender limit's 429 for a message past sender's
// count, at a limit of perMinute messages a minute and burst at once: Retry-After,
// retry_after_seconds and the sentence each name seconds, and the line records
// sender_rate_limited and names sender.
func (a limitedAnswer) requireRefused(t *testing.T, what, sender string, perMinute, burst, seconds int) {
	t.Helper()
	a.requireStatus(t, what, http.StatusTooManyRequests)
	if a.retryAfter != strconv.Itoa(seconds) {
		t.Fatalf("%s: Retry-After %q, want %d", what, a.retryAfter, seconds)
	}
	if a.Reason != "sender_rate_limited" || a.RetryAfterSeconds != seconds || a.MessagesPerMinute != perMinute || a.Burst != burst {
		t.Fatalf("%s: body %s, want reason sender_rate_limited, retry_after_seconds %d, messages_per_minute %d and burst %d",
			what, a.raw, seconds, perMinute, burst)
	}
	if want := senderLimitSentence(perMinute, burst, seconds); a.Error != want {
		t.Fatalf("%s: error %q, want %q", what, a.Error, want)
	}
	a.requireOutcome(t, what, outcomeSenderRateLimited)
	if a.line["status"] != float64(http.StatusTooManyRequests) {
		t.Fatalf("%s: the line's status is %v, want 429; line %v", what, a.line["status"], a.line)
	}
	a.requireNamed(t, what, sender)
}

// requireRoleSendLinesNameNoSender fails unless logs hold a role send's delivery line, and fails
// when any of those lines has the field sender: the request's own line is the one line of the
// listener that names it.
func requireRoleSendLinesNameNoSender(t *testing.T, logs string) {
	t.Helper()
	lines := linesIn(logs, "listener publish role send", nil)
	if len(lines) == 0 {
		t.Fatal("no role send wrote its delivery line, so nothing shows that line leaves out the sender")
	}
	for _, line := range lines {
		if got, named := line["sender"]; named {
			t.Fatalf("a role send's delivery line names sender %v, want the request's line alone to name it: %v", got, line)
		}
	}
}

// TestSenderLimitRoutes is check a-routes: a message past its sender's count is refused whole on
// both routes, with the 429 and a request line that alone names its sender, before it reaches the
// stream, a role's kept lane, a holder or the wake reports; and a send and a publish spend one
// count.
func TestSenderLimitRoutes(t *testing.T) {
	t.Parallel()
	rig := newSenderLimitRig(t)

	t.Run("send after the burst: 429, Retry-After, body, sentence, line naming the sender, nothing stored", func(t *testing.T) {
		rig.limit(4, 2)
		const sender, target = "ses_routes_send", "ses_routes_send_target"
		rig.live(t, target, nil)
		for i := range 2 {
			rig.sendFrom(t, sender, target, nil).requireStatus(t, fmt.Sprintf("send %d of the burst", i+1), http.StatusOK)
		}
		rig.sendFrom(t, sender, target, nil).requireRefused(t, "the send after the burst", sender, 4, 2, 15)
		if stored := rig.storedOn(t, contracts.LegacyAgentSubject(target)); stored != 2 {
			t.Fatalf("the stream holds %d messages for %s, want the burst's 2 and none of the refused send", stored, target)
		}
		rig.requireCount(t, sender, false)
	})

	t.Run("publish after the burst: 429, Retry-After, body, sentence, line naming the sender, nothing stored", func(t *testing.T) {
		rig.limit(4, 2)
		const sender = "ses_routes_publish"
		topic := "notifications.team.sender-limit-routes-" + id.New()
		for i := range 2 {
			rig.publishFrom(t, sender, topic, nil).requireStatus(t, fmt.Sprintf("publish %d of the burst", i+1), http.StatusOK)
		}
		rig.publishFrom(t, sender, topic, nil).requireRefused(t, "the publish after the burst", sender, 4, 2, 15)
		if stored := rig.storedOn(t, topic); stored != 2 {
			t.Fatalf("the stream holds %d messages on %s, want the burst's 2 and none of the refused publish", stored, topic)
		}
		rig.requireCount(t, sender, false)
	})

	t.Run("role message after the burst: not kept, no wake, and the session that claims the role next gets nothing of it", func(t *testing.T) {
		rig.limit(4, 2)
		const (
			sender   = "ses_routes_role"
			other    = "ses_routes_role_other"
			claimant = "ses_routes_role_claimant"
			role     = "routes-refused-role"
			refused  = "the role message after the burst"
			taken    = "the role message the claimant takes"
		)
		before := len(rig.harness.logs.String())
		rig.spend(t, sender, 2)
		rig.publishFrom(t, sender, contracts.RoleTopicPrefix+role, map[string]any{"message": refused}).
			requireRefused(t, "the role message after the burst", sender, 4, 2, 15)
		// Another sender's message to the role with keep false reports the wake and keeps nothing,
		// so the rig is shown to read the role's wake reports, and that one is the only one.
		unkept := rig.publishFrom(t, other, contracts.RoleTopicPrefix+role, map[string]any{"keep": false})
		unkept.requireStatus(t, "another sender's message with keep false", http.StatusNotFound)
		awaitWake(t, rig.wakes, wakeKindRole, role)
		rig.requireNoWake(t, wakeKindRole, role)
		assertNothingKept(t, rig.harness.client, role)

		rig.register(t, claimant, nil)
		rig.post(t, "/v1/roles/set", `{"session_id":"`+claimant+`","role":"`+role+`"}`)
		delivered := rig.publishFrom(t, other, contracts.RoleTopicPrefix+role, map[string]any{"message": taken})
		delivered.requireStatus(t, "another sender's message to the claimant", http.StatusOK)
		if delivered.Holder != claimant {
			t.Fatalf("the message after the claim went to %q, want %s; body %s", delivered.Holder, claimant, delivered.raw)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		stored, err := readStored(ctx, rig.harness.client, contracts.LegacyAgentSubject(claimant))
		if err != nil {
			t.Fatalf("read what the stream holds for %s: %v", claimant, err)
		}
		var summaries []string
		for _, message := range stored {
			var item contracts.Envelope
			if err := json.Unmarshal(message.Data, &item); err != nil {
				t.Fatalf("decode a message stored for %s: %v", claimant, err)
			}
			summaries = append(summaries, item.PayloadSummary)
		}
		if len(summaries) != 1 || summaries[0] != taken {
			t.Fatalf("the stream holds %q for %s, want only %q and nothing of %q", summaries, claimant, taken, refused)
		}
		requireRoleSendLinesNameNoSender(t, rig.harness.logs.String()[before:])
		rig.requireCount(t, sender, false)
	})

	t.Run("send after the burst to a session with no entry publishes no wake", func(t *testing.T) {
		rig.limit(4, 1)
		const sender, other, gone = "ses_routes_unwoken", "ses_routes_unwoken_other", "ses_routes_unwoken_target"
		rig.spend(t, sender, 1)
		rig.sendFrom(t, sender, gone, nil).requireRefused(t, "the send after the burst", sender, 4, 1, 15)
		// Another sender's send to the same session is answered 404 with its wake reported, so the
		// rig is shown to read the session's wake reports, and that one is the only one.
		rig.sendFrom(t, other, gone, nil).requireStatus(t, "another sender's send", http.StatusNotFound)
		awaitWake(t, rig.wakes, wakeKindAgent, gone)
		rig.requireNoWake(t, wakeKindAgent, gone)
		rig.requireCount(t, sender, false)
	})

	t.Run("a send and a publish from one sender spend one count", func(t *testing.T) {
		rig.limit(4, 1)
		const sendFirst, publishFirst, target = "ses_routes_send_first", "ses_routes_publish_first", "ses_routes_both_target"
		rig.live(t, target, nil)
		topic := "notifications.team.sender-limit-both-" + id.New()
		rig.sendFrom(t, sendFirst, target, nil).requireStatus(t, "the send", http.StatusOK)
		rig.publishFrom(t, sendFirst, topic, nil).requireRefused(t, "the publish after the send", sendFirst, 4, 1, 15)
		rig.publishFrom(t, publishFirst, topic, nil).requireStatus(t, "the publish", http.StatusOK)
		rig.sendFrom(t, publishFirst, target, nil).requireRefused(t, "the send after the publish", publishFirst, 4, 1, 15)
		rig.requireCount(t, sendFirst, false)
		rig.requireCount(t, publishFirst, false)
	})

	t.Run("Retry-After is 10 when two-thirds of a message is missing at 4 a minute", func(t *testing.T) {
		rig.limit(4, 1)
		const sender = "ses_routes_two_thirds"
		rig.spend(t, sender, 1)
		// A message comes back every 15 s, so 5 s after the take a third of it is back.
		rig.now = rig.now.Add(5 * time.Second)
		rig.publishFrom(t, sender, "notifications.team.sender-limit-wait-"+id.New(), nil).
			requireRefused(t, "the message 5 s after the burst", sender, 4, 1, 10)
		rig.requireCount(t, sender, false)
	})

	t.Run("Retry-After is 1 when the wait is below a second at 60000 a minute", func(t *testing.T) {
		rig.limit(60000, 1)
		const sender = "ses_routes_millisecond"
		rig.spend(t, sender, 1)
		rig.publishFrom(t, sender, "notifications.team.sender-limit-wait-"+id.New(), nil).
			requireRefused(t, "the message after the burst", sender, 60000, 1, 1)
		rig.requireCount(t, sender, false)
	})

	t.Run("the sentence reads 1 message a minute and after 1 second at a rate of 1 and a wait below a second", func(t *testing.T) {
		rig.limit(1, 1)
		const sender = "ses_routes_singular"
		rig.spend(t, sender, 1)
		// A message comes back every 60 s, so 59.5 s after the take half a second is left.
		rig.now = rig.now.Add(59500 * time.Millisecond)
		refused := rig.publishFrom(t, sender, "notifications.team.sender-limit-wait-"+id.New(), nil)
		refused.requireRefused(t, "the message 59.5 s after the burst", sender, 1, 1, 1)
		for _, want := range []string{"send 1 message a minute and 1 at once", "after 1 second."} {
			if !strings.Contains(refused.Error, want) {
				t.Fatalf("the sentence %q does not read %q", refused.Error, want)
			}
		}
		rig.requireCount(t, sender, false)
	})

	t.Run("publish after the burst to a topic the stream does not store still answers 400", func(t *testing.T) {
		rig.limit(4, 1)
		const sender = "ses_routes_uncaptured"
		rig.spend(t, sender, 1)
		uncaptured := rig.publishFrom(t, sender, "notifications.teamx.sender-limit", nil)
		uncaptured.requireStatus(t, "the publish to an uncaptured topic", http.StatusBadRequest)
		uncaptured.requireOutcome(t, "the publish to an uncaptured topic", outcomeInvalidRequest)
		if !strings.Contains(uncaptured.Error, "no stream subject captures") {
			t.Fatalf("the 400 says %q, want the refusal of a topic no stream subject captures", uncaptured.Error)
		}
		uncaptured.requireUnnamed(t, "the publish to an uncaptured topic")
		rig.requireCount(t, sender, false)
	})
}

// TestSenderLimitCost is check a-cost: a message costs its sender one where the listener stores
// it, may have stored it or reports a wake for it, and nothing where it refuses it before both, or
// finds it a repeat or an echo. Each case reads what the message cost off the count with the
// sender's next message, and off the field sender on the message's own line.
func TestSenderLimitCost(t *testing.T) {
	t.Parallel()
	rig := newSenderLimitRig(t)
	logs := rig.harness.logs.String
	steer := map[string]any{"payload": `{"delivery":{"mode":"steer"}}`}

	t.Run("send while the bus is down before any wake report (503) costs nothing", func(t *testing.T) {
		frozen, server := newSenderLimitRigOnServer(t)
		frozen.limit(4, 1)
		const sender, target = "ses_cost_bus_down", "ses_cost_bus_down_target"
		thaw := server.Freeze(t)
		t.Cleanup(thaw)
		down := frozen.sendFrom(t, sender, target, nil)
		thaw()
		down.requireStatus(t, "the send while the bus is down", http.StatusServiceUnavailable)
		down.requireOutcome(t, "the send while the bus is down", outcomeBusUnavailable)
		down.requireUnnamed(t, "the send while the bus is down")
		frozen.requireNoWake(t, wakeKindAgent, target)
		frozen.requireCount(t, sender, true)
	})

	// A publish that waits for NATS to reconnect sends nothing: the bus answers the route's
	// deadline before any send (bus.ErrUnacknowledged does not wrap it), so the stream cannot hold
	// the message and the 503 gives the take back.
	t.Run("topic publish while the listener waits for NATS to reconnect, sending nothing (503) costs nothing", func(t *testing.T) {
		down, server := newSenderLimitRigOnServer(t)
		down.limit(4, 1)
		const sender = "ses_cost_reconnecting"
		client := down.harness.client
		server.Stop(t)
		waitFor(t, 10*time.Second, "the listener's connection to reconnect after its server stopped", func() bool {
			return !client.Connected() && client.Connection().IsReconnecting()
		})
		unsent := down.publishFrom(t, sender, "notifications.team.sender-limit-reconnecting-"+id.New(), nil)
		unsent.requireStatus(t, "the publish while NATS reconnects", http.StatusServiceUnavailable)
		unsent.requireOutcome(t, "the publish while NATS reconnects", outcomeBusUnavailable)
		unsent.requireUnnamed(t, "the publish while NATS reconnects")
		server.Start(t)
		waitFor(t, 30*time.Second, "the listener's bus to answer again after its server started", func() bool {
			if !client.Connected() {
				return false
			}
			_, err := client.JS().StreamInfo(bus.Stream)
			return err == nil
		})
		down.requireCount(t, sender, true)
	})

	t.Run("duplicate of a stored message (a send repeated under the transport's UUID key) costs nothing", func(t *testing.T) {
		rig.limit(4, 2)
		const sender, target = "ses_cost_send_repeat", "ses_cost_send_repeat_target"
		rig.live(t, target, nil)
		key := map[string]any{"idempotency_key": uuid.NewString()}
		first := rig.sendFrom(t, sender, target, key)
		first.requireStatus(t, "the first send", http.StatusOK)
		first.requireNamed(t, "the first send", sender)
		repeat := rig.sendFrom(t, sender, target, key)
		repeat.requireStatus(t, "the repeated send", http.StatusOK)
		if !repeat.Duplicate {
			t.Fatalf("the repeated send under one UUID key answered %s, want duplicate", repeat.raw)
		}
		repeat.requireUnnamed(t, "the repeated send")
		rig.requireCount(t, sender, true)
	})

	t.Run("duplicate of a stored message (a topic publish repeated under one idempotency key) costs nothing", func(t *testing.T) {
		rig.limit(4, 2)
		const sender = "ses_cost_publish_repeat"
		topic := "notifications.team.sender-limit-repeat-" + id.New()
		key := map[string]any{"idempotency_key": uuid.NewString()}
		first := rig.publishFrom(t, sender, topic, key)
		first.requireStatus(t, "the first publish", http.StatusOK)
		first.requireNamed(t, "the first publish", sender)
		repeat := rig.publishFrom(t, sender, topic, key)
		repeat.requireStatus(t, "the repeated publish", http.StatusOK)
		repeat.requireUnnamed(t, "the repeated publish")
		if stored := rig.storedOn(t, topic); stored != 1 {
			t.Fatalf("the stream holds %d messages on %s, want the one the repeat duplicates", stored, topic)
		}
		rig.requireCount(t, sender, true)
	})

	t.Run("duplicate after a wake report costs nothing", func(t *testing.T) {
		rig.limit(4, 2)
		const sender, target = "ses_cost_woken_repeat", "ses_cost_woken_repeat_target"
		rig.recorded(t, target, nil)
		key := map[string]any{"idempotency_key": uuid.NewString()}
		first := rig.sendFrom(t, sender, target, key)
		first.requireStatus(t, "the first send", http.StatusOK)
		first.requireNamed(t, "the first send", sender)
		awaitWake(t, rig.wakes, wakeKindAgent, target)
		repeat := rig.sendFrom(t, sender, target, key)
		repeat.requireStatus(t, "the repeated send", http.StatusOK)
		if !repeat.Duplicate {
			t.Fatalf("the repeated send under one UUID key answered %s, want duplicate", repeat.raw)
		}
		awaitWake(t, rig.wakes, wakeKindAgent, target)
		repeat.requireUnnamed(t, "the repeated send")
		rig.requireCount(t, sender, true)
	})

	t.Run("echo of the sender's own message costs nothing", func(t *testing.T) {
		rig.limit(4, 1)
		const sender, role = "ses_cost_echo", "cost-echo"
		claimRoleFor(t, rig.deps.registry, rig.deps.sessions, sender, role, nil)
		before := len(logs())
		echo := rig.publishFrom(t, sender, contracts.RoleTopicPrefix+role, nil)
		echo.requireStatus(t, "the message to the sender's own role", http.StatusOK)
		if echo.Holder != sender {
			t.Fatalf("the echo answered holder %q, want the sender %s; body %s", echo.Holder, sender, echo.raw)
		}
		echo.requireUnnamed(t, "the echo")
		requireRoleSendLinesNameNoSender(t, logs()[before:])
		rig.requireCount(t, sender, true)
	})

	t.Run("repeat of a kept copy (202 after the wake) costs nothing", func(t *testing.T) {
		rig.limit(4, 2)
		const sender, role = "ses_cost_kept_repeat", "cost-kept-repeat"
		key := map[string]any{"idempotency_key": uuid.NewString()}
		before := len(logs())
		first := rig.publishFrom(t, sender, contracts.RoleTopicPrefix+role, key)
		first.requireStatus(t, "the first kept message", http.StatusAccepted)
		first.requireNamed(t, "the first kept message", sender)
		awaitWake(t, rig.wakes, wakeKindRole, role)
		repeat := rig.publishFrom(t, sender, contracts.RoleTopicPrefix+role, key)
		repeat.requireStatus(t, "the repeated kept message", http.StatusAccepted)
		if !repeat.Kept || repeat.EventID != first.EventID {
			t.Fatalf("the repeat answered %s, want kept with the first copy's event %s", repeat.raw, first.EventID)
		}
		awaitWake(t, rig.wakes, wakeKindRole, role)
		repeat.requireUnnamed(t, "the repeated kept message")
		requireRoleSendLinesNameNoSender(t, logs()[before:])
		rig.requireCount(t, sender, true)
	})

	t.Run("403 for a target with an entry costs nothing", func(t *testing.T) {
		rig.limit(4, 1)
		const sender, target = "ses_cost_entry_403", "ses_cost_entry_403_target"
		rig.live(t, target, []string{"aside"})
		refused := rig.sendFrom(t, sender, target, steer)
		refused.requireStatus(t, "the steer to a target that takes aside alone", http.StatusForbidden)
		refused.requireOutcome(t, "the steer to a target that takes aside alone", outcomeCapabilityRefused)
		refused.requireUnnamed(t, "the steer to a target that takes aside alone")
		rig.requireCount(t, sender, true)
	})

	t.Run("409 for a target with an entry costs nothing", func(t *testing.T) {
		rig.limit(4, 1)
		const sender, target = "ses_cost_entry_409", "ses_cost_entry_409_target"
		// A registration in consumer mode that nothing holds the relay subject of: the probe meets
		// no responders, so the entry reads stopped and no wake is reported for it.
		rig.register(t, target, nil)
		stopped := rig.sendFrom(t, sender, target, map[string]any{"connected_only": true})
		stopped.requireStatus(t, "the connected_only send to a stopped entry", http.StatusConflict)
		stopped.requireOutcome(t, "the connected_only send to a stopped entry", outcomeSessionStopped)
		stopped.requireUnnamed(t, "the connected_only send to a stopped entry")
		rig.requireNoWake(t, wakeKindAgent, target)
		rig.requireCount(t, sender, true)
	})

	t.Run("role message its holder cannot take (403) costs nothing", func(t *testing.T) {
		rig.limit(4, 1)
		const sender, holder, role = "ses_cost_role_403", "ses_cost_role_403_holder", "cost-role-refused"
		claimRoleFor(t, rig.deps.registry, rig.deps.sessions, holder, role, []string{"aside"})
		before := len(logs())
		refused := rig.publishFrom(t, sender, contracts.RoleTopicPrefix+role, steer)
		refused.requireStatus(t, "the steer to a holder that takes aside alone", http.StatusForbidden)
		refused.requireUnnamed(t, "the steer to a holder that takes aside alone")
		requireRoleSendLinesNameNoSender(t, logs()[before:])
		rig.requireCount(t, sender, true)
	})

	t.Run("role message repeated to the holder that has it costs nothing", func(t *testing.T) {
		rig.limit(4, 2)
		const sender, holder, role = "ses_cost_role_repeat", "ses_cost_role_repeat_holder", "cost-role-repeat"
		claimRoleFor(t, rig.deps.registry, rig.deps.sessions, holder, role, nil)
		key := map[string]any{"idempotency_key": uuid.NewString()}
		before := len(logs())
		first := rig.publishFrom(t, sender, contracts.RoleTopicPrefix+role, key)
		first.requireStatus(t, "the first role message", http.StatusOK)
		first.requireNamed(t, "the first role message", sender)
		repeat := rig.publishFrom(t, sender, contracts.RoleTopicPrefix+role, key)
		repeat.requireStatus(t, "the repeated role message", http.StatusOK)
		repeat.requireUnnamed(t, "the repeated role message")
		if stored := rig.storedOn(t, contracts.LegacyAgentSubject(holder)); stored != 1 {
			t.Fatalf("the stream holds %d forwards for %s, want the first message's one", stored, holder)
		}
		requireRoleSendLinesNameNoSender(t, logs()[before:])
		rig.requireCount(t, sender, true)
	})

	t.Run("200 that stores a message costs one", func(t *testing.T) {
		rig.limit(4, 1)
		const sender, target = "ses_cost_stored", "ses_cost_stored_target"
		rig.live(t, target, nil)
		stored := rig.sendFrom(t, sender, target, nil)
		stored.requireStatus(t, "the send", http.StatusOK)
		stored.requireNamed(t, "the send", sender)
		rig.requireCount(t, sender, false)
	})

	t.Run("role message delivered to its live holder (200) costs one", func(t *testing.T) {
		rig.limit(4, 1)
		const sender, holder, role = "ses_cost_role_delivered", "ses_cost_role_delivered_holder", "cost-role-delivered"
		claimRoleFor(t, rig.deps.registry, rig.deps.sessions, holder, role, nil)
		before := len(logs())
		delivered := rig.publishFrom(t, sender, contracts.RoleTopicPrefix+role, nil)
		delivered.requireStatus(t, "the role message", http.StatusOK)
		if delivered.Holder != holder {
			t.Fatalf("the role message went to %q, want %s; body %s", delivered.Holder, holder, delivered.raw)
		}
		delivered.requireNamed(t, "the role message", sender)
		requireRoleSendLinesNameNoSender(t, logs()[before:])
		rig.requireCount(t, sender, false)
	})

	t.Run("role message kept for a later holder (202) costs one", func(t *testing.T) {
		rig.limit(4, 1)
		const sender, role = "ses_cost_kept", "cost-kept"
		before := len(logs())
		kept := rig.publishFrom(t, sender, contracts.RoleTopicPrefix+role, nil)
		kept.requireStatus(t, "the message to a role nobody holds", http.StatusAccepted)
		if !kept.Kept {
			t.Fatalf("the message to a role nobody holds answered %s, want kept", kept.raw)
		}
		kept.requireNamed(t, "the kept message", sender)
		awaitWake(t, rig.wakes, wakeKindRole, role)
		requireRoleSendLinesNameNoSender(t, logs()[before:])
		rig.requireCount(t, sender, false)
	})

	// A keep whose first send went out unanswered and whose resend the stream refused as in
	// process may have stored the copy itself, and here it did: the next send's duplicate names
	// the keep's own copy, which is no repeat (publishPastInProcess answers it Resent), so the
	// 202 costs one, as a copy the keep stored does, and the wake's mark is not forgiven.
	t.Run("role message kept after its own unanswered send stored the copy (202) costs one", func(t *testing.T) {
		rig.limit(4, 1)
		const sender, role = "ses_cost_kept_own_send", "cost-kept-own-send"
		original := rig.deps.delivery
		t.Cleanup(func() { rig.deps.delivery = original })
		delivery := *original
		sends := 0
		delivery.keepPublish = func(ctx context.Context, subject string, item contracts.Envelope, msgID string) (bus.StoredAck, error) {
			sends++
			ack, err := original.keepPublish(ctx, subject, item, msgID)
			if sends == 1 && err == nil {
				return bus.StoredAck{}, fmt.Errorf("%w: %w", bus.ErrUnacknowledged,
					fmt.Errorf("%w: nats: duplicate message id is in process", bus.ErrDuplicateInProcess))
			}
			return ack, err
		}
		rig.deps.delivery = &delivery
		kept := rig.publishFrom(t, sender, contracts.RoleTopicPrefix+role, nil)
		kept.requireStatus(t, "the kept message whose first send went unanswered", http.StatusAccepted)
		if !kept.Kept || sends != 2 {
			t.Fatalf("the message answered %s after %d keep sends, want kept after two", kept.raw, sends)
		}
		kept.requireNamed(t, "the kept message whose first send went unanswered", sender)
		awaitWake(t, rig.wakes, wakeKindRole, role)
		rig.requireCount(t, sender, false)
	})

	t.Run("role message with no holder and keep false (404 after a wake report) costs one", func(t *testing.T) {
		rig.limit(4, 1)
		const sender, role = "ses_cost_unkept", "cost-unkept"
		before := len(logs())
		unkept := rig.publishFrom(t, sender, contracts.RoleTopicPrefix+role, map[string]any{"keep": false})
		unkept.requireStatus(t, "the message with keep false", http.StatusNotFound)
		unkept.requireOutcome(t, "the message with keep false", outcomeRoleUnclaimed)
		unkept.requireNamed(t, "the message with keep false", sender)
		awaitWake(t, rig.wakes, wakeKindRole, role)
		assertNothingKept(t, rig.harness.client, role)
		requireRoleSendLinesNameNoSender(t, logs()[before:])
		rig.requireCount(t, sender, false)
	})

	t.Run("send to a session with no entry (404 after a wake report) costs one", func(t *testing.T) {
		rig.limit(4, 1)
		const sender, gone = "ses_cost_gone", "ses_cost_gone_target"
		missing := rig.sendFrom(t, sender, gone, nil)
		missing.requireStatus(t, "the send to a session nobody knows", http.StatusNotFound)
		missing.requireOutcome(t, "the send to a session nobody knows", outcomeNoLiveSession)
		missing.requireNamed(t, "the send to a session nobody knows", sender)
		awaitWake(t, rig.wakes, wakeKindAgent, gone)
		rig.requireCount(t, sender, false)
	})

	t.Run("403 after a wake report costs one", func(t *testing.T) {
		rig.limit(4, 1)
		const sender, target = "ses_cost_record_403", "ses_cost_record_403_target"
		rig.recorded(t, target, []string{"aside"})
		refused := rig.sendFrom(t, sender, target, steer)
		refused.requireStatus(t, "the steer to a record that takes aside alone", http.StatusForbidden)
		refused.requireOutcome(t, "the steer to a record that takes aside alone", outcomeCapabilityRefused)
		refused.requireNamed(t, "the steer to a record that takes aside alone", sender)
		awaitWake(t, rig.wakes, wakeKindAgent, target)
		rig.requireCount(t, sender, false)
	})

	t.Run("409 after a wake report costs one", func(t *testing.T) {
		rig.limit(4, 1)
		const sender, target = "ses_cost_record_409", "ses_cost_record_409_target"
		rig.recorded(t, target, nil)
		stopped := rig.sendFrom(t, sender, target, map[string]any{"connected_only": true})
		stopped.requireStatus(t, "the connected_only send to a record", http.StatusConflict)
		stopped.requireOutcome(t, "the connected_only send to a record", outcomeSessionStopped)
		stopped.requireNamed(t, "the connected_only send to a record", sender)
		awaitWake(t, rig.wakes, wakeKindAgent, target)
		rig.requireCount(t, sender, false)
	})

	t.Run("send to a session with only a record while the stream is full (503 stream_full after the wake) costs one", func(t *testing.T) {
		client := setupPublishTestClient(t, bus.WithStreamMaxBytes(16<<10))
		full := newSenderLimitRigOn(t, client, testnats.URL(t))
		full.limit(4, 1)
		const sender, target = "ses_cost_full", "ses_cost_full_target"
		full.recorded(t, target, nil)
		bustest.FillStream(t, client, 64)
		refused := full.sendFrom(t, sender, target, nil)
		refused.requireStatus(t, "the send to a full stream", http.StatusServiceUnavailable)
		refused.requireOutcome(t, "the send to a full stream", outcomeStreamFull)
		refused.requireNamed(t, "the send to a full stream", sender)
		awaitWake(t, full.wakes, wakeKindAgent, target)
		full.requireCount(t, sender, false)
	})

	// A publish whose acknowledgement does not arrive in time may have stored the message, so the
	// 503 costs one (bus.ErrUnacknowledged). The server stops answering before the publish route's
	// one read, the sender stamp's, which only enriches the envelope, so the publish goes out into
	// the frozen connection and waits out the route's deadline. Thawed, the server stores what the
	// listener sent.
	t.Run("topic publish answered 503 after the listener sent it, its acknowledgement lost, costs one", func(t *testing.T) {
		lost, server := newSenderLimitRigOnServer(t)
		lost.limit(4, 1)
		const sender = "ses_cost_publish_unacknowledged"
		topic := "notifications.team.sender-limit-unacknowledged-" + id.New()
		thaw := server.Freeze(t)
		t.Cleanup(thaw)
		unacknowledged := lost.publishFrom(t, sender, topic, nil)
		thaw()
		unacknowledged.requireStatus(t, "the publish whose acknowledgement was lost", http.StatusServiceUnavailable)
		unacknowledged.requireOutcome(t, "the publish whose acknowledgement was lost", outcomeBusUnavailable)
		unacknowledged.requireNamed(t, "the publish whose acknowledgement was lost", sender)
		waitFor(t, 10*time.Second, "the stream to store the publish the frozen server held", func() bool {
			return lost.storedOn(t, topic) == 1
		})
		lost.requireCount(t, sender, false)
	})

	// The same holds for a send to a target with an entry. Its target is read before the publish,
	// so the server stops answering later: a stand-in for the target's plugin freezes it as the
	// send's connection probe reaches it and answers nothing, which the probe reads as connected.
	t.Run("send to a live target answered 503 after the listener sent it, its acknowledgement lost, costs one", func(t *testing.T) {
		lost, server := newSenderLimitRigOnServer(t)
		lost.limit(4, 1)
		const sender, target = "ses_cost_send_unacknowledged", "ses_cost_send_unacknowledged_target"
		lost.register(t, target, nil)
		thaws := make(chan func(), 1)
		t.Cleanup(func() {
			select {
			case thaw := <-thaws:
				thaw()
			default:
			}
		})
		var freeze sync.Once
		plugin := testnats.Connect(t, lost.url)
		t.Cleanup(plugin.Close)
		if _, err := plugin.Subscribe(bus.DeliverSubject(target), func(msg *natsgo.Msg) {
			if msg.Header.Get(bus.ConnectionProbeHeader) != "" {
				freeze.Do(func() { thaws <- server.Freeze(t) })
			}
		}); err != nil {
			t.Fatalf("subscribe %s's relay subject: %v", target, err)
		}
		if err := plugin.Flush(); err != nil {
			t.Fatalf("flush the relay subscription: %v", err)
		}
		unacknowledged := lost.sendFrom(t, sender, target, nil)
		select {
		case thaw := <-thaws:
			thaw()
		default:
			t.Fatalf("the send probed nothing, so its publish met no frozen server; body %s", unacknowledged.raw)
		}
		unacknowledged.requireStatus(t, "the send whose acknowledgement was lost", http.StatusServiceUnavailable)
		unacknowledged.requireOutcome(t, "the send whose acknowledgement was lost", outcomeBusUnavailable)
		unacknowledged.requireNamed(t, "the send whose acknowledgement was lost", sender)
		waitFor(t, 10*time.Second, "the stream to store the send the frozen server held", func() bool {
			return lost.storedOn(t, contracts.LegacyAgentSubject(target)) == 1
		})
		lost.requireCount(t, sender, false)
	})

	t.Run("role send whose receipt times out (504) costs one", func(t *testing.T) {
		frozen, server := newSenderLimitRigOnServer(t)
		frozen.limit(4, 1)
		const sender, holder, role = "ses_cost_timed_out", "ses_cost_timed_out_holder", "cost-timed-out"
		claimRoleFor(t, frozen.deps.registry, frozen.deps.sessions, holder, role, nil)
		// The holder answers no receipt: a forward is delivered once the stream acknowledges
		// storing it (startRoleForward). The server stops answering as the first forward goes
		// out, after the send resolved its holder live, so no acknowledgement arrives and every
		// later resolution waits out the window.
		var thaw func()
		frozen.interceptForwards(func(_ int, forward func() (func() (bus.StoredAck, error), error)) (func() (bus.StoredAck, error), error) {
			if thaw == nil {
				thaw = server.Freeze(t)
				t.Cleanup(thaw)
			}
			return forward()
		})
		before := len(frozen.harness.logs.String())
		timedOut := frozen.publishFrom(t, sender, contracts.RoleTopicPrefix+role, nil)
		if thaw == nil {
			t.Fatalf("the role send forwarded nothing, so no acknowledgement was stopped; body %s", timedOut.raw)
		}
		thaw()
		timedOut.requireStatus(t, "the role send whose window ended", http.StatusGatewayTimeout)
		timedOut.requireOutcome(t, "the role send whose window ended", outcomeDeliveryFailed)
		timedOut.requireNamed(t, "the role send whose window ended", sender)
		requireRoleSendLinesNameNoSender(t, frozen.harness.logs.String()[before:])
		frozen.requireCount(t, sender, false)
	})

	// A role send that forwarded once and did not hear the stream's acknowledgement may have
	// stored the message for that holder, so R3 has it cost one whatever the send answers next.
	// Here the stream did store it, and the role then moves to a holder that takes aside alone, so
	// the next pass answers the steer 403 (settledBeforeForward). The same holds for every answer
	// that pass can give after an attempt: an echo and a repeat as well as the 403.
	t.Run("role send answered 403 by a later holder after a forward the stream stored costs one", func(t *testing.T) {
		moved := newSenderLimitRig(t)
		moved.limit(4, 1)
		const sender, first, next, role = "ses_cost_moved", "ses_cost_moved_first", "ses_cost_moved_next", "cost-moved"
		claimRoleFor(t, moved.deps.registry, moved.deps.sessions, first, role, []string{"steer"})
		forwards := moved.loseFirstForwardAck(func() {
			claimRoleFor(t, moved.deps.registry, moved.deps.sessions, next, role, []string{"aside"})
		})
		answer := moved.publishFrom(t, sender, contracts.RoleTopicPrefix+role, steer)
		if *forwards != 1 {
			t.Fatalf("the role send forwarded %d times, want the first holder's one; body %s", *forwards, answer.raw)
		}
		if stored := moved.storedOn(t, contracts.LegacyAgentSubject(first)); stored != 1 {
			t.Fatalf("the stream holds %d forwards for %s, want the one whose acknowledgement was lost", stored, first)
		}
		answer.requireStatus(t, "the steer the next holder cannot take", http.StatusForbidden)
		answer.requireNamed(t, "the steer the stream stored for the first holder", sender)
		moved.requireCount(t, sender, false)
	})

	// A repeat the kept lane answers after such a forward costs one as well (roleSend.kept). Here
	// an earlier attempt of the message, under the same idempotency key and an event id of its own,
	// waits on the role's kept lane. The stream stores the first forward, its acknowledgement is
	// lost and the holder gives the role up, so the next pass finds the role unclaimed and answers
	// the earlier attempt's kept copy.
	t.Run("role send answered 202 for a repeat of a kept copy after a forward the stream stored costs one", func(t *testing.T) {
		released := newSenderLimitRig(t)
		released.limit(4, 1)
		const sender, holder, role = "ses_cost_released", "ses_cost_released_holder", "cost-released"
		idempotencyKey := uuid.NewString()
		earlier := listenerTestEnvelope(contracts.RoleTopicPrefix+role, publishDedupePrefix+idempotencyKey)
		earlier.SourceSession = sender
		if _, err := keepRoleMessage(*released.deps.delivery, role, earlier); err != nil {
			t.Fatalf("keep the earlier attempt; the listener log says why:\n%s", released.harness.logs.String())
		}
		claimRoleFor(t, released.deps.registry, released.deps.sessions, holder, role, nil)
		forwards := released.loseFirstForwardAck(func() {
			// The claim names no process, so the release is the forced one DELETE
			// /v1/interests/<session_id> makes (store.RoleReleaseAuthority.Force).
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if _, err := released.deps.registry.Remove(ctx, holder, []string{contracts.RoleTopicPrefix + role}, store.RoleReleaseAuthority{Force: true}); err != nil {
				t.Fatalf("release %s's claim of %s: %v", holder, role, err)
			}
		})
		answer := released.publishFrom(t, sender, contracts.RoleTopicPrefix+role, map[string]any{"idempotency_key": idempotencyKey})
		if *forwards != 1 {
			t.Fatalf("the role send forwarded %d times, want the holder's one; body %s", *forwards, answer.raw)
		}
		if stored := released.storedOn(t, contracts.LegacyAgentSubject(holder)); stored != 1 {
			t.Fatalf("the stream holds %d forwards for %s, want the one whose acknowledgement was lost", stored, holder)
		}
		answer.requireStatus(t, "the role send that found the role unclaimed", http.StatusAccepted)
		if !answer.Kept || answer.EventID != earlier.EventID {
			t.Fatalf("the role send answered %s, want kept with the earlier attempt's event %s", answer.raw, earlier.EventID)
		}
		answer.requireNamed(t, "the role message the stream stored for the holder", sender)
		awaitWake(t, released.wakes, wakeKindRole, role)
		released.requireCount(t, sender, false)
	})
}

// TestSenderLimitSenders is check a-senders: the limit counts the session a secrets-broker token
// proves, else the request's source_session, and only for a message whose source is agent or
// absent. A message with no sender takes nothing, and so does one from Dispatch, a person (source
// human) or Envoy itself, with a secrets-broker token or without one.
func TestSenderLimitSenders(t *testing.T) {
	t.Parallel()
	t.Run("a sender a secrets-broker token proves is counted and named in the field sender", func(t *testing.T) {
		rig := newBrokerRig(t)
		const bound, token = "ses_senders_broker", "access-token-of-a-counted-session"
		rig.broker.Says(token, sessionAnswer(bound, "ada@example.com", rig.caller.JKT()))
		for _, tc := range []struct {
			name   string
			fields map[string]any
		}{
			{name: "source_session is the token's session", fields: map[string]any{"source_session": bound}},
			{name: "no source_session"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				rig.deps.senderLimit = newSenderLimit(4, 1, func() time.Time { return senderLimitStart })
				topic := "notifications.team.sender-limit-broker-" + id.New()
				publish := func() limitedAnswer {
					t.Helper()
					body := messageJSON(t, map[string]any{"topic": topic, "message": countedMessage}, tc.fields)
					return serveLimited(t, rig.handler, rig.logs.String, brokerTokenRequest(t, rig.caller, http.MethodPost, "/v1/messages/publish", token, body))
				}
				first := publish()
				first.requireStatus(t, "the token's message", http.StatusOK)
				first.requireNamed(t, "the token's message", bound)
				publish().requireRefused(t, "the token's next message", bound, 4, 1, 15)
			})
		}
	})

	t.Run("a secrets-broker token does not make a message with another source count", func(t *testing.T) {
		rig := newBrokerRig(t)
		const bound, token = "ses_senders_broker_sources", "access-token-of-a-session-sending-another-source"
		rig.broker.Says(token, sessionAnswer(bound, "ada@example.com", rig.caller.JKT()))
		for _, source := range []string{"dispatch", "envoy", "human"} {
			t.Run("source "+source, func(t *testing.T) {
				rig.deps.senderLimit = newSenderLimit(4, 1, func() time.Time { return senderLimitStart })
				topic := "notifications.team.sender-limit-broker-" + source + "-" + id.New()
				publish := func(fields map[string]any) limitedAnswer {
					t.Helper()
					body := messageJSON(t, map[string]any{"topic": topic, "message": countedMessage}, fields)
					return serveLimited(t, rig.handler, rig.logs.String, brokerTokenRequest(t, rig.caller, http.MethodPost, "/v1/messages/publish", token, body))
				}
				// Two messages past a burst of 1, the first naming the token's session and the
				// second no session: each answers 200, and its line names no sender.
				for i, fields := range []map[string]any{
					{"source": source, "source_session": bound},
					{"source": source},
				} {
					what := fmt.Sprintf("the token's %s message %d", source, i+1)
					message := publish(fields)
					message.requireStatus(t, what, http.StatusOK)
					message.requireUnnamed(t, what)
				}
				// The count is still full: the token's next message, with no source, takes it.
				next := publish(nil)
				next.requireStatus(t, "the token's next message, with no source", http.StatusOK)
				next.requireNamed(t, "the token's next message, with no source", bound)
			})
		}
	})

	rig := newSenderLimitRig(t)
	topic := func() string { return "notifications.team.sender-limit-senders-" + id.New() }

	t.Run("a sender named by source_session alone is counted", func(t *testing.T) {
		rig.limit(4, 1)
		const sender = "ses_senders_named"
		// The body names no source, which the route reads as agent.
		counted := rig.publishFrom(t, sender, topic(), nil)
		counted.requireStatus(t, "the message", http.StatusOK)
		counted.requireNamed(t, "the message", sender)
		rig.requireCount(t, sender, false)
	})

	// requireUncounted holds each of messages, sent past a burst of 1, to a 200 and then to a line
	// with no sender: the statuses first, so a limit that counted them fails on the message after
	// the burst being refused.
	requireUncounted := func(t *testing.T, what string, messages []limitedAnswer) {
		t.Helper()
		for i, message := range messages {
			message.requireStatus(t, fmt.Sprintf("%s %d", what, i+1), http.StatusOK)
		}
		for i, message := range messages {
			message.requireUnnamed(t, fmt.Sprintf("%s %d", what, i+1))
		}
	}

	t.Run("no source and no sender, as the Legion daemon's notices send, is not counted", func(t *testing.T) {
		rig.limit(4, 1)
		var notices []limitedAnswer
		for range 3 {
			notices = append(notices, rig.publishFrom(t, "", topic(), map[string]any{"source_session": nil}))
		}
		requireUncounted(t, "notice", notices)
	})

	for _, source := range []string{"dispatch", "envoy", "human"} {
		t.Run("source "+source+" with a source_session is not counted", func(t *testing.T) {
			rig.limit(4, 1)
			sender := "ses_senders_" + source
			var messages []limitedAnswer
			for range 2 {
				messages = append(messages, rig.publishFrom(t, sender, topic(), map[string]any{"source": source}))
			}
			requireUncounted(t, source+" message", messages)
			rig.requireCount(t, sender, true)
		})
	}
}
