package textversions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// rulesVersion is the version of the event rules versionRules holds, which the fill writes in the
// cursor row it creates (search_text_versions_fill.rules). Each batch keeps there the lower of the
// version stored and this one (advanceCursorQuery), so the column holds the lowest rules version
// that wrote the rows up to the cursor. Version 1 read issues, primary documents, the creates,
// replies and edits of comments, the creates and replies of issue messages, and asks; version 2
// reads, besides, message.edited, comment.deleted and message.deleted (dispatch://INFRA-1523,
// rulesAdded), and takes a post's whole body from its body history (bodyFromHistory).
//
// A batch of this build that finds a lower version stored reads again what that version's fill
// passed over before it writes the events above the cursor (filler.rereadBatch): every event of
// the types added since, up to the cursor, and the rows that version's rules decided otherwise
// than this one's; and only that pass raises the column, once it has read everything. Only a
// build with these rules writes an event of the three types, so every one in the log was written
// by the roll that brought them or later, and a read of them all up to the cursor misses none.
const rulesVersion = 2

// Where a version's body came from, the values of search_text_versions.body_from this build
// writes: the event's payload, whole, which is also where a deleted post's empty body comes from
// (deletedPostVersion); the item's present row, which shows no later change; the post's body
// history, the body its first later edit or delete kept (historyBody); revision 1 of a comment's
// blocks; or the event's own text, cut short, where no source holds the whole body.
const (
	bodyFromEvent   = "event"
	bodyFromRow     = "row"
	bodyFromHistory = "history"
	bodyFromBlocks  = "blocks"
	bodyFromCut     = "cut"
)

// The kinds of item the keyword table holds, the values of search_text_versions.kind.
const (
	kindIssue   = "issue"
	kindComment = "comment"
	kindMessage = "message"
	kindAsk     = "ask"
)

// versionRules holds the rule of each event type that can start a version (rules version 2).
// Every other type is in unchangingTypes: TestEveryEventTypeIsSortedForTheKeywordFill holds each
// member of contracts.DispatchEventTypes in exactly one of the two.
//
// An issue's version holds its title and the version of its primary document its vector reads:
// issue.created starts the first, with version 1 of the primary document, which the create
// writes in its own transaction and which never gets an event of its own; issue.updated and
// issue.closed (a close can retitle in the same write) start one when the payload's title
// differs from the title of the issue's latest version, since older events carry no `changed`;
// and artifact.version starts one for the issue's primary document alone, which no other
// document's version changes, carrying the title forward. A primary document never gets
// artifact.created.
//
// A comment's version starts at comment.created and comment.answered (a reply is a new comment)
// and at a comment.edited whose body differs; an issue message's at message.created and
// message.answered (a reply is a new message) and at a message.edited whose body differs, and a
// message of no issue starts none, since the keyword table leaves direct messages out. A
// comment.deleted or message.deleted starts the post's version of the empty body
// (deletedPostVersion): the delete clears the post's body (api's clearDeletedPostBody), and the
// post's search trigger makes its search column the vector of the empty body (comments_search and
// messages_search, migrations 0059 and 0061, as 0087 wrote them), which that version's equals.
// An ask's starts at ask.opened (a reopen is one too), ask.edited and ask.answered (a restored
// answer included) when its body, its question, a space and ask_search_suffix of its options and
// answer as readEventsQuery reads it (askBodySQL), differs from its latest version's.
var versionRules = map[string]rule{
	"issue.created":    issueCreated,
	"issue.updated":    issueRetitled,
	"issue.closed":     issueRetitled,
	"artifact.version": primaryDocumentVersion,
	"comment.created":  postVersion(kindComment, false),
	"comment.answered": postVersion(kindComment, false),
	"comment.edited":   postVersion(kindComment, true),
	"comment.deleted":  deletedPostVersion(kindComment),
	"message.created":  postVersion(kindMessage, false),
	"message.answered": postVersion(kindMessage, false),
	"message.edited":   postVersion(kindMessage, true),
	"message.deleted":  deletedPostVersion(kindMessage),
	"ask.opened":       askVersion,
	"ask.edited":       askVersion,
	"ask.answered":     askVersion,
}

