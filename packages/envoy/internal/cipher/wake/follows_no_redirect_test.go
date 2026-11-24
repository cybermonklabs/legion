package wake

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
	"github.com/sjawhar/envoy/redirects/redirectstest"
)

// TestANoticeFollowsNoRedirect: whatever answers at Envoy's address answers the notice, which
// carries the broker's account token, with a 302 whose Location names a stand-in on another host
// and quotes the token in its query. The stand-in is sent nothing, so no request carries the token
// on to it, and Notify logs the 302 as the status it is: the notice's client follows no redirect.
// The twin shows a client that follows it sends the stand-in the token, so the row can see one
// sent on. Captures wake.redirect-followed/given, what the stand-in was sent.
func TestANoticeFollowsNoRedirect(t *testing.T) {
	const token = "hb7qe-not-a-secret-RDX-3f1a"
	target, sent := redirectstest.Sent(t)
	location := func(credential string) string {
		return redirectstest.To(target, strings.TrimPrefix(credential, "Bearer "))
	}
	t.Run("twin", func(t *testing.T) {
		request, err := http.NewRequest(http.MethodPost, redirectstest.Echo(t, "Authorization", location)+"/v1/messages/send", nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+token)
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatalf("a client that follows the redirect failed: %v", err)
		}
		_ = response.Body.Close()
		if text := sent(); !strings.Contains(text, token) {
			t.Fatalf("a client that follows the redirect sent the stand-in no token, so the row cannot see one sent on: %q", text)
		}
	})
	t.Run("given", func(t *testing.T) {
		var logged bytes.Buffer
		logtest.SetDefault(t, slog.NewTextHandler(&logged, nil))
		Envoy{URL: redirectstest.Echo(t, "Authorization", location), Account: staticAccount(token), HTTP: &http.Client{}}.Notify(context.Background(), "sess-123", "req-456", "approved")
		text := sent()
		redacttest.RequireNoPiece(t, "wake.redirect-followed/given", text, token)
		if text != "" {
			t.Errorf("the stand-in the Location names was asked: %q", text)
		}
		if !strings.Contains(logged.String(), "status=302") {
			t.Errorf("Notify logged no send refused by its 302: %q", logged.String())
		}
	})
}
