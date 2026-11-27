package daemon

import (
	"go/ast"
	"go/constant"
	"go/parser"
	"go/token"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/record"
)

// Every kind a lane runs has a case in the executor's switch (outbox.run): a row of a kind with
// none fails at every attempt, "no executor", and the rows queued after it on its issue's lane wait
// behind it. The kinds are read from the lanes themselves (record.OutboxLanes, LaneKinds), the
// payload type of each from the record package, whose OutboxKind method of that type returns it,
// and the switch from outbox.go, so a kind a later change adds to a lane is held too. The fault it
// catches is a kind in a lane before its executor (its control: parts_join back in the lifecycle
// lane, which it names).
func TestEveryKindOfALaneHasAnExecutor(t *testing.T) {
	t.Parallel()
	executed := executorCases(t)
	payloads := payloadTypesByKind(t)
	kinds := 0
	for _, lane := range record.OutboxLanes() {
		for _, kind := range record.LaneKinds(lane) {
			kinds++
			payload, ok := payloads[kind]
			if !ok {
				t.Errorf("%s runs on the %s lane, and no payload type of the record package is of that kind, so the executor has no case of it", kind, lane)
				continue
			}
			if !executed[payload] {
				t.Errorf("%s runs on the %s lane, and the executor's switch (outbox.run) has no case of its payload record.%s, so a row of it fails at every attempt", kind, lane, payload)
			}
		}
	}
	if kinds == 0 {
		t.Fatalf("the lanes hold no kind, so this test read nothing")
	}
}

// executorCases is the name of each record type that a case of the type switch in outbox.run
// names. It fails the test where outbox.go holds no such switch, or a switch of no case of the
// record package, so an empty answer never reads as a pass.
func executorCases(t *testing.T) map[string]bool {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "outbox.go", nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse outbox.go: %v", err)
	}
	cases := map[string]bool{}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "run" || fn.Recv == nil || receiverName(fn) != "outbox" {
			continue
		}
		ast.Inspect(fn.Body, func(node ast.Node) bool {
			clause, ok := node.(*ast.CaseClause)
			if !ok {
				return true
			}
			for _, expr := range clause.List {
				if selector, ok := expr.(*ast.SelectorExpr); ok {
					if pkg, ok := selector.X.(*ast.Ident); ok && pkg.Name == "record" {
						cases[selector.Sel.Name] = true
					}
				}
			}
			return true
		})
	}
	if len(cases) == 0 {
		t.Fatalf("outbox.go holds no method run of outbox whose switch names a type of the record package, so this test read no executor")
	}
	return cases
}

// payloadTypesByKind is the payload type of each kind, as the record package's source declares it:
// the type whose method OutboxKind returns the constant of that kind. The kind is the constant's
// value as the type checker reads it, however the constant is spelled: a string literal, or another
// package's constant, as each state event's kind is the one package stateevent names
// (record.OutboxKindStateChange is stateevent.KindChange). It type-checks the package's non-test
// source against the compiler's export data of its dependencies (compiledImports, checkPackage),
// and fails the test where a method OutboxKind returns what it cannot read as a string constant,
// or where it finds no such method.
func payloadTypesByKind(t *testing.T) map[record.OutboxKind]string {
	t.Helper()
	fset, imports := compiledImports(t)
	pkg := checkPackage(t, fset, imports, "record")
	payloads := map[record.OutboxKind]string{}
	for _, file := range pkg.files {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Name.Name != "OutboxKind" || fn.Recv == nil || fn.Body == nil || len(fn.Body.List) != 1 {
				continue
			}
			ret, ok := fn.Body.List[0].(*ast.ReturnStmt)
			if !ok || len(ret.Results) != 1 {
				continue
			}
			value := pkg.info.Types[ret.Results[0]].Value
			if value == nil || value.Kind() != constant.String {
				t.Fatalf("record.%s's OutboxKind returns what the type checker reads as no string constant, at %s: this test cannot tell its kind",
					receiverName(fn), pkg.pos(ret.Results[0]))
			}
			payloads[record.OutboxKind(constant.StringVal(value))] = receiverName(fn)
		}
	}
	if len(payloads) == 0 {
		t.Fatalf("the record package declares no payload type whose OutboxKind returns a kind, so this test read no payload")
	}
	return payloads
}

// receiverName is the name of the type fn is a method of, a pointer's or a value's.
func receiverName(fn *ast.FuncDecl) string {
	if len(fn.Recv.List) != 1 {
		return ""
	}
	typ := fn.Recv.List[0].Type
	if star, ok := typ.(*ast.StarExpr); ok {
		typ = star.X
	}
	if ident, ok := typ.(*ast.Ident); ok {
		return ident.Name
	}
	return ""
}
