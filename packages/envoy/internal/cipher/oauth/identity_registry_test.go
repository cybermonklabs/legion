package oauth_test

import (
	"context"
	"net/http"
	"net/url"
	"slices"
	"testing"
	"time"

	"authelia.com/provider/oauth2/handler/rfc8693"

	"github.com/sjawhar/envoy/internal/cipher/identity/github"
	"github.com/sjawhar/envoy/internal/cipher/identity/kubernetes"
	"github.com/sjawhar/envoy/internal/cipher/oauth"
	"github.com/sjawhar/envoy/internal/cipher/oauthstore"
	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

// TestEachSubjectTypeIsHeldToItsIssuerAudienceAndExpiry: whichever provider registered it, every
// subject token type is held to the issuer, the audience and the expiry its provider configured.
// A token for another audience, one of another issuer and one past its expiry are each answered
// 400 and no access token, for a pod's, a workflow run's, a machine's and a person's token alike,
// while a sound token of each type trades.
func TestEachSubjectTypeIsHeldToItsIssuerAudienceAndExpiry(t *testing.T) {
	s := newSignInFixtureWith(t, func(*oauth.Config) {}, func(id *identities) {
		id.accounts = []kubernetes.AccountEntry{{Account: account, Project: project, Person: person}}
		id.workflows = []github.WorkflowEntry{{Repository: repository, Workflow: workflow, Ref: ref, Project: project, Person: person}}
	})
	stranger := oidctest.New(t)
	strangerKey := stranger.PublishKey(t, "stranger-key")
	now := time.Now()
	// mint signs claims with the issuer the type trusts, or with the stranger under its own
	// address when byStranger is set.
	mint := func(trusted *oidctest.Issuer, key *oidctest.Key, claims map[string]any, byStranger bool) string {
		if byStranger {
			claims["iss"] = stranger.URL()
			return stranger.Mint(t, strangerKey, claims)
		}
		return trusted.Mint(t, key, claims)
	}
	googleClaims := func(subject, email, audience string) map[string]any {
		return map[string]any{
			"iss": s.googleIssuer.URL(), "sub": subject, "aud": audience, "iat": now.Add(-time.Minute).Unix(), "exp": now.Add(time.Hour).Unix(),
			"email": email, "email_verified": true,
		}
	}
	types := []struct {
		name string
		form func(spoil func(map[string]any), byStranger bool) url.Values
	}{
		{"a pod's", func(spoil func(map[string]any), byStranger bool) url.Values {
			claims := s.cluster.Claims(account, podAudience)
			claims["kubernetes.io"] = map[string]any{"pod": map[string]any{"uid": "pod-uid-1"}}
			spoil(claims)
			return exchangeForm(mint(s.cluster, s.clusterKey, claims, byStranger), oauthwire.SubjectTokenTypeKubernetes, "implementer-g1")
		}},
		{"a workflow run's", func(spoil func(map[string]any), byStranger bool) url.Values {
			claims := map[string]any{
				"iss": s.githubIssuer.URL(), "sub": "repo:" + repository + ":ref:" + ref, "aud": workflowAudience,
				"iat": now.Add(-time.Minute).Unix(), "exp": now.Add(15 * time.Minute).Unix(), "jti": "run-" + time.Now().Format(time.RFC3339Nano),
				"repository": repository, "workflow_ref": repository + "/" + workflow + "@" + ref,
				"job_workflow_ref": repository + "/" + workflow + "@" + ref, "ref": ref,
			}
			spoil(claims)
			return exchangeForm(mint(s.githubIssuer, s.githubKey, claims, byStranger), oauthwire.SubjectTokenTypeGitHub, "release-bot")
		}},
		{"a machine's", func(spoil func(map[string]any), byStranger bool) url.Values {
			claims := googleClaims("machine-subject", signInMachine, machineAudience)
			spoil(claims)
			return exchangeForm(mint(s.googleIssuer, s.googleKey, claims, byStranger), oauthwire.SubjectTokenTypeGoogle, "example-machine")
		}},
		{"a person's", func(spoil func(map[string]any), byStranger bool) url.Values {
			claims := googleClaims("109ada", person, deviceAudience)
			claims["hd"] = signInDomain
			spoil(claims)
			return signInForm(mint(s.googleIssuer, s.googleKey, claims, byStranger), s.machineToken(t, signInMachine, machineAudience, true), "ada-box")
		}},
	}
	s.registers(t, account, "pod-uid-1", "implementer-g1")
	sound := func(map[string]any) {}
	for _, c := range types {
		t.Run(c.name+" sound token trades", func(t *testing.T) {
			status, body := s.exchange(t, c.form(sound, false), newKey(t))
			accessTokenOf(t, status, body)
		})
		for spoilt, how := range map[string]struct {
			spoil      func(map[string]any)
			byStranger bool
		}{
			"for another audience": {spoil: func(claims map[string]any) { claims["aud"] = "https://elsewhere.example" }},
			"of another issuer":    {spoil: sound, byStranger: true},
			"past its expiry": {spoil: func(claims map[string]any) {
				claims["iat"], claims["exp"] = now.Add(-2*time.Hour).Unix(), now.Add(-time.Hour).Unix()
			}},
		} {
			t.Run(c.name+" token "+spoilt, func(t *testing.T) {
				status, body := s.exchange(t, c.form(how.spoil, how.byStranger), newKey(t))
				if status != http.StatusBadRequest || body["access_token"] != nil {
					t.Fatalf("the trade = %d %v, want 400 and no access token", status, body)
				}
			})
		}
	}
}

// TestAnActorTokenBesideAPodsTokenIsIgnoredAsBefore: only a type that names an actor reads the
// actor token. A pod's trade that carries a machine's actor token minted for another audience is
// answered as a pod's trade, as it was before: the pod's type names no actor, so nothing holds
// the stray actor token to anything.
func TestAnActorTokenBesideAPodsTokenIsIgnoredAsBefore(t *testing.T) {
	s := newSignInFixtureWith(t, func(*oauth.Config) {}, func(id *identities) {
		id.accounts = []kubernetes.AccountEntry{{Account: account, Project: project, Person: person}}
	})
	s.registers(t, account, "pod-uid-1", "implementer-g1")
	form := exchangeForm(s.podToken(t, "pod-uid-1", nil), oauthwire.SubjectTokenTypeKubernetes, "implementer-g1")
	form.Set("actor_token", s.machineToken(t, signInMachine, "https://elsewhere.example", true))
	form.Set("actor_token_type", oauthwire.ActorTokenTypeMachine)
	status, body := s.exchange(t, form, newKey(t))
	s.introspectsAs(t, accessTokenOf(t, status, body), "implementer-g1", person, project)
}

// TestTheExchangeClientPermitsEveryRegisteredType: the exchange client may present every subject
// type an identity provider registers, configured or not, beside the broker's own access token,
// and the machine's actor type and nothing else as an actor, as it listed them before the
// registry.
func TestTheExchangeClientPermitsEveryRegisteredType(t *testing.T) {
	f := newExchangeFixture(t, func(*oauth.Config) {})
	store := oauthstore.New(f.pool)
	if _, err := oauth.New(oauth.Config{
		Secret: make([]byte, 32), DeviceLifetime: time.Minute, AccessLifetime: time.Minute, RefreshLifetime: time.Hour,
		ProofSkew: time.Second, Exchange: oauth.ExchangeConfig{Registry: identities{}.registry(t)},
	}, store); err != nil {
		t.Fatalf("oauth.New: %v", err)
	}
	client, err := store.GetClient(context.Background(), oauthwire.ExchangeClientID)
	if err != nil {
		t.Fatalf("GetClient: %v", err)
	}
	exchanger := client.(rfc8693.Client)
	subject := slices.Sorted(slices.Values(exchanger.GetSupportedSubjectTokenTypes()))
	want := slices.Sorted(slices.Values([]string{
		oauthwire.SubjectTokenTypeKubernetes, oauthwire.SubjectTokenTypeGitHub,
		oauthwire.SubjectTokenTypeGoogle, oauthwire.SubjectTokenTypePerson,
		oauthwire.SubjectTokenTypeAccessToken,
	}))
	if !slices.Equal(subject, want) {
		t.Errorf("subject token types = %v, want %v", subject, want)
	}
	if got := exchanger.GetSupportedActorTokenTypes(); !slices.Equal(got, []string{oauthwire.ActorTokenTypeMachine}) {
		t.Errorf("actor token types = %v, want the machine's Google identity token alone", got)
	}
}
