package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

type commentMentionRead struct {
	Target    string  `json:"target"`
	Delivery  string  `json:"delivery"`
	SessionID *string `json:"session_id"`
}

type commentDeliveryRead struct {
	CommentID string  `json:"comment_id"`
	Target    string  `json:"target"`
	Attempt   int     `json:"attempt"`
	Delivery  string  `json:"delivery"`
	SessionID *string `json:"session_id"`
	State     string  `json:"state"`
	Error     *string `json:"error"`
	// ResolveError is what the attempt's own resolution found. A create response carries it as
	// the creating transaction wrote it, so it says, with nothing racing it, whether the
	// listener answered that mention's lookup inside mentionLookupTimeout.
	ResolveError *string `json:"resolve_error"`
	ReplyID      *string `json:"reply_id"`
}

type mentionedCommentRead struct {
	ID         string                `json:"id"`
	Body       string                `json:"body"`
	ReplyTo    *string               `json:"reply_to"`
	Mentions   []commentMentionRead  `json:"mentions"`
	Deliveries []commentDeliveryRead `json:"deliveries"`
}

func postMentionedComment(t *testing.T, handler http.Handler, issueKey string, input map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	return dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issueKey+"/comments", input, "alice")
}

func decodeMentionedComment(t *testing.T, response *httptest.ResponseRecorder) mentionedCommentRead {
	t.Helper()
	if response.Code != http.StatusCreated {
		t.Fatalf("create mentioned comment: status=%d body=%s", response.Code, response.Body.String())
	}
	return decodeBody[mentionedCommentRead](t, response)
}

// settledCommentDeliveries waits until no attempt of the comment is pending - the mention sender
// settles each after the create has answered - and returns its attempts by target and attempt.
func settledCommentDeliveries(t *testing.T, database *store.Store, commentID string) []model.CommentDelivery {
	t.Helper()
	return waitForCommentDeliveries(t, database, commentID, "no attempt pending", func(attempts []model.CommentDelivery) bool {
		for _, attempt := range attempts {
			if attempt.State == "pending" {
				return false
			}
		}
		return len(attempts) > 0
	})
}

// aResolvedMentionCreation posts input until the comment it creates committed what its mention's
// lookup found - a pinned session, or the listener's refusal - on attempt 1.
//
// A comment's creation bounds every lookup it makes at mentionLookupTimeout (250 ms,
// comment_delivery.go) and is answered without one that missed it, committing that attempt with
// neither a session nor a resolve error; the sender of that mention then resolves the target
// again after the commit (mentionResolutionFor), which is a second lookup of the product's own
// making. A creation that lost its lookup to that bound is not what a case using this helper is
// about - each is about what a committed resolution does next - so that comment's deliveries are
// settled, again puts the listener back as the case found it, and the comment is posted once
// more. Five tries: the bound is missed about once in sixty on a loaded two-core runner, so five
// in a row is not a wait a person will ever see, and a case that spent them says so rather than
// asserting against a resolution that was never recorded.
func aResolvedMentionCreation(
	t *testing.T, handler http.Handler, database *store.Store, issueKey string,
	input map[string]any, again func(),
) mentionedCommentRead {
	t.Helper()
	for try := 1; try <= 5; try++ {
		created := decodeMentionedComment(t, postMentionedComment(t, handler, issueKey, input))
		if len(created.Deliveries) != 1 {
			t.Fatalf("creation %d = %#v, want the one mention's attempt", try, created)
		}
		if created.Deliveries[0].SessionID != nil || created.Deliveries[0].ResolveError != nil {
			return created
		}
		settledCommentDeliveries(t, database, created.ID)
		again()
	}
	t.Fatalf("five creations in a row gave their mention's lookup up at %v", mentionLookupTimeout)
	return mentionedCommentRead{}
}

