package main

// The rig several of this package's tests share: a bus client of the test's own, the envelopes it
// publishes through it, the wait for a durable to have settled, the wait for a started listener
// to list a session as registered through a machine, and the wait for a product bound to stand
// armed on a test's fake clock. They are here, in a file that tests nothing itself, because the
// tests that use them are spread across this package and no one of those files owns them.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/id"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// connectPublisher opens a bus client of the test's own, apart from the listener's: a publish
// through it reaches NATS as one from any session holding its own connection does, core NATS for a
// topic the stream does not store and JetStream for one it does (bus.Client.Publish).
func connectPublisher(t *testing.T, uri string) *bus.Client {
	t.Helper()
	client, err := bus.Connect([]string{uri}, bus.WithName("listener-test-publisher"))
	if err != nil {
		t.Fatalf("connect the publisher: %v", err)
	}
	t.Cleanup(client.Close)
	return client
}

// newEnvelope is a valid envelope from source on topic carrying message, with ids of its own. Its
// dedupe key keeps the prefix it was written with: role_lane_test.go compares a role forward's
// dedupe key against that prefix, so the two are one choice.
func newEnvelope(source, topic, message string) contracts.Envelope {
	eventID := "evt-" + id.New()
	return contracts.Envelope{
		EventID:        eventID,
		Source:         source,
		SourceEventID:  eventID,
		Topic:          topic,
		DedupeKey:      "live-topics-test." + eventID,
		IssuedAt:       contracts.NowMillis(),
		PayloadSummary: message,
		Payload:        message,
		TraceID:        "trace-" + eventID,
	}
}

// publish publishes item through client and flushes, so the server holds it when publish returns.
func publish(t *testing.T, client *bus.Client, item contracts.Envelope) {
	t.Helper()
	if err := client.Publish(item); err != nil {
		t.Fatalf("publish %s: %v", item.Topic, err)
	}
	if err := client.Conn.Flush(); err != nil {
		t.Fatalf("flush the publish of %s: %v", item.Topic, err)
	}
}

// awaitDurableState waits for durable to hold pending messages for an ack with every message
// through floor acked.
func awaitDurableState(t *testing.T, client *bus.Client, durable string, pending int, floor uint64) {
	t.Helper()
	testwait.Until(t, fmt.Sprintf("the durable %s to hold %d pending ack with everything through %d acked", durable, pending, floor),
		func() (string, bool) {
			info, err := client.JS().ConsumerInfo(bus.Stream, durable)
			if err != nil {
				t.Fatalf("read the durable %s: %v", durable, err)
			}
			return fmt.Sprintf("%d pending ack, ack floor %d", info.NumAckPending, info.AckFloor.Stream),
				info.NumAckPending == pending && info.AckFloor.Stream >= floor
		})
}

// awaitSessionConsumerStored waits for the state file of sessionID's consumer to hold the position
// the server reports for it, read through conn, a connection of the test's on the consumer's
// account (testnats.AwaitConsumerStored, which says why a delete of a one-copy consumer followed by
// a create of its name waits for that). A test calls it right before the step that has the
// listener delete the consumer after it delivered or was acked - a deregistration, a delete and
// its restore, a relay restart at the ack floor - once the server has taken every ack the test's
// session sent: the wait sees only the state the server already holds.
func awaitSessionConsumerStored(t *testing.T, conn *natsgo.Conn, sessionID string) {
	t.Helper()
	name, ok := bus.SessionConsumerName(sessionID)
	if !ok {
		t.Fatalf("%s names no session consumer", sessionID)
	}
	testnats.AwaitConsumerStored(t, conn, bus.Stream, name)
}

