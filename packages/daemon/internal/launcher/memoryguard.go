package launcher

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/sjawhar/legion/daemon/internal/memoryguard"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

// newMemoryGuard builds the pod's memory guard from the classifier, the sink and the log the
// launcher gives it (serveMemoryGuard). It is memoryguard.New, held in a package variable so that a
// test can point the guard the launcher built at a fake /proc, cgroup directory, clock and
// Signaler, which memoryguard.Config takes, and keep the classifier and the sink the launcher gave
// it.
var newMemoryGuard = memoryguard.New

// containerInit is whether this launcher is its container's first process, PID 1 of the
// container's PID namespace, the one rule that also starts the orphan reaper (Run). The pod's
// classifier holds only there: PID 1 sees every process of the container and owns each one no
// role-init does, the test database's postmaster is reparented to it, and a process under no
// role-init is no role's. A launcher run anywhere else (a test of cmd/legion, a launcher run by
// hand) shares its /proc with processes the classifier knows nothing of, and would end them. It is
// a package variable so that a test of the guard can stand its test process in for PID 1.
var containerInit = func() bool { return os.Getpid() == 1 }

// maxWaitingMemoryKills is the most reports of the guard's kills a role keeps while no connection
// can carry them. A role whose daemon stays away does not make PID 1 hold a report for every kill
// it makes in the meantime, and the guard's own line in the container's log keeps what a dropped
// report would have told the role.
const maxWaitingMemoryKills = 32

// serveMemoryGuard runs the issue pod's one memory guard (INFRA-3557) and each role's sender of
// that guard's kill reports until ctx ends, and answers a channel closed once all of them have
// returned, so Run can wait for it as it waits for the test database's ticker (serveTestPG). The
// controller's pod runs none: Run's single-role branch returns before this is reached.
//
// The guard runs only where the launcher is the container's first process (containerInit). Any
// other launcher logs one line and runs none: no manager then holds a guard, so serve hands no
// settings on and no report is ever queued.
//
// The guard runs at memoryguard's defaults from the start, before any role connects, so the pod is
// guarded while the daemon is away, and each role's acknowledgement then hands it the daemon's
// settings (serve). Its lines are the launcher's own, behind no role's prefix (roleLog.linef),
// because it watches every role's processes and the launcher's together. A kill of a process in a
// role is reported to that role's manager, which sends it on that role's connection
// (queueMemoryKill); a kill of a process in no role has the guard's own line and nothing else.
//
// A guard that cannot be built (its id could not be minted) is logged and the roles run without
// one, as a pod whose test database cannot start still runs its roles.
func serveMemoryGuard(ctx context.Context, cfg Config, managers []*manager, pids *pidGuard) <-chan struct{} {
	done := make(chan struct{})
	if !containerInit() {
		managers[0].log.linef("launcher: memory guard: off reason=the launcher is pid %d, not the container's first process, so it neither sees nor owns every process of the container",
			os.Getpid())
		close(done)
		return done
	}
	byRole := make(map[string]*manager, len(managers))
	for _, m := range managers {
		byRole[m.role] = m
	}
	guard, err := newMemoryGuard(memoryguard.Config{
		Classify: newPodClassifier(cfg.RolesDir, managers, pids).classify,
		Sink: func(k memoryguard.Kill) {
			if m := byRole[k.Role]; m != nil {
				m.queueMemoryKill(k)
			}
		},
		Logf: managers[0].log.linef,
	})
	if err != nil {
		managers[0].log.linef("launcher: memory guard: off reason=%v", err)
		close(done)
		return done
	}
	var running sync.WaitGroup
	for _, m := range managers {
		m.memoryGuard = guard
		m.memoryWake = make(chan struct{}, 1)
		running.Add(1)
		go func() {
			defer running.Done()
			m.sendMemoryKillsUntil(ctx)
		}()
	}
	running.Add(1)
	go func() {
		defer running.Done()
		guard.Run(ctx)
	}()
	go func() {
		running.Wait()
		close(done)
	}()
	return done
}

