package holdrecord

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------- state

// modelDisk is one target's disk: its numeric id, and where its data came from (0 the original,
// 'H' the hold's snapshot, 'F' another snapshot). In the step-1 map, ID 0 is a target the map
// lacks.
type modelDisk struct {
	ID   int
	Snap byte
}

// modelSlot is one operator process: the command it runs, its step (0 admitted with the intent not
// yet written, which only a two-write admission has, then the command's own steps), and the
// readings the command keeps while it runs.
type modelSlot struct {
	Cmd     string
	Set     int // hold prove's --restored targets, one bit per target
	Step    int
	Gen     int          // the row's gen that hold release or hold abort read, which its row move carries
	Base    [2]modelDisk // the step-1 disks the command compares with: the recorded map, or its own reading
	Named   [2]int       // the disks a prove names
	Checked [2]int       // the disks a prove's check passed on, which its done entry records for the targets it names
}

// modelState is one state of a workflow. The journal is kept as what the package's rules read:
// which commands have a done entry (Ever), which have one after the latest done prove (Since), the
// last done entry, the latest prove's disks, which commands' latest entry is an intent, and, under
// WayOut, what the stops of commands other than hold set and hold release left (Left). toDone
// builds the record from it, so a Repeatable command can run without limit in a finite space.
type modelState struct {
	Row        byte // 'P' prepare, 'H' held, 'R' released
	Rec        bool // the done record exists
	Ever       string
	Since      string
	Last       string
	Proved     bool
	Proven     [2]int // the latest done prove's disk ids, for the targets it names
	ProvenSet  int    // the targets the latest done prove names
	Intent     string
	Complete   bool
	HoldGen    int          // the row's gen at completion
	RowGen     int          // the row restores so far
	Unresolved bool         // rollback: a logical name with no start, which only restore --key resolves
	NoStart    bool         // cutover: a cohort member with no safe start, which only replay --key resolves
	Doomed     string       // a command that failed for good after its intent: every later run of it fails
	StopLatest string       // in the rule sets since the bundle (stopsRecorded), the commands whose latest entry is a stop written after the latest done prove (or with none done), which hold abort's admission reads
	StopBefore string       // likewise, the commands whose latest entry is a stop written before the latest done prove
	Left       byte         // likewise, the stops of other commands than hold set and release: 0 none, 'U' each left nothing changed, 'C' one changed or recorded nothing
	Wrote      string       // under Fail, the commands a run of which changed something outside the journal: the hub, not buckets backup's archive
	DoomedOn   [2]modelDisk // the disks a doomed prove failed on: a prove on others may pass
	Lost       int          // under Fail, the targets whose done prove's disk is gone, one bit each
	Losts      int          // how many disks were lost so far, at most one per run
	GoneStop   bool         // abort's latest stop names a gone disk of the done prove's
	Cur        [2]modelDisk
	Step1      [2]modelDisk
	Restores   int
	Crashes    int
	NextID     int
	PrepLost   byte // PrepareRowLoss: the row was lost in prepare and restored released: 'N' before the journal was written, which may leave the hold's cohort and no journal, 'O' after it, which leaves the hold's cohort and journal, 'E' once hold abort on the released row ended them
	Slots      [2]modelSlot
}

func list(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, "|")
}

func addSet(s, c string) string {
	l := list(s)
	for _, x := range l {
		if x == c {
			return s
		}
	}
	l = append(l, c)
	sort.Strings(l)
	return strings.Join(l, "|")
}

func delSet(s, c string) string {
	var out []string
	for _, x := range list(s) {
		if x != c {
			out = append(out, x)
		}
	}
	return strings.Join(out, "|")
}

func inSet(s, c string) bool {
	for _, x := range list(s) {
		if x == c {
			return true
		}
	}
	return false
}

