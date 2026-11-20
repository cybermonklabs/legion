package store

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// providerErrorsToldVersion is the version of the migration this file tests, so the test can
// migrate through everything before it whatever lands beside it.
const providerErrorsToldVersion = 20261119114140

// runColumns are what 20261119114140 keeps of one claim's runs: the claim's run and its number,
// its told mark, and the runs it owes (provider_errors_owed), each as "<run>: <reason>", oldest
// first.
type runColumns struct {
	run, runID, told int64
	owed             string
}

func readRunColumns(t *testing.T, store *Store, token string) runColumns {
	t.Helper()
	var c runColumns
	if err := store.Pool().QueryRow(context.Background(), `select c.provider_error_run, c.provider_error_run_id,
		c.provider_errors_told_run, coalesce((select string_agg(o.run || ': ' || o.reason, ' | ' order by o.run)
			from provider_errors_owed o where o.claim_token = c.token), '')
		from claims c where c.token = $1`,
		token).Scan(&c.run, &c.runID, &c.told, &c.owed); err != nil {
		t.Fatalf("read the run columns of %s: %v", token, err)
	}
	return c
}

// earlierReleaseReason is the words of the notice a run owes that an earlier release counted: that
// release kept no model and no HTTP status of its turns, and the notice says so.
func earlierReleaseReason(run int) string {
	return fmt.Sprintf("%d turns in a row ended in a provider error, whose model and HTTP status the earlier release did not keep; the daemon keeps the task and sends it again every 10m0s at most", run)
}

// 20261119114140 keeps the notice each run of six provider-error turns owes the project's
// controller. The migration runs over a database with claims in it, one at every run from 0 to 8:
// a claim in a run at the upgrade gets the first run number, so the run it stands in is numbered
// above the told mark; a claim with an issue whose run already stands at six or more owes run 1's
// notice at once, since its next turn can be a clean one and the earlier release kept no mark of
// whether it was told, and the notice names the error and says the earlier release kept no model
// and no status of it; a claim in no run keeps 0. Each run is seeded, so a bound of the backfill
// that moves by one fails here, naming the run. A retired and a failed claim at a run of 7 owe it
// too: either can run again, in a fresh session whose write puts the run back to zero
// (TestAFailedClaimInARunOfSevenAtTheUpgradeIsToldWhenItRunsAgainInAFreshSession). The project
// controller's own claim, which has no issue, owes nothing at a run of 7: no notice names it and
// no reader reads its rows. The negative control is the column itself, which a write refuses
// before the migration.
func TestAClaimInARunAtTheUpgradeGetsTheFirstRunNumberWith20261119114140(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, providerErrorsToldVersion-1)
	type seed struct {
		token, issue, role, state string
		run                       int
	}
	var seeds []seed
	for run := 0; run <= 8; run++ {
		seeds = append(seeds, seed{fmt.Sprintf("legion-LEGION-%d-implementer", 300+run), fmt.Sprintf("LEGION-%d", 300+run), "implementer", "idle", run})
	}
	seeds = append(seeds, seed{"legion-LEGION-320-implementer", "LEGION-320", "implementer", "retired", 7},
		seed{"legion-LEGION-321-implementer", "LEGION-321", "implementer", "failed", 7},
		seed{"legion-legion-controller", "", "controller", "idle", 7})
	for _, s := range seeds {
		if _, err := store.Pool().Exec(ctx, `insert into claims (token, project, tree, issue, role, generation,
			session, session_file, state, launch_failures, prompt_failures, prompt_retires, uncertain_streak, provider_error_run)
			values ($1, 'legion', $2, $2, $3, 1, '', '', $4, 0, 0, 0, 0, $5)`,
			s.token, s.issue, s.role, s.state, s.run); err != nil {
			t.Fatalf("record the %s claim %s in a run of %d before the migration: %v", s.state, s.token, s.run, err)
		}
	}
	if _, err := store.Pool().Exec(ctx, "update claims set provider_error_run_id = 1"); err == nil || !strings.Contains(err.Error(), "provider_error_run_id") {
		t.Fatalf("writing provider_error_run_id before the migration = %v, want the column to be unknown", err)
	}

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the told mark: %v", err)
	}

	for _, s := range seeds {
		want := runColumns{run: int64(s.run)}
		if s.run > 0 {
			want.runID = 1
		}
		if s.run >= 6 && s.issue != "" {
			want.owed = "1: " + earlierReleaseReason(s.run)
		}
		if got := readRunColumns(t, store, s.token); got != want {
			t.Errorf("the %s claim %s in a run of %d reads %+v after the migration, want %+v", s.state, s.token, s.run, got, want)
		}
	}
}

