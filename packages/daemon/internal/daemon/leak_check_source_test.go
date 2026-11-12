package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/githubtest"
	"github.com/sjawhar/legion/daemon/internal/leakguard"
	"github.com/sjawhar/legion/daemon/internal/leakguard/leakguardtest"
)

// leakCheckTag is the tag these tests read the check at, spelled as the worker image pins one.
const leakCheckTag = "leak-check-v15"

// leakCheckLease is the token the stand-in GitHub mints for the check's repository, and the bearer
// its tarball route requires.
const leakCheckLease = "ghs_leak_check_lease"

// leakCheckReadSet is what a read token asks for (appauth's readPermissions): contents, pull
// requests, Actions and metadata, each at read.
var leakCheckReadSet = map[string]string{"contents": "read", "pull_requests": "read", "actions": "read", "metadata": "read"}

// leakCheckGrant is what the stand-in's installations have granted the implement App: its write
// set, of which a read mint asks for the reading half.
var leakCheckGrant = map[string]string{"actions": "write", "checks": "read", "contents": "write",
	"metadata": "read", "pull_requests": "write", "statuses": "write", "workflows": "write"}

// leakCheckTarball is a GitHub tarball of the check's repository: every member under one
// `<owner>-<repo>-<sha>` directory, the check's launcher (leakguardtest.AnswersHelp) under its
// leak-check directory.
func leakCheckTarball(t *testing.T) []byte {
	t.Helper()
	return leakguardtest.GitHubTarball(t, map[string]leakguardtest.Member{
		leakguardtest.CheckName: {Body: "#!/bin/sh\n" + leakguardtest.AnswersHelp(filepath.Join(t.TempDir(), "help-runs")), Mode: 0o755},
	})
}

// leakCheckRepo is a stand-in for the check's repository: GitHub's tarball route at any tag,
// answered to the bearer leakCheckLease, with a count of every request made of it.
type leakCheckRepo struct {
	url   string
	asked atomic.Int32
}

func serveLeakCheckRepo(t *testing.T) *leakCheckRepo {
	t.Helper()
	raw := leakCheckTarball(t)
	repo := &leakCheckRepo{}
	repo.url = githubtest.Server(t, githubtest.Spec{Token: leakCheckLease,
		Record: func(*http.Request) { repo.asked.Add(1) },
		Handle: func(w http.ResponseWriter, r *http.Request) {
			if !strings.HasPrefix(r.URL.Path, "/repos/theorem-labs/agent-infra/tarball/") {
				githubtest.Unexpected(t, w, r)
				return
			}
			_, _ = w.Write(raw)
		}})
	return repo
}

// The check is read with a token of its own: the implement App's read token on theorem-labs's
// installation, narrowed to agent-infra alone at the reading permissions — one request, on that
// installation and no other — and that token reads the tarball.
func TestTheLeakCheckIsReadWithATokenNarrowedToItsRepository(t *testing.T) {
	const checkInstallation, otherInstallation = "71", "72"
	github := provisioningStandIn(t,
		provisioningInstallation{id: checkInstallation, owner: "theorem-labs", granted: leakCheckGrant,
			answer: func(w http.ResponseWriter, repositories []string, permissions map[string]string) {
				mintedToken(w, leakCheckLease, repositories, permissions)
			}},
		provisioningInstallation{id: otherInstallation, owner: "acme", granted: leakCheckGrant,
			answer: func(w http.ResponseWriter, repositories []string, permissions map[string]string) {
				mintedToken(w, "ghs_acme", repositories, permissions)
			}},
	)
	repo := serveLeakCheckRepo(t)
	source := leakCheckSource(github.tokens, repo.url)
	served, err := source.Get(context.Background(), leakCheckTag)
	if err != nil {
		t.Fatalf("the read of the check: %v", err)
	}
	if served.Tag != leakCheckTag || len(served.Body) == 0 || repo.asked.Load() != 1 {
		t.Fatalf("served %d bytes at %q after %d requests of the repository, want the check at %s from one read",
			len(served.Body), served.Tag, repo.asked.Load(), leakCheckTag)
	}
	requests := github.requestsTo(checkInstallation)
	if len(requests) != 1 {
		t.Fatalf("%d token requests on theorem-labs's installation, want the one read mint: %+v", len(requests), requests)
	}
	if !slices.Equal(requests[0].repositories, []string{"agent-infra"}) || !maps.Equal(requests[0].permissions, leakCheckReadSet) {
		t.Fatalf("the token request named %v with %v, want [agent-infra] with %v", requests[0].repositories, requests[0].permissions, leakCheckReadSet)
	}
	if other := github.requestsTo(otherInstallation); len(other) != 0 {
		t.Fatalf("%d token requests on another owner's installation, want none: %+v", len(other), other)
	}
}

