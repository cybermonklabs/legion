// packages/envoy/internal/cipher/helper/broker_url_join_test.go
//go:build linux

package helper

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"

	"github.com/sjawhar/envoy/cipheraddr"
	"github.com/sjawhar/envoy/internal/cipher/proof"
	"github.com/sjawhar/envoy/internal/cipherclient"
)

// proofHTU is the htu the Proof header compact names, read without verifying it, "" for no
// header and "unreadable" for one that is no JWS.
func proofHTU(compact string) string {
	if compact == "" {
		return ""
	}
	sig, err := jose.ParseSigned(compact, []jose.SignatureAlgorithm{jose.ES256})
	if err != nil {
		return "unreadable"
	}
	var claims struct {
		HTU string `json:"htu"`
	}
	if json.Unmarshal(sig.UnsafePayloadWithoutVerification(), &claims) != nil {
		return "unreadable"
	}
	return claims.HTU
}

// TestTheHelperJoinsItsBrokerCallsOntoTheAddress: the helper's broker address (AGENT_SECRETS_URL)
// can carry a path, a query, user information and a port. Each call the helper makes of the broker
// - an enrolment, a renew, a revoke, the retire of a replaced login, the machine login's device
// authorization and its token refresh - goes to the address's path with the route joined onto it
// and the address's query kept as a query, where the route appended to the address's text sat
// inside the query (/broker?sig=.../v1/enrollments, the broker asked /broker) (query,
// query-trailing-slash). Each proof names the call's URL as the broker checks it
// (proof.TargetURI): no query, and none of the user information the request still carries
// (user-information), which the broker's public URL never holds. An address with neither is called
// where it was (no-query). A stand-in records the path, the query, the Basic user and the proof's
// htu each call reaches it with.
func TestTheHelperJoinsItsBrokerCallsOntoTheAddress(t *testing.T) {
	type reached struct{ method, path, query, user, htu string }
	calls := make(chan reached, 6)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, _, _ := r.BasicAuth()
		calls <- reached{r.Method, r.URL.Path, r.URL.RawQuery, user, proofHTU(r.Header.Get("Proof"))}
		lease := map[string]string{"enrollment_id": "enr-1", "lease_expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)}
		switch {
		case strings.HasSuffix(r.URL.Path, "/v1/enrollments"):
			writeJSON(w, http.StatusCreated, lease)
		case strings.HasSuffix(r.URL.Path, "/renew"):
			writeJSON(w, http.StatusOK, lease)
		case strings.Contains(r.URL.Path, "/v1/oauth/"):
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_request"})
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")
	at := "http://" + host + "/broker"
	for _, c := range []struct{ name, address, query, user string }{
		{"query", at + "?sig=k3y-7Qz4k", "sig=k3y-7Qz4k", ""},
		{"query-trailing-slash", at + "/?sig=k3y-7Qz4k", "sig=k3y-7Qz4k", ""},
		{"user-information", "http://" + "ada:pass-7Qz4k@" + host + "/broker", "", "ada"},
		{"no-query", at, "", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			b := &Broker{URL: cipheraddr.New(c.address), OperatorFile: operatorFile(t, "ada@example.com"), MachineFile: machineFilePath(t),
				HTTP: srv.Client(), Log: slog.New(slog.DiscardHandler)}
			t.Cleanup(b.Close)
			key, err := proof.NewKey()
			if err != nil {
				t.Fatal(err)
			}
			authority := &cipherclient.Authority{Key: key, CredentialID: "lcred-1"}
			ctx := context.Background()
			if _, _, err := b.enrollUnder(ctx, authority, nil, "box", "runtime-1", "thumbprint-1", nil, nil); err != nil {
				t.Errorf("enrollUnder: %v", err)
			}
			if _, err := b.renewIdentity(ctx, identity{EnrollmentID: "enr-1", Key: key}); err != nil {
				t.Errorf("renewIdentity: %v", err)
			}
			if err := b.revokeUnder(ctx, authority, nil, "enr-1"); err != nil {
				t.Errorf("revokeUnder: %v", err)
			}
			b.retireReplaced(&machineCredential{key: key, id: "lcred-0"})
			// The stand-in refuses both OAuth calls: the call itself is what the row reads.
			_, _ = b.Login(ctx, "helper-host")
			spent := "refresh-1"
			_, _, _ = b.redeem(ctx, &machineCredential{key: key, id: "lcred-1", refreshToken: spent})
			for _, want := range []reached{
				{http.MethodPost, "/broker/v1/enrollments", c.query, c.user, at + "/v1/enrollments"},
				{http.MethodPost, "/broker/v1/enrollments/enr-1/renew", c.query, c.user, at + "/v1/enrollments/enr-1/renew"},
				{http.MethodDelete, "/broker/v1/enrollments/enr-1", c.query, c.user, at + "/v1/enrollments/enr-1"},
				{http.MethodPost, "/broker/v1/operator/machines/lcred-0/retire", c.query, c.user, at + "/v1/operator/machines/lcred-0/retire"},
				{http.MethodPost, "/broker/v1/oauth/device_authorization", c.query, c.user, ""},
				{http.MethodPost, "/broker/v1/oauth/token", c.query, c.user, ""},
			} {
				select {
				case got := <-calls:
					if got != want {
						t.Errorf("an address %q was called %+v; want %+v", c.address, got, want)
					}
				default:
					t.Fatalf("an address %q: the call %+v never reached the broker", c.address, want)
				}
			}
		})
	}
}

// TestSignLauncherSignsTheOperatorRoutesOfAnAddressWithAQuery: the helper's broker address can carry
// a path, a query, user information and a port, and agent-secrets asks sign-launcher for a proof of
// each operator call's URL as the broker checks it (proof.TargetURI): the address's path with the
// route joined onto it, and no query or user information. The helper signs exactly those four
// routes there (query); a route not under the address's path is still refused. Control: an
// address with neither signs the same routes (no-query).
func TestSignLauncherSignsTheOperatorRoutesOfAnAddressWithAQuery(t *testing.T) {
	const id = "0f9a6c1e-2b7d-4c3a-9e5f-8a1b2c3d4e5f"
	for _, c := range []struct{ name, address string }{
		{"query", "https://" + "ada:pass-7Qz4k@broker.example:8443/broker?sig=k3y-7Qz4k"},
		{"no-query", "https://broker.example:8443/broker"},
	} {
		t.Run(c.name, func(t *testing.T) {
			key, err := proof.NewKey()
			if err != nil {
				t.Fatal(err)
			}
			b := &Broker{URL: cipheraddr.New(c.address), OperatorFile: operatorFile(t, "ada@example.com"), MachineFile: machineFilePath(t)}
			b.cred.Store(&machineCredential{key: key, id: "lcred-1", gone: make(chan struct{})})
			r := serveBroker(t, b, "", slog.New(slog.DiscardHandler))
			tp, err := proof.Thumbprint(&key.PublicKey)
			if err != nil {
				t.Fatal(err)
			}
			v := &proof.Verifier{Skew: time.Minute,
				LookupLauncher: func(_ context.Context, launcher string) (string, bool, error) { return tp, launcher == "lcred-1", nil },
				Replay:         func(context.Context, string, time.Time) (bool, error) { return true, nil }}
			at := "https://broker.example:8443/broker"
			for _, call := range []struct{ method, url string }{
				{http.MethodGet, at + "/v1/operator/machines"},
				{http.MethodPost, at + "/v1/operator/machines/" + id + "/revoke"},
				{http.MethodGet, at + "/v1/operator/grants"},
				{http.MethodPost, at + "/v1/operator/grants/" + id + "/revoke"},
			} {
				resp := r.call(t, Request{Op: "sign-launcher", Method: call.method, URL: call.url})
				if !resp.OK || resp.Proof == "" {
					t.Errorf("sign-launcher %s %s at an address %q: %+v; want a proof", call.method, call.url, c.address, resp)
					continue
				}
				if sub, err := v.Verify(context.Background(), resp.Proof, call.method, call.url, time.Now()); err != nil || sub.LauncherID != "lcred-1" {
					t.Errorf("%s %s: the proof = %+v, %v; want a launcher proof of lcred-1 for that URL", call.method, call.url, sub, err)
				}
			}
			outside := r.call(t, Request{Op: "sign-launcher", Method: http.MethodGet, URL: "https://broker.example:8443/v1/operator/machines"})
			if outside.OK || outside.Code != CodeBadRequest || outside.Proof != "" {
				t.Errorf("a route not under the address's path: %+v; want BAD_REQUEST and no proof", outside)
			}
		})
	}
}
