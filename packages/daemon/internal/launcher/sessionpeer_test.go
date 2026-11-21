package launcher

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claudelaunch"
	"github.com/sjawhar/legion/daemon/internal/memoryguard"
	"github.com/sjawhar/legion/daemon/internal/ompsessions"
	"github.com/sjawhar/legion/daemon/internal/sessionproxy"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/testpg/testdb"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// testWriterAudience is a writer audience of the form ompsessions.ValidWriterAudience takes. Its
// project number, pool and provider are placeholders, as every name beside testInstance is.
const testWriterAudience = "//iam.googleapis.com/projects/123456789012/locations/global/workloadIdentityPools/example-pool/providers/example-provider"

// writerDatabase is testDatabase signed in to with the writer token: it names the service account
// whose login testDatabase's User is, and the writer audience.
var writerDatabase = ompsessions.Database{
	Instance: testInstance, Name: testDatabase.Name, User: testDatabase.User,
	ImpersonateServiceAccount: testServiceAccount, WriterAudience: testWriterAudience,
}

// useTraceScope points the launcher's Yama check (requireTraceScope) at a file of the test's own
// that holds content, until the test ends, and answers its path.
func useTraceScope(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ptrace_scope")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	previous := ptraceScopeFile
	ptraceScopeFile = path
	t.Cleanup(func() { ptraceScopeFile = previous })
	return path
}

// usePeerProc points every session peer check that the test builds from now on at proc.
func usePeerProc(t *testing.T, proc string) {
	t.Helper()
	previous := peerProc
	peerProc = proc
	t.Cleanup(func() { peerProc = previous })
}

// restartAt rewrites the stat of pid, a process of pod, with start as its start time, as the
// kernel shows a process that took the pid after the first one ended.
func restartAt(t *testing.T, pod *guardPod, pid int, start uint64) {
	t.Helper()
	path := filepath.Join(pod.proc, strconv.Itoa(pid), "stat")
	stat, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	name := bytes.LastIndexByte(stat, ')')
	fields := strings.Fields(string(stat[name+1:]))
	// The start time is stat's field 22, the 20th after the command name.
	fields[19] = strconv.FormatUint(start, 10)
	pod.writeWhole(path, string(stat[:name+1])+" "+strings.Join(fields, " ")+"\n")
}

// The pids of peerRig's generation.
const (
	peerInit = 400 + iota
	peerShim
	peerAgent
	peerRenewer
	peerAdoption
	peerBroker
	peerCommand
	peerAgentCopy
	peerOrphanShim
	peerOrphanAgent
)

// peerRig is one Oh My Pi role's generation 4 in a fake /proc, which every peer check the test
// builds reads: the launcher, which is the test process; the role-init; the shim; the agent, with
// Oh My Pi's daemon broker, a command it runs and a copy of its own executable it runs; the
// shim's other children, the secrets renewer and the working copy's jj; and an orphan the
// role-init adopted that runs `legion worker-shim`, younger than the shim, with a child that runs
// the agent's executable. The start names the agent's executable through a link, and every
// process that runs it reads the file the link resolves to, as an exe link does.
type peerRig struct {
	pod     *guardPod
	manager *manager
	start   shimwire.LauncherStart
	// agent is the agent's executable as its exe link reads it.
	agent string
}

func newPeerRig(t *testing.T) *peerRig {
	t.Helper()
	exe := launcherExe(t)
	dir := t.TempDir()
	file := filepath.Join(dir, "omp-18.18.6", "omp")
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "omp")
	if err := os.Symlink(file, link); err != nil {
		t.Fatal(err)
	}
	agent, err := filepath.EvalSymlinks(link)
	if err != nil {
		t.Fatal(err)
	}
	pod := newGuardPod(t)
	pod.addLauncher(exe, 0)
	pod.add(
		guardProcess{pid: peerInit, ppid: os.Getpid(), exe: exe, args: []string{"legion", "launcher", "role-init", "--role", "tester"}, ns: testerNamespace},
		guardProcess{pid: peerShim, ppid: peerInit, exe: exe, args: []string{"legion", "worker-shim", "--connect", "tcp://daemon"}, ns: testerNamespace},
		guardProcess{pid: peerAgent, ppid: peerShim, exe: agent, args: []string{agent, "--mode", "rpc"}, ns: testerNamespace},
		guardProcess{pid: peerRenewer, ppid: peerShim, exe: "/usr/local/bin/agent-secrets", args: []string{"agent-secrets", "renew"}, ns: testerNamespace},
		guardProcess{pid: peerAdoption, ppid: peerShim, exe: "/usr/local/bin/jj", args: []string{"jj", "workspace", "update-stale"}, ns: testerNamespace},
		guardProcess{pid: peerBroker, ppid: peerAgent, exe: agent, args: []string{agent, "__omp_worker_daemon_broker"}, ns: testerNamespace},
		guardProcess{pid: peerCommand, ppid: peerAgent, exe: "/usr/bin/bash", args: []string{"bash", "-c", "psql"}, ns: testerNamespace},
		guardProcess{pid: peerAgentCopy, ppid: peerAgent, exe: agent, args: []string{agent, "-p", "write a row"}, ns: testerNamespace},
		guardProcess{pid: peerOrphanShim, ppid: peerInit, exe: exe, args: []string{"legion", "worker-shim"}, ns: testerNamespace},
		guardProcess{pid: peerOrphanAgent, ppid: peerOrphanShim, exe: agent, args: []string{agent, "--mode", "rpc"}, ns: testerNamespace},
	)
	usePeerProc(t, pod.proc)
	return &peerRig{
		pod:     pod,
		manager: &manager{role: "tester", child: &child{generation: 4, pid: peerInit}},
		start:   shimwire.LauncherStart{ID: "start-4", Generation: 4, Argv: []string{exe, shimArgument}, AgentExecutable: link},
		agent:   agent,
	}
}

