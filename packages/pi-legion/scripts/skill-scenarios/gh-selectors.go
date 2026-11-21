//go:build ignore

// Gh-selectors prints the table the Legion gh wrapper (`legion gh`, packages/daemon/cmd/legion)
// reads every owner and repository a gh command names by, read out of the daemon module's source:
//
//	go run gh-selectors.go <daemon module dir>
//
// one tab-separated line an entry, in byte order:
//
//	slot	<argument name>	<kind>	<many>	every name of ghSlotKinds, a usage's argument
//	option	<--name placeholder>	<kind>	every key of ghOptionKinds, an option of a help
//	quirk	<leaf path>	<field>	<value>	every field ghLeafQuirks sets for a leaf
//	quirk-slot	<leaf path>	<argument name>	<kind>	<many>	a leaf's own argument kinds
//	rank	<kind>	<rank>	ghKindRank, which kind a slot of several names takes
//	pattern	<variable>	<expression>	<file>	each regular expression of patternVars the wrapper
//						declares (optionalPatterns), and the file under the
//						module it is declared in
//	read-leaf	<leaf path>	every leaf of ghReadLeaves, whose call may write nothing
//	read	<leaf path>	<spelling>	<takes>	each option spelling ghReadLeaves gives that leaf, 1
//						where it takes a value and 0 where it stands alone
//	api-read	<spelling>	<takes>	each of ghAPIReadOptions, a `gh api` read's options
//	graphql-read	<spelling>	<takes>	each of ghGraphQLReadOptions, a `gh api graphql`
//						query's (main's since #4035, none before:
//						optionalReads)
//	override-header	<name>	each header ghMethodOverrideHeaders reads as a method
//
// The read lines are the grammar of the wrapper's ghWritesNothing and ghGraphQLQueryRead
// (gh_reads.go), by which main's wrapper since #4035 runs a call that writes nothing and names
// only owners it holds no directory for with the pane's own GH_CONFIG_DIR (the world's
// gh-pane-token.ts). Each set of options is a call of ghReadOptions over string constants,
// literals and their sums, as the wrapper writes them.
//
// It is the world's reading of the wrapper as daemon-routes.go is of the daemon's API: a program
// of its own that imports the standard library alone and belongs to no module, which
// daemon-pane.ts runs with `go run` outside every module, so the table is read from the label's
// own checkout and never copied into the rig, and an entry the wrapper adds is one the world
// reads with no edit (gh-selectors.ts refuses a kind it does not model). Every table must be
// there and hold one entry or more, and every value a literal: anything else is an error, never
// an empty table, since a world reading no form would refuse no owner and health would read that
// as production's answer. The build constraint keeps it out of every package build; `go run` of
// the file by name builds it all the same.
package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: gh-selectors <daemon module dir>")
		os.Exit(2)
	}
	lines, err := table(os.Args[1])
	if err != nil {
		fmt.Fprintf(os.Stderr, "gh-selectors: %v\n", err)
		os.Exit(1)
	}
	for _, line := range lines {
		fmt.Println(line)
	}
}

// packages are the daemon module's packages the table is declared in: the wrapper's own, and
// internal/ghconfig, which holds the grammar of an owner's login it refuses any other by.
var packages = []string{filepath.Join("cmd", "legion"), filepath.Join("internal", "ghconfig")}

// patternVars are the wrapper's regular expressions the world reads a command with: how a help
// spells an option line, a command word and a usage's argument, and which option's help mentions
// a selector (gh_help.go); how a value names OWNER/REPO#N, a pull request's URL, a search
// qualifier and a gh api placeholder (gh_selectors.go); and an owner's login (ghconfig's).
var patternVars = []string{
	"ghFlagLine", "ghCommandWord", "ghSlotName", "ghMentionsSelector",
	"ghRepositoryNumber", "ghPullRequestPath", "ghQualifier", "ghPlaceholder", "ownerLogin",
}

// optionalPatterns are the patterns of patternVars one shape of the wrapper declares and another
// does not: ghRepositoryNumber, OWNER/REPO#N, which main's wrapper declared before #4424
// (fdce4e94d2) for the one leaf whose quirk read it (`agent-task view`'s readsRepositoryNumber)
// and declares no longer, since it refuses `gh agent-task` whole. A source that does not declare
// it prints no line for it; gh-selectors.ts holds the printout to one shape, the pattern and a
// quirk reading it both, or neither.
var optionalPatterns = map[string]bool{"ghRepositoryNumber": true}

