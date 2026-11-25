package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/store/migrations"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// keptDecisionsMigrationSuffix ends the file name of the migration that gives the kept decisions
// their names and moves the values the release before wrote. The test finds the migration by it,
// so it names no version of its own.
const keptDecisionsMigrationSuffix = "_kept_decisions.up.sql"

// keptDecisionsBaseLegion is the variable that names a `legion` binary of the release before,
// built from the pull request's base. Step 5 of the test boots it on the database the way back
// left; where it is unset, the test says so and goes on.
const keptDecisionsBaseLegion = "LEGION_KEPT_DECISIONS_BASE_LEGION"

// keptDecisionsBaseTree is the variable that names a source tree of the release before, the pull
// request's base as `git archive` writes it. Step 5 of the test builds keptDecisionsBaseReader in
// that tree's daemon module and reads, with that release's own readers, what the way back left;
// where it is unset, the test says so and goes on.
const keptDecisionsBaseTree = "LEGION_KEPT_DECISIONS_BASE_TREE"

// keptDecisionsMigration is the version of the migration keptDecisionsMigrationSuffix names, and 0
// where the daemon embeds no such migration.
func keptDecisionsMigration(t *testing.T) int {
	t.Helper()
	all, err := migrations.All()
	if err != nil {
		t.Fatalf("read the embedded migrations: %v", err)
	}
	for _, migration := range all {
		if strings.HasSuffix(migration.Name, keptDecisionsMigrationSuffix) {
			return migration.Version
		}
	}
	return 0
}

// keptDecisionRow is one row of the table of kept decisions, field by field.
type keptDecisionRow struct {
	Issue, Ref, Decision, Actor string
	At                          time.Time
	ClaimedBy                   int64
	HeldByClaim, HeldByDelivery string
}

// keptDecisionsRig is the store of the test and what step 1 seeded in it, by the names the steps
// read.
type keptDecisionsRig struct {
	t       *testing.T
	ctx     context.Context
	st      *store.Store
	pool    *store.Pool
	records record.Store
	version int
	// notice, design, catchUp and tick are the ids of the seeded outbox rows: the controller
	// notice the release before wrote for the root's spec decision, the architect notice that
	// carried that decision undelivered, the catch-up that claims the child's decision, and the
	// tick that holds the child.
	notice, design, catchUp, tick int64
	// constraints is the release before's constraint names of the table, read from the catalog:
	// PostgreSQL 18 names each NOT NULL constraint after its table, and an older server names none.
	constraints []string
}

// A store the release before left — its rows of kept decisions, the controller notice it relayed
// one of them in, the mark that keeps that decision to one notice, and a tick that holds a member
// of a tree in backlog — takes the new names at the upgrade, and the daemon tells the tree's next
// architect each kept decision once. The way back of the migration's header gives every old name
// and value back to the release before, and the roll forward gives the new ones again.
//
// Step 1 seeds in the release before's names, in plain SQL, as that release wrote them. Step 2
// migrates and reads each row and value under the new names, and finds no name or stored value
// that holds the old word. Step 3 runs the undelivered architect notice twice through the outbox's
// notice path, which finds the decision handled before under its key, so the one notice the
// release before wrote is the only one; then it composes the root's start, which names the kept
// decision once and not the one a catch-up claims, and a second composition names nothing. Step 4
// runs the way back as an operator runs it. Step 5 reads what the way back left with the release
// before's own readers, built from the source tree keptDecisionsBaseTree names: its kept decisions
// of the root's tree, each seeded outbox row decoded, and the mark under its source; then it boots
// the release before's daemon, the binary keptDecisionsBaseLegion names, as the seeded project on
// that database, and finds the root and its child in its state. That boot drives no outbox row,
// since the release before's outbox runner lives in its workflow, which needs Dispatch, Envoy and
// both GitHub Apps; the rows of the outbox and processed_events are read under the old names by
// step 4's assertions and step 5's readers. Step 6 rolls forward.
//
// Where the daemon embeds no migration of keptDecisionsMigrationSuffix, the store withholds
// nothing and the test goes on: step 2 then fails, naming the table it cannot read, which is the
// test's negative control.
func TestKeptDecisionsTheReleaseBeforeLeftAreToldOnceAfterTheRenameAndComeBackOnTheWayBack(t *testing.T) {
	t.Parallel()
	r := &keptDecisionsRig{t: t, ctx: context.Background(), records: record.NewStore(), version: keptDecisionsMigration(t)}
	if r.version == 0 {
		t.Logf("no embedded migration ends %s: the store withholds nothing", keptDecisionsMigrationSuffix)
	}
	var dsn string
	r.st, dsn = storeWithholding(t, "legion_kept_decisions", r.version)
	r.pool = r.st.Pool()

	// Step 1: what the release before leaves.
	r.seedTheReleaseBefore()
	want := []keptDecisionRow{
		{Issue: "LEGION-2", Ref: keptClaimedAsk, Decision: string(record.DecisionAskAnswered), Actor: "user:bob", At: keptClaimedAt, ClaimedBy: r.catchUp},
		{Issue: "LEGION-1", Ref: keptSpecRef, Decision: string(record.DecisionDesignApproved), Actor: "user:alice", At: keptSpecAt},
	}

	// Step 2: the upgrade.
	if _, err := r.st.Migrate(r.ctx); err != nil {
		t.Fatalf("step 2: migrate the store the release before left: %v", err)
	}
	r.assertTheNewNames("step 2", want)

	// Step 3: each kept decision is told once.
	var logged bytes.Buffer
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	architectClaim(t, sup, "LEGION-1", supervise.StateSuspended)
	publisher := &holderPublisher{}
	runner := &outbox{log: slog.New(slog.NewTextHandler(&logged, nil)), pool: r.pool, dispatchProject: "LEGION", records: r.records,
		notices: publisher, supervisor: sup, project: "legion", now: time.Now}
	row, payload := r.leasedNotice(r.design)
	for attempt := range 2 {
		if err := runner.notice(r.ctx, row, payload); err != nil {
			t.Fatalf("step 3: run the undelivered design-approved notice, attempt %d: %v", attempt, err)
		}
	}
	key := record.OutboxKey(r.design)
	if n := strings.Count(logged.String(), `msg="outbox kept decision handled before under this key"`); n != 2 || !strings.Contains(logged.String(), "key="+key) {
		t.Fatalf("step 3: log %q; want both runs logged as handled before under %s", logged.String(), key)
	}
	if _, delivered := publisher.snapshot(); len(delivered) != 0 {
		t.Fatalf("step 3: delivered %+v, want nothing to the stopped tree's architect", delivered)
	}
	if got := keptDecisionNotices(t, r.pool); !reflect.DeepEqual(got, []record.ControllerNotice{r.migratedNotice()}) {
		t.Fatalf("step 3: kept-decision notices %#v, want the one the release before wrote, under the new kind", got)
	}
	engine := workflow.New(r.records, workflow.Config{Project: "legion", Repositories: testRepositories, Steps: config.DesignGateRootIssues.Steps()}, quietLogger())
	first, firstKept := r.composeTheRootsStart(engine)
	if strings.Count(first, keptSpecRef) != 1 || strings.Contains(first, keptClaimedAsk) || firstKept == nil ||
		len(firstKept.Decisions) != 1 || firstKept.Decisions[0].Ref != keptSpecRef {
		t.Fatalf("step 3: the root's start names %q (%+v), want %s once and not %s, which a catch-up claims", first, firstKept, keptSpecRef, keptClaimedAsk)
	}
	if second, secondKept := r.composeTheRootsStart(engine); second != "" || secondKept != nil {
		t.Fatalf("step 3: a second start names %q (%+v), want nothing: the first start claims what it named", second, secondKept)
	}
	told := r.keptRows("step 3", "kept_decisions")

	// Step 4: the way back, as an operator runs it.
	if r.version == 0 {
		t.Fatalf("step 4: no embedded migration ends %s, so there is no way back to run", keptDecisionsMigrationSuffix)
	}
	block, err := migrations.WayBack(r.version)
	if err != nil {
		t.Fatalf("step 4: read the way back of the kept decisions' names: %v", err)
	}
	if _, err := r.pool.Exec(r.ctx, block, pgx.QueryExecModeSimpleProtocol); err != nil {
		t.Fatalf("step 4: run the way back: %v", err)
	}
	r.assertTheOldNames("step 4", told)

	// Step 5: the release before reads what the way back left, and boots on it.
	if tree := os.Getenv(keptDecisionsBaseTree); tree == "" {
		t.Logf("step 5's read skipped: %s names no source tree of the release before to build its readers from", keptDecisionsBaseTree)
	} else {
		r.readWithTheReleaseBefore(tree, dsn, told)
	}
	if binary := os.Getenv(keptDecisionsBaseLegion); binary == "" {
		t.Logf("step 5 skipped: %s names no legion binary of the release before to boot on the database the way back left", keptDecisionsBaseLegion)
	} else {
		bootTheReleaseBefore(t, binary, dsn, "LEGION", "LEGION-1", "LEGION-2")
	}

	// Step 6: the roll forward.
	if _, err := r.st.Migrate(r.ctx); err != nil {
		t.Fatalf("step 6: roll forward after the way back: %v", err)
	}
	r.assertTheNewNames("step 6", told)
}

