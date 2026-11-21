package store

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// issueFactsVersion is the version of the migration this file tests, so the test can migrate
// through everything before it whatever lands beside it.
const issueFactsVersion = 20261120095128

// 20261120095128 gives each issue its own facts (dispatch://INFRA-4730): six columns of issues,
// three of claims, two of outbox, the issue lifecycle and the conversion's record, and the outbox
// kind retention_end. Before it the claims table has no issue_epoch and the outbox refuses a
// retention_end row, which are the negative controls read first.
//
// The claims that stand at the upgrade keep running: each issue a claim not retired names, whose
// claim's tree has a lifecycle whose cleanup has not started, opens its own at epoch one under
// that tree's authority — the workflow's for an issue whose claims stand under trees of both — and
// every claim of such an issue binds it, a retired one among them, so each passes its launch
// check. An issue whose only claim retired, one under a tree whose cleanup is reserved, and the
// project controller's claim open and bind nothing. A record written before reads back with none
// of its new facts, and every kind the outbox took before is taken still, with retention_end.
func TestTheIssueFactsMigrationOpensEachLiveClaimsIssueAndTakesTheRetentionEnd(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, issueFactsVersion-1)
	for _, key := range []string{"LEGION-208", "LEGION-209", "LEGION-300"} {
		if _, err := store.Pool().Exec(ctx, `insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
			values ($1, case when $1 = 'LEGION-209' then 'LEGION-208' else $1 end, 'LEGION', $1, 'implementing', 1, 'in_progress', 'A', 1)`, key); err != nil {
			t.Fatalf("record %s before the migration: %v", key, err)
		}
	}
	for _, row := range []struct {
		tree, authority string
		started         bool
	}{
		{"LEGION-208", "workflow", false},
		{"LEGION-300", "workflow", true},
		{"LEGION-400", "operator", false},
		{"LEGION-500", "workflow", false},
		{"LEGION-510", "operator", false},
	} {
		if _, err := store.Pool().Exec(ctx, `insert into tree_lifecycles (project, tree, epoch, authority, cleanup_started)
			values ('legion', $1, 1, $2, $3)`, row.tree, row.authority, row.started); err != nil {
			t.Fatalf("seed the lifecycle of %s: %v", row.tree, err)
		}
	}
	controller := claim.ControllerToken("legion")
	claims := []supervise.Claim{
		{Token: "legion-legion-legion-209-implementer", Project: "legion", Tree: "LEGION-208", Issue: "LEGION-209", Role: claim.RoleImplementer, State: supervise.StateIdle},
		{Token: "legion-legion-legion-208-architect", Project: "legion", Tree: "LEGION-208", Issue: "LEGION-208", Role: claim.RoleArchitect, State: supervise.StateRetired},
		{Token: "legion-legion-legion-300-architect", Project: "legion", Tree: "LEGION-300", Issue: "LEGION-300", Role: claim.RoleArchitect, State: supervise.StateIdle},
		{Token: "legion-legion-legion-400-architect", Project: "legion", Tree: "LEGION-400", Issue: "LEGION-400", Role: claim.RoleArchitect, State: supervise.StateIdle},
		{Token: "legion-legion-legion-501-implementer", Project: "legion", Tree: "LEGION-510", Issue: "LEGION-501", Role: claim.RoleImplementer, State: supervise.StateIdle},
		{Token: "legion-legion-legion-501-tester", Project: "legion", Tree: "LEGION-500", Issue: "LEGION-501", Role: claim.RoleTester, State: supervise.StateRetired},
		{Token: "legion-legion-legion-501-planner", Project: "legion", Tree: "LEGION-500", Issue: "LEGION-501", Role: claim.RolePlanner, State: supervise.StateIdle},
		{Token: controller, Project: "legion", Role: claim.RoleController, State: supervise.StateIdle},
	}
	for _, c := range claims {
		if _, err := store.Pool().Exec(ctx, `insert into claims (token, project, tree, issue, role, generation, session, session_file,
			state, launch_failures, prompt_failures, prompt_retires, uncertain_streak)
			values ($1, $2, $3, $4, $5, 1, '', '', $6, 0, 0, 0, 0)`,
			string(c.Token), c.Project, c.Tree, c.Issue, string(c.Role), string(c.State)); err != nil {
			t.Fatalf("seed claim %s: %v", c.Token, err)
		}
	}
	if _, err := store.Pool().Exec(ctx, "select issue_epoch from claims"); err == nil || !strings.Contains(err.Error(), "issue_epoch") {
		t.Fatalf("reading claims.issue_epoch before the migration = %v, want the column to be unknown: the fixture is not the old shape", err)
	}
	queue := func(kind string) error {
		_, err := store.Pool().Exec(ctx, `insert into outbox (kind, issue, payload, attempts, next_at, last_error)
			values ($1, 'LEGION-208', '{}', 0, now(), '')`, kind)
		return err
	}
	if err := queue("retention_end"); err == nil || !strings.Contains(err.Error(), "outbox_kind_check") {
		t.Fatalf("queueing a retention_end row before the migration = %v, want the kind constraint to refuse it", err)
	}

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the issue facts: %v", err)
	}

	lifecycles := map[string]string{}
	rows, err := store.Pool().Query(ctx, `select issue, authority || ':' || epoch || ':' || cleanup_started from issue_lifecycles where project = 'legion'`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var issue, state string
		if err := rows.Scan(&issue, &state); err != nil {
			t.Fatal(err)
		}
		lifecycles[issue] = state
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	wantLifecycles := map[string]string{
		"LEGION-209": "workflow:1:false",
		"LEGION-400": "operator:1:false",
		"LEGION-501": "workflow:1:false",
	}
	if !reflect.DeepEqual(lifecycles, wantLifecycles) {
		t.Fatalf("issue lifecycles after the migration = %v, want %v: one open at epoch one for each issue a live claim names under an open tree lifecycle, under that tree's authority, the workflow's where both stand", lifecycles, wantLifecycles)
	}

	loaded, err := store.Claims(ctx)
	if err != nil {
		t.Fatal(err)
	}
	wantEpochs := map[claim.Token]uint64{
		"legion-legion-legion-209-implementer": 1, "legion-legion-legion-208-architect": 0,
		"legion-legion-legion-300-architect": 0, "legion-legion-legion-400-architect": 1,
		"legion-legion-legion-501-implementer": 1, "legion-legion-legion-501-tester": 1,
		"legion-legion-legion-501-planner": 1, controller: 0,
	}
	if len(loaded) != len(wantEpochs) {
		t.Fatalf("the store read %d claims after the migration, want the %d seeded", len(loaded), len(wantEpochs))
	}
	for _, c := range loaded {
		if c.IssueEpoch != wantEpochs[c.Token] || c.LaunchTree != "" || c.HoldsGate {
			t.Errorf("claim %s after the migration = issue epoch %d, launch tree %q, gate %t; want issue epoch %d, no launch tree and no gate",
				c.Token, c.IssueEpoch, c.LaunchTree, c.HoldsGate, wantEpochs[c.Token])
		}
		if c.IssueEpoch == 0 || c.State == supervise.StateRetired {
			continue
		}
		if err := store.CheckLaunch(ctx, c); err != nil {
			t.Errorf("launch check of %s after the migration: %v", c.Token, err)
		}
	}

	var issue *record.Issue
	if err := store.Tx(ctx, func(tx pgx.Tx) error {
		var err error
		issue, err = record.NewStore().Issue(ctx, tx, "LEGION-209")
		return err
	}); err != nil {
		t.Fatalf("read LEGION-209 after the migration: %v", err)
	}
	if issue.RetainedUntil != nil || issue.CarriedGeneration != 0 || issue.CarriedGate || issue.ParkedByArchitect || issue.PendingClose != nil || issue.SeenSeq != 0 {
		t.Fatalf("LEGION-209 recorded before the migration reads back %+v, want none of its own facts set", issue)
	}

	for _, kind := range []string{
		"dispatch_status", "dispatch_message", "notice", "controller_notice", "supervise", "gate_seed",
		"linger_close", "workspace_remove", "issue_branch", "issue_suspend", "check_wake",
		"check_look", "check_bound", "role_message", "phase_ended", "issue_bundle", "issue_release",
		"gated_steps_publish", "claim_release", "hook_prompt", "design_gates_publish", "update_ask",
		"merge_queue_read", "merge_queue_removal", "role_holders_publish", "step_record", "plan_record",
		"conflict_confirm", "merge_forward", "pending_basis_seed", "reparent_seed", "retention_end",
	} {
		if err := queue(kind); err != nil {
			t.Fatalf("queue a %s row after the migration: %v", kind, err)
		}
	}
	if err := queue("quiesce"); err == nil || !strings.Contains(err.Error(), "outbox_kind_check") {
		t.Fatalf("queueing a row of a kind that is none = %v, want the constraint to refuse it", err)
	}
	var unattempted int
	if err := store.Pool().QueryRow(ctx, `select count(*) from outbox where attempt_key is null and attempt_body is null`).Scan(&unattempted); err != nil || unattempted != 32 {
		t.Fatalf("outbox rows with no attempt after the migration = %d, %v; want every one of the 32 queued", unattempted, err)
	}

	if _, err := store.Pool().Exec(ctx, `insert into tree_conversions (project, step) values ('legion', 0)`); err == nil || !strings.Contains(err.Error(), "tree_conversions") {
		t.Fatalf("recording step 0 of a conversion = %v, want the check to refuse a step that is none", err)
	}
	if _, err := store.Pool().Exec(ctx, `insert into tree_conversions (project, step) values ('legion', 1)`); err != nil {
		t.Fatalf("record the first step of a conversion: %v", err)
	}
}
