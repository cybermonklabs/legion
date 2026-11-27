package api

// approve_policy.go is what an approve-only token may do, in one file, so the policy is read in one
// place and none of its checks hides among the scope and session layers they run beside. An
// approve-only token is the token the secrets broker mints for a person's approve grant, binds to
// its own pod's key and spends through its proxy, never released (model.Credential.ApproveOnly). It
// acts as its person with the scopes person and approve (brokerApproveOnlyScopes), never p0, so
// these checks alone keep it to approvals.
//
// The approve list (approveList: the route files that state ApproveOnly) is compiled against the
// route table once, as the table is built (applyApprovePolicy), and the rows carry the result: ApproveOnly, ApproveStates and
// ApproveApprovalAsks. The three checks and the refusal's wording read those rows, never the list.
// The checks sit at three places in a route's chain (routeHandler), each before the layers that
// look a key up, claim it or replay what it holds:
//
//   - the route, outside every other check of the route, beside the read-only gate and inside
//     decideBrokerToken, which resolves the credential it reads (refuseApproveOnlyToken): a route
//     the list does not name is 403 APPROVE_ONLY_TOKEN;
//   - the state, once the decoder has read the body within the row's bound and before the
//     authority rules (gateApproveOnlyStates): PUT /api/v2/credential-requests/{id}/state takes
//     the token for the states approved and denied, PUT /api/v2/asks/{id}/state for answered and
//     PUT /api/v2/issues/{key}/decisions/{block_id}/state for kept, and every other state such a
//     row declares is 403 APPROVE_ONLY_TOKEN; a state the row does not declare is the decoder's 400
//     STATE_INVALID, for every caller;
//   - the ask, outside the keyed layer (gateApproveOnlyAskAnswer): a keyed answer of an ask that
//     is no approval request is 403 APPROVE_ONLY_TOKEN before its key is looked up, claimed or
//     replayed, so the token is never handed an answer that another request stored under the same
//     person's login, and an off-policy ask claims no key. The write holds the same rule in its
//     transaction (approveOnlyAnswerRefusal), where the ask's kind cannot move.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
)

// approveRoute is one entry of the approve list: an /api/v1 route the token may call, and what
// that route's /api/v2 successor takes of it.
type approveRoute struct {
	// v1 is the /api/v1 route, "<method> <pattern>".
	v1 string
	// state is the value of the body's `state` the token may send the successor, for a successor
	// that takes several operations by it; empty for a successor the token may call whole. A
	// successor that takes several operations by state has an entry for each state the token may
	// send it, and one with no state is refused when the table is built.
	state string
	// approvalsOnly is a route that answers asks of every kind, whose answer the token is held to
	// approval requests for, on both versions (approveOnlyAnswerRefusal).
	approvalsOnly bool
}

// approveList is the approve list: every /api/v1 route an approve-only token may call, each route
// file whose declaration states ApproveOnly, in the order declared walks them, with what it takes
// of the /api/v2 route that answers for it (ApproveState, ApproveApprovalAsks). Every other route
// of the table, and GET /ws/live beside it, answers the token 403 APPROVE_ONLY_TOKEN before anything
// else the route does (refuseApproveOnlyToken), so a route added later stays closed to it until its
// file states ApproveOnly. The token may call, on /api/v2, the route each entry has as its
// Successor and no other, whole or for the one state its entry names, so the list is written once
// and the two versions cannot drift.
//
// The list is the routes whose OAuthScopes name approve, the routes that settle what a person
// settles, less two: the mint and the change of a personal token, since a token they made or
// widened would be an approve credential written to the box the session runs on, which is what
// the proxy exists to keep off it. The revoke of a personal token stays. Beside them is the answer
// of an ask, which checks approve in its handler for an approval request and refuses an
// approve-only token the answer of every other ask. The answer's entry names the state answered:
// the other states of the ask's /api/v2 route are no answer, so the token is not served them.
//
// routes refuses a table this list does not map onto (applyApprovePolicy).
func approveList(declared []routeDecl) []approveRoute {
	var list []approveRoute
	for _, route := range declared {
		if route.ApproveOnly {
			list = append(list, approveRoute{v1: route.Method + " " + route.Pattern, state: route.ApproveState, approvalsOnly: route.ApproveApprovalAsks})
		}
	}
	return list
}

