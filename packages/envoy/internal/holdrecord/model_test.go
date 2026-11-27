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
// gen (a database admin deletes it, and the operator restores it), or, for the rules since the
// bundle, lost in `prepare` and restored `released` with the hold's id, which leaves the hold's
// cohort and journal until hold abort on the released row ends them. A prove names any non-empty set
// of targets, its done entry records those alone, and the step-1 map is what hold set's reads gave
// it. For the rules added since the frozen bundle, any command may also fail for good after its
// intent, before its change or after it, and every later run of it fails again: in hub-restore the
// census and a prove after a done prove, and every command of the other workflows. The record
// keeps each stop, as the code writes it, and a stop records what every run of its command in the
// hold left (LeftUnchanged's rule), so a rerun that changes nothing itself still records an earlier
// run's change. Hold abort, admitted in every workflow once a command has stopped and no intent is
// pending (in hub-restore after a done prove, once a command stopped after it), ends the hold. In
// hub-restore after a done prove, abort compares each disk with the prove's record, as the hold's
// command does, and releases over the proven disks, the residual the design states; once a command
// has failed for good there, the operator may restore a target back onto the done prove's disk.
// Hold release never moves the row on a record that is not complete while a run's change outside
// the journal stands.
//
// It explores every reachable state breadth-first, for every workflow, with two operator
// processes. An admission is what the code does: with AtomicAdmit, one compare-and-swap step that
// decides and writes the intent, and without it, a read that decides and a later write of the
// intent. It checks these invariants at every state of every workflow:
//
//   - liveness: every state whose row is not released, or whose released row left a hold's cohort
//     and journal (a row lost in prepare), can reach a released row with no such hold through
//     admitted operator steps alone, with no hand-signed PUT;
//   - one command at a time: no two processes are past their intent at once (hold set from its
//     row move on);
//   - no command outside its hold: the row is never released while another process is past its
//     intent;
//   - no false completion: complete is true only when the workflow's final command is the last
//     done entry and no command's intent is pending (but hold release's own, and hold abort's
//     after a done prove, whose one effect, the row move, has not happened while the row is held);
//   - hub-restore's safety: abort never releases over a restored disk, but after a done prove
//     under HubAbortAfterStop, where it releases over the disks that prove proved and never over
//     another; a disk is judged only by its step-1 id as recorded, and never against a target the
//     step-1 map lacks.
//
// The current rules must find no counterexample, and every earlier rule set and every rule set
// with one fix removed must find a counterexample of each class it names, which the test asserts
// by the class's name. The frozen bundle's profiles do not model the failing buckets command, so
// their state and counterexample counts are those the bundle gave, and a port that changed what the
// model explores fails here too; the current rules and their control model it, with pinned counts.

import (
	"fmt"
	"hash/maphash"
	"os"
	"runtime/debug"
	"slices"
	"strings"
	"testing"
)

// modelRules is a rule set as the model runs it: the rules the package's table and Decide read
// (rules), and the choices only the model's transitions read, which the hold's commands make
// outside this package.
type modelRules struct {
	rules
	Name                         string
	Step1Recorded                bool // hold set records the step-1 map, and abort, prove and release compare against it
	AbortMoveBeforeDone          bool // abort moves the row before its done entry
	PendingBlocksComplete        bool // an earlier design's rule: a final done entry is refused while another admitted command's intent is pending
	AtomicAdmit                  bool // admission and intent are one compare-and-swap write
	Step1Whole                   bool // hold set refuses a target it cannot read, and a target the map lacks is UNKNOWN
	ExpectedMap                  bool // DisksMoved compares Proven's ids for the named targets and Step1's for the rest
	PrepareMarksUnresolved       bool // restore --prepare marks a name with no start and goes on; restore --finish stops
	CompletionSurvivesRowRestore bool // release does not compare hold_gen, so a row restored by RestoreRow can release
	// DiskGoneRestore: after abort's disk check finds the done prove's disk gone and its stop names
	// it, the operator restores that target onto another disk, where a new prove can pass.
	DiskGoneRestore bool
	// RestoreBack: once a command has failed for good after a done prove, the operator restores a
	// target back onto the done prove's disk (step 1's for a target the prove did not name), where
	// abort's disk check passes after a disk moved and no prove can prove the new one.
	RestoreBack bool
	// PrepareRowLoss: the row may be lost in prepare and restored released with the hold's id (the
	// restore route admits released after a prepare entry), which leaves the hold's cohort and,
	// once hold set's reads wrote it, its journal.
	PrepareRowLoss bool
}

