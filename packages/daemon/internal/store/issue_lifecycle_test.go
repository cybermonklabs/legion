package store

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
	"github.com/sjawhar/legion/daemon/internal/wait"
)

// The cases of the tree barrier's cleanup over the lifecycle each issue has of its own (decision 16
// of dispatch://INFRA-4730), the barrier a claim of this release binds and its issue's cleanup
// reserves.

// inLegionsWork is the workflow's rule a workflow cleanup is given (record.InLegionsWork).
func inLegionsWork(at phase.Phase, status string) bool {
	return record.InLegionsWork(record.Issue{Phase: at, Status: status})
}

// openIssueLifecycle opens issue's own lifecycle under the workflow, as admission's promotion does.
func openIssueLifecycle(t *testing.T, store *Store, issue string) {
	t.Helper()
	if _, err := store.OpenIssueLifecycle(context.Background(), "legion", issue, treelifecycle.AuthorityWorkflow); err != nil {
		t.Fatalf("open the lifecycle of %s: %v", issue, err)
	}
}

// seedIssue records key under tree at generation, in phase and status, as the daemon's record of
// it stands when a test reads it.
func seedIssue(t *testing.T, store *Store, key, tree string, generation int, at, status string) {
	t.Helper()
	if _, err := store.Pool().Exec(context.Background(), `insert into issues
		(key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
		values ($1, $2, 'LEGION', $1, $3, $4, $5, 'B', 1)
		on conflict (key) do update set tree = excluded.tree, phase = excluded.phase,
			generation = excluded.generation, status = excluded.status`, key, tree, at, generation, status); err != nil {
		t.Fatalf("record %s: %v", key, err)
	}
}

// issueReleaser is a runtime that holds one Sandbox per issue and names each issue it was asked to
// release, in order.
type issueReleaser struct{ released []string }

func (r *issueReleaser) CleanupIssue(_ context.Context, issue string) error {
	r.released = append(r.released, issue)
	return nil
}

func issueClaim(issue string, role claim.Role) supervise.Claim {
	return supervise.Claim{Token: claim.Token("legion-legion-" + strings.ToLower(issue) + "-" + string(role)), Project: "legion",
		Tree: rootIssue, Issue: issue, Role: role, State: supervise.StateQueued}
}

// A cleanup reserves the issue's own barrier before any process of the issue exists, once the
// issue has closed at the cleanup's generation. The delayed claim of that issue cannot persist,
// while a claim of another issue of the same stored tree, whose own lifecycle stays open, is
// admitted: the cleanup is the issue's alone (R17 of dispatch://INFRA-3320). Confirming the
// empty cleanup releases that one issue's resources, and only a fresh explicit open of the issue
// starts its epoch two, which the claim then binds.
func TestAnIssueCleanupFencesADelayedClaimOfThatIssueUntilAFreshOpen(t *testing.T) {
	t.Parallel()
	store := migratedStore(t)
	ctx := context.Background()
	seedIssue(t, store, rootIssue, rootIssue, 1, "implementing", "in_progress")
	seedIssue(t, store, childIssue, rootIssue, 1, "done", "done")
	openIssueLifecycle(t, store, rootIssue)
	openIssueLifecycle(t, store, childIssue)
	lifecycle, reserved, err := store.ReserveIssueCleanup(ctx, "legion", childIssue, 1, inLegionsWork)
	if err != nil || !reserved || lifecycle.Epoch != 1 || lifecycle.Tree != childIssue {
		t.Fatalf("reserve before any claim = %+v, reserved %t, err %v; want %s's epoch 1 reserved", lifecycle, reserved, err, childIssue)
	}
	child := issueClaim(childIssue, claim.RoleImplementer)
	if _, err := store.AdmitClaim(ctx, child); !errors.Is(err, treelifecycle.ErrCleanupReserved) || !errors.Is(err, wait.ErrWaiting) {
		t.Fatalf("delayed claim of %s after its reservation = %v, want the reservation's wait", childIssue, err)
	}
	if bound, err := store.AdmitClaim(ctx, issueClaim(rootIssue, claim.RoleArchitect)); err != nil || bound.IssueEpoch != 1 {
		t.Fatalf("claim of %s while %s's cleanup is reserved = %+v, %v; want it admitted on its own epoch 1", rootIssue, childIssue, bound, err)
	}
	releaser := &issueReleaser{}
	if err := store.CleanupReservedIssue(ctx, "legion", childIssue, lifecycle.Epoch, releaser); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(releaser.released, []string{childIssue}) {
		t.Fatalf("the cleanup of %s released %v, want %s alone", childIssue, releaser.released, childIssue)
	}
	opened, err := store.OpenIssueLifecycle(ctx, "legion", childIssue, treelifecycle.AuthorityWorkflow)
	if err != nil || opened.Epoch != 2 {
		t.Fatalf("fresh open of %s = %+v, err %v, want epoch 2", childIssue, opened, err)
	}
	bound, err := store.AdmitClaim(ctx, child)
	if err != nil || bound.IssueEpoch != 2 {
		t.Fatalf("claim of %s after its confirmed cleanup = %+v, err %v; want epoch 2", childIssue, bound, err)
	}
}

