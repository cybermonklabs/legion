// unreadable_key_set_test.go drives every route that checks a token against an issuer's key set
// while that set cannot be read, on the real broker api_test.go mounts and the hold rig
// nats_hold_test.go mounts: a check that reached no verdict is answered 503 with Retry-After,
// never a refusal of the token, and a token signed by a key the issuer never published is still
// refused as it is today.
package api_test

import (
	"crypto/ecdsa"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/cipher/api"
	"github.com/sjawhar/envoy/internal/cipher/ciphertest"
	"github.com/sjawhar/envoy/internal/cipher/oauth"
	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
	"github.com/sjawhar/envoy/internal/cipher/proof"
	"github.com/sjawhar/envoy/internal/cipherclient"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// unreadableRetryAfter is the Retry-After an answer through an unreadable key set carries at the
// default spacing of a key set's failed reads (oidc.DefaultKeysFailureSpacing, 5 s), which every
// rig here keeps.
const unreadableRetryAfter = "5"

// TestEveryRouteThatChecksATokenAnswersAnUnreadableKeySet503: every row of routes() whose caller
// presents an identity token the broker checks against an issuer's key set (api.KeySetRoutes), so
// a route added later to one of those classes is a row here too. "unreadable": the issuer's key
// set fails before the first check, so the check reaches no verdict, and the route answers 503
// with Retry-After 5 and its own body: ISSUER_UNAVAILABLE on a JSON route, issuer_unavailable on
// the NATS writer hold's. "refused": the set reads and the token is signed by a key the issuer
// never published, which the route refuses as it does today, 401 ACCOUNT_TOKEN_INVALID or 403
// not_operator, with no Retry-After. The account, service and edge-listener rows run on
// newTestServer with a token of its Kubernetes issuer, the operator rows on newHoldRig with the
// operator's platform token.
func TestEveryRouteThatChecksATokenAnswersAnUnreadableKeySet503(t *testing.T) {
	unreadable, refused := newTestServer(t), newTestServer(t)
	unreadable.podIssuer.FailKeys(http.StatusServiceUnavailable)
	unreadableHold, refusedHold := newHoldRig(t), newHoldRig(t)
	unreadableHold.issuer.FailKeys(http.StatusServiceUnavailable)
	forger := neverPublished(t)

	routes := api.KeySetRoutes()
	if len(routes) == 0 {
		t.Fatal("api.KeySetRoutes names no route: routes() has no row of accountAuth, serviceAuth, edgeListenerAuth or operatorAuth")
	}
	for _, route := range routes {
		t.Run(route.Method+" "+route.Pattern, func(t *testing.T) {
			path := concretePath(route.Pattern)
			what := route.Method + " " + path
			switch route.Class {
			case api.KeySetOperator:
				t.Run("unreadable", func(t *testing.T) {
					answeredHoldIssuerUnavailable(t, what, bearerCall(t, unreadableHold.url, route.Method, path, unreadableHold.token(holdOperatorAccount)))
				})
				t.Run("refused", func(t *testing.T) {
					claims := refusedHold.issuer.Claims(holdOperatorAccount, holdNATSAudience)
					now := time.Now()
					claims["iat"], claims["exp"] = now.Add(-5*time.Second).Unix(), now.Add(10*time.Minute).Unix()
					answeredHoldRefusal(t, what+" with the operator's token signed by a key the issuer never published",
						bearerCall(t, refusedHold.url, route.Method, path, refusedHold.issuer.Mint(t, forger, claims)),
						http.StatusForbidden, "not_operator")
				})
			case api.KeySetAccount, api.KeySetService, api.KeySetEdgeListener:
				// A service's route takes a token of an account BROKER_SERVICE_LOGINS lists; every
				// other class reaches its own list only once the token has verified.
				account := dispatchAccount
				if route.Class == api.KeySetService {
					account = daemonAccount
				}
				t.Run("unreadable", func(t *testing.T) {
					answeredIssuerUnavailable(t, what, bearerCall(t, unreadable.URL, route.Method, path, unreadable.accountToken(t, account, "", time.Hour)))
				})
				t.Run("refused", func(t *testing.T) {
					answeredRefusal(t, what+" with "+account+"'s token signed by a key the issuer never published",
						bearerCall(t, refused.URL, route.Method, path, refused.tokenSignedBy(t, forger, account, "", podAudience)),
						http.StatusUnauthorized, "ACCOUNT_TOKEN_INVALID")
				})
			default:
				t.Fatalf("api.KeySetRoutes names the class %q, which this test has no rig for: give it one", route.Class)
			}
		})
	}
}

// bodyTokenRow is one route of TestATokenInTheBodyThroughAnUnreadableKeySetIsAnswered503, each
// of its two calls on a rig of its own.
type bodyTokenRow struct {
	name string
	// unreadable sends the route its request with a body token the issuer signed, while the key
	// set that token's check must read cannot be read.
	unreadable func(t *testing.T) answered
	// refused sends it with the body's token signed by a key the issuer never published, the set
	// readable.
	refused func(t *testing.T) answered
	// oauthRoute is the token endpoint, which answers in OAuth's error object.
	oauthRoute bool
	// status and code are the route's refusal of today for refused's token.
	status int
	code   string
}

// TestATokenInTheBodyThroughAnUnreadableKeySetIsAnswered503: the routes that check a token in the
// request body, which no authentication class of routes() marks, each with a row "unreadable",
// where the check meets a key set that cannot be read and the route answers 503 with Retry-After 5
// (ISSUER_UNAVAILABLE, or on the token endpoint OAuth's temporarily_unavailable carrying it), and a
// row "refused", where a token signed by a key the issuer never published is refused as today.
//
// The token login's and the token exchange's sets fail from the start. The pod enrolment, its
// renewal and the pod-session registration first read the set through a check of their own
// setup (the daemon's token login, the pod's enrolment, the registrar's bearer), so their body's
// token is signed by a key the issuer starts publishing after that read, as a rotation does,
// while the set fails: the cached set answers every other token of the request, and the body's
// is the one whose check must read the set and cannot (oidc.Keys.Key). With the set failing
// alone, the registration's bearer would fail first, and the row would prove only the service
// class. The token exchange's actor row runs on a provider with a Google issuer
// (ciphertest.WithGoogleIssuer) whose set fails from the start: the library checks the actor
// token before the subject, so the actor's check is the one that answers.
func TestATokenInTheBodyThroughAnUnreadableKeySetIsAnswered503(t *testing.T) {
	for _, row := range []bodyTokenRow{
		{
			name: "service token login",
			unreadable: func(t *testing.T) answered {
				ts := newTestServer(t)
				ts.podIssuer.FailKeys(http.StatusServiceUnavailable)
				return ts.tokenLoginCall(t, ts.accountToken(t, daemonAccount, uuid.NewString(), time.Hour))
			},
			refused: func(t *testing.T) answered {
				ts := newTestServer(t)
				return ts.tokenLoginCall(t, ts.tokenSignedBy(t, neverPublished(t), daemonAccount, uuid.NewString(), podAudience))
			},
			status: http.StatusUnauthorized, code: "TOKEN_INVALID",
		},
		{
			name: "pod enrolment",
			unreadable: func(t *testing.T) answered {
				ts := newTestServer(t)
				credentialID, launcherKey := ts.mintServiceLauncherCredential(t)
				return ts.enrolPodCall(t, launcherKey, credentialID, ts.rotatedWhileUnreadable(t))
			},
			refused: func(t *testing.T) answered {
				ts := newTestServer(t)
				credentialID, launcherKey := ts.mintServiceLauncherCredential(t)
				return ts.enrolPodCall(t, launcherKey, credentialID, neverPublished(t))
			},
			status: http.StatusForbidden, code: "POD_IDENTITY_MISMATCH",
		},
		{
			name: "renewal with a pod token",
			unreadable: func(t *testing.T) answered {
				ts := newTestServer(t)
				podUID, pod := ts.enrolDaemonPod(t)
				return ts.renewCall(t, pod, podUID, ts.rotatedWhileUnreadable(t))
			},
			refused: func(t *testing.T) answered {
				ts := newTestServer(t)
				podUID, pod := ts.enrolDaemonPod(t)
				return ts.renewCall(t, pod, podUID, neverPublished(t))
			},
			status: http.StatusForbidden, code: "POD_IDENTITY_MISMATCH",
		},
		{
			name: "pod token in the body of a pod-session registration",
			unreadable: func(t *testing.T) answered {
				ts := newTestServer(t)
				registrar := ts.cachedRegistrar(t)
				return ts.registrationCall(t, registrar, ts.tokenSignedBy(t, ts.rotatedWhileUnreadable(t), ciphertest.ExchangeRolesAccount, "pod-uid-rotated", podAudience))
			},
			refused: func(t *testing.T) answered {
				ts := newTestServer(t)
				registrar := ts.cachedRegistrar(t)
				return ts.registrationCall(t, registrar, ts.tokenSignedBy(t, neverPublished(t), ciphertest.ExchangeRolesAccount, "pod-uid-forged", podAudience))
			},
			status: http.StatusUnauthorized, code: "POD_TOKEN_INVALID",
		},
		{
			name: "token exchange subject token",
			unreadable: func(t *testing.T) answered {
				ts := newTestServer(t)
				ts.podIssuer.FailKeys(http.StatusServiceUnavailable)
				return ts.exchangeCall(t, ts.audienceToken(t, ciphertest.ExchangeAccount, "pod-uid-exchange", ciphertest.ExchangeAudience, time.Hour))
			},
			refused: func(t *testing.T) answered {
				ts := newTestServer(t)
				return ts.exchangeCall(t, ts.tokenSignedBy(t, neverPublished(t), ciphertest.ExchangeAccount, "pod-uid-exchange", ciphertest.ExchangeAudience))
			},
			oauthRoute: true, status: http.StatusBadRequest, code: "invalid_request",
		},
		{
			name: "token exchange actor token",
			unreadable: func(t *testing.T) answered {
				google := newGoogleRig(t)
				google.issuer.FailKeys(http.StatusServiceUnavailable)
				return google.signInCall(t, google.key)
			},
			refused: func(t *testing.T) answered {
				google := newGoogleRig(t)
				return google.signInCall(t, neverPublished(t))
			},
			oauthRoute: true, status: http.StatusBadRequest, code: "invalid_request",
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Run("unreadable", func(t *testing.T) {
				got := row.unreadable(t)
				if row.oauthRoute {
					answeredTemporarilyUnavailable(t, row.name, got)
					return
				}
				answeredIssuerUnavailable(t, row.name, got)
			})
			t.Run("refused", func(t *testing.T) {
				got := row.refused(t)
				what := row.name + " signed by a key the issuer never published"
				if row.oauthRoute {
					answeredOAuthRefusal(t, what, got, row.status, row.code)
					return
				}
				answeredRefusal(t, what, got, row.status, row.code)
			})
		})
	}
}

