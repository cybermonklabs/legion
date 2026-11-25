// result_grant_test.go drives a result entry's run through the token exchange: the run asks for
// an airlock placeholder and is answered one, beside a result grant the broker keeps in its
// database, and never an access token. The fixture is exchange_test.go's.
package oauth_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/oauth"
	"github.com/sjawhar/envoy/internal/cipher/oauthstore"
	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
	"github.com/sjawhar/envoy/internal/cipher/placeholder"
)

const (
	// resultsHost is the host the proxy block of the profile DISPATCH_RESULTS_TOKEN binds:
	// Dispatch's public host, the one a grant's writes go to through the proxy.
	resultsHost = "dispatch.test"
	// airlockProxyURL and airlockCA are what the broker hands a run to reach its proxy by.
	airlockProxyURL = "https://airlock.test:13381"
	airlockCA       = "-----BEGIN CERTIFICATE-----\nnot-a-real-certificate\n-----END CERTIFICATE-----\n"
	// resultsEntryDigest stands for the broker's results entry (AirlockSettings.EntryDigest): 64
	// hex digits, as cmd/cipherd computes one and as result_grants holds every one.
	resultsEntryDigest = "1111111111111111111111111111111111111111111111111111111111111111"
)

// newResultsFixture is the exchange fixture of a broker whose airlock binds DISPATCH_RESULTS_TOKEN
// to resultsHost under the entry resultsEntryDigest, with adjust applied last.
func newResultsFixture(t *testing.T, adjust func(*oauth.Config)) *exchangeFixture {
	t.Helper()
	return newExchangeFixture(t, func(c *oauth.Config) {
		c.Airlock = &oauth.AirlockSettings{ProxyURL: airlockProxyURL, CACertificatePEM: airlockCA, ResultHosts: []string{resultsHost}, EntryDigest: resultsEntryDigest}
		adjust(c)
	})
}

// askingForThePlaceholder is form asking for the airlock placeholder as its requested type.
func askingForThePlaceholder(form url.Values) url.Values {
	form.Set("requested_token_type", oauthwire.TokenTypeAirlockPlaceholder)
	return form
}

// resultGrantRow is one row of result_grants as a test reads it.
type resultGrantRow struct {
	ID, PlaceholderSHA256, Subject, Session, Principal string
	Scopes                                             []string
	CheckRunID                                         string
	ExpiresAt                                          time.Time
}

// resultGrants is every row of result_grants, oldest first.
func (f *exchangeFixture) resultGrants(t *testing.T) []resultGrantRow {
	t.Helper()
	rows, err := f.pool.Query(context.Background(), `select id::text, placeholder_sha256, subject, session, principal, scopes,
		coalesce(extra->'ci'->>'check_run_id', ''), expires_at from result_grants order by created_at, id`)
	if err != nil {
		t.Fatalf("read result_grants: %v", err)
	}
	defer rows.Close()
	var grants []resultGrantRow
	for rows.Next() {
		var g resultGrantRow
		if err := rows.Scan(&g.ID, &g.PlaceholderSHA256, &g.Subject, &g.Session, &g.Principal, &g.Scopes, &g.CheckRunID, &g.ExpiresAt); err != nil {
			t.Fatalf("scan result_grants: %v", err)
		}
		grants = append(grants, g)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read result_grants: %v", err)
	}
	return grants
}

