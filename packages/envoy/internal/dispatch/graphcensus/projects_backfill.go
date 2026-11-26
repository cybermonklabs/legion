package graphcensus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The backfill of the project of every node: the nodes of the issues, documents, asks and messages
// written before their kind's writer wrote one (backfillObjectNodes), and the project of the
// comment and block nodes written before a node named one (backfillScopes). projects.go holds what
// a node must carry and the census that reads it.

// objectKind is a kind the backfill writes a node for from its rows.
type objectKind struct {
	// kind is the node's kind, and start the id the walk over its rows starts after.
	kind, start string
	// read is one batch of its rows its marker does not mark (unmarked), after the id $1 and at
	// most $2 of them, in the order of the index that keys them, as (id, created_at, author,
	// project, session, root author, root target, own event, row identity, issue, named). session is
	// a conversation's, for a document or an ask on one; the root columns are a message's on no issue
	// alone: the author and the target of its thread's root, its parties (graphstore.ThreadParties).
	// own event is a document's own create event (ownCreateEvent), null for the other kinds and for
	// a row that has none; the row's node is stated under it. issue is the issue that owns a
	// document, which its part_of names (graphstore.ArtifactPartOf), null for every other row.
	//
	// An issue's and a document's row also names its node (node_kind and node_address, migration
	// 20261116092210), and such a row is read while it names none, so a row whose node an earlier
	// build wrote with no part_of and named nowhere is walked too; an issue's row is read as well
	// while it names a parent its node does not hang under, which the release before's PATCH leaves
	// (issueRowUnmarked). named is whether the row is read for its name alone: it names no node,
	// and its node marker is set (not unmarked, not unmarkedDocument) or it is a retired
	// document's, so its batch names it and writes no node for it (writeObjectBatch); it is false
	// for an ask and a message. A row that would be read for its name alone is not read where the
	// store holds its node in another project (nodeInAnotherProject): no pass can name it, since
	// the write of the node beside the name is refused (graphstore.ErrProjectMoved), so every run
	// would answer it moved; the census names it differs, which no command cures.
	read string
	// rows answers each address of ids $1 with its stable row identity. A document's UUID
	// distinguishes replacements even when their created_at values are equal.
	rows string
	// owners answers, for a kind whose batch writes under its rows' owners, the owner each row of
	// the ids $1 stands under, as (issue, document id): for a document the owner whose lock a
	// writer that deletes its row takes before anything else (api's removeArtifact takes its
	// document's issue's), and for an issue its own row, under which the batch reads the parent its
	// child_of names (heldRowMarks). It is empty for a kind whose batch names nothing on a row.
	owners string
}

// unmarked is the backfill's marker for these kinds, unset: the store holds no node of kind at the
// address id, at a state but gone, that names a project or parties. The node is the marker, since
// no ask's or message's row names its node in a column of its own, and an issue's and a
// document's row adds a marker of its own beside it (objectKind.read); one not gone, since a row whose node
// is gone is one a build before this one created at an address this build's removal stated gone,
// and is owed a live node as much as a row with none (classGone), while a state a later release
// adds, such as a requirement's proposed, is a node there as live is; and one naming a project or
// parties, since a node naming neither is the census's unscoped class, which this pass cures by
// stating them.
func unmarked(kind, id string) string {
	return `not exists (select 1 from graph_nodes n where n.kind = '` + kind + `' and n.id = ` + id +
		` and n.state <> '` + graphstore.StateGone + `' and ` + scopedNode + `)`
}

// scopedNode is, for the node n, whether it names a project or parties.
const scopedNode = `(n.project_key is not null or n.parties is not null)`

// nodeInAnotherProject is, for the row whose project is project and whose node is of kind at the
// address id, whether the store holds that node, at any state, in a project other than the row's:
// a node no pass can write, since a node's project never moves (writableRows' moved rows,
// graphstore.ErrProjectMoved). A row of no project, a conversation's, has none to differ.
func nodeInAnotherProject(kind, id, project string) string {
	return `exists (select 1 from graph_nodes n where n.kind = '` + kind + `' and n.id = ` + id + ` and n.project_key <> ` + project + `)`
}

// unmarkedDocument is unmarked for a document, whose address a later row takes again where a
// removal can take the row away: the row is not retired (retiredDocument, whose node the
// retirement stated gone on purpose and which is owed no live one), and the store holds no node
// not gone naming a project or parties at the row a's address that is that row's own - any such
// node, for a row no removal takes, and for one a removal can take, one whose live statement was
// made for it (ownLiveStatement). A node whose live statement is not the row's is a document a
// build before this one removed and then made again at the address, and that row is owed its own
// live statement as much as a row with none (classGone).
func unmarkedDocument(a string) string {
	return `not ` + retiredDocument(a) + ` and not exists (select 1 from graph_nodes n where n.kind = '` + graphstore.KindArtifact + `' and n.id = ` + a + `.ref_key` +
		` and n.state <> '` + graphstore.StateGone + `' and ` + scopedNode + ` and (not ` + removableDocument(a) + ` or ` +
		ownLiveStatement("n", a+".id::text", a+".issue_key") + `))`
}

// ownCreateEvent is the id of the artifacts row a's own create event, the artifact.created whose
// payload names the row, or null for a row that has none: a document no create recorded an event
// of (an issue's spec, which issue.created records, a conversation's file, whose owner records no
// event) and one made before Dispatch recorded artifact.created. An issue's document's event is its
// issue's, and a project's document's is its own (events.artifact_id), each read by the index that
// keys that owner's events (the events table's unique (issue_key, seq) and (artifact_id, seq)),
// whose rows hold every event, published or not.
func ownCreateEvent(a string) string {
	return `(case when ` + a + `.issue_key is not null then (
			select e.id from events e
			where e.issue_key = ` + a + `.issue_key and e.type = 'artifact.created' and e.payload->'artifact'->>'id' = ` + a + `.id::text
			order by e.id limit 1)
		else (
			select e.id from events e
			where e.artifact_id = ` + a + `.id and e.type = 'artifact.created'
			order by e.id limit 1)
		end)`
}

