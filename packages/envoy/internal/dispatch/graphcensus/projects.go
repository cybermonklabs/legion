package graphcensus

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// The project of every node, which a read limited to a project depends on. Each object a project
// owns - an issue, a document, an ask, a message, a comment and a document's block - is a node of
// the store whose project is a base attribute its own write states (graphstore.Node), and an
// object no project owns - a conversation's file, an ask or a comment on one, a message on no
// issue - names the parties it is between instead. This file reads the two against the rows:
// scopedRows, the one statement of what each row's node must carry; the census's classes over it
// (ProjectCensus); and the read that answers every row not yet complete (ProjectComplete).
// projects_backfill.go writes them from the rows written before their kind's writer did.

// projectKinds are the kinds whose nodes carry a project or parties, in the order the census
// reports them.
var projectKinds = []string{
	graphstore.KindIssue, graphstore.KindArtifact, graphstore.KindAsk, graphstore.KindMessage,
	graphstore.KindComment, refs.BlockNodeKind,
}

// authorParty is, for an actor stored as jsonb at column, the party graphstore.AuthorOf names it
// as: a person by their canonical login, a session by its id, a personal token's owner by theirs,
// or null for an actor that names nobody. The login is canonical by model.CanonicalLogin's rule,
// spelled in SQL (CanonicalLoginSQL).
func authorParty(column string) string {
	return `case
		when ` + column + `->>'kind' = 'user' and coalesce(` + column + `->>'id', '') <> ''
			then '` + graphstore.KindPerson + `:' || ` + CanonicalLoginSQL(column+`->>'id'`) + `
		when ` + column + `->>'kind' = 'session' and coalesce(` + column + `->>'id', '') <> ''
			then '` + graphstore.KindSession + `:' || (` + column + `->>'id')
		when coalesce(` + column + `->>'owner', '') <> ''
			then '` + graphstore.KindPerson + `:' || ` + CanonicalLoginSQL(column+`->>'owner'`) + `
	end`
}

// CanonicalLoginSQL is model.CanonicalLogin spelled in SQL over the text expression login: every
// Unicode white space trimmed from both ends, as strings.TrimSpace trims it, and the ASCII letters
// lowered and nothing else, as fold.ASCII lowers them. Postgres's lower() would fold a non-ASCII
// letter too (U+212A KELVIN SIGN to "k"), and a btrim() with no set trims the space alone, so
// either would name a person other than the one the writer names.
func CanonicalLoginSQL(login string) string {
	return `translate(btrim(` + login + `, '` + whiteSpaceSQL + `'), 'ABCDEFGHIJKLMNOPQRSTUVWXYZ', 'abcdefghijklmnopqrstuvwxyz')`
}

// whiteSpaceSQL is every character unicode.IsSpace reports, the set strings.TrimSpace trims, as
// the body of a SQL string literal. None of them is a quote.
var whiteSpaceSQL = func() string {
	var set strings.Builder
	for _, span := range unicode.White_Space.R16 {
		for r := rune(span.Lo); r <= rune(span.Hi); r += rune(span.Stride) {
			set.WriteRune(r)
		}
	}
	for _, span := range unicode.White_Space.R32 {
		for r := rune(span.Lo); r <= rune(span.Hi); r += rune(span.Stride) {
			set.WriteRune(r)
		}
	}
	return set.String()
}()

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
// project, parties, row_id, owner_key, removable, past_bound): the address its node is written
// under, the project or the parties that node must carry, by the rule its writer follows -
// graphstore.ScopeOf for an ask and a comment, graphstore.ThreadParties for a message on no issue,
// the document's own for a block - and, for a document, its row's id, its issue and whether a
// removal can take the row away, which a later row's create then takes the address of again
// (removableDocument, ownLiveStatement). past_bound marks a row whose address is past the store's
// bound, which every writer leaves out of the store - a document whose ref_key is, a block whose
// address is, and a comment one of whose relations names such a document - and which the census
// counts as a class of its own rather than as a row without a node. Each kind is named by its
// constant, filled in by scopedRowsSQL, so a renamed kind cannot leave its rows uncounted.
const scopedRows = `
	with recursive threads (id, root) as (
		select m.id, m.id from messages m where m.issue_key is null and m.in_reply_to is null
		union all
		select m.id, thread.root from messages m join threads thread on m.in_reply_to = thread.id
		where m.issue_key is null
	),
	scoped (kind, id, project, parties, row_id, owner_key, removable, past_bound) as (
		select 'KIND_ISSUE', i.key, i.project_key, null::text[], null::text, null::text, false, false from issues i
		union all
		select 'KIND_ARTIFACT', a.ref_key, a.project_key, ARTIFACT_PARTIES, a.id::text, a.issue_key, REMOVABLE_DOCUMENT,
		       octet_length(a.ref_key) > ADDRESS_BOUND
		from artifacts a
		union all
		select 'KIND_ASK', k.id::text, coalesce(i.project_key, a.project_key),
		       case when coalesce(i.project_key, a.project_key) is null then ASK_PARTIES end, null::text, null::text, false, false
		from asks k left join issues i on i.key = k.issue_key left join artifacts a on a.id = k.artifact_id
		union all
		select 'KIND_MESSAGE', m.id::text, i.project_key,
		       case when m.issue_key is null then MESSAGE_PARTIES end, null::text, null::text, false, false
		from messages m
		left join issues i on i.key = m.issue_key
		left join threads thread on thread.id = m.id
		left join messages root on root.id = thread.root
		union all
		select 'KIND_COMMENT', c.id::text, coalesce(i.project_key, owner.project_key),
		       case when coalesce(i.project_key, owner.project_key) is null then COMMENT_PARTIES end, null::text, null::text, false,
		       c.node_kind is null and (coalesce(octet_length(anchor.ref_key) > ADDRESS_BOUND, false)
		                                or coalesce(octet_length(owner.ref_key) > ADDRESS_BOUND, false))
		from comments c
		left join issues i on i.key = c.issue_key
		left join artifacts anchor on anchor.id = (c.anchor->>'artifact_id')::uuid
		left join artifacts owner on owner.id = c.artifact_id
		union all
		select 'KIND_BLOCK', d.node_id, a.project_key, BLOCK_PARTIES, null::text, null::text, false,
		       octet_length(d.node_id) > ADDRESS_BOUND
		from document_blocks d join artifacts a on a.id = d.artifact_id
	)`

