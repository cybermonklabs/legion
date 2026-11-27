package refs

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// Pool is the part of the Dispatch connection pool a rebuild uses.
type Pool interface {
	Queryer
	Begin(ctx context.Context) (pgx.Tx, error)
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Rebuild counts what RebuildAll reconciled.
type Rebuild struct {
	Documents int
	Asks      int
	Comments  int
	Messages  int
	// Orphans is the number of edges deleted because no live source writes them.
	Orphans int
	// Edges is the number of mention edges after the rebuild.
	Edges int
	// BlocksOwed is the number of documents marked owed a block index (document_block_versions).
	BlocksOwed int
	// ItemsOwed is the number of comments, messages and asks marked owed a block index of their
	// own blocks (comment_block_state, message_block_state and ask_block_state owed_at).
	ItemsOwed int
}

// RebuildAll reparses every reference source with this build's whole grammar, the bare issue
// keys, `#id`s and short ids it resolves against the database included (Mentions), and
// reconciles refs with it, one transaction per source, so the index converges on what the live
// write paths produce: the latest version of every document, every ask question, every comment
// body, and every issue message (issue-less messages are never indexed), each read by
// sourceText. An edge that survives keeps its created_at and source_seq; one the rebuild
// introduces has no source_seq, since no event introduced it, and no current excerpt: a
// document mention's excerpt is cut from the document's tree, which only the document service
// reads, so its reconciliation
// (docs.Service.RunReferenceExcerptReconciliation) stores it. An edge that survives keeps its
// payload, but StoredExcerpts hides it until the row has a current excerpt_version.
//
// A document's blocks and the mentions they hold are cut from its tree too, so the rebuild marks
// every document owed a block index, which the block-index reconciliation
// (docs.Service.RunBlockIndexReconciliation) indexes. It sets both marks such a debt of this build
// carries, as docs' oweDocumentIndex does for one document: document_block_versions.version null,
// which every release drains, and store_owed_at, this build's own (migration 20261116221539),
// which the release before this one neither reads nor clears. A pod of that release may drain the
// null version first and write each document's new rows with no node, and the second mark brings
// this build's pass to the document after it (IndexBlocks). A mark a document already holds keeps
// its time. The mark is of this build's marking rule (RebuildMarkStatement).
//
// It marks every comment and message, direct messages included, and every ask owed a block index
// of its own blocks too (owed_at, a missing state row inserted owed), and owed its embedding
// windows with it (windows_owed_at, ItemsOwedWindows), so the same reconciliation re-reads each
// item's blocks, an ask's parts included, and the mentions each holds with this build's parser and
// grammar, and writes the windows those blocks stand in. It leaves block_grammar_owed alone: it
// has just re-read every comment, ask and issue message with this build's grammar itself.
//
// It clears the whole grammar's queue (refs_grammar_owed) for every source it re-indexes: each
// source's own row goes in that source's transaction (replaceRefs), and the `scope` rows, which
// owe a re-index of the sources around a document rather than one source, are deleted at the end
// for every row enqueued before the rebuild started. A row a write enqueues while the rebuild runs
// keeps its place: the rebuild may have read that source already. It settles nothing of the forms
// queue (refs_forms_owed, RefsFormsOwed) for a source it re-indexes: it reads a source's own
// references alone and leaves its blocks to the block index it marks owed, which a process of an
// older build may take during a roll and write without those forms, so each source stays owed
// until this build's own block index of it settles the row after its whole text
// (IndexOwedItemBlocks, docs.Service.indexOwedDocument), or the forms drain does.
//
// The orphan sweep deletes the edges of every source that is gone and, in the same transaction,
// what refs_forms_owed and the two queues of the Decisions page's reading (refs_decisions_forms_owed
// and refs_decisions_forms_seed_owed) hold for every source that is gone: the sweep's delete owes
// the first queue each source whose edges it deletes (migration 20261117025906's trigger), a delete
// from it relays what it removes to the second (migration 20261126040100), and a gone source owes
// no text.
func RebuildAll(ctx context.Context, pool Pool, serverURL string) (Rebuild, error) {
	var report Rebuild
	var started time.Time
	if err := pool.QueryRow(ctx, `select now()`).Scan(&started); err != nil {
		return report, fmt.Errorf("read the database's clock: %w", err)
	}
	// The decisions query is no document a rebuild re-reads: its versions render the questions and
	// decisions whose own sources hold their references, and no live write indexes it (docs'
	// writeRevisions stands its index with no pass), so the rebuild leaves it out, as the live
	// index does.
	sources := []struct {
		kind  string
		query string
		count *int
	}{
		{"artifact", `
			select a.id::text from artifacts a
			where exists (select 1 from artifact_versions v where v.artifact_id = a.id and v.markdown is not null)
			  and not ` + DecisionsQuerySQL("a") + `
			order by a.id`, &report.Documents},
		{"ask", `select id::text from asks order by id`, &report.Asks},
		{"comment", `select id::text from comments order by id`, &report.Comments},
		{"message", `select id::text from messages where issue_key is not null order by id`, &report.Messages},
	}
	for _, source := range sources {
		ids, err := loadSourceIDs(ctx, pool, source.query)
		if err != nil {
			return report, fmt.Errorf("load %s sources: %w", source.kind, err)
		}
		for _, id := range ids {
			rebuilt, err := replaceInTx(ctx, pool, source.kind, id, serverURL)
			if err != nil {
				return report, fmt.Errorf("rebuild %s %s: %w", source.kind, id, err)
			}
			if rebuilt {
				*source.count++
			}
		}
	}
	if _, err := pool.Exec(ctx, `delete from refs_grammar_owed where kind = 'scope' and enqueued_at <= $1`, started); err != nil {
		return report, fmt.Errorf("clear the scopes owed an index with the whole grammar: %w", err)
	}
	orphans, err := sweepOrphans(ctx, pool)
	if err != nil {
		return report, err
	}
	report.Orphans = orphans
	owed, err := pool.Exec(ctx, RebuildMarkStatement, BlockIndexMarkingRule)
	if err != nil {
		return report, fmt.Errorf("mark every document owed a block index: %w", err)
	}
	report.BlocksOwed = int(owed.RowsAffected())
	// Both markers: rebuild-refs re-derives every item from its stored text, its windows
	// included, so the windows an item holds now stand for nothing until that re-index writes
	// them again (ItemsOwedWindows). The one statement that writes them clears both.
	for _, mark := range []struct{ kind, query string }{
		{"comment", `
			insert into comment_block_state (comment_id, owed_at, windows_owed_at) select id, now(), now() from comments
			on conflict (comment_id) do update set owed_at = excluded.owed_at, windows_owed_at = excluded.windows_owed_at`},
		{"message", `
			insert into message_block_state (message_id, owed_at, windows_owed_at) select id, now(), now() from messages
			on conflict (message_id) do update set owed_at = excluded.owed_at, windows_owed_at = excluded.windows_owed_at`},
		{"ask", `
			insert into ask_block_state (ask_id, owed_at, windows_owed_at) select id, now(), now() from asks
			on conflict (ask_id) do update set owed_at = excluded.owed_at, windows_owed_at = excluded.windows_owed_at`},
	} {
		owed, err := pool.Exec(ctx, mark.query)
		if err != nil {
			return report, fmt.Errorf("mark every %s owed a block index: %w", mark.kind, err)
		}
		report.ItemsOwed += int(owed.RowsAffected())
	}
	if err := pool.QueryRow(ctx, `select count(*) from refs`).Scan(&report.Edges); err != nil {
		return report, fmt.Errorf("count references: %w", err)
	}
	return report, nil
}

// RebuildMarkStatement is RebuildAll's mark of every document owed a block index, at the marking
// rule $1 (BlockIndexMarkingRule): both marks of a debt, a null version and this build's own mark
// (store_owed_at), as docs' oweDocumentIndexStatement sets them on one document. A mark the
// document already holds keeps its time and keeps the higher of its rule and $1, so a mark of a
// higher rule stays that rule's to drain; where no mark stands the row takes $1, whatever rule a
// mark cleared before left on it, since the rule is read only where a mark stands (OwedRuleKept).
// It is a named statement so the test that holds the marking rule to its statements reads it
// (TestTheMarkingRuleNamesTheStatementsItMarksWith).
var RebuildMarkStatement = `
	insert into document_block_versions (artifact_id, store_owed_at, store_owed_rule)
	select id, now(), $1::integer from artifacts where kind = 'doc'
	on conflict (artifact_id) do update set version = null,
		store_owed_at = coalesce(document_block_versions.store_owed_at, excluded.store_owed_at),
		store_owed_rule = ` + OwedRuleKept("document_block_versions", "excluded.store_owed_rule") + `
`

func loadSourceIDs(ctx context.Context, pool Pool, query string) ([]string, error) {
	rows, err := pool.Query(ctx, query)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// sweepOrphans deletes the edges of every source that is gone and, after that delete and in its
// transaction, what refs_forms_owed holds for every source that is gone, which that delete has just
// owed it (migration 20261117025906), and what refs_decisions_forms_owed and
// refs_decisions_forms_seed_owed hold for them: a gone source owes no text, and one transaction
// leaves no such row behind for a drain to list between the two. The delete from refs_forms_owed
// relays only the rows of a source that still exists (migration 20261126040100), so the second
// delete is for the rows an older build relayed before its source went, and the third for the
// sources removed since the seed read them. It reports how many edges it deleted.
func sweepOrphans(ctx context.Context, pool Pool) (int, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	orphans, err := tx.Exec(ctx, `
		delete from refs r
		where not exists (
			select 1 from artifacts a where r.from_kind = 'artifact' and a.id::text = r.from_id
			union all
			select 1 from asks k where r.from_kind = 'ask' and k.id::text = r.from_id
			union all
			select 1 from comments c where r.from_kind = 'comment' and c.id::text = r.from_id
			union all
			select 1 from messages m where r.from_kind = 'message' and m.issue_key is not null and m.id::text = r.from_id
		)
	`)
	if err != nil {
		return 0, fmt.Errorf("delete orphan references: %w", err)
	}
	// Each queue's delete names its table: the census of item-row writers and the projection
	// scan read the table a statement writes from its text, and a name built at runtime is one
	// they cannot read.
	if _, err := tx.Exec(ctx, `
		delete from refs_forms_owed o
		where not exists (
			select 1 from artifacts a where o.kind = 'artifact' and a.id = o.id
			union all
			select 1 from asks k where o.kind = 'ask' and k.id = o.id
			union all
			select 1 from comments c where o.kind = 'comment' and c.id = o.id
			union all
			select 1 from messages m where o.kind = 'message' and m.issue_key is not null and m.id = o.id
		)
	`); err != nil {
		return 0, fmt.Errorf("settle the index owed by the sources the orphan sweep found gone: %w", err)
	}
	// kept for one release: decisions-ref-repair
	if _, err := tx.Exec(ctx, `
		delete from refs_decisions_forms_owed o
		where not exists (
			select 1 from artifacts a where o.kind = 'artifact' and a.id = o.id
			union all
			select 1 from asks k where o.kind = 'ask' and k.id = o.id
			union all
			select 1 from comments c where o.kind = 'comment' and c.id = o.id
			union all
			select 1 from messages m where o.kind = 'message' and m.issue_key is not null and m.id = o.id
		)
	`); err != nil {
		return 0, fmt.Errorf("settle the decisions index owed by the sources the orphan sweep found gone: %w", err)
	}
	// kept for one release: decisions-ref-repair
	if _, err := tx.Exec(ctx, `
		delete from refs_decisions_forms_seed_owed o
		where not exists (
			select 1 from artifacts a where o.kind = 'artifact' and a.id = o.id
			union all
			select 1 from asks k where o.kind = 'ask' and k.id = o.id
			union all
			select 1 from comments c where o.kind = 'comment' and c.id = o.id
			union all
			select 1 from messages m where o.kind = 'message' and m.issue_key is not null and m.id = o.id
		)
	`); err != nil {
		return 0, fmt.Errorf("settle the bootstrap decisions index owed by the sources the orphan sweep found gone: %w", err)
	}
	return int(orphans.RowsAffected()), tx.Commit(ctx)
}

// replaceInTx reconciles one source's refs with its text in a transaction of its own, and
// reports false for a source gone since it was listed, whose edges the orphan sweep deletes.
//
// It reads the text under the lock every writer of that text takes, the source's owner lock
// (readSourceUnderOwner), so this index of the text and a concurrent edit of it are ordered
// rather than racing: without it a rebuild reads a source, an editor writes new text and its own
// edges, and this transaction then replaces those edges with the ones it built from the text it
// read, losing the edit's own index until something re-indexes that source (INFRA-3285). The
// lock is taken before the read and the read is a statement after it, so a write committed while
// the rebuild waited is the text it indexes; it is an advisory lock, so it assigns no transaction
// id, and the edges are planned under it and written last as one batch (PlanReplace,
// Replacement.Queue). The re-index paths beside this one take the same lock through the same
// read, for the same reason (ReindexOwed and ReindexScopeSource below). The lock is one owner at a
// time, held for the length of that source's own transaction, so a rebuild still never holds every
// owner: making the rebuild one transaction instead would order it against every writer at once
// and hold every owner for the length of the pass, which is minutes on a large database.
//
// A document's reconcile states the edges it moves in the node and tuple store in the same
// transaction, right after the batch that writes its refs rows (Replacement.ArtifactMentions): a
// mentions tuple to each target it inserts and a retraction to each it deletes, dated at its rows'
// own created_at, the transaction's now(), and signed by the first author of the document's newest
// version (DocumentMentioning). The rebuild appends no event, so each is appended at the sentinel
// and an assertion keeps its row's source_seq, none, as its legacy_source_seq (graphstore.QueueCaused
// with NoEvent).
//
// It reads the source's own references alone, and its blocks wait for the block index RebuildAll
// marks owed, so it settles the whole grammar's queue and leaves the forms queue owed: its
// replacement is queued without SettlingForms (replaceRefs reads the same rule).
func replaceInTx(ctx context.Context, pool Pool, fromKind, fromID, serverURL string) (bool, error) {
	read, err := sourceRead(fromKind, fromID)
	if err != nil {
		return false, err
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	var body string
	var author []byte
	found, err := readSourceUnderOwner(ctx, tx, fromKind, fromID, read, &body, &author, new(time.Time))
	if err != nil || !found {
		return false, err
	}
	writer, err := writerOf(author)
	if err != nil {
		return false, fmt.Errorf("read the author of %s %s: %w", fromKind, fromID, err)
	}
	located := NewReader(ctx, tx, writer, fromKind, fromID, serverURL).Mentions(body)
	replacement, err := PlanReplace(ctx, tx, fromKind, fromID, text.Refs(located), nil)
	if err != nil {
		return false, err
	}
	var mentions []graphstore.UncausedTuple
	if fromKind == graphstore.KindArtifact {
		record, held, err := DocumentMentioning(ctx, tx, fromID)
		if err != nil {
			return false, err
		}
		if held {
			if mentions, err = replacement.ArtifactMentions(ctx, tx, record); err != nil {
				return false, err
			}
		}
	}
	if err := store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
		replacement.Queue(b)
		return nil
	}); err != nil {
		return false, err
	}
	if err := graphstore.Caused(ctx, tx, graphstore.Uncaused{Tuples: mentions}, graphstore.NoEvent); err != nil {
		return false, fmt.Errorf("state the references of document %s: %w", fromID, err)
	}
	return true, tx.Commit(ctx)
}

// sourceRead is the statement that reads the text one reference source writes, with the author it
// records and the time that text was written: a document's latest version's markdown (id its
// uuid), the first of that version's authors, which is null where it records none; an ask's
// question, a comment's body, or an issue message's body, with the row's author. The time is the
// row's coalesce(edited_at, created_at) in each of the three tables that records an edit - asks,
// comments and issue messages, whose edit is PATCH /api/v1/messages/{id} - and the version's
// created_at for a document, since a stored version is rewritten by nothing. The author is whom
// the text's short ids resolve for (NewReader). It locks nothing: a caller that orders itself
// against the edits of that text holds the source's owner lock and runs this statement after it
// (replaceInTx, ReindexOwed).
func sourceRead(kind, id string) (string, error) {
	switch kind {
	case "artifact":
		return `select markdown, authors->0, created_at from artifact_versions
			where artifact_id = $1::uuid and markdown is not null order by number desc limit 1`, nil
	case "ask":
		return `select question, author, coalesce(edited_at, created_at) from asks where id = $1::uuid`, nil
	case "comment":
		return `select body, author, coalesce(edited_at, created_at) from comments where id = $1::uuid`, nil
	case "message":
		return `select body, author, coalesce(edited_at, created_at) from messages where id = $1::uuid and issue_key is not null`, nil
	}
	return "", fmt.Errorf("read the text of %s %s: not a reference source", kind, id)
}

// sourceText reads one reference source's text, and the time it was written, with sourceRead's
// statement, whose author it skips. found is false when no such source exists: a deleted row, a
// document with no markdown version, a direct message, which nothing indexes.
func sourceText(ctx context.Context, q Queryer, kind, id string) (string, time.Time, bool, error) {
	read, err := sourceRead(kind, id)
	if err != nil {
		return "", time.Time{}, false, err
	}
	var body string
	var writtenAt time.Time
	switch err := q.QueryRow(ctx, read, id).Scan(&body, nil, &writtenAt); {
	case errors.Is(err, pgx.ErrNoRows):
		return "", time.Time{}, false, nil
	case err != nil:
		return "", time.Time{}, false, fmt.Errorf("read the text of %s %s: %w", kind, id, err)
	}
	return body, writtenAt, true, nil
}

// readSourceUnderOwner takes the owner lock of reference source kind id (sourceOwner) and runs
// read, the statement that lock guards, in the statement after it, scanning its one row into dest.
// It is how a background pass reads a source it is about to re-index (replaceInTx, ReindexOwed,
// IndexOwedItemBlocks): the lock excludes every writer of that source, and the read sees what the
// lock's previous holder committed. A source whose owner is nil - a direct message, whose
// conversation is no owner - is read with no lock, since nothing rewrites a direct message's
// body: only an issue message is edited or deleted.
// found is false when the source is gone, before the lock or while the pass waited for it: no row
// is a source that does not exist rather than a failure.
func readSourceUnderOwner(ctx context.Context, tx pgx.Tx, kind, id, read string, dest ...any) (bool, error) {
	owner, found, err := sourceOwner(ctx, tx, kind, id)
	if err != nil || !found {
		return false, err
	}
	// The owner's lock, then the read in a statement of its own (store.LockOwnerAndRead): that
	// statement's snapshot is taken once the lock is granted, so it holds everything the lock's
	// previous holder committed, an edit of this source among it.
	if owner != nil {
		err = store.LockOwnerAndRead(ctx, tx, *owner, read, []any{id}, dest...)
	} else {
		err = tx.QueryRow(ctx, read, id).Scan(dest...)
	}
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("read %s %s under its owner: %w", kind, id, err)
	}
	return true, nil
}

