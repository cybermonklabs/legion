package graphcensus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/inboxwrite"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The backfill of every kind the one inbox moves into the store, by the node store's contract (R9's
// view end): each pass walks only the rows its marker
// test passes, which are the rows a build of the release before this one wrote during a roll or
// before it; it dates each tuple by the row's own created_at, or by the moment of the mark the row
// stands for where the row holds none; it writes each tuple under the older write's own event,
// found in the log as the node store's passes find a row's event (readCommentRows), and under the
// store's sentinel where the log holds none; and it sets the row's marker in the transaction that
// states its tuples, so a second run reads no row a first one marked. It reads no column of a
// marked row, so it is no reconcile.
//
// Each pass states exactly what this release's writer of the row states for it, through the same
// writer's own statement of the facts: a message's pointers, its author and the page it hangs
// from (graphstore.MessageFacts, graphstore.AuthorFacts), a comment's pointers
// (graphstore.CommentPointerFacts), the delivery routes a comment names (as api's
// commentMentionFacts states them), an item's blocks, their part_of and their links and its
// citations (as refs' item index states them), an attempt to reach a session or a role
// (graphstore.AttemptNode, graphstore.AttemptFactsAsWritten), each session or role an older
// build's outbox published a comment or a message to (as outbox's deliveredGraph states it), what
// each comment, message, ask, answer and decline an older build wrote owed the threads it was
// written in (inboxwrite.RecordOlder), the identities a comment's or an issue message's text names,
// as refs' item index states their mentions (refs.ItemIdentityMentions; that build stated none and
// stored no row of a person's, refs.TargetOf, so the item's text is read for them), what each
// read-state row says (graphstore.ToPerson, graphstore.FromPerson), and what each write of that
// build that changes no
// row's marker owes - a move of who an ask waits on, an edit of a comment - read off the queue of
// its events (inboxEventsPass, backfill_inbox_events.go). Every batch takes the order the live
// writers take - the owner rows, then
// the project rows an ask's waiting person is read under, then the item rows and its block state,
// then the item's own store writes, then a thread's user_threads rows in ascending login, then the
// deliveries to them - so a live write of this release and a batch over one thread take turns.
//
// The order of the passes is the order their facts hang on each other: every item's node and
// author before its pointers (the comment, object-node and ask passes run
// first), the pointers before the deliveries that walk them, each
// write's deliveries before the user_threads row they reach is marked, so each lands at or below
// that row's marked_seq and R6's equation for version holds on it.

// noEvent is graphstore.NoEvent as the SQL of the passes below spells it.
var noEvent = strconv.FormatInt(graphstore.NoEvent, 10)

// commentEventOf is the event that stored the comment alias names: its comment.created, or the
// comment.answered of a session's reply to a delivery, found under its owner as readCommentRows
// finds it, and the sentinel where the log holds neither.
func commentEventOf(alias string) string {
	return `coalesce((
		select e.id from events e
		where coalesce(e.issue_key, e.artifact_id::text, e.project_key) = coalesce(` + alias + `.issue_key, ` + alias + `.artifact_id::text)
		  and e.type in ('comment.created', 'comment.answered')
		  and e.payload->>'id' = ` + alias + `.id::text
		order by e.id limit 1), ` + noEvent + `)`
}

// messageEventOf is the event that stored the message alias names: its message.created, or the
// message.answered of a reply, under its issue, or under no owner for a message on no issue, and
// the sentinel where the log holds neither.
func messageEventOf(alias string) string {
	return `coalesce((
		select e.id from events e
		where coalesce(e.issue_key, e.artifact_id::text, e.project_key) = ` + alias + `.issue_key
		  and e.type in ('message.created', 'message.answered') and e.payload->>'id' = ` + alias + `.id::text
		order by e.id limit 1), (
		select e.id from events e
		where ` + alias + `.issue_key is null and coalesce(e.issue_key, e.artifact_id::text, e.project_key) is null
		  and e.type in ('message.created', 'message.answered') and e.payload->>'id' = ` + alias + `.id::text
		order by e.id limit 1), ` + noEvent + `)`
}

// askOpenedOf is the ask.opened that recorded the ask alias names, and the sentinel where the log
// holds none.
func askOpenedOf(alias string) string {
	return `coalesce((
		select e.id from events e
		where e.type in (` + inboxwrite.AskEventTypes + `) and e.type = 'ask.opened' and e.payload->>'id' = ` + alias + `.id::text
		order by e.id limit 1), ` + noEvent + `)`
}

// inboxPass is one pass of the inbox backfill: the kind it reports, the read of one batch of the
// keys of the rows it owes after the last key read - $1 to $n the key, $n+1 the batch's size - each
// key column read as text, the key the walk starts after, the write of one batch inside its
// transaction, and the count of the rows it still owes.
type inboxPass struct {
	kind      string
	batch     string
	start     []string
	write     func(ctx context.Context, tx pgx.Tx, keys [][]string) (inboxWrote, error)
	remaining string
}

// inboxWrote is what one batch wrote: the rows it marked, and the nodes and tuples it handed the
// store, the upper bound the report states (a node already there and a tuple a repeat key keeps
// once move nothing).
type inboxWrote struct {
	rows, nodes, tuples int64
}

func (w *inboxWrote) add(more inboxWrote) {
	w.rows += more.rows
	w.nodes += more.nodes
	w.tuples += more.tuples
}

// runInboxPass walks pass's owed rows in batches of backfillBatch, each written in a transaction
// of its own, and answers what it did and how many rows it still owes. After each batch it hands
// growth what the batch wrote, which takes the store's statistics again once the store has grown
// past them.
func runInboxPass(ctx context.Context, database *store.Store, pass inboxPass, growth *storeGrowth) (KindBackfill, error) {
	done := KindBackfill{Kind: pass.kind}
	after := slices.Clone(pass.start)
	for {
		keys, err := readKeys(ctx, database.Pool, len(after), pass.batch, batchArgs(after)...)
		if err != nil {
			return done, fmt.Errorf("read the %s rows the backfill owes: %w", pass.kind, err)
		}
		if len(keys) == 0 {
			break
		}
		after = keys[len(keys)-1]
		var wrote inboxWrote
		if err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
			var err error
			wrote, err = pass.write(ctx, tx, keys)
			return err
		}); err != nil {
			return done, fmt.Errorf("write a batch of %s: %w", pass.kind, err)
		}
		done.Rows += wrote.rows
		done.Nodes += wrote.nodes
		done.Tuples += wrote.tuples
		if err := growth.wrote(ctx, database, wrote); err != nil {
			return done, err
		}
		if len(keys) < backfillBatch {
			break
		}
	}
	if err := database.Pool.QueryRow(ctx, `select count(*) `+pass.remaining).Scan(&done.Remaining); err != nil {
		return done, fmt.Errorf("count the %s rows the backfill still owes: %w", pass.kind, err)
	}
	done.Complete = done.Remaining == 0
	return done, nil
}

// batchArgs is the arguments of a pass's read of one batch (inboxPass.batch): the key the batch
// starts after, then the batch's size.
func batchArgs(after []string) []any {
	args := make([]any, 0, len(after)+1)
	for _, key := range after {
		args = append(args, key)
	}
	return append(args, backfillBatch)
}

// column is the column-th of each key.
func column(keys [][]string, index int) []string {
	values := make([]string, len(keys))
	for at, key := range keys {
		values[at] = key[index]
	}
	return values
}

// backfillInbox runs the inbox backfill's passes in the order their facts hang on each other, and
// answers what each did, once it has taken the statistics of every table they walk
// (analyzeInboxTables), and taking the store's again as the passes grow it (storeGrowth).
func backfillInbox(ctx context.Context, database *store.Store) ([]KindBackfill, error) {
	if err := analyzeInboxTables(ctx, database); err != nil {
		return nil, err
	}
	growth := &storeGrowth{}
	if err := growth.read(ctx, database); err != nil {
		return nil, err
	}
	var done []KindBackfill
	for _, pass := range inboxPasses() {
		report, err := runInboxPass(ctx, database, pass, growth)
		done = append(done, report)
		if err != nil {
			return done, err
		}
	}
	return done, nil
}

// inboxPasses is the inbox backfill's passes in the order backfillInbox runs them, the order their
// facts hang on each other. The pass over the queue of an older build's events (inboxEventsPass)
// follows the asks pass and the comment pointer pass, whose rows its moves and its edits are held
// to. The block identity pass (blockIdentitiesPass) comes last: it states nothing, and marks the
// documents whose blocks' identity mentions the block index owes.
func inboxPasses() []inboxPass {
	passes := []inboxPass{messagesPass(), commentPointersPass(), commentMentionsPass(), pastBoundRowsPass(pastBoundComments),
		pastBoundRowsPass(pastBoundMessages)}
	for _, kind := range inboxItemKinds {
		passes = append(passes, itemBlocksPass(kind), itemLinksPass(kind), itemCitationsPass(kind))
	}
	passes = append(passes, attemptsPass(commentAttempts), attemptsPass(messageAttempts), publishedDeliveriesPass(),
		asksOwedPass(), declinesPass(), inboxEventsPass(), userThreadsPass())
	for _, table := range readStateTables {
		passes = append(passes, readStatePass(table))
	}
	return append(passes, blockIdentitiesPass())
}

// inboxTables is every table the inbox backfill's passes read or write: the rows each pass walks,
// with its item and block tables, the store's two tables, the log each row's event is read from,
// the queues of the events an older build's outbox published (events_published_owed) and of the
// events of its writes no row's marker records (events_inbox_owed), the owner rows a batch takes
// and an ask's waiting person is read under, and a document's block index the block identity pass
// reads.
func inboxTables() []string {
	tables := []string{"messages", "comments", "comment_mentions", "asks", "refs", "user_threads", "graph_nodes", "graph_tuples",
		"events", "events_published_owed", "events_inbox_owed", "issues", "artifacts", "projects",
		"document_block_versions", "document_blocks", "document_block_texts"}
	for _, kind := range inboxItemKinds {
		tables = append(tables, kind.blocks, kind.links, kind.state)
	}
	for _, table := range []attemptTable{commentAttempts, messageAttempts} {
		tables = append(tables, table.table)
	}
	for _, table := range readStateTables {
		tables = append(tables, table.table)
	}
	return tables
}

// analyzeInboxTables takes the statistics of every table the passes walk (inboxTables), in one
// ANALYZE, before the first pass reads any of them. Each pass tests a marker column this
// release's migrations add, and a table holds no statistics of a column until something analyzes
// it, so on a database nothing has analyzed since the deploy Postgres plans each test at its
// defaults, half a percent of the rows for `node_kind is null` where every message is unmarked.
// The walk of a message thread is planned the same whatever that estimate (inboxwrite.RecordOlder's
// reachedBefore), but the pass's other statements are not: over 3,000 messages no analyze had
// read, the backfill read 207 thousand rows out of messages without this and 70 thousand with
// it (TestTheMessagesPassIsLinearOnATableNoAnalyzeHasRead). The command runs on the database URL
// the server migrates the schema with, whose login owns these tables and so may analyze them;
// Postgres skips a table its login may not analyze with a warning, and the pass then plans on
// whatever statistics the table holds.
func analyzeInboxTables(ctx context.Context, database *store.Store) error {
	tables := inboxTables()
	named := make([]string, len(tables))
	for index, table := range tables {
		named[index] = pgx.Identifier{table}.Sanitize()
	}
	if _, err := database.Pool.Exec(ctx, `analyze `+strings.Join(named, ", ")); err != nil {
		return fmt.Errorf("analyze the tables the backfill walks: %w", err)
	}
	return nil
}

