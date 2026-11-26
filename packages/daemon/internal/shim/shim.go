// Package shim is `legion worker-shim`: the bridge between one phase worker's `omp --mode rpc`
// and the daemon's worker stream. The shim always dials the daemon — the stream is the daemon's
// listener, never the shim's — authenticates with the pane's boot token, and spawns OMP only
// once the daemon has acked it. From then on it carries OMP's stdio NDJSON to the stream and
// back, answers the frames that are its own (`shutdown`, `adopt-working-copy`), and outlives any
// one connection: a dropped stream is redialled, and what OMP says meanwhile waits in a bounded
// backlog.
//
// The behaviour was ported from the TypeScript shim's `--connect` mode, which LEGION-223 removed; a
// `worker-shim.ts:N` citation at a rule names its source in this repository's history. The wire
// itself is internal/shimwire's.
package shim

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/sjawhar/legion/daemon/internal/leakguard"
	"github.com/sjawhar/legion/daemon/internal/procgroup"
	"github.com/sjawhar/legion/daemon/internal/programbound"
	"github.com/sjawhar/legion/daemon/internal/redact"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/subreaper"
)

const (
	// backlogLimit is how many of the newest lines bound for the daemon wait for one that is not
	// connected (BACKLOG_LIMIT, worker-shim.ts); the oldest are dropped a whole frame at a time
	// (outbox).
	backlogLimit = 1000
	// A refused or failed connect is retried after reconnectBase, doubling to reconnectCap
	// (RECONNECT_BASE_MS and RECONNECT_CAP_MS, and cmdWorkerShimConnect's retry, worker-shim.ts).
	reconnectBase = 200 * time.Millisecond
	reconnectCap  = 5 * time.Second
	// DefaultGrace is how long a SIGTERMed child has to exit before it is killed: the shipped
	// shim's default, half the daemon's default `worker_stop_timeout_seconds`
	// (DEFAULT_TERMINATE_STDIN_GRACE_MS, worker-shim.ts).
	DefaultGrace = 5 * time.Second
	// DefaultStopGrace is the stop grace a shim assumes when its runtime names none
	// (Config.StopGrace): the daemon's default `worker_stop_timeout_seconds`, which is also what a
	// pod's launcher gets as its own stop grace (internal/runtime/sandbox's launcherCommand passes
	// both the same value, the pod's terminationGracePeriodSeconds).
	DefaultStopGrace = 10 * time.Second
	// DrainTimeout bounds reading OMP's stdout after OMP has exited. What OMP wrote is in the pipe
	// by then; only a process it started and left holding the pipe keeps it open past this.
	// Exported so internal/config can name it in the message that refuses a stop grace under
	// MinStopGrace.
	DrainTimeout = time.Second
	// MinWarmUpDrain is the least Run waits for an in-flight CodeGraph warm-up to return
	// (warmUpDrain), whatever the stop grace leaves: the warm-up's own kill grace for its
	// codegraph child (internal/workspace's codegraphStopGrace, 2 s) plus one second for reaping
	// the child and releasing the lease, measured at 0.1–0.3 s in LEGION-629.
	MinWarmUpDrain = 3 * time.Second
	// MinStopGrace is the smallest stop grace the whole stop fits, 9 s: the agent SIGTERMed and,
	// ignoring it, killed at DefaultGrace; its stdout drained for DrainTimeout; the warm-up given
	// MinWarmUpDrain to release its lease. That is why the daemon refuses a smaller
	// `worker_stop_timeout_seconds` under `runtime: kubernetes` at configuration load
	// (internal/config).
	MinStopGrace = DefaultGrace + DrainTimeout + MinWarmUpDrain
)

// warmUpDrain is how long Run waits for an in-flight CodeGraph warm-up once the shim's loop has
// ended, before it returns without it: what is left of the role's stop grace once the agent's own
// grace and the stdout drain are spent, and never below MinWarmUpDrain. The whole stop — the agent
// SIGTERMed and, ignoring it, killed at grace; its stdout drained for DrainTimeout; the warm-up's
// codegraph child SIGTERMed and, ignoring it, killed at workspace's codegraphStopGrace; the lease
// released — fits inside the stop grace exactly from MinStopGrace up, so a runtime that gives less
// is refused by the daemon's configuration load (internal/config) rather than waited out here: the
// launcher SIGKILLs the shim at the stop grace (endGeneration, internal/launcher/process.go),
// which would leave the lease behind. With the defaults, 10 s − 5 s − 1 s = 4 s.
func warmUpDrain(stopGrace, grace time.Duration) time.Duration {
	return max(stopGrace-grace-DrainTimeout, programbound.Of(&warmUpDrainFloor))
}

// warmUpDrainFloor is MinWarmUpDrain as warmUpDrain reads it, through programbound: a variable so
// that cmd/legion's tests can give it their own deadline, since a warm-up a loaded machine makes
// slow to release its lease is not left at 3 s there. MinWarmUpDrain stays the constant
// internal/config sizes MinStopGrace by.
var warmUpDrainFloor = MinWarmUpDrain

func init() {
	programbound.Register(&warmUpDrainFloor)
}

// Clock is the time the shim waits on: the backoff between dials and the grace before a kill.
type Clock interface {
	After(d time.Duration) <-chan time.Time
}

type realClock struct{}

