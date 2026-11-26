package graphcensus

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/asks"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The asks and their followers. An ask's node holds the documents it is
// anchored to, owned by and part of, who asked it, its answer and the sessions that follow it, and
// asks and ask_followers are the projection of those tuples from the moment a row names its node
// (20261116094058's node_kind and node_address). A row a build before this release wrote names
// none, and graph_edges' old arms answer it until these two passes state the row on the node and
// mark it, on the store's backfill contract: each walks only the rows its marker does not mark,
// states every fact of a row in the transaction that marks it, and reads no marked row, so a run
// after another, or beside a live server, does only the rows still owed. They are the boot step's
// first phase after the object nodes (docs' decision step), and `backfill-graph-store` runs them
// too.
//
// Each batch takes the store's lock order (api/inbox_read.go's markInboxReadThreads): it reads
// what it takes without a lock, then takes the owner row of every ask of the batch, each once and
// in ascending owner key (lockAskOwners), and only then locks its rows, in key order, and reads
// them again under those locks, leaving out a row marked since. A writer of an ask's answer or of
// its followers takes the ask's owner row before the ask's own rows, and settlement takes it
// before the room lock, the document's ask rows and the nodes of their authors (createAskBlock),
// so a batch that locked its asks rows first, in id order and across owners, could hold an
// approval ask a settlement of its issue goes on to answer while it waited for the node of an
// author that settlement held, or hold one ask of a document and wait for another that a
// settlement took in its own order. Holding the owner first, the batch waits for such a writer
// holding nothing it wants, or the writer waits for it.

// askRowsOwed is the asks rows the asks pass states (writeAskBatch): each row that names no node,
// whose node the store holds, and none of whose three documents' addresses is past the store's
// bound - such a row is left out whole, as a comment is (graphstore.AskPlaces.Fit). It is the from
// and the where every read of those rows shares - the batch's read of what it takes
// (askBatchRead), its read again under its locks (askRowsRead), the count of what is left and the
// probe of whether any is owed (AsksOwed) - over asks k and its three documents, anchor, owner and
// document.
var askRowsOwed = `
	from asks k
	join graph_nodes node on node.kind = 'ask' and node.id = k.id::text
	left join artifacts anchor on anchor.id = coalesce(k.block_artifact_id, (k.anchor->>'artifact_id')::uuid)
	left join artifacts owner on owner.id = k.artifact_id
	left join artifacts document on document.id = k.block_artifact_id
	where k.node_kind is null
	  and octet_length(coalesce(anchor.ref_key, '')) <= ` + addressBound + `
	  and octet_length(coalesce(owner.ref_key, '')) <= ` + addressBound + `
	  and octet_length(coalesce(document.ref_key, '')) <= ` + addressBound

// askBatchRead is one batch of the asks rows the asks pass owes (askRowsOwed), after the id $1 and
// at most $2 of them, each with its owner: its issue, or the document an ask on no issue belongs to
// (asks_one_owner). It locks nothing, and a follower's column is empty: writeAskBatch takes the
// owners' rows, then reads the rows again under them (askRowsRead).
var askBatchRead = `
	select k.id::text, '', coalesce(k.issue_key, ''), coalesce(k.artifact_id::text, '')` + askRowsOwed + `
	  and k.id > $1::uuid
	order by k.id
	limit $2`

// askRowsLock locks the rows of the batch $1 that still name no node, in id order as an update of
// each row locks it: an answer this release writes on such a row (asks.Settle) takes its row the
// same way, so the batch reads the answer it marks, and an answer written after it finds the row
// marked and states itself on the node. It names asks alone: a statement that also names an owner
// table takes no row lock (api's TestNoForUpdateOnOwnerTables), so the read of what the batch
// states, which joins the asks' documents, runs plainly after it (askRowsRead).
var askRowsLock = `
	select k.id from asks k
	where k.id = any($1::uuid[]) and k.node_kind is null
	order by k.id
	for no key update of k`

