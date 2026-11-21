//go:build e2e

// The rig's runtime and the seams its checks name (RIG.md): localruntime_live_test.go's runtime,
// each worker the real `legion worker-shim` around a real Oh My Pi as a child process group, with
// what a check of this change needs beside it:
//
//   - a process reported gone while it keeps running and connected (detach), which is how the
//     daemon comes to launch a claim's next generation over an older process it no longer runs (w3;
//     the kubelet that could not kill one issue pod's old container, 2026-11-12);
//   - a shim started by hand, of a given generation's boot token or of none the daemon minted
//     (startShim);
//   - every runtime read of a claim's process, counted (probesOf), so a check can say the daemon
//     asked nothing;
//   - each launch's environment extended by a check (extraEnv), and where a resume finds the
//     session it reads (sessionKept), which w2's SQL sessions take (rig_sql_sessions_live_test.go);
//   - an Oh My Pi session that holds no Legion claim, started by hand (startSession), which w2's
//     last step kills mid-turn and resumes;
//   - every shim's connection carried by the rig's worker-stream relay (rigRelay), which records
//     the frames each side sends, closes a connection from the daemon's side as a network cut
//     would, holds a shim's redial back, and keeps a shim's process running past its stop frame.
//
// What it adds to localRuntime's launch (rigRuntime.launch), and why: every secret of the spec is
// written as a launch file and named to the pane, as the tmux runtime writes them (its
// secretFiles), so the pane presents the daemon's Envoy token to the real listener, which the
// stand-in listener of the other proofs never asked for; and the shim dials the relay rather than
// the daemon's own socket. Everything else is localRuntime's: its panes, its observation sweep, its
// stop, its orphan reconciliation.
package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/tmux"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

// rigRuntime is localRuntime with the rig's seams.
type rigRuntime struct {
	*localRuntime
	relay *rigRelay
	// extraEnv, when set, is added to every launch's environment after a pane's own: the seam a
	// check that starts each worker another way takes (w2: OMP_SESSION_STORAGE=sql and its DSN).
	extraEnv func(runtime.SpawnSpec) []string
	// sessionKept, when set, is how a resume finds the session it reads, in place of the file the
	// recorded path names: w2's SQL sessions, whose session is a row of their database and never a
	// file (rigSQLSessions.kept). It answers why the session is not there.
	sessionKept func(path string) error
	// extraArgs, when set, is added to every launch's `legion worker-shim` flags, before the `--`
	// that ends them: w4's renewal hands the shim the agent-secrets flags the Kubernetes runtime's
	// launcher passes (--agent-secrets-key-dir, --pod-token-file, --agent-secrets-bin).
	extraArgs func(runtime.SpawnSpec) []string

	mu       sync.Mutex
	launches []*rigLaunch
	probes   []rigProbe
	shims    []*rigShim
}

// rigLaunch is one process the runtime started for the daemon: the spec it was started from (its
// generation and boot token among it), the pane, and when.
type rigLaunch struct {
	spec runtime.SpawnSpec
	pane *localPane
	at   time.Time
}

// running is whether the launch's process group's leader still runs.
func (l *rigLaunch) running() bool {
	select {
	case <-l.pane.done:
		return false
	default:
		return true
	}
}

// rigProbe is one read of a process the daemon made through the runtime (Runtime.Probe).
type rigProbe struct {
	at  time.Time
	loc runtime.Locator
}

func newRigRuntime(t *testing.T, local *localRuntime, relay *rigRelay) *rigRuntime {
	r := &rigRuntime{localRuntime: local, relay: relay}
	relay.resolve = r.bootTokenOf
	t.Cleanup(r.killShims)
	return r
}

func (r *rigRuntime) Spawn(ctx context.Context, spec runtime.SpawnSpec) (runtime.Locator, error) {
	if spec.ResumeSessionFile != "" {
		return runtime.Locator{}, fmt.Errorf("spawn %s: a spec with a session file is Resume's", spec.Claim)
	}
	return r.launch(ctx, spec)
}

func (r *rigRuntime) Resume(ctx context.Context, prev *runtime.Locator, spec runtime.SpawnSpec) (runtime.Locator, error) {
	if prev != nil {
		if err := r.awaitGone(ctx, *prev); err != nil {
			return runtime.Locator{}, err
		}
	}
	if spec.ResumeSessionFile != "" {
		if r.sessionKept != nil {
			if err := r.sessionKept(spec.ResumeSessionFile); err != nil {
				return runtime.Locator{}, fmt.Errorf("resume %s: %w", spec.Claim, err)
			}
		} else if _, err := os.Stat(spec.ResumeSessionFile); err != nil {
			return runtime.Locator{}, fmt.Errorf("resume %s: the recorded session file is missing: %s", spec.Claim, spec.ResumeSessionFile)
		}
	}
	return r.launch(ctx, spec)
}

// Probe records the read before it answers it, as localRuntime answers.
func (r *rigRuntime) Probe(ctx context.Context, loc runtime.Locator) (runtime.Observation, error) {
	r.mu.Lock()
	r.probes = append(r.probes, rigProbe{at: time.Now(), loc: loc})
	r.mu.Unlock()
	return r.localRuntime.Probe(ctx, loc)
}

// probesOf is every read of token's process the daemon made at or after from.
func (r *rigRuntime) probesOf(token claim.Token, from time.Time) []rigProbe {
	r.mu.Lock()
	defer r.mu.Unlock()
	var probes []rigProbe
	for _, p := range r.probes {
		if p.loc.Claim == token && !p.at.Before(from) {
			probes = append(probes, p)
		}
	}
	return probes
}

