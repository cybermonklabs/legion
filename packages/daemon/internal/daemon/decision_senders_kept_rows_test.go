package daemon

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"slices"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/testtypes"
)

// This file plants each shape the rule for a list of kept decisions
// (decision_senders_kept_loops_test.go) must read, as a package of its own beside helpers every
// row may call, and holds the walk of each row to what the rule must read of it: whether each loop
// asks the predicate of each decision its pass reads, and each list it writes whole into text. It
// stands while TestDecisionSendersAskWhetherTheReviewIsStillThePersonsWord does; delete it with
// that test.

// plantedDecisionPackage is the import path the planted source is type-checked as, and
// plantedDecisionRender the function each row plants, as go/types names it.
const (
	plantedDecisionPackage = senderModule + "planted"
	plantedDecisionRender  = plantedDecisionPackage + ".render"
)

// plantedDecisionPreamble opens every row's source: the imports a row may use, and the helpers
// it may call, one that asks the predicate and two that ask nothing.
const plantedDecisionPreamble = `package planted

import (
	"fmt"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// stands asks the predicate whether d is still the person's word.
func stands(gate *record.DesignGate, d record.DecisionMark) bool {
	return workflow.SupersedingApproval(gate, true, workflow.Decision{Kind: d.Decision, Artifact: d.Artifact, Version: d.Version}) == nil
}

// line is how a planted task names d, asking nothing.
func line(d record.DecisionMark) string { return fmt.Sprintf("%s (%s by %s)", d.Ref, d.Decision, d.Actor) }

// joined is a planted task's text of what it names.
func joined(named []string) string { return strings.Join(named, "; ") }
`

// askingChain is two helpers a row asks through, as the workflow's keptSuperseded asks through
// RefSuperseded: superseded hands its mark's fields to asked inside a workflow.Decision, and asked
// hands that decision to the predicate.
const askingChain = `
// superseded asks, through asked, the predicate of mark.
func superseded(gate *record.DesignGate, mark record.DecisionMark) bool {
	return asked(gate, workflow.Decision{Kind: mark.Decision, Artifact: mark.Artifact, Version: mark.Version})
}

// asked asks the predicate of d.
func asked(gate *record.DesignGate, d workflow.Decision) bool {
	return workflow.SupersedingApproval(gate, true, d) != nil
}
`

// plantedDecisionSource type-checks planted source against the compiler's export data of the
// daemon's dependencies (compiledImports) through the check the three packages go through
// (testtypes.CheckFiles), and reads it as the walk reads them (newDecisionWalk). Its importer is
// not safe for concurrent use, so a test's rows take it in turn.
type plantedDecisionSource struct {
	fset    *token.FileSet
	imports types.Importer
}

func newPlantedDecisionSource(t *testing.T) plantedDecisionSource {
	t.Helper()
	fset, imports := compiledImports(t)
	return plantedDecisionSource{fset: fset, imports: imports}
}

// walk is the walk of decls, planted after plantedDecisionPreamble.
func (p plantedDecisionSource) walk(t *testing.T, decls string) *decisionWalk {
	t.Helper()
	file, err := parser.ParseFile(p.fset, "planted.go", plantedDecisionPreamble+decls, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse the planted source: %v", err)
	}
	checked, err := testtypes.CheckFiles(p.fset, p.imports, plantedDecisionPackage, []*ast.File{file})
	if err != nil {
		t.Fatalf("type-check the planted source: %v", err)
	}
	return newDecisionWalk(nil, &checkedPackage{name: "planted", files: checked.Files, fset: p.fset, info: checked.Info})
}

// plantedRow is one planted shape: a function render, with what the walk must read of it. asks
// is each loop of render that reads a kept decision, in source order, by whether its own pass asks
// the predicate of each decision it reads: false for a loop that names a decision unasked, or asks
// of another, and true for one that asks of each. renders is each list of kept decisions render
// writes whole into text, as written, in source order. A row that names neither plants a shape of
// neither, which the walk must not take for one.
type plantedRow struct {
	name    string
	asks    []bool
	renders []string
	source  string
}