// storeGrowth is how far the store has grown since the backfill last took its statistics: the
// rows graph_tuples held then, and the nodes and tuples the passes have handed the store since.
// The passes write the store as they read it, and each step of a thread walk and each person a
// write reaches is a probe of graph_tuples or graph_nodes planned on those statistics, each
// meant as a seek of one read index by the address it names: graph_tuples_current or
// graph_tuples_as_of_out out of a node, graph_tuples_as_of_in into a person. Taken once, while the
// store held a few thousand tuples, the statistics left those probes planned for that small store:
// over 3,000 messages the backfill read 805 rows out of graph_tuples a message, where with its
// statistics taken again as the store grew it read 24
// (TestTheMessagesPassIsLinearOnATableNoAnalyzeHasRead). Autovacuum takes them again once a table
// has grown by a tenth, but on its own schedule, which a pass may outrun, so the backfill takes
// them itself at that same growth.
type storeGrowth struct {
	rows, grown int64
}

// storeGrowthFloor is the growth below which the store's statistics are not taken again however
// small the store, as autovacuum's own threshold has a floor: a store of a few rows would
// otherwise be analyzed after every batch.
const storeGrowthFloor = 1000

// read takes how many rows graph_tuples holds by its statistics, which the analyze before it
// has just taken, and starts the count of what was written since.
func (g *storeGrowth) read(ctx context.Context, database *store.Store) error {
	if err := database.Pool.QueryRow(ctx, `
		select greatest(reltuples, 0)::bigint from pg_class where oid = 'graph_tuples'::regclass
	`).Scan(&g.rows); err != nil {
		return fmt.Errorf("read the size of the store: %w", err)
	}
	g.grown = 0
	return nil
}

// wrote counts what one batch handed the store, and once the store has grown by a tenth of the
// rows it held when its statistics were last taken, past storeGrowthFloor, takes them again.
func (g *storeGrowth) wrote(ctx context.Context, database *store.Store, wrote inboxWrote) error {
	g.grown += wrote.nodes + wrote.tuples
	if g.grown < storeGrowthFloor+g.rows/10 {
		return nil
	}
	if _, err := database.Pool.Exec(ctx, `analyze graph_tuples, graph_nodes`); err != nil {
		return fmt.Errorf("analyze the store the backfill has grown: %w", err)
	}
	return g.read(ctx, database)
}

// lockOwnersOf takes the owner row of each row read names `for no key update`, in ascending owner
// key (events.LockOwnerRows), where read is as readOwnersInto reads it, as every writer of these
// rows takes its owner first.
func lockOwnersOf(ctx context.Context, tx pgx.Tx, read string, ids []string) error {
	owners := events.OwnerRows{}
	if err := readOwnersInto(ctx, tx, owners, read, ids); err != nil {
		return err
	}
	return events.LockOwnerRows(ctx, tx, owners)
}

// readOwnersInto adds to owners the owner of each row read names, where read answers (id, issue,
// document) for the ids $1 - the issue that owns a row, or the document no issue owns. With no ids
// there is no row to read, and it sends no statement.
func readOwnersInto(ctx context.Context, tx pgx.Tx, owners events.OwnerRows, read string, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	rows, err := tx.Query(ctx, read, ids)
	if err != nil {
		return fmt.Errorf("read the owners of a batch: %w", err)
	}
	var id, issue, document string
	if _, err := pgx.ForEachRow(rows, []any{&id, &issue, &document}, func() error {
		owners.Add(issue, document)
		return nil
	}); err != nil {
		return fmt.Errorf("read the owners of a batch: %w", err)
	}
	return nil
}

// The owner each item kind's row stands under, as (id, issue, document) for the ids $1: a comment's
// issue or the document it is on, and the issue of that document, a message's issue, and an ask's
// issue or document (asks_one_owner).
const (
	commentOwners = `
		select c.id::text, coalesce(c.issue_key, a.issue_key, ''),
		       case when coalesce(c.issue_key, a.issue_key) is null then coalesce(c.artifact_id::text, '') else '' end
		from comments c left join artifacts a on a.id = c.artifact_id
		where c.id = any($1::uuid[])`
	messageOwners = `
		select m.id::text, coalesce(m.issue_key, ''), '' from messages m where m.id = any($1::uuid[])`
	askOwners = `
		select k.id::text, coalesce(k.issue_key, a.issue_key, ''),
		       case when coalesce(k.issue_key, a.issue_key) is null then coalesce(k.artifact_id::text, '') else '' end
		from asks k left join artifacts a on a.id = k.artifact_id
		where k.id = any($1::uuid[])`
)

// writeNodesOnce is nodes with each (kind, id) once, the first of each kept: one store write
// writes a node once (graphstore.Write).
func writeNodesOnce(nodes []graphstore.Node) []graphstore.Node {
	seen := map[[2]string]struct{}{}
	kept := nodes[:0:0]
	for _, node := range nodes {
		if _, held := seen[[2]string{node.Kind, node.ID}]; held {
			continue
		}
		seen[[2]string{node.Kind, node.ID}] = struct{}{}
		kept = append(kept, node)
	}
	return kept
}

// writeRelations appends tuples in tx: those that name a person on the end ToPerson keeps them to
// through graphstore.ToPerson, and every other through graphstore.Write, which refuses a person
// tuple handed to it (personTuples).
func writeRelations(ctx context.Context, tx pgx.Tx, tuples []graphstore.Tuple) error {
	people, others := personTuples(tuples)
	if len(others) > 0 {
		if err := graphstore.Write(ctx, tx, nil, others); err != nil {
			return err
		}
	}
	return graphstore.ToPerson(ctx, tx, people)
}

// personTuples parts tuples into those of the three kinds graphstore.ToPerson alone appends - a
// mentions or a delivered_to to a person, and a person's participates_in (graphstore.NamesPerson) -
// and every other.
func personTuples(tuples []graphstore.Tuple) (people, others []graphstore.Tuple) {
	for _, tuple := range tuples {
		if graphstore.NamesPerson(tuple) {
			people = append(people, tuple)
		} else {
			others = append(others, tuple)
		}
	}
	return people, others
}

// --- messages ---------------------------------------------------------------------------------

// messagesOwed is the messages rows the messages pass owes: each that names no node (the node
// pair, migration 20261116114515), whose node the store holds (the object pass's), and whose
// replies_to ends the store can key (graphstore.MessageFacts leaves a row past the bound whole to
// its old arms, as its writer does).
var messagesOwed = `
	from messages m
	where m.node_kind is null
	  and exists (select 1 from graph_nodes n where n.kind = '` + graphstore.KindMessage + `' and n.id = m.id::text)
	  and octet_length(coalesce(m.replies_to_block, '')) <= ` + addressBound

// messagesPass states each messages row an older build wrote as this release's message writer
// states it (api's planMessageNode, createStoredMessage): one tuple per pointer the row holds and
// the page a message on no issue hangs from (graphstore.MessageFacts), who wrote it
// (graphstore.AuthorFacts), and what it owed its thread (inboxwrite.RecordOlder), under the
// message.created or message.answered that stored it, dated at its created_at; and names its node
// on the row.
func messagesPass() inboxPass {
	return inboxPass{
		kind:  "messages",
		batch: `select m.id::text ` + messagesOwed + ` and m.id > $1::uuid order by m.id limit $2`,
		start: []string{zeroUUID},
		write: func(ctx context.Context, tx pgx.Tx, keys [][]string) (inboxWrote, error) {
			return writeMessages(ctx, tx, column(keys, 0))
		},
		remaining: messagesOwed,
	}
}

// writeMessages takes the owner rows of ids, then reads again and locks the messages rows of ids
// still owed, and states them.
//
// What an issue message owed its thread (inboxwrite.RecordOlder) holds the identities its text
// names (refs.ItemIdentityMentions, inboxwrite.Write.Names), read off its body against its node, as
// writeCommentPointers reads a comment's: this release cites the text of a message on an issue, an
// issue message or a session's reply to one, and of no other - a direct message, a session's reply
// to one and a broadcast's copy name nobody (api's planPendingItem, whose cites is false for them).
func writeMessages(ctx context.Context, tx pgx.Tx, ids []string) (inboxWrote, error) {
	if err := lockOwnersOf(ctx, tx, messageOwners, ids); err != nil {
		return inboxWrote{}, err
	}
	rows, err := tx.Query(ctx, `
		select m.id::text, coalesce(m.issue_key, ''), m.author, m.created_at, coalesce(m.target, ''),
		       coalesce(m.in_reply_to::text, ''), coalesce(m.replies_to_block, ''),
		       coalesce(m.broadcast_id::text, ''), `+messageEventOf("m")+`, coalesce(m.body, ''),
		       coalesce((select n.visibility from graph_nodes n where n.kind = '`+graphstore.KindMessage+`' and n.id = m.id::text), ''),
		       (select n.made_by from graph_nodes n where n.kind = '`+graphstore.KindMessage+`' and n.id = m.id::text)
		`+messagesOwed+` and m.id = any($1::uuid[])
		order by m.id
		for no key update of m
	`, ids)
	if err != nil {
		return inboxWrote{}, fmt.Errorf("read the messages naming no node: %w", err)
	}
	type message struct {
		row              graphstore.MessageRow
		author           model.Actor
		at               time.Time
		event            int64
		body, visibility string
		maker            model.Actor
	}
	var read []message
	for rows.Next() {
		var held message
		var author, maker []byte
		if err := rows.Scan(&held.row.ID, &held.row.IssueKey, &author, &held.at, &held.row.Target,
			&held.row.InReplyTo, &held.row.RepliesToBlock, &held.row.BroadcastID, &held.event,
			&held.body, &held.visibility, &maker); err != nil {
			rows.Close()
			return inboxWrote{}, fmt.Errorf("read a message naming no node: %w", err)
		}
		if err := json.Unmarshal(author, &held.author); err != nil {
			rows.Close()
			return inboxWrote{}, fmt.Errorf("read the author of message %s: %w", held.row.ID, err)
		}
		// The node's maker is decoded into an Actor of its own, so it shares no pointer with the
		// author and keeps no member of the author's that made_by lacks.
		held.maker = held.author
		if maker != nil {
			var made model.Actor
			if err := json.Unmarshal(maker, &made); err != nil {
				rows.Close()
				return inboxWrote{}, fmt.Errorf("read the maker of message %s's node: %w", held.row.ID, err)
			}
			held.maker = made
		}
		read = append(read, held)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return inboxWrote{}, fmt.Errorf("read the messages naming no node: %w", err)
	}
	if len(read) == 0 {
		return inboxWrote{}, nil
	}
	var nodes []graphstore.Node
	var tuples []graphstore.Tuple
	marked := make([]string, 0, len(read))
	// The page of each message that sits on an agent's page, every session among their targets read
	// in one statement (graphstore.AgentPages); the batch writes nothing before its facts below.
	var targets []string
	for _, held := range read {
		if held.row.OnAgentPage() {
			targets = append(targets, held.row.Target)
		}
	}
	var pages map[string]string
	if len(targets) > 0 {
		if pages, err = graphstore.AgentPages(ctx, tx, targets); err != nil {
			return inboxWrote{}, err
		}
	}
	for index := range read {
		held := &read[index]
		if page, ok := pages[held.row.Target]; ok && held.row.OnAgentPage() {
			held.row.Page = page
		}
		var projected bool
		if nodes, tuples, projected = graphstore.MessageFacts(nodes, tuples, held.row, held.author, held.at, held.event); !projected {
			continue
		}
		if authors, facts, named := graphstore.AuthorFacts(graphstore.KindMessage, held.row.ID, held.author, held.at, held.event); named {
			nodes = append(nodes, authors...)
			tuples = append(tuples, facts...)
		}
		marked = append(marked, held.row.ID)
	}
	nodes = writeNodesOnce(nodes)
	if err := graphstore.Write(ctx, tx, nodes, tuples); err != nil {
		return inboxWrote{}, fmt.Errorf("state a batch of messages: %w", err)
	}
	wrote := inboxWrote{rows: int64(len(marked)), nodes: int64(len(nodes)), tuples: int64(len(tuples)) + statedOf(nodes)}
	for _, held := range read {
		if !slices.Contains(marked, held.row.ID) {
			continue
		}
		var issue *string
		if held.row.IssueKey != "" {
			issue = &held.row.IssueKey
		}
		written := inboxwrite.Message(held.row.ID, held.author, held.at, issue, "")
		if issue != nil && held.row.BroadcastID == "" {
			if identities, people := refs.ItemIdentityMentions(refs.ItemMessage, held.row.ID, held.visibility, held.maker, held.at, held.event, held.body, nil); len(identities)+len(people) > 0 {
				written = written.Names(identities, people)
			}
		}
		owed, err := inboxwrite.RecordOlder(ctx, tx, written, held.event)
		if err != nil {
			return inboxWrote{}, err
		}
		wrote.tuples += int64(owed)
	}
	if _, err := tx.Exec(ctx, `
		update messages set node_kind = $2, node_address = id::text
		where id = any($1::uuid[]) and node_kind is null
	`, marked, graphstore.KindMessage); err != nil {
		return inboxWrote{}, fmt.Errorf("name the nodes of a batch of messages on their rows: %w", err)
	}
	return wrote, nil
}

