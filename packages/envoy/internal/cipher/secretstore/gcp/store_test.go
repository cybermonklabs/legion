package gcp_test

import (
	"context"
	"errors"
	"maps"
	"strings"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/sjawhar/envoy/internal/cipher/secretstore"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/gcp"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/gcp/gcptest"
	"github.com/sjawhar/envoy/internal/testwait"
)

const (
	fakeProject = gcptest.Project
	fakePrefix  = fakeProject + "/secrets/agent-secrets-"
	fakeKey     = "projects/example-project/locations/global/keyRings/agent-secrets/cryptoKeys/agent-secrets"
)

func servedSecret(value string) *gcptest.FakeSecret {
	return &gcptest.FakeSecret{
		Annotations:    map[string]string{"owner": "ada@example.com", "tier": "human"},
		ReplicationKey: fakeKey, VersionKey: fakeKey,
		State: secretmanagerpb.SecretVersion_ENABLED, Value: []byte(value),
	}
}

// noVersionCreated is when the fake's no-version secret was created, as its create_time names it.
var noVersionCreated = time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)

func TestGCPListReadsAnnotationsKeyStateAndExpiry(t *testing.T) {
	f := &gcptest.FakeSecretManager{Secrets: map[string]*gcptest.FakeSecret{
		fakePrefix + "served":      servedSecret("v1"),
		fakePrefix + "default-key": {Annotations: map[string]string{"owner": "shared", "tier": "agent"}, State: secretmanagerpb.SecretVersion_ENABLED, Value: []byte("v")},
		fakePrefix + "disabled":    {Annotations: map[string]string{"owner": "shared", "tier": "agent"}, ReplicationKey: fakeKey, VersionKey: fakeKey, State: secretmanagerpb.SecretVersion_DISABLED, Value: []byte("v")},
		fakePrefix + "no-version":  {Annotations: map[string]string{"owner": "shared", "tier": "agent"}, ReplicationKey: fakeKey, NoVersion: true, Created: noVersionCreated},
		fakePrefix + "expiring":    {Annotations: map[string]string{"owner": "shared", "tier": "agent"}, ReplicationKey: fakeKey, VersionKey: fakeKey, State: secretmanagerpb.SecretVersion_ENABLED, Expires: time.Now().Add(30 * 24 * time.Hour), Value: []byte("v")},
		fakePrefix + "expired":     {Annotations: map[string]string{"owner": "shared", "tier": "agent"}, ReplicationKey: fakeKey, VersionKey: fakeKey, State: secretmanagerpb.SecretVersion_ENABLED, Expires: time.Now().Add(-time.Minute), Value: []byte("v")},
		fakePrefix + "no-tags":     {ReplicationKey: fakeKey, VersionKey: fakeKey, State: secretmanagerpb.SecretVersion_ENABLED, Value: []byte("v")},
		// The name filter is a case-insensitive substring match, so these two are listed too;
		// neither is the broker's, and a version lookup on either is denied.
		fakeProject + "/secrets/other-agent-secrets-key": {DenyVersion: true},
		fakeProject + "/secrets/AGENT-SECRETS-upper":     {DenyVersion: true},
	}}
	store := gcp.New(gcptest.Start(t, f), fakeKey)
	ctx := context.Background()
	listings, err := store.List(ctx, fakePrefix)
	if err != nil {
		t.Fatalf("List = %v; want the secrets outside the prefix dropped before any call for them", err)
	}
	byID := map[string]secretstore.Listing{}
	for _, ls := range listings {
		byID[ls.ID] = ls
	}
	if len(byID) != 5 {
		t.Fatalf("List answered %d secrets, want the 5 under the prefix with no expire_time: %+v", len(byID), listings)
	}
	onKey := func(ls secretstore.Listing) bool {
		t.Helper()
		on, err := ls.OnKey(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return on
	}
	served := byID[fakePrefix+"served"]
	if served.Source != served.ID || served.Tags["owner"] != "ada@example.com" || served.Tags["tier"] != "human" || !onKey(served) || !served.HasCurrentValue {
		t.Fatalf("served = %+v; want on the key, with a value, tagged from its annotations", served)
	}
	if ls := byID[fakePrefix+"default-key"]; onKey(ls) || !ls.HasCurrentValue {
		t.Fatalf("default-key = %+v; want not on the key", ls)
	}
	if ls := byID[fakePrefix+"disabled"]; !onKey(ls) || ls.HasCurrentValue {
		t.Fatalf("disabled = %+v; want on the key with no current value", ls)
	}
	if ls := byID[fakePrefix+"no-version"]; !onKey(ls) || ls.HasCurrentValue || !ls.Created.Equal(noVersionCreated) {
		t.Fatalf("no-version = %+v; want on the key (its replication policy's) with no current value, created %s", ls, noVersionCreated)
	}
	if ls := byID[fakePrefix+"default-key"]; !ls.Created.IsZero() {
		t.Fatalf("default-key = %+v; want no creation time, the fake naming none", ls)
	}
	for _, name := range []string{"expiring", "expired"} {
		if ls, listed := byID[fakePrefix+name]; listed {
			t.Fatalf("%s = %+v; want it left out from the moment it has an expire_time, as delete sets one", name, ls)
		}
	}
	if ls := byID[fakePrefix+"no-tags"]; len(ls.Tags) != 0 || ls.Tags == nil {
		t.Fatalf("no-tags = %+v; want an empty, non-nil tag map", ls)
	}
}

// TestGCPListAndDescribeReadNoLabel: the broker reads a secret's tags from its annotations alone,
// so the label `agent-secrets secret create` leaves on a secret it made (agent-secrets-create,
// a random value of that create's) reaches neither List's nor Describe's tags: the secret lists
// and describes with exactly its annotations as tags, and is served as the same secret without it.
func TestGCPListAndDescribeReadNoLabel(t *testing.T) {
	labelled := servedSecret("v1")
	labelled.Labels = map[string]string{secretstore.CreateLabel: "0123456789abcdef0123456789abcdef"}
	f := &gcptest.FakeSecretManager{Secrets: map[string]*gcptest.FakeSecret{
		fakePrefix + "labelled": labelled,
		fakePrefix + "plain":    servedSecret("v1"),
	}}
	store := gcp.New(gcptest.Start(t, f), fakeKey)
	ctx := context.Background()
	listings, err := store.List(ctx, fakePrefix)
	if err != nil {
		t.Fatal(err)
	}
	described, err := store.Describe(ctx, fakePrefix+"labelled")
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]secretstore.Listing{}
	for _, ls := range listings {
		byID[ls.ID] = ls
	}
	plain := byID[fakePrefix+"plain"]
	for how, ls := range map[string]secretstore.Listing{"listed": byID[fakePrefix+"labelled"], "described": described} {
		if !maps.Equal(ls.Tags, labelled.Annotations) || ls.HasCurrentValue != plain.HasCurrentValue || ls.Version != plain.Version || ls.Unfilled {
			t.Errorf("the labelled secret %s as %+v; want exactly its annotations %v as tags, served as the same secret without the label (%+v)", how, ls, labelled.Annotations, plain)
		}
	}
}

