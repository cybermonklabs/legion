package cipherclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/sjawhar/envoy/internal/redact/redacttest"
	"github.com/sjawhar/envoy/redirects/redirectstest"
)

// bodyEcho is a stand-in in a server's place that answers every request status, with a Location
// sending the client to target with the credential the request carried in its body under field (a
// JSON object's member, else a form's field) in its query (redirectstest.To), as whoever answers
// there can.
func bodyEcho(status int, target, field string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var credential string
		if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			var fields map[string]any
			_ = json.NewDecoder(r.Body).Decode(&fields)
			credential, _ = fields[field].(string)
		} else {
			credential = r.PostFormValue(field)
		}
		w.Header().Set("Location", redirectstest.To(target, credential))
		w.WriteHeader(status)
	}
}

// echoIssuer serves, until t ends, a sign-in issuer whose discovery document is its own and whose
// token endpoint is token (signInStandIn's shape), and answers its URL.
func echoIssuer(t *testing.T, token http.HandlerFunc) string {
	t.Helper()
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer": server.URL, "authorization_endpoint": server.URL + "/authorize", "token_endpoint": server.URL + "/token",
			"device_authorization_endpoint": server.URL + "/device", "jwks_uri": server.URL + "/keys",
		})
	})
	mux.Handle("POST /token", token)
	return server.URL
}

// TestAGoogleOrIssuerCallFollowsNoRedirect: whatever answers in the place of Google's security
// token service, IAM Credentials' generateIdToken or a sign-in issuer's token endpoint answers a
// call that carries a credential with a redirect to a stand-in on the same host name at another
// port, whose requests the stand-in keeps (redirectstest.Sent), and whose Location quotes that
// credential in its query: the subject token a workload identity pool's exchange carries in its
// JSON body (pool-sts, a 307, which would send the body again), the federated access token
// generateIdToken carries as its bearer (pool-iam, a 302: net/http copies Authorization to the
// same host name), the device code a sign-in's poll carries in its form (device), the refresh
// token a renewal carries in its form (refresh), and the subject token an external_account
// credential's own exchange carries in its form (external-account). Each call goes through the
// client production gives it (HTTPClient). The stand-in is sent nothing, so no request carries the
// credential on to it, and the call fails naming the redirect's status and not its Location. The
// twin shows a client that follows each of these redirects sends the stand-in the credential, so
// the rows can see one sent on. Captures cipherclient.redirect-followed/<case>, what the stand-in
// was sent.
func TestAGoogleOrIssuerCallFollowsNoRedirect(t *testing.T) {
	const (
		poolSubject     = "pool-subject-RDX-5e1b7c93"
		federated       = "federated-access-RDX-a04d2e68"
		deviceCode      = "device-code-RDX-73c9f1a2"
		refresh         = "person-refresh-RDX-9b2e4d07"
		externalSubject = "wlsubj-RDX-1f6a8c35"
		clientWord      = "cs-RDX-6d0b4e17"
		provider        = "//iam.googleapis.com/projects/000000000000/locations/global/workloadIdentityPools/example-pool/providers/example-provider"
		account         = "example-fleet@example-project.iam.gserviceaccount.com"
	)
	target, sent := redirectstest.Sent(t)
	bearerTo := func(bearer string) string { return redirectstest.To(target, bearer) }
	t.Run("twin", func(t *testing.T) {
		const credential = "followed-twin-RDX-8e3f5a10"
		post := func(t *testing.T, endpoint, header, value, body string) *http.Request {
			t.Helper()
			r, err := http.NewRequest(http.MethodPost, endpoint, strings.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			r.Header.Set(header, value)
			return r
		}
		for _, r := range []*http.Request{
			// a JSON body's 307
			post(t, newRawServer(t, bodyEcho(http.StatusTemporaryRedirect, target, "subjectToken")),
				"Content-Type", "application/json", `{"subjectToken":"`+credential+`"}`),
			// a bearer's 302
			post(t, redirectstest.EchoBearer(t, bearerTo), "Authorization", "Bearer "+credential, ""),
			// a form's 302
			post(t, echoIssuer(t, bodyEcho(http.StatusFound, target, "device_code"))+"/token",
				"Content-Type", "application/x-www-form-urlencoded", url.Values{"device_code": {credential}}.Encode()),
		} {
			redirectstest.RequireSentOn(t, r, credential, sent)
		}
	})
	dead := "http://" + redirectstest.Dead(t)
	pool := func(t *testing.T, sts, iam string) error {
		identity := &PoolIdentity{
			Subject: NewProjectedToken(writeSubject(t, poolSubject)), Audience: provider, ServiceAccount: account,
			TokenAudience: "example-client", SecurityTokenServiceURL: sts, IAMCredentialsURL: iam, HTTP: HTTPClient(),
		}
		_, _, err := identity.Token(context.Background())
		return err
	}
	signIn := func(t *testing.T, field string) *DeviceFlow {
		return &DeviceFlow{Issuer: echoIssuer(t, bodyEcho(http.StatusFound, target, field)), ClientID: "device-client", ClientSecret: clientWord, HTTP: HTTPClient()}
	}
	for _, c := range []struct {
		name, status string
		credentials  []string
		call         func(t *testing.T) error
	}{
		{"pool-sts", "HTTP 307", []string{poolSubject}, func(t *testing.T) error {
			return pool(t, newRawServer(t, bodyEcho(http.StatusTemporaryRedirect, target, "subjectToken")), dead)
		}},
		{"pool-iam", "HTTP 302", []string{federated}, func(t *testing.T) error {
			sts := newRawServer(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]string{"access_token": federated})
			})
			return pool(t, sts, redirectstest.EchoBearer(t, bearerTo))
		}},
		{"device", "HTTP 302", []string{deviceCode, clientWord}, func(t *testing.T) error {
			_, err := signIn(t, "device_code").Wait(context.Background(), &oauth2.DeviceAuthResponse{DeviceCode: deviceCode, Interval: 1, Expiry: time.Now().Add(time.Minute)})
			return err
		}},
		{"refresh", "HTTP 302", []string{refresh, clientWord}, func(t *testing.T) error {
			_, err := signIn(t, "refresh_token").Refresh(context.Background(), refresh)
			if errors.Is(err, ErrSignInEnded) {
				t.Error("a redirected renewal reads as a sign-in the issuer ended")
			}
			return err
		}},
		{"external-account", "HTTP 302", []string{externalSubject}, func(t *testing.T) error {
			identity := &ExternalAccountIdentity{
				CredentialsJSON: confidentialExternalAccountJSON(t, externalSubject, newRawServer(t, bodyEcho(http.StatusFound, target, "subject_token")), dead),
				ServiceAccount:  "robot@example.iam.gserviceaccount.com", Audience: testAudience, HTTP: HTTPClient(), IAMCredentialsURL: dead,
			}
			_, _, err := identity.Token(context.Background())
			return err
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := c.call(t)
			text := sent()
			redacttest.RequireNoPiece(t, "cipherclient.redirect-followed/"+c.name, text, c.credentials...)
			if text != "" {
				t.Errorf("the stand-in the Location names was asked: %q", text)
			}
			if err == nil || !strings.Contains(err.Error(), c.status) {
				t.Fatalf("a redirected call answered %v, want it refused naming the redirect's %s", err, c.status)
			}
			if strings.Contains(err.Error(), target) || redacttest.Piece(err.Error(), c.credentials[0]) != "" {
				t.Errorf("a redirected call's error %q names the Location", err)
			}
		})
	}
}
