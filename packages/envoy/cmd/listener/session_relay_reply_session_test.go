package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/id"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/metrics"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
	"github.com/sjawhar/envoy/natsgrant"
)

// relayRefusedReplyLine is the line the relay writes for a reply whose subject names another
// session than the one its frame was sent to (sessionRelays.onReply).
const relayRefusedReplyLine = "session relay refused a reply naming another session than the one its frame was sent to; the frame is unchanged"

// sessionUser is a NATS user holding one session's own grant, the one the auth service admits a
// session with (natsgrant.Session): publish its replies (bus.SessionReplies) and its agent stream
// frames, subscribe its relay subject and its agent stream control subject. A publish the server
// refuses reaches the client only as an asynchronous error, so each refusal on its connection is
// recorded by subject.
type sessionUser struct {
	id, seed, public string
	mu               sync.Mutex
	refused          []string
}

func newSessionUser(t *testing.T, prefix string) *sessionUser {
	t.Helper()
	seed, public := testnats.User(t)
	return &sessionUser{id: prefix + id.New(), seed: seed, public: public}
}

// entry is the user's line of the server's authorization: natsgrant.Session's lists, deny lists
// included, as the auth service answers them.
func (s *sessionUser) entry() string {
	grant, err := natsgrant.Session(s.id)
	if err != nil {
		panic(err)
	}
	quote := func(subjects []string) string {
		quoted := make([]string, len(subjects))
		for i, subject := range subjects {
			quoted[i] = strconv.Quote(subject)
		}
		return strings.Join(quoted, ", ")
	}
	rules := func(allow, deny []string) string {
		if len(deny) == 0 {
			return fmt.Sprintf("{ allow: [%s] }", quote(allow))
		}
		return fmt.Sprintf("{ allow: [%s], deny: [%s] }", quote(allow), quote(deny))
	}
	return fmt.Sprintf("{ nkey: %q, permissions: { publish: %s, subscribe: %s } }",
		s.public, rules(grant.Publish, grant.PublishDeny), rules(grant.Subscribe, grant.SubscribeDeny))
}

// connect connects to uri as the user and records every publish the server refuses it.
func (s *sessionUser) connect(t *testing.T, uri string) *natsgo.Conn {
	t.Helper()
	conn, err := testnats.ConnectSeed(t, uri, s.seed, natsgo.ErrorHandler(func(_ *natsgo.Conn, _ *natsgo.Subscription, err error) {
		const violation = `Permissions Violation for Publish to "`
		text := err.Error()
		at := strings.Index(text, violation)
		if at < 0 {
			return
		}
		subject, _, _ := strings.Cut(text[at+len(violation):], `"`)
		s.mu.Lock()
		defer s.mu.Unlock()
		s.refused = append(s.refused, subject)
	}))
	if err != nil {
		t.Fatalf("connect as session %s: %v", s.id, err)
	}
	return conn
}

// refusals is every subject the server has refused the user a publish to so far.
func (s *sessionUser) refusals() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.refused)
}

// startSessionGrantHub starts a NATS server whose listener user may do anything and whose other
// users are sessions, each holding its own grant (sessionUser), and a listener binary as machine
// machineID connected as the listener user. It returns the server's URL, the same URL carrying the
// listener user's credentials, and the listener.
func startSessionGrantHub(t *testing.T, machineID string, sessions ...*sessionUser) (string, string, *listenerProcess) {
	t.Helper()
	entries := make([]string, len(sessions))
	for i, user := range sessions {
		entries[i] = user.entry()
	}
	config := fmt.Sprintf(`jetstream {}
authorization {
  users = [
    { user: listener, password: listener, permissions: { publish: { allow: [">"] }, subscribe: { allow: [">"] } } }
    %s
  ]
}
`, strings.Join(entries, "\n    "))
	_, uri := testnats.StartConfig(t, config, []string{"-js"}, false)
	listenerURI := asListenerUser(t, uri)
	listener := startListenerProcess(t, cmdtest.Build(t, "envoy-listener"), listenerURI, machineID)
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("the listener's output:\n%s", listener.Output.String())
		}
	})
	listener.waitHealthy(t)
	return uri, listenerURI, listener
}

// requireStillPending fails the test unless sessionID's consumer still holds the message at seq for
// an ack: one message pending and the ack floor below it.
func requireStillPending(t *testing.T, consumers *bus.SessionConsumers, sessionID string, seq uint64) {
	t.Helper()
	info, err := consumers.Info(t.Context(), sessionID)
	if err != nil {
		t.Fatalf("read the consumer of %s: %v", sessionID, err)
	}
	if info.NumAckPending != 1 || info.AckFloor.Stream >= seq {
		t.Fatalf("the consumer of %s has %d pending ack, ack floor %d; want the frame at seq %d still pending, acknowledged by nobody",
			sessionID, info.NumAckPending, info.AckFloor.Stream, seq)
	}
}

// clientResendPause is how long the client a session runs pauses before it sends a refused call
// again: envoy-client's LISTENER_CALL_RETRY_DELAY_MS (packages/contracts/src/envoy-session.ts:67).
const clientResendPause = 250 * time.Millisecond

// registrationSpare is what registerAsTheClientDoes leaves of the test binary's deadline for the
// row to fail in and run its cleanups when the listener never takes the registration.
const registrationSpare = 30 * time.Second

