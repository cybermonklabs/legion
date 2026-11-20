package main

import (
	"fmt"
	"go/ast"
	"go/printer"
	"strings"
)

// checkHelperRefusals refuses a helper refusal whose code the error page cannot list: every
// Response literal in the helper package that is not OK: true and sets any field must name its
// fields and set Code to one of the documented Code constants in codes. (An OK reply may carry a
// login's confirmation code in Code, and the empty Response is no reply at all.)
func checkHelperRefusals(root string, codes []docConst) error {
	src, err := parseDir(root, helperDir)
	if err != nil {
		return err
	}
	documented := map[string]bool{}
	for _, c := range codes {
		documented[c.name] = true
	}
	var problems []string
	for _, f := range src.files {
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok || len(lit.Elts) == 0 {
				return true
			}
			if t, ok := lit.Type.(*ast.Ident); !ok || t.Name != "Response" {
				return true
			}
			var code ast.Expr
			for _, e := range lit.Elts {
				kv, ok := e.(*ast.KeyValueExpr)
				if !ok {
					problems = append(problems, src.at(lit)+": a helper Response names its fields")
					return true
				}
				key, _ := kv.Key.(*ast.Ident)
				switch {
				case key == nil:
				case key.Name == "OK":
					if v, ok := kv.Value.(*ast.Ident); ok && v.Name == "true" {
						return true
					}
				case key.Name == "Code":
					code = kv.Value
				}
			}
			switch id, _ := code.(*ast.Ident); {
			case code == nil:
				problems = append(problems, src.at(lit)+": a helper refusal sets no Code")
			case id == nil || !documented[id.Name]:
				problems = append(problems, fmt.Sprintf("%s: a helper refusal's Code is %s, not one of the documented Code constants", src.at(lit), src.text(code)))
			}
			return true
		})
	}
	return refusedAt(problems)
}

// checkExitCodes refuses an agent-secrets exit code the error page cannot list. It follows only
// the free int-returning functions that can reach the process's exit: run (the argument main hands
// os.Exit), a free int function an exit path returns, a free int function os.Exit calls, and a
// registered secret-form handler. That excludes ordinary helpers whose int has another meaning,
// such as a prompt poll's timeout. Every return of a followed function may be only 0, 1, one of
// the documented exit constants in codes, or what another followed function returns; so may
// os.Exit be called only with one of those.
func checkExitCodes(root string, codes []docConst) error {
	src, err := parseDir(root, cliDir)
	if err != nil {
		return err
	}
	documented := map[string]bool{}
	for _, c := range codes {
		documented[c.name] = true
	}
	returnsInt := func(t *ast.FuncType) bool {
		if t.Results == nil || len(t.Results.List) != 1 || len(t.Results.List[0].Names) > 1 {
			return false
		}
		id, ok := t.Results.List[0].Type.(*ast.Ident)
		return ok && id.Name == "int"
	}
	intFuncs := map[string]bool{}
	for _, f := range src.files {
		for _, d := range f.Decls {
			if fn, ok := d.(*ast.FuncDecl); ok && fn.Recv == nil && returnsInt(fn.Type) {
				intFuncs[fn.Name.Name] = true
			}
		}
	}
	callbacks, handlers, err := secretFormExitCalls(src, intFuncs)
	if err != nil {
		return err
	}
	exitFuncs := map[string]bool{}
	addExitFunc := func(e ast.Expr) bool {
		call, ok := e.(*ast.CallExpr)
		if !ok {
			return false
		}
		fun, ok := call.Fun.(*ast.Ident)
		if !ok || !intFuncs[fun.Name] || exitFuncs[fun.Name] {
			return false
		}
		exitFuncs[fun.Name] = true
		return true
	}
	exitFuncs["run"] = intFuncs["run"]
	for handler := range handlers {
		exitFuncs[handler] = true
	}
	// The argument of every os.Exit call, by file: each is a root of the closure below, and each
	// is checked as an exit code once the closure is known.
	exitArgs := make([][]ast.Expr, len(src.files))
	for i, f := range src.files {
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) != 1 {
				return true
			}
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Exit" {
				if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "os" {
					exitArgs[i] = append(exitArgs[i], call.Args[0])
					addExitFunc(call.Args[0])
				}
			}
			return true
		})
	}
	for changed := true; changed; {
		changed = false
		for _, f := range src.files {
			for _, d := range f.Decls {
				fn, ok := d.(*ast.FuncDecl)
				if !ok || fn.Recv != nil || !returnsInt(fn.Type) || !exitFuncs[fn.Name.Name] {
					continue
				}
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					if _, ok := n.(*ast.FuncLit); ok {
						return false
					}
					if ret, ok := n.(*ast.ReturnStmt); ok && len(ret.Results) == 1 {
						changed = addExitFunc(ret.Results[0]) || changed
					}
					return true
				})
			}
		}
	}
	var problems []string
	allowed := func(e ast.Expr) {
		switch v := e.(type) {
		case *ast.BasicLit:
			if v.Value == "0" || v.Value == "1" {
				return
			}
		case *ast.Ident:
			if documented[v.Name] {
				return
			}
		case *ast.CallExpr:
			if fun, ok := v.Fun.(*ast.Ident); ok && exitFuncs[fun.Name] {
				return
			}
			if callbacks[v] {
				return
			}
		}
		problems = append(problems, fmt.Sprintf("%s: exit code %s is not 0, 1 or one of the documented exit constants", src.at(e), src.text(e)))
	}
	// checkBody checks the returns of one followed exit function body, leaving every function
	// literal inside it to its own path.
	var checkBody func(body *ast.BlockStmt)
	checkBody = func(body *ast.BlockStmt) {
		ast.Inspect(body, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.FuncLit:
				if returnsInt(x.Type) {
					checkBody(x.Body)
				}
				return false
			case *ast.ReturnStmt:
				if len(x.Results) != 1 {
					problems = append(problems, src.at(x)+": an exit code is one value")
					return true
				}
				allowed(x.Results[0])
			}
			return true
		})
	}
	for i, f := range src.files {
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil || fn.Recv != nil || !returnsInt(fn.Type) || !exitFuncs[fn.Name.Name] {
				continue
			}
			checkBody(fn.Body)
		}
		for _, arg := range exitArgs[i] {
			allowed(arg)
		}
	}
	return refusedAt(problems)
}