func slotLess(a, b modelSlot) bool {
	if (a.Cmd == "") != (b.Cmd == "") {
		return b.Cmd == "" // an active process sorts first, so a one-process scenario keeps it in its one slot
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
	cmd string
	set int
}

const (
	cmdSet            = CommandSet
	cmdAbortPrepare   = "hold abort (prepare)"              // hold abort from prepare: it deletes the cohort and moves the row, and writes no entry
	cmdAbortReleased  = "hold abort (released)"             // hold abort on a row lost in prepare and restored released: its intent, the cohort's delete, its done
	cmdAbortNoJournal = "hold abort (released, no journal)" // the same with no journal: the cohort's delete alone, and no entry
	cmdSnapRestore    = "snapshot restore"                  // v59's first hub-restore command, written by hold set's process
)

var targets = []string{"A", "B"}

func (o modelOp) label() string {
	if o.cmd != CommandProve {
		return o.cmd
	}
	if o.set == 0 {
		return "hold prove (plain)"
	}
	var ts []string
	for t, n := range targets {
		if o.set&(1<<t) != 0 {
			ts = append(ts, n)
		}
	}
	return "hold prove --restored " + strings.Join(ts, ",")
}

// operatorCmd is the command the operator types: hold abort from prepare, and on a released row,
// is hold abort.
func operatorCmd(c string) string {
	if c == cmdAbortPrepare || c == cmdAbortReleased || c == cmdAbortNoJournal {
		return CommandAbort
	}
	return c
}

func scenarios(r modelRules, fail, ownRunOnly bool) []modelScenario {
	slots, hubSlots := 2, 2
	if oneHubProcess[r.Name] {
		hubSlots = 1
	}
	prep := modelState{Row: 'P'}
	ops := func(cs ...string) []modelOp {
		var out []modelOp
		for _, c := range cs {
			out = append(out, modelOp{cmd: c})
		}
		return append(out, modelOp{cmd: CommandAbort}, modelOp{cmd: CommandRelease})
	}
	// Under BackupCommand the recreate and the restore each follow buckets backup, which the
	// recreate requires.
	recreate, restore := ops("buckets recreate"), ops("buckets restore")
	if r.BackupCommand {
		recreate, restore = ops(commandBackup, "buckets recreate"), ops(commandBackup, "buckets restore")
	}
	out := []modelScenario{
		{Name: "cutover", W: WorkflowCutover, Slots: slots, Init: []modelState{prep, {Row: 'P', NoStart: true}},
			Ops: ops("create-bucket", "replay", "convert", "streams census")},
		{Name: "rollback", W: WorkflowRollback, Slots: slots, Init: []modelState{prep, {Row: 'P', Unresolved: true}},
			Ops: ops("restore --prepare", "restore --key", "restore --finish")},
		{Name: "selection-recovery", W: WorkflowSelectionRecovery, Slots: slots, Init: []modelState{prep},
			Ops: ops("create-bucket --after-loss", "replay", "replay --final")},
		{Name: "buckets-create", W: WorkflowBucketsCreate, Slots: slots, Init: []modelState{prep}, Ops: ops("buckets create")},
		{Name: "buckets-recreate", W: WorkflowBucketsRecreate, Slots: slots, Init: []modelState{prep}, Ops: recreate},
		{Name: "buckets-restore", W: WorkflowBucketsRestore, Slots: slots, Init: []modelState{prep}, Ops: restore},
		{Name: "streams-update", W: WorkflowStreamsUpdate, Slots: slots, Init: []modelState{prep}, Ops: ops("streams update")},
		{Name: "streams-census", W: WorkflowStreamsCensus, Slots: slots, Init: []modelState{prep}, Ops: ops("streams census")},
	}
	hub := modelScenario{Name: "hub-restore", W: WorkflowHubRestore, Hub: true, Slots: hubSlots,
		Ops: []modelOp{{CommandProve, 1}, {CommandProve, 2}, {CommandProve, 3}, {cmd: "streams census"}, {cmd: CommandAbort},
			{cmd: CommandRelease}}}
	if r.PlainReprove { // the plain prove of the plans before v55, which named no target
		hub.Ops = append([]modelOp{{CommandProve, 0}}, hub.Ops...)
	}
	hub.Init = []modelState{{Row: 'P', Cur: [2]modelDisk{{ID: 1}, {ID: 2}}, NextID: 3}}
	out = append(out, hub)
	for i := range out {
		out[i].Fail, out[i].OwnRunOnly = fail, ownRunOnly
	}
	return out
}

// toDone builds the done record that the state stands for, in an order whose readings by the
// package are the state's.
func toDone(sc modelScenario, s modelState) Done {
	d := Done{Workflow: sc.W, Complete: s.Complete}
	// Under WayOut, each command whose latest entry is a stop: one written before the latest done
	// prove just before that prove's done entry (StopBefore), and every other after the done
	// entries and before the intents (StopLatest), so the record keeps the order hold abort's
	// admission in hub-restore reads. What hold release's rule reads of the stops of other commands
	// than hold set and hold release is whether every one left nothing changed (Left), and every
	// such stop carries that answer. Both are set only in the rule sets since the bundle
	// (stopsRecorded), so the bundle's profiles keep the bundle's record.
	stopsOf := func(set string) []Operation {
		var out []Operation
		for _, c := range list(set) {
			o := Operation{Command: c, State: StateStop}
			if s.Left == 'U' {
				o.Outputs = map[string]string{stopLeftKey: LeftUnchanged}
			}
			out = append(out, o)
		}
		return out
	}
	stops, before := stopsOf(s.StopLatest), stopsOf(s.StopBefore)
	done := func(c string, out map[string]string) {
		d.Ops = append(d.Ops, Operation{Command: c, State: StateDone, Outputs: out})
	}
	for _, c := range list(s.Ever) {
		if c == CommandProve || c == s.Last || (s.Proved && inSet(s.Since, c)) {
			continue // the boundary, the last entry and the epoch's entries come below, in order
		}
		done(c, nil)
	}
	if s.Proved {
		out := map[string]string{}
		for tg, n := range targets {
			if s.ProvenSet&(1<<tg) != 0 {
				out["disk_id."+n] = strconv.Itoa(s.Proven[tg])
			}
		}
		d.Ops = append(d.Ops, before...)
		done(CommandProve, out)
		for _, c := range list(s.Since) {
			if c != s.Last {
				done(c, nil)
			}
		}
	}
	if s.Last != "" && !(s.Proved && s.Last == CommandProve) {
		done(s.Last, nil)
	}
	d.Ops = append(d.Ops, stops...)
	for _, c := range list(s.Intent) {
		d.Ops = append(d.Ops, Operation{Command: c, State: StateIntent})
	}
	if sc.Hub && s.Rec {
		d.Step1 = map[string]string{}
		for tg, n := range targets {
			if s.Step1[tg].ID != 0 {
				d.Step1["step1_disk_id."+n] = strconv.Itoa(s.Step1[tg].ID)
			}
		}
	}
	return d
}

// expected is a target's expected disk id: the latest prove's for a target it names, else the
// step-1 map's (0 when it lacks it).
func expected(s modelState, tg int) int {
	if s.ProvenSet&(1<<tg) != 0 {
		return s.Proven[tg]
	}
	return s.Step1[tg].ID
}

// disksMoved is the command's reading against the expected map, or v59's against Proven's named
// targets alone.
func disksMoved(r modelRules, s modelState) bool {
	if !s.Proved {
		return false
	}
	for tg := range targets {
		if s.ProvenSet&(1<<tg) == 0 && !r.ExpectedMap {
			continue
		}
		if s.Cur[tg].ID != expected(s, tg) {
			return true
		}
	}
	return false
}

func request(r modelRules, s modelState, o modelOp) modelRequest {
	return modelRequest{Request: Request{Command: o.cmd, DisksMoved: o.cmd == CommandProve && disksMoved(r, s)},
		Plain: o.cmd == CommandProve && o.set == 0}
}

// a2 is the operator's rule A2: a command is not started again while a process still runs it.
func a2(s modelState, c string) bool {
	for _, sl := range s.Slots {
		if sl.Cmd != "" && operatorCmd(sl.Cmd) == operatorCmd(c) {
			return false
		}
	}
	return true
}

// admitted is Admit's decision on a held row, and A2.
func admitted(r modelRules, t modelTable, sc modelScenario, s modelState, o modelOp) bool {
	return s.Row == 'H' && a2(s, o.cmd) && t.decide(toDone(sc, s), request(r, s, o)) == nil
}

// otherPendingAdmitted is v53's NEW-P check: another command's intent is pending, and the table
// still admits it.
func otherPendingAdmitted(r modelRules, t modelTable, sc modelScenario, s modelState, final string) bool {
	d := toDone(sc, s)
	for _, c := range list(s.Intent) {
		if c == final || c == CommandRelease {
			continue
		}
		if t.decide(d, modelRequest{Request: Request{Command: c, DisksMoved: disksMoved(r, s)}}) == nil {
			return true
		}
	}
	return false
}
