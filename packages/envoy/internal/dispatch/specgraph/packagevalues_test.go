package specgraph

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
)

// writerCalled is the one guard of what a writer of a sweep or of a base can reach, which
// changedFile, filesWritten and editTree each call with the writer's builder and the call of the
// writer: it takes a copy of each variable of the package (packageValuesNow), then builds the
// writer (build), then calls it (call), then compares each variable with its copy
// (packageValuesKept). Each of the three takes a builder, not a writer, so no code of a writer runs
// before the copy: a writer that changes a variable of the package, in the code that builds it or
// in the call, fails by name, as one that changes a table of the reader (relationKinds, slug, ...)
// would change the verdict of each later input.
func writerCalled[W any](t *testing.T, name string, build func() W, call func(W)) {
	t.Helper()
	values := packageValuesNow()
	call(build())
	packageValuesKept(t, name, values)
}

// packageValue is the copy of one variable of the package that packageValuesNow takes, by the
// variable's name.
type packageValue struct {
	name  string
	value any
}

// pointee is a variable of the package that holds a pointer, as packageValuesNow copies it: the
// address that it holds, and a copy of the object there, taken by value (pointeeOf). Each such
// variable points to an object of the standard library whose fields are unexported (a
// *regexp.Regexp, a *strings.Replacer, an error of errors.New), so the copy is an object of its own,
// which no writer holds, and what its fields point to no code of this package can write. A writer
// that points the variable to another object changes the address; one that writes the object
// whole (*slug = *other) or calls a method that changes it (Regexp.Longest) changes a field of the
// object, which the copy holds as it was.
type pointee struct {
	at    uintptr
	value any
}

// pointeeOf is the pointee of p, a pointer, or an error whose value is a pointer.
func pointeeOf(p any) pointee {
	v := reflect.ValueOf(p)
	c := reflect.New(v.Type().Elem()).Elem()
	c.Set(v.Elem())
	return pointee{v.Pointer(), c.Interface()}
}

// init builds the table of each *strings.Replacer of the package (pointerToken, valueReferences)
// before any test runs. A Replacer builds it at its first Replace, under a sync.Once, and then only
// reads it; so each copy of a Replacer that packageValuesNow takes is of the table that each later
// call reads, and no copy reads the Replacer while a first call of a test beside it builds it.
func init() {
	pointerToken.Replace("")
	valueReferences.Replace("")
}