// optionalReads are the read options one shape of the wrapper declares and another does not:
// ghGraphQLReadOptions, the options of a `gh api graphql` query that writes nothing, which main's
// wrapper declares since #4035 (cac1693d) and declared nowhere before. A source that does not
// declare it prints no line for it.
var optionalReads = map[string]bool{"ghGraphQLReadOptions": true}

// table is every line of the wrapper's table, read from the package-level declarations of the
// packages of the daemon module at module (their non-test files): the four maps, the patterns and
// the read grammar, each found by its name, each pattern with the file it is declared in, under
// module, so a reader that refuses one names where it was read. The constants are read too, which
// the read grammar's options are summed from.
func table(module string) ([]string, error) {
	values := map[string]ast.Expr{}
	files := map[string]string{}
	for _, dir := range packages {
		sources, err := filepath.Glob(filepath.Join(module, dir, "*.go"))
		if err != nil || len(sources) == 0 {
			return nil, fmt.Errorf("the daemon module %s holds no Go source in %s", module, dir)
		}
		for _, source := range sources {
			if strings.HasSuffix(source, "_test.go") {
				continue
			}
			file, err := parser.ParseFile(token.NewFileSet(), source, nil, 0)
			if err != nil {
				return nil, fmt.Errorf("read the wrapper's %s: %w", source, err)
			}
			for _, decl := range file.Decls {
				gen, isGen := decl.(*ast.GenDecl)
				if !isGen || (gen.Tok != token.VAR && gen.Tok != token.CONST) {
					continue
				}
				for _, spec := range gen.Specs {
					value, isValue := spec.(*ast.ValueSpec)
					if !isValue || len(value.Names) != len(value.Values) {
						continue
					}
					for i, name := range value.Names {
						values[name.Name] = value.Values[i]
						files[name.Name] = filepath.ToSlash(filepath.Join(dir, filepath.Base(source)))
					}
				}
			}
		}
	}
	var lines []string
	var unprintable []string
	add := func(fields ...string) {
		for _, field := range fields {
			if strings.ContainsAny(field, "\t\n") {
				unprintable = append(unprintable, field)
			}
		}
		lines = append(lines, strings.Join(fields, "\t"))
	}
	if err := eachEntry(values, "ghSlotKinds", func(key string, value ast.Expr) error {
		kind, many, err := argKind(value)
		add("slot", key, kind, many)
		return err
	}); err != nil {
		return nil, err
	}
	if err := eachEntry(values, "ghOptionKinds", func(key string, value ast.Expr) error {
		kind, err := stringOf(value)
		add("option", key, kind)
		return err
	}); err != nil {
		return nil, err
	}
	if err := eachEntry(values, "ghLeafQuirks", func(path string, value ast.Expr) error {
		return quirk(path, value, add)
	}); err != nil {
		return nil, err
	}
	if err := eachEntry(values, "ghKindRank", func(kind string, value ast.Expr) error {
		literal, isLiteral := value.(*ast.BasicLit)
		if !isLiteral || literal.Kind != token.INT {
			return fmt.Errorf("%s is no integer literal", kind)
		}
		add("rank", kind, literal.Value)
		return nil
	}); err != nil {
		return nil, err
	}
	for _, name := range patternVars {
		if values[name] == nil && optionalPatterns[name] {
			continue
		}
		pattern, err := patternOf(values[name])
		if err != nil {
			return nil, fmt.Errorf("the wrapper's %s: %w", name, err)
		}
		add("pattern", name, pattern, files[name])
	}
	if err := eachEntry(values, "ghReadLeaves", func(path string, value ast.Expr) error {
		options, err := readOptions(values, value)
		add("read-leaf", path)
		for spelling, takes := range options {
			add("read", path, spelling, takes)
		}
		return err
	}); err != nil {
		return nil, err
	}
	for _, name := range []string{"ghAPIReadOptions", "ghGraphQLReadOptions"} {
		if values[name] == nil && optionalReads[name] {
			continue
		}
		options, err := readOptions(values, values[name])
		if err != nil {
			return nil, fmt.Errorf("the wrapper's %s: %w", name, err)
		}
		kind := map[string]string{"ghAPIReadOptions": "api-read", "ghGraphQLReadOptions": "graphql-read"}[name]
		for spelling, takes := range options {
			add(kind, spelling, takes)
		}
	}
	if err := eachEntry(values, "ghMethodOverrideHeaders", func(name string, value ast.Expr) error {
		set, err := boolOf(value)
		if err == nil && !set {
			err = fmt.Errorf("is false, which names no header the wrapper reads as a method")
		}
		add("override-header", name)
		return err
	}); err != nil {
		return nil, err
	}
	if len(unprintable) > 0 {
		return nil, fmt.Errorf("the wrapper's table holds %q, which a line of this printout cannot carry (a tab or a newline)", unprintable)
	}
	slices.Sort(lines)
	return lines, nil
}

