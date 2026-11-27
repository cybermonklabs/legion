package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/tlstimeout/tlstimeouttest"

	"github.com/sjawhar/legion/daemon/internal/execfile"
	"github.com/sjawhar/legion/daemon/internal/leakguard"
	"github.com/sjawhar/legion/daemon/internal/leakguard/leakguardtest"
)

// tlsRecordLine is the start of the record line `legion gh` writes for a TLS handshake timeout to
// host (tlstimeout.Recorder.Record).
func tlsRecordLine(host string) string {
	return "legion-gh: tls handshake timeout recorded: host=" + host + " "
}

// tlsRecordEnv is what a `legion` of these tests is handed to record a TLS handshake timeout: its
// stamps in a directory of the test's own and a bound of a second (TestMain). A run handed
// neither keeps no stamp, and so records nothing.
func tlsRecordEnv(t *testing.T, env map[string]string) map[string]string {
	t.Helper()
	env[testTLSStampsEnv] = filepath.Join(t.TempDir(), "stamps")
	env[testTLSBoundEnv] = "1s"
	return env
}

// tlsCurlStandIn is a stand-in curl (testCurlEnv) for both of the curls a `legion` of these tests
// runs: the record's probe, which asks for its timings (-w), it answers as a curl whose handshake
// did not finish, writing its arguments, one a line, to probe and its environment beside it; every
// other run is the leak guard's read through curl, which it answers with answer after it reads the
// token the guard hands it on its standard input.
func tlsCurlStandIn(t *testing.T, answer string) (program, probe string) {
	t.Helper()
	dir := t.TempDir()
	program, probe = filepath.Join(dir, "curl"), filepath.Join(dir, "probe")
	script := "#!/bin/sh\ncase \" $* \" in\n*\" -w \"*)\n  printf '%s\\n' \"$@\" > '" + probe + "'\n  env > '" + probe + ".env'\n" +
		"  printf '0.001 0.002 0.000 1.000'\n  exit 28;;\nesac\ncat >/dev/null\n" + answer
	if err := execfile.Write(program, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return program, probe
}

// curlReadsPublic is the leak guard's read through curl answered by GitHub with public.
const curlReadsPublic = "printf '%s\\r\\n' 'HTTP/2 200' ''\nprintf '%s\\n' '{\"visibility\":\"public\"}'\nexit 0\n"

// R11 of dispatch://INFRA-3562, `legion gh` around the real gh: a call whose output is the
// caller's, a read the shared check alone clears, a write the guard reads whole and the exact
// rate-limit read, whose gh fails with `Get "https://<host>/graphql": net/http: TLS handshake
// timeout` at a peer that never answers TLS, exits gh's 1 with gh's line on standard error and,
// after it, one record line naming legion-gh and that host and port, nothing of the path, and
// gh's own CPU (child_user_s). A gh that exits 1 with GitHub's 500 and one that succeeds write no
// record, and neither does the guard's own visibility read, which the guard reads gh's line of:
// its standard error is that line alone.
func TestLegionGhRecordsATLSHandshakeTimeoutOfTheRealGh(t *testing.T) {
	t.Parallel()
	root, defaultDir := gitCredentialBundle(t)
	help, err := filepath.Abs(ghHelpDir)
	if err != nil {
		t.Fatal(err)
	}
	silent := tlstimeouttest.Silent(t)
	timedOut := `Get "https://` + silent + `/graphql": net/http: TLS handshake timeout`
	serverError := "HTTP 500: Internal Server Error (https://" + silent + "/graphql)"
	readCall := []string{"api", "graphql", "-f", "query={viewer{login}}", "--jq", ".data.viewer.login"}
	writeCall := []string{"pr", "create", "-R", "acme/widgets", "--title", "a title the rules pass"}
	for _, tc := range []struct {
		name string
		args []string
		// then is what the stand-in real gh does for the call, after it answered gh's help and
		// the guard's visibility reads (ghStandInPrefix); visibilityRead is a stand-in of its own
		// for a row whose call is one of those reads.
		then           string
		visibilityRead bool
		code           int
		// printed is what gh printed on its standard error, which `legion gh` passes on whole;
		// records is whether a record line follows it.
		printed string
		records bool
	}{
		{name: "a read whose gh cannot finish its handshake", args: readCall, then: "echo '" + timedOut + "' >&2\nexit 1\n", code: 1, printed: timedOut, records: true},
		{name: "a write whose gh cannot finish its handshake", args: writeCall, then: "echo '" + timedOut + "' >&2\nexit 1\n", code: 1, printed: timedOut, records: true},
		{name: "the rate-limit read whose gh cannot finish its handshake", args: []string{"api", "rate_limit"}, then: "echo '" + timedOut + "' >&2\nexit 1\n", code: 1, printed: timedOut, records: true},
		{name: "a read GitHub answers 500", args: readCall, then: "echo '" + serverError + "' >&2\nexit 1\n", code: 1, printed: serverError},
		{name: "a read that succeeds", args: readCall, then: "echo legion-implementer\nexit 0\n"},
		{
			name: "the guard's own visibility read", args: leakguard.VisibilityReadWithHeadersArgs("acme/widgets"), visibilityRead: true,
			then: "echo 'Get \"https://" + silent + "/repos/acme/widgets\": net/http: TLS handshake timeout' >&2\nexit 1\n",
			code: 1, printed: `Get "https://` + silent + `/repos/acme/widgets": net/http: TLS handshake timeout`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			stub := filepath.Join(t.TempDir(), "real-gh")
			script := ghStandInPrefix(help) + tc.then
			if tc.visibilityRead {
				script = ghStandInHelp(help) + tc.then
			}
			if err := execfile.Write(stub, []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			env := tlsRecordEnv(t, map[string]string{
				"GH_CONFIG_DIR":        defaultDir,
				"LEGION_GH_OWNERS_DIR": root,
				"LEGION_GH_PATH":       stub,
				"PUBLIC_LEAK_CHECK":    ghCheckAt(t, ghCheckPasses),
				testProvenCheckEnv:     ghProvenProgramSHA256,
				leakguard.RulesFileVar: leakguardtest.WriteRules(t, t.TempDir()),
			})
			if tc.visibilityRead {
				env[leakguard.VisibilityReadVar] = "acme/widgets"
			}
			code, _, stderr := legionGh(t, env, tc.args...)
			if code != tc.code {
				t.Fatalf("legion gh %q = exit %d, stderr %q; want gh's own %d", tc.args, code, stderr, tc.code)
			}
			lines := slices.DeleteFunc(strings.Split(stderr, "\n"), func(line string) bool { return line == "" })
			var want []string
			if tc.printed != "" {
				want = append(want, tc.printed)
			}
			if tc.records {
				if len(lines) != 2 || !strings.HasPrefix(lines[1], tlsRecordLine(silent)) || !strings.Contains(lines[1], " child_user_s=") {
					t.Fatalf("legion gh wrote %q, want gh's line and then one record line naming %q and gh's CPU", stderr, tlsRecordLine(silent))
				}
				if strings.Contains(lines[1], "/graphql") {
					t.Errorf("the record line names the call's path: %q", lines[1])
				}
				lines = lines[:1]
			}
			if !slices.Equal(lines, want) {
				t.Errorf("legion gh wrote %q on standard error, want gh's own %q and no record", stderr, want)
			}
		})
	}
}

// R11 of dispatch://INFRA-3562, the leak guard's own gh read: where the guard's REST visibility
// read of a write's destination fails with gh's `net/http: TLS handshake timeout`, the guard reads
// the visibility through curl as it always has and the call goes on, and `legion gh` records the
// timeout for api.github.com once the call is over: gh's status, 0, is the call's, and the record
// line, with no child CPU since the gh that failed was the guard's, is the last of standard error.
// Its Go probe goes to api.github.com through the proxy HTTPS_PROXY names, a peer of the test's
// that never answers, so it reaches no network; its curl probe asks https://api.github.com/ with
// none of the call's credentials.
func TestLegionGhRecordsTheGuardsGhTLSHandshakeTimeoutWhenTheCallIsOver(t *testing.T) {
	t.Parallel()
	root, defaultDir := gitCredentialBundle(t)
	help, err := filepath.Abs(ghHelpDir)
	if err != nil {
		t.Fatal(err)
	}
	stub := filepath.Join(t.TempDir(), "real-gh")
	script := ghStandInHelp(help) + "case \" $* \" in\n" + ghVisibilityReadArgv + ")\n" +
		"  echo 'Get \"https://api.github.com/repos/acme/widgets\": net/http: TLS handshake timeout' >&2\n  exit 1;;\nesac\nexit 0\n"
	if err := execfile.Write(stub, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	curl, probe := tlsCurlStandIn(t, curlReadsPublic)
	args := []string{"pr", "create", "-R", "acme/widgets", "--title", "a title the rules pass"}
	code, _, stderr := legionGh(t, tlsRecordEnv(t, map[string]string{
		"GH_CONFIG_DIR":        defaultDir,
		"LEGION_GH_OWNERS_DIR": root,
		"LEGION_GH_PATH":       stub,
		"PUBLIC_LEAK_CHECK":    leakguardtest.Write(t, t.TempDir(), leakguardtest.Passes),
		leakguard.RulesFileVar: leakguardtest.WriteRules(t, t.TempDir()),
		testCurlEnv:            curl,
		"HTTPS_PROXY":          "http://" + tlstimeouttest.Silent(t),
	}), args...)
	if code != 0 {
		t.Fatalf("legion gh %q = exit %d, stderr %q; want gh's own 0", args, code, stderr)
	}
	if lines := strings.Split(strings.TrimSuffix(stderr, "\n"), "\n"); len(lines) != 1 || !strings.HasPrefix(lines[0], tlsRecordLine("api.github.com")) || strings.Contains(lines[0], "child_user_s") {
		t.Errorf("legion gh wrote %q, want one record line naming %q and no child CPU", stderr, tlsRecordLine("api.github.com"))
	}
	asked, err := os.ReadFile(probe)
	if err != nil {
		t.Fatalf("the record ran no curl probe: %v", err)
	}
	if argv := strings.Split(strings.TrimSuffix(string(asked), "\n"), "\n"); argv[0] != "-q" || argv[len(argv)-1] != "https://api.github.com/" {
		t.Errorf("the record's curl ran as %q, want -q first and https://api.github.com/ last", argv)
	}
	environ, err := os.ReadFile(probe + ".env")
	if err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{"ghs_acme", "GH_TOKEN", "GITHUB_TOKEN", "GH_CONFIG_DIR"} {
		if strings.Contains(string(environ), token) {
			t.Errorf("the record's curl was handed %q:\n%s", token, environ)
		}
	}
}

// R11 of dispatch://INFRA-3562, `legion leak-check pre-push`: where the guard's REST visibility read
// of the push's destination fails with gh's `net/http: TLS handshake timeout` and curl's read is
// answered 404, the push is refused as it is without the record, exit 2 with the not-found words,
// and the timeout is recorded for api.github.com after the refusal, the last line the hook prints.
// Its Go probe goes through the proxy HTTPS_PROXY names, a peer that never answers.
func TestLeakCheckPrePushRecordsTheGuardsGhTLSHandshakeTimeout(t *testing.T) {
	t.Parallel()
	root, defaultDir := gitCredentialBundle(t)
	stub, _ := visibilityReadGh(t, ghCannotReachGitHub)
	curl, probe := tlsCurlStandIn(t, "printf 'HTTP/2 404\\r\\ncontent-type: application/json; charset=utf-8\\r\\n\\r\\n{\"message\":\"Not Found\"}'\nexit 0\n")
	code, _, printed := legionIn(t, t.TempDir(), tlsRecordEnv(t, map[string]string{
		"LEGION_GH_PATH": stub, "GH_CONFIG_DIR": defaultDir, "LEGION_GH_OWNERS_DIR": root, "XDG_STATE_HOME": t.TempDir(),
		"PUBLIC_LEAK_CHECK":    leakguardtest.Write(t, t.TempDir(), leakguardtest.Passes),
		leakguard.RulesFileVar: leakguardtest.WriteRules(t, t.TempDir()),
		testCurlEnv:            curl,
		"HTTPS_PROXY":          "http://" + tlstimeouttest.Silent(t),
	}), refusedRefDeletion, "leak-check", "pre-push", "origin", "https://github.com/"+noSuchRepo+".git")
	if code != leakguard.StatusSetupFailure {
		t.Errorf("exited %d, want %d: %s", code, leakguard.StatusSetupFailure, printed)
	}
	refusal, record, _ := strings.Cut(strings.TrimSuffix(printed, "\n"), "\nlegion-gh: ")
	if !strings.Contains(refusal, notFoundSays("the push to origin")) || !strings.HasPrefix("legion-gh: "+record, tlsRecordLine("api.github.com")) || strings.Contains(record, "\n") {
		t.Errorf("the hook printed %q, want the not-found refusal and then one record line naming %q", printed, tlsRecordLine("api.github.com"))
	}
	if _, err := os.Stat(probe); err != nil {
		t.Errorf("the record ran no curl probe: %v", err)
	}
}

// The real gh is given none of the variables these tests hand `legion` for its record of a TLS
// handshake timeout: the stamps directory and the bound (TestMain), which only this test binary
// reads, and which gh's allowlist (ghChildKeeps) names not.
func TestGhsChildEnvironmentGivesGhNoneOfTheTLSRecordsTestVariables(t *testing.T) {
	t.Parallel()
	for _, name := range []string{testTLSStampsEnv, testTLSBoundEnv} {
		caller := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(), name + "=/an/invented/value"}
		for _, entry := range ghChildEnviron(caller, nil) {
			if strings.HasPrefix(entry, name+"=") {
				t.Errorf("gh's environment holds %q, want no %s", entry, name)
			}
		}
	}
}
