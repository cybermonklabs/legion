package workflow

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
)

// roleForAnswers is the role that works each phase, written out here rather than read from
// phase.RoleFor, so a phase added to the daemon fails this test until somebody decides what role
// works it. The architect works two of them: admitted, where a root's own architect reads its
// spec and decomposes its tree, and accepting, where it signs the work off. No row of the table
// starts either (Table), and every site that starts, resumes or reports a phase's worker skips
// the architect.
var roleForAnswers = map[phase.Phase]claim.Role{
	phase.Admitted:       claim.RoleArchitect,
	phase.AwaitingPerson: "",
	phase.Planning:       claim.RolePlanner,
	phase.Implementing:   claim.RoleImplementer,
	phase.Testing:        claim.RoleTester,
	phase.Reviewing:      claim.RoleReviewer,
	phase.Accepting:      claim.RoleArchitect,
	phase.Retro:          claim.RoleImplementer,
	phase.Merging:        claim.RoleMerger,
	phase.AwaitingMerge:  "",
	phase.Done:           "",
	phase.Held:           "",
}

// R1: phase.RoleFor answers the architect for admitted, beside accepting, and this package's
// forwarder of the same name answers what it answers. The phases come from phase.All, so the test
// reads the whole phase set the daemon writes and not a list of its own.
func TestRoleForAnswersEveryPhaseOfPhaseAll(t *testing.T) {
	for _, p := range phase.All {
		want, written := roleForAnswers[p]
		if !written {
			t.Errorf("phase %s is in phase.All and in no entry of this test's table: decide which role works it, and write it here", p)
			continue
		}
		if got := phase.RoleFor(p); got != want {
			t.Errorf("phase.RoleFor(%s) = %q, want %q", p, got, want)
		}
		if got := RoleFor(p); got != want {
			t.Errorf("workflow.RoleFor(%s) = %q, want the %q phase.RoleFor answers: the forwarder restates nothing", p, got, want)
		}
	}
	for p := range roleForAnswers {
		if !slices.Contains(phase.All, p) {
			t.Errorf("this test's table holds phase %s, which phase.All does not: remove it here", p)
		}
	}
}

// roleForSite is one function of the daemon that reads phase.RoleFor, or this package's forwarder
// of it: the file it is in, relative to the module root, and its name, a method's prefixed with
// its receiver type.
type roleForSite struct{ file, fn string }

// roleForReader is what the reader list pins for one site: how many times the function reads
// RoleFor, and the behaviour test that holds what the site does with the answer today.
type roleForReader struct {
	calls int
	test  string
}

// roleForReaders is every non-test reader of phase.RoleFor and of workflow.RoleFor in the daemon,
// and the test that holds each one's behaviour. A reader is a call, or the function passed as a
// value to a caller that calls it (internal/daemon's wiring of api.Options.RoleFor). Only one of
// them answers differently now that RoleFor answers the architect for admitted: noticeFor, where
// a guard of its own keeps the notice roleless (R2). Every other site reaches its answer through
// a guard that treats the architect as it treated the empty role, or names the role an admitted
// root's entry numbers are of, which is what each named test holds it to.
//
// A reader this list does not hold fails the test, so a new one is decided rather than taking
// whatever RoleFor answers at admitted.
var roleForReaders = map[roleForSite]roleForReader{
	{file: "cmd/legion/main.go", fn: "entriesBehind"}:                       {calls: 1, test: "TestStatePlainNamesTheRolesBehindTheirEntryAndTheRootsAWakeNames"},
	{file: "internal/admit/admit.go", fn: "Admission.startMidPhaseMembers"}: {calls: 1, test: "TestReadmissionStartsTheTreesMidPhaseChildren"},
	{file: "internal/admit/admit.go", fn: "Admission.readSlotHolders"}:      {calls: 1, test: "TestASlotWhoseWorkerHasNoTaskIsStartedAgain"},
	{file: "internal/daemon/daemon.go", fn: "serve"}:                        {calls: 1, test: "TestAnOperatorRetryOfAStoppedArchitectRelaunchesItThroughItsMachine"},
	{file: "internal/projection/project.go", fn: "Project"}:                 {calls: 1, test: "TestProjectShowsARootsArchitectHandshake"},
	{file: "internal/workflow/effects.go", fn: "HeldNotice"}:                {calls: 1, test: "TestACarriedRootResumesFromTheApprovedHeadAndTheHold"},
	{file: "internal/workflow/effects.go", fn: "RoleFor"}:                   {calls: 1, test: "TestRoleForAnswersEveryPhaseOfPhaseAll"},
	{file: "internal/workflow/engine.go", fn: "Engine.handoff"}:             {calls: 1, test: "TestTheArchitectsCompletionInAcceptingIsRefused"},
	{file: "internal/workflow/engine.go", fn: "Engine.claimFailed"}:         {calls: 1, test: "TestAnArchitectsClaimFailedHoldsNoIssueAndWritesNoHeldNotice"},
	{file: "internal/workflow/engine.go", fn: "Engine.restartPhase"}:        {calls: 1, test: "TestTheOperatorsRestartOfAPhaseAnotherRoleWorksIsRefused"},
	{file: "internal/workflow/engine.go", fn: "Engine.retryPhase"}:          {calls: 1, test: "TestTheOperatorsRestartOfAHeldIssueRestoresThePhase"},
	{file: "internal/workflow/engine.go", fn: "Engine.backward"}:            {calls: 1, test: "TestAPlannersBackwardMoveFromAcceptingIsRefused"},
	{file: "internal/workflow/engine.go", fn: "Engine.transition"}:          {calls: 2, test: "TestAnApprovalMovesTheIssueIntoAcceptingStartingNobody"},
	{file: "internal/workflow/engine.go", fn: "noticeFor"}:                  {calls: 1, test: "TestThePhaseFinishedNoticeOutOfAdmittedCarriesNoRole"},
	{file: "internal/workflow/engine.go", fn: "Engine.phaseEnded"}:          {calls: 1, test: "TestARedTellsTheRoleWhosePhaseItEnded"},
	{file: "internal/workflow/resume.go", fn: "Engine.booted"}:              {calls: 1, test: "TestTheBootsResumeReachesASlotHolderAndWaitsWhereTheIssueHoldsNone"},
	{file: "internal/workflow/stranded.go", fn: "Stranded"}:                 {calls: 1, test: "TestStrandedNamesTheClaimsNothingWillWake"},
	{file: "internal/workflow/wake.go", fn: "OpenPhaseRole"}:                {calls: 1, test: "TestAnArchitectGetsNoBackstop"},
}

