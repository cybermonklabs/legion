package launcher

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/sjawhar/legion/daemon/internal/memoryguard"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

// newMemoryGuard builds the pod's memory guard from the classifier, the sink and the log the
// launcher gives it (serveMemoryGuard). It is memoryguard.New, held in a package variable so that a
// test can point the guard the launcher built at a fake /proc, cgroup directory, clock and
// Signaler, which memoryguard.Config takes, and keep the classifier and the sink the launcher gave
// it.
var newMemoryGuard = memoryguard.New

// newGuest builds a microVM pod's guest watch from the classifier, the sink and the log the
// launcher gives it (serveMemoryGuard). It is memoryguard.NewGuest, held in a package variable as
// newMemoryGuard is, so that a test can point the watch the launcher built at a fake /proc, cgroup
// directory and clock.
var newGuest = memoryguard.NewGuest

// guestSessionDrain is the guest watch's SessionDrain: how long after a role's session process
// leaves /proc a SIGKILL the role's processes show can still be its role-init's. Once the shim has
// exited, role-init writes the shim's status, sends SIGTERM over the role's namespace and drains
// for roleNamespaceDrain, asking whether the namespace is empty every namespaceReapPoll, so it sees
// the drain's end up to one poll and one reapNamespace pass late, and then sends kill(-1, SIGKILL)
// (runRoleShim, roleinit_linux.go). That lands milliseconds past the drain; the rest of the second
// is a margin for role-init running late under the memory pressure that raises the counter. A
// wider bound costs kills of that role left unnamed in a burst that starts within it after the
// departure, and a wrong name only within the one limit of the guest watch's rule ("What the sign
// does not show" in docs/kubernetes.md).
const guestSessionDrain = roleNamespaceDrain + time.Second

// controllerSessionDrain is the guest watch's SessionDrain in the controller's pod, whose launcher
// runs it in a microVM (INFRA-4567); stopGrace is Run's StopGrace, which Run has defaulted. The
// controller's pod has no role-init: once the shim has exited, the launcher, PID 1, drains the
// exited generation itself (cleanExitedGeneration, process.go): it sends SIGTERM over the
// container's processes, waits up to stopGrace for them to go, and then sends SIGKILL to each one
// left. So a SIGKILL the controller's processes show can be that drain's up to stopGrace after its
// session process leaves /proc, and the second past it is the margin guestSessionDrain gives
// role-init's kill to land.
func controllerSessionDrain(stopGrace time.Duration) time.Duration { return stopGrace + time.Second }

// containerInit is whether this launcher is its container's first process, PID 1 of the
// container's PID namespace, the one rule that also starts the orphan reaper (Run). Either form's
// classifier holds only there: PID 1 sees every process of the container and owns each one no
// role-init does (in the controller's pod, every one), the test database's postmaster is
// reparented to it, and a process under no role-init is no issue pod role's. A launcher run
// anywhere else (a test of cmd/legion, a launcher run by hand) shares its /proc with processes the
// classifier knows nothing of, and would end them. The clone's git lock sweep runs behind the same
// rule (serveGitLockSweep), since a pass removes a lock no process it sees holds open, and so
// does the pod's cache trimmer (serveVolumeTrim), since a sweep may delete a cache entry that no
// process it sees is using. It is a package variable so that a test of the guard, of the sweep
// or of the trimmer can stand its test process in for PID 1.
var containerInit = func() bool { return os.Getpid() == 1 }

// maxWaitingMemoryKills is the most reports of the guard's kills a role keeps while no connection
// can carry them. A role whose daemon stays away does not make PID 1 hold a report for every kill
// it makes in the meantime, and the guard's own line in the container's log keeps what a dropped
// report would have told the role.
const maxWaitingMemoryKills = 32

