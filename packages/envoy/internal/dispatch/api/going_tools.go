package api

// going_tools.go is the table of the dispatch tools a command-bar run reads and writes Dispatch
// through, one entry per tool spec of packages/contracts/src/dispatch-tools.ts, by the tool's own
// name: goingWriteTools, the tools that change something, each with the rows of the route table
// it writes, how a call becomes what each row's route takes, how its item is read before and
// after the write, how the write is undone, the class of change it is and the event its route
// appends; goingReadTools, the tools that change nothing, each with the rows it reads; and
// goingExcludedRoutes, the routes a person may call that no tool calls, each with its reason.
// TestEveryPersonRouteHasATool holds the three to the route table: every route a person's
// credential passes is a row of a tool or an excluded route.
//
// A tool whose spec binds its routes (contracts.DispatchTool.Routes) names no rows here: its rows
// are its bindings, each translated by goingTranslateBound, so the binding is written once, in the
// spec, and serves the `dispatch` command and this server alike (goingToolRows).

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/sjawhar/envoy/going"
	"github.com/sjawhar/envoy/internal/contracts"
)

// goingToolRoute is one row of the server's table a tool calls: how a call fills the row's path,
// the argument whose value picks the row where a tool calls two that differ by what it was asked
// to do, the graph relations a write of it makes, what the row's route takes of a call's
// arguments, and the rows a write's undo writes through.
type goingToolRoute struct {
	Method  string
	Pattern string
	// Path binds each placeholder of Pattern to the tool argument that fills it, or to
	// goingCallerSession. Every placeholder is bound and every binding is a placeholder, which
	// buildGoingCatalogue holds the table to.
	Path map[string]string
	// WhenArgument and WhenValue pick this row among the tool's: a claim and its release are
	// one tool and two rows, told apart by `release`, as a follow and its unfollow are by
	// `action`. WhenValue goingAnID picks the row for a call whose WhenArgument is a whole id, as
	// an edit of a document named by id is told apart from one named by slug. An empty
	// WhenArgument is a row nothing guards.
	WhenArgument string
	WhenValue    any
	// Relations are the graph relations this row's write makes, each a structural edge kind
	// (model.GraphEdgeKinds) or a `block[]` attribute of a typed block (pmdoc.LinkKinds).
	Relations []string
	// Translate turns a call's arguments into what this row's route takes, as the route stands
	// (going_translate.go): a document named by slug resolved to the id its path takes, a
	// session named by title to its id, an argument the route reads under another name moved
	// there. Every write row has one, which buildGoingCatalogue holds the table to.
	Translate goingTranslator
	// UndoRoutes are the rows of the table an undo of this row's write writes through, none for
	// a write that is not undone (goingWriteTool.Undo).
	UndoRoutes []string
}

// goingIDGuard is the type of goingAnID.
type goingIDGuard struct{}

// goingAnID, as a row's WhenValue, picks the row for a call whose WhenArgument is a whole id
// (goingIsID) rather than a name.
var goingAnID goingIDGuard

// String is how a refusal that names the row's guard says it (goingNoRouteReason).
func (goingIDGuard) String() string { return "a whole id" }

// The routes the undos write through beside the catalogue's own rows, named once each, and the
// three an upload writes through, whose owner a write reads before it (goingRouteUploadOwner).
const (
	goingIssueMessageRoute  = http.MethodPost + " /api/v1/issues/{key}/messages"
	goingIssuePatchRoute    = http.MethodPatch + " /api/v1/issues/{key}"
	goingClaimRoute         = http.MethodPost + " /api/v1/issues/{key}/claim"
	goingReleaseRoute       = http.MethodDelete + " /api/v1/issues/{key}/claim"
	goingAskResolveRoute    = http.MethodPost + " /api/v1/asks/{id}/resolve"
	goingAskEditRoute       = http.MethodPatch + " /api/v1/asks/{id}"
	goingCommentReopenRoute = http.MethodPost + " /api/v1/comments/{id}/reopen"
	goingCommentResolve     = http.MethodPost + " /api/v1/comments/{id}/resolve"
	goingCommentRejectRoute = http.MethodPost + " /api/v1/comments/{id}/reject"
	goingCommentEditRoute   = http.MethodPatch + " /api/v1/comments/{id}"
	goingMessageEditRoute   = http.MethodPatch + " /api/v1/messages/{id}"
	goingFollowRoute        = http.MethodPut + " /api/v1/asks/{id}/followers/{session_id}"
	goingUnfollowRoute      = http.MethodDelete + " /api/v1/asks/{id}/followers/{session_id}"
	goingIssueUploadRoute   = http.MethodPost + " /api/v1/issues/{key}/artifacts"
	goingProjectUploadRoute = http.MethodPost + " /api/v1/projects/{key}/artifacts"
	goingAgentUploadRoute   = http.MethodPost + " /api/v1/agents/{session_id}/artifacts"
)

// goingUploadNames is the argument of each action that names the document its upload writes, by
// action: an upload whose name is the name or the slug of a document its owner already holds is
// that document's next version (artifactByNameOrSlugSQL), so a plan's row of such an action reads
// which document its name finds before anything of it is sent (rowUpload).
var goingUploadNames = map[string]string{"dispatch_artifact": "name"}

// goingRouteUploadOwner is the owner an upload's route writes to, resolved from the values of its
// path as that route resolves them: the issue (uploadArtifact), the project
// (uploadProjectArtifact) or the session (uploadAgentArtifact), each value exactly as the route
// reads it; false for a route that is no upload, or a value it lacks.
func goingRouteUploadOwner(route GoingRoute, values map[string]string) (artifactTarget, bool) {
	switch route.Method + " " + route.Pattern {
	case goingIssueUploadRoute:
		issue := values["key"]
		return artifactTarget{IssueKey: &issue}, issue != ""
	case goingProjectUploadRoute:
		return artifactTarget{Project: values["key"]}, values["key"] != ""
	case goingAgentUploadRoute:
		return artifactTarget{Session: values["session_id"]}, values["session_id"] != ""
	}
	return artifactTarget{}, false
}

// goingUndoKind is one of the four ways a write is undone.
type goingUndoKind string

const (
	// goingUndoReverse runs the write's reverse, a route of the table whether or not a tool
	// calls it: a claim is released, a follow unfollowed, a resolved comment reopened and a
	// reopened one resolved again, an ask the run opened retracted, a suggestion the run posted
	// rejected while it is still open.
	goingUndoReverse goingUndoKind = "reverse"
	// goingUndoWriteBack writes the value the write found back over it: an issue's fields, an
	// ask's text, a document's content, a comment's or an issue message's body.
	goingUndoWriteBack goingUndoKind = "write_back"
	// goingUndoCloseCreated closes an issue the run created, posting nothing on it.
	goingUndoCloseCreated goingUndoKind = "close_created"
	// goingUndoNotUndone is a write that stays as it is, named "sent, not undone": one that
	// reached a person or a session at once (a message, a broadcast, an answer, a review), or one
	// Dispatch serves no reverse of and whose value before no read keeps. A delete of a comment or
	// an issue message is named "deleted, not undone": nothing restores a deleted post. Each table
	// entry of this kind says which, beside it.
	goingUndoNotUndone goingUndoKind = "not_undone"
)

// goingWriteClass is the kind of change a write tool makes, which decides what waits for the
// person's approval.
type goingWriteClass string

const (
	goingClassField       goingWriteClass = "field"
	goingClassLink        goingWriteClass = "link"
	goingClassClaim       goingWriteClass = "claim"
	goingClassCreatePlain goingWriteClass = "create_plain"
	goingClassCreateText  goingWriteClass = "create_text"
	goingClassText        goingWriteClass = "text"
	goingClassSuggestion  goingWriteClass = "suggestion"
	goingClassMessage     goingWriteClass = "message"
	goingClassBroadcast   goingWriteClass = "broadcast"
	goingClassReview      goingWriteClass = "review"
	goingClassAskAnswer   goingWriteClass = "ask_answer"
	goingClassDecision    goingWriteClass = "decision"
	goingClassResult      goingWriteClass = "result"
	goingClassSetting     goingWriteClass = "setting"
)

