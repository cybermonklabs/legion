package graphcensus

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The project of every node, which a read limited to a project depends on. Each object a project
// owns - an issue, a document, an ask, a message, a comment and a document's block - is a node of
// the store whose project is a base attribute its own write states (graphstore.Node), and an
// object no project owns - a conversation's file, an ask or a comment on one, a message on no
// issue - names the parties it is between instead. This file is what reads and repairs the two
// from the rows:
// scopedRows, the one statement of what each row's node must carry; the census's three classes
// over it (ProjectCensus); the read that answers every row not yet complete (ProjectComplete);
// and the backfill of the rows written before their kind's writer did this
// (backfillObjectNodes, backfillScopes).

// projectKinds are the kinds whose nodes carry a project or parties, in the order the census
// reports them.
var projectKinds = []string{
	graphstore.KindIssue, graphstore.KindArtifact, graphstore.KindAsk, graphstore.KindMessage,
	graphstore.KindComment, refs.BlockNodeKind,
}

// authorParty is, for an actor stored as jsonb at column, the party graphstore.AuthorOf names it
// as: a person by their canonical login, a session by its id, a personal token's owner by theirs,
// or null for an actor that names nobody. lower(btrim()) is model.CanonicalLogin.
func authorParty(column string) string {
	return `case
		when ` + column + `->>'kind' = 'user' and coalesce(` + column + `->>'id', '') <> ''
			then '` + graphstore.KindPerson + `:' || lower(btrim(` + column + `->>'id'))
		when ` + column + `->>'kind' = 'session' and coalesce(` + column + `->>'id', '') <> ''
			then '` + graphstore.KindSession + `:' || (` + column + `->>'id')
		when coalesce(` + column + `->>'owner', '') <> ''
			then '` + graphstore.KindPerson + `:' || lower(btrim(` + column + `->>'owner'))
	end`
}

// partiesOf is the parties array the given party expressions make, as graphstore.PartiesValue
// orders them - each once, in byte order (collate "C") - or null for none.
func partiesOf(parties ...string) string {
	return `(select array_agg(party order by party collate "C")
		from (select distinct party from unnest(array[` + strings.Join(parties, ", ") + `]::text[]) as listed(party)
		      where party is not null) as held)`
}

// sessionParties is the parties of a thing a conversation's file owns: that conversation's
// session (graphstore.ArtifactNode), or null where the file is not a conversation's.
func sessionParties(session string) string {
	return `case when ` + session + ` is not null then array['` + graphstore.KindSession + `:' || ` + session + `] end`
}

// scopedRows is every row of a project-owned kind whose node the store must hold, as (kind, id,
// project, parties): the address its node is written under, and the project or the parties that
// node must carry, by the rule its writer follows - graphstore.ScopeOf for an ask and a comment,
// graphstore.ThreadParties for a message on no issue, the document's own for a block. A row whose
// address is past the store's bound is left out, as every writer leaves it out of the store: a
// document whose ref_key is, and a comment one of whose relations names such a document.
const scopedRows = `
	with recursive threads (id, root) as (
		select m.id, m.id from messages m where m.issue_key is null and m.in_reply_to is null
		union all
		select m.id, thread.root from messages m join threads thread on m.in_reply_to = thread.id
		where m.issue_key is null
	),
	scoped (kind, id, project, parties) as (
		select 'issue', i.key, i.project_key, null::text[] from issues i
		union all
		select 'artifact', a.ref_key, a.project_key, ` + `ARTIFACT_PARTIES` + `
		from artifacts a where octet_length(a.ref_key) <= ADDRESS_BOUND
		union all
		select 'ask', k.id::text, coalesce(i.project_key, a.project_key),
		       case when coalesce(i.project_key, a.project_key) is null then ` + `ASK_PARTIES` + ` end
		from asks k left join issues i on i.key = k.issue_key left join artifacts a on a.id = k.artifact_id
		union all
		select 'message', m.id::text, i.project_key,
		       case when m.issue_key is null then ` + `MESSAGE_PARTIES` + ` end
		from messages m
		left join issues i on i.key = m.issue_key
		left join threads thread on thread.id = m.id
		left join messages root on root.id = thread.root
		union all
		select 'comment', c.id::text, coalesce(i.project_key, owner.project_key),
		       case when coalesce(i.project_key, owner.project_key) is null then ` + `COMMENT_PARTIES` + ` end
		from comments c
		left join issues i on i.key = c.issue_key
		left join artifacts anchor on anchor.id = (c.anchor->>'artifact_id')::uuid
		left join artifacts owner on owner.id = c.artifact_id
		where c.node_kind is not null
		   or ((anchor.ref_key is null or octet_length(anchor.ref_key) <= ADDRESS_BOUND)
		       and (owner.ref_key is null or octet_length(owner.ref_key) <= ADDRESS_BOUND))
		union all
		select 'block', d.node_id, a.project_key, ` + `BLOCK_PARTIES` + `
		from document_blocks d join artifacts a on a.id = d.artifact_id
		where octet_length(d.node_id) <= ADDRESS_BOUND
	)`