// launchEnvironment is a launch's environment, which both localRuntime's launch and rigRuntime's
// start from: paneEnvironment, and on a resume runtime.SessionExclusiveVariable set to 1, so the
// resumed Oh My Pi is held to its recorded session as every runtime holds it. A resume that went
// without it moved to a new session id while the older process still held the session (w3's step
// 4, 2026-11-20, when rigRuntime built its own environment).
//
// It is a method of localRuntime but lives in this file, not in localruntime_live_test.go: each
// check's control lays this file over the base's tree and leaves the base's own
// localruntime_live_test.go there (the rig_files of scripts/e2e/*.sh), so a method defined there
// would be missing from every control's build. For the same reason it writes the variable out
// (rigSessionExclusive) rather than naming runtime's constants, which a control's base may predate
// (w4's, e5e4900fe7). TestEachProofRuntimeHoldsAResumedOhMyPiToItsSession holds the two equal.
func (r *localRuntime) launchEnvironment(spec runtime.SpawnSpec, variables []string, workDir string) []string {
	env := r.paneEnvironment(variables, workDir)
	if spec.ResumeSessionFile != "" {
		env = append(env, rigSessionExclusive)
	}
	return env
}

// rigSessionExclusive is runtime.SessionExclusiveVariable set to runtime.SessionExclusiveValue,
// written out (launchEnvironment says why).
const rigSessionExclusive = "OMP_SESSION_EXCLUSIVE=1"

// launch is localRuntime.launch with the spec's every secret written and named, and the shim
// pointed at the relay (the file's header says why).
func (r *rigRuntime) launch(ctx context.Context, spec runtime.SpawnSpec) (runtime.Locator, error) {
	if err := runtime.ValidateSpawnSpec(spec, nil); err != nil {
		return runtime.Locator{}, err
	}
	l := r.localRuntime
	l.mu.Lock()
	if l.stopped {
		l.mu.Unlock()
		return runtime.Locator{}, fmt.Errorf("spawn %s: the proof's runtime has stopped", spec.Claim)
	}
	if live, ok := l.panes[spec.Claim]; ok && alive(live.pid) {
		l.mu.Unlock()
		return runtime.Locator{}, fmt.Errorf("spawn %s: its process (pid %d) still runs", spec.Claim, live.pid)
	}
	l.seq++
	seq := l.seq
	l.mu.Unlock()

	// The daemon's own controller (`controller: daemon`) works on no issue, so it runs in a window
	// and a working directory of its own, named as the tmux runtime names them (controllerWindow):
	// a locator whose window is empty is one the store refuses to read back.
	window, workDir := strings.ToLower(spec.Issue), l.workDir(spec.Issue)
	if spec.Role == claim.RoleController {
		window, workDir = "controller", filepath.Join(l.stateDir, "controller")
	}
	if err := os.MkdirAll(workDir, 0o700); err != nil {
		return runtime.Locator{}, fmt.Errorf("spawn %s: make its working directory: %w", spec.Claim, err)
	}
	files := rigSecretFiles(l.stateDir, spec)
	if err := runtime.WriteSecretFiles(l.stateDir, files); err != nil {
		return runtime.Locator{}, fmt.Errorf("spawn %s: %w", spec.Claim, err)
	}
	prompt, err := runtime.ComposePrompt(spec.Prompt)
	if err != nil {
		return runtime.Locator{}, fmt.Errorf("spawn %s: %w", spec.Claim, err)
	}
	promptFile, err := runtime.WriteSystemPrompt(l.stateDir, string(spec.Claim), prompt)
	if err != nil {
		return runtime.Locator{}, fmt.Errorf("spawn %s: %w", spec.Claim, err)
	}
	variables, err := tmux.PaneVariables(spec, tmux.PaneInputs{
		StateDir: l.stateDir, Workspace: workDir, DaemonURL: l.daemonURL, EnvoyURL: l.envoyURL,
		DispatchURL: l.dispatchURL, DispatchTokenFile: l.dispatchTokenFile,
		NATSURLs: l.natsURLs, RoleConfigFiles: l.overlays(spec.Claim),
	}, files)
	if err != nil {
		return runtime.Locator{}, fmt.Errorf("spawn %s: %w", spec.Claim, err)
	}
	argv := []string{
		l.legion, "worker-shim",
		"--connect", r.relay.address,
		"--boot-token-file", files[0].Path,
	}
	if r.extraArgs != nil {
		argv = append(argv, r.extraArgs(spec)...)
	}
	argv = append(argv, "--", l.omp, "--mode", "rpc", "--append-system-prompt", promptFile)
	if spec.ResumeSessionFile != "" {
		argv = slices.Insert(argv, len(argv)-4, "--resume="+spec.ResumeSessionFile)
	}
	logPath := filepath.Join(l.paneLogs, fmt.Sprintf("%s.%d.log", spec.Claim, seq))
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return runtime.Locator{}, fmt.Errorf("spawn %s: open its log %s: %w", spec.Claim, logPath, err)
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = workDir
	cmd.Env = l.launchEnvironment(spec, variables, workDir)
	// Each pane keeps a state home of its own, as the sandbox runtime gives each role one
	// (roleStateHome), so its Oh My Pi runs a worker-daemon broker of its own; endRoleProcesses
	// ends what the pane's generation left under it once the pane's process group has ended.
	stateHome := rigRoleStateHome(l.stateDir, spec.Claim)
	if err := os.MkdirAll(stateHome, 0o700); err != nil {
		logFile.Close()
		return runtime.Locator{}, fmt.Errorf("spawn %s: make its state home: %w", spec.Claim, err)
	}
	cmd.Env = append(cmd.Env, "XDG_STATE_HOME="+stateHome)
	if r.extraEnv != nil {
		cmd.Env = append(cmd.Env, r.extraEnv(spec)...)
	}
	cmd.Stdout, cmd.Stderr = logFile, logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		logFile.Close()
		return runtime.Locator{}, fmt.Errorf("spawn %s: %w", spec.Claim, err)
	}
	pid := cmd.Process.Pid
	ticks, _, err := startTicks(pid)
	if err != nil {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		_ = cmd.Wait()
		logFile.Close()
		return runtime.Locator{}, fmt.Errorf("spawn %s: read pid %d's start ticks: %w", spec.Claim, pid, err)
	}
	pane := &localPane{
		spec: spec, cmd: cmd, pid: pid, log: logPath, done: make(chan struct{}),
		locator: runtime.Locator{
			Runtime:     runtime.RuntimeTmux,
			Claim:       spec.Claim,
			Incarnation: strconv.Itoa(pid) + ":" + strconv.FormatUint(ticks, 10),
			Tmux:        &runtime.TmuxLocator{Window: window, Pane: "%" + strconv.Itoa(pid)},
		},
	}
	l.mu.Lock()
	l.panes[spec.Claim] = pane
	l.started = append(l.started, pane)
	l.mu.Unlock()
	r.mu.Lock()
	r.launches = append(r.launches, &rigLaunch{spec: spec, pane: pane, at: time.Now()})
	r.mu.Unlock()
	go func() {
		_ = cmd.Wait()
		if ended := endRoleProcesses(stateHome, spec.Generation); len(ended) > 0 {
			fmt.Fprintf(logFile, "rig: the process group ended; ended the %d processes of generation %d left under its state home %s, as the generation's namespace ends them: %v\n",
				len(ended), spec.Generation, stateHome, ended)
		}
		logFile.Close()
		close(pane.done)
		l.report(runtime.Observation{Locator: pane.locator, Kind: runtime.Gone, At: time.Now(),
			Detail: "the worker's process group ended; its log is " + logPath})
	}()
	return pane.locator, nil
}

