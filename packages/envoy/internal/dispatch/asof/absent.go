package asof

import (
	"fmt"
	"slices"
	"strings"
)

// AbsentField is one member that a past-time answer of a route can leave out, because the
// records at the cut cannot give it. Pointer is an RFC 6901 JSON Pointer into the answer, in
// which a segment * stands for every member of an array. Reason says why the records cannot
// give the member.
type AbsentField struct {
	Pointer string
	Reason  string
}

// The reasons of AbsentFields. The letter of each is its letter in the plan's list "Absent
// fields, by route" (INFRA-4620).
const (
	// L
	reasonLiveness = "This field shows liveness now. The route roster and the Envoy listener keep no past state."
	// G
	reasonGraph = "This field comes from the reference graph now. The graph keeps no history."
	// M
	reasonComponents = "This field comes from the present component model. The model keeps no history."
	// E
	reasonNoEvent = "No event records this field."
	// E, for a delivery attempt.
	reasonDeliveryState = "No receipt event or reply event gives the time of the state of this delivery attempt."
	// E, for the block that a reply names.
	reasonReplyBlock = "The item that holds this block changed after the cut. Dispatch keeps no record of the blocks of the item at the cut."
	// E, for the place of an anchored block.
	reasonAnchorBlock = "A stored document version records the id of a typed block only. " +
		"No record gives the place of this block in the document at the cut."
	// E, for a document version.
	reasonNoToken = "Dispatch stored this version with no document token. All such versions are from before 2026-11-03."
	// E, for a block id.
	reasonUntypedBlockID = "A stored document version records the id of a typed block only. " +
		"The block index gives the id of an untyped block only for a version that document_indexed_versions holds, " +
		"and only where no other block of that version has the same text. It gives no id of a table row or a table cell."
	// E, for a check of the Verification document.
	reasonUnindexedVersion = "No event records this field for a version that document_indexed_versions does not hold."
	// E, for whether the mark of a comment's anchor was gone.
	reasonAnchorOrphaned = "No record gives the time at which the mark of this anchor went out of the document. " +
		"Only an anchor refresh event gives that time."
	// E, for the wording of an ask.
	reasonAskWording = "The restore of a decided ask's wording changes this field with no event. " +
		"No record gives the time of the restore, so no record gives the wording at the cut."
	// P
	reasonParts = "Dispatch gives part ids and revisions only for an item that did not change after the cut."
	// D
	reasonProjectDefault = "The person comes from a project default from before the first event of that default."
	// B
	reasonMigrationFill = "A migration filled this value with no event. " +
		"The records do not give it for an ask whose newest event at the cut is older than 2026-11-04T16:42Z."
	// T
	reasonBody = "The records at the cut do not give the body. " +
		"Its event cut the body at 32,000 UTF-16 units and an edit after the cut replaced it before post_body_history began, " +
		"or the author deleted the post after the cut."
	// H
	reasonGitHub = "This field is a GitHub read now. A past-time answer makes no GitHub read."
	// H, for a result of a check that reads the program's deploy: the plan's list predates the
	// deploys (api/results_deploys.go), and this is a read now as a GitHub read is.
	reasonCurrentDeploy = "This field compares the result with the program's current deploy now. A past-time answer reads no current deploy."
	// S
	reasonBroadened = "Broadening reads search_term_stats, which counts only the present text. A past-time search does not broaden."
)

