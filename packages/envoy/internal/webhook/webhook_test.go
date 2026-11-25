package webhook

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
)

// mockPublisher records published envelopes for test assertions. Requests the handler serves at
// once can publish at once, as they may with the real publisher.
type mockPublisher struct {
	mu        sync.Mutex
	published []contracts.Envelope
	err       error
}

func (m *mockPublisher) PublishWithin(_ context.Context, item contracts.Envelope) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.published = append(m.published, item)
	return m.err
}

// mockRecorder records CIRecorder calls for test assertions.
type mockRecorder struct {
	calls      []contracts.CIObservation
	suiteCalls []contracts.CIObservation
	err        error
	suiteErr   error
}

func (m *mockRecorder) Record(_ context.Context, observation contracts.CIObservation) error {
	m.calls = append(m.calls, observation)
	return m.err
}

func (m *mockRecorder) RecordSuite(_ context.Context, observation contracts.CIObservation) error {
	m.suiteCalls = append(m.suiteCalls, observation)
	return m.suiteErr
}

// filterDate and filterTime are the date and time fields the listener's webhook alarm filters
// anchor a line on (infra/aws/rendering/metricfilters.go).
var filterDate, filterTime = regexp.MustCompile(`^\d{4}/\d{2}/\d{2}$`), regexp.MustCompile(`^\d{2}:\d{2}:\d{2}$`)

// countedAsAFailedPublish reports whether line is one the listener's webhook-publish-failure-filter
// counts: `[date=%^\d{4}/\d{2}/\d{2}$%, time=%^\d{2}:\d{2}:\d{2}$%,
// source=github||source=slack||source=ghostwispr, verb=publish, result=failed*, ...]`, a
// space-delimited pattern over the line's words.
func countedAsAFailedPublish(line string) bool {
	words := strings.Fields(line)
	if len(words) < 5 || !filterDate.MatchString(words[0]) || !filterTime.MatchString(words[1]) {
		return false
	}
	switch words[2] {
	case "github", "slack", "ghostwispr":
	default:
		return false
	}
	return words[3] == "publish" && strings.HasPrefix(words[4], "failed")
}

// A webhook delivery whose publish a clustered stream refuses because an earlier publish under the
// same message id - an earlier delivery of the same event - is still in process (nats-server's
// 10158, which the bus names bus.ErrDuplicateInProcess beside the server's own error) is no failed
// publish: that publish stores the one copy. So no route logs the line the listener's
// webhook-publish-failure-filter counts for it, while a publish that failed still logs that line,
// and both are answered 503 as before. The publisher answers each route's publish with the error
// the bus returns for each case.
func TestAWebhookDeliveryStillInProcessIsNotLoggedAsAFailedPublish(t *testing.T) {
	var logged bytes.Buffer
	previous, flags := log.Writer(), log.Flags()
	log.SetOutput(&logged)
	log.SetFlags(log.LstdFlags)
	t.Cleanup(func() { log.SetOutput(previous); log.SetFlags(flags) })
	const secret = "s"
	push := []byte(`{"ref":"refs/heads/main","repository":{"name":"legion","owner":{"login":"sjawhar"},"full_name":"sjawhar/legion"}}`)
	slackEvent := []byte(`{"type":"event_callback","event_id":"ev-in-process","team_id":"T01234567",` +
		`"event":{"type":"message","channel":"C01234567","user":"U01234567","text":"hello world"}}`)
	summary := []byte(`{"event_type":"summary_ready","payload":{"session_id":"20260326041629","type":"summary_ready"}}`)
	routes := []struct {
		source  string
		handler func(Publisher) http.Handler
		request func() *http.Request
	}{
		{"github", func(p Publisher) http.Handler {
			return GitHubHandler(secret, "@legion", "", p, &mockRecorder{}, NewBodyBudget(DefaultBodyBudget, nil))
		}, func() *http.Request {
			req := httptest.NewRequest(http.MethodPost, "/webhook/github", bytes.NewReader(push))
			req.Header.Set("X-GitHub-Delivery", "d-in-process")
			req.Header.Set("X-GitHub-Event", "push")
			req.Header.Set("X-Hub-Signature-256", githubSign(secret, push))
			return req
		}},
		{"slack", func(p Publisher) http.Handler {
			return SlackHandler(secret, p, NewBodyBudget(DefaultBodyBudget, nil))
		}, func() *http.Request {
			req := httptest.NewRequest(http.MethodPost, "/webhook/slack", bytes.NewReader(slackEvent))
			ts := strconv.FormatInt(time.Now().Unix(), 10)
			req.Header.Set("X-Slack-Request-Timestamp", ts)
			req.Header.Set("X-Slack-Signature", slackSign(secret, ts, slackEvent))
			return req
		}},
		{"ghostwispr", func(p Publisher) http.Handler {
			return GhostWisprHandler(secret, p, NewBodyBudget(DefaultBodyBudget, nil))
		}, func() *http.Request {
			req := httptest.NewRequest(http.MethodPost, "/webhook/ghostwispr", bytes.NewReader(summary))
			req.Header.Set("X-GhostWispr-Delivery", "d-in-process")
			req.Header.Set("X-GhostWispr-Event", "summary_ready")
			req.Header.Set("X-GhostWispr-Signature", ghostWisprSign(secret, summary))
			return req
		}},
	}
	inProcess := fmt.Errorf("%w: %w", bus.ErrDuplicateInProcess,
		&nats.APIError{Code: 409, ErrorCode: 10158, Description: "duplicate message id is in process"})
	failed := errors.New("nats: no response from stream")
	for _, route := range routes {
		for _, row := range []struct {
			name    string
			err     error
			counted int
		}{
			{"still in process under an earlier publish", inProcess, 0},
			{"control: a publish that failed", failed, 1},
		} {
			t.Run(route.source+"/"+row.name, func(t *testing.T) {
				logged.Reset()
				rec := httptest.NewRecorder()
				route.handler(&mockPublisher{err: row.err}).ServeHTTP(rec, route.request())
				if rec.Code != http.StatusServiceUnavailable {
					t.Fatalf("answered %d %q, want 503", rec.Code, rec.Body.String())
				}
				var counted, naming []string
				for _, line := range strings.Split(strings.TrimSpace(logged.String()), "\n") {
					if countedAsAFailedPublish(line) {
						counted = append(counted, line)
					}
					if strings.Contains(line, row.err.Error()) {
						naming = append(naming, line)
					}
				}
				if len(naming) != 1 {
					t.Fatalf("logged %d lines naming %q, want 1:\n%s", len(naming), row.err, logged.String())
				}
				if len(counted) != row.counted {
					t.Fatalf("logged %d lines the webhook-publish-failure-filter counts, want %d:\n%s", len(counted), row.counted, logged.String())
				}
			})
		}
	}
}