// waitForCommentDeliveries reads the comment's attempts, by target and attempt, until done accepts
// them; want says what done waits for. Its patience covers a whole take-over window and ten
// seconds past it, because a send on the mention sender's lanes may wait one out after the
// comment's response - a mention of an ended session does, under a server running the production
// window (listenerTakeoverWindow) - before its attempt settles.
func waitForCommentDeliveries(
	t *testing.T, database *store.Store, commentID, want string, done func([]model.CommentDelivery) bool,
) []model.CommentDelivery {
	t.Helper()
	deadline := time.Now().Add(listenerTakeoverWindow + 10*time.Second)
	for {
		rows, err := database.Pool.Query(context.Background(), `
			select `+commentDeliveryColumns+` from comment_deliveries where comment_id = $1 order by target, attempt
		`, commentID)
		if err != nil {
			t.Fatalf("read comment %s attempts: %v", commentID, err)
		}
		attempts, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (model.CommentDelivery, error) {
			return scanCommentDelivery(row)
		})
		if err != nil {
			t.Fatalf("read comment %s attempts: %v", commentID, err)
		}
		if done(attempts) {
			return attempts
		}
		if time.Now().After(deadline) {
			t.Fatalf("comment %s attempts = %#v, want %s", commentID, attempts, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestMentionedCommentRecordsFailedOfflineSession(t *testing.T) {
	t.Parallel()
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/v1/sessions" {
			_, _ = w.Write([]byte(`[]`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer listener.Close()
	handler, database := newTargetedMessageHandler(t, listener.URL)
	issue := createInteractionIssue(t, handler, "TEST", "Offline mention", "before")

	created := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{
		"body": "Please investigate this.", "mentions": []map[string]any{{"target": "session:gone"}},
	}))
	if len(created.Mentions) != 1 || created.Mentions[0].SessionID != nil {
		t.Fatalf("offline mention = %#v, want an unresolved mention", created.Mentions)
	}
	if len(created.Deliveries) != 1 || created.Deliveries[0].State != "pending" {
		t.Fatalf("offline delivery in the create response = %#v, want its pending attempt", created.Deliveries)
	}
	if settled := settledCommentDeliveries(t, database, created.ID); len(settled) != 1 || settled[0].State != "failed" ||
		settled[0].SessionID != nil || settled[0].Error == nil || *settled[0].Error != "no live session gone" {
		t.Fatalf("offline delivery = %#v, want a failed delivery with no resolved session", settled)
	}
	events := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/events", nil, "alice")
	if events.Code != http.StatusOK || !strings.Contains(events.Body.String(), `"type":"comment.delivery"`) ||
		!strings.Contains(events.Body.String(), `"state":"failed"`) {
		t.Fatalf("offline mention events: status=%d body=%s", events.Code, events.Body.String())
	}
}

func TestMentionedCommentUsesRequestedOrDefaultDeliveryMode(t *testing.T) {
	t.Parallel()
	capabilities := []string{"btw", "steer"}
	sent := []map[string]any{}
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/registry/s1":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"session_id": "s1", "title": "planner", "capabilities": capabilities,
			})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sessions":
			_ = json.NewEncoder(w).Encode([]map[string]any{{
				"session_id": "s1", "title": "planner", "capabilities": capabilities,
			}})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/send":
			var input map[string]any
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				t.Fatalf("decode mention send: %v", err)
			}
			sent = append(sent, input)
			_, _ = w.Write([]byte(`{"event_id":"mention-envelope","recipient":"s1"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer listener.Close()
	handler, database := newTargetedMessageHandler(t, listener.URL)
	issue := createInteractionIssue(t, handler, "TEST", "Mention mode", "before")

	defaultMode := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{
		"body": "Default mode.", "mentions": []map[string]any{{"target": "session:s1"}},
	}))
	if len(defaultMode.Mentions) != 1 || defaultMode.Mentions[0].Delivery != "steer" {
		t.Fatalf("default mode result = %#v", defaultMode)
	}
	if settled := settledCommentDeliveries(t, database, defaultMode.ID); len(settled) != 1 ||
		settled[0].Delivery != "steer" || settled[0].State != "sent" {
		t.Fatalf("default mode delivery = %#v", settled)
	}
	btwMode := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{
		"body": "BTW mode.", "mentions": []map[string]any{{"target": "session:s1"}}, "delivery": "btw",
	}))
	if settled := settledCommentDeliveries(t, database, btwMode.ID); len(settled) != 1 ||
		settled[0].Delivery != "btw" || settled[0].State != "sent" {
		t.Fatalf("btw mode delivery = %#v", settled)
	}
	if len(sent) != 2 {
		t.Fatalf("mention sends = %#v, want default and btw sends", sent)
	}
	var frame struct {
		Delivery struct {
			Mode string `json:"mode"`
		} `json:"delivery"`
	}
	if err := json.Unmarshal([]byte(sent[1]["payload"].(string)), &frame); err != nil {
		t.Fatalf("decode BTW frame: %v", err)
	}
	if frame.Delivery.Mode != "btw" {
		t.Fatalf("BTW frame mode = %q, want btw", frame.Delivery.Mode)
	}
	capabilities = []string{"btw"}
	unadvertisedSteer := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{
		"body": "Unavailable steer mode.", "mentions": []map[string]any{{"target": "session:s1"}}, "delivery": "steer",
	}))
	if settled := settledCommentDeliveries(t, database, unadvertisedSteer.ID); len(settled) != 1 ||
		settled[0].State != "failed" || settled[0].Error == nil ||
		*settled[0].Error != "session s1 (planner) does not advertise steer" {
		t.Fatalf("unadvertised steer result = %#v", settled)
	}

	capabilities = nil
	unadvertised := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{
		"body": "Unavailable BTW mode.", "mentions": []map[string]any{{"target": "session:s1"}}, "delivery": "btw",
	}))
	if settled := settledCommentDeliveries(t, database, unadvertised.ID); len(settled) != 1 ||
		settled[0].State != "failed" || settled[0].Error == nil ||
		*settled[0].Error != "session s1 (planner) does not advertise btw" {
		t.Fatalf("unadvertised mode result = %#v", settled)
	}
}

func TestMentionedCommentRejectsInvalidMentionInput(t *testing.T) {
	t.Parallel()
	handler, _ := newTargetedMessageHandler(t, "")
	issue := createInteractionIssue(t, handler, "TEST", "Mention validation", "before")
	tooMany := make([]map[string]any, 11)
	for index := range tooMany {
		tooMany[index] = map[string]any{"target": fmt.Sprintf("session:s%d", index)}
	}
	for _, tc := range []struct {
		name  string
		input map[string]any
	}{
		{"invalid target", map[string]any{"body": "Bad target.", "mentions": []map[string]any{{"target": "agent:s1"}}}},
		{"duplicate target", map[string]any{"body": "Duplicate target.", "mentions": []map[string]any{{"target": "session:s1"}, {"target": "session:s1"}}}},
		{"more than ten targets", map[string]any{"body": "Too many targets.", "mentions": tooMany}},
		{"delivery without target", map[string]any{"body": "No target.", "delivery": "btw"}},
		{"invalid delivery", map[string]any{"body": "Bad mode.", "mentions": []map[string]any{{"target": "session:s1"}}, "delivery": "interrupt"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := postMentionedComment(t, handler, issue.Key, tc.input)
			if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"code":"MENTION_INPUT"`) {
				t.Fatalf("%s: status=%d body=%s", tc.name, response.Code, response.Body.String())
			}
		})
	}
}

