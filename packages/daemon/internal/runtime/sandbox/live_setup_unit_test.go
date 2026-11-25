package sandbox

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/testexec"
	"github.com/sjawhar/legion/daemon/internal/fixturegit"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

func TestParseSetupMarker(t *testing.T) {
	t.Parallel()
	const head = "setup:architect:pod-1/3 cwd=/legion/workspaces/sjawhar/legion-smoke/s4a-1 uid=1000 ppid=41 git_from=/tmp helper=expected helper_count=1 helper_effective=1 helper_lines=2 git=ok"
	want := setupMarker{
		role: "architect", incarnation: "pod-1/3", cwd: "/legion/workspaces/sjawhar/legion-smoke/s4a-1",
		uid: "1000", ppid: "41", gitFrom: "/tmp", helper: "expected", helperCount: "1", helperEffective: "1", helperLines: "2",
		git: "ok",
	}
	for control, detail := range map[string]string{
		"refused": "",
		"ok":      "",
		// A failed control's error runs to the end of the line, spaces and `=` included.
		"failed": "exit 128: fatal: unable to access 'https://github.com/x/' : Could not resolve host: github.com a=b",
	} {
		t.Run(control, func(t *testing.T) {
			line := head + " git_without_helper=" + control + " git_without_helper_error=" + detail
			got, err := parseSetupMarker(line)
			if err != nil {
				t.Fatal(err)
			}
			want := want
			want.gitWithoutHelper, want.gitWithoutHelperError = control, detail
			if got != want {
				t.Errorf("parseSetupMarker(%q) = %#v, want %#v", line, got, want)
			}
		})
	}
}

func TestParseSetupMarkerRejectsMalformedRecords(t *testing.T) {
	t.Parallel()
	const fields = "cwd=/legion uid=1000 ppid=1 git_from=/tmp"
	const control = " git_without_helper=refused git_without_helper_error="
	for _, line := range []string{
		"architect:pod-1/3 " + fields + " helper=expected helper_count=1 helper_effective=1 helper_lines=2 git=ok" + control,
		"setup:architect:pod-1/3 " + fields + " helper=expected helper_count=1 helper_effective=1 git=ok" + control,
		"setup:architect:pod-1/3 " + fields + " helper=unknown helper_count=1 helper_effective=1 helper_lines=2 git=ok" + control,
		"setup:architect:pod-1/3 " + fields + " helper=expected helper_count=one helper_effective=1 helper_lines=2 git=ok" + control,
		"setup:architect:pod-1/3 " + fields + " helper=expected helper_count=1 helper_effective=x helper_lines=2 git=ok" + control,
		"setup:architect:pod-1/3 " + fields + " helper=expected helper_count=1 helper_effective=1 helper_lines=2 git=maybe" + control,
		"setup:architect:pod-1/3 " + fields + " helper=expected helper_count=1 helper_effective=1 helper_lines=2 git=ok git_without_helper=maybe git_without_helper_error=",
		"setup:architect:pod-1/3 " + fields + " helper=expected helper_count=1 helper_effective=1 helper_lines=2 git=ok git_without_helper=refused",
		"setup:architect:pod-1/3 " + fields + " helper=expected helper_count=1 helper_effective=1 helper_lines=2 git=ok git_without_helper_error=",
		// A failed control names its error, and only a failed one does.
		"setup:architect:pod-1/3 " + fields + " helper=expected helper_count=1 helper_effective=1 helper_lines=2 git=ok git_without_helper=failed git_without_helper_error=",
		"setup:architect:pod-1/3 " + fields + " helper=expected helper_count=1 helper_effective=1 helper_lines=2 git=ok git_without_helper=refused git_without_helper_error=exit 128: x",
		"setup:architect:pod-1/3 " + fields + " helper=expected helper_count=1 helper_effective=1 extra=2 git=ok" + control,
	} {
		t.Run(line, func(t *testing.T) {
			if _, err := parseSetupMarker(line); err == nil {
				t.Fatal("parseSetupMarker succeeded, want a refusal")
			}
		})
	}
}

func TestLiveSetupCarriesTheRepositoryAsAnArgument(t *testing.T) {
	t.Parallel()
	argv := liveSetup(setupRigRepository(t))
	if len(argv) != 5 || argv[0] != "/bin/sh" || argv[1] != "-c" || argv[3] != "stage4a-setup" || argv[4] != "https://github.com/sjawhar/legion-smoke" {
		t.Fatalf("liveSetup = %q, want /bin/sh -c <script> stage4a-setup https://github.com/sjawhar/legion-smoke", argv)
	}
	if strings.Contains(argv[2], "sjawhar/legion-smoke") {
		t.Errorf("liveSetup script embeds the repository instead of taking it as an argument: %q", argv[2])
	}
}

