package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
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
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/githubtest"
	"github.com/sjawhar/legion/daemon/internal/leakguard"
	"github.com/sjawhar/legion/daemon/internal/leakguard/leakguardtest"
)

// leakCheckTag is the tag these tests read the check at, spelled as the worker image pins one.
const leakCheckTag = "leak-check-v15"

// leakCheckLease is the token the stand-in GitHub mints for the check's repository, and the bearer
// its tarball route requires.
const leakCheckLease = "ghs_leak_check_lease"

// leakCheckSetting is the deployment's shared leak check these tests read, as config's leak_check
// settles it: a stand-in repository, whose owner's installation the read lease is minted on, and
// a stand-in checker.
var leakCheckSetting = leakguard.Setting{Repository: ghrepo.MustParse("example-org/deployment"), ManifestSHA256: strings.Repeat("5e", 32)}

// leakCheckTarballRoute is the path GitHub serves the check's repository's tarball under, a tag
// after it.
var leakCheckTarballRoute = "/repos/" + leakCheckSetting.Repository.String() + "/tarball/"

// leakCheckReadSet is what a read token asks for of an installation granted leakCheckGrant: the
// read lease's set cut to that grant, which holds no issues and no attestations, so contents,
// pull requests, Actions, metadata, checks and commit statuses, each at read.
var leakCheckReadSet = map[string]string{"contents": "read", "pull_requests": "read", "actions": "read",
	"metadata": "read", "checks": "read", "statuses": "read"}

// leakCheckGrant is what the stand-in's installations have granted the implement App: its write
// set less issues, security events and attestations, of which a read mint asks for the reading
// half.
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
			if !strings.HasPrefix(r.URL.Path, leakCheckTarballRoute) {
				githubtest.Unexpected(t, w, r)
				return
			}
			_, _ = w.Write(raw)
		}})
	return repo
}

// The check is read from the repository the deployment names with a token of its own: the
// implement App's read lease on that repository's owner's installation, at the reading
// permissions — one request, on that installation and no other, and naming no repository, the
// installation deciding what it reaches — and that token reads the tarball.
func TestTheLeakCheckIsReadWithAReadLeaseOnItsOwner(t *testing.T) {
	const checkInstallation, otherInstallation = "71", "72"
	github := provisioningStandIn(t,
		provisioningInstallation{id: checkInstallation, owner: "example-org", granted: leakCheckGrant,
			answer: func(w http.ResponseWriter, permissions map[string]string) {
				mintedToken(w, leakCheckLease, permissions)
			}},
		provisioningInstallation{id: otherInstallation, owner: "acme", granted: leakCheckGrant,
			answer: func(w http.ResponseWriter, permissions map[string]string) {
				mintedToken(w, "ghs_acme", permissions)
			}},
	)
	repo := serveLeakCheckRepo(t)
	source := leakCheckSource(github.tokens, repo.url, leakCheckSetting)
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
		t.Fatalf("%d token requests on example-org's installation, want the one read mint: %+v", len(requests), requests)
	}
	if !maps.Equal(requests[0].permissions, leakCheckReadSet) {
		t.Fatalf("the token request asked for %v, want %v", requests[0].permissions, leakCheckReadSet)
	}
	if other := github.requestsTo(otherInstallation); len(other) != 0 {
		t.Fatalf("%d token requests on another owner's installation, want none: %+v", len(other), other)
	}
}

