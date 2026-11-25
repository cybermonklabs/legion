// packages/envoy/internal/cipher/requests/release_give_back_test.go
// A release that fails after it minted a GitHub App token for one of the grant's names hands that
// token to nobody, so it gives it back to GitHub (Machine.GiveBacks): driven against the airlock's
// GitHub stand-in through the broker's real github-app minter, with the fixture of
// profile_check_test.go's helpers.
package requests_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/githubname"
	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
	"github.com/sjawhar/envoy/internal/cipher/proxyrules"
	"github.com/sjawhar/envoy/internal/cipher/proxyrules/proxyrulestest"
	"github.com/sjawhar/envoy/internal/cipher/requests"
	"github.com/sjawhar/envoy/internal/cipher/secrets"
	"github.com/sjawhar/envoy/internal/cipher/store/storetest"
)

// giveBackFixture is a broker with one live box session of checkOperator and two agent-tier
// secrets of theirs, which the policy grants at once: APP_KEY, a github-app secret minted against
// the stand-in, installed on standInOwner alone and minting a token of its own for every request,
// and ZED_KEY, a stored one, which the grant's name order reads after it. minted is every token
// the stand-in minted, in order.
type giveBackFixture struct {
	*requests.Machine
	local        *secrets.Local
	enrollmentID string
	grantID      string
	service      *proxyrulestest.Service

	mu     sync.Mutex
	minted []string
}

// newGiveBackFixture is the fixture of APP_KEY served through a profile of its own name, with each
// name rules binds behind its binding's kind of policytest.Kinds, by its secret's proxy tag.
func newGiveBackFixture(t *testing.T, rules proxyrules.Rules) *giveBackFixture {
	t.Helper()
	return newGiveBackFixtureOf(t, rules, policytest.KindsFile(policytest.AppProfile("APP_KEY")))
}

// newGiveBackFixtureOf is newGiveBackFixture serving APP_KEY through the profiles of file, which
// declares the kinds the bindings of rules name: each binding is the proxy tag its secret carries
// (policytest.Proxied), so the policy puts that name behind that kind.
func newGiveBackFixtureOf(t *testing.T, rules proxyrules.Rules, file string) *giveBackFixture {
	t.Helper()
	st := storetest.Open(t)
	stored := map[string]secrets.LocalSecret{
		"APP_KEY": policytest.TypeSecret("APP_KEY", checkOperator, policy.TierAgent, policy.TypeGitHubApp, standInAppValue(t)),
		"ZED_KEY": policytest.Secret("ZED_KEY", checkOperator, policy.TierAgent, "zed-v1"),
	}
	for _, binding := range rules.Names {
		secret, held := stored[binding.Name]
		if !held {
			t.Fatalf("the binding %+v names no secret of the fixture", binding)
		}
		stored[binding.Name] = policytest.Proxied(secret, binding.Kind)
	}
	local := secrets.NewLocal(stored["APP_KEY"], stored["ZED_KEY"])
	enrollmentID, key := newEnrollment(t, st)
	service, err := proxyrulestest.NewService(proxyrulestest.ServiceOptions{Plain: true})
	if err != nil {
		t.Fatalf("start the GitHub stand-in: %v", err)
	}
	t.Cleanup(service.Close)
	stand := proxyrulestest.NewGitHubStand(proxyrulestest.GitHubOptions{Installations: map[string]int64{standInOwner: 7}, DistinctTokens: true})
	f := &giveBackFixture{local: local, enrollmentID: enrollmentID, service: service}
	service.AnswerWith(func(w http.ResponseWriter, r *http.Request, seen proxyrulestest.Request) bool {
		if !isTokenRequest(seen) {
			return stand.Answer(w, r, seen)
		}
		answered := httptest.NewRecorder()
		stand.Answer(answered, r, seen)
		var token struct {
			Token string `json:"token"`
		}
		if json.Unmarshal(answered.Body.Bytes(), &token) == nil && token.Token != "" {
			f.mu.Lock()
			f.minted = append(f.minted, token.Token)
			f.mu.Unlock()
		}
		for name, values := range answered.Header() {
			w.Header()[name] = values
		}
		w.WriteHeader(answered.Code)
		_, _ = w.Write(answered.Body.Bytes())
		return true
	})
	app := secrets.GitHubApp{Base: service.URL()}
	f.Machine = &requests.Machine{
		Store:      st,
		Policy:     policytest.ProfiledCurrent(t, local, file),
		Secrets:    policytest.Store(local),
		PendingTTL: 12 * time.Hour,
		Audience:   checkAudience,
		Skew:       time.Minute,
		Replay:     replayer(st),
		Minters:    map[string]requests.Minter{policy.TypeGitHubApp: app.Mint},
		GiveBacks:  map[string]requests.GiveBack{policy.TypeGitHubApp: app.GiveBack},
	}
	f.Chain = requests.NewChainVerifier(st, checkAudience, time.Minute, f.Policy)
	session := &profileFixture{Machine: f.Machine, enrollmentID: enrollmentID, key: key}
	request, err := session.create(t, "APP_KEY", "ZED_KEY")
	if err != nil || request.State != "granted" || request.GrantID == nil {
		t.Fatalf("Create = %+v, %v; want a grant at once of two agent-tier secrets", request, err)
	}
	f.grantID = *request.GrantID
	return f
}