// applyApprovePolicy compiles list against table once and stamps the result on table's rows:
// ApproveOnly on each /api/v1 route the list names and on the /api/v2 route each has as its
// Successor, ApproveStates on a successor the token may call for some of its states, and
// ApproveApprovalAsks on each route of an entry that answers asks of every kind. It answers every
// way the list fails to map onto the table, sorted and each naming the route it concerns, and
// stamps nothing where it finds one:
//
//   - an entry that names a route twice, or one that is no /api/v1 route of the table;
//   - an entry whose route has no /api/v2 successor of its own for the token to call: none
//     declared, one that a successor row deletes in favour of a filter, or one the table does not
//     mount;
//   - an entry that names no state for a successor that takes several operations by state, which
//     would admit every one of them, and an entry that names a state of a successor that takes
//     none, which would admit the whole route for one state of it: the privileges of the two
//     versions would differ from what the list says;
//   - an entry that names a state of a successor the decoder does not read (apiRoute.decoded),
//     where no state check is mounted (gateApproveOnlyStates), so the token could send it every
//     state;
//   - an entry that names a state its successor does not declare.
//
// An approve list that opens nothing, or opens more than it names, reads as though it did not, so
// routes refuses to build a table it names any of rather than mount it.
func applyApprovePolicy(table []apiRoute, list []approveRoute) []string {
	position := make(map[string]int, len(table))
	for at, route := range table {
		position[route.Method+" "+route.Pattern] = at
	}
	// admission is what the list gives one row, of either version: the states it takes of the
	// token, none for a row it takes whole and for every /api/v1 row, and whether the row answers
	// asks of every kind.
	type admission struct {
		states []string
		asks   bool
	}
	var faults []string
	listed := map[string]bool{}
	admitted := map[int]*admission{}
	for _, entry := range list {
		at, mounted := position[entry.v1]
		switch {
		case listed[entry.v1]:
			faults = append(faults, entry.v1+" is listed twice")
			continue
		case !mounted || table[at].Version != apiV1:
			faults = append(faults, entry.v1+" is no /api/v1 route the table mounts")
			continue
		}
		listed[entry.v1] = true
		next := table[at].Successor
		target := next.Method + " " + versionPrefix(apiV2) + next.Pattern
		row, served := position[target]
		if next.Method == "" || next.Deleted || !served {
			faults = append(faults, entry.v1+" has no /api/v2 route of its own for the token to call ("+target+")")
			continue
		}
		body := table[row].Body
		switch {
		case entry.state == "" && body.Kind == bodyStateUnion:
			faults = append(faults, entry.v1+" names no state, and its successor "+target+" takes several operations by state, which the list would all admit")
			continue
		case entry.state != "" && body.Kind != bodyStateUnion:
			faults = append(faults, fmt.Sprintf("%s names the state %s, and its successor %s takes no state", entry.v1, entry.state, target))
			continue
		case entry.state != "" && !table[row].decoded():
			faults = append(faults, fmt.Sprintf("%s names the state %s, and the decoder does not read its successor %s, so no state check would hold the token to that state", entry.v1, entry.state, target))
			continue
		case entry.state != "" && body.Arms[entry.state] == nil:
			faults = append(faults, fmt.Sprintf("%s names the state %s, which its successor %s does not declare", entry.v1, entry.state, target))
			continue
		}
		// The /api/v1 route and its successor take the same admission, but for the state: the
		// /api/v1 route takes none, so its states stay nil.
		for _, stamped := range [...]int{at, row} {
			if admitted[stamped] == nil {
				admitted[stamped] = &admission{}
			}
			admitted[stamped].asks = admitted[stamped].asks || entry.approvalsOnly
		}
		if entry.state != "" {
			admitted[row].states = append(admitted[row].states, entry.state)
		}
	}
	if len(faults) > 0 {
		slices.Sort(faults)
		return slices.Compact(faults)
	}
	for row, granted := range admitted {
		slices.Sort(granted.states)
		table[row].ApproveOnly, table[row].ApproveApprovalAsks = true, granted.asks
		table[row].ApproveStates = slices.Compact(granted.states)
	}
	return nil
}

