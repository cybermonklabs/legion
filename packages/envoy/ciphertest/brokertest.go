// Package ciphertest is a public re-export of internal/cipher/ciphertest's real-broker test rig,
// existing solely so a module outside github.com/sjawhar/envoy — packages/daemon's
// cipher contract test — can drive it: Go's internal-package rule refuses to let any
// package whose own import path does not start with github.com/sjawhar/envoy import a package
// under github.com/sjawhar/envoy/internal/..., which blocks a foreign module from importing
// internal/cipher/ciphertest directly, however workspace-linked the two modules are. This facade
// changes no behavior — Rig, Option and Introspection are type aliases for the internal packages'
// own types, and NewRig and WithAccessLifetime simply forward to it — so every exported field and
// method internal/cipher/ciphertest.Rig carries (Req, UI, Introspect, MintPodToken, Store,
// Operator, ...) stays reachable exactly as documented there; that file remains the one place the
// rig's own behavior is described and changed.
package ciphertest

import (
	"testing"
	"time"

	internal "github.com/sjawhar/envoy/internal/cipher/ciphertest"
	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
	"github.com/sjawhar/envoy/internal/cipher/store/storetest"
)

// Rig is internal/cipher/ciphertest.Rig under a name a foreign module may import.
type Rig = internal.Rig

// Option is internal/cipher/ciphertest.Option: one change to how NewRig wires its broker.
type Option = internal.Option

// Introspection is internal/cipher/oauthwire.Introspection: what the broker's introspection reads
// one of its access tokens back as, which Rig.Introspect answers.
type Introspection = oauthwire.Introspection

// DaemonAccount is internal/cipher/ciphertest.DaemonAccount: the ServiceAccount every Rig lets log
// in as legion-daemon with its token.
const DaemonAccount = internal.DaemonAccount

// ExchangeRolesAccount is internal/cipher/ciphertest.ExchangeRolesAccount: the account the Rig's
// exchange lists with no fixed session, whose pods trade only for a session registered for them.
// Rig.MintAccountPodToken mints the token a registration presents for one of its pods, and
// Rig.MintSubjectToken the token that pod trades.
const ExchangeRolesAccount = internal.ExchangeRolesAccount

// DaemonExchangeAccount is internal/cipher/ciphertest.DaemonExchangeAccount: the account the
// Rig's exchange lists for a Legion daemon's own pod, whose entry fixes the session
// legion-daemon:EXAMPLE for project EXAMPLE and names the Rig's Operator as its person.
// Rig.MintSubjectToken mints the token that pod trades.
const DaemonExchangeAccount = internal.DaemonExchangeAccount

// OtherProjectAccount is internal/cipher/ciphertest.OtherProjectAccount: the account the Rig's
// exchange lists for a pod of a second project, whose entry fixes the session
// other-project-worker for project OTHER and names the Rig's Operator as its person.
const OtherProjectAccount = internal.OtherProjectAccount

// OtherProject is internal/cipher/ciphertest.OtherProject: the project OtherProjectAccount's entry
// names, and OtherProjectSession the session that entry fixes.
const (
	OtherProject        = internal.OtherProject
	OtherProjectSession = internal.OtherProjectSession
)

// ExchangeProject is internal/cipher/ciphertest.ExchangeProject: the project every account of the
// Rig's exchange but OtherProjectAccount names, ExchangeRolesAccount's and DaemonExchangeAccount's
// among them.
const ExchangeProject = internal.ExchangeProject

// NewRig forwards to internal/cipher/ciphertest.NewRig, including its own skip when
// BROKER_TEST_DATABASE_URL is unset, and its failure when the calling package's tests do not run
// through Main or Run.
func NewRig(t *testing.T, opts ...Option) *Rig {
	return internal.NewRig(t, opts...)
}

// WithAccessLifetime forwards to internal/cipher/ciphertest.WithAccessLifetime: how long an
// access token the Rig's broker issues lasts, fifteen minutes unless a test says otherwise.
func WithAccessLifetime(d time.Duration) Option {
	return internal.WithAccessLifetime(d)
}

// Main runs the calling package's tests and then drops the test binary's own database that every
// Rig makes its schema in (internal/cipher/store/storetest's Main). A package whose tests call
// NewRig uses it as its TestMain: func TestMain(m *testing.M) { os.Exit(ciphertest.Main(m)) }.
func Main(m *testing.M) int { return storetest.Main(m) }

// Run is Main for a package whose TestMain also runs another harness's Main: run runs the
// package's tests, and Run then drops the database every Rig made its schema in
// (internal/cipher/store/storetest's Run).
func Run(run func() int) int { return storetest.Run(run) }