// An installation that refuses the narrowing to agent-infra is refused with the narrowing's
// sentence, and no wider token is asked for in its place: the one token request is the narrowed
// read, and the repository is never read.
func TestARefusedNarrowingOfTheLeakCheckTokenAsksForNoWiderToken(t *testing.T) {
	const checkInstallation = "73"
	github := provisioningStandIn(t,
		provisioningInstallation{id: checkInstallation, owner: "theorem-labs", granted: leakCheckGrant,
			answer: func(w http.ResponseWriter, repositories []string, permissions map[string]string) {
				if slices.Equal(repositories, []string{"agent-infra"}) && maps.Equal(permissions, leakCheckReadSet) {
					provisioningRefusal(w)
					return
				}
				mintedToken(w, "ghs_the_owners_own_token", repositories, permissions)
			}},
	)
	repo := serveLeakCheckRepo(t)
	_, err := leakCheckSource(github.tokens, repo.url).Get(context.Background(), leakCheckTag)
	const want = leakguard.RefusalPrefix + "GitHub refused to narrow the daemon's implement App token to theorem-labs/agent-infra alone, and the daemon reads the check with no wider token: "
	if err == nil || !strings.HasPrefix(err.Error(), want) {
		t.Fatalf("a refused narrowing = %v, want the sentence %q", err, want)
	}
	if requests := github.requestsTo(checkInstallation); len(requests) != 1 || !maps.Equal(requests[0].permissions, leakCheckReadSet) {
		t.Fatalf("token requests %+v, want the narrowed read alone and no request for the owner's token", requests)
	}
	if asked := repo.asked.Load(); asked != 0 {
		t.Fatalf("%d requests of the repository, want none", asked)
	}
}

// A mint GitHub refuses for any other reason is refused with the mint's sentence: an App with no
// installation on theorem-labs (appauth.ErrNotInstalled), and an installation whose token
// exchange answers 401.
func TestAMintGitHubRefusesIsRefusedWithTheMintSentence(t *testing.T) {
	const want = leakguard.RefusalPrefix + "the daemon could not mint its read-only token for theorem-labs/agent-infra: "
	for _, tc := range []struct {
		name          string
		installations []provisioningInstallation
		// is, when set, is what the token function's own error carries.
		is error
	}{
		{"an App with no installation on theorem-labs", []provisioningInstallation{
			{id: "74", owner: "acme", granted: leakCheckGrant,
				answer: func(w http.ResponseWriter, _ []string, _ map[string]string) { w.WriteHeader(http.StatusTeapot) }},
		}, appauth.ErrNotInstalled},
		{"a token exchange answering 401", []provisioningInstallation{
			{id: "75", owner: "theorem-labs", granted: leakCheckGrant,
				answer: func(w http.ResponseWriter, _ []string, _ map[string]string) {
					w.WriteHeader(http.StatusUnauthorized)
					fmt.Fprint(w, `{"message":"Bad credentials"}`)
				}},
		}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			github := provisioningStandIn(t, tc.installations...)
			repo := serveLeakCheckRepo(t)
			_, err := leakCheckSource(github.tokens, repo.url).Get(context.Background(), leakCheckTag)
			if err == nil || !strings.HasPrefix(err.Error(), want) {
				t.Fatalf("a refused mint = %v, want the sentence %q", err, want)
			}
			if asked := repo.asked.Load(); asked != 0 {
				t.Fatalf("%d requests of the repository, want none", asked)
			}
			if tc.is != nil {
				if _, err := leakCheckToken(github.tokens)(context.Background()); !errors.Is(err, tc.is) {
					t.Fatalf("the token function's error = %v, want it to carry %v", err, tc.is)
				}
			}
			for _, installation := range tc.installations {
				requests := github.requestsTo(installation.id)
				if installation.owner != "theorem-labs" {
					if len(requests) != 0 {
						t.Fatalf("%d token requests on %s's installation, want none: %+v", len(requests), installation.owner, requests)
					}
					continue
				}
				if len(requests) == 0 {
					t.Fatalf("no token request on theorem-labs's installation, want the read mint GitHub refused")
				}
				for _, request := range requests {
					if !slices.Equal(request.repositories, []string{"agent-infra"}) || !maps.Equal(request.permissions, leakCheckReadSet) {
						t.Fatalf("a token request named %v with %v, want [agent-infra] with %v alone", request.repositories, request.permissions, leakCheckReadSet)
					}
				}
			}
		})
	}
}

// leakCheckBroker is a token_command of this test's own, in the shape of the secrets broker
// (appauth's tokencommand_test.go newFakeBroker, which is appauth's own): every run appends its
// LEGION_GITHUB_OWNER, LEGION_GITHUB_REPOS and LEGION_GITHUB_READ_ONLY to calls, and then either
// refuses the narrowing as the broker refuses one GitHub answered 422, or releases a token of the
// owner it was asked for.
type leakCheckBroker struct {
	command string
	calls   string
}

