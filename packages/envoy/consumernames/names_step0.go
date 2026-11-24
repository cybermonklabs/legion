// Step 0, first in every call: settling a logical name's open operation (settleOp for a call that
// creates, abandon for one that creates nothing), and the helpers Step 0 shares with Replace and
// Delete.

package consumernames

import (
	"context"
	"errors"
	"fmt"

	"github.com/nats-io/nats.go/jetstream"
)

// settleOp is Step 0: it settles sel's open operation. waited reports, for a candidate the server
// does not hold, whether this call has waited the settle age for it itself. A replace whose
// candidate is already the current is a published replace of the fixed name whose delete has not
// been confirmed: its delete is retried first, before anything else (finishFixedDelete). A replace
// reserved with its start pending (Op.StartPending) or with no start at all has destroyed nothing,
// so its candidate is retired, a fresh one reserved, and the start read from the predecessor then;
// a call that creates nothing closes it instead (abandon), keeping the record's saved start.
func (n *Names) settleOp(ctx context.Context, sel Selection, create CreateFunc, waited func(string) bool) (Selection, error) {
	op := sel.Record.Op
	if op.Kind == OpReplace && op.Candidate != "" && op.Candidate == sel.Record.Current && op.Old == n.fixed(sel.Logical) {
		return n.finishFixedDelete(ctx, sel)
	}
	if op.Kind == OpDelete {
		return n.settleDelete(ctx, sel)
	}
	if op.Kind == OpReplace && (op.StartPending || sel.Record.Start == 0) {
		if create == nil {
			return n.abandon(ctx, sel)
		}
		next, err := n.rereserve(ctx, sel, "")
		if err != nil {
			return next, err
		}
		if next, err = n.saveReplaceStart(ctx, next); err != nil {
			return next, err
		}
		return n.launch(ctx, next, create, OperationRecover)
	}
	info, err := n.info(ctx, sel.Logical, op.Candidate)
	if err == nil {
		return n.adopt(ctx, sel, info, OperationRecover)
	}
	if !errors.Is(err, ErrNotFound) {
		return sel, stepError(sel.Logical, OperationRecover, op.Candidate, err)
	}
	if n.cfg.Now().Sub(op.ReservedAt) < n.cfg.SettleAge && !waited(op.Candidate) {
		return sel, errSettleLater
	}
	if create == nil {
		return n.abandon(ctx, sel)
	}
	next, err := n.rereserve(ctx, sel, retryNative(sel))
	if err != nil {
		return next, err
	}
	n.observe(OperationRecover, OutcomeRetired)
	return n.launch(ctx, next, create, OperationRecover)
}

// abandon settles sel's open create or replace for a call that creates nothing (a delete, a
// cleanup, a read): its candidate is retired and the operation cleared, so the current stays what
// it was and the saved start stands for the next create. A replace of the key's fixed name (a
// convert's) is closed only once its fixed name is gone, so no fixed name is left on the server
// with no operation naming it: with a saved start the fixed name is deleted first (deleteOwned), a
// delete that fails being the call's error with the replace kept open; with no saved start the
// fixed consumer's floor is the only record of the position, so the replace is left open, as it
// was, for a call that creates.
func (n *Names) abandon(ctx context.Context, sel Selection) (Selection, error) {
	op := sel.Record.Op
	if op.Kind == OpReplace && op.Old == n.fixed(sel.Logical) {
		if sel.Record.Start == 0 {
			return sel, nil
		}
		if err := n.deleteOwned(ctx, sel.Logical, op.Old); err != nil {
			n.observe(OperationCleanup, OutcomeError)
			return sel, stepError(sel.Logical, OperationRecover, op.Old, err)
		}
	}
	record := n.abandonCandidate(sel.Record.clone(), *op)
	record.Op = nil
	next, err := n.write(ctx, sel, record)
	if err != nil {
		return next, err
	}
	n.observe(OperationRecover, OutcomeRetired)
	return next, nil
}