// serveMemoryGuard runs the pod's one memory watch and each role's sender of that watch's kill
// reports until ctx ends, and answers a channel closed once all of them have returned, so Run can
// wait for it as it waits for the test database's ticker (serveTestPG). Both of the launcher's
// forms run one, each with the classifier and the line writer of its form. An issue pod's
// (INFRA-3557) knows each process's role by the role-init above it (podClassifier), and its lines
// are the launcher's own, behind no role's prefix (roleLog.linef), because it watches every role's
// processes and the launcher's together. The controller pod's (INFRA-3531) has one role, which
// owns every process of the container (controllerClassifier), and its lines are that role's own
// (manager.logf): the container's log, where no line carries a prefix. Where the session database
// names a writer audience, the controller's pod runs the roles-dir form instead, whose role-init
// starts the controller's generation as it starts an issue pod role's, so its watch there is that
// form's: podClassifier, the roles log's lines and the role-init's drain, at that form's defaults
// until the controller's first acknowledgement hands it the controller pod's settings (serve).
//
// The watch runs only where the launcher is the container's first process (containerInit). Any
// other launcher logs one line, through its form's writer, and runs none: no manager then holds a
// guard, so serve hands no settings on and no report is ever queued.
//
// Under gVisor the watch is the memory guard (INFRA-3557), which ends a process before the host
// kernel ends the whole sandbox. It runs at its form's defaults from the start, before any role
// connects, so the pod is guarded while the daemon is away: an issue pod's at
// memoryguard.Defaults, the controller pod's at memoryguard.ControllerDefaults, its wait of 1
// second after a kill (INFRA-3531, R4), each what the daemon's configuration sends that kind of pod
// by default. Each role's acknowledgement then hands it the daemon's settings (serve). In a
// microVM pod (Config.runsGuest, the pod as built), an issue pod's (INFRA-3729) or the
// controller's (INFRA-4567), the pod's own kernel stops one process, so the guard is never built
// and no acknowledgement's settings reach one: the launcher logs one off line, through its form's
// writer, and runs the guest watch (memoryguard.Guest), which steers that kernel to the processes
// the guard would end, by its form's classifier, and reports each process it stopped through its
// form's sink, each report carrying one of the kernel's two causes, which serve sends where the
// acknowledgement carried KernelKills; the daemon sets it on the controller's acknowledgement as
// on an issue pod's. The watch's SessionDrain is its form's: guestSessionDrain in an issue pod,
// whose role-init drains a role, and controllerSessionDrain in the controller's, whose launcher
// drains the one generation itself.
// A kill of a process in a role is reported to that role's manager, which sends it on that role's
// connection (queueMemoryKill). In an issue pod a kill of a process of no role, which has the
// watch's own `ended` line as a role's has, is reported to the pod's one queue of such reports
// (noRoleReports), which sends each once, marked as of no role, on the connection of a role whose
// acknowledgement says the daemon behind it knows that report (noRoleKills), so the daemon counts
// every kill a line of the pod's log names. In the controller's pod every process is the
// controller's, so every kill is reported to its one manager. The controller's roles-dir form runs
// the issue pod's path, and the daemon never tells the controller's launcher that it knows a
// report of no role, so each such report there is dropped with a line.
func serveMemoryGuard(ctx context.Context, cfg Config, managers []*manager, pids *pidGuard) <-chan struct{} {
	done := make(chan struct{})
	controller := cfg.RolesDir == ""
	logf := managers[0].logf
	if !controller {
		logf = managers[0].log.linef
	}
	if !containerInit() {
		logf("launcher: memory guard: off reason=the launcher is pid %d, not the container's first process, so it neither sees nor owns every process of the container",
			os.Getpid())
		close(done)
		return done
	}
	var classify memoryguard.Classifier
	var sink func(memoryguard.Kill)
	var noRole *noRoleReports
	start := memoryguard.Defaults()
	drain := guestSessionDrain
	if controller {
		classify = newControllerClassifier(managers[0], pids).classify
		sink = func(k memoryguard.Kill) { managers[0].queueMemoryKill(k.LauncherMemoryKill) }
		start = memoryguard.ControllerDefaults()
		drain = controllerSessionDrain(cfg.StopGrace)
	} else {
		byRole := make(map[string]*manager, len(managers))
		for _, m := range managers {
			byRole[m.role] = m
		}
		noRole = newNoRoleReports(managers, logf)
		for _, m := range managers {
			m.noRoleReports = noRole
		}
		classify = newPodClassifier(cfg.RolesDir, managers, pids).classify
		sink = func(k memoryguard.Kill) {
			if m := byRole[k.Role]; m != nil {
				m.queueMemoryKill(k.LauncherMemoryKill)
				return
			}
			noRole.queue(k.LauncherMemoryKill)
		}
	}
	var watch func(context.Context)
	if cfg.runsGuest() {
		logf("launcher: memory guard: off reason=the pod runs in a microVM, whose own kernel stops one process")
		watch = newGuest(memoryguard.GuestConfig{Classify: classify, Sink: sink, Logf: logf, SessionDrain: drain}).Run
	} else {
		guard := newMemoryGuard(memoryguard.Config{Classify: classify, Sink: sink, Logf: logf, Start: start})
		for _, m := range managers {
			m.memoryGuard = guard
		}
		watch = guard.Run
	}
	var running sync.WaitGroup
	for _, m := range managers {
		m.memoryWake = make(chan struct{}, 1)
		running.Add(1)
		go func() {
			defer running.Done()
			m.killReportQueue.sendUntil(ctx, m.sendMemoryKills)
		}()
	}
	if noRole != nil {
		running.Add(1)
		go func() {
			defer running.Done()
			noRole.sendUntil(ctx)
		}()
	}
	running.Add(1)
	go func() {
		defer running.Done()
		watch(ctx)
	}()
	go func() {
		running.Wait()
		close(done)
	}()
	return done
}

