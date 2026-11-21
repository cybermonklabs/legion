// issuers_test.go drives the routes that check a ServiceAccount token on a broker that trusts two
// named issuers, on the same real broker api_test.go mounts: a token of an issuer the broker cannot
// read is answered 503 OIDC_ISSUER_UNAVAILABLE on every one of them, and a listed account is taken
// only from the issuers its entry names.
package api_test

import (
	"crypto/ecdsa"
	"net/http"
	"net/url"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/cipher/api"
	"github.com/sjawhar/envoy/internal/cipher/enroll"
	"github.com/sjawhar/envoy/internal/oidc"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
	"github.com/sjawhar/envoy/internal/ratelimit"
)

// issuerUnavailableMessage is the sentence a 503 OIDC_ISSUER_UNAVAILABLE carries, Dispatch's own
// for the same fault.
const issuerUnavailableMessage = "the service-account token issuer cannot be reached; try again in a minute"

// withNamedIssuers names the test server's issuers as BROKER_K8S_OIDC_ISSUER's
// first=<URL>,second=<URL> does: first is the server's own issuer, which mints every token the
// server's helpers mint, and second is the given one. The service logins' entries and the listed
// caller's entry each name the issuers names holds.
func withNamedIssuers(second *oidctest.Issuer, names []string) func(*api.Deps) {
	return func(d *api.Deps) {
		issuers := []oidc.NamedIssuer{{Name: "first", URL: d.Enroll.Issuers[0].URL}, {Name: "second", URL: second.URL()}}
		verifier := enroll.K8sPodVerifier{Issuers: oidc.NewIssuerSet(issuers, podAudience, 0)}
		d.Enroll.Pod, d.Enroll.Issuers, d.AccountTokens = verifier, issuers, verifier
		for account, login := range d.Enroll.ServiceLogins {
			login.Issuers = names
			d.Enroll.ServiceLogins[account] = login
		}
		for account, caller := range d.APIAccounts {
			caller.Issuers = names
			d.APIAccounts[account] = caller
		}
	}
}

// issuedToken is account's projected token as issuer signs it with key, issued a few seconds ago
// and living an hour, bound to podUID ("" for a token bound to no pod).
func issuedToken(t *testing.T, issuer *oidctest.Issuer, key *oidctest.Key, account, podUID string) string {
	t.Helper()
	claims := issuer.Claims(account, podAudience)
	claims["iat"] = time.Now().Add(-5 * time.Second).Unix()
	if podUID != "" {
		claims["kubernetes.io"] = map[string]any{"pod": map[string]any{"uid": podUID}}
	}
	return issuer.Mint(t, key, claims)
}

// pendingAs is GET /v1/pending, a route of the listed caller, presenting bearer.
func (ts *testServer) pendingAs(t *testing.T, bearer string) (int, []byte) {
	t.Helper()
	return ts.req(t, http.MethodGet, "/v1/pending?approver="+url.QueryEscape(testApprover), map[string]string{"Authorization": "Bearer " + bearer}, nil)
}

// enrolledPod enrolls the pod podUID of workerAccount under credentialID with a key of its own,
// proving it with token, and answers the enrollment and the key that renews it.
func (ts *testServer) enrolledPod(t *testing.T, key *ecdsa.PrivateKey, credentialID, podUID, token string) (string, *ecdsa.PrivateKey) {
	t.Helper()
	podKey := newSigningKey(t)
	status, body := ts.launcher(t, key, credentialID, http.MethodPost, "/v1/enrollments", map[string]any{
		"kind": "pod", "runtime_id": podUID, "thumbprint": thumbprintOf(t, podKey), "pod_token": token,
	})
	if status != http.StatusCreated {
		t.Fatalf("enroll the pod %s = %d %s, want 201", podUID, status, body)
	}
	return decode[wireEnrolled](t, body).EnrollmentID, podKey
}

