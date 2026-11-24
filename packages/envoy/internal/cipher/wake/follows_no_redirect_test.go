package wake

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
	"github.com/sjawhar/envoy/redirects/redirectstest"
)

// TestANoticeFollowsNoRedirect: whatever answers at Envoy's address answers the notice, which
// carries the broker's account token, with a 302, a 307 or a 308 whose Location names a stand-in
// on another host and quotes the token in its query. A client that follows the 302 asks that
// Location, and one that follows the 307 or the 308 sends the notice there again. The stand-in is
// sent nothing, so no request carries the token on to it, and Notify logs the 3xx as the status it
// is: the notice's client follows no redirect. The twins show a client that follows each of them
// sends the stand-in the token, so the rows can see one sent on. Captures
// wake.redirect-followed/given (the 302) and wake.redirect-followed/given-<status>, what the
// stand-in was sent.
func TestANoticeFollowsNoRedirect(t *testing.T) {
	const token = "hb7qe-not-a-secret-RDX-3f1a"
	target, sent := redirectstest.Sent(t)
	location := func(bearer string) string { return redirectstest.To(target, bearer) }
	// moved is the stand-in at Envoy's address for the 307 and 308 rows: redirectstest.EchoBearer's,
	// answering status where that one answers 302.
	moved := func(t *testing.T, status int) string {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Location", location(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")))
			w.WriteHeader(status)
		}))
		t.Cleanup(server.Close)
		return server.URL
	}
	t.Run("twin", func(t *testing.T) {
		request, err := http.NewRequest(http.MethodPost, redirectstest.EchoBearer(t, location)+"/v1/messages/send", nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+token)
		redirectstest.RequireSentOn(t, request, token, sent)
	})
	t.Run("given", func(t *testing.T) {
		var logged bytes.Buffer
		logtest.SetDefault(t, slog.NewTextHandler(&logged, nil))
		Envoy{URL: redirectstest.EchoBearer(t, location), Account: staticAccount(token), HTTP: &http.Client{}}.Notify(context.Background(), "sess-123", "req-456", "approved")
		text := sent()
		redacttest.RequireNoPiece(t, "wake.redirect-followed/given", text, token)
		if text != "" {
			t.Errorf("the stand-in the Location names was asked: %q", text)
		}
		if !strings.Contains(logged.String(), "status=302") {
			t.Errorf("Notify logged no send refused by its 302: %q", logged.String())
		}
	})
	for _, status := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		code := strconv.Itoa(status)
		t.Run("twin-"+code, func(t *testing.T) {
			request, err := http.NewRequest(http.MethodPost, moved(t, status)+"/v1/messages/send", strings.NewReader("{}"))
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("Authorization", "Bearer "+token)
			redirectstest.RequireSentOn(t, request, token, sent)
		})
		t.Run("given-"+code, func(t *testing.T) {
			var logged bytes.Buffer
			logtest.SetDefault(t, slog.NewTextHandler(&logged, nil))
			Envoy{URL: moved(t, status), Account: staticAccount(token), HTTP: &http.Client{}}.Notify(context.Background(), "sess-123", "req-456", "approved")
			text := sent()
			redacttest.RequireNoPiece(t, "wake.redirect-followed/given-"+code, text, token)
			if text != "" {
				t.Errorf("the stand-in the Location names was asked: %q", text)
			}
			if !strings.Contains(logged.String(), "status="+code) {
				t.Errorf("Notify logged no send refused by its %d: %q", status, logged.String())
			}
		})
	}
}
