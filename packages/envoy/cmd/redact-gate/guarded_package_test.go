package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// reflectStub is the part of package reflect a guarded read calls, so the census reads the same
// callee names (types.Func.FullName) and the same method signatures it reads in a real tree.
const reflectStub = `package reflect

type Kind uint

const (
	Invalid Kind = iota
	Bool
	Pointer
	Struct
)

type Type interface{ PkgPath() string }

type Value struct{ p *int }

func ValueOf(i any) Value                      { return Value{} }
func TypeFor[T any]() Type                     { return nil }
func (v Value) Kind() Kind                     { return Invalid }
func (v Value) IsNil() bool                    { return false }
func (v Value) Elem() Value                    { return v }
func (v Value) Type() Type                     { return nil }
func (v Value) FieldByName(name string) Value  { return v }
func (v Value) FieldByIndex(index []int) Value { return v }
func (v Value) Bool() bool                     { return false }
func (v Value) String() string                 { return "" }
func (v Value) Interface() any                 { return nil }
func (v Value) Call(in []Value) []Value        { return nil }
func (v Value) CallSlice(in []Value) []Value   { return nil }
func (v Value) Method(i int) Value             { return v }
func (v Value) MethodByName(name string) Value { return v }
func (v Value) Addr() Value                    { return v }
func (v Value) Index(i int) Value              { return v }
func (v Value) Pointer() uintptr               { return 0 }
`

// libStub stands for a library whose handle the read reflects on: an exported type to name its
// package by, the interface its unexported handle hides behind, and the handle, which keeps the
// bool the read wants in an unexported field, as nats.go's legacy KV handle keeps useDirect.
const libStub = `package lib

type Conn struct{ n int }

type KeyValue interface{ Bucket() string }

type kvs struct{ useDirect bool }
`

// libWithoutFieldStub is a library none of whose struct types has the field the read names.
const libWithoutFieldStub = `package lib

type Conn struct{ n int }

type KeyValue interface{ Bucket() string }
`

// libInterfaceFieldStub keeps the field the read names as an interface, which can hold the box.
const libInterfaceFieldStub = `package lib

type Conn struct{ n int }

type KeyValue interface{ Bucket() string }

type kvs struct{ useDirect any }
`

// libPromotedFieldStub keeps it as an interface in an embedded struct, which FieldByName finds.
const libPromotedFieldStub = `package lib

type Conn struct{ n int }

type KeyValue interface{ Bucket() string }

type options struct{ useDirect []any }

type kvs struct{ options }
`

// innerStub is a second package lib can import, whose structs keep the field the read names as an
// interface, unexported: reflect's FieldByName finds it in a lib struct that embeds one of them,
// though go/types names the field inner's and not lib's. Outer reaches it one embedded struct
// deeper, through a cycle of embedded pointers. Exported keeps it exported, as UseDirect.
const innerStub = `package inner

type S struct{ useDirect any }

type Outer struct{ *Loop }

type Loop struct {
	*Outer
	useDirect any
}

type Exported struct{ UseDirect any }
`

// libWithInner is libStub importing inner, with decl added: libStub's kvs keeps a bool by the name
// the read names, and the type decl declares reaches inner's interface by that name.
func libWithInner(decl string) string {
	return strings.Replace(libStub, "package lib\n", "package lib\n\nimport \"example.com/inner\"\n", 1) + "\n" + decl + "\n"
}

// libKvsEmbeds is a lib whose kvs, the handle the read's guard admits, embeds embedded, a struct of
// inner, and whose other struct type keeps a bool named field.
func libKvsEmbeds(embedded, field string) string {
	return "package lib\n\nimport \"example.com/inner\"\n\ntype Conn struct{ n int }\n\ntype KeyValue interface{ Bucket() string }\n\n" +
		"type kvs struct{ " + embedded + " }\n\ntype other struct{ " + field + " bool }\n"
}

