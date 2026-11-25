// packages/envoy/internal/cipherclient/source.go

package cipherclient

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
	"github.com/sjawhar/envoy/internal/googleidentity"
	"github.com/sjawhar/envoy/internal/redact"
)

// Source is one platform identity this process can prove, read fresh each time it is asked: the
// subject token of the broker's RFC 8693 exchange, and the type the exchange reads it under. The
// grant issues no refresh token, so every renewal asks its Source again — a projected token the
// kubelet rewrote in place, a metadata token minted anew, a pool token traded again, a GitHub
// Actions run's token asked of GitHub again — and a Source that caches must cache for less than
// the token it answers lives.
//
// Each implementation is one identity this process can prove. Each is a Source so one client
// serves them all and a caller names which it has in one line.
type Source interface {
	// Token is a subject token to present and the subject_token_type it is presented under, or
	// an error that never reached the broker: an identity this machine could not read is never
	// an empty token for the broker to refuse.
	Token(ctx context.Context) (token string, tokenType string, err error)
	// Describe names the identity in a refusal, without naming the token: the file read, the
	// metadata server asked, or the pool traded at.
	Describe() string
}

// ProjectedToken is the identity of a pod: the projected Kubernetes service-account token its
// runtime mounts, minted for the broker's exchange audience. The kubelet rewrites that file in
// place well before the token expires, so it is read at call time and never once at start.
//
// The file is the one MachineTokenFileVar names, and nothing this type prints names its path: a
// person who pasted the token itself into that variable would otherwise see it in every refusal
// and in every log line that names the identity (Describe). A refusal names the variable, and a
// read error the system's reason alone (redact.File).
type ProjectedToken struct {
	// Path is the file the token is read from.
	Path string
}

// NewProjectedToken is the identity of a pod holding its projected service-account token at path,
// the file MachineTokenFileVar names.
func NewProjectedToken(path string) *ProjectedToken { return &ProjectedToken{Path: path} }

func (p *ProjectedToken) Token(context.Context) (string, string, error) {
	data, err := os.ReadFile(p.Path)
	if err != nil {
		return "", "", fmt.Errorf("read %s: %w", p.Describe(), redact.File(err))
	}
	token := strings.TrimSpace(string(data))
	if token == "" {
		return "", "", fmt.Errorf("%s is empty", p.Describe())
	}
	return token, oauthwire.SubjectTokenTypeKubernetes, nil
}

func (p *ProjectedToken) Describe() string {
	return "the projected service-account token " + MachineTokenFileVar + " names"
}

// MetadataIdentity is the identity of a machine on Google Cloud: the Google identity token its
// Compute Engine metadata server mints for Audience, asked for with format=full and fetched
// through no proxy (internal/googleidentity, the one reader of that server in this repository).
type MetadataIdentity struct {
	// Audience is what the minted token's `aud` carries, which the broker checks against
	// BROKER_GOOGLE_OIDC_AUDIENCE.
	Audience string
	// FromBroker is set where the audience is the Google exchange audience the broker answered
	// (DetectMachineIdentity), not one MachineAudienceVar names.
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
	return token, oauthwire.SubjectTokenTypeGoogle, nil
}

// Describe names the audience: the broker's (FromBroker) as such, or by MachineAudienceVar where
// that setting chose it (audienceText); as written where it is a plain http(s) URL
// (internal/redact's allow-list), since the audience is a value a person can paste a credential
// into, and this text is in every trade's log line and error. It names the metadata server by
// googleidentity.MetadataHostVar only while that variable is set: a Compute Engine VM's server is
// at its own address.
func (m *MetadataIdentity) Describe() string {
	audience := audienceText(m.Audience)
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

// audienceText names the audience MachineAudienceVar sets: as written when it is a plain http(s)
// URL (internal/redact's allow-list), and by the variable alone otherwise, since the audience is a
// configured value a person can paste a credential into, and an identity's Describe is in every
// trade's log line and error.
func audienceText(audience string) string {
	if redact.Clean(audience) {
		return audience + " (" + MachineAudienceVar + ")"
	}
	return "the audience " + MachineAudienceVar + " names"
}
