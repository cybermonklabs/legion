package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fieldsModule is a module whose package box keeps its text in an unexported field, as the box
// does, and whose package pkg reads that text through reflection with READ.
var fieldsModule = map[string]string{
	"go.mod":     "module example.com/m\n\ngo 1.27\n",
	"box/box.go": "package box\n\ntype Text struct{ text *string }\n",
	"pkg/pkg.go": "package pkg\n\nimport (\n\t\"reflect\"\n\n\t\"example.com/m/box\"\n)\n\nfunc reads(t box.Text) string {\n\tREAD\n}\n",
}

// reflectionCensus writes fieldsModule, its read read, to a scratch directory, lists pkg with its
// export data, and answers the reflection census's findings over pkg (goPackage), with the
// toolchain's own reflect and no inventory entry.
func reflectionCensus(t *testing.T, read string) []finding {
	t.Helper()
	t.Setenv("GOWORK", "off")
	t.Setenv("GOFLAGS", "")
	dir := t.TempDir()
	for name, text := range fieldsModule {
		path := filepath.Join(dir, "m", name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(strings.ReplaceAll(text, "READ", read)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	listed, err := goList(filepath.Join(dir, "m"), hostBuild(), true, "./pkg")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range listed {
		if p.Error != nil {
			t.Fatalf("%s does not build: %s", p.ImportPath, p.Error.Err)
		}
	}
	g := &gate{}
	g.inv.Gate.Box = boxConfig{Type: "example.com/m/box.Text", Field: "text"}
	var c census
	g.goPackage(&c, dir, "example.com/m/pkg", newGoBuild(listed), false, nil, nil, nil)
	if len(c.Unknown) > 0 {
		t.Fatalf("the census could not read the module: %s", strings.Join(c.Unknown, "; "))
	}
	return c.Reflect
}

// TestTheReflectionCensusListsAReadThroughFields: go 1.27's reflect.Value.Fields iterates over a
// struct's fields as Field does, unexported ones among them, so a read of the box's text through
// it is struct-field access the reflection census lists and, with no inventory entry, refuses, as
// it refuses the same read through Field (the control). Every method of the toolchain's
// reflect.Value named Field... is in the census's list, so a toolchain that adds another fails here.
func TestTheReflectionCensusListsAReadThroughFields(t *testing.T) {
	for _, c := range []struct{ name, read, what string }{
		{"Fields", "for _, field := range reflect.ValueOf(t).Fields() {\n\t\treturn field.Elem().String()\n\t}\n\treturn \"\"", "(reflect.Value).Fields"},
		{"control: Field", "return reflect.ValueOf(t).Field(0).Elem().String()", "(reflect.Value).Field"},
	} {
		t.Run(c.name, func(t *testing.T) {
			found := reflectionCensus(t, c.read)
			if len(found) != 1 || found[0].What != c.what || found[0].OK || !strings.Contains(found[0].Why, "no inventory entry names this site") {
				t.Fatalf("the reflection census listed %+v; want the one read through %s, refused with no inventory entry", found, c.what)
			}
		})
	}
	for name := range valueMethods(t) {
		if strings.HasPrefix(name, "Field") && !reflectionMethods["(reflect.Value)."+name] {
			t.Errorf("reflect.Value.%s reaches a struct's fields, and the reflection census does not list it", name)
		}
	}
}