// AbsentFields is, by route ("<method> <pattern>" as in api/routes_table.go), each member that a
// past-time answer of the route can leave out. It holds the plan's list "Absent fields, by
// route", and beside it the members that the list does not name for a field the records at the
// cut do not give (decision 5): the delivery mode and the session of a delivery attempt and the
// text of the block that a reply names (commentFields, messageFields), whether the mark of a
// comment's anchor was gone (commentFields), the wording of an ask that a restore changed with no
// event, in each answer that gives it (an ask's, askWordingFields; the question of the ask of an
// agent's answer; the snippet of an ask hit, searchFields), the route's status reason of an ask's
// reach (routeReachFields), each reply of an ask's thread (commentFields), the documents of an
// approval request in the inbox (askFields), the place of an anchored block, and of the blocks
// that the text route gives, the anchored counts and the ids of a table's rows and cells, which
// the block routes leave out too (blockFields), and whether a result of a check that reads the
// program's current deploy counts (not_counted, reasonCurrentDeploy). The list's snippet tree of a
// search result is here for an ask hit alone, whose wording at the cut no record may give
// (searchFields): a past-time search draws every other snippet tree from the headline of the text
// at the cut. A route that the plan lists with no field has an empty list. A past-time answer
// names a member in the Dispatch-As-Of-Absent header only when it leaves that member out, and it
// names only members this table holds for its route (WriteHeaders, which tests check with
// ValidAbsent).
//
// The comment beside a route whose entry names no member of an ask's wording says why the route
// gives none that can be unknown at the cut.
var AbsentFields = map[string][]AbsentField{
	"GET /api/v1/issues/{key}": slices.Concat([]AbsentField{
		{"/route_status", reasonLiveness},
		{"/route_holder", reasonLiveness},
		{"/route_status_reason", reasonLiveness},
		{"/holder_state", reasonLiveness},
		{"/last_seen", reasonLiveness},
		{"/takeable_at", reasonLiveness},
		{"/design_gates", reasonNoEvent},
		{"/referenced_by_count", reasonGraph},
		{"/open_asks/*/referenced_by_count", reasonGraph},
		{"/components/ids", reasonComponents},
		{"/components/unknown", reasonComponents},
		{"/open_asks/*/revision", reasonParts},
		{"/open_asks/*/options/*/id", reasonParts},
		{"/open_asks/*/answer/id", reasonParts},
		{"/open_asks/*/waiting_on_person", reasonProjectDefault},
		{"/open_asks/*/approval/documents", reasonMigrationFill},
		{"/artifacts/*/approval/documents", reasonMigrationFill},
	}, askWordingFields("/open_asks/*")),
	// An unpaged listing is an array of issues; a paged one holds them under issues. The listing
	// counts an issue's open asks and resolve answers keys: neither gives an ask's wording.
	"GET /api/v1/issues":         slices.Concat(issueListingFields("/*"), issueListingFields("/issues/*")),
	"GET /api/v1/issues/resolve": {},

	// An ask event's payload gives the wording that the event wrote, which is a record.
	"GET /api/v1/issues/{key}/events":   eventLogFields(),
	"GET /api/v1/artifacts/{id}/events": eventLogFields(),
	"GET /api/v1/events/log":            eventLogFields(),

	// A post gives no ask's wording: a comment gives only the id of the ask whose thread holds it.
	// The text of a reply's block, which can be an ask's, is left out where its ask changed after
	// the cut, which an ask whose wording is unknown did (ItemsChangedAfterStatement).
	"GET /api/v1/issues/{key}/comments":   commentFields("/*"),
	"GET /api/v1/artifacts/{id}/comments": commentFields("/*"),
	"GET /api/v1/comments/{id}": slices.Concat(commentFields("/comment"), commentFields("/replies/*"), []AbsentField{
		{"/comment/anchor_block", reasonAnchorBlock},
	}),
	"GET /api/v1/comments/{id}/history":      {},
	"GET /api/v1/messages/{id}/history":      {},
	"GET /api/v1/issues/{key}/messages":      slices.Concat(messageFields("/*/message"), messageFields("/*/replies/*")),
	"GET /api/v1/issues/{key}/messages/{id}": slices.Concat(messageFields("/message"), messageFields("/replies/*")),
	"GET /api/v1/messages/{id}":              slices.Concat(messageFields("/message"), messageFields("/replies/*")),

	// ?thread=1 gives each ask of the list its thread.
	"GET /api/v1/issues/{key}/asks":   slices.Concat(askFields("/*"), routeReachFields("/*/thread"), commentFields("/*/thread/replies/*")),
	"GET /api/v1/artifacts/{id}/asks": askFields("/*"),
	"GET /api/v1/asks/{id}": slices.Concat(askFields("/ask"), routeReachFields(""), commentFields("/replies/*"), []AbsentField{
		{"/ask/anchor_block", reasonAnchorBlock},
	}),
	// Each ask the route lists is open at the cut, and the record gives an open ask's wording: the
	// restore writes only an ask whose decision came before the edit it undoes, so it writes an ask
	// open at the cut after the ask's first state event after the cut, which holds the wording then.
	"GET /api/v1/asks/open": {
		{"/asks/*/waiting_on_person", reasonProjectDefault},
	},
	"GET /api/v1/inbox": slices.Concat([]AbsentField{
		{"/*/snoozed_until", reasonNoEvent},
	}, askFields("/*"), routeReachFields("/*/thread"), commentFields("/*/thread/replies/*")),
	"GET /api/v1/agents/{session_id}/answers": {
		{"/items/*/ask/question", reasonAskWording},
	},

	// A document gives the id of an approval request's ask and no wording. A version's markdown,
	// an ask's block in it too, is a record: the restore's settlement writes a new version.
	"GET /api/v1/issues/{key}/artifacts":               documentFields("/*"),
	"GET /api/v1/issues/{key}/artifacts/{slug}":        documentFields(""),
	"GET /api/v1/projects/{key}/artifacts":             documentFields("/*"),
	"GET /api/v1/projects/{key}/artifacts/{slug}":      documentFields(""),
	"GET /api/v1/agents/{session_id}/artifacts/{slug}": documentFields(""),
	"GET /api/v1/artifacts/{id}":                       documentFields(""),
	// ?blocks=true gives the blocks, each as the block routes give it.
	"GET /api/v1/artifacts/{id}/text": slices.Concat([]AbsentField{
		{"/token", reasonNoToken},
	}, blockFields("/blocks/*")),
	"GET /api/v1/issues/{key}/artifacts/{slug}/blocks":   blockFields("/*"),
	"GET /api/v1/projects/{key}/artifacts/{slug}/blocks": blockFields("/*"),
	"GET /api/v1/artifacts/{id}/versions/{number}":       {},

	// A check names no ask.
	"GET /api/v1/issues/{key}/results": {
		{"/checks/*/compared_with", reasonGitHub},
		{"/checks/*/compared_nothing", reasonGitHub},
		{"/checks/*/pass_stands", reasonGitHub},
		{"/checks/*/ci", reasonGitHub},
		{"/checks/*/results/*/commit_stands", reasonGitHub},
		{"/checks/*/results/*/currency", reasonGitHub},
		{"/checks/*/results/*/not_counted", reasonCurrentDeploy},
		{"/checks/*/version", reasonUnindexedVersion},
		{"/checks/*/sha256", reasonUnindexedVersion},
		{"/checks/*/results/*/check_words_moved", reasonUnindexedVersion},
	},

	"GET /api/v1/search":         searchFields(),
	"GET /api/v1/search/results": searchFields(),
}