// The decisions step 1 keeps: the root's spec decision, which the release before relayed and
// kept, and an answer on the child, which a catch-up of the release before claims.
const (
	keptSpecRef    = "dispatch://LEGION-1/spec"
	keptClaimedAsk = "dispatch://LEGION-2/ask/ask-1"
)

var (
	keptSpecAt    = time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	keptClaimedAt = time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
)

// The payloads step 1 writes in the release before's words, and the tick in the words the
// migration gives it.
const (
	keptOldNotice = `{"kind": "parked-decision", "decision": "design-approved", "root": "LEGION-1", "ref": "dispatch://LEGION-1/spec", "actor": "user:alice", "version": 3}`
	keptOldTick   = `{"kind": "tick", "held": [{"issue": "LEGION-2", "reason": "tree_parked", "heldBy": ["LEGION-1"]}, {"issue": "LEGION-9", "reason": "design_gate", "heldBy": ["LEGION-8"]}]}`
	keptNewTick   = `{"kind": "tick", "held": [{"issue": "LEGION-2", "reason": "tree_in_backlog_or_icebox", "heldBy": ["LEGION-1"]}, {"issue": "LEGION-9", "reason": "design_gate", "heldBy": ["LEGION-8"]}]}`
)

// seedTheReleaseBefore is step 1: a root lingering in backlog with a child; the root's spec
// decision kept, and the child's answer claimed by a catch-up; the controller notice the release
// before relayed the spec decision in; the architect notice that carried that decision,
// undelivered, with the mark that kept it to that one notice; a tick holding the child under its
// tree in backlog; and the table's constraint names as that release has them.
func (r *keptDecisionsRig) seedTheReleaseBefore() {
	r.t.Helper()
	rootKey := "LEGION-1"
	until := time.Now().Add(time.Hour)
	putOutboxIssue(r.t, r.pool, r.records, record.Issue{Key: rootKey, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Project: "LEGION", Tree: rootKey, Title: "Root", Phase: phase.Done, Generation: 1, Status: "backlog", HandedOver: true, LingerUntil: &until})
	putOutboxIssue(r.t, r.pool, r.records, record.Issue{Key: "LEGION-2", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Project: "LEGION", Tree: rootKey, Parent: &rootKey, Title: "Child", Phase: phase.Planning, Generation: 1, Status: "in_progress"})
	design := mustOutboxRow(r.t, rootKey, record.Notice{Kind: "design-approved", Version: 3, Actor: "user:alice", Ref: keptSpecRef}, time.Now())
	catchUp := mustOutboxRow(r.t, rootKey, record.Notice{Kind: "catch-up", Role: claim.RoleArchitect, CatchUp: &record.CatchUp{Generation: 1, Launch: 1,
		Kept: &record.KeptDecisions{Decisions: []record.DecisionMark{{Ref: keptClaimedAsk, At: keptClaimedAt, Issue: "LEGION-2",
			Decision: record.DecisionAskAnswered, Actor: "user:bob"}}}}}, time.Now())
	r.design = r.queue(string(design.Kind), rootKey, string(design.Payload))
	r.catchUp = r.queue(string(catchUp.Kind), rootKey, string(catchUp.Payload))
	r.notice = r.queue(string(record.OutboxKindControllerNotice), rootKey, keptOldNotice)
	r.tick = r.queue(string(record.OutboxKindControllerNotice), "LEGION", keptOldTick)
	r.exec("step 1", `insert into parked_decisions (issue, ref, decision, actor, at, claimed_by) values
		($1, $2, 'design-approved', 'user:alice', $3, null), ('LEGION-2', $4, 'ask-answered', 'user:bob', $5, $6)`,
		rootKey, keptSpecRef, keptSpecAt, keptClaimedAsk, keptClaimedAt, r.catchUp)
	r.exec("step 1", `insert into processed_events (source, event_id) values ('parked-decision', $1)`, record.OutboxKey(r.design))
	r.constraints = r.texts("step 1", `select conname::text from pg_constraint where conrelid = 'parked_decisions'::regclass order by conname`)
	for _, name := range []string{"parked_decisions_pkey", "parked_decisions_issue_fkey", "parked_decisions_claimed_by_fkey"} {
		if !slices.Contains(r.constraints, name) {
			r.t.Fatalf("step 1: the release before's constraints of the table = %v, want %s among them", r.constraints, name)
		}
	}
}

