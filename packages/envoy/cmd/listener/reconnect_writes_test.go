package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/id"
	"github.com/sjawhar/envoy/internal/testnats"
)

// reconnectBufferRefusal is the text of nats.go's refusal of a send while it reconnects with the
// reconnect buffer off (nats.ErrReconnectBufExceeded), without nats.go's "nats: " prefix. A
// standing session's role claim refresh answered it during the hub's NATS restart on 2026-11-16,
// as a 503 that named a buffer and no reconnect. TestAWriteThroughANATSReconnectWaitsOrFailsByName
// shows a send in an outage holds it.
var reconnectBufferRefusal = strings.TrimPrefix(natsgo.ErrReconnectBufExceeded.Error(), "nats: ")

// reconnectingReason is what a 503 says when the bus is reconnecting (bus.ErrReconnecting).
const reconnectingReason = "NATS is reconnecting"

// reconnectWritesAnswer is one route's answer to the test: its status and body, or the error of a
// request the test gave up on itself.
type reconnectWritesAnswer struct {
	status int
	body   string
	err    error
}

// String says the answer for a failure message.
func (a reconnectWritesAnswer) String() string {
	if a.err != nil {
		return "no answer: " + a.err.Error()
	}
	return fmt.Sprintf("%d %s", a.status, strings.TrimSpace(a.body))
}

// reconnectWritesCall posts body to path on the listener at port as the shared-token caller, and
// gives up on the answer after timeout. It reports a failure rather than failing the test, so it
// runs on a goroutine of its own.
func reconnectWritesCall(port int, path, body string, timeout time.Duration) reconnectWritesAnswer {
	return reconnectWritesCallWritten(port, path, body, timeout, nil)
}

// reconnectWritesCallWritten is reconnectWritesCall, which calls written, when it is not nil, once
// the whole request is written to the listener's socket.
func reconnectWritesCallWritten(port int, path, body string, timeout time.Duration, written func()) reconnectWritesAnswer {
	request, err := http.NewRequest(http.MethodPost, "http://127.0.0.1:"+strconv.Itoa(port)+path, strings.NewReader(body))
	if err != nil {
		return reconnectWritesAnswer{err: err}
	}
	request.Header.Set("Authorization", "Bearer "+listenerTestToken)
	request.Header.Set("Content-Type", "application/json")
	if written != nil {
		request = request.WithContext(httptrace.WithClientTrace(request.Context(), &httptrace.ClientTrace{
			WroteRequest: func(info httptrace.WroteRequestInfo) {
				if info.Err == nil {
					written()
				}
			},
		}))
	}
	response, err := (&http.Client{Timeout: timeout}).Do(request)
	if err != nil {
		return reconnectWritesAnswer{err: err}
	}
	defer response.Body.Close()
	answer, err := io.ReadAll(response.Body)
	return reconnectWritesAnswer{status: response.StatusCode, body: string(answer), err: err}
}