// A claim that the earlier release failed in a run of 7 turns, whose notice was lost because no
// controller was registered at its sixth turn, is retried after the upgrade. The retry of a claim
// that a run of refused turns failed starts a fresh session (supervise's freshSession), and the
// retry's own writes put the run back to zero: the admission's write while the claim still reads
// failed, then the launch's. The run of seven is owed by the upgrade, and still owed once after
// those writes, so a controller that registers is told of it. The controls: a live claim in the
// same run owes it, and a failed claim retried in its recorded session, whose run stands at 7
// again, owes it once and no more.
func TestAFailedClaimInARunOfSevenAtTheUpgradeIsToldWhenItRunsAgainInAFreshSession(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, providerErrorsToldVersion-1)
	for issue, state := range map[string]string{"LEGION-340": "failed", "LEGION-341": "idle", "LEGION-342": "failed"} {
		if _, err := store.Pool().Exec(ctx, `insert into claims (token, project, tree, issue, role, generation,
			session, session_file, state, launch_failures, prompt_failures, prompt_retires, uncertain_streak, provider_error_run)
			values ($1, 'legion', $2, $2, 'implementer', 1, 'ses', '', $3, 0, 0, 0, 0, 7)`,
			"legion-"+issue+"-implementer", issue, state); err != nil {
			t.Fatalf("record the %s claim of %s in a run of 7 before the migration: %v", state, issue, err)
		}
	}
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the told mark: %v", err)
	}
	runAgain := func(issue string, turns int) supervise.Claim {
		c := tmuxClaim(claim.Token("legion-" + issue + "-implementer"))
		c.Tree, c.Issue = issue, issue
		c.ProviderErrorRun, c.ProviderErrorRunID = turns, 1
		for _, state := range []supervise.ClaimState{supervise.StateFailed, supervise.StateLaunching} {
			c.State = state
			if err := store.PutClaim(ctx, c); err != nil {
				t.Fatalf("put the retried claim of %s as %s: %v", issue, state, err)
			}
		}
		return c
	}
	wantRunOfSeven := func(what string, c supervise.Claim) {
		t.Helper()
		if owed := owedOf(t, store, c); len(owed) != 1 || owed[0].Run != 1 || owed[0].Reason != earlierReleaseReason(7) {
			t.Errorf("%s owes %+v, want its run of seven owed once, in the upgrade's words", what, owed)
		}
	}
	wantRunOfSeven("the control, a live claim in a run of 7 at the upgrade,", tmuxClaim("legion-LEGION-341-implementer"))
	wantRunOfSeven("the control, a failed claim in a run of 7 retried in its recorded session,", runAgain("LEGION-342", 7))
	wantRunOfSeven("a failed claim in a run of 7 at the upgrade retried in a fresh session", runAgain("LEGION-340", 0))
}