func TestMentionedCommentPinsRoleHolderBeforeSending(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	roleHolder := "s1"
	roleLookups := 0
	sent := []map[string]any{}
	const sessions = `[{"session_id":"s1","title":"planner","capabilities":["btw","steer"]}]`
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/roles/reviewer":
			mu.Lock()
			roleLookups++
			holder := roleHolder
			roleHolder = "s2"
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{
				"role": "reviewer", "holder": holder, "title": "reviewer", "capabilities": []string{"btw", "steer"},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/registry/s1":
			writeSessionRecord(t, w, sessions, "s1")
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sessions":
			_, _ = w.Write([]byte(sessions))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/send":
			var input map[string]any
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				t.Fatalf("decode role mention send: %v", err)
			}
			mu.Lock()
			sent = append(sent, input)
			mu.Unlock()
			_, _ = w.Write([]byte(`{"event_id":"mention-envelope","recipient":"s1"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer listener.Close()
	handler, database := newTargetedMessageHandler(t, listener.URL)
	issue := createInteractionIssue(t, handler, "TEST", "Pinned mention", "before")
	patched := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]any{"route": "role:reviewer"}, "alice")
	if patched.Code != http.StatusOK {
		t.Fatalf("set route: status=%d body=%s", patched.Code, patched.Body.String())
	}

	created := aResolvedMentionCreation(t, handler, database, issue.Key, map[string]any{
		"body": "Review this exact version.", "mentions": []map[string]any{{"target": "role:reviewer"}},
	}, func() {
		mu.Lock()
		roleHolder = "s1"
		roleLookups = 0
		sent = nil
		mu.Unlock()
	})
	// The lookup behind this comment moved the role to s2 as it answered, and the attempt
	// committed with the holder it read. From here the role must not be asked about again: the
	// send is made to the session the attempt names, through a read of that session alone, so
	// the lookups counted from this point are the whole proof that no role transition redirected
	// the frame. They start at zero, because what the creation itself spent - one lookup, or two
	// when it gave the first up at mentionLookupTimeout - says nothing about this.
	mu.Lock()
	roleLookups = 0
	mu.Unlock()
	settledCommentDeliveries(t, database, created.ID)
	mu.Lock()
	holder, lookups, sends := roleHolder, roleLookups, slices.Clone(sent)
	mu.Unlock()
	if lookups != 0 || holder != "s2" || len(created.Mentions) != 1 || created.Mentions[0].SessionID == nil ||
		*created.Mentions[0].SessionID != "s1" || len(sends) != 1 || sends[0]["target_session"] != "s1" {
		t.Fatalf("role resolution/sends = holder:%s lookups after the creation:%d mentions:%#v sends:%#v, want one pinned s1 delivery after role moves to s2",
			holder, lookups, created.Mentions, sends)
	}
	events := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/events", nil, "alice")
	if events.Code != http.StatusOK || !strings.Contains(events.Body.String(), `"suppress_route":true`) ||
		!strings.Contains(events.Body.String(), `"suppressed_route":"role:reviewer"`) ||
		!strings.Contains(events.Body.String(), `"suppressed_route_session_id":"s1"`) {
		t.Fatalf("pinned mention payload: status=%d body=%s", events.Code, events.Body.String())
	}
}

func TestMentionReplyRequiresItsAttemptTargetAndIsIdempotent(t *testing.T) {
	t.Parallel()
	const sessions = `[{"session_id":"s1","title":"planner","capabilities":["btw","steer"]}]`
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/registry/s1":
			writeSessionRecord(t, w, sessions, "s1")
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sessions":
			_, _ = w.Write([]byte(sessions))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/send":
			_, _ = w.Write([]byte(`{"event_id":"mention-envelope","recipient":"s1"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer listener.Close()
	handler, database := newTargetedMessageHandler(t, listener.URL)
	issue := createInteractionIssue(t, handler, "TEST", "Mention reply", "before")
	comment := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{
		"body": "Can this ship?", "mentions": []map[string]any{{"target": "session:s1"}},
	}))
	settledCommentDeliveries(t, database, comment.ID)

	human := dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+comment.ID+"/reply", map[string]any{
		"actor": map[string]any{"kind": "session", "id": "s1"}, "attempt": 1, "body": "Human cannot answer.",
	}, "alice")
	if human.Code != http.StatusForbidden || !strings.Contains(human.Body.String(), `"code":"REPLY_FORBIDDEN"`) {
		t.Fatalf("human comment reply: status=%d body=%s", human.Code, human.Body.String())
	}
	wrongSession := bearerRequest(t, handler, http.MethodPost, "/api/v1/comments/"+comment.ID+"/reply", map[string]any{
		"actor": map[string]any{"kind": "session", "id": "s2"}, "attempt": 1, "body": "Wrong session.",
	})
	if wrongSession.Code != http.StatusForbidden || !strings.Contains(wrongSession.Body.String(), `"code":"REPLY_FORBIDDEN"`) {
		t.Fatalf("wrong-session comment reply: status=%d body=%s", wrongSession.Code, wrongSession.Body.String())
	}
	answered := bearerRequest(t, handler, http.MethodPost, "/api/v1/comments/"+comment.ID+"/reply", map[string]any{
		"actor": map[string]any{"kind": "session", "id": "s1"}, "attempt": 1, "body": "Yes, it can ship.",
	})
	if answered.Code != http.StatusCreated {
		t.Fatalf("answer mention: status=%d body=%s", answered.Code, answered.Body.String())
	}
	answer := decodeBody[mentionedCommentRead](t, answered)
	if answer.ReplyTo == nil || *answer.ReplyTo != comment.ID || answer.Body != "Yes, it can ship." {
		t.Fatalf("answered comment = %#v", answer)
	}
	repeated := bearerRequest(t, handler, http.MethodPost, "/api/v1/comments/"+comment.ID+"/reply", map[string]any{
		"actor": map[string]any{"kind": "session", "id": "s1"}, "attempt": 1, "body": "A duplicate answer.",
	})
	if repeated.Code != http.StatusOK || decodeBody[mentionedCommentRead](t, repeated).ID != answer.ID {
		t.Fatalf("repeated answer: status=%d body=%s", repeated.Code, repeated.Body.String())
	}
	retry := dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+comment.ID+"/deliveries", map[string]any{
		"delivery": "steer",
	}, "alice")
	if retry.Code != http.StatusCreated || decodeBody[commentDeliveryRead](t, retry).Attempt != 2 {
		t.Fatalf("create stale reply attempt: status=%d body=%s", retry.Code, retry.Body.String())
	}
	staleAnswer := bearerRequest(t, handler, http.MethodPost, "/api/v1/comments/"+comment.ID+"/reply", map[string]any{
		"actor": map[string]any{"kind": "session", "id": "s1"}, "attempt": 2, "body": "The stale attempt also arrived.",
	})
	if staleAnswer.Code != http.StatusCreated {
		t.Fatalf("answer stale mention attempt: status=%d body=%s", staleAnswer.Code, staleAnswer.Body.String())
	}
	if stale := decodeBody[mentionedCommentRead](t, staleAnswer); stale.ID == answer.ID || stale.ReplyTo == nil || *stale.ReplyTo != comment.ID {
		t.Fatalf("stale attempt answer = %#v, want its own reply to %s", stale, comment.ID)
	}
	events := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/events", nil, "alice")
	if events.Code != http.StatusOK || !strings.Contains(events.Body.String(), `"type":"comment.answered"`) {
		t.Fatalf("answer events: status=%d body=%s", events.Code, events.Body.String())
	}
}

