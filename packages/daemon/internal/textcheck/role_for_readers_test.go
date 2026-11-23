package textcheck

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The list of every reader of phase.RoleFor stands here, and not beside the table of what RoleFor
// answers (internal/workflow's role_for_test.go, TestRoleForAnswersEveryPhaseOfPhaseAll), because
// the check reads every Go file of packages/daemon, test files included, for the test names the
// list points at. A test of internal/workflow that read them would put every test file of the
// module on the filter of the daemon's Go job, this package's and the live tests' among them, so
// an edit to one of those would start that job's whole suite. This package's job already reads
// that set for TestEveryCreatedDatabaseIsAFileCopy, and the check needs nothing a product package
// compiles: it parses source.

// roleForSite is one function of the daemon that reads phase.RoleFor, or internal/workflow's
// forwarder of it: the file it is in, relative to the module root, and its name, a method's
// prefixed with its receiver type.
type roleForSite struct{ file, fn string }

// roleForReader is what the reader list pins for one site: how many times the function reads
// RoleFor, and the behaviour test that holds what the site does with the answer today.
type roleForReader struct {
	calls int
	test  string
}

// roleForReaders is every non-test reader of phase.RoleFor and of workflow.RoleFor in the daemon,
// and the test that holds each one's behaviour. A reader is a call, or the function passed as a
// value to a caller that calls it (internal/daemon's wiring of api.Options.RoleFor). Four of them
// have a guard of their own for admitted, the phase RoleFor answers the architect for:
// noticeFor writes the notice of the move out of admitted with no role (R2),
// Engine.leftItsPhase records no worker's completion while the issue stands at admitted, the
// completion route's entryFence answers a completion at admitted as one of a phase its role does
// not work, and the stalled-tree notice's reason tells an issue at admitted as standing in a phase
// no role works, the words it wrote before RoleFor named the architect there. Every other site
// reaches its answer through a guard that starts, resumes or reports no architect as a phase
// worker, or names the role an admitted root's entry numbers are of, or names the architect in the
// notice of a task its owner stopped in the architect's phase, where the stop ends no process
// (Engine.taskStopped, at admitted as at accepting), or answers the issue's own phase for the
// architect's completion there, as its reader took the issue's phase while RoleFor named no role
// (phase.Of), or reads only the phases of a plan's steps, which admitted never is (reportsBefore,
// and Engine.restartTask, which reads its task's first step: a planner's or an implementer's, as
// plan.Supported holds every task's steps to today's order with an implementer among them),
// or reads the retro's own phase and the phase of the last step of a parent's task, neither of
// which is admitted (retroHeldNotice and Engine.startHeldRetro, which run only for a parent held
// before its retro for the issues beneath it), or never reads admitted at all because a check
// before it refuses that phase, which is what each named test holds it to. The owner walk reads no
// role: where it starts is its caller's to say (record.NoticeStartsAtParent, for a notice).
//
// A reader this list does not hold fails the test, so a new one is decided rather than taking
// whatever RoleFor answers at admitted.
var roleForReaders = map[roleForSite]roleForReader{
	{file: "cmd/legion/main.go", fn: "entriesBehind"}:                             {calls: 1, test: "TestStatePlainNamesTheRolesBehindTheirEntryAndTheRootsTheStateNamesAnArmFor"},
	{file: "internal/admit/admit.go", fn: "Admission.startMidPhaseMembers"}:       {calls: 1, test: "TestReadmissionStartsTheTreesMidPhaseChildren"},
	{file: "internal/admit/stalled.go", fn: "stalledTreeReason"}:                  {calls: 1, test: "TestAWaitByDesignIsNeverToldAsAStall"},
	{file: "internal/api/handoff.go", fn: "entryFence"}:                           {calls: 1, test: "TestArchitectUnchangedByItsCompletionAtAdmitted"},
	{file: "internal/daemon/daemon.go", fn: "serve"}:                              {calls: 1, test: "TestAnOperatorRetryOfAStoppedArchitectRelaunchesItThroughItsMachine"},
	{file: "internal/daemon/design_news_holders.go", fn: "phaseHolder"}:           {calls: 1, test: "TestOnlyAPhaseWorkerHoldsAMembersPhase"},
	{file: "internal/phase/phase.go", fn: "Of"}:                                   {calls: 1, test: "TestArchitectUnchangedByItsCompletionAtAdmitted"},
	{file: "internal/projection/project.go", fn: "Project"}:                       {calls: 1, test: "TestProjectShowsARootsArchitectHandshake"},
	{file: "internal/record/design_news.go", fn: "untold"}:                        {calls: 1, test: "TestOwedNewsOwesEachHolderTheVersionsItHasNotTaken"},
	{file: "internal/workflow/architect_start.go", fn: "writeCarriedPhase"}:       {calls: 1, test: "TestTheArchitectTaskOfEachShape"},
	{file: "internal/workflow/catch_up.go", fn: "Engine.readyWorkerTask"}:         {calls: 1, test: "TestArchitectUnchangedByAReadyWorkerOfAnIssueAtAdmitted"},
	{file: "internal/workflow/design_news.go", fn: "memberWords"}:                 {calls: 1, test: "TestTheRootArchitectsSummaryNamesEachMemberAsTheRecordStandsAtItsSend"},
	{file: "internal/workflow/effects.go", fn: "HeldNotice"}:                      {calls: 1, test: "TestACarriedRootResumesFromTheApprovedHeadAndTheHold"},
	{file: "internal/workflow/effects.go", fn: "RoleFor"}:                         {calls: 1, test: "TestRoleForAnswersEveryPhaseOfPhaseAll"},
	{file: "internal/workflow/effects.go", fn: "backwardMoveReason"}:              {calls: 1, test: "TestArchitectUnchangedByABackwardMoveFromAdmitted"},
	{file: "internal/workflow/engine.go", fn: "Engine.claimFailed"}:               {calls: 1, test: "TestAnArchitectsClaimFailedHoldsNoIssueAndWritesNoHeldNotice"},
	{file: "internal/workflow/engine.go", fn: "Engine.leftItsPhase"}:              {calls: 1, test: "TestArchitectUnchangedByAWorkersCompletionAtAdmitted"},
	{file: "internal/workflow/engine.go", fn: "Engine.restartPhase"}:              {calls: 1, test: "TestTheOperatorsRestartOfAPhaseStartsItsWorkerWithThePhasesTask"},
	{file: "internal/workflow/engine.go", fn: "Engine.retryPhase"}:                {calls: 1, test: "TestTheOperatorsRestartOfAPhaseStartsItsWorkerWithThePhasesTask"},
	{file: "internal/workflow/engine.go", fn: "backwardRefusal"}:                  {calls: 2, test: "TestArchitectUnchangedByABackwardMoveFromAdmitted"},
	{file: "internal/workflow/engine.go", fn: "Engine.transition"}:                {calls: 2, test: "TestAnApprovalMovesTheIssueIntoAcceptingStartingNobody"},
	{file: "internal/workflow/engine.go", fn: "noticeFor"}:                        {calls: 3, test: "TestThePhaseFinishedNoticeOutOfAdmittedCarriesNoRole"},
	{file: "internal/workflow/plan_stop.go", fn: "StoppedStepRole"}:               {calls: 1, test: "TestStopTaskHoldsTheIssueAndStopsTheStep"},
	{file: "internal/workflow/plan_stop.go", fn: "Engine.taskStopped"}:            {calls: 1, test: "TestStopTaskHoldsTheIssueAndStopsTheStep"},
	{file: "internal/workflow/plan_stop.go", fn: "Engine.restartTask"}:            {calls: 1, test: "TestATodoOnAStoppedRootStartsItsTaskAtStepOne"},
	{file: "internal/workflow/gate.go", fn: "Engine.tellGateMoved"}:               {calls: 1, test: "TestEveryPhaseHolderAndTheRootArchitectAreToldTheGateClosedAndOpened"},
	{file: "internal/workflow/merge_forward.go", fn: "Engine.mergeForward"}:       {calls: 1, test: "TestAConflictKeepsPhaseAndAsksTheImplementerToMergeForward"},
	{file: "internal/workflow/merge_forward.go", fn: "Engine.mergeForwardLanded"}: {calls: 1, test: "TestAConflictKeepsPhaseAndAsksTheReviewerToReadTheMerge"},
	{file: "internal/workflow/review.go", fn: "Engine.tellChecks"}:                {calls: 1, test: "TestASettlementMovesNothingAndTellsTheThreeRoles"},
	{file: "internal/workflow/resume.go", fn: "Engine.suspendedPhaseOwners"}:      {calls: 1, test: "TestTheBootsResumeReadsRoleForAndStartsNoArchitectAtAdmittedOrAccepting"},
	{file: "internal/workflow/retro_hold.go", fn: "retroHeldNotice"}:              {calls: 1, test: "TestAParentWithNoAcceptanceIsHeldBeforeItsRetro"},
	{file: "internal/workflow/retro_hold.go", fn: "Engine.startHeldRetro"}:        {calls: 2, test: "TestAParentWithNoAcceptanceIsHeldBeforeItsRetro"},
	{file: "internal/workflow/standing.go", fn: "reportsBefore"}:                  {calls: 1, test: "TestATaskNamesItsWorkingCopyItsStepAndTheReportsBeforeIt"},
	{file: "internal/workflow/steps.go", fn: "Engine.startAgent"}:                 {calls: 1, test: "TestAnIssueAtAdmittedRunsNoStep"},
	{file: "internal/workflow/steps.go", fn: "Engine.upgradedStart"}:              {calls: 2, test: "TestAnIssueAtAdmittedRunsNoStep"},
	{file: "internal/workflow/steps.go", fn: "endOfTransition"}:                   {calls: 1, test: "TestAnIssueAtAdmittedRunsNoStep"},
	{file: "internal/workflow/stranded.go", fn: "Stranded"}:                       {calls: 1, test: "TestStrandedNamesTheClaimsNothingWillWake"},
	{file: "internal/workflow/wake.go", fn: "OpenPhaseRole"}:                      {calls: 1, test: "TestAnArchitectGetsNoBackstop"},
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

// readsRoleFor is whether node reads phase.RoleFor or internal/workflow's forwarder of it: a
// selector naming either through its package, in a call or passed as a value, or a call of the
// bare name inside the package that declares it. A bare RoleFor that is no call is a field key
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
