package api

import (
	"net/http"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// personNodeTitleRunes is how much of a row's text the page shows: its first line, cut here so
// the answer carries a title and not a whole comment.
const personNodeTitleRunes = 240

// personNode is one thing on a person's page: a node they are tied to, which of the two ties it
// is, and where a reader opens it.
type personNode struct {
	// Kind is the node's kind, as the store keys it.
	Kind string `json:"kind"`
	// ID is the node's id, as the store keys it.
	ID string `json:"id"`
	// Relations are the tuple kinds that put this node on the page, sorted: authored_by,
	// delivered_to, or both where the person wrote a thing that was delivered back to a
	// session of theirs.
	Relations []string `json:"relations"`
	// Title is the first line of the node's text, cut to personNodeTitleRunes.
	Title string `json:"title"`
	// CreatedAt is when the node was written, which is the order the page reads in.
	CreatedAt time.Time `json:"created_at"`
	// Href is the dashboard path the row opens, as askItemRefExpression answers one for an ask.
	Href string `json:"href"`
}

// personNodes is one page of a person's nodes, newest first.
type personNodes struct {
	Person string       `json:"person"`
	Rows   []personNode `json:"rows"`
	Total  int          `json:"total"`
	Limit  int          `json:"limit"`
	Offset int          `json:"offset"`
}

// personNodesQuery pages the nodes one person is tied to. $1 is the canonical login, $2 the
// acting credential the visibility predicate is bound at (refs.VisibilityArg), $3 and $4 the
// page.
//
// Both arms read the tuples whose far end is the person's node or a session of theirs, which is
// the inbound read graph_tuples_inbound serves, and each keeps the newest tuple per far end
// while that newest one asserts - the current-tuple rule graphstore.CurrentIn and
// graphstore.CurrentOut read a node by, written here as the `distinct on` the index's own column
// order walks, so the planner sorts nothing.
//
//   - authored: the authored_by tuples into the person. A thing a session wrote stays that
//     session's, so it is on no person's page: a session is nobody's until a credential proves
//     it, and proving it says who received a delivery, not who wrote a comment.
//   - received: the delivered_to tuples into each session the server itself proved this
//     person's (the alias_of tuples into them, graphstore.TupleAliasOf). A delivery ends at a
//     session or a role and never at a person, so the alias is the one way a delivery reaches
//     a person at all.
//
// The items arm resolves each tied node into the row a reader opens. A node of a kind this build
// cannot open is skipped rather than refused, exactly as refs.resolveNodes skips an end a newer
// build wrote: today the store ties a person to comments, and a kind a later child moves joins
// here when this build learns to open it. The visibility predicate of that kind is bound from
// the registry (refs.VisibleKind), so a comment its author keeps to themselves is on no other
// person's page.
var personNodesQuery = `
	with authored as (
		select current.from_kind, current.from_id, '` + graphstore.TupleAuthoredBy + `'::text as relation
		from (
			select distinct on (t.from_kind, t.from_id) t.from_kind, t.from_id, t.asserted
			from graph_tuples t
			where t.to_kind = '` + graphstore.KindPerson + `' and t.to_id = $1
			  and t.kind = '` + graphstore.TupleAuthoredBy + `'
			order by t.from_kind, t.from_id, t.seq desc
		) current
		where current.asserted
	), aliases as (
		select current.session
		from (
			select distinct on (t.from_id) t.from_id as session, t.asserted
			from graph_tuples t
			where t.to_kind = '` + graphstore.KindPerson + `' and t.to_id = $1
			  and t.kind = '` + graphstore.TupleAliasOf + `' and t.from_kind = '` + graphstore.KindSession + `'
			order by t.from_id, t.seq desc
		) current
		where current.asserted
	), received as (
		select current.from_kind, current.from_id, '` + graphstore.TupleDeliveredTo + `'::text as relation
		from (
			select distinct on (t.from_kind, t.from_id, t.to_id) t.from_kind, t.from_id, t.asserted
			from aliases
			join graph_tuples t
			  on t.to_kind = '` + graphstore.KindSession + `' and t.to_id = aliases.session
			 and t.kind = '` + graphstore.TupleDeliveredTo + `'
			order by t.from_kind, t.from_id, t.to_id, t.seq desc
		) current
		where current.asserted
	), related as (
		select from_kind, from_id, array_agg(distinct relation order by relation) as relations
		from (select * from authored union all select * from received) ties
		group by from_kind, from_id
	), items as (
		select r.from_kind as kind, c.id::text as id, r.relations, c.body as text, c.created_at,
		       case
		         when i.key is not null then '/issues/' || i.key || '/comments/' || c.id::text
		         else '/projects/' || a.project_key || '/documents/' || a.slug || '?comment=' || c.id::text
		       end as href
		from related r
		join comments c on r.from_kind = 'comment' and r.from_id = c.id::text
		left join issues i on i.key = c.issue_key
		left join artifacts a on a.id = c.artifact_id
		where (i.key is not null or a.id is not null)
		  and ` + refs.VisibleKind("comment", "c.id::text", "$2") + `
	)
	select count(*) over () as total, kind, id, relations, text, created_at, href
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
// A login the store holds no person node for is 404, with the body an address nobody holds
// answers: the read says nothing about whether that person exists, has signed in or wrote
// anything.
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
	var held bool
	if err := s.deps.Store.Pool.QueryRow(r.Context(), `
		select exists (
			select 1 from graph_nodes
			where kind = $1 and id = $2 and state = $3 and `+refs.VisibleRow("graph_nodes", "$4")+`
		)`, graphstore.KindPerson, login, graphstore.StateLive, refs.VisibilityArg(caller)).Scan(&held); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if !held {
		writeError(w, "NOT_FOUND", http.StatusNotFound, "not found")
		return
	}
	rows, err := s.deps.Store.Pool.Query(r.Context(), personNodesQuery, login, refs.VisibilityArg(caller), limit, offset)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer rows.Close()
	response := personNodes{Person: login, Rows: []personNode{}, Limit: limit, Offset: offset}
	for rows.Next() {
		var row personNode
		var body string
		if err := rows.Scan(&response.Total, &row.Kind, &row.ID, &row.Relations, &body, &row.CreatedAt, &row.Href); err != nil {
			s.writeHandlerError(w, err)
			return
		}
		first, _, _ := strings.Cut(body, "\n")
		row.Title = text.HeadRunes(first, personNodeTitleRunes)
		row.CreatedAt = row.CreatedAt.UTC()
		response.Rows = append(response.Rows, row)
	}
	if err := rows.Err(); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, response)
}