// eachEntry calls each with the key and the value of every entry of the map literal declared as
// name, refusing a declaration that is not there, is no map literal, holds no entry, or keys one
// by anything but a string literal.
func eachEntry(values map[string]ast.Expr, name string, each func(key string, value ast.Expr) error) error {
	declared, found := values[name]
	if !found {
		return fmt.Errorf("the wrapper declares no %s: it reads a command's selectors some other way now", name)
	}
	literal, isLiteral := declared.(*ast.CompositeLit)
	if !isLiteral {
		return fmt.Errorf("the wrapper's %s is no map literal", name)
	}
	if len(literal.Elts) == 0 {
		return fmt.Errorf("the wrapper's %s holds no entry", name)
	}
	for _, elt := range literal.Elts {
		entry, isEntry := elt.(*ast.KeyValueExpr)
		if !isEntry {
			return fmt.Errorf("the wrapper's %s holds an element that is no key and value", name)
		}
		key, err := stringOf(entry.Key)
		if err != nil {
			return fmt.Errorf("the wrapper's %s: a key %w", name, err)
		}
		if err := each(key, entry.Value); err != nil {
			return fmt.Errorf("the wrapper's %s, %q: %w", name, key, err)
		}
	}
	return nil
}

// argKind is a ghArgKind literal's kind and whether it stands for more than one argument, "0" or
// "1": `{kind: "url", many: true}`, `{many: true}` (no kind), or `{}`.
func argKind(value ast.Expr) (kind, many string, err error) {
	many = "0"
	err = eachField(value, func(field string, value ast.Expr) error {
		switch field {
		case "kind":
			kind, err = stringOf(value)
			return err
		case "many":
			set, err := boolOf(value)
			if set {
				many = "1"
			}
			return err
		}
		return fmt.Errorf("sets the field %s, which a ghArgKind of this reading has not", field)
	})
	return kind, many, err
}

// quirk adds the lines of one ghLeafQuirk literal: a line for each boolean it sets true and each
// string it sets, and one for each of its own argument kinds.
func quirk(path string, value ast.Expr, add func(fields ...string)) error {
	return eachField(value, func(field string, value ast.Expr) error {
		if field == "slotKinds" {
			literal, isLiteral := value.(*ast.CompositeLit)
			if !isLiteral || len(literal.Elts) == 0 {
				return fmt.Errorf("its slotKinds is no map literal holding an entry")
			}
			for _, elt := range literal.Elts {
				entry, isEntry := elt.(*ast.KeyValueExpr)
				if !isEntry {
					return fmt.Errorf("its slotKinds holds an element that is no key and value")
				}
				name, err := stringOf(entry.Key)
				if err != nil {
					return fmt.Errorf("its slotKinds: a key %w", err)
				}
				kind, many, err := argKind(entry.Value)
				if err != nil {
					return fmt.Errorf("its slotKinds, %q: %w", name, err)
				}
				add("quirk-slot", path, name, kind, many)
			}
			return nil
		}
		if text, err := stringOf(value); err == nil {
			add("quirk", path, field, text)
			return nil
		}
		set, err := boolOf(value)
		if err != nil {
			return fmt.Errorf("sets %s to no string or boolean literal", field)
		}
		if set {
			add("quirk", path, field, "true")
		}
		return nil
	})
}