// currentModel is the current rules as the model runs them: the package's currentRules and every
// choice of the model's own.
var currentModel = modelRules{rules: currentRules, Name: "v63", Step1Recorded: true, AbortMoveBeforeDone: true,
	AtomicAdmit: true, Step1Whole: true, ExpectedMap: true, PrepareMarksUnresolved: true, CompletionSurvivesRowRestore: true,
	DiskGoneRestore: true, RestoreBack: true, PrepareRowLoss: true}

// The earlier versions' rules. Their hub-restore runs with one operator process, as v53's model
// did: the two-process race is shown by their generic workflows, and by the later controls in every
// workflow. v59 is the rule set the plan's versions v55 to v59 carried.
var (
	v49 = modelRules{rules: rules{PlainReprove: true}, Name: "v49"}
	v52 = modelRules{rules: rules{PlainReprove: true, AfterCompleteStrict: true}, Name: "v52", AbortMoveBeforeDone: true}
	v53 = modelRules{rules: rules{ProveEpochs: true, PlainReprove: true, AfterCompleteStrict: true, SelRecFinalRepeatable: true,
		RestoreKeyInTable: true}, Name: "v53", Step1Recorded: true, AbortMoveBeforeDone: true, PendingBlocksComplete: true}
	v59 = modelRules{rules: rules{ProveEpochs: true, AfterCompleteStrict: true, SelRecFinalRepeatable: true, RestoreKeyInTable: true,
		Serialize: true, StopEntries: true}, Name: "v59", Step1Recorded: true, AbortMoveBeforeDone: true, AtomicAdmit: true}
	// v62 is the frozen bundle's rule set: the current rules without the rules added since. Its
	// profile and every profile with one of its fixes removed run the bundle's own scenarios, so
	// they reproduce the bundle's counts.
	v62 = func() modelRules {
		r := currentModel
		r.Name, r.WayOut, r.EveryCommandStoppable, r.EndIsFinal, r.BackupCommand, r.HubAbortAfterStop = "v62", false, false, false, false, false
		r.AbortAfterOwnStop, r.DiskGoneRestore, r.RestoreBack, r.ReleasedPrepareAbort, r.PrepareRowLoss = false, false, false, false, false
		r.ReleasedNoJournalAbort = false
		return r
	}()
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
	classWayOut      = "WAY-OUT (a command that fails for good after its intent strands the hold: no command the table admits ends it)"
	classEveryRun    = "EVERY-RUN (a rerun's stop records its own run alone: hold release moves the row as unchanged over an earlier run's change)"
	classOrphan      = "ORPHAN (a row lost in prepare and restored released leaves its hold's cohort, and its journal when written, and no command ends them)"
)

// releasedOverAChange is the invariant hold release keeps: it never moves the row on a record that
// is not complete while a run's change outside the journal stands.
const releasedOverAChange = "the row is released as unchanged over a change outside the journal"

// modelProfile is a rule set and the classes its run must find: none for the current rules, and
// each control's own. Fail models a command that fails for good after its intent, which the frozen
// bundle did not model, so only the profiles of the rules added since set it.
type modelProfile struct {
	modelRules
	Want []string
	Fail bool
	// OwnRunOnly is the control of LeftUnchanged's every-run rule: a stop records what its own run
	// left alone.
	OwnRunOnly bool
}

