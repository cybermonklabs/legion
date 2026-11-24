package approval

import (
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/requests"
	"github.com/sjawhar/envoy/internal/cipher/store"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestAPublicURLThePageRefusesPrintsNoCredentialOfIt: a BROKER_PUBLIC_URL of another scheme, with
// redacttest's credentials in its user information and its query, is refused naming the URL as the
// rule reads it (refused), holding no run of the user name, the password or the token; the twin
// shows the URL as written holds all three, so the row can see one printed. Captures
// approval.public-url/refused.
func TestAPublicURLThePageRefusesPrintsNoCredentialOfIt(t *testing.T) {
	t.Parallel()
	public := "ftp://" + redacttest.User + ":" + redacttest.Password + "@secrets.example/broker?access" + "_token=" + redacttest.Token
	t.Run("twin", func(t *testing.T) {
		if len(redacttest.Held(public)) != len(redacttest.Secrets) {
			t.Errorf("the URL %q holds %v; want every credential, so the row can see one printed", public, redacttest.Held(public))
		}
	})
	t.Run("refused", func(t *testing.T) {
		page, err := New(Config{PublicURL: public, Store: &store.Store{}, Machine: &requests.Machine{}})
		if err == nil {
			t.Fatalf("New built a page on an ftp public URL: %+v", page)
		}
		redacttest.RequireNoPiece(t, "approval.public-url/refused", err.Error())
	})
}
