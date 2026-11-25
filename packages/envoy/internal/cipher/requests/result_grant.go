package requests

import (
	"context"
	"errors"
	"fmt"

	"github.com/sjawhar/envoy/internal/cipher/oauthstore"
	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
	"github.com/sjawhar/envoy/internal/cipher/placeholder"
)

// ResultGrants reads a CI run's result grant (oauthstore's result_grants), which the token
// endpoint wrote when the run traded its token for a placeholder: by the placeholder's hash, for
// a proxy connection that presents it, and by id, for the proxy's read of its values. Each answers
// oauthstore.ErrResultGrantNotLive for a grant no row holds and for one whose end the database's
// clock has passed. *oauthstore.Store is one.
type ResultGrants interface {
	ResultGrantByPlaceholder(ctx context.Context, sha256Hex string) (oauthstore.ResultGrant, error)
	ResultGrantByID(ctx context.Context, id string) (oauthstore.ResultGrant, error)
}

// ResultTokens mints the results token of a result grant for this broker pod, bound to the pod
// key whose RFC 7638 thumbprint jkt is, and ending with the grant. *oauth.Provider is one.
type ResultTokens interface {
	MintResultToken(ctx context.Context, grant oauthstore.ResultGrant, jkt string) (string, error)
}

// resultConnection is AirlockConnection for a result grant's placeholder, which starts
// placeholder.ResultPrefix: the grant read from result_grants by the placeholder's hash and from
// nowhere else, so an ended grant's placeholder never falls through to a secret grant's
// placeholders. The connection is the grant's: its id as oauthwire.ResultGrantIDPrefix and the
// uuid, its session, the check run of its job, its end, and DISPATCH_RESULTS_TOKEN's one slot,
// whose placeholder is the one presented. It belongs to no enrollment, so the enrollment id is
// empty, and the proxy names its calls by the grant alone. A broker with no grant store answers
// every result placeholder ErrGrantNotLive, and so does one whose results entry is not the
// grant's (resultEntryRefusal).
func (m *Machine) resultConnection(ctx context.Context, presented string) (AirlockConnection, error) {
	if m.ResultGrants == nil {
		return AirlockConnection{}, ErrGrantNotLive
	}
	grant, err := m.ResultGrants.ResultGrantByPlaceholder(ctx, placeholder.Hash(presented))
	if errors.Is(err, oauthstore.ErrResultGrantNotLive) {
		return AirlockConnection{}, ErrGrantNotLive
	}
	if err != nil {
		return AirlockConnection{}, err
	}
	if err := m.resultEntryRefusal(grant); err != nil {
		return AirlockConnection{}, err
	}
	expires := grant.ExpiresAt
	return AirlockConnection{
		GrantID: oauthwire.ResultGrantIDPrefix + grant.ID, SessionID: grant.Session, CheckRunID: checkRunOf(grant), Expires: &expires,
		Names: map[string]AirlockGrantName{oauthwire.AirlockResultsName: {Slots: map[string]string{SlotValue: presented}}},
	}, nil
}

// resultValues is AirlockValues for a result grant, id its uuid: the run's results token, which
// this pod mints for the grant now (ResultTokens), bound to the pod's own key (ResultKey), and
// which no table and no other pod holds. Its Expires is zero: the token ends with the grant, and
// the grant's end is what the proxy's read of the connection checks on every request, so a value
// the proxy holds needs no clock of its own. It answers beside it the policy version the proxy
// holds values against, which a result grant does not read: a policy change makes the pod mint
// again, which costs one token and changes nothing of the grant. A grant of another results entry
// than this broker's is refused before anything is minted (resultEntryRefusal).
func (m *Machine) resultValues(ctx context.Context, id string) (map[string]AirlockSecret, string, error) {
	if m.ResultGrants == nil || m.ResultTokens == nil || m.ResultKey == "" {
		return nil, "", ErrGrantNotLive
	}
	grant, err := m.ResultGrants.ResultGrantByID(ctx, id)
	if errors.Is(err, oauthstore.ErrResultGrantNotLive) {
		return nil, "", ErrGrantNotLive
	}
	if err != nil {
		return nil, "", err
	}
	if err := m.resultEntryRefusal(grant); err != nil {
		return nil, "", err
	}
	token, err := m.ResultTokens.MintResultToken(ctx, grant, m.ResultKey)
	if err != nil {
		return nil, "", err
	}
	return map[string]AirlockSecret{oauthwire.AirlockResultsName: {Slots: map[string]string{SlotValue: token}}}, m.PolicyVersion(), nil
}

// resultEntryRefusal is ErrGrantNotLive, saying why, for a result grant made under another
// results entry than this broker's (oauthstore.ResultGrant.EntryDigest against
// ResultEntryDigest): another profile DISPATCH_RESULTS_TOKEN or another private destination,
// which would send the run's token somewhere the broker that granted it would not; and for every
// grant on a broker with no results entry. The proxy answers it 407 GRANT_NOT_LIVE, with nothing
// forwarded, as it answers an ended grant.
//
// The grant is refused, not ended: its row stays, and a pod whose entry is the grant's still
// serves it. A run's token is spent once, so ending the grant here would end the run's writes on
// the pods that agree with the grant too. During a roll that changes the profile or the
// destination, pods of the old entry and of the new refuse each other's grants, in both
// directions, until the grant ends; the command takes the 407 as final, and the run's next
// attempt is granted under the entry of the pod it asks.
func (m *Machine) resultEntryRefusal(grant oauthstore.ResultGrant) error {
	if m.ResultEntryDigest == "" || grant.EntryDigest != m.ResultEntryDigest {
		return fmt.Errorf("%w: the result grant was made under another results profile or private destination than this broker's", ErrGrantNotLive)
	}
	return nil
}

// checkRunOf is the check run of the job a result grant was made for, read from the run the grant
// carries under oauthwire.ClaimCI by the one decoder introspection reads a token's run with
// (oauthwire.CIRunOf), and "" for a grant whose run does not read as one: the connection's lines
// then name no check run, and nothing else of the grant changes.
func checkRunOf(grant oauthstore.ResultGrant) string {
	run, err := oauthwire.CIRunOf(grant.Extra[oauthwire.ClaimCI])
	if err != nil || run == nil {
		return ""
	}
	return run.CheckRunID
}
