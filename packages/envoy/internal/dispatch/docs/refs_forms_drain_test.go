package docs

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// formsOwedRows is every row of the forms queue as "kind id", with the enqueued_at each holds.
func formsOwedRows(t *testing.T, database *store.Store) map[string]time.Time {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `select kind || ' ' || id::text, enqueued_at from refs_forms_owed`)
	if err != nil {
		t.Fatalf("read refs_forms_owed: %v", err)
	}
	defer rows.Close()
	owed := map[string]time.Time{}
	for rows.Next() {
		var row string
		var at time.Time
		if err := rows.Scan(&row, &at); err != nil {
			t.Fatalf("scan refs_forms_owed: %v", err)
		}
		owed[row] = at
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate refs_forms_owed: %v", err)
	}
	return owed
}

// reconcileRefsForms runs one pass of a forms drain from the start of the queue, as a loop that
// has found no document live ahead before runs its first.
func reconcileRefsForms(t *testing.T, service *Service) RefsFormsTick {
	t.Helper()
	tick, err := service.ReconcileRefsForms(context.Background(), refs.RefsFormsOwed, refs.Owed{}, newFormsLiveAhead())
	if err != nil {
		t.Fatalf("forms drain pass: %v", err)
	}
	return tick
}

// citesIssue reports whether the source holds an edge to the issue key.
func citesIssue(t *testing.T, database *store.Store, fromKind, fromID, key string) bool {
	t.Helper()
	return slices.ContainsFunc(refsFrom(t, database, fromKind, fromID), func(ref refRow) bool {
		return ref.ToKind == "issue" && ref.ToID == key
	})
}

// createIndexedNotes writes document notes of CORE-1 as a build that predates the forms writes and
// indexes it (createIndexedDocument).
func createIndexedNotes(t *testing.T, database *store.Store, service *Service, markdown string) string {
	t.Helper()
	return createIndexedDocument(t, database, service, "notes", markdown)
}