// Serial (listenerTestParallelism): it holds a wall-clock bound within a second or two of what it
// waits for: the listener's reconnect inside the route's 3.5 s budget, after the test gave up.
//
// A write the listener makes while its NATS connection is reconnecting waits for the connection
// within its caller's deadline and is then sent, or fails by name, at once or once that deadline
// ends: the answer is a 503 whose reason says the bus is reconnecting. No answer and no log line
// holds nats.go's "outbound buffer limit exceeded", which the client's reconnect buffer, kept off
// so that nothing is sent after its caller saw an error, makes nats.go answer to every send while
// it reconnects. And once the server is back, nothing of such a write reaches it: a role claim
// refresh and a publish answered 503 in an outage land nothing, the session record the claim
// refresh writes does not move, and a publish the caller gave up on lands nothing.
//
// The controls: before the outages, a role claim refresh of a new role is stored and moves the
// session record, so the second outage's checks would see a claim that landed; and in that
// outage, a send on a bus connection of the test's own fails with nats.go's refusal, whose text
// no answer and no line may hold.
//
// The server is stopped three times. The first outage is shorter than the routes' deadline, so
// each write may wait for the server and land. The second is longer, so each must fail by name and
// leave nothing behind. In the third, the test gives up on a publish, and only then starts the
// server again, before the route's deadline: a route that sent the publish after its caller gave
// up would land it then, so the stream holding no copy of it shows that the route stopped at the
// give-up. The row checks that the listener reconnected before that deadline, since a later
// reconnect would fail such a route's publish too.
//
// A role claim reads the claimant's session from the session bucket's stream leader (#5132), and
// that read fails at once, by name, while NATS reconnects, so the claim does not wait within its
// deadline. R14 of dispatch://INFRA-3589 allows both. So the write the third outage gives up on is
// a publish, which waits for the connection.
func TestAWriteThroughANATSReconnectWaitsOrFailsByName(t *testing.T) {
	const (
		machineID = "reconnect-writes"
		sessionID = "ses_reconnect_writes"
		// The routes' own deadline is 3.5 s; a caller that gives up sooner gives up at this.
		givenUpAfter = time.Second
		// The client waits this long for the routes' answers, past their own deadline.
		answerWithin = 15 * time.Second
	)
	server, uri := testnats.Start(t)
	listener := startListenerProcess(t, cmdtest.Build(t, "envoy-listener"), uri, machineID)
	listener.waitHealthy(t)
	postListener(t, listener.port, "/v1/interests/subscribe", consumerRegistration(sessionID, "epoch-1"))
	postListener(t, listener.port, "/v1/roles/set", `{"session_id":"`+sessionID+`","role":"reconnect-writes-before"}`)

	claim := func(role string) string {
		return `{"session_id":"` + sessionID + `","role":"` + role + `"}`
	}
	publishBody := func(topic string) string {
		return `{"topic":"` + topic + `","message":"a publish through a NATS reconnect"}`
	}
	// lines counts the main client's lines of kind (bus.Client's connection callbacks, in Go's text
	// format); the role lane's connection logs the same lines under name=envoy-role-lane.
	lines := func(kind string) int {
		return strings.Count(listener.Output.String(), "envoy nats "+kind+" name=envoy ")
	}
	// stop stops the server and returns once the listener's client has seen it go.
	stop := func(t *testing.T) {
		t.Helper()
		disconnects := lines("disconnected")
		server.Stop(t)
		waitFor(t, 15*time.Second, "the listener to see the NATS server go", func() bool { return lines("disconnected") > disconnects })
	}
	// observe reads the server the listener writes to, on a connection of the test's own.
	observe := func(t *testing.T) natsgo.JetStreamContext {
		t.Helper()
		conn := testnats.Connect(t, uri)
		t.Cleanup(conn.Close)
		js, err := conn.JetStream()
		if err != nil {
			t.Fatalf("open JetStream to read what the listener wrote: %v", err)
		}
		return js
	}
	bucket := func(t *testing.T, js natsgo.JetStreamContext, name string) natsgo.KeyValue {
		t.Helper()
		kv, err := js.KeyValue(name)
		if err != nil {
			t.Fatalf("open the %s bucket: %v", name, err)
		}
		return kv
	}
	// The claim refresh's control: a refresh of a new role before the outages is stored, and
	// moves the session record it writes.
	{
		const role = "reconnect-writes-control"
		js := observe(t)
		before, err := bucket(t, js, bus.SessionBucket).Get(sessionID)
		if err != nil {
			t.Fatalf("control: read the session record before the claim refresh: %v", err)
		}
		postListener(t, listener.port, "/v1/roles/set", claim(role))
		if _, err := bucket(t, js, bus.RoleClaimBucket).Get(role); err != nil {
			t.Fatalf("control: role %s's claim, refreshed before the outages, is not in the role bucket: %v", role, err)
		}
		if after, err := bucket(t, js, bus.SessionBucket).Get(sessionID); err != nil || after.Revision() == before.Revision() {
			t.Fatalf("control: the session record the claim refresh writes stayed at revision %d: %v, %v", before.Revision(), after, err)
		}
	}
	// answers is every answer a call below had, which the end of the test reads.
	var answers []reconnectWritesAnswer
	// call makes calls at once, each on a goroutine of its own, and returns their answers in order
	// once all have answered.
	call := func(calls ...func() reconnectWritesAnswer) []reconnectWritesAnswer {
		got := make([]reconnectWritesAnswer, len(calls))
		var wg sync.WaitGroup
		for i, c := range calls {
			wg.Add(1)
			go func() {
				defer wg.Done()
				got[i] = c()
			}()
		}
		wg.Wait()
		answers = append(answers, got...)
		return got
	}
	// waitedOrNamed fails the test unless answer is a success or a 503 naming the reconnect.
	waitedOrNamed := func(t *testing.T, what string, answer reconnectWritesAnswer) {
		t.Helper()
		if answer.err != nil || (answer.status != http.StatusOK && (answer.status != http.StatusServiceUnavailable || !strings.Contains(answer.body, reconnectingReason))) {
			t.Errorf("%s during the outage answered %s, want 200 once the server was back or 503 naming %q", what, answer, reconnectingReason)
		}
	}

	t.Run("a role claim refresh and a publish during an outage wait for the server or fail by name", func(t *testing.T) {
		const role, topic = "reconnect-writes-waited", "notifications.team.reconnect-writes-waited"
		stop(t)
		// The calls run on a goroutine, so the server is started again on the test's own, once
		// both requests are written to the listener's socket while the server is down.
		written := make(chan struct{}, 2)
		wrote := func() { written <- struct{}{} }
		answered := make(chan []reconnectWritesAnswer, 1)
		go func() {
			answered <- call(
				func() reconnectWritesAnswer {
					return reconnectWritesCallWritten(listener.port, "/v1/roles/set", claim(role), answerWithin, wrote)
				},
				func() reconnectWritesAnswer {
					return reconnectWritesCallWritten(listener.port, "/v1/messages/publish", publishBody(topic), answerWithin, wrote)
				},
			)
		}()
		for range 2 {
			select {
			case <-written:
			case got := <-answered:
				t.Fatalf("control: the calls answered %s and %s before both were written to the listener", got[0], got[1])
			case <-time.After(15 * time.Second):
				t.Fatal("control: the calls were not both written to the listener within 15 s")
			}
		}
		server.Start(t)
		got := <-answered
		t.Logf("during the outage the role claim refresh answered %s and the publish %s", got[0], got[1])
		waitedOrNamed(t, "the role claim refresh", got[0])
		waitedOrNamed(t, "the publish", got[1])
		js := observe(t)
		if got[0].status == http.StatusOK {
			if _, err := bucket(t, js, bus.RoleClaimBucket).Get(role); err != nil {
				t.Errorf("the role claim answered 200 is not in the role bucket: %v", err)
			}
		}
		if got[1].status == http.StatusOK {
			if _, err := js.GetLastMsg(bus.Stream, topic); err != nil {
				t.Errorf("the publish answered 200 is not in the stream: %v", err)
			}
		}
	})

	t.Run("a role claim refresh and a publish whose deadline ends in an outage fail by name and land nothing", func(t *testing.T) {
		const role, topic = "reconnect-writes-ended", "notifications.team.reconnect-writes-ended"
		listener.waitHealthy(t)
		js := observe(t)
		sessions := bucket(t, js, bus.SessionBucket)
		before, err := sessions.Get(sessionID)
		if err != nil {
			t.Fatalf("read the session record before the outage: %v", err)
		}
		reconnects := lines("reconnected")
		// The buffer refusal's control: a bus connection of the test's own, which keeps no
		// reconnect buffer as the listener's do, has its send in this outage refused with it.
		probe := connectPublisher(t, uri)
		stop(t)
		waitFor(t, 15*time.Second, "control: the test's own bus connection to start reconnecting", probe.Conn.IsReconnecting)
		if err := probe.Conn.Publish("reconnect-writes.control", []byte("{}")); err == nil || !strings.Contains(err.Error(), reconnectBufferRefusal) {
			t.Fatalf("control: a send on the test's own bus connection in the outage answered %v, want nats.go's %q", err, reconnectBufferRefusal)
		}
		got := call(
			func() reconnectWritesAnswer {
				return reconnectWritesCall(listener.port, "/v1/roles/set", claim(role), answerWithin)
			},
			func() reconnectWritesAnswer {
				return reconnectWritesCall(listener.port, "/v1/messages/publish", publishBody(topic), answerWithin)
			},
		)
		// The routes' deadline has ended by now: nothing of the two may be sent once the server
		// is back.
		t.Logf("in an outage past the routes' deadline the role claim refresh answered %s and the publish %s", got[0], got[1])
		for i, what := range []string{"the role claim refresh", "the publish"} {
			if got[i].status != http.StatusServiceUnavailable || !strings.Contains(got[i].body, reconnectingReason) {
				t.Errorf("%s whose deadline ended in the outage answered %s, want 503 naming %q", what, got[i], reconnectingReason)
			}
		}
		server.Start(t)
		waitFor(t, 30*time.Second, "the listener to reconnect", func() bool { return lines("reconnected") > reconnects })
		listener.waitHealthy(t)
		// A write held anywhere and sent after the reconnect would be on the server within this.
		time.Sleep(2 * time.Second)

		js = observe(t)
		if entry, err := bucket(t, js, bus.RoleClaimBucket).Get(role); !errors.Is(err, natsgo.ErrKeyNotFound) {
			t.Errorf("role %s's claim, answered 503 during the outage, reached the server after it came back: %v, %v", role, entry, err)
		}
		if after, err := bucket(t, js, bus.SessionBucket).Get(sessionID); err != nil || after.Revision() != before.Revision() {
			t.Errorf("the session record the role claim refresh writes moved from revision %d during the outage: %v, %v", before.Revision(), after, err)
		}
		if message, err := js.GetLastMsg(bus.Stream, topic); !errors.Is(err, natsgo.ErrMsgNotFound) {
			t.Errorf("the publish on %s, answered 503 during the outage, reached the stream after the server came back: %v, %v", topic, message, err)
		}
	})

	t.Run("a publish whose caller gives up in an outage lands nothing when the server is back before the route's deadline", func(t *testing.T) {
		const givenUp = "notifications.team.reconnect-writes-given-up"
		listener.waitHealthy(t)
		reconnects := lines("reconnected")
		stop(t)
		// call returns once the test has given up on the publish, so the give-up comes before the
		// server starts again below. The request reaches the route after called, so the route's
		// deadline (sendRequestBudget from the request's arrival) is at least that long after it.
		called := time.Now()
		got := call(func() reconnectWritesAnswer {
			return reconnectWritesCall(listener.port, "/v1/messages/publish", publishBody(givenUp), givenUpAfter)
		})[0]
		gaveUp := time.Since(called)
		// The publish waits for the connection within its caller's deadline, so the test gives up
		// on it first: it is the write the test gave up on while the route still held it.
		if got.err == nil {
			t.Fatalf("the publish the test gave up on after %s answered %s before the test gave up on it", givenUpAfter, got)
		}
		if gaveUp >= sendRequestBudget {
			t.Fatalf("UNKNOWN: the test gave up on the publish %s after the call, not before the route's deadline (sendRequestBudget, %s), so no start of the server can come between the two",
				gaveUp.Round(time.Millisecond), sendRequestBudget)
		}
		server.Start(t)
		waitFor(t, 30*time.Second, "the listener to reconnect", func() bool { return lines("reconnected") > reconnects })
		reconnected := time.Since(called)
		t.Logf("the test gave up on the publish %s after the call (%s), and saw the listener reconnect %s after it; the route's deadline is %s after the request arrived",
			gaveUp.Round(time.Millisecond), got, reconnected.Round(time.Millisecond), sendRequestBudget)
		if reconnected >= sendRequestBudget {
			t.Fatalf("UNKNOWN: the listener reconnected %s after the call, past the route's deadline (sendRequestBudget, %s): a route that went on with the publish after its caller gave up would fail it at that deadline too, so this run cannot tell the two apart",
				reconnected.Round(time.Millisecond), sendRequestBudget)
		}
		// The route's own line for the publish says that the route has answered, and the route
		// sends nothing after it answers. A route that went on with the publish answers only once
		// the stream acknowledged it or the route's deadline ended.
		var answered []map[string]any
		waitFor(t, 15*time.Second, "the publish route's line for the publish the test gave up on", func() bool {
			answered = linesIn(listener.Output.String(), "listener publish", map[string]any{"topic": givenUp})
			return len(answered) > 0
		})
		js := observe(t)
		message, err := js.GetLastMsg(bus.Stream, givenUp)
		if !errors.Is(err, natsgo.ErrMsgNotFound) {
			var stored string
			if message != nil {
				stored = string(message.Data)
			}
			t.Errorf("the publish on %s, given up on %s after the call, reached the stream once the listener reconnected %s after the call, before the route's deadline: %q (%v); the route recorded %v",
				givenUp, gaveUp.Round(time.Millisecond), reconnected.Round(time.Millisecond), stored, err, answered)
		}
	})

	// No answer and no log line holds nats.go's buffer refusal.
	for _, answer := range answers {
		if strings.Contains(answer.body, reconnectBufferRefusal) {
			t.Errorf("an answer names nats.go's buffer refusal rather than the reconnect: %s", answer)
		}
	}
	for line := range strings.SplitSeq(listener.Output.String(), "\n") {
		if strings.Contains(line, reconnectBufferRefusal) {
			t.Errorf("a log line names nats.go's buffer refusal rather than the reconnect: %s", line)
		}
	}
}