// scopedRowsSQL is scopedRows with its parts filled in.
var scopedRowsSQL = strings.NewReplacer(
	"ADDRESS_BOUND", strconv.Itoa(graphstore.MaxAddressBytes),
	"ARTIFACT_PARTIES", sessionParties("a.session_id"),
	"ASK_PARTIES", sessionParties("a.session_id"),
	"MESSAGE_PARTIES", partiesOf(authorParty("root.author"), "root.target"),
	"COMMENT_PARTIES", sessionParties("owner.session_id"),
	"BLOCK_PARTIES", sessionParties("a.session_id"),
).Replace(scopedRows)

// The classes the census and ProjectComplete name a row by. missing is a row whose node the store
// does not hold; unscoped a node holding neither a project nor parties; differs a node holding
// another project, or other parties, than its row's.
const (
	classMissing  = "missing"
	classUnscoped = "unscoped"
	classDiffers  = "differs"
)

// rowClass is the class of one row of scopedRows against its node n, null for a complete one.
const rowClass = `case
		when n.id is null then '` + classMissing + `'
		when n.project_key is null and n.parties is null then '` + classUnscoped + `'
		when n.project_key is distinct from scoped.project or n.parties is distinct from scoped.parties then '` + classDiffers + `'
	end`

// ProjectKindReport is what the project census read of one kind: how many rows it holds, and how
// many of them are in each class.
type ProjectKindReport struct {
	Kind                       string
	Rows                       int64
	Missing, Differs, Unscoped int64
}

// Findings is how many rows of the kind are incomplete, over the three classes.
func (r ProjectKindReport) Findings() int64 { return r.Missing + r.Differs + r.Unscoped }

// ProjectCensus reads, for every project-owned kind, how many rows it holds and how many of them
// have no node, a node with no project or parties, or a node in another project or between other
// parties than the row's, in one statement over q, which the store census runs in its snapshot.
func ProjectCensus(ctx context.Context, q graphstore.Queryer) ([]ProjectKindReport, error) {
	rows, err := q.Query(ctx, scopedRowsSQL+`,
		classed as (
			select scoped.kind, `+rowClass+` as class
			from scoped
			left join graph_nodes n on n.kind = scoped.kind and n.id = scoped.id
		)
		select kind, count(*),
		       count(*) filter (where class = '`+classMissing+`'),
		       count(*) filter (where class = '`+classDiffers+`'),
		       count(*) filter (where class = '`+classUnscoped+`')
		from classed group by kind
	`)
	if err != nil {
		return nil, fmt.Errorf("read the project census: %w", err)
	}
	defer rows.Close()
	read := map[string]ProjectKindReport{}
	for rows.Next() {
		var kind ProjectKindReport
		if err := rows.Scan(&kind.Kind, &kind.Rows, &kind.Missing, &kind.Differs, &kind.Unscoped); err != nil {
			return nil, fmt.Errorf("read the project census: %w", err)
		}
		read[kind.Kind] = kind
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the project census: %w", err)
	}
	reports := make([]ProjectKindReport, 0, len(projectKinds))
	for _, kind := range projectKinds {
		report := read[kind]
		report.Kind = kind
		reports = append(reports, report)
	}
	return reports, nil
}

// writeProjectCensus prints one line per kind, in the form the operator's runbook reads
// (cmd/dispatch/README.md, "The node and tuple store census").
func writeProjectCensus(w io.Writer, kinds []ProjectKindReport) {
	for _, kind := range kinds {
		fmt.Fprintf(w, "census: project kind=%s rows=%d missing=%d differs=%d unscoped=%d\n",
			kind.Kind, kind.Rows, kind.Missing, kind.Differs, kind.Unscoped)
	}
}