func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// Config is one shim's resolved flags. Every refusal belongs to the readers in config.go and has
// happened before Run: Run dials, and spawns once acked.
type Config struct {
	// Network and Address are the daemon's worker stream, as ParseAddress returns them.
	Network, Address string
	// BootToken is the pane's boot token, the hello's only field.
	BootToken string
	// Argv is the wrapped OMP invocation.
	Argv []string
	// Env is the shim's own environment (os.Environ()). OMP and the adoption command inherit it,
	// and the adoption rewrites the workspace its LEGION_WORKSPACE names.
	Env []string
	// ProviderEnv is NAME=value pairs for OMP's environment only (--provider-env-dir).
	ProviderEnv []string
	// AgentSecrets is the pod's enrollment with the secrets broker (cipher.go); nil on a
	// tmux pane, which is never enrolled.
	AgentSecrets *AgentSecrets
	// WarmCodegraph, when set, is started once with the workspace the shim's LEGION_WORKSPACE
	// names, on the first line the wrapped process writes: Oh My Pi's `ready` frame comes once its
	// extensions are loaded and its RPC loop serves, so the index build the pod needs starts
	// behind the launch, never on it (`legion worker-shim --warm-codegraph`,
	// workspace.WarmCodegraphIndex). It runs in its own goroutine under a context the shim ends
	// when its loop ends — the wrapped process has exited — and it must return once that context
	// ends, its build stopped and its lease released, since Run waits for it (bounded by
	// warmUpDrain) before returning the agent's exit status: a stop mid-build then leaves no lease
	// behind for the role's relaunch to read as a live build. start is how the warm-up starts each
	// codegraph it runs: the shim's own guard (Config.ReapOrphans), so a reaper of this process's
	// orphans never takes that codegraph's exit. `launched` is when the shim's Run
	// began — the role's launcher starting this generation — which the warm-up's success
	// line reports its build against. Nil on a tmux pane, whose workspace the daemon warms itself,
	// and on the controller, which has no workspace: nothing runs.
	WarmCodegraph func(ctx context.Context, start procgroup.Starter, dir string, launched time.Time)
	// ReapOrphans makes the shim the child subreaper of everything it starts and reaps every
	// exited child it does not wait for itself (subreaper.Guard.Serve): `legion worker-shim
	// --reap-orphans`, which the controller pod's launcher passes when it runs the shim as its own
	// child, with no role-init above it (internal/launcher's manager.command).
	//
	// There the shim's process group is the agent's, and Oh My Pi's tools leave orphans in it (a
	// `cmd &` whose shell exits). Without this the container's PID 1, the launcher, which is in a
	// group of its own, adopts and reaps each of them. The cluster's gVisor keeps a count, per
	// process group, of the members whose parent is in another group of the session, and it takes
	// one from that count when such a member is reaped but adds none when a member is reparented
	// (gVisor's pkg/sentry/kernel: the count is `ancestors` in sessions.go, taken from on a reap
	// in task_exit.go, and left as it is by reparentLocked there). So each orphan the launcher reaps
	// takes the count of the agent's group one lower, until it reads 0 while the shim, which links
	// the group, still runs. gVisor then treats the group as orphaned, and a member that is
	// stopped at that moment brings SIGHUP and SIGCONT to the whole group: the shim stops on that
	// SIGHUP, and Oh My Pi in rpc mode exits 129. An orphan this shim reaps is reaped by a parent
	// in its own group, which takes nothing from the count.
	//
	// A role under role-init needs none of this: role-init starts its shim in role-init's own
	// process group (internal/launcher's runRoleShim), so the role's orphans are reaped in their
	// own group there, and the role's process tree stays what the launcher's session peer check
	// and memory guard read (a shim whose direct children are its own). A tmux pane runs on a
	// real kernel, whose count reparenting moves.
	//
	// Every child the shim starts and waits for itself starts through one guard (s.children),
	// whether or not this is set, so the reaper never takes the exit of Oh My Pi, of the secrets
	// keygen or renewer, of a command the daemon asked for, or of the warm-up's codegraph.
	// TestEveryChildTheShimWaitsForStartsThroughItsGuard reads every start the shim's process
	// reaches.
	ReapOrphans bool
	// LeakCheckDir is where the pod's launcher installed the shared public leak check, which
	// spawnOnce reads (leakguard.Installed) before the wrapped process is started; empty is
	// leakguard.InstallDir. The launcher installs the check the daemon served once for the pod,
	// before any role starts, so the shim only reads it: with a pin in the environment
	// (leakguard.TagVariable, which the worker image sets and a tmux pane does not) and the
	// deployment's checker (leakguard.CheckerVar, which the runtime tells a role of a deployment
	// that runs a shared leak check), a role whose install is absent or of another tag runs no
	// agent.
	//
	// The installed check's path is what the shim names in PUBLIC_LEAK_CHECK for everything it
	// starts, where the environment names none of its own (spawnOnce): the runtime's own value
	// wins where there is one, and the install fills the gap where there is not.
	LeakCheckDir string
	// Log receives the shim's own lines and its one-line frame summaries: what the pane shows.
	Log io.Writer
	// Grace is how long a SIGTERMed child has before it is killed; zero is DefaultGrace.
	Grace time.Duration
	// StopGrace is how long the shim's runtime gives the whole shim to exit after its stop signal
	// before it is killed — a pod's terminationGracePeriodSeconds, which the launcher passes as
	// `--stop-grace`; zero is DefaultStopGrace. Run keeps its wait for the warm-up (warmUpDrain)
	// inside it.
	StopGrace time.Duration
	// Clock is the time the shim waits on; nil is the real one.
	Clock Clock
}

