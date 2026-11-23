package oauth_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/oauth"
	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
	"github.com/sjawhar/envoy/internal/oidc"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

// TestATokenWhoseKeySetCannotBeReadIsAnswered503WithItsWait: a subject or actor token whose key
// the broker cannot read, because its issuer's key set cannot be read now, is answered 503
// OIDC_ISSUER_UNAVAILABLE with Retry-After: 1 and no access token, whichever issuer signed it: a
// cluster's, GitHub's, or Google's, for a machine's own trade and for a person's sign-in, whose
// actor is the machine's token. Before GitHub's and Google's key sources were read as the
// cluster's is, the library refused those three as tokens it could not parse, 400
// invalid_request, which the broker's token clients take as final.
func TestATokenWhoseKeySetCannotBeReadIsAnswered503WithItsWait(t *testing.T) {
	cluster := oidctest.New(t)
	clusterKey := cluster.PublishKey(t, "cluster-key")
	f := newSignInFixtureWith(t, func(c *oauth.Config) {
		c.Exchange.Kubernetes = []oauth.IssuerConfig{{Name: clusterName, Issuer: cluster.URL(), Audience: podAudience, Keys: oidc.NewLazyVerifier(cluster.URL(), podAudience)}}
	})
	// Every key set fails before any token of it is checked, so no key is held from a good read.
	for _, issuer := range []*oidctest.Issuer{cluster, f.github, f.google} {
		issuer.FailKeys(http.StatusServiceUnavailable)
	}
	podClaims := cluster.Claims(account, podAudience)
	podClaims["kubernetes.io"] = map[string]any{"pod": map[string]any{"uid": "pod-uid-unread"}}
	for _, row := range []struct {
		name string
		form url.Values
	}{
		{"a pod's token of a cluster", exchangeForm(cluster.Mint(t, clusterKey, podClaims), oauthwire.SubjectTokenTypeKubernetes, "implementer-g1")},
		{"a workflow run's token of GitHub", exchangeForm(f.workflowToken(t, "4242", nil), oauthwire.SubjectTokenTypeGitHub, "ci-release")},
		{"a machine's token of Google", exchangeForm(f.machineToken(t, signInMachine, machineAudience, true), oauthwire.SubjectTokenTypeGoogle, "fleet-box-1")},
		{"a person's sign-in, whose actor is a machine's token of Google", signInForm(
			f.personToken(t, person, deviceAudience, true, signInDomain),
			f.machineToken(t, signInMachine, machineAudience, true),
			"ada-box-1")},
	} {
		t.Run(row.name, func(t *testing.T) {
			status, header, body := f.post(t, "/token", row.form, newKey(t))
			if status != http.StatusServiceUnavailable || body["code"] != oidc.IssuerUnavailableCode || body["access_token"] != nil {
				t.Fatalf("answered %d %v, want 503 %s and no access token", status, body, oidc.IssuerUnavailableCode)
			}
			if got := header.Get("Retry-After"); got != "1" {
				t.Fatalf("the 503 carries Retry-After %q, want \"1\", the Envoy listener's wait for the same fault", got)
			}
		})
	}
}

