// Package startwalk finds every process start a program's process reaches, for the tests that
// hold a process which reaps children it does not wait for itself to starting each child of its
// own where its reaper leaves the exit to that child's wait: the pod's launcher, its container's
// PID 1 (internal/launcher's TestEveryChildTheLaunchersProcessWaitsForStartsWhereNoReapTakesIt),
// and `legion worker-shim`, the subreaper of what it starts under --reap-orphans (internal/shim's
// TestEveryChildTheShimWaitsForStartsThroughItsGuard). Only tests import it.
//
// The walk is variable type analysis over the program (golang.org/x/tools/go/callgraph/vta), with
// rules of its own for callbacks. A call the standard library makes through a function value or an
// interface is not followed, since what the standard library calls back is only what it was
// handed; instead, every function value, closure and method set that walked code outside the
// standard library hands out is walked as though it were called, and so is every function a store
// anywhere in the program writes into a package variable, or a field or element of one, that
// walked code reads (storedFunctions). Without the first rule a callback inside sync.Once or fmt
// reaches every function of the same signature in the program; without the second, a start handed
// to the standard library (a goroutine's body, a context.AfterFunc callback, an io.Writer given to
// fmt) would go unread.
package startwalk

import (
	"errors"
	"fmt"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/tools/go/callgraph"
	"golang.org/x/tools/go/callgraph/cha"
	"golang.org/x/tools/go/callgraph/vta"
	"golang.org/x/tools/go/packages"
	"golang.org/x/tools/go/ssa"
	"golang.org/x/tools/go/ssa/ssautil"
)

// processStarts are the standard library's ways to start a process. A call of one of them outside
// the standard library is a start this walk reads.
var processStarts = []string{
	"(*os/exec.Cmd).Start",
	"(*os/exec.Cmd).Run",
	"(*os/exec.Cmd).Output",
	"(*os/exec.Cmd).CombinedOutput",
	"os.StartProcess",
	"syscall.ForkExec",
	"syscall.StartProcess",
}

// Start is one call of a process start that the walked process reaches: the function that makes
// it, what it calls, where, and a path from an entry point to it.
type Start struct {
	Caller, Callee, Site, Path string
}

func (s Start) String() string { return s.Call() + " at " + s.Site }

// Call is the start's key in a test's table of held starts: the function that makes it and the
// start it calls.
func (s Start) Call() string { return s.Caller + " -> " + s.Callee }

// Root is an entry point of the walked process: a function, by name, of the package at Path.
type Root struct {
	Path, Func string
}

// Walk loads the packages of roots from dir with overlay (file contents in place of the files on
// disk), and answers every call outside the standard library of a process start the program
// reaches from roots, one per call site. A pod runs Linux built with no cgo, and role-init's
// namespaces and a shim's subreaper exist only there, so the walk reads that build whatever the
// host builds for.
func Walk(dir string, overlay map[string][]byte, roots ...Root) ([]Start, error) {
	cfg := &packages.Config{Mode: packages.LoadAllSyntax, Dir: dir, Overlay: overlay,
		Env: append(os.Environ(), "GOOS=linux", "CGO_ENABLED=0")}
	var paths []string
	for _, root := range roots {
		if !slices.Contains(paths, root.Path) {
			paths = append(paths, root.Path)
		}
	}
	pkgs, err := packages.Load(cfg, paths...)
	if err != nil {
		return nil, err
	}
	if packages.PrintErrors(pkgs) > 0 {
		return nil, errors.New("the walked packages did not load")
	}
	prog, ssaPackages := ssautil.AllPackages(pkgs, ssa.InstantiateGenerics)
	prog.Build()
	var entries []*ssa.Function
	for _, root := range roots {
		index := slices.IndexFunc(pkgs, func(p *packages.Package) bool { return p.PkgPath == root.Path })
		var entry *ssa.Function
		if index >= 0 && ssaPackages[index] != nil {
			entry = ssaPackages[index].Func(root.Func)
		}
		if entry == nil {
			return nil, fmt.Errorf("%s holds no %s, an entry point of the walked process", root.Path, root.Func)
		}
		entries = append(entries, entry)
	}
	functions := ssautil.AllFunctions(prog)
	graph := vta.CallGraph(functions, cha.CallGraph(prog))

	w := walk{prog: prog, graph: graph, stored: storedFunctions(functions),
		parent: map[*ssa.Function]*ssa.Function{}, seen: map[*ssa.Function]bool{}}
	for _, entry := range entries {
		w.visit(entry, nil)
	}
	var starts []Start
	for len(w.queue) > 0 {
		fn := w.queue[0]
		w.queue = w.queue[1:]
		std := standardLibrary(fn)
		if node := graph.Nodes[fn]; node != nil {
			for _, edge := range node.Out {
				callee := edge.Callee.Func
				if !std && isProcessStart(callee) {
					starts = append(starts, Start{
						Caller: fn.String(), Callee: callee.String(),
						Site: siteOf(prog.Fset, edge.Pos()), Path: w.pathTo(fn),
					})
				}
				if std && edge.Site != nil && edge.Site.Common().StaticCallee() == nil {
					continue
				}
				w.visit(callee, fn)
			}
		}
		if !std {
			w.handedOut(fn)
		}
	}
	starts = append(starts, w.handed...)
	slices.SortFunc(starts, func(a, b Start) int { return strings.Compare(a.String(), b.String()) })
	return slices.CompactFunc(starts, func(a, b Start) bool { return a.String() == b.String() }), nil
}