// unchangingTypes names each event type that changes no searchable text, with the reason.
var unchangingTypes = map[string]string{
	"issue.claimed":                             "an issue's claim is its state, not its text",
	"issue.released":                            "an issue's claim is its state, not its text",
	"issue.deployed":                            "an issue's deploy is its state, not its text",
	"issue.pull_request_attribution_changed":    "an issue's pull request is its state, not its text",
	"child.added":                               "a child issue is its parent's state, not its text",
	"child.removed":                             "a child issue is its parent's state, not its text",
	"child.status":                              "a child's status is its parent's state, not its text",
	"artifact.created":                          "no primary document is created by it: an issue's spec rides issue.created",
	"artifact.renamed":                          "an issue's primary document is refused a rename (ARTIFACT_PRIMARY), and a name is no text the search reads",
	"artifact.removed":                          "it changes no issue's title or primary document version",
	"artifact.human_edited":                     "it changes no issue's title or primary document version",
	"artifact.approved":                         "it changes no issue's title or primary document version",
	"artifact.changes_requested":                "it changes no issue's title or primary document version",
	"ask.resolved":                              "it leaves the ask's question, options and answer alone",
	"ask.handed_back":                           "it leaves the ask's question, options and answer alone: a reword before a hand-back is its own ask.edited",
	"ask.asked_of":                              "it leaves the ask's question, options and answer alone",
	"ask.anchor_refreshed":                      "it leaves the ask's question, options and answer alone",
	"ask.follower_added":                        "it leaves the ask's question, options and answer alone",
	"ask.follower_removed":                      "it leaves the ask's question, options and answer alone",
	"comment.resolved":                          "the comment's body does not change",
	"comment.reopened":                          "the comment's body does not change",
	"comment.anchor_refreshed":                  "the comment's body does not change",
	"comment.delivery":                          "the comment's body does not change",
	"suggestion.accepted":                       "the comment's body does not change: the suggestion changes the document through its own artifact.version",
	"suggestion.rejected":                       "the comment's body does not change",
	"message.delivery":                          "a message's delivery carries no text",
	"message.accepted":                          "a message's acceptance carries no text",
	"block.repaired":                            "the document's own version carries the change",
	"block.invalid":                             "the document's own version carries the change",
	"result.recorded":                           "a result is not an issue, comment, message or ask",
	"result.retracted":                          "a result is not an issue, comment, message or ask",
	"project.created":                           "a project is not an issue, comment, message or ask",
	"project.updated":                           "a project is not an issue, comment, message or ask",
	"project.access_changed":                    "a project's access is not an issue, comment, message or ask",
	"settings.architecture_source.updated":      "a setting is not an issue, comment, message or ask",
	"settings.project_default_assignee.updated": "a setting is not an issue, comment, message or ask",
	"settings.repo_project.updated":             "a setting is not an issue, comment, message or ask",
	"architecture.synced":                       "an architecture sync is not an issue, comment, message or ask",
	"architecture.sync_failed":                  "an architecture sync is not an issue, comment, message or ask",
	"user_state.updated":                        "a person's state is not an issue, comment, message or ask",
	"user_agent_state.updated":                  "a person's state is not an issue, comment, message or ask",
	"user_thread_state.updated":                 "a person's state is not an issue, comment, message or ask",
	"subscription.remove_requested":             "a subscription is not an issue, comment, message or ask",
	"subscription.removed":                      "a subscription is not an issue, comment, message or ask",
	"handoff.recorded":                          "a handoff is not an issue, comment, message or ask",
	"decision.kept":                             "a kept decision is not an issue, comment, message or ask",
	"priority.updated":                          "a priority is not an issue, comment, message or ask",
	"going_plan.updated":                        "a command-bar plan's wait is a person's state, not an issue, comment, message or ask",
}

// errUnreadablePayload is a rule's answer for a payload that lacks a field the rule reads, which
// stops the fill at that event rather than letting it miss a version.
var errUnreadablePayload = errors.New("the keyword rules cannot read this event's payload")

// issuePayload is what the rules read of an issue.* event's payload, the issue as the write left
// it.
type issuePayload struct {
	Key               *string `json:"key"`
	Title             *string `json:"title"`
	PrimaryArtifactID *string `json:"primary_artifact_id"`
}

// artifactVersionPayload is what the rules read of an artifact.version event's payload: the
// document and the version the write made of it.
type artifactVersionPayload struct {
	ArtifactID *string `json:"artifact_id"`
	Version    *struct {
		Number *int32 `json:"number"`
		Size   int64  `json:"size"` // unread, but decoding it refuses a payload whose size is no integer
	} `json:"version"`
}

// postPayload is what the rules read of a comment.* or message.* event's payload, and whether
// the event cut the body (model.EventBody).
type postPayload struct {
	ID            *string `json:"id"`
	IssueKey      *string `json:"issue_key"`
	Body          *string `json:"body"`
	BodyTruncated bool    `json:"body_truncated"`
}

// askPayload is what the rules read of an ask.* event's payload: its id, its issue and its
// question. Its options and answer reach the rules inside the ask's body (event.AskBody).
type askPayload struct {
	ID       *string `json:"id"`
	IssueKey *string `json:"issue_key"`
	Question *string `json:"question"`
}

// decodePayload decodes e's payload into payload.
func decodePayload(e event, payload any) error {
	if err := json.Unmarshal(e.Payload, payload); err != nil {
		return fmt.Errorf("%w: %v", errUnreadablePayload, err)
	}
	return nil
}

// missing is the error of a payload that lacks field.
func missing(field string) error {
	return fmt.Errorf("%w: it has no %s", errUnreadablePayload, field)
}

// notUUID is the error of a payload whose field holds an id that a statement of the batch casts
// to uuid and that is not one (isUUID).
func notUUID(field string) error {
	return fmt.Errorf("%w: its %s is not a uuid", errUnreadablePayload, field)
}

// isUUID is whether id is a uuid in its canonical form: 36 characters, 8-4-4-4-12 hex digits of
// either case with a hyphen between each two groups. That form is one of those Postgres's uuid
// input reads, so a statement can cast an id that passes to uuid, and it is the form each
// Dispatch writer writes, since every payload id is an id::text. A rule sends each id the batch
// casts through it, so any other id stops the fill at its event (errUnreadablePayload), named,
// rather than failing the batch's statement on every retry: one Postgres refuses, and also one
// it reads, such as the braced form or 32 hex digits, which no writer writes. At 36 characters
// uuid.Validate checks exactly the hyphens and the hex digits; at other lengths it reads more
// forms, the urn:uuid: one among them, which Postgres refuses.
func isUUID(id string) bool {
	return len(id) == 36 && uuid.Validate(id) == nil
}