// TestGCPListingIsUnfilledOnlyForALabelledSecretWithNoVersion: a listing is Unfilled - a secret
// `agent-secrets secret create` made and has not yet given a value - only where the secret carries
// the create label and holds no version at all: one without the label, or with a version (a
// disabled one included), is not, however young.
func TestGCPListingIsUnfilledOnlyForALabelledSecretWithNoVersion(t *testing.T) {
	label := map[string]string{secretstore.CreateLabel: "0123456789abcdef0123456789abcdef"}
	tags := map[string]string{"owner": "shared", "tier": "agent"}
	f := &gcptest.FakeSecretManager{Secrets: map[string]*gcptest.FakeSecret{
		fakePrefix + "labelled-empty":    {Annotations: tags, Labels: label, ReplicationKey: fakeKey, NoVersion: true, Created: noVersionCreated},
		fakePrefix + "unlabelled-empty":  {Annotations: tags, ReplicationKey: fakeKey, NoVersion: true, Created: noVersionCreated},
		fakePrefix + "labelled-disabled": {Annotations: tags, Labels: label, ReplicationKey: fakeKey, VersionKey: fakeKey, State: secretmanagerpb.SecretVersion_DISABLED, Value: []byte("v"), Created: noVersionCreated},
	}}
	store := gcp.New(gcptest.Start(t, f), fakeKey)
	listings, err := store.List(context.Background(), fakePrefix)
	if err != nil {
		t.Fatal(err)
	}
	for _, ls := range listings {
		if want := ls.ID == fakePrefix+"labelled-empty"; ls.Unfilled != want || ls.HasCurrentValue {
			t.Errorf("%s: Unfilled %t, current value %t; want Unfilled %t and no current value", ls.ID, ls.Unfilled, ls.HasCurrentValue, want)
		}
	}
	if len(listings) != 3 {
		t.Fatalf("listed %d secrets, want 3", len(listings))
	}
}

