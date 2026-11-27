package webhook

import (
	"bytes"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/bus/bustest"
	"github.com/sjawhar/envoy/internal/testnats"
)

// A GitHub delivery whose topic this build stores, while the deployed stream does not capture it
// (a hub move's quiesce narrowed the stream, and the listener's client read that), is answered
// 503, which tells GitHub the delivery failed (GitHub does not deliver it again on its own; Dispatch's
// sweep delivers its own App's deliveries again), and logged as the failed publish it is, which the
// publish-failure alarm counts; nothing is stored, and nothing went out over core NATS. Before the
// refusal the publish went out over core NATS, nothing stored it, and the delivery was answered
// 2xx, which GitHub takes as delivered.
func TestAGitHubDeliveryWhileTheDeployedStreamLacksItsSubjectIsAnswered503(t *testing.T) {
	url := testnats.URL(t)
	client, err := bus.ConnectOwningStream([]string{url}, bus.WithReplicas(1), bus.WithStreamSubjectsRefresh(100*time.Millisecond))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	raw := testnats.Connect(t, url)
	t.Cleanup(raw.Close)
	js, err := raw.JetStream()
	if err != nil {
		t.Fatalf("open JetStream: %v", err)
	}
	const topic = "notifications.github.sjawhar.legion.push"
	bustest.NarrowStream(t, js, nil)
	bustest.AwaitDeployedCapture(t, client, topic, false, 15*time.Second)
	before, err := js.StreamInfo(bus.Stream)
	if err != nil {
		t.Fatalf("read the narrowed stream: %v", err)
	}

	var logged bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(previous) })
	push := []byte(`{"ref":"refs/heads/main","repository":{"name":"legion","owner":{"login":"sjawhar"},"full_name":"sjawhar/legion"}}`)
	req := httptest.NewRequest(http.MethodPost, "/webhook/github", bytes.NewReader(push))
	req.Header.Set("X-GitHub-Delivery", "d-not-deployed")
	req.Header.Set("X-GitHub-Event", "push")
	req.Header.Set("X-Hub-Signature-256", githubSign("s", push))
	rec := httptest.NewRecorder()
	GitHubHandler("s", "@legion", "", "", client, &mockRecorder{}, NewBodyBudget(DefaultBodyBudget, nil)).ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body = %s", rec.Code, rec.Body.String())
	}
	var failed []string
	for _, line := range strings.Split(strings.TrimSpace(logged.String()), "\n") {
		if strings.Contains(line, "github publish failed:") {
			failed = append(failed, line)
		}
	}
	if len(failed) != 1 || !strings.Contains(failed[0], bus.ErrSubjectNotDeployed.Error()) || !strings.Contains(failed[0], topic) {
		t.Fatalf("log = %q, want one github publish failed line naming %q and %s", logged.String(), bus.ErrSubjectNotDeployed, topic)
	}
	after, err := js.StreamInfo(bus.Stream)
	if err != nil {
		t.Fatalf("read the stream after the delivery: %v", err)
	}
	if after.State.LastSeq != before.State.LastSeq || after.State.Msgs != before.State.Msgs {
		t.Fatalf("stream state after the delivery = %+v, want the state before it, %+v", after.State, before.State)
	}
}