func TestMentionReplyErrorRecordsAttemptWithoutComment(t *testing.T) {
	t.Parallel()
	live := true
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/v1/registry/s1" {
			if live {
				_, _ = w.Write([]byte(`{"session_id":"s1","title":"planner","capabilities":["steer"]}`))
			} else {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"error":"no live session s1"}`))
			}
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == "/v1/sessions" {
			if live {
				_, _ = w.Write([]byte(`[{"session_id":"s1","title":"planner","capabilities":["steer"]}]`))
			} else {
				_, _ = w.Write([]byte(`[]`))
			}
			return
		}
		if r.Method == http.MethodPost && r.URL.Path == "/v1/messages/send" {
			_, _ = w.Write([]byte(`{"event_id":"mention-envelope","recipient":"s1"}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer listener.Close()
	handler, database := newTargetedMessageHandler(t, listener.URL)
	issue := createInteractionIssue(t, handler, "TEST", "Mention error reply", "before")
	comment := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{
		"body": "Are you there?", "mentions": []map[string]any{{"target": "session:s1"}},
	}))
	settledCommentDeliveries(t, database, comment.ID)
	failure := bearerRequest(t, handler, http.MethodPost, "/api/v1/comments/"+comment.ID+"/reply", map[string]any{
		"actor": map[string]any{"kind": "session", "id": "s1"}, "attempt": 1, "error": "No active model.",
	})
	if failure.Code != http.StatusOK {
		t.Fatalf("report mention failure: status=%d body=%s", failure.Code, failure.Body.String())
	}
	attempt := decodeBody[commentDeliveryRead](t, failure)
	if attempt.State != "failed" || attempt.Error == nil || *attempt.Error != "No active model." || attempt.ReplyID != nil {
		t.Fatalf("failed answer attempt = %#v", attempt)
	}
	reloaded := dispatchRequest(t, handler, http.MethodGet, "/api/v1/comments/"+comment.ID, nil, "alice")
	if reloaded.Code != http.StatusOK {
		t.Fatalf("reload comment after failed answer: status=%d body=%s", reloaded.Code, reloaded.Body.String())
	}
	var thread struct {
		Comment mentionedCommentRead   `json:"comment"`
		Replies []mentionedCommentRead `json:"replies"`
	}
	if err := json.Unmarshal(reloaded.Body.Bytes(), &thread); err != nil {
		t.Fatalf("decode reloaded comment: %v", err)
	}
	if len(thread.Replies) != 0 || len(thread.Comment.Deliveries) != 1 || thread.Comment.Deliveries[0].Error == nil ||
		*thread.Comment.Deliveries[0].Error != "No active model." {
		t.Fatalf("failed answer readback = %#v", thread)
	}
}

func TestMentionRetrySelectsOneTargetAndReresolvesIt(t *testing.T) {
	t.Parallel()
	live := false
	roleHolder := "s1"
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/registry/"):
			session := strings.TrimPrefix(r.URL.Path, "/v1/registry/")
			if !live || (session != "s1" && session != "s2") {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"error":"no live session ` + session + `"}`))
				return
			}
			title := "planner"
			if session == "s2" {
				title = "reviewer"
			}
			_, _ = w.Write([]byte(`{"session_id":"` + session + `","title":"` + title + `","capabilities":["btw","steer"]}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sessions":
			if live {
				_, _ = w.Write([]byte(`[{"session_id":"s1","title":"planner","capabilities":["btw","steer"]},{"session_id":"s2","title":"reviewer","capabilities":["btw","steer"]}]`))
			} else {
				_, _ = w.Write([]byte(`[]`))
			}
		case r.Method == http.MethodGet && r.URL.Path == "/v1/roles/reviewer":
			if roleHolder == "" {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"error":"no holder for role reviewer"}`))
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"role": "reviewer", "holder": roleHolder, "title": "reviewer", "capabilities": []string{"btw", "steer"},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/send":
			if err := json.NewDecoder(r.Body).Decode(new(map[string]any)); err != nil {
				t.Errorf("decode retry send: %v", err)
			}
			_, _ = w.Write([]byte(`{"event_id":"retry-envelope","recipient":"s1"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer listener.Close()
	handler, database := newTargetedMessageHandler(t, listener.URL)
	issue := createInteractionIssue(t, handler, "TEST", "Mention retry", "before")
	single := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{
		"body": "Retry a single mention.", "mentions": []map[string]any{{"target": "session:s1"}},
	}))
	if settled := settledCommentDeliveries(t, database, single.ID); len(settled) != 1 || settled[0].State != "failed" {
		t.Fatalf("initial retry candidate = %#v", settled)
	}
	live = true
	humanRetry := dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+single.ID+"/deliveries", map[string]any{"delivery": "steer"}, "alice")
	if humanRetry.Code != http.StatusCreated {
		t.Fatalf("human retry: status=%d body=%s", humanRetry.Code, humanRetry.Body.String())
	}
	if attempt := decodeBody[commentDeliveryRead](t, humanRetry); attempt.Attempt != 2 || attempt.Target != "session:s1" || attempt.State != "sent" {
		t.Fatalf("human retry attempt = %#v", attempt)
	}
	bearerRetry := bearerRequest(t, handler, http.MethodPost, "/api/v1/comments/"+single.ID+"/deliveries", map[string]any{
		"actor": map[string]any{"kind": "session", "id": "s2"}, "delivery": "btw",
	})
	if bearerRetry.Code != http.StatusCreated {
		t.Fatalf("bearer retry: status=%d body=%s", bearerRetry.Code, bearerRetry.Body.String())
	}
	if attempt := decodeBody[commentDeliveryRead](t, bearerRetry); attempt.Attempt != 3 || attempt.Delivery != "btw" || attempt.State != "sent" {
		t.Fatalf("bearer retry attempt = %#v", attempt)
	}

	multiple := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{
		"body": "Select exactly one retry target.", "mentions": []map[string]any{{"target": "session:s1"}, {"target": "session:s2"}},
	}))
	settledCommentDeliveries(t, database, multiple.ID)
	missingTarget := dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+multiple.ID+"/deliveries", map[string]any{"delivery": "steer"}, "alice")
	if missingTarget.Code != http.StatusBadRequest || !strings.Contains(missingTarget.Body.String(), `"code":"MENTION_INPUT"`) {
		t.Fatalf("multi-target retry without target: status=%d body=%s", missingTarget.Code, missingTarget.Body.String())
	}
	namedRetry := dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+multiple.ID+"/deliveries", map[string]any{
		"target": "session:s2", "delivery": "steer",
	}, "alice")
	if namedRetry.Code != http.StatusCreated {
		t.Fatalf("named retry: status=%d body=%s", namedRetry.Code, namedRetry.Body.String())
	}
	if attempt := decodeBody[commentDeliveryRead](t, namedRetry); attempt.Target != "session:s2" || attempt.Attempt != 2 || attempt.SessionID == nil || *attempt.SessionID != "s2" {
		t.Fatalf("named retry attempt = %#v", attempt)
	}

	role := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{
		"body": "Retry current role holder.", "mentions": []map[string]any{{"target": "role:reviewer"}},
	}))
	settledCommentDeliveries(t, database, role.ID)
	roleHolder = "s2"
	roleRetry := dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+role.ID+"/deliveries", map[string]any{"delivery": "steer"}, "alice")
	if roleRetry.Code != http.StatusCreated {
		t.Fatalf("role retry: status=%d body=%s", roleRetry.Code, roleRetry.Body.String())
	}
	if attempt := decodeBody[commentDeliveryRead](t, roleRetry); attempt.SessionID == nil || *attempt.SessionID != "s2" || attempt.State != "sent" {
		t.Fatalf("role retry attempt = %#v", attempt)
	}
	roleHolder = ""
	noHolderRetry := dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+role.ID+"/deliveries", map[string]any{"delivery": "steer"}, "alice")
	if noHolderRetry.Code != http.StatusCreated {
		t.Fatalf("no-holder retry: status=%d body=%s", noHolderRetry.Code, noHolderRetry.Body.String())
	}
	if attempt := decodeBody[commentDeliveryRead](t, noHolderRetry); attempt.SessionID != nil || attempt.State != "failed" ||
		attempt.Error == nil || *attempt.Error != "no holder for role reviewer" {
		t.Fatalf("no-holder retry attempt = %#v", attempt)
	}
}