// Run bridges until the wrapped process exits and returns its exit status: its code, or 128 plus
// the signal that ended it. ctx is the shim's own termination — the signal that stops a pane or
// a pod — which ends the child the way the daemon's `shutdown` does; with no child spawned yet,
// the shim exits 143 as that signal would have. The error is the shim's own failure to start
// the child, never the child's.
func Run(ctx context.Context, cfg Config) (int, error) {
	s := &shim{cfg: cfg, clock: cfg.Clock, grace: cfg.Grace, stopGrace: cfg.StopGrace, childExited: make(chan struct{}), started: time.Now(), children: subreaper.NewGuard()}
	if s.clock == nil {
		s.clock = realClock{}
	}
	if s.grace == 0 {
		s.grace = DefaultGrace
	}
	if s.stopGrace == 0 {
		s.stopGrace = DefaultStopGrace
	}
	out := cfg.Log
	if out == nil {
		out = io.Discard
	}
	s.log = log.New(out, "", 0)
	s.out.log = s.log
	// The reaper serves before anything is started and is stopped last, once every wait deferred
	// below has returned, so an orphan any child of the shim leaves until then is reaped here.
	if cfg.ReapOrphans {
		stopReaping, err := s.children.Serve(func(format string, args ...any) { s.log.Printf("[worker-shim] "+format, args...) })
		if err != nil {
			return 1, err
		}
		defer stopReaping()
		s.log.Print("[worker-shim] this shim is the child subreaper of everything it starts, and reaps their orphans")
	}
	s.loop, s.stop = context.WithCancel(context.Background())
	s.warm, s.stopWarm = context.WithCancel(s.loop)
	// The commands take the stop the shim was given, and end with the agent's own SIGTERM
	// (terminate) and with the shim's loop as well, so that whichever of the three comes first
	// ends them (s.commands).
	s.commands, s.cancelCommands = context.WithCancel(ctx)
	context.AfterFunc(s.loop, s.endCommands)
	// Deferred in this order so that they run in reverse: the loop ends, which ends the commands
	// the daemon asked for and which the wait behind it reaps; the renewer ends with the loop as
	// well; and the warm-up — ended with the loop too — is waited for last, inside the stop grace.
	defer s.drainWarmUp()
	defer s.renewers.Wait()
	defer s.drainCommands()
	defer s.stop()
	go s.watch(ctx)
	return s.run()
}

type shim struct {
	cfg       Config
	clock     Clock
	grace     time.Duration
	stopGrace time.Duration
	log       *log.Logger
	out       outbox

	// children is the guard every child the shim starts and waits for itself starts through: Oh
	// My Pi (spawnOnce), the secrets keygen and renewer (identity, startRenewer), every command the
	// daemon asked for (openWorkspace) and the warm-up's codegraph (warmCodegraph). Under
	// Config.ReapOrphans its reaper passes each of them over until its own wait has read its exit.
	children *subreaper.Guard

	// loop ends with the shim: every dial, connection, and backoff is bound to it.
	loop context.Context
	stop context.CancelFunc
	// warm is the warm-up's context, a child of loop that also ends the moment the child is told
	// to stop (terminate): the build has nothing to wait for once the agent is on its way out, and
	// ending it then, not at the agent's exit, keeps the whole stop inside the stop grace even when
	// the agent spends its own grace ignoring SIGTERM.
	warm     context.Context
	stopWarm context.CancelFunc
	// started is when Run began: the launch the warm-up's success line reports its build against
	// (Config.WarmCodegraph's launched).
	started time.Time

	// commands is what every git and jj the shim starts builds its budget on (runAdoption,
	// readWorkspaceWork), each keeping a budget of its own on top of it. Each of those commands
	// runs in a process group of its own now (internal/procgroup), which is a group the SIGHUP a
	// tmux kill-pane gives every process of the pane does not reach, so the shim's own end is what
	// passes that stop on: a command left on context.Background() runs on against a clone every
	// role of the issue shares until its own budget ends it, minutes after the pane it belonged to
	// is gone.
	//
	// It ends at the first of the shim's three ends, because no one of them covers every way it
	// goes. Run's own context alone never ends on the path the tmux runtime stops a pane by: the
	// daemon sends `shutdown` first (internal/runtime/tmux's stop), fromDaemon answers it with
	// terminate, the agent exits, reap's finish ends s.loop, and Run returns with the context it
	// was given still live — leaving the command running in a process that is already exiting, and
	// no SIGHUP to reach it afterwards. s.loop alone would end a command only once the shim had
	// finished — once the agent, SIGTERMed and killed at the grace if it ignores that, had been
	// reaped — which is a whole grace of that command running against the shared clone after the
	// stop arrived. So terminate ends them too, beside the warm-up, and the command's own stop is
	// spent inside the grace the agent is already being given rather than after it.
	//
	// Each of these commands is answered in a goroutine of its own (answer), so nothing of the
	// shim waits on one while it runs; Run waits for them once this context has ended and before
	// it returns, so procgroup's SIGTERM, grace and SIGKILL reach the group from a live process
	// (drainCommands).
	commands       context.Context
	cancelCommands context.CancelFunc
	// commandsEnded is when endCommands first ran, as a monotonic offset from s.started — the
	// reading time.Since takes, never a wall-clock one, so a clock step during a stop can neither
	// stretch the drain's wait nor skip it — and 0 before it has run: where drainCommands' bound
	// starts counting.
	commandsEnded atomic.Int64
	// commandRuns is every goroutine answer started, which drainCommands waits for.
	// commandsInFlight is how many of those goroutines have not returned yet — raised in answer,
	// lowered in the goroutine's own defer — which is what decides whether there is anything to
	// wait for (drainCommands).
	commandRuns      sync.WaitGroup
	commandsInFlight atomic.Int64

	mu          sync.Mutex
	child       *exec.Cmd
	stdin       *shimwire.Writer
	exiting     bool // the shim is ending before any child was spawned; none will be
	terminating bool
	childExited chan struct{}
	thumbprint  string // agent-secrets keygen's thumbprint, generated once per shim
	renewing    bool   // the agent-secrets renewer goroutine has been started

	// renewers is the agent-secrets renewer goroutine (cipher.go's startRenewer), reaped by
	// Run's deferred Wait before it returns.
	renewers sync.WaitGroup

	// warmed guards cfg.WarmCodegraph: called on the child's first line and never again, not for
	// a later frame and not when a redialled connection finds the child already running. warmUps
	// is that one goroutine, which Run waits for (drainWarmUp) before returning. warmStarted is
	// set once by warmCodegraph before its goroutine starts and read by drainWarmUp, which
	// otherwise returns at once, so a shim that never warmed registers no wait on its clock.
	warmed      sync.Once
	warmUps     sync.WaitGroup
	warmStarted atomic.Bool

	once sync.Once
	code int
	err  error
}

