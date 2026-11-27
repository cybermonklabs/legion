// named_issuers_test.go drives the REAL compiled broker trusting two named Kubernetes issuers
// (BROKER_K8S_OIDC_ISSUER=a=<URL>,b=<URL>), each a local OpenID issuer standing in for one cluster,
// on a real Postgres and over HTTP alone: each token is checked by the issuer it names, an issuer
// the broker cannot read stops neither the boot nor the other issuer and its tokens are answered
// 503 until it can be read, the token exchange takes the issuers BROKER_EXCHANGE_K8S_ISSUER names,
// a listed account is taken only from the issuers its entry names, a person's pod
// (BROKER_POD_OPERATORS) logs in only from the issuers its login entry names, and an account
// BROKER_SESSION_ACCOUNT_PAIRS pairs with a name's holder takes the name only with a token its own
// entry took. Every request and answer is logged, with no token, as the live run's transcript.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/cipher/exchange"
	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/secrets"
	"github.com/sjawhar/envoy/internal/cipher/store/storetest"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// TestTheBrokerTrustsEachNamedIssuer: with two named issuers, A and B, both up, a token of A and a
// token of B each pass enrollment, renewal, the token login, the pod-session registration and a
// route of the listed caller. Each cluster's daemon, which BROKER_POD_REGISTRARS lists for the
// worker's pods, registers its own pod under a session name of its own, since a name belongs to
// the daemon that first registered it. On each of those routes a token of a third issuer, C, gets
// the status and code a token of another issuer gets on a broker of one issuer, and so does a
// token that names A and carries a signature of B's key, under a key id A publishes.
func TestTheBrokerTrustsEachNamedIssuer(t *testing.T) {
	databaseURL := storetest.URL(t)
	a, b, c := oidctest.New(t), oidctest.New(t), oidctest.New(t)
	// One key id across the three, so the token that names A and carries B's signature names a key
	// A publishes, and only the key itself is wrong.
	aKey, bKey, cKey := a.PublishKey(t, "cluster-key"), b.PublishKey(t, "cluster-key"), c.PublishKey(t, "cluster-key")
	broker := startBroker(t, brokerOptions{Env: namedIssuersEnv(t, databaseURL, bothIssuers(a, b),
		"BROKER_SERVICE_LOGINS="+namedService+"="+namedDaemonA+"=a,b "+namedService+"="+namedDaemonB+"=a,b",
		"BROKER_API_ACCOUNTS=dispatch="+namedCaller+"=a,b",
		// The registration route writes for an account the exchange lists with no fixed session,
		// from a registrar BROKER_POD_REGISTRARS pairs with that account.
		"BROKER_EXCHANGE_K8S_AUDIENCE="+exchangeAudience,
		"BROKER_EXCHANGE_K8S_ISSUER=a",
		"BROKER_EXCHANGE_ACCOUNTS="+namedWorker+"=ACME=ada@example.com",
		"BROKER_POD_REGISTRARS="+namedDaemonA+"="+namedWorker+" "+namedDaemonB+"="+namedWorker,
	)})
	client := namedClient{t: t, base: "http://" + broker.Addr}

	// A's login and pod, which the refusals below present their tokens to.
	var loginOfA launcher
	var podOfA pod
	for _, cluster := range []struct {
		name   string
		issuer *oidctest.Issuer
		key    *oidctest.Key
		daemon string
	}{
		{"A", a, aKey, namedDaemonA},
		{"B", b, bKey, namedDaemonB},
	} {
		t.Run("a token of "+cluster.name, func(t *testing.T) {
			podUID := "pod-of-" + strings.ToLower(cluster.name)
			var login launcher
			var enrolled pod
			t.Run("passes the token login", func(t *testing.T) {
				status, code, issued := client.tokenLogin(clusterToken(t, cluster.issuer, cluster.key, cluster.daemon, ""), proofKey(t))
				if status != http.StatusOK || issued.credentialID == "" {
					t.Fatalf("answered %d %q, want 200 with a credential", status, code)
				}
				login = issued
			})
			t.Run("passes enrollment", func(t *testing.T) {
				if login.credentialID == "" {
					t.Fatal("the token login above issued no credential to enroll with")
				}
				status, code, p := client.enroll(login, podUID, clusterToken(t, cluster.issuer, cluster.key, namedWorker, podUID))
				if status != http.StatusCreated || p.enrollmentID == "" {
					t.Fatalf("answered %d %q, want 201 with an enrollment", status, code)
				}
				enrolled = p
			})
			t.Run("passes renewal", func(t *testing.T) {
				if enrolled.enrollmentID == "" {
					t.Fatal("the enrollment above made no enrollment to renew")
				}
				if status, code := client.renew(enrolled, clusterToken(t, cluster.issuer, cluster.key, namedWorker, podUID)); status != http.StatusOK {
					t.Fatalf("answered %d %q, want 200", status, code)
				}
			})
			t.Run("passes pod-session registration", func(t *testing.T) {
				status, answer := client.registerFor(clusterToken(t, cluster.issuer, cluster.key, cluster.daemon, ""), clusterToken(t, cluster.issuer, cluster.key, namedWorker, podUID), namedSession+"-"+strings.ToLower(cluster.name))
				if status != http.StatusOK {
					t.Fatalf("answered %d %q, want 200", status, codeOf(answer))
				}
			})
			t.Run("passes an API account route", func(t *testing.T) {
				if status, code := client.pending(clusterToken(t, cluster.issuer, cluster.key, namedCaller, "")); status != http.StatusOK {
					t.Fatalf("answered %d %q, want 200", status, code)
				}
			})
			if cluster.name == "A" {
				loginOfA, podOfA = login, enrolled
			}
		})
	}

	for _, refused := range []struct {
		name  string
		token func(account, podUID string) string
	}{
		{"a token of a third issuer, C,", func(account, podUID string) string { return clusterToken(t, c, cKey, account, podUID) }},
		{"a token naming A and signed with B's key", func(account, podUID string) string { return clusterToken(t, a, bKey, account, podUID) }},
	} {
		t.Run(refused.name, func(t *testing.T) {
			if loginOfA.credentialID == "" || podOfA.enrollmentID == "" {
				t.Fatal("A's login or A's pod is missing, so there is nothing to present the refused tokens to")
			}
			expect(t, "is refused ", []namedRoute{
				{"at enrollment", func() (int, string) {
					status, code, _ := client.enroll(loginOfA, "pod-refused", refused.token(namedWorker, "pod-refused"))
					return status, code
				}, http.StatusForbidden, "POD_IDENTITY_MISMATCH"},
				{"at renewal", func() (int, string) {
					return client.renew(podOfA, refused.token(namedWorker, podOfA.runtimeID))
				}, http.StatusForbidden, "POD_IDENTITY_MISMATCH"},
				{"at the token login", func() (int, string) {
					status, code, _ := client.tokenLogin(refused.token(namedDaemonA, ""), proofKey(t))
					return status, code
				}, http.StatusUnauthorized, "TOKEN_INVALID"},
				{"at pod-session registration", func() (int, string) {
					return client.register(clusterToken(t, a, aKey, namedDaemonA, ""), refused.token(namedWorker, "pod-refused"))
				}, http.StatusUnauthorized, "POD_TOKEN_INVALID"},
				{"as the registrar's own token", func() (int, string) {
					return client.register(refused.token(namedDaemonA, ""), clusterToken(t, a, aKey, namedWorker, "pod-refused"))
				}, http.StatusUnauthorized, "ACCOUNT_TOKEN_INVALID"},
				{"on an API account route", func() (int, string) {
					return client.pending(refused.token(namedCaller, ""))
				}, http.StatusUnauthorized, "ACCOUNT_TOKEN_INVALID"},
			})
		})
	}
}