// hold holds the walk of each row's source to what the row names.
func (p plantedDecisionSource) hold(t *testing.T, rows []plantedRow) {
	t.Helper()
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			walk := p.walk(t, row.source)
			render := walk.byName[plantedDecisionRender]
			if render == nil {
				t.Fatalf("the walk read no %s", short(plantedDecisionRender))
			}
			var asks []bool
			var read []string
			for _, loop := range render.keptLoops {
				asks = append(asks, loop.asks)
				read = append(read, fmt.Sprintf("%s asks %s of each decision it reads: %t, asking nothing of %v", loop.at,
					short(decisionPredicate), loop.asks, loop.unasked))
			}
			if !slices.Equal(asks, row.asks) {
				t.Errorf("the walk read render's loops that read a kept decision as %v, whether each asks %s of each decision its own "+
					"pass reads, want %v: %q", asks, short(decisionPredicate), row.asks, read)
			}
			var renders []string
			for _, written := range render.renders {
				renders = append(renders, written.expr)
			}
			if !slices.Equal(renders, row.renders) {
				t.Errorf("the walk read render as writing %q whole into text, want %q", renders, row.renders)
			}
		})
	}
}

// Each row plants a function render with loops that read kept decisions, and names, in source
// order, whether each of those loops asks the predicate of each decision its own pass reads.
func TestDecisionSendersSeeEveryLoopThatReadsAKeptDecision(t *testing.T) {
	t.Parallel()
	newPlantedDecisionSource(t).hold(t, []plantedRow{
		{name: "fmt.Sprintf of Ref and Actor alone", asks: []bool{false}, source: `
func render(marks []record.DecisionMark) string {
	var named []string
	for _, d := range marks {
		named = append(named, fmt.Sprintf("%s by %s", d.Ref, d.Actor))
	}
	return joined(named)
}
`},
		{name: "the whole decision handed to a formatting helper", asks: []bool{false}, source: `
func render(marks []record.DecisionMark) string {
	var named []string
	for _, d := range marks {
		named = append(named, line(d))
	}
	return joined(named)
}
`},
		{name: "the whole decision handed to a method", asks: []bool{false}, source: `
type task struct{ text strings.Builder }

func (t *task) add(d record.DecisionMark) {
	fmt.Fprintf(&t.text, "%s (%s by %s); ", d.Ref, d.Decision, d.Actor)
}

func render(marks []record.DecisionMark) string {
	var t task
	for _, d := range marks {
		t.add(d)
	}
	return t.text.String()
}
`},
		{name: "a switch on Decision that writes constants", asks: []bool{false}, source: `
func render(marks []record.DecisionMark) string {
	var text strings.Builder
	for _, d := range marks {
		switch d.Decision {
		case record.DecisionDesignApproved:
			text.WriteString("A person approved the spec. ")
		case record.DecisionDesignChangesRequested:
			text.WriteString("A person requested changes to the spec. ")
		}
	}
	return text.String()
}
`},
		{name: "fmt.Sprint of the whole decision", asks: []bool{false}, source: `
func render(marks []record.DecisionMark) string {
	var named []string
	for _, d := range marks {
		named = append(named, fmt.Sprint(d))
	}
	return joined(named)
}
`},
		{name: "a C-style for that indexes the list", asks: []bool{false}, source: `
func render(marks []record.DecisionMark) string {
	var named []string
	for i := 0; i < len(marks); i++ {
		named = append(named, fmt.Sprintf("%s (%s by %s)", marks[i].Ref, marks[i].Decision, marks[i].Actor))
	}
	return joined(named)
}
`},
		{name: "a range over an iterator function", asks: []bool{false}, source: `
func render(seq func(yield func(record.DecisionMark) bool)) string {
	var named []string
	for d := range seq {
		named = append(named, fmt.Sprintf("%s (%s by %s)", d.Ref, d.Decision, d.Actor))
	}
	return joined(named)
}
`},
		{name: "a range over a map", asks: []bool{false}, source: `
func render(byRef map[string]record.DecisionMark) string {
	var named []string
	for _, d := range byRef {
		named = append(named, fmt.Sprintf("%s (%s by %s)", d.Ref, d.Decision, d.Actor))
	}
	return joined(named)
}
`},
		{name: "a decision read only in the clause of a loop nested in it", asks: []bool{false}, source: `
func render(marks []record.DecisionMark) string {
	var text strings.Builder
	for _, d := range marks {
		for _, part := range []string{d.Ref, d.Actor} {
			text.WriteString(part + "; ")
		}
	}
	return text.String()
}
`},
		{name: "a loop that asks the predicate of each decision it names", asks: []bool{true}, source: `
func render(gate *record.DesignGate, marks []record.DecisionMark) string {
	var named []string
	for _, d := range marks {
		if workflow.SupersedingApproval(gate, true, workflow.Decision{Kind: d.Decision, Artifact: d.Artifact, Version: d.Version}) != nil {
			continue
		}
		named = append(named, fmt.Sprintf("%s (%s by %s)", d.Ref, d.Decision, d.Actor))
	}
	return joined(named)
}
`},
		{name: "a loop that asks through a helper that asks", asks: []bool{true}, source: `
func render(gate *record.DesignGate, marks []record.DecisionMark) string {
	var named []string
	for _, d := range marks {
		if stands(gate, d) {
			named = append(named, fmt.Sprintf("%s (%s by %s)", d.Ref, d.Decision, d.Actor))
		}
	}
	return joined(named)
}
`},
		{name: "a predicate call before the loop does not ask for it", asks: []bool{false}, source: `
func render(gate *record.DesignGate, first record.DecisionMark, marks []record.DecisionMark) string {
	if workflow.SupersedingApproval(gate, true, workflow.Decision{Kind: first.Decision, Artifact: first.Artifact, Version: first.Version}) != nil {
		return ""
	}
	var named []string
	for _, d := range marks {
		named = append(named, fmt.Sprintf("%s (%s by %s)", d.Ref, d.Decision, d.Actor))
	}
	return joined(named)
}
`},
		{name: "a predicate call in a nested loop asks for that loop alone", asks: []bool{false, true}, source: `
func render(gates map[record.DesignStep]record.DesignGate, marks []record.DecisionMark) (string, int) {
	var named []string
	spent := 0
	for _, d := range marks {
		named = append(named, fmt.Sprintf("%s (%s by %s)", d.Ref, d.Decision, d.Actor))
		for step, gate := range gates {
			if workflow.SupersedingApproval(&gate, true, workflow.Decision{Kind: d.Decision, Step: step, Artifact: d.Artifact, Version: d.Version}) != nil {
				spent++
			}
		}
	}
	return joined(named), spent
}
`},
		{name: "a loop over owed starts named kept reads no kept decision", source: `
type owedRow struct{ issue, token string }

func render(kept []owedRow) []string {
	var keys []string
	for _, keptRow := range kept {
		keys = append(keys, keptRow.issue+"/"+keptRow.token)
	}
	return keys
}
`},
		{name: "an ask of a fixed decision in the loop asks nothing of the loop's own", asks: []bool{false}, source: `
func render(gate *record.DesignGate, first record.DecisionMark, marks []record.DecisionMark) string {
	var named []string
	for _, d := range marks {
		if workflow.SupersedingApproval(gate, true, workflow.Decision{Kind: first.Decision, Artifact: first.Artifact, Version: first.Version}) != nil {
			continue
		}
		named = append(named, line(d))
	}
	return joined(named)
}
`},
		{name: "an ask of the list's first element in a loop over the list", asks: []bool{false}, source: `
func render(gate *record.DesignGate, marks []record.DecisionMark) string {
	var named []string
	for _, d := range marks {
		if stands(gate, marks[0]) {
			named = append(named, line(d))
		}
	}
	return joined(named)
}
`},
		{name: "an ask of the kept approval in a loop over the decisions", asks: []bool{false}, source: `
func render(gate *record.DesignGate, owed record.KeptDecisions) string {
	var named []string
	for _, d := range owed.Decisions {
		if workflow.SupersedingApproval(gate, true, workflow.Decision{Kind: record.DecisionDesignApproved, Artifact: owed.Artifact, Version: owed.Approved}) != nil {
			continue
		}
		named = append(named, line(d))
	}
	return joined(named)
}
`},
		{name: "an ask of another list's element at the loop's index", asks: []bool{false}, source: `
func render(gate *record.DesignGate, marks, earlier []record.DecisionMark) string {
	var named []string
	for i := range marks {
		if i < len(earlier) && stands(gate, earlier[i]) {
			named = append(named, line(marks[i]))
		}
	}
	return joined(named)
}
`},
		{name: "a C-style for that asks of a fixed index", asks: []bool{false}, source: `
func render(gate *record.DesignGate, marks []record.DecisionMark) string {
	var named []string
	for i := 0; i < len(marks); i++ {
		if stands(gate, marks[0]) {
			named = append(named, line(marks[i]))
		}
	}
	return joined(named)
}
`},
		{name: "a helper that asks of its gate alone asks nothing of the decision it is handed", asks: []bool{false}, source: `
// approvalStands asks the predicate of the gate's approval, and of nothing of d.
func approvalStands(gate *record.DesignGate, d record.DecisionMark) bool {
	return workflow.SupersedingApproval(gate, true, workflow.Decision{Kind: record.DecisionDesignApproved, Artifact: gate.ArtifactID}) == nil
}

func render(gate *record.DesignGate, marks []record.DecisionMark) string {
	var named []string
	for _, d := range marks {
		if approvalStands(gate, d) {
			named = append(named, line(d))
		}
	}
	return joined(named)
}
`},
		{name: "a chain of helpers that asks of the decision it is handed", asks: []bool{true}, source: askingChain + `
func render(gates map[string]*record.DesignGate, marks []record.DecisionMark) string {
	var named []string
	for _, d := range marks {
		if superseded(gates[d.Issue], d) {
			continue
		}
		named = append(named, line(d))
	}
	return joined(named)
}
`},
		{name: "the same chain asked of a fixed decision", asks: []bool{false}, source: askingChain + `
func render(gates map[string]*record.DesignGate, first record.DecisionMark, marks []record.DecisionMark) string {
	var named []string
	for _, d := range marks {
		if superseded(gates[d.Issue], first) {
			continue
		}
		named = append(named, line(d))
	}
	return joined(named)
}
`},
		{name: "a range over a list's indexes that asks of the element at each", asks: []bool{true}, source: `
func render(gate *record.DesignGate, marks []record.DecisionMark) string {
	var named []string
	for i := range marks {
		if stands(gate, marks[i]) {
			named = append(named, line(marks[i]))
		}
	}
	return joined(named)
}
`},
		{name: "a C-style for that asks of the element at its index", asks: []bool{true}, source: `
func render(gate *record.DesignGate, marks []record.DecisionMark) string {
	var named []string
	for i := 0; i < len(marks); i++ {
		if stands(gate, marks[i]) {
			named = append(named, fmt.Sprintf("%s (%s by %s)", marks[i].Ref, marks[i].Decision, marks[i].Actor))
		}
	}
	return joined(named)
}
`},
		{name: "a range over a map's keys that asks of the value at each", asks: []bool{true}, source: `
func render(gate *record.DesignGate, byRef map[string]record.DecisionMark) string {
	var named []string
	for ref := range byRef {
		if stands(gate, byRef[ref]) {
			named = append(named, line(byRef[ref]))
		}
	}
	return joined(named)
}
`},
		{name: "a range over a map that asks of each value", asks: []bool{true}, source: `
func render(gate *record.DesignGate, byRef map[string]record.DecisionMark) string {
	var named []string
	for _, d := range byRef {
		if stands(gate, d) {
			named = append(named, line(d))
		}
	}
	return joined(named)
}
`},
		{name: "a range over a map keyed by decisions that asks of each key", asks: []bool{true}, source: `
func render(gate *record.DesignGate, told map[record.DecisionMark]bool) string {
	var named []string
	for d, done := range told {
		if !done && stands(gate, d) {
			named = append(named, line(d))
		}
	}
	return joined(named)
}
`},
		{name: "a range over a wrapper's list that asks of the element at each index", asks: []bool{true}, source: `
func render(gate *record.DesignGate, owed record.KeptDecisions) string {
	var named []string
	for i := range owed.Decisions {
		if stands(gate, owed.Decisions[i]) {
			named = append(named, line(owed.Decisions[i]))
		}
	}
	return joined(named)
}
`},
		{name: "a method that asks of the decision its receiver holds", asks: []bool{true}, source: `
type owedMark struct{ mark record.DecisionMark }

// stands asks the predicate of the mark o holds.
func (o owedMark) stands(gate *record.DesignGate) bool {
	return workflow.SupersedingApproval(gate, true, workflow.Decision{Kind: o.mark.Decision, Artifact: o.mark.Artifact, Version: o.mark.Version}) == nil
}

func render(gate *record.DesignGate, marks []owedMark) string {
	var named []string
	for _, o := range marks {
		if o.stands(gate) {
			named = append(named, line(o.mark))
		}
	}
	return joined(named)
}
`},
	})
}