// scopedRowsSQL is scopedRows with its parts filled in.
var scopedRowsSQL = strings.NewReplacer(
	"KIND_ISSUE", graphstore.KindIssue,
	"KIND_ARTIFACT", graphstore.KindArtifact,
	"KIND_ASK", graphstore.KindAsk,
	"KIND_MESSAGE", graphstore.KindMessage,
	"KIND_COMMENT", graphstore.KindComment,
	"KIND_BLOCK", refs.BlockNodeKind,
	"ADDRESS_BOUND", strconv.Itoa(graphstore.MaxAddressBytes),
	"ARTIFACT_PARTIES", sessionParties("a.session_id"),
	"ASK_PARTIES", sessionParties("a.session_id"),
	"MESSAGE_PARTIES", partiesOf(authorParty("root.author"), "root.target"),
	"COMMENT_PARTIES", sessionParties("owner.session_id"),
	"BLOCK_PARTIES", sessionParties("a.session_id"),
	"REMOVABLE_DOCUMENT", removableDocument("a"),
).Replace(scopedRows)

// removableDocument is, for the artifacts row a, whether a removal can take it away: a document
// of an issue that is not its spec, plays no role and is a document (api's artifactRowIsUntouched
// refuses every other row, and every release that serves the removal refuses the same ones). Only
// such a row gives up an address a later row takes again, so only its node can stand for a row
// before it, and only its node is read by its row's own create event (ownLiveStatement). Every
// other document - an issue's spec, a plan or a verification document, an image, a file, a
// project's document, a conversation's file - keeps its address for as long as the store holds it,
// so a node at that address that is not gone can stand for no other row, and that is enough. Not all of
// them record an artifact.created of their own either (issue.created records the spec, and a
// conversation's upload appends no event), so reading them by that event would name them gone
// for ever. A document a removal is made to take must be added here, in the same change: api's
// TestEveryDocumentARemovalCanTakeIsReadByItsCreateEvent writes every shape of row the store
// holds and fails on a shape the removal takes that this reads by the node's state alone, or the reverse.
func removableDocument(a string) string {
	return `(` + a + `.issue_key is not null and not ` + a + `.is_primary and ` + a + `.role is null and ` + a + `.kind = 'doc')`
}

// The classes the census and ProjectComplete name a row or a node by. missing is a row whose node
// the store does not hold; gone a row of a kind whose rows only its own creates write
// (objectKinds) whose node is gone, or, for a document a removal can take away, whose node's
// live statement is another row's (ownLiveStatement); unscoped a node holding neither a project
// nor parties; differs a node holding another project, or other parties, than its row's; orphaned
// a node of one of those kinds, at any state but gone, that no row stands behind.
const (
	classMissing  = "missing"
	classGone     = "gone"
	classUnscoped = "unscoped"
	classDiffers  = "differs"
	classOrphaned = "orphaned"
)