// newVersion is the version e starts of item (kind, id) of issueKey, with the columns every row
// takes from its event. Its project is the event's, which a version of any kind needs.
func newVersion(e event, kind, id string, issueKey *string) (version, error) {
	if e.Project == "" {
		return version{}, fmt.Errorf("%w: the event names no project", errUnreadablePayload)
	}
	return version{
		Kind:       kind,
		ID:         id,
		IssueKey:   issueKey,
		ProjectKey: e.Project,
		EventID:    e.ID,
		EventSeq:   e.Seq,
		StartedAt:  e.CreatedAt,
		BodyFrom:   bodyFromEvent,
	}, nil
}

// issueKeyOf is the key of the issue an issue.* event is of: the payload's, else the event's.
func issueKeyOf(e event, p issuePayload) (string, error) {
	switch {
	case p.Key != nil && *p.Key != "":
		return *p.Key, nil
	case e.IssueKey != nil && *e.IssueKey != "":
		return *e.IssueKey, nil
	}
	return "", missing("key")
}

// primaryDocumentOf is the primary document an issue.* payload names, whose id the batch casts
// to uuid: nil for none, where the payload has no primary_artifact_id or an empty one, which the
// API writes for an issue with no primary document. An id that is not a uuid (isUUID) is a
// payload the rules cannot read, whatever the issue's latest version, so the batch's first pass
// meets it (decideVersions).
func primaryDocumentOf(p issuePayload) (*string, error) {
	switch {
	case p.PrimaryArtifactID == nil || *p.PrimaryArtifactID == "":
		return nil, nil
	case !isUUID(*p.PrimaryArtifactID):
		return nil, notUUID("primary_artifact_id")
	}
	return p.PrimaryArtifactID, nil
}

// issueCreated starts an issue's first version: its title, and version 1 of its primary
// document (primaryDocumentOf), which the create wrote in its own transaction.
func issueCreated(e event, _ latestVersion) (version, bool, error) {
	var p issuePayload
	if err := decodePayload(e, &p); err != nil {
		return version{}, false, err
	}
	key, err := issueKeyOf(e, p)
	if err != nil {
		return version{}, false, err
	}
	if p.Title == nil {
		return version{}, false, missing("title")
	}
	primary, err := primaryDocumentOf(p)
	if err != nil {
		return version{}, false, err
	}
	v, err := newVersion(e, kindIssue, key, &key)
	if err != nil {
		return version{}, false, err
	}
	v.Body = *p.Title
	if primary != nil {
		first := int32(1)
		v.SpecArtifactID, v.SpecVersion = primary, &first
	}
	return v, true, nil
}

// issueRetitled starts a version at an issue.updated or an issue.closed whose title differs from
// the title of the issue's latest version, carrying that version's primary document version
// forward; an issue with no version yet takes the payload's primary document
// (primaryDocumentOf), and no version of it.
func issueRetitled(e event, latest latestVersion) (version, bool, error) {
	var p issuePayload
	if err := decodePayload(e, &p); err != nil {
		return version{}, false, err
	}
	key, err := issueKeyOf(e, p)
	if err != nil {
		return version{}, false, err
	}
	if p.Title == nil {
		return version{}, false, missing("title")
	}
	primary, err := primaryDocumentOf(p)
	if err != nil {
		return version{}, false, err
	}
	prior := latest(kindIssue, key)
	if prior != nil && prior.Body == *p.Title {
		return version{}, false, nil
	}
	v, err := newVersion(e, kindIssue, key, &key)
	if err != nil {
		return version{}, false, err
	}
	v.Body = *p.Title
	v.SpecArtifactID = primary
	if prior != nil && prior.SpecArtifactID != nil {
		v.SpecArtifactID, v.SpecVersion = prior.SpecArtifactID, prior.SpecVersion
	}
	return v, true, nil
}

// primaryDocumentVersion starts a version at an artifact.version of an issue's primary document,
// the artifact id the issue's latest version carries, holding the version the event names and
// the title carried forward. A version of another document of the issue, and one of a project's
// document (no issue), starts none. An issue whose latest version names no primary document, or
// that has no version yet, cannot tell its primary document from its versions: the version
// starts with the artifact the event names, and no title (an empty Body) where no version gives
// one, and decide reads which document is the issue's primary one from its rows. An artifact id
// of an issue's document that is not a uuid (isUUID), which the batch casts to one, is a payload
// the rules cannot read, whatever the issue's latest version, so the batch's first pass meets it
// (decideVersions).
func primaryDocumentVersion(e event, latest latestVersion) (version, bool, error) {
	if e.IssueKey == nil {
		return version{}, false, nil
	}
	var p artifactVersionPayload
	if err := decodePayload(e, &p); err != nil {
		return version{}, false, err
	}
	if p.ArtifactID == nil {
		return version{}, false, missing("artifact_id")
	}
	if !isUUID(*p.ArtifactID) {
		return version{}, false, notUUID("artifact_id")
	}
	if p.Version == nil || p.Version.Number == nil {
		return version{}, false, missing("version.number")
	}
	key := *e.IssueKey
	prior := latest(kindIssue, key)
	if prior != nil && prior.SpecArtifactID != nil && *prior.SpecArtifactID != *p.ArtifactID {
		return version{}, false, nil
	}
	v, err := newVersion(e, kindIssue, key, &key)
	if err != nil {
		return version{}, false, err
	}
	v.SpecArtifactID, v.SpecVersion = p.ArtifactID, p.Version.Number
	if prior != nil {
		v.Body, v.BodyFrom = prior.Body, prior.BodyFrom
	}
	return v, true, nil
}