// TestATokenOfAnIssuerTheBrokerCannotReadIsAnswered503OnEveryRoute: while the broker cannot read
// the second issuer — its discovery document, or its key set — a token of that issuer is answered
// 503 OIDC_ISSUER_UNAVAILABLE, with Dispatch's sentence, at enrollment, renewal, the token login,
// the pod-session registration's body token, a route of the listed caller and the service route,
// whose caller presents its own login token. Every entry names both issuers, so nothing but the
// read can refuse it, and the first issuer's tokens are served throughout.
func TestATokenOfAnIssuerTheBrokerCannotReadIsAnswered503OnEveryRoute(t *testing.T) {
	both := []string{"first", "second"}
	for _, fault := range []struct {
		name  string
		apply func(*oidctest.Issuer)
	}{
		{"discovery document", func(issuer *oidctest.Issuer) { issuer.FailDiscovery(http.StatusServiceUnavailable) }},
		{"key set", func(issuer *oidctest.Issuer) { issuer.FailKeys(http.StatusServiceUnavailable) }},
	} {
		t.Run("its "+fault.name+" cannot be read", func(t *testing.T) {
			second := oidctest.New(t)
			secondKey := second.PublishKey(t, "second-key")
			fault.apply(second)
			ts := newTestServerWith(t, withNamedIssuers(second, both))
			credentialID, key := ts.mintServiceLauncherCredential(t)
			podUID := uuid.NewString()
			enrollmentID, podKey := ts.enrolledPod(t, key, credentialID, podUID, ts.podToken(t, workerAccount, podUID))
			registrar := ts.accountToken(t, daemonAccount, "", time.Hour)

			for _, route := range []struct {
				name string
				call func() (int, []byte)
			}{
				{"enrollment", func() (int, []byte) {
					uid := uuid.NewString()
					return ts.launcher(t, key, credentialID, http.MethodPost, "/v1/enrollments", map[string]any{
						"kind": "pod", "runtime_id": uid, "thumbprint": thumbprintOf(t, newSigningKey(t)),
						"pod_token": issuedToken(t, second, secondKey, workerAccount, uid),
					})
				}},
				{"renewal", func() (int, []byte) {
					return ts.session(t, podKey, enrollmentID, http.MethodPost, "/v1/enrollments/"+enrollmentID+"/renew",
						map[string]any{"pod_token": issuedToken(t, second, secondKey, workerAccount, podUID)})
				}},
				{"token login", func() (int, []byte) {
					return ts.tokenLogin(t, issuedToken(t, second, secondKey, daemonAccount, ""), ts.daemonRequest(t, newSigningKey(t)))
				}},
				{"pod-session registration", func() (int, []byte) {
					return ts.registerPodSession(t, registrar, issuedToken(t, second, secondKey, workerAccount, uuid.NewString()), "example-session")
				}},
				{"listed caller's route", func() (int, []byte) {
					return ts.pendingAs(t, issuedToken(t, second, secondKey, dispatchAccount, ""))
				}},
				{"service route", func() (int, []byte) {
					return ts.registerPodSession(t, issuedToken(t, second, secondKey, daemonAccount, ""), ts.podToken(t, workerAccount, uuid.NewString()), "example-session")
				}},
			} {
				t.Run(route.name, func(t *testing.T) {
					status, body := route.call()
					if refusal := decode[wireError](t, body); status != http.StatusServiceUnavailable || refusal.Code != "OIDC_ISSUER_UNAVAILABLE" || refusal.Error != issuerUnavailableMessage {
						t.Fatalf("a token of the second issuer = %d %s, want 503 OIDC_ISSUER_UNAVAILABLE %q", status, body, issuerUnavailableMessage)
					}
				})
			}
			t.Run("the first issuer's tokens are served", func(t *testing.T) {
				if status, body := ts.pendingAs(t, ts.uiToken); status != http.StatusOK {
					t.Fatalf("the listed caller's own token of the first issuer = %d %s, want 200", status, body)
				}
				if status, body := ts.session(t, podKey, enrollmentID, http.MethodPost, "/v1/enrollments/"+enrollmentID+"/renew",
					map[string]any{"pod_token": ts.podToken(t, workerAccount, podUID)}); status != http.StatusOK {
					t.Fatalf("the pod's renewal with its own token of the first issuer = %d %s, want 200", status, body)
				}
			})
		})
	}
}