// TestTheRetryAfterOfAnUnreadableKeySetIsItsFailureSpacingRoundedUp: the wait an answer through an
// unreadable key set names is that key set's own FailureSpacing, how long it answers its failure
// before it reads the issuer again, in whole seconds rounded up and at least 1: 2.5 s is
// Retry-After 3 and 400 ms is 1, on POST /v1/oauth/introspect with the listed caller's token.
// "refused": a token signed by a key the issuer never published, the set readable, is 401
// ACCOUNT_TOKEN_INVALID with no Retry-After.
func TestTheRetryAfterOfAnUnreadableKeySetIsItsFailureSpacingRoundedUp(t *testing.T) {
	for _, row := range []struct {
		name       string
		spacing    time.Duration
		retryAfter string
	}{
		{"2.5 s", 2500 * time.Millisecond, "3"},
		{"400 ms", 400 * time.Millisecond, "1"},
	} {
		t.Run(row.name, func(t *testing.T) {
			ts := newTestServer(t)
			ts.keys.FailureSpacing = row.spacing
			ts.podIssuer.FailKeys(http.StatusServiceUnavailable)
			answeredIssuerUnavailableAfter(t, "introspection with the listed caller's token, the key set's FailureSpacing "+row.spacing.String(),
				introspectCall(t, ts.URL, ts.uiToken), row.retryAfter)
		})
	}
	t.Run("refused", func(t *testing.T) {
		ts := newTestServer(t)
		ts.keys.FailureSpacing = 2500 * time.Millisecond
		answeredRefusal(t, "introspection with the listed caller's token signed by a key the issuer never published",
			introspectCall(t, ts.URL, ts.tokenSignedBy(t, neverPublished(t), dispatchAccount, "", podAudience)),
			http.StatusUnauthorized, "ACCOUNT_TOKEN_INVALID")
	})
}

