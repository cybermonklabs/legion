// packages/envoy/cmd/agent-secrets/secret_aws_printed_test.go
//
// What the AWS store forms print about the web identity token file AWS_WEB_IDENTITY_TOKEN_FILE
// names: the file holds a token, and the variable is where a person who pasted the token itself
// puts it, so a file the forms cannot read is refused naming the variable and the system's reason,
// never the path the AWS SDK's own error quotes.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/credentials/ssocreds"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/aws/aws-sdk-go-v2/service/sso"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/smithy-go"

	"github.com/sjawhar/envoy/internal/broker/policy"
	"github.com/sjawhar/envoy/internal/broker/policy/policytest"
	"github.com/sjawhar/envoy/internal/broker/secrets"
)

// refusingWith is the address of an STS that refuses every call with code and message, both of
// which whoever answers there chooses.
func refusingWith(t *testing.T, code, message string) string {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/xml")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`<ErrorResponse><Error><Type>Sender</Type><Code>` + code + `</Code><Message>` + message + `</Message></Error><RequestId>r2</RequestId></ErrorResponse>`))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// useAWSWebIdentity leaves the rest of t with the real awsClients, signing in with a web identity:
// AWS_ROLE_ARN set and AWS_WEB_IDENTITY_TOKEN_FILE naming tokenFile, with no other source the SDK
// could take (no keys, no profile, empty shared config and credentials files, no instance
// metadata), and every STS call sent to a local stand-in that refuses AssumeRoleWithWebIdentity,
// so nothing reaches AWS. It answers the tokens the stand-in was sent.
func useAWSWebIdentity(t *testing.T, tokenFile string) func() []string {
	t.Helper()
	for _, name := range []string{
		"AWS_ACCESS_KEY_ID", "AWS_ACCESS_KEY", "AWS_SECRET_ACCESS_KEY", "AWS_SECRET_KEY", "AWS_SESSION_TOKEN",
		"AWS_PROFILE", "AWS_DEFAULT_PROFILE", "AWS_ENDPOINT_URL",
	} {
		t.Setenv(name, "")
	}
	empty := filepath.Join(t.TempDir(), "empty")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AWS_CONFIG_FILE", empty)
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", empty)
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	t.Setenv("AWS_MAX_ATTEMPTS", "1")
	t.Setenv("AWS_ROLE_ARN", "arn:aws:iam::111122223333:role/agent")
	t.Setenv("AWS_WEB_IDENTITY_TOKEN_FILE", tokenFile)
	var mu sync.Mutex
	var sent []string
	stsStandIn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		mu.Lock()
		sent = append(sent, r.PostForm.Get("WebIdentityToken"))
		mu.Unlock()
		w.Header().Set("Content-Type", "text/xml")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`<ErrorResponse><Error><Type>Sender</Type><Code>InvalidIdentityToken</Code><Message>the stand-in refuses every token</Message></Error><RequestId>stand-in</RequestId></ErrorResponse>`))
	}))
	t.Cleanup(stsStandIn.Close)
	t.Setenv("AWS_ENDPOINT_URL_STS", stsStandIn.URL)
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), sent...)
	}
}