func TestGCPDescribeAndRead(t *testing.T) {
	f := &gcptest.FakeSecretManager{Secrets: map[string]*gcptest.FakeSecret{
		fakePrefix + "served":   servedSecret("hello"),
		fakePrefix + "disabled": {Annotations: map[string]string{}, ReplicationKey: fakeKey, VersionKey: fakeKey, State: secretmanagerpb.SecretVersion_DISABLED, Value: []byte("v")},
		fakePrefix + "empty":    {Annotations: map[string]string{}, ReplicationKey: fakeKey, VersionKey: fakeKey, State: secretmanagerpb.SecretVersion_ENABLED, Value: nil},
		fakePrefix + "binary":   {Annotations: map[string]string{}, ReplicationKey: fakeKey, VersionKey: fakeKey, State: secretmanagerpb.SecretVersion_ENABLED, Value: []byte{0xff, 0xfe}},
		fakePrefix + "expiring": {Annotations: map[string]string{}, ReplicationKey: fakeKey, VersionKey: fakeKey, State: secretmanagerpb.SecretVersion_ENABLED, Expires: time.Now().Add(30 * 24 * time.Hour), Value: []byte("v")},
	}}
	store := gcp.New(gcptest.Start(t, f), fakeKey)
	ctx := context.Background()

	ls, err := store.Describe(ctx, fakePrefix+"served")
	if err != nil || ls.ID != fakePrefix+"served" || !ls.HasCurrentValue {
		t.Fatalf("Describe(served) = %+v, %v", ls, err)
	}
	if on, err := ls.OnKey(ctx); err != nil || !on {
		t.Fatalf("Describe(served).OnKey = %v, %v; want on the key", on, err)
	}
	for _, name := range []string{"missing", "expiring"} {
		if _, err := store.Describe(ctx, fakePrefix+name); !errors.Is(err, secretstore.ErrNotFound) {
			t.Fatalf("Describe(%s) = %v, want ErrNotFound", name, err)
		}
	}
	if v, err := store.Read(ctx, fakePrefix+"served"); err != nil || v != "hello" {
		t.Fatalf("Read(served) = %q, %v", strings.ReplaceAll(v, "hello", "[ok]"), err)
	}
	if _, err := store.Read(ctx, fakePrefix+"missing"); !errors.Is(err, secretstore.ErrNotFound) {
		t.Fatalf("Read(missing) = %v, want ErrNotFound", err)
	}
	if _, err := store.Read(ctx, fakePrefix+"disabled"); !errors.Is(err, secretstore.ErrNotFound) {
		t.Fatalf("Read(disabled) = %v, want ErrNotFound (FAILED_PRECONDITION)", err)
	}
	// Secret Manager would still answer its value; the store refuses it as Secrets Manager refuses
	// a secret in its recovery window, so a grant made before the delete reads nothing after it.
	if _, err := store.Read(ctx, fakePrefix+"expiring"); !errors.Is(err, secretstore.ErrNotFound) {
		t.Fatalf("Read(expiring) = %v, want ErrNotFound: a secret scheduled for deletion is not readable", err)
	}
	for _, name := range []string{"empty", "binary"} {
		_, err := store.Read(ctx, fakePrefix+name)
		if !errors.Is(err, secretstore.ErrValueNotText) || errors.Is(err, secretstore.ErrNotFound) || !strings.Contains(err.Error(), "non-empty UTF-8") || strings.Contains(err.Error(), "\xff") {
			t.Fatalf("Read(%s) = %v; want ErrValueNotText, naming the rule and never the bytes", name, err)
		}
	}
}