// --- the comment pointers ---------------------------------------------------------------------

// commentPointersOwed is the comments rows the pointer pass owes: each whose tuples_stored is false
// (migration 20261116124447), whose node the store holds (the comment pass, which runs
// first), and whose replies_to_block the store can key (graphstore.CommentPointerFacts leaves a
// row past the bound whole to its old arms, as its writer does).
var commentPointersOwed = `
	from comments c
	where not c.tuples_stored and c.node_kind is not null
	  and octet_length(coalesce(c.replies_to_block, '')) <= ` + addressBound

// commentPointersKind is the comment pointer pass's kind, the census kind it marks
// (commentPointerProjection).
const commentPointersKind = "comment_pointers"

// commentPointersPass states each comment an older build wrote as this release's comment writer
// states its pointers (api's commentStoreWrite): replies_to, answers and the one part_of
// (graphstore.CommentPointerFacts), and what it owed its thread (inboxwrite.RecordOlder), under the
// event that stored it, dated at its created_at; and sets its tuples_stored.
func commentPointersPass() inboxPass {
	return inboxPass{
		kind:  commentPointersKind,
		batch: `select c.id::text ` + commentPointersOwed + ` and c.id > $1::uuid order by c.id limit $2`,
		start: []string{zeroUUID},
		write: func(ctx context.Context, tx pgx.Tx, keys [][]string) (inboxWrote, error) {
			return writeOwnedCommentPointers(ctx, tx, column(keys, 0))
		},
		remaining: commentPointersOwed,
	}
}

// writeOwnedCommentPointers takes the owner rows of ids, then states the comments of ids still
// owed (writeCommentPointers), each under the event the log holds of it.
func writeOwnedCommentPointers(ctx context.Context, tx pgx.Tx, ids []string) (inboxWrote, error) {
	if err := lockOwnersOf(ctx, tx, commentOwners, ids); err != nil {
		return inboxWrote{}, err
	}
	return writeCommentPointers(ctx, tx, ids, nil)
}

// HoldCommentCatchUp takes, in the block-index tick's read phase, the locks the tick's catch-up of
// the comments ids takes (CatchUpComments) beyond the owner lock of each that the tick already took
// (refs.LockCommentOwners): the owner of each as the backfill's passes read it (commentOwners),
// which is that same owner for every comment whose owner never moved, and, shared, the owner of the
// project whose default the waiting person of each ask the backfill owes that those comments'
// threads hang from falls through to (inboxwrite.HoldDefault), as the ask pass takes it before it
// reads that person (writeAsksOwed). Every lock of the catch-up is so taken before the tick writes
// anything, in the writers' order - the owner, then the project - and each take of one of them after
// the tick's write batch is a take the transaction holds already, which returns at once.
func HoldCommentCatchUp(ctx context.Context, tx pgx.Tx, ids []string) error {
	if err := lockOwnersOf(ctx, tx, commentOwners, ids); err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `
		select distinct k.issue_key, k.artifact_id::text `+asksOwed+`
		  and k.id in (select c.ask_id from comments c where c.id = any($1::uuid[]))
		order by 1, 2
	`, ids)
	if err != nil {
		return fmt.Errorf("read the owners of the asks a batch of comments hangs from: %w", err)
	}
	type owner struct{ issue, artifact *string }
	var owners []owner
	var issue, artifact *string
	if _, err := pgx.ForEachRow(rows, []any{&issue, &artifact}, func() error {
		owners = append(owners, owner{issue, artifact})
		return nil
	}); err != nil {
		return fmt.Errorf("read the owners of the asks a batch of comments hangs from: %w", err)
	}
	for _, held := range owners {
		if err := inboxwrite.HoldDefault(ctx, tx, held.issue, held.artifact); err != nil {
			return err
		}
	}
	return nil
}

// CatchUpComments is the rest of the backfill's write for the comments the block-index tick read
// off the events after its cursor (docs' readEventsAfterCursor), run by the tick in its own
// transaction once its write batch has given each comment of written its node (refs.CommentWrite)
// and before it commits. named is every comment those events name whose row named no node when the
// tick read them, written those of them the batch gave their node, and causes, beside each of
// written, the event that stored it, the cause the batch stated its node under.
//
// It runs the backfill's own write functions over the rows of those comments each pass still
// owes, in the order the backfill runs the passes (inboxPasses), so a comment an older build wrote
// after the backfill takes from the tick what the backfill gives one it read: the pointers of each
// of written, what it owed its thread and its tuples_stored, under the event beside it
// (writeCommentPointers); the delivery routes each of written names and their tuples_stored (the
// mention pass); threads_recorded on each of named past the store's bound
// (pastBoundComments); the blocks of each of written, their links and its citations (the comment's
// item passes); the delivery attempts of each of named (the attempt pass); and then the
// user_threads rows of every thread those comments are in whose items all hold their nodes
// (threadItemsStored), each stated and marked above every delivery the passes before it stated
// (writeUserThreads). Each pass reads again, under the locks it takes, which of those rows it still
// owes, as it does in the backfill, and leaves every other alone. The caller holds every owner and
// project lock those passes take (refs.LockCommentOwners, HoldCommentCatchUp), so each of their
// takes returns at once.
func CatchUpComments(ctx context.Context, tx pgx.Tx, named, written []string, causes []int64) error {
	if _, err := writeCommentPointers(ctx, tx, written, causes); err != nil {
		return err
	}
	comment := inboxItemKinds[0] // the comment's item tables
	for _, pass := range []struct {
		owed  string
		ids   []string
		width int
		write func(context.Context, pgx.Tx, [][]string) (inboxWrote, error)
	}{
		{`select m.comment_id::text, m.target ` + commentMentionsOwed + ` and m.comment_id = any($1::uuid[])
			order by m.comment_id, m.target`, written, 2, writeCommentMentions},
		{`select c.id::text ` + pastBoundComments.owed + ` and c.id = any($1::uuid[]) order by c.id`,
			named, 1, pastBoundComments.markRecorded},
		{`select b.comment_id::text, b.block_id ` + comment.blocksOwed() + ` and b.comment_id = any($1::uuid[])
			order by b.comment_id, b.block_id`, written, 2, comment.writeBlocks},
		{`select r.comment_id::text, r.block_id, r.to_kind, r.to_id, r.kind ` + comment.linksOwed() + `
			and r.comment_id = any($1::uuid[]) order by r.comment_id, r.block_id, r.to_kind, r.to_id, r.kind`,
			written, 5, comment.writeLinks},
		{`select r.from_id, r.to_kind, r.to_id ` + comment.citationsOwed() + ` and r.from_id = any($1::text[])
			order by r.from_id, r.to_kind, r.to_id`, written, 3, comment.writeCitations},
		{`select ` + commentAttempts.keyText() + commentAttempts.owed() + ` and d.comment_id = any($1::uuid[])
			order by ` + commentAttempts.keys, named, 3, commentAttempts.writeAttemptsAndSessions},
		{`select u.thread_id::text, u.login ` + userThreadsOwed + ` and ` + threadItemsStored + `
			and u.thread_id in (select coalesce(c.ask_id, c.reply_to, c.id) from comments c where c.id = any($1::uuid[]))
			order by u.thread_id, u.login`, named, 2, writeUserThreads},
	} {
		keys, err := readKeys(ctx, tx, pass.width, pass.owed, pass.ids)
		if err != nil {
			return fmt.Errorf("read what the comments the events name still owe: %w", err)
		}
		if len(keys) == 0 {
			continue
		}
		if _, err := pass.write(ctx, tx, keys); err != nil {
			return err
		}
	}
	return nil
}

// readKeys reads the keys sql answers through q, each width text columns.
func readKeys(ctx context.Context, q graphstore.Queryer, width int, sql string, args ...any) ([][]string, error) {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) ([]string, error) {
		key := make([]string, width)
		targets := make([]any, width)
		for index := range key {
			targets[index] = &key[index]
		}
		return key, row.Scan(targets...)
	})
}

