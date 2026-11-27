package admit

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/plan"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// appliedFactKinds is every fact type the workflow's and admission's entry functions act on, read
// from their source: the cases of the engine's switch (workflow's Engine.apply) and the types
// admission's applyFact asserts. A fact type added to either is in this list the moment it is
// added, so the table below cannot miss it.
func appliedFactKinds(t *testing.T) []string {
	t.Helper()
	kinds := map[string]bool{}
	collect := func(file, receiver, function string) {
		t.Helper()
		parsed, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		found := false
		for _, decl := range parsed.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Name.Name != function || fn.Recv == nil || len(fn.Recv.List) != 1 {
				continue
			}
			star, ok := fn.Recv.List[0].Type.(*ast.StarExpr)
			if !ok || star.X.(*ast.Ident).Name != receiver {
				continue
			}
			found = true
			ast.Inspect(fn.Body, func(node ast.Node) bool {
				var types []ast.Expr
				switch node := node.(type) {
				case *ast.CaseClause:
					types = node.List
				case *ast.TypeAssertExpr:
					types = []ast.Expr{node.Type}
				}
				for _, typ := range types {
					if sel, ok := typ.(*ast.SelectorExpr); ok && sel.X.(*ast.Ident).Name == "intake" {
						kinds[sel.Sel.Name] = true
					}
				}
				return true
			})
		}
		if !found {
			t.Fatalf("%s has no (*%s).%s: the list of fact kinds is read from it", file, receiver, function)
		}
	}
	collect("../workflow/engine.go", "Engine", "apply")
	collect("admit.go", "Admission", "applyFact")
	var list []string
	for kind := range kinds {
		list = append(list, kind)
	}
	sort.Strings(list)
	if len(list) < 40 {
		t.Fatalf("read %d fact kinds (%v), want the engine's and admission's whole lists", len(list), list)
	}
	return list
}

// stepPlaces is where each phase stands in the task default, as the records Dispatch receives
// name it: the kind and the place in the task. A phase not here runs no step.
var stepPlaces = map[phase.Phase]record.StepPlace{
	phase.Planning:     {Task: "default", Kind: "step", Place: 1},
	phase.Implementing: {Task: "default", Kind: "step", Place: 2},
	phase.Testing:      {Task: "default", Kind: "step", Place: 3},
	phase.Reviewing:    {Task: "default", Kind: "step", Place: 4},
	phase.Accepting:    {Task: "default", Kind: "step", Place: 5},
	phase.Retro:        {Task: "default", Kind: "retro", Place: 0},
	phase.Merging:      {Task: "default", Kind: "merge", Place: 0},
}

// standsAt is the place of the step issue stands in, as the end rule states it (workflow's
// standingStep): the step of the phase it stands in (record.StandsAt) on the task default, and none
// for an issue its plan holds, a stopped task's or a parent held for its children, or for a phase
// that runs no step.
func standsAt(issue record.Issue) (record.StepPlace, bool) {
	if issue.Hold != nil && (issue.Hold.Reason == record.HoldTaskStopped || issue.Hold.Reason == record.HoldForChildren) {
		return record.StepPlace{}, false
	}
	place, ok := stepPlaces[record.StandsAt(issue)]
	return place, ok
}

// runsTheDefaultTask writes the plan rows key holds as an issue of this release whose owner gave no
// plan (workflow's defaultRows, written at its first step): the task default of today's steps,
// running at the step of the phase key stands in.
func runsTheDefaultTask(key string) func(*testing.T, *pgxpool.Pool, *Admission, intake.Handler) {
	return func(t *testing.T, pool *pgxpool.Pool, _ *Admission, _ intake.Handler) {
		t.Helper()
		inTx(t, pool, func(tx pgx.Tx) {
			ctx, records := context.Background(), record.NewStore()
			stored, err := records.Issue(ctx, tx, key)
			if err != nil || stored == nil {
				t.Fatalf("read %s: %v", key, err)
			}
			at, ok := standsAt(*stored)
			if !ok {
				t.Fatalf("%s stands in %s, which runs no step", key, record.StandsAt(*stored))
			}
			roles := plan.BuiltinRoles()
			var steps []record.PlanStep
			for i, step := range (config.Project{}).RunsByDefault() {
				steps = append(steps, record.PlanStep{Issue: key, Generation: stored.Generation, Task: "default", Position: i + 1, Role: step.Role,
					AgentKey: step.Key, Type: roles[step.Role].Type})
			}
			v := record.PlanVersion{Issue: key, Generation: stored.Generation, Source: record.PlanSourceDefault, GivenAt: fixedNow}
			task := record.PlanTask{Issue: key, Generation: stored.Generation, Task: "default", BlockText: "The project's default steps.",
				StepsByDefault: true, State: record.TaskWaiting}
			for _, put := range []error{
				records.PutPlan(ctx, tx, v, []record.PlanTask{task}, steps),
				records.MoveTask(ctx, tx, key, stored.Generation, "default", record.TaskPlace{Kind: record.PlaceStep, Position: at.Place}, record.TaskRunning),
			} {
				if put != nil {
					t.Fatalf("write the task default of %s: %v", key, put)
				}
			}
		})
	}
}

