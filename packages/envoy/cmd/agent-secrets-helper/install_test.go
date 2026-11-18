//go:build linux

package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/helper"
	"github.com/sjawhar/envoy/internal/brokerclient"
	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/testwait"
)

// installRig is a temp XDG config dir, a fake systemctl on PATH that appends its argv to a log
// (all but is-active, which prints the unit's state: "active" unless a test writes another to
// the active file, and show, which prints the unit's main process from the pid file, or with
// --property=Job the unit's queued job from the calls log's .job file and nothing without one;
// neither logs anything), and the helper socket path install checks for a running helper. Its
// restart gives the unit a new main process, unless the calls log's .same-process marker exists;
// with .fail-restart it exits 1, and with .slow-restart it writes its process id to the calls
// log's .restart-pid and sleeps an hour in that process, changing no process and leaving job 7
// queued for the unit, as systemd keeps the job of a client killed while it waits, unless
// .no-queued-job exists: only install ending it at its timeout ends it. Its environment names a
// socket and a runtime directory of its own, and runAs refuses to run install against any socket
// outside the rig's directory (foreignSocket).
type installRig struct {
	t      testing.TB
	dir    string
	env    map[string]string
	unit   string
	calls  string
	active string
	pid    string
	sock   string
	exe    string
	stdout bytes.Buffer
}