// ProjectComplete answers every project-owned row whose node is not complete, as
// `<kind> <id>: <class>` (missing, unscoped or differs), in one read: the rows a read limited by a
// node's project would answer wrongly. An empty answer is every row of every kind complete.
func ProjectComplete(ctx context.Context, q graphstore.Queryer) ([]string, error) {
	rows, err := q.Query(ctx, scopedRowsSQL+`
		select scoped.kind || ' ' || scoped.id || ': ' || `+rowClass+`
		from scoped
		left join graph_nodes n on n.kind = scoped.kind and n.id = scoped.id
		where `+rowClass+` is not null
		order by scoped.kind, scoped.id
	`)
	if err != nil {
		return nil, fmt.Errorf("read the rows whose node names no project: %w", err)
	}
	defer rows.Close()
	var incomplete []string
	for rows.Next() {
		var row string
		if err := rows.Scan(&row); err != nil {
			return nil, fmt.Errorf("read the rows whose node names no project: %w", err)
		}
		incomplete = append(incomplete, row)
	}
	return incomplete, rows.Err()
}

// objectKinds are the kinds the backfill writes a node for from their rows, each with the id it
// walks from and the read of one batch of the rows its marker does not mark, after the id $1 and at
// most $2 of them, in the order of the index that keys them, as (id, created_at, author, project,
// session, root author, root target, issue). session is a conversation's, for a document or an ask
// on one; the root columns are a message's on no issue alone: the author and the target of its
// thread's root, its parties (graphstore.ThreadParties); issue is the issue that owns a document,
// which its part_of names (graphstore.ArtifactPartOf). An issue's parent, which its child_of names
// (dispatch://INFRA-1253 R9), is read again under the issue's own row (heldObjects), since a
// reparent may commit between this read and that lock: an issue's marker is its node and its
// child_of together, since graph_edges answers its parent from the store once its row names its
// node.
//
// An issue's and a document's marker is the node pair its row names (node_kind and node_address,
// migration 20261116092210), so a row whose node an INFRA-3862 build wrote with no part_of, and
// named nowhere, is walked too. An issue's row also counts as unmarked while it names a parent its
// node does not hang under, which the release before's PATCH leaves (issueRowUnmarked). An ask's
// and a message's is the node itself, which no row of theirs names in a column of its own.
var objectKinds = []struct{ kind, start, read string }{
	{graphstore.KindIssue, "", `
		select i.key, i.created_at, i.created_by, i.project_key, null::text, null::jsonb, null::text, null::text
		from issues i
		where i.key > $1 and ` + issueRowUnmarked + `
		order by i.key limit $2`},
	{graphstore.KindArtifact, "", `
		select a.ref_key, a.created_at, a.created_by, a.project_key, a.session_id, null::jsonb, null::text, a.issue_key
		from artifacts a
		where a.ref_key > $1 and octet_length(a.ref_key) <= ` + strconv.Itoa(graphstore.MaxAddressBytes) + `
		  and a.node_kind is null
		order by a.ref_key limit $2`},
	{graphstore.KindAsk, zeroUUID, `
		select k.id::text, k.created_at, k.author, coalesce(i.project_key, a.project_key),
		       case when coalesce(i.project_key, a.project_key) is null then a.session_id end, null::jsonb, null::text,
		       null::text
		from asks k left join issues i on i.key = k.issue_key left join artifacts a on a.id = k.artifact_id
		where k.id > $1::uuid and not exists (select 1 from graph_nodes n where n.kind = 'ask' and n.id = k.id::text)
		order by k.id limit $2`},
	{graphstore.KindMessage, zeroUUID, `
		with recursive batch as (
			select m.id, m.issue_key, m.in_reply_to, m.created_at, m.author
			from messages m
			where m.id > $1::uuid and not exists (select 1 from graph_nodes n where n.kind = 'message' and n.id = m.id::text)
			order by m.id limit $2
		),
		walk (message, at, in_reply_to) as (
			select batch.id, batch.id, batch.in_reply_to from batch where batch.issue_key is null
			union all
			select walk.message, parent.id, parent.in_reply_to from walk join messages parent on parent.id = walk.in_reply_to
		)
		select batch.id::text, batch.created_at, batch.author, i.project_key, null::text, root.author, root.target, null::text
		from batch
		left join issues i on i.key = batch.issue_key
		left join walk on walk.message = batch.id and walk.in_reply_to is null
		left join messages root on root.id = walk.at
		order by batch.id`},
}