func newLeakCheckBroker(t *testing.T, refuse bool) leakCheckBroker {
	t.Helper()
	dir := t.TempDir()
	calls := filepath.Join(dir, "calls")
	answer := fmt.Sprintf(`printf '{"installations":{"%%s":{"installation_id":101,"token":"%s","expires_at":"%s"}}}' "$LEGION_GITHUB_OWNER"`,
		leakCheckLease, time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
	if refuse {
		answer = `echo "agent-secrets: the narrowing cannot be served: github app 12345: GitHub answered HTTP 422 to the narrowed token for $LEGION_GITHUB_OWNER" >&2; exit 1`
	}
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s|%%s|%%s\\n' \"$LEGION_GITHUB_OWNER\" \"$LEGION_GITHUB_REPOS\" \"$LEGION_GITHUB_READ_ONLY\" >>%q\n%s\n", calls, answer)
	command := filepath.Join(dir, "broker")
	if err := os.WriteFile(command, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return leakCheckBroker{command: command, calls: calls}
}

// runs is each run's owner, repositories and read-only flag, in order.
func (b leakCheckBroker) runs(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(b.calls)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
}

// In token mode the check's token is the operator's command run for theorem-labs, narrowed to
// agent-infra and read-only, and that token reads the check; a broker that refuses the narrowing
// is refused with the narrowing's sentence, after that one run and no wider one.
func TestTheLeakCheckTokenInTokenModeIsTheCommandsNarrowReadOnlyRelease(t *testing.T) {
	for _, tc := range []struct {
		name   string
		refuse bool
	}{
		{"a broker that releases the token", false},
		{"a broker that refuses the narrowing", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			broker := newLeakCheckBroker(t, tc.refuse)
			var tarballs atomic.Int32
			raw := leakCheckTarball(t)
			// GitHub as token mode reads it — the token's App, by the GraphQL viewer and the
			// App's own route, and its bot user — and the check's tarball route.
			github := githubtest.Serve(t, githubtest.Spec{Handle: func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodPost && r.URL.Path == "/graphql":
					fmt.Fprint(w, `{"data":{"viewer":{"login":"legion-implementer[bot]"}}}`)
				case r.URL.Path == "/apps/legion-implementer":
					fmt.Fprint(w, `{"id":12345,"slug":"legion-implementer"}`)
				case r.URL.Path == "/users/legion-implementer[bot]":
					fmt.Fprint(w, `{"id":271566630}`)
				case r.URL.Path == "/repos/theorem-labs/agent-infra/tarball/"+leakCheckTag:
					tarballs.Add(1)
					if r.Header.Get("Authorization") != "Bearer "+leakCheckLease {
						t.Errorf("the tarball read carried Authorization %q, want the released token", r.Header.Get("Authorization"))
					}
					_, _ = w.Write(raw)
				default:
					githubtest.Unexpected(t, w, r)
				}
			}})
			tokens := appauth.New(config.GitHubApps{Implement: config.GitHubApp{AppID: "12345", TokenCommand: broker.command}},
				appauth.Options{BaseURL: github.URL, HTTPClient: github.Client(), Stderr: io.Discard})

			served, err := leakCheckSource(tokens, github.URL).Get(context.Background(), leakCheckTag)
			if runs := broker.runs(t); !slices.Equal(runs, []string{"theorem-labs|agent-infra|1"}) {
				t.Fatalf("the command ran as %q, want once for theorem-labs, narrowed to agent-infra and read-only", runs)
			}
			if !tc.refuse {
				if err != nil || len(served.Body) == 0 || tarballs.Load() != 1 {
					t.Fatalf("the read with the released token = %d bytes, %v after %d reads; want the check from one read", len(served.Body), err, tarballs.Load())
				}
				return
			}
			const want = leakguard.RefusalPrefix + "GitHub refused to narrow the daemon's implement App token to theorem-labs/agent-infra alone, and the daemon reads the check with no wider token: "
			if err == nil || !strings.HasPrefix(err.Error(), want) {
				t.Fatalf("a broker refusing the narrowing = %v, want the sentence %q", err, want)
			}
			if tarballs.Load() != 0 {
				t.Fatalf("%d reads of the check, want none", tarballs.Load())
			}
		})
	}
}

// A daemon with no GitHub Apps has no reader of the check: the nil interface, never a nil
// *leakguard.Source inside one.
func TestADaemonWithNoAppsHasNoLeakCheckSource(t *testing.T) {
	if source := leakCheckSource(nil, ""); source != nil {
		t.Fatalf("the source with no Apps = %#v, want nil", source)
	}
}
