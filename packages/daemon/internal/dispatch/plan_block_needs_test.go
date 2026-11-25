package dispatch

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
)

// TestAPlanDocumentHandsOnNoNeedsValueThatEchoesTheBearer: whatever answers in Dispatch's place
// answers a plan version whose task names, as the second value of its needs, a reference that
// echoes the bearer the read carried; the bearer holds the probe's token, the marker the gate
// reads. What PlanDocument answers, the document or its refusal, holds no run of the bearer: it
// refuses the block, naming its place among the typed blocks (a callout before it, whose id no
// plan reads, is the first) and the value's place among the needs, where it handed the value on
// in the block's attributes to any caller that prints them. The twin shows the stand-in's answer
// holds the echo. Captures dispatch.answer-words/plan-block-needs.
func TestAPlanDocumentHandsOnNoNeedsValueThatEchoesTheBearer(t *testing.T) {
	const bearer = "q7v2-" + oauth2calltest.Token + "-x9k3"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/artifacts/plan-1" {
			writeJSON(t, w, map[string]any{"id": "plan-1", "issue_key": "LEGION-12", "slug": "plan-md"})
			return
		}
		needs := "dispatch://LEGION-12/artifact/plan-md#b-b dispatch://LEGION-12/artifact/plan-md#b-" +
			strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		callout := ":::callout{#Note-1 kind=\"note\" title=\"\"}\nRead the spec first.\n:::"
		task := ":::plan_task{#a needs=\"" + needs + "\"}\nDo the change.\n:::"
		markdown := callout + "\n\n" + task
		writeJSON(t, w, map[string]any{"markdown": markdown, "number": 3, "blocks": []map[string]any{
			{"id": "Note-1", "type": "callout", "from": 0, "to": len(callout)},
			{"id": "a", "type": "plan_task", "from": len(callout) + 2, "to": len(markdown),
				"attributes": []map[string]string{{"name": "needs", "value": needs, "kind": "block[]"}}},
		}})
	}))
	defer server.Close()
	t.Run("twin", func(t *testing.T) {
		request, err := http.NewRequest(http.MethodGet, server.URL+"/api/v1/artifacts/plan-1/versions/3?blocks=true", nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+bearer)
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if err != nil || !strings.Contains(string(body), bearer) {
			t.Fatalf("the stand-in's answer holds no echo of the bearer (%v), so the row cannot see a leak: %s", err, body)
		}
	})

	document, err := New(server.URL, BearerTransport(bearer, nil)).PlanDocument(context.Background(), "plan-1", 3)

	oauth2calltest.RequireNoPiece(t, "dispatch.answer-words/plan-block-needs", fmt.Sprintf("%+v %v", document, err), bearer)
	if want := "typed block 2 of the document, a plan_task block, has value 2 of its needs holding a run of the credential"; err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("PlanDocument = %+v, %v; want a refusal saying %q", document, err, want)
	}
}