// TestGCPListingNamesTheLatestVersionsNumber: List and Describe answer, as each Listing's
// Version, the number of the version versions/latest resolves to, read from the GetSecretVersion
// each already makes, so the policy learns of a new value with no call of its own; a secret whose
// latest version holds no value (disabled) or that has no version answers "".
func TestGCPListingNamesTheLatestVersionsNumber(t *testing.T) {
	seventh := servedSecret("v7")
	seventh.Number = "7"
	f := &gcptest.FakeSecretManager{Secrets: map[string]*gcptest.FakeSecret{
		fakePrefix + "seventh":    seventh,
		fakePrefix + "disabled":   {Annotations: map[string]string{}, ReplicationKey: fakeKey, VersionKey: fakeKey, State: secretmanagerpb.SecretVersion_DISABLED, Value: []byte("v"), Number: "3"},
		fakePrefix + "no-version": {Annotations: map[string]string{}, ReplicationKey: fakeKey, NoVersion: true},
	}}
	store := gcp.New(gcptest.Start(t, f), fakeKey)
	ctx := context.Background()
	listings, err := store.List(ctx, fakePrefix)
	if err != nil {
		t.Fatal(err)
	}
	versions := map[string]string{}
	for _, ls := range listings {
		versions[strings.TrimPrefix(ls.ID, fakePrefix)] = ls.Version
	}
	if want := map[string]string{"seventh": "7", "disabled": "", "no-version": ""}; !maps.Equal(versions, want) {
		t.Fatalf("List's versions = %v, want %v", versions, want)
	}
	f.Mu.Lock()
	versionCalls := f.Calls["GetSecretVersion"]
	f.Mu.Unlock()
	if versionCalls != 3 {
		t.Fatalf("List made %d GetSecretVersion calls for three secrets; want one each, no more", versionCalls)
	}
	if ls, err := store.Describe(ctx, fakePrefix+"seventh"); err != nil || ls.Version != "7" {
		t.Fatalf("Describe(seventh) = %+v, %v; want version 7", ls, err)
	}
}

func TestGCPListFailureNamesTheCodeNotTheMessage(t *testing.T) {
	f := &gcptest.FakeSecretManager{FailList: status.Errorf(codes.PermissionDenied, "Permission denied on resource project 000000000000 (secret value: hunter2)")}
	store := gcp.New(gcptest.Start(t, f), fakeKey)
	_, err := store.List(context.Background(), fakePrefix)
	if err == nil || !strings.Contains(err.Error(), "PermissionDenied") || strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("List under a denied identity = %v; want the gRPC code and never the status message", err)
	}
}

func TestGCPValidNameAndPrefix(t *testing.T) {
	store := gcp.Store{KeyName: fakeKey}
	if err := store.ValidName(fakePrefix + "deel-api-key"); err != nil {
		t.Fatal(err)
	}
	if err := store.ValidName(fakePrefix + strings.Repeat("a", 256)); err == nil {
		t.Fatal("ValidName accepted a 270-character id")
	}
	if err := store.ValidName(fakePrefix + "has.dot"); err == nil {
		t.Fatal("ValidName accepted a dot")
	}
	if err := gcp.ValidPrefix(fakePrefix); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"projects/example-project/secrets/agent-secrets-", fakeProject + "/secrets/agent-secrets", fakeProject + "/agent-secrets-", "production/agent-secrets/"} {
		if err := gcp.ValidPrefix(bad); err == nil {
			t.Fatalf("ValidGCPPrefix(%q) accepted", bad)
		}
	}
	if got := gcp.Project(fakePrefix); got != "000000000000" {
		t.Fatalf("GCPProject = %q", got)
	}
	if err := gcp.ValidKeyName(fakeKey); err != nil {
		t.Fatal(err)
	}
	if err := gcp.ValidKeyName("projects/p/locations/us-west1/keyRings/r/cryptoKeys/k"); err == nil {
		t.Fatal("ValidGCPKeyName accepted a regional key")
	}
}