// objectKinds are the kinds the backfill writes a node for from their rows.
var objectKinds = []objectKind{
	{kind: graphstore.KindIssue, read: `
		select i.key, i.created_at, i.created_by, i.project_key, null::text, null::jsonb, null::text, null::bigint, i.key, null::text,
		       not (` + unmarked(graphstore.KindIssue, "i.key") + `) and i.node_kind is null
		from issues i
		where i.key > $1 and (` + unmarked(graphstore.KindIssue, "i.key") + `
		  or (` + issueRowUnmarked + ` and not ` + nodeInAnotherProject(graphstore.KindIssue, "i.key", "i.project_key") + `))
		order by i.key limit $2`,
		rows:   `select key, key from issues where key = any($1::text[])`,
		owners: `select key, '' from issues where key = any($1::text[])`},
	documentObjectKind,
	{kind: graphstore.KindAsk, start: zeroUUID, read: `
		select k.id::text, k.created_at, k.author, coalesce(i.project_key, a.project_key),
		       case when coalesce(i.project_key, a.project_key) is null then a.session_id end, null::jsonb, null::text, null::bigint, k.id::text,
		       null::text, false
		from asks k left join issues i on i.key = k.issue_key left join artifacts a on a.id = k.artifact_id
		where k.id > $1::uuid and ` + unmarked(graphstore.KindAsk, "k.id::text") + `
		order by k.id limit $2`,
		rows: `select id::text, id::text from asks where id = any($1::text[]::uuid[])`},
	{kind: graphstore.KindMessage, start: zeroUUID, read: `
		with recursive batch as (
			select m.id, m.issue_key, m.in_reply_to, m.created_at, m.author
			from messages m
			where m.id > $1::uuid and ` + unmarked(graphstore.KindMessage, "m.id::text") + `
			order by m.id limit $2
		),
		walk (message, at, in_reply_to) as (
			select batch.id, batch.id, batch.in_reply_to from batch where batch.issue_key is null
			union all
			select walk.message, parent.id, parent.in_reply_to from walk join messages parent on parent.id = walk.in_reply_to
		)
		select batch.id::text, batch.created_at, batch.author, i.project_key, null::text, root.author, root.target, null::bigint, batch.id::text,
		       null::text, false
		from batch
		left join issues i on i.key = batch.issue_key
		left join walk on walk.message = batch.id and walk.in_reply_to is null
		left join messages root on root.id = walk.at
		order by batch.id`,
		rows: `select id::text, id::text from messages where id = any($1::text[]::uuid[])`},
}

// documentObjectKind is the document's object kind, which the version pass also takes the owner
// locks of (writeVersionBatch): a version stands under its document's owner.
var documentObjectKind = objectKind{kind: graphstore.KindArtifact, read: `
		select a.ref_key, a.created_at, a.created_by, a.project_key, a.session_id, null::jsonb, null::text, ` + ownCreateEvent("a") + `, a.id::text,
		       a.issue_key, not (` + unmarkedDocument("a") + `) and a.node_kind is null
		from artifacts a
		where a.ref_key > $1 and octet_length(a.ref_key) <= ` + strconv.Itoa(graphstore.MaxAddressBytes) + `
		  and (` + unmarkedDocument("a") + `
		       or (a.node_kind is null and not ` + nodeInAnotherProject(graphstore.KindArtifact, "a.ref_key", "a.project_key") + `))
		order by a.ref_key limit $2`,
	rows:   `select ref_key, id::text from artifacts where ref_key = any($1::text[])`,
	owners: `select coalesce(issue_key, ''), id::text from artifacts where ref_key = any($1::text[])`}

// issueRowUnmarked is the test that the issues row aliased i is one its row marker does not mark:
// it names no node, or it names a parent its node's child_of does not name, which only the release
// before's issue PATCH leaves, setting parent_key in place on a marked row (store.IssueParentMoved).
// graph_edges' old child_of arm answers such a row, and the object pass states the child_of that
// names its parent, after which the store answers it.
const issueRowUnmarked = `(i.node_kind is null or (i.parent_key is not null and ` + store.IssueParentMoved + `))`

// ObjectRowStands answers whether a row of kind stands at id, for a kind the backfill writes a
// node for from its rows (objectKinds), and false for any other kind. The sweep of an orphaned
// document's node reads it under the node's lock (StateOrphanedGone). An ask's and a message's row
// is keyed by a uuid, so an id that is no uuid names no row of them; it answers false for it rather
// than let the cast in objectKind.rows abort its caller's transaction.
func ObjectRowStands(ctx context.Context, q graphstore.Queryer, kind, id string) (bool, error) {
	for _, known := range objectKinds {
		if known.kind != kind {
			continue
		}
		if known.start == zeroUUID {
			if _, err := uuid.Parse(id); err != nil {
				return false, nil
			}
		}
		standing, err := readStanding(ctx, q, known, []string{id})
		if err != nil {
			return false, fmt.Errorf("read the %s row of %s: %w", kind, id, err)
		}
		return len(standing) > 0, nil
	}
	return false, nil
}

// readStanding reads each address of ids with the stable identity of its current row.
func readStanding(ctx context.Context, q graphstore.Queryer, kind objectKind, ids []string) (map[string]string, error) {
	rows, err := q.Query(ctx, kind.rows, ids)
	if err != nil {
		return nil, err
	}
	standing := make(map[string]string, len(ids))
	var id, identity string
	if _, err := pgx.ForEachRow(rows, []any{&id, &identity}, func() error {
		standing[id] = identity
		return nil
	}); err != nil {
		return nil, err
	}
	return standing, nil
}

// zeroUUID is where a walk over a uuid key starts: every id sorts after it.
const zeroUUID = "00000000-0000-0000-0000-000000000000"

// backfillObjectNodes writes a live node for every issue, document, ask and message written before
// its kind's writer wrote one, and for every one a build before this one created at an address
// whose node is gone or holds another row's live statement, under the backfill's contract: it
// walks only the rows its marker does not mark - the marker is a node not gone naming a project or
// parties (unmarked), and for a document a removal can take such a node whose live statement is
// the row's own (unmarkedDocument)
// - and writes the marker in the transaction that writes the node, so a second run reads no row the
// first one wrote. Its input is the row's own content: a node it makes is made by the row's author
// at the row's created_at, and a node the store holds keeps the made_at and made_by it has. The
// row's author states the node live, and the backfill itself states gone a document's node that
// stood live for an earlier row; each is dated at the row's created_at or one microsecond past the
// newest statement about the node where that is later, so a read as of a time reads the order the
// store appended in. A document's statements are caused by its own create event where the read
// finds one, which is what names the row (ownLiveStatement); an issue's, an ask's and a message's,
// and a document's with no such event, are at the sentinel. It appends no event. Beside a
// document's node it states what its kind's writer states: its part_of the issue that owns it and
// an image's or a file's newest bytes; beside an issue's, its child_of to the parent its row names
// under the issue's own lock, as a claim; and it names each issue's and document's node on its row
// (writeObjectBatch), which is that row's own marker beside the node.
//
// A row the store refuses because it holds the row's node in another project
// (graphstore.ErrProjectMoved) is left, and every other row is written: the batch that holds it is
// written again one row at a time. No writer makes such a node - a document's address holds its
// issue's key and an issue's project never changes - so it is a store a person has to read. The
// run answers every such row joined, after writing all the others.
func backfillObjectNodes(ctx context.Context, database *store.Store) ([]KindBackfill, error) {
	var done []KindBackfill
	var moved []error
	for _, kind := range objectKinds {
		report, _, refused, err := writeObjectKind(ctx, database, kind, nil)
		if err != nil {
			return done, err
		}
		moved = append(moved, refused...)
		var remaining int64
		if err := database.Pool.QueryRow(ctx, `select count(*) from (`+kind.read+`) as remaining`, kind.start, nil).Scan(&remaining); err != nil {
			return done, fmt.Errorf("count the %s rows their marker does not mark: %w", kind.kind, err)
		}
		report.Remaining = remaining
		report.Complete = report.Remaining == 0
		done = append(done, report)
	}
	return done, errors.Join(moved...)
}

// ObjectPass is what one run of WriteOwedObjectNodes did: how many rows it wrote a node for,
// whether it stopped at its batch budget with rows of its kinds still to read (More), and every
// row it left because the store holds its node in another project (Moved, nil where there is
// none).
type ObjectPass struct {
	Rows  int64
	More  bool
	Moved error
}

