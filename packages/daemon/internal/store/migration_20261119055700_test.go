package store

import (
	"context"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/store/migrations"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// gateMessageVersion is the version of the migration this file tests, so the test can migrate
// through everything before it whatever lands beside it.
const gateMessageVersion = 20261119055700

// 20261119055700 adds the mark that a merger's pending task is the design gate's own
// (supervise.Delivery.GateMessage, dispatch://INFRA-2040 R8), whose send puts the gate's whole
// message in front of it. Before it the column does not exist. After it a delivery the release
// before wrote reads without the mark, and a delivery with the mark reads back with it through a
// re-put that replaces it and through a store reopened as a daemon restart reopens it, so a task
// the close queued and the opening overtook still says, at its send, that the gate opened.
//
// The way back in the migration's header is one transaction from begin to commit. It takes the
// mark off each supervise start row, which the release before decodes strictly, drops the column
// and deletes the version's row, and the roll-forward after it adds the column again at its
// default.
func TestADeliveryCarriesTheMarkOfTheDesignGatesOwnTaskAfter20261119055700(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, gateMessageVersion-1)
	// The claim is written as the release before writes it, in SQL: this daemon's PutClaim writes
	// the columns of every migration it embeds, a later one's among them (20261120042902's
	// claims.agent_resume_generation), which the database migrated only this far does not hold.
	c := tmuxClaim("legion-LEGION-208-merger")
	if _, err := store.Pool().Exec(ctx, `insert into claims (token, project, tree, issue, role, generation, session, session_file, state,
		launch_failures, prompt_failures, prompt_retires, uncertain_streak, serving_generation, given_entry, served_entry)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9, 0, 0, 0, 0, $6, 0, 0)`,
		string(c.Token), c.Project, c.Tree, c.Issue, string(c.Role), c.Generation, c.Session, c.SessionFile, string(c.State)); err != nil {
		t.Fatalf("put the claim under the release before: %v", err)
	}
	if _, err := store.Pool().Exec(ctx, `insert into pending_task_deliveries (claim_token, delivery_id, task, phase, generation, queued_at)
		values ($1, 'outbox:9', 'Merge the head.', 'merging', 1, now())`, string(c.Token)); err != nil {
		t.Fatalf("write the delivery as the release before writes it: %v", err)
	}
	if _, err := store.Pool().Exec(ctx, `update pending_task_deliveries set gate_message = true`); err == nil ||
		!strings.Contains(err.Error(), "gate_message") {
		t.Fatalf("writing gate_message before the migration = %v, want the column to be unknown: the fixture is not the old shape", err)
	}

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the gate's mark: %v", err)
	}
	if got := onlyClaim(t, store).Pending; got == nil || got.ID != "outbox:9" || got.GateMessage {
		t.Fatalf("the delivery after the migration = %+v, want outbox:9 with no mark: the release before queued no gate task", got)
	}
	marked := supervise.Delivery{ID: "outbox:10", Task: "The design gate of LEGION-208 moved.", QueuedAt: at(1), GateMessage: true}
	for _, put := range []supervise.Delivery{marked, {ID: "outbox:11", Task: "Merge the head.", Phase: phase.Merging, QueuedAt: at(2)}, marked} {
		if err := store.PutDelivery(ctx, c.Token, put); err != nil {
			t.Fatalf("put the delivery %s: %v", put.ID, err)
		}
		for _, st := range []*Store{store, reopenStore(t, store)} {
			if got := onlyClaim(t, st).Pending; got == nil || got.ID != put.ID || got.GateMessage != put.GateMessage {
				t.Fatalf("the delivery read back = %+v, want %s with the mark %v", got, put.ID, put.GateMessage)
			}
		}
	}

	var row int64
	if err := store.Pool().QueryRow(ctx, `insert into outbox (kind, issue, payload, attempts, next_at, last_error)
		values ('supervise', 'LEGION-208', '{"op":"start","tree":"LEGION-208","role":"merger","generation":1,"task":"The design gate of LEGION-208 moved.","gateMessage":true}',
		0, now(), '') returning id`).Scan(&row); err != nil {
		t.Fatalf("queue the gate's own start: %v", err)
	}
	if _, err := store.Pool().Exec(ctx, gateMessageWayBack(t)); err != nil {
		t.Fatalf("run the way back: %v", err)
	}
	var keyed, column, recorded bool
	if err := store.Pool().QueryRow(ctx, `select payload ? 'gateMessage' from outbox where id = $1`, row).Scan(&keyed); err != nil {
		t.Fatalf("read the gate's start after the way back: %v", err)
	}
	if err := store.Pool().QueryRow(ctx, `select exists (select 1 from information_schema.columns
		where table_name = 'pending_task_deliveries' and column_name = 'gate_message')`).Scan(&column); err != nil {
		t.Fatalf("read the delivery's columns after the way back: %v", err)
	}
	if err := store.Pool().QueryRow(ctx, `select exists (select 1 from schema_version where version = $1)`, gateMessageVersion).Scan(&recorded); err != nil {
		t.Fatalf("read the version rows after the way back: %v", err)
	}
	if keyed || column || recorded {
		t.Fatalf("after the way back the start carries gateMessage: %v, pending_task_deliveries.gate_message stands: %v, version %d is recorded: %v; want none of them",
			keyed, column, gateMessageVersion, recorded)
	}
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("roll forward after the way back: %v", err)
	}
	if got := onlyClaim(t, store).Pending; got == nil || got.ID != marked.ID || got.GateMessage {
		t.Fatalf("the delivery after the roll-forward = %+v, want %s at the column's default, with no mark", got, marked.ID)
	}
}

// gateMessageWayBack is the transaction the header of 20261119055700 gives as its way back: the
// lines of its psql heredoc from begin to commit, the comment's indent taken off.
func gateMessageWayBack(t *testing.T) string {
	t.Helper()
	all, err := migrations.All()
	if err != nil {
		t.Fatalf("read the embedded migrations: %v", err)
	}
	for _, migration := range all {
		if migration.Version != gateMessageVersion {
			continue
		}
		var block []string
		for _, line := range strings.Split(migration.SQL, "\n") {
			statement, comment := strings.CutPrefix(line, "--   ")
			switch {
			case comment && statement == "begin;":
				block = []string{statement}
			case comment && block != nil:
				block = append(block, statement)
				if statement == "commit;" {
					return strings.Join(block, "\n")
				}
			}
		}
		t.Fatalf("%s carries no way back from begin to commit", migration.Name)
	}
	t.Fatalf("no embedded migration is %d", gateMessageVersion)
	return ""
}
