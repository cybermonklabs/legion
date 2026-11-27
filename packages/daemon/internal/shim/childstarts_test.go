// The walk loads the shim and every package it imports from source and builds a call graph over the
// whole program, twice, as the launcher's walk does (internal/launcher's childstarts_test.go), and
// the race detector multiplies its time and memory, so the race build leaves this file out.

//go:build !race

package shim_test

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/startwalk"
)

const (
	shimPackage      = "github.com/sjawhar/legion/daemon/internal/shim"
	workspacePackage = "github.com/sjawhar/legion/daemon/internal/workspace"
	subreaperPackage = "github.com/sjawhar/legion/daemon/internal/subreaper"
)

// shimHeldStarts are the calls of a process start the shim's process may reach, each keyed by the
// function that makes it and the start it calls (startwalk.Start.Call), with how many call sites
// make it and why no pass of the shim's reaper takes its child's exit. Any other start the walk
// reaches is one that reaper can take the exit of, under --reap-orphans, which fails the child's
// own wait with `waitid: no child processes`. A count that changes fails too, in either direction.
var shimHeldStarts = map[string]struct {
	sites int
	why   string
}{
	"(*" + subreaperPackage + ".Guard).Start -> (*os/exec.Cmd).Start": {1, "the guard itself: it records the child's pid under the lock every pass of the reaper holds"},
}

// The shim's process — `legion worker-shim`, the subreaper of what it starts under --reap-orphans
// (Config.ReapOrphans) — reaps every exited child its guard does not hold, so every child that
// process starts and waits for itself must start through that guard (subreaper.Guard's Start, or
// its Starter wherever code takes a procgroup.Starter): Oh My Pi, the secrets keygen and renewer,
// every command the daemon asks for and the warm-up's codegraph. This check walks the call graph
// from shim.Run, and from workspace.WarmCodegraphIndex, which `legion worker-shim` hands the shim
// as Config.WarmCodegraph (cmd/legion's TestTheWorkerShimWarmsWithTheWorkspacesWarmUp holds it to
// that function), since a function stored in a field the walk does not follow from the store; it
// fails on every start it reaches that shimHeldStarts does not hold, naming the call site and a
// path to it. A start through procgroup.NoReaper counts as such a start.
//
// It cannot pass by reading nothing: its control plants a start where each of the shim's children
// starts, as it would be with the guard left out, and must see each one named: Oh My Pi's spawn,
// the keygen's run, the renewer's start inside the goroutine that keeps it, procgroup.NoReaper
// given to the commands the daemon asks for in place of the guard's starter, and the warm-up's
// codegraph run in the workspace package, which only the second root reaches.
func TestEveryChildTheShimWaitsForStartsThroughItsGuard(t *testing.T) {
	t.Run("this tree", func(t *testing.T) {
		sites := map[string][]startwalk.Start{}
		for _, start := range shimProcessStarts(t, nil) {
			sites[start.Call()] = append(sites[start.Call()], start)
		}
		for call, starts := range sites {
			held, ok := shimHeldStarts[call]
			switch {
			case !ok:
				for _, start := range starts {
					t.Errorf("%s starts a process outside the shim's guard (%s at %s), so the shim's reaper can take its exit; start it through subreaper.Guard (its Starter where the code takes a procgroup.Starter), or add it to shimHeldStarts with why no pass takes it. Reached by: %s",
						start.Caller, start.Callee, start.Site, start.Path)
				}
			case len(starts) != held.sites:
				t.Errorf("shimHeldStarts allows %s at %d call sites, and the walk reaches %d: %v; read them again and set the count", call, held.sites, len(starts), starts)
			}
		}
		for call := range shimHeldStarts {
			if len(sites[call]) == 0 {
				t.Errorf("shimHeldStarts allows %s, which the walk no longer reaches. Remove it only if the code that started it is gone: if that code is still there, the walk has gone blind to it, and the control subtest names what broke", call)
			}
		}
	})
	t.Run("planted starts are named", func(t *testing.T) {
		overlay := map[string][]byte{}
		plantShim(t, overlay, "shim.go", "if _, err := s.children.Start(cmd); err != nil {", "if err := cmd.Start(); err != nil {")
		plantShim(t, overlay, "agentsecrets.go", "procgroup.Run(cmd, s.children.Starter())", "cmd.Run()")
		plantShim(t, overlay, "agentsecrets.go", "pid, err := s.children.Start(cmd)", "pid, err := 0, cmd.Start()")
		plantShim(t, overlay, "commands.go", "start := s.children.Starter()", "start := procgroup.Starter(procgroup.NoReaper)")
		plantShim(t, overlay, "../workspace/codegraph.go", "done, err := start(cmd)", "done, err := func() {}, cmd.Start()")
		starts := shimProcessStarts(t, overlay)
		for _, want := range []struct{ what, caller string }{
			{"Oh My Pi's spawn with a bare cmd.Start", "(*" + shimPackage + ".shim).spawnOnce"},
			{"the keygen's run with a bare cmd.Run", "(*" + shimPackage + ".shim).identity"},
			{"the renewer's start with a bare cmd.Start, in the goroutine that keeps it", "(*" + shimPackage + ".shim).startRenewer$1"},
			{"procgroup.NoReaper in place of the guard's starter", "github.com/sjawhar/legion/daemon/internal/procgroup.NoReaper"},
			{"the warm-up's codegraph run with a bare cmd.Run", workspacePackage + ".runCodegraph"},
		} {
			if !slices.ContainsFunc(starts, func(start startwalk.Start) bool { return start.Caller == want.caller }) {
				t.Errorf("the check did not name %s, so it could pass over a real one; it named: %v", want.what, starts)
			}
		}
	})
}

// plantShim is startwalk.Plant in this package's files and the workspace's, importing os/exec and
// procgroup where the file does not.
func plantShim(t *testing.T, overlay map[string][]byte, file, anchor, replacement string) {
	t.Helper()
	startwalk.Plant(t, overlay, filepath.FromSlash(file), anchor, replacement, "os/exec", "github.com/sjawhar/legion/daemon/internal/procgroup")
}

// shimProcessStarts walks the shim's process from shim.Run and workspace.WarmCodegraphIndex, with
// overlay, and answers every call outside the standard library of a process start it reaches.
func shimProcessStarts(t *testing.T, overlay map[string][]byte) []startwalk.Start {
	t.Helper()
	starts, err := startwalk.Walk(".", overlay,
		startwalk.Root{Path: shimPackage, Func: "Run"}, startwalk.Root{Path: workspacePackage, Func: "WarmCodegraphIndex"})
	if err != nil {
		t.Fatal(err)
	}
	return slices.DeleteFunc(starts, func(start startwalk.Start) bool { return strings.HasSuffix(start.Site, "_test.go") })
}