// issueListingFields are the fields of an issue in the listing, each issue at prefix.
func issueListingFields(prefix string) []AbsentField {
	return []AbsentField{
		{prefix + "/route_status", reasonLiveness},
		{prefix + "/route_holder", reasonLiveness},
		{prefix + "/route_status_reason", reasonLiveness},
		{prefix + "/components/ids", reasonComponents},
		{prefix + "/components/unknown", reasonComponents},
	}
}

// eventLogFields are the fields of an event of the three event logs. A deleted post's own
// events lose their body, as attachPostMarks marks them.
func eventLogFields() []AbsentField {
	return []AbsentField{
		{"/*/payload/referenced_by_count", reasonGraph},
		{"/*/payload/revision", reasonParts},
		{"/*/payload/options/*/id", reasonParts},
		{"/*/payload/answer/id", reasonParts},
		{"/*/payload/body", reasonBody},
	}
}

// UndatedCommentAttempt is the members of a comment's delivery attempt that a past-time answer
// leaves out where no receipt or reply event at the cut dates the attempt (reason E): the state,
// the members that a send writes with the state, and the delivery mode and the session that a
// send resolves with no event.
func UndatedCommentAttempt() []string { return slices.Clone(undatedCommentAttempt) }

// UndatedMessageAttempt is UndatedCommentAttempt for a message's delivery attempt.
func UndatedMessageAttempt() []string { return slices.Clone(undatedMessageAttempt) }

var (
	undatedCommentAttempt = []string{"state", "reply_id", "envelope_id", "resolve_error", "created_at", "delivery", "session_id"}
	undatedMessageAttempt = []string{"state", "reply_id", "envelope_id", "created_at", "requested_by", "delivery", "session_id"}
)

// commentFields are the fields of a comment at prefix: of a comment route's comment, and of each
// reply of an ask's thread. Not in the plan's list: the text of the block that the reply names,
// which an item changed after the cut rewrote as it rewrote whether the block is removed, and the
// tree of that text, which an answer carries for a request with display trees; the delivery mode
// and the session of an attempt, which a send resolves with no event, so for an attempt that no
// event at the cut dates they are absent too (decision 5); and whether the mark of the comment's
// anchor was gone, where a write with no event that dates it orphaned the anchor
// (CommentAnchorsOrphanedNotGivenStatement).
func commentFields(prefix string) []AbsentField {
	return append(postFields(prefix, undatedCommentAttempt), AbsentField{prefix + "/anchor/orphaned", reasonAnchorOrphaned})
}

// messageFields are the fields of an issue message at prefix, with the block that the reply names
// and the delivery mode and the session of an attempt for the reasons commentFields gives.
func messageFields(prefix string) []AbsentField {
	return postFields(prefix, undatedMessageAttempt)
}

