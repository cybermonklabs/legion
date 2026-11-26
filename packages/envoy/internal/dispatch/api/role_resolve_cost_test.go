package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// contextStatements counts the statements sent on a context that carries contextStatementsKey,
// so a test counts one call's own statements and none of the server's background work.
type contextStatements struct{ sent atomic.Int64 }

type contextStatementsKey struct{}

func (c *contextStatements) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	if ctx.Value(contextStatementsKey{}) != nil {
		c.sent.Add(1)
	}
	return ctx
}

func (c *contextStatements) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// TestARoleHeldByAHiddenSessionCostsWhatAnUnheldRoleCosts: a role whose holder the caller may not
// see answers as a role no session holds, and it costs what that role costs: the same statements,
// so the time a resolve takes does not tell a caller that a session it cannot see holds the role.
// role:reviewer is held by session-priv, which has acted in PRIV alone; role:idle is held by no
// session. Each is resolved under alice's reader (an open member who does not see PRIV), and with
// no reader at all, as the delivery sweep resolves.
//
// Control: the visibility reads run only when the listener names a holder; the unheld role then
// costs fewer statements, and the test fails naming each count.
func TestARoleHeldByAHiddenSessionCostsWhatAnUnheldRoleCosts(t *testing.T) {
	const sessions = `[{"session_id":"session-priv","title":"Private","capabilities":["btw","steer"],"last_seen":300}]`
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/roles/reviewer":
			_, _ = w.Write([]byte(`{"role":"reviewer","holder":"session-priv","title":"Private","capabilities":["btw","steer"]}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/roles/idle":
			// The listener's answer for a role no session holds (dispatchenvoy.Client.Role).
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"no holder for role idle"}`))
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/registry/"):
			writeSessionRecord(t, w, sessions, strings.TrimPrefix(r.URL.Path, "/v1/registry/"))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sessions":
			_, _ = w.Write([]byte(sessions))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(listener.Close)
	counter := &contextStatements{}
	seed := seedPrivateProjectWith(t, testServerOptions{envoyURL: listener.URL, store: storetest.TracedStore(t, storetest.Open(t), counter)})
	seed.asSession(t, sessionOfPrivate, seedPrivate, http.MethodPost, "/api/v1/issues/"+seed.issue[seedPrivate]+"/comments",
		map[string]any{"body": "Done on PRIV."}, http.StatusCreated)
	alice, err := seed.server.readerFor(t.Context(), model.BrowserActor(seedAlice))
	if err != nil {
		t.Fatalf("resolve what alice sees: %v", err)
	}
	resolve := func(ctx context.Context, role string) (int64, string) {
		t.Helper()
		before := counter.sent.Load()
		_, _, refusal, _ := seed.server.resolveDeliveryTarget(context.WithValue(ctx, contextStatementsKey{}, true), model.Route{Kind: "role", ID: role}, "btw")
		return counter.sent.Load() - before, strings.ReplaceAll(refusal, role, "<role>")
	}
	for name, ctx := range map[string]context.Context{
		"alice's reader": withReader(t.Context(), alice),
		"no reader":      t.Context(),
	} {
		held, heldAnswer := resolve(ctx, "reviewer")
		unheld, unheldAnswer := resolve(ctx, "idle")
		if heldAnswer != unheldAnswer {
			t.Errorf("under %s, role:reviewer answers %q and role:idle %q, want one answer", name, heldAnswer, unheldAnswer)
		}
		if held == 0 {
			t.Errorf("under %s, role:reviewer counted no statement, so the comparison measures nothing", name)
		}
		if held != unheld {
			t.Errorf("under %s, role:reviewer, held by a session of PRIV, costs %d statements and role:idle, held by none, %d: want the same", name, held, unheld)
		}
	}
}
