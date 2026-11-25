package gcp_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"

	"github.com/sjawhar/envoy/internal/cipher/secretstore"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/gcp"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/gcp/gcptest"
)

// TestACredentialsFilesAccountPrintsOnlyWhereItIsAServiceAccounts: the account a credentials
// file names - a key file's client_email, or the account in an impersonating file's
// service_account_impersonation_url - is text in a credential's file, so the using line and every
// store refusal name it only where it has a service account's shape. Control: a service account's
// email in either field still prints.
func TestACredentialsFilesAccountPrintsOnlyWhereItIsAServiceAccounts(t *testing.T) {
	t.Parallel()
	planted := "hunter2pw-" + "fileaccount-" + "9e1d"
	const robot = "deploy-bot@example-project.iam.gserviceaccount.com"
	impersonating := func(account string) string {
		return `{"type":"external_account","service_account_impersonation_url":"https://iamcredentials.googleapis.com/v1/projects/-/serviceAccounts/` + account + `:generateAccessToken"}`
	}
	for _, row := range []struct{ name, file, want string }{
		{"a key file's client_email that is no service account's", `{"type":"service_account","client_email":"` + planted + `"}`, "the service account the credentials file names (not printed"},
		{"an impersonated account that is no service account's", impersonating(planted), "the service account the credentials file names (not printed"},
		{"control: a key file's service account", `{"type":"service_account","client_email":"` + robot + `"}`, robot},
		{"control: an impersonated service account", impersonating(robot), robot},
	} {
		t.Run(row.name, func(t *testing.T) {
			got := gcp.CredentialsFilePrincipal(&google.Credentials{JSON: []byte(row.file)})
			wantNamedNotPrinted(t, "the principal", got, []string{row.want}, planted)
		})
	}
}

// TestCheckSettingsKMSKeyName covers OpenWriter's refusals of settings whose prefix, project and
// key name do not agree (checkSettingsKMSKeyName).
func TestCheckSettingsKMSKeyName(t *testing.T) {
	t.Parallel()
	if err := gcp.CheckSettingsForTest(gcptest.Settings()); err != nil {
		t.Fatalf("valid settings refused: %v", err)
	}
	bad := gcptest.Settings()
	bad.Prefix = "not-a-prefix"
	if err := gcp.CheckSettingsForTest(bad); err == nil {
		t.Fatal("an invalid secrets_prefix was accepted")
	}
	mismatched := gcptest.Settings()
	mismatched.Fields["gcp_project"] = "999999999999"
	if err := gcp.CheckSettingsForTest(mismatched); err == nil {
		t.Fatal("a gcp_project not matching secrets_prefix's project number was accepted")
	} else if !strings.Contains(err.Error(), "999999999999") || !strings.Contains(err.Error(), gcptest.SettingsProject) {
		t.Fatalf("error does not name both projects: %v", err)
	}
	badKey := gcptest.Settings()
	badKey.Fields["kms_key_name"] = "bad"
	if err := gcp.CheckSettingsForTest(badKey); err == nil {
		t.Fatal("an invalid key name was accepted")
	}
}

// TestGcloudTokenRefreshThatNeverAnswersEndsAtTheStoreDeadline: an expired gcloud token is
// refreshed through gcloudToken.Token, which runs `gcloud auth print-access-token`. A gcloud that
// never answers - here a script whose child holds its output open, as the real gcloud's Python does
// under its shell wrapper - ends at the deadline on each store call (secretstore.CallDeadline), so a stalled
// refresh cannot outlast the store call it serves; the error names the account and the deadline.
func TestGcloudTokenRefreshThatNeverAnswersEndsAtTheStoreDeadline(t *testing.T) {
	useCallDeadline(t, 300*time.Millisecond)
	dir := t.TempDir()
	gcloud := filepath.Join(dir, "gcloud")
	if err := os.WriteFile(gcloud, []byte("#!/bin/sh\nsleep 3600\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	expired := &oauth2.Token{AccessToken: "old", TokenType: "Bearer", Expiry: time.Now().Add(-time.Minute)}
	source := oauth2.ReuseTokenSource(expired, gcp.GcloudTokenForTest(context.Background(), gcloud, gcptest.Run, "ada@example.com"))
	done := make(chan error, 1)
	go func() {
		_, err := source.Token()
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "ada@example.com") || !strings.Contains(err.Error(), "did not answer within 300ms") {
			t.Fatalf("Token() = %v; want a failure naming the account and the 300ms deadline", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the refresh of an expired token was still waiting on gcloud 10s on, past the 300ms deadline")
	}
}

// useCallDeadline makes d the deadline on each store call (secretstore.CallDeadline) for the rest
// of t.
func useCallDeadline(t *testing.T, d time.Duration) {
	t.Helper()
	restore := secretstore.CallDeadline
	secretstore.CallDeadline = d
	t.Cleanup(func() { secretstore.CallDeadline = restore })
}