// The arguments the classifier knows Legion's and the agents' own processes by. Each is matched
// only at the place in the tree its process holds (podClassifier.classify), never on its own.
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
// the test database's script, the restore's jj and git) with its descendants, and the test
// database's server with its backends, since ending one backend makes the postmaster end and
// restart every backend, which stops every role's test database at once. In each role it spares
// the role-init itself, not its descendants; the shim, the oldest direct child of the role-init
// that is this binary run as `worker-shim` (an orphan the role leaves is adopted by the role-init
// too); every direct child of the shim, which is the agent before and after its start wrapper
// execs it, the secrets renewer, the working copy's adoption and the CodeGraph warm-up; Oh My Pi's
// helpers but its eval kernel; and a Claude Code role's `claude` and its MCP server.
//
// A process is the role's whose role-init is its ancestor by the parent chain, else the role's
// whose role-init shares its PID namespace, else no role's. The ancestor comes first because a
// process can make a PID namespace of its own inside a role (a sandboxed Chromium does), which the
// namespace alone would read as no role's; the namespace catches a role's process reparented out
// of the role-init's subtree.
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
		if again := filepath.Join(resolved, TestPGDirName, "data"); again != data {
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
	// The launcher's own children and the test database, each with every process under it.
	whole := held
	for _, p := range t.Children(c.launcher) {
		if filepath.Base(p.Exe) == "postgres" && c.isTestPG(t, p.PID) {
			whole = append(whole, p.PID)
		}
	}
	for seen := map[int]bool{}; len(whole) > 0; {
		pid := whole[0]
		whole = whole[1:]
		if seen[pid] {
			continue
		}
		seen[pid] = true
		protected[pid] = true
		for _, child := range t.Children(pid) {
			whole = append(whole, child.PID)
		}
	}

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
		shim := c.shim(t, init)
		if shim == nil {
			continue
		}
		protected[shim.PID] = true
		for _, agent := range t.Children(shim.PID) {
			protected[agent.PID] = true
			bridge := argument(agent, 1) == claudeBridgeArgument
			for _, p := range t.Children(agent.PID) {
				switch {
				case bridge:
					protected[p.PID] = true
					for _, server := range t.Children(p.PID) {
						if strings.HasSuffix(argument(server, 1), claudeMCPServerSuffix) {
							protected[server.PID] = true
						}
					}
				case ompHelper(agent, p):
					protected[p.PID] = true
				}
			}
		}
	}

	processes := t.Processes()
	classes := make(map[int]memoryguard.Class, len(processes))
	for _, p := range processes {
		role, found := ancestorRole(t, p, byInit, len(processes))
		if !found {
			role = byNamespace[p.PIDNamespace]
		}
		if class := (memoryguard.Class{Role: role, Protected: protected[p.PID]}); class != (memoryguard.Class{}) {
			classes[p.PID] = class
		}
	}
	return classes
}

// isTestPG is whether pid, a direct child of the launcher whose executable is a postgres, runs in
// the test database's data directory, as the postmaster `pg_ctl` leaves behind does.
func (c *podClassifier) isTestPG(t *memoryguard.Table, pid int) bool {
	cwd, err := t.Cwd(pid)
	return err == nil && slices.Contains(c.testPGData, cwd)
}

// shim is the role's shim: the oldest direct child of its role-init that is this binary run as
// `worker-shim`. Any younger one is an orphan of the role's that the role-init adopted, as the
// init of the role's PID namespace, and whatever it calls itself it is the role's work.
func (c *podClassifier) shim(t *memoryguard.Table, init *memoryguard.Process) *memoryguard.Process {
	if c.exe == "" {
		return nil
	}
	for _, p := range t.Children(init.PID) {
		if p.Exe == c.exe && argument(p, 1) == shimArgument {
			return p
		}
	}
	return nil
}

// ompHelper is whether p, a direct child of agent, a direct child of the shim, is one of Oh My
// Pi's own helpers: the agent's executable run with a helper's first argument.
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