// A session relay run through two NATS outages writes no line that holds nats.go's "outbound
// buffer limit exceeded", and each send it makes in an outage that nats.go refuses names the
// reconnect. The ack nats.go refused is not sent once the bus is back: the relay that takes the
// session again restarts at the ack floor, which is still below the first message, so the session
// is delivered both messages again. That restart sends the second message whether or not its nak
// reached the server, so the row shows the nak only by its line, which names the reconnect.
//
// In the first outage the listener's connections are cut while the server runs on, as a network
// that drops them does, so the relay still holds its two frames. The session answers one of them
// done, so the relay acks that message, and the listener loses its machine's durable binding, so
// the relay hands the other back with a nak. The rows above cannot reach either send through the
// routes. nats.go refuses both, since the client keeps no reconnect buffer, and each line names
// the reconnect.
//
// The second outage stops the server and starts it again. A stopping server ends each pull
// request it holds with "409 Server Shutdown" (nats-server v2.15.1 signalPullConsumers), so a
// restart line the relay writes then names that; a restart line for a pull request nats.go
// refused names the reconnect.
func TestAWriteThroughANATSReconnectWaitsOrFailsByNameInASessionRelay(t *testing.T) {
	t.Parallel()
	const (
		answerFailed   = "session relay could not answer the consumer for a frame"
		pullEnded      = "session relay pull ended; restarting at the ack floor"
		serverShutdown = "409 Server Shutdown"
	)
	_, handOverFailed := relayHandOverLines(relayCancelBindingLost)
	server, uri := testnats.Start(t)
	// The listener reaches the server through a proxy the row can cut. The session's stand-in
	// reaches it directly, so it stays connected while the listener's connections are cut.
	proxy := testnats.StartProxy(t, uri)
	rig := newRelayRigUnownedOn(t, proxy.URL(), relayPauseFirst, relayPauseMax, nil)
	// The receipt wait is longer than the row, so the relay cannot pause, and so forget the frames
	// it holds, before the row takes the session away.
	rig.relays.receiptWait = time.Minute
	rig.relays.setOwned(true)
	sessionID := "ses_relay_reconnect_" + id.New()
	// A nil answer leaves the stand-in silent, so every frame stays outstanding with the relay.
	standIn := startSessionStandIn(t, uri, sessionID, nil)
	rig.subscribe(t, consumerRegistration(sessionID, "epoch-1"))
	agent := contracts.LegacyAgentSubject(sessionID)
	first, _ := rig.storeEvent(t, agent)
	second, _ := rig.storeEvent(t, agent)
	frames := standIn.collect(t, 2, 5*time.Second)
	requireFrames(t, frames, first, second)
	awaitAckState(t, rig.consumers, sessionID, 2, 0)

	// consumerCall makes one call on the listener's own session-consumer connection, the one the
	// relay answers and hands back on: it fails as bus.ErrReconnecting while nats.go reconnects
	// that connection, and succeeds once the connection is back.
	consumerCall := func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		_, err := rig.deps.consumers.consumers.Info(ctx, sessionID)
		return err
	}
	reconnecting := func(t *testing.T) {
		t.Helper()
		waitFor(t, 15*time.Second, "the listener's session-consumer connection to start reconnecting", func() bool {
			return errors.Is(consumerCall(), bus.ErrReconnecting)
		})
	}
	reconnected := func(t *testing.T) {
		t.Helper()
		waitFor(t, 30*time.Second, "the listener to reconnect", rig.harness.client.Connected)
		waitFor(t, 30*time.Second, "the listener's session-consumer connection to reconnect", func() bool { return consumerCall() == nil })
	}

	if !t.Run("an answer and a hand-over while the listener's connections are cut fail by name", func(t *testing.T) {
		proxy.Cut()
		reconnecting(t)
		// The session's done for the first frame, handed to the relay as its reply subscription
		// hands one on: with the listener's connections cut, no reply reaches it any other way.
		rig.relays.onReply(&natsgo.Msg{Subject: frames[0].msg.Reply, Data: []byte(relayReplyDone)})
		rig.relays.setOwned(false)
		awaitLogLine(t, rig, handOverFailed, 15*time.Second)
		proxy.Restore()
		reconnected(t)
		rig.relays.setOwned(true)
		// The refused ack did not reach the server: the ack floor is still below the first
		// message, so the relay that takes the session again, which restarts at that floor,
		// delivers both. The second comes back at the floor whether or not its nak reached the
		// server.
		requireFrames(t, standIn.collect(t, 2, 20*time.Second), first, second)
	}) {
		return
	}

	pulledBefore := 0
	if !t.Run("a relay through a NATS stop and start delivers its messages again", func(t *testing.T) {
		pulledBefore = len(rig.harness.logs.String())
		server.Stop(t)
		reconnecting(t)
		// The relay stops until the session's stand-in is back on the server, so no frame goes
		// out before something reads it.
		rig.relays.setOwned(false)
		server.Start(t)
		reconnected(t)
		// The stand-in's own connection ended with the server, so the session is back on another.
		again := startSessionStandIn(t, uri, sessionID, nil)
		rig.relays.setOwned(true)
		requireFrames(t, again.collect(t, 2, 20*time.Second), first, second)
	}) {
		return
	}

	logs := rig.harness.logs.String()
	for line := range strings.SplitSeq(logs, "\n") {
		if strings.Contains(line, reconnectBufferRefusal) {
			t.Errorf("a log line names nats.go's buffer refusal rather than the reconnect: %s", line)
		}
	}
	written := map[string]int{}
	for _, record := range warnLinesNaming(logs, sessionID) {
		message, _ := record["msg"].(string)
		failure, _ := record["error"].(string)
		switch {
		case message == answerFailed, message == handOverFailed:
			written[message]++
			if !strings.Contains(failure, reconnectingReason) {
				t.Errorf("the relay logged %q with error %q, want it naming %q", message, failure, reconnectingReason)
			}
		case message == pullEnded:
			if !strings.Contains(failure, reconnectingReason) && !strings.Contains(failure, serverShutdown) {
				t.Errorf("the relay logged %q with error %q, want it naming %q or the server's %q", message, failure, reconnectingReason, serverShutdown)
			}
		}
	}
	// The pull line's control: the second outage ends the relay's pull request, so at least one
	// such line comes after its start, and the error check above reads a line that is there.
	if pulled := len(linesIn(logs[pulledBefore:], pullEnded, map[string]any{"session_id": sessionID})); pulled == 0 {
		t.Errorf("the relay logged no %q for %s after the second outage began", pullEnded, sessionID)
	}
	if written[answerFailed] != 1 {
		t.Errorf("the relay logged %q %d times, want once, for the one frame the session answered while the connection was cut", answerFailed, written[answerFailed])
	}
	if written[handOverFailed] == 0 {
		t.Errorf("the relay never logged %q, the hand-over it made while the connection was cut", handOverFailed)
	}
}
