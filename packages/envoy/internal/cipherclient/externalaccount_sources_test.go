// packages/envoy/internal/cipherclient/externalaccount_sources_test.go
//
// Each credential_source an external_account credential file takes its subject token from mints
// the broker's identity token the same way: the file's own exchange at the security token
// service, whose federated access token its service_account_impersonation_url spends on the
// account's access token, which generateIdToken spends. Each row runs the real reader against
// local stand-ins: the file GOOGLE_APPLICATION_CREDENTIALS names, the security token service its
// token_url names, the impersonation URL, and the source of its subject token.

package cipherclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
)

// TestEachCredentialSourceMintsWithTheAccountsAccessToken: a credential file of the url kind (the
// shape `create-cred-config --azure` writes, with a header and a JSON answer), of the executable
// kind and of the aws kind (with the AWS credential in the environment) each sends its subject
// token to the security token service, spends the federated token at its
// service_account_impersonation_url, and spends the account's access token that answers on
// generateIdToken. The executable is told the account it acts as
// (GOOGLE_EXTERNAL_ACCOUNT_IMPERSONATED_EMAIL), and answers no subject token without it. So does
// one whose impersonation key is spelt in another case, which Go's JSON decoder, and so Google's
// library, reads as the same key.
func TestEachCredentialSourceMintsWithTheAccountsAccessToken(t *testing.T) {
	impersonationKey := "service_account_impersonation_url"
	for _, row := range []struct {
		name        string
		subjectType string
		key         string
		source      func(t *testing.T) map[string]any
		subject     func(t *testing.T, got string)
	}{
		{"url", "urn:ietf:params:oauth:token-type:jwt", impersonationKey, func(t *testing.T) map[string]any {
			metadata := newRawServer(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Metadata") != "True" {
					http.Error(w, "no Metadata header", http.StatusBadRequest)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"` + accessField + `":"url-subject-token"}`))
			})
			return map[string]any{"url": metadata + "/token", "headers": map[string]string{"Metadata": "True"},
				"format": map[string]string{"type": "json", "subject_token_field_name": accessField}}
		}, equals("url-subject-token")},
		{"executable", "urn:ietf:params:oauth:token-type:jwt", impersonationKey, func(t *testing.T) map[string]any {
			t.Setenv("GOOGLE_EXTERNAL_ACCOUNT_ALLOW_EXECUTABLES", "1")
			t.Setenv("GOOGLE_EXTERNAL_ACCOUNT_IMPERSONATED_EMAIL", "")
			answer := fmt.Sprintf(`{"version":1,"success":true,"token_type":"urn:ietf:params:oauth:token-type:jwt","id_token":"executable-subject-token","expiration_time":%d}`, time.Now().Add(time.Hour).Unix())
			refusal := `{"version":1,"success":false,"code":"no_account","message":"GOOGLE_EXTERNAL_ACCOUNT_IMPERSONATED_EMAIL is not the account"}`
			command := filepath.Join(t.TempDir(), "subject-token")
			script := "#!/bin/sh\nif [ \"$GOOGLE_EXTERNAL_ACCOUNT_IMPERSONATED_EMAIL\" = robot@example.iam.gserviceaccount.com ]; then\n" +
				"printf '%s' '" + answer + "'\nelse\nprintf '%s' '" + refusal + "'\nfi\n"
			if err := os.WriteFile(command, []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			return map[string]any{"executable": map[string]any{"command": command, "timeout_millis": 5000}}
		}, equals("executable-subject-token")},
		{"aws", "urn:ietf:params:aws:token-type:aws4_request", impersonationKey, func(t *testing.T) map[string]any {
			t.Setenv("AWS_REGION", "us-east-1")
			t.Setenv("AWS_ACCESS_KEY_ID", "AKIDEXAMPLE")
			t.Setenv("AWS_SECRET_ACCESS_KEY", "not-a-secret-aws-secret-access-key")
			t.Setenv("AWS_SESSION_TOKEN", "")
			var asked atomic.Int64
			metadata := newRawServer(t, func(w http.ResponseWriter, _ *http.Request) {
				asked.Add(1)
				http.NotFound(w, nil)
			})
			t.Cleanup(func() {
				if n := asked.Load(); n != 0 {
					t.Errorf("the AWS metadata stand-in saw %d requests; the environment holds the credential", n)
				}
			})
			return map[string]any{"environment_id": "aws1", "region_url": metadata + "/region", "url": metadata + "/credentials",
				"regional_cred_verification_url": "https://sts.{region}.amazonaws.com?Action=GetCallerIdentity&Version=2011-06-15"}
		}, func(t *testing.T, got string) {
			// The subject token is the signed GetCallerIdentity request, URL-encoded JSON.
			decoded, err := url.QueryUnescape(got)
			if err != nil {
				t.Fatalf("the AWS subject token does not unescape: %v", err)
			}
			var request struct {
				URL     string `json:"url"`
				Headers []struct {
					Key, Value string
				} `json:"headers"`
			}
			if err := json.Unmarshal([]byte(decoded), &request); err != nil {
				t.Fatalf("the AWS subject token is not the signed request: %v", err)
			}
			signed := false
			for _, h := range request.Headers {
				signed = signed || (h.Key == "Authorization" && strings.HasPrefix(h.Value, "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/"))
			}
			if !strings.HasPrefix(request.URL, "https://sts.us-east-1.amazonaws.com") || !signed {
				t.Fatalf("the AWS subject token is %s; want GetCallerIdentity at sts.us-east-1.amazonaws.com signed with AKIDEXAMPLE", decoded)
			}
		}},
		{"an impersonation key in another case", "urn:ietf:params:oauth:token-type:jwt", "Service_Account_Impersonation_URL", func(t *testing.T) map[string]any {
			return map[string]any{"file": subjectTokenFile(t, "file-subject-token")}
		}, equals("file-subject-token")},
	} {
		t.Run(row.name, func(t *testing.T) {
			sts, exchange := federatedExchange(t)
			impersonation, impersonated := impersonationStandIn(t)
			iam, _ := accountIAM(t)
			data, err := json.Marshal(map[string]any{
				"type":               "external_account",
				"audience":           "//iam.googleapis.com/projects/1/locations/global/workloadIdentityPools/p/providers/v",
				"subject_token_type": row.subjectType,
				"token_url":          sts + "/v1/token",
				row.key:              "https://iamcredentials.googleapis.com/v1/projects/-/serviceAccounts/robot@example.iam.gserviceaccount.com:generateAccessToken",
				"credential_source":  row.source(t),
			})
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "credentials.json")
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv(credentialsFileVar, path)

			source, reason, err := externalAccountFor(testAudience)
			external, ok := source.(*ExternalAccountIdentity)
			if err != nil || !ok || external.ServiceAccount != "robot@example.iam.gserviceaccount.com" {
				t.Fatalf("externalAccountFor = %#v, %q, %v; want the credential acting as robot@example.iam.gserviceaccount.com", source, reason, err)
			}
			external.IAMCredentialsURL = iam
			external.HTTP = iamCredentialsAt(t, impersonation)
			token, tokenType, err := external.Token(context.Background())
			if err != nil || token != "robot-identity-token" || tokenType != oauthwire.SubjectTokenTypeGoogle {
				t.Fatalf("Token = %q, %q, %v; want the identity token generateIdToken minted", token, tokenType, err)
			}
			sent := exchange()
			row.subject(t, sent.Get("subject_token"))
			for field, want := range map[string]string{"subject_token_type": row.subjectType, "scope": poolScope} {
				if got := sent.Get(field); got != want {
					t.Errorf("the security token service was sent %s %q; want %q", field, got, want)
				}
			}
			if n := impersonated.Load(); n != 1 {
				t.Errorf("the service_account_impersonation_url saw %d requests; want the one that mints the account's access token", n)
			}
		})
	}
}

// equals is a subject token check that wants want.
func equals(want string) func(t *testing.T, got string) {
	return func(t *testing.T, got string) {
		t.Helper()
		if got != want {
			t.Fatalf("the security token service was sent the subject token %q; want %q", got, want)
		}
	}
}

// iamCredentialsAt is a client that sends a request for Google's IAM Credentials
// (iamcredentials.googleapis.com) to the stand-in at base, and any other as it stands. The
// credential names IAM Credentials' own impersonation URL because Google's library tells an
// executable the account it acts as only where the URL is that one.
func iamCredentialsAt(t *testing.T, base string) *http.Client {
	t.Helper()
	standIn, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "iamcredentials.googleapis.com" {
			r = r.Clone(r.Context())
			r.URL.Scheme, r.URL.Host, r.Host = standIn.Scheme, standIn.Host, standIn.Host
		}
		return http.DefaultTransport.RoundTrip(r)
	})}
}

// TestAnExchangeThatAnswersNoAccessTokenMintsNothing: an exchange whose answer holds an empty
// access token is refused before generateIdToken, as a workload identity pool's is, rather than
// spending no credential there and blaming IAM Credentials.
func TestAnExchangeThatAnswersNoAccessTokenMintsNothing(t *testing.T) {
	sts := newRawServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"` + accessField + `":"","issued_token_type":"urn:ietf:params:oauth:token-type:access_token","token_type":"Bearer","expires_in":3600}`))
	})
	var minted atomic.Int64
	iam := newRawServer(t, func(w http.ResponseWriter, _ *http.Request) {
		minted.Add(1)
		http.Error(w, "no credential", http.StatusUnauthorized)
	})
	identity := &ExternalAccountIdentity{CredentialsJSON: externalAccountJSON(t, sts, map[string]any{"file": subjectTokenFile(t, "file-subject-token")}),
		ServiceAccount: "robot@example.iam.gserviceaccount.com", Audience: testAudience, HTTP: HTTPClient(), IAMCredentialsURL: iam}
	_, _, err := identity.Token(context.Background())
	if err == nil || !strings.Contains(err.Error(), "the exchange answered no access token") {
		t.Fatalf("Token: %v; want the empty access_token named", err)
	}
	if n := minted.Load(); n != 0 {
		t.Fatalf("generateIdToken saw %d requests; want none", n)
	}
}