// TestATokenLoginWhoseIssuerCannotBeReadSpendsNoAddressBucket: a token login answered 503
// OIDC_ISSUER_UNAVAILABLE gives back its address's request, as a login the broker could not
// complete does, since the broker decided nothing about the token and the daemon retries it every
// few seconds through the outage. Four such logins from one address with a bucket of two are each
// answered 503, none 429, and the bucket is still whole after them: the first issuer's daemon logs
// in twice from that address and is refused the third time.
func TestATokenLoginWhoseIssuerCannotBeReadSpendsNoAddressBucket(t *testing.T) {
	second := oidctest.New(t)
	secondKey := second.PublishKey(t, "second-key")
	second.FailDiscovery(http.StatusServiceUnavailable)
	both := []string{"first", "second"}
	ts := newTestServerWith(t, func(d *api.Deps) {
		withNamedIssuers(second, both)(d)
		d.LauncherLimits = &api.LauncherLimits{
			PerAddress: ratelimit.Limit{Every: time.Hour, Burst: 2},
			PerLogin:   ratelimit.Limit{Every: time.Hour, Burst: 100},
		}
	})
	for i := range 4 {
		status, body := ts.tokenLogin(t, issuedToken(t, second, secondKey, daemonAccount, ""), ts.daemonRequest(t, newSigningKey(t)))
		if status != http.StatusServiceUnavailable || decode[wireError](t, body).Code != "OIDC_ISSUER_UNAVAILABLE" {
			t.Fatalf("the second issuer's login %d = %d %s, want 503 OIDC_ISSUER_UNAVAILABLE: it spends none of its address's bucket of 2", i, status, body)
		}
	}
	for i, want := range []int{http.StatusOK, http.StatusOK, http.StatusTooManyRequests} {
		if status, body := ts.tokenLogin(t, ts.accountToken(t, daemonAccount, "", time.Hour), ts.daemonRequest(t, newSigningKey(t))); status != want {
			t.Fatalf("the first issuer's login %d after them = %d %s, want %d: the address's bucket of 2 is whole, and still a bucket", i, status, body, want)
		}
	}
}

