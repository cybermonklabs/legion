package api

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/admit"
	"github.com/sjawhar/legion/daemon/internal/blockers"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/plan"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/testpg/testdb"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// The tests of this file give plans through register_plan to the daemon's own workflow
// (newWorkflowHarness), and read what the store holds after each give. The tree root LEGION-208
// stands at admitted; on the Dispatch chain as the daemon's copy holds it, LEGION-211 stands under
// LEGION-208, LEGION-212 under LEGION-211, and LEGION-209, the issue whose plan is given, under
// LEGION-212, so the architects of LEGION-211 and LEGION-212 act on LEGION-209 while their issues
// stand above it (record.PlanOwners). LEGION-211 and LEGION-212 are recorded at generation 1, and
// LEGION-209 too where a case records it. Dispatch holds childDoc, LEGION-209's document, at
// versions 1 to 3, each the one task a with the steps implementer and reviewer.

// planWorld is the harness of these tests: the server, the engine it applies facts with, and the
// architects of LEGION-208 (root), LEGION-211 (sub) and LEGION-212 (third).
type planWorld struct {
	h                *harness
	engine           *workflow.Engine
	root, sub, third liveClaim
}

// newPlanWorld is the world of these tests, with LEGION-211 and LEGION-212 recorded on LEGION-209's
// chain at generation 1, and LEGION-209 too where recorded, before the architects' claims are made
// (seedUnder); LEGION-209 not recorded is a backlog child of LEGION-212 in Dispatch and its copy.
func newPlanWorld(t *testing.T, recorded bool) planWorld {
	t.Helper()
	h, engine, _ := newWorkflowHarness(t, workflow.Config{Project: testProject, Repositories: testRepositories,
		Steps: config.DesignGateRootIssues.Steps()})
	seedIssueAt(t, h, "LEGION-208", phase.Admitted)
	h.handoffs.documents = map[string]string{childDoc: "LEGION-209"}
	h.handoffs.approvals = map[string]dispatch.Approval{childDoc: {State: "none", LatestVersion: 3}}
	h.handoffs.plans = map[string]dispatch.DocumentVersion{}
	parent := "LEGION-212"
	h.handoffs.issues = map[string]dispatch.Issue{
		"LEGION-208": {Key: "LEGION-208", Status: "todo", LastSeq: 7},
		"LEGION-209": {Key: "LEGION-209", Status: "backlog", Parent: &parent},
	}
	for version := 1; version <= 3; version++ {
		h.handoffs.givePlan(childDoc, version, taskBlock("a", "implementer reviewer"))
	}
	seedUnder(t, h, "LEGION-211", "LEGION-208")
	seedUnder(t, h, "LEGION-212", "LEGION-211")
	w := planWorld{h: h, engine: engine}
	if recorded {
		seedUnder(t, h, "LEGION-209", "LEGION-212")
	} else {
		w.standUnder(t, "LEGION-209", "LEGION-212", "backlog")
	}
	w.root = newLiveClaim(t, h, "LEGION-208", claim.RoleArchitect)
	w.sub = newLiveClaimIn(t, h, "LEGION-208", "LEGION-211", claim.RoleArchitect)
	w.third = newLiveClaimIn(t, h, "LEGION-208", "LEGION-212", claim.RoleArchitect)
	return w
}

// seedUnder records key at admitted in its first run, under parent, with its row of the copy of
// Dispatch's issues under parent, so the daemon reads its chain from the copy (R15 of
// dispatch://INFRA-3320).
func seedUnder(t *testing.T, h *harness, key, parent string) {
	t.Helper()
	if err := store.Transact(context.Background(), h.store.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		return record.NewStore().PutIssue(ctx, tx, record.Issue{
			Key: key, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: "LEGION-208", Project: testProject, Title: key,
			Parent: &parent, Phase: phase.Admitted, Generation: 1, Status: "in_progress",
		})
	}); err != nil {
		t.Fatalf("seed %s under %s: %v", key, parent, err)
	}
	planWorld{h: h}.standUnder(t, key, parent, "in_progress")
}