// WriteOwedObjectNodes is the backfill's object pass (backfillObjectNodes) without its count of
// the rows that remain, for docs' block-index tick, which runs it where a build before this one
// may have written rows with no node (docs' readEventsAfterCursor): on every tick while a process
// of another build serves and the first tick after one did, on a tick whose events leave the store
// a row with no node or a node with no row (EventsOweObjectPass), on each process's first tick,
// and once a process has finished no pass for docs' objectPassInterval. That build
// creates an issue, a document, an ask and a message with no node, and a conversation's file with
// no event either, so the pass reads rows, not events: it writes, through writeObjectBatch alone,
// the node of every row its marker does not mark, as the backfill does.
//
// It opens at most batches transactions across the four kinds for the batches that write a node,
// and at most batches more for the batches whose rows are each owed their name alone (objectBudget,
// writeObjectKind), zero being no bound on either, so one tick never holds the block-index lease
// for the whole backlog a first deploy leaves, whether that backlog is owed nodes or names: it
// answers More where it stopped with rows left, and the next run reads on, from the start of each
// kind's key, past the rows the last one marked. Rows counts only the rows it wrote a node for, as
// writeObjectBatch's report does, and none it only named. A kind that fails holds back none of the
// others: the run goes on to them and answers every failure joined, and the next run reads the
// failed kind's rows again. A row the store refuses because it holds the row's node in another
// project is left, before any transaction opens, and answered in Moved, and every other row of its
// batch is written (writeObjectKind): no second run cures it, so it holds back no run, and the
// census names that row until a person reads it.
func WriteOwedObjectNodes(ctx context.Context, database *store.Store, batches int) (ObjectPass, error) {
	var pass ObjectPass
	var failed, moved []error
	var budget *objectBudget
	if batches > 0 {
		budget = &objectBudget{nodes: batches, names: batches}
	}
	for _, kind := range objectKinds {
		report, more, refused, err := writeObjectKind(ctx, database, kind, budget)
		pass.Rows += report.Rows
		pass.More = pass.More || more
		moved = append(moved, refused...)
		if err != nil {
			failed = append(failed, err)
		}
	}
	pass.Moved = errors.Join(moved...)
	return pass, errors.Join(failed...)
}

// objectBudget is what is left of an object pass's two budgets of batches (WriteOwedObjectNodes),
// shared by its four kinds: nodes, of the batches that write a node, and names, of the batches
// whose rows are each owed their name alone (objectKind.read's named).
type objectBudget struct{ nodes, names int }

// spends answers the budget a batch of rows spends: names where every row of it is owed its name
// alone, and nodes where any row is owed a node.
func (b *objectBudget) spends(rows []objectRow) *int {
	for _, row := range rows {
		if !row.named {
			return &b.nodes
		}
	}
	return &b.names
}

// writeObjectKind walks kind's rows its marker does not mark, one batch at a time from the
// start of its key, and writes each batch's nodes (writeObjectBatch), answering what it wrote.
// Each batch that opens a transaction spends one batch of budget, nil being no bound: of its
// names where every row of it is owed its name alone, and of its nodes otherwise
// (objectBudget.spends). Once the nodes are spent the walk reads whether a row a pass can write is
// left for the next run (more) and writes nothing more. While only the names are spent it leaves
// each batch whose rows are all owed their name alone for the next run, which answers more, and
// goes on to the batches after it, so a run of such rows holds back no node after them. So a walk,
// and a pass of the four kinds that share one budget, opens at most as many transactions as the
// budget's nodes and names, but for a batch written again one row at a time below.
//
// A row whose node the store holds but whose row names it nowhere - every issue and document a
// build that names no node on its row wrote, at its create or by its backfill - is owed that name
// alone, which writeObjectBatch counts as no row and no node, but its batch costs a transaction,
// the owner locks and a write of each node all the same. The first pass after this build deploys
// over that build reads every issue and document of the deployment as such a row, so a batch of
// them spends a budget of its own, rather than none, which would run a transaction for each
// backfillBatch of them in one pass, or the nodes', which would hold back behind them the rows
// owed a node. A batch that holds a row owed a node beside them spends the nodes alone, and names
// them in the same transaction.
//
// A row no pass can write - between a party the store refuses, or whose node the store holds in
// another project, live or gone - stays unmarked, so every run reads it again from the start of
// the key. So the walk writes only the rows of each batch a pass can write (writableRows), and
// leaves the rest before any transaction opens: a refused party counted in Skipped, a node in
// another project answered in moved. A batch with no other row opens no transaction and spends no
// budget, and the read past the budget skips such rows (writableRowLeft). Otherwise those rows
// would take the budget every run, hold back the rows after them, keep more, and with it the
// tick's debt, for ever, and each would cost a transaction, its owner's lock and a transaction id
// on every run. What such rows still cost each run is the read of their batches and of the
// projects the store holds their nodes in: two index reads, with no lock and no transaction, for
// each backfillBatch of them. A batch the store still refuses because a node of it was made in
// another project after that read (graphstore.ErrProjectMoved) is written again one row at a time,
// so that row alone is left, answered in moved, and every other row of the batch is written.
func writeObjectKind(ctx context.Context, database *store.Store, kind objectKind, budget *objectBudget) (report KindBackfill, more bool, moved []error, err error) {
	report = KindBackfill{Kind: kind.kind}
	after := kind.start
	add := func(written KindBackfill) {
		report.Rows += written.Rows
		report.Nodes += written.Nodes
		report.Tuples += written.Tuples
		report.Skipped += written.Skipped
	}
	// left is whether the walk left a batch of rows owed their name alone, its names spent, for
	// the next run.
	left := false
	for {
		if budget != nil && budget.nodes == 0 {
			rest, err := writableRowLeft(ctx, database, kind, after)
			return report, left || rest, moved, err
		}
		nodes, err := readObjectNodes(ctx, database, kind.kind, kind.read, after, backfillBatch)
		if err != nil {
			return report, false, moved, err
		}
		if len(nodes) == 0 {
			return report, left, moved, nil
		}
		after = nodes[len(nodes)-1].ID
		writable, skipped, unmoved, err := writableRows(ctx, database, kind, nodes)
		if err != nil {
			return report, false, moved, err
		}
		report.Skipped += skipped
		moved = append(moved, unmoved...)
		if len(writable) > 0 && budget != nil {
			// Only the names can be spent here: spent nodes end the walk above.
			if spent := budget.spends(writable); *spent > 0 {
				*spent--
			} else {
				left, writable = true, nil
			}
		}
		if len(writable) > 0 {
			written, err := writeObjectBatch(ctx, database, kind, writable)
			switch {
			case errors.Is(err, graphstore.ErrProjectMoved):
				for _, node := range writable {
					one, err := writeObjectBatch(ctx, database, kind, []objectRow{node})
					switch {
					case errors.Is(err, graphstore.ErrProjectMoved):
						moved = append(moved, err)
					case err != nil:
						return report, false, moved, err
					default:
						add(one)
					}
				}
			case err != nil:
				return report, false, moved, err
			default:
				add(written)
			}
		}
		if len(nodes) < backfillBatch {
			return report, left, moved, nil
		}
	}
}

