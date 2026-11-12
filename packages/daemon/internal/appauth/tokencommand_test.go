package appauth

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/githubtest"
)

// fakeBroker is a token_command that answers as `agent-secrets <NAME> [--github-owner O
// [--github-read-only]] -- sh -c 'printf %s "$NAME"'` does: the release of every installation it
// holds, or of the owner LEGION_GITHUB_OWNER names alone (a non-zero exit, as the broker's
// refusal, for an owner it holds none on), each token naming the owner and whether it is
// read-only. An owner its narrowing cannot be served for is refused as the broker refuses one
// GitHub answered 422 (refusesNarrowing). Every run appends its owner and read-only flag to
// calls.
type fakeBroker struct {
	dir     string
	command string
	calls   string
}

func newFakeBroker(t *testing.T, owners []string, expires time.Time) fakeBroker {
	t.Helper()
	dir := t.TempDir()
	calls := filepath.Join(dir, "calls")
	var cases strings.Builder
	var every []string
	for i, owner := range owners {
		entry := fmt.Sprintf(`"%s":{"installation_id":%d,"token":"ghs_%s_$mode","expires_at":"%s"}`, owner, 100+i, strings.ToLower(owner), expires.Format(time.RFC3339))
		every = append(every, entry)
		fmt.Fprintf(&cases, "  %s) printf '{\"installations\":{%%s}}' \"%s\" ;;\n", strings.ToLower(owner), strings.ReplaceAll(entry, `"`, `\"`))
	}
	script := fmt.Sprintf(`#!/bin/sh
printf '%%s|%%s\n' "$LEGION_GITHUB_OWNER" "$LEGION_GITHUB_READ_ONLY" >>%q
mode=write
[ -z "$LEGION_GITHUB_READ_ONLY" ] || mode=read
[ -z "$FAKE_BROKER_FAIL" ] || { echo "broker: refused" >&2; exit 3; }
if [ -n "$FAKE_BROKER_NONE" ]; then
  [ -n "$LEGION_GITHUB_OWNER" ] || { printf '{"installations":{}}'; exit 0; }
  echo "agent-secrets: github app has no installation on $LEGION_GITHUB_OWNER" >&2; exit 1
fi
if [ -n "$FAKE_BROKER_NARROW" ] && [ "$FAKE_BROKER_NARROW" = "$LEGION_GITHUB_OWNER" ]; then
  echo "agent-secrets: the narrowing cannot be served: github app 12345: GitHub answered HTTP 422 to the narrowed token for $LEGION_GITHUB_OWNER: the installation does not hold the permissions asked for; the caller fixes the narrowing" >&2; exit 1
fi
owner=$(printf %%s "$LEGION_GITHUB_OWNER" | tr A-Z a-z)
case "$owner" in
  "") printf '{"installations":{%%s}}' "%s" ;;
%s  *) echo "agent-secrets: github app has no installation on $LEGION_GITHUB_OWNER" >&2; exit 1 ;;
esac
`, calls, strings.ReplaceAll(strings.Join(every, ","), `"`, `\"`), cases.String())
	command := filepath.Join(dir, "broker")
	if err := os.WriteFile(command, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return fakeBroker{dir: dir, command: command, calls: calls}
}

func (b fakeBroker) runs(t *testing.T) []string {
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

// uninstall makes the fake broker hold no installation from now on, as the broker answers once
// the App is uninstalled everywhere.
func (b fakeBroker) uninstall(t *testing.T) {
	t.Helper()
	t.Setenv("FAKE_BROKER_NONE", "1")
}

// refusesNarrowing makes the fake broker refuse owner's narrowing, as it refuses one GitHub
// answered 422; an owner it is not told of is served every narrowing it is asked for.
func (b fakeBroker) refusesNarrowing(t *testing.T, owner string) {
	t.Helper()
	t.Setenv("FAKE_BROKER_NARROW", owner)
}

// fakeTokenGitHub answers what token mode asks of GitHub under an installation token: GraphQL's
// viewer, GET /apps/<slug>, GET /users/<login> and GET /installation/repositories. It refuses any
// JWT-only route (GET /app, the installation listing and exchange), since token mode signs none.
type fakeTokenGitHub struct {
	mu        sync.Mutex
	slug      string
	appID     string
	repos     []string
	asked     []string
	viewerErr int
	listErr   int
}

func (g *fakeTokenGitHub) serve(t *testing.T) *httptest.Server {
	return githubtest.Serve(t, githubtest.Spec{Handle: func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "token ghs_") {
			t.Errorf("%s %s with authorization %q, want an installation token", r.Method, r.URL.Path, auth)
		}
		g.asked = append(g.asked, r.Method+" "+r.URL.Path)
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/graphql":
			if g.viewerErr != 0 {
				w.WriteHeader(g.viewerErr)
				return
			}
			fmt.Fprintf(w, `{"data":{"viewer":{"login":"%s[bot]"}}}`, g.slug)
		case r.URL.Path == "/apps/"+g.slug:
			fmt.Fprintf(w, `{"id":%s,"slug":%q}`, g.appID, g.slug)
		case r.URL.Path == "/users/"+g.slug+"[bot]":
			fmt.Fprint(w, `{"id":271566630}`)
		case r.URL.Path == "/installation/repositories":
			if g.listErr != 0 {
				w.WriteHeader(g.listErr)
				return
			}
			var listed []string
			for _, repo := range g.repos {
				listed = append(listed, fmt.Sprintf(`{"full_name":%q}`, repo))
			}
			fmt.Fprintf(w, `{"total_count":%d,"repositories":[%s]}`, len(g.repos), strings.Join(listed, ","))
		default:
			t.Errorf("token mode asked GitHub %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}})
}

func tokenModeManager(server *httptest.Server, broker fakeBroker, now *time.Time, stderr *bytes.Buffer) *Manager {
	return New(config.GitHubApps{
		Implement: config.GitHubApp{AppID: "12345", TokenCommand: broker.command},
		Review:    config.GitHubApp{AppID: "67890", TokenCommand: broker.command + " review"},
	}, Options{BaseURL: server.URL, HTTPClient: server.Client(), Now: func() time.Time { return *now }, Stderr: stderr})
}

// A Token in token mode is the owner's entry of the command's release for that owner, matched
// without regard to case; its identity comes from the token alone (GraphQL viewer, GET /apps/<slug>
// checked against app_id, GET /users/<login>), read once; it is served from the cache until fewer
// than twenty minutes remain, then the command runs again.
func TestTokenModeMintsTheOwnersEntryAndRefreshes(t *testing.T) {
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	broker := newFakeBroker(t, []string{"Sjawhar", "acme"}, now.Add(time.Hour))
	github := &fakeTokenGitHub{slug: "legion-implementer", appID: "12345"}
	var stderr bytes.Buffer
	tokens := tokenModeManager(github.serve(t), broker, &now, &stderr)

	lease, err := tokens.Token(context.Background(), Implement, "SJAWHAR")
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if lease.Token != "ghs_sjawhar_write" || !lease.ExpiresAt.Equal(now.Add(time.Hour)) ||
		lease.Identity.Name != "legion-implementer[bot]" || lease.Identity.Email != "271566630+legion-implementer[bot]@users.noreply.github.com" {
		t.Fatalf("lease = %+v", lease)
	}
	if _, err := tokens.Token(context.Background(), Implement, "sjawhar"); err != nil {
		t.Fatalf("cached Token: %v", err)
	}
	if got := broker.runs(t); len(got) != 1 || got[0] != "SJAWHAR|" {
		t.Fatalf("command runs = %q, want one for SJAWHAR, write", got)
	}

	read, err := tokens.ReadToken(context.Background(), Implement, "acme")
	if err != nil || read.Token != "ghs_acme_read" {
		t.Fatalf("ReadToken = %+v, %v; want the read-only release's token", read, err)
	}
	if got := broker.runs(t); len(got) != 2 || got[1] != "acme|1" {
		t.Fatalf("command runs = %q, want the read token's run with LEGION_GITHUB_READ_ONLY=1", got)
	}

	now = now.Add(40*time.Minute + time.Second)
	if _, err := tokens.Token(context.Background(), Implement, "sjawhar"); err != nil {
		t.Fatalf("refreshed Token: %v", err)
	}
	if got := broker.runs(t); len(got) != 3 {
		t.Fatalf("command runs = %q, want a third run once fewer than twenty minutes remain", got)
	}
	github.mu.Lock()
	defer github.mu.Unlock()
	viewers, apps, users := 0, 0, 0
	for _, asked := range github.asked {
		switch asked {
		case "POST /graphql":
			viewers++
		case "GET /apps/legion-implementer":
			apps++
		case "GET /users/legion-implementer[bot]":
			users++
		}
	}
	// Every release's token is checked to be the configured App's; the bot user is read once.
	if viewers != 3 || apps != 3 || users != 1 {
		t.Fatalf("GitHub was asked %v, want the App checked for each of the three releases and the bot user read once", github.asked)
	}
}

// A refresh whose release carries another App's token is refused, though the App's identity was
// read before: the check runs on every release, never from the cache.
func TestTokenModeRefusesAnotherAppsTokenOnRefresh(t *testing.T) {
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	broker := newFakeBroker(t, []string{"sjawhar"}, now.Add(time.Hour))
	github := &fakeTokenGitHub{slug: "legion-implementer", appID: "12345"}
	var stderr bytes.Buffer
	tokens := tokenModeManager(github.serve(t), broker, &now, &stderr)
	if _, err := tokens.Token(context.Background(), Implement, "sjawhar"); err != nil {
		t.Fatalf("Token: %v", err)
	}
	github.mu.Lock()
	github.slug, github.appID = "someone-else", "99999"
	github.mu.Unlock()
	now = now.Add(41 * time.Minute)
	if _, err := tokens.Token(context.Background(), Implement, "sjawhar"); err == nil || !strings.Contains(err.Error(), "not of the configured app_id 12345") {
		t.Fatalf("refresh to another App's token = %v, want the app_id refusal", err)
	}
}

// An owner the command releases nothing for is ErrNotInstalled, an answer about the owner, whether
// the narrowed run fails (as the broker refuses an owner without an installation) or its release
// lacks the owner; a command that fails for every owner is its own error, never ErrNotInstalled,
// and carries its stderr.
func TestTokenModeAnOwnerWithoutAnInstallationIsNotInstalled(t *testing.T) {
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	broker := newFakeBroker(t, []string{"sjawhar"}, now.Add(time.Hour))
	github := &fakeTokenGitHub{slug: "legion-implementer", appID: "12345"}
	var stderr bytes.Buffer
	tokens := tokenModeManager(github.serve(t), broker, &now, &stderr)

	if _, err := tokens.Token(context.Background(), Implement, "stranger"); !errors.Is(err, ErrNotInstalled) {
		t.Fatalf("Token for an owner with no installation = %v, want ErrNotInstalled", err)
	}
	if !strings.Contains(stderr.String(), "no installation on stranger") {
		t.Errorf("the command's stderr did not reach the daemon's: %q", stderr.String())
	}

	t.Setenv("FAKE_BROKER_FAIL", "1")
	_, err := tokens.Token(context.Background(), Implement, "sjawhar")
	if err == nil || errors.Is(err, ErrNotInstalled) || !strings.Contains(err.Error(), "github_apps.implement.token_command failed (exit 3): broker: refused") {
		t.Fatalf("Token with a failing command = %v, want the command's failure with its stderr", err)
	}
}

// What a command prints is never quoted in an error: output that is no release, an entry without
// a token or with an expired one, all fail naming the field and the account alone.
func TestTokenModeRefusesAMalformedReleaseWithoutQuotingIt(t *testing.T) {
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct{ name, output, want string }{
		{"not JSON", `ghs_secret_value`, "printed no release"},
		{"no installations", `{"token":"ghs_secret_value"}`, "printed no release"},
		{"no token", `{"installations":{"acme":{"installation_id":1,"expires_at":"2026-11-03T13:00:00Z"}}}`, "released acme's installation without a token"},
		{"expired", `{"installations":{"acme":{"installation_id":1,"token":"ghs_secret_value","expires_at":"2026-11-03T11:00:00Z"}}}`, "released acme's installation with a token that expired"},
		{"no installation id", `{"installations":{"acme":{"token":"ghs_secret_value","expires_at":"2026-11-03T13:00:00Z"}}}`, "released acme's installation with no installation id"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseRelease([]byte(tc.output), "github_apps.implement.token_command", now)
			if err == nil || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), "ghs_secret_value") {
				t.Fatalf("parseRelease = %v, want %q and no token", err, tc.want)
			}
		})
	}
}

// No token reaches a log or an error: a token on the command's stderr is redacted in both, a
// release keyed by something other than a GitHub login is refused without naming the key, and
// output past releaseLimit ends the command rather than filling memory.
func TestTokenModeKeepsTokensOutOfLogsAndErrors(t *testing.T) {
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	var stderr bytes.Buffer
	leaky := New(config.GitHubApps{Implement: config.GitHubApp{AppID: "1", TokenCommand: `echo "denied for ghs_abcdefghijklmnopqrstuvwxyz0123" >&2; exit 3`}},
		Options{Now: func() time.Time { return now }, Stderr: &stderr})
	_, err := leaky.runTokenCommand(context.Background(), leaky.apps.Implement, mintRequest{role: Implement, owner: "acme"})
	if err == nil || strings.Contains(err.Error(), "ghs_abc") || !strings.Contains(err.Error(), "[redacted]") {
		t.Fatalf("error = %v, want the stderr with its token redacted", err)
	}
	if strings.Contains(stderr.String(), "ghs_abc") || !strings.Contains(stderr.String(), "denied for [redacted]") {
		t.Fatalf("daemon stderr = %q, want the line with its token redacted", stderr.String())
	}

	_, err = parseRelease([]byte(`{"installations":{"ghs_secret_value":{"installation_id":1,"token":"x","expires_at":"2026-11-03T13:00:00Z"}}}`), "f", now)
	if err == nil || strings.Contains(err.Error(), "ghs_secret_value") {
		t.Fatalf("parseRelease of a token-shaped key = %v, want a refusal that does not name it", err)
	}

	huge := New(config.GitHubApps{Implement: config.GitHubApp{AppID: "1", TokenCommand: "yes ghs_secret_value_padding"}}, Options{Now: func() time.Time { return now }})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err = huge.runTokenCommand(ctx, huge.apps.Implement, mintRequest{role: Implement, owner: "acme"})
	if err == nil || !strings.Contains(err.Error(), "printed more than") || ctx.Err() != nil {
		t.Fatalf("a command printing without end = %v (ctx %v), want the over-limit refusal before the deadline", err, ctx.Err())
	}
}

// A ReadToken for an owner the broker refuses asks nothing wider: the one narrowed run decides it,
// so no write token of any installation is released for a read-only request.
func TestTokenModeReadTokenForAMissingOwnerReleasesNothingWider(t *testing.T) {
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	broker := newFakeBroker(t, []string{"sjawhar"}, now.Add(time.Hour))
	github := &fakeTokenGitHub{slug: "legion-implementer", appID: "12345"}
	var stderr bytes.Buffer
	tokens := tokenModeManager(github.serve(t), broker, &now, &stderr)
	if _, err := tokens.ReadToken(context.Background(), Implement, "stranger"); !errors.Is(err, ErrNotInstalled) {
		t.Fatalf("ReadToken for an owner with no installation = %v, want ErrNotInstalled", err)
	}
	if got := broker.runs(t); len(got) != 1 || got[0] != "stranger|1" {
		t.Fatalf("command runs = %q, want the one narrowed read-only run", got)
	}
}

// A repository the installation does not list is not installed, and the installation's own list
// is the whole of that answer now: a release names no repository (INFRA-2543), so the broker
// refuses none and InstalledOn reads GET /installation/repositories under the owner's read
// lease. The lease is cached between the two reads, so the command runs once.
func TestTokenModeARepositoryOutsideTheInstallationsListIsNotInstalled(t *testing.T) {
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	broker := newFakeBroker(t, []string{"acme"}, now.Add(time.Hour))
	github := &fakeTokenGitHub{slug: "legion-implementer", appID: "12345", repos: []string{"acme/widgets"}}
	var stderr bytes.Buffer
	tokens := tokenModeManager(github.serve(t), broker, &now, &stderr)

	if got, err := tokens.InstalledOn(context.Background(), Implement, ghrepo.MustParse("acme/widgets")); err != nil || !got {
		t.Fatalf("InstalledOn(acme/widgets) = %v, %v; want installed", got, err)
	}
	got, err := tokens.InstalledOn(context.Background(), Implement, ghrepo.MustParse("acme/gadgets"))
	if err != nil || got {
		t.Fatalf("InstalledOn(acme/gadgets) = %v, %v; want not installed and no error", got, err)
	}
	want := []string{"acme|1"}
	if runs := broker.runs(t); !slices.Equal(runs, want) {
		t.Fatalf("command runs = %q, want %q: the owner's one read lease answers both reads", runs, want)
	}
}

// A narrowing the broker refuses is ErrNarrowingRefused, the refusal key mode gives, and the
// mapping's installation read is the one source of its own answer: it runs the command and reads
// GitHub however recently a narrowing was refused for that owner's read scope (listInstallation,
// ForgetNarrowingRefusal). Served from the Manager's memory instead, an operator who fixes what
// the broker refused and saves the mapping inside the five minutes a refusal is remembered for
// would be told the check failed, while the bundle refreshes behind it kept that memory alive
// (INFRA-513).
func TestTheInstallationReadAsksPastARememberedNarrowingRefusal(t *testing.T) {
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	broker := newFakeBroker(t, []string{"acme"}, now.Add(time.Hour))
	broker.refusesNarrowing(t, "acme")
	github := &fakeTokenGitHub{slug: "legion-implementer", appID: "12345", repos: []string{"acme/widgets"}}
	var stderr bytes.Buffer
	tokens := tokenModeManager(github.serve(t), broker, &now, &stderr)

	// A bundle's own mint for the owner whose narrowing the broker cannot serve: the Manager
	// remembers that scope's refusal.
	if _, err := tokens.ReadToken(context.Background(), Implement, "acme"); !errors.Is(err, ErrNarrowingRefused) {
		t.Fatalf("the read lease the broker refuses the narrowing of = %v, want ErrNarrowingRefused", err)
	}

	// The operator fixes what the broker refused and saves the mapping a minute later, well
	// inside the window the refusal is remembered for.
	t.Setenv("FAKE_BROKER_NARROW", "")
	now = now.Add(time.Minute)

	if got, err := tokens.InstalledOn(context.Background(), Implement, ghrepo.MustParse("acme/widgets")); err != nil || !got {
		t.Fatalf("InstalledOn(acme/widgets) once the broker serves the narrowing = %v, %v; want installed: the read asks the command, never the Manager's memory of the refusal", got, err)
	}
	want := []string{"acme|1", "acme|1"}
	if runs := broker.runs(t); !slices.Equal(runs, want) {
		t.Errorf("command runs = %q, want %q: the bundle's own mint, and the command run again for the installation read", runs, want)
	}
}

// `agent-secrets` exits 75 while its request still waits for approval: transient, never the
// configuration's answer.
func TestTokenModeAPendingApprovalIsTransient(t *testing.T) {
	tokens := New(config.GitHubApps{Implement: config.GitHubApp{AppID: "1", TokenCommand: "echo 'still waiting for approval' >&2; exit 75"}}, Options{})
	var transient *TransientError
	if _, err := tokens.Token(context.Background(), Implement, "acme"); !errors.As(err, &transient) || errors.Is(err, ErrNotInstalled) {
		t.Fatalf("Token on exit 75 = %v, want a transient error", err)
	}
}

// A command still running when its caller's deadline ends goes on: the caller gets
// ErrCommandPending, the next caller joins the same run, and the command runs once.
func TestTokenModeACallerDeadlineNeitherKillsNorRepeatsTheCommand(t *testing.T) {
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	runs, gate := filepath.Join(dir, "runs"), filepath.Join(dir, "gate")
	command := fmt.Sprintf(`echo run >>%q; while [ ! -e %q ]; do sleep 0.05; done; printf '{"installations":{"acme":{"installation_id":1,"token":"ghs_x","expires_at":"%s"}}}'`,
		runs, gate, now.Add(time.Hour).Format(time.RFC3339))
	tokens := New(config.GitHubApps{Implement: config.GitHubApp{AppID: "1", TokenCommand: command}}, Options{Now: func() time.Time { return now }})
	short, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	var transient *TransientError
	if _, err := tokens.runTokenCommand(short, tokens.apps.Implement, mintRequest{role: Implement, owner: "acme"}); !errors.Is(err, ErrCommandPending) || !errors.As(err, &transient) {
		t.Fatalf("a caller past its deadline = %v, want a transient ErrCommandPending", err)
	}
	if err := os.WriteFile(gate, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := tokens.runTokenCommand(context.Background(), tokens.apps.Implement, mintRequest{role: Implement, owner: "acme"})
	if err != nil || got["acme"].token != "ghs_x" {
		t.Fatalf("the next caller = %v, %v; want the running command's release", got, err)
	}
	if data, _ := os.ReadFile(runs); strings.Count(string(data), "run") != 1 {
		t.Fatalf("the command ran %d times, want once", strings.Count(string(data), "run"))
	}
}

// A caller whose deadline ended before the command finished, and who comes back only after it
// finished, gets the finished run's release; the command does not run again.
func TestTokenModeACallerAfterTheRunFinishedGetsItsResult(t *testing.T) {
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	runs := filepath.Join(t.TempDir(), "runs")
	command := fmt.Sprintf(`echo run >>%q; sleep 0.5; printf '{"installations":{"acme":{"installation_id":1,"token":"ghs_x","expires_at":"%s"}}}'`,
		runs, now.Add(time.Hour).Format(time.RFC3339))
	tokens := New(config.GitHubApps{Implement: config.GitHubApp{AppID: "1", TokenCommand: command}}, Options{Now: func() time.Time { return now }})
	short, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := tokens.runTokenCommand(short, tokens.apps.Implement, mintRequest{role: Implement, owner: "acme"}); !errors.Is(err, ErrCommandPending) {
		t.Fatalf("a caller past its deadline = %v, want ErrCommandPending", err)
	}
	time.Sleep(800 * time.Millisecond)
	later, cancelLater := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancelLater()
	got, err := tokens.runTokenCommand(later, tokens.apps.Implement, mintRequest{role: Implement, owner: "acme"})
	if err != nil || got["acme"].token != "ghs_x" {
		t.Fatalf("the caller after the run finished = %v, %v; want the finished run's release", got, err)
	}
	if data, _ := os.ReadFile(runs); strings.Count(string(data), "run") != 1 {
		t.Fatalf("the command ran %d times, want once", strings.Count(string(data), "run"))
	}
}

// A lease GitHub refuses with 401 (the App uninstalled while its token lived) is dropped and
// minted once more, so InstalledOn answers not installed rather than an error.
func TestTokenModeInstalledOnRemintsAfterA401(t *testing.T) {
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	broker := newFakeBroker(t, []string{"acme"}, now.Add(time.Hour))
	github := &fakeTokenGitHub{slug: "legion-implementer", appID: "12345", repos: []string{"acme/widgets"}}
	server := github.serve(t)
	var stderr bytes.Buffer
	tokens := tokenModeManager(server, broker, &now, &stderr)
	if got, err := tokens.InstalledOn(context.Background(), Implement, ghrepo.MustParse("acme/widgets")); err != nil || !got {
		t.Fatalf("InstalledOn = %v, %v; want installed", got, err)
	}
	github.mu.Lock()
	github.listErr = http.StatusUnauthorized
	github.mu.Unlock()
	t.Setenv("FAKE_BROKER_FAIL", "")
	broker.uninstall(t)
	got, err := tokens.InstalledOn(context.Background(), Implement, ghrepo.MustParse("acme/widgets"))
	if err != nil || got {
		t.Fatalf("InstalledOn after an uninstall = %v, %v; want not installed", got, err)
	}
}

// AppLogin in token mode reads the bot login under a token of any installation (the command run
// with no owner), and refuses a token whose App is not the configured app_id; GitHub's trouble on
// the viewer read is transient.
func TestTokenModeAppLoginReadsTheViewerAndChecksTheAppID(t *testing.T) {
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	broker := newFakeBroker(t, []string{"sjawhar"}, now.Add(time.Hour))
	github := &fakeTokenGitHub{slug: "legion-reviewer", appID: "67890"}
	var stderr bytes.Buffer
	tokens := tokenModeManager(github.serve(t), broker, &now, &stderr)

	login, err := tokens.AppLogin(context.Background(), Review)
	if err != nil || login != "legion-reviewer[bot]" {
		t.Fatalf("AppLogin = %q, %v; want legion-reviewer[bot]", login, err)
	}
	if got := broker.runs(t); len(got) != 1 || got[0] != "|" {
		t.Fatalf("command runs = %q, want one naming no owner at all", got)
	}

	other := tokenModeManager(github.serve(t), broker, &now, &stderr)
	_, err = other.AppLogin(context.Background(), Implement)
	if err == nil || !strings.Contains(err.Error(), "released a token of App 67890 (legion-reviewer), not of the configured app_id 12345") {
		t.Fatalf("AppLogin of a token of another App = %v, want the app_id refusal", err)
	}

	github.mu.Lock()
	github.viewerErr = http.StatusBadGateway
	github.mu.Unlock()
	fresh := tokenModeManager(github.serve(t), broker, &now, &stderr)
	var transient *TransientError
	if _, err := fresh.AppLogin(context.Background(), Review); !errors.As(err, &transient) {
		t.Fatalf("AppLogin on GitHub's 502 = %v, want a transient error", err)
	}
}

// InstalledOn in token mode reads the owner's installation's repositories: a repository it lists
// is installed, one it does not list, or an owner with no installation, is not; GitHub's trouble is
// transient. An installation token reads any public repository, so GET /repos/{owner}/{repo} could
// not say.
func TestTokenModeInstalledOnReadsTheInstallationsRepositories(t *testing.T) {
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	broker := newFakeBroker(t, []string{"acme"}, now.Add(time.Hour))
	github := &fakeTokenGitHub{slug: "legion-implementer", appID: "12345", repos: []string{"acme/Widgets"}}
	var stderr bytes.Buffer
	tokens := tokenModeManager(github.serve(t), broker, &now, &stderr)

	for _, tc := range []struct {
		repo string
		want bool
	}{{"acme/widgets", true}, {"acme/gadgets", false}, {"stranger/widgets", false}} {
		got, err := tokens.InstalledOn(context.Background(), Implement, ghrepo.MustParse(tc.repo))
		if err != nil || got != tc.want {
			t.Errorf("InstalledOn(%s) = %v, %v; want %v", tc.repo, got, err, tc.want)
		}
	}

	t.Setenv("FAKE_BROKER_FAIL", "1")
	fresh := tokenModeManager(github.serve(t), broker, &now, &stderr)
	if _, err := fresh.InstalledOn(context.Background(), Implement, ghrepo.MustParse("acme/widgets")); err == nil || !strings.Contains(err.Error(), "acme/widgets") {
		t.Fatalf("InstalledOn with a failing command = %v, want an error naming the repository", err)
	}
}

// The command sees the daemon's environment less the NATS seeds, with the two variables the
// daemon sets replacing any it inherited. The daemon names an owner to the command and no
// repository at all, since the owner's installation is the whole of a token's reach
// (INFRA-2543). A command cut off by its context is transient.
func TestTokenModeCommandEnvironmentAndDeadline(t *testing.T) {
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	seen := filepath.Join(dir, "env")
	t.Setenv("NATS_NKEY_SEED", "seed-value")
	t.Setenv(TokenCommandOwner, "inherited")
	t.Setenv(TokenCommandReadOnly, "inherited")
	command := fmt.Sprintf(`env >%q; printf '{"installations":{"acme":{"installation_id":1,"token":"ghs_x","expires_at":"%s"}}}'`, seen, now.Add(time.Hour).Format(time.RFC3339))
	tokens := New(config.GitHubApps{Implement: config.GitHubApp{AppID: "1", TokenCommand: command}}, Options{Now: func() time.Time { return now }})
	if _, err := tokens.runTokenCommand(context.Background(), tokens.apps.Implement, mintRequest{role: Implement, owner: "acme"}); err != nil {
		t.Fatal(err)
	}
	env, _ := os.ReadFile(seen)
	if strings.Contains(string(env), "seed-value") || !strings.Contains(string(env), "\nLEGION_GITHUB_OWNER=acme\n") ||
		!strings.Contains(string(env), "\nLEGION_GITHUB_READ_ONLY=\n") || strings.Contains(string(env), "inherited") {
		t.Fatalf("the command's environment:\n%s", env)
	}

	slow := New(config.GitHubApps{Implement: config.GitHubApp{AppID: "1", TokenCommand: "sleep 30"}}, Options{})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	var transient *TransientError
	if _, err := slow.runTokenCommand(ctx, slow.apps.Implement, mintRequest{role: Implement, owner: "acme"}); !errors.As(err, &transient) {
		t.Fatalf("a command past its deadline = %v, want a transient error", err)
	}
}

// A caller waiting on another's mint, which ended only because that caller's deadline did, joins
// the command still running under its own context and gets its lease.
func TestTokenModeAWaiterOutlivesTheInitiatorsDeadline(t *testing.T) {
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	gate := filepath.Join(dir, "gate")
	command := fmt.Sprintf(`while [ ! -e %q ]; do sleep 0.05; done; printf '{"installations":{"acme":{"installation_id":1,"token":"ghs_waited","expires_at":"%s"}}}'`,
		gate, now.Add(time.Hour).Format(time.RFC3339))
	github := &fakeTokenGitHub{slug: "legion-implementer", appID: "12345"}
	server := github.serve(t)
	tokens := New(config.GitHubApps{Implement: config.GitHubApp{AppID: "12345", TokenCommand: command}},
		Options{BaseURL: server.URL, HTTPClient: server.Client(), Now: func() time.Time { return now }})
	short, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	initiator := make(chan error, 1)
	go func() {
		_, err := tokens.Token(short, Implement, "acme")
		initiator <- err
	}()
	time.Sleep(100 * time.Millisecond)
	waiter := make(chan Lease, 1)
	go func() {
		lease, err := tokens.Token(context.Background(), Implement, "acme")
		if err != nil {
			t.Errorf("the waiter = %v, want the lease", err)
		}
		waiter <- lease
	}()
	if err := <-initiator; !errors.Is(err, ErrCommandPending) {
		t.Fatalf("the initiator = %v, want ErrCommandPending", err)
	}
	if err := os.WriteFile(gate, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case lease := <-waiter:
		if lease.Token != "ghs_waited" {
			t.Fatalf("the waiter's lease = %+v, want the running command's token", lease)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the waiter never got the lease")
	}
}

// Close ends a command still running and waits for it; a later mint is refused.
func TestTokenModeCloseEndsARunningCommand(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "pid")
	tokens := New(config.GitHubApps{Implement: config.GitHubApp{AppID: "1", TokenCommand: fmt.Sprintf(`echo $$ >%q; sleep 600`, pidFile)}}, Options{})
	short, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if _, err := tokens.Token(short, Implement, "acme"); !errors.Is(err, ErrCommandPending) {
		t.Fatalf("Token = %v, want ErrCommandPending", err)
	}
	closed := make(chan struct{})
	go func() { tokens.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(10 * time.Second):
		t.Fatal("Close did not end the running command")
	}
	if _, err := tokens.Token(context.Background(), Implement, "acme"); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("Token after Close = %v, want a refusal", err)
	}
}

// The command is told the owner whose installation token the daemon wants and whether it wants
// a read-only one, and nothing else: an operator's command maps those onto --github-owner and
// --github-read-only. It is told no repository, since the token reaches every repository the
// installation holds (INFRA-2543).
func TestTokenModeTellsTheCommandTheOwnerAndTheReadOnlyFlag(t *testing.T) {
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	broker := newFakeBroker(t, []string{"acme"}, now.Add(time.Hour))
	github := &fakeTokenGitHub{slug: "legion-implementer", appID: "12345"}
	var stderr bytes.Buffer
	tokens := tokenModeManager(github.serve(t), broker, &now, &stderr)

	if _, err := tokens.Token(context.Background(), Implement, "acme"); err != nil {
		t.Fatalf("Token: %v", err)
	}
	if _, err := tokens.ReadToken(context.Background(), Implement, "acme"); err != nil {
		t.Fatalf("ReadToken: %v", err)
	}
	want := []string{"acme|", "acme|1"}
	if got := broker.runs(t); !slices.Equal(got, want) {
		t.Fatalf("command runs = %q, want %q", got, want)
	}
}

// The read scope and the write scope of one owner are two runs: whether the daemon wants a
// read-only release is part of the key a run is shared under, so a caller that asked for a
// reading token never gets the run that releases a writing one, while a caller asking for the
// same scope joins the run already going.
func TestTokenModeTheReadAndWriteScopesOfOneOwnerDoNotShareOneRun(t *testing.T) {
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	runs, gate := filepath.Join(dir, "runs"), filepath.Join(dir, "gate")
	command := fmt.Sprintf(`echo run >>%q; while [ ! -e %q ]; do sleep 0.05; done; printf '{"installations":{"acme":{"installation_id":1,"token":"ghs_x","expires_at":"%s"}}}'`,
		runs, gate, now.Add(time.Hour).Format(time.RFC3339))
	tokens := New(config.GitHubApps{Implement: config.GitHubApp{AppID: "1", TokenCommand: command}}, Options{Now: func() time.Time { return now }})
	defer tokens.Close()
	for _, read := range []bool{true, false, true} {
		short, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		_, err := tokens.runTokenCommand(short, tokens.apps.Implement, mintRequest{role: Implement, owner: "acme", read: read})
		cancel()
		if !errors.Is(err, ErrCommandPending) {
			t.Fatalf("the run for read=%v = %v, want ErrCommandPending while it goes on", read, err)
		}
	}
	if err := os.WriteFile(gate, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	tokens.Close()
	data, _ := os.ReadFile(runs)
	if ran := strings.Count(string(data), "run"); ran != 2 {
		t.Fatalf("the command ran %d times, want one run for each of the two scopes", ran)
	}
}

// A read-only release never stands in for a write request of the same owner: the two are
// separate scopes, so the command runs again and the write lease is the write release's.
func TestTokenModeAReadOnlyReleaseDoesNotStandInForAWriteRequest(t *testing.T) {
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	broker := newFakeBroker(t, []string{"acme"}, now.Add(time.Hour))
	github := &fakeTokenGitHub{slug: "legion-implementer", appID: "12345"}
	var stderr bytes.Buffer
	tokens := tokenModeManager(github.serve(t), broker, &now, &stderr)

	read, err := tokens.ReadToken(context.Background(), Implement, "acme")
	if err != nil || read.Token != "ghs_acme_read" {
		t.Fatalf("ReadToken = %+v, %v; want the read-only release's token", read, err)
	}
	write, err := tokens.Token(context.Background(), Implement, "acme")
	if err != nil || write.Token != "ghs_acme_write" {
		t.Fatalf("Token after a read-only release = %+v, %v; want the write release's token", write, err)
	}
	want := []string{"acme|1", "acme|"}
	if got := broker.runs(t); !slices.Equal(got, want) {
		t.Fatalf("command runs = %q, want %q", got, want)
	}
}