// sourceOwner is the owner whose lock every writer of reference source kind id takes, and which a
// re-index of its text takes before it reads it (readSourceUnderOwner): the issue of an issue's
// document, comment, ask or issue message (api.ownerForArtifact, api.ownerOf,
// api.createStoredMessage), and the artifact of a document no issue owns and of the comments and
// asks on it (the owner api.lockedComment locks). The issue-else-document rule is store.OwnerOf's,
// which holds it for every caller. A direct message has none: its conversation is no owner, and no
// site rewrites a message's body. The read takes no lock, since no source moves between owners.
// found is false when the source is gone.
func sourceOwner(ctx context.Context, q Queryer, kind, id string) (*store.Owner, bool, error) {
	var query string
	switch kind {
	case "artifact":
		query = `select issue_key, id::text from artifacts where id = $1::uuid`
	case "ask":
		query = `select issue_key, artifact_id::text from asks where id = $1::uuid`
	case "comment":
		query = `select issue_key, artifact_id::text from comments where id = $1::uuid`
	case "message":
		query = `select issue_key, null::text from messages where id = $1::uuid`
	default:
		return nil, false, fmt.Errorf("read the owner of %s %s: not a reference source", kind, id)
	}
	var issueKey, artifactID *string
	switch err := q.QueryRow(ctx, query, id).Scan(&issueKey, &artifactID); {
	case errors.Is(err, pgx.ErrNoRows):
		return nil, false, nil
	case err != nil:
		return nil, false, fmt.Errorf("read the owner of %s %s: %w", kind, id, err)
	}
	return store.OwnerOf(issueKey, artifactID), true, nil
}

