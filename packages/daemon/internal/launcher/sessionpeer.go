package launcher

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/sjawhar/legion/daemon/internal/claudelaunch"
	"github.com/sjawhar/legion/daemon/internal/memoryguard"
	"github.com/sjawhar/legion/daemon/internal/sessionproxy"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

// ptraceScopeFile is Yama's ptrace_scope, which says which processes may attach to another with
// ptrace. A test points it at a file of its own.
var ptraceScopeFile = "/proc/sys/kernel/yama/ptrace_scope"

// peerProc is the /proc that a generation's session peer check reads the process table from
// (sessionPeer). A test points it at a fake /proc.
var peerProc = "/proc"

// requireTraceScope refuses a generation that would use the session writer token unless Yama's
// ptrace_scope (ptraceScopeFile) reads an integer of 1 or more. The session peer check admits the
// agent's own process and no descendant of it. That holds only while no descendant can act as the
// agent: take its admitted connection (pidfd_getfd), read or write its memory (/proc/<pid>/mem,
// process_vm_writev) or trace it. Each of these needs ptrace attach access, and a scope of 1 or
// more refuses that access to a descendant. A scope of 0, a file that cannot be read and text that
// is not an integer each leave that open, so each refuses the start. The error names the file and
// what it read.
func requireTraceScope() error {
	raw, err := os.ReadFile(ptraceScopeFile)
	if err != nil {
		return fmt.Errorf("the session writer token needs %s to read 1 or more, and it cannot be read: %v", ptraceScopeFile, err)
	}
	text := strings.TrimSpace(string(raw))
	if scope, err := strconv.Atoi(text); err != nil || scope < 1 {
		return fmt.Errorf("the session writer token needs %s to read 1 or more, and it reads %q:"+
			" a process of the role could then trace the agent and use its connection to the session database", ptraceScopeFile, text)
	}
	return nil
}

// sessionPeer is the Admit of one generation's session proxy when the session database names a
// writer audience (sessionproxy.Config.Admit): it decides which process may use the socket.
// It admits the launcher's own process, because the resume lookup (resumable) and a Claude Code
// keeper (claudeKeeper) connect from it. A Claude Code generation has no agent that connects, so
// for it the check admits nothing else. For an Oh My Pi generation it admits the agent alone.
//
// The check finds the agent in the process table, never from the process that connects: the
// generation's role-init (the manager's active child), its shim (roleShim), and the oldest direct
// child of the shim that runs the agent's executable. A command the agent runs is a descendant of
// the agent and never a direct child of the shim, because no process of the shim makes itself a
// subreaper, so an orphan goes to role-init. An orphan that runs `legion worker-shim` is younger
// than the real shim, and roleShim takes the oldest. The shim's other children (the secrets
// renewer, the working copy's adoption, the CodeGraph warm-up) run other executables, and Oh My
// Pi's own helpers are the agent's children. While a start wrapper (`/bin/sh -c … legion-start`)
// runs, the shim's child runs another executable, so nothing is the agent until the wrapper's exec.
//
// The first connection whose check finds the agent pins it, by its pid and its start time, under
// the check's own lock. Every later connection must come from that pid, and the pid must still
// have that start time, so a process that takes the pid after the agent ends is refused. A table
// that cannot be read, a missing role-init, shim or agent, and a peer that is not the pin each
// refuse the connection, with an error that says why: the proxy logs it as the refusal's reason.
// The check never admits on an error.
type sessionPeer struct {
	m          *manager
	generation uint64
	// launcher is this process's pid.
	launcher int
	// claude is whether the generation is a Claude Code one.
	claude bool
	// proc is the /proc the table is read from (peerProc). shim is the launcher's own executable,
	// which a role's shim runs, and agent the agent's executable with its links resolved, as a
	// process's exe link reads it.
	proc, shim, agent string

	// mu holds the pin: found is whether a check found the agent, and pid and start are its pid
	// and start time.
	mu    sync.Mutex
	found bool
	pid   int
	start uint64
}