// standUnder writes key's row of the copy of Dispatch's issues under parent ("" for none) at
// status, as a reading of Dispatch does once a person moves the issue (moveCopy), at a sequence
// above every one the copy holds of key.
func (w planWorld) standUnder(t *testing.T, key, parent, status string) {
	t.Helper()
	var seq int64
	if err := w.h.store.Pool().QueryRow(context.Background(), `select coalesce(max(greatest(status_seq, parent_seq, labels_seq)), 0)
		from dispatch_issues where key = $1`, key).Scan(&seq); err != nil {
		t.Fatalf("read the copy's sequences of %s: %v", key, err)
	}
	moveCopy(t, w.h, key, parent, status, seq+1)
}

// copyRow is key's parent ("" for none) and status in the copy of Dispatch's issues.
func (w planWorld) copyRow(t *testing.T, key string) (parent, status string) {
	t.Helper()
	if err := w.h.store.Pool().QueryRow(context.Background(), `select coalesce(parent, ''), status from dispatch_issues where key = $1`,
		key).Scan(&parent, &status); err != nil {
		t.Fatalf("read the copy's row of %s: %v", key, err)
	}
	return parent, status
}

// planAnswer is register_plan's answer to version of childDoc, as the route answers a give it took
// or answers as taken: the plan given.
func planAnswer(version int) string {
	return `{"plan":{"version":` + strconv.Itoa(version) + `,"tasks":[{"id":"a","steps":["implementer","reviewer"]}]}}`
}

// taken fails unless giver's give of version of childDoc for LEGION-209 is answered 200 with the
// plan.
func (w planWorld) taken(t *testing.T, giver liveClaim, version int, why string) {
	t.Helper()
	answer := registerPlan(w.h, giver.grant(t), "LEGION-209", childDoc, version)
	if body := strings.TrimSpace(answer.Body.String()); answer.Code != http.StatusOK || body != planAnswer(version) {
		t.Fatalf("version %d given by %s = %d %s, want 200 with the plan: %s", version, giver.token, answer.Code, body, why)
	}
}

// leaves has Dispatch move key out from under LEGION-208, a root of its own, and the issue right
// under key move up to stand where key stood, so key's architect acts on LEGION-209 no more
// (record.PlanOwners) and every other architect above LEGION-209 still does.
func (w planWorld) leaves(t *testing.T, key string) {
	t.Helper()
	var under string
	if err := w.h.store.Pool().QueryRow(context.Background(), `select key from dispatch_issues where parent = $1`, key).Scan(&under); err != nil {
		t.Fatalf("read the issue under %s in the copy: %v", key, err)
	}
	above, _ := w.copyRow(t, key)
	_, status := w.copyRow(t, under)
	w.standUnder(t, under, above, status)
	_, status = w.copyRow(t, key)
	w.standUnder(t, key, "", status)
}

// record records LEGION-209 at admitted in its first run, under the parent its row of the copy
// names, as release_children records a child, and moves that row to in_progress.
func (w planWorld) record(t *testing.T) {
	t.Helper()
	parent, _ := w.copyRow(t, "LEGION-209")
	seedUnder(t, w.h, "LEGION-209", parent)
}

// firstStep starts the first step of LEGION-209 as admission's line does (workflow's
// StartFirstPhase), on the chains the copy holds, under the fact lock, in a transaction of its own
// with the commit scope a fact's application gives the engine (Engine.OpenFact, Engine.SettleFact).
func (w planWorld) firstStep(t *testing.T) {
	t.Helper()
	if err := store.Transact(context.Background(), w.h.store.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		if err := intake.SerializeFacts(ctx, tx); err != nil {
			return err
		}
		issue, err := record.NewStore().Issue(ctx, tx, "LEGION-209")
		if err != nil || issue == nil {
			return errors.Join(err, errors.New("LEGION-209 is not recorded"))
		}
		chains, err := record.ReadChainsOf(ctx, tx, record.NewStore(), issue.Project, []string{issue.Key})
		if err != nil {
			return err
		}
		ctx = w.engine.OpenFact(ctx)
		if err := w.engine.StartFirstPhase(ctx, tx, *issue, chains, nil); err != nil {
			return err
		}
		return w.engine.SettleFact(ctx, tx)
	}); err != nil {
		t.Fatalf("start the first step of LEGION-209: %v", err)
	}
}

// versionRow is one version of a plan as issue_plans holds it: its number, its document, the giver
// it names last and the givers before it.
type versionRow struct {
	Version  int
	Document string
	Giver    string
	Earlier  []string
}

