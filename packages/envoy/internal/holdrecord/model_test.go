package holdrecord

// This file is the executable model of the writer hold's workflows, moved into the package whose
// journal it checks. Its admission and journal rules are this package's own: for the current rules
// each admission is the package's Decide on the record the model's state gives, and the step-1
// map, the proofs, the epochs, the expected disks and the moved answer the model reasons from are
// checked against the package's Step1, Proven, Epoch, Expected and Moved at every hub-restore state
// it reaches, so the model and the code it models cannot drift apart.
//
// Every workflow starts from its true initial state: the row `prepare` with the hold's id, and no
// `done` record. Every later state is reached only through modeled commands: `hold set` with each
// of its writes and refusals, `hold abort` from `prepare`, and the workflow's own commands, each
// with every refusal the hold's design states for it, and a crash between any two of a command's
// writes. In hub-restore a restore lands on any target as an outside event, from the hold's
// snapshot or another one, and in every workflow the row may be lost and restored `held` at a new
// gen (a database admin deletes it, and the operator restores it). A prove names any non-empty set
// of targets, its done entry records those alone, and the step-1 map is what hold set's reads gave
// it. For the rule added since the frozen bundle (WayOut), a buckets command may also fail for good
// after its intent, before its change or after it, and every later run of it fails again; under
// WayOut its stop records what it left, and hold abort, admitted in every workflow once no intent
// is pending, ends the hold.
//
// It explores every reachable state breadth-first, for every workflow, with two operator
// processes. An admission is what the code does: with AtomicAdmit, one compare-and-swap step that
// decides and writes the intent, and without it, a read that decides and a later write of the
// intent. It checks these invariants at every state of every workflow:
//
//   - liveness: every state whose row is not released can reach released through admitted
//     operator steps alone, with no hand-signed PUT;
//   - one command at a time: no two processes are past their intent at once (hold set from its
//     row move on);
//   - no command outside its hold: the row is never released while another process is past its
//     intent;
//   - no false completion: complete is true only when the workflow's final command is the last
//     done entry and no command's intent is pending (but hold release's own, and hold abort's
//     after a done prove, whose one effect, the row move, has not happened while the row is held);
//   - hub-restore's safety: abort never releases over a restored disk, and a disk is judged only by
//     its step-1 id as recorded, and never against a target the step-1 map lacks.
//
// The current rules must find no counterexample, and every earlier rule set and every rule set
// with one fix removed must find a counterexample of each class it names, which the test asserts
// by the class's name. The frozen bundle's profiles do not model the failing buckets command, so
// their state and counterexample counts are those the bundle gave, and a port that changed what the
// model explores fails here too; the current rules and their control model it, with pinned counts.

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The earlier versions' rules. Their hub-restore runs with one operator process, as v53's model
// did: the two-process race is shown by their generic workflows, and by the later controls in every
// workflow. v59 is the rule set the plan's versions v55 to v59 carried.
var (
	v49 = rules{Name: "v49", PlainReprove: true}
	v52 = rules{Name: "v52", PlainReprove: true, AbortMoveBeforeDone: true, AfterCompleteStrict: true}
	v53 = rules{Name: "v53", Step1Recorded: true, ProveEpochs: true, PlainReprove: true, AbortMoveBeforeDone: true,
		AfterCompleteStrict: true, SelRecFinalRepeatable: true, RestoreKeyInTable: true, PendingBlocksComplete: true}
	v59 = rules{Name: "v59", Step1Recorded: true, ProveEpochs: true, AbortMoveBeforeDone: true, AfterCompleteStrict: true,
		SelRecFinalRepeatable: true, RestoreKeyInTable: true, AtomicAdmit: true, Serialize: true, StopEntries: true}
	// v62 is the frozen bundle's rule set: the current rules without WayOut, the one rule added
	// since. Its profile and every profile with one of its fixes removed run the bundle's own
	// scenarios, so they reproduce the bundle's counts.
	v62 = func() rules { r := currentRules; r.Name, r.WayOut = "v62", false; return r }()
)

var oneHubProcess = map[string]bool{"v49": true, "v49+Step1Recorded": true, "v52": true, "v53": true, "v53-PendingBlocksComplete": true}

// The counterexample classes. A control names the classes its run must find.
const (
	classTwoWrites   = "ITEM-1 (admission and intent as two writes: a command admitted before the completion writes its intent after it, and the hold is stranded)"
	classTwoRun      = "ITEM-1 (two commands past their intent at once)"
	classOutside     = "ITEM-1 (the row is released while another command runs)"
	classPending     = "NEW-P (complete while another command's intent is pending)"
	classStop        = "STOP (a refusal without a stop entry leaves an intent that no command can end)"
	classAbortDone   = "the gate on v49 (abort's done entry before its row move)"
	classAbortCrash  = "the gate on v49 (a crash between abort's done entry and its row move)"
	classR27A        = "R27-A ("
	classR27ASel     = "R27-A (selection-recovery's second cycle: replay --final cannot run again)"
	classR27CReread  = "R27-C (step 1's disk ids are re-read, not recorded)"
	classR27CNoProve = "R27-C (no correct prove can pass against a re-read step 1)"
	classR27E        = "R27-E (a restore lands after a prove's check or its done entry)"
	classR27F        = "R27-F (a logical name with no start needs restore --key, which the table does not name)"
	classR28ASet     = "R28-A (hold set is no command: a crash between its writes, or a command run during its restart, strands the hold)"
	classR28AConv    = "R28-A (convert stops for a name with no safe start, and ErrBusy refuses the replay that gives it one)"
	classR28APrep    = "R28-A (restore --prepare stops for a name with no start, and restore --key requires its done entry)"
	classR28B        = "R28-B (a restore on a target the latest prove did not name, after completion: no prove is admitted)"
	classR28C        = "R28-C (a row restored after completion: release's hold_gen check refuses for good)"
	classR28H        = "R28-H (a disk judged against a target the step-1 map lacks)"
	classWayOut      = "WAY-OUT (a buckets command that fails for good after its intent strands the hold: no command the table admits ends it)"
)

// modelProfile is a rule set and the classes its run must find: none for the current rules, and
// each control's own. Fail models a buckets command that fails for good after its intent, which
// the frozen bundle did not model, so only the profiles of the rule added since set it.
type modelProfile struct {
	rules
	Want []string
	Fail bool
}

func modelProfiles() []modelProfile {
	v49s := v49
	v49s.Name, v49s.Step1Recorded = "v49+Step1Recorded", true
	v53p := v53
	v53p.Name, v53p.PendingBlocksComplete = "v53-PendingBlocksComplete", false
	out := []modelProfile{
		{v49, []string{classAbortDone}, false},
		{v49s, []string{classR27E}, false},
		{v52, []string{classR27CNoProve}, false},
		{v53, []string{classTwoWrites}, false},
		{v53p, []string{classPending}, false},
		{v59, []string{classR28ASet, classR28B}, false},
		{v62, nil, false},
	}
	type ab struct {
		name string
		f    func(*rules)
		want []string
	}
	for _, a := range []ab{
		{"Step1Recorded", func(r *rules) { r.Step1Recorded = false }, []string{classR27CReread}},
		{"ProveEpochs", func(r *rules) { r.ProveEpochs = false }, []string{classR27E}},
		{"AbortMoveBeforeDone", func(r *rules) { r.AbortMoveBeforeDone = false }, []string{classAbortDone}},
		{"AfterCompleteStrict", func(r *rules) { r.AfterCompleteStrict = false }, []string{classR27A}},
		{"SelRecFinalRepeatable", func(r *rules) { r.SelRecFinalRepeatable = false }, []string{classR27ASel}},
		{"RestoreKeyInTable", func(r *rules) { r.RestoreKeyInTable = false }, []string{classR27F}},
		{"AtomicAdmit", func(r *rules) { r.AtomicAdmit = false }, []string{classTwoWrites}},
		{"Serialize", func(r *rules) { r.Serialize = false }, []string{classTwoRun}},
		{"StopEntries", func(r *rules) { r.StopEntries = false }, []string{classStop}},
		{"SetCommand", func(r *rules) { r.SetCommand = false }, []string{classR28ASet}},
		{"Step1Whole", func(r *rules) { r.Step1Whole = false }, []string{classR28H}},
		{"ExpectedMap", func(r *rules) { r.ExpectedMap = false }, []string{classR28B}},
		{"ConvertStoppable", func(r *rules) { r.ConvertStoppable = false }, []string{classR28AConv}},
		{"PrepareMarksUnresolved", func(r *rules) { r.PrepareMarksUnresolved = false }, []string{classR28APrep}},
		{"CompletionSurvivesRowRestore", func(r *rules) { r.CompletionSurvivesRowRestore = false }, []string{classR28C}},
	} {
		r := v62
		r.Name = v62.Name + "-" + a.name
		a.f(&r)
		out = append(out, modelProfile{r, a.want, false})
	}
	// The current rules, with a buckets command that fails for good modeled, and their control:
	// the same rules without WayOut, which strand the hold.
	noWayOut := currentRules
	noWayOut.Name, noWayOut.WayOut = currentRules.Name+"-WayOut", false
	return append(out, modelProfile{currentRules, nil, true}, modelProfile{noWayOut, []string{classWayOut}, true})
}

