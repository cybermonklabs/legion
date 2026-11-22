package secrets_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/secrets"
	"github.com/sjawhar/envoy/internal/githubtest"
	"github.com/sjawhar/envoy/internal/logtest"
)

// tokenRequest is one POST .../access_tokens the stub GitHub received: the installation it named,
// the body it carried ("" for none) and its Content-Type.
type tokenRequest struct {
	installation string
	body         string
	contentType  string
}

// narrowingGitHub is a stub GitHub with the App installed on Acme-Org (1), ada (2) and outsider
// (3). It lists each installation with what it holds, everyGrant unless a test sets another
// (setHeld), and answers a token request naming a permission its installation does not hold
// HTTP 422, minting nothing, as GitHub does. It records every request it receives and every token
// request's body, and answers every other token request with answer, asGitHubAnswers unless a
// test sets another before its first request, and with headers. It records the bearer of every
// DELETE /installation/token and answers it revokeStatus, 204 unless a test sets another.
type narrowingGitHub struct {
	server *httptest.Server

	mu           sync.Mutex
	requests     int
	tokens       []tokenRequest
	revoked      []string
	answer       func(installation string, body []byte) (int, any)
	headers      http.Header
	revokeStatus int
	held         map[string]map[string]string
}

// everyGrant is what each installation of the stub holds unless a test sets another: every
// permission a read-only narrowing can ask for, most of them at write, as a Legion App's
// installation holds them, so a read-only token asks for each at read.
var everyGrant = map[string]string{
	"actions": "write", "checks": "write", "contents": "write", "metadata": "read", "pull_requests": "write", "statuses": "write",
}

// heldWithout is everyGrant less permissions.
func heldWithout(permissions ...string) map[string]string {
	held := maps.Clone(everyGrant)
	for _, permission := range permissions {
		delete(held, permission)
	}
	return held
}

func newNarrowingGitHub(t *testing.T) *narrowingGitHub {
	t.Helper()
	g := &narrowingGitHub{answer: asGitHubAnswers, revokeStatus: http.StatusNoContent}
	g.server = githubtest.Serve(t, githubtest.Spec{Handle: func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		g.requests++
		answer, headers, revokeStatus, held := g.answer, g.headers, g.revokeStatus, g.held
		g.mu.Unlock()
		holds := func(installation string) map[string]string {
			if grant, set := held[installation]; set {
				return grant
			}
			return everyGrant
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/app/installations":
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"id": 1, "account": map[string]string{"login": "Acme-Org"}, "permissions": holds("1")},
				{"id": 2, "account": map[string]string{"login": "ada"}, "permissions": holds("2")},
				{"id": 3, "account": map[string]string{"login": "outsider"}, "permissions": holds("3")},
			})
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/app/installations/") && strings.HasSuffix(r.URL.Path, "/access_tokens"):
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("read token request body: %v", err)
			}
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/app/installations/"), "/access_tokens")
			g.mu.Lock()
			g.tokens = append(g.tokens, tokenRequest{installation: id, body: string(body), contentType: r.Header.Get("Content-Type")})
			g.mu.Unlock()
			var asked struct {
				Permissions map[string]string `json:"permissions"`
			}
			_ = json.Unmarshal(body, &asked)
			status, reply := http.StatusUnprocessableEntity, any(map[string]string{"message": "The permissions requested are not granted to this installation."})
			if heldAll(holds(id), asked.Permissions) {
				status, reply = answer(id, body)
			}
			for name, values := range headers {
				w.Header()[name] = values
			}
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(reply)
		case r.Method == http.MethodDelete && r.URL.Path == "/installation/token":
			g.mu.Lock()
			g.revoked = append(g.revoked, r.Header.Get("Authorization"))
			g.mu.Unlock()
			w.WriteHeader(revokeStatus)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}})
	return g
}

// heldAll reports whether an installation holding held holds every permission asked names.
func heldAll(held, asked map[string]string) bool {
	for permission := range asked {
		if _, holds := held[permission]; !holds {
			return false
		}
	}
	return true
}

// setHeld makes the stub list installation as holding held, and refuse every later token request
// for it that names a permission held does not.
func (g *narrowingGitHub) setHeld(installation string, held map[string]string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.held == nil {
		g.held = map[string]map[string]string{}
	}
	g.held[installation] = held
}

// setAnswer makes the stub answer every later token request with answer.
func (g *narrowingGitHub) setAnswer(answer func(installation string, body []byte) (int, any)) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.answer = answer
}

