package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/inboxwrite"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// A reply in a direct conversation, on no issue, reads who takes part in its thread under the
// thread's root (inboxwrite.PlanRecord, inboxwrite.RootWalkLockStatement): no owner lock orders
// the writers of such a thread, so the root does, and a reply that began after another is
// committing reads that one's writer among the thread's people. Alice starts a conversation with
// s1, and bob replies in it; a transaction of the test's holds the writes of messages, so bob's
// reply stops at its insert, holding the root it took before it read the thread. s1's reply to
// alice's message is then sent and has to wait at the root, before it reads who takes part, not
// read the thread without bob and wait later at its own insert. Once the hold goes, bob's reply
// commits first, and s1's reaches bob.
func TestADirectReplyReadsWhoTakesPartUnderTheThreadsRoot(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	live := true
	sent := []map[string]any{}
	listener := sessionListener(t, &live, &sent)
	t.Cleanup(listener.Close)
	handler, database, deps := newTestServer(t, testServerOptions{envoyURL: listener.URL})
	created := postAgentMessage(t, handler, "s1", map[string]any{"body": "Where is the dashboard?", "delivery": "aside"}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("alice's direct message: status=%d body=%s", created.Code, created.Body.String())
	}
	root := decodeBody[model.Message](t, created)

	holder, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the holder: %v", err)
	}
	t.Cleanup(func() { _ = holder.Rollback(context.Background()) })
	if _, err := holder.Exec(ctx, `lock table messages in share mode`); err != nil {
		t.Fatalf("hold the writes of messages: %v", err)
	}
	bobServer, bobBackend := oneConnectionHandler(t, deps)
	sessionServer, sessionBackend := oneConnectionHandler(t, deps)
	bobAnswer := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		bobAnswer <- postAgentMessage(t, bobServer, "s1", map[string]any{
			"body": "I can show you.", "delivery": "aside", "in_reply_to": root.ID,
		}, "bob")
	}()
	if answer, answered := storetest.WaitForQueuedBehind(t, holder, bobBackend, bobAnswer); answered {
		t.Fatalf("bob's reply answered %d before it reached the hold: %s", answer.Code, answer.Body.String())
	}
	sessionAnswer := make(chan *httptest.ResponseRecorder, 1)
	go func() { sessionAnswer <- replyTo(t, sessionServer, root.ID, "On it.") }()
	if answer, answered := storetest.WaitForQueuedBehind(t, holder, sessionBackend, sessionAnswer); answered {
		t.Fatalf("s1's reply answered %d while bob's had not committed: %s", answer.Code, answer.Body.String())
	}
	var waiting string
	if err := database.Pool.QueryRow(ctx, `select query from pg_stat_activity where pid = $1`, sessionBackend).Scan(&waiting); err != nil {
		t.Fatalf("read the statement s1's reply waits at: %v", err)
	}
	if waiting != inboxwrite.RootWalkLockStatement {
		t.Errorf("s1's reply waits at %q, want the take of its thread's root, before it reads who takes part (inboxwrite.RootWalkLockStatement)", normalizedStatement(waiting))
	}
	if err := holder.Rollback(ctx); err != nil {
		t.Fatalf("let bob's reply go: %v", err)
	}
	answers := [2]*httptest.ResponseRecorder{}
	for index, answered := range []chan *httptest.ResponseRecorder{bobAnswer, sessionAnswer} {
		select {
		case answers[index] = <-answered:
		case <-time.After(storeStepWait):
			t.Fatalf("reply %d did not answer within %s of the hold going", index+1, storeStepWait)
		}
	}
	if answers[0].Code != http.StatusCreated || answers[1].Code != http.StatusCreated {
		t.Fatalf("bob's reply answered %d (%s) and s1's %d (%s), want both written",
			answers[0].Code, answers[0].Body.String(), answers[1].Code, answers[1].Body.String())
	}
	reply := decodeBody[model.Message](t, answers[1]).ID
	if reached := meInboxDeliveredTo(t, database, graphstore.KindMessage, reply); !slices.Contains(reached, graphstore.KindPerson+":bob") {
		t.Errorf("s1's reply reached %v, want bob, whose reply in the thread committed before s1's read who takes part", reached)
	}
}

// A reply on no issue reads who takes part in its thread only once it holds the thread's root
// (inboxwrite.PlanRecord, inboxwrite.RootWalkLockStatement), so a walk that reaches a root no row
// holds refuses the reply rather than read them under no lock that orders it against the
// thread's other writers. Alice starts a conversation with s1, and the store is made to hold a
// replies_to from her message to a message no row is, which no write of this release states:
// bob's reply in the conversation answers 500 and stores nothing.
func TestADirectReplyWhoseThreadsRootHoldsNoRowIsRefused(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	live := true
	sent := []map[string]any{}
	listener := sessionListener(t, &live, &sent)
	t.Cleanup(listener.Close)
	handler, database, _ := newTestServer(t, testServerOptions{envoyURL: listener.URL})
	created := postAgentMessage(t, handler, "s1", map[string]any{"body": "Where is the dashboard?", "delivery": "aside"}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("alice's direct message: status=%d body=%s", created.Code, created.Body.String())
	}
	root := decodeBody[model.Message](t, created)

	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the write of the replies_to: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if err := graphstore.Write(ctx, tx, nil, []graphstore.Tuple{{
		FromKind: graphstore.KindMessage, FromID: root.ID, Kind: graphstore.TupleRepliesTo,
		ToKind: graphstore.KindMessage, ToID: uuid.NewString(), At: time.Now().UTC(),
		Who: model.Actor{Kind: "user", ID: "alice"}, SourceEvent: graphstore.NoEvent, Asserted: true,
	}}); err != nil {
		t.Fatalf("state a replies_to from alice's message to a message no row is: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the replies_to: %v", err)
	}

	refused := postAgentMessage(t, handler, "s1", map[string]any{
		"body": "I can show you.", "delivery": "aside", "in_reply_to": root.ID,
	}, "bob")
	if refused.Code != http.StatusInternalServerError {
		t.Fatalf("bob's reply under a root no row holds answered %d (%s), want 500: it cannot take the root before it reads who takes part",
			refused.Code, refused.Body.String())
	}
	var replies int
	if err := database.Pool.QueryRow(ctx, `select count(*) from messages where in_reply_to = $1::uuid`, root.ID).Scan(&replies); err != nil {
		t.Fatalf("count the replies to alice's message: %v", err)
	}
	if replies != 0 {
		t.Errorf("the refused reply left %d reply row(s) to alice's message, want none", replies)
	}
}
