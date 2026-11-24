package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// TestADeferredSendResolvesARoleAsItsAuthorMay: a mention or message resolved after its request
// has answered resolves a role's holder under the reader of the person who wrote it. The
// listener names session-priv, which has acted in PRIV alone, as role:reviewer's holder.
//
//   - Under alice's reader (an open member who does not see PRIV) the role answers as one no
//     session holds, as it does inside her request, and under carol's (who sees PRIV) it resolves
//     to session-priv.
//   - The comment mention sender sends on a context of its own, so a queued send carries the
//     reader its comment was written under (mentionSend.reader, mentionSendContext): carol's
//     queued mention reaches session-priv, and alice's does not.
//   - A resolve with no reader at all, the delivery sweep's after a crash, answers a role as one
//     no session holds while the deployment holds a private project: it cannot tell whose
//     mention it is, so it fails closed.
//
// Controls, each on its own: a queued send that drops its reader (carol's queued mention is
// refused), and a resolve with no reader that asks nothing (it reaches session-priv); the test
// fails naming each.
func TestADeferredSendResolvesARoleAsItsAuthorMay(t *testing.T) {
	const sessions = `[{"session_id":"session-priv","title":"Private","capabilities":["btw","steer"],"last_seen":300}]`
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/roles/reviewer":
			_, _ = w.Write([]byte(`{"role":"reviewer","holder":"session-priv","title":"Private","capabilities":["btw","steer"]}`))
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/registry/"):
			writeSessionRecord(t, w, sessions, strings.TrimPrefix(r.URL.Path, "/v1/registry/"))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sessions":
			_, _ = w.Write([]byte(sessions))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(listener.Close)
	seed := seedPrivateProjectWith(t, testServerOptions{envoyURL: listener.URL})
	seed.asSession(t, sessionOfPrivate, seedPrivate, http.MethodPost, "/api/v1/issues/"+seed.issue[seedPrivate]+"/comments",
		map[string]any{"body": "Done on PRIV."}, http.StatusCreated)
	reader := func(login string) model.Actor {
		t.Helper()
		resolved, err := seed.server.readerFor(t.Context(), model.BrowserActor(login))
		if err != nil {
			t.Fatalf("resolve what %s sees: %v", login, err)
		}
		return resolved
	}
	role := model.Route{Kind: "role", ID: "reviewer"}
	resolve := func(ctx context.Context) string {
		t.Helper()
		session, _, refusal, _ := seed.server.resolveDeliveryTarget(ctx, role, "btw")
		if refusal != "" {
			return "refused: " + refusal
		}
		return session
	}

	carol := reader(seedCarol)
	if got := resolve(withReader(t.Context(), carol)); got != sessionOfPrivate {
		t.Fatalf("role:reviewer under carol's reader resolves %q, want %s", got, sessionOfPrivate)
	}
	if got := resolve(mentionSendContext(t.Context(), mentionSend{reader: &carol})); got != sessionOfPrivate {
		t.Errorf("role:reviewer in carol's queued mention resolves %q, want %s: the send dropped her reader", got, sessionOfPrivate)
	}
	alice := reader(seedAlice)
	if got := resolve(withReader(t.Context(), alice)); got == sessionOfPrivate {
		t.Errorf("role:reviewer under alice's reader resolves %s, a session of PRIV, which she does not see", got)
	}
	queued := mentionSend{reader: &alice}
	if got := resolve(mentionSendContext(t.Context(), queued)); got == sessionOfPrivate {
		t.Errorf("role:reviewer in alice's queued mention resolves %s, a session of PRIV: the send dropped her reader", got)
	}
	if got := resolve(t.Context()); got == sessionOfPrivate {
		t.Errorf("role:reviewer resolved with no reader resolves %s, a session of PRIV, while a project is private", got)
	}
}
