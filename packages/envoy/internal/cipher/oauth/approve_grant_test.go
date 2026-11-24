// approve_grant_test.go drives the token exchange's approve grant: a person's sign-in traded for the
// approve capability's proxy placeholder, never for a token that can approve. The fixture is
// person_signin_test.go's.
package oauth_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/exchange"
	"github.com/sjawhar/envoy/internal/cipher/identity/google"
	"github.com/sjawhar/envoy/internal/cipher/oauth"
	"github.com/sjawhar/envoy/internal/cipher/oauthstore"
	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
	"github.com/sjawhar/envoy/internal/cipher/placeholder"
)

// testProxyRoot is the one root approveSettings' proxy is trusted by.
const testProxyRoot = "-----BEGIN CERTIFICATE-----\ntest\n-----END CERTIFICATE-----\n"

// approveSettings is a broker serving the approve capability: its proxy, the roots it is trusted
// by, the one host its dpop block binds, and the digest every grant is made under.
func approveSettings() *oauth.ApproveSettings {
	return &oauth.ApproveSettings{
		EntryDigest: strings.Repeat("ab", 32),
		ProxyURL:    "http://proxy.test:3128", ProxyRoots: func() string { return testProxyRoot }, Host: "dispatch.test",
	}
}

// placeholderForm is the sign-in trade asking for the proxy placeholder.
func (f *signInFixture) placeholderForm(t *testing.T, session string) map[string][]string {
	t.Helper()
	form := signInForm(f.personToken(t, person, deviceAudience, true, signInDomain), f.machineToken(t, signInMachine, machineAudience, true), session)
	form.Set("requested_token_type", oauthwire.TokenTypeProxyPlaceholder)
	return form
}

