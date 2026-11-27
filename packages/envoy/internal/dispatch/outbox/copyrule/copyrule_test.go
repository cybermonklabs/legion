package copyrule

//ci:reads packages/envoy/**/*.go !**/*_test.go walks packages/envoy (../../../..) and TestOnlyHolderReadsTheCopyRule reads every .go file it finds but the test files and node_modules, to hold every reader of the mention copy rule to copyrule.Holder

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
	problems := copyRuleProblems(t, sources)
	t.Logf("%d non-test Go files read; problems: %q", len(sources), problems)
	if len(sources) < 100 {
		t.Fatalf("the walk read %d files of %s, too few to be the module", len(sources), moduleRoot)
	}
	if len(problems) != 0 {
		t.Fatalf("the copy rule is read outside copyrule.Holder:\n%s", strings.Join(problems, "\n"))
	}

	// Controls: each fault planted alone, in memory, on a copy of the module's sources.
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
		planted := map[string][]byte{}
		for path, source := range sources {
			planted[path] = source
		}
		switch {
		case plant.appended:
			if _, ok := planted[plant.file]; !ok {
				t.Fatalf("control: the walk read no %s to plant %s in", plant.file, plant.name)
			}
			planted[plant.file] = append(slices.Clone(planted[plant.file]), plant.source...)
		case plant.source == "":
			delete(planted, plant.file)
		default:
			planted[plant.file] = []byte(plant.source)
		}
		found := copyRuleProblems(t, planted)
		t.Logf("control, %s: %q", plant.name, found)
		if len(found) != 1 || !strings.HasPrefix(found[0], plant.want) {
			t.Fatalf("control: with %s the guard reports %q, want one problem saying %q", plant.name, found, plant.want)
		}
	}
}

// copyRuleProblems is each fault of sources, non-test Go files by their path under the module: a
// name of a statement of the rule, qualified or not, or a fragment of its SQL in a string, outside
// the rule's own definitions and Holder's body (ownSpans); and a package of callers none of whose
// files calls copyrule.Holder.
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
		own := ownSpans(path, file)
		ast.Inspect(file, func(node ast.Node) bool {
			if node == nil || slices.Contains(own, node) {
				return false
			}
			switch node := node.(type) {
			case *ast.SelectorExpr:
				if base, ok := node.X.(*ast.Ident); ok && base.Name == "copyrule" && node.Sel.Name == "Holder" {
					calls[filepath.ToSlash(filepath.Dir(path))] = true
				}
			case *ast.Ident:
				if slices.Contains(ruleNames, node.Name) {
					problems = append(problems, path+" names model."+node.Name)
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