// The setup runs before the agent, in its directory and as its uid, and every git it runs gets the
// allowlist liveSetupScript names and nothing else: the role's GIT_CONFIG_* entries, its PATH and
// LEGION_GH_OWNERS_DIR, an empty HOME and the four fixed variables, whatever else the role's
// environment holds.
func TestLiveSetupRunsBeforeTheAgent(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (newSetupRig).
	r := newSetupRig(t, private)
	r.roleEnv()
	global := filepath.Join(r.root, "gitconfig")
	if err := os.WriteFile(global, []byte("[credential]\n\thelper = !poisoned-global-helper\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{
		"GIT_ASKPASS": "must-not-reach-git", "SSH_ASKPASS": "must-not-reach-git",
		"GH_TOKEN": "must-not-reach-git", "GITHUB_TOKEN": "must-not-reach-git",
		"GIT_CONFIG_GLOBAL": global, "GIT_CONFIG_PARAMETERS": "'http.extraheader'='Authorization: bearer fake-token'",
		"HTTPS_PROXY": "http://127.0.0.1:9", "LC_ALL": "de_DE.UTF-8", "LANG": "de_DE.UTF-8",
	} {
		t.Setenv(name, value)
	}
	agentPID := filepath.Join(r.root, "agent-pid")
	agent := []string{"/bin/sh", "-c", `printf '%s\n' "$$" >"$1"; printf '%s:%s/%s\n' "$LEGION_ROLE" "$POD_UID" "$LEGION_GENERATION" >>"$LEGION_E2E_MARKER"`, "agent", agentPID}
	stderr, lines := r.run(r.start, agent)
	if len(lines) != 2 {
		t.Fatalf("marker lines = %q, want setup then agent", lines)
	}
	setup := parseSetupMarkerOrFail(t, lines[0])
	pid, err := os.ReadFile(agentPID)
	if err != nil {
		t.Fatal(err)
	}
	if setup.ppid != strings.TrimSpace(string(pid)) {
		t.Errorf("setup parent pid %q, want agent pid %q", setup.ppid, strings.TrimSpace(string(pid)))
	}
	if setup.cwd != r.start {
		t.Errorf("setup cwd %q, want start directory %q", setup.cwd, r.start)
	}
	if setup.uid != strconv.Itoa(os.Getuid()) {
		t.Errorf("setup uid %q, want %d", setup.uid, os.Getuid())
	}
	ceiling, err := filepath.EvalSymlinks(r.tmp)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(setup.gitFrom) != ceiling || setup.git != "ok" || setup.gitWithoutHelper != "refused" || setup.gitWithoutHelperError != "" {
		t.Errorf("setup git record = from %q, git %q, git_without_helper %q (%q); want a fresh directory of %s, ok and refused", setup.gitFrom, setup.git, setup.gitWithoutHelper, setup.gitWithoutHelperError, ceiling)
	}
	if _, err := os.Stat(setup.gitFrom); !os.IsNotExist(err) {
		t.Errorf("the setup's ls-remote directory %s outlived it: %v", setup.gitFrom, err)
	}
	if err := setupGitProblem(setup); err != nil {
		t.Errorf("setupGitProblem = %v, want nil", err)
	}
	if setup.helper != "expected" || setup.helperCount != "1" || setup.helperEffective != "1" || setup.helperLines != "2" {
		t.Errorf("setup helper record = classification %q, count %q, effective %q, lines %q; want expected, one, one and two", setup.helper, setup.helperCount, setup.helperEffective, setup.helperLines)
	}
	if lines[1] != "architect:pod-1/3" {
		t.Errorf("agent marker = %q, want architect:pod-1/3", lines[1])
	}
	if strings.Contains(stderr+strings.Join(lines, "\n"), setupRigToken) {
		t.Errorf("the helper's password reached the marker or stderr: %q %q", lines, stderr)
	}

	withHelper := r.wantEnvironment(false, "")
	audits := map[string][]string{"config": withHelper, "ls-remote-1": r.wantEnvironment(false, ceiling), "ls-remote-2": r.wantEnvironment(true, ceiling)}
	for call, want := range audits {
		if got := r.audited(call + "-env"); !slices.Equal(got, want) {
			t.Errorf("git %s environment =\n%s\nwant exactly\n%s", call, strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
		if got := r.audited(call + "-home"); !slices.Equal(got, []string{"empty"}) {
			t.Errorf("git %s HOME = %q, want an empty temporary directory", call, got)
		}
	}
	for _, call := range []string{"ls-remote-1", "ls-remote-2"} {
		if got := r.audited(call + "-cwd"); !slices.Equal(got, []string{setup.gitFrom}) {
			t.Errorf("git %s ran in %q, want the record's fresh directory %s", call, got, setup.gitFrom)
		}
	}
}

// The control ls-remote runs with every command-scope helper emptied, so on a private repository
// git must stop for want of a credential. setup-ran requires git=ok and git_without_helper=refused,
// and names why it refuses anything else: a control that succeeded proves the repository needs no
// helper, and one that failed for another reason (no DNS, a GitHub 503, its timeout) proves nothing
// about the helper, so that reason, redacted, is the checkpoint's message.
func TestLiveSetupNegativeControl(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (newSetupRig).
	for name, tc := range map[string]struct {
		network setupNetwork
		helper  bool
		// away, when set, is the template the rig's mktemp makes the setup's fresh directories
		// from (setupRig.mktemp), in place of the real mktemp's random name.
		away                  string
		git, gitWithoutHelper string
		problem               string
	}{
		"the role's helper":   {network: private, helper: true, git: "ok", gitWithoutHelper: "refused"},
		"no helper":           {network: private, git: "failed", gitWithoutHelper: "refused", problem: "with the role's helper: git=failed"},
		"a public repository": {network: public, helper: true, git: "ok", gitWithoutHelper: "ok", problem: "the negative control succeeded without the role's helper"},
		"no DNS for the control": {
			network: unresolved, helper: true, git: "ok", gitWithoutHelper: "failed",
			problem: "the negative control failed for another reason: exit 128: fatal: unable to access 'https://***@github.com/sjawhar/legion-smoke/': Could not resolve host: github.com",
		},
		// mktemp's random name can open as a JWT does (a CI run's setup made tmp.eyJ2Sg7LkC), and
		// it is a path the setup made, which carries no credential.
		"no DNS for the control, from a directory whose name holds eyJ": {
			network: unresolved, helper: true, away: "tmp.eyJXXXXXXX", git: "ok", gitWithoutHelper: "failed",
			problem: "the negative control failed for another reason: exit 128: fatal: unable to access 'https://***@github.com/sjawhar/legion-smoke/': Could not resolve host: github.com",
		},
		"a GitHub 503 for the control": {
			network: serverError, helper: true, git: "ok", gitWithoutHelper: "failed",
			problem: "the negative control failed for another reason: exit 128: fatal: unable to access 'https://github.com/sjawhar/legion-smoke/': The requested URL returned error: 503",
		},
		"the control timed out": {
			network: timedOut, helper: true, git: "ok", gitWithoutHelper: "failed",
			problem: "the negative control failed for another reason: exit 124: no stderr",
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := newSetupRig(t, tc.network)
			if tc.helper {
				r.roleEnv()
			}
			if tc.away != "" {
				r.mktemp(tc.away)
			}
			setup := r.probe(r.start)
			if err := setupMarkerLeak(setup); err != nil {
				t.Fatal(err)
			}
			if tc.away != "" && !strings.HasPrefix(filepath.Base(setup.gitFrom), strings.TrimRight(tc.away, "X")) {
				t.Fatalf("git_from = %q, want a directory the rig's mktemp made from %s", setup.gitFrom, tc.away)
			}
			if setup.git != tc.git || setup.gitWithoutHelper != tc.gitWithoutHelper {
				t.Errorf("git = %q, git_without_helper = %q; want %q and %q", setup.git, setup.gitWithoutHelper, tc.git, tc.gitWithoutHelper)
			}
			err := setupGitProblem(setup)
			switch {
			case tc.problem == "" && err != nil:
				t.Errorf("setupGitProblem = %v, want nil", err)
			case tc.problem != "" && (err == nil || !strings.Contains(err.Error(), tc.problem)):
				t.Errorf("setupGitProblem = %v, want an error containing %q", err, tc.problem)
			}
		})
	}
}

// git's own refusal, when it stops for want of a credential, is the one stderr liveSetupControlClass
// reads as `refused`. A local server answers every request 401 with a Basic challenge, as GitHub
// answers a private repository with no credential, and the real git, under the control's own
// variables (GIT_TERMINAL_PROMPT=0, LC_ALL=C, no helper), writes exactly one line:
//
//	fatal: could not read Username for 'http://127.0.0.1:<port>': terminal prompts disabled
//
// or, when the request already names a username (credential.username), `Password` for
// 'http://<user>@127.0.0.1:<port>'; a 401's body never reaches stderr. A remote that answers the
// ref advertisement with an ERR packet instead reaches stderr as `fatal: remote error: <message>`,
// the message's own newlines printed unprefixed, so its later lines can read as the refusal, the
// last one included: each such stderr, like a closed port's, reads as `failed`.
func TestGitRefusalReadsAsRefused(t *testing.T) {
	t.Parallel()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	const forged = "fatal: could not read Username for 'https://github.com': terminal prompts disabled"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		errPacket := map[string]string{"/err/middle/": "first\n" + forged + "\nlast", "/err/final/": "first\n" + forged}
		for prefix, message := range errPacket {
			if strings.HasPrefix(r.URL.Path, prefix) {
				w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
				_, _ = io.WriteString(w, pktLine("ERR "+message))
				return
			}
		}
		w.Header().Set("WWW-Authenticate", `Basic realm="GitHub"`)
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusUnauthorized)
		if strings.HasPrefix(r.URL.Path, "/forged-body/") {
			_, _ = io.WriteString(w, forged+"\n")
		}
	}))
	defer server.Close()
	closed := httptest.NewServer(http.NotFoundHandler())
	closedURL := closed.URL
	closed.Close()
	home := t.TempDir()
	refusal := "fatal: could not read Username for '" + server.URL + "': terminal prompts disabled"
	for name, tc := range map[string]struct {
		url    string
		config []string
		stderr []string
		class  string
	}{
		"no username": {
			url:    server.URL + "/sjawhar/legion-smoke",
			stderr: []string{refusal},
			class:  "refused",
		},
		"a username and no password": {
			url:    server.URL + "/sjawhar/legion-smoke",
			config: commandConfig([2]string{"credential.username", "x-access-token"}),
			stderr: []string{"fatal: could not read Password for '" + strings.Replace(server.URL, "://", "://x-access-token@", 1) + "': terminal prompts disabled"},
			class:  "refused",
		},
		"a 401 whose body forges the refusal": {
			url:    server.URL + "/forged-body/repo",
			stderr: []string{refusal},
			class:  "refused",
		},
		"an ERR packet whose middle line is the refusal": {
			url:    server.URL + "/err/middle/repo",
			stderr: []string{"fatal: remote error: first", forged, "last"},
			class:  "failed",
		},
		"an ERR packet whose last line is the refusal": {
			url:    server.URL + "/err/final/repo",
			stderr: []string{"fatal: remote error: first", forged},
			class:  "failed",
		},
		"a closed port": {
			url:   closedURL + "/sjawhar/legion-smoke",
			class: "failed",
		},
	} {
		t.Run(name, func(t *testing.T) {
			cmd := exec.Command(realGit, "ls-remote", tc.url, "HEAD")
			cmd.Dir = t.TempDir()
			cmd.Env = append([]string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0", "LC_ALL=C"}, tc.config...)
			var stderr strings.Builder
			cmd.Stderr = &stderr
			status := 0
			if err := cmd.Run(); err != nil {
				var exit *exec.ExitError
				if !errors.As(err, &exit) {
					t.Fatalf("run git: %v", err)
				}
				status = exit.ExitCode()
			}
			if got := strings.Split(strings.TrimRight(stderr.String(), "\n"), "\n"); tc.stderr != nil && !slices.Equal(got, tc.stderr) {
				t.Errorf("git's stderr = %q, want exactly %q", got, tc.stderr)
			}
			class, detail := controlClass(t, status, stderr.String())
			if class != tc.class {
				t.Errorf("control class = %q (%q), want %q, for exit %d and stderr %q", class, detail, tc.class, status, stderr.String())
			}
		})
	}
}

