// person_subject_test.go drives the REAL compiled broker through an address moving from one
// Google account to another, over HTTP, on a real Postgres, against a local OpenID issuer standing
// in for Google: the replaced account signs in on a box and approves a machine login; the account
// that holds the address now signs in two seconds after the move; Dispatch's revoke names the
// replaced account; and what that account made ends with its tokens, while the holder's sign-ins
// trade on, whether made before the revoke landed or after it. The replaced account's identity
// token minted before the move is refused on a box key it never traded, which is the trade a time
// cutoff cannot see.
package main

import (
	"crypto/ecdsa"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
	"github.com/sjawhar/envoy/internal/cipher/proof"
	"github.com/sjawhar/envoy/internal/cipher/record"
	"github.com/sjawhar/envoy/internal/cipher/store/storetest"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

// The two Google accounts of the one address: the one Google gave it first, and the one it gave
// it to after. Google names an account by its `sub`, which the address does not change.
const (
	replacedSub = "109000000000000000001"
	holderSub   = "109000000000000000002"
)

// personTokenOf is a person's own Google identity token for login as the account sub, the token
// the device grant answers `agent-secrets login`.
func personTokenOf(t *testing.T, google *oidctest.Issuer, key *oidctest.Key, sub, login string) string {
	t.Helper()
	claims := google.Claims(sub, deviceAudience)
	claims["email"] = login
	claims["email_verified"] = true
	claims["hd"] = signInDomain
	return google.Mint(t, key, claims)
}

// approvedMachineLogin logs one machine of person in through the real binary as
// agent-secrets-helper does, approved through the dispatch caller as the account approverSub of
// issuer, and polls its device code. It answers the access token the login was issued.
func (l *liveBroker) approvedMachineLogin(t *testing.T, key *ecdsa.PrivateKey, bearer, issuer, approverSub string) string {
	t.Helper()
	thumbprint, err := proof.Thumbprint(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	request, err := record.Sign(key, l.base, []record.AuthorizationDetail{
		{Type: record.KindLauncherCredential, Identifier: revocationHost},
	}, "", person, nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	status, body := l.post(t, l.plain, "/v1/oauth/device_authorization", url.Values{
		"client_id": {oauthwire.ClientID}, "scope": {oauthwire.Scope},
		"dpop_jkt": {thumbprint}, "request": {request},
	}, "")
	deviceCode, _ := body["device_code"].(string)
	userCode, _ := body["user_code"].(string)
	if status != http.StatusOK || deviceCode == "" || userCode == "" {
		t.Fatalf("the machine login's device authorization = %d %v, want 200 with a device code and a user code", status, body)
	}
	status, body = l.postJSON(t, "/v1/machine-logins/lookup", map[string]any{"code": userCode}, bearer)
	recordID, _ := body["record_id"].(string)
	if status != http.StatusOK || recordID == "" {
		t.Fatalf("looking the machine login up by its code = %d %v, want 200 with a record id", status, body)
	}
	status, body = l.postJSON(t, "/v1/credential-requests/"+recordID+"/approve", map[string]any{
		"approver": person, "code": userCode, "approver_issuer": issuer, "approver_subject": approverSub,
	}, bearer)
	if status != http.StatusOK {
		t.Fatalf("%s approves her machine login as %s = %d %v, want 200", person, approverSub, status, body)
	}
	status, body = l.post(t, l.dpop, "/v1/oauth/token", url.Values{
		"grant_type": {deviceCodeGrantType}, "device_code": {deviceCode}, "client_id": {oauthwire.ClientID},
	}, "")
	access, _ := body["access_token"].(string)
	if status != http.StatusOK || access == "" {
		t.Fatalf("the approved machine polls its device code = %d %v, want 200 with an access token", status, body)
	}
	return access
}

// TestTheBinaryRevokesAReplacedAccountBySubject is the live proof of the revoke by account. The
// replaced account's sign-in and machine login, made before the move, end with their tokens; its
// still-valid identity token, minted before the move, trades nothing on a box key it never traded;
// the holder's sign-in made two seconds after the move, before the revoke landed, survives it and
// keeps trading, and so does one made after it; and a repeat revoke answers zeros.
//
// Controls, each in enroll.Service.RevokePersonSubject: end the person's rows created before the
// revoke's own moment and record nothing, as a cutoff by time does, and the replaced account's
// trade on a new box key is answered 200; end the person's rows created before the move plus a
// ten-second margin, whatever their account, and the holder's sign-in made inside the margin is
// ended.
//
// It skips when BROKER_TEST_DATABASE_URL is unset, as every Postgres-backed broker test does.
func TestTheBinaryRevokesAReplacedAccountBySubject(t *testing.T) {
	databaseURL := storetest.URL(t)
	cluster, github, google := oidctest.New(t), oidctest.New(t), oidctest.New(t)
	github.PublishKey(t, "github-key")
	clusterKey := cluster.PublishKey(t, "cluster-key")
	googleKey := google.PublishKey(t, "google-key")
	apiToken := accountToken(t, cluster, clusterKey, apiAccount)
	broker := startBroker(t, brokerOptions{Env: googleExchangeEnv(t, databaseURL, cluster, github, google, longAccessSeconds)})
	plain := newPlainBroker(broker.Addr)
	machineToken := func() string {
		return mintMachineToken(t, google, googleKey, pinnedFleetAccount, machineAudience, true, fleetCloudProject, fleetInstanceName)
	}
	// The replaced account's own identity token, minted before the move and valid for its hour:
	// what a departing account still holds once the address is another's.
	replacedToken := personTokenOf(t, google, googleKey, replacedSub, person)

	replacedBox := newLiveBroker(broker.Addr, proofKey(t))
	replacedSignIn := replacedBox.exchanged(t, signInForm(replacedToken, machineToken(), "ada-replaced-box"))
	machineKey := proofKey(t)
	replacedMachine := newLiveBroker(broker.Addr, machineKey)
	replacedLogin := replacedMachine.approvedMachineLogin(t, machineKey, apiToken, google.URL(), replacedSub)

	// The move, and the holder's first sign-in two seconds after it, before Dispatch's revoke
	// reaches the broker.
	time.Sleep(2 * time.Second)
	holderBox := newLiveBroker(broker.Addr, proofKey(t))
	holderSignIn := holderBox.exchanged(t, signInForm(personTokenOf(t, google, googleKey, holderSub, person), machineToken(), "ada-holder-box"))

	revoke := func(t *testing.T) map[string]any {
		t.Helper()
		status, body := plain.postJSON(t, "/v1/person-subjects/revoke", map[string]any{
			"person": person, "issuer": google.URL(), "subject": replacedSub, "holder_subject": holderSub, "session_generation": 1,
		}, apiToken)
		if status != http.StatusOK || body["state"] != "revoked" {
			t.Fatalf("POST /v1/person-subjects/revoke = %d %v, want 200 revoked", status, body)
		}
		return body
	}
	t.Run("the revoke ends the replaced account's sign-in and machine login", func(t *testing.T) {
		body := revoke(t)
		if body["signins"] != float64(1) || body["machine_logins"] != float64(1) {
			t.Fatalf("the revoke answered %v, want the one sign-in and the one machine login the replaced account made", body)
		}
		if ended, _ := body["tokens_ended"].(float64); ended < 2 {
			t.Fatalf("the revoke ended %v access tokens, want at least the sign-in's and the machine login's", body["tokens_ended"])
		}
		plain.requireActive(t, replacedSignIn, apiToken, false, "the replaced account's sign-in ended with its tokens")
		plain.requireActive(t, replacedLogin, apiToken, false, "the replaced account's machine login ended with its tokens")
	})

	t.Run("the replaced account's token minted before the move trades nothing on a new box key", func(t *testing.T) {
		status, body := newLiveBroker(broker.Addr, proofKey(t)).exchange(t, signInForm(replacedToken, machineToken(), "ada-replaced-new-box"))
		description, _ := body["error_description"].(string)
		if status != http.StatusBadRequest || body["error"] != "invalid_grant" || body["access_token"] != nil {
			t.Fatalf("the replaced account's trade on a new box key = %d %v %q, want 400 invalid_grant and no access token", status, body["error"], description)
		}
		for _, names := range []string{person, "no longer holds"} {
			if !strings.Contains(description, names) {
				t.Errorf("the refusal reads %q, which does not name %q", description, names)
			}
		}
		if status, body := replacedBox.exchange(t, signInForm(replacedToken, machineToken(), "ada-replaced-box")); status != http.StatusBadRequest || body["access_token"] != nil {
			t.Fatalf("the replaced account's trade on its old box = %d %v, want 400 and no access token", status, body)
		}
	})

	t.Run("the holder's sign-in made before the revoke landed survives it and trades on", func(t *testing.T) {
		plain.requireActive(t, holderSignIn, apiToken, true, "the holder's sign-in is the holder's, made two seconds after the move")
		holderBox.exchanged(t, signInForm(personTokenOf(t, google, googleKey, holderSub, person), machineToken(), "ada-holder-box"))
	})

	t.Run("the holder signs in on another box after the revoke and trades", func(t *testing.T) {
		later := newLiveBroker(broker.Addr, proofKey(t))
		token := later.exchanged(t, signInForm(personTokenOf(t, google, googleKey, holderSub, person), machineToken(), "ada-holder-later"))
		plain.requireActive(t, token, apiToken, true, "a sign-in the holder made after the revoke")
	})

	t.Run("a repeat revoke ends nothing", func(t *testing.T) {
		body := revoke(t)
		for _, field := range []string{"signins", "machine_logins", "tokens_ended"} {
			if body[field] != float64(0) {
				t.Errorf("the repeat revoke answered %s %v, want 0", field, body[field])
			}
		}
		plain.requireActive(t, holderSignIn, apiToken, true, "a repeat revoke takes nothing from the holder")
	})
}