// run dials until the shim ends. A connection that was acked and then dropped is redialled at
// once; a dial that fails, or a stream the daemon closes before its ack — a refused hello — is
// retried with backoff (cmdWorkerShimConnect's dial loop, worker-shim.ts).
func (s *shim) run() (int, error) {
	failures := 0
	for s.loop.Err() == nil {
		acked, err := s.connect()
		if s.loop.Err() != nil {
			break
		}
		if acked {
			failures = 0
			s.log.Printf("[worker-shim] daemon stream %s closed (%v); reconnecting", s.cfg.Address, err)
			continue
		}
		delay := min(reconnectBase<<min(failures, 5), reconnectCap)
		failures++
		s.log.Printf("[worker-shim] daemon stream %s unavailable (%v); retrying in %dms", s.cfg.Address, err, delay.Milliseconds())
		select {
		case <-s.clock.After(delay):
		case <-s.loop.Done():
		}
	}
	return s.code, s.err
}

// connect is one connection: the hello, the wait for its ack, the child spawned on the first
// ack, and the daemon's frames until the stream drops. It reports whether the daemon acked. The
// hello says whether the backlog dropped a frame since the last connection took it
// (outbox.whole), and a connection the backlog cannot be handed to as its hello said is closed
// right behind its ack (outbox.attach).
func (s *shim) connect() (acked bool, err error) {
	identity, err := s.identity()
	if err != nil {
		// A pod that cannot generate its key or read its token registers as nothing: the shim ends
		// naming the failure, before any dial (finish cancels the loop, so run returns 1 and the
		// error), and the daemon observes the pod gone.
		s.finish(1, err)
		return false, err
	}
	var dialer net.Dialer
	conn, err := dialer.DialContext(s.loop, s.cfg.Network, s.cfg.Address)
	if err != nil {
		return false, err
	}
	defer conn.Close()
	defer context.AfterFunc(s.loop, func() { _ = conn.Close() })()

	whole := s.out.whole()
	w := shimwire.NewWriter(conn)
	if err := w.WriteFrame(shimwire.Hello2{BootToken: s.cfg.BootToken, AgentSecrets: identity, BacklogWhole: whole}); err != nil {
		return false, fmt.Errorf("send hello: %w", err)
	}
	r := shimwire.NewReader(conn)
	for {
		line, err := r.ReadLine()
		if err != nil {
			return false, fmt.Errorf("stream closed before hello_ack: %w", err)
		}
		if frame, err := shimwire.Decode(line); err == nil && frame.FrameType() == shimwire.TypeHelloAck {
			break
		}
		// Nothing the daemon sends before its ack is meant for a child that does not exist yet
		// (cmdWorkerShimConnect's onLine before hello_ack, worker-shim.ts). The line is the far
		// side's, so the URL rule reads it whole before it is cut (redact.Runes).
		s.log.Printf("[worker-shim] ignoring a frame received before hello_ack: %s", redact.Runes(string(line), 80))
	}
	// Spawned here, before the next line is read: the daemon's first RPC frame may share a read
	// with the ack, and it must find a child to go to (cmdWorkerShimConnect's ack, worker-shim.ts).
	if !s.spawnOnce() {
		// The shim is ending — told to stop before any child, or unable to start one — and
		// finish settles its status; nothing is left to bridge.
		<-s.loop.Done()
		return true, nil
	}
	if !s.out.attach(conn, w, whole) {
		return true, errors.New("the connection took no whole backlog")
	}
	defer s.out.detach(conn)
	for {
		line, err := r.ReadLine()
		if err != nil {
			return true, err
		}
		s.fromDaemon(line)
	}
}