// assertNoRunOpenWhereItsIssueNoLongerStands fails, naming what, for each run the store holds open
// that is over: its issue stands at another place or in another generation, or its tree's run is
// over — the root lingers — and the run is not a merge that can still land in that tree (the
// linger still runs, the pull request is open, or the linger's close handed the member over).
// lingerClosed says the fact closed the linger, which ends its window whatever the clock says.
// It states the end rule itself rather than calling workflow's (runOver, mergeCanLand): a check
// that read the rule it judges would agree with any fault in it. With mergeCanLand made to answer
// true for every merge, this check fails a linger close that leaves a merge open.
func assertNoRunOpenWhereItsIssueNoLongerStands(t *testing.T, pool *pgxpool.Pool, what string, now time.Time, lingerClosed bool) {
	t.Helper()
	rows, err := pool.Query(context.Background(), `select issue, task, kind, place, coalesce(record->>'generation', '') from step_starts where run = 'open' order by issue, kind, place`)
	if err != nil {
		t.Fatalf("read the open runs: %v", err)
	}
	type open struct {
		issue      string
		place      record.StepPlace
		generation string
	}
	var runs []open
	for rows.Next() {
		var run open
		if err := rows.Scan(&run.issue, &run.place.Task, &run.place.Kind, &run.place.Place, &run.generation); err != nil {
			t.Fatalf("scan an open run: %v", err)
		}
		runs = append(runs, run)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("read the open runs: %v", err)
	}
	inTx(t, pool, func(tx pgx.Tx) {
		ctx, records := context.Background(), record.NewStore()
		for _, run := range runs {
			stands, err := records.Issue(ctx, tx, run.issue)
			if err != nil {
				t.Fatalf("read %s: %v", run.issue, err)
			}
			if stands == nil {
				t.Errorf("%s: the run of %s at %+v is open, and the issue has no record", what, run.issue, run.place)
				continue
			}
			at := record.StandsAt(*stands)
			if place, ok := standsAt(*stands); !ok || place != run.place || strconv.FormatUint(stands.Generation, 10) != run.generation {
				t.Errorf("%s: the run of %s at %+v of generation %s is open, and the issue stands in %s at generation %d",
					what, run.issue, run.place, run.generation, at, stands.Generation)
				continue
			}
			root, err := records.Issue(ctx, tx, stands.Tree)
			if err != nil {
				t.Fatalf("read the root of %s: %v", run.issue, err)
			}
			if root == nil || !root.Lingers() {
				continue
			}
			mergeCanLand := false
			if stands.Key != root.Key && (stands.Phase == phase.Merging || stands.Phase == phase.Held && stands.Hold != nil && stands.Hold.From == phase.Merging) {
				pr, err := records.PullRequest(ctx, tx, stands.Key)
				if err != nil {
					t.Fatalf("read the pull request of %s: %v", run.issue, err)
				}
				handover, err := records.Handover(ctx, tx, stands.Key)
				if err != nil {
					t.Fatalf("read the hand-over of %s: %v", run.issue, err)
				}
				mergeCanLand = record.Lingering(*root, now) && !lingerClosed || pr != nil && pr.State == record.PullRequestOpen ||
					handover != nil && handover.Tree == root.Tree && handover.Generation == root.Generation
			}
			if !mergeCanLand {
				t.Errorf("%s: the run of %s at %+v is open, and its tree's root %s lingers", what, run.issue, run.place, root.Key)
			}
		}
	})
}

// runningFinishingTree is the finishing tree (seedFinishingTree) with each issue that stands in a
// step running it: a start of this release counted for each, as the transition or the promotion
// that started it writes.
func runningFinishingTree(t *testing.T) (*pgxpool.Pool, *Admission, intake.Handler) {
	t.Helper()
	pool, admission, engine := seedFinishingTree(t)
	for _, key := range []string{finishedRoot, planning, heldTesting, inRetro, awaiting, inAccepting} {
		countStart(t, pool, engine, key)
	}
	return pool, admission, engine
}

