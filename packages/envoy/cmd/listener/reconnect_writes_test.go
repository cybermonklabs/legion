package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptrace"
	"strconv"
	"strings"
	"testing"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/id"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
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

// reconnectWritesCall posts one request. The only wait bound is in ctx.
func reconnectWritesCall(ctx context.Context, port int, path, body string, written func()) reconnectWritesAnswer {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://127.0.0.1:"+strconv.Itoa(port)+path, strings.NewReader(body))
	if err != nil {
		return reconnectWritesAnswer{err: err}
	}
	request.Header.Set("Authorization", "Bearer "+listenerTestToken)
	request.Header.Set("Content-Type", "application/json")
	if written != nil {
		request = request.WithContext(httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
			WroteRequest: func(info httptrace.WroteRequestInfo) {
				if info.Err == nil {
					written()
				}
			},
		}))
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return reconnectWritesAnswer{err: err}
	}
	defer response.Body.Close()
	answer, err := io.ReadAll(response.Body)
	return reconnectWritesAnswer{status: response.StatusCode, body: string(answer), err: err}
}

func callReconnectRoute(t *testing.T, listener *listenerProcess, path, body string) reconnectWritesAnswer {
	t.Helper()
	ctx := testwait.Context(t)
	var answer reconnectWritesAnswer
	testwait.Returns(t, path+" response from the listener", func() {
		answer = reconnectWritesCall(ctx, listener.port, path, body, nil)
	})
	return answer
}

func postReconnectRoute(t *testing.T, listener *listenerProcess, path, body string) {
	t.Helper()
	if answer := callReconnectRoute(t, listener, path, body); answer.err != nil || answer.status != http.StatusOK {
		t.Fatalf("%s answered %s, want 200", path, answer)
	}
}

const childDisconnect = "envoy nats disconnected name=envoy "
const childReconnect = "envoy nats reconnected name=envoy "

// waitReconnectOutput reads only lines after the action that may have caused them.
func waitReconnectOutput(t *testing.T, listener *listenerProcess, from int, what string, match func(string) bool) string {
	t.Helper()
	return testwait.Until(t, what, func() (string, bool) {
		output := listener.Output.String()
		for line := range strings.SplitSeq(output[from:], "\n") {
			if match(line) {
				return line, true
			}
		}
		select {
		case <-listener.Exited:
			t.Fatalf("listener exited before %s:\n%s", what, output)
		default:
		}
		return output[from:], false
	})
}

func waitChildLine(t *testing.T, listener *listenerProcess, from int, text string) string {
	t.Helper()
	return waitReconnectOutput(t, listener, from, "the child to log "+text, func(line string) bool {
		return strings.Contains(line, text)
	})
}

func waitChildRecord(t *testing.T, listener *listenerProcess, from int, message, key, value string) string {
	t.Helper()
	return waitReconnectOutput(t, listener, from, fmt.Sprintf("the child to log %q with %s=%q", message, key, value), func(line string) bool {
		var record map[string]any
		return json.Unmarshal([]byte(line), &record) == nil && record["msg"] == message && record[key] == value
	})
}

func stopReconnectServer(t *testing.T, server *testnats.Server, listener *listenerProcess) {
	t.Helper()
	from := len(listener.Output.String())
	server.Stop(t)
	waitChildLine(t, listener, from, childDisconnect)
}

// replaceWithShortBudgetListener leaves the fixture's stored registration on the bus.
// Setup requests use the production budget. Only the outage runs in the shorter build.
func replaceWithShortBudgetListener(t *testing.T, setup *listenerProcess, binary, uri, machineID string) *listenerProcess {
	t.Helper()
	if err := setup.Cmd.Process.Kill(); err != nil {
		t.Fatalf("stop the setup listener: %v", err)
	}
	setup.WaitExit(t, "stopping the setup listener")
	listener := startListenerProcess(t, binary, uri, machineID)
	listener.waitHealthy(t)
	return listener
}

