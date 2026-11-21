package consumernames

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// Operation names a step of the protocol, for the counters' callback (Config.Observe).
type Operation string

const (
	// OperationSelect is the read that admits a call: its refusals are counted.
	OperationSelect Operation = "select"
	// OperationCreate is Ensure's create of a logical name with no current.
	OperationCreate Operation = "create"
	// OperationReplace is Replace.
	OperationReplace Operation = "replace"
	// OperationDelete is Delete.
	OperationDelete Operation = "delete"
	// OperationRecover is Step 0's settlement of an operation another call left open.
	OperationRecover Operation = "recover"
	// OperationMissingCurrent is the create of a current the server no longer holds.
	OperationMissingCurrent Operation = "missing_current"
	// OperationLastFloor is RecordLastFloor.
	OperationLastFloor Operation = "last_floor"
	// OperationCleanup is the delete of a retired name.
	OperationCleanup Operation = "cleanup"
)

// Outcome is how a step ended.
type Outcome string

const (
	OutcomeOK Outcome = "ok"
	// OutcomeAdopted is a candidate whose create's answer was lost, found by an info and
	// published.
	OutcomeAdopted Outcome = "adopted"
	// OutcomeRetired is an open operation whose candidate never landed, retired.
	OutcomeRetired Outcome = "retired"
	// OutcomeRefused is a call refused before anything changed: the cutover, lost selection
	// state, or no safe start.
	OutcomeRefused Outcome = "refused"
	// OutcomeConflict is a conditional write the store refused.
	OutcomeConflict Outcome = "conflict"
	// OutcomeLimit is a create refused at the stream's max_consumers.
	OutcomeLimit Outcome = "limit"
	OutcomeError Outcome = "error"
)

// Config is what Names runs on.
type Config struct {
	Store  Store
	Server Server
	// Prefix is the part of a physical name before its '~' (FreshName): "session" for a session
	// consumer, the logical name itself for a durable.
	Prefix func(logical string) string
	// Floor is the family's ack floor of a consumer's info.
	Floor func(*jetstream.ConsumerInfo) uint64
	// StreamID reads the stream's identity, which a last floor must carry to count.
	StreamID func(ctx context.Context) (string, error)
	// RecordFloor, when set, records a floor Delete read before it deletes (the session
	// family's resume record) and answers the floor it recorded, which the record's start is
	// then set after.
	RecordFloor func(ctx context.Context, logical string, floor uint64) (uint64, error)
	// MissingRecord, when set, decides a key never written: an error refuses the call (Dispatch's
	// two durables, a daemon checkpoint with no selection row); nil makes it a new name.
	MissingRecord func(ctx context.Context, logical string) error
	// Owner names this process in each operation it reserves.
	Owner string
	// Observe, when set, hears each step and how it ended: the counters' callback.
	Observe func(Operation, Outcome)
	// Now is the clock; time.Now when nil.
	Now func() time.Time
	// SettleAge is how old an open operation whose candidate the server does not hold must be
	// before another call retires that candidate: 2 s when 0.
	SettleAge time.Duration
	// Poll is how often a call waiting on such an operation reads again: 50 ms when 0.
	Poll time.Duration
	// CheckKey refuses a logical name before anything is written: CheckKey when nil. A family
	// whose keys are made by a rule of its own (DurableKey) passes that rule's check
	// (CheckDurableKey).
	CheckKey func(logical string) error
	// NativeFirstCreate keeps a family's native first create (CreateRequest.First answering a
	// Start with Native set) for a logical name with no record, whatever the cutover boundary: the
	// daemon's durables keep today's deliver policy new. A saved start and replay_from still win.
	// Such a first create may answer Seq 0, so its rule reads no stream; the publish then saves the
	// start the created consumer reports (its delivered stream sequence plus one), and a candidate
	// that never landed is created natively again. Off, the default and the session family's, a
	// native first create starts by start sequence at the boundary when one is set.
	NativeFirstCreate bool
	// NoDelete says the family's Server cannot delete: at the stream's max_consumers a create is
	// answered ErrConsumerLimit at once, with none of the retired names read or deleted to free a
	// slot. Such a family calls no Replace, Delete or Cleanup, each of which needs the server's
	// delete: the daemon's durables, whose grant holds none.
	NoDelete bool
	// Reserved, when set, runs after each write that reserves a candidate (a create, a replace,
	// and Step 0's or a retry's fresh candidate), before any server call or create of that
	// operation, with the operation as written. An error ends the call with the operation open
	// and nothing destroyed: Dispatch opens the connection its generations are granted on here.
	Reserved func(ctx context.Context, logical string, op Op) error
}

