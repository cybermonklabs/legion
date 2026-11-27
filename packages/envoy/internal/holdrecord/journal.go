package holdrecord

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Workflow is a hold's workflow, the name its `done` record carries.
type Workflow string

// The workflows, each the workflow's name in the hold's table.
const (
	WorkflowCutover           Workflow = "cutover"
	WorkflowRollback          Workflow = "rollback"
	WorkflowSelectionRecovery Workflow = "selection-recovery"
	WorkflowBucketsCreate     Workflow = "buckets-create"
	WorkflowBucketsRecreate   Workflow = "buckets-recreate"
	WorkflowBucketsRestore    Workflow = "buckets-restore"
	WorkflowStreamsUpdate     Workflow = "streams-update"
	WorkflowHubRestore        Workflow = "hub-restore"
	WorkflowStreamsCensus     Workflow = "streams-census"
)

// Operation is one journal entry. State is StateIntent, StateDone or StateStop. An `intent` entry
// carries the command's inputs, a `done` entry its outputs, and a `stop` entry its reason in
// Outputs["reason"] and, when AppendStopLeft wrote it, what the command left in Outputs["left"]:
// LeftUnchanged, or what it changed. Done.Latest and Entry.Left read them. Row names the hold row
// the entry was written under, which binds the journal to the log entries it names: the row Admit
// read for an `intent`, and the row read when a `done` or `stop` entry is written. The restore
// route reads a journal as unknown when an entry's row is not in its verified log, as another
// history's journal put back alone does once that history wrote an entry under a row it moved to,
// or an entry names none. Histories that share a row share the binding of the entries written
// under it, so it does not tell them apart.
type Operation struct {
	Command string            `json:"command"`
	State   string            `json:"state"`
	Inputs  map[string]string `json:"inputs,omitempty"`
	Outputs map[string]string `json:"outputs,omitempty"`
	Row     *RowRef           `json:"row,omitempty"`
}

// RowRef names one hold row, and so the log entry written with it: its gen and the time the
// operator signed it.
type RowRef struct {
	Gen      uint64    `json:"gen"`
	SignedAt time.Time `json:"signed_at"`
}

// Done is the `done` record, the hold's journal: its workflow, its entries in order, the
// completion and the row's `gen` at completion, and the step-1 map, `step1_disk_id.<target>` and
// `step1_volume.<claim>`, which StartJournal wrote.
type Done struct {
	Workflow Workflow          `json:"workflow"`
	Ops      []Operation       `json:"ops"`
	Complete bool              `json:"complete"`
	HoldGen  uint64            `json:"hold_gen"`
	Step1    map[string]string `json:"step1,omitempty"`
}

// Proof is the outputs of the latest `done` `hold prove` entry: each restored target's proven disk
// by name and by numeric id (a target absent from Disks keeps its step-1 disk).
type Proof struct {
	Disks   map[string]string
	DiskIDs map[string]string
}

// Request is what a command asks Admit for. DisksMoved is Moved's answer for the command's own
// reading of every target, which admits `hold prove` after completion. Every `hold prove` names its
// targets with `--restored`, so a prove's request always holds restored targets. RowReleased says
// the row is `released` with the hold's id, which Admit sets from the row it reads: Decide then
// admits `hold abort` alone, and only while the journal shows prepare (a row lost in `prepare` and
// restored `released`, ReleasedPrepareAbort), or for the zero Done, which stands for a hold with no
// journal (ReleasedNoJournalAbort); the cohort-delete route asks Decide the same, passing the zero
// Done for a hold that has no done record.
type Request struct {
	Command     string
	DisksMoved  bool
	RowReleased bool
}

