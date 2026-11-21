package holdrecord

import "slices"

// rules are the rule choices tableFor and Decide read: those that changed between versions of the
// hold's design, so the model in model_test.go builds the table of each version and of each rule
// set with one fix removed (its modelRules adds the choices only its transitions read).
// currentRules are the design's, and the package's functions use them.
type rules struct {
	ProveEpochs           bool // --restored after a done prove, and an epoch per done prove for the census
	PlainReprove          bool // the earlier designs' prove that names no target
	AfterCompleteStrict   bool // after completion a command is admitted only when the final command can follow it again
	SelRecFinalRepeatable bool // replay --final is Repeatable in selection-recovery
	RestoreKeyInTable     bool // restore --key is in rollback's table, Repeatable
	Serialize             bool // ErrBusy while another command's intent is pending
	StopEntries           bool // a Stoppable command's refusal writes a stop entry, which ends its pending intent
	SetCommand            bool // hold set is a journal command, its intent before its row move, Stoppable
	ConvertStoppable      bool // convert's stop for a name with no safe start ends its intent, so replay is admitted
	// WayOut: the buckets commands are Stoppable, a stop records what its command left, hold
	// release follows a record whose other entries are stops that left nothing changed, and hold
	// abort is every workflow's way out: outside hub-restore it is admitted once a command's latest
	// entry is a stop and no intent is pending, so a command that fails for good after its intent
	// cannot hold the hold.
	WayOut bool
	// EveryCommandStoppable: every command of every workflow is Stoppable, not the buckets
	// commands alone, since any command's own check can refuse after its intent (a streams update
	// that differs otherwise, a census that cannot read a stream), and a command that cannot stop
	// holds its intent pending for good: ErrBusy then refuses release and abort alike.
	EveryCommandStoppable bool
	// EndIsFinal: a done hold release or hold abort ends the journal, and Decide refuses every
	// command after it. Admit reads the row before the journal, so a process can read the row
	// held and another end the hold before its journal read; this rule makes that process's
	// decision a refusal, and the journal's compare-and-swap refuses a write decided before it.
	EndIsFinal bool
	// BackupCommand: `envoy-infra buckets backup`, a snapshot of each copied bucket, runs under a
	// hold, as the first command of buckets-recreate and buckets-restore, Repeatable. The recreate,
	// which deletes a bucket, requires a done `buckets backup` entry in this hold, which is what
	// Decide checks; Decide sees no bucket names, so that the backup holds each bucket the recreate
	// deletes is the recreate command's to check, from the backup's outputs. The restore requires
	// no backup, since a lost bucket cannot be backed up, and it restores from an archive an
	// earlier hold took. Its stop records LeftUnchanged, but its done entry counts as a change under
	// hold release's rule as every done entry does, so after a backup the way out of a recreate or
	// restore that stops is hold abort.
	BackupCommand bool
	// HubAbortAfterStop: in hub-restore, after a done hold prove, hold abort is admitted only once
	// the pending command's latest entry is a stop (a census that fails for good, say): a command
	// whose latest entry, after the latest done prove, is a stop. With no stop there the prove still
	// excludes it (ErrExcluded), and a stop from before the prove lifts nothing. A crashed abort's
	// rerun needs nothing more: the stop that admitted it is still that command's latest entry. The abort
	// releases the row, and its entry carries the stop's record of what was left. Hold release
	// stays refused: hub-restore's completion is its clean census. Comparing each disk with the
	// done prove's record before the abort releases is the hold's command's (t-10's), not this
	// table's. After a failed re-prove on a disk that moved, the operator restores that target back
	// onto the done prove's disk, a restore this table already admits, and then runs abort; if the
	// proven disk is gone, abort's check writes a stop naming it, and AbortAfterOwnStop's path
	// follows. The residual: on this path clients reach the restored and proven hub before its
	// census, until a streams-census hold runs clean.
	HubAbortAfterStop bool
	// AbortAfterOwnStop: hub-restore's hold abort is admitted again after its own stop, whenever
	// that stop was written, and reruns the reconcile for the stores that stop named; its own
	// pending intent admits it too, since the stop that admitted a crashed run is no longer its
	// latest entry. An abort admitted on its own pending intent is a rerun of a crashed abort: it
	// repeats the whole abort from its start, the disk check against the newest done prove and then
	// the reconcile over every store, and trusts nothing the crashed run wrote. An abort whose disk
	// check found the done prove's
	// disk gone (a 404 by name, or that name now has another numeric id) writes a stop naming the
	// disk; that stop admits a restore onto another disk and a new hold prove there, which this
	// table already admits before completion: the new prove's done opens a new epoch, abort's check
	// reads the newest done prove, and abort is admitted again after its own stop.
	AbortAfterOwnStop bool
}

