package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/listenerwire"
)

// registrationBehindALostStreamRead bounds a registration whose consumer create joins the stream's
// state behind a read the server never answers: the session consumers' 250 ms overlap bound
// (internal/bus, stream_state.go), the registration's own calls and room for a loaded test
// machine, against the 5 s that lost read takes to time out.
const registrationBehindALostStreamRead = time.Second

// A session's restart whose read of the notification stream's state is lost holds no other
// session's registration. Session A's restart at its ack floor (bus.SessionConsumers.RestartAtFloor,
// the call a relay restart makes) sends the stream info every session's consumer calls share, and
// the relay drops it on a connection that stays up, so nothing answers it for its whole 5 s. Session
// B then registers for the first time: its consumer create joins the next read of the stream's
// state, which starts once A's has been out the overlap bound, so B is answered 200 well inside the
// registration's 3.5 s store budget. Behind A's whole 5 s that budget ran out, and B was answered
// 503 though nothing was wrong with B or with the bus (2fdc4f23, run 2 of the hand-back row).
func TestARegistrationBehindAnotherSessionsLostStreamReadIsAnswered(t *testing.T) {
	const (
		restarting  = "ses_restart_whose_stream_read_is_lost"
		registering = "ses_registers_behind_the_lost_read"
	)
	client := setupPublishTestClient(t)
	registry, sessions := setupSessionsTest(t, nil, nil)
	relay := startSubjectStallRelay(t, testnats.URL(t))
	delivery, err := connectSessionDelivery([]string{relay.relayed}, bus.SessionConsumerInactiveThreshold, client, logging.NewWithWriter("test", io.Discard))
	if err != nil {
		t.Fatalf("open the session-consumer connection through the relay: %v", err)
	}
	t.Cleanup(delivery.close)
	delivery.relay = &recordingRelay{}
	mux := http.NewServeMux()
	registerV1Routes(mux, &listenerDeps{client: client, registry: registry, sessions: sessions, consumers: delivery}, "test-machine", logging.New("test"))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	agent := []string{contracts.LegacyAgentSubject(restarting)}
	if _, err := delivery.consumers.Ensure(ctx, restarting, agent, 0); err != nil {
		t.Fatalf("create the restarting session's consumer: %v", err)
	}
	relay.stallFirst(1, "$JS.API.STREAM.INFO."+bus.Stream)
	restarted := make(chan struct{})
	t.Cleanup(func() {
		cancel()
		<-restarted
	})
	go func() {
		defer close(restarted)
		_, _ = delivery.consumers.RestartAtFloor(ctx, restarting, agent)
	}()
	waitFor(t, 5*time.Second, "the restart's stream read to be dropped", func() bool { return relay.droppedRequests() == 1 })

	body, err := json.Marshal(listenerwire.SubscribeRequest{SessionID: registering, Topics: []string{contracts.LegacyAgentSubject(registering)}, SelfSubscribed: true,
		Delivery: &listenerwire.SubscribeDelivery{Consumer: true, Epoch: "e"}})
	if err != nil {
		t.Fatalf("marshal the registration: %v", err)
	}
	recorder := httptest.NewRecorder()
	started := time.Now()
	mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/interests/subscribe", strings.NewReader(string(body))))
	took := time.Since(started)
	if recorder.Code != http.StatusOK || took > registrationBehindALostStreamRead {
		t.Fatalf("a registration behind another session's lost stream read answered %d in %s (body %s); want 200 within %s",
			recorder.Code, took.Round(time.Millisecond), recorder.Body.String(), registrationBehindALostStreamRead)
	}
}
