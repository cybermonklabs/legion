package workspace

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
	"github.com/sjawhar/legion/daemon/internal/clock"
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
			err = ensureRepoClone(context.Background(), echoesTheToken{word: "clone", token: token, end: ending.end}, clock.Real(), cloneDir, "https://github.com/acme/widgets", "main", source)
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

// echoedToken is the provisioning token of the cases whose stand-in GitHub writes the token back in
// another form than git sent it. It holds oauth2calltest.Token, the made-up credential the check's
// markers name, so what a scrub leaves of it shows in the output a case captures.
const echoedToken = "ghs_" + oauth2calltest.Token + "-7c41e09b"

// percentEncoded is s with each byte whose index encode selects written as a %XX escape, in upper-
// or lower-case hexadecimal as upper says: a Location may encode any of a token's bytes, in either
// case.
func percentEncoded(s string, encode func(int) bool, upper bool) string {
	format := "%%%02x"
	if upper {
		format = "%%%02X"
	}
	var out strings.Builder
	for i := range len(s) {
		if encode(i) {
			fmt.Fprintf(&out, format, s[i])
		} else {
			out.WriteByte(s[i])
		}
	}
	return out.String()
}

// percentEscape is a %XX escape, in either case.
var percentEscape = regexp.MustCompile(`%[0-9A-Fa-f]{2}`)

// unescaped is s with each %XX escape read as the byte it names, as a reader of a URL reads it.
func unescaped(s string) string {
	return percentEscape.ReplaceAllStringFunc(s, func(e string) string {
		b, _ := strconv.ParseUint(e[1:], 16, 8)
		return string([]byte{byte(b)})
	})
}

// requireNoEcho is oauth2calltest.RequireNoPiece of text under capture for token and the Basic
// credential git sends it in (the one-shot credential's helper answers git with the user
// x-access-token), and the same of text percent-decoded (unescaped) under capture/decoded.
func requireNoEcho(t *testing.T, capture, text, token string) {
	t.Helper()
	credentials := []string{token, base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))}
	oauth2calltest.RequireNoPiece(t, capture, text, credentials...)
	oauth2calltest.RequireNoPiece(t, capture+"/decoded", unescaped(text), credentials...)
}