// postVersion is the rule of an event of a comment or a message of kind: the version holds the
// body the event carries, which decide reads whole from another source where the event cut it.
// onlyWhenChanged is an edit's rule, which starts a version only when the body differs from the
// item's latest version's; where the event cut the body, decide compares the whole one. A
// message of no issue starts none. The id of a post whose event cut its body must be a uuid
// (isUUID): reading the whole body casts it to one (presentBody, firstRevisionBody). No statement
// casts the id of a post whose event carries its body whole, which the version's row holds as
// text, so that id need not be one.
func postVersion(kind string, onlyWhenChanged bool) rule {
	return func(e event, latest latestVersion) (version, bool, error) {
		p, issueKey, ok, err := decodePost(e, kind)
		if err != nil || !ok {
			return version{}, false, err
		}
		if p.Body == nil {
			return version{}, false, missing("body")
		}
		if p.BodyTruncated && !isUUID(*p.ID) {
			return version{}, false, notUUID("id")
		}
		if onlyWhenChanged && !p.BodyTruncated {
			if prior := latest(kind, *p.ID); prior != nil && prior.Body == *p.Body {
				return version{}, false, nil
			}
		}
		v, err := newVersion(e, kind, *p.ID, issueKey)
		if err != nil {
			return version{}, false, err
		}
		v.Body, v.BodyTruncated = *p.Body, p.BodyTruncated
		return v, true, nil
	}
}

// deletedPostVersion is the rule of a delete of a comment or a message of kind (comment.deleted,
// message.deleted): the delete clears the post's body, so the version holds the empty body. The
// rule reads no body from the payload: what the event says is that the body is gone, which its
// type says whole, so the version's body_from is bodyFromEvent, as for a body an event carries
// whole; no other source could say more, the post's row holding the empty body and its history
// the body the delete replaced. It starts no version where the post's latest version is already
// empty, as an edit that keeps the body starts none; a post that stands never has an empty body
// (the create and the edit refuse one), so only an earlier delete's version is. A message of no
// issue starts none. No statement casts the post's id, which need not be a uuid.
func deletedPostVersion(kind string) rule {
	return func(e event, latest latestVersion) (version, bool, error) {
		p, issueKey, ok, err := decodePost(e, kind)
		if err != nil || !ok {
			return version{}, false, err
		}
		if prior := latest(kind, *p.ID); prior != nil && prior.Body == "" {
			return version{}, false, nil
		}
		v, err := newVersion(e, kind, *p.ID, issueKey)
		if err != nil {
			return version{}, false, err
		}
		return v, true, nil
	}
}

// decodePost reads e's payload as an event of a post of kind: the payload, with its id, which
// must be there, and the post's issue, the payload's, else the event's. ok is false for a message
// of no issue, whose events start no version, since the keyword table leaves direct messages out.
func decodePost(e event, kind string) (postPayload, *string, bool, error) {
	var p postPayload
	if err := decodePayload(e, &p); err != nil {
		return postPayload{}, nil, false, err
	}
	if p.ID == nil || *p.ID == "" {
		return postPayload{}, nil, false, missing("id")
	}
	issueKey := p.IssueKey
	if issueKey == nil {
		issueKey = e.IssueKey
	}
	return p, issueKey, kind != kindMessage || issueKey != nil, nil
}

// askVersion is the rule of ask.opened, ask.edited and ask.answered: the version holds the ask's
// body as readEventsQuery made it in SQL (event.AskBody), its question, a space and
// ask_search_suffix of its options and answer, and starts only when that body differs from the
// ask's latest version's. An event read with no body while its payload holds a question is a bug
// of the fill's read, not a payload the rules cannot read, and is answered with an error that
// says so.
func askVersion(e event, latest latestVersion) (version, bool, error) {
	var p askPayload
	if err := decodePayload(e, &p); err != nil {
		return version{}, false, err
	}
	if p.ID == nil || *p.ID == "" {
		return version{}, false, missing("id")
	}
	if p.Question == nil {
		return version{}, false, missing("question")
	}
	if e.AskBody == nil {
		return version{}, false, fmt.Errorf("the event of ask %s was read with no body, which readEventsQuery reads of every ask event (askBodySQL): a bug of the fill, not of the payload", *p.ID)
	}
	if prior := latest(kindAsk, *p.ID); prior != nil && prior.Body == *e.AskBody {
		return version{}, false, nil
	}
	issueKey := p.IssueKey
	if issueKey == nil {
		issueKey = e.IssueKey
	}
	v, err := newVersion(e, kindAsk, *p.ID, issueKey)
	if err != nil {
		return version{}, false, err
	}
	v.Body = *e.AskBody
	return v, true, nil
}