// TestAGoogleAccountTokenThroughAnUnreadableKeySetIsAnswered503: identityAccount's Google path, a
// caller presenting a machine's Google identity token, on POST /v1/oauth/introspect, where
// BROKER_API_ACCOUNTS lists the account (newGoogleCallerServer), and on GET
// /v1/project-repositories, where an edge-listener entry names it (googleEdgeServerOf).
// "unreadable": the Google issuer's key set fails before the first check, and the route answers
// 503 ISSUER_UNAVAILABLE with Retry-After 5. "refused": the token is signed by a key the issuer
// never published while the set reads, and the route refuses it 401 ACCOUNT_TOKEN_INVALID, as
// today.
func TestAGoogleAccountTokenThroughAnUnreadableKeySetIsAnswered503(t *testing.T) {
	t.Run("POST /v1/oauth/introspect", func(t *testing.T) {
		t.Run("unreadable", func(t *testing.T) {
			g := newGoogleCallerServer(t)
			g.issuer.FailKeys(http.StatusServiceUnavailable)
			answeredIssuerUnavailable(t, "introspection with the listed Google caller's token",
				introspectCall(t, g.URL, googleIdentityToken(t, g.issuer, g.key, listenerGoogleAccount)))
		})
		t.Run("refused", func(t *testing.T) {
			g := newGoogleCallerServer(t)
			answeredRefusal(t, "introspection with the listed Google caller's token signed by a key the issuer never published",
				introspectCall(t, g.URL, googleIdentityToken(t, g.issuer, neverPublished(t), listenerGoogleAccount)),
				http.StatusUnauthorized, "ACCOUNT_TOKEN_INVALID")
		})
	})
	t.Run("GET "+repositoriesPath, func(t *testing.T) {
		const services = "edge-listener/edge-g/PRIV=google:" + edgeGoogleAccount
		path := repositoriesPath + "?project=PRIV"
		t.Run("unreadable", func(t *testing.T) {
			google := oidctest.New(t)
			key := google.PublishKey(t, "google-key")
			ts, _ := googleEdgeServerOf(t, google, services)
			google.FailKeys(http.StatusServiceUnavailable)
			answeredIssuerUnavailable(t, "the Google edge listener's repositories",
				bearerCall(t, ts.URL, http.MethodGet, path, googleIdentityToken(t, google, key, edgeGoogleAccount)))
		})
		t.Run("refused", func(t *testing.T) {
			google := oidctest.New(t)
			google.PublishKey(t, "google-key")
			ts, _ := googleEdgeServerOf(t, google, services)
			answeredRefusal(t, "the Google edge listener's repositories with its token signed by a key the issuer never published",
				bearerCall(t, ts.URL, http.MethodGet, path, googleIdentityToken(t, google, neverPublished(t), edgeGoogleAccount)),
				http.StatusUnauthorized, "ACCOUNT_TOKEN_INVALID")
		})
	})
}

