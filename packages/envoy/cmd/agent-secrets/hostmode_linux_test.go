// packages/envoy/cmd/agent-secrets/hostmode_linux_test.go
//go:build linux

// fakeHelperWithPids and TestRegisterExecStillPinsTheReplacedProcess need helper.PeerOf
// (SO_PEERPIDFD), which is Linux-only (internal/broker/helper/peer.go carries the same
// //go:build linux), and the register tests below check the root a helper answers against this
// process's /proc ancestry; everything else in hostmode_test.go that never needs either stays
// untagged so a darwin build of this package's test binary isn't broken by a helper detail no
// darwin client build ever touches.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/brokertest"
	"github.com/sjawhar/envoy/internal/broker/helper"
)

// peerRequest is one connection fakeHelperWithPids answered: the pid the kernel names for its
// peer (0 when it named none) and the request line, as sent.
type peerRequest struct {
	pid  int
	line string
}

// fakeHelperWithPids answers one canned line per connection and records the peer pid and the
// request line of each — the pidfd pin only Linux provides. Each connection is served on a
// goroutine of its own, its request read within registerCallWait(0), the wait its one client
// (register --exec with no --wait) gives one call (serveFakeHelper).
func fakeHelperWithPids(t *testing.T, canned helper.Response) (string, <-chan peerRequest) {
	t.Helper()
	sock := filepath.Join(shortTempDir(t), "h.sock")
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: sock, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	seen := make(chan peerRequest, 8)
	serveFakeHelper(t, ln, registerCallWait(0), func(_ int, conn *net.UnixConn, line []byte) []byte {
		// The client is still connected, waiting for the answer, so its pid is still the peer's.
		got := peerRequest{line: string(line)}
		if peer, err := helper.PeerOf(conn); err == nil {
			got.pid = peer.PID()
			peer.Close()
		}
		seen <- got
		data, _ := json.Marshal(canned)
		return data
	})
	return sock, seen
}

// TestRegisterExecStillPinsTheReplacedProcess must run in a subprocess: --exec replaces the
// process image. With --exec the client sends no root and does not attach — the pid the helper
// pins is the very pid the command runs as — and that is unchanged by the launcher ancestor the
// other forms now name.
func TestRegisterExecStillPinsTheReplacedProcess(t *testing.T) {
	sock, requests := fakeHelperWithPids(t, helper.Response{OK: true, State: "enrolling", RuntimeID: "session-exec"})
	cmd := exec.Command("go", "run", ".", "register", "--session-id", "exec", "--exec", "--", "sh", "-c", "echo $$")
	cmd.Env = append(os.Environ(), "AGENT_SECRETS_HELPER_SOCK="+sock)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("register --exec: %v", err)
	}
	printed, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		t.Fatalf("sh must print its pid, got %q", out)
	}
	select {
	case seen := <-requests:
		if seen.pid != printed {
			t.Fatalf("the pid the helper pinned (%d) must be the pid the command runs as (%d)", seen.pid, printed)
		}
		if strings.Contains(seen.line, `"root_pid"`) || strings.Contains(seen.line, `"attach"`) {
			t.Fatalf("register --exec sent %s; want neither root_pid nor attach", strings.TrimSpace(seen.line))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the helper saw no register")
	}
}

// realHelper serves a real helper.Server — the daemon's own registry, pidfd pinning and sign op —
// on a socket, with a broker that can never enroll anyone (no launcher credential, nothing
// listening), so every session it registers stays unenrolled.
func realHelper(t *testing.T) string {
	t.Helper()
	_, sock := serveRealHelper(t, "http://127.0.0.1:1", "ada@example.com")
	return sock
}

// serveRealHelper is serveRealHelperWith sending the helper's broker calls through
// http.DefaultClient.
func serveRealHelper(t *testing.T, brokerURL, operator string) (*helper.Server, string) {
	t.Helper()
	return serveRealHelperWith(t, brokerURL, operator, http.DefaultClient)
}

