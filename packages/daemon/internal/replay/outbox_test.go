package replay

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"strconv"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/record"
)

// outboxSource is the file of package record that declares every outbox kind and the supervise
// ops a supervise row may carry, beside this package's.
const outboxSource = "../record/outbox.go"

// wantRows is the handling the design decides for the rows it names: a linger's close applies
// the linger's expiry at the row's own time, a gate seed applies the artifact read it seeds or the
// reset of a gate above Dispatch's version, and a status write becomes the Dispatch event its
// write makes, and a hand-over's write the hand-over; a start opens the issue's next recorded
// segment, and a suspend and the two closes pause or cancel its segments. Each is executed.
var wantRows = map[RowKind]RowHandling{
	{Kind: record.OutboxKindLingerClose}:                  {Way: OutboxExecuted, Facts: []string{"LingerExpired"}},
	{Kind: record.OutboxKindGateSeed}:                     {Way: OutboxExecuted, Facts: []string{"DispatchArtifact", "GateAboveDispatch"}},
	{Kind: record.OutboxKindDispatchStatus}:               {Way: OutboxExecuted, Facts: []string{"DispatchIssue", "Handover"}},
	{Kind: record.OutboxKindSupervise, Op: "start"}:       {Way: OutboxExecuted},
	{Kind: record.OutboxKindSupervise, Op: "suspend"}:     {Way: OutboxExecuted},
	{Kind: record.OutboxKindSupervise, Op: "issue_close"}: {Way: OutboxExecuted},
	{Kind: record.OutboxKindSupervise, Op: "tree_close"}:  {Way: OutboxExecuted},
}

// Several of the daemon's outbox executors apply a fact back into the daemon, and a supervise row
// starts or stops the work a replay times, so a replay's worker stub cannot mark every row done:
// every record.OutboxKind and every supervise op, read from record/outbox.go as data (its
// OutboxKind constants and the ops validSuperviseOp takes), is either executed by the stub, with
// the facts its executor applies, or finished under a named assumption (OutboxHandling). It fails
// for a kind with no entry, for an entry that names no kind, for a fact that is no fact type of
// package intake, for a finished row under no assumption the tool's list holds, and for a
// handling other than the design's (wantRows).
func TestEveryOutboxKindIsExecutedOrFinishedUnderAnAssumption(t *testing.T) {
	kinds, ops := outboxVocabulary(t)
	var laned []record.OutboxKind
	for _, lane := range record.OutboxLanes() {
		laned = append(laned, record.LaneKinds(lane)...)
	}
	slices.Sort(laned)
	if !slices.Equal(kinds, laned) {
		t.Errorf("%s declares the outbox kinds %v and its lanes run %v: the read of its constants differs from the kinds the outbox runs",
			outboxSource, kinds, laned)
	}
	if !slices.Contains(ops, "start") {
		t.Fatalf("%s's validSuperviseOp takes the ops %v, and no start: the read of the ops missed them", outboxSource, ops)
	}
	var rows []RowKind
	for _, kind := range kinds {
		if kind != record.OutboxKindSupervise {
			rows = append(rows, RowKind{Kind: kind})
			continue
		}
		for _, op := range ops {
			rows = append(rows, RowKind{Kind: kind, Op: op})
		}
	}

	facts := intakeFactTypes(t)
	for _, row := range rows {
		handling, handled := OutboxHandling[row]
		if !handled {
			t.Errorf("the outbox row %s is neither executed nor finished under an assumption: OutboxHandling holds no entry for it", rowName(row))
			continue
		}
		switch handling.Way {
		case OutboxExecuted:
			if handling.Does == "" {
				t.Errorf("OutboxHandling executes %s and says nothing of what it does", rowName(row))
			}
			if handling.Assumption != 0 {
				t.Errorf("OutboxHandling executes %s and names assumption %d, which only a row finished without acting rests on", rowName(row), handling.Assumption)
			}
		case OutboxFinished:
			if !slices.ContainsFunc(Assumptions, func(a Assumption) bool { return a.Number == handling.Assumption }) {
				t.Errorf("OutboxHandling finishes %s under assumption %d, which Assumptions lacks", rowName(row), handling.Assumption)
			}
			if len(handling.Facts) > 0 {
				t.Errorf("OutboxHandling finishes %s without acting and names the facts %v it applies", rowName(row), handling.Facts)
			}
		default:
			t.Errorf("OutboxHandling handles %s by %q, which is no OutboxWay", rowName(row), handling.Way)
		}
		for _, fact := range handling.Facts {
			if !slices.Contains(facts, fact) {
				t.Errorf("OutboxHandling has %s apply %s, which is no fact type of package intake", rowName(row), fact)
			}
		}
		if want, decided := wantRows[row]; decided {
			if handling.Way != want.Way {
				t.Errorf("OutboxHandling handles %s by %s, want %s", rowName(row), handling.Way, want.Way)
			}
			for _, fact := range want.Facts {
				if !slices.Contains(handling.Facts, fact) {
					t.Errorf("OutboxHandling has %s apply %v, which lacks %s, a fact its executor applies back into the daemon", rowName(row), handling.Facts, fact)
				}
			}
		}
	}
	for row := range OutboxHandling {
		if !slices.Contains(rows, row) {
			t.Errorf("OutboxHandling holds an entry for %s, which is no outbox kind, or no op, %s declares", rowName(row), outboxSource)
		}
	}
}

