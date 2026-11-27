package api

// The units below are what the S3c-issues and S4 tests need of later slices' test files, for
// those slices of cybermonklabs/legion#4674: H's markRestricted (project_access_test.go, S2's) and
// stubBrokerTokens (broker_token_test.go, S7's), and the seed's writes of an access list or a
// group through the access store's writes the access routes make (S2's routes, which this release
// does not serve). Each is a test-setup bridge of the slice lists; the slice that brings its file
// deletes it.

import (
	"context"
	"fmt"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/ciphertoken/ciphertokentest"
	"github.com/sjawhar/envoy/internal/dispatch/access"
	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
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

// The units below are issue_repo_restricted_test.go's (S7), which repowrite_private_owner_test.go
// (S3c-issues) reads; S7 deletes them here when that file comes.

// issueWritesSnapshot is everything an issue write could have written: each issue's repository,
// title and status, and how many events, related-item records and near-copy records exist.
func issueWritesSnapshot(t *testing.T, database *store.Store) string {
	t.Helper()
	var snapshot string
	if err := database.Pool.QueryRow(context.Background(), `
		select concat_ws(' | ',
			(select count(*) from events), (select count(*) from related_checks), (select count(*) from near_copy_checks),
			(select coalesce(string_agg(concat_ws(',', key, coalesce(repo, '-'), title, status), '; ' order by key), '') from issues))
	`).Scan(&snapshot); err != nil {
		t.Fatalf("read what the issue writes wrote: %v", err)
	}
	return snapshot
}

// proxyShows answers whether the GitHub proxy shows login repository: its own read
// (routes' repositoryVisibleQuery, refs.RepositoryClause) for the viewer it resolves a cookie to
// (access.Resolve of model.BrowserActor).
func proxyShows(t *testing.T, database *store.Store, login, repository string) bool {
	t.Helper()
	viewer, err := access.Resolve(context.Background(), database.Pool, model.BrowserActor(login))
	if err != nil {
		t.Fatalf("resolve what %s sees: %v", login, err)
	}
	var shown bool
	if err := database.Pool.QueryRow(context.Background(), `select `+refs.RepositoryClause("$2", "$1"),
		refs.VisibilityArg(model.Actor{Viewer: &viewer}), repository).Scan(&shown); err != nil {
		t.Fatalf("read whether %s sees %s: %v", login, repository, err)
	}
	return shown
}

// repoOf is a repository an answer names, "<none>" for none.
func repoOf(repo *string) string {
	if repo == nil {
		return "<none>"
	}
	return *repo
}

// maskedAnswer is an answer with answerMasks applied, the bytes two runs of one build share.
func maskedAnswer(response *httptest.ResponseRecorder) string {
	body := strings.TrimSpace(response.Body.String())
	for _, mask := range answerMasks {
		body = mask.pattern.ReplaceAllString(body, mask.replacement)
	}
	return fmt.Sprintf("%d %s", response.Code, body)
}

// answerMasks are the parts of an answer that differ from one run to the next: a time, a uuid and a
// duration in milliseconds.
var answerMasks = []struct {
	pattern     *regexp.Regexp
	replacement string
}{
	{regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})`), "<time>"},
	{regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`), "<uuid>"},
	{regexp.MustCompile(`"(\w+_ms)":\d+`), `"$1":<ms>`},
}