// issueRowUnmarked is the test that the issues row aliased i is one its marker does not mark: it
// names no node, or it names a parent its node's child_of does not name, which only the release
// before's issue PATCH leaves, setting parent_key in place on a marked row (store.IssueParentMoved,
// R9 of dispatch://INFRA-1253). graph_edges' old child_of arm answers such a row, and the object
// pass states the child_of that names its parent, after which the store answers it.
const issueRowUnmarked = `(i.node_kind is null or (i.parent_key is not null and ` + store.IssueParentMoved + `))`

// zeroUUID is where a walk over a uuid key starts: every id sorts after it.
const zeroUUID = "00000000-0000-0000-0000-000000000000"

// backfillObjectNodes writes the node of every issue, document, ask and message written before
// its kind's writer wrote one, under the backfill's contract: it walks only the rows its
// marker does not mark (objectKinds), dates each node at its row's own created_at, made by the
// row's own author, and writes the marker in the transaction that writes the node, so a second run
// reads no row the first one wrote. Beside a document's node it states what its kind's writer
// states (R16 of dispatch://INFRA-1253): its part_of the issue that owns it, at the node's time,
// and an image's or a file's newest bytes (writeObjectBatch); beside an issue's, its child_of to
// the parent its row names under the issue's own lock, as a claim (R9), so graph_edges answers
// that parent from the store once the row names its node, and a parent the release before moved
// in place on a marked row replaces the one the node held. It appends no event, and every fact it
// states carries the sentinel.
func backfillObjectNodes(ctx context.Context, database *store.Store) ([]KindBackfill, error) {
	var done []KindBackfill
	for _, kind := range objectKinds {
		report := KindBackfill{Kind: kind.kind}
		after := kind.start
		for {
			nodes, tuples, err := readObjectNodes(ctx, database, kind.kind, kind.read, after, backfillBatch)
			if err != nil {
				return done, err
			}
			if len(nodes) == 0 {
				break
			}
			after = nodes[len(nodes)-1].ID
			written, stated, err := writeObjectBatch(ctx, database, kind.kind, nodes, tuples)
			if err != nil {
				return done, err
			}
			report.Rows += written
			report.Nodes += written
			report.Tuples += stated
			if len(nodes) < backfillBatch {
				break
			}
		}
		left, _, err := readObjectNodes(ctx, database, kind.kind, kind.read, kind.start, math.MaxInt32)
		if err != nil {
			return done, err
		}
		report.Remaining = int64(len(left))
		report.Complete = report.Remaining == 0
		done = append(done, report)
	}
	return done, nil
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

// readObjectNodes reads one batch of a kind's rows its marker does not mark, and answers the node
// its kind's writer writes for each (graphstore.IssueNode and its siblings), with the part_of to
// its issue of each document an issue owns (graphstore.ArtifactPartOf), dated as its node. An
// issue's child_of is writeObjectBatch's, from the parent its row names under the issue's lock.
func readObjectNodes(ctx context.Context, database *store.Store, kind, read, after string, limit int) ([]graphstore.Node, []graphstore.Tuple, error) {
	rows, err := database.Pool.Query(ctx, read, after, limit)
	if err != nil {
		return nil, nil, fmt.Errorf("read the %s rows naming no node: %w", kind, err)
	}
	defer rows.Close()
	var nodes []graphstore.Node
	var tuples []graphstore.Tuple
	for rows.Next() {
		var id string
		var createdAt time.Time
		var authorJSON, rootAuthorJSON []byte
		var project, session, rootTarget, issue *string
		if err := rows.Scan(&id, &createdAt, &authorJSON, &project, &session, &rootAuthorJSON, &rootTarget, &issue); err != nil {
			return nil, nil, fmt.Errorf("read a %s row naming no node: %w", kind, err)
		}
		var author model.Actor
		if err := json.Unmarshal(authorJSON, &author); err != nil {
			return nil, nil, fmt.Errorf("read the author of %s %s: %w", kind, id, err)
		}
		var parties []string
		if session != nil {
			parties = []string{graphstore.PartySession(*session)}
		}
		switch kind {
		case graphstore.KindIssue:
			nodes = append(nodes, graphstore.IssueNode(id, derefText(project), author, createdAt))
		case graphstore.KindArtifact:
			nodes = append(nodes, graphstore.ArtifactNode(id, derefText(project), derefText(session), author, createdAt))
			if issue != nil {
				tuples = append(tuples, graphstore.ArtifactPartOf(id, *issue, author, createdAt))
			}
		case graphstore.KindAsk:
			nodes = append(nodes, graphstore.AskNode(id, derefText(project), parties, author, createdAt))
		case graphstore.KindMessage:
			if project == nil {
				var root model.Actor
				if len(rootAuthorJSON) > 0 {
					if err := json.Unmarshal(rootAuthorJSON, &root); err != nil {
						return nil, nil, fmt.Errorf("read the thread root's author of message %s: %w", id, err)
					}
				}
				parties = graphstore.ThreadParties(root, rootTarget)
			}
			nodes = append(nodes, graphstore.MessageNode(id, derefText(project), parties, author, createdAt))
		}
	}
	return nodes, tuples, rows.Err()
}

func derefText(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// writeObjectBatch writes one batch's nodes and the facts beside them in one transaction, in the
// store's lock order (api/inbox_read.go's markInboxReadThreads): the owner rows the batch's rows
// stand under first (lockObjectOwners), then graphstore.Write, which takes the node rows in
// ascending (kind, id), states each one's base attributes and appends the facts, and only then the
// rows that name their nodes. A node a live writer wrote meanwhile is written again with nothing
// moved, and states nothing: each node restates its object (graphstore.IssueNode and its
// siblings), so a state or a visibility a later write gave it stays. One in another project fails
// the batch (graphstore.ErrProjectMoved), which is a difference for the census to report rather
// than for this run to paper over. An ask's and a message's row carries no column naming its node,
// so their write takes no lock of a row beside the nodes'.
//
// The batch was read outside this transaction, so an issue's or a document's row may have been
// removed or named its node since (api's removeArtifact, a second run of this pass), and an issue
// may have been moved under another parent (a PATCH, of this release or the one before). Under the
// owner rows it reads the batch's rows again (heldObjects) and writes the nodes and the facts of
// the rows still there and still unmarked, and no others, as writeBlockBatch does under the
// revision fence: a node written for a removed row would bring the document back as a live node
// hanging under its issue, with no row. An issue's child_of is the parent that read answers, stated
// as a claim (issueParentClaims), so a node holds one parent, the one its row names: a parent a
// PATCH claimed meanwhile, or a stale one the release before moved in place, is retracted where
// the row names another. It answers how many rows it gave a node and how many tuples it stated:
// the base attributes of each node it made, its part_of or its child_of, and the bytes of each
// image and file.
func writeObjectBatch(ctx context.Context, database *store.Store, kind string, nodes []graphstore.Node, tuples []graphstore.Tuple) (int64, int64, error) {
	ids := make([]string, len(nodes))
	for index, node := range nodes {
		ids[index] = node.ID
	}
	var written, stated int64
	if err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
		if err := lockObjectOwners(ctx, tx, kind, ids); err != nil {
			return err
		}
		held, err := heldObjects(ctx, tx, kind, ids)
		if err != nil {
			return err
		}
		nodes, tuples := nodes, tuples
		if held != nil {
			nodes = slices.DeleteFunc(slices.Clone(nodes), func(node graphstore.Node) bool {
				_, still := held[node.ID]
				return !still
			})
			tuples = slices.DeleteFunc(slices.Clone(tuples), func(tuple graphstore.Tuple) bool {
				_, still := held[tuple.FromID]
				return !still
			})
			ids = slices.Collect(maps.Keys(held))
		}
		if len(nodes) == 0 {
			return nil
		}
		if err := graphstore.Write(ctx, tx, nodes, tuples); err != nil {
			return err
		}
		written, stated = int64(len(nodes)), statedOf(nodes)+int64(len(tuples))
		switch kind {
		case graphstore.KindIssue:
			claims, parents := issueParentClaims(nodes, held)
			arms, params, err := graphstore.WriteArms(1, nil, nil, claims)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, "with "+arms+"\nselect 1", params...); err != nil {
				return fmt.Errorf("state the parents of a batch of issues: %w", err)
			}
			stated += parents
			_, err = tx.Exec(ctx, `
				update issues set node_kind = $2, node_address = key
				where key = any($1::text[]) and node_kind is null
			`, ids, graphstore.KindIssue)
			return err
		case graphstore.KindArtifact:
			values, err := nameArtifactNodes(ctx, tx, ids)
			if err != nil || len(values) == 0 {
				return err
			}
			stated += int64(len(values))
			return graphstore.Write(ctx, tx, nil, values)
		}
		return nil
	}); err != nil {
		return 0, 0, fmt.Errorf("write a batch of %s nodes: %w", kind, err)
	}
	return written, stated, nil
}