// live is the tokens the stand-in minted that it still holds as live: a read of one of
// standInOwner's repositories made with each, which every token of this owner-wide grant reaches,
// is answered 401 only for a token it forgot at its revoke.
func (f *giveBackFixture) live(t *testing.T) (minted, live []string) {
	t.Helper()
	f.mu.Lock()
	minted = append([]string(nil), f.minted...)
	f.mu.Unlock()
	for _, token := range minted {
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, f.service.URL()+"/repos/"+standInOwner+"/"+standInRepository, nil)
		if err != nil {
			t.Fatalf("build the read: %v", err)
		}
		request.Header.Set("Authorization", "Bearer "+token)
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatalf("read with a minted token: %v", err)
		}
		_ = response.Body.Close()
		if response.StatusCode != http.StatusUnauthorized {
			live = append(live, token)
		}
	}
	return minted, live
}

// TestAReleaseThatFailsGivesBackTheTokensItMinted: a release reads a grant's names in order and
// mints a GitHub App token for APP_KEY before it reads ZED_KEY. When anything after that mint fails
// - ZED_KEY's own read, the airlock's split of it, the recheck of its stored source, the airlock
// placeholders for it, the release's grant.used row, or the proxy's own release of an airlock
// name - the release hands nothing over, and the token it minted is revoked at GitHub rather than
// left live for an hour with no holder. So is the token a session's release that succeeds minted
// for APP_KEY behind the airlock, which it minted only to learn the slots it hands placeholders
// for. A release that hands its token over - the session's for a name the airlock does not hold
// back, the proxy's for one it does - revokes nothing, which is what says the revoke is for a token
// nobody holds.
func TestAReleaseThatFailsGivesBackTheTokensItMinted(t *testing.T) {
	zedForAWS := proxyrules.Rules{Names: []proxyrules.Binding{{Name: "ZED_KEY", Kind: "aws"}}}
	zedBehind := proxyrules.Rules{Names: []proxyrules.Binding{{Name: "ZED_KEY", Kind: "example"}}}
	appBehind := proxyrules.Rules{Names: []proxyrules.Binding{{Name: "APP_KEY", Kind: "example"}}}
	for _, tc := range []struct {
		name  string
		rules proxyrules.Rules
		// breakIt makes the failure, after the grant exists and before the release.
		breakIt func(t *testing.T, f *giveBackFixture)
		// airlockRelease releases the grant as the airlock's proxy does (AirlockValues), and
		// otherwise as the session does (Values).
		airlockRelease bool
		want           error
		// live is how many of the tokens minted are live after the release.
		live int
	}{
		{name: "a release that succeeds", live: 1},
		// The session is handed APP_KEY's placeholders, and the proxy mints a value of its own when
		// it first carries one (AirlockValues), so the token minted to learn APP_KEY's slots goes to
		// nobody.
		{name: "a release that succeeds with APP_KEY behind the airlock", rules: appBehind},
		{name: "the proxy's release that succeeds", rules: appBehind, airlockRelease: true, live: 1},
		{name: "a later name whose value the store no longer holds", want: requests.ErrSecretNotInStore,
			breakIt: func(t *testing.T, f *giveBackFixture) { f.local.Delete(policytest.ID("ZED_KEY")) }},
		{name: "a later name the airlock cannot split", rules: zedForAWS, want: requests.ErrAirlockValueShape},
		// The minted value names its one installation twice, as ada and ADA, which the airlock's
		// split refuses, since one placeholder would stand for both: APP_KEY's own token, minted
		// moments before, goes to nobody.
		{name: "a minted value the airlock cannot split", rules: appBehind, want: requests.ErrAirlockValueShape,
			breakIt: func(t *testing.T, f *giveBackFixture) {
				mint := f.Minters[policy.TypeGitHubApp]
				f.Minters[policy.TypeGitHubApp] = func(ctx context.Context, req secrets.MintRequest) (secrets.Minted, error) {
					minted, err := mint(ctx, req)
					var value struct {
						Installations map[string]json.RawMessage `json:"installations"`
					}
					if err != nil || json.Unmarshal([]byte(minted.Value), &value) != nil {
						t.Errorf("the mint: %v", err)
						return minted, err
					}
					value.Installations[strings.ToUpper(standInOwner)] = value.Installations[standInOwner]
					doubled, _ := json.Marshal(value)
					minted.Value = string(doubled)
					return minted, nil
				}
			}},
		{name: "a later name the policy stops serving during the reads", want: requests.ErrGrantNotLive,
			breakIt: func(t *testing.T, f *giveBackFixture) {
				mint := f.Minters[policy.TypeGitHubApp]
				f.Minters[policy.TypeGitHubApp] = func(ctx context.Context, req secrets.MintRequest) (secrets.Minted, error) {
					minted, err := mint(ctx, req)
					// ZED_KEY's tags no longer make it an agent secret, so the policy reloaded
					// here drops it, while its stored value the release reads next is still there.
					unserved := policytest.Secret("ZED_KEY", checkOperator, "nobody's tier", "zed-v1")
					f.local.Put(unserved)
					if err := f.Policy.Refresh(context.Background()); err != nil {
						t.Errorf("policy refresh: %v", err)
					}
					return minted, err
				}
			}},
		{name: "a release whose airlock placeholders cannot be written", rules: zedBehind, want: errAnyFailure,
			breakIt: func(t *testing.T, f *giveBackFixture) { renameTable(t, f, "airlock_placeholders") }},
		{name: "a release whose grant.used row cannot be written", want: errAnyFailure,
			breakIt: func(t *testing.T, f *giveBackFixture) { renameTable(t, f, "audit") }},
		{name: "the proxy's release whose grant.used row cannot be written", rules: appBehind, airlockRelease: true, want: errAnyFailure,
			breakIt: func(t *testing.T, f *giveBackFixture) { renameTable(t, f, "audit") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newGiveBackFixture(t, tc.rules)
			if tc.breakIt != nil {
				tc.breakIt(t, f)
			}
			var err error
			if tc.airlockRelease {
				_, _, err = f.AirlockValues(t.Context(), f.grantID)
			} else {
				var release requests.Release
				release, err = f.Values(t.Context(), f.grantID, f.enrollmentID, requests.ReleaseCall{})
				if err != nil && release.Values != nil {
					t.Fatalf("a failed release handed over %d values", len(release.Values))
				}
			}
			switch {
			case tc.want == nil && err != nil:
				t.Fatalf("the release answered %v, want a release", err)
			case tc.want == errAnyFailure && err == nil, tc.want != nil && tc.want != errAnyFailure && !errors.Is(err, tc.want):
				t.Fatalf("the release answered %v, want %v", err, tc.want)
			}
			f.WaitGiveBacks()
			minted, live := f.live(t)
			if len(minted) != 1 || len(live) != tc.live {
				t.Fatalf("the release minted %d tokens, %d of them live afterwards; want one, %d live", len(minted), len(live), tc.live)
			}
			if err != nil && strings.Contains(err.Error(), minted[0]) {
				t.Fatalf("the refusal holds the token: %v", err)
			}
		})
	}
}

