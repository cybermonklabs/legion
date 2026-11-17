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
// make it, and how its child is held from every reap of that process (the orphan reaper's passes
// and the controller pod's drain, pidGuard.reap), or why it never runs there.
type heldStart struct {
	sites int
	why   string
}

// heldStarts are those calls, each keyed by the function that makes it and the start it calls
// (processStart.call). Any other start the walk reaches is one the reaper can take the exit of,
// which fails its own wait with `waitid: no child processes` (or under gVisor `no such process`).
// A count that changes fails too, in either direction, so a dependency that adds a start to a
// function already here, or a function here that no longer starts anything, is read again rather
// than let through on the old reason.
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
// standard library hands out is walked as though it were called, and so is every function stored,
// anywhere in the program, into a package variable walked code reads. Without the first rule a
// callback inside sync.Once or fmt reaches every function of the same signature in the program;
// without the second, a start handed to the standard library (a goroutine's body, a
// context.AfterFunc callback, an http.Handler) would go unread.
//
// It cannot pass by reading nothing: its control plants four starts the launcher would reach, and
// must see each named: a bare cmd.Run in newCheckInstall; one in a package variable's function that
// newCheckInstall hands to context.AfterFunc; procgroup.NoReaper given to the workspace restore in
// place of the guard's starter; and the install's run of the check as it was before the guard took
// it (a bare cmd.Run in leakguard.verifyServed).
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
				t.Errorf("heldStarts allows %s, which the launcher's process no longer reaches: remove it", call)
			}
		}
	})
	t.Run("planted starts are named", func(t *testing.T) {
		overlay := map[string][]byte{}
		plant(t, overlay, "leakcheck.go", "\tif dir == \"\" {\n\t\tdir = leakguard.InstallDir\n\t}\n",
			"\tif dir == \"\" {\n\t\tdir = leakguard.InstallDir\n\t}\n"+
				"\t_ = exec.Command(\"/bin/true\").Run()\n"+
				"\t_ = context.AfterFunc(context.Background(), plantedCallback)\n")
		plant(t, overlay, "leakcheck.go", "", "\nvar plantedCallback = func() { _ = exec.Command(\"/bin/false\").Run() }\n")
		plant(t, overlay, "launcher.go", "restore := newBundleRestore(guard.starter())",
			"restore := newBundleRestore(procgroup.NoReaper)")
		// The defect this check came from: the install's run of the check with a bare cmd.Run, in
		// another package the launcher calls.
		plant(t, overlay, "../leakguard/install.go", "procgroup.Err(procgroup.Run(cmd, start))",
			"procgroup.Err(cmd.Run())")
		starts := launcherProcessStarts(t, overlay)
		for _, want := range []struct {
			what           string
			caller, callee string
		}{
			{"the bare cmd.Run in newCheckInstall", launcherPackage + ".newCheckInstall", "(*os/exec.Cmd).Run"},
			{"the package variable's start handed to context.AfterFunc", launcherPackage + ".init$", "(*os/exec.Cmd).Run"},
			{"procgroup.NoReaper in place of the guard's starter", "github.com/sjawhar/legion/daemon/internal/procgroup.NoReaper", "(*os/exec.Cmd).Start"},
			{"the install's run as it was before the guard", "github.com/sjawhar/legion/daemon/internal/leakguard.verifyServed", "(*os/exec.Cmd).Run"},
		} {
			named := slices.ContainsFunc(starts, func(start processStart) bool {
				return strings.HasPrefix(start.caller, want.caller) && start.callee == want.callee
			})
			if !named {
				t.Errorf("the check did not name %s, so it could pass over a real one; it named: %v", want.what, starts)
			}
		}
	})
}

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
	// A pod runs Linux, and role-init's namespaces exist only there (roleinit_linux.go), so the
	// walk reads the Linux build whatever the host builds for.
	cfg := &packages.Config{Mode: packages.LoadAllSyntax, Dir: ".", Overlay: overlay, Env: append(os.Environ(), "GOOS=linux")}
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
				if !std && slices.Contains(processStarts, callee.String()) {
					starts = append(starts, processStart{
						caller: fn.String(), callee: callee.String(),
						site: siteOf(prog.Fset, edge), path: w.pathTo(fn),
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
	slices.SortFunc(starts, func(a, b processStart) int { return strings.Compare(a.String(), b.String()) })
	return slices.CompactFunc(starts, func(a, b processStart) bool { return a.String() == b.String() })
}

// storedFunctions is every function value stored, anywhere in the program, into a package
// variable or a field or element of one (a package variable's initializer is such a store, in its
// package's init), by that variable.
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
}

func (w *walk) visit(fn, from *ssa.Function) {
	if fn == nil || w.seen[fn] {
		return
	}
	w.seen[fn] = true
	w.parent[fn] = from
	w.queue = append(w.queue, fn)
}

// handedOut walks what fn hands out: every function it uses as a value, every closure it makes,
// the methods of every type it puts into an interface, and every function stored into a package
// variable it reads (storedFunctions).
func (w *walk) handedOut(fn *ssa.Function) {
	for _, block := range fn.Blocks {
		for _, instruction := range block.Instrs {
			switch value := instruction.(type) {
			case *ssa.MakeClosure:
				w.visit(value.Fn.(*ssa.Function), fn)
			case *ssa.MakeInterface:
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
					w.visit(value, fn)
				case *ssa.Global:
					for _, stored := range w.stored[value] {
						w.visit(stored, fn)
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

// siteOf is where edge's call is, as a path relative to the module and a line.
func siteOf(fset *token.FileSet, edge *callgraph.Edge) string {
	position := fset.Position(edge.Pos())
	file := position.Filename
	if i := strings.Index(file, "/packages/daemon/"); i >= 0 {
		file = file[i+len("/packages/daemon/"):]
	}
	return file + ":" + strconv.Itoa(position.Line)
}
