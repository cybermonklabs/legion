package appauth

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/redirects/redirectstest"
)

// TestATokenExchangeRedirectNamesNoRunOfTheAppsJWT: whatever answers in GitHub's place answers the
// token exchange, which carries the App's JWT, with a redirect whose Location quotes that JWT. One
// net/http cannot parse (unparsed) is net/http's error whatever the client's redirect policy; one
// that parses names an address on another host nothing listens at (elsewhere), which the Manager's
// client does not follow (it follows a redirect only on its own origin, as GitHub's move of a
// repository answers one), so the 302 is the exchange's answer, refused by its status. The mint's
// error names the exchange's failure by its class or its status and holds no run of the JWT nor a
// word of the Location. The twin shows the stand-in's echo reaches net/http's own error. Captures
// appauth.redirect/<case>.
func TestATokenExchangeRedirectNamesNoRunOfTheAppsJWT(t *testing.T) {
	_, privatePEM := testAppKey(t)
	dead := redirectstest.Dead(t)
	for _, c := range []struct {
		name     string
		location func(jwt string) string
	}{
		{"twin", redirectstest.Unparsed},
		{"unparsed", redirectstest.Unparsed},
		{"elsewhere", func(jwt string) string { return redirectstest.To(dead, jwt) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			var mu sync.Mutex
			var jwt string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/app/installations":
					fmt.Fprint(w, `[{"id":77,"account":{"login":"acme"}}]`)
				case "/app/installations/77":
					fmt.Fprint(w, installationGranting("77", grantedEverythingAsked))
				case "/app/installations/77/access_tokens":
					mu.Lock()
					jwt = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
					mu.Unlock()
					w.Header().Set("Location", c.location(r.Header.Get("Authorization")))
					w.WriteHeader(http.StatusFound)
				default:
					t.Errorf("unexpected GitHub path %s", r.URL.Path)
				}
			}))
			defer server.Close()
			tokens := New(config.GitHubApps{Implement: config.GitHubApp{AppID: "12345", PrivateKey: privatePEM}},
				Options{BaseURL: server.URL, HTTPClient: server.Client()})
			defer tokens.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			var err error
			if c.name == "twin" {
				// The exchange as net/http's own client asks it, with the JWT the Manager signs.
				var request *http.Request
				request, err = http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/app/installations/77/access_tokens", nil)
				if err != nil {
					t.Fatal(err)
				}
				signed, signErr := tokens.generateJWT("12345", privatePEM, time.Now())
				if signErr != nil {
					t.Fatal(signErr)
				}
				tokens.appHeaders(request, signed)
				var response *http.Response
				if response, err = server.Client().Do(request); err == nil {
					_ = response.Body.Close()
				}
			} else {
				_, err = tokens.Token(ctx, Implement, "acme")
			}
			if err == nil {
				t.Fatal("the exchange answered no error")
			}
			mu.Lock()
			signed := jwt
			mu.Unlock()
			if !strings.HasPrefix(signed, "eyJ") {
				t.Fatalf("the exchange carried no JWT the stand-in could echo: %q", signed)
			}
			if c.name == "twin" {
				if !strings.Contains(err.Error(), signed) {
					t.Fatalf("net/http's own error holds no echo of the JWT, so the rows cannot see a leak: %q", err)
				}
				return
			}
			oauth2calltest.RequireNoPiece(t, "appauth.redirect/"+c.name, err.Error(), signed)
			if strings.Contains(err.Error(), "moved") {
				t.Errorf("the mint's error names the Location: %q", err)
			}
		})
	}
}

// TestTheManagersClientFollowsARedirectOnlyOnItsOwnOrigin: InstalledOn reads a repository GitHub
// moved, whose installation read GitHub answers with a 301 on its own host, by following it, as it
// did (moved); a 302 naming a stand-in on another host is not followed: the read is refused by
// its status, and the stand-in is asked nothing (elsewhere).
func TestTheManagersClientFollowsARedirectOnlyOnItsOwnOrigin(t *testing.T) {
	_, privatePEM := testAppKey(t)
	target, asked := redirectstest.Target(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/acme/old/installation":
			http.Redirect(w, r, "/repositories/7/installation", http.StatusMovedPermanently)
		case "/repositories/7/installation":
			fmt.Fprint(w, `{"id":77}`)
		case "/repos/acme/far/installation":
			w.Header().Set("Location", redirectstest.To(target, "far"))
			w.WriteHeader(http.StatusFound)
		default:
			t.Errorf("unexpected GitHub path %s", r.URL.Path)
		}
	}))
	defer server.Close()
	tokens := New(config.GitHubApps{Implement: config.GitHubApp{AppID: "12345", PrivateKey: privatePEM}},
		Options{BaseURL: server.URL, HTTPClient: server.Client()})
	defer tokens.Close()
	t.Run("moved", func(t *testing.T) {
		installed, err := tokens.InstalledOn(context.Background(), Implement, ghrepo.MustParse("acme/old"))
		if err != nil || !installed {
			t.Fatalf("InstalledOn(acme/old), moved on GitHub's own host = %v, %v; want the redirect followed and installed", installed, err)
		}
	})
	t.Run("elsewhere", func(t *testing.T) {
		installed, err := tokens.InstalledOn(context.Background(), Implement, ghrepo.MustParse("acme/far"))
		if err == nil || installed || !strings.Contains(err.Error(), "(302)") {
			t.Fatalf("InstalledOn(acme/far), redirected to another host = %v, %v; want it refused by its status", installed, err)
		}
		if n := asked(); n != 0 {
			t.Errorf("the redirect's target on another host was asked %d times, want none", n)
		}
	})
}