// postFields are the fields of a post at prefix: its body, the block that it replies to, and the
// members attempt of each of its delivery attempts.
func postFields(prefix string, attempt []string) []AbsentField {
	fields := []AbsentField{
		{prefix + "/body", reasonBody},
		{prefix + "/reply_block/text", reasonReplyBlock},
		{prefix + "/reply_block/removed", reasonReplyBlock},
		{prefix + "/reply_block/text_tree", reasonReplyBlock},
	}
	for _, member := range attempt {
		fields = append(fields, AbsentField{prefix + "/deliveries/*/" + member, reasonDeliveryState})
	}
	return fields
}

// askFields are the fields of an ask at prefix.
func askFields(prefix string) []AbsentField {
	return append([]AbsentField{
		{prefix + "/referenced_by_count", reasonGraph},
		{prefix + "/revision", reasonParts},
		{prefix + "/options/*/id", reasonParts},
		{prefix + "/answer/id", reasonParts},
		{prefix + "/waiting_on_person", reasonProjectDefault},
		{prefix + "/approval/documents", reasonMigrationFill},
	}, askWordingFields(prefix)...)
}

// askWordingFields are the wording of an ask at prefix, which is unknown at a cut where a write
// with no event changed it at a time that no record gives (AsksChangedAfterStatement): the
// question, the options, multiple and the urgency, and the question's tree, which an answer
// carries for a request with display trees. The trees of an option's label and description are
// members of the option, so they go out with the options. Not in the plan's list.
func askWordingFields(prefix string) []AbsentField {
	return []AbsentField{
		{prefix + "/question", reasonAskWording},
		{prefix + "/options", reasonAskWording},
		{prefix + "/multiple", reasonAskWording},
		{prefix + "/urgency", reasonAskWording},
		{prefix + "/question_tree", reasonAskWording},
	}
}

// routeReachFields are the reach of an ask's route at prefix, which the route roster gives now.
// The route's status reason is not in the plan's list for an ask: it gives the reason for a
// status, and a past-time answer gives no status.
func routeReachFields(prefix string) []AbsentField {
	return []AbsentField{
		{prefix + "/route_status", reasonLiveness},
		{prefix + "/route_holder", reasonLiveness},
		{prefix + "/route_status_reason", reasonLiveness},
	}
}

// documentFields are the fields of a document at prefix.
func documentFields(prefix string) []AbsentField {
	return []AbsentField{
		{prefix + "/approval/documents", reasonMigrationFill},
	}
}

// blockFields are the fields of each block at prefix: of the two block routes' blocks, and of the
// blocks the text route gives.
func blockFields(prefix string) []AbsentField {
	return []AbsentField{
		{prefix + "/token", reasonNoEvent},
		{prefix + "/embedding", reasonNoEvent},
		{prefix + "/id", reasonUntypedBlockID},
		{prefix + "/table/rows/*/id", reasonUntypedBlockID},
		{prefix + "/table/rows/*/cells/*/id", reasonUntypedBlockID},
		{prefix + "/references", reasonGraph},
	}
}

// searchFields are the fields of a search answer. An ask hit's href links the ask's typed block
// only while the block is a node of the reference graph, which is the graph now: it is left out
// where the block's document changed after the cut. An ask hit's snippet, and its tree in an
// answer with display trees, come from the keyword table's version of the ask at the cut, which
// holds the wording of the ask's newest event at the cut: they are left out where the ask's
// wording at the cut no record gives (AsksChangedAfterStatement). The plan's list names the
// snippet tree, and not the snippet.
func searchFields() []AbsentField {
	return []AbsentField{
		{"/query/broadened", reasonBroadened},
		{"/results/*/href", reasonGraph},
		{"/results/*/snippet", reasonAskWording},
		{"/results/*/snippet_tree", reasonAskWording},
	}
}

// Absent reports whether AbsentFields holds pointer for route.
func Absent(route, pointer string) bool {
	return slices.ContainsFunc(AbsentFields[route], func(field AbsentField) bool { return field.Pointer == pointer })
}

// HeaderValue is the value of the Dispatch-As-Of-Absent header (contracts.DispatchAsOfAbsentHeader)
// that names pointers: each once, sorted, joined by commas.
func HeaderValue(pointers []string) string {
	names := slices.Clone(pointers)
	slices.Sort(names)
	return strings.Join(slices.Compact(names), ",")
}

// ValidAbsent refuses pointers that AbsentFields does not hold for route, and names each one.
func ValidAbsent(route string, pointers []string) error {
	var unknown []string
	for _, pointer := range pointers {
		if !Absent(route, pointer) {
			unknown = append(unknown, pointer)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	return fmt.Errorf("asof: AbsentFields holds no %s for %s", strings.Join(unknown, ", "), route)
}