// OwedQueue is one queue of text owed a re-index: a table of (kind, id, enqueued_at) a trigger
// upserts a row into for every source a write left indexed with a grammar older than the one
// this build reads it with, which a drain lists oldest first (ListOwed), settles (SettleOwed)
// and re-indexes (ReindexOwed). Dispatch holds the five below, and the table each names is the
// whole of the difference between them: one drain, one order, for all five.
type OwedQueue struct {
	// table is the queue's table, which every statement of the drain names.
	table string
}

// BlockGrammarOwed is the queue of comments, asks and issue messages an older build indexed
// without the block grammar (migration 0121, block_grammar_owed).
var BlockGrammarOwed = OwedQueue{table: "block_grammar_owed"}

// RefsGrammarOwed is the queue of sources an older build indexed without this build's whole
// grammar (migration 20261103205037, refs_grammar_owed). A queue of its own rather than a row
// in the block grammar's: that migration says why.
var RefsGrammarOwed = OwedQueue{table: "refs_grammar_owed"}

// RefsFormsOwed is the queue of sources an older build indexed without the reference forms
// migration 20261117025900 adds (an issue's tab addresses and a document's dashboard URLs,
// refs_forms_owed), and the rule it keeps is one sentence: a row stays owed until a build that
// reads those forms has indexed that text itself. A queue of its own rather than a row in the
// whole grammar's, and drained in a duty of its own (docs.Service.RunRefsFormsReconciliation):
// that migration says why. Every delete from refs and from each block table owes the source it
// deletes the edges of (migrations 20261117025906-20261117025910), and this build settles a
// source's row only in a transaction that indexed the source's whole text, its blocks too, after
// every delete that index made, in a statement its write phase already sends after them
// (Replacement.SettlingForms, ItemIndexing.SettlingForms), so only an older build's re-index,
// and a write of this build that left the blocks to its block index, leave the source owed.
var RefsFormsOwed = OwedQueue{table: "refs_forms_owed"}

