package api

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
	"github.com/sjawhar/legion/daemon/internal/githubtest"
)

// controllerServedLine is the line the controller's credential route logs a served bundle under
// (controllerGitHubCredential).
const controllerServedLine = "api: controller github credential served"

// echoedOwnerReviewApp is the daemon's token manager (appauth.New) with the review App in key
// mode, reading GitHub at a stand-in whose installation discovery lists the App on acme
// (installation 77) and on an account whose login is the discovery's own Authorization, the
// App's JWT (installation 78), as whatever answers in GitHub's place may write it. acme's
// installation is granted every permission a mint asks for, and its exchange answers a token in
// GitHub's shape holding the permissions it was asked for. jwt is the JWT the last discovery
// carried.
func echoedOwnerReviewApp(t *testing.T) (tokens *appauth.Manager, jwt func() string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	privatePEM := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	var mu sync.Mutex
	var sent string
	github := githubtest.Serve(t, githubtest.Spec{Handle: func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/app/installations":
			echoed := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			mu.Lock()
			sent = echoed
			mu.Unlock()
			fmt.Fprintf(w, `[{"id":77,"account":{"login":"acme"}},{"id":78,"account":{"login":%q}}]`, echoed)
		case "/app/installations/77":
			fmt.Fprint(w, `{"id":77,"permissions":{"actions":"write","attestations":"read","checks":"write","contents":"write","issues":"write","metadata":"read","pull_requests":"write","security_events":"write","statuses":"write","workflows":"write"},"suspended_at":null}`)
		case "/app/installations/77/access_tokens":
			var asked struct {
				Permissions map[string]string `json:"permissions"`
			}
			if err := json.NewDecoder(r.Body).Decode(&asked); err != nil {
				t.Errorf("the exchange body is not the JSON an access_tokens request is: %v", err)
			}
			// A map of strings always marshals, and json.Marshal sorts its keys.
			held, _ := json.Marshal(asked.Permissions)
			fmt.Fprintf(w, `{"token":%q,"expires_at":%q,"repository_selection":"all","permissions":%s}`,
				githubtest.InstallationToken(t.Name()), time.Now().Add(time.Hour).UTC().Format(time.RFC3339), held)
		case "/app":
			fmt.Fprint(w, `{"slug":"legion-reviewer"}`)
		case "/users/legion-reviewer[bot]":
			fmt.Fprint(w, `{"id":271566631}`)
		default:
			githubtest.Unexpected(t, w, r)
		}
	}})
	tokens = appauth.New(config.GitHubApps{Review: config.GitHubApp{AppID: sinkAppID, PrivateKey: privatePEM}},
		appauth.Options{BaseURL: github.URL, HTTPClient: github.Client(), Stderr: io.Discard, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	t.Cleanup(tokens.Close)
	return tokens, func() string {
		mu.Lock()
		defer mu.Unlock()
		if !strings.HasPrefix(sent, "eyJhbGci") {
			t.Fatalf("the review App's discovery carried no JWT the stand-in could echo: %q", sent)
		}
		return sent
	}
}

// TestTheControllerCredentialNamesAListedOwnerOnlyInItsShape: whatever answers in GitHub's place
// lists the review App's installations on acme and on an account whose login echoes the
// discovery's Authorization, the App's JWT (echoedOwnerReviewApp). Key-mode discovery takes any
// login it lists and lowercases it, so the owners the controller's credential route reads with
// its bundle hold the JWT case-folded, which holds every run of the JWT that has no capital
// letter; the bundle leaves that name out, as no directory's, and is served from acme.
// The route's line (log) and the answer's installed (served) name each listed owner only as a
// login GitHub allows (githubrest.ShownLogin), the fixed phrase in the echo's place, in the
// read's order, so the operator and the controller still see an entry is there. The twin shows
// the read the route takes them from (appauth.ControllerCredential's Installed) holds the JWT
// case-folded, so the rows can see a leak. Captures api.answer-words/listed-owner-<case>.
func TestTheControllerCredentialNamesAListedOwnerOnlyInItsShape(t *testing.T) {
	for _, name := range []string{"twin", "log", "served"} {
		t.Run(name, func(t *testing.T) {
			tokens, jwt := echoedOwnerReviewApp(t)
			if name == "twin" {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				read := appauth.NewInstalled()
				credential := appauth.ControllerCredential(tokens, servesOneOf(ghrepo.MustParse("acme/widgets")), ghrepo.Renames{}, read, nil,
					slog.New(slog.NewTextHandler(io.Discard, nil)))
				if _, err := credential(ctx, ""); err != nil {
					t.Fatalf("the controller's bundle beside an owner that echoes the JWT: %v", err)
				}
				if owners, _ := read.Owners(appauth.Review); !slices.Equal(owners, []string{"acme", strings.ToLower(jwt())}) {
					t.Fatalf("the bundle's read answered owners %q for a listing of acme and the JWT, want acme and the JWT case-folded, so the rows can see a leak", owners)
				}
				return
			}
			log, kept := newKeptLog()
			h := controllerCredentialServer(t, tokens, log, context.Background())
			recorder := h.controllerGitHubCredential(h.mintedSecret())
			if recorder.Code != http.StatusOK {
				t.Fatalf("controller github credential beside an owner that echoes the JWT = %d, want 200; body %s", recorder.Code, recorder.Body)
			}
			sent := jwt()
			switch name {
			case "log":
				line, _, _ := kept.line(t, slog.LevelInfo, controllerServedLine, "installed")
				oauth2calltest.RequireNoPiece(t, "api.answer-words/listed-owner-log", line, sent, strings.ToLower(sent))
				if shown := `"installed":["acme","` + githubrest.NotALogin + `"]`; !strings.Contains(line, shown) {
					t.Errorf("the route logged %s, want %s: each listed owner, the echo as the fixed phrase", line, shown)
				}
			case "served":
				oauth2calltest.RequireNoPiece(t, "api.answer-words/listed-owner-served", recorder.Body.String(), sent, strings.ToLower(sent))
				var answer ControllerCredentialResponse
				decodeInto(t, recorder, &answer)
				if want := []string{"acme", githubrest.NotALogin}; !slices.Equal(answer.Installed, want) {
					t.Errorf("the answer's installed = %q, want %q: each listed owner, the echo as the fixed phrase", answer.Installed, want)
				}
				if dirs := slices.Sorted(maps.Keys(answer.Owners)); !slices.Equal(dirs, []string{"acme"}) {
					t.Errorf("the answer's owners = %q, want acme alone: the echoed owner is no directory", dirs)
				}
			}
		})
	}
}
