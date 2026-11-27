package api

// v2_asks.go is the asks of /api/v2 (R1 of dispatch://INFRA-4071/spec): one list of asks across
// projects, GET /api/v2/asks, which answers for GET /api/v1/asks/open, GET /api/v1/inbox and GET
// /api/v1/references/questions (the route map's J24); one ask, its reword and hand-on, its
// followers and its state; an issue's asks and the create of one. Every read and every write of an
// ask answers the one ask row, askV2 (J4, J24), and every write runs the work its /api/v1 route
// runs (openAsk, rewordAsk, handAskOn, recordAnswer, recordResolution, recordDecline,
// writeAskFollower), each path object checked first and answered with its own 404 (R7).

import (
	"cmp"
	"context"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
	"github.com/sjawhar/envoy/internal/dispatch/asks"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// The authorities of the ask rows (authorities_v2.go): a value that reads or
// writes as a person needs the person scope, every other value none.
var (
	askMarkAuthority = authorityRule{
		Where: authorityWhere{In: partQuery, Name: "include"}, Value: "mark", Scopes: routeScopeSet{model.ScopePerson},
	}
	askWaitingOnMeAuthority = authorityRule{
		Where: authorityWhere{In: partQuery, Name: "waiting_on"}, Value: "me", Scopes: routeScopeSet{model.ScopePerson},
	}
	askAskedOfAuthority = authorityRule{
		Where: authorityWhere{In: partBody, Name: "asked_of"}, Scopes: routeScopeSet{model.ScopePerson},
	}
	askAnsweredAuthority = authorityRule{
		Where: authorityWhere{In: partBody, Name: "state"}, Value: "answered", Scopes: routeScopeSet{model.ScopePerson},
	}
	askDeclinedAuthority = authorityRule{
		Where: authorityWhere{In: partBody, Name: "state"}, Value: "declined", Scopes: routeScopeSet{model.ScopePerson},
	}
)

// The refusals of an ask's writes: a question, options or urgency its open or its reword refuses,
// and a block ask whose block its document no longer holds when the ask is reworded, answered,
// resolved or declined.
var (
	askInvalidRefusal      = apicode.Refusal{Condition: apicode.AskInvalid, Code: apicode.InvalidAsk}
	askBlockMissingRefusal = apicode.Refusal{Condition: apicode.AskBlockMissing, Code: apicode.TargetNotFound}
)

// askOpenRefusals are the refusals of the ask a document's or an issue's ask list opens (openAsk),
// with more, the route's own, after them: its kind, its question, options and urgency, a value past
// its bound, and its anchor and what the document service answers it (anchorRefusals).
func askOpenRefusals(more ...apicode.Refusal) []apicode.Refusal {
	return append(anchorRefusals([]apicode.Refusal{
		undeclaredKeyRefusal, valuePastBoundRefusal, anchorInvalidRefusal, artifactNotDocumentRefusal,
		askInvalidRefusal,
		{Condition: apicode.AskKindRefused, Code: apicode.AskKindInput},
	}...), more...)
}

// The refusals of a follow and an unfollow of an ask (changeAskFollowerV2, askFollowerActor): a
// blank {session_id}, and another session's follow from a credential without the person scope.
var (
	followerSessionBlankRefusal   = apicode.Refusal{Condition: apicode.FollowerSessionBlank, Code: apicode.FollowerInput}
	followOfAnotherSessionRefusal = apicode.Refusal{Condition: apicode.FollowOfAnotherSession, Code: apicode.FollowerForbidden}
)

// askRowText is what every description of a route answering asks says of the row it answers.
const askRowText = "Each ask is one row: the ask's own fields at top level, opened_event_id, ref (its dispatch:// address, " +
	"\"\" for an ask no address names), issue {key, title, assignee} or document {project, slug, name}, the issue's priority " +
	"and last_reply; include=thread adds its thread (replies, rewordings, answers, followers, claimant, route, route_status, " +
	"route_holder) and include=mark (the person scope) the caller's mark on it, {kind, id, snoozed_until}."

// v2AskRows are the asks: rows 33, 79 and 171; 34 and 39; 35 and 37; 40; 41; 36, 38 and
// 42; 93; and 94 of the route map.
func (s *server) v2AskRows() []routeRow {
	askIDs := func() *idRoute { return &idRoute{Kind: "ask", NotFound: apicode.AskNotFound} }
	// askList is the List of the two lists of asks here: no bounds of their own, so 50 and 200
	// (listBounds.limits), which each one's description words its page from.
	askList := listBounds{}
	return []routeRow{
		{
			Method: http.MethodGet, Pattern: "/asks", Auth: authAny, Versions: onlyV2,
			Description: "The asks of every project, filtered; with no filter, every open ask of every project the caller reads, " +
				"whoever it waits on. state=open (the default; an open ask on an open issue or a project document), answered, " +
				"resolved or all. project=<key>, repeatable, keeps those projects' asks. waiting_on=me (the person scope) or " +
				"<email> keeps the asks waiting on that person, resolved as each ask is read in three steps: the person the " +
				"question was handed to, then the issue's assignee, then the project's default assignee (an email nobody has " +
				"signed in with is 400 ASSIGNEE_NOT_ALLOWED), and waiting_on=none the asks no step names anybody for. " +
				"author_session=<session> keeps one session's own asks. " +
				"from and to (RFC3339) window created_at, from inclusive and to exclusive. source=<kind>:<id> (kind artifact, " +
				"ask, comment or message; id a whole uuid or a unique prefix of 8 characters or more) reads the question map of " +
				"the document that source's text belongs to: one row for each `:::ask{#id}` block the document's latest version " +
				"declares with the ask settlement made for it, question_block_id naming the block a #id in the source's text " +
				"names, of every state where no state is given; the document's id is beside the page as document, a source on " +
				"no document is 404 NOT_FOUND and a prefix two sources carry 409 ID_AMBIGUOUS (any other source is 400 " +
				"INVALID_QUERY). " + askRowText + " sort=priority (the default: the issue's priority, unset last, then whose " +
				"turn it is, the person's before the agent's, then the latest activity, newest first) or created_at (oldest " +
				"first), each reversed after a -. " + includedPageLimitText(askList, askListV2Query{}) + " {items, total, limit, offset, count, " +
				"waiting_on_human, waiting_on_agent, as_of}: count the asks the filters keep, as total does, the two waiting_on " +
				"counts the asks among them whose state is open by whose turn it is (under state=open, the open asks on open " +
				"issues and project documents; under another state, every ask of state open it keeps, one on a closed issue " +
				"among them), and as_of the database time the read was made at.",
			Handler:  s.listAsksV2,
			Resource: "asks", Kind: kindCollection, Key: "{id}",
			TimeFields:   []string{"created_at"},
			CrossProject: true,
			Query:        askListV2Query{},
			List:         askList,
			Shape:        routeShape{Status: http.StatusOK, Response: askListV2{}},
			Authorities:  []authorityRule{askWaitingOnMeAuthority, askMarkAuthority},
			Refusals: listRefusals(
				actingSessionMismatchRefusal, assigneeNotSignedInRefusal, refPrefixAmbiguousRefusal,
				apicode.Refusal{Condition: apicode.QuestionSourceWithoutDocument, Code: apicode.NotFound},
			),
		},
		{
			Method: http.MethodGet, Pattern: "/asks/{id}", Auth: authAny, Versions: onlyV2,
			Description: "Read one ask, as every list of asks answers it, with the block position of its anchor. " + askRowText +
				" Its thread's claimant is the session holding its issue's claim and route its issue's route, whom a person's " +
				"answer and replies also reach, and route_holder the live session that route reaches now: the session itself or " +
				"the role's holder; each is null when absent, route_holder also when nobody holds the route or the Envoy " +
				"listener did not answer, and all are null on a project document's ask. 404 ASK_NOT_FOUND for an id no ask has.",
			Handler:  s.getAskV2,
			Resource: "asks", Kind: kindItem, Key: "{id}",
			Query:       askReadV2Query{},
			Shape:       routeShape{Status: http.StatusOK, Response: askV2{}},
			IDs:         askIDs(),
			Authorities: []authorityRule{askMarkAuthority},
			Refusals:    []apicode.Refusal{undeclaredKeyRefusal, askMissingRefusal, actingSessionMismatchRefusal},
		},
		{
			Method: http.MethodPatch, Pattern: "/asks/{id}", Auth: authAny, Versions: onlyV2,
			Description: "Change an open ask, one of two ways a request: reword it {question?, options?, multiple?, urgency?}, " +
				"its author or a credential holding the person scope (another session's ask is 403 NOT_AUTHOR otherwise), a " +
				"block ask's `:::ask` block rewritten with it; or hand it to another person {asked_of}, the email of somebody " +
				"who has signed in (400 ASSIGNEE_NOT_ALLOWED otherwise), which needs the person scope: the ask waits on them " +
				"from now on, wherever its issue goes, and naming the assignee of its own issue empties the field instead, so the " +
				"ask follows its issue again; either way the row records who handed it on and when (asked_of_by, asked_of_at). " +
				"A body naming both, or neither, is 400 INVALID_ASK. A bearer names its session in actor on a reword; a hand-on " +
				"is the person's act, recorded as the person the credential acts for. Answers the ask as GET " +
				"/api/v2/asks/{id} reads it. An ask that is not open is 409 ASK_NOT_OPEN, an approval request's wording 409 " +
				"ASK_KIND_FIXED, an ask on a closed issue 409 ISSUE_CLOSED; 404 ASK_NOT_FOUND. " +
				specWriterDocumentText("a reword of a block ask in its spec or Verification document"),
			Handler:  s.editAskV2,
			Resource: "asks", Kind: kindItem, Key: "{id}",
			Body:        requestBody{Kind: bodyJSON, Type: editAskV2Input{}},
			Shape:       routeShape{Status: http.StatusOK, Response: askWriteV2{}},
			Authorities: []authorityRule{askAskedOfAuthority},
			Scope:       scopeProject, ProjectOf: projectPathAsk,
			IDs: askIDs(),
			// A reword of a block ask rewrites its block (docs.SetAskBlockText).
			Refusals: documentRefusals(
				undeclaredKeyRefusal, askMissingRefusal, notAuthorRefusal, assigneeNotSignedInRefusal, valuePastBoundRefusal,
				issueClosedRefusal, askBlockMissingRefusal, documentPastSizeBoundRefusal, askInvalidRefusal,
				apicode.Refusal{Condition: apicode.AskNotOpenToEdit, Code: apicode.AskNotOpen},
				apicode.Refusal{Condition: apicode.AskKindChange, Code: apicode.AskKindFixed},
				apicode.Refusal{Condition: apicode.AskBlockUnrepresentable, Code: apicode.AskBlockText},
				specWriterNotArchitectRefusal,
			),
		},
		{
			Method: http.MethodDelete, Pattern: "/asks/{id}/followers/{session_id}", Auth: authAny, Versions: onlyV2,
			Description: "Unfollow an ask: a session's own credential for itself, and a credential holding the person scope for " +
				"any session; a bearer that holds no person scope names its session in actor, and acting on another session is " +
				"403 FOLLOWER_FORBIDDEN. 204; 404 FOLLOWER_NOT_FOUND for a session that does not follow it, 404 ASK_NOT_FOUND.",
			Handler:  s.unfollowAskV2,
			Resource: "followers", Kind: kindItem, Key: "{session_id}",
			Body:  requestBody{Kind: bodyJSON, Type: actorInput{}, Optional: true},
			Shape: routeShape{Status: http.StatusNoContent},
			Scope: scopeProject, ProjectOf: projectPathAsk, SessionPath: "session_id",
			IDs: askIDs(),
			Refusals: []apicode.Refusal{
				undeclaredKeyRefusal, askMissingRefusal, followerSessionBlankRefusal, followOfAnotherSessionRefusal,
				{Condition: apicode.SessionNotFollowing, Code: apicode.FollowerNotFound},
			},
		},
		{
			Method: http.MethodPut, Pattern: "/asks/{id}/followers/{session_id}", Auth: authAny, Versions: onlyV2,
			Description: "Follow an ask, so its answer, edits, resolution and replies reach the session: a session's own " +
				"credential for itself, and a credential holding the person scope for any session; a bearer that holds no " +
				"person scope names its session in actor, and acting on another session is 403 FOLLOWER_FORBIDDEN. Answers 200 " +
				"the follower {session_id, since}; following an ask the session already follows answers the same and writes " +
				"nothing. 404 ASK_NOT_FOUND.",
			Handler:  s.followAskV2,
			Resource: "followers", Kind: kindItem, Key: "{session_id}",
			Body:  requestBody{Kind: bodyJSON, Type: actorInput{}, Optional: true},
			Shape: routeShape{Status: http.StatusOK, Response: model.AskFollower{}},
			Scope: scopeProject, ProjectOf: projectPathAsk, SessionPath: "session_id",
			IDs:      askIDs(),
			Refusals: []apicode.Refusal{undeclaredKeyRefusal, askMissingRefusal, followerSessionBlankRefusal, followOfAnotherSessionRefusal},
		},
		{
			Method: http.MethodPut, Pattern: "/asks/{id}/state", Auth: authAny, Versions: onlyV2,
			Description: "Move an ask to a state, its body by state. {state: answered, selected?, text?, expected_edited_at, " +
				"expected_answer_at?} answers it, which needs the person scope, and the approve scope for an approval request: " +
				"expected_edited_at is the edited_at of the wording answered, null for one never edited (absent is 400 " +
				"ASK_REVISION_REQUIRED, one the ask has moved past 409 ASK_EDITED), and a change of the current answer by the " +
				"person who gave it names that answer's at in expected_answer_at (another person's change is 403 NOT_ANSWERER, " +
				"one the answer has moved past 409 ASK_ANSWER_CHANGED, an approval's 409 ASK_APPROVAL_REVIEW). {state: resolved " +
				"or retracted, reason} closes it without an answer, for any caller, a bearer naming its session in actor. " +
				"{state: declined, reason?} declines it, which needs the person scope: the reason stands beside the words " +
				"Dispatch writes for the asking agent, and an approval request is 409 ASK_NOT_DECLINABLE. A block ask's block " +
				"records the state too. A body equal to the ask's present state and fields, from the caller who set them, " +
				"answers 200 and writes nothing; any other first answer to an answered ask, and a resolve or a decline of one, " +
				"is 409 ASK_ANSWERED, and a resolved ask 409 ASK_RESOLVED. An approve-only token (the secrets broker's " +
				"proxy for a person's approvals) may send state answered of an approval request and no other declared " +
				"state: its answer of any other ask, a saved answer under an Idempotency-Key and a repeat of the person's " +
				"own answer included, and each other state this route declares (resolved, retracted, declined) are 403 " +
				"APPROVE_ONLY_TOKEN. " +
				"Answers the ask as GET /api/v2/asks/{id} reads it; " +
				"400 STATE_INVALID for a state it does not declare, 400 INVALID_ANSWER, INVALID_RESOLUTION or CAP_EXCEEDED for a body its " +
				"state refuses, 409 ISSUE_CLOSED on a closed issue, 404 ASK_NOT_FOUND. " +
				specWriterDocumentText("a retraction or a resolution of a block ask whose block, or a copy of it, is in its "+
					"spec or Verification document,"),
			Handler:  s.putAskStateV2,
			Resource: "state", Kind: kindState,
			Body: requestBody{Kind: bodyStateUnion, Arms: map[string]any{
				"answered":  answerAskStateInput{},
				"resolved":  resolveAskStateInput{},
				"retracted": resolveAskStateInput{},
				"declined":  declineAskStateInput{},
			}},
			Shape:       routeShape{Status: http.StatusOK, Response: askWriteV2{}},
			Authorities: []authorityRule{askAnsweredAuthority, askDeclinedAuthority},
			Scope:       scopeProject, ProjectOf: projectPathAsk,
			IDs: askIDs(),
			// A block ask's state is written into its block (docs.SetBlockAttributes, docs.SettleCopiesOf).
			Refusals: documentRefusals(
				undeclaredKeyRefusal, askMissingRefusal, issueClosedRefusal, valuePastBoundRefusal, askBlockMissingRefusal,
				apicode.Refusal{Condition: apicode.AskAnsweredBefore, Code: apicode.AskAnswered},
				apicode.Refusal{Condition: apicode.AnsweredFirstByAnother, Code: apicode.AskClosed},
				apicode.Refusal{Condition: apicode.AskResolvedBefore, Code: apicode.AskResolved},
				apicode.Refusal{Condition: apicode.AnswerMoved, Code: apicode.AskAnswerChanged},
				apicode.Refusal{Condition: apicode.ApprovalAnsweredBefore, Code: apicode.AskApprovalReview},
				apicode.Refusal{Condition: apicode.QuestionEditedSinceRead, Code: apicode.AskEdited},
				apicode.Refusal{Condition: apicode.ApprovalDeclined, Code: apicode.AskNotDeclinable},
				apicode.Refusal{Condition: apicode.RevisionMissing, Code: apicode.AskRevisionRequired},
				apicode.Refusal{Condition: apicode.AnswerInvalid, Code: apicode.InvalidAnswer},
				apicode.Refusal{Condition: apicode.ResolutionInvalid, Code: apicode.InvalidResolution},
				apicode.Refusal{Condition: apicode.NotTheAnswerer, Code: apicode.NotAnswerer},
				reasonMissingRefusal,
				specWriterNotArchitectRefusal,
			),
		},
		{
			Method: http.MethodGet, Pattern: "/issues/{key}/asks", Auth: authAny, Versions: onlyV2,
			Description: "List an issue's asks, oldest first, each as GET /api/v2/asks answers it. state=all (the default), " +
				"open, answered or resolved. " + askRowText + " " + includedPageLimitText(askList, issueAsksV2Query{}) +
				" {items, total, limit, offset}. 404 ISSUE_NOT_FOUND for an issue that does not exist.",
			Handler:  s.listIssueAsksV2,
			Resource: "asks", Kind: kindCollection, Key: "{id}",
			Query:       issueAsksV2Query{},
			List:        askList,
			Shape:       routeShape{Status: http.StatusOK, Response: page[askV2]{}},
			Authorities: []authorityRule{askMarkAuthority},
			Refusals:    listRefusals(issueMissingRefusal, actingSessionMismatchRefusal),
		},
		{
			Method: http.MethodPost, Pattern: "/issues/{key}/asks", Auth: authAny, Versions: onlyV2,
			Description: "Open a question ask on an issue {question, options?, multiple?, urgency?, anchor?}, a bearer naming its " +
				"session in actor: at most 8 options with unique labels, urgency low, med (the default), high or blocking. " +
				"Answers 201 the ask as GET /api/v2/asks/{id} reads it, with advice beside it. After the write commits, a " +
				"related-item check searches the project for the question and waits at most 5 seconds; the advice's " +
				"suggestions say whether it found related items (outcome, with the reason in missing when not made), how it " +
				"searched, and the three items most like it with each ask item's answer state. 400 INVALID_ASK or ASK_KIND_INPUT " +
				"for a question it refuses, 400 CAP_EXCEEDED past a bound, 409 ISSUE_CLOSED on a closed issue, 404 " +
				"ISSUE_NOT_FOUND.",
			Handler:  s.createIssueAskV2,
			Resource: "asks", Kind: kindCollection, Key: "{id}",
			Body: requestBody{Kind: bodyJSON, Type: createAskInput{}},
			Shape: routeShape{
				Status: http.StatusCreated, Response: askWriteV2{}, Advice: writeAdvice{},
				OtherAdvice: []any{suggestionsOnlyAdvice{}}, Refusals: anchorRefusalAnswers(),
			},
			Scope: scopeProject, ProjectOf: projectPathIssue,
			Refusals: askOpenRefusals(issueMissingRefusal, issueClosedRefusal, anchorDocumentMissingRefusal),
		},
	}
}

// askWriteV2 is the ask a /api/v2 write of one answers - PATCH /api/v2/asks/{id}, PUT
// /api/v2/asks/{id}/state, the two creates and the ask of an approval request - and the core of
// every read of one (askV2): the ask as the dashboard reads it (shapedAsk) with what the lists
// read beside it - its address, its owner, its issue's priority and its newest reply - at top
// level. A write never answers the members include= adds to a read, its thread and the caller's
// mark (completeWrittenAskV2), so its type holds neither and the document states what the write
// answers (dispatch://INFRA-4071/spec R6); so no answer a write's key keeps can hold a reply's
// block (TestEveryKeyedAnswerThatCanHoldAReplyBlockIsReadAfresh). It is a type of its own, so no
// /api/v1 shape's tags change (R3).
type askWriteV2 struct {
	shapedAsk
	// OpenedEventID is the id of the ask's ask.opened event, which every ask has: a value, where
	// model.Ask's is a pointer the /api/v1 document leaves optional (R6). A read that finds none
	// is the server's fault, and answers 500 naming the ask in the log (completeAsksV2).
	OpenedEventID int64 `json:"opened_event_id"`
	// Ref is the ask's dispatch:// address, under its issue or the project document that owns it
	// (refs.AskRef), and "" for an ask no address names.
	Ref string `json:"ref"`
	// Issue is the ask's issue, and Document its project document; the other is null.
	Issue    *inboxIssue    `json:"issue"`
	Document *inboxDocument `json:"document"`
	// Priority is the ask's issue's priority, null for one with none and for a document's ask.
	Priority *int `json:"priority"`
	// LastReply is the newest reply in the ask's thread, null for an ask nobody has replied to.
	LastReply *model.AskLastReply `json:"last_reply"`
	// QuestionBlockID is, on a read of a source's question map (source=), the `:::ask{#id}` block
	// of the source's document this row is the ask of: the block a #id in the source's text names,
	// which a copy of an ask in another block holds under an id of its own.
	QuestionBlockID string `json:"question_block_id,omitempty"`
}

// askV2 is the one ask row of /api/v2's reads (the route map's J4 and J24): the ask as a write
// answers it (askWriteV2) and the members include= adds.
type askV2 struct {
	askWriteV2
	// Thread is the ask's thread, with include=thread.
	Thread *askThread `json:"thread,omitempty"`
	// Mark is the caller's mark on the ask, with include=mark.
	Mark *askMarkV2 `json:"mark,omitempty"`
}

// askMarkV2 is the caller's own mark on an ask, as a mark of the caller's reads (the route map's
// section (e)): its kind and id, and when the caller snoozed it until, null where they have not.
type askMarkV2 struct {
	Kind         string  `json:"kind"`
	ID           string  `json:"id"`
	SnoozedUntil *string `json:"snoozed_until"`
}

// askListV2Query is GET /api/v2/asks's query.
type askListV2Query struct {
	pageQuery
	// State is open, answered, resolved or all; none is open, or all with Source.
	State string `query:"state" enum:"open,answered,resolved,all"`
	// Project keeps the asks of these projects.
	Project []string `query:"project"`
	// WaitingOn is me, a person's email, or none: the asks no step names anybody for.
	WaitingOn queryFilter[string] `query:"waiting_on"`
	// AuthorSession keeps the asks one session asked.
	AuthorSession string `query:"author_session"`
	// Source is `<kind>:<id>`, a reference source whose document's question map the list reads.
	Source string `query:"source"`
	// From and To window created_at.
	From *time.Time `query:"from"`
	To   *time.Time `query:"to"`
	// Include adds thread, which renders each listed row whole (renders), and mark.
	Include []string `query:"include,comma" enum:"thread,mark" renders:"thread"`
	// Sort is priority (the Inbox's order) or created_at.
	Sort querySort `query:"sort" enum:"priority,created_at" default:"priority"`
}

// askListV2 is the body GET /api/v2/asks answers: the envelope of ask rows, with GET
// /api/v1/asks/open's counts and snapshot time beside it, and the question map's document where
// the read named a source.
type askListV2 struct {
	page[askV2]
	Count          int     `json:"count"`
	WaitingOnHuman int     `json:"waiting_on_human"`
	WaitingOnAgent int     `json:"waiting_on_agent"`
	AsOf           string  `json:"as_of"`
	Document       *string `json:"document,omitempty"`
}

// askReadV2Query is GET /api/v2/asks/{id}'s query.
type askReadV2Query struct {
	Include []string `query:"include,comma" enum:"thread,mark"`
}

// issueAsksV2Query is GET /api/v2/issues/{key}/asks's query.
type issueAsksV2Query struct {
	pageQuery
	State string `query:"state" enum:"all,open,answered,resolved" default:"all"`
	// Include adds thread, which renders each listed row whole (renders), and mark.
	Include []string `query:"include,comma" enum:"thread,mark" renders:"thread"`
}

// editAskV2Input is PATCH /api/v2/asks/{id}'s body: a reword, editAskInput's fields, or a hand-on,
// asked_of.
type editAskV2Input struct {
	editAskInput
	AskedOf *string `json:"asked_of"`
}

// answerAskStateInput is the answered arm of PUT /api/v2/asks/{id}/state: answerAskInput's fields.
type answerAskStateInput struct {
	State string `json:"state"`
	answerAskInput
}

// resolveAskStateInput is the resolved and the retracted arm of PUT /api/v2/asks/{id}/state: the
// reason the ask closes without an answer.
type resolveAskStateInput struct {
	State  string       `json:"state"`
	Reason string       `json:"reason" required:"true"`
	Actor  *model.Actor `json:"actor"`
}

// declineAskStateInput is the declined arm of PUT /api/v2/asks/{id}/state: the person's optional
// reason.
type declineAskStateInput struct {
	State  string       `json:"state"`
	Reason *string      `json:"reason"`
	Actor  *model.Actor `json:"actor"`
}

// askV2Include is what a read of asks adds to each row (include, J1).
type askV2Include struct {
	thread bool
	// markOf is the canonical login whose mark each row carries, "" for none.
	markOf string
}

// askIncludeOf reads include's members: thread, and mark, which is the caller's own and so needs
// the person the caller acts for (actingPerson, which the row's authority has already held to the
// person scope). It writes a refusal itself and answers false.
func (s *server) askIncludeOf(w http.ResponseWriter, r *http.Request, include []string) (askV2Include, bool) {
	read := askV2Include{thread: slices.Contains(include, "thread")}
	if slices.Contains(include, "mark") {
		person, ok := s.actingPerson(w, r)
		if !ok {
			return askV2Include{}, false
		}
		read.markOf = person.ID
	}
	return read, true
}

// askV2Read is one statement over the asks a /api/v2 read lists: the conditions they meet, over
// askReadFrom's joins, the arguments those conditions bind, and the order they are listed in.
type askV2Read struct {
	conditions []string
	arguments  []any
	order      string
	// markOf is the login whose snooze each row carries (include=mark), "" for none.
	markOf string
}

// argument binds value and answers its placeholder.
func (read *askV2Read) argument(value any) string {
	read.arguments = append(read.arguments, value)
	return "$" + strconv.Itoa(len(read.arguments))
}

// where adds a condition every row meets.
func (read *askV2Read) where(condition string) {
	read.conditions = append(read.conditions, condition)
}

// clause is the conditions joined, true for none.
func (read askV2Read) clause() string {
	if len(read.conditions) == 0 {
		return "true"
	}
	return strings.Join(read.conditions, " and ")
}

// askV2Columns are what a /api/v2 read of asks selects beyond askReadColumns, off askReadFrom's
// joins: the ask's issue's key, title, assignee and priority, and its own document's project, slug
// and name.
const askV2Columns = `, i.key, i.title, i.assignee, i.priority, ar.project_key, ar.slug, ar.name`

// askV2Rows reads the rows read lists, in its order, one page of them where page is given and
// every one where it is nil. A row is the ask as askReadColumns reads it with what askV2Columns
// adds, and the caller's snooze where read carries a mark; completeAsksV2 fills in the rest.
func (s *server) askV2Rows(ctx context.Context, q queryer, read askV2Read, page *pageQuery) ([]askV2, error) {
	read.arguments = slices.Clone(read.arguments)
	snooze, snoozeJoin := `null::timestamptz`, ``
	if read.markOf != "" {
		snooze = `sn.snoozed_until`
		snoozeJoin = `
		left join user_ask_snooze sn on sn.ask_id = a.id and sn.login = ` + read.argument(read.markOf)
	}
	statement := `select ` + askReadColumns + askV2Columns + `, ` + snooze + `
		` + askReadFrom + snoozeJoin + `
		where ` + read.clause() + `
		order by ` + read.order
	if page != nil {
		statement += ` limit ` + read.argument(page.Limit) + ` offset ` + read.argument(page.Offset)
	}
	rows, err := q.Query(ctx, statement, read.arguments...)
	if err != nil {
		return nil, fmt.Errorf("read the asks: %w", err)
	}
	defer rows.Close()
	listed := []askV2{}
	for rows.Next() {
		var row askV2
		var issueKey, issueTitle, issueAssignee, documentProject, documentSlug, documentName *string
		var snoozedUntil *time.Time
		ask, reply, err := scanAskRead(rows, &issueKey, &issueTitle, &issueAssignee, &row.Priority,
			&documentProject, &documentSlug, &documentName, &snoozedUntil)
		if err != nil {
			return nil, err
		}
		row.Ask, row.LastReply = ask, reply
		if issueKey != nil {
			row.Issue = &inboxIssue{Key: *issueKey, Title: *issueTitle, Assignee: issueAssignee}
		}
		if documentProject != nil {
			row.Document = &inboxDocument{Project: *documentProject, Slug: *documentSlug, Name: *documentName}
		}
		if read.markOf != "" {
			row.Mark = &askMarkV2{Kind: "ask", ID: ask.ID}
			if snoozedUntil != nil {
				stamp := timestampValue(*snoozedUntil)
				row.Mark.SnoozedUntil = &stamp
			}
		}
		listed = append(listed, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the asks: %w", err)
	}
	return listed, nil
}

// completeAsksV2 fills in what every row of rows a read of asks lists carries beyond its read
// (askV2Rows): its identity event, its backlink count and its part ids (prepareAsksV2), its address
// and its trees (finishAsksV2), its anchor's block position on the read of one ask (single) alone,
// and the thread where include asks for it, read with the rows' texts through one reader, so the
// texts are one query and one store.
func (s *server) completeAsksV2(ctx context.Context, r *http.Request, rows []askV2, thread, single bool) error {
	who := s.requestReader(r)
	if err := s.prepareAsksV2(ctx, who, rows, single); err != nil {
		return err
	}
	reader := s.readDisplayTrees(ctx, wantsDisplayTrees(r))
	if thread {
		ids := make([]string, len(rows))
		for index := range rows {
			ids[index] = rows[index].ID
		}
		threads, err := s.loadAskThreads(ctx, who, ids, reader)
		if err != nil {
			return err
		}
		// loadAskThreads reads no answers (an Inbox row is an open ask), so they are merged here,
		// as an issue's ask list merges them (attachIssueAskThreads).
		answers, err := s.loadAskAnswersByAsk(ctx, s.deps.Store.Pool, ids)
		if err != nil {
			return err
		}
		for index := range rows {
			read := threads[rows[index].ID]
			read.Answers = askAnswerReads(answers[rows[index].ID], reader)
			rows[index].Thread = &read
		}
	}
	if err := finishAsksV2(rows, reader); err != nil {
		return err
	}
	reader.resolve()
	return nil
}

// completeWrittenAskV2 fills in what the ask a write answers carries beyond what writtenAskRowsV2
// reads, as GET /api/v2/asks/{id} reads it with no include: its anchor's block position, its
// identity event's id, its address and its trees - never its thread, whose read ends with a
// listener call that may store the routes it read (loadAskReaches), so a write's read-back reaches
// no write of its own outside the transactions that marked the write's key
// (TestEveryKeyedWriteCommitsInsideATransactionTheMarkHooks). A write runs it once its
// transaction has committed: the position is the document service's to read and the trees are
// read off any transaction of the caller's, and neither fails the answer - an anchor read fails
// only with the request's context (anchorBlock), and a row with no ask.opened event, which the
// write itself appended, is the server's fault (finishAsksV2).
func (s *server) completeWrittenAskV2(ctx context.Context, r *http.Request, rows []askV2) (askWriteV2, error) {
	if err := s.anchorAsksV2(ctx, rows); err != nil {
		return askWriteV2{}, err
	}
	reader := s.readDisplayTrees(ctx, wantsDisplayTrees(r))
	if err := finishAsksV2(rows, reader); err != nil {
		return askWriteV2{}, err
	}
	reader.resolve()
	return rows[0].askWriteV2, nil
}

// prepareAsksV2 fills in the identity event, the inbound backlink count and the part ids every
// list of asks batches (loadAskDetails), and the anchor's block position on the read of one ask
// (single).
func (s *server) prepareAsksV2(ctx context.Context, who model.Actor, rows []askV2, single bool) error {
	if err := loadAskDetails(ctx, s.deps.Store.Pool, who, askV2Pointers(rows)); err != nil {
		return err
	}
	if single {
		return s.anchorAsksV2(ctx, rows)
	}
	return nil
}

// askV2Pointers addresses each row's ask in place, which loadAskDetails fills.
func askV2Pointers(rows []askV2) []*model.Ask {
	pointers := make([]*model.Ask, len(rows))
	for index := range rows {
		pointers[index] = &rows[index].Ask
	}
	return pointers
}

// anchorAsksV2 fills in each row's anchor block position (anchorBlock).
func (s *server) anchorAsksV2(ctx context.Context, rows []askV2) error {
	for index := range rows {
		position, err := s.anchorBlock(ctx, rows[index].Anchor)
		if err != nil {
			return err
		}
		rows[index].AnchorPosition = position
	}
	return nil
}

// finishAsksV2 gives every row its identity event's id, its address and its trees, registered
// with reader for its caller to resolve. A row whose read found no opened event is the server's
// fault, never an answer without the id (dispatch://INFRA-4071/spec R6).
func finishAsksV2(rows []askV2, reader *displayTreeReader) error {
	for index := range rows {
		row := &rows[index]
		if row.Ask.OpenedEventID == nil {
			return fmt.Errorf("the /api/v2 read of ask %s found no ask.opened event id, which every ask row carries", row.ID)
		}
		row.OpenedEventID = *row.Ask.OpenedEventID
		row.Ref = askV2Ref(*row)
		// The row's shape is taken once the ask is final: loadAskDetails has just written the
		// part ids of its options and of its answer into the row these fields stand in front of.
		row.shape(reader)
	}
	return nil
}

// askV2Ref is the dispatch:// address of the ask row holds, as every writer of an ask's address
// writes it (refs.AskRef): under its issue, or under the project document that owns it.
func askV2Ref(row askV2) string {
	ref := text.Ref{Kind: "ask", ID: row.ID}
	switch {
	case row.IssueKey != nil:
		ref.IssueKey = *row.IssueKey
	case row.Document != nil:
		ref.Project, ref.Document = row.Document.Project, row.Document.Slug
	}
	return refs.AskRef(ref)
}

// askV2ByID is the ask id names as askV2Rows reads it on q, with the mark of markOf where it names
// one, or 404 ASK_NOT_FOUND.
func (s *server) askV2ByID(ctx context.Context, q queryer, id, markOf string) ([]askV2, error) {
	read := askV2Read{order: "a.id", markOf: markOf}
	read.where("a.id = " + read.argument(id))
	rows, err := s.askV2Rows(ctx, q, read, nil)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, askNotFoundV2(id)
	}
	return rows, nil
}

// askNotFoundV2 is the 404 ASK_NOT_FOUND a /api/v2 route answers for an id no ask has, written
// once for each place that answers it: the read of the ask (askV2ByID), the check that it exists
// (requireAskV2) and the approve-only check of a keyed answer (gateApproveOnlyAskAnswer).
func askNotFoundV2(id string) *apiError {
	return &apiError{code: apicode.AskNotFound, message: "no ask has the id " + id}
}

// readAskV2 reads the ask id names as GET /api/v2/asks/{id} answers it, with include's members.
func (s *server) readAskV2(ctx context.Context, r *http.Request, id string, include askV2Include) (askV2, error) {
	rows, err := s.askV2ByID(ctx, s.deps.Store.Pool, id, include.markOf)
	if err != nil {
		return askV2{}, err
	}
	if err := s.completeAsksV2(ctx, r, rows, include.thread, true); err != nil {
		return askV2{}, err
	}
	return rows[0], nil
}

// writtenAskRowsV2 reads on q what the answer of a write of the ask id names reads from the
// tables: its row (askV2ByID) and what every list of asks batches beside it (loadAskDetails),
// its inbound backlink count as who counts it, for completeWrittenAskV2 to complete. A write of
// the ask reads them on its own transaction before it commits (writtenAskV2), so a read that
// fails rolls the write back.
func (s *server) writtenAskRowsV2(ctx context.Context, q queryer, who model.Actor, id string) ([]askV2, error) {
	rows, err := s.askV2ByID(ctx, q, id, "")
	if err != nil {
		return nil, err
	}
	if err := loadAskDetails(ctx, q, who, askV2Pointers(rows)); err != nil {
		return nil, err
	}
	return rows, nil
}

// writtenAskV2 is the ask a /api/v2 write answers, read in two steps: readBack, the write's
// askReadBack, reads its tables on q (writtenAskRowsV2): the write's transaction, before it
// commits, or the pool for writeUnchanged's no-op. answer then completes the row, after the write
// commits, or at once for writeUnchanged's no-op (completeWrittenAskV2). reader is the caller's
// credential (requestReader), which newWrittenAskV2 resolves when the route makes one, since q
// holds no request.
type writtenAskV2 struct {
	server *server
	reader model.Actor
	rows   []askV2
}

// newWrittenAskV2 is the writtenAskV2 of a /api/v2 request that answers an ask, read as the
// request's caller.
func (s *server) newWrittenAskV2(r *http.Request) *writtenAskV2 {
	return &writtenAskV2{server: s, reader: s.requestReader(r)}
}

// readBack reads the rows of the ask id names on q: the write's transaction, or the pool for
// writeUnchanged's no-op. It reads them as the caller (reader), so the ask's inbound backlink
// count is the caller's: a write, the no-op that answers in its place (writeUnchanged) and GET
// /api/v2/asks/{id} count as the one person whose request they answer, and a route answers one
// count of an ask whether it wrote or not.
func (written *writtenAskV2) readBack(ctx context.Context, q queryer, id string) (err error) {
	written.rows, err = written.server.writtenAskRowsV2(ctx, q, written.reader, id)
	return err
}

// answer is the ask the write answers, after the write commits, or at once for writeUnchanged's
// no-op.
func (written *writtenAskV2) answer(r *http.Request) (askWriteV2, error) {
	return written.server.completeWrittenAskV2(r.Context(), r, written.rows)
}

// write answers the write with its ask row (answer), at status.
func (written *writtenAskV2) write(w http.ResponseWriter, r *http.Request, status int) {
	row, err := written.answer(r)
	if err != nil {
		written.server.writeHandlerError(w, r, err)
		return
	}
	WriteJSON(w, status, row)
}

// writeUnchanged answers a request that wrote nothing to the ask id names - a state the caller
// set already (putChangesNothing) - with the ask as a write of it answers it, read back as the
// same caller on the pool, at status. A request that writes reads its answer on its own
// transaction instead (readBack).
func (written *writtenAskV2) writeUnchanged(w http.ResponseWriter, r *http.Request, status int, id string) {
	if err := written.readBack(r.Context(), written.server.deps.Store.Pool, id); err != nil {
		written.server.writeHandlerError(w, r, err)
		return
	}
	written.write(w, r, status)
}

// requireAskV2 answers whether the ask id names exists, and where it does not writes 404
// ASK_NOT_FOUND (askNotFoundV2, requireHeldV2).
func (s *server) requireAskV2(w http.ResponseWriter, r *http.Request, id string) bool {
	missing := askNotFoundV2(id)
	return s.requireHeldV2(w, r, id, "ask", `select exists (select 1 from asks where id = $1)`,
		missing.code, missing.message)
}

// requireIssueV2 answers whether the issue key names exists, and where it does not writes 404
// ISSUE_NOT_FOUND (dispatch://INFRA-4071/spec R7).
func (s *server) requireIssueV2(w http.ResponseWriter, r *http.Request, key string) bool {
	var held bool
	if err := s.deps.Store.Pool.QueryRow(r.Context(), `select exists (select 1 from issues where key = $1)`, key).Scan(&held); err != nil {
		s.writeHandlerError(w, r, fmt.Errorf("read whether the issue exists: %w", err))
		return false
	}
	if !held {
		s.writeHandlerError(w, r, errorf(apicode.IssueNotFound, "no issue %s", key))
	}
	return held
}

// askListOrder is the ORDER BY of a sort of GET /api/v2/asks. priority is the Inbox's order
// (listInbox): the issue's priority, unset last, then whose turn it is, a person's before an
// agent's ('human' sorts after 'agent', so descending), then the latest activity, newest first;
// a sort descending after a - reverses every key of it.
func askListOrder(sort querySort) string {
	switch {
	case sort.Field == "created_at" && sort.Descending:
		return "a.created_at desc, a.id desc"
	case sort.Field == "created_at":
		return "a.created_at asc, a.id asc"
	case sort.Descending:
		return "i.priority desc nulls first, waiting_on asc, coalesce(lr.created_at, a.created_at) asc, a.id asc"
	}
	return "i.priority asc nulls last, waiting_on desc, coalesce(lr.created_at, a.created_at) desc, a.id desc"
}

// askStateCondition is the condition of an ask list's state, the state written in as a literal so
// the partial index an open read can use stays eligible under a generic plan (asks.go,
// listIssueAsksQuery*). An open ask on the list across projects is one whose issue is open too, as
// the Inbox and GET /api/v1/asks/open read it; acrossProjects says which list it is.
func askStateCondition(state string, acrossProjects bool) string {
	switch state {
	case "open":
		if acrossProjects {
			return "a.state = 'open' and (i.key is null or i.closed_at is null)"
		}
		return "a.state = 'open'"
	case "answered":
		return "a.state = 'answered'"
	case "resolved":
		return "a.state = 'resolved'"
	}
	return ""
}

// askSource reads source=<kind>:<id> into the kind and id of a reference source, the kinds
// refs.ScopeOf reads (questionSourceKinds), the id a whole uuid or an id written short
// (text.IsIDPrefix): any other value names no source, and is refused as a malformed query.
func askSource(source string) (kind, id string, err error) {
	kind, id, found := strings.Cut(source, ":")
	id = strings.TrimSpace(id)
	_, notUUID := uuid.Parse(id)
	if !found || !slices.Contains(questionSourceKinds, kind) || (notUUID != nil && !text.IsIDPrefix(id)) {
		return "", "", errorf(apicode.InvalidQuery,
			"source is <kind>:<id>, the kind one of %s and the id a whole uuid or a prefix of one", strings.Join(questionSourceKinds, ", "))
	}
	return kind, id, nil
}

// listAsksV2 answers GET /api/v2/asks: the asks the filters keep, across projects, one page of
// them and the counts beside it, read in one snapshot; with a source, the question map of that
// source's document, one row per question block, paged in memory, since one document's questions
// are few.
func (s *server) listAsksV2(w http.ResponseWriter, r *http.Request) {
	actor, human, err := s.optionalActor(r)
	if err != nil {
		s.writeAuthenticationError(w, r, err)
		return
	}
	query := v2Query[askListV2Query](r)
	include, ok := s.askIncludeOf(w, r, query.Include)
	if !ok {
		return
	}
	read := askV2Read{order: askListOrder(query.Sort), markOf: include.markOf}
	state := query.State
	var questions *referenceQuestions
	if query.Source != "" {
		kind, id, err := askSource(query.Source)
		if err != nil {
			s.writeHandlerError(w, r, err)
			return
		}
		answer, ok := s.readQuestionMap(w, r, actor.Recorded(), kind, id, human)
		if !ok {
			return
		}
		questions = &answer
		ids := make([]string, 0, len(answer.Questions))
		for _, question := range answer.Questions {
			if !slices.Contains(ids, question.Ask) {
				ids = append(ids, question.Ask)
			}
		}
		read.where("a.id = any(" + read.argument(ids) + "::uuid[])")
		state = cmp.Or(state, "all")
	}
	if condition := askStateCondition(cmp.Or(state, "open"), true); condition != "" {
		read.where(condition)
	}
	if len(query.Project) > 0 {
		read.where("coalesce(i.project_key, ar.project_key) = any(" + read.argument(query.Project) + "::text[])")
	}
	// The person each ask waits on, three steps resolved as it is read (askedOfPersonExpression),
	// as the Inbox's ?assignee= reads it (inboxAssigneeFilter); every login is resolved before the
	// snapshot below opens, since the pool gives a caller holding a connection no second one.
	switch waitingOn := query.WaitingOn; {
	case waitingOn.None:
		read.where(askedOfPersonExpression + " is null")
	case waitingOn.Value == "":
	case waitingOn.Value == "me":
		person, ok := s.actingPerson(w, r)
		if !ok {
			return
		}
		read.where(askedOfPersonExpression + " = " + read.argument(person.ID))
	default:
		login, err := s.allowedLogin(r.Context(), waitingOn.Value)
		if err != nil {
			s.writeHandlerError(w, r, err)
			return
		}
		read.where(askedOfPersonExpression + " = " + read.argument(login))
	}
	if query.AuthorSession != "" {
		read.where("a.author->>'kind' = 'session' and a.author->>'id' = " + read.argument(query.AuthorSession))
	}
	if query.From != nil {
		read.where("a.created_at >= " + read.argument(*query.From))
	}
	if query.To != nil {
		read.where("a.created_at < " + read.argument(*query.To))
	}

	answer := askListV2{}
	var rows []askV2
	var asOf time.Time
	err = pgx.BeginTxFunc(r.Context(), s.deps.Store.Pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		if err := tx.QueryRow(r.Context(), `select statement_timestamp()`).Scan(&asOf); err != nil {
			return fmt.Errorf("read the asks' snapshot time: %w", err)
		}
		if questions != nil {
			var err error
			rows, err = s.askV2Rows(r.Context(), tx, read, nil)
			return err
		}
		if err := tx.QueryRow(r.Context(), `select count(*),
				count(*) filter (where a.state = 'open' and `+waitingOnExpression+` = 'human'),
				count(*) filter (where a.state = 'open' and `+waitingOnExpression+` = 'agent')
			`+askReadFrom+`
			where `+read.clause(), read.arguments...).Scan(&answer.Count, &answer.WaitingOnHuman, &answer.WaitingOnAgent); err != nil {
			return fmt.Errorf("count the asks: %w", err)
		}
		var err error
		rows, err = s.askV2Rows(r.Context(), tx, read, &query.pageQuery)
		return err
	})
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	if err := s.completeAsksV2(r.Context(), r, rows, include.thread, false); err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	answer.AsOf = timestampValue(asOf)
	if questions == nil {
		answer.page = newPage(rows, answer.Count, query.pageQuery)
		WriteJSON(w, http.StatusOK, answer)
		return
	}
	// One row per question block, in the order the asks were read and each ask's blocks in block
	// order (the question map's), so a copy of an ask in a second block is a row of its own.
	blocks := map[string][]string{}
	for _, question := range questions.Questions {
		blocks[question.Ask] = append(blocks[question.Ask], question.BlockID)
	}
	items := []askV2{}
	for _, row := range rows {
		for _, block := range blocks[row.ID] {
			item := row
			item.QuestionBlockID = block
			items = append(items, item)
			if item.State == "open" {
				switch item.WaitingOn {
				case "human":
					answer.WaitingOnHuman++
				case "agent":
					answer.WaitingOnAgent++
				}
			}
		}
	}
	answer.page = pageOf(items, query.pageQuery)
	answer.Count = answer.Total
	answer.Document = &questions.Document
	WriteJSON(w, http.StatusOK, answer)
}

