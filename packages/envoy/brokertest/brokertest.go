// Package brokertest is a public re-export of internal/broker/brokertest's real-broker test rig,
// existing solely so a module outside github.com/sjawhar/envoy — packages/daemon's
// agentsecrets contract test — can drive it: Go's internal-package rule refuses to let any
// package whose own import path does not start with github.com/sjawhar/envoy import a package
// under github.com/sjawhar/envoy/internal/..., which blocks a foreign module from importing
// internal/broker/brokertest directly, however workspace-linked the two modules are. This facade
// changes no behavior — Rig is a type alias for the internal package's own type, and NewRig
// simply forwards to it — so every exported field and method internal/broker/brokertest.Rig
// carries (Req, UI, MintPodToken, Store, Operator, ...) stays reachable exactly as documented
// there; that file remains the one place the rig's own behavior is described and changed.
package brokertest

import (
	"testing"

	internal "github.com/sjawhar/envoy/internal/broker/brokertest"
	"github.com/sjawhar/envoy/internal/broker/store/storetest"
)

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
func NewRig(t *testing.T) *Rig {
	return internal.NewRig(t)
}

// Main is internal/broker/store/storetest.Main, which every Rig's database needs: a package whose
// tests call NewRig returns it from TestMain, func TestMain(m *testing.M) {
// os.Exit(brokertest.Main(m.Run)) }, and it drops the template the rigs' databases are cloned
// from once run has returned.
func Main(run func() int) int {
	return storetest.Main(run)
}

// Available reports whether BROKER_TEST_DATABASE_URL is set, the server every Rig's database is
// made on: whether NewRig would give a rig rather than skip.
func Available() bool {
	return storetest.Available()
}
