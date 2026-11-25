// packages/envoy/internal/cipher/identity/google/source.go
//
// The client half of this provider: what a machine of this kind proves itself with. A Compute
// Engine instance reads its own identity token from the metadata server; a machine with no Google
// service account of its own federates a credential it already holds (pool.go); a workload
// holding an external_account credential file is found by the detector (detect.go). The server
// half verifies such a token (verify.go).

package google

import (
	"context"
	"net/http"
	"os"

	"github.com/sjawhar/envoy/internal/cipher/identity"
	"github.com/sjawhar/envoy/internal/googleidentity"
	"github.com/sjawhar/envoy/internal/redact"
)

// The variables this provider's own sources read beside identity.MachineTokenFileVar and
// identity.MachineAudienceVar.
const (
	// PoolAudienceVar is the workload identity pool provider an external credential is traded
	// at, the full //iam.googleapis.com/projects/… resource name.
	PoolAudienceVar = "AGENT_SECRETS_POOL_AUDIENCE"
	// PoolAccountVar is the managed identity generateIdToken is called on.
	PoolAccountVar = "AGENT_SECRETS_POOL_ACCOUNT"
)

// MetadataIdentity is the identity of a machine on Google Cloud: the Google identity token its
// Compute Engine metadata server mints for Audience, asked for with format=full and fetched
// through no proxy (internal/googleidentity, the one reader of that server in this repository).
type MetadataIdentity struct {
	// Audience is the token's own aud, the client id the broker checks against
	// BROKER_GOOGLE_OIDC_AUDIENCE.
	Audience string
	// FromBroker is set where the audience is the Google exchange audience the broker answered
	// (DetectMachineIdentity), not one identity.MachineAudienceVar names.
	FromBroker bool
}

// NewMetadataIdentity is the identity of a machine whose metadata server mints its token for
// audience.
func NewMetadataIdentity(audience string) *MetadataIdentity {
	return &MetadataIdentity{Audience: audience}
}

func (m *MetadataIdentity) Token(ctx context.Context) (string, string, error) {
	token, err := googleidentity.Token(ctx, m.Audience)
	if err != nil {
		return "", "", err
	}
	return token, SubjectTokenType, nil
}

// Describe names the audience: the broker's (FromBroker) as such, or by
// identity.MachineAudienceVar where that setting chose it (identity.AudienceText); as written
// where it is a plain http(s) URL (internal/redact's allow-list), since the audience is a value a
// person can paste a credential into, and this text is in every trade's log line and error. It
// names the metadata server by googleidentity.MetadataHostVar only while that variable is set: a
// Compute Engine VM's server is at its own address.
func (m *MetadataIdentity) Describe() string {
	audience := identity.AudienceText(m.Audience)
	if m.FromBroker {
		audience = "the Google exchange audience the broker names"
		if redact.Clean(m.Audience) {
			audience = m.Audience
		}
	}
	described := "this machine's Google identity token for " + audience
	if os.Getenv(googleidentity.MetadataHostVar) != "" {
		described += " (" + googleidentity.MetadataHostVar + " names the metadata server)"
	}
	return described
}

// tokenFile is the credential a workload identity pool federates: the projected token file
// identity.MachineTokenFileVar names, answered under the JWT token type Google's security token
// service reads an OIDC provider's token under, which is what the pool provider verifies it as.
type tokenFile struct {
	Path string
}

func (t *tokenFile) Token(context.Context) (string, string, error) {
	token, err := identity.ReadTokenFile(t.Path, t.Describe())
	if err != nil {
		return "", "", err
	}
	return token, "urn:ietf:params:oauth:token-type:jwt", nil
}

func (t *tokenFile) Describe() string {
	return "the projected service-account token " + identity.MachineTokenFileVar + " names"
}

// Sources are the identities a machine of this kind holds, in the order a client lists them: the
// instance's own metadata token, and a pool mint for a machine with no Google service account of
// its own. The broker mints a launcher credential for either, so a machine naming one registers
// its own sessions.
func Sources() []identity.SourceEntry {
	return []identity.SourceEntry{
		{
			Name:      "metadata",
			Provider:  Kind,
			Launcher:  true,
			Variables: []identity.Variable{{Name: identity.MachineAudienceVar}},
			FromEnv: func(need func(string) (string, error), _ *http.Client) (identity.Source, error) {
				audience, err := need(identity.MachineAudienceVar)
				if err != nil {
					return nil, err
				}
				return NewMetadataIdentity(audience), nil
			},
		},
		{
			Name:     "pool",
			Provider: Kind,
			Launcher: true,
			Variables: []identity.Variable{
				{Name: identity.MachineTokenFileVar, Path: true},
				{Name: identity.MachineAudienceVar},
				{Name: PoolAudienceVar},
				{Name: PoolAccountVar},
			},
			FromEnv: func(need func(string) (string, error), client *http.Client) (identity.Source, error) {
				path, err := need(identity.MachineTokenFileVar)
				if err != nil {
					return nil, err
				}
				audience, err := need(identity.MachineAudienceVar)
				if err != nil {
					return nil, err
				}
				pool, err := need(PoolAudienceVar)
				if err != nil {
					return nil, err
				}
				account, err := need(PoolAccountVar)
				if err != nil {
					return nil, err
				}
				return &PoolIdentity{
					Subject: &tokenFile{Path: path}, Audience: pool, ServiceAccount: account,
					TokenAudience: audience, HTTP: client,
				}, nil
			},
		},
	}
}

// ActorTokenType is the actor token type a trade carries beside a subject of subjectType: a
// person's sign-in on a machine presents that machine's own Google identity token as the actor,
// and no other subject type takes one.
func ActorTokenType(subjectType string) (string, bool) {
	if subjectType == SubjectTokenType {
		return ActorTokenTypeMachine, true
	}
	return "", false
}
