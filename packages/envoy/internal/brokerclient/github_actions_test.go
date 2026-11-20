// packages/envoy/internal/brokerclient/github_actions_test.go

package brokerclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/envoy/internal/broker/oauthwire"
)

// runnerRequestToken is the bearer the stand-in runner endpoint takes, which no answer, error or
// description may carry.
const runnerRequestToken = "runner-request-bearer"

// runnerAsked is one request the stand-in runner endpoint was asked.
type runnerAsked struct{ path, apiVersion, audience, authorization string }

// stubRunner is the runner's OIDC token endpoint: each request is answered by answer, numbered
// from 1, and kept. Its URL carries a query, as the runner's own does.
func stubRunner(t *testing.T, answer func(n int, w http.ResponseWriter)) (string, func() []runnerAsked) {
	t.Helper()
	var mu sync.Mutex
	var asked []runnerAsked
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		asked = append(asked, runnerAsked{
			path: r.URL.Path, apiVersion: r.URL.Query().Get("api-version"),
			audience: r.URL.Query().Get("audience"), authorization: r.Header.Get("Authorization"),
		})
		n := len(asked)
		mu.Unlock()
		answer(n, w)
	}))
	t.Cleanup(server.Close)
	return server.URL + "/_apis/idtoken?api-version=2.0", func() []runnerAsked {
		mu.Lock()
		defer mu.Unlock()
		return append([]runnerAsked(nil), asked...)
	}
}

// TestMachineIdentityGitHubActionsReadsTheRunnersEndpoint: AGENT_SECRETS_MACHINE_IDENTITY
// github-actions asks the runner's endpoint, with the bearer the runner gave the job, for a token
// of AGENT_SECRETS_MACHINE_AUDIENCE at every read, and presents the answer as a workflow run's
// token. Each variable it needs is named when it is missing, and a refusal of the endpoint names
// the variable and never the bearer.
func TestMachineIdentityGitHubActionsReadsTheRunnersEndpoint(t *testing.T) {
	url, asked := stubRunner(t, func(n int, w http.ResponseWriter) {
		_ = json.NewEncoder(w).Encode(map[string]any{"count": 1, "value": "job.oidc.token." + strconv.Itoa(n)})
	})
	env := map[string]string{
		IdentityVar: IdentityGitHubActions, ActionsTokenURLVar: url,
		ActionsTokenVar: runnerRequestToken, MachineAudienceVar: "example-audience",
	}
	getenv := func(name string) string { return env[name] }
	source, err := MachineIdentity(getenv)
	if err != nil {
		t.Fatalf("MachineIdentity: %v", err)
	}
	for _, want := range []string{"job.oidc.token.1", "job.oidc.token.2"} {
		token, tokenType, err := source.Token(context.Background())
		if err != nil {
			t.Fatalf("Token: %v", err)
		}
		if token != want || tokenType != oauthwire.SubjectTokenTypeGitHub {
			t.Errorf("Token = %q, %q, want %q, %q", token, tokenType, want, oauthwire.SubjectTokenTypeGitHub)
		}
	}
	wantAsked := runnerAsked{path: "/_apis/idtoken", apiVersion: "2.0", audience: "example-audience", authorization: "Bearer " + runnerRequestToken}
	if got := asked(); len(got) != 2 || got[0] != wantAsked || got[1] != wantAsked {
		t.Errorf("the endpoint was asked %+v, want %+v twice: once at every read", got, wantAsked)
	}
	if describe := source.Describe(); !strings.Contains(describe, ActionsTokenURLVar) || strings.Contains(describe, runnerRequestToken) {
		t.Errorf("Describe = %q, want it to name %s and not the bearer", describe, ActionsTokenURLVar)
	}

	for _, missing := range []string{ActionsTokenVar, MachineAudienceVar} {
		without := func(name string) string {
			if name == missing {
				return ""
			}
			return env[name]
		}
		if _, err := MachineIdentity(without); err == nil || !strings.Contains(err.Error(), missing+" is required by "+IdentityVar+"=github-actions") {
			t.Errorf("MachineIdentity without %s: %v, want a refusal naming it", missing, err)
		}
	}
	noEndpoint := func(name string) string {
		if name == IdentityVar {
			return IdentityGitHubActions
		}
		return ""
	}
	if _, err := MachineIdentity(noEndpoint); err == nil || !strings.Contains(err.Error(), ActionsTokenURLVar+" is required by "+IdentityVar+"=github-actions") {
		t.Errorf("MachineIdentity with no endpoint and no token file: %v, want a refusal naming %s", err, ActionsTokenURLVar)
	}

	refusing, _ := stubRunner(t, func(_ int, w http.ResponseWriter) {
		http.Error(w, "denied for "+runnerRequestToken, http.StatusForbidden)
	})
	env[ActionsTokenURLVar] = refusing
	refused, err := MachineIdentity(getenv)
	if err != nil {
		t.Fatalf("MachineIdentity: %v", err)
	}
	_, _, err = refused.Token(context.Background())
	if err == nil || !strings.Contains(err.Error(), ActionsTokenURLVar) || !strings.Contains(err.Error(), "HTTP 403") || strings.Contains(err.Error(), runnerRequestToken) {
		t.Errorf("Token from a refusing endpoint: %v, want an error naming %s and HTTP 403 and not the bearer", err, ActionsTokenURLVar)
	}
}