func TestCommentMentionReadProjectionIncludesReplyChain(t *testing.T) {
	t.Parallel()
	const sessions = `[{"session_id":"s1","title":"planner","capabilities":["steer"]}]`
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/registry/s1":
			writeSessionRecord(t, w, sessions, "s1")
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sessions":
			_, _ = w.Write([]byte(sessions))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/send":
			_, _ = w.Write([]byte(`{"event_id":"projection-envelope","recipient":"s1"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer listener.Close()
	handler, _ := newTargetedMessageHandler(t, listener.URL)
	issue := createInteractionIssue(t, handler, "TEST", "Mention projection", "before")
	root := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{
		"body": "Root mention.", "mentions": []map[string]any{{"target": "session:s1"}},
	}))
	reply := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{
		"body": "Reply mention.", "reply_to": root.ID, "mentions": []map[string]any{{"target": "session:s1"}},
	}))
	if reply.ReplyTo == nil || *reply.ReplyTo != root.ID {
		t.Fatalf("reply mention = %#v", reply)
	}
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/comments/"+root.ID, nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("read mention thread: status=%d body=%s", response.Code, response.Body.String())
	}
	var thread struct {
		Comment mentionedCommentRead   `json:"comment"`
		Replies []mentionedCommentRead `json:"replies"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &thread); err != nil {
		t.Fatalf("decode mention thread: %v", err)
	}
	if len(thread.Comment.Mentions) != 1 || len(thread.Comment.Deliveries) != 1 ||
		len(thread.Replies) != 1 || thread.Replies[0].ID != reply.ID || len(thread.Replies[0].Mentions) != 1 ||
		len(thread.Replies[0].Deliveries) != 1 {
		t.Fatalf("mention thread projection = %#v", thread)
	}
}

func TestMentionedCommentDoesNotReresolveMissingRoleHolder(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	roleLookups := 0
	sends := 0
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/roles/reviewer":
			mu.Lock()
			roleLookups++
			mu.Unlock()
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"no holder for role reviewer"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/send":
			mu.Lock()
			sends++
			mu.Unlock()
			w.WriteHeader(http.StatusInternalServerError)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer listener.Close()
	handler, database := newTargetedMessageHandler(t, listener.URL)
	issue := createInteractionIssue(t, handler, "TEST", "Missing role holder", "before")
	patched := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]any{"route": "role:reviewer"}, "alice")
	if patched.Code != http.StatusOK {
		t.Fatalf("set role route: status=%d body=%s", patched.Code, patched.Body.String())
	}

	created := aResolvedMentionCreation(t, handler, database, issue.Key, map[string]any{
		"body": "No holder should remain unresolved.", "mentions": []map[string]any{{"target": "role:reviewer"}},
	}, func() {
		mu.Lock()
		roleLookups = 0
		sends = 0
		mu.Unlock()
	})
	// Attempt 1 committed with the listener's refusal on it, which is the resolution this case
	// is about: it is kept, never taken again, so the comment reaches no holder that appears
	// later. The lookups counted since the last creation began are that proof: the creation's
	// own lookup is the one, and a sender that looked the role up again for the attempt makes
	// two. They are counted from the creation's start, not from its answer: the creation queues
	// the mention's send before it writes its response (createCommentFor), so the sender can
	// claim the attempt and look the role up again before the answer is read. An earlier
	// creation whose lookup was given up at mentionLookupTimeout - which the mention's sender
	// resolves again, as an attempt with nothing recorded on it asks - has settled before
	// again() zeroed the count (aResolvedMentionCreation).
	settled := settledCommentDeliveries(t, database, created.ID)
	mu.Lock()
	lookups, sendCount := roleLookups, sends
	mu.Unlock()
	if lookups != 1 || sendCount != 0 || len(created.Mentions) != 1 || created.Mentions[0].SessionID != nil ||
		created.Deliveries[0].ResolveError == nil || *created.Deliveries[0].ResolveError != "no holder for role reviewer" ||
		len(settled) != 1 || settled[0].SessionID != nil || settled[0].State != "failed" ||
		settled[0].Error == nil || *settled[0].Error != "no holder for role reviewer" {
		t.Fatalf("missing-holder mention = lookups by the creation and its send:%d sends:%d comment:%#v attempts:%#v, want the creation's one lookup and one unresolved failed attempt",
			lookups, sendCount, created, settled)
	}
	events := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/events", nil, "alice")
	if events.Code != http.StatusOK || !strings.Contains(events.Body.String(), `"suppress_route":false`) {
		t.Fatalf("missing-holder payload: status=%d body=%s", events.Code, events.Body.String())
	}
}

