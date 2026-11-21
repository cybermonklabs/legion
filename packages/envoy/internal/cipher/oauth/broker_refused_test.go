package oauth_test

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/oauth"
	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess/projectaccesstest"
)

// TestARefusalWithABodyItCannotReadStillRefusesTheTrade: Dispatch answering 401 or 403 to the
// broker's own account token refuses the trade whatever the refusal's body: one past what the
// client reads of an answer, and one that ends before the length its header declares, are each
// the refusal, never a failure to ask that an operator entry's token or a person's trade naming no
// project would mint through unchecked. It runs the real Dispatch client, not a stand-in for it.
func TestARefusalWithABodyItCannotReadStillRefusesTheTrade(t *testing.T) {
	const says = "Dispatch refused the broker's own account token (BROKER_PROJECT_ACCESS_TOKEN_FILE)"
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		for _, body := range projectaccesstest.RefusalBodies {
			fixture := func(t *testing.T) *exchangeFixture {
				t.Helper()
				dispatch := projectaccess.New(projectaccesstest.NewRefusing(t, status, body), projectaccesstest.StaticAccount("token"), nil)
				return newExchangeFixture(t, func(c *oauth.Config) { c.Exchange.Projects = dispatch })
			}
			t.Run(fmt.Sprintf("%d with a body %s refuses an entry's token", status, body), func(t *testing.T) {
				f := fixture(t)
				f.registers(t, account, "pod-uid-1", "implementer-g1")
				got, answer := f.exchange(t, exchangeForm(f.podToken(t, "pod-uid-1", nil), oauthwire.SubjectTokenTypeKubernetes, "implementer-g1"), newKey(t))
				refusedSaying(t, got, answer, http.StatusInternalServerError, "server_error", says)
			})
			t.Run(fmt.Sprintf("%d with a body %s refuses a person's trade naming no project", status, body), func(t *testing.T) {
				f := fixture(t)
				key := newKey(t)
				got, answer := f.exchange(t, exchangeForm(f.devBoxToken(t, key, person), oauthwire.SubjectTokenTypeAccessToken, "ada-devbox"), key)
				refusedSaying(t, got, answer, http.StatusInternalServerError, "server_error", says)
			})
		}
	}
}
