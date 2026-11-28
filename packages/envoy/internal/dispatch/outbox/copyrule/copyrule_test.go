package copyrule

//ci:reads packages/envoy/**/*.go !**/*_test.go walks packages/envoy (../../../..) and TestOnlyHolderReadsTheCopyRule reads every .go file it finds but the test files and node_modules, to hold every reader of the mention copy rule to copyrule.Holder

import (
	"go/ast"
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

// moduleRoot is packages/envoy, from this package's directory.
var moduleRoot = filepath.Join("..", "..", "..", "..")

// ruleNames are the copy rule's statements (internal/dispatch/model), and ruleText a fragment of
// their SQL that no other statement holds.
var (
	ruleNames = []string{"MentionCopyHolderSQL", "MentionCopyRolesSQL", "MentionCopyRuleSQL"}
	ruleText  = []string{"owed_keys &&", "owed.owed_key"}
)

// ruleFile and holderFile are the two files that may name the rule: model.go only in the
// definitions of the rule's statements, and copyrule.go only in Holder's body (ownSpans).
const (
	ruleFile   = "internal/dispatch/model/model.go"
	holderFile = "internal/dispatch/outbox/copyrule/copyrule.go"
)

// callers are the packages of the two roads of a copy, each of which must read the rule through
// Holder: the API's send of a mention and the outbox's fallback pass.
var callers = []string{"internal/dispatch/api", "internal/dispatch/outbox"}

// The copy rule has one reader, Holder, which both roads of a mention's copy call, so the rule is
// read in one snapshot on both and a second copy of it cannot come back unseen. Across every
// non-test Go file of the module (packages/envoy), nothing but the definitions of the rule's
// statements in model.go and the body of Holder names one of those statements, by its name
// qualified or not, or holds a fragment of their SQL in a string; and the API's package and the
// outbox's each call Holder. The controls in the same run plant each fault alone, in memory - a
// caller that runs a statement of the rule itself, an inlined copy of its SQL, a road that no
// longer calls Holder, a second reader beside the rule's definitions in model.go, a copy of its SQL
// there, and a second reader beside Holder in copyrule.go - and the guard must name each.
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
	findings := make(map[string]copyRuleFindings, len(sources))
	for path, source := range sources {
		findings[path] = fileFindings(t, path, source)
	}
	problems := copyRuleProblems(findings)
	t.Logf("%d non-test Go files read; problems: %q", len(sources), problems)
	if len(sources) < 100 {
		t.Fatalf("the walk read %d files of %s, too few to be the module", len(sources), moduleRoot)
	}
	if len(problems) != 0 {
		t.Fatalf("the copy rule is read outside copyrule.Holder:\n%s", strings.Join(problems, "\n"))
	}

	// Controls: each fault planted alone, in memory, on a copy of the module's findings, in which
	// only the file planted in is read again and the file deleted has none.
	for _, plant := range []struct {
		name, file, source string
		// appended is that source goes at the end of file's own; otherwise it is the whole of a
		// new file, and an empty source deletes file.
		appended bool
		want     string
	}{
		{name: "a caller that runs a statement of the rule", file: "internal/dispatch/api/planted_rule.go",
			source: "package api\n\nfunc plantedRule() {\n\t_, _ = pool.Query(ctx, model.MentionCopyRolesSQL, 1)\n}\n",
			want:   "internal/dispatch/api/planted_rule.go names model.MentionCopyRolesSQL"},
		{name: "an inlined copy of the rule's SQL", file: "internal/dispatch/outbox/planted_text.go",
			source: "package outbox\n\nconst plantedText = `select k.id from events k where k.owed_keys && $1`\n",
			want:   "internal/dispatch/outbox/planted_text.go holds the rule's SQL"},
		{name: "a road that no longer calls Holder", file: "internal/dispatch/api/comment_delivery.go",
			want: "internal/dispatch/api calls no copyrule.Holder"},
		{name: "a second reader beside the rule's definitions", file: ruleFile, appended: true,
			source: "\nfunc plantedRuleReader(ctx context.Context, db pgx.Tx) {\n\t_, _ = db.Query(ctx, MentionCopyRolesSQL, 1)\n}\n",
			want:   ruleFile + " names model.MentionCopyRolesSQL"},
		{name: "a copy of the rule's SQL beside its definitions", file: ruleFile, appended: true,
			source: "\nconst plantedRuleCopy = `select owed.owed_key from events k cross join lateral unnest(k.owed_keys) as owed(owed_key)`\n",
			want:   ruleFile + " holds the rule's SQL"},
		{name: "a second reader beside Holder", file: holderFile, appended: true,
			source: "\nfunc plantedHolderReader(ctx context.Context, db Querier) {\n\t_, _ = db.Query(ctx, model.MentionCopyHolderSQL, 1, \"\")\n}\n",
			want:   holderFile + " names model.MentionCopyHolderSQL"},
	} {
		planted := maps.Clone(findings)
		switch {
		case plant.appended:
			source, ok := sources[plant.file]
			if !ok {
				t.Fatalf("control: the walk read no %s to plant %s in", plant.file, plant.name)
			}
			planted[plant.file] = fileFindings(t, plant.file, append(slices.Clone(source), plant.source...))
		case plant.source == "":
			delete(planted, plant.file)
		default:
			planted[plant.file] = fileFindings(t, plant.file, []byte(plant.source))
		}
		found := copyRuleProblems(planted)
		t.Logf("control, %s: %q", plant.name, found)
		if len(found) != 1 || !strings.HasPrefix(found[0], plant.want) {
			t.Fatalf("control: with %s the guard reports %q, want one problem saying %q", plant.name, found, plant.want)
		}
	}
}