// modelTable is what the model's transitions ask of a rule set's table. For the current rules it
// is the package's own Decide, Final and Stoppable, which is what the hold's commands call; for an
// earlier rule set or a control it is that set's table.
type modelTable struct {
	decide    func(d Done, req modelRequest) error
	final     func(w Workflow, command string) bool
	stoppable func(w Workflow, command string) bool
}

func tableOf(r rules) modelTable {
	if r == currentRules {
		return modelTable{
			decide: func(d Done, req modelRequest) error {
				if req.Plain {
					return fmt.Errorf("the current rules model no plain prove, and Decide has none")
				}
				return Decide(d, req.Request)
			},
			final:     Final,
			stoppable: Stoppable,
		}
	}
	t := tableFor(r)
	return modelTable{decide: t.Decide, final: t.Final, stoppable: t.Stoppable}
}

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
	HoldGen    int    // the row's gen at completion
	RowGen     int    // the row restores so far
	Unresolved bool   // rollback: a logical name with no start, which only restore --key resolves
	NoStart    bool   // cutover: a cohort member with no safe start, which only replay --key resolves
	Doomed     bool   // a buckets command failed for good after its intent: every run of it fails
	StopLatest string // under WayOut, the commands whose latest entry is a stop, which hold abort's admission reads
	Left       byte   // under WayOut, the stops of other commands than hold set and release: 0 none, 'U' each left nothing changed, 'C' one changed or recorded nothing
	Cur        [2]modelDisk
	Step1      [2]modelDisk
	Restores   int
	Crashes    int
	NextID     int
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
// admits it. Fail models a buckets command that fails for good after its intent.
type modelScenario struct {
	Name  string
	W     Workflow
	Ops   []modelOp
	Init  []modelState
	Hub   bool
	Slots int
	Fail  bool
}

type modelOp struct {
	cmd string
	set int
}