// goingWriteClasses is every class a write tool may name.
var goingWriteClasses = []goingWriteClass{
	goingClassField, goingClassLink, goingClassClaim, goingClassCreatePlain, goingClassCreateText,
	goingClassText, goingClassSuggestion, goingClassMessage, goingClassBroadcast, goingClassReview,
	goingClassAskAnswer, goingClassDecision, goingClassResult, goingClassSetting,
}

// goingNoEvent is the event of a write tool whose route appends no event: a deployment setting
// the outbox never publishes, a person's own read mark.
const goingNoEvent = "none"

// goingObserve reads what a write of one tool changes: before it (answer nil) and after it, where
// answer is what the write's route answered, status its status and before what the read before
// saw (goingObserver.observe).
type goingObserve func(o goingObserver, ctx context.Context, prepared goingPrepared, answer []byte, status int, before goingObservation) (goingObservation, error)

// goingWriteTool is one tool that changes something.
type goingWriteTool struct {
	// Routes are the tool's rows, tried in order: the row a call performs is the first whose
	// guard it satisfies and whose placeholders it fills. Nil for a tool whose spec binds its
	// routes, whose rows are those bindings (goingToolRows).
	Routes []goingToolRoute
	// Observe reads the tool's item before and after its write; nil reads nothing before a
	// write that makes an item and the made item's id after it (goingObserver.made).
	Observe goingObserve
	// Undo is how a write of the tool is undone. UndoRoutes are the rows a bound tool's undo
	// writes through; a tool that names its own rows names them on each row.
	Undo       goingUndoKind
	UndoRoutes []string
	// Class is the kind of change the write is.
	Class goingWriteClass
	// Event is the event type (model.Event.Type) the tool's route appends, goingNoEvent for a
	// route that appends none.
	Event string
	// Proposed is a tool the answer run's decision offers (going_decision.go), the actions of the
	// catalogue it serves at GET /api/v1/going/catalogue: going/decision_models.json's measurement
	// was made over exactly these.
	Proposed bool
}

// goingReadTool is one tool that changes nothing: the rows it reads. A tool whose spec binds its
// routes names none, and its rows are those bindings (goingToolRows). A row whose Path is nil is
// one the tool reads in process rather than by a call's arguments: the answer run's own reads.
type goingReadTool struct {
	Routes []goingToolRoute
	// Pick is the row of Routes a call reads, by "<METHOD> <pattern>", and what that row's route
	// takes of the call, for a tool whose command reads the item its arguments name rather than
	// the first of its rows they fill: an item named by a reference, above all. serverURL is the
	// server's own address, under which a dashboard URL names an item. Nil for a tool whose call
	// reads its first row whose guard the arguments satisfy and whose placeholders they fill
	// (goingReadCall).
	Pick func(arguments map[string]any, serverURL string) (string, goingCall, error)
	// Choose is the row of Routes a call is sent to, and what that row's route takes of it, for a
	// tool whose row hangs on what the item its arguments name is, which Choose reads through
	// read first: dispatch_doc_read reads an uploaded file at one of its versions where it reads
	// a document's text (goingChooseDocRead). Pick names the row such a call reads with no read
	// made (GoingReadRoute). Nil for a tool whose row its arguments pick alone.
	Choose func(ctx context.Context, read goingReader, arguments map[string]any, serverURL string) (string, goingCall, error)
}

// goingToolRows is the rows of the tool spec names: the rows its entry names, or, for a spec that
// binds its routes, one row per binding, each translated by goingTranslateBound and undone
// through undo. ok is false where the two disagree: an entry naming rows for a bound spec, which
// would state its routes twice, or none for a spec that binds none.
func goingToolRows(spec contracts.DispatchTool, named []goingToolRoute, undo []string) (rows []goingToolRoute, ok bool) {
	if len(spec.Routes) == 0 {
		return named, len(named) > 0
	}
	if len(named) > 0 {
		return nil, false
	}
	rows = make([]goingToolRoute, 0, len(spec.Routes))
	for _, binding := range spec.Routes {
		path := binding.Path
		if path == nil {
			path = map[string]string{}
		}
		rows = append(rows, goingToolRoute{
			Method: binding.Method, Pattern: binding.Pattern, Path: path,
			Translate: goingTranslateBound(binding), UndoRoutes: undo,
		})
	}
	return rows, true
}

// goingTranslateBound is the translation of a row a spec binds: the path as chooseGoingRoute
// filled it, each argument the binding sends in the query under its own name, a list sent as one
// parameter per item, and, for every method but GET, a JSON body of each argument the binding
// sends in it, under its own name, with the session the caller acts as in its actor where it
// acts as one, as the `dispatch` command's generic command sends it.
func goingTranslateBound(binding contracts.ToolRoute) goingTranslator {
	return func(ctx context.Context, read goingReader, call goingCallInput) (goingCall, error) {
		bound := goingCall{path: call.path}
		// Keep the resolved id in the prepared path used for the write and its landing.
		if binding.Method == http.MethodPost && binding.Pattern == "/api/v1/comments/{id}/reopen" {
			id, err := goingPostID(ctx, read, "comment", call.path["id"])
			if err != nil {
				return goingCall{}, err
			}
			bound.path = map[string]string{"id": id}
		}
		for _, name := range binding.Query {
			value, named := call.arguments[name]
			if !named || value == nil {
				continue
			}
			if bound.query == nil {
				bound.query = url.Values{}
			}
			items, many := value.([]any)
			if !many {
				items = []any{value}
			}
			for _, item := range items {
				text, isText := goingArgumentText(item)
				if !isText {
					return goingCall{}, goingUntranslatable(fmt.Sprintf("%s is sent in the query, which takes no %T", name, item))
				}
				bound.query.Add(name, text)
			}
		}
		if binding.Method != http.MethodGet {
			bound.body = goingPick(call.arguments, binding.Body...)
			if call.session != "" {
				bound.body["actor"] = goingActorSession(call.session)
			}
		}
		return bound, nil
	}
}

// goingArgumentText is an argument as a path segment or a query parameter carries it: a text as
// it is, a number in its shortest decimal form, and a boolean as true or false, the words every
// route that reads one takes. Any other value has none.
func goingArgumentText(value any) (string, bool) {
	switch value := value.(type) {
	case string:
		return value, true
	case float64:
		return strconv.FormatFloat(value, 'f', -1, 64), true
	case json.Number:
		return value.String(), true
	case int:
		return strconv.Itoa(value), true
	case bool:
		return strconv.FormatBool(value), true
	}
	return "", false
}

