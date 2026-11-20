package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// Two broadcasts to the same two sessions, named in opposite orders, both commit. Each copy hangs
// from its recipient's page, and a broadcast takes every recipient's page in one node write, in
// ascending (kind, id), before any copy (writeBroadcast), so the two take the pages in one order.
//
// The pages are there first - a broadcast to both made them - and a transaction holds the page
// that sorts first, so the two queue behind it one after the other: first the broadcast naming
// that page first, then the one naming it second, each on a server whose store is one connection
// of its own, so the backend each runs on is known. A write that took each page with its copy, in
// the order its request names its recipients, would have the second hold the other page by then,
// and once the hold went the first would take the held page and wait on the second, which waits on
// it: Postgres would end one of them (40P01) and its request would fail.
func TestBroadcastsNamingTheirRecipientsInOppositeOrdersBothCommit(t *testing.T) {
	ctx := context.Background()
	listener, _ := newBroadcastListener(t, broadcastSessions)
	handler, database, deps := newTestServer(t, testServerOptions{envoyURL: listener.URL})
	made := postBroadcast(t, handler, map[string]any{
		"body": "Pages first.", "delivery": "steer", "session_ids": []string{"planner", "tester"},
	}, "alice")
	if made.Code != http.StatusCreated {
		t.Fatalf("broadcast to planner and tester: status=%d body=%s", made.Code, made.Body.String())
	}
	awaitBroadcastDeliveries(t, handler, decodeBody[broadcastResponse](t, made).ID)

	holder, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the holder: %v", err)
	}
	defer holder.Rollback(context.Background())
	first := graphstore.SessionPageID("planner")
	if tag, err := holder.Exec(ctx, `select 1 from graph_nodes where kind = $1 and id = $2 for update`, graphstore.KindPage, first); err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("hold page %s: %d rows, %v", first, tag.RowsAffected(), err)
	}

	var answers []chan *httptest.ResponseRecorder
	for _, order := range [][]string{{"planner", "tester"}, {"tester", "planner"}} {
		one := storetest.SingleConnection(t, database)
		sender := deps
		sender.Store = one
		mux := http.NewServeMux()
		t.Cleanup(Register(mux, t.Context(), sender).Wait)
		conn, err := one.Pool.Acquire(ctx)
		if err != nil {
			t.Fatalf("acquire the sender's connection: %v", err)
		}
		pid := conn.Conn().PgConn().PID()
		conn.Release()
		answered := make(chan *httptest.ResponseRecorder, 1)
		answers = append(answers, answered)
		go func() {
			answered <- postBroadcast(t, mux, map[string]any{
				"body": "Report in.", "delivery": "steer", "session_ids": order,
			}, "alice")
		}()
		if answer, early := storetest.WaitForQueuedBehind(t, holder, pid, answered, storeStepWait); early {
			t.Fatalf("the broadcast to %v answered %d without waiting on page %s: %s", order, answer.Code, first, answer.Body.String())
		}
	}
	if err := holder.Rollback(ctx); err != nil {
		t.Fatalf("let page %s go: %v", first, err)
	}
	for index, answered := range answers {
		select {
		case answer := <-answered:
			if answer.Code != http.StatusCreated {
				t.Errorf("broadcast %d once the page went: status=%d body=%s, want both committed", index+1, answer.Code, answer.Body.String())
				continue
			}
			awaitBroadcastDeliveries(t, handler, decodeBody[broadcastResponse](t, answer).ID)
		case <-time.After(storeStepWait):
			t.Fatalf("broadcast %d did not answer within %s of the page going", index+1, storeStepWait)
		}
	}
}
