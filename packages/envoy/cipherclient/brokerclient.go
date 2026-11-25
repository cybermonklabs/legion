// Package cipherclient is a public re-export of internal/cipherclient's token client, existing
// solely so a module outside github.com/sjawhar/envoy — packages/daemon's `legion controller
// start`, which mints its own Dispatch token, and its daemon's own Dispatch client — can use it:
// Go's internal-package rule refuses to let any package whose own import path does not start with
// github.com/sjawhar/envoy import a package under github.com/sjawhar/envoy/internal/..., however
// workspace-linked the two modules are. packages/envoy/ciphertest is the same facade for the
// broker's test rig, and says the same.
//
// This changes no behaviour — every type here is an alias for the internal package's own, and
// every function forwards to it — so internal/cipherclient remains the one place the client's
// behaviour is described and changed, and the repository still holds one implementation of the
// RFC 8693 trade, one renewal loop and one DPoP signer.
package cipherclient

import (
	"crypto/ecdsa"
	"net/http"

	internal "github.com/sjawhar/envoy/internal/cipherclient"
)

// The client, its configuration, what a trade answers, and an identity a machine can prove.
type (
	Client       = internal.Client
	Config       = internal.Config
	Token        = internal.Token
	Source       = internal.Source
	RefusedError = internal.RefusedError
)

// The variables a program reads to build this machine's identity; internal/cipherclient's
// environment.go documents each.
const (
	BrokerURLVar        = internal.BrokerURLVar
	IdentityVar         = internal.IdentityVar
	MachineTokenFileVar = internal.MachineTokenFileVar
	MachineAudienceVar  = internal.MachineAudienceVar
	PoolAudienceVar     = internal.PoolAudienceVar
	PoolAccountVar      = internal.PoolAccountVar
	SessionVar          = internal.SessionVar
	KeyDirVar           = internal.KeyDirVar
	RoleKeyFile         = internal.RoleKeyFile
	IdentityKubernetes  = internal.IdentityKubernetes
	IdentityMetadata    = internal.IdentityMetadata
	IdentityPool        = internal.IdentityPool
)

// New forwards to internal/cipherclient.New.
func New(cfg Config) (*Client, error) { return internal.New(cfg) }

// MachineIdentity forwards to internal/cipherclient.MachineIdentity: the identity IdentityVar
// names, or nil where this machine proves none.
func MachineIdentity(getenv func(string) string) (Source, error) {
	return internal.MachineIdentity(getenv)
}

// MachineKey forwards to internal/cipherclient.MachineKey: the key a machine trade proves, which
// is a pod role's own agent-secrets key where IdentityVar is kubernetes and AGENT_SECRETS_KEY_DIR
// names that role's directory, and a fresh key of this process's own everywhere else.
func MachineKey(getenv func(string) string) (*ecdsa.PrivateKey, error) {
	return internal.MachineKey(getenv)
}

// TradeSession forwards to internal/cipherclient.TradeSession: the session a trade writes
// under, the caller's own where it acts as one and SessionVar's otherwise.
func TradeSession(getenv func(string) string, own string) string {
	return internal.TradeSession(getenv, own)
}

// HTTPClient forwards to internal/cipherclient.HTTPClient.
func HTTPClient() *http.Client { return internal.HTTPClient() }

// NewDPoPTransport forwards to internal/cipherclient.NewDPoPTransport: a RoundTripper that signs
// a DPoP proof of key for each request it sends through base, carrying ath beside an
// `Authorization: DPoP` token, which is how a caller presents a Client's token to Dispatch or the
// listener.
func NewDPoPTransport(key *ecdsa.PrivateKey, base http.RoundTripper) http.RoundTripper {
	return internal.NewDPoPTransport(key, base)
}

// NewTokenTransport forwards to internal/cipherclient.NewTokenTransport: a RoundTripper that
// presents client's token as `Authorization: DPoP <token>` beside a proof of its key on each
// request it sends through base, sending nothing when the trade fails, and has client drop a
// token the server answers 401 BROKER_TOKEN_NOT_ACTIVE for (Client.Drop), so the next request
// trades again.
func NewTokenTransport(client *Client, base http.RoundTripper) http.RoundTripper {
	return internal.NewTokenTransport(client, base)
}
