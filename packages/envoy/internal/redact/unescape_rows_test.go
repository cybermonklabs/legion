package redact_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"golang.org/x/oauth2"

	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestTheBoundaryReadsAnAnswersBodyWithItsReferencesDecoded: an issuer's refusal is an HTML page
// that writes a URL's separators as character references, once or twice over (&amp;quest;). The
// boundary reads the body as a reader of the page sees it (redact.UnescapeHTML, until none is
// left) before the rule, so OAuth2's answer prints none of the URL's credentials (the review of
// #4368 at fd07192703).
func TestTheBoundaryReadsAnAnswersBodyWithItsReferencesDecoded(t *testing.T) {
	for _, row := range []struct{ name, body string }{
		{"query-once", "<p>Continue at https://idp.example/cb&quest;session&equals;" + redacttest.Token + " now</p>"},
		{"query-twice", "<p>Continue at https://idp.example/cb&amp;quest;session&amp;equals;" + redacttest.Token + " now</p>"},
		{"at-twice", "<p>clone https://" + redacttest.Token + "&amp;commat;github.example/o/r failed</p>"},
	} {
		t.Run(row.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/html")
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, row.body)
			}))
			defer srv.Close()
			cfg := &oauth2.Config{ClientID: "probe-client", Endpoint: oauth2.Endpoint{TokenURL: srv.URL, AuthStyle: oauth2.AuthStyleInParams}}
			_, err := redact.OAuth2(cfg.Exchange(context.Background(), "the-code"))
			if err == nil {
				t.Fatal("OAuth2 answered no error")
			}
			redacttest.RequireNone(t, "redact.body-references/"+row.name, err.Error())
		})
	}
}
