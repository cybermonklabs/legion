package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/sjawhar/envoy/tlstimeout/tlstimeouttest"

	"github.com/sjawhar/legion/daemon/internal/execfile"
	"github.com/sjawhar/legion/daemon/internal/ghroute/ghroutetest"
	"github.com/sjawhar/legion/daemon/internal/leakguard"
	"github.com/sjawhar/legion/daemon/internal/leakguard/leakguardtest"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// tlsRecordLine is the start of the record line `legion gh` writes for a TLS handshake timeout to
// host (tlstimeout.Recorder.RecordContext), naming event as the line does: handshake_timeout for
// Go's `net/http: TLS handshake timeout`, guard_bound for the leak guard's own bound stopping its
// gh read before GitHub sent any answer.
func tlsRecordLine(host, event string) string {
	return "legion-gh: tls handshake timeout recorded: host=" + host + " event=" + event + " "
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
				if len(lines) != 2 || !strings.HasPrefix(lines[1], tlsRecordLine(silent, "handshake_timeout")) || !strings.Contains(lines[1], " child_user_s=") {
					t.Fatalf("legion gh wrote %q, want gh's line and then one record line naming %q and gh's CPU", stderr, tlsRecordLine(silent, "handshake_timeout"))
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
	if lines := strings.Split(strings.TrimSuffix(stderr, "\n"), "\n"); len(lines) != 1 || !strings.HasPrefix(lines[0], tlsRecordLine("api.github.com", "handshake_timeout")) || strings.Contains(lines[0], "child_user_s") {
		t.Errorf("legion gh wrote %q, want one record line naming %q and no child CPU", stderr, tlsRecordLine("api.github.com", "handshake_timeout"))
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
	if !strings.Contains(refusal, notFoundSays("the push to origin")) || !strings.HasPrefix("legion-gh: "+record, tlsRecordLine("api.github.com", "handshake_timeout")) || strings.Contains(record, "\n") {
		t.Errorf("the hook printed %q, want the not-found refusal and then one record line naming %q", printed, tlsRecordLine("api.github.com", "handshake_timeout"))
	}
	if _, err := os.Stat(probe); err != nil {
		t.Errorf("the record ran no curl probe: %v", err)
	}
}

// legionWithTheProductsBounds runs this test binary as `legion args…` as legionIn does, with none
// of this binary's deadline (testDeadlineEnv), so the run keeps the product's own bounds
// (boundByTheEnvironment): a row that reads one such bound's own stop, the leak guard's
// ghReadBound of 8 s, would otherwise have to outlast what is left of this binary's deadline,
// which the bound would then be. Every other wait of the run has its product bound too, which a
// stand-in answers well within.
func legionWithTheProductsBounds(t *testing.T, env map[string]string, stdin string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	cmd := legionCommand(t, t.TempDir(), env, args...)
	cmd.Env = slices.DeleteFunc(cmd.Env, func(entry string) bool { return strings.HasPrefix(entry, testDeadlineEnv+"=") })
	return runLegionCommand(t, cmd, stdin)
}

// R11 of dispatch://INFRA-3562, the leak guard's own bound: where the real gh of the guard's REST
// visibility read prints nothing and sleeps past ghReadBound, as the real gh does while its TLS
// handshake stalls (it prints `net/http: TLS handshake timeout` only at net/http's 10 s), the guard
// stops it at the bound, reads the visibility through curl as before, and the command records the
// stall for api.github.com once the call is over, as the last line it prints, naming the guard's
// bound as its event. `legion gh`'s guarded write is cleared by curl's public and exits gh's 0,
// its record the one line of standard error, with no child CPU since the gh stopped was the
// guard's; `legion leak-check pre-push` is refused for curl's 404, exit 2 with the not-found
// words, and then records. Each runs with the product's bounds (legionWithTheProductsBounds), so
// the guard's bound is its 8 s. The record's Go probe goes through the proxy HTTPS_PROXY names, a
// peer that never answers, and its curl probe asks https://api.github.com/.
func TestTheGuardsOwnBoundOnItsGhReadIsRecordedAsItsOwnTLSEvent(t *testing.T) {
	t.Parallel()
	root, defaultDir := gitCredentialBundle(t)
	help, err := filepath.Abs(ghHelpDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		// args and stdin are the row's command; gh is its stand-in real gh, whose REST visibility
		// read prints nothing and sleeps past the guard's bound; answer is the stand-in curl's
		// answer to the guard's read through curl; code is the status the command exits with, and
		// before what it prints ahead of the record line.
		args   []string
		stdin  string
		gh     func(t *testing.T) string
		answer string
		code   int
		before string
	}{
		{
			name: "legion gh", args: []string{"gh", "pr", "create", "-R", "acme/widgets", "--title", "a title the rules pass"}, answer: curlReadsPublic,
			gh: func(t *testing.T) string {
				stub := filepath.Join(t.TempDir(), "real-gh")
				script := ghStandInHelp(help) + "case \" $* \" in\n" + ghVisibilityReadArgv + ")\n  exec sleep 30;;\nesac\nexit 0\n"
				if err := execfile.Write(stub, []byte(script), 0o700); err != nil {
					t.Fatal(err)
				}
				return stub
			},
		},
		{
			name: "legion leak-check pre-push", args: []string{"leak-check", "pre-push", "origin", "https://github.com/" + noSuchRepo + ".git"}, stdin: refusedRefDeletion,
			answer: "printf 'HTTP/2 404\\r\\ncontent-type: application/json; charset=utf-8\\r\\n\\r\\n{\"message\":\"Not Found\"}'\nexit 0\n",
			code:   leakguard.StatusSetupFailure, before: notFoundSays("the push to origin"),
			gh: func(t *testing.T) string {
				stub, _ := visibilityReadGh(t, "exec sleep 30\n")
				return stub
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			curl, probe := tlsCurlStandIn(t, tc.answer)
			code, _, printed := legionWithTheProductsBounds(t, tlsRecordEnv(t, map[string]string{
				"GH_CONFIG_DIR": defaultDir, "LEGION_GH_OWNERS_DIR": root, "LEGION_GH_PATH": tc.gh(t), "XDG_STATE_HOME": t.TempDir(),
				"PUBLIC_LEAK_CHECK":    leakguardtest.Write(t, t.TempDir(), leakguardtest.Passes),
				leakguard.RulesFileVar: leakguardtest.WriteRules(t, t.TempDir()),
				testCurlEnv:            curl,
				"HTTPS_PROXY":          "http://" + tlstimeouttest.Silent(t),
			}), tc.stdin, tc.args...)
			if code != tc.code {
				t.Errorf("legion %s exited %d, want %d: %s", tc.args[0], code, tc.code, printed)
			}
			lines := strings.Split(strings.TrimSuffix(printed, "\n"), "\n")
			record, earlier := lines[len(lines)-1], strings.Join(lines[:len(lines)-1], "\n")
			want := tlsRecordLine("api.github.com", "guard_bound")
			if !strings.HasPrefix(record, want) || strings.Contains(record, "child_user_s") || strings.Count(printed, "tls handshake timeout recorded") != 1 ||
				!strings.Contains(earlier, tc.before) || (tc.before == "" && earlier != "") {
				t.Errorf("legion %s printed %q; want %q and then one record line naming %q and no child CPU", tc.args[0], printed, tc.before, want)
			}
			asked, err := os.ReadFile(probe)
			if err != nil {
				t.Fatalf("the record ran no curl probe: %v", err)
			}
			if argv := strings.Split(strings.TrimSuffix(string(asked), "\n"), "\n"); argv[len(argv)-1] != "https://api.github.com/" {
				t.Errorf("the record's curl ran as %q, want https://api.github.com/ last", argv)
			}
		})
	}
}

// A SIGTERM that reaches `legion gh` or `legion leak-check pre-push` while it records a TLS
// handshake timeout ends the record's probes, and the command exits 143, the status of a death by
// that signal (endedBySignal), rather than with the status of the call it recorded: the record
// runs once that call is over, and its probes would otherwise run on to their bound. Each row's
// record is held in its curl probe, a stand-in that becomes a sleep of about a day of its own
// (ghroutetest.HoldsStandIn), under a bound of two days (testTLSBoundEnv), while its Go probe
// waits on a peer that never answers TLS, so only the signal ends the record before the row gives
// up. The signal goes to the command's own pid, as a stop sends it. The record still writes its
// line, the last the command prints, after the call's own output, and its curl is gone once the
// command has exited. The rows are the three places a record is made: after the real gh's own
// timeout (runGhChild), and after the leak guard's own gh read met one, once a `legion gh` call
// is over and once a pre-push has been refused (recordGuardTLSTimeout).
func TestASignalDuringATLSRecordEndsTheCommandWithTheSignalsStatus(t *testing.T) {
	t.Parallel()
	root, defaultDir := gitCredentialBundle(t)
	help, err := filepath.Abs(ghHelpDir)
	if err != nil {
		t.Fatal(err)
	}
	silent := tlstimeouttest.Silent(t)
	standIn := func(t *testing.T, script string) string {
		t.Helper()
		stub := filepath.Join(t.TempDir(), "real-gh")
		if err := execfile.Write(stub, []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
		return stub
	}
	for _, tc := range []struct {
		name string
		// args and stdin are the row's command; gh is its stand-in real gh, answer the stand-in
		// curl's answer to the guard's read through curl, host the host the record names, and
		// before what the command prints ahead of the record line. seconds is the sleep the
		// record's curl probe becomes.
		args                 []string
		stdin                string
		gh                   func(t *testing.T) string
		answer, host, before string
		seconds              string
	}{
		{
			name: "the real gh's own timeout", args: []string{"gh", "api", "rate_limit"}, host: silent, seconds: "86387",
			gh: func(t *testing.T) string {
				return standIn(t, ghStandInPrefix(help)+"echo 'Get \"https://"+silent+"/rate_limit\": net/http: TLS handshake timeout' >&2\nexit 1\n")
			},
			before: "net/http: TLS handshake timeout",
		},
		{
			name: "the guard's gh read's timeout, once the call is over", args: []string{"gh", "pr", "create", "-R", "acme/widgets", "--title", "a title the rules pass"},
			host: "api.github.com", seconds: "86386", answer: curlReadsPublic,
			gh: func(t *testing.T) string {
				return standIn(t, ghStandInHelp(help)+"case \" $* \" in\n"+ghVisibilityReadArgv+")\n"+
					"  echo 'Get \"https://api.github.com/repos/acme/widgets\": net/http: TLS handshake timeout' >&2\n  exit 1;;\nesac\nexit 0\n")
			},
		},
		{
			name: "the guard's gh read's timeout, once the pre-push is refused", args: []string{"leak-check", "pre-push", "origin", "https://github.com/" + noSuchRepo + ".git"},
			stdin: refusedRefDeletion, host: "api.github.com", seconds: "86385", before: notFoundSays("the push to origin"),
			answer: "printf 'HTTP/2 404\\r\\ncontent-type: application/json; charset=utf-8\\r\\n\\r\\n{\"message\":\"Not Found\"}'\nexit 0\n",
			gh: func(t *testing.T) string {
				stub, _ := visibilityReadGh(t, ghCannotReachGitHub)
				return stub
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			pidFile := filepath.Join(t.TempDir(), "curl.pid")
			ghroutetest.KillRecordedHelper(t, pidFile, tc.seconds)
			// The record's curl probe is the run that asks for its timings (-w); every other run is
			// the guard's read through curl, which reads the token on its standard input first.
			curl := filepath.Join(t.TempDir(), "curl")
			script := "#!/bin/sh\ncase \" $* \" in\n*\" -w \"*)\n" + ghroutetest.HoldsStandIn(pidFile, tc.seconds) + ";;\nesac\ncat >/dev/null\n" + tc.answer
			if err := execfile.Write(curl, []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			command := legionCommand(t, t.TempDir(), map[string]string{
				"GH_CONFIG_DIR": defaultDir, "LEGION_GH_OWNERS_DIR": root, "LEGION_GH_PATH": tc.gh(t), "XDG_STATE_HOME": t.TempDir(),
				"PUBLIC_LEAK_CHECK":    leakguardtest.Write(t, t.TempDir(), leakguardtest.Passes),
				leakguard.RulesFileVar: leakguardtest.WriteRules(t, t.TempDir()),
				testCurlEnv:            curl,
				"HTTPS_PROXY":          "http://" + silent,
				testTLSStampsEnv:       filepath.Join(t.TempDir(), "stamps"),
				testTLSBoundEnv:        "48h",
			}, tc.args...)
			command.Stdin = strings.NewReader(tc.stdin)
			// Standard output and standard error are a file rather than a pipe, so nothing of the
			// stand-ins' can hold the wait open.
			printed, err := os.Create(filepath.Join(t.TempDir(), "printed"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { printed.Close() })
			command.Stdout, command.Stderr = printed, printed
			if err := command.Start(); err != nil {
				t.Fatal(err)
			}
			exited := make(chan struct{})
			var waited error
			go func() {
				waited = command.Wait()
				close(exited)
			}()
			t.Cleanup(func() {
				select {
				case <-exited:
				default:
					_ = command.Process.Kill()
					<-exited
					t.Logf("legion %s still ran when the test ended, having printed: %s", tc.args[0], readRecord(t, printed.Name()))
				}
			})
			var pid int
			testwait.Eventually(t, "the record's curl probe to hold the record, or the command to end", func() bool {
				select {
				case <-exited:
					return true
				default:
				}
				var running bool
				pid, running = ghroutetest.RecordedHelper(pidFile, tc.seconds)
				return running
			})
			select {
			case <-exited:
				t.Fatalf("legion %s = %v before its record's curl held the record: %s", tc.args[0], waited, readRecord(t, printed.Name()))
			default:
			}
			// The signal must come while the curl holds the record: with no running pid recorded,
			// the check of its end below would pass a signal that came before the record.
			if pid <= 0 || !ghroutetest.HelperRunning(pid, tc.seconds) {
				t.Fatalf("the record's curl had recorded no running pid (%d) in %s when the signal was to be sent", pid, pidFile)
			}
			if err := command.Process.Signal(syscall.SIGTERM); err != nil {
				t.Fatal(err)
			}
			testwait.Receive(t, "legion "+tc.args[0]+" to end after a SIGTERM during its record", exited)
			out := readRecord(t, printed.Name())
			if exit, ok := waited.(*exec.ExitError); !ok || exit.ExitCode() != 143 {
				t.Errorf("legion %s after a SIGTERM during its record = %v; want exit 143: %s", tc.args[0], waited, out)
			}
			lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
			if last := lines[len(lines)-1]; !strings.HasPrefix(last, tlsRecordLine(tc.host, "handshake_timeout")) || !strings.Contains(strings.Join(lines[:len(lines)-1], "\n"), tc.before) {
				t.Errorf("legion %s printed %q; want %q and then the record line, naming %q", tc.args[0], out, tc.before, tlsRecordLine(tc.host, "handshake_timeout"))
			}
			if ghroutetest.HelperRunning(pid, tc.seconds) {
				t.Errorf("the record's curl (pid %d) still runs after legion %s exited; want it stopped with the record", pid, tc.args[0])
			}
		})
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
