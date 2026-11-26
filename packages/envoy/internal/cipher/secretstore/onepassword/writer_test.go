package onepassword_test

import (
	"context"
	"errors"
	"maps"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/secretstore"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/onepassword"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/onepassword/onepasswordtest"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/secretstoretest"
)

// testSettings are a broker's settings on the 1Password store over testVault, under testPrefix,
// from an address that prints its fields.
func testSettings() secretstore.Settings {
	return secretstore.Settings{
		Store: onepassword.Name, Prefix: testPrefix,
		Fields: map[string]string{"onepassword_vault": testVault},
		Words:  func(s string) string { return s },
	}
}

// signedInAs makes every writer OpenWriter opens for the rest of t run op over client, signed in
// as account, with neither OP_SERVICE_ACCOUNT_TOKEN nor OP_ACCOUNT set; it answers how many
// times the SDK was started.
func signedInAs(t *testing.T, client onepassword.Client, account onepassword.OpAccount) *atomic.Int32 {
	t.Helper()
	t.Setenv("OP_SERVICE_ACCOUNT_TOKEN", "")
	t.Setenv("OP_ACCOUNT", "")
	var started atomic.Int32
	t.Cleanup(onepassword.SwapHooks(onepassword.Hooks{
		SDKClient: func(context.Context, string, string) (onepassword.Client, error) {
			started.Add(1)
			return client, nil
		},
		OpClient: func(string, secretstore.RunChild) onepassword.Client { return client },
		Whoami: func(context.Context, secretstore.RunChild) (onepassword.OpAccount, error) {
			return account, nil
		},
	}))
	return &started
}

// ada is a person op is signed in as.
var ada = onepassword.OpAccount{Email: "Ada@Example.com"}

