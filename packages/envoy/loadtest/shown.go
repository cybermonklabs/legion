package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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

// issueKeyPattern is an issue key as Dispatch names one: a project key (contracts.ProjectKeyPattern),
// a hyphen and a positive decimal with no leading zero.
var issueKeyPattern = regexp.MustCompile(strings.TrimSuffix(contracts.ProjectKeyPattern, "$") + `-[1-9][0-9]*$`)

// isIssueKey says whether s is an issue key as Dispatch names one (issueKeyPattern).
func isIssueKey(s string) bool { return issueKeyPattern.MatchString(s) }

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
// ask in any other shape is refused (eventInShape, listingInShape), and the issue listing names only
// the project's issue keys (issueNumber). So every id and key a bundle holds is in Dispatch's shape,
// and the seed, the plan, the run and the checks, which read them from the bundle, name them as they
// are, as replay-fetch names a cache or bundle file after them.

// anchorIDs is the document an anchor names.
type anchorIDs struct {
	ArtifactID string `json:"artifact_id"`
}

// eventInShape says whether ev names every issue in an issue key's shape (isIssueKey) and every
// document, comment and ask by an id in Dispatch's shape (isDispatchID), as the load test reads
// them: the envelope's issue key and document, and the payload's members that the decoders of
// replay_reconstruct.go (wireIssue, wireComment, wireAsk, windowAnchor, createdSpecs) and
// replay_plan.go (rpIssueCreated, rpComment, rpAsk, rpArtifactCreated, rpVersion) read: an issue
// event's key, parent, blockers and primary document; a comment's or an ask's own id, document,
// thread, ask, anchor's document, block's document, approval's document and nested ask; a created
// artifact's id; a version's document. A member that is absent, null or empty names nothing. An
// event whose payload does not decode into those members' types is not in shape: no decoder of the
// load test reads it either.
func eventInShape(ev replayEvent) bool {
	if (ev.IssueKey != nil && !isIssueKey(*ev.IssueKey)) || (ev.ArtifactID != nil && !isDispatchID(*ev.ArtifactID)) {
		return false
	}
	var keys, ids []string
	decode := func(v any) bool { return len(ev.Payload) == 0 || json.Unmarshal(ev.Payload, v) == nil }
	switch {
	case strings.HasPrefix(ev.Type, evPrefixIssue):
		var p struct {
			Key               string   `json:"key"`
			Parent            *string  `json:"parent"`
			BlockedBy         []string `json:"blocked_by"`
			PrimaryArtifactID *string  `json:"primary_artifact_id"`
		}
		if !decode(&p) {
			return false
		}
		keys = append(append(keys, p.Key, orEmpty(p.Parent)), p.BlockedBy...)
		ids = append(ids, orEmpty(p.PrimaryArtifactID))
	case commentStates[ev.Type], strings.HasPrefix(ev.Type, evPrefixAsk):
		var p struct {
			ID            string     `json:"id"`
			ArtifactID    *string    `json:"artifact_id"`
			ReplyTo       *string    `json:"reply_to"`
			ThreadRootID  *string    `json:"thread_root_id"`
			AskID         *string    `json:"ask_id"`
			Anchor        *anchorIDs `json:"anchor"`
			BlockArtifact *struct {
				ID string `json:"id"`
			} `json:"block_artifact"`
			Approval *anchorIDs `json:"approval"`
			Ask      *struct {
				ID     string     `json:"id"`
				Anchor *anchorIDs `json:"anchor"`
			} `json:"ask"`
		}
		if !decode(&p) {
			return false
		}
		ids = append(ids, p.ID, orEmpty(p.ArtifactID), orEmpty(p.ReplyTo), orEmpty(p.ThreadRootID), orEmpty(p.AskID))
		for _, a := range []*anchorIDs{p.Anchor, p.Approval} {
			if a != nil {
				ids = append(ids, a.ArtifactID)
			}
		}
		if p.BlockArtifact != nil {
			ids = append(ids, p.BlockArtifact.ID)
		}
		if p.Ask != nil {
			ids = append(ids, p.Ask.ID)
			if p.Ask.Anchor != nil {
				ids = append(ids, p.Ask.Anchor.ArtifactID)
			}
		}
	case ev.Type == evArtifactCreated:
		var p struct {
			Artifact struct {
				ID string `json:"id"`
			} `json:"artifact"`
		}
		if !decode(&p) {
			return false
		}
		ids = append(ids, p.Artifact.ID)
	case ev.Type == evArtifactVersion:
		var p struct {
			ArtifactID string `json:"artifact_id"`
		}
		if !decode(&p) {
			return false
		}
		ids = append(ids, p.ArtifactID)
	}
	return !slices.ContainsFunc(keys, func(k string) bool { return k != "" && !isIssueKey(k) }) &&
		!slices.ContainsFunc(ids, func(id string) bool { return id != "" && !isDispatchID(id) })
}

// listingInShape says whether an artifact listing names every artifact by an id in Dispatch's shape
// (isDispatchID).
func listingInShape(listing []listedArtifact) bool {
	return !slices.ContainsFunc(listing, func(la listedArtifact) bool { return !isDispatchID(la.ID) })
}

// orEmpty is *s, and "" for nil.
func orEmpty(s *string) string {
	if s == nil {
		return ""
	}
	return *s
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
