package api

import (
	"bytes"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// ownerReadRule is one rule of plan D5's list: a read or write that must see what the request's
// caller cannot, so it reads through store.AsOwner on the request's own transaction. functions
// are the functions ("<package directory under the module> <function>", a method as
// "<receiver type>.<name>") that may make the call for it; the rule holds where one of them does.
type ownerReadRule struct {
	rule      string
	functions []string
}

// ownerReadRules is plan D5's list of every caller of store.AsOwner.
var ownerReadRules = []ownerReadRule{
	{"the cycle and depth rules: the issue's ancestors", []string{"internal/dispatch/refs IssueAncestors"}},
	{"the cycle and depth rules: the issue's descendants", []string{"internal/dispatch/refs IssueDescendantRows"}},
	{"the wait-path walk and writeBlockedBy's cycle walk", []string{"internal/dispatch/api assertNoWaitPath", "internal/dispatch/api writeBlockedBy"}},
	{"refuseOpenForeignChildren: the refusal reads every open foreign child", []string{"internal/dispatch/api refuseOpenForeignChildren"}},
	{"parentStatusConflictOf", []string{"internal/dispatch/api parentStatusConflictOf"}},
	{"openChildrenOfAClose", []string{"internal/dispatch/api openChildrenOfAClose"}},
	{"the status change's cascade filter", []string{"internal/dispatch/api server.planStatusChangeUnderLocks"}},
	{"the projectOf* resolvers of withinProjectScope, which answer a hidden target as a missing one", []string{
		"internal/dispatch/api server.projectOfIssue", "internal/dispatch/api issueProject", "internal/dispatch/api server.projectOfProject",
		"internal/dispatch/api server.projectOfArtifact", "internal/dispatch/api server.projectOfAsk", "internal/dispatch/api server.projectOfComment",
		"internal/dispatch/api server.projectOfMessage", "internal/dispatch/api server.projectOfCheckResult", "internal/dispatch/api checkIssueProject",
		"internal/dispatch/api projectVisibility",
	}},
	{"the session ties written inside a request", []string{"internal/dispatch/access TieSession"}},
	{"the reached projects written inside a request", []string{"internal/dispatch/store RecordReachedProjects"}},
	{"the document removal's refusal over citations and what points at the document", []string{"internal/dispatch/api artifactIsUntouched", "internal/dispatch/refs DocumentIsCited"}},
}

// TestOwnerReadsAreTheListedRules (plan D5, R3): every call of store.AsOwner in the module's
// non-test Go files is made by a function a rule of ownerReadRules names, and every rule of the
// list has one of its functions make it. A caller the list does not name fails, since a read as
// the owner the list does not hold is a read nobody judged; so does a rule none of whose functions
// calls it, since the rule would read under the request role and miss the hidden rows it judges.
func TestOwnerReadsAreTheListedRules(t *testing.T) {
	t.Parallel()
	callers := asOwnerCallers(t, moduleRoot)
	named := map[string]bool{}
	for _, rule := range ownerReadRules {
		for _, function := range rule.functions {
			named[function] = true
		}
	}
	for _, caller := range slices.Sorted(maps.Keys(callers)) {
		if !named[caller] {
			t.Errorf("%s calls store.AsOwner (%s), and no rule of plan D5's list names it", caller, strings.Join(callers[caller], ", "))
		}
	}
	for _, rule := range ownerReadRules {
		if !slices.ContainsFunc(rule.functions, func(function string) bool { return len(callers[function]) > 0 }) {
			t.Errorf("%s: none of %s calls store.AsOwner", rule.rule, strings.Join(rule.functions, ", "))
		}
	}
	t.Logf("%d functions of the module call store.AsOwner", len(callers))
}

// asOwnerCallers is every function of the module under root whose body calls store.AsOwner (or
// AsOwner, inside package store), keyed "<package directory under root> <function>", with the
// file and line of each call. It reads the non-test files go/build's default context selects.
func asOwnerCallers(t *testing.T, root string) map[string][]string {
	t.Helper()
	dirs := map[string]bool{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			name := entry.Name()
			if path != root && (name == "vendor" || name == "testdata" || name == "node_modules" || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_")) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.Contains(content, []byte("AsOwner(")) {
			dirs[filepath.Dir(path)] = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk the module: %v", err)
	}
	callers := map[string][]string{}
	for _, dir := range slices.Sorted(maps.Keys(dirs)) {
		pkg, err := build.Default.ImportDir(dir, 0)
		if err != nil {
			t.Fatalf("read the package in %s: %v", dir, err)
		}
		relative, err := filepath.Rel(root, dir)
		if err != nil {
			t.Fatalf("name %s under the module: %v", dir, err)
		}
		fset := token.NewFileSet()
		for _, name := range pkg.GoFiles {
			file, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.SkipObjectResolution)
			if err != nil {
				t.Fatalf("parse %s: %v", filepath.Join(relative, name), err)
			}
			for _, decl := range file.Decls {
				for _, unit := range declarationUnits(decl) {
					ast.Inspect(unit.node, func(node ast.Node) bool {
						call, ok := node.(*ast.CallExpr)
						if !ok || !callsAsOwner(call.Fun, pkg.Name) {
							return true
						}
						key := filepath.ToSlash(relative) + " " + unit.name
						callers[key] = append(callers[key], filepath.ToSlash(filepath.Join(relative, name))+":"+strconv.Itoa(fset.Position(call.Pos()).Line))
						return true
					})
				}
			}
		}
	}
	return callers
}

// callsAsOwner reports whether fun, the function a call names, is store.AsOwner: the selector
// store.AsOwner, or the bare name inside package store.
func callsAsOwner(fun ast.Expr, pkg string) bool {
	switch fun := fun.(type) {
	case *ast.SelectorExpr:
		ident, ok := fun.X.(*ast.Ident)
		return ok && ident.Name == "store" && fun.Sel.Name == "AsOwner"
	case *ast.Ident:
		return pkg == "store" && fun.Name == "AsOwner"
	}
	return false
}
