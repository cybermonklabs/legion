package cipher

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/sjawhar/envoy/internal/cipher/wire"
)

// personSubjectsRevokePath is the broker's route that ends a replaced Google account's credentials
// under an address.
const personSubjectsRevokePath = "/v1/person-subjects/revoke"

// RevokePersonSubject tells the broker that the account revocation.Subject, of the identity
// provider revocation.Issuer, no longer holds the address revocation.Person, which the account
// revocation.HolderSubject holds now (POST /v1/person-subjects/revoke). The broker ends, in one
// transaction, every sign-in on a machine and every machine login of the address's own machines
// that the replaced account made, or that records no account, whenever it issued them, with every
// token minted under them and every session they enrolled; it refuses the replaced account any
// new one, and leaves every credential of the holder's and every service's login. A repeat
// answers as the first did with zero counts, so a caller that lost the answer asks again.
//
// It answers the broker's counts once the broker answers 200 with its state "revoked", or with
// "superseded" and the generation that superseded it (SupersededBy): the broker had applied a later
// replacement at the address, so this revoke changed nothing there, which the caller reports and
// does not settle (api.revokeOwedBrokerCredentials). It answers an error for anything else: an
// *Error for any other status, a 404 included (the broker answers the route 200 or 400, so a 404
// is a wrong DISPATCH_AGENT_SECRETS_URL, a proxy in the way or a broker without the route, which
// ended nothing), an *AccountTokenError for Dispatch's own token, and a plain error for a transport
// failure or an answer that is not the broker's.
func (c *Client) RevokePersonSubject(ctx context.Context, revocation wire.RevokePersonSubjectRequest) (wire.PersonSubjectRevokedResponse, error) {
	var revoked wire.PersonSubjectRevokedResponse
	body, err := json.Marshal(revocation)
	if err != nil {
		return revoked, fmt.Errorf("agent-secrets: encode the revocation of a replaced account of %s: %w", revocation.Person, err)
	}
	answer, err := c.do(ctx, http.MethodPost, personSubjectsRevokePath, body)
	if err != nil {
		return revoked, fmt.Errorf("agent-secrets: revoke a replaced account of %s: %w", revocation.Person, err)
	}
	if err := json.Unmarshal(answer, &revoked); err != nil {
		return revoked, fmt.Errorf("agent-secrets: read the revocation of a replaced account of %s: %w", revocation.Person, err)
	}
	switch {
	case revoked.State == "revoked":
	case revoked.State == "superseded" && revoked.SupersededBy != nil:
	default:
		return revoked, fmt.Errorf("agent-secrets: the revocation of a replaced account of %s answered state %q, want \"revoked\", or \"superseded\" with superseded_by", revocation.Person, revoked.State)
	}
	return revoked, nil
}