// issueParentClaims is the child_of of each issue of nodes, the batch's nodes still unmarked under
// the issue's own row, to the parent held names for it, read under that row (heldObjects), or to
// none where it names none: a claim, which retracts every other parent the node holds and asserts
// this one where the node is not at it already, dated and made as the node is, at the sentinel. It
// answers how many of them name a parent, the tuples they can assert.
func issueParentClaims(nodes []graphstore.Node, held map[string]*string) ([]graphstore.Claim, int64) {
	claims := make([]graphstore.Claim, len(nodes))
	var parents int64
	for index, node := range nodes {
		parent := ""
		if named := held[node.ID]; named != nil {
			parent = *named
			parents++
		}
		claims[index] = graphstore.Claim{
			FromKind: graphstore.KindIssue, FromID: node.ID, Kind: graphstore.TupleChildOf,
			ToKind: graphstore.KindIssue, ToID: parent,
			At: node.MadeAt, Who: node.MadeBy, SourceEvent: graphstore.NoEvent,
		}
	}
	return claims, parents
}

// lockObjectOwners takes the owner rows a batch of issues or documents stands under before the
// batch writes a node: an issue's own row, and a document's issue's row, or the document's own row
// where no issue owns it, each once, in ascending owner key (events.LockOwnerRows), as
// lockBlockDocuments takes the owners of a block batch's documents. A batch of asks or messages
// names no node on a row of its own, and takes none.
func lockObjectOwners(ctx context.Context, tx pgx.Tx, kind string, ids []string) error {
	owners := events.OwnerRows{}
	switch kind {
	case graphstore.KindIssue:
		for _, key := range ids {
			owners.Add(key, "", "issue "+key)
		}
	case graphstore.KindArtifact:
		rows, err := tx.Query(ctx, `
			select id::text, coalesce(issue_key, '') from artifacts where ref_key = any($1::text[])
		`, ids)
		if err != nil {
			return fmt.Errorf("load the owners of a batch's documents: %w", err)
		}
		documents, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) ([2]string, error) {
			var document [2]string
			err := row.Scan(&document[0], &document[1])
			return document, err
		})
		if err != nil {
			return fmt.Errorf("load the owners of a batch's documents: %w", err)
		}
		for _, document := range documents {
			owners.Add(document[1], document[0], "document "+document[0])
		}
	default:
		return nil
	}
	return events.LockOwnerRows(ctx, tx, owners)
}