// A workflow cleanup that names a generation its issue never closed at, or none, reserves nothing
// and is no error, and neither does one of an issue back in Legion's work at that very
// generation: the outbox's ordinary fences finish its row, and the issue's lifecycle stays open
// for its claims.
func TestAnIssueCleanupOfAnotherGenerationOrOfAnIssueInWorkReservesNothing(t *testing.T) {
	t.Parallel()
	store := migratedStore(t)
	ctx := context.Background()
	seedIssue(t, store, childIssue, rootIssue, 1, "done", "done")
	openIssueLifecycle(t, store, childIssue)
	for _, generation := range []uint64{0, 2, 1 << 63} {
		if lifecycle, reserved, err := store.ReserveIssueCleanup(ctx, "legion", childIssue, generation, inLegionsWork); err != nil || reserved {
			t.Fatalf("reserve at generation %d = %+v, reserved %t, err %v; want nothing reserved and no error", generation, lifecycle, reserved, err)
		}
	}
	seedIssue(t, store, childIssue, rootIssue, 1, "implementing", "in_progress")
	if lifecycle, reserved, err := store.ReserveIssueCleanup(ctx, "legion", childIssue, 1, inLegionsWork); err != nil || reserved {
		t.Fatalf("reserve of %s back in work = %+v, reserved %t, err %v; want nothing reserved and no error", childIssue, lifecycle, reserved, err)
	}
	if bound, err := store.AdmitClaim(ctx, issueClaim(childIssue, claim.RoleImplementer)); err != nil || bound.IssueEpoch != 1 {
		t.Fatalf("claim after the unrunnable cleanups = %+v, err %v; want the open epoch 1", bound, err)
	}
}

// A workflow cleanup cannot select the operator's authority by passing a malformed input: the
// authority is stored when the operator's spawn opened the issue, not inferred from a zero
// generation at the cleanup. The operator's own close reserves it, and asked again after its
// cleanup confirmed reserves nothing.
func TestAMalformedWorkflowCleanupCannotSelectTheOperatorsIssue(t *testing.T) {
	t.Parallel()
	store := migratedStore(t)
	ctx := context.Background()
	if _, err := store.OpenIssueLifecycle(ctx, "legion", childIssue, treelifecycle.AuthorityOperator); err != nil {
		t.Fatal(err)
	}
	seedIssue(t, store, childIssue, rootIssue, 1, "done", "done")
	if _, reserved, err := store.ReserveIssueCleanup(ctx, "legion", childIssue, 1, inLegionsWork); err == nil || reserved || !strings.Contains(err.Error(), "authority") {
		t.Fatalf("workflow reserve of the operator's issue: reserved %t, err %v", reserved, err)
	}
	if _, reserved, err := store.ReserveIssueCleanup(ctx, "legion", childIssue, 0, inLegionsWork); err != nil || reserved {
		t.Fatalf("workflow reserve with generation 0: reserved %t, err %v", reserved, err)
	}
	releaser := &issueReleaser{}
	if err := store.CleanupReservedIssue(ctx, "legion", childIssue, 0, releaser); err == nil || len(releaser.released) != 0 {
		t.Fatalf("cleanup with epoch 0 = %v after releasing %v; want a refusal before any release", err, releaser.released)
	}
	if lifecycle, err := store.OpenIssueLifecycle(ctx, "legion", childIssue, treelifecycle.AuthorityWorkflow); err == nil {
		t.Fatalf("workflow open of the operator's issue = %+v, want an authority refusal", lifecycle)
	}
	lifecycle, reserved, err := store.ReserveOperatorIssueCleanup(ctx, "legion", childIssue)
	if err != nil || !reserved {
		t.Fatalf("the operator's close = %+v, reserved %t, err %v; want it reserved", lifecycle, reserved, err)
	}
	if err := store.CleanupReservedIssue(ctx, "legion", childIssue, lifecycle.Epoch, releaser); err != nil {
		t.Fatal(err)
	}
	if _, reserved, err := store.ReserveOperatorIssueCleanup(ctx, "legion", childIssue); err != nil || reserved {
		t.Fatalf("the operator's close asked again after its cleanup confirmed: reserved %t, err %v; want nothing reserved", reserved, err)
	}
}

