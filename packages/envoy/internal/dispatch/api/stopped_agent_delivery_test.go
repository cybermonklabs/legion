package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/listenerwire"
)

// A Legion agent whose session has stopped is still a message's recipient while the listener's
// agent registry names it: the listener hands what is sent to it to the daemon that answers for
// it and answers the send as delivered (cmd/listener/legion_handoff.go). So Dispatch records that
// session on the attempt whatever mode it was sent in, the attempt is sent, and the reply the
// daemon posts as that session is the attempt's own.

const (
	stoppedAgent        = "stopped-1"
	stoppedNewest       = "stopped-2"
	registeredRole      = "legion-test-test-1-planner"
	unregisteredRole    = "legion-test-test-1-reviewer"
	unregisteredSession = "gone"
)

// stoppedAgentListener is an Envoy listener with no live session and no live role holder, whose
// Legion agent registry holds two stopped sessions of the role legion-test-test-1-planner,
// stopped-2 newest and stopped-1 before it, each of a project whose daemon answers (`answering`),
// and holds no entry for any other role token, such as legion-test-test-1-reviewer. Its registry
// reads fail while registryDown is set, and its entries carry `answering: false` while
// notAnswering is set, as the entries of a project whose daemon runs without `answers` do. While
// released is set its entries carry `released: true`, as the listener of 043f9c67 answered the
// entry its daemon wrote for a claim idle release suspended; the registry's entry no longer
// declares that field, so this is where a TestReleased test writes it. Its role reads answer the
// role token's project's flag beside the entries, an empty list's included (`answering`, true
// unless notAnswering is set). It takes every send, as its hand-off does, and records the session
// each one is for; while refuseSends is set it records each and refuses it as a listener with no
// daemon answering for the agent does. It records every role publish's body too, and answers it
// as the publish route answers a role no live session holds: 202 kept, or, while publishAnswer
// names it, 200 for a live holder that claimed the role meanwhile or 404 for a publish it kept
// nothing of. While storeSends is set it takes each send as main's listener takes a send to a
// session Envoy still knows that no daemon answers for (#2643): 200, stored for that session,
// state stopped. Its role lookup names roleHolder as the role's live holder while that is set, and
// does not answer while rolesDown is set.
//
// It records what the listener's own routes make of each send it took as handed off (handedOff):
// the send is published on the hand-off subject of its session under the project test
// (listenerwire.LegionHandoffSubject).
type stoppedAgentListener struct {
	*httptest.Server
	registryDown  atomic.Bool
	refuseSends   atomic.Bool
	storeSends    atomic.Bool
	released      atomic.Bool
	notAnswering  atomic.Bool
	rolesDown     atomic.Bool
	publishAnswer atomic.Int32
	roleHolder    atomic.Pointer[string]

	mu        sync.Mutex
	sends     []string
	publishes []map[string]any
	handoffs  []string
}