// versionsOf is every version of the plan of LEGION-209 at generation, in version order.
func (w planWorld) versionsOf(t *testing.T, generation uint64) []versionRow {
	t.Helper()
	rows, err := w.h.store.Pool().Query(context.Background(), `select version, artifact_id, given_by, earlier_givers
		from issue_plans where issue = 'LEGION-209' and generation = $1 order by version`, int64(generation))
	if err != nil {
		t.Fatalf("read the versions of LEGION-209: %v", err)
	}
	versions, err := pgx.CollectRows(rows, pgx.RowToStructByPos[versionRow])
	if err != nil {
		t.Fatalf("read the versions of LEGION-209: %v", err)
	}
	for i := range versions {
		if len(versions[i].Earlier) == 0 {
			versions[i].Earlier = nil
		}
	}
	return versions
}

// tasksOf is the task of each row of plan_tasks of LEGION-209 at generation, in task order.
func (w planWorld) tasksOf(t *testing.T, generation uint64) []string {
	t.Helper()
	rows, err := w.h.store.Pool().Query(context.Background(), `select task from plan_tasks
		where issue = 'LEGION-209' and generation = $1 order by task`, int64(generation))
	if err != nil {
		t.Fatalf("read the tasks of LEGION-209: %v", err)
	}
	tasks, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read the tasks of LEGION-209: %v", err)
	}
	return tasks
}

// defaultVersion is the version the first step writes for the project's default steps: version
// 0, of no document and no giver.
var defaultVersion = versionRow{}

// ownersVersion is version of childDoc as the store holds it: giver the giver it added last
// (given_by), and earlier the givers it added before, in the order it added them.
func ownersVersion(version int, giver liveClaim, earlier ...liveClaim) versionRow {
	row := versionRow{Version: version, Document: childDoc, Giver: string(giver.token)}
	for _, before := range earlier {
		row.Earlier = append(row.Earlier, string(before.token))
	}
	return row
}

// phaseOf is the phase LEGION-209 stands in.
func (w planWorld) phaseOf(t *testing.T) phase.Phase {
	t.Helper()
	got := recordedChild(t, w.h, "LEGION-209")
	if got == nil {
		t.Fatal("LEGION-209 is not recorded")
	}
	return got.Phase
}

// notTaken is the reason of each plan-not-taken notice the outbox holds for LEGION-209.
func (w planWorld) notTaken(t *testing.T) []string {
	t.Helper()
	rows, err := w.h.store.Pool().Query(context.Background(), `select payload->>'reason' from outbox
		where issue = 'LEGION-209' and kind = 'notice' and payload->>'kind' = 'plan-not-taken' order by id`)
	if err != nil {
		t.Fatalf("read the plan-not-taken notices: %v", err)
	}
	reasons, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read the plan-not-taken notices: %v", err)
	}
	return reasons
}

// A version that a first step passed over is no duplicate of the give that was passed over, though
// a give of it again has that give's id: the store no longer holds the version, so the give is
// judged as any version is and stored, with its task a in place of the default steps' task. Here
// the architect of LEGION-211 gives version 1 for LEGION-209, Dispatch then moves LEGION-211 out
// from above it, so LEGION-209's first step passes the version over and runs the default steps,
// and the default task is stopped; then version 1 is given again at the same generation by the
// root's architect, or by LEGION-211's architect once its issue stands above LEGION-209 again.
//
// The fault it catches is a give answered as taken for its id alone, which stores nothing (its
// control: intake.ApplyFact answering a PlanGiven whose id is marked as a duplicate, under which the
// default steps stay). The second row's fault is an id that names the giver (its control: the
// route's id naming the giver's claim, with intake.ApplyFact answering a marked id as a duplicate,
// under which the first row passes and the second stores nothing).
func TestAPlanVersionPassedOverIsStoredWhenGivenAgain(t *testing.T) {
	for _, tc := range []struct {
		name  string
		giver func(w planWorld) liveClaim
		back  bool
	}{
		{"by a new owner", func(w planWorld) liveClaim { return w.root }, false},
		{"by its owner again", func(w planWorld) liveClaim { return w.sub }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newPlanWorld(t, true)
			w.taken(t, w.sub, 1, "premise: the sub-architect's version 1")
			w.leaves(t, "LEGION-211")
			w.firstStep(t)
			if got := w.versionsOf(t, 1); !reflect.DeepEqual(got, []versionRow{defaultVersion}) {
				t.Fatalf("premise: after its first step LEGION-209 holds %+v, want the default steps alone", got)
			}
			if tc.back {
				w.standUnder(t, "LEGION-211", "LEGION-208", "in_progress")
				w.standUnder(t, "LEGION-212", "LEGION-211", "in_progress")
			}
			giver := tc.giver(w)
			if stopped := stopTask(w.h, giver.grant(t), "LEGION-209", plan.DefaultTask); stopped.Code != http.StatusOK {
				t.Fatalf("stop the default steps = %d %s, want 200", stopped.Code, stopped.Body)
			}

			w.taken(t, giver, 1, "the version passed over is judged again and stored")
			if got, want := w.versionsOf(t, 1), []versionRow{defaultVersion, ownersVersion(1, giver)}; !reflect.DeepEqual(got, want) {
				t.Errorf("LEGION-209 holds %+v at generation 1, want %+v", got, want)
			}
			if got := w.tasksOf(t, 1); !slices.Equal(got, []string{"a"}) {
				t.Errorf("LEGION-209 holds the tasks %q at generation 1, want [a]: the version's task, and the default steps' task gone", got)
			}
		})
	}
}