func modelProfiles() []modelProfile {
	v49s := v49
	v49s.Name, v49s.Step1Recorded = "v49+Step1Recorded", true
	v53p := v53
	v53p.Name, v53p.PendingBlocksComplete = "v53-PendingBlocksComplete", false
	out := []modelProfile{
		{modelRules: v49, Want: []string{classAbortDone}},
		{modelRules: v49s, Want: []string{classR27E}},
		{modelRules: v52, Want: []string{classR27CNoProve}},
		{modelRules: v53, Want: []string{classTwoWrites}},
		{modelRules: v53p, Want: []string{classPending}},
		{modelRules: v59, Want: []string{classR28ASet, classR28B}},
		{modelRules: v62},
	}
	type ab struct {
		name string
		f    func(*modelRules)
		want []string
	}
	for _, a := range []ab{
		{"Step1Recorded", func(r *modelRules) { r.Step1Recorded = false }, []string{classR27CReread}},
		{"ProveEpochs", func(r *modelRules) { r.ProveEpochs = false }, []string{classR27E}},
		{"AbortMoveBeforeDone", func(r *modelRules) { r.AbortMoveBeforeDone = false }, []string{classAbortDone}},
		{"AfterCompleteStrict", func(r *modelRules) { r.AfterCompleteStrict = false }, []string{classR27A}},
		{"SelRecFinalRepeatable", func(r *modelRules) { r.SelRecFinalRepeatable = false }, []string{classR27ASel}},
		{"RestoreKeyInTable", func(r *modelRules) { r.RestoreKeyInTable = false }, []string{classR27F}},
		{"AtomicAdmit", func(r *modelRules) { r.AtomicAdmit = false }, []string{classTwoWrites}},
		{"Serialize", func(r *modelRules) { r.Serialize = false }, []string{classTwoRun}},
		{"StopEntries", func(r *modelRules) { r.StopEntries = false }, []string{classStop}},
		{"SetCommand", func(r *modelRules) { r.SetCommand = false }, []string{classR28ASet}},
		{"Step1Whole", func(r *modelRules) { r.Step1Whole = false }, []string{classR28H}},
		{"ExpectedMap", func(r *modelRules) { r.ExpectedMap = false }, []string{classR28B}},
		{"ConvertStoppable", func(r *modelRules) { r.ConvertStoppable = false }, []string{classR28AConv}},
		{"PrepareMarksUnresolved", func(r *modelRules) { r.PrepareMarksUnresolved = false }, []string{classR28APrep}},
		{"CompletionSurvivesRowRestore", func(r *modelRules) { r.CompletionSurvivesRowRestore = false }, []string{classR28C}},
	} {
		r := v62
		r.Name = v62.Name + "-" + a.name
		a.f(&r)
		out = append(out, modelProfile{modelRules: r, Want: a.want})
	}
	// The current rules, with a command that fails for good modeled, and their controls: the same
	// rules without WayOut, without EveryCommandStoppable (the buckets commands alone Stoppable),
	// without HubAbortAfterStop (a done prove excludes abort for good), without DiskGoneRestore
	// (no restore onto another disk after abort's stop names the done prove's disk gone) and
	// without RestoreBack (no restore back onto the done prove's disk after a disk moved), each of
	// which strands the hold, without ReleasedPrepareAbort (no abort on a row lost in prepare and
	// restored released) and without ReleasedNoJournalAbort (no abort there when the journal was
	// never written), each of which orphans its hold, and the same rules with each stop recording
	// its own run alone (OwnRunOnly), under which hold release moves the row over an earlier run's
	// change. AbortAfterOwnStop has no control here: without it a doomed census's rerun writes a stop
	// after the new prove, which admits abort, so its absence strands no hold, and its rows in
	// table_test.go and journal_test.go are its controls.
	noWayOut := currentModel
	noWayOut.Name, noWayOut.WayOut = currentModel.Name+"-WayOut", false
	bucketsAlone := currentModel
	bucketsAlone.Name, bucketsAlone.EveryCommandStoppable = currentModel.Name+"-EveryCommandStoppable", false
	proveExcludes := currentModel
	proveExcludes.Name, proveExcludes.HubAbortAfterStop = currentModel.Name+"-HubAbortAfterStop", false
	noGone := currentModel
	noGone.Name, noGone.DiskGoneRestore = currentModel.Name+"-DiskGoneRestore", false
	noBack := currentModel
	noBack.Name, noBack.RestoreBack = currentModel.Name+"-RestoreBack", false
	ownRun := currentModel
	ownRun.Name = currentModel.Name + "-OwnRunOnly"
	noReleasedAbort := currentModel
	noReleasedAbort.Name, noReleasedAbort.ReleasedPrepareAbort = currentModel.Name+"-ReleasedPrepareAbort", false
	noJournalAbort := currentModel
	noJournalAbort.Name, noJournalAbort.ReleasedNoJournalAbort = currentModel.Name+"-ReleasedNoJournalAbort", false
	return append(out, modelProfile{modelRules: currentModel, Fail: true},
		modelProfile{modelRules: noWayOut, Want: []string{classWayOut}, Fail: true},
		modelProfile{modelRules: bucketsAlone, Want: []string{classWayOut}, Fail: true},
		modelProfile{modelRules: proveExcludes, Want: []string{classWayOut}, Fail: true},
		modelProfile{modelRules: noGone, Want: []string{classWayOut}, Fail: true},
		modelProfile{modelRules: noBack, Want: []string{classWayOut}, Fail: true},
		modelProfile{modelRules: noReleasedAbort, Want: []string{classOrphan}, Fail: true},
		modelProfile{modelRules: noJournalAbort, Want: []string{classOrphan}, Fail: true},
		modelProfile{modelRules: ownRun, Want: []string{classEveryRun}, Fail: true, OwnRunOnly: true})
}