// TestASignInIsTradedForAnApprovePlaceholderAndNeverAToken: a person's sign-in that asks for the
// proxy placeholder is answered the placeholder of a fresh approve grant - no access token, the
// placeholder type, N_A, at most 900 seconds, the grant's id and the proxy release naming the
// capability's one host - and the placeholder is no token introspection reads as live. The grant
// it wrote is live by that placeholder, holds the scope approve alone, the person, the session and
// the capability's entry digest, and the token the broker mints for it introspects as an
// approve-only token: the project scope person and the scope approve, bound to the pod's key. Once
// the sign-in's credential is revoked, the grant is no longer live.
func TestASignInIsTradedForAnApprovePlaceholderAndNeverAToken(t *testing.T) {
	f := newSignInFixtureWith(t, func(c *oauth.Config) { c.Approve = approveSettings() })
	key := newKey(t)
	before := f.count(t, "oauth_access_tokens")
	status, body := f.exchange(t, f.placeholderForm(t, "ada-box-1"), key)
	held, _ := body["access_token"].(string)
	if status != http.StatusOK || body["issued_token_type"] != oauthwire.TokenTypeProxyPlaceholder || body["token_type"] != "N_A" ||
		!strings.HasPrefix(held, placeholder.MintedPrefix("approve")) {
		t.Fatalf("the placeholder trade = %d %v; want 200, the placeholder type, N_A and an approve placeholder", status, body)
	}
	if seconds, _ := body["expires_in"].(float64); seconds <= 0 || seconds > 900 {
		t.Errorf("expires_in = %v; want at most 900 seconds", body["expires_in"])
	}
	release, _ := body[oauthwire.ExtraProxy].(map[string]any)
	names, _ := release["names"].(map[string]any)
	name, _ := names["DISPATCH_APPROVE_TOKEN"].(map[string]any)
	if release["proxy_password"] != held || name["value"] != held || release["proxy_url"] != "http://proxy.test:3128" {
		t.Errorf("the proxy release = %v; want the placeholder as the proxy password and DISPATCH_APPROVE_TOKEN's value", release)
	}
	if after := f.count(t, "oauth_access_tokens"); after != before {
		t.Fatalf("the trade stored %d access tokens; want none", after-before)
	}
	if answer, err := f.provider.Introspect(context.Background(), held); err != nil || answer.Active {
		t.Fatalf("the placeholder introspects as %+v, %v; want not live", answer, err)
	}

	grants := oauthstore.ApproveGrants{Store: oauthstore.New(f.pool)}
	grantID, _ := body[oauthwire.ExtraGrantID].(string)
	grant, err := grants.MintedByID(context.Background(), strings.TrimPrefix(grantID, "approve:"))
	if err != nil {
		t.Fatalf("the grant %s: %v", grantID, err)
	}
	if byHash, err := grants.MintedByPlaceholder(context.Background(), grant.PlaceholderSHA256); err != nil || byHash.ID != grant.ID {
		t.Fatalf("the grant by its placeholder's hash = %+v, %v", byHash, err)
	}
	if len(grant.Scopes) != 1 || grant.Scopes[0] != "approve" || grant.Session != "ada-box-1" || grant.Extra[oauth.ClaimPerson] != person ||
		grant.EntryDigest != approveSettings().EntryDigest {
		t.Fatalf("the grant = %+v; want the scope approve, session ada-box-1, %s and the entry digest", grant, person)
	}
	pod := newKey(t)
	minted, err := f.provider.MintGrantToken(context.Background(), grant, thumbprint(t, pod))
	if err != nil {
		t.Fatal(err)
	}
	answer, err := f.provider.Introspect(context.Background(), minted)
	if err != nil || !answer.Active || answer.Scope != "approve" || answer.ProjectScope != oauthwire.ProjectScopePerson ||
		answer.Person != person || answer.Session != "ada-box-1" || len(answer.Projects) != 0 || answer.Thumbprint != thumbprint(t, pod) {
		t.Fatalf("the minted token introspects as %+v, %v; want an approve-only token of %s bound to the pod's key", answer, err, person)
	}

	if _, err := f.pool.Exec(context.Background(), `update launcher_credentials set revoked_at = now() where id = (select credential_id from approve_grants where id = $1)`, grant.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := grants.MintedByID(context.Background(), grant.ID); !errors.Is(err, oauthstore.ErrMintedGrantNotLive) {
		t.Fatalf("the grant after its sign-in was revoked = %v; want ErrMintedGrantNotLive", err)
	}
}

// TestAnApproveGrantNamesNoProject: a sign-in trade that names projects and asks for the
// placeholder writes a grant holding the project scope person and no projects, so the token the
// broker mints for it is the approve-only shape (a person, the scope approve, no project), which
// is the one shape Dispatch serves on its approve routes alone.
func TestAnApproveGrantNamesNoProject(t *testing.T) {
	f := newSignInFixtureWith(t, func(c *oauth.Config) {
		c.Exchange.Projects = newFakeAccess()
		c.Approve = approveSettings()
	})
	status, body := f.exchange(t, withProjects(url.Values(f.placeholderForm(t, "ada-box-projects")), "PRIV"), newKey(t))
	id, _ := body[oauthwire.ExtraGrantID].(string)
	if status != http.StatusOK || id == "" {
		t.Fatalf("the placeholder trade naming PRIV = %d %v; want 200 and a grant", status, body)
	}
	grant, err := oauthstore.ApproveGrants{Store: oauthstore.New(f.pool)}.MintedByID(context.Background(), strings.TrimPrefix(id, "approve:"))
	if err != nil {
		t.Fatal(err)
	}
	if projects, named := grant.Extra[oauth.ClaimProjects]; named || grant.Extra[oauth.ClaimProjectScope] != oauthwire.ProjectScopePerson {
		t.Fatalf("the grant's claims hold projects %v (%v) and project scope %v; want no projects and the project scope person", projects, named, grant.Extra[oauth.ClaimProjectScope])
	}
	minted, err := f.provider.MintGrantToken(context.Background(), grant, thumbprint(t, newKey(t)))
	if err != nil {
		t.Fatal(err)
	}
	if answer, err := f.provider.Introspect(context.Background(), minted); err != nil || !answer.Active || len(answer.Projects) != 0 || answer.Scope != "approve" {
		t.Fatalf("the minted token introspects as %+v, %v; want live, scope approve, no project", answer, err)
	}
}

// TestAnApprovePlaceholderCarriesTheProxyRootsOfItsTrade: the proxy release beside the placeholder
// carries the roots ProxyRoots answers at the trade, not at the broker's start: a pod on a pool
// learns its root from its first intermediate, after the start, and a rollout adds a second root.
// So a trade before the pod holds an intermediate carries no root, and each later trade the roots
// as they are then.
func TestAnApprovePlaceholderCarriesTheProxyRootsOfItsTrade(t *testing.T) {
	roots := ""
	f := newSignInFixtureWith(t, func(c *oauth.Config) {
		c.Approve = approveSettings()
		c.Approve.ProxyRoots = func() string { return roots }
	})
	carried := func(session string) any {
		t.Helper()
		status, body := f.exchange(t, f.placeholderForm(t, session), newKey(t))
		release, _ := body[oauthwire.ExtraProxy].(map[string]any)
		if status != http.StatusOK || release == nil {
			t.Fatalf("the placeholder trade for %s = %d %v; want 200 and a proxy release", session, status, body)
		}
		return release["ca_certificate"]
	}
	if got := carried("ada-box-roots-1"); got != nil && got != "" {
		t.Errorf("a trade before the pod holds an intermediate carries the roots %q; want none", got)
	}
	roots = testProxyRoot
	if got := carried("ada-box-roots-2"); got != testProxyRoot {
		t.Errorf("a trade once the pod holds its root carries %q; want that root", got)
	}
	roots = testProxyRoot + "-----BEGIN CERTIFICATE-----\nnext\n-----END CERTIFICATE-----\n"
	if got := carried("ada-box-roots-3"); got != roots {
		t.Errorf("a trade during a rollout of a second root carries %q; want both roots", got)
	}
}

// TestOnlyASignInIsTradedForTheApprovePlaceholder: the placeholder is refused invalid_request, with
// nothing written, to a broker that serves no approve capability and to every trade that is no
// person's sign-in: a dev box's machine token trade asking for it, and a machine whose fleet entry
// names a person, whose trade acts for that person and holds a launcher credential but carries no
// sign-in of theirs.
func TestOnlyASignInIsTradedForTheApprovePlaceholder(t *testing.T) {
	plain := newSignInFixture(t)
	status, body := plain.exchange(t, plain.placeholderForm(t, "ada-box-2"), newKey(t))
	if status != http.StatusBadRequest || body["error"] != "invalid_request" {
		t.Fatalf("the placeholder trade on a broker serving no approve capability = %d %v; want 400 invalid_request", status, body)
	}
	f := newSignInFixtureWith(t, func(c *oauth.Config) { c.Approve = approveSettings() })
	key := newKey(t)
	form := exchangeForm(f.devBoxToken(t, key, person), oauthwire.SubjectTokenTypeAccessToken, "ada-box-3")
	form.Set("requested_token_type", oauthwire.TokenTypeProxyPlaceholder)
	status, body = f.exchange(t, form, key)
	if status != http.StatusBadRequest || body["error"] != "invalid_request" {
		t.Fatalf("a machine token's placeholder trade = %d %v; want 400 invalid_request", status, body)
	}
	if n := f.count(t, "approve_grants"); n != 0 {
		t.Fatalf("the refused trades wrote %d approve grants; want none", n)
	}
	// An entry names its person with a project, which the exchange holds to Dispatch's answer.
	access := newFakeAccess()
	access.sees[person] = []string{project}
	fleet := newSignInFixtureWith(t, func(c *oauth.Config) {
		c.Exchange.Projects = access
		c.Approve = approveSettings()
	}, listing(nil, nil, google.GoogleEntry{Account: signInMachine, Service: signInFleet, Project: project, Person: person}))
	form = exchangeForm(fleet.machineToken(t, signInMachine, machineAudience, true), oauthwire.SubjectTokenTypeGoogle, "example-machine")
	if traded := fleet.exchanged(t, form, key); traded == "" {
		t.Fatal("the fleet's machine trade minted no token for its person; the row below would prove nothing")
	}
	form.Set("requested_token_type", oauthwire.TokenTypeProxyPlaceholder)
	status, body = fleet.exchange(t, form, key)
	if status != http.StatusBadRequest || body["error"] != "invalid_request" {
		t.Fatalf("the placeholder trade of a machine whose fleet names %s = %d %v; want 400 invalid_request", person, status, body)
	}
	if n := fleet.count(t, "approve_grants"); n != 0 {
		t.Fatalf("the fleet machine's refused trade wrote %d approve grants; want none", n)
	}
}

// TestAPlaceholderTradeThatAsksForReadIsRefused: a trade narrowed to reading approves nothing, so
// a sign-in's placeholder trade that asks for the scope read - with the DPoP-Holder proof a
// person's read trade sends, with a cut or without one - is refused invalid_request naming the
// scope, and writes no approve grant and no access token of either table. The sign-in takes a
// grant of its own first, so it holds the credential a read trade names and the refusal is not
// the one a sign-in holding none gets.
func TestAPlaceholderTradeThatAsksForReadIsRefused(t *testing.T) {
	f := newSignInFixtureWith(t, func(c *oauth.Config) { c.Approve = approveSettings() })
	box := newKey(t)
	if status, body := f.exchange(t, f.placeholderForm(t, "ada-box-read"), box); status != http.StatusOK {
		t.Fatalf("the sign-in's first placeholder trade = %d %v; want 200", status, body)
	}
	grants := f.count(t, "approve_grants")
	tokens := f.count(t, "oauth_access_tokens") + f.count(t, "oauth_narrowed_access_tokens")
	for _, c := range []struct{ name, cut string }{
		{"with no cut", ""},
		{"with a cut", time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)},
	} {
		t.Run(c.name, func(t *testing.T) {
			form := url.Values(f.placeholderForm(t, "ada-box-read"))
			form.Set("scope", oauthwire.ScopeRead)
			if c.cut != "" {
				form.Set(oauthwire.FormAsOf, c.cut)
			}
			status, body := f.postWithHeaders(t, form, http.Header{
				"DPoP":                     {dpopProof(t, box, f.url+"/token")},
				oauthwire.HeaderDPoPHolder: {dpopProof(t, newKey(t), f.url+"/token")},
			})
			refusedSaying(t, status, body, http.StatusBadRequest, "invalid_request", "asks for the scope read")
			if after := f.count(t, "approve_grants"); after != grants {
				t.Errorf("the refused trade left %d approve grants; want the %d there were", after, grants)
			}
			if after := f.count(t, "oauth_access_tokens") + f.count(t, "oauth_narrowed_access_tokens"); after != tokens {
				t.Errorf("the refused trade left %d access tokens; want the %d there were", after, tokens)
			}
		})
	}
}

