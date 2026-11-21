package webhook

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/id"
	"github.com/sjawhar/envoy/internal/verify"
)

// ghostWisprSkip returns true for event types that should be accepted but not published.
func ghostWisprSkip(event string) bool {
	switch normalizeGhostWisprEventType(event) {
	case "session_started", "session_ended", "summary_ready":
		return false
	}
	return true
}

// ghostWisprMaxBody is the largest Ghost Wispr event body the handler reads: 1 MiB, the limit the
// route has always had. An event names a session, its times and the links to its documents
// (ghostWisprPayload), and a current unit sends no title or summary text, so a real body holds some
// short fields. Only an older unit adds one meeting's summary.
const ghostWisprMaxBody = 1 << 20

// GhostWisprHandler returns the HTTP handler for Ghost Wispr webhook events. Its publish runs under
// the request's own context, so it ends when the sender gives up on the request, and at the bus's
// own 5 s bound otherwise (bus.Client.PublishWithin), inside the 10 s the listener's server gives a
// route to answer: Ghost Wispr sets no answer deadline of its own. It checks the signature of every
// event with secret and answers 401 to one that does not check, so a handler built with an empty
// secret refuses every event (verify.GhostWispr).
func GhostWisprHandler(secret string, publisher Publisher) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusOK)
			return
		}
		body, ok := readBody(w, r, ghostWisprMaxBody)
		if !ok {
			return
		}
		delivery := strings.TrimSpace(r.Header.Get("X-GhostWispr-Delivery"))
		event := normalizeGhostWisprEventType(r.Header.Get("X-GhostWispr-Event"))
		signature := strings.TrimSpace(r.Header.Get("X-GhostWispr-Signature"))
		if delivery == "" || event == "" {
			http.Error(w, "missing ghostwispr headers", http.StatusBadRequest)
			return
		}
		if !verify.GhostWispr(secret, body, signature) {
			http.Error(w, "invalid signature", http.StatusUnauthorized)
			return
		}
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil || payload == nil {
			http.Error(w, "invalid json", http.StatusBadRequest)
			return
		}
		bodyEvent := normalizeGhostWisprEventType(ghostWisprString(payload["event_type"]))
		if bodyEvent != "" && bodyEvent != event {
			http.Error(w, "event mismatch", http.StatusBadRequest)
			return
		}
		if ghostWisprSkip(event) {
			log.Printf("ghostwispr skipped unsupported event delivery=%s event=%s", delivery, event)
			writeOK(w)
			return
		}
		sessionID := ghostWisprPayloadSessionID(payload)
		if sessionID == "" {
			http.Error(w, "missing session_id", http.StatusBadRequest)
			return
		}
		payloadType := normalizeGhostWisprEventType(ghostWisprPayloadType(payload))
		if payloadType != "" && payloadType != event {
			http.Error(w, "event mismatch", http.StatusBadRequest)
			return
		}
		item := contracts.GhostWisprEnvelope(contracts.GhostWisprEnvelopeInput{
			EventType: event,
			Delivery:  delivery,
			Body:      payload,
			EventID:   id.New(),
			TraceID:   id.New(),
		})
		if err := item.Validate(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := publisher.PublishWithin(r.Context(), item); err != nil {
			deliveryFailed(w, "ghostwispr publish", err)
			return
		}
		writeOK(w)
	}
}

func writeOK(w http.ResponseWriter) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func ghostWisprPayloadSessionID(body map[string]any) string {
	payload := ghostWisprMap(body["payload"])
	return ghostWisprString(payload["session_id"])
}

func ghostWisprPayloadType(body map[string]any) string {
	payload := ghostWisprMap(body["payload"])
	return ghostWisprString(payload["type"])
}

func ghostWisprMap(value any) map[string]any {
	payload, _ := value.(map[string]any)
	return payload
}

func ghostWisprString(value any) string {
	switch text := value.(type) {
	case string:
		return strings.TrimSpace(text)
	default:
		return ""
	}
}

func normalizeGhostWisprEventType(event string) string {
	event = strings.TrimSpace(strings.ToLower(event))
	return strings.ReplaceAll(event, ".", "_")
}