// eachField calls each with the name and the value of every field the struct literal value sets
// by name.
func eachField(value ast.Expr, each func(field string, value ast.Expr) error) error {
	literal, isLiteral := value.(*ast.CompositeLit)
	if !isLiteral {
		return fmt.Errorf("is no struct literal")
	}
	for _, elt := range literal.Elts {
		entry, isEntry := elt.(*ast.KeyValueExpr)
		if !isEntry {
			return fmt.Errorf("sets a field by its place rather than its name")
		}
		field, isIdent := entry.Key.(*ast.Ident)
		if !isIdent {
			return fmt.Errorf("sets a field this reading cannot name")
		}
		if err := each(field.Name, entry.Value); err != nil {
			return err
		}
	}
	return nil
}

// stringOf is the string a string literal spells.
func stringOf(value ast.Expr) (string, error) {
	literal, isLiteral := value.(*ast.BasicLit)
	if !isLiteral || literal.Kind != token.STRING {
		return "", fmt.Errorf("is no string literal")
	}
	return strconv.Unquote(literal.Value)
}

// boolOf is the boolean `true` or `false` spells.
func boolOf(value ast.Expr) (bool, error) {
	ident, isIdent := value.(*ast.Ident)
	if !isIdent || (ident.Name != "true" && ident.Name != "false") {
		return false, fmt.Errorf("is no boolean literal")
	}
	return ident.Name == "true", nil
}

// patternOf is the expression `regexp.MustCompile(<string literal>)` compiles.
func patternOf(value ast.Expr) (string, error) {
	call, isCall := value.(*ast.CallExpr)
	if value == nil || !isCall || len(call.Args) != 1 {
		return "", fmt.Errorf("is not declared as regexp.MustCompile of one expression")
	}
	selector, isSelector := call.Fun.(*ast.SelectorExpr)
	if !isSelector || selector.Sel.Name != "MustCompile" {
		return "", fmt.Errorf("is not declared as regexp.MustCompile of one expression")
	}
	return stringOf(call.Args[0])
}

// readOptions is the options `ghReadOptions(<values>, <switches>)` declares, each spelling "1"
// where it takes a value and "0" where it stands alone, as that function builds its map: the
// spellings of the first argument, then of the second, a spelling in both standing alone.
func readOptions(values map[string]ast.Expr, value ast.Expr) (map[string]string, error) {
	call, isCall := value.(*ast.CallExpr)
	if value == nil || !isCall || len(call.Args) != 2 {
		return nil, fmt.Errorf("is not declared as ghReadOptions of two string expressions")
	}
	if fun, isIdent := call.Fun.(*ast.Ident); !isIdent || fun.Name != "ghReadOptions" {
		return nil, fmt.Errorf("is not declared as ghReadOptions of two string expressions")
	}
	options := map[string]string{}
	for i, takes := range []string{"1", "0"} {
		text, err := constantString(values, call.Args[i], 0)
		if err != nil {
			return nil, err
		}
		for _, spelling := range strings.Fields(text) {
			options[spelling] = takes
		}
	}
	return options, nil
}

// constantString is the string value spells: a string literal, a package-level name declared as
// one, or a sum of them, parenthesized or not.
func constantString(values map[string]ast.Expr, value ast.Expr, depth int) (string, error) {
	if depth > 16 {
		return "", fmt.Errorf("names constants more than 16 deep")
	}
	switch v := value.(type) {
	case *ast.BasicLit:
		return stringOf(v)
	case *ast.ParenExpr:
		return constantString(values, v.X, depth+1)
	case *ast.Ident:
		named, found := values[v.Name]
		if !found {
			return "", fmt.Errorf("names %s, which no package-level declaration of the wrapper gives a value", v.Name)
		}
		return constantString(values, named, depth+1)
	case *ast.BinaryExpr:
		if v.Op == token.ADD {
			left, err := constantString(values, v.X, depth+1)
			if err != nil {
				return "", err
			}
			right, err := constantString(values, v.Y, depth+1)
			return left + right, err
		}
	}
	return "", fmt.Errorf("is no string literal, constant or sum of them")
}
