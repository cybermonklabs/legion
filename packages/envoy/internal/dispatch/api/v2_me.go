package api

// v2_me.go is the caller's own things under /api/v2/me (R1 of dispatch://INFRA-4071/spec; the
// route map's rule 10): who the caller is, in one shape where /api/v1/whoami
// answers two; their personal agent tokens; their answers; and their marks, the person's own state
// on one object, which eleven /api/v1 rows wrote and read one kind at a time and four routes
// answer here (the route map's section (e)). Every route runs the work its /api/v1 route runs
// (agentTokensOf, createAgentTokenFrom, updateAgentTokenScopesFrom, revokeAgentTokenOf,
// readMyAnswers, writeUserIssueStateIn, writeUserAgentStateIn, writeDocumentRead, moveThreadReadIn,
// snoozeAskIn, unsnoozeAskIn, markInboxReadThreads, dismissInboxThreads), each path object checked
// first and answered with its own 404 (R7): the {id} of an agent mark is a session and keeps the
// answer /api/v1 gives it (R7), and an absent mark is its target's default state.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// The refusals of the objects a /api/v2/me route's path names (dispatch://INFRA-4071/spec R7).
var (
	agentTokenMissingRefusal = apicode.Refusal{Condition: apicode.AgentTokenMissing, Code: apicode.AgentTokenNotFound}
	askMissingRefusal        = apicode.Refusal{Condition: apicode.AskMissing, Code: apicode.AskNotFound}
	threadMissingRefusal     = apicode.Refusal{Condition: apicode.ThreadMissing, Code: apicode.ThreadNotFound}
)

// markTargetRefusals are the refusals of a mark's {kind}/{id}: a kind that is none of the six, and
// the 404s of its target, by kind, which must exist, but for an agent mark's session, which keeps
// the answer /api/v1 gives it (dispatch://INFRA-4071/spec R7).
func markTargetRefusals() []apicode.Refusal {
	return []apicode.Refusal{
		undeclaredKeyRefusal, issueMissingRefusal, documentMissingRefusal, threadMissingRefusal,
		messageMissingRefusal, askMissingRefusal,
		{Condition: apicode.MarkKindUnknown, Code: apicode.InvalidKind},
	}
}

// markWriteRefusals are the refusals of a write of marks (writeMarks): its targets' (markTargetRefusals),
// a state or a read_version it refuses, an ask mark's snooze it refuses, and a mark that moved from
// the state the write was read under.
func markWriteRefusals() []apicode.Refusal {
	return append(markTargetRefusals(),
		apicode.Refusal{Condition: apicode.MarkStateInvalid, Code: apicode.InvalidState},
		apicode.Refusal{Condition: apicode.SnoozeInvalid, Code: apicode.SnoozeInput},
		apicode.Refusal{Condition: apicode.MarkStateMoved, Code: apicode.StateStale},
	)
}

// tokenScopesInvalidRefusal is the refusal of a personal token's scopes, at its create and at a
// change of them (tokenScopes).
var tokenScopesInvalidRefusal = apicode.Refusal{Condition: apicode.TokenScopesInvalid, Code: apicode.TokenScopesInput}

// markKinds are the kinds of mark (the route map's section (e)), in the order a list of every kind
// answers them.
var markKinds = []string{"issue", "agent", "document", "thread", "message", "ask"}

// markArms are the body of a mark's write by its kind: what PATCH /api/v2/me/marks/{kind}/{id}
// decodes by the path's {kind}, and each item of PATCH /api/v2/me/marks by its own kind.
var markArms = map[string]any{
	"issue":    issueMarkV2Input{},
	"agent":    agentMarkV2Input{},
	"document": documentMarkV2Input{},
	"thread":   threadMarkV2Input{},
	"message":  messageMarkV2Input{},
	"ask":      askMarkV2Input{},
}

// markText is what every mark route says of a mark and of its fields by kind.
const markText = "A mark is the caller's own state on one object, {kind, id, ...fields}, a field that is null left " +
	"out: issue (id an issue key) {pinned, last_read_seq, pinned_items, seq}, pinned_items the markers of the issue's " +
	"pinned items; agent (id a session) {cleared_before, read_through, unread_replies, pinned, pinned_at}, " +
	"unread_replies the session's replies to the caller's direct messages newer than both marks and not read by id; " +
	"document (id a document) {read_version, read_at}; thread (id an ask, or the root of a comment or an issue " +
	"message thread) {read_through, dismissed, dismissed_at, version}, a row only for a thread the caller wrote in; " +
	"message (id the root of a direct conversation) {dismissed, dismissed_at}; ask {snoozed_until}"

// markWriteText is what both mark writes say of the fields each kind takes and their rules.
const markWriteText = "issue {pinned?, last_read_seq?, pinned_items?, seq?, actor?}: each field replaces the stored " +
	"one (last_read_seq from 0 to 2147483647, the largest event position the store holds), and a seq not above the " +
	"stored one is 409 STATE_STALE with the stored mark as " +
	"state; every write appends user_state.updated. agent {cleared_before?, read_through?, read_replies?, pinned?}, " +
	"at least one: cleared_before replaces, read_through moves forward only, each RFC3339 and no more than a minute " +
	"ahead of the server, read_replies marks the session's own replies read by id, and pinned pins the session to the " +
	"caller's sidebar or unpins it. document {read_version}: replaces, from 0 to 2147483647 (the largest version " +
	"number the store holds). thread {read_through?, " +
	"dismissed?, version?}, at least one: read_through moves forward only, and dismissed true, which takes the version " +
	"GET /api/v2/me/threads answered for the thread, takes it out of the Inbox and marks it read only while that " +
	"version is still the thread's, so a dismissal buries no write nobody has read; dismissed false puts it back. " +
	"message {dismissed, newest_reply?, unread_replies?}: dismissed true, which takes the newest session reply and the " +
	"unread replies the page drew in the thread, takes it out of the Inbox only while those are still the thread's; " +
	"dismissed false puts it back. ask {snoozed_until}: an RFC3339 time in the future replaces the snooze, and null " +
	"un-snoozes. A field of the wrong JSON type is 400 INVALID_JSON, a value its field refuses 400 INVALID_STATE, and " +
	"a malformed snooze 400 SNOOZE_INPUT."