// check is a fresh peer check of the rig's generation, as startSession builds one.
func (r *peerRig) check(t *testing.T) *sessionPeer {
	t.Helper()
	check, err := newSessionPeer(r.manager, r.start, nil)
	if err != nil {
		t.Fatalf("newSessionPeer: %v", err)
	}
	return check
}

// The check admits the launcher and the agent, the oldest direct child of the shim that runs the
// agent's executable, and refuses every other process of the role. Each process is the first
// that connects to a fresh check, so the check takes the agent from the table, never from the
// peer: a process that connects first is refused, and the agent is admitted after it. A child of
// the agent is refused, also when it runs the agent's executable, and so are the shim's other
// children, and an orphan that runs `legion worker-shim` with its child.
func TestTheSessionPeerCheckAdmitsTheLauncherAndTheAgentAlone(t *testing.T) {
	r := newPeerRig(t)
	for _, tc := range []struct {
		pid  int
		what string
	}{
		{peerAgentCopy, "a child of the agent that runs the agent's executable"},
		{peerBroker, "Oh My Pi's daemon broker, a child of the agent"},
		{peerCommand, "a command the agent runs"},
		{peerRenewer, "agent-secrets renew, a direct child of the shim"},
		{peerAdoption, "the working copy's jj, a direct child of the shim"},
		{peerOrphanShim, "an orphan that runs legion worker-shim, younger than the shim"},
		{peerOrphanAgent, "that orphan's child, which runs the agent's executable"},
		{peerShim, "the shim"},
		{peerInit, "the role-init"},
	} {
		t.Run(tc.what, func(t *testing.T) {
			check := r.check(t)
			want := fmt.Sprintf("pid %d is not the agent of generation 4, which is pid %d", tc.pid, peerAgent)
			if err := check.admit(sessionproxy.Peer{PID: tc.pid}); err == nil || err.Error() != want {
				t.Fatalf("the first connection, from pid %d, was answered %v; want %q", tc.pid, err, want)
			}
			if err := check.admit(sessionproxy.Peer{PID: peerAgent}); err != nil {
				t.Fatalf("the agent, pid %d, was refused after pid %d: %v", peerAgent, tc.pid, err)
			}
			if err := check.admit(sessionproxy.Peer{PID: os.Getpid()}); err != nil {
				t.Fatalf("the launcher was refused: %v", err)
			}
			if err := check.admit(sessionproxy.Peer{PID: tc.pid}); err == nil {
				t.Fatalf("pid %d was admitted once the agent was pinned", tc.pid)
			}
		})
	}
}

// The check pins the agent by its pid and start time. A process that took the agent's pid after
// the agent ended has another start time, and it is refused, though its place in the table and its
// executable are the agent's.
func TestTheSessionPeerCheckRefusesAProcessThatTookTheAgentsPid(t *testing.T) {
	r := newPeerRig(t)
	check := r.check(t)
	if err := check.admit(sessionproxy.Peer{PID: peerAgent}); err != nil {
		t.Fatalf("the agent was refused: %v", err)
	}
	restartAt(t, r.pod, peerAgent, 987654)
	want := fmt.Sprintf("pid %d is another process now: it started at 987654, and the agent of generation 4 started at %d", peerAgent, peerAgent*10)
	if err := check.admit(sessionproxy.Peer{PID: peerAgent}); err == nil || err.Error() != want {
		t.Fatalf("the reused pid was answered %v; want %q", err, want)
	}
}

// While the deployment's start wrapper runs, the shim's child runs /bin/sh, so no process is the
// agent: a connection is refused, and nothing is pinned. Once the wrapper execs the agent, the
// same pid runs the agent's executable, and it is admitted.
func TestTheSessionPeerCheckKnowsAWrappedAgentOnlyOnceTheWrapperHasExeced(t *testing.T) {
	r := newPeerRig(t)
	wrapper := guardProcess{pid: peerAgent, ppid: peerShim, exe: "/usr/bin/dash",
		args: []string{"/bin/sh", "-c", `legion-setup && exec "$@"`, "legion-start", r.agent, "--mode", "rpc"}, ns: testerNamespace}
	r.pod.add(wrapper)
	check := r.check(t)
	want := fmt.Sprintf("the shim of generation 4, pid %d, has no direct child that runs %s", peerShim, r.agent)
	if err := check.admit(sessionproxy.Peer{PID: peerAgent}); err == nil || err.Error() != want {
		t.Fatalf("the wrapper was answered %v; want %q", err, want)
	}
	if check.found {
		t.Fatalf("the check pinned pid %d before the wrapper's exec", check.pid)
	}
	wrapper.exe, wrapper.args = r.agent, []string{r.agent, "--mode", "rpc"}
	r.pod.add(wrapper)
	if err := check.admit(sessionproxy.Peer{PID: peerAgent}); err != nil {
		t.Fatalf("the agent the wrapper exec'd was refused: %v", err)
	}
}