// RefsDecisionsFormsOwed is the queue of sources a build that reads an issue's Decisions page as
// no more than its document indexed (migration 20261126040000, refs_decisions_forms_owed):
// `dispatch://KEY/decisions` and the page's own link, `/issues/KEY/decisions`, both name the issue
// the page reads the decision nodes of, where the release before reads the first as no reference
// and the second as the issue's retired decisions document (RetiredDecisionsSlug). It is
// RefsFormsOwed's rule again, for a source that release has already settled: a row stays owed
// until a build that reads the page has indexed that text itself.
//
// A queue of its own because RefsFormsOwed alone cannot hold the debt. The release before drains
// RefsFormsOwed too, and settles a row of it as soon as it has indexed the text with the grammar
// it knows, in the transaction that deleted the edges of this reading, so nothing there says the
// text was ever indexed without them. A trigger on that table relays each row a delete removes
// from it (migration 20261126040100), whoever deletes it, and a trigger on each block table owes
// the owner of a block edge to the retired document, the one edge that build can add and delete
// none (migrations 20261126040200-20261126040500). This build deletes the row it relayed in a
// statement of its own, after the delete from RefsFormsOwed, in every transaction that indexed
// the source's whole text (settleDecisionsFormsStatement): the relay fires at the end of the
// statement that deletes the row it relays, so a delete beside it, in one statement, would run
// before the relay and leave the row for good. It is drained by the duty that drains RefsFormsOwed
// (docs.Service.RunRefsFormsReconciliation).
// kept for one release: decisions-ref-repair
var RefsDecisionsFormsOwed = OwedQueue{table: "refs_decisions_forms_owed"}