// createIndexedDocument writes document slug of CORE-1 as a build that predates the forms writes
// and indexes it: the live text the server serves, version 1 holding that text, its settlement done
// and its block index recorded at version 1, with no edge of the tab address it writes, since that
// build's grammar reads none. Its version's own trigger owes it to the forms queue.
func createIndexedDocument(t *testing.T, database *store.Store, service *Service, slug, markdown string) string {
	t.Helper()
	ctx := context.Background()
	var document string
	if err := database.Pool.QueryRow(ctx, `
		insert into artifacts (issue_key, project_key, slug, name, kind, created_by)
		values ('CORE-1', 'CORE', $1, $1 || '.md', 'doc', '{"kind":"user","id":"alice"}')
		returning id::text
	`, slug).Scan(&document); err != nil {
		t.Fatalf("create %s: %v", slug, err)
	}
	seedServiceText(t, service, document, markdown)
	live, _, err := service.TextWithBlocks(ctx, document)
	if err != nil {
		t.Fatalf("read %s: %v", slug, err)
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into artifact_versions (artifact_id, number, markdown, created_at, authors)
		values ($1, 1, $2, $3, '[{"kind":"user","id":"alice"}]')
	`, document, live, refsGrammarWritten); err != nil {
		t.Fatalf("write version 1 of %s: %v", slug, err)
	}
	settle(t, database, document)
	if _, err := database.Pool.Exec(ctx, `update document_block_versions set version = 1 where artifact_id = $1`, document); err != nil {
		t.Fatalf("index the version as the earlier release does: %v", err)
	}
	return document
}

// Every text a build that predates an issue's tab addresses and a document's dashboard URLs
// stored is re-indexed by one pass of the forms drain, with no edit and no command run: a comment,
// an ask's question and an issue message through refs.ReindexOwed, and a document indexed by the
// drain itself at its latest version, its own edge and its block's mention both, each settled in
// the transaction that indexed it. The queue is empty after it.
func TestTheFormsDrainIndexesTextAnOlderBuildStored(t *testing.T) {
	database, service := blockIndexService(t)
	ctx := context.Background()
	comment := createRawComment(t, database, "The plan: dispatch://CORE-2/plan.")
	message := createRawMessage(t, database, "The verification tab: https://dispatch.example/issues/CORE-2/verification.")
	var ask string
	if err := database.Pool.QueryRow(ctx, `
		insert into asks (issue_key, author, question, created_at)
		values ('CORE-1', '{"kind":"session","id":"s1"}', 'Ship after dispatch://CORE-2/children?', $1)
		returning id::text
	`, refsGrammarWritten).Scan(&ask); err != nil {
		t.Fatalf("write the ask: %v", err)
	}
	notes := createIndexedNotes(t, database, service, "The plan: dispatch://CORE-2/plan.\n")
	if owed := formsOwedRows(t, database); len(owed) != 4 {
		t.Fatalf("the forms queue holds %v after writes an older build makes, want the four sources", owed)
	}

	tick := reconcileRefsForms(t, service)

	if tick.Texts != 3 || tick.Documents != 1 || tick.OwedTexts != 0 || tick.LiveAhead != 0 || tick.Unindexed != 0 {
		t.Fatalf("pass = %#v, want three texts and the document indexed and nothing left owed", tick)
	}
	if owed := formsOwedRows(t, database); len(owed) != 0 {
		t.Fatalf("the forms queue holds %v after the pass, want it empty", owed)
	}
	assertRef(t, database, "comment", comment, "issue", "CORE-2", refsGrammarWritten)
	assertRef(t, database, "message", message, "issue", "CORE-2", refsGrammarWritten)
	assertRef(t, database, "ask", ask, "issue", "CORE-2", refsGrammarWritten)
	assertRef(t, database, "artifact", notes, "issue", "CORE-2", refsGrammarWritten)
	holder := holderOf(t, service, notes, "dispatch://CORE-2/plan")
	if !slices.ContainsFunc(blockPairsOf(t, database, notes), func(pair blockPair) bool {
		return pair.BlockID == holder && pair.ToKind == "issue" && pair.ToID == "CORE-2"
	}) {
		t.Fatalf("the notes' blocks hold %#v, want block %s citing CORE-2", blockPairsOf(t, database, notes), holder)
	}
	if at := blockIndexAt(t, database, notes); at == nil || *at != 1 {
		t.Fatalf("the notes' block index is at %v after the drain indexed them, want version 1", at)
	}
}

// A document's row is settled only by this drain's own index of it. The drain owes the document a
// block index and indexes it in the same pass, and any build's block-index pass may take a
// document owed one in between: here another process of this build's protocol takes it under its
// owner row and records it indexed at its version, clearing the mark the drain's debt wrote (as
// refs.RevisionFenceStatement does for a pass that wins), with no edge of the tab address. The
// drain then finds the document no longer owed and leaves its row owed, with the enqueued_at it
// listed; the next pass indexes it and settles the row.
func TestTheFormsDrainLeavesADocumentAnotherProcessIndexedFirstOwed(t *testing.T) {
	database, service := blockIndexService(t)
	notes := createIndexedNotes(t, database, service, "The plan: dispatch://CORE-2/plan.\n")
	listed := formsOwedRows(t, database)
	if len(listed) != 1 {
		t.Fatalf("the forms queue holds %v, want the notes alone", listed)
	}

	// The other process speaks this build's protocol: its index clears the mark beside the
	// version, as the fence's own statement does.
	tick := raceAnotherBlockIndex(t, database, service, notes,
		refs.RevisionFenceStatement, notes, 1, refs.BlockIndexMarkingRule)

	if tick.Documents != 0 || tick.Unindexed != 1 || tick.OwedTexts != 1 {
		t.Fatalf("pass = %#v, want the notes left owed, unindexed by this drain", tick)
	}
	owed := formsOwedRows(t, database)
	if len(owed) != 1 || !owed["artifact "+notes].Equal(listed["artifact "+notes]) {
		t.Fatalf("the forms queue holds %v after another process indexed the notes first, want their row as listed (%v)", owed, listed)
	}
	if citesIssue(t, database, "artifact", notes, "CORE-2") {
		t.Fatalf("the notes cite CORE-2 though no index of this build ran: %#v", refsFrom(t, database, "artifact", notes))
	}

	next := reconcileRefsForms(t, service)

	if next.Documents != 1 || next.OwedTexts != 0 {
		t.Fatalf("the next pass = %#v, want the notes indexed and settled", next)
	}
	assertRef(t, database, "artifact", notes, "issue", "CORE-2", refsGrammarWritten)
}

// An older build's index cannot settle the debt the drain owes a document. The other process here
// predates the block index's mark (#3178): under the owner row it records the document indexed at
// its version and leaves the mark the drain's debt wrote, store_owed_at at this build's rule, with
// no edge of the tab address, which that build's grammar does not read. The row still reads owed
// at this build's rule (refs.OwedMarkListed, refs/block_stand.go), so the drain indexes the
// document itself, which the revision fence admits at that version for the mark, writes the edge
// and settles the forms row with its own index.
func TestTheFormsDrainIndexesADocumentAnOlderBuildIndexedFirst(t *testing.T) {
	database, service := blockIndexService(t)
	notes := createIndexedNotes(t, database, service, "The plan: dispatch://CORE-2/plan.\n")
	if listed := formsOwedRows(t, database); len(listed) != 1 {
		t.Fatalf("the forms queue holds %v, want the notes alone", listed)
	}

	// The other process speaks an older build's protocol: its index writes the version alone.
	tick := raceAnotherBlockIndex(t, database, service, notes,
		`update document_block_versions set version = 1 where artifact_id = $1`, notes)

	if tick.Documents != 1 || tick.Unindexed != 0 || tick.OwedTexts != 0 {
		t.Fatalf("pass = %#v, want the notes indexed and settled by this drain", tick)
	}
	if owed := formsOwedRows(t, database); len(owed) != 0 {
		t.Fatalf("the forms queue holds %v after this drain indexed the notes, want none", owed)
	}
	assertRef(t, database, "artifact", notes, "issue", "CORE-2", refsGrammarWritten)
}

// raceAnotherBlockIndex runs a forms drain pass while another process indexes document first. A
// build before the owner lock protocol serialises on the owner's row, so it holds the document's
// owner row, the issue's, for the length of its index. While a process of such a build is in the
// fleet, this build takes that row too, beside its advisory lock (store.LockOwner), so the drain's
// index waits on it. The fleet holds one here: a server_processes row with no
// owner_lock_protocol, as api.registerEarlierBuildProcess writes one. Once the drain has owed the
// document its block index and waits on that row, the other process records its index with
// record and its arguments and commits, and the drain's pass goes on. It answers the drain's tick.
func raceAnotherBlockIndex(t *testing.T, database *store.Store, service *Service, document, record string, arguments ...any) RefsFormsTick {
	t.Helper()
	ctx := context.Background()
	if _, err := database.Pool.Exec(ctx, `
		insert into server_processes (instance, commit, signing_keys)
		values (gen_random_uuid(), 'earlier0build', '{fingerprint}')`); err != nil {
		t.Fatalf("register a process of an earlier build: %v", err)
	}
	other, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the other process's index: %v", err)
	}
	defer other.Rollback(ctx)
	if _, err := other.Exec(ctx, `select 1 from issues where key = 'CORE-1' for no key update`); err != nil {
		t.Fatalf("take the document's owner row as the other process's block index does: %v", err)
	}
	var blocker int32
	if err := other.QueryRow(ctx, `select pg_backend_pid()`).Scan(&blocker); err != nil {
		t.Fatalf("read the other index's backend: %v", err)
	}
	passed := make(chan RefsFormsTick, 1)
	failed := make(chan error, 1)
	go func() {
		tick, err := service.ReconcileRefsForms(ctx, refs.RefsFormsOwed, refs.Owed{}, newFormsLiveAhead())
		if err != nil {
			failed <- err
			return
		}
		passed <- tick
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var owedIndex bool
		var waiting int
		if err := database.Pool.QueryRow(ctx, `
			select (select version is null from document_block_versions where artifact_id = $1),
			       (select count(*) from pg_stat_activity where datname = current_database()
			          and $2 = any(pg_blocking_pids(pid)))
		`, document, blocker).Scan(&owedIndex, &waiting); err != nil {
			t.Fatalf("read the drain's progress: %v", err)
		}
		if owedIndex && waiting > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the drain never owed the document a block index and waited on its owner row (owed %v, waiting %d)", owedIndex, waiting)
		}
		time.Sleep(10 * time.Millisecond)
	}
	written, err := other.Exec(ctx, record, arguments...)
	if err != nil {
		t.Fatalf("record the document indexed as the other process does: %v", err)
	}
	if written.RowsAffected() != 1 {
		t.Fatalf("the other index recorded %d revisions, want one", written.RowsAffected())
	}
	if err := other.Commit(ctx); err != nil {
		t.Fatalf("commit the other process's index: %v", err)
	}
	select {
	case tick := <-passed:
		return tick
	case err := <-failed:
		t.Fatalf("forms drain pass: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("the forms drain pass did not end once the owner row was free")
	}
	return RefsFormsTick{}
}

// A document whose live text is ahead of its latest version, and differs from it, is not indexed
// at that version, which would date the edit's blocks to it: its row stays owed, and the
// settlement that writes its next version indexes it (a version of this build) or owes it again
// (one of an older build).
func TestTheFormsDrainLeavesALiveAheadDocumentOwed(t *testing.T) {
	database, service := blockIndexService(t)
	notes := createOwedDocument(t, database, service, "CORE-1", "", "notes", "The plan: dispatch://CORE-2/plan.\n", "Old text.\n")
	if owed := formsOwedRows(t, database); len(owed) != 1 {
		t.Fatalf("the forms queue holds %v, want the notes alone", owed)
	}

	tick := reconcileRefsForms(t, service)

	if tick.LiveAhead != 1 || tick.Documents != 0 || tick.OwedTexts != 1 {
		t.Fatalf("pass = %#v, want the notes left owed as live ahead", tick)
	}
	if owed := formsOwedRows(t, database); len(owed) != 1 {
		t.Fatalf("the forms queue holds %v, want the notes still owed", owed)
	}
	if citesIssue(t, database, "artifact", notes, "CORE-2") {
		t.Fatalf("the notes cite what only their unsettled live text writes: %#v", refsFrom(t, database, "artifact", notes))
	}
}

// A document that is gone owes no text: its row, which an older build's removal leaves behind when
// its delete of the document's edges owes the forms queue, is settled.
func TestTheFormsDrainSettlesAGoneDocument(t *testing.T) {
	database, service := blockIndexService(t)
	ctx := context.Background()
	var gone string
	if err := database.Pool.QueryRow(ctx, `select gen_random_uuid()::text`).Scan(&gone); err != nil {
		t.Fatalf("draw a uuid: %v", err)
	}
	for _, statement := range []string{
		`insert into refs (from_kind, from_id, to_kind, to_id) values ('artifact', $1, 'issue', 'CORE-2')`,
		`delete from refs where from_kind = 'artifact' and from_id = $1`,
	} {
		if _, err := database.Pool.Exec(ctx, statement, gone); err != nil {
			t.Fatalf("delete a removed document's edges as an older build does: %v", err)
		}
	}
	if owed := formsOwedRows(t, database); len(owed) != 1 {
		t.Fatalf("the forms queue holds %v, want the gone document", owed)
	}

	tick := reconcileRefsForms(t, service)

	if tick.Gone != 1 || tick.OwedTexts != 0 {
		t.Fatalf("pass = %#v, want the gone document's row settled", tick)
	}
}

// The forms drain's own index of a document settles every queue the document is in, after its own
// delete from refs: a document this drain indexes once leaves nothing owed, even where its index
// deletes an edge an older build wrote that the text no longer holds.
func TestTheFormsDrainsIndexSettlesAfterItsOwnDelete(t *testing.T) {
	database, service := blockIndexService(t)
	notes := createIndexedNotes(t, database, service, "The plan: dispatch://CORE-2/plan.\n")
	if _, err := database.Pool.Exec(context.Background(), `
		insert into refs (from_kind, from_id, to_kind, to_id) values ('artifact', $1, 'issue', 'CORE-1')
	`, notes); err != nil {
		t.Fatalf("write an edge the text does not hold: %v", err)
	}

	tick := reconcileRefsForms(t, service)

	if tick.Documents != 1 || tick.OwedTexts != 0 {
		t.Fatalf("pass = %#v, want the notes indexed and nothing owed", tick)
	}
	if citesIssue(t, database, "artifact", notes, "CORE-1") {
		t.Fatalf("the notes keep an edge their text does not hold: %#v", refsFrom(t, database, "artifact", notes))
	}
	if owed := formsOwedRows(t, database); len(owed) != 0 {
		t.Fatalf("the forms queue holds %v after the drain's index deleted an edge, want it empty", owed)
	}
	var grammar int
	if err := database.Pool.QueryRow(context.Background(),
		`select count(*) from refs_grammar_owed where kind = 'artifact' and id = $1::uuid`, notes).Scan(&grammar); err != nil {
		t.Fatalf("count the whole grammar's row: %v", err)
	}
	if grammar != 0 {
		t.Fatalf("the whole grammar's queue still owes the notes after this build indexed them")
	}
}

// A row the drain leaves owed never holds back the rows past it: more documents than one pass
// indexes (blockIndexDocumentBatch), each holding an element this build cannot read
// (ErrDocNewerContent) and so left owed by every pass, were owed before a document the drain can
// index, and that document is indexed by the second pass. Each pass starts past the last row the
// one before it took (refs.ListOwedAfter), so the first ends at the document past its bound and
// the second starts there, and a pass that reaches the end of the queue starts the next walk at its
// start, where the stuck documents are listed, and left owed, again.
func TestTheFormsDrainPassesOverMoreStuckDocumentsThanOnePassTakes(t *testing.T) {
	database, service := blockIndexService(t)
	ctx := context.Background()
	stuck := make([]string, blockIndexDocumentBatch+1)
	for index := range stuck {
		stuck[index] = createIndexedDocument(t, database, service, fmt.Sprintf("stuck-%d", index), "The plan: dispatch://CORE-2/plan.\n")
		if err := service.InjectUnknownElementForTest(ctx, stuck[index]); err != nil {
			t.Fatalf("write an element this build does not know into %s: %v", stuck[index], err)
		}
		if err := service.Evict(ctx, stuck[index]); err != nil {
			t.Fatalf("evict %s: %v", stuck[index], err)
		}
		if _, _, err := service.TextWithBlocks(ctx, stuck[index]); !errors.Is(err, ErrDocNewerContent) {
			t.Fatalf("read %s: %v, want ErrDocNewerContent", stuck[index], err)
		}
	}
	readable := createIndexedDocument(t, database, service, "readable", "The plan: dispatch://CORE-2/plan.\n")
	// The stuck documents were owed first, one second apart, and the readable one after them all.
	owedFrom := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for index, document := range append(slices.Clone(stuck), readable) {
		if _, err := database.Pool.Exec(ctx, `update refs_forms_owed set enqueued_at = $2 where kind = 'artifact' and id = $1::uuid`,
			document, owedFrom.Add(time.Duration(index)*time.Second)); err != nil {
			t.Fatalf("order the owed documents: %v", err)
		}
	}
	liveAhead := newFormsLiveAhead()
	pass := func(after refs.Owed) RefsFormsTick {
		t.Helper()
		tick, err := service.ReconcileRefsForms(ctx, refs.RefsFormsOwed, after, liveAhead)
		if err != nil {
			t.Fatalf("forms drain pass: %v", err)
		}
		return tick
	}

	first := pass(refs.Owed{})
	if !first.Full || first.Documents != 0 || first.Cursor.ID != stuck[blockIndexDocumentBatch-1] {
		t.Fatalf("first pass = %#v, want it to stop after the %d documents it may take, at %s", first, blockIndexDocumentBatch, stuck[blockIndexDocumentBatch-1])
	}
	if citesIssue(t, database, "artifact", readable, "CORE-2") {
		t.Fatalf("the readable document was indexed by a pass that stopped before it")
	}
	second := pass(first.Cursor)
	if second.Documents != 1 || second.Full || second.Cursor != (refs.Owed{}) {
		t.Fatalf("second pass = %#v, want it to start past the first one's last row, index the readable document and reach the end of the queue", second)
	}
	assertRef(t, database, "artifact", readable, "issue", "CORE-2", refsGrammarWritten)
	owed := formsOwedRows(t, database)
	if _, held := owed["artifact "+readable]; held || len(owed) != len(stuck) {
		t.Fatalf("the forms queue holds %v, want the %d stuck documents alone", owed, len(stuck))
	}
	if again := pass(second.Cursor); !again.Full || again.Documents != 0 || again.Cursor.ID != stuck[blockIndexDocumentBatch-1] {
		t.Fatalf("the next walk's first pass = %#v, want it to start over at the stuck documents", again)
	}
}

// The drain remembers the documents it found live ahead, as the block index does: one listed again
// at the same version, with the same pending-settlement mark and no room resident, is counted live
// ahead without being read, so it is not indexed at the version its live text is ahead of even
// once that version holds the live text. A new version has the next pass read it again, index it
// and settle its row, and the memo forgets it.
func TestTheFormsDrainDoesNotRereadALiveAheadDocumentUntilItMoves(t *testing.T) {
	database, service := blockIndexService(t)
	ctx := context.Background()
	notes := createOwedDocument(t, database, service, "CORE-1", "", "notes", "The plan: dispatch://CORE-2/plan.\n", "Old text.\n")
	closeDocumentRoom(t, service, notes)
	liveAhead := newFormsLiveAhead()
	pass := func() RefsFormsTick {
		t.Helper()
		tick, err := service.ReconcileRefsForms(ctx, refs.RefsFormsOwed, refs.Owed{}, liveAhead)
		if err != nil {
			t.Fatalf("forms drain pass: %v", err)
		}
		return tick
	}
	live, _, err := service.TextWithBlocks(ctx, notes)
	if err != nil {
		t.Fatalf("read the notes: %v", err)
	}

	if first := pass(); first.LiveAhead != 1 || first.Documents != 0 || len(liveAhead.documents) != 1 {
		t.Fatalf("first pass = %#v remembering %v, want the notes live ahead and remembered", first, liveAhead.documents)
	}
	// Version 1 now holds the live text, which a read would index.
	if _, err := database.Pool.Exec(ctx, `update artifact_versions set markdown = $2 where artifact_id = $1 and number = 1`, notes, live); err != nil {
		t.Fatalf("store the live text as version 1: %v", err)
	}
	if second := pass(); second.LiveAhead != 1 || second.Documents != 0 || second.OwedTexts != 1 {
		t.Fatalf("second pass = %#v, want the notes counted live ahead unread and still owed", second)
	}
	if citesIssue(t, database, "artifact", notes, "CORE-2") {
		t.Fatalf("the notes were indexed by a pass that remembered them live ahead")
	}

	if _, err := database.Pool.Exec(ctx, `
		insert into artifact_versions (artifact_id, number, markdown, authors)
		values ($1, 2, $2, '[{"kind":"user","id":"alice"}]')
	`, notes, live); err != nil {
		t.Fatalf("write version 2: %v", err)
	}
	if third := pass(); third.Documents != 1 || third.LiveAhead != 0 || third.OwedTexts != 0 {
		t.Fatalf("third pass = %#v, want the notes read again, indexed and settled", third)
	}
	if !citesIssue(t, database, "artifact", notes, "CORE-2") {
		t.Fatalf("the notes hold %#v after the pass that indexed them, want an edge to CORE-2", refsFrom(t, database, "artifact", notes))
	}
	if len(liveAhead.documents) != 0 || len(liveAhead.rows) != 0 {
		t.Fatalf("the drain still remembers %v, want nothing", liveAhead.documents)
	}
}

// A version's index whose block pass loses the revision fence writes no block of the version
// (refs.BlockIndexing.Queue queues nothing for it), so its reconciliation settles nothing of the
// forms queue either: the document's row stays owed for the next pass, as the block index's own
// mark stays. The pass here runs at version 1, the version the document's blocks already stand
// at with no mark, which the fence refuses. The control is the same index one version above the
// stored one: it wins, writes its blocks and settles the row, so what kept the row owed was the
// fence and not an index that settles nothing.
func TestABlockPassThatLosesTheFenceLeavesTheFormsRowOwed(t *testing.T) {
	ctx := context.Background()
	database, service := blockIndexService(t)
	const markdown = "# Fenced\n\nThe plan is dispatch://CORE-1/plan.\n"
	document := createOwedDocument(t, database, service, "CORE-1", "", "fenced", markdown, "")
	settle(t, database, document)
	if tick := reconcileBlockIndex(t, service, ""); tick.Documents != 1 {
		t.Fatalf("the first tick indexed %d documents, want the fixture's one", tick.Documents)
	}
	if stands := blockIndexAt(t, database, document); stands == nil || *stands != 1 {
		t.Fatalf("the document's blocks stand at %s, want version 1", standsText(stands))
	}
	row := "artifact " + document
	oweForms := func() {
		t.Helper()
		if _, err := database.Pool.Exec(ctx, `
			insert into refs_forms_owed (kind, id) values ('artifact', $1::uuid)
			on conflict (kind, id) do update set enqueued_at = now()
		`, document); err != nil {
			t.Fatalf("owe the document to the forms queue: %v", err)
		}
	}
	index := func(version int) {
		t.Helper()
		live, blocks, err := service.TextWithBlocks(ctx, document)
		if err != nil {
			t.Fatalf("read the document: %v", err)
		}
		var versionAt time.Time
		if err := database.Pool.QueryRow(ctx, `
			select created_at from artifact_versions where artifact_id = $1 and number = $2
		`, document, version).Scan(&versionAt); err != nil {
			t.Fatalf("read version %d: %v", version, err)
		}
		tx, err := database.Pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin the index of version %d: %v", version, err)
		}
		defer tx.Rollback(ctx)
		_, _, err = service.indexRenderedVersion(ctx, tx, document, version, versionAt, live, blocks, nil, nil)
		if err != nil {
			t.Fatalf("index version %d: %v", version, err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit the index of version %d: %v", version, err)
		}
	}

	oweForms()
	index(1)
	if _, owed := formsOwedRows(t, database)[row]; !owed {
		t.Errorf("a block pass that lost the revision fence settled the document's forms row: it wrote no block of the version, so its index is not current")
	}
	if stands := blockIndexAt(t, database, document); stands == nil || *stands != 1 {
		t.Errorf("the losing pass moved the document's blocks to %s, want them at version 1", standsText(stands))
	}

	second := nextVersionRow(t, database, document, markdown)
	oweForms()
	index(second)
	if stands := blockIndexAt(t, database, document); stands == nil || *stands != second {
		t.Fatalf("the control pass at version %d, above the stored version 1, moved the blocks to %s, not to %d", second, standsText(stands), second)
	}
	if _, owed := formsOwedRows(t, database)[row]; owed {
		t.Errorf("the control pass at version %d won the fence and left the forms row owed, so the losing pass proves nothing", second)
	}
}