// awaitSessionMachine waits for listener's GET /v1/sessions to list sessionID as registered
// through machineID, so a delivery through this listener reads that session entry.
func (p *listenerProcess) awaitSessionMachine(t *testing.T, sessionID, machineID string) {
	t.Helper()
	testwait.Until(t, fmt.Sprintf("the listener on port %d to list %s as registered through %s", p.port, sessionID, machineID),
		func() (string, bool) {
			status, body := callListener(t, p.port, http.MethodGet, "/v1/sessions", "")
			var listed []struct {
				SessionID string `json:"session_id"`
				MachineID string `json:"machine_id"`
			}
			if status != http.StatusOK || json.Unmarshal([]byte(body), &listed) != nil {
				return fmt.Sprintf("%d %s", status, body), false
			}
			for _, row := range listed {
				if row.SessionID == sessionID && row.MachineID == machineID {
					return body, true
				}
			}
			return body, false
		})
}

// awaitArmed waits until clk holds a timer whose deadline matches, and returns the deadlines it
// saw pending then. It names the one product bound a test is about: a fake clock carries every
// bound the code arms on it, so a test that means one of them waits for that bound's own deadline
// rather than for a count of timers.
func awaitArmed(t *testing.T, clk *clocktest.Fake, what string, matches func(time.Time) bool) []time.Time {
	t.Helper()
	return testwait.Until(t, what, func() ([]time.Time, bool) {
		armed := clk.Armed()
		return armed, slices.ContainsFunc(armed, matches)
	})
}

// armedAt matches a timer armed for exactly want, the deadline a test computes from the moment its
// fake stood at when the code was started and the bound's length.
func armedAt(want time.Time) func(time.Time) bool {
	return func(deadline time.Time) bool { return deadline.Equal(want) }
}

// awaitArmedBeforeReturn waits until clk's pending deadlines are the ones the test is about
// (ready), as awaitArmed and clocktest's AwaitArmed do, unless the code that is to arm them has
// returned first, which returned reports along with what it returned. Code that has returned arms
// nothing more, so a wait for its timers alone would end only at the test binary's deadline, and
// take every later row of the binary with it; a return first fails the test at once, naming what
// the code returned. The deadlines are read before the return, so code that armed them and is
// waiting on them is never taken for code that returned.
func awaitArmedBeforeReturn(t *testing.T, clk *clocktest.Fake, what string, ready func([]time.Time) bool, returned func() (string, bool)) []time.Time {
	t.Helper()
	var outcome string
	var first bool
	armed := testwait.Until(t, what+", or the return of the code that arms it", func() ([]time.Time, bool) {
		armed := clk.Armed()
		if ready(armed) {
			return armed, true
		}
		outcome, first = returned()
		return armed, first
	})
	if first {
		t.Fatalf("the code returned before it armed %s: %s", what, outcome)
	}
	return armed
}

// timersArmed is ready once at least n deadlines are pending, what clocktest's AwaitArmed waits for.
func timersArmed(n int) func([]time.Time) bool {
	return func(armed []time.Time) bool { return len(armed) >= n }
}

// deadlineArmed is ready once a pending deadline matches, what awaitArmed waits for.
func deadlineArmed(matches func(time.Time) bool) func([]time.Time) bool {
	return func(armed []time.Time) bool { return slices.ContainsFunc(armed, matches) }
}

// answerReturned reports a route's return once its answer is in answers, which holds one, with the
// answer's status and body. It takes the answer, since the test fails on it.
func answerReturned(answers <-chan *httptest.ResponseRecorder) func() (string, bool) {
	return func() (string, bool) {
		select {
		case recorder := <-answers:
			return fmt.Sprintf("it answered %d: %s", recorder.Code, recorder.Body.String()), true
		default:
			return "", false
		}
	}
}

// closeReturned reports the code's return once done is closed, with what describe reads of what it
// returned then.
func closeReturned(done <-chan struct{}, describe func() string) func() (string, bool) {
	return func() (string, bool) {
		select {
		case <-done:
			return describe(), true
		default:
			return "", false
		}
	}
}

// recorderAnswer is what describe reads of a route that wrote its answer into recorder.
func recorderAnswer(recorder *httptest.ResponseRecorder) func() string {
	return func() string { return fmt.Sprintf("it answered %d: %s", recorder.Code, recorder.Body.String()) }
}