// A Claude Code generation has no agent that connects: the launcher keeps its session. So its
// check admits the launcher alone, and it needs no agent executable.
func TestTheSessionPeerCheckOfAClaudeCodeGenerationAdmitsOnlyTheLauncher(t *testing.T) {
	r := newPeerRig(t)
	exe := launcherExe(t)
	r.pod.add(
		guardProcess{pid: peerAgent, ppid: peerShim, exe: exe, args: []string{"legion", "claude-bridge", "--", "/usr/local/bin/claude", "-p"}, ns: testerNamespace},
		guardProcess{pid: peerBroker, ppid: peerAgent, exe: "/usr/local/bin/claude", args: []string{"/usr/local/bin/claude", "-p"}, ns: testerNamespace},
	)
	start := r.start
	start.AgentExecutable = ""
	check, err := newSessionPeer(r.manager, start, []string{claudelaunch.SessionVariable + "=" + claudelaunch.NewSessionID()})
	if err != nil {
		t.Fatalf("newSessionPeer of a Claude Code start: %v", err)
	}
	for _, pid := range []int{peerAgent, peerBroker, peerShim, peerCommand} {
		const want = "generation 4 is a Claude Code one, whose session only the launcher writes"
		if err := check.admit(sessionproxy.Peer{PID: pid}); err == nil || err.Error() != want {
			t.Errorf("pid %d was answered %v; want %q", pid, err, want)
		}
	}
	if err := check.admit(sessionproxy.Peer{PID: os.Getpid()}); err != nil {
		t.Errorf("the launcher was refused: %v", err)
	}
}

// The check never admits the agent's pid on an error: with no generation running, with another
// generation running, with a table it cannot read, and with no role-init, no shim or no agent in
// the table, the agent's pid is refused with the reason. The launcher is admitted all the same.
func TestTheSessionPeerCheckRefusesWithoutItsGenerationItsTableItsShimOrItsAgent(t *testing.T) {
	for _, tc := range []struct {
		name string
		// change takes from the rig what the case lacks.
		change func(t *testing.T, r *peerRig)
		want   string
	}{
		{"no generation runs", func(_ *testing.T, r *peerRig) { r.manager.child = nil }, "generation 4 runs no process"},
		{"another generation runs", func(_ *testing.T, r *peerRig) { r.manager.child.generation = 5 }, "the role runs generation 5, not generation 4"},
		{"the table cannot be read", func(t *testing.T, _ *peerRig) { usePeerProc(t, filepath.Join(t.TempDir(), "absent")) }, "read the process table: "},
		{"the role-init is gone", func(_ *testing.T, r *peerRig) { r.manager.child.pid = peerInit + 99 },
			fmt.Sprintf("the role-init of generation 4, pid %d, is not in the process table", peerInit+99)},
		// Neither child of the role-init is the launcher's executable run as worker-shim.
		{"the role-init has no shim", func(_ *testing.T, r *peerRig) {
			r.pod.add(
				guardProcess{pid: peerShim, ppid: peerInit, exe: "/usr/local/bin/legion-old", args: []string{"legion", "worker-shim"}, ns: testerNamespace},
				guardProcess{pid: peerOrphanShim, ppid: peerInit, exe: "/usr/bin/sleep", args: []string{"sleep", "600"}, ns: testerNamespace},
			)
		}, fmt.Sprintf("the role-init of generation 4, pid %d, has no shim", peerInit)},
		{"the shim has no agent", func(_ *testing.T, r *peerRig) {
			r.pod.add(guardProcess{pid: peerAgent, ppid: peerShim, exe: "/usr/bin/node", args: []string{"node", "agent.js"}, ns: testerNamespace})
		}, fmt.Sprintf("the shim of generation 4, pid %d, has no direct child that runs ", peerShim)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newPeerRig(t)
			tc.change(t, r)
			check := r.check(t)
			err := check.admit(sessionproxy.Peer{PID: peerAgent})
			if err == nil || !strings.HasPrefix(err.Error(), tc.want) {
				t.Fatalf("the agent's pid was answered %v; want a refusal starting %q", err, tc.want)
			}
			if check.found {
				t.Fatalf("the check pinned pid %d on a refusal", check.pid)
			}
			if err := check.admit(sessionproxy.Peer{PID: os.Getpid()}); err != nil {
				t.Fatalf("the launcher was refused: %v", err)
			}
		})
	}
}

// writerRig is an issue pod's launcher of one role, tester, whose session database names a writer
// audience, as TestAnIssuePodsSessionURLNamesTheSocketAsTheRoleSeesIt builds one, with the writer
// token under its roles directory: its manager, with no generation running, the roles directory,
// the launcher's log and the fake dialer.
type writerRig struct {
	m        *manager
	rolesDir string
	log      *lockedBuffer
	dialer   *sessionDialer
}

