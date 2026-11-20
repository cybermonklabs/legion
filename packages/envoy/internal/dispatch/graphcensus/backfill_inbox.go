package graphcensus

import (
	"context"
	"encoding/json"
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

// The backfill of every kind dispatch://INFRA-1152 moves into the store but the mention tuples,
// by dispatch://INFRA-1149's contract (R9's view end): each pass walks only the rows its marker
// test passes, which are the rows a build of the release before this one wrote during a roll or
// before it; it dates each tuple by the row's own created_at, or by the moment of the mark the row
// stands for where the row holds none; it writes each tuple under the older write's own event,
// found in the log as INFRA-1149's passes find a row's event (readCommentRows), and under the
// store's sentinel where the log holds none; and it sets the row's marker in the transaction that
// states its tuples, so a second run reads no row a first one marked. It reads no column of a
// marked row, so it is no reconcile.
//
// Each pass states exactly what this release's writer of the row states for it, through the same
// writer's own statement of the facts: a message's pointers, its author and the page it hangs
// from (graphstore.MessageFacts, graphstore.AuthorFacts), a comment's pointers
// (graphstore.CommentPointerFacts), an item's blocks, their part_of and their links and its
// citations (as refs' item index states them), an attempt to reach a session or a role
// (graphstore.AttemptNode, graphstore.AttemptFactsAsWritten), what each comment, message, ask,
// answer and decline an older build wrote owed the threads it was written in
// (inboxwrite.RecordOlder), and what each read-state row says (graphstore.ToPerson,
// graphstore.FromPerson). Every batch takes the order the live writers take - the owner rows, then
// the project rows an ask's waiting person is read under, then the item rows and its block state,
// then the item's own store writes, then a thread's user_threads rows in ascending login, then the
// deliveries to them - so a live write of this release and a batch over one thread take turns.
//
// The order of the passes is the order their facts hang on each other: every item's node and
// author before its pointers (the passes of dispatch://INFRA-1149, dispatch://INFRA-3862 and
// dispatch://INFRA-1150 run first), the pointers before the deliveries that walk them, each
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

// askEventTypes is the type list of the partial index events_ask_payload_id (migration 0030),
// which a read of an ask's events by payload->>'id' repeats beside the one type it reads, so the
// planner can prove the index's predicate and read it.
const askEventTypes = `'ask.opened', 'ask.answered', 'ask.resolved', 'ask.edited'`

// askOpenedOf is the ask.opened that recorded the ask alias names, and the sentinel where the log
// holds none.
func askOpenedOf(alias string) string {
	return `coalesce((
		select e.id from events e
		where e.type in (` + askEventTypes + `) and e.type = 'ask.opened' and e.payload->>'id' = ` + alias + `.id::text
		order by e.id limit 1), ` + noEvent + `)`
}

// inboxPass is one pass of this child's backfill: the kind it reports, the read of one batch of the
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
		args := make([]any, 0, len(after)+1)
		for _, column := range after {
			args = append(args, column)
		}
		args = append(args, backfillBatch)
		rows, err := database.Pool.Query(ctx, pass.batch, args...)
		if err != nil {
			return done, fmt.Errorf("read the %s rows the backfill owes: %w", pass.kind, err)
		}
		keys, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) ([]string, error) {
			key := make([]string, len(after))
			targets := make([]any, len(key))
			for index := range key {
				targets[index] = &key[index]
			}
			return key, row.Scan(targets...)
		})
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

// column is the column-th of each key.
func column(keys [][]string, index int) []string {
	values := make([]string, len(keys))
	for at, key := range keys {
		values[at] = key[index]
	}
	return values
}

// backfillInbox runs this child's passes in the order their facts hang on each other, and answers
// what each did, once it has taken the statistics of every table they walk (analyzeInboxTables),
// and taking the store's again as the passes grow it (storeGrowth).
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

// inboxPasses is this child's passes in the order backfillInbox runs them, the order their facts
// hang on each other.
func inboxPasses() []inboxPass {
	passes := []inboxPass{messagesPass(), commentPointersPass()}
	for _, kind := range inboxItemKinds {
		passes = append(passes, itemBlocksPass(kind), itemLinksPass(kind), itemCitationsPass(kind))
	}
	passes = append(passes, attemptsPass(commentAttempts), attemptsPass(messageAttempts),
		asksOwedPass(), declinesPass(), userThreadsPass())
	for _, table := range readStateTables {
		passes = append(passes, readStatePass(table))
	}
	return passes
}