// A transfer from GitHub that GitHub, or whatever answers in its place, refuses with words of its
// own prints no credential of them: git names a redirect's Location in its refusal (git 2.43 keeps
// the query; the userinfo case is the same line with user information in the Location), and
// prints an ERR packet's or a sideband line's words as they came. A Location can carry user
// information or a query value, or echo the token less a byte or percent-encoded, all of it or
// every other byte, and a line can echo the token less a byte, percent-encoded every other byte
// (outside any URL, where the fail-closed rule reads a '%' as ordinary text), or the Basic
// credential git sent it in; a Location whose user information is percent-encoded is one the URL
// rule cannot read. Every run under the one-shot credential scrubs its stderr of the token
// (scrubTokens) and gives it through the URL rule, and what it prints of the stderr through the
// fail-closed rule too, so the error Fetch returns, the line its retry logs, and the shared
// clone's fetch's and clone's errors hold none of them, nor a run of the token or of the Basic
// credential, as written or percent-decoded. The controls: each error and line still names git's
// refusal and the Location's host and path, or the words around the echo, with the token's place
// redacted, and the one the rule cannot read prints as the rule prints that Location, its words
// not printed. The cases whose stderr holds the probe's user and password run under
// testInstallationToken, which shares no run with them. Captures
// workspace.git-redirect/<run>-<case>.
func TestARunFromGitHubPrintsNoRedirectTargetsCredentials(t *testing.T) {
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("find git: %v", err)
	}
	every := func(int) bool { return true }
	everyOther := func(i int) bool { return i%2 == 0 }
	moved := func(path string) string { return "https://github.example/moved/" + path + "/info/refs" }
	refused := func(location string) []string {
		return []string{"unable to update url base from redirection", "redirect: " + location}
	}
	basic := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + echoedToken))
	const encodedUserinfo = "https://not-a-secret-user%3Anot-a-secret-password%40github.example/moved/widgets.git/info/refs"
	if shown := oauth2calltest.Said(encodedUserinfo).String(); strings.Contains(shown, "github.example") {
		t.Fatalf("the rule reads %q as %q, want its words not printed: the case it names proves nothing", encodedUserinfo, shown)
	}
	for _, c := range []struct {
		name, token, stderr string
		shows               []string
	}{
		{"query", testInstallationToken, redirectRefusal("https://github.example/moved/widgets.git/info/refs?service=git-upload-pack&access_token=not-a-secret-token"), refused(moved("widgets.git"))},
		{"userinfo", testInstallationToken, redirectRefusal("https://not-a-secret-user:not-a-secret-password@github.example/moved/widgets.git/info/refs"), refused(moved("widgets.git"))},
		{"run", echoedToken, redirectRefusal(moved(echoedToken[1:])), refused(moved("[redacted]"))},
		{"encoded", echoedToken, redirectRefusal(moved(percentEncoded(echoedToken, every, true))), refused(moved("***"))},
		{"encoded-in-part", echoedToken, redirectRefusal(moved(percentEncoded(echoedToken, everyOther, false))), refused(moved("***"))},
		{"basic", echoedToken, "fatal: remote error: refused Authorization: Basic " + basic, []string{"fatal: remote error: refused Authorization: Basic [redacted]"}},
		{"line-run", echoedToken, "remote: refused " + echoedToken[1:], []string{"remote: refused [redacted]"}},
		{"line-encoded", echoedToken, "remote: refused " + percentEncoded(echoedToken, everyOther, false), []string{"remote: refused ***"}},
		{"userinfo-encoded", testInstallationToken, redirectRefusal(encodedUserinfo), []string{oauth2calltest.Said(encodedUserinfo).String()}},
	} {
		check := func(t *testing.T, run, text string) {
			t.Helper()
			requireNoEcho(t, "workspace.git-redirect/"+run+"-"+c.name, text, c.token)
			for _, shown := range c.shows {
				if !strings.Contains(text, shown) {
					t.Errorf("%s printed %q, want %q in it", run, text, shown)
				}
			}
		}
		errorText := func(err error) string {
			if err == nil {
				return ""
			}
			return err.Error()
		}
		t.Run("the fetch's transfer, "+c.name, func(t *testing.T) {
			fakeGit := filepath.Join(t.TempDir(), "git")
			script := "#!/bin/sh\nif [ -n \"${" + provisioningTokenFileEnv + ":-}\" ]; then\n" +
				"  printf '%s\\n' '" + c.stderr + "' >&2\n  exit 128\nfi\nexec '" + realGit + "' \"$@\"\n"
			if err := os.WriteFile(fakeGit, []byte(script), 0o700); err != nil {
				t.Fatalf("write the stand-in git: %v", err)
			}
			request := fetchRequest(t)
			request.Token = staticToken(c.token)
			_, err := Fetch(context.Background(), NewRunner(time.Minute, map[string]string{"git": fakeGit}), request)
			check(t, "fetch", errorText(err))
		})
		t.Run("the fetch's retried transfer, "+c.name, func(t *testing.T) {
			clock := newTestClock()
			request := fetchRequest(t)
			request.Token, request.Clock = staticToken(c.token), clock
			var logged []string
			request.Log = func(line string) { logged = append(logged, line) }
			run := &failingClone{clock: clock, result: fatal("error: RPC failed; HTTP 502 curl 22 The requested URL returned error: 502\n" + c.stderr + "\n")}
			_, err := Fetch(context.Background(), run, request)
			if len(logged) != len(fetchBackoff) {
				t.Fatalf("Fetch logged %q, want a line for each of its %d retries", logged, len(fetchBackoff))
			}
			for _, line := range logged {
				check(t, "log", line)
			}
			check(t, "retried", errorText(err))
		})
		t.Run("the shared clone's fetch, "+c.name, func(t *testing.T) {
			source, err := newProvisioningCredential(t.TempDir(), c.token)
			if err != nil {
				t.Fatal(err)
			}
			check(t, "clone-fetch", errorText(ensureFetchConfiguration(context.Background(), answersWith{word: "fetch", stderr: c.stderr}, t.TempDir(), source)))
		})
		t.Run("the clone from GitHub, "+c.name, func(t *testing.T) {
			source, err := newProvisioningCredential(t.TempDir(), c.token)
			if err != nil {
				t.Fatal(err)
			}
			cloneDir := filepath.Join(t.TempDir(), "repos", "acme", "widgets")
			check(t, "clone", errorText(ensureRepoClone(context.Background(), answersWith{word: "clone", stderr: c.stderr}, clock.Real(), cloneDir, "https://github.com/acme/widgets", "main", source)))
		})
	}
}
