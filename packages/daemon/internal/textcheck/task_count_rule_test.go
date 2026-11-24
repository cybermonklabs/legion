package textcheck

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// A step of the workflow acts on the scope of its task (internal/workflow's TaskScope) whatever
// the plan's count of tasks, so a plan of one task runs by the rules of a plan of several, and the
// task a copy of an issue is read for (record.Issue.ViewTask) is read only where the copy is made.
// This check holds the daemon's production code to that: it parses every Go file of
// packages/daemon/internal but the tests, and fails on each comparison or switch whose operand is
// the count of a plan's tasks (`len(<x>.Tasks)`, `len(walks)`), and on each read of ViewTask,
// outside the sites below. An allocation's size (`make(..., len(x.Tasks))`) and a count told in a
// text are no branch, and are not read.

// taskCountSite is one function of the daemon: the directory of its package under internal, and
// its name, a method's prefixed with its receiver type (functionName).
type taskCountSite struct{ dir, fn string }

// taskCountRules is each function that may branch on the count of a plan's tasks, with why. An
// entry whose function no longer branches fails the check, so the list stays the list of what is.
var taskCountRules = map[taskCountSite]string{
	{"workflow", "textNamesTask"}: "the one presentation rule: a text names the task only where the plan holds a choice of tasks",
	{"plan", "Validate"}:          "a plan's validation refuses a plan that holds no task",
	{"api", "withoutTasks"}:       "the served issue's task views, taken off for a client below their contract: whether the field was on the wire, not a rule of a plan",
}

// viewTaskReaders is where a copy's task (record.Issue.ViewTask) may be read: every function of
// internal/workflow's scope.go, which makes each copy and reads its task, and the store's refusal
// to write a copy (record.Postgres.PutIssue).
func viewTaskReader(dir, file, fn string) bool {
	return dir == "workflow" && file == "scope.go" || dir == "record" && fn == "Postgres.PutIssue"
}

// taskCountFile is one parsed Go file and its path relative to internal.
type taskCountFile struct {
	path string
	file *ast.File
}

// taskCountFindings is each branch on a count of a plan's tasks and each read of ViewTask in files
// that the rules above do not allow, as `<path>:<line>: <what> in <function>`, and each count rule
// that matched a branch.
func taskCountFindings(fset *token.FileSet, files []taskCountFile) ([]string, map[taskCountSite]bool) {
	var found []string
	matched := map[taskCountSite]bool{}
	for _, f := range files {
		dir, name := filepath.Dir(f.path), filepath.Base(f.path)
		dir = strings.Split(filepath.ToSlash(dir), "/")[0]
		for _, decl := range f.file.Decls {
			fn, node := "(package level)", ast.Node(decl)
			if decl, ok := decl.(*ast.FuncDecl); ok {
				if decl.Body == nil {
					continue
				}
				fn, node = functionName(decl), decl.Body
			}
			site := taskCountSite{dir, fn}
			report := func(at token.Pos, what string) {
				found = append(found, fset.Position(at).String()+": "+what+" in "+fn)
			}
			ast.Inspect(node, func(node ast.Node) bool {
				switch node := node.(type) {
				case *ast.BinaryExpr:
					if isComparison(node.Op) && (isTaskCount(node.X) || isTaskCount(node.Y)) {
						if _, ok := taskCountRules[site]; ok {
							matched[site] = true
						} else {
							report(node.Pos(), "a branch on the count of a plan's tasks")
						}
					}
				case *ast.SwitchStmt:
					if node.Tag != nil && isTaskCount(node.Tag) {
						if _, ok := taskCountRules[site]; ok {
							matched[site] = true
						} else {
							report(node.Pos(), "a switch on the count of a plan's tasks")
						}
					}
				case *ast.SelectorExpr:
					if node.Sel.Name == "ViewTask" && !viewTaskReader(dir, name, fn) {
						report(node.Pos(), "a read of the task a copy is read for (ViewTask)")
					}
				}
				return true
			})
		}
	}
	return found, matched
}

// isComparison is whether op compares two values.
func isComparison(op token.Token) bool {
	return slices.Contains([]token.Token{token.EQL, token.NEQ, token.LSS, token.LEQ, token.GTR, token.GEQ}, op)
}

