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
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/helper"
	"github.com/sjawhar/envoy/internal/cipherclient"
	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/testwait"
	"github.com/sjawhar/envoy/testexec"
)

// installRig is a temp XDG config dir, a fake systemctl on PATH that appends its argv to a log
// (all but is-active, which prints the unit's state: "active" unless a test writes another to
// the active file, and show, which prints the unit's main process from the pid file, or with
// --property=Job the unit's queued job from the calls log's .job file and nothing without one;
// neither logs anything), and the helper socket path install checks for a running helper. Every
// call of it, is-active and show included, first records its own start in the calls log's
// .starts file (systemctlStart). Its restart gives the unit a new main process, unless the calls
// log's .same-process marker exists; with .fail-restart it exits 1. With .slow-restart it queues
// job 7 for the unit, as systemd keeps the job of a client killed while it waits, unless
// .no-queued-job exists: it writes .job, then the mark .queued once .job is whole. It then changes
// no process and holds until the rig's release file exists or this test process is gone: never
// for a span of its own, so no restart ends by itself before install stops waiting for it
// (releaseRestartAtGiveUp). Its environment names a socket and a runtime directory of its own, and
// runAs refuses to run install against any socket outside the rig's directory (foreignSocket).
type installRig struct {
	t       testing.TB
	dir     string
	env     map[string]string
	unit    string
	calls   string
	active  string
	pid     string
	sock    string
	exe     string
	release string
	stdout  bytes.Buffer
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
	r := &installRig{t: t, dir: dir, calls: filepath.Join(dir, "systemctl.log"), active: filepath.Join(dir, "active"), pid: filepath.Join(dir, "main-pid"), sock: filepath.Join(dir, "h.sock"), exe: filepath.Join(real, "agent-secrets-helper"), release: filepath.Join(dir, "release")}
	t.Cleanup(func() {
		if err := os.WriteFile(r.release, nil, 0o644); err != nil {
			t.Errorf("release the rig's held restart: %v", err)
		}
	})
	if err := os.WriteFile(r.active, []byte("active\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(r.pid, []byte("4100\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The first line is the call's start record (systemctlStart): /proc/uptime as it runs, its own
	// /proc/<pid>/stat line whole, which says when the kernel started it, and its arguments, by tabs.
	script := "#!/bin/sh\nread -r up _ < /proc/uptime; read -r stat < /proc/$$/stat; printf '%s\\t%s\\t%s\\n' \"$up\" \"$stat\" \"$*\" >> " + r.calls + ".starts\n" +
		"if [ \"$2\" = is-active ]; then read -r state < " + r.active + "; echo \"$state\"; exit 0; fi\n" +
		"if [ \"$2\" = show ] && [ \"$3\" = --property=Job ]; then if [ -e " + r.calls + ".job ]; then read -r job < " + r.calls + ".job; echo \"$job\"; fi; exit 0; fi\n" +
		"if [ \"$2\" = show ]; then read -r pid < " + r.pid + "; echo \"$pid\"; exit 0; fi\n" +
		"echo \"$*\" >> " + r.calls + "\n" +
		"if [ \"$2\" = restart ] && [ -e " + r.calls + ".fail-restart ]; then exit 1; fi\n" +
		"if [ \"$2\" = restart ] && [ -e " + r.calls + ".slow-restart ]; then [ -e " + r.calls + ".no-queued-job ] || { echo 7 > " + r.calls + ".job; : > " + r.calls + ".queued; }\n" +
		"  while [ ! -e " + r.release + " ] && kill -0 " + strconv.Itoa(os.Getpid()) + " 2>/dev/null; do /bin/sleep 0.05; done; exit 0; fi\n" +
		"if [ \"$2\" = restart ] && [ ! -e " + r.calls + ".same-process ]; then read -r pid < " + r.pid + "; echo $((pid + 1)) > " + r.pid + "; fi\n"
	if err := testexec.WriteFile(filepath.Join(bin, "systemctl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := testexec.WriteFile(r.exe, []byte("binary"), 0o755); err != nil {
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
// $XDG_STATE_HOME/agent-secrets (cipherclient.SignIns), and the unit carries neither this
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
			want := filepath.Join(home, ".local", "state", "agent-secrets", cipherclient.SignInsDir)
			if c.stateHome != "" {
				r.env["XDG_STATE_HOME"] = filepath.Join(home, c.stateHome)
				want = filepath.Join(home, c.stateHome, "agent-secrets", cipherclient.SignInsDir)
			}
			if c.named != "" {
				r.env[cipherclient.SignInDirVar] = filepath.Join(home, c.named)
				want = filepath.Join(home, c.named)
			}
			if err := r.run("--url", "https://secrets.example"); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(r.unit)
			if err != nil {
				t.Fatal(err)
			}
			line := "\nEnvironment=" + cipherclient.SignInDirVar + "=" + want + "\n"
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
		cipherclient.IdentityVar:               cipherclient.IdentityPool,
		cipherclient.MachineTokenFileVar:       filepath.Join(dir, "token"),
		cipherclient.MachineAudienceVar:        "https://secrets.example",
		cipherclient.PoolAudienceVar:           "//iam.googleapis.com/projects/1/locations/global/workloadIdentityPools/p/providers/v",
		cipherclient.PoolAccountVar:            "example-fleet@example-project.iam.gserviceaccount.com",
		cipherclient.SignInIssuerVar:           "https://accounts.google.com",
		cipherclient.SignInClientIDVar:         "example-device-client.apps.googleusercontent.com",
		cipherclient.SignInClientSecretFileVar: filepath.Join(dir, "device-client-secret"),
		cipherclient.PersonVar:                 "ada@example.com",
		cipherclient.SessionVar:                "this-box",
	}
	for name, value := range identity {
		r.env[name] = value
	}
	// install loads the helper's own configuration, which reads the device client's secret.
	if err := os.WriteFile(identity[cipherclient.SignInClientSecretFileVar], []byte("example-device-secret"), 0o600); err != nil {
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
	spaced.env[cipherclient.PoolAccountVar] = "an account@example.com"
	if err := spaced.run("--url", "https://secrets.example"); err == nil || !strings.Contains(err.Error(), cipherclient.PoolAccountVar) {
		t.Fatalf("install with a spaced pool account: %v; want a refusal naming %s", err, cipherclient.PoolAccountVar)
	}
	if _, err := os.Stat(spaced.unit); !os.IsNotExist(err) {
		t.Fatalf("a refused install wrote the unit: %v", err)
	}

	// An identity's own words are not paths, so a value with no leading slash is no refusal:
	// a pool audience begins `//iam.googleapis.com/`, and a client id and a person never do.
	relative := newInstallRig(t)
	relative.env[cipherclient.IdentityVar] = cipherclient.IdentityMetadata
	relative.env[cipherclient.MachineAudienceVar] = "https://secrets.example"
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
		cipherclient.MachineTokenFileVar, cipherclient.SignInClientSecretFileVar,
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
	err := r.run("--url", userinfoURL("https", "tokuser9f3", "hunter2pw%40x", "broker.invalid"))
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
	if err := testexec.WriteFile(spaced, []byte("binary"), 0o755); err != nil {
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
// (helper.InstallRestartTimeout) is one install stops waiting for; with no job left queued for
// the unit and its main process unchanged, install sends undrain and returns an error naming the
// timeout, well before the restart would have returned. The restart's systemctl holds until the
// test releases it (releaseRestartAtGiveUp), so an install that returns only after that release
// waited for the restart to return.
//
// Install returns within installReturnsWithin of its start, less the time the machine held some
// of its systemctl calls between install's exec of each and the call's first line
// (systemctlStart.held): the kernel and the stand-in spend that time, and a loaded machine can
// stretch it past any bound. Only outlivedRestartCalls of the calls but the restart are taken off,
// the number a correct install makes here, and where install makes more, the ones the machine
// held least. Install chooses how many calls it makes, so a call past that number, held time and
// all, is install's own, and so is every other part of the span: an install that spends past the
// bound, on more calls or anywhere else, fails as it failed main's. A number the row fixes also
// keeps the half tick each held time reads high under gVisor (testexec.ClockTick) from growing
// with the calls install adds. The restart's own call is not counted: its timeout runs from
// before install execs it, so a late start of it holds install up no longer.
func TestInstallUndrainsWhenTheRestartOutlivesItsTimeout(t *testing.T) {
	r := newInstallRig(t)
	shortenRestartTimeout(t)
	for _, marker := range []string{".slow-restart", ".no-queued-job"} {
		if err := os.WriteFile(r.calls+marker, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ops := r.fakeHelper(t, nil)
	released := r.releaseRestartAtGiveUp(t)
	started := time.Now()
	err := r.run("--url", "https://secrets.example")
	took := time.Since(started)
	if released() {
		t.Fatalf("install returned %v only once the test released its restart: it waited for a restart its timeout is 200ms", err)
	}
	var helds []time.Duration
	for _, call := range r.starts() {
		if !call.restart() {
			helds = append(helds, call.held())
		}
	}
	slices.Sort(helds)
	taken := helds[:min(len(helds), outlivedRestartCalls)]
	var held time.Duration
	for _, h := range taken {
		held += h
	}
	if own := took - held; own > installReturnsWithin {
		t.Fatalf("install took %s of its own, past %s, over a restart its timeout is 200ms: it returned %s after its start, of which the machine held %d of its calls but the restart %s between install's exec of each and its first line", own, installReturnsWithin, took, len(taken), held)
	} else if took > installReturnsWithin {
		t.Logf("install returned %s after its start, past %s, of which the machine held %d of its calls but the restart %s between install's exec of each and its first line: %s of its own", took, installReturnsWithin, len(taken), held, own)
	}
	if err == nil || !strings.Contains(err.Error(), "did not finish within 200ms") {
		t.Fatalf("install whose restart outlived its timeout: %v; want an error naming the 200ms timeout", err)
	}
	if strings.Join(*ops, ",") != "sessions,drain,undrain" {
		t.Fatalf("ops asked: %q; want the running check, drain, then undrain", *ops)
	}
}

// installReturnsWithin is how soon after its start an install whose restart outlives its 200 ms
// timeout returns, less the time the machine held its systemctl calls before their first lines:
// its reads, its drain, the timeout, the kill, two reads of the unit and the undrain, each a
// stand-in that answers at once. An install that waited on the restart past its timeout returns
// past it.
const installReturnsWithin = 10 * time.Second

// outlivedRestartCalls is how many systemctl calls but the restart a correct install makes in
// TestInstallUndrainsWhenTheRestartOutlivesItsTimeout: it checks the unit is active, reloads and
// enables the unit it wrote, reads the main process before the restart and after it, and reads the
// unit's job.
const outlivedRestartCalls = 6

// TestInstallLeavesTheDrainWhileItsTimedOutRestartIsQueued: killing systemctl at the timeout
// leaves systemd's restart job queued for the unit. An undrain then would let a session register
// on the helper that job is about to stop, and lose its grants when it does, which is what the
// drain is for; so install sends no undrain while a job for the unit is queued, says so, and
// leaves the drain to end with the restart or at the helper's own bound.
//
// Install gives its restart the whole timeout, so the unit read it runs after the restart begins
// that timeout or more after the read before the restart ran its first line (restartSpan): a run
// whose span is shorter fails, whatever it queued. The timeout's clock starts before install
// execs the restart's systemctl, so a loaded machine that holds that systemctl before its first
// line can let the timeout kill it before it has queued its job; the kill then leaves no job
// queued, and that run shows nothing of a queued one. The rig's systemctl records that hold,
// which install cannot cause (systemctlStart.held), so a timed-out run with no job queued is
// taken again, on a rig of its own and within a minute, only where a systemctl call of that run
// shows the machine held it half the timeout or more. Any other run with no job queued is judged.
func TestInstallLeavesTheDrainWhileItsTimedOutRestartIsQueued(t *testing.T) {
	shortenRestartTimeout(t)
	within := testwait.Bounded(t, time.Minute, installSpare)
	until := time.Now().Add(within)
	for runs := 1; ; runs++ {
		r := newInstallRig(t)
		if err := os.WriteFile(r.calls+".slow-restart", nil, 0o644); err != nil {
			t.Fatal(err)
		}
		ops := r.fakeHelper(t, nil)
		released := r.releaseRestartAtGiveUp(t)
		err := r.run("--url", "https://secrets.example")
		if released() {
			t.Fatalf("run %d: install returned %v only once the test released its restart: it waited for a restart its timeout is 200ms", runs, err)
		}
		starts := r.starts()
		if span, ok := restartSpan(starts); ok && span+2*testexec.ClockTick < restartTimeout {
			t.Fatalf("run %d: install began the unit read after its restart %s after the read before it ran its first line: it ended the restart before the restart's 200ms timeout", runs, span)
		}
		_, statErr := os.Stat(r.calls + ".queued")
		if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
			t.Fatal(statErr)
		}
		timedOut := err != nil && strings.Contains(err.Error(), "did not finish within 200ms")
		if queued := statErr == nil; !queued && timedOut {
			if call := longestHeld(starts); call.held() >= restartTimeout/2 {
				if time.Now().Before(until) {
					t.Logf("run %d is not judged: the 200ms timeout ended the restart's systemctl before it queued job 7, and the machine held the call %q %s between install's exec of it and its first line: running install again", runs, call.args, call.held())
					continue
				}
				t.Fatalf("no run of the right order in %d runs over %s: in each, the 200ms timeout ended the restart's systemctl before it queued job 7, and the machine held a systemctl call half that timeout or more before its first line", runs, within)
			}
		}
		if strings.Join(*ops, ",") != "sessions,drain" {
			t.Fatalf("run %d: ops asked: %q; want the running check and drain, and no undrain while job 7 is queued", runs, *ops)
		}
		if !timedOut || !strings.Contains(err.Error(), "job 7") {
			t.Fatalf("run %d: install whose timed-out restart is still queued: %v; want an error naming the timeout and the queued job 7", runs, err)
		}
		return
	}
}

// systemctlStart is one call of the rig's systemctl as its own first line recorded it in the
// calls log's .starts file: the call's arguments, and its start mark (testexec.StartMark): when
// the kernel started its process, which is when install ran it, and when that first line ran.
type systemctlStart struct {
	args string
	mark testexec.StartMark
}

// held is how long the machine held the call between install's exec of it and its first line.
// Install cannot cause that span: the process is the stand-in's from its exec on.
func (s systemctlStart) held() time.Duration {
	return max(s.mark.Late(), 0)
}

// restart reports whether the call was a restart, by its second argument, as the stand-in reads it.
func (s systemctlStart) restart() bool {
	args := strings.Fields(s.args)
	return len(args) > 1 && args[1] == "restart"
}

// starts reads the calls log's .starts file: one systemctlStart per call of the rig's systemctl,
// in the order the calls ran.
func (r *installRig) starts() []systemctlStart {
	r.t.Helper()
	text, err := os.ReadFile(r.calls + ".starts")
	if err != nil {
		r.t.Fatalf("read the rig's systemctl starts: %v", err)
	}
	var starts []systemctlStart
	for line := range strings.Lines(string(text)) {
		up, rest, _ := strings.Cut(strings.TrimSuffix(line, "\n"), "\t")
		stat, args, cut := strings.Cut(rest, "\t")
		mark, err := testexec.ParseStartMark(up, stat)
		if !cut || err != nil {
			r.t.Fatalf("the rig's systemctl recorded a start line of another shape: %q: %v", line, err)
		}
		starts = append(starts, systemctlStart{args: args, mark: mark})
	}
	return starts
}

// restartSpan is the most install's restart can have run: from the first line of the unit read
// install runs just before it (the first read of the main process) to the kernel's start of the
// next call after that read but the restart, which install runs only once the restart has ended.
// ok is false where the rig recorded no such pair.
func restartSpan(starts []systemctlStart) (span time.Duration, ok bool) {
	for i, read := range starts {
		if !strings.Contains(read.args, "--property=MainPID") {
			continue
		}
		for _, next := range starts[i+1:] {
			if !next.restart() {
				return next.mark.Started - read.mark.Reached, true
			}
		}
		return 0, false
	}
	return 0, false
}

// longestHeld is the call the machine held longest before its first line.
func longestHeld(starts []systemctlStart) systemctlStart {
	var longest systemctlStart
	for _, call := range starts {
		if call.held() > longest.held() {
			longest = call
		}
	}
	return longest
}

// shortenRestartTimeout sets install's restart timeout to 200 ms for the test.
func shortenRestartTimeout(t *testing.T) {
	t.Helper()
	previous := restartTimeout
	restartTimeout = 200 * time.Millisecond
	t.Cleanup(func() { restartTimeout = previous })
}

// installSpare is what a test that waits on install leaves of the test binary's deadline
// (testwait): time for install to return once its held restart is released, which takes two reads
// of the unit (systemctlReadTimeout each) and an undrain, and for the test's own report.
const installSpare = 30 * time.Second

// releaseRestartAtGiveUp writes the rig's release, which ends a restart its systemctl holds, once
// only installSpare is left of the test binary's deadline, and answers whether it has. An install
// that returns only after that waited for the restart past the timeout that should have ended its
// wait; one that never stops waiting still returns, and its test fails naming that.
func (r *installRig) releaseRestartAtGiveUp(t *testing.T) func() bool {
	t.Helper()
	var released atomic.Bool
	timer := time.AfterFunc(testwait.Remaining(t, installSpare), func() {
		released.Store(true)
		_ = os.WriteFile(r.release, nil, 0o644)
	})
	t.Cleanup(func() { timer.Stop() })
	return released.Load
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
