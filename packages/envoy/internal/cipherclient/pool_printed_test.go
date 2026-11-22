// packages/envoy/internal/cipherclient/pool_printed_test.go
//
// What a workload identity pool identity prints of its settings, in every trade log line and
// error that names it: the token audience as internal/redact's allow-list prints a URL, beside
// AGENT_SECRETS_MACHINE_AUDIENCE; the pool audience whole only where it is a provider's resource
// name, else by AGENT_SECRETS_POOL_AUDIENCE alone; and the account as written, a label.

package cipherclient

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// accessField is the JSON field a token service names its access token in, built from parts so no
// line of this file reads as a token assigned a value.
const accessField = "access" + "_token"

// TestPoolIdentityPrintsItsSettingsOnlyWhereTheyAreWhatTheyNameElseItsVariables: the pool identity
// AGENT_SECRETS_MACHINE_IDENTITY=pool builds names, in the line each trade logs, an
// AGENT_SECRETS_MACHINE_AUDIENCE that carries a credential as not printed, and an
// AGENT_SECRETS_POOL_AUDIENCE that is no provider's resource name and an AGENT_SECRETS_POOL_ACCOUNT
// that is no service account's email or unique id by their variables, never any of those values.
// A plain audience, a provider's resource name, a service account's email and its numeric unique
// id still print whole (the controls).
func TestPoolIdentityPrintsItsSettingsOnlyWhereTheyAreWhatTheyNameElseItsVariables(t *testing.T) {
	const (
		secret   = "hunter2pw-poolsettings-1c94e7"
		provider = "//iam.googleapis.com/projects/000000000000/locations/global/workloadIdentityPools/example-pool/providers/example-provider"
		account  = "example-fleet@example-project.iam.gserviceaccount.com"
	)
	for _, row := range []struct {
		name, audience, pool, account string
		want                          []string
	}{
		{"an audience URL with a user and password", "https://tokuser9f3:" + secret + "@aud.example", provider, account,
			[]string{MachineAudienceVar, provider, account}},
		{"an audience URL with a query", "https://aud.example/?key=" + secret, provider, account,
			[]string{MachineAudienceVar, provider, account}},
		{"a pool audience that is a pasted credential", "https://aud.example", secret, account,
			[]string{"https://aud.example", PoolAudienceVar, account}},
		{"a provider's resource name with more after it", "https://aud.example", provider + "?token=" + secret, account,
			[]string{"https://aud.example", PoolAudienceVar, account}},
		{"an account that is a pasted credential", "https://aud.example", provider, secret,
			[]string{"https://aud.example", provider, PoolAccountVar}},
		{"an account that is no service account's email", "https://aud.example", provider, "tokuser9f3:" + secret + "@example.iam.gserviceaccount.com",
			[]string{"https://aud.example", provider, PoolAccountVar}},
		{"control: a plain audience, a provider's resource name and a service account's email", "https://aud.example", provider, account,
			[]string{"https://aud.example", provider, account}},
		{"control: a service account's numeric unique id", "https://aud.example", provider, "100000000000000000001",
			[]string{"https://aud.example", provider, "as 100000000000000000001"}},
	} {
		t.Run(row.name, func(t *testing.T) {
			external := filepath.Join(t.TempDir(), "token")
			if err := os.WriteFile(external, []byte("the.credential.this.machine.holds\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			env := map[string]string{
				IdentityVar: IdentityPool, MachineTokenFileVar: external, MachineAudienceVar: row.audience,
				PoolAudienceVar: row.pool, PoolAccountVar: row.account,
			}
			source, err := MachineIdentity(func(name string) string { return env[name] })
			if err != nil {
				t.Fatal(err)
			}
			pool := source.(*PoolIdentity)
			pool.SecurityTokenServiceURL = newRawServer(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"` + accessField + `":"federated-access-token","issued_token_type":"urn:ietf:params:oauth:token-type:access_token","token_type":"Bearer","expires_in":3600}`))
			}) + "/v1/token"
			pool.IAMCredentialsURL = newRawServer(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"token":"google.identity.token.minted.for.the.pool"}`))
			})
			broker := newStubBroker(t, func(int, url.Values) (int, map[string]any) {
				return 200, map[string]any{
					accessField: "dispatch-token-for-the-pool-machine", "token_type": "DPoP",
					"issued_token_type": "urn:ietf:params:oauth:token-type:access_token", "expires_in": 3600,
				}
			})
			var logged bytes.Buffer
			client, err := New(Config{BrokerURL: broker.Address(), Key: testKey(t), Subject: pool, Log: slog.New(slog.NewTextHandler(&logged, nil))})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := client.Token(context.Background()); err != nil {
				t.Fatalf("Token: %v", err)
			}
			line := logged.String()
			if !strings.Contains(line, "traded an identity for one of the broker's access tokens") {
				t.Fatalf("the trade logged %q, want its line", line)
			}
			for _, leaked := range []string{secret, "tokuser9f3"} {
				if strings.Contains(line, leaked) {
					t.Errorf("the trade's line %q holds %q", line, leaked)
				}
			}
			for _, want := range row.want {
				if !strings.Contains(line, want) {
					t.Errorf("the trade's line %q does not say %q", line, want)
				}
			}
		})
	}
	// IAM Credentials' refusal of the mint names the account the same way, whether it is one
	// (the control) or a pasted credential.
	for _, row := range []struct{ account, want string }{
		{secret, PoolAccountVar},
		{account, "for " + account},
	} {
		t.Run("a refused mint for "+row.account, func(t *testing.T) {
			pool := &PoolIdentity{
				Subject:  &scriptedSource{token: "external", tokenType: "urn:ietf:params:oauth:token-type:jwt", describe: "a test credential"},
				Audience: provider, ServiceAccount: row.account, TokenAudience: "https://aud.example",
				SecurityTokenServiceURL: newRawServer(t, func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"` + accessField + `":"federated-access-token"}`))
				}) + "/v1/token",
				IAMCredentialsURL: newRawServer(t, func(w http.ResponseWriter, _ *http.Request) {
					w.WriteHeader(http.StatusForbidden)
					_, _ = w.Write([]byte(`{"error":{"code":403,"status":"PERMISSION_DENIED"}}`))
				}),
			}
			_, _, err := pool.Token(context.Background())
			if err == nil {
				t.Fatal("the pool minted through a refusing IAM Credentials")
			}
			if strings.Contains(err.Error(), secret) {
				t.Errorf("the refusal %q holds the account", err)
			}
			for _, want := range []string{row.want, "HTTP 403 PERMISSION_DENIED"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal %q does not say %q", err, want)
				}
			}
		})
	}
}