// queueMemoryKill takes the guard's report of one of this role's processes it ended and wakes
// this role's sender, which writes it once a connection can carry it (sendMemoryKills). It is the
// guard's sink and runs on the guard's loop, so it waits on no connection: it only queues. A role
// already holding maxWaitingMemoryKills reports drops the oldest, with a line.
func (m *manager) queueMemoryKill(k memoryguard.Kill) {
	report := shimwire.LauncherMemoryKill{
		Guard: k.Guard, Sequence: k.Sequence, PID: k.PID, Command: k.Command, SizeBytes: k.SizeBytes,
		ReadingBytes: k.ReadingBytes, CounterBytes: k.CounterBytes, LimitBytes: k.LimitBytes, At: k.At,
	}
	var dropped *shimwire.LauncherMemoryKill
	m.mu.Lock()
	if len(m.memoryKills) == maxWaitingMemoryKills {
		oldest := m.memoryKills[0]
		dropped = &oldest
		m.memoryKills = slices.Delete(m.memoryKills, 0, 1)
	}
	m.memoryKills = append(m.memoryKills, report)
	m.mu.Unlock()
	if dropped != nil {
		m.logf("launcher: memory guard: report dropped sequence=%d pid=%d reason=%d reports already wait for a connection, the most a role keeps",
			dropped.Sequence, dropped.PID, maxWaitingMemoryKills)
	}
	select {
	case m.memoryWake <- struct{}{}:
	default:
	}
}

// sendMemoryKillsUntil is this role's sender of the guard's reports: it sends what waits each time
// the guard queues a report, until ctx ends. A connection that comes up later sends what waited
// for it itself (openMemoryReports).
func (m *manager) sendMemoryKillsUntil(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-m.memoryWake:
			m.sendMemoryKills()
		}
	}
}

// openMemoryReports makes writer, this role's new connection, the one the guard's reports of this
// role's processes go on, and sends the reports waiting for it. serve calls it after the
// connection's state frame, as it calls reportRestore, so the daemon reads the role's state
// first. carried is whether the connection's acknowledgement carried guard settings: one that
// carried none may come from a daemon that does not know the report, which closes a connection on
// a frame it does not know, so every report waiting then, and each made while that connection is
// up, is dropped with a line rather than sent.
func (m *manager) openMemoryReports(writer *shimwire.Writer, carried bool) {
	m.mu.Lock()
	m.reportWriter, m.reportsCarried = writer, carried
	m.mu.Unlock()
	m.sendMemoryKills()
}

// sendMemoryKills writes the reports waiting on this role's connection, oldest first, one at a
// time. The writer is taken under the manager's lock and written outside it, as state and
// reportRestore write theirs, and shimwire.Writer writes each frame whole under a lock of its own,
// so a report never interleaves with a command's answer. A report leaves the queue only once it is
// written; a write that fails leaves it, and the rest, for the next connection. memorySend holds
// one send at a time, so serve's send at a connection's start and the sender's never write one
// report twice or two out of order.
func (m *manager) sendMemoryKills() {
	m.memorySend.Lock()
	defer m.memorySend.Unlock()
	for {
		m.mu.Lock()
		writer, carried := m.reportWriter, m.reportsCarried
		if writer == nil || len(m.memoryKills) == 0 {
			m.mu.Unlock()
			return
		}
		if !carried {
			dropped := m.memoryKills
			m.memoryKills = nil
			m.mu.Unlock()
			for _, report := range dropped {
				m.logf("launcher: memory guard: report dropped sequence=%d pid=%d reason=this connection's acknowledgement carries no memory guard settings, so the daemon behind it may not know the report",
					report.Sequence, report.PID)
			}
			return
		}
		report := m.memoryKills[0]
		m.mu.Unlock()
		if err := writer.WriteFrame(report); err != nil {
			m.logf("launcher: memory guard: report unsent sequence=%d pid=%d reason=%v; the next connection carries it",
				report.Sequence, report.PID, err)
			return
		}
		m.mu.Lock()
		// The report is still the oldest unless the queue dropped it while it was being written.
		if len(m.memoryKills) > 0 && m.memoryKills[0].Sequence == report.Sequence {
			m.memoryKills = slices.Delete(m.memoryKills, 0, 1)
		}
		m.mu.Unlock()
	}
}