// registerAsTheClientDoes posts body to the listener's /v1/interests/subscribe as the test's
// shared-token caller and returns once the listener takes it (200), sending a 5xx again as the
// client a session runs does. envoy-client sends a 5xx once more after 250 ms (the 5xx branch of
// createEnvoyClient's request, packages/envoy-client/src/transport.ts; LISTENER_CALL_RETRY_DELAY_MS
// and LISTENER_CALL_ATTEMPTS, packages/contracts/src/envoy-session.ts:67 and :75), and pi-envoy
// and claude-envoy register again 20 to 40 s after a failed registration and then at every
// heartbeat (each plugin's heartbeat and its armRetry, whose delay is heartbeatRetryDelayMs of
// packages/envoy-client/src/envoy-settings.ts: ensureHeartbeat in
// packages/pi-envoy/extensions/envoy.ts, heartbeat in
// packages/claude-envoy/src/envoy-channel-server.ts). So a registration refused because the
// server answered the listener late - the 503 consumer_error of a session consumer call past its
// bound, or the bus-unavailable 503 of store work past its own - is a registration the session
// makes again, not one it never makes.
//
// Each 5xx is sent again after its Retry-After when it carries one, else after the client's own
// pause (clientResendPause), until registrationSpare before the test binary's deadline, which is
// the one bound: a listener that never takes the registration fails the row then, naming the last
// refusal (its status, reason and body) and how many registrations were sent. The client itself
// ignores Retry-After: envoy-client pauses 250 ms whatever the answer says, and the plugins' next
// registration comes 20 to 40 s later. The helper still waits a Retry-After out because it sends
// again far more often than the client does, and the listener sets one (1 s, on the bus-unavailable
// 503) where a sooner try would meet the same late server: the wait stays well under the plugins'
// 20 to 40 s and spares that server a registration every 250 ms. Any other status fails the row at
// once, since a 4xx is the listener's answer to this registration and the client does not send it
// again. A row that asserts that a registration is refused must not register through this: it would
// take the refusal it asserts for a slow listener.
func registerAsTheClientDoes(t *testing.T, port int, body string) {
	t.Helper()
	var request struct {
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal([]byte(body), &request); err != nil {
		t.Fatalf("read the session id of the registration %s: %v", body, err)
	}
	end := time.Now().Add(testwait.Remaining(t, registrationSpare))
	gaveUp := func(sent int, last string) {
		t.Helper()
		line := fmt.Sprintf("the listener took none of %d registrations of %s sent as the client sends them, each 5xx again after its Retry-After or %s, before %s ahead of the test binary's deadline; the last refusal: %s",
			sent, request.SessionID, clientResendPause, registrationSpare, last)
		_, _ = os.Stderr.WriteString(line + "\n")
		t.Fatal(line)
	}
	last := "none, no registration was answered"
	for sent := 1; ; sent++ {
		ctx, cancel := context.WithDeadline(t.Context(), end)
		post, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://127.0.0.1:"+strconv.Itoa(port)+"/v1/interests/subscribe", strings.NewReader(body))
		if err != nil {
			cancel()
			t.Fatalf("build the registration of %s: %v", request.SessionID, err)
		}
		post.Header.Set("Authorization", "Bearer "+listenerTestToken)
		post.Header.Set("Content-Type", "application/json")
		response, err := http.DefaultClient.Do(post)
		if err != nil {
			cancel()
			if ctx.Err() != nil {
				gaveUp(sent, fmt.Sprintf("%s (registration %d had no answer by then: %v)", last, sent, err))
			}
			t.Fatalf("registration %d of %s: %v", sent, request.SessionID, err)
		}
		answer, err := io.ReadAll(response.Body)
		_ = response.Body.Close()
		cancel()
		if err != nil {
			t.Fatalf("registration %d of %s: read the answer: %v", sent, request.SessionID, err)
		}
		if response.StatusCode == http.StatusOK {
			return
		}
		if response.StatusCode < http.StatusInternalServerError {
			t.Fatalf("registration %d of %s: status %d: %s", sent, request.SessionID, response.StatusCode, answer)
		}
		var refusal struct {
			Reason string `json:"reason"`
			Code   string `json:"code"`
		}
		_ = json.Unmarshal(answer, &refusal)
		reason := refusal.Reason
		if reason == "" {
			reason = refusal.Code
		}
		if reason == "" {
			reason = "none given"
		}
		pause, retryAfter := clientResendPause, response.Header.Get("Retry-After")
		if seconds, err := strconv.Atoi(retryAfter); err == nil && seconds > 0 {
			pause = time.Duration(seconds) * time.Second
		}
		last = fmt.Sprintf("registration %d answered %d, reason %q, Retry-After %q: %s", sent, response.StatusCode, reason, retryAfter, bytes.TrimSpace(answer))
		if time.Now().Add(pause).After(end) {
			gaveUp(sent, last)
		}
		t.Logf("%s; sent again in %s, as the client sends a 5xx again", last, pause)
		time.Sleep(pause)
	}
}

// Two sessions behind one real listener, each holding its own grant. s2 holds a frame unwritten
// (received, no done) and s1 answers done for it twice: on s2's own reply subject, which s1's
// grant does not let it publish to, and on its own reply prefix carrying s2's frame token, which
// its grant does. Neither moves the frame: the server refuses the first and the relay refuses the
// second, logging it. s2 is still delivered the frame when it registers a new epoch, and its own
// done acknowledges it.
//
// On main the listener gave every frame a token from one counter shared by every session behind its
// reply inbox (_INBOX.<inbox>.1, .2, ...) and the session grant let a session publish to every
// _INBOX. subject, so s1's done on s2's token acknowledged s2's frame, which was then never
// delivered: measured on nats-server 2.15.1 with three servers, and by
// this row's first form at main, which failed with s2's consumer at 0 pending and its ack floor on
// the frame, and s2 receiving nothing after its new epoch.
func TestAnotherSessionsDoneMovesNoFrameAndTheSessionIsStillDeliveredIt(t *testing.T) {
	t.Parallel()
	s1, s2 := newSessionUser(t, "ses_one_"), newSessionUser(t, "ses_two_")
	uri, listenerURI, listener := startSessionGrantHub(t, "relay-reply-sessions", s1, s2)
	consumers := observeConsumers(t, listenerURI)
	publisher := connectPublisher(t, listenerURI)
	one := startSessionStandInOn(t, s1.connect(t, uri), s1.id, answerDone)
	two := startSessionStandInOn(t, s2.connect(t, uri), s2.id, func(relayedFrame) []string { return []string{relayReplyReceived} })
	registerAsTheClientDoes(t, listener.port, consumerRegistration(s1.id, "epoch-1"))
	registerAsTheClientDoes(t, listener.port, consumerRegistration(s2.id, "epoch-1"))

	publish(t, publisher, newEnvelope("agent", contracts.LegacyAgentSubject(s1.id), "for one"))
	ownFrame := one.collect(t, 1, 10*time.Second)[0]
	awaitAcked(t, consumers, s1.id, ownFrame.seq)
	held := newEnvelope("agent", contracts.LegacyAgentSubject(s2.id), "for two")
	publish(t, publisher, held)
	heldFrame := two.collect(t, 1, 10*time.Second)[0]
	requireFrames(t, []relayedFrame{heldFrame}, held)

	// Each frame's reply subject is the listener's inbox, the session the frame went to and a
	// random token: <inbox>.<session id>.<token>.
	inbox := ownFrame.listener()
	for _, frame := range []struct {
		session string
		reply   string
	}{{s1.id, ownFrame.msg.Reply}, {s2.id, heldFrame.msg.Reply}} {
		token, ok := strings.CutPrefix(frame.reply, inbox+"."+frame.session+".")
		if !ok || len(token) != 26 || strings.Contains(token, ".") {
			t.Fatalf("a frame of %s carries reply subject %q, want %s.%s.<26-character token>", frame.session, frame.reply, inbox, frame.session)
		}
	}
	token := heldFrame.msg.Reply[strings.LastIndex(heldFrame.msg.Reply, ".")+1:]
	forged := inbox + "." + s1.id + "." + token
	t.Logf("s1's own frame was answered on %s; s2's frame waits on %s; s1 forges %s", ownFrame.msg.Reply, heldFrame.msg.Reply, forged)

	// First forgery: s1's done on s2's own reply subject. The server refuses it under s1's grant;
	// under a grant admitting every _INBOX. subject it reaches the relay as s2's own done and the
	// frame is acknowledged away, which this wait names as soon as it sees it.
	one.reply(heldFrame, relayReplyDone)
	deadline := time.Now().Add(listenerWaitBound(t))
	for !slices.Contains(s1.refusals(), heldFrame.msg.Reply) {
		info, err := consumers.Info(t.Context(), s2.id)
		if err != nil {
			t.Fatalf("read the consumer of %s: %v", s2.id, err)
		}
		if info.NumAckPending == 0 || info.AckFloor.Stream >= heldFrame.seq {
			t.Fatalf("s1's done on s2's reply subject %s acknowledged s2's frame (seq %d) away: the consumer of s2 has %d pending ack, ack floor %d",
				heldFrame.msg.Reply, heldFrame.seq, info.NumAckPending, info.AckFloor.Stream)
		}
		if time.Now().After(deadline) {
			t.Fatalf("the server never refused s1's done on s2's reply subject %s; it refused %v", heldFrame.msg.Reply, s1.refusals())
		}
		time.Sleep(20 * time.Millisecond)
	}
	// Second forgery: s1's done on its own reply prefix, which its grant admits, carrying s2's
	// frame token. The relay refuses it and says so; a relay that took it would acknowledge s2's
	// frame away, which this wait names as soon as it sees it.
	one.reply(relayedFrame{msg: &natsgo.Msg{Reply: forged}}, relayReplyDone)
	refusedLine := []string{`"msg":"` + relayRefusedReplyLine + `"`, `"session_id":"` + s2.id + `"`, `"reply_session_id":"` + s1.id + `"`}
	deadline = time.Now().Add(listenerWaitBound(t))
	for !slices.ContainsFunc(strings.Split(listener.Output.String(), "\n"), func(line string) bool {
		return !slices.ContainsFunc(refusedLine, func(field string) bool { return !strings.Contains(line, field) })
	}) {
		info, err := consumers.Info(t.Context(), s2.id)
		if err != nil {
			t.Fatalf("read the consumer of %s: %v", s2.id, err)
		}
		if info.NumAckPending == 0 || info.AckFloor.Stream >= heldFrame.seq {
			t.Fatalf("s1's done on %s, its own prefix carrying s2's frame token, acknowledged s2's frame (seq %d) away: the consumer of s2 has %d pending ack, ack floor %d",
				forged, heldFrame.seq, info.NumAckPending, info.AckFloor.Stream)
		}
		if time.Now().After(deadline) {
			t.Fatalf("the listener never logged that it refused s1's done on %s", forged)
		}
		time.Sleep(20 * time.Millisecond)
	}
	requireStillPending(t, consumers, s2.id, heldFrame.seq)

	// s2 restarts and registers a new epoch: the relay restarts at the ack floor, which deletes the
	// consumer holding the frame s2 never wrote and creates it again, so the row first waits for the
	// consumer's stored state (awaitSessionConsumerStored); it sends s2 the frame again, and s2's own
	// done, the control, acknowledges it.
	awaitSessionConsumerStored(t, publisher.Conn, s2.id)
	registerAsTheClientDoes(t, listener.port, consumerRegistration(s2.id, "epoch-2"))
	again := two.collect(t, 1, 10*time.Second)[0]
	requireFrames(t, []relayedFrame{again}, held)
	two.reply(again, relayReplyDone)
	awaitAcked(t, consumers, s2.id, heldFrame.seq)

	if got := s1.refusals(); !slices.Equal(got, []string{heldFrame.msg.Reply}) {
		t.Fatalf("the server refused s1 publishes to %v, want only s2's reply subject %s", got, heldFrame.msg.Reply)
	}
	if got := s2.refusals(); len(got) != 0 {
		t.Fatalf("the server refused s2 publishes to %v, want none: its replies are under its own prefix", got)
	}
}

// A session holding only its own grant answers every request the listener makes of it: each relay
// frame's received and done, which acknowledge the frame, and the send route's connection probe,
// whose reply subject names the session too. The server refuses none of its publishes. The row
// above is the control that a refusal would be seen: there the same recording catches the one
// publish s1's grant refuses.
//
// On main, under this grant, the server refused all three replies - the frame's received and done
// on _INBOX.<inbox>.1 and the probe's done on a plain _INBOX.<id> - so the frame was never
// acknowledged and the send answered connected only after the probe's one-second silence.
//
// The session registers the three delivery modes, as a plugin whose conversation takes messages
// does: a probed session that advertises none answers a plain send no_receiver (sendHandler), and
// this row reads connected.
func TestASessionOnItsOwnGrantAnswersItsFramesAndTheConnectionProbe(t *testing.T) {
	t.Parallel()
	s1 := newSessionUser(t, "ses_own_grant_")
	uri, listenerURI, listener := startSessionGrantHub(t, "relay-own-grant", s1)
	consumers := observeConsumers(t, listenerURI)
	publisher := connectPublisher(t, listenerURI)
	one := startSessionStandInOn(t, s1.connect(t, uri), s1.id, answerDone)
	registerAsTheClientDoes(t, listener.port, fmt.Sprintf(
		`{"session_id":%q,"topics":[],"self_subscribed":true,"delivery":{"consumer":true,"epoch":"epoch-1"},"capabilities":["aside","btw","steer"]}`, s1.id))

	item := newEnvelope("agent", contracts.LegacyAgentSubject(s1.id), "for the session")
	publish(t, publisher, item)
	frame := one.collect(t, 1, 10*time.Second)[0]
	requireFrames(t, []relayedFrame{frame}, item)
	awaitAcked(t, consumers, s1.id, frame.seq)

	started := time.Now()
	status, body := callListener(t, listener.port, http.MethodPost, "/v1/messages/send",
		fmt.Sprintf(`{"target_session":%q,"source":"agent","source_session":"ses_sender","message":"are you there?"}`, s1.id))
	elapsed := time.Since(started)
	if status != http.StatusOK || !strings.Contains(body, `"state":"`+sessionStateConnected+`"`) {
		t.Fatalf("the send answered %d %s, want 200 and state connected", status, body)
	}
	if probed := one.probed.Load(); probed != 1 {
		t.Fatalf("the session was probed %d times, want once", probed)
	}
	reply := *one.probeReply.Load()
	if !bus.SubjectContains(bus.SessionReplies(s1.id), reply) {
		t.Fatalf("the probe asked on reply subject %q, which the session's grant entry %q does not admit", reply, bus.SessionReplies(s1.id))
	}
	t.Logf("the send answered connected after %s; the probe asked on %s", elapsed, reply)
	// A refused publish comes back on the connection's next round trip.
	if err := one.conn.Flush(); err != nil {
		t.Fatalf("flush the session's connection: %v", err)
	}
	time.Sleep(200 * time.Millisecond)
	if got := s1.refusals(); len(got) != 0 {
		t.Fatalf("the server refused the session publishes to %v, want none", got)
	}
}

// countingAcker is the acker of a frame whose answers are counted rather than sent.
type countingAcker struct {
	acks, inProgress, naks atomic.Int32
}

func (a *countingAcker) Ack() error                       { a.acks.Add(1); return nil }
func (a *countingAcker) InProgress() error                { a.inProgress.Add(1); return nil }
func (a *countingAcker) NakWithDelay(time.Duration) error { a.naks.Add(1); return nil }

// A reply whose subject names another session than the frame's moves nothing, whatever its word:
// no answer to the server, the frame still awaited and not marked answered, and the run's pause
// backoff untouched. The first is logged and the next held back, until relayRefusedReplyLogEvery
// has passed, when the next line says how many it held. The control is the frame's own session's
// done, on the same token, which acks it and ends it.
func TestAReplyNamingAnotherSessionMovesNoFrame(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	relays := &sessionRelays{replyPrefix: "relay.reply.", frames: map[string]*relayFrame{}, logger: logging.NewWithWriter("test", &logs)}
	run := &relayRun{sessionID: "ses_two", outstanding: map[string]*relayFrame{}}
	acker := &countingAcker{}
	const token = "TOKENOFTHEFRAMEOFSESTWO234"
	frame := &relayFrame{run: run, acker: acker, sentAt: time.Now(), delivered: 1}
	relays.frames[token] = frame
	run.outstanding[token] = frame
	forge := func(word string) {
		relays.onReply(&natsgo.Msg{Subject: relays.replyPrefix + "ses_one." + token, Data: []byte(word)})
	}

	words := []string{relayReplyReceived, relayReplyProgress, relayReplyDone, relayReplyNak}
	for _, word := range words {
		forge(word)
	}
	if acker.acks.Load()+acker.inProgress.Load()+acker.naks.Load() != 0 {
		t.Fatalf("another session's replies answered the server: %d acks, %d in progress, %d naks",
			acker.acks.Load(), acker.inProgress.Load(), acker.naks.Load())
	}
	if relays.frames[token] != frame || run.outstanding[token] != frame || frame.answered || run.answered.Load() {
		t.Fatal("another session's replies changed the frame: it is no longer awaited, or it is marked answered")
	}
	refusals := func() []string {
		var lines []string
		for line := range strings.SplitSeq(strings.TrimSpace(logs.String()), "\n") {
			if strings.Contains(line, relayRefusedReplyLine) {
				lines = append(lines, line)
			}
		}
		return lines
	}
	first := refusals()
	if len(first) != 1 {
		t.Fatalf("the relay logged %d refusals for %d replies within a minute, want the first alone:\n%s", len(first), len(words), logs.String())
	}
	for _, field := range []string{`"session_id":"ses_two"`, `"reply_session_id":"ses_one"`, `"reply":"received"`, `"held":0`} {
		if !strings.Contains(first[0], field) {
			t.Fatalf("the first refusal line carries no %s: %s", field, first[0])
		}
	}

	// A minute on, the next refusal is logged and counts the three held back.
	relays.framesMu.Lock()
	relays.refusedLoggedAt = relays.refusedLoggedAt.Add(-relayRefusedReplyLogEvery)
	relays.framesMu.Unlock()
	forge(relayReplyDone)
	lines := refusals()
	if len(lines) != 2 {
		t.Fatalf("the relay logged %d refusals once a minute had passed, want 2:\n%s", len(lines), logs.String())
	}
	for _, field := range []string{`"reply":"done"`, `"held":3`} {
		if !strings.Contains(lines[1], field) {
			t.Fatalf("the second refusal line carries no %s: %s", field, lines[1])
		}
	}
	if acker.acks.Load()+acker.inProgress.Load()+acker.naks.Load() != 0 || relays.frames[token] != frame {
		t.Fatal("the logged refusal moved the frame")
	}

	relays.onReply(&natsgo.Msg{Subject: relays.replyPrefix + "ses_two." + token, Data: []byte(relayReplyDone)})
	if acker.acks.Load() != 1 || relays.frames[token] != nil || run.outstanding[token] != nil || !run.answered.Load() {
		t.Fatalf("the frame's own session's done: %d acks, frame still awaited %t, run answered %t; want one ack and the frame ended",
			acker.acks.Load(), relays.frames[token] != nil, run.answered.Load())
	}
}

// The session a reply names is the first token after the reply prefix, since a session id holds
// no dot (bus.SessionConsumerName), and the rest is the token. So ses_two's own reply subject with
// a token past its id, ses_two.x.<ses_two's frame token>, names ses_two and the token
// x.<ses_two's frame token>, which no frame holds: it moves nothing and logs nothing, whatever its
// word. The refusal line is still free for a reply that does name another session than its
// frame's, which is the control: ses_three's done on ses_two's token, logged with ses_three as the
// session the reply names.
//
// Read from the last dot, the same subject named session "ses_two.x" against ses_two's own frame
// and took the refusal line, which writes one line a minute: any session could hold that line
// with a token of its own, and a forgery in the same minute showed only as a count.
func TestAReplyWithATokenPastItsSessionIDMovesNoFrameAndLogsNothing(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	relays := &sessionRelays{replyPrefix: "relay.reply.", frames: map[string]*relayFrame{}, logger: logging.NewWithWriter("test", &logs)}
	run := &relayRun{sessionID: "ses_two", outstanding: map[string]*relayFrame{}}
	acker := &countingAcker{}
	const token = "TOKENOFTHEFRAMEOFSESTWO234"
	frame := &relayFrame{run: run, acker: acker, sentAt: time.Now(), delivered: 1}
	relays.frames[token] = frame
	run.outstanding[token] = frame

	for _, word := range []string{relayReplyReceived, relayReplyProgress, relayReplyDone, relayReplyNak} {
		relays.onReply(&natsgo.Msg{Subject: relays.replyPrefix + "ses_two.x." + token, Data: []byte(word)})
	}
	if acker.acks.Load()+acker.inProgress.Load()+acker.naks.Load() != 0 {
		t.Fatalf("replies on ses_two.x.<token> answered the server: %d acks, %d in progress, %d naks",
			acker.acks.Load(), acker.inProgress.Load(), acker.naks.Load())
	}
	if relays.frames[token] != frame || run.outstanding[token] != frame || frame.answered || run.answered.Load() {
		t.Fatal("replies on ses_two.x.<token> changed the frame: it is no longer awaited, or it is marked answered")
	}
	if strings.Contains(logs.String(), relayRefusedReplyLine) {
		t.Fatalf("replies on ses_two.x.<token> took the refusal line:\n%s", logs.String())
	}

	relays.onReply(&natsgo.Msg{Subject: relays.replyPrefix + "ses_three." + token, Data: []byte(relayReplyDone)})
	var refusals []string
	for line := range strings.SplitSeq(strings.TrimSpace(logs.String()), "\n") {
		if strings.Contains(line, relayRefusedReplyLine) {
			refusals = append(refusals, line)
		}
	}
	if len(refusals) != 1 {
		t.Fatalf("the relay logged %d refusals for ses_three's done on ses_two's token, want 1:\n%s", len(refusals), logs.String())
	}
	for _, field := range []string{`"session_id":"ses_two"`, `"reply_session_id":"ses_three"`, `"reply":"done"`, `"held":0`} {
		if !strings.Contains(refusals[0], field) {
			t.Fatalf("the refusal line carries no %s: %s", field, refusals[0])
		}
	}
	if acker.acks.Load()+acker.inProgress.Load()+acker.naks.Load() != 0 || relays.frames[token] != frame {
		t.Fatal("ses_three's done moved ses_two's frame")
	}
}

// onReply is the one callback that carries every session's receipts on an edge replica's relay
// connection, run one at a time (nats.go). The re-mark a nak'd gap-marked frame owes is a KV read
// and write (reMarkWithheldGap), so onReply runs it in a goroutine: a slow re-mark of one
// session's frame holds up no other session's receipt.
func TestANakGapReMarkRunsOffTheReceiptCallback(t *testing.T) {
	t.Parallel()
	frameFor := func(relays *sessionRelays, sessionID, token, gapMark string) *countingAcker {
		run := &relayRun{sessionID: sessionID, outstanding: map[string]*relayFrame{}}
		acker := &countingAcker{}
		frame := &relayFrame{run: run, acker: acker, sentAt: time.Now(), delivered: 1, gapMark: gapMark}
		relays.framesMu.Lock()
		relays.frames[token] = frame
		run.outstanding[token] = frame
		relays.framesMu.Unlock()
		return acker
	}
	nak := func(relays *sessionRelays, sessionID, token string) {
		relays.onReply(&natsgo.Msg{Subject: relays.replyPrefix + sessionID + "." + token, Data: []byte(relayReplyNak)})
	}

	t.Run("a slow re-mark does not block another session's receipt", func(t *testing.T) {
		relays := &sessionRelays{replyPrefix: "relay.reply.", frames: map[string]*relayFrame{}, logger: logging.New("test")}
		started, release := make(chan struct{}), make(chan struct{})
		relays.reMark = func(string) {
			close(started)
			<-release
		}
		defer close(release)
		frameFor(relays, "ses_slow", "TOKENOFTHESLOWSESSIONSFRAME1", "a-gap-mark")
		otherAcker := frameFor(relays, "ses_other", "TOKENOFTHEOTHERSESSIONFRAME2", "")

		// The slow session's gap-marked frame is nak'd on a goroutine of the test's own, so onReply
		// must hand the re-mark off and return for this call to return at once while the re-mark
		// blocks. Run synchronously (the revert), onReply blocks here, and the row fails with the
		// message below rather than hanging the package until the go test timeout.
		naked := make(chan struct{})
		go func() {
			nak(relays, "ses_slow", "TOKENOFTHESLOWSESSIONSFRAME1")
			close(naked)
		}()
		select {
		case <-naked:
		case <-time.After(listenerWaitBound(t)):
			t.Fatal("onReply did not return in time of a nak carrying a gap mark; it waited on the re-mark instead of running it off the callback")
		}
		select {
		case <-started:
		case <-time.After(listenerWaitBound(t)):
			t.Fatal("onReply never started the re-mark")
		}
		// The re-mark is still blocked. The other session's done is acked at once.
		done := make(chan struct{})
		go func() {
			relays.onReply(&natsgo.Msg{Subject: relays.replyPrefix + "ses_other.TOKENOFTHEOTHERSESSIONFRAME2", Data: []byte(relayReplyDone)})
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(listenerWaitBound(t)):
			t.Fatal("the other session's receipt was blocked behind the slow re-mark")
		}
		if otherAcker.acks.Load() != 1 {
			t.Fatalf("the other session's done acked %d times while the re-mark blocked, want 1", otherAcker.acks.Load())
		}
	})

	t.Run("a re-mark runs only for a nak carrying a gap mark", func(t *testing.T) {
		relays := &sessionRelays{replyPrefix: "relay.reply.", frames: map[string]*relayFrame{}, logger: logging.New("test")}
		var reMarked atomic.Int32
		relays.reMark = func(string) { reMarked.Add(1) }
		// Control: a nak of a frame with no gap mark runs no re-mark.
		frameFor(relays, "ses_plain", "TOKENOFAPLAINSESSIONFRAME001", "")
		nak(relays, "ses_plain", "TOKENOFAPLAINSESSIONFRAME001")
		time.Sleep(50 * time.Millisecond)
		if got := reMarked.Load(); got != 0 {
			t.Fatalf("a nak with no gap mark ran the re-mark %d times, want 0", got)
		}
		// A nak of a gap-marked frame runs the re-mark once.
		frameFor(relays, "ses_marked", "TOKENOFAMARKEDSESSIONFRAME01", "a-gap-mark")
		nak(relays, "ses_marked", "TOKENOFAMARKEDSESSIONFRAME01")
		deadline := time.Now().Add(listenerWaitBound(t))
		for reMarked.Load() == 0 && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		if got := reMarked.Load(); got != 1 {
			t.Fatalf("a nak with a gap mark ran the re-mark %d times, want 1", got)
		}
	})
}

// A frame of a session this replica admitted no registration of is held unacknowledged, for the
// consumer to deliver again once the session registers here, never acknowledged away: edgeWithholds
// answers frameHold for it (the relay loop's `case frameHold: continue`), not frameDrop (which
// acknowledges and counts). This row fails if a not-admitted frame is acknowledged instead of held.
func TestTheRelayHoldsANotAdmittedFrameRatherThanAcknowledgingIt(t *testing.T) {
	t.Parallel()
	m := &sessionRelays{repositories: &edgeRepositories{}, delivery: &sessionDelivery{}}
	msg := bus.SessionMessage{Subject: contracts.LegacyAgentSubject("ses_unadmitted"), Data: []byte(`{"topic":"notifications.agent.ses_unadmitted"}`)}
	if verdict := m.edgeWithholds("ses_unadmitted", msg); verdict.action != frameHold {
		t.Fatalf("edgeWithholds for a session this replica admitted no registration of = action %d, reason %q; want it held (frameHold, unacknowledged), not acknowledged away",
			verdict.action, verdict.reason)
	}
}

// The relay loop holds a frame of a session this replica no longer admits rather than
// acknowledging it: edgeWithholds answers frameHold, and the loop's `case frameHold: continue`
// leaves the frame pending for the consumer to deliver it again once the session registers here.
// The session is admitted when relayNATS passes its own not-admitted guard, and the admission is
// forgotten during the restart's stored-row read, before the first frame is pulled, so the loop
// reaches the hold with no admission - the window a prune opens before the relay applies the same
// pass's lapse. This row fails if the loop acknowledges a held frame (acknowledgeWithheld in place
// of the continue): the consumer's ack floor would then pass the frame.
func TestTheRelayLoopHoldsAFrameWhoseAdmissionWasForgotten(t *testing.T) {
	t.Parallel()
	met := metrics.New()
	grant, err := newEdgeGrant("edge-a", []string{"PRIV"}, met)
	if err != nil {
		t.Fatalf("the edge's grant: %v", err)
	}
	rig := newRelayRigUnownedAs(t, relayPauseFirst, relayPauseMax, grant)
	rig.relays.repositories = &edgeRepositories{withheld: newEdgeWithheldCounter(met)}

	const sessionID = "ses_relay_loop_hold"
	agent := contracts.LegacyAgentSubject(sessionID)
	rig.subscribe(t, consumerRegistration(sessionID, "epoch-1"))
	rig.requireConsumer(t, sessionID)
	_, seq := rig.storeEvent(t, agent)

	// Admit the session so relayNATS enters its loop, then forget the admission during the
	// restart's stored-row read, before the first frame is pulled: the loop then judges the frame
	// with no admission, as it does in the window between a prune and the relay applying the lapse.
	rig.deps.consumers.admissions.admit(sessionID, []string{"PRIV"})
	var once sync.Once
	rig.deps.consumers.afterStoredRowRead = func() {
		once.Do(func() {
			rig.deps.consumers.admissions.prune(time.Now().Add(time.Hour), func(string) bool { return true })
		})
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		rig.relays.relayNATS(ctx, &relayRun{sessionID: sessionID, outstanding: map[string]*relayFrame{}})
	}()
	// Wait for the loop to pull the frame; it then holds it (or, under the mutation, acknowledges it).
	testwait.Eventually(t, "the relay to pull the held frame", func() bool {
		info, infoErr := rig.consumers.Info(context.Background(), sessionID)
		return infoErr == nil && info.Delivered.Consumer >= 1
	})
	cancel()
	<-done
	// The held frame is still pending, acknowledged by nobody; a loop that acknowledged it would
	// have advanced the consumer's ack floor past it.
	requireStillPending(t, rig.consumers, sessionID, seq)
}

// N2/N6: two re-marks of one session, the second asked while the first is in flight, never run at
// once, so noteRepositoryGap's read and write cannot interleave and leave the in-memory mark and
// the entry's mark diverged, which would send the session a second, redundant gap notice. The two
// naks are driven through onReply, which hands each nak'd gap-marked frame's re-mark to scheduleReMark;
// scheduleReMark holds one in flight per session and folds a re-mark asked meanwhile into a single
// further run after it. The second re-mark's folding is read from reMarkAgain under reMarkMu, not
// waited out on a clock. Control: with onReply's call site reverted to a plain `go m.reMark`,
// scheduleReMark is never reached, reMarking is never set, and the first check fails.
func TestTwoReMarksOfOneSessionDoNotRunAtOnce(t *testing.T) {
	t.Parallel()
	relays := &sessionRelays{replyPrefix: "relay.reply.", frames: map[string]*relayFrame{}, logger: logging.New("test")}
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	relays.reMark = func(string) {
		entered <- struct{}{}
		<-release
	}
	const sessionID = "ses_remark_serial"
	frameFor := func(token string) {
		run := &relayRun{sessionID: sessionID, outstanding: map[string]*relayFrame{}}
		frame := &relayFrame{run: run, acker: &countingAcker{}, sentAt: time.Now(), delivered: 1, gapMark: "a-gap-mark"}
		relays.framesMu.Lock()
		relays.frames[token] = frame
		run.outstanding[token] = frame
		relays.framesMu.Unlock()
	}
	nak := func(token string) {
		relays.onReply(&natsgo.Msg{Subject: relays.replyPrefix + sessionID + "." + token, Data: []byte(relayReplyNak)})
	}
	frameFor("TOKENOFTHEFIRSTREMARKFRAME01")
	frameFor("TOKENOFTHESECONDREMARKFRAME1")
	// The first nak: onReply hands the re-mark to scheduleReMark, whose run blocks on release here.
	// onReply itself returns at once, so this call does not block.
	nak("TOKENOFTHEFIRSTREMARKFRAME01")
	select {
	case <-entered:
	case <-time.After(listenerWaitBound(t)):
		t.Fatal("onReply never started the first re-mark")
	}
	// The second nak of the same session while the first re-mark is in flight: scheduleReMark folds
	// it into reMarkAgain rather than running it at once.
	nak("TOKENOFTHESECONDREMARKFRAME1")
	relays.reMarkMu.Lock()
	running, folded := relays.reMarking[sessionID], relays.reMarkAgain[sessionID]
	relays.reMarkMu.Unlock()
	if !running {
		close(release)
		t.Fatal("the first re-mark is not tracked in flight: onReply did not run it through scheduleReMark")
	}
	if !folded {
		close(release)
		t.Fatal("the second re-mark was not folded into reMarkAgain: two re-marks of one session could run at once")
	}
	// The folded re-mark runs once the first finishes, so the session's owed mark is not dropped.
	close(release)
	select {
	case <-entered:
	case <-time.After(listenerWaitBound(t)):
		t.Fatal("the folded re-mark never ran after the first finished; the session's owed mark is dropped")
	}
}

// N7: a withheld-gap re-mark that completes before the relay loop clears the frame's own mark as sent
// must not lose the gap notice. The re-mark writes a fresh id (reMarkWithheldGap), so the loop's
// repositoryGapSent of the frame's mark X cannot take the re-mark's: the entry keeps a mark, owed to
// the session's next relay run. Control (head): the re-mark reused X, the loop cleared X as sent, and
// the entry ended with no mark - the notice owed nowhere.
func TestANakReMarkBeforeTheLoopsClearKeepsTheGapNotice(t *testing.T) {
	t.Parallel()
	rig := newEdgeRig(t)
	m := &sessionRelays{sessions: rig.deps.sessions, logger: logging.New("test")}
	m.reMark = m.reMarkWithheldGap
	ctx := context.Background()
	const sessionID = "ses_remark_before_clear"
	if err := rig.deps.sessions.Put(sessionID, session.SessionEntry{MachineID: "test-machine", Projects: []string{"PRIV"}}); err != nil {
		t.Fatalf("register %s: %v", sessionID, err)
	}
	// The relay loop noted mark X for a withheld event and published frame F carrying X; F was nak'd,
	// so a re-mark is owed. The nak's re-mark completes before the loop's repositoryGapSent.
	mark, err := m.noteRepositoryGap(ctx, sessionID)
	if err != nil {
		t.Fatalf("the loop's mark: %v", err)
	}
	m.reMarkWithheldGap(sessionID)
	m.repositoryGapSent(ctx, sessionID, mark)
	if owed, stored := m.owedRepositoryGap(sessionID), storedEdgeSessionEntry(t, rig, sessionID).RepositoryGap; owed == "" && stored == "" {
		t.Errorf("a re-mark before the loop's clear lost the gap notice: owed=%q stored=%q", owed, stored)
	}
}

// openOwningStreamRegistry connects an owning-stream client and opens a session registry on it,
// both closed when the test ends. name labels the pair in any failure.
func openOwningStreamRegistry(t *testing.T, name string) (*bus.Client, *session.SessionRegistry) {
	t.Helper()
	client, err := bus.ConnectOwningStream([]string{testnats.URL(t)}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("connect %s: %v", name, err)
	}
	t.Cleanup(client.Close)
	reg, err := session.OpenSessionRegistry(t.Context(), client.Conn, session.WithSessionReplicas(1))
	if err != nil {
		t.Fatalf("open %s's registry: %v", name, err)
	}
	return client, reg
}

// N10: a nak's re-mark whose write fails notes a fresh id in memory, so this listener's next relay
// run still sends it (owedRepositoryGap). The loop has cleared the frame's own mark as sent, and
// noteRepositoryGap notes nothing when its write fails, so without the in-memory note the notice is
// owed nowhere. The write is made to fail by closing the relay's registry connection. Control: with
// the in-memory note removed, the failed re-mark owes nothing.
func TestANakReMarkWhoseWriteFailsStillOwesAFreshMark(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	run := func(sessionID string, failWrite bool) string {
		client, reg := openOwningStreamRegistry(t, sessionID)
		m := &sessionRelays{sessions: reg, logger: logging.New("test")}
		if err := reg.Put(sessionID, session.SessionEntry{MachineID: "test-machine", Projects: []string{"PRIV"}}); err != nil {
			t.Fatalf("register %s: %v", sessionID, err)
		}
		mark, err := m.noteRepositoryGap(ctx, sessionID)
		if err != nil {
			t.Fatalf("the loop's mark for %s: %v", sessionID, err)
		}
		// The loop publishes frame F carrying the mark, then clears it as sent.
		m.repositoryGapSent(ctx, sessionID, mark)
		if failWrite {
			client.Close() // the re-mark's KV write now fails
		}
		m.reMarkWithheldGap(sessionID) // F is nak'd: the re-mark
		return m.owedRepositoryGap(sessionID)
	}
	if owed := run("ses_remark_ok", false); owed == "" {
		t.Fatal("CONTROL: a working re-mark owed no mark; the row cannot tell")
	}
	if owed := run("ses_remark_fail", true); owed == "" {
		t.Error("a re-mark whose write failed owes no mark, though its next run should send a fresh one it noted")
	}
}

// The repository-gap-sent residue (the failing-write cell of the re-mark matrix, beside
// TestANakReMarkBeforeTheLoopsClearKeepsTheGapNotice and TestANakReMarkWhoseWriteFailsStillOwesAFreshMark):
// frame F carried mark X; F is nak'd and its re-mark runs before the relay loop's clear of X (the
// N7 ordering), and the re-mark's write fails, so it notes a fresh mark Y in this process alone
// (reMarkWithheldGap). The loop's clear then runs with the bus answering again: repositoryGapSent
// forgets only the mark the frame carried, so Y stays owed in memory and the next frame this run
// sends carries it, while X is cleared from the entry. The failing write is the re-mark's registry
// on a closed connection; the clear runs on the relay's working registry. Control: the same steps
// with the re-mark's write working keep a mark owed on the entry too.
func TestARepositoryGapSentForgetsOnlyTheMarkTheFrameCarried(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	_, reader := openOwningStreamRegistry(t, "reader")
	run := func(sessionID string, failWrite bool) (owed, stored, mark string) {
		_, reg := openOwningStreamRegistry(t, sessionID)
		failingClient, failing := openOwningStreamRegistry(t, sessionID+"-failing")
		m := &sessionRelays{sessions: reg, logger: logging.New("test")}
		if err := reg.Put(sessionID, session.SessionEntry{MachineID: "test-machine", Projects: []string{"PRIV"}}); err != nil {
			t.Fatalf("register %s: %v", sessionID, err)
		}
		mark, err := m.noteRepositoryGap(ctx, sessionID)
		if err != nil {
			t.Fatalf("the loop's mark: %v", err)
		}
		testwait.Eventually(t, "the relay's cache to show the loop's mark", func() bool {
			entry, _ := reg.Lookup(sessionID)
			return entry.RepositoryGap == mark
		})
		if failWrite {
			failingClient.Close()
			m.sessions = failing
		}
		m.reMarkWithheldGap(sessionID)
		m.sessions = reg
		m.repositoryGapSent(ctx, sessionID, mark)
		got, _, err := reader.StoredByWriter(ctx, sessionID)
		if err != nil {
			t.Fatalf("read %s: %v", sessionID, err)
		}
		testwait.Eventually(t, "the relay's cache to show the stored mark", func() bool {
			entry, _ := reg.Lookup(sessionID)
			return entry.RepositoryGap == got.RepositoryGap
		})
		return m.owedRepositoryGap(sessionID), got.RepositoryGap, mark
	}
	// Control, the write working: the re-mark's fresh mark reaches the entry, so a mark is owed both
	// in memory and on the entry.
	if owed, stored, _ := run("ses_gap_sent_ok", false); owed == "" || stored == "" {
		t.Fatalf("CONTROL: a working re-mark before the loop's clear kept no mark (owed=%q stored=%q)", owed, stored)
	}
	// The failing write: X is cleared from the entry (stored empty), and the fresh mark Y, distinct
	// from X, is owed in memory alone for the next frame this run sends.
	owed, stored, mark := run("ses_gap_sent_fail", true)
	t.Logf("failed re-mark before the loop's clear: owed=%q stored=%q mark=%q", owed, stored, mark)
	if owed == "" || owed == mark || stored != "" {
		t.Errorf("a failed re-mark did not leave a fresh mark owed in memory alone: owed=%q mark=%q stored=%q (want owed non-empty and != mark, stored empty)", owed, mark, stored)
	}
}