// TestMachineIdentityGitHubActionsFollowsNoRedirect: the request carries the runner's bearer. A
// redirect it followed would send that bearer on to a target on the runner's own origin, and take
// a token that another origin minted as the job's. So no request follows one: a 3xx is the
// endpoint's refusal, named by its status.
func TestMachineIdentityGitHubActionsFollowsNoRedirect(t *testing.T) {
	elsewhere, elsewhereAsked := stubRunner(t, func(_ int, w http.ResponseWriter) {
		_ = json.NewEncoder(w).Encode(map[string]any{"value": "token-from-the-other-origin"})
	})
	for _, status := range []int{301, 302, 303, 307, 308} {
		for _, target := range []string{elsewhere, "/_apis/collect"} {
			label := strconv.Itoa(status) + " to " + target
			url, asked := stubRunner(t, func(n int, w http.ResponseWriter) {
				if n == 1 {
					w.Header().Set("Location", target)
					w.WriteHeader(status)
					_, _ = w.Write([]byte("Redirecting to " + target))
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"value": "token-of-the-redirect"})
			})
			source, err := MachineIdentity(func(name string) string {
				return map[string]string{
					IdentityVar: IdentityGitHubActions, ActionsTokenURLVar: url,
					ActionsTokenVar: runnerRequestToken, MachineAudienceVar: "example-audience",
				}[name]
			})
			if err != nil {
				t.Fatalf("MachineIdentity: %v", err)
			}
			token, _, err := source.Token(context.Background())
			want := "the runner's OIDC token endpoint (" + ActionsTokenURLVar + ") answered HTTP " + strconv.Itoa(status)
			if err == nil || err.Error() != want {
				t.Errorf("%s: Token = %q, %v, want the refusal %q", label, token, err, want)
			}
			if got := asked(); len(got) != 1 {
				t.Errorf("%s: the runner's origin was asked %d times, want once: %+v", label, len(got), got)
			}
		}
	}
	if got := elsewhereAsked(); len(got) != 0 {
		t.Errorf("the other origin was asked %+v, want nothing", got)
	}
}

// TestMachineIdentityGitHubActionsReadsATokenFile: a job with no runner endpoint reads its run's
// token from AGENT_SECRETS_MACHINE_TOKEN_FILE at every read, as a workflow run's token, and an
// empty file is refused naming it.
func TestMachineIdentityGitHubActionsReadsATokenFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run-token")
	write := func(token string) {
		if err := os.WriteFile(path, []byte(token), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("first.run.token\n")
	source, err := MachineIdentity(func(name string) string {
		return map[string]string{IdentityVar: IdentityGitHubActions, MachineTokenFileVar: path}[name]
	})
	if err != nil {
		t.Fatalf("MachineIdentity: %v", err)
	}
	for _, want := range []string{"first.run.token", "second.run.token"} {
		token, tokenType, err := source.Token(context.Background())
		if err != nil {
			t.Fatalf("Token: %v", err)
		}
		if token != want || tokenType != oauthwire.SubjectTokenTypeGitHub {
			t.Errorf("Token = %q, %q, want %q, %q", token, tokenType, want, oauthwire.SubjectTokenTypeGitHub)
		}
		write("second.run.token\n")
	}
	write("  \n")
	if _, _, err := source.Token(context.Background()); err == nil || !strings.Contains(err.Error(), path+" is empty") {
		t.Errorf("Token from an empty file: %v, want an error naming %s", err, path)
	}
}