// TestAnApproveGrantEndsAtItsEndOrWithItsSignIn: the approve_grants table of migration 0046 as its
// two reads answer it. A grant is live by its placeholder's hash and by its id while it lasts. Past
// its end it is live by neither, and so is a grant whose sign-in's credential has passed its own
// expiry; an id that is no uuid is no grant. The row holds the placeholder's SHA-256 and never the
// placeholder, and the delete of the sign-in's credential removes the grant made from it.
func TestAnApproveGrantEndsAtItsEndOrWithItsSignIn(t *testing.T) {
	f := newSignInFixtureWith(t, func(c *oauth.Config) { c.Approve = approveSettings() })
	ctx := context.Background()
	grants := oauthstore.ApproveGrants{Store: oauthstore.New(f.pool)}
	trade := func(session string) (string, oauthstore.MintedGrant) {
		t.Helper()
		status, body := f.exchange(t, f.placeholderForm(t, session), newKey(t))
		held, _ := body["access_token"].(string)
		id, _ := body[oauthwire.ExtraGrantID].(string)
		if status != http.StatusOK || held == "" {
			t.Fatalf("the placeholder trade for %s = %d %v; want 200 and a placeholder", session, status, body)
		}
		grant, err := grants.MintedByID(ctx, strings.TrimPrefix(id, "approve:"))
		if err != nil {
			t.Fatalf("the fresh grant %s: %v; want it live", id, err)
		}
		if byHash, err := grants.MintedByPlaceholder(ctx, grant.PlaceholderSHA256); err != nil || byHash.ID != grant.ID {
			t.Fatalf("the fresh grant by its placeholder's hash = %+v, %v; want %s", byHash, err, grant.ID)
		}
		return held, grant
	}
	exec := func(statement, id string) {
		t.Helper()
		if _, err := f.pool.Exec(ctx, statement, id); err != nil {
			t.Fatal(err)
		}
	}
	notLive := func(what string, grant oauthstore.MintedGrant) {
		t.Helper()
		if _, err := grants.MintedByID(ctx, grant.ID); !errors.Is(err, oauthstore.ErrMintedGrantNotLive) {
			t.Errorf("%s by its id = %v; want ErrMintedGrantNotLive", what, err)
		}
		if _, err := grants.MintedByPlaceholder(ctx, grant.PlaceholderSHA256); !errors.Is(err, oauthstore.ErrMintedGrantNotLive) {
			t.Errorf("%s by its placeholder's hash = %v; want ErrMintedGrantNotLive", what, err)
		}
	}

	held, ended := trade("ada-box-ended")
	var row string
	if err := f.pool.QueryRow(ctx, `select row_to_json(a)::text from approve_grants a where id = $1`, ended.ID).Scan(&row); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(held))
	if strings.Contains(row, held) || !strings.Contains(row, hex.EncodeToString(sum[:])) {
		t.Errorf("the grant's row holds the placeholder, or not its SHA-256: %d bytes read", len(row))
	}
	exec(`update approve_grants set expires_at = now() - interval '1 second' where id = $1`, ended.ID)
	notLive("a grant past its end", ended)

	_, lapsed := trade("ada-box-lapsed")
	exec(`update launcher_credentials set expires_at = now() - interval '1 second'
		where id = (select credential_id from approve_grants where id = $1)`, lapsed.ID)
	notLive("a grant whose sign-in's credential has expired", lapsed)

	if _, err := grants.MintedByID(ctx, "not-a-uuid"); !errors.Is(err, oauthstore.ErrMintedGrantNotLive) {
		t.Errorf("an id that is no uuid = %v; want ErrMintedGrantNotLive", err)
	}

	// Each trade here proves a box key of its own, so each grant hangs from a credential of its
	// own, and the delete of one credential takes its one grant and leaves the other two.
	_, deleted := trade("ada-box-deleted")
	before := f.count(t, "approve_grants")
	exec(`delete from launcher_credentials where id = (select credential_id from approve_grants where id = $1)`, deleted.ID)
	var left int
	if err := f.pool.QueryRow(ctx, `select count(*) from approve_grants where id = $1`, deleted.ID).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if after := f.count(t, "approve_grants"); before != 3 || after != 2 || left != 0 {
		t.Fatalf("approve grants before and after the credential's delete = %d and %d, the deleted one's rows %d; want 3, 2 and 0", before, after, left)
	}
}