// RefsDecisionsFormsSeedOwed is the bootstrap half of RefsDecisionsFormsOwed (migration
// 20261126040000, refs_decisions_forms_seed_owed): the sources already stored when this release
// was deployed, which the seed (migration 20261126040600) owes an index with the Decisions page's
// reading. Nothing else inserts into it, and the live queue's writers insert nowhere else: the
// seed holds a row for every stored text uncommitted until it commits, so a seed into the live
// queue made every writer of the release before wait for it and could deadlock with one. A source
// in both queues is indexed once, and a complete index clears its row of each
// (settleDecisionsFormsStatement). It has no trigger, so no delete from it relays anything. It is
// drained by the same duty, one pass, and a drain that finds it empty never reads it again.
// kept for one release: decisions-ref-repair
var RefsDecisionsFormsSeedOwed = OwedQueue{table: "refs_decisions_forms_seed_owed"}

// settleDecisionsFormsStatement is the delete that clears one source's row of both halves of the
// Decisions page's queue, the live one (RefsDecisionsFormsOwed) and the bootstrap one
// (RefsDecisionsFormsSeedOwed), with the source's kind as $1 and its id as $2. A write that
// indexed the source's whole text queues it as a statement of its own after every delete from
// RefsFormsOwed it queued, and never inside one: the relay of the delete into the live queue fires
// after its statement. The bootstrap delete rides in the same statement, since no trigger writes
// that table.
// kept for one release: decisions-ref-repair
const settleDecisionsFormsStatement = `
	with seed as (delete from refs_decisions_forms_seed_owed where kind = $1 and id = $2::uuid)
	delete from refs_decisions_forms_owed where kind = $1 and id = $2::uuid`

// Owed is one source a queue owes an index, as of the write that last owed it (EnqueuedAt).
//
// Kind is the source's own kind - comment, ask or message - or, in the whole grammar's queue and
// the forms queues, `artifact`, a document read at its latest version, or, in the whole grammar's
// queue alone, `scope`, whose id is a document whose question blocks moved. Those last two name no
// text this re-indexes: what a `scope` row owes is a re-index of every source whose text belongs
// to that document (ScopeSources) and what an `artifact` row owes is a block index of that
// document, each the drain's own work (docs.Service.reindexOwedText, and for the forms queues
// docs.Service.drainFormsDocument). ReindexOwed answers an error for either rather than reading a
// row with no text of its own and settling it, so no caller can drain one by handing it here.
type Owed struct {
	Queue      OwedQueue
	Kind       string
	ID         string
	EnqueuedAt time.Time
}

// reindexable reports whether the row names a text ReindexOwed can read and index: a comment,
// an ask or an issue message, the three kinds sourceText reads as one source's own text.
func (owed Owed) reindexable() bool {
	switch ItemKind(owed.Kind) {
	case ItemComment, ItemAsk, ItemMessage:
		return true
	}
	return false
}

// listQuery is the statement a drain of this queue lists its oldest rows with, and listAfterQuery
// the one it lists the rows past a row it listed with (ListOwedAfter): the row comparison is on
// the three columns of the order, so it is a bound of the same index scan and the read starts at
// the cursor rather than reading every row before it.
//
// The order columns are written qualified: unqualified, `id` would name the selected `id::text`
// rather than the table's uuid, so every tick would sort the whole tie group the seed enqueues
// under one enqueued_at by a text it casts per row, and on the whole grammar's queue the index
// that carries all three order columns (refs_grammar_owed_enqueued) could not serve the order
// at all. The two orders are the same - a uuid's canonical text is its bytes in lowercase hex,
// hyphens at fixed positions - so the rows come back as they always did
// (TestTheDrainSortsEachQueueByItsTablesID).
func (queue OwedQueue) listQuery() string {
	return `
		select o.kind, o.id::text, o.enqueued_at from ` + queue.table + ` o
		order by o.enqueued_at, o.kind, o.id limit $1
	`
}

func (queue OwedQueue) listAfterQuery() string {
	return `
		select o.kind, o.id::text, o.enqueued_at from ` + queue.table + ` o
		where (o.enqueued_at, o.kind, o.id) > ($2, $3, $4::uuid)
		order by o.enqueued_at, o.kind, o.id limit $1
	`
}

// Table is the name of the queue's table.
func (queue OwedQueue) Table() string {
	return queue.table
}

// Count is how many rows the queue holds.
func (queue OwedQueue) Count(ctx context.Context, q Queryer) (int, error) {
	var owed int
	if err := q.QueryRow(ctx, `select count(*) from `+queue.table).Scan(&owed); err != nil {
		return 0, fmt.Errorf("count what %s owes an index: %w", queue.table, err)
	}
	return owed, nil
}