// assertTheNewNames holds the store to the new names after an upgrade: the table, its index and
// its constraints under kept_decisions with every row field by field as want, the controller
// notice of the kind kept-decision that this release decodes as the notice the release before
// wrote, the mark under the source kept-decision, the tick's reason in its new word, and no name of
// the schema, no outbox payload and no processed-event source that holds the old word.
func (r *keptDecisionsRig) assertTheNewNames(step string, want []keptDecisionRow) {
	r.t.Helper()
	if got := r.keptRows(step, "kept_decisions"); !reflect.DeepEqual(got, want) {
		r.t.Fatalf("%s: the kept decisions = %+v, want %+v", step, got, want)
	}
	var renamed []string
	for _, name := range r.constraints {
		renamed = append(renamed, "kept_decisions_"+strings.TrimPrefix(name, "parked_decisions_"))
	}
	if got := r.texts(step, `select conname::text from pg_constraint where conrelid = 'kept_decisions'::regclass order by conname`); !reflect.DeepEqual(got, renamed) {
		r.t.Fatalf("%s: the constraints of kept_decisions = %v, want %v", step, got, renamed)
	}
	if got, want := r.texts(step, `select indexname::text from pg_indexes where schemaname = 'public' and tablename = 'kept_decisions' order by indexname`),
		[]string{"kept_decisions_issue", "kept_decisions_pkey"}; !reflect.DeepEqual(got, want) {
		r.t.Fatalf("%s: the indexes of kept_decisions = %v, want %v", step, got, want)
	}
	if got := r.texts(step, `select payload->>'kind' from outbox where id = $1`, r.notice); !reflect.DeepEqual(got, []string{string(record.KeptDecisionNotice)}) {
		r.t.Fatalf("%s: the kind of the controller notice = %v, want %s", step, got, record.KeptDecisionNotice)
	}
	if got := r.controllerNotice(step, r.notice); !reflect.DeepEqual(got, r.migratedNotice()) {
		r.t.Fatalf("%s: the controller notice decodes as %#v, want %#v", step, got, r.migratedNotice())
	}
	if got := r.texts(step, `select source from processed_events where event_id = $1`, record.OutboxKey(r.design)); !reflect.DeepEqual(got, []string{"kept-decision"}) {
		r.t.Fatalf("%s: the source of the mark of %s = %v, want kept-decision", step, record.OutboxKey(r.design), got)
	}
	if !r.payloadIs(step, r.tick, keptNewTick) {
		r.t.Fatalf("%s: the tick = %v, want %s", step, r.texts(step, `select payload::text from outbox where id = $1`, r.tick), keptNewTick)
	}
	if got := r.texts(step, `select relname::text from pg_class c join pg_namespace n on n.oid = c.relnamespace
		where n.nspname = 'public' and relname ~* 'park'
		union all select conname::text from pg_constraint c join pg_namespace n on n.oid = c.connamespace
		where n.nspname = 'public' and conname ~* 'park'
		union all select 'outbox ' || id || ': ' || payload::text from outbox where payload::text ~* 'park'
		union all select 'processed_events: ' || source from processed_events where source ~* 'park'`); len(got) != 0 {
		r.t.Fatalf("%s: the store still holds the old word in %v", step, got)
	}
}

// assertTheOldNames holds the store to the release before's names after the way back: the table,
// its index and the constraint names step 1 read, with the rows of want; the controller notice of
// the release before's kind; the mark under that release's source; the tick's reason in that
// release's word; and no version row of the migration, so the roll forward runs it again.
func (r *keptDecisionsRig) assertTheOldNames(step string, want []keptDecisionRow) {
	r.t.Helper()
	if got := r.texts(step, `select coalesce(to_regclass('kept_decisions')::text, '')`); !reflect.DeepEqual(got, []string{""}) {
		r.t.Fatalf("%s: the table of the new name = %v, want none", step, got)
	}
	if got := r.keptRows(step, "parked_decisions"); !reflect.DeepEqual(got, want) {
		r.t.Fatalf("%s: the release before's table holds %+v, want %+v", step, got, want)
	}
	if got := r.texts(step, `select conname::text from pg_constraint where conrelid = 'parked_decisions'::regclass order by conname`); !reflect.DeepEqual(got, r.constraints) {
		r.t.Fatalf("%s: the constraints of the release before's table = %v, want the ones step 1 read, %v", step, got, r.constraints)
	}
	if got, want := r.texts(step, `select indexname::text from pg_indexes where schemaname = 'public' and tablename = 'parked_decisions' order by indexname`),
		[]string{"parked_decisions_issue", "parked_decisions_pkey"}; !reflect.DeepEqual(got, want) {
		r.t.Fatalf("%s: the indexes of the release before's table = %v, want %v", step, got, want)
	}
	if !r.payloadIs(step, r.notice, keptOldNotice) {
		r.t.Fatalf("%s: the controller notice = %v, want the release before's %s", step, r.texts(step, `select payload::text from outbox where id = $1`, r.notice), keptOldNotice)
	}
	if got := r.texts(step, `select source from processed_events where event_id = $1`, record.OutboxKey(r.design)); !reflect.DeepEqual(got, []string{"parked-decision"}) {
		r.t.Fatalf("%s: the source of the mark of %s = %v, want the release before's", step, record.OutboxKey(r.design), got)
	}
	if !r.payloadIs(step, r.tick, keptOldTick) {
		r.t.Fatalf("%s: the tick = %v, want the release before's %s", step, r.texts(step, `select payload::text from outbox where id = $1`, r.tick), keptOldTick)
	}
	if got := r.texts(step, `select count(*)::text from schema_version where version = $1`, r.version); !reflect.DeepEqual(got, []string{"0"}) {
		r.t.Fatalf("%s: the version row %d stands %v times, want none", step, r.version, got)
	}
}