// askRowsRead reads again, under the owner locks writeAskBatch took and the row locks of
// askRowsLock, the rows of the batch $1 the pass still owes (askRowsOwed). Each row answers its
// author, created_at, answer state, answer and resolution, the addresses of its three documents
// (graphstore.AskPlaces), the ask.opened that recorded it and the newest event that wrote its
// answer state, each $2 where none is found.
var askRowsRead = `
	select k.id::text, k.author, k.created_at, k.state, k.answer, k.resolution,
	       coalesce(anchor.ref_key, ''), coalesce(owner.ref_key, ''), coalesce(document.ref_key, ''),
	       coalesce(k.block_id, ''),
	       coalesce((select e.id from events e
	                 where e.payload->>'id' = k.id::text and e.type = 'ask.opened'
	                 order by e.id limit 1), $2),
	       coalesce((select e.id from events e
	                 where e.payload->>'id' = k.id::text and e.type in ('ask.opened', 'ask.answered', 'ask.resolved')
	                 order by e.id desc limit 1), $2)` + askRowsOwed + `
	  and k.id = any($1::uuid[])
	order by k.id`

// askBatchRow is one row of a batch the asks or the follower pass read without a lock
// (askBatchRead, askFollowerBatchRead): the ask, the session that follows it for a follower row,
// and the ask's owner - its issue, or the document an ask on no issue belongs to - which is the
// owner a follower row stands under too.
type askBatchRow struct {
	askID, session  string
	issue, document string
}

// readAskBatch runs read, a pass's read of one batch that locks nothing, on the pool with args.
func readAskBatch(ctx context.Context, database *store.Store, read string, args ...any) ([]askBatchRow, error) {
	rows, err := database.Pool.Query(ctx, read, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (askBatchRow, error) {
		var batched askBatchRow
		err := row.Scan(&batched.askID, &batched.session, &batched.issue, &batched.document)
		return batched, err
	})
}

// lockAskOwners takes the owner row of every ask of batch, each once, in ascending owner key
// (events.LockOwnerRows), the one order over owner rows, which lockBlockDocuments takes a block
// batch's owners in: a batch over the asks of two issues takes the two issues' rows in key order
// whatever order its asks' ids put them in. An owner gone since the batch was read locks nothing,
// and the read under the locks leaves out its rows, which went with it.
func lockAskOwners(ctx context.Context, tx pgx.Tx, batch []askBatchRow) error {
	owners := events.OwnerRows{}
	for _, row := range batch {
		owners.Add(row.issue, row.document)
	}
	return events.LockOwnerRows(ctx, tx, owners)
}

// backfillAsks states each asks row that names no node on the ask's node and marks the row: the
// documents it is anchored to, owned by and part of and who asked it (graphstore.AskFacts), dated
// at the row's created_at under the ask.opened that recorded it, and its answer state
// (asks.AnswerFacts), dated at the answer's or the resolution's own `at` - the row's created_at for
// an open ask - under the newest event that wrote it, each written by the person the row records
// beside it. An ask whose node the store does not hold yet is left for the object pass that writes
// it (backfillObjectNodes), which runs before this.
func backfillAsks(ctx context.Context, database *store.Store) (KindBackfill, error) {
	done := KindBackfill{Kind: "asks"}
	after := zeroUUID
	for {
		batch, err := readAskBatch(ctx, database, askBatchRead, after, backfillBatch)
		if err != nil {
			return done, fmt.Errorf("read the asks naming no node: %w", err)
		}
		if len(batch) == 0 {
			break
		}
		after = batch[len(batch)-1].askID
		rows, nodes, tuples, err := writeAskBatch(ctx, database, batch)
		if err != nil {
			return done, err
		}
		done.Rows += rows
		done.Nodes += nodes
		done.Tuples += tuples
		if len(batch) < backfillBatch {
			break
		}
	}
	if err := database.Pool.QueryRow(ctx, `select count(*) `+askRowsOwed).Scan(&done.Remaining); err != nil {
		return done, fmt.Errorf("count the asks still naming no node: %w", err)
	}
	done.Complete = done.Remaining == 0
	return done, nil
}

