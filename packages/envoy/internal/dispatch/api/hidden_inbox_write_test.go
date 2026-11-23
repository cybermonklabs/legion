package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// TestAnInboxWriteNeverReachesAThreadItsCallerMayNotSee: alice wrote a comment on PRIV-1 while PRIV
// was public, and one on PUB-1, and carol replied to both once PRIV was private, so alice holds an
// unread Reply row of each. Her token that lists PUB, a full member's person-scope token, sees PUB
// alone, since alice does not see PRIV. Each Inbox write it makes answers PRIV-1's thread exactly as
// a thread of an issue nobody holds: Mark all read narrowed to PRIV-1 marks 0, as narrowed to
// NOPE-1, and narrowed to project PRIV as to NOPE; a dismissal narrowed to PRIV-1 dismisses 0, as
// narrowed to NOPE-1; a restore naming the PRIV thread by id restores 0, as one naming a thread
// nobody holds; and the thread's own read mark answers 204 and moves nothing, as a thread nobody
// holds. None of them waits on PRIV-1's owner lock, which a writer of PRIV holds while each runs,
// none appends an event to PRIV's log, and alice's PRIV row stays unread.
//
// The Agent half (myAgentSeenAnd) is held alike: alice also wrote, while PRIV was public, one-off
// messages to s-priv-agent, a session that acted in PRIV, and to s-pub-agent, one her own
// credential proved in PUB, and each replied once PRIV was private. The token's Mark all read of
// its Agent threads marks s-pub-agent's thread alone: s-priv-agent's reply stays unread and no
// user_agent_state.updated event names that session. Her unnarrowed Mark all read then marks the
// PUB Reply thread alone, and her unnarrowed dismissal dismisses the two PUB threads alone, so the
// token still writes over what it sees.
func TestAnInboxWriteNeverReachesAThreadItsCallerMayNotSee(t *testing.T) {
	t.Parallel()
	seed := seedPrivateProjectWith(t, testServerOptions{settle: time.Hour})
	const caller privateCaller = "alice's person-scope token that lists PUB"
	threadOn := func(issue string) string {
		t.Helper()
		return createdID(t, seed.request(t, callerAlice, http.MethodPost, "/api/v1/issues/"+issue+"/comments",
			map[string]any{"body": "alice asks on " + issue + "."}))
	}
	seed.setVisibility(t, seedPrivate, model.ProjectPublic)
	hiddenThread := threadOn(seed.issue[seedPrivate])
	const privAgent, pubAgent = "s-priv-agent", "s-pub-agent"
	seed.asSession(t, privAgent, seedPrivate, http.MethodPost, "/api/v1/issues/"+seed.issue[seedPrivate]+"/comments",
		map[string]any{"body": privAgent + " works on PRIV."}, http.StatusCreated)
	seed.asSessionOf(t, seedAlice, pubAgent, seedPublic, http.MethodPost, "/api/v1/issues/"+seed.issue[seedPublic]+"/comments",
		map[string]any{"body": pubAgent + " works on PUB."}, http.StatusCreated)
	agentRoots := map[string]string{}
	for _, session := range []string{privAgent, pubAgent} {
		agentRoots[session] = createdID(t, seed.request(t, callerAlice, http.MethodPost, "/api/v1/agents/"+session+"/messages",
			map[string]any{"body": "A word for " + session + ".", "delivery": "btw"}))
	}
	seed.makePrivate(t, seedPrivate)
	agentReplies := map[string]string{}
	for session, root := range agentRoots {
		agentReplies[session] = writeAgentReply(t, seed.database.Pool, root, "session", session, time.Now())
	}
	visibleThread := threadOn(seed.issue[seedPublic])
	for issue, thread := range map[string]string{seed.issue[seedPrivate]: hiddenThread, seed.issue[seedPublic]: visibleThread} {
		seed.mustRequest(t, callerCarol, http.MethodPost, "/api/v1/issues/"+issue+"/comments",
			map[string]any{"body": "carol answers.", "reply_to": thread}, http.StatusCreated)
	}
	if _, unread := threadMark(t, seed.database, seedAlice, hiddenThread); !unread {
		t.Fatalf("alice's row of the PRIV thread is read before the walk; carol's reply should have left it unread")
	}
	minted := mintBody(string(caller), []string{seedPublic})
	minted["scopes"] = []string{model.ScopePerson}
	token := decodeBody[agentTokenResponse](t, dispatchRequest(t, seed.handler, http.MethodPost, "/api/v1/me/agent-tokens", minted, seedAlice))
	if token.Token == "" {
		t.Fatalf("mint %s: no token in the answer", caller)
	}
	seed.tokens[caller] = token.Token

	privEvents := func() int {
		t.Helper()
		var count int
		if err := seed.database.Pool.QueryRow(context.Background(), `
			select count(*) from events where type = 'user_thread_state.updated' and project_key = $1
		`, seedPrivate).Scan(&count); err != nil {
			t.Fatalf("count PRIV's thread-state events: %v", err)
		}
		return count
	}
	eventsBefore := privEvents()
	// call makes one write as the token while a writer of PRIV-1 holds its owner lock, failing when
	// the write queues behind that writer, and answers it.
	call := func(name, method, path string, body any) *httptest.ResponseRecorder {
		t.Helper()
		held := holdIssueOwnerLock(t, seed, seed.issue[seedPrivate])
		answered := make(chan *httptest.ResponseRecorder, 1)
		go func() { answered <- seed.request(t, caller, method, path, body) }()
		answer, waiting := awaitAnswerOrWait(t, seed, held, answered)
		if answer == nil {
			t.Errorf("%s queued behind the writer holding PRIV-1's owner lock, at %q", name, waiting)
			answer = held.letGo(t, answered)
		} else if err := held.tx.Rollback(context.Background()); err != nil {
			t.Fatalf("let PRIV-1's writer go: %v", err)
		}
		return answer
	}
	now := func() string { return time.Now().UTC().Format(time.RFC3339Nano) }
	nobodysThread := uuid.NewString()
	for _, pair := range []struct {
		name, method    string
		hidden, missing string
		hiddenBody      map[string]any
		missingBody     map[string]any
	}{
		{"Mark all read narrowed to an issue", http.MethodPost, "/api/v1/me/inbox/read", "/api/v1/me/inbox/read",
			map[string]any{"read_through": now(), "issue": seed.issue[seedPrivate]},
			map[string]any{"read_through": now(), "issue": "NOPE-1"}},
		{"Mark all read narrowed to a project", http.MethodPost, "/api/v1/me/inbox/read", "/api/v1/me/inbox/read",
			map[string]any{"read_through": now(), "project": []string{seedPrivate}},
			map[string]any{"read_through": now(), "project": []string{"NOPE"}}},
		{"a dismissal narrowed to an issue", http.MethodPost, "/api/v1/me/inbox/dismiss", "/api/v1/me/inbox/dismiss",
			map[string]any{"dismiss_through": now(), "issue": seed.issue[seedPrivate]},
			map[string]any{"dismiss_through": now(), "issue": "NOPE-1"}},
		{"a restore naming the thread", http.MethodPost, "/api/v1/me/inbox/dismiss", "/api/v1/me/inbox/dismiss",
			map[string]any{"threads": []string{inboxDismissReplyPrefix + hiddenThread}, "restore": true},
			map[string]any{"threads": []string{inboxDismissReplyPrefix + nobodysThread}, "restore": true}},
		{"the thread's own read mark", http.MethodPut, "/api/v1/me/threads/" + hiddenThread + "/read", "/api/v1/me/threads/" + nobodysThread + "/read",
			map[string]any{"read_through": now()}, map[string]any{"read_through": now()}},
	} {
		hidden := call(pair.name+" naming PRIV's thread", pair.method, pair.hidden, pair.hiddenBody)
		missing := call(pair.name+" naming nothing", pair.method, pair.missing, pair.missingBody)
		got := strings.TrimSpace(hidden.Body.String())
		want := strings.TrimSpace(strings.NewReplacer("NOPE-1", seed.issue[seedPrivate], "NOPE", seedPrivate, nobodysThread, hiddenThread).
			Replace(missing.Body.String()))
		if hidden.Code != missing.Code || got != want {
			t.Errorf("%s: PRIV's thread answers %d %s, and one nobody holds %d %s",
				pair.name, hidden.Code, got, missing.Code, strings.TrimSpace(missing.Body.String()))
		}
		if hidden.Code >= http.StatusBadRequest {
			t.Errorf("%s: PRIV's thread answers %d %s, want the write's own success", pair.name, hidden.Code, got)
		}
		if _, unread := threadMark(t, seed.database, seedAlice, hiddenThread); !unread {
			t.Errorf("%s marked alice's PRIV thread read through a token that does not see PRIV", pair.name)
		}
		if after := privEvents(); after != eventsBefore {
			t.Errorf("%s appended %d thread-state events to PRIV's log, which the token does not see", pair.name, after-eventsBefore)
		}
	}
	agentStateEvents := func(session string) int {
		t.Helper()
		var count int
		if err := seed.database.Pool.QueryRow(context.Background(), `
			select count(*) from events where type = 'user_agent_state.updated' and payload->>'session_id' = $1
		`, session).Scan(&count); err != nil {
			t.Fatalf("count %s's agent-state events: %v", session, err)
		}
		return count
	}
	replyRead := func(reply string) bool {
		t.Helper()
		var read bool
		if err := seed.database.Pool.QueryRow(context.Background(), `
			select exists (select 1 from user_agent_reply_read where login = $1 and reply_id = $2::uuid)
		`, seedAlice, reply).Scan(&read); err != nil {
			t.Fatalf("read whether alice read %s: %v", reply, err)
		}
		return read
	}
	privAgentEvents := agentStateEvents(privAgent)
	agents := seed.request(t, caller, http.MethodPost, "/api/v1/me/inbox/read", map[string]any{"read_through": now(), "kind": []string{"agent"}})
	if agents.Code != http.StatusOK || markedBy(t, agents) != 1 {
		t.Errorf("the token's Mark all read of its Agent threads: %d %s, want %s's thread alone marked",
			agents.Code, strings.TrimSpace(agents.Body.String()), pubAgent)
	}
	if replyRead(agentReplies[privAgent]) {
		t.Errorf("the token's Mark all read of its Agent threads marked %s's reply read, a session it may not see", privAgent)
	}
	if after := agentStateEvents(privAgent); after != privAgentEvents {
		t.Errorf("the token's Mark all read of its Agent threads appended %d user_agent_state.updated events naming %s", after-privAgentEvents, privAgent)
	}
	if !replyRead(agentReplies[pubAgent]) {
		t.Errorf("the token's Mark all read of its Agent threads left %s's reply unread", pubAgent)
	}
	marked := seed.request(t, caller, http.MethodPost, "/api/v1/me/inbox/read", map[string]any{"read_through": now()})
	if marked.Code != http.StatusOK || markedBy(t, marked) != 1 {
		t.Errorf("the token's unnarrowed Mark all read: %d %s, want PUB's Reply thread alone marked", marked.Code, strings.TrimSpace(marked.Body.String()))
	}
	if _, unread := threadMark(t, seed.database, seedAlice, hiddenThread); !unread {
		t.Errorf("the token's unnarrowed Mark all read marked alice's PRIV thread read")
	}
	if _, unread := threadMark(t, seed.database, seedAlice, visibleThread); unread {
		t.Errorf("the token's unnarrowed Mark all read left alice's PUB thread unread")
	}
	dismissed := decodeBody[struct {
		Dismissed int `json:"dismissed"`
	}](t, seed.request(t, caller, http.MethodPost, "/api/v1/me/inbox/dismiss", map[string]any{"dismiss_through": now()}))
	if dismissed.Dismissed != 2 {
		t.Errorf("the token's unnarrowed dismissal dismissed %d threads, want PUB's Reply thread and %s's alone", dismissed.Dismissed, pubAgent)
	}
	if at := threadDismissal(t, seed.database, seedAlice, hiddenThread); at != nil {
		t.Errorf("the token's unnarrowed dismissal dismissed alice's PRIV thread at %v", *at)
	}
	if at := agentThreadDismissal(t, seed.database, seedAlice, agentRoots[privAgent]); at != nil {
		t.Errorf("the token's unnarrowed dismissal dismissed alice's thread with %s at %v", privAgent, *at)
	}
}

// holdIssueOwnerLock begins a transaction on the seed's pool that takes issue's owner lock, as every
// writer of the issue takes it, and answers it; it is rolled back when the test ends, if nothing
// let it go before.
func holdIssueOwnerLock(t *testing.T, seed privateSeed, issue string) heldLock {
	t.Helper()
	ctx := context.Background()
	tx, err := seed.database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin %s's writer: %v", issue, err)
	}
	t.Cleanup(func() { _ = tx.Rollback(ctx) })
	var pid int32
	if err := tx.QueryRow(ctx, `select pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatalf("read the writer's backend: %v", err)
	}
	if err := store.LockOwner(ctx, tx, store.IssueOwner(issue)); err != nil {
		t.Fatalf("hold %s's owner lock: %v", issue, err)
	}
	return heldLock{tx: tx, pid: pid}
}
