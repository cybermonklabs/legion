package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/ciphertest"
	"github.com/sjawhar/envoy/internal/cipher/helper"
	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
	"github.com/sjawhar/envoy/internal/cipher/proof"
	"github.com/sjawhar/envoy/internal/cipher/requests"
	"github.com/sjawhar/envoy/internal/cipher/secrets"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp/githubapptest"
)

// TestTheExecFormReleasesAGitHubProfileNarrowedByItsTarget drives the real broker (its handlers on
// a real Postgres, through ciphertest's rig), a real helper and this binary, with the broker's own
// GitHub App minter aimed at a stub GitHub installed on acme and ada, which answers a token
// request as GitHub does: the permissions and repositories its body names. Every github-app
// secret the broker serves is a profile's, so the agent-tier github-app secret APP_KEY is served
// by the profile of its own name (policytest.AppProfile), and APP_KEY_PROOFS and APP_KEY_MISSING
// are profiles of APP_KEY on one repository of ada's. A session key the machine enrolled reads
// them: APP_KEY gives every account of its profile its own token, each minted with the profile's
// permissions; APP_KEY_PROOFS gives ada's token alone, minted with the access_tokens body of that
// profile; and APP_KEY_MISSING, on a repository GitHub refuses with 422, runs nothing, the broker
// answering PROFILE_MINT_REFUSED naming it. --target narrows APP_KEY: Ada, which the client
// lowercases before it signs it, gives ada's token alone, minted for ada's installation alone, and
// Ada/proofs that token cut to the one repository, though a grant of the whole profile is live;
// an account the profile does not name is refused 400 TARGET_REFUSED, nothing runs, and GitHub is
// asked for no token.
func TestTheExecFormReleasesAGitHubProfileNarrowedByItsTarget(t *testing.T) {
	type tokenRequest struct{ installation, body string }
	var mu sync.Mutex
	var asked []tokenRequest
	// held is what each installation's account approved for the App, as GitHub's listing names it:
	// every permission the profiles below ask for, most of them at write.
	held := map[string]string{"actions": "write", "checks": "read", "contents": "write", "metadata": "read", "pull_requests": "write", "statuses": "write"}
	github := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/app/installations":
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"id": 7, "account": map[string]string{"login": "acme"}, "permissions": held},
				{"id": 8, "account": map[string]string{"login": "ada"}, "permissions": held},
			})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/access_tokens"):
			body, _ := io.ReadAll(r.Body)
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/app/installations/"), "/access_tokens")
			mu.Lock()
			asked = append(asked, tokenRequest{id, string(body)})
			mu.Unlock()
			var scope struct {
				Repositories []string          `json:"repositories"`
				Permissions  map[string]string `json:"permissions"`
			}
			_ = json.Unmarshal(body, &scope)
			if slices.Contains(scope.Repositories, "missing") {
				w.WriteHeader(http.StatusUnprocessableEntity)
				_ = json.NewEncoder(w).Encode(map[string]string{"message": "There is at least one repository that does not exist or is not accessible to the parent installation."})
				return
			}
			answer := map[string]any{
				"token": "ghs-" + id, "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
				"permissions": scope.Permissions, "repository_selection": "all",
			}
			if len(scope.Repositories) > 0 {
				var repositories []map[string]string
				for _, name := range scope.Repositories {
					repositories = append(repositories, map[string]string{"name": name})
				}
				answer["repository_selection"], answer["repositories"] = "selected", repositories
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(answer)
		default:
			t.Errorf("unexpected request to GitHub: %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(github.Close)
	take := func() []tokenRequest {
		mu.Lock()
		defer mu.Unlock()
		got := asked
		asked = nil
		return got
	}

	profiles := policytest.ProfilesFile(
		policytest.AppProfile("APP_KEY"),
		`{"name": "APP_KEY_PROOFS", "source": "APP_KEY", "github": {"accounts": ["ada"], "repositories": ["ada/proofs"], "permissions": {"contents": "read", "metadata": "read"}}}`,
		`{"name": "APP_KEY_MISSING", "source": "APP_KEY", "github": {"accounts": ["ada"], "repositories": ["ada/missing"], "permissions": {"contents": "read", "metadata": "read"}}}`,
	)
	rig := ciphertest.NewRig(t, ciphertest.WithProfiles(profiles), ciphertest.WithMinters(map[string]requests.Minter{
		policy.TypeGitHubApp: secrets.GitHubApp{Base: github.URL, HTTP: github.Client()}.Mint,
	}))
	_, appKey := githubapptest.Key(t)
	value, err := json.Marshal(map[string]string{"app_id": "123456", "private_key": appKey})
	if err != nil {
		t.Fatal(err)
	}
	rig.Secrets.Put(policytest.TypeSecret("APP_KEY", rig.Operator, policy.TierAgent, policy.TypeGitHubApp, string(value)))
	if status, body := rig.Reread(t, "APP_KEY"); status != http.StatusOK {
		t.Fatalf("reread APP_KEY = %d %s", status, body)
	}

	binary := buildAgentSecrets(t)
	_, sock := serveRealHelper(t, rig.URL, rig.Operator)
	if code, exit, stderr := machineLoginThen(t, rig, binary, sock, "approve", nil); exit != 0 {
		t.Fatalf("machine login (code %s): exit %d, stderr %q", code, exit, stderr)
	}
	boxKey, err := proof.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	thumbprint, err := proof.Thumbprint(&boxKey.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	enrolled, err := helper.Call(sock, helper.Request{Op: "enroll-session-key", RuntimeID: "target-box-" + t.Name(), Thumbprint: thumbprint}, untilGiveUp(t))
	if err != nil || !enrolled.OK {
		t.Fatalf("enroll-session-key: %+v %v", enrolled, err)
	}
	keyDir := writeKeyDir(t, boxKey, enrolled.EnrollmentID)
	run := func(name string, flags ...string) (stdout, stderr string, exit int) {
		t.Helper()
		args := append(append([]string{name, "--reason", "profile end to end"}, flags...), "--", "sh", "-c", `printf %s "$`+name+`"`)
		return runAgentSecrets(t, binary, rig.URL, keyDir, nil, args...)
	}
	release := func(name string, flags ...string) map[string]mintedRelease {
		t.Helper()
		stdout, stderr, exit := run(name, flags...)
		if exit != 0 {
			t.Fatalf("release of %s %q: exit %d, stderr %q; want exit 0", name, flags, exit, stderr)
		}
		var got struct {
			Installations map[string]mintedRelease `json:"installations"`
		}
		if err := json.Unmarshal([]byte(stdout), &got); err != nil {
			t.Fatalf("the command printed %q, not the release JSON: %v", stdout, err)
		}
		return got.Installations
	}
	body := func(request tokenRequest) map[string]any {
		t.Helper()
		var got map[string]any
		if err := json.Unmarshal([]byte(request.body), &got); err != nil {
			t.Fatalf("the access_tokens body %q: %v", request.body, err)
		}
		return got
	}

	installations := release("APP_KEY")
	if len(installations) != 2 || installations["acme"].Token != "ghs-7" || installations["ada"].Token != "ghs-8" {
		t.Fatalf("APP_KEY's release: installations %+v; want acme's and ada's tokens", installations)
	}
	tokens := take()
	if len(tokens) != 2 {
		t.Fatalf("GitHub was asked for tokens %+v, want acme's and ada's", tokens)
	}
	for _, token := range tokens {
		if got, want := body(token), map[string]any{"permissions": map[string]any{"metadata": "read"}}; !reflect.DeepEqual(got, want) {
			t.Fatalf("installation %s's access_tokens body = %v, want the profile's permissions %v", token.installation, got, want)
		}
	}

	installations = release("APP_KEY", "--target", "Ada")
	if len(installations) != 1 || installations["ada"].Token != "ghs-8" {
		t.Fatalf("APP_KEY's release narrowed to Ada: installations %+v; want ada's token alone", installations)
	}
	tokens = take()
	if len(tokens) != 1 || tokens[0].installation != "8" {
		t.Fatalf("GitHub was asked for tokens %+v, want installation 8's alone", tokens)
	}
	if got, want := body(tokens[0]), map[string]any{"permissions": map[string]any{"metadata": "read"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the access_tokens body = %v, want the profile's permissions %v", got, want)
	}

	installations = release("APP_KEY", "--target", "Ada/proofs")
	if len(installations) != 1 || installations["ada"].Token != "ghs-8" {
		t.Fatalf("APP_KEY's release narrowed to Ada/proofs: installations %+v; want ada's token alone", installations)
	}
	tokens = take()
	if len(tokens) != 1 || tokens[0].installation != "8" {
		t.Fatalf("GitHub was asked for tokens %+v, want installation 8's alone", tokens)
	}
	if got, want := body(tokens[0]), map[string]any{"repositories": []any{"proofs"}, "permissions": map[string]any{"metadata": "read"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the access_tokens body = %v, want %v", got, want)
	}

	stdout, stderr, exit := run("APP_KEY", "--target", "outsider")
	if exit != 1 || stdout != "" || !strings.Contains(stderr, "TARGET_REFUSED") {
		t.Fatalf("a target outside the profile: exit %d, stdout %q, stderr %q; want exit 1, nothing run, and TARGET_REFUSED", exit, stdout, stderr)
	}
	if tokens := take(); len(tokens) != 0 {
		t.Fatalf("a target outside the profile: GitHub was asked for tokens %+v, want none", tokens)
	}

	installations = release("APP_KEY_PROOFS")
	if len(installations) != 1 || installations["ada"].Token != "ghs-8" || installations["ada"].InstallationID != 8 {
		t.Fatalf("APP_KEY_PROOFS's release: installations %+v; want ada's token alone", installations)
	}
	tokens = take()
	if len(tokens) != 1 || tokens[0].installation != "8" {
		t.Fatalf("GitHub was asked for tokens %+v, want installation 8's alone", tokens)
	}
	if got, want := body(tokens[0]), map[string]any{"repositories": []any{"proofs"}, "permissions": map[string]any{"contents": "read", "metadata": "read"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the access_tokens body = %v, want %v", got, want)
	}

	stdout, stderr, exit = run("APP_KEY_MISSING")
	if exit != 1 || stdout != "" || !strings.Contains(stderr, "PROFILE_MINT_REFUSED") || !strings.Contains(stderr, "(missing)") || strings.Contains(stderr, "MINT_FAILED") {
		t.Fatalf("release of a profile on a repository GitHub refuses: exit %d, stdout %q, stderr %q; want exit 1, nothing run, and PROFILE_MINT_REFUSED naming the repository", exit, stdout, stderr)
	}
	if tokens := take(); len(tokens) != 1 || tokens[0].installation != "8" {
		t.Fatalf("GitHub was asked for tokens %+v, want installation 8's alone", tokens)
	}
}

// mintedRelease is one installation of a github-app release.
type mintedRelease struct {
	InstallationID int64  `json:"installation_id"`
	Token          string `json:"token"`
}