// setHeaders makes the stub send headers with every later token request's answer.
func (g *narrowingGitHub) setHeaders(headers http.Header) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.headers = headers
}

// setRevokeStatus makes the stub answer every later revoke with status.
func (g *narrowingGitHub) setRevokeStatus(status int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.revokeStatus = status
}

// takeRevoked returns the Authorization header of every revoke since the last call, and forgets
// them.
func (g *narrowingGitHub) takeRevoked() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	revoked := g.revoked
	g.revoked = nil
	return revoked
}

// asGitHubAnswers is the 201 GitHub gives a token request with body: the permissions the body
// names, or every permission the App holds when it names none, and, when it names repositories,
// those alone with the selection "selected"; with none, the installation's own selection, "all".
func asGitHubAnswers(installation string, body []byte) (int, any) {
	var asked struct {
		Repositories []string          `json:"repositories"`
		Permissions  map[string]string `json:"permissions"`
	}
	_ = json.Unmarshal(body, &asked)
	answer := map[string]any{
		"token": "token-" + installation, "expires_at": "2026-10-23T13:00:00Z", "repository_selection": "all",
		"permissions": map[string]string{"administration": "write", "contents": "write", "metadata": "read"},
	}
	if asked.Permissions != nil {
		answer["permissions"] = asked.Permissions
	}
	if len(asked.Repositories) > 0 {
		var repositories []map[string]string
		for _, name := range asked.Repositories {
			repositories = append(repositories, map[string]string{"name": name, "full_name": "owner/" + name})
		}
		answer["repository_selection"] = "selected"
		answer["repositories"] = repositories
	}
	return http.StatusCreated, answer
}

// take returns how many requests the stub received and the token requests among them since the
// last take, and forgets them.
func (g *narrowingGitHub) take() (int, []tokenRequest) {
	g.mu.Lock()
	defer g.mu.Unlock()
	requests, tokens := g.requests, g.tokens
	g.requests, g.tokens = 0, nil
	return requests, tokens
}

// readOnlyBody is the permissions a read-only narrowing asks GitHub for of an installation that
// holds every one of them, as the token request's body decodes.
var readOnlyBody = map[string]any{"actions": "read", "checks": "read", "contents": "read", "metadata": "read", "pull_requests": "read", "statuses": "read"}

// TestGitHubAppMintSendsTheNarrowingAsTheAccessTokensBody: a narrowed release asks GitHub for one
// token, for the installation of the narrowing's owner alone, with GitHub's access_tokens body
// naming the repositories and, for read-only, contents, metadata, pull_requests, actions, checks
// and statuses, each at read, though the installation holds most of them at write; every other
// listed owner gets no token request. A release with no narrowing is today's: every listed
// owner's token, each asked for with no body.
func TestGitHubAppMintSendsTheNarrowingAsTheAccessTokensBody(t *testing.T) {
	key, err := testAppKey()
	if err != nil {
		t.Fatal(err)
	}
	github := newNarrowingGitHub(t)
	app := secrets.GitHubApp{Base: github.server.URL, HTTP: github.server.Client()}
	value := githubAppValue(t, "123456", key)

	for _, tc := range []struct {
		name      string
		narrowing *secrets.GitHubNarrowing
		owners    []string       // the installations Mint returns
		minted    []string       // the installations asked for a token
		body      map[string]any // each token request's decoded body, nil for none
	}{
		{
			name:      "repositories and read-only",
			narrowing: &secrets.GitHubNarrowing{Owner: "acme-org", Repositories: []string{"proofs", "widgets"}, ReadOnly: true},
			owners:    []string{"Acme-Org"},
			minted:    []string{"1"},
			body: map[string]any{
				"repositories": []any{"proofs", "widgets"},
				"permissions":  readOnlyBody,
			},
		},
		{
			name:      "repositories alone",
			narrowing: &secrets.GitHubNarrowing{Owner: "ada", Repositories: []string{"lean"}},
			owners:    []string{"ada"},
			minted:    []string{"2"},
			body:      map[string]any{"repositories": []any{"lean"}},
		},
		{
			name:      "read-only alone",
			narrowing: &secrets.GitHubNarrowing{Owner: "ada", ReadOnly: true},
			owners:    []string{"ada"},
			minted:    []string{"2"},
			body:      map[string]any{"permissions": readOnlyBody},
		},
		{
			name:      "the owner alone",
			narrowing: &secrets.GitHubNarrowing{Owner: "ada"},
			owners:    []string{"ada"},
			minted:    []string{"2"},
		},
		{
			name:   "no narrowing",
			owners: []string{"Acme-Org", "ada"},
			minted: []string{"1", "2"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			release, err := app.Mint(t.Context(), secrets.MintRequest{Value: value, GitHubOwners: []string{"acme-org", "ada"}, GitHubNarrowing: tc.narrowing})
			if err != nil {
				t.Fatalf("Mint: %v", err)
			}
			_, tokens := github.take()
			var got mintedGitHubApp
			if err := json.Unmarshal([]byte(release.Value), &got); err != nil {
				t.Fatalf("Mint output: %v", err)
			}
			if len(got.Installations) != len(tc.owners) {
				t.Fatalf("Mint installations = %v, want %v alone", got.Installations, tc.owners)
			}
			for _, owner := range tc.owners {
				if _, ok := got.Installations[owner]; !ok {
					t.Fatalf("Mint installations = %v, want %v alone", got.Installations, tc.owners)
				}
			}
			var minted []string
			for _, token := range tokens {
				minted = append(minted, token.installation)
				if tc.body == nil {
					if token.body != "" {
						t.Fatalf("token request for installation %s carried %q, want no body", token.installation, token.body)
					}
					continue
				}
				var body map[string]any
				if err := json.Unmarshal([]byte(token.body), &body); err != nil {
					t.Fatalf("token request body %q: %v", token.body, err)
				}
				if !reflect.DeepEqual(body, tc.body) {
					t.Fatalf("token request body = %v, want %v", body, tc.body)
				}
				if token.contentType != "application/json" {
					t.Fatalf("token request Content-Type = %q, want application/json", token.contentType)
				}
			}
			if !reflect.DeepEqual(minted, tc.minted) {
				t.Fatalf("token requests for installations %v, want %v", minted, tc.minted)
			}
		})
	}
}

