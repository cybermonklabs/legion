package embedqueue

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/embed"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/retry"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/logtest"
)

// countingTrigger is one trigger the coverage counts are kept by, as pg_trigger holds it: the
// table it is on, its name, whether it fires for each row or once for each statement, the events
// it fires after, in the order insert, update, delete, truncate, and the columns an update must
// name to fire it, sorted, nil for an update of any column.
type countingTrigger struct {
	table, name string
	row         bool
	events      []string
	columns     []string
}

// String is the trigger as a failure names it.
func (trigger countingTrigger) String() string {
	level := "statement"
	if trigger.row {
		level = "row"
	}
	events := strings.Join(trigger.events, " or ")
	if len(trigger.columns) > 0 {
		events += " (update of " + strings.Join(trigger.columns, ", ") + ")"
	}
	return fmt.Sprintf("%s on %s: after %s, for each %s", trigger.name, trigger.table, events, level)
}

// countingTriggers is every trigger the coverage counts are kept by (migration
// 20261124011834_search_coverage_counts), each named search_coverage_<table>_<what>: the name is
// the contract this test holds the migration to, so a trigger the migration names otherwise is
// missing here and one it adds that this list does not hold is an extra. On embeddings, three
// statement triggers, after insert, after update and after delete, since Postgres takes
// transition tables only on a trigger of one event; on components, artifact_versions,
// check_results and handoffs two each, after insert and after delete. The row triggers of issues,
// artifacts and artifact_versions refresh the state row of the issue a write concerns, each fired
// by an update of the columns that issue's class is read from: an issue's title and created_at,
// an artifact's issue_key and is_primary, and a version's markdown, the issue's spec where the
// artifact is its primary document. Each of those seven tables has a statement trigger after
// truncate, and search_coverage_issues has a row trigger of its own that writes the old class
// minus and the new class plus as delta rows. route_roster is read directly and has none.
var countingTriggers = []countingTrigger{
	{"embeddings", "search_coverage_embeddings_insert", false, []string{"insert"}, nil},
	{"embeddings", "search_coverage_embeddings_update", false, []string{"update"}, nil},
	{"embeddings", "search_coverage_embeddings_delete", false, []string{"delete"}, nil},
	{"embeddings", "search_coverage_embeddings_truncate", false, []string{"truncate"}, nil},
	{"components", "search_coverage_components_insert", false, []string{"insert"}, nil},
	{"components", "search_coverage_components_delete", false, []string{"delete"}, nil},
	{"components", "search_coverage_components_truncate", false, []string{"truncate"}, nil},
	{"artifact_versions", "search_coverage_artifact_versions_insert", false, []string{"insert"}, nil},
	{"artifact_versions", "search_coverage_artifact_versions_delete", false, []string{"delete"}, nil},
	{"artifact_versions", "search_coverage_artifact_versions_rows", true, []string{"insert", "update", "delete"}, []string{"markdown"}},
	{"artifact_versions", "search_coverage_artifact_versions_truncate", false, []string{"truncate"}, nil},
	{"check_results", "search_coverage_check_results_insert", false, []string{"insert"}, nil},
	{"check_results", "search_coverage_check_results_delete", false, []string{"delete"}, nil},
	{"check_results", "search_coverage_check_results_truncate", false, []string{"truncate"}, nil},
	{"handoffs", "search_coverage_handoffs_insert", false, []string{"insert"}, nil},
	{"handoffs", "search_coverage_handoffs_delete", false, []string{"delete"}, nil},
	{"handoffs", "search_coverage_handoffs_truncate", false, []string{"truncate"}, nil},
	{"issues", "search_coverage_issues_rows", true, []string{"insert", "update", "delete"}, []string{"created_at", "title"}},
	{"issues", "search_coverage_issues_truncate", false, []string{"truncate"}, nil},
	{"artifacts", "search_coverage_artifacts_rows", true, []string{"insert", "update", "delete"}, []string{"is_primary", "issue_key"}},
	{"artifacts", "search_coverage_artifacts_truncate", false, []string{"truncate"}, nil},
	{"search_coverage_issues", "search_coverage_issue_classes", true, []string{"insert", "update", "delete"}, nil},
}

// The bits of pg_trigger.tgtype (Postgres's catalog/pg_trigger.h, TRIGGER_TYPE_*).
const (
	triggerTypeRow      = 1 << 0
	triggerTypeBefore   = 1 << 1
	triggerTypeInsert   = 1 << 2
	triggerTypeDelete   = 1 << 3
	triggerTypeUpdate   = 1 << 4
	triggerTypeTruncate = 1 << 5
	triggerTypeInstead  = 1 << 6
)

// installedCountingTrigger is one trigger whose name begins search_coverage_ as pg_trigger holds
// it: what countingTrigger names, its timing, and tgenabled, 'O' for a trigger that fires in the
// ordinary mode of session_replication_role.
type installedCountingTrigger struct {
	countingTrigger
	timing  string
	enabled string
}