// writableRowLeft answers whether a row of kind after the id after is one a pass can write: a row
// its marker does not mark whose parties the store takes and whose node, if the store holds one,
// names no other project (writableRows). It reads the kind's rows one batch at a time until it
// finds one or none is left.
func writableRowLeft(ctx context.Context, database *store.Store, kind objectKind, after string) (bool, error) {
	for {
		nodes, err := readObjectNodes(ctx, database, kind.kind, kind.read, after, backfillBatch)
		if err != nil || len(nodes) == 0 {
			return false, err
		}
		writable, _, _, err := writableRows(ctx, database, kind, nodes)
		if err != nil {
			return false, err
		}
		if len(writable) > 0 {
			return true, nil
		}
		if len(nodes) < backfillBatch {
			return false, nil
		}
		after = nodes[len(nodes)-1].ID
	}
}

// writableRows answers, of a batch of kind's rows, the ones a pass can write (writable), and
// leaves the others: each that names a party the store refuses (partiesRefused), counted in
// skipped, since one such row of an earlier build would otherwise fail every batch it falls in
// and every run of the backfill with it, and the census names it; and each that names a project
// other than the one the store holds its node in, live or gone, answered in moved as the store's
// refusal of that write (graphstore.ErrProjectMoved). It reads the projects the store holds the
// batch's nodes in, with no lock and outside any transaction, so a row it leaves costs that read
// alone; the rows it answers are written and read again under their locks (writeObjectBatch).
func writableRows(ctx context.Context, database *store.Store, kind objectKind, nodes []objectRow) (writable []objectRow, skipped int64, moved []error, err error) {
	ids := make([]string, len(nodes))
	for index, node := range nodes {
		ids[index] = node.ID
	}
	held, err := heldProjects(ctx, database.Pool, kind.kind, ids)
	if err != nil {
		return nil, 0, nil, err
	}
	for _, node := range nodes {
		switch project, found := held[node.ID]; {
		case partiesRefused(node.Parties):
			skipped++
		case found && node.Project != "" && project != node.Project:
			moved = append(moved, fmt.Errorf("write graph node %s/%s in project %s, which the store holds in %s: %w",
				kind.kind, node.ID, node.Project, project, graphstore.ErrProjectMoved))
		default:
			writable = append(writable, node)
		}
	}
	return writable, skipped, moved, nil
}

// heldProjects answers, by id, the project the store holds each node of kind at ids in, for the
// nodes that hold one.
func heldProjects(ctx context.Context, q graphstore.Queryer, kind string, ids []string) (map[string]string, error) {
	rows, err := q.Query(ctx, `
		select id, project_key from graph_nodes where kind = $1 and id = any($2) and project_key is not null
	`, kind, ids)
	if err != nil {
		return nil, fmt.Errorf("read the projects of the %s nodes the store holds: %w", kind, err)
	}
	held := map[string]string{}
	var id, project string
	if _, err := pgx.ForEachRow(rows, []any{&id, &project}, func() error {
		held[id] = project
		return nil
	}); err != nil {
		return nil, fmt.Errorf("read the projects of the %s nodes the store holds: %w", kind, err)
	}
	return held, nil
}

// statedOf is how many base attributes the node write states for nodes it makes: visibility and
// state for each, and its project or its parties.
func statedOf(nodes []graphstore.Node) int64 {
	var stated int64
	for _, node := range nodes {
		stated += 2
		if node.Project != "" || len(node.Parties) > 0 {
			stated++
		}
	}
	return stated
}

// objectRow keeps the row's identity separate from its reusable graph address, and, for a
// document an issue owns, that issue, which its part_of names (graphstore.ArtifactPartOf), and
// whether the row was read for its name alone (objectKind.read's named).
type objectRow struct {
	graphstore.Node
	identity string
	issue    string
	named    bool
}

// readObjectNodes reads one batch of a kind's rows their marker does not mark, and answers the node its
// kind's writer writes for each (graphstore.IssueNode and its siblings), caused by the row's own
// create event where the read found one, and at the sentinel otherwise, with the issue that owns
// each document an issue owns and whether the row was read for its name alone.
func readObjectNodes(ctx context.Context, database *store.Store, kind, read, after string, limit int) ([]objectRow, error) {
	rows, err := database.Pool.Query(ctx, read, after, limit)
	if err != nil {
		return nil, fmt.Errorf("read the %s rows their marker does not mark: %w", kind, err)
	}
	defer rows.Close()
	var nodes []objectRow
	for rows.Next() {
		var id, identity string
		var createdAt time.Time
		var authorJSON, rootAuthorJSON []byte
		var project, session, rootTarget, issue *string
		var ownEvent *int64
		var named bool
		if err := rows.Scan(&id, &createdAt, &authorJSON, &project, &session, &rootAuthorJSON, &rootTarget, &ownEvent, &identity, &issue, &named); err != nil {
			return nil, fmt.Errorf("read a %s row its marker does not mark: %w", kind, err)
		}
		var author model.Actor
		if err := json.Unmarshal(authorJSON, &author); err != nil {
			return nil, fmt.Errorf("read the author of %s %s: %w", kind, id, err)
		}
		_, parties := graphstore.SessionScope(derefText(project), derefText(session))
		var node graphstore.Node
		switch kind {
		case graphstore.KindIssue:
			node = graphstore.IssueNode(id, derefText(project), author, createdAt)
		case graphstore.KindArtifact:
			node = graphstore.ArtifactNode(id, derefText(project), derefText(session), author, createdAt)
		case graphstore.KindAsk:
			node = graphstore.AskNode(id, derefText(project), parties, author, createdAt)
		case graphstore.KindMessage:
			if project == nil {
				if parties, err = threadParties(id, rootAuthorJSON, rootTarget); err != nil {
					return nil, err
				}
			}
			node = graphstore.MessageNode(id, derefText(project), parties, author, createdAt)
		default:
			return nil, fmt.Errorf("the backfill writes no node of kind %q", kind)
		}
		if ownEvent != nil {
			node.SourceEvent = *ownEvent
		}
		nodes = append(nodes, objectRow{Node: node, identity: identity, issue: derefText(issue), named: named})
	}
	return nodes, rows.Err()
}

// threadParties is the parties of message id on no issue: those of its thread's root, whose author
// and target the read answers (graphstore.ThreadParties). A root author the read did not find is
// the zero actor.
func threadParties(id string, rootAuthorJSON []byte, rootTarget *string) ([]string, error) {
	var root model.Actor
	if len(rootAuthorJSON) > 0 {
		if err := json.Unmarshal(rootAuthorJSON, &root); err != nil {
			return nil, fmt.Errorf("read the thread root's author of message %s: %w", id, err)
		}
	}
	return graphstore.ThreadParties(root, rootTarget), nil
}

