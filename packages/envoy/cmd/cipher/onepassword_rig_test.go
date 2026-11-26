// packages/envoy/cmd/cipher/onepassword_rig_test.go
//
// The 1Password store as the secret forms' tests write it: one vault of onepasswordtest.Fake,
// which every writer the forms open reaches through op signed in as ada@example.com
// (onepassword.SwapHooks), behind a client that counts each call and fails the ones a test asks it
// to. A 1Password item's value comes with the item's read, so a value read is no call of its own:
// the rig counts and refuses the forms' value reads where every one goes through, Hooks.Read.
package main

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"sync"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/secretstore"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/onepassword"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/onepassword/onepasswordtest"
)

// The vault and the title prefix of the rig's broker: an id of 1Password's form, no real vault's.
const (
	testOnePasswordVault  = "vault0000000000000000000aa"
	testOnePasswordPrefix = "agent-secrets-"
)

// onePasswordSettings are a broker's settings on the rig's vault, as a broker of this release that
// reads the type tag answers them.
func onePasswordSettings() Settings {
	return Settings{Store: onepassword.Name, SecretsPrefix: testOnePasswordPrefix, SecretTypeTag: true, Fields: map[string]string{"onepassword_vault": testOnePasswordVault}}
}

// onePasswordTypeStore is the 1Password store as typeStore.
type onePasswordTypeStore struct{ rig *onePasswordRig }

// onePasswordRig is the vault, and what the client in front of it counts and fails.
type onePasswordRig struct {
	fake *onepasswordtest.Fake

	mu sync.Mutex
	// calls counts every call of the client, refused ones included; tagWrites the Puts that change
	// an item's tags and not its value, refused ones included; and values the forms' value reads.
	calls, tagWrites, values int
	// loseTagAt and refuseTagAt are the tag write whose answer is lost and the one refused, by
	// tagWrites, 0 for none.
	loseTagAt, refuseTagAt int
	// refuseReads fails every List and Get; refuseValues every value read.
	refuseReads, refuseValues error
	afterWrite                func()
	// sdkStarts counts the SDK clients the forms started, which no rig's sign-in asks for.
	sdkStarts int
	// hangCreate makes every Create answer nothing until its context ends, having made nothing.
	hangCreate bool
}

// newOnePasswordTypeStore makes an empty vault and hooks every writer opened for the rest of t to
// it, through op signed in as ada@example.com, with neither OP_SERVICE_ACCOUNT_TOKEN nor OP_ACCOUNT
// set.
func newOnePasswordTypeStore(t *testing.T) onePasswordTypeStore {
	t.Helper()
	t.Setenv("OP_SERVICE_ACCOUNT_TOKEN", "")
	t.Setenv("OP_ACCOUNT", "")
	rig := &onePasswordRig{fake: &onepasswordtest.Fake{}}
	t.Cleanup(onepassword.SwapHooks(onepassword.Hooks{
		SDKClient: func(context.Context, string, string) (onepassword.Client, error) {
			rig.mu.Lock()
			rig.sdkStarts++
			rig.mu.Unlock()
			return nil, errors.New("the rig's writers sign in through op")
		},
		OpClient: func(string, secretstore.RunChild) onepassword.Client { return onePasswordRigClient{rig} },
		Whoami: func(context.Context, secretstore.RunChild) (onepassword.OpAccount, error) {
			return onepassword.OpAccount{Email: "ada@example.com"}, nil
		},
		Read: func(ctx context.Context, source string, read func(context.Context, string) (string, error)) (string, error) {
			rig.mu.Lock()
			rig.values++
			refuse := rig.refuseValues
			rig.mu.Unlock()
			if refuse != nil {
				return "", refuse
			}
			return read(ctx, source)
		},
	}))
	return onePasswordTypeStore{rig}
}

// onePasswordRigClient is the client every writer the rig hooks reaches the vault through.
type onePasswordRigClient struct{ rig *onePasswordRig }