// libFuncFieldStub keeps the field the read names, exported, as the func type funcType: holdsDynamic
// counts a func type as holding nothing, while Call or CallSlice on the result reaches what the func
// answers.
func libFuncFieldStub(funcType string) string {
	return "package lib\n\ntype Conn struct{ n int }\n\ntype KeyValue interface{ Bucket() string }\n\ntype kvs struct{ UseDirect " + funcType + " }\n"
}

// libMethodStub keeps the field the read names, exported, as a struct with no interface field
// whose exported method answers an interface: Method or MethodByName, then Call, on the result
// reaches that answer.
const libMethodStub = `package lib

type Conn struct{ n int }

type KeyValue interface{ Bucket() string }

type opts struct{ n int }

func (opts) Held() any { return nil }

type kvs struct{ UseDirect opts }
`

// resultRead is guardedRead reading the field named field, its result then used as use reads it.
func resultRead(field, use string) string {
	read := strings.Replace(guardedRead, `kvs.FieldByName("useDirect")`, `kvs.FieldByName("`+field+`")`, 1)
	return strings.Replace(read, "return useDirect.Bool()", use, 1)
}

// libLocalTypeStub declares, inside a function, a struct type that keeps it as an interface: a
// type no export data holds, whose values still name lib as their package.
const libLocalTypeStub = libStub + `
func Wrap(v any) any {
	type held struct{ useDirect any }
	return &held{useDirect: v}
}
`

// genericLibStub is libStub with one generic type, as listenerwire declares APIErrorAs[Text]: a
// type of the guarded package whose instance holds whatever its type argument is.
const genericLibStub = libStub + `
type Holder[T any] struct{ V T }
`

// boxStub stands for package redact: the box keeps its text in an unexported field.
const boxStub = `package box

type Text struct{ text *string }
`

// genericRead holds a box in the guarded package's generic type and reads the box's storage under
// the guard main's readsDirect uses: an instance of lib.Holder[box.Text] is lib's to reflect, so
// the guard holds it to lib's package, while lib's import closure holds no box.
const genericRead = `package pkg

import (
	"reflect"

	"example.com/box"
	"example.com/lib"
)

var libPackage = reflect.TypeFor[lib.Conn]().PkgPath()

func readsDirect(held lib.Holder[box.Text]) bool {
	kvs := reflect.ValueOf(held)
	if kvs.Kind() != reflect.Struct || kvs.Type().PkgPath() != libPackage {
		return true
	}
	text := kvs.FieldByIndex([]int{0, 0})
	return text.IsNil()
}
`

// guardedRead is main's readsDirect (internal/bus/kv.go) over the stubs: the handle's type is
// held to lib's package by its PkgPath guard before its unexported field is read.
const guardedRead = `package pkg

import (
	"reflect"

	"example.com/lib"
)

var libPackage = reflect.TypeFor[lib.Conn]().PkgPath()

func readsDirect(legacy lib.KeyValue) bool {
	handle := reflect.ValueOf(legacy)
	if handle.Kind() != reflect.Pointer || handle.IsNil() {
		return true
	}
	kvs := handle.Elem()
	if kvs.Kind() != reflect.Struct || kvs.Type().PkgPath() != libPackage {
		return true
	}
	useDirect := kvs.FieldByName("useDirect")
	return useDirect.Bool()
}
`

// stubImporter answers the stub packages by path.
type stubImporter map[string]*types.Package

func (s stubImporter) Import(path string) (*types.Package, error) {
	if p, ok := s[path]; ok {
		return p, nil
	}
	return nil, fmt.Errorf("no stub of %s", path)
}

// guardedCensus runs the census over source, package example.com/pkg in /repo/pkg/kv.go, with
// entry its one reflection entry (none where nil) and closure lib's import closure (none recorded
// where nil), and answers the one reflection finding it lists. lib is libStub.
func guardedCensus(t *testing.T, source string, entry *reflectionExempt, closure map[string]bool) finding {
	t.Helper()
	return guardedCensusOf(t, libStub, source, entry, closure)
}

