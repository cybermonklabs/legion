// Package agent is the loop of the Dispatch server's agent process (envoy-dispatch agent): it
// says hello to the server under a new instance id, keeps one renew waiting at all times, which
// carries the process's health and answers a stop at once, and claims ready runs, each worked on
// a goroutine of its own, as many at once as its slots. A run is one agentloop.Run per step on
// the chat model, each started afresh from the run's saved steps, with five tools whose every
// read of Dispatch, GitHub or the web is a call of agent/v1 (going/agentwire) to the server,
// which makes it as the run's person.
//
// The agent holds no Dispatch data and no Dispatch secret: its configuration is the chat model's
// settings alone, and it reads nothing but its socket and the model's token file.
package agent

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"runtime/debug"
	"runtime/metrics"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/agentloop"
	"github.com/sjawhar/envoy/going/agentwire"
)

// retryAfter is how long the loop waits before it makes a renew or a claim again after one failed
// for any reason but its context ending: the server may be starting or stopping.
const retryAfter = time.Second

// stuckLimit is how many goroutines the watchdog counts stuck before the agent exits, so its
// server starts it again.
const stuckLimit = 3

// claimGate is the share of the agent's memory limit (GOMEMLIMIT) above which it takes no new
// claim.
const claimGate = 0.7

// exitWait is how long Run, once its context ends, waits for its runs' goroutines to return.
const exitWait = 500 * time.Millisecond

// Config is the agent's configuration, the chat model's settings alone: the model its runs call,
// a zero ID where the server names none; the chat deployment's tokens a minute, which the agent
// paces its calls to its share of, 0 for no quota; how many runs it works at once; and whether
// the test hooks are on (hooks.go), which only an end-to-end test's server turns on.
type Config struct {
	Model           agentloop.Model
	TokensPerMinute int
	Slots           int
	Hooks           bool
}

// errStuck is Run's end once the watchdog counted stuckLimit goroutines stuck.
var errStuck = fmt.Errorf("%d goroutines of the agent's runs are stuck past their watchdog; the agent exits, so its server starts it again", stuckLimit)

// agent is one instance of the agent process: its client of the server, its configuration and
// pacer, the runs it works, and the goroutines its watchdog counts stuck.
type agent struct {
	client   agentwire.Client
	config   Config
	pacer    *pacer
	instance string

	mu   sync.Mutex
	held map[string]*worker
	// claimed wakes the renewer when a run is claimed, so the renew that waits names its lease
	// and a stop of it is answered at once.
	claimed chan struct{}

	stuck    atomic.Int64
	exit     chan struct{}
	exitOnce sync.Once
	running  sync.WaitGroup
}

// Run is the agent's loop at the server's socket client calls, until ctx ends, when it returns
// nil. It fails when the server refuses its hello, and once its watchdog counts stuckLimit
// goroutines stuck; the process supervisor answers either by starting the agent again.
func Run(ctx context.Context, client agentwire.Client, config Config) error {
	a := &agent{
		client: client, config: config, pacer: newPacer(config.TokensPerMinute), instance: uuid.NewString(),
		held: map[string]*worker{}, claimed: make(chan struct{}, 1), exit: make(chan struct{}),
	}
	if a.config.Slots <= 0 {
		a.config.Slots = 1
	}
	if _, err := client.Hello(ctx, agentwire.HelloRequest{Instance: a.instance}); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return fmt.Errorf("say hello to the server: %w", err)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var renewing sync.WaitGroup
	renewing.Go(func() { a.renew(ctx) })
	err := a.claim(ctx)
	cancel()
	renewing.Wait()
	if err != nil {
		// The stuck goroutines never return, so the process exits beside them.
		return err
	}
	ran := make(chan struct{})
	go func() {
		a.running.Wait()
		close(ran)
	}()
	select {
	case <-ran:
	case <-time.After(exitWait):
	}
	return nil
}

// claim claims ready runs, one at a time, while a slot is free and the agent's memory is under its
// claim gate, and works each on a goroutine of its own, until ctx ends or the watchdog's count
// ends the agent.
func (a *agent) claim(ctx context.Context) error {
	slots := make(chan struct{}, a.config.Slots)
	gated := false
	for {
		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
			return nil
		case <-a.exit:
			return errStuck
		}
		release := sync.OnceFunc(func() { <-slots })
		if over, used, limit := overMemory(); over {
			release()
			if !gated {
				slog.Warn("agent: its memory is above 70% of its limit, so it claims no run until it falls", "used", used, "limit", limit)
				gated = true
			}
			pause(ctx)
			continue
		} else if gated {
			slog.Info("agent: its memory is under 70% of its limit again, so it claims runs", "used", used, "limit", limit)
			gated = false
		}
		answer, err := a.client.Claim(ctx, agentwire.ClaimRequest{Instance: a.instance})
		if err != nil || answer.Run == nil {
			release()
			if err != nil && ctx.Err() == nil {
				slog.Warn("agent: claim a run", "error", err)
				pause(ctx)
			}
			continue
		}
		w := newWorker(ctx, a, answer.Run, release)
		a.hold(w)
		a.running.Go(func() { a.work(w) })
	}
}