// TestGCPReadQuotaCost pins what each operation costs against Secret Manager's 600-per-minute read
// quota, which the whole project shares (AccessSecretVersion has its own, 90,000 per minute): a
// List of N secrets under the prefix is one ListSecrets and N GetSecretVersion, a Describe is one
// GetSecret and one GetSecretVersion, and a Read is one GetSecret and one AccessSecretVersion.
func TestGCPReadQuotaCost(t *testing.T) {
	f := &gcptest.FakeSecretManager{Secrets: map[string]*gcptest.FakeSecret{
		fakePrefix + "a": servedSecret("v"),
		fakePrefix + "b": servedSecret("v"),
		fakePrefix + "c": servedSecret("v"),
	}}
	store := gcp.New(gcptest.Start(t, f), fakeKey)
	ctx := context.Background()
	cost := func(op func() error) int {
		t.Helper()
		before := f.Reads()
		if err := op(); err != nil {
			t.Fatal(err)
		}
		return f.Reads() - before
	}
	if got := cost(func() error { _, err := store.List(ctx, fakePrefix); return err }); got != 1+3 {
		t.Fatalf("List of 3 secrets cost %d reads, want 4", got)
	}
	if got := cost(func() error { _, err := store.Describe(ctx, fakePrefix+"a"); return err }); got != 2 {
		t.Fatalf("Describe cost %d reads, want 2", got)
	}
	if got := cost(func() error { _, err := store.Read(ctx, fakePrefix+"a"); return err }); got != 1 {
		t.Fatalf("Read cost %d reads, want 1 (its value access is the separate quota)", got)
	}
}

// TestGCPRetriesAnUnavailableMetadataCall pins NewGCPClient's retry: a transient UNAVAILABLE on
// ListSecrets, GetSecret or GetSecretVersion is retried, so it fails neither a reload nor a reread.
func TestGCPRetriesAnUnavailableMetadataCall(t *testing.T) {
	f := &gcptest.FakeSecretManager{
		Secrets:     map[string]*gcptest.FakeSecret{fakePrefix + "a": servedSecret("v")},
		Unavailable: map[string]int{"ListSecrets": 1, "GetSecret": 1, "GetSecretVersion": 2},
	}
	store := gcp.New(gcptest.Start(t, f), fakeKey)
	ctx := context.Background()
	if ls, err := store.List(ctx, fakePrefix); err != nil || len(ls) != 1 {
		t.Fatalf("List through one UNAVAILABLE each = %d secrets, %v; want 1 after retries", len(ls), err)
	}
	if _, err := store.Describe(ctx, fakePrefix+"a"); err != nil {
		t.Fatalf("Describe through one UNAVAILABLE each = %v; want it to succeed after retries", err)
	}
	if f.Calls["ListSecrets"] != 2 || f.Calls["GetSecret"] != 2 || f.Calls["GetSecretVersion"] != 4 {
		t.Fatalf("calls = %v; want each failed call made again once", f.Calls)
	}
}

// TestGCPReadChecksDeletionOncePerWindow pins Read's cache: releasing the same secret again within
// gcpHeldFor costs no metadata read, the first release after it reads again, and a secret
// scheduled for deletion since is refused from that read on.
func TestGCPReadChecksDeletionOncePerWindow(t *testing.T) {
	f := &gcptest.FakeSecretManager{Secrets: map[string]*gcptest.FakeSecret{fakePrefix + "a": servedSecret("v")}}
	store := gcp.New(gcptest.Start(t, f), fakeKey)
	now := time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC)
	gcp.SetClockForTest(store, func() time.Time { return now })
	ctx := context.Background()
	read := func() error { _, err := store.Read(ctx, fakePrefix+"a"); return err }
	if err := read(); err != nil {
		t.Fatal(err)
	}
	before := f.Reads()
	for range 5 {
		if err := read(); err != nil {
			t.Fatal(err)
		}
	}
	if got := f.Reads() - before; got != 0 {
		t.Fatalf("five releases within the window cost %d metadata reads, want 0", got)
	}
	f.Mu.Lock()
	f.Secrets[fakePrefix+"a"].Expires = now.Add(30 * 24 * time.Hour)
	f.Mu.Unlock()
	now = now.Add(gcp.HeldForForTest)
	if err := read(); !errors.Is(err, secretstore.ErrNotFound) {
		t.Fatalf("Read after the window of a secret scheduled for deletion since = %v, want ErrNotFound", err)
	}
	if got := f.Reads() - before; got != 1 {
		t.Fatalf("the release after the window cost %d metadata reads, want 1", got)
	}
}

