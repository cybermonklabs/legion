// packages/envoy/internal/cipherclient/googleprinted_test.go
//
// What a mint through Google's token services prints: the call, the service's status and its
// registered error code, and nothing of its answer's body, which a server can fill with anything,
// an echo of the subject token it was sent included.

package cipherclient

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAGoogleRefusalPrintsItsStatusAndCodeAlone: the security token service's OAuth refusal and
// IAM Credentials' API refusal each print their status and registered code, never their
// description or message, even where those repeat the subject token, the federated token or the
// configured URL's credential. A configured service URL prints only as internal/redact's
// allow-list does, whole for a plain one (the control: the check sees a URL where one prints).
func TestAGoogleRefusalPrintsItsStatusAndCodeAlone(t *testing.T) {
	const external = "external-hunter2pwSUBJECT"
	const federated = "federated-hunter2pwACCESS"
	sts := newRawServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/refuse/v1/token" {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"error":"invalid_grant","error_description":"echo of ` + external + `"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"access_token":"` + federated + `"}`))
	})
	iam := newRawServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"error":{"code":403,"message":"echo of ` + federated + `","status":"PERMISSION_DENIED"}}`))
	})
	file := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(file, []byte(external+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	pool := func(stsURL string) *PoolIdentity {
		return &PoolIdentity{
			Subject: NewProjectedToken(file), Audience: "//iam.googleapis.com/example",
			ServiceAccount: "example-fleet@example-project.iam.gserviceaccount.com", TokenAudience: "example-client",
			SecurityTokenServiceURL: stsURL, IAMCredentialsURL: iam,
		}
	}
	holds := func(t *testing.T, what, text string, want ...string) {
		t.Helper()
		for _, secret := range []string{external, federated, "hunter2pw", "tokuser9f3"} {
			if strings.Contains(text, secret) {
				t.Errorf("%s = %q, which holds %q", what, text, secret)
			}
		}
		for _, w := range want {
			if !strings.Contains(text, w) {
				t.Errorf("%s = %q, want it to name %q", what, text, w)
			}
		}
	}

	_, _, err := pool(sts + "/refuse/v1/token").Token(context.Background())
	if err == nil {
		t.Fatal("the pool minted through a refusing security token service")
	}
	holds(t, "the security token service's refusal", err.Error(), "security token service", sts+"/refuse/v1/token", "HTTP 400", "invalid_grant")

	_, _, err = pool(sts + "/v1/token").Token(context.Background())
	if err == nil {
		t.Fatal("the pool minted through a refusing IAM Credentials")
	}
	holds(t, "IAM Credentials' refusal", err.Error(), "generateIdToken", iam, "HTTP 403", "PERMISSION_DENIED")

	userinfo := "tokuser9f3" + ":" + "hunter2pw"
	withCredential := strings.Replace(sts, "http://", "http://"+userinfo+"@", 1) + "/refuse/v1/token"
	_, _, err = pool(withCredential).Token(context.Background())
	if err == nil {
		t.Fatal("the pool minted through a refusing security token service")
	}
	holds(t, "the refusal beside a URL that carries a credential", err.Error(), "security token service", "HTTP 400")
}

// TestACredentialsFilePrintsItsSettingAndNeverItsPathOrContent: the file
// GOOGLE_APPLICATION_CREDENTIALS names holds a credential, so no refusal names its path or
// quotes it: not a file that is not there, not a type of its own, not the security token
// service's refusal of the exchange it describes, whose body x/oauth2 quotes. Each names the
// variable.
func TestACredentialsFilePrintsItsSettingAndNeverItsPathOrContent(t *testing.T) {
	const secret = "hunter2pwCREDFILE"
	check := func(t *testing.T, what, text string) {
		t.Helper()
		if strings.Contains(text, secret) {
			t.Errorf("%s = %q, which holds the credential", what, text)
		}
		if !strings.Contains(text, credentialsFileVar) {
			t.Errorf("%s = %q, want it to name %s", what, text, credentialsFileVar)
		}
	}
	t.Setenv(credentialsFileVar, filepath.Join(t.TempDir(), secret))
	if _, _, err := externalAccountFor("agent-secrets-vm"); err == nil {
		t.Fatal("externalAccountFor read a file that is not there")
	} else {
		check(t, "the missing file's error", err.Error())
	}

	file := filepath.Join(t.TempDir(), "credentials.json")
	if err := os.WriteFile(file, []byte(`{"type":"`+secret+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(credentialsFileVar, file)
	if _, reason, err := externalAccountFor("agent-secrets-vm"); err != nil || reason == "" {
		t.Fatalf("externalAccountFor = %q, %v; want a reason it skipped the file", reason, err)
	} else {
		check(t, "the skip reason", reason)
	}

	sts := newRawServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"invalid_grant","error_description":"echo of ` + secret + `"}`))
	})
	subject := filepath.Join(t.TempDir(), "subject")
	if err := os.WriteFile(subject, []byte("a-subject-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	identity := &ExternalAccountIdentity{
		CredentialsJSON: []byte(`{"type":"external_account","audience":"//iam.googleapis.com/example","subject_token_type":"urn:ietf:params:oauth:token-type:jwt","token_url":"` + sts + `/v1/token","credential_source":{"file":"` + subject + `"},"service_account_impersonation_url":"https://iamcredentials.googleapis.com/v1/projects/-/serviceAccounts/robot@example.iam.gserviceaccount.com:generateAccessToken"}`),
		ServiceAccount:  "robot@example.iam.gserviceaccount.com", Audience: "agent-secrets-vm", HTTP: HTTPClient(),
	}
	if _, _, err := identity.Token(context.Background()); err == nil {
		t.Fatal("the identity minted through a refusing security token service")
	} else {
		check(t, "the refused exchange", err.Error())
		if !strings.Contains(err.Error(), "HTTP 400") {
			t.Errorf("the refused exchange = %q, want its status", err)
		}
	}
}
