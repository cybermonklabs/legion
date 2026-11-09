package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/id"
	"github.com/sjawhar/envoy/internal/session"
)

// deregister sends DELETE /v1/sessions/<sessionID><query> through the rig's routes and returns
// the answer; query carries the condition the caller names, "" for a request that names none.
// Every outcome of this route is a 200, so any other status is the row's failure.
func (rig *sessionConsumerRig) deregister(t *testing.T, sessionID, query string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	rig.mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodDelete, "/v1/sessions/"+sessionID+query, nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("DELETE /v1/sessions/%s%s: status %d: %s", sessionID, query, recorder.Code, recorder.Body.String())
	}
	return recorder
}

// requireNoConsumer fails the test unless the stream holds no consumer named for sessionID
// (bus.SessionConsumerName, s-<session id>), which is the slot the deregistration frees.
func (rig *sessionConsumerRig) requireNoConsumer(t *testing.T, sessionID string) {
	t.Helper()
	if info, err := rig.consumers.Info(context.Background(), sessionID); !errors.Is(err, bus.ErrSessionConsumerNotFound) {
		name, _ := bus.SessionConsumerName(sessionID)
		t.Fatalf("the stream holds %s (%+v, %v); want it deleted with the registration", name, info, err)
	}
}

// requireConsumer fails the test unless sessionID's consumer is on the stream.
func (rig *sessionConsumerRig) requireConsumer(t *testing.T, sessionID string) {
	t.Helper()
	if _, err := rig.consumers.Info(context.Background(), sessionID); err != nil {
		name, _ := bus.SessionConsumerName(sessionID)
		t.Fatalf("the stream holds no %s (%v); want the consumer of the live registration", name, err)
	}
}

// A deregistration that names no condition removes the registration whoever wrote it, so nothing
// is left to deliver to: its session consumer goes with it rather than holding a slot of the
// stream's consumer cap until the server's inactivity threshold reaches it.
func TestDeregistrationWithNoConditionDeletesTheSessionsConsumer(t *testing.T) {
	rig := newSessionConsumerRig(t)
	const sessionID = "ses_deregister_plain"
	rig.subscribe(t, `{"session_id":"`+sessionID+`","self_subscribed":true,"topics":[],
		"delivery":{"consumer":true,"epoch":"e"}}`)
	rig.requireConsumer(t, sessionID)

	recorder := rig.deregister(t, sessionID, "")

	if recorder.Body.Len() != 0 {
		t.Fatalf("the answer carries a body: %s; want none, as this route has always answered a request naming no condition", recorder.Body.String())
	}
	rig.requireNoConsumer(t, sessionID)
}

// The deregistration a client sends for its own instance deletes the consumer too: the answer
// says the registration is gone, and nothing stands behind it.
func TestDeregistrationOfTheSessionsOwnInstanceDeletesItsConsumer(t *testing.T) {
	rig := newSessionConsumerRig(t)
	const (
		sessionID  = "ses_deregister_own_instance"
		instanceID = "4f1c2a9e8b7d6c5a4f1c2a9e8b7d6c5a"
	)
	rig.subscribe(t, `{"session_id":"`+sessionID+`","self_subscribed":true,"topics":[],
		"client_instance_id":"`+instanceID+`","delivery":{"consumer":true,"epoch":"e"}}`)
	rig.requireConsumer(t, sessionID)

	recorder := rig.deregister(t, sessionID, "?client_instance_id="+instanceID)

	if got := strings.TrimSpace(recorder.Body.String()); got != `{"deleted":true}` {
		t.Fatalf("the answer is %s, want {\"deleted\":true}", got)
	}
	rig.requireNoConsumer(t, sessionID)
}

