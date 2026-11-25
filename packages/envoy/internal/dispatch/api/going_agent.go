package api

// going_agent.go is the server's side of agent/v1 (going/agentwire), the contract between this
// server and its agent process (envoy-dispatch agent), which keeps the model loop of each
// command-bar run and makes every read and write of Dispatch as a call to this server. The server
// listens on a Unix socket in a directory only its user can open (AgentListener), and answers a
// call only under the secret it made at its start, compared in constant time.
//
// A run is a job on going_runs: queued, it is ready; a claim takes it under a lease, a random
// token each claim makes anew, which every step, frames call, call and end of the run carries and
// the server checks in the transaction that writes it, so an owner whose lease ended writes
// nothing. The agent keeps one renew waiting, which renews its leases for goingLeaseTerm and is
// answered after goingRenewWait, or at once when the person stops one of its runs. A NOTIFY on
// GoingRunsChannel wakes the waiting claims and renews of every process: a run that becomes ready
// and a run that is stopped send one. A person's runs share one lock (goingPersonLock): at most
// goingWorkingPerPerson work at once, which leaves the next one queued, and at most
// goingQueuedPerPerson wait in the queue, which refuses a new one (admitGoingRun).
//
// The calls themselves - a read or write of Dispatch through the route table as the run's
// person, a GitHub read and a web read - are going_agent_call.go's and going_agent_reads.go's;
// what the process supervisor needs of the listener - the agent's renewals, its run slots and its
// death - is going_agent_supervision.go's.

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/going/agentwire"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// goingRunShape is the highest saved shape of a run (going_runs.shape) this build reads: it
// claims only a run whose shape is at or below it, and writes it into its row of server_processes
// at boot (Background.GoingRunShape), so a run a newer build saved waits for a build that reads
// it.
const goingRunShape = 1

// GoingRunsChannel is the NOTIFY channel a run that becomes ready, or is stopped, is announced
// on, which cmd/dispatch registers on its listener hub for the GoingRunSignal it hands Deps.
const GoingRunsChannel = "dispatch_going_runs"

// The bounds of agent/v1 on the server's side (background-runs design, section 5).
const (
	// goingClaimWait is the longest a claim waits for a ready run before it answers none.
	goingClaimWait = 25 * time.Second
	// goingLeaseTerm is how long a claim's or a renew's lease runs.
	goingLeaseTerm = 30 * time.Second
	// goingRenewWait is how long a renew is held before it is answered, unless a run of it is
	// stopped first.
	goingRenewWait = 10 * time.Second
	// goingWorkingPerPerson is how many of one person's runs work at once; a claim leaves the
	// next one queued.
	goingWorkingPerPerson = 3
	// goingQueuedPerPerson is how many of one person's runs wait in the queue; a new one past it
	// is refused (admitGoingRun).
	goingQueuedPerPerson = 20
	// goingHistoryLimit is the most the whole steps of a claim's history may hold: the steps of
	// the run's last agentwire.WholeTurns model turns, each with all its tool results; each older
	// step goes as its note. A run whose whole steps pass it fails with goingRunHistoryTooLarge.
	goingHistoryLimit = 2 << 20
	// goingAgentFrameTime bounds the read of a request frame and the write of an answer frame.
	goingAgentFrameTime = 10 * time.Second
	// goingClaimBatch is how many ready runs one claim's transaction looks at, oldest first.
	goingClaimBatch = 32
)

// goingRunHistoryTooLarge is the stop reason of a run whose last agentwire.WholeTurns model turns
// pass goingHistoryLimit, so no claim can answer them. It is the agent's own reason for the same
// bound (agent.ReasonHistoryTooLarge).
const goingRunHistoryTooLarge = "HISTORY_TOO_LARGE: the last 8 model turns of this run, each with its tool results, pass 2 MiB, which no model call can be given"

// codeGoingRunQueueFull refuses a new run of a person who has goingQueuedPerPerson runs queued.
const codeGoingRunQueueFull = "GOING_RUN_QUEUE_FULL"

// GoingRunSignal wakes every claim and renew this process holds waiting: cmd/dispatch registers
// Notify on its listener hub for GoingRunsChannel, and Wake for each LISTEN the hub makes, so a
// notification the hub missed while it had no connection wakes them too. Each waiter reads the
// store again when woken, so a wake says only "look again". A nil signal in Deps is a server
// whose waiters no NOTIFY reaches, which register gives a signal of its own.
type GoingRunSignal struct {
	mu    sync.Mutex
	woken chan struct{}
}

// NewGoingRunSignal is a signal nothing has woken yet.
func NewGoingRunSignal() *GoingRunSignal {
	return &GoingRunSignal{woken: make(chan struct{})}
}

// Notify wakes every waiter, whatever payload the notification carried. It never blocks, as a
// hub's handler must not.
func (g *GoingRunSignal) Notify(string) {
	g.Wake()
}

// Wake wakes every waiter. It never blocks.
func (g *GoingRunSignal) Wake() {
	g.mu.Lock()
	defer g.mu.Unlock()
	close(g.woken)
	g.woken = make(chan struct{})
}

// wait is a channel closed at the next wake. A waiter takes it before it reads the store, so a
// wake between its read and its wait is not lost.
func (g *GoingRunSignal) wait() <-chan struct{} {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.woken
}

// AgentListener is the server's end of agent/v1: the Unix socket, the secret, and the state of
// the calls it serves. The process supervisor writes Secret to the agent's standard input when it
// starts the agent, and Path names the socket.
type AgentListener struct {
	s        *server
	dir      string
	made     bool
	path     string
	listener net.Listener
	secret   []byte

	// slots holds one token for each call the server runs for its agents, at most goingCallSlots:
	// the next one waits for a free slot. A slot is held until its call ends, whether or not the
	// agent that made it still waits, so the calls of an agent that died count until they end.
	slots chan struct{}
	// calls counts the calls running, which Close waits for.
	calls backgroundWork
	// keys holds each call key while a try of it runs, so a second try waits for the first.
	keys goingCallKeys
	// serving counts the connections being answered.
	serving backgroundWork

	mu        sync.Mutex
	instances map[string]bool
	conns     map[net.Conn]struct{}
	closeIn   func()
	stopping  chan struct{}
	stopOnce  sync.Once
	health    atomic.Pointer[agentwire.Health]
	// renewed is when the latest renew arrived, in Unix nanoseconds, 0 before any (LastRenewal).
	renewed atomic.Int64
	// runSlots is how many runs one agent instance may hold at once (SetRunSlots), 0 for no bound
	// of the server's.
	runSlots atomic.Int64
	// suspects (under mu) are the runs an agent that died held, which the next claims take one at a
	// time, each while its instance holds no other run, until it saves a new step or ends
	// (claimNext, settleSuspect).
	suspects map[string]bool

	// claimWait, renewWait, callTime and callKeyWait are goingClaimWait, goingRenewWait,
	// goingCallTime and goingCallTime; a test shortens them.
	claimWait   time.Duration
	renewWait   time.Duration
	callTime    time.Duration
	callKeyWait time.Duration
	// web is how a web read reaches the web (goingWebReader).
	web goingWebReader
}