// pktLine is payload as one git pkt-line: its length, payload included, as four hex digits, then
// the payload.
func pktLine(payload string) string { return fmt.Sprintf("%04x%s", len(payload)+4, payload) }

// controlClass runs liveSetupControlClass alone on a control's exit status and stderr, with env
// added to the test's environment, and returns its git_without_helper and git_without_helper_error.
func controlClass(t *testing.T, status int, stderr string, env ...string) (string, string) {
	t.Helper()
	script := "status=$1\ncontrol=$2\n" + liveSetupControlClass + `printf '%s\n%s\n' "$git_without_helper" "$git_without_helper_error"`
	cmd := exec.Command("/bin/sh", "-c", script, "control-class", strconv.Itoa(status), strings.TrimRight(stderr, "\n"))
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("run liveSetupControlClass: %v", err)
	}
	class, detail, _ := strings.Cut(strings.TrimSuffix(string(out), "\n"), "\n")
	return class, detail
}

// A failed control's error is git's first stderr line with control characters removed, and a
// GitHub token, a URL's user info and a JWT each replaced by `***`, since the marker is read with
// kubectl exec and printed in the run's log. A JWT is found by its header, as setupMarkerLeak
// finds one: in compact JSON or with whitespace, behind other base64url characters or alone. A
// dotted name with no such header stays.
func TestLiveSetupControlErrorIsRedacted(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct{ stderr, want string }{
		"a stateless installation token": {
			stderr: "fatal: unable to access 'https://github.com/x/': token " + setupRigOutageToken + " expired",
			want:   "exit 128: fatal: unable to access 'https://github.com/x/': token ghs_*** expired",
		},
		"a personal access token": {
			stderr: "fatal: auth ghp_EXAMPLEtoken0123456789abcdefghijklmn rejected",
			want:   "exit 128: fatal: auth ghp_*** rejected",
		},
		"a bare JWT": {
			stderr: "fatal: bearer eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJ4In0.c2ln refused",
			want:   "exit 128: fatal: bearer *** refused",
		},
		// RFC 7515 lets a header hold whitespace: `{ "alg"` and `{ "iss"` each open eyAi, not eyJ.
		"a JWT whose header holds whitespace": {
			stderr: "fatal: bearer " + compactJWT(`{ "alg":"HS256"}`, `{ "iss":"123456"}`, "signature") + " refused",
			want:   "exit 128: fatal: bearer *** refused",
		},
		"a JWT behind other base64url characters": {
			stderr: "fatal: bearer x" + compactJWT(`{"alg":"RS256"}`, `{"iss":"123456"}`, "signature") + " refused",
			want:   "exit 128: fatal: bearer *** refused",
		},
		"dotted names": {
			stderr: "fatal: unable to access 'https://api.github.com/x/': git 2.43.0 could not resolve host: api.github.com",
			want:   "exit 128: fatal: unable to access 'https://api.github.com/x/': git 2.43.0 could not resolve host: api.github.com",
		},
		"user info in a URL": {
			stderr: "fatal: unable to access 'https://x-access-token:s3cret@github.com/x/': Could not resolve host: github.com",
			want:   "exit 128: fatal: unable to access 'https://***@github.com/x/': Could not resolve host: github.com",
		},
		"only the first line": {
			stderr: "fatal: first\nfatal: second ghp_EXAMPLEtoken0123456789abcdefghijklmn",
			want:   "exit 128: fatal: first",
		},
		"control characters": {
			stderr: "fatal: \x1b[31mred\x1b[0m\tline",
			want:   "exit 128: fatal: [31mred[0mline",
		},
		"nothing on stderr": {
			stderr: "",
			want:   "exit 128: no stderr",
		},
	} {
		t.Run(name, func(t *testing.T) {
			class, detail := controlClass(t, 128, tc.stderr)
			if class != "failed" || detail != tc.want {
				t.Errorf("control = %q, %q; want failed, %q", class, detail, tc.want)
			}
		})
	}
}

// compactJWT is a JWT in compact serialization: header, claims and signature, each base64url.
func compactJWT(header, claims, signature string) string {
	encode := base64.RawURLEncoding.EncodeToString
	return encode([]byte(header)) + "." + encode([]byte(claims)) + "." + encode([]byte(signature))
}

// The setup runs under the role's locale, and in a UTF-8 locale a bracket expression matches more
// than ASCII: GNU grep's [A-Za-z] takes é into a candidate run, whose decode then fails at every
// offset, and GNU sed's [:space:] holds an em space, which stops the user-info match short of its
// `@`. So each row's error, read under each locale, is the one the C locale gives. A locale this
// machine lacks reads as C, and the row then cannot see the fault there: the test logs each such
// locale, and needs one that sees it.
func TestLiveSetupControlErrorIsRedactedInEveryLocale(t *testing.T) {
	emSpace := "\u2003"
	rows := map[string]struct{ stderr, want string }{
		"a JWT behind letters outside ASCII": {
			stderr: "fatal: bearer éééé" + compactJWT(`{"alg":"none"}`, `{}`, "") + " refused",
			want:   "exit 128: fatal: bearer éééé*** refused",
		},
		"user info that holds an em space": {
			stderr: "fatal: unable to access 'https://x-access-token:s3" + emSpace + "cret@github.com/x/': Could not resolve host: github.com",
			want:   "exit 128: fatal: unable to access 'https://***@github.com/x/': Could not resolve host: github.com",
		},
	}
	seen := 0
	for _, locale := range []string{"C", "C.UTF-8", "en_US.UTF-8"} {
		// The fault's own mechanism, read in this locale: [:space:] holding an em space.
		probe := exec.Command("/bin/sh", "-c", `printf 'a%sb@\n' "$1" | sed -E 's/[^@[:space:]]+@/x/'`, "probe", emSpace)
		probe.Env = append(os.Environ(), "LC_ALL="+locale)
		out, err := probe.Output()
		if err != nil {
			t.Fatalf("probe %s: %v", locale, err)
		}
		if strings.TrimSpace(string(out)) == "x" {
			t.Logf("LC_ALL=%s classes an em space as no space, as C does: its rows cannot see the fault here", locale)
		} else {
			seen++
		}
		for name, tc := range rows {
			t.Run(locale+"/"+name, func(t *testing.T) {
				class, detail := controlClass(t, 128, tc.stderr, "LC_ALL="+locale)
				if class != "failed" || detail != tc.want {
					t.Errorf("control under LC_ALL=%s = %q, %q; want failed, %q", locale, class, detail, tc.want)
				}
			})
		}
	}
	if seen == 0 {
		t.Fatal("no locale this test reads classes an em space as a space, so none of its rows can fail for the locale fault")
	}
}