// threadItemsStored is true of a user_threads row u each item of whose thread the thread pass's
// catch-up states (writeUserThreads) holds the node that catch-up states it out of: the thread
// holds no comment whose row names no node where the store holds such a comment
// (refs.CommentInTheStore), and the issue of a message thread no message the store holds no node of
// (messagesOwed's bound). The catch-up passes such an item by, so a row of its thread marked
// meanwhile would stand at a version that already counts the item's write, while the item's
// deliveries, stated once it has its node, land above the mark, where R6's equation counts them
// again. The backfill gives every comment and message its node before its thread pass runs; the
// block-index tick runs that pass while the release before this one may still be writing
// (CatchUpComments, CatchUpOwedRows), so it reads only such rows: a comment takes its node from the
// tick that reads its comment.created or comment.answered, and a message from the object pass, and
// the rows of its thread are taken after that.
var threadItemsStored = `not exists (
		select 1 from comments c` + refs.CommentStoreJoins + `
		where (c.id = u.thread_id or c.reply_to = u.thread_id or c.ask_id = u.thread_id)
		  and coalesce(c.ask_id, c.reply_to, c.id) = u.thread_id
		  and c.node_kind is null and ` + refs.CommentInTheStore + `)
	  and not (u.kind = '` + graphstore.KindMessage + `' and exists (
		select 1 from messages m
		where m.issue_key = u.issue_key and m.node_kind is null
		  and octet_length(coalesce(m.replies_to_block, '')) <= ` + addressBound + `
		  and not exists (select 1 from graph_nodes n where n.kind = '` + graphstore.KindMessage + `' and n.id = m.id::text)))`

// backfillRan is true where a run of the backfill has recorded the horizon it began at
// (recordBackfillHorizon), of any marking rule: every run walks the inbox's passes after that. A
// row a build before this one left unmarked before any run is the backfill's, which takes the
// table in one walk.
const backfillRan = `exists (select 1 from graph_backfill_horizon)`

// RollRows is what the block-index tick's catch-up of the rows another build left unmarked did
// (CatchUpOwedRows): how many rows it marked, and whether a pass read a full batch, so that rows
// may still be owed.
type RollRows struct {
	Rows int64
	More bool
}

// rollPasses are the backfill's passes CatchUpOwedRows runs, in the order the backfill runs them:
// the session pass over the sessions a published fallback copy reached (fallbackSessionsPass), the
// one arm of that pass no event of the log names and no other pass here writes, and then every
// pass of the inbox (inboxPasses) but the decline pass, each attempt pass writing the nodes of the
// sessions its rows reached before it marks them (writeAttemptsAndSessions). A decline holds no
// row but its event, which the tick reads once (CatchUpEvents), so a pass over declines would read
// the log. The comment pointer pass, the pass over the comments past the store's bound and the
// comment mention pass owe only rows an older build inserts with a comment, in the transaction
// that appends the comment.created or comment.answered storing it (migrations 20261116150000 and
// 20261120090000), and the tick gives every comment those events name whose row names no node the
// same writes while it reads them (CatchUpComments); but a build that writes a comment's node
// itself, as the store's own release does, writes a comment whose row names its node, which that
// catch-up passes by, so only these passes reach it and the rows written with it; the event that
// stores such a comment makes the tick owe them (rollOwedByEvents). The published-delivery pass
// reads here the queue the trigger of migration 20261122100122 fills (publishedDeliveriesQueued)
// rather than the log: an older build's outbox adds the topics an event reached to its
// published_destinations after the event commits, appending no event, so no read of the log after
// the tick's cursor finds a publish that lands after the tick has read its event. The pass over the
// queue of an older build's events (inboxEventsPass) reads the queue migration 20261122100123
// fills, here as in the backfill: a move of who an ask waits on and an edit of a comment change no
// row's marker, so no pass over a marker reaches them, and the queue holds each such event until
// the batch that states it deletes it, whichever of the backfill and the tick takes it first.
// Each pass reads its rows through the index that holds the rows it owes
// (TestTheRollPassesReadTheirOwedRowsThroughThePartialIndexes), but two: the asks pass reads asks
// whole, as the object pass does, since its marker test reads an answer's moment through a cast no
// index predicate may hold, and the fallback copies pass reads the published copies whole, a copy
// holding no marker.
func rollPasses() []inboxPass {
	passes := []inboxPass{fallbackSessionsPass()}
	for _, pass := range inboxPasses() {
		switch pass.kind {
		case declinesKind:
			continue
		case commentAttempts.table:
			pass.write = commentAttempts.writeAttemptsAndSessions
		case messageAttempts.table:
			pass.write = messageAttempts.writeAttemptsAndSessions
		case publishedDeliveriesKind:
			// kept for one release: events-published-owed
			pass.batch, pass.remaining = publishedDeliveriesQueuedBatch, publishedDeliveriesQueued
		}
		passes = append(passes, pass)
	}
	return passes
}

// CatchUpOwedRows is the block-index tick's catch-up of the rows of the inbox's caches a build
// before this one leaves unmarked after the backfill: every kind the backfill's inbox passes mark
// but the declines, which the tick gives while it reads their events (rollPasses), which that
// build inserts, and updates with this release's marker taken off (migrations 20261116150000,
// 20261116160000 and 20261116170000), not always with an event that names the row - its delivery
// sender's claims append none, its read marks name a person and no thread, and its outbox's record
// of the topics an event reached appends none (migration 20261122100122 queues that event in
// events_published_owed instead). Only the backfill marks such a row again, so without this the
// unmarked class R9 holds to zero would stand until someone ran the backfill again. The docs' tick
// runs it on each tick that owes the object pass (writeObjectNodesAnotherBuildWrote) - every tick
// while another build serves, the first after it left, a tick whose events leave such a row owed
// (EventsOweObjectPass), each process's first and one an interval after its last pass - and keeps
// that debt while it answers More or fails.
//
// It runs each pass's own write over one batch of the rows that pass owes, from its first, each
// batch in a transaction of its own under the locks the backfill's batch takes, outside the tick's
// other transactions, so each class reaches zero one batch a tick after that build stops writing.
// Until a run of the backfill has recorded its horizon (backfillRan) it reads nothing more: every
// such row is the backfill's then.
func CatchUpOwedRows(ctx context.Context, database *store.Store) (RollRows, error) {
	var done RollRows
	var ran bool
	if err := database.Pool.QueryRow(ctx, `select `+backfillRan).Scan(&ran); err != nil {
		return done, fmt.Errorf("read whether the backfill has run: %w", err)
	}
	if !ran {
		return done, nil
	}
	var failed []error
	for _, pass := range rollPasses() {
		keys, err := readKeys(ctx, database.Pool, len(pass.start), pass.batch, batchArgs(pass.start)...)
		if err != nil {
			failed = append(failed, fmt.Errorf("read the %s rows another build left unmarked: %w", pass.kind, err))
			continue
		}
		if len(keys) == 0 {
			continue
		}
		done.More = done.More || len(keys) == backfillBatch
		var wrote inboxWrote
		if err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
			var err error
			wrote, err = pass.write(ctx, tx, keys)
			return err
		}); err != nil {
			failed = append(failed, fmt.Errorf("write a batch of the %s rows another build left unmarked: %w", pass.kind, err))
			continue
		}
		done.Rows += wrote.rows
	}
	return done, errors.Join(failed...)
}

// CatchUpEvents is the block-index tick's write, in its own transaction once its write batch has
// run, of what the backfill gives the writes of a build before this one that the events in (after,
// through] are and that hold no row of their own: each decline among them whose participation the
// store does not state yet (the decline pass's own read, declinesOwed, and write, writeDeclines),
// and the node of each session they name that holds none (the session pass's arm over the log,
// eventSessions). The tick reads each event once, so each is given that write once, where a pass
// over declines would read the log again on every tick. Until a run of the backfill has recorded
// its horizon (backfillRan) it writes nothing: every such write is the backfill's then. Its first
// statement reads that beside whether any event in (after, through] is of a type its two reads
// read (catchUpEventTypes), and where none is it reads nothing more, since both would answer
// nothing: so a tick whose events hold no decline and name no session costs one statement here.
// It answers how many declines and sessions it wrote.
func CatchUpEvents(ctx context.Context, tx pgx.Tx, after, through int64) (int, error) {
	if through <= after {
		return 0, nil
	}
	var owed bool
	if err := tx.QueryRow(ctx, `select `+backfillRan+` and exists (
		select 1 from events e where e.id > $1 and e.id <= $2 and e.type in `+catchUpEventTypes+`)`, after, through).Scan(&owed); err != nil {
		return 0, fmt.Errorf("read whether the backfill has run: %w", err)
	}
	if !owed {
		return 0, nil
	}
	sessions, err := eventSessions(ctx, tx, after, through)
	if err != nil {
		return 0, err
	}
	if len(sessions) > 0 {
		if _, _, err := writeSessionBatch(ctx, tx, sessions); err != nil {
			return 0, err
		}
	}
	keys, err := readKeys(ctx, tx, 1, `select e.id::text `+declinesOwed+` and e.id > $1 and e.id <= $2 order by e.id`, after, through)
	if err != nil {
		return 0, fmt.Errorf("read the declines the events after the cursor are: %w", err)
	}
	var declined inboxWrote
	if len(keys) > 0 {
		if declined, err = writeDeclines(ctx, tx, keys); err != nil {
			return 0, err
		}
	}
	return len(sessions) + int(declined.rows), nil
}

// catchUpEventTypes are the types of the events CatchUpEvents gives a write: the ask.follower_added,
// ask.opened and comment.delivery that name a session (eventSessions), and a decline's
// ask.resolved (declinesOwed). A read it adds over another type adds that type here.
const catchUpEventTypes = `('ask.follower_added', 'ask.opened', 'comment.delivery', 'ask.resolved')`

// InboxRowsOwed is, by the kind of each pass the block-index tick catches up (rollPasses) and of the
// decline pass, how many rows that pass still owes: the backfill's own count of what it has left
// (KindBackfill.Remaining), which reads zero once every row a build before this one left is marked.
func InboxRowsOwed(ctx context.Context, q graphstore.Queryer) (map[string]int64, error) {
	owed := map[string]int64{}
	for _, pass := range append(rollPasses(), declinesPass()) {
		var count int64
		if err := q.QueryRow(ctx, `select count(*) `+pass.remaining).Scan(&count); err != nil {
			return nil, fmt.Errorf("count the %s rows still owed: %w", pass.kind, err)
		}
		owed[pass.kind] = count
	}
	return owed, nil
}