// goingAgentDirectory is the directory the agent's socket is made in: dispatch-agent under
// runtime, the process's runtime directory (XDG_RUNTIME_DIR, which cmd/dispatch reads through its
// settings table), made or narrowed to mode 0700, or, where runtime is empty, a directory of its
// own under the system's temporary directory, which os.MkdirTemp makes with mode 0700. made says
// the directory is this process's own, which Close removes.
func goingAgentDirectory(runtime string) (dir string, made bool, err error) {
	if runtime != "" {
		dir = filepath.Join(runtime, "dispatch-agent")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return "", false, fmt.Errorf("make the agent's socket directory: %w", err)
		}
		if err := os.Chmod(dir, 0o700); err != nil {
			return "", false, fmt.Errorf("narrow the agent's socket directory to its user: %w", err)
		}
		return dir, false, nil
	}
	dir, err = os.MkdirTemp("", "dispatch-agent-")
	if err != nil {
		return "", false, fmt.Errorf("make the agent's socket directory: %w", err)
	}
	return dir, true, nil
}

// listenAgentAtItsDirectory listens for the agent in its directory under runtime
// (goingAgentDirectory), which Background.ListenAgent hands the process.
func (s *server) listenAgentAtItsDirectory(runtime string) (*AgentListener, error) {
	dir, made, err := goingAgentDirectory(runtime)
	if err != nil {
		return nil, err
	}
	agent, err := s.listenAgent(dir, made)
	if err != nil && made {
		_ = os.RemoveAll(dir)
	}
	return agent, err
}

// listenAgent listens for the agent on agent.sock in dir, under a secret of SecretSize random
// bytes, and serves its calls until Close (newAgentListener, start). made says dir is this
// listener's own, which Close removes.
func (s *server) listenAgent(dir string, made bool) (*AgentListener, error) {
	agent, err := s.newAgentListener(dir, made)
	if err != nil {
		return nil, err
	}
	agent.start()
	return agent, nil
}

// newAgentListener binds agent.sock in dir and makes the secret, serving nothing until start. A
// socket file left there by a process that ended is replaced; one a live server answers on is
// refused, so two servers sharing a runtime directory never take each other's agent.
func (s *server) newAgentListener(dir string, made bool) (*AgentListener, error) {
	path := filepath.Join(dir, "agent.sock")
	if conn, err := net.DialTimeout("unix", path, time.Second); err == nil {
		_ = conn.Close()
		return nil, fmt.Errorf("another server process listens for its agent at %s", path)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("remove the socket an ended process left at %s: %w", path, err)
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("listen for the agent at %s: %w", path, err)
	}
	secret := make([]byte, agentwire.SecretSize)
	if _, err := rand.Read(secret); err != nil {
		_ = listener.Close()
		return nil, fmt.Errorf("make the agent's secret: %w", err)
	}
	return &AgentListener{
		s: s, dir: dir, made: made, path: path, listener: listener, secret: secret,
		slots: make(chan struct{}, goingCallSlots), instances: map[string]bool{}, conns: map[net.Conn]struct{}{}, suspects: map[string]bool{},
		stopping: make(chan struct{}), claimWait: goingClaimWait, renewWait: goingRenewWait,
		callTime: goingCallTime, callKeyWait: goingCallTime, web: goingWebReader{admit: goingPublicAddress, limit: goingWebReadTime},
	}, nil
}

// start answers the agent's connections until Close.
func (a *AgentListener) start() {
	a.serving.start()
	go func() {
		defer a.serving.done()
		a.accept()
	}()
}

// Path is the socket the agent dials.
func (a *AgentListener) Path() string { return a.path }

// Secret is the secret the agent sends with every call, SecretSize bytes, which the process
// supervisor writes to the agent's standard input.
func (a *AgentListener) Secret() []byte { return a.secret }

// SetCloseStandardInput is how Stop closes the agent's standard input, which the process
// supervisor sets once it starts the agent: the agent exits when it closes.
func (a *AgentListener) SetCloseStandardInput(closeIn func()) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.closeIn = closeIn
}

// Health is the agent's own figures as its latest renew carried them, and false before any.
func (a *AgentListener) Health() (agentwire.Health, bool) {
	health := a.health.Load()
	if health == nil {
		return agentwire.Health{}, false
	}
	return *health, true
}

// Stop is the first half of the server's shutdown, after its HTTP drain: from now on a claim is
// answered at once with no run, every renew waiting is answered at once and each later one too,
// and the agent's standard input is closed, so the agent exits. The calls in flight run on; Close
// waits for them.
func (a *AgentListener) Stop() {
	a.stopOnce.Do(func() {
		close(a.stopping)
		a.mu.Lock()
		closeIn := a.closeIn
		a.mu.Unlock()
		if closeIn != nil {
			closeIn()
		}
	})
}

// stopped reports whether Stop has run.
func (a *AgentListener) stopped() bool {
	select {
	case <-a.stopping:
		return true
	default:
		return false
	}
}

// Close is the second half of the shutdown: it stops the agent if Stop has not, waits for the
// calls in flight until by, then closes the socket and every connection still open, and removes
// the socket's directory where it is this listener's own. A call still running at by goes on to
// its own end, and either commits with its landing or does not, so the next agent makes it once.
func (a *AgentListener) Close(by time.Time) {
	a.Stop()
	ran := make(chan struct{})
	go func() {
		a.calls.wait()
		close(ran)
	}()
	timer := time.NewTimer(time.Until(by))
	defer timer.Stop()
	select {
	case <-ran:
	case <-timer.C:
		slog.Warn("dispatch: the agent's calls in flight did not end before the shutdown's bound; the socket closes with them running")
	}
	_ = a.listener.Close()
	a.mu.Lock()
	for conn := range a.conns {
		_ = conn.Close()
	}
	a.mu.Unlock()
	a.serving.wait()
	if a.made {
		_ = os.RemoveAll(a.dir)
	} else {
		_ = os.Remove(a.path)
	}
}

// accept answers each connection on a goroutine of its own until the listener closes.
func (a *AgentListener) accept() {
	for {
		conn, err := a.listener.Accept()
		if err != nil {
			if !errors.Is(err, net.ErrClosed) {
				slog.Error("dispatch: accept a connection of the agent", "error", err)
			}
			return
		}
		a.mu.Lock()
		a.conns[conn] = struct{}{}
		a.mu.Unlock()
		a.serving.start()
		go func() {
			defer a.serving.done()
			defer func() {
				a.mu.Lock()
				delete(a.conns, conn)
				a.mu.Unlock()
				_ = conn.Close()
			}()
			a.serve(conn)
		}()
	}
}

