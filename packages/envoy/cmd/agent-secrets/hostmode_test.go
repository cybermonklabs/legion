// packages/envoy/cmd/agent-secrets/hostmode_test.go
//
// These tests cover the Signer abstraction (buildSigner, fileSigner, helperSigner) and the three
// host-mode subcommands: register, whoami, and sign. Everything that doesn't exec a child runs
// in-process (this file is package main, same as main_test.go); register --exec must run in a
// subprocess, exactly as main_test.go's exec-form tests do, because it replaces the process
// image.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime/pprof"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/brokeraddr"
	"github.com/sjawhar/envoy/internal/broker/helper"
	"github.com/sjawhar/envoy/internal/broker/proof"
)

// TestBuildSignerSelectsMode covers the Signer abstraction's mode selection: file mode when
// AGENT_SECRETS_KEY_DIR has a readable key.pem, helper mode when it doesn't but the helper at
// AGENT_SECRETS_HELPER_SOCK says this process is in a session it registered, a stop naming the
// helper when the socket that variable names is not there, and the chain's refusal naming both
// variables when neither is set. XDG_RUNTIME_DIR points at an empty
// directory so a helper running on the test machine, at the default socket buildSigner falls back
// to, cannot change the answer, and the state directory and the broker URL are the test's own, so
// no sign-in, machine cache or broker of the machine running it can either.
func TestBuildSignerSelectsMode(t *testing.T) {
	isolateChain(t)
	t.Setenv("AGENT_SECRETS_KEY_DIR", "")
	t.Setenv("AGENT_SECRETS_HELPER_SOCK", "")
	if _, _, err := buildSigner(); err == nil ||
		!strings.Contains(err.Error(), "no broker identity for this process") ||
		!strings.Contains(err.Error(), "AGENT_SECRETS_KEY_DIR") ||
		!strings.Contains(err.Error(), "AGENT_SECRETS_HELPER_SOCK") {
		t.Fatalf("no identity must name both variables: %v", err)
	}

	emptyDir := t.TempDir()
	sock, _ := fakeHelper(t, helper.Response{OK: true, Proof: "eyJ.fake.proof"})
	t.Setenv("AGENT_SECRETS_KEY_DIR", emptyDir)
	t.Setenv("AGENT_SECRETS_HELPER_SOCK", sock)
	_, signer, err := buildSigner()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := signer.(*helperSigner); !ok {
		t.Fatalf("a key dir without key.pem falls through to the helper: %T", signer)
	}

	t.Setenv("AGENT_SECRETS_HELPER_SOCK", "/x/h.sock")
	if _, _, err := buildSigner(); err == nil || !strings.Contains(err.Error(), "agent-secrets-helper at /x/h.sock") ||
		strings.Contains(err.Error(), "no broker identity for this process") {
		t.Fatalf("a named socket that is not there is a stopped helper, which stops the chain: %v", err)
	}

	t.Setenv("AGENT_SECRETS_KEY_DIR", newKeyDir(t))
	id, signer, err := buildSigner()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := signer.(*fileSigner); !ok {
		t.Fatalf("key.pem present means file mode: %T", signer)
	}
	if id != testEnrollmentID {
		t.Fatalf("enrollment id: got %q, want %q", id, testEnrollmentID)
	}
}

// isolateChain gives an in-process walk of the chain a home, a state directory and a runtime
// directory of the test's own, no broker URL and no machine identity setting, so neither a
// sign-in, a machine cache, a broker nor a machine identity of the machine running the test
// reaches its answer.
func isolateChain(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	t.Setenv("AGENT_SECRETS_SIGNIN_DIR", "")
	t.Setenv("AGENT_SECRETS_URL", "")
	t.Setenv("AGENT_SECRETS_MACHINE_IDENTITY", "")
}