func newWriterRig(t *testing.T, database ompsessions.Database, dialer *sessionDialer) *writerRig {
	t.Helper()
	r := &writerRig{log: &lockedBuffer{}, dialer: dialer}
	r.rolesDir = namespaceScratch(t, filepath.Join("tester", RolePrivateDir, shimwire.GenerationDir(9), ompsessions.SocketName))
	for _, name := range []string{RoleLauncherDir, RoleGHDir, RolePrivateDir, RoleStateDir, RoleAgentSecretsDir} {
		if err := os.MkdirAll(filepath.Join(r.rolesDir, "tester", name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(r.rolesDir, "tester", RoleLauncherDir, LauncherTokenFile), []byte(ownToken("launcher-token-tester")+"\n"), 0o400); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(r.rolesDir, SessionWriterDirName), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(r.tokenPath(), []byte(testWriterToken), 0o400); err != nil {
		t.Fatal(err)
	}
	managers, err := newManagers(Config{
		Connect: "tcp://127.0.0.1:1", Sandbox: "legion-legion-legion-208", PodUID: "pod-1", RolesDir: r.rolesDir, Roles: []string{"tester"},
		StopGrace: 5 * time.Second, Stdout: r.log, Stderr: r.log, SessionDatabase: database, SessionDialer: dialer.build,
	}, &pidGuard{})
	if err != nil {
		t.Fatal(err)
	}
	r.m = managers[0]
	return r
}

// testWriterToken is the writer token's content in every test's roles directory.
const testWriterToken = "fake-session-writer-token"

// tokenPath is the writer token's path, as the launcher reads it.
func (r *writerRig) tokenPath() string {
	return filepath.Join(r.rolesDir, SessionWriterDirName, SessionWriterTokenFile)
}

// generationDir is the directory of tester's generation, as the launcher sees it.
func (r *writerRig) generationDir(generation uint64) string {
	return generationDir(filepath.Join(r.rolesDir, "tester", RolePrivateDir), generation)
}

// session starts the session of start's generation, as start does, in a directory of its own,
// with env the generation's environment. A proxy it starts is closed when the test ends.
func (r *writerRig) session(t *testing.T, start shimwire.LauncherStart, env []string) (*sessionproxy.Proxy, string, error) {
	t.Helper()
	if err := os.MkdirAll(r.generationDir(start.Generation), 0o700); err != nil {
		t.Fatal(err)
	}
	proxy, clientURL, err := r.m.startSession(context.Background(), start, env, r.generationDir(start.Generation))
	if proxy != nil {
		t.Cleanup(func() { _ = proxy.Close() })
	}
	return proxy, clientURL, err
}

// dials is how many dials the fake has seen, the proof dial of each generation among them.
func (d *sessionDialer) dials() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.bounds)
}

// testExecutable is the test binary, the one absolute executable every machine that runs the
// test has.
func testExecutable(t *testing.T) string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return self
}

// roleExecutable is a copy of the test binary in dir, which is outside /tmp, and the launcher's
// own executable until the test ends (launcherExecutable). A role generation's shim and agent
// stand-ins run this copy: `go test` builds the binary under /tmp where TMPDIR is unset, as on
// GitHub's runners, and role-init binds the role's own temporary directory over /tmp, so the
// role could not start the binary at that path. The shim rule (roleShim) then knows the shim by
// the copy, as it knows a pod's shim by the launcher's own binary.
func roleExecutable(t *testing.T, dir string) string {
	t.Helper()
	body, err := os.ReadFile(testExecutable(t))
	if err != nil {
		t.Fatal(err)
	}
	copied := filepath.Join(dir, "launcher.test")
	if err := os.WriteFile(copied, body, 0o755); err != nil {
		t.Fatal(err)
	}
	previous := launcherExecutable
	launcherExecutable = func() (string, error) { return copied, nil }
	t.Cleanup(func() { launcherExecutable = previous })
	return copied
}