// TestTokenLoginsThroughAnUnreadableKeySetSpendNoAddressRequest: a token login whose token check
// reached no verdict, because the issuer's key set could not be read, gives back the address
// request it spent before its token was checked. "unreadable": eleven logins from one address,
// one more than its bucket holds (api.DefaultLauncherLimits), are each answered 503
// ISSUER_UNAVAILABLE and none 429; once the set answers again and its FailureSpacing has passed
// since its last failed read, the next login from that address is answered 200. The set's spacing
// is 300 ms here (Retry-After 1), so the row waits no 5 s for it. "refused": a token signed by a
// key the issuer never published, the set readable, is 401 TOKEN_INVALID and keeps its address
// request as it did: once the bucket's ten are spent, the eleventh such login is 429.
func TestTokenLoginsThroughAnUnreadableKeySetSpendNoAddressRequest(t *testing.T) {
	burst := api.DefaultLauncherLimits.PerAddress.Burst
	t.Run("unreadable", func(t *testing.T) {
		const spacing = 300 * time.Millisecond
		ts := newTestServer(t)
		ts.keys.FailureSpacing = spacing
		ts.podIssuer.FailKeys(http.StatusServiceUnavailable)
		var answeredAt time.Time
		for i := range burst + 1 {
			got := ts.tokenLoginCall(t, ts.accountToken(t, daemonAccount, "", time.Hour))
			answeredAt = time.Now()
			answeredIssuerUnavailableAfter(t, fmt.Sprintf("the daemon's token login %d of %d from one address", i+1, burst+1), got, "1")
		}
		ts.podIssuer.FailKeys(0)
		testwait.Eventually(t, "the key set's FailureSpacing to pass since the last login's answer", func() bool {
			return time.Since(answeredAt) >= spacing
		})
		if got := ts.tokenLoginCall(t, ts.accountToken(t, daemonAccount, "", time.Hour)); got.status != http.StatusOK {
			t.Fatalf("the daemon's token login once the key set answers again = %d %s, want 200: the %d logins the outage answered 503 spent none of the address's bucket of %d",
				got.status, got.body, burst+1, burst)
		}
	})
	t.Run("refused", func(t *testing.T) {
		ts := newTestServer(t)
		forger := neverPublished(t)
		for i := range burst {
			answeredRefusal(t, fmt.Sprintf("the daemon's token login %d of %d signed by a key the issuer never published", i+1, burst),
				ts.tokenLoginCall(t, ts.tokenSignedBy(t, forger, daemonAccount, "", podAudience)), http.StatusUnauthorized, "TOKEN_INVALID")
		}
		got := ts.tokenLoginCall(t, ts.tokenSignedBy(t, forger, daemonAccount, "", podAudience))
		var body wireError
		_ = json.Unmarshal(got.body, &body)
		if got.status != http.StatusTooManyRequests || body.Code != "RATE_LIMITED" {
			t.Fatalf("the daemon's token login %d signed by a key the issuer never published = %d %s, want 429 RATE_LIMITED: each refused token keeps the address request it spent",
				burst+1, got.status, got.body)
		}
	})
}

