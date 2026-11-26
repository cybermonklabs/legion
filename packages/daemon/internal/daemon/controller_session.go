package daemon

import (
	"context"

	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// mind is the keeper's look at a controller that runs: ready, working or idle. A working claim's
// turn open longer than turnBound is ended (TurnPastBound, dispatch://INFRA-2045 R2), through the
// machine's own row for it; the rule is off at 0. The keeper stops no live controller for any
// other reason: a session older than `controller_session_max_age_hours` is replaced inside the
// next relaunch the daemon makes anyway (supervise's replaceAgedSession, R3), never by the keeper.
func (k *controllerKeeper) mind(ctx context.Context, m *supervise.Machine) error {
	c := m.Claim()
	if c.State != supervise.StateWorking || k.turnBound <= 0 {
		return nil
	}
	// The machine is asked only about a turn the claim's times show open (Claim.OpenTurn): a claim
	// working for a compaction with no turn under it has its turn closed (compactStart), and is
	// asked nothing at each look of the compaction. Whether the turn is the one read here is the
	// machine's to say (supervise's Machine.endTurnPastBound).
	start, open := c.OpenTurn()
	if !open || !start.Add(k.turnBound).Before(k.now()) {
		return nil
	}
	return m.Handle(ctx, supervise.TurnPastBound{Claim: c.Token, Bound: k.turnBound, TurnStart: start})
}