// TestGitHubAppMintRefusesANarrowingToAnUncoveredOwner: a narrowing names an account github-owners
// lists or it is refused as the caller's narrowing (class narrowing, not the App's refused),
// without a request to GitHub, even where the App is installed on that account; and a listed
// account the App is not installed on mints nothing.
func TestGitHubAppMintRefusesANarrowingToAnUncoveredOwner(t *testing.T) {
	key, err := testAppKey()
	if err != nil {
		t.Fatal(err)
	}
	github := newNarrowingGitHub(t)
	app := secrets.GitHubApp{Base: github.server.URL, HTTP: github.server.Client()}
	value := githubAppValue(t, "123456", key)

	_, err = app.Mint(t.Context(), secrets.MintRequest{Value: value, GitHubOwners: []string{"acme-org", "ada"}, GitHubNarrowing: &secrets.GitHubNarrowing{Owner: "outsider", ReadOnly: true}})
	if secrets.MintErrorClass(err) != "narrowing" || !strings.Contains(err.Error(), "outsider") || !strings.Contains(err.Error(), "github-owners") {
		t.Fatalf("Mint narrowed to an unlisted owner = %v (class %s), want class narrowing naming the owner and github-owners", err, secrets.MintErrorClass(err))
	}
	if requests, _ := github.take(); requests != 0 {
		t.Fatalf("GitHub received %d requests for an unlisted owner, want none", requests)
	}

	_, err = app.Mint(t.Context(), secrets.MintRequest{Value: value, GitHubOwners: []string{"acme-org", "grace"}, GitHubNarrowing: &secrets.GitHubNarrowing{Owner: "grace"}})
	if secrets.MintErrorClass(err) != "refused" || !strings.Contains(err.Error(), "grace") {
		t.Fatalf("Mint narrowed to a listed owner with no installation = %v, want refused naming the owner", err)
	}
	if _, tokens := github.take(); len(tokens) != 0 {
		t.Fatalf("token requests %v for an owner with no installation, want none", tokens)
	}
}