// begin counts one call and answers the refusal of a read where the rig refuses reads.
func (c onePasswordRigClient) begin(read bool) error {
	c.rig.mu.Lock()
	defer c.rig.mu.Unlock()
	c.rig.calls++
	if read {
		return c.rig.refuseReads
	}
	return nil
}

// landed runs the rig's afterWrite, where one is set, once a write has landed.
func (c onePasswordRigClient) landed() {
	c.rig.mu.Lock()
	after := c.rig.afterWrite
	c.rig.mu.Unlock()
	if after != nil {
		after()
	}
}

func (c onePasswordRigClient) List(ctx context.Context) ([]onepassword.Overview, error) {
	if err := c.begin(true); err != nil {
		return nil, err
	}
	return c.rig.fake.List(ctx)
}

func (c onePasswordRigClient) Get(ctx context.Context, ids ...string) ([]onepassword.Item, error) {
	if err := c.begin(true); err != nil {
		return nil, err
	}
	return c.rig.fake.Get(ctx, ids...)
}

func (c onePasswordRigClient) Create(ctx context.Context, item onepassword.Item) (onepassword.Item, error) {
	_ = c.begin(false)
	c.rig.mu.Lock()
	hang := c.rig.hangCreate
	c.rig.mu.Unlock()
	if hang {
		<-ctx.Done()
		return onepassword.Item{}, ctx.Err()
	}
	made, err := c.rig.fake.Create(ctx, item)
	if err == nil {
		c.landed()
	}
	return made, err
}

// Put writes item; a Put that changes its tags and not its value is a tag write, which the rig
// refuses as access denied, or carries out and loses the answer of, where a test asked it to.
func (c onePasswordRigClient) Put(ctx context.Context, item onepassword.Item) (onepassword.Item, error) {
	c.begin(false)
	held, _ := c.rig.fake.Item(item.ID)
	lose := false
	if held.Credential == item.Credential {
		c.rig.mu.Lock()
		c.rig.tagWrites++
		at := c.rig.tagWrites
		refuse, loseIt := at == c.rig.refuseTagAt, at == c.rig.loseTagAt
		c.rig.mu.Unlock()
		if refuse {
			return onepassword.Item{}, fmt.Errorf("%w: the rig refused the tag write", onepassword.ErrDenied)
		}
		lose = loseIt
	}
	put, err := c.rig.fake.Put(ctx, item)
	if err != nil {
		return put, err
	}
	c.landed()
	if lose {
		return onepassword.Item{}, onepasswordtest.ErrAnswerLost
	}
	return put, nil
}

func (c onePasswordRigClient) Archive(ctx context.Context, id string) error {
	c.begin(false)
	return c.rig.fake.Archive(ctx, id)
}

// title is the item title of the secret a session asks for as name.
func (s onePasswordTypeStore) title(t *testing.T, name string) string {
	t.Helper()
	slug, err := policy.NameToSlug(name)
	if err != nil {
		t.Fatal(err)
	}
	return testOnePasswordPrefix + slug
}

// item is the vault's item of name: its active one, else the one of its title archived last, and
// false where the vault holds none.
func (s onePasswordTypeStore) item(t *testing.T, name string) (onepassword.Item, bool) {
	t.Helper()
	title := s.title(t, name)
	var found onepassword.Item
	ok := false
	for _, item := range s.rig.fake.Items() {
		if item.Title != title {
			continue
		}
		if item.State == onepassword.StateActive {
			return item, true
		}
		if !ok || item.UpdatedAt.After(found.UpdatedAt) {
			found, ok = item, true
		}
	}
	return found, ok
}

// hold makes item the vault's item of name, replacing its active one where it holds one.
func (s onePasswordTypeStore) hold(t *testing.T, name string, change func(item *onepassword.Item)) {
	t.Helper()
	item, ok := s.item(t, name)
	if !ok || item.State != onepassword.StateActive {
		item = onepassword.Item{Title: s.title(t, name)}
	}
	change(&item)
	s.rig.fake.Hold(item)
}