// TestTheBrokerBootsWithAnIssuerItCannotRead: B's discovery document fails at boot. The broker
// starts, logs one line for A, read, and one for B, not read, each with its name and its URL,
// answers /healthz 200, and serves A's token login. A token of B gets 503 OIDC_ISSUER_UNAVAILABLE
// on enrollment, renewal, the token login, the pod-session registration, a route of the listed
// caller and a service-login route, and twelve token logins of B from one address, past the
// address's burst of ten, each get 503 and none 429. Once B answers, B's token login answers 200
// within 60 seconds, and once the background read has read B too, one line names B read again and
// none says B cannot be read, the boot's line having said so. The control boots with B up and sees
// B's first token pass, so the 503 is the unread issuer's.
func TestTheBrokerBootsWithAnIssuerItCannotRead(t *testing.T) {
	binary := buildBroker(t)
	a, b := oidctest.New(t), oidctest.New(t)
	aKey, bKey := a.PublishKey(t, "a-key"), b.PublishKey(t, "b-key")
	settings := []string{
		"BROKER_SERVICE_LOGINS=" + namedService + "=" + namedDaemonA + "=a,b " + namedService + "=" + namedDaemonB + "=a,b",
		"BROKER_API_ACCOUNTS=dispatch=" + namedCaller + "=a,b",
	}

	t.Run("the control boots with B up and its first token of B passes the token login", func(t *testing.T) {
		control := startBroker(t, brokerOptions{Binary: binary, Env: namedIssuersEnv(t, storetest.URL(t), bothIssuers(a, b), settings...)})
		client := namedClient{t: t, base: "http://" + control.Addr}
		if status, code, _ := client.tokenLogin(clusterToken(t, b, bKey, namedDaemonB, ""), proofKey(t)); status != http.StatusOK {
			t.Fatalf("answered %d %q, want 200", status, code)
		}
	})

	b.FailDiscovery(http.StatusServiceUnavailable)
	broker := startBroker(t, brokerOptions{Binary: binary, Env: namedIssuersEnv(t, storetest.URL(t), bothIssuers(a, b), settings...)})
	client := namedClient{t: t, base: "http://" + broker.Addr}

	t.Run("the broker starts and logs a line for each issuer with its name, its URL and whether it read it", func(t *testing.T) {
		for _, want := range []map[string]any{
			{"severity": "INFO", "name": "a", "url": a.URL(), "read": true},
			{"severity": "INFO", "name": "b", "url": b.URL(), "read": false},
		} {
			if got := len(recordsWith(broker.Boot, issuerBootMessage, want)); got != 1 {
				t.Errorf("the boot log holds %d lines %q carrying %v, want one:\n%s", got, issuerBootMessage, want, broker.Boot)
			}
		}
	})
	t.Run("/healthz answers 200", func(t *testing.T) { requireHealthy(t, broker.Addr) })

	var loginOfA launcher
	var podOfA pod
	t.Run("a token of A passes the token login", func(t *testing.T) {
		status, code, issued := client.tokenLogin(clusterToken(t, a, aKey, namedDaemonA, ""), proofKey(t))
		if status != http.StatusOK || issued.credentialID == "" {
			t.Fatalf("answered %d %q, want 200 with a credential", status, code)
		}
		loginOfA = issued
		status, code, podOfA = client.enroll(loginOfA, "pod-of-a", clusterToken(t, a, aKey, namedWorker, "pod-of-a"))
		if status != http.StatusCreated {
			t.Fatalf("the enrollment of A's pod, which the renewal below renews, answered %d %q, want 201", status, code)
		}
	})
	if loginOfA.credentialID == "" || podOfA.enrollmentID == "" {
		t.Fatal("A's login or A's pod is missing, so there is nothing to present B's tokens to")
	}

	expect(t, "a token of B gets 503 OIDC_ISSUER_UNAVAILABLE ", []namedRoute{
		{"on enrollment", func() (int, string) {
			status, code, _ := client.enroll(loginOfA, "pod-of-b", clusterToken(t, b, bKey, namedWorker, "pod-of-b"))
			return status, code
		}, http.StatusServiceUnavailable, issuerUnavailable},
		{"on renewal", func() (int, string) {
			return client.renew(podOfA, clusterToken(t, b, bKey, namedWorker, podOfA.runtimeID))
		}, http.StatusServiceUnavailable, issuerUnavailable},
		{"on the token login", func() (int, string) {
			status, code, _ := client.tokenLogin(clusterToken(t, b, bKey, namedDaemonB, ""), proofKey(t))
			return status, code
		}, http.StatusServiceUnavailable, issuerUnavailable},
		{"on pod-session registration", func() (int, string) {
			return client.register(clusterToken(t, a, aKey, namedDaemonA, ""), clusterToken(t, b, bKey, namedWorker, "pod-of-b"))
		}, http.StatusServiceUnavailable, issuerUnavailable},
		{"on an API account route", func() (int, string) {
			return client.pending(clusterToken(t, b, bKey, namedCaller, ""))
		}, http.StatusServiceUnavailable, issuerUnavailable},
		{"on a service-login route", func() (int, string) {
			return client.register(clusterToken(t, b, bKey, namedDaemonB, ""), clusterToken(t, a, aKey, namedWorker, podOfA.runtimeID))
		}, http.StatusServiceUnavailable, issuerUnavailable},
	})

	t.Run("twelve token logins of B from one address each get 503 and none 429", func(t *testing.T) {
		for i := range 12 {
			if status, code, _ := client.tokenLogin(clusterToken(t, b, bKey, namedDaemonB, ""), proofKey(t)); status != http.StatusServiceUnavailable || code != issuerUnavailable {
				t.Fatalf("login %d answered %d %q, want 503 %s: a login answered 503 spends none of its address's burst of 10", i+1, status, code, issuerUnavailable)
			}
		}
	})

	b.FailDiscovery(0)
	answered := time.Now()
	t.Run("once B answers, its token login answers 200 within 60 seconds and one line names B read", func(t *testing.T) {
		var last string
		if !testwait.Holds(time.Minute, time.Second, func() bool {
			status, code, issued := client.tokenLogin(clusterToken(t, b, bKey, namedDaemonB, ""), proofKey(t))
			last = fmt.Sprintf("%d %q", status, code)
			return status == http.StatusOK && issued.credentialID != ""
		}) {
			t.Fatalf("B's token login was not answered 200 within a minute of B answering; it last answered %s", last)
		}
		t.Logf("B's token login answered 200 %s after B answered", time.Since(answered).Round(time.Millisecond))
		// The background read of B writes this line of package oidc's once it has read B, just
		// before it notes B read; the token login above has noted B read already.
		read := map[string]any{"issuer": b.URL()}
		if !testwait.Holds(time.Until(answered.Add(time.Minute)), 250*time.Millisecond, func() bool {
			return len(recordsWith(broker.Log(), "oidc: discovered the issuer", read)) != 0
		}) {
			t.Fatalf("no line %q carrying %v within a minute of B answering:\n%s", "oidc: discovered the issuer", read, broker.Log())
		}
		want := map[string]any{"severity": "INFO", "name": "b", "url": b.URL()}
		if !testwait.Holds(time.Until(answered.Add(time.Minute)), 250*time.Millisecond, func() bool {
			return len(recordsWith(broker.Log(), issuerReadableMessage, want)) != 0
		}) {
			t.Fatalf("no line %q carrying %v within a minute of B answering:\n%s", issuerReadableMessage, want, broker.Log())
		}
		if got := recordsWith(broker.Log(), issuerReadableMessage, map[string]any{"name": "b"}); len(got) != 1 {
			t.Fatalf("B's recovery wrote %d lines %q, want one:\n%s", len(got), issuerReadableMessage, broker.Log())
		}
		if got := recordsWith(broker.Log(), issuerUnreadableMessage, map[string]any{"name": "b"}); len(got) != 0 {
			t.Fatalf("the tokens of B answered 503 after the boot's line said B was not read wrote %d lines %q, want none:\n%s", len(got), issuerUnreadableMessage, broker.Log())
		}
	})
}

