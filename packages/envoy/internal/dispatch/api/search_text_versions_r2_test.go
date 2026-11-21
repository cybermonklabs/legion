// search_text_versions_r2_test.go holds INFRA-4684's R2 (dispatch://INFRA-4684/spec#b-k-deleted)
// over dispatch://INFRA-1523's routes: the keyword table's fill reads the edit of an issue
// message and the delete of a comment or a message, and the relation of the versions at a time
// (store.TextVersions) gives every version of a post the post's present deleted_at: the rows
// that INFRA-4684's plan names.
package api

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// postKeywordVersion is one row of the keyword table as these tests read it, with the type of the
// event that started it and whether its vector is the one its body makes
// (search_vector over search_text, 0087).
type postKeywordVersion struct {
	eventType, body, bodyFrom string
	startedAt                 time.Time
	vectorOfBody              bool
}

// keywordVersions is each row of the keyword table of item (kind, id), in its owner's seq order.
func keywordVersions(t *testing.T, database *store.Store, kind, id string) []postKeywordVersion {
	t.Helper()
	rows, err := database.Pool.Query(t.Context(), `
		select e.type, v.body, v.body_from, v.started_at, v.search = search_vector('', search_text(v.body))
		  from search_text_versions v join events e on e.id = v.event_id
		 where v.kind = $1 and v.id = $2
		 order by v.event_seq`, kind, id)
	if err != nil {
		t.Fatalf("read the keyword versions of %s %s: %v", kind, id, err)
	}
	defer rows.Close()
	var versions []postKeywordVersion
	for rows.Next() {
		var v postKeywordVersion
		if err := rows.Scan(&v.eventType, &v.body, &v.bodyFrom, &v.startedAt, &v.vectorOfBody); err != nil {
			t.Fatalf("read a keyword version of %s %s: %v", kind, id, err)
		}
		versions = append(versions, v)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the keyword versions of %s %s: %v", kind, id, err)
	}
	return versions
}

// versionAt is the version of item (kind, id) that the relation gives at at, its body and its
// deleted_at, and false where it gives none.
func versionAt(t *testing.T, database *store.Store, kind, id string, at time.Time) (string, *time.Time, bool) {
	t.Helper()
	var body string
	var deletedAt *time.Time
	err := database.Pool.QueryRow(t.Context(), `select v.body, v.deleted_at from `+store.TextVersions{Cut: at}.SQL()+` v
		where v.kind = $1 and v.id = $2`, kind, id).Scan(&body, &deletedAt)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return "", nil, false
	case err != nil:
		t.Fatalf("read the version of %s %s at %s: %v", kind, id, at, err)
	}
	return body, deletedAt, true
}

// requireVersions holds the keyword versions of item (kind, id) to the event types and bodies of
// want, in order, each with the vector its body makes.
func requireVersions(t *testing.T, got []postKeywordVersion, kind, id string, want ...[2]string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s %s has %d keyword versions, want %d", kind, id, len(got), len(want))
	}
	for index, v := range got {
		if v.eventType != want[index][0] || v.body != want[index][1] || !v.vectorOfBody {
			t.Errorf("%s %s's version %d is %s %s (its vector the body's: %t), want %s %s", kind, id, index,
				v.eventType, abbreviateAnswer([]byte(v.body)), v.vectorOfBody, want[index][0], abbreviateAnswer([]byte(want[index][1])))
		}
	}
}

// TestAnEditedIssueMessageStartsAKeywordVersion: an edit of an issue message starts a version of
// the message's text, and the relation gives the text before the edit at a time before it and the
// edited text from the edit on.
func TestAnEditedIssueMessageStartsAKeywordVersion(t *testing.T) {
	w := newSearchAtCutWorld(t, "KVME", "Count the waders", "The count's notes.")
	message := w.message(t, "The tern count is due.")
	w.edit(t, "/api/v1/messages/"+message, "The plover count is due.")
	w.settle(t)
	fillTextVersions(t, w.database)

	versions := keywordVersions(t, w.database, "message", message)
	requireVersions(t, versions, "message", message,
		[2]string{"message.created", "The tern count is due."}, [2]string{"message.edited", "The plover count is due."})
	if versions[1].bodyFrom != "event" {
		t.Errorf("the edit's version holds its body from %s, want the event", versions[1].bodyFrom)
	}
	if body, _, _ := versionAt(t, w.database, "message", message, versions[1].startedAt.Add(-time.Microsecond)); body != "The tern count is due." {
		t.Errorf("the relation gives the message %q just before the edit, want the text before it", body)
	}
	if body, _, _ := versionAt(t, w.database, "message", message, versions[1].startedAt); body != "The plover count is due." {
		t.Errorf("the relation gives the message %q at the edit, want the edited text", body)
	}
}