// The arguments the classifier knows Legion's and the agents' own processes by. Each is matched
// only at the place in the tree its process holds (podClassifier.classify,
// controllerClassifier.classify), never on its own.
const (
	// shimArgument is a role's shim, `legion worker-shim`.
	shimArgument = "worker-shim"
	// ompHelperPrefix starts the first argument of each helper Oh My Pi starts from its own
	// executable. ompEvalKernel is the one such helper the guard may end: the kernel of Oh My
	// Pi's JavaScript eval tool runs the code the agent writes, as a Python kernel does, so its
	// memory is the agent's work and not Oh My Pi's own.
	ompHelperPrefix = "__omp_worker_"
	ompEvalKernel   = "__omp_worker_js_eval_process"
	// claudeBridgeArgument is a Claude Code role's agent, `legion claude-bridge -- claude …`
	// (claudelaunch.Argv), whose child is `claude`; claudeMCPServerSuffix ends the first
	// argument of Claude Code's MCP server, `bun <plugin dir>/dist/envoy-channel.js`
	// (claudelaunch.MCPConfig).
	claudeBridgeArgument  = "claude-bridge"
	claudeMCPServerSuffix = "/claude-envoy/dist/envoy-channel.js"
)

// podClassifier is the issue pod's memoryguard.Classifier: which processes the guard must spare,
// and whose role each process is. A process is known by its place in the process tree first and by
// its executable and arguments second, so a program that copies the name of one of these processes
// (an `omp` or a `worker-shim` the agent's own command runs) is not spared for it.
//
// It spares, outside every role, the launcher itself, each process the launcher holds (pidGuard:
// the test database's script, the leak-check install's run of the check, each cache trimmer
// sweep's deleter) with its descendants, and the test database's server with its backends, since
// ending one backend makes the postmaster end and restart every backend, which stops every role's
// test database at once. In each role it spares the role-init itself, not its
// descendants; the shim, the oldest direct child of the role-init that is this binary run as
// `worker-shim` (an orphan the role leaves is adopted by the role-init too); every direct child of
// the shim but the CodeGraph warm-up (codegraphWarmUp), which is the agent before and after its
// start wrapper execs it, the secrets renewer and the working copy's adoption, and never an orphan,
// since a shim under role-init adopts none (only the controller pod's single-role shim reaps
// orphans, reapingShim, and its classifier tells them from the shim's own, shimOwnChildren); Oh My
// Pi's helpers but its eval kernel, each reached from the agent through a chain of helpers; and a
// Claude Code role's `claude` and its MCP server. The warm-up and all it starts may be ended: the
// shim starts it during the agent's session load (warmCodegraph, internal/shim), its end ends no
// session, and the next start runs it again or finds the index its last build left.
//
// A process is the role's whose role-init is its ancestor by the parent chain, else the role's
// whose role-init shares its PID namespace, else no role's. The ancestor comes first because a
// process can make a PID namespace of its own inside a role (a sandboxed Chromium does), which the
// namespace alone would read as no role's; the namespace catches a role's process reparented out
// of the role-init's subtree.
//
// It marks as a session process (memoryguard.Class.Session), whose end ends its role's session,
// each role-init, its shim, and every direct child of the shim: the agent (a Claude Code role's
// bridge), and the shim's other children, which no place in the tree tells from it (the secrets
// renewer, the working copy's adoption, the CodeGraph warm-up). That superset of the agent costs
// kills left unnamed, and a wrong name only within the one limit of the guest watch's rule ("What
// the sign does not show" in docs/kubernetes.md). Nothing below the agent is one: an Oh My Pi
// helper can end alone.
//
// It keeps nothing a scan changes: each role-init is read under its manager's lock and the
// launcher's own children under the pid guard's lock, at each scan, so the guard's loop and its
// hourly scan may ask it at the same time.
type podClassifier struct {
	// launcher is this process, and exe its executable as /proc/self/exe reads it, once: a role's
	// shim is this same binary.
	launcher int
	exe      string
	// testPGData is the test database's data directory, where its postmaster and every backend
	// run, both as the launcher names it and with the links in the roles directory's path
	// resolved, since a cwd link reads the resolved path (the image's /var/run is a link to
	// /run).
	testPGData []string
	managers   []*manager
	pids       *pidGuard
}