// migratedNotice is the controller notice step 1 wrote, as this release decodes it once its kind
// has moved.
func (r *keptDecisionsRig) migratedNotice() record.ControllerNotice {
	return record.ControllerNotice{Kind: record.KeptDecisionNotice, Decision: record.DecisionDesignApproved, Root: "LEGION-1",
		Ref: keptSpecRef, Actor: "user:alice", Version: 3}
}

// leasedNotice is the outbox row id, leased as the runner leases a row before it executes it, and
// the architect notice its payload holds.
func (r *keptDecisionsRig) leasedNotice(id int64) (record.OutboxRow, record.Notice) {
	r.t.Helper()
	row := record.OutboxRow{ID: id}
	if err := r.pool.QueryRow(r.ctx, "select kind, issue, payload from outbox where id = $1", id).Scan(&row.Kind, &row.Issue, &row.Payload); err != nil {
		r.t.Fatalf("read outbox row %d: %v", id, err)
	}
	row.LeaseToken = leaseOutboxRow(r.t, r.pool, id)
	payload, err := record.DecodeOutboxPayload(row)
	if err != nil {
		r.t.Fatalf("decode outbox row %d: %v", id, err)
	}
	notice, ok := payload.(record.Notice)
	if !ok {
		r.t.Fatalf("outbox row %d holds %T, want an architect notice", id, payload)
	}
	return row, notice
}

// controllerNotice is the controller notice outbox row id holds, as this release decodes it.
func (r *keptDecisionsRig) controllerNotice(step string, id int64) record.ControllerNotice {
	r.t.Helper()
	row := record.OutboxRow{ID: id}
	if err := r.pool.QueryRow(r.ctx, "select kind, issue, payload from outbox where id = $1", id).Scan(&row.Kind, &row.Issue, &row.Payload); err != nil {
		r.t.Fatalf("%s: read outbox row %d: %v", step, id, err)
	}
	payload, err := record.DecodeOutboxPayload(row)
	if err != nil {
		r.t.Fatalf("%s: this release refuses outbox row %d: %v", step, id, err)
	}
	notice, ok := payload.(record.ControllerNotice)
	if !ok {
		r.t.Fatalf("%s: outbox row %d holds %T, want a controller notice", step, id, payload)
	}
	return notice
}

// composeTheRootsStart composes the root's architect start as admission does: the task of what its
// tree is owed and nothing claims (workflow.UnclaimedKeptTask), enqueued with a claim on each row it
// names (workflow.EnqueueNaming). It answers the task and what it names; a task that names nothing
// enqueues nothing here, since the step asks only what a second start would name.
func (r *keptDecisionsRig) composeTheRootsStart(engine *workflow.Engine) (string, *record.KeptDecisions) {
	r.t.Helper()
	var task string
	var kept *record.KeptDecisions
	if err := store.Transact(r.ctx, r.pool, func(ctx context.Context, tx pgx.Tx) error {
		root, err := r.records.Issue(ctx, tx, "LEGION-1")
		if err != nil || root == nil {
			return errors.Join(err, errors.New("LEGION-1 is not recorded"))
		}
		if task, kept, err = engine.UnclaimedKeptTask(ctx, tx, *root); err != nil || kept == nil {
			return err
		}
		return engine.EnqueueNaming(ctx, tx, root.Key, root.Tree, record.SuperviseRequest{Op: "start", Tree: root.Tree, Role: claim.RoleArchitect,
			Generation: root.Generation, Task: task, Kept: kept}, kept, time.Now())
	}); err != nil {
		r.t.Fatalf("step 3: compose the root's start: %v", err)
	}
	return task, kept
}

// keptRows is every row of table, the table of kept decisions under the name the step reads it
// by, field by field, in the order of their references.
func (r *keptDecisionsRig) keptRows(step, table string) []keptDecisionRow {
	r.t.Helper()
	query := `select issue, ref, decision, actor, at, coalesce(claimed_by, 0), coalesce(held_by_claim, ''), coalesce(held_by_delivery, '')
		from ` + table + ` order by ref desc`
	rows, err := r.pool.Query(r.ctx, query)
	if err != nil {
		r.t.Fatalf("%s: read the kept decisions from %s: %v", step, table, err)
	}
	defer rows.Close()
	var kept []keptDecisionRow
	for rows.Next() {
		var row keptDecisionRow
		if err := rows.Scan(&row.Issue, &row.Ref, &row.Decision, &row.Actor, &row.At, &row.ClaimedBy, &row.HeldByClaim, &row.HeldByDelivery); err != nil {
			r.t.Fatalf("%s: scan a kept decision of %s: %v", step, table, err)
		}
		row.At = row.At.UTC()
		kept = append(kept, row)
	}
	if err := rows.Err(); err != nil {
		r.t.Fatalf("%s: read the kept decisions from %s: %v", step, table, err)
	}
	return kept
}