// TestAListedAccountIsTakenOnlyFromTheIssuersItsEntryNames: an account's entry that names the
// first issuer alone, as an entry naming none is given at load, takes its token of the first
// issuer and answers its token of the second 403 ACCOUNT_NOT_LISTED on the listed caller's route,
// at the token login and on the service route, as it answers an account no entry lists; an entry
// that names both takes both. A login of the second issuer then holds a credential only while its
// entry names that issuer.
func TestAListedAccountIsTakenOnlyFromTheIssuersItsEntryNames(t *testing.T) {
	for _, c := range []struct {
		name     string
		names    []string
		takesTwo bool
	}{
		{"an entry naming the first issuer alone", []string{"first"}, false},
		{"an entry naming both issuers", []string{"first", "second"}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			second := oidctest.New(t)
			secondKey := second.PublishKey(t, "second-key")
			ts := newTestServerWith(t, withNamedIssuers(second, c.names))
			want := func(listed bool) (int, string) {
				if listed {
					return http.StatusOK, ""
				}
				return http.StatusForbidden, "ACCOUNT_NOT_LISTED"
			}
			for _, issuer := range []struct {
				name   string
				token  func(account string) string
				listed bool
			}{
				{"first", func(account string) string { return ts.accountToken(t, account, "", time.Hour) }, true},
				{"second", func(account string) string { return issuedToken(t, second, secondKey, account, "") }, c.takesTwo},
			} {
				t.Run("the listed caller's token of the "+issuer.name+" issuer", func(t *testing.T) {
					status, body := ts.pendingAs(t, issuer.token(dispatchAccount))
					if wantStatus, wantCode := want(issuer.listed); status != wantStatus || wantCode != "" && decode[wireError](t, body).Code != wantCode {
						t.Fatalf("GET /v1/pending = %d %s, want %d %s", status, body, wantStatus, wantCode)
					}
				})
				t.Run("the daemon's token login with a token of the "+issuer.name+" issuer", func(t *testing.T) {
					status, body := ts.tokenLogin(t, issuer.token(daemonAccount), ts.daemonRequest(t, newSigningKey(t)))
					if wantStatus, wantCode := want(issuer.listed); status != wantStatus || wantCode != "" && decode[wireError](t, body).Code != wantCode {
						t.Fatalf("token login = %d %s, want %d %s", status, body, wantStatus, wantCode)
					}
				})
				t.Run("the daemon on the service route with a token of the "+issuer.name+" issuer", func(t *testing.T) {
					// A caller the route takes reaches the registration, which this server refuses
					// 403 POD_ACCOUNT_NOT_LISTED, since no exchange entry lists the pod's account.
					// A caller the route refuses is 403 ACCOUNT_NOT_LISTED.
					wantCode := "ACCOUNT_NOT_LISTED"
					if issuer.listed {
						wantCode = "POD_ACCOUNT_NOT_LISTED"
					}
					status, body := ts.registerPodSession(t, issuer.token(daemonAccount), ts.podToken(t, workerAccount, uuid.NewString()), "example-session")
					if status != http.StatusForbidden || decode[wireError](t, body).Code != wantCode {
						t.Fatalf("POST /v1/oauth/exchange/pod-sessions = %d %s, want 403 %s", status, body, wantCode)
					}
				})
				t.Run("an account no entry lists, with a token of the "+issuer.name+" issuer", func(t *testing.T) {
					if status, body := ts.pendingAs(t, issuer.token(unlistedAccount)); status != http.StatusForbidden || decode[wireError](t, body).Code != "ACCOUNT_NOT_LISTED" {
						t.Fatalf("GET /v1/pending = %d %s, want 403 ACCOUNT_NOT_LISTED", status, body)
					}
				})
			}
		})
	}

	t.Run("a credential of the second issuer's login stops once its entry stops naming that issuer", func(t *testing.T) {
		second := oidctest.New(t)
		secondKey := second.PublishKey(t, "second-key")
		both := []string{"first", "second"}
		ts := newTestServerWith(t, withNamedIssuers(second, both))
		key := newSigningKey(t)
		status, body := ts.tokenLogin(t, issuedToken(t, second, secondKey, daemonAccount, ""), ts.daemonRequest(t, key))
		if status != http.StatusOK {
			t.Fatalf("the second issuer's login = %d %s, want 200", status, body)
		}
		credentialID := decode[wireTokenLogin](t, body).CredentialID
		if got := ts.column(t, `select coalesce(issuer, '') from launcher_credentials where id=$1`, credentialID); !slices.Equal(got, []string{second.URL()}) {
			t.Fatalf("the credential records the issuer %v, want %s", got, second.URL())
		}
		if got := ts.column(t, `select detail->>'issuer' from audit where kind='launcher_credential.token_login'`); !slices.Equal(got, []string{second.URL()}) {
			t.Fatalf("the token_login audit rows name the issuers %v, want %s", got, second.URL())
		}
		firstID, firstKey := ts.mintServiceLauncherCredentialAs(t, otherDaemonAccount)
		if status, body := ts.enrollPodWith(t, key, credentialID); status != http.StatusCreated {
			t.Fatalf("enroll with the second issuer's login while its entry names it = %d %s, want 201", status, body)
		}
		ts.ServiceLogins[daemonAccount] = enroll.ServiceLogin{Service: "legion-daemon", Issuers: []string{"first"}}
		ts.ServiceLogins[otherDaemonAccount] = enroll.ServiceLogin{Service: "legion-daemon", Issuers: []string{"first"}}
		if status, body := ts.enrollPodWith(t, key, credentialID); status != http.StatusUnauthorized || decode[wireError](t, body).Code != "LAUNCHER_INVALID" {
			t.Fatalf("enroll with the second issuer's login once its entry names the first alone = %d %s, want 401 LAUNCHER_INVALID", status, body)
		}
		if status, body := ts.enrollPodWith(t, firstKey, firstID); status != http.StatusCreated {
			t.Fatalf("enroll with the first issuer's login under the same entries = %d %s, want 201: only the credential of the issuer no longer named stops", status, body)
		}
	})
}