func startShortBudgetReconnectFixture(t *testing.T, machineID, registration string) (*testnats.Server, *listenerProcess) {
	t.Helper()
	prompt, short := promptListenerBinary(t), shortBudgetListenerBinary(t)
	server, uri := testnats.Start(t)
	setup := startListenerProcess(t, prompt, uri, machineID)
	setup.waitHealthy(t)
	postReconnectRoute(t, setup, "/v1/interests/subscribe", registration)
	return server, replaceWithShortBudgetListener(t, setup, short, uri, machineID)
}

// A write submitted while NATS reconnects waits for the connection or answers 503 by
// name. A refused or cancelled write does not reach the server once it returns.
//
// A role claim reads the claimant's session from the session bucket's stream leader (#5132), and
// that read fails at once, by name, while NATS reconnects, so the claim does not wait within its
// deadline. R14 of dispatch://INFRA-3589 allows both. So the write the third outage gives up on is
// a publish, which waits for the connection.
func TestAWriteThroughANATSReconnectWaitsOrFailsByName(t *testing.T) {
	t.Parallel()
	const machineID, sessionID = "reconnect-writes", "ses_reconnect_writes"
	prompt, short := promptListenerBinary(t), shortBudgetListenerBinary(t)

	claim := func(role string) string {
		return `{"session_id":"` + sessionID + `","role":"` + role + `"}`
	}
	publishBody := func(topic string) string {
		return `{"topic":"` + topic + `","message":"a publish through a NATS reconnect"}`
	}
	watchBufferRefusal := func(t *testing.T, listener *listenerProcess) {
		t.Helper()
		t.Cleanup(func() {
			if strings.Contains(listener.Output.String(), reconnectBufferRefusal) {
				t.Errorf("the listener logged nats.go's buffer refusal instead of the reconnect:\n%s", listener.Output.String())
			}
		})
	}
	start := func(t *testing.T) (*testnats.Server, string, *listenerProcess) {
		t.Helper()
		server, uri := testnats.Start(t)
		listener := startListenerProcess(t, prompt, uri, machineID)
		listener.waitHealthy(t)
		postReconnectRoute(t, listener, "/v1/interests/subscribe", consumerRegistration(sessionID, "epoch-1"))
		postReconnectRoute(t, listener, "/v1/roles/set", claim("reconnect-writes-before"))
		watchBufferRefusal(t, listener)
		return server, uri, listener
	}
	observe := func(t *testing.T, uri string) natsgo.JetStreamContext {
		t.Helper()
		conn := testnats.Connect(t, uri)
		t.Cleanup(conn.Close)
		js, err := conn.JetStream(natsgo.Context(testwait.Context(t)))
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
	startCall := func(ctx context.Context, listener *listenerProcess, path, body string, written func()) <-chan reconnectWritesAnswer {
		answered := make(chan reconnectWritesAnswer, 1)
		go func() { answered <- reconnectWritesCall(ctx, listener.port, path, body, written) }()
		return answered
	}
	readAnswer := func(t *testing.T, what string, answered <-chan reconnectWritesAnswer) reconnectWritesAnswer {
		t.Helper()
		answer := testwait.Receive(t, what, answered)
		if strings.Contains(answer.body, reconnectBufferRefusal) {
			t.Errorf("an answer names nats.go's buffer refusal rather than the reconnect: %s", answer)
		}
		return answer
	}
	waitedOrNamed := func(t *testing.T, what string, answer reconnectWritesAnswer) {
		t.Helper()
		if answer.err != nil || (answer.status != http.StatusOK && (answer.status != http.StatusServiceUnavailable || !strings.Contains(answer.body, reconnectingReason))) {
			t.Errorf("%s during the outage answered %s, want 200 once the server was back or 503 naming %q", what, answer, reconnectingReason)
		}
	}
	// The marker uses the same listener publish path and connection. A publish buffered
	// through the outage would reach JetStream on that connection before the marker.
	marker := func(t *testing.T, listener *listenerProcess, js natsgo.JetStreamContext, topic string) {
		t.Helper()
		postReconnectRoute(t, listener, "/v1/messages/publish", publishBody(topic))
		testwait.Until(t, "the listener's marker "+topic+" to reach the stream", func() (error, bool) {
			_, err := js.GetLastMsg(bus.Stream, topic)
			return err, err == nil
		})
	}

	server, uri, listener := start(t)
	t.Run("a role claim refresh and a publish during an outage wait for the server or fail by name", func(t *testing.T) {
		const role, topic = "reconnect-writes-waited", "notifications.team.reconnect-writes-waited"
		listener.waitHealthy(t)
		stopReconnectServer(t, server, listener)
		written := make(chan struct{}, 2)
		wrote := func() { written <- struct{}{} }
		ctx := testwait.Context(t)
		claimed := startCall(ctx, listener, "/v1/roles/set", claim(role), wrote)
		published := startCall(ctx, listener, "/v1/messages/publish", publishBody(topic), wrote)
		testwait.Receive(t, "the first outage request to be written to the child's socket", written)
		testwait.Receive(t, "both outage requests to be written to the child's socket", written)
		server.Start(t)
		claimAnswer := readAnswer(t, "the role claim refresh answer", claimed)
		publishAnswer := readAnswer(t, "the publish answer", published)
		t.Logf("during the outage the role claim refresh answered %s and the publish %s", claimAnswer, publishAnswer)
		waitedOrNamed(t, "the role claim refresh", claimAnswer)
		waitedOrNamed(t, "the publish", publishAnswer)
		js := observe(t, uri)
		if claimAnswer.status == http.StatusOK {
			if _, err := bucket(t, js, bus.RoleClaimBucket).Get(role); err != nil {
				t.Errorf("the role claim answered 200 is not in the role bucket: %v", err)
			}
		}
		if publishAnswer.status == http.StatusOK {
			if _, err := js.GetLastMsg(bus.Stream, topic); err != nil {
				t.Errorf("the publish answered 200 is not in the stream: %v", err)
			}
		}
	})

	t.Run("a role claim refresh and a publish whose deadline ends in an outage fail by name and land nothing", func(t *testing.T) {
		const role, topic = "reconnect-writes-ended", "notifications.team.reconnect-writes-ended"
		server, uri, listener := start(t)
		js := observe(t, uri)
		sessions := bucket(t, js, bus.SessionBucket)
		initial, err := sessions.Get(sessionID)
		if err != nil {
			t.Fatalf("control: read the session record before the claim refresh: %v", err)
		}
		const controlRole = "reconnect-writes-control"
		postReconnectRoute(t, listener, "/v1/roles/set", claim(controlRole))
		if _, err := bucket(t, js, bus.RoleClaimBucket).Get(controlRole); err != nil {
			t.Fatalf("control: role %s's claim is not in the role bucket: %v", controlRole, err)
		}
		before, err := sessions.Get(sessionID)
		if err != nil || before.Revision() == initial.Revision() {
			t.Fatalf("control: the session record the claim refresh writes stayed at revision %d: %v, %v", initial.Revision(), before, err)
		}
		listener = replaceWithShortBudgetListener(t, listener, short, uri, machineID)
		watchBufferRefusal(t, listener)
		probe := connectPublisher(t, uri)
		stopReconnectServer(t, server, listener)
		testwait.Until(t, "the test's own bus connection to start reconnecting", func() (natsgo.Status, bool) {
			status := probe.Conn.Status()
			return status, status == natsgo.RECONNECTING
		})
		if err := probe.Conn.Publish("reconnect-writes.control", []byte("{}")); err == nil || !strings.Contains(err.Error(), reconnectBufferRefusal) {
			t.Fatalf("control: a send on the test's own bus connection in the outage answered %v, want nats.go's %q", err, reconnectBufferRefusal)
		}
		from := len(listener.Output.String())
		ctx := testwait.Context(t)
		claimed := startCall(ctx, listener, "/v1/roles/set", claim(role), nil)
		published := startCall(ctx, listener, "/v1/messages/publish", publishBody(topic), nil)
		claimAnswer := readAnswer(t, "the role claim refresh to answer before NATS returns", claimed)
		publishAnswer := readAnswer(t, "the publish to answer before NATS returns", published)
		t.Logf("in the outage the child answered: role claim refresh %s, publish %s", claimAnswer, publishAnswer)
		for _, want := range []struct {
			name, reason string
			answer       reconnectWritesAnswer
		}{
			{"role claim refresh", "confirm role claimant registration:", claimAnswer},
			{"publish", reconnectingReason + ": context deadline exceeded", publishAnswer},
		} {
			if want.answer.err != nil || want.answer.status != http.StatusServiceUnavailable ||
				!strings.Contains(want.answer.body, reconnectingReason) || !strings.Contains(want.answer.body, want.reason) {
				t.Fatalf("%s in the outage answered %s, want 503 naming %q and %q", want.name, want.answer, reconnectingReason, want.reason)
			}
		}
		line := waitChildRecord(t, listener, from, "listener publish", "topic", topic)
		if !strings.Contains(line, `"status":503`) || !strings.Contains(line, `"outcome":"bus_unavailable"`) {
			t.Fatalf("the child's publish log before reconnect = %s, want bus-unavailable 503", line)
		}
		t.Log(line)
		from = len(listener.Output.String())
		server.Start(t)
		waitChildLine(t, listener, from, childReconnect)
		listener.waitHealthy(t)
		js = observe(t, uri)
		marker(t, listener, js, topic+"-marker")
		if entry, err := bucket(t, js, bus.RoleClaimBucket).Get(role); !errors.Is(err, natsgo.ErrKeyNotFound) {
			t.Errorf("role %s's claim, answered 503 during the outage, reached the server after it came back: %v, %v", role, entry, err)
		}
		if after, err := bucket(t, js, bus.SessionBucket).Get(sessionID); err != nil || after.Revision() != before.Revision() {
			t.Errorf("the session record the role claim refresh writes moved from revision %d during the outage: %v, %v", before.Revision(), after, err)
		}
		if message, err := js.GetLastMsg(bus.Stream, topic); !errors.Is(err, natsgo.ErrMsgNotFound) {
			t.Errorf("the publish on %s, answered 503 during the outage, reached the stream before the marker: %v, %v", topic, message, err)
		}
	})

	t.Run("a publish whose caller gives up in an outage lands nothing when the server is back before the route's deadline", func(t *testing.T) {
		const topic = "notifications.team.reconnect-writes-given-up"
		listener.waitHealthy(t)
		stopReconnectServer(t, server, listener)
		from := len(listener.Output.String())
		ctx, cancel := context.WithCancel(testwait.Context(t))
		defer cancel()
		answered := startCall(ctx, listener, "/v1/messages/publish", publishBody(topic), nil)
		waitChildRecord(t, listener, from, "listener publish entering bus call", "topic", topic)
		cancel()
		if answer := readAnswer(t, "the client to give up on the publish", answered); !errors.Is(answer.err, context.Canceled) {
			t.Fatalf("the publish the caller cancelled answered %s, want context canceled", answer)
		}
		// The route must end because of the caller, not because its own deadline expired.
		line := waitChildRecord(t, listener, from, "listener publish bus call failed", "topic", topic)
		if !strings.Contains(line, reconnectingReason+": context canceled") {
			t.Fatalf("the child's bus call ended with %s, want the reconnect wait cancelled by its caller", line)
		}
		line = waitChildRecord(t, listener, from, "listener publish", "topic", topic)
		if !strings.Contains(line, `"status":503`) {
			t.Fatalf("the child's cancelled publish log = %s, want 503", line)
		}
		from = len(listener.Output.String())
		server.Start(t)
		waitChildLine(t, listener, from, childReconnect)
		js := observe(t, uri)
		marker(t, listener, js, topic+"-marker")
		if message, err := js.GetLastMsg(bus.Stream, topic); !errors.Is(err, natsgo.ErrMsgNotFound) {
			t.Errorf("the publish on %s reached the stream after its caller cancelled it: %v (%v)", topic, message, err)
		}
	})
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
	rig := newPromptSessionConsumerRig(t, proxy.URL(), proxy.URL())
	ctx := testwait.Context(t)
	observer, err := bus.ConnectSessionConsumers([]string{uri}, 0, nil, bus.WithPromptReconnect())
	if err != nil {
		t.Fatalf("connect the test's view of the session consumers: %v", err)
	}
	t.Cleanup(observer.Close)
	relays, err := startSessionRelays(rig.deps.consumers, rig.harness.config)
	if err != nil {
		t.Fatalf("start the session relays: %v", err)
	}
	t.Cleanup(func() { relays.stop(ctx) })
	// This row takes the frames away by withdrawing ownership, not by a receipt timeout.
	relays.receiptWait = math.MaxInt64
	relays.setOwned(true)
	sessionID := "ses_relay_reconnect_" + id.New()
	standIn := startSessionStandIn(t, uri, sessionID, nil)
	registerReconnectSession(t, rig, consumerRegistration(sessionID, "epoch-1"))
	agent := contracts.LegacyAgentSubject(sessionID)
	store := func() contracts.Envelope {
		item := listenerTestEnvelope(agent, "relay-test-"+id.New())
		if _, err := rig.harness.client.JS().Publish(agent, marshalListenerEnvelope(t, item), natsgo.Context(ctx)); err != nil {
			t.Fatalf("store a message for the relay: %v", err)
		}
		return item
	}
	first, second := store(), store()
	collect := func(t *testing.T, session *sessionStandIn) []relayedFrame {
		t.Helper()
		return []relayedFrame{
			testwait.Receive(t, "the first frame at the session stand-in", session.frames),
			testwait.Receive(t, "the second frame at the session stand-in", session.frames),
		}
	}
	frames := collect(t, standIn)
	requireFrames(t, frames, first, second)
	testwait.Until(t, "the consumer to hold two pending acks and an ack floor of zero", func() (string, bool) {
		info, err := observer.Info(ctx, sessionID)
		if err != nil {
			return err.Error(), false
		}
		return fmt.Sprintf("%d pending, floor %d", info.NumAckPending, info.AckFloor.Stream), info.NumAckPending == 2 && info.AckFloor.Stream == 0
	})

	reconnected := func(t *testing.T, from int) {
		t.Helper()
		testwait.Until(t, "the listener to reconnect", func() (bool, bool) {
			connected := rig.harness.client.Connected()
			return connected, connected
		})
		waitConsumerConnectionEvent(t, rig, from, "envoy nats reconnected")
		if _, err := rig.deps.consumers.consumers.Info(ctx, sessionID); err != nil {
			t.Fatalf("read the session consumer after reconnect: %v", err)
		}
	}

	if !t.Run("an answer and a hand-over while the listener's connections are cut fail by name", func(t *testing.T) {
		from := len(rig.harness.logs.String())
		proxy.Cut()
		waitConsumerConnectionEvent(t, rig, from, "envoy nats disconnected")
		// The session's done for the first frame, handed to the relay as its reply subscription
		// hands one on: with the listener's connections cut, no reply reaches it any other way.
		relays.onReply(&natsgo.Msg{Subject: frames[0].msg.Reply, Data: []byte(relayReplyDone)})
		relays.setOwned(false)
		testwait.Until(t, "the relay to log "+handOverFailed, func() (string, bool) {
			logs := rig.harness.logs.String()[from:]
			return logs, strings.Contains(logs, handOverFailed)
		})
		proxy.Restore()
		reconnected(t, from)
		relays.setOwned(true)
		// The refused ack did not reach the server: the ack floor is still below the first
		// message, so the relay that takes the session again, which restarts at that floor,
		// delivers both. The second comes back at the floor whether or not its nak reached the
		// server.
		requireFrames(t, collect(t, standIn), first, second)
	}) {
		return
	}

	pulledBefore := 0
	if !t.Run("a relay through a NATS stop and start delivers its messages again", func(t *testing.T) {
		pulledBefore = len(rig.harness.logs.String())
		server.Stop(t)
		waitConsumerConnectionEvent(t, rig, pulledBefore, "envoy nats disconnected")
		// The relay stops until the session's stand-in is back on the server, so no frame goes
		// out before something reads it.
		relays.setOwned(false)
		server.Start(t)
		reconnected(t, pulledBefore)
		// The stand-in's own connection ended with the server, so the session is back on another.
		again := startSessionStandIn(t, uri, sessionID, nil)
		relays.setOwned(true)
		requireFrames(t, collect(t, again), first, second)
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