// serve answers the one call conn carries. The request frame is read within the largest request
// limit and goingAgentFrameTime; a request without the secret, or with another one, is refused
// before anything else is read of it, and so is one of another contract or past its call's own
// limit. While the call runs, a read of conn waits for the agent to close it: a claim or a renew
// the agent stopped waiting for ends then.
func (a *AgentListener) serve(conn net.Conn) {
	_ = conn.SetReadDeadline(time.Now().Add(goingAgentFrameTime))
	payload, err := agentwire.ReadFrame(conn, agentwire.MaxRequestLimit)
	var tooLarge agentwire.FrameTooLargeError
	switch {
	case errors.As(err, &tooLarge):
		a.answer(context.Background(), conn, "", nil, goingAgentRefusal(agentwire.CodeTooLarge, "%s", err.Error()))
		return
	case err != nil:
		return
	}
	var request agentwire.Request
	if err := json.Unmarshal(payload, &request); err != nil {
		a.answer(context.Background(), conn, "", nil, goingAgentRefusal(agentwire.CodeBadRequest, "the request is no agent/v1 frame: %v", err))
		return
	}
	if len(request.Secret) != agentwire.SecretSize || subtle.ConstantTimeCompare(request.Secret, a.secret) != 1 {
		a.answer(context.Background(), conn, "", nil, goingAgentRefusal(agentwire.CodeUnauthorized, "the request carries no secret of this server's agent"))
		return
	}
	if request.Version != agentwire.Version {
		a.answer(context.Background(), conn, "", nil, goingAgentRefusal(agentwire.CodeVersion, "this server speaks %s, and the request speaks %q", agentwire.Version, request.Version))
		return
	}
	limit := agentwire.RequestLimit(request.Call)
	switch {
	case limit == 0:
		a.answer(context.Background(), conn, "", nil, goingAgentRefusal(agentwire.CodeBadRequest, "%q is no call of %s", request.Call, agentwire.Version))
		return
	case len(payload) > limit:
		a.answer(context.Background(), conn, request.Call, nil, goingAgentRefusal(agentwire.CodeTooLarge, "a %s request of %d bytes is past its limit of %d", request.Call, len(payload), limit))
		return
	}
	_ = conn.SetReadDeadline(time.Time{})
	ctx, cancel := context.WithCancel(context.WithoutCancel(a.s.lifetime))
	defer cancel()
	go func() {
		var closed [1]byte
		_, _ = conn.Read(closed[:])
		cancel()
	}()
	body, refusal := a.dispatch(ctx, request)
	a.answer(ctx, conn, request.Call, body, refusal)
}

// answer writes the answer frame of call: body, or refusal where the call was refused. ctx is the
// call's own, which ends as the agent closes the connection (serve). A write that fails is logged
// as a warning, but for a renew whose agent closed the connection before its answer, which is
// logged at debug: the agent ends its renew on purpose as a claim lands, to renew the new lease
// too (agent.renew), so the answer finds the connection closed once for each run it starts.
func (a *AgentListener) answer(ctx context.Context, conn net.Conn, call string, body any, refusal *agentwire.Error) {
	envelope := agentwire.Answer{Error: refusal}
	if refusal == nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			envelope.Error = goingAgentRefusal(agentwire.CodeInternal, "encode the %s answer: %v", call, err)
		} else {
			envelope.Body = encoded
		}
	}
	frame, err := json.Marshal(envelope)
	if err != nil {
		slog.Error("dispatch: encode an answer to the agent", "call", call, "error", err)
		return
	}
	_ = conn.SetWriteDeadline(time.Now().Add(goingAgentFrameTime))
	if err := agentwire.WriteFrame(conn, frame, agentwire.AnswerLimit(call)); err != nil {
		if call == agentwire.CallRenew && goingAgentClosed(ctx, err) {
			slog.Debug("dispatch: the agent closed its renew before the answer", "error", err)
			return
		}
		slog.Warn("dispatch: write an answer to the agent", "call", call, "error", err)
	}
}

// goingAgentClosed reports whether a write of an answer failed because the agent closed the
// connection first: a broken pipe or a reset, on a call whose context ended, which serve's read of
// the connection ends as the agent closes it.
func goingAgentClosed(ctx context.Context, err error) bool {
	return ctx.Err() != nil && (errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.ECONNRESET))
}

// dispatch decodes the request's body as its call's shape, refusing a member the shape does not
// declare, and answers the call.
func (a *AgentListener) dispatch(ctx context.Context, request agentwire.Request) (any, *agentwire.Error) {
	switch request.Call {
	case agentwire.CallHello:
		return goingAgentDecoded(request.Body, func(hello agentwire.HelloRequest) (any, *agentwire.Error) { return a.hello(ctx, hello) })
	case agentwire.CallClaim:
		return goingAgentDecoded(request.Body, func(claim agentwire.ClaimRequest) (any, *agentwire.Error) { return a.claim(ctx, claim) })
	case agentwire.CallRenew:
		return goingAgentDecoded(request.Body, func(renew agentwire.RenewRequest) (any, *agentwire.Error) { return a.renew(ctx, renew) })
	case agentwire.CallStep:
		return goingAgentDecoded(request.Body, func(step agentwire.StepRequest) (any, *agentwire.Error) { return a.step(ctx, step) })
	case agentwire.CallFrames:
		return goingAgentDecoded(request.Body, func(frames agentwire.FramesRequest) (any, *agentwire.Error) { return a.frames(ctx, frames) })
	case agentwire.CallCall:
		return goingAgentDecoded(request.Body, func(call agentwire.CallRequest) (any, *agentwire.Error) { return a.call(ctx, call) })
	case agentwire.CallEnd:
		return goingAgentDecoded(request.Body, func(end agentwire.EndRequest) (any, *agentwire.Error) { return a.end(ctx, end) })
	}
	return nil, goingAgentRefusal(agentwire.CodeBadRequest, "%q is no call of %s", request.Call, agentwire.Version)
}

// goingAgentDecoded decodes body as T, refusing a member T does not declare, and answers what
// answer makes of it.
func goingAgentDecoded[T any](body json.RawMessage, answer func(T) (any, *agentwire.Error)) (any, *agentwire.Error) {
	var request T
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return nil, goingAgentRefusal(agentwire.CodeBadRequest, "the body is no %T: %v", request, err)
	}
	return answer(request)
}

// goingAgentRefusal is a refusal of code, its message formatted.
func goingAgentRefusal(code, format string, arguments ...any) *agentwire.Error {
	return &agentwire.Error{Code: code, Message: fmt.Sprintf(format, arguments...)}
}

// goingAgentFailed is a refusal of a failure of the server's own, logged.
func goingAgentFailed(call string, err error) *agentwire.Error {
	slog.Error("dispatch: answer the agent", "call", call, "error", err)
	return goingAgentRefusal(agentwire.CodeInternal, "%s: %v", call, err)
}

// goingStaleLease is the refusal of a step, frames call, call, end or renew whose lease is not the
// run's.
func goingStaleLease(run string) *agentwire.Error {
	return goingAgentRefusal(agentwire.CodeStaleLease,
		"run %s is not held under this lease token: another owner claimed it, or it no longer works; nothing was written", run)
}

