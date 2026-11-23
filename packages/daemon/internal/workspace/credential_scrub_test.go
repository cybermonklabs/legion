package workspace

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
)

// echoesTheToken answers a command whose argv holds word as a git does that echoed its credentialed
// URL and then failed: stderr naming the token, with exit 128, or with exit -1 and end, the error
// of a signal's death. Every other command succeeds, `jj config get` with `false`.
type echoesTheToken struct {
	word, token string
	end         error
}

func (r echoesTheToken) Timeout() time.Duration { return time.Minute }

func (r echoesTheToken) Run(_ context.Context, command Command) (Result, error) {
	switch {
	case slices.Contains(command.Argv, r.word):
		said := "fatal: unable to access 'https://x-access-token:" + r.token + "@github.com/acme/widgets/': The requested URL returned error: 502"
		if r.end != nil {
			return Result{ExitCode: -1, Stderr: said}, r.end
		}
		return Result{ExitCode: 128, Stderr: said}, nil
	case slices.Contains(command.Argv, "get"):
		return Result{Stdout: "false\n"}, nil
	}
	return Result{}, nil
}

// What a run under the one-shot credential returns carries no token, whether the run exited or a
// signal ended it: the shared clone's fetch (ensureFetchConfiguration) and its clone from GitHub
// (ensureRepoClone's `jj git clone`) scrub their stderr, as Fetch's transfers do, before an error
// is made from it, through commandFailure for an exit and through runFailure for a signal's death.
func TestARunUnderTheProvisioningCredentialReturnsItsTokenScrubbed(t *testing.T) {
	const token = "ghs_provisioningTokenThatMustNotLeak"
	killed := exec.Command("sh", "-c", "kill -KILL $$").Run()
	for _, ending := range []struct {
		name string
		end  error
	}{{"exited 128", nil}, {"killed", killed}} {
		t.Run("the shared clone's fetch, "+ending.name, func(t *testing.T) {
			source, err := newProvisioningCredential(t.TempDir(), token)
			if err != nil {
				t.Fatal(err)
			}
			err = ensureFetchConfiguration(context.Background(), echoesTheToken{word: "fetch", token: token, end: ending.end}, t.TempDir(), source)
			if err == nil || strings.Contains(err.Error(), token) || !strings.Contains(err.Error(), "unable to access 'https://github.com/acme/widgets/'") {
				t.Errorf("ensureFetchConfiguration = %v, want the failure with its token scrubbed", err)
			}
		})
		t.Run("the clone from GitHub, "+ending.name, func(t *testing.T) {
			source, err := newProvisioningCredential(t.TempDir(), token)
			if err != nil {
				t.Fatal(err)
			}
			cloneDir := filepath.Join(t.TempDir(), "repos", "acme", "widgets")
			err = ensureRepoClone(context.Background(), echoesTheToken{word: "clone", token: token, end: ending.end}, WallClock{}, cloneDir, "https://github.com/acme/widgets", "main", source)
			if err == nil || strings.Contains(err.Error(), token) || !strings.Contains(err.Error(), "unable to access 'https://github.com/acme/widgets/'") {
				t.Errorf("ensureRepoClone = %v, want the failure with its token scrubbed", err)
			}
		})
	}
}

// redirectRefusal is git's refusal of a redirect it will not follow, as git 2.43 printed it against a
// stand-in GitHub whose first answer (info/refs) redirected to a Location ending in another path:
// git names the Location it was sent to, query and all.
func redirectRefusal(location string) string {
	return "fatal: unable to update url base from redirection:\n  asked for: https://github.com/acme/widgets/info/refs?service=git-upload-pack\n   redirect: " + location
}

// answersWith answers a command whose argv holds word with stderr and exit 128, as git does that
// refused a transfer. Every other command succeeds, `jj config get` with `false`.
type answersWith struct{ word, stderr string }

func (r answersWith) Timeout() time.Duration { return time.Minute }

func (r answersWith) Run(_ context.Context, command Command) (Result, error) {
	switch {
	case slices.Contains(command.Argv, r.word):
		return Result{ExitCode: 128, Stderr: r.stderr}, nil
	case slices.Contains(command.Argv, "get"):
		return Result{Stdout: "false\n"}, nil
	}
	return Result{}, nil
}

// A transfer from GitHub that GitHub, or whatever answers in its place, redirects to a Location
// carrying user information or a query value prints none of them: git names the Location in its
// refusal (git 2.43 keeps the query; the userinfo case is the same line with user information in
// the Location), and every run under the one-shot credential gives its stderr through the URL rule
// with the token scrub, so the error Fetch returns, and the shared clone's fetch's and clone's,
// hold neither. The controls: each error still names git's refusal and the Location's host and path.
// Captures workspace.git-redirect/<run>-<case>.
func TestARunFromGitHubPrintsNoRedirectTargetsCredentials(t *testing.T) {
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("find git: %v", err)
	}
	for _, location := range []struct{ name, url string }{
		{"query", "https://github.example/moved/widgets.git/info/refs?service=git-upload-pack&access_token=not-a-secret-token"},
		{"userinfo", "https://not-a-secret-user:not-a-secret-password@github.example/moved/widgets.git/info/refs"},
	} {
		said := redirectRefusal(location.url)
		check := func(t *testing.T, run string, err error) {
			t.Helper()
			text := ""
			if err != nil {
				text = err.Error()
			}
			oauth2calltest.RequireNone(t, "workspace.git-redirect/"+run+"-"+location.name, text)
			if !strings.Contains(text, "unable to update url base from redirection") || !strings.Contains(text, "redirect: https://github.example/moved/widgets.git/info/refs") {
				t.Errorf("%s = %v, want git's refusal naming the Location's host and path", run, err)
			}
		}
		t.Run("the fetch's transfer, "+location.name, func(t *testing.T) {
			fakeGit := filepath.Join(t.TempDir(), "git")
			script := "#!/bin/sh\nif [ -n \"${" + provisioningTokenFileEnv + ":-}\" ]; then\n" +
				"  printf '%s\\n' '" + said + "' >&2\n  exit 128\nfi\nexec '" + realGit + "' \"$@\"\n"
			if err := os.WriteFile(fakeGit, []byte(script), 0o700); err != nil {
				t.Fatalf("write the stand-in git: %v", err)
			}
			_, err := Fetch(context.Background(), NewRunner(time.Minute, map[string]string{"git": fakeGit}), fetchRequest(t))
			check(t, "fetch", err)
		})
		t.Run("the shared clone's fetch, "+location.name, func(t *testing.T) {
			source, err := newProvisioningCredential(t.TempDir(), testInstallationToken)
			if err != nil {
				t.Fatal(err)
			}
			check(t, "clone-fetch", ensureFetchConfiguration(context.Background(), answersWith{word: "fetch", stderr: said}, t.TempDir(), source))
		})
		t.Run("the clone from GitHub, "+location.name, func(t *testing.T) {
			source, err := newProvisioningCredential(t.TempDir(), testInstallationToken)
			if err != nil {
				t.Fatal(err)
			}
			cloneDir := filepath.Join(t.TempDir(), "repos", "acme", "widgets")
			check(t, "clone", ensureRepoClone(context.Background(), answersWith{word: "clone", stderr: said}, WallClock{}, cloneDir, "https://github.com/acme/widgets", "main", source))
		})
	}
}