// pathParameter is a pattern's wildcard segment, such as {record}.
var pathParameter = regexp.MustCompile(`\{[^}]*\}`)

// concretePath is pattern with each wildcard segment filled in with an id no row names: the route
// authenticates its caller before it reads anything of the path.
func concretePath(pattern string) string {
	return pathParameter.ReplaceAllStringFunc(pattern, func(string) string { return uuid.NewString() })
}

// bearerCall sends method path to base with token as its bearer and reads the whole answer.
func bearerCall(t *testing.T, base, method, path, token string) answered {
	t.Helper()
	return send(t, http.DefaultClient, request(t, method, base+path, nil, map[string]string{"Authorization": "Bearer " + token}))
}

// neverPublished is a signing key no issuer publishes.
func neverPublished(t *testing.T) *oidctest.Key {
	t.Helper()
	return oidctest.NewKey(t, "never-published")
}

// tokenSignedBy is a projected ServiceAccount token of ts's issuer for account, podUID ("" for
// none) and audience, issued a minute ago and live for an hour (oidctest.Issuer.Claims), signed by
// key rather than by the key the issuer published at the server's start.
func (ts *testServer) tokenSignedBy(t *testing.T, key *oidctest.Key, account, podUID, audience string) string {
	t.Helper()
	claims := ts.podIssuer.Claims(account, audience)
	if podUID != "" {
		claims["kubernetes.io"] = map[string]any{"pod": map[string]any{"uid": podUID}}
	}
	return ts.podIssuer.Mint(t, key, claims)
}

// rotatedWhileUnreadable is a signing key ts's issuer starts publishing after the server has read
// its key set, as an issuer rotating its keys does, while the set fails from then on: a token it
// signs names a key id the cached set does not hold, so its check must read the set, and cannot,
// while every token of the key the set holds still verifies from the cache.
func (ts *testServer) rotatedWhileUnreadable(t *testing.T) *oidctest.Key {
	t.Helper()
	key := ts.podIssuer.PublishKey(t, "rotated-"+uuid.NewString())
	ts.podIssuer.FailKeys(http.StatusServiceUnavailable)
	return key
}

// tokenLoginCall posts token to POST /v1/launcher-credentials/token as the daemon's token login,
// with a request object a fresh key signed.
func (ts *testServer) tokenLoginCall(t *testing.T, token string) answered {
	t.Helper()
	return send(t, http.DefaultClient, request(t, http.MethodPost, ts.URL+"/v1/launcher-credentials/token",
		jsonBody(t, map[string]any{"token": token, "request": ts.daemonRequest(t, newSigningKey(t))}),
		map[string]string{"Content-Type": "application/json"}))
}

// launcherCall is method path with body signed by the launcher credential launcherID's key, as
// ts.launcher sends it, read whole.
func (ts *testServer) launcherCall(t *testing.T, key *ecdsa.PrivateKey, launcherID, method, path string, body any) answered {
	t.Helper()
	p, err := proof.SignLauncher(key, launcherID, method, ts.URL+path, time.Now())
	if err != nil {
		t.Fatalf("proof.SignLauncher: %v", err)
	}
	return send(t, http.DefaultClient, request(t, method, ts.URL+path, jsonBody(t, body),
		map[string]string{"Proof": p, "Content-Type": "application/json"}))
}

