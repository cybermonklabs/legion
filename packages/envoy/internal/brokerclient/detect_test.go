// packages/envoy/internal/brokerclient/detect_test.go
//
// DetectMachineIdentity finds the identity a machine can trade at the broker with no setting of
// its own, from the exchange audiences the broker's settings name: a projected Kubernetes token
// minted for the exchange audience, an external_account credential file with a service account
// to act as, or the Compute Engine metadata server, in that order. Each row runs the real
// readers against local stand-ins: a token file at DefaultProjectedTokenFile, a credentials file
// GOOGLE_APPLICATION_CREDENTIALS names, and a metadata server GCE_METADATA_HOST points at.

package brokerclient

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/oauthwire"
)

// testExchange is the broker's two exchange audiences in these rows: its Google one, which a
// metadata or an external_account identity token must carry, and its Kubernetes one, which a
// projected token must carry.
var testExchange = ExchangeSettings{Google: "agent-secrets-vm", Kubernetes: "agent-secrets-exchange"}

// detectMetadata is a Compute Engine metadata server that names robot@ as the instance's
// service account and counts every request, so a row asserts it was never asked.
type detectMetadata struct{ requests atomic.Int64 }

// startDetectMetadata serves a detectMetadata and points GCE_METADATA_HOST at it for the rest of t.
func startDetectMetadata(t *testing.T) *detectMetadata {
	t.Helper()
	m := &detectMetadata{}
	base := newRawServer(t, func(w http.ResponseWriter, r *http.Request) {
		m.requests.Add(1)
		w.Header().Set("Metadata-Flavor", "Google")
		if r.URL.Path != "/computeMetadata/v1/instance/service-accounts/default/email" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte("machine@example.iam.gserviceaccount.com"))
	})
	t.Setenv("GCE_METADATA_HOST", strings.TrimPrefix(base, "http://"))
	return m
}

// closedAddress is a loopback address nothing listens on.
func closedAddress(t *testing.T) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := lis.Addr().String()
	lis.Close()
	return addr
}