// goingWriteTools is every dispatch tool that changes something, by the tool's own name.
//
// The rows of a tool that names its own are tried in order, so the row a call performs is the
// first whose guard it satisfies and whose placeholders it fills: a document write names its
// issue, else its project, else the document itself.
var goingWriteTools = map[string]goingWriteTool{
	// A created issue is closed by its undo, with nothing posted on it.
	"dispatch_issue": {
		Routes: []goingToolRoute{{
			Method: http.MethodPost, Pattern: "/api/v1/issues", Path: map[string]string{},
			Relations:  []string{"child_of", "blocked_by", "affects", "mentions"},
			Translate:  goingArgumentsAsBody,
			UndoRoutes: []string{goingIssuePatchRoute},
		}},
		Observe: goingObserver.issueCreated, Undo: goingUndoCloseCreated,
		Class: goingClassCreatePlain, Event: "issue.created", Proposed: true,
	},
	"dispatch_issue_update": {
		Routes: []goingToolRoute{{
			Method: http.MethodPatch, Pattern: "/api/v1/issues/{key}",
			Path:       map[string]string{"key": "issue"},
			Relations:  []string{"child_of", "blocked_by", "affects"},
			Translate:  goingTranslateIssueUpdate,
			UndoRoutes: []string{goingIssuePatchRoute},
		}},
		Observe: goingObserver.issueUpdated, Undo: goingUndoWriteBack,
		Class: goingClassField, Event: "issue.updated", Proposed: true,
	},
	// A release is undone by taking the claim back only where the caller held it before, and a
	// claim by releasing it.
	"dispatch_claim": {
		Routes: []goingToolRoute{
			{
				Method: http.MethodDelete, Pattern: "/api/v1/issues/{key}/claim",
				Path: map[string]string{"key": "issue"}, WhenArgument: "release", WhenValue: true,
				Translate:  goingTranslateClaim,
				UndoRoutes: []string{goingClaimRoute},
			},
			{
				Method: http.MethodPost, Pattern: "/api/v1/issues/{key}/claim",
				Path:       map[string]string{"key": "issue"},
				Translate:  goingTranslateClaim,
				UndoRoutes: []string{goingReleaseRoute},
			},
		},
		Observe: goingObserver.claimed, Undo: goingUndoReverse,
		Class: goingClassClaim, Event: "issue.claimed", Proposed: true,
	},
	// An ask the turn opened is retracted.
	"dispatch_ask": {
		Routes: []goingToolRoute{
			{
				Method: http.MethodPost, Pattern: "/api/v1/issues/{key}/asks",
				Path:       map[string]string{"key": "issue"},
				Relations:  []string{"owned_by", "anchored_to", "followed_by", "mentions"},
				Translate:  goingTranslateIssueAsk,
				UndoRoutes: []string{goingAskResolveRoute},
			},
			{
				Method: http.MethodPost, Pattern: "/api/v1/artifacts/{id}/asks",
				Path:       map[string]string{"id": "artifact"},
				Relations:  []string{"owned_by", "anchored_to", "followed_by", "mentions"},
				Translate:  goingTranslateDocumentAsk,
				UndoRoutes: []string{goingAskResolveRoute},
			},
		},
		Observe: goingObserver.askOpened, Undo: goingUndoReverse,
		Class: goingClassMessage, Event: "ask.opened", Proposed: true,
	},
	"dispatch_edit_ask": {
		Routes: []goingToolRoute{{
			Method: http.MethodPatch, Pattern: "/api/v1/asks/{id}",
			Path: map[string]string{"id": "ask"}, Relations: []string{"mentions"},
			Translate:  goingTranslateEditAsk,
			UndoRoutes: []string{goingAskEditRoute},
		}},
		Observe: goingObserver.askEdited, Undo: goingUndoWriteBack,
		Class: goingClassText, Event: "ask.edited", Proposed: true,
	},
	// A resolved ask stays resolved: Dispatch serves no reopen of an ask.
	"dispatch_resolve_ask": {
		Routes: []goingToolRoute{{
			Method: http.MethodPost, Pattern: "/api/v1/asks/{id}/resolve",
			Path:      map[string]string{"id": "ask"},
			Translate: goingTranslateResolveAsk,
		}},
		Observe: goingObserver.askResolved, Undo: goingUndoNotUndone,
		Class: goingClassAskAnswer, Event: "ask.resolved", Proposed: true,
	},
	// A resolved comment is reopened by the route dispatch_reopen_comment calls.
	"dispatch_resolve_comment": {
		Routes: []goingToolRoute{{
			Method: http.MethodPost, Pattern: "/api/v1/comments/{id}/resolve",
			Path:       map[string]string{"id": "comment"},
			Translate:  goingTranslateResolveComment,
			UndoRoutes: []string{goingCommentReopenRoute},
		}},
		Observe: goingObserver.commentResolvedBy, Undo: goingUndoReverse,
		Class: goingClassField, Event: "comment.resolved", Proposed: true,
	},
	"dispatch_follow": {
		Routes: []goingToolRoute{
			{
				Method: http.MethodDelete, Pattern: "/api/v1/asks/{id}/followers/{session_id}",
				Path:         map[string]string{"id": "ask", "session_id": goingCallerSession},
				WhenArgument: "action", WhenValue: "unfollow",
				Relations:  []string{"followed_by"},
				Translate:  goingTranslateNoBody,
				UndoRoutes: []string{goingFollowRoute},
			},
			{
				Method: http.MethodPut, Pattern: "/api/v1/asks/{id}/followers/{session_id}",
				Path:       map[string]string{"id": "ask", "session_id": goingCallerSession},
				Relations:  []string{"followed_by"},
				Translate:  goingTranslateNoBody,
				UndoRoutes: []string{goingUnfollowRoute},
			},
		},
		Observe: goingObserver.followedBy, Undo: goingUndoReverse,
		Class: goingClassLink, Event: "ask.follower_added", Proposed: true,
	},
	// A comment reaches the people and sessions it mentions when it is posted, so it stays.
	"dispatch_comment": {
		Routes: []goingToolRoute{
			{
				Method: http.MethodPost, Pattern: "/api/v1/issues/{key}/comments",
				Path:      map[string]string{"key": "issue"},
				Relations: []string{"owned_by", "anchored_to", "answers", "replies_to", "mentions"},
				Translate: goingTranslateIssueComment,
			},
			{
				Method: http.MethodPost, Pattern: "/api/v1/artifacts/{id}/comments",
				Path:      map[string]string{"id": "artifact"},
				Relations: []string{"owned_by", "anchored_to", "answers", "replies_to", "mentions"},
				Translate: goingTranslateDocumentComment,
			},
		},
		Undo: goingUndoNotUndone, Class: goingClassText, Event: "comment.created", Proposed: true,
	},
	// A suggestion the run posted is rejected by its undo while it is still open.
	"dispatch_suggest": {
		Routes: []goingToolRoute{
			{
				Method: http.MethodPost, Pattern: "/api/v1/issues/{key}/comments",
				Path:       map[string]string{"key": "issue"},
				Relations:  []string{"owned_by", "anchored_to", "mentions"},
				Translate:  goingTranslateIssueSuggest,
				UndoRoutes: []string{goingCommentRejectRoute},
			},
			{
				Method: http.MethodPost, Pattern: "/api/v1/artifacts/{id}/comments",
				Path:       map[string]string{"id": "artifact"},
				Relations:  []string{"owned_by", "anchored_to", "mentions"},
				Translate:  goingTranslateDocumentSuggest,
				UndoRoutes: []string{goingCommentRejectRoute},
			},
		},
		Undo: goingUndoReverse, Class: goingClassSuggestion, Event: "comment.created", Proposed: true,
	},
	// A message reaches its session when it is sent, so it stays.
	"dispatch_message": {
		Routes: []goingToolRoute{
			{
				Method: http.MethodPost, Pattern: "/api/v1/issues/{key}/messages",
				Path:      map[string]string{"key": "issue"},
				Relations: []string{"replies_to", "mentions"},
				Translate: goingTranslateIssueMessage,
			},
			{
				Method: http.MethodPost, Pattern: "/api/v1/messages/{id}/reply",
				Path:      map[string]string{"id": "in_reply_to"},
				Relations: []string{"replies_to", "mentions"},
				Translate: goingTranslateMessageReply,
			},
		},
		Undo: goingUndoNotUndone, Class: goingClassMessage, Event: "message.created", Proposed: true,
	},
	// The four corrections of a post: an edit replaces its body, a delete removes it. Each names
	// its own post, so the one placeholder binds to the tool's own argument. An edit re-indexes the
	// new body, which is the one edge family a body writes; a delete clears the body, so it writes
	// no edge at all and removes the ones the post had. Each reads the post before it, its author
	// and its body, which its row shows (goingObserver.postEdited, postDeleted). An edit is written
	// back: its route answers the body it replaced (commentEdited, messageEdited), and the undo
	// writes that body back through the same route under a condition that the post still holds
	// the body the edit left (goingCondition.ExpectBody), so words its author wrote since are kept.
	// A delete is not undone: no route restores a deleted post, so it waits for the person in
	// both modes, as every change of text does (going_modes.go), and is named deleted, not undone.
	"dispatch_edit_comment": {
		Routes: []goingToolRoute{{
			Method: http.MethodPatch, Pattern: "/api/v1/comments/{id}",
			Path: map[string]string{"id": "comment"}, Relations: []string{"mentions"},
			Translate:  goingTranslateEditPost,
			UndoRoutes: []string{goingCommentEditRoute},
		}},
		Observe: goingObserver.postEdited, Undo: goingUndoWriteBack, Class: goingClassText, Event: "comment.edited",
	},
	"dispatch_delete_comment": {
		Routes: []goingToolRoute{{
			Method: http.MethodDelete, Pattern: "/api/v1/comments/{id}",
			Path:      map[string]string{"id": "comment"},
			Translate: goingTranslateDeletePost,
		}},
		Observe: goingObserver.postDeleted, Undo: goingUndoNotUndone, Class: goingClassText, Event: "comment.deleted",
	},
	"dispatch_edit_message": {
		Routes: []goingToolRoute{{
			Method: http.MethodPatch, Pattern: "/api/v1/messages/{id}",
			Path: map[string]string{"id": "message"}, Relations: []string{"mentions"},
			Translate:  goingTranslateEditPost,
			UndoRoutes: []string{goingMessageEditRoute},
		}},
		Observe: goingObserver.postEdited, Undo: goingUndoWriteBack, Class: goingClassText, Event: "message.edited",
	},
	"dispatch_delete_message": {
		Routes: []goingToolRoute{{
			Method: http.MethodDelete, Pattern: "/api/v1/messages/{id}",
			Path:      map[string]string{"id": "message"},
			Translate: goingTranslateDeletePost,
		}},
		Observe: goingObserver.postDeleted, Undo: goingUndoNotUndone, Class: goingClassText, Event: "message.deleted",
	},
	// A send to one session, which reaches it when it is sent, so it stays. A send to a session
	// that works on an issue is posted on that issue (going_messages.go) as a message that targets
	// the session, so it is kept and threaded in the issue's conversation: the first row, whose
	// {key} is that issue (goingSendIssue), with the mention and reply edges every issue message
	// makes. A send to a session that works on no issue goes to the session's own conversation,
	// which belongs to no issue: the second row, whose path names the session the tool was asked
	// to reach, not the calling session, so the placeholder binds to the tool's own argument. That
	// body is indexed without its references counted (createStoredMessage: a message with no issue
	// takes refs.IndexItem alone), so it makes no mention edge; a send naming in_reply_to makes
	// the replies_to edge every message reply makes.
	"dispatch_send_to_agent": {
		Routes: []goingToolRoute{
			{
				Method: http.MethodPost, Pattern: "/api/v1/issues/{key}/messages",
				Path:      map[string]string{"key": goingSendIssue},
				Relations: []string{"replies_to", "mentions"},
				Translate: goingTranslateSendOnIssue,
			},
			{
				Method: http.MethodPost, Pattern: "/api/v1/agents/{session_id}/messages",
				Path: map[string]string{"session_id": "session"}, Relations: []string{"replies_to"},
				Translate: goingTranslateSendToAgent,
			},
		},
		Observe: goingObserver.sentToAgent, Undo: goingUndoNotUndone,
		Class: goingClassMessage, Event: "message.created", Proposed: true,
	},
	// A document's content is written back by an upload of the content read before the edit,
	// under the document's own name, through its owner's upload route. A call naming the
	// document by id, beside the issue or the project its spec requires, takes the first row,
	// the document's own route, since the owner's slug routes read a slug or a filename and
	// never an id. The issue row binds both its placeholders to the issue, so a call naming the
	// issue alone takes it and edits the issue's spec, as the tool's spec says; the translation
	// reads the document's slug from artifact (goingTranslateIssueDocEdit).
	"dispatch_doc_edit": {
		Routes: []goingToolRoute{
			{
				Method: http.MethodPost, Pattern: "/api/v1/artifacts/{id}/edits",
				Path:         map[string]string{"id": "artifact"},
				WhenArgument: "artifact", WhenValue: goingAnID,
				Relations:  []string{"mentions", "needs", "serves"},
				Translate:  goingTranslateDocEdit,
				UndoRoutes: []string{goingIssueUploadRoute, goingProjectUploadRoute},
			},
			{
				Method: http.MethodPost, Pattern: "/api/v1/issues/{key}/artifacts/{slug}/edits",
				Path:       map[string]string{"key": "issue", "slug": "issue"},
				Relations:  []string{"mentions", "needs", "serves"},
				Translate:  goingTranslateIssueDocEdit,
				UndoRoutes: []string{goingIssueUploadRoute},
			},
			{
				Method: http.MethodPost, Pattern: "/api/v1/projects/{key}/artifacts/{slug}/edits",
				Path:       map[string]string{"key": "project", "slug": "artifact"},
				Relations:  []string{"mentions", "needs", "serves"},
				Translate:  goingTranslateDocEdit,
				UndoRoutes: []string{goingProjectUploadRoute},
			},
		},
		Observe: goingObserver.documentEditedBy, Undo: goingUndoWriteBack,
		Class: goingClassText, Event: "artifact.version", Proposed: true,
	},
	// An approval request opens an ask, so its undo is that ask's retraction. The first row binds
	// the request's document to the issue the call names, whose spec, or whose document the call
	// names, the translation reads; the second takes a call naming the project, whose document
	// the call names by id or by slug.
	"dispatch_request_approval": {
		Routes: []goingToolRoute{
			{
				Method: http.MethodPost, Pattern: "/api/v1/artifacts/{id}/approval-requests",
				Path: map[string]string{"id": "issue"}, Relations: []string{"owned_by"},
				Translate:  goingTranslateRequestApproval,
				UndoRoutes: []string{goingAskResolveRoute},
			},
			{
				Method: http.MethodPost, Pattern: "/api/v1/artifacts/{id}/approval-requests",
				Path: map[string]string{"id": "artifact"}, Relations: []string{"owned_by"},
				Translate:  goingTranslateRequestApproval,
				UndoRoutes: []string{goingAskResolveRoute},
			},
		},
		Observe: goingObserver.approvalRequested, Undo: goingUndoReverse,
		Class: goingClassMessage, Event: "ask.opened", Proposed: true,
	},
	// A plan review's approval is the review's own word, so it stays. The row binds the review's
	// document to the issue the call names, whose plan.md, or whose document the call names, the
	// translation reads.
	"dispatch_plan_approve": {
		Routes: []goingToolRoute{{
			Method: http.MethodPost, Pattern: "/api/v1/artifacts/{id}/plan-reviews",
			Path:      map[string]string{"id": "issue"},
			Translate: goingTranslatePlanApprove,
		}},
		Undo: goingUndoNotUndone, Class: goingClassReview, Event: "artifact.approved", Proposed: true,
	},
	// A review is the person's own word on a version, which answers the approval request and
	// reaches whoever asked, so it stays. The row binds the review's document to the issue the
	// call names, whose spec, or whose document the call names, the translation reads.
	"dispatch_review": {
		Routes: []goingToolRoute{{
			Method: http.MethodPost, Pattern: "/api/v1/artifacts/{id}/versions/{number}/reviews",
			Path:      map[string]string{"id": "issue", "number": "version"},
			Translate: goingTranslateReview,
		}},
		Undo: goingUndoNotUndone, Class: goingClassReview, Event: "artifact.approved",
	},
	// A retraction and a record are two shapes of one tool: the retraction's row names the
	// result in its path, so a call carrying `retract` fills it and a record, which carries
	// none, falls through to the row below. A result is evidence others read at once, and a
	// retraction is itself the record that undoes a result, so neither is undone.
	"dispatch_result": {
		Routes: []goingToolRoute{
			{
				Method: http.MethodPost, Pattern: "/api/v1/results/{id}/retract",
				Path:      map[string]string{"id": "retract"},
				Translate: goingTranslateResultRetract,
			},
			{
				Method: http.MethodPost, Pattern: "/api/v1/issues/{key}/results",
				Path:      map[string]string{"key": "issue"},
				Translate: goingTranslateResultRecord,
			},
		},
		Undo: goingUndoNotUndone, Class: goingClassResult, Event: "result.recorded", Proposed: true,
	},
	// Dispatch serves no removal of an upload's version, so an upload stays.
	"dispatch_artifact": {
		Routes: []goingToolRoute{
			{
				Method: http.MethodPost, Pattern: "/api/v1/issues/{key}/artifacts",
				Path:      map[string]string{"key": "issue"},
				Relations: []string{"attached_to", "mentions"},
				Translate: goingTranslateUpload,
			},
			{
				Method: http.MethodPost, Pattern: "/api/v1/projects/{key}/artifacts",
				Path:      map[string]string{"key": "project"},
				Relations: []string{"mentions"},
				Translate: goingTranslateUpload,
			},
			{
				Method: http.MethodPost, Pattern: "/api/v1/agents/{session_id}/artifacts",
				Path:      map[string]string{"session_id": goingCallerSession},
				Translate: goingTranslateAgentUpload,
			},
		},
		Observe: goingObserver.uploaded, Undo: goingUndoNotUndone,
		Class: goingClassCreateText, Event: "artifact.created", Proposed: true,
	},
	// Dispatch serves no removal of a project, so a project stays.
	"dispatch_project": {
		Routes: []goingToolRoute{{
			Method: http.MethodPost, Pattern: "/api/v1/projects", Path: map[string]string{},
			Translate: goingArgumentsAsBody,
		}},
		Undo: goingUndoNotUndone, Class: goingClassCreatePlain, Event: "project.created", Proposed: true,
	},
	// A sync names one source by its repository, else every source of the project. The one-source
	// row is first, so a call carrying `repo` fills it and one without falls through to the
	// every-source row. An import replaces the components it imported before, which no route
	// puts back, so it stays.
	"dispatch_architecture_sync": {
		Routes: []goingToolRoute{
			{
				Method: http.MethodPost, Pattern: "/api/v1/projects/{key}/architecture-sources/{owner}/{repo}/sync",
				Path:      map[string]string{"key": "project", "owner": "repo", "repo": "repo"},
				Relations: []string{"depends_on", "affects"},
				Translate: goingTranslateArchitectureSyncRepo,
			},
			{
				Method: http.MethodPost, Pattern: "/api/v1/projects/{key}/architecture-sources/sync",
				Path: map[string]string{"key": "project"}, Relations: []string{"depends_on", "affects"},
				Translate: goingTranslateNoBody,
			},
		},
		Undo: goingUndoNotUndone, Class: goingClassSetting, Event: "architecture.synced", Proposed: true,
	},
	// A handoff's earlier versions stay beside each new one, so a handoff stays.
	"dispatch_handoff_write": {
		Routes: []goingToolRoute{{
			Method: http.MethodPost, Pattern: "/api/v1/issues/{key}/handoffs/{phase}",
			Path:      map[string]string{"key": "issue", "phase": "phase"},
			Translate: goingTranslateHandoff,
		}},
		Undo: goingUndoNotUndone, Class: goingClassResult, Event: "handoff.recorded", Proposed: true,
	},
	// The person's own state of one issue. Its route replaces the stored list of the issue's
	// pinned and dismissed items whole, so the translation reads that state first and sends the
	// list with the call's items added, under the next seq of the row it read
	// (goingTranslateIssueState). The state before is not kept, so a change is not undone.
	"dispatch_set_issue_state": {
		Routes: []goingToolRoute{{
			Method: http.MethodPut, Pattern: "/api/v1/me/issues/{key}/state",
			Path:      map[string]string{"key": "issue"},
			Translate: goingTranslateIssueState,
		}},
		Undo: goingUndoNotUndone, Class: goingClassSetting, Event: "user_state.updated",
	},

	// The tools whose spec binds their routes. A setting's value before is not read, so a
	// setting's change is not undone: the person changes it again to put it back.
	"dispatch_save_architecture_source":       {Undo: goingUndoNotUndone, Class: goingClassSetting, Event: "settings.architecture_source.updated"},
	"dispatch_remove_architecture_source":     {Undo: goingUndoNotUndone, Class: goingClassSetting, Event: "settings.architecture_source.updated"},
	"dispatch_sync_first_architecture_source": {Undo: goingUndoNotUndone, Class: goingClassSetting, Event: "architecture.synced"},
	"dispatch_set_default_assignee":           {Undo: goingUndoNotUndone, Class: goingClassSetting, Event: "settings.project_default_assignee.updated"},
	"dispatch_set_default_repository":         {Undo: goingUndoNotUndone, Class: goingClassSetting, Event: "project.updated"},
	"dispatch_set_delivery_settings":          {Undo: goingUndoNotUndone, Class: goingClassSetting, Event: goingNoEvent},
	"dispatch_set_legion_daemon":              {Undo: goingUndoNotUndone, Class: goingClassSetting, Event: goingNoEvent},
	"dispatch_unset_legion_daemon":            {Undo: goingUndoNotUndone, Class: goingClassSetting, Event: goingNoEvent},
	// A session group the run made stays: no route reads one group or names who changed it, and
	// its delete takes no condition, so an undo that deleted it could take with it the sessions
	// another writer put in it since. The person deletes it to take it back.
	"dispatch_create_session_group": {Undo: goingUndoNotUndone, Class: goingClassSetting, Event: goingNoEvent},
	"dispatch_edit_session_group":   {Undo: goingUndoNotUndone, Class: goingClassSetting, Event: goingNoEvent},
	// A deleted group's sessions are not read before, so a delete is not undone.
	"dispatch_delete_session_group": {Undo: goingUndoNotUndone, Class: goingClassSetting, Event: goingNoEvent},
	// A priority the run made stays: a delete is final, since no route makes a deleted priority
	// live again, and takes no condition, so an undo that deleted it could end a priority another
	// writer edited, or tied issues to, since. The person deletes it to take it back.
	"dispatch_create_priority": {Undo: goingUndoNotUndone, Class: goingClassCreateText, Event: "priority.updated"},
	// A priority's fields before the edit are not read, so an edit is not undone.
	"dispatch_edit_priority": {Undo: goingUndoNotUndone, Class: goingClassField, Event: "priority.updated"},
	// Dispatch serves no route that makes a deleted priority live again.
	"dispatch_delete_priority": {Undo: goingUndoNotUndone, Class: goingClassField, Event: "priority.updated"},
	// A session subscribes itself; no route subscribes another session.
	"dispatch_unsubscribe_issue":    {Undo: goingUndoNotUndone, Class: goingClassLink, Event: "subscription.remove_requested"},
	"dispatch_unsubscribe_document": {Undo: goingUndoNotUndone, Class: goingClassLink, Event: "subscription.remove_requested"},
	// A delivery reaches its session when it is made.
	"dispatch_redeliver_message":       {Undo: goingUndoNotUndone, Class: goingClassMessage, Event: "message.delivery"},
	"dispatch_accept_message_delivery": {Undo: goingUndoNotUndone, Class: goingClassMessage, Event: "message.accepted"},
	"dispatch_redeliver_comment":       {Undo: goingUndoNotUndone, Class: goingClassMessage, Event: "comment.delivery"},
	"dispatch_reply_comment_delivery":  {Undo: goingUndoNotUndone, Class: goingClassMessage, Event: "comment.answered"},
	// A read mark only moves forward, and a dismissal's rows are not read before.
	"dispatch_mark_thread_read":   {Undo: goingUndoNotUndone, Class: goingClassSetting, Event: "user_thread_state.updated"},
	"dispatch_mark_inbox_read":    {Undo: goingUndoNotUndone, Class: goingClassSetting, Event: "user_thread_state.updated"},
	"dispatch_dismiss_inbox":      {Undo: goingUndoNotUndone, Class: goingClassSetting, Event: "user_thread_state.updated"},
	"dispatch_mark_document_read": {Undo: goingUndoNotUndone, Class: goingClassSetting, Event: goingNoEvent},
	"dispatch_set_agent_state":    {Undo: goingUndoNotUndone, Class: goingClassSetting, Event: "user_agent_state.updated"},
	// A broadcast reaches every session it names when it is sent.
	"dispatch_broadcast": {Undo: goingUndoNotUndone, Class: goingClassBroadcast, Event: "message.created"},
	// The ask's holder before the hand-on is not read, so a hand-on is not undone.
	"dispatch_hand_ask": {Undo: goingUndoNotUndone, Class: goingClassField, Event: "ask.asked_of"},
	// An answer and a decline reach whoever asked when they are written.
	"dispatch_answer_ask":  {Undo: goingUndoNotUndone, Class: goingClassAskAnswer, Event: "ask.answered"},
	"dispatch_decline_ask": {Undo: goingUndoNotUndone, Class: goingClassAskAnswer, Event: "ask.resolved"},
	// A thread the run reopened is resolved again by its undo, while it is still open.
	"dispatch_reopen_comment": {
		Observe: goingObserver.commentResolvedBy, Undo: goingUndoReverse, UndoRoutes: []string{goingCommentResolve},
		Class: goingClassField, Event: "comment.reopened",
	},
	// An accepted suggestion's change is the document's next version, which no route takes back
	// out, and a rejection reaches the suggestion's writer when it is made.
	"dispatch_accept_suggestion": {Undo: goingUndoNotUndone, Class: goingClassSuggestion, Event: "suggestion.accepted"},
	"dispatch_reject_suggestion": {Undo: goingUndoNotUndone, Class: goingClassSuggestion, Event: "suggestion.rejected"},
	// A document's name before the rename is not read, and no route brings back a removed
	// document or takes a version back out.
	"dispatch_rename_document":  {Undo: goingUndoNotUndone, Class: goingClassField, Event: "artifact.renamed"},
	"dispatch_remove_document":  {Undo: goingUndoNotUndone, Class: goingClassText, Event: "artifact.removed"},
	"dispatch_rebuild_document": {Undo: goingUndoNotUndone, Class: goingClassText, Event: "artifact.version"},
	"dispatch_name_version":     {Undo: goingUndoNotUndone, Class: goingClassField, Event: "artifact.version"},
	// A review is the person's own word, which answers the approval request and reaches whoever
	// asked.
	"dispatch_review_document": {Undo: goingUndoNotUndone, Class: goingClassReview, Event: "artifact.approved"},
	// Keeping a decision records that the person read it, which no route unrecords.
	"dispatch_keep_decision": {Undo: goingUndoNotUndone, Class: goingClassDecision, Event: "decision.kept"},
	// A snooze stays: its route keeps no record of the snooze it replaced, so an undo that ended
	// it could wake an ask the person had snoozed before the run. The person ends it to take it
	// back.
	"dispatch_snooze_ask":   {Undo: goingUndoNotUndone, Class: goingClassSetting, Event: "user_state.updated"},
	"dispatch_unsnooze_ask": {Undo: goingUndoNotUndone, Class: goingClassSetting, Event: "user_state.updated"},
}

