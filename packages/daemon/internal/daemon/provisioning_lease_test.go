package daemon

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/githubtest"
)

// provisioningTokenRequest is one installation token request a provisioning mint made: the
// installation it was addressed to, and the repositories and permissions its body named. Every
// mint of this daemon names permissions, so they are never nil in a request a provisioning mint
// made; no mint names a repository, the installation deciding what its token reaches
// (INFRA-2543), so repositories is nil in every one.
type provisioningTokenRequest struct {
	installation string
	repositories []string
	permissions  map[string]string
}

// provisioningInstallation is one owner's installation on the stand-in: the id installation
// discovery gives for owner, what owner's account has granted the App (granted, which the
// permissions read answers and every mint cuts its request down to), and how that installation's
// token exchange answers a request, given the repositories and the permissions its body named.
type provisioningInstallation struct {
	id      string
	owner   string
	granted map[string]string
	answer  func(w http.ResponseWriter, repositories []string, permissions map[string]string)
}

// provisioningGitHub is a GitHub stand-in over the App endpoints a provisioning mint calls —
// installation discovery, the token exchange and the App's identity — with an installation of its
// own per owner, and a record of every token request's body, so a test reads what each mint asked
// for and not only what it got back. It is appmint_test.go's gitHubStandIn with that record
// added, and the two answer a token request through the same pair of helpers (exchangeBody and
// mintedToken); the boot tests' mints depend on gitHubStandIn's own answers, which stay as they
// are.
type provisioningGitHub struct {
	tokens *appauth.Manager

	mu       sync.Mutex
	requests []provisioningTokenRequest
}

// requestsTo is the token requests the stand-in recorded for one installation, in the order they
// arrived.
func (g *provisioningGitHub) requestsTo(installation string) []provisioningTokenRequest {
	g.mu.Lock()
	defer g.mu.Unlock()
	var requests []provisioningTokenRequest
	for _, request := range g.requests {
		if request.installation == installation {
			requests = append(requests, request)
		}
	}
	return requests
}

// provisioningStandIn serves installations and returns the stand-in, whose tokens is a real App
// token manager pointed at it, the implement App id 1 and the review App id 2.
func provisioningStandIn(t *testing.T, installations ...provisioningInstallation) *provisioningGitHub {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	privatePEM := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	byID := make(map[string]provisioningInstallation, len(installations))
	for _, installation := range installations {
		byID[installation.id] = installation
	}
	github := &provisioningGitHub{}
	server := githubtest.Serve(t, githubtest.Spec{Handle: func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/app/installations":
			discovered := make([]string, 0, len(installations))
			for _, installation := range installations {
				discovered = append(discovered, fmt.Sprintf(`{"id":%s,"account":{"login":%q}}`, installation.id, installation.owner))
			}
			fmt.Fprintf(w, "[%s]", strings.Join(discovered, ","))
		case strings.HasSuffix(r.URL.Path, "/access_tokens"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/app/installations/"), "/access_tokens")
			installation, known := byID[id]
			if !known {
				t.Errorf("a token request for the unknown installation %q", id)
				http.NotFound(w, r)
				return
			}
			repositories, permissions := exchangeBody(t, r)
			github.mu.Lock()
			github.requests = append(github.requests, provisioningTokenRequest{installation: id, repositories: repositories, permissions: permissions})
			github.mu.Unlock()
			installation.answer(w, repositories, permissions)
		// The permissions read every mint makes before its exchange: what the owner's account
		// has granted the App, which the request is cut down to (appauth's exchangeGranted).
		case strings.HasPrefix(r.URL.Path, "/app/installations/"):
			id := strings.TrimPrefix(r.URL.Path, "/app/installations/")
			installation, known := byID[id]
			if !known {
				t.Errorf("a permissions read of the unknown installation %q", id)
				http.NotFound(w, r)
				return
			}
			granted, err := json.Marshal(installation.granted)
			if err != nil {
				t.Errorf("encode %s's granted permissions: %v", installation.owner, err)
			}
			fmt.Fprintf(w, `{"id":%s,"permissions":%s}`, id, granted)
		case r.URL.Path == "/app":
			fmt.Fprint(w, `{"slug":"legion-test"}`)
		case strings.HasPrefix(r.URL.Path, "/users/"):
			fmt.Fprint(w, `{"id":42}`)
		default:
			githubtest.Unexpected(t, w, r)
		}
	}})
	github.tokens = appauth.New(config.GitHubApps{
		Implement: config.GitHubApp{AppID: "1", PrivateKey: privatePEM},
		Review:    config.GitHubApp{AppID: "2", PrivateKey: privatePEM},
	}, appauth.Options{BaseURL: server.URL, HTTPClient: server.Client(), Stderr: io.Discard})
	return github
}

