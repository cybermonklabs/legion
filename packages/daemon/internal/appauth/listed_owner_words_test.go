package appauth

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
	"github.com/sjawhar/legion/daemon/internal/githubtest"
)

// TestAListedOwnerThatEchoesTheAppJWTIsSaidOnlyInItsShape: whatever answers in GitHub's place
// lists an installation of the App on an account whose login echoes the discovery's
// Authorization, the App's JWT. Discovery takes any login it lists, and the owners it answers are
// lowercased, so the name reaching a bundle is the JWT case-folded: no directory's name, which
// the bundle leaves out. A role's bundle is served from acme and says the left-out owner on its
// log (left-out); a controller's bundle asked for no owner, where that account is the App's only
// one, is refused naming every owner and why each was left out (no-owner). Each named the owner
// as discovery listed it, the case-folded JWT ("eyjhbgci..."), which holds the JWT's runs that
// have no capital letter, and now names it only in its shape (githubrest.NotALogin). The twin
// shows the owners discovery answers hold the case-folded JWT, so the rows can see a leak.
// Captures appauth.answer-words/listed-owner-<case>.
func TestAListedOwnerThatEchoesTheAppJWTIsSaidOnlyInItsShape(t *testing.T) {
	_, privatePEM := testAppKey(t)
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		name string
		// alone lists the echoed account alone; otherwise acme is listed before it.
		alone bool
	}{
		{name: "twin"},
		{name: "left-out"},
		{name: "no-owner", alone: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			var mu sync.Mutex
			var jwt string
			server := githubtest.Serve(t, githubtest.Spec{Handle: func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Path == "/app/installations":
					sent := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
					mu.Lock()
					jwt = sent
					mu.Unlock()
					listed := fmt.Sprintf(`{"id":78,"account":{"login":%q}}`, sent)
					if !c.alone {
						listed = `{"id":77,"account":{"login":"acme"}},` + listed
					}
					fmt.Fprintf(w, "[%s]", listed)
				case r.URL.Path == "/app/installations/77":
					fmt.Fprint(w, installationGranting("77", grantedEverythingAsked))
				case r.URL.Path == "/app/installations/77/access_tokens":
					_, asked := askedInExchange(t, r)
					fmt.Fprint(w, mintedFor("ghs_acme", now.Add(time.Hour), asked.Permissions))
				case r.URL.Path == "/app":
					fmt.Fprint(w, `{"slug":"legion-implementer"}`)
				case r.URL.Path == "/users/legion-implementer[bot]":
					fmt.Fprint(w, `{"id":271566630}`)
				default:
					githubtest.Unexpected(t, w, r)
				}
			}})
			tokens := New(config.GitHubApps{
				Implement: config.GitHubApp{AppID: "12345", PrivateKey: privatePEM},
				Review:    config.GitHubApp{AppID: "12345", PrivateKey: privatePEM},
			}, Options{BaseURL: server.URL, HTTPClient: server.Client(), Now: func() time.Time { return now }, Stderr: &bytes.Buffer{}})
			defer tokens.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			credential := func() string {
				mu.Lock()
				defer mu.Unlock()
				return jwt
			}
			log, said := bundleLog()
			served := servesOneOf("acme/widgets")

			var printed string
			switch c.name {
			case "twin":
				owners, err := tokens.InstalledOwners(ctx, Implement)
				if err != nil {
					t.Fatalf("InstalledOwners: %v", err)
				}
				if sent := credential(); !strings.HasPrefix(sent, "eyJ") || !slices.Contains(owners, strings.ToLower(sent)) {
					t.Fatalf("discovery answered owners %q for a listing of the JWT %q, want the JWT case-folded among them, so the rows can see a leak", owners, sent)
				}
				return
			case "left-out":
				bundle, err := GitHubCredential(tokens, served, ghrepo.Renames{}, NewInstalled(), NewLeftOut(), log)(ctx, claim.RoleImplementer, ghrepo.MustParse("acme/widgets"))
				if err != nil {
					t.Fatalf("the implementer's bundle beside an owner that echoes the JWT: %v", err)
				}
				if want := []string{"acme"}; !slices.Equal(bundle.DirNames(), want) {
					t.Fatalf("the bundle holds %q, want %q: the echoed owner is left out", bundle.DirNames(), want)
				}
				printed = said.String()
			case "no-owner":
				_, err := ControllerCredential(tokens, served, ghrepo.Renames{}, NewInstalled(), NewLeftOut(), log)(ctx, "")
				if err == nil {
					t.Fatal("the controller's bundle of an App whose one owner is no directory's name was served; want it refused")
				}
				printed = err.Error() + "\n" + said.String()
			}
			sent := credential()
			if !strings.HasPrefix(sent, "eyJ") {
				t.Fatalf("discovery carried no JWT the stand-in could echo: %q", sent)
			}
			oauth2calltest.RequireNoPiece(t, "appauth.answer-words/listed-owner-"+c.name, printed, sent, strings.ToLower(sent))
			if !strings.Contains(printed, githubrest.NotALogin) || !strings.Contains(printed, "no lowercased GitHub login") {
				t.Errorf("the bundle said %q, want the left-out owner named as %q and why it is left out", printed, githubrest.NotALogin)
			}
		})
	}
}