// orphanKindList is the kinds the census reads the state and the node side of too, as a SQL list:
// the kinds the backfill writes a node for from their rows (objectKinds) - an issue, a document,
// an ask and a message - whose rows only their own creates write and only a document's removal
// deletes. A node of one of them stands while its row does, and the removal of this build
// states it gone as it deletes the row; a build before this one writes no node, so during a roll
// or after a rollback it can create a row at an address whose node this build stated gone (gone),
// or remove a row whose node this build wrote (orphaned). The store census reads both directions
// for a comment and a block already (projections), so the project census reads them for these
// alone. A kind added to objectKinds is read every way from that one list.
var orphanKindList = func() string {
	quoted := make([]string, len(objectKinds))
	for index, kind := range objectKinds {
		quoted[index] = "'" + kind.kind + "'"
	}
	return strings.Join(quoted, ", ")
}()

// ownLiveStatement is, for the node of a document row a removal can take away, whether the newest
// state statement a create, a removal or the backfill made about the node, in the order the store
// appends them, was made for that row: the document's address (its issue's key and its slug) is
// one a removed row gives up and a later row takes again, so the node alone cannot say which row it
// stands for, and that statement says it.
//
// A statement made for the row is caused by the row's own create event, the artifact.created
// whose payload names the row's id: this build's create states the node live under it (api's
// storeArtifact, which states a node an earlier row left standing gone first), and the backfill
// finds it and states the row's node under it too (readObjectNodes). Every release that creates
// such a row appends that event, and an event names one row, so a statement caused by it names that
// row whatever its time: an earlier row's statement, caused by that row's own event, names the
// earlier row, and a removal's, caused by its artifact.removed, names the row it removed. A row
// with no create event of its own - one made before Dispatch recorded artifact.created - is made
// for by a statement at the sentinel, as the backfill writes it then. A later row always has its
// own event, so a sentinel statement never stands for it. Nothing in this rests on time.
//
// Every other state statement is passed over: one at the sentinel about a row that has its own
// create event, and one caused by an event that names no document. That is what a later release
// writes when it marks a standing document's node with a state of its own, such as a requirement's
// proposed (graphstore.Node's Restates), and the mark says nothing of which row the node stands
// for: read as the newest statement, it would make a row that stands, with its own node, read as
// gone, and the backfill would state that node gone and live again over the mark.
//
// Time keeps the order the store appends in, which is what a read as of a time reads: this
// build's create and removal read their one time under the address's owner lock, from the
// database's clock and past every statement the store holds about the address
// (graphstore.StatementTime), and the backfill dates a statement past every one before it too.
//
// ownerKey is the row's issue: a document a removal can take is an issue's, and its create event
// is its issue's, read through the issue's own events by the index that keys them (the events
// table's unique (issue_key, seq)).
func ownLiveStatement(node, rowID, ownerKey string) string {
	noCreateEvent := `not exists (
				select 1 from events o
				where o.issue_key = ` + ownerKey + `
				  and o.type = 'artifact.created' and o.payload->'artifact'->>'id' = ` + rowID + `)`
	return `coalesce((
		select coalesce(e.payload->'artifact'->>'id' = ` + rowID + `, true)
		from graph_tuples t
		left join events e on e.id = t.source_event and e.payload->'artifact'->>'id' is not null
		where t.from_kind = ` + node + `.kind and t.from_id = ` + node + `.id and t.kind = '` + graphstore.AttributeState + `'
		  and t.to_kind = '` + graphstore.KindValue + `'
		  and (e.id is not null
		       or (t.source_event = ` + strconv.FormatInt(graphstore.NoEvent, 10) + ` and ` + noCreateEvent + `))
		order by t.seq desc limit 1), false)`
}

// rowClass is the class of one row of scopedRows against its node n, null for a complete one and
// for a row whose address is past the store's bound, which no writer projects and the census
// counts as a class of its own (ProjectKindReport.PastBound). A node at any state but gone is one
// the row projects: a state a later release adds, such as a requirement's proposed, reads here as
// live does, as the store census reads it (presentNodes). A node that names another project than
// its row's is differs at any state, gone included, and read before gone: a node's project never
// moves (graphstore.ErrProjectMoved), so the backfill, which a gone line names as its cure, cannot
// write it, and only a person can tell which of the two is wrong.
var rowClass = `case
		when scoped.past_bound then null
		when n.id is null then '` + classMissing + `'
		when n.project_key is not null and n.project_key is distinct from scoped.project then '` + classDiffers + `'
		when scoped.kind in (` + orphanKindList + `) and n.state = '` + graphstore.StateGone + `' then '` + classGone + `'
		when scoped.removable and not ` + ownLiveStatement("n", "scoped.row_id", "scoped.owner_key") + ` then '` + classGone + `'
		when n.project_key is null and n.parties is null then '` + classUnscoped + `'
		when n.project_key is distinct from scoped.project or n.parties is distinct from scoped.parties then '` + classDiffers + `'
	end`

