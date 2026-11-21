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

// forget drops the claim from the watch, whatever incarnation it held, and the session and the
// Dispatch token its last launch named with it.
func (r *Runtime) forget(token claim.Token) {
	r.mu.Lock()
	delete(r.watch, token)
	delete(r.sessions, token)
	live, moved := r.keepDispatchToken(token, "", false)
	r.mu.Unlock()
	r.logLiveDispatchTokens(live, moved)
}

// forgetIf drops loc from the watch only while it is still the claim's recorded incarnation, so a
// stale locator never drops a newer relaunch's; the session a launch named and the Dispatch token
// its generation carried go with that launch's incarnation in the same way.
func (r *Runtime) forgetIf(loc runtime.Locator) {
	r.mu.Lock()
	if current, ok := r.watch[loc.Claim]; ok && current.Incarnation == loc.Incarnation {
		delete(r.watch, loc.Claim)
	}
	if named, ok := r.sessions[loc.Claim]; ok && named.incarnation == loc.Incarnation {
		delete(r.sessions, loc.Claim)
	}
	live, moved := len(r.dispatchTokenClaims), false
	if held, ok := r.dispatchTokenClaims[loc.Claim]; ok && held == loc.Incarnation {
		live, moved = r.keepDispatchToken(loc.Claim, "", false)
	}
	r.mu.Unlock()
	r.logLiveDispatchTokens(live, moved)
}

// recorded is the watch's incarnation for the claim.
func (r *Runtime) recorded(token claim.Token) (runtime.Locator, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	loc, ok := r.watch[token]
	return loc, ok
}

// launchedSession is the session a launch the runtime chose the session for started or resumed
// under, and the incarnation that launch started as.
type launchedSession struct {
	incarnation, session string
}

// nameSession records the session l's launch, starting as loc, runs under where prepare chose it:
// a Claude Code launch's (claudeInputs), and an Oh My Pi launch's that trades an exchange identity
// (ompInputs), which its `--session-id <id>`, `--resume <id>` or `--resume=<id>` names. An Oh My
// Pi launch that trades nothing — a kept pod built before the exchange identity — has no chosen id,
// since relaunch cleared it (l.omp is zero): its agent mints one and reports it at its
// registration, and it leaves no record for the claim. The record goes when the process is let
// go, with the watch's (forget, forgetIf).
func (r *Runtime) nameSession(loc runtime.Locator, l launch) {
	r.mu.Lock()
	defer r.mu.Unlock()
	session := l.omp.session
	if l.runsClaude() {
		session = l.claude.session
	}
	if session == "" {
		delete(r.sessions, loc.Claim)
		return
	}
	r.sessions[loc.Claim] = launchedSession{incarnation: loc.Incarnation, session: session}
}

// LaunchSession is runtime.LaunchSessionNamer: the session the claim's latest launch started or
// resumed under where the runtime chose it (nameSession), while loc is the incarnation that launch
// started as.
func (r *Runtime) LaunchSession(loc runtime.Locator) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	named, ok := r.sessions[loc.Claim]
	if !ok || named.incarnation != loc.Incarnation {
		return "", false
	}
	return named.session, true
}

// DispatchTokens answers the two counts a deployment under dispatch_token_broker reads before it
// removes dispatch_token_transition_file (DispatchTokens): the launches since this runtime was
// built that carried the transition token, with the time of the last, and the claims whose live
// generation started with a Dispatch token (dispatchTokenClaims). Without the setting nothing is
// counted, and nothing reads them (the daemon's dispatchTokensOf).
func (r *Runtime) DispatchTokens() DispatchTokens {
	r.mu.Lock()
	defer r.mu.Unlock()
	return DispatchTokens{TransitionLaunches: r.transitionLaunches, LastTransitionLaunch: r.lastTransitionLaunch, LiveLaunches: len(r.dispatchTokenClaims)}
}

// launchedDispatchToken keeps, under dispatch_token_broker, once the start of a launch starting as
// loc is sent, whether that start carried DISPATCH_TOKEN (carried): the transition token
// dispatchCredential gave a launch that keeps a pod with no exchange identity, the one token a
// launch carries under the setting. Such a launch is counted with the time its start was sent,
// and logged naming the claim, the pod and both counts: those are the launches a deployment waits
// out before it removes dispatch_token_transition_file.
func (r *Runtime) launchedDispatchToken(loc runtime.Locator, carried bool) {
	if !r.dispatch.Broker {
		return
	}
	r.mu.Lock()
	live, moved := r.keepDispatchToken(loc.Claim, loc.Incarnation, carried)
	if carried {
		r.transitionLaunches++
		r.lastTransitionLaunch = r.now()
	}
	launches := r.transitionLaunches
	r.mu.Unlock()
	if carried {
		r.log.Warn("sandbox: a launch into a pod with no exchange identity carries the transition Dispatch token",
			"claim", loc.Claim, "pod", loc.Sandbox.Name, "transitionLaunches", launches, "liveLaunchesWithDispatchToken", live)
	}
	r.logLiveDispatchTokens(live, moved)
}

// observeDispatchToken keeps, under dispatch_token_broker, whether loc's generation, which an
// observation saw running, carried DISPATCH_TOKEN, as the address record its start wrote says
// (evaluate, recordedDispatchToken), while loc is still the claim's recorded incarnation: an
// observation of a locator a relaunch or a release has since let go writes nothing, so a process
// the watch no longer holds is never counted again. It is what counts, after a restart, the
// generations an earlier daemon started.
func (r *Runtime) observeDispatchToken(loc runtime.Locator, carried bool) {
	if !r.dispatch.Broker {
		return
	}
	r.mu.Lock()
	if current, ok := r.watch[loc.Claim]; !ok || current.Incarnation != loc.Incarnation {
		r.mu.Unlock()
		return
	}
	live, moved := r.keepDispatchToken(loc.Claim, loc.Incarnation, carried)
	r.mu.Unlock()
	r.logLiveDispatchTokens(live, moved)
}

// keepDispatchToken records, by claim, whether its live generation, starting as incarnation,
// carried DISPATCH_TOKEN (dispatchTokenClaims), and answers how many claims' generations do and
// whether that number moved. Without dispatch_token_broker it keeps nothing and nothing moves:
// every launch then carries the shared bearer, and the number says nothing a deployment acts on.
// Called with r.mu held.
func (r *Runtime) keepDispatchToken(token claim.Token, incarnation string, carried bool) (live int, moved bool) {
	if !r.dispatch.Broker {
		return 0, false
	}
	before := len(r.dispatchTokenClaims)
	if carried {
		r.dispatchTokenClaims[token] = incarnation
	} else {
		delete(r.dispatchTokenClaims, token)
	}
	return len(r.dispatchTokenClaims), len(r.dispatchTokenClaims) != before
}

// logLiveDispatchTokens writes the number of live launches carrying a Dispatch token each time it
// moved, which it does under dispatch_token_broker alone (keepDispatchToken): there it is the
// number a deployment waits to see at zero before it removes dispatch_token_transition_file. It is
// called once r.mu is let go, so no log write holds the runtime's lock.
func (r *Runtime) logLiveDispatchTokens(live int, moved bool) {
	if moved {
		r.log.Info("sandbox: live launches carrying a Dispatch token", "count", live)
	}
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