// rigRoleStateHome is the state home a claim's panes keep, every generation the same one, as the
// sandbox runtime keeps a role's under its scratch volume.
func rigRoleStateHome(stateDir string, token claim.Token) string {
	return filepath.Join(stateDir, "role-state", string(token))
}

// endRoleProcesses kills every process that generation left under stateHome, each whose
// environment names stateHome as its XDG_STATE_HOME and generation as its LEGION_GENERATION, which
// every process a pane starts inherits, and waits up to 5 s for each to go: what the generation's
// PID namespace's end takes with it in a pod (internal/launcher's role-init, Kill(-1, SIGKILL) at
// the generation's end), which a process-group kill does not reach — Oh My Pi's worker-daemon
// broker and the named services it runs, each in a session of its own. The state home is the
// claim's, every generation the same one, so the generation keeps the sweep to the namespace that
// ended: a later generation the daemon started beside a process the runtime reported gone while
// it ran (detach, w3) keeps its processes, as its own namespace does in a pod. It runs once the
// pane's process group has ended, before the pane is reported gone, so no next launch of the claim
// starts while they run. It answers the pids it killed.
func endRoleProcesses(stateHome string, generation uint64) []int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	home := "XDG_STATE_HOME=" + stateHome + "\x00"
	ofGeneration := "LEGION_GENERATION=" + strconv.FormatUint(generation, 10) + "\x00"
	var killed []int
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid == os.Getpid() {
			continue
		}
		env, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "environ"))
		if err != nil {
			continue
		}
		if variables := strings.SplitAfter(string(env), "\x00"); !slices.Contains(variables, home) || !slices.Contains(variables, ofGeneration) {
			continue
		}
		if syscall.Kill(pid, syscall.SIGKILL) == nil {
			killed = append(killed, pid)
		}
	}
	for _, pid := range killed {
		for deadline := time.Now().Add(5 * time.Second); alive(pid) && time.Now().Before(deadline); {
			time.Sleep(20 * time.Millisecond)
		}
	}
	sort.Ints(killed)
	return killed
}

// rigSecretFiles are the files a spec's secrets are written to, named as the tmux runtime names
// them: the boot token as `<claim>`, first, and each other secret as `<claim>-<lowercased name>`,
// sorted by name.
func rigSecretFiles(stateDir string, spec runtime.SpawnSpec) []runtime.SecretFile {
	files := []runtime.SecretFile{{Variable: "LEGION_BOOT_TOKEN", Path: runtime.SecretFilePath(stateDir, string(spec.Claim)), Value: spec.BootToken}}
	names := make([]string, 0, len(spec.Secrets))
	for name := range spec.Secrets {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		files = append(files, runtime.SecretFile{
			Variable: name,
			Path:     runtime.SecretFilePath(stateDir, string(spec.Claim)+"-"+strings.ToLower(name)),
			Value:    spec.Secrets[name],
		})
	}
	return files
}

// launchesOf is every launch of token, oldest first.
func (r *rigRuntime) launchesOf(token claim.Token) []*rigLaunch {
	r.mu.Lock()
	defer r.mu.Unlock()
	var launches []*rigLaunch
	for _, l := range r.launches {
		if l.spec.Claim == token {
			launches = append(launches, l)
		}
	}
	return launches
}

