package onepassword_test

import (
	"context"
	"errors"
	"maps"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/secretstore"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/onepassword"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/onepassword/onepasswordtest"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/secretstoretest"
)

// testVault is the vault the store tests read: an id of 1Password's form, and no real vault's.
const testVault = "vault0000000000000000000aa"

// testPrefix is the title prefix the store tests list under.
const testPrefix = "cipher-"

// owned is the tags of a secret a person owns at the agent tier.
func owned() map[string]string {
	return map[string]string{"owner": "ada@example.com", "tier": "agent"}
}

// readTrustMutant names, in the variable of that name, the contract's mutant of the 1Password
// store: one whose value reads trust their kept items a minute longer than ReadLag documents.
const readTrustMutant = "ONEPASSWORD_READ_TRUST_MUTANT"

// TestOnePasswordRunsTheStoreContract runs the store contract on the 1Password store over the fake
// vault: each fixture is an API Credential item another writer made, its value the credential
// and its tags the cipher section's fields, an archived item for one scheduled for deletion. Every
// item of the vault is on the vault's own encryption, and no create leaves an item waiting for its
// value, so the rig holds neither an off-key nor an Unfilled secret. The store's reads answer the
// items it keeps for its ReadLag, which the rig's Pass lets go by on the clock the vault and the
// store share.
func TestOnePasswordRunsTheStoreContract(t *testing.T) {
	secretstoretest.StoreContract(t, "onepassword", func(t *testing.T) secretstoretest.StoreRig {
		at := &clock{now: time.Date(2026, 11, 23, 9, 0, 0, 0, time.UTC)}
		fake := &onepasswordtest.Fake{Now: at.Now}
		store := onepassword.New(fake, testVault, onepassword.Unlimited, at.Now)
		if os.Getenv(readTrustMutant) != "" {
			onepassword.SetReadLag(store, onepassword.ReadLag+time.Minute)
		}
		ids := map[string]string{}
		return secretstoretest.StoreRig{
			Store: store, Prefix: testPrefix, ReadLag: onepassword.ReadLag,
			Pass: func(_ *testing.T, d time.Duration) { at.Add(d) },
			Put: func(_ *testing.T, f secretstoretest.Fixture) {
				title := testPrefix + f.Slug
				if f.Outside {
					title = "x" + title
				}
				state := onepassword.StateActive
				if f.Deleted {
					state = onepassword.StateArchived
				}
				ids[f.Slug] = fake.Hold(onepassword.Item{ID: ids[f.Slug], Title: title, State: state, Credential: f.Value, Cipher: maps.Clone(f.Tags)}).ID
			},
			Remove: func(_ *testing.T, slug string) { fake.Remove(ids[slug]) },
		}
	})
}

// TestTheContractRefusesAReadTrustLongerThanReadLag: the contract's mutant 1Password store, whose
// value reads trust their kept items a minute past ReadLag, fails "Read of a source the store no
// longer holds is ErrNotFound", run in a child whose deadline cuts the case's wait to seconds.
func TestTheContractRefusesAReadTrustLongerThanReadLag(t *testing.T) {
	const failing = "TestOnePasswordRunsTheStoreContract/onepassword/Read_of_a_source_the_store_no_longer_holds_is_ErrNotFound"
	cmd := exec.Command(os.Args[0], "-test.run", "^TestOnePasswordRunsTheStoreContract$", "-test.count=1", "-test.timeout=40s")
	cmd.Env = append(os.Environ(), readTrustMutant+"=1")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("the contract passed a store whose reads trust their kept items past ReadLag; want it to fail %s\n%s", failing, out)
	}
	if !strings.Contains(string(out), "--- FAIL: "+failing) {
		t.Fatalf("the mutant's run does not fail %s\n%s", failing, out)
	}
}

// recordingClient is a Client that records the ids of each Get it passes on.
type recordingClient struct {
	onepassword.Client
	mu    sync.Mutex
	lists int
	gets  [][]string
}

func (c *recordingClient) List(ctx context.Context) ([]onepassword.Overview, error) {
	c.mu.Lock()
	c.lists++
	c.mu.Unlock()
	return c.Client.List(ctx)
}

func (c *recordingClient) Get(ctx context.Context, ids ...string) ([]onepassword.Item, error) {
	c.mu.Lock()
	c.gets = append(c.gets, slices.Clone(ids))
	c.mu.Unlock()
	return c.Client.Get(ctx, ids...)
}

// since is the Lists and the Gets' ids c has passed on after the counts lists and gets.
func (c *recordingClient) since(lists, gets int) (int, [][]string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lists - lists, slices.Clone(c.gets[gets:])
}

