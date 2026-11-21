package ciphertoken_test

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/ciphertoken/ciphertokentest"
)

// The two accounts GoogleAccounts reads for: example names alone.
const (
	firstAccount  = "first@example-project.iam.gserviceaccount.com"
	secondAccount = "second@example-project.iam.gserviceaccount.com"
)

// TestGoogleAccountsMintsEachAccountsOwnToken: each Read mints at generateIdToken for the account it
// names, for the reader's audience with includeEmail, with this pod's own access token.
func TestGoogleAccountsMintsEachAccountsOwnToken(t *testing.T) {
	ciphertokentest.NewMetadataStand(t)
	iam := ciphertokentest.NewIAMStand(t)
	accounts := &ciphertoken.GoogleAccounts{Audience: gatewayAudience, IAMCredentialsURL: iam.Server.URL}
	for _, account := range []string{firstAccount, secondAccount} {
		if token, err := accounts.Read(account); err != nil || token != ciphertokentest.IAMIdentityToken {
			t.Fatalf("Read(%s) = %q, %v; want the minted identity token", account, token, err)
		}
	}
	asked := iam.Requests()
	if len(asked) != 2 {
		t.Fatalf("IAM got %d requests, want one per account", len(asked))
	}
	for i, account := range []string{firstAccount, secondAccount} {
		if want := "/v1/projects/-/serviceAccounts/" + account + ":generateIdToken"; asked[i].Path != want {
			t.Errorf("mint %d asked %s, want %s", i, asked[i].Path, want)
		}
		if asked[i].Body["audience"] != gatewayAudience || asked[i].Body["includeEmail"] != true {
			t.Errorf("mint %d's body is %v; want the audience and includeEmail", i, asked[i].Body)
		}
		if asked[i].Authorization != "Bearer "+ciphertokentest.MetadataAccessBearer {
			t.Errorf("mint %d was called with %q, want the pod's own access token", i, asked[i].Authorization)
		}
	}
}

// TestGoogleAccountsRefusesAnEmptyAccount: a Read that names no account is refused and reads
// nothing, not this machine's own identity.
func TestGoogleAccountsRefusesAnEmptyAccount(t *testing.T) {
	metadata := ciphertokentest.NewMetadataStand(t)
	iam := ciphertokentest.NewIAMStand(t)
	accounts := &ciphertoken.GoogleAccounts{Audience: gatewayAudience, IAMCredentialsURL: iam.Server.URL}
	if token, err := accounts.Read(""); err == nil || token != "" || !strings.Contains(err.Error(), "no service account") {
		t.Fatalf("Read(\"\") = %q, %v; want a refusal naming no account", token, err)
	}
	if paths := metadata.Paths(); len(paths) != 0 || len(iam.Requests()) != 0 {
		t.Fatalf("a refused Read asked the metadata server %v and IAM %d times; want nothing", paths, len(iam.Requests()))
	}
}

// TestGoogleAccountsSharesOneReaderPerAccount: two reads of one account mint twice at IAM, each
// read its own identity token, and read this pod's own access token from the metadata server once,
// since the account's one reader keeps the access-token source.
func TestGoogleAccountsSharesOneReaderPerAccount(t *testing.T) {
	metadata := ciphertokentest.NewMetadataStand(t)
	iam := ciphertokentest.NewIAMStand(t)
	accounts := &ciphertoken.GoogleAccounts{Audience: gatewayAudience, IAMCredentialsURL: iam.Server.URL}
	for range 2 {
		if _, err := accounts.Read(firstAccount); err != nil {
			t.Fatalf("Read(%s): %v", firstAccount, err)
		}
	}
	if got := len(iam.Requests()); got != 2 {
		t.Fatalf("two reads minted %d times, want twice", got)
	}
	if got := metadata.Asked(ciphertokentest.MetadataTokenPath); got != 1 {
		t.Fatalf("two reads of one account asked the metadata server for the access token %d times, want once", got)
	}
}

// TestGoogleAccountsMintsForEachConcurrentReader: two reads of one account made at once each reach
// IAM with a mint of their own, while IAM holds the first: no read joins another's mint, as
// GoogleAccount's own Read joins them.
func TestGoogleAccountsMintsForEachConcurrentReader(t *testing.T) {
	ciphertokentest.NewMetadataStand(t)
	iam := ciphertokentest.NewIAMStand(t)
	release := iam.Hold(time.Minute)
	defer release()
	accounts := &ciphertoken.GoogleAccounts{Audience: gatewayAudience, IAMCredentialsURL: iam.Server.URL}
	var done sync.WaitGroup
	for range 2 {
		done.Go(func() { _, _ = accounts.Read(firstAccount) })
	}
	for deadline := time.Now().Add(5 * time.Second); len(iam.Requests()) < 2; time.Sleep(2 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("two concurrent reads reached IAM %d times within 5s while it held them, want twice", len(iam.Requests()))
		}
	}
	release()
	done.Wait()
}