// current is token's latest launch, and nil before its first.
func (r *rigRuntime) current(token claim.Token) *rigLaunch {
	launches := r.launchesOf(token)
	if len(launches) == 0 {
		return nil
	}
	return launches[len(launches)-1]
}

// bootTokenOf is the claim and generation the launch that minted token was started for: what
// the relay names a connection by, since a boot token is a secret and is never printed.
func (r *rigRuntime) bootTokenOf(token string) (claim.Token, uint64, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, l := range r.launches {
		if l.spec.BootToken == token {
			return l.spec.Claim, l.spec.Generation, true
		}
	}
	return "", 0, false
}

// detach is the runtime saying token's current process is gone while that process keeps running
// and connected: the runtime forgets it, so the daemon's next launch of the claim starts a process
// beside it rather than waiting it out, and reports it gone, as BET's runtime reported the
// old pod's process at 20:26:16Z on 2026-11-12 while the kubelet could not kill it. It answers
// the launch it forgot, whose process the test's cleanup still kills.
func (r *rigRuntime) detach(token claim.Token) *rigLaunch {
	r.t.Helper()
	launch := r.current(token)
	if launch == nil {
		r.t.Fatalf("the rig's runtime has launched nothing for %s", token)
	}
	l := r.localRuntime
	l.mu.Lock()
	pane, ok := l.panes[token]
	if ok && pane == launch.pane {
		delete(l.panes, token)
	}
	l.mu.Unlock()
	if !ok || pane != launch.pane {
		r.t.Fatalf("%s's current launch is not the process this runtime records for it", token)
	}
	if !launch.running() {
		r.t.Fatalf("%s's process (pid %d) has already ended; there is nothing to report gone while it runs", token, launch.pane.pid)
	}
	l.report(runtime.Observation{Locator: launch.pane.locator, Kind: runtime.Gone, At: time.Now(),
		Detail: "the rig reports this process gone while it keeps running and connected"})
	return launch
}

// rigShim is a `legion worker-shim` a check started by hand, outside any launch.
type rigShim struct {
	name string
	cmd  *exec.Cmd
	pid  int
	log  string
	done chan struct{}
}

func (s *rigShim) running() bool {
	select {
	case <-s.done:
		return false
	default:
		return true
	}
}

// kill ends the shim's process group and waits for it.
func (s *rigShim) kill() {
	_ = killGroup(s.pid)
	select {
	case <-s.done:
	case <-time.After(15 * time.Second):
	}
}

// killGroup SIGKILLs the process group pid leads, as a person ends a process on its node.
func killGroup(pid int) error { return syscall.Kill(-pid, syscall.SIGKILL) }

// startShim runs a shim that dials the relay with bootToken, wrapping argv: a second process of a
// generation the daemon minted, or one with a token it never minted (w3). Its output is written
// beside the panes' logs. The wrapped process starts only once the daemon acks the hello, as in a
// pane, so a refused shim never runs it.
func (r *rigRuntime) startShim(name, bootToken string, argv ...string) *rigShim {
	r.t.Helper()
	dir := r.t.TempDir()
	tokenFile := filepath.Join(dir, "boot-token")
	writeProofFile(r.t, tokenFile, bootToken)
	logPath := filepath.Join(r.paneLogs, "shim-"+name+".log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		r.t.Fatalf("open %s: %v", logPath, err)
	}
	args := append([]string{"worker-shim", "--connect", r.relay.address, "--boot-token-file", tokenFile, "--"}, argv...)
	cmd := exec.Command(r.legion, args...)
	cmd.Dir = dir
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + dir, "TMPDIR=" + dir, "LANG=C.UTF-8"}
	cmd.Stdout, cmd.Stderr = logFile, logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		logFile.Close()
		r.t.Fatalf("start the shim %s: %v", name, err)
	}
	shim := &rigShim{name: name, cmd: cmd, pid: cmd.Process.Pid, log: logPath, done: make(chan struct{})}
	go func() {
		_ = cmd.Wait()
		logFile.Close()
		close(shim.done)
	}()
	r.mu.Lock()
	r.shims = append(r.shims, shim)
	r.mu.Unlock()
	return shim
}

func (r *rigRuntime) killShims() {
	r.mu.Lock()
	shims := slices.Clone(r.shims)
	r.mu.Unlock()
	for _, s := range shims {
		s.kill()
	}
}

// rigSession is an Oh My Pi session the rig started by hand, holding no Legion claim: `omp --mode
// rpc` with no shim around it, so no boot token, no daemon and no worker stream (w2's last step).
// A step writes its RPC commands to its stdin (send) and reads what it answered (answer); every
// line it writes is kept in its log beside the panes' logs.
type rigSession struct {
	name  string
	cmd   *exec.Cmd
	pid   int
	stdin io.WriteCloser
	log   string
	done  chan struct{}

	mu sync.Mutex
	// answers is each `response` frame it wrote, by the id of the request it answers; events is
	// the type of every other frame, in order.
	answers map[string]map[string]any
	events  []string
}