// goingRunID refuses a run id or lease token that is no UUID, which no run or lease holds.
func goingRunID(values ...string) *agentwire.Error {
	for _, value := range values {
		if _, err := uuid.Parse(value); err != nil {
			return goingAgentRefusal(agentwire.CodeBadRequest, "%q is no run id or lease token", value)
		}
	}
	return nil
}

// hello frees, at once, every lease an earlier agent instance of this server's holds - the
// instances that said hello before this one - and no lease of an agent of another server on the
// same database, and makes those runs ready again, announced on GoingRunsChannel.
func (a *AgentListener) hello(ctx context.Context, request agentwire.HelloRequest) (agentwire.HelloAnswer, *agentwire.Error) {
	if request.Instance == "" {
		return agentwire.HelloAnswer{}, goingAgentRefusal(agentwire.CodeBadRequest, "a hello names its instance")
	}
	a.mu.Lock()
	earlier := make([]string, 0, len(a.instances))
	for instance := range a.instances {
		if instance != request.Instance {
			earlier = append(earlier, instance)
		}
	}
	a.instances[request.Instance] = true
	a.mu.Unlock()
	if len(earlier) == 0 {
		return agentwire.HelloAnswer{}, nil
	}
	var freed int
	if err := a.s.deps.Store.Pool.QueryRow(ctx, `
		with freed as (
			update going_runs set state = 'queued', lease_token = null, lease_owner = null, lease_ends_at = null
			where state = 'working' and lease_owner = any($1::text[])
			returning 1
		)
		select count(*) from freed
	`, earlier).Scan(&freed); err != nil {
		return agentwire.HelloAnswer{}, goingAgentFailed(agentwire.CallHello, err)
	}
	if freed > 0 {
		if err := a.s.notifyGoingRuns(ctx, "ready"); err != nil {
			slog.Warn("dispatch: announce the runs a hello freed", "error", err)
		}
	}
	a.mu.Lock()
	for _, instance := range earlier {
		delete(a.instances, instance)
	}
	a.mu.Unlock()
	return agentwire.HelloAnswer{Freed: freed}, nil
}

// notifyGoingRuns announces payload on GoingRunsChannel, which wakes the waiting claims and renews
// of every process.
func (s *server) notifyGoingRuns(ctx context.Context, payload string) error {
	_, err := s.deps.Store.Pool.Exec(ctx, `select pg_notify($1, $2)`, GoingRunsChannel, payload)
	return err
}

// claim takes one ready run for the instance, waiting up to claimWait for one, woken by each
// announcement on GoingRunsChannel. It answers no run once the server is stopping, once the wait
// ends, or once the agent stops waiting.
func (a *AgentListener) claim(ctx context.Context, request agentwire.ClaimRequest) (agentwire.ClaimAnswer, *agentwire.Error) {
	if request.Instance == "" {
		return agentwire.ClaimAnswer{}, goingAgentRefusal(agentwire.CodeBadRequest, "a claim names its instance")
	}
	waited := time.NewTimer(a.claimWait)
	defer waited.Stop()
	for {
		if a.stopped() {
			return agentwire.ClaimAnswer{}, nil
		}
		woken := a.s.goingRuns.wait()
		run, err := a.claimNext(ctx, request.Instance)
		if err != nil {
			if ctx.Err() != nil {
				return agentwire.ClaimAnswer{}, nil
			}
			return agentwire.ClaimAnswer{}, goingAgentFailed(agentwire.CallClaim, err)
		}
		if run != nil {
			return agentwire.ClaimAnswer{Run: run}, nil
		}
		select {
		case <-woken:
		case <-waited.C:
			return agentwire.ClaimAnswer{}, nil
		case <-ctx.Done():
			return agentwire.ClaimAnswer{}, nil
		case <-a.stopping:
			return agentwire.ClaimAnswer{}, nil
		}
	}
}

// goingPersonLock is the transaction lock on login's runs that a claim and a run's start take
// (admitGoingRun), so the per-person bounds hold under any number of agents and servers.
const goingPersonLock = `select pg_advisory_xact_lock(hashtextextended('going_runs:' || $1, 0))`

// claimOne takes, in one transaction, the oldest ready run - queued, or working with its lease
// run out - whose shape this build reads and whose person works fewer than goingWorkingPerPerson
// runs, each row locked for update and skipped where another claim holds it; only, where it is
// not nil, names the runs it may take. most, where it is above 0, is how many runs the instance
// may hold: an instance that holds that many already takes none, counted under a transaction lock
// on the instance, so its claims at once never pass it. It sets a fresh lease token, the instance
// as the owner and the lease's end, and answers the run with its history and whether this server
// names a decision model, which a line call of the run asks (agentwire.KindLine); nil where no run
// is ready. A run whose history no claim can answer fails with goingRunHistoryTooLarge, and the
// next is taken.
func (a *AgentListener) claimOne(ctx context.Context, instance string, only []string, most int64) (*agentwire.ClaimedRun, error) {
	tx, err := a.s.deps.Store.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if most > 0 {
		if _, err := tx.Exec(ctx, `select pg_advisory_xact_lock(hashtextextended('going_agent:' || $1, 0))`, instance); err != nil {
			return nil, fmt.Errorf("lock %s's runs: %w", instance, err)
		}
		var held int64
		if err := tx.QueryRow(ctx, `
			select count(*) from going_runs where state = 'working' and lease_owner = $1 and lease_ends_at >= now()
		`, instance).Scan(&held); err != nil {
			return nil, fmt.Errorf("count the runs %s holds: %w", instance, err)
		}
		if held >= most {
			return nil, tx.Commit(ctx)
		}
	}
	rows, err := tx.Query(ctx, `
		select id::text, author_login from going_runs
		where state in ('queued', 'working') and (state = 'queued' or lease_ends_at < now()) and shape <= $1
		  and ($3::uuid[] is null or id = any($3::uuid[]))
		order by created_at
		limit $2
		for update skip locked
	`, goingRunShape, goingClaimBatch, only)
	if err != nil {
		return nil, fmt.Errorf("read the ready runs: %w", err)
	}
	type candidate struct{ id, login string }
	candidates, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (candidate, error) {
		var c candidate
		err := row.Scan(&c.id, &c.login)
		return c, err
	})
	if err != nil {
		return nil, fmt.Errorf("read the ready runs: %w", err)
	}
	full := map[string]bool{}
	locked := map[string]bool{}
	for _, c := range candidates {
		if full[c.login] {
			continue
		}
		if !locked[c.login] {
			if _, err := tx.Exec(ctx, goingPersonLock, c.login); err != nil {
				return nil, fmt.Errorf("lock %s's runs: %w", c.login, err)
			}
			locked[c.login] = true
			var working int
			if err := tx.QueryRow(ctx, `
				select count(*) from going_runs where author_login = $1 and state = 'working' and lease_ends_at >= now()
			`, c.login).Scan(&working); err != nil {
				return nil, fmt.Errorf("count %s's working runs: %w", c.login, err)
			}
			if working >= goingWorkingPerPerson {
				full[c.login] = true
				continue
			}
		}
		run, err := claimGoingRun(ctx, tx, c.id, instance)
		if err != nil {
			return nil, err
		}
		if run == nil {
			continue
		}
		if err := tx.Commit(ctx); err != nil {
			return nil, err
		}
		run.Decider = a.s.deps.Decider != nil
		return run, nil
	}
	return nil, tx.Commit(ctx)
}