// Each row plants a function render and names, in source order, each list of kept decisions it
// writes whole into text, in a loop or outside one, and whether each of its loops that reads a
// kept decision asks of each.
func TestDecisionSendersSeeEveryKeptListWrittenWhole(t *testing.T) {
	t.Parallel()
	newPlantedDecisionSource(t).hold(t, []plantedRow{
		{name: "fmt.Sprintf of a whole list outside any loop", renders: []string{"marks"}, source: `
func render(marks []record.DecisionMark) string {
	return fmt.Sprintf("decided: %v", marks)
}
`},
		{name: "the owed wrapper's list printed whole", renders: []string{"owed.Decisions"}, source: `
func render(owed record.KeptDecisions) string {
	return fmt.Sprintf("%v", owed.Decisions)
}
`},
		{name: "the owed wrapper printed whole through its pointer", renders: []string{"owed"}, source: `
func render(owed *record.KeptDecisions) string {
	return fmt.Sprint(owed)
}
`},
		{name: "a field of a type of the function's own that holds the owed wrapper", renders: []string{"t.owed"}, source: `
type tellTree struct {
	tree string
	owed record.KeptDecisions
}

func render(t tellTree) string {
	return fmt.Sprintf("%s: %v", t.tree, t.owed)
}
`},
		{name: "a whole list handed to a helper that prints its any parameter", renders: []string{"owed.Decisions"}, source: `
// listed prints whatever it is given.
func listed(items any) string { return fmt.Sprint(items) }

func render(owed record.KeptDecisions) string {
	return "decided: " + listed(owed.Decisions)
}
`},
		{name: "a whole list handed to a method that prints its any parameter", renders: []string{"marks"}, source: `
type task struct{ text strings.Builder }

// list writes whatever it is given into the task's text.
func (t *task) list(items any) { fmt.Fprint(&t.text, items) }

func render(marks []record.DecisionMark) string {
	var t task
	t.list(marks)
	return t.text.String()
}
`},
		{name: "a whole list converted to any at the print", renders: []string{"any(marks)"}, source: `
func render(marks []record.DecisionMark) string {
	return fmt.Sprint(any(marks))
}
`},
		{name: "an error that prints a whole list", renders: []string{"marks"}, source: `
func render(marks []record.DecisionMark) error {
	return fmt.Errorf("owed %v", marks)
}
`},
		{name: "a map keyed by decisions printed whole", renders: []string{"told"}, source: `
func render(told map[record.DecisionMark]bool) string {
	return fmt.Sprint(told)
}
`},
		{name: "a map whose values are decisions printed whole", renders: []string{"byRef"}, source: `
func render(byRef map[string]record.DecisionMark) string {
	return fmt.Sprint(byRef)
}
`},
		{name: "a map of lists printed list by list", renders: []string{"marks"}, source: `
func render(byIssue map[string][]record.DecisionMark) string {
	var text strings.Builder
	for issue, marks := range byIssue {
		fmt.Fprintf(&text, "%s: %v; ", issue, marks)
	}
	return text.String()
}
`},
		{name: "a whole list printed in a loop that asks of its own decision", asks: []bool{true}, renders: []string{"marks"}, source: `
func render(gate *record.DesignGate, marks []record.DecisionMark) string {
	var named []string
	for _, d := range marks {
		if stands(gate, d) {
			named = append(named, fmt.Sprint(marks))
		}
	}
	return joined(named)
}
`},
		{name: "a type that holds a decision past a cycle of its own printed whole", renders: []string{"t"}, source: `
type thread struct {
	replies []*thread
	mark    record.DecisionMark
}

func render(t *thread) string {
	return fmt.Sprint(t)
}
`},
		{name: "a type of a cycle of its own that holds no decision is no list", source: `
type chain struct {
	tree string
	next *chain
}

func render(c *chain) string {
	return fmt.Sprint(c)
}
`},
		{name: "a helper that prints only the wrapper's approval writes no list", source: `
// approval names the version owed's approval stands at, and reads nothing of its decisions.
func approval(owed record.KeptDecisions) string {
	return fmt.Sprintf("the %s is approved at version %d", owed.Step, owed.Approved)
}

func render(owed record.KeptDecisions) string {
	return approval(owed) + fmt.Sprintf(" with %d decisions", len(owed.Decisions))
}
`},
		{name: "a loop over wrappers that prints only each one's approval reads no decision", source: `
func render(owed []record.KeptDecisions) []string {
	var lines []string
	for _, o := range owed {
		lines = append(lines, fmt.Sprintf("the %s at version %d", o.Step, o.Approved))
	}
	return lines
}
`},
	})
}
