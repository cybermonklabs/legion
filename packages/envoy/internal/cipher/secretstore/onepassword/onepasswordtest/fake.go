// Package onepasswordtest is the 1Password store's test support: Fake, one vault in memory behind
// onepassword.Client, which counts the requests each call spends and can fail or lose a call's
// answer on purpose.
package onepasswordtest

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/secretstore"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/onepassword"
)

// The calls of onepassword.Client, as Cost, Calls, LoseNextAnswer and DenyNext name them.
const (
	CallList    = "list"
	CallGet     = "get"
	CallCreate  = "create"
	CallPut     = "put"
	CallArchive = "archive"
)

// ErrAnswerLost is the error of a call whose answer LoseNextAnswer lost: the call was carried out,
// and its caller cannot tell.
var ErrAnswerLost = errors.New("the connection to 1Password closed before its answer came")

// Fake is one 1Password vault in memory, safe for concurrent use. Its zero value is an empty vault
// whose every call spends one request.
type Fake struct {
	// Cost is the requests each call spends, by call (CallList and the others); a call it does not
	// name spends one. Set it before the first call.
	Cost map[string]int
	// Now is the clock of the items' CreatedAt and UpdatedAt, time.Now where nil. Set it before the
	// first call.
	Now func() time.Time

	mu          sync.Mutex
	items       map[string]onepassword.Item
	made        int
	calls       map[string]int
	requests    int
	lose        map[string]bool
	deny        map[string]string
	dropSection bool
}

// Hold makes item an item of the vault, or replaces the one of its ID, as another writer, such
// as the 1Password app, makes or changes it, and answers it as the vault holds it: an item with no
// ID gets a new one, every Hold moves Version and UpdatedAt, and an item with no State is active.
// It spends no request.
func (f *Fake) Hold(item onepassword.Item) onepassword.Item {
	f.mu.Lock()
	defer f.mu.Unlock()
	if item.State == "" {
		item.State = onepassword.StateActive
	}
	return f.hold(item)
}

// Remove deletes the item for good, as a person does from the Archive. It spends no request.
func (f *Fake) Remove(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.items, id)
}

// Item answers the item of id as the vault holds it, and false where it holds none.
func (f *Fake) Item(id string) (onepassword.Item, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	item, ok := f.items[id]
	return clone(item), ok
}

// Items is every item of the vault, in the order of their ids.
func (f *Fake) Items() []onepassword.Item {
	f.mu.Lock()
	defer f.mu.Unlock()
	items := make([]onepassword.Item, 0, len(f.items))
	for _, id := range slices.Sorted(maps.Keys(f.items)) {
		items = append(items, clone(f.items[id]))
	}
	return items
}

// Calls is how many calls of each kind the fake has been asked, denied and lost ones included.
func (f *Fake) Calls() map[string]int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return maps.Clone(f.calls)
}

// Requests is how many requests every call so far has spent (Cost).
func (f *Fake) Requests() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests
}

// LoseNextAnswer makes the next call of kind call carry out what it asks and answer ErrAnswerLost.
func (f *Fake) LoseNextAnswer(call string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.lose == nil {
		f.lose = map[string]bool{}
	}
	f.lose[call] = true
}

// DenyNext makes the next call of kind call do nothing and answer an error wrapping
// onepassword.ErrDenied, with message as 1Password's own words.
func (f *Fake) DenyNext(call, message string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.deny == nil {
		f.deny = map[string]string{}
	}
	f.deny[call] = message
}

// DropSection makes every later Get answer each item without its cipher section's fields, as a
// GetAll that dropped the section would: the store contract's control.
func (f *Fake) DropSection() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dropSection = true
}

// List answers every item of the vault, active and archived, in the order of their ids.
func (f *Fake) List(ctx context.Context) ([]onepassword.Overview, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin(ctx, CallList); err != nil {
		return nil, err
	}
	overviews := make([]onepassword.Overview, 0, len(f.items))
	for _, id := range slices.Sorted(maps.Keys(f.items)) {
		item := f.items[id]
		overviews = append(overviews, onepassword.Overview{ID: item.ID, Title: item.Title, State: item.State, UpdatedAt: item.UpdatedAt})
	}
	return overviews, f.end(CallList)
}