// TestBuildSignerNamesTheEnrollmentErrorDiagnostic covers the enrollment.error side-channel: when
// key.pem is present but the enrollment file the launcher writes on success is missing (an
// enroll that failed), buildSigner's error appends whatever the launcher recorded in
// enrollment.error, so a caller sees why enrollment failed rather than a bare "no such file";
// with no enrollment.error file, the message is the bare missing-file error.
func TestBuildSignerNamesTheEnrollmentErrorDiagnostic(t *testing.T) {
	dir := newKeyDir(t)
	if err := os.Remove(filepath.Join(dir, "enrollment")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	t.Setenv("AGENT_SECRETS_KEY_DIR", dir)
	t.Setenv("AGENT_SECRETS_HELPER_SOCK", "")

	if _, _, err := buildSigner(); err == nil || strings.Contains(err.Error(), "enrollment.error") {
		t.Fatalf("no enrollment.error file must not be mentioned: %v", err)
	}

	if err := os.WriteFile(filepath.Join(dir, "enrollment.error"), []byte("broker 503 DATABASE: postgres unreachable\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := buildSigner()
	if err == nil || !strings.Contains(err.Error(), "enrollment.error") || !strings.Contains(err.Error(), "postgres unreachable") {
		t.Fatalf("a missing enrollment names the error file and its content: %v", err)
	}
}

// TestBuildSignerFallsBackToTheRuntimeDirDefaults covers a process whose AGENT_SECRETS_* were
// stripped (omp's eval kernel starts with a filtered environment): with both variables unset,
// buildSigner still finds a box's key dir at $XDG_RUNTIME_DIR/agent-secrets and a host's helper
// socket at $XDG_RUNTIME_DIR/agent-secrets/helper.sock, and falls back to neither when its file
// is not there.
func TestBuildSignerFallsBackToTheRuntimeDirDefaults(t *testing.T) {
	isolateChain(t)
	t.Setenv("AGENT_SECRETS_KEY_DIR", "")
	t.Setenv("AGENT_SECRETS_HELPER_SOCK", "")

	runtime := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", runtime)
	if _, _, err := buildSigner(); err == nil || !strings.Contains(err.Error(), "no broker identity for this process") {
		t.Fatalf("an empty runtime dir is no identity: %v", err)
	}

	copyKeyDir(t, newKeyDir(t), filepath.Join(runtime, "agent-secrets"))
	id, signer, err := buildSigner()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := signer.(*fileSigner); !ok || id != testEnrollmentID {
		t.Fatalf("the default key dir means file mode: %T %q", signer, id)
	}

	hostRuntime := shortTempDir(t)
	t.Setenv("XDG_RUNTIME_DIR", hostRuntime)
	sock := filepath.Join(hostRuntime, "agent-secrets", "helper.sock")
	fakeHelperAt(t, sock, helper.Response{OK: true, Proof: "eyJ.fake.proof"})
	_, signer, err = buildSigner()
	if err != nil {
		t.Fatal(err)
	}
	if hs, ok := signer.(*helperSigner); !ok || hs.sock != sock {
		t.Fatalf("the default socket means helper mode on %s: %#v", sock, signer)
	}
}

// copyKeyDir copies a key dir fixture's key.pem and enrollment into dst.
func copyKeyDir(t *testing.T, src, dst string) {
	t.Helper()
	if err := os.MkdirAll(dst, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"key.pem", "enrollment"} {
		copyFile(t, filepath.Join(src, name), filepath.Join(dst, name))
	}
}

func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestFileSignerMatchesWhatProofVerifyExpects round-trips a fileSigner's compact JWS through a
// real proof.Verifier: the same htm/htu/eid comparison the broker itself performs.
func TestFileSignerMatchesWhatProofVerifyExpects(t *testing.T) {
	key, err := proof.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	thumb, err := proof.Thumbprint(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	signer := &fileSigner{key: key, enrollmentID: "enr-round-trip"}
	const method, url = http.MethodGet, "https://broker.example/v1/enrollments/self"
	compact, err := signer.Sign(method, url)
	if err != nil {
		t.Fatal(err)
	}
	verifier := &proof.Verifier{
		Skew:   time.Minute,
		Lookup: func(ctx context.Context, enrollmentID string) (string, bool, error) { return thumb, true, nil },
		Replay: func(ctx context.Context, jti string, expires time.Time) (bool, error) { return true, nil },
	}
	id, err := verifier.Verify(context.Background(), compact, method, url, time.Now())
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if id.EnrollmentID != "enr-round-trip" {
		t.Fatalf("verified enrollment id: got %q", id.EnrollmentID)
	}
}

// fakeHelper answers one canned line per connection and returns every decoded request line it
// received, so a test can assert what the client actually sent the helper; most callers ignore
// the second return value, ready with `sock, _ := fakeHelper(...)`.
// The pid-observing variant (fakeHelperWithPids, hostmode_linux_test.go) is Linux-only, since it
// pins the connecting pid with helper.PeerOf (SO_PEERPIDFD); this one only needs an ordinary unix
// socket, so it stays available on every platform this package's test binary is built for.
func fakeHelper(t *testing.T, canned helper.Response) (string, <-chan helper.Request) {
	t.Helper()
	sock := filepath.Join(shortTempDir(t), "h.sock")
	return sock, fakeHelperAt(t, sock, canned)
}

// fakeHelperAt is fakeHelper listening at a path the caller chooses (a default socket path, say).
func fakeHelperAt(t *testing.T, sock string, canned helper.Response) <-chan helper.Request {
	t.Helper()
	return fakeHelperAnswering(t, sock, func(int) helper.Response { return canned })
}

// fakeHelperAnswering is fakeHelperAt answering each request by answer, given how many requests
// came before it: a helper whose answer changes between calls, such as one that says the caller is
// in its session and then refuses to sign. It serves each connection on a goroutine of its own and
// reads its request within anyHelperCallWait (serveFakeHelper).
func fakeHelperAnswering(t *testing.T, sock string, answer func(call int) helper.Response) <-chan helper.Request {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(sock), 0o700); err != nil {
		t.Fatal(err)
	}
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: sock, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	reqs := make(chan helper.Request, 8)
	serveFakeHelper(t, ln, anyHelperCallWait, func(call int, _ *net.UnixConn, line []byte) []byte {
		var req helper.Request
		_ = json.Unmarshal(line, &req)
		reqs <- req
		data, _ := json.Marshal(answer(call))
		return data
	})
	return reqs
}

// anyHelperCallWait is the longest wait a client of this package gives one call to the helper,
// as this package's tests make those calls: the read bound of fakeHelperAnswering and
// fakeHelperByOp, which answer every op. It is the longest of helper.BrokerCallWait (login, enroll
// --helper and unenroll --helper), helperSignWait (sign and sign-request), launcherSignWait
// (sign-launcher), loginStatusWait (login-status), identityDeadline (identity's probe and its
// version question) and registerCallWait for register --wait 1, the longest --wait a test gives
// register against those two fakes.
var anyHelperCallWait = max(helper.BrokerCallWait, helperSignWait, launcherSignWait, loginStatusWait, identityDeadline, registerCallWait(1))

// serveFakeHelper serves the connections ln takes for one of this package's fake helpers, each on
// a goroutine of its own, as the real helper serves them (helper.Server.Serve): a connection that
// sends nothing holds only itself, so the next call still gets its answer. Each connection's
// request line is read within readWithin, the longest wait the fake's clients give one call, so
// past it no client is still waiting for the answer. serve is given the call's number (how many
// connections came before it), the connection and the request line, and returns the answer line,
// without its newline, or nil to hang up with no answer; a connection that sent no whole line
// gets no answer and is not given to serve. ln closes when the test ends.
//
// When the test fails, the fake logs what it did with each connection and when (fakeHelperLog),
// so a call that got no answer shows whether the fake never took the connection, never read its
// request, or answered it late.
func serveFakeHelper(t *testing.T, ln *net.UnixListener, readWithin time.Duration, serve func(call int, conn *net.UnixConn, line []byte) []byte) {
	t.Helper()
	events := &fakeHelperLog{sock: ln.Addr().String()}
	t.Cleanup(func() { ln.Close() })
	// Registered after the close, so it runs before it: the socket table still lists the socket.
	t.Cleanup(func() { events.reportIfFailed(t) })
	go func() {
		for call := 0; ; call++ {
			conn, err := ln.AcceptUnix()
			if err != nil {
				if !errors.Is(err, net.ErrClosed) {
					events.note("accept failed, so the fake takes no more connections: %v", err)
				}
				return
			}
			events.note("call %d: connection taken", call)
			go func() {
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(readWithin))
				line, err := bufio.NewReader(conn).ReadBytes('\n')
				events.note("call %d: request read, %d bytes, error %v", call, len(line), err)
				if err != nil {
					return
				}
				answer := serve(call, conn, line)
				if answer == nil {
					events.note("call %d: hung up with no answer", call)
					return
				}
				_, err = conn.Write(append(answer, '\n'))
				events.note("call %d: answer written, error %v", call, err)
			}()
		}
	}()
}

// fakeHelperLog is what one fake helper did, with the time of each step, for the log of a test
// that failed.
type fakeHelperLog struct {
	sock   string
	mu     sync.Mutex
	events []string
}

func (l *fakeHelperLog) note(format string, args ...any) {
	line := time.Now().UTC().Format("15:04:05.000000") + " " + fmt.Sprintf(format, args...)
	l.mu.Lock()
	l.events = append(l.events, line)
	l.mu.Unlock()
}

// goroutinesLogged holds each failed test whose goroutines a fake has logged, so a test with more
// than one fake logs them once.
var goroutinesLogged sync.Map

// reportIfFailed logs, when t failed, every step the fake took, the lines of the unix socket table
// (/proc/net/unix, Linux) that name its socket, and the test process's goroutines.
func (l *fakeHelperLog) reportIfFailed(t *testing.T) {
	if !t.Failed() {
		return
	}
	l.mu.Lock()
	events := strings.Join(l.events, "\n")
	l.mu.Unlock()
	t.Logf("the fake helper at %s, as of %s:\n%s", l.sock, time.Now().UTC().Format("15:04:05.000000"), events)
	if table, err := os.ReadFile("/proc/net/unix"); err == nil {
		var lines []string
		for _, line := range strings.Split(string(table), "\n") {
			if strings.HasSuffix(line, " "+l.sock) {
				lines = append(lines, line)
			}
		}
		t.Logf("the unix socket table's lines for %s (Num RefCount Protocol Flags Type St Inode Path):\n%s", l.sock, strings.Join(lines, "\n"))
	}
	if _, logged := goroutinesLogged.LoadOrStore(t, true); !logged {
		var b strings.Builder
		_ = pprof.Lookup("goroutine").WriteTo(&b, 2)
		t.Logf("the test process's goroutines:\n%s", b.String())
	}
}

// TestFakeHelperAnswersPastAConnectionThatSendsNothing pins fakeHelperAnswering's own contract: a
// connection that sends no request holds only itself, so a call behind it still gets its answer,
// within the wait enroll --helper gives one call (helper.BrokerCallWait).
func TestFakeHelperAnswersPastAConnectionThatSendsNothing(t *testing.T) {
	sock, _ := fakeHelper(t, helper.Response{OK: true, EnrollmentID: "enr-behind"})
	silent, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer silent.Close()
	resp, err := helper.Call(sock, helper.Request{Op: "enroll-session-key"}, helper.BrokerCallWait)
	if err != nil || resp.EnrollmentID != "enr-behind" {
		t.Fatalf("a call behind a connection that sends nothing = %+v, %v; want the fake's answer", resp, err)
	}
}

// TestHelperSignerAsksTheHelper covers both a granted proof and the helper's error code
// reaching the caller unwrapped.
func TestHelperSignerAsksTheHelper(t *testing.T) {
	sock, _ := fakeHelper(t, helper.Response{OK: true, Proof: "eyJ.fake.proof"})
	signer := &helperSigner{sock: sock}
	p, err := signer.Sign(http.MethodGet, "https://s/v1/enrollments/self")
	if err != nil || p != "eyJ.fake.proof" {
		t.Fatalf("sign: %q %v", p, err)
	}

	sock2, _ := fakeHelper(t, helper.Response{Code: helper.CodeNotASession, Error: "pid 5 is not inside a registered machine session"})
	if _, err := (&helperSigner{sock: sock2}).Sign(http.MethodGet, "https://s/v1/enrollments/self"); err == nil ||
		!strings.Contains(err.Error(), helper.CodeNotASession) {
		t.Fatalf("the helper's code reaches the caller: %v", err)
	}
}

func TestHelperSignerRefusesAnEmptyProof(t *testing.T) {
	sock, _ := fakeHelper(t, helper.Response{OK: true})
	if _, err := (&helperSigner{sock: sock}).Sign(http.MethodGet, "https://s/v1/enrollments/self"); err == nil ||
		!strings.Contains(err.Error(), "no Proof header") {
		t.Fatalf("a helper that answered OK with no proof = %v; want an error naming the missing Proof header", err)
	}
}

// TestHelperSignerSignsRequestObjects covers SignRequestObject: it must send Op "sign-request"
// with the requested secret names, reason and lifetime (never audience — the helper signs against
// its own configured broker URL, not a value this peer supplies), return the helper's signed
// request object verbatim, and surface the helper's error code unwrapped exactly like Sign does.
func TestHelperSignerSignsRequestObjects(t *testing.T) {
	sock, reqs := fakeHelper(t, helper.Response{OK: true, RequestObject: "eyJ.fake.request"})
	signer := &helperSigner{sock: sock}
	lifetime := 120
	ro, err := signer.SignRequestObject(brokeraddr.New("https://secrets.test"), []string{"DEEL_API_KEY", "OTHER_KEY"}, "why", &lifetime)
	if err != nil || ro != "eyJ.fake.request" {
		t.Fatalf("sign-request: %q %v", ro, err)
	}
	select {
	case req := <-reqs:
		if req.Op != "sign-request" || !reflect.DeepEqual(req.Secrets, []string{"DEEL_API_KEY", "OTHER_KEY"}) || req.Reason != "why" || req.LifetimeSeconds == nil || *req.LifetimeSeconds != lifetime {
			t.Fatalf("request sent to the helper: %+v", req)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the fake never saw a request")
	}

	sock2, _ := fakeHelper(t, helper.Response{Code: helper.CodeNotASession, Error: "pid 5 is not inside a registered machine session"})
	if _, err := (&helperSigner{sock: sock2}).SignRequestObject(brokeraddr.New("https://secrets.test"), []string{"DEEL_API_KEY"}, "why", nil); err == nil ||
		!strings.Contains(err.Error(), helper.CodeNotASession) {
		t.Fatalf("the helper's code reaches the caller: %v", err)
	}
}

// TestRegisterRefusesWithNoHelperSocket pins the agent box's answer: with
// AGENT_SECRETS_HELPER_SOCK unset and no socket at the default path (a box's runtime dir holds its
// key dir, never a helper), register is a usage error at once, naming the variable and the path,
// rather than waiting out the connect patience for a helper that is not there.
func TestRegisterRefusesWithNoHelperSocket(t *testing.T) {
	runtimeDir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)
	t.Setenv("AGENT_SECRETS_HELPER_SOCK", "")
	var stdout, stderr bytes.Buffer
	start := time.Now()
	code := cmdRegister(nil, &stdout, &stderr)
	if code != exitUsageError {
		t.Fatalf("register with no helper socket: exit %d, want %d; stderr %q", code, exitUsageError, stderr.String())
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("register waited %s for a helper that is not there", elapsed)
	}
	sock := filepath.Join(runtimeDir, "agent-secrets", "helper.sock")
	if msg := stderr.String(); !strings.Contains(msg, "AGENT_SECRETS_HELPER_SOCK is unset") || !strings.Contains(msg, sock) ||
		!strings.Contains(msg, "an agent box has a key dir instead") {
		t.Fatalf("the refusal names the variable, the default path and the box case: %q", msg)
	}
}

// TestRegisterExecWarnsWhenWaitedButNotEnrolled covers the --exec half of "--wait was given and
// state isn't enrolled": the launch still goes ahead (--exec never blocks a launch on the
// broker), but it says so on stderr, since the agent then starts with a session whose secrets
// calls fail until the helper enrolls it. Subprocess: --exec replaces the process image.
func TestRegisterExecWarnsWhenWaitedButNotEnrolled(t *testing.T) {
	binary := buildAgentSecrets(t)
	sock, _ := fakeHelper(t, helper.Response{OK: true, RuntimeID: "h:1:1", State: "enrolling", Error: "waiting on approver"})
	cmd := exec.Command(binary, "register", "--wait", "1", "--exec", "--", "sh", "-c", "echo ran")
	cmd.Env = append(os.Environ(), "AGENT_SECRETS_HELPER_SOCK="+sock)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil || strings.TrimSpace(string(out)) != "ran" {
		t.Fatalf("the command must still run: %q %v (stderr %q)", out, err, stderr.String())
	}
	if !strings.Contains(stderr.String(), "not enrolled") || !strings.Contains(stderr.String(), "waiting on approver") {
		t.Fatalf("a warning names the unfinished enrollment and its last error: %q", stderr.String())
	}

	// Without --wait nothing was asked to finish, so a launch says nothing.
	cmd = exec.Command(binary, "register", "--exec", "--", "sh", "-c", "echo ran")
	cmd.Env = append(os.Environ(), "AGENT_SECRETS_HELPER_SOCK="+sock)
	stderr.Reset()
	cmd.Stderr = &stderr
	if out, err := cmd.Output(); err != nil || strings.TrimSpace(string(out)) != "ran" || stderr.Len() != 0 {
		t.Fatalf("register --exec without --wait: %q %v, stderr %q", out, err, stderr.String())
	}
}

// TestRegisterExecStillPinsTheReplacedProcess lives in hostmode_linux_test.go (//go:build
// linux): it needs fakeHelperWithPids, which pins the connecting pid with helper.PeerOf
// (SO_PEERPIDFD, Linux-only).

// TestRegisterExecWithoutHelperWarnsAndStillExecs waits the client's 10 s connect patience; keep
// it — it is the launch-ordering case tmux-resurrect hits at boot.
func TestRegisterExecWithoutHelperWarnsAndStillExecs(t *testing.T) {
	cmd := exec.Command("go", "run", ".", "register", "--exec", "--", "sh", "-c", "echo ran")
	cmd.Env = append(os.Environ(), "AGENT_SECRETS_HELPER_SOCK="+filepath.Join(shortTempDir(t), "absent.sock"))
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil || strings.TrimSpace(string(out)) != "ran" {
		t.Fatalf("the command must still run: %q %v", out, err)
	}
	if !strings.Contains(stderr.String(), "unreachable") {
		t.Fatalf("a warning names the unreachable helper: %q", stderr.String())
	}
}

// TestWhoamiAlwaysPrintsJSON proves whoami prints the broker's raw GET /v1/enrollments/self JSON
// body even without --json (scripts/agentbox's box-doctor and host-doctor both pipe bare
// `agent-secrets whoami` straight into jq): the same route and shape as self --json. Every field
// but lease_expires_at must match exactly; that one field is fakeBroker's own
// time.Now().Add(...), freshly computed on each of the two separate invocations, so it is
// compared as "present and parseable" rather than byte-equal.
func TestWhoamiAlwaysPrintsJSON(t *testing.T) {
	binary := buildAgentSecrets(t)
	broker, _ := fakeBroker(t)
	defer broker.Close()
	keyDir := newKeyDir(t)

	selfOut, selfErr, selfCode := runAgentSecrets(t, binary, broker.URL, keyDir, nil, "self", "--json")
	whoamiOut, whoamiErr, whoamiCode := runAgentSecrets(t, binary, broker.URL, keyDir, nil, "whoami")

	if selfCode != 0 || whoamiCode != 0 {
		t.Fatalf("exit codes: self=%d (stderr %q) whoami=%d (stderr %q)", selfCode, selfErr, whoamiCode, whoamiErr)
	}
	self, whoami := oneJSONObject(t, selfOut), oneJSONObject(t, whoamiOut)
	if !mapsEqual(keySet(self), keySet(whoami)) {
		t.Fatalf("bare whoami must print self --json's exact key set: self=%v whoami=%v", keySet(self), keySet(whoami))
	}
	for k, v := range self {
		if k == "lease_expires_at" {
			continue
		}
		if !reflect.DeepEqual(whoami[k], v) {
			t.Fatalf("field %q: self=%v whoami=%v", k, v, whoami[k])
		}
	}
	if _, err := time.Parse(time.RFC3339, whoami["lease_expires_at"].(string)); err != nil {
		t.Fatalf("whoami lease_expires_at must parse as RFC3339: %v", err)
	}
}

// TestSignWithoutEnrollmentUsesTheBoxsOwnIdentity proves sign's default path signs a real,
// verifiable proof for the box's own enrollment id, without making a broker call.
func TestSignWithoutEnrollmentUsesTheBoxsOwnIdentity(t *testing.T) {
	keyDir := newKeyDir(t)
	t.Setenv("AGENT_SECRETS_KEY_DIR", keyDir)
	t.Setenv("AGENT_SECRETS_HELPER_SOCK", "")

	var stdout, stderr bytes.Buffer
	const method, url = "GET", "https://broker.example/v1/enrollments/self"
	code := cmdSign([]string{"--method", method, "--url", url}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("sign: exit %d, stderr %q", code, stderr.String())
	}
	compact := strings.TrimSpace(stdout.String())

	key, err := loadKey(filepath.Join(keyDir, "key.pem"), keyDirFile(keyFile))
	if err != nil {
		t.Fatal(err)
	}
	thumb, err := proof.Thumbprint(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	var seenEnrollment string
	verifier := &proof.Verifier{
		Skew: time.Minute,
		Lookup: func(ctx context.Context, enrollmentID string) (string, bool, error) {
			seenEnrollment = enrollmentID
			return thumb, true, nil
		},
		Replay: func(ctx context.Context, jti string, expires time.Time) (bool, error) { return true, nil },
	}
	if _, err := verifier.Verify(context.Background(), compact, method, url, time.Now()); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if seenEnrollment != testEnrollmentID {
		t.Fatalf("signed enrollment id: got %q, want %q", seenEnrollment, testEnrollmentID)
	}
}

// TestSignWithEnrollmentOverridesTheEnrollmentID proves --enrollment signs with this box's own
// key for a caller-supplied id instead of the box's own — the deliberate cross-enrollment
// substitution the broker must refuse.
func TestSignWithEnrollmentOverridesTheEnrollmentID(t *testing.T) {
	keyDir := newKeyDir(t)
	t.Setenv("AGENT_SECRETS_KEY_DIR", keyDir)
	t.Setenv("AGENT_SECRETS_HELPER_SOCK", "")

	const otherEnrollment = "enr-other-box"
	var stdout, stderr bytes.Buffer
	const method, url = "GET", "https://broker.example/v1/enrollments/self"
	code := cmdSign([]string{"--method", method, "--url", url, "--enrollment", otherEnrollment}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("sign --enrollment: exit %d, stderr %q", code, stderr.String())
	}
	compact := strings.TrimSpace(stdout.String())

	key, err := loadKey(filepath.Join(keyDir, "key.pem"), keyDirFile(keyFile))
	if err != nil {
		t.Fatal(err)
	}
	thumb, err := proof.Thumbprint(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	var seenEnrollment string
	verifier := &proof.Verifier{
		Skew: time.Minute,
		Lookup: func(ctx context.Context, enrollmentID string) (string, bool, error) {
			seenEnrollment = enrollmentID
			return thumb, true, nil
		},
		Replay: func(ctx context.Context, jti string, expires time.Time) (bool, error) { return true, nil },
	}
	if _, err := verifier.Verify(context.Background(), compact, method, url, time.Now()); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if seenEnrollment != otherEnrollment {
		t.Fatalf("--enrollment must substitute the signed id: got %q, want %q", seenEnrollment, otherEnrollment)
	}
}

// TestSignEnrollmentRefusedInHelperMode covers "--enrollment works only with a key dir; the
// helper signs only its own enrollment".
func TestSignEnrollmentRefusedInHelperMode(t *testing.T) {
	t.Setenv("AGENT_SECRETS_KEY_DIR", "")
	t.Setenv("AGENT_SECRETS_HELPER_SOCK", "/x/h.sock")
	var stdout, stderr bytes.Buffer
	code := cmdSign([]string{"--method", "GET", "--url", "https://s/v1/enrollments/self", "--enrollment", "enr-x"}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("--enrollment must be refused in helper mode")
	}
	if !strings.Contains(stderr.String(), "helper") {
		t.Fatalf("refusal must explain helper mode signs only its own enrollment: %q", stderr.String())
	}
}

// TestIdentity covers `agent-secrets identity`, the local test every caller that chooses between
// the broker and secretsd asks: exit 0 for a box key dir holding key.pem or a fresh
// enrollment.pending, and for a process the helper's sign op recognizes (OK: enrolled;
// NOT_ENROLLED: enrolling); exit 1 otherwise. A helper that cannot be asked — a socket nothing
// listens on, a socket absent although the helper's unit is installed, or no answer within 2 s —
// is also exit 1, with a notice on stderr; with nothing there at all it is silent. A helper that
// answers with a code this client does not know (one a later helper adds) is exit 1 with a
// notice naming the code. It never prints to stdout, since callers run it in front of a command
// whose stdout is the caller's.
// Every case runs twice: once with the paths named by AGENT_SECRETS_KEY_DIR and
// AGENT_SECRETS_HELPER_SOCK, once with both unset and the files at the $XDG_RUNTIME_DIR defaults.
func TestIdentity(t *testing.T) {
	for _, mode := range []string{"named", "defaults"} {
		for _, tc := range identityCases() {
			t.Run(mode+"/"+tc.name, func(t *testing.T) {
				p := identityPaths(t, mode)
				tc.setup(t, p)

				var stdout, stderr bytes.Buffer
				start := time.Now()
				code := cmdIdentity(nil, &stdout, &stderr)
				elapsed := time.Since(start)
				if code != tc.exit {
					t.Fatalf("exit %d, want %d (stderr %q)", code, tc.exit, stderr.String())
				}
				if stdout.Len() != 0 {
					t.Fatalf("identity printed to stdout: %q", stdout.String())
				}
				want := ""
				if tc.stderr != nil {
					want = tc.stderr(mode, p)
				}
				if want != "" && !strings.Contains(stderr.String(), want) {
					t.Fatalf("stderr %q, want the notice %q", stderr.String(), want)
				}
				if want == "" && stderr.Len() != 0 {
					t.Fatalf("stderr %q, want nothing", stderr.String())
				}
				if tc.slow && (elapsed < 1500*time.Millisecond || elapsed > 4*time.Second) {
					t.Fatalf("a helper that never answers is given up on after about 2 s, took %s", elapsed)
				}
				if !tc.slow && elapsed > 5*time.Second {
					t.Fatalf("identity took %s; only a silent helper may hold it up", elapsed)
				}
			})
		}
	}

	t.Run("arguments", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		if code := cmdIdentity([]string{"extra"}, &stdout, &stderr); code != exitUsageError {
			t.Fatalf("identity with an argument: exit %d, want %d", code, exitUsageError)
		}
	})
}

// TestIdentityWithoutSourceAsksNothing: identity with no flag keeps its contract after the use
// command became a chain of four sources. For every row of TestIdentity it answers the row's exit
// code while a broker that names an exchange audience and a metadata server both stand by, and
// neither is asked anything: it reads neither the sign-in, nor the machine cache, nor the network.
// A process holding only a machine session, in the machine cache or in the key dir where a client
// before #4143 kept one, answers 1.
func TestIdentityWithoutSourceAsksNothing(t *testing.T) {
	cases := append(identityCases(),
		identityCase{name: "only a machine cache", setup: func(t *testing.T, p identityRowPaths) {
			writeSessionFiles(t, filepath.Join(p.state, "agent-secrets", "machine"), "cached-enrollment")
		}, exit: 1},
		identityCase{name: "an old key-dir machine session", setup: func(t *testing.T, p identityRowPaths) {
			writeSessionFiles(t, p.keyDir, "old-machine-enrollment")
			if err := os.WriteFile(filepath.Join(p.keyDir, "machine-session"), nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}, exit: 1},
	)
	for _, mode := range []string{"named", "defaults"} {
		for _, tc := range cases {
			t.Run(mode+"/"+tc.name, func(t *testing.T) {
				p := identityPaths(t, mode)
				broker := newChainBroker(t, storeSettings("gcp", "agent-secrets-vm"))
				t.Setenv("AGENT_SECRETS_URL", broker.URL())
				metadata := startMetadataServer(t, "machine@example.iam.gserviceaccount.com")
				tc.setup(t, p)

				var stdout, stderr bytes.Buffer
				if code := cmdIdentity(nil, &stdout, &stderr); code != tc.exit {
					t.Fatalf("exit %d, want %d (stderr %q)", code, tc.exit, stderr.String())
				}
				if broker.callCount() != 0 || metadata.totalRequests() != 0 {
					t.Fatalf("identity asked the broker %d times and the metadata server %d times; it asks neither", broker.callCount(), metadata.totalRequests())
				}
			})
		}
	}
}

// identityRowPaths is where one identity row's process keeps its state: its home, its key dir and
// helper socket, as the row's mode names them, and its XDG state directory.
type identityRowPaths struct{ home, keyDir, sock, state string }

// identityCase is one row of TestIdentity: what it puts on disk, the exit code identity answers,
// the notice it prints (nil for none; mode is "named" or "defaults"), and whether the row waits
// out a helper that never answers.
type identityCase struct {
	name   string
	setup  func(t *testing.T, p identityRowPaths)
	exit   int
	stderr func(mode string, p identityRowPaths) string
	slow   bool
}

// identityPaths gives one row a home, a runtime directory and a state directory of its own, with
// the key dir and the helper socket named by AGENT_SECRETS_KEY_DIR and AGENT_SECRETS_HELPER_SOCK
// (mode "named") or left at the $XDG_RUNTIME_DIR defaults with both unset (mode "defaults").
func identityPaths(t *testing.T, mode string) identityRowPaths {
	t.Helper()
	runtime := shortTempDir(t)
	p := identityRowPaths{home: t.TempDir(), state: t.TempDir()}
	t.Setenv("HOME", p.home)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_RUNTIME_DIR", runtime)
	t.Setenv("XDG_STATE_HOME", p.state)
	if mode == "named" {
		p.keyDir = filepath.Join(t.TempDir(), "keys")
		p.sock = filepath.Join(shortTempDir(t), "h.sock")
		t.Setenv("AGENT_SECRETS_KEY_DIR", p.keyDir)
		t.Setenv("AGENT_SECRETS_HELPER_SOCK", p.sock)
	} else {
		p.keyDir = filepath.Join(runtime, "agent-secrets")
		p.sock = filepath.Join(runtime, "agent-secrets", "helper.sock")
		t.Setenv("AGENT_SECRETS_KEY_DIR", "")
		t.Setenv("AGENT_SECRETS_HELPER_SOCK", "")
	}
	return p
}

// identityCases is every row of TestIdentity.
func identityCases() []identityCase {
	type paths = identityRowPaths
	unreachable := func(_ string, p paths) string {
		return "agent-secrets: helper unreachable at " + p.sock + "; not an agent session"
	}
	// namedOnly: the notice appears only when AGENT_SECRETS_HELPER_SOCK names the absent socket,
	// since an explicit path says a helper was expected there.
	namedOnly := func(mode string, p paths) string {
		if mode == "named" {
			return unreachable(mode, p)
		}
		return ""
	}
	return []identityCase{
		{name: "key.pem", setup: func(t *testing.T, p paths) { copyKeyDir(t, newKeyDir(t), p.keyDir) }, exit: 0},
		{name: "fresh marker", setup: func(t *testing.T, p paths) { writePendingMarker(t, p.keyDir, 0) }, exit: 0},
		{name: "stale marker", setup: func(t *testing.T, p paths) { writePendingMarker(t, p.keyDir, 200*time.Second) }, exit: 1, stderr: namedOnly},
		{name: "helper OK", setup: func(t *testing.T, p paths) {
			fakeHelperAt(t, p.sock, helper.Response{OK: true, Proof: "eyJ.fake.proof", EnrollmentID: "enr-h"})
		}, exit: 0},
		{name: "helper NOT_ENROLLED", setup: func(t *testing.T, p paths) {
			fakeHelperAt(t, p.sock, helper.Response{Code: helper.CodeNotEnrolled, Error: "not enrolled yet"})
		}, exit: 0},
		{name: "helper NOT_A_SESSION", setup: func(t *testing.T, p paths) {
			fakeHelperAt(t, p.sock, helper.Response{Code: helper.CodeNotASession, Error: "pid 5 is not inside a registered machine session"})
		}, exit: 1},
		{name: "helper answers an unknown code", setup: func(t *testing.T, p paths) {
			fakeHelperAt(t, p.sock, helper.Response{Code: "SOME_LATER_CODE", Error: "a code this client predates"})
		}, exit: 1, stderr: func(string, paths) string {
			return "answered SOME_LATER_CODE: a code this client predates; not an agent session"
		}},
		{name: "nothing listening", setup: func(t *testing.T, p paths) { staleSocketAt(t, p.sock) }, exit: 1, stderr: unreachable},
		{name: "unit installed, no socket", setup: func(t *testing.T, p paths) { installHelperUnit(t, p.home) }, exit: 1, stderr: unreachable},
		{name: "unit under XDG_CONFIG_HOME, no socket", setup: func(t *testing.T, p paths) {
			t.Setenv("XDG_CONFIG_HOME", filepath.Join(p.home, "xdg"))
			installHelperUnit(t, p.home)
			if _, err := os.Stat(filepath.Join(p.home, "xdg", "systemd", "user", "agent-secrets-helper.service")); err != nil {
				t.Fatal(err)
			}
		}, exit: 1, stderr: unreachable},
		{name: "helper never answers", setup: func(t *testing.T, p paths) { silentHelperAt(t, p.sock) }, exit: 1, stderr: unreachable, slow: true},
		{name: "nothing", setup: func(t *testing.T, p paths) {}, exit: 1, stderr: namedOnly},
	}
}

// staleSocketAt leaves a socket file at sock that nothing listens on: the state a helper killed
// outright leaves behind (a clean stop removes the file), which refuses every connection.
func staleSocketAt(t *testing.T, sock string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(sock), 0o700); err != nil {
		t.Fatal(err)
	}
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: sock, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	ln.SetUnlinkOnClose(false)
	ln.Close()
}

// silentHelperAt accepts connections at sock and never answers one.
func silentHelperAt(t *testing.T, sock string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(sock), 0o700); err != nil {
		t.Fatal(err)
	}
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: sock, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	var held []net.Conn
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := ln.AcceptUnix()
			if err != nil {
				return
			}
			held = append(held, conn)
		}
	}()
	t.Cleanup(func() {
		ln.Close()
		<-done
		for _, c := range held {
			c.Close()
		}
	})
}