// TestAKeySetTheBrokerCannotReadIsAnswered503: the broker reads B's discovery document at boot, and
// B's key set fails before any token is checked. A token of B gets 503 OIDC_ISSUER_UNAVAILABLE at
// enrollment and at the token login, and fifty tokens of B sent within 5 seconds cost B's issuer
// at most two key-set requests. Once the key set answers, a token of B passes both routes within
// 15 seconds.
func TestAKeySetTheBrokerCannotReadIsAnswered503(t *testing.T) {
	a, b := oidctest.New(t), oidctest.New(t)
	aKey, bKey := a.PublishKey(t, "a-key"), b.PublishKey(t, "b-key")
	b.FailKeys(http.StatusServiceUnavailable)
	broker := startBroker(t, brokerOptions{Env: namedIssuersEnv(t, storetest.URL(t), bothIssuers(a, b),
		"BROKER_SERVICE_LOGINS="+namedService+"="+namedDaemonA+"=a,b "+namedService+"="+namedDaemonB+"=a,b",
	)})
	client := namedClient{t: t, base: "http://" + broker.Addr}
	if want := (map[string]any{"name": "b", "url": b.URL(), "read": true}); len(recordsWith(broker.Boot, issuerBootMessage, want)) == 0 {
		t.Fatalf("the boot log holds no line %q carrying %v: B's discovery document must be read at boot, so that only its key set fails:\n%s", issuerBootMessage, want, broker.Boot)
	}
	status, code, loginOfA := client.tokenLogin(clusterToken(t, a, aKey, namedDaemonA, ""), proofKey(t))
	if status != http.StatusOK || loginOfA.credentialID == "" {
		t.Fatalf("A's token login answered %d %q, want 200 with a credential, which B's pods enroll under", status, code)
	}
	enrollB := func() (int, string) {
		uid := "pod-of-b-" + uuid.NewString()
		status, code, _ := client.enroll(loginOfA, uid, clusterToken(t, b, bKey, namedWorker, uid))
		return status, code
	}
	loginB := func() (int, string) {
		status, code, _ := client.tokenLogin(clusterToken(t, b, bKey, namedDaemonB, ""), proofKey(t))
		return status, code
	}

	fetchesBefore := b.JWKSFetches()
	expect(t, "a token of B gets 503 OIDC_ISSUER_UNAVAILABLE ", []namedRoute{
		{"at enrollment", enrollB, http.StatusServiceUnavailable, issuerUnavailable},
		{"at the token login", loginB, http.StatusServiceUnavailable, issuerUnavailable},
	})
	t.Run("fifty tokens of B sent within 5 seconds cost B's issuer at most two key-set requests", func(t *testing.T) {
		started := time.Now()
		for i := range 50 {
			call := enrollB
			if i%2 == 1 {
				call = loginB
			}
			if status, code := call(); status != http.StatusServiceUnavailable || code != issuerUnavailable {
				t.Fatalf("token %d of B answered %d %q, want 503 %s", i+1, status, code, issuerUnavailable)
			}
		}
		if elapsed := time.Since(started); elapsed >= 5*time.Second {
			t.Fatalf("the fifty tokens took %s, so their key-set requests say nothing of 5 seconds", elapsed)
		}
		if fetches := b.JWKSFetches() - fetchesBefore; fetches > 2 {
			t.Fatalf("B's key set was requested %d times for the tokens of B, want at most 2: a key-set read that failed is not made again for 5 seconds", fetches)
		}
	})

	b.FailKeys(0)
	t.Run("once B's key set answers, a token of B passes both routes within 15 seconds", func(t *testing.T) {
		deadline := time.Now().Add(15 * time.Second)
		for _, route := range []struct {
			name string
			call func() (int, string)
			want int
		}{
			{"the token login", loginB, http.StatusOK},
			{"enrollment", enrollB, http.StatusCreated},
		} {
			var last string
			if !testwait.Holds(time.Until(deadline), 500*time.Millisecond, func() bool {
				status, code := route.call()
				last = fmt.Sprintf("%d %q", status, code)
				return status == route.want
			}) {
				t.Fatalf("a token of B at %s was not answered %d within 15 seconds of B's key set answering; it last answered %s", route.name, route.want, last)
			}
		}
	})
}

// namedRegistrationAudience is BROKER_POD_REGISTRATION_AUDIENCE where a test sets it: a second
// audience of the clusters' tokens, which POST /v1/oauth/exchange/pod-sessions alone accepts, apart
// from enrolmentAudience and exchangeAudience, since the broker refuses to start on one equal to
// either.
const namedRegistrationAudience = "https://broker.example/pod-registration"