// v2MeRows are the caller's own routes: rows 189, 120 to 123, 126, 124 and
// 135, 131 and 132, 129, and 125, 127, 128, 130, 133 and 136 of the route map.
func (s *server) v2MeRows() []routeRow {
	person := routeScopeSet{model.ScopePerson}
	settles := routeScopeSet{model.ScopePerson, model.ScopeApprove}
	tokenIDs := func() *idRoute { return &idRoute{Reason: "a personal agent token id"} }
	markIDs := func() *idRoute {
		return &idRoute{Reason: "a mark's target by its kind: an issue key, a session, or the id of a document, a thread, a message or an ask"}
	}
	return []routeRow{
		{
			Method: http.MethodGet, Pattern: "/me", Auth: authAny, Versions: onlyV2,
			Description: "Who the server takes the caller for, one shape for every credential: {kind, person, session, " +
				"service, project, project_scope, scopes, cut, sign_in_required}. kind is user for a person at a browser and agent for every " +
				"other credential, a personal token holding the person scope included, since the kind names the " +
				"credential and not the act. person is the lowercase email of the person the credential acts for: the " +
				"person at the browser, a personal token's owner, or a broker-issued token's person, null for the shared " +
				"token and for a broker-issued token that names a service. session is the session the credential proved, " +
				"null for every credential but an access token the secrets broker issued; service is a verified " +
				"service-account token's Kubernetes subject, or the service a broker-issued token naming no person " +
				"names: the fleet of a machine nobody has signed in on, a read entry's service, the Kubernetes account " +
				"an exchange entry lists, or a job's identity as its job entry lists it, null for every other; project " +
				"is the one project key the credential lets the caller write, null where it names none, a job's token " +
				"whose entry names public among them. project_scope is the project scope of an access token the secrets " +
				"broker issued, null for every other credential: named for a token an operator's or a job's entry mapped " +
				"to its project, and for an exchange entry's token naming its account and no project, public for a job's " +
				"token that names no one project, person for a person's own token and for a machine's token that names " +
				"no person, and none for a read entry's token; a token narrowed to reading answers the word it narrows. " +
				"scopes is the " +
				"OAuth scopes the credential holds, [] for none: person acts as the person on every route the dashboard " +
				"reaches, and approve settles what a person settles. Read-only credentials list read after those scopes. " +
				"cut is their past read time in UTC, or null when there is no cut; this route reports the cut rather than " +
				"reading Dispatch at that time. A Legion daemon's own broker token answers [\"legion-status\"]: with " +
				"DISPATCH_LEGION_STATUS_SCOPE on, legion-status or person lets a credential make the three writes of a " +
				"Legion issue that PATCH /api/v2/issues/{key} refuses to every other credential, in both its forms, but " +
				"for the owning architect's status write: a broker-issued token whose session the daemon's own token " +
				"published as the architect of an issue above writes done, backlog, icebox or triage. sign_in_required " +
				"is true where Dispatch refuses " +
				"every write of the credential with a 403 until a person signs in on its machine, which " +
				"is a broker-issued token of a machine nobody has signed in on, and false for every other credential, " +
				"including a job's token, an exchange entry's token naming its account and every read-only credential.",
			Handler:  s.getMeV2,
			Resource: "me", Kind: kindSingleton,
			Shape:    routeShape{Status: http.StatusOK, Response: meV2{}},
			Refusals: []apicode.Refusal{undeclaredKeyRefusal},
		},
		{
			Method: http.MethodGet, Pattern: "/me/agent-tokens", Auth: authAny, Versions: onlyV2,
			Description: "The caller's personal agent tokens, newest first, each {id, name, prefix, scopes, created_at, " +
				"last_used_at, revoked_at}: a revoked token stays listed with its revoked_at; a read-only token lists read. Needs the person scope. " +
				"limit 1 to 200 (default 50) and offset; {items, total, limit, offset}.",
			Handler:  s.listAgentTokensV2,
			Resource: "agent-tokens", Kind: kindCollection, Key: "{id}",
			Query:       agentTokensV2Query{},
			Shape:       routeShape{Status: http.StatusOK, Response: page[model.AgentToken]{}},
			OAuthScopes: person,
			Refusals:    listRefusals(),
		},
		{
			Method: http.MethodPost, Pattern: "/me/agent-tokens", Auth: authAny, Versions: onlyV2,
			Description: "Mint a personal agent token {name, scopes?}: scopes is the set the new token holds, defaulting " +
				"to the scopes the caller's own credential holds, and [] is a token that holds none. A blank name is 400 " +
				"AGENT_TOKEN_INPUT. Read alone or with person mints a read-only token. An unknown scope, approve without " +
				"person, or read with approve is 400 TOKEN_SCOPES_INPUT. Except for read, a scope the caller does not " +
				"hold is 403 OAUTH_SCOPE_REQUIRED. Answers 201 the " +
				"token with its plaintext in token, the one time it is shown, so an Idempotency-Key never keeps the " +
				"answer: a repeat under the key is 409 KEY_COMMITTED and mints no second token. Needs the person and " +
				"approve scopes.",
			Handler:  s.createAgentTokenV2,
			Resource: "agent-tokens", Kind: kindCollection, Key: "{id}",
			Body:              requestBody{Kind: bodyJSON, Type: createAgentTokenInput{}},
			Shape:             routeShape{Status: http.StatusCreated, Response: createAgentTokenResponse{}},
			OAuthScopes:       settles,
			Scope:             scopeSession,
			AnswerHoldsSecret: true,
			Refusals: []apicode.Refusal{
				undeclaredKeyRefusal, tokenScopesInvalidRefusal,
				{Condition: apicode.AgentTokenNameBlank, Code: apicode.AgentTokenInput},
			},
		},
		{
			Method: http.MethodDelete, Pattern: "/me/agent-tokens/{id}", Auth: authAny, Versions: onlyV2,
			Description: "Revoke one of the caller's personal agent tokens: it is marked revoked, keeping its first " +
				"revoked_at and staying listed, and fails authentication with 401 TOKEN_REVOKED from then on. 204, a " +
				"token revoked already included. Needs the person and approve scopes. 404 AGENT_TOKEN_NOT_FOUND for an " +
				"id that is no token of the caller's, another person's among them.",
			Handler:  s.revokeAgentTokenV2,
			Resource: "agent-tokens", Kind: kindItem, Key: "{id}",
			Shape:       routeShape{Status: http.StatusNoContent},
			OAuthScopes: settles,
			Scope:       scopeSession,
			IDs:         tokenIDs(),
			Refusals:    []apicode.Refusal{undeclaredKeyRefusal, agentTokenMissingRefusal},
		},
		{
			Method: http.MethodPatch, Pattern: "/me/agent-tokens/{id}", Auth: authAny, Versions: onlyV2,
			Description: "Set the scopes of one of the caller's personal agent tokens {scopes}: the whole set it is to " +
				"hold, with the mint's refusals. A change to or from read only is always 400 TOKEN_SCOPES_INPUT; mint " +
				"a new token instead. Answers the token, with no plaintext. Needs the person and approve scopes. " +
				"404 AGENT_TOKEN_NOT_FOUND for an id that is no token of the caller's, another person's among them.",
			Handler:  s.updateAgentTokenV2,
			Resource: "agent-tokens", Kind: kindItem, Key: "{id}",
			Body:        requestBody{Kind: bodyJSON, Type: updateAgentTokenScopesInput{}},
			Shape:       routeShape{Status: http.StatusOK, Response: model.AgentToken{}},
			OAuthScopes: settles,
			Scope:       scopeSession,
			IDs:         tokenIDs(),
			Refusals:    []apicode.Refusal{undeclaredKeyRefusal, agentTokenMissingRefusal, tokenScopesInvalidRefusal},
		},
		{
			Method: http.MethodGet, Pattern: "/me/answers", Auth: authAny, Versions: onlyV2,
			Description: "The caller's own answers and replies on asks across issues and project documents, newest " +
				"first: each item is an answer (an ask.answered event's answer, and whether it is still the ask's current " +
				"one) or a reply, {kind, at, ask_id, ref, question, ask_kind, ask_state, edited_at, owner, answer?, " +
				"current, reply?}, with the ask's question, state, owner and deep link. Needs the person scope. limit 1 to " +
				"200 (default 50) and offset; {items, total, limit, offset}.",
			Handler:  s.listMyAnswersV2,
			Resource: "answers", Kind: kindCollection,
			Query:       myAnswersV2Query{},
			Shape:       routeShape{Status: http.StatusOK, Response: page[model.MyAnswerRow]{}},
			List:        listBounds{Default: myAnswersDefaultLimit, Max: myAnswersMaxLimit},
			OAuthScopes: person,
			Refusals:    listRefusals(),
		},
		{
			Method: http.MethodGet, Pattern: "/me/marks", Auth: authAny, Versions: onlyV2,
			Description: "The caller's stored marks, of the kind kind= names (issue, agent, document, thread, message " +
				"or ask; every kind where it names none), each of the ids id= names (repeatable; every id where it names " +
				"none), by kind in that order and then by id. " + markText + ". An agent mark is listed for every session " +
				"the caller has cleared, read, pinned or holds an unread reply from; a thread mark for every thread the " +
				"caller wrote in; a mark of any other kind once the caller has written it. Needs the person scope. limit 1 " +
				"to 200 (default 50) and offset; {items, total, " +
				"limit, offset}.",
			Handler:  s.listMarksV2,
			Resource: "marks", Kind: kindCollection, Key: "{kind}/{id}",
			Query:       markListV2Query{},
			Shape:       routeShape{Status: http.StatusOK, Response: page[markV2]{}},
			OAuthScopes: person,
			Refusals:    listRefusals(),
		},
		{
			Method: http.MethodPatch, Pattern: "/me/marks", Auth: authAny, Versions: onlyV2,
			Description: "Write several of the caller's marks at once, answering {updated}, how many it changed (0 for a " +
				"request that changes nothing, which appends no event). The body is either {items}, a named set, each " +
				"item {kind, id, ...fields} of one mark written as PATCH /api/v2/me/marks/{kind}/{id} writes it, every " +
				"target held to exist before anything is written, a kind that is none of the six 400 INVALID_KIND, and " +
				"every item written in one transaction, so a refusal of any item writes none of them; or {filter?, set}, " +
				"the Inbox's own " +
				"narrowing of GET /api/v2/me/threads - filter {kind (reply, agent; both by default), priority (P0, P1, P2, " +
				"P3, none), urgency (low, med, high, blocking, none), project (a key, none), issue or artifact}, each " +
				"absent list meaning every value - with set {read_through}, marking read every unread thread it narrows " +
				"to whose newest reply is at or before read_through, an agent reply marked read by id; {dismissed_through}, " +
				"taking out of the Inbox every thread it narrows to whose newest reply is at or before it; or {dismissed: " +
				"false, dismissed_through?}, putting back every dismissed thread it narrows to, dismissed at or before " +
				"dismissed_through where it is given. Each time is RFC3339 and no more than a minute ahead of the server. " +
				"Both forms at once, neither, a malformed filter, and items naming one mark twice are 400 INVALID_QUERY. " +
				markWriteText + " Needs the person scope, which a project-scoped session's token does not hold. A " +
				"credential that proved a session, a token the secrets broker issued, writes through items the agent " +
				"marks of that session alone: an agent item of another session is 403 SESSION_SCOPE, and no item is " +
				"written. The filter form names no session, so with kind agent it marks the agent replies of every " +
				"session, as POST /api/v1/me/inbox/read does.",
			Handler:  s.patchMarksV2,
			Resource: "marks", Kind: kindCollection, Key: "{kind}/{id}",
			Body: requestBody{Kind: bodyJSON, Type: marksPatchV2Input{}},
			Shape: routeShape{
				Status: http.StatusOK, Response: markUpdatesV2{},
				Refusals: []routeAnswer{{Status: http.StatusConflict, Body: markStaleV2{}}},
			},
			OAuthScopes: person,
			Scope:       scopeSession,
			Refusals:    markWriteRefusals(),
		},
		{
			Method: http.MethodGet, Pattern: "/me/marks/{kind}/{id}", Auth: authAny, Versions: onlyV2,
			Description: "The caller's mark of one kind on one object, its target's default state where the caller has " +
				"written none. " + markText + ". A kind that is none of the six is 400 INVALID_KIND. Needs the person " +
				"scope. The target must exist: 404 ISSUE_NOT_FOUND, ARTIFACT_NOT_FOUND, THREAD_NOT_FOUND (an id that is no " +
				"ask, root comment or root message), MESSAGE_NOT_FOUND or ASK_NOT_FOUND by kind; an agent mark's session " +
				"is not held to exist.",
			Handler:  s.getMarkV2,
			Resource: "marks", Kind: kindItem, Key: "{kind}/{id}",
			Shape:       routeShape{Status: http.StatusOK, Response: markV2{}},
			OAuthScopes: person,
			IDs:         markIDs(),
			Refusals:    markTargetRefusals(),
		},
		{
			Method: http.MethodPatch, Pattern: "/me/marks/{kind}/{id}", Auth: authAny, Versions: onlyV2,
			Description: "Write the caller's mark of one kind on one object, the fields the body names under that kind's " +
				"rules, and answer the mark. The body's fields are the path's kind's: " + markWriteText + " A kind that is " +
				"none of the six is 400 INVALID_KIND. Needs the person scope, which a project-scoped session's token does " +
				"not hold. A credential that proved a session, a token the secrets broker issued, writes the agent mark of " +
				"that session alone: an agent mark of another session is 403 SESSION_SCOPE. The target must exist: 404 " +
				"ISSUE_NOT_FOUND, ARTIFACT_NOT_FOUND, THREAD_NOT_FOUND, MESSAGE_NOT_FOUND or ASK_NOT_FOUND by kind; an " +
				"agent mark's session is not held to exist.",
			Handler:  s.patchMarkV2,
			Resource: "marks", Kind: kindItem, Key: "{kind}/{id}",
			Body: requestBody{Kind: bodyKindUnion, Arms: markArms},
			Shape: routeShape{
				Status: http.StatusOK, Response: markV2{},
				Refusals: []routeAnswer{{Status: http.StatusConflict, Body: markStaleV2{}}},
			},
			OAuthScopes: person,
			Scope:       scopeSession,
			IDs:         markIDs(),
			Refusals:    markWriteRefusals(),
		},
	}
}