// saveReplaceStart writes the start of a replace reserved with no start: the sequence after the
// predecessor's floor, which ends its pending start (Op.StartPending). A predecessor the server
// does not hold leaves the operation open with an error.
func (n *Names) saveReplaceStart(ctx context.Context, sel Selection) (Selection, error) {
	op := sel.Record.Op
	info, err := n.info(ctx, sel.Logical, op.Old)
	if errors.Is(err, ErrNotFound) {
		return sel, stepError(sel.Logical, OperationReplace, op.Old, fmt.Errorf("the replace reserved no start and its predecessor is absent: %w", err))
	}
	if err != nil {
		return sel, stepError(sel.Logical, OperationReplace, op.Old, err)
	}
	record := sel.Record.clone()
	record.Start = n.cfg.Floor(info) + 1
	record.Op.StartPending = false
	return n.write(ctx, sel, record)
}

// finishFixedDelete ends sel's published replace of the fixed name: it deletes that name, the
// operation's old one, and once the server confirms the delete (or holds no such consumer)
// clears the operation. A delete that fails is the call's error, naming the name, and leaves the
// operation open, so the next call's Step 0 deletes it again before anything else.
func (n *Names) finishFixedDelete(ctx context.Context, sel Selection) (Selection, error) {
	old := sel.Record.Op.Old
	if err := n.deleteOwned(ctx, sel.Logical, old); err != nil {
		n.observe(OperationCleanup, OutcomeError)
		return sel, stepError(sel.Logical, OperationRecover, old, err)
	}
	record := sel.Record.clone()
	record.Op = nil
	return n.write(ctx, sel, record)
}

// settleDelete finishes an open delete: it records the predecessor's floor unless the delete
// recorded it, deletes the predecessor and clears the current.
func (n *Names) settleDelete(ctx context.Context, sel Selection) (Selection, error) {
	op := sel.Record.Op
	if !op.FloorRecorded {
		next, err := n.recordDeleteFloor(ctx, sel, nil)
		if err != nil {
			return next, err
		}
		sel = next
	}
	if err := n.deleteOwned(ctx, sel.Logical, op.Old); err != nil {
		return sel, stepError(sel.Logical, OperationDelete, op.Old, err)
	}
	next, err := n.clearDeleted(ctx, sel)
	n.observeErr(OperationRecover, err)
	return next, err
}

// recordDeleteFloor reads the floor of the open delete's predecessor (from info when the caller
// holds it), records it (Config.RecordFloor) and writes the start after it with FloorRecorded, in
// one conditional write. A predecessor the server does not hold has no floor to read: the start
// is then the sequence after the record's last floor of it on this stream, else the saved start,
// and never a floor of 0. Any other failed read stops the call.
func (n *Names) recordDeleteFloor(ctx context.Context, sel Selection, info *jetstream.ConsumerInfo) (Selection, error) {
	op := sel.Record.Op
	record := sel.Record.clone()
	if info == nil {
		read, err := n.info(ctx, sel.Logical, op.Old)
		switch {
		case err == nil:
			info = read
		case errors.Is(err, ErrNotFound):
		default:
			return sel, stepError(sel.Logical, OperationDelete, op.Old, err)
		}
	}
	if info != nil {
		floor, err := n.recordFloor(ctx, sel.Logical, n.cfg.Floor(info))
		if err != nil {
			return sel, stepError(sel.Logical, OperationDelete, op.Old, err)
		}
		record.Start = floor + 1
	} else if start, ok, err := n.lastFloorStart(ctx, record, op.Old); err != nil {
		return sel, err
	} else if ok {
		record.Start = start
	}
	record.Op.FloorRecorded = true
	return n.write(ctx, sel, record)
}

func (n *Names) recordFloor(ctx context.Context, logical string, floor uint64) (uint64, error) {
	if n.cfg.RecordFloor == nil {
		return floor, nil
	}
	return n.cfg.RecordFloor(ctx, logical, floor)
}

// clearDeleted clears the current and the operation of a delete whose predecessor is gone; the
// record and its start stay, so the next create makes a fresh name at that start.
func (n *Names) clearDeleted(ctx context.Context, sel Selection) (Selection, error) {
	record := sel.Record.clone()
	if record.Current == record.Op.Old {
		record.Current = ""
	}
	record.Op, record.LastFloor = nil, nil
	next, err := n.write(ctx, sel, record)
	next.Info = nil
	return next, err
}