// A mention inside an ask's thread is delivered to a session, and that session answers through
// the delivery callback. Both the receipt and the answer have to name the ask: the ask card and
// the Inbox row select every reply on ask_id, and the stream keys its refresh on the event's
// own ask_id.
func TestCallbackReplyUnderAnAskJoinsTheAskThread(t *testing.T) {
	t.Parallel()
	live := true
	sent := []map[string]any{}
	listener := sessionListener(t, &live, &sent)
	defer listener.Close()
	handler, database := newTargetedMessageHandler(t, listener.URL)
	issue := createInteractionIssue(t, handler, "TEST", "Ask thread callback", "before")
	askResponse := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
		"question": "Which approach?", "actor": sessionActor(),
	})
	if askResponse.Code != http.StatusCreated {
		t.Fatalf("create ask: status=%d body=%s", askResponse.Code, askResponse.Body.String())
	}
	ask := decodeBody[model.Ask](t, askResponse)
	clarification := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{
		"body": "Say more, @session:s1.", "ask_id": ask.ID,
		"mentions": []map[string]any{{"target": "session:s1"}},
	}))
	settledCommentDeliveries(t, database, clarification.ID)

	answered := bearerRequest(t, handler, http.MethodPost, "/api/v1/comments/"+clarification.ID+"/reply", map[string]any{
		"actor": map[string]any{"kind": "session", "id": "s1"}, "attempt": 1, "body": "The second approach.",
	})
	if answered.Code != http.StatusCreated {
		t.Fatalf("callback reply: status=%d body=%s", answered.Code, answered.Body.String())
	}
	reply := decodeBody[model.Comment](t, answered)
	if reply.AskID == nil || *reply.AskID != ask.ID || reply.ReplyTo != nil {
		t.Fatalf("callback reply ask_id=%#v reply_to=%#v, want the ask and no reply_to", reply.AskID, reply.ReplyTo)
	}
	if reply.Turn == nil || *reply.Turn != "human" {
		t.Fatalf("callback reply turn = %#v, want human", reply.Turn)
	}

	thread := dispatchRequest(t, handler, http.MethodGet, "/api/v1/asks/"+ask.ID, nil, "alice")
	if thread.Code != http.StatusOK {
		t.Fatalf("read ask thread: status=%d body=%s", thread.Code, thread.Body.String())
	}
	read := decodeBody[struct {
		Replies []model.Comment `json:"replies"`
	}](t, thread)
	if len(read.Replies) != 2 || read.Replies[1].ID != reply.ID {
		t.Fatalf("ask replies = %#v, want the clarification then the callback reply", read.Replies)
	}

	events := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/events", nil, "alice")
	if events.Code != http.StatusOK {
		t.Fatalf("read events: status=%d body=%s", events.Code, events.Body.String())
	}
	log := decodeBody[[]struct {
		Type    string         `json:"type"`
		Payload map[string]any `json:"payload"`
	}](t, events)
	receipts := map[string]int{}
	for _, entry := range log {
		if entry.Type != "comment.delivery" && entry.Type != "comment.answered" {
			continue
		}
		receipts[entry.Type]++
		if entry.Payload["ask_id"] != ask.ID {
			t.Fatalf("%s ask_id = %#v, want %s", entry.Type, entry.Payload["ask_id"], ask.ID)
		}
	}
	if receipts["comment.delivery"] != 1 || receipts["comment.answered"] != 1 {
		t.Fatalf("receipts = %#v, want one comment.delivery and one comment.answered, each naming %s", receipts, ask.ID)
	}
}

// routeMover moves an issue's delivery route while a comment's resolution of that route is in
// flight, which is the fixture's own act rather than any lookup's work. It keeps the move
// clear of the lookup in both directions.
//
// Nothing it runs is bound to a lookup request's context. A comment's creation bounds every
// listener lookup it makes at mentionLookupTimeout (250 ms, comment_delivery.go) and is
// answered without a lookup that missed it, so a move on the request's context is cancelled
// the moment its update has to wait - behind another transaction's row lock, or behind a
// loaded runner's scheduler - and then the route the locked row carries never moves at all,
// which is the one thing these cases are about. The test's own context bounds it instead.
//
// It also runs on one connection, taken before the comment request: a move is a statement on a
// warm connection rather than a pool acquisition that may first have to open one.
//
// What a case cannot leave to timing is that its move lands before the comment's transaction
// reads the route under the issue row's lock, because a move the transaction read past proves
// nothing. prepare settles that with the lock itself rather than with a clock: it writes the
// new route and holds the write open, so the transaction's own "for no key update" read of
// that row waits for release, however late the lookup reached this listener and whether or not
// Dispatch was still waiting for its answer.
type routeMover struct {
	t    *testing.T
	conn *pgxpool.Conn
	// held is the prepared move: the open transaction holding the issue row, or nil.
	held pgx.Tx
}

func newRouteMover(t *testing.T, database *store.Store) *routeMover {
	t.Helper()
	conn, err := database.Pool.Acquire(t.Context())
	if err != nil {
		t.Fatalf("take the connection the route moves run on: %v", err)
	}
	// A prepared move nothing releases would hold the issue row for as long as the test ran,
	// and the comment's transaction would wait on it rather than fail. Postgres ends it
	// instead, and the case fails on the assertion it is about.
	if _, err := conn.Exec(t.Context(), `set idle_in_transaction_session_timeout = '30s'`); err != nil {
		t.Fatalf("bound a prepared route move: %v", err)
	}
	mover := &routeMover{t: t, conn: conn}
	t.Cleanup(func() {
		if mover.held != nil {
			_ = mover.held.Rollback(context.Background())
		}
		conn.Release()
	})
	return mover
}