// getAskV2 answers GET /api/v2/asks/{id}: the one ask, its thread with include=thread.
func (s *server) getAskV2(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	id := r.PathValue("id")
	if !s.requireAskV2(w, r, id) {
		return
	}
	include, ok := s.askIncludeOf(w, r, v2Query[askReadV2Query](r).Include)
	if !ok {
		return
	}
	row, err := s.readAskV2(r.Context(), r, id, include)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, row)
}

// editAskV2 answers PATCH /api/v2/asks/{id}: a reword (rewordAsk, PATCH /api/v1/asks/{id}'s
// work) or a hand-on (handAskOn, PUT /api/v1/asks/{id}/asked-of's), which the row's authority
// holds to the person scope, answered with the ask read on the write's transaction
// (writtenAskV2).
func (s *server) editAskV2(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	id := r.PathValue("id")
	if !s.requireAskV2(w, r, id) {
		return
	}
	input := v2Body[editAskV2Input](r)
	written := s.newWrittenAskV2(r)
	reword := input.Question != nil || input.Options != nil || input.Multiple != nil || input.Urgency != nil
	switch {
	case input.AskedOf != nil && reword:
		// Two writes of two transactions under one key could leave the first standing where the
		// second is refused, so a request makes one of them.
		writeError(w, r, apicode.InvalidAsk, "a PATCH of an ask rewords it or hands it on (asked_of), not both; send them as two requests")
		return
	case input.AskedOf != nil:
		actor, ok := s.actingPerson(w, r)
		if !ok {
			return
		}
		if _, ok := s.handAskOn(w, r, actor, id, *input.AskedOf, written.readBack); !ok {
			return
		}
	default:
		if _, ok := s.rewordAsk(w, r, input.editAskInput, written.readBack); !ok {
			return
		}
	}
	written.write(w, r, http.StatusOK)
}

