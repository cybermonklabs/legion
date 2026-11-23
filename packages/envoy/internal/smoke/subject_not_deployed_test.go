//go:build smoke

package smoke

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/bus/bustest"
	"github.com/sjawhar/envoy/internal/contracts"
)

// The listener image refuses a send while a hub move's quiesce has narrowed the stream, so that
// it no longer captures the target's agent subject: the send is the 503 that says the message was
// not sent (contracts.MessageNotSentPrefix) and names bus.ErrSubjectNotDeployed, and nothing goes
// out, neither to a core subscriber nor to the stream.
// It is the image's check, not the source's: an image built before the refusal, once its listener
// has read the narrowed subjects, answers such a send 200 and hands it to whoever is subscribed
// at that instant over core NATS, where nothing stores it.
//
// The listener in the image reads the deployed subjects every 60 s, so the test waits for that read
// by its effect: a send before it takes JetStream, and no stream answers it, which is a 503 that
// does not name the refusal.
func TestSmokeASendWhileTheStreamLacksTheAgentSubjectAnswers503(t *testing.T) {
	const (
		target = "ses_smoke_not_deployed"
		sender = "ses_smoke_not_deployed_sender"
	)
	natsC, baseURL := startSmokeStack(t)
	ctx := context.Background()

	register, err := apiRequest(http.MethodPost, baseURL+"/v1/interests/subscribe",
		`{"session_id":"`+target+`","topics":[],"self_subscribed":true,"delivery":{"consumer":true,"epoch":"smoke"}}`)
	if err != nil {
		t.Fatalf("register %s: %v", target, err)
	}
	register.Body.Close()
	if register.StatusCode != http.StatusOK {
		t.Fatalf("register %s: status %d", target, register.StatusCode)
	}

	natsURL, err := natsC.ConnectionString(ctx)
	if err != nil {
		t.Fatalf("read the NATS container's address: %v", err)
	}
	conn, err := natsgo.Connect(natsURL, natsgo.Timeout(5*time.Second))
	if err != nil {
		t.Fatalf("connect to NATS: %v", err)
	}
	t.Cleanup(conn.Close)
	js, err := conn.JetStream()
	if err != nil {
		t.Fatalf("open JetStream: %v", err)
	}
	bustest.NarrowStream(t, js, nil)

	type answer struct {
		status int
		body   string
		error  string
	}
	send := func() answer {
		t.Helper()
		resp, err := apiRequest(http.MethodPost, baseURL+"/v1/messages/send",
			`{"target_session":"`+target+`","source":"agent","source_session":"`+sender+`","message":"sent while the stream is narrowed"}`)
		if err != nil {
			t.Fatalf("POST /v1/messages/send: %v", err)
		}
		defer resp.Body.Close()
		raw, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read the send's answer: %v", err)
		}
		var decoded struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(raw, &decoded)
		return answer{status: resp.StatusCode, body: string(raw), error: decoded.Error}
	}
	refused := func(a answer) bool {
		return a.status == http.StatusServiceUnavailable && strings.Contains(a.error, bus.ErrSubjectNotDeployed.Error())
	}

	// Step 1: send once a second until the listener has read the narrowing, for at most 90 s.
	deadline := time.Now().Add(90 * time.Second)
	for {
		a := send()
		if a.status == http.StatusOK {
			t.Fatalf("a send while the stream lacks the agent subject was answered 200: %s", a.body)
		}
		if refused(a) {
			// The refusal opens with the prefix envoy-client reads as a message nothing stored,
			// never with "the message bus did not answer": the bus was never asked.
			if !strings.HasPrefix(a.error, contracts.MessageNotSentPrefix+": ") {
				t.Fatalf("the refusal reads %q, want it to open with %q", a.error, contracts.MessageNotSentPrefix+": ")
			}
			break
		}
		if a.status != http.StatusServiceUnavailable {
			t.Fatalf("a send while the stream lacks the agent subject: status %d, want 503: %s", a.status, a.body)
		}
		if time.Now().After(deadline) {
			t.Fatalf("no send in 90 s was refused naming %q; the last answer: %d %s", bus.ErrSubjectNotDeployed, a.status, a.body)
		}
		time.Sleep(time.Second)
	}

	// Step 2: only now a core subscriber of every notification topic, and the stream's last
	// sequence as the baseline. One held from the start would be handed the JetStream publishes of
	// step 1's earlier sends, which the listener made before its read.
	received, err := conn.SubscribeSync(bus.NotificationTopicPrefix + ">")
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if err := conn.Flush(); err != nil {
		t.Fatalf("flush the subscription: %v", err)
	}
	info, err := js.StreamInfo(bus.Stream)
	if err != nil {
		t.Fatalf("read the stream: %v", err)
	}
	baseline := info.State.LastSeq

	// Step 3: one more send is refused, and nothing of it goes out. The listener asks a session
	// on consumer delivery whether it is connected before it publishes: a core request on the
	// session's relay subject (bus.DeliverSubject) carrying bus.ConnectionProbeHeader with the
	// value "1" and no body, which is no copy of the message, so the subscriber passes over that
	// probe and nothing else.
	if a := send(); !refused(a) {
		t.Fatalf("the send after the listener's read: %d %s, want the 503 naming %q", a.status, a.body, bus.ErrSubjectNotDeployed)
	}
	if err := conn.Flush(); err != nil {
		t.Fatalf("flush the subscriber: %v", err)
	}
	quiet := time.Now().Add(500 * time.Millisecond)
	for {
		msg, err := received.NextMsg(time.Until(quiet))
		if errors.Is(err, natsgo.ErrTimeout) {
			break
		}
		if err != nil {
			t.Fatalf("read the subscriber: %v", err)
		}
		if msg.Subject == bus.DeliverSubject(target) && msg.Header.Get(bus.ConnectionProbeHeader) == "1" && len(msg.Data) == 0 {
			continue
		}
		t.Fatalf("the subscriber was handed %s (reply %q); a refused send sends nothing", msg.Subject, msg.Reply)
	}
	after, err := js.StreamInfo(bus.Stream)
	if err != nil {
		t.Fatalf("read the stream after the send: %v", err)
	}
	if after.State.LastSeq != baseline {
		t.Fatalf("the stream's last sequence moved from %d to %d; a refused send stores nothing", baseline, after.State.LastSeq)
	}
}