// newSessionPeer is the session peer check of command's generation, whose environment is env. A
// Claude Code start is one whose environment names claudelaunch.SessionVariable, as newClaudeKeeper
// reads it: it needs no agent executable. Any other start must name the agent's executable as an
// absolute path (shimwire.LauncherStart.AgentExecutable), which is resolved once, here, and the
// launcher's own executable must be readable: without either no process could be the agent, so the
// start is refused rather than every connection of the generation.
func newSessionPeer(m *manager, command shimwire.LauncherStart, env []string) (*sessionPeer, error) {
	p := &sessionPeer{m: m, generation: command.Generation, launcher: os.Getpid(), proc: peerProc}
	if envValue(env, claudelaunch.SessionVariable) != "" {
		p.claude = true
		return p, nil
	}
	switch {
	case command.AgentExecutable == "":
		return nil, errors.New("an Oh My Pi start with the session writer token must name the agent's executable (agentExecutable), and this start names none")
	case !filepath.IsAbs(command.AgentExecutable):
		return nil, fmt.Errorf("the agent's executable %q is not an absolute path", command.AgentExecutable)
	}
	agent, err := filepath.EvalSymlinks(command.AgentExecutable)
	if err != nil {
		return nil, fmt.Errorf("resolve the agent's executable: %v", err)
	}
	shim, err := os.Readlink("/proc/self/exe")
	if err != nil {
		return nil, fmt.Errorf("read the launcher's own executable, which a role's shim runs: %v", err)
	}
	p.shim, p.agent = shim, agent
	return p, nil
}

// admit is the check itself (sessionproxy.Config.Admit).
func (p *sessionPeer) admit(peer sessionproxy.Peer) error {
	if peer.PID == p.launcher {
		return nil
	}
	if p.claude {
		return fmt.Errorf("generation %d is a Claude Code one, whose session only the launcher writes", p.generation)
	}
	p.m.mu.Lock()
	active := p.m.child
	p.m.mu.Unlock()
	switch {
	case active == nil:
		return fmt.Errorf("generation %d runs no process", p.generation)
	case active.generation != p.generation:
		return fmt.Errorf("the role runs generation %d, not generation %d", active.generation, p.generation)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.found {
		agent, err := p.findAgent(active.pid)
		if err != nil {
			return err
		}
		p.found, p.pid, p.start = true, agent.PID, agent.StartTime
	}
	if peer.PID != p.pid {
		return fmt.Errorf("pid %d is not the agent of generation %d, which is pid %d", peer.PID, p.generation, p.pid)
	}
	start, err := memoryguard.ReadStartTime(p.proc, p.pid)
	if err != nil {
		return fmt.Errorf("read the start time of the agent of generation %d, pid %d: %v", p.generation, p.pid, err)
	}
	if start != p.start {
		return fmt.Errorf("pid %d is another process now: it started at %d, and the agent of generation %d started at %d",
			p.pid, start, p.generation, p.start)
	}
	return nil
}

// findAgent is the generation's agent in the process table: the oldest direct child of the shim
// of roleInit that runs the agent's executable.
func (p *sessionPeer) findAgent(roleInit int) (*memoryguard.Process, error) {
	t, err := memoryguard.Scan(p.proc)
	if err != nil {
		return nil, fmt.Errorf("read the process table: %v", err)
	}
	init, ok := t.Get(roleInit)
	if !ok {
		return nil, fmt.Errorf("the role-init of generation %d, pid %d, is not in the process table", p.generation, roleInit)
	}
	shim := roleShim(t, init, p.shim)
	if shim == nil {
		return nil, fmt.Errorf("the role-init of generation %d, pid %d, has no shim", p.generation, roleInit)
	}
	for _, child := range t.Children(shim.PID) {
		if child.Exe == p.agent {
			return child, nil
		}
	}
	return nil, fmt.Errorf("the shim of generation %d, pid %d, has no direct child that runs %s", p.generation, shim.PID, p.agent)
}