// An earlier release names none of the columns and the table, so its claim write leaves each of
// them as it stands, and a run it begins would keep the number of the run before it: after a
// rollback and a roll forward that run would read as the run already told. The trigger moves the
// number for such a write, whichever release makes it. The earlier release's upsert sets
// provider_error_run and no column of 20261119114140, so a write here that sets that one column
// alone is its write, as the row's trigger sees it.
//
// The claim's first run was told before the rollback. Under the earlier release a clean turn
// resets the run and the next failed turn begins another, and a write whose reset the store never
// took drops the run below the row's: each begin moves the number, and nothing else does. Back on
// this release, the machine reads the claim with the number the trigger gave it, its next run of
// six owes its notice under that number, which stands above the told mark, and a write of this
// release that moves the number itself keeps the number it wrote: one that is not the row's plus
// one, which a trigger that renumbered this release's own begin would write instead.
func TestARunAnEarlierReleaseBeginsReadsAsANewRunWith20261119114140(t *testing.T) {
	ctx := context.Background()
	store := migratedStore(t)
	c := tmuxClaim("legion-LEGION-212-implementer")
	c.ProviderErrorRun, c.ProviderErrorRunID = 6, 1
	c.ProviderErrorsOwed = supervise.OwedNotice{Run: 1, Reason: "6 turns in a row ended in a provider error (HTTP 529)"}
	if err := store.PutClaim(ctx, c); err != nil {
		t.Fatalf("put the claim whose first run reached six: %v", err)
	}
	markTold(t, store, c, 1)

	earlierWrite := func(run int) {
		t.Helper()
		if _, err := store.Pool().Exec(ctx, "update claims set provider_error_run = $2 where token = $1", string(c.Token), run); err != nil {
			t.Fatalf("write the run %d as the earlier release does: %v", run, err)
		}
	}
	for _, step := range []struct {
		run, wantID int
		what        string
	}{
		{0, 1, "the clean turn that ends the first run"},
		{0, 1, "a second write of the run at zero"},
		{1, 2, "the first failed turn of a new run"},
		{2, 2, "the second failed turn of that run"},
		{5, 2, "a climb whose writes in between the store did not take"},
		{1, 3, "a run below the row's, whose reset the store never took"},
	} {
		earlierWrite(step.run)
		if got := readRunColumns(t, store, string(c.Token)); got != (runColumns{run: int64(step.run), runID: int64(step.wantID), told: 1}) {
			t.Fatalf("after %s the row reads %+v, want the run numbered %d, the first run told and nothing owed", step.what, got, step.wantID)
		}
	}

	restored := onlyClaim(t, store)
	if restored.ProviderErrorRunID != 3 {
		t.Fatalf("this release reads the claim's run numbered %d, want the 3 the trigger gave it", restored.ProviderErrorRunID)
	}
	restored.ProviderErrorRun = 6
	restored.ProviderErrorsOwed = supervise.OwedNotice{Run: restored.ProviderErrorRunID, Reason: "6 turns in a row ended in a provider error (HTTP 503)"}
	if err := store.PutClaim(ctx, restored); err != nil {
		t.Fatalf("put the run of six that began under the earlier release: %v", err)
	}
	if owed := owedOf(t, store, restored); len(owed) != 1 || owed[0].Run != 3 || !strings.Contains(owed[0].Reason, "HTTP 503") {
		t.Fatalf("the claim owes %+v, want the run of six numbered 3 owed and untold: the old mark must not match it", owed)
	}

	restored.ProviderErrorRun, restored.ProviderErrorRunID = 1, 9
	if err := store.PutClaim(ctx, restored); err != nil {
		t.Fatalf("put this release's first turn of the next run: %v", err)
	}
	if got := readRunColumns(t, store, string(c.Token)); got.runID != 9 || got.told != 1 {
		t.Fatalf("after this release's own begin the row reads %+v, want the 9 it wrote and the told mark untouched", got)
	}
}

// earlierReleaseTakes puts the claim of token, of issue, and then writes its run up to upTo turns
// and its state as the earlier release writes them: that release's upsert sets provider_error_run
// and the state and no column of 20261119114140, so a write here that sets those two alone is its
// write, as the row's triggers see it. An issue of "" is the project controller's own claim.
func earlierReleaseTakes(t *testing.T, store *Store, token, issue, state string, upTo int) {
	t.Helper()
	ctx := context.Background()
	c := tmuxClaim(claim.Token(token))
	c.Issue = issue
	if issue == "" {
		c.Role, c.Tree = claim.RoleController, ""
	}
	if err := store.PutClaim(ctx, c); err != nil {
		t.Fatalf("put %s: %v", token, err)
	}
	for run := 1; run <= upTo; run++ {
		if _, err := store.Pool().Exec(ctx, "update claims set provider_error_run = $2, state = $3 where token = $1", token, run, state); err != nil {
			t.Fatalf("write the run %d of %s as the earlier release does: %v", run, token, err)
		}
	}
}

