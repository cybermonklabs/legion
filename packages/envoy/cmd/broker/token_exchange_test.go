// token_exchange_test.go drives the REAL compiled broker's RFC 8693 token exchange and its RFC
// 7662 introspection route over HTTP, on a real Postgres, against a local OpenID issuer standing
// in for the cluster's: a pod carrying nothing but its projected service-account token and a key
// of its own is answered a Dispatch access token; a listed service account reads that token back;
// a role whose token's life has passed renews with the standing token it still holds; and each
// way in that must not work is refused. Nothing here is a unit test — the binary is the one
// cmd/broker builds, the schema is a fresh one on the test server, and every request crosses a
// socket.
package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/broker/brokertest"
	"github.com/sjawhar/envoy/internal/broker/exchange"
	"github.com/sjawhar/envoy/internal/broker/oauthwire"
	"github.com/sjawhar/envoy/internal/broker/projectaccess"
	"github.com/sjawhar/envoy/internal/broker/projectaccess/projectaccesstest"
	"github.com/sjawhar/envoy/internal/broker/proof"
	"github.com/sjawhar/envoy/internal/broker/store/storetest"
	"github.com/sjawhar/envoy/internal/brokerclient"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

const (
	// exchangeGrantType is RFC 8693's own grant type, which the library spells in a package its
	// users cannot import.
	exchangeGrantType = "urn:ietf:params:oauth:grant-type:token-exchange"
	// podAccount is the identity BROKER_EXCHANGE_ACCOUNTS lists here whose pods the test drives,
	// with the project and the person its entry gives it; listedAccount is listed too, for another
	// project and another person; otherAccount is listed nowhere; apiAccount is the listed caller
	// of the introspection route, BROKER_API_ACCOUNTS' own; registrarAccount is the deployment's
	// own daemon, which BROKER_SERVICE_LOGINS lets log in as registrarService, which
	// BROKER_POD_REGISTRARS lists as the registrar of podAccount's and listedAccount's pods, and
	// which registers the pods it starts under their session names.
	podAccount       = "system:serviceaccount:example-ns:example-worker"
	listedAccount    = "system:serviceaccount:other-ns:example-worker"
	otherAccount     = "system:serviceaccount:example-ns:example-other"
	apiAccount       = "system:serviceaccount:example-ns:example-dispatch"
	registrarAccount = "system:serviceaccount:example-ns:example-daemon"
	registrarService = "legion-daemon"
	person           = "ada@example.com"
	project          = "ACME"
	// readerAccount is the account BROKER_EXCHANGE_READ_ACCOUNTS lists here: its pods are minted
	// a token that only reads, naming readerService and the session readerSession the entry
	// fixes, and readerPodUID is the pod of it this test drives.
	readerAccount = "system:serviceaccount:example-ns:example-reader"
	readerService = "example-reader"
	readerSession = "example-reader-session"
	readerPodUID  = "pod-uid-reader"
	// enrolmentAudience is what a pod's enrolment token and a listed caller's own token carry;
	// exchangeAudience is what a token presented to the exchange must carry, and workflowAudience
	// what a GitHub workflow run's would. A token minted for one is not spendable at another.
	enrolmentAudience = "https://broker.example"
	exchangeAudience  = "https://broker.example/exchange"
	workflowAudience  = "https://broker.example/ci"
	// heldSession is the session name the first pod writes under, which belongs to its account:
	// a second pod of the same account writes under it too, and a pod of another account may not
	// take it. renewalSession is the renewing role's own.
	heldSession    = "example-session"
	renewalSession = "example-renewal"
	// firstPodUID is the pod that first claims heldSession; secondPodUID is another pod of the
	// very same listed account, which is the same principal and which its deployment registers
	// for that name as well; thirdPodUID is a pod of that account registered for nothing.
	firstPodUID  = "pod-uid-1"
	secondPodUID = "pod-uid-2"
	thirdPodUID  = "pod-uid-3"
	// longAccessSeconds is the access-token lifetime of the run every step but the renewal drives,
	// long enough that a token minted at its start is still live at its end; shortAccessSeconds is
	// the few seconds the renewal run is started with, so that proof waits seconds, not minutes.
	longAccessSeconds  = 900
	shortAccessSeconds = 2
	// The two fleets the Google half of this test drives, each a placeholder: pinnedFleet's
	// account is held to one Compute Engine project and one instance-name pattern and is given a
	// person and a project, so its machines write as that person; poolFleet's is pinned to
	// nothing and given neither, so its machines are its service and nothing else, which is what
	// a token minted through a workload identity pool and a token from a pod's metadata server
	// get. strangerFleet is listed nowhere.
	pinnedFleetAccount = "example-fleet@example-project.iam.gserviceaccount.com"
	poolFleetAccount   = "example-pool@example-project.iam.gserviceaccount.com"
	strangerFleet      = "example-stranger@example-project.iam.gserviceaccount.com"
	pinnedFleetService = "example-fleet"
	poolFleetService   = "example-pool"
	fleetCloudProject  = "example-project"
	fleetInstanceGlob  = "example-vm-*"
	fleetInstanceName  = "example-vm-7"
	fleetProject       = "FLEET"
	fleetPerson        = "grace@example.com"
	// machineAudience is what a machine's Google identity token must carry here, its own beside
	// the two above.
	machineAudience = "https://broker.example/machines"
	// The four secrets the machine half of this test asks for. personSecret is fleetPerson's
	// own, which no machine gets without her sign-in; sharedSecret is shared and agent tier,
	// which every session gets at once; pinnedFleetSecret and poolFleetSecret are the two
	// fleets' services' own, each of which goes to that fleet's machines and to no one else.
	personSecret      = "PERSON_SECRET"
	sharedSecret      = "SHARED_SECRET"
	pinnedFleetSecret = "PINNED_FLEET_SECRET"
	poolFleetSecret   = "POOL_FLEET_SECRET"
	// otherPersonSecret is person's own, nobody else's: what a signed-in person is refused on a
	// box, so that a sign-in is read as that one person's authority and not as every person's.
	otherPersonSecret = "OTHER_PERSON_SECRET"
	// The values the fake store holds for them.
	personSecretValue      = "person-secret-v1"
	otherPersonSecretValue = "other-person-secret-v1"
	sharedSecretValue      = "shared-secret-v1"
	pinnedFleetSecretValue = "pinned-fleet-secret-v1"
	poolFleetSecretValue   = "pool-fleet-secret-v1"
	// secretKMSKey is the key the fake store records against each, a placeholder of the shape an
	// ARN has.
	secretKMSKey = "arn:aws:kms:us-east-1:111122223333:key/1234abcd-12ab-34cd-56ef-1234567890ab"
)

// fakeSecret is one row of the store the broker reads with BROKER_FAKE_SECRETS_FILE: its path,
// the owner tag that decides who gets it, and its value. Its exposed name is the last path
// element upper-cased with its hyphens turned to underscores, which is what a request asks for.
func fakeSecret(path, owner, value string) string {
	return `{"name": "` + path + `", "kms_key_id": "` + secretKMSKey +
		`", "tags": {"owner": "` + owner + `", "tier": "agent"}, "value": "` + value + `"}`
}

