package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
)

func TestTailPrintsEnvelopesOnTheSubject(t *testing.T) {
	server, url := testnats.Start(t)
	// The deadline bounds the tail, not the server's start: the server runs in this process and
	// is accepting connections by the time Start returns, so the only thing left to outlive its
	// deadline is the tail itself.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	publisher, err := bus.ConnectOwningStream([]string{url})
	if err != nil {
		t.Fatalf("connect publisher: %v", err)
	}
	t.Cleanup(publisher.Close)

	var output bytes.Buffer
	result := make(chan error, 1)
	go func() {
		result <- tail(ctx, []string{url}, "notifications.dispatch.document.T.d.>", 2, &output)
	}()
	waitForNATSSubscription(t, ctx, server, "notifications.dispatch.document.T.d.>")
	for _, envelope := range []contracts.Envelope{
		{EventID: "artifact-1", Topic: "notifications.dispatch.document.T.d.artifact.version"},
		{EventID: "artifact-2", Topic: "notifications.dispatch.document.T.d.artifact.version"},
		{EventID: "issue-1", Topic: "notifications.dispatch.issue.T-1.ask.opened"},
	} {
		if err := publisher.Publish(envelope); err != nil {
			t.Fatalf("publish %q: %v", envelope.EventID, err)
		}
	}

	if err := <-result; err != nil {
		t.Fatalf("tail: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("tail output lines = %d, want 2: %q", len(lines), output.String())
	}
	for _, line := range lines {
		var envelope contracts.Envelope
		if err := json.Unmarshal([]byte(line), &envelope); err != nil {
			t.Fatalf("decode output envelope %q: %v", line, err)
		}
		if !strings.HasSuffix(envelope.Topic, ".artifact.version") {
			t.Fatalf("tail topic = %q, want document artifact version", envelope.Topic)
		}
	}
}

// The listener's connection probe (cmd/listener/connection_probe.go) is a header-only core
// request on notifications.envoy.deliver.<session>, so an operator's natstail on notifications.>
// meets one at every send to a session with consumer delivery. The probe's own comment names
// exactly that tail as a subscriber that stays silent: it must pass the probe over and go on
// tailing, rather than end on a message that carries no envelope.
func TestTailPassesOverTheListenersConnectionProbe(t *testing.T) {
	server, url := testnats.Start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	publisher, err := bus.ConnectOwningStream([]string{url})
	if err != nil {
		t.Fatalf("connect publisher: %v", err)
	}
	t.Cleanup(publisher.Close)

	var output bytes.Buffer
	result := make(chan error, 1)
	go func() {
		result <- tail(ctx, []string{url}, "notifications.>", 1, &output)
	}()
	waitForNATSSubscription(t, ctx, server, "notifications.>")

	probe := natsgo.NewMsg(bus.DeliverSubject("ses_probed"))
	probe.Header.Set(bus.ConnectionProbeHeader, "1")
	if err := publisher.Conn.PublishMsg(probe); err != nil {
		t.Fatalf("publish the probe: %v", err)
	}
	if err := publisher.Publish(contracts.Envelope{EventID: "after-probe", Topic: "notifications.team.tail"}); err != nil {
		t.Fatalf("publish the envelope: %v", err)
	}

	if err := <-result; err != nil {
		t.Fatalf("tail ended on the listener's connection probe: %v", err)
	}
	var envelope contracts.Envelope
	if err := json.Unmarshal(bytes.TrimSpace(output.Bytes()), &envelope); err != nil {
		t.Fatalf("decode output envelope %q: %v", output.String(), err)
	}
	if envelope.EventID != "after-probe" {
		t.Fatalf("tail printed %q, want the envelope published after the probe", envelope.EventID)
	}
}

func waitForNATSSubscription(t *testing.T, ctx context.Context, server *testnats.Server, subject string) {
	t.Helper()
	endpoint := server.MonitorURL() + "/subsz?subs=1"
	deadline := time.Now().Add(5 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			t.Fatalf("create NATS subscription request: %v", err)
		}
		response, err := http.DefaultClient.Do(request)
		if err == nil {
			var status struct {
				Subscriptions []struct {
					Subject string `json:"subject"`
				} `json:"subscriptions_list"`
			}
			err = json.NewDecoder(response.Body).Decode(&status)
			_ = response.Body.Close()
			if err == nil {
				for _, subscription := range status.Subscriptions {
					if subscription.Subject == subject {
						return
					}
				}
			}
		}
		lastErr = err
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("NATS never registered subscription %q at %s: %v", subject, endpoint, lastErr)
}
