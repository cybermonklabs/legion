package replay

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// The commits of the two recorded releases whose rows today's table lacks: the conflict rows of
// the release the rule legion-v20.42.5 comes from, and the awaiting_merge, ready and checks_red of
// the release the rule legion-v19.57.3 comes from.
const (
	conflictRelease      = "958173da0ea4cd03290a1964edf9eb92e6685200"
	awaitingMergeRelease = "9542c6fbbecd184ccee73523ab3defa39d6cf23a"
)

// replayedRule is the rule the repository keeps under name (record.SlotRuleNamed), which the test
// fails without.
func replayedRule(t *testing.T, name string) record.SlotRule {
	t.Helper()
	rule, kept := record.SlotRuleNamed(name)
	if !kept {
		t.Fatalf("the repository keeps no slot rule named %s", name)
	}
	return rule
}

// recordedRow is one move a recorded window can hold, as a row's three words, and where it is
// read from: a table a replay runs, or a recorded release whose rows today's table lacks.
type recordedRow struct {
	From    phase.Phase
	Trigger workflow.TriggerKind
	To      phase.Phase
	Source  string
}

func (r recordedRow) String() string {
	return fmt.Sprintf("%s -> %s by %s (%s)", r.From, r.To, r.Trigger, r.Source)
}

// releaseRows is the rows of the recorded releases that today's table lacks, held as data: the
// conflict rows of the release at 958173da (from testing, reviewing, accepting, retro and merging
// back to implementing), and, of the release at 9542c6fb, the merger's ready move into
// awaiting_merge, the merge that closed an issue from there, and checks_red back to implementing
// from every phase a pull request's checks can turn red in.
func releaseRows() []recordedRow {
	var rows []recordedRow
	for _, from := range []phase.Phase{phase.Testing, phase.Reviewing, phase.Accepting, phase.Retro, phase.Merging} {
		rows = append(rows, recordedRow{From: from, Trigger: workflow.TriggerConflict, To: phase.Implementing, Source: "the release at " + conflictRelease})
	}
	rows = append(rows,
		recordedRow{From: phase.Merging, Trigger: TriggerReady, To: PhaseAwaitingMerge, Source: "the release at " + awaitingMergeRelease},
		recordedRow{From: PhaseAwaitingMerge, Trigger: workflow.TriggerPullRequestMerged, To: phase.Done, Source: "the release at " + awaitingMergeRelease})
	for _, from := range append(slices.Clone(phase.Order[slices.Index(phase.Order, phase.Implementing)+1:]), PhaseAwaitingMerge) {
		rows = append(rows, recordedRow{From: from, Trigger: workflow.TriggerChecksRed, To: phase.Implementing, Source: "the release at " + awaitingMergeRelease})
	}
	return rows
}

// folds is the mapping of the release at 9542c6fb onto today's phases (assumption 19): its
// awaiting_merge is read as merging.
var folds = map[phase.Phase]phase.Phase{PhaseAwaitingMerge: phase.Merging}

// fold is p read on today's phases.
func fold(p phase.Phase) phase.Phase {
	if today, folded := folds[p]; folded {
		return today
	}
	return p
}

// recordedRows is every move a window this tool replays can record: each row of the table of
// today's line under main, under legion-v20.42.5 and under legion-v19.57.3 (workflow.TableFor,
// over phase.Order, the line of every issue of a recorded window, which ran before plan rows
// existed), and each row of releaseRows, once each.
func recordedRows(t *testing.T) []recordedRow {
	t.Helper()
	var rows []recordedRow
	seen := make(map[[3]string]bool)
	add := func(row recordedRow) {
		key := [3]string{string(row.From), string(row.Trigger), string(row.To)}
		if !seen[key] {
			seen[key] = true
			rows = append(rows, row)
		}
	}
	for _, name := range []string{record.MainRule.Name, "legion-v20.42.5", "legion-v19.57.3"} {
		for _, row := range workflow.TableFor(replayedRule(t, name), phase.Order) {
			add(recordedRow{From: row.From, Trigger: row.Trigger, To: row.To, Source: "the table of " + name})
		}
	}
	for _, row := range releaseRows() {
		add(row)
	}
	return rows
}

// wantMove is how the replay must make the moves of one trigger: the way, the fact of a move a
// daemon handler makes, and for a move of a recorded release, the phase it goes to whatever it
// left, the phase it folds and the assumptions its mapping rests on.
type wantMove struct {
	way         MoveWay
	fact        string
	to          phase.Phase
	folds       phase.Phase
	assumptions []int
}