// putAskStateV2 answers PUT /api/v2/asks/{id}/state: each state's arm runs its /api/v1 route's
// work through the one transition engine (closeAsk), once the no-op rule of
// dispatch://INFRA-4071/spec R8 has found the body is a change, answered with the ask read on the
// write's transaction (writtenAskV2). An equal state from the caller who set it answers the ask
// and writes nothing (putChangesNothing): the answer by the person who gave it, the resolution or
// the decline by the actor who wrote it. An approval request's answer from a credential that does
// not hold approve is refused before that compare, as every answer of its is (approvalAnswerRefusal).
func (s *server) putAskStateV2(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	id := r.PathValue("id")
	if !s.requireAskV2(w, r, id) {
		return
	}
	ask, err := s.loadAsk(r.Context(), s.deps.Store.Pool, id)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	written := s.newWrittenAskV2(r)
	switch body := v2BodyArm(r).(type) {
	case answerAskStateInput:
		actor, ok := s.actingPerson(w, r)
		if !ok {
			return
		}
		if ask.State == "answered" && ask.Answer != nil {
			// Before the compare, as recordAnswer holds every answer to it: an answer actor may not
			// make is refused, never answered as it stands by a no-op, and an approve-only token's
			// answer of an ask that is no approval request is 403 APPROVE_ONLY_TOKEN. A request that
			// carries an Idempotency-Key was refused before this, ahead of its key
			// (gateApproveOnlyAskAnswer); this is the same rule for the request that carries none.
			if err := approveOnlyAnswerRefusal(approveOnlyRequest(r), ask.Kind); err != nil {
				s.writeHandlerError(w, r, err)
				return
			}
			if err := approvalAnswerRefusal(ask, actor); err != nil {
				s.writeHandlerError(w, r, err)
				return
			}
			sent, current := body, body
			if sent.Text != nil {
				lined := pmdoc.LineFeeds(*sent.Text)
				sent.Text = &lined
			}
			current.Selected, current.Text = ask.Answer.Selected, ask.Answer.Text
			if putChangesNothing(sent, current, setBy(model.CanonicalLogin(ask.Answer.User)), actor.ID) {
				written.writeUnchanged(w, r, http.StatusOK, id)
				return
			}
		}
		if _, ok := s.recordAnswer(w, r, actor, body.answerAskInput, written.readBack); !ok {
			return
		}
	case resolveAskStateInput:
		actor, ok := s.requireActor(w, r, body.Actor)
		if !ok {
			return
		}
		if resolution := ask.Resolution; ask.State == "resolved" && resolution != nil && resolution.Kind == body.State {
			sent, current := body, body
			sent.Reason, current.Reason = strings.TrimSpace(sent.Reason), resolution.Reason
			if putChangesNothing(sent, current, setBy(actorIdentity(resolution.Actor)), actorIdentity(actor)) {
				written.writeUnchanged(w, r, http.StatusOK, id)
				return
			}
		}
		if _, ok := s.recordResolution(w, r, actor, body.State, body.Reason, written.readBack); !ok {
			return
		}
	case declineAskStateInput:
		actor, ok := s.actingPerson(w, r)
		if !ok {
			return
		}
		if resolution := ask.Resolution; ask.State == "resolved" && resolution != nil && resolution.Kind == model.AskResolutionDeclined {
			note := ""
			if body.Reason != nil {
				note = strings.TrimSpace(*body.Reason)
			}
			sent, current := body, body
			sent.Reason, current.Reason = &note, &resolution.Reason
			if putChangesNothing(sent, current, setBy(actorIdentity(resolution.Actor)), actorIdentity(actor)) {
				written.writeUnchanged(w, r, http.StatusOK, id)
				return
			}
		}
		if _, ok := s.recordDecline(w, r, actor, declineAskInput{Note: body.Reason, Actor: body.Actor}, written.readBack); !ok {
			return
		}
	default:
		s.writeHandlerError(w, r, fmt.Errorf("PUT %s: the state body was decoded as %T, which no state of an ask is", r.URL.Path, body))
		return
	}
	written.write(w, r, http.StatusOK)
}

