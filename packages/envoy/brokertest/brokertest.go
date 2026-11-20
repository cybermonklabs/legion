// Package brokertest is a public re-export of internal/broker/brokertest's real-broker test rig,
// existing solely so a module outside github.com/sjawhar/envoy — packages/daemon's
// agentsecrets contract test and its sandbox runtime's kind test — can drive it: Go's
// internal-package rule refuses to let any package whose own import path does not start with
// github.com/sjawhar/envoy import a package under github.com/sjawhar/envoy/internal/..., which
// blocks a foreign module from importing internal/broker/brokertest directly, however
// workspace-linked the two modules are. This facade changes no behavior — Rig is a type alias for
// the internal package's own type, and NewRig simply forwards to it, with its options — so every
// exported field and method internal/broker/brokertest.Rig
// carries (Req, UI, MintPodToken, Store, Operator, ...) stays reachable exactly as documented
// there; that file remains the one place the rig's own behavior is described and changed.
package brokertest

import (
	"net/http"
	"testing"

	internal "github.com/sjawhar/envoy/internal/broker/brokertest"
	"github.com/sjawhar/envoy/internal/broker/policy"
)

// Option is internal/broker/brokertest.Option: how NewRig wires its broker.
type Option = internal.Option

// The owner and tiers a secret PutSecret writes is tagged with (internal/broker/policy):
// OwnerShared for a secret nobody owns alone, TierAgent for one its owner's agents use without
// asking, TierHuman for one a person approves every request for.
const (
	OwnerShared = policy.OwnerShared
	TierAgent   = policy.TierAgent
	TierHuman   = policy.TierHuman
)

// WithListen is internal/broker/brokertest.WithListen: the broker listens on address, which a
// process outside the test's own reaches.
func WithListen(address string) Option { return internal.WithListen(address) }

// WithClusterIssuer is internal/broker/brokertest.WithClusterIssuer: the broker verifies pod,
// login and caller tokens against a real cluster's service-account issuer for audience.
func WithClusterIssuer(issuer, audience string, client *http.Client, logins, services map[string]string) Option {
	return internal.WithClusterIssuer(issuer, audience, client, logins, services)
}

// Rig is internal/broker/brokertest.Rig under a name a foreign module may import.
type Rig = internal.Rig

// DaemonAccount is internal/broker/brokertest.DaemonAccount: the ServiceAccount every Rig lets log
// in as legion-daemon with its token.
const DaemonAccount = internal.DaemonAccount

// ExchangeRolesAccount is internal/broker/brokertest.ExchangeRolesAccount: the account the Rig's
// exchange lists with no fixed session, whose pods trade only for a session registered for them.
// Rig.MintAccountPodToken mints the token a registration presents for one of its pods, and
// Rig.MintSubjectToken the token that pod trades.
const ExchangeRolesAccount = internal.ExchangeRolesAccount

// NewRig forwards to internal/broker/brokertest.NewRig, including its own skip when
// BROKER_TEST_DATABASE_URL is unset.
func NewRig(t *testing.T, options ...Option) *Rig {
	return internal.NewRig(t, options...)
}