// TestGitHubAppMintNamesTheNarrowingWhenGitHubRefusesIt: GitHub's 422 to a narrowed token request
// is the caller's narrowing (class narrowing), and the error names what it likely got wrong: the
// repositories it names, or the read access it asks for. A 422 to a request that sent no body (no
// narrowing, or an owner alone) narrowed nothing and stays the App's (class refused). No error
// quotes GitHub's own text.
func TestGitHubAppMintNamesTheNarrowingWhenGitHubRefusesIt(t *testing.T) {
	key, err := testAppKey()
	if err != nil {
		t.Fatal(err)
	}
	github := newNarrowingGitHub(t)
	github.setAnswer(func(string, []byte) (int, any) {
		return http.StatusUnprocessableEntity, map[string]string{"message": "There is at least one repository that does not exist or is not accessible to the parent installation."}
	})
	app := secrets.GitHubApp{Base: github.server.URL, HTTP: github.server.Client()}
	value := githubAppValue(t, "123456", key)

	for _, tc := range []struct {
		name      string
		narrowing *secrets.GitHubNarrowing
		class     string
		mentions  []string
	}{
		{"repositories", &secrets.GitHubNarrowing{Owner: "ada", Repositories: []string{"proofs", "widgets"}}, "narrowing", []string{"422", "ada", "proofs, widgets"}},
		{"read-only", &secrets.GitHubNarrowing{Owner: "ada", ReadOnly: true}, "narrowing", []string{"422", "ada", "contents: read"}},
		{"the owner alone", &secrets.GitHubNarrowing{Owner: "ada"}, "refused", []string{"422"}},
		{"no narrowing", nil, "refused", []string{"422"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := app.Mint(t.Context(), secrets.MintRequest{Value: value, GitHubOwners: []string{"acme-org", "ada"}, GitHubNarrowing: tc.narrowing})
			if secrets.MintErrorClass(err) != tc.class {
				t.Fatalf("Mint = %v (class %s), want class %s", err, secrets.MintErrorClass(err), tc.class)
			}
			for _, want := range tc.mentions {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("Mint = %v, want it to name %q", err, want)
				}
			}
			if strings.Contains(err.Error(), "parent installation") {
				t.Fatalf("Mint = %v quotes GitHub's own text", err)
			}
		})
	}
}

// TestGitHubAppMintKeepsARateLimited422Upstream: a 422 GitHub answers because the App is rate
// limited (X-RateLimit-Remaining: 0, or a secondary-rate-limit message) is no verdict on the
// narrowing, so it stays the upstream class an operator watches, not the caller's narrowing, and
// its error names neither the repositories as a likely cause nor GitHub's own text. Neither names
// a wait, which counts as a minute, so the Retry ends the mint at its one token request.
func TestGitHubAppMintKeepsARateLimited422Upstream(t *testing.T) {
	key, err := testAppKey()
	if err != nil {
		t.Fatal(err)
	}
	github := newNarrowingGitHub(t)
	retry, clock, _ := testRetry(atTheTop)
	app := secrets.GitHubApp{Base: github.server.URL, HTTP: github.server.Client(), Retry: retry}
	value := githubAppValue(t, "123456", key)
	narrowing := &secrets.GitHubNarrowing{Owner: "ada", Repositories: []string{"proofs"}, ReadOnly: true}

	for _, tc := range []struct {
		name    string
		headers http.Header
		message string
	}{
		{"no requests left", http.Header{"X-Ratelimit-Remaining": {"0"}}, "Validation Failed"},
		{"a secondary rate limit", nil, "You have exceeded a secondary rate limit. Please wait a few minutes before you try again."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			github.setHeaders(tc.headers)
			github.setAnswer(func(string, []byte) (int, any) {
				return http.StatusUnprocessableEntity, map[string]string{"message": tc.message}
			})
			github.take()
			_, err := app.Mint(t.Context(), secrets.MintRequest{Value: value, GitHubOwners: []string{"acme-org", "ada"}, GitHubNarrowing: narrowing})
			if secrets.MintErrorClass(err) != "upstream" || !strings.Contains(err.Error(), "rate limit") || strings.Contains(err.Error(), "proofs") || strings.Contains(err.Error(), "few minutes") {
				t.Fatalf("Mint = %v (class %s), want class upstream naming the rate limit, not the narrowing or GitHub's text", err, secrets.MintErrorClass(err))
			}
			if _, tokens := github.take(); len(tokens) != 1 || secrets.MintAttempts(err) != 1 {
				t.Fatalf("Mint made %d token requests (%d attempts); want one, since a rate limit naming no wait ends the mint", len(tokens), secrets.MintAttempts(err))
			}
		})
	}
	if waits := clock.slept(); len(waits) != 0 {
		t.Fatalf("the mints waited %v; want no wait", waits)
	}
}