// A deregistration another client instance sends leaves a live registration standing, and that
// registration's session still reads its consumer: the consumer stays, as the entry does.
func TestDeregistrationOfAnotherInstanceKeepsTheSessionConsumer(t *testing.T) {
	rig := newSessionConsumerRig(t)
	const (
		sessionID = "ses_deregister_other_instance"
		holding   = "b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2"
		leaving   = "4f1c2a9e8b7d6c5a4f1c2a9e8b7d6c5a"
	)
	rig.subscribe(t, `{"session_id":"`+sessionID+`","self_subscribed":true,"topics":[],
		"client_instance_id":"`+holding+`","delivery":{"consumer":true,"epoch":"e"}}`)

	recorder := rig.deregister(t, sessionID, "?client_instance_id="+leaving)

	var answer deleteSessionResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil {
		t.Fatalf("decode the answer %s: %v", recorder.Body.String(), err)
	}
	if answer.Deleted || answer.Reason != "the registration belongs to another client instance" {
		t.Fatalf("the answer is %+v, want the registration kept for another client instance", answer)
	}
	if entry := rig.entry(t, sessionID); entry.Delivery != session.DeliveryConsumer {
		t.Fatalf("the session entry is %+v, want the other instance's registration still in consumer delivery", entry)
	}
	rig.requireConsumer(t, sessionID)
}

// A consumer deleted with its registration stays deleted while this listener keeps relaying: the
// delete dropped the pending start the relay's restarts leave behind, so a restart that was
// already past its wanted check finds the consumer missing and ends (bus.ErrSessionConsumerNotFound)
// rather than creating one at that start.
//
// No heartbeat runs during the DELETE here, and that is what the row rests on: a registration
// behind the delete writes the entry again and creates the consumer again by design, since the
// session is live again and must be delivered to. What must not happen is the listener's own
// retry bringing back the consumer of a session that registered no more.
func TestADeletedSessionsConsumerIsNotBroughtBackByTheRelaysRetry(t *testing.T) {
	// The relay's windows are shortened to a tenth of production's, so the row spends its own
	// wait over several of the relay's retries rather than seconds of the clock.
	const window = 100 * time.Millisecond
	rig := newRelayRigReceipt(t, window, 2*window, window)
	sessionID := "ses_deregister_retry_" + id.New()
	rig.subscribe(t, consumerRegistration(sessionID, "epoch-1"))
	// A message nothing answers: the relay sends its frame, pauses for want of a receipt, and
	// restarts the consumer at its ack floor again and again, each restart deleting the consumer
	// and creating it at that floor. That is the retry the delete must outlast.
	rig.storeEvent(t, contracts.AgentSubject(sessionID))
	rig.awaitPending(t, sessionID, 0)

	rig.deregister(t, sessionID, "")

	rig.requireNoConsumer(t, sessionID)
	for deadline := time.Now().Add(10 * window); time.Now().Before(deadline); time.Sleep(window / 2) {
		rig.requireNoConsumer(t, sessionID)
	}
}

// The resume lane is the listener's own: its record of where a deleted session consumer stopped,
// which is where that session's consumer is created again (bus.SessionResumeSubject). A publish
// to it through the route would let any bearer choose where another session's delivery resumes,
// skipping every message stored before the floor it wrote or handing the session again what it
// read already, so the route refuses that prefix as it refuses the kept lane, and stores nothing.
func TestAPublishToTheListenersResumeLaneIsRefused(t *testing.T) {
	d, client := keptRoleDeps(t)
	for _, subject := range []string{bus.SessionResumeSubject("ses_resume_mine"), bus.SessionResumeSubject("ses_resume_another")} {
		t.Run(subject, func(t *testing.T) {
			recorder, _ := publishThrough(d,
				`{"topic":"`+subject+`","source":"agent","message":"{\"floor\":1,\"stream_id\":\"mine\"}"}`)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("publish to %s: status %d, body %s; want 400", subject, recorder.Code, recorder.Body.String())
			}
			if body := recorder.Body.String(); !strings.Contains(body, bus.SessionResumeSubjectPrefix) {
				t.Fatalf("the refusal of %s is %q, want it to name %s", subject, body, bus.SessionResumeSubjectPrefix)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if _, err := client.LastStoredMessage(ctx, subject); !errors.Is(err, natsgo.ErrMsgNotFound) {
				t.Fatalf("the stream holds a message on %s (%v); the refused publish stored nothing", subject, err)
			}
		})
	}
}