// An installation the account has not granted the reading permissions is refused with the
// refusal's own sentence, and no wider token is asked for in its place: the one token request is
// the read mint, and the repository is never read.
func TestARefusedReadingLeaseForTheLeakCheckAsksForNoWiderToken(t *testing.T) {
	const checkInstallation = "73"
	github := provisioningStandIn(t,
		provisioningInstallation{id: checkInstallation, owner: "example-org", granted: leakCheckGrant,
			answer: func(w http.ResponseWriter, permissions map[string]string) {
				if maps.Equal(permissions, leakCheckReadSet) {
					provisioningRefusal(w)
					return
				}
				mintedToken(w, "ghs_the_owners_own_token", permissions)
			}},
	)
	repo := serveLeakCheckRepo(t)
	_, err := leakCheckSource(github.tokens, repo.url, leakCheckSetting).Get(context.Background(), leakCheckTag)
	const want = leakguard.RefusalPrefix + "GitHub refused the daemon's implement App the reading permissions on example-org, and the daemon reads example-org/deployment with no wider token: "
	if err == nil || !strings.HasPrefix(err.Error(), want) {
		t.Fatalf("a refused narrowing = %v, want the sentence %q", err, want)
	}
	if requests := github.requestsTo(checkInstallation); len(requests) != 1 || !maps.Equal(requests[0].permissions, leakCheckReadSet) {
		t.Fatalf("token requests %+v, want the read mint alone and no request for the owner's token", requests)
	}
	if asked := repo.asked.Load(); asked != 0 {
		t.Fatalf("%d requests of the repository, want none", asked)
	}
}

// A read of the check GitHub answers 401 drops the read lease the App's token manager cached for
// the check's repository, so the next read mints a new token: two token requests, the second read
// served. A read answered 404 drops nothing, and the next read is made with the cached lease: one
// token request.
func TestALeakCheckReadAnswered401MintsANewTokenAndA404DoesNot(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   int
		requests int
	}{
		{"a read answered 401", http.StatusUnauthorized, 2},
		{"a read answered 404", http.StatusNotFound, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const checkInstallation = "76"
			github := provisioningStandIn(t,
				provisioningInstallation{id: checkInstallation, owner: "example-org", granted: leakCheckGrant,
					answer: func(w http.ResponseWriter, permissions map[string]string) {
						mintedToken(w, leakCheckLease, permissions)
					}},
			)
			raw := leakCheckTarball(t)
			var asked atomic.Int32
			url := githubtest.Server(t, githubtest.Spec{Token: leakCheckLease, Handle: func(w http.ResponseWriter, r *http.Request) {
				if !strings.HasPrefix(r.URL.Path, leakCheckTarballRoute) {
					githubtest.Unexpected(t, w, r)
					return
				}
				if asked.Add(1) == 1 {
					w.WriteHeader(tc.status)
					fmt.Fprintf(w, `{"status":"%d"}`, tc.status)
					return
				}
				_, _ = w.Write(raw)
			}})
			source := leakCheckSource(github.tokens, url, leakCheckSetting)

			want := fmt.Sprintf("%sGitHub answered the read of example-org/deployment at %s with HTTP %d", leakguard.RefusalPrefix, leakCheckTag, tc.status)
			if _, err := source.Get(context.Background(), leakCheckTag); err == nil || !strings.HasPrefix(err.Error(), want) {
				t.Fatalf("the first read = %v, want the sentence %q", err, want)
			}
			served, err := source.Get(context.Background(), leakCheckTag)
			if err != nil || len(served.Body) == 0 || asked.Load() != 2 {
				t.Fatalf("the second read = %d bytes, %v after %d reads; want the check from a second read", len(served.Body), err, asked.Load())
			}
			requests := github.requestsTo(checkInstallation)
			if len(requests) != tc.requests {
				t.Fatalf("%d token requests on example-org's installation, want %d: %+v", len(requests), tc.requests, requests)
			}
			for _, request := range requests {
				if !maps.Equal(request.permissions, leakCheckReadSet) {
					t.Fatalf("a token request asked for %v, want %v", request.permissions, leakCheckReadSet)
				}
			}
		})
	}
}