func helperSessions(t *testing.T, sock string) []helper.SessionInfo {
	t.Helper()
	resp, err := helper.Call(sock, helper.Request{Op: "sessions"}, 5*time.Second)
	if err != nil || !resp.OK {
		t.Fatalf("sessions: %+v %v", resp, err)
	}
	return resp.Sessions
}

// TestIdentityAsksARealHelperWithoutRegistering runs identity against the real helper from a
// process no session registered: it answers 1 (NOT_A_SESSION) and leaves the helper's sessions
// exactly as they were — identity must never become a registration, which is what `register`
// from an unregistered process is. nocredential_linux_test.go covers identity inside a
// registered session.
func TestIdentityAsksARealHelperWithoutRegistering(t *testing.T) {
	binary := buildAgentSecrets(t)
	sock := realHelper(t)
	env := append(os.Environ(), "AGENT_SECRETS_HELPER_SOCK="+sock, "AGENT_SECRETS_KEY_DIR=", "XDG_RUNTIME_DIR="+t.TempDir(), "HOME="+t.TempDir())

	before := helperSessions(t, sock)
	cmd := exec.Command(binary, "identity")
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if code := cmd.ProcessState.ExitCode(); code != 1 {
		t.Fatalf("identity from an unregistered process: exit %d (%v), output %q; want 1", code, err, out)
	}
	if after := helperSessions(t, sock); len(after) != len(before) {
		t.Fatalf("identity changed the helper's sessions: before %+v, after %+v", before, after)
	}
}

// launcherReport is what the launcher helper below tells its parent: its own pid, which is the
// session root the helper must pin, and what `register` and `identity` answered inside it.
type launcherReport struct {
	PID          int    `json:"pid"`
	RegisterCode int    `json:"register_code"`
	RegisterOut  string `json:"register_out"`
	RegisterErr  string `json:"register_err"`
	IdentityCode int    `json:"identity_code"`
	IdentityErr  string `json:"identity_err"`
}

// TestRegisterLauncherHelper is the session launcher the test below runs: this same test binary,
// started through a symlink named `omp`, so the kernel's comm for it is `omp` exactly as a real
// session launcher's is, and no session registered it. It runs `register --session-id` and then
// `identity` as its own children, reports both, and stays alive until its parent closes its
// stdin, so the parent can read the helper's sessions while the launcher is still there.
func TestRegisterLauncherHelper(t *testing.T) {
	if os.Getenv("AGENT_SECRETS_LAUNCHER_HELPER") == "" {
		t.Skip("subprocess helper")
	}
	binary := os.Getenv("AGENT_SECRETS_LAUNCHER_BINARY")
	run := func(args ...string) (int, string, string) {
		cmd := exec.Command(binary, args...)
		var out, errOut strings.Builder
		cmd.Stdout, cmd.Stderr = &out, &errOut
		_ = cmd.Run()
		return cmd.ProcessState.ExitCode(), out.String(), errOut.String()
	}
	report := launcherReport{PID: os.Getpid()}
	report.RegisterCode, report.RegisterOut, report.RegisterErr =
		run("register", "--session-id", os.Getenv("AGENT_SECRETS_LAUNCHER_SESSION"), "--wait", "5")
	report.IdentityCode, _, report.IdentityErr = run("identity")
	line, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Println(string(line))
	_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
}