func derefText(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// writeObjectBatch writes one batch's nodes in one transaction, which takes their rows in ascending
// (kind, id) and states each one's base attributes, and answers what it wrote. A node that is its
// row's own and not gone is written restating it (graphstore.Node's Restates), as the backfill's
// other passes write theirs: the run owns the row's project or parties, not the state or the
// visibility another write gave the node, so a node at a state a later release adds keeps it. Any
// other node is written as the row makes it: one the store does not hold, one held gone, and one
// another row left standing are a node for a row that is new to it, which owes nothing to the row
// before but the address, as this build's create writes it (api's storeArtifact). A node a live
// writer wrote meanwhile is written again with nothing moved, and states nothing; a gone node is
// moved live, which states its state alone; one in another project fails the batch
// (graphstore.ErrProjectMoved), which is a difference for the census to report rather than for
// this run to paper over. Each node and what it states carry its row's own create event where the
// read found one (readObjectNodes), and the sentinel otherwise: the batch writes its nodes in
// ascending id, in runs of consecutive nodes of one cause, each run's nodes queued as one write
// (graphstore.PlanUncaused and graphstore.QueueNodes) and then stated through graphstore.QueueCaused
// under that cause, and the report counts the tuples the store appended for those runs
// (graphstore.Counting).
//
// The batch was read before this transaction opened, and a removal may commit in between: a node
// written live then would stand behind no row. So for a kind a writer deletes rows of
// (objectKind.owners) the transaction first takes the owner lock each of its rows stands under
// (lockObjectOwners): api's removeArtifact takes the document's issue owner lock before it states
// the node gone and deletes the row, and a removal of a build before this one takes it too and
// states nothing. Under those locks it reads the stable identity of each row that still stands
// (objectKind.rows), and writes only the node of the same row the batch read. A replacement
// document has another UUID even when its created_at is identical; it is left for the next run,
// which the report's remaining count names. A removal waiting for this batch removes a row whose
// node is live, and this build's removal then states that node gone. The owner locks come first
// and the nodes after them, the order every writer of these nodes takes (a document's create and
// its removal). A kind no writer deletes a row of takes no lock and is read again the same way,
// but an issue, whose batch takes its own row as its owner lock, so the parent its child_of names
// is read under it (heldRowMarks).
//
// Beside each node it states what its kind's writer states: a document's part_of the issue that
// owns it, with the node's time, its writer and its cause (graphstore.ArtifactPartOf), among the
// run's own tuples, and, once the nodes are written, an image's or a file's newest bytes
// (nameArtifactNodes); an issue's child_of to the parent its row names, as a claim
// (issueParentClaims), so graph_edges answers that parent from the store once the row names its
// node, and a parent the release before moved in place on a marked row replaces the one the node
// held. Then it names each node on its issue's or document's row (node_kind and node_address),
// after the node rows, as the lock order on api/inbox_read.go's markInboxReadThreads has a
// projection row follow its node, and last it writes the head rows of every node it named, once
// (graphstore.WriteHeads), the last rows a write takes. A document retired since the batch was read is owed no live
// node, since its retirement stated it gone (retiredDocument): no node of it is written, and its
// row, where it names no node yet, is named with the rest.
//
// Under those locks it reads the nodes the store holds of the batch (heldNodes). One that is not
// gone, its row's own (ownLiveStatement, for a document a removal can take; any such one for the
// rest, whose address no later row takes) and naming a project or parties was written meanwhile, by
// another run of the backfill that took the owner locks first, and is left as it is, unless its
// issue's or document's own row is still unmarked (heldRowMarks). Such a node whose row is unmarked
// only in naming no node - one a build that names no node on its row wrote, at its create or by
// its backfill - is owed that name and not a node: it is written again with the rest, which moves
// nothing of it and states only what its kind's writer states beside it, and named, and it counts
// as no row and no node of the report; a batch of such rows alone spends the object pass's budget
// of names, not its budget of nodes (writeObjectKind). One that is not gone and its row's own but
// names neither is written again, which states its project or parties alone. A document whose
// node is not gone but not its row's own is one a build before this one removed and made again at
// the address: its node is stated gone, by the backfill, and then live, under the row's own event,
// as this build's create states it (api's queueNewArtifact), both as the run's own state tuples
// (stateTuple). A node the store holds is dated one microsecond past the newest statement it holds
// about it where the row's created_at is no later - a row a build before this one dated at its
// transaction's start can predate a removal it waited behind - so a read of the node as of a time
// reads the statements in the order the store appended them. A document's part_of is its row's
// from its creation, so it is dated at the row's created_at, as its create dates it, and moved past
// the newest statement of its node only where that statement is later: one at that very time comes
// before it in the log's own order (seq), and a read as of that time reads the document under its
// issue, as its row has it.
//
// It is handed only the rows writableRows answers, so no row between a party
// graphstore.PartiesValue refuses reaches it: a row's parties are its own content, which the
// read under the locks does not change.
func writeObjectBatch(ctx context.Context, database *store.Store, kind objectKind, nodes []objectRow) (KindBackfill, error) {
	ids := make([]string, len(nodes))
	for index, node := range nodes {
		ids[index] = node.ID
	}
	var written KindBackfill
	if err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
		if kind.owners != "" {
			if err := lockObjectOwners(ctx, tx, kind, ids); err != nil {
				return err
			}
		}
		standing, err := readStanding(ctx, tx, kind, ids)
		if err != nil {
			return fmt.Errorf("read which rows of the batch stand: %w", err)
		}
		marks, err := heldRowMarks(ctx, tx, kind.kind, ids)
		if err != nil {
			return err
		}
		held, err := heldNodes(ctx, tx, kind.kind, ids)
		if err != nil {
			return err
		}
		var write []graphstore.Node
		// taken holds, by id, the gone statement of each node a writer left standing for an
		// earlier row, which goes in before that node's live one, under the same cause.
		taken := map[string]graphstore.Node{}
		// partOf holds, by id, the part_of of each document an issue owns, stated with its node.
		partOf := map[string]graphstore.Tuple{}
		// createdAt holds, by id, each row's own created_at, which an issue's child_of claim is
		// dated at (issueParentClaims) whatever its node's statements are dated at.
		createdAt := make(map[string]time.Time, len(nodes))
		// retired holds the address of each retired document whose row names no node yet, which is
		// named with the written rows and given no node write.
		var retired []string
		// named counts the rows of write whose node the store already holds, as their row's own,
		// not gone and naming a project or parties, and whose row names no node: each is owed its
		// name alone, and counts as no row and no node the batch wrote.
		var named int64
		for _, row := range nodes {
			node := row.Node
			if identity, stands := standing[node.ID]; !stands || identity != row.identity {
				continue
			}
			stored, found := held[node.ID]
			// A document retired since the batch was read is owed no live node: its retirement
			// stated it gone, in the transaction that retired it (retiredDocument). Its row is still
			// owed its node's name where it names none, which is named below with no node written.
			if found && stored.retired {
				if marks[node.ID].owed {
					retired = append(retired, node.ID)
				}
				continue
			}
			if found && stored.present && stored.own && stored.scoped {
				if !marks[node.ID].owed {
					continue
				}
				if marks[node.ID].unnamed {
					named++
				}
			}
			node.Restates = found && stored.present && stored.own
			if found && !stored.newest.Before(node.MadeAt) {
				node.MadeAt = stored.newest.Add(time.Microsecond)
			}
			if found && stored.present && !stored.own {
				gone := node
				gone.State, gone.MadeBy = graphstore.StateGone, backfillActor
				taken[node.ID] = gone
			}
			if row.issue != "" {
				at := row.MadeAt
				if found && stored.newest.After(at) {
					at = node.MadeAt
				}
				partOf[node.ID] = graphstore.ArtifactPartOf(node.ID, row.issue, node.MadeBy, at)
			}
			createdAt[node.ID] = row.MadeAt
			write = append(write, node)
		}
		written.Rows, written.Nodes = int64(len(write))-named, int64(len(write))-named
		// heads is every node the batch's writes name, whose head rows it writes once, last.
		var heads []graphstore.NodeKey
		for start := 0; start < len(write); {
			end := start + 1
			for end < len(write) && write[end].SourceEvent == write[start].SourceEvent {
				end++
			}
			run := write[start:end]
			// A node a writer left standing for an earlier row is stated gone and then live by
			// the run's own tuples, both under its cause: one node write of the node in this
			// transaction leaves one mark on its row (graphstore's nodeMarks), so a second write of
			// it would lose the first's statement, and the node write then states no state of its
			// own beside them.
			var stated []graphstore.Tuple
			for _, node := range run {
				if statement, ok := taken[node.ID]; ok {
					stated = append(stated, stateTuple(statement), stateTuple(node))
				}
				if fact, ok := partOf[node.ID]; ok {
					stated = append(stated, fact)
				}
			}
			// PlanUncaused sets each node's SourceEvent to the sentinel where it stands, so the
			// run's one cause (the loop above grouped the nodes by it) is read before it.
			cause := run[0].SourceEvent
			made := graphstore.PlanUncaused(run, stated)
			if err := store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
				graphstore.QueueNodes(b, made)
				return nil
			}); err != nil {
				return err
			}
			var appended int64
			if err := store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
				graphstore.QueueCaused(b, graphstore.Counting(made, &appended), cause)
				return nil
			}); err != nil {
				return err
			}
			heads = append(heads, graphstore.HeldHeads(made)...)
			written.Tuples += appended
			start = end
		}
		if len(write) == 0 && len(retired) == 0 {
			return nil
		}
		writtenIDs := make([]string, len(write))
		for index, node := range write {
			writtenIDs[index] = node.ID
		}
		switch kind.kind {
		case graphstore.KindIssue:
			claims, parents := issueParentClaims(write, marks, createdAt)
			arms, params, err := graphstore.WriteArms(1, nil, nil, claims)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, "with "+arms+"\nselect 1", params...); err != nil {
				return fmt.Errorf("state the parents of a batch of issues: %w", err)
			}
			heads = append(heads, graphstore.HeadsOfClaims(claims)...)
			written.Tuples += parents
			if _, err := tx.Exec(ctx, `
				update issues set node_kind = $2, node_address = key
				where key = any($1::text[]) and node_kind is null
			`, writtenIDs, graphstore.KindIssue); err != nil {
				return fmt.Errorf("name the nodes of an issue batch on their rows: %w", err)
			}
		case graphstore.KindArtifact:
			values, err := nameArtifactNodes(ctx, tx, append(writtenIDs, retired...))
			if err != nil {
				return err
			}
			if len(values) > 0 {
				written.Tuples += int64(len(values))
				if err := store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
					graphstore.QueueWrite(b, nil, values)
					return nil
				}); err != nil {
					return err
				}
				heads = append(heads, graphstore.HeadsOf(nil, values)...)
			}
		}
		// The head rows of every node the batch named, the last rows it takes: after its node
		// rows, its tuples and the rows that name its nodes.
		return graphstore.WriteHeads(ctx, tx, heads)
	}); err != nil {
		return KindBackfill{}, fmt.Errorf("write a batch of %s nodes: %w", kind.kind, err)
	}
	return written, nil
}

