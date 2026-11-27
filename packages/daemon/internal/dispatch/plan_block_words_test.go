package dispatch

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
	"github.com/sjawhar/legion/daemon/internal/githubtest"
)

// TestAPlanBlockDispatchCannotCutIsRefusedWithoutItsText: Dispatch, or whatever answers in its
// place, answers a plan version whose block cannot be cut from its markdown, with an id and a
// text that echo the bearer the read carried and quote a URL with made-up credentials. The
// refusal names the document, the version and the block by its place in the answer, and none of
// the block's id or text. It printed the id, and the text with %q. The control in each case is
// the refusal itself, which names the block's place.
func TestAPlanBlockDispatchCannotCutIsRefusedWithoutItsText(t *testing.T) {
	bearer := githubtest.InstallationToken("plan read")
	echoed := bearer + " " + oauth2calltest.Words
	unfenced := "the plan " + echoed
	for _, tc := range []struct {
		name, capture, want string
		block               map[string]any
	}{
		{name: "range", capture: "dispatch.answer-words/plan-block-range", want: "block 1 of the answer spans 0 to",
			block: map[string]any{"id": echoed, "type": "plan_task", "from": 0, "to": len(unfenced) + 1}},
		{name: "unfenced", capture: "dispatch.answer-words/plan-block-unfenced", want: "block 1 of the answer is not a fenced block",
			block: map[string]any{"id": echoed, "type": "plan_task", "from": 0, "to": len(unfenced)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(t, w, map[string]any{"markdown": unfenced, "number": 3, "blocks": []map[string]any{tc.block}})
			}))
			defer server.Close()

			_, err := New(server.URL, BearerTransport(bearer, nil)).PlanDocument(context.Background(), "plan-6", 3)

			if err == nil || !strings.Contains(err.Error(), "document plan-6 at version 3") {
				t.Fatalf("PlanDocument = %v, want a refusal naming plan-6 at version 3", err)
			}
			oauth2calltest.RequireNoPiece(t, tc.capture, err.Error(), bearer)
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("PlanDocument = %v, want it to name %q", err, tc.want)
			}
		})
	}
}