// settleOwedStatement is the delete that settles one row of this queue, guarded by the
// enqueued_at the row was listed with: both writers of a settlement run it, the drain's own at
// once (SettleOwed) and the one a write phase queues (QueueSettleOwed).
func (queue OwedQueue) settleOwedStatement() string {
	return `delete from ` + queue.table + ` where kind = $1 and id = $2::uuid and enqueued_at = $3`
}

// ListOwed lists at most limit sources queue owes an index, oldest first, and locks nothing.
func ListOwed(ctx context.Context, q Queryer, queue OwedQueue, limit int) ([]Owed, error) {
	return ListOwedAfter(ctx, q, queue, Owed{}, limit)
}

// ListOwedAfter is ListOwed from the row after after, a row an earlier call listed, in the
// queue's order (enqueued_at, kind, id): the keyset cursor of a drain that walks the whole queue
// one batch a pass, so a row it leaves owed is passed over until the walk comes round again
// rather than listed first on every pass. A zero after lists from the start. A row a write owes
// again meanwhile takes a later enqueued_at, which sorts past the cursor, so the walk reaches it.
func ListOwedAfter(ctx context.Context, q Queryer, queue OwedQueue, after Owed, limit int) ([]Owed, error) {
	query, args := queue.listQuery(), []any{limit}
	if !after.EnqueuedAt.IsZero() {
		query, args = queue.listAfterQuery(), []any{limit, after.EnqueuedAt, after.Kind, after.ID}
	}
	rows, err := q.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list what %s owes an index: %w", queue.table, err)
	}
	defer rows.Close()
	owed := []Owed{}
	for rows.Next() {
		source := Owed{Queue: queue}
		if err := rows.Scan(&source.Kind, &source.ID, &source.EnqueuedAt); err != nil {
			return nil, fmt.Errorf("scan a row of %s: %w", queue.table, err)
		}
		owed = append(owed, source)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list what %s owes an index: %w", queue.table, err)
	}
	return owed, nil
}

// Before reports whether owed sorts before other in a queue's order (enqueued_at, kind, id), the
// order ListOwedAfter walks in. Both are rows of one queue, so each id is a uuid's canonical text,
// whose order is the uuid's.
func (owed Owed) Before(other Owed) bool {
	if !owed.EnqueuedAt.Equal(other.EnqueuedAt) {
		return owed.EnqueuedAt.Before(other.EnqueuedAt)
	}
	if owed.Kind != other.Kind {
		return owed.Kind < other.Kind
	}
	return owed.ID < other.ID
}

// SettleOwed deletes source's row from its queue while it still holds the enqueued_at source
// was listed with, and reports whether it did: a write that lands meanwhile moves that time,
// and the source stays owed for the next pass, which reads its newer text.
func SettleOwed(ctx context.Context, q Execer, source Owed) (bool, error) {
	settled, err := q.Exec(ctx, source.Queue.settleOwedStatement(), source.Kind, source.ID, source.EnqueuedAt)
	if err != nil {
		return false, fmt.Errorf("settle the index %s owes for %s %s: %w", source.Queue.table, source.Kind, source.ID, err)
	}
	return settled.RowsAffected() > 0, nil
}

