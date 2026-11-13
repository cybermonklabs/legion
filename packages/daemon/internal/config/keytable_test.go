package config

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// requiredBy is the vocabulary a descriptor's RequiredBy is held to, the Dispatch settings table's
// (packages/envoy/cmd/dispatch/settings.go) and the one its generator already enforces.
var requiredBy = regexp.MustCompile(`^(yes|no|when .+)$`)

// keyName is the shape of a key's own name, the last part of the path an operator writes it
// under. Every name the key table holds is lowercase snake_case; the pattern accepts a capital
// as well, which no row uses, so it refuses only a name that is no key name at all.
var keyName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*$`)

// loaderKeys is what the loader's own source says about its keys, read from every non-test file of
// internal/config: the nestedKeys group every describedMembers(...) site names, with the site that
// named it. The top-level keys are no part of it: the loader dispatches each of them through the
// key table and nothing else (readKeys over topLevelByName), so the table is the list and a second
// reading of the source would only read the table back.
type loaderKeys struct {
	// groups are the nestedKeys paths describedMembers(...) sites name.
	groups map[string]string
}

// readLoaderKeys parses every non-test file of internal/config. A test file's own helpers are not
// the loader, so they are left out; everything the loader itself compiles is read.
func readLoaderKeys(t *testing.T) loaderKeys {
	t.Helper()
	fileSet := token.NewFileSet()
	var files []*ast.File
	var paths []string
	err := filepath.WalkDir(".", func(path string, entry fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case entry.IsDir() && path != ".":
			return fs.SkipDir
		case entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go"):
			return nil
		}
		parsed, err := parser.ParseFile(fileSet, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		files, paths = append(files, parsed), append(paths, path)
		return nil
	})
	if err != nil {
		t.Fatalf("parse internal/config: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("parsed no file of internal/config")
	}
	source := &loaderSource{fileSet: fileSet, files: files, constants: map[string]string{}}
	source.readConstants()
	keys := loaderKeys{groups: map[string]string{}}
	for i, file := range files {
		source.readFile(t, file, paths[i], &keys)
	}
	return keys
}

// loaderSource is the parsed package, with the package-level string constants a call site's
// argument may name.
type loaderSource struct {
	fileSet   *token.FileSet
	files     []*ast.File
	constants map[string]string
}

// readConstants collects the package's string constants. They are resolved until nothing more
// resolves, since one is written from another (agentSecretsKey = kubernetesKey +
// ".agent_secrets").
func (s *loaderSource) readConstants() {
	for again := true; again; {
		again = false
		for _, file := range s.files {
			for _, declaration := range file.Decls {
				general, ok := declaration.(*ast.GenDecl)
				if !ok || general.Tok != token.CONST {
					continue
				}
				for _, spec := range general.Specs {
					value, ok := spec.(*ast.ValueSpec)
					if !ok || len(value.Names) != 1 || len(value.Values) != 1 {
						continue
					}
					name := value.Names[0].Name
					if _, known := s.constants[name]; known {
						continue
					}
					if text, ok := s.text(value.Values[0]); ok {
						s.constants[name], again = text, true
					}
				}
			}
		}
	}
}

// text is a string expression's value: a literal, a constant the package declares, or the two
// joined with +.
func (s *loaderSource) text(expression ast.Expr) (string, bool) {
	switch node := expression.(type) {
	case *ast.BasicLit:
		if node.Kind != token.STRING {
			return "", false
		}
		value, err := strconv.Unquote(node.Value)
		return value, err == nil
	case *ast.Ident:
		value, ok := s.constants[node.Name]
		return value, ok
	case *ast.BinaryExpr:
		if node.Op != token.ADD {
			return "", false
		}
		left, leftOK := s.text(node.X)
		right, rightOK := s.text(node.Y)
		return left + right, leftOK && rightOK
	case *ast.ParenExpr:
		return s.text(node.X)
	}
	return "", false
}

// readFile collects one file's describedMembers(...) sites.
func (s *loaderSource) readFile(t *testing.T, file *ast.File, path string, keys *loaderKeys) {
	t.Helper()
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Body == nil {
			continue
		}
		ast.Inspect(function.Body, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			identifier, ok := call.Fun.(*ast.Ident)
			if !ok || identifier.Name != "describedMembers" || len(call.Args) < 3 {
				return true
			}
			site := path + ":" + s.line(call)
			group, ok := s.text(call.Args[2])
			if !ok {
				t.Errorf("%s names a describedMembers group the guard cannot read; pass a string constant", site)
				return true
			}
			keys.groups[group] = site
			if _, known := nestedByPath[group]; !known {
				t.Errorf("%s names the describedMembers group %q, which nestedKeys does not hold", site, group)
			}
			return true
		})
	}
}

func (s *loaderSource) line(node ast.Node) string {
	return strconv.Itoa(s.fileSet.Position(node.Pos()).Line)
}

// The key table is the whole of the loader's top-level dispatch, and every nested key name the
// loader accepts carries a line of its own (describedMembers, the loader's one reader of a
// mapping). This is what makes "a key with no description fails the build" true: a describedMembers
// group nestedKeys does not hold, a description left empty, a RequiredBy outside the vocabulary, a
// row with no reader, a descriptor naming a key the loader classifies rather than dispatches, and
// a key named twice each fail here.
func TestNoKeyBypassesTheKeyTable(t *testing.T) {
	keys := readLoaderKeys(t)

	described := map[string]bool{}
	for _, row := range topLevelKeys {
		if described[row.Name] {
			t.Errorf("%s is in the key table twice", row.Name)
		}
		described[row.Name] = true
		checkDescriptor(t, row.Name, row.Name, row.RequiredBy, row.Description)
		if row.Read == nil {
			t.Errorf("%s: no Read, so the loader dispatches the key to nothing", row.Name)
		}
		// A positive-integer key resolves into one field of the configuration, and its default
		// is the number the table renders its line from.
		if p := row.Positive; p != nil {
			if (p.Duration == nil) == (p.Count == nil) {
				t.Errorf("%s names a Positive naming neither one duration field nor one count field", row.Name)
			}
			if p.Value <= 0 {
				t.Errorf("%s names the positive default %d", row.Name, p.Value)
			}
		}
		// A key the loader classifies is a key it does not dispatch: it is refused, or accepted
		// and read for nothing, so a row for it would describe a key no value reaches.
		for _, class := range []struct {
			name string
			why  map[string]string
		}{{"tossed", tossedKeys}, {"a migration refusal", migrationKeys}, {"warned", warnedKeys}} {
			if _, held := class.why[row.Name]; held {
				t.Errorf("%s is in the key table and is %s: the loader does not dispatch it", row.Name, class.name)
			}
		}
	}

	checkNestedKeys(t, keys, described)
}

// checkDescriptor holds one row, top-level or nested, to the vocabulary and to one line of prose.
// label is the key as an operator writes it, which a failure names; name is the key's own last
// part, which carries the shape.
func checkDescriptor(t *testing.T, label, name, required, description string) {
	t.Helper()
	if !keyName.MatchString(name) {
		t.Errorf("%q is no configuration key name", label)
	}
	if !requiredBy.MatchString(required) {
		t.Errorf("%s: RequiredBy %q, want yes, no, or when <condition>", label, required)
	}
	if description == "" || strings.Contains(description, "\n") {
		t.Errorf("%s: Description %q, want one line", label, description)
	}
}

// checkNestedKeys holds the nested half of the table: every group belongs to a top-level key and
// is used, and every member carries a line. A describedMembers site's names carry their own
// lines, so a name described under one mapping never stands in for the same name under another.
func checkNestedKeys(t *testing.T, keys loaderKeys, described map[string]bool) {
	t.Helper()
	for _, group := range nestedKeys {
		if !described[group.topLevel()] {
			t.Errorf("the nestedKeys group %s belongs to %s, which is no top-level key", group.Path, group.topLevel())
		}
		if _, used := keys.groups[group.Path]; !used {
			t.Errorf("the nestedKeys group %s is named by no describedMembers(...) site", group.Path)
		}
		seen := map[string]bool{}
		for _, member := range group.Members {
			if seen[member.Name] {
				t.Errorf("%s names %s twice", group.Path, member.Name)
			}
			seen[member.Name] = true
			checkDescriptor(t, group.Path+"."+member.Name, member.Name, member.RequiredBy, member.Description)
		}
	}
}

// The reference prints every top-level key whole, and under each the nested keys of the mapping
// that carries their lines, so the reference never carries a key with no description.
func TestConfigReferenceCarriesEveryDescribedKey(t *testing.T) {
	entries := ConfigReference()
	if len(entries) != len(topLevelKeys) {
		t.Fatalf("the reference holds %d entries, want one per top-level key (%d)", len(entries), len(topLevelKeys))
	}
	for index, entry := range entries {
		row := topLevelKeys[index]
		if entry.Name != row.Name || entry.Required != row.RequiredBy || entry.Description != row.Description || entry.Default != row.Default {
			t.Errorf("entry %d is %+v, want the table's %+v", index, entry, row)
		}
	}
	for _, entry := range entries {
		for _, key := range entry.Nested {
			group, described := nestedByPath[key.Path]
			if !described {
				t.Errorf("the reference carries %s.%s, whose mapping no nestedKeys group describes", key.Path, key.Name)
				continue
			}
			if !slices.ContainsFunc(group.Members, func(m member) bool {
				return m.Name == key.Name && m.Description == key.Description && m.Description != ""
			}) {
				t.Errorf("the reference carries %s.%s with no line of its mapping's own", key.Path, key.Name)
			}
		}
	}
	for _, group := range nestedKeys {
		index := slices.IndexFunc(entries, func(e ConfigKey) bool { return e.Name == group.topLevel() })
		if index < 0 {
			t.Fatalf("no reference entry for %s", group.topLevel())
		}
		for _, member := range group.Members {
			if !slices.ContainsFunc(entries[index].Nested, func(k ConfigNestedKey) bool {
				return k.Path == group.Path && k.Name == member.Name
			}) {
				t.Errorf("the reference leaves out %s.%s", group.Path, member.Name)
			}
		}
	}
}

// Every nested entry's path begins with the top-level key it is printed under, so a generator can
// render one table per key with no further parsing.
func TestConfigReferenceNestsEveryKeyUnderItsOwn(t *testing.T) {
	for _, entry := range ConfigReference() {
		for _, key := range entry.Nested {
			if key.Path != entry.Name && !strings.HasPrefix(key.Path, entry.Name+".") && !strings.HasPrefix(key.Path, entry.Name+"[") {
				t.Errorf("%s is printed under %s", key.Path+"."+key.Name, entry.Name)
			}
		}
	}
}