// claimGoingRun leases run to instance in tx and reads it with its history and the turns of its
// conversation before it (agentwire.EarlierTurns of them), or fails it with
// goingRunHistoryTooLarge and answers nil where its last model turns pass goingHistoryLimit. The
// run's first claim writes how long it waited in the queue into its budget_used, as queued_ms:
// from its created_at to the claim. A later claim of the run leaves it.
//
// A withdrawn run (goingRunWithdrawerOf) is stopped with its withdrawer's reason and answers nil,
// so no agent works it again. The turns hand on no note of a withdrawn earlier run, whoever
// withdrew it, and the run records as its own each post of the earlier runs whose notes they hand
// on (going_run_posts), in the claim's own transaction, so the delete of a post an earlier run read
// withdraws the follow-up that was given what that run read.
func claimGoingRun(ctx context.Context, tx pgx.Tx, run, instance string) (*agentwire.ClaimedRun, error) {
	claimed := agentwire.ClaimedRun{ID: run}
	var continues *string
	var chip []byte
	var budget []byte
	var withdrawer goingRunWithdrawer
	if err := tx.QueryRow(ctx, `
		update going_runs r set state = 'working', lease_token = gen_random_uuid(), lease_owner = $2,
		  lease_ends_at = now() + make_interval(secs => $3),
		  budget_used = case when budget_used ? 'queued_ms' then budget_used
		    else budget_used || jsonb_build_object('queued_ms',
		      greatest(floor(extract(epoch from clock_timestamp() - created_at) * 1000), 0)::bigint) end
		where id = $1::uuid
		returning coalesce(conversation_id, id)::text, continues::text, words, context, coalesce(kind, ''), coalesce(line, ''), coalesce(level, ''), coalesce(mode, ''),
		  shape, budget_used, lease_token::text, lease_ends_at, `+goingRunWithdrawerOf("r.id")+`
	`, run, instance, goingLeaseTerm.Seconds()).Scan(&claimed.Conversation, &continues, &claimed.Words, &chip, &claimed.Kind, &claimed.Line,
		&claimed.Level, &claimed.Mode, &claimed.Shape, &budget, &claimed.LeaseToken, &claimed.LeaseEndsAt, &withdrawer); err != nil {
		return nil, fmt.Errorf("lease run %s: %w", run, err)
	}
	if withdrawer != goingRunNotWithdrawn {
		if err := stopGoingRunIn(ctx, tx, run, withdrawer.reason(), []string{}); err != nil {
			return nil, fmt.Errorf("stop run %s, which is withdrawn: %w", run, err)
		}
		return nil, nil
	}
	if continues != nil {
		claimed.Continues = *continues
	}
	claimed.Context = chip
	if err := json.Unmarshal(budget, &claimed.BudgetUsed); err != nil {
		return nil, fmt.Errorf("read what run %s spent: %w", run, err)
	}
	// The history's last agentwire.WholeTurns model turns go whole: every step from the
	// WholeTurns-th newest model step on, which is every step of a run that made fewer model
	// calls (agentwire.WholeFrom).
	rows, err := tx.Query(ctx, `
		select step, kind, note, case when step >= coalesce((
		    select m.step from going_run_steps m where m.run_id = $1::uuid and m.kind = $2
		    order by m.step desc offset $3 - 1 limit 1), 0) then body end
		from going_run_steps where run_id = $1::uuid
		order by step
	`, run, agentwire.StepModel, agentwire.WholeTurns)
	if err != nil {
		return nil, fmt.Errorf("read run %s's history: %w", run, err)
	}
	history, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (agentwire.SavedStep, error) {
		var step agentwire.SavedStep
		var body []byte
		err := row.Scan(&step.Step, &step.Kind, &step.Note, &body)
		step.Body = body
		return step, err
	})
	if err != nil {
		return nil, fmt.Errorf("read run %s's history: %w", run, err)
	}
	whole := 0
	for _, step := range history {
		whole += len(step.Body)
	}
	if whole > goingHistoryLimit {
		if _, err := tx.Exec(ctx, `
			update going_runs set state = 'failed', stop_reason = $2, lease_token = null, lease_owner = null,
			  lease_ends_at = null, finished_at = now()
			where id = $1::uuid
		`, run, goingRunHistoryTooLarge); err != nil {
			return nil, fmt.Errorf("fail run %s, whose history is too large: %w", run, err)
		}
		return nil, nil
	}
	claimed.History = history
	if claimed.History == nil {
		claimed.History = []agentwire.SavedStep{}
	}
	if claimed.Rows, err = goingPendingRows(ctx, tx, run); err != nil {
		return nil, err
	}
	turns, err := tx.Query(ctx, `
		select e.id::text, e.words, array(select s.note from going_run_steps s where s.run_id = e.id order by s.step)
		from going_runs r
		join going_runs e on coalesce(e.conversation_id, e.id) = coalesce(r.conversation_id, r.id) and e.created_at < r.created_at
		where r.id = $1::uuid and not `+goingRunWithdrawnOf("e.id")+`
		order by e.created_at desc
		limit $2
	`, run, agentwire.EarlierTurns)
	if err != nil {
		return nil, fmt.Errorf("read the turns before run %s: %w", run, err)
	}
	claimed.Turns, err = pgx.CollectRows(turns, func(row pgx.CollectableRow) (agentwire.Turn, error) {
		var turn agentwire.Turn
		err := row.Scan(&turn.Run, &turn.Words, &turn.Notes)
		return turn, err
	})
	if err != nil {
		return nil, fmt.Errorf("read the turns before run %s: %w", run, err)
	}
	slices.Reverse(claimed.Turns)
	if claimed.Turns == nil {
		claimed.Turns = []agentwire.Turn{}
	}
	if len(claimed.Turns) > 0 {
		earlier := make([]string, 0, len(claimed.Turns))
		for _, turn := range claimed.Turns {
			earlier = append(earlier, turn.Run)
		}
		if _, err := tx.Exec(ctx, `
			insert into going_run_posts (run_id, comment_id, message_id)
			select $1::uuid, comment_id, message_id from going_run_posts where run_id = any($2::uuid[])
			on conflict do nothing
		`, run, earlier); err != nil {
			return nil, fmt.Errorf("record the posts the turns before run %s read: %w", run, err)
		}
	}
	return &claimed, nil
}

