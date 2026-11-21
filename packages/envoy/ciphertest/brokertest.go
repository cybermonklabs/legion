// Package ciphertest is a public re-export of internal/cipher/ciphertest's real-broker test rig,
// existing solely so a module outside github.com/sjawhar/envoy — packages/daemon's
// cipher contract test — can drive it: Go's internal-package rule refuses to let any
// package whose own import path does not start with github.com/sjawhar/envoy import a package
// under github.com/sjawhar/envoy/internal/..., which blocks a foreign module from importing
// internal/cipher/ciphertest directly, however workspace-linked the two modules are. This facade
// changes no behavior — Rig is a type alias for the internal package's own type, and NewRig
// simply forwards to it — so every exported field and method internal/cipher/ciphertest.Rig
// carries (Req, UI, MintPodToken, Store, Operator, ...) stays reachable exactly as documented
// there; that file remains the one place the rig's own behavior is described and changed.
package ciphertest

import (
	"testing"

	internal "github.com/sjawhar/envoy/internal/cipher/ciphertest"
	"github.com/sjawhar/envoy/internal/cipher/store/storetest"
)

// Rig is internal/cipher/ciphertest.Rig under a name a foreign module may import.
type Rig = internal.Rig

// DaemonAccount is internal/cipher/ciphertest.DaemonAccount: the ServiceAccount every Rig lets log
// in as legion-daemon with its token.
const DaemonAccount = internal.DaemonAccount

// ExchangeRolesAccount is internal/cipher/ciphertest.ExchangeRolesAccount: the account the Rig's
// exchange lists with no fixed session, whose pods trade only for a session registered for them.
// Rig.MintAccountPodToken mints the token a registration presents for one of its pods, and
// Rig.MintSubjectToken the token that pod trades.
const ExchangeRolesAccount = internal.ExchangeRolesAccount

// NewRig forwards to internal/cipher/ciphertest.NewRig, including its own skip when
// BROKER_TEST_DATABASE_URL is unset, and its failure when the calling package's tests do not run
// through Main.
func NewRig(t *testing.T) *Rig {
	return internal.NewRig(t)
}

// Main runs the calling package's tests and then drops the test binary's own database that every
// Rig makes its schema in (internal/cipher/store/storetest's Main). A package whose tests call
// NewRig uses it as its TestMain: func TestMain(m *testing.M) { os.Exit(ciphertest.Main(m)) }.
func Main(m *testing.M) int { return storetest.Main(m) }