// setupMarkerLeak finds a JWT in any field of a record but its two paths: in the control's error
// and in a field no program's text should reach, with its header as compact JSON or with
// whitespace. It is quiet on CI's record, whose git_from, a name mktemp drew at random, holds eyJ,
// on a JWT in the paths themselves, and on a dotted host name in the control's error.
func TestSetupMarkerLeakFindsAJWTOutsideTheRecordsPaths(t *testing.T) {
	ci := parseSetupMarkerOrFail(t, "setup:architect:pod-1/3 cwd=/tmp/TestLiveSetupNegativeControlno_DNS_for_the_control3505482992/001/start uid=1001 ppid=67790 git_from=/tmp/TestLiveSetupNegativeControlno_DNS_for_the_control3505482992/001/tmp/tmp.eyJ2Sg7LkC helper=expected helper_count=1 helper_effective=1 helper_lines=2 git=ok git_without_helper=failed git_without_helper_error=exit 128: fatal: unable to access 'https://***@github.com/sjawhar/legion-smoke/': Could not resolve host: github.com")
	appJWT := compactJWT(`{"alg":"RS256","typ":"JWT"}`, `{"iat":1764036000,"exp":1764036600,"iss":"123456"}`, "signature")
	for name, tc := range map[string]struct {
		plant func(m *setupMarker)
		leak  bool
	}{
		"a GitHub App's JWT in the control's error": {
			plant: func(m *setupMarker) { m.gitWithoutHelperError = "exit 128: fatal: bearer " + appJWT + " refused" },
			leak:  true,
		},
		// RFC 7519's unsecured JWT: the shortest payload, {}, and an empty signature.
		"an unsecured JWT with no claims in the control's error": {
			plant: func(m *setupMarker) {
				m.gitWithoutHelperError = "exit 128: fatal: bearer " + compactJWT(`{"alg":"none"}`, `{}`, "") + " refused"
			},
			leak: true,
		},
		// RFC 7515 lets a header hold whitespace, and `{ "alg"` opens eyAi, not eyJ.
		"a JWT whose header holds whitespace in the control's error": {
			plant: func(m *setupMarker) {
				m.gitWithoutHelperError = "exit 128: fatal: bearer " + compactJWT(`{ "alg":"HS256"}`, `{"iss":"123456"}`, "signature") + " refused"
			},
			leak: true,
		},
		"the rig's outage token in the control's error": {
			plant: func(m *setupMarker) {
				m.gitWithoutHelperError = "exit 128: fatal: unable to access 'https://x-access-token:" + setupRigOutageToken + "@github.com/sjawhar/legion-smoke/': Could not resolve host: github.com"
			},
			leak: true,
		},
		// parseSetupMarker takes any word as the uid, so a script that printed the wrong variable
		// there would put the token in it.
		"the rig's outage token as the uid": {plant: func(m *setupMarker) { m.uid = setupRigOutageToken }, leak: true},
		"CI's record":                       {plant: func(*setupMarker) {}},
		"a JWT in the setup's own paths": {
			plant: func(m *setupMarker) { m.cwd, m.gitFrom = "/legion/"+appJWT+"/start", "/tmp/"+appJWT },
		},
		// Three dotted parts, each base64url, with no JSON header among them.
		"a dotted host name in the control's error": {
			plant: func(m *setupMarker) {
				m.gitWithoutHelperError = "exit 128: fatal: unable to access 'https://api.github.com/sjawhar/legion-smoke/': Could not resolve host: api.github.com"
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			m := ci
			tc.plant(&m)
			if err := setupMarkerLeak(m); (err != nil) != tc.leak {
				t.Errorf("setupMarkerLeak(%+v) = %v, want a leak: %t", m, err, tc.leak)
			}
		})
	}
}

// The ls-remote calls run in a fresh directory, with git's repository discovery stopped at its
// parent (GIT_CEILING_DIRECTORIES). A .git in that parent, which an operator's volume at /tmp or an
// earlier generation's agent could leave, with a url.insteadOf that sends github.com to a server
// answering the first ls-remote and refusing the second, would otherwise make a broken helper read
// as git=ok with the control refused. The server answers neither call, and the record is the one
// the probe gives without that .git.
func TestLiveSetupIgnoresARepositoryAboveItsDirectory(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (newSetupRig).
	for name, helper := range map[string]string{
		"the role's helper": workspace.GitHubCredentialHelper,
		"a broken helper":   "!different git-credential",
	} {
		t.Run(name, func(t *testing.T) {
			r := newSetupRig(t, private)
			r.roleEnv()
			t.Setenv("GIT_CONFIG_VALUE_1", helper)
			clean := r.probe(r.start)

			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if requests.Add(1) == 1 {
					w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
					_, _ = io.WriteString(w, pktLine("# service=git-upload-pack\n")+"0000"+
						pktLine("0123456789012345678901234567890123456789 HEAD\x00agent=forged\n")+"0000")
					return
				}
				w.Header().Set("WWW-Authenticate", `Basic realm="GitHub"`)
				w.WriteHeader(http.StatusUnauthorized)
			}))
			defer server.Close()
			r.git(fixturegit.InitWorkTree(r.tmp, "")[1:]...)
			r.git("-C", r.tmp, "config", "url."+server.URL+"/.insteadOf", "https://github.com/")

			attacked := r.probe(r.start)
			if got, want := setupOutcome(attacked), setupOutcome(clean); got != want {
				t.Errorf("record with a .git above its directory = %s, want the record without it, %s", got, want)
			}
			if n := requests.Load(); n != 0 {
				t.Errorf("the rewritten server answered %d ls-remote calls, want none", n)
			}
		})
	}
}

// setup-ran reads git_from as the directory the ls-remote calls ran in, and refuses one that is
// not absolute or is the setup's own directory, the issue clone, or below it.
func TestSetupGitProblemRefusesTheCloneDirectory(t *testing.T) {
	t.Parallel()
	base := setupMarker{cwd: "/legion/workspaces/sjawhar/legion-smoke/s4a-1", git: "ok", gitWithoutHelper: "refused"}
	for gitFrom, accepted := range map[string]bool{
		"/tmp/tmp.Ab12Cd": true,
		"/legion/workspaces/sjawhar/legion-smoke/s4a-10":  true,
		"/legion/workspaces/sjawhar/legion-smoke/s4a-1":   false,
		"/legion/workspaces/sjawhar/legion-smoke/s4a-1/x": false,
		"tmp.Ab12Cd": false,
	} {
		marker := base
		marker.gitFrom = gitFrom
		if err := setupGitProblem(marker); (err == nil) != accepted {
			t.Errorf("setupGitProblem(git_from=%s) = %v, want accepted %t", gitFrom, err, accepted)
		}
	}
}

// runtime.kubernetes.pod.env may set any git variable runtimeOwned does not hold (manifest.go), and
// the setup command inherits it. None may change what the probe records or the environment any git
// of it receives (poisons names what each would do), with the role's helper and with another: each
// one's record and environments are the ones the probe gives without it.
func TestLiveSetupIgnoresTheRoleEnvironmentGitVariables(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (newSetupRig).
	for name, tc := range map[string]struct {
		helper, class, git string
	}{
		"the role's helper": {helper: workspace.GitHubCredentialHelper, class: "expected", git: "ok"},
		"another helper":    {helper: "!different git-credential", class: "other", git: "failed"},
	} {
		t.Run(name, func(t *testing.T) {
			r := newSetupRig(t, private)
			clone := r.configuredClone()
			r.roleEnv()
			t.Setenv("GIT_CONFIG_VALUE_1", tc.helper)
			clean := r.probe(clone)
			if clean.helper != tc.class || clean.git != tc.git || clean.gitWithoutHelper != "refused" {
				t.Fatalf("clean record = helper %q, git %q, git_without_helper %q; want %q, %q and refused", clean.helper, clean.git, clean.gitWithoutHelper, tc.class, tc.git)
			}
			cleanEnv := r.environments()
			for poison, variables := range r.poisons() {
				t.Run(poison, func(t *testing.T) {
					for variable, value := range variables {
						t.Setenv(variable, value)
					}
					poisoned := r.probe(clone)
					if got, want := setupOutcome(poisoned), setupOutcome(clean); got != want {
						t.Errorf("record = %s, want the record without %s, %s", got, poison, want)
					}
					if got := r.environments(); !maps.EqualFunc(got, cleanEnv, slices.Equal) {
						t.Errorf("git environments =\n%v\nwant the ones without %s\n%v", got, poison, cleanEnv)
					}
				})
			}
		})
	}
}