// item names one item of the keyword table, the key its versions share: its kind and its id.
type item struct{ kind, id string }

// decided is what decideVersions made of a batch of events: the versions they start, in event id
// order, each with its body whole where a source holds it; Cut, how many of them hold a post's
// body cut short because no source held it whole (bodyFromCut), the count the fill's progress
// line reads; and Bytes, what the events decided count against the batch's bound (batchBytes),
// which bounds the batch's statements (statementTimeout).
type decided struct {
	Versions []version
	Cut      int
	Bytes    int64
}

// decideVersions decides, in the caller's transaction tx, the versions events start under rules:
// events in ascending id order, as the fill reads the log, of which it decides those that fit in
// maxBytes. Each event goes through the rule of its type (rules.rules) with the item's latest
// version, which is the latest of the item decided earlier in the same batch, else the item's
// latest row in the table (latestVersions), so two events of one item in one batch each see the
// other's change: an issue's title and its primary document version are carried forward from row
// to row, and an edit is compared with the text before it. An event of a type no rule reads
// starts no version.
//
// What an event does not hold is read from the item's rows, in the same transaction:
//
//   - A post whose event cut its body (body_truncated) takes it whole, in this order: from its
//     present row where, in the one snapshot that reads the row, no committed event of the post
//     after the event changes its text (postBodyReads: a comment's comment.edited and
//     comment.deleted, an issue message's message.edited and message.deleted), so the row still
//     holds the text the event wrote (presentBody, bodyFromRow); else from the post's body
//     history, the body the first of those later events kept, where each of the post's events
//     of those types kept one (historyBody, bodyFromHistory); for a comment.created, from
//     revision 1 of its blocks when every block of that revision was inserted by the comment's
//     own transaction (firstRevisionBody, bodyFromBlocks); else it keeps the event's text
//     (bodyFromCut), with a warning naming the item and the event, and counts in Cut. An edit
//     whose whole body equals its latest version's starts no version.
//   - An issue.created whose primary document's version 1 was not written at its created_at, by
//     the create's own transaction, still holds that version, with a warning naming the issue.
//   - An issue event before any version of its issue, which only a log that lacks the issue's
//     issue.created holds, as after a reset of the log that kept the issue's rows, reads the
//     issue from its rows (readIssueRows), with a warning naming the issue and the event: its
//     primary document (artifacts.is_primary), whose version at the event's time an
//     issue.updated or issue.closed holds, and for an artifact.version, which carries no
//     title, the issue's present title (bodyFromRow). An artifact.version of an issue whose
//     latest version names no primary document is held to artifacts.is_primary the same way.
//
// Each event counts against maxBytes, in order, what decider.size gives it, the count batchBytes
// states. The first event after the first whose count would take the total past maxBytes ends the
// events decided, and the next batch starts at it. Only an event decided is warned of, so the
// batch that writes an event warns of it once, and Cut counts the versions decided alone.
//
// logger takes the warnings. The first event whose payload a rule cannot read
// (errUnreadablePayload) ends the events decided too, where the bound has not ended them before
// it. It answers the versions decided with their bytes (decided.Bytes), how many events, from
// the first, it decided, and the error naming an unreadable payload where one ended them; with
// every event decided, that count is len(events). Any other error, a statement that failed,
// events out of id order or an ask's event read with no body (askVersion), answers no version.
func decideVersions(ctx context.Context, tx pgx.Tx, logger *slog.Logger, rules *ruleSet, events []event, maxBytes int64) (decided, int, error) {
	// A first pass runs each event through the rule of its type with no latest version
	// (noLatest), as though the event were its item's first, and names the item of each version
	// it starts, so the table's latest rows of all of them are read in one statement. That names
	// every item the second pass reads the latest version of: with no latest version, each rule
	// that reads one starts a version of that same item, and the item a rule's version names does
	// not depend on the latest version, so the second pass starts versions only of the items named
	// here. Each rule reads every field it can find missing or not a uuid whatever the latest
	// version, so the first pass meets the first unreadable payload.
	//
	// It names, too, the primary document version each version it starts reads. With those of
	// the latest rows read, these are every version a row of the batch can read the markdown of,
	// so they are measured in one statement (readSpecVersions): an issue.created's or an
	// artifact.version's version reads the same one whatever the latest version is, a version
	// that carries one forward carries that of a version of the batch or of a row read, and a
	// version of an issue with no version yet measures the one it reads of the issue's rows
	// itself (seedIssue).
	var items []item
	named := map[item]bool{}
	var specs []specVersion
	measured := map[specVersion]bool{}
	measure := func(v *version) {
		if key, ok := v.spec(); ok && !measured[key] {
			measured[key] = true
			specs = append(specs, key)
		}
	}
	read := len(events)
	var unreadable error
	for index, e := range events {
		if index > 0 && e.ID <= events[index-1].ID {
			return decided{}, 0, fmt.Errorf("decide the keyword versions: event %d comes after event %d, out of id order", e.ID, events[index-1].ID)
		}
		apply, ok := rules.rules[e.Type]
		if !ok {
			continue
		}
		v, started, err := apply(e, noLatest)
		if err != nil {
			if !errors.Is(err, errUnreadablePayload) {
				return decided{}, 0, eventError(e, err)
			}
			read, unreadable = index, eventError(e, err)
			break
		}
		if !started {
			continue
		}
		if key := (item{v.Kind, v.ID}); !named[key] {
			named[key] = true
			items = append(items, key)
		}
		measure(&v)
	}
	events = events[:read]
	stored, err := latestVersions(ctx, tx, items)
	if err != nil {
		return decided{}, 0, err
	}
	for _, v := range stored {
		measure(v)
	}
	texts, err := readSpecVersions(ctx, tx, specs)
	if err != nil {
		return decided{}, 0, err
	}
	d := &decider{tx: tx, logger: logger, rules: rules, stored: stored, batch: map[item]*version{}, specs: texts}
	for index, e := range events {
		v, starts, err := d.start(ctx, e)
		if err != nil {
			return decided{}, 0, eventError(e, err)
		}
		size, err := d.size(e, v, starts)
		if err != nil {
			return decided{}, 0, eventError(e, err)
		}
		if index > 0 && d.out.Bytes+size > maxBytes {
			return d.out, index, nil
		}
		d.out.Bytes += size
		d.flush()
		if starts {
			d.keep(v)
		}
	}
	return d.out, read, unreadable
}