// classedRows follows scopedRowsSQL: every row of scoped with its class against its node, null for
// a complete one, and every node of an object kind at a state but gone that no row of scoped stands
// behind, as orphaned, each with the project of its row (row_project, null for an orphaned node,
// which stands behind none), whether it is a row the store projects, whether its address is past
// the store's bound, and whether the row names a party the store refuses (refusedPartySQL).
var classedRows = `,
	classed (kind, id, row_project, class, is_row, past_bound, refused) as (
		select scoped.kind, scoped.id, scoped.project, ` + rowClass + `, not scoped.past_bound, scoped.past_bound, ` + refusedPartySQL("scoped.parties") + `
		from scoped
		left join graph_nodes n on n.kind = scoped.kind and n.id = scoped.id and not scoped.past_bound
		union all
		select n.kind, n.id, null::text, '` + classOrphaned + `', false, false, false
		from graph_nodes n
		where n.kind in (` + orphanKindList + `) and n.state <> '` + graphstore.StateGone + `'
		  and not exists (select 1 from scoped where scoped.kind = n.kind and scoped.id = n.id)
	)`

// refusedPartySQL is graphstore.PartiesValue's refusal spelled in SQL over the text[] expression
// parties: whether one party is empty or holds a character unicode.IsSpace reports (whiteSpaceSQL).
// No writer writes a node between such parties, and every pass of the backfill skips its row
// (partiesRefused), so the census names no command that writes it.
func refusedPartySQL(parties string) string {
	return `exists (select 1 from unnest(` + parties + `) as listed(party)
		where party = '' or translate(party, '` + whiteSpaceSQL + `', '') <> party)`
}

// ProjectKindReport is what the project census read of one kind: how many rows it holds, and how
// many of them are in each class, and, for a kind whose rows only its own creates write, how many
// of its rows name a node that is gone and how many of its nodes not gone no row stands behind.
// PastBound is how many of its rows have an address past the store's bound (graphstore.
// MaxAddressBytes): no writer projects them, so they are a class of their own, in no other count
// and no finding, as the store census counts its own (KindReport.PastBound).
type ProjectKindReport struct {
	Kind                                       string
	Rows                                       int64
	Missing, Gone, Differs, Unscoped, Orphaned int64
	PastBound                                  int64
}

// Findings is how many rows and nodes of the kind are incomplete, over the five classes.
func (r ProjectKindReport) Findings() int64 {
	return r.Missing + r.Gone + r.Differs + r.Unscoped + r.Orphaned
}