// goingReadTools is every dispatch tool that changes nothing, by the tool's own name, and the
// answer run's own reads, by going's names for them (going/reads.go), which no tool spec states:
// the server makes those in process, through the same functions the rows they name answer with
// (going_reads.go), and a caller is offered one only where its credential may call every row it
// names (goingReadsFor).
var goingReadTools = map[string]goingReadTool{
	// A run's roster read names how many sessions it lists (goingTranslateRoster), so the agents
	// list reads no more of a roster than it answers.
	going.ReadRoster: {Routes: []goingToolRoute{
		{Method: http.MethodGet, Pattern: "/api/v1/agents", Translate: goingTranslateRoster},
	}},
	going.ReadProgress:        {Routes: goingReadRows("GET /api/v1/issues", "GET /api/v1/issues/{key}")},
	going.ReadArchitecture:    {Routes: goingReadRows("GET /api/v1/projects/{key}/architecture")},
	going.ReadProjectActivity: {Routes: goingReadRows("GET /api/v1/projects/{key}/activity")},
	going.ReadDelivery: {Routes: goingReadRows(
		"GET /api/v1/delivery/measures", "GET /api/v1/delivery/timeline", "GET /api/v1/delivery/pipeline")},

	// The tools whose command is written by hand, each with the rows its command reads and the
	// argument that fills each of their placeholders.
	//
	// dispatch_read reads the item its arguments name (goingPickRead); the reference that names an
	// ask or a comment fills those rows' {id}, and an issue's dispatch://KEY/comments reads the
	// list of its comments. With history, the reference that names a comment or an issue message
	// fills its history row's {id}. The lists of an issue's asks and of a document's asks and
	// comments, and a project document's summary, are reads its command makes to find an item,
	// which no call picks.
	"dispatch_read": {Pick: goingPickRead, Routes: []goingToolRoute{
		{Method: http.MethodGet, Pattern: "/api/v1/issues/{key}", Path: map[string]string{"key": "issue"}},
		{Method: http.MethodGet, Pattern: "/api/v1/issues/{key}/events", Path: map[string]string{"key": "issue"}},
		{Method: http.MethodGet, Pattern: "/api/v1/issues/{key}/asks", Path: map[string]string{"key": "issue"}},
		{Method: http.MethodGet, Pattern: "/api/v1/issues/{key}/comments", Path: map[string]string{"key": "issue"}},
		{Method: http.MethodGet, Pattern: "/api/v1/issues/{key}/messages/{id}", Path: map[string]string{"key": "issue", "id": "message"}},
		{Method: http.MethodGet, Pattern: "/api/v1/issues/{key}/artifacts/{slug}", Path: map[string]string{"key": "issue", "slug": "artifact"}},
		{Method: http.MethodGet, Pattern: "/api/v1/projects/{key}/artifacts", Path: map[string]string{"key": "project"}},
		{Method: http.MethodGet, Pattern: "/api/v1/projects/{key}/artifacts/{slug}", Path: map[string]string{"key": "project", "slug": "artifact"}},
		{Method: http.MethodGet, Pattern: "/api/v1/projects/{key}/artifacts/{slug}/summary", Path: map[string]string{"key": "project", "slug": "artifact"}},
		{Method: http.MethodGet, Pattern: "/api/v1/artifacts/{id}/asks", Path: map[string]string{"id": "artifact"}},
		{Method: http.MethodGet, Pattern: "/api/v1/artifacts/{id}/comments", Path: map[string]string{"id": "artifact"}},
		{Method: http.MethodGet, Pattern: "/api/v1/asks/{id}", Path: map[string]string{"id": "ref"}},
		{Method: http.MethodGet, Pattern: "/api/v1/comments/{id}", Path: map[string]string{"id": "ref"}},
		{Method: http.MethodGet, Pattern: "/api/v1/comments/{id}/history", Path: map[string]string{"id": "ref"}},
		{Method: http.MethodGet, Pattern: "/api/v1/messages/{id}", Path: map[string]string{"id": "message"}},
		{Method: http.MethodGet, Pattern: "/api/v1/messages/{id}/history", Path: map[string]string{"id": "ref"}},
		{Method: http.MethodGet, Pattern: "/api/v1/references", Path: map[string]string{}},
	}},
	// dispatch_doc_read reads the document its arguments name (goingPickDocRead), or a version of
	// the uploaded file they name, which a read of its record tells (goingChooseDocRead); the
	// reference that names a file of an agent's conversation fills those rows' session and slug.
	// The list of an issue's documents and the blocks rows are reads its command makes beside the
	// text, which no call picks.
	"dispatch_doc_read": {Pick: goingPickDocRead, Choose: goingChooseDocRead, Routes: []goingToolRoute{
		{Method: http.MethodGet, Pattern: "/api/v1/issues/{key}/artifacts", Path: map[string]string{"key": "issue"}},
		{Method: http.MethodGet, Pattern: "/api/v1/issues/{key}/artifacts/{slug}/text", Path: map[string]string{"key": "issue", "slug": "artifact"}},
		{Method: http.MethodGet, Pattern: "/api/v1/issues/{key}/artifacts/{slug}/blocks", Path: map[string]string{"key": "issue", "slug": "artifact"}},
		{Method: http.MethodGet, Pattern: "/api/v1/issues/{key}/artifacts/{slug}/versions/{number}", Path: map[string]string{"key": "issue", "slug": "artifact", "number": "version"}},
		{Method: http.MethodGet, Pattern: "/api/v1/projects/{key}/artifacts/{slug}/text", Path: map[string]string{"key": "project", "slug": "artifact"}},
		{Method: http.MethodGet, Pattern: "/api/v1/projects/{key}/artifacts/{slug}/blocks", Path: map[string]string{"key": "project", "slug": "artifact"}},
		{Method: http.MethodGet, Pattern: "/api/v1/projects/{key}/artifacts/{slug}/versions/{number}", Path: map[string]string{"key": "project", "slug": "artifact", "number": "version"}},
		{Method: http.MethodGet, Pattern: "/api/v1/artifacts/{id}/text", Path: map[string]string{"id": "artifact"}},
		{Method: http.MethodGet, Pattern: "/api/v1/artifacts/{id}/blocks", Path: map[string]string{"id": "artifact"}},
		{Method: http.MethodGet, Pattern: "/api/v1/artifacts/{id}/versions/{number}", Path: map[string]string{"id": "artifact", "number": "version"}},
		{Method: http.MethodGet, Pattern: "/api/v1/agents/{session_id}/artifacts/{slug}", Path: map[string]string{"session_id": "ref", "slug": "ref"}},
		{Method: http.MethodGet, Pattern: "/api/v1/agents/{session_id}/artifacts/{slug}/versions/{number}", Path: map[string]string{"session_id": "ref", "slug": "ref", "number": "version"}},
	}},
	"dispatch_results": {Routes: []goingToolRoute{
		{Method: http.MethodGet, Pattern: "/api/v1/issues/{key}/results", Path: map[string]string{"key": "issue"}},
	}},
	// A search's words are the route's q (goingTranslateSearch).
	"dispatch_search": {Routes: []goingToolRoute{
		{Method: http.MethodGet, Pattern: "/api/v1/search", Path: map[string]string{}, Translate: goingTranslateSearch},
		{Method: http.MethodGet, Pattern: "/api/v1/search/results", Path: map[string]string{}, Translate: goingTranslateSearch},
	}},
	// A POST that writes nothing: the related-item check over a text as long as a spec.
	"dispatch_related": {Routes: []goingToolRoute{
		{Method: http.MethodPost, Pattern: "/api/v1/projects/{key}/related", Path: map[string]string{"key": "project"}},
	}},
	// An issue with no priority is the route's `none` (goingTranslateIssueList).
	"dispatch_issues": {Routes: []goingToolRoute{
		{Method: http.MethodGet, Pattern: "/api/v1/issues", Path: map[string]string{}, Translate: goingTranslateIssueList},
	}},
	"dispatch_projects":   {Routes: []goingToolRoute{{Method: http.MethodGet, Pattern: "/api/v1/projects", Path: map[string]string{}}}},
	"dispatch_priorities": {Routes: []goingToolRoute{{Method: http.MethodGet, Pattern: "/api/v1/priorities", Path: map[string]string{}}}},
	"dispatch_open_asks":  {Routes: []goingToolRoute{{Method: http.MethodGet, Pattern: "/api/v1/asks/open", Path: map[string]string{}}}},
	"dispatch_answers": {Routes: []goingToolRoute{
		{Method: http.MethodGet, Pattern: "/api/v1/agents/{session_id}/answers", Path: map[string]string{"session_id": goingCallerSession}},
	}},
	"dispatch_whoami": {Routes: []goingToolRoute{{Method: http.MethodGet, Pattern: "/api/v1/whoami", Path: map[string]string{}}}},
	"dispatch_limits": {Routes: []goingToolRoute{{Method: http.MethodGet, Pattern: "/api/v1/limits", Path: map[string]string{}}}},
	// A version names a handoff Dispatch reads by its id alone (goingTranslateHandoffVersion).
	"dispatch_handoff_read": {Routes: []goingToolRoute{
		{Method: http.MethodGet, Pattern: "/api/v1/handoffs/{id}", Path: map[string]string{"id": "version"}, Translate: goingTranslateHandoffVersion},
		{Method: http.MethodGet, Pattern: "/api/v1/issues/{key}/handoffs/{phase}/latest", Path: map[string]string{"key": "issue", "phase": "phase"}},
		{Method: http.MethodGet, Pattern: "/api/v1/issues/{key}/handoffs", Path: map[string]string{"key": "issue"}},
	}},
	"dispatch_steps": {Routes: []goingToolRoute{
		{Method: http.MethodGet, Pattern: "/api/v1/issues/{key}/steps", Path: map[string]string{"key": "issue"}},
	}},
	"dispatch_plans": {Routes: []goingToolRoute{
		{Method: http.MethodGet, Pattern: "/api/v1/projects/{key}/plans", Path: map[string]string{"key": "project"}},
	}},
	"dispatch_inbox": {Routes: []goingToolRoute{
		{Method: http.MethodGet, Pattern: "/api/v1/inbox", Path: map[string]string{}},
		{Method: http.MethodGet, Pattern: "/api/v1/me/replies", Path: map[string]string{}},
	}},

	// The tools whose spec binds their routes.
	"dispatch_list_routes":                       {},
	"dispatch_read_block_schema":                 {},
	"dispatch_check_markdown":                    {},
	"dispatch_find_related":                      {},
	"dispatch_check_result":                      {},
	"dispatch_list_repo_mappings":                {},
	"dispatch_check_app_installation":            {},
	"dispatch_list_architecture_sources":         {},
	"dispatch_list_project_architecture_sources": {},
	"dispatch_read_first_architecture_source":    {},
	"dispatch_list_default_assignees":            {},
	"dispatch_read_new_issue_settings":           {},
	"dispatch_read_delivery_settings":            {},
	"dispatch_read_legion_daemon":                {},
	"dispatch_read_legion_board":                 {},
	"dispatch_read_issue_legion":                 {},
	"dispatch_read_delivery_run":                 {},
	"dispatch_list_ci_reconcile_passes":          {},
	"dispatch_list_users":                        {},
	"dispatch_list_person_nodes":                 {},
	"dispatch_read_dashboard_builds":             {},
	"dispatch_read_priority_history":             {},
	"dispatch_resolve_issue":                     {},
	"dispatch_read_issue_summary":                {},
	"dispatch_read_issue_references":             {},
	"dispatch_list_collaborators":                {},
	"dispatch_read_question_map":                 {},
	"dispatch_read_reference_targets":            {},
	"dispatch_read_block_links":                  {},
	"dispatch_list_issue_subscribers":            {},
	"dispatch_read_coverage":                     {},
	"dispatch_list_issue_messages":               {},
	"dispatch_read_role_holders":                 {},
	"dispatch_read_handoff":                      {},
	"dispatch_read_message_blocks":               {},
	"dispatch_list_my_answers":                   {},
	"dispatch_read_search_coverage":              {},
	"dispatch_count_related_checks":              {},
	"dispatch_list_near_copy_checks":             {},
	"dispatch_read_near_copy_check":              {},
	"dispatch_list_agent_messages":               {},
	"dispatch_read_agent_document":               {},
	"dispatch_list_broadcasts":                   {},
	"dispatch_read_broadcast":                    {},
	"dispatch_list_decisions":                    {},
	"dispatch_read_ask":                          {},
	"dispatch_list_ask_followers":                {},
	"dispatch_read_comment":                      {},
	"dispatch_read_comment_blocks":               {},
	"dispatch_list_document_events":              {},
	"dispatch_read_document_references":          {},
	"dispatch_read_document_changes":             {},
	"dispatch_list_document_subscribers":         {},
	"dispatch_read_document":                     {},
	"dispatch_list_reviews":                      {},
	"dispatch_read_block":                        {},
	"dispatch_read_my_state":                     {},
	"dispatch_read_my_agent_state":               {},
	"dispatch_read_document_read_mark":           {},
	"dispatch_read_event_log":                    {},
}

