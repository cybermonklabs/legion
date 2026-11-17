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

// heldStarts are the functions outside the standard library that the launcher's process reaches
// and that start a process themselves, each with how its child is held from every reap of that
// process (the orphan reaper's passes and the controller pod's drain, pidGuard.reap), or why it
// never runs there. Any other start the walk reaches is one the reaper can take the exit of, which
// fails its own wait with `waitid: no child processes` (or under gVisor `no such process`). An
// entry the walk no longer reaches is named too, so the list never outlives what it allows.
var heldStarts = map[string]string{
	"(*" + launcherPackage + ".pidGuard).start":    "the guard itself: it records the child's pid under the lock every reap holds",
	"(*" + launcherPackage + ".roleProcess).start": "a generation: its manager records the pid under its own lock as it starts it, and the orphan reaper skips every manager's child",
	launcherPackage + ".runRoleShim":               "role-init's shim, in role-init's own process: role-init's reaper skips its pid",
	// The session proxy's Cloud SQL connector reaches Google's credential and client-certificate
	// code, which can start a helper the machine's own configuration names. A pod names none: it
	// signs in as its own identity from the metadata server.
	"(*cloud.google.com/go/auth/internal/transport/cert.secureConnectSource).getClientCertificate": "SecureConnect's certificate command, which runs only where ~/.secureConnect/context_aware_metadata.json names one; no pod holds that file",
	"(*google.golang.org/api/internal/cert.secureConnectSource).getClientCertificate":              "SecureConnect's certificate command, which runs only where ~/.secureConnect/context_aware_metadata.json names one; no pod holds that file",
	"github.com/googleapis/enterprise-certificate-proxy/client.Cred":                               "the enterprise certificate signer, which runs only where gcloud's certificate_config.json names one; no pod holds that file",
	"(cloud.google.com/go/auth/credentials/internal/externalaccount.runtimeEnvironment).run":       "an external-account credential's executable, which runs only for such a credentials file and GOOGLE_EXTERNAL_ACCOUNT_ALLOW_EXECUTABLES=1; a pod has neither",
	"(golang.org/x/oauth2/google/externalaccount.runtimeEnvironment).run":                          "an external-account credential's executable, which runs only for such a credentials file and GOOGLE_EXTERNAL_ACCOUNT_ALLOW_EXECUTABLES=1; a pod has neither",
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
// two rules of its own. A call the standard library makes through a function value or an interface
// is not followed, since what the standard library calls back is only what it was handed; instead,
// every function value, closure and method set that walked code outside the standard library hands
// out is walked as though it were called. Without the first rule a callback inside sync.Once or
// fmt reaches every function of the same signature in the program; without the second, a start
// handed to the standard library (a goroutine's body, an http.Handler) would go unread.
//
// It cannot pass by reading nothing: its control plants three starts the launcher would reach, a
// bare cmd.Run in newCheckInstall, procgroup.NoReaper given to the workspace restore in place of
// the guard's starter, and the install's run of the check as it was before the guard took it (a
// bare cmd.Run in leakguard.verifyServed), and must see each named.
func TestEveryChildTheLaunchersProcessWaitsForStartsWhereNoReapTakesIt(t *testing.T) {
	t.Run("this tree", func(t *testing.T) {
		starts, reached := launcherProcessStarts(t, nil)
		for _, start := range starts {
			if _, held := heldStarts[start.caller]; !held {
				t.Errorf("%s starts a process outside the guard (%s at %s), so PID 1's reaper can take its exit; start it through pidGuard (a procgroup.Starter of the guard's), or add it to heldStarts with why no reap takes it. Reached by: %s",
					start.caller, start.callee, start.site, start.path)
			}
		}
		for caller := range heldStarts {
			if !reached[caller] {
				t.Errorf("heldStarts allows %s, which the launcher's process no longer reaches: remove it", caller)
			}
		}
	})
	t.Run("planted starts are named", func(t *testing.T) {
		overlay := map[string][]byte{}
		plant(t, overlay, "leakcheck.go", "\tif dir == \"\" {\n\t\tdir = leakguard.InstallDir\n\t}\n",
			"\t_ = exec.Command(\"/bin/true\").Run()\n", true)
		plant(t, overlay, "launcher.go", "restore := newBundleRestore(guard.starter())",
			"restore := newBundleRestore(procgroup.NoReaper)", false)
		// The defect this check came from: the install's run of the check with a bare cmd.Run, in
		// another package the launcher calls.
		plant(t, overlay, "../leakguard/install.go", "procgroup.Err(procgroup.Run(cmd, start))",
			"procgroup.Err(cmd.Run())", false)
		starts, _ := launcherProcessStarts(t, overlay)
		want := map[string]bool{
			launcherPackage + ".newCheckInstall -> (*os/exec.Cmd).Run":                               false,
			"github.com/sjawhar/legion/daemon/internal/procgroup.NoReaper -> (*os/exec.Cmd).Start":   false,
			"github.com/sjawhar/legion/daemon/internal/leakguard.verifyServed -> (*os/exec.Cmd).Run": false,
		}
		for _, start := range starts {
			key := start.caller + " -> " + start.callee
			if _, planted := want[key]; planted {
				want[key] = true
			}
		}
		for key, named := range want {
			if !named {
				t.Errorf("the check did not name the planted %s, so it could pass over a real one; it named: %v", key, starts)
			}
		}
	})
}

// plant puts into overlay file (a path from this package's directory) with addition written after
// anchor, or in its place when after is false, importing os/exec and procgroup where the file
// does not.
func plant(t *testing.T, overlay map[string][]byte, file, anchor, addition string, after bool) {
	t.Helper()
	source, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	text := string(source)
	if !strings.Contains(text, anchor) {
		t.Fatalf("%s no longer holds %q, where the control plants its start", file, anchor)
	}
	if after {
		text = strings.Replace(text, anchor, anchor+addition, 1)
	} else {
		text = strings.Replace(text, anchor, addition, 1)
	}
	for _, path := range []string{"os/exec", "github.com/sjawhar/legion/daemon/internal/procgroup"} {
		if !strings.Contains(text, strconv.Quote(path)) {
			text = strings.Replace(text, "import (\n", "import (\n\t"+strconv.Quote(path)+"\n", 1)
		}
	}
	path, err := filepath.Abs(file)
	if err != nil {
		t.Fatal(err)
	}
	overlay[path] = []byte(text)
}

// processStart is one call of a process start that the launcher's process reaches: the function
// that makes it, what it calls, where, and a path from an entry point to it.
type processStart struct {
	caller, callee, site, path string
}

func (s processStart) String() string { return s.caller + " -> " + s.callee + " at " + s.site }

// launcherProcessStarts walks the launcher's process from launcher.Run and launcher.RoleInit, with
// overlay (file contents in place of the files on disk), and answers every call outside the
// standard library of a processStarts function it reaches, and every function it reaches.
func launcherProcessStarts(t *testing.T, overlay map[string][]byte) ([]processStart, map[string]bool) {
	t.Helper()
	cfg := &packages.Config{Mode: packages.LoadAllSyntax, Dir: ".", Overlay: overlay}
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
	graph := vta.CallGraph(ssautil.AllFunctions(prog), cha.CallGraph(prog))

	w := walk{prog: prog, graph: graph, parent: map[*ssa.Function]*ssa.Function{}, seen: map[*ssa.Function]bool{}}
	for _, root := range roots {
		w.visit(root, nil)
	}
	reached := map[string]bool{}
	var starts []processStart
	for len(w.queue) > 0 {
		fn := w.queue[0]
		w.queue = w.queue[1:]
		reached[fn.String()] = true
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
	return slices.CompactFunc(starts, func(a, b processStart) bool { return a.String() == b.String() }), reached
}

// walk is the state of one walk of the launcher's process: the functions it has reached, in the
// order it reached them, and for each the function it was reached from.
type walk struct {
	prog   *ssa.Program
	graph  *callgraph.Graph
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
// and the methods of every type it puts into an interface.
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
				if value, ok := (*operand).(*ssa.Function); ok {
					w.visit(value, fn)
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
