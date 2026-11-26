package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"

	"github.com/sjawhar/envoy/going/agentwire"
)

// The supervisor's bounds (section 5 of the background-runs design).
const (
	// agentMemoryEvery is how often the supervisor reads the agent's resident memory.
	agentMemoryEvery = 50 * time.Millisecond
	// agentRenewalsMissed is how long the agent may go without a renew, three of the renews the
	// server answers every 10 s, before the supervisor kills it as stuck as a whole: a SIGSTOP,
	// a deadlock, a runtime that no longer schedules.
	agentRenewalsMissed = 30 * time.Second
	// agentRestartFirst is the wait before the agent starts again after an exit, and
	// agentRestartMost the longest: each quick exit doubles the wait before it.
	agentRestartFirst = time.Second
	agentRestartMost  = 30 * time.Second
	// agentQuickExit is the run below which an exit is quick: an agent that exits within a minute
	// of its start.
	agentQuickExit = time.Minute
	// agentKillWindow is the hour within which two kills for memory no run alone caused halve the
	// agent's slots, and the hour they stay halved for.
	agentKillWindow = time.Hour
	// agentMaxProcs is the most CPUs the agent's runtime schedules on (GOMAXPROCS).
	agentMaxProcs = 4
	// agentNice is the agent's priority where the kernel refuses it SCHED_IDLE (lowerPriority):
	// nice 10, beside the server's 0.
	agentNice = 10
	// lowerPriorityPasses bounds the passes lowerPriority makes over the agent's threads.
	lowerPriorityPasses = 10
	// agentExitGrace is how long the server's close waits for the agent process, whose standard
	// input it closed, to exit before it kills it, and then for the killed process to be reaped.
	agentExitGrace = 2 * time.Second
	// agentExitedTimeout bounds the database work of an exit: freeing the runs the agent held.
	agentExitedTimeout = 10 * time.Second
	// agentExitRecordWait is the record's share of the server's shutdown: how long the server's
	// close waits for that database work once the agent process has exited, after which the
	// record is cancelled and the runs it would have freed keep their leases, which run out. With
	// the database answering the record takes milliseconds; with it unanswering, every moment
	// waited here is added to the shutdown before its idle wait (waitUntilIdle) begins counting
	// the database silent. So the share is kept to half a second.
	agentExitRecordWait = 500 * time.Millisecond
)

// errAgentStopping is a start refused because the server is stopping its agent.
var errAgentStopping = errors.New("the server is stopping its agent")

// agentEnd is the server's end of agent/v1 that the supervisor drives (api.AgentListener).
type agentEnd interface {
	Path() string
	Secret() []byte
	SetCloseStandardInput(closeIn func())
	LastRenewal() time.Time
	Health() (agentwire.Health, bool)
	SetRunSlots(slots int)
	AgentExited(ctx context.Context, stopping bool) (bool, error)
	Stop()
	Close(by time.Time)
}

