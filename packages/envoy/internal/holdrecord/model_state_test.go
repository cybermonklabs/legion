package holdrecord

import (
	"fmt"
	"iter"
	"math/bits"
	"sort"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------- commands

// modelCmd is one command the model runs, an index into cmdNames; noCmd is none. The indexes
// follow the names' sorted order, which init checks, so a set of commands (modelCmds) lists its
// members in their names' sorted order, the order in which toDone writes a set's entries, and two
// processes' commands compare as their names do (slotLess).
type modelCmd uint8

const (
	noCmd modelCmd = iota
	cmdBackup
	cmdBucketsCreate
	cmdBucketsRecreate
	cmdBucketsRestore
	cmdConvert
	cmdCreateBucket
	cmdCreateBucketAfterLoss
	cmdAbort
	cmdAbortPrepare   // hold abort from prepare: it deletes the cohort and moves the row, and writes no entry
	cmdAbortReleased  // hold abort on a row lost in prepare and restored released: its intent, the cohort's delete, its done
	cmdAbortNoJournal // the same with no journal: the cohort's delete alone, and no entry
	cmdProve
	cmdRelease
	cmdSet
	cmdReplay
	cmdReplayFinal
	cmdRestoreFinish
	cmdRestoreKey
	cmdRestorePrepare
	cmdSnapRestore // v59's first hub-restore command, written by hold set's process
	cmdCensus
	cmdStreamsUpdate
	numCmds
)

// A set of commands holds every command in its 32 bits.
const _ = 32 - numCmds

var cmdNames = [numCmds]string{
	cmdBackup:                commandBackup,
	cmdBucketsCreate:         "buckets create",
	cmdBucketsRecreate:       "buckets recreate",
	cmdBucketsRestore:        "buckets restore",
	cmdConvert:               "convert",
	cmdCreateBucket:          "create-bucket",
	cmdCreateBucketAfterLoss: "create-bucket --after-loss",
	cmdAbort:                 CommandAbort,
	cmdAbortPrepare:          "hold abort (prepare)",
	cmdAbortReleased:         "hold abort (released)",
	cmdAbortNoJournal:        "hold abort (released, no journal)",
	cmdProve:                 CommandProve,
	cmdRelease:               CommandRelease,
	cmdSet:                   CommandSet,
	cmdReplay:                "replay",
	cmdReplayFinal:           "replay --final",
	cmdRestoreFinish:         "restore --finish",
	cmdRestoreKey:            "restore --key",
	cmdRestorePrepare:        "restore --prepare",
	cmdSnapRestore:           "snapshot restore",
	cmdCensus:                "streams census",
	cmdStreamsUpdate:         "streams update",
}

// cmdByName is each command by its name, for the package's readings, which name commands.
var cmdByName = map[string]modelCmd{}

func init() {
	if !sort.StringsAreSorted(cmdNames[:]) {
		panic("holdmodel: cmdNames is not in its names' sorted order, which a set's order and slotLess rely on")
	}
	for c, name := range cmdNames {
		cmdByName[name] = modelCmd(c)
	}
}

func (c modelCmd) String() string { return cmdNames[c] }

// modelCmds is a set of commands, one bit per modelCmd.
type modelCmds uint32

func (s modelCmds) has(c modelCmd) bool          { return s&(1<<c) != 0 }
func (s modelCmds) with(c modelCmd) modelCmds    { return s | 1<<c }
func (s modelCmds) without(c modelCmd) modelCmds { return s &^ (1 << c) }

// all yields the set's members in their names' sorted order.
func (s modelCmds) all() iter.Seq[modelCmd] {
	return func(yield func(modelCmd) bool) {
		for t := s; t != 0; t &= t - 1 {
			if !yield(modelCmd(bits.TrailingZeros32(uint32(t)))) {
				return
			}
		}
	}
}

// String is the set's names in order, joined by "|".
func (s modelCmds) String() string {
	var names []string
	for c := range s.all() {
		names = append(names, c.String())
	}
	return strings.Join(names, "|")
}

// ---------------------------------------------------------------- state

// A state's numbers, its disk ids, gens, counts, steps and sets of targets, are int8: at the
// model's bounds each stays below 100 (anotherDisk is the largest), so a state is about a hundred
// bytes rather than the four hundred ints take, and the walk's millions of states are hashed,
// compared and held in that much less.

// modelDisk is one target's disk: its numeric id, and where its data came from (0 the original,
// 'H' the hold's snapshot, 'F' another snapshot). In the step-1 map, ID 0 is a target the map
// lacks.
type modelDisk struct {
	ID   int8
	Snap byte
}

// modelSlot is one operator process: the command it runs, its step (0 admitted with the intent not
// yet written, which only a two-write admission has, then the command's own steps), and the
// readings the command keeps while it runs.
type modelSlot struct {
	Cmd     modelCmd
	Set     int8 // hold prove's --restored targets, one bit per target
	Step    int8
	Gen     int8         // the row's gen that hold release or hold abort read, which its row move carries
	Base    [2]modelDisk // the step-1 disks the command compares with: the recorded map, or its own reading
	Named   [2]int8      // the disks a prove names
	Checked [2]int8      // the disks a prove's check passed on, which its done entry records for the targets it names
}

// modelJournal is the record a state stands for, kept as what the package's rules read: which
// commands have a done entry (Ever), which have one after the latest done prove (Since), the last
// done entry, the latest prove's disks, which commands' latest entry is an intent, and, under
// WayOut, what the stops of commands other than hold set and hold release left (Left). toDone
// combines the journal with the scenario's workflow and hub flag to build the record. A
// Repeatable command can run without limit in this finite space. Each walk fixes its scenario
// and table: admission depends on the journal and request (modelTable.admits), and disagreement
// depends on the journal and current disks. explore owns both caches, so neither crosses walks.
type modelJournal struct {
	Rec        bool // the done record exists
	Ever       modelCmds
	Since      modelCmds
	Last       modelCmd
	Proved     bool
	Proven     [2]int8 // the latest done prove's disk ids, for the targets it names
	ProvenSet  int8    // the targets the latest done prove names
	Intent     modelCmds
	Complete   bool
	StopLatest modelCmds    // in the rule sets since the bundle (stopsRecorded), the commands whose latest entry is a stop written after the latest done prove (or with none done), which hold abort's admission reads
	StopBefore modelCmds    // likewise, the commands whose latest entry is a stop written before the latest done prove
	Left       byte         // likewise, the stops of other commands than hold set and release: 0 none, 'U' each left nothing changed, 'C' one changed or recorded nothing
	Step1      [2]modelDisk // the step-1 map
}

// modelState is one state of a workflow: the record it stands for (modelJournal), and the row, the
// disks, the processes and what the model counts outside the record. It holds no pointer, so the
// collector never scans a walk's states.
type modelState struct {
	modelJournal
	Row        byte         // 'P' prepare, 'H' held, 'R' released
	HoldGen    int8         // the row's gen at completion
	RowGen     int8         // the row restores so far
	Unresolved bool         // rollback: a logical name with no start, which only restore --key resolves
	NoStart    bool         // cutover: a cohort member with no safe start, which only replay --key resolves
	Doomed     modelCmd     // a command that failed for good after its intent: every later run of it fails
	Wrote      modelCmds    // under Fail, the commands a run of which changed something outside the journal: the hub, not buckets backup's archive
	DoomedOn   [2]modelDisk // the disks a doomed prove failed on: a prove on others may pass
	Lost       int8         // under Fail, the targets whose done prove's disk is gone, one bit each
	Losts      int8         // how many disks were lost so far, at most one per run
	GoneStop   bool         // abort's latest stop names a gone disk of the done prove's
	Cur        [2]modelDisk
	Restores   int8
	Crashes    int8
	NextID     int8
	PrepLost   byte // PrepareRowLoss: the row was lost in prepare and restored released: 'N' before the journal was written, which may leave the hold's cohort and no journal, 'O' after it, which leaves the hold's cohort and journal, 'E' once hold abort on the released row ended them
	Slots      [2]modelSlot
}

func slotLess(a, b modelSlot) bool {
	if (a.Cmd == noCmd) != (b.Cmd == noCmd) {
		return b.Cmd == noCmd // an active process sorts first, so a one-process scenario keeps it in its one slot
	}
	if a.Cmd != b.Cmd {
		return a.Cmd < b.Cmd
	}
	if a.Set != b.Set {
		return a.Set < b.Set
	}
	if a.Step != b.Step {
		return a.Step < b.Step
	}
	if a.Gen != b.Gen {
		return a.Gen < b.Gen
	}
	if a.Base != b.Base {
		return fmt.Sprint(a.Base) < fmt.Sprint(b.Base)
	}
	if a.Named != b.Named {
		return fmt.Sprint(a.Named) < fmt.Sprint(b.Named)
	}
	if a.Checked != b.Checked {
		return fmt.Sprint(a.Checked) < fmt.Sprint(b.Checked)
	}
	return false
}

// canon orders the two processes, which are alike, so a state and its mirror are one state.
func canon(s modelState) modelState {
	if slotLess(s.Slots[1], s.Slots[0]) {
		s.Slots[0], s.Slots[1] = s.Slots[1], s.Slots[0]
	}
	return s
}

// ---------------------------------------------------------------- scenarios

// modelScenario is one workflow as the operator drives it: the commands the operator sends in held,
// its starting states, and its processes. hold set and hold abort from prepare are every
// scenario's, and hold abort in held is every scenario's too, though only a table that names it
// admits it. Fail models a command that fails for good after its intent.
type modelScenario struct {
	Name  string
	W     Workflow
	Ops   []modelOp
	Init  []modelState
	Hub   bool
	Slots int
	Fail  bool
	// OwnRunOnly: a stop records its own run alone, LeftUnchanged's every-run rule's control.
	OwnRunOnly bool
}

type modelOp struct {
	cmd modelCmd
	set int8
}

var targets = []string{"A", "B"}

// proveLabels is each prove's label, by its set of targets.
var proveLabels = func() []string {
	out := make([]string, 1<<len(targets))
	out[0] = "hold prove (plain)"
	for set := 1; set < len(out); set++ {
		var ts []string
		for t, n := range targets {
			if set&(1<<t) != 0 {
				ts = append(ts, n)
			}
		}
		out[set] = "hold prove --restored " + strings.Join(ts, ",")
	}
	return out
}()

func (o modelOp) label() string {
	if o.cmd != cmdProve {
		return o.cmd.String()
	}
	return proveLabels[o.set]
}

// operatorCmd is the command the operator types: hold abort from prepare, and on a released row,
// is hold abort.
func operatorCmd(c modelCmd) modelCmd {
	if c == cmdAbortPrepare || c == cmdAbortReleased || c == cmdAbortNoJournal {
		return cmdAbort
	}
	return c
}

func scenarios(r modelRules, fail, ownRunOnly bool) []modelScenario {
	slots, hubSlots := 2, 2
	if oneHubProcess[r.Name] {
		hubSlots = 1
	}
	prep := modelState{Row: 'P'}
	ops := func(cs ...modelCmd) []modelOp {
		var out []modelOp
		for _, c := range cs {
			out = append(out, modelOp{cmd: c})
		}
		return append(out, modelOp{cmd: cmdAbort}, modelOp{cmd: cmdRelease})
	}
	// Under BackupCommand the recreate and the restore each follow buckets backup, which the
	// recreate requires.
	recreate, restore := ops(cmdBucketsRecreate), ops(cmdBucketsRestore)
	if r.BackupCommand {
		recreate, restore = ops(cmdBackup, cmdBucketsRecreate), ops(cmdBackup, cmdBucketsRestore)
	}
	out := []modelScenario{
		{Name: "cutover", W: WorkflowCutover, Slots: slots, Init: []modelState{prep, {Row: 'P', NoStart: true}},
			Ops: ops(cmdCreateBucket, cmdReplay, cmdConvert, cmdCensus)},
		{Name: "rollback", W: WorkflowRollback, Slots: slots, Init: []modelState{prep, {Row: 'P', Unresolved: true}},
			Ops: ops(cmdRestorePrepare, cmdRestoreKey, cmdRestoreFinish)},
		{Name: "selection-recovery", W: WorkflowSelectionRecovery, Slots: slots, Init: []modelState{prep},
			Ops: ops(cmdCreateBucketAfterLoss, cmdReplay, cmdReplayFinal)},
		{Name: "buckets-create", W: WorkflowBucketsCreate, Slots: slots, Init: []modelState{prep}, Ops: ops(cmdBucketsCreate)},
		{Name: "buckets-recreate", W: WorkflowBucketsRecreate, Slots: slots, Init: []modelState{prep}, Ops: recreate},
		{Name: "buckets-restore", W: WorkflowBucketsRestore, Slots: slots, Init: []modelState{prep}, Ops: restore},
		{Name: "streams-update", W: WorkflowStreamsUpdate, Slots: slots, Init: []modelState{prep}, Ops: ops(cmdStreamsUpdate)},
		{Name: "streams-census", W: WorkflowStreamsCensus, Slots: slots, Init: []modelState{prep}, Ops: ops(cmdCensus)},
	}
	hub := modelScenario{Name: "hub-restore", W: WorkflowHubRestore, Hub: true, Slots: hubSlots,
		Ops: []modelOp{{cmdProve, 1}, {cmdProve, 2}, {cmdProve, 3}, {cmd: cmdCensus}, {cmd: cmdAbort}, {cmd: cmdRelease}}}
	if r.PlainReprove { // the plain prove of the plans before v55, which named no target
		hub.Ops = append([]modelOp{{cmdProve, 0}}, hub.Ops...)
	}
	hub.Init = []modelState{{Row: 'P', Cur: [2]modelDisk{{ID: 1}, {ID: 2}}, NextID: 3}}
	out = append(out, hub)
	for i := range out {
		out[i].Fail, out[i].OwnRunOnly = fail, ownRunOnly
		if r.SetCommand {
			out[i].Ops = append([]modelOp{{cmd: cmdSet}}, out[i].Ops...)
		}
	}
	return out
}

// toDone builds the done record that the journal stands for, in an order whose readings by the
// package are the journal's. It reads nothing of a state but its journal.
func toDone(sc modelScenario, j modelJournal) Done {
	d := Done{Workflow: sc.W, Complete: j.Complete}
	// Under WayOut, each command whose latest entry is a stop: one written before the latest done
	// prove just before that prove's done entry (StopBefore), and every other after the done
	// entries and before the intents (StopLatest), so the record keeps the order hold abort's
	// admission in hub-restore reads. What hold release's rule reads of the stops of other commands
	// than hold set and hold release is whether every one left nothing changed (Left), and every
	// such stop carries that answer. Both are set only in the rule sets since the bundle
	// (stopsRecorded), so the bundle's profiles keep the bundle's record.
	stopsOf := func(set modelCmds) []Operation {
		var out []Operation
		for c := range set.all() {
			o := Operation{Command: c.String(), State: StateStop}
			if j.Left == 'U' {
				o.Outputs = map[string]string{stopLeftKey: LeftUnchanged}
			}
			out = append(out, o)
		}
		return out
	}
	stops, before := stopsOf(j.StopLatest), stopsOf(j.StopBefore)
	done := func(c modelCmd, out map[string]string) {
		d.Ops = append(d.Ops, Operation{Command: c.String(), State: StateDone, Outputs: out})
	}
	for c := range j.Ever.all() {
		if c == cmdProve || c == j.Last || (j.Proved && j.Since.has(c)) {
			continue // the boundary, the last entry and the epoch's entries come below, in order
		}
		done(c, nil)
	}
	if j.Proved {
		out := map[string]string{}
		for tg, n := range targets {
			if j.ProvenSet&(1<<tg) != 0 {
				out["disk_id."+n] = itoa(j.Proven[tg])
			}
		}
		d.Ops = append(d.Ops, before...)
		done(cmdProve, out)
		for c := range j.Since.all() {
			if c != j.Last {
				done(c, nil)
			}
		}
	}
	if j.Last != noCmd && !(j.Proved && j.Last == cmdProve) {
		done(j.Last, nil)
	}
	d.Ops = append(d.Ops, stops...)
	for c := range j.Intent.all() {
		d.Ops = append(d.Ops, Operation{Command: c.String(), State: StateIntent})
	}
	if sc.Hub && j.Rec {
		d.Step1 = map[string]string{}
		for tg, n := range targets {
			if j.Step1[tg].ID != 0 {
				d.Step1["step1_disk_id."+n] = itoa(j.Step1[tg].ID)
			}
		}
	}
	return d
}

// itoa is a state's number as the record writes it.
func itoa(n int8) string { return strconv.Itoa(int(n)) }

// expected is a target's expected disk id: the latest prove's for a target it names, else the
// step-1 map's (0 when it lacks it).
func (j modelJournal) expected(tg int) int8 {
	if j.ProvenSet&(1<<tg) != 0 {
		return j.Proven[tg]
	}
	return j.Step1[tg].ID
}

// disksMoved is the command's reading of the disks cur against the expected map, or v59's against
// Proven's named targets alone.
func disksMoved(r modelRules, j modelJournal, cur [2]modelDisk) bool {
	if !j.Proved {
		return false
	}
	for tg := range targets {
		if j.ProvenSet&(1<<tg) == 0 && !r.ExpectedMap {
			continue
		}
		if cur[tg].ID != j.expected(tg) {
			return true
		}
	}
	return false
}

// modelAsk is a request of Admit as the model's transitions make it: a modelRequest, with its
// command by index.
type modelAsk struct {
	cmd                            modelCmd
	disksMoved, rowReleased, plain bool
}

func (a modelAsk) request() modelRequest {
	return modelRequest{Request: Request{Command: a.cmd.String(), DisksMoved: a.disksMoved, RowReleased: a.rowReleased},
		Plain: a.plain}
}

func request(r modelRules, s modelState, o modelOp) modelAsk {
	return modelAsk{cmd: o.cmd, disksMoved: o.cmd == cmdProve && disksMoved(r, s.modelJournal, s.Cur),
		plain: o.cmd == cmdProve && o.set == 0}
}

// a2 is the operator's rule A2: a command is not started again while a process still runs it.
func a2(s modelState, c modelCmd) bool {
	for _, sl := range s.Slots {
		if sl.Cmd != noCmd && operatorCmd(sl.Cmd) == operatorCmd(c) {
			return false
		}
	}
	return true
}

// admitted is Admit's decision on a held row, and A2.
func admitted(r modelRules, t modelTable, sc modelScenario, s modelState, o modelOp) bool {
	return s.Row == 'H' && a2(s, o.cmd) && t.admits(sc, s.modelJournal, request(r, s, o))
}

// otherPendingAdmitted is v53's NEW-P check: another command's intent is pending, and the table
// still admits it.
func otherPendingAdmitted(r modelRules, t modelTable, sc modelScenario, s modelState, final modelCmd) bool {
	for c := range s.Intent.all() {
		if c == final || c == cmdRelease {
			continue
		}
		if t.admits(sc, s.modelJournal, modelAsk{cmd: c, disksMoved: disksMoved(r, s.modelJournal, s.Cur)}) {
			return true
		}
	}
	return false
}
