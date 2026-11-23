// cross_daemon_exchange_test.go drives the REAL compiled broker over HTTP, on a real Postgres,
// against a local OpenID issuer standing in for the cluster's, with two deployments' daemons
// listed for one pod account: the session name the first daemon's pods trade under is not the
// second daemon's to register, and a pod whose registration under it the second daemon is refused
// trades nothing under it.
package main

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
	"github.com/sjawhar/envoy/internal/cipher/store/storetest"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

// secondRegistrarAccount is the second daemon's login account. The broker this file starts lists
// it in BROKER_SERVICE_LOGINS, logging in as legion-daemon-b, and in BROKER_POD_REGISTRARS as a
// registrar of podAccount's pods, beside registrarAccount, the first daemon's.
const secondRegistrarAccount = "system:serviceaccount:example-ns:example-daemon-b"

// TestAPodAnotherDaemonRegisteredCannotTradeForASessionThisDaemonsPodHolds is the live proof that a
// session name stays with the daemon whose pods trade under it. Daemon A (registrarAccount)
// registers pod a-1 for S, and a-1 trades for a token the listed API account reads back live under
// S; this is the test's precondition, and a failure of it is reported beginning UNKNOWN:, since
// the test then says nothing of the rule it is about. A then registers a second pod, a-2, for S, as
// a relaunch does, and a-2 trades for a token read back live under S. Daemon B
// (secondRegistrarAccount), listed for the same pod account and logging in as a service of its
// own, registering its pod b-1 for S is refused 409 SESSION_NAME_HELD_BY_ANOTHER_REGISTRAR, so it
// writes nothing, and b-1's trade under S is refused as the trade of a pod no registration names:
// 400 invalid_grant, its description beginning pod_session_not_registered:, with no access token.
// All three pods run as podAccount, one principal at the exchange, so the claim on the name
// refuses none of them: what refuses b-1 is that S is A's. Every failure names S.
func TestAPodAnotherDaemonRegisteredCannotTradeForASessionThisDaemonsPodHolds(t *testing.T) {
	const session = "example-cross-daemon-session"
	databaseURL := storetest.URL(t)
	cluster, github := oidctest.New(t), oidctest.New(t)
	clusterKey := cluster.PublishKey(t, "cluster-key")
	github.PublishKey(t, "github-key")
	// os/exec keeps the last value of a key given twice, so these two override exchangeEnv's
	// own, which list registrarAccount alone.
	env := append(exchangeEnv(t, databaseURL, cluster, github, longAccessSeconds),
		"BROKER_SERVICE_LOGINS="+registrarService+"="+registrarAccount+" legion-daemon-b="+secondRegistrarAccount,
		"BROKER_POD_REGISTRARS="+registrarAccount+"="+podAccount+" "+secondRegistrarAccount+"="+podAccount)
	broker := startBroker(t, brokerOptions{Env: env})
	broker.Await(t, "token exchange")
	live := newLiveBroker(broker.Addr, proofKey(t))
	apiToken := accountToken(t, cluster, clusterKey, apiAccount)
	daemonA := accountToken(t, cluster, clusterKey, registrarAccount)
	daemonB := accountToken(t, cluster, clusterKey, secondRegistrarAccount)
	// Each pod's two tokens: the enrolment-audience one its registration names it by, and the
	// exchange-audience one it trades itself, both bound to the one pod uid.
	enrolment := func(uid string) string {
		return mintPodToken(t, cluster, clusterKey, podAccount, enrolmentAudience, uid)
	}
	subject := func(uid string) string {
		return mintPodToken(t, cluster, clusterKey, podAccount, exchangeAudience, uid)
	}
	// trades answers "" when the pod uid's trade under the session is answered 200 with a token
	// the API account reads back live under the session, and otherwise what was answered instead,
	// with no token in it.
	trades := func(uid string) string {
		status, body := live.exchange(t, exchangeForm(subject(uid), session))
		token, _ := body["access_token"].(string)
		if status != http.StatusOK || token == "" {
			return fmt.Sprintf("the trade answered %d %v %q and %d bytes of access token", status, body["error"], body["error_description"], len(token))
		}
		status, body = live.introspect(t, token, apiToken)
		if status != http.StatusOK || body["active"] != true || body["session"] != session {
			return fmt.Sprintf("the introspection of its token answered %d %v", status, body)
		}
		return ""
	}

	if status, body := live.registerPodSession(t, enrolment("a-1"), session, daemonA); status != http.StatusOK || body["session"] != session {
		t.Fatalf("UNKNOWN: %s's registration of a-1 for %s = %d %v, want 200 naming %s", registrarAccount, session, status, body, session)
	}
	if failed := trades("a-1"); failed != "" {
		t.Fatalf("UNKNOWN: a-1, which %s registered for %s, does not trade for a token read back live under %s: %s", registrarAccount, session, session, failed)
	}

	if status, body := live.registerPodSession(t, enrolment("a-2"), session, daemonA); status != http.StatusOK || body["session"] != session {
		t.Fatalf("%s's registration of a second pod, a-2, for %s = %d %v, want 200 naming %s: the daemon holding a name registers the pod that replaces another under it", registrarAccount, session, status, body, session)
	}
	if failed := trades("a-2"); failed != "" {
		t.Fatalf("a-2, which %s registered for %s, does not trade for a token read back live under %s: %s", registrarAccount, session, session, failed)
	}

	// Each check below reports and goes on, so a registration wrongly admitted also shows whether
	// its pod then trades.
	status, body := live.registerPodSession(t, enrolment("b-1"), session, daemonB)
	t.Logf("%s's registration of b-1 for %s = %d %v", secondRegistrarAccount, session, status, body)
	if status != http.StatusConflict || body["code"] != "SESSION_NAME_HELD_BY_ANOTHER_REGISTRAR" {
		t.Errorf("%s, listed for %s, registering b-1 for %s, which %s's pods trade under = %d %v; want 409 SESSION_NAME_HELD_BY_ANOTHER_REGISTRAR: %s is %s's",
			secondRegistrarAccount, podAccount, session, registrarAccount, status, body, session, registrarAccount)
	}
	status, body = live.exchange(t, exchangeForm(subject("b-1"), session))
	description, _ := body["error_description"].(string)
	if status != http.StatusBadRequest || body["error"] != "invalid_grant" || !strings.HasPrefix(description, oauthwire.PodSessionNotRegistered+":") || body["access_token"] != nil {
		t.Errorf("b-1's trade under %s, a name %s holds, after %s was refused its registration = %d %v %q with an access token %t; want 400 invalid_grant, its description beginning %s:, and no access token",
			session, registrarAccount, secondRegistrarAccount, status, body["error"], description, body["access_token"] != nil, oauthwire.PodSessionNotRegistered)
	}
}