// agentSupervisor runs the command bar's agent process (envoy-dispatch agent) for the server: it
// starts the agent at boot with an environment of its own, writes the secret of agent/v1 to its
// standard input and keeps that open, since the agent exits when it closes; it starts the agent
// again after each exit, 1 s after, then twice as long after each quick exit, up to 30 s. It reads
// the agent's resident memory every 50 ms and kills it with SIGKILL above its ceiling, and kills
// an agent that renewed nothing for 30 s. Each exit frees the runs the agent held
// (agentEnd.AgentExited); two kills for memory within an hour that no run alone caused halve the
// agent's run slots, to 1 at the least, for an hour. It is the process's agentShutdown: Stop
// closes the agent's standard input after the HTTP drain, and Close waits for the agent to exit
// and, for its share of the shutdown, for the record of that exit. Each kill goes to the process
// the supervisor started (os.Process.Kill), never to its pid, which another process may hold once
// the agent has exited and been reaped.
type agentSupervisor struct {
	end     agentEnd
	command []string
	env     []string
	limits  agentMemory
	// after is how the supervisor waits before a restart, time.After; a test records the waits.
	after func(time.Duration) <-chan time.Time
	// renewalsMissed is agentRenewalsMissed; a test shortens it.
	renewalsMissed time.Duration

	// memoryKills counts the agent's kills for memory since the server started.
	memoryKills atomic.Int64

	mu sync.Mutex
	// stopping is closed when the server stops its agent (closeStandardInput).
	stopping chan struct{}
	// stdin, process and started are the running agent's standard input, process and start; nil
	// while none runs.
	stdin   io.Closer
	process *os.Process
	started time.Time
	// slots is the agent's run slots now, limits.Slots but for the hour after a halving, which
	// ends at slotsUntil and restore gives back.
	slots      int
	slotsUntil time.Time
	restore    *time.Timer
	// unexplained is the latest kill for memory that no run alone caused.
	unexplained time.Time

	// gone is closed once the supervisor starts no agent any more and no agent process runs,
	// before the last exit's record (exited) is written; done once that record has ended too.
	gone     chan struct{}
	goneOnce sync.Once
	done     chan struct{}
	// records is the context of every exit's record, which Close cancels once the record's share
	// of the shutdown is over (agentExitRecordWait).
	records       context.Context
	cancelRecords context.CancelFunc
}

// startAgentSupervisor starts the agent process, command run with env, for end, and supervises it
// until the server stops it.
func startAgentSupervisor(end agentEnd, command, env []string, limits agentMemory) *agentSupervisor {
	s := newAgentSupervisor(end, command, env, limits)
	go s.run()
	return s
}

// agentExecutable is the file the server starts its agent from: on Linux /proc/self/exe, which
// names the file this process runs, whatever a deploy has put at its path since, so an agent
// started after an exit is the server's own build; elsewhere the path os.Executable names. A
// Linux with no /proc to read refuses, as os.Executable does there.
func agentExecutable() (string, error) {
	if runtime.GOOS != "linux" {
		return os.Executable()
	}
	const self = "/proc/self/exe"
	if _, err := os.Stat(self); err != nil {
		return "", err
	}
	return self, nil
}

// newAgentSupervisor is a supervisor that has started nothing yet.
func newAgentSupervisor(end agentEnd, command, env []string, limits agentMemory) *agentSupervisor {
	records, cancelRecords := context.WithCancel(context.Background())
	s := &agentSupervisor{end: end, command: command, env: env, limits: limits, after: time.After,
		renewalsMissed: agentRenewalsMissed, stopping: make(chan struct{}), slots: limits.Slots,
		gone: make(chan struct{}), done: make(chan struct{}), records: records, cancelRecords: cancelRecords}
	end.SetRunSlots(limits.Slots)
	end.SetCloseStandardInput(s.closeStandardInput)
	return s
}

// agentTestHooks is what a server under its own test hooks turns on in its agent: the loop's test
// hooks (DISPATCH_TEST_AGENT_HOOKS=1, going/agent's hooks.go), the wait after each start before
// the agent allocates at full speed (DISPATCH_TEST_AGENT_GROW_AFTER, growAgentHeap), zero for
// none, the agent's GOMAXPROCS (DISPATCH_TEST_AGENT_PROCS), zero for the CPUs it is given, and
// the time the agent's runs tell their model as the time now (DISPATCH_TEST_AGENT_NOW,
// going/agent's Config.Now), zero for the clock's.
type agentTestHooks struct {
	Loop      bool
	GrowAfter time.Duration
	Procs     int
	Now       time.Time
}

// procs is the agent's GOMAXPROCS: as many CPUs as the server schedules on, agentMaxProcs at the
// most, or the hook's where a test sets one.
func (h agentTestHooks) procs() int {
	if h.Procs > 0 {
		return h.Procs
	}
	return min(agentMaxProcs, runtime.GOMAXPROCS(0))
}