// writeCommentPointers reads again and locks the comments rows of ids still owed, and states them,
// under the owner locks its caller holds. Each is stated under the event beside it in causes, and,
// where causes is nil, under the event the log holds of it (commentEventOf).
//
// What each owed its thread (inboxwrite.RecordOlder) holds the identities its text names
// (refs.ItemIdentityMentions, inboxwrite.Write.Names): the build that wrote it stated none of
// their mentions and stored no row of a person's, so its body, as the comment holds it now, is the
// one record of whom it names, read against the comment's node, its visibility and its maker, as
// this release's write of the same text reads its own (api's planPendingItem, which every comment
// write cites through). A session's or a role's mention it names as a delivery route the mention
// pass states again from its comment_mentions row under the same event, which the repeat key keeps
// once. The identities the store holds a mention of out of the node already are no standing here:
// the mentions are stated under the comment's own event, which only this pass and the mention pass
// state, and a mention a re-index of the text stated at the sentinel (refs.ReindexOwed) explains no
// participation under that event (graphcensus' mentioned_alone).
func writeCommentPointers(ctx context.Context, tx pgx.Tx, ids []string, causes []int64) (inboxWrote, error) {
	event, args := commentEventOf("c"), []any{ids}
	if causes != nil {
		event = `(select named.cause from unnest($1::uuid[], $2::bigint[]) as named (id, cause) where named.id = c.id)`
		args = append(args, causes)
	}
	rows, err := tx.Query(ctx, `
		select c.id::text, c.author, c.created_at, coalesce(c.reply_to::text, ''), coalesce(c.ask_id::text, ''),
		       coalesce(c.answers::text, ''), coalesce(c.replies_to_block, ''), c.issue_key, c.artifact_id::text,
		       `+event+`, coalesce(c.body, ''), c.node_kind, c.node_address
		`+commentPointersOwed+` and c.id = any($1::uuid[])
		order by c.id
		for no key update of c
	`, args...)
	if err != nil {
		return inboxWrote{}, fmt.Errorf("read the comments whose pointers are no tuples: %w", err)
	}
	type comment struct {
		id                   string
		author               model.Actor
		at                   time.Time
		pointers             graphstore.CommentPointers
		issueKey, artifactID *string
		event                int64
		body, visibility     string
		nodeKind, nodeID     string
		maker                model.Actor
	}
	var read []comment
	for rows.Next() {
		var held comment
		var author []byte
		if err := rows.Scan(&held.id, &author, &held.at, &held.pointers.ReplyTo, &held.pointers.AskID,
			&held.pointers.Answers, &held.pointers.RepliesToBlock, &held.issueKey, &held.artifactID, &held.event,
			&held.body, &held.nodeKind, &held.nodeID); err != nil {
			rows.Close()
			return inboxWrote{}, fmt.Errorf("read a comment whose pointers are no tuples: %w", err)
		}
		if err := json.Unmarshal(author, &held.author); err != nil {
			rows.Close()
			return inboxWrote{}, fmt.Errorf("read the author of comment %s: %w", held.id, err)
		}
		held.maker = held.author
		read = append(read, held)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return inboxWrote{}, fmt.Errorf("read the comments whose pointers are no tuples: %w", err)
	}
	// The visibility and the maker of each comment's node, which its mentions are read under
	// (refs.ItemIdentityMentions), read in a statement of their own once the comments rows are
	// held: graph_nodes is a step after the item rows in the writers' order.
	if len(read) > 0 {
		kinds, nodeIDs := make([]string, len(read)), make([]string, len(read))
		for index, held := range read {
			kinds[index], nodeIDs[index] = held.nodeKind, held.nodeID
		}
		nodes, err := tx.Query(ctx, `
			select named.position::int, n.visibility, n.made_by
			from unnest($1::text[], $2::text[]) with ordinality as named (kind, id, position)
			join graph_nodes n on n.kind = named.kind and n.id = named.id
		`, kinds, nodeIDs)
		if err != nil {
			return inboxWrote{}, fmt.Errorf("read the nodes of the comments whose pointers are no tuples: %w", err)
		}
		var position int
		var visibility string
		var maker []byte
		if _, err := pgx.ForEachRow(nodes, []any{&position, &visibility, &maker}, func() error {
			held := &read[position-1]
			held.visibility = visibility
			// The node's maker is decoded into an Actor of its own, so it shares no pointer with
			// the author held.maker was set to and keeps no member of the author's that made_by
			// lacks.
			var made model.Actor
			if err := json.Unmarshal(maker, &made); err != nil {
				return err
			}
			held.maker = made
			return nil
		}); err != nil {
			return inboxWrote{}, fmt.Errorf("read the nodes of the comments whose pointers are no tuples: %w", err)
		}
	}
	var tuples []graphstore.Tuple
	marked := make([]string, 0, len(read))
	for _, held := range read {
		var stored bool
		if tuples, stored = graphstore.CommentPointerFacts(tuples, held.id, held.pointers, held.author, held.at, held.event); stored {
			marked = append(marked, held.id)
		}
	}
	if len(marked) == 0 {
		return inboxWrote{}, nil
	}
	if err := graphstore.Write(ctx, tx, nil, tuples); err != nil {
		return inboxWrote{}, fmt.Errorf("state the pointers of a batch of comments: %w", err)
	}
	wrote := inboxWrote{rows: int64(len(marked)), tuples: int64(len(tuples))}
	for _, held := range read {
		if !slices.Contains(marked, held.id) {
			continue
		}
		written := inboxwrite.Comment(held.id, held.author, held.at, held.issueKey, held.artifactID, "")
		if identities, people := refs.ItemIdentityMentions(refs.ItemComment, held.id, held.visibility, held.maker, held.at, held.event, held.body, nil); len(identities)+len(people) > 0 {
			written = written.Names(identities, people)
		}
		owed, err := inboxwrite.RecordOlder(ctx, tx, written, held.event)
		if err != nil {
			return inboxWrote{}, err
		}
		wrote.tuples += int64(owed)
	}
	if _, err := tx.Exec(ctx, `
		update comments set tuples_stored = true where id = any($1::uuid[]) and not tuples_stored
	`, marked); err != nil {
		return inboxWrote{}, fmt.Errorf("mark a batch of comments' pointers stored: %w", err)
	}
	return wrote, nil
}

// --- the rows past the store's bound ----------------------------------------------------------

// pastBoundTable is one table whose rows an older build wrote past the store's address bound the
// past-bound pass marks: the kind it reports, its table, the read of the owner of each of its rows
// (lockOwnersOf), the rows it owes, `from` the table under the alias the batch's key names, and the
// statement that marks a batch of them, by id ($1), which takes the rows still owed `for no key
// update` in ascending id, as every writer of these rows takes them after their owners.
type pastBoundTable struct {
	kind, table, alias, owners, owed, mark string
}

// pastBoundComments are the comments rows an older build wrote that no pointer pass takes and
// this release's writer would not have stated either: a comment left whole out of the store
// (refs.CommentInTheStore), and one whose replies_to_block the store cannot key, which the pointer
// pass leaves to its old arms as the writer does.
var pastBoundComments = pastBoundTable{
	kind: "comments_past_bound", table: "comments", alias: "c", owners: commentOwners,
	mark: `
	with held as (
		select c.id from comments c
		where c.id = any($1::uuid[]) and not c.threads_recorded
		order by c.id
		for no key update of c
	)
	update comments set threads_recorded = true from held where comments.id = held.id`,
	owed: `
	from comments c` + refs.CommentStoreJoins + `
	where not c.tuples_stored and not c.threads_recorded
	  and (not (` + refs.CommentInTheStore + `) or octet_length(coalesce(c.replies_to_block, '')) > ` + addressBound + `)`,
}

// pastBoundMessages are the messages rows an older build wrote whose replies_to end the store
// cannot key (graphstore.MessageFacts), which the messages pass leaves to their old arms, as the
// writer does.
var pastBoundMessages = pastBoundTable{
	kind: "messages_past_bound", table: "messages", alias: "m", owners: messageOwners,
	mark: `
	with held as (
		select m.id from messages m
		where m.id = any($1::uuid[]) and not m.threads_recorded
		order by m.id
		for no key update of m
	)
	update messages set threads_recorded = true from held where messages.id = held.id`,
	owed: `
	from messages m
	where m.node_kind is null and not m.threads_recorded
	  and octet_length(coalesce(m.replies_to_block, '')) > ` + addressBound,
}

// pastBoundRowsPass sets threads_recorded on each row of table an older build wrote past the
// store's bound (migrations 20261116124447 and 20261116114515), after the owner rows of its batch,
// and states nothing: no tuple of such a row can be stated, as this release's writer states none
// of one (inboxwrite's Write.Unstored), and the trigger that fired on its insert wrote its
// user_threads rows then, each moved row unmarked by its depth test (migration 20261116160000),
// which the thread pass marks. The mark is what passes the row by the triggers' marker test, which
// the release that drops those triggers reads: without it such a row would stand unmarked for
// good, the one row no pass takes, and the triggers would never go.
func pastBoundRowsPass(table pastBoundTable) inboxPass {
	return inboxPass{
		kind:      table.kind,
		batch:     `select ` + table.alias + `.id::text ` + table.owed + ` and ` + table.alias + `.id > $1::uuid order by ` + table.alias + `.id limit $2`,
		start:     []string{zeroUUID},
		write:     table.markRecorded,
		remaining: table.owed,
	}
}

// markRecorded takes the owner rows of the rows of table the keys name, then marks those of them
// still owed recorded (pastBoundRowsPass).
func (table pastBoundTable) markRecorded(ctx context.Context, tx pgx.Tx, keys [][]string) (inboxWrote, error) {
	ids := column(keys, 0)
	if err := lockOwnersOf(ctx, tx, table.owners, ids); err != nil {
		return inboxWrote{}, err
	}
	tag, err := tx.Exec(ctx, table.mark, ids)
	if err != nil {
		return inboxWrote{}, fmt.Errorf("mark a batch of %s past the store's bound recorded: %w", table.table, err)
	}
	return inboxWrote{rows: tag.RowsAffected()}, nil
}

// --- the delivery routes a comment names ------------------------------------------------------

// commentMentionsOwed is the comment_mentions rows the mention pass owes: each whose tuples_stored
// is false (migration 20261120090000), of a comment whose node the store holds (the comment pass,
// which runs first, or the tick that gave the comment its node), whose target routes to a session
// or a role and whose resolved session the store can key - the rows the census counts unmarked
// (commentMentionProjection). A row whose target or session is past the store's bound states no
// tuple and keeps its false, as this release's comment write leaves one (api's mentionStored).
var commentMentionsOwed = func() string {
	_, route := routeEnd(`m.target`)
	return `
	from comment_mentions m
	join comments c on c.id = m.comment_id
	join graph_nodes n on n.kind = '` + graphstore.KindComment + `' and n.id = m.comment_id::text
	where not m.tuples_stored and ` + route + ` is not null and ` + graphstore.AddressFitsSQL(`coalesce(m.resolved_session_id, '')`)
}()

// commentMentionsPass states each comment_mentions row an older build wrote as this release's
// comment write states the delivery route it records (api's commentMentionFacts): a mentions tuple
// out of the comment's node to the session or role its target routes to, a second to the session
// it resolved to, and the comment's urgency, its delivery value, each made by the comment's author
// at its created_at under the event that caused the comment's node - the comment's own write, the
// event the census holds those tuples to (commentMentionProjection); and sets the row's
// tuples_stored.
func commentMentionsPass() inboxPass {
	return inboxPass{
		kind: "comment_mentions",
		batch: `select m.comment_id::text, m.target ` + commentMentionsOwed + `
			and (m.comment_id, m.target) > ($1::uuid, $2)
			order by m.comment_id, m.target limit $3`,
		start:     []string{zeroUUID, ""},
		write:     writeCommentMentions,
		remaining: commentMentionsOwed,
	}
}