// heldObjects reads again, under the owner rows lockObjectOwners took, which rows of a batch of
// issues or documents are still there and still unmarked (an issue's by issueRowUnmarked), and
// answers their ids, each with the parent an issue's row names now, nil for a document and for an
// issue with none; it answers nil for a batch of asks or messages, whose rows name no node in a
// column of their own and are written whole. A plain read is enough: the one writer that deletes
// such a row, the removal of an issue's document, takes that issue's row first (api's
// removeArtifact, through beginArtifactWrite), every PATCH that moves an issue's parent takes the
// issue's row first (api's lockPatchRows, in this release and the one before), another run of this
// pass names a node only under the owner rows this one holds, and no route moves a document to
// another owner or changes its ref_key, so what this reads stands until the batch commits.
func heldObjects(ctx context.Context, tx pgx.Tx, kind string, ids []string) (map[string]*string, error) {
	var read string
	switch kind {
	case graphstore.KindIssue:
		read = `select i.key, i.parent_key from issues i where i.key = any($1::text[]) and ` + issueRowUnmarked
	case graphstore.KindArtifact:
		read = `select ref_key, null::text from artifacts where ref_key = any($1::text[]) and node_kind is null`
	default:
		return nil, nil
	}
	rows, err := tx.Query(ctx, read, ids)
	if err != nil {
		return nil, fmt.Errorf("read the batch's %s rows again under their owners: %w", kind, err)
	}
	type stillRow struct {
		id     string
		parent *string
	}
	still, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (stillRow, error) {
		var held stillRow
		err := row.Scan(&held.id, &held.parent)
		return held, err
	})
	if err != nil {
		return nil, fmt.Errorf("read the batch's %s rows again under their owners: %w", kind, err)
	}
	held := make(map[string]*string, len(still))
	for _, row := range still {
		held[row.id] = row.parent
	}
	return held, nil
}