// A mint GitHub refuses for any other reason is refused with the mint's sentence: an App with no
// installation on the check's owner (appauth.ErrNotInstalled), and an installation whose token
// exchange answers 401.
func TestAMintGitHubRefusesIsRefusedWithTheMintSentence(t *testing.T) {
	const want = leakguard.RefusalPrefix + "the daemon could not mint its read-only token for example-org/deployment: "
	for _, tc := range []struct {
		name          string
		installations []provisioningInstallation
		// is, when set, is what the token function's own error carries.
		is error
	}{
		{"an App with no installation on the check's owner", []provisioningInstallation{
			{id: "74", owner: "acme", granted: leakCheckGrant,
				answer: func(w http.ResponseWriter, _ map[string]string) { w.WriteHeader(http.StatusTeapot) }},
		}, appauth.ErrNotInstalled},
		{"a token exchange answering 401", []provisioningInstallation{
			{id: "75", owner: "example-org", granted: leakCheckGrant,
				answer: func(w http.ResponseWriter, _ map[string]string) {
					w.WriteHeader(http.StatusUnauthorized)
					fmt.Fprint(w, `{"message":"Bad credentials"}`)
				}},
		}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			github := provisioningStandIn(t, tc.installations...)
			repo := serveLeakCheckRepo(t)
			_, err := leakCheckSource(github.tokens, repo.url, leakCheckSetting).Get(context.Background(), leakCheckTag)
			if err == nil || !strings.HasPrefix(err.Error(), want) {
				t.Fatalf("a refused mint = %v, want the sentence %q", err, want)
			}
			if asked := repo.asked.Load(); asked != 0 {
				t.Fatalf("%d requests of the repository, want none", asked)
			}
			if tc.is != nil {
				if _, err := leakCheckToken(github.tokens, leakCheckSetting.Repository)(context.Background()); !errors.Is(err, tc.is) {
					t.Fatalf("the token function's error = %v, want it to carry %v", err, tc.is)
				}
			}
			for _, installation := range tc.installations {
				requests := github.requestsTo(installation.id)
				if installation.owner != "example-org" {
					if len(requests) != 0 {
						t.Fatalf("%d token requests on %s's installation, want none: %+v", len(requests), installation.owner, requests)
					}
					continue
				}
				if len(requests) == 0 {
					t.Fatalf("no token request on example-org's installation, want the read mint GitHub refused")
				}
				for _, request := range requests {
					if !maps.Equal(request.permissions, leakCheckReadSet) {
						t.Fatalf("a token request asked for %v, want %v alone", request.permissions, leakCheckReadSet)
					}
				}
			}
		})
	}
}

// leakCheckBroker is a token_command of this test's own, in the shape of the secrets broker
// (appauth's tokencommand_test.go newFakeBroker, which is appauth's own): every run appends its
// LEGION_GITHUB_OWNER and LEGION_GITHUB_READ_ONLY to calls, and then either
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
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s|%%s\\n' \"$LEGION_GITHUB_OWNER\" \"$LEGION_GITHUB_READ_ONLY\" >>%q\n%s\n", calls, answer)
	command := filepath.Join(dir, "broker")
	if err := os.WriteFile(command, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return leakCheckBroker{command: command, calls: calls}
}

// runs is each run's owner and read-only flag, in order.
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

// In token mode the check's token is the operator's command run for the check's owner, read-only,
// and that token reads the check; a broker that refuses the narrowing is refused with the
// refusal's sentence, after that one run and no wider one.
func TestTheLeakCheckTokenInTokenModeIsTheCommandsReadOnlyRelease(t *testing.T) {
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
				case r.URL.Path == leakCheckTarballRoute+leakCheckTag:
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

			served, err := leakCheckSource(tokens, github.URL, leakCheckSetting).Get(context.Background(), leakCheckTag)
			if runs := broker.runs(t); !slices.Equal(runs, []string{"example-org|1"}) {
				t.Fatalf("the command ran as %q, want once for example-org and read-only", runs)
			}
			if !tc.refuse {
				if err != nil || len(served.Body) == 0 || tarballs.Load() != 1 {
					t.Fatalf("the read with the released token = %d bytes, %v after %d reads; want the check from one read", len(served.Body), err, tarballs.Load())
				}
				return
			}
			const want = leakguard.RefusalPrefix + "GitHub refused the daemon's implement App the reading permissions on example-org, and the daemon reads example-org/deployment with no wider token: "
			if err == nil || !strings.HasPrefix(err.Error(), want) {
				t.Fatalf("a broker refusing the narrowing = %v, want the sentence %q", err, want)
			}
			if tarballs.Load() != 0 {
				t.Fatalf("%d reads of the check, want none", tarballs.Load())
			}
		})
	}
}