// wantMoves is the decision of the design for each trigger. gate_opened is the pass's own
// promotion and never injected. The completions a handoff or a review settles go through the
// engine's ReplayTransition at the end of their segment, as does the release at 9542c6fb's
// checks_red, which moved an issue back to implementing from any phase (assumption 21). A
// sign-off, a merge and a backward move go through the daemon's own handler of the fact the API or
// GitHub applies, so the guards that handler keeps hold. A conflict is drawn at the fitted rate
// and its recorded moves are not injected (assumptions 3 and 23). The ready move of the release at
// 9542c6fb is a mark inside merging, which folds that release's awaiting_merge into merging
// (assumptions 19 and 20).
var wantMoves = map[workflow.TriggerKind]wantMove{
	workflow.TriggerGateOpened:          {way: MovePass},
	workflow.TriggerPlannerCompleted:    {way: MoveTransition},
	workflow.TriggerImplementationReady: {way: MoveTransition},
	workflow.TriggerTesterPassed:        {way: MoveTransition},
	workflow.TriggerTesterFailed:        {way: MoveTransition},
	workflow.TriggerReviewApproved:      {way: MoveTransition},
	workflow.TriggerReviewRejected:      {way: MoveTransition},
	workflow.TriggerRetroCompleted:      {way: MoveTransition},
	workflow.TriggerSignOff:             {way: MoveFact, fact: "intake.SignOff"},
	workflow.TriggerPullRequestMerged:   {way: MoveFact, fact: "intake.PullRequestMerged"},
	workflow.TriggerBackward:            {way: MoveFact, fact: "intake.BackwardMove"},
	workflow.TriggerConflict:            {way: MoveDraw, to: phase.Implementing, assumptions: []int{3, 23}},
	workflow.TriggerChecksRed:           {way: MoveTransition, to: phase.Implementing, assumptions: []int{21}},
	TriggerReady:                        {way: MoveMark, folds: PhaseAwaitingMerge, assumptions: []int{19, 20}},
}

// A replay makes every move a recorded window can hold, and the trace builder's map from a
// recorded move to the move a replay makes (MoveSources) says how for each trigger: every row of
// the tables a replay runs (workflow.Table and the rows workflow.TableFor adds for the rules
// legion-v20.42.5 and legion-v19.57.3, read as data, so a row added there fails here until it has a
// source), and the rows of the two recorded releases that today's table lacks, held as data
// (releaseRows). The test fails for a row whose trigger has no entry, for an entry that differs
// from the way the design decides for its trigger (wantMoves) or that names no row, and for a
// mapping of the release at 9542c6fb that does not land on today's phases: awaiting_merge read as
// merging, so its merge is merging's and its ready move a mark that moves nothing. The rules
// legion-v20.42.5 and legion-v19.57.3 are the ones the repository keeps (record.SlotRuleNamed).
func TestEveryRowOfTheTableHasASourceInTheRecord(t *testing.T) {
	t.Parallel()
	facts := intakeFactTypes(t)
	rows := recordedRows(t)
	triggers := make(map[workflow.TriggerKind]bool)
	for _, row := range rows {
		triggers[row.Trigger] = true
		if _, decided := wantMoves[row.Trigger]; !decided {
			t.Errorf("the row %s has a trigger this test holds no decision for: add how a replay makes it to wantMoves", row)
		}
		source, sourced := MoveSources[row.Trigger]
		if !sourced {
			t.Errorf("the row %s has no source in the record: MoveSources holds no entry for %s", row, row.Trigger)
			continue
		}
		if folded := fold(row.To); folded != row.To && fold(row.From) == folded && source.Way != MoveMark {
			t.Errorf("the row %s enters %s, which is read as %s, the phase it left, so the move moves nothing: its source must be a mark, and MoveSources makes it by %s",
				row, row.To, folded, source.Way)
		}
	}
	for _, row := range rows {
		from, to := fold(row.From), fold(row.To)
		if from == row.From && to == row.To || from == to {
			continue
		}
		if !slices.ContainsFunc(rows, func(other recordedRow) bool {
			return other.From == from && other.Trigger == row.Trigger && other.To == to
		}) {
			t.Errorf("the row %s is read on today's phases as %s -> %s by %s, which no table and no recorded release holds", row, from, to, row.Trigger)
		}
	}
	for _, trigger := range slices.Sorted(maps.Keys(MoveSources)) {
		source := MoveSources[trigger]
		if !triggers[trigger] {
			t.Errorf("MoveSources holds an entry for %s, which names no row of a table a replay runs nor of a recorded release", trigger)
		}
		if source.Line != phaseChangedLine {
			t.Errorf("MoveSources reads the moves of %s from the line %q, want %q, the line every phase move writes", trigger, source.Line, phaseChangedLine)
		}
		for _, number := range source.Assumptions {
			if !listsAssumption(number) {
				t.Errorf("MoveSources maps the moves of %s under assumption %d, which Assumptions lacks", trigger, number)
			}
		}
		want, decided := wantMoves[trigger]
		if !decided {
			continue
		}
		if source.Way != want.way {
			t.Errorf("MoveSources makes the moves of %s by %s, want %s", trigger, source.Way, want.way)
		}
		if want.fact != "" && source.Fact != want.fact {
			t.Errorf("MoveSources makes the moves of %s with the fact %q, want %q, the fact whose handler keeps that move's guards", trigger, source.Fact, want.fact)
		}
		switch {
		case source.Way == MoveFact || source.Way == MoveMark:
			if name, ofIntake := strings.CutPrefix(source.Fact, "intake."); ofIntake && !slices.Contains(facts, name) {
				t.Errorf("MoveSources makes the moves of %s with %s, which is no fact type of package intake", trigger, source.Fact)
			}
			if source.Fact == "" {
				t.Errorf("MoveSources makes the moves of %s by %s and names no fact", trigger, source.Way)
			}
		case source.Fact != "":
			t.Errorf("MoveSources makes the moves of %s by %s and names the fact %q, which that way applies none of", trigger, source.Way, source.Fact)
		}
		if source.To != want.to {
			t.Errorf("MoveSources moves %s to %q whatever the issue left, want %q", trigger, source.To, want.to)
		}
		if source.Folds != want.folds {
			t.Errorf("MoveSources folds %q into the phase a move of %s left, want %q (assumption 19)", source.Folds, trigger, want.folds)
		}
		for _, number := range want.assumptions {
			if !slices.Contains(source.Assumptions, number) {
				t.Errorf("MoveSources maps the moves of %s under the assumptions %v, which lack %d", trigger, source.Assumptions, number)
			}
		}
	}
}

