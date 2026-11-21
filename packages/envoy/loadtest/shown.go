package main

import (
	"crypto/sha256"
	"encoding/hex"
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

// notADispatchID is what shownID prints in place of an id that is not one of Dispatch's.
const notADispatchID = "(an id that is not one of Dispatch's)"

// shownID is id, an object id a Dispatch answer names, as a message may print it: the id where it
// is one of Dispatch's (isDispatchID), and notADispatchID otherwise.
func shownID(id string) string {
	if isDispatchID(id) {
		return id
	}
	return notADispatchID
}

// bundleFileName is s, a document id or an issue key a Dispatch answer names, as replay-fetch names
// a file of its cache or of the bundle after it: s where it is in Dispatch's shape (isDispatchID,
// isIssueKey), which holds no path separator, no ".." and no more than a few dozen bytes, and
// otherwise the hex SHA-256 of s, which is neither shape, so no two names meet.
func bundleFileName(s string) string {
	if isDispatchID(s) || isIssueKey(s) {
		return s
	}
	digest := sha256.Sum256([]byte(s))
	return hex.EncodeToString(digest[:])
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
