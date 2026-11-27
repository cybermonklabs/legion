package refs_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/api"
	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

func init() { refs.SeedStoreThroughItsWriters = seedStoreThroughItsWriters }

// seedStoreThroughItsWriters writes the rows that address the people and agents of a running
// deployment - a comment a person wrote, a comment a session wrote and an issue routed to a
// role - and then runs the writer that states the store from such rows, graphcensus's backfill:
// the person, the session, the role and the role's page are each a node that writer makes from
// one of those rows, as it makes them for a database written before the store existed. It then
// sends a message to that session through the API's own handler, whose request writes the
// message's delivery attempt and the attempt's node, as a running deployment's does
// (sendTargetedMessage). No kind is named here; which kinds the store then holds is what the
// census reads from graph_nodes.
func seedStoreThroughItsWriters(t *testing.T, ctx context.Context, database *store.Store) {
	t.Helper()
	for _, statement := range []string{
		`insert into projects (key, name, default_assignee) values ('CENSUS', 'Census project', 'alice')`,
		`insert into issues (key, project_key, number, title, created_by, rank, route)
		 values ('CENSUS-1', 'CENSUS', 1, 'Census issue', '{"kind":"user","id":"alice"}', 'U', 'role:census-role')`,
		`insert into comments (issue_key, author, body)
		 values ('CENSUS-1', '{"kind":"user","id":"alice"}', 'A person''s remark.')`,
		`insert into comments (issue_key, author, body)
		 values ('CENSUS-1', '{"kind":"session","id":"census-session"}', 'A session''s remark.')`,
	} {
		if _, err := database.Pool.Exec(ctx, statement); err != nil {
			t.Fatalf("seed the rows that address people and agents: %v", err)
		}
	}
	if _, err := graphcensus.Backfill(ctx, database, io.Discard); err != nil {
		t.Fatalf("state the store from those rows through its backfill: %v", err)
	}
	sendTargetedMessage(t, database, "CENSUS-1", "census-session")
}

// sendTargetedMessage posts one issue message to session as alice through the API's own route,
// POST /api/v1/issues/{key}/messages, on a server whose stand-in Envoy listener knows that session
// and takes every send to it: the request stores the message and its first delivery attempt, and
// the send that follows settles that attempt sent, which is the path a running deployment's
// delivery takes. The attempt is what the census needs a row of.
func sendTargetedMessage(t *testing.T, database *store.Store, key, session string) {
	t.Helper()
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/registry/"+session:
			_, _ = w.Write([]byte(`{"session_id":"` + session + `","title":"census","capabilities":["steer"]}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/send":
			_, _ = w.Write([]byte(`{"event_id":"census-envelope","recipient":"` + session + `"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"no live session"}`))
		}
	}))
	t.Cleanup(listener.Close)
	broker := events.NewBroker()
	documents := docs.New(docs.Deps{Store: database, Events: broker, ServerURL: "https://dispatch.example", Settle: 20 * time.Millisecond})
	t.Cleanup(func() {
		if err := documents.Shutdown(context.Background()); err != nil {
			t.Errorf("shut down the document service: %v", err)
		}
	})
	people := store.NewPgPeopleStore(database.Pool, auth.SingleSigningKey("signing-key"), nil)
	if err := people.Record(context.Background(), "alice"); err != nil {
		t.Fatalf("record alice: %v", err)
	}
	deps, err := api.NewDeps(api.DepsInput{
		Store:     database,
		Identity:  identity.HeaderIdentity{Header: "X-Dispatch-User", People: people},
		ServerURL: "https://dispatch.example",
		EnvoyURL:  listener.URL,
		Docs:      documents,
		Events:    broker,
	})
	if err != nil {
		t.Fatalf("API dependencies: %v", err)
	}
	mux := http.NewServeMux()
	// t.Context, the server's lifetime, is cancelled before cleanups run, so Wait returns once
	// the work the server started has ended, before the database closes under it.
	t.Cleanup(api.Register(mux, t.Context(), deps).Wait)
	body, err := json.Marshal(map[string]any{"body": "A message to the census session.", "target": "session:" + session, "delivery": "steer"})
	if err != nil {
		t.Fatalf("encode the message: %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/issues/"+key+"/messages", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Dispatch-User", "alice")
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("POST /api/v1/issues/%s/messages: status=%d body=%s, want %d", key, response.Code, response.Body.String(), http.StatusCreated)
	}
	var state string
	if err := database.Pool.QueryRow(context.Background(), `
		select d.state from message_deliveries d join messages m on m.id = d.message_id
		where m.issue_key = $1 and d.node_kind is not null
	`, key).Scan(&state); err != nil {
		t.Fatalf("read the message's delivery attempt: %v", err)
	}
	if state != "sent" {
		t.Fatalf("the message's delivery attempt is %s, want sent: the stand-in listener takes every send to %s", state, session)
	}
}