// StepError is a step of the protocol that failed on one physical name: a create, a delete, or the
// read of a floor. Every error a call returns from such a step is one.
type StepError struct {
	Logical   string
	Operation Operation
	// Physical is the name the step was on.
	Physical string
	Err      error
}

func (e *StepError) Error() string {
	return fmt.Sprintf("consumer names: %s of %s on %s: %v", e.Operation, e.Logical, e.Physical, e.Err)
}

func (e *StepError) Unwrap() error { return e.Err }

func stepError(logical string, operation Operation, physical string, err error) error {
	if err == nil {
		return nil
	}
	var step *StepError
	if errors.As(err, &step) || errors.Is(err, ErrConflict) {
		return err
	}
	return &StepError{Logical: logical, Operation: operation, Physical: physical, Err: err}
}

// reserved runs Config.Reserved on sel's operation.
func (n *Names) reserved(ctx context.Context, sel Selection) error {
	if n.cfg.Reserved == nil {
		return nil
	}
	return n.cfg.Reserved(ctx, sel.Logical, *sel.Record.Op)
}

// Names runs the protocol for one family.
type Names struct {
	cfg Config
}

// New is Names over cfg.
func New(cfg Config) *Names {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.SettleAge <= 0 {
		cfg.SettleAge = 2 * time.Second
	}
	if cfg.Poll <= 0 {
		cfg.Poll = 50 * time.Millisecond
	}
	return &Names{cfg: cfg}
}

// Selection is a logical name's record as a call settled it.
type Selection struct {
	Logical string
	Record  Record
	// Revision is the record's store revision; 0 with Exists unset.
	Revision uint64
	// Exists says the store holds a record for the key.
	Exists bool
	// State is the store's sentinel as the call read it.
	State StoreState
	// Info is Record.Current's info as the call read it; nil when there is no current, when the
	// call read none (Delete), or when the server does not hold it (Missing).
	Info *jetstream.ConsumerInfo
	// Missing says Record.Current names a consumer the server does not hold, on a call that
	// creates nothing.
	Missing bool
	// Created says this call created Record.Current.
	Created bool
}

func (s Selection) with(record Record, revision uint64) Selection {
	s.Record, s.Revision, s.Exists = record, revision, true
	return s
}

// CreateRequest is how Ensure creates a logical name with no current.
type CreateRequest struct {
	Create CreateFunc
	// First is the family's first-create rule, for a key with no record.
	First func(ctx context.Context) (Start, error)
	// Lower, when above 0, is the caller's own start, honoured when it is below the saved one.
	Lower uint64
}

// maxConflicts bounds how often one call reads the record again after a refused write.
const maxConflicts = 16

// errSettleLater says Step 0 met an open operation too young to settle, and the call reads again.
var errSettleLater = errors.New("consumer names: an operation in flight is younger than the settle age")

func (n *Names) checkKey(logical string) error {
	if n.cfg.CheckKey != nil {
		return n.cfg.CheckKey(logical)
	}
	return CheckKey(logical)
}

func (n *Names) observe(op Operation, outcome Outcome) {
	if n.cfg.Observe != nil {
		n.cfg.Observe(op, outcome)
	}
}

