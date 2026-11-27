package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"strings"

	"github.com/sjawhar/envoy/internal/contracts"
)

// A member of Dispatch's answer that the load test decodes as a plain string and keeps or decides
// on as it came - an issue key, an object's id, an event's type - is Dispatch's words, or the words
// of whatever answers in Dispatch's place, which can echo the request back, its bearer and all. A
// log line, an error or a file the load test writes names such a member only in the one shape
// Dispatch gives it, and a fixed phrase in place of anything else, as the daemon names GitHub's
// members (githubrest.ShownSHA): nothing else the answer wrote there prints, and every decision is
// still made on the member as it came. A shape and not the rule's reading, since a member the load
// test keeps outlives the record of the request it answered: seed.json and the id maps hold it for
// later runs, and replay-fetch reads most answers back from its cache.

// issueKeyDigits is how many digits an issue key's number may have: ten, which is every number
// Dispatch can give an issue, since it numbers one from its project's own counter into an integer
// column (internal/dispatch/store/migrations/0001_init.up.sql: issues.number, whose largest value
// is 2,147,483,647). The bound is what makes a key short, which the file names built from one
// rest on (replay_bundle.go): a key is a project key's ten bytes at most, a hyphen and these
// digits, so 21 bytes.
const issueKeyDigits = 10

// isIssueKey says whether s is an issue key as Dispatch names one: a project key and a hyphen, as
// contracts.IssueProject reads them, then a positive decimal of at most issueKeyDigits digits with
// no leading zero. It reads the project key through IssueProject, the package that owns that
// grammar, and not by editing the text of contracts.ProjectKeyPattern.
func isIssueKey(s string) bool {
	project, ok := contracts.IssueProject(s)
	if !ok {
		return false
	}
	// IssueProject read s as the project, a hyphen and a decimal of at least one digit.
	number := s[len(project)+1:]
	return number[0] != '0' && len(number) <= issueKeyDigits
}

// notAnIssueKey is what shownKey prints in place of a key that is not an issue key.
const notAnIssueKey = "(a key that is not an issue key)"

// shownKey is key, an issue key a Dispatch answer names, as a message may print it: the key where
// it is an issue key (isIssueKey), and notAnIssueKey otherwise.
func shownKey(key string) string {
	if isIssueKey(key) {
		return key
	}
	return notAnIssueKey
}

// dispatchIDPattern is an object id as Dispatch names one: a UUID in the canonical lowercase form
// Postgres writes the uuid that keys every artifact, comment and ask
// (internal/dispatch/store/migrations/0001_init.up.sql).
var dispatchIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// isDispatchID says whether s is an object id as Dispatch names one (dispatchIDPattern).
func isDispatchID(s string) bool { return dispatchIDPattern.MatchString(s) }

// replay-fetch settles the shape of the ids and keys a bundle keeps where it decodes the answer that
// names them, once: an event or an artifact listing that names an issue, a document, a comment or an
// ask in any other shape is refused (eventInShape, readListing), and the issue listing names only
// the project's issue keys (issueNumber). So every id and key a bundle holds is in Dispatch's shape,
// and the seed, the plan, the run and the checks, which read them from the bundle, name them as they
// are, as replay-fetch names a cache or bundle file after them.

// dispatchID and issueKey are an object id and an issue key of Dispatch's shape, as a decoder
// takes one: a member of this type decodes from a JSON string in that shape, from a JSON null and
// from the empty string, which name nothing, and from nothing else. A decoder that reads a member
// of Dispatch's into one of these refuses an answer out of shape where it decodes it, with no
// reader of that member left to remember the check.
type (
	dispatchID string
	issueKey   string
)

// The refusals of a member out of shape. Each is fixed words: whatever answers in Dispatch's
// place writes what it likes in a member, its reader's own bearer among it, so no part of the
// member is named.
var (
	errNotADispatchID = errors.New("json: a string that is not an object id in Dispatch's shape")
	errNotAnIssueKey  = errors.New("json: a string that is not an issue key in Dispatch's shape")
)

// UnmarshalJSON decodes a JSON string holding an id of Dispatch's shape (shapedMember).
func (id *dispatchID) UnmarshalJSON(data []byte) error {
	s, err := shapedMember(data, isDispatchID, errNotADispatchID)
	*id = dispatchID(s)
	return err
}

// UnmarshalJSON decodes a JSON string holding an issue key of Dispatch's shape (shapedMember).
func (k *issueKey) UnmarshalJSON(data []byte) error {
	s, err := shapedMember(data, isIssueKey, errNotAnIssueKey)
	*k = issueKey(s)
	return err
}

// shapedMember is the string data holds, where inShape takes it; "" for a JSON null, which
// encoding/json decodes into a string as nothing at all, and for the empty string, neither of
// which names anything, as every reader of one of these members reads them; and notInShape for
// any other string. A JSON value that is no string is encoding/json's own type error for a
// string, which names no value.
func shapedMember(data []byte, inShape func(string) bool, notInShape error) (string, error) {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return "", err
	}
	if s != "" && !inShape(s) {
		return "", notInShape
	}
	return s, nil
}