// eventError names the event err was met at.
func eventError(e event, err error) error {
	return fmt.Errorf("decide the keyword version of event %d (%s): %w", e.ID, e.Type, err)
}

// decider is one batch's decision as decideVersions makes it: the rules it decides under, the
// latest versions the table held of the batch's items, the latest each item has taken in this
// batch and the seq of the first, the primary document versions the batch's rows can name,
// measured, the versions decided so far, and the warnings of the event being decided, which wait
// until it is known to be in the batch.
//
// A re-read batch (decideReread) gives it, in place of the latest versions, the row each event's
// item held just before the event's own seq (before, by event id), and the event being decided
// (event): an event read again comes after rows the table holds of its item's later events.
type decider struct {
	tx       pgx.Tx
	logger   *slog.Logger
	rules    *ruleSet
	stored   map[item]*version
	before   map[int64]*version
	event    int64
	batch    map[item]*version
	firsts   map[item]int32
	specs    map[specVersion]specText
	out      decided
	warnings []warning
}

// warning is a line the decider logs at WARN of an event once the event is in the batch
// (decider.flush).
type warning struct {
	message string
	args    []any
}

// latest is the latestVersion the batch's rules read: the item's latest version decided in this
// batch, else its latest row in the table. In a re-read batch it is the later of the item's
// version decided earlier in the batch and its row just before the event being decided (before):
// the batch reads an item's events in seq order, so a version it decided earlier comes before the
// event too.
func (d *decider) latest(kind, id string) *version {
	decidedHere, inBatch := d.batch[item{kind, id}]
	if d.before != nil {
		prior := d.before[d.event]
		if inBatch && (prior == nil || decidedHere.EventSeq > prior.EventSeq) {
			return decidedHere
		}
		return prior
	}
	if inBatch {
		return decidedHere
	}
	return d.stored[item{kind, id}]
}

// start runs e through the rule of its type with the batch's latest versions (latest), and
// completes the version it starts from the item's rows (complete): the version, and whether e
// starts one.
func (d *decider) start(ctx context.Context, e event) (version, bool, error) {
	apply, ok := d.rules.rules[e.Type]
	if !ok {
		return version{}, false, nil
	}
	d.event = e.ID
	v, started, err := apply(e, d.latest)
	if err != nil || !started {
		return version{}, false, err
	}
	keep, err := d.complete(ctx, e, &v)
	if err != nil || !keep {
		return version{}, false, err
	}
	return v, true, nil
}

// complete gives the version v that e starts what the event does not hold, from the item's rows
// (decideVersions), and answers whether v is still a version: false for an artifact.version the
// rows show is not of the issue's primary document, and for an edit whose whole body is its
// latest version's. It holds a warning (warn) of a post's version it keeps with its event's cut
// text, where no source holds the whole body (wholeBody), and of an issue.created version whose
// primary document's version 1 was not made at the event's created_at, by the create's own
// transaction, or is not there at all, which the version holds all the same. An edit whose
// whole body is its latest version's starts no version and is warned of nothing.
func (d *decider) complete(ctx context.Context, e event, v *version) (bool, error) {
	prior := d.latest(v.Kind, v.ID)
	switch v.Kind {
	case kindIssue:
		switch {
		case e.Type == "issue.created":
			if key, ok := v.spec(); ok {
				if made := d.specs[key].made; made == nil || !made.Equal(v.StartedAt) {
					d.warn("dispatch: the keyword fill found that the issue's create did not write version 1 of its primary document, which the issue's first version holds all the same",
						"issue", v.ID, "event_id", v.EventID, "artifact_id", key.artifact, "version_created_at", made, "event_created_at", v.StartedAt)
				}
			}
			return true, nil
		case prior == nil:
			return d.seedIssue(ctx, e, v)
		case e.Type == "artifact.version" && prior.SpecArtifactID == nil:
			rows, found, err := readIssueRows(ctx, d.tx, v.ID, e.CreatedAt)
			if err != nil {
				return false, err
			}
			return found && rows.primary != nil && *rows.primary == *v.SpecArtifactID, nil
		}
	case kindComment, kindMessage:
		if !v.BodyTruncated {
			return true, nil
		}
		if err := d.wholeBody(ctx, e, v); err != nil {
			return false, err
		}
		if prior != nil && prior.Body == v.Body {
			return false, nil
		}
		if v.BodyFrom == bodyFromCut {
			d.warn("dispatch: the keyword fill found no source that holds the whole body its event cut, so the version keeps the event's text",
				"kind", v.Kind, "id", v.ID, "event_id", e.ID, "event_type", e.Type)
		}
		return true, nil
	}
	return true, nil
}