// R2: the reader list is complete and nothing reads RoleFor that this test does not hold. The
// daemon's packages are parsed from the module root, which is this package's own directory with
// the two last elements removed; test files are left out of the reader list and read for the
// test names the list points at.
func TestEveryReaderOfRoleForLeavesTheArchitectUnchanged(t *testing.T) {
	root := filepath.Join("..", "..")
	found := map[roleForSite]int{}
	declared := map[string]bool{}
	fset := token.NewFileSet()
	if err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if path != root && strings.HasPrefix(entry.Name(), ".") {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		if strings.HasSuffix(path, "_test.go") {
			for _, decl := range file.Decls {
				if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil && strings.HasPrefix(fn.Name.Name, "Test") {
					declared[fn.Name.Name] = true
				}
			}
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		for _, decl := range file.Decls {
			site := roleForSite{file: filepath.ToSlash(relative), fn: "(package level)"}
			node := ast.Node(decl)
			if fn, ok := decl.(*ast.FuncDecl); ok {
				if fn.Body == nil {
					continue
				}
				site.fn, node = functionName(fn), fn.Body
			}
			ast.Inspect(node, func(node ast.Node) bool {
				if readsRoleFor(node) {
					found[site]++
				}
				return true
			})
		}
		return nil
	}); err != nil {
		t.Fatalf("read the daemon's packages: %v", err)
	}

	for _, site := range sortedSites(roleForReaders) {
		pinned := roleForReaders[site]
		calls, reads := found[site]
		switch {
		case !reads:
			t.Errorf("%s's %s reads RoleFor in this test's table and not in the source: remove the entry, or name the function that reads it now", site.file, site.fn)
		case calls != pinned.calls:
			t.Errorf("%s's %s calls RoleFor %d times, want the %d this test's table pins: decide what each new call answers for a phase the architect works", site.file, site.fn, calls, pinned.calls)
		}
		if !declared[pinned.test] {
			t.Errorf("%s's %s names the behaviour test %s, which no test file of the daemon declares", site.file, site.fn, pinned.test)
		}
	}
	for _, site := range sortedSites(found) {
		if _, pinned := roleForReaders[site]; !pinned {
			t.Errorf("%s's %s reads RoleFor and is in no entry of this test's table: decide what it must answer for admitted, which the architect works, and write the site and the test that holds it here",
				site.file, site.fn)
		}
	}
}

// functionName is fn's name, a method's prefixed with its receiver type.
func functionName(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return fn.Name.Name
	}
	receiver := fn.Recv.List[0].Type
	if pointer, ok := receiver.(*ast.StarExpr); ok {
		receiver = pointer.X
	}
	if named, ok := receiver.(*ast.Ident); ok {
		return named.Name + "." + fn.Name.Name
	}
	return fn.Name.Name
}

// readsRoleFor is whether node reads phase.RoleFor or this package's forwarder of it: a selector
// naming either through its package, in a call or passed as a value, or a call of the bare name
// inside the package that declares it. A bare RoleFor that is no call is a field key
// (api.Options.RoleFor) and not a read, and appauth.AppRoleFor is another function and is not one
// of these.
func readsRoleFor(node ast.Node) bool {
	switch read := node.(type) {
	case *ast.CallExpr:
		name, ok := read.Fun.(*ast.Ident)
		return ok && name.Name == "RoleFor"
	case *ast.SelectorExpr:
		pkg, ok := read.X.(*ast.Ident)
		return ok && (pkg.Name == "phase" || pkg.Name == "workflow") && read.Sel.Name == "RoleFor"
	}
	return false
}

// sortedSites is every key of sites, by file and then by function, so a failure lists the sites
// in one order whatever order the map walk gives.
func sortedSites[V any](sites map[roleForSite]V) []roleForSite {
	keys := make([]roleForSite, 0, len(sites))
	for site := range sites {
		keys = append(keys, site)
	}
	slices.SortFunc(keys, func(a, b roleForSite) int {
		if a.file != b.file {
			return strings.Compare(a.file, b.file)
		}
		return strings.Compare(a.fn, b.fn)
	})
	return keys
}