// Plant replaces, in overlay's copy of file (a path from the test's package directory; the file
// on disk when the overlay holds none yet), the first anchor with replacement, or appends
// replacement when anchor is empty, and imports each of imports where the file does not. A
// test's control plants the starts its walk must name with it.
func Plant(t testing.TB, overlay map[string][]byte, file, anchor, replacement string, imports ...string) {
	t.Helper()
	path, err := filepath.Abs(file)
	if err != nil {
		t.Fatal(err)
	}
	source, planted := overlay[path]
	if !planted {
		if source, err = os.ReadFile(file); err != nil {
			t.Fatal(err)
		}
	}
	text := string(source)
	switch {
	case anchor == "":
		text += replacement
	case strings.Contains(text, anchor):
		text = strings.Replace(text, anchor, replacement, 1)
	default:
		t.Fatalf("%s no longer holds %q, where the control plants its start", file, anchor)
	}
	for _, imported := range imports {
		if !strings.Contains(text, strconv.Quote(imported)) {
			text = strings.Replace(text, "import (\n", "import (\n\t"+strconv.Quote(imported)+"\n", 1)
		}
	}
	overlay[path] = []byte(text)
}

// storedFunctions is every function value and closure that a Store anywhere in the program writes
// directly into a package variable, or into a field or element of one (a package variable's
// initializer is such a store, in its package's init), by that variable. A function that reaches a
// package variable another way is not here: in a map or struct literal, through a pointer to the
// variable, inside a struct stored whole, put into an interface first, or sent on a channel.
func storedFunctions(functions map[*ssa.Function]bool) map[*ssa.Global][]*ssa.Function {
	stored := map[*ssa.Global][]*ssa.Function{}
	for fn := range functions {
		for _, block := range fn.Blocks {
			for _, instruction := range block.Instrs {
				store, ok := instruction.(*ssa.Store)
				if !ok {
					continue
				}
				global := globalUnder(store.Addr)
				if global == nil {
					continue
				}
				switch value := store.Val.(type) {
				case *ssa.Function:
					stored[global] = append(stored[global], value)
				case *ssa.MakeClosure:
					stored[global] = append(stored[global], value.Fn.(*ssa.Function))
				}
			}
		}
	}
	return stored
}

// globalUnder is the package variable address names, directly or through fields and elements of
// it, and nil for any other address.
func globalUnder(address ssa.Value) *ssa.Global {
	for {
		switch value := address.(type) {
		case *ssa.Global:
			return value
		case *ssa.FieldAddr:
			address = value.X
		case *ssa.IndexAddr:
			address = value.X
		default:
			return nil
		}
	}
}

// walk is the state of one walk of a process: the functions it has reached, in the order it
// reached them, and for each the function it was reached from.
type walk struct {
	prog   *ssa.Program
	graph  *callgraph.Graph
	stored map[*ssa.Global][]*ssa.Function
	parent map[*ssa.Function]*ssa.Function
	seen   map[*ssa.Function]bool
	queue  []*ssa.Function
	// handed is every process start walked code hands out as a value (walk.value).
	handed []Start
}