// fromDaemon routes one daemon frame: those the shim acts on or answers itself, and everything
// else to OMP unchanged (createShimBridge's onLine, worker-shim.ts).
func (s *shim) fromDaemon(line []byte) {
	frame, err := shimwire.Decode(line)
	if err != nil {
		switch typ := frameType(line); typ {
		case shimwire.TypeAdoptWorkingCopy, shimwire.TypeWorkspaceWork, shimwire.TypeAgentSecretsEnrollment, shimwire.TypeAgentSecretsToken:
			// The daemon is its only sender, and it is never OMP's to read.
			s.log.Printf("[worker-shim] refusing a malformed %s frame: %v", typ, err)
			return
		}
		s.toChild(line)
		return
	}
	switch f := frame.(type) {
	case shimwire.Shutdown:
		s.terminate("the daemon's shutdown")
		return
	case shimwire.AdoptWorkingCopy:
		s.answer(func() { s.adopt(f) })
		return
	case shimwire.WorkspaceWork:
		s.answer(func() { s.workspaceWork(f) })
		return
	case shimwire.AgentSecretsEnrollment:
		s.enroll(f)
		return
	case shimwire.AgentSecretsToken:
		s.podToken(f)
		return
	}
	s.toChild(line)
}

func frameType(line []byte) string {
	var head struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(line, &head)
	return head.Type
}

func (s *shim) toChild(line []byte) {
	if err := s.stdin.WriteLine(line); err != nil {
		s.log.Printf("[worker-shim] could not forward a daemon frame to the wrapped process: %v", err)
	}
}

func (s *shim) toDaemon(frame shimwire.Frame) {
	line, err := json.Marshal(frame)
	if err != nil {
		s.log.Printf("[worker-shim] could not encode a %s frame: %v", frame.FrameType(), err)
		return
	}
	s.out.send(line)
}

// spawnOnce reads the pod's shared public leak check, names it where its environment names none,
// then starts the wrapped process, unless that process already runs or the shim is ending. A
// read that refuses and a process that cannot be started each end the shim with that failure.
//
// The pod's launcher installs the check the daemon served once for the pod, before it starts any
// role, so the shim reads it and installs nothing (leakguard.Installed, at Config.LeakCheckDir).
// With no pin in the environment, which is every tmux pane, and with no deployment checker in it
// (leakguard.CheckerVar), which is every role of a deployment that runs no shared leak check,
// nothing is read and nothing refused. With both, an install that is absent or of another tag
// ends the shim with exit 1 and runs no agent.
//
// The read is here, and before the start, for two orderings this placement guarantees. The
// harness is started after it, so no turn of the agent reaches a write path before the check its
// environment names is known to be in place. And connect calls spawnOnce on the daemon's hello
// ack and reads the next frame only once it has returned, so nothing the daemon asks of this shim
// runs before the read either. A role whose check is not in place runs no agent at all: every
// write path would refuse anyway.
//
// PUBLIC_LEAK_CHECK is set from the install only where the environment names none of its own,
// and the name is added to the shim's own environment, which is what the wrapped process and
// everything else this shim runs start from. A runtime that names the check
// (sandbox's mainEnvironment) therefore decides; where it names none, the install is what the
// role's writes are checked against rather than none at all.
func (s *shim) spawnOnce() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.child != nil {
		return true
	}
	if s.exiting {
		return false
	}
	check, err := leakguard.Installed(s.cfg.Env, s.cfg.LeakCheckDir)
	if err != nil {
		s.exiting = true
		s.finish(1, err)
		return false
	}
	if check != "" {
		s.log.Printf("[worker-shim] the shared public leak check %s the pod's launcher installed is at %s", envValue(s.cfg.Env, leakguard.TagVariable), check)
		if envValue(s.cfg.Env, leakguard.CheckVar) == "" {
			s.log.Printf("[worker-shim] %s is unset, so the installed check %s is what this role's writes are checked against", leakguard.CheckVar, check)
			s.cfg.Env = append(slices.Clone(s.cfg.Env), leakguard.CheckVar+"="+check)
		}
	}
	cmd := exec.Command(s.cfg.Argv[0], s.cfg.Argv[1:]...)
	cmd.Env = append(slices.Clone(s.cfg.Env), s.cfg.ProviderEnv...)
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		s.exiting = true
		s.finish(1, fmt.Errorf("spawn %s: %w", s.cfg.Argv[0], err))
		return false
	}
	// OMP's stdout is a pipe of the shim's own rather than exec's, so the process's exit and the
	// end of its output are two facts the shim waits on separately (see reap).
	stdout, childStdout, err := os.Pipe()
	if err != nil {
		s.exiting = true
		s.finish(1, fmt.Errorf("spawn %s: %w", s.cfg.Argv[0], err))
		return false
	}
	cmd.Stdout = childStdout
	if _, err := s.children.Start(cmd); err != nil {
		_ = stdout.Close()
		_ = childStdout.Close()
		s.exiting = true
		s.finish(1, fmt.Errorf("spawn %s: %w", s.cfg.Argv[0], err))
		return false
	}
	_ = childStdout.Close()
	s.child = cmd
	s.stdin = shimwire.NewWriter(stdin)
	s.log.Printf("[worker-shim] spawned %s (pid %d)", s.cfg.Argv[0], cmd.Process.Pid)
	pumped := make(chan struct{})
	go s.pump(stdout, pumped)
	go s.reap(cmd, stdout, pumped)
	return true
}