// Get answers the items of ids, in their order, and an error wrapping secretstore.ErrNotFound for
// an id the vault does not hold.
func (f *Fake) Get(ctx context.Context, ids ...string) ([]onepassword.Item, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin(ctx, CallGet); err != nil {
		return nil, err
	}
	items := make([]onepassword.Item, 0, len(ids))
	for _, id := range ids {
		item, ok := f.items[id]
		if !ok {
			return nil, fmt.Errorf("the fake vault holds no item %s: %w", id, secretstore.ErrNotFound)
		}
		item = clone(item)
		if f.dropSection {
			item.Cipher = nil
		}
		items = append(items, item)
	}
	return items, f.end(CallGet)
}

// Create makes item a new active item of the vault, at Version 1.
func (f *Fake) Create(ctx context.Context, item onepassword.Item) (onepassword.Item, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin(ctx, CallCreate); err != nil {
		return onepassword.Item{}, err
	}
	item.ID, item.State, item.Version = "", onepassword.StateActive, 0
	return f.hold(item), f.end(CallCreate)
}

// Put writes item's title, credential and cipher fields onto the item of its ID, which keeps its
// state, and moves its Version and UpdatedAt. It accepts any Version: whether 1Password refuses a
// Put of an item whose Version is older than the vault's is not measured.
func (f *Fake) Put(ctx context.Context, item onepassword.Item) (onepassword.Item, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin(ctx, CallPut); err != nil {
		return onepassword.Item{}, err
	}
	held, ok := f.items[item.ID]
	if !ok {
		return onepassword.Item{}, fmt.Errorf("the fake vault holds no item %s: %w", item.ID, secretstore.ErrNotFound)
	}
	held.Title, held.Credential, held.Cipher = item.Title, item.Credential, item.Cipher
	return f.hold(held), f.end(CallPut)
}

// Archive moves the item to the Archive, which moves its UpdatedAt.
func (f *Fake) Archive(ctx context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin(ctx, CallArchive); err != nil {
		return err
	}
	held, ok := f.items[id]
	if !ok {
		return fmt.Errorf("the fake vault holds no item %s: %w", id, secretstore.ErrNotFound)
	}
	held.State, held.UpdatedAt = onepassword.StateArchived, f.next(held.UpdatedAt)
	f.items[id] = held
	return f.end(CallArchive)
}

// begin counts one call of kind call and the requests it spends, and answers the denial DenyNext
// set for it, or ctx's error. The caller holds f.mu.
func (f *Fake) begin(ctx context.Context, call string) error {
	if f.calls == nil {
		f.calls = map[string]int{}
	}
	f.calls[call]++
	cost, ok := f.Cost[call]
	if !ok {
		cost = 1
	}
	f.requests += cost
	if message, ok := f.deny[call]; ok {
		delete(f.deny, call)
		return fmt.Errorf("%w: %s", onepassword.ErrDenied, message)
	}
	return ctx.Err()
}

// end answers ErrAnswerLost when LoseNextAnswer lost call's answer, which the call has carried out.
// The caller holds f.mu.
func (f *Fake) end(call string) error {
	if f.lose[call] {
		delete(f.lose, call)
		return ErrAnswerLost
	}
	return nil
}

// hold stores item. One the vault holds by its ID keeps that item's CreatedAt, and its Version
// and UpdatedAt move; any other is a new item at Version 1, with a new ID where it has none. The
// caller holds f.mu.
func (f *Fake) hold(item onepassword.Item) onepassword.Item {
	if f.items == nil {
		f.items = map[string]onepassword.Item{}
	}
	if held, ok := f.items[item.ID]; ok && item.ID != "" {
		item.CreatedAt, item.Version, item.UpdatedAt = held.CreatedAt, held.Version+1, f.next(held.UpdatedAt)
	} else {
		if item.ID == "" {
			f.made++
			item.ID = fmt.Sprintf("fakeitem%018d", f.made)
		}
		item.UpdatedAt = f.next(time.Time{})
		item.CreatedAt, item.Version = item.UpdatedAt, 1
	}
	f.items[item.ID] = clone(item)
	return clone(item)
}

// next is the clock's time, or a nanosecond after last where the clock has not passed it, so each
// change moves an item's UpdatedAt.
func (f *Fake) next(last time.Time) time.Time {
	now := time.Now()
	if f.Now != nil {
		now = f.Now()
	}
	if after := last.Add(time.Nanosecond); !now.After(after) {
		return after
	}
	return now
}

// clone is item with a map of its own.
func clone(item onepassword.Item) onepassword.Item {
	item.Cipher = maps.Clone(item.Cipher)
	return item
}