// size is what e counts against the batch's bound, as batchBytes states it: the bytes
// readEventsQuery counted of it (event.PayloadBytes and event.RowBodyBytes) and, where e starts
// the version v, what v's vector reads that those do not hold. A version that names a document
// version no statement measured is a bug of the fill, answered with an error that says so rather
// than counted as nothing.
func (d *decider) size(e event, v version, starts bool) (int64, error) {
	size := e.PayloadBytes + e.RowBodyBytes
	if !starts {
		return size, nil
	}
	if v.BodyTruncated {
		size += max(int64(len(v.Body))-e.RowBodyBytes, 0)
	}
	if key, ok := v.spec(); ok {
		text, measured := d.specs[key]
		if !measured {
			return 0, fmt.Errorf("the issue's version names version %d of document %s, which the batch did not measure before its rules decided (readSpecVersions): a bug of the fill",
				key.number, key.artifact)
		}
		size += text.bytes
	}
	return size, nil
}

// keep adds v, which is in the batch, to the versions decided, as its item's latest version for
// the events after v's. A version that keeps its event's cut text counts in Cut.
func (d *decider) keep(v version) {
	if v.BodyFrom == bodyFromCut {
		d.out.Cut++
	}
	d.out.Versions = append(d.out.Versions, v)
	key := item{v.Kind, v.ID}
	d.batch[key] = &v
	if _, kept := d.firsts[key]; !kept {
		if d.firsts == nil {
			d.firsts = map[item]int32{}
		}
		d.firsts[key] = v.EventSeq
	}
}

// firstSeq is the seq of the first event of post (kind, id) where the keyword table holds no row
// of it, which the body history's read counts the post's changes from (historyBodyQuery): the
// seq of its first version decided in this batch, else seq, that of the event being decided,
// which is then the post's create. The create of a comment or of an issue's message always
// starts a version, so a post with no row has its create in the batch.
func (d *decider) firstSeq(kind, id string, seq int32) int32 {
	if first, kept := d.firsts[item{kind, id}]; kept {
		return first
	}
	return seq
}

// warn holds a warning of the event being decided until flush logs it, once the event is known
// to be in the batch: an event the bound leaves to the next batch is warned of by that batch.
// Each warning the decider logs of an event goes through it, from the code that decides the
// version (start and what it calls), since that code runs for the first event the bound leaves
// out too.
func (d *decider) warn(message string, args ...any) {
	d.warnings = append(d.warnings, warning{message: message, args: args})
}

// flush logs the warnings held of the event just decided, which is in the batch.
func (d *decider) flush() {
	for _, w := range d.warnings {
		d.logger.Warn(w.message, w.args...)
	}
	d.warnings = d.warnings[:0]
}

// seedIssue completes v, which e starts of an issue that has no version yet, from the issue's
// rows (decideVersions), and answers whether it is still a version.
func (d *decider) seedIssue(ctx context.Context, e event, v *version) (bool, error) {
	rows, found, err := readIssueRows(ctx, d.tx, v.ID, e.CreatedAt)
	if err != nil {
		return false, err
	}
	if !found {
		d.warn("dispatch: the keyword fill met an issue event that names an issue the log never created and the database does not hold; it starts no version",
			"issue", v.ID, "event_id", e.ID, "event_type", e.Type)
		return false, nil
	}
	if e.Type == "artifact.version" {
		if rows.primary == nil || *rows.primary != *v.SpecArtifactID {
			return false, nil
		}
		v.Body, v.BodyFrom = rows.title, bodyFromRow
	} else {
		if v.SpecArtifactID == nil {
			v.SpecArtifactID = rows.primary
		}
		if v.SpecArtifactID != nil && rows.primary != nil && *v.SpecArtifactID == *rows.primary {
			v.SpecVersion = rows.specVersion
			if key, ok := v.spec(); ok {
				if _, measured := d.specs[key]; !measured {
					d.specs[key] = specText{bytes: rows.specBytes}
				}
			}
		}
	}
	d.warn("dispatch: the keyword fill found no earlier version of the issue in the log, so its version reads the issue's rows",
		"issue", v.ID, "event_id", e.ID, "event_type", e.Type, "body_from", v.BodyFrom)
	return true, nil
}

// issueRows is what an issue's rows say of it at a time: its present title, its primary
// document, the latest version of that document made at or before the time, and the bytes of
// that version's markdown, which a row that names it reads into its vector.
type issueRows struct {
	title       string
	primary     *string
	specVersion *int32
	specBytes   int64
}

// issueRowsQuery reads issueRows of issue $1 at time $2.
const issueRowsQuery = `
	select i.title, a.id::text, s.number, coalesce(s.bytes, 0)
	  from public.issues i
	  left join public.artifacts a on a.issue_key = i.key and a.is_primary
	  left join lateral (
	        select av.number, octet_length(av.markdown) as bytes from public.artifact_versions av
	         where av.artifact_id = a.id and av.created_at <= $2
	         order by av.number desc
	         limit 1) s on true
	 where i.key = $1`