// TestARegistrationTokenIsCheckedByTheIssuerItNames: with BROKER_POD_REGISTRATION_AUDIENCE set and
// four named issuers, A and B up, C's discovery document failing at boot, and D's discovery
// document read at boot and its key set failing, namedDaemonA, which logs in with a token of A and
// which BROKER_POD_REGISTRARS lists for namedWorker's pods, registers at POST
// /v1/oauth/exchange/pod-sessions, each under a session name of its own, a pod whose token of the
// registration audience A issued and one whose token of that audience B issued, each answered 200
// naming that pod and that name: a daemon starts pods in each cluster, so a token of the
// registration audience is checked by the issuer it names, as a token of the broker's own audience
// is. A pod whose token of the registration audience C or D issued is answered 503
// OIDC_ISSUER_UNAVAILABLE, as on every route that checks a ServiceAccount token. D's is a token
// only the registration audience's verifier decides, since the verifier of the broker's own
// audience refuses its audience before it needs D's key. A token of the registration audience
// from E, an issuer BROKER_K8S_OIDC_ISSUER does not name, is 401 POD_TOKEN_INVALID.
func TestARegistrationTokenIsCheckedByTheIssuerItNames(t *testing.T) {
	a, b, c, d, e := oidctest.New(t), oidctest.New(t), oidctest.New(t), oidctest.New(t), oidctest.New(t)
	aKey, bKey, cKey, dKey, eKey := a.PublishKey(t, "a-key"), b.PublishKey(t, "b-key"), c.PublishKey(t, "c-key"), d.PublishKey(t, "d-key"), e.PublishKey(t, "e-key")
	c.FailDiscovery(http.StatusServiceUnavailable)
	d.FailKeys(http.StatusServiceUnavailable)
	broker := startBroker(t, brokerOptions{Env: namedIssuersEnv(t, storetest.URL(t), bothIssuers(a, b)+",c="+c.URL()+",d="+d.URL(),
		"BROKER_SERVICE_LOGINS="+namedService+"="+namedDaemonA+"=a",
		"BROKER_POD_REGISTRARS="+namedDaemonA+"="+namedWorker,
		"BROKER_POD_REGISTRATION_AUDIENCE="+namedRegistrationAudience,
		// The registration route writes for an account the exchange lists with no fixed session.
		"BROKER_EXCHANGE_K8S_AUDIENCE="+exchangeAudience,
		"BROKER_EXCHANGE_K8S_ISSUER=a,b",
		"BROKER_EXCHANGE_ACCOUNTS="+namedWorker+"=ACME=ada@example.com==a,b",
	)})
	client := namedClient{t: t, base: "http://" + broker.Addr}
	for _, want := range []map[string]any{
		{"name": "c", "url": c.URL(), "read": false},
		{"name": "d", "url": d.URL(), "read": true},
	} {
		if len(recordsWith(broker.Boot, issuerBootMessage, want)) == 0 {
			t.Fatalf("the boot log holds no line %q carrying %v: C's discovery document must fail at boot and D's be read, so that only D's key set fails:\n%s", issuerBootMessage, want, broker.Boot)
		}
	}
	registrar := clusterToken(t, a, aKey, namedDaemonA, "")
	for _, row := range []struct {
		name, about string
		issuer      *oidctest.Issuer
		key         *oidctest.Key
		status      int
		code        string
	}{
		{"A", "is registered", a, aKey, http.StatusOK, ""},
		{"B", "is registered", b, bKey, http.StatusOK, ""},
		{"C", "gets 503 OIDC_ISSUER_UNAVAILABLE while C's discovery document cannot be read", c, cKey, http.StatusServiceUnavailable, issuerUnavailable},
		{"D", "gets 503 OIDC_ISSUER_UNAVAILABLE while D's key set cannot be read", d, dKey, http.StatusServiceUnavailable, issuerUnavailable},
		{"E", "gets 401 POD_TOKEN_INVALID, as BROKER_K8S_OIDC_ISSUER does not name E", e, eKey, http.StatusUnauthorized, "POD_TOKEN_INVALID"},
	} {
		t.Run("a pod whose token of the registration audience "+row.name+" issued "+row.about, func(t *testing.T) {
			podUID := "pod-of-" + strings.ToLower(row.name)
			session := namedSession + "-" + strings.ToLower(row.name)
			status, answer := client.registerFor(registrar, clusterTokenFor(t, row.issuer, row.key, namedWorker, podUID, namedRegistrationAudience), session)
			if status != row.status || codeOf(answer) != row.code {
				t.Fatalf("answered %d %q, want %d %q", status, codeOf(answer), row.status, row.code)
			}
			if status != http.StatusOK {
				return
			}
			var registered struct {
				PodUID  string `json:"pod_uid"`
				Session string `json:"session"`
			}
			if err := json.Unmarshal(answer, &registered); err != nil || registered.PodUID != podUID || registered.Session != session {
				t.Fatalf("the registration answered %s (%v), want the pod %s and the session %s", answer, err, podUID, session)
			}
		})
	}
}