// A daemon with no GitHub Apps has no reader of the check, and neither has a daemon whose
// deployment names no shared leak check, though it holds Apps to read one with: each answer is the
// nil interface, never a nil *leakguard.Source inside one. Under the deployment's setting a daemon
// with Apps reads the repository the setting names.
func TestADaemonWithNoAppsOrNoLeakCheckSettingHasNoLeakCheckSource(t *testing.T) {
	if source := leakCheckSource(nil, "", leakCheckSetting); source != nil {
		t.Fatalf("the source with no Apps = %#v, want nil", source)
	}
	github := provisioningStandIn(t)
	if source := leakCheckSource(github.tokens, "", leakguard.Setting{}); source != nil {
		t.Fatalf("the source of a deployment that names no leak check = %#v, want nil", source)
	}
	source, ok := leakCheckSource(github.tokens, "", leakCheckSetting).(*leakguard.Source)
	if !ok || source.Repository != leakCheckSetting.Repository {
		t.Fatalf("the source under the deployment's setting = %#v, want a *leakguard.Source of %s", source, leakCheckSetting.Repository)
	}
}

// The deployment's shared leak check reaches the runtime that starts each role, under either
// runtime: the Sandbox runtime's Options carry the whole setting, which it reads the check with
// and tells every role the checker of, and the tmux runtime's carry the checker, which every pane
// is told. A daemon that dropped either would start roles that never ran the deployment's check.
func TestTheLeakCheckSettingReachesEitherRuntime(t *testing.T) {
	cfg := config.Config{Runtime: config.Runtime{Name: "tmux"}, LeakCheck: leakCheckSetting}
	if got := tmuxOptions(cfg, "test", nil, "", "", quietLogger()).LeakCheckSHA256; got != leakCheckSetting.ManifestSHA256 {
		t.Errorf("tmux LeakCheckSHA256 = %q, want the deployment's checker %q", got, leakCheckSetting.ManifestSHA256)
	}
	k := kubernetesConfig(t, "https://127.0.0.1:1")
	k.LeakCheck = leakCheckSetting
	opts, err := sandboxOptions(k, *k.Runtime.Kubernetes, "test", "", lookup(nil), quietLogger())
	if err != nil {
		t.Fatalf("sandboxOptions: %v", err)
	}
	if opts.LeakCheckSetting != leakCheckSetting {
		t.Errorf("sandbox LeakCheckSetting = %+v, want the deployment's setting %+v", opts.LeakCheckSetting, leakCheckSetting)
	}
}

// The boot says once which shared leak check the deployment runs: under the deployment's setting,
// one line naming the repository the check is read from and the manifest digest every role is held
// to; under none, one plain line naming the two settings that would name one.
func TestTheBootSaysOnceWhichLeakCheckIsConfigured(t *testing.T) {
	const notConfigured = "daemon: the deployment leak check is not configured (LEGION_LEAK_CHECK_REPOSITORY and LEGION_LEAK_CHECK_MANIFEST_SHA256, or leak_check.repository and leak_check.manifest_sha256 in legion.yaml): role pods start without it, and no write is refused for its absence"
	const configured = "daemon: the deployment leak check is configured: role pods install it from the repository at the worker image's pin, and every role is held to its manifest digest"
	for _, tc := range []struct {
		name    string
		setting leakguard.Setting
		want    []string
	}{
		{name: "no setting", setting: leakguard.Setting{}, want: []string{`level=INFO msg="` + notConfigured + `"`}},
		{name: "the deployment's setting", setting: leakCheckSetting, want: []string{`level=INFO msg="` + configured +
			`" repository=example-org/deployment manifest_sha256=` + leakCheckSetting.ManifestSHA256}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf strings.Builder
			logLeakCheck(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{ReplaceAttr: dropTime})), tc.setting)
			var got []string
			for logged := range strings.Lines(buf.String()) {
				got = append(got, strings.TrimSuffix(logged, "\n"))
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("the boot logged %q, want %q", got, tc.want)
			}
		})
	}
}

// dropTime leaves a record's time out, so a line reads the same at every run.
func dropTime(groups []string, attr slog.Attr) slog.Attr {
	if len(groups) == 0 && attr.Key == slog.TimeKey {
		return slog.Attr{}
	}
	return attr
}
