// comment_delete_version_test.go holds R3 of dispatch://INFRA-1523 on a document's versions: an
// accept names the version it makes after the suggestion's body (acceptedVersionName) and records
// that version by the comment's id (artifact_versions.accepted_from), and the delete of that
// comment renames the version (renameAcceptedVersion, comment_delete.go).
package api

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// versionsLinkedTo is the number of each version of the document that records the comment's
// accept as the one that made it (artifact_versions.accepted_from), in number order.
func versionsLinkedTo(t *testing.T, database *store.Store, artifactID, commentID string) []int {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select number from artifact_versions where artifact_id = $1 and accepted_from = $2 order by number
	`, artifactID, commentID)
	if err != nil {
		t.Fatalf("read the versions linked to comment %s: %v", commentID, err)
	}
	numbers, err := pgx.CollectRows(rows, pgx.RowTo[int])
	if err != nil {
		t.Fatalf("read the versions linked to comment %s: %v", commentID, err)
	}
	return numbers
}

// unlinkAcceptedVersion takes the comment's link off every version of the document, which is how
// a version stands that an accept made before the accept recorded the link.
func unlinkAcceptedVersion(t *testing.T, database *store.Store, artifactID, commentID string) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), `
		update artifact_versions set accepted_from = null where artifact_id = $1 and accepted_from = $2
	`, artifactID, commentID); err != nil {
		t.Fatalf("take comment %s's link off its accepted version: %v", commentID, err)
	}
}

// giveVersionTheTimeOf gives version other of the document the created_at of version accepted,
// so a lookup of the accepted version by its accept's time finds both.
func giveVersionTheTimeOf(t *testing.T, database *store.Store, artifactID string, accepted, other int) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), `
		update artifact_versions set created_at = (
			select created_at from artifact_versions where artifact_id = $1 and number = $2
		) where artifact_id = $1 and number = $3
	`, artifactID, accepted, other); err != nil {
		t.Fatalf("give version %d the accepted version's time: %v", other, err)
	}
}

// documentVersionNames reads the document's versions as GET /api/v1/artifacts/{id} serves them,
// each version's name by its number.
func documentVersionNames(t *testing.T, handler http.Handler, artifactID string) map[int]string {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+artifactID, nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("read the document: status=%d body=%s", response.Code, response.Body.String())
	}
	read := decodeBody[struct {
		Versions []model.Version `json:"versions"`
	}](t, response)
	names := make(map[int]string, len(read.Versions))
	for _, version := range read.Versions {
		if version.Summary != nil {
			names[version.Number] = *version.Summary
		}
	}
	return names
}

// versionNamed is the number of the one version whose name holds text, failing t unless exactly
// one does.
func versionNamed(t *testing.T, names map[int]string, text string) int {
	t.Helper()
	found := 0
	for number, name := range names {
		if strings.Contains(name, text) {
			if found != 0 {
				t.Fatalf("versions %d and %d are both named with %q: %v", found, number, text, names)
			}
			found = number
		}
	}
	if found == 0 {
		t.Fatalf("no version is named with %q: %v", text, names)
	}
	return found
}

// acceptSuggestionNamed opens a suggestion on quote whose body holds phrase, accepts it, and
// returns the comment.
func acceptSuggestionNamed(t *testing.T, handler http.Handler, issueKey, quote, phrase string) model.Comment {
	t.Helper()
	comment := createThreadComment(t, handler, issueKey, map[string]any{
		"body": "Say this instead, " + phrase + ".", "anchor": map[string]any{"artifact": "spec", "quote": quote},
		"suggestion": map[string]string{"replace_with": "slow"},
	}, "alice")
	if accepted := dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+comment.ID+"/accept", map[string]any{}, "alice"); accepted.Code != http.StatusOK {
		t.Fatalf("accept: status=%d body=%s", accepted.Code, accepted.Body.String())
	}
	return comment
}

// The delete renames the version the accept made to the name an accept gives a suggestion with no
// body, and touches no other version; the artifact.version event keeps the name it carried, as
// every event keeps its payload.
func TestDeletingAnAcceptedSuggestionRenamesTheVersionItMade(t *testing.T) {
	_, handler, database := browserDocumentService(t)
	issue := createInteractionIssue(t, handler, "TEST", "Rename the accepted version", "The quick brown fox")
	const phrase = "zarquonrenamedversion"
	comment := acceptSuggestionNamed(t, handler, issue.Key, "quick", phrase)
	before := documentVersionNames(t, handler, issue.PrimaryArtifactID)
	accepted := versionNamed(t, before, phrase)

	deleteThreadComment(t, handler, comment.ID, "alice")

	after := documentVersionNames(t, handler, issue.PrimaryArtifactID)
	if want := `Accepted suggestion: "quick" → "slow"`; after[accepted] != want {
		t.Fatalf("version %d after the delete is named %q, want %q", accepted, after[accepted], want)
	}
	for number, name := range before {
		if number != accepted && after[number] != name {
			t.Fatalf("version %d was renamed from %q to %q; the delete renames the accepted version alone", number, name, after[number])
		}
	}
	var kept int
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) from events
		where issue_key = $1 and type = 'artifact.version' and strpos(payload::text, $2) > 0
	`, issue.Key, phrase).Scan(&kept); err != nil {
		t.Fatalf("read the artifact.version events: %v", err)
	}
	if kept != 1 {
		t.Fatalf("%d artifact.version events hold the name the accept gave, want the accept's own one, kept as written", kept)
	}
}