// nonDispatchFacts is the facts the daemon's producers other than its Dispatch consumer apply —
// its API, its ticks and boots, its outbox executors, supervision, the launcher and GitHub intake,
// each through intake.ApplyFact — named here as data. The test reads every fact type from package
// intake's source, and these must be among them: a parse that found fewer would pass over what it
// missed.
var nonDispatchFacts = []string{
	"DispatchConsumerPosition", "EntrySweep", "ControllerTick", "DaemonBooted", "CutoverMergerRepair", "CatchUpApplied",
	"GateRequestFound", "LingerExpired", "Handover", "ClaimReady", "ClaimFailed", "TaskConfirmed", "SessionLost",
	"SessionStoreFault", "WorkerSilent", "WorkerBackgroundStalled", "ProjectResumed", "HandoffComplete", "CheckReport",
	"BackwardMove", "RetryOrEscalate", "SignOff", "CloseRoot", "GateRegistered", "ChildAdoption", "RestartPhase",
	"DispatchIssue", "PullRequestOpened", "PullRequestSynchronized", "PullRequestReview", "PullRequestChecks",
	"PullRequestMergeability", "PullRequestMerged", "PullRequestClosed", "PullRequestDequeued", "Push", "BasePush",
	"PullRequestMissing", "MergeQueueReadDone", "ConflictConfirmDone", "GateAboveDispatch", "DispatchArtifact",
	"CheckLookEnded", "CheckWakeDue", "CheckBound", "ListenerWake", "PodBackstopDue", "MemoryGuardKill", "VolumeNearFull",
	"ControllerMemoryGuardKill", "PullRequestReviewRequests", "PullRequestMergeStateUnread",
	"PullRequestEnqueued", "QueueTimeline", "QueueBootStatus", "ApprovalsBootRead", "ModelRequests", "SignalDeadline", "TurnEdge",
}

// wantFacts is the way the design decides for the facts it names: the API's sign-off, backward
// move, close, retry, release of children and gate registration, a failed claim's hold and a
// merge rebuilt from the record and injected, and the facts only a Dispatch event brings decoded
// from the recorded events; the tick, the boot, a linger's close and a hand-over made by the
// replay's own driver and worker stub as the daemon's own producers make them.
var wantFacts = map[string]FactWay{
	"SignOff": FactInjected, "BackwardMove": FactInjected, "CloseRoot": FactInjected, "RetryOrEscalate": FactInjected,
	"ChildAdoption": FactInjected, "GateRegistered": FactInjected, "ClaimFailed": FactInjected, "PullRequestMerged": FactInjected,
	"DispatchAskOpened": FactInjected, "DispatchAskRestored": FactInjected, "DispatchAskClosed": FactInjected,
	"DispatchAskReply": FactInjected, "DispatchApprovalMoved": FactInjected, "DispatchAskTurn": FactInjected, "DispatchClaim": FactInjected,
	"ControllerTick": FactProduced, "DaemonBooted": FactProduced, "LingerExpired": FactProduced, "Handover": FactProduced,
}

