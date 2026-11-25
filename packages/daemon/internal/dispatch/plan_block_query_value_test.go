package dispatch

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
)

// TestAPlanBlockNamedAsTheReadsQueryValueIsHandedOn: the plan read asks ?blocks=true, so its
// record holds the value `true` beside the bearer, and a task may be named `true`, which the
// task-id grammar takes. PlanDocument hands that block on, its id and its steps as Dispatch wrote
// them: `true` is shorter than a run the rule reads as an echo of a credential
// (oauth2call.Echoes), so it is no echo of one.
func TestAPlanBlockNamedAsTheReadsQueryValueIsHandedOn(t *testing.T) {
	const bearer = "q7v2-" + oauth2calltest.Token + "-x9k3"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/artifacts/plan-1" {
			writeJSON(t, w, map[string]any{"id": "plan-1", "issue_key": "LEGION-12", "slug": "plan-md"})
			return
		}
		if r.URL.Query().Get("blocks") != "true" {
			t.Errorf("the plan read asked %q, want ?blocks=true", r.URL.RawQuery)
		}
		task := ":::plan_task{#true steps=\"implementer true\"}\nDo the change.\n:::"
		writeJSON(t, w, map[string]any{"markdown": task, "number": 3, "blocks": []map[string]any{
			{"id": "true", "type": "plan_task", "from": 0, "to": len(task),
				"attributes": []map[string]string{{"name": "steps", "value": "implementer true", "kind": "string"}}},
		}})
	}))
	defer server.Close()

	document, err := New(server.URL, BearerTransport(bearer, nil)).PlanDocument(context.Background(), "plan-1", 3)
	if err != nil || len(document.Blocks) != 1 || document.Blocks[0].ID != "true" {
		t.Fatalf("PlanDocument = %+v, %v; want the one block, named true, handed on", document, err)
	}
}
