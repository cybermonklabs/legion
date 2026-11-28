package ciphertest

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/api"
)

// WithoutDispatchSignIn wires the broker as BROKER_DISPATCH_SIGNIN unset does: every step of a
// Dispatch sign-in, its device request, approval, device code grant, refresh and trade, is
// refused DISPATCH_SIGNIN_OFF, and every other login is served as ever. Without it a Rig serves
// the Dispatch sign-in, as a broker started with BROKER_DISPATCH_SIGNIN=on does.
func WithoutDispatchSignIn() Option {
	return func(c *rigConfig) { c.dispatchSignInOff = true }
}

// ReplicaWithoutDispatchSignIn starts a second broker beside this Rig's, on its database and with
// its wiring but with BROKER_DISPATCH_SIGNIN unset, as the setting's rollback leaves replicas of
// both while it rolls, and answers that replica's base URL. Its provider shares the store's
// signing secret, so it serves every login and token of the Rig's, and its machine-login service
// holds the Rig's URL as the audience a login's request object names; its own URL is the address
// its token endpoint holds a DPoP proof to, so a proof sent to it names that URL. Every step of a
// Dispatch sign-in it is asked, an approval, a device code grant, a refresh and a trade, is
// refused DISPATCH_SIGNIN_OFF.
func (r *Rig) ReplicaWithoutDispatchSignIn(t *testing.T) string {
	t.Helper()
	options := append(slices.Clone(r.providerOptions), func(c *providerConfig) { c.dispatchSignIn = false })
	provider := NewOAuthProvider(t, r.Store, r.podIssuer, options...)
	logins := *r.deps.MachineLogin
	logins.OAuth = provider
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	deps := r.deps
	deps.PublicURL = srv.URL
	deps.OAuth, deps.MachineLogin, deps.DeviceLogin, deps.TokenHooks = provider, &logins, logins.LoginDevice, logins.TokenHooks()
	api.Register(mux, deps)
	return srv.URL
}