// A give of the version in force is answered as taken, 200 with the plan, and stores no plan row,
// whoever gives it and whatever id it has: the sub-architect's version 1 given again by itself,
// which changes no row, and by the root's architect, which the version then names as its last
// giver, the one change; and version 1 given for LEGION-209 before the daemon recorded it, which
// LEGION-209's first step carried to generation 1, given again there under an id never marked,
// whose mark is the one change. Each table's rows are read whole before and after
// (testdb.TableRows), so a row changed in place counts.
//
// The faults it catches: a give of the version in force judged as a later version (its control:
// versionRefusal taking no give as the version in force, sameVersion false, under which each give
// is refused 409 PLAN_VERSION_OLD), and a give of it that writes its row again (its control:
// record.Store.AddPlanGiver without its guard, under which a giver's give again rewrites the
// version's givers and the time it was given, in place, which a count of rows does not see).
func TestAGiveOfTheVersionInForceStoresNothing(t *testing.T) {
	t.Run("taken at the issue's generation, given again by its giver and by another owner", func(t *testing.T) {
		w := newPlanWorld(t, true)
		w.taken(t, w.sub, 1, "premise: the sub-architect's version 1")
		before := testdb.TableRows(t, w.h.store.Pool())
		w.taken(t, w.sub, 1, "the sub-architect gives the version it gave")
		again := testdb.TableRows(t, w.h.store.Pool())
		if changed := testdb.ChangedTables(before, again); len(changed) != 0 {
			t.Errorf("the sub-architect's give again changed the rows of %q, want no table changed", changed)
		}
		w.taken(t, w.root, 1, "the root's architect gives the version the sub-architect gave")
		if changed := testdb.ChangedTables(again, testdb.TableRows(t, w.h.store.Pool())); !slices.Equal(changed, []string{"issue_plans"}) {
			t.Errorf("the root's architect's give changed the rows of %q, want issue_plans alone: the giver it adds", changed)
		}
		if got, want := w.versionsOf(t, 1), []versionRow{ownersVersion(1, w.root, w.sub)}; !reflect.DeepEqual(got, want) {
			t.Errorf("LEGION-209 holds %+v, want %+v: version 1 given by both owners", got, want)
		}
	})

	t.Run("given at generation 0 and carried to the issue's generation by its first step", func(t *testing.T) {
		w := newPlanWorld(t, false)
		w.taken(t, w.root, 1, "premise: version 1 given before the daemon recorded LEGION-209")
		w.record(t)
		w.firstStep(t)
		if got := w.versionsOf(t, 1); !reflect.DeepEqual(got, []versionRow{ownersVersion(1, w.root)}) {
			t.Fatalf("premise: after its first step LEGION-209 holds %+v at generation 1, want version 1 carried from generation 0", got)
		}
		before := testdb.TableRows(t, w.h.store.Pool())
		w.taken(t, w.root, 1, "the first step carried version 1 to generation 1, where the give names it")
		// The give's id, which names generation 1, was never marked: the give marks it, and
		// writes nothing else.
		if changed := testdb.ChangedTables(before, testdb.TableRows(t, w.h.store.Pool())); !slices.Equal(changed, []string{"processed_events"}) {
			t.Errorf("the give again changed the rows of %q, want processed_events alone: its mark", changed)
		}
	})
}

