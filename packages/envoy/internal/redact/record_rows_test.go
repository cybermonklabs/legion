package redact_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"golang.org/x/oauth2"

	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestARedirectedRefreshPrintsNoCredentialOfAnEarlierHop: a token endpoint answers a refresh with a
// redirect whose Location puts the refresh token in its path. Followed to a port nothing answers
// on, the *url.Error names the Location; followed to a second hop that echoes its own path, the
// answer x/oauth2 makes an error of is that second hop's, a GET that carries no form. Each prints
// none of the refresh token: the failed hop's error carries the record of every hop before it,
// and so does the answer (the red team's redirect probes of #4368 at fd07192703). The refresh runs
// as production code runs x/oauth2, through a RefusalClient and OAuth2, with no scope. The
// designated assertion is redact.redirect/<case>.
func TestARedirectedRefreshPrintsNoCredentialOfAnEarlierHop(t *testing.T) {
	mode := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/echo/") {
			w.Header().Set("Content-Type", "text/plain")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, "no route "+r.URL.Path)
			return
		}
		_ = r.ParseForm()
		switch token := r.PostForm.Get("refresh_token"); mode {
		case "307-location-to-dead-port":
			w.Header().Set("Location", "http://127.0.0.1:1/"+token+"/token")
			w.WriteHeader(http.StatusTemporaryRedirect)
		case "302-second-hop":
			w.Header().Set("Location", "/echo/"+token)
			w.WriteHeader(http.StatusFound)
		}
	}))
	defer srv.Close()
	for _, name := range []string{"307-location-to-dead-port", "302-second-hop"} {
		t.Run(name, func(t *testing.T) {
			mode = name
			config := &oauth2.Config{ClientID: "probe-client", Endpoint: oauth2.Endpoint{TokenURL: srv.URL + "/token", AuthStyle: oauth2.AuthStyleInParams}}
			ctx := context.WithValue(context.Background(), oauth2.HTTPClient, redact.RefusalClient(nil))
			_, err := redact.OAuth2(config.TokenSource(ctx, &oauth2.Token{RefreshToken: redacttest.Token}).Token())
			if err == nil {
				t.Fatal("the refresh answered no error")
			}
			redacttest.RequireNone(t, "redact.redirect/"+name, err.Error())
		})
	}
}

// TestALibraryErrorWithNoRecordPrintsItsClassAlone: an error of a library that hands back only
// text, read by OAuth2 with no scope and holding no failed hop or answer, comes with no record of
// the requests behind it, so nothing says which credentials its words echo: it prints its class
// and no word (the boundary fails closed). The error here is go-oidc's discovery error shape, a
// status line and a body echoing the bearer. The designated assertion is redact.no-record.
func TestALibraryErrorWithNoRecordPrintsItsClassAlone(t *testing.T) {
	library := fmt.Errorf("403 Forbidden: the gateway saw Authorization: Bearer %s", redacttest.Token)
	_, err := redact.OAuth2(struct{}{}, library)
	if err == nil {
		t.Fatal("OAuth2 answered no error")
	}
	redacttest.RequireNone(t, "redact.no-record", err.Error())
}
