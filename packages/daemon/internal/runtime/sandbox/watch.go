package sandbox

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// observer is the running Observe's queue: the watched claims to evaluate, each once however many
// changes marked it, and a wake for its loop.
type observer struct {
	dirty map[claim.Token]bool
	wake  chan struct{}
}

// mark queues token for evaluation. Called with r.mu held.
func (o *observer) mark(token claim.Token) {
	o.dirty[token] = true
	select {
	case o.wake <- struct{}{}:
	default:
	}
}

// join records loc as its claim's incarnation and, under a running Observe, evaluates it at once
// against the synced stores: a process that died while no runtime watched it is reported the
// moment the runtime is told of it.
func (r *Runtime) join(loc runtime.Locator) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.watch[loc.Claim] = loc
	if r.observer != nil {
		r.observer.mark(loc.Claim)
	}
}

// adopt joins loc only when the watch holds nothing for its claim. The daemon's sweep reads its
// claims unordered against the machines, so a located Known may be older than the incarnation a
// relaunch already recorded here; the watch's entry is the newer one and stays.
func (r *Runtime) adopt(loc runtime.Locator) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.watch[loc.Claim]; ok {
		return
	}
	r.watch[loc.Claim] = loc
	if r.observer != nil {
		r.observer.mark(loc.Claim)
	}
}

// forget drops the claim from the watch, whatever incarnation it held, and the session its last
// launch named with it.
func (r *Runtime) forget(token claim.Token) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.watch, token)
	delete(r.sessions, token)
}

// forgetIf drops loc from the watch only while it is still the claim's recorded incarnation, so a
// stale locator never drops a newer relaunch's; the session a launch named goes with that
// launch's incarnation in the same way.
func (r *Runtime) forgetIf(loc runtime.Locator) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if current, ok := r.watch[loc.Claim]; ok && current.Incarnation == loc.Incarnation {
		delete(r.watch, loc.Claim)
	}
	if named, ok := r.sessions[loc.Claim]; ok && named.incarnation == loc.Incarnation {
		delete(r.sessions, loc.Claim)
	}
}

// recorded is the watch's incarnation for the claim.
func (r *Runtime) recorded(token claim.Token) (runtime.Locator, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	loc, ok := r.watch[token]
	return loc, ok
}

// launchedSession is the session a Claude Code launch started or resumed under, and the
// incarnation that launch started as.
type launchedSession struct {
	incarnation, session string
}

// nameSession records the session l's Claude Code launch, starting as loc, runs under: the id
// prepare chose (claudeInputs), which its `--session-id` or `--resume` names. Only a deployment
// that names exchange_audience registers a role's session, so only it keeps the record; an Oh My
// Pi launch names nothing, since its agent mints its id, and leaves no record for the claim. The
// record goes when the process is let go, with the watch's (forget, forgetIf).
func (r *Runtime) nameSession(loc runtime.Locator, l launch) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !l.runsClaude() || r.agentSecrets == nil || r.agentSecrets.ExchangeAudience == "" {
		delete(r.sessions, loc.Claim)
		return
	}
	r.sessions[loc.Claim] = launchedSession{incarnation: loc.Incarnation, session: l.claude.session}
}

// LaunchSession is runtime.LaunchSessionNamer: the session the claim's latest Claude Code launch
// started or resumed under, while loc is the incarnation that launch started as.
func (r *Runtime) LaunchSession(loc runtime.Locator) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	named, ok := r.sessions[loc.Claim]
	if !ok || named.incarnation != loc.Incarnation {
		return "", false
	}
	return named.session, true
}

// Observe reports on every watched claim: each one at once, again whenever its Sandbox or pod
// changes, and every claim again each probe interval. Every observation carries the recorded
// locator, whatever pod it saw, so the supervisor's incarnation fence holds (B3). Gone and
// NotRecordedProcess are final for an incarnation, so once one is delivered the claim leaves the
// watch; Alive and Uncertain stay. The recorded incarnation is read again after each evaluation,
// and a verdict whose locator stopped being the claim's recorded one meanwhile — Suspend, Release,
// or a relaunch took it — is dropped. One taken between that read and the send still reaches the
// consumer; it carries an incarnation the claim no longer holds, which the supervisor's
// incarnation fence drops (supervise/machine.go, fence). Delivery blocks on the consumer: nothing
// else is dropped, and a claim changed many times while the consumer was busy is evaluated once,
// against the latest stores. The channel closes when ctx ends. One Observe runs at a time.
func (r *Runtime) Observe(ctx context.Context) (<-chan runtime.Observation, error) {
	r.mu.Lock()
	if r.observer != nil {
		r.mu.Unlock()
		return nil, errors.New("sandbox runtime: already observed; one Observe runs at a time")
	}
	o := &observer{dirty: map[claim.Token]bool{}, wake: make(chan struct{}, 1)}
	for token := range r.watch {
		o.mark(token)
	}
	r.observer = o
	r.mu.Unlock()

	out := make(chan runtime.Observation)
	go func() {
		defer close(out)
		defer func() {
			r.mu.Lock()
			r.observer = nil
			r.mu.Unlock()
		}()
		tick := time.NewTicker(r.probeInterval)
		defer tick.Stop()
		for {
			for {
				loc, ok := r.nextDirty(o)
				if !ok {
					break
				}
				obs := r.evaluate(ctx, loc)
				if current, ok := r.recorded(loc.Claim); !ok || current.Incarnation != loc.Incarnation {
					continue
				}
				select {
				case <-ctx.Done():
					return
				case out <- obs:
				}
				if obs.Kind == runtime.Gone || obs.Kind == runtime.NotRecordedProcess {
					r.forgetIf(loc)
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-o.wake:
			case <-tick.C:
				r.mu.Lock()
				for token := range r.watch {
					o.mark(token)
				}
				r.mu.Unlock()
			}
		}
	}()
	return out, nil
}

// nextDirty takes the first queued claim still in the watch, with its recorded locator.
func (r *Runtime) nextDirty(o *observer) (runtime.Locator, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for len(o.dirty) > 0 {
		tokens := make([]claim.Token, 0, len(o.dirty))
		for token := range o.dirty {
			tokens = append(tokens, token)
		}
		token := slices.Min(tokens)
		delete(o.dirty, token)
		if loc, ok := r.watch[token]; ok {
			return loc, true
		}
	}
	return runtime.Locator{}, false
}