// currentRules are the hold's rules as its design stands: the frozen model's v62 (every fix, with
// no account move) and the rules added since, WayOut, EveryCommandStoppable, EndIsFinal,
// BackupCommand and HubAbortAfterStop.
var currentRules = rules{ProveEpochs: true, AfterCompleteStrict: true, SelRecFinalRepeatable: true, RestoreKeyInTable: true,
	Serialize: true, StopEntries: true, SetCommand: true, ConvertStoppable: true, WayOut: true, EveryCommandStoppable: true,
	EndIsFinal: true, BackupCommand: true, HubAbortAfterStop: true, AbortAfterOwnStop: true}

var current = tableFor(currentRules)

// commandBackup is `envoy-infra buckets backup` (BackupCommand). Its stop records LeftUnchanged:
// it writes only the archive and its manifest, outside the hub.
const commandBackup = "buckets backup"

type entry struct {
	name       string
	requires   []string
	excludes   []string
	final      bool
	repeatable bool
	stoppable  bool
	epochal    bool
	// skipsSet is a command that needs no `done` hold set entry: hold abort, which ends a hold
	// whose hold set stopped as well as any other.
	skipsSet bool
	// needsStop is a command admitted only once some command's latest entry is a stop, or its own
	// intent is pending, a rerun after a crash: hold abort outside hub-restore, the way out of a
	// failed command and not a way to end a workflow that is going well. The rerun is admitted
	// since the stop the crashed run followed may have been superseded since (the stopped command
	// ran again and finished).
	needsStop bool
	// stopLiftsExcludes is a command whose excludes refuse it only until, after the latest done
	// hold prove, some command's latest entry is a stop: hub-restore's hold abort under
	// HubAbortAfterStop.
	stopLiftsExcludes bool
	// ownStopLifts is a stopLiftsExcludes command whose own latest stop, whenever written, or its
	// own pending intent lifts its excludes too: hub-restore's hold abort under AbortAfterOwnStop.
	ownStopLifts bool
}

// table is one rule set's workflow table and rules.
type table struct {
	rows  map[Workflow][]entry
	Rules rules
}

// tableFor builds the table that the rules give.
func tableFor(r rules) table {
	stop := r.StopEntries
	cut := []entry{{name: "create-bucket"}, {name: "replay", repeatable: true}, {name: "convert", stoppable: stop && r.ConvertStoppable},
		{name: "streams census", requires: []string{"convert"}, final: true}}
	rb := []entry{{name: "restore --prepare"}}
	if r.RestoreKeyInTable {
		rb = append(rb, entry{name: "restore --key", requires: []string{"restore --prepare"}, repeatable: true})
	}
	rb = append(rb, entry{name: "restore --finish", requires: []string{"restore --prepare"}, final: true, stoppable: stop})
	sr := []entry{{name: "create-bucket --after-loss", final: true},
		{name: "replay", requires: []string{"create-bucket --after-loss"}, repeatable: true},
		{name: "replay --final", requires: []string{"create-bucket --after-loss"}, final: true, repeatable: r.SelRecFinalRepeatable}}
	prove := entry{name: CommandProve, repeatable: r.PlainReprove || r.ProveEpochs, stoppable: stop}
	var hub []entry
	if r.SetCommand {
		hub = []entry{prove} // hold set, which every command but hold abort requires, carries the restore's inputs
	} else {
		prove.requires = []string{"snapshot restore"}
		hub = []entry{{name: "snapshot restore"}, prove}
	}
	hub = append(hub,
		entry{name: "streams census", requires: []string{CommandProve}, final: true, epochal: r.ProveEpochs, repeatable: r.ProveEpochs,
			stoppable: stop},
		entry{name: CommandAbort, excludes: []string{CommandProve}, final: true, stoppable: stop, skipsSet: true,
			stopLiftsExcludes: r.HubAbortAfterStop, ownStopLifts: r.AbortAfterOwnStop})
	buckets := func(name string) []entry { return []entry{{name: name, final: true, stoppable: stop && r.WayOut}} }
	recreate, restore := buckets("buckets recreate"), buckets("buckets restore")
	if r.BackupCommand {
		backup := entry{name: commandBackup, repeatable: true, stoppable: stop && r.WayOut}
		recreate[0].requires = []string{commandBackup}
		recreate = append([]entry{backup}, recreate...)
		restore = append([]entry{backup}, restore...)
	}
	rows := map[Workflow][]entry{
		WorkflowCutover:           cut,
		WorkflowRollback:          rb,
		WorkflowSelectionRecovery: sr,
		WorkflowBucketsCreate:     buckets("buckets create"),
		WorkflowBucketsRecreate:   recreate,
		WorkflowBucketsRestore:    restore,
		WorkflowStreamsUpdate:     {{name: "streams update", final: true}},
		WorkflowStreamsCensus:     {{name: "streams census", final: true}},
		WorkflowHubRestore:        hub,
	}
	if r.WayOut { // hold abort, every workflow's way out; hub-restore names it already
		for w := range rows {
			if w != WorkflowHubRestore {
				rows[w] = append(rows[w], entry{name: CommandAbort, final: true, stoppable: stop, skipsSet: true, needsStop: true})
			}
		}
	}
	if r.EveryCommandStoppable {
		for _, entries := range rows {
			for i := range entries {
				entries[i].stoppable = stop
			}
		}
	}
	return table{Rules: r, rows: rows}
}