// TestALongEditedIssueMessageKeepsItsWholeEarlierBody: an edit of an issue message to a body
// longer than its event carries, edited again later, keeps its whole body in its version: the
// row shows the later edit, so the body comes from the message's body history, the body the
// later edit replaced (body_from history).
func TestALongEditedIssueMessageKeepsItsWholeEarlierBody(t *testing.T) {
	w := newSearchAtCutWorld(t, "KVLM", "Count the waders", "The count's notes.")
	message := w.message(t, "The tern count is due.")
	paragraph := strings.Repeat("shoreline ", 2000)
	long := "The plover count is due.\n\n" + paragraph + "\n\n" + paragraph + "\n\nThe avocet count closes it."
	if _, truncated := model.EventBody(long); !truncated {
		t.Fatalf("the long edit rides whole in its event; the test needs one its event cuts")
	}
	w.edit(t, "/api/v1/messages/"+message, long)
	w.edit(t, "/api/v1/messages/"+message, "The curlew count is due.")
	w.settle(t)
	fillTextVersions(t, w.database)

	versions := keywordVersions(t, w.database, "message", message)
	requireVersions(t, versions, "message", message, [2]string{"message.created", "The tern count is due."},
		[2]string{"message.edited", long}, [2]string{"message.edited", "The curlew count is due."})
	if versions[1].bodyFrom != "history" {
		t.Errorf("the long edit's version holds its body from %s, want the message's body history", versions[1].bodyFrom)
	}
}

// TestADeletedPostHasNoKeywordTextFromItsDelete: the delete of a comment and of an issue message
// starts a version with empty text and the vector of empty text, from the event; the versions
// before it keep their text. At and after the delete the relation gives the post no text, and at
// every time it gives the post's deleted_at.
func TestADeletedPostHasNoKeywordTextFromItsDelete(t *testing.T) {
	w := newSearchAtCutWorld(t, "KVDL", "Count the shorebirds", "The count's notes.")
	posts := map[string]string{
		"comment": w.comment(t, "A note on the zebrafinch survey."),
		"message": w.message(t, "The kingfisher count is due."),
	}
	bodies := map[string]string{"comment": "A note on the zebrafinch survey.", "message": "The kingfisher count is due."}
	asOfWrite(t, w.handler, http.MethodDelete, "/api/v1/comments/"+posts["comment"], nil, "alice")
	asOfWrite(t, w.handler, http.MethodDelete, "/api/v1/messages/"+posts["message"], nil, "alice")
	w.settle(t)
	fillTextVersions(t, w.database)

	for kind, id := range posts {
		versions := keywordVersions(t, w.database, kind, id)
		requireVersions(t, versions, kind, id, [2]string{kind + ".created", bodies[kind]}, [2]string{kind + ".deleted", ""})
		if versions[1].bodyFrom != "event" {
			t.Errorf("the %s's delete holds its body from %s, want the event", kind, versions[1].bodyFrom)
		}
		deletedAt := presentDeletedAt(t, w.database, kind, id)
		for _, at := range []time.Time{versions[0].startedAt, versions[1].startedAt, versions[1].startedAt.Add(time.Hour)} {
			body, given, found := versionAt(t, w.database, kind, id, at)
			wantBody := bodies[kind]
			if !at.Before(versions[1].startedAt) {
				wantBody = ""
			}
			if !found || body != wantBody || given == nil || !given.Equal(deletedAt) {
				t.Errorf("at %s the relation gives the %s %q deleted at %v (found %t), want %q deleted at %s",
					at.Format(time.RFC3339Nano), kind, body, given, found, wantBody, deletedAt.Format(time.RFC3339Nano))
			}
		}
	}
}

// TestADeletedPostCarriesItsDeletedAtBeforeTheFillReachesTheDelete: read through the relation
// right after a comment's and an issue message's deletes commit, before the fill reads them, each
// version of each post carries the post's deleted_at.
func TestADeletedPostCarriesItsDeletedAtBeforeTheFillReachesTheDelete(t *testing.T) {
	w := newSearchAtCutWorld(t, "KVDB", "Count the shorebirds", "The count's notes.")
	posts := map[string]string{
		"comment": w.comment(t, "A note on the zebrafinch survey."),
		"message": w.message(t, "The kingfisher count is due."),
	}
	w.edit(t, "/api/v1/comments/"+posts["comment"], "A note on the zebrafinch survey, corrected.")
	w.edit(t, "/api/v1/messages/"+posts["message"], "The kingfisher count is due on Friday.")
	w.settle(t)
	fillTextVersions(t, w.database)
	asOfWrite(t, w.handler, http.MethodDelete, "/api/v1/comments/"+posts["comment"], nil, "alice")
	asOfWrite(t, w.handler, http.MethodDelete, "/api/v1/messages/"+posts["message"], nil, "alice")

	for kind, id := range posts {
		versions := keywordVersions(t, w.database, kind, id)
		if len(versions) != 2 {
			t.Fatalf("the %s has %d keyword versions before the fill reads its delete, want its create and its edit", kind, len(versions))
		}
		deletedAt := presentDeletedAt(t, w.database, kind, id)
		for _, v := range versions {
			body, given, found := versionAt(t, w.database, kind, id, v.startedAt)
			if !found || body != v.body || given == nil || !given.Equal(deletedAt) {
				t.Errorf("at its %s the relation gives the %s %q deleted at %v (found %t), want its version %q deleted at %s",
					v.eventType, kind, body, given, found, v.body, deletedAt.Format(time.RFC3339Nano))
			}
		}
	}
}

// presentDeletedAt is the deleted_at the row of post id of kind holds, which must be set.
func presentDeletedAt(t *testing.T, database *store.Store, kind, id string) time.Time {
	t.Helper()
	var deletedAt *time.Time
	if err := database.Pool.QueryRow(t.Context(), `select deleted_at from `+kind+`s where id = $1::uuid`, id).Scan(&deletedAt); err != nil || deletedAt == nil {
		t.Fatalf("read the %s's deleted_at: %v, %v", kind, deletedAt, err)
	}
	return *deletedAt
}