// readIssueRows reads issueRows of issue key at time at; found is false when no issue has the
// key.
func readIssueRows(ctx context.Context, tx pgx.Tx, key string, at time.Time) (issueRows, bool, error) {
	var rows issueRows
	err := tx.QueryRow(ctx, issueRowsQuery, key, at).Scan(&rows.title, &rows.primary, &rows.specVersion, &rows.specBytes)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return issueRows{}, false, nil
	case err != nil:
		return issueRows{}, false, fmt.Errorf("read issue %s: %w", key, err)
	}
	return rows, true, nil
}

// specVersion names a version of an issue's primary document as the rules carry it: the
// document's id as the version names it, and the version's number.
type specVersion struct {
	artifact string
	number   int32
}

// spec is the primary document version v names, whose markdown the insert reads into v's vector
// (insertVersionsQuery): ok is false for a version that names none, as every version of a kind
// other than an issue.
func (v *version) spec() (specVersion, bool) {
	if v.SpecArtifactID == nil || v.SpecVersion == nil {
		return specVersion{}, false
	}
	return specVersion{artifact: *v.SpecArtifactID, number: *v.SpecVersion}, true
}

// specText is what a batch measures of a version of a primary document (readSpecVersions): the
// bytes of its markdown, 0 where no such version is or it holds none, and when it was made, nil
// where no such version is.
type specText struct {
	bytes int64
	made  *time.Time
}

// specVersionsQuery measures each primary document version ($1 its documents' ids and $2 its
// numbers): the bytes of its markdown, read from the column's own length without reading the
// markdown, and when it was made. A version that is not there is answered with nulls.
const specVersionsQuery = `
	select s.artifact_id, s.number, octet_length(v.markdown), v.created_at
	  from unnest($1::text[], $2::integer[]) as s (artifact_id, number)
	  left join public.artifact_versions v on v.artifact_id = s.artifact_id::uuid and v.number = s.number`

// readSpecVersions measures specs in tx, in one statement (specVersionsQuery): each of them is in
// the map.
func readSpecVersions(ctx context.Context, tx pgx.Tx, specs []specVersion) (map[specVersion]specText, error) {
	texts := make(map[specVersion]specText, len(specs))
	if len(specs) == 0 {
		return texts, nil
	}
	artifacts := make([]string, len(specs))
	numbers := make([]int32, len(specs))
	for index, key := range specs {
		artifacts[index], numbers[index] = key.artifact, key.number
	}
	rows, err := tx.Query(ctx, specVersionsQuery, artifacts, numbers)
	if err != nil {
		return nil, fmt.Errorf("measure %d versions of the issues' primary documents: %w", len(specs), err)
	}
	defer rows.Close()
	for rows.Next() {
		var key specVersion
		var bytes *int64
		var text specText
		if err := rows.Scan(&key.artifact, &key.number, &bytes, &text.made); err != nil {
			return nil, fmt.Errorf("measure %d versions of the issues' primary documents: %w", len(specs), err)
		}
		if bytes != nil {
			text.bytes = *bytes
		}
		texts[key] = text
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("measure %d versions of the issues' primary documents: %w", len(specs), err)
	}
	return texts, nil
}

// latestVersionsQuery reads, for each item ($1 its kinds and $2 its ids), its latest row in the
// keyword table: the one with the greatest event_seq, read backwards through the primary key
// (kind, id, event_seq), one probe for each item. It reads the columns the rules compare and carry
// forward alone.
const latestVersionsQuery = `
	select l.kind, l.id, l.body, l.body_from, l.spec_artifact_id::text, l.spec_version
	  from unnest($1::text[], $2::text[]) as k (kind, id)
	  cross join lateral (
	        select v.kind, v.id, v.body, v.body_from, v.spec_artifact_id, v.spec_version
	          from public.search_text_versions v
	         where v.kind = k.kind and v.id = k.id
	         order by v.event_seq desc
	         limit 1) l`

// latestVersions reads the latest row in the keyword table of each of items, in tx: an item with
// no row is not in the map. A version read from the table holds its kind, its id, its body as its
// Body, its BodyFrom and its primary document version, and none of its other columns.
func latestVersions(ctx context.Context, tx pgx.Tx, items []item) (map[item]*version, error) {
	latest := make(map[item]*version, len(items))
	if len(items) == 0 {
		return latest, nil
	}
	kinds := make([]string, len(items))
	ids := make([]string, len(items))
	for index, key := range items {
		kinds[index], ids[index] = key.kind, key.id
	}
	rows, err := tx.Query(ctx, latestVersionsQuery, kinds, ids)
	if err != nil {
		return nil, fmt.Errorf("read the latest keyword versions of %d items: %w", len(items), err)
	}
	defer rows.Close()
	for rows.Next() {
		var v version
		if err := rows.Scan(&v.Kind, &v.ID, &v.Body, &v.BodyFrom, &v.SpecArtifactID, &v.SpecVersion); err != nil {
			return nil, fmt.Errorf("read the latest keyword versions of %d items: %w", len(items), err)
		}
		latest[item{v.Kind, v.ID}] = &v
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the latest keyword versions of %d items: %w", len(items), err)
	}
	return latest, nil
}