// newPodClassifier is the classifier of the pod whose roles directory is rolesDir, whose roles
// managers serve and whose launcher holds pids.
func newPodClassifier(rolesDir string, managers []*manager, pids *pidGuard) *podClassifier {
	exe, _ := os.Readlink("/proc/self/exe")
	data := filepath.Join(testPGDir(rolesDir), "data")
	paths := []string{data}
	if resolved, err := filepath.EvalSymlinks(rolesDir); err == nil {
		if again := filepath.Join(testPGDir(resolved), "data"); again != data {
			paths = append(paths, again)
		}
	}
	return &podClassifier{launcher: os.Getpid(), exe: exe, testPGData: paths, managers: managers, pids: pids}
}

// runningRole is one role's running generation: the role-init that is the init of its namespaces.
type runningRole struct {
	pid  int
	role string
}

// classify is the classifier itself (memoryguard.Classifier).
func (c *podClassifier) classify(t *memoryguard.Table) map[int]memoryguard.Class {
	c.pids.mu.Lock()
	held := c.pids.held()
	c.pids.mu.Unlock()
	var inits []runningRole
	for _, m := range c.managers {
		m.mu.Lock()
		if m.child != nil {
			inits = append(inits, runningRole{pid: m.child.pid, role: m.role})
		}
		m.mu.Unlock()
	}

	protected := map[int]bool{c.launcher: true}
	session := map[int]bool{}
	// The launcher's own children and the test database, each with every process under it.
	whole := held
	for _, p := range t.Children(c.launcher) {
		if filepath.Base(p.Exe) == "postgres" && c.isTestPG(t, p.PID) {
			whole = append(whole, p.PID)
		}
	}
	protectTrees(t, whole, protected)

	byInit := make(map[int]string, len(inits))
	byNamespace := make(map[string]string, len(inits))
	for _, ri := range inits {
		init, ok := t.Get(ri.pid)
		if !ok {
			continue
		}
		byInit[init.PID] = ri.role
		if init.PIDNamespace != "" {
			byNamespace[init.PIDNamespace] = ri.role
		}
		protected[init.PID] = true
		session[init.PID] = true
		shim := roleShim(t, init, c.exe)
		if shim == nil {
			continue
		}
		protected[shim.PID] = true
		session[shim.PID] = true
		for _, agent := range t.Children(shim.PID) {
			session[agent.PID] = true
			if codegraphWarmUp(agent) {
				continue
			}
			protectAgent(t, agent, protected)
		}
	}

	processes := t.Processes()
	classes := make(map[int]memoryguard.Class, len(processes))
	for _, p := range processes {
		role, found := ancestorRole(t, p, byInit, len(processes))
		if !found {
			role = byNamespace[p.PIDNamespace]
		}
		if class := (memoryguard.Class{Role: role, Protected: protected[p.PID], Session: session[p.PID]}); class != (memoryguard.Class{}) {
			classes[p.PID] = class
		}
	}
	return classes
}

// protectTrees spares each of roots and every process under it. It takes each pid once: the scan
// reads one process at a time, so a pid reused between two reads can make the parent links a loop.
func protectTrees(t *memoryguard.Table, roots []int, protected map[int]bool) {
	for seen := map[int]bool{}; len(roots) > 0; {
		pid := roots[0]
		roots = roots[1:]
		if seen[pid] {
			continue
		}
		seen[pid] = true
		protected[pid] = true
		for _, child := range t.Children(pid) {
			roots = append(roots, child.PID)
		}
	}
}

// controllerClassifier is the controller pod's memoryguard.Classifier (INFRA-3531): which
// processes the guard must spare, and whose role each process is. The controller's pod has no
// role-init: the launcher is PID 1 of the controller's container, and its generation is the shim
// itself, started in the container's own namespaces (manager.command). A process is known by its
// place in the tree first and by its executable and arguments second, as podClassifier knows one,
// so a program that copies the name or the arguments of a protected one anywhere else in the tree
// (an `omp` copy or a helper's argument under another executable below the agent, a `worker-shim`
// that is not the manager's generation) is not spared for it.
//
// It spares the launcher itself; each process the launcher holds (pidGuard) with its descendants;
// the shim, the manager's running generation, only while that process
// is this binary run as `worker-shim`, since the manager keeps its generation's pid until its wait
// is done with it, and once the shim is reaped the kernel may give that pid to another process;
// and each of the shim's own children with what is the agent's own under it (protectAgent): the
// agent before and after its start wrapper execs it, Oh My Pi's helpers but its eval kernel, and
// the secrets renewer and its keygen, which are Legion's own by construction (shimOwnChildren). A
// command the agent runs is the agent's child, never the shim's; one it leaves behind becomes the
// shim's child, since this pod's shim reaps orphans (reapingShim), and stays the controller's work
// as it was when the launcher adopted it.
//
// Every process of the container is the controller's, protected or not: no other role runs
// there, so a kill of an orphan the launcher or the shim adopted, or of a process a `kubectl exec`
// started under no shim, is reported to the controller as a kill of its agent's command is, and
// every line of the guard's names the manager's role.
//
// It marks as a session process (memoryguard.Class.Session) the shim and each of the shim's own
// children, as podClassifier marks a role's (the controller's pod has no role-init): the agent
// and the shim's other children, which no place in the tree tells from it. Only the guest watch
// reads the mark, which a microVM controller pod's launcher runs over this classifier
// (serveMemoryGuard, INFRA-4567); the guard a gVisor controller pod's launcher runs ignores it.
//
// It keeps nothing a scan changes: the shim's pid is read under the manager's lock and the
// launcher's own children under the pid guard's lock, at each scan, so the guard's loop and its
// hourly scan may ask it at the same time.
type controllerClassifier struct {
	// launcher is this process, and exe its executable as /proc/self/exe reads it, once: the shim
	// is this same binary.
	launcher int
	exe      string
	manager  *manager
	pids     *pidGuard
}