// anchorIDs is the document an anchor names.
type anchorIDs struct {
	ArtifactID dispatchID `json:"artifact_id"`
}

// eventInShape says whether ev names every issue in an issue key's shape (issueKey) and every
// document, comment and ask by an id in Dispatch's shape (dispatchID), as the load test reads
// them: the envelope's issue key and document, and the payload's members that the decoders of
// replay_reconstruct.go (wireIssue, wireComment, wireAsk, windowAnchor, createdSpecs) and
// replay_plan.go (rpIssueCreated, rpComment, rpAsk, rpArtifactCreated, rpVersion) read: an issue
// event's key, parent, blockers and primary document; a comment's or an ask's own id, document,
// thread, ask, anchor's document, block's document, approval's document and nested ask; a created
// artifact's id; a version's document. Each is read into its shape's own type, so the shape is
// the decode and no member of one of these payloads is checked a second time. A member that is
// absent, null or empty names nothing. An event whose payload does not decode into those members'
// types is not in shape: no decoder of the load test reads it either.
func eventInShape(ev replayEvent) bool {
	if (ev.IssueKey != nil && !isIssueKey(*ev.IssueKey)) || (ev.ArtifactID != nil && !isDispatchID(*ev.ArtifactID)) {
		return false
	}
	decode := func(v any) bool { return len(ev.Payload) == 0 || json.Unmarshal(ev.Payload, v) == nil }
	switch {
	case strings.HasPrefix(ev.Type, evPrefixIssue):
		var p struct {
			Key               issueKey    `json:"key"`
			Parent            *issueKey   `json:"parent"`
			BlockedBy         []issueKey  `json:"blocked_by"`
			PrimaryArtifactID *dispatchID `json:"primary_artifact_id"`
		}
		return decode(&p)
	case commentStates[ev.Type], strings.HasPrefix(ev.Type, evPrefixAsk):
		var p struct {
			ID            dispatchID  `json:"id"`
			ArtifactID    *dispatchID `json:"artifact_id"`
			ReplyTo       *dispatchID `json:"reply_to"`
			ThreadRootID  *dispatchID `json:"thread_root_id"`
			AskID         *dispatchID `json:"ask_id"`
			Anchor        *anchorIDs  `json:"anchor"`
			BlockArtifact *struct {
				ID dispatchID `json:"id"`
			} `json:"block_artifact"`
			Approval *anchorIDs `json:"approval"`
			Ask      *struct {
				ID     dispatchID `json:"id"`
				Anchor *anchorIDs `json:"anchor"`
			} `json:"ask"`
		}
		return decode(&p)
	case ev.Type == evArtifactCreated:
		var p struct {
			Artifact struct {
				ID dispatchID `json:"id"`
			} `json:"artifact"`
		}
		return decode(&p)
	case ev.Type == evArtifactVersion:
		var p struct {
			ArtifactID dispatchID `json:"artifact_id"`
		}
		return decode(&p)
	}
	return true
}

// replayEventTypes are the event types this package names (the ev constants of replay_bundle.go).
var replayEventTypes = []string{
	evIssueCreated, evIssueUpdated, evIssueClosed, evIssueClaimed, evIssueReleased,
	evCommentCreated, evCommentEdited, evCommentResolved, evCommentReopened, evCommentAnchorRefreshed,
	evArtifactCreated, evArtifactVersion,
	evAskOpened, evAskAnswered, evAskResolved,
	evMessageCreated, evMessageDelivery, evMessageAccepted,
	evChildStatus,
}

// notAReplayEventType is what shownEventType prints in place of a type this package does not name.
const notAReplayEventType = "a type the replay does not name"

// shownEventType is t, an event's type as Dispatch's event log names it, as a message may print
// it: t where it is one of replayEventTypes, and notAReplayEventType otherwise. A caller that
// decodes a family of events (evPrefixIssue, evPrefixAsk) takes every type the family's prefix
// begins, so a family is no shape.
func shownEventType(t string) string {
	if slices.Contains(replayEventTypes, t) {
		return t
	}
	return notAReplayEventType
}

// referencesETagPattern is the ETag Dispatch writes for a references answer: 64 lowercase
// hexadecimal digits in double quotes (internal/dispatch/api/references.go, referencesETag).
var referencesETagPattern = regexp.MustCompile(`^"[0-9a-f]{64}"$`)

// shownETag is etag, the ETag header of a references answer, as refs-fetch's TSV records it: the
// ETag where it is one Dispatch writes (referencesETagPattern) or empty, and otherwise "sha256:"
// and the hex SHA-256 of what the answer sent there. Two answers whose ETags differ still record
// two values, since the TSV compares two builds' answers, and nothing the answer wrote prints.
func shownETag(etag string) string {
	if etag == "" || referencesETagPattern.MatchString(etag) {
		return etag
	}
	digest := sha256.Sum256([]byte(etag))
	return "sha256:" + hex.EncodeToString(digest[:])
}
