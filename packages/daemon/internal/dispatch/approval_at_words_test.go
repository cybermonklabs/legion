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

// TestAnApprovalWhoseTimeDoesNotParseIsRefusedWithoutItsText: Dispatch, or whatever answers in
// its place, names an artifact's approval time with words that echo the bearer the read carried
// and quote a URL with made-up credentials. time.Parse's error quotes the value it could not
// read, so the refusal names the field and why, and none of the value. It printed the parse
// error. The control is the refusal itself, which names the artifact and the field.
func TestAnApprovalWhoseTimeDoesNotParseIsRefusedWithoutItsText(t *testing.T) {
	bearer := githubtest.InstallationToken("approval read")
	echoed := "at " + bearer + " " + oauth2calltest.Words
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, map[string]any{"id": "artifact-6", "issue_key": "LEGION-6", "approval": map[string]any{
			"state": "approved", "latest_version": 3, "version": 3, "at": echoed}})
	}))
	defer server.Close()

	_, err := New(server.URL, bearer).Approval(context.Background(), "artifact-6")

	if err == nil || !strings.Contains(err.Error(), `artifact "artifact-6"`) || !strings.Contains(err.Error(), "RFC 3339") {
		t.Fatalf("Approval = %v, want a refusal naming the artifact and its approval time", err)
	}
	oauth2calltest.RequireNoPiece(t, "dispatch.answer-words/approval-at", err.Error(), bearer)
}