// A reservation whose transaction commits while a start waits on the shared serializer wins: the
// start is refused with the named cleanup wait once the reservation commits, and writes nothing.
func TestAnIssueReservationThatCommitsFirstRefusesAWaitingStart(t *testing.T) {
	t.Parallel()
	store := migratedStore(t)
	ctx := context.Background()
	openIssueLifecycle(t, store, childIssue)
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := treelifecycle.Issues.ReserveCleanup(ctx, tx, "legion", childIssue, treelifecycle.AuthorityWorkflow); err != nil {
		t.Fatal(err)
	}
	child := issueClaim(childIssue, claim.RoleImplementer)
	admitted := make(chan error, 1)
	go func() {
		_, err := store.AdmitClaim(ctx, child)
		admitted <- err
	}()
	// The start waits on the serializer the reservation holds; an answer before then is a start that
	// did not wait.
	var early error
	answered := false
	if !testwait.Holds(testwait.Bounded(t, 10*time.Second, time.Second), 10*time.Millisecond, func() bool {
		select {
		case early = <-admitted:
			answered = true
			return true
		default:
		}
		return waitingOnTheSerializer(t, store)
	}) {
		t.Fatal("the start neither answered nor waited on the reservation's serializer")
	}
	if answered {
		t.Fatalf("the start did not wait for the reservation's serializer: %v", early)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-admitted; !errors.Is(err, treelifecycle.ErrCleanupReserved) || !errors.Is(err, wait.ErrWaiting) {
		t.Fatalf("start after the reservation committed = %v, want the reservation's wait", err)
	}
	var count int
	if err := store.Pool().QueryRow(ctx, `select count(*) from claims where token = $1`, string(child.Token)).Scan(&count); err != nil || count != 0 {
		t.Fatalf("the refused start left %d claim rows (err %v)", count, err)
	}
}

// A restart reads the same unconfirmed reservation from Postgres. It still rejects delayed claim
// persistence, and its retry returns the original epoch rather than losing the cleanup marker in a
// runtime-local cache.
func TestARestartRetainsTheIssueCleanupReservation(t *testing.T) {
	t.Parallel()
	store := migratedStore(t)
	ctx := context.Background()
	seedIssue(t, store, childIssue, rootIssue, 1, "done", "done")
	openIssueLifecycle(t, store, childIssue)
	lifecycle, reserved, err := store.ReserveIssueCleanup(ctx, "legion", childIssue, 1, inLegionsWork)
	if err != nil || !reserved {
		t.Fatalf("reserve = %+v, reserved %t, err %v", lifecycle, reserved, err)
	}
	restarted, err := Open(ctx, store.pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(restarted.Close)
	if _, err := restarted.AdmitClaim(ctx, issueClaim(childIssue, claim.RoleImplementer)); !errors.Is(err, treelifecycle.ErrCleanupReserved) {
		t.Fatalf("delayed claim after restart = %v, want the reservation's wait", err)
	}
	retry, resumed, err := restarted.ReserveIssueCleanup(ctx, "legion", childIssue, 1, inLegionsWork)
	if err != nil || !resumed || retry.Epoch != lifecycle.Epoch {
		t.Fatalf("reservation retry after restart = %+v, resumed %t, err %v", retry, resumed, err)
	}
}

// The reserved cleanup waits, releasing nothing, while any stored claim of its issue has not
// retired; a claim of another issue under the same stored tree holds nothing of it. Once every
// claim of the issue retired it releases that issue alone, ends the pause's hold of that issue's
// pod and of no other, and confirms. A retained claim token is then no authority to launch a later
// lifetime: the next open rebinds it, and its persisted IssueEpoch changes with that lifecycle.
func TestAReservedIssueCleanupWaitsForEveryClaimOfItsIssueAndTheNextEpochRebindsIt(t *testing.T) {
	t.Parallel()
	store := migratedStore(t)
	ctx := context.Background()
	seedIssue(t, store, rootIssue, rootIssue, 1, "implementing", "in_progress")
	seedIssue(t, store, childIssue, rootIssue, 1, "done", "done")
	openIssueLifecycle(t, store, rootIssue)
	openIssueLifecycle(t, store, childIssue)
	c := issueClaim(childIssue, claim.RoleImplementer)
	first, err := store.AdmitClaim(ctx, c)
	if err != nil || first.IssueEpoch != 1 {
		t.Fatalf("first claim = %+v, err %v", first, err)
	}
	if _, err := store.AdmitClaim(ctx, issueClaim(rootIssue, claim.RoleArchitect)); err != nil {
		t.Fatal(err)
	}
	pause, _ := pauseLegion(t, store, pausedAt)
	for _, issue := range []string{rootIssue, childIssue} {
		if _, err := holdPod(store, pause, issue, pausedAt); err != nil {
			t.Fatalf("hold the pod of %s: %v", issue, err)
		}
	}
	lifecycle, reserved, err := store.ReserveIssueCleanup(ctx, "legion", childIssue, 1, inLegionsWork)
	if err != nil || !reserved {
		t.Fatalf("reserve = reserved %t, err %v", reserved, err)
	}
	releaser := &issueReleaser{}
	err = store.CleanupReservedIssue(ctx, "legion", childIssue, lifecycle.Epoch, releaser)
	if !errors.Is(err, wait.ErrWaiting) || !strings.Contains(err.Error(), string(c.Token)+":queued") || strings.Contains(err.Error(), rootIssue) ||
		len(releaser.released) != 0 {
		t.Fatalf("cleanup with a queued claim = %v after releasing %v; want a wait naming that claim alone and no release", err, releaser.released)
	}
	first.State = supervise.StateRetired
	if err := store.PutClaim(ctx, first); err != nil {
		t.Fatal(err)
	}
	if err := store.CleanupReservedIssue(ctx, "legion", childIssue, lifecycle.Epoch, releaser); err != nil || !reflect.DeepEqual(releaser.released, []string{childIssue}) {
		t.Fatalf("cleanup once every claim of %s retired = %v after releasing %v; want %s released alone and a confirmation", childIssue, err, releaser.released, childIssue)
	}
	if _, held, err := store.PausePodHoldOf(ctx, "LEGION", childIssue); err != nil || held {
		t.Fatalf("after %s's cleanup its pod's hold stands = %t, %v; want it ended", childIssue, held, err)
	}
	if _, held, err := store.PausePodHoldOf(ctx, "LEGION", rootIssue); err != nil || !held {
		t.Fatalf("after %s's cleanup the hold of %s's pod stands = %t, %v; want it standing: the cleanup deleted no pod of another issue", childIssue, rootIssue, held, err)
	}
	if err := store.CleanupReservedIssue(ctx, "legion", childIssue, lifecycle.Epoch, releaser); err == nil || len(releaser.released) != 1 {
		t.Fatalf("cleanup of a confirmed reservation = %v after releasing %v; want a refusal and no second release", err, releaser.released)
	}
	openIssueLifecycle(t, store, childIssue)
	second, err := store.AdmitClaim(ctx, c)
	if err != nil || second.IssueEpoch != 2 {
		t.Fatalf("reactivated claim = %+v, err %v", second, err)
	}
}

// Every lifecycle operation of an issue takes the shared apply_fact serializer before any
// lifecycle row (holdSerializerThenLock).
func TestIssueCleanupStepsTakeTheSerializerBeforeTheirRows(t *testing.T) {
	t.Parallel()
	const lifecycleRow = `select 1 from issue_lifecycles where project = 'legion' and issue = $1 for update`
	t.Run("reservation", func(t *testing.T) {
		store := migratedStore(t)
		ctx := context.Background()
		seedIssue(t, store, rootIssue, rootIssue, 1, "done", "done")
		openIssueLifecycle(t, store, rootIssue)
		holdSerializerThenLock(t, store, lifecycleRow, func() error {
			_, reserved, err := store.ReserveIssueCleanup(ctx, "legion", rootIssue, 1, inLegionsWork)
			if err == nil && !reserved {
				return errors.New("reserved nothing")
			}
			return err
		})
	})
	t.Run("cleanup", func(t *testing.T) {
		store := migratedStore(t)
		ctx := context.Background()
		seedIssue(t, store, rootIssue, rootIssue, 1, "done", "done")
		openIssueLifecycle(t, store, rootIssue)
		lifecycle, reserved, err := store.ReserveIssueCleanup(ctx, "legion", rootIssue, 1, inLegionsWork)
		if err != nil || !reserved {
			t.Fatalf("reserve = reserved %t, err %v", reserved, err)
		}
		holdSerializerThenLock(t, store, lifecycleRow, func() error {
			return store.CleanupReservedIssue(ctx, "legion", rootIssue, lifecycle.Epoch, &issueReleaser{})
		})
	})
}

// An issue that never had a lifecycle reserves no cleanup at its close: there is nothing for one
// to release, and the outbox's ordinary fences finish the row.
func TestAWorkflowCleanupOfAnIssueNeverOpenedReservesNothing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := migratedStore(t)
	seedIssue(t, store, childIssue, rootIssue, 1, "done", "done")
	lifecycle, reserved, err := store.ReserveIssueCleanup(ctx, "legion", childIssue, 1, inLegionsWork)
	if err != nil || reserved {
		t.Fatalf("ReserveIssueCleanup of an issue never opened = %+v, %t, %v; want nothing reserved and no error", lifecycle, reserved, err)
	}
}

// IssueLive is whether the issue has a lifecycle of its own whose cleanup has not confirmed: open,
// or reserved and releasing. An issue whose cleanup confirmed, one never opened and one only
// another project opened are none, and so is a child with no lifecycle of its own under a live
// root, its record's tree and its Dispatch parent both that root (R17).
func TestIssueLiveIsAnIssueLifecycleWhoseCleanupIsNotConfirmed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := migratedStore(t)
	for _, row := range []struct {
		project, issue     string
		started, confirmed bool
	}{
		{"legion", "LEGION-801", false, false},
		{"legion", "LEGION-802", true, false},
		{"legion", "LEGION-803", true, true},
		{"other", "LEGION-804", false, false},
	} {
		if _, err := store.Pool().Exec(ctx, `insert into issue_lifecycles (project, issue, epoch, authority, cleanup_started, cleanup_confirmed_at)
			values ($1, $2, 1, 'workflow', $3, case when $4 then now() end)`, row.project, row.issue, row.started, row.confirmed); err != nil {
			t.Fatal(err)
		}
	}
	// The root is live, by its own lifecycle and by the tree lifecycle the release before binds; the
	// child under it has neither.
	seedIssue(t, store, childIssue, rootIssue, 1, "implementing", "in_progress")
	if _, err := store.Pool().Exec(ctx, `insert into dispatch_issues (key, project, status, blockers, dispatch_seq, parent)
		values ($1, 'LEGION', 'in_progress', '{}', 1, $2)`, childIssue, rootIssue); err != nil {
		t.Fatal(err)
	}
	openIssueLifecycle(t, store, rootIssue)
	openLingeringTree(t, store)
	for issue, want := range map[string]bool{"LEGION-801": true, "LEGION-802": true, "LEGION-803": false, "LEGION-804": false, "LEGION-805": false,
		rootIssue: true, childIssue: false} {
		if live, err := store.IssueLive(ctx, "legion", issue); err != nil || live != want {
			t.Errorf("IssueLive(%s) = %t, %v; want %t", issue, live, err, want)
		}
	}
}