// ProjectCensus reads, for every project-owned kind, how many rows it holds and how many of them
// have no node, a node that is gone, a node with no project or parties, or a node in another
// project or between other parties than the row's, how many nodes of an orphan kind not gone no row
// stands behind, and how many rows have an address past the store's bound, in one statement over
// q, which the store census runs in its snapshot.
func ProjectCensus(ctx context.Context, q graphstore.Queryer) ([]ProjectKindReport, error) {
	rows, err := q.Query(ctx, scopedRowsSQL+classedRows+`
		select kind, count(*) filter (where is_row),
		       count(*) filter (where class = '`+classMissing+`'),
		       count(*) filter (where class = '`+classGone+`'),
		       count(*) filter (where class = '`+classDiffers+`'),
		       count(*) filter (where class = '`+classUnscoped+`'),
		       count(*) filter (where class = '`+classOrphaned+`'),
		       count(*) filter (where past_bound)
		from classed group by kind
	`)
	if err != nil {
		return nil, fmt.Errorf("read the project census: %w", err)
	}
	defer rows.Close()
	read := map[string]ProjectKindReport{}
	for rows.Next() {
		var kind ProjectKindReport
		if err := rows.Scan(&kind.Kind, &kind.Rows, &kind.Missing, &kind.Gone, &kind.Differs, &kind.Unscoped, &kind.Orphaned,
			&kind.PastBound); err != nil {
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

// writeProjectCensus prints two lines per kind, in the form the operator's runbook reads
// (cmd/dispatch/README.md, "The node and tuple store census"): its rows and classes, and its rows
// whose address is past the store's bound, on a line of its own so a reader of the first finds it
// unchanged.
func writeProjectCensus(w io.Writer, kinds []ProjectKindReport) {
	for _, kind := range kinds {
		fmt.Fprintf(w, "census: project kind=%s rows=%d missing=%d gone=%d differs=%d unscoped=%d orphaned=%d\n",
			kind.Kind, kind.Rows, kind.Missing, kind.Gone, kind.Differs, kind.Unscoped, kind.Orphaned)
		fmt.Fprintf(w, "census: project past_bound kind=%s addresses=%d\n", kind.Kind, kind.PastBound)
	}
}

// ProjectComplete answers every project-owned row whose node is not complete, and every node of an
// orphan kind, at any state but gone, that no row stands behind, as `<kind> <id>: <class>` (missing, gone, unscoped,
// differs or orphaned) and the class's cure, in one read: the rows and the nodes a read limited by
// a node's project would answer wrongly. An empty answer is every row and every node of every kind
// complete. The cure is the backfill for a row with no node, a node that is not the row's, or a
// node naming neither a project nor parties (backfillCure), but for a row that names a party the
// store refuses, which no writer and no pass of the backfill writes a node for (refusedCure); for
// an orphaned document node at an issue's address, the block-index tick of this build, which
// states it gone as this build's removal states a removed document's node (sweptCure, docs'
// writeObjectNodesAnotherBuildWrote and StateOrphanedGone), since no backfill reads a node no row
// stands behind; none in-band for any other orphaned node, whose row no release removes, so only a
// delete outside the builds made it (unsweptCure); and none for a node that names another project
// or other parties than its row, since the backfill walks no node naming either and only a person
// can tell which of the two is wrong (differsCure).
func ProjectComplete(ctx context.Context, q graphstore.Queryer) ([]string, error) {
	return projectIncomplete(ctx, q, nil)
}

// ProjectCompleteFor is ProjectComplete restricted to the rows of project key: the rows whose node
// the store does not hold, holds gone or holds for another row, or whose node names no project or
// another than the row's, each with its cure. An orphaned node stands behind no row, so it is no
// row of key's. A switch of key to private reads it in its own transaction, since a private
// project's reads would answer such a row wrongly.
func ProjectCompleteFor(ctx context.Context, q graphstore.Queryer, key string) ([]string, error) {
	return incompleteLines(ctx, q, "row_project = $2", nil, key)
}

// The cures ProjectComplete ends its lines with.
const (
	backfillCure = "; run envoy-dispatch backfill-graph-store"
	differsCure  = "; no command cures it: compare the row with its node"
	refusedCure  = "; no command cures it: the row names a party the store refuses, one empty or holding white space"
	sweptCure    = "; this build's block-index tick states it gone"
	unsweptCure  = "; no in-band cure: a delete outside the builds made it"
)

// projectIncomplete is ProjectComplete's read, of at most limit lines in its order when limit is
// not nil: the store census prints only its first lines (reportedDifferences), and a database an
// earlier release left without nodes holds every row of it.
func projectIncomplete(ctx context.Context, q graphstore.Queryer, limit *int) ([]string, error) {
	return incompleteLines(ctx, q, "true", limit)
}

// incompleteLines is the read of ProjectComplete over the lines of classed that restriction
// admits, of at most limit lines in its order when limit is not nil. limit is bound at $1, and
// args, which restriction reads, from $2 on.
func incompleteLines(ctx context.Context, q graphstore.Queryer, restriction string, limit *int, args ...any) ([]string, error) {
	rows, err := q.Query(ctx, scopedRowsSQL+classedRows+`
		select kind, id, class, refused,
		       class = '`+classOrphaned+`' and kind = '`+graphstore.KindArtifact+`' and `+issueDocumentAddress("classed.id")+`
		from classed
		where class is not null and `+restriction+`
		order by kind, id
		limit $1
	`, append([]any{limit}, args...)...)
	if err != nil {
		return nil, fmt.Errorf("read the rows whose node names no project: %w", err)
	}
	var incomplete []string
	var kind, id, class string
	var refused, swept bool
	if _, err := pgx.ForEachRow(rows, []any{&kind, &id, &class, &refused, &swept}, func() error {
		line := kind + " " + id + ": " + class
		switch {
		case class == classOrphaned && swept:
			line += sweptCure
		case class == classOrphaned:
			line += unsweptCure
		case class == classDiffers:
			line += differsCure
		case refused:
			line += refusedCure
		default:
			line += backfillCure
		}
		incomplete = append(incomplete, line)
		return nil
	}); err != nil {
		return nil, fmt.Errorf("read the rows whose node names no project: %w", err)
	}
	return incomplete, nil
}