func (t table) row(w Workflow, c string) (entry, bool) {
	for _, e := range t.rows[w] {
		if e.name == c {
			return e, true
		}
	}
	return entry{}, false
}

// Commands is the table's Commands.
func (t table) Commands(w Workflow) []string {
	var out []string
	for _, e := range t.rows[w] {
		out = append(out, e.name)
	}
	return out
}

// Admits is the table's Admits.
func (t table) Admits(w Workflow, c string) bool {
	if c == CommandSet {
		return t.Rules.SetCommand
	}
	_, ok := t.row(w, c)
	return ok
}

// Final is the table's Final.
func (t table) Final(w Workflow, c string) bool { e, _ := t.row(w, c); return e.final }

// Requires is the table's Requires.
func (t table) Requires(w Workflow, c string) []string { e, _ := t.row(w, c); return e.requires }

// Excludes is the table's Excludes.
func (t table) Excludes(w Workflow, c string) []string { e, _ := t.row(w, c); return e.excludes }

// Repeatable is the table's Repeatable.
func (t table) Repeatable(w Workflow, c string) bool { e, _ := t.row(w, c); return e.repeatable }

// Stoppable is the table's Stoppable.
func (t table) Stoppable(w Workflow, c string) bool {
	switch c {
	case CommandRelease:
		return t.Rules.StopEntries
	case CommandSet:
		return t.Rules.SetCommand && t.Rules.StopEntries
	}
	e, _ := t.row(w, c)
	return e.stoppable
}

// Epochal is the table's Epochal.
func (t table) Epochal(w Workflow, c string) bool { e, _ := t.row(w, c); return e.epochal }

// modelRequest is a Request with the plain prove of the earlier designs, a `hold prove` that named
// no target and proved Proven's disks again, which only the model's earlier rule sets send.
type modelRequest struct {
	Request
	Plain bool
}