// A give of a version below the version in force is refused 409 PLAN_VERSION_OLD, whatever its
// id, and stores nothing: version 1, which LEGION-209's first step carried to generation 1 with
// version 2 from generation 0, given there under an id never marked; and version 1 taken at
// generation 1 and given again, under the id of its own take, once version 2 is in force there.
//
// The fault it catches is a version the store holds answered as taken though a later version is in
// force (its control: versionRefusal taking a held version at or below the one in force as taken,
// under which both gives answer 200 with version 1's plan while version 2 runs).
func TestAGiveOfAVersionBelowThePlanInForceIsRefusedWhateverItsID(t *testing.T) {
	for _, tc := range []struct {
		name  string
		given func(t *testing.T, w planWorld)
	}{
		{"carried from generation 0, under an id never marked", func(t *testing.T, w planWorld) {
			w.taken(t, w.root, 1, "premise: version 1 at generation 0")
			w.taken(t, w.root, 2, "premise: version 2 at generation 0")
			w.record(t)
			w.firstStep(t)
		}},
		{"taken at the issue's generation, under the id of its own take", func(t *testing.T, w planWorld) {
			w.record(t)
			w.taken(t, w.root, 1, "premise: version 1 at generation 1")
			w.taken(t, w.root, 2, "premise: version 2 at generation 1")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newPlanWorld(t, false)
			tc.given(t, w)
			before := testdb.TableRows(t, w.h.store.Pool())
			refusedSaying(t, registerPlan(w.h, w.root.grant(t), "LEGION-209", childDoc, 1), http.StatusConflict, "PLAN_VERSION_OLD",
				"the plan in force of LEGION-209 is version 2, and Legion takes only a later version: version 1 changed nothing.")
			if changed := testdb.ChangedTables(before, testdb.TableRows(t, w.h.store.Pool())); len(changed) != 0 {
				t.Errorf("the refused give changed the rows of %q, want no table changed", changed)
			}
		})
	}
}

// Before LEGION-209's first step, a version in force that only an earlier owner gave is passed over
// at the give, by the rule its first step reads (record.GivenByEarlierOwners): the owner's give is
// judged as the generation's first plan, stored as the owner's, and run by the first step, which
// tells nobody that a plan was not taken. Here the architect of LEGION-211 gives a version,
// Dispatch then moves LEGION-211 out from above LEGION-209, and the root's architect gives the
// same version, or a lower one.
//
// The faults it catches: the same version answered as taken while it stays the earlier owner's, so
// the first step passes it over and runs the default steps, and the lower version refused
// PLAN_VERSION_OLD against the earlier owner's version (one control for both: versionRefusal
// reading no earlier owner's plan at the give, under which the same version keeps the departed
// sub-architect as a giver and the lower version is refused).
func TestAnOwnersGiveOverAnEarlierOwnersVersionIsStoredAndRuns(t *testing.T) {
	for _, tc := range []struct {
		name           string
		earlier, given int
	}{
		{"the same version", 1, 1},
		{"a lower version", 2, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newPlanWorld(t, true)
			w.taken(t, w.sub, tc.earlier, "premise: the sub-architect's version")
			w.leaves(t, "LEGION-211")

			w.taken(t, w.root, tc.given, "the root's architect's version")
			if got, want := w.versionsOf(t, 1), []versionRow{ownersVersion(tc.given, w.root)}; !reflect.DeepEqual(got, want) {
				t.Fatalf("LEGION-209 holds %+v at generation 1, want %+v: the root's architect's version alone", got, want)
			}
			w.firstStep(t)
			if got := w.phaseOf(t); got != phase.Implementing {
				t.Errorf("LEGION-209 after its first step stands in %s, want implementing, the first step of the root's architect's version", got)
			}
			if got := w.notTaken(t); len(got) != 0 {
				t.Errorf("the plan-not-taken notices of LEGION-209 = %q, want none", got)
			}
		})
	}
}