// projectedTokenAt points DefaultProjectedTokenFile at a file of the test's own for the rest of
// t, holding a JWT minted for aud when aud is not nil, and absent otherwise.
func projectedTokenAt(t *testing.T, aud []string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "token")
	previous := DefaultProjectedTokenFile
	DefaultProjectedTokenFile = path
	t.Cleanup(func() { DefaultProjectedTokenFile = previous })
	if aud != nil {
		payload, err := json.Marshal(map[string]any{"aud": aud, "sub": "system:serviceaccount:legion:worker"})
		if err != nil {
			t.Fatal(err)
		}
		jwt := "eyJhbGciOiJSUzI1NiJ9." + base64.RawURLEncoding.EncodeToString(payload) + ".c2lnbmF0dXJl"
		if err := os.WriteFile(path, []byte(jwt+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

// externalAccountFile writes an external_account credentials file whose subject token is in a
// file of its own, whose security token service is sts and whose service account to act as is
// robot@ at impersonation, and points GOOGLE_APPLICATION_CREDENTIALS at it.
func externalAccountFile(t *testing.T, sts, impersonation string) {
	t.Helper()
	dir := t.TempDir()
	subject := filepath.Join(dir, "subject")
	if err := os.WriteFile(subject, []byte("the.workload.credential"), 0o600); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(map[string]any{
		"type":                              "external_account",
		"audience":                          "//iam.googleapis.com/projects/1/locations/global/workloadIdentityPools/p/providers/v",
		"subject_token_type":                "urn:ietf:params:oauth:token-type:jwt",
		"token_url":                         sts + "/v1/token",
		"service_account_impersonation_url": impersonation + "/v1/projects/-/serviceAccounts/robot@example.iam.gserviceaccount.com:generateAccessToken",
		"credential_source":                 map[string]any{"file": subject},
	})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "credentials.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", path)
}

// reasons is each skipped source's reason, in order.
func reasons(skipped []Skipped) []string {
	out := make([]string, len(skipped))
	for i, s := range skipped {
		out[i] = s.Reason
	}
	return out
}

func TestMachineIdentityIsFoundWithNoSetting(t *testing.T) {
	t.Run("projected token for the exchange audience", func(t *testing.T) {
		path := projectedTokenAt(t, []string{"agent-secrets-exchange"})
		t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "")
		metadata := startDetectMetadata(t)
		source, skipped, err := DetectMachineIdentity(context.Background(), testExchange)
		if err != nil {
			t.Fatal(err)
		}
		projected, ok := source.(*ProjectedToken)
		if !ok || projected.Path != path {
			t.Fatalf("source = %#v (skipped %v); want the projected token at %s", source, reasons(skipped), path)
		}
		if n := metadata.requests.Load(); n != 0 {
			t.Fatalf("the metadata server saw %d requests; a projected token found first asks it nothing", n)
		}
	})

	t.Run("projected token for another audience", func(t *testing.T) {
		projectedTokenAt(t, []string{"agent-secrets"})
		t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "")
		startDetectMetadata(t)
		source, skipped, err := DetectMachineIdentity(context.Background(), testExchange)
		if err != nil {
			t.Fatal(err)
		}
		if len(skipped) == 0 || !strings.Contains(skipped[0].Reason, "agent-secrets-exchange") || !strings.Contains(skipped[0].Reason, `"agent-secrets"`) {
			t.Fatalf("skipped = %v; want the projected token's reason to name both its audience and the broker's", reasons(skipped))
		}
		if metadata, ok := source.(*MetadataIdentity); !ok || metadata.Audience != "agent-secrets-vm" {
			t.Fatalf("source = %#v; want the metadata server's identity once the projected token was skipped", source)
		}
	})

	t.Run("external_account", func(t *testing.T) {
		projectedTokenAt(t, nil)
		var mintPath, mintBearer string
		var mintRequest map[string]any
		sts := newRawServer(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"access_token":"not-a-secret-federated-access-token","issued_token_type":"urn:ietf:params:oauth:token-type:access_token","token_type":"Bearer","expires_in":3600}`))
		})
		impersonation := newRawServer(t, func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer not-a-secret-federated-access-token" {
				http.Error(w, "not the federated token", http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"accessToken":"not-a-secret-robot-access-token","expireTime":"` + time.Now().Add(time.Hour).UTC().Format(time.RFC3339) + `"}`))
		})
		iam := newRawServer(t, func(w http.ResponseWriter, r *http.Request) {
			mintPath, mintBearer = r.URL.Path, r.Header.Get("Authorization")
			mintRequest = readJSONBody(t, r)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"token":"robot-identity-token"}`))
		})
		externalAccountFile(t, sts, impersonation)
		metadata := startDetectMetadata(t)

		source, skipped, err := DetectMachineIdentity(context.Background(), testExchange)
		if err != nil {
			t.Fatal(err)
		}
		external, ok := source.(*ExternalAccountIdentity)
		if !ok {
			t.Fatalf("source = %#v (skipped %v); want the external_account credential", source, reasons(skipped))
		}
		if external.ServiceAccount != "robot@example.iam.gserviceaccount.com" || external.Audience != "agent-secrets-vm" {
			t.Fatalf("external_account identity acts as %q for %q; want robot@example.iam.gserviceaccount.com for agent-secrets-vm", external.ServiceAccount, external.Audience)
		}
		external.IAMCredentialsURL = iam
		token, tokenType, err := external.Token(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if token != "robot-identity-token" || tokenType != oauthwire.SubjectTokenTypeGoogle {
			t.Fatalf("Token = %q, %q; want the identity token generateIdToken minted, as a Google identity token", token, tokenType)
		}
		if want := "/v1/projects/-/serviceAccounts/robot@example.iam.gserviceaccount.com:generateIdToken"; mintPath != want {
			t.Fatalf("generateIdToken was called at %s, want %s", mintPath, want)
		}
		if mintBearer != "Bearer not-a-secret-robot-access-token" {
			t.Fatalf("generateIdToken was called with %q; want the access token the credentials file mints", mintBearer)
		}
		if mintRequest["audience"] != "agent-secrets-vm" || mintRequest["includeEmail"] != true {
			t.Fatalf("generateIdToken was asked %v; want audience agent-secrets-vm and includeEmail", mintRequest)
		}
		if n := metadata.requests.Load(); n != 0 {
			t.Fatalf("the metadata server saw %d requests; a credentials file found first asks it nothing", n)
		}
	})

	t.Run("credentials file does not parse", func(t *testing.T) {
		projectedTokenAt(t, nil)
		path := filepath.Join(t.TempDir(), "credentials.json")
		if err := os.WriteFile(path, []byte("not json"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", path)
		metadata := startDetectMetadata(t)
		source, _, err := DetectMachineIdentity(context.Background(), testExchange)
		if err == nil || source != nil || !strings.Contains(err.Error(), path) {
			t.Fatalf("DetectMachineIdentity = %#v, %v; want an error naming %s, which is present but broken", source, err, path)
		}
		if n := metadata.requests.Load(); n != 0 {
			t.Fatalf("the metadata server saw %d requests; a broken credentials file stops before it", n)
		}
	})

	t.Run("metadata server", func(t *testing.T) {
		projectedTokenAt(t, nil)
		t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "")
		startDetectMetadata(t)
		source, skipped, err := DetectMachineIdentity(context.Background(), testExchange)
		if err != nil {
			t.Fatal(err)
		}
		if metadata, ok := source.(*MetadataIdentity); !ok || metadata.Audience != "agent-secrets-vm" {
			t.Fatalf("source = %#v (skipped %v); want the metadata server's identity for agent-secrets-vm", source, reasons(skipped))
		}
	})

	t.Run("none", func(t *testing.T) {
		path := projectedTokenAt(t, nil)
		t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "")
		t.Setenv("GCE_METADATA_HOST", closedAddress(t))
		start := time.Now()
		source, skipped, err := DetectMachineIdentity(context.Background(), testExchange)
		elapsed := time.Since(start)
		if err != nil || source != nil {
			t.Fatalf("DetectMachineIdentity = %#v, %v; want no identity and no error", source, err)
		}
		want := []string{"no projected token at " + path, "GOOGLE_APPLICATION_CREDENTIALS is unset", "no Google metadata server"}
		if got := reasons(skipped); strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Fatalf("skipped = %q; want %q", got, want)
		}
		if elapsed > 2*time.Second {
			t.Fatalf("finding no identity took %s; a laptop must learn it has none within 2 s", elapsed)
		}
	})

	t.Run("no Google audience", func(t *testing.T) {
		projectedTokenAt(t, nil)
		externalAccountFile(t, "http://127.0.0.1:1", "http://127.0.0.1:1")
		metadata := startDetectMetadata(t)
		source, skipped, err := DetectMachineIdentity(context.Background(), ExchangeSettings{Kubernetes: "agent-secrets-exchange"})
		if err != nil || source != nil {
			t.Fatalf("DetectMachineIdentity = %#v, %v; want no identity and no error", source, err)
		}
		got := reasons(skipped)
		if len(got) != 3 || !strings.Contains(got[1], "no Google exchange audience") || !strings.Contains(got[2], "no Google exchange audience") {
			t.Fatalf("skipped = %q; want the credentials file and the metadata server skipped because the broker names no Google exchange audience", got)
		}
		if n := metadata.requests.Load(); n != 0 {
			t.Fatalf("the metadata server saw %d requests; with no Google audience nothing asks it", n)
		}
	})

	t.Run("no Kubernetes audience", func(t *testing.T) {
		projectedTokenAt(t, []string{"agent-secrets-exchange"})
		t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "")
		startDetectMetadata(t)
		source, skipped, err := DetectMachineIdentity(context.Background(), ExchangeSettings{Google: "agent-secrets-vm"})
		if err != nil {
			t.Fatal(err)
		}
		if len(skipped) == 0 || !strings.Contains(skipped[0].Reason, "no Kubernetes exchange audience") {
			t.Fatalf("skipped = %q; want the projected token skipped because the broker names no Kubernetes exchange audience", reasons(skipped))
		}
		if _, ok := source.(*MetadataIdentity); !ok {
			t.Fatalf("source = %#v; want the metadata server's identity", source)
		}
	})
}