// stateTuple is node's state as a tuple of its own, dated and written as the node is, for a write
// that states a node's state itself rather than through its node write (graphstore.PlanUncaused's
// stated tuples), which then states no state of that node beside it.
func stateTuple(node graphstore.Node) graphstore.Tuple {
	return graphstore.Tuple{
		FromKind: node.Kind, FromID: node.ID, Kind: graphstore.AttributeState,
		ToKind: graphstore.KindValue, ToID: node.State, At: node.MadeAt, Who: node.MadeBy, Asserted: true,
	}
}

// rowMark is what an issue's or a document's own row says of its marker, read under the batch's
// owner locks (heldRowMarks): whether it is still unmarked - an issue's by issueRowUnmarked, a
// document's by naming no node - whether it names no node, and the parent an issue's row names,
// nil for none and for a document.
type rowMark struct {
	owed, unnamed bool
	parent        *string
}

// heldRowMarks reads again, under the owner locks lockObjectOwners took, the row marker of each
// issue or document row of ids that stands (rowMark), and answers nil for a batch of asks or
// messages, whose rows name no node in a column of their own. A plain read is enough: every PATCH
// that moves an issue's parent takes the issue's row first (api's lockPatchRows, in this release
// and the one before), every writer that names a document's node on its row holds that document's
// owner lock, and another run of this pass names a node only under the owner locks this one holds,
// so what this reads stands until the batch commits.
func heldRowMarks(ctx context.Context, tx pgx.Tx, kind string, ids []string) (map[string]rowMark, error) {
	var read string
	switch kind {
	case graphstore.KindIssue:
		read = `select i.key, ` + issueRowUnmarked + `, i.node_kind is null, i.parent_key from issues i where i.key = any($1::text[])`
	case graphstore.KindArtifact:
		read = `select ref_key, node_kind is null, node_kind is null, null::text from artifacts where ref_key = any($1::text[])`
	default:
		return nil, nil
	}
	rows, err := tx.Query(ctx, read, ids)
	if err != nil {
		return nil, fmt.Errorf("read the batch's %s rows again under their owners: %w", kind, err)
	}
	marks := make(map[string]rowMark, len(ids))
	var id string
	var mark rowMark
	if _, err := pgx.ForEachRow(rows, []any{&id, &mark.owed, &mark.unnamed, &mark.parent}, func() error {
		held := mark
		if mark.parent != nil {
			// The scan writes every row into one target, so each row keeps a parent of its own.
			parent := *mark.parent
			held.parent = &parent
		}
		marks[id] = held
		return nil
	}); err != nil {
		return nil, fmt.Errorf("read the batch's %s rows again under their owners: %w", kind, err)
	}
	return marks, nil
}

// issueParentClaims is the child_of of each issue of nodes, the issue nodes the batch wrote, to
// the parent marks names for it, read under the issue's own row (heldRowMarks), or to none where
// it names none: a claim, which retracts every other parent the node holds and asserts this one
// where the node is not at it already, made as the node is, at the sentinel. It is dated at the
// row's own created_at (createdAt), the time graph_edges' arm over the rows that name no node dates
// the same edge at, and the time an issue's create dates its child_of, so the store answers the
// edge the backfill states at the moment the old arm answered it, whatever its node's statements
// are dated at. It answers how many of them name a parent, the tuples they can assert.
func issueParentClaims(nodes []graphstore.Node, marks map[string]rowMark, createdAt map[string]time.Time) ([]graphstore.Claim, int64) {
	claims := make([]graphstore.Claim, len(nodes))
	var parents int64
	for index, node := range nodes {
		parent := ""
		if named := marks[node.ID].parent; named != nil {
			parent = *named
			parents++
		}
		claims[index] = graphstore.Claim{
			FromKind: graphstore.KindIssue, FromID: node.ID, Kind: graphstore.TupleChildOf,
			ToKind: graphstore.KindIssue, ToID: parent,
			At: createdAt[node.ID], Who: node.MadeBy, SourceEvent: graphstore.NoEvent,
		}
	}
	return claims, parents
}

