package api

// The units below are what the S3c-issues and S4 tests need of later slices' test files, for
// those slices of cybermonklabs/legion#4674: H's markRestricted (project_access_test.go, S2's) and
// stubBrokerTokens (broker_token_test.go, S7's), and the seed's writes of an access list or a
// group through the access store's writes the access routes make (S2's routes, which this release
// does not serve). Each is a test-setup bridge of the slice lists; the slice that brings its file
// deletes it.

import (
	"context"
	"testing"

	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/ciphertoken/ciphertokentest"
	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// Test helpers the S3c-documents tests read; the rest of this file is S3c-issues's.

// stubBrokerTokens is the broker-token check a test server builds on its database, asking broker
// whether a token is live, with each option applied to the verifier's options before it is built.
func stubBrokerTokens(t *testing.T, broker *ciphertokentest.StubBroker, options ...func(*ciphertoken.Options)) func(*store.Store) *ciphertoken.Verifier {
	return func(database *store.Store) *ciphertoken.Verifier {
		opts := ciphertoken.Options{
			BrokerURL: broker.Server.URL,
			Account:   ciphertoken.NewAccountToken("DISPATCH_AGENT_SECRETS_ACCOUNT_TOKEN_FILE", accountTokenFile(t, credentialTestAccountToken)),
			Origins:   []string{testServerURL},
			Replay:    store.NewDPoPProofs(database.Pool),
			// What Dispatch itself passes: the check keys its bucket on the address
			// Dispatch's own proxy-trust rule reads, never one of its own, and serves a
			// read-only token on the reads of its read class and an approve-only token on the
			// routes its approve list names (cmd/dispatch, brokerTokenVerifier).
			Address:           auth.ForwardedAddress,
			ServesReadOnly:    true,
			ServesApproveOnly: true,
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

// markRestricted marks login a restricted member, as S2's access write does.
func markRestricted(t *testing.T, database *store.Store, login string) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), `update people set member_kind = 'restricted' where email = $1`, login); err != nil {
		t.Fatalf("mark %s restricted: %v", login, err)
	}
}