// TestTheExchangeTakesTheIssuersItNames: with BROKER_EXCHANGE_K8S_AUDIENCE set and three issuers
// named, A, B and C, the broker refuses to boot, naming BROKER_EXCHANGE_K8S_ISSUER, when that
// setting is missing, names an issuer the list does not hold, or is set without the exchange's
// audience, when an entry of BROKER_EXCHANGE_ACCOUNTS or of BROKER_EXCHANGE_READ_ACCOUNTS names
// an issuer the setting does not name or names one twice, and when the setting leaves out A
// beside an entry of BROKER_EXCHANGE_READ_ACCOUNTS that names no issuer and so takes A alone. With
// the setting naming A and B, the exchange takes a token of A and a token of B for an account
// whose entry names both, and refuses with 400 invalid_request and no access token a token of C,
// a token of B for an account whose entry names A alone, and a token of B for an account whose
// entry names no issuer, which takes A, the first, alone. For a read entry that names no issuer
// it mints the read-only token for a token of A and refuses one of B and one of C; for a read
// entry that names B alone it mints the read-only token for a token of B and refuses one of A,
// so the read entry's issuers are read. With B's discovery document failing, the broker boots,
// the exchange takes a token of A, and a token of B gets 503 OIDC_ISSUER_UNAVAILABLE. With one
// issuer and no BROKER_EXCHANGE_K8S_ISSUER, the exchange takes that issuer's token for an entry
// that names no issuer, whether the issuer is named or is one bare URL.
func TestTheExchangeTakesTheIssuersItNames(t *testing.T) {
	const (
		// namingA's entry names A alone, and namingNone's names no issuer; namedWorker's names A
		// and B. Each fixes a session of its own, so its pods exchange with no registration.
		// reading is the account of a read entry that names no issuer, and readingB the account
		// of one that names B alone; each fixes its session, as every read entry does, and its
		// token names its service. readEntry is the setting listing reading alone, readEntries
		// the setting listing both, and readHeadOfB readingB's entry up to its session, which
		// the refusals below end with an issuer field of their own.
		namingA           = "system:serviceaccount:legion:legion-worker-a"
		namingNone        = "system:serviceaccount:legion:legion-worker-none"
		reading           = "system:serviceaccount:legion:legion-reader"
		readingB          = "system:serviceaccount:legion:legion-reader-b"
		serviceOfReading  = "legion-reader"
		serviceOfReadingB = "legion-reader-b"
		sessionOfA        = "example-session-a"
		sessionOfNone     = "example-session-none"
		sessionOfReading  = "example-session-reading"
		sessionOfReadingB = "example-session-reading-b"
		readEntry         = "BROKER_EXCHANGE_READ_ACCOUNTS=" + reading + "=" + serviceOfReading + "=" + sessionOfReading
		readHeadOfB       = readingB + "=" + serviceOfReadingB + "=" + sessionOfReadingB
		readEntryOfB      = readHeadOfB + "=b"
		readEntries       = readEntry + " " + readEntryOfB
		exchangeIssuer    = "BROKER_EXCHANGE_K8S_ISSUER"
	)
	binary := buildBroker(t)
	a, b, c := oidctest.New(t), oidctest.New(t), oidctest.New(t)
	aKey, bKey, cKey := a.PublishKey(t, "a-key"), b.PublishKey(t, "b-key"), c.PublishKey(t, "c-key")
	threeIssuers := bothIssuers(a, b) + ",c=" + c.URL()
	audience := "BROKER_EXCHANGE_K8S_AUDIENCE=" + exchangeAudience
	accounts := "BROKER_EXCHANGE_ACCOUNTS=" + namedWorker + "=ACME=ada@example.com=" + namedSession + "=a,b " +
		namingA + "=ACME=ada@example.com=" + sessionOfA + "=a " +
		namingNone + "=ACME=ada@example.com=" + sessionOfNone

	for _, refusal := range []struct {
		name string
		env  []string
		says []string
	}{
		{"BROKER_EXCHANGE_K8S_ISSUER is missing", []string{audience}, []string{exchangeIssuer}},
		// The broker's log is JSON, so a quoted name in a refusal reads \"name\" in it.
		{"BROKER_EXCHANGE_K8S_ISSUER names an issuer the list does not hold", []string{audience, exchangeIssuer + "=a,d"}, []string{exchangeIssuer, `\"d\"`}},
		{"BROKER_EXCHANGE_K8S_ISSUER is set without the exchange audience", []string{exchangeIssuer + "=a,b"}, []string{exchangeIssuer, "BROKER_EXCHANGE_K8S_AUDIENCE"}},
		{"an entry of BROKER_EXCHANGE_ACCOUNTS names an issuer the setting does not name",
			[]string{audience, exchangeIssuer + "=a,b", "BROKER_EXCHANGE_ACCOUNTS=" + namedWorker + "=ACME=ada@example.com==c"},
			[]string{exchangeIssuer, "BROKER_EXCHANGE_ACCOUNTS", `\"c\"`}},
		{"an entry of BROKER_EXCHANGE_ACCOUNTS names one issuer twice",
			[]string{audience, exchangeIssuer + "=a,b", "BROKER_EXCHANGE_ACCOUNTS=" + namedWorker + "=ACME=ada@example.com==a,a"},
			[]string{exchangeIssuer, "BROKER_EXCHANGE_ACCOUNTS", `\"a\"`}},
		{"BROKER_EXCHANGE_K8S_ISSUER leaves out A beside an entry of BROKER_EXCHANGE_READ_ACCOUNTS that names no issuer",
			[]string{audience, exchangeIssuer + "=b,c", readEntry},
			[]string{exchangeIssuer, "BROKER_EXCHANGE_READ_ACCOUNTS", reading, `\"a\"`}},
		{"an entry of BROKER_EXCHANGE_READ_ACCOUNTS names an issuer the setting does not name",
			[]string{audience, exchangeIssuer + "=a,b", "BROKER_EXCHANGE_READ_ACCOUNTS=" + readHeadOfB + "=c"},
			[]string{exchangeIssuer, "BROKER_EXCHANGE_READ_ACCOUNTS", readingB, `\"c\"`}},
		{"an entry of BROKER_EXCHANGE_READ_ACCOUNTS names one issuer twice",
			[]string{audience, exchangeIssuer + "=a,b", "BROKER_EXCHANGE_READ_ACCOUNTS=" + readHeadOfB + "=b,b"},
			[]string{exchangeIssuer, "BROKER_EXCHANGE_READ_ACCOUNTS", readingB, `\"b\"`, "twice"}},
	} {
		t.Run("the broker refuses to boot when "+refusal.name, func(t *testing.T) {
			out := runRefusedBroker(t, binary, append([]string{
				"BROKER_DATABASE_URL=postgres://nonexistent-host-this-test-must-never-reach/db",
				"BROKER_PUBLIC_URL=https://secrets.internal.example",
				"BROKER_K8S_OIDC_ISSUER=" + threeIssuers,
				"BROKER_K8S_OIDC_AUDIENCE=" + enrolmentAudience,
			}, refusal.env...)...)
			for _, want := range refusal.says {
				if !strings.Contains(out, want) {
					t.Fatalf("the broker refused, but its output does not carry %s:\n%s", want, out)
				}
			}
		})
	}

	// trade trades the token of account's pod, minted by issuer with key for the exchange's
	// audience, under the session account's entry fixes.
	trade := func(t *testing.T, addr string, issuer *oidctest.Issuer, key *oidctest.Key, account, session string) (int, map[string]any) {
		t.Helper()
		token := mintPodToken(t, issuer, key, account, exchangeAudience, "pod-"+uuid.NewString())
		return newLiveBroker(addr, proofKey(t)).exchange(t, exchangeForm(token, session))
	}
	taken := func(t *testing.T, status int, body map[string]any) {
		t.Helper()
		if token, _ := body["access_token"].(string); status != http.StatusOK || token == "" {
			t.Fatalf("answered %d %v, want 200 with an access token", status, body)
		}
	}
	refused := func(t *testing.T, status int, body map[string]any) {
		t.Helper()
		if status != http.StatusBadRequest || body["error"] != "invalid_request" || body["access_token"] != nil {
			t.Fatalf("answered %d %v, want 400 invalid_request and no access token", status, body)
		}
	}

	t.Run("with the setting naming A and B", func(t *testing.T) {
		broker := startBroker(t, brokerOptions{Binary: binary, Env: namedIssuersEnv(t, storetest.URL(t), threeIssuers,
			audience, exchangeIssuer+"=a,b", accounts, readEntries, "BROKER_API_ACCOUNTS=dispatch="+namedCaller)})
		broker.Await(t, "token exchange")
		// The listed caller, whose entry names no issuer and so takes A, reads each read entry's
		// minted token back.
		callerToken := accountToken(t, a, aKey, namedCaller)
		for _, row := range []struct {
			name    string
			issuer  *oidctest.Issuer
			key     *oidctest.Key
			account string
			session string
			taken   bool
			// readsAs is the service a read entry's taken token names, which holds the scope
			// read alone; empty for an account entry.
			readsAs string
		}{
			{"the exchange takes a token of A for an account whose entry names A and B", a, aKey, namedWorker, namedSession, true, ""},
			{"the exchange takes a token of B for an account whose entry names A and B", b, bKey, namedWorker, namedSession, true, ""},
			{"the exchange refuses a token of C, an issuer the setting does not name", c, cKey, namedWorker, namedSession, false, ""},
			{"the exchange refuses a token of B for an account whose entry names A alone", b, bKey, namingA, sessionOfA, false, ""},
			{"the exchange takes a token of A for an account whose entry names no issuer", a, aKey, namingNone, sessionOfNone, true, ""},
			{"the exchange refuses a token of B for an account whose entry names no issuer", b, bKey, namingNone, sessionOfNone, false, ""},
			{"the exchange mints the read-only token for a token of A for a read entry's account whose entry names no issuer, which takes A alone", a, aKey, reading, sessionOfReading, true, serviceOfReading},
			{"the exchange refuses a token of B for a read entry's account whose entry names no issuer, which takes A alone", b, bKey, reading, sessionOfReading, false, ""},
			{"the exchange refuses a token of C for a read entry's account whose entry names no issuer", c, cKey, reading, sessionOfReading, false, ""},
			{"the exchange mints the read-only token for a token of B for a read entry's account whose entry names B alone", b, bKey, readingB, sessionOfReadingB, true, serviceOfReadingB},
			{"the exchange refuses a token of A for a read entry's account whose entry names B alone", a, aKey, readingB, sessionOfReadingB, false, ""},
		} {
			t.Run(row.name, func(t *testing.T) {
				status, body := trade(t, broker.Addr, row.issuer, row.key, row.account, row.session)
				if !row.taken {
					refused(t, status, body)
					return
				}
				taken(t, status, body)
				if row.readsAs == "" {
					return
				}
				token, _ := body["access_token"].(string)
				status, answer := newPlainBroker(broker.Addr).introspect(t, token, callerToken)
				if status != http.StatusOK || answer["active"] != true || answer["scope"] != "read" || answer["project_scope"] != "none" ||
					answer["service"] != row.readsAs || answer["session"] != row.session || answer["person"] != nil || answer["projects"] != nil {
					t.Fatalf("introspection answered %d %v, want an active token holding the scope read alone and the project scope none, naming the service %s and the session %s, and no person and no project", status, answer, row.readsAs, row.session)
				}
			})
		}
	})

	t.Run("with B's discovery document failing", func(t *testing.T) {
		b.FailDiscovery(http.StatusServiceUnavailable)
		t.Cleanup(func() { b.FailDiscovery(0) })
		broker := startBroker(t, brokerOptions{Binary: binary, Env: namedIssuersEnv(t, storetest.URL(t), threeIssuers,
			audience, exchangeIssuer+"=a,b", accounts)})
		broker.Await(t, "token exchange")
		t.Run("the broker boots and answers /healthz 200", func(t *testing.T) { requireHealthy(t, broker.Addr) })
		t.Run("the exchange takes a token of A", func(t *testing.T) {
			status, body := trade(t, broker.Addr, a, aKey, namedWorker, namedSession)
			taken(t, status, body)
		})
		t.Run("a token of B gets 503 OIDC_ISSUER_UNAVAILABLE", func(t *testing.T) {
			status, body := trade(t, broker.Addr, b, bKey, namedWorker, namedSession)
			if status != http.StatusServiceUnavailable || body["code"] != issuerUnavailable || body["access_token"] != nil {
				t.Fatalf("answered %d %v, want 503 %s and no access token", status, body, issuerUnavailable)
			}
		})
	})

	for _, one := range []struct{ name, issuers string }{
		{"named", "a=" + a.URL()},
		{"written as one bare URL", a.URL()},
	} {
		t.Run("with one issuer "+one.name+" and no BROKER_EXCHANGE_K8S_ISSUER, the exchange takes its token for an entry that names no issuer", func(t *testing.T) {
			broker := startBroker(t, brokerOptions{Binary: binary, Env: namedIssuersEnv(t, storetest.URL(t), one.issuers,
				audience, "BROKER_EXCHANGE_ACCOUNTS="+namingNone+"=ACME=ada@example.com="+sessionOfNone)})
			broker.Await(t, "token exchange")
			status, body := trade(t, broker.Addr, a, aKey, namingNone, sessionOfNone)
			taken(t, status, body)
		})
	}
}

