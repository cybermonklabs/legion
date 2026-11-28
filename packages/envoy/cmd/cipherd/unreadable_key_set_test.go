package main

import (
	"encoding/json"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/api/unreadablekeysettest"
	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
	"github.com/sjawhar/envoy/internal/cipher/proof"
	"github.com/sjawhar/envoy/internal/cipher/record"
	"github.com/sjawhar/envoy/internal/cipher/store/storetest"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

// TestTheBinaryAnswersAnUnreadableKeySet503WithItsRetryAfter is the outage of a token issuer's key
// set on the REAL compiled broker, on a real Postgres, over a local OpenID issuer standing in for
// the cluster's whose discovery document reads and whose key set answers 503 from the start. The
// boot reads discovery alone (issuerKeys), so the broker starts, and its first check of a cluster
// token is its first read of the set. Three requests a deployment makes are each answered 503
// with Retry-After 5, the spacing of a key set's failed reads, and with the very answer of its
// shape that every client's test replays (unreadablekeysettest): a listed caller's introspection
// of a live token and the daemon's token login, two JSON routes, and a pod's token exchange, the
// token endpoint. None of them is a refusal of a token that may be sound. Once the set answers
// again and the 5 s those answers named have passed, the same three requests are answered as they
// are today: the token's introspection, a DPoP-bound access token, and a launcher credential.
//
// The key set reads the wall clock and no setting moves its spacing, so the wait is a real 5 s.
// It skips when BROKER_TEST_DATABASE_URL is unset, as every Postgres-backed broker test does.
func TestTheBinaryAnswersAnUnreadableKeySet503WithItsRetryAfter(t *testing.T) {
	databaseURL := storetest.URL(t)
	cluster, github := oidctest.New(t), oidctest.New(t)
	clusterKey := cluster.PublishKey(t, "cluster-key")
	githubKey := github.PublishKey(t, "github-key")
	cluster.FailKeys(http.StatusServiceUnavailable)

	key := proofKey(t)
	jkt, err := proof.Thumbprint(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	broker := startBroker(t, brokerOptions{Env: exchangeEnv(t, databaseURL, cluster, github, longAccessSeconds)})
	broker.AwaitRecord(t, "token exchange", map[string]any{"state": "on"})
	live := newLiveBroker(broker.Addr, key)

	// The live token the introspection reads back: a workflow run's trade, which checks its token
	// against GitHub's key set alone, so the cluster's outage does not reach it.
	status, body := live.exchange(t, runForm(mintRunToken(t, github, githubKey, releaseWorkflow, "refs/heads/main", "7"), "ci-release"))
	runToken, _ := body["access_token"].(string)
	if status != http.StatusOK || runToken == "" {
		t.Fatalf("the exchange of a run of %s while the cluster's key set cannot be read = %d %v, want 200 with an access token: that trade reads GitHub's key set, not the cluster's", releaseWorkflow, status, body)
	}
	if fetches := cluster.JWKSFetches(); fetches != 0 {
		t.Fatalf("the cluster issuer served %d key-set reads before the first check of one of its tokens, want none: the boot reads its discovery document alone, so the first check below is the first read of the set", fetches)
	}

	apiToken := accountToken(t, cluster, clusterKey, apiAccount)
	registrarToken := accountToken(t, cluster, clusterKey, registrarAccount)
	podToken := mintPodToken(t, cluster, clusterKey, podAccount, exchangeAudience, firstPodUID)
	answers := unreadablekeysettest.Read(t)
	rows := []struct {
		name   string
		client *http.Client
		// call is the request, built again for each send: the token login's request object is
		// signed when it is sent.
		call func(t *testing.T) brokerCall
		// outage is the broker's answer of the route's shape while the key set cannot be read,
		// as every client's test replays it.
		outage unreadablekeysettest.Answer
		// served fails t unless status and body are what the route answers today.
		served func(t *testing.T, status int, body []byte)
	}{
		{
			name:   "a listed caller's introspection of a live token",
			client: live.plain,
			call: func(*testing.T) brokerCall {
				return formCall("/v1/oauth/introspect", url.Values{"token": {runToken}}, apiToken)
			},
			outage: answers.JSONRoute,
			served: func(t *testing.T, status int, body []byte) {
				var answer oauthwire.Introspection
				if err := json.Unmarshal(body, &answer); err != nil || status != http.StatusOK {
					t.Fatalf("the introspection = %d %s (%v), want 200 with the token's introspection", status, body, err)
				}
				want := oauthwire.Introspection{
					Active: true, ClientID: oauthwire.ExchangeClientID, Thumbprint: jkt, Session: "ci-release", Person: person,
					ProjectScope: oauthwire.ProjectScopeNamed, Projects: []string{project}, ExpiresAt: answer.ExpiresAt,
				}
				if !reflect.DeepEqual(answer, want) || answer.ExpiresAt <= time.Now().Unix() {
					t.Fatalf("the introspection answered %+v, want %+v with an expiry in the future: the run's token, live, read back whole", answer, want)
				}
			},
		},
		{
			name:   "a pod's token exchange",
			client: live.dpop,
			call: func(*testing.T) brokerCall {
				return formCall("/v1/oauth/token", exchangeForm(podToken, heldSession), "")
			},
			outage: answers.TokenEndpoint,
			served: func(t *testing.T, status int, body []byte) {
				var answer map[string]any
				if err := json.Unmarshal(body, &answer); err != nil {
					t.Fatalf("the exchange answered %d %s, which is not JSON: %v", status, body, err)
				}
				if accessToken, _ := answer["access_token"].(string); status != http.StatusOK || accessToken == "" ||
					answer["token_type"] != "DPoP" || answer["issued_token_type"] != oauthwire.SubjectTokenTypeAccessToken {
					t.Fatalf("the exchange of %s's pod token = %d %v, want 200 with a DPoP-bound access token of type %s", podAccount, status, answer, oauthwire.SubjectTokenTypeAccessToken)
				}
			},
		},
		{
			name:   "the daemon's token login",
			client: live.plain,
			call: func(t *testing.T) brokerCall {
				request, err := record.Sign(proofKey(t), live.base, []record.AuthorizationDetail{
					{Type: record.KindLauncherCredential, Identifier: "example-host-" + registrarService, Service: registrarService},
				}, "", "", nil, time.Now())
				if err != nil {
					t.Fatal(err)
				}
				return jsonCall(t, "/v1/launcher-credentials/token", map[string]any{"token": registrarToken, "request": request}, "")
			},
			outage: answers.JSONRoute,
			served: func(t *testing.T, status int, body []byte) {
				var login struct {
					CredentialID string `json:"credential_id"`
				}
				if err := json.Unmarshal(body, &login); err != nil || status != http.StatusOK || login.CredentialID == "" {
					t.Fatalf("%s's token login as %s = %d %s (%v), want 200 with a credential_id", registrarAccount, registrarService, status, body, err)
				}
			},
		},
	}

	// The moment the last outage answer arrived, and the longest wait an answer named.
	var answered time.Time
	var wait time.Duration
	for _, row := range rows {
		t.Run("while the cluster's key set cannot be read, "+row.name+" is answered 503 with Retry-After 5", func(t *testing.T) {
			status, header, body := live.sendWithHeader(t, row.client, row.call(t))
			answered = time.Now()
			got := unreadablekeysettest.Captured(t, row.outage.Request, status, header, body)
			if !reflect.DeepEqual(got, row.outage) {
				t.Fatalf("the answer = %d, Retry-After %q, Content-Type %q, %s; want %d, Retry-After %q, Content-Type %q, %s: the answer the clients replay, since the check reached no verdict on the token",
					got.Status, got.RetryAfter, got.ContentType, got.Body, row.outage.Status, row.outage.RetryAfter, row.outage.ContentType, row.outage.Body)
			}
			seconds, err := strconv.Atoi(got.RetryAfter)
			if err != nil {
				t.Fatalf("Retry-After %q is no number of seconds: %v", got.RetryAfter, err)
			}
			wait = max(wait, time.Duration(seconds)*time.Second)
		})
	}
	if t.Failed() {
		return
	}

	// The set answers again, and the requests are sent once the wait the outage answers named has
	// passed since the last of them, as a client that waits out Retry-After sends its next one.
	// Every failed read of the set ended before that answer was sent, so the broker reads the set
	// again for the first request below.
	cluster.FailKeys(0)
	time.Sleep(time.Until(answered.Add(wait)))
	// The deployment's daemon registers the pod for its session, as it does before the pod's first
	// trade, a step the outage would have answered 503 too.
	live.registered(t, mintPodToken(t, cluster, clusterKey, podAccount, enrolmentAudience, firstPodUID), heldSession, registrarToken)
	for _, row := range rows {
		t.Run("once the set answers again and Retry-After has passed, "+row.name+" is answered as today", func(t *testing.T) {
			status, body := live.send(t, row.client, row.call(t))
			row.served(t, status, body)
		})
	}
}