// The end of a step's run reads what the fact changed and no other issue's runs: a fact that takes
// one issue out of its step makes as many statements reading as many rows with 200 runs open in
// the store, each of an issue of its own tree standing in its step, as with 2. The fact ends the
// one run it should, so the measure is of a settle that ran.
func TestTheEndOfARunReadsNoOtherIssuesRuns(t *testing.T) {
	type cost struct{ statements, rows int64 }
	measure := func(t *testing.T, others int) cost {
		t.Helper()
		pool, _, engine := runningFinishingTree(t)
		inTx(t, pool, func(tx pgx.Tx) {
			ctx, records := context.Background(), record.NewStore()
			for i := range others {
				key := fmt.Sprintf("LEGION-%d", 6000+i)
				other := record.Issue{Key: key, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: key, Project: testProject,
					Title: key, Phase: phase.Testing, Generation: 1, Status: "testing", DispatchStatus: "testing", Rank: "Z", LastDispatchSeq: 1}
				if err := records.PutIssue(ctx, tx, other); err != nil {
					t.Fatalf("seed %s: %v", key, err)
				}
				if _, err := records.RaiseStepStarts(ctx, tx, key, stepPlaces[phase.Testing], func(starts int64) record.StepRecord {
					return record.StepRecord{ID: fmt.Sprintf("%s-%d", key, starts), Task: "default", Step: 3, Kind: "step", Role: "tester", Type: "testing", Generation: 1}
				}); err != nil {
					t.Fatalf("open the run of %s: %v", key, err)
				}
			}
		})
		counter := &statementCounter{}
		counted := countedPool(t, pool, counter)
		if _, err := intake.ApplyFact(context.Background(), counted, "dispatch", "park-planning", intake.DispatchIssue{Key: planning, ResolvedRepo: "acme/widgets", Seq: 4,
			Type: "issue.updated", Status: "backlog", Title: planning, Parent: finishedRoot, Rank: "B", HandedOver: true}, engine); err != nil {
			t.Fatalf("park %s: %v", planning, err)
		}
		measured := cost{counter.sent.Load(), counter.rows.Load()}
		assertStepWrites(t, pool, planning, start("step", 1, 1, 1), end("stopped", "step", 1, 1, 1))
		var open int
		if err := pool.QueryRow(context.Background(), `select count(*) from step_starts where run = 'open' and issue ~ '^LEGION-6[0-9]{3}$'`).Scan(&open); err != nil || open != others {
			t.Fatalf("open runs of the other issues = %d, %v; want all %d still open", open, err, others)
		}
		return measured
	}
	few, many := measure(t, 2), measure(t, 200)
	if few != many {
		t.Errorf("parking one issue made %d statements reading %d rows with 200 other runs open, and %d reading %d with 2; want as many",
			many.statements, many.rows, few.statements, few.rows)
	}
}