// A version that one of its givers gave while it owned the issue is that giver's while it owns the
// issue, whoever else gave it (record.GivenByOwner): the root's architect gives version 1, the
// architect of LEGION-211 gives it again, which the version then names as its last giver, and
// Dispatch moves LEGION-211 out from above LEGION-209 before LEGION-209's first step. The first
// step runs version 1 and tells nobody that a plan was not taken: the root's architect still owns
// the issue, and was told version 1 was taken.
//
// The fault it catches is a version judged by its last giver alone (its control: record.GivenByOwner
// reading given_by alone, as a release before this one does, under which the first step passes the
// version over and runs the default steps).
func TestAVersionRunsWhileOneOfItsGiversOwnsTheIssue(t *testing.T) {
	w := newPlanWorld(t, true)
	w.taken(t, w.root, 1, "the root's architect's version 1")
	w.taken(t, w.sub, 1, "the sub-architect's give of the same version")
	if got, want := w.versionsOf(t, 1), []versionRow{ownersVersion(1, w.sub, w.root)}; !reflect.DeepEqual(got, want) {
		t.Fatalf("LEGION-209 holds %+v, want %+v", got, want)
	}
	w.leaves(t, "LEGION-211")

	w.firstStep(t)
	if got := w.phaseOf(t); got != phase.Implementing {
		t.Errorf("LEGION-209 after its first step stands in %s, want implementing, the first step of version 1", got)
	}
	if got := w.notTaken(t); len(got) != 0 {
		t.Errorf("the plan-not-taken notices of LEGION-209 = %q, want none", got)
	}
}

// A version every giver of which owns the issue no more is passed over: at LEGION-209's first
// step, which runs the default steps and tells the root's architect in one notice that names the
// version and every one of its givers; or at the root's architect's give of that version before the
// first step, which stores it as the root's architect's alone. The architects of LEGION-211 and
// LEGION-212 each give version 1, and Dispatch then moves both issues out from above LEGION-209.
//
// The faults it catches: a notice that names one giver of the version (its control: giversOf
// naming the last giver alone), and a version passed over at the give that keeps the givers who left
// (its control: versionRefusal reading no earlier owner's plan at the give, under which addGiver
// adds the root's architect to the two who left).
func TestAVersionEveryGiverOfWhichLeftIsPassedOver(t *testing.T) {
	given := func(t *testing.T) planWorld {
		t.Helper()
		w := newPlanWorld(t, true)
		w.taken(t, w.sub, 1, "premise: the sub-architect of LEGION-211's version 1")
		w.taken(t, w.third, 1, "premise: the sub-architect of LEGION-212's give of the same version")
		w.leaves(t, "LEGION-211")
		w.leaves(t, "LEGION-212")
		return w
	}

	t.Run("at the first step", func(t *testing.T) {
		w := given(t)
		w.firstStep(t)
		if got := w.phaseOf(t); got != phase.Planning {
			t.Errorf("LEGION-209 after its first step stands in %s, want planning, the first of the default steps", got)
		}
		want := []string{"The first step of LEGION-209 took no plan of an earlier owner: version 1 of document " + childDoc + ", given by " +
			string(w.third.token) + " and " + string(w.sub.token) + ". It runs the default steps. To run a plan it did not take, give it again with register_plan as your own."}
		if got := w.notTaken(t); !slices.Equal(got, want) {
			t.Errorf("the plan-not-taken notices of LEGION-209 = %q, want %q", got, want)
		}
	})

	t.Run("at the owner's give", func(t *testing.T) {
		w := given(t)
		w.taken(t, w.root, 1, "the root's architect's give of the version passed over")
		if got, want := w.versionsOf(t, 1), []versionRow{ownersVersion(1, w.root)}; !reflect.DeepEqual(got, want) {
			t.Errorf("LEGION-209 holds %+v, want %+v: the root's architect's version alone", got, want)
		}
	})
}