// The launcher's own connections through a writer generation's socket are admitted: the resume
// lookups (resumable, and sessionByID for a start that names its session by its id alone) and a
// Claude Code keeper connect from the launcher's process, before any process of the generation
// runs. Each reaches the session table through the fake dialer, and the proxy logs no refusal. An
// Oh My Pi generation and a Claude Code one admit the launcher alike.
func TestTheLaunchersOwnSessionConnectionsPassTheWriterCheck(t *testing.T) {
	database, dialer := sessionDatabaseAt(t, testdb.Database(t, "legion_launcher_writer_test"))
	database.ImpersonateServiceAccount, database.WriterAudience = testServiceAccount, testWriterAudience
	shortenResumeLookup(t)
	useTraceScope(t, "1\n")
	r := newWriterRig(t, database, dialer)
	claude := []string{
		claudelaunch.SessionVariable + "=" + claudelaunch.NewSessionID(),
		ompsessions.StorageVariable + "=" + ompsessions.SQLStorage, "CLAUDE_CONFIG_DIR=" + t.TempDir(),
	}
	for _, tc := range []struct {
		name  string
		start shimwire.LauncherStart
		env   []string
	}{
		{"an Oh My Pi generation", shimwire.LauncherStart{Generation: 4, AgentExecutable: testExecutable(t)}, nil},
		{"a Claude Code generation", shimwire.LauncherStart{Generation: 5}, claude},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, clientURL, err := r.session(t, tc.start, tc.env)
			if err != nil {
				t.Fatalf("startSession: %v; the launcher's log: %s", err, r.log.String())
			}
			before := dialer.dials()
			_, _, err = resumable(context.Background(), tc.env, "sessions/one.jsonl", clientURL, nil)
			if !errors.Is(err, shimwire.SessionGone) {
				t.Fatalf("the resume lookup answered %v; want the table's answer that it holds no such session", err)
			}
			if after := dialer.dials(); after <= before {
				t.Fatalf("the fake dialer saw %d dials before the resume lookup and %d after it; want more", before, after)
			}
			before = dialer.dials()
			if _, err := sessionByID(context.Background(), tc.env, "01a1f5de-4835-7667-b005-e99e3e17edd3", clientURL); !errors.Is(err, shimwire.SessionGone) {
				t.Fatalf("the lookup of a session by its id answered %v; want the table's answer that it holds no such session", err)
			}
			if after := dialer.dials(); after <= before {
				t.Fatalf("the fake dialer saw %d dials before the lookup by id and %d after it; want more", before, after)
			}
			if tc.env != nil {
				keeper, err := newClaudeKeeper(tc.env, "", clientURL, nil)
				if err != nil {
					t.Fatal(err)
				}
				before := dialer.dials()
				if err := keeper.open(context.Background(), nil); err != nil {
					t.Fatalf("the keeper's connection: %v; the launcher's log: %s", err, r.log.String())
				}
				keeper.close()
				if after := dialer.dials(); after <= before {
					t.Fatalf("the fake dialer saw %d dials before the keeper's connection and %d after it; want more", before, after)
				}
			}
			if log := r.log.String(); strings.Contains(log, "refused a connection") {
				t.Fatalf("the proxy refused a connection of the launcher's own: %s", log)
			}
		})
	}
}

// D5: a writer generation starts only where Yama's ptrace_scope reads 1 or more, since only then
// can no process of the role trace the agent and take its connection. A scope of 0, an absent
// file and text that is not an integer each refuse the start before anything is written or
// dialed, naming the file and what it read. A scope of 1 lets the start go on.
func TestAWriterGenerationIsRefusedUnlessYamaForbidsTracingTheAgent(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content string
		absent  bool
		want    string
	}{
		{"a scope of 0", "0\n", false, `and it reads "0": a process of the role could then trace the agent`},
		{"no scope file", "", true, "and it cannot be read: "},
		{"a scope that is no integer", "classic\n", false, `and it reads "classic": `},
		{"a scope of 1", "1\n", false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := useTraceScope(t, tc.content)
			if tc.absent {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
			r := newWriterRig(t, writerDatabase, &sessionDialer{addr: idleUpstream(t)})
			proxy, _, err := r.session(t, shimwire.LauncherStart{Generation: 4, AgentExecutable: testExecutable(t)}, nil)
			if tc.want == "" {
				if err != nil || proxy == nil {
					t.Fatalf("startSession = %v; want the generation's proxy", err)
				}
				return
			}
			prefix := "generation 4: the session writer token needs " + path + " to read 1 or more, "
			if err == nil || !strings.HasPrefix(err.Error(), prefix) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("startSession = %v; want a refusal starting %q and naming %q", err, prefix, tc.want)
			}
			if proxy != nil || r.dialer.dials() != 0 {
				t.Fatalf("the refused generation started a proxy (%v) or dialed (%d)", proxy != nil, r.dialer.dials())
			}
			if _, err := os.Stat(filepath.Join(r.generationDir(4), ompsessions.DSNFileName)); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("the refused generation wrote its URL file (%v)", err)
			}
		})
	}
}

// An Oh My Pi writer start must name the agent's executable as an absolute path: the check knows
// the agent by it, so a start that names none, or a relative one, is refused before anything is
// written or dialed. A Claude Code start names none and goes on.
func TestAnOhMyPiWriterStartMustNameItsAgentsExecutableAsAnAbsolutePath(t *testing.T) {
	useTraceScope(t, "1\n")
	claude := []string{claudelaunch.SessionVariable + "=" + claudelaunch.NewSessionID()}
	for _, tc := range []struct {
		name  string
		agent string
		env   []string
		want  string
	}{
		{"no agent executable", "", nil, "generation 4: an Oh My Pi start with the session writer token must name the agent's executable (agentExecutable), and this start names none"},
		{"a relative agent executable", "omp", nil, `generation 4: the agent's executable "omp" is not an absolute path`},
		{"an absolute agent executable", testExecutable(t), nil, ""},
		{"a Claude Code start", "", claude, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newWriterRig(t, writerDatabase, &sessionDialer{addr: idleUpstream(t)})
			proxy, _, err := r.session(t, shimwire.LauncherStart{Generation: 4, AgentExecutable: tc.agent}, tc.env)
			if tc.want == "" {
				if err != nil || proxy == nil {
					t.Fatalf("startSession = %v; want the generation's proxy", err)
				}
				return
			}
			if err == nil || err.Error() != tc.want {
				t.Fatalf("startSession = %v; want %q", err, tc.want)
			}
			if proxy != nil || r.dialer.dials() != 0 {
				t.Fatalf("the refused generation started a proxy (%v) or dialed (%d)", proxy != nil, r.dialer.dials())
			}
		})
	}
}