// The errors. Each is wrapped with what it names.
var (
	// ErrUnknown is a row, record or cutover row that does not verify, or an answer without its
	// `sig`.
	ErrUnknown = errors.New("holdrecord: unknown")
	// ErrNotHeld is a row that is not `held` with the hold's id.
	ErrNotHeld = errors.New("holdrecord: the row is not held with this hold id")
	// ErrNotAdmitted is a command that the workflow's table does not admit.
	ErrNotAdmitted = errors.New("holdrecord: the workflow does not admit this command")
	// ErrOutOfOrder is a command whose Requires do not all have a `done` entry, or `hold release`
	// before the record is complete.
	ErrOutOfOrder = errors.New("holdrecord: a required command has no done entry")
	// ErrAlreadyDone is a command that already has a `done` entry and is not Repeatable, a command
	// after completion that the workflow's final command cannot follow again, or an epoch command
	// already `done` in its epoch.
	ErrAlreadyDone = errors.New("holdrecord: the command is already done")
	// ErrExcluded is a command that Excludes lists against a command with a `done` entry.
	ErrExcluded = errors.New("holdrecord: an excluding command is done")
	// ErrNoIntent is a `done` or `stop` entry whose command's latest entry is not its `intent`.
	ErrNoIntent = errors.New("holdrecord: the command's latest entry is not its intent")
	// ErrBusy is a command refused while another command's latest entry is an `intent` with no
	// `done` or `stop` after it, so at most one command is past its intent at any time.
	ErrBusy = errors.New("holdrecord: another command's intent is pending")
)

// CommandRelease is `hold release`, which every workflow admits once the record is complete, or
// while every entry but `hold set`'s and its own is a `stop` that left nothing changed
// (LeftUnchanged) or that stop's `intent`, so nothing changed. It trusts each stop's record, which
// by LeftUnchanged's rule names what every run of its command in the hold left, so it never moves
// the row over a change a crashed run made outside the journal.
const CommandRelease = "hold release"

// CommandSet is `hold set`, the first command of every workflow. Its `intent` is the one entry
// Admit writes with the row `prepare`, after its preflight passes and before it moves the row to
// `held`, and in `hub-restore` it carries the restore's inputs. Its `done` follows the steps it runs
// in `held`. Every workflow admits it while it has no `done` entry, it is Stoppable in every
// workflow, and every other command but CommandRelease and CommandAbort requires its `done` entry.
const CommandSet = "hold set"

// CommandProve is `hold prove`, `hub-restore`'s proof of the disks a restore landed on, which names
// each restored target with `--restored`. Its `done` entry records each named target's disk by name
// and by numeric id (Proven) and opens an epoch (Epoch); it is Repeatable and Stoppable, and after
// completion it is admitted only when a disk moved from the expected one (Request.DisksMoved).
const CommandProve = "hold prove"

// CommandAbort is `hold abort`, every workflow's way out of a held hold. It is final and Stoppable
// in every workflow, it needs no `hold set` `done` entry, and like every command it is refused
// ErrBusy while another command's `intent` is pending. Outside `hub-restore` it is admitted only
// once a command's latest entry is a `stop`, a command that stopped and has not run again since,
// and refused ErrOutOfOrder ("no command has stopped") otherwise, so it is the way out of a failed
// command, not a way to end a workflow that is going well. In `hub-restore`, before any done
// CommandProve, the hold's command passes its disk check only with each target's disk its step-1
// one. After a done prove, abort is admitted only once the pending command's latest entry is a
// `stop` (a stop after the latest done prove, a census that fails for good, say), or once its own
// latest entry is a stop, whenever written, and is refused ErrExcluded otherwise; another command's
// stop from before the latest prove lifts nothing. It then releases the row: the hold's command
// compares each disk with the done prove's record first, which is t-10's check, not this
// package's. After a failed re-prove on a disk that moved, the operator restores that target back
// onto the done prove's disk and then runs abort. If the check finds the done prove's disk gone (a
// 404 by name, or that name now has another numeric id), abort writes a stop naming it; the
// operator restores that target onto another disk and runs a new prove, whose done opens a new
// epoch, and abort, admitted again after its own stop, reruns the reconcile for the stores that
// stop named and releases over the new epoch's disks. The residual: on this path clients reach the
// restored and proven hub before its census, until a streams-census hold runs clean. Release stays
// refused, since hub-restore's completion is its clean census. Abort moves the row to `released`
// before its `done` entry, which completes the record; its `intent` carries each stop's record of
// what its command left (Admit). It is refused after completion. From `prepare` it deletes the
// cohort (DeleteCohort) and moves the row, and writes no entry. On a row lost in `prepare` and
// restored `released` with the hold's id, whose journal shows prepare (no done `hold set`, no
// ended entry, only `hold set`'s and abort's own entries), Admit admits it (Request.RowReleased,
// even over `hold set`'s pending intent, which can no longer move the row), it deletes the cohort,
// which the cohort-delete route admits on that row too, and its `done` entry ends the journal; the
// row is released already. When the row was lost before the journal was written, there is no
// journal for Admit to read: the abort deletes the cohort alone, which the route admits on the
// released row of a hold with no done record (ReleasedNoJournalAbort), and writes no entry. Like
// every command, its own pending intent admits it again, so two processes running abort at once are
// both admitted: the operator runs one command of a hold at a time (assumption A2).
const CommandAbort = "hold abort"