// mark is c's counts now, for since.
func (c *recordingClient) mark() (int, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lists, len(c.gets)
}

// list is store's List under testPrefix, failing t on an error, by ID.
func list(t *testing.T, store secretstore.Store, opts secretstore.ReadOptions) map[string]secretstore.Listing {
	t.Helper()
	listings, err := store.List(context.Background(), testPrefix, opts)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	byID := map[string]secretstore.Listing{}
	for _, ls := range listings {
		if _, twice := byID[ls.ID]; twice {
			t.Fatalf("List answered %s twice", ls.ID)
		}
		byID[ls.ID] = ls
	}
	return byID
}

// TestAListReadsOnlyChangedItems: a List reads in full, in one Get, only the items under the
// prefix whose UpdatedAt moved since the store last read them: the first List every one of them
// and none outside the prefix, the next after one item changed that item alone, and one after no
// change none.
func TestAListReadsOnlyChangedItems(t *testing.T) {
	fake := &onepasswordtest.Fake{}
	client := &recordingClient{Client: fake}
	store := onepassword.New(client, testVault, onepassword.Unlimited, nil)
	var under []string
	for _, slug := range []string{"deel-api-key", "other-key", "third-key"} {
		under = append(under, fake.Hold(onepassword.Item{Title: testPrefix + slug, Credential: "v", Cipher: owned()}).ID)
	}
	fake.Hold(onepassword.Item{Title: "someone-elses-login", Credential: "v"})

	lists, gets := client.mark()
	list(t, store, secretstore.ReadOptions{})
	if n, got := client.since(lists, gets); n != 1 || len(got) != 1 || !slices.Equal(slices.Sorted(slices.Values(got[0])), slices.Sorted(slices.Values(under))) {
		t.Fatalf("the first List made %d Lists and Gets %v; want one List and one Get of %v", n, got, under)
	}

	changed := fake.Hold(onepassword.Item{ID: under[1], Title: testPrefix + "other-key", Credential: "new", Cipher: owned()})
	lists, gets = client.mark()
	byID := list(t, store, secretstore.ReadOptions{})
	if n, got := client.since(lists, gets); n != 1 || len(got) != 1 || !slices.Equal(got[0], []string{changed.ID}) {
		t.Fatalf("the List after one change made %d Lists and Gets %v; want one List and one Get of %s", n, got, changed.ID)
	}
	if ls := byID[testPrefix+"other-key"]; ls.Version != "2" {
		t.Errorf("%s: Version %q after its change, want 2", ls.ID, ls.Version)
	}

	lists, gets = client.mark()
	list(t, store, secretstore.ReadOptions{})
	if n, got := client.since(lists, gets); n != 1 || len(got) != 0 {
		t.Fatalf("the List after no change made %d Lists and Gets %v; want one List and no Get", n, got)
	}
}

// TestTwoActiveItemsOfOneTitleAreMarked: two active items of one title are one listing whose
// Duplicates holds both items' sources, from List and from Describe alike; an archived item of the
// same title is not among them.
func TestTwoActiveItemsOfOneTitleAreMarked(t *testing.T) {
	fake := &onepasswordtest.Fake{}
	store := onepassword.New(fake, testVault, onepassword.Unlimited, nil)
	title := testPrefix + "deel-api-key"
	first := fake.Hold(onepassword.Item{Title: title, Credential: "one", Cipher: owned()})
	second := fake.Hold(onepassword.Item{Title: title, Credential: "two", Cipher: owned()})
	fake.Hold(onepassword.Item{Title: title, State: onepassword.StateArchived, Credential: "old", Cipher: owned()})
	fake.Hold(onepassword.Item{Title: testPrefix + "other-key", Credential: "v", Cipher: owned()})
	want := []string{onepassword.Source(testVault, first.ID), onepassword.Source(testVault, second.ID)}
	slices.Sort(want)

	byID := list(t, store, secretstore.ReadOptions{IncludeDeleted: true})
	if got := byID[title].Duplicates; !slices.Equal(got, want) {
		t.Errorf("List: %s's Duplicates %v, want %v", title, got, want)
	}
	if got := byID[testPrefix+"other-key"].Duplicates; got != nil {
		t.Errorf("List: a title one item holds has Duplicates %v", got)
	}
	described, err := onepassword.New(fake, testVault, onepassword.Unlimited, nil).Describe(context.Background(), title, secretstore.ReadOptions{})
	if err != nil {
		t.Fatalf("Describe(%s): %v", title, err)
	}
	if !slices.Equal(described.Duplicates, want) {
		t.Errorf("Describe(%s): Duplicates %v, want %v", title, described.Duplicates, want)
	}
}