// meV2 is GET /api/v2/me's answer: one shape for every credential, where GET /api/v1/whoami answers
// a person's browser whoamiUser and every other credential whoamiAgent.
type meV2 struct {
	// Kind is user for a person at a browser and agent for every other credential.
	Kind    string  `json:"kind" enum:"user,agent"`
	Person  *string `json:"person"`
	Session *string `json:"session"`
	Service *string `json:"service"`
	Project *string `json:"project"`
	// ProjectScope is a broker-issued token's project scope as the broker answered it, read as its
	// base word (model.Credential.ProjectScope), as GET /api/v1/whoami answers it; null for every
	// other credential, a person at a browser included.
	ProjectScope *string  `json:"project_scope"`
	Scopes       []string `json:"scopes"`
	Cut          *string  `json:"cut"`
	// SignInRequired is whether Dispatch refuses every write this credential makes until a person
	// signs in on its machine (403 SIGN_IN_REQUIRED), the decision writeNeedsASignedInPerson acts
	// on (signInRequired), as GET /api/v1/whoami answers it; false for a person at a browser.
	SignInRequired bool `json:"sign_in_required"`
}

// getMeV2 answers GET /api/v2/me: what GET /api/v1/whoami tells the caller, in one shape.
func (s *server) getMeV2(w http.ResponseWriter, r *http.Request) {
	actor, human, err := s.optionalActor(r)
	if err != nil {
		s.writeAuthenticationError(w, r, err)
		return
	}
	me := meV2{Kind: "agent", Project: actor.Project, Service: actor.Service, Scopes: whoamiScopes(actor.Credential)}
	switch {
	case human:
		me.Kind, me.Person, me.Project, me.Service = "user", new(actor.ID), nil, nil
	case actor.Owner != nil:
		me.Person = new(model.CanonicalLogin(*actor.Owner))
	}
	if !human && actor.Kind == "session" && actor.ID != "" {
		me.Session = new(actor.ID)
	}
	if actor.Credential != nil && !actor.Credential.Cut.IsZero() {
		me.Cut = new(cutText(actor.Credential.Cut))
	}
	if !human && actor.Credential != nil && actor.Credential.ProjectScope != "" {
		me.ProjectScope = new(actor.Credential.ProjectScope)
	}
	_, me.SignInRequired = s.signInRequired(r)
	WriteJSON(w, http.StatusOK, me)
}

// agentTokensV2Query is GET /api/v2/me/agent-tokens's query: one page.
type agentTokensV2Query struct {
	pageQuery
}

// listAgentTokensV2 answers GET /api/v2/me/agent-tokens: one page of the tokens GET
// /api/v1/me/agent-tokens lists whole (agentTokensOf).
func (s *server) listAgentTokensV2(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.actingPerson(w, r)
	if !ok {
		return
	}
	tokens, err := s.agentTokensOf(r.Context(), actor.ID)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, pageOf(tokens, v2Query[agentTokensV2Query](r).pageQuery))
}

// createAgentTokenV2 answers POST /api/v2/me/agent-tokens: the mint POST /api/v1/me/agent-tokens
// makes (createAgentTokenFrom).
func (s *server) createAgentTokenV2(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.actingPerson(w, r)
	if !ok {
		return
	}
	s.createAgentTokenFrom(w, r, actor, v2Body[createAgentTokenInput](r))
}

// requireAgentTokenV2 answers whether the token id names is one owner holds, revoked or not, and
// where it is not writes 404 AGENT_TOKEN_NOT_FOUND (requireHeldV2): another person's token is no
// token of the caller's.
func (s *server) requireAgentTokenV2(w http.ResponseWriter, r *http.Request, owner, id string) bool {
	return s.requireHeldV2(w, r, id, "agent token", `select exists (
		select 1 from agent_tokens where id = $1 and owner = $2
		union all
		select 1 from agent_read_tokens where id = $1 and owner = $2
	)`,
		apicode.AgentTokenNotFound, "you hold no agent token "+id, owner)
}

