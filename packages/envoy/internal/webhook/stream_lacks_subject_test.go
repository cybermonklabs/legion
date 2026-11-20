package webhook

import (
	"bytes"
	"log"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/id"
	"github.com/sjawhar/envoy/internal/testnats"
)

// A delivery whose publish the notifications stream did not take because, as the listener read
// it, the deployed stream captures no subject of the envelope's (bus.ErrStreamLacksSubject) is
// answered at once with a 503, which the sender delivers again, and its answer is that refusal's
// own words, naming the subject and the stream's generation, where a 503 of any other failure
// says only that the service is unavailable. It is logged as the failed publish it is, which the
// publish-failure alarm counts, and the stream stores nothing. One row per webhook route, each
// over a newer build's stream that dropped that route's subject family.
func TestAWebhookDeliveryTheStreamLacksTheSubjectOfIsAnswered503NamingIt(t *testing.T) {
	for _, route := range []struct {
		name    string
		family  string
		action  string
		request func(t *testing.T, client *bus.Client) (http.Handler, *http.Request)
	}{
		{"GitHub", "notifications.github.>", "github publish", func(t *testing.T, client *bus.Client) (http.Handler, *http.Request) {
			push := []byte(`{"ref":"refs/heads/main","repository":{"name":"legion","owner":{"login":"sjawhar"},"full_name":"sjawhar/legion"}}`)
			req := httptest.NewRequest(http.MethodPost, "/webhook/github", bytes.NewReader(push))
			req.Header.Set("X-GitHub-Delivery", "d-lacks-"+id.New())
			req.Header.Set("X-GitHub-Event", "push")
			req.Header.Set("X-Hub-Signature-256", githubSign("s", push))
			return GitHubHandler("s", "@legion", "", client, &mockRecorder{}), req
		}},
		{"Slack", "notifications.slack.>", "slack publish", func(t *testing.T, client *bus.Client) (http.Handler, *http.Request) {
			body := []byte(`{"type":"event_callback","event_id":"ev-lacks-` + id.New() + `","team_id":"T01234567","event":{"type":"message","channel":"C01234567","user":"U01234567","text":"hello"}}`)
			req := httptest.NewRequest(http.MethodPost, "/webhook/slack", bytes.NewReader(body))
			timestamp := strconv.FormatInt(time.Now().Unix(), 10)
			req.Header.Set("X-Slack-Request-Timestamp", timestamp)
			req.Header.Set("X-Slack-Signature", slackSign("s", timestamp, body))
			return SlackHandler("s", client), req
		}},
		{"Ghost Wispr", "notifications.ghostwispr.>", "ghostwispr publish", func(t *testing.T, client *bus.Client) (http.Handler, *http.Request) {
			body := []byte(`{"event_type":"summary_ready","payload":{"session_id":"20260326041629","type":"summary_ready"}}`)
			req := httptest.NewRequest(http.MethodPost, "/webhook/ghostwispr", bytes.NewReader(body))
			req.Header.Set("X-GhostWispr-Delivery", "d-lacks-"+id.New())
			req.Header.Set("X-GhostWispr-Event", "summary_ready")
			req.Header.Set("X-GhostWispr-Signature", ghostWisprSign("s", body))
			return GhostWisprHandler("s", client), req
		}},
	} {
		t.Run(route.name, func(t *testing.T) {
			url := testnats.URL(t)
			var subjects []string
			for _, carried := range bus.StreamCarriedSubjects() {
				if carried != route.family {
					subjects = append(subjects, carried)
				}
			}
			if len(subjects) == len(bus.StreamCarriedSubjects()) {
				t.Fatalf("this build carries no %s to drop", route.family)
			}
			raw := testnats.Connect(t, url)
			t.Cleanup(raw.Close)
			js, err := raw.JetStream()
			if err != nil {
				t.Fatalf("open JetStream: %v", err)
			}
			if _, err := js.AddStream(&natsgo.StreamConfig{
				Name: bus.Stream, Subjects: subjects, Retention: natsgo.LimitsPolicy, Discard: natsgo.DiscardNew, Storage: natsgo.FileStorage,
				Metadata: map[string]string{bus.StreamIDMetadataKey: id.New(), "dispatch.stream-generation": "1000000"},
			}); err != nil {
				t.Fatalf("the newer build creates %s without %s: %v", bus.Stream, route.family, err)
			}
			client, err := bus.ConnectOwningStream([]string{url}, bus.WithReplicas(1))
			if err != nil {
				t.Fatalf("connect: %v", err)
			}
			t.Cleanup(client.Close)

			var logged bytes.Buffer
			previous := log.Writer()
			log.SetOutput(&logged)
			t.Cleanup(func() { log.SetOutput(previous) })
			handler, req := route.request(t, client)
			rec := httptest.NewRecorder()
			started := time.Now()
			handler.ServeHTTP(rec, req)
			elapsed := time.Since(started)
			answer := strings.TrimSpace(rec.Body.String())
			t.Logf("status %d after %s, answer %q, log %q", rec.Code, elapsed, answer, logged.String())

			if rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503; body = %s", rec.Code, answer)
			}
			family := strings.TrimSuffix(route.family, ">")
			if !strings.HasPrefix(answer, bus.ErrStreamLacksSubject.Error()+": ") || !strings.Contains(answer, `"`+family) || !strings.Contains(answer, "generation 1000000") {
				t.Errorf("the answer reads %q, want the refusal of a stream that does not capture the subject, naming a subject under %s and generation 1000000", answer, family)
			}
			if got := logged.String(); !strings.Contains(got, route.action+" failed: ") || !strings.Contains(got, bus.ErrStreamLacksSubject.Error()) {
				t.Errorf("log = %q, want a %q line naming %q", got, route.action+" failed", bus.ErrStreamLacksSubject)
			}
			if elapsed >= 5*time.Second {
				t.Errorf("the delivery was answered after %s, the bus's whole publish bound", elapsed)
			}
			info, err := js.StreamInfo(bus.Stream)
			if err != nil {
				t.Fatalf("read the stream: %v", err)
			}
			if info.State.Msgs != 0 {
				t.Errorf("the stream holds %d messages, want none", info.State.Msgs)
			}
		})
	}
}