// startSession runs `omp --mode rpc <args>` in the rig's profile (r.home, r.profile) as a session
// that holds no Legion claim, in a working directory of its own under the state directory and a
// process group of its own, which the running step's cleanup kills. Its environment is a pane's
// (localRuntime.paneEnvironment) with no LEGION_ variable. Of what a claim's pane is told
// (tmux.PaneVariables) it keeps the overlays (PI_CONFIG_FILES), so its turns go to the run's
// model, and Envoy as a pane reaches it: the listener's front as ENVOY_URL, the bearer
// envoyTokenFile holds as ENVOY_TOKEN_FILE, and the rig's NATS as ENVOY_NATS_URL. Every other
// variable there is the claim's or the daemon's. env is added last: w2's SQL sessions name their
// store there.
func (r *rigRuntime) startSession(name, envoyTokenFile string, env []string, args ...string) *rigSession {
	r.t.Helper()
	workDir := filepath.Join(r.stateDir, "sessions-without-a-claim", name)
	if err := os.MkdirAll(workDir, 0o700); err != nil {
		r.t.Fatalf("make the working directory of the session %s: %v", name, err)
	}
	variables := []string{
		"PI_CONFIG_FILES=" + strings.Join(r.overlays(""), ":"),
		"ENVOY_URL=" + r.envoyURL,
		"ENVOY_TOKEN_FILE=" + envoyTokenFile,
		"ENVOY_NATS_URL=" + strings.Join(r.natsURLs, ","),
	}
	var environment []string
	for _, variable := range append(r.paneEnvironment(variables, workDir), env...) {
		if !strings.HasPrefix(variable, "LEGION_") {
			environment = append(environment, variable)
		}
	}
	logPath := filepath.Join(r.paneLogs, "session-"+name+".log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		r.t.Fatalf("open %s: %v", logPath, err)
	}
	cmd := exec.Command(r.omp, append([]string{"--mode", "rpc"}, args...)...)
	cmd.Dir = workDir
	cmd.Env = environment
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		logFile.Close()
		r.t.Fatalf("pipe the session %s's stdout: %v", name, err)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		logFile.Close()
		r.t.Fatalf("pipe the session %s's stdin: %v", name, err)
	}
	if err := cmd.Start(); err != nil {
		logFile.Close()
		r.t.Fatalf("start the session %s: %v", name, err)
	}
	s := &rigSession{name: name, cmd: cmd, pid: cmd.Process.Pid, stdin: stdin, log: logPath,
		done: make(chan struct{}), answers: map[string]map[string]any{}}
	read := make(chan struct{})
	go func() {
		defer close(read)
		// A frame has no bound the session keeps to (a streamed reply's update carries all of it),
		// so each line is read whole rather than by a scanner that would stop at one and leave
		// the session blocked on a full pipe.
		lines := bufio.NewReader(stdout)
		for {
			line, err := lines.ReadBytes('\n')
			if len(line) > 0 {
				_, _ = logFile.Write(line)
				s.keep(line)
			}
			if err != nil {
				return
			}
		}
	}()
	go func() {
		<-read
		_ = cmd.Wait()
		logFile.Close()
		close(s.done)
	}()
	r.t.Cleanup(s.kill)
	return s
}

// keep records one line the session wrote: a response under its request's id, any other frame
// by its type.
func (s *rigSession) keep(line []byte) {
	var frame map[string]any
	if json.Unmarshal(line, &frame) != nil {
		return
	}
	typ, _ := frame["type"].(string)
	s.mu.Lock()
	defer s.mu.Unlock()
	if id, ok := frame["id"].(string); ok && typ == "response" {
		s.answers[id] = frame
		return
	}
	s.events = append(s.events, typ)
}

// send writes one RPC command to the session's stdin.
func (s *rigSession) send(command map[string]any) error {
	line, err := json.Marshal(command)
	if err != nil {
		return err
	}
	_, err = s.stdin.Write(append(line, '\n'))
	return err
}

// answer is the session's response to the request id, and whether it has written one.
func (s *rigSession) answer(id string) (map[string]any, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	frame, ok := s.answers[id]
	return frame, ok
}

// saw is how many frames of typ the session has written.
func (s *rigSession) saw(typ string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, event := range s.events {
		if event == typ {
			n++
		}
	}
	return n
}

func (s *rigSession) running() bool {
	select {
	case <-s.done:
		return false
	default:
		return true
	}
}

// kill SIGKILLs the session's process group, Oh My Pi and every process its tools started, and
// waits for it.
func (s *rigSession) kill() {
	_ = killGroup(s.pid)
	select {
	case <-s.done:
	case <-time.After(15 * time.Second):
	}
}

// ---- the worker-stream relay ---------------------------------------------------------------------

// rigRelay carries every shim's connection to the daemon's worker stream: it accepts on a socket
// of its own, which every shim of the rig dials, reads the shim's hello, dials the daemon's stream
// (whatever address the daemon booted last bound it at) and copies each line both ways, recording
// what it carried. It changes nothing it carries but where a check asks it to:
//
//   - stubborn: a stop frame the daemon sends a bound connection is read and dropped, so the shim
//     and its process keep running, as a process that will not stop does (A2 of w3); a stop frame
//     before the hello's ack — a refusal of a generation the claim has left — is carried;
//   - cut: both ends of the connection closed at once, the daemon's included, while the shim's
//     process runs, as a network cut ends it;
//   - hold: a shim's redial is closed before it reaches the daemon, so the shim keeps dialling,
//     as a shim the network keeps from the daemon does;
//   - holdPrompts: the first prompt the daemon sends a connection of a claim, and every line after
//     it on that connection, waits in the relay until releasePrompts, as a daemon that answers late
//     sends its prompt late (w2's card that starts a resumed session's first turn).
type rigRelay struct {
	t        *testing.T
	path     string
	address  string
	upstream atomic.Value
	resolve  func(string) (claim.Token, uint64, bool)
	ln       net.Listener

	mu       sync.Mutex
	links    []*relayLink
	stubborn map[string]bool
	held     map[string]bool
	prompts  map[claim.Token]chan struct{}
	seq      int
}