// writeAskBatch takes the owner locks of batch's asks (lockAskOwners), then locks the rows of
// batch still owed (askRowsLock), reads them again (askRowsRead) and states them, in one
// transaction, and answers how many rows it marked and the nodes and the tuples it stated as the
// upper bound the report states. Every node of the batch - the authors' and the answerers' - goes
// in its one store write before the first tuple, and the rows are marked after both, as
// writeCommentBatch orders its write. The ask nodes themselves are not written again: the object
// pass wrote each, and this states only the facts the row projects.
func writeAskBatch(ctx context.Context, database *store.Store, batch []askBatchRow) (int64, int64, int64, error) {
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("begin an ask batch: %w", err)
	}
	defer tx.Rollback(ctx)
	if err := lockAskOwners(ctx, tx, batch); err != nil {
		return 0, 0, 0, err
	}
	read := make([]string, len(batch))
	for index, row := range batch {
		read[index] = row.askID
	}
	if _, err := tx.Exec(ctx, askRowsLock, read); err != nil {
		return 0, 0, 0, fmt.Errorf("lock the asks naming no node: %w", err)
	}
	rows, err := tx.Query(ctx, askRowsRead, read, graphstore.NoEvent)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("read the asks naming no node: %w", err)
	}
	var ids []string
	var nodes []graphstore.Node
	var tuples []graphstore.Tuple
	written := map[[2]string]struct{}{}
	addNodes := func(more []graphstore.Node) {
		for _, node := range more {
			if _, made := written[[2]string{node.Kind, node.ID}]; !made {
				written[[2]string{node.Kind, node.ID}] = struct{}{}
				nodes = append(nodes, node)
			}
		}
	}
	for rows.Next() {
		var id, state, blockID string
		var authorJSON, answerJSON, resolutionJSON []byte
		var createdAt time.Time
		var places graphstore.AskPlaces
		var opened, settled int64
		if err := rows.Scan(&id, &authorJSON, &createdAt, &state, &answerJSON, &resolutionJSON,
			&places.Anchor, &places.Owner, &places.Document, &blockID, &opened, &settled); err != nil {
			rows.Close()
			return 0, 0, 0, fmt.Errorf("read an ask naming no node: %w", err)
		}
		var author model.Actor
		if err := json.Unmarshal(authorJSON, &author); err != nil {
			rows.Close()
			return 0, 0, 0, fmt.Errorf("read the author of ask %s: %w", id, err)
		}
		var answer *model.AskAnswer
		if len(answerJSON) > 0 {
			answer = &model.AskAnswer{}
			if err := json.Unmarshal(answerJSON, answer); err != nil {
				rows.Close()
				return 0, 0, 0, fmt.Errorf("read the answer of ask %s: %w", id, err)
			}
		}
		var resolution *model.AskResolution
		if len(resolutionJSON) > 0 {
			resolution = &model.AskResolution{}
			if err := json.Unmarshal(resolutionJSON, resolution); err != nil {
				rows.Close()
				return 0, 0, 0, fmt.Errorf("read the resolution of ask %s: %w", id, err)
			}
		}
		// The ask's node is the object pass's, written already; its facts are what this states.
		askNodes, facts, _ := graphstore.AskFacts(id, "", nil, author, createdAt, opened, places.On(blockID))
		addNodes(askNodes[1:])
		tuples = append(tuples, facts...)
		who, at := backfillActor, createdAt
		switch {
		case state == asks.StateResolved && resolution != nil:
			who, at = resolution.Actor, resolution.At
		case answer != nil:
			who, at = model.Actor{Kind: "user", ID: answer.User, Act: answer.Act}, answer.At
		}
		answerNodes, answerFacts, err := asks.AnswerFacts(id, state, answer, resolution, who, at, settled)
		if err != nil {
			rows.Close()
			return 0, 0, 0, err
		}
		addNodes(answerNodes)
		tuples = append(tuples, answerFacts...)
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, 0, 0, fmt.Errorf("read the asks naming no node: %w", err)
	}
	if len(ids) == 0 {
		return 0, 0, 0, nil
	}
	if err := store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
		graphstore.QueueWrite(b, nodes, tuples)
		b.Queue("name the nodes of an ask batch on their rows", `
			update asks set node_kind = $2, node_address = id::text
			where id = any($1::uuid[]) and node_kind is null
		`, ids, graphstore.KindAsk)
		graphstore.QueueHeads(b, graphstore.HeadsOf(nodes, tuples))
		return nil
	}); err != nil {
		return 0, 0, 0, fmt.Errorf("state a batch of asks: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, 0, 0, fmt.Errorf("commit an ask batch: %w", err)
	}
	return int64(len(ids)), int64(len(nodes)), int64(len(tuples)) + 2*int64(len(nodes)), nil
}