// TestATokenSignedByAKeyItsIssuerNeverPublishedIsStillRefused: the control of the test above. A
// GitHub or Google token signed by a key its issuer never published, while that issuer's key set
// is read, is a token the broker judged: the library refuses it 400 invalid_request as a token it
// could not parse, with no access token and no Retry-After, since no wait changes the answer.
//
// While the key sets cannot be read, a token the library refuses whatever key signed it is refused
// the same way and not answered 503: an expired token of GitHub, of Google or of a cluster, a token
// of GitHub or of Google whose iss names another issuer, and a GitHub token with no jti. So is a
// person's token in a sign-in, expired or of another issuer, though the library judges the
// machine's actor token first and stops at its key. A live token of the same key set is answered
// 503 meanwhile, a person's included, so the key set does fail.
func TestATokenSignedByAKeyItsIssuerNeverPublishedIsStillRefused(t *testing.T) {
	type refusalRow struct {
		name string
		form url.Values
	}
	now := time.Now()

	t.Run("while the key sets are read", func(t *testing.T) {
		f := newSignInFixture(t)
		stray := oidctest.New(t).PublishKey(t, "stray-key")
		for _, row := range []refusalRow{
			{"a workflow run's token of GitHub signed by a key GitHub never published", exchangeForm(f.github.Mint(t, stray, map[string]any{
				"iss": f.github.URL(), "sub": "repo:" + repository + ":ref:" + ref, "aud": workflowAudience,
				"iat": now.Add(-time.Minute).Unix(), "exp": now.Add(15 * time.Minute).Unix(), "jti": "run-4343",
			}), oauthwire.SubjectTokenTypeGitHub, "ci-release")},
			{"a machine's token of Google signed by a key Google never published", exchangeForm(f.google.Mint(t, stray, map[string]any{
				"iss": f.google.URL(), "sub": "110" + signInMachine, "aud": machineAudience,
				"iat": now.Add(-time.Minute).Unix(), "exp": now.Add(30 * time.Minute).Unix(),
				"email": signInMachine, "email_verified": true,
			}), oauthwire.SubjectTokenTypeGoogle, "fleet-box-1")},
		} {
			t.Run(row.name, func(t *testing.T) { requireRefused(t, f.exchangeFixture, row.form, unparsedToken) })
		}
	})

	t.Run("while the key sets cannot be read", func(t *testing.T) {
		f := newSignInFixture(t)
		// Every key set fails before any token of it is checked, so no key is held from a good read.
		for _, issuer := range []*oidctest.Issuer{f.cluster, f.github, f.google} {
			issuer.FailKeys(http.StatusServiceUnavailable)
		}
		expired := func(claims map[string]any) {
			claims["iat"], claims["exp"] = now.Add(-20*time.Minute).Unix(), now.Add(-5*time.Minute).Unix()
		}
		elsewhere := func(claims map[string]any) { claims["iss"] = "https://issuer.elsewhere.example" }
		live := func(map[string]any) {}
		machine := func(change func(map[string]any)) string {
			claims := map[string]any{
				"iss": f.google.URL(), "sub": "110" + signInMachine, "aud": machineAudience,
				"iat": now.Add(-time.Minute).Unix(), "exp": now.Add(30 * time.Minute).Unix(),
				"email": signInMachine, "email_verified": true,
			}
			change(claims)
			return f.google.Mint(t, f.googleKey, claims)
		}
		personOf := func(change func(map[string]any)) string {
			claims := map[string]any{
				"iss": f.google.URL(), "sub": "109" + person, "aud": deviceAudience,
				"iat": now.Add(-time.Minute).Unix(), "exp": now.Add(30 * time.Minute).Unix(),
				"email": person, "email_verified": true, "hd": signInDomain,
			}
			change(claims)
			return f.google.Mint(t, f.googleKey, claims)
		}

		for _, row := range []refusalRow{
			{"a live workflow run's token of GitHub", exchangeForm(f.workflowToken(t, "4400", nil), oauthwire.SubjectTokenTypeGitHub, "ci-release")},
			{"a person's sign-in whose person token is live", signInForm(personOf(live), machine(live), "ada-box-1")},
		} {
			t.Run(row.name+" is answered 503", func(t *testing.T) {
				status, header, body := f.post(t, "/token", row.form, newKey(t))
				if status != http.StatusServiceUnavailable || body["code"] != oidc.IssuerUnavailableCode || header.Get("Retry-After") != "1" {
					t.Fatalf("answered %d %v (Retry-After %q), want 503 %s with Retry-After: 1: the key set must fail for the rows after this to mean anything", status, body, header.Get("Retry-After"), oidc.IssuerUnavailableCode)
				}
			})
		}
		for _, row := range []refusalRow{
			{"an expired workflow run's token of GitHub", exchangeForm(f.workflowToken(t, "4401", expired), oauthwire.SubjectTokenTypeGitHub, "ci-release")},
			{"a workflow run's token whose iss is not GitHub's", exchangeForm(f.workflowToken(t, "4402", elsewhere), oauthwire.SubjectTokenTypeGitHub, "ci-release")},
			{"a workflow run's token with no jti", exchangeForm(f.workflowToken(t, "4403", func(claims map[string]any) { delete(claims, "jti") }), oauthwire.SubjectTokenTypeGitHub, "ci-release")},
			{"an expired machine's token of Google", exchangeForm(machine(expired), oauthwire.SubjectTokenTypeGoogle, "fleet-box-1")},
			{"a machine's token whose iss is not Google's", exchangeForm(machine(elsewhere), oauthwire.SubjectTokenTypeGoogle, "fleet-box-1")},
			{"an expired pod's token of a cluster", exchangeForm(f.podToken(t, "pod-uid-expired", expired), oauthwire.SubjectTokenTypeKubernetes, "implementer-g1")},
			{"a person's sign-in whose person token has expired", signInForm(personOf(expired), machine(live), "ada-box-1")},
			{"a person's sign-in whose person token is of another issuer", signInForm(personOf(elsewhere), machine(live), "ada-box-1")},
		} {
			t.Run(row.name+" is refused, not answered 503", func(t *testing.T) { requireRefused(t, f.exchangeFixture, row.form, unparsedToken) })
		}
	})
}

