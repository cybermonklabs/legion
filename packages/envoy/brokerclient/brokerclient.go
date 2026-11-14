// Package brokerclient is a public re-export of internal/brokerclient's token client, and of the
// agent-secrets rules for the helper's socket and a key directory's machine session, existing
// solely so a module outside github.com/sjawhar/envoy — packages/daemon's `legion controller
// start`, which mints its own Dispatch token and keeps the controller's own key directory — can
// use them: Go's internal-package rule refuses to let any package whose own import path does not
// start with github.com/sjawhar/envoy import a package under github.com/sjawhar/envoy/internal/...,
// however workspace-linked the two modules are. packages/envoy/brokertest is the same facade for
// the broker's test rig, and says the same.
//
// This changes no behaviour — every type here is an alias for the internal package's own, and
// every function forwards to it — so internal/brokerclient and internal/broker/helper remain the
// one place the client's behaviour is described and changed, and the repository still holds one
// implementation of the RFC 8693 trade, one renewal loop, one DPoP signer and one rule for when
// the helper signs.
package brokerclient

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/broker/helper"
	internal "github.com/sjawhar/envoy/internal/brokerclient"
)

// The client, its configuration, what a trade answers, and an identity a machine can prove.
type (
	Client       = internal.Client
	Config       = internal.Config
	Token        = internal.Token
	Source       = internal.Source
	RefusedError = internal.RefusedError
)

// The variables a program reads to build this machine's identity, and the one that names where
// its sign-ins are kept; internal/brokerclient's environment.go and signinauthority.go document
// each.
const (
	BrokerURLVar        = internal.BrokerURLVar
	IdentityVar         = internal.IdentityVar
	MachineTokenFileVar = internal.MachineTokenFileVar
	MachineAudienceVar  = internal.MachineAudienceVar
	PoolAudienceVar     = internal.PoolAudienceVar
	PoolAccountVar      = internal.PoolAccountVar
	SessionVar          = internal.SessionVar
	SignInDirVar        = internal.SignInDirVar
	IdentityKubernetes  = internal.IdentityKubernetes
	IdentityMetadata    = internal.IdentityMetadata
	IdentityPool        = internal.IdentityPool
)

// HelperSocketVar is internal/broker/helper.SocketVar: the variable that names the
// agent-secrets helper's socket where a machine sets one.
const HelperSocketVar = helper.SocketVar

// New forwards to internal/brokerclient.New.
func New(cfg Config) (*Client, error) { return internal.New(cfg) }

// MachineIdentity forwards to internal/brokerclient.MachineIdentity: the identity IdentityVar
// names, or nil where this machine proves none.
func MachineIdentity(getenv func(string) string) (Source, error) {
	return internal.MachineIdentity(getenv)
}

// TradeSession forwards to internal/brokerclient.TradeSession: the session a trade writes
// under, the caller's own where it acts as one and SessionVar's otherwise.
func TradeSession(getenv func(string) string, own string) string {
	return internal.TradeSession(getenv, own)
}

// HTTPClient forwards to internal/brokerclient.HTTPClient.
func HTTPClient() *http.Client { return internal.HTTPClient() }

// HelperSocket forwards to internal/broker/helper.Socket: the helper's socket under getenv, and
// whether a helper socket is set or present there, which is when agent-secrets takes the helper
// to sign ahead of any machine session of its own.
func HelperSocket(getenv func(string) string) (string, bool) { return helper.Socket(getenv) }

// DropMachineSession forwards to internal/brokerclient.DropMachineSession: it removes the machine
// session agent-secrets kept in the key directory dir, answering whether dir held one, and leaves
// a directory without agent-secrets's marker as it is.
func DropMachineSession(dir string) (bool, error) { return internal.DropMachineSession(dir) }