// goingReadRows is rows the answer run reads in process, each "<METHOD> <pattern>": no call's
// arguments fill them, so none carries a Path.
func goingReadRows(names ...string) []goingToolRoute {
	rows := make([]goingToolRoute, 0, len(names))
	for _, name := range names {
		method, pattern, _ := strings.Cut(name, " ")
		rows = append(rows, goingToolRoute{Method: method, Pattern: pattern})
	}
	return rows
}

// The reasons a route a person may call has no tool: the routes the command bar's spec leaves out.
const (
	goingExcludedCredential = "a credential: the bar never reads, grants or revokes one"
	goingExcludedAgentToken = "an agent token: the bar never mints, lists, changes or revokes one"
	goingExcludedSignIn     = "a sign-in: the bar never lists or ends one"
	goingExcludedTestHook   = "a test hook, which a deployment serves only with its test hooks on"
	goingExcludedBar        = "the command bar's own route"
	goingExcludedDaemon     = "only the Legion daemon writes it: every other caller is refused (403)"
	// The status change writes several issues, each in a transaction of its own, so no landing of
	// one request records it and no Undo walks it back. The bar never calls it: a plan that sets
	// an option of dispatch_issue_update is written out as one update row for each issue the
	// option reaches (expandStatusOptions), each with its own landing and its own Undo.
	goingExcludedStatusChange = "a status change with an option: the bar reaches its outcome through one update row for each issue it reaches"
)