// nameArtifactNodes names each document's node on its row, then reads, in a statement of its own,
// the bytes of the newest version of each image and file it named, and answers them as the values
// its node states (graphstore.ArtifactBytes), dated at that version and by its first author, or by
// the row's author where the version names none. A version row missing its mime, its size or its
// sha256 states none of them. A newest version with no present node of its own, none or one held
// gone (presentVersionNode), is the version pass's to state, with that node (writeVersionBatch),
// so the bytes of one version are stated once whichever of the two passes reaches it; but a
// version whose address (its document's
// ref_key, `@v` and its number) is past the store's bound is no node of the store and no row the
// version pass reads, so its bytes are stated here, as its upload states them on its document's
// node (api's statedVersionBytes).
//
// The read follows the update, under the owner rows the batch took first: an upload of a later
// version takes its issue's row, or its project document's own row, before it writes, and reads its
// document's row FOR SHARE, which the update's lock conflicts with, before it states that
// version's bytes on the node the row names (api's statedVersionBytes). So either the upload
// commits first and this read, a statement after it, finds its version, with the node the upload
// wrote for it, as the newest, or the upload waits for this transaction, sees the node named, and
// states its bytes after these.
func nameArtifactNodes(ctx context.Context, tx pgx.Tx, refKeys []string) ([]graphstore.Tuple, error) {
	rows, err := tx.Query(ctx, `
		update artifacts set node_kind = $2, node_address = ref_key
		where ref_key = any($1::text[]) and node_kind is null
		returning id::text, kind <> 'doc'
	`, refKeys, graphstore.KindArtifact)
	if err != nil {
		return nil, fmt.Errorf("name the nodes of a document batch on their rows: %w", err)
	}
	var files []string
	for rows.Next() {
		var id string
		var file bool
		if err := rows.Scan(&id, &file); err != nil {
			rows.Close()
			return nil, fmt.Errorf("name the nodes of a document batch on their rows: %w", err)
		}
		if file {
			files = append(files, id)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("name the nodes of a document batch on their rows: %w", err)
	}
	if len(files) == 0 {
		return nil, nil
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
	var stated []graphstore.Tuple
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
		where n.kind = 'comment' and n.id > $1 and n.project_key is null and n.parties is null
		order by n.id limit $2`},
	{refs.BlockNodeKind, `
		select n.id, a.project_key, case when a.project_key is null then a.session_id end
		from graph_nodes n
		join artifacts a on a.ref_key = split_part(n.id, '#', 1)
		where n.kind = 'block' and n.id > $1 and n.project_key is null and n.parties is null
		order by n.id limit $2`},
}

// backfillScopes gives every comment and block node the comment and block writers stored before a
// node named its project its project, or its conversation's party (graphstore.FillScopes), under
// the same contract: its marker is the node's own project_key and parties, null on exactly the
// nodes it has to fill, and the fill writes them in the transaction that states the fact. A block
// node whose document is gone has no row to read a project from, and is left as it is: no row of
// the census holds it, and no read answers it.
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
			filled, err := writeScopeBatch(ctx, database, kind.kind, scopes)
			if err != nil {
				return done, err
			}
			report.Rows += int64(len(scopes))
			report.Nodes += filled
			report.Tuples += filled
			if len(scopes) < backfillBatch {
				break
			}
		}
		left, err := readScopes(ctx, database, kind.kind, kind.read, "", math.MaxInt32)
		if err != nil {
			return done, err
		}
		report.Remaining = int64(len(left))
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
		return err
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
		scope.Kind, scope.Project = kind, derefText(project)
		if session != nil {
			scope.Parties = []string{graphstore.PartySession(*session)}
		}
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