// openWriter opens the writer of testSettings, failing t on an error.
func openWriter(t *testing.T) secretstore.Writer {
	t.Helper()
	w, err := onepassword.Backend{}.OpenWriter(context.Background(), testSettings(), secretstore.WriterOptions{})
	if err != nil {
		t.Fatalf("OpenWriter: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })
	return w
}

// fakeCalls are the fake's calls each writer contract call names.
var fakeCalls = map[string]string{
	"create": onepasswordtest.CallCreate, "set": onepasswordtest.CallPut, "tags": onepasswordtest.CallPut,
	"delete": onepasswordtest.CallArchive,
}

// TestOnePasswordRunsTheWriterContract (c-op-contract, the writer half) runs the writer contract
// on the writer OpenWriter opens, over the fake vault, signed in as a person through op.
// Restore has no call on 1Password, so "restore" names no call of the fake: the contract's
// restore case meets ErrNoRestore, which keeps the archived item.
func TestOnePasswordRunsTheWriterContract(t *testing.T) {
	secretstoretest.WriterContract(t, "onepassword", func(t *testing.T) secretstoretest.WriterRig {
		fake := &onepasswordtest.Fake{}
		signedInAs(t, fake, ada)
		return secretstoretest.WriterRig{
			Writer: openWriter(t), Prefix: testPrefix,
			LoseNextAnswer: func(call string) {
				if c, ok := fakeCalls[call]; ok {
					fake.LoseNextAnswer(c)
				}
			},
			DenyNext: func(call, message string) {
				if c, ok := fakeCalls[call]; ok {
					fake.DenyNext(c, message)
				}
			},
			Retag: func(t *testing.T, id string, tags map[string]string) {
				for _, item := range fake.Items() {
					if item.Title == id && item.State == onepassword.StateActive {
						maps.Copy(item.Cipher, tags)
						fake.Hold(item)
						return
					}
				}
				t.Fatalf("the other writer's retag: the vault holds no active item %s", id)
			},
		}
	})
}

// itemsTitled is the vault's items titled title.
func itemsTitled(fake *onepasswordtest.Fake, title string) []onepassword.Item {
	var items []onepassword.Item
	for _, item := range fake.Items() {
		if item.Title == title {
			items = append(items, item)
		}
	}
	return items
}

// createWith runs a whole create of id with value through w and answers its error.
func createWith(w secretstore.Writer, id, value string) (secretstore.Create, error) {
	ctx := context.Background()
	c := w.PrepareCreate(id, owned())
	if _, err := c.Begin(ctx); err != nil {
		return c, err
	}
	return c, c.Store(ctx, value)
}

// TestALostCreateAnswerMakesOneItem (c-op-contract's second control): the fake carries out the
// first create and loses its answer; the create reads the title again, finds the item its own
// token marks, and finishes with it, so the vault holds one item of the title, with the value,
// and its tags carry no create token.
func TestALostCreateAnswerMakesOneItem(t *testing.T) {
	fake := &onepasswordtest.Fake{}
	signedInAs(t, fake, ada)
	w := openWriter(t)
	id := testPrefix + "deel-api-key"
	fake.LoseNextAnswer(onepasswordtest.CallCreate)
	c, err := createWith(w, id, "first-value")
	if err != nil {
		t.Fatalf("a create whose answer was lost = %v (unsettled %q); want it finished with its own item", err, c.Unsettled())
	}
	items := itemsTitled(fake, id)
	if len(items) != 1 || items[0].Credential != "first-value" {
		t.Fatalf("the vault holds %d items titled %s (%+v); want one, with the value", len(items), id, items)
	}
	if calls := fake.Calls()[onepasswordtest.CallCreate]; calls != 1 {
		t.Errorf("the create called Create %d times; want once", calls)
	}
	held, err := w.CurrentTags(context.Background(), id)
	if err != nil || !maps.Equal(held.Tags, owned()) {
		t.Errorf("CurrentTags = %v, %v; want the created tags %v and no create token", held.Tags, err, owned())
	}
}

// TestACreateRefusesAnArchivedTitle: a title an archived item holds is refused, before the value
// is read, naming the 1Password app's Archive; a title an active item holds is refused too; and
// neither refusal makes an item.
func TestACreateRefusesAnArchivedTitle(t *testing.T) {
	fake := &onepasswordtest.Fake{}
	signedInAs(t, fake, ada)
	w := openWriter(t)
	archived, active := testPrefix+"gone-key", testPrefix+"held-key"
	fake.Hold(onepassword.Item{Title: archived, State: onepassword.StateArchived, Credential: "v", Cipher: owned()})
	fake.Hold(onepassword.Item{Title: active, Credential: "v", Cipher: owned()})
	for id, want := range map[string]string{
		archived: archived + " is an archived item of vault " + testVault + "; restore it, or delete it for good, from the 1Password app's Archive",
		active:   active + " is already an item of vault " + testVault,
	} {
		_, err := w.PrepareCreate(id, owned()).Begin(context.Background())
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Begin of %s = %v; want it refused: %s", id, err, want)
		}
		if n := len(itemsTitled(fake, id)); n != 1 {
			t.Errorf("after the refused create the vault holds %d items titled %s; want 1", n, id)
		}
	}
	if calls := fake.Calls()[onepasswordtest.CallCreate]; calls != 0 {
		t.Errorf("the refused creates called Create %d times; want none", calls)
	}
}

// TestRestoreRefusesNamingTheArchive: Restore answers ErrNoRestore, naming the 1Password app's
// Archive and the item's title, and makes no call.
func TestRestoreRefusesNamingTheArchive(t *testing.T) {
	fake := &onepasswordtest.Fake{}
	signedInAs(t, fake, ada)
	w := openWriter(t)
	id := testPrefix + "gone-key"
	fake.Hold(onepassword.Item{Title: id, State: onepassword.StateArchived, Credential: "v", Cipher: owned()})
	before := len(fake.Calls())
	err := w.Restore(context.Background(), id)
	if !errors.Is(err, secretstore.ErrNoRestore) || !strings.Contains(err.Error(), "restore the item "+id+" from the 1Password app's Archive") {
		t.Fatalf("Restore = %v; want ErrNoRestore naming the item and the 1Password app's Archive", err)
	}
	if after := len(fake.Calls()); after != before {
		t.Errorf("Restore made calls: %v", fake.Calls())
	}
}

// racingClient is a client whose next Put, once it lands, lets another writer retag the item.
type racingClient struct {
	*onepasswordtest.Fake
	retag func()
}

func (c *racingClient) Put(ctx context.Context, item onepassword.Item) (onepassword.Item, error) {
	put, err := c.Fake.Put(ctx, item)
	if retag := c.retag; err == nil && retag != nil {
		c.retag = nil
		retag()
	}
	return put, err
}

// TestARacedTagWriteReadsItsTagsBack: another writer's retag that lands between the tag write's
// Put and its read-back is beside the write, so the write reads its tags back and holds them to
// the check, whose refusal comes after the write landed: Landed, and ErrTagsChanged through it.
// A retag before the write's read refuses it, writing nothing.
func TestARacedTagWriteReadsItsTagsBack(t *testing.T) {
	fake := &onepasswordtest.Fake{}
	client := &racingClient{Fake: fake}
	signedInAs(t, client, ada)
	w := openWriter(t)
	ctx := context.Background()
	id := testPrefix + "deel-api-key"
	item := fake.Hold(onepassword.Item{Title: id, Credential: "v", Cipher: owned()})
	held, err := w.CurrentTags(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	client.retag = func() {
		now, _ := fake.Item(item.ID)
		now.Cipher["tier"] = "human"
		fake.Hold(now)
	}
	refuseOthers := func(after map[string]string) error {
		if after["tier"] != held.Tags["tier"] {
			return secretstore.ErrTagsChanged
		}
		return nil
	}
	err = w.SetTags(ctx, id, held, map[string]string{"judge": "600"}, refuseOthers)
	if !secretstore.Landed(err) || !errors.Is(err, secretstore.ErrTagsChanged) {
		t.Fatalf("a tag write another writer's retag raced after its Put = %v; want Landed and ErrTagsChanged", err)
	}
	if now, _ := fake.Item(item.ID); now.Cipher["judge"] != "600" || now.Cipher["tier"] != "human" {
		t.Errorf("the item's tags are %v; want the write's judge beside the other writer's tier", now.Cipher)
	}

	stale, err := w.CurrentTags(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	now, _ := fake.Item(item.ID)
	now.Cipher["tier"] = "agent"
	fake.Hold(now)
	puts := fake.Calls()[onepasswordtest.CallPut]
	if err := w.SetTags(ctx, id, stale, map[string]string{"judge": "300"}, refuseOthers); !errors.Is(err, secretstore.ErrTagsChanged) || secretstore.Landed(err) {
		t.Fatalf("a tag write after another writer's retag = %v; want ErrTagsChanged, written nowhere", err)
	}
	if got := fake.Calls()[onepasswordtest.CallPut]; got != puts {
		t.Errorf("the refused tag write made %d Puts; want none", got-puts)
	}
}

// TestAWriteOfATitleTwoItemsHoldIsRefused: a write of a title two active items hold is refused
// naming both, and writes neither.
func TestAWriteOfATitleTwoItemsHoldIsRefused(t *testing.T) {
	fake := &onepasswordtest.Fake{}
	signedInAs(t, fake, ada)
	w := openWriter(t)
	id := testPrefix + "deel-api-key"
	first := fake.Hold(onepassword.Item{Title: id, Credential: "one", Cipher: owned()})
	second := fake.Hold(onepassword.Item{Title: id, Credential: "two", Cipher: owned()})
	err := w.Set(context.Background(), id, "three")
	if err == nil || !strings.Contains(err.Error(), onepassword.Source(testVault, first.ID)) || !strings.Contains(err.Error(), onepassword.Source(testVault, second.ID)) {
		t.Fatalf("Set of a title two items hold = %v; want it refused naming both", err)
	}
	if puts := fake.Calls()[onepasswordtest.CallPut]; puts != 0 {
		t.Errorf("the refused Set made %d Puts", puts)
	}
}

// TestTheWriterSignsInAs1PasswordsOwnToolsDo: with OP_SERVICE_ACCOUNT_TOKEN set the writer starts
// the SDK and names the service account, which is no person; without it, it runs op and names
// whom op answered, its email lowercased as the person's, with OP_ACCOUNT in the rerun prefix
// where it chose the account; and a service account op answered is no person either.
func TestTheWriterSignsInAs1PasswordsOwnToolsDo(t *testing.T) {
	fake := &onepasswordtest.Fake{}
	started := signedInAs(t, fake, ada)
	ctx := context.Background()

	w := openWriter(t)
	email, err := w.PersonEmail(ctx)
	if w.Identity() != "op: Ada@Example.com" || email != "ada@example.com" || err != nil || w.RerunPrefix() != "" {
		t.Errorf("op sign-in: identity %q, person %q (%v), rerun prefix %q", w.Identity(), email, err, w.RerunPrefix())
	}
	t.Setenv("OP_ACCOUNT", "my team")
	if w := openWriter(t); w.RerunPrefix() != "OP_ACCOUNT='my team' " {
		t.Errorf("rerun prefix with OP_ACCOUNT set = %q", w.RerunPrefix())
	}
	if started.Load() != 0 {
		t.Errorf("the op sign-in started the SDK %d times", started.Load())
	}

	t.Setenv("OP_SERVICE_ACCOUNT_TOKEN", "a-token-for-the-test")
	w = openWriter(t)
	if _, err := w.PersonEmail(ctx); err == nil || w.Identity() != "OP_SERVICE_ACCOUNT_TOKEN: a 1Password service account" || w.RerunPrefix() != "" || started.Load() != 1 {
		t.Errorf("token sign-in: identity %q, person error %v, rerun prefix %q, SDK starts %d", w.Identity(), err, w.RerunPrefix(), started.Load())
	}
	if strings.Contains(w.RerunPrefix()+w.Identity()+w.ChooseAnother(), "a-token-for-the-test") {
		t.Error("the token sign-in prints the token")
	}

	t.Setenv("OP_SERVICE_ACCOUNT_TOKEN", "")
	signedInAs(t, fake, onepassword.OpAccount{ServiceAccount: true})
	w = openWriter(t)
	if _, err := w.PersonEmail(ctx); err == nil || w.Identity() != "op: a service account" {
		t.Errorf("op signed in as a service account: identity %q, person error %v", w.Identity(), err)
	}
}

// TestOpenWriterRefusesSettingsWithNoVault: settings that name no vault id, or a prefix that is no
// title prefix, are refused before any sign-in.
func TestOpenWriterRefusesSettingsWithNoVault(t *testing.T) {
	fake := &onepasswordtest.Fake{}
	signedInAs(t, fake, ada)
	for _, s := range []secretstore.Settings{
		{Store: onepassword.Name, Prefix: testPrefix, Fields: map[string]string{}, Words: func(s string) string { return s }},
		{Store: onepassword.Name, Prefix: "example/agent-secrets/", Fields: map[string]string{"onepassword_vault": testVault}, Words: func(s string) string { return s }},
	} {
		if _, err := (onepassword.Backend{}).OpenWriter(context.Background(), s, secretstore.WriterOptions{}); err == nil {
			t.Errorf("OpenWriter(%+v) opened a writer", s)
		}
	}
}