// TestAReleaseDoesNotWaitOnItsGiveBack: a give-back runs in the background, so a release answers
// at once however long GitHub takes to answer the revoke. The give-back here holds until the test
// lets it go, and the session's release of APP_KEY behind the airlock, which gives APP_KEY's token
// back, and the release whose grant.used row fails, which gives it back too, each answer while
// it is still held; once it is let go it runs, with the secret's name and the value minted.
func TestAReleaseDoesNotWaitOnItsGiveBack(t *testing.T) {
	appBehind := proxyrules.Rules{Names: []proxyrules.Binding{{Name: "APP_KEY", Kind: "example"}}}
	for _, failing := range []bool{false, true} {
		t.Run(fmt.Sprintf("a release that fails: %t", failing), func(t *testing.T) {
			f := newGiveBackFixture(t, appBehind)
			held := make(chan struct{})
			var gaveBack []string
			f.GiveBacks = map[string]requests.GiveBack{policy.TypeGitHubApp: func(ctx context.Context, name, value, form string) {
				<-held
				gaveBack = append(gaveBack, name)
			}}
			if failing {
				renameTable(t, f, "audit")
			}
			answered := make(chan error, 1)
			go func() {
				_, err := f.Values(t.Context(), f.grantID, f.enrollmentID, requests.ReleaseCall{})
				answered <- err
			}()
			select {
			case err := <-answered:
				if (err != nil) != failing {
					t.Fatalf("the release answered %v, want a failure %t", err, failing)
				}
			case <-time.After(10 * time.Second):
				close(held)
				t.Fatal("the release did not answer while its give-back was held")
			}
			close(held)
			f.WaitGiveBacks()
			if len(gaveBack) != 1 || gaveBack[0] != "APP_KEY" {
				t.Fatalf("the give-back ran for %q, want APP_KEY once", gaveBack)
			}
		})
	}
}