// A run that an earlier release takes to six writes no owed row of that release's own, and after
// a roll forward its next turn can be a clean one, after which no turn of the run is left to owe
// it. The second trigger owes it at the earlier release's write: the run of six reads owed, under
// the number the first trigger gave it, with the earlier release's words, before this release
// writes anything of it. This release's next write of the run keeps that one row and writes its
// own words into it, and once the run is told no write of it owes it again. The control: the same
// writes that stop at five owe nothing.
//
// The cost the migration's header states is here too: such a run that the earlier release told at
// its sixth turn is owed once more, because that release kept no mark that it told it.
func TestARunAnEarlierReleaseTakesToSixIsOwedWith20261119114140(t *testing.T) {
	ctx := context.Background()
	store := migratedStore(t)
	for _, seed := range []struct {
		token, issue string
		upTo         int
		owed         bool
	}{
		{"legion-LEGION-212-implementer", "LEGION-212", 6, true},
		{"legion-LEGION-213-implementer", "LEGION-213", 5, false},
	} {
		earlierReleaseTakes(t, store, seed.token, seed.issue, "idle", seed.upTo)
		want := runColumns{run: int64(seed.upTo), runID: 1}
		if seed.owed {
			want.owed = "1: " + earlierReleaseReason(6)
		}
		if got := readRunColumns(t, store, seed.token); got != want {
			t.Errorf("after the earlier release took the idle claim %s to %d turns the row reads %+v, want %+v", seed.token, seed.upTo, got, want)
		}
	}

	restored := claimOf(t, store, "legion-LEGION-212-implementer")
	if restored.ProviderErrorsOwed != (supervise.OwedNotice{Run: 1, Reason: earlierReleaseReason(6)}) {
		t.Fatalf("this release reads the claim owing %+v, want the earlier release's run of six", restored.ProviderErrorsOwed)
	}
	restored.ProviderErrorRun = 7
	restored.ProviderErrorsOwed = supervise.OwedNotice{Run: 1, Reason: "7 turns in a row on claude-opus-5 ended in a provider error (HTTP 529)"}
	if err := store.PutClaim(ctx, restored); err != nil {
		t.Fatalf("put this release's seventh turn of the run: %v", err)
	}
	if got := readRunColumns(t, store, string(restored.Token)); got.owed != "1: "+restored.ProviderErrorsOwed.Reason {
		t.Fatalf("after this release's seventh turn the row reads %+v, want the one owed run in this release's words", got)
	}
	markTold(t, store, restored, 1)
	restored.ProviderErrorRun = 8
	if err := store.PutClaim(ctx, restored); err != nil {
		t.Fatalf("put this release's eighth turn of the run: %v", err)
	}
	if _, err := store.Pool().Exec(ctx, "update claims set provider_error_run = 9 where token = $1", string(restored.Token)); err != nil {
		t.Fatalf("write the ninth turn as the earlier release does: %v", err)
	}
	if got := readRunColumns(t, store, string(restored.Token)); got != (runColumns{run: 9, runID: 1, told: 1}) {
		t.Fatalf("after the run was told, an eighth and a ninth turn of it leave the row %+v, want it told and nothing owed", got)
	}
}

// The owed trigger's filter: a run an earlier release takes to six on a claim it fails or retires
// in the same writes is owed as a live claim's is, since either can run again, and the retry of a
// claim a run of refused turns failed starts a fresh session whose write puts the run back to zero
// (TestAFailedClaimInARunOfSevenAtTheUpgradeIsToldWhenItRunsAgainInAFreshSession says the same of
// the backfill). The project controller's own claim, which has no issue, owes nothing at a run of
// seven: no notice names it and no reader reads its rows. The control is the idle claim of
// TestARunAnEarlierReleaseTakesToSixIsOwedWith20261119114140.
func TestTheOwedTriggerOwesAFailedOrRetiredClaimAndNotTheControllers(t *testing.T) {
	store := migratedStore(t)
	for _, seed := range []struct {
		token, issue, state string
		upTo                int
		owed                bool
	}{
		{"legion-LEGION-215-implementer", "LEGION-215", "failed", 6, true},
		{"legion-LEGION-214-implementer", "LEGION-214", "retired", 7, true},
		{"legion-legion-controller", "", "idle", 7, false},
	} {
		earlierReleaseTakes(t, store, seed.token, seed.issue, seed.state, seed.upTo)
		want := runColumns{run: int64(seed.upTo), runID: 1}
		if seed.owed {
			want.owed = "1: " + earlierReleaseReason(6)
		}
		if got := readRunColumns(t, store, seed.token); got != want {
			t.Errorf("after the earlier release took the %s claim %s to %d turns the row reads %+v, want %+v", seed.state, seed.token, seed.upTo, got, want)
		}
	}
}