// agentEnvironment is the agent's whole environment: the socket it calls the server at, how many
// runs it works at once, its CPUs and its memory limit, and the chat model's settings as the
// server read them, with the file of certificate authorities the server trusts the chat model's
// endpoint by (trustFile, SSL_CERT_FILE), where it names one - nothing of the server's own
// environment, so the agent holds no secret of Dispatch. hooks turns the agent's test hooks on,
// which only a server under its own test hooks asks for.
func agentEnvironment(chat answererConfig, socket string, limits agentMemory, trustFile string, hooks agentTestHooks) []string {
	env := []string{
		"DISPATCH_AGENT_SOCKET=" + socket,
		"DISPATCH_AGENT_SLOTS=" + strconv.Itoa(limits.Slots),
		"GOMAXPROCS=" + strconv.Itoa(hooks.procs()),
		"GOMEMLIMIT=" + strconv.FormatInt(limits.AgentLimit, 10),
	}
	set := func(name, value string) {
		if value != "" {
			env = append(env, name+"="+value)
		}
	}
	set("SSL_CERT_FILE", trustFile)
	if hooks.Loop {
		set("DISPATCH_TEST_AGENT_HOOKS", "1")
	}
	if hooks.GrowAfter > 0 {
		set("DISPATCH_TEST_AGENT_GROW_AFTER", hooks.GrowAfter.String())
	}
	if !hooks.Now.IsZero() {
		set("DISPATCH_TEST_AGENT_NOW", hooks.Now.Format(time.RFC3339Nano))
	}
	set("DISPATCH_ANSWERER", chat.Place)
	switch chat.Place {
	case placeAnswererAzure:
		set("DISPATCH_AZURE_CHAT_ENDPOINT", chat.Endpoint)
		set("DISPATCH_AZURE_CHAT_DEPLOYMENT", chat.Model)
		set("DISPATCH_AZURE_CHAT_API_VERSION", chat.APIVersion)
		set("DISPATCH_AZURE_CHAT_SCOPE", chat.Scope)
		set("DISPATCH_AZURE_CHAT_TOKENS_PER_MINUTE", strconv.Itoa(chat.TokensPerMinute))
		set("DISPATCH_AZURE_TENANT_ID", chat.TenantID)
		set("DISPATCH_AZURE_CLIENT_ID", chat.ClientID)
		set("DISPATCH_AZURE_FEDERATED_TOKEN_FILE", chat.FederatedToken)
	case placeAnswererEndpoint:
		set("DISPATCH_CHAT_ENDPOINT", chat.Endpoint)
		set("DISPATCH_CHAT_MODEL", chat.Model)
		set("DISPATCH_CHAT_TOKEN_FILE", chat.TokenFile)
	}
	return env
}

// nextAgentRestart is the wait before the agent starts again, after the wait before its last start
// (0 for none) and a run of ran: agentRestartFirst after the first exit and after a run of
// agentQuickExit or more, and otherwise twice the last wait, at most agentRestartMost.
func nextAgentRestart(previous, ran time.Duration) time.Duration {
	if previous == 0 || ran >= agentQuickExit {
		return agentRestartFirst
	}
	return min(2*previous, agentRestartMost)
}

// run starts the agent, watches it until it exits, and starts it again, until the server stops it.
func (s *agentSupervisor) run() {
	defer close(s.done)
	defer s.closeGone()
	var wait time.Duration
	for {
		started := time.Now()
		cmd, err := s.start()
		if errors.Is(err, errAgentStopping) {
			return
		}
		var ran time.Duration
		if err != nil {
			slog.Error("dispatch: start the agent process", "error", err)
		} else {
			killedForMemory := s.watch(cmd, started)
			ran = time.Since(started)
			s.exited(killedForMemory)
		}
		select {
		case <-s.stopping:
			return
		default:
		}
		wait = nextAgentRestart(wait, ran)
		slog.Warn("dispatch: the agent process exited; it starts again", "after", wait, "ran", ran.Round(time.Millisecond), "exit", exitOf(cmd))
		select {
		case <-s.after(wait):
		case <-s.stopping:
			return
		}
	}
}

// exitOf names how cmd's process ended, for the log.
func exitOf(cmd *exec.Cmd) string {
	if cmd == nil || cmd.ProcessState == nil {
		return "not started"
	}
	return cmd.ProcessState.String()
}