// enrolPodCall enrols a fresh pod of workerAccount under the launcher credential credentialID,
// its pod token signed by signer.
func (ts *testServer) enrolPodCall(t *testing.T, launcherKey *ecdsa.PrivateKey, credentialID string, signer *oidctest.Key) answered {
	t.Helper()
	podUID := uuid.NewString()
	return ts.launcherCall(t, launcherKey, credentialID, http.MethodPost, "/v1/enrollments", map[string]any{
		"kind": "pod", "runtime_id": podUID, "thumbprint": thumbprintOf(t, newSigningKey(t)),
		"pod_token": ts.tokenSignedBy(t, signer, workerAccount, podUID, podAudience),
	})
}

// enrolDaemonPod enrols a pod of workerAccount under the daemon's token login (enrolPodWith),
// answering the pod's uid and its enrollment: the login and the enrolment have both read ts's key
// set.
func (ts *testServer) enrolDaemonPod(t *testing.T) (podUID string, pod enrolled) {
	t.Helper()
	credentialID, launcherKey := ts.mintServiceLauncherCredential(t)
	podUID = uuid.NewString()
	return podUID, ts.enrolPodWith(t, launcherLogin{id: credentialID, key: launcherKey},
		podEnrolment{account: workerAccount, podUID: podUID, session: "implementer-renewing"})
}

// renewCall renews pod's enrollment, of the pod podUID of workerAccount, as the pod, with its pod
// token signed by signer.
func (ts *testServer) renewCall(t *testing.T, pod enrolled, podUID string, signer *oidctest.Key) answered {
	t.Helper()
	path := "/v1/enrollments/" + pod.id + "/renew"
	return send(t, http.DefaultClient, ts.sessionCall(t, pod.key, pod.id, http.MethodPost, path,
		map[string]any{"pod_token": ts.tokenSignedBy(t, signer, workerAccount, podUID, podAudience)}, nil))
}

// registrationSession is the session name a registration row registers its pod for.
const registrationSession = "implementer-g1"

// cachedRegistrar is daemonAccount's own token after a registration it made of a pod of
// ciphertest.ExchangeRolesAccount, which read ts's key set for both of its tokens.
func (ts *testServer) cachedRegistrar(t *testing.T) string {
	t.Helper()
	registrar := ts.accountToken(t, daemonAccount, "", time.Hour)
	if status, body := ts.registerPodSession(t, registrar, ts.rolesPodToken(t, "pod-uid-cached"), registrationSession); status != http.StatusOK {
		t.Fatalf("the registration that reads the key set = %d %s, want 200", status, body)
	}
	return registrar
}

// registrationCall posts a registration of podToken's pod to POST /v1/oauth/exchange/pod-sessions
// with registrar as the bearer, read whole.
func (ts *testServer) registrationCall(t *testing.T, registrar, podToken string) answered {
	t.Helper()
	return send(t, http.DefaultClient, request(t, http.MethodPost, ts.URL+"/v1/oauth/exchange/pod-sessions",
		jsonBody(t, map[string]any{"pod_token": podToken, "session": registrationSession}),
		map[string]string{"Authorization": "Bearer " + registrar, "Content-Type": "application/json"}))
}

// exchangeCall trades subjectToken, a pod's token of ciphertest.ExchangeAccount, at POST
// /v1/oauth/token as a pod's RFC 8693 exchange does, with a DPoP proof of a key of its own.
func (ts *testServer) exchangeCall(t *testing.T, subjectToken string) answered {
	t.Helper()
	return tokenEndpointCall(t, ts.URL, url.Values{
		"grant_type": {cipherclient.GrantTypeTokenExchange}, "client_id": {oauthwire.ExchangeClientID},
		"subject_token": {subjectToken}, oauth.FormSubjectTokenType: {oauthwire.SubjectTokenTypeKubernetes},
		oauthwire.FormSession: {ciphertest.ExchangeSession},
	})
}

