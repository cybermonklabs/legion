package api

import (
	"context"
	"net/http"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"
)

// The sessions whose comments name a short id of PRIV's: carol's, proved by a broker token, and
// one carol's personal token names in its body.
const (
	recordedAuthorSession     = "3d0c5e3a-7b4f-4c9d-9e5a-3f4b5c6d7e91"
	recordedAuthorBodySession = "3d0c5e3a-7b4f-4c9d-9e5a-3f4b5c6d7e92"
)

// TestAnAgentsShortIDResolvesAmongWhatItsPersonSees: a short id a stored text writes resolves among
// the nodes its author's person sees (access.RecordedViewer), at the write and at every rebuild
// alike, and the row stores no credential a rebuild could read again. So a session author, whose
// owner is the person it wrote for, reads as that person, not as an agent credential naming no
// list, which sees no private project. carol sees PRIV, and comments on
// PRIV's issue; then an agent of hers comments there too, naming that comment by the first eight
// characters of its id. Its stored reference is the comment's whole id, for both of her agents:
//
//   - a session of carol's, proved by a broker token naming PRIV;
//   - a session carol's personal token that lists PRIV names in its body, whose stored author
//     records carol as its owner.
//
// A reader still sees the reference only through its own visibility.
//
// Control: RecordedViewer reading such an author as an agent credential naming no list; the short
// id names nothing, and the comment stores it as it wrote it.
func TestAnAgentsShortIDResolvesAmongWhatItsPersonSees(t *testing.T) {
	t.Parallel()
	seed := seedPrivateProject(t)
	issue := seed.issue[seedPrivate]
	target := createdID(t, seed.mustRequest(t, callerCarol, http.MethodPost, "/api/v1/issues/"+issue+"/comments",
		map[string]any{"body": "The comment the short id names."}, http.StatusCreated))
	text := "See dispatch://" + issue + "/comment/" + target[:8] + "."
	body := map[string]any{"body": text}
	for _, row := range []struct {
		name  string
		write func(t *testing.T) string
	}{
		{"a session of carol's", func(t *testing.T) string {
			return createdID(t, seed.asSessionOf(t, seedCarol, recordedAuthorSession, seedPrivate, http.MethodPost,
				"/api/v1/issues/"+issue+"/comments", body, http.StatusCreated))
		}},
		{"a session carol's token that lists PRIV names", func(t *testing.T) string {
			return createdID(t, seed.mustRequest(t, callerCarolPrivToken, http.MethodPost, "/api/v1/issues/"+issue+"/comments",
				map[string]any{"body": text, "actor": map[string]string{"kind": "session", "id": recordedAuthorBodySession}}, http.StatusCreated))
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			comment := row.write(t)
			rows, err := seed.database.Pool.Query(context.Background(), `
				select to_id from refs where from_kind = 'comment' and from_id = $1 and to_kind = 'comment' order by to_id
			`, comment)
			if err != nil {
				t.Fatalf("read the references of %s's comment: %v", row.name, err)
			}
			cited, err := pgx.CollectRows(rows, pgx.RowTo[string])
			if err != nil {
				t.Fatalf("read the references of %s's comment: %v", row.name, err)
			}
			if !slices.Equal(cited, []string{target}) {
				t.Errorf("%s's comment naming %s stores the comment references %v, want PRIV's comment %s whole: its person sees PRIV",
					row.name, target[:8], cited, target)
			}
		})
	}
}
