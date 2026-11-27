package admit

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/signals"
)

// UseSignals registers read, which answers the signals this pass admits below P0 by at the moment
// of the pass (R11 and R12 of the tool's spec): the snapshot of each signal's reading and limit
// and, for the back-off, the history of the memory guard's kills (signals.Snapshot), and whether
// admission by signals is on. Each pass asks it once, before it judges the line (promoteHolds), so
// a change is in force from the next pass on. The snapshot's At is the moment of the pass. read
// reads through the pass's own transaction tx, so it sees what the fact that started the pass
// wrote, and an error it answers ends the pass with that error, which rolls the fact back.
//
// Where it answers false, or no function is registered, which is the daemon's own until a person
// turns admission by signals on, the pass admits below P0 under the cap as it always did, whatever
// rule it judges by. Where it answers true, the pass decides each entry below P0 that takes a slot
// of the cap by the rule's own half (record.SlotRule.SignalsLet): the rule's mode, or the
// snapshot's where the rule names none, so the daemon's own rule admits by signals once a caller
// supplies a snapshot that says so. R10's start of a P0 issue outside the cap is decided by the
// P0 bound alone, and no pass takes a slot from a holder because a signal is over its limit. It is
// a setter rather than a parameter of New for the reason UseExperiments is.
func (a *Admission) UseSignals(read func(ctx context.Context, tx pgx.Tx, at time.Time) (signals.Snapshot, bool, error)) {
	a.signalRead = read
}

// passGate is what one admission pass reads once, before it judges the line, and every judgement
// of the pass shares: the P0 bound in force (p0BoundAt), the kind of the fact whose pass it is,
// and the signals it admits below P0 by (UseSignals) with whether admission by signals is on.
type passGate struct {
	p0Bound   *int
	kind      signals.PassKind
	bySignals bool
	snapshot  signals.Snapshot
}

// gateFor reads the pass's gate for a pass of kind, at the admission's clock: the P0 bound and the
// signals, each through the pass's transaction tx.
func (a *Admission) gateFor(ctx context.Context, tx pgx.Tx, kind signals.PassKind) (passGate, error) {
	bound, err := a.p0BoundAt(ctx, tx)
	if err != nil {
		return passGate{}, err
	}
	gate := passGate{p0Bound: bound, kind: kind}
	if a.signalRead == nil {
		return gate, nil
	}
	at := a.now()
	snapshot, bySignals, err := a.signalRead(ctx, tx, at)
	if err != nil {
		return passGate{}, fmt.Errorf("read the signals of the %s pass at %s: %w", kind, at.Format(time.RFC3339), err)
	}
	gate.snapshot, gate.bySignals = snapshot, bySignals
	gate.snapshot.At = at
	return gate, nil
}

// asEvent is the gate as the pass of an event judges it, whatever kind of pass reads it: the stall
// measure asks whether the line could move, which a tick's own bar on a start below P0 must not
// answer for it.
func (g passGate) asEvent() passGate {
	g.kind = signals.PassEvent
	return g
}