// approveOnlyRefusalFor is the message of every 403 APPROVE_ONLY_TOKEN refuseApproveOnlyToken
// answers: the approve list in words, the /api/v1 routes it names and the /api/v2 routes each has
// as its successor, the states of a route that takes several by state beside it. It reads the
// rows applyApprovePolicy stamped and derives nothing, so it names every route the token may call
// on the day the table changes, and each is a route a running Dispatch serves.
func approveOnlyRefusalFor(routes []apiRoute) string {
	var v1, v2 []string
	for _, route := range routes {
		if !route.ApproveOnly {
			continue
		}
		name := route.Method + " " + route.Pattern
		if route.Version == apiV1 {
			v1 = append(v1, name)
			continue
		}
		if route.ApproveStates != nil {
			name += " (state " + strings.Join(route.ApproveStates, " or ") + ")"
		}
		v2 = append(v2, name)
	}
	slices.Sort(v1)
	slices.Sort(v2)
	return "this request's credential is an approve-only token, which may call only the routes Dispatch's approve list names (on /api/v1: " +
		strings.Join(v1, ", ") + "; on /api/v2: " + strings.Join(v2, ", ") + "): nothing ran"
}

// refuseApproveOnlyToken is the route check, the one place an approve-only token is held to the
// approve list: register wraps every row of the table in it, listed being the row's
// apiRoute.ApproveOnly, and GET /ws/live with listed false. On a route the list does not name, a
// caller decideBrokerToken resolved to an approve-only credential is answered 403
// APPROVE_ONLY_TOKEN, with the table's one message (server.approveRefusal), and next does not run;
// every other request runs next as it always did. A row the list names is handed back as it is,
// and the state and ask checks hold the token to less on the rows that need them. It reads the
// decision decideBrokerToken carried and resolves nothing of its own, for the reason
// refuseReadOnlyToken gives, and carries no WWW-Authenticate challenge, since no scope makes such
// a route the token's to call.
func (s *server) refuseApproveOnlyToken(listed bool, next http.HandlerFunc) http.HandlerFunc {
	if listed {
		return next
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if approveOnlyRequest(r) {
			writeError(w, r, apicode.ApproveOnlyToken, s.approveRefusal)
			return
		}
		next(w, r)
	}
}

// gateApproveOnlyStates is the state check of a /api/v2 row the list names by state
// (apiRoute.ApproveStates): an approve-only token that sends such a row a state outside those, or a
// body that names none, is answered 403 APPROVE_ONLY_TOKEN and nothing runs. register wraps it just
// outside gateV2Authorities, so it runs on the body the decoder read within the row's bound: after
// the route's scope, login, project and session checks, and before the authority gate's rules and
// the keyed layer, so a refused request claims no idempotency key. register mounts it only on a row
// the decoder reads, and applyApprovePolicy refuses a list that names a state of any other row. A
// request of any other credential passes untouched, and so does a row the list names whole or not
// at all, which is returned unwrapped. A state the row does not declare never reaches it: the
// decoder refused that 400 STATE_INVALID.
func (s *server) gateApproveOnlyStates(route apiRoute, next http.HandlerFunc) http.HandlerFunc {
	if len(route.ApproveStates) == 0 {
		return next
	}
	return func(w http.ResponseWriter, r *http.Request) {
		decoded, ok := decodedV2(r)
		if !ok {
			// register wraps this gate inside the decoder's layers alone, so a request that
			// reaches it undecoded is the server's own fault, and it is refused rather than let
			// past a rule nothing could judge.
			s.writeHandlerError(w, r, fmt.Errorf("%s %s: the approve-only state gate holds no decoded request", route.Method, route.Pattern))
			return
		}
		if approveOnlyRequest(r) {
			if state, outside := decoded.stateOutside(route.ApproveStates); outside {
				writeError(w, r, apicode.ApproveOnlyToken, fmt.Sprintf(
					"this request's credential is an approve-only token, which may send %s %s the state %s alone, and this request sends state %q: nothing ran",
					route.Method, route.Pattern, strings.Join(route.ApproveStates, " or "), state))
				return
			}
		}
		next(w, r)
	}
}

// stateOutside is the first value of the body's `state` that none of allowed names, and true, or
// "" and true for a body that sends no `state` at all; false when every `state` it sends is one of
// allowed. It reads the body as holdsFor reads a rule on a member, every member that matches
// `state` ignoring case, so no spelling of the member reaches the handler past the approve-only
// state check (apiRoute.ApproveStates). The decoder has refused a state its route has no arm for,
// and a body with none, before the check runs.
func (d *v2Request) stateOutside(allowed []string) (string, bool) {
	found := false
	for name, raw := range d.members {
		if !strings.EqualFold(name, "state") {
			continue
		}
		found = true
		if state := memberValue(raw); !slices.Contains(allowed, state) {
			return state, true
		}
	}
	return "", !found
}