// A version no link names - one an accept made before the accept recorded the link - is found by
// the time its accept wrote it, so another named version of the document carrying that same time
// makes the lookup ambiguous: the delete renames neither, rather than one it cannot tell is the
// accept's, and stands.
func TestADeleteRenamesNoVersionWhenTwoCarryTheAcceptsTime(t *testing.T) {
	_, handler, database := browserDocumentService(t)
	issue := createInteractionIssue(t, handler, "TEST", "Two versions at one time", "The quick brown fox")
	const byHand = "A version named by hand"
	if named := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/versions",
		map[string]any{"summary": byHand}, "alice"); named.Code != http.StatusCreated {
		t.Fatalf("name a version: status=%d body=%s", named.Code, named.Body.String())
	}
	const phrase = "zarquonambiguousversion"
	comment := acceptSuggestionNamed(t, handler, issue.Key, "quick", phrase)
	before := documentVersionNames(t, handler, issue.PrimaryArtifactID)
	accepted, other := versionNamed(t, before, phrase), versionNamed(t, before, byHand)
	giveVersionTheTimeOf(t, database, issue.PrimaryArtifactID, accepted, other)
	unlinkAcceptedVersion(t, database, issue.PrimaryArtifactID, comment.ID)

	deleteThreadComment(t, handler, comment.ID, "alice")

	after := documentVersionNames(t, handler, issue.PrimaryArtifactID)
	if after[accepted] != before[accepted] || after[other] != byHand {
		t.Fatalf("after the delete the versions are named %v, want %v: an ambiguous lookup renames none", after, before)
	}
}

// The accept records the version it made, so the delete renames that version by the record: a
// second named version of the document carrying the accept's very time, which leaves the lookup by
// time no single version, is no candidate, and keeps the name it was given.
func TestADeleteRenamesTheVersionItsAcceptRecordedWhereAnotherCarriesTheAcceptsTime(t *testing.T) {
	_, handler, database := browserDocumentService(t)
	issue := createInteractionIssue(t, handler, "TEST", "The accept's own version", "The quick brown fox")
	const byHand = "A version named by hand"
	if named := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/versions",
		map[string]any{"summary": byHand}, "alice"); named.Code != http.StatusCreated {
		t.Fatalf("name a version: status=%d body=%s", named.Code, named.Body.String())
	}
	const phrase = "zarquonrecordedversion"
	comment := acceptSuggestionNamed(t, handler, issue.Key, "quick", phrase)
	before := documentVersionNames(t, handler, issue.PrimaryArtifactID)
	accepted, other := versionNamed(t, before, phrase), versionNamed(t, before, byHand)
	if linked := versionsLinkedTo(t, database, issue.PrimaryArtifactID, comment.ID); !slices.Equal(linked, []int{accepted}) {
		t.Fatalf("the versions recording comment %s's accept = %v, want the version %d it made alone", comment.ID, linked, accepted)
	}
	giveVersionTheTimeOf(t, database, issue.PrimaryArtifactID, accepted, other)

	deleteThreadComment(t, handler, comment.ID, "alice")

	after := documentVersionNames(t, handler, issue.PrimaryArtifactID)
	if want := `Accepted suggestion: "quick" → "slow"`; after[accepted] != want {
		t.Fatalf("version %d after the delete is named %q, want %q", accepted, after[accepted], want)
	}
	if after[other] != byHand {
		t.Fatalf("version %d after the delete is named %q, want %q: the delete renames the recorded version alone", other, after[other], byHand)
	}
}

// A version no link names - one an accept made before the accept recorded the link - is still
// found by the time its accept wrote it, and renamed, where no other named version of the document
// carries that time.
func TestADeleteFindsAnAcceptedVersionNoLinkNamesByTheAcceptsTime(t *testing.T) {
	_, handler, database := browserDocumentService(t)
	issue := createInteractionIssue(t, handler, "TEST", "An accept before the link", "The quick brown fox")
	const phrase = "zarquonunlinkedversion"
	comment := acceptSuggestionNamed(t, handler, issue.Key, "quick", phrase)
	accepted := versionNamed(t, documentVersionNames(t, handler, issue.PrimaryArtifactID), phrase)
	unlinkAcceptedVersion(t, database, issue.PrimaryArtifactID, comment.ID)
	if linked := versionsLinkedTo(t, database, issue.PrimaryArtifactID, comment.ID); len(linked) != 0 {
		t.Fatalf("the versions recording comment %s's accept after the unlink = %v, want none", comment.ID, linked)
	}

	deleteThreadComment(t, handler, comment.ID, "alice")

	if want, after := `Accepted suggestion: "quick" → "slow"`, documentVersionNames(t, handler, issue.PrimaryArtifactID); after[accepted] != want {
		t.Fatalf("version %d after the delete is named %q, want %q", accepted, after[accepted], want)
	}
}