// The project controller's own claim has no issue, and no reader of the owed rows reads a claim
// with none (record.Store.ProviderErrorsOwed, ProjectProviderErrorsOwed), so a row of it would be
// told by nothing and deleted by nothing. Its claim writes at a run of six, two runs of it, owe no
// row. The control: a worker's claim written the same way owes one row for each run.
func TestTheControllersOwnRunsOfSixOweNoRow(t *testing.T) {
	ctx := context.Background()
	store := migratedStore(t)
	controller := supervise.Claim{Token: "legion-legion-controller", Project: "legion", Role: claim.RoleController,
		Generation: 1, State: supervise.StateIdle}
	worker := tmuxClaim("legion-LEGION-350-implementer")
	worker.Issue = "LEGION-350"
	for _, c := range []supervise.Claim{controller, worker} {
		for run := uint64(1); run <= 2; run++ {
			c.ProviderErrorRun, c.ProviderErrorRunID = 6, run
			c.ProviderErrorsOwed = supervise.OwedNotice{Run: run, Reason: "6 turns in a row ended in a provider error (HTTP 529)"}
			if err := store.PutClaim(ctx, c); err != nil {
				t.Fatalf("put run %d of %s: %v", run, c.Token, err)
			}
		}
	}
	rows := func(token claim.Token) int {
		var n int
		if err := store.Pool().QueryRow(ctx, "select count(*) from provider_errors_owed where claim_token = $1", string(token)).Scan(&n); err != nil {
			t.Fatalf("count the owed rows of %s: %v", token, err)
		}
		return n
	}
	if n := rows(worker.Token); n != 2 {
		t.Fatalf("the control: the worker's claim holds %d owed rows after two runs of six, want 2", n)
	}
	if n := rows(controller.Token); n != 0 {
		t.Fatalf("the controller's own claim holds %d owed rows after two runs of six, want none: no reader tells or deletes them", n)
	}
}

// markTold marks c's runs told through run, as admission's write of their notice does.
func markTold(t *testing.T, store *Store, c supervise.Claim, run uint64) {
	t.Helper()
	if err := store.Tx(context.Background(), func(tx pgx.Tx) error {
		return record.NewStore().MarkProviderErrorsTold(context.Background(), tx, c.Token, run)
	}); err != nil {
		t.Fatalf("mark %s told through run %d: %v", c.Token, run, err)
	}
}

// owedOf is what admission reads c owes (record.Store.ProviderErrorsOwed), nil for nothing.
func owedOf(t *testing.T, store *Store, c supervise.Claim) []supervise.OwedNotice {
	t.Helper()
	var owed *record.OwedProviderErrors
	if err := store.Tx(context.Background(), func(tx pgx.Tx) (err error) {
		owed, err = record.NewStore().ProviderErrorsOwed(context.Background(), tx, c.Token)
		return err
	}); err != nil {
		t.Fatalf("read what %s owes: %v", c.Token, err)
	}
	if owed == nil {
		return nil
	}
	return owed.Runs
}

// claimOf is the claim of token as this release reads it back (Store.Claims).
func claimOf(t *testing.T, store *Store, token string) supervise.Claim {
	t.Helper()
	claims, err := store.Claims(context.Background())
	if err != nil {
		t.Fatalf("claims: %v", err)
	}
	for _, c := range claims {
		if string(c.Token) == token {
			return c
		}
	}
	t.Fatalf("no claim %s among %d", token, len(claims))
	return supervise.Claim{}
}