// TestAWSWebIdentityTokenFileIsNamedByItsVariableNeverItsPath: a web identity token file the forms
// cannot read - none at the path, a directory there - is refused, before any STS call, naming
// AWS_WEB_IDENTITY_TOKEN_FILE and the system's reason, never the path, which the AWS SDK's own
// error quotes ("unable to read file at <path>: open <path>: ..."). A file that holds a token is
// still read and its token sent to STS (the control: the stand-in is the endpoint the form reached).
func TestAWSWebIdentityTokenFileIsNamedByItsVariableNeverItsPath(t *testing.T) {
	const secret = "hunter2pw-webidentity-6d1f08"
	for _, row := range []struct {
		name  string
		place func(t *testing.T) string
		want  string
	}{
		{"no file at the path", func(t *testing.T) string {
			return filepath.Join(t.TempDir(), secret, "token")
		}, "read the web identity token file AWS_WEB_IDENTITY_TOKEN_FILE names: open: no such file or directory"},
		{"the variable holds the token itself", func(*testing.T) string {
			return secret
		}, "read the web identity token file AWS_WEB_IDENTITY_TOKEN_FILE names: open: no such file or directory"},
		{"a directory at the path", func(t *testing.T) string {
			dir := filepath.Join(t.TempDir(), secret)
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			return dir
		}, "read the web identity token file AWS_WEB_IDENTITY_TOKEN_FILE names: read: is a directory"},
	} {
		t.Run(row.name, func(t *testing.T) {
			local := secrets.NewLocal(policytest.Secret("HELD_KEY", "ada@example.com", policy.TierAgent, "v1"))
			startSecretBroker(t, servedBy(local))
			sent := useAWSWebIdentity(t, row.place(t))

			_, stderr, code := runSecret("list")
			if code != 1 {
				t.Fatalf("exit %d, stderr %q; want 1", code, stderr)
			}
			wantNamedNotPrinted(t, "stderr", stderr, []string{row.want}, []string{secret, "unable to read file at"}...)
			if got := sent(); len(got) != 0 {
				t.Errorf("STS was sent %d tokens for a file that cannot be read, want none", len(got))
			}
		})
	}
	t.Run("control: a file that holds a token", func(t *testing.T) {
		local := secrets.NewLocal(policytest.Secret("HELD_KEY", "ada@example.com", policy.TierAgent, "v1"))
		startSecretBroker(t, servedBy(local))
		file := filepath.Join(t.TempDir(), "token")
		if err := os.WriteFile(file, []byte("not-a-secret-web-identity-token"), 0o600); err != nil {
			t.Fatal(err)
		}
		sent := useAWSWebIdentity(t, file)

		_, stderr, code := runSecret("list")
		if code != 1 || !strings.Contains(stderr, "InvalidIdentityToken") {
			t.Fatalf("exit %d, stderr %q; want 1 with the stand-in's InvalidIdentityToken", code, stderr)
		}
		if got := sent(); len(got) == 0 || got[0] != "not-a-secret-web-identity-token" {
			t.Fatalf("STS was sent %q, want the file's token", got)
		}
	})
	// A token file a long form read at its start and that is gone when the SDK reads it again, as
	// it does at each refresh of the credentials: that read is the SDK's, through the retriever
	// awsClients gives it, and it too names the variable alone.
	t.Run("a later read of the file, by the SDK", func(t *testing.T) {
		file := filepath.Join(t.TempDir(), secret, "token")
		if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte("not-a-secret-web-identity-token"), 0o600); err != nil {
			t.Fatal(err)
		}
		useAWSWebIdentity(t, file)
		_, st, _, err := awsClients(context.Background(), "us-east-1")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(file); err != nil {
			t.Fatal(err)
		}
		_, err = st.GetCallerIdentity(context.Background(), &sts.GetCallerIdentityInput{})
		if err == nil {
			t.Fatal("the SDK signed in with a token file that is gone")
		}
		wantNamedNotPrinted(t, "the error", err.Error(), []string{"read the web identity token file AWS_WEB_IDENTITY_TOKEN_FILE names: open: no such file or directory"}, []string{secret, "unable to read file at"}...)
	})
}

