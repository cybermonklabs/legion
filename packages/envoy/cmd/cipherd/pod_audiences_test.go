// pod_audiences_test.go drives BROKER_K8S_OIDC_AUDIENCE as a list of audiences on the REAL
// compiled broker, over HTTP, on a real Postgres, against a local OpenID issuer standing in for
// the cluster's, as a deployment sets it while it moves its callers from one audience to another:
// every route that verifies a cluster token against the setting takes a token of each listed
// audience and judges it as it judges a token of the one audience today, the routes of the
// cluster's other audiences take none of them, and a list the broker cannot take refuses the
// start. It also holds discoverVerifiers and natsCalloutBoot, which build those verifiers, to one
// pod verifier for the whole list beside the NATS auth service's verifier of its own audience.
package main

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/ciphertest/podclient"
	"github.com/sjawhar/envoy/internal/cipher/config"
	"github.com/sjawhar/envoy/internal/cipher/record"
	"github.com/sjawhar/envoy/internal/cipher/store"
	"github.com/sjawhar/envoy/internal/cipher/store/storetest"
	"github.com/sjawhar/envoy/internal/oidc"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

const (
	// podAudienceNew and podAudienceOld are the two audiences these tests list in
	// BROKER_K8S_OIDC_AUDIENCE, the one the callers move to first, and podAudienceList is the
	// setting's value that lists them, white space between them.
	podAudienceNew  = "example-pod-new"
	podAudienceOld  = "example-pod-old"
	podAudienceList = podAudienceNew + " " + podAudienceOld
	// unlistedPodAudience is an audience of the cluster's tokens that no setting names.
	unlistedPodAudience = "example-pod-unlisted"
	// listExchangeAudience and listRegistrationAudience are the cluster's two other audiences the
	// list is set beside: the token exchange's (BROKER_EXCHANGE_K8S_AUDIENCE) and the registration
	// route's own (BROKER_POD_REGISTRATION_AUDIENCE). The NATS auth service's is the one
	// natsCalloutEnv sets.
	listExchangeAudience     = "example-exchange"
	listRegistrationAudience = "example-pod-registration"
)

// listedPodAudiences is what podAudienceList lists, in its order.
var listedPodAudiences = []string{podAudienceNew, podAudienceOld}

// clusterToken is account's projected token of audience, signed by cluster with key and bound to
// the pod uid, or to no pod where uid is empty, as mintPodToken's is. It is issued five seconds
// ago, as accountToken's is, so the routes that refuse a token living longer than an hour from
// its issue take it; an expired one was issued twenty minutes ago and expired ten minutes ago.
func clusterToken(t *testing.T, cluster *oidctest.Issuer, key *oidctest.Key, account, audience, uid string, expired bool) string {
	t.Helper()
	claims := cluster.Claims(account, audience)
	claims["iat"] = time.Now().Add(-5 * time.Second).Unix()
	if expired {
		claims["iat"], claims["exp"] = time.Now().Add(-20*time.Minute).Unix(), time.Now().Add(-10*time.Minute).Unix()
	}
	if uid != "" {
		claims["kubernetes.io"] = map[string]any{"pod": map[string]any{"uid": uid}}
	}
	return cluster.Mint(t, key, claims)
}

