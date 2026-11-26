package store

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/testwait"
)

// beforeRefsDecisionsForms is the schema the seed's test stores its text at: the last migration
// before 20261126040000_refs_decisions_forms_owed, which the queue's table, its relay, its four
// insert triggers and its seed follow. lastRefsDecisionsFormsTrigger is the last of the triggers
// (20261126040500) that the seed runs after.
const (
	beforeRefsDecisionsForms      = 20261125005337
	lastRefsDecisionsFormsTrigger = 20261126040500
)

// readOwedIn is every row of queue, a refs_*_owed table, with the enqueued_at it holds.
func readOwedIn(t *testing.T, store *Store, queue string) map[owedSource]time.Time {
	t.Helper()
	rows, err := store.Pool.Query(context.Background(), `select kind, id::text, enqueued_at from `+queue)
	if err != nil {
		t.Fatalf("read %s: %v", queue, err)
	}
	defer rows.Close()
	owed := map[owedSource]time.Time{}
	for rows.Next() {
		var source owedSource
		var at time.Time
		if err := rows.Scan(&source.kind, &source.id, &at); err != nil {
			t.Fatalf("scan %s: %v", queue, err)
		}
		owed[source] = at
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate %s: %v", queue, err)
	}
	return owed
}

// assertOwedIn fails unless queue holds exactly the sources in want, and returns what it holds.
func assertOwedIn(t *testing.T, store *Store, queue string, want ...owedSource) map[owedSource]time.Time {
	t.Helper()
	owed := readOwedIn(t, store, queue)
	got := map[owedSource]bool{}
	for source := range owed {
		got[source] = true
	}
	wanted := map[owedSource]bool{}
	for _, source := range want {
		wanted[source] = true
	}
	if !reflect.DeepEqual(got, wanted) {
		t.Fatalf("%s holds %v, want %v", queue, got, wanted)
	}
	return owed
}

// Text stored before this release was indexed by no build that reads an issue's Decisions page as
// the issue, so the seed owes the bootstrap queue every comment, every ask, every issue message and
// every document with a markdown version, with no edit and no command run. An image's version and
// a direct message, which no mention is indexed from, are owed nothing, no `scope` row is written
// (the reading changes no `#id`), and the live queue, which only the relay and the insert triggers
// write, holds nothing the seed wrote.
func TestTheRefsDecisionsFormsSeedOwesTextStoredBeforeItInItsOwnQueue(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	migrateThrough(t, store, beforeRefsDecisionsForms)
	seedCoreWithADefaultAssignee(t, store)
	fixture := writeRefsGrammarFixture(t, store)

	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	assertOwedIn(t, store, "refs_decisions_forms_seed_owed",
		owedSource{"artifact", fixture.document},
		owedSource{"ask", fixture.ask},
		owedSource{"comment", fixture.comment},
		owedSource{"message", fixture.message})
	assertOwedIn(t, store, "refs_decisions_forms_owed")
}

// The relay is live before the seed runs, and the seed writes no key of the live queue, so a row a
// settle of an older build relayed between them stays as that settle gave it, and its source, which
// the seed read, is in both queues. A complete index of the source clears each
// (refs.queueSettleSourceOwed).
func TestTheRefsDecisionsFormsSeedLeavesWhatTheRelayAlreadyOwed(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	migrateThrough(t, store, lastRefsDecisionsFormsTrigger)
	seedCoreWithADefaultAssignee(t, store)
	fixture := writeRefsGrammarFixture(t, store)
	if _, err := store.Pool.Exec(ctx, `delete from refs_forms_owed where kind = 'comment' and id = $1::uuid`, fixture.comment); err != nil {
		t.Fatalf("settle the comment's row as an older build does: %v", err)
	}
	owed := assertOwedIn(t, store, "refs_decisions_forms_owed", owedSource{"comment", fixture.comment})
	enqueued := owed[owedSource{"comment", fixture.comment}]

	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	after := assertOwedIn(t, store, "refs_decisions_forms_owed", owedSource{"comment", fixture.comment})
	if at := after[owedSource{"comment", fixture.comment}]; !at.Equal(enqueued) {
		t.Fatalf("the seed moved the relayed comment's enqueued_at to %v, want the relay's %v", at, enqueued)
	}
	assertOwedIn(t, store, "refs_decisions_forms_seed_owed",
		owedSource{"artifact", fixture.document},
		owedSource{"ask", fixture.ask},
		owedSource{"comment", fixture.comment},
		owedSource{"message", fixture.message})
}