// copyRuleFindings is what the guard finds in one non-test Go file: each name of a statement of the
// rule, qualified or not, or fragment of its SQL in a string, outside the rule's own definitions
// and Holder's body (ownSpans), and whether the file calls copyrule.Holder.
type copyRuleFindings struct {
	problems    []string
	callsHolder bool
}

// fileFindings is the guard's findings in source, the file at path under the module. A file in
// which none of MentionCopy, owed and copyrule appears, one of which every name of the rule's
// statements, fragment of its SQL and call of Holder holds, finds nothing and is not parsed.
func fileFindings(t *testing.T, path string, source []byte) copyRuleFindings {
	t.Helper()
	var findings copyRuleFindings
	code := string(source)
	if !strings.Contains(code, "MentionCopy") && !strings.Contains(code, "owed") && !strings.Contains(code, "copyrule") {
		return findings
	}
	file, err := parser.ParseFile(token.NewFileSet(), path, code, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	own := ownSpans(path, file)
	ast.Inspect(file, func(node ast.Node) bool {
		if node == nil || slices.Contains(own, node) {
			return false
		}
		switch node := node.(type) {
		case *ast.SelectorExpr:
			if base, ok := node.X.(*ast.Ident); ok && base.Name == "copyrule" && node.Sel.Name == "Holder" {
				findings.callsHolder = true
			}
		case *ast.Ident:
			if slices.Contains(ruleNames, node.Name) {
				findings.problems = append(findings.problems, path+" names model."+node.Name)
			}
		case *ast.BasicLit:
			if node.Kind != token.STRING {
				return true
			}
			text, err := strconv.Unquote(node.Value)
			if err != nil {
				t.Fatalf("read a string of %s: %v", path, err)
			}
			for _, fragment := range ruleText {
				if strings.Contains(text, fragment) {
					findings.problems = append(findings.problems, path+" holds the rule's SQL ("+fragment+")")
					break
				}
			}
		}
		return true
	})
	return findings
}

// copyRuleProblems is each fault of a module whose non-test Go files, by their path under the
// module, hold findings: each file's problems, in path order, and a package of callers none of
// whose files calls copyrule.Holder.
func copyRuleProblems(findings map[string]copyRuleFindings) []string {
	var problems []string
	calls := map[string]bool{}
	for _, path := range slices.Sorted(maps.Keys(findings)) {
		file := findings[path]
		problems = append(problems, file.problems...)
		if file.callsHolder {
			calls[filepath.ToSlash(filepath.Dir(path))] = true
		}
	}
	for _, caller := range callers {
		if !calls[caller] {
			problems = append(problems, caller+" calls no copyrule.Holder")
		}
	}
	return problems
}

// ownSpans are the nodes of file, at path, that may name the rule: in model.go each const spec that
// defines one of the rule's statements, its name and its value, and in copyrule.go the body of
// Holder, the rule's one reader. Every other node of those two files is read as any file's is.
func ownSpans(path string, file *ast.File) []ast.Node {
	var own []ast.Node
	for _, decl := range file.Decls {
		switch decl := decl.(type) {
		case *ast.GenDecl:
			if path != ruleFile || decl.Tok != token.CONST {
				continue
			}
			for _, spec := range decl.Specs {
				if value := spec.(*ast.ValueSpec); slices.ContainsFunc(value.Names, func(name *ast.Ident) bool {
					return slices.Contains(ruleNames, name.Name)
				}) {
					own = append(own, value)
				}
			}
		case *ast.FuncDecl:
			if path == holderFile && decl.Recv == nil && decl.Name.Name == "Holder" && decl.Body != nil {
				own = append(own, decl.Body)
			}
		}
	}
	return own
}