// tokenLoginAt posts a token login to the broker listening at addr, presenting token for
// registrarService under a request object signed by a fresh key, as podBroker.logIn signs one,
// and answers the broker's status and body: the login a refusal row reads, where logIn fails.
func tokenLoginAt(t *testing.T, addr, token string) (int, []byte) {
	t.Helper()
	base := "http://" + addr
	request, err := record.Sign(proofKey(t), base, []record.AuthorizationDetail{
		{Type: record.KindLauncherCredential, Identifier: "example-host-" + registrarService, Service: registrarService},
	}, "", "", nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return podBroker{t: t, base: base}.post("/v1/launcher-credentials/token", nil, map[string]any{"token": token, "request": request})
}

// podTokenKind is one kind of cluster token a row presents: of audience, and past its expiry
// where expired is set.
type podTokenKind struct {
	name     string
	audience string
	expired  bool
}

// readerRefusal is how a reader refused a token: the status, the refusal's code and its text.
type readerRefusal struct {
	status int
	code   string
	text   string
}

// readerWant is what a reader must answer a token: take it, where takes is set, or refuse it with
// status and code, in a text holding each of says.
type readerWant struct {
	takes  bool
	status int
	code   string
	says   []string
}

// podTokenReader is a reader of BROKER_K8S_OIDC_AUDIENCE: a route that verifies a cluster token,
// and how a row drives it.
type podTokenReader struct {
	name string
	// take presents a token of kind, which the reader must take, and fails t unless it does.
	take func(t *testing.T, kind podTokenKind)
	// refuse presents a token of kind and answers the reader's refusal.
	refuse func(t *testing.T, kind podTokenKind) readerRefusal
	// wants is what the reader must answer a token of kind, and whether a row asks it at all.
	wants func(kind podTokenKind) (readerWant, bool)
}

// listedAudienceReader is the wants of a reader of BROKER_K8S_OIDC_AUDIENCE: it takes a sound
// token of each listed audience, and of also where also is not empty; it refuses an expired token
// of a listed audience as expired, since a token is judged by the listed audience it carries, and
// every other token as invalid, an expired one of no listed audience included, since a token's
// audience is judged before its expiry.
func listedAudienceReader(expired, invalid readerWant, also string) func(podTokenKind) (readerWant, bool) {
	return func(kind podTokenKind) (readerWant, bool) {
		listed := slices.Contains(listedPodAudiences, kind.audience)
		switch {
		case !kind.expired && (listed || also != "" && kind.audience == also):
			return readerWant{takes: true}, true
		case listed:
			return expired, true
		}
		return invalid, true
	}
}

// refusalFromBody is a refusal the broker answered as bytes, in the JSON every refusal of the API
// is written in (brokerRefusal).
func refusalFromBody(status int, answer []byte) readerRefusal {
	code, text := brokerRefusal(answer)
	return readerRefusal{status: status, code: code, text: text}
}

// refusalFromJSON is a refusal the broker answered as decoded JSON.
func refusalFromJSON(status int, body map[string]any) readerRefusal {
	code, _ := body["code"].(string)
	text, _ := body["error"].(string)
	return readerRefusal{status: status, code: code, text: text}
}

// requireRefusal fails t unless got is want's refusal: its status, its code, and a text holding
// each of want.says. A 429 RATE_LIMITED is a bucket of the broker's this test spent past its own
// plan, which is the test's fault and never a refusal row: it fails, saying so.
func requireRefusal(t *testing.T, who string, got readerRefusal, want readerWant) {
	t.Helper()
	if got.status == http.StatusTooManyRequests {
		t.Fatalf("%s %d %s %q: a rate limit, which this test plans never to meet and which is no answer to the token", who, got.status, got.code, got.text)
	}
	if got.status != want.status || got.code != want.code {
		t.Fatalf("%s %d %s %q, want %d %s", who, got.status, got.code, got.text, want.status, want.code)
	}
	for _, say := range want.says {
		if !strings.Contains(got.text, say) {
			t.Fatalf("%s %d %s with the text %q, which does not hold %s", who, got.status, got.code, got.text, say)
		}
	}
}

// TestEveryPodTokenReaderTakesEachListedAudience is the inventory of the readers of
// BROKER_K8S_OIDC_AUDIENCE, on the real binary with the setting listing two audiences: each
// reader, declared below in one list, takes a token of each listed audience — the enrolment of a
// pod (M1) by a launcher logged in with it, the renewal of that pod (M2), the token login of
// BROKER_SERVICE_LOGINS (M3), the registrar's own token at the registration route and its release
// (M4), the pod token a registration carries (M5), which also takes the registration audience,
// Dispatch's call as a caller of BROKER_API_ACCOUNTS (M6) and the edge listener's GET
// /v1/project-repositories (M7). Each refuses an expired token of the second listed audience as
// expired, and a token of an audience nobody lists, sound or expired, and one of the exchange,
// registration or NATS audience, as invalid, each with the codes it answers one audience today; the
// token login's refusal names both listed audiences. The renewal refuses another pod's token of the
// second listed audience, and the enrolment one bound to no pod. The token exchange (X1) refuses a
// pod's token of each listed audience as minted for another audience and mints for the pod's token
// of its own. A broker with neither BROKER_K8S_OIDC_ISSUER nor BROKER_K8S_OIDC_AUDIENCE has no
// token login and no account tokens. The test fails for a member with no row. M1 to M7 and X1 are
// the plan's names for the readers (dispatch://INFRA-5514/artifact/plan-md).
//
// The token login spends its source address's bucket (a burst of 10) before it verifies the
// token, and its account's (a burst of 5) once the token verifies, and each broker process holds
// its own (api.DefaultLauncherLimits). So the broker of the readers makes two token logins, M1's,
// and M3 runs on a further broker of the same settings on a schema of its own, which makes eight,
// two of them accepted, so that its logins do not move the launcher credential M1's rows hold. A
// 429 is a failure of the row that meets it (requireRefusal), never a pass or a refusal.
func TestEveryPodTokenReaderTakesEachListedAudience(t *testing.T) {
	rig := newEdgeRig(t)
	nats := natsCalloutEnv(t, "edge-listener/edge-a/PRIV="+edgeListenerAccount)
	natsAudience := envSetting(t, nats, "BROKER_NATS_K8S_AUDIENCE")
	settings := rig.env(rig.access, nats, []string{
		// After the rig's own, one audience: os/exec keeps the last copy of a variable.
		"BROKER_K8S_OIDC_AUDIENCE=" + podAudienceList,
		"BROKER_SERVICE_LOGINS=" + registrarService + "=" + registrarAccount,
		// The account the enrolled pods run as, bound to the service the daemon logs in as.
		"BROKER_SERVICES=" + registrarService + "=" + podAccount,
		"BROKER_API_ACCOUNTS=dispatch=" + apiAccount,
		"BROKER_POD_REGISTRARS=" + registrarAccount + "=" + podAccount,
		"BROKER_POD_REGISTRATION_AUDIENCE=" + listRegistrationAudience,
		"BROKER_EXCHANGE_K8S_AUDIENCE=" + listExchangeAudience,
		// The registered pods' account, whose entry fixes no session, so a registration names
		// one; and the read entry the exchange's rows trade for, which fixes its session, so
		// its pod is registered for nothing (TestTheBinaryExchangesAReadEntrysPodTokenForOneThatOnlyReads).
		"BROKER_EXCHANGE_ACCOUNTS=" + podAccount + "=" + project + "=" + person,
		"BROKER_EXCHANGE_READ_ACCOUNTS=" + readerAccount + "=" + readerService + "=" + readerSession,
	})
	readers := startBroker(t, brokerOptions{Env: settings})
	// The token exchange is wired after the bind line startBroker waits for (Await).
	readers.AwaitRecord(t, "token exchange", map[string]any{"state": "on", "kubernetes": true})
	logins := startBroker(t, brokerOptions{Env: slices.Concat(settings, []string{"BROKER_DATABASE_URL=" + storetest.URL(t)})})
	base := "http://" + readers.Addr
	live := newLiveBroker(readers.Addr, proofKey(t))
	serial := 0
	next := func(prefix string) string {
		serial++
		return prefix + strconv.Itoa(serial)
	}
	tokenOf := func(t *testing.T, account string, kind podTokenKind, uid string) string {
		t.Helper()
		return clusterToken(t, rig.cluster, rig.key, account, kind.audience, uid, kind.expired)
	}
	// The registrar's own sound token of the first listed audience, which M4 holds the
	// registration route to take, so an M5 row is about the pod's token alone.
	registrarToken := func(t *testing.T) string {
		t.Helper()
		return clusterToken(t, rig.cluster, rig.key, registrarAccount, podAudienceNew, "", false)
	}

	// launcher is the daemon's launcher credential as M1's latest token login left it: a login
	// moves the account's one credential to its own key, so only the latest key proves it.
	var launcher podclient.Launcher
	// enrolled is the pod M1 enrolled with a token of each listed audience, which M2 renews.
	enrolled := map[string]podclient.Pod{}
	enrolledWith := func(t *testing.T, audience string) podclient.Pod {
		t.Helper()
		p, found := enrolled[audience]
		if !found {
			t.Fatalf("no pod was enrolled with a token of %s at M1, so there is no enrollment to renew", audience)
		}
		return p
	}
	renew := func(t *testing.T, p podclient.Pod, podToken string) (int, []byte) {
		t.Helper()
		return podBroker{t: t, base: base}.signed(pod{runtimeID: p.RuntimeID, key: p.Key, enrollmentID: p.EnrollmentID},
			"/v1/enrollments/"+p.EnrollmentID+"/renew", map[string]any{"pod_token": podToken})
	}
	enrolRefused := func(t *testing.T, token func(account, uid string) string) readerRefusal {
		t.Helper()
		if launcher.CredentialID == "" {
			t.Fatal("no launcher credential: M1's token logins with a token of a listed audience were refused, so no enrolment reaches the pod's token")
		}
		status, code := podclient.Pods{T: t, Base: base, Token: token}.EnrollRefused(launcher, next("example-pod-uid-"), podAccount)
		return readerRefusal{status: status, code: code}
	}
	const releasePath = "/v1/oauth/exchange/pod-sessions/release"
	pendingPath := "/v1/pending?approver=" + url.QueryEscape(person)
	const repositoriesPath = "/v1/project-repositories?project=PRIV"
	// The refusals of the routes that authenticate a caller's own token (serviceAuth,
	// accountAuth and edgeListenerAuth).
	accountExpired := readerWant{status: http.StatusUnauthorized, code: "ACCOUNT_TOKEN_EXPIRED"}
	accountInvalid := readerWant{status: http.StatusUnauthorized, code: "ACCOUNT_TOKEN_INVALID"}
	podExpired := readerWant{status: http.StatusForbidden, code: "POD_TOKEN_EXPIRED"}
	podMismatch := readerWant{status: http.StatusForbidden, code: "POD_IDENTITY_MISMATCH"}

	m1 := podTokenReader{
		name: "M1 enrolment",
		take: func(t *testing.T, kind podTokenKind) {
			pods := podclient.Pods{T: t, Base: base, Token: podclient.IssuerTokens(t, rig.cluster, rig.key, kind.audience)}
			launcher = pods.LogIn(registrarService, registrarAccount)
			enrolled[kind.audience] = pods.Enroll(launcher, next("example-pod-uid-"), podAccount)
		},
		refuse: func(t *testing.T, kind podTokenKind) readerRefusal {
			return enrolRefused(t, func(account, uid string) string { return tokenOf(t, account, kind, uid) })
		},
		wants: listedAudienceReader(podExpired, podMismatch, ""),
	}
	m2 := podTokenReader{
		name: "M2 renewal",
		take: func(t *testing.T, kind podTokenKind) {
			p := enrolledWith(t, kind.audience)
			if status, answer := renew(t, p, tokenOf(t, podAccount, kind, p.RuntimeID)); status != http.StatusOK {
				t.Fatalf("the pod enrolled with a token of %s renewed with its own token of it = %d %s, want 200", kind.audience, status, answer)
			}
		},
		refuse: func(t *testing.T, kind podTokenKind) readerRefusal {
			p := enrolledWith(t, podAudienceOld)
			return refusalFromBody(renew(t, p, tokenOf(t, podAccount, kind, p.RuntimeID)))
		},
		wants: listedAudienceReader(podExpired, podMismatch, ""),
	}
	m3 := podTokenReader{
		name: "M3 token login",
		take: func(t *testing.T, kind podTokenKind) {
			podBroker{t: t, base: "http://" + logins.Addr, issuer: rig.cluster, key: rig.key, audience: kind.audience}.logIn(registrarService, registrarAccount)
		},
		refuse: func(t *testing.T, kind podTokenKind) readerRefusal {
			return refusalFromBody(tokenLoginAt(t, logins.Addr, tokenOf(t, registrarAccount, kind, "")))
		},
		wants: listedAudienceReader(readerWant{status: http.StatusUnauthorized, code: "TOKEN_EXPIRED"},
			readerWant{status: http.StatusUnauthorized, code: "TOKEN_INVALID", says: []string{strconv.Quote(podAudienceNew), strconv.Quote(podAudienceOld)}}, ""),
	}
	m4 := podTokenReader{
		name: "M4 the registrar's own token",
		take: func(t *testing.T, kind podTokenKind) {
			bearer, session := tokenOf(t, registrarAccount, kind, ""), next("example-session-")
			podToken := clusterToken(t, rig.cluster, rig.key, podAccount, listRegistrationAudience, next("example-pod-uid-"), false)
			if status, body := live.registerPodSession(t, podToken, session, bearer); status != http.StatusOK || body["session"] != session {
				t.Fatalf("the registration by the registrar's token of %s = %d %v, want 200 naming %s", kind.audience, status, body, session)
			}
			if status, body := live.postJSON(t, releasePath, map[string]any{"sessions": []string{session}}, bearer); status != http.StatusOK {
				t.Fatalf("the release by the registrar's token of %s = %d %v, want 200", kind.audience, status, body)
			}
		},
		refuse: func(t *testing.T, kind podTokenKind) readerRefusal {
			bearer, session := tokenOf(t, registrarAccount, kind, ""), next("example-session-")
			podToken := clusterToken(t, rig.cluster, rig.key, podAccount, listRegistrationAudience, next("example-pod-uid-"), false)
			registered := refusalFromJSON(live.registerPodSession(t, podToken, session, bearer))
			released := refusalFromJSON(live.postJSON(t, releasePath, map[string]any{"sessions": []string{session}}, bearer))
			if released.status != registered.status || released.code != registered.code {
				t.Errorf("the registrar's token was answered %d %s by the release and %d %s by the registration: both routes are serviceAuth's, which answers one token alike at both",
					released.status, released.code, registered.status, registered.code)
			}
			return registered
		},
		wants: listedAudienceReader(accountExpired, accountInvalid, ""),
	}
	m5 := podTokenReader{
		name: "M5 the pod token a registration carries",
		take: func(t *testing.T, kind podTokenKind) {
			session := next("example-session-")
			if status, body := live.registerPodSession(t, tokenOf(t, podAccount, kind, next("example-pod-uid-")), session, registrarToken(t)); status != http.StatusOK || body["session"] != session {
				t.Fatalf("the registration of a pod by its token of %s = %d %v, want 200 naming %s", kind.audience, status, body, session)
			}
		},
		refuse: func(t *testing.T, kind podTokenKind) readerRefusal {
			return refusalFromJSON(live.registerPodSession(t, tokenOf(t, podAccount, kind, next("example-pod-uid-")), next("example-session-"), registrarToken(t)))
		},
		wants: listedAudienceReader(readerWant{status: http.StatusUnauthorized, code: "POD_TOKEN_EXPIRED"},
			readerWant{status: http.StatusUnauthorized, code: "POD_TOKEN_INVALID"}, listRegistrationAudience),
	}
	m6 := podTokenReader{
		name: "M6 a caller of BROKER_API_ACCOUNTS",
		take: func(t *testing.T, kind podTokenKind) {
			if status, body := live.callAs(t, http.MethodGet, pendingPath, tokenOf(t, apiAccount, kind, ""), "", ""); status != http.StatusOK {
				t.Fatalf("Dispatch's GET /v1/pending with its token of %s = %d %v, want 200", kind.audience, status, body)
			}
		},
		refuse: func(t *testing.T, kind podTokenKind) readerRefusal {
			return refusalFromJSON(live.callAs(t, http.MethodGet, pendingPath, tokenOf(t, apiAccount, kind, ""), "", ""))
		},
		wants: listedAudienceReader(accountExpired, accountInvalid, ""),
	}
	m7 := podTokenReader{
		name: "M7 an edge listener",
		take: func(t *testing.T, kind podTokenKind) {
			status, answer := live.callAs(t, http.MethodGet, repositoriesPath, tokenOf(t, edgeListenerAccount, kind, ""), "", "")
			requirePRIVsRepositories(t, "the edge listener of PRIV with its token of "+kind.audience, status, answer)
		},
		refuse: func(t *testing.T, kind podTokenKind) readerRefusal {
			return refusalFromJSON(live.callAs(t, http.MethodGet, repositoriesPath, tokenOf(t, edgeListenerAccount, kind, ""), "", ""))
		},
		wants: listedAudienceReader(accountExpired, accountInvalid, ""),
	}
	exchangeOf := func(t *testing.T, kind podTokenKind) (int, map[string]any) {
		t.Helper()
		return live.exchange(t, exchangeForm(tokenOf(t, readerAccount, kind, readerPodUID), ""))
	}
	x1 := podTokenReader{
		name: "X1 the token exchange",
		take: func(t *testing.T, kind podTokenKind) {
			status, body := exchangeOf(t, kind)
			if token, _ := body["access_token"].(string); status != http.StatusOK || token == "" {
				t.Fatalf("the exchange of the read entry's pod token of %s = %d %v, want 200 with an access token", kind.audience, status, body)
			}
		},
		refuse: func(t *testing.T, kind podTokenKind) readerRefusal {
			status, body := exchangeOf(t, kind)
			code, _ := body["error"].(string)
			text, _ := body["error_description"].(string)
			return readerRefusal{status: status, code: code, text: text}
		},
		// The exchange keeps its one audience: it mints for a sound token of it and refuses a
		// pod's sound token of each listed audience as minted for another.
		wants: func(kind podTokenKind) (readerWant, bool) {
			switch {
			case kind.expired:
				return readerWant{}, false
			case kind.audience == listExchangeAudience:
				return readerWant{takes: true}, true
			case slices.Contains(listedPodAudiences, kind.audience):
				return readerWant{status: http.StatusBadRequest, code: "invalid_request", says: []string{"not minted for " + listExchangeAudience}}, true
			}
			return readerWant{}, false
		},
	}
	// The readers of BROKER_K8S_OIDC_AUDIENCE, M1 to M7 and X1, in one list.
	members := []podTokenReader{m1, m2, m3, m4, m5, m6, m7, x1}

	rows := map[string]int{}
	row := func(member podTokenReader, name string, check func(t *testing.T)) {
		rows[member.name]++
		t.Run(member.name+": "+name, check)
	}
	// The sound tokens of the listed audiences come first: M1's rows enrol the pods M2's renew
	// and leave the launcher credential M1's refusal rows enrol with.
	for _, kind := range []podTokenKind{
		{name: "a sound token of the first listed audience", audience: podAudienceNew},
		{name: "a sound token of the second listed audience", audience: podAudienceOld},
		{name: "an expired token of the second listed audience", audience: podAudienceOld, expired: true},
		{name: "a sound token of an audience nobody lists", audience: unlistedPodAudience},
		{name: "an expired token of an audience nobody lists", audience: unlistedPodAudience, expired: true},
		{name: "a token of the exchange's audience", audience: listExchangeAudience},
		{name: "a token of the registration audience", audience: listRegistrationAudience},
		{name: "a token of the NATS audience", audience: natsAudience},
	} {
		for _, member := range members {
			want, asked := member.wants(kind)
			if !asked {
				continue
			}
			row(member, kind.name, func(t *testing.T) {
				if want.takes {
					member.take(t, kind)
					return
				}
				requireRefusal(t, member.name+" answered "+kind.name, member.refuse(t, kind), want)
			})
		}
	}
	// The pod UID is checked as before.
	row(m1, "a token of the second listed audience bound to no pod", func(t *testing.T) {
		got := enrolRefused(t, func(account, _ string) string {
			return clusterToken(t, rig.cluster, rig.key, account, podAudienceOld, "", false)
		})
		requireRefusal(t, "an enrolment with a pod token bound to no pod answered", got, podMismatch)
	})
	row(m2, "the token of the second listed audience of another pod of the same account", func(t *testing.T) {
		p := enrolledWith(t, podAudienceOld)
		got := refusalFromBody(renew(t, p, clusterToken(t, rig.cluster, rig.key, podAccount, podAudienceOld, next("example-pod-uid-"), false)))
		requireRefusal(t, "a renewal with another pod's token answered", got, podMismatch)
	})
	for _, member := range members {
		if rows[member.name] == 0 {
			t.Errorf("%s, a reader of BROKER_K8S_OIDC_AUDIENCE, has no row", member.name)
		}
	}

	t.Run("neither setting", func(t *testing.T) {
		off := startBroker(t, brokerOptions{Env: []string{
			"BROKER_DATABASE_URL=" + storetest.URL(t),
			"BROKER_LISTEN_ADDR=127.0.0.1:0",
			"BROKER_PUBLIC_URL=http://127.0.0.1:0",
			"BROKER_FAKE_SECRETS_FILE=" + emptySecretsFile(t),
		}})
		requireRefusal(t, "a broker with neither setting answered the daemon's token login",
			refusalFromBody(tokenLoginAt(t, off.Addr, clusterToken(t, rig.cluster, rig.key, registrarAccount, podAudienceNew, "", false))),
			readerWant{status: http.StatusNotFound, code: "TOKEN_LOGIN_OFF"})
		requireRefusal(t, "a broker with neither setting answered Dispatch's GET /v1/pending",
			refusalFromJSON(newPlainBroker(off.Addr).callAs(t, http.MethodGet, pendingPath, clusterToken(t, rig.cluster, rig.key, apiAccount, podAudienceNew, "", false), "", "")),
			readerWant{status: http.StatusUnauthorized, code: "ACCOUNT_TOKENS_OFF"})
	})
}

// TestTheBrokerBinaryRefusesAPodAudienceListAtStart drives the real binary with each
// BROKER_K8S_OIDC_AUDIENCE it must refuse, with BROKER_K8S_OIDC_ISSUER set, and asserts it exits
// non-zero without listening, its one fatal line naming what the row says: white space alone
// names the setting; an audience listed twice names the setting, the audience and the word
// "twice"; and a listed audience equal to BROKER_EXCHANGE_K8S_AUDIENCE, to
// BROKER_POD_REGISTRATION_AUDIENCE or to BROKER_NATS_K8S_AUDIENCE names both settings, the
// exchange's row with that one audience alone listed, since a token of it would be spendable on
// both settings' routes. The database it is given is unreachable, so a broker that took the list
// fails on the host instead of on the setting.
func TestTheBrokerBinaryRefusesAPodAudienceListAtStart(t *testing.T) {
	binary := buildBroker(t)
	nats := natsCalloutEnv(t, "edge-listener/edge-a/PRIV="+edgeListenerAccount)
	natsAudience := envSetting(t, nats, "BROKER_NATS_K8S_AUDIENCE")
	for _, row := range []struct {
		name, audiences string
		more            []string
		names           []string
	}{
		{"white space alone", " \t\n ", nil,
			[]string{"BROKER_K8S_OIDC_AUDIENCE"}},
		{"an audience listed twice", podAudienceNew + " " + podAudienceNew, nil,
			[]string{"BROKER_K8S_OIDC_AUDIENCE", podAudienceNew, "twice"}},
		{"one listed audience equal to the exchange's", listExchangeAudience, []string{"BROKER_EXCHANGE_K8S_AUDIENCE=" + listExchangeAudience},
			[]string{"BROKER_K8S_OIDC_AUDIENCE", "BROKER_EXCHANGE_K8S_AUDIENCE"}},
		{"the second listed audience equal to the registration audience", podAudienceNew + " " + listRegistrationAudience,
			[]string{"BROKER_POD_REGISTRATION_AUDIENCE=" + listRegistrationAudience},
			[]string{"BROKER_K8S_OIDC_AUDIENCE", "BROKER_POD_REGISTRATION_AUDIENCE"}},
		{"the second listed audience equal to the NATS audience", podAudienceNew + " " + natsAudience, nats,
			[]string{"BROKER_K8S_OIDC_AUDIENCE", "BROKER_NATS_K8S_AUDIENCE"}},
	} {
		t.Run(row.name, func(t *testing.T) {
			out := runRefusedBroker(t, binary, append([]string{
				"BROKER_DATABASE_URL=postgres://nonexistent-host-this-test-must-never-reach/db",
				"BROKER_PUBLIC_URL=https://secrets.internal.example",
				"BROKER_K8S_OIDC_ISSUER=https://cluster.example",
				"BROKER_K8S_OIDC_AUDIENCE=" + row.audiences,
			}, row.more...)...)
			refusals := recordsWith(out, "broker: fatal", nil)
			if len(refusals) != 1 {
				t.Fatalf("the broker logged %d %q lines, want one:\n%s", len(refusals), "broker: fatal", out)
			}
			refusal, _ := refusals[0]["error"].(string)
			for _, want := range row.names {
				if !strings.Contains(refusal, want) {
					t.Fatalf("the broker refused with %q, which does not name %s", refusal, want)
				}
			}
		})
	}
}

// TestTheBrokerBuildsOnePodVerifierForTheListAndKeepsTheNATSAudienceApart: with
// BROKER_K8S_OIDC_AUDIENCE listing two audiences and the NATS auth service on, the boot reads the
// cluster issuer's key set once, through the broker's own issuerKeys, and builds every verifier of
// the cluster's tokens on that one set (found.K8sKeys), which the issuer then serves once for all
// of them: the pod verifier takes a token of each listed audience and refuses one of the NATS
// audience, and the NATS auth service's verifier (natsCalloutBoot's PlatformTokens), which
// holdRoutes hands the NATS writer hold as its operator's token check, refuses a token of each
// listed audience and takes one of its own. With neither BROKER_K8S_OIDC_ISSUER nor
// BROKER_K8S_OIDC_AUDIENCE the boot reads no key set and builds no pod verifier.
func TestTheBrokerBuildsOnePodVerifierForTheListAndKeepsTheNATSAudienceApart(t *testing.T) {
	const operatorAccount = "system:serviceaccount:envoy-operator:envoy-operator"
	cluster := oidctest.New(t)
	key := cluster.PublishKey(t, "signing-key")
	nats := natsCalloutEnv(t, "edge-listener/edge-a/PRIV="+edgeListenerAccount+" operator/-/*="+operatorAccount)
	natsAudience := envSetting(t, nats, "BROKER_NATS_K8S_AUDIENCE")
	// load is config.Load of a whole configuration: productionEnv's, a database and an address
	// nothing reaches, and each of settings.
	load := func(t *testing.T, settings ...[]string) config.Config {
		t.Helper()
		env := map[string]string{"BROKER_DATABASE_URL": "postgres://unused", "BROKER_PUBLIC_URL": "https://secrets.internal.example"}
		for _, setting := range slices.Concat(append([][]string{productionEnv(t)}, settings...)...) {
			name, value, _ := strings.Cut(setting, "=")
			env[name] = value
		}
		cfg, err := config.Load(func(name string) string { return env[name] })
		if err != nil {
			t.Fatalf("config.Load: %v", err)
		}
		return cfg
	}
	// readKeys is issuerKeys, the broker's own read of an issuer's key set, recording each issuer
	// it is asked for and the set it answered.
	var asked []string
	var read *oidc.Keys
	readKeys := func(ctx context.Context, issuer string) (*oidc.Keys, error) {
		asked = append(asked, issuer)
		keys, err := issuerKeys(ctx, issuer)
		read = keys
		return keys, err
	}

	t.Run("two listed audiences beside the NATS auth service", func(t *testing.T) {
		asked, read = nil, nil
		cfg := load(t, []string{"BROKER_K8S_OIDC_ISSUER=" + cluster.URL(), "BROKER_K8S_OIDC_AUDIENCE=" + podAudienceList}, nats)
		found, err := discoverVerifiers(t.Context(), cfg, readKeys)
		if err != nil {
			t.Fatal(err)
		}
		tokens, err := natsCalloutBoot(t.Context(), cfg, found, readKeys)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(asked, []string{cluster.URL()}) || found.K8sKeys == nil || found.K8sKeys != read {
			t.Fatalf("the boot read the key sets of %v and holds %p as the cluster's, want the cluster's issuer read once and the set it answered, %p", asked, found.K8sKeys, read)
		}
		if found.Pod == nil || tokens.K8s == nil {
			t.Fatalf("the boot built pod verifier %#v and NATS verifier %#v, want both", found.Pod, tokens.K8s)
		}
		hold := holdRoutes(cfg, &store.Store{}, "public", cfg.NATS.Services, tokens, nil)
		if hold.OperatorToken == nil || hold.OperatorAccount != operatorAccount {
			t.Fatalf("the NATS writer hold was handed the operator %q and a token check %v, want %s with the NATS auth service's", hold.OperatorAccount, hold.OperatorToken != nil, operatorAccount)
		}
		mint := func(audience string) string {
			return clusterToken(t, cluster, key, operatorAccount, audience, "", false)
		}
		for _, audience := range listedPodAudiences {
			if claims, err := found.Pod.VerifyAccount(t.Context(), mint(audience)); err != nil || claims.Subject != operatorAccount {
				t.Errorf("the pod verifier answered a token of the listed audience %s %+v, %v; want %s's claims", audience, claims, err, operatorAccount)
			}
			if _, _, _, _, err := tokens.Verify(t.Context(), mint(audience)); !errors.Is(err, oidc.ErrAudience) {
				t.Errorf("the NATS auth service's verifier answered a token of the listed audience %s %v, want oidc.ErrAudience", audience, err)
			}
			if _, _, _, _, err := hold.OperatorToken(t.Context(), mint(audience)); !errors.Is(err, oidc.ErrAudience) {
				t.Errorf("the NATS writer hold's token check answered a token of the listed audience %s %v, want oidc.ErrAudience", audience, err)
			}
		}
		if _, err := found.Pod.VerifyAccount(t.Context(), mint(natsAudience)); !errors.Is(err, oidc.ErrAudience) {
			t.Errorf("the pod verifier answered a token of the NATS audience %s %v, want oidc.ErrAudience", natsAudience, err)
		}
		if account, _, _, _, err := tokens.Verify(t.Context(), mint(natsAudience)); err != nil || account != operatorAccount {
			t.Errorf("the NATS auth service's verifier answered a token of its own audience %q, %v; want %s", account, err, operatorAccount)
		}
		if account, _, _, _, err := hold.OperatorToken(t.Context(), mint(natsAudience)); err != nil || account != operatorAccount {
			t.Errorf("the NATS writer hold's token check answered a token of the NATS audience %q, %v; want %s", account, err, operatorAccount)
		}
		if discovery, sets := cluster.DiscoveryRequests(), cluster.JWKSFetches(); discovery != 1 || sets != 1 {
			t.Errorf("the issuer served %d discovery documents and %d key sets, want one of each: every verifier reads the one key set the boot read", discovery, sets)
		}
	})

	t.Run("neither setting", func(t *testing.T) {
		asked, read = nil, nil
		found, err := discoverVerifiers(t.Context(), load(t), readKeys)
		if err != nil {
			t.Fatal(err)
		}
		if found.Pod != nil || found.K8sKeys != nil || len(asked) != 0 {
			t.Fatalf("with neither setting the boot read the key sets of %v and built pod verifier %#v on %p, want none", asked, found.Pod, found.K8sKeys)
		}
	})
}