// newControllerClassifier is the classifier of the controller's pod, whose one role m serves and
// whose launcher holds pids.
func newControllerClassifier(m *manager, pids *pidGuard) *controllerClassifier {
	exe, _ := os.Readlink("/proc/self/exe")
	return &controllerClassifier{launcher: os.Getpid(), exe: exe, manager: m, pids: pids}
}

// classify is the classifier itself (memoryguard.Classifier).
func (c *controllerClassifier) classify(t *memoryguard.Table) map[int]memoryguard.Class {
	c.pids.mu.Lock()
	held := c.pids.held()
	c.pids.mu.Unlock()
	shimPID := 0
	c.manager.mu.Lock()
	if c.manager.child != nil {
		shimPID = c.manager.child.pid
	}
	c.manager.mu.Unlock()

	protected := map[int]bool{c.launcher: true}
	session := map[int]bool{}
	protectTrees(t, held, protected)
	if shim, ok := t.Get(shimPID); ok && isShim(c.exe, shim) {
		protected[shim.PID] = true
		session[shim.PID] = true
		for _, agent := range shimOwnChildren(t, shim) {
			session[agent.PID] = true
			protectAgent(t, agent, protected)
		}
	}

	processes := t.Processes()
	classes := make(map[int]memoryguard.Class, len(processes))
	for _, p := range processes {
		classes[p.PID] = memoryguard.Class{Role: c.manager.role, Protected: protected[p.PID], Session: session[p.PID]}
	}
	return classes
}

// shimOwnChildren are the direct children of shim that the shim started itself. The controller
// pod's single-role shim, the one shim this classifier reads, reaps orphans (reapOrphansFlag,
// which reapingShim gives it), so it also has for direct children the orphans its tree leaves,
// which nothing in the tree tells from its own but their age and their executable: its own are
// the agent, the oldest direct child that is not the secrets binary, since an orphan descends from
// the agent's commands and so is younger than the agent, and the secrets keygen and renewer, the
// executable the shim's shimwire.AgentSecretsBinFlag names run as shimwire.AgentSecretsKeygen or
// shimwire.AgentSecretsRenew. Any other direct child is an orphan the shim adopted. An orphan that
// runs that executable as either task is taken for the shim's own: that is the rule's cost.
func shimOwnChildren(t *memoryguard.Table, shim *memoryguard.Process) []*memoryguard.Process {
	children := t.Children(shim.PID)
	options := shim.Args
	if end := slices.Index(options, "--"); end >= 0 {
		options = options[:end]
	}
	secrets := flagValue(options, "--"+shimwire.AgentSecretsBinFlag)
	resolved := secrets
	if secrets != "" {
		if path, err := filepath.EvalSymlinks(secrets); err == nil {
			resolved = path
		}
	}
	var own []*memoryguard.Process
	agent := false
	for _, p := range children {
		task := argument(p, 1)
		switch {
		case secrets != "" && p.Exe != "" && (p.Exe == secrets || p.Exe == resolved) && (task == shimwire.AgentSecretsKeygen || task == shimwire.AgentSecretsRenew):
			own = append(own, p)
		case !agent:
			agent = true
			own = append(own, p)
		}
	}
	return own
}

// flagValue is the value of the flag name among a command's options, written as `name value` or
// `name=value`, and "" where they hold none.
func flagValue(options []string, name string) string {
	for i, option := range options {
		if option == name && i+1 < len(options) {
			return options[i+1]
		}
		if value, ok := strings.CutPrefix(option, name+"="); ok {
			return value
		}
	}
	return ""
}