// start starts one agent: its command with the agent's environment, its output into the server's
// log, the secret on its standard input, which stays open, and its priority lowered.
func (s *agentSupervisor) start() (*exec.Cmd, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	select {
	case <-s.stopping:
		return nil, errAgentStopping
	default:
	}
	cmd := exec.Command(s.command[0], s.command[1:]...)
	cmd.Env = s.env
	// A writer of its own rather than the server's *os.File, so the agent holds a pipe of the
	// server's and not the server's own standard error, which outlives no reader of the server.
	output := struct{ io.Writer }{os.Stderr}
	cmd.Stdout, cmd.Stderr = output, output
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	if _, err := stdin.Write(s.end.Secret()); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, fmt.Errorf("give the agent its secret: %w", err)
	}
	s.stdin, s.process, s.started = stdin, cmd.Process, time.Now()
	lowerPriority(cmd.Process.Pid)
	slog.Info("dispatch: started the agent process", "pid", cmd.Process.Pid, "ceiling", s.limits.Ceiling,
		"gomemlimit", s.limits.AgentLimit, "slots", s.slots)
	return cmd, nil
}

// watch waits for the agent cmd started at started to exit, reading its resident memory every
// agentMemoryEvery: above its ceiling it is killed with SIGKILL, and so is an agent whose latest
// renew, or whose start where it has renewed nothing, is renewalsMissed old. It reports whether the
// agent was killed for memory.
func (s *agentSupervisor) watch(cmd *exec.Cmd, started time.Time) bool {
	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()
	pid := cmd.Process.Pid
	ticker := time.NewTicker(agentMemoryEvery)
	defer ticker.Stop()
	killed, killedForMemory := false, false
	for {
		select {
		case <-exited:
			return killedForMemory
		case <-ticker.C:
		}
		if killed {
			continue
		}
		if memory, err := residentMemory(pid); err == nil && memory > s.limits.Ceiling {
			_ = cmd.Process.Kill()
			killed, killedForMemory = true, true
			slog.Warn("dispatch: agent killed for memory", "pid", pid, "memory", memory, "ceiling", s.limits.Ceiling)
			s.memoryKills.Add(1)
			continue
		}
		renewed := s.end.LastRenewal()
		if renewed.Before(started) {
			renewed = started
		}
		if since := time.Since(renewed); since > s.renewalsMissed {
			_ = cmd.Process.Kill()
			killed = true
			slog.Warn("dispatch: agent killed: it renewed nothing for 30s", "pid", pid, "since_renewal", since.Round(time.Millisecond))
		}
	}
}

// exited reports the agent's exit to the server's end, which frees the runs it held, and counts
// a kill for memory that no run alone caused (noteUnexplainedKill). An exit after the server
// stopped its agent is the last: the process is gone before its record is written. The record
// runs under agentExitedTimeout and the supervisor's records context, which Close cancels once
// the record's share of the shutdown is over.
func (s *agentSupervisor) exited(killedForMemory bool) {
	s.mu.Lock()
	s.stdin, s.process = nil, nil
	var stopping bool
	select {
	case <-s.stopping:
		stopping = true
	default:
	}
	s.mu.Unlock()
	if stopping {
		s.closeGone()
	}
	ctx, cancel := context.WithTimeout(s.records, agentExitedTimeout)
	defer cancel()
	alone, err := s.end.AgentExited(ctx, stopping)
	if err != nil {
		slog.Error("dispatch: free the runs of the agent process that exited; their leases run out instead", "error", err)
		return
	}
	if killedForMemory && !alone {
		s.noteUnexplainedKill(time.Now())
	}
}

