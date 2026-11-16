// packages/envoy/cmd/agent-secrets/hostmode_linux_test.go
//go:build linux

// fakeHelperWithPids and TestRegisterExecStillPinsTheReplacedProcess need helper.PeerOf
// (SO_PEERPIDFD), which is Linux-only (internal/broker/helper/peer.go carries the same
// //go:build linux); everything else in hostmode_test.go that never needs to observe a
// connecting pid stays untagged so a darwin build of this package's test binary isn't broken by
// a helper detail no darwin client build ever touches.
package main

import (
	"bufio"
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

// fakeHelperWithPids answers one canned line per connection and records the peer pid of each
// request — the pidfd pin only Linux provides.
func fakeHelperWithPids(t *testing.T, canned helper.Response) (string, <-chan int) {
	t.Helper()
	sock := filepath.Join(shortTempDir(t), "h.sock")
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: sock, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	pids := make(chan int, 8)
	go func() {
		for {
			conn, err := ln.AcceptUnix()
			if err != nil {
				return
			}
			peer, err := helper.PeerOf(conn)
			if err == nil {
				pids <- peer.PID()
				peer.Close()
			}
			_, _ = bufio.NewReader(conn).ReadBytes('\n')
			data, _ := json.Marshal(canned)
			_, _ = conn.Write(append(data, '\n'))
			conn.Close()
		}
	}()
	return sock, pids
}

// TestRegisterExecStillPinsTheReplacedProcess must run in a subprocess: --exec replaces the
// process image. With --exec the client sends no root at all — the pid the helper pins is the
// very pid the command runs as — and that is unchanged by the launcher ancestor the other forms
// now name.
func TestRegisterExecStillPinsTheReplacedProcess(t *testing.T) {
	sock, pids := fakeHelperWithPids(t, helper.Response{OK: true, State: "enrolling", RuntimeID: "session-exec"})
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
	case registered := <-pids:
		if registered != printed {
			t.Fatalf("the pid the helper pinned (%d) must be the pid the command runs as (%d)", registered, printed)
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
// it, `register --session-id` refuses and sends the helper nothing at all, rather than enrolling a
// session whose root is the command's own process and printing `enrolled` for it.
func TestRegisterWithoutAnOmpAncestorRefusesAndSendsNothing(t *testing.T) {
	binary := buildAgentSecrets(t)
	sock, requests := fakeHelper(t, helper.Response{OK: true, RuntimeID: "session-s", EnrollmentID: "enr-h", State: "enrolled"})
	code, stdout, stderr := runWithNoLauncherAbove(t, binary,
		[]string{"AGENT_SECRETS_HELPER_SOCK=" + sock, "AGENT_SECRETS_KEY_DIR=", "HOME=" + t.TempDir(), "XDG_RUNTIME_DIR=" + t.TempDir()},
		"register", "--session-id", "s-3890", "--wait", "5")
	if code != 1 || stdout != "" {
		t.Fatalf("register with no launcher above it: exit %d, stdout %q, stderr %q; want 1 and nothing on stdout", code, stdout, stderr)
	}
	if !strings.Contains(stderr, "no session launcher (omp)") || !strings.Contains(stderr, "register --exec") {
		t.Fatalf("the refusal names the missing launcher and the relaunch: %q", stderr)
	}
	select {
	case req := <-requests:
		t.Fatalf("the helper was sent %+v; a refused register sends it nothing", req)
	case <-time.After(250 * time.Millisecond):
	}
}

// runWithNoLauncherAbove runs binary in a process tree with no `omp` anywhere above it: the test
// process itself may well be inside a session launcher (every agent session on a developer box
// is), so the command is started from a shell this test orphans — the kernel reparents it to the
// nearest subreaper, or to init — and the shell waits for a go-ahead file so this test can read
// its ancestry before the command runs. It fails, rather than skipping, when an `omp` is still
// above that shell: a check that cannot see the thing it is for must say so.
//
// The orphan cannot be waited on, so it reports through files: its stdout, its stderr and its
// exit code, the last written under another name and renamed, so a code read is never a partial
// write.
func runWithNoLauncherAbove(t *testing.T, binary string, env []string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "orphan.sh")
	const body = `d=$1
shift
printf '%s\n' "$$" > "$d/shell.pid"
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
