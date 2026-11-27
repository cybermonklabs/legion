package oauth2call

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestAskedURLNamesAURLAsUnansweredNamesIt: the daemon names the URL of a request that got an
// answer through AskedURL, and of one that got none through Unanswered, so the URL has one name
// either way. A URL with made-up credentials in its user information, its query and its fragment
// prints its scheme, its authority as redact.URLWithCredentials and its path (asked), the name
// Unanswered gives a request of that URL that got no answer (unanswered); the URL as it came holds
// the credentials (twin). Captures oauth2call.asked-url.
func TestAskedURLNamesAURLAsUnansweredNamesIt(t *testing.T) {
	t.Parallel()
	raw := "https://" + redacttest.User + ":" + redacttest.Password + "@cache.example:8418/widgets/healthz?sig=" + redacttest.Token + "#" + redacttest.Token
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("url.Parse(%q): %v", raw, err)
	}
	const want = "https://" + redact.URLWithCredentials + "/widgets/healthz"
	t.Run("twin", func(t *testing.T) {
		t.Parallel()
		if !strings.Contains(u.String(), redacttest.Token) || !strings.Contains(u.String(), redacttest.Password) {
			t.Fatalf("the URL as it came is %q; want the credentials in it, so the row can see them printed", u)
		}
	})
	t.Run("asked", func(t *testing.T) {
		t.Parallel()
		said := AskedURL(u)
		redacttest.RequireNone(t, "oauth2call.asked-url", said)
		if said != want {
			t.Errorf("AskedURL(%q) = %q, want %q", raw, said, want)
		}
	})
	t.Run("unanswered", func(t *testing.T) {
		t.Parallel()
		request := &http.Request{Method: http.MethodGet, URL: u}
		failed := Unanswered(&url.Error{Op: "Get", URL: raw, Err: errors.New("connection refused")}, request, "")
		if !strings.Contains(failed.Error(), `"`+AskedURL(u)+`"`) {
			t.Errorf("Unanswered names the request %q, want it named %q as AskedURL names it", failed, AskedURL(u))
		}
	})
}