// protectAgent spares agent, a direct child of the shim (a role's in an issue pod, the one
// generation in the controller's), and what runs under it that is the agent's own rather than its
// work: under a Claude Code bridge, `claude` and its MCP server; under Oh My Pi, every helper
// reached from the agent through a chain of helpers, since a helper starts helpers of its own (the
// daemon broker starts the LSP multiplexer). What a helper runs that is no helper (the
// multiplexer's gopls), the eval kernel wherever it runs with all it starts, and a helper's name
// under the agent's command, which no chain of helpers reaches, stay the agent's work.
func protectAgent(t *memoryguard.Table, agent *memoryguard.Process, protected map[int]bool) {
	protected[agent.PID] = true
	if argument(agent, 1) == claudeBridgeArgument {
		for _, p := range t.Children(agent.PID) {
			protected[p.PID] = true
			for _, server := range t.Children(p.PID) {
				if strings.HasSuffix(argument(server, 1), claudeMCPServerSuffix) {
					protected[server.PID] = true
				}
			}
		}
		return
	}
	// The walk goes down from the agent through helpers alone and takes each pid once: the scan
	// reads one process at a time, so a pid reused between two reads can make the parent links a
	// loop.
	parents := []int{agent.PID}
	for seen := map[int]bool{agent.PID: true}; len(parents) > 0; {
		parent := parents[0]
		parents = parents[1:]
		for _, p := range t.Children(parent) {
			if !seen[p.PID] && ompHelper(agent, p) {
				seen[p.PID] = true
				protected[p.PID] = true
				parents = append(parents, p.PID)
			}
		}
	}
}

// codegraphWarmUp is whether p, a direct child of a role's shim, is the CodeGraph warm-up rather
// than the agent: the image's `codegraph` command (runCodegraph, internal/workspace), a shell
// script that execs its bundled node on lib/dist/bin/codegraph.js, so the warm-up reads as that
// script under its shell until the exec (argument 1 is the script, or argument 0 where it runs as
// itself) and as node on codegraph.js after it. Each is matched by a whole base name, `codegraph`
// or `codegraph.js`, never by a part of one, so an agent whose arguments name the CodeGraph plugin
// (`pi-codegraph`) is not taken for the warm-up.
func codegraphWarmUp(p *memoryguard.Process) bool {
	for i, arg := range p.Args {
		if base := filepath.Base(arg); (base == "codegraph" && i <= 1) || base == "codegraph.js" {
			return true
		}
	}
	return false
}

// isTestPG is whether pid, a direct child of the launcher whose executable is a postgres, runs in
// the test database's data directory, as the postmaster `pg_ctl` leaves behind does.
func (c *podClassifier) isTestPG(t *memoryguard.Table, pid int) bool {
	cwd, err := t.Cwd(pid)
	return err == nil && slices.Contains(c.testPGData, cwd)
}

// roleShim is the shim of the role whose role-init is init: the oldest direct child of init that
// is exe run as `worker-shim` (isShim), where exe is the launcher's own executable, since a role's
// shim is the launcher's binary. Any younger one is an orphan of the role's that the role-init
// adopted, as the init of the role's PID namespace, and whatever it calls itself it is the role's
// work. It is the one rule for the shim: the pod's classifier (podClassifier.classify) and a
// generation's session peer check (sessionPeer) both call it. With no exe, which a launcher that
// cannot read its own executable has, no process is the shim.
func roleShim(t *memoryguard.Table, init *memoryguard.Process, exe string) *memoryguard.Process {
	for _, p := range t.Children(init.PID) {
		if isShim(exe, p) {
			return p
		}
	}
	return nil
}

// isShim is whether p is a role's shim: exe, the launcher's own executable where it could be read,
// run as `worker-shim`.
func isShim(exe string, p *memoryguard.Process) bool {
	return exe != "" && p.Exe == exe && argument(p, 1) == shimArgument
}

// ompHelper is whether p, a child of agent (a direct child of the shim) or of one of agent's
// helpers, is one of Oh My Pi's own helpers: the agent's executable run with a helper's first
// argument other than the eval kernel's.
func ompHelper(agent, p *memoryguard.Process) bool {
	first := argument(p, 1)
	return p.Exe != "" && p.Exe == agent.Exe && strings.HasPrefix(first, ompHelperPrefix) && first != ompEvalKernel
}

// argument is p's argument i, "" where it has none.
func argument(p *memoryguard.Process, i int) string {
	if i < len(p.Args) {
		return p.Args[i]
	}
	return ""
}