// An admitted claim stores, for the release before (R22 of dispatch://INFRA-3320), the tree its
// issue's record stores, whatever tree the claim was built with, and the epoch that tree's
// lifecycle stands at, 0 for a tree with none. An operator's claim on an issue this project
// records nothing of — one never recorded, and one only another project's daemon records — keeps
// the tree the operator named. The claim read back carries both and the issue's own epoch, and the
// project controller's claim binds neither.
func TestAnAdmittedClaimStoresItsIssuesStoredTreeAndThatTreesEpoch(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := migratedStore(t)
	seedIssue(t, store, childIssue, rootIssue, 1, "implementing", "in_progress")
	if _, err := store.Pool().Exec(ctx, `insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
		values ('OTHER-7', 'OTHER-1', 'OTHER', 'OTHER-7', 'implementing', 1, 'in_progress', 'B', 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Pool().Exec(ctx, `insert into tree_lifecycles (project, tree, epoch, authority, cleanup_started)
		values ('legion', $1, 3, 'workflow', true)`, rootIssue); err != nil {
		t.Fatal(err)
	}
	openIssueLifecycle(t, store, childIssue)
	for _, issue := range []string{"LEGION-400", "OTHER-7"} {
		if _, err := store.OpenIssueLifecycle(ctx, "legion", issue, treelifecycle.AuthorityOperator); err != nil {
			t.Fatal(err)
		}
	}
	member := issueClaim(childIssue, claim.RoleImplementer)
	member.Tree = "LEGION-300"
	operator := supervise.Claim{Token: "legion-legion-legion-400-architect", Project: "legion", Tree: "LEGION-400", Issue: "LEGION-400",
		Role: claim.RoleArchitect, State: supervise.StateQueued}
	spawn := supervise.Claim{Token: "legion-legion-other-7-architect", Project: "legion", Tree: "OTHER-7", Issue: "OTHER-7",
		Role: claim.RoleArchitect, State: supervise.StateQueued}
	controller := supervise.Claim{Token: claim.ControllerToken("legion"), Project: "legion", Role: claim.RoleController, State: supervise.StateQueued}
	want := map[claim.Token]struct {
		tree              string
		treeEpoch, issues uint64
	}{
		member.Token:     {rootIssue, 3, 1},
		operator.Token:   {"LEGION-400", 0, 1},
		spawn.Token:      {"OTHER-7", 0, 1},
		controller.Token: {"", 0, 0},
	}
	for _, c := range []supervise.Claim{member, operator, spawn, controller} {
		bound, err := store.AdmitClaim(ctx, c)
		if err != nil {
			t.Fatalf("admit %s: %v", c.Token, err)
		}
		if w := want[c.Token]; bound.Tree != w.tree || bound.TreeEpoch != w.treeEpoch || bound.IssueEpoch != w.issues {
			t.Errorf("admitted %s = tree %q at epoch %d, issue epoch %d; want tree %q at epoch %d, issue epoch %d",
				c.Token, bound.Tree, bound.TreeEpoch, bound.IssueEpoch, w.tree, w.treeEpoch, w.issues)
		}
	}
	loaded, err := store.Claims(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for token, w := range want {
		i := slices.IndexFunc(loaded, func(c supervise.Claim) bool { return c.Token == token })
		if i < 0 {
			t.Fatalf("the store holds no claim %s", token)
		}
		if got := loaded[i]; got.Tree != w.tree || got.TreeEpoch != w.treeEpoch || got.IssueEpoch != w.issues {
			t.Errorf("stored %s = tree %q at epoch %d, issue epoch %d; want tree %q at epoch %d, issue epoch %d",
				token, got.Tree, got.TreeEpoch, got.IssueEpoch, w.tree, w.treeEpoch, w.issues)
		}
		if err := store.CheckLaunch(ctx, loaded[i]); err != nil {
			t.Errorf("launch check of %s: %v", token, err)
		}
	}
}
