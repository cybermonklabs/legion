package store

import (
	"context"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/record"
)

// reachNoticeKindsVersion is the version of the migration this file tests, so the test can
// migrate through everything before it whatever lands beside it.
const reachNoticeKindsVersion = 20261116082000

// The notices this change adds are queued under two kinds of their own, which the release before it
// never runs (record.OutboxKindReachNotice, record.OutboxKindReachControllerNotice): the outbox
// refuses a kind its constraint does not name, so neither could be queued before this migration,
// which is the negative control, read before it runs. After it both are queued, and so is every
// kind the constraint already named and the role-holders publication both lists name
// (20261116165926), since the migration rewrites the list whole and a kind left out would stop its
// own effect. A kind that is none is still refused.
//
// A database a build of this change migrated before the two kinds existed can hold such a notice
// under `notice` or `controller_notice`, where this release refuses to decode it, because a row's
// kind and its notice's kind must agree (record.DecodeOutboxPayload). The migration moves each such
// row to its own kind, where it decodes, and moves no other notice.
func TestTheOutboxTakesTheReachNoticeKindsWith20261116082000(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, reachNoticeKindsVersion-1)
	queue := func(kind, payload string) error {
		_, err := store.Pool().Exec(ctx, `insert into outbox (kind, issue, payload, attempts, next_at, last_error)
			values ($1, 'LEGION-208', $2::jsonb, 0, now(), '')`, kind, payload)
		return err
	}
	added := []string{"reach_notice", "reach_controller_notice"}
	for _, kind := range added {
		err := queue(kind, `{}`)
		if err == nil || !strings.Contains(err.Error(), "outbox_kind_check") {
			t.Fatalf("queueing a %s row before the migration = %v, want the kind constraint to refuse it: it is a row the migration exists for", kind, err)
		}
		t.Logf("before the migration a %s row is refused: %v", kind, err)
	}
	written := map[string]struct{ kind, payload string }{
		"list":      {"controller_notice", `{"kind":"unreachable-claims","unreachable":[{"claim":"legion-legion-209-implementer"}]}`},
		"locked":    {"controller_notice", `{"kind":"worker-locked-out","role":"implementer","claim":"legion-legion-209-implementer","reason":"r"}`},
		"architect": {"notice", `{"kind":"claim-out-of-reach","role":"implementer","claim":"legion-legion-209-implementer","reason":"r"}`},
		"tick":      {"controller_notice", `{"kind":"tick"}`},
		"finished":  {"notice", `{"kind":"phase-finished","role":"implementer","summary":"done"}`},
	}
	ids := map[string]int64{}
	for name, row := range written {
		var id int64
		if err := store.Pool().QueryRow(ctx, `insert into outbox (kind, issue, payload, attempts, next_at, last_error)
			values ($1, 'LEGION-209', $2::jsonb, 0, now(), '') returning id`, row.kind, row.payload).Scan(&id); err != nil {
			t.Fatalf("queue the %s row a build before the kinds wrote: %v", name, err)
		}
		ids[name] = id
	}
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the reach notice kinds: %v", err)
	}
	for name, want := range map[string]record.OutboxKind{
		"list": record.OutboxKindReachControllerNotice, "locked": record.OutboxKindReachControllerNotice,
		"architect": record.OutboxKindReachNotice, "tick": record.OutboxKindControllerNotice, "finished": record.OutboxKindNotice,
	} {
		var row record.OutboxRow
		var kind string
		if err := store.Pool().QueryRow(ctx, "select id, kind, payload from outbox where id = $1", ids[name]).Scan(&row.ID, &kind, &row.Payload); err != nil {
			t.Fatalf("read the %s row back: %v", name, err)
		}
		row.Kind = record.OutboxKind(kind)
		if row.Kind != want {
			t.Errorf("after the migration the %s row is a %s row, want %s", name, row.Kind, want)
		}
		if _, err := record.DecodeOutboxPayload(row); err != nil {
			t.Errorf("after the migration the %s row does not decode: %v", name, err)
		}
	}
	for _, kind := range append([]string{
		"dispatch_status", "dispatch_message", "notice", "controller_notice", "supervise", "gate_seed",
		"linger_close", "workspace_remove", "issue_branch", "issue_suspend", "check_wake",
		"check_look", "check_bound", "role_message", "phase_ended", "issue_bundle", "issue_release",
		"gated_steps_publish", "claim_release", "hook_prompt", "design_gates_publish", "update_ask",
		"merge_queue_read", "merge_queue_removal", "role_holders_publish",
	}, added...) {
		if err := queue(kind, `{}`); err != nil {
			t.Fatalf("queue a %s row after the migration: %v", kind, err)
		}
	}
	t.Logf("after the migration the outbox takes every kind, reach_notice and reach_controller_notice among them")
	if err := queue("quiesce", `{}`); err == nil || !strings.Contains(err.Error(), "outbox_kind_check") {
		t.Fatalf("queueing a row of a kind that is none = %v, want the constraint to refuse it", err)
	}
}