// R4: a microVM issue pod's launcher with a writer audience serves the pod-local metadata endpoint
// from the pod's Workload Identity token, never from the writer token: the endpoint's exchange
// sends the workload-identity file's token, though the writer token sits under the same roles
// directory.
func TestTheMetadataEndpointExchangesItsOwnTokenAndNeverTheWriterToken(t *testing.T) {
	holdMetadataVariables(t, "")
	rolesDir := t.TempDir()
	identity := filepath.Join(rolesDir, WorkloadIdentityDirName, WorkloadIdentityTokenFile)
	writer := filepath.Join(rolesDir, SessionWriterDirName, SessionWriterTokenFile)
	for path, token := range map[string]string{identity: "fake-workload-identity-token", writer: testWriterToken} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		writeSubjectToken(t, path, token)
	}
	sts := &fakeSTS{t: t, tokenFile: identity, expiresIn: 3600}
	exchange := httptest.NewServer(sts)
	t.Cleanup(exchange.Close)
	cfg := Config{
		Connect: "tcp://127.0.0.1:1", Sandbox: "legion-legion-legion-208", PodUID: "pod-1", RolesDir: rolesDir, Roles: []string{"tester"},
		MicroVM: true, SessionDatabase: writerDatabase,
		Metadata: &MetadataConfig{WorkloadPool: testWorkloadPool, IdentityProvider: testIdentityProvider, address: "127.0.0.1:0", tokenURL: exchange.URL},
	}
	if err := cfg.validate(); err != nil {
		t.Fatalf("a microVM issue pod's launcher with a writer audience is refused: %v", err)
	}
	log := &lockedBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	stopped, address := serveMetadata(ctx, cfg, func(format string, args ...any) { _, _ = fmt.Fprintf(log, format+"\n", args...) })
	if address == "" {
		cancel()
		t.Fatalf("the endpoint did not listen: %s", log.String())
	}
	t.Cleanup(func() {
		cancel()
		<-stopped
	})
	if response, body := getMetadata(t, "http://"+address, tokenPath, flavor); response.StatusCode != http.StatusOK {
		t.Fatalf("the endpoint answered the token request %d %s", response.StatusCode, redacted(body))
	}
	exchanges := sts.answered()
	if len(exchanges) != 1 || exchanges[0].subject != "fake-workload-identity-token" {
		t.Fatalf("the endpoint made %d exchanges; want one, of the workload-identity token and never the writer token", len(exchanges))
	}
}

// The markers of the session peer test's stand-ins (startPeerGeneration). peerPlanEnv carries the
// plan every stand-in reads, and marks the shim stand-in, which TestMain runs (sessionPeerShim).
// peerStandInEnv names which of the other two stand-ins a process is, "agent" or "child"
// (TestSessionPeerStandInHelper).
const (
	peerPlanEnv    = "LEGION_SESSION_PEER_TEST_PLAN"
	peerStandInEnv = "LEGION_SESSION_PEER_TEST_STAND_IN"
)

// peerPlan is what the stand-ins read: the generation's URL file as the role sees it, and the
// roles directory as the launcher sees it, under which the agent looks for the writer token. The
// plan names the directory and not the token's path, so the environment it travels in holds
// that path only where the launcher puts it.
type peerPlan struct {
	DSNFile  string
	RolesDir string
}

// The files the stand-ins write in the role's own /tmp: what the child saw, what the agent saw,
// the agent's environment, and the file the test writes to let the child end.
const (
	peerChildSaw     = "peer-child"
	peerAgentSaw     = "peer-agent"
	peerAgentEnviron = "peer-agent-environ"
	peerRelease      = "peer-release"
)

// sessionPeerShim is the shim stand-in: the test binary run as `<binary> worker-shim`, the
// argv the shim rule knows a shim by (roleShim), so TestMain runs it before the test framework
// reads any argument. It starts the agent stand-in as its one direct child, and exits with that
// child's status, as the shim ends with its agent.
func sessionPeerShim() int {
	self, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, "shim stand-in:", err)
		return 1
	}
	agent := exec.Command(self, "-test.run=^TestSessionPeerStandInHelper$")
	agent.Env = append(os.Environ(), peerStandInEnv+"=agent")
	agent.Stdout, agent.Stderr = os.Stdout, os.Stderr
	if err := agent.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "shim stand-in:", err)
		return 1
	}
	return 0
}