func (s onePasswordTypeStore) put(t *testing.T, name string, tags map[string]string, value string) {
	t.Helper()
	s.hold(t, name, func(item *onepassword.Item) {
		item.Cipher, item.Credential = maps.Clone(tags), value
	})
}

func (s onePasswordTypeStore) has(t *testing.T, name string) bool {
	_, ok := s.item(t, name)
	return ok
}

func (s onePasswordTypeStore) value(t *testing.T, name string) string {
	item, _ := s.item(t, name)
	return item.Credential
}

func (s onePasswordTypeStore) tags(t *testing.T, name string) map[string]string {
	item, ok := s.item(t, name)
	if !ok {
		return nil
	}
	tags := maps.Clone(item.Cipher)
	delete(tags, onepassword.CreateField)
	return tags
}

// refuseValueReads refuses every value read of the forms from now on: as 1Password's refusal of
// the sign-in when denied, and as a read that failed otherwise.
func (s onePasswordTypeStore) refuseValueReads(denied bool) {
	err := errors.New("the read of the item's value failed")
	if denied {
		err = fmt.Errorf("%w: the rig refused the value read", onepassword.ErrDenied)
	}
	s.rig.mu.Lock()
	defer s.rig.mu.Unlock()
	s.rig.refuseValues = err
}

func (s onePasswordTypeStore) valueReads() int {
	s.rig.mu.Lock()
	defer s.rig.mu.Unlock()
	return s.rig.values
}

func (s onePasswordTypeStore) readPermissions() []string {
	return []string{"read access to the vault's values (read_items for a service account, View and Copy Passwords for a person)"}
}

func (s onePasswordTypeStore) calls() int {
	s.rig.mu.Lock()
	defer s.rig.mu.Unlock()
	return s.rig.calls
}

func (s onePasswordTypeStore) tagWrites() int {
	s.rig.mu.Lock()
	defer s.rig.mu.Unlock()
	return s.rig.tagWrites
}

// refuseTagReads refuses every List and Get from now on as 1Password's refusal of the sign-in,
// whose words name no permission.
func (s onePasswordTypeStore) refuseTagReads() {
	s.rig.mu.Lock()
	defer s.rig.mu.Unlock()
	s.rig.refuseReads = fmt.Errorf("%w: the rig refused the read", onepassword.ErrDenied)
}

func (s onePasswordTypeStore) tagReadPermission() string {
	return "read access to the vault (read_items for a service account, View Items for a person)"
}

func (s onePasswordTypeStore) storeName() string { return "1Password vault " + testOnePasswordVault }

func (s onePasswordTypeStore) madeBeforeValue() bool { return false }

func (s onePasswordTypeStore) restores() bool { return false }

func (s onePasswordTypeStore) loseTagWriteAnswer(n int) {
	s.rig.mu.Lock()
	defer s.rig.mu.Unlock()
	s.rig.loseTagAt = s.rig.tagWrites + n
}

func (s onePasswordTypeStore) refuseTagWrite(n int) {
	s.rig.mu.Lock()
	defer s.rig.mu.Unlock()
	s.rig.refuseTagAt = s.rig.tagWrites + n
}

func (s onePasswordTypeStore) retag(t *testing.T, name string, set map[string]string, remove ...string) {
	t.Helper()
	if _, ok := s.item(t, name); !ok {
		t.Fatalf("the other writer's retag: the vault holds no %s", s.title(t, name))
	}
	s.hold(t, name, func(item *onepassword.Item) {
		if item.Cipher == nil {
			item.Cipher = map[string]string{}
		}
		maps.Copy(item.Cipher, set)
		for _, key := range remove {
			delete(item.Cipher, key)
		}
	})
}

