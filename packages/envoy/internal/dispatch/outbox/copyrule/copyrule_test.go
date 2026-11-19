package copyrule

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// moduleRoot is packages/envoy, from this package's directory.
var moduleRoot = filepath.Join("..", "..", "..", "..")

// ruleNames are the copy rule's statements (internal/dispatch/model), and ruleText a fragment of
// their SQL that no other statement holds.
var (
	ruleNames = []string{"MentionCopyHolderSQL", "MentionCopyRolesSQL", "MentionCopyRuleSQL"}
	ruleText  = []string{"owed_keys &&", "owed.owed_key"}
)

// ruleOwners are the two files that may name the rule: the one that defines it and Holder's.
var ruleOwners = []string{"internal/dispatch/model/model.go", "internal/dispatch/outbox/copyrule/copyrule.go"}

// callers are the packages of the two roads of a copy, each of which must read the rule through
// Holder: the API's send of a mention and the outbox's fallback pass.
var callers = []string{"internal/dispatch/api", "internal/dispatch/outbox"}

// The copy rule has one reader, Holder, which both roads of a mention's copy call, so the rule is
// read in one snapshot on both and a second copy of it cannot come back unseen. Across every
// non-test Go file of the module (packages/envoy), no file but the rule's own (model.go) and
// Holder's names one of the rule's statements or holds a fragment of its SQL in a string, and the
// API's package and the outbox's each call Holder. The control in the same run plants each fault
// in memory - a caller that runs a statement of the rule itself, an inlined copy of its SQL, and a
// road that no longer calls Holder - and the guard must name each.
func TestOnlyHolderReadsTheCopyRule(t *testing.T) {
	if _, err := os.Stat(filepath.Join(moduleRoot, "go.mod")); err != nil {
		t.Fatalf("the module root %s holds no go.mod: %v", moduleRoot, err)
	}
	sources := map[string][]byte{}
	err := filepath.WalkDir(moduleRoot, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if entry.Name() == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		source, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(moduleRoot, path)
		if err != nil {
			return err
		}
		sources[filepath.ToSlash(relative)] = source
		return nil
	})
	if err != nil {
		t.Fatalf("walk the module: %v", err)
	}
	problems := copyRuleProblems(t, sources)
	t.Logf("%d non-test Go files read; problems: %q", len(sources), problems)
	if len(sources) < 100 {
		t.Fatalf("the walk read %d files of %s, too few to be the module", len(sources), moduleRoot)
	}
	if len(problems) != 0 {
		t.Fatalf("the copy rule is read outside copyrule.Holder:\n%s", strings.Join(problems, "\n"))
	}

	// Control: each fault planted in memory, on a copy of the module's sources.
	planted := map[string][]byte{}
	for path, source := range sources {
		planted[path] = source
	}
	planted["internal/dispatch/api/planted_rule.go"] = []byte("package api\n\nfunc plantedRule() {\n" +
		"\t_, _ = pool.Query(ctx, model.MentionCopyRolesSQL, 1)\n}\n")
	planted["internal/dispatch/outbox/planted_text.go"] = []byte("package outbox\n\nconst plantedText = `select k.id from events k where k.owed_keys && $1`\n")
	delete(planted, "internal/dispatch/api/comment_delivery.go")
	found := copyRuleProblems(t, planted)
	t.Logf("control: %q", found)
	for _, want := range []string{
		"internal/dispatch/api/planted_rule.go names model.MentionCopyRolesSQL",
		"internal/dispatch/outbox/planted_text.go holds the rule's SQL",
		"internal/dispatch/api calls no copyrule.Holder",
	} {
		if !slices.ContainsFunc(found, func(problem string) bool { return strings.HasPrefix(problem, want) }) {
			t.Fatalf("control: the guard reports %q, which does not say %q", found, want)
		}
	}
}

// copyRuleProblems is each fault of sources, non-test Go files by their path under the module: a
// file other than ruleOwners that names a statement of the rule or holds a fragment of its SQL in a
// string, and a package of callers none of whose files calls copyrule.Holder.
func copyRuleProblems(t *testing.T, sources map[string][]byte) []string {
	t.Helper()
	var problems []string
	calls := map[string]bool{}
	paths := make([]string, 0, len(sources))
	for path := range sources {
		paths = append(paths, path)
	}
	slices.Sort(paths)
	for _, path := range paths {
		source := string(sources[path])
		if !strings.Contains(source, "MentionCopy") && !strings.Contains(source, "owed") && !strings.Contains(source, "copyrule") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, source, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		owner := slices.Contains(ruleOwners, path)
		ast.Inspect(file, func(node ast.Node) bool {
			switch node := node.(type) {
			case *ast.SelectorExpr:
				if base, ok := node.X.(*ast.Ident); ok && base.Name == "copyrule" && node.Sel.Name == "Holder" {
					calls[filepath.ToSlash(filepath.Dir(path))] = true
				}
				if !owner && slices.Contains(ruleNames, node.Sel.Name) {
					problems = append(problems, path+" names model."+node.Sel.Name)
				}
			case *ast.BasicLit:
				if node.Kind != token.STRING || owner {
					return true
				}
				text, err := strconv.Unquote(node.Value)
				if err != nil {
					t.Fatalf("read a string of %s: %v", path, err)
				}
				for _, fragment := range ruleText {
					if strings.Contains(text, fragment) {
						problems = append(problems, path+" holds the rule's SQL ("+fragment+")")
						break
					}
				}
			}
			return true
		})
	}
	for _, caller := range callers {
		if !calls[caller] {
			problems = append(problems, caller+" calls no copyrule.Holder")
		}
	}
	return problems
}