const (
	cmdSet          = CommandSet
	cmdAbortPrepare = "hold abort (prepare)" // hold abort from prepare: it deletes the cohort and moves the row, and writes no entry
	cmdSnapRestore  = "snapshot restore"     // v59's first hub-restore command, written by hold set's process
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

// operatorCmd is the command the operator types: hold abort from prepare is hold abort.
func operatorCmd(c string) string {
	if c == cmdAbortPrepare {
		return CommandAbort
	}
	return c
}

// bucketsCommand reports whether c is one of the buckets commands, the commands Fail lets fail for
// good after their intent: a restore whose snapshot can never be read, a recreate whose create is
// refused after its delete.
func bucketsCommand(c string) bool {
	return c == "buckets create" || c == "buckets recreate" || c == "buckets restore"
}

func scenarios(r rules, fail bool) []modelScenario {
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
	out := []modelScenario{
		{Name: "cutover", W: WorkflowCutover, Slots: slots, Init: []modelState{prep, {Row: 'P', NoStart: true}},
			Ops: ops("create-bucket", "replay", "convert", "streams census")},
		{Name: "rollback", W: WorkflowRollback, Slots: slots, Init: []modelState{prep, {Row: 'P', Unresolved: true}},
			Ops: ops("restore --prepare", "restore --key", "restore --finish")},
		{Name: "selection-recovery", W: WorkflowSelectionRecovery, Slots: slots, Init: []modelState{prep},
			Ops: ops("create-bucket --after-loss", "replay", "replay --final")},
		{Name: "buckets-create", W: WorkflowBucketsCreate, Slots: slots, Init: []modelState{prep}, Ops: ops("buckets create")},
		{Name: "buckets-recreate", W: WorkflowBucketsRecreate, Slots: slots, Init: []modelState{prep}, Ops: ops("buckets recreate")},
		{Name: "buckets-restore", W: WorkflowBucketsRestore, Slots: slots, Init: []modelState{prep}, Ops: ops("buckets restore")},
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
		out[i].Fail = fail
	}
	return out
}

// toDone builds the done record that the state stands for, in an order whose readings by the
// package are the state's.
func toDone(sc modelScenario, s modelState) Done {
	d := Done{Workflow: sc.W, Complete: s.Complete}
	// Under WayOut, each command whose latest entry is a stop, after its done entries and before the
	// intents. What hold release's rule reads of the stops of other commands than hold set and hold
	// release is whether every one left nothing changed (Left), and every such stop carries that
	// answer. StopLatest is set only under WayOut, so every other rule set's record is as before.
	var stops []Operation
	for _, c := range list(s.StopLatest) {
		o := Operation{Command: c, State: stateStop}
		if s.Left == 'U' {
			o.Outputs = map[string]string{stopLeftKey: LeftUnchanged}
		}
		stops = append(stops, o)
	}
	done := func(c string, out map[string]string) {
		d.Ops = append(d.Ops, Operation{Command: c, State: stateDone, Outputs: out})
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
		d.Ops = append(d.Ops, Operation{Command: c, State: stateIntent})
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
func disksMoved(r rules, s modelState) bool {
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

func request(r rules, s modelState, o modelOp) modelRequest {
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
func admitted(r rules, t modelTable, sc modelScenario, s modelState, o modelOp) bool {
	return s.Row == 'H' && a2(s, o.cmd) && t.decide(toDone(sc, s), request(r, s, o)) == nil
}

// otherPendingAdmitted is v53's NEW-P check: another command's intent is pending, and the table
// still admits it.
func otherPendingAdmitted(r rules, t modelTable, sc modelScenario, s modelState, final string) bool {
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

// ---------------------------------------------------------------- transitions

type modelEdge struct {
	to     modelState
	label  string
	op     bool   // an operator step, which liveness may use
	unsafe string // a safety violation on this step
}

func released(s modelState, abort bool) string {
	if abort && (s.Cur[0].Snap != 0 || s.Cur[1].Snap != 0) {
		return "abort released over a restored disk"
	}
	return ""
}

// hold set's steps. It runs past its intent, in held, from its row move on.
const (
	setRead    = 1 + iota // step 1's reads and the record's insert
	setPre                // the preflight
	setIntent             // its intent, by Admit, with the row prepare (SetCommand)
	setMove               // the row's move from prepare to held
	setRestore            // v59 hub-restore: snapshot restore's intent
	setAge                // v59 hub-restore: the age check, then snapshot restore's done
	setSteps              // steps 2 to 6 in held, then its done entry (SetCommand)
)

func runs(sl modelSlot) bool {
	switch sl.Cmd {
	case "", cmdAbortPrepare:
		return false
	case cmdSet:
		return sl.Step >= setRestore
	}
	return sl.Step >= 1
}

func running(s modelState) int {
	n := 0
	for _, sl := range s.Slots {
		if runs(sl) {
			n++
		}
	}
	return n
}

// othersRunning reports whether a process other than k is past its intent.
func othersRunning(s modelState, k int) bool {
	for j, sl := range s.Slots {
		if j != k && runs(sl) {
			return true
		}
	}
	return false
}

func idle(s modelState, k int) modelState { s.Slots[k] = modelSlot{}; return s }

func next(r rules, t modelTable, sc modelScenario, s modelState, bound int) []modelEdge {
	var out []modelEdge
	for k := range sc.Slots {
		sl := s.Slots[k]
		if sl.Cmd == "" {
			switch s.Row {
			case 'P':
				if a2(s, cmdSet) && (!r.SetCommand || !s.Rec ||
					t.decide(toDone(sc, s), modelRequest{Request: Request{Command: cmdSet}}) == nil) {
					ns := s
					ns.Slots[k] = modelSlot{Cmd: cmdSet, Step: setRead}
					out = append(out, modelEdge{to: ns, label: "hold set: started", op: true})
				}
				if a2(s, cmdAbortPrepare) {
					ns := s
					ns.Slots[k] = modelSlot{Cmd: cmdAbortPrepare, Step: 1}
					out = append(out, modelEdge{to: ns, label: "hold abort from prepare: started", op: true})
				}
			case 'H':
				all := sc.Ops
				if r.SetCommand {
					all = append([]modelOp{{cmd: cmdSet}}, sc.Ops...)
				}
				for _, o := range all {
					if !admitted(r, t, sc, s, o) {
						continue
					}
					out = append(out, admit(r, sc, s, k, o))
				}
			}
			continue
		}
		out = append(out, steps(r, t, sc, s, k)...)
		if s.Crashes < bound {
			cs := idle(s, k)
			cs.Crashes++
			out = append(out, modelEdge{to: cs, label: "crash in " + modelOp{sl.Cmd, sl.Set}.label()})
		}
	}
	if s.Row == 'H' && s.RowGen < 1 {
		ns := s
		ns.RowGen++
		out = append(out, modelEdge{to: ns, label: "outside: the row is lost and restored held at a new gen"})
	}
	if sc.Hub && s.Row == 'H' {
		// Assumption A1: no restore lands between hold abort's or hold release's last disk check and
		// its row move. Such a restore is a restore outside any hold, which the restore procedure
		// forbids and no hold can stop.
		window := false
		for _, sl := range s.Slots {
			if (sl.Cmd == CommandAbort && sl.Step >= 2) || (sl.Cmd == CommandRelease && sl.Step == 2) {
				window = true
			}
		}
		if s.Restores < bound && !window {
			for tg, n := range targets {
				for _, snap := range []byte{'H', 'F'} {
					ns := s
					ns.Cur[tg] = modelDisk{ID: s.NextID, Snap: snap}
					ns.NextID++
					ns.Restores++
					from := map[byte]string{'H': "the hold's snapshot", 'F': "another snapshot"}[snap]
					out = append(out, modelEdge{to: ns, label: fmt.Sprintf("outside: a restore lands on %s from %s", n, from)})
				}
			}
		}
	}
	return out
}

// admit starts a command on a held row: one compare-and-swap that writes its intent, or, without
// AtomicAdmit, a read whose intent is written later.
func admit(r rules, sc modelScenario, s modelState, k int, o modelOp) modelEdge {
	ns := s
	ns.Slots[k] = modelSlot{Cmd: o.cmd, Set: o.set, Gen: s.RowGen}
	if sc.Hub {
		if r.Step1Recorded {
			ns.Slots[k].Base = s.Step1
		} else {
			ns.Slots[k].Base = s.Cur // the command's own step-1 reading, at its start
		}
		for tg := range targets {
			if o.set&(1<<tg) != 0 {
				ns.Slots[k].Named[tg] = s.Cur[tg].ID
			}
		}
	}
	first := 1
	if o.cmd == cmdSet {
		first = setSteps // its rerun in held: the row move is done, so it runs steps 2 to 6 again
	}
	lock := o.cmd != CommandRelease || r.Serialize
	switch {
	case !lock: // v53's hold release writes no entry: its check is its admission
		ns.Slots[k].Step = first
		return modelEdge{to: ns, label: o.label() + ": checked", op: true}
	case r.AtomicAdmit:
		ns = writeIntent(ns, o.cmd)
		ns.Slots[k].Step = first
		return modelEdge{to: ns, label: o.label() + ": admitted, intent written", op: true}
	}
	return modelEdge{to: ns, label: o.label() + ": admitted", op: true}
}

func writeIntent(s modelState, c string) modelState {
	s.Intent = addSet(s.Intent, c)
	s.StopLatest = delSet(s.StopLatest, c)
	if c != CommandRelease {
		s.Complete = false
	}
	return s
}

// finish writes the command's done entry, and completes the record in the same write when the
// command is final.
func finish(t modelTable, sc modelScenario, s modelState, k int) modelState {
	sl := s.Slots[k]
	c := sl.Cmd
	s.Ever, s.Last = addSet(s.Ever, c), c
	if c == CommandProve {
		if sl.Set != 0 { // a plain prove keeps the named set and records the same disks again
			s.ProvenSet = sl.Set
		}
		s.Proved, s.Since = true, ""
		for tg := range targets {
			s.Proven[tg] = 0
			if s.ProvenSet&(1<<tg) != 0 {
				s.Proven[tg] = sl.Checked[tg]
			}
		}
	} else {
		s.Since = addSet(s.Since, c)
	}
	s.Intent = delSet(s.Intent, c)
	s.StopLatest = delSet(s.StopLatest, c)
	if t.final(sc.W, c) {
		s.Complete, s.HoldGen = true, s.RowGen
	}
	return idle(s, k)
}

// refuse ends a command whose own check refused: with a stop entry when the command is Stoppable,
// which keeps complete, else with its intent left. rerun marks a refusal whose stated way out is the
// command's own rerun, which ErrBusy admits. Its stop records nothing of what the command left,
// which counts as changed (refuseLeft records it).
func refuse(r rules, t modelTable, sc modelScenario, s modelState, k int, why string, rerun bool) modelEdge {
	return refuseLeft(r, t, sc, s, k, why, rerun, 'C')
}

// refuseLeft is refuse whose stop records what the command left: 'U' nothing changed, 'C' a change.
// Under WayOut a stop of a command other than hold set and hold release joins Left, which hold
// release's rule reads; other rule sets never set it, so their states are as the bundle's.
func refuseLeft(r rules, t modelTable, sc modelScenario, s modelState, k int, why string, rerun bool, left byte) modelEdge {
	c := s.Slots[k].Cmd
	ns := idle(s, k)
	if t.stoppable(sc.W, c) && inSet(s.Intent, c) {
		ns.Intent = delSet(ns.Intent, c)
		if r.WayOut {
			ns.StopLatest = addSet(ns.StopLatest, c)
		}
		label := c + ": " + why + ", stop entry"
		if r.WayOut && c != cmdSet && c != CommandRelease {
			if left == 'C' || ns.Left == 0 {
				ns.Left = left
			}
			label += map[byte]string{'U': " left unchanged", 'C': " left changed"}[left]
		}
		return modelEdge{to: ns, label: label, op: true}
	}
	suffix := ""
	switch {
	case !inSet(s.Intent, c):
	case rerun:
		suffix = ", its intent pending for its rerun"
	case r.Serialize:
		suffix = ", its intent left pending"
	}
	return modelEdge{to: ns, label: c + ": " + why + suffix, op: true}
}

// transient lists the refusals the hold's design states for a command, whose way out is the
// command's own rerun once the operator has done the step it names.
var transient = map[string]string{
	"streams move backup":        "stops, an absent stream's verified archive is missing",
	"streams move restore":       "stops, a present stream differs from its backup",
	"streams census":             "refuses, the run is not clean",
	"convert":                    "stops for K, F's info cannot be read",
	"create-bucket":              "stops as UNKNOWN, the bucket's configuration or content is not this hold's",
	"create-bucket --after-loss": "stops, the inputs or the bucket are not the selection record's",
	"restore --finish":           "refuses, a server's id is still the one restore --prepare recorded",
}

func setStep(r rules, t modelTable, sc modelScenario, s modelState, k int) []modelEdge {
	sl := s.Slots[k]
	ns := s
	to := func(step int, label string) modelEdge {
		n := ns
		n.Slots[k].Step = step
		return modelEdge{to: n, label: label, op: true}
	}
	switch sl.Step {
	case setRead:
		if s.Rec {
			return []modelEdge{to(setPre, "hold set: its record found")}
		}
		ins := ns
		ins.Rec = true
		if sc.Hub {
			ins.Step1 = s.Cur
		}
		ins.Slots[k].Step = setPre
		out := []modelEdge{{to: ins, label: "hold set: step 1 read, record inserted", op: true}}
		if sc.Hub {
			for tg, n := range targets {
				if r.Step1Whole {
					out = append(out, refuse(r, t, sc, s, k, "target "+n+"'s disk cannot be read, UNKNOWN, refused", false))
					continue
				}
				p := ins
				p.Step1[tg] = modelDisk{}
				out = append(out, modelEdge{to: p, label: "hold set: target " + n + " unread, record inserted without it", op: true})
			}
		}
		return out
	case setPre:
		nx := setMove
		if r.SetCommand {
			nx = setIntent
		}
		return []modelEdge{refuse(r, t, sc, s, k, "the preflight refuses", false), to(nx, "hold set: the preflight passes")}
	case setIntent:
		if s.Row != 'P' || t.decide(toDone(sc, s), modelRequest{Request: Request{Command: cmdSet}}) != nil {
			return []modelEdge{refuse(r, t, sc, s, k, "Admit refuses", false)}
		}
		ns = writeIntent(ns, cmdSet)
		return []modelEdge{to(setMove, "hold set: admitted with the row prepare, intent written")}
	case setMove:
		if s.Row != 'P' {
			return []modelEdge{refuse(r, t, sc, s, k, "409, the row moved", false)}
		}
		ns.Row = 'H'
		if !r.SetCommand && sc.Hub {
			return []modelEdge{to(setRestore, "hold set: row moved to held")}
		}
		return []modelEdge{to(setSteps, "hold set: row moved to held")}
	case setRestore: // v59: Admit of snapshot restore, by hold set's process
		if t.decide(toDone(sc, s), modelRequest{Request: Request{Command: cmdSnapRestore}}) != nil {
			return []modelEdge{refuse(r, t, sc, s, k, "Admit refuses snapshot restore", false)}
		}
		ns = writeIntent(ns, cmdSnapRestore)
		return []modelEdge{to(setAge, cmdSnapRestore+": admitted, intent written")}
	case setAge: // v59: snapshot restore's done, its own write after its intent's
		ns.Ever, ns.Last, ns.Since = addSet(ns.Ever, cmdSnapRestore), cmdSnapRestore, addSet(ns.Since, cmdSnapRestore)
		ns.Intent = delSet(ns.Intent, cmdSnapRestore)
		return []modelEdge{to(setSteps, cmdSnapRestore+": done")}
	case setSteps:
		out := []modelEdge{refuse(r, t, sc, s, k, "a step of 2 to 6 fails", false)}
		if r.SetCommand {
			return append(out, modelEdge{to: finish(t, sc, ns, k), label: "hold set: steps 2 to 6 pass, done", op: true})
		}
		return append(out, modelEdge{to: idle(ns, k), label: "hold set: steps 2 to 6 pass, the report printed", op: true})
	}
	panic("holdmodel: no hold set step " + strconv.Itoa(sl.Step))
}

func steps(r rules, t modelTable, sc modelScenario, s modelState, k int) []modelEdge {
	sl := s.Slots[k]
	c := sl.Cmd
	ns := s
	adv := func(ns modelState, label string) modelEdge {
		ns.Slots[k].Step++
		return modelEdge{to: ns, label: label, op: true}
	}
	if sl.Step == 0 { // a two-write admission writes its intent now, whatever was written since its read
		ns = writeIntent(ns, c)
		if c == cmdSet {
			ns.Slots[k].Step = setSteps
			return []modelEdge{{to: ns, label: c + ": intent written", op: true}}
		}
		return []modelEdge{adv(ns, modelOp{c, sl.Set}.label()+": intent written")}
	}
	switch c {
	case cmdSet:
		return setStep(r, t, sc, s, k)
	case cmdAbortPrepare:
		if sl.Step == 1 {
			return []modelEdge{adv(ns, "hold abort from prepare: cohort deleted")}
		}
		if s.Row != 'P' {
			return []modelEdge{{to: idle(ns, k), label: "hold abort from prepare: 409, the row moved", op: true}}
		}
		m := idle(ns, k)
		m.Row = 'R'
		u := ""
		if othersRunning(m, k) {
			u = "the row is released while another command runs"
		}
		return []modelEdge{{to: m, label: "hold abort from prepare: row released", op: true, unsafe: u}}
	}
	putRow := func(ns modelState, abort bool) (modelState, string) {
		ns.Row = 'R'
		u := ""
		if sc.Hub {
			u = released(ns, abort)
		}
		if u == "" && othersRunning(ns, k) {
			u = "the row is released while another command runs"
		}
		return ns, u
	}
	reread := func() string {
		if sc.Hub && sl.Base != s.Step1 {
			return "a disk judged by a re-read step-1 id"
		}
		return ""
	}
	lacks := func() bool { // a target the step-1 map lacks, which Step1Whole reads as UNKNOWN
		return r.Step1Whole && (sl.Base[0].ID == 0 || sl.Base[1].ID == 0)
	}
	switch {
	case c == CommandRelease:
		if sl.Step == 1 && !r.CompletionSurvivesRowRestore && s.Complete && s.HoldGen != s.RowGen {
			return []modelEdge{refuse(r, t, sc, s, k, "refused, hold_gen is not the row's gen", false)}
		}
		if !sc.Hub || sl.Step == 2 {
			if sc.Hub && s.RowGen != sl.Gen {
				return []modelEdge{refuse(r, t, sc, s, k, "409 gen_mismatch, the row was restored since its read", true)}
			}
			m, u := putRow(idle(ns, k), false)
			m.Intent = delSet(m.Intent, c)
			return []modelEdge{{to: m, label: c + ": PUT", op: true, unsafe: u}}
		}
		if r.Step1Whole && (s.Step1[0].ID == 0 || s.Step1[1].ID == 0) {
			return []modelEdge{refuse(r, t, sc, s, k, "UNKNOWN, a target the step-1 map lacks", false)}
		}
		ok := s.Proved
		for tg := range targets {
			ok = ok && s.Cur[tg].ID == expected(s, tg)
		}
		if !ok {
			return []modelEdge{refuse(r, t, sc, s, k, "refused, a disk is not the expected one", false)}
		}
		ns.Slots[k].Gen = s.RowGen
		return []modelEdge{adv(ns, c+": disks checked")}
	case !sc.Hub && c != CommandAbort:
		doomable := sc.Fail && bucketsCommand(c)
		if sl.Step == 1 {
			if doomable && s.Doomed { // it fails for good: every run fails before any further change
				return []modelEdge{refuseLeft(r, t, sc, s, k, "fails again, nothing more changed", false, 'U')}
			}
			switch {
			case c == "restore --finish" && s.Unresolved:
				return []modelEdge{refuse(r, t, sc, s, k, "stops, a name has no start", false)}
			case c == "restore --prepare" && s.Unresolved && !r.PrepareMarksUnresolved:
				return []modelEdge{refuse(r, t, sc, s, k, "stops, a name has no start", false)}
			case c == "convert" && s.NoStart:
				return []modelEdge{refuse(r, t, sc, s, k, "stops, no safe start for K", false)}
			}
			switch c {
			case "restore --key":
				ns.Unresolved = false
			case "replay":
				ns.NoStart = false // replay --key gives K a start
			}
			out := []modelEdge{adv(ns, c+": effect")}
			if why, ok := transient[c]; ok {
				out = append(out, refuse(r, t, sc, s, k, why, true))
			}
			if doomable {
				ds := s
				ds.Doomed = true
				out = append(out, refuseLeft(r, t, sc, ds, k, "fails for good before any change", false, 'U'))
			}
			return out
		}
		if t.final(sc.W, c) && r.PendingBlocksComplete && otherPendingAdmitted(r, t, sc, s, c) {
			return []modelEdge{{to: idle(ns, k), label: c + ": refuses to complete, another command's intent has no done", op: true}}
		}
		label := c + ": done"
		if t.final(sc.W, c) {
			label += ", complete"
		}
		out := []modelEdge{{to: finish(t, sc, ns, k), label: label, op: true}}
		if doomable { // it fails for good after its change: a recreate that deleted the bucket
			ds := s
			ds.Doomed = true
			out = append(out, refuseLeft(r, t, sc, ds, k, "fails for good after its change", false, 'C'))
		}
		return out
	case c == CommandProve:
		if sl.Step == 1 { // restart and check
			if lacks() {
				return []modelEdge{refuse(r, t, sc, s, k, "UNKNOWN, a target the step-1 map lacks", false)}
			}
			ok := true
			u := ""
			for tg := range targets {
				if sl.Set == 0 { // the plain prove: every disk the expected one
					ok = ok && s.Cur[tg].ID == expected(s, tg)
				} else if sl.Set&(1<<tg) != 0 {
					ok = ok && s.Cur[tg].ID == sl.Named[tg] && s.Cur[tg].ID != sl.Base[tg].ID
					if sl.Base[tg].ID == 0 {
						u = "a disk judged against a target the step-1 map lacks"
					}
				} else {
					ok = ok && s.Cur[tg].ID == sl.Base[tg].ID
				}
			}
			if u == "" {
				u = reread()
			}
			if !ok {
				e := refuse(r, t, sc, ns, k, "check fails", false)
				if u != "a disk judged against a target the step-1 map lacks" {
					e.unsafe = u
				}
				return []modelEdge{e}
			}
			ns.Slots[k].Checked = [2]int{s.Cur[0].ID, s.Cur[1].ID}
			e := adv(ns, c+": check passes")
			e.unsafe = u
			return []modelEdge{e}
		}
		return []modelEdge{{to: finish(t, sc, ns, k), label: c + ": done", op: true}}
	case c == "streams census":
		if sl.Step == 1 { // a run that is not clean is Stoppable here, and the epoch's rerun is the way out
			return []modelEdge{adv(ns, c+": read and removals"), refuse(r, t, sc, s, k, "refuses, the run is not clean", true)}
		}
		if r.PendingBlocksComplete && otherPendingAdmitted(r, t, sc, s, c) {
			return []modelEdge{{to: idle(ns, k), label: c + ": refuses to complete, another command's intent has no done", op: true}}
		}
		return []modelEdge{{to: finish(t, sc, ns, k), label: c + ": done, complete", op: true}}
	case c == CommandAbort:
		moveRow := func(ns modelState) ([]modelEdge, bool) {
			if s.RowGen != sl.Gen {
				return []modelEdge{refuse(r, t, sc, s, k, "409 gen_mismatch, the row was restored since its read", true)}, true
			}
			return nil, false
		}
		switch sl.Step {
		case 1:
			if !sc.Hub { // outside hub-restore abort reads no disk: it ends the hold as the record stands
				return []modelEdge{adv(ns, c+": nothing to check outside hub-restore")}
			}
			if lacks() {
				return []modelEdge{refuse(r, t, sc, s, k, "UNKNOWN, a target the step-1 map lacks", false)}
			}
			ok := true
			for tg := range targets {
				ok = ok && s.Cur[tg].ID == sl.Base[tg].ID && s.Cur[tg].Snap != 'H'
			}
			if !ok {
				e := refuse(r, t, sc, s, k, "refused, a disk is not step 1's", false)
				e.unsafe = reread()
				return []modelEdge{e}
			}
			e := adv(ns, c+": disks checked")
			e.unsafe = reread()
			return []modelEdge{e}
		case 2:
			if r.AbortMoveBeforeDone {
				if e, stop := moveRow(ns); stop {
					return e
				}
				m, u := putRow(ns, true)
				m.Slots[k].Step = 3
				return []modelEdge{{to: m, label: c + ": row released", op: true, unsafe: u}}
			}
			d := finish(t, sc, ns, k)
			d.Slots[k] = sl
			d.Slots[k].Step = 3
			return []modelEdge{{to: d, label: c + ": done, complete", op: true}}
		default:
			if r.AbortMoveBeforeDone {
				return []modelEdge{{to: finish(t, sc, ns, k), label: c + ": done, complete", op: true}}
			}
			if e, stop := moveRow(ns); stop {
				return e
			}
			m, u := putRow(idle(ns, k), true)
			return []modelEdge{{to: m, label: c + ": row released", op: true, unsafe: u}}
		}
	}
	panic("holdmodel: no steps for " + c)
}

// ---------------------------------------------------------------- invariants and classes

// pendingAtCompletion lists the intents that may not be pending at completion: all but hold
// release's own lock, and hold abort's after a done prove while the row is held.
func pendingAtCompletion(s modelState) []string {
	var out []string
	for _, c := range list(s.Intent) {
		if c == CommandRelease || (c == CommandAbort && s.Proved && s.Row == 'H') {
			continue
		}
		out = append(out, c)
	}
	return out
}

// modelViolation is one counterexample: the state's workflow, the invariant it breaks, its class,
// and the shortest trace to it.
type modelViolation struct {
	Workflow, Kind, Class string
	Trace                 []string
}

// journalWrite reports whether a trace step writes the journal or the row.
func journalWrite(label string) bool {
	for _, w := range []string{"intent written", ": done", "stop entry", ": PUT", "row released", "record inserted", "row moved to held"} {
		if strings.Contains(label, w) {
			return true
		}
	}
	return false
}

// raced reports whether a command was admitted by a read, and another write came before its
// intent's write: the two-write admission's race.
func raced(trace []string) bool {
	admittedAt := map[string]int{}
	for i, tl := range trace {
		if strings.HasPrefix(tl, "crash in ") { // a crash before the intent's write ends that admission
			delete(admittedAt, strings.TrimPrefix(tl, "crash in "))
			continue
		}
		if strings.HasSuffix(tl, ": admitted") {
			admittedAt[strings.TrimSuffix(tl, ": admitted")] = i
			continue
		}
		if strings.HasSuffix(tl, ": intent written") && !strings.HasSuffix(tl, "admitted, intent written") &&
			!strings.HasSuffix(tl, "prepare, intent written") {
			c := strings.TrimSuffix(tl, ": intent written")
			if a, ok := admittedAt[c]; ok {
				for _, between := range trace[a+1 : i] {
					if journalWrite(between) {
						return true
					}
				}
			}
		}
	}
	for c, a := range admittedAt { // admitted, and the trace ends before its intent, after another write
		written := false
		for _, tl := range trace[a+1:] {
			if tl == c+": intent written" {
				written = true
			}
		}
		if !written {
			for _, between := range trace[a+1:] {
				if journalWrite(between) {
					return true
				}
			}
		}
	}
	return false
}

func has(trace []string, label string) bool {
	for _, tl := range trace {
		if tl == label {
			return true
		}
	}
	return false
}

func hasPrefix(trace []string, p string) bool {
	for _, tl := range trace {
		if strings.HasPrefix(tl, p) {
			return true
		}
	}
	return false
}

func refusal(label string) bool {
	for _, w := range []string{"refus", "UNKNOWN", "check fails", "stops"} {
		if strings.Contains(label, w) {
			return true
		}
	}
	return false
}

// setRaced reports whether another command was admitted while hold set ran in held: v59's hold set
// holds no lock.
func setRaced(trace []string) bool {
	in := false
	for _, tl := range trace {
		switch {
		case tl == "hold set: row moved to held":
			in = true
		case strings.HasPrefix(tl, "hold set: steps 2 to 6") || tl == "crash in hold set" ||
			(strings.HasPrefix(tl, "hold set: ") && refusal(tl)):
			in = false
		case in && (strings.HasSuffix(tl, ": admitted") || strings.HasSuffix(tl, ": admitted, intent written")) &&
			!strings.HasPrefix(tl, cmdSnapRestore):
			return true
		}
	}
	return false
}

func setRuns(s modelState) bool {
	for _, sl := range s.Slots {
		if sl.Cmd == cmdSet && runs(sl) {
			return true
		}
	}
	return false
}

// classifyR28 names the classes of round 28, which the earlier rules carry by construction, and ""
// for any other.
func classifyR28(r rules, sc modelScenario, kind string, s modelState, trace []string) string {
	switch {
	case kind == "a disk judged against a target the step-1 map lacks":
		return classR28H
	case !r.SetCommand && (setRaced(trace) || ((kind == "two commands past their intent at once" ||
		kind == "the row is released while another command runs") && setRuns(s))):
		return classR28ASet
	case kind != "liveness":
		return ""
	case sc.Hub && !r.Step1Whole && s.Rec && (s.Step1[0].ID == 0 || s.Step1[1].ID == 0):
		return classR28H // a target the map lacks: hold abort's disk check can never pass
	case !r.SetCommand && inSet(s.Intent, cmdSnapRestore):
		return classR28ASet
	case !r.ConvertStoppable && s.NoStart && inSet(s.Intent, "convert"):
		return classR28AConv
	case !r.PrepareMarksUnresolved && s.Unresolved && inSet(s.Intent, "restore --prepare"):
		return classR28APrep
	case !r.CompletionSurvivesRowRestore && s.Complete && s.HoldGen != s.RowGen:
		return classR28C
	case sc.Hub && !r.ExpectedMap && s.Proved:
		for tg := range targets {
			if s.ProvenSet&(1<<tg) == 0 && s.Cur[tg].ID != s.Step1[tg].ID {
				return classR28B
			}
		}
	}
	return ""
}

func classify(r rules, sc modelScenario, kind string, s modelState, trace []string) string {
	// A buckets command failed for good after its intent, and no command the table admits ends the
	// hold: the class WayOut fixes. Only the profiles that model the failure (Fail) reach it.
	if kind == "liveness" && sc.Fail && s.Doomed {
		return classWayOut
	}
	if c := classifyR28(r, sc, kind, s, trace); c != "" {
		return c
	}
	switch {
	case raced(trace):
		return classTwoWrites
	case has(trace, "hold abort: done, complete") && !r.AbortMoveBeforeDone:
		return classAbortDone
	case kind == "liveness" && !r.AbortMoveBeforeDone && hasPrefix(trace, "hold abort: admitted"):
		return classAbortDone // its done entry will come before a row move that a restored row's new gen refuses
	}
	switch kind {
	case "two commands past their intent at once":
		return classTwoRun
	case "the row is released while another command runs":
		return classOutside
	case "a disk judged by a re-read step-1 id", "abort released over a restored disk":
		return classR27CReread
	case "no false completion":
		if has(trace, "hold abort: done, complete") && !r.AbortMoveBeforeDone {
			return classAbortDone
		}
		return classPending
	}
	// liveness
	finalDone, reopened, admittedEarly, keyDone, stopMissing := false, false, false, false, false
	pendingAdmission := map[string]bool{}
	for _, tl := range trace {
		if strings.HasSuffix(tl, ": admitted") {
			pendingAdmission[strings.TrimSuffix(tl, ": admitted")] = true
		}
		if strings.HasSuffix(tl, "done, complete") {
			finalDone = true
		}
		if strings.HasSuffix(tl, ": intent written") {
			c := strings.TrimSuffix(tl, ": intent written")
			if finalDone && pendingAdmission[c] {
				admittedEarly = true
			}
			if finalDone {
				reopened = true
			}
		}
		if (strings.HasSuffix(tl, ": admitted, intent written") || strings.HasSuffix(tl, ": admitted")) && finalDone &&
			!strings.HasPrefix(tl, CommandRelease) {
			reopened = true
		}
		if tl == "restore --key: done" {
			keyDone = true
		}
		if strings.HasSuffix(tl, "its intent left pending") && refusal(tl) {
			stopMissing = true
		}
	}
	switch {
	case admittedEarly:
		return classTwoWrites
	case stopMissing, r.Serialize && !r.StopEntries:
		return classStop
	}
	if !sc.Hub {
		switch {
		case reopened && sc.Name == "selection-recovery":
			return classR27ASel
		case reopened:
			return "R27-A (" + sc.Name + ": a command admitted after the final command completed the record)"
		case sc.Name == "rollback" && s.Unresolved && !keyDone:
			return classR27F
		}
		return "UNCLASSIFIED (liveness)"
	}
	for i, tl := range trace {
		if tl == "hold abort: done, complete" && i+1 < len(trace) && trace[i+1] == "crash in hold abort" {
			return classAbortCrash
		}
	}
	provedAt, checkedAt, restoreAfterProve, restoreLanded := -1, -1, false, false
	for i, tl := range trace {
		switch {
		case tl == "hold prove: done":
			provedAt = i
		case tl == "hold prove: check passes":
			checkedAt = i
		case strings.HasPrefix(tl, "outside: a restore lands"):
			restoreLanded = true
			if provedAt >= 0 || checkedAt >= 0 { // after a prove's check, its done entry records a disk no longer attached
				restoreAfterProve = true
			}
		}
	}
	switch {
	case restoreAfterProve:
		return classR27E
	case provedAt < 0 && restoreLanded:
		return classR27CNoProve
	}
	return "UNCLASSIFIED (liveness)"
}

// ---------------------------------------------------------------- the package's readings

// disagreement checks, at a hub-restore state of the current rules, that the package's own
// readings of the record the state stands for are the model's: Step1, Proven, Epoch, Expected and
// Moved. It returns "" when they agree, else what differs.
func disagreement(sc modelScenario, s modelState) string {
	if !sc.Hub || !s.Rec {
		return ""
	}
	d := toDone(sc, s)
	full := s.Step1[0].ID != 0 && s.Step1[1].ID != 0
	step1, ok := Step1(d)
	if ok != (s.Step1[0].ID != 0 || s.Step1[1].ID != 0) {
		return fmt.Sprintf("Step1 answers %v for the step-1 disks %v", ok, s.Step1)
	}
	for tg, n := range targets {
		want := ""
		if s.Step1[tg].ID != 0 {
			want = strconv.Itoa(s.Step1[tg].ID)
		}
		if step1["step1_disk_id."+n] != want {
			return fmt.Sprintf("Step1 holds %q for target %s, and the model %q", step1["step1_disk_id."+n], n, want)
		}
	}
	proof, proved := Proven(d)
	if proved != s.Proved {
		return fmt.Sprintf("Proven answers %v, and the model's state proved %v", proved, s.Proved)
	}
	_, ents, epoch := Epoch(d)
	if epoch != s.Proved {
		return fmt.Sprintf("Epoch answers %v, and the model's state proved %v", epoch, s.Proved)
	}
	if s.Proved {
		for tg, n := range targets {
			want := ""
			if s.ProvenSet&(1<<tg) != 0 {
				want = strconv.Itoa(s.Proven[tg])
			}
			if proof.DiskIDs[n] != want {
				return fmt.Sprintf("Proven's DiskIDs hold %q for target %s, and the model %q", proof.DiskIDs[n], n, want)
			}
		}
		since := ""
		for _, o := range ents {
			if o.State == stateDone {
				since = addSet(since, o.Command)
			}
		}
		if since != s.Since {
			return fmt.Sprintf("Epoch's done entries are %q, and the model's epoch %q", since, s.Since)
		}
	}
	if !full {
		read := map[string]string{"A": strconv.Itoa(s.Cur[0].ID), "B": strconv.Itoa(s.Cur[1].ID)}
		if _, err := Moved(d, read); !errors.Is(err, ErrUnknown) {
			return fmt.Sprintf("Moved answers %v for a step-1 map that lacks a target", err)
		}
		return ""
	}
	want, ok := Expected(d)
	if !ok {
		return "Expected answers false for a whole step-1 map"
	}
	for tg, n := range targets {
		if want[n] != strconv.Itoa(expected(s, tg)) {
			return fmt.Sprintf("Expected holds %q for target %s, and the model %d", want[n], n, expected(s, tg))
		}
	}
	if s.Proved {
		read := map[string]string{"A": strconv.Itoa(s.Cur[0].ID), "B": strconv.Itoa(s.Cur[1].ID)}
		moved, err := Moved(d, read)
		if err != nil || moved != disksMoved(currentRules, s) {
			return fmt.Sprintf("Moved answers %v, %v, and the model %v", moved, err, disksMoved(currentRules, s))
		}
	}
	return ""
}

// ---------------------------------------------------------------- exploration

func startLabel(sc modelScenario, s modelState) string {
	l := "start: prepare"
	switch {
	case s.Unresolved:
		l += ", a logical name with no start"
	case s.NoStart:
		l += ", a cohort member with no safe start"
	case sc.Hub:
		l += ", a hub"
	}
	return l
}

// explore walks every state of one workflow under the rules and returns how many it reached, every
// counterexample, and every state at which the package's readings and the model's differ (checked
// for the current rules alone, whose readings the package's functions are).
func explore(r rules, t modelTable, sc modelScenario, bound int) (int, []modelViolation, []string) {
	type node struct {
		s      modelState
		parent int
		label  string
	}
	var nodes []node
	idx := map[modelState]int{}
	var opEdges [][]int32
	trace := func(i int) []string {
		var tr []string
		for i >= 0 {
			tr = append([]string{nodes[i].label}, tr...)
			i = nodes[i].parent
		}
		return tr
	}
	for _, s := range sc.Init {
		s = canon(s)
		if _, ok := idx[s]; !ok {
			idx[s] = len(nodes)
			nodes = append(nodes, node{s, -1, startLabel(sc, s)})
			opEdges = append(opEdges, nil)
		}
	}
	var viol []modelViolation
	var differ []string
	add := func(kind string, s modelState, tr []string) {
		viol = append(viol, modelViolation{sc.Name, kind, classify(r, sc, kind, s, tr), tr})
	}
	seenUnsafe := map[string]bool{}
	for i := 0; i < len(nodes); i++ {
		s := nodes[i].s
		if r == currentRules {
			if why := disagreement(sc, s); why != "" && len(differ) < 5 {
				differ = append(differ, why+", after "+strings.Join(trace(i), " -> "))
			}
		}
		if s.Complete && (!t.final(sc.W, s.Last) || len(pendingAtCompletion(s)) > 0) {
			add("no false completion", s, trace(i))
		}
		if running(s) >= 2 {
			add("two commands past their intent at once", s, trace(i))
		}
		for _, e := range next(r, t, sc, s, bound) {
			to := canon(e.to)
			j, ok := idx[to]
			if !ok {
				j = len(nodes)
				idx[to] = j
				nodes = append(nodes, node{to, i, e.label})
				opEdges = append(opEdges, nil)
			}
			if e.op {
				opEdges[i] = append(opEdges[i], int32(j))
			}
			if e.unsafe != "" {
				key := e.unsafe + "|" + strconv.Itoa(j)
				if !seenUnsafe[key] {
					seenUnsafe[key] = true
					add(e.unsafe, to, append(trace(i), e.label))
				}
			}
		}
	}
	rev := make([][]int32, len(nodes))
	for i, es := range opEdges {
		for _, j := range es {
			rev[j] = append(rev[j], int32(i))
		}
	}
	good := make([]bool, len(nodes))
	var q []int32
	for i, n := range nodes {
		if n.s.Row == 'R' {
			good[i] = true
			q = append(q, int32(i))
		}
	}
	for len(q) > 0 {
		j := q[0]
		q = q[1:]
		for _, i := range rev[j] {
			if !good[i] {
				good[i] = true
				q = append(q, i)
			}
		}
	}
	for i, n := range nodes {
		if !good[i] {
			add("liveness", n.s, trace(i))
		}
	}
	return len(nodes), viol, differ
}

// modelRun is one rule set's run at one bound: its states and counterexamples in all, by
// workflow, and by class, the shortest trace of each class, and where the package's readings
// differed from the model's.
type modelRun struct {
	states, total int
	byWorkflow    map[string]int
	byClass       map[string]int
	shortest      map[string]modelViolation
	differ        []string
}

func runModel(r rules, bound int, fail bool) modelRun {
	t := tableOf(r)
	out := modelRun{byWorkflow: map[string]int{}, byClass: map[string]int{}, shortest: map[string]modelViolation{}}
	for _, sc := range scenarios(r, fail) {
		n, v, differ := explore(r, t, sc, bound)
		out.states += n
		out.total += len(v)
		out.byWorkflow[sc.Name] = n
		out.differ = append(out.differ, differ...)
		for _, x := range v {
			out.byClass[x.Class]++
			if s, ok := out.shortest[x.Class]; !ok || len(x.Trace) < len(s.Trace) {
				out.shortest[x.Class] = x
			}
		}
	}
	return out
}

// found is how many of the run's counterexamples are of the class want names: a class whose name
// starts with it, as a control's class names a prefix (R27-A's names each workflow after it).
func (m modelRun) found(want string) int {
	n := 0
	for c, k := range m.byClass {
		if strings.HasPrefix(c, want) {
			n += k
		}
	}
	return n
}

// modelCount is a rule set's run as the frozen model counted it at bound 2: its states and its
// counterexamples, and, for a control, how many it found of each class it names.
type modelCount struct {
	states, counterexamples int
	classes                 map[string]int
}

// frozenBound2 is the frozen model's run at bound 2 (its run-bound2.out), one row per rule set.
var frozenBound2 = map[string]modelCount{
	"v49":                              {117021, 80869, map[string]int{classAbortDone: 5139}},
	"v49+Step1Recorded":                {140075, 67585, map[string]int{classR27E: 392}},
	"v52":                              {89187, 43878, map[string]int{classR27CNoProve: 3520}},
	"v53":                              {251235, 27688, map[string]int{classTwoWrites: 2112}},
	"v53-PendingBlocksComplete":        {263970, 45099, map[string]int{classPending: 2961}},
	"v59":                              {124894, 43017, map[string]int{classR28ASet: 28604, classR28B: 384}},
	"v62":                              {31948, 0, nil},
	"v62-Step1Recorded":                {20164, 7052, map[string]int{classR27CReread: 552}},
	"v62-ProveEpochs":                  {22000, 2304, map[string]int{classR27E: 2304}},
	"v62-AbortMoveBeforeDone":          {39760, 734, map[string]int{classAbortDone: 734}},
	"v62-AfterCompleteStrict":          {33370, 1422, map[string]int{classR27A: 1422}},
	"v62-SelRecFinalRepeatable":        {31678, 297, map[string]int{classR27ASel: 297}},
	"v62-RestoreKeyInTable":            {31585, 123, map[string]int{classR27F: 123}},
	"v62-AtomicAdmit":                  {286517, 67661, map[string]int{classTwoWrites: 67661}},
	"v62-Serialize":                    {135471, 86280, map[string]int{classTwoRun: 60724}},
	"v62-StopEntries":                  {30975, 1995, map[string]int{classStop: 1995}},
	"v62-SetCommand":                   {39130, 8294, map[string]int{classR28ASet: 7590}},
	"v62-Step1Whole":                   {113110, 7380, map[string]int{classR28H: 7380}},
	"v62-ExpectedMap":                  {28204, 864, map[string]int{classR28B: 864}},
	"v62-ConvertStoppable":             {31990, 90, map[string]int{classR28AConv: 90}},
	"v62-PrepareMarksUnresolved":       {31831, 45, map[string]int{classR28APrep: 45}},
	"v62-CompletionSurvivesRowRestore": {31534, 1035, map[string]int{classR28C: 1035}},
}

// The frozen v62's runs, by workflow (run-bound2.out and run-bound3.out).
var (
	frozenV62Bound2 = map[string]int{"cutover": 3622, "rollback": 1348, "selection-recovery": 1652, "buckets-create": 476,
		"buckets-recreate": 476, "buckets-restore": 476, "streams-update": 476, "streams-census": 482, "hub-restore": 22940}
	frozenV62Bound3 = map[string]int{"cutover": 4908, "rollback": 1842, "selection-recovery": 2253, "buckets-create": 651,
		"buckets-recreate": 651, "buckets-restore": 651, "streams-update": 651, "streams-census": 657, "hub-restore": 127548}
)

// pinnedBound2 is this model's run at bound 2 of the rule added since the bundle (WayOut), with a
// buckets command that fails for good modeled: the current rules and their control. The control's
// workflows but the three buckets ones are v62's, as the bundle counted them.
var pinnedBound2 = map[string]modelCount{
	"v63":        {144617, 0, nil},
	"v63-WayOut": {32083, 135, map[string]int{classWayOut: 135}},
}

// The current rules' runs by workflow, pinned from this model at bounds 2 and 3.
var (
	pinnedCurrentBound2 = map[string]int{"cutover": 7828, "rollback": 2469, "selection-recovery": 1926, "buckets-create": 864,
		"buckets-recreate": 864, "buckets-restore": 864, "streams-update": 609, "streams-census": 616, "hub-restore": 128577}
	pinnedCurrentBound3 = map[string]int{"cutover": 10617, "rollback": 3381, "selection-recovery": 2632, "buckets-create": 1186,
		"buckets-recreate": 1186, "buckets-restore": 1186, "streams-update": 837, "streams-census": 844, "hub-restore": 765341}
	pinnedCurrentBound3Total = 787210
)

// The model of the hold's workflows, run on this package's journal and table. The frozen bundle's
// profiles reproduce its counts: v62 finds no counterexample at two crashes and restores per run
// (31,948 states) and at three (139,812), and every earlier rule set and every rule set with one of
// v62's fixes removed is a control that finds a counterexample of each class it names, asserted by
// the class's name, with the bundle's counts. The current rules add WayOut and are run with a
// buckets command that fails for good after its intent: they find no counterexample, at bounds 2
// and 3, with pinned state counts, and their control, the same rules without WayOut, finds the
// stranded hold (classWayOut). At every hub-restore state of the current rules the package's Step1,
// Proven, Epoch, Expected and Moved read the record as the model does.
func TestTheHoldJournalModel(t *testing.T) {
	if currentRules.Name != "v63" || v62.WayOut {
		t.Fatalf("the current rules are %q and v62's WayOut is %v; the counts below are v62's and v63's", currentRules.Name, v62.WayOut)
	}
	for _, w := range tableFor(currentRules).Workflows() {
		for _, c := range append([]string{CommandSet, CommandRelease}, Commands(w)...) {
			if Admits(w, c) != current.Admits(w, c) || fmt.Sprint(Requires(w, c)) != fmt.Sprint(current.Requires(w, c)) ||
				fmt.Sprint(Excludes(w, c)) != fmt.Sprint(current.Excludes(w, c)) || Repeatable(w, c) != current.Repeatable(w, c) ||
				Stoppable(w, c) != current.Stoppable(w, c) || Epochal(w, c) != current.Epochal(w, c) || Final(w, c) != current.Final(w, c) {
				t.Errorf("%s's %q: the package's table functions are not the current rules' table", w, c)
			}
		}
	}
	for _, p := range modelProfiles() {
		p := p
		t.Run(p.Name, func(t *testing.T) {
			t.Parallel()
			got := runModel(p.rules, 2, p.Fail)
			want, ok := frozenBound2[p.Name]
			if !ok {
				want, ok = pinnedBound2[p.Name]
			}
			if !ok {
				t.Fatalf("no count for the rules %s: it ran %d states and %d counterexamples, by workflow %v, by class %v",
					p.Name, got.states, got.total, got.byWorkflow, got.byClass)
			}
			if got.states != want.states || got.total != want.counterexamples {
				t.Errorf("rules %s at bound 2: %d states and %d counterexamples, and the pinned count %d and %d",
					p.Name, got.states, got.total, want.states, want.counterexamples)
			}
			for c, n := range got.byClass {
				if strings.HasPrefix(c, "UNCLASSIFIED") {
					t.Errorf("rules %s found %d counterexamples of no named class, the shortest %s", p.Name, n,
						strings.Join(got.shortest[c].Trace, " -> "))
				}
			}
			for _, why := range got.differ {
				t.Errorf("rules %s: the package's reading differs from the model's: %s", p.Name, why)
			}
			if len(p.Want) == 0 {
				for c, v := range got.shortest {
					t.Errorf("rules %s found %d counterexamples of %s, the shortest (%s, %s): %s",
						p.Name, got.byClass[c], c, v.Workflow, v.Kind, strings.Join(v.Trace, " -> "))
				}
				byWorkflow := frozenV62Bound2
				if p.rules == currentRules {
					byWorkflow = pinnedCurrentBound2
				}
				for w, n := range byWorkflow {
					if got.byWorkflow[w] != n {
						t.Errorf("rules %s's %s at bound 2: %d states, and the pinned count %d", p.Name, w, got.byWorkflow[w], n)
					}
				}
				if len(byWorkflow) != len(got.byWorkflow) {
					t.Errorf("rules %s ran the workflows %v, and the pinned counts name %v", p.Name, got.byWorkflow, byWorkflow)
				}
				return
			}
			for _, c := range p.Want {
				n := got.found(c)
				if n == 0 {
					t.Errorf("control %s found no counterexample of its class %s", p.Name, c)
					continue
				}
				if n != want.classes[c] {
					t.Errorf("control %s found %d counterexamples of its class %s, and the pinned count %d", p.Name, n, c, want.classes[c])
				}
				t.Logf("control %s found %d counterexamples of its class %s, the shortest (%s): %s", p.Name, n, c,
					got.shortest[c].Workflow, strings.Join(got.shortest[c].Trace, " -> "))
			}
		})
	}
	for _, b := range []struct {
		r          rules
		fail       bool
		total      int
		byWorkflow map[string]int
	}{{v62, false, 139812, frozenV62Bound3}, {currentRules, true, pinnedCurrentBound3Total, pinnedCurrentBound3}} {
		b := b
		t.Run(b.r.Name+" at bound 3", func(t *testing.T) {
			t.Parallel()
			got := runModel(b.r, 3, b.fail)
			if got.states != b.total || got.total != 0 {
				t.Errorf("rules %s at bound 3: %d states and %d counterexamples, and the pinned count %d and 0 (by workflow %v)",
					b.r.Name, got.states, got.total, b.total, got.byWorkflow)
			}
			for w, n := range b.byWorkflow {
				if got.byWorkflow[w] != n {
					t.Errorf("rules %s's %s at bound 3: %d states, and the pinned count %d", b.r.Name, w, got.byWorkflow[w], n)
				}
			}
			for c, v := range got.shortest {
				t.Errorf("rules %s at bound 3 found %d counterexamples of %s, the shortest (%s, %s): %s",
					b.r.Name, got.byClass[c], c, v.Workflow, v.Kind, strings.Join(v.Trace, " -> "))
			}
			for _, why := range got.differ {
				t.Errorf("the package's reading differs from the model's at bound 3: %s", why)
			}
		})
	}
}