// inboxTables is every table this child's passes read or write: the rows each pass walks, with its
// item and block tables, the store's two tables, the log each row's event is read from, and the
// owner rows a batch takes and an ask's waiting person is read under.
func inboxTables() []string {
	tables := []string{"messages", "comments", "asks", "refs", "user_threads", "graph_nodes", "graph_tuples", "events",
		"issues", "artifacts", "projects"}
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
// key (events.LockOwnerRows), where read answers (id, issue, document) for the ids $1 - the issue
// that owns a row, or the document no issue owns - as every writer of these rows takes its owner
// first.
func lockOwnersOf(ctx context.Context, tx pgx.Tx, read string, ids []string) error {
	rows, err := tx.Query(ctx, read, ids)
	if err != nil {
		return fmt.Errorf("read the owners of a batch: %w", err)
	}
	owners := events.OwnerRows{}
	var id, issue, document string
	if _, err := pgx.ForEachRow(rows, []any{&id, &issue, &document}, func() error {
		owners.Add(issue, document, id)
		return nil
	}); err != nil {
		return fmt.Errorf("read the owners of a batch: %w", err)
	}
	return events.LockOwnerRows(ctx, tx, owners)
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
func writeRelations(ctx context.Context, tx pgx.Tx, nodes []graphstore.Node, tuples []graphstore.Tuple) error {
	people, others := personTuples(tuples)
	if len(nodes) > 0 || len(others) > 0 {
		if err := graphstore.Write(ctx, tx, writeNodesOnce(nodes), others); err != nil {
			return err
		}
	}
	return graphstore.ToPerson(ctx, tx, people)
}

// personTuples parts tuples into the mentions of a person, which graphstore.ToPerson alone
// appends, and every other.
func personTuples(tuples []graphstore.Tuple) (people, others []graphstore.Tuple) {
	for _, tuple := range tuples {
		if tuple.Kind == graphstore.TupleMentions && tuple.ToKind == graphstore.KindPerson {
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
// states it (api's writeMessageNode, createStoredMessage): one tuple per pointer the row holds and
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
func writeMessages(ctx context.Context, tx pgx.Tx, ids []string) (inboxWrote, error) {
	if err := lockOwnersOf(ctx, tx, messageOwners, ids); err != nil {
		return inboxWrote{}, err
	}
	rows, err := tx.Query(ctx, `
		select m.id::text, coalesce(m.issue_key, ''), m.author, m.created_at, coalesce(m.target, ''),
		       coalesce(m.in_reply_to::text, ''), coalesce(m.replies_to_block, ''),
		       coalesce(m.broadcast_id::text, ''), `+messageEventOf("m")+`
		`+messagesOwed+` and m.id = any($1::uuid[])
		order by m.id
		for no key update of m
	`, ids)
	if err != nil {
		return inboxWrote{}, fmt.Errorf("read the messages naming no node: %w", err)
	}
	type message struct {
		row    graphstore.MessageRow
		author model.Actor
		at     time.Time
		event  int64
	}
	var read []message
	for rows.Next() {
		var held message
		var author []byte
		if err := rows.Scan(&held.row.ID, &held.row.IssueKey, &author, &held.at, &held.row.Target,
			&held.row.InReplyTo, &held.row.RepliesToBlock, &held.row.BroadcastID, &held.event); err != nil {
			rows.Close()
			return inboxWrote{}, fmt.Errorf("read a message naming no node: %w", err)
		}
		if err := json.Unmarshal(author, &held.author); err != nil {
			rows.Close()
			return inboxWrote{}, fmt.Errorf("read the author of message %s: %w", held.row.ID, err)
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
	for index := range read {
		held := &read[index]
		if held.row.OnAgentPage() {
			page, ok, err := graphstore.AgentPage(ctx, tx, held.row.Target)
			if err != nil {
				return inboxWrote{}, err
			}
			if ok {
				held.row.Page = page
			}
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
		owed, err := inboxwrite.RecordOlder(ctx, tx, inboxwrite.Message(held.row.ID, held.author, held.at, issue, ""), held.event)
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
// (migration 20261116124447), whose node the store holds (dispatch://INFRA-1149's pass, which runs
// first), and whose replies_to_block the store can key (graphstore.CommentPointerFacts leaves a
// row past the bound whole to its old arms, as its writer does).
var commentPointersOwed = `
	from comments c
	where not c.tuples_stored and c.node_kind is not null
	  and octet_length(coalesce(c.replies_to_block, '')) <= ` + addressBound

// commentPointersPass states each comment an older build wrote as this release's comment writer
// states its pointers (api's commentStoreWrite): replies_to, answers and the one part_of
// (graphstore.CommentPointerFacts), and what it owed its thread (inboxwrite.RecordOlder), under the
// event that stored it, dated at its created_at; and sets its tuples_stored.
func commentPointersPass() inboxPass {
	return inboxPass{
		kind:  "comment_pointers",
		batch: `select c.id::text ` + commentPointersOwed + ` and c.id > $1::uuid order by c.id limit $2`,
		start: []string{zeroUUID},
		write: func(ctx context.Context, tx pgx.Tx, keys [][]string) (inboxWrote, error) {
			return writeCommentPointers(ctx, tx, column(keys, 0))
		},
		remaining: commentPointersOwed,
	}
}

// writeCommentPointers takes the owner rows of ids, then reads again and locks the comments rows of
// ids still owed, and states them.
func writeCommentPointers(ctx context.Context, tx pgx.Tx, ids []string) (inboxWrote, error) {
	if err := lockOwnersOf(ctx, tx, commentOwners, ids); err != nil {
		return inboxWrote{}, err
	}
	rows, err := tx.Query(ctx, `
		select c.id::text, c.author, c.created_at, coalesce(c.reply_to::text, ''), coalesce(c.ask_id::text, ''),
		       coalesce(c.answers::text, ''), coalesce(c.replies_to_block, ''), c.issue_key, c.artifact_id::text,
		       `+commentEventOf("c")+`
		`+commentPointersOwed+` and c.id = any($1::uuid[])
		order by c.id
		for no key update of c
	`, ids)
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
	}
	var read []comment
	for rows.Next() {
		var held comment
		var author []byte
		if err := rows.Scan(&held.id, &author, &held.at, &held.pointers.ReplyTo, &held.pointers.AskID,
			&held.pointers.Answers, &held.pointers.RepliesToBlock, &held.issueKey, &held.artifactID, &held.event); err != nil {
			rows.Close()
			return inboxWrote{}, fmt.Errorf("read a comment whose pointers are no tuples: %w", err)
		}
		if err := json.Unmarshal(author, &held.author); err != nil {
			rows.Close()
			return inboxWrote{}, fmt.Errorf("read the author of comment %s: %w", held.id, err)
		}
		read = append(read, held)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return inboxWrote{}, fmt.Errorf("read the comments whose pointers are no tuples: %w", err)
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
		owed, err := inboxwrite.RecordOlder(ctx, tx, inboxwrite.Comment(held.id, held.author, held.at, held.issueKey, held.artifactID, ""), held.event)
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
// uuid), read off the item's own row, which no marker of this child's is a column of.
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
// (refs' itemScope.blockNodeOf).
func (kind inboxItemKind) blocksOwed() string {
	return `
	from ` + kind.blocks + ` b
	join graph_nodes item on item.kind = '` + kind.kind + `' and item.id = b.` + kind.column + `::text
	where b.node_kind is null and octet_length(b.node_id) <= ` + addressBound
}

// itemBlocksPass states each block row of kind an older build wrote as refs' item index states a
// block it writes or takes over (indexItemBlocks): the block's node, read under its item's
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
// it inserts (replaceItemBlockRefs): one tuple out of the block's node, under the row's own kind and
// to its own far end, dated by the row and carrying its source_seq, made by the block's maker, under
// the event that recorded the item; and names the block's node on the row.
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
// a far end the store can key (refs' replaceRefsOf, inboxwrite's markCitations).
func (kind inboxItemKind) citationsOwed() string {
	return `
	from refs r
	join graph_nodes item on item.kind = r.from_kind and item.id = r.from_id
	where r.from_kind = '` + kind.kind + `' and not r.tuples_stored and octet_length(r.to_id) <= ` + addressBound
}

// itemCitationsPass states each refs row out of an item of kind an older build wrote as refs'
// replaceRefsOf states a citation, and as an answer's catch-up states an older ask's (markCitations):
// the mentions tuple of the row's own kind out of the item's node to its far end, dated by the
// row and carrying its source_seq, made by who made the item's node, under the event that recorded
// the item; and sets the row's tuples_stored.
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
	if err := writeRelations(ctx, tx, nil, tuples); err != nil {
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
// read of a row's key and the item's author and event, and the key of a scanned row.
type attemptTable struct {
	table, items, item string
	keys, address      string
	event              func(alias string) string
	key                func(columns []string) (graphstore.AttemptKey, error)
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

func (table attemptTable) writeAttempts(ctx context.Context, tx pgx.Tx, keys [][]string) (inboxWrote, error) {
	var named string
	var args []any
	if table.table == "comment_deliveries" {
		named = `(d.comment_id, d.target, d.attempt) in (select * from unnest($1::uuid[], $2::text[], $3::text[]::int[]))`
		args = []any{column(keys, 0), column(keys, 1), column(keys, 2)}
	} else {
		named = `(d.message_id, d.attempt) in (select * from unnest($1::uuid[], $2::text[]::int[]))`
		args = []any{column(keys, 0), column(keys, 1)}
	}
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