// relayConn is the record of one connection the relay carried, named by the launch its boot
// token was minted for (the token itself is never printed): what a check reads, as a copy
// (connsOf).
type relayConn struct {
	id         int
	bootToken  string
	claim      claim.Token
	generation uint64
	known      bool
	opened     time.Time
	helloAt    time.Time
	held       bool
	ackAt      time.Time
	closedAt   time.Time
	cut        bool
	toShim     []relayFrame
	toDaemon   []relayFrame
}

// relayLink is one connection while the relay carries it: its record and its two sockets. Its
// own lock orders the daemon's socket arriving against an end, so a cut that lands while the
// relay is still dialling the daemon closes that socket too.
type relayLink struct {
	rec          relayConn
	mu           sync.Mutex
	ended        bool
	shim, daemon net.Conn
}

// relayFrame is one line the relay carried: its type, the request id and command a response
// answers, and whether a stubborn connection's drop kept it from the shim.
type relayFrame struct {
	at      time.Time
	typ     string
	id      string
	command string
	dropped bool
}

// label names a connection as a note prints it.
func (c relayConn) label() string {
	if !c.known {
		return fmt.Sprintf("connection %d (a boot token the rig's launches never minted)", c.id)
	}
	return fmt.Sprintf("connection %d (%s generation %d)", c.id, c.claim, c.generation)
}

// newRigRelay listens on a socket of its own, under a short directory: a unix socket's path is
// bounded at about a hundred bytes.
func newRigRelay(t *testing.T) *rigRelay {
	t.Helper()
	path := filepath.Join(shortTempDir(t), "relay.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen for the rig's worker-stream relay: %v", err)
	}
	r := &rigRelay{t: t, path: path, address: "unix://" + path, ln: ln, stubborn: map[string]bool{}, held: map[string]bool{},
		prompts: map[claim.Token]chan struct{}{}}
	r.upstream.Store("")
	t.Cleanup(r.close)
	go r.accept()
	return r
}

// setUpstream is the daemon's stream address, which every daemon boot hands its runtime.
func (r *rigRelay) setUpstream(address string) { r.upstream.Store(address) }

func (r *rigRelay) accept() {
	for {
		shim, err := r.ln.Accept()
		if err != nil {
			return
		}
		go r.carry(shim)
	}
}

func (r *rigRelay) close() {
	_ = r.ln.Close()
	r.mu.Lock()
	links := slices.Clone(r.links)
	for token, held := range r.prompts {
		close(held)
		delete(r.prompts, token)
	}
	r.mu.Unlock()
	for _, c := range links {
		c.end()
	}
}

func (c *relayLink) end() {
	c.mu.Lock()
	c.ended = true
	shim, daemon := c.shim, c.daemon
	c.mu.Unlock()
	if shim != nil {
		_ = shim.Close()
	}
	if daemon != nil {
		_ = daemon.Close()
	}
}

// withDaemon records the daemon's socket, and answers false when the link has already ended.
func (c *relayLink) withDaemon(daemon net.Conn) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ended {
		return false
	}
	c.daemon = daemon
	return true
}

// carry reads the shim's hello, decides what to do with the connection, and copies both ways.
func (r *rigRelay) carry(shim net.Conn) {
	c := &relayLink{shim: shim, rec: relayConn{opened: time.Now()}}
	r.mu.Lock()
	r.seq++
	c.rec.id = r.seq
	r.links = append(r.links, c)
	r.mu.Unlock()

	fromShim := bufio.NewReaderSize(shim, 64*1024)
	_ = shim.SetReadDeadline(time.Now().Add(30 * time.Second))
	hello, err := fromShim.ReadBytes('\n')
	_ = shim.SetReadDeadline(time.Time{})
	if err != nil {
		r.finish(c)
		return
	}
	token := ""
	if frame, err := shimwire.Decode(hello); err == nil {
		switch f := frame.(type) {
		case shimwire.Hello2:
			token = f.BootToken
		case shimwire.Hello:
			token = f.BootToken
		}
	}
	claimToken, generation, known := r.resolve(token)
	r.mu.Lock()
	c.rec.bootToken, c.rec.claim, c.rec.generation, c.rec.known, c.rec.helloAt = token, claimToken, generation, known, time.Now()
	held := r.held[token]
	c.rec.held = held
	r.mu.Unlock()
	if held {
		r.finish(c)
		return
	}
	address, _ := r.upstream.Load().(string)
	network, at, ok := strings.Cut(address, "://")
	if !ok {
		r.finish(c)
		return
	}
	daemon, err := net.DialTimeout(network, at, 10*time.Second)
	if err != nil {
		r.finish(c)
		return
	}
	if !c.withDaemon(daemon) {
		_ = daemon.Close()
		r.finish(c)
		return
	}
	if _, err := daemon.Write(hello); err != nil {
		r.finish(c)
		return
	}
	done := make(chan struct{}, 2)
	go func() { r.copyLines(c, bufio.NewReaderSize(daemon, 64*1024), shim, true); done <- struct{}{} }()
	go func() { r.copyLines(c, fromShim, daemon, false); done <- struct{}{} }()
	<-done
	r.finish(c)
	<-done
}

