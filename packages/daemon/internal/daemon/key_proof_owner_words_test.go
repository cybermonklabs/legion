package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/redact"
)

// keyProofLine is what the boot's proof of an App's key logs for an App installed on an account
// (proveAppKeys).
const keyProofLine = "legion workflow proved a GitHub App's key"

// appTokensID is the App id appTokens configures each role's App with.
var appTokensID = map[appauth.AppRole]string{appauth.Implement: "1", appauth.Review: "2"}

// TestTheKeyProofLineNamesAListedOwnerOnlyInItsShape: whatever answers in GitHub's place lists
// each App's installations on acme and on an account whose login echoes the discovery's
// Authorization, the App's JWT. Key-mode discovery takes any login it lists and lowercases it, so
// the owners the boot's proof of each App's key reads hold the JWT case-folded, which holds every
// run of the JWT that has no capital letter. The proof's line for each App (installed-on) names
// each owner only as a login GitHub allows (githubrest.ShownLogin), the fixed phrase in the
// echo's place, in the read's order, so the operator still sees an entry is there; the boot's
// record of the read (appauth.Installed.RecordProof), which every launch's addressing decides on,
// keeps the owners as they came. The twin shows the owners discovery answers hold the JWT
// case-folded, so the row can see a leak. Captures daemon.answer-words/listed-owner-installed-on.
func TestTheKeyProofLineNamesAListedOwnerOnlyInItsShape(t *testing.T) {
	for _, name := range []string{"twin", "installed-on"} {
		t.Run(name, func(t *testing.T) {
			var mu sync.Mutex
			// sent is the JWT each App's discovery carried, by the App id it names.
			sent := map[string]string{}
			apps, _ := appTokens(t, func(w http.ResponseWriter, r *http.Request, app string, _ int) {
				jwt := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
				mu.Lock()
				sent[app] = jwt
				mu.Unlock()
				fmt.Fprintf(w, `[{"id":77,"account":{"login":"acme"}},{"id":78,"account":{"login":%q}}]`, jwt)
			})
			defer apps.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			// jwt is the JWT role's App discovery carried.
			jwt := func(role appauth.AppRole) string {
				t.Helper()
				mu.Lock()
				defer mu.Unlock()
				carried := sent[appTokensID[role]]
				if !strings.HasPrefix(carried, "eyJhbGci") {
					t.Fatalf("the %s App's discovery carried no JWT the stand-in could echo: %q", role, carried)
				}
				return carried
			}

			if name == "twin" {
				owners, err := apps.InstalledOwners(ctx, appauth.Implement)
				if err != nil {
					t.Fatalf("InstalledOwners: %v", err)
				}
				if echoed := strings.ToLower(jwt(appauth.Implement)); !slices.Equal(owners, []string{"acme", echoed}) {
					t.Fatalf("discovery answered owners %q for a listing of acme and the JWT, want acme and the JWT case-folded, so the row can see a leak", owners)
				}
				return
			}
			logs := &syncBuffer{}
			installed := appauth.NewInstalled()
			if err := proveAppKeys(ctx, apps, installed, newStartWaits(productionAppCalls()),
				slog.New(slog.NewJSONHandler(logs, redact.HandlerOptions(nil)))); err != nil {
				t.Fatalf("the boot's proof of each App's key: %v", err)
			}
			printed := logs.String()
			secrets := make([]string, 0, 2*len(appauth.Roles))
			for _, role := range appauth.Roles {
				secrets = append(secrets, jwt(role), strings.ToLower(jwt(role)))
			}
			oauth2calltest.RequireNoPiece(t, "daemon.answer-words/listed-owner-installed-on", printed, secrets...)
			shown := `"installedOn":["acme","` + notALogin + `"]`
			if lines, named := strings.Count(printed, keyProofLine), strings.Count(printed, shown); lines != len(appauth.Roles) || named != lines {
				t.Errorf("the proof logged %d lines of %q and %d naming %s, want one of each per App; log:\n%s", lines, keyProofLine, named, shown, printed)
			}
			for _, role := range appauth.Roles {
				if owners, _ := installed.Owners(role); !slices.Equal(owners, []string{"acme", strings.ToLower(jwt(role))}) {
					t.Errorf("the boot recorded the %s App installed on %q, want the owners as discovery answered them", role, owners)
				}
			}
		})
	}
}
