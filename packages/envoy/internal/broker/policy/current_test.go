package policy_test

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"

	"github.com/sjawhar/envoy/internal/broker/policy"
	"github.com/sjawhar/envoy/internal/broker/policy/policytest"
	"github.com/sjawhar/envoy/internal/broker/secrets"
	"github.com/sjawhar/envoy/internal/metrics"
)

// awsListLag is the AWS store's ListLag, which every Local-backed Current here runs on.
var awsListLag = policytest.Store(secrets.NewLocal()).ListLag()

// staleLister freezes ListSecrets at construction while DescribeSecret stays live: AWS's
// documented ListSecrets lag, reproduced.
type staleLister struct {
	*secrets.Local
	page *secretsmanager.ListSecretsOutput
}

func (s staleLister) ListSecrets(context.Context, *secretsmanager.ListSecretsInput, ...func(*secretsmanager.Options)) (*secretsmanager.ListSecretsOutput, error) {
	return s.page, nil
}

func TestRefreshKeepsWhatARecentRereadSettled(t *testing.T) {
	store := secrets.NewLocal(policytest.Secret("OLD_KEY", "ada@example.com", policy.TierAgent, "v1"))
	stale, err := store.ListSecrets(context.Background(), &secretsmanager.ListSecretsInput{})
	if err != nil {
		t.Fatal(err)
	}
	loader := policytest.Loader(store)
	loader.Store = policytest.StoreThrough(store, staleLister{Local: store, page: stale}) // the listing predates everything below
	cur, err := policy.NewCurrent(t.Context(), loader, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	store.Put(policytest.Secret("NEW_KEY", "ada@example.com", policy.TierAgent, "v1"))
	if lk, _ := cur.RefreshOne(context.Background(), "NEW_KEY"); !lk.Served {
		t.Fatal("reread must serve NEW_KEY")
	}
	store.Delete(policytest.ID("OLD_KEY"))
	if lk, _ := cur.RefreshOne(context.Background(), "OLD_KEY"); lk.Served {
		t.Fatal("reread must drop OLD_KEY")
	}
	if err := cur.Refresh(context.Background()); err != nil { // the stale listing still lists OLD_KEY and not NEW_KEY
		t.Fatal(err)
	}
	set := cur.Get()
	if _, ok := set.Secrets["NEW_KEY"]; !ok {
		t.Fatal("a full reload from a lagging listing dropped NEW_KEY")
	}
	if _, ok := set.Secrets["OLD_KEY"]; ok {
		t.Fatal("a full reload from a lagging listing resurrected deleted OLD_KEY")
	}
}

// TestRecentNamesExpireAfterListLag pins that the window is bounded: a reread outlives a lagging
// listing until listLag has passed since it, and after that the listing wins again.
func TestRecentNamesExpireAfterListLag(t *testing.T) {
	store := secrets.NewLocal()
	stale, err := store.ListSecrets(context.Background(), &secretsmanager.ListSecretsInput{})
	if err != nil {
		t.Fatal(err)
	}
	loader := policytest.Loader(store)
	loader.Store = policytest.StoreThrough(store, staleLister{Local: store, page: stale}) // never lists NEW_KEY
	cur, err := policy.NewCurrent(t.Context(), loader, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	reread := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	now := reread
	policy.SetNow(cur, func() time.Time { return now })
	store.Put(policytest.Secret("NEW_KEY", "ada@example.com", policy.TierAgent, "v1"))
	if lk, _ := cur.RefreshOne(context.Background(), "NEW_KEY"); !lk.Served {
		t.Fatal("reread must serve NEW_KEY")
	}

	now = reread.Add(awsListLag - time.Second)
	if err := cur.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := cur.Get().Secrets["NEW_KEY"]; !ok {
		t.Fatal("a full reload inside listLag of the reread dropped NEW_KEY")
	}

	now = reread.Add(awsListLag + time.Second)
	if err := cur.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := cur.Get().Secrets["NEW_KEY"]; ok {
		t.Fatal("a full reload listLag after the reread still kept NEW_KEY over the listing")
	}
}

// slowStore is the AWS store with a longer documented lag, as Secret Manager's six hours.
type slowStore struct{ secrets.AWS }

func (slowStore) ListLag() time.Duration { return 6 * time.Hour }

// TestAnchorsLastTheStoresOwnLag pins that the window is the store's: on a store whose metadata
// may lag a write for hours, a reread that stopped serving a secret still holds past AWS's five
// minutes against a listing that has not caught up, so a stale listing cannot reinstate it.
func TestAnchorsLastTheStoresOwnLag(t *testing.T) {
	store := secrets.NewLocal(policytest.Secret("OLD_KEY", "ada@example.com", policy.TierAgent, "v1"))
	stale, err := store.ListSecrets(context.Background(), &secretsmanager.ListSecretsInput{})
	if err != nil {
		t.Fatal(err)
	}
	loader := policytest.Loader(store)
	loader.Store = slowStore{policytest.StoreThrough(store, staleLister{Local: store, page: stale})} // still lists OLD_KEY as agent tier
	cur, err := policy.NewCurrent(t.Context(), loader, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	reread := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	now := reread
	policy.SetNow(cur, func() time.Time { return now })
	store.Put(policytest.Secret("OLD_KEY", "ada@example.com", policy.TierHuman, "v1"))
	if lk, _ := cur.RefreshOne(context.Background(), "OLD_KEY"); lk.Secret.Tier != policy.TierHuman {
		t.Fatalf("reread = %+v; want OLD_KEY human tier", lk)
	}
	now = reread.Add(time.Hour)
	if err := cur.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := cur.Get().Secrets["OLD_KEY"].Tier; got != policy.TierHuman {
		t.Fatalf("a reload an hour after the reread, inside the store's lag, served OLD_KEY as %s from the stale listing", got)
	}
}

// slowLister answers a frozen page, then runs fetched: a reload whose listing is fetched at one
// time and whose Load returns at a later one.
type slowLister struct {
	*secrets.Local
	page    *secretsmanager.ListSecretsOutput
	fetched func()
}

func (s *slowLister) ListSecrets(context.Context, *secretsmanager.ListSecretsInput, ...func(*secretsmanager.Options)) (*secretsmanager.ListSecretsOutput, error) {
	if s.fetched != nil {
		s.fetched()
	}
	return s.page, nil
}

// TestARefreshStraddlingListLagStillRereadsTheName pins that the window is measured from when the
// listing was fetched: a reload whose lagging listing was read inside listLag of a reread, but
// whose Load returns after it ran out, still rereads that name rather than trusting the listing.
func TestARefreshStraddlingListLagStillRereadsTheName(t *testing.T) {
	store := secrets.NewLocal(policytest.Secret("OLD_KEY", "ada@example.com", policy.TierAgent, "v1"))
	stale, err := store.ListSecrets(context.Background(), &secretsmanager.ListSecretsInput{})
	if err != nil {
		t.Fatal(err)
	}
	lister := &slowLister{Local: store, page: stale} // lists OLD_KEY and never NEW_KEY
	loader := policytest.Loader(store)
	loader.Store = policytest.StoreThrough(store, lister)
	cur, err := policy.NewCurrent(t.Context(), loader, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	reread := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	now := reread
	policy.SetNow(cur, func() time.Time { return now })
	store.Put(policytest.Secret("NEW_KEY", "ada@example.com", policy.TierAgent, "v1"))
	if lk, _ := cur.RefreshOne(context.Background(), "NEW_KEY"); !lk.Served {
		t.Fatal("reread must serve NEW_KEY")
	}
	store.Delete(policytest.ID("OLD_KEY"))
	if lk, _ := cur.RefreshOne(context.Background(), "OLD_KEY"); lk.Served {
		t.Fatal("reread must drop OLD_KEY")
	}

	// The listing is fetched inside listLag of the rereads, and Load returns after it ran out.
	now = reread.Add(awsListLag - time.Second)
	lister.fetched = func() { now = reread.Add(awsListLag + time.Second) }
	if err := cur.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	set := cur.Get()
	if _, ok := set.Secrets["NEW_KEY"]; !ok {
		t.Fatal("a reload whose listing predates listLag's end dropped NEW_KEY")
	}
	if _, ok := set.Secrets["OLD_KEY"]; ok {
		t.Fatal("a reload whose listing predates listLag's end resurrected deleted OLD_KEY")
	}
}

// TestAReloadThatCatchesAChangeKeepsItForListLag pins that a reload which, rereading a recent
// name, finds an answer the live policy did not hold re-anchors that name: a secret deleted after
// a reread changed its answer, with no reread of its own, is dropped by the next reload's
// re-describe, and a reload listLag after the reread, from a listing that still predates the
// delete, keeps it out rather than serving it again.
func TestAReloadThatCatchesAChangeKeepsItForListLag(t *testing.T) {
	store := secrets.NewLocal(policytest.Secret("OLD_KEY", "ada@example.com", policy.TierAgent, "v1"))
	stale, err := store.ListSecrets(context.Background(), &secretsmanager.ListSecretsInput{})
	if err != nil {
		t.Fatal(err)
	}
	lister := &slowLister{Local: store, page: stale} // lists OLD_KEY, agent tier, throughout
	loader := policytest.Loader(store)
	loader.Store = policytest.StoreThrough(store, lister)
	cur, err := policy.NewCurrent(t.Context(), loader, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	reread := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	now := reread
	policy.SetNow(cur, func() time.Time { return now })
	store.Put(policytest.Secret("OLD_KEY", "ada@example.com", policy.TierHuman, "v1"))
	if lk, _ := cur.RefreshOne(context.Background(), "OLD_KEY"); lk.Secret.Tier != policy.TierHuman {
		t.Fatalf("reread = %+v; want OLD_KEY served at human tier", lk)
	}

	// Deleted (from the console, say) while the next reload lists: its listing still shows OLD_KEY,
	// and its own re-describe finds it gone.
	now = reread.Add(awsListLag / 2)
	lister.fetched = func() { store.Delete(policytest.ID("OLD_KEY")) }
	if err := cur.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := cur.Get().Secrets["OLD_KEY"]; ok {
		t.Fatal("the reload whose own re-describe found OLD_KEY deleted still served it")
	}

	lister.fetched = nil
	now = reread.Add(awsListLag + time.Second)
	if err := cur.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := cur.Get().Secrets["OLD_KEY"]; ok {
		t.Fatal("a reload listLag after the reread, but not after the reload that found OLD_KEY deleted, served it again from a lagging listing")
	}
}

// TestARereadOfANameThatExistsNowhereCostsNoReload pins that a reread finding absent a name the
// live policy did not serve is not kept for the next reload: anyone may ask for a reread, so
// otherwise every name a caller invents would cost each reload in the next listLag one more
// DescribeSecret under the writer lock. TestRefreshKeepsWhatARecentRereadSettled pins the other
// side: a served name a reread finds gone is still kept, so a lagging listing cannot bring it back.
func TestARereadOfANameThatExistsNowhereCostsNoReload(t *testing.T) {
	store := secrets.NewLocal(policytest.Secret("OLD_KEY", "ada@example.com", policy.TierAgent, "v1"))
	count := &policytest.CountingDescriber{SecretsManagerAPI: store}
	loader := policytest.Loader(store)
	loader.Store = policytest.StoreThrough(store, count)
	cur, err := policy.NewCurrent(t.Context(), loader, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if lk, err := cur.RefreshOne(context.Background(), "NO_SUCH_KEY"); err != nil || lk.Served || lk.Reason != policy.ReasonAbsent {
		t.Fatalf("RefreshOne(NO_SUCH_KEY) = %+v, %v; want absent", lk, err)
	}
	before := count.Calls.Load()
	if err := cur.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := count.Calls.Load() - before; got != 0 {
		t.Fatalf("the reload after a reread of a name that exists nowhere made %d DescribeSecret calls; want 0", got)
	}
}

// TestARereadThatConfirmsTheLiveAnswerCostsNoReload pins that a reread finding a served secret as
// the live policy already serves it records nothing: anyone may reread an existing name, and on a
// store whose ListLag is hours each anchor would cost every reload in it a describe.
func TestARereadThatConfirmsTheLiveAnswerCostsNoReload(t *testing.T) {
	store := secrets.NewLocal(policytest.Secret("OLD_KEY", "ada@example.com", policy.TierAgent, "v1"))
	count := &policytest.CountingDescriber{SecretsManagerAPI: store}
	loader := policytest.Loader(store)
	loader.Store = policytest.StoreThrough(store, count)
	cur, err := policy.NewCurrent(t.Context(), loader, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if lk, err := cur.RefreshOne(context.Background(), "OLD_KEY"); err != nil || !lk.Served {
			t.Fatalf("RefreshOne(OLD_KEY) = %+v, %v; want served", lk, err)
		}
	}
	before := count.Calls.Load()
	if err := cur.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := count.Calls.Load() - before; got != 0 {
		t.Fatalf("the reload after rereads that confirmed the live answer made %d DescribeSecret calls; want 0", got)
	}
}

// TestARereadThatConfirmsAMaxGrantSecretCostsNoReload is the same for a secret with a max-grant
// tag: a Secret compares equal to itself by value, so a reread that finds it unchanged anchors
// nothing and the next reload describes nothing.
func TestARereadThatConfirmsAMaxGrantSecretCostsNoReload(t *testing.T) {
	limited := policytest.KindSecret("LIMITED_KEY", "ada@example.com", policy.TierAgent, policy.KindGoogleLogin, "v1")
	limited.Tags[policy.TagMaxGrant] = "1h"
	store := secrets.NewLocal(limited)
	count := &policytest.CountingDescriber{SecretsManagerAPI: store}
	loader := policytest.Loader(store)
	loader.Store = policytest.StoreThrough(store, count)
	cur, err := policy.NewCurrent(t.Context(), loader, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if lk, err := cur.RefreshOne(context.Background(), "LIMITED_KEY"); err != nil || !lk.Served {
			t.Fatalf("RefreshOne(LIMITED_KEY) = %+v, %v; want served", lk, err)
		}
	}
	before := count.Calls.Load()
	if err := cur.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := count.Calls.Load() - before; got != 0 {
		t.Fatalf("the reload after rereads that confirmed a max-grant secret made %d DescribeSecret calls; want 0", got)
	}
}

// heldCurrent loads an empty namespace's policy and starts a reload whose listing waits until the
// test ends, returning once that reload holds the writer lock: a reload stuck on a slow Secrets
// Manager.
func heldCurrent(t *testing.T) *policy.Current {
	t.Helper()
	store := secrets.NewLocal()
	page, err := store.ListSecrets(context.Background(), &secretsmanager.ListSecretsInput{})
	if err != nil {
		t.Fatal(err)
	}
	lister := &slowLister{Local: store, page: page}
	loader := policytest.Loader(store)
	loader.Store = policytest.StoreThrough(store, lister)
	cur, err := policy.NewCurrent(t.Context(), loader, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	listing, release := make(chan struct{}), make(chan struct{})
	lister.fetched = func() { close(listing); <-release }
	refreshed := make(chan error, 1)
	go func() { refreshed <- cur.Refresh(context.Background()) }()
	<-listing
	t.Cleanup(func() { close(release); <-refreshed })
	return cur
}

// rereadWithin runs RefreshOne(ctx, name) and answers its error, failing t when it has not
// returned within a second: long enough for any answer that waits on nothing, far shorter than a
// held reload.
func rereadWithin(t *testing.T, cur *policy.Current, ctx context.Context, name string) error {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		_, err := cur.RefreshOne(ctx, name)
		done <- err
	}()
	select {
	case err := <-done:
		return err
	case <-time.After(time.Second):
		t.Fatalf("RefreshOne(%s) waited on the writer lock a reload holds", name)
		return nil
	}
}

// TestARereadOfANameNoSecretCanCarryWaitsOnNoReload pins that RefreshOne refuses such a name
// before it takes the writer lock: free text, or a name past Secrets Manager's name limit, is
// answered ErrNameInvalid at once while a reload holds the lock.
func TestARereadOfANameNoSecretCanCarryWaitsOnNoReload(t *testing.T) {
	cur := heldCurrent(t)
	for _, name := range []string{"not a secret name", "A" + strings.Repeat("B", 512-len(policytest.Prefix))} {
		if err := rereadWithin(t, cur, context.Background(), name); !errors.Is(err, policy.ErrNameInvalid) {
			t.Fatalf("RefreshOne(%.20s...) = %v, want ErrNameInvalid", name, err)
		}
	}
}

// TestARereadWaitsForTheWriterLockOnlyWhileItsCallerDoes pins that RefreshOne's wait for the lock
// a reload holds ends with its caller's context: one already done returns its error without
// waiting, and one whose deadline passes while it waits returns then.
func TestARereadWaitsForTheWriterLockOnlyWhileItsCallerDoes(t *testing.T) {
	cur := heldCurrent(t)
	done, cancel := context.WithCancel(context.Background())
	cancel()
	if err := rereadWithin(t, cur, done, "NEW_KEY"); !errors.Is(err, context.Canceled) {
		t.Fatalf("RefreshOne with its context done = %v, want context.Canceled", err)
	}
	expiring, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := rereadWithin(t, cur, expiring, "NEW_KEY"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("RefreshOne whose deadline passed while it waited = %v, want context.DeadlineExceeded", err)
	}
}

// hangingLister answers page until hang; from then on it holds each call until unhang releases
// every held call with page, or until the call's context ends and it answers that context's error,
// as the SDK does: a Secrets Manager that stops answering, then answers again. heldCalls counts the
// calls it has held. hang and unhang pair: a repeated hang keeps holding, and an unhang with
// nothing held does nothing.
type hangingLister struct {
	*secrets.Local
	page      *secretsmanager.ListSecretsOutput
	heldCalls atomic.Int64
	mu        sync.Mutex
	held      chan struct{} // closed by unhang; nil while it answers
}

func (h *hangingLister) hang() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.held == nil {
		h.held = make(chan struct{})
	}
}

func (h *hangingLister) unhang() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.held != nil {
		close(h.held)
		h.held = nil
	}
}

func (h *hangingLister) ListSecrets(ctx context.Context, _ *secretsmanager.ListSecretsInput, _ ...func(*secretsmanager.Options)) (*secretsmanager.ListSecretsOutput, error) {
	h.mu.Lock()
	held := h.held
	h.mu.Unlock()
	if held != nil {
		h.heldCalls.Add(1)
		select {
		case <-held:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return h.page, nil
}

// hangingLoader is the loader of an empty namespace whose listing lister answers.
func hangingLoader(t *testing.T) (*hangingLister, policy.Loader) {
	t.Helper()
	store := secrets.NewLocal()
	page, err := store.ListSecrets(context.Background(), &secretsmanager.ListSecretsInput{})
	if err != nil {
		t.Fatal(err)
	}
	lister := &hangingLister{Local: store, page: page}
	loader := policytest.Loader(store)
	loader.Store = policytest.StoreThrough(store, lister)
	return lister, loader
}

// TestAReloadGivesUpWithinItsInterval pins that a periodic reload whose Secrets Manager stops
// answering fails once its interval has passed, logging LoadFailedMessage, rather than holding the
// writer lock every reread waits on for as long as the process lives. It runs in a synctest
// bubble, which waits for the reload goroutine to return, so none of its lines reach a later
// test's capture.
func TestAReloadGivesUpWithinItsInterval(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		lister, loader := hangingLoader(t)
		logged := policytest.CaptureLog(t)
		cur, err := policy.NewCurrent(t.Context(), loader, 50*time.Millisecond)
		if err != nil {
			t.Fatal(err)
		}
		lister.hang()
		const failed = "ERROR " + policy.LoadFailedMessage + " error=\"list secrets under " + policytest.Prefix + ": context deadline exceeded\"\n"
		await(t, func() bool { return strings.Contains(logged.String(), failed) })
		lister.unhang()
		if err := rereadWithin(t, cur, context.Background(), "NEW_KEY"); err != nil {
			t.Fatalf("RefreshOne after the hung reload gave up = %v", err)
		}
	})
}

// TestAReloadItsShutdownEndsLogsNothing pins that a reload cut short because NewCurrent's context
// ended, the broker shutting down, is no failed load: LoadFailedMessage says a reload failed while
// the broker was serving, so a reload in flight when that context ends logs nothing.
func TestAReloadItsShutdownEndsLogsNothing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		lister, loader := hangingLoader(t)
		logged := policytest.CaptureLog(t)
		ctx, shutdown := context.WithCancel(t.Context())
		if _, err := policy.NewCurrent(ctx, loader, time.Minute); err != nil {
			t.Fatal(err)
		}
		lister.hang()
		time.Sleep(time.Minute + time.Second)
		synctest.Wait()
		if held := lister.heldCalls.Load(); held != 1 {
			t.Fatalf("%d reloads held on the listing a minute in, want the first", held)
		}
		shutdown()
		synctest.Wait() // the reload has returned, and its goroutine with it
		if logged.String() != "" {
			t.Fatalf("a reload the shutdown ended logged:\n%s", logged)
		}
	})
}

// refusedLine is the exact RefusedMessage line Current writes, as CaptureLog reads it, for the
// secret whose whole name in the store is id: refused for reason, its owner tag owner, first_load
// on the pod's first load, and was the reason before on a change of reason ("" for none).
func refusedLine(id, reason, owner string, firstLoad bool, was string) string {
	line := "ERROR " + policy.RefusedMessage + " name=" + id + " reason=" + reason + " owner=" + textValue(owner) + " first_load=" + strconv.FormatBool(firstLoad)
	if was != "" {
		line += " was=" + was
	}
	return line + "\n"
}

// servableLine is the exact ServableMessage line Current writes, as CaptureLog reads it, for the
// secret whose whole name in the store is id, which was refused for reason with owner: now is
// servable or absent.
func servableLine(id, reason, owner, now string) string {
	return "INFO " + policy.ServableMessage + " name=" + id + " reason=" + reason + " owner=" + textValue(owner) + " now=" + now + "\n"
}

// textValue is a fixture's value as slog's text output writes it: as it is, and quoted when it is
// empty. No fixture value holds a space, a quote or an equals sign, which it would quote too.
func textValue(value string) string {
	if value == "" {
		return `""`
	}
	return value
}

// unservable is the broker_unservable_secrets sample of the secret whose whole name in the store
// is id, refused for reason with owner, as the scrape writes it.
func unservable(id, reason, owner string) string {
	return `broker_unservable_secrets{secret="` + id + `",reason="` + reason + `",owner="` + owner + `"} 1` + "\n"
}

// TestThePolicyWritesAChangeOfASecretsStateNotEachLoad pins the rule the policy's lines follow: a
// line marks a change of a secret's state, never a load. The pod's first load writes one
// RefusedMessage line for each secret it refuses, marked first_load, the owner empty for the two
// reasons that are the owner tag's own; a load, a boot's catch-up load or a reread that finds what
// the live policy holds writes nothing; a failed load keeps the live set and writes nothing; a
// change of reason writes one line naming the reason before; a secret that gains a value, and one
// that leaves the store, writes one ServableMessage line, now=servable and now=absent; and a
// reread keeps the same set under the same rule. broker_unservable_secrets holds one series for
// each refused secret after every step, its owner the one the store holds now, and a reread of a
// name the store does not hold adds none. A refusal moves no Version. The steps call Refresh and
// RefreshOne themselves, in a synctest bubble whose clock moves only when they wait, so the
// hour's reload NewCurrent starts never runs between them and its goroutine is gone when the
// bubble ends.
//
// The named revert: write a line for every refused secret at every publish, not only for one
// whose state changed; the catch-up load's step then fails.
func TestThePolicyWritesAChangeOfASecretsStateNotEachLoad(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		noOwner := policytest.Secret("NO_OWNER_KEY", owner, policy.TierAgent, "v")
		delete(noOwner.Tags, policy.TagOwner)
		store := &failingSecrets{Local: secrets.NewLocal(
			policytest.Secret("SERVED_KEY", owner, policy.TierAgent, "v"),
			policytest.Secret("NO_VALUE_KEY", owner, policy.TierAgent, ""),
			policytest.Secret("BAD_OWNER_KEY", "sjawhar", policy.TierAgent, "v"),
			policytest.Secret("GOING_KEY", owner, "admin", "v"),
			noOwner,
		)}
		loader := policy.Loader{Store: policytest.StoreThrough(store.Local, store), Prefix: policytest.Prefix}
		logged := policytest.CaptureLog(t)
		cur, err := policy.NewCurrent(t.Context(), loader, time.Hour)
		if err != nil {
			t.Fatalf("NewCurrent: %v", err)
		}
		registry := metrics.New()
		policy.NewUnservableSecrets(registry, cur)
		ctx := context.Background()
		refresh := func() error { return cur.Refresh(ctx) }
		reread := func(name string) func() error {
			return func() error { _, err := cur.RefreshOne(ctx, name); return err }
		}
		badOwner, going, noOwnerID, noValue, newKey := policytest.ID("BAD_OWNER_KEY"), policytest.ID("GOING_KEY"), policytest.ID("NO_OWNER_KEY"), policytest.ID("NO_VALUE_KEY"), policytest.ID("NEW_KEY")

		written, version := 0, cur.Get().Version
		for _, step := range []struct {
			name string
			// act is the step; nil for the first load, which NewCurrent took above.
			act func() error
			// lines is every line the step writes, in order.
			lines string
			// series is every sample of broker_unservable_secrets after it, in any order.
			series []string
			// moves is whether the step moves the policy's Version.
			moves bool
		}{
			{name: "the pod's first load", lines: refusedLine(badOwner, policy.ReasonOwnerTagMalformed, "", true, "") +
				refusedLine(going, policy.ReasonTierTagMalformed, owner, true, "") +
				refusedLine(noOwnerID, policy.ReasonOwnerTagMissing, "", true, "") +
				refusedLine(noValue, policy.ReasonNoCurrentValue, owner, true, ""),
				series: []string{
					unservable(badOwner, policy.ReasonOwnerTagMalformed, ""), unservable(going, policy.ReasonTierTagMalformed, owner),
					unservable(noOwnerID, policy.ReasonOwnerTagMissing, ""), unservable(noValue, policy.ReasonNoCurrentValue, owner),
				}},
			{name: "the boot's catch-up load over the same store", act: refresh,
				series: []string{
					unservable(badOwner, policy.ReasonOwnerTagMalformed, ""), unservable(going, policy.ReasonTierTagMalformed, owner),
					unservable(noOwnerID, policy.ReasonOwnerTagMissing, ""), unservable(noValue, policy.ReasonNoCurrentValue, owner),
				}},
			{name: "a reread of a refused secret that has not changed", act: reread("NO_VALUE_KEY"),
				series: []string{
					unservable(badOwner, policy.ReasonOwnerTagMalformed, ""), unservable(going, policy.ReasonTierTagMalformed, owner),
					unservable(noOwnerID, policy.ReasonOwnerTagMissing, ""), unservable(noValue, policy.ReasonNoCurrentValue, owner),
				}},
			{name: "a reread of a name the store does not hold", act: reread("NO_SUCH_KEY"),
				series: []string{
					unservable(badOwner, policy.ReasonOwnerTagMalformed, ""), unservable(going, policy.ReasonTierTagMalformed, owner),
					unservable(noOwnerID, policy.ReasonOwnerTagMissing, ""), unservable(noValue, policy.ReasonNoCurrentValue, owner),
				}},
			{name: "a load the store fails", act: func() error {
				live := cur.Get()
				store.mu.Lock()
				store.failing = true
				store.mu.Unlock()
				defer func() {
					store.mu.Lock()
					store.failing = false
					store.mu.Unlock()
				}()
				if err := cur.Refresh(ctx); err == nil {
					return errors.New("Refresh with the listing failing = nil error, want its failure")
				}
				if cur.Get() != live {
					return errors.New("a failed load replaced the live set")
				}
				return nil
			}, series: []string{
				unservable(badOwner, policy.ReasonOwnerTagMalformed, ""), unservable(going, policy.ReasonTierTagMalformed, owner),
				unservable(noOwnerID, policy.ReasonOwnerTagMissing, ""), unservable(noValue, policy.ReasonNoCurrentValue, owner),
			}},
			{name: "a refusal whose reason changes", act: func() error {
				store.Put(policytest.Secret("NO_OWNER_KEY", owner, "admin", "v"))
				return refresh()
			}, lines: refusedLine(noOwnerID, policy.ReasonTierTagMalformed, owner, false, policy.ReasonOwnerTagMissing),
				series: []string{
					unservable(badOwner, policy.ReasonOwnerTagMalformed, ""), unservable(going, policy.ReasonTierTagMalformed, owner),
					unservable(noOwnerID, policy.ReasonTierTagMalformed, owner), unservable(noValue, policy.ReasonNoCurrentValue, owner),
				}},
			{name: "a secret that gains a value", act: func() error {
				store.Put(policytest.Secret("NO_VALUE_KEY", owner, policy.TierAgent, "v1"))
				return refresh()
			}, lines: servableLine(noValue, policy.ReasonNoCurrentValue, owner, "servable"), moves: true,
				series: []string{
					unservable(badOwner, policy.ReasonOwnerTagMalformed, ""), unservable(going, policy.ReasonTierTagMalformed, owner),
					unservable(noOwnerID, policy.ReasonTierTagMalformed, owner),
				}},
			{name: "a refused secret that leaves the store", act: func() error {
				store.Delete(going)
				return refresh()
			}, lines: servableLine(going, policy.ReasonTierTagMalformed, owner, "absent"),
				series: []string{unservable(badOwner, policy.ReasonOwnerTagMalformed, ""), unservable(noOwnerID, policy.ReasonTierTagMalformed, owner)}},
			{name: "a reread that finds a new secret refused", act: func() error {
				store.Put(policytest.Secret("NEW_KEY", owner, policy.TierAgent, ""))
				return reread("NEW_KEY")()
			}, lines: refusedLine(newKey, policy.ReasonNoCurrentValue, owner, false, ""),
				series: []string{
					unservable(badOwner, policy.ReasonOwnerTagMalformed, ""), unservable(noOwnerID, policy.ReasonTierTagMalformed, owner),
					unservable(newKey, policy.ReasonNoCurrentValue, owner),
				}},
			{name: "the load after that reread", act: refresh,
				series: []string{
					unservable(badOwner, policy.ReasonOwnerTagMalformed, ""), unservable(noOwnerID, policy.ReasonTierTagMalformed, owner),
					unservable(newKey, policy.ReasonNoCurrentValue, owner),
				}},
			{name: "an owner tag that changes under the same reason", act: func() error {
				store.Put(policytest.Secret("NEW_KEY", other, policy.TierAgent, ""))
				return refresh()
			}, series: []string{
				unservable(badOwner, policy.ReasonOwnerTagMalformed, ""), unservable(noOwnerID, policy.ReasonTierTagMalformed, owner),
				unservable(newKey, policy.ReasonNoCurrentValue, other),
			}},
			{name: "a reread that finds a refused secret servable", act: func() error {
				store.Put(policytest.Secret("BAD_OWNER_KEY", owner, policy.TierAgent, "v"))
				return reread("BAD_OWNER_KEY")()
			}, lines: servableLine(badOwner, policy.ReasonOwnerTagMalformed, "", "servable"), moves: true,
				series: []string{unservable(noOwnerID, policy.ReasonTierTagMalformed, owner), unservable(newKey, policy.ReasonNoCurrentValue, other)}},
		} {
			if step.act != nil {
				if err := step.act(); err != nil {
					t.Fatalf("%s: %v", step.name, err)
				}
			}
			if got := logged.String()[written:]; got != step.lines {
				t.Fatalf("%s logged:\n%s\nwant:\n%s", step.name, got, step.lines)
			}
			written = len(logged.String())
			slices.Sort(step.series)
			if got, want := samplesOf(t, registry, "broker_unservable_secrets"), strings.Join(step.series, ""); got != want {
				t.Fatalf("after %s broker_unservable_secrets reads\n%s\nwant one series for each refused secret\n%s", step.name, got, want)
			}
			if moved := cur.Get().Version != version; moved != step.moves {
				t.Fatalf("%s moved the policy version: %t, want %t", step.name, moved, step.moves)
			}
			version = cur.Get().Version
		}
	})
}