// copyLines copies one direction line by line, recording each line's frame.
func (r *rigRelay) copyLines(c *relayLink, from *bufio.Reader, to net.Conn, toShim bool) {
	for {
		line, err := from.ReadBytes('\n')
		if len(line) > 0 {
			frame := relayFrameOf(line)
			r.mu.Lock()
			if toShim {
				if frame.typ == shimwire.TypeHelloAck && c.rec.ackAt.IsZero() {
					c.rec.ackAt = frame.at
				}
				frame.dropped = frame.typ == shimwire.TypeShutdown && !c.rec.ackAt.IsZero() && r.stubborn[c.rec.bootToken]
				c.rec.toShim = append(c.rec.toShim, frame)
			} else {
				c.rec.toDaemon = append(c.rec.toDaemon, frame)
			}
			var held chan struct{}
			if toShim && frame.typ == shimwire.TypePrompt {
				held = r.prompts[c.rec.claim]
			}
			r.mu.Unlock()
			if held != nil {
				<-held
			}
			if !frame.dropped {
				if _, werr := to.Write(line); werr != nil {
					return
				}
			}
		}
		if err != nil {
			return
		}
	}
}

func relayFrameOf(line []byte) relayFrame {
	var head struct {
		Type    string `json:"type"`
		ID      any    `json:"id"`
		Command string `json:"command"`
	}
	_ = json.Unmarshal(line, &head)
	id := ""
	if head.ID != nil {
		id = fmt.Sprint(head.ID)
	}
	return relayFrame{at: time.Now(), typ: head.Type, id: id, command: head.Command}
}

func (r *rigRelay) finish(c *relayLink) {
	c.end()
	r.mu.Lock()
	if c.rec.closedAt.IsZero() {
		c.rec.closedAt = time.Now()
	}
	r.mu.Unlock()
}

// makeStubborn has every bound connection of bootToken read and drop the daemon's stop frame.
func (r *rigRelay) makeStubborn(bootToken string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stubborn[bootToken] = true
}

// hold closes every redial of bootToken before it reaches the daemon, until release.
func (r *rigRelay) hold(bootToken string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.held[bootToken] = true
}

func (r *rigRelay) release(bootToken string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.held, bootToken)
}

// holdPrompts has the relay hold the daemon's prompts to every connection of token's shims, the
// claim's next launches included, until releasePrompts.
func (r *rigRelay) holdPrompts(token claim.Token) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.prompts[token] == nil {
		r.prompts[token] = make(chan struct{})
	}
}

// releasePrompts carries every prompt holdPrompts held, and every line behind it, in order.
func (r *rigRelay) releasePrompts(token claim.Token) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if held := r.prompts[token]; held != nil {
		close(held)
		delete(r.prompts, token)
	}
}

// cut closes every open connection of bootToken at both ends at once, the daemon's included,
// while the shim's process runs, and answers when it did and how many it closed.
func (r *rigRelay) cut(bootToken string) (time.Time, int) {
	r.mu.Lock()
	var open []*relayLink
	for _, c := range r.links {
		if c.rec.bootToken == bootToken && c.rec.closedAt.IsZero() {
			c.rec.cut = true
			open = append(open, c)
		}
	}
	r.mu.Unlock()
	at := time.Now()
	for _, c := range open {
		c.end()
	}
	return at, len(open)
}

// connsOf is a copy of the record of every connection bootToken's shims made, oldest first.
func (r *rigRelay) connsOf(bootToken string) []relayConn {
	r.mu.Lock()
	defer r.mu.Unlock()
	var conns []relayConn
	for _, c := range r.links {
		if c.rec.bootToken == bootToken {
			copied := c.rec
			copied.toShim = slices.Clone(c.rec.toShim)
			copied.toDaemon = slices.Clone(c.rec.toDaemon)
			conns = append(conns, copied)
		}
	}
	return conns
}

// firstFrame is the first frame of typ the daemon sent c, and whether there was one.
func (c relayConn) firstFrame(typ string) (relayFrame, bool) {
	for _, f := range c.toShim {
		if f.typ == typ {
			return f, true
		}
	}
	return relayFrame{}, false
}

// typesToShim lists the types of the frames the daemon sent c, in order, with a dropped one
// marked, for a note.
func (c relayConn) typesToShim() string {
	var types []string
	for _, f := range c.toShim {
		word := f.typ
		if f.dropped {
			word += "(dropped)"
		}
		types = append(types, word)
	}
	if len(types) == 0 {
		return "none"
	}
	return strings.Join(types, ",")
}

// ---- the listener's front ------------------------------------------------------------------------

// rigFront is the address every pane and the daemon reach the listener at: a reverse proxy that
// carries every request to the real listener unchanged, but where a check asks it not to:
//
//   - blockRegistrations: a session's registration and its role claim (`/v1/interests/subscribe`,
//     `/v1/roles/set`, each naming the session in its body) end with the connection closed and no
//     answer, as a listener the session cannot reach ends them, while every other session and
//     every read go through (w5: "stop the registrations at the rig's listener");
//   - hold: every request waits unanswered until answer is called or its caller gives up, as a
//     listener that accepts a connection and does not answer (w4);
//   - hangNext: a session's next registration waits unanswered for as long as it is told or until
//     its caller gives up, and then its connection is closed, while every other request goes
//     through, as a listener that accepts one heartbeat's connection and does not answer it (w4).
//
// Every registration and role claim is recorded as it reaches the front, before any hold, with
// when it ended and the status it was answered with (arrivals), so a check counts a session's
// attempts through an outage (w4).
type rigFront struct {
	url    string
	target *url.URL
	proxy  *httputil.ReverseProxy

	mu      sync.Mutex
	blocked map[string]bool
	refused []rigFrontRefusal
	arrived []rigFrontArrival
	hangs   map[string]time.Duration
	held    chan struct{}
}