// writeCommentMentions takes the owner rows of the comments keys names, then reads again and locks
// the comment_mentions rows of keys still owed, in key order, as the delivery sender takes a
// mention's row after its comment's owner (api's claimCommentDeliveryAttempt), and states them. A
// comment's mention of one session or role is stated once however many of its rows name it, and
// its delivery once, the mode its first row records: the writer applies one mode to every target
// (api's validateCommentMentions).
func writeCommentMentions(ctx context.Context, tx pgx.Tx, keys [][]string) (inboxWrote, error) {
	if err := lockOwnersOf(ctx, tx, commentOwners, column(keys, 0)); err != nil {
		return inboxWrote{}, err
	}
	routeKind, routeID := routeEnd(`m.target`)
	rows, err := tx.Query(ctx, `
		select m.comment_id::text, m.target, `+routeKind+`, `+routeID+`, m.resolved_session_id, m.delivery,
		       c.author, c.created_at, n.source_event
		`+commentMentionsOwed+`
		  and (m.comment_id, m.target) in (select * from unnest($1::uuid[], $2::text[]))
		order by m.comment_id, m.target
		for no key update of m
	`, column(keys, 0), column(keys, 1))
	if err != nil {
		return inboxWrote{}, fmt.Errorf("read the comment_mentions rows no tuple states: %w", err)
	}
	type mention struct {
		comment, target, routeKind, routeID, delivery string
		resolved                                      *string
		author                                        model.Actor
		at                                            time.Time
		event                                         int64
	}
	var read []mention
	for rows.Next() {
		var held mention
		var author []byte
		if err := rows.Scan(&held.comment, &held.target, &held.routeKind, &held.routeID, &held.resolved, &held.delivery,
			&author, &held.at, &held.event); err != nil {
			rows.Close()
			return inboxWrote{}, fmt.Errorf("read a comment_mentions row no tuple states: %w", err)
		}
		if err := json.Unmarshal(author, &held.author); err != nil {
			rows.Close()
			return inboxWrote{}, fmt.Errorf("read the author of comment %s: %w", held.comment, err)
		}
		read = append(read, held)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return inboxWrote{}, fmt.Errorf("read the comment_mentions rows no tuple states: %w", err)
	}
	if len(read) == 0 {
		return inboxWrote{}, nil
	}
	tuples := make([]graphstore.Tuple, 0, 3*len(read))
	stated := make(map[[3]string]struct{}, 2*len(read))
	comments, targets := make([]string, len(read)), make([]string, len(read))
	ends := make([][2]string, 0, 2)
	for index, held := range read {
		comments[index], targets[index] = held.comment, held.target
		ends = append(ends[:0], [2]string{held.routeKind, held.routeID})
		if held.resolved != nil {
			ends = append(ends, [2]string{graphstore.KindSession, *held.resolved})
		}
		for _, end := range ends {
			key := [3]string{held.comment, end[0], end[1]}
			if _, seen := stated[key]; seen {
				continue
			}
			stated[key] = struct{}{}
			tuples = append(tuples, graphstore.Tuple{
				FromKind: graphstore.KindComment, FromID: held.comment, Kind: graphstore.TupleMentions, ToKind: end[0], ToID: end[1],
				At: held.at, Who: held.author, SourceEvent: held.event, Asserted: true,
			})
		}
		if index == 0 || read[index-1].comment != held.comment {
			tuples = append(tuples, graphstore.Tuple{
				FromKind: graphstore.KindComment, FromID: held.comment, Kind: graphstore.TupleDelivery, ToKind: graphstore.KindValue,
				ToID: held.delivery, At: held.at, Who: held.author, SourceEvent: held.event, Asserted: true,
			})
		}
	}
	if err := writeRelations(ctx, tx, tuples); err != nil {
		return inboxWrote{}, fmt.Errorf("state the delivery routes of a batch of comments: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		update comment_mentions m set tuples_stored = true
		from unnest($1::uuid[], $2::text[]) as named (comment_id, target)
		where m.comment_id = named.comment_id and m.target = named.target and not m.tuples_stored
	`, comments, targets); err != nil {
		return inboxWrote{}, fmt.Errorf("mark a batch of comment_mentions rows stored: %w", err)
	}
	return inboxWrote{rows: int64(len(read)), tuples: int64(len(tuples))}, nil
}

// --- an item's blocks, their links and its citations ----------------------------------------------

// inboxItemKind is the storage of one item kind whose blocks, links and citations the item passes
// state, as refs' item index keeps them (refs.ItemKind's tables): the item's own table and the
// column naming it in each block table, its blocks, their links and their state, the read of its
// owner rows, the event that recorded it, and the column of a block row naming its own maker,
// which only an ask's has (an ask's answer part is made by whoever answered).
type inboxItemKind struct {
	kind                         string
	items, column                string
	blocks, links, state, owners string
	event                        func(alias string) string
	maker                        string
}

var inboxItemKinds = []inboxItemKind{
	{kind: graphstore.KindComment, items: "comments", column: "comment_id", blocks: "comment_blocks",
		links: "comment_block_refs", state: "comment_block_state", owners: commentOwners, event: commentEventOf},
	{kind: graphstore.KindMessage, items: "messages", column: "message_id", blocks: "message_blocks",
		links: "message_block_refs", state: "message_block_state", owners: messageOwners, event: messageEventOf},
	{kind: graphstore.KindAsk, items: "asks", column: "ask_id", blocks: "ask_blocks",
		links: "ask_block_refs", state: "ask_block_state", owners: askOwners, event: askOpenedOf, maker: "made_by"},
}

// eventOf is the event that recorded the item of this kind whose id the SQL itemID spells (a
// uuid), read off the item's own row, which no marker of the inbox backfill is a column of.
func (kind inboxItemKind) eventOf(itemID string) string {
	return `(select ` + kind.event("i") + ` from ` + kind.items + ` i where i.id = ` + itemID + `)`
}

// lockItems takes, for the items ids of kind, their owner rows, their own rows and their block
// state rows, each `for no key update` and in ascending key: the order refs' item index takes them
// in (owner, item, state), so the index of one of them and this batch take turns.
func (kind inboxItemKind) lockItems(ctx context.Context, tx pgx.Tx, ids []string) error {
	slices.Sort(ids)
	ids = slices.Compact(ids)
	if err := lockOwnersOf(ctx, tx, kind.owners, ids); err != nil {
		return err
	}
	for _, lock := range []string{
		`select 1 from ` + kind.items + ` where id = any($1::uuid[]) order by id for no key update`,
		`select 1 from ` + kind.state + ` where ` + kind.column + ` = any($1::uuid[]) order by ` + kind.column + ` for no key update`,
	} {
		if _, err := tx.Exec(ctx, lock, ids); err != nil {
			return fmt.Errorf("lock a batch of %s items: %w", kind.kind, err)
		}
	}
	return nil
}

// blocksOwed is the block rows of kind the block pass owes: each that names no node (migration
// 20261116140000) of an item whose node the store holds, at an address the store can key
// (refs' ItemScope.blockNodeOf).
func (kind inboxItemKind) blocksOwed() string {
	return `
	from ` + kind.blocks + ` b
	join graph_nodes item on item.kind = '` + kind.kind + `' and item.id = b.` + kind.column + `::text
	where b.node_kind is null and octet_length(b.node_id) <= ` + addressBound
}

// itemBlocksPass states each block row of kind an older build wrote as refs' item index states a
// block it writes or takes over (planItemBlocks): the block's node, read under its item's
// visibility, project and parties and made by the block's maker at the row's created_at - live
// where the block stands at a position, gone where the item no longer holds it - and its part_of to
// the item, dated by the row, where the store does not place it there now; under the event that
// recorded the item; and names the node on the row.
func itemBlocksPass(kind inboxItemKind) inboxPass {
	return inboxPass{
		kind: kind.blocks,
		batch: `select b.` + kind.column + `::text, b.block_id ` + kind.blocksOwed() + `
			and (b.` + kind.column + `, b.block_id) > ($1::uuid, $2)
			order by b.` + kind.column + `, b.block_id limit $3`,
		start:     []string{zeroUUID, ""},
		write:     kind.writeBlocks,
		remaining: kind.blocksOwed(),
	}
}

func (kind inboxItemKind) writeBlocks(ctx context.Context, tx pgx.Tx, keys [][]string) (inboxWrote, error) {
	if err := kind.lockItems(ctx, tx, column(keys, 0)); err != nil {
		return inboxWrote{}, err
	}
	maker := "null::jsonb"
	if kind.maker != "" {
		maker = "b." + kind.maker
	}
	rows, err := tx.Query(ctx, `
		select b.`+kind.column+`::text, b.block_id, b.node_id, b.position is not null, b.created_at, `+maker+`,
		       item.visibility, coalesce(item.project_key, ''), item.parties, item.made_by, `+kind.eventOf(`b.`+kind.column)+`,
		       exists (
		           select 1 from (`+graphstore.CurrentTupleRead(`t.to_kind, t.to_id`, `t.asserted`,
		`t.from_kind = '`+refs.BlockNodeKind+`' and t.from_id = b.node_id and t.kind = '`+graphstore.TuplePartOf+`'
		                and t.to_kind = '`+kind.kind+`' and t.to_id = b.`+kind.column+`::text`)+`) placed
		           where placed.asserted)
		`+kind.blocksOwed()+`
		  and (b.`+kind.column+`, b.block_id) in (select * from unnest($1::uuid[], $2::text[]))
		order by b.`+kind.column+`, b.block_id
		for no key update of b
	`, column(keys, 0), column(keys, 1))
	if err != nil {
		return inboxWrote{}, fmt.Errorf("read the %s naming no node: %w", kind.blocks, err)
	}
	var nodes []graphstore.Node
	var tuples []graphstore.Tuple
	var items, blockIDs []string
	for rows.Next() {
		var item, blockID, address, visibility, project string
		var current, placed bool
		var createdAt time.Time
		var blockMaker, itemMaker []byte
		var parties []string
		var event int64
		if err := rows.Scan(&item, &blockID, &address, &current, &createdAt, &blockMaker,
			&visibility, &project, &parties, &itemMaker, &event, &placed); err != nil {
			rows.Close()
			return inboxWrote{}, fmt.Errorf("read a %s row naming no node: %w", kind.blocks, err)
		}
		var who model.Actor
		made := itemMaker
		if len(blockMaker) > 0 {
			made = blockMaker
		}
		if err := json.Unmarshal(made, &who); err != nil {
			rows.Close()
			return inboxWrote{}, fmt.Errorf("read the maker of %s: %w", address, err)
		}
		state := graphstore.StateLive
		if !current {
			state = graphstore.StateGone
		}
		nodes = append(nodes, graphstore.Node{
			Kind: refs.BlockNodeKind, ID: address, Supertag: refs.BlockSupertag,
			Visibility: visibility, State: state, Project: project, Parties: parties,
			MadeAt: createdAt, MadeBy: who, SourceEvent: event,
		})
		if !placed {
			tuples = append(tuples, graphstore.Tuple{
				FromKind: refs.BlockNodeKind, FromID: address, Kind: graphstore.TuplePartOf,
				ToKind: kind.kind, ToID: item,
				At: createdAt, Who: who, SourceEvent: event, Asserted: true,
			})
		}
		items, blockIDs = append(items, item), append(blockIDs, blockID)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return inboxWrote{}, fmt.Errorf("read the %s naming no node: %w", kind.blocks, err)
	}
	if len(items) == 0 {
		return inboxWrote{}, nil
	}
	if err := graphstore.Write(ctx, tx, nodes, tuples); err != nil {
		return inboxWrote{}, fmt.Errorf("state a batch of %s: %w", kind.blocks, err)
	}
	if _, err := tx.Exec(ctx, `
		update `+kind.blocks+` b set node_kind = $3, node_address = b.node_id
		from unnest($1::uuid[], $2::text[]) as named (item, block)
		where b.`+kind.column+` = named.item and b.block_id = named.block and b.node_kind is null
	`, items, blockIDs, refs.BlockNodeKind); err != nil {
		return inboxWrote{}, fmt.Errorf("name the nodes of a batch of %s on their rows: %w", kind.blocks, err)
	}
	return inboxWrote{rows: int64(len(items)), nodes: int64(len(nodes)), tuples: int64(len(tuples)) + statedOf(nodes)}, nil
}

// linksOwed is the link rows of kind the link pass owes: each that names no node (migration
// 20261116140000) out of a block whose node the store holds - read off graph_nodes and not off its
// block row, which the block pass has marked - to a far end the store can key.
func (kind inboxItemKind) linksOwed() string {
	return `
	from ` + kind.links + ` r
	join graph_nodes block on block.kind = '` + refs.BlockNodeKind + `' and block.id = r.from_node
	where r.node_kind is null and octet_length(r.to_id) <= ` + addressBound
}

// itemLinksPass states each link row of kind an older build wrote as refs' item index states a link
// it inserts (ItemIndexing.queueBlockRefs): one tuple out of the block's node, under the row's own
// kind and to its own far end, dated by the row and carrying its source_seq, made by the block's
// maker, under the event that recorded the item; and names the block's node on the row.
func itemLinksPass(kind inboxItemKind) inboxPass {
	return inboxPass{
		kind: kind.links,
		batch: `select r.` + kind.column + `::text, r.block_id, r.to_kind, r.to_id, r.kind ` + kind.linksOwed() + `
			and (r.` + kind.column + `, r.block_id, r.to_kind, r.to_id, r.kind) > ($1::uuid, $2, $3, $4, $5)
			order by r.` + kind.column + `, r.block_id, r.to_kind, r.to_id, r.kind limit $6`,
		start:     []string{zeroUUID, "", "", "", ""},
		write:     kind.writeLinks,
		remaining: kind.linksOwed(),
	}
}

func (kind inboxItemKind) writeLinks(ctx context.Context, tx pgx.Tx, keys [][]string) (inboxWrote, error) {
	if err := kind.lockItems(ctx, tx, column(keys, 0)); err != nil {
		return inboxWrote{}, err
	}
	rows, err := tx.Query(ctx, `
		select r.`+kind.column+`::text, r.block_id, r.from_node, r.to_kind, r.to_id, r.kind, r.created_at, r.source_seq,
		       block.made_by, `+kind.eventOf(`r.`+kind.column)+`
		`+kind.linksOwed()+`
		  and (r.`+kind.column+`, r.block_id, r.to_kind, r.to_id, r.kind) in (
		      select * from unnest($1::uuid[], $2::text[], $3::text[], $4::text[], $5::text[]))
		order by r.`+kind.column+`, r.block_id, r.to_kind, r.to_id, r.kind
		for no key update of r
	`, column(keys, 0), column(keys, 1), column(keys, 2), column(keys, 3), column(keys, 4))
	if err != nil {
		return inboxWrote{}, fmt.Errorf("read the %s naming no node: %w", kind.links, err)
	}
	var tuples []graphstore.Tuple
	var items, blockIDs, toKinds, toIDs, kinds []string
	for rows.Next() {
		var item, blockID string
		var tuple graphstore.Tuple
		var maker []byte
		if err := rows.Scan(&item, &blockID, &tuple.FromID, &tuple.ToKind, &tuple.ToID, &tuple.Kind, &tuple.At,
			&tuple.LegacySourceSeq, &maker, &tuple.SourceEvent); err != nil {
			rows.Close()
			return inboxWrote{}, fmt.Errorf("read a %s row naming no node: %w", kind.links, err)
		}
		if err := json.Unmarshal(maker, &tuple.Who); err != nil {
			rows.Close()
			return inboxWrote{}, fmt.Errorf("read the maker of %s: %w", tuple.FromID, err)
		}
		tuple.FromKind, tuple.Asserted = refs.BlockNodeKind, true
		tuples = append(tuples, tuple)
		items, blockIDs = append(items, item), append(blockIDs, blockID)
		toKinds, toIDs, kinds = append(toKinds, tuple.ToKind), append(toIDs, tuple.ToID), append(kinds, tuple.Kind)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return inboxWrote{}, fmt.Errorf("read the %s naming no node: %w", kind.links, err)
	}
	if len(tuples) == 0 {
		return inboxWrote{}, nil
	}
	// The links are stated in this function's own body, beside the update that names their nodes
	// on the link rows: a person's through graphstore.ToPerson, every other through
	// graphstore.Write (personTuples).
	people, others := personTuples(tuples)
	if err := graphstore.Write(ctx, tx, nil, others); err != nil {
		return inboxWrote{}, fmt.Errorf("state a batch of %s: %w", kind.links, err)
	}
	if err := graphstore.ToPerson(ctx, tx, people); err != nil {
		return inboxWrote{}, fmt.Errorf("state a batch of %s naming a person: %w", kind.links, err)
	}
	if _, err := tx.Exec(ctx, `
		update `+kind.links+` r set node_kind = $6, node_address = r.from_node
		from unnest($1::uuid[], $2::text[], $3::text[], $4::text[], $5::text[]) as named (item, block, to_kind, to_id, kind)
		where r.`+kind.column+` = named.item and r.block_id = named.block and r.to_kind = named.to_kind
		  and r.to_id = named.to_id and r.kind = named.kind and r.node_kind is null
	`, items, blockIDs, toKinds, toIDs, kinds, refs.BlockNodeKind); err != nil {
		return inboxWrote{}, fmt.Errorf("name the nodes of a batch of %s on their rows: %w", kind.links, err)
	}
	return inboxWrote{rows: int64(len(tuples)), tuples: int64(len(tuples))}, nil
}

// citationsOwed is the refs rows out of an item of kind the citation pass owes: each whose
// tuples_stored is false (migration 20261116135959), out of an item whose node the store holds, to
// a far end the store can key (refs' planItemReplaceOver, inboxwrite's queueMarkCitations).
func (kind inboxItemKind) citationsOwed() string {
	return `
	from refs r
	join graph_nodes item on item.kind = r.from_kind and item.id = r.from_id
	where r.from_kind = '` + kind.kind + `' and not r.tuples_stored and octet_length(r.to_id) <= ` + addressBound
}

// itemCitationsPass states each refs row out of an item of kind an older build wrote as refs'
// PlanItemReplace states a citation, and as an answer's catch-up states an older ask's
// (queueMarkCitations): the mentions tuple of the row's own kind out of the item's node to its far
// end, dated by the row and carrying its source_seq, made by who made the item's node, under the
// event that recorded the item; and sets the row's tuples_stored.
func itemCitationsPass(kind inboxItemKind) inboxPass {
	return inboxPass{
		kind: kind.kind + "_refs",
		batch: `select r.from_id, r.to_kind, r.to_id ` + kind.citationsOwed() + `
			and (r.from_id, r.to_kind, r.to_id) > ($1, $2, $3)
			order by r.from_id, r.to_kind, r.to_id limit $4`,
		start:     []string{"", "", ""},
		write:     kind.writeCitations,
		remaining: kind.citationsOwed(),
	}
}

func (kind inboxItemKind) writeCitations(ctx context.Context, tx pgx.Tx, keys [][]string) (inboxWrote, error) {
	if err := kind.lockItems(ctx, tx, column(keys, 0)); err != nil {
		return inboxWrote{}, err
	}
	rows, err := tx.Query(ctx, `
		select r.from_id, r.to_kind, r.to_id, r.kind, r.created_at, r.source_seq, item.made_by, `+kind.eventOf(`r.from_id::uuid`)+`
		`+kind.citationsOwed()+`
		  and (r.from_id, r.to_kind, r.to_id) in (select * from unnest($1::text[], $2::text[], $3::text[]))
		order by r.from_id, r.to_kind, r.to_id
		for no key update of r
	`, column(keys, 0), column(keys, 1), column(keys, 2))
	if err != nil {
		return inboxWrote{}, fmt.Errorf("read the citations of %s items no tuple states: %w", kind.kind, err)
	}
	var tuples []graphstore.Tuple
	var items, toKinds, toIDs []string
	for rows.Next() {
		tuple := graphstore.Tuple{FromKind: kind.kind, Asserted: true}
		var maker []byte
		if err := rows.Scan(&tuple.FromID, &tuple.ToKind, &tuple.ToID, &tuple.Kind, &tuple.At, &tuple.LegacySourceSeq,
			&maker, &tuple.SourceEvent); err != nil {
			rows.Close()
			return inboxWrote{}, fmt.Errorf("read a citation of a %s no tuple states: %w", kind.kind, err)
		}
		if err := json.Unmarshal(maker, &tuple.Who); err != nil {
			rows.Close()
			return inboxWrote{}, fmt.Errorf("read who made %s %s: %w", kind.kind, tuple.FromID, err)
		}
		tuples = append(tuples, tuple)
		items, toKinds, toIDs = append(items, tuple.FromID), append(toKinds, tuple.ToKind), append(toIDs, tuple.ToID)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return inboxWrote{}, fmt.Errorf("read the citations of %s items no tuple states: %w", kind.kind, err)
	}
	if len(tuples) == 0 {
		return inboxWrote{}, nil
	}
	if err := writeRelations(ctx, tx, tuples); err != nil {
		return inboxWrote{}, fmt.Errorf("state a batch of %s citations: %w", kind.kind, err)
	}
	if _, err := tx.Exec(ctx, `
		update refs r set tuples_stored = true
		from unnest($1::text[], $2::text[], $3::text[]) as named (from_id, to_kind, to_id)
		where r.from_kind = $4 and r.from_id = named.from_id and r.to_kind = named.to_kind and r.to_id = named.to_id
		  and not r.tuples_stored
	`, items, toKinds, toIDs, kind.kind); err != nil {
		return inboxWrote{}, fmt.Errorf("mark a batch of %s citations stored: %w", kind.kind, err)
	}
	return inboxWrote{rows: int64(len(tuples)), tuples: int64(len(tuples))}, nil
}

// --- the delivery attempts ------------------------------------------------------------------------

// attemptTable is one of the two attempt tables: its name, the item it attempts to deliver, the
// key columns of a row, the address of a row's node as SQL over d (graphstore.AttemptKey.ID), the
// event that recorded the item, as SQL over an alias of its row, the key of a scanned row, and the
// join that reads the receipt of a row the listener took, which recordedSessions reads the session
// it reached by too: the first comment.delivery saying so under the comment's owner for a
// comment's mention, and none for a message's attempt, whose receipt recordedSessions reads
// nothing of (reachedSessions).
type attemptTable struct {
	table, items, item string
	keys, address      string
	event              func(alias string) string
	key                func(columns []string) (graphstore.AttemptKey, error)
	receipt            string
}

// The attempt numbers are read as the int4 the rows hold them in (parseAttempt), so the keys the
// pass states name exactly the rows it read.
var (
	commentAttempts = attemptTable{
		table: "comment_deliveries", items: "comments", item: "comment_id",
		keys:    `d.comment_id, d.target, d.attempt`,
		address: `'comment/' || d.comment_id::text || '/' || d.target || '/' || d.attempt::text`,
		event:   commentEventOf,
		key: func(columns []string) (graphstore.AttemptKey, error) {
			attempt, err := parseAttempt(columns[2])
			return graphstore.CommentAttempt(columns[0], columns[1], attempt), err
		},
		receipt: `
		join comments c on c.id = d.comment_id
		left join lateral (
			select e.id, e.created_at from events e
			where coalesce(e.issue_key, e.artifact_id::text, e.project_key) = coalesce(c.issue_key, c.artifact_id::text)
			  and e.type = 'comment.delivery' and e.payload->>'comment_id' = d.comment_id::text
			  and e.payload->>'session_id' = d.session_id and e.payload->>'state' in ` + model.DeliveryTakenStatesSQL + `
			order by e.id limit 1
		) receipt on true`,
	}
	messageAttempts = attemptTable{
		table: "message_deliveries", items: "messages", item: "message_id",
		keys:    `d.message_id, d.attempt`,
		address: `'message/' || d.message_id::text || '/' || d.attempt::text`,
		event:   messageEventOf,
		key: func(columns []string) (graphstore.AttemptKey, error) {
			attempt, err := parseAttempt(columns[1])
			return graphstore.MessageAttempt(columns[0], attempt), err
		},
		receipt: `
		cross join lateral (select null::bigint as id, null::timestamptz as created_at) receipt`,
	}
)

// parseAttempt reads an attempt row's number from its text, refusing one outside the int4 the
// column holds rather than reading it as some other number.
func parseAttempt(text string) (int, error) {
	attempt, err := strconv.ParseInt(text, 10, 32)
	return int(attempt), err
}

// owed is the rows of the table the attempt pass owes: each that names no node (migration
// 20261116102800) at an address the store can key (graphstore.AttemptOpened).
func (table attemptTable) owed() string {
	return `
	from ` + table.table + ` d
	where ` + table.owedRow()
}

// owedRow is owed's test of one row d.
func (table attemptTable) owedRow() string {
	return `d.node_kind is null and octet_length(` + table.address + `) <= ` + addressBound
}

// keyText is the table's key columns, each as text, in key order.
func (table attemptTable) keyText() string {
	if table.table == "comment_deliveries" {
		return `d.comment_id::text, d.target, d.attempt::text`
	}
	return `d.message_id::text, d.attempt::text`
}

// attemptsPass states each attempt row an older build wrote as the write that opened it states it
// (graphstore.AttemptOpened, QueueAttempted): the attempt's node, made at the row's created_at by
// the item's author for its first attempt, which the item's own write opened, and by the delivery
// machinery (graphstore.AttemptWriter) for a later one, which a Retry or a sender opened; then
// what the row holds now (graphstore.AttemptFactsAsWritten), dated at its created_at; the first
// attempt under the event that recorded its item, a later one at the sentinel, as its opening
// recorded none. The node is written before the row names it, as the row's foreign key asks.
func attemptsPass(table attemptTable) inboxPass {
	batch := `select ` + table.keyText() + table.owed()
	start := []string{zeroUUID, "", "0"}
	if table.table == "comment_deliveries" {
		batch += ` and (d.comment_id, d.target, d.attempt) > ($1::uuid, $2, $3::text::int) order by d.comment_id, d.target, d.attempt limit $4`
	} else {
		batch += ` and (d.message_id, d.attempt) > ($1::uuid, $2::text::int) order by d.message_id, d.attempt limit $3`
		start = []string{zeroUUID, "0"}
	}
	return inboxPass{kind: table.table, batch: batch, start: start, write: table.writeAttempts, remaining: table.owed()}
}

// named is the test over d of the rows keys names, and its arguments, from $1.
func (table attemptTable) named(keys [][]string) (string, []any) {
	if table.table == "comment_deliveries" {
		return `(d.comment_id, d.target, d.attempt) in (select * from unnest($1::uuid[], $2::text[], $3::text[]::int[]))`,
			[]any{column(keys, 0), column(keys, 1), column(keys, 2)}
	}
	return `(d.message_id, d.attempt) in (select * from unnest($1::uuid[], $2::text[]::int[]))`,
		[]any{column(keys, 0), column(keys, 1)}
}

// writeAttemptsAndSessions is the attempt pass's write of the rows keys names (writeAttempts) for
// the block-index tick, after the node of each session one of them the listener took reached that
// the store holds no node of (reachedSessions, writeSessionBatch). The backfill runs its session
// pass before its attempt pass, so every attempt it marks reached a session that holds its node; an
// attempt a build before this one settled after the backfill reached a session no pass gave one.
// It takes the rows `for no key update` in key order first, as the attempt pass's read takes them,
// so a row the sessions were read off is the row that pass marks.
func (table attemptTable) writeAttemptsAndSessions(ctx context.Context, tx pgx.Tx, keys [][]string) (inboxWrote, error) {
	named, args := table.named(keys)
	if _, err := tx.Exec(ctx, `
		select 1 from `+table.table+` d where `+named+`
		order by `+table.keys+`
		for no key update of d
	`, args...); err != nil {
		return inboxWrote{}, fmt.Errorf("lock a batch of %s: %w", table.table, err)
	}
	sessions, err := table.reachedSessions(ctx, tx, keys)
	if err != nil {
		return inboxWrote{}, err
	}
	var wrote inboxWrote
	if len(sessions) > 0 {
		if wrote.nodes, wrote.tuples, err = writeSessionBatch(ctx, tx, sessions); err != nil {
			return inboxWrote{}, err
		}
	}
	attempts, err := table.writeAttempts(ctx, tx, keys)
	if err != nil {
		return inboxWrote{}, err
	}
	wrote.add(attempts)
	return wrote, nil
}

func (table attemptTable) writeAttempts(ctx context.Context, tx pgx.Tx, keys [][]string) (inboxWrote, error) {
	named, args := table.named(keys)
	rows, err := tx.Query(ctx, `
		select `+table.keyText()+`, d.created_at, i.author, case when d.attempt = 1 then `+table.event("i")+` else `+noEvent+` end
		from `+table.table+` d
		join `+table.items+` i on i.id = d.`+table.item+`
		where `+table.owedRow()+` and `+named+`
		order by `+table.keys+`
		for no key update of d
	`, args...)
	if err != nil {
		return inboxWrote{}, fmt.Errorf("read the %s naming no node: %w", table.table, err)
	}
	// writer is who wrote the attempt as its row stores them - the item's author column for a first
	// attempt, which Postgres gives back in one spelling for one actor, and empty for the delivery
	// machinery - the key its facts are read under (AttemptFactsAsWritten).
	type attempt struct {
		key    graphstore.AttemptKey
		who    model.Actor
		writer string
		at     time.Time
		event  int64
	}
	var read []attempt
	width := len(keys[0])
	for rows.Next() {
		columns := make([]string, width)
		targets := make([]any, 0, width+3)
		for index := range columns {
			targets = append(targets, &columns[index])
		}
		var held attempt
		var author []byte
		targets = append(targets, &held.at, &author, &held.event)
		if err := rows.Scan(targets...); err != nil {
			rows.Close()
			return inboxWrote{}, fmt.Errorf("read a %s row naming no node: %w", table.table, err)
		}
		if held.key, err = table.key(columns); err != nil {
			rows.Close()
			return inboxWrote{}, fmt.Errorf("read the attempt number of a %s row: %w", table.table, err)
		}
		held.who = graphstore.AttemptWriter
		if held.key.Attempt == 1 {
			if err := json.Unmarshal(author, &held.who); err != nil {
				rows.Close()
				return inboxWrote{}, fmt.Errorf("read the author of %s: %w", held.key.ID(), err)
			}
			held.writer = string(author)
		}
		read = append(read, held)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return inboxWrote{}, fmt.Errorf("read the %s naming no node: %w", table.table, err)
	}
	if len(read) == 0 {
		return inboxWrote{}, nil
	}
	nodes := make([]graphstore.Node, 0, len(read))
	for _, held := range read {
		nodes = append(nodes, graphstore.AttemptNode(held.key, held.who, held.at, held.event))
	}
	if err := graphstore.Write(ctx, tx, nodes, nil); err != nil {
		return inboxWrote{}, fmt.Errorf("write the nodes of a batch of %s: %w", table.table, err)
	}
	if _, err := tx.Exec(ctx, `
		update `+table.table+` d set node_kind = '`+graphstore.KindDeliveryAttempt+`', node_address = `+table.address+`
		where `+named+` and d.node_kind is null
	`, args...); err != nil {
		return inboxWrote{}, fmt.Errorf("name the nodes of a batch of %s on their rows: %w", table.table, err)
	}
	// What the rows hold is read once per writer rather than once per row: AttemptFactsAsWritten
	// states the attempts of one writer in one statement however many it names, and a batch's
	// attempts have few writers - each item's author for its first attempt and the delivery
	// machinery for every later one. Each fact then carries the event of the attempt it is out of.
	var writers []string
	byWriter := map[string][]attempt{}
	eventOf := make(map[string]int64, len(read))
	for _, held := range read {
		if _, seen := byWriter[held.writer]; !seen {
			writers = append(writers, held.writer)
		}
		byWriter[held.writer] = append(byWriter[held.writer], held)
		eventOf[held.key.ID()] = held.event
	}
	var facts []graphstore.Tuple
	for _, writer := range writers {
		group := byWriter[writer]
		named := make([]graphstore.AttemptKey, len(group))
		for index, held := range group {
			named[index] = held.key
		}
		stated, err := graphstore.AttemptFactsAsWritten(ctx, tx, group[0].who, named...)
		if err != nil {
			return inboxWrote{}, err
		}
		for index := range stated {
			stated[index].SourceEvent = eventOf[stated[index].FromID]
		}
		facts = append(facts, stated...)
	}
	if err := graphstore.Write(ctx, tx, nil, facts); err != nil {
		return inboxWrote{}, fmt.Errorf("state a batch of %s: %w", table.table, err)
	}
	return inboxWrote{rows: int64(len(read)), nodes: int64(len(nodes)), tuples: int64(len(facts)) + statedOf(nodes)}, nil
}
