package intake

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
)

// TestAReviewFieldTheIntakeCannotReadPrintsNoneOfIt: a submitted review whose review_id (review-id)
// or submitted_at (submitted-at) holds a token instead of what GitHub writes there is named by its
// member, and what the intake says of it holds no run of the token. It printed the value as GitHub's
// event carried it. The twin shows each field holds the token and is still named, so the row can
// see one printed. Captures intake.review-field/<case>.
func TestAReviewFieldTheIntakeCannotReadPrintsNoneOfIt(t *testing.T) {
	carried, err := json.Marshal("https://example.invalid/?access" + "_token=" + oauth2calltest.Token)
	if err != nil {
		t.Fatal(err)
	}
	review := func(field string) map[string]json.RawMessage {
		raw := map[string]json.RawMessage{
			"number": json.RawMessage(`7`), "action": json.RawMessage(`"submitted"`),
			"state": json.RawMessage(`"approved"`),
		}
		raw[field] = carried
		return raw
	}
	said := func(field string) (string, bool) {
		_, unread, err := decodeReview(ghrepo.MustParse("acme/widgets"), review(field))
		if err != nil {
			return err.Error(), true
		}
		return strings.Join(unread, "; "), len(unread) > 0
	}
	cases := []string{"review_id", "submitted_at"}
	t.Run("twin", func(t *testing.T) {
		if !strings.Contains(string(carried), oauth2calltest.Token) {
			t.Errorf("the field %s holds no token, so the row could see none printed", carried)
		}
		for _, field := range cases {
			if text, named := said(field); !named || !strings.Contains(text, field) {
				t.Errorf("%s: the intake says %q; want it to name %s", field, text, field)
			}
		}
	})
	for _, field := range cases {
		t.Run(strings.ReplaceAll(field, "_", "-"), func(t *testing.T) {
			text, named := said(field)
			if !named || !strings.Contains(text, field) {
				t.Fatalf("the intake says %q; want it to name %s", text, field)
			}
			oauth2calltest.RequireNoPiece(t, "intake.review-field/"+strings.ReplaceAll(field, "_", "-"), text, oauth2calltest.Token)
		})
	}
}