// ancestorRole is the role whose role-init is p or one of p's ancestors by the parent chain. A
// chain longer than the table's processes is one a pid reused between two reads of the scan made
// into a loop, and has no role-init.
func ancestorRole(t *memoryguard.Table, p *memoryguard.Process, byInit map[int]string, bound int) (string, bool) {
	for q, steps := p, 0; q != nil && steps <= bound; steps++ {
		if role, ok := byInit[q.PID]; ok {
			return role, true
		}
		q, _ = t.Get(q.PPID)
	}
	return "", false
}

// queueMemoryKill queues a role's report without waiting for its connection.
func (m *manager) queueMemoryKill(report shimwire.LauncherMemoryKill) {
	m.offer(&m.mu, &m.memoryKills, report, m.logf,
		"launcher: memory guard: report dropped sequence=%d pid=%d reason=%d reports already wait for a connection, the most a role keeps")
}

// killReportQueue serializes sends and protects the in-flight head from overflow. Each caller
// supplies the pending slice and its lock. A role uses the same lock for the queue and its carrier.
type killReportQueue struct {
	memoryWake chan struct{}
	send       sync.Mutex
	sending    uint64
}

// offer never waits for a connection. It drops the oldest report if full, or the next oldest
// while a write of the head is underway: that write may still succeed.
func (q *killReportQueue) offer(mu *sync.Mutex, pending *[]shimwire.LauncherMemoryKill, report shimwire.LauncherMemoryKill, logf func(string, ...any), full string) {
	mu.Lock()
	var dropped shimwire.LauncherMemoryKill
	fullQueue := len(*pending) >= maxWaitingMemoryKills
	if fullQueue {
		i := 0
		if (*pending)[0].Sequence == q.sending {
			i = 1
		}
		dropped = (*pending)[i]
		*pending = slices.Delete(*pending, i, i+1)
	}
	*pending = append(*pending, report)
	mu.Unlock()
	if fullQueue {
		logf(full, dropped.Sequence, dropped.PID, maxWaitingMemoryKills)
	}
	select {
	case q.memoryWake <- struct{}{}:
	default:
	}
}

// sendUntil sends when the watch queues a report. A new connection also calls the sender directly.
func (q *killReportQueue) sendUntil(ctx context.Context, send func()) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-q.memoryWake:
			send()
		}
	}
}

// openMemoryReports makes writer, this role's new connection, the one the kill reports of this
// role's processes go on, and sends the reports waiting for it. serve calls it after the
// connection's state frame, so the daemon reads the role's state first. carried is whether the
// connection's acknowledgement says the daemon behind it knows the report (serve): guard settings
// for a gVisor pod's launcher, KernelKills for a microVM pod's. A daemon that does not know the
// report may close a connection on a frame it does not know, or tell a role of the kernel's kill
// in the guard's words, so where carried is false every report waiting then, and each made while
// that connection is up, is dropped with a line rather than sent. noRole is whether it also says
// the daemon knows a report of a process of no role (NoRoleKills, beside what carried asks), which
// makes the connection one the pod's reports of no role can go on, and those waiting for such a
// connection are sent then too (noRoleReports.sendWaiting).
func (m *manager) openMemoryReports(writer *shimwire.Writer, carried, noRole bool) {
	m.mu.Lock()
	m.reportWriter, m.reportsCarried, m.reportsNoRole = writer, carried, noRole
	m.mu.Unlock()
	m.sendMemoryKills()
	if m.noRoleReports != nil {
		m.noRoleReports.sendWaiting()
	}
}

// sendMemoryKills selects the role's carrier. Older acknowledgements do not carry kill reports.
func (m *manager) sendMemoryKills() {
	carrier := func() (*shimwire.Writer, string) {
		if m.reportWriter == nil || m.reportsCarried {
			return m.reportWriter, ""
		}
		if m.cfg.runsGuest() {
			return nil, "this connection's acknowledgement carries no kernelKills, so the daemon behind it may not know the report"
		}
		return nil, "this connection's acknowledgement carries no memory guard settings, so the daemon behind it may not know the report"
	}
	m.killReportQueue.sendWaiting(&m.mu, &m.memoryKills, carrier, m.logf,
		"launcher: memory guard: report dropped sequence=%d pid=%d reason=%s",
		"launcher: memory guard: report unsent sequence=%d pid=%d reason=%v; the next connection carries it")
}

