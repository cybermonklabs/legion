package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// TestACommentInAnAskThreadTakesItsAuthorsNodesInAscendingOrder is this change's R17
// over the comment path's two node writes. A comment by a session in an ask's thread makes two
// nodes of the store: the session that wrote it, and - for a credential the secrets broker
// scoped to a person - that person, whom the session is an alias of. The author's own write
// takes the pair in one statement, in ascending (kind, id), person before session. The follow
// of the ask writes the session alone, so a follow before that write would take the pair in the
// other order, and a write of the same two nodes running beside it would hold the one it waits
// for: a 40P01 on a new session whose owner the store holds no person of yet.
//
// The interleaving is arranged, not raced for. Another transaction writes the person's node and
// stops there; the comment is started and waited on until its own backend has queued behind
// that row; then that transaction writes the session's node. With the comment path's nodes
// taken in one order the session's row is free, because the comment is waiting at the person's
// row and has written no node after it. With the follow first, the comment holds the session
// and waits for the person while that transaction waits for the session, and Postgres breaks
// the cycle.
func TestACommentInAnAskThreadTakesItsAuthorsNodesInAscendingOrder(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	listener := newQueueingListener(t, map[string]fakeSession{
		brokerTestSession: {title: "alice's agent", capabilities: queuedModes},
	})
	rig := newIdentityRigOn(t, listener.url)
	caller := newBrokerCaller(t)
	const token = "a-token-proving-the-ask-threads-session"
	rig.broker.Says(token, liveNamedToken(caller.jkt))
	issue := rig.seedRoutedIssue(t)
	ask := openAskAs(t, rig.handler, issue, "the-session-that-asked", "Which option?")

	// The comment runs on a store of one connection, so the backend it waits on is known before
	// it starts and no other session queued behind the same row is taken for it.
	call := storetest.SingleConnection(t, rig.database)
	callDeps := rig.deps
	callDeps.Store = call
	routes := http.NewServeMux()
	t.Cleanup(Register(routes, t.Context(), callDeps).Wait)
	backend := singleConnectionBackend(t, ctx, call)

	// Neither node is in the store before this: the person has written nothing, and the broker's
	// session has not been recorded. Both are new, which is the case the order has to survive.
	for _, absent := range []struct{ kind, id string }{
		{graphstore.KindPerson, brokerTestPerson},
		{graphstore.KindSession, brokerTestSession},
	} {
		if _, held, err := graphstore.ReadNode(ctx, rig.database.Pool, absent.kind, absent.id); err != nil || held {
			t.Fatalf("the store already holds %s %s: %t (err %v), so two new nodes are not what this races",
				absent.kind, absent.id, held, err)
		}
	}

	// The other write of the same two nodes, in ascending (kind, id): the person first.
	other, err := rig.database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the write that holds the author's person node: %v", err)
	}
	defer other.Rollback(context.Background())
	who := model.Actor{Kind: "user", ID: brokerTestPerson}
	at := time.Now().UTC()
	if err := graphstore.Write(ctx, other, []graphstore.Node{
		graphstore.PersonNode(brokerTestPerson, who, at, graphstore.NoEvent),
	}, nil); err != nil {
		t.Fatalf("write the author's person node: %v", err)
	}

	commented := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		response := httptest.NewRecorder()
		routes.ServeHTTP(response, caller.presenting(t, http.MethodPost, "/api/v1/issues/"+issue+"/comments", token,
			map[string]any{"body": "The session answers in the ask's thread.", "ask_id": ask}))
		commented <- response
	}()
	if answer, early := storetest.WaitForQueuedBehind(t, other, backend, commented); early {
		t.Fatalf("the comment finished without waiting at the author's person node: status=%d body=%s",
			answer.Code, answer.Body.String())
	}

	// The session's node, the second of the pair. The comment is waiting at the person's row and
	// holds no node after it, so this takes the session's row and commits.
	if err := graphstore.Write(ctx, other, []graphstore.Node{
		graphstore.SessionNode(brokerTestSession, who, at, graphstore.NoEvent),
	}, nil); err != nil {
		t.Fatalf("write the author's session node beside the comment: %v%s", err, orderDeadlockNote(err))
	}
	if err := other.Commit(ctx); err != nil {
		t.Fatalf("commit the write of the author's two nodes: %v", err)
	}
	if response := testwait.Receive(t, "the comment to finish once the other write committed", commented); response.Code != http.StatusCreated {
		t.Fatalf("the comment in the ask's thread: status=%d body=%s", response.Code, response.Body.String())
	}

	// What it left: the session follows the ask, and both nodes are there once.
	var follows bool
	if err := rig.database.Pool.QueryRow(ctx,
		`select exists (select 1 from ask_followers where ask_id = $1::uuid and session_id = $2)`,
		ask, brokerTestSession).Scan(&follows); err != nil {
		t.Fatalf("read whether the session follows the ask: %v", err)
	}
	if !follows {
		t.Errorf("the session that commented in the ask's thread does not follow the ask")
	}
}

// singleConnectionBackend is the backend of a one-connection store's connection, which the call
// under test runs on. It is read before the call starts, because a call that does not wait hands
// its connection back before the test could read it there.
func singleConnectionBackend(t *testing.T, ctx context.Context, single *store.Store) uint32 {
	t.Helper()
	conn, err := single.Pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire the call's connection: %v", err)
	}
	defer conn.Release()
	return conn.Conn().PgConn().PID()
}

// orderDeadlockNote names a 40P01 in a failure, the answer this test exists to keep out of the
// comment path.
func orderDeadlockNote(err error) string {
	var failure *pgconn.PgError
	if errors.As(err, &failure) && failure.Code == "40P01" {
		return " (40P01: the two writes took the author's two nodes in opposite orders)"
	}
	return ""
}
