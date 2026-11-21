//go:build ignore

// Daemon-completion prints how a Legion daemon takes a phase completion, read out of its source:
// on a first line how its engine answers a role that does not run the issue's phase (lateRole),
// `recorded` or `refused`, and on a second the phase its completion route holds a completion's
// handoff record to (recordRoute), `left` or `issue`:
//
//	go run daemon-completion.go <daemon module dir>
//
// reading the module's internal/workflow/engine.go and internal/api/handoff.go (it takes the
// module rather than the two files, since `go run` reads every leading argument ending in .go as a
// file of the program). The rig reads each label's daemon out of the checkout that label is built
// from, never by the label's name: what a label is built from moves (the base label is main at the
// commit the head's newest forward merge brought in, and moves with every later one), so a table
// keyed by the name models a daemon the checkout may not have (completion-answers.ts's
// LabelDaemon). Each value has two shapes, this checkout's and an older daemon's (main ff2a10ef's
// among them), each read by the code that makes it, and a source holding neither or both is an
// error naming the file and what was looked for, never a shape this guesses. Like daemon-routes.go
// it imports the standard library alone and belongs to no module, since health and the scorer
// read it too and building daemon-pane.go first builds the daemon module's whole dependency graph
// (daemon-routes.go says what that cost). The build constraint keeps it out of every package
// build; `go run` of the file by name builds it all the same.
package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: daemon-completion <daemon module dir>")
		os.Exit(2)
	}
	late, err := lateRole(filepath.Join(os.Args[1], "internal", "workflow", "engine.go"))
	var route string
	if err == nil {
		route, err = recordRoute(filepath.Join(os.Args[1], "internal", "api", "handoff.go"))
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "daemon-completion: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("%s\n%s\n", late, route)
}

// notCurrentPhaseCode is the refusal an engine answers a late role with where it takes none.
const notCurrentPhaseCode = "HANDOFF_NOT_CURRENT_PHASE"

// refusingGate is the condition of the older engine's gate, which refuses every completion from a
// role that does not run the issue's phase.
const refusingGate = "RoleFor(issue.Phase) != fact.Role"

// lateRole is how the engine in `file` answers a completion from a role that does not run the
// issue's phase, read out of its `handoff` method, in one of the two shapes an engine has had:
//   - `recorded`: `handoff` asks `leftItsPhase`, a method of the same file whose answer turns on
//     `row.Completed()`, so a late role whose row holds no completion is recorded and one whose
//     row holds one is refused (this checkout's);
//   - `refused`: `handoff` holds `if RoleFor(issue.Phase) != fact.Role { return
//     refused("HANDOFF_NOT_CURRENT_PHASE", …) }`, which refuses every late role (an older
//     daemon's, main ff2a10ef's among them).
func lateRole(file string) (string, error) {
	parsed, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
	if err != nil {
		return "", fmt.Errorf("read the daemon's %s: %w", file, err)
	}
	handoff := method(parsed, "handoff")
	if handoff == nil {
		return "", fmt.Errorf("%s declares no handoff method, so how its engine answers a late role is unknown", file)
	}
	recorded := callsMethod(handoff, "leftItsPhase") && holdsCall(method(parsed, "leftItsPhase"), "row.Completed()")
	refused := refusesLateRoles(handoff)
	shapes := fmt.Sprintf("a call of leftItsPhase, a method of the file that reads row.Completed() (recorded), or `if %s { return refused(%q, …) }` (refused)", refusingGate, notCurrentPhaseCode)
	switch {
	case recorded && refused:
		return "", fmt.Errorf("%s's handoff holds both %s, so which one answers a late role is unknown", file, shapes)
	case recorded:
		return "recorded", nil
	case refused:
		return "refused", nil
	}
	return "", fmt.Errorf("%s's handoff holds neither %s: the engine answers a late role some other way now", file, shapes)
}

// method is the method `name` that `file` declares, or nil where it declares none.
func method(file *ast.File, name string) *ast.FuncDecl {
	for _, declared := range file.Decls {
		function, isFunction := declared.(*ast.FuncDecl)
		if isFunction && function.Recv != nil && function.Name.Name == name {
			return function
		}
	}
	return nil
}

// callsMethod says whether `function` calls a method named `name` (`<x>.<name>(…)`).
func callsMethod(function *ast.FuncDecl, name string) bool {
	found := false
	ast.Inspect(function, func(node ast.Node) bool {
		if call, isCall := node.(*ast.CallExpr); isCall {
			if selector, isSelector := call.Fun.(*ast.SelectorExpr); isSelector && selector.Sel.Name == name {
				found = true
			}
		}
		return !found
	})
	return found
}