// A replay feeds the daemon's chain every kind of fact the daemon applies, or says why it leaves
// one out: each fact type of package intake, read from its source as every type with the method
// intake.Fact names (intakeFactTypes) — the ones Dispatch events decode to and the ones the
// daemon's other producers apply, its API, ticks, boots, outbox executors, supervision and GitHub
// intake among them — is injected by the replay from the record, produced by the replay as the
// daemon's own producer produces it, named as the act of a release later than every recorded
// window (FactUnrecorded), or left out under a named assumption (FactSources). It fails for a
// type with no entry, for an entry that names no fact type, for an entry left out under no
// assumption the tool's list holds, and for a way that differs from the design's (wantFacts).
func TestEveryFactANonDispatchProducerAppliesHasASource(t *testing.T) {
	t.Parallel()
	facts := intakeFactTypes(t)
	for _, name := range nonDispatchFacts {
		if !slices.Contains(facts, name) {
			t.Errorf("package intake's source declares no fact type %s, which a producer of the daemon applies: the read of its fact types missed it", name)
		}
	}
	for _, name := range facts {
		source, sourced := FactSources[name]
		if !sourced {
			t.Errorf("the fact intake.%s has no source in the replay: FactSources holds no entry for it", name)
			continue
		}
		switch source.Way {
		case FactInjected, FactProduced, FactUnrecorded:
			if source.From == "" {
				t.Errorf("FactSources %s intake.%s and says from nothing", source.Way, name)
			}
			if source.Assumption != 0 {
				t.Errorf("FactSources %s intake.%s and names assumption %d, which only a fact left out is under", source.Way, name, source.Assumption)
			}
		case FactLeftOut:
			if !listsAssumption(source.Assumption) {
				t.Errorf("FactSources leaves intake.%s out under assumption %d, which Assumptions lacks", name, source.Assumption)
			}
		default:
			t.Errorf("FactSources comes by intake.%s by %q, which is no FactWay", name, source.Way)
		}
		if want, decided := wantFacts[name]; decided && source.Way != want {
			t.Errorf("FactSources comes by intake.%s by %s, want %s", name, source.Way, want)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(FactSources)) {
		if !slices.Contains(facts, name) {
			t.Errorf("FactSources holds an entry for %s, which is no fact type of package intake", name)
		}
	}
}

// intakeDir is where package intake's source stands beside this package's.
const intakeDir = "../intake"

// intakeFactTypes is the name of every fact type package intake declares, read from the package's
// own non-test source as data: the method intake.Fact's interface names, and every type with a
// method of that name, so a fact type a later change adds is read with the rest. It fails rather
// than answer none.
func intakeFactTypes(t *testing.T) []string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(filepath.FromSlash(intakeDir), "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	files := token.NewFileSet()
	var parsed []*ast.File
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(files, path, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		parsed = append(parsed, file)
	}
	method := ""
	for _, file := range parsed {
		for _, decl := range file.Decls {
			types, isGen := decl.(*ast.GenDecl)
			if !isGen || types.Tok != token.TYPE {
				continue
			}
			for _, spec := range types.Specs {
				spec := spec.(*ast.TypeSpec)
				if spec.Name.Name != "Fact" {
					continue
				}
				if fact, isInterface := spec.Type.(*ast.InterfaceType); isInterface && len(fact.Methods.List) == 1 && len(fact.Methods.List[0].Names) == 1 {
					method = fact.Methods.List[0].Names[0].Name
				}
			}
		}
	}
	if method == "" {
		t.Fatalf("package intake's source under %s declares no interface Fact of one method", intakeDir)
	}
	names := make(map[string]bool)
	for _, file := range parsed {
		for _, decl := range file.Decls {
			fn, isFunc := decl.(*ast.FuncDecl)
			if !isFunc || fn.Recv == nil || len(fn.Recv.List) != 1 || fn.Name.Name != method {
				continue
			}
			receiver := fn.Recv.List[0].Type
			if star, isPointer := receiver.(*ast.StarExpr); isPointer {
				receiver = star.X
			}
			if ident, isIdent := receiver.(*ast.Ident); isIdent {
				names[ident.Name] = true
			}
		}
	}
	if len(names) == 0 {
		t.Fatalf("package intake's source under %s declares no type with the method %s of intake.Fact", intakeDir, method)
	}
	return slices.Sorted(maps.Keys(names))
}