// TestRegisterSessionIDInsideAnUnregisteredOmpAttachesTheOmpTree is this change's own
// case: a session launcher that registered nothing at launch, and `register --session-id` run
// from inside it. The session must be the launcher's process tree, so the next `agent-secrets
// identity` from inside that tree exits 0 — before this change the helper pinned the register
// command itself, which had already exited, and identity answered NOT_A_SESSION while register
// had printed `enrolled` and exited 0.
func TestRegisterSessionIDInsideAnUnregisteredOmpAttachesTheOmpTree(t *testing.T) {
	rig := brokertest.NewRig(t)
	binary := buildAgentSecrets(t)
	_, sock := serveRealHelper(t, rig.URL, rig.Operator)
	if code, exit, stderr := machineLoginThen(t, rig, binary, sock, "approve", nil); exit != 0 {
		t.Fatalf("machine login (code %s): exit %d, stderr %q", code, exit, stderr)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	launcher := filepath.Join(t.TempDir(), "omp")
	if err := os.Symlink(self, launcher); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(launcher, "-test.run", "^TestRegisterLauncherHelper$")
	cmd.Env = append(os.Environ(),
		"AGENT_SECRETS_LAUNCHER_HELPER=1",
		"AGENT_SECRETS_LAUNCHER_BINARY="+binary,
		"AGENT_SECRETS_LAUNCHER_SESSION=s-3890",
		"AGENT_SECRETS_HELPER_SOCK="+sock,
		"AGENT_SECRETS_URL="+rig.URL,
		"AGENT_SECRETS_KEY_DIR=",
		"XDG_RUNTIME_DIR="+t.TempDir(),
		"HOME="+t.TempDir())
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waited := make(chan struct{})
	t.Cleanup(func() {
		_ = stdin.Close()
		select {
		case <-waited:
		case <-time.After(10 * time.Second):
			_ = cmd.Process.Kill()
		}
		<-waited
	})
	go func() { _ = cmd.Wait(); close(waited) }()

	reports := make(chan launcherReport, 1)
	go func() {
		lines := bufio.NewScanner(stdout)
		for lines.Scan() {
			var report launcherReport
			if err := json.Unmarshal(lines.Bytes(), &report); err == nil && report.PID != 0 {
				reports <- report
				return
			}
		}
	}()
	var report launcherReport
	select {
	case report = <-reports:
	case <-time.After(60 * time.Second):
		t.Fatalf("the launcher never reported; stderr %q", stderr.String())
	}

	if report.RegisterCode != 0 || !strings.HasSuffix(strings.TrimSpace(report.RegisterOut), "enrolled") {
		t.Fatalf("register --session-id inside the launcher: exit %d, stdout %q, stderr %q; want 0 and an enrolled line",
			report.RegisterCode, report.RegisterOut, report.RegisterErr)
	}
	if report.IdentityCode != 0 {
		t.Fatalf("identity after register --session-id: exit %d, stderr %q; want 0 — the session must be the launcher's tree",
			report.IdentityCode, report.IdentityErr)
	}
	sessions := helperSessions(t, sock)
	if len(sessions) != 1 || sessions[0].PID != report.PID || sessions[0].RuntimeID != "session-s-3890" {
		t.Fatalf("the helper's sessions are %+v, want one rooted at the launcher %d as session-s-3890", sessions, report.PID)
	}
}

// TestRegisterWithoutAnOmpAncestorRefusesAndSendsNothing: with no session launcher anywhere above
// it and in no session, `register --session-id` is refused NOT_A_SESSION by the helper, which
// alone knows the caller is in no session, and nothing is registered, rather than a session whose
// root is the command's own process, with `enrolled` printed for it. The register request does
// reach the helper: the name, which the verification's check k1 runs, now means no registration.
func TestRegisterWithoutAnOmpAncestorRefusesAndSendsNothing(t *testing.T) {
	binary := buildAgentSecrets(t)
	sock := realHelper(t)
	code, stdout, stderr := runWithNoLauncherAbove(t, binary,
		[]string{"AGENT_SECRETS_HELPER_SOCK=" + sock, "AGENT_SECRETS_KEY_DIR=", "HOME=" + t.TempDir(), "XDG_RUNTIME_DIR=" + t.TempDir()},
		"register", "--session-id", "s-3890", "--wait", "5")
	if code != 1 || stdout != "" {
		t.Fatalf("register with no launcher above it: exit %d, stdout %q, stderr %q; want 1 and nothing on stdout", code, stdout, stderr)
	}
	for _, want := range []string{helper.CodeNotASession, "no session launcher (omp)", "register --exec"} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("the helper's refusal names %q: %q", want, stderr)
		}
	}
	if sessions := helperSessions(t, sock); len(sessions) != 0 {
		t.Fatalf("a refused register registered %+v", sessions)
	}
}

