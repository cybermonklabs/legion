// googleaccount_test.go drives the reader of a Google identity token both ways it reads one. This
// machine's own, against a stub metadata server: how long it reuses what it read, and what
// concurrent readers of a cold cache cost, which on a listener is the per-request authentication
// path (introspect.go). And a named service account's, minted at generateIdToken against
// brokertokentest's metadata server and IAM stands-in: the call's shape, the one mint concurrent
// readers share, and that a mint that fails, redirects or stalls is an error with no fallback.
package brokertoken_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/brokertoken"
	"github.com/sjawhar/envoy/internal/brokertoken/brokertokentest"
	"github.com/sjawhar/envoy/internal/googleidentity"
)

// metadataAudience is the broker audience these reads ask the metadata server to mint for.
const metadataAudience = "https://broker.test"

// stubMetadata stands up a metadata server at the host GCE_METADATA_HOST names, answering
// handler, and returns how many requests it was asked.
func stubMetadata(t *testing.T, handler http.HandlerFunc) *atomic.Int64 {
	t.Helper()
	var asked atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked.Add(1)
		handler(w, r)
	}))
	t.Cleanup(server.Close)
	t.Setenv(googleidentity.MetadataHostVar, strings.TrimPrefix(server.URL, "http://"))
	return &asked
}

// TestGoogleAccountReadsAgainOnceItsCacheLapses: a token read once is answered to every later
// reader without reaching the metadata server while it is younger than TTL, and a reader past the
// TTL reads afresh.
func TestGoogleAccountReadsAgainOnceItsCacheLapses(t *testing.T) {
	var minted atomic.Int64
	asked := stubMetadata(t, func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, "token-%d\n", minted.Add(1))
	})
	account := brokertoken.NewGoogleAccount(metadataAudience)

	first, err := account.Read()
	if err != nil || first != "token-1" {
		t.Fatalf("the first read = %q, %v; want the stub's first token", first, err)
	}
	if cached, err := account.Read(); err != nil || cached != first {
		t.Fatalf("the read inside the TTL = %q, %v; want the cached %q", cached, err, first)
	}
	if got := asked.Load(); got != 1 {
		t.Fatalf("the metadata server was asked %d times inside the TTL, want 1", got)
	}

	account.TTL = 0
	if second, err := account.Read(); err != nil || second != "token-2" {
		t.Fatalf("the read past the TTL = %q, %v; want the stub's second token", second, err)
	}
	if got := asked.Load(); got != 2 {
		t.Errorf("the metadata server was asked %d times across a lapsed cache, want 2", got)
	}
}

