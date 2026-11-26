package oauth

import (
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/oauthstore"
)

// TestDecideDropsTheAccountAndTimeASessionCarries: an exchange of a machine access token runs on
// the session the library reads out of that token's own row (oauthstore's Store, whose doc says so),
// so whatever claims the row carries reach decide. decide deletes a carried account
// (ClaimAccountIssuer, ClaimAccountSubject) and a carried creation time (ClaimCredentialCreatedAt)
// for every kind of subject token: a token records the account its own trade names and the
// creation time of its own credential (personSignIn, approverAccount), or none. Here the session
// carries all three before decide runs, for each kind the claims check decides, and after decide
// none of the three is left.
//
// Through a whole exchange only the account's delete shows
// (TestAnExchangedTokenRecordsOnlyTheAccountItsOwnTradeNames): approverAccount writes the login's
// own creation time over a carried one, so this is where the time's delete is held.
//
// Control: each delete removed in turn; its claim is left for every kind.
func TestDecideDropsTheAccountAndTimeASessionCarries(t *testing.T) {
	provider, subjects := claimsDecider(t)
	for _, subject := range subjects {
		session := oauthstore.NewExchangeSession()
		session.Extra = map[string]any{
			ClaimAccountIssuer:       "https://carried.example",
			ClaimAccountSubject:      "109carried",
			ClaimCredentialCreatedAt: float64(1794900000),
		}
		decideOn(t, provider, subject, session)
		for _, claim := range []string{ClaimAccountIssuer, ClaimAccountSubject, ClaimCredentialCreatedAt} {
			if value, carried := session.Extra[claim]; carried {
				t.Errorf("decide on a %s subject token left %s = %v, which the session carried in; want it dropped", subject.tokenType, claim, value)
			}
		}
	}
}
