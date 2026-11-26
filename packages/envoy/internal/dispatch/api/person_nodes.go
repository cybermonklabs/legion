package api

import (
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// personNodeFirstLineRunes is how much of a row's text the page shows: its first line, cut here
// so the answer carries a line and not a whole comment, by refs.FirstLineWithin, which moves the
// cut back before a reference that crosses it, except where text.HeadRefs says the cut by length
// stands.
const personNodeFirstLineRunes = 240

// personNode is one thing on a person's page: a node they are tied to, which of the two ties it
// is, and where a reader opens it.
type personNode struct {
	// Kind is the node's kind, as the store keys it.
	Kind string `json:"kind"`
	// ID is the node's id, as the store keys it.
	ID string `json:"id"`
	// Relations are the tuple kinds that put this node on the page, sorted: authored_by,
	// delivered_to - into the person's own node or a session the server proved theirs - or both
	// where the person wrote a thing that was delivered back to a session of theirs.
	Relations []string `json:"relations"`
	// FirstLine is the first line of the node's text, cut to personNodeFirstLineRunes. It is
	// named as every cut first line an answer carries is (refs.Candidate's first_line). A post its
	// author deleted keeps its row with the first line "", the body the delete cleared.
	FirstLine string `json:"first_line"`
	// Title is FirstLine again, under the name the release before answers it by: a page of that
	// release draws the row from title, so an open tab of it that reads this build, during a roll
	// or after one, still draws each row's line. It goes once no page of the release before is
	// served (root-deletes.txt).
	// kept for one release: person-node-title
	Title string `json:"title"`
	// FirstLineTree is FirstLine read as the tree a reader is shown, present only for a caller
	// that sent `Dispatch-Trees: 1`: the page's preview draws the line through the one renderer
	// every stored text is drawn with. The line is a cut, which no writer stores a tree of, so it
	// is read where it stands (shapeBuilt), neither asked of the table nor stored in it.
	FirstLineTree model.DisplayTree `json:"first_line_tree,omitempty"`
	// CreatedAt is when the node was written, which is the order the page reads in.
	CreatedAt time.Time `json:"created_at"`
	// Href is the dashboard path the row opens, as askItemRefExpression answers one for an ask.
	Href string `json:"href"`
	// EditedAt is when the author of a comment or a message last edited it, so the page marks it
	// as edited; absent on a post nobody edited, and on an ask.
	EditedAt *time.Time `json:"edited_at,omitempty"`
	// DeletedAt and DeletedBy mark a comment or a message its author deleted, as every read of the
	// post carries them (model.DeletedPostMark), so the page draws the deleted line in its place.
	// Both are absent on a post that stands, and on an ask.
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

// personNodesTied is what both statements of the page read first: the nodes one person is tied
// to, each with the relations that tie it there. $1 is the canonical login.
//
// Every arm reads the tuples whose far end is the person's node or a session of theirs, which is
// the inbound read graph_tuples_as_of_in serves, and each keeps the newest tuple per far end
// while that newest one asserts - the current-tuple rule graphstore.CurrentIn and
// graphstore.CurrentOut read a node by, spliced here as graphstore.CurrentTupleRead.
//
//   - authored: the authored_by tuples into the person. A thing a session wrote stays that
//     session's, so it is on no person's page: a session is nobody's until a credential proves
//     it, and proving it says who received a delivery, not who wrote a comment.
//   - received: the delivered_to tuples into the person's own node, and into each session the
//     server itself proved this person's (the alias_of tuples into them,
//     graphstore.TupleAliasOf). Every write that reaches a person delivers to that person's node
//     (inboxwrite.Record, which appends through graphstore.ToPerson): a reply to the other
//     people taking part in its thread, an ask to the person it waits on, a session's reply to
//     the person who started its direct conversation, and a comment or a message to each person
//     its text mentions. A write that moves who an ask waits on retracts the delivery to the
//     person it waited on (inboxwrite.QueueMoveWaiting), and the current-tuple rule leaves the
//     retracted delivery out. A delivery an attempt records ends at the session it reached or at
//     a role, and the alias is the one way such a delivery reaches a person.
var personNodesTied = `
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
			` + graphstore.CurrentTupleRead(`t.from_kind, t.from_id`, `t.from_kind, t.from_id, t.asserted`,
	`t.to_kind = '`+graphstore.KindPerson+`' and t.to_id = $1
			  and t.kind = '`+graphstore.TupleDeliveredTo+`'`) + `
		) current
		where current.asserted
		union all
		select current.from_kind, current.from_id, '` + graphstore.TupleDeliveredTo + `'::text
		from (
			` + graphstore.CurrentTupleRead(`t.from_kind, t.from_id, t.to_id`, `t.from_kind, t.from_id, t.asserted`,
	`t.to_kind = '`+graphstore.KindSession+`' and t.to_id in (select session from aliases)
			  and t.kind = '`+graphstore.TupleDeliveredTo+`'`) + `
		) current
		where current.asserted
	), related as (
		select from_kind, from_id, array_agg(distinct relation order by relation) as relations
		from (select * from authored union all select * from received) ties
		group by from_kind, from_id
	)`

// personPageShownQuery is whether the page of the person $1 names answers anyone but that person
// (R15 of this change): the store holds the person, not gone, and visible to the reader
// whose acting credential is bound at $2 (refs.VisibilityArg), and the person is tied to at least
// one node, not gone, that the reader may see (personNodesTied). Each tied node is held to the
// predicate of its own kind, read off the one row the store holds of it (refs.VisibleRow), and a
// node the store holds no row of is visible, as refs.VisibleKind reads one. A node of a kind the
// registry holds no predicate for counts for nobody, as VisibleKind refuses one: $3 is every kind
// it holds one for (personPageKinds), so a kind a later release adds ties nobody to a page here
// until this build registers it. A person whose only node is reserved for its author therefore
// has a page that author alone opens.
var personPageShownQuery = personNodesTied + `
	select exists (
		select 1 from graph_nodes
		where kind = '` + graphstore.KindPerson + `' and id = $1 and state <> '` + graphstore.StateGone + `'
		  and ` + refs.VisibleRow("graph_nodes", "$2") + `
	) and exists (
		select 1
		from related r
		left join graph_nodes tied on tied.kind = r.from_kind and tied.id = r.from_id
		where r.from_kind = any($3::text[])
		  and (tied.kind is null
		       or (tied.state <> '` + graphstore.StateGone + `' and ` + refs.VisibleRow("tied", "$2") + `))
	)`

// personPageKinds is every node kind the registry holds a predicate for (refs.VisibilityKinds),
// which personPageShownQuery binds at $3. It is read once: refs fills its registry before this
// package initializes, and nothing moves it afterwards.
var personPageKinds = refs.VisibilityKinds()

// personNodeItems is each node personNodesTied ties the person to as the row a reader opens - its
// kind, id and relations, its text, when it was written and the dashboard path it opens at - kept
// only where the reader whose acting credential is bound at $2 (refs.VisibilityArg) may see it,
// through the predicate of its own kind bound from the registry (refs.VisibleKind), so a node its
// author keeps to themselves is on no other person's page. The list pages it (personNodesQuery).
// Whether the page answers anyone is not its to say: personPageShownQuery reads the ties
// themselves, so a reader that rule answers 200 for a person all of whose nodes they may see are
// ones this read skips reads that page with no rows.
//
// It opens the three kinds a person writes and a write delivers to them (graphstore.AuthorFacts,
// inboxwrite.Record), each read by its row's key, at the path the dashboard opens it at:
//
//   - a comment, on an issue (`/issues/<key>/comments/<id>`) or on a project's document
//     (`?comment=`), as GET /api/v1/me/replies opens one (myRepliesPageRest);
//   - an ask, on an issue or on a project's document, at the path askItemRefExpression spells;
//   - a message on an issue (`/issues/<key>/messages/<id>`), as GET /api/v1/me/replies opens one.
//
// Anything else is skipped rather than refused, exactly as refs.resolveNodes skips an end a newer
// build wrote: a node of a kind this build cannot open, which joins here when it learns to; a
// message on no issue, which a direct conversation is made of; and a comment or an ask on a
// document no project owns, an agent conversation's file. Neither of the last two has an address
// of its own (refs' itemRef) or a path on the dashboard: a direct conversation is opened whole,
// at its session's page (`/agents/<session>/live`), and not at one of its messages, and the page
// of an agent's file shows no comment or ask. No writer states the node of a comment, an ask or a
// message gone - a document's is the one this store's writers move gone, on its removal or
// retirement - so a row standing is a node the store holds live.
//
// Each comment and message row carries its post's marks, as every read of a post does: edited_at
// where its author edited it, and deleted_at and deleted_by where its author deleted it. An ask
// row carries none. A deleted post keeps its row, and its text is the empty body the delete left.
// An edit or a delete of a comment delivers from the comment's own node (the outbox's
// deliveredItem), so the delivered_to tuples it writes fold into the comment's one row in
// related, and no correction is a row of its own.
var personNodeItems = `, items as (
		select r.from_kind as kind, c.id::text as id, r.relations, c.body as text, c.created_at,
		       c.edited_at, c.deleted_at, c.deleted_by,
		       case
		         when i.key is not null then '/issues/' || i.key || '/comments/' || c.id::text
		         else '/projects/' || a.project_key || '/documents/' || a.slug || '?comment=' || c.id::text
		       end as href
		from related r
		join comments c on r.from_kind = '` + graphstore.KindComment + `' and r.from_id = c.id::text
		left join issues i on i.key = c.issue_key
		left join artifacts a on a.id = c.artifact_id
		where (i.key is not null or a.project_key is not null)
		  and ` + refs.VisibleKind(graphstore.KindComment, "c.id::text", "$2") + `
		union all
		select r.from_kind, a.id::text, r.relations, a.question, a.created_at,
		       null::timestamptz, null::timestamptz, null::jsonb, ` + askItemRefExpression + `
		from related r
		join asks a on r.from_kind = '` + graphstore.KindAsk + `' and r.from_id = a.id::text
		left join issues i on i.key = a.issue_key
		left join artifacts ar on ar.id = a.artifact_id
		where (i.key is not null or ar.project_key is not null)
		  and ` + refs.VisibleKind(graphstore.KindAsk, "a.id::text", "$2") + `
		union all
		select r.from_kind, m.id::text, r.relations, m.body, m.created_at,
		       m.edited_at, m.deleted_at, m.deleted_by,
		       '/issues/' || i.key || '/messages/' || m.id::text
		from related r
		join messages m on r.from_kind = '` + graphstore.KindMessage + `' and r.from_id = m.id::text
		join issues i on i.key = m.issue_key
		where ` + refs.VisibleKind(graphstore.KindMessage, "m.id::text", "$2") + `
	)`

// personNodesQuery pages the rows of one person's page (personNodeItems), newest first. $2 is the
// acting credential the visibility predicate is bound at (refs.VisibilityArg), $3 and $4 the page.
var personNodesQuery = personNodesTied + personNodeItems + `
	select count(*) over () as total, kind, id, relations, text, created_at, href, edited_at, deleted_at, deleted_by
	from items
	order by created_at desc, kind, id desc
	limit $3 offset $4`

// listPersonNodes answers GET /api/v1/people/{login}/nodes: what one person wrote and what
// reached them, newest first.
//
// It is a named read of the graph and no traversal: the route takes the person and a page, and
// takes no kind, no filter and no edge to walk, so no caller chooses what the store is asked.
// The whole route belongs to people - routeScopes gives it model.ScopePerson, and gateRouteScopes
// refuses a credential the deployment configured with 403 OAUTH_SCOPE_REQUIRED before this
// handler runs - and every row it answers passes the visibility predicate of its own kind.
//
// The page answers 200 only where the person holds at least one node the reader may see, each
// through its own kind's predicate, or where the reader is that person (personPageShownQuery).
// Any other login answers 404, with the body a login nobody holds answers, byte for byte: the
// read says nothing about whether that person exists, has signed in or wrote anything, and a
// person whose only node is reserved for its author - a node a later release writes, beside
// which this one serves during a roll and alone after a rollback - has no page anybody else can
// open. The caller's own login is the one exception, answered 200 with what they may see, none
// at all where they hold nothing: a person who has written nothing and been reached by nothing
// opens their own page from the signed-in login, and telling them they are who they signed in as
// tells them nothing new.
//
// The decision and the list run in one read-only transaction at repeatable read, as
// schemanodes.Read's reads do, so the list reads the snapshot the decision read: a write that
// commits between the two - one that moves the person's only visible node gone, or reserves it
// for its author - is read by neither, and the reader is answered the page as it stood when the
// route decided, never 200 with no rows for a person the route decided holds a node they may see
// of a kind and in a place the list opens (personNodeItems).
func (s *server) listPersonNodes(w http.ResponseWriter, r *http.Request) {
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
	seen := refs.VisibilityArg(caller)
	tx, err := s.deps.Store.Pool.BeginTx(r.Context(), pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	// The transaction reads only, so its end is a rollback whatever happened.
	defer tx.Rollback(r.Context())
	var shown bool
	if err := tx.QueryRow(r.Context(), personPageShownQuery, login, seen, personPageKinds).Scan(&shown); err != nil {
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
		var editedAt, deletedAt *time.Time
		var deletedBy []byte
		if err := rows.Scan(&response.Total, &row.Kind, &row.ID, &row.Relations, &body, &row.CreatedAt, &row.Href,
			&editedAt, &deletedAt, &deletedBy); err != nil {
			s.writeHandlerError(w, err)
			return
		}
		row.FirstLine = refs.FirstLineWithin(body, s.deps.ServerURL, personNodeFirstLineRunes)
		// kept for one release: person-node-title
		row.Title = row.FirstLine
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
	shapeBuilt(wantsDisplayTrees(r), func(reader *displayTreeReader) {
		for index := range response.Rows {
			reader.text(&response.Rows[index].FirstLineTree, response.Rows[index].FirstLine)
		}
	})
	WriteJSON(w, http.StatusOK, response)
}
