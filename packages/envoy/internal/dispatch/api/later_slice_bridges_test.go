package api

// The units below are what S4's tests need of later slices' test files, for slice S4 of
// cybermonklabs/legion#4674: H's markRestricted (project_access_test.go, S2's) and
// stubBrokerTokens (broker_token_test.go, S7's), and the seed's writes of an access list or a
// group through the access store's writes the access routes make (S2's routes, which this release
// does not serve). Each is a test-setup bridge of the slice lists; the slice that brings its file
// deletes it.

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/ciphertoken/ciphertokentest"
	"github.com/sjawhar/envoy/internal/dispatch/access"
	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// markRestricted marks login a restricted member, as S2's access write does.
func markRestricted(t *testing.T, database *store.Store, login string) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), `update people set member_kind = 'restricted' where email = $1`, login); err != nil {
		t.Fatalf("mark %s restricted: %v", login, err)
	}
}

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

// accessWrite runs write in a transaction as carol, the owner of the seed's projects, commits it
// and publishes the event it appended, as an access route does once its write commits: this
// release serves no route that writes an access list or a group (PUT and DELETE
// /api/v1/projects/{key}/access/people/{email} and /api/v1/projects/{key}/access/groups/{name},
// and PUT /api/v1/access-groups/{name}, come with a later slice), so a test reaches the state such
// a route leaves through the access store's write the route makes.
func (s privateSeed) accessWrite(t *testing.T, what string, write func(ctx context.Context, tx pgx.Tx, by model.Actor) (model.Event, error)) {
	t.Helper()
	ctx := context.Background()
	tx, err := s.database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin to %s: %v", what, err)
	}
	defer tx.Rollback(ctx)
	event, err := write(ctx, tx, model.Actor{Kind: "user", ID: seedCarol})
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit to %s: %v", what, err)
	}
	s.server.publish(ctx, event)
}

// addPrincipal names principal, a person (access.PrincipalPerson) or a group
// (access.PrincipalGroup), on project key's access list, as PUT
// /api/v1/projects/{key}/access/people/{email} or .../access/groups/{name} does (accessWrite).
func (s privateSeed) addPrincipal(t *testing.T, key, kind, principal string) {
	t.Helper()
	s.accessWrite(t, "name "+principal+" on "+key+"'s list", func(ctx context.Context, tx pgx.Tx, by model.Actor) (model.Event, error) {
		_, event, err := access.AddPrincipal(ctx, tx, s.deps.Events, key, kind, principal, by)
		return event, err
	})
}

// removePrincipal takes principal, a person or a group, off project key's access list, as DELETE
// /api/v1/projects/{key}/access/people/{email} or .../access/groups/{name} does (accessWrite).
func (s privateSeed) removePrincipal(t *testing.T, key, kind, principal string) {
	t.Helper()
	s.accessWrite(t, "take "+principal+" off "+key+"'s list", func(ctx context.Context, tx pgx.Tx, by model.Actor) (model.Event, error) {
		_, event, err := access.RemovePrincipal(ctx, tx, s.deps.Events, key, kind, principal, by)
		return event, err
	})
}

// putGroup makes group name, carol's, holding exactly members, or writes it again, as carol's PUT
// /api/v1/access-groups/{name} does (accessWrite).
func (s privateSeed) putGroup(t *testing.T, name string, members ...string) {
	t.Helper()
	s.accessWrite(t, "write group "+name, func(ctx context.Context, tx pgx.Tx, by model.Actor) (model.Event, error) {
		_, event, err := access.PutGroup(ctx, tx, s.deps.Events, name, seedCarol, members, by)
		return event, err
	})
}