// TestGitHubAppMintRefusesATokenWiderThanTheNarrowing: Mint releases a narrowed token only when
// GitHub's own answer describes no more than the narrowing asked: for read-only, no permission
// past contents, metadata, pull_requests, actions, checks and statuses at read and not none named;
// for repositories, a selection of
// those alone, named. Anything else (a body something between dropped or widened) is refused as
// GitHub's fault (class upstream), and the token GitHub minted is revoked at GitHub with
// DELETE /installation/token, as its own bearer, before Mint returns. The error names which rule
// failed and never a value GitHub's answer holds, nor the token. A token not narrowed is not
// checked, and a repository GitHub names in another case than asked is the same repository.
func TestGitHubAppMintRefusesATokenWiderThanTheNarrowing(t *testing.T) {
	key, err := testAppKey()
	if err != nil {
		t.Fatal(err)
	}
	github := newNarrowingGitHub(t)
	app := secrets.GitHubApp{Base: github.server.URL, HTTP: github.server.Client()}
	value := githubAppValue(t, "123456", key)
	readOnly := &secrets.GitHubNarrowing{Owner: "ada", ReadOnly: true}
	proofs := &secrets.GitHubNarrowing{Owner: "ada", Repositories: []string{"proofs"}}
	answering := func(change func(map[string]any)) func(string, []byte) (int, any) {
		return func(installation string, body []byte) (int, any) {
			status, answer := asGitHubAnswers(installation, body)
			change(answer.(map[string]any))
			return status, answer
		}
	}
	// Every value a wider answer below holds that the request did not, so that no error quotes one.
	gitHubValues := []string{"write", "administration", "secrets", `"all"`, "SECRETS"}

	for _, tc := range []struct {
		name      string
		narrowing *secrets.GitHubNarrowing
		change    func(map[string]any)
		mentions  string
	}{
		{"write contents", readOnly, func(a map[string]any) { a["permissions"] = map[string]string{"contents": "write", "metadata": "read"} }, "more than read access"},
		{"another permission", readOnly, func(a map[string]any) {
			a["permissions"] = map[string]string{"administration": "write", "contents": "read", "metadata": "read"}
		}, "a permission past contents, metadata, pull requests, actions, checks and statuses"},
		{"no permissions named", readOnly, func(a map[string]any) { delete(a, "permissions") }, "names no permissions"},
		{"every repository", proofs, func(a map[string]any) { a["repository_selection"] = "all"; delete(a, "repositories") }, "repository selection"},
		{"another repository", proofs, func(a map[string]any) {
			a["repositories"] = []map[string]string{{"name": "proofs"}, {"name": "SECRETS"}}
		}, "a repository the narrowing does not name"},
		{"no repositories named", proofs, func(a map[string]any) { delete(a, "repositories") }, "names no repositories"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			github.setAnswer(answering(tc.change))
			release, err := app.Mint(t.Context(), secrets.MintRequest{Value: value, GitHubOwners: []string{"acme-org", "ada"}, GitHubNarrowing: tc.narrowing})
			if release.Value != "" || secrets.MintErrorClass(err) != "upstream" || !strings.Contains(err.Error(), tc.mentions) || strings.Contains(err.Error(), "token-") {
				t.Fatalf("Mint = %q, %v (class %s), want nothing released, class upstream naming %q and no token", release.Value, err, secrets.MintErrorClass(err), tc.mentions)
			}
			for _, quoted := range gitHubValues {
				if strings.Contains(err.Error(), quoted) {
					t.Fatalf("Mint = %v quotes %q from GitHub's answer", err, quoted)
				}
			}
			if revoked := github.takeRevoked(); !reflect.DeepEqual(revoked, []string{"Bearer token-2"}) {
				t.Fatalf("revokes at GitHub = %q, want one DELETE /installation/token as the refused token", revoked)
			}
		})
	}

	github.setAnswer(answering(func(a map[string]any) { a["repositories"] = []map[string]string{{"name": "Proofs"}} }))
	if release, err := app.Mint(t.Context(), secrets.MintRequest{Value: value, GitHubOwners: []string{"acme-org", "ada"}, GitHubNarrowing: proofs}); err != nil || !strings.Contains(release.Value, "token-2") {
		t.Fatalf("Mint with GitHub naming the repository in its own case = %q, %v; want ada's token", release.Value, err)
	}
	github.setAnswer(asGitHubAnswers)
	if release, err := app.Mint(t.Context(), secrets.MintRequest{Value: value, GitHubOwners: []string{"acme-org", "ada"}}); err != nil || !strings.Contains(release.Value, "token-1") || !strings.Contains(release.Value, "token-2") {
		t.Fatalf("Mint with no narrowing and GitHub's whole answer = %q, %v; want every listed owner's token", release.Value, err)
	}
	if revoked := github.takeRevoked(); len(revoked) != 0 {
		t.Fatalf("revokes of released tokens = %q, want none", revoked)
	}
}