// pump carries OMP's stdout to the daemon unchanged (createShimBridge's forwardToSocket,
// worker-shim.ts), each line as a frame of its own but for an rpc_chunk sequence's, which reach
// the outbox as one frame (chunkHold). The child's first line is also when the workspace's
// CodeGraph warm-up starts (Config.WarmCodegraph), in its own goroutine so the pump never waits
// on it.
func (s *shim) pump(stdout *os.File, done chan<- struct{}) {
	defer close(done)
	defer stdout.Close()
	r := shimwire.NewReader(stdout)
	held := chunkHold{out: &s.out}
	for {
		line, err := r.ReadLine()
		if err != nil {
			// A sequence the child never finished goes as it is, and the daemon's reader drops it.
			held.release()
		}
		if errors.Is(err, shimwire.ErrLineTooLong) {
			// OMP chunks every frame over the limit; a longer line is a child whose stream can no
			// longer be framed, and nothing from mid-line can be forwarded honestly.
			s.log.Printf("[worker-shim] the wrapped process broke its framing (%v); ending it", err)
			s.terminate("broken framing")
			_, _ = io.Copy(io.Discard, stdout)
			return
		}
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, os.ErrClosed) {
				s.log.Printf("[worker-shim] reading the wrapped process's stdout: %v", err)
			}
			return
		}
		s.warmed.Do(s.warmCodegraph)
		line = bytes.Clone(line)
		// One read of the line serves the hold and the summary. encoding/json fills head's type even
		// when another member has the wrong JSON type, so head.Type is the type frameType reads
		// whatever the error; a line is a chunk only when the whole of head decoded.
		var head lineHead
		chunk := json.Unmarshal(line, &head) == nil && head.Type == shimwire.TypeRPCChunk
		held.forward(line, head, chunk)
		if summary := summarize(line, head.Type); summary != "" {
			s.log.Print(summary)
		}
	}
}

// chunkHold is the rpc_chunk sequence of OMP's that the pump is reading — a frame over the line
// limit, such as a long turn's agent_end — held until its last chunk is read and then sent to the
// outbox as one frame. So a connection that breaks under it leaves the whole sequence, from its
// chunk 0, for the next, and nothing the shim sends of its own lands between two of its chunks. A
// line that does not continue the sequence — one that is not a chunk, a chunk at a place the
// daemon's reader refuses (shimwire's RPCChunk.Placement, the check that reader makes first: an id
// empty or over its bound, a count under two or over the most a sequence may declare, an index
// under zero or at or past its count), a chunk of another sequence, or an index that is not the
// next — sends what is held first, as one frame, and so does the pump's end: the daemon's reader
// drops that broken sequence as it would have, and nothing changes order. The first two go alone
// after it, as every line went before the pump held sequences. So the pump holds one sequence at
// most, which OMP writes back to back, and no more of it than a sequence may declare, each line
// under the line limit.
type chunkHold struct {
	out   *outbox
	lines [][]byte
	id    string
	next  int // the index a chunk has to have to continue the sequence
}

// lineHead is what the pump reads of each of OMP's lines: its type and, for an rpc_chunk, where it
// sits in its sequence, never its data.
type lineHead struct {
	Type    string `json:"type"`
	ChunkID string `json:"chunkId"`
	Index   int    `json:"index"`
	Count   int    `json:"count"`
}

// forward sends one of OMP's lines to the outbox, or holds it with the rest of its sequence when
// it is a chunk at a place the daemon's reader takes (RPCChunk.Placement), which head reads.
func (h *chunkHold) forward(line []byte, head lineHead, chunk bool) {
	place := shimwire.RPCChunk{ChunkID: head.ChunkID, Index: head.Index, Count: head.Count}
	if !chunk || place.Placement() != nil {
		h.release()
		h.out.send(line)
		return
	}
	if head.ChunkID != h.id || head.Index != h.next {
		h.release()
	}
	h.lines = append(h.lines, line)
	h.id, h.next = head.ChunkID, head.Index+1
	if head.Index == head.Count-1 {
		h.release()
	}
}

// release sends what is held to the outbox as one frame, if anything is.
func (h *chunkHold) release() {
	if len(h.lines) == 0 {
		return
	}
	h.out.send(h.lines...)
	clear(h.lines)
	h.lines, h.id, h.next = h.lines[:0], "", 0
}

// warmCodegraph starts cfg.WarmCodegraph for the workspace LEGION_WORKSPACE names, once the
// wrapped process has written its first line (pump), under s.warm: the warm-up's build ends when
// the child is told to stop or has exited, and Run waits for its return (drainWarmUp). The command
// refuses --warm-codegraph without the variable (cmd/legion/worker_shim.go), so an empty value
// here is a Config built by hand.
func (s *shim) warmCodegraph() {
	if s.cfg.WarmCodegraph == nil {
		return
	}
	dir := envValue(s.cfg.Env, "LEGION_WORKSPACE")
	if dir == "" {
		s.log.Printf("[worker-shim] no CodeGraph warm-up: LEGION_WORKSPACE is unset")
		return
	}
	s.warmStarted.Store(true)
	s.warmUps.Add(1)
	go func() {
		defer s.warmUps.Done()
		s.cfg.WarmCodegraph(s.warm, s.children.Starter(), dir, s.started)
	}()
}