// holdsCall says whether `function`, nil for none, holds the call `call` as Go prints it.
func holdsCall(function *ast.FuncDecl, call string) bool {
	if function == nil {
		return false
	}
	found := false
	ast.Inspect(function, func(node ast.Node) bool {
		if held, isCall := node.(*ast.CallExpr); isCall && types.ExprString(held) == call {
			found = true
		}
		return !found
	})
	return found
}

// refusesLateRoles says whether `function` holds the older engine's gate: `if refusingGate`, its
// body returning `refused(notCurrentPhaseCode, …)`.
func refusesLateRoles(function *ast.FuncDecl) bool {
	found := false
	ast.Inspect(function, func(node ast.Node) bool {
		gate, isIf := node.(*ast.IfStmt)
		if !isIf || gate.Cond == nil || types.ExprString(gate.Cond) != refusingGate {
			return !found
		}
		for _, statement := range gate.Body.List {
			returned, isReturn := statement.(*ast.ReturnStmt)
			if !isReturn || len(returned.Results) == 0 {
				continue
			}
			call, isCall := returned.Results[0].(*ast.CallExpr)
			if !isCall || len(call.Args) == 0 || types.ExprString(call.Fun) != "refused" {
				continue
			}
			if code, isCode := call.Args[0].(*ast.BasicLit); isCode && code.Kind == token.STRING && code.Value == fmt.Sprintf("%q", notCurrentPhaseCode) {
				found = true
			}
		}
		return !found
	})
	return found
}

// routeShapes are the two shapes a completion route has held a completion's handoff record to a
// phase in, each the statements of one function of the route's file that make it, as Go prints
// them (the function has moved: main ff2a10ef's `handoffComplete` holds them, and this checkout's
// `answerCompletion`, which `handoffComplete` calls):
//   - `left`: the phase the completion is of (`phase.Of`), the issue's when the role works it and
//     otherwise the one the role left, and a completion of a left phase naming a record is read
//     back (this checkout's);
//   - `issue`: the phase the issue stands in, so only a completion of the role's own phase is
//     read back (an older daemon's, main ff2a10ef's among them).
var routeShapes = []struct {
	name       string
	statements []string
}{
	{"left", []string{
		"of := phase.Of(grant.Role, issue.Phase)",
		"word, writer, handoffPhase := phase.Handoff(of)",
		"recordBacked := handoffPhase && writer == grant.Role && (of == issue.Phase || namesRecord)",
	}},
	{"issue", []string{
		"word, writer, handoffPhase := phase.Handoff(issue.Phase)",
		"recordBacked := handoffPhase && writer == grant.Role",
	}},
}

// recordRoute is the shape of routeShapes the completion route in `file` holds: the one whose
// statements are all assignments of one function of the file, every assignment read as Go
// prints it.
func recordRoute(file string) (string, error) {
	parsed, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
	if err != nil {
		return "", fmt.Errorf("read the daemon's %s: %w", file, err)
	}
	var shapes, found, wanted []string
	for _, shape := range routeShapes {
		wanted = append(wanted, fmt.Sprintf("`%s` (%s)", strings.Join(shape.statements, "; "), shape.name))
	}
	for _, declared := range parsed.Decls {
		function, isFunction := declared.(*ast.FuncDecl)
		if !isFunction {
			continue
		}
		var assigned []string
		ast.Inspect(function, func(node ast.Node) bool {
			if assignment, isAssignment := node.(*ast.AssignStmt); isAssignment {
				assigned = append(assigned, fmt.Sprintf("%s %s %s", exprList(assignment.Lhs), assignment.Tok, exprList(assignment.Rhs)))
			}
			return true
		})
		for _, shape := range routeShapes {
			holds := true
			for _, statement := range shape.statements {
				holds = holds && slices.Contains(assigned, statement)
			}
			if holds {
				shapes = append(shapes, shape.name)
				found = append(found, fmt.Sprintf("%s in %s", shape.name, function.Name.Name))
			}
		}
	}
	switch len(shapes) {
	case 1:
		return shapes[0], nil
	case 0:
		return "", fmt.Errorf("%s holds neither %s in any function: the route holds a completion's record to a phase some other way now", file, strings.Join(wanted, " nor "))
	}
	return "", fmt.Errorf("%s holds more than one shape of the route (%s), so which phase it holds a completion's record to is unknown", file, strings.Join(found, ", "))
}

// exprList is `expressions` as Go prints them, comma-separated.
func exprList(expressions []ast.Expr) string {
	printed := make([]string, 0, len(expressions))
	for _, expression := range expressions {
		printed = append(printed, types.ExprString(expression))
	}
	return strings.Join(printed, ", ")
}