// LeftUnchanged is what a stop records (AppendStopLeft) when no run of its command in this hold
// changed anything outside the journal: it records `unchanged` only when this run changed nothing,
// and no earlier run of the command in this hold recorded a change or ended with no entry (an
// intent with no stop or done after it, a run cut by a crash, whose change no entry records).
// Otherwise the stop names what those runs left, or that what a run cut after its intent left is
// unknown, which counts as changed. Only stops that record it leave a hold that CommandRelease can
// end without completion; a stop that records anything else, or nothing (AppendStop), counts as
// changed, and the way out is CommandAbort.
const LeftUnchanged = "unchanged"

// stopLeftKey is the key of a stop's Outputs that holds what its command left.
const stopLeftKey = "left"

// The three states of a journal entry, as the `done` record stores them in Operation.State.
const (
	StateIntent = "intent"
	StateDone   = "done"
	StateStop   = "stop"
)

// Entry is one journal entry as the reader answers it: its index in Done.Ops and the entry.
type Entry struct {
	Index int
	Operation
}

// LeftItem is one stop's record of what its command left: the stop's index in Done.Ops, its
// command, and what it left, LeftUnchanged or what it changed (AppendStopLeft), or "" for a stop
// that recorded nothing (AppendStop), which counts as changed.
type LeftItem struct {
	Stop    int
	Command string
	Left    string
}

// Latest is the journal's latest entry of command, among states when any are given (StateIntent,
// StateDone, StateStop), and false when there is none: Latest(c) is c's latest entry, and
// Latest(c, StateDone) its latest done entry, whose outputs a later command reads.
func (d Done) Latest(command string, states ...string) (Entry, bool) {
	return latest(d.Ops, command, states...)
}

// Left is the record of what stopped commands left that the entry carries, by the stops' order in
// the journal: a stop's own, one item, and a hold abort intent's, one item for each stop before it
// (Admit writes them, under `left.<index>`). It answers false for every other entry.
func (e Entry) Left() ([]LeftItem, bool) {
	switch {
	case e.State == StateStop:
		return []LeftItem{{Stop: e.Index, Command: e.Command, Left: stopLeft(e.Operation)}}, true
	case e.State == StateIntent && e.Command == CommandAbort:
		var items []LeftItem
		for k, v := range e.Inputs {
			index, ok := strings.CutPrefix(k, leftInputPrefix)
			stop, err := strconv.Atoi(index)
			if !ok || err != nil {
				continue
			}
			command, left, _ := strings.Cut(v, leftInputSeparator)
			if left == leftNothing {
				left = ""
			}
			items = append(items, LeftItem{Stop: stop, Command: command, Left: left})
		}
		slices.SortFunc(items, func(a, b LeftItem) int { return a.Stop - b.Stop })
		return items, true
	default:
		return nil, false
	}
}

// The format of hold abort's intent's records (Admit, withStopRecords; read by Entry.Left): the key
// `left.<the stop's index>`, the value `<command>: <left>`, with leftNothing for a stop that
// recorded nothing. No command's name holds ": ".
const (
	leftInputPrefix    = "left."
	leftInputSeparator = ": "
	leftNothing        = "nothing recorded, so counted as changed"
)

// latest is Done.Latest over ops, which may be part of a journal (an epoch's entries, say).
func latest(ops []Operation, command string, states ...string) (Entry, bool) {
	for i := len(ops) - 1; i >= 0; i-- {
		if ops[i].Command == command && (len(states) == 0 || slices.Contains(states, ops[i].State)) {
			return Entry{Index: i, Operation: ops[i]}, true
		}
	}
	return Entry{}, false
}