// noRoleReports is an issue pod's one queue of the reports of the processes of no role its watch
// stopped (serveMemoryGuard's sink): a process no role-init is the ancestor of and that shares no
// role-init's PID namespace, such as the test database's server or what a `kubectl exec` started.
// The watch writes its `ended` line for such a kill as for a role's, and the daemon counts every
// kill an issue pod's lines name, so the report goes to the daemon too, marked as of no role
// (shimwire.LauncherMemoryKill.NoRole); but no role's connection is its own, so it waits here, at
// most maxWaitingMemoryKills of them as a role's queue keeps, and goes once, oldest first, on the
// connection of the first role of the pod's role list whose acknowledgement says the daemon behind
// it knows a report of no role (manager.reportsNoRole). That first role is the issue's architect,
// whose connection lasts the pod's life (serveVolumeTrim).
//
// A daemon that does not know the report would tell the role whose connection carried it of a
// kill that was not its own, so a report is never written on any other connection. Where no
// connection is up, the report waits for one. Where connections are up and none of them can carry
// it, which is a daemon built before the report (its acknowledgement carries no noRoleKills) or
// the controller's pod in the roles-dir form, which the daemon keeps no kill of, every report
// waiting then is dropped with the launcher's own line rather than kept for a daemon that may
// never come; the `ended` line in the pod's log stays.
//
// Its lines are the launcher's own, behind no role's prefix. The queue locks before the managers
// when it looks for a carrier. A manager never calls its sender under the manager's lock.
type noRoleReports struct {
	killReportQueue
	managers []*manager
	logf     func(format string, args ...any)
	mu       sync.Mutex
	kills    []shimwire.LauncherMemoryKill
}

// newNoRoleReports is the queue of the reports of no role of the pod whose roles managers serve,
// in the order of the pod's role list, with its lines written through logf.
func newNoRoleReports(managers []*manager, logf func(format string, args ...any)) *noRoleReports {
	return &noRoleReports{
		killReportQueue: killReportQueue{memoryWake: make(chan struct{}, 1)},
		managers:        managers,
		logf:            logf,
	}
}

// queue takes the watch's report of a process of no role it stopped, marks it so, and wakes the
// queue's sender. It is the watch's sink and runs on the watch's loop, so, as queueMemoryKill, it
// waits on no connection: it only queues.
func (q *noRoleReports) queue(report shimwire.LauncherMemoryKill) {
	report.NoRole = true
	q.offer(&q.mu, &q.kills, report, q.logf,
		"launcher: memory guard: report dropped sequence=%d pid=%d role=none reason=%d reports of no role already wait for a connection, the most the pod keeps")
}

func (q *noRoleReports) sendUntil(ctx context.Context) {
	q.killReportQueue.sendUntil(ctx, q.sendWaiting)
}

// carrier returns the first connection that knows reports of no role, or a reason to drop the
// reports if connections are up but none know them. With no connections it returns no reason.
func (q *noRoleReports) carrier() (*shimwire.Writer, string) {
	up := false
	for _, m := range q.managers {
		m.mu.Lock()
		writer, noRole := m.reportWriter, m.reportsNoRole
		m.mu.Unlock()
		if writer == nil {
			continue
		}
		if noRole {
			return writer, ""
		}
		up = true
	}
	if up {
		return nil, "no connection's acknowledgement says the daemon behind it knows a report of a process of no role (noRoleKills)"
	}
	return nil, ""
}

func (q *noRoleReports) sendWaiting() {
	q.killReportQueue.sendWaiting(&q.mu, &q.kills, q.carrier, q.logf,
		"launcher: memory guard: report dropped sequence=%d pid=%d role=none reason=%s",
		"launcher: memory guard: report unsent sequence=%d pid=%d role=none reason=%v; the next connection that can carry it does")
}

// sendWaiting selects the carrier under the queue's lock and writes outside it. A failed write
// leaves the head for the next connection. A missing carrier with a reason drops each report.
func (q *killReportQueue) sendWaiting(mu *sync.Mutex, pending *[]shimwire.LauncherMemoryKill, carrier func() (*shimwire.Writer, string), logf func(string, ...any), unsupported, unsent string) {
	q.send.Lock()
	defer q.send.Unlock()
	for {
		mu.Lock()
		if len(*pending) == 0 {
			mu.Unlock()
			return
		}
		writer, reason := carrier()
		if writer == nil {
			if reason == "" {
				mu.Unlock()
				return
			}
			dropped := *pending
			*pending = nil
			mu.Unlock()
			for _, report := range dropped {
				logf(unsupported, report.Sequence, report.PID, reason)
			}
			return
		}
		report := (*pending)[0]
		q.sending = report.Sequence
		mu.Unlock()
		err := writer.WriteFrame(report)
		mu.Lock()
		q.sending = 0
		if err == nil {
			*pending = slices.Delete(*pending, 0, 1)
		}
		mu.Unlock()
		if err != nil {
			logf(unsent, report.Sequence, report.PID, err)
			return
		}
	}
}