// revokeAgentTokenV2 answers DELETE /api/v2/me/agent-tokens/{id}: the revoke DELETE
// /api/v1/me/agent-tokens/{id} makes (revokeAgentTokenOf), the token held to be the caller's first.
func (s *server) revokeAgentTokenV2(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.actingPerson(w, r)
	if !ok || !s.requireAgentTokenV2(w, r, actor.ID, r.PathValue("id")) {
		return
	}
	revoked, err := s.revokeAgentTokenOf(r.Context(), r.PathValue("id"), actor.ID)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	if !revoked {
		writeError(w, r, apicode.AgentTokenNotFound, "you hold no agent token "+r.PathValue("id"))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// updateAgentTokenV2 answers PATCH /api/v2/me/agent-tokens/{id}: the scopes PATCH
// /api/v1/me/agent-tokens/{id} sets (updateAgentTokenScopesFrom), the token held to be the
// caller's first.
func (s *server) updateAgentTokenV2(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.actingPerson(w, r)
	if !ok || !s.requireAgentTokenV2(w, r, actor.ID, r.PathValue("id")) {
		return
	}
	s.updateAgentTokenScopesFrom(w, r, actor, v2Body[updateAgentTokenScopesInput](r))
}

// myAnswersV2Query is GET /api/v2/me/answers's query: one page.
type myAnswersV2Query struct {
	pageQuery
}

// listMyAnswersV2 answers GET /api/v2/me/answers: the page GET /api/v1/me/answers answers
// (readMyAnswers), in the envelope. A page past the end holds no row to carry the list's total,
// so the total is read from the list's first row, which every list but an empty one has.
func (s *server) listMyAnswersV2(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.actingPerson(w, r)
	if !ok {
		return
	}
	query := v2Query[myAnswersV2Query](r).pageQuery
	answers, err := s.readMyAnswers(r, caller, query.Limit, query.Offset)
	if err == nil && len(answers.Rows) == 0 && query.Offset > 0 {
		var first model.MyAnswersResponse
		first, err = s.readMyAnswers(r, caller, 1, 0)
		answers.Total = first.Total
	}
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, newPage(answers.Rows, answers.Total, query))
}

// markV2 is one mark of the caller's (the route map's section (e)): its kind, its target's id, and
// the fields of its kind, each left out where its kind has none or it is null. It is one type for
// every kind, so a list of every kind answers one item type; markText says which fields each kind
// carries.
type markV2 struct {
	Kind string `json:"kind" enum:"issue,agent,document,thread,message,ask"`
	ID   string `json:"id"`
	// Pinned is an issue's pin or a session's place in the caller's sidebar.
	Pinned *bool `json:"pinned,omitempty"`
	// LastReadSeq, PinnedItems and Seq are an issue mark's read mark, the markers of its pinned
	// items (PUT /api/v1/me/issues/{key}/state's dismissed) and its write guard.
	LastReadSeq *int      `json:"last_read_seq,omitempty"`
	PinnedItems *[]string `json:"pinned_items,omitempty"`
	Seq         *int64    `json:"seq,omitempty"`
	// ClearedBefore, UnreadReplies and PinnedAt are an agent mark's Clear, its unread replies and
	// when the session was pinned.
	ClearedBefore *string `json:"cleared_before,omitempty"`
	UnreadReplies *int    `json:"unread_replies,omitempty"`
	PinnedAt      *string `json:"pinned_at,omitempty"`
	// ReadThrough is an agent mark's or a thread mark's read mark.
	ReadThrough *string `json:"read_through,omitempty"`
	// ReadVersion and ReadAt are a document mark's read version and when it was read.
	ReadVersion *int    `json:"read_version,omitempty"`
	ReadAt      *string `json:"read_at,omitempty"`
	// Dismissed and DismissedAt are a thread mark's or a message mark's dismissal from the Inbox,
	// and Version the thread's version, which a thread's dismissal names.
	Dismissed   *bool   `json:"dismissed,omitempty"`
	DismissedAt *string `json:"dismissed_at,omitempty"`
	Version     *int64  `json:"version,omitempty"`
	// SnoozedUntil is an ask mark's snooze.
	SnoozedUntil *string `json:"snoozed_until,omitempty"`
}

// markStaleV2 is the 409 STATE_STALE body of an issue mark whose seq is not above the stored one:
// the stored mark as state, as PUT /api/v1/me/issues/{key}/state answers the stored state.
type markStaleV2 struct {
	Code  string `json:"code"`
	Error string `json:"error"`
	State markV2 `json:"state"`
}

// markUpdatesV2 is PATCH /api/v2/me/marks's answer: how many marks it changed.
type markUpdatesV2 struct {
	Updated int `json:"updated"`
}

// The arms of a mark's write, one per kind (markArms). The issue arm takes PUT
// /api/v1/me/issues/{key}/state's body with dismissed named pinned_items, which is what it holds;
// the agent arm is PUT /api/v1/me/agents/{session_id}/state's; the document arm PUT
// /api/v1/me/documents/{id}/read's with version named read_version; the thread and message arms
// PUT /api/v1/me/threads/{id}/read's and POST /api/v1/me/inbox/dismiss's for one row; and the ask
// arm PUT and DELETE /api/v1/me/asks/{id}/snooze's.
type (
	issueMarkV2Input struct {
		Pinned      *bool     `json:"pinned"`
		LastReadSeq *int      `json:"last_read_seq"`
		PinnedItems *[]string `json:"pinned_items"`
		Seq         *int64    `json:"seq"`
		// Actor names the acting session on the event the write appends (actSessionFromBody).
		Actor *model.Actor `json:"actor"`
	}
	agentMarkV2Input    userAgentStateInput
	documentMarkV2Input struct {
		ReadVersion *int `json:"read_version"`
	}
	threadMarkV2Input struct {
		ReadThrough *string `json:"read_through"`
		Dismissed   *bool   `json:"dismissed"`
		// Version is the thread's version as GET /api/v2/me/threads answered it, which a
		// dismissal is written under.
		Version *int64 `json:"version"`
	}
	messageMarkV2Input struct {
		Dismissed *bool `json:"dismissed"`
		// NewestReply and UnreadReplies are what the page drew in the thread: its newest session
		// reply and the ids it drew as unread, which a dismissal is held to.
		NewestReply   *string   `json:"newest_reply"`
		UnreadReplies *[]string `json:"unread_replies"`
	}
	askMarkV2Input struct {
		// SnoozedUntil is an RFC3339 time, or null to un-snooze; it stays raw so the two can be
		// told from a body that leaves it out.
		SnoozedUntil json.RawMessage `json:"snoozed_until"`
	}
)

// sessionsAddressed is the session an agent mark's write goes into: the path's {id}, which is a
// session for this kind alone (sessionAddressing).
func (agentMarkV2Input) sessionsAddressed(r *http.Request) []string {
	return []string{r.PathValue("id")}
}

// marksPatchV2Input is PATCH /api/v2/me/marks's body: the named items, or the Inbox's narrowing
// and what to set on the threads it narrows to, never both.
//
// An agent mark names a session, in the path's {id} or in an item's, and gateV2Authorities holds
// each to the session the caller's credential proved (sessionAddressing). Both marks writes need
// the person scope, so a broker token that lacks it is refused by the route's own gate
// (gateRouteScopes) before any session rule; one that holds it marks, through the path or the
// items, its own session's agent conversation and is refused 403 SESSION_SCOPE for any other's,
// though the row declares OAuth scopes (refusesSessionOtherThanProved, which exempts no such row
// as refusesAnotherSession does). A credential that proves no session, a browser cookie or a
// personal token, marks the person's agent conversation with any session. The filter form names
// no session, and sessionsAddressed names only the items, so a filter of kind agent
// (`{"filter": {"kind": ["agent"]}, "set": {...}}`, patchMarksByFilter) marks the agent replies of
// every session, even where a broker token of one session sends it, as /api/v1's POST
// /me/inbox/read and POST /me/inbox/dismiss do.
type marksPatchV2Input struct {
	Items  []markItemV2        `json:"items"`
	Filter *inboxThreadFilters `json:"filter"`
	Set    *markSetV2          `json:"set"`
}

// sessionsAddressed is the session of each agent item: the item's id, which a broker token's
// session is held to as an agent mark's path is (sessionAddressing).
func (body marksPatchV2Input) sessionsAddressed(*http.Request) []string {
	var sessions []string
	for _, item := range body.Items {
		if item.Kind == "agent" {
			sessions = append(sessions, item.ID)
		}
	}
	return sessions
}

// markItemV2 is one item of PATCH /api/v2/me/marks: a mark's kind and id, and the members of its
// write, which are its kind's arm (markArms) and are read in it by the handler (markItemWrite),
// as the decoder reads a mark's body by the path's {kind}. The document declares it as the union
// of those arms (itemUnions).
type markItemV2 struct {
	Kind    string `json:"kind" required:"true" enum:"issue,agent,document,thread,message,ask"`
	ID      string `json:"id" required:"true"`
	members json.RawMessage
}

func (item *markItemV2) UnmarshalJSON(data []byte) error {
	var head struct {
		Kind string `json:"kind"`
		ID   string `json:"id"`
	}
	if err := json.Unmarshal(data, &head); err != nil {
		return err
	}
	item.Kind, item.ID, item.members = head.Kind, head.ID, slices.Clone(data)
	return nil
}

// markSetV2 is what PATCH /api/v2/me/marks sets on the threads its filter narrows to: a read mark,
// a dismissal through a time, or a restore.
type markSetV2 struct {
	ReadThrough      *string `json:"read_through"`
	DismissedThrough *string `json:"dismissed_through"`
	Dismissed        *bool   `json:"dismissed"`
}

// markListV2Query is GET /api/v2/me/marks's query.
type markListV2Query struct {
	pageQuery
	// Kind keeps the marks of one kind; none is every kind.
	Kind *string `query:"kind" enum:"issue,agent,document,thread,message,ask"`
	// ID keeps the marks on these targets.
	ID []string `query:"id"`
}

// markTargetID is the spelling a mark's target id is matched in: the canonical one of a uuid, so
// a document, thread, message or ask named in another of uuid.Parse's spellings is the one it
// names, and the id as written otherwise (an issue key, a session).
func markTargetID(kind, id string) string {
	if kind == "issue" || kind == "agent" {
		return id
	}
	if parsed, err := uuid.Parse(id); err == nil {
		return parsed.String()
	}
	return id
}

// requireThreadV2 answers whether the id names a thread - an ask, or the root of a comment thread
// or a message thread, each a thread by its root's id (user_threads) - and where it does not
// writes 404 THREAD_NOT_FOUND (requireHeldV2).
func (s *server) requireThreadV2(w http.ResponseWriter, r *http.Request, id string) bool {
	return s.requireHeldV2(w, r, id, "thread", `
		select exists (select 1 from asks where id = $1)
		    or exists (select 1 from comments where id = $1 and ask_id is null and reply_to is null)
		    or exists (select 1 from messages where id = $1 and in_reply_to is null)
	`, apicode.ThreadNotFound, "no ask, root comment or root message has the id "+id)
}

// requireMarkTarget answers whether a mark's target exists, and where it does not writes its
// kind's 404 (dispatch://INFRA-4071/spec R7), or 400 INVALID_KIND for a kind that is none of the
// six. An agent mark's target is a session, which keeps the answer /api/v1 gives it (R7): it is
// not held to exist.
func (s *server) requireMarkTarget(w http.ResponseWriter, r *http.Request, kind, id string) bool {
	switch kind {
	case "issue":
		return s.requireIssueV2(w, r, id)
	case "agent":
		return true
	case "document":
		return s.requireArtifactV2(w, r, id)
	case "thread":
		return s.requireThreadV2(w, r, id)
	case "message":
		return s.requireMessageV2(w, r, id)
	case "ask":
		return s.requireAskV2(w, r, id)
	}
	writeError(w, r, apicode.InvalidKind, "kind must be one of "+strings.Join(slices.Sorted(maps.Keys(markArms)), ", "))
	return false
}

// issueMark is the issue mark of state.
func issueMark(key string, state userIssueState) markV2 {
	items := state.Dismissed
	if items == nil {
		items = []string{}
	}
	return markV2{
		Kind: "issue", ID: key, Pinned: new(state.Pinned), LastReadSeq: new(state.LastReadSeq), PinnedItems: &items,
		Seq: new(state.Seq),
	}
}

// agentMark is the agent mark of state, a session's zero state where the caller holds none.
func agentMark(session string, state userAgentState) markV2 {
	return markV2{
		Kind: "agent", ID: session, ClearedBefore: state.ClearedBefore, ReadThrough: state.ReadThrough,
		UnreadReplies: new(state.UnreadReplies), Pinned: new(state.PinnedAt != nil), PinnedAt: state.PinnedAt,
	}
}

// defaultMark is a mark of kind on id the caller has not written: its target's default state.
func defaultMark(kind, id string) markV2 {
	switch kind {
	case "issue":
		return issueMark(id, userIssueState{})
	case "agent":
		return agentMark(id, userAgentState{})
	case "thread", "message":
		return markV2{Kind: kind, ID: id, Dismissed: new(false)}
	}
	return markV2{Kind: kind, ID: id}
}

// The caller's stored marks of each kind but agent's, each read from the kind's own table: $1 the
// caller's login, and $2 the one target to read, every target where it is null, by target id.
const (
	issueMarksQuery = `select issue_key, pinned, last_read_seq, dismissed, seq from user_issue_state
		where login = $1 and ($2::text is null or issue_key = $2) order by issue_key`
	documentMarksQuery = `select artifact_id::text, version, read_at from user_document_reads
		where login = $1 and ($2::text is null or artifact_id::text = $2) order by artifact_id`
	threadMarksQuery = `select thread_id::text, read_through, dismissed_at, version from user_threads
		where login = $1 and ($2::text is null or thread_id::text = $2) order by thread_id`
	messageMarksQuery = `select message_id::text, dismissed_at from user_agent_thread_dismissed
		where login = $1 and ($2::text is null or message_id::text = $2) order by message_id`
	askMarksQuery = `select ask_id::text, snoozed_until from user_ask_snooze
		where login = $1 and ($2::text is null or ask_id::text = $2) order by ask_id`
)

// marksOf is the caller's stored marks of kind, on the one target only names or on every target
// where it is nil, by target id. Each kind's marks are keyed on the login its /api/v1 routes key
// them on: the issue state and the agent state on the actor's id, which actingPerson has already
// made the canonical login, and the rest on the canonical login.
func (s *server) marksOf(ctx context.Context, actor model.Actor, kind string, only *string) ([]markV2, error) {
	if kind == "agent" {
		states, err := s.loadUserAgentStates(ctx, actor.ID, only)
		if err != nil {
			return nil, err
		}
		marks := make([]markV2, 0, len(states))
		for _, session := range slices.Sorted(maps.Keys(states)) {
			marks = append(marks, agentMark(session, states[session]))
		}
		return marks, nil
	}
	login := model.CanonicalLogin(actor.ID)
	var rows pgx.Rows
	var err error
	switch kind {
	case "issue":
		rows, err = s.deps.Store.Pool.Query(ctx, issueMarksQuery, actor.ID, only)
	case "document":
		rows, err = s.deps.Store.Pool.Query(ctx, documentMarksQuery, login, only)
	case "thread":
		rows, err = s.deps.Store.Pool.Query(ctx, threadMarksQuery, login, only)
	case "message":
		rows, err = s.deps.Store.Pool.Query(ctx, messageMarksQuery, login, only)
	case "ask":
		rows, err = s.deps.Store.Pool.Query(ctx, askMarksQuery, login, only)
	default:
		return nil, fmt.Errorf("no mark has the kind %q", kind)
	}
	if err != nil {
		return nil, fmt.Errorf("read the %s marks: %w", kind, err)
	}
	marks, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (markV2, error) { return scanMark(kind, row) })
	if err != nil {
		return nil, fmt.Errorf("read the %s marks: %w", kind, err)
	}
	return marks, nil
}