// noteUnexplainedKill counts a kill for memory at now that no run alone caused: one within
// agentKillWindow of the one before halves the agent's run slots, to 1 at the least, until
// agentKillWindow after it.
func (s *agentSupervisor) noteUnexplainedKill(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	previous := s.unexplained
	s.unexplained = now
	if previous.IsZero() || now.Sub(previous) > agentKillWindow {
		return
	}
	s.slots = max(1, s.slots/2)
	s.slotsUntil = now.Add(agentKillWindow)
	s.end.SetRunSlots(s.slots)
	if s.restore != nil {
		s.restore.Stop()
	}
	s.restore = time.AfterFunc(agentKillWindow, s.restoreSlots)
	slog.Warn("dispatch: the agent was killed for memory twice within an hour with no run alone to blame; its slots are halved for an hour",
		"slots", s.slots)
}

// restoreSlots gives the agent its whole slots back once agentKillWindow has passed since they
// were last halved.
func (s *agentSupervisor) restoreSlots() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if time.Now().Before(s.slotsUntil) {
		return
	}
	s.slots = s.limits.Slots
	s.end.SetRunSlots(s.slots)
	slog.Info("dispatch: the agent's slots are whole again", "slots", s.slots)
}

// closeStandardInput is the stop of the agent, which the server's end calls at its Stop: no agent
// starts from now on, and the running one's standard input closes, so it exits. With none running,
// the agent is gone at once.
func (s *agentSupervisor) closeStandardInput() {
	s.mu.Lock()
	select {
	case <-s.stopping:
		s.mu.Unlock()
		return
	default:
	}
	close(s.stopping)
	stdin, process := s.stdin, s.process
	s.mu.Unlock()
	if process == nil {
		s.closeGone()
	}
	if stdin != nil {
		_ = stdin.Close()
		slog.Info("dispatch: closed the agent's standard input", "pid", process.Pid)
	}
}

// closeGone closes gone, once.
func (s *agentSupervisor) closeGone() {
	s.goneOnce.Do(func() { close(s.gone) })
}

// Stop is the first half of the agent's shutdown, after the HTTP drain: the server's end stops,
// which closes the agent's standard input (closeStandardInput).
func (s *agentSupervisor) Stop() {
	s.end.Stop()
}

// Close is the second half: the server's end closes once the calls in flight have ended or by
// passes; the agent process, whose standard input Stop closed, is waited for agentExitGrace, then
// killed through the process the supervisor started (os.Process.Kill), which signals nothing once
// that process is reaped, and waited for as long again; and the record of its exit, the database
// write that frees the runs it held, is waited for agentExitRecordWait, then cancelled, so the
// runs keep their leases, which run out. After the calls in flight, then, the agent's part of the
// shutdown is the agent's exit and at most agentExitRecordWait, whether the database answers or
// not.
func (s *agentSupervisor) Close(by time.Time) {
	defer s.cancelRecords()
	s.end.Close(by)
	if !closedWithin(s.gone, agentExitGrace) {
		s.mu.Lock()
		process := s.process
		s.mu.Unlock()
		if process != nil {
			slog.Warn("dispatch: the agent did not exit when its standard input closed; it is killed", "pid", process.Pid)
			_ = process.Kill()
		}
		if !closedWithin(s.gone, agentExitGrace) {
			slog.Warn("dispatch: the agent process had not exited when the shutdown went on")
		}
	}
	if !closedWithin(s.done, agentExitRecordWait) {
		// The record's own error says the runs keep their leases (exited).
		s.cancelRecords()
		if !closedWithin(s.done, agentExitGrace) {
			slog.Warn("dispatch: the record of the agent's exit had not ended when the shutdown went on")
		}
	}
	s.mu.Lock()
	if s.restore != nil {
		s.restore.Stop()
	}
	s.mu.Unlock()
}

// closedWithin waits up to within for ch to close, and reports whether it did.
func closedWithin(ch <-chan struct{}, within time.Duration) bool {
	timer := time.NewTimer(within)
	defer timer.Stop()
	select {
	case <-ch:
		return true
	case <-timer.C:
		return false
	}
}

// running is when the running agent started, and false while none runs.
func (s *agentSupervisor) running() (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.started, s.process != nil
}

// runSlots is the agent's run slots now.
func (s *agentSupervisor) runSlots() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.slots
}