// admitGoingRun takes login's run lock (goingPersonLock) in tx, the transaction of the request
// that starts a run, and refuses the run 429 GOING_RUN_QUEUE_FULL where login already has
// goingQueuedPerPerson runs queued. The lock is the claim's, so the count holds until tx ends.
func admitGoingRun(ctx context.Context, tx pgx.Tx, login string) error {
	if _, err := tx.Exec(ctx, goingPersonLock, login); err != nil {
		return fmt.Errorf("lock %s's runs: %w", login, err)
	}
	var queued int
	if err := tx.QueryRow(ctx, `select count(*) from going_runs where author_login = $1 and state = 'queued'`, login).Scan(&queued); err != nil {
		return fmt.Errorf("count %s's queued runs: %w", login, err)
	}
	if queued >= goingQueuedPerPerson {
		return errorf(http.StatusTooManyRequests, codeGoingRunQueueFull,
			"%d of your runs already wait in the queue, the most one person may have; this run was not started: wait for one to start, or stop one", queued)
	}
	return nil
}

// renew renews, for goingLeaseTerm, each lease the request names that the instance still holds of
// a working run, keeps the agent's health and when the renew arrived (LastRenewal), and answers
// once renewWait has passed, once a renewed run is stopped, once the server is stopping, or once
// the agent stops waiting. A lease whose run was stopped is answered stopped; any other lease not
// renewed is lost.
func (a *AgentListener) renew(ctx context.Context, request agentwire.RenewRequest) (agentwire.RenewAnswer, *agentwire.Error) {
	if request.Instance == "" {
		return agentwire.RenewAnswer{}, goingAgentRefusal(agentwire.CodeBadRequest, "a renew names its instance")
	}
	a.renewed.Store(time.Now().UnixNano())
	health := request.Health
	a.health.Store(&health)
	answer := agentwire.RenewAnswer{Renewed: []agentwire.RenewedLease{}, Stopped: []string{}, Lost: []string{}}
	runs, tokens := make([]string, 0, len(request.Leases)), make([]string, 0, len(request.Leases))
	for _, lease := range request.Leases {
		if goingRunID(lease.Run, lease.LeaseToken) != nil {
			answer.Lost = append(answer.Lost, lease.Run)
			continue
		}
		runs, tokens = append(runs, lease.Run), append(tokens, lease.LeaseToken)
	}
	if len(runs) > 0 {
		rows, err := a.s.deps.Store.Pool.Query(ctx, `
			update going_runs r set lease_ends_at = now() + make_interval(secs => $4)
			from unnest($1::uuid[], $2::uuid[]) as l(id, token)
			where r.id = l.id and r.lease_token = l.token and r.lease_owner = $3 and r.state = 'working'
			returning r.id::text, r.lease_ends_at
		`, runs, tokens, request.Instance, goingLeaseTerm.Seconds())
		if err != nil {
			return agentwire.RenewAnswer{}, goingAgentFailed(agentwire.CallRenew, err)
		}
		renewed, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (agentwire.RenewedLease, error) {
			var lease agentwire.RenewedLease
			err := row.Scan(&lease.Run, &lease.EndsAt)
			return lease, err
		})
		if err != nil {
			return agentwire.RenewAnswer{}, goingAgentFailed(agentwire.CallRenew, err)
		}
		answer.Renewed = append(answer.Renewed, renewed...)
		held := map[string]bool{}
		for _, lease := range renewed {
			held[lease.Run] = true
		}
		var unheld []string
		for _, run := range runs {
			if !held[run] {
				unheld = append(unheld, run)
			}
		}
		stopped, err := a.s.stoppedGoingRuns(ctx, unheld)
		if err != nil {
			return agentwire.RenewAnswer{}, goingAgentFailed(agentwire.CallRenew, err)
		}
		for _, run := range unheld {
			if stopped[run] {
				answer.Stopped = append(answer.Stopped, run)
			} else {
				answer.Lost = append(answer.Lost, run)
			}
		}
	}
	if len(answer.Stopped) > 0 {
		return answer, nil
	}
	waited := time.NewTimer(a.renewWait)
	defer waited.Stop()
	for {
		woken := a.s.goingRuns.wait()
		renewed := make([]string, 0, len(answer.Renewed))
		for _, lease := range answer.Renewed {
			renewed = append(renewed, lease.Run)
		}
		stopped, err := a.s.stoppedGoingRuns(ctx, renewed)
		if err != nil {
			if ctx.Err() != nil {
				return answer, nil
			}
			return agentwire.RenewAnswer{}, goingAgentFailed(agentwire.CallRenew, err)
		}
		if len(stopped) > 0 {
			kept := answer.Renewed[:0]
			for _, lease := range answer.Renewed {
				if stopped[lease.Run] {
					answer.Stopped = append(answer.Stopped, lease.Run)
				} else {
					kept = append(kept, lease)
				}
			}
			answer.Renewed = kept
			return answer, nil
		}
		select {
		case <-woken:
		case <-waited.C:
			return answer, nil
		case <-ctx.Done():
			return answer, nil
		case <-a.stopping:
			return answer, nil
		}
	}
}

// stoppedGoingRuns is which of runs are stopped.
func (s *server) stoppedGoingRuns(ctx context.Context, runs []string) (map[string]bool, error) {
	stopped := map[string]bool{}
	if len(runs) == 0 {
		return stopped, nil
	}
	rows, err := s.deps.Store.Pool.Query(ctx, `select id::text from going_runs where id = any($1::uuid[]) and state = 'stopped'`, runs)
	if err != nil {
		return nil, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		stopped[id] = true
	}
	return stopped, nil
}

