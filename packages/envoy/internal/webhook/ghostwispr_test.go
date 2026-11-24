package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/sjawhar/envoy/internal/bus"
	"net/http/httptest"
	"strings"
	"testing"
)

func ghostWisprSign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func TestGhostWisprHandler(t *testing.T) {
	validBody := `{"event_type":"summary_ready","payload":{"session_id":"20260326041629","type":"summary_ready"}}`

	cases := []struct {
		name          string
		secret        string
		method        string
		body          string
		delivery      string
		event         string
		signature     string
		publishErr    error
		wantStatus    int
		wantPublished int
	}{
		{
			name:       "non-POST returns 200",
			method:     "GET",
			wantStatus: 200,
		},
		{
			name:       "missing delivery header",
			method:     "POST",
			body:       validBody,
			event:      "summary_ready",
			wantStatus: 400,
		},
		{
			name:       "missing event header",
			method:     "POST",
			body:       validBody,
			delivery:   "d-123",
			wantStatus: 400,
		},
		{
			name:       "invalid signature",
			secret:     "test-secret",
			method:     "POST",
			body:       validBody,
			delivery:   "d-123",
			event:      "summary_ready",
			signature:  "sha256=invalid",
			wantStatus: 401,
		},
		{
			name:          "valid summary_ready with signing",
			secret:        "test-secret",
			method:        "POST",
			body:          validBody,
			delivery:      "d-123",
			event:         "summary_ready",
			wantStatus:    200,
			wantPublished: 1,
		},
		{
			name:       "an event with no secret configured → 401",
			method:     "POST",
			body:       `{"event_type":"session_ended","payload":{"session_id":"20260326041629","type":"session_ended"}}`,
			delivery:   "d-456",
			event:      "session_ended",
			wantStatus: 401,
		},
		{
			name:       "non-publishable event type",
			secret:     "test-secret",
			method:     "POST",
			body:       `{"event_type":"recording_started","payload":{"session_id":"abc","type":"recording_started"}}`,
			delivery:   "d-789",
			event:      "recording_started",
			wantStatus: 200,
		},
		{
			name:       "missing session_id",
			secret:     "test-secret",
			method:     "POST",
			body:       `{"event_type":"summary_ready","payload":{"type":"summary_ready"}}`,
			delivery:   "d-000",
			event:      "summary_ready",
			wantStatus: 400,
		},
		{
			name:       "event type mismatch (header vs body)",
			secret:     "test-secret",
			method:     "POST",
			body:       `{"event_type":"session_started","payload":{"session_id":"abc","type":"session_started"}}`,
			delivery:   "d-mismatch",
			event:      "summary_ready",
			wantStatus: 400,
		},
		{
			name:          "publish failure returns 503",
			secret:        "test-secret",
			method:        "POST",
			body:          validBody,
			delivery:      "d-fail",
			event:         "summary_ready",
			publishErr:    fmt.Errorf("nats unavailable"),
			wantStatus:    503,
			wantPublished: 1,
		},
		{
			name:          "a publish refused as too large returns 422",
			secret:        "test-secret",
			method:        "POST",
			body:          validBody,
			delivery:      "d-too-large",
			event:         "summary_ready",
			publishErr:    fmt.Errorf("publish: %w", bus.ErrTooLarge),
			wantStatus:    422,
			wantPublished: 1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pub := &mockPublisher{err: tc.publishErr}
			handler := GhostWisprHandler(tc.secret, pub, NewBodyBudget(DefaultBodyBudget, nil))

			body := []byte(tc.body)
			req := httptest.NewRequest(tc.method, "/webhook/ghostwispr", strings.NewReader(tc.body))

			if tc.delivery != "" {
				req.Header.Set("X-GhostWispr-Delivery", tc.delivery)
			}
			if tc.event != "" {
				req.Header.Set("X-GhostWispr-Event", tc.event)
			}
			sig := tc.signature
			if sig == "" && tc.secret != "" {
				sig = ghostWisprSign(tc.secret, body)
			}
			if sig != "" {
				req.Header.Set("X-GhostWispr-Signature", sig)
			}

			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d; body = %s", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if len(pub.published) != tc.wantPublished {
				t.Errorf("published = %d, want %d", len(pub.published), tc.wantPublished)
			}
		})
	}
}

