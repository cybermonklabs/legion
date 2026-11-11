package config

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"reflect"
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
// under. One legacy alias of the file is camelCase, which is why a capital is accepted.
var keyName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*$`)

// loaderKeys is what the loader's own source says about its keys, read from every non-test file of
// internal/config: the top-level keys it dispatches outside the key table, the nested key names
// every members(...) site may carry, and the nestedKeys group every describedMembers(...) site
// names, each with the site that passed it.
type loaderKeys struct {
	// positive are the durationKeys and countKeys rows: the key, and the default in its row.
	positive map[string]int
	// cases are the top-level keys readKeys still dispatches with a case label of its own.
	cases map[string]string
	// nested are the nested key names a members(...) or describedMembers(...) site may carry.
	nested map[string]string
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
	source := &loaderSource{fileSet: fileSet, files: files, constants: map[string]string{}, lists: map[string][]string{}}
	source.readDeclarations()
	keys := loaderKeys{positive: map[string]int{}, cases: map[string]string{}, nested: map[string]string{}, groups: map[string]string{}}
	source.readPositiveRows(t, keys.positive)
	for i, file := range files {
		source.readFile(t, file, paths[i], &keys)
	}
	return keys
}

// loaderSource is the parsed package, with the package-level string constants and []string
// variables a call site's argument may name.
type loaderSource struct {
	fileSet   *token.FileSet
	files     []*ast.File
	constants map[string]string
	lists     map[string][]string
}

// readDeclarations collects the package's string constants and its []string variables. Constants
// are resolved until nothing more resolves, since one is written from another
// (agentSecretsKey = kubernetesKey + ".agent_secrets").
func (s *loaderSource) readDeclarations() {
	for again := true; again; {
		again = false
		for _, file := range s.files {
			for _, declaration := range file.Decls {
				general, ok := declaration.(*ast.GenDecl)
				if !ok || (general.Tok != token.CONST && general.Tok != token.VAR) {
					continue
				}
				for _, spec := range general.Specs {
					value, ok := spec.(*ast.ValueSpec)
					if !ok || len(value.Names) != 1 || len(value.Values) != 1 {
						continue
					}
					name := value.Names[0].Name
					if general.Tok == token.CONST {
						if _, known := s.constants[name]; known {
							continue
						}
						if text, ok := s.text(value.Values[0]); ok {
							s.constants[name], again = text, true
						}
						continue
					}
					if _, known := s.lists[name]; known {
						continue
					}
					if list, ok := s.stringList(value.Values[0]); ok {
						s.lists[name], again = list, true
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

// stringList is a []string expression's elements, for a composite literal of strings.
func (s *loaderSource) stringList(expression ast.Expr) ([]string, bool) {
	composite, ok := expression.(*ast.CompositeLit)
	if !ok {
		return nil, false
	}
	if array, ok := composite.Type.(*ast.ArrayType); !ok || array.Len != nil {
		return nil, false
	}
	list := make([]string, 0, len(composite.Elts))
	for _, element := range composite.Elts {
		text, ok := s.text(element)
		if !ok {
			return nil, false
		}
		list = append(list, text)
	}
	return list, true
}

// names is one argument of a members(...) site as the names it contributes: a literal or constant,
// a []string literal, an append of either, a package-level []string, or a local variable built
// from those in the function the call is in. A value it cannot follow answers false, and the
// caller fails naming the site rather than passing over it. within is the function the call is in
// and building is the local variable being followed, whose own name in an assignment to itself
// (known = append(known, …)) contributes nothing a second time.
func (s *loaderSource) names(expression ast.Expr, within *ast.FuncDecl, building string) ([]string, bool) {
	if text, ok := s.text(expression); ok {
		return []string{text}, true
	}
	if list, ok := s.stringList(expression); ok {
		return list, true
	}
	switch node := expression.(type) {
	case *ast.Ident:
		if node.Name == building {
			return nil, true
		}
		if list, ok := s.lists[node.Name]; ok {
			return list, true
		}
		return s.localNames(node.Name, within)
	case *ast.CallExpr:
		identifier, ok := node.Fun.(*ast.Ident)
		if !ok || identifier.Name != "append" {
			return nil, false
		}
		var list []string
		for _, argument := range node.Args {
			part, ok := s.names(argument, within, building)
			if !ok {
				return nil, false
			}
			list = append(list, part...)
		}
		return list, true
	}
	return nil, false
}

// localNames is every name a local variable of the enclosing function is built from: its
// declaration and every later assignment to it, each read as an argument of its own.
func (s *loaderSource) localNames(name string, within *ast.FuncDecl) ([]string, bool) {
	if within == nil {
		return nil, false
	}
	var list []string
	found, followed := false, true
	ast.Inspect(within.Body, func(node ast.Node) bool {
		assignment, ok := node.(*ast.AssignStmt)
		if !ok {
			return true
		}
		for index, target := range assignment.Lhs {
			identifier, ok := target.(*ast.Ident)
			if !ok || identifier.Name != name || index >= len(assignment.Rhs) {
				continue
			}
			found = true
			part, ok := s.names(assignment.Rhs[index], within, name)
			if !ok {
				followed = false
				return false
			}
			// An assignment written from the variable itself (known = append(known, …)) already
			// carries what the declaration gave it, so the parts are joined and deduped below.
			list = append(list, part...)
		}
		return true
	})
	if !found || !followed {
		return nil, false
	}
	slices.Sort(list)
	return slices.Compact(list), true
}

// readPositiveRows collects the durationKeys and countKeys rows: each key, and the default beside
// it, which the key's descriptor must name.
func (s *loaderSource) readPositiveRows(t *testing.T, into map[string]int) {
	t.Helper()
	for _, file := range s.files {
		for _, declaration := range file.Decls {
			general, ok := declaration.(*ast.GenDecl)
			if !ok || general.Tok != token.VAR {
				continue
			}
			for _, spec := range general.Specs {
				value, ok := spec.(*ast.ValueSpec)
				if !ok || len(value.Names) != 1 || len(value.Values) != 1 {
					continue
				}
				if value.Names[0].Name != "durationKeys" && value.Names[0].Name != "countKeys" {
					continue
				}
				composite, ok := value.Values[0].(*ast.CompositeLit)
				if !ok {
					t.Fatalf("%s is no composite literal; the guard reads its rows", value.Names[0].Name)
				}
				for _, element := range composite.Elts {
					row, ok := element.(*ast.CompositeLit)
					if !ok || len(row.Elts) < 2 {
						t.Fatalf("%s holds a row the guard cannot read at %s", value.Names[0].Name, s.at(element))
					}
					key, keyOK := s.text(row.Elts[0])
					number, numberOK := row.Elts[1].(*ast.BasicLit)
					if !keyOK || !numberOK || number.Kind != token.INT {
						t.Fatalf("%s holds a row the guard cannot read at %s", value.Names[0].Name, s.at(element))
					}
					read, err := strconv.Atoi(strings.ReplaceAll(number.Value, "_", ""))
					if err != nil {
						t.Fatalf("%s row %s has default %s, which is no number", value.Names[0].Name, key, number.Value)
					}
					into[key] = read
				}
			}
		}
	}
	if len(into) == 0 {
		t.Fatal("the guard read no durationKeys or countKeys row")
	}
}

// readFile collects one file's case labels of readKeys and its members(...) and
// describedMembers(...) sites.
func (s *loaderSource) readFile(t *testing.T, file *ast.File, path string, keys *loaderKeys) {
	t.Helper()
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Body == nil {
			continue
		}
		// describedMembers' own delegation to members passes the group's names, which it has
		// already looked up: reading it would be reading the table through itself.
		if function.Name.Name == "describedMembers" {
			continue
		}
		if function.Name.Name == "readKeys" {
			s.readCaseLabels(function, keys.cases)
		}
		ast.Inspect(function.Body, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			identifier, ok := call.Fun.(*ast.Ident)
			if !ok || len(call.Args) < 3 {
				return true
			}
			site := path + ":" + s.line(call)
			switch identifier.Name {
			case "members":
				for _, argument := range call.Args[2:] {
					names, ok := s.names(argument, function, "")
					if !ok {
						t.Errorf("%s passes members(...) a list of names the guard cannot follow to its declaration; name each key there, or carry the site over to describedMembers", site)
						continue
					}
					for _, name := range names {
						if _, seen := keys.nested[name]; !seen {
							keys.nested[name] = site
						}
					}
				}
			case "describedMembers":
				group, ok := s.text(call.Args[2])
				if !ok {
					t.Errorf("%s names a describedMembers group the guard cannot read; pass a string constant", site)
					return true
				}
				keys.groups[group] = site
				if _, known := nestedByPath[group]; !known {
					t.Errorf("%s names the describedMembers group %q, which nestedKeys does not hold", site, group)
					return true
				}
				// A describedMembers site's names come with their lines, so only a plain
				// members(...) site leaves a name for the undescribed list to hold.
			}
			return true
		})
	}
}

// readCaseLabels collects every top-level key readKeys still dispatches with a case label of its
// own. The table replaced that switch, so there are none; one added back is a key the table does
// not carry, which is what this reads for.
func (s *loaderSource) readCaseLabels(function *ast.FuncDecl, into map[string]string) {
	ast.Inspect(function.Body, func(node ast.Node) bool {
		clause, ok := node.(*ast.CaseClause)
		if !ok {
			return true
		}
		for _, expression := range clause.List {
			if text, ok := s.text(expression); ok {
				into[text] = "readKeys:" + s.line(expression)
			}
		}
		return true
	})
}

func (s *loaderSource) at(node ast.Node) string { return s.fileSet.Position(node.Pos()).String() }

func (s *loaderSource) line(node ast.Node) string {
	return strconv.Itoa(s.fileSet.Position(node.Pos()).Line)
}

// The loader dispatches every top-level key through the key table, and every nested key name it
// accepts carries a line or is named on the undescribed list. This is what makes "a key with no
// description fails the build" true: a key the loader dispatches that no descriptor names, a name
// passed to members(...) that is in neither the descriptors nor the undescribed list, a
// description left empty, a RequiredBy outside the vocabulary, a descriptor naming a key the
// loader does not dispatch, and a key named twice each fail here.
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

	for key, where := range keys.cases {
		if !described[key] {
			t.Errorf("readKeys dispatches %s at %s, and no descriptor names it: add a row to topLevelKeys (keytable.go)", key, where)
		}
	}
	for key, value := range keys.positive {
		if !described[key] {
			t.Errorf("the loader dispatches %s (a durationKeys or countKeys row), and no descriptor names it: add a row to topLevelKeys (keytable.go)", key)
			continue
		}
		row := topLevelByName[key]
		if want := "`" + strconv.Itoa(value) + "`"; !strings.Contains(row.Default, want) {
			t.Errorf("%s: Default %q does not name its row's default %s", key, row.Default, want)
		}
		if reflect.ValueOf(row.Read).Pointer() != reflect.ValueOf(readPositive).Pointer() {
			t.Errorf("%s is a durationKeys or countKeys row, so its Read must be readPositive", key)
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
// is used, every member carries a line, and every name a plain members(...) site passes is on the
// undescribed list, named once. A describedMembers site's names carry their own lines, so a name
// described under one mapping never stands in for the same name under another.
func checkNestedKeys(t *testing.T, keys loaderKeys, described map[string]bool) {
	t.Helper()
	nested := map[string]bool{}
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
			nested[member.Name] = true
		}
	}

	waiting := map[string]bool{}
	for _, name := range undescribed {
		if waiting[name] {
			t.Errorf("%s is on the undescribed list twice", name)
		}
		waiting[name] = true
	}

	for name, where := range keys.nested {
		if !waiting[name] {
			t.Errorf("%s passes the nested key %s, whose line is not written: name it on the undescribed list, or carry the site over to describedMembers with a line for every name it passes (keytable.go)", where, name)
		}
	}
	for name := range waiting {
		if _, passed := keys.nested[name]; !passed {
			t.Errorf("the undescribed list names %s, which no members(...) site passes: remove it", name)
		}
	}
}

// The reference prints every top-level key whole, and under each the nested keys of the mappings
// that carry their lines; a mapping whose names are still on the undescribed list reaches no
// entry, so the reference never carries a key with no description.
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

// The guard follows a list a members(...) site builds, rather than passing over it: the two sites
// that pass one are read to the same names the loader accepts there.
func TestTheGuardFollowsABuiltListOfNames(t *testing.T) {
	keys := readLoaderKeys(t)
	for _, name := range append(append([]string{}, volumeSources...), "items", "default_mode") {
		if _, passed := keys.nested[name]; !passed {
			t.Errorf("the guard did not read %s, which a built list of names passes to members(...)", name)
		}
	}
}
