package api

import (
	"net/http"
	"sync/atomic"
	"testing"
)

// madeUpGadgetRows are made-up rows served in both versions: a PUT and a DELETE of a gadget, and a
// POST of one, each a write any caller may make.
func madeUpGadgetRows() []routeRow {
	put, remove, create := madeUpRow(), madeUpRow(), madeUpRow()
	put.Method, put.Pattern, put.Kind, put.Resource = http.MethodPut, "/gadgets/{id}", kindItem, "gadgets"
	put.Body = requestBody{Kind: bodyJSON, Type: widgetBody{}}
	remove.Method, remove.Pattern, remove.Kind, remove.Resource = http.MethodDelete, "/gadgets/{id}", kindItem, "gadgets"
	create.Method, create.Pattern, create.Resource = http.MethodPost, "/gadgets", "gadgets"
	create.Body = requestBody{Kind: bodyJSON, Type: widgetBody{}}
	rows := []routeRow{put, remove, create}
	for index := range rows {
		rows[index].Scope = scopeDeployment
	}
	return rows
}

// Every write method of a route the rules of /api/v2 bind is keyed (R8, decision 7, keyedMethod):
// a PUT and a DELETE sent twice under one key run once, the repeat answered the first answer's
// body, marked a replay. The same rows' PUT and DELETE under /api/v1 stay unkeyed, so each send
// runs and none is a replay. One key sent to a /api/v1 route and then to its /api/v2 route is two
// requests under one key: the fingerprint holds the route's pattern, so the second is 422
// KEY_REUSED.
func TestAV2PutAndDeleteAreKeyedAndAV1OneIsNot(t *testing.T) {
	_, _, _, served := newRegisteredTestServer(t, testServerOptions{})
	var runs atomic.Int32
	handler := mountMadeUpRows(served, &runs, madeUpGadgetRows()...)
	body := map[string]any{"title": "A gadget"}

	for _, write := range []struct {
		method string
		body   any
	}{{http.MethodPut, body}, {http.MethodDelete, nil}} {
		before := runs.Load()
		first := keyedRequest(t, handler, write.method, "/api/v2/gadgets/g1", write.body, "alice", "gadget-v2-"+write.method)
		again := keyedRequest(t, handler, write.method, "/api/v2/gadgets/g1", write.body, "alice", "gadget-v2-"+write.method)
		if first.Code != http.StatusOK || again.Code != http.StatusOK || replayed(first) || !replayed(again) ||
			again.Body.String() != first.Body.String() {
			t.Errorf("/api/v2 %s twice under one key: %d %s then %d %s (replayed %v), want the second the first's replay",
				write.method, first.Code, first.Body.String(), again.Code, again.Body.String(), replayed(again))
		}
		if ran := runs.Load() - before; ran != 1 {
			t.Errorf("/api/v2 %s twice under one key ran %d times, want once", write.method, ran)
		}

		before = runs.Load()
		first = keyedRequest(t, handler, write.method, "/api/v1/gadgets/g1", write.body, "alice", "gadget-v1-"+write.method)
		again = keyedRequest(t, handler, write.method, "/api/v1/gadgets/g1", write.body, "alice", "gadget-v1-"+write.method)
		if first.Code != http.StatusOK || again.Code != http.StatusOK || replayed(again) {
			t.Errorf("/api/v1 %s twice under one key: %d then %d (replayed %v), want both run unkeyed",
				write.method, first.Code, again.Code, replayed(again))
		}
		if ran := runs.Load() - before; ran != 2 {
			t.Errorf("/api/v1 %s twice under one key ran %d times, want twice: a /api/v1 %s takes no key", write.method, ran, write.method)
		}
	}

	if v1 := keyedRequest(t, handler, http.MethodPost, "/api/v1/gadgets", body, "alice", "gadget-both"); v1.Code != http.StatusOK {
		t.Fatalf("the /api/v1 POST: %d %s", v1.Code, v1.Body.String())
	}
	v2 := keyedRequest(t, handler, http.MethodPost, "/api/v2/gadgets", body, "alice", "gadget-both")
	if code, _ := refusalOf(t, v2); v2.Code != http.StatusUnprocessableEntity || code != "KEY_REUSED" {
		t.Fatalf("the /api/v1 POST's key sent to its /api/v2 route: %d %s, want 422 KEY_REUSED", v2.Code, v2.Body.String())
	}
}
