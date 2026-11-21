package main

import (
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/broker/projectaccess/projectaccesstest"
	"github.com/sjawhar/envoy/internal/broker/store/storetest"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

// brokerAccessAccount is the broker's own Kubernetes account in the rows that ask Dispatch: the
// subject and the service of its read entry, which the Dispatch stand-in admits as Dispatch admits
// the account its DISPATCH_ACCESS_ACCOUNTS names (projectaccesstest.AccessService).
const brokerAccessAccount = projectaccesstest.AccessService

// brokerReadEntry is the broker's own read entry as BROKER_EXCHANGE_READ_ACCOUNTS spells one: its
// account as the subject and as the token's service, and the session it fixes.
const brokerReadEntry = brokerAccessAccount + "=" + brokerAccessAccount + "=" + projectaccesstest.AccessSession

// brokerAccessFile writes the broker's own projected token for audience, the exchange's, bound to a
// pod and signed by issuer with key, into a file of t's own, as the kubelet mounts the file
// BROKER_PROJECT_ACCESS_TOKEN_FILE names: the token the broker trades at its own token endpoint
// for its read token, and never presents to Dispatch.
func brokerAccessFile(t *testing.T, issuer *oidctest.Issuer, key *oidctest.Key, audience string) string {
	t.Helper()
	claims := issuer.Claims(brokerAccessAccount, audience)
	claims["kubernetes.io"] = map[string]any{"pod": map[string]any{"uid": "broker-pod-uid"}}
	return writeTempFile(t, "broker-exchange-token", []byte(issuer.Mint(t, key, claims)+"\n"))
}

// introspectAt points the Dispatch stand-in's check of the broker's read token at broker, as
// Dispatch asks the broker its DISPATCH_AGENT_SECRETS_URL names, presenting token, a projected
// token of an account the broker's BROKER_API_ACCOUNTS lists, as Dispatch presents its own there.
func introspectAt(t *testing.T, dispatch *projectaccesstest.Server, broker *brokerProcess, token string) {
	t.Helper()
	dispatch.IntrospectAt(t, "http://"+broker.Addr, introspectionAccount(token))
}

// introspectionAccount is a brokertoken.Account that never rotates.
type introspectionAccount string

func (a introspectionAccount) Read() (string, error) { return string(a), nil }

// readTokenTraded is the line the broker's read token logs at each trade that lands
// (brokerclient's own), whose identity names the projected token file it traded.
const readTokenTraded = "traded an identity for one of the broker's access tokens"

// TestPersonAccessStartsAfterBrokerServes (the broker starts to trade only after it serves): the
// compiled broker with BROKER_PROJECT_ACCESS_URL set trades for its own read token at its own token
// endpoint once its server serves, and that first trade lands, from the projected token in
// BROKER_PROJECT_ACCESS_TOKEN_FILE, with no failed trade before it: a starting broker never waits on
// itself. A trade that asks Dispatch then presents that read token under the DPoP scheme.
//
// Control: main trading for the read token before srv.Serve (readToken.Token(serving) above the
// serving goroutine): the trade waits on a socket nothing reads for brokerclient's 30-second trade
// bound, the boot stalls past awaitTimeout, and the first trade fails.
func TestPersonAccessStartsAfterBrokerServes(t *testing.T) {
	databaseURL := storetest.URL(t)
	cluster, github := oidctest.New(t), oidctest.New(t)
	clusterKey := cluster.PublishKey(t, "cluster-key")
	github.PublishKey(t, "github-key")
	dispatch := projectAccessStandIn(t)
	broker := startBroker(t, brokerOptions{Env: projectAccessEnv(t, databaseURL, cluster, github, clusterKey, dispatch)})
	traded := broker.AwaitRecord(t, readTokenTraded, nil)
	if identity, _ := traded["identity"].(string); !strings.Contains(identity, "broker-exchange-token") {
		t.Fatalf("the broker's first trade traded %q, want its projected token from BROKER_PROJECT_ACCESS_TOKEN_FILE", identity)
	}
	if failed := recordsWith(broker.Log(), "a trade got no usable answer from the secrets broker; asking again", nil); len(failed) != 0 {
		t.Fatalf("the broker logged a failed trade before its first landed: %v", failed)
	}

	apiToken := accountToken(t, cluster, clusterKey, apiAccount)
	introspectAt(t, dispatch, broker, apiToken)
	live, access := devBoxSession(t, broker.Addr, apiToken, fullMember, "example-host-erin-serving")
	if status, body := live.exchange(t, machineTrade(access, "erin-serving-1", publicProject)); status != http.StatusOK {
		t.Fatalf("%s's trade naming %s = %d %v, want a token: Dispatch is asked whether she sees it", fullMember, publicProject, status, body)
	}
	presented := dispatch.Presentations()
	if len(presented) == 0 {
		t.Fatal("the trade naming a project asked the Dispatch stand-in nothing")
	}
	for i, p := range presented {
		if !strings.HasPrefix(p.Authorization, "DPoP ") || p.Proof == "" {
			t.Errorf("call %d to Dispatch presented %q, want the broker's read token under the DPoP scheme with a proof", i+1, p.Authorization)
		}
	}
}