// installedCountingTriggers reads every trigger of the database whose name begins
// search_coverage_, by table and name.
func installedCountingTriggers(t *testing.T, database *store.Store) map[string]installedCountingTrigger {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select c.relname::text, t.tgname::text, t.tgtype::int, t.tgenabled::text,
		       array(select a.attname::text from pg_attribute a
		              where a.attrelid = t.tgrelid and a.attnum = any(t.tgattr::int2[])
		              order by a.attname)
		  from pg_trigger t join pg_class c on c.oid = t.tgrelid
		 where not t.tgisinternal and t.tgname like 'search\_coverage\_%'`)
	if err != nil {
		t.Fatalf("read the counting triggers: %v", err)
	}
	installed, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (installedCountingTrigger, error) {
		var trigger installedCountingTrigger
		var tgtype int
		if err := row.Scan(&trigger.table, &trigger.name, &tgtype, &trigger.enabled, &trigger.columns); err != nil {
			return trigger, err
		}
		trigger.row = tgtype&triggerTypeRow != 0
		switch {
		case tgtype&triggerTypeInstead != 0:
			trigger.timing = "instead of"
		case tgtype&triggerTypeBefore != 0:
			trigger.timing = "before"
		default:
			trigger.timing = "after"
		}
		for _, event := range []struct {
			bit  int
			name string
		}{{triggerTypeInsert, "insert"}, {triggerTypeUpdate, "update"}, {triggerTypeDelete, "delete"}, {triggerTypeTruncate, "truncate"}} {
			if tgtype&event.bit != 0 {
				trigger.events = append(trigger.events, event.name)
			}
		}
		if len(trigger.columns) == 0 {
			trigger.columns = nil
		}
		return trigger, nil
	})
	if err != nil {
		t.Fatalf("scan the counting triggers: %v", err)
	}
	byName := map[string]installedCountingTrigger{}
	for _, trigger := range installed {
		byName[trigger.table+"."+trigger.name] = trigger
	}
	return byName
}

// countingTriggersDiffer names every way the installed triggers differ from countingTriggers:
// each listed trigger missing, each one whose level, timing, events or update columns differ,
// each one not enabled in the ordinary mode, and each trigger named search_coverage_ that the
// list does not hold. It is empty when they agree.
func countingTriggersDiffer(installed map[string]installedCountingTrigger) []string {
	var differences []string
	listed := map[string]bool{}
	for _, want := range countingTriggers {
		key := want.table + "." + want.name
		listed[key] = true
		got, found := installed[key]
		switch {
		case !found:
			differences = append(differences, "missing: "+want.String())
		case got.timing != "after" || got.row != want.row || !slices.Equal(got.events, want.events) || !slices.Equal(got.columns, want.columns):
			differences = append(differences, fmt.Sprintf("%s is %s (%s), want %s", key, got.countingTrigger, got.timing, want))
		case got.enabled != "O":
			differences = append(differences, fmt.Sprintf("%s has tgenabled %q, want 'O': it must fire in the ordinary mode", key, got.enabled))
		}
	}
	for _, key := range slices.Sorted(maps.Keys(installed)) {
		if !listed[key] {
			differences = append(differences, "not in countingTriggers: "+installed[key].countingTrigger.String())
		}
	}
	return differences
}

// TestEveryCountedTableCarriesItsCountingTrigger holds R7h: every table the coverage counts are
// read from carries the triggers that keep them (countingTriggers), each an ordinary trigger
// that fires after its events and is enabled, on a fresh database and again after
// withTableTriggersOff has turned each such table's triggers off and back on, as the tests that
// seed past the triggers do. The triggers are ordinary, not `enable always`, since the migration
// census admits no ENABLE ALWAYS TRIGGER (internal/pgmigrate/exempt.go): a write under
// session_replication_role = replica fires none of them, and the recount is what follows it.
func TestEveryCountedTableCarriesItsCountingTrigger(t *testing.T) {
	database := storetest.Open(t)
	if differences := countingTriggersDiffer(installedCountingTriggers(t, database)); len(differences) > 0 {
		t.Fatalf("on a fresh database the counting triggers differ from the %d listed:\n%s",
			len(countingTriggers), strings.Join(differences, "\n"))
	}

	var tables []string
	for _, trigger := range countingTriggers {
		if !slices.Contains(tables, trigger.table) {
			tables = append(tables, trigger.table)
		}
	}
	for _, table := range tables {
		withTableTriggersOff(t, database, table, func(querier) {})
	}
	if differences := countingTriggersDiffer(installedCountingTriggers(t, database)); len(differences) > 0 {
		t.Fatalf("after withTableTriggersOff on %s the counting triggers differ from the %d listed:\n%s",
			strings.Join(tables, ", "), len(countingTriggers), strings.Join(differences, "\n"))
	}
}

// TestTwoWritesToOneIssuesInputsLeaveItsStateRowCurrent holds R7's "after every write that fires
// the triggers" where two transactions change two inputs of one issue at once: A edits its title
// and B rewrites its spec's latest version in place, B's statement starting while A, which has
// refreshed the issue, is still open. The issue's vector is of the text both writes make
// together, so it is current only once the issue's state row is computed from both. B's refresh
// waits for the issue's state row until A commits and only then computes the row, so it reads A's
// title; a refresh that computed before it waited would read the title A had not committed yet
// and write, after A's commit, a class with no vector of the issue's text.
func TestTwoWritesToOneIssuesInputsLeaveItsStateRowCurrent(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	const key, title, spec = "TWO-1", "The title A writes", "## Summary\n\nThe spec B writes."
	seedIssue(t, database, "TWO", key, " ")
	document := seedSpec(t, database.Pool, "TWO", key, "\t")
	var version string
	if err := database.Pool.QueryRow(ctx, `select id::text from artifact_versions where artifact_id = $1`, document).Scan(&version); err != nil {
		t.Fatalf("read the spec's version: %v", err)
	}
	updateRow(t, database.Pool, "issue", key,
		`embedding = $3::vector, model = $4, embedded_at = now(),
		 embedded_hash = encode(sha256(convert_to(issue_embedding_text($5, $6), 'UTF8')), 'hex')`,
		embed.Literal(oneVector()), configuredIdentity, title, spec)
	fillCounts(t, database)
	requireMainsAnswer(t, database, "before the writes")

	a, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin A: %v", err)
	}
	defer func() { _ = a.Rollback(ctx) }()
	b, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin B: %v", err)
	}
	defer func() { _ = b.Rollback(ctx) }()
	var bPID uint32
	if err := b.QueryRow(ctx, `select pg_backend_pid()`).Scan(&bPID); err != nil {
		t.Fatalf("read B's backend: %v", err)
	}
	mustExec(t, a, `update issues set title = $2 where key = $1`, key, title)
	rewritten := make(chan error, 1)
	go func() {
		_, err := b.Exec(ctx, `update artifact_versions set markdown = $2 where id = $1`, version, spec)
		rewritten <- err
	}()
	if err, early := storetest.WaitForQueuedBehind(t, a, bPID, rewritten); early {
		t.Fatalf("B's rewrite of the spec ended (%v) before A committed: it never waited on A", err)
	}
	if err := a.Commit(ctx); err != nil {
		t.Fatalf("commit A: %v", err)
	}
	select {
	case err := <-rewritten:
		if err != nil {
			t.Fatalf("B's rewrite of the spec: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("B's rewrite of the spec did not end within 10s of A's commit")
	}
	if err := b.Commit(ctx); err != nil {
		t.Fatalf("commit B: %v", err)
	}
	requireMainsAnswer(t, database, "after A and B committed")
}

// TestAClaimOfAnIssuesRowRefreshesNothing holds that a claim is no input of an issue's class: an
// update of an issue's embeddings row refreshes the issue's state row only where it changed the
// row's embedding is null, embedded_hash, model, dead or pending_since. The issue's row holds a
// vector of an older title, so the queue claims it. Each of the queue's two claims, scanPending's
// and claimRenewal's, writes the row (its xmin moves) and changes next_attempt_at alone, and each
// leaves the issue's state row as it was, its xmin unchanged, and writes no delta row. An update
// of embedded_hash alone, to the row's content_hash, refreshes it: the state row is written again
// and the issue moves from lacking to current, one delta row minus and one plus. After each write
// the counts equal main's statement's.
func TestAClaimOfAnIssuesRowRefreshesNothing(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	const key = "CLM-1"
	seedIssue(t, database, "CLM", key, "The title the vector is of")
	embedRowAs(t, database.Pool, "issue", key, configuredIdentity)
	mustExec(t, database.Pool, `update issues set title = 'The title the queue claims' where key = $1`, key)
	fillCounts(t, database)
	requireMainsAnswer(t, database, "before the claims")

	// rows reads the xmin of the issue's embeddings row and of its state row, and how many delta
	// rows there are.
	rows := func() (embeddingXmin, stateXmin string, deltas int64) {
		t.Helper()
		if err := database.Pool.QueryRow(ctx, `
			select e.xmin::text, s.xmin::text, (select count(*) from search_coverage_deltas)
			from embeddings e, search_coverage_issues s
			where e.kind = 'issue' and e.id = $1 and s.key = $1`, key).Scan(&embeddingXmin, &stateXmin, &deltas); err != nil {
			t.Fatalf("read the issue's rows: %v", err)
		}
		return embeddingXmin, stateXmin, deltas
	}
	embeddingXmin, stateXmin, deltas := rows()
	requireUnrefreshed := func(claim string) {
		t.Helper()
		claimedXmin, gotStateXmin, gotDeltas := rows()
		if claimedXmin == embeddingXmin {
			t.Fatalf("%s did not write the issue's embeddings row (xmin %s before and after)", claim, claimedXmin)
		}
		embeddingXmin = claimedXmin
		if gotStateXmin != stateXmin || gotDeltas != deltas {
			t.Fatalf("after %s the issue's state row has xmin %s and there are %d delta rows, want xmin %s and %d: the claim refreshed the issue",
				claim, gotStateXmin, gotDeltas, stateXmin, deltas)
		}
		requireMainsAnswer(t, database, "after "+claim)
	}

	deps := Deps{Store: database, Embedder: &fakeEmbedder{}}
	claimed, settled, err := scanPending(ctx, deps)
	if err != nil || len(claimed) != 1 || claimed[0].id != key || settled != 0 {
		t.Fatalf("scanPending = %v, %d settled, %v, want %s claimed", claimed, settled, err, key)
	}
	requireUnrefreshed("scanPending's claim")
	renewal := newClaimRenewal(deps, claimed)
	renewal.last = time.Now().Add(-renewClaimInterval - time.Second)
	renewal.renew(ctx)
	requireUnrefreshed("claimRenewal's renewal")

	updateRow(t, database.Pool, "issue", key, `embedded_hash = content_hash`)
	if _, gotStateXmin, gotDeltas := rows(); gotStateXmin == stateXmin || gotDeltas != deltas+2 {
		t.Fatalf("after the update of embedded_hash the issue's state row has xmin %s and there are %d delta rows, want a new xmin and %d: the update did not refresh the issue",
			gotStateXmin, gotDeltas, deltas+2)
	}
	requireMainsAnswer(t, database, "after the update of embedded_hash")
}

// countedTables are the seven tables the counting triggers are on (countingTriggers, but for the
// issues' state rows, which the triggers write themselves).
var countedTables = []string{"embeddings", "issues", "artifacts", "artifact_versions", "components", "check_results", "handoffs"}

// writeTally is what the session it runs in has written and not yet sent to the cumulative
// statistics, as pg_stat_xact_user_tables counts it: the rows inserted, updated or deleted in the
// counted tables ($1, countedTables), triggers' writes and cascades included, and the delta rows
// inserted. Since Postgres 15 those counts hold, beside the current transaction's, an earlier
// transaction's of the same session until the session flushes them, which it does only between
// transactions; so what one transaction wrote is the tally at its end less the tally at its start.
const writeTally = `
	select coalesce(sum(n_tup_ins + n_tup_upd + n_tup_del) filter (where relname = any($1::text[])), 0)::bigint,
	       coalesce(sum(n_tup_ins) filter (where relname = 'search_coverage_deltas'), 0)::bigint
	from pg_stat_xact_user_tables where schemaname = 'public'`

// requireDeltaRowsBounded fails the test unless write inserted at most twice as many delta rows as
// it changed rows of the counted tables: each write statement inserts one delta row for each class
// it moved, and a write that passed twice its changed rows would build delta rows up faster than
// the bound the route reads (coverageDeltaBound) was sized for.
func requireDeltaRowsBounded(t *testing.T, write string, deltas, changed int64) {
	t.Helper()
	if deltas > 2*changed {
		t.Errorf("%s inserted %d delta rows for %d rows it changed in the counted tables, more than twice as many", write, deltas, changed)
	}
}

// requireWriteKept runs write in a transaction of its own and commits it, then compares the
// report's three sides in one snapshot (requireMainsAnswer) right after it, with no fold and no
// recount between. Inside the write's transaction it reads what the write did (writeTally, at the
// transaction's start and again before its commit) and holds its delta rows to its changed rows
// (requireDeltaRowsBounded). write answers the rows it changed that pg_stat_xact_user_tables does
// not count: a truncate's, the rows the table held. It answers the delta rows the write inserted.
func requireWriteKept(t *testing.T, database *store.Store, write string, run func(tx pgx.Tx) (uncounted int64)) int64 {
	t.Helper()
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("%s: begin its transaction: %v", write, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tally := func() (changed, deltas int64) {
		t.Helper()
		if err := tx.QueryRow(ctx, writeTally, countedTables).Scan(&changed, &deltas); err != nil {
			t.Fatalf("%s: read what it wrote: %v", write, err)
		}
		return changed, deltas
	}
	changedBefore, deltasBefore := tally()
	uncounted := run(tx)
	changed, deltas := tally()
	changed, deltas = changed-changedBefore, deltas-deltasBefore
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("%s: commit: %v", write, err)
	}
	requireDeltaRowsBounded(t, write, deltas, changed+uncounted)
	requireMainsAnswer(t, database, "after "+write)
	return deltas
}

// deltaRows is how many delta rows search_coverage_deltas holds.
func deltaRows(t *testing.T, database *store.Store) int64 {
	t.Helper()
	var rows int64
	if err := database.Pool.QueryRow(context.Background(), `select count(*) from search_coverage_deltas`).Scan(&rows); err != nil {
		t.Fatalf("count the delta rows: %v", err)
	}
	return rows
}

// requirePoolWriteKept is requireWriteKept for a write of this package that sends its one
// statement on the pool itself (scanPending, claimRenewal.renew, commitEmbeddings, retryRows), so
// that the test holds no transaction of it: each is one update of rows of embeddings, which writes
// no other counted table, so the rows it changed are the rows it answers (run's answer), and the
// delta rows it inserted are how many more search_coverage_deltas holds after it, since nothing in
// the test folds or recounts. It answers those delta rows.
func requirePoolWriteKept(t *testing.T, database *store.Store, write string, run func() (changed int64)) int64 {
	t.Helper()
	before := deltaRows(t, database)
	changed := run()
	deltas := deltaRows(t, database) - before
	requireDeltaRowsBounded(t, write, deltas, changed)
	requireMainsAnswer(t, database, "after "+write)
	return deltas
}

// issueStateXmins is the xmin of every issue's state row (search_coverage_issues), by key: a write
// that leaves an issue's xmin as it was did not write its state row.
func issueStateXmins(t *testing.T, database *store.Store) map[string]string {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `select key, xmin::text from search_coverage_issues`)
	if err != nil {
		t.Fatalf("read the issues' state rows: %v", err)
	}
	xmins, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) ([2]string, error) {
		var pair [2]string
		return pair, row.Scan(&pair[0], &pair[1])
	})
	if err != nil {
		t.Fatalf("read the issues' state rows: %v", err)
	}
	byKey := make(map[string]string, len(xmins))
	for _, pair := range xmins {
		byKey[pair[0]] = pair[1]
	}
	return byKey
}

// requireClaimRefreshedNothing fails the test unless claim claimed an issue's row and left the
// state row of every issue whose row it claimed as before (its xmin unchanged): a claim moves
// next_attempt_at alone, which moves no count and refreshes no issue.
func requireClaimRefreshedNothing(t *testing.T, database *store.Store, claim string, claimed []pendingRow, before map[string]string) {
	t.Helper()
	after := issueStateXmins(t, database)
	issues := 0
	for _, row := range claimed {
		if row.kind != issueKind {
			continue
		}
		issues++
		if after[row.id] != before[row.id] {
			t.Errorf("%s wrote the state row of %s (xmin %s, was %s): a claim refreshed the issue", claim, row.id, after[row.id], before[row.id])
		}
	}
	if issues == 0 {
		t.Errorf("%s claimed no issue's row, so it shows nothing of the issues' state rows: %v", claim, claimed)
	}
}

// documentOf is a document's markdown made of blocks, each (id, type, text), one blank line apart,
// and the blocks with their places in it, as the version write's parser reports them.
func documentOf(blocks ...[3]string) (string, []model.ArtifactBlock) {
	var markdown strings.Builder
	parsed := make([]model.ArtifactBlock, 0, len(blocks))
	for _, block := range blocks {
		if markdown.Len() > 0 {
			markdown.WriteString("\n\n")
		}
		from := markdown.Len()
		markdown.WriteString(block[2])
		parsed = append(parsed, model.ArtifactBlock{ID: block[0], Type: block[1], From: from, To: markdown.Len()})
	}
	return markdown.String(), parsed
}

// TestEveryWriteKeepsTheCounts holds R7c: right after each write of R7's writers (plan, the four
// lists, "R7's tables and writers"), each firing the triggers of the table it writes, with no fold
// and no recount between, the report's three sides answer the same members, kinds and counts in one
// snapshot for the configured identity, for none and for another (requireMainsAnswer). The counts
// are filled once after a seed of every state an item can be in (seedEveryCoverageState). Each write
// inserts at most twice as many delta rows as the rows it changed in the counted tables
// (requireDeltaRowsBounded). The queue's two claims, scanPending's and claimRenewal's, leave the
// state row of every issue whose row they claimed as it was, and a claim that settles nothing, the
// renewal and the project_key fill insert no delta row.
//
// A write of this package is made by its own function or statement: the claims, the embed, the
// failure and the death by scanPending, claimRenewal.renew, commitEmbeddings and retryRows, and the
// coverage pass's steps by their statements (insertMissingIssues, repairStaleIssues,
// dropForeignVectors, readmitForeignDead, settleDeadNothingToEmbed); a document's windows by
// refs.WriteDocumentWindows, the one writer of them (writeWindows). A write of a package that
// imports this one is the statement it sends: the version removal of api/artifact_removal.go, the
// re-import of architecture/importer.go, the vector writes of cmd/dispatch-seed, the project_key
// fill of store and the whole-text delete of docs/block_index.go.
func TestEveryWriteKeepsTheCounts(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	q := database.Pool
	states := seedEveryCoverageState(t, database)
	const project = "EVRY"
	for _, issue := range [][2]string{
		{"EVRY-2", "An issue a primary document moves to"},
		{"EVRY-3", "An issue whose delete cascades to its result and its handoff"},
		{"EVRY-4", "An issue whose result its document's delete cascades to"},
		{"EVRY-BLANK", " "},
	} {
		seedIssue(t, database, project, issue[0], issue[1])
	}
	// A row that died at a text with nothing to embed, which settleDeadNothingToEmbed settles.
	updateRow(t, q, "issue", "EVRY-BLANK", `dead = true, confirmed_failures = 10, attempt_count = 12, dead_model = $3`, configuredIdentity)
	projectDocument := func(slug string) string {
		t.Helper()
		var id string
		if err := q.QueryRow(ctx, `
			insert into artifacts (project_key, slug, name, kind, created_by, ref_key)
			values ($1, $2, $2 || '.md', 'doc', '{"kind":"user","id":"alice"}', $1 || '/' || $2)
			returning id::text`, project, slug).Scan(&id); err != nil {
			t.Fatalf("seed the project document %s: %v", slug, err)
		}
		return id
	}
	notes, checked := projectDocument("notes"), projectDocument("checked")
	var other string
	if err := q.QueryRow(ctx, `
		insert into artifacts (issue_key, project_key, slug, name, kind, created_by)
		values ('EVRY-2', $1, 'other', 'other.md', 'doc', '{"kind":"user","id":"alice"}')
		returning id::text`, project).Scan(&other); err != nil {
		t.Fatalf("seed EVRY-2's other document: %v", err)
	}
	insertResult := func(q querier, issue, artifact string) {
		t.Helper()
		mustExec(t, q, `insert into check_results (issue_key, check_issue_key, check_artifact_id, check_block_id,
				check_version, check_sha256, role, commit_sha, verdict, actor)
			values ($1, $1, $2::uuid, 'k1', 1, 'a-check-sha', 'tester', repeat('a', 40), 'pass', '{"kind":"user","id":"alice"}'::jsonb)`,
			issue, artifact)
	}
	insertResult(q, "EVRY-4", checked)
	seedComponentsArchitecture(t, q, project, 906)
	insertComponent(t, q, project, "old-a", 906)
	insertComponent(t, q, project, "old-b", 906)
	// A stale issue row, which repairStaleIssues points at its issue's text.
	withTableTriggersOff(t, database, "issues", func(q querier) {
		mustExec(t, q, `update issues set title = 'A title restored with its trigger off' where key = $1`, states.titleRestored)
	})
	// A whole-text row, which the block index retires once its owner's window is current.
	mustExec(t, q, `select embeddings_enqueue('comment', 'every-write-comment', 'A whole-text row the block index retires')`)
	// Issue rows with no project, which the project_key fill labels again.
	mustExec(t, q, `update embeddings set project_key = null where kind = 'issue' and id like 'ORCL-%'`)
	fillCounts(t, database)
	requireMainsAnswer(t, database, "after the fill")

	// issues, and embeddings through embeddings_enqueue.
	requireWriteKept(t, database, "an issue's insert, which its trigger enqueues through embeddings_enqueue", func(tx pgx.Tx) int64 {
		mustExec(t, tx, `insert into issues (key, project_key, number, title, created_by, rank)
			values ('EVRY-1', $1, 100, 'An issue every write keeps', '{"kind":"user","id":"alice"}', 'A')`, project)
		return 0
	})
	requireWriteKept(t, database, "an issue's row embedded at its text, as commitEmbeddings commits it", func(tx pgx.Tx) int64 {
		embedRowAs(t, tx, "issue", "EVRY-1", configuredIdentity)
		return 0
	})
	requireWriteKept(t, database, "a title edit", func(tx pgx.Tx) int64 {
		mustExec(t, tx, `update issues set title = 'An issue every write keeps, edited' where key = 'EVRY-1'`)
		return 0
	})
	requireWriteKept(t, database, "a created_at change", func(tx pgx.Tx) int64 {
		mustExec(t, tx, `update issues set created_at = now() - interval '2 hours' where key = 'EVRY-1'`)
		return 0
	})

	// artifacts and artifact_versions.
	var spec string
	requireWriteKept(t, database, "a primary document's insert", func(tx pgx.Tx) int64 {
		if err := tx.QueryRow(ctx, `
			insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by)
			values ('EVRY-1', $1, 'spec', 'spec.md', 'doc', true, '{"kind":"user","id":"alice"}')
			returning id::text`, project).Scan(&spec); err != nil {
			t.Fatalf("insert EVRY-1's primary document: %v", err)
		}
		return 0
	})
	requireWriteKept(t, database, "a new primary version", func(tx pgx.Tx) int64 {
		addVersion(t, tx, spec, 1, "## Summary\n\nThe spec every write keeps.\n")
		return 0
	})
	requireWriteKept(t, database, "a primary version's markdown changed in place", func(tx pgx.Tx) int64 {
		mustExec(t, tx, `update artifact_versions set markdown = '## Summary\n\nThe spec, changed in place.\n' where artifact_id = $1 and number = 1`, spec)
		return 0
	})
	requireWriteKept(t, database, "a version of a document that is not primary", func(tx pgx.Tx) int64 {
		addVersion(t, tx, other, 1, "Notes on EVRY-2, which are not its spec.")
		return 0
	})
	requireWriteKept(t, database, "an update of is_primary", func(tx pgx.Tx) int64 {
		mustExec(t, tx, `update artifacts set is_primary = false where id = $1`, spec)
		return 0
	})
	requireWriteKept(t, database, "an update of issue_key", func(tx pgx.Tx) int64 {
		mustExec(t, tx, `update artifacts set issue_key = 'EVRY-2', is_primary = true where id = $1`, spec)
		return 0
	})
	requireWriteKept(t, database, "a primary document's removal, its versions first (api/artifact_removal.go)", func(tx pgx.Tx) int64 {
		for _, table := range []string{"artifact_versions", "doc_updates", "doc_snapshots", "doc_checkpoints", "document_search",
			"doc_compaction_owed", "search_term_queue", "user_threads"} {
			mustExec(t, tx, `delete from `+table+` where artifact_id = $1`, spec)
		}
		mustExec(t, tx, `delete from artifacts where id = $1`, spec)
		return 0
	})

	// embeddings: the windows, through their one writer.
	twoSections, twoSectionsBlocks := documentOf(
		[3]string{"h1", "heading", "# Survey"}, [3]string{"p1", "paragraph", "The first section's paragraph."},
		[3]string{"h2", "heading", "# Findings"}, [3]string{"p2", "paragraph", "The second section's paragraph."})
	rewritten, rewrittenBlocks := documentOf(
		[3]string{"h1", "heading", "# Survey"}, [3]string{"p1", "paragraph", "The first section's paragraph, rewritten."},
		[3]string{"h2", "heading", "# Findings"}, [3]string{"p2", "paragraph", "The second section's paragraph."})
	oneSection, oneSectionBlocks := documentOf(
		[3]string{"h1", "heading", "# Survey"}, [3]string{"p1", "paragraph", "The first section's paragraph, rewritten."})
	for _, write := range []struct {
		name, markdown string
		blocks         []model.ArtifactBlock
	}{
		{"a document's windows inserted (refs.WriteDocumentWindows)", twoSections, twoSectionsBlocks},
		{"a document's window rewritten (refs.WriteDocumentWindows)", rewritten, rewrittenBlocks},
		{"a document's window dropped (refs.WriteDocumentWindows)", oneSection, oneSectionBlocks},
	} {
		requireWriteKept(t, database, write.name, func(tx pgx.Tx) int64 {
			if err := refs.WriteDocumentWindows(ctx, tx, notes, write.markdown, write.blocks); err != nil {
				t.Fatalf("%s: %v", write.name, err)
			}
			return 0
		})
	}
	requireWriteKept(t, database, "a window whose text becomes blank", func(tx pgx.Tx) int64 {
		enqueueWindow(t, tx, "comment_block", "comment_block/oracle", "current", " \t ", "b1")
		return 0
	})
	requireWriteKept(t, database, "the delete of a whole-text row (docs/block_index.go)", func(tx pgx.Tx) int64 {
		mustExec(t, tx, `delete from embeddings where kind = 'comment' and id = 'every-write-comment'`)
		return 0
	})
	requireWriteKept(t, database, "a delete of an issue's row", func(tx pgx.Tx) int64 {
		mustExec(t, tx, `delete from embeddings where kind = 'issue' and id = 'EVRY-1'`)
		return 0
	})
	requireWriteKept(t, database, "an issue's row enqueued again through embeddings_enqueue", func(tx pgx.Tx) int64 {
		mustExec(t, tx, `select embeddings_enqueue('issue', key, `+issueText+`) from issues i where key = 'EVRY-1'`)
		return 0
	})

	// embeddings: the queue's own writes.
	deps := Deps{Store: database, Embedder: &fakeEmbedder{}}
	var claimed []pendingRow
	before := issueStateXmins(t, database)
	requirePoolWriteKept(t, database, "scanPending's settling of the blank rows and claim of the rest", func() int64 {
		pending, settled, err := scanPending(ctx, deps)
		if err != nil || settled == 0 || len(pending) == 0 {
			t.Fatalf("scanPending = %d claimed, %d settled, %v; want blank rows settled and rows claimed", len(pending), settled, err)
		}
		claimed = pending
		return int64(len(pending) + settled)
	})
	requireClaimRefreshedNothing(t, database, "scanPending's claim", claimed, before)

	before = issueStateXmins(t, database)
	renewal := newClaimRenewal(deps, claimed)
	renewal.last = time.Now().Add(-renewClaimInterval - time.Second)
	if deltas := requirePoolWriteKept(t, database, "claimRenewal's renewal of the claim", func() int64 {
		renewal.renew(ctx)
		return int64(len(claimed))
	}); deltas != 0 {
		t.Errorf("claimRenewal's renewal inserted %d delta rows, want none: a claim moves no count", deltas)
	}
	requireClaimRefreshedNothing(t, database, "claimRenewal's renewal", claimed, before)

	requirePoolWriteKept(t, database, "commitEmbeddings' embed of the claimed rows", func() int64 {
		succeeded, permanent, err := commitEmbeddings(ctx, deps, claimed, fakeVectors(len(claimed)))
		if err != nil || len(permanent) != 0 || succeeded != len(claimed) {
			t.Fatalf("commitEmbeddings = %d, %d permanent, %v; want all %d committed", succeeded, len(permanent), err, len(claimed))
		}
		return int64(succeeded)
	})
	requireWriteKept(t, database, "a title edit that makes an issue's row wait", func(tx pgx.Tx) int64 {
		mustExec(t, tx, `update issues set title = 'An issue a primary document moved to, edited' where key = 'EVRY-2'`)
		return 0
	})
	var reclaimed []pendingRow
	before = issueStateXmins(t, database)
	if deltas := requirePoolWriteKept(t, database, "scanPending's claim of a row, settling none", func() int64 {
		pending, settled, err := scanPending(ctx, deps)
		if err != nil || settled != 0 || !slices.ContainsFunc(pending, func(row pendingRow) bool { return row.kind == issueKind && row.id == "EVRY-2" }) {
			t.Fatalf("scanPending = %v, %d settled, %v; want EVRY-2's row claimed and nothing settled", pending, settled, err)
		}
		reclaimed = pending
		return int64(len(pending))
	}); deltas != 0 {
		t.Errorf("scanPending's claim inserted %d delta rows, want none: a claim moves no count", deltas)
	}
	requireClaimRefreshedNothing(t, database, "scanPending's claim of a row", reclaimed, before)
	row := reclaimed[slices.IndexFunc(reclaimed, func(row pendingRow) bool { return row.kind == issueKind && row.id == "EVRY-2" })]
	requirePoolWriteKept(t, database, "retryRows' failure of a row", func() int64 {
		if blocked := retryRows(ctx, deps, []pendingRow{row}, false); blocked {
			t.Fatal("retryRows' failure of EVRY-2's row did not write it")
		}
		return 1
	})
	row.attempts++
	row.confirmedFailures = retry.DeadLetterAttempts - 1
	requirePoolWriteKept(t, database, "retryRows' death of a row", func() int64 {
		if blocked := retryRows(ctx, deps, []pendingRow{row}, true); blocked {
			t.Fatal("retryRows' death of EVRY-2's row did not write it")
		}
		return 1
	})
	var dead bool
	if err := q.QueryRow(ctx, `select dead from embeddings where kind = 'issue' and id = 'EVRY-2'`).Scan(&dead); err != nil || !dead {
		t.Fatalf("EVRY-2's row is dead %t (%v) after retryRows' death, want dead", dead, err)
	}

	// embeddings: the coverage pass's steps, each its own statement.
	for _, step := range []struct {
		name      string
		statement string
		kinds     []string
		identity  bool
	}{
		{"insertMissingIssues", insertMissingIssues, []string{issueKind}, false},
		{"repairStaleIssues", repairStaleIssues, []string{issueKind}, false},
		{"dropForeignVectors after an identity change", dropForeignVectors, coverageKinds, true},
		{"readmitForeignDead", readmitForeignDead, coverageKinds, true},
		{"settleDeadNothingToEmbed", settleDeadNothingToEmbed, coverageKinds, true},
	} {
		requireWriteKept(t, database, step.name, func(tx pgx.Tx) int64 {
			for _, kind := range step.kinds {
				args := []any{kind, coverageStatementRows}
				if step.identity {
					args = []any{kind, configuredIdentity, coverageStatementRows}
				}
				if _, err := tx.Exec(ctx, step.statement, args...); err != nil {
					t.Fatalf("%s of %s: %v", step.name, kind, err)
				}
			}
			return 0
		})
	}

	// embeddings: the writes of other packages.
	requireWriteKept(t, database, "the seed's vector writes (cmd/dispatch-seed)", func(tx pgx.Tx) int64 {
		literal := embed.Literal(oneVector())
		mustExec(t, tx, `
			update embeddings e
			   set embedding = v.embedding::vector, model = $1, embedded_hash = e.content_hash,
			       embedded_at = $2, pending_since = $2, next_attempt_at = $2, attempt_count = 0
			  from unnest($3::text[], $4::text[], $5::text[]) as v (kind, id, embedding)
			 where e.kind = v.kind and e.id = v.id`, configuredIdentity, time.Now(),
			[]string{"document_block", "message_block"}, []string{"document_block/oracle#pending", "message_block/oracle#pending"},
			[]string{literal, literal})
		return 0
	})
	var labelled int64
	if deltas := requireWriteKept(t, database, "the project_key fill (store.FillEmbeddingProjectKeys)", func(tx pgx.Tx) int64 {
		tag, err := tx.Exec(ctx, `
			update embeddings e set project_key = embeddings_owner_project_key(e.kind, e.id)
			where e.project_key is null and embeddings_owner_project_key(e.kind, e.id) is not null`)
		if err != nil {
			t.Fatalf("fill project_key: %v", err)
		}
		labelled = tag.RowsAffected()
		return 0
	}); deltas != 0 || labelled == 0 {
		t.Errorf("the project_key fill labelled %d rows and inserted %d delta rows, want rows labelled and no delta row: it moves no count", labelled, deltas)
	}

	// components, check_results, handoffs and route_roster.
	requireWriteKept(t, database, "a source's re-import, delete then insert (architecture/importer.go)", func(tx pgx.Tx) int64 {
		mustExec(t, tx, `delete from components where project_key = $1 and source_id = 1`, project)
		for _, id := range []string{"new-a", "new-b", "new-c"} {
			insertComponent(t, tx, project, id, 906)
		}
		return 0
	})
	requireWriteKept(t, database, "a result's insert", func(tx pgx.Tx) int64 {
		insertResult(tx, "EVRY-3", notes)
		return 0
	})
	requireWriteKept(t, database, "a handoff's insert", func(tx pgx.Tx) int64 {
		mustExec(t, tx, `insert into handoffs (issue_key, phase, version, body, author)
			values ('EVRY-3', 'implement', 1, '{}', '{"kind":"user","id":"alice"}'::jsonb)`)
		return 0
	})
	requireWriteKept(t, database, "an issue's delete, which cascades to its result and its handoff", func(tx pgx.Tx) int64 {
		mustExec(t, tx, `delete from issues where key = 'EVRY-3'`)
		return 0
	})
	requireWriteKept(t, database, "a document's delete, which cascades to a result through check_artifact_id", func(tx pgx.Tx) int64 {
		mustExec(t, tx, `delete from artifacts where id = $1`, checked)
		return 0
	})
	requireWriteKept(t, database, "route_roster's upsert", func(tx pgx.Tx) int64 {
		mustExec(t, tx, `insert into route_roster (answered, sessions, sent_at, expires_at)
			values (true, '[{"session_id":"s-one"},{"session_id":"s-two"},{"session_id":"s-three"}]'::jsonb, now(), now() + interval '5 seconds')
			on conflict (id) do update set answered = excluded.answered, sessions = excluded.sessions,
			  sent_at = excluded.sent_at, expires_at = excluded.expires_at`)
		return 0
	})

	// Last, a truncate of embeddings, which empties every window kind.
	requireWriteKept(t, database, "a truncate of embeddings", func(tx pgx.Tx) int64 {
		var rows int64
		if err := tx.QueryRow(ctx, `select count(*) from embeddings`).Scan(&rows); err != nil {
			t.Fatalf("count embeddings' rows: %v", err)
		}
		mustExec(t, tx, `truncate embeddings`)
		return rows
	})
}

// TestTheNextRecountFollowsEveryRowWrittenWithTriggersOff holds R7d: rows written with no trigger
// firing - an issue's title, an issue's spec in place, a window row of embeddings inserted and
// another deleted, and a row of each not-indexed table a count keeps (components,
// artifact_versions, check_results, handoffs), each inside withTableTriggersOff, and an issue
// written under session_replication_role = replica (withTriggersOff) - are in the counts after one
// recount (recountCoverage), which the report's three sides then answer alike (requireMainsAnswerFor).
// It runs once with an embedder configured, the queue having embedded every row it could with that
// identity before the writes and the report read for it, and once with none, the report read for
// no identity. Before the recount the counts miss the writes: the report counts one component fewer
// than the table holds.
func TestTheNextRecountFollowsEveryRowWrittenWithTriggersOff(t *testing.T) {
	for _, configured := range []struct{ name, identity string }{
		{"an embedder configured", configuredIdentity},
		{"no embedder", ""},
	} {
		t.Run(configured.name, func(t *testing.T) {
			ctx := context.Background()
			database := storetest.Open(t)
			q := database.Pool
			states := seedEveryCoverageState(t, database)
			const project = "OFFT"
			seedIssue(t, database, project, "OFFT-1", "An issue whose result and handoff are written with their triggers off")
			seedComponentsArchitecture(t, q, project, 907)
			var document string
			if err := q.QueryRow(ctx, `
				insert into artifacts (project_key, slug, name, kind, created_by, ref_key)
				values ($1, 'notes', 'notes.md', 'doc', '{"kind":"user","id":"alice"}', $1 || '/notes')
				returning id::text`, project).Scan(&document); err != nil {
				t.Fatalf("seed the project document: %v", err)
			}
			fillCounts(t, database)
			if configured.identity != "" {
				drain(t, Deps{Store: database, Embedder: newPlaceEmbedder(configured.identity)})
			}

			withTableTriggersOff(t, database, "issues", func(q querier) {
				mustExec(t, q, `update issues set title = 'A title written with its trigger off' where key = $1`, states.titleEdited)
			})
			withTableTriggersOff(t, database, "artifact_versions", func(q querier) {
				mustExec(t, q, `update artifact_versions set markdown = $2 where artifact_id = $1 and number = 2`,
					states.specRestoredDocument, "## Summary\n\nThe latest version, written in place with its triggers off.\n")
				addVersion(t, q, document, 1, "A version written with its table's triggers off.")
			})
			withTableTriggersOff(t, database, "embeddings", func(q querier) {
				enqueueWindow(t, q, "document_block", "document_block/off", "b1", "A window written with its table's triggers off", "b1")
				mustExec(t, q, `delete from embeddings where kind = 'comment_block' and id = 'comment_block/oracle#current'`)
			})
			withTableTriggersOff(t, database, "components", func(q querier) {
				insertComponent(t, q, project, "written-with-its-triggers-off", 907)
			})
			withTableTriggersOff(t, database, "check_results", func(q querier) {
				mustExec(t, q, `insert into check_results (issue_key, check_issue_key, check_artifact_id, check_block_id,
						check_version, check_sha256, role, commit_sha, verdict, actor)
					values ('OFFT-1', 'OFFT-1', $1::uuid, 'k1', 1, 'a-check-sha', 'tester', repeat('a', 40), 'pass', '{"kind":"user","id":"alice"}'::jsonb)`,
					document)
			})
			withTableTriggersOff(t, database, "handoffs", func(q querier) {
				mustExec(t, q, `insert into handoffs (issue_key, phase, version, body, author)
					values ('OFFT-1', 'implement', 1, '{}', '{"kind":"user","id":"alice"}'::jsonb)`)
			})
			withTriggersOff(t, database, func(q querier) {
				mustExec(t, q, `insert into issues (key, project_key, number, title, created_by, rank)
					values ('OFFT-2', $1, 100, 'An issue written under session_replication_role = replica', '{"kind":"user","id":"alice"}', 'A')`, project)
			})

			report, err := Coverage(ctx, database, configured.identity)
			if err != nil {
				t.Fatalf("Coverage before the recount: %v", err)
			}
			var components int64
			if err := q.QueryRow(ctx, `select count(*) from components`).Scan(&components); err != nil {
				t.Fatalf("count the components: %v", err)
			}
			if kept := notIndexedCountsOf(t, report)["component"]; kept != components-1 {
				t.Fatalf("before the recount the counts hold %d components of the table's %d, want one fewer: the component written with its triggers off fired a trigger", kept, components)
			}

			if err := recountCoverage(ctx, database); err != nil {
				t.Fatalf("recount: %v", err)
			}
			requireMainsAnswerFor(t, database, "after one recount", configured.identity)
		})
	}
}

// TestTheRefreshAndTheRecountRenderAnIssuesInputsAlike holds the two renderings of an issue's
// source equal: this package's (issueSourceFrom, issueSource), which the record step records
// (recordIssueSources) and the recount's find of stale issues compares (staleIssueKeys), and
// search_coverage_issue_refresh's own (migration 20261124011834_search_coverage_counts), which
// compares the recorded source and writes the state row's inputs. Were they to differ no count
// would be wrong, so no other test would fail, but every refresh would build and hash its issue's
// text and every recount would find every issue stale. It seeds an issue of each shape of input
// the rendering reads - a primary document of two versions, one of one version, one of none, a
// document of the issue that is not its primary one, no document, an embeddings row and none -
// records their sources and fills the counts, and then the find answers no key. As its control,
// a title written with its table's triggers off makes the find answer that issue's key alone.
func TestTheRefreshAndTheRecountRenderAnIssuesInputsAlike(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	q := database.Pool
	const project = "RNDR"
	seedIssue(t, database, project, "RNDR-1", "An issue whose primary document has two versions")
	twoVersions := seedSpec(t, q, project, "RNDR-1", "## Summary\n\nThe first version.\n")
	addVersion(t, q, twoVersions, 2, "## Summary\n\nThe latest version.\n")
	seedIssue(t, database, project, "RNDR-2", "An issue whose primary document has one version")
	seedSpec(t, q, project, "RNDR-2", "## Summary\n\nIts one version.\n")
	seedIssue(t, database, project, "RNDR-3", "An issue whose primary document has no version")
	mustExec(t, q, `
		insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by)
		values ('RNDR-3', $1, 'spec', 'spec.md', 'doc', true, '{"kind":"user","id":"alice"}')`, project)
	seedIssue(t, database, project, "RNDR-4", "An issue whose one document is not its primary one")
	var notes string
	if err := q.QueryRow(ctx, `
		insert into artifacts (issue_key, project_key, slug, name, kind, created_by)
		values ('RNDR-4', $1, 'notes', 'notes.md', 'doc', '{"kind":"user","id":"alice"}')
		returning id::text`, project).Scan(&notes); err != nil {
		t.Fatalf("seed RNDR-4's document: %v", err)
	}
	addVersion(t, q, notes, 1, "Notes, not the spec.")
	seedIssue(t, database, project, "RNDR-5", "An issue with no document")
	seedIssue(t, database, project, "RNDR-6", "An issue the queue holds no row of")
	mustExec(t, q, `delete from embeddings where kind = 'issue' and id = 'RNDR-6'`)

	// stale is what the recount's find answers over every issue, and for each key it answers, the
	// inputs its state row records beside the inputs the find renders.
	stale := func() (keys, differences []string) {
		t.Helper()
		rows, err := q.Query(ctx, staleIssueKeys, "", coverageStatementRows)
		if err != nil {
			t.Fatalf("find the stale issues: %v", err)
		}
		if keys, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
			t.Fatalf("read the stale issues: %v", err)
		}
		rows, err = q.Query(ctx, `
			select i.key, coalesce(s.inputs, '(no state row)'), search_coverage_issue_inputs(`+issueSource+`, i.created_at, e.id, e.xmin)
			from `+issueSourceFrom+`
			left join search_coverage_issues s on s.key = i.key
			where i.key = any($1) order by i.key`, keys)
		if err != nil {
			t.Fatalf("read the stale issues' inputs: %v", err)
		}
		differences, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (string, error) {
			var key, recorded, rendered string
			err := row.Scan(&key, &recorded, &rendered)
			return fmt.Sprintf("%s: its state row records %q, staleIssueKeys renders %q", key, recorded, rendered), err
		})
		if err != nil {
			t.Fatalf("read the stale issues' inputs: %v", err)
		}
		return keys, differences
	}

	recordOnce(t, database)
	fillCounts(t, database)
	if keys, differences := stale(); len(keys) > 0 {
		t.Fatalf("after the fill staleIssueKeys answers %d issues, want none: search_coverage_issue_refresh and issueSourceFrom with issueSource render their inputs apart:\n%s",
			len(keys), strings.Join(differences, "\n"))
	}

	withTableTriggersOff(t, database, "issues", func(q querier) {
		mustExec(t, q, `update issues set title = 'A title written with its trigger off' where key = 'RNDR-1'`)
	})
	if keys, differences := stale(); !slices.Equal(keys, []string{"RNDR-1"}) {
		t.Fatalf("after RNDR-1's title was written with its table's triggers off staleIssueKeys answers %v, want RNDR-1 alone:\n%s",
			keys, strings.Join(differences, "\n"))
	}
}

// ───── the counts loop: the fold, the recount, the fill (R7e to R7i, R7o, R7x) ─────

// setForTest sets variable to value until the test ends, as shortenIdentityRecheck sets the
// identity recheck's interval: the counts loop's periods, bounds, random sources and clock are
// package variables, and no test that sets one runs in parallel.
func setForTest[T any](t *testing.T, variable *T, value T) {
	t.Helper()
	saved := *variable
	*variable = value
	t.Cleanup(func() { *variable = saved })
}

// fractions is a random source that answers values in turn, from the first again after the last.
func fractions(values ...float64) func() float64 {
	var mu sync.Mutex
	next := 0
	return func() float64 {
		mu.Lock()
		defer mu.Unlock()
		value := values[next%len(values)]
		next++
		return value
	}
}

// fakeCountsClock is a counts clock (countsClock) a test moves itself: After records the wait it was
// asked for, calls onWait with how many waits came before it, and moves the time on by the wait at
// once, so the loop's periods run as fast as their work does and fake time stands still while
// it works. Once onWait answers true it cancels the loop's context (stop) and answers a channel that
// never fires.
type fakeCountsClock struct {
	mu     sync.Mutex
	now    time.Time
	waits  []time.Duration
	onWait func(index int) (stop bool)
	stop   context.CancelFunc
}

func (c *fakeCountsClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeCountsClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	index := len(c.waits)
	c.waits = append(c.waits, d)
	c.mu.Unlock()
	if c.onWait(index) {
		c.stop()
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	fired := make(chan time.Time, 1)
	fired <- c.now
	return fired
}

// clockedLine is one line the counts loop logged, at the fake time it logged it.
type clockedLine struct {
	at      time.Time
	level   slog.Level
	message string
	attrs   map[string]slog.Value
}

// clockedLog is a slog.Handler that keeps every line with clock's time, so a test reads when, in
// the loop's own time, a recount started or failed.
type clockedLog struct {
	clock *fakeCountsClock
	mu    sync.Mutex
	lines []clockedLine
}

func (l *clockedLog) Enabled(context.Context, slog.Level) bool { return true }

func (l *clockedLog) Handle(_ context.Context, record slog.Record) error {
	at := l.clock.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, clockedLine{at, record.Level, record.Message, attrsOf(record)})
	return nil
}

func (l *clockedLog) WithAttrs([]slog.Attr) slog.Handler { return l }
func (l *clockedLog) WithGroup(string) slog.Handler      { return l }

// logged is every line l holds of message, in order.
func (l *clockedLog) logged(message string) []clockedLine {
	l.mu.Lock()
	defer l.mu.Unlock()
	var matched []clockedLine
	for _, line := range l.lines {
		if line.message == message {
			matched = append(matched, line)
		}
	}
	return matched
}

// The lines the counts loop logs.
const (
	recountStartedLine = "dispatch embedqueue: coverage counts recount started"
	recountedLine      = "dispatch embedqueue: coverage counts recounted"
	recountFailedLine  = "dispatch embedqueue: coverage counts recount failed; it runs again later"
	fillFailedLine     = "dispatch embedqueue: coverage counts fill failed; it runs again later"
	foldFailedLine     = "dispatch embedqueue: coverage counts fold failed; it runs again at the next period"
)

// clockedStatements records the SQL of every statement started on the pool it traces
// (storetest.Traced), each with clock's time.
type clockedStatements struct {
	clock *fakeCountsClock
	mu    sync.Mutex
	sent  []clockedStatement
}

type clockedStatement struct {
	at  time.Time
	sql string
}

func (c *clockedStatements) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	at := c.clock.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sent = append(c.sent, clockedStatement{at, data.SQL})
	return ctx
}

func (c *clockedStatements) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// of is when each statement whose SQL is sql started, in order.
func (c *clockedStatements) of(sql string) []time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	var at []time.Time
	for _, sent := range c.sent {
		if sent.sql == sql {
			at = append(at, sent.at)
		}
	}
	return at
}

// servingNow is a Serving channel already closed: the process serves.
func servingNow() <-chan struct{} {
	serving := make(chan struct{})
	close(serving)
	return serving
}

// runCountsUntilStopped runs RunCounts on database with clock as the loop's clock (countsTime)
// until clock stops it, and fails the test when it does not return within within.
func runCountsUntilStopped(t *testing.T, database *store.Store, clock *fakeCountsClock, within time.Duration) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	clock.stop = cancel
	setForTest[countsClock](t, &countsTime, clock)
	done := make(chan struct{})
	go func() {
		defer close(done)
		RunCounts(ctx, CountsDeps{Store: database, Serving: servingNow()})
	}()
	select {
	case <-done:
	case <-time.After(within):
		cancel()
		<-done
		t.Fatalf("the counts loop did not reach the end of its run within %v (waits asked for: %v)", within, clock.waits)
	}
}

// seedComponentsArchitecture writes the architecture source and snapshot project's components
// hang from.
func seedComponentsArchitecture(t *testing.T, q querier, project string, snapshot int) {
	t.Helper()
	mustExec(t, q, `insert into projects (key, name, default_assignee) values ($1, 'Test', 'alice') on conflict (key) do nothing`, project)
	mustExec(t, q, `insert into architecture_sources (project_key, id, repo, branch, installation_id, created_by)
		values ($1, 1, 'live/arch', 'main', 1, '{"kind":"system","id":"test"}')`, project)
	mustExec(t, q, `insert into architecture_snapshots (id, project_key, commit, files) values ($2, $1, 'c1', '{}')`, project, snapshot)
}

// insertComponent writes component id of project, from the snapshot seedComponentsArchitecture
// wrote.
func insertComponent(t *testing.T, q querier, project, id string, snapshot int) {
	t.Helper()
	mustExec(t, q, `insert into components (project_key, id, title, prose, snapshot_id, source_id) values ($1, $2, $2, '', $3, 1)`,
		project, id, snapshot)
}

// TestTheCountsLoopRecountsAtLeastEveryElevenMinutes holds R7e. Over 10,000 draws of the gap the
// loop waits from one recount's start to the next (recountGap), for T (coverageRecountTimeout) at
// 20 s (its floor), at 59 s, at 60 s and at 5 minutes, every gap is at least coverageRecountInterval and at most
// that plus coverageRecountJitter, and that sum plus T is at most 11 minutes; the loop's own
// values are the schedule of its own T. The loop starts each recount that gap after the one before
// started, read in its own time with the random source injected. And with the interval shortened,
// RunCounts, with no embedder and with one embedding beside it (Run), recounts twice, and the
// counts follow a component and an issue's title written between the two with their tables'
// triggers off.
func TestTheCountsLoopRecountsAtLeastEveryElevenMinutes(t *testing.T) {
	if interval, jitter := recountSchedule(coverageRecountTimeout); coverageRecountInterval != interval || coverageRecountJitter != jitter {
		t.Errorf("the loop recounts every %v plus up to %v, want its T's (%v) schedule, %v plus up to %v",
			coverageRecountInterval, coverageRecountJitter, coverageRecountTimeout, interval, jitter)
	}
	for _, timeout := range []time.Duration{20 * time.Second, 59 * time.Second, time.Minute, 5 * time.Minute} {
		interval, jitter := recountSchedule(timeout)
		if interval+jitter+timeout > 11*time.Minute {
			t.Errorf("T %v: the interval %v plus the jitter %v plus T pass 11 minutes", timeout, interval, jitter)
		}
		setForTest(t, &coverageRecountInterval, interval)
		setForTest(t, &coverageRecountJitter, jitter)
		for range 10000 {
			if gap := recountGap(); gap < interval || gap > interval+jitter {
				t.Fatalf("T %v: a recount starts %v after the one before, want %v to %v", timeout, gap, interval, interval+jitter)
			}
		}
	}

	t.Run("the loop's own gaps", func(t *testing.T) {
		database := storetest.Open(t)
		seedIssue(t, database, "GAPS", "GAPS-1", "An issue the recounts count")
		fillCounts(t, database)
		interval, jitter := recountSchedule(20 * time.Second)
		setForTest(t, &coverageRecountInterval, interval)
		setForTest(t, &coverageRecountJitter, jitter)
		drawn := []float64{0, 0.999, 0.5}
		setForTest(t, &coverageRecountRandom, fractions(drawn...))
		clock := &fakeCountsClock{now: time.Date(2026, 11, 24, 0, 0, 0, 0, time.UTC)}
		log := &clockedLog{clock: clock}
		logtest.SetDefault(t, log)
		clock.onWait = func(int) bool { return len(log.logged(recountStartedLine)) >= 4 }
		runCountsUntilStopped(t, database, clock, 2*time.Minute)
		started := log.logged(recountStartedLine)
		for k := 1; k < len(started); k++ {
			want := interval + time.Duration(drawn[(k-1)%len(drawn)]*float64(jitter))
			if gap := started[k].at.Sub(started[k-1].at); gap != want {
				t.Errorf("recount %d started %v after the one before, want %v", k+1, gap, want)
			}
		}
	})

	for _, withEmbedder := range []bool{false, true} {
		name := "no embedder"
		if withEmbedder {
			name = "an embedder"
		}
		t.Run(name, func(t *testing.T) {
			database := storetest.Open(t)
			states := seedEveryCoverageState(t, database)
			seedComponentsArchitecture(t, database.Pool, "ORCL", 902)
			fillCounts(t, database)
			setForTest(t, &coverageRecountInterval, time.Second)
			setForTest(t, &coverageRecountJitter, 0)
			records := &logtest.Recorder{}
			logtest.SetDefault(t, records)
			if withEmbedder {
				runQueue(t, Deps{Store: database, Embedder: newSyncEmbedder(configuredIdentity)})
			}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() {
				defer close(done)
				RunCounts(ctx, CountsDeps{Store: database, Serving: servingNow()})
			}()
			t.Cleanup(func() { cancel(); <-done })
			eventually(t, 30*time.Second, "a first recount", func() bool {
				return len(passRecords(records, slog.LevelInfo, recountedLine)) > 0
			})

			withTableTriggersOff(t, database, "components", func(q querier) {
				insertComponent(t, q, "ORCL", "written-with-its-triggers-off", 902)
			})
			withTableTriggersOff(t, database, "issues", func(q querier) {
				mustExec(t, q, `update issues set title = 'A title written with its trigger off' where key = $1`, states.titleEdited)
			})
			written := len(records.Records())
			eventually(t, 30*time.Second, "a recount started after the writes, and its end", func() bool {
				after := records.Records()[written:]
				for position, record := range after {
					if record.Message == recountStartedLine {
						return slices.ContainsFunc(after[position:], func(record slog.Record) bool { return record.Message == recountedLine })
					}
				}
				return false
			})
			requireMainsAnswer(t, database, "after the recount that followed the writes with their triggers off")
		})
	}
}

// liveWrites is one live writer's writes, on q, of each of the three sorts of table the counts
// keep: on embeddings, a window written, a window embedded and a window deleted; on issues, two
// issues written, an issue's title edited, an issue's row embedded and an issue deleted; and on
// components, two components written and one deleted. suffix names what the writer writes, so two
// writers write different rows; the rows it embeds, edits and deletes are its arguments. Each
// sort moves a count by more than nothing, so a count that lost or doubled the writes would show.
type liveWrites struct {
	suffix                       string
	embedWindow, deleteWindow    [2]string // kind, id
	editIssue, embedIssue        string
	deleteIssue, deleteComponent string
}

func (w liveWrites) write(t *testing.T, q querier) {
	t.Helper()
	enqueueWindow(t, q, "document_block", "document_block/live", w.suffix, "A window a live writer wrote", "b1")
	embedRowAs(t, q, w.embedWindow[0], w.embedWindow[1], configuredIdentity)
	mustExec(t, q, `delete from embeddings where kind = $1 and id = $2`, w.deleteWindow[0], w.deleteWindow[1])
	mustExec(t, q, `insert into issues (key, project_key, number, title, created_by, rank)
		select 'LIVE-' || $1 || '-' || n, 'LIVE', 100 * n + $1::int, 'An issue a live writer wrote', '{"kind":"user","id":"alice"}', 'A'
		from (values (1), (2)) written (n)`, w.suffix)
	mustExec(t, q, `update issues set title = 'A title a live writer edited ' || $2 where key = $1`, w.editIssue, w.suffix)
	embedRowAs(t, q, "issue", w.embedIssue, configuredIdentity)
	mustExec(t, q, `delete from issues where key = $1`, w.deleteIssue)
	insertComponent(t, q, "LIVE", "live-"+w.suffix, 903)
	insertComponent(t, q, "LIVE", "live-"+w.suffix+"-b", 903)
	mustExec(t, q, `delete from components where project_key = 'LIVE' and id = $1`, w.deleteComponent)
}

// countsHolds are the groups the recount and the fill count in a statement of their own, each with
// the kind of a class every database requireCountsBesideLiveWrites seeds holds live, unmatched,
// with no model: the statement of the group is held on that class's base row, which the fold
// writes too.
var countsHolds = []struct{ group, kind string }{
	{"the issue group", issueKind},
	{"the window group", "document_block"},
	{"a not-indexed group", "component"},
}

// requireCountsBesideLiveWrites holds R7f for one of the three statements that write the base
// (the recount, the fill, the fold), which run calls once, held in each group in turn. On other
// connections, a first writer commits its writes (liveWrites) before the statement's snapshot, and
// a second opens and writes before it and commits after it while the statement still runs: the
// test holds the statement on a class's base row of the group (countsHolds), which a third
// transaction holds - locked for update, or for the fill, which finds no base row, inserted and not
// committed - and which no writer's commit needs, since a write inserts delta rows and never touches
// the base. The second writer commits while the statement waits, then the hold ends. After the next
// fold, the counts equal main's statement's.
func requireCountsBesideLiveWrites(t *testing.T, fill bool, run func(context.Context, *store.Store) error) {
	t.Helper()
	for _, held := range countsHolds {
		t.Run("held in "+held.group, func(t *testing.T) {
			requireCountsBesideLiveWritesHeld(t, fill, run, held.kind)
		})
	}
}

// requireCountsBesideLiveWritesHeld is requireCountsBesideLiveWrites with the statement held on
// the base row of kind's live unmatched class.
func requireCountsBesideLiveWritesHeld(t *testing.T, fill bool, run func(context.Context, *store.Store) error, kind string) {
	t.Helper()
	ctx := context.Background()
	database := storetest.Open(t)
	seedEveryCoverageState(t, database)
	seedComponentsArchitecture(t, database.Pool, "LIVE", 903)
	for _, suffix := range []string{"0", "00"} {
		mustExec(t, database.Pool, `insert into issues (key, project_key, number, title, created_by, rank)
			values ('LIVE-' || $1, 'LIVE', length($1), 'An issue a live writer deletes', '{"kind":"user","id":"alice"}', 'A')`, suffix)
		insertComponent(t, database.Pool, "LIVE", "live-"+suffix, 903)
	}
	if !fill {
		fillCounts(t, database)
	}
	first := liveWrites{suffix: "1", embedWindow: [2]string{"document_block", "document_block/oracle#pending"},
		deleteWindow: [2]string{"comment_block", "comment_block/oracle#current"},
		editIssue:    "ORCL-1", embedIssue: "ORCL-13", deleteIssue: "LIVE-0", deleteComponent: "live-0"}
	second := liveWrites{suffix: "2", embedWindow: [2]string{"message_block", "message_block/oracle#pending"},
		deleteWindow: [2]string{"ask_part", "ask_part/oracle#current"},
		editIssue:    "ORCL-2", embedIssue: "ORCL-12", deleteIssue: "LIVE-00", deleteComponent: "live-00"}

	begin := func(what string) pgx.Tx {
		t.Helper()
		tx, err := database.Pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin %s: %v", what, err)
		}
		t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
		return tx
	}
	writer := begin("the first writer")
	first.write(t, writer)
	if err := writer.Commit(ctx); err != nil {
		t.Fatalf("commit the first writer: %v", err)
	}
	open := begin("the second writer")
	second.write(t, open)
	hold := begin("the hold on a base row")
	if fill {
		mustExec(t, hold, `insert into search_coverage_counts (kind, model, matched, dead, items) values ($1, '', false, false, 0)`, kind)
	} else {
		tag, err := hold.Exec(ctx, `select 1 from search_coverage_counts where kind = $1 and model = '' and not matched and not dead for update`, kind)
		if err != nil || tag.RowsAffected() != 1 {
			t.Fatalf("hold the base row of %s's live unmatched class: %v (rows %d)", kind, err, tag.RowsAffected())
		}
	}

	ran := make(chan error, 1)
	go func() { ran <- run(ctx, database) }()
	storetest.WaitForLockWaiters(t, hold, 1)
	committed := make(chan error, 1)
	go func() { committed <- open.Commit(ctx) }()
	select {
	case err := <-committed:
		if err != nil {
			t.Fatalf("commit the second writer: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the second writer's commit waited on the statement or the hold")
	}
	select {
	case err := <-ran:
		t.Fatalf("the statement ended (%v) while the hold still stood", err)
	default:
	}
	if err := hold.Rollback(ctx); err != nil {
		t.Fatalf("end the hold: %v", err)
	}
	select {
	case err := <-ran:
		if err != nil {
			t.Fatalf("the statement beside the live writes: %v", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("the statement did not end within 60s of the hold's end")
	}
	foldCounts(t, database)
	requireMainsAnswer(t, database, "after the next fold")
	if _, err := Coverage(ctx, database, configuredIdentity); err != nil {
		t.Errorf("Coverage after the next fold: %v", err)
	}
}

// TestARecountBesideLiveWritesLosesAndDoublesNothing holds R7f for the recount
// (requireCountsBesideLiveWrites).
func TestARecountBesideLiveWritesLosesAndDoublesNothing(t *testing.T) {
	requireCountsBesideLiveWrites(t, false, recountCoverage)
}

// TestAFillBesideLiveWritesLosesAndDoublesNothing holds R7f for the fill, on counts that hold
// nothing yet (requireCountsBesideLiveWrites).
func TestAFillBesideLiveWritesLosesAndDoublesNothing(t *testing.T) {
	requireCountsBesideLiveWrites(t, true, FillCoverage)
}

// TestAFoldBesideLiveWritesLosesAndDoublesNothing holds R7f for the fold
// (requireCountsBesideLiveWrites).
func TestAFoldBesideLiveWritesLosesAndDoublesNothing(t *testing.T) {
	requireCountsBesideLiveWrites(t, false, foldCoverage)
}

// countsLockWaits names, by backend, every session of this test's database that waits for the
// counts lock (countsLock), with the backends that hold it from it (pg_blocking_pids), and how many
// sessions hold it.
const countsLockWaits = `
	select coalesce(array_agg(pid) filter (where not granted), '{}'),
	       coalesce(array_agg(blocker) filter (where not granted), '{}'),
	       count(*) filter (where granted)
	from (select l.pid, l.granted, (pg_blocking_pids(l.pid))[1] as blocker
	      from pg_locks l
	      where l.locktype = 'advisory' and l.objsubid = 1
	        and l.database = (select oid from pg_database where datname = current_database())
	        and l.objid::bigint = (hashtext('search-coverage-counts')::bigint & 4294967295)) counted`

// TestAFoldAndARecountTakeTurnsOnOneTransactionLock holds R7g: a fold and a recount started at once
// take turns on the one counts lock. The first is held, inside its transaction and so holding the
// lock, on the component's base row, which the test holds; the second, started while it is held,
// waits for the lock, and pg_locks names the first's backend as what it waits on. Once the hold
// ends both end, the counts equal main's statement's, and pg_locks holds the counts lock for no
// session, the two pooled connections that ran them still open: the lock ended with each
// transaction.
func TestAFoldAndARecountTakeTurnsOnOneTransactionLock(t *testing.T) {
	ops := map[string]func(context.Context, *store.Store) error{"fold": foldCoverage, "recount": recountCoverage}
	for _, order := range [][2]string{{"recount", "fold"}, {"fold", "recount"}} {
		t.Run("the "+order[1]+" waits for the "+order[0], func(t *testing.T) {
			ctx := context.Background()
			database := storetest.Open(t)
			seedEveryCoverageState(t, database)
			seedComponentsArchitecture(t, database.Pool, "TURN", 904)
			fillCounts(t, database)
			// A component's delta row, so the fold too writes the component's base row.
			insertComponent(t, database.Pool, "TURN", "turns", 904)
			hold, err := database.Pool.Begin(ctx)
			if err != nil {
				t.Fatalf("begin the hold: %v", err)
			}
			t.Cleanup(func() { _ = hold.Rollback(context.Background()) })
			mustExec(t, hold, `select 1 from search_coverage_counts where kind = 'component' for update`)

			ended := make(chan error, 2)
			go func() { ended <- ops[order[0]](ctx, database) }()
			storetest.WaitForLockWaiters(t, hold, 1)
			var waiting, blockers []int32
			var holders int
			if err := database.Pool.QueryRow(ctx, countsLockWaits).Scan(&waiting, &blockers, &holders); err != nil || holders != 1 || len(waiting) != 0 {
				t.Fatalf("while the %s is held, the counts lock is held by %d sessions and waited for by %v (%v), want held by it alone",
					order[0], holders, waiting, err)
			}
			var holderPID int32
			if err := database.Pool.QueryRow(ctx, `select pid from pg_locks where locktype = 'advisory' and granted
				and database = (select oid from pg_database where datname = current_database())`).Scan(&holderPID); err != nil {
				t.Fatalf("read the counts lock's holder: %v", err)
			}
			go func() { ended <- ops[order[1]](ctx, database) }()
			eventually(t, 30*time.Second, "the "+order[1]+" waiting for the counts lock", func() bool {
				if err := database.Pool.QueryRow(ctx, countsLockWaits).Scan(&waiting, &blockers, &holders); err != nil {
					t.Fatalf("read the counts lock's waiters: %v", err)
				}
				return len(waiting) == 1
			})
			if blockers[0] != holderPID {
				t.Errorf("the %s waits for the counts lock on backend %d, want the %s's backend %d", order[1], blockers[0], order[0], holderPID)
			}
			if err := hold.Rollback(ctx); err != nil {
				t.Fatalf("end the hold: %v", err)
			}
			for range 2 {
				select {
				case err := <-ended:
					if err != nil {
						t.Fatalf("a fold or a recount: %v", err)
					}
				case <-time.After(60 * time.Second):
					t.Fatal("the fold and the recount did not both end within 60s of the hold's end")
				}
			}
			if err := database.Pool.QueryRow(ctx, countsLockWaits).Scan(&waiting, &blockers, &holders); err != nil || holders != 0 || len(waiting) != 0 {
				t.Errorf("after both transactions ended the counts lock is held by %d sessions and waited for by %v (%v), want none",
					holders, waiting, err)
			}
			requireMainsAnswer(t, database, "after the fold and the recount")
		})
	}
}

// TestTheFoldRunsEveryTenSecondsPlusItsJitter holds R7h: with the fold's random source injected,
// each fold starts 10 s plus the injected fraction of 1 s after the one before, read in the loop's
// own time at the fold statement each sends (foldDeltas), the first that long after the loop's
// first run ended. No recount falls due in the time it runs.
func TestTheFoldRunsEveryTenSecondsPlusItsJitter(t *testing.T) {
	database := storetest.Open(t)
	seedIssue(t, database, "FOLD", "FOLD-1", "An issue the folds count")
	fillCounts(t, database)
	drawn := []float64{0, 0.25, 0.5, 0.75, 0.999}
	setForTest(t, &coverageFoldRandom, fractions(drawn...))
	start := time.Date(2026, 11, 24, 0, 0, 0, 0, time.UTC)
	clock := &fakeCountsClock{now: start}
	statements := &clockedStatements{clock: clock}
	traced := &store.Store{Pool: storetest.Traced(t, database.Pool, statements, nil)}
	clock.onWait = func(int) bool { return len(statements.of(foldDeltas)) >= 6 }
	runCountsUntilStopped(t, traced, clock, 2*time.Minute)

	folds := statements.of(foldDeltas)
	if len(folds) < 6 {
		t.Fatalf("the loop folded %d times, want 6", len(folds))
	}
	gap := func(k int) time.Duration {
		return 10*time.Second + time.Duration(drawn[k%len(drawn)]*float64(time.Second))
	}
	if first := folds[0].Sub(start); first != gap(0) {
		t.Errorf("the first fold started %v after the first run, want %v", first, gap(0))
	}
	for k := 1; k < len(folds); k++ {
		if got := folds[k].Sub(folds[k-1]); got != gap(k) {
			t.Errorf("fold %d started %v after the one before, want %v", k+1, got, gap(k))
		}
	}
}

// TestAFailedFoldLogsAndTheNextOneCatchesUp holds R7i: a fold made to fail - the component's base
// row refuses the sum the fold adds to it - logs an ERROR line; the fold of the next period runs
// and succeeds, folding every delta row, and the counts then equal main's statement's.
func TestAFailedFoldLogsAndTheNextOneCatchesUp(t *testing.T) {
	database := storetest.Open(t)
	seedEveryCoverageState(t, database)
	seedComponentsArchitecture(t, database.Pool, "FAIL", 905)
	fillCounts(t, database)
	clock := &fakeCountsClock{now: time.Date(2026, 11, 24, 0, 0, 0, 0, time.UTC)}
	statements := &clockedStatements{clock: clock}
	traced := &store.Store{Pool: storetest.Traced(t, database.Pool, statements, nil)}
	records := &logtest.Recorder{}
	logtest.SetDefault(t, records)
	clock.onWait = func(index int) bool {
		switch index {
		case 1: // before the first fold; the loop's first run, a recount, is done
			insertComponent(t, database.Pool, "FAIL", "folded", 905)
			mustExec(t, database.Pool, `alter table search_coverage_counts add constraint fold_refused check (kind <> 'component') not valid`)
		case 2: // before the second fold
			if failed := passRecords(records, slog.LevelError, foldFailedLine); len(failed) != 1 {
				t.Errorf("the first fold logged %d ERROR lines, want 1: %v", len(failed), records.Records())
			}
			mustExec(t, database.Pool, `alter table search_coverage_counts drop constraint fold_refused`)
		}
		return index == 3
	}
	runCountsUntilStopped(t, traced, clock, 2*time.Minute)

	if folds := statements.of(foldDeltas); len(folds) != 2 {
		t.Fatalf("the loop sent %d folds, want the failed one and the next", len(folds))
	}
	if failed := passRecords(records, slog.LevelError, foldFailedLine); len(failed) != 1 {
		t.Errorf("the loop logged %d failed folds, want 1", len(failed))
	}
	var unfolded int
	if err := database.Pool.QueryRow(context.Background(), `select count(*) from search_coverage_deltas`).Scan(&unfolded); err != nil || unfolded != 0 {
		t.Errorf("after the next fold %d delta rows wait (%v), want the fold to have caught up", unfolded, err)
	}
	requireMainsAnswer(t, database, "after the fold that caught up")
}

// requireBackoffWaits fails the test unless each of waits is the injected fraction (drawn, in turn)
// of its bound: the first bound 10 s, each later one twice the one before, none past 10 minutes.
func requireBackoffWaits(t *testing.T, waits []time.Duration, drawn []float64) {
	t.Helper()
	bound := 10 * time.Second
	for k, wait := range waits {
		if bound > 10*time.Minute {
			t.Fatalf("wait %d: the bound passed 10 minutes", k+1)
		}
		if want := time.Duration(drawn[k%len(drawn)] * float64(bound)); wait != want {
			t.Errorf("wait %d is %v, want %v of its bound %v, %v", k+1, wait, drawn[k%len(drawn)], bound, want)
		}
		bound = min(2*bound, 10*time.Minute)
	}
}

// TestTheFillBacksOffExponentiallyWithFullJitter holds R7o: with the backoff's random source
// injected and every fill made to fail before its first statement (FailCoverageFillsForTest), the
// loop logs each failure at ERROR and waits after it the injected fraction of its bound, the first
// bound 10 s and each later one twice the one before up to 10 minutes, never past them; no fold
// runs while no fill has ended.
func TestTheFillBacksOffExponentiallyWithFullJitter(t *testing.T) {
	database := storetest.Open(t)
	FailCoverageFillsForTest(1000)
	t.Cleanup(func() { FailCoverageFillsForTest(0) })
	drawn := []float64{0.5, 0.1, 0.999999, 0.75, 1e-9, 0.3}
	setForTest(t, &coverageBackoffRandom, fractions(drawn...))
	clock := &fakeCountsClock{now: time.Date(2026, 11, 24, 0, 0, 0, 0, time.UTC)}
	statements := &clockedStatements{clock: clock}
	traced := &store.Store{Pool: storetest.Traced(t, database.Pool, statements, nil)}
	records := &logtest.Recorder{}
	logtest.SetDefault(t, records)
	clock.onWait = func(index int) bool { return index == 12 }
	runCountsUntilStopped(t, traced, clock, time.Minute)

	if len(clock.waits) != 13 {
		t.Fatalf("the loop waited %d times, want 13", len(clock.waits))
	}
	requireBackoffWaits(t, clock.waits, drawn)
	failed := passRecords(records, slog.LevelError, fillFailedLine)
	if len(failed) != len(clock.waits) {
		t.Errorf("the loop logged %d failed fills, want one before each of its %d waits", len(failed), len(clock.waits))
	}
	for _, record := range failed {
		if logged, _ := attrsOf(record)["error"].Any().(error); !errors.Is(logged, errFillMadeToFail) {
			t.Errorf("a failed fill's line logs %v, want the fill made to fail", logged)
		}
	}
	if folds := statements.of(foldDeltas); len(folds) != 0 {
		t.Errorf("the loop folded %d times while no fill had ended", len(folds))
	}
}

// TestAFailedRecountBacksOffAsTheFillDoes holds R7x: with the backoff's random source injected and
// the recount made to fail three times in a row (its deadline, coverageRecountTimeout, past before
// its first statement), each failure is logged at ERROR, and the waits from each failure to the
// recount that tries again, read in the loop's own time, are the injected fractions of 10 s, 20 s
// and 40 s; the folds go on meanwhile. The recount that then ends puts the next one back on
// coverageRecountInterval.
func TestAFailedRecountBacksOffAsTheFillDoes(t *testing.T) {
	database := storetest.Open(t)
	seedIssue(t, database, "BACK", "BACK-1", "An issue the recounts count")
	fillCounts(t, database)
	drawn := []float64{0.5, 0.25, 0.75}
	setForTest(t, &coverageBackoffRandom, fractions(drawn...))
	setForTest(t, &coverageRecountInterval, 30*time.Second)
	setForTest(t, &coverageRecountJitter, 0)
	setForTest(t, &coverageRecountTimeout, time.Nanosecond)
	clock := &fakeCountsClock{now: time.Date(2026, 11, 24, 0, 0, 0, 0, time.UTC)}
	statements := &clockedStatements{clock: clock}
	traced := &store.Store{Pool: storetest.Traced(t, database.Pool, statements, nil)}
	log := &clockedLog{clock: clock}
	logtest.SetDefault(t, log)
	clock.onWait = func(int) bool {
		if len(log.logged(recountFailedLine)) == 3 {
			coverageRecountTimeout = 2 * time.Minute
		}
		return len(log.logged(recountStartedLine)) == 5
	}
	runCountsUntilStopped(t, traced, clock, 2*time.Minute)

	started, failed := log.logged(recountStartedLine), log.logged(recountFailedLine)
	if len(started) != 5 || len(failed) != 3 || len(log.logged(recountedLine)) < 1 {
		t.Fatalf("the loop started %d recounts, %d of them failed and %d ended, want 5 of which the first 3 failed",
			len(started), len(failed), len(log.logged(recountedLine)))
	}
	var waits []time.Duration
	for k := range 3 {
		if failed[k].level != slog.LevelError {
			t.Errorf("failure %d is logged at %v, want ERROR", k+1, failed[k].level)
		}
		waits = append(waits, started[k+1].at.Sub(failed[k].at))
	}
	requireBackoffWaits(t, waits, drawn)
	if next := started[4].at.Sub(started[3].at); next != 30*time.Second {
		t.Errorf("the recount after the one that ended started %v after it, want coverageRecountInterval, 30s", next)
	}
	if folds := statements.of(foldDeltas); len(folds) == 0 {
		t.Error("the loop folded nothing while the recount backed off")
	}
}