// exchangeBody is the repositories and the permissions a token request's body named, each nil
// where the body named none. Every stand-in GitHub of this package answers from it, as GitHub
// answers a token request from the body it was sent (mintedToken).
func exchangeBody(t *testing.T, r *http.Request) ([]string, map[string]string) {
	t.Helper()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		t.Errorf("read a token request's body: %v", err)
		return nil, nil
	}
	if len(body) == 0 {
		return nil, nil
	}
	var payload struct {
		Repositories []string          `json:"repositories"`
		Permissions  map[string]string `json:"permissions"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Errorf("decode the token request body %q: %v", body, err)
	}
	return payload.Repositories, payload.Permissions
}

// mintedToken answers a token request with token, as GitHub's own answer is shaped: the selection
// of the repositories the request named, which no mint of this daemon names, and the permissions
// it asked for, which GitHub names in every answer it makes and the mint holds its answer to
// (appauth's narrowedTo).
func mintedToken(w http.ResponseWriter, token string, repositories []string, permissions map[string]string) {
	named := make([]string, 0, len(repositories))
	for _, repository := range repositories {
		named = append(named, fmt.Sprintf(`{"name":%q}`, repository))
	}
	// A map of strings always marshals, and json.Marshal sorts its keys.
	held, _ := json.Marshal(permissions)
	fmt.Fprintf(w, `{"token":%q,"expires_at":%q,"repository_selection":"selected","repositories":[%s],"permissions":%s}`,
		token, time.Now().Add(time.Hour).Format(time.RFC3339), strings.Join(named, ","), held)
}

// provisioningRefusal is GitHub's 422 to a token request for a permission the installation is
// not granted, which it answers by minting nothing rather than by leaving that permission out.
// It carries no rate-limit signal, so appauth reads it as the narrowing refused
// (ErrNarrowingRefused) rather than as GitHub's trouble.
func provisioningRefusal(w http.ResponseWriter) {
	w.WriteHeader(http.StatusUnprocessableEntity)
	fmt.Fprint(w, `{"message":"Validation Failed","errors":[{"resource":"Permission","code":"invalid"}]}`)
}

// The Sandbox runtime's provisioning token is a reading lease: implementTokens mints the
// implement App's installation token on the owner of the repository the pod clones, narrowed to
// contents, pull requests, Actions and metadata at read, which is all a clone, a fetch and a
// repository cache do (INFRA-1254, INFRA-513).
// Four installations, one per case, so no case's mints or cached leases reach another's:
//
//   - one granted the implement App's whole set: the one request names the reading set and no
//     repository, and that token is served;
//   - one not granted Actions: the mint asks for what the installation is granted of the reading
//     set, so the one request leaves actions out and the adapter is served that token, with no
//     second call;
//   - one that refuses the reading request: the refusal is returned as it came and nothing else
//     is minted. It once fell back to the App's own write token for that owner, from when a
//     request named the repository and a 422 could mean the installation did not reach it; no
//     request names a repository now (INFRA-2543), so that write token would reach the owner's
//     whole installation, and a refused reading lease fails the mint rather than hand it out;
//   - one in GitHub's trouble (a 500): the error is returned as it came, with no second mint and
//     nothing logged.
func TestTheProvisioningTokenIsAReadingLease(t *testing.T) {
	const (
		readingOwner = "acme-reading"
		narrowOwner  = "acme-no-actions"
		refusedOwner = "acme-no-reading"
		flakyOwner   = "acme-flaky"

		readingInstallation = "11"
		narrowInstallation  = "22"
		refusedInstallation = "33"
		flakyInstallation   = "44"
	)
	readingRepo := ghrepo.MustParse(readingOwner + "/legion")
	narrowRepo := ghrepo.MustParse(narrowOwner + "/legion")
	refusedRepo := ghrepo.MustParse(refusedOwner + "/legion")
	flakyRepo := ghrepo.MustParse(flakyOwner + "/legion")
	readingSet := map[string]string{"contents": "read", "pull_requests": "read", "actions": "read", "metadata": "read"}
	withoutActions := map[string]string{"contents": "read", "pull_requests": "read", "metadata": "read"}
	// writeSet is what the implement App's write token asks for (appauth's writePermissions),
	// which is what each installation below is granted, bar the one its own case takes a
	// permission from.
	writeSet := map[string]string{"actions": "write", "checks": "read", "contents": "write",
		"metadata": "read", "pull_requests": "write", "statuses": "write", "workflows": "write"}
	withoutActionsGranted := map[string]string{"checks": "read", "contents": "write",
		"metadata": "read", "pull_requests": "write", "statuses": "write", "workflows": "write"}
	// allRead is whether every permission a request named is a read, which is the reading
	// request and never the write token's.
	allRead := func(permissions map[string]string) bool {
		for _, access := range permissions {
			if access != "read" {
				return false
			}
		}
		return len(permissions) > 0
	}

	github := provisioningStandIn(t,
		provisioningInstallation{id: readingInstallation, owner: readingOwner, granted: writeSet,
			answer: func(w http.ResponseWriter, repositories []string, permissions map[string]string) {
				mintedToken(w, "reading-lease", repositories, permissions)
			}},
		// An installation whose account has not granted the App Actions: the permissions read
		// says so, and the mint leaves actions out of its request rather than be refused with it.
		provisioningInstallation{id: narrowInstallation, owner: narrowOwner, granted: withoutActionsGranted,
			answer: func(w http.ResponseWriter, repositories []string, permissions map[string]string) {
				if permissions["actions"] != "" {
					provisioningRefusal(w)
					return
				}
				mintedToken(w, "narrowed-lease", repositories, permissions)
			}},
		// An installation that refuses the narrowed read — a grant that changed between the
		// permissions read and the exchange reaches the adapter as a 422 — and would mint the
		// App's own write token if asked, which the adapter never does.
		provisioningInstallation{id: refusedInstallation, owner: refusedOwner, granted: writeSet,
			answer: func(w http.ResponseWriter, repositories []string, permissions map[string]string) {
				if allRead(permissions) {
					provisioningRefusal(w)
					return
				}
				mintedToken(w, "own-installation-token", repositories, permissions)
			}},
		provisioningInstallation{id: flakyInstallation, owner: flakyOwner, granted: writeSet,
			answer: func(w http.ResponseWriter, _ []string, _ map[string]string) {
				w.WriteHeader(http.StatusInternalServerError)
				fmt.Fprint(w, `{"message":"Server Error"}`)
			}},
	)
	// The adapter as sandboxRuntime builds it, with the identity serving map of a deployment that
	// configures no rename.
	adapter := newImplementTokens(github.tokens, func(repo ghrepo.Repository) ghrepo.Repository { return repo })
	ctx := context.Background()

	// The request carries the reading permissions and no repository, and that token is what a pod
	// clones with.
	token, err := adapter.Token(ctx, readingRepo)
	if err != nil || token != "reading-lease" {
		t.Fatalf("the token for %s = %q, %v; want the reading lease", readingRepo, token, err)
	}
	requests := github.requestsTo(readingInstallation)
	if len(requests) != 1 {
		t.Fatalf("%d token requests for %s, want the one read mint: %+v", len(requests), readingRepo, requests)
	}
	if !maps.Equal(requests[0].permissions, readingSet) {
		t.Fatalf("the token request asked for %v, want %v", requests[0].permissions, readingSet)
	}

	// An installation not granted actions: the mint asks for what it is granted of the reading
	// set, so the one request leaves actions out and the adapter is served that token.
	token, err = adapter.Token(ctx, narrowRepo)
	if err != nil || token != "narrowed-lease" {
		t.Fatalf("the token for %s = %q, %v; want the lease narrowed past actions", narrowRepo, token, err)
	}
	requests = github.requestsTo(narrowInstallation)
	if len(requests) != 1 {
		t.Fatalf("%d token requests for %s, want the one mint the granted cut makes: %+v", len(requests), narrowRepo, requests)
	}
	if !maps.Equal(requests[0].permissions, withoutActions) {
		t.Fatalf("the token request asked for %v, want %v: the reading set without actions", requests[0].permissions, withoutActions)
	}

	// An installation that refuses the reading request: the refusal is the adapter's answer, and
	// the one request is all GitHub is asked.
	token, err = adapter.Token(ctx, refusedRepo)
	if !appauth.NarrowingRefused(err) || token != "" {
		t.Fatalf("the token for %s = %q, %v; want the refusal returned and no token", refusedRepo, token, err)
	}
	requests = github.requestsTo(refusedInstallation)
	if len(requests) != 1 {
		t.Fatalf("%d token requests for %s, want the read mint alone: %+v", len(requests), refusedRepo, requests)
	}
	if !maps.Equal(requests[0].permissions, readingSet) {
		t.Fatalf("the token request asked for %v, want the reading set %v", requests[0].permissions, readingSet)
	}

	// GitHub's trouble is returned as it came, and no second mint is made.
	token, err = adapter.Token(ctx, flakyRepo)
	var transient *appauth.TransientError
	if !errors.As(err, &transient) || token != "" {
		t.Fatalf("the token for %s = %q, %v; want GitHub's trouble returned", flakyRepo, token, err)
	}
	if errors.Is(err, appauth.ErrNarrowingRefused) {
		t.Fatalf("a 500 = %v, want it never read as the narrowing the installation refused", err)
	}
	requests = github.requestsTo(flakyInstallation)
	if len(requests) == 0 {
		t.Fatalf("no token request for %s, want the read mint that failed", flakyRepo)
	}
	for _, request := range requests {
		if request.permissions["contents"] != "read" {
			t.Fatalf("a token request for %s asked for %v, want no mint of the App's write token: %+v", flakyRepo, request.permissions, requests)
		}
	}

	// No request names a repository: the installation decides what the lease reaches
	// (INFRA-2543), and the repository the pod clones keys the lease alone.
	for _, installation := range []string{readingInstallation, narrowInstallation, refusedInstallation, flakyInstallation} {
		for _, request := range github.requestsTo(installation) {
			if request.repositories != nil {
				t.Fatalf("a token request to installation %s named %v, want no repository", installation, request.repositories)
			}
		}
	}
}

// A 422 carrying GitHub's rate limit is GitHub's trouble and not its answer about the
// installation, and the adapter tells the two apart so that a caller reading its failure reads
// a rate limit as nothing about what the account grants: the trouble is returned as it came,
// and the next provisioning asks again.
func TestARateLimited422OfTheProvisioningReadIsNoFallbackToTheWriteToken(t *testing.T) {
	const (
		limitedOwner        = "acme-rate-limited"
		limitedInstallation = "55"
	)
	limitedRepo := ghrepo.MustParse(limitedOwner + "/legion")
	writeSet := map[string]string{"actions": "write", "checks": "read", "contents": "write",
		"metadata": "read", "pull_requests": "write", "statuses": "write", "workflows": "write"}

	github := provisioningStandIn(t,
		provisioningInstallation{id: limitedInstallation, owner: limitedOwner, granted: writeSet,
			answer: func(w http.ResponseWriter, _ []string, _ map[string]string) {
				// GitHub's secondary rate limit, which it answers 422 to a token request with.
				w.WriteHeader(http.StatusUnprocessableEntity)
				fmt.Fprint(w, `{"message":"You have exceeded a secondary rate limit. Please wait a few minutes before you try again."}`)
			}},
	)
	adapter := newImplementTokens(github.tokens, func(repo ghrepo.Repository) ghrepo.Repository { return repo })

	token, err := adapter.Token(context.Background(), limitedRepo)
	var transient *appauth.TransientError
	if !errors.As(err, &transient) || token != "" {
		t.Fatalf("the token for %s = %q, %v; want GitHub's trouble returned", limitedRepo, token, err)
	}
	if appauth.NarrowingRefused(err) {
		t.Fatalf("a rate-limited 422 = %v, want it never read as the narrowing the installation refused", err)
	}
	for _, request := range github.requestsTo(limitedInstallation) {
		if request.permissions["contents"] != "read" {
			t.Fatalf("a token request asked for %v, want the reading lease and no second mint", request.permissions)
		}
		if request.repositories != nil {
			t.Fatalf("a token request named %v, want no repository", request.repositories)
		}
	}
}