// askFollowerRowsOwed is the ask_followers rows the follower pass states (writeAskFollowerBatch):
// each row that names no node, of an ask whose node the store holds and a session the store can
// key. It is the from and the where every read of those rows shares, as askRowsOwed is the asks
// pass's, over ask_followers f and the ask k it follows.
var askFollowerRowsOwed = `
	from ask_followers f
	join asks k on k.id = f.ask_id
	join graph_nodes node on node.kind = 'ask' and node.id = f.ask_id::text
	where f.node_kind is null and octet_length(f.session_id) <= ` + addressBound

// askFollowerBatchRead is one batch of the follower rows the follower pass owes
// (askFollowerRowsOwed), after the pair ($1, $2) and at most $3 of them, each with its ask's owner,
// which a follower row stands under: the follow route takes it before the row (api's
// changeAskFollower). It locks nothing, as askBatchRead locks nothing.
var askFollowerBatchRead = `
	select f.ask_id::text, f.session_id, coalesce(k.issue_key, ''), coalesce(k.artifact_id::text, '')` + askFollowerRowsOwed + `
	  and (f.ask_id, f.session_id) > ($1::uuid, $2::text)
	order by f.ask_id, f.session_id
	limit $3`

// askFollowerRowsRead reads again, under the owner rows writeAskFollowerBatch took, the follower
// rows of the batch of pairs ($1, $2) the pass still owes (askFollowerRowsOwed), locked in key
// order as an update locks each, as askRowsLock locks the asks.
var askFollowerRowsRead = `
	select f.ask_id::text, f.session_id, f.since` + askFollowerRowsOwed + `
	  and (f.ask_id, f.session_id) in (select * from unnest($1::uuid[], $2::text[]))
	order by f.ask_id, f.session_id
	for no key update of f`

// backfillAskFollowers states each ask_followers row that names no node as the ask node's
// followed_by to the session, dated at the row's since, and marks the row; the session is a node
// of the store from the same write on, as a follow makes it (asks.Follow). No row records who
// added a follower, so the writer is the run (backfillActor), and no event is found for a row, so
// the cause is the sentinel. A row of an ask the store holds no node of is left for the object
// pass, and one of a session past the store's bound is no relation the store can key.
func backfillAskFollowers(ctx context.Context, database *store.Store) (KindBackfill, error) {
	done := KindBackfill{Kind: "ask_followers"}
	after := [2]string{zeroUUID, ""}
	for {
		batch, err := readAskBatch(ctx, database, askFollowerBatchRead, after[0], after[1], backfillBatch)
		if err != nil {
			return done, fmt.Errorf("read the ask followers naming no node: %w", err)
		}
		if len(batch) == 0 {
			break
		}
		last := batch[len(batch)-1]
		after = [2]string{last.askID, last.session}
		rows, nodes, tuples, err := writeAskFollowerBatch(ctx, database, batch)
		if err != nil {
			return done, err
		}
		done.Rows += rows
		done.Nodes += nodes
		done.Tuples += tuples
		if len(batch) < backfillBatch {
			break
		}
	}
	if err := database.Pool.QueryRow(ctx, `select count(*) `+askFollowerRowsOwed).Scan(&done.Remaining); err != nil {
		return done, fmt.Errorf("count the ask followers still naming no node: %w", err)
	}
	done.Complete = done.Remaining == 0
	return done, nil
}