// Decide is the table's Decide.
func (t table) Decide(d Done, req modelRequest) error {
	w, c := d.Workflow, req.Command
	if t.Rules.EndIsFinal {
		for _, end := range []string{CommandRelease, CommandAbort} {
			if hasDone(d.Ops, end) {
				return wrap(ErrAlreadyDone, "the hold has ended: "+end+" is done")
			}
		}
	}
	if t.Rules.Serialize {
		for _, p := range pending(d) {
			if p != c {
				return wrap(ErrBusy, p)
			}
		}
	}
	if c == CommandRelease {
		if d.Complete || t.unchanged(d.Ops) {
			return nil
		}
		return wrap(ErrOutOfOrder, "the record is not complete")
	}
	if t.Rules.SetCommand {
		if c == CommandSet {
			if hasDone(d.Ops, CommandSet) {
				return wrap(ErrAlreadyDone, CommandSet)
			}
			return nil
		}
		if e, ok := t.row(w, c); !(ok && e.skipsSet) && !hasDone(d.Ops, CommandSet) {
			return wrap(ErrOutOfOrder, CommandSet)
		}
	}
	e, ok := t.row(w, c)
	if !ok {
		return wrap(ErrNotAdmitted, string(w))
	}
	if d.Complete && t.Rules.AfterCompleteStrict && !t.afterComplete(d, req) {
		return wrap(ErrAlreadyDone, "the record is complete")
	}
	for _, x := range e.excludes {
		if e.stopLiftsExcludes && (stoppedSinceProve(d) || (e.ownStopLifts && (latestIsStop(d.Ops, c) || slices.Contains(pending(d), c)))) {
			break
		}
		if hasDone(d.Ops, x) {
			return wrap(ErrExcluded, x)
		}
	}
	if e.needsStop && !anyStopped(d.Ops) && !slices.Contains(pending(d), c) {
		return wrap(ErrOutOfOrder, "no command has stopped")
	}
	for _, q := range e.requires {
		if e.epochal && q == CommandProve {
			continue // the epoch's boundary, below
		}
		ops := d.Ops
		if e.epochal {
			_, ents, ok := Epoch(d)
			if !ok {
				return wrap(ErrOutOfOrder, CommandProve)
			}
			ops = ents
		}
		if !hasDone(ops, q) {
			return wrap(ErrOutOfOrder, q)
		}
	}
	if w == WorkflowHubRestore && c == CommandProve {
		return t.decideProve(d, req)
	}
	if e.epochal {
		_, ents, ok := Epoch(d)
		if !ok {
			return wrap(ErrOutOfOrder, CommandProve)
		}
		if hasDone(ents, c) {
			return wrap(ErrAlreadyDone, "in this epoch")
		}
		return nil
	}
	if hasDone(d.Ops, c) && !e.repeatable {
		return wrap(ErrAlreadyDone, c)
	}
	return nil
}

// unchanged reports whether the record shows nothing changed: no entry but hold release's own,
// which a crashed release leaves, and, with SetCommand, hold set's, whose restart and barrier
// change no stored data. With WayOut a command's stop that recorded LeftUnchanged, and the intent
// it ends, change nothing either; a stop that recorded anything else, or nothing, a done entry,
// an intent no stop ends, and an intent followed by another of the same command before its stop or
// done (a run cut after its intent, whose change no entry records) all count as changes. It reads
// each stop's record as written, but for the cut run, which the journal itself shows: by
// LeftUnchanged's rule a stop records what every run of its command left, so a crashed run's
// change is in its rerun's stop, which the model checks (releasedOverAChange).
func (t table) unchanged(ops []Operation) bool {
	latest := map[string]Operation{}
	for _, o := range ops {
		if o.Command == CommandRelease || (t.Rules.SetCommand && o.Command == CommandSet) {
			continue
		}
		if !t.Rules.WayOut || o.State == stateDone || (o.State == stateStop && o.Outputs[stopLeftKey] != LeftUnchanged) {
			return false
		}
		if o.State == stateIntent && latest[o.Command].State == stateIntent {
			return false // the run before this intent ended with no entry
		}
		latest[o.Command] = o
	}
	for _, o := range latest {
		if o.State != stateStop {
			return false
		}
	}
	return true
}

func (t table) decideProve(d Done, req modelRequest) error {
	_, proved := Proven(d)
	switch {
	case req.Plain && (!proved || !t.Rules.PlainReprove):
		return wrap(ErrNotAdmitted, "a prove that names no restored target")
	case req.Plain:
		if d.Complete {
			return wrap(ErrAlreadyDone, "a plain prove after completion")
		}
		return nil
	case !proved:
		return nil
	case !t.Rules.ProveEpochs:
		return wrap(ErrAlreadyDone, "a prove after a done prove")
	case d.Complete && !req.DisksMoved:
		return wrap(ErrAlreadyDone, "the record is complete and no disk moved")
	}
	return nil
}

// afterComplete reports whether a command is admitted after completion: only where the workflow's
// final command can follow it again.
func (t table) afterComplete(d Done, req modelRequest) bool {
	switch {
	case d.Workflow == WorkflowSelectionRecovery && (req.Command == "replay" || req.Command == "replay --final"):
		return true
	case d.Workflow == WorkflowHubRestore && req.Command == CommandProve && !req.Plain && req.DisksMoved && t.Rules.ProveEpochs:
		return true
	}
	return false
}