// prepare writes route on the issue and holds that write open, taking the issue row's lock
// before the comment request is made; release lets it land.
func (m *routeMover) prepare(issueKey, route string) {
	m.t.Helper()
	held, err := m.conn.Begin(m.t.Context())
	if err != nil {
		m.t.Fatalf("open the move of %s to %s: %v", issueKey, route, err)
	}
	if _, err := held.Exec(m.t.Context(), `
		update issues set route = $2 where key = $1
	`, issueKey, route); err != nil {
		m.t.Fatalf("write the move of %s to %s: %v", issueKey, route, err)
	}
	m.held = held
}

// release lands the prepared move, from inside the lookup handler whose route it is moving.
func (m *routeMover) release() {
	held := m.held
	m.held = nil
	if err := held.Commit(m.t.Context()); err != nil {
		m.t.Errorf("land the prepared route move: %v", err)
	}
}

// move writes route on the issue in one statement, for a case that moves the route again on a
// later lookup and so cannot hold the row across the comment's earlier rounds: the transaction
// would wait for a release that only the next lookup makes, and that lookup only comes once
// the round it is waiting on has ended.
func (m *routeMover) move(issueKey, route string) {
	if _, err := m.conn.Exec(m.t.Context(), `
		update issues set route = $2 where key = $1
	`, issueKey, route); err != nil {
		m.t.Errorf("move the route of %s to %s mid-resolution: %v", issueKey, route, err)
	}
}