// tokenEndpointCall posts form to base's POST /v1/oauth/token with a DPoP proof of a fresh key,
// read whole.
func tokenEndpointCall(t *testing.T, base string, form url.Values) answered {
	t.Helper()
	client := &http.Client{Transport: cipherclient.NewDPoPTransport(newSigningKey(t), http.DefaultTransport)}
	return send(t, client, request(t, http.MethodPost, base+"/v1/oauth/token", strings.NewReader(form.Encode()),
		map[string]string{"Content-Type": "application/x-www-form-urlencoded"}))
}

// googleRig is the test server with a Google issuer at its token exchange
// (ciphertest.WithGoogleIssuer), the issuer standing in for Google, and the key it publishes.
type googleRig struct {
	*testServer
	issuer *oidctest.Issuer
	key    *oidctest.Key
}

// newGoogleRig starts a Google issuer publishing one key and a test server whose token exchange
// takes it.
func newGoogleRig(t *testing.T) *googleRig {
	t.Helper()
	google := oidctest.New(t)
	key := google.PublishKey(t, "google-key")
	return &googleRig{
		testServer: newProfiledTestServer(t, "", func(*api.Deps) {}, ciphertest.WithGoogleIssuer(google)),
		issuer:     google, key: key,
	}
}

// signInCall is a person's sign-in trade on a machine at POST /v1/oauth/token: the person's own
// Google identity token as the subject, signed by the issuer's key, and the machine's as the
// actor, signed by machineSigner.
func (g *googleRig) signInCall(t *testing.T, machineSigner *oidctest.Key) answered {
	t.Helper()
	const (
		person  = "ada@example.com"
		machine = "example-fleet@example-project.iam.gserviceaccount.com"
	)
	now := time.Now()
	personToken := g.issuer.Mint(t, g.key, map[string]any{
		"iss": g.issuer.URL(), "sub": "109" + person, "aud": ciphertest.GooglePersonAudience,
		"iat": now.Add(-time.Minute).Unix(), "exp": now.Add(30 * time.Minute).Unix(),
		"email": person, "email_verified": true, "hd": "example.com",
	})
	machineToken := g.issuer.Mint(t, machineSigner, map[string]any{
		"iss": g.issuer.URL(), "sub": "110" + machine, "aud": ciphertest.GoogleMachineAudience,
		"iat": now.Add(-time.Minute).Unix(), "exp": now.Add(30 * time.Minute).Unix(),
		"email": machine, "email_verified": true,
	})
	return tokenEndpointCall(t, g.URL, url.Values{
		"grant_type": {cipherclient.GrantTypeTokenExchange}, "client_id": {oauthwire.ExchangeClientID},
		"subject_token": {personToken}, oauth.FormSubjectTokenType: {oauthwire.SubjectTokenTypePerson},
		"actor_token": {machineToken}, "actor_token_type": {oauthwire.ActorTokenTypeMachine},
		oauthwire.FormSession: {"ada-box-1"},
	})
}

// answeredIssuerUnavailable fails t unless got is a JSON route's answer to a check that reached no
// verdict: 503, Retry-After 5, and {"code":"ISSUER_UNAVAILABLE","error":...} naming the outage.
func answeredIssuerUnavailable(t *testing.T, what string, got answered) {
	t.Helper()
	var body wireError
	_ = json.Unmarshal(got.body, &body)
	if got.status != http.StatusServiceUnavailable || got.header.Get("Retry-After") != unreadableRetryAfter || body.Code != "ISSUER_UNAVAILABLE" || body.Error == "" {
		t.Fatalf("%s while the issuer's key set cannot be read = %d (Retry-After %q) %s, want 503 with Retry-After %s and code ISSUER_UNAVAILABLE naming the outage: the check reached no verdict on the token, so the answer is no refusal of it",
			what, got.status, got.header.Get("Retry-After"), got.body, unreadableRetryAfter)
	}
}

// answeredHoldIssuerUnavailable is answeredIssuerUnavailable on a route of the NATS writer hold,
// whose body is the hold's refusal: {"error":"issuer_unavailable","message":...}.
func answeredHoldIssuerUnavailable(t *testing.T, what string, got answered) {
	t.Helper()
	var body struct{ Error, Message string }
	_ = json.Unmarshal(got.body, &body)
	if got.status != http.StatusServiceUnavailable || got.header.Get("Retry-After") != unreadableRetryAfter || body.Error != "issuer_unavailable" || body.Message == "" {
		t.Fatalf("%s while the issuer's key set cannot be read = %d (Retry-After %q) %s, want 503 with Retry-After %s and error issuer_unavailable naming the outage: the check reached no verdict on the operator's token, so the answer is no refusal of it",
			what, got.status, got.header.Get("Retry-After"), got.body, unreadableRetryAfter)
	}
}