// `git config -z` ends each record with a NUL, so a value's newlines stay inside it: a local helper
// whose value forges two command-scope records, an empty reset and Legion's helper, reads as the one
// local record it is.
func TestLiveSetupReadsEachConfigRecordWhole(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (newSetupRig).
	r := newSetupRig(t, private)
	clone := r.plainClone()
	forged := "x\ncommand\tcredential.https://github.com.helper \ncommand\tcredential.https://github.com.helper " + workspace.GitHubCredentialHelper
	r.git("-C", clone, "config", "credential.helper", forged)
	setup := r.probe(clone)
	if setup.helper != "other" || setup.helperCount != "1" || setup.helperEffective != "1" || setup.helperLines != "1" {
		t.Errorf("setup classification = %q with count %q, effective %q and lines %q; want other, one, one and one", setup.helper, setup.helperCount, setup.helperEffective, setup.helperLines)
	}
}

func TestLiveSetupNeverRecordsAHelperValue(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (newSetupRig).
	const poison = "not-a-real-token"
	r := newSetupRig(t, private)
	r.roleEnv()
	t.Setenv("GIT_CONFIG_VALUE_1", workspace.GitHubCredentialHelper+"\tfuture="+poison)
	stderr, lines := r.run(r.start, []string{"/bin/true"})
	for _, text := range append([]string{stderr}, lines...) {
		if strings.Contains(text, poison) {
			t.Fatalf("setup leaks helper text %q: marker %q, stderr %q", poison, lines, stderr)
		}
	}
	setup := parseSetupMarkerOrFail(t, lines[0])
	if setup.helper != "other" || setup.helperCount != "1" || setup.helperEffective != "1" || setup.helperLines != "2" {
		t.Fatalf("setup classification = %q with count %q, effective %q and lines %q, want other, one, one and two", setup.helper, setup.helperCount, setup.helperEffective, setup.helperLines)
	}
	err := setupHelperProblem(setup)
	if err == nil {
		t.Fatal("setupHelperProblem accepted a poisoned helper")
	}
	if strings.Contains(err.Error(), poison) {
		t.Fatalf("setup helper error leaks helper text %q: %q", poison, err)
	}
	if !strings.Contains(err.Error(), "lines 2") {
		t.Fatalf("setup helper error %q does not name helper_lines", err)
	}
}