// nameArtifactNodes names each document's node on its row, then reads, in a statement of its own,
// the bytes of the newest version of each image and file it named, and answers them as the values
// its node states (graphstore.ArtifactBytes), dated at that version and by its first author, or by
// the row's author where the version names none. A version row missing its mime, its size or its
// sha256 states none of them. A newest version with no present node of its own, none or one held
// gone (presentVersionNode), is the version pass's to state, with that node (writeVersionBatch),
// so the bytes of one version are stated once whichever of the two passes reaches it; but a
// version whose address (its document's ref_key, `@v` and its number) is past the store's bound
// is no node of the store and no row the version pass reads, so its bytes are stated here, as its
// upload states them on its document's node (api's statedVersionBytes).
//
// Beside them it answers the mentions each document it named owes its node for the refs rows it
// holds (documentMentionMoves), as the backfill's pass over the references states them: graph_edges'
// arm over refs answers a document's row only while the document's row names no node (migration
// 20261119100000), so a document named without them would leave each of its references answered
// by neither arm until that pass ran.
//
// The read follows the update, under the owner locks the batch took first: an upload of a later
// version takes its issue's row, or its project document's own row, before it writes, and reads its
// document's row FOR SHARE, which the update's lock conflicts with, before it states that
// version's bytes on the node the row names (api's statedVersionBytes). So either the upload
// commits first and this read, a statement after it, finds its version, with the node the upload
// wrote for it, as the newest, or the upload waits for this transaction, sees the node named, and
// states its bytes after these. Every writer of a document's refs rows holds the same owner lock
// (writeReferenceBatch says which), so the references read after the update are the ones that
// stand as the batch commits.
func nameArtifactNodes(ctx context.Context, tx pgx.Tx, refKeys []string) ([]graphstore.Tuple, error) {
	rows, err := tx.Query(ctx, `
		update artifacts set node_kind = $2, node_address = ref_key
		where ref_key = any($1::text[]) and node_kind is null
		returning id::text, kind <> 'doc'
	`, refKeys, graphstore.KindArtifact)
	if err != nil {
		return nil, fmt.Errorf("name the nodes of a document batch on their rows: %w", err)
	}
	var named, files []string
	for rows.Next() {
		var id string
		var file bool
		if err := rows.Scan(&id, &file); err != nil {
			rows.Close()
			return nil, fmt.Errorf("name the nodes of a document batch on their rows: %w", err)
		}
		named = append(named, id)
		if file {
			files = append(files, id)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("name the nodes of a document batch on their rows: %w", err)
	}
	if len(named) == 0 {
		return nil, nil
	}
	stated, err := documentMentionMoves(ctx, tx, named, time.Now().UTC(), nil)
	if err != nil {
		return nil, fmt.Errorf("read the references of the documents a batch named: %w", err)
	}
	if len(files) == 0 {
		return stated, nil
	}
	rows, err = tx.Query(ctx, `
		select a.ref_key, v.mime, v.size, v.sha256, v.created_at, coalesce(v.authors->0, a.created_by)
		from artifacts a
		cross join lateral (
			select number, mime, size, sha256, created_at, authors from artifact_versions
			where artifact_id = a.id order by number desc limit 1
		) v
		where a.id = any($1::uuid[]) and v.mime is not null and v.size is not null and v.sha256 is not null
		  and (`+presentVersionNode+`
		       or octet_length(a.ref_key || '@v' || v.number) > `+addressBound+`)
	`, files)
	if err != nil {
		return nil, fmt.Errorf("read the newest bytes of a batch's images and files: %w", err)
	}
	defer rows.Close()
	// stated holds the named documents' mentions already; the bytes follow them.
	for rows.Next() {
		var refKey, mime, digest string
		var size int64
		var at time.Time
		var whoJSON []byte
		if err := rows.Scan(&refKey, &mime, &size, &digest, &at, &whoJSON); err != nil {
			return nil, fmt.Errorf("read the newest bytes of a batch's images and files: %w", err)
		}
		var who model.Actor
		if err := json.Unmarshal(whoJSON, &who); err != nil {
			return nil, fmt.Errorf("read the author of the newest version of %s: %w", refKey, err)
		}
		stated = graphstore.ArtifactBytes(stated, refKey, mime, size, digest, who, at)
	}
	return stated, rows.Err()
}

// storedNode is what the store holds of one node of a batch: whether it is present, at any state
// but gone, whether its live statement is its row's own, whether it names a project or parties,
// whether it is the node of a retired document, and when its newest statement is dated.
type storedNode struct {
	present, own, scoped, retired bool
	newest                        time.Time
}

// heldNodes answers, of the batch's ids, each node the store holds (storedNode). A node is its
// row's own when, for a document a removal can take, its live statement was made for that row
// (ownLiveStatement); any node of the other kinds, and of a document no removal takes, is its
// row's own, since no later row takes its address. A document's node is a retired document's
// where the row at its address is retired (retiredDocument): the retirement writes that node, gone,
// in the transaction that retires the row, under the owner lock the batch holds by now, so a
// retired row always has one here.
func heldNodes(ctx context.Context, tx pgx.Tx, kind string, ids []string) (map[string]storedNode, error) {
	rows, err := tx.Query(ctx, `
		select n.id, n.state <> '`+graphstore.StateGone+`',
		       coalesce((select not `+removableDocument("a")+` or `+ownLiveStatement("n", "a.id::text", "a.issue_key")+`
		                 from artifacts a where n.kind = '`+graphstore.KindArtifact+`' and a.ref_key = n.id), true),
		       `+scopedNode+`,
		       coalesce((select `+retiredDocument("a")+`
		                 from artifacts a where n.kind = '`+graphstore.KindArtifact+`' and a.ref_key = n.id), false),
		       (select max(t.at) from graph_tuples t where t.from_kind = n.kind and t.from_id = n.id)
		from graph_nodes n where n.kind = $1 and n.id = any($2::text[])
	`, kind, ids)
	if err != nil {
		return nil, fmt.Errorf("read the nodes the store holds of the batch's %s rows: %w", kind, err)
	}
	held := make(map[string]storedNode, len(ids))
	var id string
	var stored storedNode
	var newest *time.Time
	if _, err := pgx.ForEachRow(rows, []any{&id, &stored.present, &stored.own, &stored.scoped, &stored.retired, &newest}, func() error {
		stored.newest = time.Time{}
		if newest != nil {
			stored.newest = *newest
		}
		held[id] = stored
		return nil
	}); err != nil {
		return nil, fmt.Errorf("read the nodes the store holds of the batch's %s rows: %w", kind, err)
	}
	return held, nil
}

// partiesRefused reports whether graphstore.PartiesValue refuses parties, a party that is empty
// or holds white space: a node between them cannot be written, so every pass of the backfill
// skips its row, counted in Skipped, rather than fail the batch it falls in and every run after it.
func partiesRefused(parties []string) bool {
	_, err := graphstore.PartiesValue(parties)
	return err != nil
}

// lockObjectOwners takes the owner lock each row of ids stands under (objectKind.owners), each
// once and in owner key order (events.LockOwnerRows, store.LockOwners).
func lockObjectOwners(ctx context.Context, tx pgx.Tx, kind objectKind, ids []string) error {
	rows, err := tx.Query(ctx, kind.owners, ids)
	if err != nil {
		return fmt.Errorf("read the owners of the batch's %s rows: %w", kind.kind, err)
	}
	owners := events.OwnerRows{}
	var issue, document string
	if _, err := pgx.ForEachRow(rows, []any{&issue, &document}, func() error {
		owners.Add(issue, document)
		return nil
	}); err != nil {
		return fmt.Errorf("read the owners of the batch's %s rows: %w", kind.kind, err)
	}
	return events.LockOwnerRows(ctx, tx, owners)
}

// scopeKinds are the kinds whose writers stored nodes before a node named its project, each with
// the read of one batch of those nodes, after the id $1 and at most $2 of them, as (id, project,
// session): the comment nodes and the block nodes, whose project is their row's
// (scopedRows' rule).
var scopeKinds = []struct{ kind, read string }{
	{graphstore.KindComment, `
		select n.id, coalesce(i.project_key, owner.project_key),
		       case when coalesce(i.project_key, owner.project_key) is null then owner.session_id end
		from graph_nodes n
		join comments c on c.id = n.id::uuid
		left join issues i on i.key = c.issue_key
		left join artifacts owner on owner.id = c.artifact_id
		where n.kind = '` + graphstore.KindComment + `' and n.id > $1 and n.project_key is null and n.parties is null
		order by n.id limit $2`},
	{refs.BlockNodeKind, `
		select n.id, a.project_key, case when a.project_key is null then a.session_id end
		from graph_nodes n
		join artifacts a on a.ref_key = split_part(n.id, '#', 1)
		where n.kind = '` + refs.BlockNodeKind + `' and n.id > $1 and n.project_key is null and n.parties is null
		order by n.id limit $2`},
}

// backfillScopes gives every comment and block node the comment and block writers stored before a
// node named its project its project, or its conversation's party (graphstore.FillScopes), under
// the same contract: its marker is the node's own project_key and parties, null on exactly the
// nodes it has to fill, and the fill writes them in the transaction that states the fact. A block
// node whose document is gone has no row to read a project from, and is left as it is: no row of
// the census holds it, and no read answers it. A scope between a party graphstore.PartiesValue
// refuses is skipped and counted in Skipped, as the object pass skips one (writableRows).
func backfillScopes(ctx context.Context, database *store.Store) ([]KindBackfill, error) {
	var done []KindBackfill
	for _, kind := range scopeKinds {
		report := KindBackfill{Kind: kind.kind + "_project"}
		after := ""
		for {
			scopes, err := readScopes(ctx, database, kind.kind, kind.read, after, backfillBatch)
			if err != nil {
				return done, err
			}
			if len(scopes) == 0 {
				break
			}
			after = scopes[len(scopes)-1].ID
			fillable := make([]graphstore.Scope, 0, len(scopes))
			for _, scope := range scopes {
				if partiesRefused(scope.Parties) {
					report.Skipped++
					continue
				}
				fillable = append(fillable, scope)
			}
			if len(fillable) > 0 {
				filled, err := writeScopeBatch(ctx, database, kind.kind, fillable)
				if err != nil {
					return done, err
				}
				report.Rows += int64(len(fillable))
				report.Nodes += filled
				report.Tuples += filled
			}
			if len(scopes) < backfillBatch {
				break
			}
		}
		var remaining int64
		if err := database.Pool.QueryRow(ctx, `select count(*) from (`+kind.read+`) as remaining`, "", nil).Scan(&remaining); err != nil {
			return done, fmt.Errorf("count the %s nodes naming no project: %w", kind.kind, err)
		}
		report.Remaining = remaining
		report.Complete = report.Remaining == 0
		done = append(done, report)
	}
	return done, nil
}

// writeScopeBatch fills one batch's scopes in one transaction. A block node is written under the
// revision fence (refs.IndexBlocks), so the fill of a block's node takes the fence of each
// document it fills first, as every other writer of a block's node does (lockBlockDocuments); a
// comment's node is written under no fence, and its fill takes the node rows alone.
func writeScopeBatch(ctx context.Context, database *store.Store, kind string, scopes []graphstore.Scope) (int64, error) {
	var filled int64
	if err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
		if kind == refs.BlockNodeKind {
			documents, err := blockDocuments(ctx, tx, scopes)
			if err != nil {
				return err
			}
			if err := lockBlockDocuments(ctx, tx, documents); err != nil {
				return err
			}
		}
		var err error
		filled, err = graphstore.FillScopes(ctx, tx, scopes)
		if err != nil {
			return err
		}
		scoped := make([]graphstore.NodeKey, len(scopes))
		for index, scope := range scopes {
			scoped[index] = graphstore.NodeKey{Kind: scope.Kind, ID: scope.ID}
		}
		return graphstore.WriteHeads(ctx, tx, scoped)
	}); err != nil {
		return 0, fmt.Errorf("fill the project of a batch of %s nodes: %w", kind, err)
	}
	return filled, nil
}