// answeredTemporarilyUnavailable is answeredIssuerUnavailable at the token endpoint, whose body is
// OAuth's error object: {"error":"temporarily_unavailable","error_description":...,"code":"ISSUER_UNAVAILABLE"}.
func answeredTemporarilyUnavailable(t *testing.T, what string, got answered) {
	t.Helper()
	var body struct {
		Error       string `json:"error"`
		Description string `json:"error_description"`
		Code        string `json:"code"`
	}
	_ = json.Unmarshal(got.body, &body)
	if got.status != http.StatusServiceUnavailable || got.header.Get("Retry-After") != unreadableRetryAfter ||
		body.Error != "temporarily_unavailable" || body.Description == "" || body.Code != "ISSUER_UNAVAILABLE" {
		t.Fatalf("%s while the issuer's key set cannot be read = %d (Retry-After %q) %s, want 503 with Retry-After %s, error temporarily_unavailable and code ISSUER_UNAVAILABLE naming the outage: the library reached no verdict on the token, so the answer is no refusal of it",
			what, got.status, got.header.Get("Retry-After"), got.body, unreadableRetryAfter)
	}
}

// answeredRefusal fails t unless got is a JSON route's refusal status code, with no Retry-After:
// a token the issuer never signed is refused, not waited out.
func answeredRefusal(t *testing.T, what string, got answered, status int, code string) {
	t.Helper()
	var body wireError
	_ = json.Unmarshal(got.body, &body)
	if got.status != status || body.Code != code || got.header.Get("Retry-After") != "" {
		t.Fatalf("%s = %d (Retry-After %q) %s, want %d %s with no Retry-After", what, got.status, got.header.Get("Retry-After"), got.body, status, code)
	}
}

// answeredHoldRefusal is answeredRefusal on a route of the NATS writer hold, whose refusal names
// its code in error.
func answeredHoldRefusal(t *testing.T, what string, got answered, status int, code string) {
	t.Helper()
	var body struct{ Error string }
	_ = json.Unmarshal(got.body, &body)
	if got.status != status || body.Error != code || got.header.Get("Retry-After") != "" {
		t.Fatalf("%s = %d (Retry-After %q) %s, want %d %s with no Retry-After", what, got.status, got.header.Get("Retry-After"), got.body, status, code)
	}
}

// answeredOAuthRefusal is answeredRefusal at the token endpoint, whose refusal names its code in
// OAuth's error.
func answeredOAuthRefusal(t *testing.T, what string, got answered, status int, code string) {
	t.Helper()
	var body struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(got.body, &body)
	if got.status != status || body.Error != code || got.header.Get("Retry-After") != "" {
		t.Fatalf("%s = %d (Retry-After %q) %s, want %d %s with no Retry-After", what, got.status, got.header.Get("Retry-After"), got.body, status, code)
	}
}

// answeredIssuerUnavailableAfter is answeredIssuerUnavailable for a key set whose FailureSpacing a
// row has set: the answer carries Retry-After retryAfter.
func answeredIssuerUnavailableAfter(t *testing.T, what string, got answered, retryAfter string) {
	t.Helper()
	var body wireError
	_ = json.Unmarshal(got.body, &body)
	if got.status != http.StatusServiceUnavailable || got.header.Get("Retry-After") != retryAfter || body.Code != "ISSUER_UNAVAILABLE" || body.Error == "" {
		t.Fatalf("%s while the issuer's key set cannot be read = %d (Retry-After %q) %s, want 503 with Retry-After %s and code ISSUER_UNAVAILABLE naming the outage",
			what, got.status, got.header.Get("Retry-After"), got.body, retryAfter)
	}
}

// introspectCall is POST /v1/oauth/introspect to base with bearer as the caller's own token,
// reading back a token nobody issued, read whole.
func introspectCall(t *testing.T, base, bearer string) answered {
	t.Helper()
	return send(t, http.DefaultClient, request(t, http.MethodPost, base+"/v1/oauth/introspect",
		strings.NewReader(url.Values{"token": {"a-token-nobody-issued"}}.Encode()),
		map[string]string{"Authorization": "Bearer " + bearer, "Content-Type": "application/x-www-form-urlencoded"}))
}