// drainWarmUp waits for the warm-up warmCodegraph started, if any, to return — its codegraph
// child ended by s.warm's end, its lease released — before Run returns the agent's exit status,
// so a role stopped mid-build leaves no lease its relaunch would read as a live build. The wait is
// bounded by warmUpDrain — what the role's stop grace leaves, never under MinWarmUpDrain: a
// warm-up still running past it is logged and left, so the shim's exit still reaches the daemon
// before the launcher's kill. A shim whose warm-up never started has nothing to drain and
// registers no wait on its clock.
func (s *shim) drainWarmUp() {
	if !s.warmStarted.Load() {
		return
	}
	drained := make(chan struct{})
	go func() {
		s.warmUps.Wait()
		close(drained)
	}()
	bound := warmUpDrain(s.stopGrace, s.grace)
	select {
	case <-drained:
	case <-s.clock.After(bound):
		s.log.Printf("[worker-shim] the CodeGraph warm-up is still running %v after the wrapped process exited; leaving it", bound)
	}
}

// reap waits for the wrapped process, lets the pump forward what it wrote before exiting, and
// ends the shim with its status.
func (s *shim) reap(cmd *exec.Cmd, stdout *os.File, pumped <-chan struct{}) {
	_ = cmd.Wait()
	s.children.Done(cmd.Process.Pid)
	close(s.childExited)
	select {
	case <-pumped:
	case <-time.After(DrainTimeout):
		s.log.Printf("[worker-shim] the wrapped process exited and something it started still holds its stdout; closing it")
		_ = stdout.Close()
		<-pumped
	}
	s.finish(exitStatus(cmd.ProcessState), nil)
}

func exitStatus(state *os.ProcessState) int {
	if status, ok := state.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		return 128 + int(status.Signal())
	}
	return state.ExitCode()
}

// watch is the shim's own termination: ctx ending ends the child as `shutdown` does, and with no
// child yet ends the shim (cmdWorkerShimConnect's onTerminate callback, worker-shim.ts).
func (s *shim) watch(ctx context.Context) {
	select {
	case <-ctx.Done():
		if !s.terminate("signalled to stop") {
			s.log.Print("[worker-shim] signalled to stop before the wrapped process was spawned; exiting")
			s.finish(128+int(syscall.SIGTERM), nil)
		}
	case <-s.loop.Done():
	}
}

// terminate SIGTERMs the child and kills it if it is still running when the grace runs out. The
// bridge keeps running meanwhile, so what OMP says on its way out still reaches the daemon. The
// warm-up's context ends here too (s.warm), and so does the one every command the daemon asked
// for runs on (s.commands): neither a build nor a jj against the issue's shared clone has
// anything left to serve once the agent is on its way out, and ending them now rather than at the
// agent's exit spends their own stops inside the grace the agent is being given rather than after
// it — which is what keeps the whole stop inside the stop grace whatever the agent does with its
// own. It reports false, having done nothing but settle that no child will be spawned, when none
// was.
func (s *shim) terminate(reason string) bool {
	s.mu.Lock()
	child := s.child
	switch {
	case child == nil:
		s.exiting = true
		s.mu.Unlock()
		return false
	case s.terminating:
		s.mu.Unlock()
		return true
	}
	s.terminating = true
	s.mu.Unlock()

	s.log.Printf("[worker-shim] %s: sending SIGTERM to the wrapped process", reason)
	s.stopWarm()
	s.endCommands()
	_ = child.Process.Signal(syscall.SIGTERM)
	expired := s.clock.After(s.grace)
	go func() {
		select {
		case <-s.childExited:
		case <-expired:
			s.log.Printf("[worker-shim] the wrapped process is still running %v after SIGTERM; killing it", s.grace)
			_ = child.Process.Kill()
		}
	}()
	return true
}

func (s *shim) finish(code int, err error) {
	s.once.Do(func() {
		s.code, s.err = code, err
		s.stop()
	})
}

// envValue is name's value in env, the last assignment winning as it does for a process started
// with env; empty when env assigns none.
func envValue(env []string, name string) string {
	for i := len(env) - 1; i >= 0; i-- {
		if value, ok := strings.CutPrefix(env[i], name+"="); ok {
			return value
		}
	}
	return ""
}

// The raw frame types summarize has a line for: Oh My Pi writes each with no typed frame of its
// own, so the shim reads them as shimwire.Raw.
const (
	rawErrorType          = "error"
	rawExtensionErrorType = "extension_error"
)

// summarize is the one line the pane shows for a frame worth a human's glance, or nothing
// (summarizeShimFrame, worker-shim.ts). It is given line's type, which the pump has read, and
// decodes only the frames it has a line for: every other line Oh My Pi writes — an `rpc_chunk`'s
// few hundred kilobytes of base64 among them — it reads no further than that type.
func summarize(line []byte, typ string) string {
	switch typ {
	case shimwire.TypeAgentStart, shimwire.TypeAgentEnd, shimwire.TypeResponse, shimwire.TypeToolExecutionStart,
		rawErrorType, rawExtensionErrorType:
	default:
		return ""
	}
	frame, err := shimwire.Decode(line)
	if err != nil {
		return ""
	}
	switch f := frame.(type) {
	case shimwire.AgentStart:
		return shimwire.TypeAgentStart
	case shimwire.AgentEnd:
		return shimwire.TypeAgentEnd
	case shimwire.Response:
		if !f.Success {
			return strings.TrimSpace("error " + f.Command + " " + f.Error)
		}
	case shimwire.ToolExecutionStart:
		tool := f.ToolName
		if tool == "" {
			tool = "unknown"
		}
		var args bytes.Buffer
		if json.Compact(&args, f.Args) != nil || args.String() == "null" {
			args.Reset()
			args.WriteString("{}")
		}
		// The rule before the cut: a userinfo cut before its "@" is one the log's rule cannot find.
		return shimwire.TypeToolExecutionStart + " " + tool + " " + redact.Runes(args.String(), 80)
	case shimwire.Raw:
		switch f.Type {
		case rawErrorType:
			var event struct {
				Message *string `json:"message"`
			}
			if json.Unmarshal(f.JSON, &event) == nil && event.Message != nil {
				return rawErrorType + " " + *event.Message
			}
			return rawErrorType + " " + string(f.JSON)
		case rawExtensionErrorType:
			// An extension that failed leaves the agent running without what it gives it; this
			// line is the one place a pod's log says so (Oh My Pi reports it on its RPC stream only).
			var event struct {
				ExtensionPath string `json:"extensionPath"`
				Event         string `json:"event"`
				Error         string `json:"error"`
			}
			_ = json.Unmarshal(f.JSON, &event)
			return rawExtensionErrorType + " " + event.ExtensionPath + " " + event.Event + ": " + event.Error
		}
	}
	return ""
}