// TestAFailingReloadLogsOnceUntilItLoadsAgain pins the reload NewCurrent runs to the same rule: a
// run of reloads that fail writes LoadFailedMessage once, at its first failure, and the reload that
// loads again writes LoadRecoveredMessage once, after which a reload of a store that answers writes
// nothing. The store-read counter shows each reload was made. It runs on the synctest reload
// shape of TestAReloadItsShutdownEndsLogsNothing, whose bubble moves the clock reload by reload.
//
// The named revert: drop the failing flag from the reload loop, logging every failed reload; the
// second failure's line then fails this test.
func TestAFailingReloadLogsOnceUntilItLoadsAgain(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := &failingSecrets{Local: secrets.NewLocal(policytest.Secret("DEEL_API_KEY", owner, policy.TierAgent, "v"))}
		registry := metrics.New()
		loader := policy.Loader{Store: policytest.StoreThrough(store.Local, store), Prefix: policytest.Prefix, StoreReads: policy.NewStoreReads(registry)}
		logged := policytest.CaptureLog(t)
		if _, err := policy.NewCurrent(t.Context(), loader, time.Minute); err != nil {
			t.Fatal(err)
		}
		setFailing := func(failing bool) {
			store.mu.Lock()
			store.failing = failing
			store.mu.Unlock()
		}
		const (
			failed    = "ERROR " + policy.LoadFailedMessage + " error=\"list secrets under " + policytest.Prefix + ": ThrottlingException: Rate exceeded\"\n"
			recovered = "INFO " + policy.LoadRecoveredMessage + "\n"
		)
		for _, step := range []struct {
			name    string
			failing bool
			// wait is how long the step lets the clock run: the reloads at one minute and at two
			// for the first, the next one for each after it.
			wait   time.Duration
			lists  int
			logged string
		}{
			{"two reloads that fail", true, 2*time.Minute + time.Second, 3, failed},
			{"the reload that loads again", false, time.Minute, 4, failed + recovered},
			{"a reload after it", false, time.Minute, 5, failed + recovered},
		} {
			setFailing(step.failing)
			time.Sleep(step.wait)
			synctest.Wait()
			if got, want := samplesOf(t, registry, "broker_policy_store_reads_total"), `broker_policy_store_reads_total{read="list"} `+strconv.Itoa(step.lists)+"\n"; got != want {
				t.Fatalf("after %s the store reads are\n%s\nwant\n%s", step.name, got, want)
			}
			if logged.String() != step.logged {
				t.Fatalf("after %s the reload logged:\n%s\nwant:\n%s", step.name, logged, step.logged)
			}
		}
	})
}