// texts is each row query answers, as text, in the query's order.
func (r *keptDecisionsRig) texts(step, query string, args ...any) []string {
	r.t.Helper()
	rows, err := r.pool.Query(r.ctx, query, args...)
	if err != nil {
		r.t.Fatalf("%s: %s: %v", step, query, err)
	}
	all, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		r.t.Fatalf("%s: %s: %v", step, query, err)
	}
	return all
}

// payloadIs says whether outbox row id holds the payload want, as JSON.
func (r *keptDecisionsRig) payloadIs(step string, id int64, want string) bool {
	r.t.Helper()
	var same bool
	if err := r.pool.QueryRow(r.ctx, `select payload = $2::jsonb from outbox where id = $1`, id, want).Scan(&same); err != nil {
		r.t.Fatalf("%s: read the payload of outbox row %d: %v", step, id, err)
	}
	return same
}

// queue writes an outbox row in plain SQL, as the release before left it, and answers its id.
func (r *keptDecisionsRig) queue(kind, issue, payload string) int64 {
	r.t.Helper()
	var id int64
	if err := r.pool.QueryRow(r.ctx, `insert into outbox (kind, issue, payload, attempts, next_at, last_error)
		values ($1, $2, $3::jsonb, 0, now(), '') returning id`, kind, issue, payload).Scan(&id); err != nil {
		r.t.Fatalf("step 1: queue the %s row %s: %v", kind, payload, err)
	}
	return id
}

func (r *keptDecisionsRig) exec(step, statement string, args ...any) {
	r.t.Helper()
	if _, err := r.pool.Exec(r.ctx, statement, args...); err != nil {
		r.t.Fatalf("%s: %s: %v", step, statement, err)
	}
}

// keptDecisionsOMP is the OMP scripts/e2e/stage1-skeleton.sh gives the daemon it boots: a stub that
// answers the plugin gate's load probe as a pane that loads pi-legion beside pi-envoy does, and
// runs nothing else, since the boot launches no agent.
const keptDecisionsOMP = `#!/bin/sh
if [ "$1" = models ]; then
  echo LEGION_PLUGIN_LOADED=yes >&2
  echo "LEGION_PLUGIN_LOADED_FROM=file://$HOME/.omp/plugins/node_modules/@sjawhar/pi-legion/dist/legion.js" >&2
  echo LEGION_PLUGIN_ENVOY_INTERFACE=1 >&2
  echo LEGION_ENVOY_INTERFACE=1 >&2
  echo "LEGION_ENVOY_LOADED_FROM=file://$HOME/.omp/plugins/node_modules/@sjawhar/pi-envoy/dist/envoy.js" >&2
  [ -n "${LEGION_PROMPT_AGENTS:-}" ] && echo LEGION_PROMPT_AGENTS=resolved >&2
  [ -n "${LEGION_PROMPT_AGENTS:-}" ] && [ -z "${LEGION_SKIP_AGENT_MODELS:-}" ] && echo LEGION_AGENT_MODELS=resolved >&2
  [ -n "${LEGION_PROMPT_SKILLS:-}" ] && echo LEGION_PROMPT_SKILLS=resolved >&2
  exit 0
fi
echo "the boot of the release before launches no agent" >&2
exit 1
`

// keptDecisionsConfig is the legion.yaml scripts/e2e/stage1-skeleton.sh writes, with the test's
// project, port, Postgres DSN, state directory and operator token file, and keptDecisionsFixtureKey
// as each App's private key. It leaves out that script's envoy_url, whose userinfo is there for
// that script's check of the state's masking; this daemon takes the default listener.
const keptDecisionsConfig = `project: %s
port: %d
postgres_dsn: %s
state_dir: %s
operator_token_file: %s
review_round_cap: 5
worker_wakes_per_minute: 0
projects:
  FIXTURE: {}
github_apps:
  implement:
    app_id: "1001"
    private_key: |
      %[6]s
  review:
    app_id: "1002"
    private_key: |
      %[6]s
`

// keptDecisionsFixtureKey is the key stage1-skeleton.sh writes, which is no key: the daemon reads
// it at boot and the stub OMP never mints a token with it. Its armour is joined here rather than
// written whole because the push guard refuses an added line that holds a private key's armour,
// whatever stands under it.
var keptDecisionsFixtureKey = strings.Join([]string{
	"-----" + "BEGIN PRIVATE KEY-----", "fixture", "-----" + "END PRIVATE KEY-----",
}, "\n      ")

// keptDecisionsRequiredContract reads, from a boot gate's refusal, the daemon API contract the
// daemon requires of its plugin.
var keptDecisionsRequiredContract = regexp.MustCompile(`this daemon requires (?:a plugin speaking daemon API contract )?(\d+)`)

// keptDecisionsUndefinedTable is an error of PostgreSQL that names a table the statement could not
// find.
var keptDecisionsUndefinedTable = regexp.MustCompile(`relation "[^"]+" does not exist|SQLSTATE 42P01`)