// The issue's route is resolved through the Envoy listener before the comment's transaction
// opens, so the route can move while that lookup is in flight. The locked issue row is the
// authority: a resolution it contradicts is taken again against the route the lock found, and
// a route that keeps moving is a conflict rather than a comment whose route suppression was
// decided from a route the issue no longer has.
//
// Serial: the suppression it reads measures a span. That suppression is decided from the
// redo's resolution of the locked route, and Dispatch takes that resolution only from a lookup
// the listener answers inside mentionLookupTimeout (250 ms, comment_delivery.go): one it gave
// up on pins no session and suppresses nothing. This listener is one more goroutine of this
// process, so a parallel neighbour's work is measured with it - measured on this pod, a bare
// loopback lookup under two busy loops on two cores misses that bound about once in sixty. The
// moves themselves are off the clock (routeMover) and the bound is the product's, so what is
// left is this measurement, and it runs with the machine to itself.
func TestCommentRouteResolutionIsVerifiedAgainstTheLockedIssue(t *testing.T) {
	var database *store.Store
	var mover *routeMover
	lookups := []string{}
	const movedRoute = "role:auditor"
	const sessions = `[{"session_id":"s1","title":"planner","capabilities":["btw","steer"]}]`
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/v1/roles/"):
			role := strings.TrimPrefix(r.URL.Path, "/v1/roles/")
			lookups = append(lookups, role)
			// The route moves while Dispatch is asking the listener who holds the old
			// one, and the comment's transaction waits on that move's lock whatever
			// this lookup does. One move: the redo's own lookup finds the route the
			// locked row carries and touches no database at all.
			if len(lookups) == 1 {
				mover.release()
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"role": role, "holder": "s1", "title": "planner", "capabilities": []string{"btw", "steer"},
			})
		case r.URL.Path == "/v1/registry/s1":
			writeSessionRecord(t, w, sessions, "s1")
		case r.URL.Path == "/v1/sessions":
			_, _ = w.Write([]byte(sessions))
		case r.URL.Path == "/v1/messages/send":
			_, _ = w.Write([]byte(`{"event_id":"mention-envelope","recipient":"s1"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer listener.Close()
	var handler http.Handler
	// The seeded spec's settlement locks the very issue row these moves take, and one that
	// fired inside a resolution would hold the move behind it (testServerOptions.settle). It
	// decides nothing here, so it is kept out of the way for the whole test.
	handler, database, _ = newTestServer(t, testServerOptions{envoyURL: listener.URL, settle: time.Hour})
	issue := createInteractionIssue(t, handler, "TEST", "Route moves mid-resolution", "before")
	patched := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]any{
		"route": "role:reviewer",
	}, "alice")
	if patched.Code != http.StatusOK {
		t.Fatalf("set route: status=%d body=%s", patched.Code, patched.Body.String())
	}
	mover = newRouteMover(t, database)
	// One move, landed at the first lookup: the redo resolves the route the locked row carries.
	mover.prepare(issue.Key, movedRoute)
	created := postMentionedComment(t, handler, issue.Key, map[string]any{
		"body": "Mentioning the session the new route holds.", "mentions": []map[string]any{{"target": "session:s1"}},
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("comment across one route move: status=%d body=%s", created.Code, created.Body.String())
	}
	settledCommentDeliveries(t, database, decodeMentionedComment(t, created).ID)
	if len(lookups) != 2 || lookups[0] != "reviewer" || lookups[1] != "auditor" {
		t.Fatalf("route lookups = %#v, want the stale reviewer then the locked auditor", lookups)
	}
	events := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/events", nil, "alice")
	if events.Code != http.StatusOK || !strings.Contains(events.Body.String(), `"suppressed_route":"role:auditor"`) {
		t.Fatalf("suppression after the route moved: status=%d body=%s", events.Code, events.Body.String())
	}

	// A route that keeps moving is refused rather than decided from a stale resolution.
	lookups = nil
	moved := 0
	listener.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/v1/roles/"):
			role := strings.TrimPrefix(r.URL.Path, "/v1/roles/")
			lookups = append(lookups, role)
			moved++
			mover.move(issue.Key, fmt.Sprintf("role:moved-%d", moved))
			_ = json.NewEncoder(w).Encode(map[string]any{
				"role": role, "holder": "s1", "title": "planner", "capabilities": []string{"btw", "steer"},
			})
		case r.URL.Path == "/v1/registry/s1":
			writeSessionRecord(t, w, sessions, "s1")
		case r.URL.Path == "/v1/sessions":
			_, _ = w.Write([]byte(sessions))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	conflict := postMentionedComment(t, handler, issue.Key, map[string]any{
		"body": "The route will not hold still.", "mentions": []map[string]any{{"target": "session:s1"}},
	})
	if conflict.Code != http.StatusConflict || !strings.Contains(conflict.Body.String(), "ROUTE_CHANGED") {
		t.Fatalf("comment across a moving route: status=%d body=%s", conflict.Code, conflict.Body.String())
	}
	if len(lookups) != 2 {
		t.Fatalf("route lookups = %#v, want the resolution taken exactly twice before the conflict", lookups)
	}
}

// A comment the write is going to refuse costs the Envoy listener nothing. Its mentions are
// resolved before the transaction opens - one lookup each - so an owner that does not exist, or
// a suggestion on a closed issue (which still takes every other comment), has to be found first,
// from the pool, or every doomed request wakes the listener once per mention it names.
func TestACommentItsOwnerRefusesResolvesNoMention(t *testing.T) {
	t.Parallel()
	lookups := 0
	const sessions = `[{"session_id":"s1","title":"planner","capabilities":["steer"]},` +
		`{"session_id":"s2","title":"tester","capabilities":["steer"]}]`
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lookups++
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/registry/s1":
			writeSessionRecord(t, w, sessions, "s1")
		case r.Method == http.MethodGet && r.URL.Path == "/v1/registry/s2":
			writeSessionRecord(t, w, sessions, "s2")
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sessions":
			_, _ = w.Write([]byte(sessions))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer listener.Close()
	handler, database := newTargetedMessageHandler(t, listener.URL)
	issue := createInteractionIssue(t, handler, "TEST", "Refused before the listener", "before")
	if _, err := database.Pool.Exec(context.Background(), `
		update issues set status = 'done', closed_at = now() where key = $1
	`, issue.Key); err != nil {
		t.Fatalf("close the issue: %v", err)
	}
	mentions := map[string]any{
		"body": "Two mentions the listener must never be asked about.",
		"mentions": []map[string]any{
			{"target": "session:s1"}, {"target": "session:s2"},
		},
	}
	lookups = 0

	missing := postMentionedComment(t, handler, "TEST-404", mentions)
	if missing.Code != http.StatusNotFound {
		t.Fatalf("comment on an issue that does not exist: status=%d body=%s", missing.Code, missing.Body.String())
	}
	suggestion := map[string]any{
		"body": mentions["body"], "mentions": mentions["mentions"],
		"anchor": map[string]any{"artifact": "spec", "quote": "before"}, "suggestion": map[string]string{"replace_with": "after"},
	}
	closed := postMentionedComment(t, handler, issue.Key, suggestion)
	if closed.Code != http.StatusConflict || !strings.Contains(closed.Body.String(), `"code":"ISSUE_CLOSED"`) {
		t.Fatalf("suggestion on a closed issue: status=%d body=%s", closed.Code, closed.Body.String())
	}
	if lookups != 0 {
		t.Fatalf("the listener answered %d lookups, want none for a missing owner and a closed issue's suggestion", lookups)
	}
}

// A mention's attempt row is committed pending before its send, so the mentioned session can
// answer the frame while it is still in flight. replyComment's body branch appends
// comment.answered and records the reply, leaving no comment.delivery of its own, so the settle
// transaction still owes the receipt: without it an answered mention carries none at all, and
// neither the comment's delivery projection nor the issue event log describes the delivery that
// plainly happened.
func TestReplyDuringAMentionSendStillRecordsTheDeliveryReceipt(t *testing.T) {
	t.Parallel()
	var handler http.Handler
	replied := 0
	const sessions = `[{"session_id":"s1","title":"planner","capabilities":["btw","steer"]}]`
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/registry/s1":
			writeSessionRecord(t, w, sessions, "s1")
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sessions":
			_, _ = w.Write([]byte(sessions))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/send":
			var request struct {
				Payload string `json:"payload"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Errorf("decode mention send: %v", err)
			}
			var frame struct {
				Delivery struct {
					Attempt   int    `json:"attempt"`
					CommentID string `json:"comment_id"`
					Target    string `json:"target"`
				} `json:"delivery"`
			}
			if err := json.Unmarshal([]byte(request.Payload), &frame); err != nil {
				t.Errorf("decode mention delivery frame: %v", err)
			}
			// The session answers the mention it was just woken with, before this send returns.
			reply := bearerRequest(t, handler, http.MethodPost, "/api/v1/comments/"+frame.Delivery.CommentID+"/reply", map[string]any{
				"actor": map[string]any{"kind": "session", "id": "s1"}, "attempt": frame.Delivery.Attempt,
				"target": frame.Delivery.Target, "body": "Answered before the send returned.",
			})
			if reply.Code != http.StatusCreated {
				t.Errorf("reply from inside the mention send: status=%d body=%s", reply.Code, reply.Body.String())
			}
			replied++
			_, _ = w.Write([]byte(`{"event_id":"mention-envelope","recipient":"s1"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer listener.Close()
	handler, database := newTargetedMessageHandler(t, listener.URL)
	issue := createInteractionIssue(t, handler, "TEST", "Mention answered mid-send", "before")
	comment := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{
		"body": "Answer this while it is still in flight.", "mentions": []map[string]any{{"target": "session:s1"}},
	}))
	// The reply settles the row while the send is in flight; the sender's settle transaction,
	// after the send returns, is what appends the receipt the answered row owes.
	receipts := awaitCommentDeliveryReceipts(t, database, comment.ID, "session:s1", 1)
	if replied != 1 {
		t.Fatalf("the session answered %d times, want exactly one reply from inside the send", replied)
	}

	attempt := commentAttemptRow(t, database, comment.ID, "session:s1", 1)
	if !strings.HasPrefix(attempt, "sent//") || strings.HasPrefix(attempt, "sent///") {
		t.Fatalf("attempt row = %q, want the reply recorded against a sent attempt", attempt)
	}
	if receipts != attempt {
		t.Fatalf("comment.delivery events for attempt 1 = %q, want the one receipt %q the answered row owes", receipts, attempt)
	}
	var answers int
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) from events where type = 'comment.answered'
	`).Scan(&answers); err != nil {
		t.Fatalf("count answers: %v", err)
	}
	if answers != 1 {
		t.Fatalf("comment.answered events = %d, want the session's one reply", answers)
	}
}