// stopLeft is what a stop entry recorded its command left, "" for nothing recorded.
func stopLeft(o Operation) string { return o.Outputs[stopLeftKey] }

// Commands returns the workflow's commands, in the table's order.
func Commands(w Workflow) []string { return current.Commands(w) }

// Admits reports whether the workflow's table names the command.
func Admits(w Workflow, command string) bool { return current.Admits(w, command) }

// Final reports whether the command's `done` entry completes the record.
func Final(w Workflow, command string) bool { return current.Final(w, command) }

// Requires returns the commands that must already have a `done` entry. For an Epochal command,
// `hold prove` is the epoch's boundary, and every other one must be `done` among the epoch's
// entries.
func Requires(w Workflow, command string) []string { return current.Requires(w, command) }

// Excludes returns the commands whose `done` entry refuses the command.
func Excludes(w Workflow, command string) []string { return current.Excludes(w, command) }

// Repeatable reports whether the command is admitted again after its `done` entry. An Epochal
// command is Repeatable once in each epoch.
func Repeatable(w Workflow, command string) bool { return current.Repeatable(w, command) }

// Stoppable reports whether the command writes a `stop` entry when its own check refuses. Every
// command of every workflow is Stoppable, since any command's own check can refuse after its intent
// (a streams update that differs otherwise, a census that cannot read a stream, a restore whose
// snapshot never arrives), and a command that could not stop would hold its intent pending for
// good, which ErrBusy would let neither release nor abort end. A command's stop records what it
// left (AppendStopLeft), and the operator ends the hold with `hold abort`, or with `hold release`
// when every stop left nothing changed. `hold release` is Stoppable in every workflow too: its disk
// check in `hub-restore` refuses before its PUT.
func Stoppable(w Workflow, command string) bool { return current.Stoppable(w, command) }

// Epochal reports whether the command counts only the entries of its epoch.
func Epochal(w Workflow, command string) bool { return current.Epochal(w, command) }

// Decide is Admit's decision on a verified record. It returns nil when the command may write its
// `intent` entry. With Request.RowReleased it admits `hold abort` alone, and only while the journal
// shows prepare, and refuses anything else ErrNotHeld. Otherwise it returns the first refusal in
// this order: ErrAlreadyDone once the hold has ended (a done `hold release` or `hold abort`);
// ErrBusy; ErrOutOfOrder for `hold release` before completion (or a record whose other entries are
// not all stops that left nothing changed); ErrAlreadyDone for a second `hold set`; ErrOutOfOrder
// for any other command before `hold set`'s done entry, but `hold abort`; ErrNotAdmitted;
// ErrAlreadyDone after completion; ErrExcluded; ErrOutOfOrder for `hold abort` outside
// `hub-restore` when no command has stopped; ErrOutOfOrder for a required command with no done
// entry; ErrAlreadyDone.
func Decide(d Done, req Request) error { return current.Decide(d, modelRequest{Request: req}) }

// Proven returns the latest `done` `hold prove` entry's outputs, and false before any `hold prove`
// is `done`. `hold release`'s disk check in `hub-restore` reads it, through Expected, so the disks
// it trusts are always the latest prove's.
func Proven(d Done) (Proof, bool) {
	b, _, ok := Epoch(d)
	if !ok {
		return Proof{}, false
	}
	p := Proof{Disks: map[string]string{}, DiskIDs: map[string]string{}}
	for k, v := range b.Outputs {
		switch {
		case strings.HasPrefix(k, "disk_id."):
			p.DiskIDs[strings.TrimPrefix(k, "disk_id.")] = v
		case strings.HasPrefix(k, "disk."):
			p.Disks[strings.TrimPrefix(k, "disk.")] = v
		}
	}
	return p, true
}

// Step1 returns the record's step-1 map, and false for a record without one. Every comparison with
// a target's step-1 disk reads it, never a fresh read. `hold set` refuses, UNKNOWN, a target it
// cannot read, so the map names every target, and a target that the map lacks is UNKNOWN to `hold
// prove`, `hold abort` and `hold release`.
func Step1(d Done) (map[string]string, bool) { return d.Step1, len(d.Step1) > 0 }