// TestGCPReadCachesARefusalAndCoalescesConcurrentChecks pins the rest of Read's bound: repeated
// releases of a secret scheduled for deletion cost one metadata read per window, as readable ones
// do, and concurrent releases of a cold secret wait for one check.
func TestGCPReadCachesARefusalAndCoalescesConcurrentChecks(t *testing.T) {
	gone := servedSecret("v")
	gone.Expires = time.Now().Add(30 * 24 * time.Hour)
	f := &gcptest.FakeSecretManager{Secrets: map[string]*gcptest.FakeSecret{fakePrefix + "gone": gone, fakePrefix + "cold": servedSecret("v")}}
	store := gcp.New(gcptest.Start(t, f), fakeKey)
	ctx := context.Background()
	before := f.Reads()
	for range 5 {
		if _, err := store.Read(ctx, fakePrefix+"gone"); !errors.Is(err, secretstore.ErrNotFound) {
			t.Fatalf("Read(gone) = %v, want ErrNotFound", err)
		}
	}
	if got := f.Reads() - before; got != 1 {
		t.Fatalf("five releases of a secret scheduled for deletion cost %d metadata reads, want 1", got)
	}
	before = f.Reads()
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if _, err := store.Read(ctx, fakePrefix+"cold"); err != nil {
				t.Error(err)
			}
		})
	}
	testwait.Returns(t, "the eight concurrent releases to return", wg.Wait)
	if got := f.Reads() - before; got != 1 {
		t.Fatalf("eight concurrent releases of a cold secret cost %d metadata reads, want 1", got)
	}
}

// TestGCPBoundsItsRetriesAndKeepsAKeyFailureAnError pins two failure answers. A metadata call that
// keeps answering UNAVAILABLE is made gcpMetadataAttempts times and then fails with its own code,
// and the pauses between those calls stay bounded: each is within the ceiling gax.Backoff draws
// it under, 200 ms doubling at each retry, and one call's pauses come to no more than failWithin,
// the 10 s within which a lasting outage must fail it. The row reads the pauses the store hands
// gax to wait out (gcp.RecordMetadataPausesForTest) rather than any clock, and waits none of them.
// gax draws each pause at random up to its ceiling, so the row reads the pauses of
// retriedDescribes calls rather than one call's two: a pause past its ceiling, such as one
// doubled, shows in at least one of them but for a chance of one in 2^40. A FAILED_PRECONDITION on
// an enabled version (an unusable key, say) is an error the broker logs, not a secret with no
// value.
//
// The named revert: double the pause boundedRetryer answers; a pause past its ceiling then fails
// this row.
func TestGCPBoundsItsRetriesAndKeepsAKeyFailureAnError(t *testing.T) {
	const (
		// retriedDescribes is how many Describes the row makes through the outage, each one
		// call's gcpMetadataAttempts GetSecrets and the pauses between them.
		retriedDescribes = 20
		// failWithin is the bound within which a call through a lasting UNAVAILABLE fails, which
		// its pauses together may not pass.
		failWithin = 10 * time.Second
		// firstCeiling and lastCeiling are the store's backoff: the first retry's pause is drawn up
		// to 200 ms, and each retry after it up to twice the one before, to at most 2 s.
		firstCeiling, lastCeiling = 200 * time.Millisecond, 2 * time.Second
	)
	broken := servedSecret("v")
	broken.KeyUnusable = true
	f := &gcptest.FakeSecretManager{
		Secrets:     map[string]*gcptest.FakeSecret{fakePrefix + "a": servedSecret("v"), fakePrefix + "broken": broken},
		Unavailable: map[string]int{"GetSecret": retriedDescribes * gcp.MetadataAttemptsForTest},
	}
	client := gcptest.Start(t, f)
	var pauses []time.Duration
	gcp.RecordMetadataPausesForTest(client, func(pause time.Duration) { pauses = append(pauses, pause) })
	store := gcp.New(client, fakeKey)
	ctx := testwait.Context(t)
	for describe := 1; describe <= retriedDescribes; describe++ {
		pauses = nil
		if _, err := store.Describe(ctx, fakePrefix+"a"); err == nil || !strings.Contains(err.Error(), "Unavailable") {
			t.Fatalf("Describe %d through a lasting UNAVAILABLE = %v; want Unavailable", describe, err)
		}
		if len(pauses) != gcp.MetadataAttemptsForTest-1 {
			t.Fatalf("Describe %d through a lasting UNAVAILABLE paused %d times (%v), want %d, one before each retry", describe, len(pauses), pauses, gcp.MetadataAttemptsForTest-1)
		}
		var waited time.Duration
		ceiling := firstCeiling
		for retry, pause := range pauses {
			if pause > ceiling {
				t.Fatalf("Describe %d paused %v before its retry %d, past that retry's ceiling of %v (its pauses: %v)", describe, pause, retry+1, ceiling, pauses)
			}
			waited += pause
			ceiling = min(2*ceiling, lastCeiling)
		}
		if waited > failWithin {
			t.Fatalf("Describe %d through a lasting UNAVAILABLE paused %v in all (%v), past the %v within which it must fail", describe, waited, pauses, failWithin)
		}
	}
	f.Mu.Lock()
	calls := f.Calls["GetSecret"]
	f.Unavailable = nil
	f.Mu.Unlock()
	if calls != retriedDescribes*gcp.MetadataAttemptsForTest {
		t.Fatalf("GetSecret calls = %d, want %d, %d for each of %d Describes", calls, retriedDescribes*gcp.MetadataAttemptsForTest, gcp.MetadataAttemptsForTest, retriedDescribes)
	}
	if _, err := store.Read(context.Background(), fakePrefix+"broken"); err == nil || errors.Is(err, secretstore.ErrNotFound) {
		t.Fatalf("Read of an enabled version that answers FAILED_PRECONDITION = %v; want a logged error, not ErrNotFound", err)
	}
}