// guardedCensusOf is guardedCensus with lib's source given, which may import inner (innerStub).
func guardedCensusOf(t *testing.T, lib, source string, entry *reflectionExempt, closure map[string]bool) finding {
	t.Helper()
	fset := token.NewFileSet()
	stubs := stubImporter{}
	var libSource *checkedSource
	for _, stub := range []struct{ path, source string }{{"reflect", reflectStub}, {"example.com/inner", innerStub}, {"example.com/lib", lib}, {"example.com/box", boxStub}} {
		f, err := parser.ParseFile(fset, stub.path+".go", stub.source, 0)
		if err != nil {
			t.Fatal(err)
		}
		stubInfo := &types.Info{Defs: map[*ast.Ident]types.Object{}}
		pkg, err := (&types.Config{Importer: stubs}).Check(stub.path, fset, []*ast.File{f}, stubInfo)
		if err != nil {
			t.Fatal(err)
		}
		stubs[stub.path] = pkg
		if stub.path == "example.com/lib" {
			libSource = &checkedSource{files: []*ast.File{f}, info: stubInfo}
		}
	}
	f, err := parser.ParseFile(fset, "/repo/pkg/kv.go", source, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	info := &types.Info{
		Uses:       map[*ast.Ident]types.Object{},
		Defs:       map[*ast.Ident]types.Object{},
		Selections: map[*ast.SelectorExpr]*types.Selection{},
		Types:      map[ast.Expr]types.TypeAndValue{},
		Instances:  map[*ast.Ident]types.Instance{},
	}
	if _, err := (&types.Config{Importer: stubs}).Check("example.com/pkg", fset, []*ast.File{f}, info); err != nil {
		t.Fatalf("the case does not type-check: %v", err)
	}
	g := &gate{}
	g.inv.Gate.Box = boxConfig{Type: "example.com/box.Text", Field: "text"}
	g.inv.Gate.External = []string{"example.com/oauth.RetrieveError"}
	if entry != nil {
		g.inv.Gate.Reflection = []reflectionExempt{*entry}
	}
	if closure != nil {
		g.imports = map[string]map[string]bool{"example.com/lib": closure}
	}
	// The guarded package as the census type-checks it from source (sourceChecked).
	g.declared = map[string]*checkedSource{"example.com/lib": libSource}
	var c census
	g.structural(&c, "/repo", "example.com/pkg", nil, fset, []*ast.File{f}, info)
	if len(c.Reflect) != 1 {
		t.Fatalf("the census listed %d reflection sites, want the one FieldByName: %+v", len(c.Reflect), c.Reflect)
	}
	return c.Reflect[0]
}

// TestAGuardedPackageEntryPassesOnlyAReadItsGuardHoldsToACleanPackage: a guarded_package entry
// passes main's read of a library handle's unexported field, behind an interface, only as
// guardedPackageRead (guarded.go) admits it, its guard's package written in place or through an
// unexported variable; every other shape, each breaking one of that rule's conditions, is refused
// with its reason, and with no entry the site is refused as before.
func TestAGuardedPackageEntryPassesOnlyAReadItsGuardHoldsToACleanPackage(t *testing.T) {
	entry := &reflectionExempt{File: "pkg/kv.go", Function: "example.com/pkg.readsDirect", GuardedPackage: "example.com/lib", Why: "test"}
	clean := map[string]bool{"example.com/lib": true, "fmt": true}
	edit := func(old, new string) string {
		if !strings.Contains(guardedRead, old) {
			t.Fatalf("the case's edit finds no %q in the read", old)
		}
		return strings.Replace(guardedRead, old, new, 1)
	}
	const guard = "kvs.Kind() != reflect.Struct || kvs.Type().PkgPath() != libPackage"
	for _, c := range []struct {
		name    string
		source  string
		entry   *reflectionExempt
		closure map[string]bool
		ok      bool
		why     string
	}{
		{"the guard through a package variable", guardedRead, entry, clean, true, "its guard holds its value's type to example.com/lib"},
		{"the guard in place", edit("!= libPackage", "!= reflect.TypeFor[lib.Conn]().PkgPath()"), entry, clean, true, "its guard holds its value's type to example.com/lib"},
		{"no entry", guardedRead, nil, clean, false, "no inventory entry names this site"},
		{"no PkgPath guard", edit(guard, "kvs.Kind() != reflect.Struct"), entry, clean, false, "does not run only under a guard"},
		{"the guard does not return", edit("!= libPackage {\n\t\treturn true\n\t}", "!= libPackage {\n\t\tprintln()\n\t}"), entry, clean, false, "does not run only under a guard"},
		{"the receiver stored again after the guard", edit("\tuseDirect :=", "\tkvs = handle\n\tuseDirect :="), entry, clean, false, "stored to 2 times"},
		{"the receiver's address taken", edit("\tuseDirect :=", "\t_ = &kvs\n\tuseDirect :="), entry, clean, false, "stored to 2 times"},
		{"the guard's variable stored again", guardedRead + "\nfunc init() { libPackage = \"example.com/lib\" }\n", entry, clean, false, "does not run only under a guard"},
		{"the guard's variable is exported, so another package can store to it", strings.ReplaceAll(guardedRead, "libPackage", "LibPackage"), entry, clean, false, "does not run only under a guard"},
		{"a label in the function", edit("\treturn useDirect.Bool()", "\tif false {\n\t\tgoto done\n\t}\ndone:\n\treturn useDirect.Bool()"), entry, clean, false, "holds a label"},
		{"a pointer type names no package", edit("reflect.TypeFor[lib.Conn]()", "reflect.TypeFor[*lib.Conn]()"), entry, clean, false, "does not run only under a guard"},
		{"the entry names another package", guardedRead, &reflectionExempt{File: entry.File, Function: entry.Function, GuardedPackage: "example.com/other", Why: "test"}, clean, false, "the guard here holds the value's type to example.com/lib"},
		{"the closure holds the box", guardedRead, entry, map[string]bool{"example.com/lib": true, "example.com/box": true}, false, "holds the box's package example.com/box"},
		{"the closure holds an external error type's package", guardedRead, entry, map[string]bool{"example.com/lib": true, "example.com/oauth": true}, false, "the external error type example.com/oauth.RetrieveError"},
		{"no closure listed", guardedRead, entry, nil, false, "whose import closure no go list of these builds names"},
		{"FieldByIndex in place of FieldByName", edit(`kvs.FieldByName("useDirect")`, "kvs.FieldByIndex([]int{0})"), entry, clean, false, "admits a reflect.Value.FieldByName with a constant name alone"},
		{"a field name that is no constant", edit(`kvs.FieldByName("useDirect")`, "kvs.FieldByName(fieldName)") + "\nvar fieldName = \"useDirect\"\n", entry, clean, false, "whose name is a constant"},
		{"Elem on the result", edit("return useDirect.Bool()", "return useDirect.Elem().Bool()"), entry, clean, false, "its result's Elem reaches what the field holds"},
		{"Interface on the result", edit("return useDirect.Bool()", "return useDirect.Interface() != nil"), entry, clean, false, "its result's Interface reaches what the field holds"},
		{"Kind and Bool on the result, as kv.go reads it", edit("\treturn useDirect.Bool()", "\tif useDirect.Kind() != reflect.Bool {\n\t\treturn true\n\t}\n\treturn useDirect.Bool()"), entry, clean, true, "its guard holds its value's type to example.com/lib"},
		{"String on the result, which answers a closed value", edit("return useDirect.Bool()", `return useDirect.String() != ""`), entry, clean, true, "its guard holds its value's type to example.com/lib"},
		{"Pointer on the result, which answers an address", edit("return useDirect.Bool()", "return useDirect.Pointer() != 0"), entry, clean, false, "its result's Pointer reaches what the field holds, or past it: it answers uintptr"},
		{"Type on the result, whose Method(i).Func calls the field's methods", edit("return useDirect.Bool()", `return useDirect.Type().PkgPath() != ""`), entry, clean, false, "its result's Type reaches what the field holds, or past it: it answers reflect.Type"},
		{"the result passed on", edit("return useDirect.Bool()", "return use(useDirect)") + "\nfunc use(reflect.Value) bool { return false }\n", entry, clean, false, "used 1 times, 0 of them as a method call's receiver"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := guardedCensus(t, c.source, c.entry, c.closure)
			if got.OK != c.ok || !strings.Contains(got.Why, c.why) {
				t.Fatalf("the census answered ok=%t %q at %s:%d; want ok=%t and %q in its reason", got.OK, got.Why, got.File, got.Line, c.ok, c.why)
			}
		})
	}
	// The guarded package's own types, read from its source: a generic type's instance is lib's to
	// reflect and holds whatever its type argument is, so the guard holds lib.Holder[box.Text] to
	// lib while lib's import closure shows no box; a field the read names that is an interface, or
	// holds one, can hold the box at run time, whether the struct type keeping it declares it or
	// embeds it, at package level or in a function; so can one of another package's struct that a
	// lib type embeds, by value or by pointer, at any depth, or is defined over, since reflect's
	// FieldByName matches a name's text whatever package declares it, beside a lib type that keeps
	// a bool by that name, and one FieldByName does not return, shadowed or ambiguous, is held all
	// the same; a package none of whose struct types has that field leaves the census nothing to
	// read; and a field with no interface in it still reaches a value of any type through its
	// result's Call or CallSlice (an exported func field), its Method or MethodByName (an exported
	// method of the field's type), or Addr or Index first, so each is refused on the result, the read
	// as the result's receiver and through the variable that holds it alike.
	fieldByName := strings.Replace(strings.Replace(genericRead, "kvs.FieldByIndex([]int{0, 0})", `kvs.FieldByName("V")`, 1), "text.IsNil()", "text.Bool()", 1)
	for _, c := range []struct{ name, lib, source, why string }{
		{"a generic type of the guarded package holds the box, read by FieldByIndex", genericLibStub, genericRead, "admits a reflect.Value.FieldByName with a constant name alone"},
		{"a generic type of the guarded package holds the box, read by FieldByName", genericLibStub, fieldByName, "which declares the generic type Holder"},
		{"the guarded package declares a generic type", genericLibStub, guardedRead, "which declares the generic type Holder"},
		{"the field is an interface", libInterfaceFieldStub, guardedRead, "types the field useDirect with an interface or a type parameter, which can hold the box at run time: kvs.useDirect (any)"},
		{"the field holds interfaces, promoted from an embedded struct", libPromotedFieldStub, guardedRead, "kvs.useDirect ([]any)"},
		{"a struct type declared in a function keeps the field as an interface", libLocalTypeStub, guardedRead, "held.useDirect (any)"},
		{"no struct type of the package has the field", libWithoutFieldStub, guardedRead, "no struct type example.com/lib declares has a field useDirect"},
		{"another package's struct embedded by value keeps the field as an interface", libWithInner("type wrapped struct{ inner.S }"), guardedRead, "wrapped.useDirect (any), promoted through the embedded example.com/inner.S"},
		{"another package's struct embedded by pointer keeps the field as an interface", libWithInner("type wrapped struct{ *inner.S }"), guardedRead, "wrapped.useDirect (any), promoted through the embedded *example.com/inner.S"},
		{"the handle itself embeds another package's struct whose unexported field is an interface, beside another lib type that keeps a bool", libKvsEmbeds("inner.S", "useDirect"), guardedRead, "kvs.useDirect (any), promoted through the embedded example.com/inner.S"},
		{"control: the handle embeds another package's struct whose exported field is an interface", libKvsEmbeds("inner.Exported", "UseDirect"), strings.Replace(guardedRead, `FieldByName("useDirect")`, `FieldByName("UseDirect")`, 1), "kvs.UseDirect (any)"},
		{"another package's struct two embeddings down, through a cycle of embedded pointers, keeps the field as an interface", libWithInner("type wrapped struct{ inner.Outer }"), guardedRead, "wrapped.useDirect (any), promoted through the embedded example.com/inner.Outer, then *example.com/inner.Loop"},
		{"a lib type defined over another package's struct keeps the field as an interface", libWithInner("type wrapped inner.S"), guardedRead, "wrapped.useDirect (any)"},
		{"a field FieldByName does not return, shadowed by a shallower one, is held all the same", libWithInner("type wrapped struct {\n\tuseDirect bool\n\tinner.S\n}"), guardedRead, "wrapped.useDirect (any), promoted through the embedded example.com/inner.S"},
		{"two fields of the name at one depth, which FieldByName does not return, are held all the same", libWithInner("type wrapped struct {\n\tinner.S\n\tplain\n}\n\ntype plain struct{ useDirect bool }"), guardedRead, "wrapped.useDirect (any), promoted through the embedded example.com/inner.S"},
		{"Call on an exported func field answering an interface", libFuncFieldStub("func() any"), resultRead("UseDirect", "return useDirect.Call(nil)[0].IsNil()"), "its result's Call reaches what the field holds, or past it: it answers []reflect.Value"},
		{"Call on the read's result itself, held by no variable", libFuncFieldStub("func() any"), strings.Replace(guardedRead, "useDirect := kvs.FieldByName(\"useDirect\")\n\treturn useDirect.Bool()", `return kvs.FieldByName("UseDirect").Call(nil)[0].IsNil()`, 1), "its result's Call reaches what the field holds"},
		{"CallSlice on an exported variadic func field", libFuncFieldStub("func(...any) any"), resultRead("UseDirect", "return useDirect.CallSlice(nil)[0].IsNil()"), "its result's CallSlice reaches what the field holds"},
		{"MethodByName and Call on a field whose method answers an interface", libMethodStub, resultRead("UseDirect", `return useDirect.MethodByName("Held").Call(nil)[0].IsNil()`), "its result's MethodByName reaches what the field holds, or past it: it answers reflect.Value"},
		{"Method and Call on a field whose method answers an interface", libMethodStub, resultRead("UseDirect", "return useDirect.Method(0).Call(nil)[0].IsNil()"), "its result's Method reaches what the field holds"},
		{"Addr, then MethodByName and Call on the field's address", libMethodStub, resultRead("UseDirect", `return useDirect.Addr().MethodByName("Held").Call(nil)[0].IsNil()`), "its result's Addr reaches what the field holds"},
		{"Index, then Call on an element of a slice of funcs", libFuncFieldStub("[]func() any"), resultRead("UseDirect", "return useDirect.Index(0).Call(nil)[0].IsNil()"), "its result's Index reaches what the field holds"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := guardedCensusOf(t, c.lib, c.source, entry, clean)
			if got.OK || !strings.Contains(got.Why, c.why) {
				t.Fatalf("the census answered ok=%t %q at %s:%d; want ok=false and %q in its reason", got.OK, got.Why, got.File, got.Line, c.why)
			}
		})
	}
	// Control: lib importing inner and keeping inner's struct in a field that is no embedding, which
	// FieldByName does not look into, passes, so each refusal above is its embedding's.
	if got := guardedCensusOf(t, libWithInner("type wrapped struct{ held inner.S }"), guardedRead, entry, clean); !got.OK || !strings.Contains(got.Why, "1 struct types example.com/lib declares") {
		t.Fatalf("control: the census answered ok=%t %q for a field of inner's struct that is no embedding; want ok=true over kvs alone", got.OK, got.Why)
	}
}