// The givers of a version of a plan given for LEGION-209 before the daemon recorded it, at
// generation 0, are the owners the route judged by Dispatch's chain (planOwner): a give by another
// owner adds that owner, and a give by a giver writes nothing. Here the architect of LEGION-211
// gives version 1, the root's architect gives it, and the give of LEGION-211's architect is
// delivered again: it is a giver, so nothing is written and the version names the root's architect
// as its last giver still.
//
// The fault it catches is a give that writes its giver again on a second delivery (its control:
// record.Store.AddPlanGiver without its guard, adding a giver the version names already, under
// which the second delivery makes the sub-architect the last giver again).
func TestAGiversGiveAgainAtGenerationZeroWritesNothing(t *testing.T) {
	w := newPlanWorld(t, false)
	w.taken(t, w.sub, 1, "the sub-architect's version 1")
	w.taken(t, w.root, 1, "the root's architect's give of the same version")
	want := []versionRow{ownersVersion(1, w.root, w.sub)}
	if got := w.versionsOf(t, 0); !reflect.DeepEqual(got, want) {
		t.Fatalf("LEGION-209 holds %+v at generation 0, want %+v", got, want)
	}
	before := testdb.TableRows(t, w.h.store.Pool())

	w.taken(t, w.sub, 1, "the sub-architect's give delivered again")
	if got := w.versionsOf(t, 0); !reflect.DeepEqual(got, want) {
		t.Errorf("after the second delivery LEGION-209 holds %+v at generation 0, want %+v unchanged", got, want)
	}
	if changed := testdb.ChangedTables(before, testdb.TableRows(t, w.h.store.Pool())); len(changed) != 0 {
		t.Errorf("the second delivery changed the rows of %q, want no table changed", changed)
	}
}

// handedOver writes LEGION-209 as a hand-over of the release before left it (that release's
// admit's readmit), which this release's first boot converts and hands over no more: a root of its
// own on its Dispatch chain, at generation 2, recorded as having left the tree LEGION-208. With its
// run, it stands in the phase it ran in, and its plan of generation 1 is carried to generation 2
// with each task where it stood (record.Store.CarryPlan, carried); without, it stands at admitted
// and nothing is carried.
func (w planWorld) handedOver(t *testing.T, run bool) {
	t.Helper()
	at := phase.Admitted
	if run {
		at = phase.Implementing
	}
	if err := store.Transact(context.Background(), w.h.store.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		if err := record.NewStore().PutIssue(ctx, tx, record.Issue{Key: "LEGION-209", Repo: "acme/widgets", DefaultBranch: "main",
			GitHubRepo: "acme/widgets", Tree: "LEGION-209", Project: testProject, Title: "LEGION-209", Phase: at, Generation: 2,
			Status: "in_progress", HandedOver: true, HandedOverFrom: "LEGION-208"}); err != nil {
			return err
		}
		if !run {
			return nil
		}
		return record.NewStore().CarryPlan(ctx, tx, "LEGION-209", 1, 2, true)
	}); err != nil {
		t.Fatalf("hand LEGION-209 over: %v", err)
	}
	w.standUnder(t, "LEGION-209", "", "in_progress")
}

// The plan of a hand-over of the release before that carried its run is the plan that run walks
// (that release's readmit carried it with each task where it stood), so the new owner's give of its
// version in force is answered as taken, stores no plan row, and adds the new owner to the
// version's givers, the one change of the store beside the mark of its id (testdb.TableRows). A
// hand-over that carried no run carried no plan, so the new owner's give of the same version is the
// new generation's first plan, and is stored. The root's architect gives version 1 for LEGION-209
// at generation 1, the run starts there or does not, and LEGION-209 is then handed over as a root
// of its own at generation 2, whose owner is its own architect.
//
// The fault it catches is the carried version refused PLAN_VERSION_OLD to the owner of the run
// that walks it (its control: versionRefusal taking no give as the version in force, sameVersion
// false).
func TestTheNewOwnersGiveOfAHandedOverVersion(t *testing.T) {
	t.Run("a hand-over that carries its run", func(t *testing.T) {
		w := newPlanWorld(t, true)
		w.taken(t, w.root, 1, "premise: the root's architect's version 1")
		w.firstStep(t)
		w.handedOver(t, true)
		owner := newLiveClaim(t, w.h, "LEGION-209", claim.RoleArchitect)
		before := testdb.TableRows(t, w.h.store.Pool())

		w.taken(t, owner, 1, "the version the carried run walks")
		// The give's id, which names generation 2, was never marked: the give marks it, adds the
		// new owner to the version's givers, and writes nothing else.
		if changed := testdb.ChangedTables(before, testdb.TableRows(t, w.h.store.Pool())); !slices.Equal(changed, []string{"issue_plans", "processed_events"}) {
			t.Errorf("the give changed the rows of %q, want issue_plans and processed_events alone: the giver and the mark", changed)
		}
		if got, want := w.versionsOf(t, 2), []versionRow{ownersVersion(1, owner, w.root)}; !reflect.DeepEqual(got, want) {
			t.Errorf("LEGION-209 holds %+v at generation 2, want %+v", got, want)
		}
	})

	t.Run("a hand-over that carries no run", func(t *testing.T) {
		w := newPlanWorld(t, true)
		w.taken(t, w.root, 1, "premise: the root's architect's version 1")
		w.handedOver(t, false)
		owner := newLiveClaim(t, w.h, "LEGION-209", claim.RoleArchitect)

		w.taken(t, owner, 1, "the new generation's first plan")
		if got, want := w.versionsOf(t, 2), []versionRow{ownersVersion(1, owner)}; !reflect.DeepEqual(got, want) {
			t.Errorf("LEGION-209 holds %+v at generation 2, want %+v", got, want)
		}
	})
}