// secretFormExitCalls follows the CLI's secret-form table rather than accepting arbitrary
// callback results as exit codes. Every registered handler must be one of the package's local
// int-returning functions, and is one root of checkExitCodes's exit-path closure.
func secretFormExitCalls(src *source, intFuncs map[string]bool) (map[*ast.CallExpr]bool, map[string]bool, error) {
	calls := map[*ast.CallExpr]bool{}
	handlers := map[string]bool{}
	var table *ast.CompositeLit
	var problems []string
	for _, file := range src.files {
		ast.Inspect(file, func(n ast.Node) bool {
			assign, ok := n.(*ast.AssignStmt)
			if !ok {
				return true
			}
			for _, lhs := range assign.Lhs {
				id, ok := lhs.(*ast.Ident)
				if !ok || id.Name != "secretForms" {
					continue
				}
				if len(assign.Lhs) != 1 || len(assign.Rhs) != 1 {
					problems = append(problems, src.at(assign)+": secretForms must have one literal table")
					continue
				}
				lit, ok := assign.Rhs[0].(*ast.CompositeLit)
				if !ok || table != nil {
					problems = append(problems, src.at(assign)+": secretForms must have one literal table")
					continue
				}
				table = lit
			}
			return true
		})
	}
	if table == nil {
		return calls, handlers, refusedAt(problems)
	}
	array, ok := table.Type.(*ast.ArrayType)
	if !ok {
		return nil, nil, fmt.Errorf("%s: secretForms must be a slice of secretForm", src.at(table))
	}
	element, ok := array.Elt.(*ast.Ident)
	if !ok || element.Name != "secretForm" || array.Len != nil {
		return nil, nil, fmt.Errorf("%s: secretForms must be a slice of secretForm", src.at(table))
	}
	for _, item := range table.Elts {
		row, ok := item.(*ast.CompositeLit)
		if !ok || len(row.Elts) != 2 {
			problems = append(problems, src.at(item)+": a secret form needs help and an exit-code handler")
			continue
		}
		handler, ok := row.Elts[1].(*ast.Ident)
		if !ok || !intFuncs[handler.Name] {
			problems = append(problems, src.at(row)+": a secret form's handler must be a checked local exit-code function")
			continue
		}
		handlers[handler.Name] = true
	}
	for _, file := range src.files {
		ast.Inspect(file, func(n ast.Node) bool {
			loop, ok := n.(*ast.RangeStmt)
			if !ok {
				return true
			}
			tableName, ok := loop.X.(*ast.Ident)
			row, rowOK := loop.Value.(*ast.Ident)
			if !ok || tableName.Name != "secretForms" || !rowOK {
				return true
			}
			ast.Inspect(loop.Body, func(n ast.Node) bool {
				if assign, ok := n.(*ast.AssignStmt); ok {
					for _, lhs := range assign.Lhs {
						ast.Inspect(lhs, func(n ast.Node) bool {
							if id, ok := n.(*ast.Ident); ok && id.Obj == row.Obj {
								problems = append(problems, src.at(lhs)+": a secret form's registered handler cannot be reassigned")
							}
							return true
						})
					}
				}
				if call, ok := n.(*ast.CallExpr); ok {
					if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "run" {
						if id, ok := sel.X.(*ast.Ident); ok && id.Obj == row.Obj {
							calls[call] = true
						}
					}
				}
				return true
			})
			return true
		})
	}
	return calls, handlers, refusedAt(problems)
}

// refusedAt is one error listing every problem, or nil when there is none.
func refusedAt(problems []string) error {
	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("%s", strings.Join(problems, "\n"))
}

// text is the source text of e, as gofmt would print it.
func (s *source) text(e ast.Expr) string {
	var b strings.Builder
	if err := printer.Fprint(&b, s.fset, e); err != nil {
		return fmt.Sprintf("%T", e)
	}
	return b.String()
}