// writeAskFollowerBatch takes the owner rows of batch's asks (lockAskOwners), then reads again and
// locks the follower rows of batch still owed (askFollowerRowsRead) and states them, in one
// transaction - the sessions' nodes, then each followed_by, then the marks - and answers how many
// rows it marked and the nodes and the tuples it stated as the upper bound the report states.
func writeAskFollowerBatch(ctx context.Context, database *store.Store, batch []askBatchRow) (int64, int64, int64, error) {
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("begin an ask follower batch: %w", err)
	}
	defer tx.Rollback(ctx)
	if err := lockAskOwners(ctx, tx, batch); err != nil {
		return 0, 0, 0, err
	}
	readAsks := make([]string, len(batch))
	readSessions := make([]string, len(batch))
	for index, row := range batch {
		readAsks[index], readSessions[index] = row.askID, row.session
	}
	rows, err := tx.Query(ctx, askFollowerRowsRead, readAsks, readSessions)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("read the ask followers naming no node: %w", err)
	}
	var askIDs, sessions []string
	var nodes []graphstore.Node
	var tuples []graphstore.Tuple
	made := map[string]struct{}{}
	for rows.Next() {
		var askID, session string
		var since time.Time
		if err := rows.Scan(&askID, &session, &since); err != nil {
			rows.Close()
			return 0, 0, 0, fmt.Errorf("read an ask follower naming no node: %w", err)
		}
		if _, ok := made[session]; !ok {
			made[session] = struct{}{}
			nodes = append(nodes, graphstore.SessionNode(session, backfillActor, since, graphstore.NoEvent))
		}
		tuples = append(tuples, graphstore.Tuple{
			FromKind: graphstore.KindAsk, FromID: askID, Kind: graphstore.TupleFollowedBy,
			ToKind: graphstore.KindSession, ToID: session,
			At: since, Who: backfillActor, SourceEvent: graphstore.NoEvent, Asserted: true,
		})
		askIDs = append(askIDs, askID)
		sessions = append(sessions, session)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, 0, 0, fmt.Errorf("read the ask followers naming no node: %w", err)
	}
	if len(askIDs) == 0 {
		return 0, 0, 0, nil
	}
	if err := store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
		graphstore.QueueWrite(b, nodes, tuples)
		b.Queue("name the nodes of an ask follower batch on their rows", `
			update ask_followers f set node_kind = $3, node_address = p.ask_id
			from unnest($1::text[], $2::text[]) as p (ask_id, session_id)
			where f.ask_id = p.ask_id::uuid and f.session_id = p.session_id and f.node_kind is null
		`, askIDs, sessions, graphstore.KindAsk)
		graphstore.QueueHeads(b, graphstore.HeadsOf(nodes, tuples))
		return nil
	}); err != nil {
		return 0, 0, 0, fmt.Errorf("state a batch of ask followers: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, 0, 0, fmt.Errorf("commit an ask follower batch: %w", err)
	}
	return int64(len(askIDs)), int64(len(nodes)), int64(len(tuples)) + 2*int64(len(nodes)), nil
}

// BackfillAsks is the boot step's first phase (docs' decision step): the object nodes of every
// issue, document, ask and message written before their writers wrote one (backfillObjectNodes),
// so every ask a build before this release wrote has its node before a mark names it, then the
// asks and the ask_followers rows that name no node, each in committed batches of backfillBatch.
// It answers what each pass did; an error stops it where it stands, and every batch committed
// before it stays.
//
// kept for one release: decisions-step
func BackfillAsks(ctx context.Context, database *store.Store) ([]KindBackfill, error) {
	done, err := backfillObjectNodes(ctx, database)
	if err != nil {
		return done, err
	}
	for _, write := range decisionBackfills {
		report, err := write(ctx, database)
		done = append(done, report...)
		if err != nil {
			return done, err
		}
	}
	return done, nil
}

// AsksOwed reports whether BackfillAsks has anything to do: a row of an object kind that names no
// node, or an asks or ask_followers row the passes would mark. Each read stops at its first owed
// row, so a store that owes one costs little, but a store that owes nothing is read to the end: an
// anti-join of issues, artifacts, asks and messages to graph_nodes, then asks and ask_followers,
// which no index keys on a null marker. The decisions step reads it only where another build can
// have written such a row (docs' decision step, probe).
//
// kept for one release: decisions-step
func AsksOwed(ctx context.Context, q graphstore.Queryer) (bool, error) {
	for _, kind := range objectKinds {
		var owed bool
		if err := q.QueryRow(ctx, `select exists (`+kind.read+`)`, kind.start, 1).Scan(&owed); err != nil {
			return false, fmt.Errorf("read whether a %s row names no node: %w", kind.kind, err)
		}
		if owed {
			return true, nil
		}
	}
	var owed bool
	if err := q.QueryRow(ctx, `
		select exists (select 1 `+askRowsOwed+`) or exists (select 1 `+askFollowerRowsOwed+`)
	`).Scan(&owed); err != nil {
		return false, fmt.Errorf("read whether an ask or a follower names no node: %w", err)
	}
	return owed, nil
}