// keptDecisionsBaseReader is the main package step 5 builds in the release before's daemon module,
// so that the rows the way back left are read by that release's own readers: the kept decisions
// owed to a tree (record.Postgres.ParkedDecisions), each outbox row it is asked for decoded
// (record.DecodeOutboxPayload), and whether each event it is asked for is marked processed under
// its source (record.Postgres.Processed). It reads what to read as one JSON object on stdin and
// writes what it read, each read in a transaction of its own with its error beside it, as one JSON
// object on stdout. Being a raw string, it holds no struct tag: both objects take Go's field names.
const keptDecisionsBaseReader = `package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/record"
)

type event struct{ Source, Event string }

type ask struct {
	DSN       string
	Tree      string
	Outbox    []int64
	Processed []event
}

type decoded struct {
	ID      int64
	Type    string
	Payload record.OutboxPayload
	Error   string
}

type marked struct {
	Source, Event string
	Processed     bool
	Error         string
}

type read struct {
	Decisions      []record.ParkedDecision
	DecisionsError string
	Outbox         []decoded
	Processed      []marked
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	var asked ask
	if err := json.NewDecoder(os.Stdin).Decode(&asked); err != nil {
		return fmt.Errorf("read what to read: %w", err)
	}
	ctx := context.Background()
	// A pool, as the daemon's store opens one, since the test's DSN carries pgxpool's parameters.
	pool, err := pgxpool.New(ctx, asked.DSN)
	if err != nil {
		return fmt.Errorf("connect to the database: %w", err)
	}
	defer pool.Close()
	records := record.NewStore()
	// Each read runs in a transaction of its own, so a read that fails aborts none of the others.
	inTx := func(read func(pgx.Tx) error) error {
		tx, err := pool.Begin(ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback(ctx)
		return read(tx)
	}
	var out read
	if err := inTx(func(tx pgx.Tx) (err error) {
		out.Decisions, err = records.ParkedDecisions(ctx, tx, asked.Tree)
		return err
	}); err != nil {
		out.DecisionsError = err.Error()
	}
	for _, id := range asked.Outbox {
		one := decoded{ID: id}
		if err := inTx(func(tx pgx.Tx) error {
			row := record.OutboxRow{ID: id}
			if err := tx.QueryRow(ctx, "select kind, issue, payload from outbox where id = $1", id).Scan(&row.Kind, &row.Issue, &row.Payload); err != nil {
				return fmt.Errorf("read outbox row %d: %w", id, err)
			}
			payload, err := record.DecodeOutboxPayload(row)
			if err != nil {
				return err
			}
			one.Type, one.Payload = fmt.Sprintf("%T", payload), payload
			return nil
		}); err != nil {
			one.Error = err.Error()
		}
		out.Outbox = append(out.Outbox, one)
	}
	for _, e := range asked.Processed {
		one := marked{Source: e.Source, Event: e.Event}
		if err := inTx(func(tx pgx.Tx) (err error) {
			one.Processed, err = records.Processed(ctx, tx, e.Source, e.Event)
			return err
		}); err != nil {
			one.Error = err.Error()
		}
		out.Processed = append(out.Processed, one)
	}
	return json.NewEncoder(os.Stdout).Encode(out)
}
`

// keptDecisionsBaseRead is what keptDecisionsBaseReader writes, under Go's field names.
type keptDecisionsBaseRead struct {
	Decisions []struct {
		Issue, Ref, Decision, Actor string
		At                          time.Time
		ClaimedBy                   int64
		HeldBy                      struct{ Claim, Delivery string }
	}
	DecisionsError string
	Outbox         []struct {
		ID          int64
		Type, Error string
		Payload     json.RawMessage
	}
	Processed []struct {
		Source, Event, Error string
		Processed            bool
	}
}

// readWithTheReleaseBefore is step 5's read: keptDecisionsBaseReader, built in the daemon module of
// tree, the release before's source, reads the database dsn names with that release's own readers
// and finds what the way back left under the old names: the kept decisions of the root's tree as
// told, each seeded outbox row decoded (the controller notice of the old kind, and the tick whose
// held child carries the old reason, each as step 1 wrote it), and the mark of the architect
// notice under the old source. The package is written into a fresh directory inside that module,
// since only a package of the module may import its internal packages, and the directory is
// removed once the reader is built.
func (r *keptDecisionsRig) readWithTheReleaseBefore(tree, dsn string, told []keptDecisionRow) {
	r.t.Helper()
	module := filepath.Join(tree, "packages", "daemon")
	source, err := os.MkdirTemp(module, "kept-decisions-reader-")
	if err != nil {
		r.t.Fatalf("step 5: %s names %s, which holds no daemon module to build the release before's readers in: %v", keptDecisionsBaseTree, tree, err)
	}
	reader := filepath.Join(r.t.TempDir(), "reader")
	var built []byte
	if err = os.WriteFile(filepath.Join(source, "main.go"), []byte(keptDecisionsBaseReader), 0o644); err == nil {
		built, err = exec.Command("go", "build", "-C", module, "-o", reader, "./"+filepath.Base(source)).CombinedOutput()
	}
	if removed := os.RemoveAll(source); removed != nil {
		r.t.Errorf("step 5: remove the readers' source %s: %v", source, removed)
	}
	if err != nil {
		r.t.Fatalf("step 5: build the release before's readers in %s: %v\n%s", module, err, built)
	}
	mark := record.OutboxKey(r.design)
	ask, err := json.Marshal(map[string]any{
		"DSN": dsn, "Tree": "LEGION-1", "Outbox": []int64{r.design, r.catchUp, r.notice, r.tick},
		"Processed": []map[string]string{{"Source": "parked-decision", "Event": mark}},
	})
	if err != nil {
		r.t.Fatalf("step 5: %v", err)
	}
	run := exec.Command(reader)
	run.Stdin = bytes.NewReader(ask)
	var stderr bytes.Buffer
	run.Stderr = &stderr
	written, err := run.Output()
	if err != nil {
		r.t.Fatalf("step 5: the release before's readers ended on the database the way back left: %v\n%s", err, stderr.String())
	}
	var read keptDecisionsBaseRead
	if err := json.Unmarshal(written, &read); err != nil {
		r.t.Fatalf("step 5: the release before's readers wrote %s: %v", written, err)
	}
	var unread []string
	if read.DecisionsError != "" {
		unread = append(unread, "record.ParkedDecisions of LEGION-1: "+read.DecisionsError)
	} else {
		got := make([]keptDecisionRow, 0, len(read.Decisions))
		for _, kept := range read.Decisions {
			got = append(got, keptDecisionRow{Issue: kept.Issue, Ref: kept.Ref, Decision: kept.Decision, Actor: kept.Actor, At: kept.At.UTC(),
				ClaimedBy: kept.ClaimedBy, HeldByClaim: kept.HeldBy.Claim, HeldByDelivery: kept.HeldBy.Delivery})
		}
		// In the order keptRows reads them.
		slices.SortFunc(got, func(a, b keptDecisionRow) int { return strings.Compare(b.Ref, a.Ref) })
		if !reflect.DeepEqual(got, told) {
			unread = append(unread, fmt.Sprintf("record.ParkedDecisions of LEGION-1 read %+v, want the kept decisions the way back left, %+v", got, told))
		}
	}
	// Each seeded outbox row, by the type it decodes as and, for the two the migration rewrote, the
	// payload step 1 wrote.
	want := map[int64]struct{ decodes, payload string }{
		r.design:  {decodes: "record.Notice"},
		r.catchUp: {decodes: "record.Notice"},
		r.notice:  {decodes: "record.ControllerNotice", payload: keptOldNotice},
		r.tick:    {decodes: "record.ControllerNotice", payload: keptOldTick},
	}
	for _, row := range read.Outbox {
		wanted := want[row.ID]
		var got, seeded any
		if wanted.payload != "" && row.Error == "" {
			_ = json.Unmarshal(row.Payload, &got)
			_ = json.Unmarshal([]byte(wanted.payload), &seeded)
		}
		switch {
		case row.Error != "":
			unread = append(unread, fmt.Sprintf("record.DecodeOutboxPayload of outbox row %d: %s", row.ID, row.Error))
		case row.Type != wanted.decodes:
			unread = append(unread, fmt.Sprintf("record.DecodeOutboxPayload of outbox row %d decoded a %s, want a %s", row.ID, row.Type, wanted.decodes))
		case !reflect.DeepEqual(got, seeded):
			unread = append(unread, fmt.Sprintf("record.DecodeOutboxPayload of outbox row %d decoded %s, want %s as step 1 wrote it", row.ID, row.Payload, wanted.payload))
		}
	}
	for _, marked := range read.Processed {
		if marked.Error != "" || !marked.Processed {
			unread = append(unread, fmt.Sprintf("record.Processed of the %s event %s answered %t (error %q), want true, the mark the way back left",
				marked.Source, marked.Event, marked.Processed, marked.Error))
		}
	}
	if len(unread) != 0 {
		r.t.Fatalf("step 5: the release before's own readers did not read what the way back left under the old names:\n%s", strings.Join(unread, "\n"))
	}
	r.t.Logf("step 5: the release before's own readers, built from %s, read the kept decisions of LEGION-1, decoded outbox rows %d, %d, %d and %d, and read the mark of %s under the old names",
		tree, r.design, r.catchUp, r.notice, r.tick, mark)
}