// TestAPlaceholderTradeThatTakesASessionNameLogsIt: a placeholder trade claims its session name as
// every exchange does (claimSession), so a claim that takes the name from the account paired with
// its own writes the warning the exchange writes for the same take, once the claim has committed,
// naming the session, whom it took the name from and who has it now.
func TestAPlaceholderTradeThatTakesASessionNameLogsIt(t *testing.T) {
	f := newSignInFixtureWith(t, func(c *oauth.Config) { c.Approve = approveSettings() })
	const session, holder = "ada-box-paired", "account:system:serviceaccount:legion:earlier"
	ctx := context.Background()
	if _, err := f.store.BindSession(ctx, session, holder, false, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	f.store.SetSessionPairs([][2]string{{exchange.PersonPrincipal(person), holder}})
	logged := capturedLog(t)
	if status, body := f.exchange(t, f.placeholderForm(t, session), newKey(t)); status != http.StatusOK {
		t.Fatalf("the placeholder trade under the paired account's name = %d %v; want 200", status, body)
	}
	lines := logged.lines(`"msg":"broker: the token exchange took a session name from the account paired with its own"`,
		`"session":"`+session+`"`, `"from":"`+holder+`"`, `"to":"`+exchange.PersonPrincipal(person)+`"`)
	if len(lines) != 1 {
		t.Fatalf("%d warnings name the session, its earlier holder and the person; want 1", len(lines))
	}
}

// TestAPlaceholderTradeHoldsItsSessionNameUntilItsGrantEnds: a placeholder trade claims its session
// name as every exchange does (claimSession), for the person, until oauthstore.SessionRetention
// past the grant's end, which is the expiry the grant sets on the trade's session. While the claim
// lasts, an identity paired with no other is refused the name.
func TestAPlaceholderTradeHoldsItsSessionNameUntilItsGrantEnds(t *testing.T) {
	f := newSignInFixtureWith(t, func(c *oauth.Config) { c.Approve = approveSettings() })
	const session = "ada-box-held"
	ctx := context.Background()
	status, body := f.exchange(t, f.placeholderForm(t, session), newKey(t))
	id, _ := body[oauthwire.ExtraGrantID].(string)
	if status != http.StatusOK || id == "" {
		t.Fatalf("the placeholder trade for %s = %d %v; want 200 and a grant", session, status, body)
	}
	grant, err := oauthstore.ApproveGrants{Store: oauthstore.New(f.pool)}.MintedByID(ctx, strings.TrimPrefix(id, "approve:"))
	if err != nil {
		t.Fatal(err)
	}
	var holder string
	var until time.Time
	if err := f.pool.QueryRow(ctx, `select principal, expires_at from oauth_session_bindings where session = $1`, session).Scan(&holder, &until); err != nil {
		t.Fatalf("the claim on %s: %v; want the placeholder trade to have claimed it", session, err)
	}
	if want := grant.ExpiresAt.Add(oauthstore.SessionRetention); holder != exchange.PersonPrincipal(person) || !until.Equal(want) {
		t.Fatalf("the claim on %s = %s until %s; want %s until %s, the retention past the grant's end", session, holder, until, exchange.PersonPrincipal(person), want)
	}
	if _, err := f.store.BindSession(ctx, session, "account:system:serviceaccount:legion:stranger", false, time.Now().Add(time.Hour)); !errors.Is(err, oauthstore.ErrSessionHeld) {
		t.Fatalf("a stranger's claim of %s = %v; want ErrSessionHeld", session, err)
	}
}

// TestTheSweepDeletesApproveGrantsThatHaveEnded: every review trades a grant of its own, so the
// broker's sweep (oauthstore.Store.Sweep) deletes each grant past its end, through the table's
// expires index, and keeps a live one, so the table does not grow with every review ever made.
func TestTheSweepDeletesApproveGrantsThatHaveEnded(t *testing.T) {
	f := newSignInFixtureWith(t, func(c *oauth.Config) { c.Approve = approveSettings() })
	ctx := context.Background()
	grantID := func(session string) string {
		t.Helper()
		status, body := f.exchange(t, f.placeholderForm(t, session), newKey(t))
		id, _ := body[oauthwire.ExtraGrantID].(string)
		if status != http.StatusOK || id == "" {
			t.Fatalf("the placeholder trade for %s = %d %v; want 200 and a grant", session, status, body)
		}
		return strings.TrimPrefix(id, "approve:")
	}
	ended, live := grantID("ada-box-swept"), grantID("ada-box-kept")
	if _, err := f.pool.Exec(ctx, `update approve_grants set expires_at = now() - interval '1 second' where id = $1`, ended); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Sweep(ctx); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	var left string
	if err := f.pool.QueryRow(ctx, `select coalesce(string_agg(id::text, ','), '') from approve_grants`).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != live {
		t.Fatalf("approve grants after the sweep = %q; want the live grant %s alone, the ended %s deleted", left, live, ended)
	}
}