// outbox is every frame bound for the daemon: written to the connection when one is attached,
// held in the backlog of the newest backlogLimit lines when none is, and replayed in order —
// ahead of anything newer — when the next connection is acked (createShimBridge's forward and
// onConnect, worker-shim.ts).
//
// A frame is one line, or the whole rpc_chunk sequence of one of OMP's frames too large for one
// line (chunkHold). The outbox moves frames, never part of one: the daemon's reader reassembles a
// sequence only from its chunk 0 on one stream, so a sequence is written, held, replayed and
// dropped whole. One a connection breaks under waits from its chunk 0 for the next, and the part
// the broken connection carried is its reader's to drop.
//
// What the backlog dropped since a connection last took it whole is what the next hello says
// (whole, shimwire.Hello2.BacklogWhole), and a connection whose hello said it dropped nothing is
// handed the backlog only if that still holds when its ack comes (attach).
type outbox struct {
	mu   sync.Mutex
	conn net.Conn
	w    *shimwire.Writer
	// backlog is every line waiting for a connection, oldest first, and frames is how many of
	// those lines each frame is, in the same order.
	backlog [][]byte
	frames  []int
	// dropped is whether the backlog has ever dropped a frame, which is logged once for the
	// shim's life; gap is whether it dropped one since a connection last took it whole, or a
	// replay broke partway (attach).
	dropped bool
	gap     bool
	log     *log.Logger
}

// send writes the lines of one frame to the connection, or holds them in the backlog when there
// is none or it breaks under them.
func (o *outbox) send(lines ...[]byte) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.w != nil {
		if writeFrame(o.w, lines) {
			return
		}
		// The stream broke under this frame, so the daemon never got it whole: it waits with the
		// rest for the next connection. Closing the connection is what tells its reader.
		_ = o.conn.Close()
		o.conn, o.w = nil, nil
	}
	o.backlog = append(o.backlog, lines...)
	o.frames = append(o.frames, len(lines))
	for len(o.backlog) > backlogLimit {
		o.dropOldest()
		o.gap = true
		if !o.dropped {
			o.dropped = true
			o.log.Printf("[worker-shim] no daemon client connected; dropping oldest frames beyond the last %d lines", backlogLimit)
		}
	}
}

// whole is whether the backlog has dropped no frame since a connection last took it whole: what
// the next hello says (shimwire.Hello2.BacklogWhole). A replay that broke partway counts as such a
// drop (attach): the frames it wrote before it broke left the backlog, and the dead connection may
// never have carried them to a daemon.
func (o *outbox) whole() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return !o.gap
}

// attach replays the backlog on a newly acked connection and makes it the one frames go to, and
// reports whether it did. whole is what that connection's hello said of the backlog (whole). A
// connection whose hello said the backlog had dropped no frame, where it has dropped one since —
// while the hello waited for its ack — is handed nothing and closed: its daemon reads the replay
// as every frame since the last connection, and has acted on that word by the time the ack
// comes, so the replay goes to the next connection instead, whose hello says what was dropped.
// A connection that breaks mid-replay is closed, and what it did not take whole waits for the
// next. What it did take whole is gone from the backlog though its daemon may never have read it,
// so the backlog is marked as one that dropped frames (gap), and the next hello says so (whole).
func (o *outbox) attach(conn net.Conn, w *shimwire.Writer, whole bool) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	if whole && o.gap {
		o.log.Printf("[worker-shim] the backlog dropped frames while the hello that said it had dropped none waited for its ack; closing that connection, and the next hello says so")
		_ = conn.Close()
		return false
	}
	for replayed := false; len(o.frames) > 0; replayed = true {
		if !writeFrame(w, o.backlog[:o.frames[0]]) {
			o.gap = o.gap || replayed
			_ = conn.Close()
			return false
		}
		o.dropOldest()
	}
	o.backlog, o.frames, o.gap = nil, nil, false
	o.conn, o.w = conn, w
	return true
}

// dropOldest drops the backlog's oldest frame, every line of it.
func (o *outbox) dropOldest() {
	n := o.frames[0]
	clear(o.backlog[:n])
	o.backlog, o.frames = o.backlog[n:], o.frames[1:]
}

// writeFrame writes the lines of one frame in order and reports whether w took every one.
func writeFrame(w *shimwire.Writer, lines [][]byte) bool {
	for _, line := range lines {
		if w.WriteLine(line) != nil {
			return false
		}
	}
	return true
}

func (o *outbox) detach(conn net.Conn) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.conn == conn {
		o.conn, o.w = nil, nil
	}
}