// TestRegisterSessionIDInsideAnExecSessionWithNoOmpNamesIt: a session `register --exec` started
// for a command that is no launcher — the operator's dotfiles start Claude Code sessions as
// `register --exec -- claude`, and their SessionStart hook runs `register --session-id` from
// inside — has no `omp` above the hook. The helper finds the caller inside that session and names
// it, so the register prints the session-scoped runtime id and `enrolled` and exits 0, and
// identity from the same tree exits 0. `sh` stands for `claude` here.
func TestRegisterSessionIDInsideAnExecSessionWithNoOmpNamesIt(t *testing.T) {
	rig := brokertest.NewRig(t)
	binary := buildAgentSecrets(t)
	_, sock := serveRealHelper(t, rig.URL, rig.Operator)
	if code, exit, stderr := machineLoginThen(t, rig, binary, sock, "approve", nil); exit != 0 {
		t.Fatalf("machine login (code %s): exit %d, stderr %q", code, exit, stderr)
	}
	code, stdout, stderr := runWithNoLauncherAbove(t, binary,
		[]string{"AGENT_SECRETS_HELPER_SOCK=" + sock, "AGENT_SECRETS_URL=" + rig.URL, "AGENT_SECRETS_KEY_DIR=", "HOME=" + t.TempDir(), "XDG_RUNTIME_DIR=" + t.TempDir()},
		"register", "--exec", "--", "sh", "-c", `"$0" register --session-id s-exec --wait 5 || exit $?; "$0" identity >/dev/null || exit 10`, binary)
	if code != 0 || !strings.HasPrefix(stdout, "session-s-exec\t") || !strings.HasSuffix(strings.TrimSpace(stdout), "enrolled") {
		t.Fatalf("register --session-id inside an --exec session with no omp: exit %d (10: identity refused), stdout %q, stderr %q; want 0 and session-s-exec enrolled", code, stdout, stderr)
	}
}