// actorIdentity is the writer an actor names, as putChangesNothing compares a setter with a
// caller: a person by their canonical login, a session by its id, each under its kind.
func actorIdentity(actor model.Actor) string {
	if actor.Kind == "user" {
		return "user:" + model.CanonicalLogin(actor.ID)
	}
	return actor.Kind + ":" + actor.ID
}

// changeAskFollowerV2 is the follower write both /api/v2 follower routes make, as /api/v1's
// (changeAskFollower) make it, once the path's ask is held to exist: the session named in the path
// put on or taken off the ask's followers, recorded as who the credential acts for, with readBack,
// where it is given, reading what the route answers of the ask and the session on the write's own
// transaction before it commits (writeAskFollower). It writes a refusal itself and answers false.
func (s *server) changeAskFollowerV2(
	w http.ResponseWriter, r *http.Request, follow bool, readBack func(ctx context.Context, q queryer, askID, sessionID string) error,
) bool {
	actor, human, err := s.optionalActor(r)
	if err != nil {
		s.writeAuthenticationError(w, r, err)
		return false
	}
	askID := r.PathValue("id")
	if !s.requireAskV2(w, r, askID) {
		return false
	}
	sessionID := strings.TrimSpace(r.PathValue("session_id"))
	if sessionID == "" {
		writeError(w, r, apicode.FollowerInput, "follower session id is required")
		return false
	}
	// A person at a browser writes as themselves (changeAskFollower); every other credential acts
	// as the addressed session, or as a person on another, as its body and scopes say.
	actor = actor.Recorded()
	if !human {
		var ok bool
		if actor, ok = s.askFollowerActor(w, r, actor, s.callerHoldsScope(r, model.ScopePerson), sessionID, v2Body[actorInput](r).Actor); !ok {
			return false
		}
	}
	var back func(ctx context.Context, q queryer) error
	if readBack != nil {
		back = func(ctx context.Context, q queryer) error { return readBack(ctx, q, askID, sessionID) }
	}
	return s.writeAskFollower(w, r, actor, askID, sessionID, follow, back)
}