// rigFrontArrival is one registration or role claim as it reached the front: when, the session
// its body names, the route, whether hangNext held it, when it ended and the status it was
// answered with (0 for none: held until its caller gave up, refused, or hung).
type rigFrontArrival struct {
	at      time.Time
	session string
	path    string
	hung    bool
	ended   time.Time
	status  int
}

// rigFrontRefusal is one request the front ended for a blocked session.
type rigFrontRefusal struct {
	at      time.Time
	session string
	path    string
}

func newRigFront(t *testing.T, listenerURL string) *rigFront {
	t.Helper()
	target, err := url.Parse(listenerURL)
	if err != nil {
		t.Fatalf("parse the listener's address %s: %v", listenerURL, err)
	}
	f := &rigFront{target: target, proxy: httputil.NewSingleHostReverseProxy(target), blocked: map[string]bool{}, hangs: map[string]time.Duration{}}
	server := httptest.NewServer(f)
	t.Cleanup(server.Close)
	t.Cleanup(f.answer)
	f.url = server.URL
	return f
}

func (f *rigFront) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	registration := req.Method == http.MethodPost && (req.URL.Path == "/v1/interests/subscribe" || req.URL.Path == "/v1/roles/set")
	session, index := "", -1
	var answered *frontAnswer
	if registration {
		body, err := io.ReadAll(req.Body)
		req.Body.Close()
		if err != nil {
			http.Error(w, "read the request", http.StatusBadRequest)
			return
		}
		var named struct {
			SessionID string `json:"session_id"`
		}
		_ = json.Unmarshal(body, &named)
		session = named.SessionID
		req.Body = io.NopCloser(strings.NewReader(string(body)))
		req.ContentLength = int64(len(body))
		var hang time.Duration
		hang, index = f.arrive(session, req.URL.Path)
		defer func() { f.end(index, answered) }()
		if hang > 0 {
			select {
			case <-time.After(hang):
			case <-req.Context().Done():
			}
			closeUnanswered(w)
			return
		}
	}
	f.mu.Lock()
	held := f.held
	f.mu.Unlock()
	if held != nil {
		select {
		case <-held:
		case <-req.Context().Done():
			return
		}
	}
	if registration {
		f.mu.Lock()
		blocked := f.blocked[session]
		if blocked {
			f.refused = append(f.refused, rigFrontRefusal{at: time.Now(), session: session, path: req.URL.Path})
		}
		f.mu.Unlock()
		if blocked {
			closeUnanswered(w)
			return
		}
		answered = &frontAnswer{ResponseWriter: w}
		w = answered
	}
	f.proxy.ServeHTTP(w, req)
}

// closeUnanswered ends a request with its connection closed and nothing written.
func closeUnanswered(w http.ResponseWriter) {
	if hijacker, ok := w.(http.Hijacker); ok {
		if conn, _, err := hijacker.Hijack(); err == nil {
			_ = conn.Close()
			return
		}
	}
	panic(http.ErrAbortHandler)
}

// arrive records a registration as it reaches the front, and answers how long to hang it (0: not
// at all) and its index among the arrivals.
func (f *rigFront) arrive(session, path string) (time.Duration, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	hang := f.hangs[session]
	if path != "/v1/interests/subscribe" {
		hang = 0
	}
	if hang > 0 {
		delete(f.hangs, session)
	}
	f.arrived = append(f.arrived, rigFrontArrival{at: time.Now(), session: session, path: path, hung: hang > 0})
	return hang, len(f.arrived) - 1
}

// end records when the registration at index ended and, where the listener answered it, how.
func (f *rigFront) end(index int, answered *frontAnswer) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.arrived[index].ended = time.Now()
	if answered != nil {
		f.arrived[index].status = answered.status
	}
}

// frontAnswer is the status the listener answered a registration with, as the front passed it on.
type frontAnswer struct {
	http.ResponseWriter
	status int
}

func (a *frontAnswer) WriteHeader(status int) {
	if a.status == 0 {
		a.status = status
	}
	a.ResponseWriter.WriteHeader(status)
}

func (a *frontAnswer) Write(body []byte) (int, error) {
	if a.status == 0 {
		a.status = http.StatusOK
	}
	return a.ResponseWriter.Write(body)
}

func (a *frontAnswer) Unwrap() http.ResponseWriter { return a.ResponseWriter }

// arrivals is every registration and role claim of session that reached the front, oldest first.
func (f *rigFront) arrivals(session string) []rigFrontArrival {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []rigFrontArrival
	for _, a := range f.arrived {
		if a.session == session {
			out = append(out, a)
		}
	}
	return out
}

// hangNext holds session's next registration unanswered for up to d.
func (f *rigFront) hangNext(session string, d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hangs[session] = d
}

// blockRegistrations ends every registration and role claim of session until unblock.
func (f *rigFront) blockRegistrations(session string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.blocked[session] = true
}

func (f *rigFront) unblock(session string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.blocked, session)
}

// refusals is every request the front ended for session.
func (f *rigFront) refusals(session string) []rigFrontRefusal {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []rigFrontRefusal
	for _, r := range f.refused {
		if r.session == session {
			out = append(out, r)
		}
	}
	return out
}

// hold has every request wait unanswered until answer.
func (f *rigFront) hold() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.held == nil {
		f.held = make(chan struct{})
	}
}

// answer lets every held request through, and every later one.
func (f *rigFront) answer() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.held != nil {
		close(f.held)
		f.held = nil
	}
}