// unparsedToken is the words the library refuses a subject token with when its key function
// refuses it, as the broker answers a token it refuses with no key.
const unparsedToken = "Unable to parse the JSON web token"

// requireRefused fails the test unless form is refused 400 invalid_request with no access token,
// no Retry-After, and a description saying says.
func requireRefused(t *testing.T, f *exchangeFixture, form url.Values, says string) {
	t.Helper()
	status, header, body := f.post(t, "/token", form, newKey(t))
	description, _ := body["error_description"].(string)
	if status != http.StatusBadRequest || body["error"] != "invalid_request" || body["access_token"] != nil || !strings.Contains(description, says) {
		t.Fatalf("answered %d %v, want 400 invalid_request saying %q", status, body, says)
	}
	if got := header.Get("Retry-After"); got != "" {
		t.Fatalf("the refusal carries Retry-After %q, want none", got)
	}
}

// TestASpentJTIIsRefusedWhileTheKeySetCannotBeRead: a workflow run's token is presented once, and
// then GitHub's key set fails. A token carrying the same jti whose key the broker does not hold is
// refused 400 as one presented before, as the library refuses it once it can read the key, and not
// answered 503: the replay store, not a key, says it is spent. A token with a fresh jti and the
// same unread key is answered 503, so the key set does fail.
func TestASpentJTIIsRefusedWhileTheKeySetCannotBeRead(t *testing.T) {
	f := newExchangeFixture(t, func(*oauth.Config) {})
	f.exchanged(t, exchangeForm(f.workflowToken(t, "4500", nil), oauthwire.SubjectTokenTypeGitHub, "ci-release"), newKey(t))
	f.github.FailKeys(http.StatusServiceUnavailable)
	unread := oidctest.New(t).PublishKey(t, "rotated-key")
	now := time.Now()
	token := func(run string) string {
		return f.github.Mint(t, unread, map[string]any{
			"iss": f.github.URL(), "sub": "repo:" + repository + ":ref:" + ref, "aud": workflowAudience,
			"iat": now.Add(-time.Minute).Unix(), "exp": now.Add(15 * time.Minute).Unix(), "jti": "run-" + run,
			"repository": repository, "workflow_ref": repository + "/" + workflow + "@" + ref,
			"job_workflow_ref": repository + "/" + workflow + "@" + ref, "ref": ref, "run_id": run,
		})
	}
	t.Run("a fresh jti whose key cannot be read is answered 503", func(t *testing.T) {
		status, header, body := f.post(t, "/token", exchangeForm(token("4501"), oauthwire.SubjectTokenTypeGitHub, "ci-release"), newKey(t))
		if status != http.StatusServiceUnavailable || body["code"] != oidc.IssuerUnavailableCode || header.Get("Retry-After") != "1" {
			t.Fatalf("answered %d %v (Retry-After %q), want 503 %s with Retry-After: 1", status, body, header.Get("Retry-After"), oidc.IssuerUnavailableCode)
		}
	})
	t.Run("the spent jti is refused, not answered 503", func(t *testing.T) {
		requireRefused(t, f, exchangeForm(token("4500"), oauthwire.SubjectTokenTypeGitHub, "ci-release"), "must be used only once")
	})
}