// Expected returns each target's expected disk id: Proven's for each target the latest `done`
// `hold prove` names, and Step1's for every other target. It returns false for a record without a
// step-1 map.
func Expected(d Done) (map[string]string, bool) {
	s1, ok := Step1(d)
	if !ok {
		return nil, false
	}
	out := map[string]string{}
	for k, v := range s1 {
		if t, found := strings.CutPrefix(k, "step1_disk_id."); found {
			out[t] = v
		}
	}
	if p, proved := Proven(d); proved {
		for t, id := range p.DiskIDs {
			out[t] = id
		}
	}
	return out, true
}

// Moved reports whether read, the command's own reading of each target's disk id, differs from
// Expected for any target. It returns ErrUnknown when read and the step-1 map do not name the same
// targets.
func Moved(d Done, read map[string]string) (bool, error) {
	want, ok := Expected(d)
	if !ok || len(want) != len(read) {
		return false, wrap(ErrUnknown, "the targets read are not the step-1 map's")
	}
	moved := false
	for t, id := range read {
		w, found := want[t]
		if !found {
			return false, wrap(ErrUnknown, "target "+t+" is not in the step-1 map")
		}
		moved = moved || w != id
	}
	return moved, nil
}

// Epoch returns the epoch's boundary, the latest `done` `hold prove` entry, and the entries after
// it, and false before any `hold prove` is `done`. The boundary is the census's prerequisite in
// `hub-restore`, and the entries after it carry the epoch's census.
func Epoch(d Done) (Operation, []Operation, bool) {
	for i := len(d.Ops) - 1; i >= 0; i-- {
		if d.Ops[i].Command == CommandProve && d.Ops[i].State == StateDone {
			return d.Ops[i], d.Ops[i+1:], true
		}
	}
	return Operation{}, nil, false
}

// hasDone reports whether the entries hold a `done` entry of the command.
func hasDone(ops []Operation, command string) bool {
	for _, o := range ops {
		if o.Command == command && o.State == StateDone {
			return true
		}
	}
	return false
}

// pending returns the commands whose latest entry is an `intent`, in the order of their first
// entry.
func pending(d Done) []string {
	latest := map[string]string{}
	var order []string
	for _, o := range d.Ops {
		if _, seen := latest[o.Command]; !seen {
			order = append(order, o.Command)
		}
		latest[o.Command] = o.State
	}
	var out []string
	for _, c := range order {
		if latest[c] == StateIntent {
			out = append(out, c)
		}
	}
	return out
}

// CutRun reports whether a run of command in the journal ended with no entry: an intent of command
// whose command's entry before it is another intent, with no stop or done of it between, a run cut
// after its intent, whose change no entry records. By LeftUnchanged's rule a stop after a cut run
// cannot record LeftUnchanged (AppendStopLeft refuses it), and hold release does not follow one.
func (d Done) CutRun(command string) bool { return cutRun(d.Ops, command) }

// cutRun is Done.CutRun over ops, which table.unchanged reads for each command.
func cutRun(ops []Operation, command string) bool {
	for i, o := range ops {
		if o.Command != command || o.State != StateIntent {
			continue
		}
		if before, ok := latest(ops[:i], command); ok && before.State == StateIntent {
			return true
		}
	}
	return false
}

// latestIsStop reports whether command's latest entry in ops is a `stop`.
func latestIsStop(ops []Operation, command string) bool {
	e, ok := latest(ops, command)
	return ok && e.State == StateStop
}

// stoppedSinceProve reports whether, among the entries after the latest done hold prove (Epoch),
// some command's latest entry is a stop.
func stoppedSinceProve(d Done) bool {
	_, ents, ok := Epoch(d)
	return ok && anyStopped(ents)
}

// anyStopped reports whether some command's latest entry is a `stop`: a command that stopped and
// has not run again since.
func anyStopped(ops []Operation) bool {
	latest := map[string]string{}
	for _, o := range ops {
		latest[o.Command] = o.State
	}
	for _, state := range latest {
		if state == StateStop {
			return true
		}
	}
	return false
}

func wrap(err error, what string) error { return fmt.Errorf("%w: %s", err, what) }