// TestAnAWSProfileTheConfigDoesNotHoldIsNotPrinted: AWS_PROFILE or AWS_DEFAULT_PROFILE naming a
// profile the shared config and credentials files do not hold is refused naming the variable,
// never the profile, which the AWS SDK's own error quotes: the variable is where a person who
// pasted a credential in the wrong place puts it. A profile the files hold still loads (the
// control).
func TestAnAWSProfileTheConfigDoesNotHoldIsNotPrinted(t *testing.T) {
	const secret = "hunter2pw-awsprofile-3d90c4"
	useProfiles := func(t *testing.T) {
		t.Helper()
		for _, name := range []string{
			"AWS_ACCESS_KEY_ID", "AWS_ACCESS_KEY", "AWS_SECRET_ACCESS_KEY", "AWS_SECRET_KEY", "AWS_SESSION_TOKEN",
			"AWS_PROFILE", "AWS_DEFAULT_PROFILE", "AWS_WEB_IDENTITY_TOKEN_FILE", "AWS_ROLE_ARN",
		} {
			t.Setenv(name, "")
		}
		dir := t.TempDir()
		configFile := filepath.Join(dir, "config")
		if err := os.WriteFile(configFile, []byte("[profile work]\nregion = us-east-1\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		credentialsFile := filepath.Join(dir, "credentials")
		if err := os.WriteFile(credentialsFile, []byte("[work]\naws_access_key_id = AKIAEXAMPLE\naws_secret_access_key = not-a-secret\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("HOME", t.TempDir())
		t.Setenv("AWS_CONFIG_FILE", configFile)
		t.Setenv("AWS_SHARED_CREDENTIALS_FILE", credentialsFile)
		t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	}
	for _, variable := range []string{"AWS_PROFILE", "AWS_DEFAULT_PROFILE"} {
		t.Run(variable, func(t *testing.T) {
			local := secrets.NewLocal(policytest.Secret("HELD_KEY", "ada@example.com", policy.TierAgent, "v1"))
			startSecretBroker(t, servedBy(local))
			useProfiles(t)
			t.Setenv(variable, secret)

			_, stderr, code := runSecret("list")
			if code != 1 {
				t.Fatalf("exit %d, stderr %q; want 1", code, stderr)
			}
			// The refusal lists the profiles the files hold, as the GCP refusals list theirs.
			wantNamedNotPrinted(t, "stderr", stderr, []string{"load your AWS sign-in: " + variable + " names a profile the shared config and credentials files do not hold (not printed); they hold: work"}, []string{secret}...)
		})
		t.Run("control: "+variable+" naming a profile the files hold", func(t *testing.T) {
			useProfiles(t)
			t.Setenv(variable, "work")
			if _, _, _, err := awsClients(context.Background(), "us-east-1"); err != nil {
				t.Fatalf("awsClients refused a profile the files hold: %v", err)
			}
		})
	}
}

// awsSignInOnly leaves t with no AWS source but static keys and no address setting, and one
// attempt per call: each row of TestTheAWSSDKsSignInFailurePrintsNoSetting sets what it tests.
func awsSignInOnly(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"AWS_ACCESS_KEY", "AWS_SECRET_KEY", "AWS_SESSION_TOKEN", "AWS_PROFILE", "AWS_DEFAULT_PROFILE",
		"AWS_WEB_IDENTITY_TOKEN_FILE", "AWS_ROLE_ARN", "AWS_ENDPOINT_URL", "AWS_ENDPOINT_URL_STS",
		"AWS_ENDPOINT_URL_SECRETS_MANAGER", "AWS_CONTAINER_CREDENTIALS_FULL_URI",
		"AWS_CONTAINER_CREDENTIALS_RELATIVE_URI", "AWS_CONTAINER_AUTHORIZATION_TOKEN",
		"AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE",
	} {
		t.Setenv(name, "")
	}
	empty := filepath.Join(t.TempDir(), "empty")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AWS_CONFIG_FILE", empty)
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", empty)
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	t.Setenv("AWS_MAX_ATTEMPTS", "1")
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIA"+"EXAMPLEKEY")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "not-a-"+"secret")
}

// TestTheAWSSDKsSignInFailurePrintsNoSetting: the AWS SDK's failure to load or to read the
// sign-in quotes what it was configured with - the path of a file an AWS setting names, an
// address setting's URL with its user, password and query, a setting's value it cannot read, and
// STS's own message - and the forms print none of it. Each row puts a made-up secret where a
// person or a server could put one, and the form names the setting, the file call's reason, the
// request's class or AWS's error code. The STS row is the control: AWS's code, which the server
// wrote beside the echoed secret, still prints, so the check sees what AWS answered.
func TestTheAWSSDKsSignInFailurePrintsNoSetting(t *testing.T) {
	secret := "hunter2pw-" + "awssignin-" + "5c2e"
	user := "tokuser" + "7d2a"
	closing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
			_ = conn.Close()
		}
	}))
	t.Cleanup(closing.Close)
	refusing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/xml")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`<ErrorResponse><Error><Type>Sender</Type><Code>AccessDenied</Code><Message>echo: ` + secret + `</Message></Error><RequestId>r1</RequestId></ErrorResponse>`))
	}))
	t.Cleanup(refusing.Close)
	// refusingAs is an STS that refuses with code, which whoever answers there chooses.
	refusingAs := func(t *testing.T, code string) string {
		return refusingWith(t, code, "no")
	}
	// roleProfile signs in with the profile AWS_PROFILE=role, whose role_arn the SDK assumes with
	// STS's AssumeRole under the static keys of its source_profile.
	roleProfile := func(t *testing.T) {
		dir := t.TempDir()
		config, credentials := filepath.Join(dir, "config"), filepath.Join(dir, "credentials")
		if err := os.WriteFile(config, []byte("[profile role]\nrole_arn = arn:aws:iam::111122223333:role/agent\nsource_profile = base\nregion = us-east-1\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(credentials, []byte("[base]\naws_access_key_id = AKIA"+"EXAMPLEKEY\naws_secret_access_key = not-a-"+"secret\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("AWS_ACCESS_KEY_ID", "")
		t.Setenv("AWS_SECRET_ACCESS_KEY", "")
		t.Setenv("AWS_CONFIG_FILE", config)
		t.Setenv("AWS_SHARED_CREDENTIALS_FILE", credentials)
		t.Setenv("AWS_PROFILE", "role")
	}
	// expiredSSOSession is an sso_session profile, AWS_PROFILE=sso, whose cached token has expired
	// and whose refresh IAM Identity Center refuses with InvalidGrantException at
	// AWS_ENDPOINT_URL_SSO_OIDC, writing a made-up secret in its description.
	expiredSSOSession := func(t *testing.T) {
		oidc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-Amzn-Errortype", "InvalidGrantException")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"echo: ` + secret + `"}`))
		}))
		t.Cleanup(oidc.Close)
		configFile := filepath.Join(t.TempDir(), "config")
		profile := "[profile sso]\nsso_session = corp\nsso_account_id = 111122223333\nsso_role_name = Agent\nregion = us-east-1\n" +
			"[sso-session corp]\nsso_start_url = https://example.awsapps.com/start\nsso_region = us-east-1\n"
		if err := os.WriteFile(configFile, []byte(profile), 0o600); err != nil {
			t.Fatal(err)
		}
		cached, err := ssocreds.StandardCachedTokenFilepath("corp")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(cached), 0o700); err != nil {
			t.Fatal(err)
		}
		// The cached token's fields, the credential-looking names built from parts, so no line of
		// this file reads as a token beside its value.
		token, err := json.Marshal(map[string]string{
			"access" + "Token": "expired-access", "expiresAt": "2020-01-01T00:00:00Z",
			"refresh" + "Token": "made-up-refresh-1", "clientId": "made-up-client-1", "client" + "Secret": "made-up-registration-1",
			"registrationExpiresAt": "2099-01-01T00:00:00Z", "region": "us-east-1", "startUrl": "https://example.awsapps.com/start",
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(cached, token, 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("AWS_ACCESS_KEY_ID", "")
		t.Setenv("AWS_SECRET_ACCESS_KEY", "")
		t.Setenv("AWS_CONFIG_FILE", configFile)
		t.Setenv("AWS_PROFILE", "sso")
		t.Setenv("AWS_ENDPOINT_URL_SSO_OIDC", oidc.URL)
	}
	directory := func(t *testing.T) string {
		dir := filepath.Join(t.TempDir(), secret)
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	// unparseable is an address with a user and a password that url.Parse refuses (its path holds
	// the escape %zz), built with url.UserPassword so no line of this file spells a credentialed
	// URL.
	unparseable := func(host string) string {
		return (&url.URL{Scheme: "http", User: url.UserPassword(user, secret), Host: host}).String() + "/%zz"
	}
	for _, row := range []struct {
		name string
		set  func(t *testing.T)
		want string
	}{
		{"AWS_SHARED_CREDENTIALS_FILE names a directory", func(t *testing.T) { t.Setenv("AWS_SHARED_CREDENTIALS_FILE", directory(t)) },
			"load your AWS sign-in: read the file AWS_SHARED_CREDENTIALS_FILE names: read: is a directory"},
		{"AWS_CONFIG_FILE names a directory", func(t *testing.T) { t.Setenv("AWS_CONFIG_FILE", directory(t)) },
			"load your AWS sign-in: read the file AWS_CONFIG_FILE names: read: is a directory"},
		{"AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE names no file", func(t *testing.T) {
			t.Setenv("AWS_ACCESS_KEY_ID", "")
			t.Setenv("AWS_SECRET_ACCESS_KEY", "")
			t.Setenv("AWS_CONTAINER_CREDENTIALS_FULL_URI", "http://127.0.0.1:1/creds")
			t.Setenv("AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE", filepath.Join(t.TempDir(), secret))
		}, "read the file AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE names: open: no such file or directory"},
		{"AWS_CONTAINER_CREDENTIALS_FULL_URI with a token in its query", func(t *testing.T) {
			t.Setenv("AWS_ACCESS_KEY_ID", "")
			t.Setenv("AWS_SECRET_ACCESS_KEY", "")
			t.Setenv("AWS_CONTAINER_CREDENTIALS_FULL_URI", closing.URL+"/creds?token="+secret)
		}, "read your AWS sign-in: a request the AWS SDK made failed: the request failed (the address is not printed; AWS_CONTAINER_CREDENTIALS_FULL_URI is set)"},
		{"AWS_CONTAINER_CREDENTIALS_FULL_URI that does not parse", func(t *testing.T) {
			t.Setenv("AWS_ACCESS_KEY_ID", "")
			t.Setenv("AWS_SECRET_ACCESS_KEY", "")
			t.Setenv("AWS_CONTAINER_CREDENTIALS_FULL_URI", unparseable("127.0.0.1"))
		}, "your AWS sign-in: "},
		{"AWS_ENDPOINT_URL_STS that does not parse, with a credential in it", func(t *testing.T) {
			t.Setenv("AWS_ENDPOINT_URL_STS", unparseable("127.0.0.1:1"))
		}, "read your AWS sign-in: the AWS SDK could not sign in from this environment (its message is not printed: it can quote an AWS setting's value); check AWS_ENDPOINT_URL_STS and ~/.aws/config"},
		{"AWS_ENDPOINT_URL with a secret in its path", func(t *testing.T) { t.Setenv("AWS_ENDPOINT_URL", closing.URL+"/"+secret) },
			"read your AWS sign-in: a request the AWS SDK made failed: the request failed (the address is not printed; AWS_ENDPOINT_URL is set)"},
		{"AWS_MAX_ATTEMPTS that is no number but a credential", func(t *testing.T) { t.Setenv("AWS_MAX_ATTEMPTS", secret) },
			"load your AWS sign-in: the AWS SDK could not sign in from this environment (its message is not printed: it can quote an AWS setting's value); check AWS_MAX_ATTEMPTS and ~/.aws/config"},
		{"STS refuses with its own message: its code prints, never the message", func(t *testing.T) { t.Setenv("AWS_ENDPOINT_URL_STS", refusing.URL) },
			"read your AWS sign-in: AWS refused it: AccessDenied (AWS's message is not printed)"},
		{"STS refuses with a code that is a made-up credential", func(t *testing.T) { t.Setenv("AWS_ENDPOINT_URL_STS", refusingAs(t, user+"CodeEcho")) },
			"read your AWS sign-in: AWS refused it: an AWS error (its code is none this client names, and AWS's message is not printed)"},
		{"STS refuses a malformed role or session with ValidationError", func(t *testing.T) { t.Setenv("AWS_ENDPOINT_URL_STS", refusingAs(t, "ValidationError")) },
			"read your AWS sign-in: AWS refused it: ValidationError (AWS's message is not printed)"},
		{"an sso_session sign-in whose refresh IAM Identity Center refuses", expiredSSOSession,
			"read your AWS sign-in: the IAM Identity Center (SSO) session has expired or is invalid (IAM Identity Center answered InvalidGrantException): run `aws sso login` again"},
		{"a web identity token file with no role to assume", func(t *testing.T) {
			tokenFile := filepath.Join(t.TempDir(), "token")
			if err := os.WriteFile(tokenFile, []byte("the.web.identity"), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("AWS_ACCESS_KEY_ID", "")
			t.Setenv("AWS_SECRET_ACCESS_KEY", "")
			t.Setenv("AWS_WEB_IDENTITY_TOKEN_FILE", tokenFile)
		}, "load your AWS sign-in: the web identity sign-in names no role to assume: set AWS_ROLE_ARN, or role_arn in the profile, beside its token file"},
		{"STS refuses a role_arn profile's AssumeRole, naming the permission", func(t *testing.T) {
			roleProfile(t)
			t.Setenv("AWS_ENDPOINT_URL_STS", refusingWith(t, "AccessDenied",
				"User: arn:aws:iam::111122223333:user/"+user+" is not authorized to perform: sts:AssumeRole on resource: arn:aws:iam::111122223333:role/agent; echo: "+secret))
		}, "read your AWS sign-in: AWS refused it: AccessDenied (missing permission: sts:AssumeRole; AWS's message is not printed)"},
		{"STS refuses a web identity's AssumeRoleWithWebIdentity, naming the permission in STS's words", func(t *testing.T) {
			tokenFile := filepath.Join(t.TempDir(), "token")
			if err := os.WriteFile(tokenFile, []byte("the.web.identity"), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("AWS_ACCESS_KEY_ID", "")
			t.Setenv("AWS_SECRET_ACCESS_KEY", "")
			t.Setenv("AWS_WEB_IDENTITY_TOKEN_FILE", tokenFile)
			t.Setenv("AWS_ROLE_ARN", "arn:aws:iam::111122223333:role/agent")
			t.Setenv("AWS_ENDPOINT_URL_STS", refusingWith(t, "AccessDenied", "Not authorized to perform sts:AssumeRoleWithWebIdentity; echo: "+secret))
		}, "read your AWS sign-in: AWS refused it: AccessDenied (missing permission: sts:AssumeRoleWithWebIdentity; AWS's message is not printed)"},
	} {
		t.Run(row.name, func(t *testing.T) {
			local := secrets.NewLocal(policytest.Secret("HELD_KEY", "ada@example.com", policy.TierAgent, "v1"))
			startSecretBroker(t, servedBy(local))
			awsSignInOnly(t)
			row.set(t)
			_, stderr, code := runSecret("list")
			if code != 1 {
				t.Fatalf("exit %d, stderr %q; want 1", code, stderr)
			}
			wantNamedNotPrinted(t, "stderr", stderr, []string{row.want}, secret, user)
		})
	}
}

// failingList is a Secrets Manager whose ListSecrets fails with err, as the SDK's call returns it.
type failingList struct {
	*secrets.Local
	err error
}

func (f failingList) ListSecrets(context.Context, *secretsmanager.ListSecretsInput, ...func(*secretsmanager.Options)) (*secretsmanager.ListSecretsOutput, error) {
	return nil, f.err
}

// useSSOProfile signs in with the IAM Identity Center profile AWS_PROFILE=sso: a legacy one
// (sso_start_url in the profile) with no cached token, or an sso_session one whose cached token
// has the fields token gives, its SSO calls sent to ssoURL where it is set.
func useSSOProfile(t *testing.T, legacy bool, token map[string]string, ssoURL string) {
	t.Helper()
	profile := "[profile sso]\nsso_session = corp\nsso_account_id = 111122223333\nsso_role_name = Agent\nregion = us-east-1\n" +
		"[sso-session corp]\nsso_start_url = https://example.awsapps.com/start\nsso_region = us-east-1\n"
	if legacy {
		profile = "[profile sso]\nsso_start_url = https://example.awsapps.com/start\nsso_region = us-east-1\nsso_account_id = 111122223333\nsso_role_name = Agent\nregion = us-east-1\n"
	}
	configFile := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(configFile, []byte(profile), 0o600); err != nil {
		t.Fatal(err)
	}
	if token != nil {
		cached, err := ssocreds.StandardCachedTokenFilepath("corp")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(cached), 0o700); err != nil {
			t.Fatal(err)
		}
		body, err := json.Marshal(token)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(cached, body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("AWS_ACCESS_KEY_ID", "")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "")
	t.Setenv("AWS_CONFIG_FILE", configFile)
	t.Setenv("AWS_PROFILE", "sso")
	if ssoURL != "" {
		t.Setenv("AWS_ENDPOINT_URL_SSO", ssoURL)
	}
}

// TestAnIAMIdentityCenterFailureIsSaidOnlyWhereTheSDKOrIAMIdentityCenterSaysIt: an IAM Identity
// Center sign-in reads as an expired session only where the SDK's own error says so - a legacy
// profile with no cached token, an sso_session token it cannot refresh - or IAM Identity Center
// refused the session's token; a role or account IAM Identity Center does not find prints as its
// refusal by its code, in the sign-in and in a store call alike; and the SDK's own words inside
// STS's message, which whoever answers there writes, read as STS's refusal, neither as an expired
// session nor as a sign-in with no role. Each server writes a made-up secret in its message. The
// expired rows are the controls: the check sees an expired session where there is one.
func TestAnIAMIdentityCenterFailureIsSaidOnlyWhereTheSDKOrIAMIdentityCenterSaysIt(t *testing.T) {
	planted := "hunter2pw-" + "ssoecho-" + "2b7e"
	const expired = "the IAM Identity Center (SSO) session has expired or is invalid"
	cachedToken := func(expiresAt string) map[string]string {
		return map[string]string{"access" + "Token": "made-up-access-1", "expiresAt": expiresAt, "region": "us-east-1", "startUrl": "https://example.awsapps.com/start"}
	}
	ssoRefusing := func(t *testing.T, code string, status int) string {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-Amzn-Errortype", code)
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"message":"echo: ` + planted + `"}`))
		}))
		t.Cleanup(srv.Close)
		return srv.URL
	}
	stsRefusing := func(t *testing.T, message string) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/xml")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`<ErrorResponse><Error><Type>Sender</Type><Code>AccessDenied</Code><Message>` + message + `; echo: ` + planted + `</Message></Error><RequestId>r3</RequestId></ErrorResponse>`))
		}))
		t.Cleanup(srv.Close)
		t.Setenv("AWS_ENDPOINT_URL_STS", srv.URL)
	}
	for _, row := range []struct {
		name string
		set  func(t *testing.T)
		want string
	}{
		{"control: a legacy profile with no cached token", func(t *testing.T) { useSSOProfile(t, true, nil, "") },
			expired + ": run `aws sso login` again"},
		{"control: an sso_session token the SDK cannot refresh", func(t *testing.T) { useSSOProfile(t, false, cachedToken("2020-01-01T00:00:00Z"), "") },
			expired + ": run `aws sso login` again"},
		{"control: a session token IAM Identity Center refuses", func(t *testing.T) {
			useSSOProfile(t, false, cachedToken("2099-01-01T00:00:00Z"), ssoRefusing(t, "UnauthorizedException", http.StatusUnauthorized))
		}, expired + " (IAM Identity Center answered UnauthorizedException): run `aws sso login` again"},
		{"a role IAM Identity Center does not find", func(t *testing.T) {
			useSSOProfile(t, false, cachedToken("2099-01-01T00:00:00Z"), ssoRefusing(t, "ResourceNotFoundException", http.StatusNotFound))
		}, "read your AWS sign-in: IAM Identity Center refused it: ResourceNotFoundException (its message is not printed); check the profile's sso_account_id and sso_role_name"},
		{"STS's message holding the SDK's words for a token it cannot refresh", func(t *testing.T) {
			stsRefusing(t, "cached SSO token is expired, or not present, and cannot be refreshed")
		}, "read your AWS sign-in: AWS refused it: AccessDenied (AWS's message is not printed)"},
		{"STS's message holding the SDK's words for a sign-in with no role", func(t *testing.T) { stsRefusing(t, "role ARN is not set") },
			"read your AWS sign-in: AWS refused it: AccessDenied (AWS's message is not printed)"},
	} {
		t.Run(row.name, func(t *testing.T) {
			local := secrets.NewLocal(policytest.Secret("HELD_KEY", "ada@example.com", policy.TierAgent, "v1"))
			startSecretBroker(t, servedBy(local))
			awsSignInOnly(t)
			row.set(t)
			_, stderr, code := runSecret("list")
			if code != 1 {
				t.Fatalf("exit %d, stderr %q; want 1", code, stderr)
			}
			wantNamedNotPrinted(t, "stderr", stderr, []string{row.want}, planted)
		})
	}
	// A store call's credential refresh that IAM Identity Center refused, as the SDK chains it.
	refresh := func(code string) error {
		sso := &smithy.OperationError{ServiceID: sso.ServiceID, OperationName: "GetRoleCredentials", Err: &smithy.GenericAPIError{Code: code, Message: "echo: " + planted}}
		return &smithy.OperationError{ServiceID: secretsmanager.ServiceID, OperationName: "ListSecrets", Err: fmt.Errorf("failed to refresh cached credentials, %w", sso)}
	}
	for _, row := range []struct{ name, code, want string }{
		{"control: a store call whose session token IAM Identity Center refuses", "UnauthorizedException",
			"Secrets Manager ListSecrets failed: " + expired + " (IAM Identity Center answered UnauthorizedException): run `aws sso login` again"},
		{"a store call whose role IAM Identity Center does not find", "ResourceNotFoundException",
			"Secrets Manager ListSecrets failed: IAM Identity Center refused it: ResourceNotFoundException (its message is not printed); check the profile's sso_account_id and sso_role_name"},
	} {
		t.Run(row.name, func(t *testing.T) {
			local := secrets.NewLocal(policytest.Secret("HELD_KEY", "ada@example.com", policy.TierAgent, "v1"))
			startSecretBroker(t, servedBy(local))
			useAWS(t, failingList{Local: local, err: refresh(row.code)}, testAccount, adaSignIn)
			_, stderr, code := runSecret("list")
			if code != 1 {
				t.Fatalf("exit %d, stderr %q; want 1", code, stderr)
			}
			wantNamedNotPrinted(t, "stderr", stderr, []string{row.want}, planted)
		})
	}
}

// TestTheWebIdentityTokenFileIsNamedByItsVariable: the web identity token file holds a token, so
// the using line and a store's refusal name it by AWS_WEB_IDENTITY_TOKEN_FILE, never by its path,
// which here carries a made-up credential, and so does a later failed read of it (awsFileName),
// in a store call whose credential refresh reads it again as well as in the sign-in. Control: the
// command a person pastes to run again still names the file, which signed in.
func TestTheWebIdentityTokenFileIsNamedByItsVariable(t *testing.T) {
	planted := "hunter2pw-" + "webidpath-" + "31d0"
	file := filepath.Join(t.TempDir(), planted, "token")
	for _, name := range []string{"AWS_ACCESS_KEY_ID", "AWS_ACCESS_KEY", "AWS_SECRET_ACCESS_KEY", "AWS_SECRET_KEY", "AWS_PROFILE", "AWS_DEFAULT_PROFILE"} {
		t.Setenv(name, "")
	}
	t.Setenv("AWS_WEB_IDENTITY_TOKEN_FILE", file)
	t.Setenv("AWS_ROLE_ARN", "arn:aws:iam::111122223333:role/agent")
	source := awsSourceOf(nil)
	wantNamedNotPrinted(t, "the source's words", source.phrase+"; "+source.another, []string{"the web identity token file AWS_WEB_IDENTITY_TOKEN_FILE names"}, planted)
	wantNamedNotPrinted(t, "a later read's words", awsFileName(file), []string{"the file AWS_WEB_IDENTITY_TOKEN_FILE names"}, planted)
	if !strings.Contains(source.prefix, file) {
		t.Errorf("the command to paste %q does not name the file that signed in", source.prefix)
	}
	t.Run("a store call whose credential refresh cannot read the file again", func(t *testing.T) {
		_, read := webIdentityToken(file).GetIdentityToken()
		if read == nil {
			t.Fatal("control: the token file was read, so the row is blind")
		}
		// The SDK's chain for a store call whose credential refresh failed to read the token.
		failed := &smithy.OperationError{ServiceID: secretsmanager.ServiceID, OperationName: "ListSecrets",
			Err: fmt.Errorf("failed to refresh cached credentials, %w", fmt.Errorf("failed to retrieve jwt from provide source, %w", read))}
		wantNamedNotPrinted(t, "the store call's words", awsStoreFailure("ListSecrets", failed).Error(),
			[]string{"Secrets Manager ListSecrets failed: read the file AWS_WEB_IDENTITY_TOKEN_FILE names: open: no such file or directory"}, planted)
	})
}