// stopGoingRun stops run for reason: it is set stopped and its lease freed, so its owner can write
// nothing more of it, its end frame names the reason (stopGoingRunIn), and the stop is announced on
// GoingRunsChannel, which answers the owner's waiting renew at once, on whichever process it
// waits. A run that waited for its person also appends going_plan.updated, since its plan no
// longer waits. It reports whether the run was one that could stop: queued, working or waiting.
func (s *server) stopGoingRun(ctx context.Context, run, reason string) (bool, error) {
	tx, err := s.deps.Store.Pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var state, login string
	err = tx.QueryRow(ctx, `select coalesce(state, ''), author_login from going_runs where id = $1::uuid for update`, run).Scan(&state, &login)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("hold run %s: %w", run, err)
	}
	switch state {
	case "queued", "working", agentwire.StateWaiting:
	default:
		return false, nil
	}
	if err := stopGoingRunIn(ctx, tx, run, reason, []string{}); err != nil {
		return false, err
	}
	var events []model.Event
	if state == agentwire.StateWaiting {
		appended, err := s.appendGoingPlanEvent(ctx, tx, login, run)
		if err != nil {
			return false, err
		}
		events = append(events, appended)
	}
	// Delivered when the transaction commits, so no renew reads the run before its stop.
	if _, err := tx.Exec(ctx, `select pg_notify($1, $2)`, GoingRunsChannel, "stop"); err != nil {
		return false, fmt.Errorf("announce the stop of run %s: %w", run, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	s.publish(ctx, events...)
	return true, nil
}

// stopGoingRunIn sets run stopped for reason in tx, its lease freed, and appends its end frame,
// which offers offers; the end of a new-issue run whose create landed also names the issue it
// filed and offers Undo (goingNewIssueEnd). The caller holds the run's row for update.
func stopGoingRunIn(ctx context.Context, tx pgx.Tx, run, reason string, offers []string) error {
	ended := agentwire.EndFrame{State: agentwire.StateStopped, Reason: reason, Offers: offers}
	if err := goingNewIssueEnd(ctx, tx, run, &ended); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		update going_runs set state = 'stopped', stop_reason = $2, lease_token = null, lease_owner = null,
		  lease_ends_at = null, finished_at = now()
		where id = $1::uuid
	`, run, reason); err != nil {
		return fmt.Errorf("stop run %s: %w", run, err)
	}
	frame, err := json.Marshal(ended)
	if err != nil {
		return err
	}
	if err := appendGoingRunFrames(ctx, tx, run, []agentwire.Frame{{Name: agentwire.FrameEnd, Data: frame}}); err != nil {
		return fmt.Errorf("end run %s: %w", run, err)
	}
	return nil
}

// step saves one step of a run and appends its frames to the run's events, in sequence, in one
// transaction that holds the run's row for share and checks, under it, that the request's lease
// token is the run's and the run works: a stale owner's step is refused and nothing of it is
// written. The text of a reply frame that answers is appended so the words the model gave a link
// to an item stand beside the item's chip (goingKeepLinkWords), each item read in the same
// transaction. A second try of a step already saved saves nothing again, and answers the events'
// sequence as they stand. A new step of a suspect run ends its suspicion (settleSuspect).
func (a *AgentListener) step(ctx context.Context, request agentwire.StepRequest) (agentwire.StepAnswer, *agentwire.Error) {
	if refusal := goingRunID(request.Run, request.LeaseToken); refusal != nil {
		return agentwire.StepAnswer{}, refusal
	}
	if request.Step < 1 || request.Kind == "" || !json.Valid(request.Body) {
		return agentwire.StepAnswer{}, goingAgentRefusal(agentwire.CodeBadRequest, "a step has a number from 1, a kind and a JSON body")
	}
	if refusal := goingFramesRefused(request.Frames, "a step"); refusal != nil {
		return agentwire.StepAnswer{}, refusal
	}
	tx, err := a.s.deps.Store.Pool.Begin(ctx)
	if err != nil {
		return agentwire.StepAnswer{}, goingAgentFailed(agentwire.CallStep, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if held, err := goingLeaseHeld(ctx, tx, request.Run, request.LeaseToken); err != nil {
		return agentwire.StepAnswer{}, goingAgentFailed(agentwire.CallStep, err)
	} else if !held {
		return agentwire.StepAnswer{}, goingStaleLease(request.Run)
	}
	tag, err := tx.Exec(ctx, `
		insert into going_run_steps (run_id, step, kind, body, note) values ($1::uuid, $2, $3, $4::jsonb, $5)
		on conflict (run_id, step) do nothing
	`, request.Run, request.Step, request.Kind, string(request.Body), request.Note)
	if err != nil {
		return agentwire.StepAnswer{}, goingAgentFailed(agentwire.CallStep, err)
	}
	if tag.RowsAffected() > 0 {
		if err := goingKeepLinkWords(ctx, tx, request.Run, request.Frames); err != nil {
			return agentwire.StepAnswer{}, goingAgentFailed(agentwire.CallStep, err)
		}
		if err := appendGoingRunFrames(ctx, tx, request.Run, request.Frames); err != nil {
			return agentwire.StepAnswer{}, goingAgentFailed(agentwire.CallStep, err)
		}
	}
	var sequence int64
	if err := tx.QueryRow(ctx, `select coalesce(max(sequence), 0) from going_run_events where run_id = $1::uuid`, request.Run).Scan(&sequence); err != nil {
		return agentwire.StepAnswer{}, goingAgentFailed(agentwire.CallStep, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return agentwire.StepAnswer{}, goingAgentFailed(agentwire.CallStep, err)
	}
	if tag.RowsAffected() > 0 {
		a.settleSuspect(request.Run)
	}
	return agentwire.StepAnswer{Sequence: sequence}, nil
}

// frames appends frames to a run's events, in sequence, and saves no step: the frame of a tool
// call as the call starts, which the bar shows while the call runs. It is refused as a step is:
// a frame with no name or no JSON data, and a lease token that is not the run's or a run that no
// longer works, checked under the run's row held for share in the transaction that appends, so a
// stale owner appends nothing; and its request is bounded as a step's (agentwire.RequestLimit).
// It ends no suspicion (settleSuspect): a call's frame goes before the call, which may be what
// kills the agent.
func (a *AgentListener) frames(ctx context.Context, request agentwire.FramesRequest) (agentwire.FramesAnswer, *agentwire.Error) {
	if refusal := goingRunID(request.Run, request.LeaseToken); refusal != nil {
		return agentwire.FramesAnswer{}, refusal
	}
	if len(request.Frames) == 0 {
		return agentwire.FramesAnswer{}, goingAgentRefusal(agentwire.CodeBadRequest, "a frames call carries one frame or more")
	}
	if refusal := goingFramesRefused(request.Frames, "a frames call"); refusal != nil {
		return agentwire.FramesAnswer{}, refusal
	}
	tx, err := a.s.deps.Store.Pool.Begin(ctx)
	if err != nil {
		return agentwire.FramesAnswer{}, goingAgentFailed(agentwire.CallFrames, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if held, err := goingLeaseHeld(ctx, tx, request.Run, request.LeaseToken); err != nil {
		return agentwire.FramesAnswer{}, goingAgentFailed(agentwire.CallFrames, err)
	} else if !held {
		return agentwire.FramesAnswer{}, goingStaleLease(request.Run)
	}
	if err := appendGoingRunFrames(ctx, tx, request.Run, request.Frames); err != nil {
		return agentwire.FramesAnswer{}, goingAgentFailed(agentwire.CallFrames, err)
	}
	var sequence int64
	if err := tx.QueryRow(ctx, `select coalesce(max(sequence), 0) from going_run_events where run_id = $1::uuid`, request.Run).Scan(&sequence); err != nil {
		return agentwire.FramesAnswer{}, goingAgentFailed(agentwire.CallFrames, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return agentwire.FramesAnswer{}, goingAgentFailed(agentwire.CallFrames, err)
	}
	return agentwire.FramesAnswer{Sequence: sequence}, nil
}

// goingFramesRefused refuses the frames of of, a step or a frames call, where one has no name or
// no JSON data.
func goingFramesRefused(frames []agentwire.Frame, of string) *agentwire.Error {
	for _, frame := range frames {
		if frame.Name == "" || !json.Valid(frame.Data) {
			return goingAgentRefusal(agentwire.CodeBadRequest, "each frame of %s has a name and JSON data", of)
		}
	}
	return nil
}

// goingLeaseHeld locks run's row for share in tx and reports whether token is its lease token and
// the run works. A claim locks the row for update, so no new lease is taken until tx ends.
func goingLeaseHeld(ctx context.Context, tx pgx.Tx, run, token string) (bool, error) {
	var held bool
	err := tx.QueryRow(ctx, `
		select coalesce(lease_token = $2::uuid, false) and state = 'working' from going_runs where id = $1::uuid for share
	`, run, token).Scan(&held)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return held, err
}

// end ends the run's turn: it sets the run's state and stop reason, frees its lease, and adds what
// the turn spent to its budget, keeping every other field of budget_used (the queued_ms of its
// first claim), in one statement whose condition is the request's lease token and the run
// working, so a stale owner's end changes nothing; and in the same transaction it appends
// the run's end frame (agentwire.EndFrame), which names what the run's record offers its person
// next. A turn that ends done while a row of the run's plan waits for its person leaves the run
// waiting (goingRowsWaitSQL), and a run left waiting appends going_plan.updated, since its plan now
// waits in its person's Inbox. The end of a new-issue run names the issue it filed and offers Undo,
// or, where it filed none, ends the run failed and says why (goingNewIssueEnd). The statement locks
// the run's row for the update it makes, which no share lock is taken before: a share lock held
// while the update waits behind a renew's would deadlock with it. An ended run is no suspect any
// more, and its instance's run slot is free (runEnded).
func (a *AgentListener) end(ctx context.Context, request agentwire.EndRequest) (agentwire.EndAnswer, *agentwire.Error) {
	if refusal := goingRunID(request.Run, request.LeaseToken); refusal != nil {
		return agentwire.EndAnswer{}, refusal
	}
	switch request.State {
	case agentwire.StateDone, agentwire.StateWaiting, agentwire.StateStopped, agentwire.StateFailed:
	default:
		return agentwire.EndAnswer{}, goingAgentRefusal(agentwire.CodeBadRequest, "an end leaves its run done, waiting, stopped or failed, not %q", request.State)
	}
	for _, offer := range request.Offers {
		if offer != agentwire.OfferContinue {
			return agentwire.EndAnswer{}, goingAgentRefusal(agentwire.CodeBadRequest, "an end offers %q, not %q", agentwire.OfferContinue, offer)
		}
	}
	tx, err := a.s.deps.Store.Pool.Begin(ctx)
	if err != nil {
		return agentwire.EndAnswer{}, goingAgentFailed(agentwire.CallEnd, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if request.State == agentwire.StateDone {
		var waits bool
		if err := tx.QueryRow(ctx, goingRowsWaitSQL, request.Run).Scan(&waits); err != nil {
			return agentwire.EndAnswer{}, goingAgentFailed(agentwire.CallEnd, err)
		}
		if waits {
			request.State = agentwire.StateWaiting
		}
	}
	ended := agentwire.EndFrame{State: request.State, Reason: request.Reason, Offers: append([]string{}, request.Offers...)}
	if err := goingNewIssueEnd(ctx, tx, request.Run, &ended); err != nil {
		return agentwire.EndAnswer{}, goingAgentFailed(agentwire.CallEnd, err)
	}
	frame, err := json.Marshal(ended)
	if err != nil {
		return agentwire.EndAnswer{}, goingAgentFailed(agentwire.CallEnd, err)
	}
	var login string
	err = tx.QueryRow(ctx, `
		update going_runs set state = $3, stop_reason = nullif($4, ''), lease_token = null, lease_owner = null,
		  lease_ends_at = null, finished_at = case when $3 = 'waiting' then null else now() end,
		  budget_used = budget_used || jsonb_build_object(
		    'calls', coalesce((budget_used->>'calls')::int, 0) + $5,
		    'working_ms', coalesce((budget_used->>'working_ms')::bigint, 0) + $6,
		    'model_ms', coalesce((budget_used->>'model_ms')::bigint, 0) + $8,
		    'pace_ms', coalesce((budget_used->>'pace_ms')::bigint, 0) + $10,
		    'tool_ms', coalesce((budget_used->>'tool_ms')::bigint, 0) + $9,
		    'tokens', coalesce((budget_used->>'tokens')::int, 0) + $7)
		where id = $1::uuid and lease_token = $2::uuid and state = 'working'
		returning author_login
	`, request.Run, request.LeaseToken, ended.State, ended.Reason, request.Used.Calls, request.Used.WorkingMS, request.Used.Tokens,
		request.Used.ModelMS, request.Used.ToolMS, request.Used.PaceMS).Scan(&login)
	if errors.Is(err, pgx.ErrNoRows) {
		return agentwire.EndAnswer{}, goingStaleLease(request.Run)
	}
	if err != nil {
		return agentwire.EndAnswer{}, goingAgentFailed(agentwire.CallEnd, err)
	}
	if err := appendGoingRunFrames(ctx, tx, request.Run, []agentwire.Frame{{Name: agentwire.FrameEnd, Data: frame}}); err != nil {
		return agentwire.EndAnswer{}, goingAgentFailed(agentwire.CallEnd, err)
	}
	// A turn that leaves its run waiting puts its plan in the person's Inbox.
	var events []model.Event
	if ended.State == agentwire.StateWaiting {
		appended, err := a.s.appendGoingPlanEvent(ctx, tx, login, request.Run)
		if err != nil {
			return agentwire.EndAnswer{}, goingAgentFailed(agentwire.CallEnd, err)
		}
		events = append(events, appended)
	}
	if err := tx.Commit(ctx); err != nil {
		return agentwire.EndAnswer{}, goingAgentFailed(agentwire.CallEnd, err)
	}
	a.s.publish(ctx, events...)
	a.runEnded(request.Run)
	return agentwire.EndAnswer{}, nil
}

// appendGoingRunFrames appends frames to run's events in tx, in order, each the next sequence
// after the last, under the run's events lock, so the frames of two writers never share a
// sequence, and announces the run on GoingRunEventsChannel when tx commits, which wakes the
// streams of its events on every process. The announcement names the run as its canonical uuid
// text, which is what a stream waits on whatever form of the id the writer was given, and goes
// in the insert's own statement: Postgres runs a data-modifying WITH to its end whatever reads it.
func appendGoingRunFrames(ctx context.Context, tx pgx.Tx, run string, frames []agentwire.Frame) error {
	if len(frames) == 0 {
		return nil
	}
	if _, err := tx.Exec(ctx, `select pg_advisory_xact_lock(hashtextextended('going_run_events:' || $1, 0))`, run); err != nil {
		return err
	}
	names, data := make([]string, 0, len(frames)), make([]string, 0, len(frames))
	for _, frame := range frames {
		names, data = append(names, frame.Name), append(data, string(frame.Data))
	}
	_, err := tx.Exec(ctx, `
		with appended as (
			insert into going_run_events (run_id, sequence, name, data)
			select $1::uuid, base.last + f.n, f.name, f.data::jsonb
			from (select coalesce(max(sequence), 0) as last from going_run_events where run_id = $1::uuid) base,
			     unnest($2::text[], $3::text[]) with ordinality as f(name, data, n)
		)
		select pg_notify($4, $1::uuid::text)
	`, run, names, data, GoingRunEventsChannel)
	return err
}