// gateApproveOnlyAskAnswer is the ask check of the answer routes, POST /api/v1/asks/{id}/answer and
// PUT /api/v2/asks/{id}/state (apiRoute.ApproveApprovalAsks), which serve asks of every kind and
// which the token may call for an approval request alone. An approve-only token that sends one an
// Idempotency-Key is answered 403 APPROVE_ONLY_TOKEN when the ask its path names is no approval
// request, before the keyed layer looks the key up, claims it or replays what it holds. The keyed
// layer keeps one answer per login and the token acts as its person, so without this check the
// token would be handed the answer another credential of that person stored under the same key for
// an ask it may not answer, and would claim keys for asks it may not touch.
//
// register wraps it just outside the keyed layer: on /api/v2 after the decoder has read the body
// within its bound and the state check and the authority rules have held the token to the state
// answered, and on both versions after the path's id was resolved into the whole one. A request
// with no key is not judged here: it does no keyed lookup, and the write holds the answer to the
// same rule inside its transaction (approveOnlyAnswerRefusal), so an unkeyed request keeps the
// refusals the engine already gave it, an answered ask's conflict on /api/v1 included. The ask's
// kind is read alone, not the ask. An id no ask has, a kind that cannot be read, and a database
// that cannot be reached refuse the request: it is refused for the ask it names or as an error of
// the server, never let through to a key it cannot judge.
func (s *server) gateApproveOnlyAskAnswer(route apiRoute, next http.HandlerFunc) http.HandlerFunc {
	if !route.ApproveApprovalAsks {
		return next
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if _, keyed, _ := requestKeyHeader(r); !keyed || !approveOnlyRequest(r) {
			next(w, r)
			return
		}
		id := r.PathValue("id")
		if route.Version != apiV2 && !requireUUIDPath(w, r, "ask", apicode.AskIDInput) {
			return
		}
		kind, err := s.askKind(r.Context(), id)
		switch {
		case errors.Is(err, pgx.ErrNoRows) && route.Version == apiV2:
			// The route's own 404, as the handler answers it. On /api/v1 writeHandlerError answers
			// pgx.ErrNoRows as the handler does.
			err = askNotFoundV2(id)
		case err == nil:
			err = approveOnlyAnswerRefusal(true, kind)
		}
		if err != nil {
			s.writeHandlerError(w, r, err)
			return
		}
		next(w, r)
	}
}

// askKind reads the kind of the ask id names, and pgx.ErrNoRows where no ask has that id: the kind
// alone, so a check that needs no more loads neither the ask's answer nor its content. An id that
// is no uuid names no ask and is not read.
func (s *server) askKind(ctx context.Context, id string) (string, error) {
	if _, err := uuid.Parse(id); err != nil {
		return "", pgx.ErrNoRows
	}
	pool, err := s.readyPool()
	if err != nil {
		return "", err
	}
	var kind string
	switch err := pool.QueryRow(ctx, `select kind from asks where id = $1`, id).Scan(&kind); {
	case errors.Is(err, pgx.ErrNoRows):
		return "", err
	case err != nil:
		return "", fmt.Errorf("read the kind of ask %s: %w", id, err)
	}
	return kind, nil
}

// approveOnlyRequest reports whether decideBrokerToken resolved r's caller to an approve-only
// credential (model.Credential.ApproveOnly), read as readOnlyCredential reads a read-only one
// (resolvedCredential). The person actor a handler acts as (actingPerson) carries no credential,
// so a handler that holds such a token to less than its route reads it here: the answer of an ask,
// which holds it to approval requests alone.
func approveOnlyRequest(r *http.Request) bool {
	credential := resolvedCredential(r.Context())
	return credential != nil && credential.ApproveOnly
}

// approveOnlyAnswerRefusal is the refusal of an answer to an ask of kind sent under an approve-only
// token (approveOnly, from approveOnlyRequest), and nil for any other credential and for an
// approval request: the token answers an approval request and no other ask, 403 APPROVE_ONLY_TOKEN
// under the approve-only checks' own code. recordAnswer holds every answer to it inside its
// transaction, PUT /api/v2/asks/{id}/state holds the answer it would answer as it stands to it
// (putAskStateV2), so that no-op tells the token nothing about an ask it may not answer, and
// gateApproveOnlyAskAnswer holds a keyed answer to it before any key is judged.
func approveOnlyAnswerRefusal(approveOnly bool, kind string) error {
	if approveOnly && kind != "approval" {
		return errorf(apicode.ApproveOnlyToken,
			"this request's credential is an approve-only token, which answers an approval request and no other ask, and this ask is a %s: nothing was recorded", kind)
	}
	return nil
}