// TestAnAccountIsAcceptedFromTheIssuersItsEntryNames: a BROKER_API_ACCOUNTS entry that names no
// issuer accepts its subject's token of A, the first issuer, on GET /v1/pending, and answers its
// subject's token of B 403 ACCOUNT_NOT_LISTED; an entry that names A and B accepts both. A
// BROKER_SERVICE_LOGINS entry gives the same answers at the token login. A subject no entry lists
// gets 403 ACCOUNT_NOT_LISTED from either issuer. After a restart with an entry that no longer
// names B, the credential B's token login made stops authenticating, while the credential A's
// login made under the entry that names no issuer still does.
func TestAnAccountIsAcceptedFromTheIssuersItsEntryNames(t *testing.T) {
	const (
		callerNamingNone = "system:serviceaccount:example-ns:example-dispatch-one"
		callerNamingBoth = "system:serviceaccount:example-ns:example-dispatch-both"
		loginNamingNone  = "system:serviceaccount:legion-system:legion-daemon-one"
		loginNamingBoth  = "system:serviceaccount:legion-system:legion-daemon-both"
		stranger         = "system:serviceaccount:example-ns:example-stranger"
	)
	databaseURL := storetest.URL(t)
	binary := buildBroker(t)
	a, b := oidctest.New(t), oidctest.New(t)
	aKey, bKey := a.PublishKey(t, "a-key"), b.PublishKey(t, "b-key")
	// env is the broker's environment with the second login's entry naming its issuers as names
	// says.
	env := func(names string) []string {
		return namedIssuersEnv(t, databaseURL, bothIssuers(a, b),
			"BROKER_API_ACCOUNTS=dispatch="+callerNamingNone+" dispatch="+callerNamingBoth+"=a,b",
			"BROKER_SERVICE_LOGINS="+namedService+"="+loginNamingNone+" "+namedService+"="+loginNamingBoth+names)
	}
	broker := startBroker(t, brokerOptions{Binary: binary, Env: env("=a,b")})
	client := namedClient{t: t, base: "http://" + broker.Addr}
	tokenOf := map[string]func(t *testing.T, account string) string{
		"A": func(t *testing.T, account string) string { return clusterToken(t, a, aKey, account, "") },
		"B": func(t *testing.T, account string) string { return clusterToken(t, b, bKey, account, "") },
	}

	for _, c := range []struct {
		name, account, issuer string
		listed                bool
	}{
		{"the entry naming no issuer accepts its subject's token of A", callerNamingNone, "A", true},
		{"the entry naming no issuer answers its subject's token of B 403 ACCOUNT_NOT_LISTED", callerNamingNone, "B", false},
		{"the entry naming A and B accepts its subject's token of A", callerNamingBoth, "A", true},
		{"the entry naming A and B accepts its subject's token of B", callerNamingBoth, "B", true},
		{"a subject no entry lists gets 403 ACCOUNT_NOT_LISTED from A", stranger, "A", false},
		{"a subject no entry lists gets 403 ACCOUNT_NOT_LISTED from B", stranger, "B", false},
	} {
		t.Run("GET /v1/pending: "+c.name, func(t *testing.T) {
			status, code := client.pending(tokenOf[c.issuer](t, c.account))
			if c.listed && status != http.StatusOK || !c.listed && (status != http.StatusForbidden || code != "ACCOUNT_NOT_LISTED") {
				t.Fatalf("answered %d %q, want %s", status, code, map[bool]string{true: "200", false: "403 ACCOUNT_NOT_LISTED"}[c.listed])
			}
		})
	}

	// The credentials the logins below issue that the restart is read against: B's, made by the
	// entry naming both, and A's, made by the entry naming none.
	var fromB, fromA launcher
	for _, c := range []struct {
		name, account, issuer string
		listed                bool
		keep                  *launcher
	}{
		{"the entry naming no issuer accepts its subject's token of A", loginNamingNone, "A", true, &fromA},
		{"the entry naming no issuer answers its subject's token of B 403 ACCOUNT_NOT_LISTED", loginNamingNone, "B", false, nil},
		{"the entry naming A and B accepts its subject's token of A", loginNamingBoth, "A", true, nil},
		{"the entry naming A and B accepts its subject's token of B", loginNamingBoth, "B", true, &fromB},
		{"a subject no entry lists gets 403 ACCOUNT_NOT_LISTED from A", stranger, "A", false, nil},
		{"a subject no entry lists gets 403 ACCOUNT_NOT_LISTED from B", stranger, "B", false, nil},
	} {
		t.Run("the token login: "+c.name, func(t *testing.T) {
			status, code, issued := client.tokenLogin(tokenOf[c.issuer](t, c.account), proofKey(t))
			if c.listed && (status != http.StatusOK || issued.credentialID == "") || !c.listed && (status != http.StatusForbidden || code != "ACCOUNT_NOT_LISTED") {
				t.Fatalf("answered %d %q, want %s", status, code, map[bool]string{true: "200 with a credential", false: "403 ACCOUNT_NOT_LISTED"}[c.listed])
			}
			if c.keep != nil {
				*c.keep = issued
			}
		})
	}
	if fromA.credentialID == "" || fromB.credentialID == "" {
		t.Fatal("A's or B's login issued no credential, so there is nothing to read against the restart")
	}
	t.Run("the credential B's token login made authenticates while its entry names B", func(t *testing.T) {
		if status, code := client.authenticates(fromB); status != http.StatusNoContent {
			t.Fatalf("answered %d %q, want 204", status, code)
		}
	})

	broker.Terminate(t)
	if err := broker.AwaitExit(t); err != nil {
		t.Fatalf("the first broker's exit = %v, want a clean one:\n%s", err, broker.Log())
	}
	restarted := startBroker(t, brokerOptions{Binary: binary, Env: env("=a")})
	client = namedClient{t: t, base: "http://" + restarted.Addr}
	expect(t, "after a restart with an entry that no longer names B, ", []namedRoute{
		{"the credential B's token login made stops authenticating", func() (int, string) { return client.authenticates(fromB) }, http.StatusUnauthorized, "LAUNCHER_INVALID"},
		{"the credential A's token login made under the entry naming no issuer still authenticates", func() (int, string) { return client.authenticates(fromA) }, http.StatusNoContent, ""},
	})
}

