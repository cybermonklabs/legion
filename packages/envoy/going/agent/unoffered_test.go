package agent

import (
	"bytes"
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/going"
	"github.com/sjawhar/envoy/going/agentwire"
	"github.com/sjawhar/envoy/internal/contracts"
)

// TestTheBarsModelIsNotOfferedDispatchAPI: dispatch_api, a generic read of any route by its path,
// is a tool of Dispatch the bar does not offer its model (going.UnofferedTools). find_tools answers
// it for neither its name nor a need in its words, and answers from exactly the tools it would
// answer from were dispatch_api no tool of Dispatch, so every answer of it is that answer and the
// list leaves out that tool alone. call_tool and propose_plan refuse it by name with its reason
// before any call, and the bar reads the refusal as the model's slip.
func TestTheBarsModelIsNotOfferedDispatchAPI(t *testing.T) {
	const api = "dispatch_api"
	reason := going.UnofferedTools[api]
	if _, known := dispatchTool(api); !known || reason == "" {
		t.Fatalf("%s is a tool of Dispatch %t, and going.UnofferedTools gives it the reason %q, want a tool with a reason", api, known, reason)
	}

	w := &worker{}
	for _, need := range []string{api, "read one route of Dispatch's HTTP API by its path"} {
		told, refused := w.findTools(t.Context(), 0, json.RawMessage(`{"need":`+strconv.Quote(need)+`}`))
		list, _, _ := strings.Cut(told, "\n[")
		var specs []foundSpec
		if err := json.Unmarshal([]byte(list), &specs); refused || err != nil || len(specs) == 0 {
			t.Fatalf("find_tools answered %s for the need %q (refused %t, %v), want a list of tools", note(told), need, refused, err)
		}
		if strings.Contains(told, api) {
			t.Errorf("find_tools answered %s for the need %q, want no %s", note(told), need, api)
		}
	}

	absent := findablesOf(slices.DeleteFunc(slices.Clone(contracts.DispatchTools), func(tool contracts.DispatchTool) bool { return tool.Name == api }))
	sameTool := func(a, b findable) bool {
		return a.name == b.name && a.description == b.description && bytes.Equal(a.schema, b.schema) && a.text == b.text
	}
	if found := findableTools(); !slices.EqualFunc(found, absent, sameTool) {
		names := func(tools []findable) []string {
			out := make([]string, len(tools))
			for at, tool := range tools {
				out[at] = tool.name
			}
			return out
		}
		t.Errorf("find_tools answers from %v, want the tools it would answer from were %s no tool of Dispatch: %v", names(found), api, names(absent))
	}

	server, client := newFakeServer(t, func(agentwire.CallRequest) agentwire.CallAnswer {
		return agentwire.CallAnswer{Status: http.StatusOK, Body: []byte(`{}`)}
	})
	a := &agent{client: client, held: map[string]*worker{}, claimed: make(chan struct{}, 1), exit: make(chan struct{})}
	w = newWorker(t.Context(), a, &agentwire.ClaimedRun{ID: "run-1", Level: "standard", LeaseToken: "lease-1"}, func() {})
	arguments := map[string]any{"path": "/api/v1/issues/DEMO-7/events"}
	told, refused := w.callTool(t.Context(), 3, json.RawMessage(`{"tool":"dispatch_api","arguments":{"path":"/api/v1/issues/DEMO-7/events"}}`))
	if !refused || !strings.HasPrefix(told, noSuchTool+` "dispatch_api"`) || !strings.Contains(told, reason) {
		t.Errorf("call_tool of %s told %q (refused %t), want it refused by name with the reason %q", api, told, refused, reason)
	}
	if words, slip := slipWords(namedLabel(api), arguments, told); !slip || words != unknownToolLabel.Past {
		t.Errorf("call_tool's refusal of %s reads as %q (slip %t), want the slip %q", api, words, slip, unknownToolLabel.Past)
	}
	told, refused = w.proposePlan(t.Context(), 4, json.RawMessage(`{"summary":"Read the events.","changes":[{"tool":"dispatch_api","arguments":{"path":"/api/v1/issues/DEMO-7/events"}}]}`))
	if !refused || !strings.Contains(told, `"dispatch_api"`) || !strings.Contains(told, reason) {
		t.Errorf("propose_plan of %s told %q (refused %t), want it refused by name with the reason %q", api, told, refused, reason)
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	if len(server.calls) != 0 {
		t.Errorf("call_tool and propose_plan of %s made the calls %+v, want none", api, server.calls)
	}
}
