// The walk loads the launcher and every package it imports from source and builds a call graph
// over the whole program, twice: without the race detector the test took 24 s at a 1.95 GB peak on
// a development host and 66 s at 2.34 GB in a pod, and the detector multiplies both. So the race
// build leaves this file out, and daemon-go's plain step for what the race build cannot run runs
// its test by name (.github/scripts/ci_job_reads.py).

//go:build !race

package launcher

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/startwalk"
)

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
// (startwalk.Start.Call). Any other start the walk reaches is one the reaper can take the exit of,
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
// exit before the install's own wait could. This check walks the call graph from launcher.Run and
// launcher.RoleInit and fails on every start it reaches that heldStarts does not hold, naming the
// call site and a path to it. A start through procgroup.NoReaper counts as such a start: it is
// where the walk ends up when code the launcher reaches is handed the start of a process that does
// not reap.
//
// The walk is internal/startwalk's: variable type analysis over the program, with its rules for
// the callbacks the standard library makes and for functions stored in package variables.
//
// It cannot pass by reading nothing, nor with a rule of its own broken: its control plants starts
// the launcher would reach, and must see each named (plantedStarts): a bare cmd.Run in
// newCheckInstall; one in a package variable's function that newCheckInstall hands to
// context.AfterFunc; procgroup.NoReaper handed to procgroup.Run in place of the guard's starter;
// the install's run of the check as it was before the guard took it (a bare cmd.Run in
// leakguard.verifyServed); a closure only sync.Once.Do calls; a start made through each other
// shape Go gives it: a method value called, a method value handed to a helper, to errgroup.Go and
// to sync.OnceValue, a method expression, and a method promoted from an embedded *exec.Cmd and
// called through an interface; a writer only fmt calls; a method value stored in a package
// variable; and functions an init stores into a package variable's field and element. And a
// command only formatted, which starts nothing, must stay unnamed.
func TestEveryChildTheLaunchersProcessWaitsForStartsWhereNoReapTakesIt(t *testing.T) {
	t.Parallel()
	t.Run("this tree", func(t *testing.T) {
		sites := map[string][]startwalk.Start{}
		for _, start := range launcherProcessStarts(t, nil) {
			sites[start.Call()] = append(sites[start.Call()], start)
		}
		for call, starts := range sites {
			held, ok := heldStarts[call]
			switch {
			case !ok:
				for _, start := range starts {
					t.Errorf("%s starts a process outside the guard (%s at %s), so PID 1's reaper can take its exit; start it through pidGuard (a procgroup.Starter of the guard's), or add it to heldStarts with why no reap takes it. Reached by: %s",
						start.Caller, start.Callee, start.Site, start.Path)
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
			named := slices.ContainsFunc(starts, func(start startwalk.Start) bool {
				return start.Caller == want.caller || strings.HasSuffix(want.caller, "$") && strings.HasPrefix(start.Caller, want.caller)
			})
			if !named {
				t.Errorf("the check did not name %s, so it could pass over a real one; it named: %v", want.what, starts)
			}
		}
		// Formatting a command puts it into an interface and starts nothing, so it must not be
		// named, or the gate would go red on a log line.
		for _, start := range starts {
			if start.Caller == launcherPackage+".plantedFormatted" {
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

	"github.com/sjawhar/legion/daemon/internal/procgroup"
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
	plantedNoReaper()
}

func plantedFormatted() { _ = fmt.Sprint(exec.Command("/bin/true")) }

// plantedNoReaper hands procgroup.Run the start of a process that does not reap, where every child
// of the launcher's own starts through the guard's starter (pidGuard.starter).
func plantedNoReaper() { _ = procgroup.Run(exec.Command("/bin/true"), procgroup.NoReaper) }

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

// plant is startwalk.Plant in this package's files, importing os/exec and procgroup where the
// file does not.
func plant(t *testing.T, overlay map[string][]byte, file, anchor, replacement string) {
	t.Helper()
	startwalk.Plant(t, overlay, file, anchor, replacement, "os/exec", "github.com/sjawhar/legion/daemon/internal/procgroup")
}

// launcherProcessStarts walks the launcher's process from launcher.Run and launcher.RoleInit, with
// overlay (file contents in place of the files on disk), and answers every call outside the
// standard library of a process start it reaches, one per call site.
func launcherProcessStarts(t *testing.T, overlay map[string][]byte) []startwalk.Start {
	t.Helper()
	starts, err := startwalk.Walk(".", overlay,
		startwalk.Root{Path: launcherPackage, Func: "Run"}, startwalk.Root{Path: launcherPackage, Func: "RoleInit"})
	if err != nil {
		t.Fatal(err)
	}
	return starts
}
