package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/exchange"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess"
	"github.com/sjawhar/envoy/internal/ciphertoken"
)

// newRestrictedTokenRig's broker asks a stand-in of Dispatch's membership answer
// (projectaccesstest), which lists erin on ACC for the whole test whatever the real Dispatch beside
// it does. These rows instead point the real broker's project check at the real Dispatch's own
// GET /api/v1/access/person and GET /api/v1/access/projects/{key}, as a deployment's
// BROKER_PROJECT_ACCESS_URL does, with the broker's service-account token that
// DISPATCH_ACCESS_ACCOUNTS lists, so an owner's write to a list reaches the broker's answer.

// realAccess is the broker's question to the real Dispatch through a projectaccess.Client a row
// can replace by a new one (forget), which holds no answer: what the broker holds once its cache of
// Dispatch's answers (projectaccess.CacheTTL) has run out, without the row waiting for that.
type realAccess struct {
	mu      sync.Mutex
	fresh   func() *projectaccess.Client
	current *projectaccess.Client
}

func (a *realAccess) client() *projectaccess.Client {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.current
}

// forget drops every answer the broker holds, as their running out does.
func (a *realAccess) forget() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.current = a.fresh()
}

func (a *realAccess) Access(ctx context.Context, project, person string) (projectaccess.Answer, error) {
	return a.client().Access(ctx, project, person)
}

func (a *realAccess) MemberKind(ctx context.Context, person string) (string, error) {
	return a.client().MemberKind(ctx, person)
}

func (a *realAccess) States(ctx context.Context, projects []string) (map[string]projectaccess.Answer, error) {
	return a.client().States(ctx, projects)
}

// newRealAccessRig is newRestrictedTokenRig with the broker asking the real Dispatch, and carol, a
// full member, owning OTH, a public project whose list does not name erin. It answers the broker's
// access too, for a row to make it forget what it holds.
func newRealAccessRig(t *testing.T) (restrictedTokenRig, *realAccess) {
	t.Helper()
	issuer := newServiceTokenIssuer(t)
	var served http.Handler
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { served.ServeHTTP(w, r) }))
	t.Cleanup(front.Close)
	brokerAccount := accountTokenFile(t, issuer.Mint(t, issuer.signing, issuer.Claims(listedServiceAccount, serviceTokenAudience)))
	access := &realAccess{fresh: func() *projectaccess.Client {
		return projectaccess.New(front.URL, ciphertoken.NewAccountToken(brokerAccount), nil)
	}}
	access.forget()
	r := newRestrictedTokenRigWith(t, access, testServerOptions{oidc: issuer.verifier(t), accessAccounts: []string{listedServiceAccount}})
	served = r.dispatch
	if created := dispatchRequest(t, r.dispatch, http.MethodPost, "/api/v1/projects", map[string]string{"key": "OTH", "name": "Another project"}, accessCarol); created.Code != http.StatusCreated {
		t.Fatalf("carol creates OTH: %d %s", created.Code, created.Body.String())
	}
	return r, access
}

