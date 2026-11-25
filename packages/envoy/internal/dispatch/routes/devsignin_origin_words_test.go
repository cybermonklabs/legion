package routes

import (
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestADevSignInOriginTheFenceRefusesPrintsNoneOfIt: a dashboard origin off this machine carrying
// redacttest's credentials in its user information and its query (remote) is refused for the dev
// sign-in naming the settings, and the refusal holds no run of a credential. The refusal quoted the
// origin as written. The twin shows the origin holds every credential and is still refused as no
// loopback origin, so the row can see one printed. Captures routes.devsignin-origin/remote.
func TestADevSignInOriginTheFenceRefusesPrintsNoneOfIt(t *testing.T) {
	origin := "https://" + redacttest.User + ":" + redacttest.Password + "@dashboard.example/?access" + "_token=" + redacttest.Token
	t.Run("twin", func(t *testing.T) {
		if held := redacttest.Held(origin); len(held) != len(redacttest.Secrets) {
			t.Errorf("the origin %q holds %v; want every credential, so the row can see one printed", origin, held)
		}
		if _, err := DevSignInOrigin(origin); err == nil || !strings.Contains(err.Error(), "is for a loopback origin only") {
			t.Errorf("DevSignInOrigin = %v; want it refused as no loopback origin", err)
		}
	})
	t.Run("remote", func(t *testing.T) {
		_, err := DevSignInOrigin(origin)
		if err == nil {
			t.Fatalf("DevSignInOrigin took %q; want it refused", origin)
		}
		if !strings.Contains(err.Error(), "DISPATCH_SERVER_URL") {
			t.Errorf("DevSignInOrigin = %v; want a refusal naming DISPATCH_SERVER_URL", err)
		}
		redacttest.RequireNoPiece(t, "routes.devsignin-origin/remote", err.Error(), redacttest.User, redacttest.Password, redacttest.Token)
	})
}