// goingExcludedRoutes is every route a person's credential may call that no tool calls, by
// "<METHOD> <pattern>", each with its reason.
var goingExcludedRoutes = map[string]string{
	"GET /api/v1/credential-requests":                               goingExcludedCredential,
	"POST /api/v1/credential-requests/{id}/approve":                 goingExcludedCredential,
	"POST /api/v1/credential-requests/{id}/deny":                    goingExcludedCredential,
	"POST /api/v1/credential-requests/{id}/dismiss":                 goingExcludedCredential,
	"POST /api/v1/credential-requests/machine-lookup":               goingExcludedCredential,
	"GET /api/v1/credential-grants":                                 goingExcludedCredential,
	"POST /api/v1/credential-grants/{id}/revoke":                    goingExcludedCredential,
	"GET /api/v1/machine-logins":                                    goingExcludedCredential,
	"POST /api/v1/machine-logins/{id}/revoke":                       goingExcludedCredential,
	"GET /api/v1/credentials/capabilities":                          goingExcludedCredential,
	"GET /api/v1/me/agent-tokens":                                   goingExcludedAgentToken,
	"POST /api/v1/me/agent-tokens":                                  goingExcludedAgentToken,
	"PATCH /api/v1/me/agent-tokens/{id}":                            goingExcludedAgentToken,
	"DELETE /api/v1/me/agent-tokens/{id}":                           goingExcludedAgentToken,
	"GET /api/v1/signins":                                           goingExcludedSignIn,
	"POST /api/v1/signins/{id}/revoke":                              goingExcludedSignIn,
	"POST /api/v1/going/requests":                                   goingExcludedBar,
	"GET /api/v1/going/requests/{id}/stream":                        goingExcludedBar,
	"POST /api/v1/going/requests/{id}/cancel":                       goingExcludedBar,
	"POST /api/v1/going/runs":                                       goingExcludedBar,
	"GET /api/v1/going/runs/{id}":                                   goingExcludedBar,
	"GET /api/v1/going/runs/{id}/events":                            goingExcludedBar,
	"POST /api/v1/going/runs/{id}/stop":                             goingExcludedBar,
	"GET /api/v1/going/runs/{id}/plan":                              goingExcludedBar,
	"POST /api/v1/going/runs/{id}/plan/approve":                     goingExcludedBar,
	"POST /api/v1/going/runs/{id}/plan/revise":                      goingExcludedBar,
	"POST /api/v1/going/runs/{id}/plan/reject":                      goingExcludedBar,
	"POST /api/v1/going/runs/{id}/undo":                             goingExcludedBar,
	"GET /api/v1/me/going/settings":                                 goingExcludedBar,
	"PUT /api/v1/me/going/settings":                                 goingExcludedBar,
	"GET /api/v1/going/capability":                                  goingExcludedBar,
	"GET " + goingCataloguePath:                                     goingExcludedBar,
	"GET /api/v1/me/going-runs":                                     goingExcludedBar,
	"GET /api/v1/me/going-plans":                                    goingExcludedBar,
	"PUT /api/v1/issues/{key}/design-gates":                         goingExcludedDaemon,
	"PUT /api/v1/projects/{key}/gated-steps":                        goingExcludedDaemon,
	"PUT /api/v1/issues/{key}/role-holders":                         goingExcludedDaemon,
	"PUT /api/v1/issues/{key}/steps/{id}":                           goingExcludedDaemon + " STEP_WRITER",
	"PUT /api/v1/issues/{key}/steps/{id}/rating":                    goingExcludedDaemon + " STEP_WRITER",
	"PUT /api/v1/issues/{key}/steps/{id}/experiment":                goingExcludedDaemon + " STEP_WRITER",
	"PUT /api/v1/issues/{key}/plans/{id}":                           goingExcludedDaemon + " STEP_WRITER",
	"POST /api/v1/issues/{key}/status-change":                       goingExcludedStatusChange,
	"POST /api/v1/live/_test/disconnect":                            goingExcludedTestHook,
	"POST /api/v1/live/_test/rotate":                                goingExcludedTestHook,
	"POST /api/v1/artifacts/_test/quiesce":                          goingExcludedTestHook,
	"POST /api/v1/artifacts/{id}/_test/outside-schema":              goingExcludedTestHook,
	"POST /api/v1/artifacts/{id}/_test/newer-content":               goingExcludedTestHook,
	"POST /api/v1/agents/{session_id}/stream/_test/publish":         goingExcludedTestHook,
	"GET /api/v1/artifacts/_test/rooms":                             goingExcludedTestHook,
	"GET /api/v1/artifacts/{id}/_test/room-counters":                goingExcludedTestHook,
	"POST /api/v1/artifacts/{id}/_test/hold-persistence":            goingExcludedTestHook,
	"PUT /api/v1/artifacts/{id}/_test/live-write-pauses/{point}":    goingExcludedTestHook,
	"GET /api/v1/artifacts/{id}/_test/live-write-pauses/{point}":    goingExcludedTestHook,
	"DELETE /api/v1/artifacts/{id}/_test/live-write-pauses/{point}": goingExcludedTestHook,
	"POST /api/v1/writes/_test/slow":                                goingExcludedTestHook,
	"POST /api/v1/settings/new-issue/_test":                         goingExcludedTestHook,
	"GET /api/v1/messages/_test/queued":                             goingExcludedTestHook,
}
