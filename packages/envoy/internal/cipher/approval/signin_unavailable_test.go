package approval

import (
	"bytes"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/logtest"
)

// The sign-in-unavailable page, which any browser that starts a sign-in reads, and its log line
// name the issuer through redact.URL: an issuer holding a made-up token in its query, its fragment
// or, percent-encoded, its path prints none of it. config.Load refuses the first two at boot
// (oidc.IssuerRefusal); the page holds whatever it is given. A plain issuer is named as written.
func TestTheSignInUnavailablePageNamesNoCredentialOfTheIssuer(t *testing.T) {
	const token = "madeUpIssuerToken0123456789"
	for name, issuer := range map[string]string{
		"a token in the query":         "https://accounts.example.com/?token=" + token,
		"a token in the fragment":      "https://accounts.example.com/#" + token,
		"an encoded token in the path": "https://accounts.example.com/%6D" + token,
	} {
		t.Run(name, func(t *testing.T) {
			var logged bytes.Buffer
			logtest.SetDefault(t, slog.NewTextHandler(&logged, nil))
			page := &Page{cfg: Config{SignIn: SignIn{Issuer: issuer}}}
			w := httptest.NewRecorder()
			page.writeSignInUnavailable(w, errors.New("the provider did not answer"))
			if w.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503", w.Code)
			}
			for what, text := range map[string]string{"page": w.Body.String(), "log line": logged.String()} {
				if strings.Contains(text, token[:10]) {
					t.Errorf("the %s holds part of the issuer's token: %s", what, text)
				}
			}
		})
	}
	page := &Page{cfg: Config{SignIn: SignIn{Issuer: "https://accounts.example.com"}}}
	w := httptest.NewRecorder()
	page.writeSignInUnavailable(w, errors.New("the provider did not answer"))
	if !strings.Contains(w.Body.String(), "https://accounts.example.com") {
		t.Errorf("a plain issuer is not named on the page: %s", w.Body.String())
	}
}