// TestGitHubAppMintAsksForTheOptionalReadsTheInstallationHolds: an App holds actions, checks and
// statuses only where it declares them and each installation's account approved that grant, and
// GitHub answers 422 to a token request naming a permission the installation does not hold,
// minting nothing. A read-only narrowing asks, in its one token request, for the ones the
// installation's listing says it holds alone, so the release carries every read the installation
// holds rather than failing, and the broker logs at WARN which reads that token is without, naming
// no token. The broker's narrowing check (MintRequest.Check) makes the same token request and
// revokes the token it gets, but it releases nothing, so it does not log that line, whose words say
// a token was released for the installation: one check mints once for each repository it probes,
// and each would write that line for a release that never happened. A 422 to that request is still
// the caller's narrowing.
func TestGitHubAppMintAsksForTheOptionalReadsTheInstallationHolds(t *testing.T) {
	var logged bytes.Buffer
	logtest.SetDefault(t, slog.NewTextHandler(&logged, nil))
	key, err := testAppKey()
	if err != nil {
		t.Fatal(err)
	}
	github := newNarrowingGitHub(t)
	app := secrets.GitHubApp{Base: github.server.URL, HTTP: github.server.Client()}
	value := githubAppValue(t, "123456", key)
	readOnly := &secrets.GitHubNarrowing{Owner: "ada", ReadOnly: true}
	// missingReads is the release's WARN, read by the same words where the release must write it
	// and where the check must not, so a rewording fails the release's reading first rather than
	// passing the check's.
	const missingReads = "does not hold every read a read-only token asks for"

	for _, tc := range []struct {
		name    string
		held    map[string]string // what ada's installation holds
		body    string            // the release's one token request
		missing string            // the WARN's missing=, "" where it logs none
	}{
		{"every read", everyGrant, `{"permissions":{"actions":"read","checks":"read","contents":"read","metadata":"read","pull_requests":"read","statuses":"read"}}`, ""},
		{"no actions", heldWithout("actions"), `{"permissions":{"checks":"read","contents":"read","metadata":"read","pull_requests":"read","statuses":"read"}}`, "actions"},
		{"no checks", heldWithout("checks"), `{"permissions":{"actions":"read","contents":"read","metadata":"read","pull_requests":"read","statuses":"read"}}`, "checks"},
		{"no statuses", heldWithout("statuses"), `{"permissions":{"actions":"read","checks":"read","contents":"read","metadata":"read","pull_requests":"read"}}`, "statuses"},
		{"none of the three", heldWithout("actions", "checks", "statuses"), `{"permissions":{"contents":"read","metadata":"read","pull_requests":"read"}}`, "actions,checks,statuses"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logged.Reset()
			github.setHeld("2", tc.held)
			github.take()
			release, err := app.Mint(t.Context(), secrets.MintRequest{Value: value, GitHubOwners: []string{"acme-org", "ada"}, GitHubNarrowing: readOnly})
			if err != nil || !strings.Contains(release.Value, "token-2") {
				t.Fatalf("Mint for an installation holding %v = %q, %v; want ada's token", tc.held, release.Value, err)
			}
			asked := []tokenRequest{{installation: "2", body: tc.body, contentType: "application/json"}}
			if _, tokens := github.take(); !reflect.DeepEqual(tokens, asked) {
				t.Fatalf("token requests = %+v, want %+v", tokens, asked)
			}
			line := logged.String()
			if tc.missing == "" {
				if strings.Contains(line, missingReads) {
					t.Fatalf("log = %q; want no line about a missing read for an installation holding every one", line)
				}
			} else if !strings.Contains(line, missingReads) || !strings.Contains(line, "level=WARN") || !strings.Contains(line, "app=123456") ||
				!strings.Contains(line, "installation=2") || !strings.Contains(line, "missing="+tc.missing+"\n") {
				t.Fatalf("log = %q, want one WARN naming the App, the installation and missing=%s", line, tc.missing)
			}
			if strings.Contains(line, "token-") {
				t.Fatalf("log = %q holds the token", line)
			}
			if revoked := github.takeRevoked(); len(revoked) != 0 {
				t.Fatalf("revokes of the released token = %q, want none", revoked)
			}

			logged.Reset()
			checked, err := app.Mint(t.Context(), secrets.MintRequest{Value: value, GitHubOwners: []string{"acme-org", "ada"}, GitHubNarrowing: readOnly, Check: true})
			if err != nil || checked.Value != "" {
				t.Fatalf("a check of a read-only narrowing of an installation holding %v = %q, %v; want it passed with nothing released", tc.held, checked.Value, err)
			}
			if _, tokens := github.take(); !reflect.DeepEqual(tokens, asked) {
				t.Fatalf("the check's token requests = %+v, want the release's own, %+v", tokens, asked)
			}
			if revoked := github.takeRevoked(); len(revoked) != 1 {
				t.Fatalf("the check's revokes = %q, want the one token the check minted given back", revoked)
			}
			if strings.Contains(logged.String(), missingReads) {
				t.Fatalf("log = %q; want no line about a token released without a read, since a check releases none", logged.String())
			}
		})
	}

	// A 422 to the reads the installation holds is the narrowing's, and the error names the three
	// permissions every read-only token asks for rather than a grant it left out.
	github.setHeld("2", heldWithout("actions"))
	github.setAnswer(func(string, []byte) (int, any) {
		return http.StatusUnprocessableEntity, map[string]string{"message": "The permissions requested are not granted to this installation."}
	})
	github.take()
	release, err := app.Mint(t.Context(), secrets.MintRequest{Value: value, GitHubOwners: []string{"acme-org", "ada"}, GitHubNarrowing: readOnly})
	if release.Value != "" || secrets.MintErrorClass(err) != "narrowing" || !strings.Contains(err.Error(), "contents: read") {
		t.Fatalf("Mint refused = %q, %v (class %s); want nothing released and class narrowing naming the read access", release.Value, err, secrets.MintErrorClass(err))
	}
	if _, tokens := github.take(); len(tokens) != 1 {
		t.Fatalf("%d token requests for a refused read-only narrowing, want the one", len(tokens))
	}
}

