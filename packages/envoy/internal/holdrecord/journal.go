package holdrecord

import (
	"errors"
	"fmt"
	"strings"
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

// Operation is one journal entry. State is `intent`, `done` or `stop`. An `intent` entry carries
// the command's inputs, a `done` entry its outputs, and a `stop` entry its reason in
// Outputs["reason"] and, when AppendStopLeft wrote it, what the command left in Outputs["left"]:
// LeftUnchanged, or what it changed.
type Operation struct {
	Command string            `json:"command"`
	State   string            `json:"state"`
	Inputs  map[string]string `json:"inputs,omitempty"`
	Outputs map[string]string `json:"outputs,omitempty"`
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
// targets with `--restored`, so a prove's request always holds restored targets.
type Request struct {
	Command    string
	DisksMoved bool
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
// (LeftUnchanged) or that stop's `intent`, so nothing changed.
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
// ErrBusy while another command's `intent` is pending, so it is admitted once that command has
// stopped. In `hub-restore` a `done` CommandProve entry excludes it. It moves the row to `released`
// before its `done` entry, which completes the record; its `intent` carries each stop's record of
// what its command left (Admit). It is refused after completion. From `prepare` it deletes the
// cohort (DeleteCohort) and moves the row, and writes no entry.
const CommandAbort = "hold abort"

// LeftUnchanged is what a stop records (AppendStopLeft) when its command changed nothing outside
// the journal before it failed; a rerun's stop records what every run of the command left, a run
// cut after its intent included. Only stops that record it leave a hold that CommandRelease can
// end without completion; a stop that records anything else, or nothing (AppendStop), counts as
// changed, and the way out is CommandAbort.
const LeftUnchanged = "unchanged"

// stopLeftKey is the key of a stop's Outputs that holds what its command left.
const stopLeftKey = "left"

// The three states of a journal entry, as the `done` record stores them.
const (
	stateIntent = "intent"
	stateDone   = "done"
	stateStop   = "stop"
)

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

// Stoppable reports whether the command writes a `stop` entry when its own check refuses. Each
// Stoppable command is either needed again later, or leaves nothing that a later step must
// finish, or, as the buckets commands that fail for good after their intent, records in its stop
// what it left (AppendStopLeft), so the operator ends the hold with `hold abort`, or with `hold
// release` when it left nothing changed. `hold release` is Stoppable in every workflow: its disk
// check in `hub-restore` refuses before its PUT.
func Stoppable(w Workflow, command string) bool { return current.Stoppable(w, command) }

// Epochal reports whether the command counts only the entries of its epoch.
func Epochal(w Workflow, command string) bool { return current.Epochal(w, command) }

// Decide is Admit's decision on a verified record. It returns nil when the command may write its
// `intent` entry, and otherwise the first refusal in this order: ErrBusy, ErrOutOfOrder for `hold
// release`, ErrNotAdmitted, ErrAlreadyDone after completion, ErrExcluded, ErrOutOfOrder,
// ErrAlreadyDone.
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
		if d.Ops[i].Command == CommandProve && d.Ops[i].State == stateDone {
			return d.Ops[i], d.Ops[i+1:], true
		}
	}
	return Operation{}, nil, false
}

// hasDone reports whether the entries hold a `done` entry of the command.
func hasDone(ops []Operation, command string) bool {
	for _, o := range ops {
		if o.Command == command && o.State == stateDone {
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
		if latest[c] == stateIntent {
			out = append(out, c)
		}
	}
	return out
}

func wrap(err error, what string) error { return fmt.Errorf("%w: %s", err, what) }
