// packages/envoy/internal/brokerclient/externalaccount_printed_test.go
//
// What an external_account identity prints of its audience and its account, in every trade's log
// line and error that names it: the audience (the broker's Google exchange audience, text a
// broker's settings answer wrote) only where it is a plain http(s) URL internal/redact prints, and
// the account only where it is a service account's email or unique id, the account the
// credentials file GOOGLE_APPLICATION_CREDENTIALS names otherwise.
package brokerclient

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAnExternalAccountIdentityPrintsItsAudienceAndAccountOnlyWhereTheyAreWhatTheyName: an
// external_account identity whose audience carries a credential, and whose account is no service
// account's, names neither in Describe, nor in the error of a mint the security token service or
// IAM Credentials refuses. Control: a plain audience and a service account's email print whole.
func TestAnExternalAccountIdentityPrintsItsAudienceAndAccountOnlyWhereTheyAreWhatTheyName(t *testing.T) {
	const secret = "hunter2pw-externalaccount-3f81a0"
	refusing := func(t *testing.T) string {
		return newRawServer(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":"access_denied"}`))
		})
	}
	trading := func(t *testing.T) string {
		return newRawServer(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"` + accessField + `":"federated","issued_token_type":"urn:ietf:params:oauth:token-type:access_token","token_type":"Bearer","expires_in":3600}`))
		})
	}
	identity := func(t *testing.T, sts, audience, account string) *ExternalAccountIdentity {
		t.Helper()
		dir := t.TempDir()
		subject := filepath.Join(dir, "subject")
		if err := os.WriteFile(subject, []byte("the.workload.credential"), 0o600); err != nil {
			t.Fatal(err)
		}
		data, err := json.Marshal(map[string]any{
			"type":               "external_account",
			"audience":           "//iam.googleapis.com/projects/1/locations/global/workloadIdentityPools/p/providers/v",
			"subject_token_type": "urn:ietf:params:oauth:token-type:jwt",
			"token_url":          sts + "/v1/token",
			"credential_source":  map[string]any{"file": subject},
		})
		if err != nil {
			t.Fatal(err)
		}
		return &ExternalAccountIdentity{CredentialsJSON: data, ServiceAccount: account, Audience: audience, HTTP: HTTPClient(), IAMCredentialsURL: refusing(t)}
	}
	for _, row := range []struct {
		name, audience, account string
		want                    []string
	}{
		{"an audience with a user and a password", "https://tokuser9f3:" + secret + "@aud.example", "fleet-robot@example-project.iam.gserviceaccount.com",
			[]string{"the Google exchange audience the broker names", "fleet-robot@example-project.iam.gserviceaccount.com"}},
		{"an account that is no service account's", "https://aud.example", secret,
			[]string{"https://aud.example", "the service account " + credentialsFileVar + " names (not printed"}},
		{"control: a plain audience and a service account's email", "https://aud.example", "fleet-robot@example-project.iam.gserviceaccount.com",
			[]string{"https://aud.example", "fleet-robot@example-project.iam.gserviceaccount.com"}},
	} {
		t.Run(row.name, func(t *testing.T) {
			refused := identity(t, refusing(t), row.audience, row.account)
			_, _, stsErr := refused.Token(context.Background())
			minted := identity(t, trading(t), row.audience, row.account)
			_, _, iamErr := minted.Token(context.Background())
			if stsErr == nil || iamErr == nil {
				t.Fatalf("Token = %v, %v; want the refusals of the security token service and of IAM Credentials", stsErr, iamErr)
			}
			for what, text := range map[string]string{"Describe": refused.Describe(), "the security token service's refusal": stsErr.Error(), "IAM Credentials' refusal": iamErr.Error()} {
				for _, leaked := range []string{secret, "tokuser9f3"} {
					if strings.Contains(text, leaked) {
						t.Errorf("%s %q holds %q", what, text, leaked)
					}
				}
			}
			for _, want := range row.want {
				if !strings.Contains(refused.Describe(), want) {
					t.Errorf("Describe %q does not say %q", refused.Describe(), want)
				}
			}
			if account := row.want[len(row.want)-1]; !strings.Contains(stsErr.Error()+iamErr.Error(), account) {
				t.Errorf("the refusals %q and %q do not name the account as %q", stsErr, iamErr, account)
			}
		})
	}
}

// TestAnExternalAccountMintsFailureSaysWhatFailed: an external_account credential whose exchange
// fails says which step failed in words of this client's own: a subject token file its
// credential_source names that cannot be read prints the file call's reason, never its path; a
// refusal of the token service names that service and the registered code its answer carried,
// never the rest of the body. Control: the registered code prints, so the check sees the answer.
func TestAnExternalAccountMintsFailureSaysWhatFailed(t *testing.T) {
	const secret = "hunter2pw-externalmint-62c0"
	sts := newRawServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"echo: ` + secret + `"}`))
	})
	credential := func(t *testing.T, subject string) []byte {
		t.Helper()
		data, err := json.Marshal(map[string]any{
			"type":               "external_account",
			"audience":           "//iam.googleapis.com/projects/1/locations/global/workloadIdentityPools/p/providers/v",
			"subject_token_type": "urn:ietf:params:oauth:token-type:jwt",
			"token_url":          sts + "/v1/token",
			"credential_source":  map[string]any{"file": subject},
		})
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	for _, row := range []struct {
		name    string
		subject func(t *testing.T) string
		want    string
	}{
		{"a subject token file that is not there", func(t *testing.T) string { return filepath.Join(t.TempDir(), secret) },
			"read the subject token file its credential_source names: open: no such file or directory"},
		{"control: the token service refuses the exchange", func(t *testing.T) string {
			subject := filepath.Join(t.TempDir(), "subject")
			if err := os.WriteFile(subject, []byte("the.workload.credential"), 0o600); err != nil {
				t.Fatal(err)
			}
			return subject
		}, "the token service the credential names: the server answered HTTP 400 invalid_grant"},
	} {
		t.Run(row.name, func(t *testing.T) {
			identity := &ExternalAccountIdentity{CredentialsJSON: credential(t, row.subject(t)), ServiceAccount: "deploy-bot@example-project.iam.gserviceaccount.com",
				Audience: "https://aud.example", HTTP: HTTPClient()}
			_, _, err := identity.Token(context.Background())
			if err == nil {
				t.Fatal("the exchange minted a token")
			}
			if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "error_description") {
				t.Errorf("the error %q holds the path or the answer's body", err)
			}
			if !strings.Contains(err.Error(), row.want) {
				t.Errorf("the error %q does not say %q", err, row.want)
			}
		})
	}
}