// TestGCPReadDoesNotCacheAFailedCheck pins that a metadata check failing for another reason than
// not-found (a denied call, an outage past its retries) is not remembered: the next release
// checks again, so one caller's failure never refuses every release of the secret for 30 s.
func TestGCPReadDoesNotCacheAFailedCheck(t *testing.T) {
	f := &gcptest.FakeSecretManager{
		Secrets:     map[string]*gcptest.FakeSecret{fakePrefix + "a": servedSecret("v")},
		Unavailable: map[string]int{"GetSecret": gcp.MetadataAttemptsForTest},
	}
	store := gcp.New(gcptest.Start(t, f), fakeKey)
	ctx := context.Background()
	if _, err := store.Read(ctx, fakePrefix+"a"); err == nil || errors.Is(err, secretstore.ErrNotFound) {
		t.Fatalf("first Read through an outage = %v, want its error", err)
	}
	if v, err := store.Read(ctx, fakePrefix+"a"); err != nil || v != "v" {
		t.Fatalf("second Read = %q, %v; want a fresh check that succeeds", v, err)
	}
}

// TestGCPReadCachesADisabledVersionsVerdict pins that a secret whose latest version is disabled
// costs one GetSecretVersion per window however often it is released, as Read's other check does.
func TestGCPReadCachesADisabledVersionsVerdict(t *testing.T) {
	off := servedSecret("v")
	off.State = secretmanagerpb.SecretVersion_DISABLED
	f := &gcptest.FakeSecretManager{Secrets: map[string]*gcptest.FakeSecret{fakePrefix + "off": off}}
	store := gcp.New(gcptest.Start(t, f), fakeKey)
	for range 5 {
		if _, err := store.Read(context.Background(), fakePrefix+"off"); !errors.Is(err, secretstore.ErrNotFound) {
			t.Fatalf("Read(off) = %v, want ErrNotFound", err)
		}
	}
	if got := f.Calls["GetSecretVersion"]; got != 1 {
		t.Fatalf("five releases of a disabled secret made %d GetSecretVersion calls, want 1", got)
	}
}
