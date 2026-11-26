package textcheck

// The branch of dispatch://INFRA-2614 wrote this check in internal/workflow's role_for_test.go;
// it moved here because it walks every .go file of the module, test files included, and a row
// keyed on a file of internal/workflow would put every test file of packages/daemon - this
// package's and the live tests' among them - on daemon-go's filter (dispatch://INFRA-1413).
//ci:reads packages/daemon/**/*.go walks every .go file under packages/daemon (../..,) and TestEveryReaderOfRoleForLeavesTheArchitectUnchanged parses each one

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

// roleForSite is one function of the daemon that reads phase.RoleFor, or one of its forwarders
// (readsRoleFor): the file it is in, relative to the module root, and its name, a method's
// prefixed with its receiver type.
type roleForSite struct{ file, fn string }

// roleForReader is what the reader list pins for one site: how many times the function reads
// RoleFor, and the behaviour tests that hold what the site does with the answer today. The first
// holds its answer for a phase the architect works; where a task of the deployment's roles added
// the read or moved it onto a forwarder, the failing test of that task follows, which holds its
// answer at step, where the forwarder names the role the plan gives the running step.
type roleForReader struct {
	calls int
	tests []string
}

// roleForReaders is every non-test reader of phase.RoleFor in the daemon, read directly or through
// one of its forwarders (internal/workflow's RoleFor, record.Issue.RunningAt and Walk.RoleAt), and
// the tests that hold each one's behaviour. A reader is a call, or the function passed as a value
// to a caller that calls it. Four of them have a guard of their own for admitted, the phase RoleFor
// answers the architect for: Walk.endingRole reads the role whose step a move ends, which
// noticeFor leaves out of the notice of the move out of admitted; Engine.leftItsPhase records no
// worker's completion while the issue stands at admitted, before it reads the roles of the steps
// there (rolesAt); the completion's entry fence (intake.EntryFence, which the completion route and
// the workflow's handoff both ask) answers a completion at admitted as one of a phase its role does
// not work; and the stalled-tree notice's reason tells an issue at admitted as standing in a phase
// no role works, the words it wrote before RoleFor named the architect there. Every other site
// reaches its answer through a guard that starts, resumes or reports no architect as a phase
// worker (OpenStepsOf answers none for the architect's steps, so the paths that read it, OpenWorkers
// among them, are no readers here), or names the role an admitted root's entry numbers are
// of, or names the architect in the notice of a task its owner stopped in the architect's phase,
// where the stop ends no process (Engine.taskStopped, at admitted as at accepting), or answers the
// issue's own phase for the architect's completion there, as its reader took the issue's phase
// while RoleFor named no role (phase.Of, and the completion route's answerCompletion, which reads
// the phase LeftBy answers where the role does not work the issue's), or reads only the places of
// a plan's steps, which admitted never is (reportsBefore, Walk.RoleAt, and Engine.startAtFirstStep
// and Engine.startReady, which read their task's first step: a planner's, an
// implementer's or a role of the deployment's, as plan.Supported holds every task to an
// implementer step with each tester, reviewer and acceptance step after it, and
// Engine.startAfterWait, which reads the step a task whose start waited stands
// at, a step place: a first step whose join ended or is queued again, or a step whose freed agent
// takes it, since only a step waits for its join or its agent), or reads the retro's own
// phase (retroHeldNotice and Engine.StartHeldRetro, which run only for a parent held before its
// retro for the issues beneath it), or answers the running steps' roles to the readers this list
// holds (record.RunningOf, and record.stepOfRow, the one reading of a row of plan rows, which
// Postgres.AgentWaiter reads too), or reads of the running steps at a phase only whether one is
// held, never its role, and answers by the issue's own phase where no step runs (record's
// Issue.HoldsQueued), or reads the role of the step a held issue's retry restores
// only past the refusal of a plan's own hold, which is the one hold an issue at admitted takes
// (heldStepRole, which Engine.retryOrEscalate reads for the retry of a named claim and
// Engine.retryPhase for the role it restarts), or never reads admitted at all because a check
// before it refuses that phase, which is what each named test holds it to. A parent's wait on its
// open children (awaiting_children.go, dispatch://INFRA-3320) is of the last kind: each of its
// readers runs only past record.OpenChildHolds, which refuses admitted, or on the start a hold
// kept, which is never admitted's. Where one reads accepting's architect it starts no phase
// worker: the return of a parent whose pull request merged during the wait starts what a move
// into accepting starts, a child's failed architect again and the accepting step's run
// (ReturnFromChildren, through start and startStandingStep in internal/workflow's effects.go),
// and the stop holds accepting with no role told (StopForChildren), since the architect's notice
// tells it. The entry the phase's role stands on is read for R9's exemption of a move out of retro
// or merging (samePhaseExempt) only past record.OpenChildHolds, by the stop and by the guard on a
// start of the phase the issue stands in, so it reads accepting's architect at most and never
// admitted's. The owner walk reads no role: where it starts is its caller's to say
// (record.NoticeStartsAtParent, for a notice).
//
// A reader this list does not hold fails the test, so a new one is decided rather than taking
// whatever RoleFor answers at admitted.
var roleForReaders = map[roleForSite]roleForReader{
	{file: "cmd/legion/main.go", fn: "entriesBehind"}: {calls: 1, tests: []string{"TestStatePlainNamesTheRolesBehindTheirEntryAndTheRootsTheStateNamesAnArmFor"}},
	{file: "internal/admit/stalled.go", fn: "stalledTreeReason"}: {calls: 1, tests: []string{
		"TestAWaitByDesignIsNeverToldAsAStall", "TestTheAdmissionReadsTheRunningDeploymentRole"}},
	{file: "internal/api/handoff.go", fn: "server.answerCompletion"}: {calls: 1, tests: []string{
		"TestArchitectUnchangedByItsCompletionAtAdmitted", "TestADeploymentRoleReportsWithAVerdictAndACommit"}},
	{file: "internal/api/operator.go", fn: "server.worksPhase"}: {calls: 1, tests: []string{
		"TestAnOperatorRetryOfAStoppedArchitectRelaunchesItThroughItsMachine", "TestTheOperatorRetriesADeploymentRolesClaim"}},
	{file: "internal/daemon/design_news_holders.go", fn: "phaseHolder"}: {calls: 1, tests: []string{"TestOnlyAPhaseWorkerHoldsAMembersPhase"}},
	// Whether the task the per-issue read judges stands in implementing, for a plan of several tasks:
	// a running step of that task at implementing, which no step at admitted is.
	{file: "internal/daemon/issue_reads.go", fn: "issueRecords.implementing"}: {calls: 1, tests: []string{
		"TestAMissingPullRequestIsOfTheCompletingTask", "TestThePerIssueReadTellsTheArchitectOfAMissingPullRequest"}},
	{file: "internal/daemon/outbox.go", fn: "outbox.heldStartRefused"}: {calls: 1, tests: []string{
		"TestAStartRowLaunchesTheAgentOfAHeldClaimAgainOnlyWhenSomeoneDecidedIt"}},
	{file: "internal/intake/facts.go", fn: "EntryFence"}: {calls: 1, tests: []string{
		"TestArchitectUnchangedByItsCompletionAtAdmitted", "TestADeploymentRoleReportsWithAVerdictAndACommit",
		"TestACompletionOfALeftPhaseOfAnEntryRaisedSinceIsRefused"}},
	{file: "internal/phase/phase.go", fn: "Of"}: {calls: 1, tests: []string{"TestArchitectUnchangedByItsCompletionAtAdmitted"}},
	// The role of the issue's running step at its phase, and of the phase where no step runs at it.
	{file: "internal/projection/project.go", fn: "Project"}: {calls: 2, tests: []string{
		"TestProjectShowsARootsArchitectHandshake", "TestTheServedStateNamesTheRunningDeploymentRole"}},
	{file: "internal/record/design_news.go", fn: "untold"}: {calls: 1, tests: []string{"TestOwedNewsOwesEachHolderTheVersionsItHasNotTaken"}},
	// Whether queued work stands at a running step's phase, away from step, where it reads only
	// whether the step is held and never its role; at admitted, where no step runs, the issue's own
	// phase answers, as it did before running steps were read.
	{file: "internal/record/record.go", fn: "Issue.HoldsQueued"}: {calls: 1, tests: []string{
		"TestAQueuedStartIsHeldToTheRunningStepOfItsRole", "TestAnOwedStartOfAnotherStepsRoleIsEnded",
		"TestAStartOfAnotherStepsRoleFinishesWithoutActing", "TestALateStartWhoseStepTheTaskLeftSpawnsNoProcess",
		"TestPhaseHoldsAnswersFromTheRoleOfTheRunningStep", "TestOnlyThePhaseOwnersTaskAdoptsTheWorkingCopy",
		"TestAnIssueThatComesBackIsNotStrandedByTheStartFence"}},
	// The role of the phase of an issue with no plan rows.
	{file: "internal/record/record.go", fn: "RunningOf"}: {calls: 1, tests: []string{
		"TestAnIssueReadsTheRoleOfItsRunningStep"}},
	// The role of a row's retro or merge, and of a step whose role runs as a phase of Legion's own.
	{file: "internal/record/record.go", fn: "stepOfRow"}: {calls: 1, tests: []string{
		"TestAnIssueReadsEachOfItsRunningSteps", "TestTheStepThatWaitedLongestForItsAgentIsNext"}},
	{file: "internal/workflow/architect_start.go", fn: "writeCarriedPhase"}: {calls: 1, tests: []string{
		"TestTheArchitectTaskOfEachShape", "TestTheWorkflowsOtherReadersNameTheRunningDeploymentRole"}},
	// The role the return out of awaiting_children starts: by the held start's phase on an issue
	// with no plan rows, and by the place of the walk on a task with them.
	{file: "internal/workflow/awaiting_children.go", fn: "Engine.ReturnFromChildren"}: {calls: 2, tests: []string{
		"TestAParentWhosePullRequestMergedGoesBackToTheStartItHeld", "TestAReturnOnAWalkWithNoTesterStartsTheReviewer"}},
	{file: "internal/workflow/design_news.go", fn: "memberWords"}: {calls: 1, tests: []string{
		"TestTheRootArchitectsSummaryNamesEachMemberAsTheRecordStandsAtItsSend"}},
	// The role of the held step, and of the phase the hold is from where no step stands there.
	{file: "internal/workflow/effects.go", fn: "HeldNotice"}: {calls: 2, tests: []string{
		"TestACarriedRootResumesFromTheApprovedHeadAndTheHold", "TestAHeldStepOfTheDeploymentNamesItsRole"}},
	{file: "internal/workflow/effects.go", fn: "RoleFor"}: {calls: 1, tests: []string{"TestRoleForAnswersEveryPhaseOfPhaseAll"}},
	// The role of the held step a retry restarts: of a retry naming a claim, read past the refusal
	// of a plan's own hold, and of the restart of the phase the hold is from.
	{file: "internal/workflow/engine.go", fn: "heldStepRole"}: {calls: 1, tests: []string{
		"TestARestartNamingAClaimRestartsNoStepAnotherClaimWorks",
		"TestTheOperatorsRestartOfAPhaseStartsItsWorkerWithThePhasesTask", "TestARetryOfADeploymentStepRestartsItsRole",
		"TestAnIssueHeldAtTheReleaseIsRetriedAsBefore"}},
	// The role the move starts, read by the phase on an issue with no plan rows and by the place on
	// a task with them.
	{file: "internal/workflow/engine.go", fn: "Engine.transition"}: {calls: 2, tests: []string{
		"TestAnApprovalMovesTheIssueIntoAcceptingStartingNobody", "TestAnIssueAtAdmittedRunsNoStep",
		"TestADeploymentRoleOfAnyTypeStartsAndReports"}},
	// The requester's role, twice, and the roles of the two earlier places that run as step.
	{file: "internal/workflow/engine.go", fn: "backwardRefusal"}: {calls: 4, tests: []string{
		"TestArchitectUnchangedByABackwardMoveFromAdmitted", "TestABackwardMoveToStepNamesWhichStep", "TestADeploymentRoleMovesItsTaskBack"}},
	// The retro's role, the merge's, and the role a step's role runs as.
	{file: "internal/workflow/line.go", fn: "Walk.RoleAt"}: {calls: 3, tests: []string{"TestADeploymentRoleOfAnyTypeStartsAndReports"}},
	// The running steps a settlement of a held issue's checks tells, at the phase its hold is
	// from (record.Issue.RunningAt). A hold from admitted names the architect's role, whom
	// tellChecks tells as the issue's owner too.
	{file: "internal/workflow/review.go", fn: "Engine.checksSteps"}: {calls: 1, tests: []string{
		"TestASettlementMovesNothingAndTellsTheThreeRoles", "TestASettlementOfAHeldIssueTellsTheWorkerOfItsHeldPhase"}},
	// The role whose step a move ends (Engine.transition, Engine.arrive, Engine.holdBeforeRetro,
	// endOfTransition and, at the wait on a parent's open children, holdForChildren, StopForChildren
	// and samePhaseExempt read it), read by the place on a task with plan rows and by the phase on
	// an issue with none.
	{file: "internal/workflow/line.go", fn: "Walk.endingRole"}: {calls: 2, tests: []string{
		"TestThePhaseFinishedNoticeOutOfAdmittedCarriesNoRole", "TestAHeldStepOfTheDeploymentNamesItsRole"}},
	{file: "internal/workflow/merge_forward.go", fn: "Engine.mergeForward"}: {calls: 1, tests: []string{
		"TestAConflictKeepsPhaseAndAsksTheImplementerToMergeForward", "TestAMergeForwardAtAStepOfADeploymentRoleTellsThatRole"}},
	{file: "internal/workflow/merge_forward.go", fn: "Engine.mergeForwardLanded"}: {calls: 1, tests: []string{
		"TestAConflictKeepsPhaseAndAsksTheReviewerToReadTheMerge", "TestAMergeForwardAtAStepOfADeploymentRoleTellsThatRole"}},
	// The role of a stopped task's first step where it starts again, whatever the plan's count of
	// tasks.
	{file: "internal/workflow/plan_stop.go", fn: "Engine.startAtFirstStep"}: {calls: 1, tests: []string{
		"TestATodoOnAStoppedRootStartsItsTaskAtStepOne", "TestARetryOfADeploymentStepRestartsItsRole",
		"TestAStoppedTaskStartsAgainOnlyWhereItsNeedsAreDone"}},
	// The role of the step the stop ended, and of the phase where no step of the task ran.
	{file: "internal/workflow/plan_stop.go", fn: "Engine.taskStopped"}: {calls: 2, tests: []string{
		"TestStopTaskHoldsTheIssueAndStopsTheStep", "TestAStopReachesTheDeploymentRoleThatRuns"}},
	// The roles of the steps at a phase, a failed claim's (Engine.claimFailed), a late report's
	// (Engine.leftItsPhase) and an operator's restart's (Engine.restartPhase) through worksAt.
	{file: "internal/workflow/plan_stop.go", fn: "rolesAt"}: {calls: 1, tests: []string{
		"TestAnArchitectsClaimFailedHoldsNoIssueAndWritesNoHeldNotice", "TestAHeldStepOfTheDeploymentNamesItsRole",
		"TestArchitectUnchangedByAWorkersCompletionAtAdmitted", "TestALateCompletionBeforeADeploymentStepIsTakenAndMovesNothing",
		"TestTheOperatorsRestartOfAPhaseStartsItsWorkerWithThePhasesTask", "TestARetryOfADeploymentStepRestartsItsRole"}},
	// The role of the step a task whose start waited stands at: the end of its join, its join again
	// at the issue's generation, and the hand-on of its freed agent each start it through here.
	{file: "internal/workflow/run_end.go", fn: "Engine.startAfterWait"}: {calls: 1, tests: []string{
		"TestAFreedAgentTakesTheStepThatWaitedLongest", "TestAnAgentFreedByAMoveToANextStepGoesToItsWaiter",
		"TestTheEndOfAJoinStartsTheStepThatWaits", "TestTheEndOfAJoinWhoseAgentIsBusyWaitsForTheAgent",
		"TestAJoinThatWaitedAtAReadmissionStartsAgain"}},
	{file: "internal/workflow/retro_hold.go", fn: "Engine.StartHeldRetro"}: {calls: 1, tests: []string{
		"TestAParentWithNoAcceptanceIsHeldBeforeItsRetro", "TestAHeldRetroAfterADeploymentStepGetsItsReport"}},
	{file: "internal/workflow/retro_hold.go", fn: "retroHeldNotice"}: {calls: 1, tests: []string{"TestAParentWithNoAcceptanceIsHeldBeforeItsRetro"}},
	{file: "internal/workflow/standing.go", fn: "reportsBefore"}:     {calls: 1, tests: []string{"TestATaskNamesItsWorkingCopyItsStepAndTheReportsBeforeIt"}},
	// The role of the first step of a task whose needs are done.
	{file: "internal/workflow/tasks.go", fn: "Engine.startReady"}: {calls: 1, tests: []string{"TestEachTaskStartsWhenItsNeedsAreDone"}},
	{file: "internal/workflow/steps.go", fn: "Engine.startAgent"}: {calls: 1, tests: []string{
		"TestAnIssueAtAdmittedRunsNoStep", "TestTheStartOfADeploymentStepNamesItsAgent"}},
	{file: "internal/workflow/steps.go", fn: "Engine.upgradedStart"}: {calls: 2, tests: []string{
		"TestAnIssueAtAdmittedRunsNoStep", "TestATaskMovesToItsRowsPlaceAndCountsItsReturn"}},
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
		if len(pinned.tests) == 0 {
			t.Errorf("%s's %s names no behaviour test: name the test that holds what it answers for admitted", site.file, site.fn)
		}
		for _, test := range pinned.tests {
			if !declared[test] {
				t.Errorf("%s's %s names the behaviour test %s, which no test file of the daemon declares", site.file, site.fn, test)
			}
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

// readsRoleFor is whether node reads phase.RoleFor or one of its forwarders: internal/workflow's
// RoleFor, a selector naming either through its package, in a call or passed as a value, or a call
// of the bare name inside the package that declares it; internal/workflow's Walk.RoleAt, which
// answers phase.RoleFor of every place but a step of a deployment's role, and so the architect at
// the owner's acceptance, a selector of RoleAt on any value; and record.Issue.RunningAt, whose
// steps carry the role of their phase (record.RunningOf: the architect at admitted for an issue
// with no plan rows, and at the owner's acceptance), a call of RunningAt on any value but one of
// phase.Step, where a step names only the role a plan gives it and never the architect. A bare
// RoleFor that is no call is a field key and not a read, and appauth.AppRoleFor is another
// function and is not one of these.
func readsRoleFor(node ast.Node) bool {
	switch read := node.(type) {
	case *ast.CallExpr:
		if name, ok := read.Fun.(*ast.Ident); ok {
			return name.Name == "RoleFor"
		}
		if selector, ok := read.Fun.(*ast.SelectorExpr); ok && selector.Sel.Name == "RunningAt" {
			return len(read.Args) != 1 || !isStep(read.Args[0])
		}
	case *ast.SelectorExpr:
		if read.Sel.Name == "RoleAt" {
			return true
		}
		pkg, ok := read.X.(*ast.Ident)
		return ok && (pkg.Name == "phase" || pkg.Name == "workflow") && read.Sel.Name == "RoleFor"
	}
	return false
}

// isStep is whether expr is phase.Step.
func isStep(expr ast.Expr) bool {
	selector, ok := expr.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := selector.X.(*ast.Ident)
	return ok && pkg.Name == "phase" && selector.Sel.Name == "Step"
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