// TestGitHubAppMintStillRefusesWhenGitHubDoesNotRevoke: a revoke GitHub does not confirm (here a
// 500, which the Retry asks three times, all of them the token itself) is logged at WARN, never
// naming the token, and the wider token is still refused.
func TestGitHubAppMintStillRefusesWhenGitHubDoesNotRevoke(t *testing.T) {
	var logged bytes.Buffer
	logtest.SetDefault(t, slog.NewTextHandler(&logged, nil))
	key, err := testAppKey()
	if err != nil {
		t.Fatal(err)
	}
	github := newNarrowingGitHub(t)
	github.setRevokeStatus(http.StatusInternalServerError)
	github.setAnswer(func(installation string, body []byte) (int, any) {
		status, answer := asGitHubAnswers(installation, body)
		answer.(map[string]any)["permissions"] = map[string]string{"contents": "write", "metadata": "read"}
		return status, answer
	})
	retry, _, _ := testRetry(atTheTop)
	app := secrets.GitHubApp{Base: github.server.URL, HTTP: github.server.Client(), Retry: retry}

	release, err := app.Mint(t.Context(), secrets.MintRequest{Value: githubAppValue(t, "123456", key), GitHubOwners: []string{"acme-org", "ada"}, GitHubNarrowing: &secrets.GitHubNarrowing{Owner: "ada", ReadOnly: true}})
	if release.Value != "" || secrets.MintErrorClass(err) != "upstream" {
		t.Fatalf("Mint = %q, %v (class %s), want nothing released and class upstream", release.Value, err, secrets.MintErrorClass(err))
	}
	if revoked := github.takeRevoked(); !reflect.DeepEqual(revoked, []string{"Bearer token-2", "Bearer token-2", "Bearer token-2"}) {
		t.Fatalf("revokes at GitHub = %q, want three, each as the refused token", revoked)
	}
	if line := logged.String(); !strings.Contains(line, "level=WARN") || !strings.Contains(line, "did not revoke") || strings.Contains(line, "token-2") {
		t.Fatalf("log = %q, want a WARN line that GitHub did not revoke the token, without the token", line)
	}
}