// While the seed's transaction holds a row for every stored text uncommitted, a writer of the
// release before, whose settle the relay writes a row of the live queue for and whose insert of the
// retired document's edge a trigger owes a row of it for, waits for no one: the seed writes only
// the bootstrap queue, and no writer inserts into that. The writes commit while the seed is open,
// the live queue shows them and the bootstrap queue shows nothing, and once the seed commits the
// bootstrap queue holds every text and the live queue the two relayed rows still. A seed into the
// live queue made these two writes wait for its commit and left a cycle of 40P01 where a writer
// relayed two sources in the reverse of the seed's order.
func TestAWriteOfTheReleaseBeforeNeverWaitsForTheSeed(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	migrateThrough(t, store, lastRefsDecisionsFormsTrigger)
	seedCoreWithADefaultAssignee(t, store)
	fixture := writeRefsGrammarFixture(t, store)
	seedSQL, err := migrationFiles.ReadFile("migrations/20261126040600_refs_decisions_forms_seed.up.sql")
	if err != nil {
		t.Fatalf("read the seed: %v", err)
	}
	seed, err := store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the seed's transaction: %v", err)
	}
	defer seed.Rollback(ctx)
	if _, err := seed.Exec(ctx, string(seedSQL)); err != nil {
		t.Fatalf("run the seed without committing it: %v", err)
	}

	writes := make(chan error, 2)
	go func() {
		_, err := store.Pool.Exec(ctx, `delete from refs_forms_owed where kind = 'comment' and id = $1::uuid`, fixture.comment)
		writes <- err
	}()
	go func() {
		_, err := store.Pool.Exec(ctx, `
			insert into block_refs (artifact_id, block_id, from_node, to_kind, to_id, created_version)
			values ($1::uuid, 'b1', 'node#b1', 'artifact', 'CORE-1/decisions-md', 1)
		`, fixture.document)
		writes <- err
	}()
	for range 2 {
		if err := testwait.Receive[error](t, "a write of the release before to commit while the seed's transaction is open", writes); err != nil {
			t.Fatalf("a write of the release before failed while the seed was open: %v", err)
		}
	}
	live := owedSource{"comment", fixture.comment}
	relayed := assertOwedIn(t, store, "refs_decisions_forms_owed", live, owedSource{"artifact", fixture.document})
	assertOwedIn(t, store, "refs_decisions_forms_seed_owed")

	if err := seed.Commit(ctx); err != nil {
		t.Fatalf("commit the seed: %v", err)
	}

	assertOwedIn(t, store, "refs_decisions_forms_seed_owed",
		owedSource{"artifact", fixture.document},
		owedSource{"ask", fixture.ask},
		owedSource{"comment", fixture.comment},
		owedSource{"message", fixture.message})
	after := assertOwedIn(t, store, "refs_decisions_forms_owed", live, owedSource{"artifact", fixture.document})
	for source, at := range relayed {
		if !after[source].Equal(at) {
			t.Fatalf("the seed's commit moved the live row of %v from %v to %v", source, at, after[source])
		}
	}
}

// No trigger of this queue is created after its seed: a trigger migration created after it would
// leave a write between the seed's read and the trigger's commit unowed. Four insert triggers and
// the relay are the triggers there are.
func TestTheRefsDecisionsFormsSeedRunsAfterEveryTriggerOfTheQueue(t *testing.T) {
	var seed int
	triggers := map[string]int{}
	for _, migration := range shippedMigrations(t) {
		switch {
		case strings.HasSuffix(migration.Name, "_refs_decisions_forms_seed.up.sql"):
			seed = migration.Version
		case strings.Contains(migration.Name, "decisions_forms_insert_trigger"),
			strings.HasSuffix(migration.Name, "_refs_forms_decisions_relay_trigger.up.sql"):
			triggers[migration.Name] = migration.Version
		}
	}
	if seed == 0 || len(triggers) != 5 {
		t.Fatalf("the shipped set holds the seed at %d and the triggers %v, want the seed and the relay with each of the four insert triggers", seed, triggers)
	}
	for name, version := range triggers {
		if version >= seed {
			t.Errorf("%s (%d) is not before the seed (%d): the seed must run after every trigger, as the forms queue's does", name, version, seed)
		}
	}
}

