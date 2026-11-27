package main

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// orderModule is a module whose boundary, p.Entry, calls six functions of its package, each of
// which sets a field.
var orderModule = map[string]string{
	"go.mod": "module example.com/m\n\ngo 1.27\n",
	"p/p.go": `package p

type T struct{ A string }

func Entry() []T { return []T{f(), b(), e(), a(), d(), c()} }

func a() T { return T{A: "a"} }
func b() T { return T{A: "b"} }
func c() T { return T{A: "c"} }
func d() T { return T{A: "d"} }
func e() T { return T{A: "e"} }
func f() T { return T{A: "f"} }
`,
}

// TestTheFieldCensusListsItsFindingsInOneOrder: the field census lists the fields the boundary's
// functions set function by function, in the order of the functions' names, so two runs over one
// tree print the same lines in the same order. Its scope is a set, and ten runs over orderModule,
// each a fresh census, list the same twelve fields (Entry's six elements, and each function's A),
// sorted by function, each time.
func TestTheFieldCensusListsItsFindingsInOneOrder(t *testing.T) {
	t.Setenv("GOWORK", "off")
	t.Setenv("GOFLAGS", "")
	dir := t.TempDir()
	writeTree(t, filepath.Join(dir, "m"), orderModule)
	listed, err := goList(filepath.Join(dir, "m"), hostBuild(), true, "./p")
	if err != nil {
		t.Fatal(err)
	}
	build := newGoBuild(listed)
	var first []string
	for run := range 10 {
		g := &gate{}
		g.inv.Gate.Box = boxConfig{Type: "example.com/box.Text", Field: "text"}
		g.inv.Gate.BoundaryFields.Entry = []string{"example.com/m/p.Entry"}
		var c census
		g.goPackage(&c, dir, "example.com/m/p", build, false, nil, nil, nil)
		if len(c.Unknown) > 0 {
			t.Fatalf("the census could not read the module: %s", strings.Join(c.Unknown, "; "))
		}
		var functions, lines []string
		for _, f := range c.Fields {
			functions = append(functions, f.Function)
			lines = append(lines, f.Function+": "+f.What)
		}
		if len(c.Fields) != 12 || !slices.IsSorted(functions) {
			t.Fatalf("run %d: the field census listed\n%s\nwant Entry's six elements and each function's A, sorted by function", run, strings.Join(lines, "\n"))
		}
		if first == nil {
			first = lines
		} else if !slices.Equal(lines, first) {
			t.Fatalf("run %d listed\n%s\nand the first run\n%s", run, strings.Join(lines, "\n"), strings.Join(first, "\n"))
		}
	}
}