func (n *Names) observeErr(op Operation, err error) {
	switch {
	case err == nil:
		n.observe(op, OutcomeOK)
	case errors.Is(err, ErrConflict):
		n.observe(op, OutcomeConflict)
	case errors.Is(err, ErrConsumerLimit):
		n.observe(op, OutcomeLimit)
	case errors.Is(err, ErrCutoverNotRun), IsSelectionStateLost(err), errors.Is(err, ErrNoSafeStart):
		n.observe(op, OutcomeRefused)
	default:
		n.observe(op, OutcomeError)
	}
}

// Select settles logical's record and returns it: it admits the call (Admit, lost selection
// state), runs Step 0 on an open operation, and reads the current consumer. A current the server
// does not hold is created again at the start the record proves safe when create is given
// (missing current), and is marked Missing otherwise; with no safe start the call stops with
// ErrNoSafeStart. Select creates no consumer for a key with no current: Ensure does.
func (n *Names) Select(ctx context.Context, logical string, create CreateFunc) (Selection, error) {
	return n.selectRecord(ctx, logical, create, true)
}

// Resolve is Select for a call that reads no consumer: it admits the call and runs Step 0 without
// creating anything, and leaves Info unread, for a caller that holds the current's info already.
func (n *Names) Resolve(ctx context.Context, logical string) (Selection, error) {
	return n.selectRecord(ctx, logical, nil, false)
}

func (n *Names) selectRecord(ctx context.Context, logical string, create CreateFunc, readCurrent bool) (Selection, error) {
	if err := n.checkKey(logical); err != nil {
		return Selection{}, err
	}
	var waitingSince time.Time
	var waitingFor string
	for conflicts := 0; ; {
		sel, err := n.read(ctx, logical)
		if err != nil {
			return sel, err
		}
		if op := sel.Record.Op; op != nil {
			next, err := n.settleOp(ctx, sel, create, func(candidate string) bool {
				// The wait is measured on this process's clock too, so a reservation stamped by a
				// clock ahead of this one is settled within one settle age of this call seeing it.
				if candidate != waitingFor {
					waitingFor, waitingSince = candidate, n.cfg.Now()
				}
				return n.cfg.Now().Sub(waitingSince) >= n.cfg.SettleAge
			})
			switch {
			case errors.Is(err, errSettleLater):
				if err := sleep(ctx, n.cfg.Poll); err != nil {
					return sel, fmt.Errorf("%w: %s's %s of %s: %w", ErrBusy, logical, op.Kind, op.Candidate, err)
				}
				continue
			case errors.Is(err, ErrConflict):
				if conflicts++; conflicts > maxConflicts {
					return sel, err
				}
				continue
			case err != nil:
				return next, err
			}
			sel = next
		}
		if sel.Record.Current == "" || !readCurrent || sel.Info != nil {
			return sel, nil
		}
		info, err := n.cfg.Server.Info(ctx, sel.Record.Current)
		if err == nil {
			sel.Info = info
			return sel, nil
		}
		if !errors.Is(err, ErrNotFound) {
			return sel, err
		}
		if create == nil {
			sel.Missing = true
			return sel, nil
		}
		start, err := n.safeStart(ctx, sel)
		if err != nil {
			n.observeErr(OperationMissingCurrent, err)
			return sel, err
		}
		created, err := n.reserve(ctx, sel, Start{Seq: start}, create, OperationMissingCurrent)
		if errors.Is(err, ErrConflict) {
			if conflicts++; conflicts > maxConflicts {
				return created, err
			}
			continue
		}
		return created, err
	}
}