// packageValuesNow is a copy of each variable of the package, of its code and of its tests, which
// shares with the variable no object that code of the package can write through (sharedObject): a
// value that holds no slice, map or pointer as it is, since a string cannot change through a copy
// of it; a slice or a map cloned, with each list in it; a statement or a check with its lists and
// its test copied; and a variable that holds a pointer as its address and a copy of its object
// (pointee), pointerToken among them. TestEachVariableOfThePackageIsGuarded fails for a variable
// that the list does not name and for a copy that shares an object with the variable. Each table
// that a sweep reads of its own is a function that gives a new value at each call (sweepMaps,
// sweepPaths, namesOfNothing, addableMembers, recordKinds, sweepFloors, scenarioFileTypes and the
// patterns of blockTexts), not a variable.
func packageValuesNow() []packageValue {
	statement := func(s Statement) Statement {
		s.Serves, s.About = slices.Clone(s.Serves), slices.Clone(s.About)
		return s
	}
	check := func(c Check) Check {
		if c.Test != nil {
			test := *c.Test
			c.Test = &test
		}
		return c
	}
	lists := func(m map[string][]string) map[string][]string {
		c := make(map[string][]string, len(m))
		for key, list := range m {
			c[key] = slices.Clone(list)
		}
		return c
	}
	faults := slices.Clone(plantedFaults)
	for i := range faults {
		faults[i].retired = slices.Clone(faults[i].retired)
	}
	return []packageValue{
		// The variables of the code.
		{"backtickSpan", pointeeOf(backtickSpan)},
		{"fileEnding", pointeeOf(fileEnding)},
		{"slashBetweenLetters", pointeeOf(slashBetweenLetters)},
		{"endsWithCall", pointeeOf(endsWithCall)},
		{"underscoreBetween", pointeeOf(underscoreBetween)},
		{"lowerCamel", pointeeOf(lowerCamel)},
		{"packageSymbol", pointeeOf(packageSymbol)},
		{"productNames", slices.Clone(productNames)},
		{"nameRun", pointeeOf(nameRun)},
		{"slug", pointeeOf(slug)},
		{"positionWord", pointeeOf(positionWord)},
		{"gitDiffArgs", slices.Clone(gitDiffArgs)},
		{"pointerToken", pointeeOf(pointerToken)},
		{"blockAttributes", lists(blockAttributes)},
		{"checkFields", checkFields},
		{"heading", pointeeOf(heading)},
		{"problemKinds", problemKinds},
		{"ErrUnprintable", pointeeOf(ErrUnprintable)},
		{"valueReferences", pointeeOf(valueReferences)},
		{"relationKinds", lists(relationKinds)},
		{"sentenceEnd", pointeeOf(sentenceEnd)},
		{"scenarioRequired", slices.Clone(scenarioRequired)},
		{"scenarioOptional", maps.Clone(scenarioOptional)},
		{"ErrSelector", pointeeOf(ErrSelector)},
		{"topLevelTestName", pointeeOf(topLevelTestName)},
		// The variables of the tests: the check case from which memberScenario builds the base of
		// TestAMemberLeftOutOfAFileIsReadByItsMark, and the tables of the other tests.
		{"checkStateApproved", checkStateApproved},
		{"checkStateRun", checkStateRun},
		{"checkStateTop", statement(checkStateTop)},
		{"checkStateLeaf", statement(checkStateLeaf)},
		{"checkStateCheck", check(checkStateCheck)},
		{"checkStateProduction", check(checkStateProduction)},
		{"checkStateProductionAgent", check(checkStateProductionAgent)},
		{"checkStateGraph", check(checkStateGraph)},
		{"checkStateProductionProgram", check(checkStateProductionProgram)},
		{"checkStateGraphProgram", check(checkStateGraphProgram)},
		{"checkStateCodeAgent", check(checkStateCodeAgent)},
		{"checkStateCodeQuery", check(checkStateCodeQuery)},
		{"checkStateTie", slices.Clone(checkStateTie)},
		{"libraryInputTypes", slices.Clone(libraryInputTypes)},
		{"plantedFaults", faults},
		{"statementLeafTwo", statement(statementLeafTwo)},
		{"statementLeafTwoCheck", check(statementLeafTwoCheck)},
		{"statementSupporter", statement(statementSupporter)},
		{"statementSupporterCheck", check(statementSupporterCheck)},
		{"statementTopCheck", check(statementTopCheck)},
		{"statementTopProduction", check(statementTopProduction)},
		{"statementFast", statement(statementFast)},
		{"statementFastCheck", check(statementFastCheck)},
		{"statementLeafSupporter", statement(statementLeafSupporter)},
		{"statementLeafSupporterCheck", check(statementLeafSupporterCheck)},
		{"statementSupporterOfSupporter", statement(statementSupporterOfSupporter)},
		{"statementSupporterOfSupporterCheck", check(statementSupporterOfSupporterCheck)},
		{"longName", longName},
		{"jsonLongName", jsonLongName},
		{"none", slices.Clone(none)},
	}
}

// packageValuesKept fails the test unless each variable of the package holds what before, the copy
// that packageValuesNow took before a writer of the file name was built, holds, so that a writer
// that changes one, for a later call of its own sweep or for a test that runs beside it, fails by
// name.
func packageValuesKept(t *testing.T, name string, before []packageValue) {
	t.Helper()
	for i, now := range packageValuesNow() {
		if !reflect.DeepEqual(now.value, before[i].value) {
			t.Fatalf("a writer of %s changed %s, a value of the package: it holds %.300s after the writer ran, where it held %.300s", name, now.name, fmt.Sprintf("%+v", now.value), fmt.Sprintf("%+v", before[i].value))
		}
	}
}

