package natscallout

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"

	"github.com/sjawhar/envoy/internal/bus/signedrecord"
	"github.com/sjawhar/envoy/internal/cipher/natshold"
	"github.com/sjawhar/envoy/natsgrant"
)

// HoldView is the auth service's view of the NATS writer hold: the hold row in the broker's
// Postgres, which its read reads (natshold.Read on a pool of its own), and which decides the
// operator's grant (natsgrant.Operator of the row's phase) and, while it cannot be read, refuses
// every other client (decide.go). Deps.Hold holds it, and nil there is a broker whose
// BROKER_NATS_HOLD_READ is false.
//
// It reads the row at every State, and caches nothing. A reading it cannot trust is unknown at
// once, whatever it read before: a callout replica that kept prepare across a failed read while the
// operator wrote held would admit a writer after the hold's census, since the server installs a
// connection's permissions at its authorization and never asks again. So no row, a row that does
// not verify against its keys (signedrecord.Verify over signedrecord.HoldRow), a phase outside the
// row's three, a verified gen below the highest this view has read (its floor), a verified row at
// the floor's gen other than the one read there, and any read error (Postgres unreachable, a
// timeout, a failed transaction) are each unknown. The floor keeps a database writer who is not the
// operator from putting an older signed released back under a running broker, and from putting a
// second signed row at the gen read: a writer who puts the hold's older prepare row back has the
// operator's next move signed at the gen the older row names, so a released row at the held row's
// gen verifies too. A broker that starts again starts with no floor, which is the residual the
// hold's design states. Two decisions that read across the operator's write can meet the floor in
// the wrong order, the older reading after the newer one; that reading is unknown and its client is
// refused and retries, which fails closed.
//
// One view serves every decision made through the Deps that holds it, and its floor is the
// process's one floor of the row, which the broker's /v1/nats-hold routes share
// (api.NATSHold.RowFloor): cmd/cipherd makes one floor and gives it to both, so neither half of the
// process trusts a row the other has moved past, and a row the routes wrote moves the view's floor
// too. Every method is safe for concurrent use.
type HoldView struct {
	read    func(ctx context.Context) (natshold.Reading, bool, error)
	keys    []ed25519.PublicKey
	floor   *signedrecord.Floor
	metrics *Metrics
}

// NewHoldView is the view over read and keys, BROKER_NATS_HOLD_SIGNING_KEYS, with floor the
// process's floor of the row (nil gives the view one of its own), setting
// broker_nats_callout_hold_unknown on metrics (nil for none). With read nil it is nil, the setting
// off, which reads nothing and answers released, known.
func NewHoldView(read func(ctx context.Context) (natshold.Reading, bool, error), keys []ed25519.PublicKey, floor *signedrecord.Floor, metrics *Metrics) *HoldView {
	if read == nil {
		return nil
	}
	if floor == nil {
		floor = &signedrecord.Floor{}
	}
	return &HoldView{read: read, keys: keys, floor: floor, metrics: metrics}
}

// errNoHoldRow is the reading of a table that holds no hold row: the operator has not written the
// first signed row yet, or a database writer deleted it.
var errNoHoldRow = errors.New("natscallout: the writer hold has no row")

// State is the hold row's phase, read now, and whether it is known: false, with no phase, for every
// reading phase refuses.
func (v *HoldView) State(ctx context.Context) (natsgrant.OperatorPhase, bool) {
	phase, err := v.phase(ctx)
	return phase, err == nil
}

// phase is the hold row's phase, read now, or why the reading is unknown. The nil view, the
// setting off, answers released and reads nothing. Every reading of a view sets
// broker_nats_callout_hold_unknown.
func (v *HoldView) phase(ctx context.Context) (natsgrant.OperatorPhase, error) {
	if v == nil {
		return natsgrant.OperatorPhaseReleased, nil
	}
	phase, err := v.verified(ctx)
	v.metrics.holdReading(err != nil)
	if err != nil {
		return "", err
	}
	return phase, nil
}

// verified reads the row once and answers its phase once every check passes. The floor moves only
// for a row whose phase is one of the three and whose signature verifies, so a row no reader can
// trust moves nothing.
func (v *HoldView) verified(ctx context.Context) (natsgrant.OperatorPhase, error) {
	row, found, err := v.read(ctx)
	switch {
	case err != nil:
		return "", err
	case !found:
		return "", errNoHoldRow
	}
	phase := natsgrant.OperatorPhase(row.Phase)
	switch phase {
	case natsgrant.OperatorPhaseReleased, natsgrant.OperatorPhasePrepare, natsgrant.OperatorPhaseHeld:
	default:
		return "", errors.New("natscallout: the writer hold's row holds a phase none of the three")
	}
	lines := signedrecord.HoldRow(row.Gen, row.Phase, row.HoldID, row.SignedAt)
	if err := signedrecord.Verify(v.keys, row.Sig, lines); err != nil {
		return "", fmt.Errorf("natscallout: the writer hold's row at gen %d: %w", row.Gen, err)
	}
	if err := v.floor.Accept(row.Gen, row.SignedAt, lines); err != nil {
		return "", fmt.Errorf("natscallout: the writer hold's row: %w", err)
	}
	return phase, nil
}