// clock is a clock a test moves.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// TestAValueReadUsesTheKeptItem: a value read right after a List answers the kept item, with no
// call; one once that List is older than the store trusts it for lists the vault again and reads
// no item whose UpdatedAt has not moved; and one after the item changed reads that item.
func TestAValueReadUsesTheKeptItem(t *testing.T) {
	fake := &onepasswordtest.Fake{}
	client := &recordingClient{Client: fake}
	at := &clock{now: time.Date(2026, 11, 23, 9, 0, 0, 0, time.UTC)}
	store := onepassword.New(client, testVault, onepassword.Unlimited, at.Now)
	item := fake.Hold(onepassword.Item{Title: testPrefix + "deel-api-key", Credential: "first", Cipher: owned()})
	source := list(t, store, secretstore.ReadOptions{})[item.Title].Source
	ctx := context.Background()

	lists, gets := client.mark()
	if value, err := store.Read(ctx, source); err != nil || value != "first" {
		t.Fatalf("Read(%s) = %q, %v; want first", source, value, err)
	}
	if n, got := client.since(lists, gets); n != 0 || len(got) != 0 {
		t.Fatalf("a Read right after the List made %d Lists and Gets %v; want no call", n, got)
	}

	at.Add(time.Hour)
	lists, gets = client.mark()
	if value, err := store.Read(ctx, source); err != nil || value != "first" {
		t.Fatalf("Read(%s) = %q, %v; want first", source, value, err)
	}
	if n, got := client.since(lists, gets); n != 1 || len(got) != 0 {
		t.Fatalf("a Read an hour after the List made %d Lists and Gets %v; want one List and no Get", n, got)
	}

	fake.Hold(onepassword.Item{ID: item.ID, Title: item.Title, Credential: "second", Cipher: owned()})
	at.Add(time.Hour)
	lists, gets = client.mark()
	if value, err := store.Read(ctx, source); err != nil || value != "second" {
		t.Fatalf("Read(%s) after a change = %q, %v; want second", source, value, err)
	}
	if n, got := client.since(lists, gets); n != 1 || len(got) != 1 || !slices.Equal(got[0], []string{item.ID}) {
		t.Fatalf("a Read after a change made %d Lists and Gets %v; want one List and one Get of %s", n, got, item.ID)
	}

	fake.Remove(item.ID)
	at.Add(time.Hour)
	if _, err := store.Read(ctx, source); !errors.Is(err, secretstore.ErrNotFound) {
		t.Fatalf("Read(%s) of a removed item = %v, want ErrNotFound", source, err)
	}
}

// TestATitleOutsideTheSlugRuleIsNeverServed: the prefix match is byte for byte, so a title that
// differs from it only in case is not listed; a title under the prefix whose rest breaks the
// policy's slug rule - capitals, a trailing space - is listed and refused as name-malformed; and
// none of them is served as DEEL_API_KEY, whose reread answers absent.
func TestATitleOutsideTheSlugRuleIsNeverServed(t *testing.T) {
	fake := &onepasswordtest.Fake{}
	for _, title := range []string{"Cipher-Deel-Api-Key", "cipher-Deel-Api-Key", "cipher-deel-api-key "} {
		fake.Hold(onepassword.Item{Title: title, Credential: "v", Cipher: owned()})
	}
	loader := policy.Loader{Store: onepassword.New(fake, testVault, onepassword.Unlimited, nil), Prefix: testPrefix}
	ctx := context.Background()
	set, err := loader.Load(ctx)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(set.Secrets) != 0 {
		t.Errorf("Load served %v, want nothing", slices.Collect(maps.Keys(set.Secrets)))
	}
	if _, listed := set.Refused["Cipher-Deel-Api-Key"]; listed {
		t.Error("Load refused Cipher-Deel-Api-Key, a title outside the prefix it should never have listed")
	}
	for _, id := range []string{"cipher-Deel-Api-Key", "cipher-deel-api-key "} {
		if got := set.Refused[id].Reason; got != policy.ReasonNameMalformed {
			t.Errorf("Load refused %q for %q, want %s", id, got, policy.ReasonNameMalformed)
		}
	}
	lookup, err := loader.LoadOne(ctx, "DEEL_API_KEY")
	if err != nil || lookup.Served || lookup.Reason != policy.ReasonAbsent {
		t.Errorf("LoadOne(DEEL_API_KEY) = %+v, %v; want absent", lookup, err)
	}
}
