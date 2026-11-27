package api

import (
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// personNodeTitleRunes is how much of a row's text the page shows: its first line, cut here so
// the answer carries a title and not a whole comment, by refs.FirstLineWithin, which moves the cut
// back before a reference that crosses it, except where text.HeadRefs says the cut by length
// stands.
const personNodeTitleRunes = 240

// personNode is one thing on a person's page: a node they are tied to, which of the two ties it
// is, and where a reader opens it.
type personNode struct {
	// Kind is the node's kind, as the graph addresses it: the kind the store keys it by, but
	// lens_block for a finding of a lens, which the store keys as a block (graphstore.AddressKind).
	Kind string `json:"kind"`
	// ID is the node's id, as the store keys it.
	ID string `json:"id"`
	// Relations are the tuple kinds that put this node on the page, sorted: authored_by,
	// delivered_to, or both where the person wrote a thing that was delivered back to a
	// session of theirs.
	Relations []string `json:"relations"`
	// Title is the first line of the node's text, cut to personNodeTitleRunes. A comment its
	// author deleted keeps its row with the title "", the body the delete cleared.
	Title string `json:"title"`
	// CreatedAt is when the node was written, which is the order the page reads in.
	CreatedAt time.Time `json:"created_at"`
	// Href is the dashboard path the row opens, as askItemRefExpression answers one for an ask:
	// a comment's own place in its issue or its document, and for a lens, a finding, a risk or a
	// constraint, which the dashboard opens no page of, the page of the issue it stands under. A
	// run of the command bar stands under no issue and has no page, so its row opens nothing and
	// its href is empty.
	Href string `json:"href"`
	// State is the node state the store holds for the node (graphstore.NodeStates), live for a
	// node it holds no row of, as every read of a node answers it. A node that is gone is on no
	// page. Every row of this build carries it, and a server of the release before answers the
	// page without it during a roll, so the document marks it optional (`required:"false"`) and a
	// reader takes a row that carries none as live.
	State string `json:"state" enum:"live,gone,proposed,settled,overtaken" required:"false"`
	// Visibility is the visibility the store holds for the node, project for a node it holds no
	// row of: a node reserved for its author is on its author's page alone, which reads it as
	// author. It is optional in the document as State is, and a row that carries none is
	// project.
	Visibility string `json:"visibility" enum:"project,author" required:"false"`
	// EditedAt is when the comment's author last edited it, so the page marks it as edited;
	// absent on a comment nobody edited and on a row of every other kind.
	EditedAt *time.Time `json:"edited_at,omitempty"`
	// DeletedAt and DeletedBy mark a comment its author deleted, as every read of the post
	// carries them (model.DeletedPostMark), so the page draws the deleted line in its place.
	// Both are absent on a comment that stands and on a row of every other kind.
	DeletedAt *time.Time   `json:"deleted_at,omitempty"`
	DeletedBy *model.Actor `json:"deleted_by,omitempty"`
}

// personNodes is one page of a person's nodes, newest first.
type personNodes struct {
	Person string       `json:"person"`
	Rows   []personNode `json:"rows"`
	Total  int          `json:"total"`
	Limit  int          `json:"limit"`
	Offset int          `json:"offset"`
}

// personNodeTies is what both statements of the page read first: the ties of one person to the
// nodes they hold, one row a tie. $1 is the canonical login.
//
// Both arms read the tuples whose far end is the person's node or a session of theirs, which is
// the inbound read graph_tuples_as_of_in serves, and each keeps the newest tuple per far end
// while that newest one asserts - the current-tuple rule graphstore.CurrentIn and
// graphstore.CurrentOut read a node by, spliced here as graphstore.CurrentTupleRead.
//
//   - authored: the authored_by tuples into the person. A thing a session wrote stays that
//     session's, so it is on no person's page: a session is nobody's until a credential proves
//     it, and proving it says who received a delivery, not who wrote a comment.
//   - received: the delivered_to tuples into each session the server itself proved this
//     person's (the alias_of tuples into them, graphstore.TupleAliasOf). A delivery ends at a
//     session or a role and never at a person, so the alias is the one way a delivery reaches
//     a person at all.
var personNodeTies = `
	with authored as (
		select current.from_kind, current.from_id, '` + graphstore.TupleAuthoredBy + `'::text as relation
		from (
			` + graphstore.CurrentTupleRead(`t.from_kind, t.from_id`, `t.from_kind, t.from_id, t.asserted`,
	`t.to_kind = '`+graphstore.KindPerson+`' and t.to_id = $1
			  and t.kind = '`+graphstore.TupleAuthoredBy+`'`) + `
		) current
		where current.asserted
	), aliases as (
		select current.session
		from (
			` + graphstore.CurrentTupleRead(`t.from_id`, `t.from_id as session, t.asserted`,
	`t.to_kind = '`+graphstore.KindPerson+`' and t.to_id = $1
			  and t.kind = '`+graphstore.TupleAliasOf+`' and t.from_kind = '`+graphstore.KindSession+`'`) + `
		) current
		where current.asserted
	), received as (
		select current.from_kind, current.from_id, '` + graphstore.TupleDeliveredTo + `'::text as relation
		from (
			` + graphstore.CurrentTupleRead(`t.from_kind, t.from_id, t.to_id`, `t.from_kind, t.from_id, t.asserted`,
	`t.to_kind = '`+graphstore.KindSession+`' and t.to_id in (select session from aliases)
			  and t.kind = '`+graphstore.TupleDeliveredTo+`'`) + `
		) current
		where current.asserted
	)`

// personNodeNotGone is the test that the node whose stored state refs.StoredNodeJoin answers
// (refs.StoredState) is not gone, which a node the store holds no row of is not. Both statements
// hold each tied node to it, after the visibility predicate of the node's own kind, whatever kind
// that is (refs.VisibleNode, bound at the acting credential $2, refs.VisibilityArg): so a node
// reserved for its author counts for its author alone, a node of a kind the registry does not
// hold for nobody, and a node that is gone for nobody either.
const personNodeNotGone = refs.StoredState + ` is distinct from '` + graphstore.StateGone + `'`

// personNodesHeld is the nodes one person holds that the reader may see (personNodeNotGone), each
// once with every relation that ties it to them, and beside each the state and the visibility the
// store holds for it (refs.StoredNodeJoin), stored_state and stored_visibility, which its row
// answers.
var personNodesHeld = personNodeTies + `, related as (
		select from_kind, from_id, array_agg(distinct relation order by relation) as relations
		from (select * from authored union all select * from received) ties
		group by from_kind, from_id
	), held as (
		select r.from_kind, r.from_id, r.relations, ` + relatedStoredColumns + `
		from related r
		` + relatedStoredJoin + `
		where ` + refs.VisibleNode("r.from_kind", "r.from_id", "$2") + `
		  and ` + personNodeNotGone + `
	)`

// relatedStoredJoin and relatedStoredColumns are the stored state and visibility of each node
// personNodesHeld ties to the person, and tieStoredJoin the same of each tie personPageShownQuery
// reads.
var (
	relatedStoredJoin, relatedStoredColumns = refs.StoredNodeJoin("r.from_kind", "r.from_id")
	tieStoredJoin, _                        = refs.StoredNodeJoin("tie.from_kind", "tie.from_id")
)

// personPageShownQuery is whether the page of the person $1 names answers anyone but that
// person: the store holds the person, not gone and visible to the reader, and the person holds
// at least one node the reader may see that the page lists (personNodeListed). That second test
// is made over the ties one by one, not over the page's grouped rows (personNodesHeld), so it
// stops at the first tie the reader may see rather than grouping and probing every tie first. It
// reads no tie the page would not list: a person tied to a node of a kind the page opens no row
// of - a block of a document they wrote - holds no row there, and a decision that counted it would
// answer 200 with no rows.
var personPageShownQuery = personNodeTies + `
	select exists (
		select 1 from graph_nodes
		where kind = '` + graphstore.KindPerson + `' and id = $1 and state <> '` + graphstore.StateGone + `'
		  and ` + refs.VisibleRow("graph_nodes", "$2") + `
	) and exists (
		select 1 from (select from_kind, from_id from authored union all select from_kind, from_id from received) tie
		` + tieStoredJoin + `
		where ` + refs.VisibleNode("tie.from_kind", "tie.from_id", "$2") + `
		  and ` + personNodeNotGone + `
		  and ` + personNodeListed("tie.from_kind", "tie.from_id") + `
	)`

// personNodeListed is the test that the page lists the node the store keys at kind and id (SQL
// text), by the conditions personNodesQuery's items arm opens a held node by, arm for arm: a
// comment with a place, its issue's or its document's; a lens, a finding, a risk or a
// constraint (refs.StandingKinds, by the kind the graph addresses it by) that stands under an
// issue (refs.StandingIssueSQL); and a run of the command bar. A node of any other kind is listed
// by no arm, and the test is false for it. Each arm probes one row by its key, so the decision
// that reads it stops at its first listed tie. A comment's node is keyed by the comment's id,
// which the comment arm alone reads as a uuid.
func personNodeListed(kind, id string) string {
	return `(case
		when ` + kind + ` = '` + graphstore.KindComment + `' then exists (
			select 1 from comments c
			left join issues i on i.key = c.issue_key
			left join artifacts a on a.id = c.artifact_id
			where c.id = (` + id + `)::uuid and (i.key is not null or a.id is not null))
		when ` + graphstore.AddressKindSQL(kind, id) + ` in (` + store.QuoteLiterals(refs.StandingKinds()) + `) then exists (
			select 1 from graph_nodes n
			where n.kind = ` + kind + ` and n.id = ` + id + ` and ` + refs.StandingIssueSQL("n.kind", "n.id") + ` is not null)
		when ` + kind + ` = '` + graphstore.KindInteraction + `' then exists (
			select 1 from graph_nodes n where n.kind = ` + kind + ` and n.id = ` + id + `)
		else false
	end)`
}

// personNodesQuery pages the nodes one person holds that the reader may see (personNodesHeld),
// $3 and $4 being the page.
//
// The items arm resolves each held node into the row a reader opens. A node of a kind this build
// cannot open is skipped rather than refused, exactly as refs.resolveNodes skips an end a newer
// build wrote: today the page opens comments, the lenses, findings, risks and constraints, and the
// runs of the command bar; a kind joins here when this build learns to open it, its predicate
// already bound by the arm above, and joins personNodeListed with the same condition, so the
// page's decision (personPageShownQuery) counts what this lists and nothing else. A lens, a
// finding, a risk or a constraint is a row of the store alone: its title is its text
// (refs.StandingTextSQL), its time its row's made_at, and it
// opens the page of the issue it stands under (refs.StandingIssueSQL), so one that stands under no
// issue - a constraint of the lens catalogue - is skipped as a comment with no place is. A run of
// the command bar (an interaction) is a row of the store alone too, listed to the credential that
// asked it and to no other by its kind's whole-credential predicate, which the arm above binds, so
// a person's cookie lists none of what their personal token asked: its title is its words
// (graphstore.AttributeWords), its time its row's made_at, and it opens nothing, since a run stands
// under no issue and the dashboard has no page of one.
//
// The issue a lens, a finding, a risk or a constraint stands under is read once a row, behind
// `offset 0`, which keeps Postgres from copying the walk into both the test and the address that
// read it. Its text is read after the page is cut, for the rows on the page alone: such a row
// carries no text into the page, and its own graph_nodes row, by the kind the store keys it by,
// is read again for the rows the limit kept.
//
// Each comment row carries its comment's marks, as every read of a post does: edited_at where its
// author edited it, and deleted_at and deleted_by where its author deleted it; a row of every
// other kind carries none. A deleted comment keeps its row, and its text is the empty body the
// delete left. An edit or a delete of a comment delivers from the comment's own node (the
// outbox's deliveredItem), so the delivered_to tuples it writes fold into the comment's one row
// in related, and no correction is a row of its own.
var personNodesQuery = personNodesHeld + `, items as (
		select h.from_kind as kind, c.id::text as id, h.relations, c.body as text, c.created_at, h.stored_state, h.stored_visibility,
		       case
		         when i.key is not null then '/issues/' || i.key || '/comments/' || c.id::text
		         else '/projects/' || a.project_key || '/documents/' || a.slug || '?comment=' || c.id::text
		       end as href,
		       h.from_kind as stored_kind, c.edited_at, c.deleted_at, c.deleted_by
		from held h
		join comments c on h.from_kind = '` + graphstore.KindComment + `' and h.from_id = c.id::text
		left join issues i on i.key = c.issue_key
		left join artifacts a on a.id = c.artifact_id
		where (i.key is not null or a.id is not null)
		union all
		select standing.kind, standing.id, standing.relations, null::text, standing.made_at, standing.stored_state,
		       standing.stored_visibility, '/issues/' || standing.issue_key, standing.stored_kind,
		       null::timestamptz, null::timestamptz, null::jsonb
		from (
			select ` + graphstore.AddressKindSQL("h.from_kind", "h.from_id") + ` as kind, h.from_id as id, h.relations,
			       n.made_at, h.stored_state, h.stored_visibility, ` + refs.StandingIssueSQL("n.kind", "n.id") + ` as issue_key,
			       h.from_kind as stored_kind
			from held h
			join graph_nodes n on n.kind = h.from_kind and n.id = h.from_id
			where ` + graphstore.AddressKindSQL("h.from_kind", "h.from_id") + ` in (` + store.QuoteLiterals(refs.StandingKinds()) + `)
			offset 0
		) standing
		where standing.issue_key is not null
		union all
		select h.from_kind, h.from_id, h.relations, coalesce(n.attributes->>'` + graphstore.AttributeWords + `', ''), n.made_at,
		       h.stored_state, h.stored_visibility, '', h.from_kind, null::timestamptz, null::timestamptz, null::jsonb
		from held h
		join graph_nodes n on n.kind = h.from_kind and n.id = h.from_id
		where h.from_kind = '` + graphstore.KindInteraction + `'
	)
	select page.total, page.kind, page.id, page.relations, coalesce(page.text, ` + refs.StandingTextSQL("n", "$2") + `, ''),
	       page.created_at, page.href, page.stored_state, page.stored_visibility, page.edited_at, page.deleted_at, page.deleted_by
	from (
		select count(*) over () as total, kind, id, relations, text, created_at, href, stored_state, stored_visibility, stored_kind,
		       edited_at, deleted_at, deleted_by
		from items
		order by created_at desc, kind, id desc
		limit $3 offset $4
	) page
	left join graph_nodes n on page.text is null and n.kind = page.stored_kind and n.id = page.id
	order by page.created_at desc, page.kind, page.id desc`

// listPersonNodes answers GET /api/v1/people/{login}/nodes: what one person wrote and what
// reached them, newest first.
//
// It is a named read of the graph and no traversal: the route takes the person and a page, and
// takes no kind, no filter and no edge to walk, so no caller chooses what the store is asked.
// The whole route belongs to people - routeScopes gives it model.ScopePerson, and gateRouteScopes
// refuses a credential the deployment configured with 403 OAUTH_SCOPE_REQUIRED before this
// handler runs - and every row it answers passes the visibility predicate of its own kind.
//
// The page answers 200 only where the person holds at least one node the reader may see, through
// each node's own kind's predicate, or where the reader is that person (personPageShownQuery).
// Any other login answers 404, with the body an address nobody holds answers, byte for byte: the
// read says nothing about whether that person exists, has signed in or wrote anything, and a
// person whose only node is reserved for their author has no page anybody else can open. The
// caller's own login is the one exception, answered 200 with what they may see, none at all
// where they hold nothing: a person who has written nothing and been reached by nothing opens
// their own page from the signed-in login, and telling them they are who they signed in as tells
// them nothing new.
//
// Each row's predicate is bound at the credential the request itself proved, not at the person
// actingPerson reads it as: a kind that compares the whole credential - a run of the command bar -
// is listed to the credential that wrote it and to no other, so a person's cookie lists none of
// what their personal token asked and their token none of what the cookie asked, while every other
// kind compares the author a credential proved (graphstore.AuthorIsSQL) and lists the same for both.
// The caller is the one gateRouteScopes resolved for
// the route's scope and handed on the request's context, which optionalActor and actingPerson
// both read, so it is resolved once.
//
// The decision and the list run in one read-only transaction at repeatable read, as
// schemanodes.Read's reads do, so the list reads the snapshot the decision read: a write that
// commits between the two - one that moves the person's only visible node gone, or reserves it
// for its author - is read by neither, and the reader is answered the page as it stood when the
// route decided, never 200 with no rows for a person the route decided holds a node they may see.
func (s *server) listPersonNodes(w http.ResponseWriter, r *http.Request) {
	credential, _, err := s.optionalActor(r)
	if err != nil {
		s.writeAuthenticationError(w, err)
		return
	}
	caller, ok := s.actingPerson(w, r)
	if !ok {
		return
	}
	limit, offset, err := parseMyAnswersPage(r)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	login := model.CanonicalLogin(r.PathValue("login"))
	// The acting credential is marshalled once and bound by both reads: it is one value for
	// the request, and the two statements name the same reader.
	seen := refs.VisibilityArg(credential)
	tx, err := s.deps.Store.Pool.BeginTx(r.Context(), pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	// The transaction reads only, so its end is a rollback whatever happened.
	defer tx.Rollback(r.Context())
	var shown bool
	if err := tx.QueryRow(r.Context(), personPageShownQuery, login, seen).Scan(&shown); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if !shown {
		// caller.ID is the acting person's canonical login (actingPerson), the form login takes
		// above, so a login spelled in another case is still the reader's own.
		if login != caller.ID {
			writeError(w, "NOT_FOUND", http.StatusNotFound, "not found")
			return
		}
		WriteJSON(w, http.StatusOK, personNodes{Person: login, Rows: []personNode{}, Limit: limit, Offset: offset})
		return
	}
	// The test seam: a case commits a write here, between the decision and the list (server
	// betweenPersonPageReads). Nothing but a test sets it.
	if s.betweenPersonPageReads != nil {
		s.betweenPersonPageReads()
	}
	rows, err := tx.Query(r.Context(), personNodesQuery, login, seen, limit, offset)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer rows.Close()
	response := personNodes{Person: login, Rows: []personNode{}, Limit: limit, Offset: offset}
	for rows.Next() {
		var row personNode
		var body string
		var stored refs.StoredNode
		var editedAt, deletedAt *time.Time
		var deletedBy []byte
		if err := rows.Scan(&response.Total, &row.Kind, &row.ID, &row.Relations, &body, &row.CreatedAt, &row.Href,
			&stored.State, &stored.Visibility, &editedAt, &deletedAt, &deletedBy); err != nil {
			s.writeHandlerError(w, err)
			return
		}
		row.State, row.Visibility = refs.StoredStateAndVisibility(stored)
		row.Title = refs.FirstLineWithin(body, s.deps.ServerURL, personNodeTitleRunes)
		row.CreatedAt = row.CreatedAt.UTC()
		if editedAt != nil {
			edited := editedAt.UTC()
			row.EditedAt = &edited
		}
		if row.DeletedAt, row.DeletedBy, err = model.DeletedPostMark(deletedAt, deletedBy); err != nil {
			s.writeHandlerError(w, err)
			return
		}
		response.Rows = append(response.Rows, row)
	}
	if err := rows.Err(); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, response)
}