// StillOwed reports whether source's row still holds the enqueued_at source was listed with,
// read in the drain's read phase, under the source's owner lock. Every writer of that text takes
// that lock before it writes, and the owing trigger fires in that write's write phase, so no
// write can move the row while this transaction holds the lock: what this read answers is what
// the write phase's delete finds, and the drain decides whether to write anything without
// reading one of its own statements' results before the rest are sent. A drain outside this
// package that writes the text itself - the forms drain's index of a document
// (docs.Service.indexOwedDocument) - reads it the same way.
func StillOwed(ctx context.Context, q Queryer, source Owed) (bool, error) {
	var enqueuedAt time.Time
	switch err := q.QueryRow(ctx,
		`select enqueued_at from `+source.Queue.table+` where kind = $1 and id = $2::uuid`,
		source.Kind, source.ID).Scan(&enqueuedAt); {
	case errors.Is(err, pgx.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("read the index %s owes for %s %s: %w", source.Queue.table, source.Kind, source.ID, err)
	}
	return enqueuedAt.Equal(source.EnqueuedAt), nil
}

// QueueSettleOwed queues SettleOwed's delete on a write phase's batch, first among the drain's
// writes, so the row is settled before the text it was owed for is written. It is still guarded
// by the enqueued_at the row was listed with, which StillOwed read under the source's owner lock.
func QueueSettleOwed(b *store.WriteBatch, source Owed) {
	b.Queue(fmt.Sprintf("settle the index %s owes for %s %s", source.Queue.table, source.Kind, source.ID),
		source.Queue.settleOwedStatement(), source.Kind, source.ID, source.EnqueuedAt)
}

// ReindexOwed reconciles one owed comment's, ask's or issue message's refs with its text, read
// with the grammar this build reads it with, and settles its owed row, in one transaction. It
// takes the source's owner lock (sourceOwner), the lock every writer of that text takes, and
// reads the text in the statement after it (readSourceUnderOwner, sourceRead), so a write
// committed while it waited is the text it indexes; a direct message, whose conversation is no
// owner, has no such lock and needs none, since nothing rewrites a direct message's body: only an
// issue message is edited or deleted. It takes no source row: a row lock would assign the
// transaction a transaction id to hold through everything it reads and plans, and what it would
// exclude beyond the owner lock is a writer of that row that takes no owner lock, which no writer
// a server runs is: api's lock table queues behind that item's owner lock the serving writers of
// a comment, a message or an ask that open their own transaction
// (TestEveryWriterOnTheLockTableWaitsForTheOwnerLockOfTheRowItWrites,
// item_row_owner_lock_test.go), and those that write inside a transaction their caller opened
// rest on the lock that caller took, which that table does not drive; its gap map names each with
// why (TestTheLockTablePlacesEveryWriterTheCensusNames). Everything it writes is read and planned
// first, under that lock and with no transaction id held - the owed row itself (StillOwed), a
// comment's or a message's block index (ReadItemIndex, PlanItemIndex), an ask's part index
// (PlanAskParts) and the references the text cites (PlanReplace) - and its writes then go as one
// batch, the owed row's delete first (QueueSettleOwed, store.RunWrite). The text's own write then
// settles every queue that source is in again, after every delete its index of the text and its
// blocks made (reindexPlan.queue), since each such delete owes refs_forms_owed the source it
// rewrites (migrations 20261117025906-20261117025910), and does it for refs_decisions_forms_owed
// in a statement of its own after the delete from refs_forms_owed, which relays the row it
// removes there (settleDecisionsFormsStatement). It reports whether it re-indexed the source.
//
// The owed row is read and settled only while it holds the enqueued_at source was listed with: a
// write that lands between the listing and the lock moves it, and this pass is left with nothing
// written for the text it read, for the next pass, which reads the newer text under the same
// lock. Dropping the source's row lock leaves no deadlock with the trigger that writes the owed
// row: the trigger fires on the write's update of the text, in that write's write phase, under
// the owner lock it took before its first read, so a write and this pass never hold a row the
// other waits for, and the owner lock alone orders them.
//
// A new edge takes the time the text was written (sourceRead) and no source_seq, since no event
// introduced it, as a rebuild's does; a re-index of text no one rewrote appends no event, as
// rebuild-refs appends none. So no ?since= read lists such an edge until a later write of the
// source stamps it with that write's event (Stamp), and then it is listed as that write's. A
// source that is gone has its edges deleted with its owed row, and its blocks and an ask's
// parts are indexed with its text (planReindex).
func ReindexOwed(ctx context.Context, pool Pool, source Owed, serverURL string) (bool, error) {
	if !source.reindexable() {
		return false, fmt.Errorf("re-index %s %s: a %s row of %s names no text of its own",
			source.Kind, source.ID, source.Kind, source.Queue.table)
	}
	read, err := sourceRead(source.Kind, source.ID)
	if err != nil {
		return false, err
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	var body string
	var writtenAt time.Time
	var author []byte
	found, err := readSourceUnderOwner(ctx, tx, source.Kind, source.ID, read, &body, &author, &writtenAt)
	if err != nil {
		return false, err
	}
	owes, err := StillOwed(ctx, tx, source)
	if err != nil || !owes {
		return false, err
	}
	plan, err := planReindex(ctx, tx, source.Kind, source.ID, body, serverURL, author, writtenAt, found)
	if err != nil {
		return false, err
	}
	if err := store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
		QueueSettleOwed(b, source)
		plan.queue(b)
		return nil
	}); err != nil {
		return false, fmt.Errorf("re-index %s %s with this build's grammar: %w", source.Kind, source.ID, err)
	}
	return true, tx.Commit(ctx)
}

// reindexPlan is one source's text as a re-index of it is planned: the index of its own blocks -
// a comment's and a message's, an ask's parts - and the reconciliation of the references its
// text cites. Both are planned with no transaction id held and written by queue, which puts
// their statements on a write phase's batch. A source that is gone holds neither, and its edges
// are deleted instead.
type reindexPlan struct {
	kind, id string
	// found is what the read of the source answered, and indexed whether the plan holds an index
	// of the source's own blocks: a comment's and a message's, read from the body, and an ask's
	// parts, read from its row, where an ask gone since has none.
	found, indexed bool
	indexing       ItemIndexing
	replacement    Replacement
}

// planReindex plans the re-index of kind id's text, body read with the whole grammar through a
// reader of the source's own (NewReader), for author, the author the source's row records as
// sourceRead reads it, in tx, which holds the source's owner lock and read the text in the
// statement after it (readSourceUnderOwner). found is what that read answered: a source that is
// gone is planned as the delete of its edges. A comment or a message has its blocks planned too
// (PlanItemIndex, whose first queued statement takes the item's block state row), and an ask its
// parts (PlanAskParts, read from the row the owner lock guards and through the same reader), so
// the mentions its blocks hold are read with the same grammar as its own.
// The reconciliation of its own references is queued after the blocks, so where the plan holds
// the source's blocks it settles the forms queue too (Replacement.SettlingForms), after both
// deletes, and the source's row is settled only for its whole text.
func planReindex(ctx context.Context, tx pgx.Tx, kind, id, body, serverURL string, author []byte, writtenAt time.Time, found bool) (reindexPlan, error) {
	plan := reindexPlan{kind: kind, id: id, found: found}
	if !found {
		return plan, nil
	}
	writer, err := writerOf(author)
	if err != nil {
		return reindexPlan{}, fmt.Errorf("read the author of %s %s: %w", kind, id, err)
	}
	resolve := NewReader(ctx, tx, writer, kind, id, serverURL)
	located := resolve.Mentions(body)
	switch itemKind := ItemKind(kind); itemKind {
	case ItemComment, ItemMessage:
		index, err := ReadItemIndex(ctx, tx, itemKind, id)
		if err != nil {
			return reindexPlan{}, fmt.Errorf("index the blocks of %s %s: %w", kind, id, err)
		}
		if plan.indexing, err = PlanItemIndex(index, itemKind, id, body, serverURL, writtenAt, located, nil); err != nil {
			return reindexPlan{}, fmt.Errorf("index the blocks of %s %s: %w", kind, id, err)
		}
		plan.indexed = true
	case ItemAsk:
		// An ask's parts are its question, its options and its answer, where sourceRead reads
		// the question alone, which is what the ask's own refs are written from. They are read
		// from the row the owner lock guards, in the read phase, and planned as every other
		// item's blocks are.
		ask, _, held, err := askForParts(ctx, tx, id)
		if err != nil {
			return reindexPlan{}, err
		}
		if held {
			index, err := ReadItemIndex(ctx, tx, ItemAsk, id)
			if err != nil {
				return reindexPlan{}, fmt.Errorf("index the parts of ask %s: %w", id, err)
			}
			if plan.indexing, err = PlanAskParts(index, resolve, ask, writtenAt); err != nil {
				return reindexPlan{}, err
			}
			plan.indexed = true
		}
	}
	replacement, err := PlanReplace(ctx, tx, kind, id, text.Refs(located), &writtenAt)
	if err != nil {
		return reindexPlan{}, fmt.Errorf("index %s %s: %w", kind, id, err)
	}
	if plan.indexed {
		replacement = replacement.SettlingForms()
	}
	plan.replacement = replacement
	return plan, nil
}