// scanMark reads one row of the statement of kind's marks.
func scanMark(kind string, row pgx.CollectableRow) (markV2, error) {
	mark := markV2{Kind: kind}
	switch kind {
	case "issue":
		var state userIssueState
		var pinnedItems []byte
		if err := row.Scan(&mark.ID, &state.Pinned, &state.LastReadSeq, &pinnedItems, &state.Seq); err != nil {
			return markV2{}, err
		}
		if err := json.Unmarshal(pinnedItems, &state.Dismissed); err != nil {
			return markV2{}, err
		}
		return issueMark(mark.ID, state), nil
	case "document":
		var version int
		var at time.Time
		if err := row.Scan(&mark.ID, &version, &at); err != nil {
			return markV2{}, err
		}
		mark.ReadVersion, mark.ReadAt = &version, new(timestampValue(at))
	case "thread":
		var readThrough time.Time
		var dismissedAt *time.Time
		var version int64
		if err := row.Scan(&mark.ID, &readThrough, &dismissedAt, &version); err != nil {
			return markV2{}, err
		}
		mark.ReadThrough, mark.Dismissed, mark.DismissedAt = new(timestampValue(readThrough)), new(dismissedAt != nil), timestampPtr(dismissedAt)
		mark.Version = &version
	case "message":
		var dismissedAt time.Time
		if err := row.Scan(&mark.ID, &dismissedAt); err != nil {
			return markV2{}, err
		}
		mark.Dismissed, mark.DismissedAt = new(true), new(timestampValue(dismissedAt))
	case "ask":
		var snoozedUntil time.Time
		if err := row.Scan(&mark.ID, &snoozedUntil); err != nil {
			return markV2{}, err
		}
		mark.SnoozedUntil = new(timestampValue(snoozedUntil))
	}
	return mark, nil
}