func readScopes(ctx context.Context, database *store.Store, kind, read, after string, limit int) ([]graphstore.Scope, error) {
	rows, err := database.Pool.Query(ctx, read, after, limit)
	if err != nil {
		return nil, fmt.Errorf("read the %s nodes naming no project: %w", kind, err)
	}
	defer rows.Close()
	var scopes []graphstore.Scope
	for rows.Next() {
		var scope graphstore.Scope
		var project, session *string
		if err := rows.Scan(&scope.ID, &project, &session); err != nil {
			return nil, fmt.Errorf("read a %s node naming no project: %w", kind, err)
		}
		scope.Kind = kind
		scope.Project, scope.Parties = graphstore.SessionScope(derefText(project), derefText(session))
		scopes = append(scopes, scope)
	}
	return scopes, rows.Err()
}

// blockDocuments is the ids of the documents the block scopes belong to, read in tx off each
// block's address, which is its document's ref_key, '#' and its own id.
func blockDocuments(ctx context.Context, tx pgx.Tx, scopes []graphstore.Scope) ([]string, error) {
	refKeys := make([]string, len(scopes))
	for index, scope := range scopes {
		refKeys[index], _, _ = strings.Cut(scope.ID, "#")
	}
	rows, err := tx.Query(ctx, `select id::text from artifacts where ref_key = any($1)`, refKeys)
	if err != nil {
		return nil, fmt.Errorf("read the documents of a batch of block nodes: %w", err)
	}
	defer rows.Close()
	var documents []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("read the documents of a batch of block nodes: %w", err)
		}
		documents = append(documents, id)
	}
	return documents, rows.Err()
}