// lowerPriorityRefused is set once the kernel refused to lower the agent's priority, which is
// logged once for the process rather than at each start.
var lowerPriorityRefused atomic.Bool

// lowerPriority puts every thread of process pid under the SCHED_IDLE policy. The kernel gives a
// thread under it a CPU only where no thread of the server's policy wants one, and takes the CPU
// back as soon as such a thread wakes on it; at a nice value alone, a server thread that wakes on
// a CPU the agent holds can wait out the agent thread's slice (check c29 measured the server's
// threads waiting 224 ms in one second to run beside an agent at nice 10).
//
// Linux keeps a policy for each thread, and a thread takes its maker's. A thread the agent's
// runtime makes while a pass runs, by a thread the pass has not reached, keeps the policy it was
// made with, so a pass is made again until one finds every thread already under SCHED_IDLE: every
// thread made after that pass is made by a thread under SCHED_IDLE and takes it, so none escapes.
// Where the kernel refuses SCHED_IDLE, every thread is set to nice agentNice instead, the same
// way, and that is logged once.
func lowerPriority(pid int) {
	idle := true
	for range lowerPriorityPasses {
		changed := false
		for _, id := range threadsOf(pid) {
			lowered, err := lowerThread(id, idle)
			switch {
			case errors.Is(err, unix.ESRCH):
				// The thread has ended.
			case err != nil && idle:
				if !lowerPriorityRefused.Swap(true) {
					slog.Warn("dispatch: the kernel refused the agent's threads SCHED_IDLE; they run at nice 10", "pid", pid, "thread", id, "error", err)
				}
				idle, changed = false, true
			case err != nil:
				slog.Warn("dispatch: lower the agent's priority", "pid", pid, "thread", id, "error", err)
				return
			case lowered:
				changed = true
			}
		}
		if !changed {
			return
		}
	}
}

// threadsOf is the id of each thread of process pid, as /proc/<pid>/task lists them.
func threadsOf(pid int) []int {
	tasks, _ := os.ReadDir(filepath.Join("/proc", strconv.Itoa(pid), "task"))
	ids := make([]int, 0, len(tasks))
	for _, task := range tasks {
		if id, err := strconv.Atoi(task.Name()); err == nil {
			ids = append(ids, id)
		}
	}
	return ids
}

// lowerThread puts thread id under SCHED_IDLE, or at nice agentNice where idle is false, and
// reports whether it changed the thread: one already there is left as it is.
func lowerThread(id int, idle bool) (bool, error) {
	if idle {
		policy, err := threadPolicy(id)
		if err != nil {
			return false, err
		}
		if policy == schedIdle {
			return false, nil
		}
		return true, setThreadIdle(id)
	}
	// getpriority(2) answers 20 less the nice value.
	if priority, err := unix.Getpriority(unix.PRIO_PROCESS, id); err != nil || 20-priority == agentNice {
		return false, err
	}
	return true, unix.Setpriority(unix.PRIO_PROCESS, id, agentNice)
}

// residentMemory is process pid's resident memory in bytes, its VmRSS in /proc/<pid>/status.
func residentMemory(pid int) (int64, error) {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "status"))
	if err != nil {
		return 0, err
	}
	return vmRSS(data)
}

// vmRSS reads the VmRSS line of a /proc/<pid>/status, which counts kibibytes.
func vmRSS(status []byte) (int64, error) {
	scanner := bufio.NewScanner(bytes.NewReader(status))
	for scanner.Scan() {
		value, found := bytes.CutPrefix(scanner.Bytes(), []byte("VmRSS:"))
		if !found {
			continue
		}
		fields := bytes.Fields(value)
		if len(fields) != 2 || string(fields[1]) != "kB" {
			return 0, fmt.Errorf("VmRSS reads %q, not a count of kB", value)
		}
		kibibytes, err := strconv.ParseInt(string(fields[0]), 10, 64)
		if err != nil {
			return 0, fmt.Errorf("VmRSS reads %q, not a count of kB", value)
		}
		return kibibytes << 10, nil
	}
	return 0, errors.New("the status holds no VmRSS line")
}
