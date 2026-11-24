package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/roles"
	"github.com/sjawhar/envoy/listenerwire"
)

// A publish-route forward is a delivery only when the stream stores it inside the fence and the
// route's own budget after the resolution that chose its holder (roles.ForwardStoredTooLate). The
// route resolves A, passes the fence, and its forward waits inside the listener's forward
// (holdForward) while B takes the role, the listener's clock moves 65 minutes, past the life of
// the takeover record B's claim wrote, and the sweep ends that record. Released, the stream stores
// the forward on A's own subject, where nothing will copy it to B: the sender is told the send
// failed, 504 delivery_failed naming roles.ErrForwardStoredTooLate and A, not 200. The sender's
// retry under the same key resolves B, and B receives the message exactly once, through a sweep
// after it too. Control: at the base the route answers the held forward 200 delivered for A, and B
// never receives the message.
func TestAPublishForwardStoredPastItsFenceFailsAndItsRetryReachesTheNewHolderOnce(t *testing.T) {
	rig := newCrashRig(t)
	const a, b, role = "ses_publate_a", "ses_publate_b", "publate"
	const message = "a publish whose forward waits past its fence"
	rig.gone(a)
	rig.claim(rig.away, a, role)
	next := rig.relayed(b, answerDone)
	body, err := json.Marshal(map[string]any{
		"topic": contracts.RoleTopicPrefix + role, "source": "agent", "source_session": crashSender,
		"message": message, "idempotency_key": uuid.NewString(),
	})
	if err != nil {
		t.Fatal(err)
	}

	held, release := rig.holdForward(message)
	answered := make(chan *httptest.ResponseRecorder, 1)
	go func() { answered <- rig.send(rig.mux, http.MethodPost, "/v1/messages/publish", string(body)) }()
	crashAwait(t, held, 10*time.Second, "the publish's forward to "+a+" passing its fence and waiting")
	rig.claim(rig.mux, b, role)
	rig.clock.advance(65 * time.Minute)
	rig.sweep(rig.cfg(), role)
	release()
	var first *httptest.ResponseRecorder
	select {
	case first = <-answered:
	case <-time.After(30 * time.Second):
		t.Fatal("the held publish was not answered within 30 s of its release")
	}

	var failure listenerwire.RoleSendFailure
	if err := json.Unmarshal(first.Body.Bytes(), &failure); err != nil || first.Code != http.StatusGatewayTimeout ||
		failure.Reason != string(sendDeliveryFailed) || failure.Holder != a || !strings.Contains(failure.Error, roles.ErrForwardStoredTooLate.Error()) {
		t.Fatalf("the held publish answered %d %s, want 504 delivery_failed naming %s and %q: the stream stored its forward 65 minutes after %s was resolved, after the takeover record had ended, so nothing copies it to %s",
			first.Code, first.Body.String(), a, roles.ErrForwardStoredTooLate, a, b)
	}
	// The row's state is real: the forward the answer calls no delivery is on A's own subject.
	rig.awaitStored(contracts.LegacyAgentSubject(a), failure.EventID, "the late forward on "+a+"'s own subject")

	retry := rig.send(rig.mux, http.MethodPost, "/v1/messages/publish", string(body))
	var delivered listenerwire.PublishResponse
	if err := json.Unmarshal(retry.Body.Bytes(), &delivered); err != nil || retry.Code != http.StatusOK || delivered.Holder != b {
		t.Fatalf("the sender's retry answered %d %s, want 200 naming the new holder %s", retry.Code, retry.Body.String(), b)
	}
	got := crashAwaitEvents(t, next, []string{delivered.EventID}, nil, "the new holder, from the sender's retry")
	rig.sweep(rig.cfg(), role)
	got = append(got, crashDrain(next, 2*time.Second)...)
	received := 0
	for _, frame := range got {
		if frame.item.PayloadSummary == message {
			received++
		}
	}
	if received != 1 {
		t.Fatalf("the new holder %s received the message %d times (frames %v), want exactly once", b, received, frameEvents(got))
	}
}