// isTaskCount is whether expr is the count of a plan's tasks: len of a field Tasks on any value
// (record.StoredPlan.Tasks, plan.Plan.Tasks, and the plan of a fact), or len of the walks of a run
// (internal/workflow's walksOf), whose count is the plan's.
func isTaskCount(expr ast.Expr) bool {
	call, ok := ast.Unparen(expr).(*ast.CallExpr)
	if !ok || len(call.Args) != 1 {
		return false
	}
	if fun, ok := call.Fun.(*ast.Ident); !ok || fun.Name != "len" {
		return false
	}
	switch arg := ast.Unparen(call.Args[0]).(type) {
	case *ast.SelectorExpr:
		return arg.Sel.Name == "Tasks"
	case *ast.Ident:
		return arg.Name == "walks"
	}
	return false
}

// The planted control runs the check over files of its own first: a branch of each shape and a
// read of ViewTask in workflow's tasks.go, each of which it must name, and the same shapes where
// the rules allow them, with an allocation size, none of which it may name. Then the daemon's
// production files, read from internal (this package's parent), must hold no finding, and each
// rule above must still match a branch.
func TestNoStepBranchesOnItsPlansCountOfTasks(t *testing.T) {
	planted := map[string]string{
		"workflow/tasks.go": `package workflow
func plantedBranch(s TaskScope, issue record.Issue) bool { return len(s.plan.Tasks) > 1 && issue.ViewTask != "" }
func plantedSwitch(stored *record.StoredPlan) int { switch len(stored.Tasks) { case 1: return 1 }; return 0 }
func plantedWalks(walks []Walk) bool { return len(walks) == 1 }
func plantedSize(stored *record.StoredPlan) []int { return make([]int, len(stored.Tasks)) }
`,
		"workflow/scope.go": `package workflow
func textNamesTask(s TaskScope) bool { return s.plan != nil && len(s.plan.Tasks) > 1 }
func scopeOn(issue record.Issue) string { return issue.ViewTask }
`,
	}
	fset := token.NewFileSet()
	var control []taskCountFile
	for _, path := range []string{"workflow/scope.go", "workflow/tasks.go"} {
		file, err := parser.ParseFile(fset, path, planted[path], 0)
		if err != nil {
			t.Fatalf("parse the planted %s: %v", path, err)
		}
		control = append(control, taskCountFile{path, file})
	}
	found, matched := taskCountFindings(fset, control)
	want := []string{
		"workflow/tasks.go:2:67: a branch on the count of a plan's tasks in plantedBranch",
		"workflow/tasks.go:2:92: a read of the task a copy is read for (ViewTask) in plantedBranch",
		"workflow/tasks.go:3:53: a switch on the count of a plan's tasks in plantedSwitch",
		"workflow/tasks.go:4:47: a branch on the count of a plan's tasks in plantedWalks",
	}
	if !slices.Equal(found, want) || !matched[taskCountSite{"workflow", "textNamesTask"}] {
		t.Fatalf("the check over the planted files found\n%s\nwant\n%s\nand the rule textNamesTask matched (%t): the check cannot see what it is for",
			strings.Join(found, "\n"), strings.Join(want, "\n"), matched[taskCountSite{"workflow", "textNamesTask"}])
	}

	root := ".."
	fset = token.NewFileSet()
	var files []taskCountFile
	if err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if path != root && (strings.HasPrefix(entry.Name(), ".") || entry.Name() == "testdata") {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		files = append(files, taskCountFile{filepath.ToSlash(relative), file})
		return nil
	}); err != nil {
		t.Fatalf("read the daemon's packages: %v", err)
	}
	if len(files) == 0 {
		t.Fatalf("UNKNOWN: read no Go file under %s, so the check saw nothing", root)
	}
	found, matched = taskCountFindings(fset, files)
	for _, finding := range found {
		t.Errorf("%s: a step acts on its task's scope whatever the plan's count of tasks; read the task's own state, or name the presentation rule in taskCountRules", finding)
	}
	for site, why := range taskCountRules {
		if !matched[site] {
			t.Errorf("%s's %s is a count rule (%s) and branches on no count of tasks: remove its entry", site.dir, site.fn, why)
		}
	}
	t.Logf("read %d production files under internal", len(files))
}