// TestGhostWisprHandlerRefusesAnEventItCannotCheck drives the handler the /webhook/ghostwispr route
// serves. Built with no secret it has nothing to check a signature against, so it answers 401 and
// publishes nothing, signed or not; built with a secret it publishes only an event that secret
// signed.
func TestGhostWisprHandlerRefusesAnEventItCannotCheck(t *testing.T) {
	const body = `{"event_type":"session_ended","payload":{"session_id":"20260326041629","type":"session_ended"}}`
	for _, tc := range []struct {
		name, secret, signature   string
		wantStatus, wantPublished int
	}{
		{name: "no secret, an unsigned event", wantStatus: 401},
		{name: "no secret, an event signed with the empty key", signature: ghostWisprSign("", []byte(body)), wantStatus: 401},
		{name: "control: a secret and a bad signature", secret: "s3cret", signature: "sha256=00", wantStatus: 401},
		{name: "control: a secret and no signature", secret: "s3cret", wantStatus: 401},
		{name: "control: a secret and a good signature", secret: "s3cret", signature: ghostWisprSign("s3cret", []byte(body)), wantStatus: 200, wantPublished: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pub := &mockPublisher{}
			req := httptest.NewRequest("POST", "/webhook/ghostwispr", strings.NewReader(body))
			req.Header.Set("X-GhostWispr-Delivery", "d-check")
			req.Header.Set("X-GhostWispr-Event", "session_ended")
			if tc.signature != "" {
				req.Header.Set("X-GhostWispr-Signature", tc.signature)
			}
			rec := httptest.NewRecorder()
			GhostWisprHandler(tc.secret, pub, NewBodyBudget(DefaultBodyBudget, nil)).ServeHTTP(rec, req)
			t.Logf("secret set %v, signature %q: status %d, body %q, published %d", tc.secret != "", tc.signature, rec.Code, strings.TrimSpace(rec.Body.String()), len(pub.published))
			if rec.Code != tc.wantStatus || len(pub.published) != tc.wantPublished {
				t.Errorf("status %d, published %d; want status %d, published %d", rec.Code, len(pub.published), tc.wantStatus, tc.wantPublished)
			}
		})
	}
}

// A unit sends no title or summary text, because the stream keeps an envelope for its whole
// retention window and no deletion on the unit can recall it. It sends when the session ran and
// where its current text is, and each event type's envelope carries those fields. An older unit
// still sends its title and summary, and they still pass beside the new fields.
func TestGhostWisprHandlerPassesSessionFields(t *testing.T) {
	const secret = "test-secret"
	const sessionFields = `"session_id":"20261115090000",` +
		`"started_at":"2026-11-15T09:00:00Z","ended_at":"2026-11-15T09:42:10Z",` +
		`"drive_folder_url":"https://drive.google.com/drive/folders/example-folder-id",` +
		`"session_url":"https://ghost-wispr.example-tailnet.ts.net/api/sessions/20261115090000"`
	wantSession := map[string]string{
		"session_id":       "20261115090000",
		"started_at":       "2026-11-15T09:00:00Z",
		"ended_at":         "2026-11-15T09:42:10Z",
		"drive_folder_url": "https://drive.google.com/drive/folders/example-folder-id",
		"session_url":      "https://ghost-wispr.example-tailnet.ts.net/api/sessions/20261115090000",
	}
	for _, tc := range []struct {
		name, event, payload string
		want                 map[string]string
		omitted              []string
	}{
		{
			name:    "session_started",
			event:   "session_started",
			payload: sessionFields + `,"type":"session_started","duration":0`,
			omitted: []string{"title", "summary"},
		},
		{
			name:    "session_ended",
			event:   "session_ended",
			payload: sessionFields + `,"type":"session_ended","duration":2530`,
			want:    map[string]string{"duration": "2530"},
			omitted: []string{"title", "summary"},
		},
		{
			name:  "summary_ready",
			event: "summary_ready",
			payload: sessionFields + `,"type":"summary_ready","version":3,"duration":2530,` +
				`"summary_preset":"default","summary_model":"example-model"`,
			want: map[string]string{
				"version": "3", "payload_type": "summary_ready",
				"summary_preset": "default", "summary_model": "example-model",
			},
			omitted: []string{"title", "summary"},
		},
		{
			name:  "an older unit's summary_ready keeps its title and summary",
			event: "summary_ready",
			payload: sessionFields + `,"type":"summary_ready","version":2,` +
				`"title":"Budget review","summary":"The team agreed the budget."`,
			want: map[string]string{"title": "Budget review", "summary": "The team agreed the budget."},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"event_type":"` + tc.event + `","created_at":"2026-11-15T09:42:11Z","payload":{` + tc.payload + `}}`
			req := httptest.NewRequest("POST", "/webhook/ghostwispr", strings.NewReader(body))
			req.Header.Set("X-GhostWispr-Delivery", "20261115090000."+tc.event)
			req.Header.Set("X-GhostWispr-Event", strings.ReplaceAll(tc.event, "_", "."))
			req.Header.Set("X-GhostWispr-Signature", ghostWisprSign(secret, []byte(body)))
			pub := &mockPublisher{}
			rec := httptest.NewRecorder()
			GhostWisprHandler(secret, pub, NewBodyBudget(DefaultBodyBudget, nil)).ServeHTTP(rec, req)
			if rec.Code != 200 || len(pub.published) != 1 {
				t.Fatalf("status = %d, published = %d; want 200 and 1; body = %s", rec.Code, len(pub.published), rec.Body.String())
			}
			var payload map[string]string
			if err := json.Unmarshal([]byte(pub.published[0].Payload), &payload); err != nil {
				t.Fatalf("envelope payload %q is not a JSON object of strings: %v", pub.published[0].Payload, err)
			}
			for _, want := range []map[string]string{wantSession, tc.want} {
				for key, value := range want {
					if payload[key] != value {
						t.Errorf("payload[%q] = %q, want %q; payload = %s", key, payload[key], value, pub.published[0].Payload)
					}
				}
			}
			for _, key := range tc.omitted {
				if value, found := payload[key]; found {
					t.Errorf("payload carries %q = %q, which the body did not send", key, value)
				}
			}
		})
	}
}