// TestTheReplayReadSpendsNothing: while GitHub's key set cannot be read, a live workflow run's
// token is presented three times and answered 503 each time, and the replay store then holds no row
// for its jti: the exchange asked the store whether the jti was spent (TokenExchangeJTISpent) and
// wrote nothing. Once the key set answers, the same token mints, and it is then held as presented.
func TestTheReplayReadSpendsNothing(t *testing.T) {
	f := newExchangeFixture(t, func(*oauth.Config) {})
	// No spacing between failed reads, so the read after the key set heals is the next request's
	// and not one 5 seconds later.
	f.config.GitHub.Keys.(*oidc.Keys).FailureSpacing = 0
	f.github.FailKeys(http.StatusServiceUnavailable)
	const run, jti = "4700", "run-4700"
	token := f.workflowToken(t, run, nil)
	rows := func() int {
		t.Helper()
		var n int
		if err := f.pool.QueryRow(context.Background(), `select count(*) from oauth_exchange_jtis where jti = $1`, jti).Scan(&n); err != nil {
			t.Fatalf("count the replay store's rows for %s: %v", jti, err)
		}
		return n
	}
	for i := range 3 {
		status, header, body := f.post(t, "/token", exchangeForm(token, oauthwire.SubjectTokenTypeGitHub, "ci-release"), newKey(t))
		if status != http.StatusServiceUnavailable || body["code"] != oidc.IssuerUnavailableCode || header.Get("Retry-After") != "1" {
			t.Fatalf("presentation %d while the key set cannot be read: answered %d %v (Retry-After %q), want 503 %s with Retry-After: 1", i+1, status, body, header.Get("Retry-After"), oidc.IssuerUnavailableCode)
		}
	}
	if n := rows(); n != 0 {
		t.Fatalf("after three presentations answered 503 the replay store holds %d rows for %s, want none: the read spent the jti", n, jti)
	}
	f.github.FailKeys(0)
	f.exchanged(t, exchangeForm(token, oauthwire.SubjectTokenTypeGitHub, "ci-release"), newKey(t))
	if n := rows(); n != 1 {
		t.Fatalf("after the mint the replay store holds %d rows for %s, want one", n, jti)
	}
}

// TestAnUnreadableReplayStoreIsAnswered503: a workflow run's jti the replay store holds as
// presented, carried by a token whose key the broker does not hold while GitHub's key set cannot be
// read, is refused 400 while the store can be read. While the store cannot be read either (its table
// renamed away), nothing says whether the jti was presented, so the token is answered 503 with
// Retry-After: 1 and not refused. Once the store can be read again, it is refused 400 again.
func TestAnUnreadableReplayStoreIsAnswered503(t *testing.T) {
	f := newExchangeFixture(t, func(*oauth.Config) {})
	const run, jti = "4800", "run-4800"
	if _, err := f.pool.Exec(context.Background(), `insert into oauth_exchange_jtis (jti, expires_at) values ($1, now() + interval '1 hour')`, jti); err != nil {
		t.Fatalf("hold %s as presented: %v", jti, err)
	}
	f.github.FailKeys(http.StatusServiceUnavailable)
	unread := oidctest.New(t).PublishKey(t, "rotated-key")
	now := time.Now()
	token := f.github.Mint(t, unread, map[string]any{
		"iss": f.github.URL(), "sub": "repo:" + repository + ":ref:" + ref, "aud": workflowAudience,
		"iat": now.Add(-time.Minute).Unix(), "exp": now.Add(15 * time.Minute).Unix(), "jti": jti,
		"repository": repository, "workflow_ref": repository + "/" + workflow + "@" + ref,
		"job_workflow_ref": repository + "/" + workflow + "@" + ref, "ref": ref, "run_id": run,
	})
	form := exchangeForm(token, oauthwire.SubjectTokenTypeGitHub, "ci-release")
	rename := func(from, to string) {
		t.Helper()
		if _, err := f.pool.Exec(context.Background(), `alter table `+from+` rename to `+to); err != nil {
			t.Fatalf("rename %s to %s: %v", from, to, err)
		}
	}

	t.Run("the store can be read: the spent jti is refused", func(t *testing.T) {
		requireRefused(t, f, form, "must be used only once")
	})
	rename("oauth_exchange_jtis", "oauth_exchange_jtis_hidden")
	t.Run("the store cannot be read: answered 503", func(t *testing.T) {
		status, header, body := f.post(t, "/token", form, newKey(t))
		if status != http.StatusServiceUnavailable || body["code"] != oidc.IssuerUnavailableCode || header.Get("Retry-After") != "1" {
			t.Fatalf("answered %d %v (Retry-After %q), want 503 %s with Retry-After: 1: an unreadable replay store says nothing of the jti", status, body, header.Get("Retry-After"), oidc.IssuerUnavailableCode)
		}
	})
	rename("oauth_exchange_jtis_hidden", "oauth_exchange_jtis")
	t.Run("the store can be read again: the spent jti is refused", func(t *testing.T) {
		requireRefused(t, f, form, "must be used only once")
	})
}