// TestTheBinaryExchangesAPodsTokenIntrospectsItAndRenewsIt is the live proof of the exchange: the
// real binary on a fresh schema, a pod's projected token exchanged with a DPoP proof of a key this
// test generates and no client credential at all, the minted token read back by the listed API
// account, a second pod of the same account writing under the first's session name once its
// deployment has registered it too, a lapsed token
// renewed from the same standing pod token, and the eight refusals that
// keep the credential-less path from being a way in for anyone else. It skips when
// BROKER_TEST_DATABASE_URL is unset, as every Postgres-backed broker test does.
func TestTheBinaryExchangesAPodsTokenIntrospectsItAndRenewsIt(t *testing.T) {
	databaseURL := storetest.URL(t)
	// Two issuers the test controls: the cluster's, which mints every service-account token here,
	// and GitHub's, which mints none but is what the broker reports its second exchange issuer on.
	cluster, github := oidctest.New(t), oidctest.New(t)
	clusterKey := cluster.PublishKey(t, "cluster-key")
	github.PublishKey(t, "github-key")

	// The key the caller itself holds: every token the exchange mints here is bound to it, and an
	// introspection answers its thumbprint.
	key := proofKey(t)
	jkt, err := proof.Thumbprint(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	apiToken := accountToken(t, cluster, clusterKey, apiAccount)
	strangerToken := accountToken(t, cluster, clusterKey, otherAccount)

	broker := startBroker(t, brokerOptions{Env: exchangeEnv(t, databaseURL, cluster, github, longAccessSeconds)})
	addr := broker.Addr
	live := newLiveBroker(addr, key)

	t.Run("the real binary boots on a fresh schema with its token exchange on", func(t *testing.T) {
		// The binary logs the state of its exchange after the bind line startBroker synchronises
		// on, so the boot is read once that line has arrived.
		broker.AwaitRecord(t, "token exchange", nil)
		boot := broker.Log()
		t.Logf("the broker's own boot lines:\n%s", boot)
		requireHealthy(t, addr)
		for _, want := range []struct {
			message string
			fields  map[string]any
		}{
			{"broker listening", map[string]any{"addr": addr}},
			{"token exchange", map[string]any{"state": "on", "kubernetes": true, "github": true, "accounts": 2, "read_accounts": 1}},
		} {
			if len(recordsWith(boot, want.message, want.fields)) == 0 {
				t.Fatalf("the boot log has no %q line carrying %v; the real binary must log the address it bound — the one that just answered /healthz — and report its token exchange on, with both issuers, the two listed accounts and the one read entry:\n%s", want.message, want.fields, boot)
			}
		}
	})

	registrarToken := accountToken(t, cluster, clusterKey, registrarAccount)
	standingPodToken := mintPodToken(t, cluster, clusterKey, podAccount, exchangeAudience, firstPodUID)
	// The deployment's daemon registers the pod it starts, with the pod's own enrolment-audience
	// token — the one it already hands POST /v1/enrollments — before that pod exchanges anything.
	live.registered(t, mintPodToken(t, cluster, clusterKey, podAccount, enrolmentAudience, firstPodUID), heldSession, registrarToken)
	var accessToken string

	t.Run("a pod's token, a proof of the caller's own key and no client credential mint an access token", func(t *testing.T) {
		status, body := live.exchange(t, exchangeForm(standingPodToken, heldSession))
		if status != http.StatusOK {
			t.Fatalf("the exchange of %s's pod token = %d %v, want 200: a listed account's pod, presenting its projected token with a DPoP proof of its own key and no client credential, is answered a Dispatch access token", podAccount, status, body)
		}
		accessToken, _ = body["access_token"].(string)
		if accessToken == "" || body["token_type"] != "DPoP" || body["issued_token_type"] != oauthwire.SubjectTokenTypeAccessToken {
			t.Fatalf("the exchange answered token_type %v, issued_token_type %v and %d bytes of access token, want a DPoP-bound access token of type %s",
				body["token_type"], body["issued_token_type"], len(accessToken), oauthwire.SubjectTokenTypeAccessToken)
		}
	})

	t.Run("a listed service account introspects it and reads the session, the person, the project, the expiry and the key", func(t *testing.T) {
		if accessToken == "" {
			t.Fatal("the exchange minted no access token, so there is nothing to read back")
		}
		status, body := live.introspect(t, accessToken, apiToken)
		if status != http.StatusOK || body["active"] != true {
			t.Fatalf("introspection by the listed API account %s = %d %v, want 200 with the token live", apiAccount, status, body)
		}
		for _, field := range []struct {
			name      string
			got, want any
			proves    string
		}{
			{"session", body["session"], heldSession, "the session the token's writes are recorded as"},
			{"person", body["person"], person, "the person the entry gives that account"},
			{"projects", fmt.Sprint(body["projects"]), "[" + project + "]", "the one project the entry gives that account"},
			{"project_scope", body["project_scope"], oauthwire.ProjectScopeNamed, "that the projects are the one its entry names"},
			{"client_id", body["client_id"], oauthwire.ExchangeClientID, "the client the exchange issued it to"},
			{"jkt", body["jkt"], jkt, "the thumbprint of this test's own key, which is what makes a stolen token useless"},
		} {
			if field.got != field.want {
				t.Errorf("introspection answered %s %v, want %v: the answer must name %s", field.name, field.got, field.want, field.proves)
			}
		}
		expiry, _ := body["exp"].(float64)
		if int64(expiry) <= time.Now().Unix() {
			t.Errorf("introspection answered exp %d, which is not in the future: the answer must carry an expiry, so a caller that remembers it keeps it no longer than the token lives", int64(expiry))
		}
	})

	t.Run("a second pod of the same account writes under the session name the first holds", func(t *testing.T) {
		// Its deployment registered it as it started it, the one thing the first pod's own
		// registration does not cover.
		live.registered(t, mintPodToken(t, cluster, clusterKey, podAccount, enrolmentAudience, secondPodUID), heldSession, registrarToken)
		status, body := live.exchange(t, exchangeForm(mintPodToken(t, cluster, clusterKey, podAccount, exchangeAudience, secondPodUID), heldSession))
		second, _ := body["access_token"].(string)
		if status != http.StatusOK || second == "" {
			t.Fatalf("the exchange of a second pod of %s naming %s = %d %v, want 200: the session name belongs to the account, so the pod that replaces another in a rolling update, or a session resumed in a relaunched pod, writes under it", podAccount, heldSession, status, body)
		}
		status, body = live.introspect(t, second, apiToken)
		if status != http.StatusOK || body["active"] != true || body["session"] != heldSession || body["person"] != person {
			t.Fatalf("introspection of the second pod's token = %d %v, want 200, live, under the session %s acting for %s", status, body, heldSession, person)
		}
	})

	t.Run("a lapsed token is renewed by the same standing pod token", func(t *testing.T) {
		// Its own broker run, on a schema of its own, started with an access-token lifetime of a
		// few seconds: the proof is that the first token lapses, not that the test waits.
		renewalBroker := startBroker(t, brokerOptions{Env: exchangeEnv(t, storetest.URL(t), cluster, github, shortAccessSeconds)})
		renewalBroker.Await(t, "token exchange")
		renewalAddr := renewalBroker.Addr
		renewal := newLiveBroker(renewalAddr, key)
		podToken := mintPodToken(t, cluster, clusterKey, podAccount, exchangeAudience, "pod-uid-renewal")
		renewal.registered(t, mintPodToken(t, cluster, clusterKey, podAccount, enrolmentAudience, "pod-uid-renewal"), renewalSession, registrarToken)

		status, body := renewal.exchange(t, exchangeForm(podToken, renewalSession))
		first, _ := body["access_token"].(string)
		if status != http.StatusOK || first == "" {
			t.Fatalf("the renewing role's first exchange = %d %v, want 200 with an access token", status, body)
		}

		time.Sleep((shortAccessSeconds + 1) * time.Second)
		status, body = renewal.introspect(t, first, apiToken)
		if status != http.StatusOK || body["active"] != false {
			t.Fatalf("introspection of the first token %ds after it was minted = %d %v, want 200 with active false: this broker was started with a %ds access-token lifetime, past which its token is not live",
				shortAccessSeconds+1, status, body, shortAccessSeconds)
		}

		status, body = renewal.exchange(t, exchangeForm(podToken, renewalSession))
		second, _ := body["access_token"].(string)
		if status != http.StatusOK || second == "" {
			t.Fatalf("the second exchange of the same standing pod token = %d %v, want 200: a pod's projected token is a standing credential its kubelet refreshes, so a role renewing under its own session name presents it again and is not refused as a replay", status, body)
		}
		if second == first {
			t.Fatal("the renewal answered the very token that lapsed, want a token of its own")
		}
		status, body = renewal.introspect(t, second, apiToken)
		if status != http.StatusOK || body["active"] != true || body["session"] != renewalSession {
			t.Fatalf("introspection of the renewed token = %d %v, want 200, live, under the session %s", status, body, renewalSession)
		}
	})

	// The pod of the other listed account below is registered for the name it asks for, so what
	// refuses it is the claim on that name and not a missing registration.
	live.registered(t, mintPodToken(t, cluster, clusterKey, listedAccount, enrolmentAudience, "pod-uid-other"), heldSession, registrarToken)

	// The negative controls on the exchange, each the request that works but for one thing. The
	// broker's own refusals name what they refused; the two the library answers are pinned by
	// their status and OAuth error code alone, which is all the broker decides there.
	for _, refusal := range []struct {
		name   string
		form   url.Values
		status int
		code   string
		says   string
		proves string
	}{
		{
			name:   "an unlisted service account mints nothing",
			form:   exchangeForm(mintPodToken(t, cluster, clusterKey, otherAccount, exchangeAudience, "pod-uid-unlisted"), "example-unlisted"),
			status: http.StatusBadRequest, code: "invalid_request",
			says:   "no entry lists the service account " + otherAccount,
			proves: "a sound, correctly-audienced pod token of an account BROKER_EXCHANGE_ACCOUNTS does not list is refused, naming the account",
		},
		{
			name:   "a token minted for another audience mints nothing",
			form:   exchangeForm(mintPodToken(t, cluster, clusterKey, podAccount, enrolmentAudience, firstPodUID), "example-wrong-audience"),
			status: http.StatusBadRequest, code: "invalid_request",
			says:   "not minted for " + exchangeAudience,
			proves: "the listed account's own token, minted for the enrolment audience instead of the exchange's, is refused naming the audience this broker accepts",
		},
		{
			name:   "a token with no pod uid mints nothing",
			form:   exchangeForm(mintPodToken(t, cluster, clusterKey, podAccount, exchangeAudience, ""), "example-unbound"),
			status: http.StatusBadRequest, code: "invalid_request",
			says:   "bound to no pod",
			proves: "the listed account's token carrying no kubernetes.io pod uid — the kind `kubectl create token` mints — proves only that the account exists, not that Kubernetes minted it for a pod of this cluster",
		},
		{
			name:   "a pod of another listed account asking for the session name the first holds mints nothing",
			form:   exchangeForm(mintPodToken(t, cluster, clusterKey, listedAccount, exchangeAudience, "pod-uid-other"), heldSession),
			status: http.StatusBadRequest, code: "invalid_grant",
			says:   "The session name '" + heldSession + "' belongs to another identity",
			proves: "the session name belongs to the account whose pods claimed it, so a pod of another account, listed for another project and another person, cannot mint a token naming it",
		},
		{
			name:   "the same exchange carrying a client secret mints nothing",
			form:   with(exchangeForm(standingPodToken, "example-with-secret"), "client_secret", "an-example-secret"),
			status: http.StatusUnauthorized, code: "invalid_client",
			proves: "the credential-less path is not a way in for a caller that presents a credential, right or wrong",
		},
		{
			name:   "the same credential-less request from the machine-login client id mints nothing",
			form:   with(exchangeForm(standingPodToken, "example-other-client"), "client_id", oauthwire.ClientID),
			status: http.StatusBadRequest, code: "invalid_grant",
			proves: "the credential-less path is " + oauthwire.ExchangeClientID + " on the token-exchange grant alone",
		},
	} {
		t.Run(refusal.name, func(t *testing.T) {
			status, body := live.exchange(t, refusal.form)
			description, _ := body["error_description"].(string)
			if status != refusal.status || body["error"] != refusal.code || body["access_token"] != nil {
				t.Fatalf("that exchange = %d %v %q, want %d %s and no access token: %s",
					status, body["error"], description, refusal.status, refusal.code, refusal.proves)
			}
			if refusal.says != "" && !strings.Contains(description, refusal.says) {
				t.Fatalf("the refusal reads %q, which does not carry %q: %s", description, refusal.says, refusal.proves)
			}
		})
	}

	// The negative controls on introspection, which no caller reaches without a listed account's
	// own token: both are refused before the token they ask about is ever read back.
	for _, refusal := range []struct {
		name   string
		bearer string
		status int
		code   string
		proves string
	}{
		{
			name: "an introspection with no caller token is refused", bearer: "",
			status: http.StatusUnauthorized, code: "ACCOUNT_TOKEN_INVALID",
			proves: "the route takes the calling service's own Kubernetes ServiceAccount token, and a request with no Authorization header at all presents none",
		},
		{
			name: "an introspection by an unlisted service account is refused", bearer: strangerToken,
			status: http.StatusForbidden, code: "ACCOUNT_NOT_LISTED",
			proves: "a sound token of this cluster, for the right audience, but of an account BROKER_API_ACCOUNTS does not name, reads nothing back",
		},
	} {
		t.Run(refusal.name, func(t *testing.T) {
			status, body := live.introspect(t, "a-token-nobody-issued", refusal.bearer)
			if status != refusal.status || body["code"] != refusal.code {
				t.Fatalf("that introspection = %d %v, want %d %s: %s", status, body, refusal.status, refusal.code, refusal.proves)
			}
		})
	}
}

// TestAPodWritesOnlyUnderASessionRegisteredForIt is the live proof of the second rule, on the real
// binary: a pod exchanges under a session name only while a registration its deployment made says
// that pod uid may use it. The three pods here are one account, so they are one principal and the
// claim on the name refuses none of them — the registration is all that tells them apart. The pod
// its deployment registered writes under the name; the pod that replaces it writes under it too,
// once registered, which is why a relaunch is not a lockout; and a third pod of that very account,
// registered for nothing, is refused it, the refusal naming the session. Registering that third
// pod is what then answers it.
func TestAPodWritesOnlyUnderASessionRegisteredForIt(t *testing.T) {
	databaseURL := storetest.URL(t)
	cluster, github := oidctest.New(t), oidctest.New(t)
	clusterKey := cluster.PublishKey(t, "cluster-key")
	github.PublishKey(t, "github-key")
	key := proofKey(t)

	broker := startBroker(t, brokerOptions{Env: exchangeEnv(t, databaseURL, cluster, github, longAccessSeconds)})
	broker.Await(t, "token exchange")
	live := newLiveBroker(broker.Addr, key)
	registrarToken := accountToken(t, cluster, clusterKey, registrarAccount)
	// Each pod's two tokens: the enrolment-audience one its registration names it by, and the
	// exchange-audience one it presents itself, both bound to the one pod uid.
	enrolment := func(uid string) string {
		return mintPodToken(t, cluster, clusterKey, podAccount, enrolmentAudience, uid)
	}
	subject := func(uid string) string {
		return mintPodToken(t, cluster, clusterKey, podAccount, exchangeAudience, uid)
	}

	for _, uid := range []string{firstPodUID, secondPodUID} {
		t.Run("the pod its deployment registered writes under the name ("+uid+")", func(t *testing.T) {
			live.registered(t, enrolment(uid), heldSession, registrarToken)
			status, body := live.exchange(t, exchangeForm(subject(uid), heldSession))
			if status != http.StatusOK || body["access_token"] == nil {
				t.Fatalf("the exchange of the registered pod %s naming %s = %d %v, want 200 with an access token", uid, heldSession, status, body)
			}
		})
	}

	t.Run("an unregistered third pod of the same account is refused that name", func(t *testing.T) {
		status, body := live.exchange(t, exchangeForm(subject(thirdPodUID), heldSession))
		description, _ := body["error_description"].(string)
		if status != http.StatusBadRequest || body["error"] != "invalid_grant" || body["access_token"] != nil {
			t.Fatalf("the exchange of the unregistered pod %s of %s naming %s = %d %v %q, want 400 invalid_grant and no access token: the name belongs to the account, so nothing but the registration keeps one of its pods out of another's session",
				thirdPodUID, podAccount, heldSession, status, body, description)
		}
		if !strings.Contains(description, "'"+heldSession+"'") || !strings.Contains(description, "registration") {
			t.Fatalf("the refusal reads %q, which must name the session %s and say that a pod writes under a name its deployment registered for it", description, heldSession)
		}
	})

	t.Run("registering that third pod answers it", func(t *testing.T) {
		live.registered(t, enrolment(thirdPodUID), heldSession, registrarToken)
		status, body := live.exchange(t, exchangeForm(subject(thirdPodUID), heldSession))
		if status != http.StatusOK || body["access_token"] == nil {
			t.Fatalf("the exchange of %s once registered for %s = %d %v, want 200 with an access token", thirdPodUID, heldSession, status, body)
		}
	})
}

// pairedPodAccount is the account a deployment moves podAccount's pods to: the rows that pair the
// two (BROKER_SESSION_ACCOUNT_PAIRS) list it beside podAccount for the same project and person, as
// an operator moving a deployment lists its new account. pairedSession is the role's session name
// those rows move from one account's pods to the other's.
const (
	pairedPodAccount = "system:serviceaccount:example-next:example-worker"
	pairedSession    = "example-paired-session"
)

// exchangeTookName is the line the broker logs once a token exchange that took its session name
// from the account paired with its own has committed.
const exchangeTookName = "broker: the token exchange took a session name from the account paired with its own"

// pairedExchangeEnv is exchangeEnv with pairedPodAccount listed for the exchange beside
// podAccount, and listedAccount's entry kept: the third listed account, which no pair here names.
// registrarAccount is listed as the registrar of pairedPodAccount's pods too
// (BROKER_POD_REGISTRARS), since a row registers a moved pod of that account. It pairs nothing
// itself; a row adds BROKER_SESSION_ACCOUNT_PAIRS to it.
func pairedExchangeEnv(t *testing.T, databaseURL string, cluster, github *oidctest.Issuer) []string {
	t.Helper()
	return append(exchangeEnv(t, databaseURL, cluster, github, longAccessSeconds), "BROKER_EXCHANGE_ACCOUNTS="+
		podAccount+"="+project+"="+person+" "+pairedPodAccount+"="+project+"="+person+" "+listedAccount+"=OTHER=mallory@example.com",
		"BROKER_POD_REGISTRARS="+registrarAccount+"="+podAccount+" "+registrarAccount+"="+pairedPodAccount+" "+registrarAccount+"="+listedAccount)
}

// pairedRegistrar is a broker serving pairedExchangeEnv with podAccount and pairedPodAccount
// paired (BROKER_SESSION_ACCOUNT_PAIRS) and both bound to registrarService (BROKER_SERVICES), so
// the registrar's one login enrols the pods of both: the broker's process, the test's own
// connection to its database, the podBroker that enrols its pods with the cluster's tokens, and
// the registrar's launcher credential.
type pairedRegistrar struct {
	broker   *brokerProcess
	db       *pgx.Conn
	pods     podBroker
	launcher launcher
}

// startPairedRegistrar starts that broker on databaseURL, with env beside its own settings, waits
// for its "session account pairs" line to say on, and logs registrarService in as
// registrarAccount.
func startPairedRegistrar(t *testing.T, databaseURL string, cluster, github *oidctest.Issuer, clusterKey *oidctest.Key, env ...string) pairedRegistrar {
	t.Helper()
	broker := startBroker(t, brokerOptions{Env: append(append(pairedExchangeEnv(t, databaseURL, cluster, github),
		"BROKER_SERVICES="+registrarService+"="+podAccount+" "+registrarService+"="+pairedPodAccount,
		"BROKER_SESSION_ACCOUNT_PAIRS="+podAccount+"="+pairedPodAccount), env...)})
	broker.AwaitRecord(t, "session account pairs", map[string]any{"state": "on"})
	pods := podBroker{t: t, base: "http://" + broker.Addr, issuer: cluster, key: clusterKey, audience: enrolmentAudience}
	return pairedRegistrar{broker: broker, db: connectToBroker(t, databaseURL), pods: pods, launcher: pods.logIn(registrarService, registrarAccount)}
}

// TestAPairedAccountsPodExchangesUnderTheNameItsPairHolds is BROKER_SESSION_ACCOUNT_PAIRS at the
// token exchange, on the real binary: a deployment moves a role's pods from podAccount to
// pairedPodAccount, and the role's next pod trades under the session name the role's earlier pod
// claimed. Three accounts are listed for the exchange and each one's pod is registered for that
// name, so what decides each trade is who holds the name and nothing else. Served by a broker
// started without the setting, the moved account's trade is refused invalid_grant, as it always
// was. Served by a broker started with the pair on the same database, as the next generation of a
// rollout is, the same trade is answered a token under that name, the name is the moved account's
// from then on, and the broker logs the take once, naming the name, the account it took the name
// from and the account that took it. listedAccount, which no pair names, is refused the name by
// both brokers.
func TestAPairedAccountsPodExchangesUnderTheNameItsPairHolds(t *testing.T) {
	databaseURL := storetest.URL(t)
	cluster, github := oidctest.New(t), oidctest.New(t)
	clusterKey := cluster.PublishKey(t, "cluster-key")
	github.PublishKey(t, "github-key")
	db := connectToBroker(t, databaseURL)
	binary := buildBroker(t)
	key := proofKey(t)
	oldPrincipal, movedPrincipal := exchange.AccountPrincipal(podAccount), exchange.AccountPrincipal(pairedPodAccount)
	// Each account's one pod, which its deployment's daemon registers for the name before it
	// trades under it.
	podUIDs := map[string]string{podAccount: "pod-uid-old", pairedPodAccount: "pod-uid-moved", listedAccount: "pod-uid-unpaired"}
	trade := func(broker *liveBroker, account string) (int, map[string]any) {
		t.Helper()
		return broker.exchange(t, exchangeForm(mintPodToken(t, cluster, clusterKey, account, exchangeAudience, podUIDs[account]), pairedSession))
	}
	refusedTheName := func(status int, body map[string]any) bool {
		description, _ := body["error_description"].(string)
		return status == http.StatusBadRequest && body["error"] == "invalid_grant" && body["access_token"] == nil &&
			strings.Contains(description, "The session name '"+pairedSession+"' belongs to another identity")
	}

	t.Log("--- a broker started without the pair: the old account's pod claims the name")
	unpaired := startBroker(t, brokerOptions{Binary: binary, Env: pairedExchangeEnv(t, databaseURL, cluster, github)})
	unpaired.AwaitRecord(t, "session account pairs", map[string]any{"state": "off"})
	before := newLiveBroker(unpaired.Addr, key)
	registrarToken := accountToken(t, cluster, clusterKey, registrarAccount)
	for account, uid := range podUIDs {
		before.registered(t, mintPodToken(t, cluster, clusterKey, account, enrolmentAudience, uid), pairedSession, registrarToken)
	}
	if status, body := trade(before, podAccount); status != http.StatusOK || body["access_token"] == nil {
		t.Fatalf("the old account's pod trading under %s = %d %v, want 200 with an access token", pairedSession, status, body)
	}

	t.Log("--- unpaired, the moved account's pod is refused the name")
	if status, body := trade(before, pairedPodAccount); !refusedTheName(status, body) {
		t.Fatalf("the moved account's pod trading under %s with no pair = %d %v, want 400 invalid_grant naming the name another identity's", pairedSession, status, body)
	}
	if holder, _ := sessionHolder(t, db, pairedSession); holder != oldPrincipal {
		t.Fatalf("after the refusal the name %s is held by %q, want the old account %q still", pairedSession, holder, oldPrincipal)
	}

	t.Log("--- a broker started with the pair on the same database: the moved account's pod takes the name")
	paired := startBroker(t, brokerOptions{Binary: binary, Env: append(pairedExchangeEnv(t, databaseURL, cluster, github),
		"BROKER_SESSION_ACCOUNT_PAIRS="+podAccount+"="+pairedPodAccount)})
	paired.AwaitRecord(t, "session account pairs", map[string]any{"state": "on"})
	after := newLiveBroker(paired.Addr, key)
	status, body := trade(after, pairedPodAccount)
	token, _ := body["access_token"].(string)
	if status != http.StatusOK || token == "" {
		t.Fatalf("the moved account's pod trading under %s with the pair = %d %v, want 200 with an access token: a paired account takes the name its pair holds", pairedSession, status, body)
	}
	if status, answer := after.introspect(t, token, accountToken(t, cluster, clusterKey, apiAccount)); status != http.StatusOK || answer["active"] != true || answer["session"] != pairedSession {
		t.Fatalf("introspection of the moved account's token = %d %v, want 200, live, under the session %s", status, answer, pairedSession)
	}
	if holder, _ := sessionHolder(t, db, pairedSession); holder != movedPrincipal {
		t.Fatalf("after the take the name %s is held by %q, want the moved account %q", pairedSession, holder, movedPrincipal)
	}
	paired.AwaitRecord(t, exchangeTookName, map[string]any{"severity": "WARNING", "session": pairedSession, "from": oldPrincipal, "to": movedPrincipal})
	if takes := recordsWith(paired.Log(), exchangeTookName, nil); len(takes) != 1 {
		t.Fatalf("the paired broker logged %d %q lines, want the one take: %v", len(takes), exchangeTookName, takes)
	}
	if takes := recordsWith(unpaired.Log(), exchangeTookName, nil); len(takes) != 0 {
		t.Fatalf("the unpaired broker logged %v, want no take: its refusal took nothing", takes)
	}

	t.Log("--- an account no pair names is refused the name, by either broker")
	for _, broker := range []*liveBroker{before, after} {
		if status, body := trade(broker, listedAccount); !refusedTheName(status, body) {
			t.Fatalf("the unpaired account's pod trading under %s at %s = %d %v, want 400 invalid_grant naming the name another identity's: a pair takes for its two accounts alone",
				pairedSession, broker.base, status, body)
		}
	}
	if holder, _ := sessionHolder(t, db, pairedSession); holder != movedPrincipal {
		t.Fatalf("after the unpaired account's refusals the name %s is held by %q, want the moved account %q still", pairedSession, holder, movedPrincipal)
	}
}

// TestAPairedAccountsPodThatExchangedFirstEnrolsAndReadsItsPairsGrant is the other order a Legion
// pod may name its session in: it trades a token under the name before it enrols under it. One
// service login enrols the pods of podAccount and pairedPodAccount, which BROKER_SERVICES binds to
// its service, BROKER_SESSION_ACCOUNT_PAIRS pairs and the exchange lists. podAccount's pod enrols
// the name and asks for a secret a person owns, which that person approves on the broker's own
// page. pairedPodAccount's pod then trades under the name, which takes the name for its account,
// and enrols under it: it is enrolled, it reads the same value from the same grant, and the broker
// holds one grant and one request for the name, on the moved pod's enrollment, so the grant
// followed the name across the pair rather than the person being asked again.
func TestAPairedAccountsPodThatExchangedFirstEnrolsAndReadsItsPairsGrant(t *testing.T) {
	databaseURL := storetest.URL(t)
	cluster, github := oidctest.New(t), oidctest.New(t)
	clusterKey := cluster.PublishKey(t, "cluster-key")
	github.PublishKey(t, "github-key")
	// The cluster's issuer serves the approval page's provider too, as session_carry_test.go's
	// does, which is what lets a person sign in and press Approve against this broker.
	brokertest.SignIn(cluster, cluster.PublishKey(t, "signin-key"))
	paired := startPairedRegistrar(t, databaseURL, cluster, github, clusterKey, "BROKER_SIGNIN_ISSUER="+cluster.URL())
	rig := carryRig{t: t, ctx: context.Background(), addr: paired.broker.Addr, base: paired.pods.base, db: paired.db, signIn: cluster}
	const oldPod, movedPod = "pod-uid-old", "pod-uid-moved"

	t.Log("--- the old account's pod enrols the name under the service's one login and is granted a person's secret")
	_, grant := rig.pairedGrant(paired.pods, paired.launcher, oldPod)

	t.Log("--- the moved account's pod trades under the name, which takes it")
	live := newLiveBroker(paired.broker.Addr, proofKey(t))
	live.registered(t, mintPodToken(t, cluster, clusterKey, pairedPodAccount, enrolmentAudience, movedPod), pairedSession,
		accountToken(t, cluster, clusterKey, registrarAccount))
	status, body := live.exchange(t, exchangeForm(mintPodToken(t, cluster, clusterKey, pairedPodAccount, exchangeAudience, movedPod), pairedSession))
	if status != http.StatusOK || body["access_token"] == nil {
		t.Fatalf("the moved account's pod trading under %s = %d %v, want 200 with an access token: a paired account takes the name its pair holds", pairedSession, status, body)
	}
	movedPrincipal := exchange.AccountPrincipal(pairedPodAccount)
	if holder, _ := sessionHolder(t, rig.db, pairedSession); holder != movedPrincipal {
		t.Fatalf("after the trade the name %s is held by %q, want the moved account %q", pairedSession, holder, movedPrincipal)
	}

	t.Log("--- the moved account's pod enrols the name and reads the grant the old account's pod was approved")
	moved := paired.pods.enroll(paired.launcher, movedPod, pairedPodAccount, pairedSession)
	rig.requireValue(moved, grant, personSecret, personSecretValue,
		"an enrolment under a name its account's trade took from the paired account takes what that account's enrollment of the name holds")
	if held := rig.held(pairedSession); held.Requests != 1 || held.Grants != 1 || held.Grant != grant || held.GrantEnrollment != moved.enrollmentID {
		t.Fatalf("after the move the broker holds %+v for session %s, want one request and the one grant %s, on the moved pod's enrollment %s: the moved pod takes the grant rather than %s being asked again",
			held, pairedSession, grant, moved.enrollmentID, fleetPerson)
	}
}

// TestAPairIsRefusedANameAnExchangeEntryFixesThatItsPairHolds is the fixed name against
// BROKER_SESSION_ACCOUNT_PAIRS on the real binary, in the window before the provider's first
// exchange, when nothing has reserved the names the configuration fixes yet: the read entry's
// session, readerSession, can still be held by another account as an ordinary claim, which is
// what podAccount's pod does here by enrolling under it. pairedPodAccount, paired with podAccount,
// enrols under the same name and is refused 403 SESSION_HELD, the name staying podAccount's, since
// a name the configuration fixes goes to its entry and to no pair. What refuses it is the
// broker's own read of the names its exchange entries fix (oauthstore.Store.Reserved, which
// oauth.New sets from the exchange's reservations): the row itself is an ordinary claim, so
// without that read the pair would take it.
func TestAPairIsRefusedANameAnExchangeEntryFixesThatItsPairHolds(t *testing.T) {
	databaseURL := storetest.URL(t)
	cluster, github := oidctest.New(t), oidctest.New(t)
	clusterKey := cluster.PublishKey(t, "cluster-key")
	github.PublishKey(t, "github-key")
	paired := startPairedRegistrar(t, databaseURL, cluster, github, clusterKey)

	t.Log("--- before any exchange, the old account's pod enrols under the name the read entry fixes")
	paired.pods.enroll(paired.launcher, "pod-uid-old", podAccount, readerSession)
	held := exchange.AccountPrincipal(podAccount)
	if holder, fixed := sessionHolder(t, paired.db, readerSession); holder != held || fixed {
		t.Fatalf("after the old account's enrolment the name %s is held by %q (the configuration's hold: %t), want %q, an ordinary claim", readerSession, holder, fixed, held)
	}

	t.Log("--- the paired account's pod is refused it")
	if _, status, answer := paired.pods.enrolPod(paired.launcher, "pod-uid-moved", pairedPodAccount, readerSession); status != http.StatusForbidden || decodeError(t, answer).Code != "SESSION_HELD" {
		t.Fatalf("the paired account's pod enrolling %s, a name the read entry fixes = %d %s, want 403 SESSION_HELD: no pair takes a name the configuration fixes", readerSession, status, bytes.TrimSpace(answer))
	}
	if holder, fixed := sessionHolder(t, paired.db, readerSession); holder != held || fixed {
		t.Fatalf("after the paired account's refusal the name %s is held by %q (the configuration's hold: %t), want %q still, an ordinary claim", readerSession, holder, fixed, held)
	}
	if takes := recordsWith(paired.broker.Log(), enrollmentTookName, nil); len(takes) != 0 {
		t.Fatalf("the broker logged %v, want no take: its refusal took nothing", takes)
	}
}

// TestTheBinaryExchangesAReadEntrysPodTokenForOneThatOnlyReads is the live proof of the read
// entry, on the real binary and a fresh schema: a pod of the account BROKER_EXCHANGE_READ_ACCOUNTS
// lists presents its projected token with a DPoP proof of its own key and no client credential,
// naming no session and no scope, and is answered a token the listed API account reads back as
// exactly the read entry's: the scope read, the project scope none, the entry's service and the
// session it fixes, no person and no project, and this test's own key. Nothing was registered for
// the pod, since the entry fixes the session. The same request naming the scope read is refused
// invalid_scope, so the scope is the entry's to grant and never the request's.
func TestTheBinaryExchangesAReadEntrysPodTokenForOneThatOnlyReads(t *testing.T) {
	databaseURL := storetest.URL(t)
	cluster, github := oidctest.New(t), oidctest.New(t)
	clusterKey := cluster.PublishKey(t, "cluster-key")
	github.PublishKey(t, "github-key")
	key := proofKey(t)
	jkt, err := proof.Thumbprint(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	apiToken := accountToken(t, cluster, clusterKey, apiAccount)
	broker := startBroker(t, brokerOptions{Env: exchangeEnv(t, databaseURL, cluster, github, longAccessSeconds)})
	broker.Await(t, "token exchange")
	live := newLiveBroker(broker.Addr, key)
	form := exchangeForm(mintPodToken(t, cluster, clusterKey, readerAccount, exchangeAudience, readerPodUID), "")
	var token string

	t.Run("a read entry's pod token mints a token with no session and no scope named", func(t *testing.T) {
		status, body := live.exchange(t, form)
		token, _ = body["access_token"].(string)
		if status != http.StatusOK || token == "" || body["token_type"] != "DPoP" {
			t.Fatalf("the exchange of %s's pod token = %d %v, want 200 with a DPoP-bound access token: the read entry fixes the session, so the pod names none and is registered for nothing", readerAccount, status, body)
		}
	})

	t.Run("a listed service account introspects it as the read entry's token and nothing more", func(t *testing.T) {
		if token == "" {
			t.Fatal("the exchange minted no access token, so there is nothing to read back")
		}
		status, body := live.introspect(t, token, apiToken)
		if status != http.StatusOK {
			t.Fatalf("introspection by the listed API account %s = %d %v, want 200", apiAccount, status, body)
		}
		// Read back through the one declaration both ends of the route share, so the answer is
		// compared whole: a person, a project or a second scope would each be a field the
		// literal does not carry.
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		var answer oauthwire.Introspection
		if err := json.Unmarshal(raw, &answer); err != nil {
			t.Fatalf("the introspection answer %s is not an introspection: %v", raw, err)
		}
		want := oauthwire.Introspection{Active: true, Scope: "read", ClientID: oauthwire.ExchangeClientID, Thumbprint: jkt, Session: readerSession, Service: readerService, ProjectScope: "none", ExpiresAt: answer.ExpiresAt}
		if !reflect.DeepEqual(answer, want) {
			t.Errorf("introspection answered %+v, want %+v: a read entry's token holds the scope read and the project scope none, names its service and the session it fixes, and names no person and no project", answer, want)
		}
		if answer.ExpiresAt <= time.Now().Unix() {
			t.Errorf("introspection answered exp %d, which is not in the future", answer.ExpiresAt)
		}
	})

	t.Run("the same request naming the scope read mints nothing", func(t *testing.T) {
		status, body := live.exchange(t, with(form, "scope", oauthwire.ScopeRead))
		if status != http.StatusBadRequest || body["error"] != "invalid_scope" || body["access_token"] != nil {
			t.Fatalf("an exchange naming the scope %s = %d %v, want 400 invalid_scope and no access token: the exchange client is registered for %s alone, and the read entry grants read without being asked", oauthwire.ScopeRead, status, body, oauthwire.Scope)
		}
	})
}

// TestTheBinaryExchangesAMachinesGoogleIdentityToken is the live proof of the machine grant: the
// real binary on a fresh schema, with an issuer of this test's own standing in for Google, a
// machine presenting nothing but its Google identity token and a key of its own is answered a
// DPoP-bound Dispatch access token; the listed API account reads that token back and finds the
// person and project its fleet's entry gives it; a fleet listed with no person is answered a
// token that names its service instead, which a machine carrying no Compute Engine claims at all
// — a workload identity pool's token, and the one a pod's metadata server answers — also gets;
// and each way in that must not work is refused with a message of its own.
//
// It is also the live proof of the bridge the same trade builds to the secrets routes: the
// token answer names the launcher credential the exchange minted for the machine, that
// credential's own key enrols a session at POST /v1/enrollments, and that session reads the
// secrets its fleet's service owns and the shared agent-tier ones — the whole of what a machine
// does, from a metadata token to a secret, with no step a person takes.
//
// Secret authority follows the access token, so a machine reads no person's secret: the fleet
// whose entry names a person is answered a Dispatch token that says so, and its machine is still
// refused that person's own secret, which waits for her approval. A fleet whose entry names
// nobody enrols just the same and reads its own service's secrets: no enrolment is refused for
// want of a person.
//
// Each machine here signs with a key of its own, as two real machines do: a launcher credential
// is keyed on the key it holds, so one key cannot be two machines' credentials.
// It skips when BROKER_TEST_DATABASE_URL is unset, as every Postgres-backed broker test does.
func TestTheBinaryExchangesAMachinesGoogleIdentityToken(t *testing.T) {
	databaseURL := storetest.URL(t)
	// Three issuers the test controls: the cluster's, GitHub's, and the one standing in for
	// Google, which BROKER_GOOGLE_OIDC_ISSUER points the broker at.
	cluster, github, google := oidctest.New(t), oidctest.New(t), oidctest.New(t)
	github.PublishKey(t, "github-key")
	clusterKey := cluster.PublishKey(t, "cluster-key")
	googleKey := google.PublishKey(t, "google-key")

	key, poolKey := proofKey(t), proofKey(t)
	jkt, err := proof.Thumbprint(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	apiToken := accountToken(t, cluster, clusterKey, apiAccount)

	broker := startBroker(t, brokerOptions{Env: googleExchangeEnv(t, databaseURL, cluster, github, google, longAccessSeconds)})
	live := newLiveBroker(broker.Addr, key)
	poolLive := newLiveBroker(broker.Addr, poolKey)

	t.Run("the real binary boots with the machine entries its Google settings make", func(t *testing.T) {
		broker.AwaitRecord(t, "token exchange", nil)
		boot := broker.Log()
		t.Logf("the broker's own boot lines:\n%s", boot)
		requireHealthy(t, broker.Addr)
		for _, want := range []struct {
			message string
			fields  map[string]any
		}{
			{"token exchange", map[string]any{"state": "on", "kubernetes": true, "github": true, "accounts": 2, "read_accounts": 1, "workflows": 0, "machines": 2}},
			{"google machine identity", map[string]any{"issuer": google.URL(), "audience": machineAudience,
				"services": pinnedFleetService + "," + poolFleetService, "fleets": 2}},
		} {
			if len(recordsWith(boot, want.message, want.fields)) == 0 {
				t.Fatalf("the boot log has no %q line carrying %v; the binary must report the Google issuer it trusts and the machine entries its settings make:\n%s", want.message, want.fields, boot)
			}
		}
	})

	var fleetToken, fleetCredential, poolCredential string
	t.Run("a pinned machine's identity token and a proof of its own key mint an access token", func(t *testing.T) {
		subject := mintMachineToken(t, google, googleKey, pinnedFleetAccount, machineAudience, true, fleetCloudProject, fleetInstanceName)
		status, body := live.exchange(t, machineForm(subject, "example-machine-1"))
		if status != http.StatusOK {
			t.Fatalf("the exchange of %s's identity token = %d %v, want 200: a listed fleet's machine, presenting its Google identity token with a DPoP proof of its own key and no client credential, is answered a Dispatch access token", pinnedFleetAccount, status, body)
		}
		fleetToken, _ = body["access_token"].(string)
		if fleetToken == "" || body["token_type"] != "DPoP" || body["issued_token_type"] != oauthwire.SubjectTokenTypeAccessToken {
			t.Fatalf("the exchange answered token_type %v, issued_token_type %v and %d bytes of access token, want a DPoP-bound access token of type %s",
				body["token_type"], body["issued_token_type"], len(fleetToken), oauthwire.SubjectTokenTypeAccessToken)
		}
		fleetCredential, _ = body[oauthwire.ExtraCredentialID].(string)
		expires, _ := body[oauthwire.ExtraCredentialExpiresAt].(string)
		if fleetCredential == "" || expires == "" {
			t.Fatalf("the token answer carries %s %v and %s %v, want the launcher credential the exchange minted and its expiry: without it the machine reaches no secrets route",
				oauthwire.ExtraCredentialID, body[oauthwire.ExtraCredentialID], oauthwire.ExtraCredentialExpiresAt, body[oauthwire.ExtraCredentialExpiresAt])
		}
		if _, err := time.Parse(time.RFC3339, expires); err != nil {
			t.Errorf("%s = %q, which is no RFC 3339 time: %v", oauthwire.ExtraCredentialExpiresAt, expires, err)
		}
	})

	t.Run("the listed caller reads it back as the person and project its fleet writes for", func(t *testing.T) {
		if fleetToken == "" {
			t.Fatal("the exchange minted no access token, so there is nothing to read back")
		}
		status, body := live.introspect(t, fleetToken, apiToken)
		if status != http.StatusOK || body["active"] != true {
			t.Fatalf("introspection of the machine's token = %d %v, want 200 with the token live", status, body)
		}
		for _, field := range []struct {
			name      string
			got, want any
			proves    string
		}{
			{"session", body["session"], "example-machine-1", "the session the machine asked to write under"},
			{"person", body["person"], fleetPerson, "the person BROKER_VM_EXCHANGE_ACCOUNTS gives that fleet"},
			{"projects", fmt.Sprint(body["projects"]), "[" + fleetProject + "]", "the one project that entry gives it"},
			{"project_scope", body["project_scope"], oauthwire.ProjectScopeNamed, "that the projects are the one its entry names"},
			{"service", body["service"], nil, "no service, since this token names a person"},
			{"client_id", body["client_id"], oauthwire.ExchangeClientID, "the client the exchange issued it to"},
			{"jkt", body["jkt"], jkt, "the thumbprint of this test's own key"},
		} {
			if field.got != field.want {
				t.Errorf("introspection answered %s %v, want %v: the answer must name %s", field.name, field.got, field.want, field.proves)
			}
		}
	})

	t.Run("a fleet listed with no person is answered a token naming its service", func(t *testing.T) {
		// No Compute Engine claims at all, which is what a token minted through a workload
		// identity pool carries and what a pod's metadata server answers on Google Kubernetes
		// Engine; this fleet's entry pins neither a project nor an instance name, so it is taken.
		subject := mintMachineToken(t, google, googleKey, poolFleetAccount, machineAudience, true, "", "")
		status, body := poolLive.exchange(t, machineForm(subject, "example-pool-1"))
		token, _ := body["access_token"].(string)
		if status != http.StatusOK || token == "" {
			t.Fatalf("the exchange of %s's identity token = %d %v, want 200: an entry that pins neither a project nor an instance name takes a token carrying no Compute Engine claims", poolFleetAccount, status, body)
		}
		poolCredential, _ = body[oauthwire.ExtraCredentialID].(string)
		if poolCredential == "" {
			t.Fatalf("the service-only token answer carries no %s; every machine the exchange answers holds a credential", oauthwire.ExtraCredentialID)
		}
		status, body = live.introspect(t, token, apiToken)
		if status != http.StatusOK || body["active"] != true {
			t.Fatalf("introspection of the service-only token = %d %v, want 200 with the token live", status, body)
		}
		for _, field := range []struct {
			name      string
			got, want any
			proves    string
		}{
			{"session", body["session"], "example-pool-1", "the session the machine asked to write under"},
			{"service", body["service"], poolFleetService, "the service BROKER_VM_SERVICES gives that fleet"},
			{"person", body["person"], nil, "no person, since this fleet's entry names none"},
			{"projects", body["projects"], nil, "no project, since a token naming no person names none"},
			{"project_scope", body["project_scope"], oauthwire.ProjectScopePerson, "that no entry maps the token to a project"},
		} {
			if field.got != field.want {
				t.Errorf("introspection answered %s %v, want %v: the answer must name %s", field.name, field.got, field.want, field.proves)
			}
		}
	})

	t.Run("the credential the exchange minted enrols a session with its fleet's authority", func(t *testing.T) {
		if fleetCredential == "" {
			t.Fatal("the exchange named no credential, so there is nothing to enrol under")
		}
		machine := podBroker{t: t, base: "http://" + broker.Addr}
		session := machine.enrolMachine(key, fleetCredential, "example-machine-1")
		for _, want := range []struct {
			secret, value, proves string
		}{
			{secret: pinnedFleetSecret, value: pinnedFleetSecretValue, proves: "its own fleet's service owns it"},
			{secret: sharedSecret, value: sharedSecretValue, proves: "a shared agent-tier secret goes to every session at once"},
		} {
			granted := machine.ask(session, want.secret)
			if granted.State != "granted" || granted.GrantID == nil {
				t.Fatalf("the machine's request for %s = %+v, want it granted at once: %s", want.secret, granted, want.proves)
			}
			if got := machine.values(session, *granted.GrantID)[want.secret]; got != want.value {
				t.Fatalf("the machine read %s = %q, want %q", want.secret, got, want.value)
			}
		}
		if denied := machine.ask(session, poolFleetSecret); denied.State != "denied" {
			t.Fatalf("the machine's request for %s = %+v, want it denied: that secret is the other fleet's service's", poolFleetSecret, denied)
		}
	})

	t.Run("a machine reads no person's secret, though its fleet entry names one", func(t *testing.T) {
		// The whole point of the shape: this fleet's entry names fleetPerson, and the Dispatch
		// token the same trade minted says so. Her secrets are still hers: the credential the
		// exchange minted names no person, so the machine's session acts for its service, and
		// asking for her secret opens an approval request to her rather than granting it.
		machine := podBroker{t: t, base: "http://" + broker.Addr}
		session := machine.enrolMachine(key, fleetCredential, "example-machine-person-check")
		asked := machine.ask(session, personSecret)
		if asked.State != "pending" || asked.GrantID != nil {
			t.Fatalf("the machine's request for %s = %+v, want it pending with no grant: %s owns it and nobody has signed in on this machine", personSecret, asked, fleetPerson)
		}
	})

	t.Run("a fleet that names no person enrols too, with its own service's authority", func(t *testing.T) {
		if poolCredential == "" {
			t.Fatal("the service-only exchange named no credential, so there is nothing to enrol under")
		}
		machine := podBroker{t: t, base: "http://" + broker.Addr}
		session := machine.enrolMachine(poolKey, poolCredential, "example-pool-1")
		granted := machine.ask(session, poolFleetSecret)
		if granted.State != "granted" || granted.GrantID == nil {
			t.Fatalf("the service-only machine's request for %s = %+v, want it granted at once: no enrolment is refused for want of a person", poolFleetSecret, granted)
		}
		if got := machine.values(session, *granted.GrantID)[poolFleetSecret]; got != poolFleetSecretValue {
			t.Fatalf("the service-only machine read %s = %q, want %q", poolFleetSecret, got, poolFleetSecretValue)
		}
		if denied := machine.ask(session, pinnedFleetSecret); denied.State != "denied" {
			t.Fatalf("the service-only machine's request for %s = %+v, want it denied: that secret is the other fleet's service's", pinnedFleetSecret, denied)
		}
	})

	// The person half of the same shape: secret authority follows the access token, so what the
	// machine was refused just above is exactly what a person signed in on that machine reaches,
	// and nothing more. Each sign-in holds a box key of its own, as `agent-secrets login` makes
	// one: a launcher credential is keyed on the key it holds.
	graceBoxKey, graceSecondBoxKey := proofKey(t), proofKey(t)
	graceBox := newLiveBroker(broker.Addr, graceBoxKey)
	graceSecondBox := newLiveBroker(broker.Addr, graceSecondBoxKey)
	signIn := func(live *liveBroker, account, session string) string {
		t.Helper()
		instance := fleetInstanceName
		if account != pinnedFleetAccount {
			instance = ""
		}
		status, body := live.exchange(t, signInForm(
			mintPersonToken(t, google, googleKey, fleetPerson, deviceAudience, true, signInDomain),
			mintMachineToken(t, google, googleKey, account, machineAudience, true, fleetCloudProject, instance), session))
		credential, _ := body[oauthwire.ExtraCredentialID].(string)
		if status != http.StatusOK || credential == "" {
			t.Fatalf("%s's sign-in trade on %s = %d %v, want 200 with the launcher credential the sign-in holds", fleetPerson, account, status, body)
		}
		return credential
	}

	var graceCredential, graceSecondCredential string
	var graceSessionPod, graceSecondPod pod
	t.Run("a person signed in on a machine reads the secret that person owns", func(t *testing.T) {
		graceCredential = signIn(graceBox, pinnedFleetAccount, "grace-box-1")
		machine := podBroker{t: t, base: "http://" + broker.Addr}
		graceSessionPod = machine.enrolMachine(graceBoxKey, graceCredential, "grace-box-1")
		granted := machine.ask(graceSessionPod, personSecret)
		if granted.State != "granted" || granted.GrantID == nil {
			t.Fatalf("the signed-in person's request for %s = %+v, want it granted at once: %s owns it and she signed in on this box", personSecret, granted, fleetPerson)
		}
		if got := machine.values(graceSessionPod, *granted.GrantID)[personSecret]; got != personSecretValue {
			t.Fatalf("the signed-in person read %s = %q, want %q", personSecret, got, personSecretValue)
		}
	})

	t.Run("that session is still refused another person's secret", func(t *testing.T) {
		machine := podBroker{t: t, base: "http://" + broker.Addr}
		asked := machine.ask(graceSessionPod, otherPersonSecret)
		if asked.State != "pending" || asked.GrantID != nil {
			t.Fatalf("%s's session asking for %s = %+v, want it pending with no grant: that secret is %s's, and a sign-in is one person's authority", fleetPerson, otherPersonSecret, asked, person)
		}
	})

	t.Run("the same person signs in on a second machine, under a box key of its own", func(t *testing.T) {
		graceSecondCredential = signIn(graceSecondBox, poolFleetAccount, "grace-box-2")
		if graceSecondCredential == graceCredential {
			t.Fatalf("both boxes hold the credential %s, want one each: a sign-in is keyed on the person, the machine and the box key", graceCredential)
		}
		machine := podBroker{t: t, base: "http://" + broker.Addr}
		graceSecondPod = machine.enrolMachine(graceSecondBoxKey, graceSecondCredential, "grace-box-2")
		granted := machine.ask(graceSecondPod, personSecret)
		if granted.State != "granted" || granted.GrantID == nil {
			t.Fatalf("the second box's request for %s = %+v, want it granted: the person is signed in there too", personSecret, granted)
		}
	})

	t.Run("revoking one sign-in ends its credential and its session, and leaves the other", func(t *testing.T) {
		plain := newPlainBroker(broker.Addr)
		status, body := plain.callAs(t, http.MethodGet, "/v1/person-signins?person="+url.QueryEscape(fleetPerson), apiToken, "", "")
		if status != http.StatusOK {
			t.Fatalf("GET /v1/person-signins = %d %v, want 200", status, body)
		}
		rows, _ := body["signins"].([]any)
		var target string
		for _, raw := range rows {
			row, _ := raw.(map[string]any)
			if row["machine"] == pinnedFleetAccount {
				target, _ = row["signin_id"].(string)
			}
		}
		if target == "" {
			t.Fatalf("the list %v carries no sign-in on %s", body, pinnedFleetAccount)
		}
		if status, body := plain.callAs(t, http.MethodPost, "/v1/person-signins/"+target+"/revoke", apiToken,
			"application/json", `{"person":"`+fleetPerson+`"}`); status != http.StatusOK || body["state"] != "revoked" {
			t.Fatalf("the revoke = %d %v, want 200 revoked", status, body)
		}
		machine := podBroker{t: t, base: "http://" + broker.Addr}
		if _, status, answer := machine.enrolSession(graceBoxKey, graceCredential, "grace-box-1-again"); status != http.StatusUnauthorized {
			t.Fatalf("the revoked sign-in's credential enrolled = %d %s, want 401: no launcher proof signed with that box key authenticates again", status, answer)
		}
		if status, answer := machine.askRaw(graceSessionPod, personSecret); status == http.StatusOK {
			t.Fatalf("the revoked sign-in's session read %s = %d %s, want it refused: the revoke ended every session that credential enrolled", personSecret, status, answer)
		}
		granted := machine.ask(graceSecondPod, personSecret)
		if granted.State != "granted" || granted.GrantID == nil {
			t.Fatalf("the person's second box asking for %s = %+v, want it granted: revoking one sign-in ends one", personSecret, granted)
		}
	})

	// The negative controls on the machine grant, each the request that works but for one thing.
	for _, refusal := range []struct {
		name   string
		form   url.Values
		status int
		code   string
		says   string
		proves string
	}{
		{
			name:   "an unlisted service account mints nothing",
			form:   machineForm(mintMachineToken(t, google, googleKey, strangerFleet, machineAudience, true, fleetCloudProject, fleetInstanceName), "example-unlisted"),
			status: http.StatusBadRequest, code: "invalid_request",
			says:   "no entry lists the Google account " + strangerFleet,
			proves: "a sound, correctly-audienced identity token of an account no VM service lists is refused, naming the account",
		},
		{
			name:   "an email the provider has not verified mints nothing",
			form:   machineForm(mintMachineToken(t, google, googleKey, pinnedFleetAccount, machineAudience, false, fleetCloudProject, fleetInstanceName), "example-unverified"),
			status: http.StatusBadRequest, code: "invalid_request",
			says:   "email_verified is not true",
			proves: "the broker checks email_verified itself, since a token for an address nobody verified proves no account",
		},
		{
			name:   "a machine with no Compute Engine claims against a pinned entry mints nothing",
			form:   machineForm(mintMachineToken(t, google, googleKey, pinnedFleetAccount, machineAudience, true, "", ""), "example-unpinned"),
			status: http.StatusBadRequest, code: "invalid_request",
			says:   "carrying no Compute Engine identity",
			proves: "an entry that pins a project and an instance name refuses a token that says nothing at all about where it was minted",
		},
		{
			name:   "a machine of another Compute Engine project mints nothing",
			form:   machineForm(mintMachineToken(t, google, googleKey, pinnedFleetAccount, machineAudience, true, "other-project", fleetInstanceName), "example-other-project"),
			status: http.StatusBadRequest, code: "invalid_request",
			says:   "minted in the project other-project",
			proves: "the entry's project pin is compared with the token's own google.compute_engine claims",
		},
		{
			name:   "a machine whose instance name the pattern does not match mints nothing",
			form:   machineForm(mintMachineToken(t, google, googleKey, pinnedFleetAccount, machineAudience, true, fleetCloudProject, "example-other-1"), "example-other-name"),
			status: http.StatusBadRequest, code: "invalid_request",
			says:   "minted on the instance example-other-1",
			proves: "the entry's instance-name pattern is compared with the token's own instance name",
		},
		{
			name:   "a token minted for another audience mints nothing",
			form:   machineForm(mintMachineToken(t, google, googleKey, pinnedFleetAccount, enrolmentAudience, true, fleetCloudProject, fleetInstanceName), "example-wrong-audience"),
			status: http.StatusBadRequest, code: "invalid_request",
			says:   "not minted for " + machineAudience,
			proves: "the fleet's own token, minted for another audience of this broker, is refused naming the audience the machine grant accepts",
		},
		{
			name:   "a machine naming no session mints nothing",
			form:   machineForm(mintMachineToken(t, google, googleKey, pinnedFleetAccount, machineAudience, true, fleetCloudProject, fleetInstanceName), ""),
			status: http.StatusBadRequest, code: "invalid_request",
			says:   "is listed with no session of its own",
			proves: "a fleet whose entry fixes no session writes under a name its machine names, so a request naming none is refused",
		},
		{
			name:   "a machine's token presented as a pod's mints nothing",
			form:   exchangeForm(mintMachineToken(t, google, googleKey, pinnedFleetAccount, machineAudience, true, fleetCloudProject, fleetInstanceName), "example-as-a-pod"),
			status: http.StatusBadRequest, code: "invalid_request",
			proves: "each subject token type is checked against its own issuer, so a machine's token is not a pod's",
		},
	} {
		t.Run(refusal.name, func(t *testing.T) {
			status, body := live.exchange(t, refusal.form)
			description, _ := body["error_description"].(string)
			if status != refusal.status || body["error"] != refusal.code || body["access_token"] != nil {
				t.Fatalf("that exchange = %d %v %q, want %d %s and no access token: %s",
					status, body["error"], description, refusal.status, refusal.code, refusal.proves)
			}
			if refusal.says != "" && !strings.Contains(description, refusal.says) {
				t.Fatalf("the refusal reads %q, which does not carry %q: %s", description, refusal.says, refusal.proves)
			}
		})
	}
}

// The Dispatch the c-token-projects tests stand in for: PRIV is private, owned by carol and
// listing bob; PUB is public. bob is a restricted member, carol and erin full ones, and alice is a
// person Dispatch holds no row for, whom PRIV does not list.
const (
	privateProject = "PRIV"
	publicProject  = "PUB"
	listedNowhere  = "alice@example.com"
	restricted     = "bob@example.com"
	privateOwner   = "carol@example.com"
	fullMember     = "erin@example.com"
)

// projectAccessStandIn serves Dispatch's two project-access routes over those rows.
func projectAccessStandIn(t *testing.T) *projectaccesstest.Server {
	t.Helper()
	return projectaccesstest.NewServer(t, map[string]projectaccesstest.Row{
		privateProject: {Visibility: "private", Owner: privateOwner, Listed: []string{restricted}},
		publicProject:  {Visibility: "public", Owner: privateOwner},
	}, map[string]string{restricted: projectaccess.MemberRestricted, privateOwner: projectaccess.MemberFull, fullMember: projectaccess.MemberFull})
}

// projectAccessEnv is exchangeEnv with the broker's project-access check pointed at server: its
// URL, and a file holding the one bearer it takes, as a projected token is mounted.
func projectAccessEnv(t *testing.T, databaseURL string, cluster, github *oidctest.Issuer, server *projectaccesstest.Server) []string {
	t.Helper()
	tokenFile := filepath.Join(t.TempDir(), "dispatch-access-token")
	if err := os.WriteFile(tokenFile, []byte(server.Token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return append(exchangeEnv(t, databaseURL, cluster, github, longAccessSeconds),
		"BROKER_PROJECT_ACCESS_URL="+server.URL, "BROKER_PROJECT_ACCESS_TOKEN_FILE="+tokenFile)
}

// devBoxSession logs who's machine host in through the real binary under a key of its own, and
// answers that machine's broker client and its login's access token.
func devBoxSession(t *testing.T, addr, apiToken, who, host string) (*liveBroker, string) {
	t.Helper()
	key := proofKey(t)
	live := newLiveBroker(addr, key)
	access, _ := live.machineLoginAs(t, key, apiToken, who, host)
	return live, access
}

// machineTrade is a dev box session's trade of its machine login's access token: the session it
// writes under and, when projects is not empty, the projects it names.
func machineTrade(accessToken, session, projects string) url.Values {
	form := exchangeForm(accessToken, session)
	form.Set("subject_token_type", oauthwire.SubjectTokenTypeAccessToken)
	if projects != "" {
		form.Set(oauthwire.FormProjects, projects)
	}
	return form
}

// projectsOf is an introspection answer's projects, nil when it names none.
func projectsOf(t *testing.T, answer map[string]any) []string {
	t.Helper()
	raw, present := answer["projects"]
	if !present {
		return nil
	}
	list, ok := raw.([]any)
	if !ok {
		t.Fatalf("the answer's projects are %#v, want a list", raw)
	}
	projects := make([]string, 0, len(list))
	for _, item := range list {
		key, ok := item.(string)
		if !ok {
			t.Fatalf("the answer's projects hold %#v, want strings", item)
		}
		projects = append(projects, key)
	}
	return projects
}

// TestATradeNamesOnlyProjectsItsPersonSees is c-token-projects against the real broker binary and
// a Dispatch stand-in answering both project-access routes: a trade naming a project its person
// may see is answered a token whose introspection lists exactly that project; a trade naming one
// its person may not see is refused; a restricted member's trade naming none is refused; and a
// full member's trade naming none, and the same trade while Dispatch cannot be reached, are
// answered with no project, as today.
func TestATradeNamesOnlyProjectsItsPersonSees(t *testing.T) {
	databaseURL := storetest.URL(t)
	cluster, github := oidctest.New(t), oidctest.New(t)
	clusterKey := cluster.PublishKey(t, "cluster-key")
	github.PublishKey(t, "github-key")
	apiToken := accountToken(t, cluster, clusterKey, apiAccount)
	dispatch := projectAccessStandIn(t)
	broker := startBroker(t, brokerOptions{Env: projectAccessEnv(t, databaseURL, cluster, github, dispatch)})
	broker.AwaitRecord(t, "project access check", map[string]any{"state": "on"})
	trade := func(t *testing.T, who, host, session, projects string) (*liveBroker, int, map[string]any) {
		t.Helper()
		live, access := devBoxSession(t, broker.Addr, apiToken, who, host)
		status, body := live.exchange(t, machineTrade(access, session, projects))
		return live, status, body
	}
	introspected := func(t *testing.T, live *liveBroker, body map[string]any, who string) map[string]any {
		t.Helper()
		token, _ := body["access_token"].(string)
		if token == "" {
			t.Fatalf("%s's trade was answered %v, want an access token", who, body)
		}
		status, answer := live.introspect(t, token, apiToken)
		if status != http.StatusOK || answer["active"] != true || answer["project_scope"] != oauthwire.ProjectScopePerson || answer["person"] != who {
			t.Fatalf("introspection of %s's token = %d %v, want a live token acting as %s", who, status, answer, who)
		}
		return answer
	}

	t.Run("a trade naming a project its person may see is answered a token listing exactly that project", func(t *testing.T) {
		live, _, body := trade(t, privateOwner, "example-host-carol-1", "carol-box-1", privateProject)
		if got := projectsOf(t, introspected(t, live, body, privateOwner)); !slices.Equal(got, []string{privateProject}) {
			t.Fatalf("%s's token names the projects %v, want exactly [%s]", privateOwner, got, privateProject)
		}
	})
	t.Run("a trade naming a project its person may not see is refused", func(t *testing.T) {
		live, status, body := trade(t, listedNowhere, "example-host-alice-1", "alice-box-1", privateProject)
		if token, _ := body["access_token"].(string); token != "" {
			_, answer := live.introspect(t, token, apiToken)
			t.Fatalf("the broker answered %s's trade naming %s with a token naming %v; a trade naming a project its person may not see must be refused", listedNowhere, privateProject, projectsOf(t, answer))
		}
		description, _ := body["error_description"].(string)
		if status != http.StatusBadRequest || body["error"] != "invalid_request" || !strings.Contains(description, "cannot see: "+privateProject) {
			t.Fatalf("%s's trade naming %s = %d %v %q, want 400 invalid_request naming %s", listedNowhere, privateProject, status, body["error"], description, privateProject)
		}
	})
	t.Run("a restricted member's trade naming none is refused", func(t *testing.T) {
		_, status, body := trade(t, restricted, "example-host-bob-1", "bob-box-1", "")
		description, _ := body["error_description"].(string)
		if status != http.StatusBadRequest || body["error"] != "invalid_request" || body["access_token"] != nil || !strings.Contains(description, restricted+" is a restricted member") {
			t.Fatalf("%s's trade naming no project = %d %v %q, want 400 invalid_request saying %s is a restricted member", restricted, status, body["error"], description, restricted)
		}
	})
	t.Run("a full member's trade naming none is answered with no project, as today", func(t *testing.T) {
		live, _, body := trade(t, privateOwner, "example-host-carol-2", "carol-box-2", "")
		if got := projectsOf(t, introspected(t, live, body, privateOwner)); got != nil {
			t.Fatalf("%s's token naming no project names %v, want none", privateOwner, got)
		}
	})
	t.Run("the same trade while Dispatch cannot be reached is answered with no project, as today", func(t *testing.T) {
		live, access := devBoxSession(t, broker.Addr, apiToken, fullMember, "example-host-erin-1")
		dispatch.SetDown(true)
		t.Cleanup(func() { dispatch.SetDown(false) })
		before := dispatch.Calls()
		_, body := live.exchange(t, machineTrade(access, "erin-box-1", ""))
		if dispatch.Calls() == before {
			t.Fatalf("the trade asked the stand-in nothing while it was down, so this case proves nothing about an outage")
		}
		if got := projectsOf(t, introspected(t, live, body, fullMember)); got != nil {
			t.Fatalf("%s's token minted while Dispatch was down names %v, want none", fullMember, got)
		}
	})
}

// TestTheProjectsClaimSurvivesTheStore: the list a trade asks for comes back from the token's own
// row, through the broker's Postgres-backed store and its introspection route, as that list. The
// stored claim is a string because the session's extra claims return from the row as JSON, where a
// list would read back as no projects at all.
func TestTheProjectsClaimSurvivesTheStore(t *testing.T) {
	databaseURL := storetest.URL(t)
	cluster, github := oidctest.New(t), oidctest.New(t)
	clusterKey := cluster.PublishKey(t, "cluster-key")
	github.PublishKey(t, "github-key")
	apiToken := accountToken(t, cluster, clusterKey, apiAccount)
	broker := startBroker(t, brokerOptions{Env: projectAccessEnv(t, databaseURL, cluster, github, projectAccessStandIn(t))})
	broker.Await(t, "project access check")
	const session = "carol-store-1"
	live, access := devBoxSession(t, broker.Addr, apiToken, privateOwner, "example-host-carol-store")
	status, body := live.exchange(t, machineTrade(access, session, publicProject+" "+privateProject))
	token, _ := body["access_token"].(string)
	if status != http.StatusOK || token == "" {
		t.Fatalf("the trade for the session %s = %d %v, want an access token", session, status, body)
	}
	status, answer := live.introspect(t, token, apiToken)
	if got := projectsOf(t, answer); status != http.StatusOK || answer["active"] != true || !slices.Equal(got, []string{privateProject, publicProject}) {
		t.Fatalf("the token of the session %s introspects through the broker's store with projects %v (%d %v), want [%s %s]: the claim must come back from its row as the list the trade asked for",
			session, got, status, answer, privateProject, publicProject)
	}
}

// exchangeEnv is what one broker run of this test needs beyond productionEnv: the fresh schema it
// migrates and serves from, the cluster issuer with the audience a pod enrols with and the one it
// exchanges with, the GitHub issuer, the two listed accounts with their projects and persons, the
// read entry with its service and the session it fixes, the two listed callers — dispatch, which
// reads a token back and ends one, and listener, which only reads one back — the registrar that
// logs in as the deployment's daemon and registers its pods' session names, listed for the pods
// of both listed accounts (BROKER_POD_REGISTRARS), and how long an access token it mints lasts.
func exchangeEnv(t *testing.T, databaseURL string, cluster, github *oidctest.Issuer, accessSeconds int, extraSecrets ...string) []string {
	t.Helper()
	fakeSecretsFile := filepath.Join(t.TempDir(), "fake-secrets.json")
	rows := append([]string{
		fakeSecret("example/agent-secrets/person-secret", fleetPerson, personSecretValue),
		fakeSecret("example/agent-secrets/other-person-secret", person, otherPersonSecretValue),
		fakeSecret("example/agent-secrets/shared-secret", "shared", sharedSecretValue),
	}, extraSecrets...)
	if err := os.WriteFile(fakeSecretsFile, []byte(`{"secrets": [`+strings.Join(rows, ",\n")+`]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// The machine-login page is named here because the rows these brokers serve include a
	// machine login's own device grant (token_revocation_test.go); it is optional, and the
	// broker main_test.go's productionEnv starts serves none.
	return []string{
		"BROKER_MACHINE_LOGIN_URL=" + testMachineLoginURL,
		"BROKER_DATABASE_URL=" + databaseURL,
		"BROKER_LISTEN_ADDR=127.0.0.1:0",
		"BROKER_PUBLIC_URL=http://127.0.0.1:0",
		"BROKER_FAKE_SECRETS_FILE=" + fakeSecretsFile,
		"BROKER_K8S_OIDC_ISSUER=" + cluster.URL(),
		"BROKER_K8S_OIDC_AUDIENCE=" + enrolmentAudience,
		"BROKER_EXCHANGE_K8S_AUDIENCE=" + exchangeAudience,
		"BROKER_EXCHANGE_ACCOUNTS=" + podAccount + "=" + project + "=" + person + " " + listedAccount + "=OTHER=mallory@example.com",
		"BROKER_EXCHANGE_READ_ACCOUNTS=" + readerAccount + "=" + readerService + "=" + readerSession,
		"BROKER_EXCHANGE_GITHUB_ISSUER=" + github.URL(),
		"BROKER_EXCHANGE_GITHUB_AUDIENCE=" + workflowAudience,
		"BROKER_API_ACCOUNTS=dispatch=" + apiAccount + " listener=" + listenerAccount,
		"BROKER_SERVICE_LOGINS=" + registrarService + "=" + registrarAccount,
		"BROKER_POD_REGISTRARS=" + registrarAccount + "=" + podAccount + " " + registrarAccount + "=" + listedAccount,
		"BROKER_ACCESS_TOKEN_SECONDS=" + strconv.Itoa(accessSeconds),
	}
}

// TestAPodsEntryNamingAPrivateProjectIsHeldToItsPerson is the credential the production path
// makes for a private project: a pod's account listed in BROKER_EXCHANGE_ACCOUNTS with the
// project PRIV and the person it acts for. Against a Dispatch stand-in that says the entry's
// person sees PRIV, the pod's trade is answered a token whose introspection names exactly [PRIV]
// under the named scope; against one that says they do not, the same trade is refused naming the
// project and the person.
func TestAPodsEntryNamingAPrivateProjectIsHeldToItsPerson(t *testing.T) {
	for _, c := range []struct {
		name   string
		listed []string
		minted bool
	}{
		{name: "the entry's person sees PRIV", listed: []string{person}, minted: true},
		{name: "the entry's person does not see PRIV", listed: nil, minted: false},
	} {
		t.Run(c.name, func(t *testing.T) {
			databaseURL := storetest.URL(t)
			cluster, github := oidctest.New(t), oidctest.New(t)
			clusterKey := cluster.PublishKey(t, "cluster-key")
			github.PublishKey(t, "github-key")
			dispatch := projectaccesstest.NewServer(t, map[string]projectaccesstest.Row{
				privateProject: {Visibility: "private", Owner: privateOwner, Listed: c.listed},
			}, map[string]string{person: projectaccess.MemberFull})
			env := append(projectAccessEnv(t, databaseURL, cluster, github, dispatch),
				"BROKER_EXCHANGE_ACCOUNTS="+podAccount+"="+privateProject+"="+person)
			broker := startBroker(t, brokerOptions{Env: env})
			broker.AwaitRecord(t, "project access check", map[string]any{"state": "on"})
			live := newLiveBroker(broker.Addr, proofKey(t))
			apiToken := accountToken(t, cluster, clusterKey, apiAccount)
			live.registered(t, mintPodToken(t, cluster, clusterKey, podAccount, enrolmentAudience, firstPodUID), heldSession,
				accountToken(t, cluster, clusterKey, registrarAccount))

			status, body := live.exchange(t, exchangeForm(mintPodToken(t, cluster, clusterKey, podAccount, exchangeAudience, firstPodUID), heldSession))
			if !c.minted {
				description, _ := body["error_description"].(string)
				t.Logf("the refusal: %d %v %s", status, body["error"], description)
				if status != http.StatusBadRequest || body["error"] != "invalid_request" || body["access_token"] != nil ||
					!strings.Contains(description, "its entry names the project "+privateProject+", which its person "+person+" cannot see") {
					t.Fatalf("the pod's trade = %d %v %q, want 400 invalid_request naming %s and %s", status, body["error"], description, privateProject, person)
				}
				return
			}
			token, _ := body["access_token"].(string)
			if status != http.StatusOK || token == "" {
				t.Fatalf("the pod's trade = %d %v, want an access token", status, body)
			}
			status, answer := live.introspect(t, token, apiToken)
			t.Logf("introspection of the pod's token: %d %v", status, answer)
			if got := projectsOf(t, answer); status != http.StatusOK || answer["active"] != true || answer["project_scope"] != oauthwire.ProjectScopeNamed ||
				answer["person"] != person || !slices.Equal(got, []string{privateProject}) {
				t.Fatalf("introspection = %d %v, want a live named token acting for %s naming exactly [%s]", status, answer, person, privateProject)
			}
		})
	}
}

// googleExchangeEnv is exchangeEnv with the Google half configured: the issuer standing in for
// Google's, the audience a machine's identity token must carry, the two fleets, the pins of the
// first, and the person and project the first writes for. The second fleet is listed with no pin
// and no person, which is what mints a token that names its service and nobody.
func googleExchangeEnv(t *testing.T, databaseURL string, cluster, github, google *oidctest.Issuer, accessSeconds int) []string {
	t.Helper()
	return append(exchangeEnv(t, databaseURL, cluster, github, accessSeconds,
		fakeSecret("example/agent-secrets/pinned-fleet-secret", pinnedFleetService, pinnedFleetSecretValue),
		fakeSecret("example/agent-secrets/pool-fleet-secret", poolFleetService, poolFleetSecretValue)),
		"BROKER_GOOGLE_OIDC_ISSUER="+google.URL(),
		"BROKER_GOOGLE_OIDC_AUDIENCE="+machineAudience,
		"BROKER_VM_SERVICES="+pinnedFleetService+"="+pinnedFleetAccount+" "+poolFleetService+"="+poolFleetAccount,
		"BROKER_VM_PROJECTS="+pinnedFleetAccount+"="+fleetCloudProject,
		"BROKER_VM_INSTANCES="+pinnedFleetAccount+"="+fleetInstanceGlob,
		"BROKER_VM_EXCHANGE_ACCOUNTS="+pinnedFleetAccount+"="+fleetProject+"="+fleetPerson,
		// The device-flow client a person signs in on one of those machines through, which is
		// what turns the person half of the exchange on (person_signin_test.go).
		"BROKER_SIGNIN_DEVICE_AUDIENCE="+deviceAudience,
	)
}

// mintMachineToken is a Google identity token as a machine presents one: the account it was
// minted for in its `email` claim, whether the address is verified, and the Compute Engine
// instance it was minted on — none when instance is empty, which is what a token minted through a
// workload identity pool, and the one a pod's metadata server answers, carry.
func mintMachineToken(t *testing.T, google *oidctest.Issuer, key *oidctest.Key, account, audience string, verified any, cloudProject, instance string) string {
	t.Helper()
	claims := google.Claims(account, audience)
	claims["email"] = account
	claims["email_verified"] = verified
	if instance != "" {
		claims["google"] = map[string]any{"compute_engine": map[string]any{
			"project_id":    cloudProject,
			"zone":          "example-zone-a",
			"instance_name": instance,
			"instance_id":   "1234567890",
		}}
	}
	return google.Mint(t, key, claims)
}

// machineForm is a complete RFC 8693 token exchange request as a machine makes it: the exchange
// client with no credential of its own, the Google identity token it presents, and the session
// name it asks to write under.
func machineForm(subjectToken, session string) url.Values {
	form := exchangeForm(subjectToken, session)
	form.Set("subject_token_type", oauthwire.SubjectTokenTypeGoogle)
	return form
}

// mintPodToken is a projected service-account token as the cluster's issuer mints one: the account
// it belongs to, the audience it is spendable at, and the pod it is bound to — none when uid is
// empty, which is what `kubectl create token` mints.
func mintPodToken(t *testing.T, cluster *oidctest.Issuer, key *oidctest.Key, account, audience, uid string) string {
	t.Helper()
	claims := cluster.Claims(account, audience)
	if uid != "" {
		claims["kubernetes.io"] = map[string]any{"pod": map[string]any{"uid": uid}}
	}
	return cluster.Mint(t, key, claims)
}

// accountToken is a service account's own token for the enrolment audience, the one a caller of
// the broker's named-caller routes presents. Its issue time is recent because a token that lives
// longer than an hour from its issue is refused there.
func accountToken(t *testing.T, cluster *oidctest.Issuer, key *oidctest.Key, account string) string {
	t.Helper()
	claims := cluster.Claims(account, enrolmentAudience)
	claims["iat"] = time.Now().Add(-5 * time.Second).Unix()
	return cluster.Mint(t, key, claims)
}

// exchangeForm is a complete RFC 8693 token exchange request as a pod makes it: the exchange
// client with no credential of its own, the projected token it presents, and the session name it
// asks to write under.
func exchangeForm(subjectToken, session string) url.Values {
	return url.Values{
		"grant_type":         {exchangeGrantType},
		"client_id":          {oauthwire.ExchangeClientID},
		"subject_token":      {subjectToken},
		"subject_token_type": {oauthwire.SubjectTokenTypeKubernetes},
		"session":            {session},
	}
}

// with is form with one field set, for a control that differs from the request that works by
// exactly that field.
func with(form url.Values, name, value string) url.Values {
	changed := url.Values{}
	maps.Copy(changed, form)
	changed.Set(name, value)
	return changed
}

// liveBroker is one running broker binary as this test drives it: a client that signs an RFC 9449
// DPoP proof of the caller's key for every request, which is what the exchange judges a
// credential-less caller on, and a plain one for the introspection route, which takes a listed
// caller's ServiceAccount token instead.
type liveBroker struct {
	base  string
	dpop  *http.Client
	plain *http.Client
}

// newPlainBroker drives the broker listening at addr with no key: the routes a listed caller
// reaches present that caller's ServiceAccount token and sign no proof.
func newPlainBroker(addr string) *liveBroker {
	return &liveBroker{base: "http://" + addr, plain: &http.Client{}}
}

// newLiveBroker drives the broker listening at addr, signing a proof of key for every request the
// exchange judges a credential-less caller on.
func newLiveBroker(addr string, key *ecdsa.PrivateKey) *liveBroker {
	live := newPlainBroker(addr)
	live.dpop = &http.Client{Transport: brokerclient.NewDPoPTransport(key, http.DefaultTransport)}
	return live
}

// exchange posts an RFC 8693 token exchange, with a DPoP proof of the caller's key, and answers
// the status and the body the broker gave.
func (l *liveBroker) exchange(t *testing.T, form url.Values) (int, map[string]any) {
	t.Helper()
	return l.post(t, l.dpop, "/v1/oauth/token", form, "")
}

// introspect reads token back the way a listed caller does: its own ServiceAccount token on the
// Authorization header, no proof, the token in the form. An empty bearer sends no header at all.
func (l *liveBroker) introspect(t *testing.T, token, bearer string) (int, map[string]any) {
	t.Helper()
	return l.post(t, l.plain, "/v1/oauth/introspect", url.Values{"token": {token}}, bearer)
}

// registerPodSession records that the pod podToken names may exchange under session, the way a
// deployment's daemon does: its own ServiceAccount login token on the Authorization header, the
// pod's enrolment-audience token and the name in a JSON body. An empty bearer sends no header.
func (l *liveBroker) registerPodSession(t *testing.T, podToken, session, bearer string) (int, map[string]any) {
	t.Helper()
	return l.postJSON(t, "/v1/oauth/exchange/pod-sessions", map[string]any{"pod_token": podToken, "session": session}, bearer)
}

// registered registers the pod for session and fails the test unless the broker took it: a step
// a test makes on the way to an exchange, not the thing it is proving.
func (l *liveBroker) registered(t *testing.T, podToken, session, bearer string) {
	t.Helper()
	if status, body := l.registerPodSession(t, podToken, session, bearer); status != http.StatusOK || body["session"] != session {
		t.Fatalf("registering a pod for %s = %d %v, want 200 naming that session", session, status, body)
	}
}

// brokerCall is one request to the live broker as send makes it: the method and path, the body
// and the content type it is sent under (empty for a request that carries neither), the bearer
// whose Authorization header goes on when it is not empty, and the request half as the transcript
// shows it, with every credential named rather than repeated.
type brokerCall struct {
	method      string
	path        string
	contentType string
	body        string
	bearer      string
	shown       string
}

// send sends one request to the broker, reads its answer and logs both halves as the live run's
// transcript. The answer is handed back as the bytes the broker sent, because what a route's
// answer has to be is its caller's to hold it to: JSON for most of them, and RFC 7009's empty
// body for a revocation that worked.
func (l *liveBroker) send(t *testing.T, client *http.Client, call brokerCall) (int, []byte) {
	t.Helper()
	request, err := http.NewRequest(call.method, l.base+call.path, strings.NewReader(call.body))
	if err != nil {
		t.Fatal(err)
	}
	if call.contentType != "" {
		request.Header.Set("Content-Type", call.contentType)
	}
	if call.bearer != "" {
		request.Header.Set("Authorization", "Bearer "+call.bearer)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("%s %s: %v", call.method, call.path, err)
	}
	defer response.Body.Close()
	answer, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("> %s %s %s\n< %d %s", call.method, call.path, call.shown, response.StatusCode, shownAnswer(answer))
	return response.StatusCode, answer
}

// post sends one form to the broker and answers the status and the JSON body it gave.
func (l *liveBroker) post(t *testing.T, client *http.Client, path string, form url.Values, bearer string) (int, map[string]any) {
	t.Helper()
	status, answer := l.send(t, client, brokerCall{
		method: http.MethodPost, path: path, contentType: formContentType,
		body: form.Encode(), bearer: bearer, shown: shownForm(form),
	})
	var body map[string]any
	if err := json.Unmarshal(answer, &body); err != nil {
		t.Fatalf("POST %s answered %d %s, which is not JSON: %v", path, status, answer, err)
	}
	return status, body
}

// postJSON sends one JSON body to the broker, with no DPoP proof, every credential in the body
// named in the transcript rather than repeated.
func (l *liveBroker) postJSON(t *testing.T, path string, fields map[string]any, bearer string) (int, map[string]any) {
	t.Helper()
	encoded, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	shown := map[string]any{}
	for name, value := range fields {
		if credentialFields[name] || name == "pod_token" {
			shown[name] = "<" + name + ">"
			continue
		}
		shown[name] = value
	}
	status, answer := l.send(t, l.plain, brokerCall{
		method: http.MethodPost, path: path, contentType: "application/json",
		body: string(encoded), bearer: bearer, shown: fmt.Sprint(shown),
	})
	var body map[string]any
	if err := json.Unmarshal(answer, &body); err != nil {
		t.Fatalf("POST %s answered %d %s, which is not JSON: %v", path, status, answer, err)
	}
	return status, body
}

// formContentType is what an OAuth request's form body is sent as (RFC 6749 Section 3.2).
const formContentType = "application/x-www-form-urlencoded"

// credentialFields are the request fields whose value is a credential; the transcript names those
// and shows every other field in full, since a refusal's own sentence is the evidence.
var credentialFields = map[string]bool{"subject_token": true, "token": true, "client_secret": true}

// shownForm is a request's form as the transcript shows it.
func shownForm(form url.Values) string {
	shown := url.Values{}
	for name, values := range form {
		if credentialFields[name] {
			shown.Set(name, "<"+name+">")
			continue
		}
		shown[name] = values
	}
	return shown.Encode()
}

// shownAnswer is the broker's answer as the transcript shows it: what it sent, with the access
// token a token answer carries named rather than repeated. An answer that is not a JSON object —
// a revocation's empty body, or a refusal in some other form — is shown as it came, since it
// carries no token to name.
func shownAnswer(answer []byte) string {
	shown := strings.TrimSpace(string(answer))
	var body map[string]any
	if err := json.Unmarshal(answer, &body); err != nil {
		return shown
	}
	if token, _ := body["access_token"].(string); token != "" {
		shown = strings.ReplaceAll(shown, token, "<access_token>")
	}
	return shown
}