// daemonHandlers is the chain the daemon applies a fact with (internal/daemon's workflow
// handlers): Dispatch's blocker mirror (blockers.Mirror, which no PlanGiven moves), the workflow
// (workflow.Engine, which judges and stores a plan and tells its workers), and admission
// (admit.Admission, which promotes its waiting line on every fact).
func daemonHandlers(engine *workflow.Engine) []intake.Handler {
	records, log := record.NewStore(), slog.New(slog.NewTextHandler(io.Discard, nil))
	return []intake.Handler{blockers.NewMirror(records, testProject), engine, admit.New(records, engine, 8, testProject, log)}
}

// A second delivery of a give that was stored and is in force writes nothing, through every handler
// the daemon applies a fact with (daemonHandlers): no plan row, no notice to a worker that a version
// reached it, no outbox row, no message on the issue and no row changed in place, which every
// table's rows, read whole before and after (testdb.TableRows), hold. Its id stays marked, so a
// daemon of the release before, after a way back, reads it as a give already applied. The give is
// the root's architect's version 1, after LEGION-209's first step ran the default steps and the
// default task was stopped, so its first delivery stores the version and starts its task again,
// which writes rows and tells its worker.
//
// The faults it catches: a second delivery applied as a first one (its control: planGiven writing
// a give of the version in force as a later version, under which the second delivery's write is
// refused as a version stored already, and the delivery fails), and a second delivery that writes
// its row again (its control: record.Store.AddPlanGiver without its guard, under which the second
// delivery rewrites the version's givers and the time it was given, in place, which a count of rows
// does not see).
func TestASecondDeliveryOfAStoredGiveWritesNothing(t *testing.T) {
	w := newPlanWorld(t, true)
	w.firstStep(t)
	if stopped := stopTask(w.h, w.root.grant(t), "LEGION-209", plan.DefaultTask); stopped.Code != http.StatusOK {
		t.Fatalf("stop the default steps = %d %s, want 200", stopped.Code, stopped.Body)
	}
	const id = "plan:LEGION-209:1:" + childDoc + ":1"
	fact := intake.PlanGiven{Issue: "LEGION-209", Generation: 1, GivenBy: w.root.token, Plan: plan.Plan{ArtifactID: childDoc, Version: 1,
		Tasks: []plan.Task{{ID: "a", Steps: implementerReviewer, Text: "Do the change."}}}}
	deliver := func(what string) map[string]string {
		t.Helper()
		result, err := intake.ApplyFact(context.Background(), w.h.store.Pool(), "api", id, fact, daemonHandlers(w.engine)...)
		if err != nil || result.Refusal != nil || result.Duplicate {
			t.Fatalf("%s = %+v, %v, want it applied with no refusal", what, result, err)
		}
		return testdb.TableRows(t, w.h.store.Pool())
	}
	before := testdb.TableRows(t, w.h.store.Pool())
	first := deliver("the first delivery")
	if changed := testdb.ChangedTables(before, first); len(changed) == 0 {
		t.Fatal("premise: the first delivery changed no table, want the version stored and its task started")
	}
	if changed := testdb.ChangedTables(first, deliver("the second delivery")); len(changed) != 0 {
		t.Errorf("the second delivery changed the rows of %q, want no table changed", changed)
	}
	assertProcessedEvent(t, w.h, id)
}