// TestATokenThatNeverExpiresIsRefusedWhileTheKeySetIsRead: with every key set read, the exchange
// refuses 400 a token that does not expire within its type's window, though the library admits most
// of them: an exp of null, 0, 0.5, 9223371974719179008, 9223372000000000000 or 9223372036854774784
// (the last three overflow the library's own window check), and an exp missing or past the window,
// for GitHub's, Google's and a cluster's tokens. The issuers' own tokens still mint: a GitHub
// workflow token and a Google identity token living an hour, and a pod's projected token living
// 24 hours, the broker's window for a cluster's tokens (kubernetesTolerance).
func TestATokenThatNeverExpiresIsRefusedWhileTheKeySetIsRead(t *testing.T) {
	// The exchange fixture lists the workflow and the pod's account, the sign-in fixture the
	// machine.
	e, g := newExchangeFixture(t, func(*oauth.Config) {}), newSignInFixture(t)
	now := time.Now()
	exp := func(value any) func(map[string]any) {
		return func(claims map[string]any) { claims["exp"] = value }
	}
	lives := func(lifetime time.Duration) func(map[string]any) {
		return func(claims map[string]any) {
			claims["iat"], claims["exp"] = now.Add(-time.Minute).Unix(), now.Add(lifetime-time.Minute).Unix()
		}
	}
	machine := func(change func(map[string]any)) url.Values {
		claims := map[string]any{
			"iss": g.google.URL(), "sub": "110" + signInMachine, "aud": machineAudience,
			"iat": now.Add(-time.Minute).Unix(), "exp": now.Add(30 * time.Minute).Unix(),
			"email": signInMachine, "email_verified": true,
		}
		change(claims)
		return exchangeForm(g.google.Mint(t, g.googleKey, claims), oauthwire.SubjectTokenTypeGoogle, "fleet-box-1")
	}
	github := func(run string, change func(map[string]any)) url.Values {
		return exchangeForm(e.workflowToken(t, run, change), oauthwire.SubjectTokenTypeGitHub, "ci-release")
	}
	pod := func(uid string, change func(map[string]any)) url.Values {
		e.registers(t, account, uid, "implementer-g1")
		return exchangeForm(e.podToken(t, uid, change), oauthwire.SubjectTokenTypeKubernetes, "implementer-g1")
	}
	type tradeRow struct {
		name string
		f    *exchangeFixture
		form url.Values
	}
	for _, row := range []tradeRow{
		{"a workflow run's token with an exp of 0", e, github("4600", exp(0))},
		{"a workflow run's token with an exp of null", e, github("4601", exp(nil))},
		{"a workflow run's token with an exp of 0.5", e, github("4602", exp(json.Number("0.5")))},
		{"a workflow run's token with an exp of 9223371974719179008", e, github("4603", exp(json.Number("9223371974719179008")))},
		{"a workflow run's token with an exp of 9223372000000000000", e, github("4606", exp(json.Number("9223372000000000000")))},
		{"a workflow run's token with an exp of 9223372036854774784", e, github("4607", exp(json.Number("9223372036854774784")))},
		{"a workflow run's token with no exp", e, github("4604", func(claims map[string]any) { delete(claims, "exp") })},
		{"a workflow run's token living a minute past an hour", e, github("4605", lives(time.Hour+2*time.Minute))},
		{"a machine's token with an exp of 0", g.exchangeFixture, machine(exp(0))},
		{"a machine's token with an exp of 9223371974719179008", g.exchangeFixture, machine(exp(json.Number("9223371974719179008")))},
		{"a pod's token with an exp of 0", e, pod("pod-uid-exp-zero", exp(0))},
		{"a pod's token with an exp of 9223371974719179008", e, pod("pod-uid-exp-overflow", exp(json.Number("9223371974719179008")))},
		{"a pod's token with an exp of 9223372036854774784", e, pod("pod-uid-exp-overflow-high", exp(json.Number("9223372036854774784")))},
		{"a pod's token living a minute past 24 hours", e, pod("pod-uid-exp-long", lives(24*time.Hour+2*time.Minute))},
	} {
		t.Run(row.name+" is refused", func(t *testing.T) { requireRefused(t, row.f, row.form, unparsedToken) })
	}
	for _, row := range []tradeRow{
		{"a workflow run's token living an hour", e, github("4610", lives(time.Hour))},
		{"a machine's token living an hour", g.exchangeFixture, machine(lives(time.Hour))},
		{"a pod's token living 24 hours", e, pod("pod-uid-day", lives(24*time.Hour))},
	} {
		t.Run(row.name+" mints", func(t *testing.T) { row.f.exchanged(t, row.form, newKey(t)) })
	}
}
