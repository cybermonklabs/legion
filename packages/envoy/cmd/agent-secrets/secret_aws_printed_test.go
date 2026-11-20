// packages/envoy/cmd/agent-secrets/secret_aws_printed_test.go
//
// What the AWS store forms print about the web identity token file AWS_WEB_IDENTITY_TOKEN_FILE
// names: the file holds a token, and the variable is where a person who pasted the token itself
// puts it, so a file the forms cannot read is refused naming the variable and the system's reason,
// never the path the AWS SDK's own error quotes.
package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/sts"

	"github.com/sjawhar/envoy/internal/broker/policy"
	"github.com/sjawhar/envoy/internal/broker/policy/policytest"
	"github.com/sjawhar/envoy/internal/broker/secrets"
)

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
		{"AWS_ENDPOINT_URL_STS that does not parse", func(t *testing.T) {
			t.Setenv("AWS_ENDPOINT_URL_STS", unparseable("127.0.0.1:1"))
		}, "read your AWS sign-in: "},
		{"AWS_ENDPOINT_URL with a secret in its path", func(t *testing.T) { t.Setenv("AWS_ENDPOINT_URL", closing.URL+"/"+secret) },
			"read your AWS sign-in: a request the AWS SDK made failed: the request failed (the address is not printed; AWS_ENDPOINT_URL is set)"},
		{"AWS_MAX_ATTEMPTS that is no number", func(t *testing.T) { t.Setenv("AWS_MAX_ATTEMPTS", secret) },
			"load your AWS sign-in: the AWS SDK could not sign in from this environment (its message is not printed: it can quote an AWS setting's value)"},
		{"STS refuses with its own message: its code prints, never the message", func(t *testing.T) { t.Setenv("AWS_ENDPOINT_URL_STS", refusing.URL) },
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
			wantNamedNotPrinted(t, "stderr", stderr, []string{row.want}, secret, user)
		})
	}
}
