package api

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// The architect's decision of 2026-11-20 09:18Z on dispatch://INFRA-1523: the delete of a comment
// whose accepted suggestion named a document version after the comment's body renames that
// version (renameAcceptedVersion), and no event of the document says so, so a reader holding the
// document's versions - the web app's cached issue read - kept the old name, the comment's words,
// once its refetch failed. The delete's own comment.deleted names the version and the name it took
// (renamed_version), as the events table stores it and as the event log reads it. That name quotes
// the suggestion's quote and replacement, the document's own text since the accept, and none of
// the comment's body.
func TestTheDeleteOfAnAcceptedSuggestionNamesTheVersionItRenamed(t *testing.T) {
	t.Parallel()
	_, handler, database := browserDocumentService(t)
	issue := createInteractionIssue(t, handler, "TEST", "Name the renamed version", "The quick brown fox")
	const phrase = "zarquonversionbody"
	comment := acceptSuggestionNamed(t, handler, issue.Key, "quick", phrase)
	accepted := versionNamed(t, documentVersionNames(t, handler, issue.PrimaryArtifactID), phrase)
	const renamed = `Accepted suggestion: "quick" → "slow"`
	want := map[string]any{"artifact_id": issue.PrimaryArtifactID, "number": float64(accepted), "summary": renamed}

	beforeDelete := lastEventID(t, database)
	deleteThreadComment(t, handler, comment.ID, "alice")

	// The control: every read of the document's versions gives the name the event names.
	if after := documentVersionNames(t, handler, issue.PrimaryArtifactID); after[accepted] != renamed {
		t.Fatalf("version %d after the delete is named %q, want %q", accepted, after[accepted], renamed)
	}
	deletes := 0
	for _, event := range storedCommentEvents(t, database, comment.ID, beforeDelete) {
		if event.eventType != "comment.deleted" {
			continue
		}
		deletes++
		if got := event.payload["renamed_version"]; !reflect.DeepEqual(got, want) {
			t.Errorf("the stored comment.deleted names the renamed version %#v, want %#v", got, want)
		}
		if strings.Contains(event.raw, phrase) {
			t.Errorf("the stored comment.deleted carries the comment's words %q: %s", phrase, event.raw)
		}
	}
	if deletes != 1 {
		t.Fatalf("the delete stored %d comment.deleted events, want one", deletes)
	}
	read := 0
	for _, event := range decodedIssueEvents(t, handler, issue.Key) {
		if event.Type != "comment.deleted" || event.Payload["id"] != comment.ID {
			continue
		}
		read++
		if got := event.Payload["renamed_version"]; !reflect.DeepEqual(got, want) {
			t.Errorf("the event log's comment.deleted names the renamed version %#v, want %#v", got, want)
		}
		encoded, err := json.Marshal(event.Payload)
		if err != nil {
			t.Fatalf("encode the event log's comment.deleted: %v", err)
		}
		if strings.Contains(string(encoded), phrase) {
			t.Errorf("the event log's comment.deleted carries the comment's words %q: %s", phrase, encoded)
		}
	}
	if read != 1 {
		t.Fatalf("the event log reads %d comment.deleted events of the comment, want one", read)
	}
}