// TestAGiveBackIsHandedTheProfilesValueForm: a release gives each value it hands nobody back with
// the value form of the GitHub profile it was minted for (Machine.GiveBacks), so the minter's
// GiveBack reads the value as that form says instead of guessing it from the value's text. APP_KEY
// of either form, behind the airlock so the session's release gives its token back, is given back
// as its own form, and the token it minted is revoked at GitHub.
func TestAGiveBackIsHandedTheProfilesValueForm(t *testing.T) {
	appBehind := proxyrules.Rules{Names: []proxyrules.Binding{{Name: "APP_KEY", Kind: "example"}}}
	for form, profile := range map[string]string{
		githubname.ValueInstallations: policytest.AppProfile("APP_KEY"),
		githubname.ValueToken:         `{"name": "APP_KEY", "github": {"accounts": ["` + standInOwner + `"], "permissions": {"metadata": "read"}, "value": "token"}}`,
	} {
		t.Run(form, func(t *testing.T) {
			f := newGiveBackFixtureOf(t, appBehind, policytest.KindsFile(profile))
			giveBack := f.GiveBacks[policy.TypeGitHubApp]
			var forms []string
			f.GiveBacks = map[string]requests.GiveBack{policy.TypeGitHubApp: func(ctx context.Context, name, value, given string) {
				forms = append(forms, given)
				giveBack(ctx, name, value, given)
			}}
			if _, err := f.Values(t.Context(), f.grantID, f.enrollmentID, requests.ReleaseCall{}); err != nil {
				t.Fatalf("Values: %v", err)
			}
			f.WaitGiveBacks()
			if len(forms) != 1 || forms[0] != form {
				t.Fatalf("the give-back was handed the forms %q; want %q once", forms, form)
			}
			if minted, live := f.live(t); len(minted) != 1 || len(live) != 0 {
				t.Fatalf("the release minted %d tokens, %d of them live after the give-back; want one, revoked", len(minted), len(live))
			}
		})
	}
}

// errAnyFailure is a row's want for a failure whose error is the store's own.
var errAnyFailure = errors.New("any failure")

// renameTable takes table out of the fixture's own schema, so the release's write to it cannot be
// made and everything the release did before that write stands.
func renameTable(t *testing.T, f *giveBackFixture, table string) {
	t.Helper()
	if _, err := f.Store.Pool.Exec(context.Background(), `alter table `+table+` rename to `+table+`_gone`); err != nil {
		t.Fatalf("rename the %s table: %v", table, err)
	}
}