// queue queues the plan's writes on a write phase's batch: the source's blocks and the mentions
// each holds, then the reconciliation of its own references, whose settle of the queues follows
// both deletes; for a source that is gone, the delete of its edges and then the settle of what
// it owes the queues, since that delete owes refs_forms_owed the source whose edges it deletes
// (migration 20261117025906), and a gone source owes no text. That settle is a statement of its
// own after the delete: the delete's trigger owes the row at the end of its statement, which a
// delete of the row in the same statement would not see. The same holds a second time for
// refs_decisions_forms_owed, whose row the delete from refs_forms_owed relays, so its delete is
// a statement after that one (queueSettleSourceOwed).
func (p reindexPlan) queue(b *store.WriteBatch) {
	if !p.found {
		b.Queue(fmt.Sprintf("delete the references of %s %s, which is gone", p.kind, p.id),
			`delete from refs where from_kind = $1 and from_id = $2`, p.kind, p.id)
		queueSettleSourceOwed(b, p.kind, p.id, true)
		return
	}
	if p.indexed {
		p.indexing.Queue(b)
	}
	p.replacement.Queue(b)
}

// ReindexScopeSource re-indexes one comment, ask or issue message whose text belongs to a document
// whose question blocks moved (ScopeSources), so that a `#id` it wrote before its question settled
// is stored as an edge to that ask now. It writes nothing for a text that holds no `#id` candidate
// at all (HoldsQuestionCandidate), which is what most of a document's scope is - for a spec, every
// message, ask and comment of its issue. It reports whether it re-indexed the source, and the
// source's own owed row, if it has one, is settled by that re-index (Replacement.Queue).
//
// The text is read twice: once with no lock (SourceText), which decides only whether this source
// is worth a transaction, and again inside the transaction under the source's owner lock
// (readSourceUnderOwner), which is the text the index is written from. Locking first would take
// the lock every writer of that text takes on every source of the scope before throwing most of
// them away, and a concurrent write of each would wait behind it; the artifact branch of
// docs.Service.reindexScope already reads first and owes the work after. Everything the write
// needs is read and planned under that lock, with no transaction id held (planReindex), and its
// statements go as one batch.
//
// A text that gains a `#id` between the two reads is indexed by the write that gave it one: a
// write of this build re-indexes the text it wrote in its own transaction (replaceRefs), reading
// the question map as it stands after the settlement this scope row was enqueued by, and a write
// of an older build owes the source its own row (20261103205038-20261103205040), which the drain
// takes in a later tick. One that loses its `#id` between them is left alone by the locked read,
// which is what the write that removed it already indexed.
func ReindexScopeSource(ctx context.Context, pool Pool, kind, id, serverURL string) (bool, error) {
	unlocked, _, found, err := SourceText(ctx, pool, kind, id)
	if err != nil || !found || !HoldsQuestionCandidate(unlocked, serverURL) {
		return false, err
	}
	read, err := sourceRead(kind, id)
	if err != nil {
		return false, err
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	var body string
	var writtenAt time.Time
	var author []byte
	found, err = readSourceUnderOwner(ctx, tx, kind, id, read, &body, &author, &writtenAt)
	if err != nil || !found {
		return false, err
	}
	if !HoldsQuestionCandidate(body, serverURL) {
		return false, nil
	}
	plan, err := planReindex(ctx, tx, kind, id, body, serverURL, author, writtenAt, true)
	if err != nil {
		return false, err
	}
	if err := store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
		plan.queue(b)
		return nil
	}); err != nil {
		return false, fmt.Errorf("re-index %s %s against the questions of its document: %w", kind, id, err)
	}
	return true, tx.Commit(ctx)
}

// SourceText is the text one reference source writes, with the time it was written, taking no
// lock: what a drain reads to decide whether that text has anything a re-index would move. found
// is false for a source that is gone, a document with no markdown version and a direct message.
func SourceText(ctx context.Context, q Queryer, kind, id string) (body string, writtenAt time.Time, found bool, err error) {
	return sourceText(ctx, q, kind, id)
}

// HoldsQuestionCandidate reports whether body writes a `#id` the whole grammar would resolve
// against the question map of the document its text belongs to. The grammar itself answers it:
// text.Mentions asks a resolver for that map only when the body holds such a candidate, so a
// resolver that answers nothing and records the question tells which bodies are worth a re-index
// when a document's question blocks move, and no second reading of the grammar can drift from it.
func HoldsQuestionCandidate(body, serverURL string) bool {
	probe := &questionProbe{}
	// A probe that resolves nothing fails nothing: text.Mentions returns an error only from the
	// resolver, and this one answers every call.
	_, _ = text.Mentions(body, serverURL, probe)
	return probe.asked
}

// questionProbe is the text.Resolver HoldsQuestionCandidate reads a body with: it resolves no
// key, no `#id` and no short id, and records that the grammar asked for the question map.
type questionProbe struct {
	asked bool
}

func (p *questionProbe) Issues(keys []string) (map[string]bool, error) { return nil, nil }

func (p *questionProbe) Questions() (map[string]text.Ref, error) {
	p.asked = true
	return nil, nil
}

func (p *questionProbe) Items(prefixes []text.Prefix) (map[int]string, error) { return nil, nil }
