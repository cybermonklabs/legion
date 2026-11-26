package daemon

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// answerAppendWait is the longest a claim's start waits for an answer being added to its
// conversation before it starts anyway.
const answerAppendWait = 10 * time.Second

// answerGate keeps the answer loop and a claim's start off one conversation at once (plan 2.8, "One
// writer"). It wraps the store's CheckLaunch, the check every start makes first (launchAfter), as
// launchgate.go does, with state of its own per claim token and never the machine's lock. Appends on
// one claim token take turns, whatever their session (the loop's per-session workers and its
// hand-off consumer append at once): each waits until no other append on the token writes, then
// writes only after checking that the claim has not been started and, from the machine's View (read
// without the machine's lock), does not run the conversation. A start waits until no append on its
// token writes, at most answerAppendWait, then marks the claim started before it asks the store
// underneath. It never calls Machine.Claim, which waits out a decision in flight.
type answerGate struct {
	supervise.Store
	// view is the claim's machine's View, false when the daemon supervises no such claim.
	view func(claim.Token) (supervise.ClaimView, bool)
	wait time.Duration
	log  *slog.Logger

	mu sync.Mutex
	// changed is closed, and replaced, whenever an append ends: a start or an append waiting on it
	// looks again.
	changed chan struct{}
	tokens  map[claim.Token]*answerGateState
}

// answerGateState is one claim's: whether an append to any of its conversations writes now (one at
// a time, Append), and whether it was started (since a launch at generation gen passed its check).
type answerGateState struct {
	appending bool
	started   bool
	gen       uint64
}

func newAnswerGate(store supervise.Store, view func(claim.Token) (supervise.ClaimView, bool), log *slog.Logger) *answerGate {
	return &answerGate{Store: store, view: view, wait: answerAppendWait, log: log, changed: make(chan struct{}), tokens: map[claim.Token]*answerGateState{}}
}

func (g *answerGate) state(token claim.Token) *answerGateState {
	st, ok := g.tokens[token]
	if !ok {
		st = &answerGateState{}
		g.tokens[token] = st
	}
	return st
}

// CheckLaunch is a start's check: it waits until no append to the claim's conversations writes, at
// most the gate's wait, marks the claim started, then asks the store underneath. A start the store
// refuses is not started.
func (g *answerGate) CheckLaunch(ctx context.Context, c supervise.Claim) error {
	timer := time.NewTimer(g.wait)
	defer timer.Stop()
	g.mu.Lock()
	st := g.state(c.Token)
wait:
	for st.appending {
		changed := g.changed
		g.mu.Unlock()
		select {
		case <-changed:
			g.mu.Lock()
		case <-timer.C:
			g.log.Warn("answers: a claim starts while an answer is still being added to its conversation", "claim", c.Token, "waited", g.wait)
			g.mu.Lock()
			break wait
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	st.started, st.gen = true, c.Generation
	g.mu.Unlock()
	if err := g.Store.CheckLaunch(ctx, c); err != nil {
		g.mu.Lock()
		if st.gen == c.Generation {
			st.started = false
		}
		g.mu.Unlock()
		return err
	}
	return nil
}

// Running is whether session's conversation is its claim's current one and the claim runs it, is
// starting, or was started (answerloop.Gate).
func (g *answerGate) Running(token claim.Token, session string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.runningLocked(token, session)
}

// Released is whether session's conversation is its claim's current one and the daemon released
// the claim (supervise.Claim.Released) and no start of it has passed its check since
// (answerloop.Gate): the loop answers the agent's questions as any stopped agent's and gives it
// everything else through its wake (answerloop.Wakes).
func (g *answerGate) Released(token claim.Token, session string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	view, ok := g.view(token)
	if !ok || view.Session == "" || view.Session != session {
		return false
	}
	return !g.runsLocked(token, view) && view.Released()
}

// HeldForCyberPolicy is whether token's claim is held for OpenAI's cyber-policy refusal of its
// session's content, as the machine's view says (supervise.Claim.HeldForCyberPolicy), whatever
// conversation of the claim the loop answers from (answerloop.Gate): the loop calls no model for
// it, since no action the daemon starts by itself may send that content to OpenAI again. The view
// is the whole of the claim's record here: every claim the store holds has a machine before the
// loop runs (the supervisor's restore at boot, Create after it, and serve starting the loop only
// once supervision started), and the supervisor removes none, so a token no machine holds is one
// the store holds no claim of either.
func (g *answerGate) HeldForCyberPolicy(token claim.Token) bool {
	view, ok := g.view(token)
	return ok && view.HeldForCyberPolicy()
}

// runningLocked is Running under g.mu, of the claim's view read once (runsLocked).
func (g *answerGate) runningLocked(token claim.Token, session string) bool {
	view, ok := g.view(token)
	if !ok || view.Session == "" || view.Session != session {
		return false
	}
	return g.runsLocked(token, view)
}

// runsLocked is whether token's claim, view its view, whose session is the one asked of, runs the
// conversation, is starting, or was started, under g.mu. A start's mark lasts until the machine
// shows a launch after it (a later generation) that runs nothing any more: the process the start
// began has ended, and the mark is cleared.
//
// The loop answers for every claim that runs nothing: one that failed, one the workflow or an
// operator suspended (a parked child, or a tree whose root left the workflow during its linger),
// one retired, one closed, and one the daemon released (supervise.Claim.Released): an idle
// release of its issue pod, which keeps the session, its phase and its pending task for the wake
// that brings the pod back (INFRA-945), or a project's pause, which suspends a claim the same way.
// A failed or retired claim held for OpenAI's cyber-policy refusal is answered by no model
// (HeldForCyberPolicy): the loop passes its questions to the controller.
// The listener hands a released agent's message to the daemon and raises no wake of its own when
// it did, so the loop answers the agent's questions from its conversation and gives it everything
// else through one outbox row that wakes it (answerloop.Wakes, the daemon's releasedWakes). The
// gate still never lets an append write while the claim runs or is starting: a start's mark and a
// launch that runs the conversation each answer true, and a start waits for an append to end.
func (g *answerGate) runsLocked(token claim.Token, view supervise.ClaimView) bool {
	if st := g.tokens[token]; st != nil && st.started {
		if view.Generation <= st.gen || !view.RunsNothing() {
			return true
		}
		st.started = false
	}
	return !view.RunsNothing()
}

// Append runs write once no other append on the claim writes, and while no start of the claim can
// pass its check, unless the claim runs the conversation or was started: then it writes nothing and
// answers false (answerloop.Gate). Appends on one claim token take turns whatever their session:
// each waits for the one writing to end and only then checks the claim, since a start may have
// passed meanwhile. A conversation that is not its claim's current one is written whatever the
// claim does, since no start resumes it, but it takes its turn with the claim's other appends.
func (g *answerGate) Append(token claim.Token, session string, write func() error) (bool, error) {
	g.mu.Lock()
	st := g.state(token)
	for st.appending {
		changed := g.changed
		g.mu.Unlock()
		<-changed
		g.mu.Lock()
	}
	if g.runningLocked(token, session) {
		g.mu.Unlock()
		return false, nil
	}
	st.appending = true
	g.mu.Unlock()
	defer func() {
		g.mu.Lock()
		st.appending = false
		close(g.changed)
		g.changed = make(chan struct{})
		g.mu.Unlock()
	}()
	return true, write()
}