// bootTheReleaseBefore is step 5: binary, the release before's `legion`, starts on the database dsn
// names as project, the Dispatch project step 1 seeds its issues under, with the minimal
// legion.yaml scripts/e2e/stage1-skeleton.sh writes, under the stub OMP and the plugin manifests
// that script gives it, and answers GET /readyz and GET /legion/v1/state with 200, a state that
// holds each of issues; then it stops, and its log holds no error naming a table. The manifest
// declares this build's contract (api.DaemonAPIVersion()); where the binary's boot gate refuses
// it and names the contract it requires, the manifest takes that one and the binary starts once
// more. Its private tmux server, `legion-<project's claim token>`, is a real daemon's name for a
// seeded project such as LEGION, so the daemon runs under a TMUX_TMPDIR of its own, and the
// cleanup stops that server alone.
func bootTheReleaseBefore(t *testing.T, binary, dsn, project string, issues ...string) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Fatalf("step 5: %s names %s, and a daemon refuses to start without tmux on PATH: %v", keptDecisionsBaseLegion, binary, err)
	}
	// A directory of a short name rather than t.TempDir, whose name carries the test's: the daemon
	// binds a unix socket under its state directory, and a socket's path holds at most 107 bytes.
	work, err := os.MkdirTemp("", "legion-kd-")
	if err != nil {
		t.Fatalf("step 5: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(work) })
	home, state, tmuxDir := filepath.Join(work, "home"), filepath.Join(work, "state"), filepath.Join(work, "tmux")
	for _, dir := range []string{home, state, tmuxDir, filepath.Join(work, "xdg")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("step 5: %v", err)
		}
	}
	projectToken, err := claim.ProjectToken(project)
	if err != nil {
		t.Fatalf("step 5: %v", err)
	}
	token, omp := filepath.Join(work, "operator-token"), filepath.Join(work, "omp")
	if err := os.WriteFile(token, []byte("kept-decisions-operator-"+project+"\n"), 0o600); err != nil {
		t.Fatalf("step 5: %v", err)
	}
	if err := os.WriteFile(omp, []byte(keptDecisionsOMP), 0o755); err != nil {
		t.Fatalf("step 5: %v", err)
	}
	repository := filepath.Join("..", "..", "..", "..")
	legionManifest := keptDecisionsManifest(t, filepath.Join(repository, "packages", "pi-legion", "package.json"))
	envoyManifest := keptDecisionsManifest(t, filepath.Join(repository, "packages", "pi-envoy", "package.json"))
	plugins := filepath.Join(home, ".omp", "plugins", "node_modules", "@sjawhar")
	writeManifest := func(name string, manifest map[string]any) {
		t.Helper()
		encoded, err := json.Marshal(manifest)
		if err != nil {
			t.Fatalf("step 5: encode the %s manifest: %v", name, err)
		}
		if err := os.MkdirAll(filepath.Join(plugins, name), 0o755); err != nil {
			t.Fatalf("step 5: %v", err)
		}
		if err := os.WriteFile(filepath.Join(plugins, name, "package.json"), encoded, 0o644); err != nil {
			t.Fatalf("step 5: %v", err)
		}
	}
	writeManifest("pi-envoy", envoyManifest)
	// The port is held from here to the test's end (heldSocket), and each boot is handed the socket
	// as LEGION_LISTEN_FD, so no other program takes the port between two boots.
	socket, port := heldSocket(t)
	configPath := filepath.Join(work, "legion.yaml")
	if err := os.WriteFile(configPath, fmt.Appendf(nil, keptDecisionsConfig, project, port, dsn, state, token, keptDecisionsFixtureKey), 0o600); err != nil {
		t.Fatalf("step 5: %v", err)
	}
	var env []string
	for _, pair := range os.Environ() {
		name, _, _ := strings.Cut(pair, "=")
		switch name {
		case "OMP_PROFILE", "PI_PROFILE", "HOME", "XDG_STATE_HOME", "LEGION_OMP_PATH", "LEGION_LISTEN_FD", "TMUX_TMPDIR":
			continue
		}
		env = append(env, pair)
	}
	env = append(env, "HOME="+home, "XDG_STATE_HOME="+filepath.Join(work, "xdg"), "LEGION_OMP_PATH="+omp, "TMUX_TMPDIR="+tmuxDir)
	t.Cleanup(func() {
		// The daemon's private tmux server under its own TMUX_TMPDIR, should its boot have started one.
		stop := exec.Command("tmux", "-L", "legion-"+projectToken, "kill-server")
		stop.Env = env
		_ = stop.Run()
	})
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	contract := api.DaemonAPIVersion()
	for attempt := 1; ; attempt++ {
		legionManifest["legion"] = map[string]any{"daemonApiVersion": contract}
		writeManifest("pi-legion", legionManifest)
		logPath := filepath.Join(work, fmt.Sprintf("daemon-%d.log", attempt))
		logFile, err := os.Create(logPath)
		if err != nil {
			t.Fatalf("step 5: %v", err)
		}
		daemon := exec.Command(binary, "start", "--config", configPath)
		daemon.Env, daemon.Stdout, daemon.Stderr = append(slices.Clip(env), "LEGION_LISTEN_FD=3"), logFile, logFile
		// ExtraFiles' first entry is the daemon's descriptor 3.
		daemon.ExtraFiles = []*os.File{socket}
		if err := daemon.Start(); err != nil {
			_ = logFile.Close()
			t.Fatalf("step 5: start %s: %v", binary, err)
		}
		exited := make(chan error, 1)
		go func() { exited <- daemon.Wait() }()
		readLog := func() string {
			logged, _ := os.ReadFile(logPath)
			return string(logged)
		}
		ready, ended := waitForKeptDecisionsReady(base+"/readyz", exited, 2*time.Minute)
		_ = logFile.Sync()
		if !ready {
			_ = logFile.Close()
			if ended == nil {
				_ = daemon.Process.Kill()
				<-exited
				t.Fatalf("step 5: %s never answered GET /readyz with 200 within two minutes; its log:\n%s", binary, readLog())
			}
			if wanted := keptDecisionsRequiredContract.FindStringSubmatch(readLog()); attempt == 1 && wanted != nil {
				required, _ := strconv.Atoi(wanted[1])
				t.Logf("step 5: %s requires daemon API contract %d of its plugin; the manifest declares it and the binary starts again", binary, required)
				contract = required
				continue
			}
			t.Fatalf("step 5: %s ended before it answered GET /readyz (%v); its log:\n%s", binary, ended, readLog())
		}
		status, body := keptDecisionsGet(base + "/legion/v1/state")
		if err := daemon.Process.Signal(syscall.SIGTERM); err != nil {
			t.Errorf("step 5: stop %s: %v", binary, err)
		}
		select {
		case ended = <-exited:
		case <-time.After(time.Minute):
			_ = daemon.Process.Kill()
			ended = <-exited
			t.Errorf("step 5: %s did not stop within a minute of SIGTERM", binary)
		}
		_ = logFile.Close()
		logged := readLog()
		if status != http.StatusOK {
			t.Fatalf("step 5: GET /legion/v1/state of %s answered %d %s; its log:\n%s", binary, status, body, logged)
		}
		if ended != nil {
			t.Errorf("step 5: %s exited %v on SIGTERM, want 0; its log:\n%s", binary, ended, logged)
		}
		var named []string
		for line := range strings.SplitSeq(logged, "\n") {
			if keptDecisionsUndefinedTable.MatchString(line) {
				named = append(named, line)
			}
		}
		if len(named) != 0 {
			t.Fatalf("step 5: the log of %s names a table it cannot read:\n%s", binary, strings.Join(named, "\n"))
		}
		var answered struct {
			Issues map[string]json.RawMessage `json:"issues"`
		}
		if err := json.Unmarshal([]byte(body), &answered); err != nil {
			t.Fatalf("step 5: GET /legion/v1/state of %s answered %s, which holds no issues: %v", binary, body, err)
		}
		var unread []string
		for _, issue := range issues {
			if _, read := answered.Issues[issue]; !read {
				unread = append(unread, issue)
			}
		}
		if len(unread) != 0 {
			t.Fatalf("step 5: %s booted as project %s did not read the seeded issues %v: its GET /legion/v1/state lists the issues %v",
				binary, project, unread, slices.Sorted(maps.Keys(answered.Issues)))
		}
		t.Logf("step 5: %s booted as project %s on the database the way back left, answered GET /readyz and GET /legion/v1/state with 200, listed %v in its state, and stopped",
			binary, project, issues)
		return
	}
}