// markOf is the caller's mark of kind on id: the stored one, or its target's default state.
func (s *server) markOf(ctx context.Context, actor model.Actor, kind, id string) (markV2, error) {
	marks, err := s.marksOf(ctx, actor, kind, &id)
	switch {
	case err != nil:
		return markV2{}, err
	case len(marks) == 0:
		return defaultMark(kind, id), nil
	}
	return marks[0], nil
}

// listMarksV2 answers GET /api/v2/me/marks: one page of the caller's stored marks of the kind kind=
// names, or of every kind, on the targets id= names, or on every target, by kind and then by id.
// It reads what GET /api/v1/me/state and GET /api/v1/me/agents/state read, and each other kind's
// table the same way.
func (s *server) listMarksV2(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.actingPerson(w, r)
	if !ok {
		return
	}
	query := v2Query[markListV2Query](r)
	kinds := markKinds
	if query.Kind != nil {
		kinds = []string{*query.Kind}
	}
	var marks []markV2
	for _, kind := range kinds {
		wanted := map[string]bool{}
		for _, id := range query.ID {
			wanted[markTargetID(kind, id)] = true
		}
		of, err := s.marksOf(r.Context(), actor, kind, nil)
		if err != nil {
			s.writeHandlerError(w, r, err)
			return
		}
		for _, mark := range of {
			if len(wanted) == 0 || wanted[mark.ID] {
				marks = append(marks, mark)
			}
		}
	}
	WriteJSON(w, http.StatusOK, pageOf(marks, query.pageQuery))
}

// getMarkV2 answers GET /api/v2/me/marks/{kind}/{id}: the caller's mark of one kind on one object,
// the target held to exist first, as GET /api/v1/me/documents/{id}/read answered a document's.
func (s *server) getMarkV2(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.actingPerson(w, r)
	if !ok {
		return
	}
	kind, id := r.PathValue("kind"), r.PathValue("id")
	if !s.requireMarkTarget(w, r, kind, id) {
		return
	}
	mark, err := s.markOf(r.Context(), actor, kind, id)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, mark)
}

// patchMarkV2 answers PATCH /api/v2/me/marks/{kind}/{id}: the write of the /api/v1 route of the
// path's kind (writeMarks), answered with the mark as it then stands.
func (s *server) patchMarkV2(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.actingPerson(w, r)
	if !ok {
		return
	}
	kind, id := r.PathValue("kind"), r.PathValue("id")
	if _, ok := s.writeMarks(w, r, actor, []markWrite{{kind: kind, id: id, arm: v2BodyArm(r)}}); !ok {
		return
	}
	mark, err := s.markOf(r.Context(), actor, kind, id)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, mark)
}

// patchMarksV2 answers PATCH /api/v2/me/marks: the named items, each written as its kind's PATCH
// /api/v2/me/marks/{kind}/{id} writes it (writeMarks), or the Inbox's narrowing with what to set,
// written as POST /api/v1/me/inbox/read and POST /api/v1/me/inbox/dismiss write their narrowing
// (patchMarksByFilter).
func (s *server) patchMarksV2(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.actingPerson(w, r)
	if !ok {
		return
	}
	body := v2Body[marksPatchV2Input](r)
	var updated int
	switch {
	case len(body.Items) > 0 && (body.Filter != nil || body.Set != nil):
		writeError(w, r, apicode.InvalidQuery, "name items, or a filter with set, not both")
		return
	case len(body.Items) > 0:
		writes := make([]markWrite, 0, len(body.Items))
		named := map[string]bool{}
		for _, item := range body.Items {
			write, err := markItemWrite(item)
			if err != nil {
				s.writeHandlerError(w, r, err)
				return
			}
			if named[write.kind+"/"+write.id] {
				writeError(w, r, apicode.InvalidQuery, fmt.Sprintf("items names the %s mark on %s twice", write.kind, write.id))
				return
			}
			named[write.kind+"/"+write.id] = true
			writes = append(writes, write)
		}
		if updated, ok = s.writeMarks(w, r, actor, writes); !ok {
			return
		}
	case body.Set == nil:
		writeError(w, r, apicode.InvalidQuery, "name items, or a filter with set: set is what the narrowing's threads take")
		return
	default:
		if updated, ok = s.patchMarksByFilter(w, r, actor, body.Filter, *body.Set); !ok {
			return
		}
	}
	WriteJSON(w, http.StatusOK, markUpdatesV2{Updated: updated})
}

// markWrite is one mark a request writes: its kind, its target's id and its kind's arm, decoded.
type markWrite struct {
	kind, id string
	arm      any
}

// markItemWrite reads one item of PATCH /api/v2/me/marks in its kind's arm, as the decoder reads a
// mark's body by the path's {kind}: a kind that is none of the six is 400 INVALID_KIND naming them,
// and a member the arm does not declare is ignored.
func markItemWrite(item markItemV2) (markWrite, error) {
	arm, known := markArms[item.Kind]
	if !known {
		return markWrite{}, errorf(apicode.InvalidKind, "kind must be one of %s", strings.Join(slices.Sorted(maps.Keys(markArms)), ", "))
	}
	value := reflect.New(reflect.TypeOf(arm))
	if err := json.Unmarshal(item.members, value.Interface()); err != nil {
		return markWrite{}, errorf(apicode.InvalidJSON, "the %s mark on %s: %v", item.Kind, item.ID, err)
	}
	return markWrite{kind: item.Kind, id: markTargetID(item.Kind, item.ID), arm: value.Elem().Interface()}, nil
}

// markStep is one write of a mark that is no dismissal, run in the transaction of every write of its
// request once each has been read: it answers how many marks it changed and the events, not yet
// appended, that announce them.
type markStep func(ctx context.Context, tx pgx.Tx) (int, []model.Event, error)

// markEvents is the one event of a step that announces one, or none.
func markEvents(event *model.Event) (int, []model.Event) {
	if event == nil {
		return 0, nil
	}
	return 1, []model.Event{*event}
}

// markDismissals are the thread and message dismissals and restores one request writes, each
// named row with what the page drew in it: one dismissal and one restore, each written as POST
// /api/v1/me/inbox/dismiss writes the rows it names (dismissInboxThreadsIn); and the threads whose
// read mark it moves (moveThreadReadIn), whose owners and rows are taken with the dismissals'
// (lockMarkThreads).
type markDismissals struct {
	dismiss, restore inboxDismissRows
	read             []string
}

// namedRowsScope is the narrowing a dismissal that names its rows carries, which it never reads
// (readInboxDismissThreads), as dismissInbox passes it.
var namedRowsScope = inboxReadScope{reply: true, agent: true, priorityNone: true, projectNone: true, urgencyNone: true}

// markStale is an issue mark's write refused for its seq (userStateStale), with the stored mark.
type markStale struct {
	mark markV2
}

func (stale *markStale) Error() string { return (&userStateStale{}).Error() }

// writeMarks writes the marks writes name, under each kind's rules, and answers how many it
// changed, or writes the refusal and answers false: every target is held to exist and every write
// read before anything is written, and then every write runs in one transaction (writeMarksIn), so
// a refusal of any of them - one the database makes among them, a reply id that is no message of
// the session or a snooze already past by the database's clock - leaves every mark as it was.
func (s *server) writeMarks(w http.ResponseWriter, r *http.Request, actor model.Actor, writes []markWrite) (int, bool) {
	for _, write := range writes {
		if !s.requireMarkTarget(w, r, write.kind, write.id) {
			return 0, false
		}
	}
	dismissals := markDismissals{
		dismiss: inboxDismissRows{seenVersions: map[string]int64{}, seenNewest: map[string]string{}},
	}
	var steps []markStep
	for _, write := range writes {
		step, ok := s.planMarkWrite(w, r, actor, write, &dismissals)
		if !ok {
			return 0, false
		}
		if step != nil {
			steps = append(steps, step)
		}
	}
	updated, events, err := s.writeMarksIn(r.Context(), actor, steps, dismissals)
	var stale *markStale
	if errors.As(err, &stale) {
		code, status := refusalPair(r, apicode.StateStale)
		WriteJSON(w, status, markStaleV2{Code: code, Error: stale.Error(), State: stale.mark})
		return 0, false
	}
	if err != nil {
		s.writeHandlerError(w, r, err)
		return 0, false
	}
	s.publish(r.Context(), events...)
	return updated, true
}