func (s onePasswordTypeStore) setValue(t *testing.T, name, value string) {
	t.Helper()
	if _, ok := s.item(t, name); !ok {
		t.Fatalf("the other writer's value: the vault holds no %s", s.title(t, name))
	}
	s.hold(t, name, func(item *onepassword.Item) { item.Credential = value })
}

func (s onePasswordTypeStore) afterEachWrite(f func()) {
	s.rig.mu.Lock()
	defer s.rig.mu.Unlock()
	s.rig.afterWrite = f
}

// servedBy answers a reread as a broker on the vault would: absent for a name no active item
// holds, name-duplicated for one two do, no-current-value for one with no credential, and
// otherwise what its tags answer (servedAs).
func (s onePasswordTypeStore) servedBy(t *testing.T) func(name string) Reread {
	return func(name string) Reread {
		slug, err := policy.NameToSlug(name)
		if err != nil {
			return Reread{Name: name, Reason: policy.ReasonAbsent}
		}
		var active []onepassword.Item
		for _, item := range s.rig.fake.Items() {
			if item.Title == testOnePasswordPrefix+slug && item.State == onepassword.StateActive {
				active = append(active, item)
			}
		}
		switch {
		case len(active) == 0:
			return Reread{Name: name, Reason: policy.ReasonAbsent}
		case len(active) > 1:
			return Reread{Name: name, Reason: policy.ReasonNameDuplicated}
		case active[0].Credential == "":
			return Reread{Name: name, Reason: policy.ReasonNoCurrentValue}
		}
		return servedAs(name, s.tags(t, name))
	}
}

// sdkStarted is how many SDK clients the forms started.
func (s onePasswordTypeStore) sdkStarted() int {
	s.rig.mu.Lock()
	defer s.rig.mu.Unlock()
	return s.rig.sdkStarts
}

// TestTheCLIStartsNoSDKWithoutTheStore (Review focus 4): with OP_SERVICE_ACCOUNT_TOKEN set, every
// secret form against a broker on AWS starts no 1Password SDK, which runs its core as WebAssembly
// and would cost every form its start. Control: the same form against a broker on a 1Password
// vault starts it once.
func TestTheCLIStartsNoSDKWithoutTheStore(t *testing.T) {
	rig := storeRigs["aws"](t)
	startSecretBrokerWith(t, rig.settings, rig.answer)
	rig.store.put(t, "HELD_KEY", ownerTier("ada@example.com", policy.TierAgent), "v1")
	var started int
	t.Cleanup(onepassword.SwapHooks(onepassword.Hooks{
		SDKClient: func(context.Context, string, string) (onepassword.Client, error) {
			started++
			return nil, errors.New("this form must start no 1Password SDK")
		},
	}))
	t.Setenv("OP_SERVICE_ACCOUNT_TOKEN", "a-token-for-the-test")
	for _, args := range [][]string{{"list"}, {"show", "HELD_KEY"}, {"retag", "HELD_KEY", "--tier", policy.TierHuman}, {"delete", "HELD_KEY"}} {
		if _, stderr, code := runSecret(args...); code != 0 {
			t.Fatalf("%v against a broker on AWS: exit %d stderr %q", args, code, stderr)
		}
	}
	if started != 0 {
		t.Fatalf("forms against a broker on AWS started the 1Password SDK %d times; want none", started)
	}

	store := newOnePasswordTypeStore(t)
	startSecretBrokerWith(t, onePasswordSettings(), store.servedBy(t))
	t.Setenv("OP_SERVICE_ACCOUNT_TOKEN", "a-token-for-the-test")
	if _, _, code := runSecret("list"); code == 0 || store.sdkStarted() != 1 {
		t.Fatalf("control: list against a broker on a 1Password vault with a token exited %d and started the SDK %d times; want it started once, and refused by the rig", code, store.sdkStarted())
	}
}