// sha256Hex is the hex SHA-256 of text, as result_grants keys a placeholder.
func sha256Hex(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// grantOf is the live grant the placeholder held opens, read as every broker pod reads it: by
// the placeholder's hash, through the store.
func (f *exchangeFixture) grantOf(t *testing.T, held string) oauthstore.MintedGrant {
	t.Helper()
	grant, err := oauthstore.New(f.pool).ResultGrants().MintedByPlaceholder(context.Background(), placeholder.Hash(held))
	if err != nil {
		t.Fatalf("the grant of the placeholder: %v", err)
	}
	return grant
}

// TestAResultEntrysRunIsGrantedAPlaceholderAndNoToken: a run of the result entry's workflow that
// asks for the airlock placeholder is answered 200, no-store, with the placeholder as its
// access_token and as the airlock's proxy password and the value of DISPATCH_RESULTS_TOKEN, bound
// to the entry's one host. The placeholder is no token the broker's introspection knows. The
// broker keeps a grant row holding the run and the workflow's session, and stores no access token.
func TestAResultEntrysRunIsGrantedAPlaceholderAndNoToken(t *testing.T) {
	f := newResultsFixture(t, func(*oauth.Config) {})
	key := newKey(t)
	before := f.count(t, "oauth_access_tokens")
	status, header, body := f.post(t, "/token", askingForThePlaceholder(f.resultRunForm(t, "7")), key)
	if status != http.StatusOK {
		t.Fatalf("the placeholder exchange = %d %v, want 200", status, body)
	}
	if got := header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	held, _ := body["access_token"].(string)
	if prefix := placeholder.MintedPrefix(oauthwire.AirlockResultsWord); !strings.HasPrefix(held, prefix) {
		t.Fatalf("access_token = %q, want a placeholder starting %s", held, prefix)
	}
	if body["issued_token_type"] != oauthwire.TokenTypeAirlockPlaceholder || body["token_type"] != "N_A" {
		t.Errorf("issued_token_type = %v and token_type = %v, want %s and N_A", body["issued_token_type"], body["token_type"], oauthwire.TokenTypeAirlockPlaceholder)
	}
	if expires, _ := body["expires_in"].(float64); expires < 890 || expires > 900 {
		t.Errorf("expires_in = %v, want about 900", body["expires_in"])
	}
	grantID, _ := body[oauthwire.ExtraGrantID].(string)
	if !strings.HasPrefix(grantID, "result:") {
		t.Errorf("grant_id = %q, want result:<id>", grantID)
	}
	airlock, _ := body[oauthwire.ExtraAirlock].(map[string]any)
	names, _ := airlock["names"].(map[string]any)
	entry, _ := names[oauthwire.AirlockResultsName].(map[string]any)
	slots, _ := entry["slots"].(map[string]any)
	if airlock["proxy_url"] != airlockProxyURL || airlock["ca_certificate"] != airlockCA || airlock["proxy_password"] != held ||
		entry["value"] != held || slots["value"] != held || !reflect.DeepEqual(entry["hosts"], []any{resultsHost}) || len(names) != 1 {
		t.Errorf("airlock = %v, want the proxy, its CA, the placeholder as the password and as %s's value and slot, bound to [%s]", airlock, oauthwire.AirlockResultsName, resultsHost)
	}
	if answer := f.introspected(t, held); answer.Active {
		t.Errorf("Introspect(the placeholder) = %+v, want not live", answer)
	}
	grants := f.resultGrants(t)
	if len(grants) != 1 {
		t.Fatalf("result_grants holds %d rows, want 1", len(grants))
	}
	g := grants[0]
	if "result:"+g.ID != grantID || g.PlaceholderSHA256 != sha256Hex(held) || g.Session != resultsSession || g.CheckRunID != "97" ||
		g.Principal == "" || g.Subject != g.Principal || !reflect.DeepEqual(g.Scopes, []string{oauthwire.ScopeResults}) {
		t.Errorf("the grant row = %+v, want id %s, the placeholder's hash, session %s, the run's check run 97, the principal and the scope results", g, grantID, resultsSession)
	}
	if after := f.count(t, "oauth_access_tokens"); after != before {
		t.Errorf("access tokens after the placeholder exchange = %d, want the %d there were", after, before)
	}
}

// TestAResultGrantRecordsTheEntryItWasGrantedUnder: a run's grant records the results entry of the
// broker that made it (AirlockSettings.EntryDigest) in its row's entry_digest, and the grant every
// pod reads by the placeholder carries it, so a pod compares the grant's entry with its own before
// it mints the run's token. Two brokers of two entries each record their own.
func TestAResultGrantRecordsTheEntryItWasGrantedUnder(t *testing.T) {
	const another = "2222222222222222222222222222222222222222222222222222222222222222"
	for _, digest := range []string{resultsEntryDigest, another} {
		t.Run(digest[:4], func(t *testing.T) {
			f := newResultsFixture(t, func(c *oauth.Config) { c.Airlock.EntryDigest = digest })
			held := f.exchanged(t, askingForThePlaceholder(f.resultRunForm(t, "7")), newKey(t))
			var stored string
			if err := f.pool.QueryRow(context.Background(), `select entry_digest from result_grants where placeholder_sha256 = $1`, sha256Hex(held)).Scan(&stored); err != nil {
				t.Fatalf("read the grant's entry_digest: %v", err)
			}
			if read := f.grantOf(t, held).EntryDigest; stored != digest || read != digest {
				t.Errorf("the grant made under the entry %s holds entry_digest %q and reads back %q; want the broker's entry", digest, stored, read)
			}
		})
	}
}

// TestAResultEntrysRunIsMintedNoAccessToken: the same run asking for no type, which is an access
// token, is refused invalid_request with a hint naming the placeholder type, and nothing is
// written: no access token and no grant. Any process of the job can ask GitHub for a fresh run
// token, so this is what keeps a results token out of the job.
func TestAResultEntrysRunIsMintedNoAccessToken(t *testing.T) {
	f := newResultsFixture(t, func(*oauth.Config) {})
	key := newKey(t)
	before := f.count(t, "oauth_access_tokens")
	status, body := f.exchange(t, f.resultRunForm(t, "7"), key)
	refusedSaying(t, status, body, http.StatusBadRequest, "invalid_request", oauthwire.TokenTypeAirlockPlaceholder)
	if after := f.count(t, "oauth_access_tokens"); after != before {
		t.Errorf("access tokens after the refused exchange = %d, want the %d there were", after, before)
	}
	if grants := f.resultGrants(t); len(grants) != 0 {
		t.Errorf("result_grants holds %+v, want nothing", grants)
	}
}

// TestOnlyAResultEntryIsGrantedAPlaceholder: a run of an exact workflow entry and a pod of a
// named entry, each asking for the placeholder, are refused invalid_request, and no grant is
// written.
func TestOnlyAResultEntryIsGrantedAPlaceholder(t *testing.T) {
	f := newResultsFixture(t, func(*oauth.Config) {})
	key := newKey(t)
	f.registers(t, account, "pod-uid-1", "implementer-g1")
	for name, form := range map[string]url.Values{
		"an exact workflow entry's run": exchangeForm(f.workflowToken(t, "4242", nil), oauthwire.SubjectTokenTypeGitHub, "ci-release"),
		"a named entry's pod":           exchangeForm(f.podToken(t, "pod-uid-1", nil), oauthwire.SubjectTokenTypeKubernetes, "implementer-g1"),
	} {
		status, body := f.exchange(t, askingForThePlaceholder(form), key)
		if description, _ := body["error_description"].(string); status != http.StatusBadRequest || body["error"] != "invalid_request" ||
			body["access_token"] != nil || !strings.Contains(description, "Only a result entry's run") {
			t.Errorf("%s asking for the placeholder = %d %v, want 400 invalid_request saying only a result entry's run is granted one", name, status, body)
		}
	}
	if grants := f.resultGrants(t); len(grants) != 0 {
		t.Errorf("result_grants holds %+v, want nothing", grants)
	}
}

// TestARunTokenIsSpentOnceAcrossBothAnswers: one GitHub token exchanged for a placeholder is
// spent: presented again, asking for the placeholder or for an access token, it is refused
// invalid_request saying it must be used only once.
func TestARunTokenIsSpentOnceAcrossBothAnswers(t *testing.T) {
	f := newResultsFixture(t, func(*oauth.Config) {})
	key := newKey(t)
	subject := f.resultRunToken(t, "7")
	form := func() url.Values { return exchangeForm(subject, oauthwire.SubjectTokenTypeGitHub, "") }
	if status, body := f.exchange(t, askingForThePlaceholder(form()), key); status != http.StatusOK {
		t.Fatalf("the first exchange = %d %v, want 200", status, body)
	}
	for name, again := range map[string]url.Values{"the placeholder": askingForThePlaceholder(form()), "an access token": form()} {
		t.Run(name, func(t *testing.T) {
			status, body := f.exchange(t, again, key)
			refusedSaying(t, status, body, http.StatusBadRequest, "invalid_request", "must be used only once")
		})
	}
	if grants := f.resultGrants(t); len(grants) != 1 {
		t.Errorf("result_grants holds %d rows, want the first exchange's 1", len(grants))
	}
}

// TestTwoRunsGetTwoGrants: two runs of the workflow are answered two placeholders and two grant
// rows, each holding its own run's check run.
func TestTwoRunsGetTwoGrants(t *testing.T) {
	f := newResultsFixture(t, func(*oauth.Config) {})
	key := newKey(t)
	var held []string
	for _, run := range []string{"7", "8"} {
		status, body := f.exchange(t, askingForThePlaceholder(f.resultRunForm(t, run)), key)
		if status != http.StatusOK {
			t.Fatalf("run %s's exchange = %d %v, want 200", run, status, body)
		}
		token, _ := body["access_token"].(string)
		held = append(held, token)
	}
	if held[0] == held[1] {
		t.Fatalf("two runs were answered one placeholder %q", held[0])
	}
	grants := f.resultGrants(t)
	if len(grants) != 2 || grants[0].CheckRunID == grants[1].CheckRunID || grants[0].ID == grants[1].ID {
		t.Fatalf("result_grants = %+v, want two rows of two check runs", grants)
	}
	for i, g := range grants {
		if g.PlaceholderSHA256 != sha256Hex(held[i]) {
			t.Errorf("grant %d keys %s, want the hash of its own placeholder", i, g.PlaceholderSHA256)
		}
	}
}

// TestTheGrantLifetimeIsTheShorterOfFifteenMinutesAndTheAccessLifetime: under an access lifetime
// of 900 s a grant lasts 900 s, and under 300 s it lasts 300 s; its row ends when its answer says.
func TestTheGrantLifetimeIsTheShorterOfFifteenMinutesAndTheAccessLifetime(t *testing.T) {
	for _, c := range []struct{ access, want time.Duration }{{15 * time.Minute, 15 * time.Minute}, {5 * time.Minute, 5 * time.Minute}, {time.Hour, 15 * time.Minute}} {
		f := newResultsFixture(t, func(config *oauth.Config) { config.AccessLifetime = c.access })
		asked := time.Now()
		status, body := f.exchange(t, askingForThePlaceholder(f.resultRunForm(t, "7")), newKey(t))
		if status != http.StatusOK {
			t.Fatalf("the exchange under an access lifetime of %v = %d %v, want 200", c.access, status, body)
		}
		if expires, _ := body["expires_in"].(float64); time.Duration(expires)*time.Second > c.want || time.Duration(expires)*time.Second < c.want-10*time.Second {
			t.Errorf("under an access lifetime of %v, expires_in = %v, want %v", c.access, body["expires_in"], c.want.Seconds())
		}
		grants := f.resultGrants(t)
		if len(grants) != 1 {
			t.Fatalf("result_grants holds %d rows, want 1", len(grants))
		}
		if end := grants[0].ExpiresAt.Sub(asked); end < c.want-10*time.Second || end > c.want+10*time.Second {
			t.Errorf("under an access lifetime of %v the grant ends %v after the request, want %v", c.access, end, c.want)
		}
	}
}

// TestAMintedResultsTokenIsNoSubjectTokenForAnotherExchange: the results token a broker pod mints
// for a result grant (MintGrantToken) is live, bound to the pod's key and to the grant's run, and
// ends with the grant; presented as the subject token of another exchange it mints nothing, since
// the library refuses a client's own token, so the capability the airlock holds never turns into
// a token of another session. The access lifetime is an hour, longer than the grant's fifteen
// minutes, so a token that ended with the access lifetime and not with its grant reads apart.
func TestAMintedResultsTokenIsNoSubjectTokenForAnotherExchange(t *testing.T) {
	f := newResultsFixture(t, func(c *oauth.Config) { c.AccessLifetime = time.Hour })
	run := newKey(t)
	grant := f.grantOf(t, f.exchanged(t, askingForThePlaceholder(f.resultRunForm(t, "7")), run))
	pod := newKey(t)
	minted, err := f.provider.MintGrantToken(context.Background(), grant, thumbprint(t, pod))
	if err != nil {
		t.Fatalf("MintGrantToken: %v", err)
	}
	answer := f.introspected(t, minted)
	if !answer.Active || answer.Scope != oauthwire.ScopeResults || answer.Thumbprint != thumbprint(t, pod) || answer.Session != grant.Session ||
		answer.CI == nil || answer.CI.CheckRunID != "97" || answer.ExpiresAt != grant.ExpiresAt.Unix() {
		t.Fatalf("the minted token introspects as %+v (run %+v); want it live, scope %s, bound to the pod's key, the grant's session and run, ending at %v", answer, answer.CI, oauthwire.ScopeResults, grant.ExpiresAt)
	}
	before := f.count(t, "oauth_access_tokens")
	status, body := f.exchange(t, exchangeForm(minted, oauthwire.SubjectTokenTypeAccessToken, "a-session-of-its-own"), pod)
	if status != http.StatusBadRequest || body["access_token"] != nil {
		t.Fatalf("an exchange of a minted results token = %d %v, want 400 and no access token", status, body)
	}
	const ownToken = "Clients are not allowed to perform a token exchange on their own tokens."
	if hint, _ := body["error_description"].(string); body["error"] != "invalid_grant" || !strings.Contains(hint, ownToken) {
		t.Errorf("the refusal reads %v %q, which is not the library's refusal of a client's own token %q", body["error"], hint, ownToken)
	}
	if after := f.count(t, "oauth_access_tokens"); after != before {
		t.Fatalf("access tokens after the refused exchange = %d, want the %d there were", after, before)
	}
}