// writeMarksIn is writeMarks' one transaction, which answers how many marks changed and the events
// appended, to publish; any error rolls back everything before it. It takes its locks in one order
// before any write, so no two of a person's mark writes can each hold a lock the other waits on:
//
//  1. the caller's marks lock, alone (lockPersonMarks, marksAlone), first, before anything else.
//     Every row of a mark is the caller's own - user_issue_state, user_agent_state,
//     user_agent_read, user_agent_pin, user_agent_reply_read, user_document_reads,
//     user_ask_snooze, user_threads and user_agent_thread_dismissed are each keyed on the login -
//     and the kinds other than thread and message take their rows in item order, which no lock
//     taken up front can order: a row of user_issue_state or user_ask_snooze may not exist before
//     its write inserts it. So this write runs before or after every other of the same person's
//     that writes more than one mark row - another of these, a mark-all, a dismissal, a
//     conversation's state - and waits holding nothing. Two people's writes never take one
//     another's rows; what they share is taken in a level that does not conflict with itself (an
//     owner `for share`, a foreign key's `for key share`) or in one sorted order (the projects,
//     below), so neither waits on the other in a cycle.
//  2. then, before any item is written, steps 1 to 3 of the Inbox's lock order
//     (markInboxReadThreads) over every thread the request names - the owner of each, `for share`,
//     in owner-key order; the root of each Agent thread it dismisses, `for update`, in id order; and
//     the caller's user_threads row of each, `for no key update`, in thread-id order - which is the
//     order a mark-all, a dismissal, a one-thread read mark and a session's reply take them in, so
//     this transaction takes them as every other writer of the Inbox does (lockMarkThreads).
//  3. the dismissals and the restores, which take the order's steps 4 and 5 and re-take what step
//     2 took; then each item write that is no dismissal, in order, over the caller's own rows.
//  4. the project of every event, in one sorted order (LockOwners), after every row and before the
//     first event is appended: the order's step 6, which every one of these writers takes last.
func (s *server) writeMarksIn(ctx context.Context, actor model.Actor, steps []markStep, dismissals markDismissals) (int, []model.Event, error) {
	tx, err := s.begin(ctx)
	if err != nil {
		return 0, nil, err
	}
	defer tx.Rollback(ctx)
	// The transaction holds owner rows `for share` while its writes run, as the dismissal's own
	// does, so it is held to the same bound (boundInboxStatements).
	if _, err := tx.Exec(ctx, s.boundInboxStatements()); err != nil {
		return 0, nil, fmt.Errorf("bound the marks' statements: %w", err)
	}
	login := model.CanonicalLogin(actor.ID)
	if err := lockPersonMarks(ctx, tx, login, marksAlone); err != nil {
		return 0, nil, err
	}
	for _, rows := range []*inboxDismissRows{&dismissals.dismiss, &dismissals.restore} {
		slices.Sort(rows.threads)
		slices.Sort(rows.roots)
		slices.Sort(rows.seenUnread)
		rows.seenUnread = slices.Compact(rows.seenUnread)
	}
	threads := slices.Concat(dismissals.read, dismissals.dismiss.threads, dismissals.restore.threads)
	slices.Sort(threads)
	if err := lockMarkThreads(ctx, tx, actor, login, slices.Compact(threads), dismissals.dismiss.roots); err != nil {
		return 0, nil, err
	}
	updated := 0
	var events []model.Event
	for _, pass := range []struct {
		rows    inboxDismissRows
		restore bool
	}{{dismissals.dismiss, false}, {dismissals.restore, true}} {
		if !pass.rows.named() {
			continue
		}
		changed, announced, err := s.dismissInboxThreadsIn(ctx, tx, actor,
			inboxDismissPlanOf(inboxDismissRequest{Restore: pass.restore}), pass.rows, namedRowsScope)
		if err != nil {
			return 0, nil, err
		}
		updated += changed
		events = append(events, announced...)
	}
	for _, step := range steps {
		changed, announced, err := step(ctx, tx)
		if err != nil {
			return 0, nil, err
		}
		updated += changed
		events = append(events, announced...)
	}
	appended := make([]model.Event, 0, len(events))
	if len(events) > 0 {
		if err := s.deps.Events.LockOwners(ctx, tx, events...); err != nil {
			return 0, nil, fmt.Errorf("lock the changed marks' projects: %w", err)
		}
		for _, event := range events {
			written, err := s.appendEvent(ctx, tx, event)
			if err != nil {
				return 0, nil, err
			}
			appended = append(appended, written)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, nil, err
	}
	return updated, appended, nil
}

// lockMarkThreads takes steps 1 to 3 of the Inbox's lock order (markInboxReadThreads) over the
// threads a request of the caller's marks names, sorted, and over the Agent threads it dismisses,
// sorted: each thread's owner `for share`, documents then issues (lockInboxReadOwners); the root
// of each Agent thread still in the caller's Inbox `for update` (inboxDismissLockAgentRoots), read
// as the dismissal reads its roots so that no root of another person's is ever taken; and the
// caller's user_threads row of each thread `for no key update` (inboxReadLockThreads). A thread the
// caller has no row for takes nothing, as it is written nothing.
func lockMarkThreads(ctx context.Context, tx pgx.Tx, actor model.Actor, login string, threads, roots []string) error {
	read := newInboxReadThreads()
	if len(threads) > 0 {
		if err := readInboxReplyRows(ctx, tx, &read, inboxDismissNamedThreads, login, threads); err != nil {
			return err
		}
	}
	if len(roots) > 0 {
		if err := readInboxAgentRows(ctx, tx, &read, inboxDismissAgentByID, append(unreadDirectRepliesArgs(actor.ID, nil), roots)); err != nil {
			return err
		}
	}
	if err := lockInboxReadOwners(ctx, tx, read); err != nil {
		return err
	}
	if len(read.agentRoots) > 0 {
		if _, err := tx.Exec(ctx, inboxDismissLockAgentRoots, read.agentRoots); err != nil {
			return fmt.Errorf("lock the agent threads' roots: %w", err)
		}
	}
	if len(read.threads) > 0 {
		if _, err := tx.Exec(ctx, inboxReadLockThreads, login, read.threads); err != nil {
			return fmt.Errorf("lock the caller's threads: %w", err)
		}
	}
	return nil
}

// planMarkWrite reads one mark's write against its kind's rules, writing the refusal of one that
// breaks them, and answers the step that makes it, nil where all it makes is a dismissal or a
// restore, which it adds to dismissals.
func (s *server) planMarkWrite(w http.ResponseWriter, r *http.Request, actor model.Actor, write markWrite, dismissals *markDismissals) (markStep, bool) {
	switch input := write.arm.(type) {
	case issueMarkV2Input:
		acting, ok := actSessionFromBody(w, r, actor, input.Actor)
		if !ok {
			return nil, false
		}
		if err := lastReadSeqRefusal(input.LastReadSeq); err != nil {
			s.writeHandlerError(w, r, err)
			return nil, false
		}
		state := userIssueStateInput{Pinned: input.Pinned, LastReadSeq: input.LastReadSeq, Dismissed: input.PinnedItems, Seq: input.Seq}
		return func(ctx context.Context, tx pgx.Tx) (int, []model.Event, error) {
			_, event, err := s.writeUserIssueStateIn(ctx, tx, acting, write.id, state)
			var stale *userStateStale
			if errors.As(err, &stale) {
				return 0, nil, &markStale{mark: issueMark(write.id, stale.state)}
			}
			if err != nil {
				return 0, nil, err
			}
			return 1, []model.Event{event}, nil
		}, true
	case agentMarkV2Input:
		parsed, err := parseUserAgentStateInput(write.id, userAgentStateInput(input))
		if err != nil {
			s.writeHandlerError(w, r, err)
			return nil, false
		}
		return func(ctx context.Context, tx pgx.Tx) (int, []model.Event, error) {
			announced, err := s.writeUserAgentStateIn(ctx, tx, actor, write.id, parsed)
			if err != nil || !announced {
				return 0, nil, err
			}
			return 1, []model.Event{userAgentStateEvent(actor, write.id)}, nil
		}, true
	case documentMarkV2Input:
		// read_version is the column PUT /api/v1/me/documents/{id}/read writes
		// (user_document_reads.version, an integer column: fitsIntegerColumn), so a version past it
		// is refused before the transaction makes a query with it.
		if input.ReadVersion == nil || *input.ReadVersion < 0 || !fitsIntegerColumn(*input.ReadVersion) {
			writeError(w, r, apicode.InvalidState, fmt.Sprintf("read_version must be a whole number from 0 to %d", maxIntegerColumn))
			return nil, false
		}
		return func(ctx context.Context, tx pgx.Tx) (int, []model.Event, error) {
			_, err := writeDocumentRead(ctx, tx, model.CanonicalLogin(actor.ID), write.id, *input.ReadVersion)
			return 1, nil, err
		}, true
	case threadMarkV2Input:
		return s.planThreadMark(w, r, actor, write.id, input, dismissals)
	case messageMarkV2Input:
		return planMessageMark(w, r, write.id, input, dismissals)
	case askMarkV2Input:
		return s.planAskMark(w, r, actor, write.id, input)
	}
	s.writeHandlerError(w, r, fmt.Errorf("a %s mark's write was read as %T, which no kind's arm is", write.kind, write.arm))
	return nil, false
}

// planThreadMark reads a thread mark's write: read_through moves the read mark forward
// (moveThreadReadIn), and dismissed adds the thread to the request's dismissal, held to the version
// the page drew, or to its restore.
func (s *server) planThreadMark(w http.ResponseWriter, r *http.Request, actor model.Actor, id string, input threadMarkV2Input, dismissals *markDismissals) (markStep, bool) {
	if input.ReadThrough == nil && input.Dismissed == nil {
		writeError(w, r, apicode.InvalidState, "read_through or dismissed is required")
		return nil, false
	}
	if input.ReadThrough != nil {
		if _, err := parseAgentStateCutoff("read_through", input.ReadThrough); err != nil {
			s.writeHandlerError(w, r, err)
			return nil, false
		}
	}
	switch {
	case input.Dismissed == nil:
	case !*input.Dismissed:
		dismissals.restore.threads = append(dismissals.restore.threads, id)
	case input.Version == nil || *input.Version < 0:
		writeError(w, r, apicode.InvalidState, "dismissed true takes version, the thread's version as GET "+
			"/api/v2/me/threads answered it, a non-negative integer: a dismissal is written only while the thread still holds it")
		return nil, false
	default:
		dismissals.dismiss.threads = append(dismissals.dismiss.threads, id)
		dismissals.dismiss.seenVersions[id] = *input.Version
	}
	if input.ReadThrough == nil {
		return nil, true
	}
	dismissals.read = append(dismissals.read, id)
	return func(ctx context.Context, tx pgx.Tx) (int, []model.Event, error) {
		event, err := s.moveThreadReadIn(ctx, tx, actor, id, *input.ReadThrough)
		changed, events := markEvents(event)
		return changed, events, err
	}, true
}

// planMessageMark reads a message mark's write: dismissed adds the direct conversation to the
// request's dismissal, held to what the page drew in it, or to its restore.
func planMessageMark(w http.ResponseWriter, r *http.Request, id string, input messageMarkV2Input, dismissals *markDismissals) (markStep, bool) {
	const takes = "dismissed true takes newest_reply and unread_replies, each a message id: the newest session reply " +
		"the page drew in the thread and the replies it drew as unread"
	switch {
	case input.Dismissed == nil:
		writeError(w, r, apicode.InvalidState, "dismissed is required")
		return nil, false
	case !*input.Dismissed:
		dismissals.restore.roots = append(dismissals.restore.roots, id)
		return nil, true
	case input.NewestReply == nil || input.UnreadReplies == nil:
		writeError(w, r, apicode.InvalidState, takes)
		return nil, false
	}
	newest, err := uuid.Parse(*input.NewestReply)
	if err != nil {
		writeError(w, r, apicode.InvalidState, takes)
		return nil, false
	}
	unread := make([]string, 0, len(*input.UnreadReplies))
	for _, reply := range *input.UnreadReplies {
		parsed, err := uuid.Parse(reply)
		if err != nil {
			writeError(w, r, apicode.InvalidState, takes)
			return nil, false
		}
		unread = append(unread, parsed.String())
	}
	dismissals.dismiss.roots = append(dismissals.dismiss.roots, id)
	dismissals.dismiss.seenNewest[id] = newest.String()
	dismissals.dismiss.seenUnread = append(dismissals.dismiss.seenUnread, unread...)
	return nil, true
}

// planAskMark reads an ask mark's write: a time snoozes the ask until it (snoozeAskIn), and null
// un-snoozes it (unsnoozeAskIn).
func (s *server) planAskMark(w http.ResponseWriter, r *http.Request, actor model.Actor, id string, input askMarkV2Input) (markStep, bool) {
	if len(input.SnoozedUntil) == 0 {
		writeError(w, r, apicode.SnoozeInput, "snoozed_until is required: the time to snooze the ask until, or null to un-snooze it")
		return nil, false
	}
	if string(input.SnoozedUntil) == "null" {
		return func(ctx context.Context, tx pgx.Tx) (int, []model.Event, error) {
			event, err := s.unsnoozeAskIn(ctx, tx, actor, id)
			changed, events := markEvents(event)
			return changed, events, err
		}, true
	}
	var text string
	var until time.Time
	err := json.Unmarshal(input.SnoozedUntil, &text)
	if err == nil {
		until, err = time.Parse(time.RFC3339, text)
	}
	if err != nil {
		writeError(w, r, apicode.SnoozeInput, "snoozed_until must be an RFC3339 timestamp, or null to un-snooze the ask")
		return nil, false
	}
	return func(ctx context.Context, tx pgx.Tx) (int, []model.Event, error) {
		_, event, err := s.snoozeAskIn(ctx, tx, actor, id, until)
		changed, events := markEvents(event)
		return changed, events, err
	}, true
}

// restoreThroughAll is the mark a restore under the Inbox's narrowing that names no
// dismissed_through matches its threads by: every dismissal, since each is stored as no later than
// the moment it was written, and the read compares each with this mark no later than now.
const restoreThroughAll = "infinity"

// patchMarksByFilter writes what set names on the threads filter narrows to - the read mark POST
// /api/v1/me/inbox/read writes (markInboxReadThreads), or the dismissal or restore POST
// /api/v1/me/inbox/dismiss writes (dismissInboxThreads) - and answers how many threads it changed,
// or writes the refusal and answers false.
func (s *server) patchMarksByFilter(w http.ResponseWriter, r *http.Request, actor model.Actor, filter *inboxThreadFilters, set markSetV2) (int, bool) {
	var filters inboxThreadFilters
	if filter != nil {
		filters = *filter
	}
	scope, err := parseInboxReadScope(filters)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return 0, false
	}
	checked := func(name string, mark *string) bool {
		if _, err := parseAgentStateCutoff(name, mark); err != nil {
			s.writeHandlerError(w, r, err)
			return false
		}
		return true
	}
	var changed int
	var events []model.Event
	switch {
	case set.ReadThrough != nil && set.DismissedThrough == nil && set.Dismissed == nil:
		if !checked("read_through", set.ReadThrough) {
			return 0, false
		}
		changed, events, err = s.markInboxReadThreads(r.Context(), actor, *set.ReadThrough, scope)
	case set.ReadThrough == nil && set.DismissedThrough != nil && (set.Dismissed == nil || *set.Dismissed):
		if !checked("dismissed_through", set.DismissedThrough) {
			return 0, false
		}
		changed, events, err = s.dismissInboxThreads(r.Context(), actor,
			inboxDismissPlanOf(inboxDismissRequest{DismissThrough: set.DismissedThrough}), inboxDismissRows{}, scope)
	case set.ReadThrough == nil && set.Dismissed != nil && !*set.Dismissed:
		through := restoreThroughAll
		if set.DismissedThrough != nil {
			if !checked("dismissed_through", set.DismissedThrough) {
				return 0, false
			}
			through = *set.DismissedThrough
		}
		changed, events, err = s.dismissInboxThreads(r.Context(), actor,
			inboxDismissPlanOf(inboxDismissRequest{DismissThrough: &through, Restore: true}), inboxDismissRows{}, scope)
	default:
		writeError(w, r, apicode.InvalidState, "set takes read_through, dismissed_through, or dismissed false "+
			"(with dismissed_through to bound the restore), one of the three")
		return 0, false
	}
	if err != nil {
		s.writeHandlerError(w, r, err)
		return 0, false
	}
	s.publish(r.Context(), events...)
	return changed, true
}