// work works w's run, under a recover: a panic of the run's own goroutine fails the run with its
// reason and stops nothing else. The run's slot is free and its lease no longer renewed once it
// returns.
func (a *agent) work(w *worker) {
	defer w.release()
	defer a.drop(w)
	defer func() {
		if recovered := recover(); recovered != nil {
			slog.Error("agent: a run's goroutine panicked; the run fails and the agent goes on", "run", w.run.ID, "panic", recovered,
				"stack", string(debug.Stack()))
			w.end(agentwire.StateFailed, fmt.Sprintf("%s: the agent's goroutine of this run panicked: %v", codeAgentFailed, recovered), nil)
		}
	}()
	w.work()
}

// hold adds w to the runs the agent works, whose leases each renew names, and wakes the renewer,
// so the renew that waits names w's lease too.
func (a *agent) hold(w *worker) {
	a.mu.Lock()
	a.held[w.run.ID] = w
	a.mu.Unlock()
	select {
	case a.claimed <- struct{}{}:
	default:
	}
}

// drop takes w from the runs the agent works: its lease is renewed no more.
func (a *agent) drop(w *worker) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.held[w.run.ID] == w {
		delete(a.held, w.run.ID)
	}
}

// leases is the lease of each run the agent works.
func (a *agent) leases() []agentwire.Lease {
	a.mu.Lock()
	defer a.mu.Unlock()
	leases := make([]agentwire.Lease, 0, len(a.held))
	for _, w := range a.held {
		leases = append(leases, agentwire.Lease{Run: w.run.ID, LeaseToken: w.run.LeaseToken})
	}
	return leases
}

// worker is the worker of run, nil for none.
func (a *agent) worker(run string) *worker {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.held[run]
}

// renew keeps one renew of the agent's leases waiting at the server until ctx ends, each carrying
// the process's health. A run the person stops is answered at once, and its worker's context
// cancelled, its model stream in flight with it; a run whose lease was lost is given up. A run
// claimed while a renew waits ends that renew and starts the next, which names its lease.
func (a *agent) renew(ctx context.Context) {
	for ctx.Err() == nil {
		leases := a.leases()
		health := agentwire.RuntimeHealth()
		health.Working, health.StuckGoroutines = len(leases), int(a.stuck.Load())
		waiting, cancel := context.WithCancel(ctx)
		answered := make(chan struct{})
		var answer agentwire.RenewAnswer
		var err error
		go func() {
			defer close(answered)
			answer, err = a.client.Renew(waiting, agentwire.RenewRequest{Instance: a.instance, Leases: leases, Health: health})
		}()
		select {
		case <-answered:
		case <-a.claimed:
			cancel()
			<-answered
			continue
		}
		cancel()
		if err != nil {
			if ctx.Err() == nil {
				slog.Warn("agent: renew", "error", err)
				pause(ctx)
			}
			continue
		}
		for _, run := range answer.Stopped {
			if w := a.worker(run); w != nil {
				slog.Info("agent: its person stopped a run", "run", run)
				w.cancel()
			}
		}
		for _, run := range answer.Lost {
			if w := a.worker(run); w != nil {
				slog.Warn("agent: a run is no longer held under its lease; the agent gives it up", "run", run)
				w.lose()
			}
		}
	}
}

// watch starts the watchdog of one step of w, which gives w's run up once the step runs within
// past its start, and answers what the step calls once it returns: it stops the watchdog and
// reports whether the step returned before the watchdog gave it up. The watchdog gives up a run
// whose goroutine ignores its context: it renews the run's lease no more, so its lease runs out
// and the next claim takes it, frees its slot, and counts the goroutine stuck; at stuckLimit the
// agent exits. A goroutine that returns after all is no longer counted.
func (a *agent) watch(w *worker, within time.Duration) func() bool {
	gaveUp := make(chan struct{})
	timer := time.AfterFunc(within, func() {
		defer close(gaveUp)
		w.lose()
		a.drop(w)
		w.release()
		stuck := a.stuck.Add(1)
		slog.Warn("agent: the watchdog gave up a run whose step ran past its time; its goroutine is counted stuck",
			"run", w.run.ID, "within", within, "stuck", stuck)
		if stuck >= stuckLimit {
			a.exitOnce.Do(func() { close(a.exit) })
		}
	})
	return func() bool {
		if timer.Stop() {
			return true
		}
		<-gaveUp
		a.stuck.Add(-1)
		return false
	}
}

// overMemory reports whether the process's memory, as the runtime counts it against its limit -
// all it has mapped less what it released - is above claimGate of its limit (GOMEMLIMIT), with
// both; false with no limit set.
func overMemory() (bool, uint64, uint64) {
	limit := debug.SetMemoryLimit(-1)
	if limit <= 0 || limit == math.MaxInt64 {
		return false, 0, 0
	}
	samples := []metrics.Sample{{Name: "/memory/classes/total:bytes"}, {Name: "/memory/classes/heap/released:bytes"}}
	metrics.Read(samples)
	var used uint64
	if samples[0].Value.Kind() == metrics.KindUint64 && samples[1].Value.Kind() == metrics.KindUint64 {
		used = samples[0].Value.Uint64() - samples[1].Value.Uint64()
	}
	return float64(used) > claimGate*float64(limit), used, uint64(limit)
}

// pause waits retryAfter, or until ctx ends.
func pause(ctx context.Context) {
	timer := time.NewTimer(retryAfter)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
	}
}