// Every delete from refs_forms_owed relays what it removes into refs_decisions_forms_owed, whoever
// sends it: the release before this one settles a source's row of the first queue as soon as it
// has indexed the text with the grammar it knows, and the relay is what keeps the debt. Only a
// source that still exists is relayed, an issue message needs its issue, a delete that removes
// nothing relays nothing, and a row already owed keeps its enqueued_at.
func TestTheRefsFormsDeleteRelaysToTheDecisionsQueue(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	seedCoreWithADefaultAssignee(t, store)
	fixture := writeRefsGrammarFixture(t, store)
	write := func(what, query string, args ...any) {
		t.Helper()
		if _, err := store.Pool.Exec(ctx, query, args...); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	const queue = "refs_decisions_forms_owed"
	comment := owedSource{"comment", fixture.comment}
	assertOwedIn(t, store, queue)

	write("delete nothing", `delete from refs_forms_owed where kind = 'comment' and id = $1::uuid and enqueued_at < '2000-01-01'`, fixture.comment)
	assertOwedIn(t, store, queue)

	// The settle an older build makes: the one row, by kind and id, in a statement of its own.
	write("settle the comment as an older build does", `
		with grammar as (delete from refs_grammar_owed where kind = 'comment' and id = $1::uuid)
		delete from refs_forms_owed where kind = 'comment' and id = $1::uuid
	`, fixture.comment)
	owed := assertOwedIn(t, store, queue, comment)

	// A row already owed keeps the enqueued_at it holds.
	write("owe the comment again", `insert into refs_forms_owed (kind, id) values ('comment', $1::uuid)`, fixture.comment)
	write("settle it again", `delete from refs_forms_owed where kind = 'comment' and id = $1::uuid`, fixture.comment)
	if at := readOwedIn(t, store, queue)[comment]; !at.Equal(owed[comment]) {
		t.Fatalf("a second settle moved the relayed comment's enqueued_at to %v, want %v", at, owed[comment])
	}

	// A source that is gone, an issue message with no issue, and a row a drain settles for a source
	// that is gone: none is relayed.
	write("drain the queue", `delete from refs_decisions_forms_owed`)
	var gone string
	if err := store.Pool.QueryRow(ctx, `select gen_random_uuid()::text`).Scan(&gone); err != nil {
		t.Fatalf("draw a uuid: %v", err)
	}
	write("owe the gone sources and the direct message", `
		insert into refs_forms_owed (kind, id) values
			('artifact', $1::uuid), ('ask', $1::uuid), ('comment', $1::uuid), ('message', $1::uuid), ('message', $2::uuid)
	`, gone, fixture.direct)
	write("settle them", `delete from refs_forms_owed where id in ($1::uuid, $2::uuid)`, gone, fixture.direct)
	assertOwedIn(t, store, queue)

	// One statement that deletes every source's row relays each once: the four texts, and not the
	// image, which no trigger owes.
	write("owe every source", `
		insert into refs_forms_owed (kind, id) values
			('artifact', $1::uuid), ('ask', $2::uuid), ('comment', $3::uuid), ('message', $4::uuid)
		on conflict (kind, id) do nothing
	`, fixture.document, fixture.ask, fixture.comment, fixture.message)
	write("settle every row", `delete from refs_forms_owed`)
	assertOwedIn(t, store, queue,
		owedSource{"artifact", fixture.document},
		owedSource{"ask", fixture.ask},
		owedSource{"comment", fixture.comment},
		owedSource{"message", fixture.message})
}

// The relay fires at the end of the statement that deletes the row it relays, so the delete of
// the relayed row that clears it is a statement of its own after that one. Beside it, as a
// sibling data-modifying common table expression of the same statement, the clear runs before
// the relay and the relay then writes the row again, for good.
func TestAClearOfTheRelayedRowBesideItsDeleteLeavesTheRowOwed(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	seedCoreWithADefaultAssignee(t, store)
	fixture := writeRefsGrammarFixture(t, store)
	comment := owedSource{"comment", fixture.comment}
	const queue = "refs_decisions_forms_owed"
	write := func(what, query string) {
		t.Helper()
		if _, err := store.Pool.Exec(ctx, query, fixture.comment); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	write("settle the comment's row", `delete from refs_forms_owed where kind = 'comment' and id = $1::uuid`)
	assertOwedIn(t, store, queue, comment)
	write("owe the comment again", `insert into refs_forms_owed (kind, id) values ('comment', $1::uuid)`)

	write("settle it with the clear beside the delete", `
		with cleared as (delete from refs_decisions_forms_owed where kind = 'comment' and id = $1::uuid)
		delete from refs_forms_owed where kind = 'comment' and id = $1::uuid
	`)
	assertOwedIn(t, store, queue, comment)

	write("owe the comment again", `insert into refs_forms_owed (kind, id) values ('comment', $1::uuid)`)
	write("settle it", `delete from refs_forms_owed where kind = 'comment' and id = $1::uuid`)
	write("clear the row in a statement of its own", `delete from refs_decisions_forms_owed where kind = 'comment' and id = $1::uuid`)
	assertOwedIn(t, store, queue)
}

// A block table's insert of an edge to the retired decisions document owes the owner of the block,
// whether or not anything was deleted: the release before this one reads the Decisions page's link
// as that document, and a block that also names the issue loses nothing while it adds the
// document's edge. Only that target is read, a row an upsert finds present is not an insert, one
// statement owes each owner once, an issue message is owed and a direct message is not, and a
// row already owed keeps its enqueued_at.
func TestABlockTablesInsertOfTheRetiredDocumentsEdgeOwesItsOwner(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	seedCoreWithADefaultAssignee(t, store)
	fixture := writeRefsGrammarFixture(t, store)
	const queue = "refs_decisions_forms_owed"
	write := func(what, query string, args ...any) {
		t.Helper()
		if _, err := store.Pool.Exec(ctx, query, args...); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	// The forms queue and the relay's rows are not what this test reads: the writes owe the forms
	// queue through their own triggers, and settling them relays them.
	write("drain the queues", `delete from refs_forms_owed; delete from refs_decisions_forms_owed`)
	insert := func(table, column string) string {
		extraColumn, extraValue := "", ""
		if table == "block_refs" {
			extraColumn, extraValue = ", created_version", ", 1"
		}
		return fmt.Sprintf(`
			insert into %s (%s, block_id, from_node, to_kind, to_id%s)
			values ($1::uuid, 'b1', 'node#b1', $2, $3%s)
			on conflict do nothing`, table, column, extraColumn, extraValue)
	}

	for _, owner := range []struct {
		table, column, kind, id string
	}{
		{"block_refs", "artifact_id", "artifact", fixture.document},
		{"comment_block_refs", "comment_id", "comment", fixture.comment},
		{"message_block_refs", "message_id", "message", fixture.message},
		{"ask_block_refs", "ask_id", "ask", fixture.ask},
	} {
		source := owedSource{owner.kind, owner.id}
		query := insert(owner.table, owner.column)

		// The issue's own edge, an artifact that is not the retired document, a block and a
		// document whose key only contains the slug owe nothing.
		for _, target := range [][2]string{{"issue", "CORE-1"}, {"artifact", "CORE-1/spec"}, {"block", "CORE-1/decisions-md#b1"}, {"artifact", "CORE-1/decisions-md-notes"}} {
			write(owner.table+" "+target[0]+" "+target[1], query, owner.id, target[0], target[1])
		}
		assertOwedIn(t, store, queue)

		// The retired document's edge owes the owner.
		write(owner.table+" retired document", query, owner.id, "artifact", "CORE-1/decisions-md")
		assertOwedIn(t, store, queue, source)

		// An insert that finds the row already there inserts nothing and owes nothing.
		write("clear the row", `delete from refs_decisions_forms_owed`)
		write(owner.table+" retired document again", query, owner.id, "artifact", "CORE-1/decisions-md")
		assertOwedIn(t, store, queue)

		// A project's document and an agent's, whose keys end in the same slug, owe too, and a row
		// already owed keeps the enqueued_at it holds.
		write(owner.table+" a project's retired document", query, owner.id, "artifact", "CORE/decisions-md")
		owed := assertOwedIn(t, store, queue, source)
		write(owner.table+" an agent's retired document", query, owner.id, "artifact", "agent/s1/decisions-md")
		if at := readOwedIn(t, store, queue)[source]; !at.Equal(owed[source]) {
			t.Fatalf("%s: a second insert moved the owed row's enqueued_at to %v, want %v", owner.table, at, owed[source])
		}
		write("clear the row", `delete from refs_decisions_forms_owed`)
	}

	// One statement inserting the retired document's edge in two blocks owes the document once, and
	// the same statement's other rows owe nothing.
	write("two blocks in one statement", `
		insert into block_refs (artifact_id, block_id, from_node, to_kind, to_id, created_version) values
			($1::uuid, 'b2', 'node#b2', 'artifact', 'CORE-1/decisions-md', 1),
			($1::uuid, 'b3', 'node#b3', 'artifact', 'CORE-1/decisions-md', 1),
			($1::uuid, 'b3', 'node#b3', 'issue', 'CORE-1', 1)
	`, fixture.document)
	assertOwedIn(t, store, queue, owedSource{"artifact", fixture.document})
	write("clear the row", `delete from refs_decisions_forms_owed`)

	// An upsert that does update a row owes nothing: the row was already there.
	write("upsert", `
		insert into block_refs (artifact_id, block_id, from_node, to_kind, to_id, created_version, to_types)
		values ($1::uuid, 'b2', 'node#b2', 'artifact', 'CORE-1/decisions-md', 1, array['spec'])
		on conflict (artifact_id, block_id, to_kind, to_id, kind) do update set to_types = excluded.to_types
	`, fixture.document)
	assertOwedIn(t, store, queue)

	// A direct message holds no block edge any build writes, and is owed nothing if one is written.
	write("a direct message's edge", insert("message_block_refs", "message_id"), fixture.direct, "artifact", "CORE-1/decisions-md")
	assertOwedIn(t, store, queue)
}

// The drain's reads, exactly as refs.ListOwedAfter runs them on refs.RefsDecisionsFormsOwed and on
// refs.RefsDecisionsFormsSeedOwed, go through each queue's own index in the order the index holds,
// as the forms queue's do (TestTheRefsFormsDrainReadsItsIndexInOrder): no sort, and the read past
// a cursor bounds that index scan with the cursor.
func TestTheRefsDecisionsFormsDrainReadsItsIndexesInOrder(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	for _, queue := range []string{"refs_decisions_forms_owed", "refs_decisions_forms_seed_owed"} {
		index := queue + "_enqueued"
		read := `select o.kind, o.id::text, o.enqueued_at from ` + queue + ` o order by o.enqueued_at, o.kind, o.id limit 200`
		plan := explainWithoutSeqScan(t, store, read)
		if !strings.Contains(plan, index) || strings.Contains(plan, "Sort") {
			t.Errorf("the owed read's plan does not read %s in order:\n%s", index, plan)
		}
		past := `select o.kind, o.id::text, o.enqueued_at from ` + queue + ` o
			where (o.enqueued_at, o.kind, o.id) > ('2026-01-01T00:00:00Z'::timestamptz, 'comment', '00000000-0000-4000-8000-000000000000'::uuid)
			order by o.enqueued_at, o.kind, o.id limit 200`
		plan = explainWithoutSeqScan(t, store, past)
		if !strings.Contains(plan, index) || strings.Contains(plan, "Sort") ||
			!strings.Contains(plan, "Index Cond") || strings.Contains(plan, "Filter") {
			t.Errorf("the read past a cursor does not start at the cursor in %s:\n%s", index, plan)
		}
	}
}