// TestGoogleAccountSharesOneMetadataReadBetweenConcurrentReaders: readers that arrive while a read
// is in flight take that read's answer rather than each asking the metadata server in turn. The
// metadata server here refuses, which is the case the rule is for: nothing is cached on a failure,
// so before this each queued reader paid internal/googleidentity's whole timeout after the one
// before it, while the lock was held across the read. Every reader is answered the one refusal,
// which names the audience and no token, and the next read asks again because no failure is kept.
func TestGoogleAccountSharesOneMetadataReadBetweenConcurrentReaders(t *testing.T) {
	const readers = 8
	release := make(chan struct{})
	asked := stubMetadata(t, func(w http.ResponseWriter, _ *http.Request) {
		<-release
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	account := brokertoken.NewGoogleAccount(metadataAudience)

	var started, done sync.WaitGroup
	started.Add(readers)
	done.Add(readers)
	errs := make([]error, readers)
	for i := range readers {
		go func() {
			defer done.Done()
			started.Done()
			_, errs[i] = account.Read()
		}()
	}
	// The whole point is what the readers behind the first one do, so the stub is held until
	// every reader has been scheduled and given a moment to reach the read; nothing a reader
	// blocked inside the shared read is observable from here.
	started.Wait()
	time.Sleep(50 * time.Millisecond)
	close(release)
	done.Wait()

	for i := range readers {
		if errs[i] == nil || !strings.Contains(errs[i].Error(), metadataAudience) {
			t.Fatalf("reader %d = %v, want a refusal naming the audience", i, errs[i])
		}
	}
	if got := asked.Load(); got != 1 {
		t.Errorf("the metadata server was asked %d times for %d readers of a cold cache, want the one read they share", got, readers)
	}
	if _, err := account.Read(); err == nil {
		t.Fatal("the read after the shared one succeeded; a failure is never cached, so it asks again")
	}
	if got := asked.Load(); got != 2 {
		t.Errorf("the metadata server was asked %d times over the later read, want 2: no failure is cached", got)
	}
}

// judgeAccount is the service account the mint tests name, a placeholder of the form
// secrets.ValidServiceAccountEmail takes, and gatewayAudience the audience its token is minted
// for, as the broker's judge mints one for its model gateway.
const (
	judgeAccount    = "judge-model@example-project.iam.gserviceaccount.com"
	gatewayAudience = "https://gateway.example"
)

// mintPath is the generateIdToken route a mint of judgeAccount's token calls, and mintPrefix how
// every failed one opens: the account and the audience first, since the judge writes the error
// into the audit row a person reads.
const (
	mintPath   = "/v1/projects/-/serviceAccounts/" + judgeAccount + ":generateIdToken"
	mintPrefix = "brokertoken: mint the Google identity token of " + judgeAccount + " for " + gatewayAudience + " at IAM Service Account Credentials: "
)

// mintingAccount is a reader of judgeAccount's identity token for gatewayAudience, minted at iam.
func mintingAccount(iam *brokertokentest.IAMStand) *brokertoken.GoogleAccount {
	account := brokertoken.NewGoogleAccount(gatewayAudience)
	account.ServiceAccount = judgeAccount
	account.IAMCredentialsURL = iam.Server.URL
	return account
}

// TestGoogleAccountWithNoServiceAccountReadsTheMetadataIdentityAndAsksIAMNothing: a reader with no
// service account is the one every listener runs, and every judge with BROKER_JUDGE_MODEL_ACCOUNT
// unset, and it reads as it always has, the metadata server's identity token, even with an IAM
// stand-in named for it to reach: it asks generateIdToken nothing and reads no access token.
func TestGoogleAccountWithNoServiceAccountReadsTheMetadataIdentityAndAsksIAMNothing(t *testing.T) {
	metadata := brokertokentest.NewMetadataStand(t)
	iam := brokertokentest.NewIAMStand(t)
	account := brokertoken.NewGoogleAccount(gatewayAudience)
	account.IAMCredentialsURL = iam.Server.URL

	if token, err := account.Read(); err != nil || token != brokertokentest.MetadataIdentityToken {
		t.Fatalf("Read = %q, %v; want the metadata server's identity token", token, err)
	}
	if got := iam.Requests(); len(got) != 0 {
		t.Errorf("IAM Service Account Credentials got %v; want no request from a reader with no service account", got)
	}
	if got := metadata.Paths(); len(got) != 1 || got[0] != brokertokentest.MetadataIdentityPath {
		t.Errorf("the metadata server got %v; want the identity path alone, and no access token read", got)
	}
}

// TestGoogleAccountWithAServiceAccountMintsItsIdentityTokenAtGenerateIdToken: a reader with a
// service account answers the token generateIdToken mints, from one call of the shape Google's
// reference gives — a POST for that account, includeEmail set, the reader's audience — carrying
// the pod's own access token as its bearer, and it never asks the metadata server's identity
// path. The minted token is cached as a read one is: a second Read inside the TTL asks nothing.
func TestGoogleAccountWithAServiceAccountMintsItsIdentityTokenAtGenerateIdToken(t *testing.T) {
	metadata := brokertokentest.NewMetadataStand(t)
	iam := brokertokentest.NewIAMStand(t)
	account := mintingAccount(iam)

	if token, err := account.Read(); err != nil || token != brokertokentest.IAMIdentityToken {
		t.Fatalf("Read = %q, %v; want the token generateIdToken minted", token, err)
	}
	requests := iam.Requests()
	if len(requests) != 1 {
		t.Fatalf("IAM Service Account Credentials got %d requests; want the one mint", len(requests))
	}
	mint := requests[0]
	if mint.Method != http.MethodPost || mint.Path != mintPath {
		t.Errorf("the mint was %s %s; want POST %s", mint.Method, mint.Path, mintPath)
	}
	if mint.Body["includeEmail"] != true || mint.Body["audience"] != gatewayAudience || len(mint.Body) != 2 {
		t.Errorf("the mint asked %v; want includeEmail true and the audience %s, and nothing else", mint.Body, gatewayAudience)
	}
	if want := "Bearer " + brokertokentest.MetadataAccessBearer; mint.Authorization != want {
		t.Errorf("the mint carried Authorization %q; want %q, the pod's own access token", mint.Authorization, want)
	}
	if got := metadata.Asked(brokertokentest.MetadataIdentityPath); got != 0 {
		t.Errorf("the metadata server's identity path was asked %d times; want none on the mint route", got)
	}

	if token, err := account.Read(); err != nil || token != brokertokentest.IAMIdentityToken {
		t.Fatalf("the read inside the TTL = %q, %v; want the cached mint", token, err)
	}
	if got := len(iam.Requests()); got != 1 {
		t.Errorf("IAM Service Account Credentials got %d requests over two reads inside the TTL; want 1", got)
	}
	if got := metadata.Paths(); len(got) != 1 {
		t.Errorf("the metadata server got %v over two reads inside the TTL; want the one access-token read", got)
	}
}

// TestGoogleAccountWithAServiceAccountSharesOneMintAmongConcurrentReaders: readers that arrive
// while a mint is in flight take its token, as they take a metadata read's, rather than each
// minting in turn. The stand-in holds its answer until the first mint has reached it and the
// other readers have had a moment to queue behind it.
func TestGoogleAccountWithAServiceAccountSharesOneMintAmongConcurrentReaders(t *testing.T) {
	const readers = 8
	brokertokentest.NewMetadataStand(t)
	iam := brokertokentest.NewIAMStand(t)
	release := iam.Hold(time.Minute)
	defer release()
	account := mintingAccount(iam)

	var started, done sync.WaitGroup
	started.Add(readers)
	done.Add(readers)
	tokens := make([]string, readers)
	errs := make([]error, readers)
	for i := range readers {
		go func() {
			defer done.Done()
			started.Done()
			tokens[i], errs[i] = account.Read()
		}()
	}
	started.Wait()
	for deadline := time.Now().Add(5 * time.Second); len(iam.Requests()) == 0; time.Sleep(2 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("no mint reached IAM Service Account Credentials within 5s")
		}
	}
	time.Sleep(50 * time.Millisecond)
	release()
	done.Wait()

	for i := range readers {
		if errs[i] != nil || tokens[i] != brokertokentest.IAMIdentityToken {
			t.Errorf("reader %d = %q, %v; want the one minted token", i, tokens[i], errs[i])
		}
	}
	if got := len(iam.Requests()); got != 1 {
		t.Errorf("IAM Service Account Credentials got %d requests from %d readers of a cold cache; want the one mint they share", got, readers)
	}
}

// TestGoogleAccountWithARefusedMintIsAnErrorAndNoFallback: a mint Google refuses is an error that
// names the account and the audience before what failed, carries Google's reason cut to 512
// bytes, and holds neither the pod's access token nor any identity token. The metadata server
// here would still hand out an identity token, so a reader that fell back would answer a bearer
// for the pod's own identity; it answers none, and the identity path is never asked.
func TestGoogleAccountWithARefusedMintIsAnErrorAndNoFallback(t *testing.T) {
	metadata := brokertokentest.NewMetadataStand(t)
	iam := brokertokentest.NewIAMStand(t)
	// Google's refusal of a caller without roles/iam.serviceAccountOpenIdTokenCreator, and a
	// marker that only a body read past the cut would carry.
	const refusal = `{"error":{"code":403,"message":"Permission 'iam.serviceAccounts.getOpenIdToken' denied on resource (or it may not exist).","status":"PERMISSION_DENIED"}}`
	iam.AnswersStatus(http.StatusForbidden, refusal+strings.Repeat(" ", 512)+"past-the-cut")
	account := mintingAccount(iam)

	token, err := account.Read()
	if token != "" || err == nil {
		t.Fatalf("Read = %q, %v; want no token and an error", token, err)
	}
	text := err.Error()
	if !strings.HasPrefix(text, mintPrefix) {
		t.Errorf("the error %q does not open with %q", text, mintPrefix)
	}
	for _, want := range []string{"HTTP 403", "PERMISSION_DENIED"} {
		if !strings.Contains(text, want) {
			t.Errorf("the error %q does not carry %q", text, want)
		}
	}
	if strings.Contains(text, "past-the-cut") {
		t.Errorf("the error %q carries Google's body past 512 bytes", text)
	}
	for _, held := range []string{brokertokentest.MetadataAccessBearer, brokertokentest.MetadataIdentityToken} {
		if strings.Contains(text, held) {
			t.Errorf("the error %q holds the token %q", text, held)
		}
	}
	if got := metadata.Asked(brokertokentest.MetadataIdentityPath); got != 0 {
		t.Errorf("the metadata server's identity path was asked %d times; a refused mint falls back to nothing", got)
	}
}

// TestGoogleAccountWithAServiceAccountFollowsNoRedirect: the mint carries the pod's own access
// token, so a 3xx is an error and never a second call: the host a Location names gets nothing,
// even one that would answer a token.
func TestGoogleAccountWithAServiceAccountFollowsNoRedirect(t *testing.T) {
	brokertokentest.NewMetadataStand(t)
	iam := brokertokentest.NewIAMStand(t)
	elsewhere := brokertokentest.NewIAMStand(t)
	iam.Redirects(elsewhere.Server.URL + mintPath)
	account := mintingAccount(iam)

	token, err := account.Read()
	if token != "" || err == nil || !strings.HasPrefix(err.Error(), mintPrefix) || !strings.Contains(err.Error(), "HTTP 307") {
		t.Fatalf("Read = %q, %v; want no token and an error naming the account, the audience and the 307", token, err)
	}
	if got := elsewhere.Requests(); len(got) != 0 {
		t.Errorf("the redirect's target got %v; want no request, and no access token", got)
	}
}

// TestGoogleAccountMintIsBounded: a generateIdToken that holds its answer costs a reader
// MintTimeout and not the whole hold, and the error names the account, the audience and the
// deadline.
func TestGoogleAccountMintIsBounded(t *testing.T) {
	brokertokentest.NewMetadataStand(t)
	iam := brokertokentest.NewIAMStand(t)
	release := iam.Hold(5 * time.Second)
	defer release()
	account := mintingAccount(iam)
	account.MintTimeout = 300 * time.Millisecond

	began := time.Now()
	token, err := account.Read()
	took := time.Since(began)
	if token != "" || err == nil || !strings.HasPrefix(err.Error(), mintPrefix) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Read = %q, %v; want no token and the deadline, naming the account and the audience", token, err)
	}
	if took > 2*time.Second {
		t.Errorf("Read took %s against a mint held 5s; want it ended near its 300ms bound", took)
	}
}