// followAskV2 answers PUT /api/v2/asks/{id}/followers/{session_id}: 200 with the follower, which
// /api/v1 answers 204, a follow of a session already following changing nothing
// (dispatch://INFRA-4071/spec R8: a follower records no setter, so any caller's equal PUT is a
// no-op). The follower is read on the follow's own transaction, before it commits, as every other
// /api/v2 ask write reads what it answers (askReadBack).
func (s *server) followAskV2(w http.ResponseWriter, r *http.Request) {
	var answer model.AskFollower
	if !s.changeAskFollowerV2(w, r, true, func(ctx context.Context, q queryer, askID, sessionID string) error {
		followers, err := asks.Followers(ctx, q, askID)
		if err != nil {
			return err
		}
		for _, follower := range followers {
			if follower.SessionID == sessionID {
				answer = follower
				return nil
			}
		}
		return fmt.Errorf("session %s follows ask %s, and its read of the followers finds no such row", sessionID, askID)
	}) {
		return
	}
	WriteJSON(w, http.StatusOK, answer)
}

// unfollowAskV2 answers DELETE /api/v2/asks/{id}/followers/{session_id}: 204.
func (s *server) unfollowAskV2(w http.ResponseWriter, r *http.Request) {
	if !s.changeAskFollowerV2(w, r, false, nil) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// listIssueAsksV2 answers GET /api/v2/issues/{key}/asks: an issue's asks, oldest first, one page
// of them.
func (s *server) listIssueAsksV2(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	key := r.PathValue("key")
	if !s.requireIssueV2(w, r, key) {
		return
	}
	query := v2Query[issueAsksV2Query](r)
	include, ok := s.askIncludeOf(w, r, query.Include)
	if !ok {
		return
	}
	read := askV2Read{order: "a.created_at, a.id", markOf: include.markOf}
	read.where("a.issue_key = " + read.argument(key))
	if condition := askStateCondition(query.State, false); condition != "" {
		read.where(condition)
	}
	// The count reads the ask's own columns alone, so it reads them through no join.
	var total int
	if err := s.deps.Store.Pool.QueryRow(r.Context(), `select count(*) from asks a where `+read.clause(), read.arguments...).Scan(&total); err != nil {
		s.writeHandlerError(w, r, fmt.Errorf("count the issue's asks: %w", err))
		return
	}
	rows, err := s.askV2Rows(r.Context(), s.deps.Store.Pool, read, &query.pageQuery)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	if err := s.completeAsksV2(r.Context(), r, rows, include.thread, false); err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, newPage(rows, total, query.pageQuery))
}

// createIssueAskV2 answers POST /api/v2/issues/{key}/asks: the ask POST /api/v1/issues/{key}/asks
// opens (openAsk), answered as the ask row, read on the write's transaction (writtenAskV2), with
// the write's advice beside it.
func (s *server) createIssueAskV2(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	key := r.PathValue("key")
	if !s.requireIssueV2(w, r, key) {
		return
	}
	written := s.newWrittenAskV2(r)
	_, advice, suggestions, ok := s.openAsk(w, r, issueOwner(key), v2Body[createAskInput](r), written.readBack)
	if !ok {
		return
	}
	row, err := written.answer(r)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusCreated, withSuggestions(row, advice, suggestions))
}