// TestGitHubAppMintReadsARetriedRevokesRefusalAsRevoked: GitHub revokes the token at the first
// DELETE /installation/token and that answer is lost (the connection drops before the 204), so the
// Retry makes the DELETE again, as the token GitHub has just revoked, and GitHub answers it 401, or
// 404 for a token it no longer knows. That is the revoke done: the wider token is still refused,
// and Mint logs no line saying GitHub did not revoke it. A 401 or a 404 to the first DELETE keeps
// its meaning, a revoke GitHub did not confirm, and is logged as one.
func TestGitHubAppMintReadsARetriedRevokesRefusalAsRevoked(t *testing.T) {
	wider := reply{status: http.StatusCreated, body: `{"token":"token-7","expires_at":"2026-11-14T13:00:00Z","permissions":{"contents":"write","metadata":"read"}}`}
	revokedAndLost := reply{lost: true}
	badCredentials := reply{status: http.StatusUnauthorized, body: `{"message":"Bad credentials"}`}
	notFound := reply{status: http.StatusNotFound, body: `{"message":"Not Found"}`}
	for _, tc := range []struct {
		name    string
		revokes []reply
		warned  bool
	}{
		{"the first answer lost, then GitHub's 401", []reply{revokedAndLost, badCredentials}, false},
		{"the first answer lost, then a 404", []reply{revokedAndLost, notFound}, false},
		{"a 401 to the first DELETE", []reply{badCredentials}, true},
		{"a 404 to the first DELETE", []reply{notFound}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logged bytes.Buffer
			logtest.SetDefault(t, slog.NewTextHandler(&logged, nil))
			retry, _, _ := testRetry(atTheBottom)
			up := newScripted(t, append([]reply{installed, wider}, tc.revokes...)...)
			release, err := mintGitHub(t.Context(), t, up, retry, &secrets.GitHubNarrowing{Owner: "acme", ReadOnly: true})
			if release.Value != "" || secrets.MintErrorClass(err) != "upstream" || !strings.Contains(err.Error(), "more than read access") {
				t.Fatalf("Mint = %q, %v (class %s); want nothing released and class upstream naming the read access", release.Value, err, secrets.MintErrorClass(err))
			}
			var deletes int
			for _, request := range up.requests() {
				if request.method == http.MethodDelete && request.path == "/installation/token" {
					deletes++
				}
			}
			if deletes != len(tc.revokes) {
				t.Fatalf("Mint sent %d revokes; want %d", deletes, len(tc.revokes))
			}
			if warned := strings.Contains(logged.String(), "did not revoke"); warned != tc.warned {
				t.Fatalf("log = %q; a line saying GitHub did not revoke the token: %t, want %t", logged.String(), warned, tc.warned)
			}
		})
	}
}

// TestGitHubAppMintReadsTheAnswerToANarrowingAtTheMaximum: GitHub's answer to a narrowed token
// carries a full repository object for each repository, about 5 KB, and GitHub has minted the
// token by the time it answers, so an answer Mint could not read whole would leave a token neither
// released nor revoked. At secrets.MaxNarrowedRepositories, with each object eight times GitHub's
// size (an answer over 4 MiB, past the 1 MiB other calls read to), Mint reads it, checks it and
// releases the token.
func TestGitHubAppMintReadsTheAnswerToANarrowingAtTheMaximum(t *testing.T) {
	key, err := testAppKey()
	if err != nil {
		t.Fatal(err)
	}
	github := newNarrowingGitHub(t)
	github.setAnswer(func(installation string, body []byte) (int, any) {
		status, answer := asGitHubAnswers(installation, body)
		repositories := answer.(map[string]any)["repositories"].([]map[string]string)
		for _, repository := range repositories {
			repository["description"] = strings.Repeat("x", 40<<10)
		}
		return status, answer
	})
	app := secrets.GitHubApp{Base: github.server.URL, HTTP: github.server.Client()}
	names := make([]string, secrets.MaxNarrowedRepositories)
	for i := range names {
		names[i] = fmt.Sprintf("repo-%03d", i)
	}

	release, err := app.Mint(t.Context(), secrets.MintRequest{Value: githubAppValue(t, "123456", key), GitHubOwners: []string{"acme-org", "ada"}, GitHubNarrowing: &secrets.GitHubNarrowing{Owner: "ada", Repositories: names, ReadOnly: true}})
	if err != nil || !strings.Contains(release.Value, "token-2") {
		t.Fatalf("Mint narrowed to %d repositories = %q, %v; want ada's token", len(names), release.Value, err)
	}
	if revoked := github.takeRevoked(); len(revoked) != 0 {
		t.Fatalf("revokes at GitHub = %q, want none", revoked)
	}
}