// rowName is a row kind as a report names it: the kind, and a supervise row's op after it.
func rowName(row RowKind) string {
	if row.Op == "" {
		return string(row.Kind)
	}
	return string(row.Kind) + " " + string(row.Op)
}

// outboxVocabulary is every outbox kind record/outbox.go declares, in order — each constant of
// type OutboxKind, a line naming no type of its own taking the one above it — and every supervise
// op its validSuperviseOp takes, read from the file as data so a kind or an op a later change adds
// is read with the rest. It fails rather than answer none.
func outboxVocabulary(t *testing.T) ([]record.OutboxKind, []record.SuperviseOp) {
	t.Helper()
	path := filepath.FromSlash(outboxSource)
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	var kinds []record.OutboxKind
	var ops []record.SuperviseOp
	for _, decl := range file.Decls {
		switch decl := decl.(type) {
		case *ast.GenDecl:
			if decl.Tok != token.CONST {
				continue
			}
			typed := ""
			for _, spec := range decl.Specs {
				value := spec.(*ast.ValueSpec)
				if ident, named := value.Type.(*ast.Ident); named {
					typed = ident.Name
				} else if value.Type != nil || len(value.Values) > 0 {
					typed = ""
				}
				if typed != "OutboxKind" {
					continue
				}
				for _, expr := range value.Values {
					if text, ok := stringLiteral(expr); ok {
						kinds = append(kinds, record.OutboxKind(text))
					}
				}
			}
		case *ast.FuncDecl:
			if decl.Name.Name != "validSuperviseOp" || decl.Body == nil {
				continue
			}
			ast.Inspect(decl.Body, func(node ast.Node) bool {
				if clause, isCase := node.(*ast.CaseClause); isCase {
					for _, expr := range clause.List {
						if text, ok := stringLiteral(expr); ok {
							ops = append(ops, record.SuperviseOp(text))
						}
					}
				}
				return true
			})
		}
	}
	if len(kinds) == 0 {
		t.Fatalf("%s declares no constant of type OutboxKind", path)
	}
	if len(ops) == 0 {
		t.Fatalf("%s's validSuperviseOp takes no op", path)
	}
	slices.Sort(kinds)
	return kinds, ops
}

// stringLiteral is the value of expr where it is a string literal.
func stringLiteral(expr ast.Expr) (string, bool) {
	literal, isLiteral := expr.(*ast.BasicLit)
	if !isLiteral || literal.Kind != token.STRING {
		return "", false
	}
	text, err := strconv.Unquote(literal.Value)
	return text, err == nil
}