// modelTable is what the model's transitions ask of a rule set's table. For the current rules it
// is the package's own Decide, Final and Stoppable, which is what the hold's commands call; for an
// earlier rule set or a control it is that set's table.
type modelTable struct {
	decide    func(d Done, req modelRequest) error
	final     func(w Workflow, command string) bool
	stoppable func(w Workflow, command string) bool
	decided   *modelDecided // one walk's answers of decide (explore makes it)
}

// modelDecided is one walk's answers of decide: whether it admitted each request on the record
// of each journal the walk asked about. A state's steps ask about the state's one journal again
// and again, so the journal asked about last is kept at hand, with its answers.
type modelDecided struct {
	byJournal map[modelJournal]*modelAnswers
	last      modelJournal
	answers   *modelAnswers
}

// modelAnswers is decide's answer to each request on one journal's record, by modelAsk.index:
// 0 while not asked, then answerAdmitted or answerRefused.
type modelAnswers [numCmds << 3]uint8

const (
	answerAdmitted = 1 + iota
	answerRefused
)

// index is the request's place in a modelAnswers: its command, and a bit for each of its flags.
func (a modelAsk) index() int {
	i := int(a.cmd) << 3
	if a.disksMoved {
		i |= 1
	}
	if a.rowReleased {
		i |= 2
	}
	if a.plain {
		i |= 4
	}
	return i
}

// admits reports whether decide admits a on the record j stands for (toDone). decide is a
// function of the record and the request alone, and the record of a journal (toDone's whole
// input), so a walk asks decide once for each journal and request it reaches, and every other
// state with that journal takes the same answer.
func (t modelTable) admits(sc modelScenario, j modelJournal, a modelAsk) bool {
	d := t.decided
	if d.answers == nil || d.last != j {
		answers, ok := d.byJournal[j]
		if !ok {
			answers = new(modelAnswers)
			d.byJournal[j] = answers
		}
		d.last, d.answers = j, answers
	}
	switch d.answers[a.index()] {
	case answerAdmitted:
		return true
	case answerRefused:
		return false
	}
	ok := t.decide(toDone(sc, j), a.request()) == nil
	d.answers[a.index()] = answerRefused
	if ok {
		d.answers[a.index()] = answerAdmitted
	}
	return ok
}