// parkFinishingTree parks the running tree's root, a person's: it leaves the workflow and its
// tree lingers.
func parkFinishingTree(t *testing.T, pool *pgxpool.Pool, admission *Admission, engine intake.Handler) {
	t.Helper()
	apply(t, pool, admission, "root-parked", intake.DispatchIssue{Key: finishedRoot, ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.updated", Status: "backlog", Title: finishedRoot, Rank: "A", HandedOver: true}, engine)
}

// wayOut is a way out of a step that neither the workflow nor admission knows: a handler of its
// own that takes the planning child out of the workflow by a bare write of its record.
type wayOut struct{}

func (wayOut) Apply(ctx context.Context, tx pgx.Tx, _ intake.Fact) (intake.Result, error) {
	records := record.NewStore()
	child, err := records.Issue(ctx, tx, planning)
	if err != nil || child == nil {
		return intake.Result{}, err
	}
	child.Phase, child.Status = phase.Done, "backlog"
	return intake.Result{}, records.PutIssue(ctx, tx, *child)
}

// Every way out of a step ends its run, whatever fact takes the issue out and whatever writes it:
// the end follows from the record at the one point every fact passes, not from a call each way
// out must remember. For each kind of fact the workflow and admission apply, on a tree whose
// issues each run a step, no run is left open where its issue no longer stands, nor in a tree whose
// run is over, but for a merge that can still land there. Each row that takes an issue out says
// which, and the row fails when the fact did not take it out, so a row cannot quietly stop testing
// its way out. A row whose fact is no way out, though it acts on an issue standing in a step, says
// which issue it keeps there, and fails when that issue's step moved or its run ended. A kind of
// fact the table has no row for fails by name.
func TestEveryWayOutOfAStepEndsItsRun(t *testing.T) {
	type row struct {
		name   string
		source string
		// arrange brings the running tree to the state the fact applies on.
		arrange func(t *testing.T, pool *pgxpool.Pool, admission *Admission, engine intake.Handler)
		fact    intake.Fact
		// leaves is the issue the fact takes out of the step it stands in, "" for none.
		leaves string
		// keeps is the issue the fact acts on and leaves in the step it stands in, its run still
		// open, "" for none, and then what the fact must have written on that issue's pull request,
		// nil for nothing to check.
		keeps        string
		then         func(t *testing.T, pr record.PullRequest)
		lingerClosed bool
	}
	none := func(*testing.T, *pgxpool.Pool, *Admission, intake.Handler) {}
	parked := parkFinishingTree
	closed := closeFinishingTree
	issueFact := func(key, status string) intake.DispatchIssue {
		fact := intake.DispatchIssue{Key: key, ResolvedRepo: "acme/widgets", Seq: 4, Type: "issue.updated", Status: status, Title: key, Rank: "B", HandedOver: true}
		if key != finishedRoot {
			fact.Parent = finishedRoot
		}
		return fact
	}
	moves := map[string][]row{
		"DispatchIssue": {
			{name: "a person parks a child", arrange: none, fact: issueFact(planning, "backlog"), leaves: planning},
			{name: "a person parks the root, and its tree lingers", arrange: none, fact: issueFact(finishedRoot, "backlog"), leaves: finishedRoot},
			{name: "a person sets a running child back to todo", arrange: none, fact: issueFact(planning, "todo"), leaves: planning},
			{name: "a person reopens the closed root, and the tree is promoted again", arrange: closed, fact: issueFact(finishedRoot, "todo")},
		},
		"BackwardMove": {{name: "the implementer moves its retro back", arrange: none,
			fact: intake.BackwardMove{Issue: inRetro, Requester: claim.RoleImplementer, To: phase.Implementing, Reason: "redo"}, leaves: inRetro}},
		// The route builds each architect's request with its owner and the run it read (api's
		// signOff): here the tree's root architect, and the run the start counted.
		"SignOff": {{name: "the architect accepts a child", arrange: none, fact: intake.SignOff{Issue: inAccepting, By: ownersArchitect(t, finishedRoot), Generation: 1,
			Entry: record.StepEntry{Place: stepPlaces[phase.Accepting], Starts: 1}}, leaves: inAccepting}},
		// A first plan is taken only before the issue's work (workflow's planGiven), so a plan of a
		// child whose step runs is refused, and the child keeps its step. The stop of the task a
		// child of this release runs, the task default its first step wrote, holds the child for
		// the stopped task, which stands in no step.
		"PlanGiven": {{name: "an owner's plan of a child whose work started is refused and keeps its step", arrange: none,
			fact: intake.PlanGiven{Issue: planning, Generation: 1, GivenBy: ownersArchitect(t, finishedRoot), Plan: plan.Plan{ArtifactID: "plan-artifact", Version: 1,
				Tasks: []plan.Task{{ID: "a", Steps: []plan.Step{{Role: "implementer"}}, Text: "Build the widget."}}}}, keeps: planning}},
		"TaskStopped": {{name: "the owner stops the task of a child in planning", arrange: runsTheDefaultTask(planning),
			fact: intake.TaskStopped{Issue: planning, Task: "default", By: ownersArchitect(t, finishedRoot), Generation: 1}, leaves: planning}},
		"PullRequestMerged": {{name: "a member's merge lands while its tree lingers", arrange: func(t *testing.T, pool *pgxpool.Pool, admission *Admission, engine intake.Handler) {
			seedAwaitingPullRequest(t, pool)
			parked(t, pool, admission, engine)
		}, source: "github", fact: intake.PullRequestMerged{Repo: "acme/widgets", Number: 5, MergeSHA: "merge-5", HeadSHA: "head-5"}, leaves: awaiting}},
		"LingerExpired": {{name: "the linger's close stops a merge that can no longer land", arrange: parked, source: "timer",
			fact: intake.LingerExpired{Issue: finishedRoot, Generation: lingerOfRoot}, lingerClosed: true}},
		// A head that conflicts with its base moves no issue (dispatch://INFRA-4374): the second
		// read that confirms the conflict asks the implementer to merge the base forward while the
		// merger keeps merging's step (workflow's mergeForward, which writes no transition), and
		// the one that answers no state releases the first read (workflow's conflictConfirmDone).
		// Neither ends the step's run.
		"PullRequestMergeability": {{name: "a confirmed conflict asks the merge forward and keeps the step", arrange: conflictReadOfAwaiting,
			source: "github", fact: intake.PullRequestMergeability{Repo: "acme/widgets", Number: 5, Base: "main", HeadSHA: "head-5",
				Mergeable: record.MergeabilityConflicting, MergeState: record.MergeStateDirty, ReadAt: fixedNow,
				Rules: &record.BaseRules{Required: []string{"ci"}}}, keeps: awaiting, then: func(t *testing.T, pr record.PullRequest) {
				if pr.MergeForward == nil || pr.MergeForward.Head != "head-5" || pr.MergeForward.Phase != phase.Merging {
					t.Fatalf("the merge forward of %s = %+v, want one asked of head-5 in merging", awaiting, pr.MergeForward)
				}
			}}},
		"ConflictConfirmDone": {{name: "a second read that answers no state releases the first and keeps the step", arrange: conflictReadOfAwaiting,
			source: "outbox", fact: intake.ConflictConfirmDone{Issue: awaiting, Number: 5, Head: "head-5", FirstReadAt: awaitingFirstRead}, keeps: awaiting,
			then: func(t *testing.T, pr record.PullRequest) {
				if pr.ConflictRead != nil || pr.MergeForward != nil {
					t.Fatalf("%s's pull request holds first read %+v and merge forward %+v, want the first read released and nothing asked", awaiting, pr.ConflictRead, pr.MergeForward)
				}
			}}},
		"ControllerTick": {
			{name: "applied to the running tree", arrange: none, fact: intake.ControllerTick{}},
			// A Dispatch child opened under a member in the middle of its phase holds it: the pass
			// moves it to awaiting_children, which runs no step (dispatch://INFRA-3320 R9).
			{name: "an open child stops a member in the middle of its phase", arrange: func(t *testing.T, pool *pgxpool.Pool, _ *Admission, _ intake.Handler) {
				spMirror(t, pool, "LEGION-777", "todo", planning, 1)
			}, source: "controller", fact: intake.ControllerTick{}, leaves: planning},
		},
	}
	kinds := appliedFactKinds(t)
	for _, kind := range kinds {
		if _, ok := moves[kind]; !ok {
			moves[kind] = []row{{name: "applied to the running tree", arrange: none, fact: zeroFact(t, kind)}}
		}
	}
	for kind := range moves {
		if !slices.Contains(kinds, kind) {
			t.Errorf("the table has a row for %s, which neither the workflow nor admission applies", kind)
		}
	}
	for _, kind := range kinds {
		for _, tc := range moves[kind] {
			t.Run(kind+"/"+tc.name, func(t *testing.T) {
				pool, admission, engine := runningFinishingTree(t)
				tc.arrange(t, pool, admission, engine)
				var before, kept record.Issue
				if tc.leaves != "" {
					before = issue(t, pool, tc.leaves)
				}
				if tc.keeps != "" {
					kept = issue(t, pool, tc.keeps)
				}
				source := tc.source
				if source == "" {
					source = "api"
				}
				if _, err := intake.ApplyFact(context.Background(), pool, source, "the-way-out", tc.fact, engine, admission); err != nil {
					t.Fatalf("apply %s: %v", kind, err)
				}
				if tc.leaves != "" {
					after := issue(t, pool, tc.leaves)
					if at, _ := standsAt(after); at == stepPlaces[record.StandsAt(before)] && after.Generation == before.Generation {
						t.Fatalf("%s left %s in %s at generation %d, want it out of the step it stood in", kind, tc.leaves, record.StandsAt(after), after.Generation)
					}
				}
				if tc.keeps != "" {
					after := issue(t, pool, tc.keeps)
					if record.StandsAt(after) != record.StandsAt(kept) || after.Generation != kept.Generation {
						t.Fatalf("%s moved %s from %s to %s, want it kept in the step it stood in", kind, tc.keeps, record.StandsAt(kept), record.StandsAt(after))
					}
					var open int
					if err := pool.QueryRow(context.Background(), `select count(*) from step_starts where issue = $1 and run = 'open'`, tc.keeps).Scan(&open); err != nil {
						t.Fatalf("read the open runs of %s: %v", tc.keeps, err)
					}
					if open != 1 {
						t.Fatalf("%s left %s with %d open runs, want the run of the step it stands in still open", kind, tc.keeps, open)
					}
					t.Logf("%s kept %s in %s with its run open", kind, tc.keeps, record.StandsAt(after))
					if tc.then != nil {
						inTx(t, pool, func(tx pgx.Tx) {
							pr, err := record.NewStore().PullRequest(context.Background(), tx, tc.keeps)
							if err != nil || pr == nil {
								t.Fatalf("read the pull request of %s = %v, %v", tc.keeps, pr, err)
							}
							tc.then(t, *pr)
						})
					}
				}
				assertNoRunOpenWhereItsIssueNoLongerStands(t, pool, kind+"/"+tc.name, fixedNow, tc.lingerClosed)
			})
		}
	}
	t.Run("a way out neither the workflow nor admission knows", func(t *testing.T) {
		pool, admission, engine := runningFinishingTree(t)
		if _, err := intake.ApplyFact(context.Background(), pool, "controller", "the-unknown-way-out", intake.ControllerTick{}, engine, admission, wayOut{}); err != nil {
			t.Fatalf("apply the unknown way out: %v", err)
		}
		if got := issue(t, pool, planning); got.Phase != phase.Done {
			t.Fatalf("the unknown way out left %s in %s, want it done", planning, got.Phase)
		}
		assertNoRunOpenWhereItsIssueNoLongerStands(t, pool, "the unknown way out", fixedNow, false)
	})
}

// zeroFact is the zero value of the fact type kind names, for a kind the table applies to the
// running tree as it is: it moves nothing of it, and the tree's runs must stay as they were.
func zeroFact(t *testing.T, kind string) intake.Fact {
	t.Helper()
	for _, fact := range []intake.Fact{
		intake.GateRegistered{}, intake.DispatchArtifact{}, intake.GateAboveDispatch{}, intake.GateRequestFound{},
		intake.CatchUpApplied{}, intake.DispatchAskOpened{}, intake.DispatchAskClosed{}, intake.DispatchAskReply{},
		intake.DispatchApprovalMoved{}, intake.DispatchAskRestored{}, intake.HandoffComplete{}, intake.PullRequestOpened{},
		intake.PullRequestSynchronized{}, intake.Push{}, intake.BasePush{}, intake.PullRequestChecks{},
		intake.PullRequestMergeability{}, intake.PullRequestReview{}, intake.PullRequestClosed{}, intake.PullRequestDequeued{},
		intake.PullRequestReviewRequests{}, intake.PullRequestMergeStateUnread{},
		intake.MergeQueueReadDone{}, intake.PullRequestMissing{}, intake.ClaimReady{}, intake.ClaimFailed{},
		intake.SessionLost{}, intake.SessionStoreFault{}, intake.MemoryGuardKill{}, intake.VolumeNearFull{}, intake.RetryOrEscalate{},
		intake.RestartPhase{}, intake.SignOff{}, intake.DaemonBooted{}, intake.CutoverMergerRepair{},
		intake.CheckReport{}, intake.CheckLookEnded{}, intake.CheckWakeDue{}, intake.CheckBound{},
		intake.ListenerWake{}, intake.PodBackstopDue{}, intake.WorkerSilent{}, intake.WorkerBackgroundStalled{},
		intake.ControllerTick{}, intake.ChildAdoption{}, intake.DispatchConsumerPosition{}, intake.EntrySweep{},
		intake.Handover{}, intake.CloseRoot{}, intake.MergeHoldsChanged{Project: "LEGION", Event: "dispatch-71"}, intake.ControllerMemoryGuardKill{}, intake.PendingBasisRead{},
		intake.BasisChangesRead{}, intake.ReparentRead{}, intake.CloseChecksRead{}, intake.DispatchResultChanged{},
		// A change of the P0 bound, which records it and makes the pass every fact that is not a
		// Dispatch issue makes.
		intake.P0BoundChanged{Change: store.P0BoundChange{Mode: store.P0BoundUnbounded, By: store.PauseActor{Kind: store.PauseByOperator}, Reason: "runaway"}},
	} {
		if name := factName(fact); name == kind {
			return fact
		}
	}
	t.Fatalf("the fact kind %s has no row in this test: add one that applies it to an issue that stands in a step", kind)
	return nil
}

// factName is the name of fact's type in package intake.
func factName(fact intake.Fact) string { return reflect.TypeOf(fact).Name() }