func newInstallRig(t *testing.T) *installRig {
	t.Helper()
	// TMPDIR may sit behind a symlink; install writes the resolved path, so the rig's binary
	// lives at one. The socket keeps the short unresolved path (unix socket paths are capped).
	dir := shortTempDir(t)
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	r := &installRig{t: t, dir: dir, calls: filepath.Join(dir, "systemctl.log"), active: filepath.Join(dir, "active"), pid: filepath.Join(dir, "main-pid"), sock: filepath.Join(dir, "h.sock"), exe: filepath.Join(real, "agent-secrets-helper")}
	if err := os.WriteFile(r.active, []byte("active\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(r.pid, []byte("4100\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nif [ \"$2\" = is-active ]; then read -r state < " + r.active + "; echo \"$state\"; exit 0; fi\n" +
		"if [ \"$2\" = show ] && [ \"$3\" = --property=Job ]; then if [ -e " + r.calls + ".job ]; then read -r job < " + r.calls + ".job; echo \"$job\"; fi; exit 0; fi\n" +
		"if [ \"$2\" = show ]; then read -r pid < " + r.pid + "; echo \"$pid\"; exit 0; fi\n" +
		"echo \"$*\" >> " + r.calls + "\n" +
		"if [ \"$2\" = restart ] && [ -e " + r.calls + ".fail-restart ]; then exit 1; fi\n" +
		"if [ \"$2\" = restart ] && [ -e " + r.calls + ".slow-restart ]; then [ -e " + r.calls + ".no-queued-job ] || echo 7 > " + r.calls + ".job; echo $$ > " + r.calls + ".restart-pid; exec /bin/sleep 3600; fi\n" +
		"if [ \"$2\" = restart ] && [ ! -e " + r.calls + ".same-process ]; then read -r pid < " + r.pid + "; echo $((pid + 1)) > " + r.pid + "; fi\n"
	if err := os.WriteFile(filepath.Join(bin, "systemctl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(r.exe, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	// XDG_RUNTIME_DIR too, so a test that unsets the socket gets the rig's default socket and not
	// the box's: with neither set, helper.DefaultSocket is /run/user/<uid>/agent-secrets/helper.sock,
	// the socket of the helper this machine runs.
	r.env = map[string]string{"HOME": dir, "XDG_CONFIG_HOME": filepath.Join(dir, "cfg"), "XDG_RUNTIME_DIR": filepath.Join(dir, "run"), "AGENT_SECRETS_HELPER_SOCK": r.sock}
	r.unit = filepath.Join(dir, "cfg", "systemd", "user", "agent-secrets-helper.service")
	return r
}

func (r *installRig) run(args ...string) error {
	r.t.Helper()
	return r.runAs(r.exe, args...)
}

// runAs runs install as the binary at exe in the rig's environment, and fails the test without
// running it when the socket install would reach is not the rig's own (foreignSocket).
func (r *installRig) runAs(exe string, args ...string) error {
	r.t.Helper()
	if err := r.foreignSocket(); err != nil {
		r.t.Fatal(err)
	}
	r.stdout.Reset()
	return install(args, env(r.env), exe, &r.stdout)
}

// foreignSocket names the socket install would reach when that is an absolute path outside the
// rig's directory, and is nil otherwise. install answers the helper there, drains it, and runs
// the rig's systemctl, which restarts nothing; so a drain of the machine's own helper leaves it
// refusing every registration on the box. That happened on 2026-11-16 (dispatch://INFRA-3950):
// the rig named no XDG_RUNTIME_DIR, and a test that unset the socket drained
// /run/user/<uid>/agent-secrets/helper.sock. A relative socket can reach no other user's
// directory and install refuses it before it calls any helper.
func (r *installRig) foreignSocket() error {
	cfg, err := loadConfig(env(r.env))
	if err != nil {
		// install refuses the same environment the same way, before it calls any helper.
		return nil
	}
	if !filepath.IsAbs(cfg.Socket) {
		return nil
	}
	if rel, err := filepath.Rel(r.dir, cfg.Socket); err != nil || !filepath.IsLocal(rel) {
		return fmt.Errorf("install would reach %s, a socket this test did not create under %s", cfg.Socket, r.dir)
	}
	return nil
}

// TestInstallRigRefusesASocketItDidNotCreate: install drains the helper on the socket it reaches,
// and the rig's systemctl restarts nothing, so a rig that reached the machine's own helper would
// leave it refusing every registration on the box (dispatch://INFRA-3950). The rig's own socket,
// and the default socket under its own runtime directory, pass; the default a rig with no runtime
// directory reaches, /run/user/<uid>/agent-secrets/helper.sock, and any other socket outside the
// rig's directory, are refused, naming the socket.
func TestInstallRigRefusesASocketItDidNotCreate(t *testing.T) {
	r := newInstallRig(t)
	if err := r.foreignSocket(); err != nil {
		t.Fatalf("the rig's own socket: %v", err)
	}
	delete(r.env, "AGENT_SECRETS_HELPER_SOCK")
	if err := r.foreignSocket(); err != nil {
		t.Fatalf("the default socket under the rig's own runtime directory: %v", err)
	}
	delete(r.env, "XDG_RUNTIME_DIR")
	machines := helper.DefaultSocket(func(string) string { return "" })
	if err := r.foreignSocket(); err == nil || !strings.Contains(err.Error(), machines) {
		t.Fatalf("foreignSocket with no socket and no runtime directory: %v; want a refusal naming %s", err, machines)
	}
	r.env["AGENT_SECRETS_HELPER_SOCK"] = "/run/user/1/agent-secrets/helper.sock"
	if err := r.foreignSocket(); err == nil || !strings.Contains(err.Error(), "/run/user/1/agent-secrets/helper.sock") {
		t.Fatalf("foreignSocket with another directory's socket: %v; want a refusal naming it", err)
	}
}

func (r *installRig) systemctlCalls(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(r.calls)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

// fakeHelper answers on the rig's socket with the given sessions and returns the ops it was
// asked, in order. With knowsDrain false it answers `drain` as an older helper does.
func (r *installRig) fakeHelper(t *testing.T, sessions []helper.SessionInfo, knowsDrain ...bool) *[]string {
	t.Helper()
	return r.scriptedHelper(t, func(op string) helper.Response {
		if op == "drain" && len(knowsDrain) > 0 && !knowsDrain[0] {
			return helper.Response{Code: helper.CodeBadRequest, Error: "unknown op drain"}
		}
		return helper.Response{OK: true, Sessions: sessions}
	})
}

// scriptedHelper answers each op on the rig's socket with answer(op) and returns the ops it was
// asked, in order.
func (r *installRig) scriptedHelper(t *testing.T, answer func(op string) helper.Response) *[]string {
	t.Helper()
	ln, err := net.Listen("unix", r.sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	ops := &[]string{}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			line, _ := bufio.NewReader(conn).ReadBytes('\n')
			var req helper.Request
			_ = json.Unmarshal(line, &req)
			// install reads each reply before its next call, so this append happens-before the test reads ops.
			*ops = append(*ops, req.Op)
			data, _ := json.Marshal(answer(req.Op))
			_, _ = conn.Write(append(data, '\n'))
			_ = conn.Close()
		}
	}()
	return ops
}

func TestInstallWritesTheUnitForThisBinary(t *testing.T) {
	r := newInstallRig(t)
	if err := r.run("--url", "https://secrets.example"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(r.unit)
	if err != nil {
		t.Fatal(err)
	}
	unit := string(data)
	for _, want := range []string{
		"\nExecStart=" + r.exe + " serve\n",
		"\nEnvironment=AGENT_SECRETS_URL=https://secrets.example\n",
		"\nRestart=always\n", "\nRestartSec=10\n", "\nWantedBy=default.target\n",
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("unit lacks %q:\n%s", want, unit)
		}
	}
	if fi, _ := os.Stat(r.unit); fi.Mode().Perm() != 0o600 {
		t.Errorf("unit mode %v, want 0600: it holds AGENT_SECRETS_URL", fi.Mode().Perm())
	}
	calls := r.systemctlCalls(t)
	if strings.Join(calls, ";") != "--user daemon-reload;--user enable --now agent-secrets-helper.service" {
		t.Fatalf("systemctl calls: %q", calls)
	}
}

func TestInstallResolvesSymlinksAndTakesTheURLFromTheEnvironment(t *testing.T) {
	r := newInstallRig(t)
	link := filepath.Join(filepath.Dir(r.exe), "link")
	if err := os.Symlink(r.exe, link); err != nil {
		t.Fatal(err)
	}
	r.env["AGENT_SECRETS_URL"] = "https://env.example"
	if err := r.runAs(link); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(r.unit)
	if !strings.Contains(string(data), "ExecStart="+r.exe+" serve\n") || !strings.Contains(string(data), "AGENT_SECRETS_URL=https://env.example\n") {
		t.Fatalf("unit:\n%s", data)
	}
}

// TestInstallWritesTheHelpersEnvironmentIntoTheUnit: each helper setting set in install's
// environment (the socket, the operator file, the machine file and the key directory whose
// sign-ins say whose authority a session enrols under, beside the broker URL) becomes an
// Environment= line, so the unit's helper reads the machine file and listens on the socket
// install probed; one that is unset is left out, and a value a unit line would need escaped is
// refused, writing nothing.
func TestInstallWritesTheHelpersEnvironmentIntoTheUnit(t *testing.T) {
	r := newInstallRig(t)
	dir := filepath.Dir(r.sock)
	r.env["AGENT_SECRETS_OPERATOR_FILE"] = filepath.Join(dir, "operator")
	r.env["AGENT_SECRETS_MACHINE_FILE"] = filepath.Join(dir, "machine.json")
	r.env["AGENT_SECRETS_KEY_DIR"] = filepath.Join(dir, "keys")
	if err := r.run("--url", "https://secrets.example"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(r.unit)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"\nEnvironment=AGENT_SECRETS_URL=https://secrets.example\n",
		"\nEnvironment=AGENT_SECRETS_HELPER_SOCK=" + r.sock + "\n",
		"\nEnvironment=AGENT_SECRETS_OPERATOR_FILE=" + filepath.Join(dir, "operator") + "\n",
		"\nEnvironment=AGENT_SECRETS_MACHINE_FILE=" + filepath.Join(dir, "machine.json") + "\n",
		"\nEnvironment=AGENT_SECRETS_KEY_DIR=" + filepath.Join(dir, "keys") + "\n",
	} {
		if !strings.Contains(string(data), want) {
			t.Errorf("unit lacks %q:\n%s", want, data)
		}
	}

	bare := newInstallRig(t)
	delete(bare.env, "AGENT_SECRETS_HELPER_SOCK")
	if err := bare.run("--url", "https://secrets.example"); err != nil {
		t.Fatal(err)
	}
	// Two lines: the URL, and the sign-in directory, which install always writes (unitValue).
	if data, _ := os.ReadFile(bare.unit); strings.Count(string(data), "Environment=") != 2 {
		t.Fatalf("with only the URL set the unit has other Environment= lines:\n%s", data)
	}

	spaced := newInstallRig(t)
	spaced.env["AGENT_SECRETS_MACHINE_FILE"] = filepath.Join(dir, "my machine.json")
	if err := spaced.run("--url", "https://secrets.example"); err == nil || !strings.Contains(err.Error(), "AGENT_SECRETS_MACHINE_FILE") {
		t.Fatalf("install with a spaced machine file: %v; want a refusal naming AGENT_SECRETS_MACHINE_FILE", err)
	}
	if _, err := os.Stat(spaced.unit); !os.IsNotExist(err) {
		t.Fatalf("a refused install wrote the unit: %v", err)
	}
}

// TestInstallWritesTheResolvedSignInDirectoryIntoTheUnit: the sign-in store falls back to
// $XDG_STATE_HOME/agent-secrets (brokerclient.SignIns), and the unit carries neither this
// shell's AGENT_SECRETS_SIGNIN_DIR nor its XDG_STATE_HOME. So install resolves the directory
// and writes it, set or not: copied only when set, a box with a non-default XDG_STATE_HOME has
// `agent-secrets login` write one directory while the unit's helper reads ~/.local/state, and
// every session on it enrols as the machine rather than as the person signed in.
func TestInstallWritesTheResolvedSignInDirectoryIntoTheUnit(t *testing.T) {
	for _, c := range []struct{ name, stateHome, named string }{
		{name: "a state home of its own", stateHome: "state"},
		{name: "no state home at all"},
		{name: "the directory named outright", stateHome: "state", named: "signins"},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newInstallRig(t)
			home := r.env["HOME"]
			want := filepath.Join(home, ".local", "state", "agent-secrets", brokerclient.SignInsDir)
			if c.stateHome != "" {
				r.env["XDG_STATE_HOME"] = filepath.Join(home, c.stateHome)
				want = filepath.Join(home, c.stateHome, "agent-secrets", brokerclient.SignInsDir)
			}
			if c.named != "" {
				r.env[brokerclient.SignInDirVar] = filepath.Join(home, c.named)
				want = filepath.Join(home, c.named)
			}
			if err := r.run("--url", "https://secrets.example"); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(r.unit)
			if err != nil {
				t.Fatal(err)
			}
			line := "\nEnvironment=" + brokerclient.SignInDirVar + "=" + want + "\n"
			if !strings.Contains(string(data), line) {
				t.Errorf("unit lacks %q, so its helper reads another directory than `agent-secrets login` writes:\n%s", line, data)
			}
		})
	}
}

// TestInstallWritesThisMachinesIdentityIntoTheUnit: without these the installed helper proves
// no identity of its own — it falls back to the device-grant machine login, which a broker with
// BROKER_MACHINE_LOGIN_URL unset does not serve — and knows no sign-in client, so it ignores
// every `agent-secrets login` on the box. Each is written when set, with the same escaping and
// absolute-path rules as the helper's own files.
func TestInstallWritesThisMachinesIdentityIntoTheUnit(t *testing.T) {
	r := newInstallRig(t)
	dir := filepath.Dir(r.sock)
	identity := map[string]string{
		brokerclient.IdentityVar:               brokerclient.IdentityPool,
		brokerclient.MachineTokenFileVar:       filepath.Join(dir, "token"),
		brokerclient.MachineAudienceVar:        "https://secrets.example",
		brokerclient.PoolAudienceVar:           "//iam.googleapis.com/projects/1/locations/global/workloadIdentityPools/p/providers/v",
		brokerclient.PoolAccountVar:            "example-fleet@example-project.iam.gserviceaccount.com",
		brokerclient.SignInIssuerVar:           "https://accounts.google.com",
		brokerclient.SignInClientIDVar:         "example-device-client.apps.googleusercontent.com",
		brokerclient.SignInClientSecretFileVar: filepath.Join(dir, "device-client-secret"),
		brokerclient.PersonVar:                 "ada@example.com",
		brokerclient.SessionVar:                "this-box",
	}
	for name, value := range identity {
		r.env[name] = value
	}
	// install loads the helper's own configuration, which reads the device client's secret.
	if err := os.WriteFile(identity[brokerclient.SignInClientSecretFileVar], []byte("example-device-secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := r.run("--url", "https://secrets.example"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(r.unit)
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range identity {
		if want := "\nEnvironment=" + name + "=" + value + "\n"; !strings.Contains(string(data), want) {
			t.Errorf("unit lacks %q:\n%s", want, data)
		}
	}

	spaced := newInstallRig(t)
	spaced.env[brokerclient.PoolAccountVar] = "an account@example.com"
	if err := spaced.run("--url", "https://secrets.example"); err == nil || !strings.Contains(err.Error(), brokerclient.PoolAccountVar) {
		t.Fatalf("install with a spaced pool account: %v; want a refusal naming %s", err, brokerclient.PoolAccountVar)
	}
	if _, err := os.Stat(spaced.unit); !os.IsNotExist(err) {
		t.Fatalf("a refused install wrote the unit: %v", err)
	}

	// An identity's own words are not paths, so a value with no leading slash is no refusal:
	// a pool audience begins `//iam.googleapis.com/`, and a client id and a person never do.
	relative := newInstallRig(t)
	relative.env[brokerclient.IdentityVar] = brokerclient.IdentityMetadata
	relative.env[brokerclient.MachineAudienceVar] = "https://secrets.example"
	if err := relative.run("--url", "https://secrets.example"); err != nil {
		t.Fatalf("install with a metadata identity: %v; want it written, since neither value is a path", err)
	}
}

// TestInstallRefusesARelativeHelperPath: the unit's helper runs in another working directory than
// install, so a relative path in any variable install writes would name another file; install
// refuses it, naming the variable, and writes nothing.
func TestInstallRefusesARelativeHelperPath(t *testing.T) {
	for _, name := range []string{
		"AGENT_SECRETS_HELPER_SOCK", "AGENT_SECRETS_OPERATOR_FILE", "AGENT_SECRETS_MACHINE_FILE",
		brokerclient.MachineTokenFileVar, brokerclient.SignInClientSecretFileVar,
	} {
		r := newInstallRig(t)
		r.env[name] = "relative/" + strings.ToLower(name)
		err := r.run("--url", "https://secrets.example")
		if err == nil || !strings.Contains(err.Error(), name) || !strings.Contains(err.Error(), "absolute") {
			t.Errorf("install with a relative %s: %v; want a refusal naming it and asking for an absolute path", name, err)
		}
		if _, err := os.Stat(r.unit); !os.IsNotExist(err) {
			t.Errorf("an install refused for a relative %s wrote the unit: %v", name, err)
		}
	}
}

func TestInstallIsANoOpWhenNothingChanged(t *testing.T) {
	r := newInstallRig(t)
	if err := r.run("--url", "https://secrets.example"); err != nil {
		t.Fatal(err)
	}
	before, _ := os.Stat(r.unit)
	_ = os.Remove(r.calls)
	r.fakeHelper(t, nil)
	if err := r.run("--url", "https://secrets.example"); err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(r.unit)
	if !after.ModTime().Equal(before.ModTime()) || !os.SameFile(before, after) {
		t.Error("the unchanged unit was rewritten")
	}
	if calls := r.systemctlCalls(t); calls != nil {
		t.Fatalf("systemctl ran on an unchanged unit: %q", calls)
	}
}

// TestInstallNarrowsAnUnchangedUnitAnOlderInstallLeftReadable: a unit an older install wrote
// 0644 holds the same broker URL; a re-run with nothing changed leaves it 0600 and runs nothing.
func TestInstallNarrowsAnUnchangedUnitAnOlderInstallLeftReadable(t *testing.T) {
	r := newInstallRig(t)
	if err := r.run("--url", "https://secrets.example"); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(r.unit, 0o644); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(r.calls)
	r.fakeHelper(t, nil)
	if err := r.run("--url", "https://secrets.example"); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(r.unit); fi.Mode().Perm() != 0o600 {
		t.Errorf("unit mode %v, want 0600", fi.Mode().Perm())
	}
	if calls := r.systemctlCalls(t); calls != nil {
		t.Fatalf("systemctl ran on an unchanged unit: %q", calls)
	}
}

func TestInstallDrainsAndRestartsAHelperWithNoSessions(t *testing.T) {
	r := newInstallRig(t)
	ops := r.fakeHelper(t, nil)
	if err := r.run("--url", "https://secrets.example"); err != nil {
		t.Fatal(err)
	}
	calls := r.systemctlCalls(t)
	if len(calls) != 3 || calls[2] != "--user restart agent-secrets-helper.service" {
		t.Fatalf("systemctl calls: %q", calls)
	}
	if strings.Join(*ops, ",") != "sessions,drain" {
		t.Fatalf("ops asked: %q, want the running check then drain", *ops)
	}
}

// TestInstallNeverRestartsAHelperThatCannotDrain: a helper without the drain op cannot refuse a
// register between install's check and the restart, so install lists its sessions (none here, or
// two) and says to restart at a quiet moment, never restarting it itself.
func TestInstallNeverRestartsAHelperThatCannotDrain(t *testing.T) {
	const notice = "not restarted: the running agent-secrets-helper predates drain, so a session could start between the check and the restart. Re-run with --restart-with-sessions at a quiet moment.\n"
	for _, tc := range []struct {
		name     string
		sessions []helper.SessionInfo
		want     string
	}{
		{name: "no sessions", want: notice},
		{name: "sessions", sessions: []helper.SessionInfo{{PID: 41, RuntimeID: "rt-a"}, {PID: 42, RuntimeID: "rt-b"}}, want: "41\trt-a\t\t\n42\trt-b\t\t\n" + notice},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newInstallRig(t)
			ops := r.fakeHelper(t, tc.sessions, false)
			if err := r.run("--url", "https://secrets.example"); err != nil {
				t.Fatal(err)
			}
			for _, c := range r.systemctlCalls(t) {
				if strings.Contains(c, "restart") {
					t.Fatalf("restarted a helper that cannot drain: %q", c)
				}
			}
			if strings.Join(*ops, ",") != "sessions,drain,sessions" {
				t.Fatalf("ops asked: %q", *ops)
			}
			if out := r.stdout.String(); out != tc.want {
				t.Fatalf("output:\n%s\nwant:\n%s", out, tc.want)
			}
		})
	}
}

// TestInstallDoesNotRestartAHelperThatWasNotRunning: enable --now starts the new unit, so no
// restart follows.
func TestInstallDoesNotRestartAHelperThatWasNotRunning(t *testing.T) {
	r := newInstallRig(t)
	if err := r.run("--url", "https://secrets.example"); err != nil {
		t.Fatal(err)
	}
	for _, c := range r.systemctlCalls(t) {
		if strings.Contains(c, "restart") {
			t.Fatalf("restarted a helper that was not running: %q", c)
		}
	}
}

func TestInstallNeverRestartsAHelperWithSessions(t *testing.T) {
	r := newInstallRig(t)
	r.fakeHelper(t, []helper.SessionInfo{{PID: 41, RuntimeID: "rt-a"}, {PID: 42, RuntimeID: "rt-b"}})
	if err := r.run("--url", "https://secrets.example"); err != nil {
		t.Fatal(err)
	}
	for _, c := range r.systemctlCalls(t) {
		if strings.Contains(c, "restart") {
			t.Fatalf("restarted a helper with sessions: %q", c)
		}
	}
	out := r.stdout.String()
	if !strings.Contains(out, "41\trt-a") || !strings.Contains(out, "42\trt-b") ||
		!strings.Contains(out, "not restarted: 2 sessions are registered; a restart ends their grants. Re-run with --restart-with-sessions.") {
		t.Fatalf("output:\n%s", out)
	}
}

func TestInstallRestartsWithSessionsWhenTold(t *testing.T) {
	r := newInstallRig(t)
	r.fakeHelper(t, []helper.SessionInfo{{PID: 41, RuntimeID: "rt-a"}})
	if err := r.run("--url", "https://secrets.example", "--restart-with-sessions"); err != nil {
		t.Fatal(err)
	}
	calls := r.systemctlCalls(t)
	if len(calls) != 3 || calls[2] != "--user restart agent-secrets-helper.service" {
		t.Fatalf("systemctl calls: %q", calls)
	}
}

func TestInstallRefusesWithoutAURL(t *testing.T) {
	r := newInstallRig(t)
	err := r.run()
	if err == nil || !strings.Contains(err.Error(), "--url") || !strings.Contains(err.Error(), "AGENT_SECRETS_URL") {
		t.Fatalf("install without a URL: %v", err)
	}
	if _, statErr := os.Stat(r.unit); statErr == nil {
		t.Fatal("wrote a unit without a URL")
	}
	if err := r.run("--url", "https://a b"); err == nil {
		t.Fatal("accepted a URL with whitespace")
	}
}

// TestInstallRefusesAnEscapedURLWithoutNamingItsCredential: a password with a character a URL
// must percent-escape trips the unit's escaping check, and the refusal names the URL redacted.
func TestInstallRefusesAnEscapedURLWithoutNamingItsCredential(t *testing.T) {
	r := newInstallRig(t)
	err := r.run("--url", "https://tokuser9f3:hunter2pw%40x@broker.invalid")
	if err == nil || !strings.Contains(err.Error(), redact.NotPrinted) {
		t.Fatalf("install: %v; want a refusal naming the URL as not printed", err)
	}
	if strings.Contains(err.Error(), "hunter2pw") || strings.Contains(err.Error(), "tokuser9f3") {
		t.Fatalf("install refusal names the URL's credential: %v", err)
	}
}

// TestInstallRefusesAnExecutablePathSystemdWouldSplit: ExecStart splits on whitespace and expands
// specifiers, so install refuses such a path rather than escaping it.
func TestInstallRefusesAnExecutablePathSystemdWouldSplit(t *testing.T) {
	r := newInstallRig(t)
	spaced := filepath.Join(filepath.Dir(r.exe), "my helper")
	if err := os.WriteFile(spaced, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := r.runAs(spaced, "--url", "https://s"); err == nil || !strings.Contains(err.Error(), spaced) {
		t.Fatalf("install from %q: %v", spaced, err)
	}
}

// TestInstallRefusesAHelperItsUnitDoesNotRun: a helper answers on the socket while the unit is
// not active, so something else started it; enabling the unit would start a second helper on
// its socket. install lists that helper's sessions, says to stop it, and writes and starts
// nothing.
func TestInstallRefusesAHelperItsUnitDoesNotRun(t *testing.T) {
	r := newInstallRig(t)
	if err := os.WriteFile(r.active, []byte("inactive\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ops := r.fakeHelper(t, []helper.SessionInfo{{PID: 41, RuntimeID: "rt-a"}})
	err := r.run("--url", "https://secrets.example")
	if err == nil || !strings.Contains(err.Error(), "not run by agent-secrets-helper.service") || !strings.Contains(err.Error(), r.sock) ||
		!strings.Contains(err.Error(), "stop it, then re-run install") {
		t.Fatalf("install beside an unmanaged helper: %v", err)
	}
	if !strings.Contains(r.stdout.String(), "41\trt-a") {
		t.Fatalf("output %q; want the running helper's sessions", r.stdout.String())
	}
	if _, statErr := os.Stat(r.unit); statErr == nil {
		t.Fatal("wrote the unit beside an unmanaged helper")
	}
	if calls := r.systemctlCalls(t); calls != nil {
		t.Fatalf("systemctl ran beside an unmanaged helper: %q", calls)
	}
	if strings.Join(*ops, ",") != "sessions" {
		t.Fatalf("ops asked: %q; want only the running check", *ops)
	}
}

// TestInstallUndrainsWhenTheRestartFails: a drain found no sessions, so the helper refuses new
// ones, and then the restart failed: install sends undrain, so the running helper takes sessions
// again, and returns the restart's error.
func TestInstallUndrainsWhenTheRestartFails(t *testing.T) {
	r := newInstallRig(t)
	if err := os.WriteFile(r.calls+".fail-restart", nil, 0o644); err != nil {
		t.Fatal(err)
	}
	ops := r.fakeHelper(t, nil)
	err := r.run("--url", "https://secrets.example")
	if err == nil || !strings.Contains(err.Error(), "systemctl --user restart agent-secrets-helper.service") {
		t.Fatalf("install with a failing restart: %v; want the restart's error", err)
	}
	if strings.Join(*ops, ",") != "sessions,drain,undrain" {
		t.Fatalf("ops asked: %q; want the running check, drain, then undrain", *ops)
	}
}

// TestInstallUndrainsWhenTheRestartLeavesTheSameMainProcess: systemctl's restart answers success
// but the unit's main process is the one it was before, so the restart reached no process of the
// helper install drained: install sends undrain, so that helper takes sessions again, and returns
// an error saying the helper was not restarted. Before, install returned nil and left the helper
// refusing every registration (dispatch://INFRA-3950).
func TestInstallUndrainsWhenTheRestartLeavesTheSameMainProcess(t *testing.T) {
	r := newInstallRig(t)
	if err := os.WriteFile(r.calls+".same-process", nil, 0o644); err != nil {
		t.Fatal(err)
	}
	ops := r.fakeHelper(t, nil)
	err := r.run("--url", "https://secrets.example")
	if err == nil || !strings.Contains(err.Error(), "main process is still 4100") {
		t.Fatalf("install whose restart left the main process: %v; want an error naming the unchanged process 4100", err)
	}
	if strings.Join(*ops, ",") != "sessions,drain,undrain" {
		t.Fatalf("ops asked: %q; want the running check, drain, then undrain", *ops)
	}
}

// TestInstallUndrainsWhenTheRestartOutlivesItsTimeout: a restart still running at its timeout
// (helper.InstallRestartTimeout) is one install stops waiting for and ends; with no job left
// queued for the unit and its main process unchanged, install sends undrain and returns an error
// naming the timeout. The rig's restart would run an hour, so install answering at all, once its
// clock has passed the timeout, is install giving up at it.
func TestInstallUndrainsWhenTheRestartOutlivesItsTimeout(t *testing.T) {
	r := newInstallRig(t)
	for _, marker := range []string{".slow-restart", ".no-queued-job"} {
		if err := os.WriteFile(r.calls+marker, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ops := r.fakeHelper(t, nil)
	err := r.runPastTheRestartTimeout(t, "--url", "https://secrets.example")
	if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("did not finish within %s", restartTimeout)) {
		t.Fatalf("install whose restart outlived its timeout: %v; want an error naming the %s timeout", err, restartTimeout)
	}
	if strings.Join(*ops, ",") != "sessions,drain,undrain" {
		t.Fatalf("ops asked: %q; want the running check, drain, then undrain", *ops)
	}
}

// TestInstallLeavesTheDrainWhileItsTimedOutRestartIsQueued: ending systemctl at the timeout
// leaves systemd's restart job queued for the unit. An undrain then would let a session register
// on the helper that job is about to stop, and lose its grants when it does, which is what the
// drain is for; so install sends no undrain while a job for the unit is queued, says so, and
// leaves the drain to end with the restart or at the helper's own bound.
func TestInstallLeavesTheDrainWhileItsTimedOutRestartIsQueued(t *testing.T) {
	r := newInstallRig(t)
	if err := os.WriteFile(r.calls+".slow-restart", nil, 0o644); err != nil {
		t.Fatal(err)
	}
	ops := r.fakeHelper(t, nil)
	err := r.runPastTheRestartTimeout(t, "--url", "https://secrets.example")
	if strings.Join(*ops, ",") != "sessions,drain" {
		t.Fatalf("ops asked: %q; want the running check and drain, and no undrain while job 7 is queued", *ops)
	}
	if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("did not finish within %s", restartTimeout)) || !strings.Contains(err.Error(), "job 7") {
		t.Fatalf("install whose timed-out restart is still queued: %v; want an error naming the %s timeout and the queued job 7", err, restartTimeout)
	}
}

// runPastTheRestartTimeout runs install with args, its restart's timeout on a still clock
// (restartClock), and moves that clock past the timeout once install has armed it and the rig's
// slow restart is running. It answers install's error, and fails the test unless install armed
// that timeout alone, restartTimeout from where the clock stood, and ended the restart's process:
// the rig's restart sleeps an hour, so one still running when install answered is one install
// left behind. That process is the rig's own, ended in the test's cleanup by its exact process id
// should it still run then, whatever the test failed on.
func (r *installRig) runPastTheRestartTimeout(t *testing.T, args ...string) error {
	t.Helper()
	still := clocktest.New(time.Now())
	previous := restartClock
	restartClock = still
	t.Cleanup(func() { restartClock = previous })
	started := still.Now()
	if err := r.foreignSocket(); err != nil {
		t.Fatal(err)
	}
	// Registered before install starts, so a row that fails anywhere leaves no restart behind: a
	// process the rig's systemctl left holds the test binary's stderr, and go test waits for it.
	t.Cleanup(func() {
		if pid, _ := r.restartPID(); restartRuns(pid) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})
	r.stdout.Reset()
	answered := make(chan error, 1)
	go func() { answered <- install(args, env(r.env), r.exe, &r.stdout) }()

	var restart int
	var early error
	returned := false
	testwait.Until(t, "install to arm its restart's timeout and the rig's restart to be running, or install's answer", func() (string, bool) {
		select {
		case early = <-answered:
			returned = true
			return "install answered", true
		default:
		}
		pid, err := r.restartPID()
		if err != nil {
			t.Fatalf("read the slow restart's process id: %v", err)
		}
		restart = pid
		armed := still.Armed()
		return fmt.Sprintf("armed %v, restart process %d", armed, restart), len(armed) > 0 && restart != 0
	})
	if returned {
		t.Fatalf("install answered %v before it armed its restart's timeout and ran the restart", early)
	}
	if armed := still.Armed(); len(armed) != 1 || !armed[0].Equal(started.Add(restartTimeout)) {
		t.Fatalf("install armed %v for its restart, want its timeout alone, %s from %v", armed, restartTimeout, started)
	}
	still.Advance(restartTimeout)
	err := testwait.Receive(t, "install's answer once its restart's timeout had passed", answered)
	if restartRuns(restart) {
		t.Fatalf("install answered %v with its restart (process %d) still running: it did not end the restart at its timeout", err, restart)
	}
	return err
}

// restartPID is the process id the rig's slow restart wrote, 0 before it has written one.
func (r *installRig) restartPID() (int, error) {
	written, err := os.ReadFile(r.calls + ".restart-pid")
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(written)))
	if err != nil {
		return 0, nil
	}
	return pid, nil
}

// restartRuns reports whether process pid is the rig's slow restart, still running: the sleep the
// rig's systemctl execs, read from the process's own command line, so no other process that
// later took the same id is ever taken for it. install waits for the systemctl it ends, so a
// restart install ended is gone by the time install answers.
func restartRuns(pid int) bool {
	if pid <= 0 {
		return false
	}
	cmdline, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	return err == nil && string(cmdline) == "/bin/sleep\x003600\x00"
}

// TestInstallNeverRestartsWhenASessionArrivesBeforeTheDrain: the running check finds no sessions,
// but one registers before the drain, which finds it: install does not restart, and prints the
// session and why.
func TestInstallNeverRestartsWhenASessionArrivesBeforeTheDrain(t *testing.T) {
	r := newInstallRig(t)
	r.scriptedHelper(t, func(op string) helper.Response {
		if op == "drain" {
			return helper.Response{OK: true, Sessions: []helper.SessionInfo{{PID: 43, RuntimeID: "rt-late"}}}
		}
		return helper.Response{OK: true}
	})
	if err := r.run("--url", "https://secrets.example"); err != nil {
		t.Fatal(err)
	}
	for _, c := range r.systemctlCalls(t) {
		if strings.Contains(c, "restart") {
			t.Fatalf("restarted a helper whose drain found a session: %q", c)
		}
	}
	out := r.stdout.String()
	if !strings.Contains(out, "43\trt-late") || !strings.Contains(out, "not restarted: 1 sessions are registered") {
		t.Fatalf("output:\n%s", out)
	}
}

// TestInstallRestartsOnTheReRunItAsksFor: an install that changes the unit while sessions are
// registered does not restart and says to re-run with --restart-with-sessions; that re-run, the
// unit now unchanged, restarts the running helper.
func TestInstallRestartsOnTheReRunItAsksFor(t *testing.T) {
	r := newInstallRig(t)
	r.fakeHelper(t, []helper.SessionInfo{{PID: 41, RuntimeID: "rt-a"}})
	if err := r.run("--url", "https://secrets.example"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.stdout.String(), "Re-run with --restart-with-sessions.") {
		t.Fatalf("output:\n%s", r.stdout.String())
	}
	for _, c := range r.systemctlCalls(t) {
		if strings.Contains(c, "restart") {
			t.Fatalf("restarted a helper with sessions: %q", c)
		}
	}
	_ = os.Remove(r.calls)
	if err := r.run("--url", "https://secrets.example", "--restart-with-sessions"); err != nil {
		t.Fatal(err)
	}
	if calls := r.systemctlCalls(t); strings.Join(calls, ";") != "--user restart agent-secrets-helper.service" {
		t.Fatalf("systemctl calls on the re-run: %q; want only the restart", calls)
	}
}

// TestInstallRefusesAnUnmanagedHelperWhenTheUnitMatches: the unit file already holds the bytes
// install would write, but the unit is not active and a helper answers on the socket: install
// still refuses, naming the socket, and runs nothing.
func TestInstallRefusesAnUnmanagedHelperWhenTheUnitMatches(t *testing.T) {
	r := newInstallRig(t)
	if err := r.run("--url", "https://secrets.example"); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(r.calls)
	if err := os.WriteFile(r.active, []byte("inactive\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r.fakeHelper(t, []helper.SessionInfo{{PID: 41, RuntimeID: "rt-a"}})
	err := r.run("--url", "https://secrets.example")
	if err == nil || !strings.Contains(err.Error(), "not run by agent-secrets-helper.service") || !strings.Contains(err.Error(), r.sock) {
		t.Fatalf("install beside an unmanaged helper, the unit unchanged: %v", err)
	}
	if calls := r.systemctlCalls(t); calls != nil {
		t.Fatalf("systemctl ran beside an unmanaged helper: %q", calls)
	}
}

// TestInstallStartsAnUnchangedUnitThatIsNotActive: the unit file matches but the unit is not
// running and nothing answers on the socket: install enables and starts it.
func TestInstallStartsAnUnchangedUnitThatIsNotActive(t *testing.T) {
	r := newInstallRig(t)
	if err := r.run("--url", "https://secrets.example"); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(r.calls)
	if err := os.WriteFile(r.active, []byte("inactive\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := r.run("--url", "https://secrets.example"); err != nil {
		t.Fatal(err)
	}
	if calls := r.systemctlCalls(t); strings.Join(calls, ";") != "--user daemon-reload;--user enable --now agent-secrets-helper.service" {
		t.Fatalf("systemctl calls: %q", calls)
	}
}