func tableOf(r modelRules) modelTable {
	if r == currentModel {
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
	t := tableFor(r.rules)
	return modelTable{decide: t.Decide, final: t.Final, stoppable: t.Stoppable}
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

// stateSeed is the seed of the visited set's state hash.
var stateSeed = maphash.MakeSeed()

// modelVisited is a walk's visited set: each state's node, under a 64-bit hash of the state. A
// key holds a state only when the node under it holds that very state, so the set never takes two
// states for one; a state whose hash another state's node holds goes under the next free key.
type modelVisited struct {
	node map[uint64]int32
	hash func(modelState) uint64
}

// find returns the node that holds s, or false and the key a node of s goes under. stateOf is a
// node's state.
func (v modelVisited) find(s modelState, stateOf func(int32) *modelState) (int32, uint64, bool) {
	for h := v.hash(s); ; h++ {
		j, ok := v.node[h]
		if !ok {
			return -1, h, false
		}
		if *stateOf(j) == s {
			return j, h, true
		}
	}
}

// modelReading is what disagreement reads of a state: its journal and its disks.
type modelReading struct {
	journal modelJournal
	cur     [2]modelDisk
}

// modelUnsafe is one unsafe step's kind and the node it reaches, each counted once.
type modelUnsafe struct {
	kind string
	to   int32
}

// nodeChunk is how many nodes a chunk of a walk's nodes holds.
const nodeChunk = 1 << 14

// nodeList is a walk's nodes, held in chunks of nodeChunk, so a walk that grows to hundreds of
// megabytes never copies the nodes it holds, and never holds two copies of them, as a slice's
// growth does. The first chunk grows as a slice does, so a small walk allocates little.
type nodeList[T any] struct {
	chunks [][]T
	n      int32
}

// add appends v and returns its index.
func (l *nodeList[T]) add(v T) int32 {
	last := len(l.chunks) - 1
	if last < 0 || len(l.chunks[last]) == nodeChunk {
		var chunk []T
		if last >= 0 {
			chunk = make([]T, 0, nodeChunk)
		}
		l.chunks = append(l.chunks, chunk)
		last++
	}
	l.chunks[last] = append(l.chunks[last], v)
	l.n++
	return l.n - 1
}

func (l *nodeList[T]) at(i int32) *T { return &l.chunks[i/nodeChunk][i%nodeChunk] }

// explore walks every state of one workflow under the rules and returns how many it reached, every
// counterexample, and every state at which the package's readings and the model's differ (checked
// for the current rules alone, whose readings the package's functions are).
func explore(r modelRules, t modelTable, sc modelScenario, bound int) (int, []modelViolation, []string) {
	// A node is a state, the node it was first reached from, and that step's label, an index into
	// labels. It holds no pointer, so the collector never scans the walk's nodes.
	type node struct {
		s      modelState
		parent int32
		label  int32
	}
	var nodes nodeList[node]
	var labels []string
	labelIDs := map[string]int32{}
	labelOf := func(l string) int32 {
		id, ok := labelIDs[l]
		if !ok {
			id = int32(len(labels))
			labels = append(labels, l)
			labelIDs[l] = id
		}
		return id
	}
	visited := modelVisited{node: map[uint64]int32{}, hash: func(s modelState) uint64 { return maphash.Comparable(stateSeed, s) }}
	stateOf := func(j int32) *modelState { return &nodes.at(j).s }
	// reach returns the node of s, which it adds, reached from parent by the step label, when s is
	// new.
	reach := func(s modelState, parent int32, label string) int32 {
		j, h, ok := visited.find(s, stateOf)
		if !ok {
			j = nodes.add(node{s, parent, labelOf(label)})
			visited.node[h] = j
		}
		return j
	}
	// The operator steps, which liveness may use, in one list: node i's lead to the nodes
	// opTo[opFrom[i]:opFrom[i+1]]. A node's steps are all found while it is expanded, which the
	// walk does in node order.
	var opFrom, opTo []int32
	trace := func(i int32) []string {
		n := 0
		for j := i; j >= 0; j = nodes.at(j).parent {
			n++
		}
		tr := make([]string, n)
		for j := i; j >= 0; j = nodes.at(j).parent {
			n--
			tr[n] = labels[nodes.at(j).label]
		}
		return tr
	}
	for _, s := range sc.Init {
		s = canon(s)
		reach(s, -1, startLabel(sc, s))
	}
	var viol []modelViolation
	var differ []string
	add := func(kind string, s modelState, tr []string) {
		viol = append(viol, modelViolation{sc.Name, kind, classify(r, sc, kind, s, tr), tr})
	}
	seenUnsafe := map[modelUnsafe]bool{}
	t.decided = &modelDecided{byJournal: map[modelJournal]*modelAnswers{}}
	// readings is disagreement's answer for each journal and disks the walk reached: a function of
	// the two, so each pair is checked once and every state with that pair takes its answer.
	readings := map[modelReading]string{}
	current := r == currentModel
	var edges []modelEdge // next's steps from one state, the buffer reused for every state
	for i := int32(0); i < nodes.n; i++ {
		s := nodes.at(i).s
		opFrom = append(opFrom, int32(len(opTo)))
		if current {
			k := modelReading{s.modelJournal, s.Cur}
			why, seen := readings[k]
			if !seen {
				why = disagreement(sc, s.modelJournal, s.Cur)
				readings[k] = why
			}
			if why != "" && len(differ) < 5 {
				differ = append(differ, why+", after "+strings.Join(trace(i), " -> "))
			}
		}
		if s.Complete && (!t.final(sc.W, s.Last.String()) || pendingAtCompletion(s) != 0) {
			add("no false completion", s, trace(i))
		}
		if running(s) >= 2 {
			add("two commands past their intent at once", s, trace(i))
		}
		edges = next(r, t, sc, s, bound, edges[:0])
		for _, e := range edges {
			to := canon(e.to)
			j := reach(to, i, e.label)
			if e.op {
				opTo = append(opTo, j)
			}
			if e.unsafe != "" {
				if u := (modelUnsafe{e.unsafe, j}); !seenUnsafe[u] {
					seenUnsafe[u] = true
					add(e.unsafe, to, append(trace(i), e.label))
				}
			}
		}
	}
	opFrom = append(opFrom, int32(len(opTo)))
	n := nodes.n
	// The operator steps reversed, in one list too: the steps into node j come from the nodes
	// revFrom[revStart[j]:revStart[j+1]], in node order.
	revStart := make([]int32, n+1)
	for _, j := range opTo {
		revStart[j+1]++
	}
	for j := range n {
		revStart[j+1] += revStart[j]
	}
	revFrom := make([]int32, len(opTo))
	fill := slices.Clone(revStart[:n])
	for i := range n {
		for _, j := range opTo[opFrom[i]:opFrom[i+1]] {
			revFrom[fill[j]] = i
			fill[j]++
		}
	}
	good := make([]bool, n)
	var q []int32
	for i := range n {
		if s := &nodes.at(i).s; s.Row == 'R' && s.PrepLost != 'O' && s.PrepLost != 'N' {
			good[i] = true
			q = append(q, i)
		}
	}
	for len(q) > 0 {
		j := q[0]
		q = q[1:]
		for _, i := range revFrom[revStart[j]:revStart[j+1]] {
			if !good[i] {
				good[i] = true
				q = append(q, i)
			}
		}
	}
	for i := range n {
		if !good[i] {
			add("liveness", nodes.at(i).s, trace(i))
		}
	}
	return int(n), viol, differ
}

// modelRun is one rule set's run at one bound: its states and counterexamples in all, by
// workflow, and by class, the workflows each class was found in, the shortest trace of each class,
// and where the package's readings differed from the model's.
type modelRun struct {
	states, total int
	byWorkflow    map[string]int
	byClass       map[string]int
	classIn       map[string]map[string]int
	shortest      map[string]modelViolation
	differ        []string
}

func runModel(r modelRules, bound int, fail, ownRunOnly bool) modelRun {
	t := tableOf(r)
	out := modelRun{byWorkflow: map[string]int{}, byClass: map[string]int{}, classIn: map[string]map[string]int{},
		shortest: map[string]modelViolation{}}
	for _, sc := range scenarios(r, fail, ownRunOnly) {
		n, v, differ := explore(r, t, sc, bound)
		out.states += n
		out.total += len(v)
		out.byWorkflow[sc.Name] = n
		out.differ = append(out.differ, differ...)
		for _, x := range v {
			out.byClass[x.Class]++
			if out.classIn[x.Class] == nil {
				out.classIn[x.Class] = map[string]int{}
			}
			out.classIn[x.Class][x.Workflow]++
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

// pinnedBound2 is this model's run at bound 2 of the rules added since the bundle, with a command
// that fails for good modeled and a row lost in prepare: the current rules and their eight
// controls, the rules without WayOut, without EveryCommandStoppable, without HubAbortAfterStop,
// without DiskGoneRestore, without RestoreBack, without ReleasedPrepareAbort, without
// ReleasedNoJournalAbort, and with each stop recording its own run alone.
//
// Cost, measured with getrusage(RUSAGE_CHILDREN) in a local scoped run of the whole of
// TestTheHoldJournalModel at its default (every profile at bound 2, and v62 at bound 3),
// GOMAXPROCS=4 and -test.parallel=4, two large walks at once (failWalks) and a 4 GiB soft memory
// limit: peak RSS 829,824 kB, wall 18.1 s. Each large walk is about 1.3 to 1.5 million states.
var pinnedBound2 = map[string]modelCount{
	"v63":                        {1450140, 0, nil},
	"v63-WayOut":                 {1430444, 14517, map[string]int{classWayOut: 14517}},
	"v63-EveryCommandStoppable":  {1404646, 7058, map[string]int{classWayOut: 7058}},
	"v63-HubAbortAfterStop":      {666292, 151408, map[string]int{classWayOut: 151408}},
	"v63-DiskGoneRestore":        {1256666, 67238, map[string]int{classWayOut: 67238}},
	"v63-RestoreBack":            {853976, 41760, map[string]int{classWayOut: 41760}},
	"v63-ReleasedPrepareAbort":   {1448875, 1056, map[string]int{classOrphan: 1056}},
	"v63-ReleasedNoJournalAbort": {1449810, 495, map[string]int{classOrphan: 495}},
	"v63-OwnRunOnly":             {1459879, 144, map[string]int{classEveryRun: 144}},
}

// The current rules' runs by workflow, pinned from this model at bound 2. Bound 3 is not pinned:
// see TestTheHoldJournalModel's bound-3 comment.
var pinnedCurrentBound2 = map[string]int{"cutover": 55800, "rollback": 7197, "selection-recovery": 9701, "buckets-create": 1208,
	"buckets-recreate": 3016, "buckets-restore": 4403, "streams-update": 1208, "streams-census": 1588, "hub-restore": 1366019}

// failWalks holds the large walks, those with a command that fails for good (about 1.3 million
// states each at bound 2), to two at once, so the test's peak memory stays inside a CI runner
// whatever its CPU count.
var failWalks = make(chan struct{}, 2)

func TestTheModelCommandSetCanBeRangedTwice(t *testing.T) {
	seq := modelCmds(0).with(cmdBackup).with(cmdRelease).all()
	want := []modelCmd{cmdBackup, cmdRelease}
	for pass := range 2 {
		if got := slices.Collect(seq); !slices.Equal(got, want) {
			t.Errorf("pass %d: commands %v, want %v", pass, got, want)
		}
	}
}

// The visited set keeps states whose hashes collide apart: with one hash for every state, each new
// state goes under the next free key, and each state already added finds its own node.
func TestTheVisitedSetKeepsStatesWithOneHashApart(t *testing.T) {
	var states []modelState
	v := modelVisited{node: map[uint64]int32{}, hash: func(modelState) uint64 { return 7 }}
	stateOf := func(j int32) *modelState { return &states[j] }
	for i := range 3 {
		s := modelState{Row: 'H', NextID: int8(i)}
		_, h, ok := v.find(s, stateOf)
		if ok || h != uint64(7+i) {
			t.Fatalf("state %d is found %v, under the key %d; want it new, under the key %d", i, ok, h, 7+i)
		}
		v.node[h] = int32(len(states))
		states = append(states, s)
	}
	for i, s := range states {
		if j, h, ok := v.find(s, stateOf); !ok || j != int32(i) {
			t.Errorf("state %d is found %v as node %d under the key %d; want node %d", i, ok, j, h, i)
		}
	}
}

// The model of the hold's workflows, run on this package's journal and table. The frozen bundle's
// profiles reproduce its counts: v62 finds no counterexample at two crashes and restores per run
// (31,948 states) and at three (139,812), and every earlier rule set and every rule set with one of
// v62's fixes removed is a control that finds a counterexample of each class it names, asserted by
// the class's name, with the bundle's counts. The current rules add WayOut, EveryCommandStoppable,
// EndIsFinal, BackupCommand, HubAbortAfterStop, AbortAfterOwnStop, ReleasedPrepareAbort and
// ReleasedNoJournalAbort, and are run with a command that fails for good after its intent (in
// hub-restore the census, or a prove after a done prove, after which a done prove's disk may be
// lost) and a row lost in prepare and restored released: they find no counterexample at bound 2 by
// default, and at bound 3 with HOLDRECORD_MODEL_BOUND3=1, with pinned state counts. Their controls
// without WayOut, without EveryCommandStoppable, without HubAbortAfterStop, without DiskGoneRestore
// and without RestoreBack find the stranded hold (classWayOut): the first in every workflow but
// hub-restore, the second in every workflow that runs no buckets command, the last three in
// hub-restore. Their controls without ReleasedPrepareAbort and without ReleasedNoJournalAbort each
// find the hold a row lost in prepare orphans (classOrphan), with its journal and with none. Their
// control in which a stop records its own run alone (OwnRunOnly) finds hold release moving the row
// over an earlier run's change (classEveryRun). At every hub-restore state of the current rules the
// package's Step1, Proven, Epoch, Expected and Moved read the record as the model does.
func TestTheHoldJournalModel(t *testing.T) {
	// A soft memory limit keeps the heap near its live size if the model grows. The default run at
	// parallelism 4 peaks below 1 GiB RSS (pinnedBound2). Cleanup restores the limit after the
	// parallel subtests; a defer would run before they resume. Bound 3's opt-in walk has no measured
	// live size with this representation, so it runs without the limit in the capped scope below.
	if os.Getenv("HOLDRECORD_MODEL_BOUND3") != "1" {
		prev := debug.SetMemoryLimit(4 << 30)
		t.Cleanup(func() { debug.SetMemoryLimit(prev) })
	}
	if currentModel.Name != "v63" || currentModel.rules != currentRules || v62.WayOut || v62.EveryCommandStoppable || v62.EndIsFinal ||
		v62.BackupCommand || v62.HubAbortAfterStop || v62.AbortAfterOwnStop || v62.DiskGoneRestore || v62.RestoreBack ||
		v62.ReleasedPrepareAbort || v62.PrepareRowLoss || v62.ReleasedNoJournalAbort {
		t.Fatalf("the current rules are %q, the model's runs another table than the package's, or v62 carries a rule added since; the counts below are v62's and v63's", currentModel.Name)
	}
	for _, p := range modelProfiles() {
		p := p
		t.Run(p.Name, func(t *testing.T) {
			t.Parallel()
			if p.Fail { // the walks with a failing command are the large ones: at most two at once
				failWalks <- struct{}{}
				defer func() { <-failWalks }()
			}
			got := runModel(p.modelRules, 2, p.Fail, p.OwnRunOnly)
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
				if p.modelRules == currentModel {
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
				t.Logf("control %s found %d counterexamples of its class %s, in %v, the shortest (%s): %s", p.Name, n, c,
					got.classIn[c], got.shortest[c].Workflow, strings.Join(got.shortest[c].Trace, " -> "))
			}
			if p.Name == currentModel.Name+"-HubAbortAfterStop" && got.classIn[classWayOut]["hub-restore"] == 0 {
				// Without the rule a done prove excludes abort for good, so a census that fails for
				// good after a prove strands hub-restore.
				t.Errorf("control %s found no %s in hub-restore", p.Name, classWayOut)
			}
			if (p.Name == currentModel.Name+"-DiskGoneRestore" || p.Name == currentModel.Name+"-RestoreBack") && got.classIn[classWayOut]["hub-restore"] == 0 {
				// Without the restore onto another disk, an abort whose stop names the done prove's
				// disk gone can never pass its check, and no prove can be admitted that it reads;
				// without the restore back, an abort after a disk moved cannot pass its check either.
				t.Errorf("control %s found no %s in hub-restore", p.Name, classWayOut)
			}
			if p.Name == currentModel.Name+"-EveryCommandStoppable" {
				// The fix is every command's stop, not the buckets commands' alone: without it the
				// hold is stranded in workflows that run no buckets command.
				for _, w := range []string{"cutover", "rollback", "selection-recovery", "streams-update", "streams-census"} {
					if got.classIn[classWayOut][w] == 0 {
						t.Errorf("control %s found no %s in %s", p.Name, classWayOut, w)
					}
				}
			}
			if p.Name == currentModel.Name+"-WayOut" {
				// Without WayOut no workflow but hub-restore has a way out of a stopped command: the
				// record keeps each stop, as the code writes it, so hold release reads the stopped
				// command as a change in every workflow, the buckets ones included.
				for _, w := range []string{"cutover", "rollback", "selection-recovery", "buckets-create", "buckets-recreate",
					"buckets-restore", "streams-update", "streams-census"} {
					if got.classIn[classWayOut][w] == 0 {
						t.Errorf("control %s found no %s in %s", p.Name, classWayOut, w)
					}
				}
			}
		})
	}
	// Bound 3 of the frozen v62 is the bundle's own count, 139,812 states, and runs always. Bound 3
	// of the current rules runs only when HOLDRECORD_MODEL_BOUND3=1 is set, and it is not pinned.
	// Before compact states, a 64 GiB scope (systemd-run --user --scope -p MemoryMax=64G) had not
	// finished it after 17 minutes, at 56.4 GiB RSS, and was stopped. No count of it exists. When it
	// does run, it asserts no counterexample and logs its state count. Without the variable its
	// subtest is not created, so the package prints no skip.
	bound3 := []struct {
		r          modelRules
		fail       bool
		total      int
		byWorkflow map[string]int
	}{{v62, false, 139812, frozenV62Bound3}}
	if os.Getenv("HOLDRECORD_MODEL_BOUND3") == "1" {
		bound3 = append(bound3, struct {
			r          modelRules
			fail       bool
			total      int
			byWorkflow map[string]int
		}{currentModel, true, -1, nil})
	}
	for _, b := range bound3 {
		b := b
		t.Run(b.r.Name+" at bound 3", func(t *testing.T) {
			t.Parallel()
			got := runModel(b.r, 3, b.fail, false)
			if (b.total >= 0 && got.states != b.total) || got.total != 0 {
				t.Errorf("rules %s at bound 3: %d states and %d counterexamples, and the pinned count %d (-1: not pinned) and 0 (by workflow %v)",
					b.r.Name, got.states, got.total, b.total, got.byWorkflow)
			}
			t.Logf("rules %s at bound 3: %d states, by workflow %v", b.r.Name, got.states, got.byWorkflow)
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