// exportModule is a module whose guarded package, lib, declares a type that embeds EMBED, a struct
// of another package, inner, whose unexported useDirect is INNER, beside kvs, which keeps a bool by
// that name; pkg's readsDirect is guardedRead.
var exportModule = map[string]string{
	"go.mod":         "module example.com/m\n\ngo 1.27\n",
	"inner/inner.go": "package inner\n\ntype S struct{ useDirect INNER }\n\nfunc New(v INNER) S { return S{useDirect: v} }\n",
	"lib/lib.go":     "package lib\n\nimport \"example.com/m/inner\"\n\ntype Conn struct{ n int }\n\ntype KeyValue interface{ Bucket() string }\n\ntype kvs struct{ useDirect bool }\n\ntype wrapped struct{ EMBED }\n",
	"pkg/kv.go":      strings.ReplaceAll(guardedRead, "example.com/lib", "example.com/m/lib"),
}

// exportedGuardedCensus writes exportModule, with inner's field typed innerType and lib's wrapped
// embedding embed, to a scratch directory, lists pkg with its export data and records each listed
// package's import closure as goModuleCensus does (recordImports), and runs the census over pkg
// (goPackage) as that listing builds it, with a guarded_package entry for readsDirect naming lib.
// It answers the one reflection finding the census lists.
func exportedGuardedCensus(t *testing.T, innerType, embed string) finding {
	t.Helper()
	t.Setenv("GOWORK", "off")
	t.Setenv("GOFLAGS", "")
	dir := t.TempDir()
	for name, text := range exportModule {
		text = strings.ReplaceAll(strings.ReplaceAll(text, "INNER", innerType), "EMBED", embed)
		path := filepath.Join(dir, "m", name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	listed, err := goList(filepath.Join(dir, "m"), hostBuild(), true, "./pkg")
	if err != nil {
		t.Fatal(err)
	}
	g := &gate{}
	for _, p := range listed {
		if p.Error != nil {
			t.Fatalf("%s does not build: %s", p.ImportPath, p.Error.Err)
		}
		g.recordImports(p.ImportPath, p.Deps)
	}
	build := newGoBuild(listed)
	g.inv.Gate.Box = boxConfig{Type: "example.com/box.Text", Field: "text"}
	g.inv.Gate.Reflection = []reflectionExempt{{File: "m/pkg/kv.go", Function: "example.com/m/pkg.readsDirect", GuardedPackage: "example.com/m/lib", Why: "test"}}
	var c census
	for _, p := range listed {
		if p.ImportPath == "example.com/m/pkg" {
			g.goPackage(&c, dir, p.ImportPath, build, false, nil, nil, nil)
		}
	}
	if len(c.Unknown) > 0 {
		t.Fatalf("the census could not read the module: %s", strings.Join(c.Unknown, "; "))
	}
	if len(c.Reflect) != 1 {
		t.Fatalf("the census listed %d reflection sites, want the one FieldByName: %+v", len(c.Reflect), c.Reflect)
	}
	return c.Reflect[0]
}

// TestAGuardedReadReadsAnotherPackagesEmbeddedStructFromItsExportData: the census type-checks the
// guarded package alone from its source (sourceChecked), each package it imports from that
// package's export data, and that export data keeps an exported struct's unexported fields: as the
// gate lists and reads a module, a lib type that embeds inner's struct, by value or by pointer,
// whose unexported useDirect is an interface is refused beside lib's kvs, which keeps a bool by
// that name. Control: where inner keeps a bool there the read passes over both of lib's struct
// types that hold the name, wrapped's through inner's export data, so the module was listed and read
// through to inner's field.
func TestAGuardedReadReadsAnotherPackagesEmbeddedStructFromItsExportData(t *testing.T) {
	for _, c := range []struct {
		name, innerType, embed string
		ok                     bool
		why                    string
	}{
		{"embedded by value", "any", "inner.S", false, "wrapped.useDirect (any), promoted through the embedded example.com/m/inner.S"},
		{"embedded by pointer", "any", "*inner.S", false, "wrapped.useDirect (any), promoted through the embedded *example.com/m/inner.S"},
		{"control: inner keeps a bool", "bool", "inner.S", true, "2 struct types example.com/m/lib declares"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := exportedGuardedCensus(t, c.innerType, c.embed)
			if got.OK != c.ok || !strings.Contains(got.Why, c.why) {
				t.Fatalf("the census answered ok=%t %q at %s:%d; want ok=%t and %q in its reason", got.OK, got.Why, got.File, got.Line, c.ok, c.why)
			}
		})
	}
}
