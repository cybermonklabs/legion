package api

// The unit below is what S4's tests need of a later slice's test file, for slice S4 of
// cybermonklabs/legion#4674: H's stubBrokerTokens (broker_token_test.go, S7's). It is a
// test-setup bridge of the slice lists; the slice that brings its file deletes it.

import (
	"testing"

	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/ciphertoken/ciphertokentest"
	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// stubBrokerTokens is the broker-token check a test server builds on its database, asking broker
// whether a token is live, with each option applied to the verifier's options before it is built.
func stubBrokerTokens(t *testing.T, broker *ciphertokentest.StubBroker, options ...func(*ciphertoken.Options)) func(*store.Store) *ciphertoken.Verifier {
	return func(database *store.Store) *ciphertoken.Verifier {
		opts := ciphertoken.Options{
			BrokerURL: broker.Server.URL,
			Account:   ciphertoken.NewAccountToken(accountTokenFile(t, credentialTestAccountToken)),
			Origins:   []string{testServerURL},
			Replay:    store.NewDPoPProofs(database.Pool),
			// What Dispatch itself passes: the check keys its bucket on the address
			// Dispatch's own proxy-trust rule reads, never one of its own, and serves a
			// read-only token on the routes its read list names (cmd/dispatch,
			// brokerTokenVerifier).
			Address:        auth.ForwardedAddress,
			ServesReadOnly: true,
		}
		for _, option := range options {
			option(&opts)
		}
		verifier, err := ciphertoken.New(opts)
		if err != nil {
			t.Fatalf("build the broker token verifier: %v", err)
		}
		return verifier
	}
}