func (w *walk) visit(fn, from *ssa.Function) {
	if fn == nil || w.seen[fn] {
		return
	}
	w.seen[fn] = true
	w.parent[fn] = from
	w.queue = append(w.queue, fn)
}

// value walks v, a function fn hands out as a value at pos, and records it as a start of fn's
// when it is one: a bound method value (cmd.Run, whose call is $bound's) or a method expression
// ($thunk), which whoever is handed it may call where the walk does not follow, the standard
// library among them.
func (w *walk) value(v, fn *ssa.Function, pos token.Pos) {
	if isProcessStart(v) {
		w.handed = append(w.handed, Start{
			Caller: fn.String(), Callee: v.String(), Site: "handed out at " + siteOf(w.prog.Fset, pos), Path: w.pathTo(fn),
		})
	}
	w.visit(v, fn)
}

// isProcessStart is whether fn is one of processStarts, or a synthetic wrapper of one: its bound
// method value ($bound), its method expression ($thunk), or its promotion through an embedded
// field.
func isProcessStart(fn *ssa.Function) bool {
	name := fn.String()
	if fn.Synthetic != "" {
		if method, ok := fn.Object().(*types.Func); ok {
			name = method.FullName()
		}
	}
	return slices.Contains(processStarts, name)
}

// handedOut walks what fn hands out: every function it uses as a value, every closure it makes,
// the methods of every type it puts into an interface, and every function stored into a package
// variable it reads (storedFunctions). A function or closure handed out that is a process start is
// recorded as one of fn's (walk.value), a closure's function being among its instruction's
// operands; an interface's methods are only walked, since putting a *exec.Cmd into an interface
// (fmt.Sprint(cmd), an error's %s) starts nothing, and a start called through the interface is a
// call edge the walk reads (isProcessStart resolves a promoted wrapper).
func (w *walk) handedOut(fn *ssa.Function) {
	for _, block := range fn.Blocks {
		for _, instruction := range block.Instrs {
			if value, ok := instruction.(*ssa.MakeInterface); ok {
				methods := w.prog.MethodSets.MethodSet(value.X.Type())
				for i := range methods.Len() {
					w.visit(w.prog.MethodValue(methods.At(i)), fn)
				}
			}
			// A function in the callee place of a static call is that call, not a value handed
			// out; every other function among the operands is one.
			var operands []*ssa.Value
			if call, ok := instruction.(ssa.CallInstruction); ok && call.Common().StaticCallee() != nil {
				for i := range call.Common().Args {
					operands = append(operands, &call.Common().Args[i])
				}
			} else {
				operands = instruction.Operands(nil)
			}
			for _, operand := range operands {
				if operand == nil || *operand == nil {
					continue
				}
				switch value := (*operand).(type) {
				case *ssa.Function:
					w.value(value, fn, instruction.Pos())
				case *ssa.Global:
					for _, stored := range w.stored[value] {
						w.value(stored, fn, instruction.Pos())
					}
				}
			}
		}
	}
}

// pathTo is the chain of functions from an entry point to fn, first to last.
func (w *walk) pathTo(fn *ssa.Function) string {
	var chain []string
	for current := fn; current != nil; current = w.parent[current] {
		chain = append(chain, current.String())
	}
	slices.Reverse(chain)
	return strings.Join(chain, " -> ")
}

// standardLibrary is whether fn is the standard library's: its package path's first element has no
// dot, as every module path's does. A synthetic wrapper is its method's.
func standardLibrary(fn *ssa.Function) bool {
	for fn.Pkg == nil && fn.Origin() != nil && fn.Origin() != fn {
		fn = fn.Origin()
	}
	var pkg *types.Package
	switch {
	case fn.Pkg != nil:
		pkg = fn.Pkg.Pkg
	case fn.Object() != nil:
		pkg = fn.Object().Pkg()
	}
	if pkg == nil {
		return false
	}
	first, _, _ := strings.Cut(pkg.Path(), "/")
	return !strings.Contains(first, ".")
}

// siteOf is where pos is, as a path relative to the module, a line and a column, so two starts on
// one line are two call sites.
func siteOf(fset *token.FileSet, pos token.Pos) string {
	position := fset.Position(pos)
	file := position.Filename
	if i := strings.Index(file, "/packages/daemon/"); i >= 0 {
		file = file[i+len("/packages/daemon/"):]
	}
	return file + ":" + strconv.Itoa(position.Line) + ":" + strconv.Itoa(position.Column)
}
