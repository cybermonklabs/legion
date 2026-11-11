package main

import (
	"go/ast"
	"go/constant"
	"go/parser"
	"go/token"
	"go/types"
	"path/filepath"
	"testing"
	"time"
)

// Where the infra module writes this listener's stop budget again: read is the path from this
// package's directory, path names the same file from the repository root for a failure read in
// CI, and name is the constant in it.
var (
	infraStopBudgetRead = filepath.Join("..", "..", "infra", "gcp", "clusteredhub_listener.go")
	infraStopBudgetPath = "packages/envoy/infra/gcp/clusteredhub_listener.go"
	infraStopBudgetName = "clusteredHubListenerShutdownSeconds"
)

// TestTheInfraCopyOfTheStopBudgetIsTheStopBudget holds the infra module's copy of this listener's
// stop budget to stopBudget itself.
//
// The number is written twice because packages/envoy and packages/envoy/infra are two Go modules
// and neither imports the other: the infra components render the listener pods'
// terminationGracePeriodSeconds out of their own clusteredHubListenerShutdownSeconds, which is
// this listener's budget copied by hand. This listener owns the number - stopBudget is the sum of
// the three deadlines shutdownListener actually spends, stopHTTPGrace, stopLaneGrace and
// stopClientDrain - and the infra constant follows it. A copy that drifts grants the stop a
// window that is not the one the stop takes, and neither module's own tests can see it: each side
// is consistent with itself.
//
// This is the one check that reads both. The infra module holds its own side: its
// clusteredHubListenerShutdownSeconds is the 28 seconds its pod tests read out of the pods it
// renders (packages/envoy/infra/gcp/clusteredhub_listener_test.go and listener_grace_test.go,
// which hold each rendered grace against that copy and its preStop wait). So with this check the
// chain runs unbroken from the deadlines this process spends to the seconds a kubelet grants it.
//
// The copy is read by parsing the Go source and evaluating the constant's expression, not by
// matching the text of the sum, so it is read correctly whether it is written 10 + 5 + 13, with
// other spacing, or as 28. A file that cannot be read or parsed, a file that holds no constant of
// that name, and a constant whose expression is no longer a constant expression each fail by
// name: each is this check losing its other side, and a lost side must red rather than pass.
func TestTheInfraCopyOfTheStopBudgetIsTheStopBudget(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, infraStopBudgetRead, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s, which holds the infra module's copy of this listener's stop budget as %s: %v", infraStopBudgetPath, infraStopBudgetName, err)
	}

	value := infraConstExpr(file, infraStopBudgetName)
	if value == nil {
		t.Fatalf("%s declares no constant %s: the infra module's copy of this listener's stop budget (%s, in packages/envoy/cmd/listener/main.go) was renamed or removed, and nothing holds the seconds the listener pods grant to the seconds this listener spends. Whoever moved it points this check at the name it has now.",
			infraStopBudgetPath, infraStopBudgetName, stopBudget)
	}

	// The expression is evaluated in the universe scope, so a copy that is still a literal sum is
	// read whatever its spelling, and one that now names something out of its own package is not
	// silently skipped: Eval reports it here.
	expr := types.ExprString(value)
	evaluated, err := types.Eval(fset, nil, token.NoPos, expr)
	if err != nil || evaluated.Value == nil || evaluated.Value.Kind() != constant.Int {
		t.Fatalf("%s declares %s = %s, which this check cannot evaluate as an integer constant (%v): the copy of this listener's stop budget has to stay a constant expression of literals for anything to hold it to stopBudget, which is %s. Whoever made it something else holds the two together another way.",
			infraStopBudgetPath, infraStopBudgetName, expr, err, stopBudget)
	}
	copied, exact := constant.Int64Val(evaluated.Value)
	if !exact {
		t.Fatalf("%s declares %s = %s, whose value does not fit an int64: that is no count of seconds. stopBudget (packages/envoy/cmd/listener/main.go) is %s.",
			infraStopBudgetPath, infraStopBudgetName, expr, stopBudget)
	}

	// The budget is a count of whole seconds on the infra side, so a budget here that is not one
	// cannot be stated there at all: truncating it would let a stop of 28.5 s pass against a copy
	// of 28 and grant the stop half a second less than it spends.
	if stopBudget%time.Second != 0 {
		t.Fatalf("stopBudget (packages/envoy/cmd/listener/main.go) is %s, which is not whole seconds, while %s states the same budget in seconds as %s = %s: a budget the infra module cannot state is a budget its pods cannot grant.",
			stopBudget, infraStopBudgetPath, infraStopBudgetName, expr)
	}
	budget := int64(stopBudget / time.Second)
	if copied != budget {
		t.Fatalf("%s declares %s = %s, %d seconds, where stopBudget (packages/envoy/cmd/listener/main.go) is %s, %d seconds: the two modules state one number and neither can import the other, so a copy that drifts grants this listener's stop a window it does not take. This listener owns the number; whoever moves stopBudget moves the copy with it.",
			infraStopBudgetPath, infraStopBudgetName, expr, copied, stopBudget, budget)
	}
	t.Logf("%s declares %s = %s, %d seconds, which is stopBudget (%s)", infraStopBudgetPath, infraStopBudgetName, expr, copied, stopBudget)
}

// infraConstExpr returns the expression the constant named name is declared with in file, or nil
// when the file declares no constant of that name with a value of its own. A spec that carries no
// value - a line of a const block that repeats the one above it, as an iota run does - is no
// expression to evaluate, so it is reported as the name being absent rather than read as the
// previous line's.
func infraConstExpr(file *ast.File, name string) ast.Expr {
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			valueSpec, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, ident := range valueSpec.Names {
				if ident.Name == name && i < len(valueSpec.Values) {
					return valueSpec.Values[i]
				}
			}
		}
	}
	return nil
}