// installHelperUnit writes the helper's user unit where install writes it under home (the test
// clears XDG_CONFIG_HOME), the file whose presence says the helper is installed on this machine.
func installHelperUnit(t *testing.T, home string) {
	t.Helper()
	unit := helper.UnitPath(func(k string) string {
		return map[string]string{"HOME": home, "XDG_CONFIG_HOME": os.Getenv("XDG_CONFIG_HOME")}[k]
	})
	if err := os.MkdirAll(filepath.Dir(unit), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(unit, []byte("[Service]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestRegisterExecLooksTheCommandUpBeforeItRegisters: `register --exec` with a command that is not
// on PATH exits 127 naming it and asks the helper nothing, so a mistyped command makes no
// enrolment.
func TestRegisterExecLooksTheCommandUpBeforeItRegisters(t *testing.T) {
	binary := buildAgentSecrets(t)
	sock, requests := fakeHelper(t, helper.Response{OK: true, RuntimeID: "h:1:1", State: "enrolled"})
	const missing = "agent-secrets-test-command-on-no-path"
	cmd := exec.Command(binary, "register", "--exec", "--", missing)
	cmd.Env = append(os.Environ(), "AGENT_SECRETS_HELPER_SOCK="+sock)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	err := cmd.Run()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != exitNotFound || !strings.Contains(stderr.String(), missing) {
		t.Fatalf("register --exec %s = %v, stderr %q; want exit %d naming the command", missing, err, stderr.String(), exitNotFound)
	}
	select {
	case req := <-requests:
		t.Fatalf("the helper was asked %+v for a command that is not on PATH; want nothing", req)
	default:
	}
}