func newStoppedAgentListener(t *testing.T) *stoppedAgentListener {
	t.Helper()
	listener := &stoppedAgentListener{}
	agent := func(sessionID string) map[string]any {
		entry := map[string]any{
			"session_id": sessionID, "legion_project": "test", "issue": "TEST-1", "role": "planner",
			"role_token": registeredRole, "title": "planner " + sessionID, "harness": "omp",
			"started_at": "2026-10-26T00:00:00Z", "recorded_at": "2026-10-26T00:00:00Z", "live": false,
			// The listener derives `answering` at the read from the project's answering record,
			// false while the project has stated none.
			"answering": !listener.notAnswering.Load(),
		}
		// The listener of 043f9c67 answered `released` for the entry its daemon wrote for a claim
		// idle release suspended, and omitted it otherwise.
		if listener.released.Load() {
			entry["released"] = true
		}
		return entry
	}
	listener.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		registry := strings.HasPrefix(r.URL.Path, "/v1/legion/agents")
		switch {
		case registry && listener.registryDown.Load():
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":"nats: timeout"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sessions":
			_, _ = w.Write([]byte(`[]`))
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/registry/"):
			id := strings.TrimPrefix(r.URL.Path, "/v1/registry/")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"no live session ` + id + `"}`))
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/roles/") && listener.rolesDown.Load():
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":"service starting"}`))
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/roles/"):
			role := strings.TrimPrefix(r.URL.Path, "/v1/roles/")
			if holder := listener.roleHolder.Load(); holder != nil {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"role": role, "holder": *holder, "holder_state": "live", "title": "planner " + *holder,
					"capabilities": []string{"aside", "btw", "steer"},
				})
				return
			}
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"no holder for role ` + role + `","reason":"unclaimed"}`))
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/legion/agents/sessions/"):
			id := strings.TrimPrefix(r.URL.Path, "/v1/legion/agents/sessions/")
			if id != stoppedAgent && id != stoppedNewest {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"error":"unknown legion agent ` + id + `"}`))
				return
			}
			_ = json.NewEncoder(w).Encode(agent(id))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/legion/agents/roles/"+registeredRole:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"entries": []any{agent(stoppedNewest), agent(stoppedAgent)}, "answering": !listener.notAnswering.Load(),
			})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/legion/agents/roles/"):
			_ = json.NewEncoder(w).Encode(map[string]any{"entries": []any{}, "answering": !listener.notAnswering.Load()})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/send":
			var input struct {
				TargetSession string `json:"target_session"`
			}
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				t.Errorf("decode listener send: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			listener.mu.Lock()
			defer listener.mu.Unlock()
			listener.sends = append(listener.sends, input.TargetSession)
			count := len(listener.sends)
			if listener.refuseSends.Load() {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"error":"no live session ` + input.TargetSession + `"}`))
				return
			}
			if listener.storeSends.Load() {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"event_id": "stored-" + strconv.Itoa(count), "recipient": input.TargetSession,
					"recipient_state": "gone", "state": "stopped",
				})
				return
			}
			listener.handoffs = append(listener.handoffs, listenerwire.LegionHandoffSubject("test", input.TargetSession))
			_ = json.NewEncoder(w).Encode(map[string]any{
				"event_id": "handed-off-" + strconv.Itoa(count), "recipient": input.TargetSession,
			})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/publish":
			var input map[string]any
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				t.Errorf("decode listener publish: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			listener.mu.Lock()
			defer listener.mu.Unlock()
			listener.publishes = append(listener.publishes, input)
			count := len(listener.publishes)
			topic, _ := input["topic"].(string)
			switch listener.publishAnswer.Load() {
			case http.StatusOK:
				_ = json.NewEncoder(w).Encode(map[string]any{
					"event_id": "delivered-" + strconv.Itoa(count), "topic": topic, "holder": "woken", "holder_state": "live",
				})
			case http.StatusNotFound:
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"error":"no holder for role ` + strings.TrimPrefix(topic, "notifications.role.") +
					`","reason":"unclaimed","claim_revision":0}`))
			default:
				w.WriteHeader(http.StatusAccepted)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"event_id": "kept-" + strconv.Itoa(count), "topic": topic, "kept": true,
					"kept_reason": "unclaimed", "kept_until": 1793003600000,
				})
			}
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"not found"}`))
		}
	}))
	t.Cleanup(listener.Close)
	return listener
}

func (l *stoppedAgentListener) sent() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string{}, l.sends...)
}

// published is the body of every role publish the listener took, in order.
func (l *stoppedAgentListener) published() []map[string]any {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]map[string]any{}, l.publishes...)
}

// handedOff is the hand-off subject of every send the listener took as handed off, in order.
func (l *stoppedAgentListener) handedOff() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string{}, l.handoffs...)
}

// assertHandedOff checks a message's one attempt was recorded for sessionID and sent.
func assertHandedOff(t *testing.T, name string, message model.Message, sessionID string) {
	t.Helper()
	if got := message.Deliveries; len(got) != 1 || got[0].Attempt != 1 || got[0].State != "sent" ||
		got[0].SessionID != sessionID || got[0].Error != nil {
		t.Fatalf("%s: deliveries = %#v, want attempt 1 sent to %s", name, got, sessionID)
	}
}

// replyAs posts the daemon's reply to a message's first attempt as sessionID.
func replyAs(t *testing.T, handler http.Handler, messageID, sessionID, body string) *httptest.ResponseRecorder {
	t.Helper()
	return bearerRequest(t, handler, http.MethodPost, "/api/v1/messages/"+messageID+"/reply", map[string]any{
		"actor": map[string]any{"kind": "session", "id": sessionID}, "attempt": 1, "body": body,
	})
}

func assertAnswered(t *testing.T, name string, response *httptest.ResponseRecorder, messageID string) {
	t.Helper()
	if response.Code != http.StatusCreated {
		t.Fatalf("%s: reply status=%d body=%s, want 201", name, response.Code, response.Body.String())
	}
	if reply := decodeBody[model.Message](t, response); reply.InReplyTo == nil || *reply.InReplyTo != messageID {
		t.Fatalf("%s: reply = %#v, want it in %s's thread", name, reply, messageID)
	}
}

func TestMessageToAStoppedRegisteredAgentIsSentToItsSessionAndTakesItsReply(t *testing.T) {
	t.Parallel()
	listener := newStoppedAgentListener(t)
	handler, _ := newTargetedMessageHandler(t, listener.URL)
	issue := createInteractionIssue(t, handler, "TEST", "Stopped agent", "before")

	// steer: the registry entry advertises no capabilities, so a live session's check would
	// refuse it; the stopped agent's daemon answers every mode by reply.
	onIssue := createIssueMessage(t, handler, issue.Key, map[string]any{
		"body": "Why did you pick this?", "target": "session:" + stoppedAgent, "delivery": "steer",
	}, "alice")
	assertHandedOff(t, "an issue message", onIssue, stoppedAgent)

	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/agents/"+stoppedAgent+"/messages", map[string]any{
		"body": "And on the conversation page?", "delivery": "btw",
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("issue-less message: status=%d body=%s", created.Code, created.Body.String())
	}
	issueless := decodeBody[model.Message](t, created)
	if issueless.IssueKey != nil {
		t.Fatalf("conversation page message = %#v, want no issue", issueless)
	}
	assertHandedOff(t, "an issue-less message", issueless, stoppedAgent)

	if got := listener.sent(); len(got) != 2 || got[0] != stoppedAgent || got[1] != stoppedAgent {
		t.Fatalf("listener sends = %v, want both messages sent to %s", got, stoppedAgent)
	}

	assertAnswered(t, "the issue message", replyAs(t, handler, onIssue.ID, stoppedAgent, "Because of the spec."), onIssue.ID)
	assertAnswered(t, "the issue-less message", replyAs(t, handler, issueless.ID, stoppedAgent, "Same answer."), issueless.ID)
}

// A role's newest session takes the role's messages while no live session holds the role and its
// project answers.
func TestRoleMessageToAStoppedRegisteredRoleGoesToItsNewestSession(t *testing.T) {
	t.Parallel()
	listener := newStoppedAgentListener(t)
	handler, _ := newTargetedMessageHandler(t, listener.URL)
	issue := createInteractionIssue(t, handler, "TEST", "Stopped role", "before")

	message := createIssueMessage(t, handler, issue.Key, map[string]any{
		"body": "Who planned this?", "target": "role:" + registeredRole, "delivery": "aside",
	}, "alice")
	assertHandedOff(t, "a role message", message, stoppedNewest)
	if got := listener.sent(); len(got) != 1 || got[0] != stoppedNewest {
		t.Fatalf("listener sends = %v, want one to %s", got, stoppedNewest)
	}

	// The role's older session is not the attempt's recipient, so its reply is refused.
	older := replyAs(t, handler, message.ID, stoppedAgent, "Not me.")
	if older.Code != http.StatusForbidden || !strings.Contains(older.Body.String(), `"code":"REPLY_FORBIDDEN"`) {
		t.Fatalf("older session's reply: status=%d body=%s, want 403 REPLY_FORBIDDEN", older.Code, older.Body.String())
	}
	assertAnswered(t, "the role message", replyAs(t, handler, message.ID, stoppedNewest, "I did."), message.ID)
}

// A role whose newest registry entry is a stopped agent - one whose issue has left the workflow,
// or one that failed or was suspended by the workflow or the operator - and whose project
// answers, is answered by that agent's daemon. So a message and a mention of the role are sent to
// its stopped newest session and settle sent there, and that session's reply, which its daemon
// posts, answers each. A session named outright is handed off as ever.
func TestARoleMessageAndARoleMentionOfAStoppedRoleGoToItsNewestSession(t *testing.T) {
	t.Parallel()
	listener := newStoppedAgentListener(t)
	handler, database := newTargetedMessageHandler(t, listener.URL)
	issue := createInteractionIssue(t, handler, "TEST", "Running role", "before")

	message := createIssueMessage(t, handler, issue.Key, map[string]any{
		"body": "Who holds this?", "target": "role:" + registeredRole, "delivery": "aside",
	}, "alice")
	assertHandedOff(t, "a role message", message, stoppedNewest)

	sessionTarget, roleTarget := "session:"+stoppedAgent, "role:"+registeredRole
	comment := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{
		"body": "Can either of you explain this?", "delivery": "btw",
		"mentions": []map[string]any{{"target": sessionTarget}, {"target": roleTarget}},
	}))
	if len(comment.Deliveries) != 2 {
		t.Fatalf("mention deliveries = %#v, want one per mention", comment.Deliveries)
	}
	want := map[string]string{sessionTarget: stoppedAgent, roleTarget: stoppedNewest}
	// The mention sender settles each attempt after the comment's response, so the outcome is
	// read off the settled rows rather than off the pending attempts that response carries.
	for _, delivery := range settledCommentDeliveries(t, database, comment.ID) {
		if delivery.State != "sent" || delivery.Attempt != 1 || delivery.Error != nil ||
			delivery.SessionID == nil || *delivery.SessionID != want[delivery.Target] {
			t.Fatalf("the %s mention = %#v (session %v, error %q), want attempt 1 sent to %s",
				delivery.Target, delivery, deref(delivery.SessionID), deref(delivery.Error), want[delivery.Target])
		}
		if row := commentAttemptRow(t, database, comment.ID, delivery.Target, 1); row != "sent///"+want[delivery.Target] {
			t.Fatalf("the %s mention's attempt row = %q, want sent to %s", delivery.Target, row, want[delivery.Target])
		}
	}

	sent := map[string]int{}
	for _, session := range listener.sent() {
		sent[session]++
	}
	if len(sent) != 2 || sent[stoppedNewest] != 2 || sent[stoppedAgent] != 1 {
		t.Fatalf("listener sends = %v, want the role message and the role mention sent to %s and the session mention to %s",
			sent, stoppedNewest, stoppedAgent)
	}
	// The role has an entry, so nothing is kept for it: nothing is published on the role's topic.
	if got := listener.published(); len(got) != 0 {
		t.Fatalf("listener publishes = %v, want none for a role with an entry", got)
	}

	assertAnswered(t, "the role message", replyAs(t, handler, message.ID, stoppedNewest, "I hold the answer."), message.ID)
	answered := bearerRequest(t, handler, http.MethodPost, "/api/v1/comments/"+comment.ID+"/reply", map[string]any{
		"actor": map[string]any{"kind": "session", "id": stoppedNewest}, "attempt": 1, "target": roleTarget,
		"body": "Answered by " + stoppedNewest + "'s daemon.",
	})
	if answered.Code != http.StatusCreated {
		t.Fatalf("%s's reply to the role mention: status=%d body=%s, want 201", stoppedNewest, answered.Code, answered.Body.String())
	}
}

func TestMentionOfAStoppedRegisteredAgentIsSentToItsSessionAndTakesItsReply(t *testing.T) {
	t.Parallel()
	listener := newStoppedAgentListener(t)
	handler, database := newTargetedMessageHandler(t, listener.URL)
	issue := createInteractionIssue(t, handler, "TEST", "Stopped mention", "before")

	sessionTarget, roleTarget := "session:"+stoppedAgent, "role:"+registeredRole
	comment := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{
		"body": "Can either of you explain this?", "delivery": "btw",
		"mentions": []map[string]any{{"target": sessionTarget}, {"target": roleTarget}},
	}))
	want := map[string]string{sessionTarget: stoppedAgent, roleTarget: stoppedNewest}
	if len(comment.Deliveries) != 2 {
		t.Fatalf("mention deliveries = %#v, want one per mention", comment.Deliveries)
	}
	// The create response carries the pending attempts; the mention sender settles them behind it.
	for _, delivery := range settledCommentDeliveries(t, database, comment.ID) {
		if delivery.State != "sent" || delivery.Attempt != 1 || delivery.Error != nil ||
			delivery.SessionID == nil || *delivery.SessionID != want[delivery.Target] {
			t.Fatalf("mention delivery = %#v (session %v, error %q), want attempt 1 sent to %s",
				delivery, deref(delivery.SessionID), deref(delivery.Error), want[delivery.Target])
		}
		if row := commentAttemptRow(t, database, comment.ID, delivery.Target, 1); row != "sent///"+want[delivery.Target] {
			t.Fatalf("%s attempt row = %q, want sent to %s", delivery.Target, row, want[delivery.Target])
		}
	}
	if got := listener.sent(); len(got) != 2 {
		t.Fatalf("listener sends = %v, want one per mention", got)
	}

	for target, sessionID := range want {
		answered := bearerRequest(t, handler, http.MethodPost, "/api/v1/comments/"+comment.ID+"/reply", map[string]any{
			"actor": map[string]any{"kind": "session", "id": sessionID}, "attempt": 1, "target": target,
			"body": "Answered by " + sessionID + "'s daemon.",
		})
		if answered.Code != http.StatusCreated {
			t.Fatalf("%s's reply to %s: status=%d body=%s", sessionID, target, answered.Code, answered.Body.String())
		}
		if reply := decodeBody[model.Comment](t, answered); reply.ReplyTo == nil || *reply.ReplyTo != comment.ID {
			t.Fatalf("%s's reply = %#v, want it in the mentioning comment's thread", sessionID, reply)
		}
	}
}

// A target the registry does not name, every target while the registry does not answer, a
// target the registry names whose daemon does not answer - the listener refuses the send, having
// handed it to no daemon - and a target whose project does not answer - the listener hands it to
// no daemon and only stores a send for a session that will not run again, so no send is made -
// are answered exactly as with no registry, with the session they recorded then: a message to
// the session, whose listener answer is no live session, is queued by Dispatch itself on the
// session's own subject (queueForUnknownSession), and a mention of the session keeps the
// listener's failure, as do a role message and a role mention.
//
// One role is kept for its first holder instead (roleKept): a role the registry holds no entry
// for while the role read says its project answers, whose first worker is coming. Each role
// message and role mention is published on the role's topic, which the listener keeps on the
// role's kept lane, and recorded queued with the kept copy's envelope and no session. Its control
// is the role with no entry whose read says its project does not answer, which keeps the
// listener's answer as with no registry.
func TestTargetTheRegistryDoesNotNameKeepsTheListenersAnswer(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name         string
		registryDown bool
		refuseSends  bool
		notAnswering bool
		session      string
		role         string
		// roleKept is a role the delivery keeps for its next holder rather than failing it with
		// the listener's no-holder answer.
		roleKept bool
		// wantSends is the least number of sends each named session takes, nil when the
		// resolution never reaches a send at all. A send the listener refuses with no live
		// session is one a listener take-over gives too, so each delivery tries again inside
		// the take-over window and the exact count is that window's; what this pins is that
		// every try went to the session the route resolves to and to no other.
		wantSends map[string]int
	}{
		{name: "unregistered", session: unregisteredSession, role: unregisteredRole, roleKept: true},
		// The role read's empty list says the project does not answer: its loop is off, so the
		// role keeps the listener's answer, as with no registry.
		{name: "unregistered, project not answering", notAnswering: true, session: unregisteredSession, role: unregisteredRole},
		{name: "registry unavailable", registryDown: true, session: stoppedAgent, role: registeredRole},
		{
			name: "no daemon answers", refuseSends: true, session: stoppedAgent, role: registeredRole,
			wantSends: map[string]int{stoppedAgent: 3, stoppedNewest: 2},
		},
		// The project's answering flag is the one check that names no session. Its listener
		// takes a send as main's does for a project that does not answer, stored for the stopped
		// session; the redirect makes none.
		{name: "project not answering", notAnswering: true, session: stoppedAgent, role: registeredRole},
	} {
		t.Run(tc.name, func(t *testing.T) {
			listener := newStoppedAgentListener(t)
			listener.registryDown.Store(tc.registryDown)
			listener.refuseSends.Store(tc.refuseSends)
			listener.notAnswering.Store(tc.notAnswering)
			listener.storeSends.Store(tc.notAnswering)
			handler, database := newTargetedMessageHandler(t, listener.URL)
			issue := createInteractionIssue(t, handler, "TEST", "Unanswered", "before")

			failed := func(name string, message model.Message, sessionID, failure string) {
				t.Helper()
				if got := message.Deliveries; len(got) != 1 || got[0].State != "failed" || got[0].SessionID != sessionID ||
					got[0].Error == nil || *got[0].Error != failure {
					t.Fatalf("%s: deliveries = %#v, want failed %q", name, got, failure)
				}
			}
			queued := func(name string, message model.Message, sessionID string) {
				t.Helper()
				if got := message.Deliveries; len(got) != 1 || got[0].State != "queued" || got[0].SessionID != sessionID ||
					got[0].Error != nil || got[0].EnvelopeID == nil {
					t.Fatalf("%s: deliveries = %#v, want queued for %s by Dispatch, naming its envelope and no error", name, got, sessionID)
				}
			}
			kept := func(name string, message model.Message) {
				t.Helper()
				if got := message.Deliveries; len(got) != 1 || got[0].State != model.DeliveryStateQueued || got[0].SessionID != "" ||
					got[0].Error != nil || got[0].EnvelopeID == nil || !strings.HasPrefix(*got[0].EnvelopeID, "kept-") {
					t.Fatalf("%s: deliveries = %#v, want queued for the role's next holder, naming no session and the kept copy's envelope", name, got)
				}
			}
			noLive := contracts.NoLiveSession + " " + tc.session
			noHolder := "no holder for role " + tc.role
			queued("an issue message", createIssueMessage(t, handler, issue.Key, map[string]any{
				"body": "Anyone?", "target": "session:" + tc.session, "delivery": "btw",
			}, "alice"), tc.session)

			created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/agents/"+tc.session+"/messages", map[string]any{
				"body": "Anyone here?", "delivery": "btw",
			}, "alice")
			if created.Code != http.StatusCreated {
				t.Fatalf("issue-less message: status=%d body=%s", created.Code, created.Body.String())
			}
			queued("an issue-less message", decodeBody[model.Message](t, created), tc.session)

			roleMessage := createIssueMessage(t, handler, issue.Key, map[string]any{
				"body": "Who holds this?", "target": "role:" + tc.role, "delivery": "btw",
			}, "alice")
			if tc.roleKept {
				kept("a role message", roleMessage)
			} else {
				failed("a role message", roleMessage, "", noHolder)
			}

			sessionTarget, roleTarget := "session:"+tc.session, "role:"+tc.role
			comment := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{
				"body":     "Mentioning nobody live.",
				"mentions": []map[string]any{{"target": sessionTarget}, {"target": roleTarget}},
			}))
			want := map[string]string{sessionTarget: noLive, roleTarget: noHolder}
			if len(comment.Deliveries) != 2 {
				t.Fatalf("mention deliveries = %#v, want one per mention", comment.Deliveries)
			}
			// The mention sender settles each attempt after the comment's response.
			for _, delivery := range settledCommentDeliveries(t, database, comment.ID) {
				if delivery.Target == roleTarget && tc.roleKept {
					if delivery.State != model.DeliveryStateQueued || delivery.SessionID != nil || delivery.Error != nil ||
						delivery.EnvelopeID == nil || !strings.HasPrefix(*delivery.EnvelopeID, "kept-") {
						t.Fatalf("%s mention delivery = %#v (session %v, error %q), want queued for the role's next holder with no session",
							delivery.Target, delivery, deref(delivery.SessionID), deref(delivery.Error))
					}
					if row := commentAttemptRow(t, database, comment.ID, delivery.Target, 1); row != model.DeliveryStateQueued+"///" {
						t.Fatalf("%s mention attempt row = %q, want queued with no session", delivery.Target, row)
					}
					continue
				}
				failure := want[delivery.Target]
				if delivery.State != "failed" || delivery.SessionID != nil || delivery.Error == nil || *delivery.Error != failure {
					t.Fatalf("%s mention delivery = %#v (session %v, error %q), want failed %q with no session",
						delivery.Target, delivery, deref(delivery.SessionID), deref(delivery.Error), failure)
				}
				if row := commentAttemptRow(t, database, comment.ID, delivery.Target, 1); row != "failed/"+failure+"//" {
					t.Fatalf("%s mention attempt row = %q, want failed with no session", delivery.Target, row)
				}
			}

			counted := map[string]int{}
			for _, session := range listener.sent() {
				counted[session]++
			}
			for session, least := range tc.wantSends {
				if counted[session] < least {
					t.Fatalf("listener sends = %v, want at least %d to %s", counted, least, session)
				}
			}
			for session := range counted {
				if _, wanted := tc.wantSends[session]; !wanted {
					t.Fatalf("listener sends = %v, want none to %s", counted, session)
				}
			}
			published := listener.published()
			if !tc.roleKept && len(published) != 0 {
				t.Fatalf("listener publishes = %v, want none", published)
			}
			if tc.roleKept && len(published) != 2 {
				t.Fatalf("listener publishes = %v, want the role message's and the role mention's", published)
			}
			for i, got := range published {
				if got["topic"] != "notifications.role."+tc.role || got["keep"] != true {
					t.Fatalf("publish %d = %#v, want it kept on the role's topic", i, got)
				}
			}
		})
	}
}