// read admits a call and reads logical's record.
func (n *Names) read(ctx context.Context, logical string) (Selection, error) {
	sel := Selection{Logical: logical}
	state, err := n.cfg.Store.State(ctx)
	if err == nil {
		err = Admit(state)
	}
	if err != nil {
		n.observeErr(OperationSelect, err)
		return sel, err
	}
	sel.State = state
	record, revision, err := n.cfg.Store.Read(ctx, logical)
	switch {
	case err == nil:
		return sel.with(record, revision), nil
	case errors.Is(err, ErrNoRecord):
		if n.cfg.MissingRecord != nil {
			if err := n.cfg.MissingRecord(ctx, logical); err != nil {
				n.observe(OperationSelect, OutcomeRefused)
				return sel, err
			}
		}
		return sel, nil
	default:
		n.observeErr(OperationSelect, err)
		return sel, err
	}
}

// write writes record over sel's revision.
func (n *Names) write(ctx context.Context, sel Selection, record Record) (Selection, error) {
	revision, err := n.cfg.Store.Write(ctx, sel.Logical, record, sel.Revision)
	if err != nil {
		return sel, err
	}
	return sel.with(record, revision), nil
}

// settleOp is Step 0: it settles sel's open operation. waited reports, for a candidate the server
// does not hold, whether this call has waited the settle age for it itself. A replace reserved with
// no start has destroyed nothing, so its candidate is retired, a fresh one reserved, and the start
// read from the predecessor then.
func (n *Names) settleOp(ctx context.Context, sel Selection, create CreateFunc, waited func(string) bool) (Selection, error) {
	op := sel.Record.Op
	if op.Kind == OpDelete {
		return n.settleDelete(ctx, sel)
	}
	if op.Kind == OpReplace && sel.Record.Start == 0 {
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
	info, err := n.cfg.Server.Info(ctx, op.Candidate)
	if err == nil {
		return n.publish(ctx, sel, info, OperationRecover, OutcomeAdopted)
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
// it was and the saved start stands for the next create.
func (n *Names) abandon(ctx context.Context, sel Selection) (Selection, error) {
	record := sel.Record.clone().retire(sel.Record.Op.Candidate)
	record.Op = nil
	next, err := n.write(ctx, sel, record)
	if err != nil {
		return next, err
	}
	n.observe(OperationRecover, OutcomeRetired)
	return next, nil
}

// saveReplaceStart writes the start of a replace reserved with no start: the sequence after the
// predecessor's floor. A predecessor the server does not hold leaves the operation open with an
// error.
func (n *Names) saveReplaceStart(ctx context.Context, sel Selection) (Selection, error) {
	op := sel.Record.Op
	info, err := n.cfg.Server.Info(ctx, op.Old)
	if errors.Is(err, ErrNotFound) {
		return sel, stepError(sel.Logical, OperationReplace, op.Old, fmt.Errorf("the replace reserved no start and its predecessor is absent: %w", err))
	}
	if err != nil {
		return sel, stepError(sel.Logical, OperationReplace, op.Old, err)
	}
	record := sel.Record.clone()
	record.Start = n.cfg.Floor(info) + 1
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
	if err := n.cfg.Server.Delete(ctx, op.Old); err != nil {
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
		read, err := n.cfg.Server.Info(ctx, op.Old)
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

// lastFloorStart is the sequence after record's last floor, when that floor was read from name on
// this stream: both identities non-empty and equal, since a stream that carries no identity cannot
// be told from one recreated with its sequences begun again.
func (n *Names) lastFloorStart(ctx context.Context, record Record, name string) (uint64, bool, error) {
	lf := record.LastFloor
	if lf == nil || lf.Name != name || lf.StreamID == "" {
		return 0, false, nil
	}
	id, err := n.cfg.StreamID(ctx)
	if err != nil || id != lf.StreamID {
		return 0, false, err
	}
	return lf.Floor + 1, true, nil
}

// safeStart is the start of a missing current: the sequence after the record's last floor when
// that was read from the current on this stream (lastFloorStart), else the saved start, else
// ErrNoSafeStart. It never searches the server's other consumers and never uses a first-create
// rule.
func (n *Names) safeStart(ctx context.Context, sel Selection) (uint64, error) {
	record := sel.Record
	if start, ok, err := n.lastFloorStart(ctx, record, record.Current); err != nil || ok {
		return start, err
	}
	if record.Start > 0 {
		return record.Start, nil
	}
	return 0, fmt.Errorf("selection of %s names a missing consumer and no safe start is known: %w", sel.Logical, ErrNoSafeStart)
}

// Ensure returns logical's current consumer, and creates one when the record selects none: at the
// saved start, or the caller's lower one; for a key with no record at replay_from while the store
// is in replay mode, else at the family's first-create rule, no later than the cutover boundary
// unless the rule is native and Config.NativeFirstCreate keeps it. created says this call created
// the current.
func (n *Names) Ensure(ctx context.Context, logical string, req CreateRequest) (Selection, bool, error) {
	for conflicts := 0; ; {
		sel, err := n.Select(ctx, logical, req.Create)
		if err != nil || sel.Record.Current != "" {
			return sel, sel.Created, err
		}
		start, err := n.startFor(ctx, sel, req)
		if err != nil {
			return sel, false, err
		}
		created, err := n.reserve(ctx, sel, start, req.Create, OperationCreate)
		if errors.Is(err, ErrConflict) {
			if conflicts++; conflicts > maxConflicts {
				return created, false, err
			}
			continue
		}
		return created, err == nil, err
	}
}

func (n *Names) startFor(ctx context.Context, sel Selection, req CreateRequest) (Start, error) {
	if sel.Exists && sel.Record.Start > 0 {
		start := sel.Record.Start
		if req.Lower > 0 && req.Lower < start {
			start = req.Lower
		}
		return Start{Seq: start}, nil
	}
	if sel.State.ReplayFrom > 0 {
		return Start{Seq: sel.State.ReplayFrom}, nil
	}
	start, err := req.First(ctx)
	if err != nil {
		return Start{}, err
	}
	if n.cfg.NativeFirstCreate && start.Native != "" {
		return start, nil
	}
	if b := sel.State.Boundary; b > 0 {
		switch {
		case start.Native == NativeAll:
			// Every message the stream holds starts no later than the boundary already.
		case start.Native != "" || start.Seq > b:
			start = Start{Seq: b}
		}
	}
	return start, nil
}

// reserve writes a create of a fresh candidate at start, then creates and publishes it.
func (n *Names) reserve(ctx context.Context, sel Selection, start Start, create CreateFunc, operation Operation) (Selection, error) {
	record := sel.Record.clone()
	record.Op = &Op{Kind: OpCreate, Owner: n.cfg.Owner, Candidate: FreshName(n.cfg.Prefix(sel.Logical)), Native: start.Native, ReservedAt: n.cfg.Now()}
	record.Start = start.Seq
	next, err := n.write(ctx, sel, record)
	if err != nil {
		n.observeErr(operation, err)
		return next, err
	}
	if err := n.reserved(ctx, next); err != nil {
		return next, err
	}
	return n.launch(ctx, next, create, operation)
}

// Replace replaces sel's current with a fresh consumer at start: it reserves the replace with the
// start, creates the candidate, publishes it and retires the predecessor, then deletes the
// predecessor. A start of 0 reserves the replace with no start, and the start is read from the
// predecessor's floor after the reservation. At the stream's max_consumers it deletes the retired
// names the server holds and tries again, and then deletes the predecessor, whose start is saved,
// and creates the candidate: the cap is never updated.
func (n *Names) Replace(ctx context.Context, sel Selection, start uint64, create CreateFunc) (Selection, error) {
	if sel.Record.Current == "" {
		return sel, fmt.Errorf("consumer names: %s has no current to replace: %w", sel.Logical, ErrNotFound)
	}
	record := sel.Record.clone()
	record.Op = &Op{Kind: OpReplace, Owner: n.cfg.Owner, Old: sel.Record.Current, Candidate: FreshName(n.cfg.Prefix(sel.Logical)), ReservedAt: n.cfg.Now()}
	record.Start = start
	next, err := n.write(ctx, sel, record)
	if err != nil {
		n.observeErr(OperationReplace, err)
		return next, err
	}
	if err := n.reserved(ctx, next); err != nil {
		return next, err
	}
	if start == 0 {
		if next, err = n.saveReplaceStart(ctx, next); err != nil {
			n.observeErr(OperationReplace, err)
			return next, err
		}
	}
	return n.launch(ctx, next, create, OperationReplace)
}

// launch creates sel's open operation's candidate and publishes it. No name is sent to the
// server's create twice: each retry retires the candidate it tried and reserves a fresh one. A
// create whose outcome is unknown is settled with an info: a candidate the server holds is
// adopted, and one it does not is retired, with a fresh candidate tried once in its place, by
// start sequence. At the stream's max_consumers the retired names the server holds are deleted
// and a fresh candidate tried, then a replace's predecessor, whose start is saved; a family that
// cannot delete (Config.NoDelete) is answered the cap at once. A refusal the server answered leaves
// the operation open for Step 0.
func (n *Names) launch(ctx context.Context, sel Selection, create CreateFunc, operation Operation) (Selection, error) {
	predecessorDeleted, retried := false, false
	for attempt := 0; ; attempt++ {
		op := sel.Record.Op
		start := Start{Seq: sel.Record.Start, Native: op.Native}
		info, err := create(ctx, op.Candidate, start)
		switch {
		case err == nil:
			return n.publish(ctx, sel, info, operation, OutcomeOK)
		case errors.Is(err, ErrConsumerLimit):
			switch {
			case n.cfg.NoDelete:
				n.observe(operation, OutcomeLimit)
				return sel, stepError(sel.Logical, operation, op.Candidate, err)
			case attempt == 0:
				n.deleteRetiredHeld(ctx, sel)
			case op.Kind == OpReplace && op.Old != "" && !predecessorDeleted:
				if err := n.cfg.Server.Delete(ctx, op.Old); err != nil {
					n.observe(operation, OutcomeError)
					return sel, stepError(sel.Logical, operation, op.Old, err)
				}
				predecessorDeleted = true
			default:
				n.observe(operation, OutcomeLimit)
				return sel, stepError(sel.Logical, operation, op.Candidate, err)
			}
			next, err := n.rereserve(ctx, sel, op.Native)
			if err != nil {
				n.observeErr(operation, err)
				return next, err
			}
			sel = next
			continue
		case answered(err):
			n.observe(operation, OutcomeError)
			return sel, stepError(sel.Logical, operation, op.Candidate, err)
		}
		found, infoErr := n.cfg.Server.Info(ctx, op.Candidate)
		if infoErr == nil {
			return n.publish(ctx, sel, found, operation, OutcomeAdopted)
		}
		if !errors.Is(infoErr, ErrNotFound) || retried {
			n.observe(operation, OutcomeError)
			return sel, stepError(sel.Logical, operation, op.Candidate, err)
		}
		retried = true
		next, werr := n.rereserve(ctx, sel, retryNative(sel))
		if werr != nil {
			n.observeErr(operation, werr)
			return next, werr
		}
		sel = next
	}
}

// rereserve retires sel's open operation's candidate and reserves a fresh one, keeping native as
// the operation's native policy, then runs Config.Reserved on it.
func (n *Names) rereserve(ctx context.Context, sel Selection, native Native) (Selection, error) {
	op := sel.Record.Op
	record := sel.Record.clone().retire(op.Candidate)
	retry := *op
	retry.Candidate, retry.Native, retry.ReservedAt, retry.Owner = FreshName(n.cfg.Prefix(sel.Logical)), native, n.cfg.Now(), n.cfg.Owner
	record.Op = &retry
	next, err := n.write(ctx, sel, record)
	if err != nil {
		return next, err
	}
	return next, n.reserved(ctx, next)
}

// retryNative is the native policy a fresh candidate retried in place of sel's open operation's
// keeps: none, so the retry starts by start sequence at the saved start, unless no start is saved
// (a native first create of Config.NativeFirstCreate whose candidate never reported one), where it
// keeps the operation's native policy, as today's first create does.
func retryNative(sel Selection) Native {
	if sel.Record.Start == 0 {
		return sel.Record.Op.Native
	}
	return ""
}

// answered reports whether err is the server's answer to a create rather than a create whose
// outcome is unknown.
func answered(err error) bool {
	var api *jetstream.APIError
	return errors.As(err, &api) || errors.Is(err, ErrConsumerLimit)
}

// publish makes sel's candidate, created as info, the current, and retires and deletes a
// replace's predecessor. Delivery from the candidate begins only after this returns. A predecessor
// whose delete the server confirmed is dropped from the retired names again, so a session replaced
// many times keeps a record of one size; one whose delete failed stays retired, for the cleanup. A
// refused drop leaves it retired too, and the cleanup finds it gone.
func (n *Names) publish(ctx context.Context, sel Selection, info *jetstream.ConsumerInfo, operation Operation, outcome Outcome) (Selection, error) {
	op := sel.Record.Op
	record := sel.Record.clone()
	record.Current = op.Candidate
	if info != nil && info.Config.DeliverPolicy == jetstream.DeliverByStartSequencePolicy && info.Config.OptStartSeq > 0 {
		record.Start = info.Config.OptStartSeq
	} else if info != nil && op.Native != "" && record.Start == 0 {
		// A native first create with no start saved: the consumer's own, the sequence after the
		// last one it reports delivered, which for a consumer that delivered nothing yet is the
		// first it will deliver.
		record.Start = info.Delivered.Stream + 1
	}
	record.LastFloor = nil
	old := ""
	if op.Kind == OpReplace {
		old = op.Old
		record = record.retire(old)
	}
	record.Op = nil
	next, err := n.write(ctx, sel, record)
	if err != nil {
		n.observeErr(operation, err)
		return next, err
	}
	next.Info, next.Created, next.Missing = info, true, false
	n.observe(operation, outcome)
	if old == "" {
		return next, nil
	}
	if err := n.cfg.Server.Delete(ctx, old); err != nil {
		n.observe(OperationCleanup, OutcomeError)
		return next, nil
	}
	kept := next.Record.clone()
	kept.Retired = slices.DeleteFunc(kept.Retired, func(name string) bool { return name == old })
	if len(kept.Retired) == 0 {
		kept.Retired = nil
	}
	if dropped, err := n.write(ctx, next, kept); err == nil {
		dropped.Info, dropped.Created = next.Info, next.Created
		next = dropped
	}
	return next, nil
}

// deleteRetiredHeld deletes the retired names of sel that the server holds, to free their slots.
func (n *Names) deleteRetiredHeld(ctx context.Context, sel Selection) {
	for _, name := range sel.Record.Retired {
		if _, err := n.cfg.Server.Info(ctx, name); err == nil {
			n.observeErr(OperationCleanup, n.cfg.Server.Delete(ctx, name))
		}
	}
}

// Delete deletes logical's current consumer: it reserves the delete, reads the consumer's floor
// (from listed when that is the current's info), records it (Config.RecordFloor) and writes the
// start after it, deletes the consumer, and clears the current. The record and its start stay.
// A floor that cannot be read stops the call, and is never taken as 0. It answers the floor it
// recorded, 0 when it recorded none (no current, or a current the server no longer held).
func (n *Names) Delete(ctx context.Context, logical string, listed *jetstream.ConsumerInfo) (uint64, error) {
	for conflicts := 0; ; {
		floor, err := n.deleteOnce(ctx, logical, listed)
		if errors.Is(err, ErrConflict) {
			if conflicts++; conflicts <= maxConflicts {
				continue
			}
		}
		n.observeErr(OperationDelete, err)
		return floor, err
	}
}

func (n *Names) deleteOnce(ctx context.Context, logical string, listed *jetstream.ConsumerInfo) (uint64, error) {
	sel, err := n.selectRecord(ctx, logical, nil, false)
	if err != nil {
		return 0, err
	}
	current := sel.Record.Current
	if current == "" {
		return 0, nil
	}
	record := sel.Record.clone()
	record.Op = &Op{Kind: OpDelete, Owner: n.cfg.Owner, Old: current, ReservedAt: n.cfg.Now()}
	if sel, err = n.write(ctx, sel, record); err != nil {
		return 0, err
	}
	var info *jetstream.ConsumerInfo
	if listed != nil && listed.Name == current {
		info = listed
	}
	before := sel.Record.Start
	if sel, err = n.recordDeleteFloor(ctx, sel, info); err != nil {
		return 0, err
	}
	var floor uint64
	if sel.Record.Start != before || info != nil {
		floor = sel.Record.Start - 1
	}
	if err := n.cfg.Server.Delete(ctx, current); err != nil {
		return floor, stepError(logical, OperationDelete, current, err)
	}
	if _, err := n.clearDeleted(ctx, sel); err != nil {
		return floor, err
	}
	return floor, nil
}

// RecordLastFloor writes floor, read from consumer name on the stream streamID, as logical's last
// floor, when name is still the current and no operation is open. A floor of another name changes
// nothing.
func (n *Names) RecordLastFloor(ctx context.Context, logical, name string, floor uint64, streamID string) error {
	for conflicts := 0; ; {
		sel, err := n.read(ctx, logical)
		if err != nil {
			return err
		}
		if sel.Record.Current != name || sel.Record.Op != nil {
			return nil
		}
		if lf := sel.Record.LastFloor; lf != nil && *lf == (Floor{Floor: floor, Name: name, StreamID: streamID}) {
			return nil
		}
		record := sel.Record.clone()
		record.LastFloor = &Floor{Floor: floor, Name: name, StreamID: streamID}
		_, err = n.write(ctx, sel, record)
		if errors.Is(err, ErrConflict) {
			if conflicts++; conflicts <= maxConflicts {
				continue
			}
		}
		n.observeErr(OperationLastFloor, err)
		return err
	}
}

// Cleanup deletes logical's retired names that the server holds, and drops from the record each
// retired name the server no longer holds. It answers how many it deleted. Only retired names are
// deleted, never the current, never by name pattern. A refused write of the record leaves the
// names for the next cleanup, which finds them gone.
func (n *Names) Cleanup(ctx context.Context, logical string) (int, error) {
	sel, err := n.selectRecord(ctx, logical, nil, false)
	if err != nil || len(sel.Record.Retired) == 0 {
		return 0, err
	}
	deleted := 0
	gone := map[string]bool{}
	for _, name := range sel.Record.Retired {
		if name == sel.Record.Current {
			continue
		}
		_, err := n.cfg.Server.Info(ctx, name)
		switch {
		case err == nil:
			if err := n.cfg.Server.Delete(ctx, name); err != nil {
				n.observe(OperationCleanup, OutcomeError)
				continue
			}
			deleted++
			n.observe(OperationCleanup, OutcomeOK)
		case !errors.Is(err, ErrNotFound):
			continue
		}
		gone[name] = true
	}
	if len(gone) == 0 {
		return deleted, nil
	}
	record := sel.Record.clone()
	record.Retired = slices.DeleteFunc(record.Retired, func(name string) bool { return gone[name] })
	if len(record.Retired) == 0 {
		record.Retired = nil
	}
	if _, err := n.write(ctx, sel, record); err != nil && !errors.Is(err, ErrConflict) {
		return deleted, err
	}
	return deleted, nil
}

func sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