// Each variable of the package is in the copy that the guard of the writers takes
// (packageValuesNow), so that a writer that changes one fails by name: this test reads each file of
// the package with go/parser and fails for each variable declared at its top level that the copy
// does not name, for each name of the copy that names no such variable or that the copy names
// twice, and when it finds no variable at all, since it then read none. It also takes two copies
// and fails for each object that both hold and that code of the package can write through
// (sharedObject): a copy that held a variable's slice, map or pointer would change with the
// variable, and the guard would not see the change.
func TestEachVariableOfThePackageIsGuarded(t *testing.T) {
	t.Parallel()
	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	declared := map[string]string{} // the name of each variable, and where the package declares it
	for _, path := range paths {
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		if f.Name.Name != "specgraph" {
			continue
		}
		for _, decl := range f.Decls {
			if g, isVar := decl.(*ast.GenDecl); isVar && g.Tok == token.VAR {
				for _, spec := range g.Specs {
					for _, name := range spec.(*ast.ValueSpec).Names {
						if name.Name != "_" {
							declared[name.Name] = fset.Position(name.Pos()).String()
						}
					}
				}
			}
		}
	}
	if len(declared) == 0 {
		t.Fatal("go/parser found no variable declared at the top level of the package, so the test read none")
	}
	copied := map[string]int{}
	for _, v := range packageValuesNow() {
		copied[v.name]++
	}
	for _, name := range slices.Sorted(maps.Keys(declared)) {
		if copied[name] == 0 {
			t.Errorf("%s declares the variable %s, and packageValuesNow does not copy it, so a writer that changes it passes", declared[name], name)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(copied)) {
		switch {
		case declared[name] == "":
			t.Errorf("packageValuesNow copies %s, and the package declares no variable of that name", name)
		case copied[name] > 1:
			t.Errorf("packageValuesNow copies %s %d times", name, copied[name])
		}
	}
	a, b := packageValuesNow(), packageValuesNow()
	for i := range a {
		if at, shared := sharedObject(reflect.ValueOf(a[i].value), reflect.ValueOf(b[i].value), ""); shared {
			t.Errorf("two copies of %s hold one object at %q, which code of the package can write through, so a writer that changes the variable there changes its copy too", a[i].name, at)
		}
	}
}

// sharedObject is the first object that a and b, two copies of one variable, both hold, by its
// path, and whether there is one: a map, the array of a slice with room, or the object of a
// pointer, through which code of the package can write. It does not enter a string, which no code
// can change through a copy of it, an unexported field of a type of another package, or a value of
// an unexported type of another package (reflect's own types among them), which no code of this
// package can name.
func sharedObject(a, b reflect.Value, at string) (string, bool) {
	if !a.IsValid() || !b.IsValid() || a.Type() != b.Type() || foreignUnexported(a.Type()) {
		return "", false
	}
	switch a.Kind() {
	case reflect.Map:
		if a.IsNil() || b.IsNil() {
			return "", false
		}
		if a.UnsafePointer() == b.UnsafePointer() {
			return at, true
		}
		for _, key := range a.MapKeys() {
			if p, shared := sharedObject(a.MapIndex(key), b.MapIndex(key), fmt.Sprintf("%s[%v]", at, key)); shared {
				return p, true
			}
		}
	case reflect.Slice:
		if a.Cap() > 0 && b.Cap() > 0 && a.UnsafePointer() == b.UnsafePointer() {
			return at, true
		}
		fallthrough
	case reflect.Array:
		for i := range min(a.Len(), b.Len()) {
			if p, shared := sharedObject(a.Index(i), b.Index(i), fmt.Sprintf("%s[%d]", at, i)); shared {
				return p, true
			}
		}
	case reflect.Pointer:
		if a.IsNil() || b.IsNil() {
			return "", false
		}
		if a.Pointer() == b.Pointer() {
			return at, true
		}
		return sharedObject(a.Elem(), b.Elem(), at)
	case reflect.Interface:
		return sharedObject(a.Elem(), b.Elem(), at)
	case reflect.Struct:
		for i := range a.NumField() {
			if f := a.Type().Field(i); f.IsExported() || f.PkgPath == reflect.TypeFor[Input]().PkgPath() {
				if p, shared := sharedObject(a.Field(i), b.Field(i), at+"."+f.Name); shared {
					return p, true
				}
			}
		}
	}
	return "", false
}

// foreignUnexported is whether t, or what t points to, is an unexported type of another package,
// such as reflect's own type of a reflect.Type.
func foreignUnexported(t reflect.Type) bool {
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t.PkgPath() != "" && t.PkgPath() != reflect.TypeFor[Input]().PkgPath() && !token.IsExported(t.Name())
}