// TestARestrictedMembersTradeAsksTheRealDispatch: against the real Dispatch's membership answer,
// erin's machine trade naming ACC, the one project whose list names her, is the named scope listing
// exactly ACC, living at most exchange.RestrictedLifetime by its answer and by its introspected exp;
// one naming OTH (public, its list not naming her), NOPE (no such project), none, or ACC and OTH is
// refused. Control on the same rig: carol's trades naming none and OTH are the person scope.
//
// Control: the broker's projectAccess letting a named machine trade through unchecked, and erin's
// trades naming OTH and NOPE are answered 200, named.
func TestARestrictedMembersTradeAsksTheRealDispatch(t *testing.T) {
	r, _ := newRealAccessRig(t)
	erins, erinsDevice, _ := r.approvedLogin(t, accessErin, "erin-laptop")
	status, body := r.exchange(t, erins, erinsDevice.AccessToken, "erin-agent", "ACC")
	if status != http.StatusOK {
		t.Fatalf("erin's trade naming ACC against the real Dispatch = %d %v, want 200", status, body)
	}
	token, _ := body["access_token"].(string)
	answer := r.introspect(t, token)
	if !answer.Active || answer.ProjectScope != ciphertoken.ProjectScopeNamed || !slices.Equal(answer.Projects, []string{"ACC"}) || answer.Person != accessErin {
		t.Fatalf("erin's token introspects as %+v, want named [ACC] for erin", answer)
	}
	if expiresIn, _ := body["expires_in"].(float64); expiresIn <= 0 || time.Duration(expiresIn)*time.Second > exchange.RestrictedLifetime {
		t.Errorf("erin's token expires_in %v, want at most %s", body["expires_in"], exchange.RestrictedLifetime)
	}
	if lifetime := time.Until(time.Unix(answer.ExpiresAt, 0)); lifetime > exchange.RestrictedLifetime {
		t.Errorf("erin's token introspects live %s more, want at most %s", lifetime, exchange.RestrictedLifetime)
	}
	for i, asked := range []string{"OTH", "NOPE", "", "ACC OTH"} {
		status, body := r.exchange(t, erins, erinsDevice.AccessToken, "erin-agent-"+string(rune('a'+i)), asked)
		if status == http.StatusOK {
			t.Errorf("erin's trade naming %q against the real Dispatch = 200 %v, want it refused", asked, body)
		}
	}

	carols, carolsDevice, _ := r.approvedLogin(t, accessCarol, "carol-laptop")
	for i, asked := range []string{"", "OTH"} {
		status, body := r.exchange(t, carols, carolsDevice.AccessToken, "carol-agent-"+string(rune('a'+i)), asked)
		if status != http.StatusOK {
			t.Fatalf("carol's trade naming %q = %d %v, want 200", asked, status, body)
		}
		token, _ := body["access_token"].(string)
		if answer := r.introspect(t, token); !answer.Active || answer.ProjectScope != ciphertoken.ProjectScopePerson {
			t.Errorf("carol's token naming %q introspects as %+v, want the person scope", asked, answer)
		}
	}
}

// TestARemovalEndsARestrictedMembersTradeWithNoRevoke: with no pass of the broker revocation loop,
// once alice takes erin off ACC's list and the broker's cache of the real Dispatch's answer has run
// out (realAccess.forget), erin's machine trades naming ACC, and naming no project, are refused: the
// 325-second bound on a removal rests on that, not on the revoke. Control on the same rig: alice puts
// erin back, and once the cache has run out again the same device token trades for ACC.
//
// Control: the broker's projectAccess letting a named machine trade through unchecked, and erin's
// trade naming ACC after her removal is answered 200, its token named and active, named.
func TestARemovalEndsARestrictedMembersTradeWithNoRevoke(t *testing.T) {
	r, access := newRealAccessRig(t)
	erins, erinsDevice, _ := r.approvedLogin(t, accessErin, "erin-laptop")
	_ = r.sessionToken(t, erins, erinsDevice, "erin-agent", "ACC")
	if removed := dispatchRequest(t, r.dispatch, http.MethodDelete, "/api/v1/projects/ACC/access/people/"+accessErin, nil, "alice"); removed.Code != http.StatusOK {
		t.Fatalf("alice takes erin off ACC's list: %d %s", removed.Code, removed.Body.String())
	}
	access.forget()
	for i, asked := range []string{"ACC", ""} {
		status, body := r.exchange(t, erins, erinsDevice.AccessToken, "erin-after-"+string(rune('a'+i)), asked)
		if status == http.StatusOK {
			token, _ := body["access_token"].(string)
			t.Errorf("erin's trade naming %q after her removal and the cache's end, with no revoke = 200, introspecting %+v; want it refused", asked, r.introspect(t, token))
		}
	}
	if listed := dispatchRequest(t, r.dispatch, http.MethodPut, "/api/v1/projects/ACC/access/people/"+accessErin, nil, "alice"); listed.Code != http.StatusOK {
		t.Fatalf("alice puts erin back on ACC's list: %d %s", listed.Code, listed.Body.String())
	}
	access.forget()
	if status, body := r.exchange(t, erins, erinsDevice.AccessToken, "erin-back", "ACC"); status != http.StatusOK {
		t.Errorf("control: erin's trade naming ACC once she is back on the list = %d %v, want 200", status, body)
	}
}