// TestSessionPeerStandInHelper is the agent stand-in and its child, both the test binary, so both
// run the agent's executable. The agent starts the child, which connects to the generation's
// socket first and reports it. The agent then tries to read the writer token, writes out its own
// environment, connects, and reports what it saw. A connection is admitted when the upstream
// echoes it through the proxy. The child stays until the test lets it end, so the test can read
// its pid in the launcher's process table; the agent stays until the child ends.
func TestSessionPeerStandInHelper(t *testing.T) {
	which := os.Getenv(peerStandInEnv)
	if which == "" {
		return
	}
	fail := func(err error) {
		fmt.Fprintf(os.Stderr, "%s stand-in: %v\n", which, err)
		os.Exit(2)
	}
	var plan peerPlan
	if err := json.Unmarshal([]byte(os.Getenv(peerPlanEnv)), &plan); err != nil {
		fail(err)
	}
	socket, err := peerSocket(plan.DSNFile)
	if err != nil {
		fail(err)
	}
	if which == "child" {
		if err := sayInTmp(peerChildSaw, "connect="+peerConnect(socket)); err != nil {
			fail(err)
		}
		for {
			if _, err := os.Stat(filepath.Join("/tmp", peerRelease)); err == nil {
				os.Exit(0)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	self, err := os.Executable()
	if err != nil {
		fail(err)
	}
	child := exec.Command(self, "-test.run=^TestSessionPeerStandInHelper$")
	child.Env = append(os.Environ(), peerStandInEnv+"=child")
	child.Stdout, child.Stderr = os.Stdout, os.Stderr
	if err := child.Start(); err != nil {
		fail(err)
	}
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if _, err := os.Stat(filepath.Join("/tmp", peerChildSaw)); err == nil {
			break
		}
		if time.Now().After(deadline) {
			fail(errors.New("the child never reported its connection"))
		}
	}
	token := "read"
	if _, err := os.ReadFile(filepath.Join(plan.RolesDir, SessionWriterDirName, SessionWriterTokenFile)); err != nil {
		token = "unreadable"
	}
	if err := sayInTmp(peerAgentEnviron, strings.Join(os.Environ(), "\x00")); err != nil {
		fail(err)
	}
	if err := sayInTmp(peerAgentSaw, "token="+token+" connect="+peerConnect(socket)); err != nil {
		fail(err)
	}
	_ = child.Wait()
	os.Exit(0)
}

// peerSocket is the socket the URL file at path names, as Oh My Pi's client reads it: the
// URL's `path` parameter.
func peerSocket(path string) (string, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	parsed, err := url.Parse(strings.TrimSpace(string(body)))
	if err != nil {
		return "", err
	}
	socket := parsed.Query().Get("path")
	if socket == "" {
		return "", fmt.Errorf("the URL file %s names no socket", path)
	}
	return socket, nil
}

// peerConnect connects to socket and answers "admitted" when the proxy carries a round trip to
// the upstream, which echoes it, and otherwise what ended the connection.
func peerConnect(socket string) string {
	conn, err := net.DialTimeout("unix", socket, 10*time.Second)
	if err != nil {
		return "unreachable: " + err.Error()
	}
	defer func() { _ = conn.Close() }()
	if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return "unreachable: " + err.Error()
	}
	if _, err := conn.Write([]byte("ping")); err != nil {
		return "refused"
	}
	echoed := make([]byte, len("ping"))
	if _, err := io.ReadFull(conn, echoed); err != nil || string(echoed) != "ping" {
		return "refused"
	}
	return "admitted"
}

// sayInTmp writes text to name in the role's own /tmp whole, through a rename, so the test never
// reads half of it.
func sayInTmp(name, text string) error {
	path := filepath.Join("/tmp", name)
	if err := os.WriteFile(path+".tmp", []byte(text), 0o600); err != nil {
		return err
	}
	return os.Rename(path+".tmp", path)
}

// peerGeneration is generation 1 of tester, run through m.start under the real role-init, with
// the writer token in the roles directory: the shim stand-in, the agent stand-in and its child
// run in the role's namespaces, and the fake dialer's upstream echoes what it is sent.
type peerGeneration struct {
	t *testing.T
	*writerRig
	// exe is the test binary, which the role-init, the shim and both stand-ins run.
	exe string
}

// startPeerGeneration starts the generation and returns once the agent stand-in has reported.
func startPeerGeneration(t *testing.T) *peerGeneration {
	t.Helper()
	requireRoleNamespaces(t)
	useTraceScope(t, "1\n")
	scratch := namespaceScratch(t, "")
	exe := roleExecutable(t, scratch)
	home, homeMounts := filepath.Join(scratch, "home"), filepath.Join(scratch, "home-mounts")
	// The fixed root is where the role finds its socket, so it is short, as the image's
	// /var/run/legion is.
	fixed := namespaceScratch(t, filepath.Join(RolePrivateDir, shimwire.GenerationDir(9), ompsessions.SocketName))
	for _, dir := range []string{filepath.Join(fixed, RoleGHDir), filepath.Join(fixed, RolePrivateDir), filepath.Join(fixed, RoleStateDir),
		filepath.Join(fixed, RoleAgentSecretsDir), home, homeMounts} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	useRoleLayout(t, home, homeMounts, fixed)
	g := &peerGeneration{t: t, writerRig: newWriterRig(t, writerDatabase, &sessionDialer{addr: echoUpstream(t)}), exe: exe}
	t.Cleanup(g.m.stopActive)
	plan, err := json.Marshal(peerPlan{
		DSNFile:  filepath.Join(generationDir(filepath.Join(fixed, RolePrivateDir), 1), ompsessions.DSNFileName),
		RolesDir: g.rolesDir,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := g.m.start(context.Background(), shimwire.LauncherStart{
		ID: "start-1", Generation: 1, Argv: []string{exe, shimArgument}, Env: []string{peerPlanEnv + "=" + string(plan)}, AgentExecutable: exe,
	}); !got.OK {
		t.Fatalf("start = %#v; the launcher's log: %s", got, g.log.String())
	}
	g.said(peerAgentSaw)
	return g
}

// roleTmp is name in the role's own /tmp, as the launcher sees it.
func (g *peerGeneration) roleTmp(name string) string {
	return filepath.Join(g.rolesDir, RoleHomesDir, "tester", "tmp", name)
}

// said is what a stand-in wrote to name, once it has. A stand-in that never writes it fails the
// test, naming the launcher's log, which holds every line of the role and its role-init.
func (g *peerGeneration) said(name string) string {
	g.t.Helper()
	var said string
	if !testwait.Holds(30*time.Second, testwait.Poll, func() bool {
		body, err := os.ReadFile(g.roleTmp(name))
		said = string(body)
		return err == nil
	}) {
		g.t.Fatalf("%s was never written; the launcher's log: %s", name, g.log.String())
	}
	return said
}

// release lets the child stand-in end, and with it the agent, the shim and the generation.
func (g *peerGeneration) release() {
	if err := os.WriteFile(g.roleTmp(peerRelease), nil, 0o644); err != nil {
		g.t.Error(err)
	}
}

// A real role's agent is admitted, and its child is refused and logged. The generation runs under
// the real role-init; its shim is the test binary run as `worker-shim`, which starts the agent
// stand-in, which starts its child, all three the test binary. The child connects first, so it
// could have become the pin, and it is refused: the fake dialer sees no dial for it, and the
// proxy's one refusal line names the child's pid in the launcher's PID namespace and its command.
// The agent connects next and is admitted: its round trip reaches the upstream. Inside the role's
// namespaces the agent cannot read the writer token, which the roles directory holds.
func TestARealRolesAgentIsAdmittedAndItsChildIsRefused(t *testing.T) {
	g := startPeerGeneration(t)
	defer g.release()
	if said := g.said(peerChildSaw); said != "connect=refused" {
		t.Errorf("the agent's child saw %q; want its connection refused", said)
	}
	if said := g.said(peerAgentSaw); said != "token=unreadable connect=admitted" {
		t.Errorf("the agent saw %q; want the writer token unreadable and its connection admitted", said)
	}
	if dials := g.dialer.dials(); dials != 2 {
		t.Errorf("the fake dialer saw %d dials; want the proof dial and the agent's", dials)
	}
	g.m.mu.Lock()
	roleInit := g.m.child.pid
	g.m.mu.Unlock()
	table, err := memoryguard.Scan("/proc")
	if err != nil {
		t.Fatal(err)
	}
	init, ok := table.Get(roleInit)
	if !ok {
		t.Fatalf("the role-init, pid %d, is not in the process table", roleInit)
	}
	shim := roleShim(table, init, g.exe)
	if shim == nil || len(table.Children(shim.PID)) != 1 {
		t.Fatalf("the role-init, pid %d, has no shim with one child", roleInit)
	}
	agent := table.Children(shim.PID)[0]
	if len(table.Children(agent.PID)) != 1 {
		t.Fatalf("the agent, pid %d, has %d children; want its one child", agent.PID, len(table.Children(agent.PID)))
	}
	child := table.Children(agent.PID)[0]
	want := fmt.Sprintf("sessionproxy: refused a connection to %s from pid %d (%q): pid %d is not the agent of generation 1, which is pid %d",
		testInstance, child.PID, child.Name, child.PID, agent.PID)
	if !testwait.Holds(10*time.Second, testwait.Poll, func() bool { return strings.Contains(g.log.String(), want) }) {
		t.Fatalf("the launcher's log holds no line %q: %s", want, g.log.String())
	}
	if refusals := strings.Count(g.log.String(), "refused a connection"); refusals != 1 {
		t.Errorf("the launcher's log holds %d refusals; want the child's alone: %s", refusals, g.log.String())
	}
}

// R4: no process of a writer generation is told the writer token's path or its content. The agent
// stand-in's environment, which holds all its role-init and its shim were given, names neither,
// and no file of the generation's directory holds either.
func TestTheWriterTokenIsInNoEnvironmentAndNoFileOfItsGeneration(t *testing.T) {
	g := startPeerGeneration(t)
	defer g.release()
	for _, entry := range strings.Split(g.said(peerAgentEnviron), "\x00") {
		if strings.Contains(entry, g.tokenPath()) || strings.Contains(entry, testWriterToken) {
			t.Errorf("the generation's environment holds the writer token's path or content: %q", entry)
		}
	}
	if !strings.Contains(g.said(peerAgentEnviron), peerPlanEnv+"=") {
		t.Fatal("the agent's environment file does not hold the start's own environment")
	}
	files := 0
	err := filepath.WalkDir(g.generationDir(1), func(path string, entry fs.DirEntry, err error) error {
		if err != nil || !entry.Type().IsRegular() {
			return err
		}
		files++
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.Contains(body, []byte(g.tokenPath())) || bytes.Contains(body, []byte(testWriterToken)) {
			t.Errorf("%s holds the writer token's path or content", path)
		}
		return nil
	})
	if err != nil || files == 0 {
		t.Fatalf("read the generation's directory: %v (%d files)", err, files)
	}
}
