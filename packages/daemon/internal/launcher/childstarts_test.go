// The walk loads the launcher and every package it imports from source and builds a call graph
// over the whole program, twice: without the race detector the test took 24 s at a 1.95 GB peak on
// a development host and 66 s at 2.34 GB in a pod, and the detector multiplies both. So the race
// build leaves this file out, and daemon-go's plain step for what the race build cannot run runs
// its test by name (.github/scripts/ci_job_reads.py).

//go:build !race

package launcher

import (
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
// the standard library is a start this check reads.
var processStarts = []string{
	"(*os/exec.Cmd).Start",
	"(*os/exec.Cmd).Run",
	"(*os/exec.Cmd).Output",
	"(*os/exec.Cmd).CombinedOutput",
	"os.StartProcess",
	"syscall.ForkExec",
	"syscall.StartProcess",
}

// launcherPackage is this package's import path, the one the launcher's process entry points are in.
const launcherPackage = "github.com/sjawhar/legion/daemon/internal/launcher"

// heldStart is one call of a process start the launcher's process may reach: how many call sites
// make it, and how its child is held from every reap of that process (the orphan reaper's passes,
// pidGuard.reapOrphans, and the controller pod's drain, pidGuard.reap), or why it never runs there.
type heldStart struct {
	sites int
	why   string
}

// heldStarts are those calls, each keyed by the function that makes it and the start it calls
// (processStart.call). Any other start the walk reaches is one the reaper can take the exit of,
// which fails its own wait with `waitid: no child processes` (or under gVisor at times `wait: no
// child processes` or `waitid: no such process`). A count that changes fails too, in either
// direction, so a dependency that adds a start to a function already here, or a function here that
// no longer starts anything, is read again rather than let through on the old reason.
var heldStarts = map[string]heldStart{
	"(*" + launcherPackage + ".pidGuard).start -> (*os/exec.Cmd).Start":    {1, "the guard itself: it records the child's pid under the lock every reap holds"},
	"(*" + launcherPackage + ".roleProcess).start -> (*os/exec.Cmd).Start": {1, "a generation: its manager records the pid under its own lock as it starts it, and the orphan reaper skips every manager's child"},
	launcherPackage + ".runRoleShim -> (*os/exec.Cmd).Start":               {1, "role-init's shim, in role-init's own process: role-init's reaper skips its pid"},
	// The session proxy's Cloud SQL connector reaches Google's credential and client-certificate
	// code, which can start a helper the machine's own configuration names. A pod names none: it
	// signs in as its own identity from the metadata server.
	"(*cloud.google.com/go/auth/internal/transport/cert.secureConnectSource).getClientCertificate -> (*os/exec.Cmd).Output": {1, "SecureConnect's certificate command, which runs only where ~/.secureConnect/context_aware_metadata.json names one; no pod holds that file"},
	"(*google.golang.org/api/internal/cert.secureConnectSource).getClientCertificate -> (*os/exec.Cmd).Output":              {1, "SecureConnect's certificate command, which runs only where ~/.secureConnect/context_aware_metadata.json names one; no pod holds that file"},
	"github.com/googleapis/enterprise-certificate-proxy/client.Cred -> (*os/exec.Cmd).Start":                                {1, "the enterprise certificate signer, which runs only where gcloud's certificate_config.json names one; no pod holds that file"},
	"(cloud.google.com/go/auth/credentials/internal/externalaccount.runtimeEnvironment).run -> (*os/exec.Cmd).Run":          {1, "an external-account credential's executable, which runs only for such a credentials file and GOOGLE_EXTERNAL_ACCOUNT_ALLOW_EXECUTABLES=1; a pod has neither"},
	"(golang.org/x/oauth2/google/externalaccount.runtimeEnvironment).run -> (*os/exec.Cmd).Run":                             {1, "an external-account credential's executable, which runs only for such a credentials file and GOOGLE_EXTERNAL_ACCOUNT_ALLOW_EXECUTABLES=1; a pod has neither"},
}

// The launcher's process — `legion launcher`, and `legion launcher role-init` in a role's
// namespaces — is its PID namespace's PID 1, which reaps every exited child it does not hold, so
// every child that process starts and waits for itself must start where no reap takes it: through
// pidGuard (a procgroup.Starter of the guard's), or as a generation its manager records. The
// leak-check install's run of the check once started with a bare cmd.Run, and the reaper took its
// exit before the install's own wait could; the put-back's jj and git had done the same before it.
// This check walks the call graph from launcher.Run and launcher.RoleInit and fails on every start
// it reaches that heldStarts does not hold, naming the call site and a path to it. A start through
// procgroup.NoReaper counts as such a start: it is where the walk ends up when code the launcher
// reaches is handed the start of a process that does not reap.
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
//
// It cannot pass by reading nothing, nor with a rule of its own broken: its control plants starts
// the launcher would reach, and must see each named (plantedStarts): a bare cmd.Run in
// newCheckInstall; one in a package variable's function that newCheckInstall hands to
// context.AfterFunc; procgroup.NoReaper given to the workspace restore in place of the guard's
// starter; the install's run of the check as it was before the guard took it (a bare cmd.Run in
// leakguard.verifyServed); a closure only sync.Once.Do calls; a start made through each other
// shape Go gives it: a method value called, a method value handed to a helper, to errgroup.Go and
// to sync.OnceValue, a method expression, and a method promoted from an embedded *exec.Cmd and
// called through an interface; a writer only fmt calls; a method value stored in a package
// variable; and functions an init stores into a package variable's field and element. And a
// command only formatted, which starts nothing, must stay unnamed.
func TestEveryChildTheLaunchersProcessWaitsForStartsWhereNoReapTakesIt(t *testing.T) {
	t.Run("this tree", func(t *testing.T) {
		sites := map[string][]processStart{}
		for _, start := range launcherProcessStarts(t, nil) {
			sites[start.call()] = append(sites[start.call()], start)
		}
		for call, starts := range sites {
			held, ok := heldStarts[call]
			switch {
			case !ok:
				for _, start := range starts {
					t.Errorf("%s starts a process outside the guard (%s at %s), so PID 1's reaper can take its exit; start it through pidGuard (a procgroup.Starter of the guard's), or add it to heldStarts with why no reap takes it. Reached by: %s",
						start.caller, start.callee, start.site, start.path)
				}
			case len(starts) != held.sites:
				t.Errorf("heldStarts allows %s at %d call sites, and the walk reaches %d: %v; read them again and set the count", call, held.sites, len(starts), starts)
			}
		}
		for call := range heldStarts {
			if len(sites[call]) == 0 {
				t.Errorf("heldStarts allows %s, which the walk no longer reaches. Remove it only if the code that started it is gone: if that code is still there, the walk has gone blind to it, and the control subtest names the rule that broke", call)
			}
		}
	})
	t.Run("planted starts are named", func(t *testing.T) {
		overlay := map[string][]byte{}
		plant(t, overlay, "leakcheck.go", "\tif dir == \"\" {\n\t\tdir = leakguard.InstallDir\n\t}\n",
			"\tif dir == \"\" {\n\t\tdir = leakguard.InstallDir\n\t}\n"+
				"\t_ = exec.Command(\"/bin/true\").Run()\n"+
				"\t_ = context.AfterFunc(context.Background(), plantedCallback)\n"+
				"\tplantedStarts()\n")
		plant(t, overlay, "leakcheck.go", "", "\nvar plantedCallback = func() { _ = exec.Command(\"/bin/false\").Run() }\n")
		plant(t, overlay, "launcher.go", "restore := newBundleRestore(guard.starter())",
			"restore := newBundleRestore(procgroup.NoReaper)")
		// The defect this check came from: the install's run of the check with a bare cmd.Run, in
		// another package the launcher calls.
		plant(t, overlay, "../leakguard/install.go", "procgroup.Err(procgroup.Run(cmd, start))",
			"procgroup.Err(cmd.Run())")
		planted, err := filepath.Abs("planted_starts.go")
		if err != nil {
			t.Fatal(err)
		}
		overlay[planted] = []byte(plantedStartsSource)
		starts := launcherProcessStarts(t, overlay)
		for _, want := range []struct{ what, caller string }{
			{"the bare cmd.Run in newCheckInstall", launcherPackage + ".newCheckInstall"},
			{"the package variable's start handed to context.AfterFunc", launcherPackage + ".init$"},
			{"procgroup.NoReaper in place of the guard's starter", "github.com/sjawhar/legion/daemon/internal/procgroup.NoReaper"},
			{"the install's run as it was before the guard", "github.com/sjawhar/legion/daemon/internal/leakguard.verifyServed"},
			{"a closure only sync.Once.Do calls", launcherPackage + ".plantedClosure$1"},
			{"a method value called", launcherPackage + ".plantedMethodValue"},
			{"a method value handed to a helper", launcherPackage + ".plantedHelperValue"},
			{"a method value handed to errgroup.Go", launcherPackage + ".plantedErrgroup"},
			{"a method value handed to sync.OnceValue", launcherPackage + ".plantedOnceValue"},
			{"a method expression", launcherPackage + ".plantedMethodExpression"},
			{"a method promoted from an embedded *exec.Cmd, called through an interface", launcherPackage + ".plantedPromoted"},
			{"a writer handed to fmt.Fprint as an io.Writer", "(" + launcherPackage + ".plantedWriter).Write"},
			{"a method value stored in a package variable", launcherPackage + ".plantedStoredBound"},
			{"a function stored into a package variable's field in an init", launcherPackage + ".plantedFieldStart"},
			{"a function stored into a package variable's element in an init", launcherPackage + ".plantedElementStart"},
		} {
			// Every start the walk answers is a process start, a promoted method's wrapper
			// included, whose own name is its receiver's, so a row is matched by its caller.
			named := slices.ContainsFunc(starts, func(start processStart) bool {
				return start.caller == want.caller || strings.HasSuffix(want.caller, "$") && strings.HasPrefix(start.caller, want.caller)
			})
			if !named {
				t.Errorf("the check did not name %s, so it could pass over a real one; it named: %v", want.what, starts)
			}
		}
		// Formatting a command puts it into an interface and starts nothing, so it must not be
		// named, or the gate would go red on a log line.
		for _, start := range starts {
			if start.caller == launcherPackage+".plantedFormatted" {
				t.Errorf("the check named %v, a command only formatted, which starts nothing", start)
			}
		}
	})
}

// plantedStartsSource is the file the control adds to the launcher's package: one function per
// shape a start can take beyond a plain call, each called from plantedStarts, which the control
// calls from newCheckInstall.
const plantedStartsSource = `package launcher

import (
	"fmt"
	"os/exec"
	"sync"

	"golang.org/x/sync/errgroup"
)

func plantedStarts() {
	plantedClosure()
	plantedMethodValue()
	plantedHelperValue()
	plantedErrgroup()
	plantedOnceValue()
	plantedMethodExpression()
	plantedPromoted()
	plantedFormatted()
	plantedInterfaceHanded()
	plantedStoredBound()
	plantedFieldStored()
}

func plantedFormatted() { _ = fmt.Sprint(exec.Command("/bin/true")) }

func plantedClosure() {
	var once sync.Once
	once.Do(func() { _ = exec.Command("/bin/true").Run() })
}

func plantedMethodValue() {
	run := exec.Command("/bin/true").Run
	_ = run()
}

func plantedHelperValue() { plantedRun(exec.Command("/bin/true").Run) }

func plantedRun(run func() error) { _ = run() }

func plantedErrgroup() {
	var group errgroup.Group
	group.Go(exec.Command("/bin/true").Run)
	_ = group.Wait()
}

func plantedOnceValue() { _ = sync.OnceValue(exec.Command("/bin/true").Run)() }

func plantedMethodExpression() {
	run := (*exec.Cmd).Run
	_ = run(exec.Command("/bin/true"))
}

type plantedEmbedded struct{ *exec.Cmd }

func plantedPromoted() {
	var runner interface{ Run() error } = plantedEmbedded{exec.Command("/bin/true")}
	_ = runner.Run()
}

type plantedWriter struct{}

func (plantedWriter) Write(p []byte) (int, error) {
	_ = exec.Command("/bin/true").Run()
	return len(p), nil
}

func plantedInterfaceHanded() { fmt.Fprint(plantedWriter{}, "x") }

var plantedBound = exec.Command("/bin/true").Run

func plantedStoredBound() { _ = sync.OnceValue(plantedBound)() }

// plantedMarker is the result type of the two functions an init stores below: a func() would be
// among the callees variable type analysis gives some dependency's own dynamic call of a func()
// (gRPC's, which the launcher reaches), and the walk would reach it that way with the store rule
// removed.
type plantedMarker struct{}

var plantedTable struct{ onStart func() plantedMarker }

var plantedArray [1]func() plantedMarker

// Named rather than literal, so their names do not depend on how many init functions the package
// has; each is stored by the init alone and reached only through the store.
func plantedFieldStart() plantedMarker { _ = exec.Command("/bin/true").Run(); return plantedMarker{} }

func plantedElementStart() plantedMarker { _ = exec.Command("/bin/false").Run(); return plantedMarker{} }

func init() {
	plantedTable.onStart = plantedFieldStart
	plantedArray[0] = plantedElementStart
}

func plantedFieldStored() {
	_ = sync.OnceValue(plantedTable.onStart)()
	_ = sync.OnceValue(plantedArray[0])()
}
`

// plant replaces, in overlay's copy of file (a path from this package's directory; the file on
// disk when the overlay holds none yet), the first anchor with replacement, or appends
// replacement when anchor is empty, and imports os/exec and procgroup where the file does not.
func plant(t *testing.T, overlay map[string][]byte, file, anchor, replacement string) {
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
	for _, imported := range []string{"os/exec", "github.com/sjawhar/legion/daemon/internal/procgroup"} {
		if !strings.Contains(text, strconv.Quote(imported)) {
			text = strings.Replace(text, "import (\n", "import (\n\t"+strconv.Quote(imported)+"\n", 1)
		}
	}
	overlay[path] = []byte(text)
}

// processStart is one call of a process start that the launcher's process reaches: the function
// that makes it, what it calls, where, and a path from an entry point to it.
type processStart struct {
	caller, callee, site, path string
}

func (s processStart) String() string { return s.call() + " at " + s.site }

// call is the start's key in heldStarts: the function that makes it and the start it calls.
func (s processStart) call() string { return s.caller + " -> " + s.callee }

// launcherProcessStarts walks the launcher's process from launcher.Run and launcher.RoleInit, with
// overlay (file contents in place of the files on disk), and answers every call outside the
// standard library of a processStarts function it reaches, one per call site.
func launcherProcessStarts(t *testing.T, overlay map[string][]byte) []processStart {
	t.Helper()
	// A pod runs Linux built with no cgo, and role-init's namespaces exist only there
	// (roleinit_linux.go), so the walk reads that build whatever the host builds for.
	cfg := &packages.Config{Mode: packages.LoadAllSyntax, Dir: ".", Overlay: overlay,
		Env: append(os.Environ(), "GOOS=linux", "CGO_ENABLED=0")}
	pkgs, err := packages.Load(cfg, launcherPackage)
	if err != nil {
		t.Fatal(err)
	}
	if packages.PrintErrors(pkgs) > 0 {
		t.Fatal("the launcher's packages did not load")
	}
	prog, ssaPackages := ssautil.AllPackages(pkgs, ssa.InstantiateGenerics)
	prog.Build()
	var roots []*ssa.Function
	for _, name := range []string{"Run", "RoleInit"} {
		root := ssaPackages[0].Func(name)
		if root == nil {
			t.Fatalf("%s holds no %s, an entry point of the launcher's process", launcherPackage, name)
		}
		roots = append(roots, root)
	}
	functions := ssautil.AllFunctions(prog)
	graph := vta.CallGraph(functions, cha.CallGraph(prog))

	w := walk{prog: prog, graph: graph, stored: storedFunctions(functions),
		parent: map[*ssa.Function]*ssa.Function{}, seen: map[*ssa.Function]bool{}}
	for _, root := range roots {
		w.visit(root, nil)
	}
	var starts []processStart
	for len(w.queue) > 0 {
		fn := w.queue[0]
		w.queue = w.queue[1:]
		std := standardLibrary(fn)
		if node := graph.Nodes[fn]; node != nil {
			for _, edge := range node.Out {
				callee := edge.Callee.Func
				if !std && isProcessStart(callee) {
					starts = append(starts, processStart{
						caller: fn.String(), callee: callee.String(),
						site: siteOf(prog.Fset, edge.Pos()), path: w.pathTo(fn),
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
	slices.SortFunc(starts, func(a, b processStart) int { return strings.Compare(a.String(), b.String()) })
	return slices.CompactFunc(starts, func(a, b processStart) bool { return a.String() == b.String() })
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

// walk is the state of one walk of the launcher's process: the functions it has reached, in the
// order it reached them, and for each the function it was reached from.
type walk struct {
	prog   *ssa.Program
	graph  *callgraph.Graph
	stored map[*ssa.Global][]*ssa.Function
	parent map[*ssa.Function]*ssa.Function
	seen   map[*ssa.Function]bool
	queue  []*ssa.Function
	// handed is every process start walked code hands out as a value (walk.value).
	handed []processStart
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
		w.handed = append(w.handed, processStart{
			caller: fn.String(), callee: v.String(), site: "handed out at " + siteOf(w.prog.Fset, pos), path: w.pathTo(fn),
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
