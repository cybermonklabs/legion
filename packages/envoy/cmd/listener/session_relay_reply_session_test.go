package main

import (
	"bytes"
	"fmt"
	"net/http"
	"slices"
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
	"github.com/sjawhar/envoy/internal/testnats"
)

// relayRefusedReplyLine is the line the relay writes for a reply whose subject names another
// session than the one its frame was sent to (sessionRelays.onReply).
const relayRefusedReplyLine = "session relay refused a reply naming another session than the one its frame was sent to; the frame is unchanged"

// sessionUser is a NATS user holding one session's own grant, the one the auth service admits a
// session with once natsgrant.Session publishes bus.SessionReplies: publish its replies
// (bus.SessionReplies) and its agent stream frames, subscribe its relay subject and its agent
// stream control subject. A publish the server refuses reaches the client only as an asynchronous
// error, so each refusal on its connection is recorded by subject.
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

// entry is the user's line of the server's authorization.
func (s *sessionUser) entry() string {
	return fmt.Sprintf("{ nkey: %q, permissions: { publish: { allow: [%q, %q] }, subscribe: { allow: [%q, %q] } } }",
		s.public, bus.SessionReplies(s.id), contracts.AgentStreamFramesSubject(s.id),
		bus.DeliverSubject(s.id), contracts.AgentStreamControlSubject(s.id))
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
	s1, s2 := newSessionUser(t, "ses_one_"), newSessionUser(t, "ses_two_")
	uri, listenerURI, listener := startSessionGrantHub(t, "relay-reply-sessions", s1, s2)
	consumers := observeConsumers(t, listenerURI)
	publisher := connectPublisher(t, listenerURI)
	one := startSessionStandInOn(t, s1.connect(t, uri), s1.id, answerDone)
	two := startSessionStandInOn(t, s2.connect(t, uri), s2.id, func(relayedFrame) []string { return []string{relayReplyReceived} })
	postListener(t, listener.port, "/v1/interests/subscribe", consumerRegistration(s1.id, "epoch-1"))
	postListener(t, listener.port, "/v1/interests/subscribe", consumerRegistration(s2.id, "epoch-1"))

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
	deadline := time.Now().Add(5 * time.Second)
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
	deadline = time.Now().Add(10 * time.Second)
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

	// s2 restarts and registers a new epoch: the relay restarts at the ack floor and sends it the
	// frame it never wrote, and s2's own done, the control, acknowledges it.
	postListener(t, listener.port, "/v1/interests/subscribe", consumerRegistration(s2.id, "epoch-2"))
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
func TestASessionOnItsOwnGrantAnswersItsFramesAndTheConnectionProbe(t *testing.T) {
	s1 := newSessionUser(t, "ses_own_grant_")
	uri, listenerURI, listener := startSessionGrantHub(t, "relay-own-grant", s1)
	consumers := observeConsumers(t, listenerURI)
	publisher := connectPublisher(t, listenerURI)
	one := startSessionStandInOn(t, s1.connect(t, uri), s1.id, answerDone)
	postListener(t, listener.port, "/v1/interests/subscribe", consumerRegistration(s1.id, "epoch-1"))

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