// TestAPairedAccountsPodTakesNoNameWithATokenOfAnIssuerItsEntryDoesNotName is
// BROKER_SESSION_ACCOUNT_PAIRS against the issuers an account's tokens are taken from, on the real
// binary with two named issuers, A and B, and a third, C, that BROKER_K8S_OIDC_ISSUER does not
// name. A session name's holder is the account alone (exchange.AccountPrincipal), whatever cluster
// signed the token, so what keeps a pair to the clusters an operator listed is the check each
// token meets where it comes in, which runs before any claim on a name: the exchange takes a pod's
// token only from an issuer its account's BROKER_EXCHANGE_ACCOUNTS entry names
// (exchange.Set.Pod), and an enrollment only from an issuer BROKER_K8S_OIDC_ISSUER names
// (enroll.Service.verifyPodToken), since a BROKER_SERVICES entry names none.
//
// oldAccount's exchange entry names A and B, and movedAccount's names none, so it takes A alone:
// an entry written for one cluster. The two accounts are paired and bound to the daemon's service,
// and the daemon, which BROKER_POD_REGISTRARS lists for both accounts' pods, registers each pod for
// the name before it trades. While oldAccount holds the name, movedAccount's pod trading a token
// of B is refused invalid_request for its issuer, and its pod enrolling with a token of C is
// refused 403 POD_IDENTITY_MISMATCH: the name stays oldAccount's and no take is logged. The pair
// is then shown live from each named issuer: movedAccount's pod trading a token of A takes the
// name, and oldAccount's pod enrolling with a token of B takes it back, each take logged once.
func TestAPairedAccountsPodTakesNoNameWithATokenOfAnIssuerItsEntryDoesNotName(t *testing.T) {
	const (
		oldAccount   = namedWorker
		movedAccount = "system:serviceaccount:legion:legion-worker-next"
	)
	databaseURL := storetest.URL(t)
	a, b, c := oidctest.New(t), oidctest.New(t), oidctest.New(t)
	aKey, bKey, cKey := a.PublishKey(t, "a-key"), b.PublishKey(t, "b-key"), c.PublishKey(t, "c-key")
	broker := startBroker(t, brokerOptions{Env: namedIssuersEnv(t, databaseURL, bothIssuers(a, b),
		"BROKER_SERVICES="+namedService+"="+oldAccount+" "+namedService+"="+movedAccount,
		"BROKER_SERVICE_LOGINS="+namedService+"="+namedDaemonA,
		"BROKER_EXCHANGE_K8S_AUDIENCE="+exchangeAudience,
		"BROKER_EXCHANGE_K8S_ISSUER=a,b",
		"BROKER_EXCHANGE_ACCOUNTS="+oldAccount+"=ACME=ada@example.com==a,b "+movedAccount+"=ACME=ada@example.com",
		"BROKER_SESSION_ACCOUNT_PAIRS="+oldAccount+"="+movedAccount,
		"BROKER_POD_REGISTRARS="+namedDaemonA+"="+oldAccount+" "+namedDaemonA+"="+movedAccount,
	)})
	broker.AwaitRecord(t, "session account pairs", map[string]any{"state": "on"})
	db := connectToBroker(t, databaseURL)
	live := newLiveBroker(broker.Addr, proofKey(t))
	client := namedClient{t: t, base: "http://" + broker.Addr}
	oldPrincipal, movedPrincipal := exchange.AccountPrincipal(oldAccount), exchange.AccountPrincipal(movedAccount)
	registrar := clusterToken(t, a, aKey, namedDaemonA, "")
	// trade has the daemon register the pod uid of account, by a token issuer signs with key, for
	// namedSession, and then trades that pod's token of the same issuer under the name.
	trade := func(issuer *oidctest.Issuer, key *oidctest.Key, account, uid string) (int, map[string]any) {
		t.Helper()
		live.registered(t, clusterToken(t, issuer, key, account, uid), namedSession, registrar)
		return live.exchange(t, exchangeForm(mintPodToken(t, issuer, key, account, exchangeAudience, uid), namedSession))
	}
	// holds fails t unless principal holds namedSession, as an ordinary claim, and the broker has
	// logged exchangeTakes takes of a name by an exchange and enrollmentTakes by an enrollment.
	holds := func(principal string, exchangeTakes, enrollmentTakes int) {
		t.Helper()
		if holder, fixed := sessionHolder(t, db, namedSession); holder != principal || fixed {
			t.Fatalf("the name %s is held by %q (the configuration's hold: %t), want %q", namedSession, holder, fixed, principal)
		}
		if takes := recordsWith(broker.Log(), exchangeTookName, nil); len(takes) != exchangeTakes {
			t.Fatalf("the broker logged %d %q lines, want %d: %v", len(takes), exchangeTookName, exchangeTakes, takes)
		}
		if takes := recordsWith(broker.Log(), enrollmentTookName, nil); len(takes) != enrollmentTakes {
			t.Fatalf("the broker logged %d %q lines, want %d: %v", len(takes), enrollmentTookName, enrollmentTakes, takes)
		}
	}

	t.Log("--- the old account's pod trades a token of A under the name, which makes the name its account's")
	if status, body := trade(a, aKey, oldAccount, "pod-old-a"); status != http.StatusOK || body["access_token"] == nil {
		t.Fatalf("the old account's pod trading a token of A under %s = %d %v, want 200 with an access token", namedSession, status, body)
	}
	holds(oldPrincipal, 0, 0)

	t.Log("--- the moved account's pod trades a token of B, which its entry does not name, and takes nothing")
	status, body := trade(b, bKey, movedAccount, "pod-moved-b")
	description, _ := body["error_description"].(string)
	if status != http.StatusBadRequest || body["error"] != "invalid_request" || body["access_token"] != nil ||
		!strings.Contains(description, "does not name the issuer of its token") {
		t.Fatalf("the moved account's pod trading a token of B under %s = %d %v, want 400 invalid_request saying its entry does not name the token's issuer: the pair reaches a token only once its own entry took it", namedSession, status, body)
	}
	holds(oldPrincipal, 0, 0)

	t.Log("--- the moved account's pod enrols with a token of C, which no setting names, and takes nothing")
	status, code, daemon := client.tokenLogin(clusterToken(t, a, aKey, namedDaemonA, ""), proofKey(t))
	if status != http.StatusOK || daemon.credentialID == "" {
		t.Fatalf("the daemon's token login of A = %d %q, want 200 with a credential", status, code)
	}
	pods := func(issuer *oidctest.Issuer, key *oidctest.Key) podBroker {
		return podBroker{t: t, base: client.base, issuer: issuer, key: key, audience: enrolmentAudience}
	}
	if _, status, answer := pods(c, cKey).enrolPod(daemon, "pod-moved-c", movedAccount, namedSession); status != http.StatusForbidden || codeOf(answer) != "POD_IDENTITY_MISMATCH" {
		t.Fatalf("the moved account's pod enrolling %s with a token of C = %d %s, want 403 POD_IDENTITY_MISMATCH: a token of an issuer the broker does not name is refused before the name is claimed", namedSession, status, bytes.TrimSpace(answer))
	}
	holds(oldPrincipal, 0, 0)

	t.Log("--- the moved account's pod trades a token of A, which its entry names, and takes the name")
	if status, body := trade(a, aKey, movedAccount, "pod-moved-a"); status != http.StatusOK || body["access_token"] == nil {
		t.Fatalf("the moved account's pod trading a token of A under %s = %d %v, want 200 with an access token: a paired account takes the name its pair holds", namedSession, status, body)
	}
	broker.AwaitRecord(t, exchangeTookName, map[string]any{"session": namedSession, "from": oldPrincipal, "to": movedPrincipal})
	holds(movedPrincipal, 1, 0)

	t.Log("--- the old account's pod enrols with a token of B, which BROKER_K8S_OIDC_ISSUER names, and takes the name back")
	if _, status, answer := pods(b, bKey).enrolPod(daemon, "pod-old-b", oldAccount, namedSession); status != http.StatusCreated {
		t.Fatalf("the old account's pod enrolling %s with a token of B = %d %s, want 201: a paired account takes the name its pair holds", namedSession, status, bytes.TrimSpace(answer))
	}
	broker.AwaitRecord(t, enrollmentTookName, map[string]any{"session": namedSession, "from": movedPrincipal, "to": oldPrincipal})
	holds(oldPrincipal, 1, 1)
}