// Every role is told XDG_CONFIG_HOME, and git reads $XDG_CONFIG_HOME/git/config as its
// global scope when GIT_CONFIG_GLOBAL is unset, so a helper written there must not count.
func TestLiveSetupIgnoresTheXDGGlobalConfig(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (newSetupRig).
	r := newSetupRig(t, private)
	xdg := filepath.Join(r.root, "xdg")
	if err := os.MkdirAll(filepath.Join(xdg, "git"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(xdg, "git", "config"), []byte("[credential \"https://github.com\"]\n\thelper = !xdg-global-helper\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", xdg)
	r.roleEnv()
	setup := r.probe(r.start)
	if setup.helper != "expected" || setup.helperLines != "2" {
		t.Errorf("setup helper record = classification %q, lines %q; want expected and two, with the XDG global config unread", setup.helper, setup.helperLines)
	}
}

func TestLiveSetupClassifiesGitCredentialEnv(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (newSetupRig).
	for name, changed := range map[string]bool{
		"the runtime helper": false,
		"a changed helper":   true,
	} {
		t.Run(name, func(t *testing.T) {
			r := newSetupRig(t, private)
			r.roleEnv()
			if changed {
				t.Setenv("GIT_CONFIG_VALUE_1", "!different git-credential")
			}
			setup := r.probe(r.start)
			want := "expected"
			if changed {
				want = "other"
			}
			if setup.helper != want || setup.helperCount != "1" || setup.helperEffective != "1" || setup.helperLines != "2" {
				t.Errorf("setup classification = %q with count %q, effective %q and lines %q, want %q, one, one and two", setup.helper, setup.helperCount, setup.helperEffective, setup.helperLines, want)
			}
		})
	}
}

func TestLiveSetupClassifiesEffectiveRepositoryHelpers(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (newSetupRig).
	extra := func(key, value string) func(t *testing.T, r *setupRig, clone string) {
		return func(t *testing.T, r *setupRig, _ string) {
			r.roleEnv()
			index := strconv.Itoa(len(gitConfigPairs))
			t.Setenv("GIT_CONFIG_COUNT", strconv.Itoa(len(gitConfigPairs)+1))
			t.Setenv("GIT_CONFIG_KEY_"+index, key)
			t.Setenv("GIT_CONFIG_VALUE_"+index, value)
		}
	}
	for name, tc := range map[string]struct {
		configure                       func(t *testing.T, r *setupRig, clone string)
		helper, count, effective, lines string
	}{
		"the live repository and runtime environment": {
			configure: func(_ *testing.T, r *setupRig, _ string) { r.roleEnv() },
			helper:    "expected", count: "3", effective: "1", lines: "6",
		},
		"only the repository helper": {
			configure: func(*testing.T, *setupRig, string) {},
			helper:    "other", count: "2", effective: "1", lines: "4",
		},
		"an extra command helper": {
			configure: extra("credential.https://github.com.helper", "!extra credential"),
			helper:    "other", count: "4", effective: "2", lines: "7",
		},
		"a local non-legion helper cleared by command scope": {
			configure: func(_ *testing.T, r *setupRig, clone string) {
				r.roleEnv()
				r.git("-C", clone, "config", "--add", "credential.https://github.com.helper", "!local credential")
			},
			helper: "expected", count: "4", effective: "1", lines: "7",
		},
		"a generic command helper after legion's": {
			configure: extra("credential.helper", "!generic credential"),
			helper:    "other", count: "4", effective: "2", lines: "7",
		},
		"a command helper under another URL form after legion's": {
			configure: extra("credential.https://github.com/.helper", "!slash credential"),
			helper:    "other", count: "4", effective: "2", lines: "7",
		},
		"an empty command helper under another URL form after legion's": {
			configure: extra("credential.https://github.com/.helper", ""),
			helper:    "other", count: "3", effective: "2", lines: "7",
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := newSetupRig(t, private)
			clone := r.configuredClone()
			tc.configure(t, r, clone)
			setup := r.probe(clone)
			if setup.helper != tc.helper || setup.helperCount != tc.count || setup.helperEffective != tc.effective || setup.helperLines != tc.lines {
				t.Errorf("setup classification = %q with count %q, effective %q and lines %q, want %q, %q, %q and %q", setup.helper, setup.helperCount, setup.helperEffective, setup.helperLines, tc.helper, tc.count, tc.effective, tc.lines)
			}
		})
	}
}

// git runs credential.helper and credential.https://github.com.helper as one list for an
// https://github.com request, in its read order, so liveSetupScript must classify both keys
// together: two fake helpers that record only their own name show the order, and an empty
// command-scope url-specific value drops a local generic helper.
func TestGitCombinesBothHelperKeysInReadOrder(t *testing.T) {
	t.Parallel()
	h := newHelperLog(t)
	generic, specific := h.helper("generic"), h.helper("specific")
	genericFirst := h.repo("generic-first", [2]string{"credential.helper", generic}, [2]string{"credential.https://github.com.helper", specific})
	if got := h.fill(genericFirst); strings.Join(got, " ") != "generic specific" {
		t.Errorf("helpers run = %q, want generic then specific: git combines both keys in read order", got)
	}
	specificFirst := h.repo("specific-first", [2]string{"credential.https://github.com.helper", specific}, [2]string{"credential.helper", generic})
	if got := h.fill(specificFirst); strings.Join(got, " ") != "specific generic" {
		t.Errorf("helpers run = %q, want specific then generic", got)
	}
	reset := commandConfig([2]string{"credential.https://github.com.helper", ""}, [2]string{"credential.https://github.com.helper", specific})
	if got := h.fill(h.repo("reset", [2]string{"credential.helper", generic}), reset...); strings.Join(got, " ") != "specific" {
		t.Errorf("helpers run = %q, want specific alone: the empty command-scope value drops the local generic helper", got)
	}
}

// For a request for https://github.com/acme/widgets.git with credential.useHttpPath on, git runs a
// helper set under any URL form that matches the request, not only credential.https://github.com:
// one with a trailing slash, a path prefix, no scheme, a wildcard host or the default port. It
// ignores a form for another path, another scheme or another host. An empty value under a matching
// form empties the list as credential.https://github.com's does. So liveSetupScript counts a record
// under any other helper key as an entry, never as a reset.
func TestGitCredentialHelperURLForms(t *testing.T) {
	t.Parallel()
	h := newHelperLog(t)
	ran := h.helper("ran")
	outside := t.TempDir()
	for key, applies := range map[string]bool{
		"credential.https://github.com.helper":                  true,
		"credential.https://github.com/.helper":                 true,
		"credential.https://github.com/acme.helper":             true,
		"credential.https://github.com/acme/widgets.git.helper": true,
		"credential.github.com.helper":                          true,
		"credential.https://*.com.helper":                       true,
		"credential.https://github.com:443.helper":              true,
		"credential.https://github.com/other.helper":            false,
		"credential.http://github.com.helper":                   false,
		"credential.https://gist.github.com.helper":             false,
	} {
		got := h.fill(outside, commandConfig([2]string{key, ran}, [2]string{"credential.useHttpPath", "true"})...)
		if want := map[bool]string{true: "ran", false: ""}[applies]; strings.Join(got, " ") != want {
			t.Errorf("%s: helpers run = %q, want %q", key, got, want)
		}
	}
	generic := h.helper("generic")
	reset := commandConfig([2]string{"credential.helper", generic}, [2]string{"credential.https://github.com/.helper", ""})
	if got := h.fill(outside, reset...); len(got) != 0 {
		t.Errorf("helpers run = %q, want none: an empty value under credential.https://github.com/ empties the list", got)
	}
}

// setupRig runs liveSetupScript through the real start wrapper from a directory of its own, with
// TMPDIR at a directory of the rig's own, where the script's mktemp makes the fresh directory its
// ls-remote calls run in; a fake git first on PATH that hands `git config` to the real git and
// stands in for ls-remote; and a fake `legion` whose `git-credential get` answers a password only
// under the owners directory the rig names. The fake ls-remote reads its configuration through the
// real git as a real ls-remote does: from the directory it runs in, with git's repository discovery
// and GIT_CEILING_DIRECTORIES, never through GIT_CONFIG. It succeeds when that configuration
// rewrites github.com with url.insteadOf (the rewritten server answers it) or names an
// http.extraHeader for github.com, or when the real git's `credential fill` for
// sjawhar/legion-smoke, run with the environment the probe gave it, returns a password. Otherwise it
// ends as the rig's network says: as git refuses a private repository with no credential
// (TestGitRefusalReadsAsRefused quotes that text from the real git), or as git fails when the host
// does not resolve, or on a GitHub 5xx. Each fake call records its environment, its HOME's
// emptiness and its directory under the rig's audit directory.
type setupRig struct {
	t                                          *testing.T
	root, start, bin, audit, owners, tmp, home string
	marker, realGit                            string
}

// setupNetwork is how the rig's fake ls-remote ends when nothing authenticates it. The three
// outages start after the helper's ls-remote has passed, so only the negative control meets them.
type setupNetwork string

const (
	// private is GitHub answering a private repository 401: git stops for want of a credential.
	private setupNetwork = "private"
	// public is a repository anyone may read: every ls-remote succeeds.
	public setupNetwork = "public"
	// unresolved is a DNS outage: the control never reaches GitHub.
	unresolved setupNetwork = "unresolved"
	// serverError is GitHub answering the control 503.
	serverError setupNetwork = "server-error"
	// timedOut is the control outliving its `timeout 60`, which kills git and exits 124 with
	// nothing on stderr.
	timedOut setupNetwork = "timed-out"
)

const setupRigToken = "fake-installation-token"

// setupRigOutageToken is a GitHub-token-shaped string the rig's outage errors carry, ghs_ and an
// id before a JWT, so a test can see the control's recorded error never holds it (setupMarkerLeak).
const setupRigOutageToken = "ghs_1234567_eyJhbGciOiJSUzI1NiJ9.eyJpbnN0YWxsYXRpb25faWQiOjF9.c2lnbmF0dXJl"

// setupMarkerLeak is an error when a setup record carries a JWT, as setupRigOutageToken does. It
// reads every field but the two paths the setup made itself, cwd (pwd -P) and git_from (mktemp -d,
// whose random name can open with eyJ as a JWT's header does), which carry no credential. The field
// meant to hold another program's text is git_without_helper_error, a failed control's first
// stderr line. fmt prints the fields with a space between them, so no JWT spans two.
func setupMarkerLeak(m setupMarker) error {
	m.cwd, m.gitFrom = "", ""
	if holdsJWT(fmt.Sprint(m)) {
		return fmt.Errorf("the setup record, its paths aside, holds a JWT: %+v", m)
	}
	return nil
}

// holdsJWT reports whether text holds a JWT in compact serialization: a header, a dot, a payload
// (`{}` is e30; a JWE's encrypted key may be empty), a dot and the rest, each part base64url. The
// header is the one part read: it is a JSON object that names its alg (RFC 7515 4.1.1, RFC 7516
// 4.1.1). Decoding it finds a header written with whitespace, `{ "alg"` (eyAi), as well as compact
// JSON's eyJ, and finds one behind other base64url characters, as setupRigOutageToken's follows
// ghs_1234567_; a dotted name such as api.github.com has no such header.
func holdsJWT(text string) bool {
	notBase64URL := func(r rune) bool {
		return !('A' <= r && r <= 'Z' || 'a' <= r && r <= 'z' || '0' <= r && r <= '9' || r == '-' || r == '_')
	}
	parts := strings.Split(text, ".")
	for i := range len(parts) - 2 {
		if strings.IndexFunc(parts[i+1], notBase64URL) >= 0 {
			continue
		}
		run := parts[i][strings.LastIndexFunc(parts[i], notBase64URL)+1:]
		for start := range len(run) {
			header, err := base64.RawURLEncoding.DecodeString(run[start:])
			var fields map[string]json.RawMessage
			if err == nil && json.Unmarshal(header, &fields) == nil && fields["alg"] != nil {
				return true
			}
		}
	}
	return false
}

const setupRigGit = `#!/bin/sh
real=%s
audit=%s
network=%s
environment() {
  env | grep -v -E '^(PWD|SHLVL|OLDPWD|_|HOME)=' | LC_ALL=C sort
}
home() {
  if [ -z "$(ls -A "$HOME")" ]; then echo empty; else echo nonempty; fi
}
case $1 in
config)
  environment >"$audit/config-env"
  home >"$audit/config-home"
  exec "$real" "$@"
  ;;
ls-remote)
  n=1
  while [ -e "$audit/ls-remote-$n-env" ]; do n=$((n + 1)); done
  environment >"$audit/ls-remote-$n-env"
  home >"$audit/ls-remote-$n-home"
  pwd >"$audit/ls-remote-$n-cwd"
  if [ "$n" -gt 1 ]; then
    case $network in
    unresolved)
      echo "fatal: unable to access 'https://x-access-token:%s@github.com/sjawhar/legion-smoke/': Could not resolve host: github.com" >&2
      exit 128
      ;;
    server-error)
      echo "fatal: unable to access 'https://github.com/sjawhar/legion-smoke/': The requested URL returned error: 503" >&2
      exit 128
      ;;
    timed-out)
      exit 124
      ;;
    esac
  fi
  if [ "$network" != public ]; then
    rewrite=$(env -u GIT_CONFIG "$real" config --get-regexp '^url\..*\.insteadof$' 2>/dev/null) || rewrite=
    [ -z "$rewrite" ] || exec "$real" "$@"
    header=$(env -u GIT_CONFIG "$real" config --get-urlmatch http.extraheader https://github.com/ 2>/dev/null) || header=
    if [ -z "$header" ]; then
      answer=$(printf 'protocol=https\nhost=github.com\npath=sjawhar/legion-smoke.git\n\n' | "$real" credential fill 2>/dev/null) || answer=
      case $answer in
      *password=?*) ;;
      *)
        echo "fatal: could not read Username for 'https://github.com/sjawhar/legion-smoke': terminal prompts disabled" >&2
        exit 128
        ;;
      esac
    fi
  fi
  printf '0123456789012345678901234567890123456789\tHEAD\n'
  ;;
*)
  echo "fake git: unexpected arguments: $*" >&2
  exit 2
  ;;
esac
`

const setupRigLegion = `#!/bin/sh
[ "$1" = git-credential ] && [ "$2" = get ] || exit 0
cat >/dev/null
[ "${LEGION_GH_OWNERS_DIR-}" = %s ] || exit 1
printf 'username=x-access-token\npassword=%s\n'
`

func newSetupRig(t *testing.T, network setupNetwork) *setupRig {
	t.Helper()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	r := &setupRig{
		t: t, root: root, start: filepath.Join(root, "start"), bin: filepath.Join(root, "bin"),
		audit: filepath.Join(root, "audit"), owners: filepath.Join(root, "owners"), tmp: filepath.Join(root, "tmp"),
		marker: filepath.Join(root, "marker"), realGit: realGit, home: filepath.Join(root, "home"),
	}
	for _, dir := range []string{r.start, r.bin, r.owners, r.tmp, r.home} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	scripts := map[string]string{
		"git":    fmt.Sprintf(setupRigGit, shellQuote(realGit), shellQuote(r.audit), network, setupRigOutageToken),
		"legion": fmt.Sprintf(setupRigLegion, shellQuote(r.owners), setupRigToken),
	}
	for name, script := range scripts {
		if err := testexec.WriteFile(filepath.Join(r.bin, name), []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	// The test process may inherit command-scope git entries of its own; the rig starts with none.
	for name, value := range map[string]string{
		"GIT_CONFIG_COUNT": "0", "LEGION_E2E_MARKER": r.marker, "LEGION_ROLE": "architect", "POD_UID": "pod-1",
		"LEGION_GENERATION": "3", "LEGION_GH_OWNERS_DIR": r.owners, "TMPDIR": r.tmp,
	} {
		t.Setenv(name, value)
	}
	return r
}

// roleEnv sets the role's command-scope git entries, GitConfigEnv's as the runtime sets them.
func (r *setupRig) roleEnv() {
	for _, variable := range GitConfigEnv() {
		r.t.Setenv(variable.Name, variable.Value)
	}
}

// mktemp puts a mktemp first on the rig's PATH whose `mktemp -d` makes its directory from
// template under TMPDIR, as the real one makes tmp.XXXXXXXXXX there, so a case chooses what the
// setup's fresh directory's name holds. Any other call is the real mktemp's.
func (r *setupRig) mktemp(template string) {
	r.t.Helper()
	real, err := exec.LookPath("mktemp")
	if err != nil {
		r.t.Fatal(err)
	}
	script := fmt.Sprintf("#!/bin/sh\n[ \"$*\" = -d ] || exec %[1]s \"$@\"\nexec %[1]s -d \"${TMPDIR:-/tmp}\"/%[2]s\n", shellQuote(real), shellQuote(template))
	if err := testexec.WriteFile(filepath.Join(r.bin, "mktemp"), []byte(script), 0o700); err != nil {
		r.t.Fatal(err)
	}
}

// poisons are the git variables the role's environment may carry, each set that would change what
// the probe reads or how its ls-remote authenticates were it to reach the probe's git: GIT_CONFIG
// makes `git config` alone read a forged file holding Legion's helper, GIT_DIR and GIT_COMMON_DIR
// hand git another repository's configuration, whose http.extraHeader authenticates ls-remote with
// no helper, an askpass answers git's prompt with no helper, and GIT_CONFIG_PARAMETERS adds a helper
// at command scope. gh reads GH_TOKEN and GITHUB_TOKEN; git ignores them, but they must not reach it.
func (r *setupRig) poisons() map[string]map[string]string {
	r.t.Helper()
	forged := filepath.Join(r.root, "forged-config")
	if err := os.WriteFile(forged, []byte("[credential \"https://github.com\"]\n\thelper =\n\thelper = "+workspace.GitHubCredentialHelper+"\n"), 0o600); err != nil {
		r.t.Fatal(err)
	}
	headers := filepath.Join(r.root, "headers")
	r.git(fixturegit.InitWorkTree(headers, "")[1:]...)
	r.git("-C", headers, "config", "http.https://github.com/.extraheader", "Authorization: basic Zm9yZ2Vk")
	askpass := filepath.Join(r.root, "askpass")
	if err := testexec.WriteFile(askpass, []byte("#!/bin/sh\necho forged\n"), 0o700); err != nil {
		r.t.Fatal(err)
	}
	poisons := map[string]map[string]string{
		"GIT_CONFIG":            {"GIT_CONFIG": forged},
		"GIT_DIR":               {"GIT_DIR": filepath.Join(headers, ".git")},
		"GIT_COMMON_DIR":        {"GIT_COMMON_DIR": filepath.Join(headers, ".git")},
		"an askpass":            {"GIT_ASKPASS": askpass, "SSH_ASKPASS": askpass},
		"GIT_CONFIG_PARAMETERS": {"GIT_CONFIG_PARAMETERS": "'credential.https://github.com.helper'='" + workspace.GitHubCredentialHelper + "'"},
		"gh tokens":             {"GH_TOKEN": "forged", "GITHUB_TOKEN": "forged"},
	}
	all := map[string]string{}
	for _, poison := range poisons {
		maps.Copy(all, poison)
	}
	poisons["all of them"] = all
	return poisons
}

// configuredClone is a repository whose credential configuration the daemon wrote, as the issue
// clone's is.
func (r *setupRig) configuredClone() string {
	r.t.Helper()
	clone := r.plainClone()
	if _, err := workspace.ConfigureRepositoryCredential(context.Background(), workspace.NewRunner(time.Minute, map[string]string{"git": r.realGit}), clone); err != nil {
		r.t.Fatalf("configure repository credential: %v", err)
	}
	return clone
}

func (r *setupRig) plainClone() string {
	r.t.Helper()
	clone := filepath.Join(r.root, "clone")
	r.git(fixturegit.InitWorkTree(clone, "")[1:]...)
	return clone
}

func (r *setupRig) git(args ...string) {
	r.t.Helper()
	// Every repository the rig makes or configures lies under its root, which the commands that
	// create one need as their parent, so the helper runs there rather than in the directory the
	// test process happens to have.
	//
	// The command carries an environment of its own because a test may plant a git variable with
	// t.Setenv long after this package's TestMain guard has run:
	// TestLiveSetupIgnoresTheRoleEnvironmentGitVariables plants the maps setupRig.poisons returns,
	// GIT_DIR and GIT_COMMON_DIR naming the repository the rig forges among them. GIT_DIR outranks
	// the directory a command runs in, so a command with no Env of its own would write into that
	// repository rather than into the rig's own (TestFixtureCommandsIgnoreAPlantedRepository). The
	// home is the rig's, so every fixture of one rig reads one git and jj configuration, and the
	// environment is built for each call, so a variable the test plants after the rig was made is
	// still left out.
	output, err := fixturegit.Isolated(r.t, r.home).Try(r.root, append([]string{r.realGit}, args...)...)
	if err != nil {
		r.t.Fatalf("git %q: %v: %s", args, err, output)
	}
}

// run runs the start wrapper with liveSetup and agent from start, and returns the wrapper's stderr
// and the marker's lines, with the marker and the audit directory emptied first.
func (r *setupRig) run(start string, agent []string) (string, []string) {
	r.t.Helper()
	if err := os.RemoveAll(r.audit); err != nil {
		r.t.Fatal(err)
	}
	if err := os.Mkdir(r.audit, 0o700); err != nil {
		r.t.Fatal(err)
	}
	if err := os.Remove(r.marker); err != nil && !os.IsNotExist(err) {
		r.t.Fatal(err)
	}
	got := runStart(r.t, start, r.bin, "", "", nil, liveSetup(setupRigRepository(r.t)), agent)
	if got.exit != 0 {
		r.t.Fatalf("the wrapper exited %d: %s", got.exit, got.stderr)
	}
	return got.stderr, readLines(r.t, r.marker)
}

// probe runs the setup alone from start and returns its one record.
func (r *setupRig) probe(start string) setupMarker {
	r.t.Helper()
	_, lines := r.run(start, []string{"/bin/true"})
	if len(lines) != 1 {
		r.t.Fatalf("marker lines = %q, want its setup line alone", lines)
	}
	return parseSetupMarkerOrFail(r.t, lines[0])
}

// environments is every environment the fake git recorded in the last run, by call.
func (r *setupRig) environments() map[string][]string {
	r.t.Helper()
	entries, err := os.ReadDir(r.audit)
	if err != nil {
		r.t.Fatal(err)
	}
	env := map[string][]string{}
	for _, entry := range entries {
		if call, ok := strings.CutSuffix(entry.Name(), "-env"); ok {
			env[call] = r.audited(entry.Name())
		}
	}
	return env
}

// audited is the lines the fake git recorded under name in the last run.
func (r *setupRig) audited(name string) []string {
	r.t.Helper()
	body, err := os.ReadFile(filepath.Join(r.audit, name))
	if err != nil {
		r.t.Fatalf("the fake git recorded no %s: %v", name, err)
	}
	return strings.Split(strings.TrimRight(string(body), "\n"), "\n")
}

// wantEnvironment is the whole environment, HOME aside, that liveSetupScript's allowlist gives
// every git under roleEnv: gitConfigPairs as command-scope entries, with each helper key's
// value emptied for the negative control, the role's PATH (runStart's) and owners directory, the
// four fixed variables, and, for an ls-remote call, GIT_CEILING_DIRECTORIES at ceiling.
func (r *setupRig) wantEnvironment(withoutHelper bool, ceiling string) []string {
	env := []string{
		"PATH=" + r.bin + string(os.PathListSeparator) + os.Getenv("PATH"),
		"LEGION_GH_OWNERS_DIR=" + r.owners,
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0", "LC_ALL=C",
		"GIT_CONFIG_COUNT=" + strconv.Itoa(len(gitConfigPairs)),
	}
	if ceiling != "" {
		env = append(env, "GIT_CEILING_DIRECTORIES="+ceiling)
	}
	for i, pair := range gitConfigPairs {
		value := pair[1]
		if withoutHelper && strings.HasSuffix(strings.ToLower(pair[0]), "helper") {
			value = ""
		}
		env = append(env, "GIT_CONFIG_KEY_"+strconv.Itoa(i)+"="+pair[0], "GIT_CONFIG_VALUE_"+strconv.Itoa(i)+"="+value)
	}
	slices.Sort(env)
	return env
}

// setupOutcome is what a record says about git, every field but the run's own directory and pid.
func setupOutcome(m setupMarker) string {
	return fmt.Sprintf("helper=%s helper_count=%s helper_effective=%s helper_lines=%s git=%s git_without_helper=%s",
		m.helper, m.helperCount, m.helperEffective, m.helperLines, m.git, m.gitWithoutHelper)
}

func setupRigRepository(t *testing.T) ghrepo.Repository {
	t.Helper()
	repo, err := ghrepo.Parse("repo", "sjawhar/legion-smoke")
	if err != nil {
		t.Fatal(err)
	}
	return repo
}

func parseSetupMarkerOrFail(t *testing.T, line string) setupMarker {
	t.Helper()
	setup, err := parseSetupMarker(line)
	if err != nil {
		t.Fatal(err)
	}
	return setup
}

// helperLog runs the real git's `credential fill` for https://github.com/acme/widgets.git in a
// clean environment, with fake helpers that record only their own name when git asks them.
type helperLog struct {
	t                 *testing.T
	dir, ran, realGit string
}

func newHelperLog(t *testing.T) *helperLog {
	t.Helper()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	return &helperLog{t: t, dir: dir, ran: filepath.Join(dir, "ran"), realGit: realGit}
}

func (h *helperLog) helper(name string) string {
	h.t.Helper()
	path := filepath.Join(h.dir, "helper-"+name)
	if err := testexec.WriteFile(path, []byte("#!/bin/sh\n[ \"$1\" = get ] && echo "+name+" >>"+h.ran+"\nexit 0\n"), 0o700); err != nil {
		h.t.Fatal(err)
	}
	return path
}

func (h *helperLog) repo(name string, keys ...[2]string) string {
	h.t.Helper()
	path := filepath.Join(h.dir, name)
	// Every command here runs under the log's own home, so none of them reads a repository,
	// configuration or credential of the process that runs the tests.
	git := fixturegit.Isolated(h.t, h.dir)
	// The init creates path, so it runs in the log's own directory, which holds it; the configure
	// below runs in the repository it names with -C, which the init has made by then.
	initArgv := append([]string{h.realGit}, fixturegit.InitWorkTree(path, "")[1:]...)
	if output, err := git.Try(h.dir, initArgv...); err != nil {
		h.t.Fatalf("init %s: %v: %s", name, err, output)
	}
	for _, kv := range keys {
		if output, err := git.Try(path, h.realGit, "-C", path, "config", "--add", kv[0], kv[1]); err != nil {
			h.t.Fatalf("config %s: %v: %s", kv[0], err, output)
		}
	}
	return path
}

// fill returns the helpers git ran, in order, for one request from dir under extra.
func (h *helperLog) fill(dir string, extra ...string) []string {
	h.t.Helper()
	if err := os.WriteFile(h.ran, nil, 0o600); err != nil {
		h.t.Fatal(err)
	}
	cmd := exec.Command(h.realGit, "credential", "fill")
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader("protocol=https\nhost=github.com\npath=acme/widgets.git\n\n")
	cmd.Env = append([]string{"PATH=" + os.Getenv("PATH"), "HOME=" + h.dir, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0"}, extra...)
	_ = cmd.Run()
	body, err := os.ReadFile(h.ran)
	if err != nil {
		h.t.Fatal(err)
	}
	return strings.Fields(string(body))
}

// commandConfig is pairs as GIT_CONFIG_COUNT, GIT_CONFIG_KEY_<n> and GIT_CONFIG_VALUE_<n>.
func commandConfig(pairs ...[2]string) []string {
	env := []string{"GIT_CONFIG_COUNT=" + strconv.Itoa(len(pairs))}
	for i, pair := range pairs {
		env = append(env, "GIT_CONFIG_KEY_"+strconv.Itoa(i)+"="+pair[0], "GIT_CONFIG_VALUE_"+strconv.Itoa(i)+"="+pair[1])
	}
	return env
}
