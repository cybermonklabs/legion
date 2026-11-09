package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// LEGION-271, acceptance 5. A comment mention's key was attempt-scoped exactly like a targeted
// message's, and its claim path resumes only a pending row, so a mention whose send landed and
// then missed the listener client's window was recorded failed and its retry reached the agent a
// second time. The key is now scoped to the mention's target and mode, so the stream recognises
// the repeat and the retry's row records it as a duplicate rather than a fresh delivery. A missed
// receipt is tried again inside the listener take-over window (takeoverRetry) under the same key,
// so the attempt is recorded failed only when every try of that window missed; this listener
// answers none of them until the comment's own request has answered.
func TestCommentMentionThatLandedThenMissedTheReceiptIsNotDeliveredTwiceOnRetry(t *testing.T) {
	t.Parallel()
	var listenerState struct {
		sync.Mutex
		keys      []string
		delivered map[string]struct{}
	}
	listenerState.delivered = map[string]struct{}{}
	// answering is closed when the comment's own request has answered. Until then this listener
	// takes every send and answers none of them, so a try misses the client's window because its
	// answer never comes at all, whatever the machine does with the goroutines meanwhile. A sleep
	// longer than that window ordered the two by wall clock instead, and a runner stalled for
	// longer than the sleep left the client's expiry and the listener's answer both due at the
	// same moment: the try that was answered first inside the window recorded the attempt sent,
	// where this test wants every try of the first request to miss (CI job 128890477766 of this
	// branch read sent at the first assertion below). released frees a send this listener is
	// still holding when the test ends before it answered anything, so Close waits on no request.
	answering, released := make(chan struct{}), make(chan struct{})
	const sessions = `[{"session_id":"s1","title":"planner","capabilities":["steer"]}]`
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/registry/s1":
			writeSessionRecord(t, w, sessions, "s1")
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sessions":
			_, _ = w.Write([]byte(sessions))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/send":
			var request struct {
				TargetSession  string `json:"target_session"`
				IdempotencyKey string `json:"idempotency_key"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Errorf("decode mention send: %v", err)
			}
			listenerState.Lock()
			listenerState.keys = append(listenerState.keys, request.IdempotencyKey)
			key := request.IdempotencyKey + "@" + request.TargetSession
			_, held := listenerState.delivered[key]
			listenerState.delivered[key] = struct{}{}
			listenerState.Unlock()
			select {
			case <-answering:
			case <-released:
				// The send landed; the answer never came, which is the whole of this test.
				return
			}
			if held {
				_, _ = w.Write([]byte(`{"event_id":"mention-envelope-2","recipient":"s1","duplicate":true}`))
				return
			}
			_, _ = w.Write([]byte(`{"event_id":"mention-envelope","recipient":"s1"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer listener.Close()
	// Runs before the Close above: a send this listener holds is let go whatever the test did.
	defer close(released)
	// envoyTimeout is the listener client's window: what ends a try this listener answers nothing
	// to, and what every call it does answer - each resolution, and the retry's send - has to
	// finish inside. While a missed try was a sleep waited out, the window had to be the shorter
	// of that pair, and 150 ms was all any answered call got; now that nothing is answered until
	// the request has, the window is free to be the generous one a loaded machine needs, and it
	// costs this test only the length of the tries nobody answers. takeoverWindow is this test's
	// own for the same reason: testTakeoverWindow's 750 ms is shorter than one try here, and this
	// one leaves room for the second try the key is proved stable across (the window plus
	// takeoverFirstBackoff's 250 ms).
	handler, database, _ := newTestServer(t, testServerOptions{
		envoyURL: listener.URL, envoyTimeout: 1500 * time.Millisecond, takeoverWindow: 2 * time.Second,
	})
	issue := createInteractionIssue(t, handler, "TEST", "A mention that landed then timed out", "before")
	comment := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{
		"body": "The mention landed; the answer did not.", "mentions": []map[string]any{{"target": "session:s1"}},
	}))
	if len(comment.Deliveries) != 1 || comment.Deliveries[0].State != "failed" {
		t.Fatalf("first attempt = %#v, want it recorded failed once every try missed its receipt", comment.Deliveries)
	}
	close(answering)

	retry := dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+comment.ID+"/deliveries", map[string]any{
		"delivery": "steer",
	}, "alice")
	if retry.Code != http.StatusCreated {
		t.Fatalf("retry the failed mention: status=%d body=%s", retry.Code, retry.Body.String())
	}

	listenerState.Lock()
	keys := append([]string(nil), listenerState.keys...)
	delivered := len(listenerState.delivered)
	listenerState.Unlock()
	original := comment.ID + ":session:s1:steer"
	if len(keys) < 2 || slices.ContainsFunc(keys, func(key string) bool { return key != original }) {
		t.Fatalf("mention send keys = %#v, want every send under the target-and-mode key %q", keys, original)
	}
	if delivered != 1 {
		t.Fatalf("the listener delivered %d distinct keys %#v, want the agent to receive the mention once", delivered, keys)
	}

	var state, failure string
	var duplicate bool
	if err := database.Pool.QueryRow(context.Background(), `
		select state, duplicate, coalesce(error, '') from comment_deliveries
		where comment_id = $1 and target = 'session:s1' order by attempt desc limit 1
	`, comment.ID).Scan(&state, &duplicate, &failure); err != nil {
		t.Fatalf("read the retried mention attempt: %v", err)
	}
	if state != "sent" || !duplicate {
		// The row's own error, so a failure here says what the send reported rather than only
		// that it was not what this test wants.
		t.Fatalf("retried mention attempt = %s/duplicate=%v/error=%q, want sent and duplicate", state, duplicate, failure)
	}
}

// LEGION-271, acceptance 7's other half. A timeout resolving the target sent nothing, so its
// text must not tell a human the message may already have been delivered; only the send half
// carries that promise. deliveryErrorText is shared between the two, which is why this is its
// own test rather than a line in the one above.
func TestResolutionTimeoutDoesNotClaimTheMessageMayHaveLanded(t *testing.T) {
	t.Parallel()
	held := make(chan struct{})
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && (r.URL.Path == "/v1/registry/s1" || r.URL.Path == "/v1/sessions") {
			<-held
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer func() {
		close(held)
		listener.Close()
	}()
	handler, _ := newTargetedMessageHandler(t, listener.URL, 150*time.Millisecond)
	issue := createInteractionIssue(t, handler, "TEST", "A resolution that timed out", "before")

	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/messages", map[string]any{
		"body": "The listener never answered the lookup.", "target": "session:s1", "delivery": "steer",
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("create targeted message: status=%d body=%s", created.Code, created.Body.String())
	}
	message := decodeBody[model.Message](t, created)
	if len(message.Deliveries) != 1 || message.Deliveries[0].State != "failed" {
		t.Fatalf("attempt = %#v, want one attempt recorded failed", message.Deliveries)
	}
	failure := ""
	if message.Deliveries[0].Error != nil {
		failure = *message.Deliveries[0].Error
	}
	if strings.Contains(failure, "may already have been delivered") {
		t.Fatalf("resolution-timeout error = %q, but nothing was sent", failure)
	}
	if failure == "" {
		t.Fatalf("resolution-timeout attempt recorded no error at all")
	}
}