// TestAPersonsPodLogsInThroughTheIssuersItsLoginNames: BROKER_POD_OPERATORS maps ada's account to
// ada, and names no issuer, and the account's BROKER_SERVICE_LOGINS entry names A alone of three
// named issuers: A and B, which the broker reads, and C, whose discovery document fails at boot.
// Ada's pod logs in with a token of A bound to it, enrols itself under that login with no session
// name, its enrollment names ada as its operator, and it is granted ada's own agent-tier secret
// with no approval: it acts as ada. The same pod's token of B, an issuer the broker trusts and the
// entry does not name, is refused 403 ACCOUNT_NOT_LISTED at the token login, as every account's
// token of an issuer its entry does not name is, so it logs in for no one; and its token of C is
// answered 503 OIDC_ISSUER_UNAVAILABLE.
func TestAPersonsPodLogsInThroughTheIssuersItsLoginNames(t *testing.T) {
	const (
		person  = "ada@example.com"
		account = "system:serviceaccount:people:ada"
		service = "person-ada"
		secret  = "ADA_AGENT_TOKEN"
		podUID  = "ada-pod-1"
	)
	a, b, c := oidctest.New(t), oidctest.New(t), oidctest.New(t)
	aKey, bKey, cKey := a.PublishKey(t, "a-key"), b.PublishKey(t, "b-key"), c.PublishKey(t, "c-key")
	c.FailDiscovery(http.StatusServiceUnavailable)
	store := &requesterStore{
		path: filepath.Join(t.TempDir(), "fake-secrets.json"),
		rows: []secrets.LocalSecret{requesterRow(secret, person, policy.TierAgent, "ada-agent-token-value", nil)},
	}
	store.write(t)
	broker := startBroker(t, brokerOptions{Env: namedIssuersEnv(t, storetest.URL(t), bothIssuers(a, b)+",c="+c.URL(),
		"BROKER_SERVICE_LOGINS="+service+"="+account+"=a",
		"BROKER_POD_OPERATORS="+person+"="+account,
		// namedIssuersEnv names a store that holds no secret. The broker reads the last value a
		// setting is given, so it reads this store, which holds ada's own secret.
		"BROKER_FAKE_SECRETS_FILE="+store.path,
	)})
	if want := (map[string]any{"name": "c", "url": c.URL(), "read": false}); len(recordsWith(broker.Boot, issuerBootMessage, want)) != 1 {
		t.Fatalf("the boot log holds no one line %q carrying %v: C must be unread for its token's 503 below:\n%s", issuerBootMessage, want, broker.Boot)
	}
	client := namedClient{t: t, base: "http://" + broker.Addr}

	var login launcher
	var enrolled pod
	t.Run("a token of A, which its login entry names, logs ada's pod in", func(t *testing.T) {
		status, code, issued := client.tokenLoginAs(service, clusterToken(t, a, aKey, account, podUID), proofKey(t))
		if status != http.StatusOK || issued.credentialID == "" {
			t.Fatalf("answered %d %q, want 200 with a credential", status, code)
		}
		login = issued
	})
	t.Run("the pod enrols itself under that login, and its enrollment acts as ada", func(t *testing.T) {
		if login.credentialID == "" {
			t.Fatal("the token login above issued no credential to enrol with")
		}
		status, code, p := client.enroll(login, podUID, clusterToken(t, a, aKey, account, podUID))
		if status != http.StatusCreated || p.enrollmentID == "" {
			t.Fatalf("answered %d %q, want 201 with an enrollment", status, code)
		}
		enrolled = p
		if operator := client.operatorOf(enrolled); operator != person {
			t.Fatalf("the pod's enrollment %s names the operator %q, want %s: a pod-bound token of an account BROKER_POD_OPERATORS maps logs in as its person", enrolled.enrollmentID, operator, person)
		}
	})
	t.Run("the pod is granted ada's own agent-tier secret with no approval", func(t *testing.T) {
		if enrolled.enrollmentID == "" {
			t.Fatal("the enrollment above made no enrollment to ask with")
		}
		reply := podBroker{t: t, base: client.base, issuer: a, key: aKey, audience: enrolmentAudience}.ask(enrolled, secret)
		if reply.State != "granted" || reply.GrantID == nil {
			t.Fatalf("the pod's request for %s answered %q, want granted: the pod acts as ada, whose own agent-tier secret needs no approval", secret, reply.State)
		}
	})
	expect(t, "the same pod's token login ", []namedRoute{
		{"with a token of B, which its login entry does not name, is refused 403 ACCOUNT_NOT_LISTED", func() (int, string) {
			status, code, _ := client.tokenLoginAs(service, clusterToken(t, b, bKey, account, podUID), proofKey(t))
			return status, code
		}, http.StatusForbidden, "ACCOUNT_NOT_LISTED"},
		{"with a token of C, which the broker cannot read, gets 503 OIDC_ISSUER_UNAVAILABLE", func() (int, string) {
			status, code, _ := client.tokenLoginAs(service, clusterToken(t, c, cKey, account, podUID), proofKey(t))
			return status, code
		}, http.StatusServiceUnavailable, issuerUnavailable},
	})
}