// runWithNoLauncherAbove runs binary in a process tree with no `omp` anywhere above it: the test
// process itself may well be inside a session launcher (every agent session on a developer box
// is), so the command is started from a shell this test orphans — the kernel reparents it to the
// nearest subreaper, or to init — and the shell waits for a go-ahead file so this test can read
// its ancestry before the command runs. It fails, rather than skipping, when an `omp` is still
// above that shell: a check that cannot see the thing it is for must say so.
//
// The orphan cannot be waited on, so it reports through files: its process ID, its stdout, its
// stderr and its exit code. The process ID and the exit code are written under another name and
// renamed, so a read of either is never a partial write.
func runWithNoLauncherAbove(t *testing.T, binary string, env []string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "orphan.sh")
	const body = `d=$1
shift
printf '%s\n' "$$" > "$d/shell.pid.tmp"
mv "$d/shell.pid.tmp" "$d/shell.pid"
i=0
while [ ! -e "$d/go" ]; do
	if [ -e "$d/stop" ]; then exit 1; fi
	i=$((i + 1))
	if [ "$i" -gt 1500 ]; then exit 1; fi
	sleep 0.02
done
"$@" > "$d/out" 2> "$d/err"
printf '%s\n' "$?" > "$d/code.tmp"
mv "$d/code.tmp" "$d/code"
`
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	outer := exec.Command("sh", "-c", `sh "$0" "$@" &`, script, dir, binary)
	outer.Args = append(outer.Args, args...)
	outer.Env = append(os.Environ(), env...)
	if err := outer.Run(); err != nil {
		t.Fatalf("starting the orphaned shell: %v", err)
	}
	shellPID := atoiFile(t, filepath.Join(dir, "shell.pid"), 10*time.Second)
	t.Cleanup(func() { _ = os.WriteFile(filepath.Join(dir, "stop"), nil, 0o600) })
	if pid, found := ancestorNamed(t, shellPID, "omp"); found {
		t.Fatalf("pid %d is still an `omp` above the orphaned shell %d, so this test could not show the refusal it is for", pid, shellPID)
	}
	if err := os.WriteFile(filepath.Join(dir, "go"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	code = atoiFile(t, filepath.Join(dir, "code"), 60*time.Second)
	return code, readFile(t, filepath.Join(dir, "out")), readFile(t, filepath.Join(dir, "err"))
}

// atoiFile is the number in path once it is there, waiting at most wait for it.
func atoiFile(t *testing.T, path string, wait time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(wait)
	for {
		data, err := os.ReadFile(path)
		if err == nil {
			n, err := strconv.Atoi(strings.TrimSpace(string(data)))
			if err != nil {
				t.Fatalf("%s holds %q, want a number", path, data)
			}
			return n
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s was never written (%v)", path, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

// ancestorNamed is the nearest ancestor of pid, pid itself included, whose /proc comm is name.
// It reads /proc itself rather than calling the code under test, so it can contradict it.
func ancestorNamed(t *testing.T, pid int, name string) (int, bool) {
	t.Helper()
	for hops := 0; hops < 64 && pid > 1; hops++ {
		data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if err != nil {
			t.Fatalf("reading the ancestry of the orphaned shell at pid %d: %v", pid, err)
		}
		stat := string(data)
		open, close := strings.IndexByte(stat, '('), strings.LastIndexByte(stat, ')')
		if open < 0 || close < open+1 || close+2 > len(stat) {
			t.Fatalf("/proc/%d/stat has no comm: %q", pid, stat)
		}
		if stat[open+1:close] == name {
			return pid, true
		}
		fields := strings.Fields(stat[close+2:])
		if len(fields) < 2 {
			t.Fatalf("/proc/%d/stat has %d fields after its comm", pid, len(fields))
		}
		parent, err := strconv.Atoi(fields[1])
		if err != nil {
			t.Fatalf("/proc/%d/stat names the parent %q: %v", pid, fields[1], err)
		}
		pid = parent
	}
	return 0, false
}

// parentPID is pid's parent as /proc names it, read here rather than through the code under
// test so it can contradict it.
func parentPID(t *testing.T, pid int) int {
	t.Helper()
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		t.Fatal(err)
	}
	stat := string(data)
	fields := strings.Fields(stat[strings.LastIndexByte(stat, ')')+2:])
	parent, err := strconv.Atoi(fields[1])
	if err != nil {
		t.Fatalf("/proc/%d/stat names the parent %q: %v", pid, fields[1], err)
	}
	return parent
}

// stubLauncher makes nearestLauncher answer pid and err for this test: every register without
// --exec asks it which ancestor to name, and a test process is not started by a session
// launcher.
func stubLauncher(t *testing.T, pid int, err error) {
	t.Helper()
	previous := nearestLauncher
	nearestLauncher = func() (int, error) { return pid, err }
	t.Cleanup(func() { nearestLauncher = previous })
}

// fixedLauncher makes this test process's parent its launcher: a real ancestor, since the
// command checks that the root the helper answers is one. It returns that pid.
func fixedLauncher(t *testing.T) int {
	t.Helper()
	launcher := os.Getppid()
	stubLauncher(t, launcher, nil)
	return launcher
}

// TestRegisterWithoutExecPrintsRuntimeEnrollmentState covers register's contract without
// --exec: "<runtime_id>\t<enrollment_id>\t<state>" on stdout, exit 0.
func TestRegisterWithoutExecPrintsRuntimeEnrollmentState(t *testing.T) {
	launcher := fixedLauncher(t)
	sock, _ := fakeHelper(t, helper.Response{OK: true, RuntimeID: "h:1:1", EnrollmentID: "enr-h", State: "enrolled", RootPID: launcher})
	t.Setenv("AGENT_SECRETS_HELPER_SOCK", sock)
	var stdout, stderr bytes.Buffer
	code := cmdRegister(nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("register: exit %d, stderr %q", code, stderr.String())
	}
	if got, want := stdout.String(), "h:1:1\tenr-h\tenrolled\n"; got != want {
		t.Fatalf("register output: got %q, want %q", got, want)
	}
}

// TestRegisterPrintsTheBrokersEnrollmentOnOneLine: the enrollment register prints, which the
// broker made, and the runtime id and state beside it print as they came where they hold no
// control character and Go-quoted where they hold one, as TestTheEnrollmentIDsPrintOnOneLine
// checks for the other forms that print an enrollment. It is here because register without
// --exec takes the helper's answer only for a root above this process, which only Linux can check.
func TestRegisterPrintsTheBrokersEnrollmentOnOneLine(t *testing.T) {
	launcher := fixedLauncher(t)
	sock, _ := fakeHelperByOp(t, map[string]helper.Response{"register": {OK: true, RuntimeID: forged, EnrollmentID: forged, State: forged, RootPID: launcher}})
	t.Setenv("AGENT_SECRETS_HELPER_SOCK", sock)
	var stdout, stderr bytes.Buffer
	if code := cmdRegister(nil, &stdout, &stderr); code != 0 {
		t.Fatalf("register = %d (stderr %q), want 0", code, stderr.String())
	}
	everyLineIsOne(t, "register's stdout", stdout.String(), 3)
}

// TestRegisterWithoutAWaitPrintsEnrollingAndExitsZero: with no positive --wait, a confirmed root
// whose session is still enrolling is printed as that state, and the command exits 0.
func TestRegisterWithoutAWaitPrintsEnrollingAndExitsZero(t *testing.T) {
	launcher := fixedLauncher(t)
	sock, _ := fakeHelper(t, helper.Response{OK: true, RuntimeID: "h:1:1", EnrollmentID: "enr-h", State: "enrolling", Error: "waiting on approver", RootPID: launcher})
	t.Setenv("AGENT_SECRETS_HELPER_SOCK", sock)
	var stdout, stderr bytes.Buffer
	code := cmdRegister(nil, &stdout, &stderr)
	if code != 0 || stdout.String() != "h:1:1\tenr-h\tenrolling\n" || stderr.String() != "" {
		t.Fatalf("register with no wait, still enrolling: exit %d, stdout %q, stderr %q; want 0, the enrolling line and nothing on stderr", code, stdout.String(), stderr.String())
	}
}

// TestRegisterSendsTheNearestLauncherAsRoot: without --exec the client attaches (the helper must
// never pin its own process, which ends with the command) and names the session launcher it found
// above itself as the root to pin. The fields go on the wire as attach and root_pid.
func TestRegisterSendsTheNearestLauncherAsRoot(t *testing.T) {
	launcher := fixedLauncher(t)
	sock, reqs := fakeHelper(t, helper.Response{OK: true, RuntimeID: "session-s", EnrollmentID: "enr-h", State: "enrolled", RootPID: launcher})
	t.Setenv("AGENT_SECRETS_HELPER_SOCK", sock)
	var stdout, stderr bytes.Buffer
	if code := cmdRegister([]string{"--session-id", "s"}, &stdout, &stderr); code != 0 {
		t.Fatalf("register --session-id: exit %d, stderr %q", code, stderr.String())
	}
	select {
	case req := <-reqs:
		if req.Op != "register" || !req.Attach || req.RootPID != launcher {
			t.Fatalf("the request the helper saw: %+v; want register, attach, root_pid %d", req, launcher)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the fake helper saw no request")
	}
	wire, err := json.Marshal(helper.Request{Op: "register", Attach: true, RootPID: 4242})
	if err != nil || !strings.Contains(string(wire), `"attach":true`) || !strings.Contains(string(wire), `"root_pid":4242`) {
		t.Fatalf("the fields go on the wire as attach and root_pid: %s %v", wire, err)
	}
}

// TestRegisterWithNoLauncherStillAsksTheHelper: with no `omp` above it, register attaches and
// names no root, and the helper decides: it alone knows whether the caller is inside a session
// already (one `register --exec -- claude` started, say). Its answer for that session, rooted
// above this process, is printed, and the command exits 0.
func TestRegisterWithNoLauncherStillAsksTheHelper(t *testing.T) {
	stubLauncher(t, 0, errNoLauncher)
	root := os.Getppid()
	sock, reqs := fakeHelper(t, helper.Response{OK: true, RuntimeID: "session-s", EnrollmentID: "enr-h", State: "enrolled", RootPID: root})
	t.Setenv("AGENT_SECRETS_HELPER_SOCK", sock)
	var stdout, stderr bytes.Buffer
	code := cmdRegister([]string{"--session-id", "s"}, &stdout, &stderr)
	if code != 0 || stdout.String() != "session-s\tenr-h\tenrolled\n" {
		t.Fatalf("register with no launcher inside a session: exit %d, stdout %q, stderr %q; want 0 and the session's line", code, stdout.String(), stderr.String())
	}
	select {
	case req := <-reqs:
		if req.Op != "register" || !req.Attach || req.RootPID != 0 {
			t.Fatalf("the request the helper saw: %+v; want register, attach, no root_pid", req)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the fake helper saw no request")
	}
}

// TestRegisterAcceptsARootAboveTheLauncher: inside a registered session whose root is not the
// nearest `omp` — an `omp` run under a shell the docs register with `--exec -- bash` — the
// helper answers that session, rooted above the launcher this command named. A root that is an
// ancestor of this process is this session's, so the command prints it and exits 0.
func TestRegisterAcceptsARootAboveTheLauncher(t *testing.T) {
	launcher := fixedLauncher(t)
	above := parentPID(t, launcher)
	if above <= 1 {
		t.Fatalf("the test process's parent %d has no parent above init (%d), so this test cannot name a root above it", launcher, above)
	}
	sock, _ := fakeHelper(t, helper.Response{OK: true, RuntimeID: "session-s", EnrollmentID: "enr-h", State: "enrolled", RootPID: above})
	t.Setenv("AGENT_SECRETS_HELPER_SOCK", sock)
	var stdout, stderr bytes.Buffer
	code := cmdRegister([]string{"--session-id", "s"}, &stdout, &stderr)
	if code != 0 || stdout.String() != "session-s\tenr-h\tenrolled\n" {
		t.Fatalf("register answered a root above its launcher: exit %d, stdout %q, stderr %q; want 0 and the session's line", code, stdout.String(), stderr.String())
	}
}

// TestRegisterRefusesAHelperThatNamesNoRoot: a helper from before this change pins the caller,
// which ends with the command, and says nothing about the root it pinned. Its `enrolled` is
// never printed as success: the command refuses, naming the helper and the relaunch.
func TestRegisterRefusesAHelperThatNamesNoRoot(t *testing.T) {
	fixedLauncher(t)
	sock, _ := fakeHelper(t, helper.Response{OK: true, RuntimeID: "session-s", EnrollmentID: "enr-h", State: "enrolled"})
	t.Setenv("AGENT_SECRETS_HELPER_SOCK", sock)
	var stdout, stderr bytes.Buffer
	code := cmdRegister([]string{"--session-id", "s"}, &stdout, &stderr)
	if code != 1 || stdout.String() != "" {
		t.Fatalf("register against a helper that names no root: exit %d, stdout %q, stderr %q; want 1 and nothing on stdout", code, stdout.String(), stderr.String())
	}
	if msg := stderr.String(); !strings.Contains(msg, sock) || !strings.Contains(msg, "before this fix") || !strings.Contains(msg, "register --exec") {
		t.Fatalf("the refusal names the helper's socket, its age and --exec: %q", msg)
	}
}

// TestRegisterRefusesARootThatIsNotAnAncestor: a root the helper answers that is not a process
// above this command — the command's own process, which ends with it, or a child of it — is not
// this session's root, so its `enrolled` is never printed.
func TestRegisterRefusesARootThatIsNotAnAncestor(t *testing.T) {
	fixedLauncher(t)
	child := exec.Command("sleep", "30")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = child.Process.Kill(); _ = child.Wait() })
	for _, tc := range []struct {
		name string
		root int
	}{
		{"this process", os.Getpid()},
		{"a child of it", child.Process.Pid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sock, _ := fakeHelper(t, helper.Response{OK: true, RuntimeID: "session-s", EnrollmentID: "enr-h", State: "enrolled", RootPID: tc.root})
			t.Setenv("AGENT_SECRETS_HELPER_SOCK", sock)
			var stdout, stderr bytes.Buffer
			code := cmdRegister([]string{"--session-id", "s"}, &stdout, &stderr)
			if code != 1 || stdout.String() != "" || !strings.Contains(stderr.String(), "not an ancestor") {
				t.Fatalf("register answered %s as the root: exit %d, stdout %q, stderr %q; want 1, nothing on stdout, and not an ancestor", tc.name, code, stdout.String(), stderr.String())
			}
		})
	}
}

// TestRegisterWithoutExecExitsOneWhenWaitedButNotEnrolled covers "exits 1 if --wait was given
// and state isn't enrolled": the state is printed, and stderr says it is not enrolled yet and why.
func TestRegisterWithoutExecExitsOneWhenWaitedButNotEnrolled(t *testing.T) {
	launcher := fixedLauncher(t)
	sock, _ := fakeHelper(t, helper.Response{OK: true, RuntimeID: "h:1:1", EnrollmentID: "enr-h", State: "enrolling", Error: "waiting on approver", RootPID: launcher})
	t.Setenv("AGENT_SECRETS_HELPER_SOCK", sock)
	var stdout, stderr bytes.Buffer
	code := cmdRegister([]string{"--wait", "1"}, &stdout, &stderr)
	if code != 1 || stdout.String() != "h:1:1\tenr-h\tenrolling\n" || !strings.Contains(stderr.String(), "not enrolled yet: waiting on approver") {
		t.Fatalf("register --wait, not enrolled: exit %d, stdout %q, stderr %q; want 1, the enrolling line, and not enrolled yet: waiting on approver", code, stdout.String(), stderr.String())
	}
}

// TestRegisterFindsTheHelperOnItsDefaultSocket pins that register, like machine login and every
// other helper-mode form, reaches a helper listening on its default socket
// ($XDG_RUNTIME_DIR/agent-secrets/helper.sock) with AGENT_SECRETS_HELPER_SOCK unset: a machine set
// up by starting the helper with no settings registers its sessions without exporting the variable.
func TestRegisterFindsTheHelperOnItsDefaultSocket(t *testing.T) {
	launcher := fixedLauncher(t)
	runtimeDir := shortTempDir(t)
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)
	t.Setenv("AGENT_SECRETS_HELPER_SOCK", "")
	reqs := fakeHelperAt(t, filepath.Join(runtimeDir, "agent-secrets", "helper.sock"),
		helper.Response{OK: true, RuntimeID: "h:1:1", EnrollmentID: "enr-h", State: "enrolled", RootPID: launcher})
	var stdout, stderr bytes.Buffer
	if code := cmdRegister(nil, &stdout, &stderr); code != 0 {
		t.Fatalf("register with only the default socket: exit %d, stderr %q", code, stderr.String())
	}
	if got, want := stdout.String(), "h:1:1\tenr-h\tenrolled\n"; got != want {
		t.Fatalf("register output: got %q, want %q", got, want)
	}
	select {
	case req := <-reqs:
		if req.Op != "register" {
			t.Fatalf("request sent to the default socket: %+v", req)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the helper on the default socket never saw a request")
	}
}