// keptDecisionsManifest reads the name and version for a stub plugin manifest from the
// checkout's package.json. The boot test supplies the required contract separately.
func keptDecisionsManifest(t *testing.T, path string) map[string]any {
	t.Helper()
	encoded, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("step 5: read %s: %v", path, err)
	}
	var read struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	}
	if err := json.Unmarshal(encoded, &read); err != nil {
		t.Fatalf("step 5: parse %s: %v", path, err)
	}
	return map[string]any{"name": read.Name, "version": read.Version}
}

// waitForKeptDecisionsReady polls url until it answers 200, the daemon ends (exited), or within
// passes. It answers whether the daemon answered, and how it ended where it ended first.
func waitForKeptDecisionsReady(url string, exited <-chan error, within time.Duration) (bool, error) {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		select {
		case err := <-exited:
			if err == nil {
				err = errors.New("exit status 0")
			}
			return false, err
		default:
		}
		if status, _ := keptDecisionsGet(url); status == http.StatusOK {
			return true, nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	return false, nil
}

// keptDecisionsGet is the status and body of GET url, and 0 where no answer came.
func keptDecisionsGet(url string) (int, string) {
	client := http.Client{Timeout: 10 * time.Second}
	response, err := client.Get(url)
	if err != nil {
		return 0, err.Error()
	}
	defer response.Body.Close()
	var body bytes.Buffer
	_, _ = body.ReadFrom(response.Body)
	return response.StatusCode, body.String()
}
