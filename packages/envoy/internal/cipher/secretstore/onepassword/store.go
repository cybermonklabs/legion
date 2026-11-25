package onepassword

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/secretstore"
)

// Name is BROKER_SECRETS_STORE's value for the 1Password store.
const Name = "onepassword"

// sourcePrefix opens every source the 1Password store answers. No source starts with mint:.
const sourcePrefix = "onepassword:"

// Source is the source of the item item of vault vault: onepassword:<vault>/<item>.
func Source(vault, item string) string { return sourcePrefix + vault + "/" + item }

// maxNameLength is the longest title the store serves a secret under.
const maxNameLength = 255

// readHeldFor is how long a value read trusts the store's last List of the vault: within it, a
// read answers the item the store keeps, with no call, while its UpdatedAt is the one that List
// answered; after it, the read lists the vault again, and reads the item in full only when its
// UpdatedAt moved. So a burst of releases costs one List, and an item archived or deleted for good
// stops being released at most this long after the store's last List, or a reread, shows it.
const readHeldFor = 10 * time.Second

// Store is the 1Password store: the agent secrets are the items of one vault. Each List of the
// vault answers every item's title, state and UpdatedAt and no field (Client.List), so the store
// keeps every item it has read in full, by id, and at each List reads in full, in one Get, only
// the items whose UpdatedAt moved. Build it with New; it is safe for concurrent use.
type Store struct {
	client Client
	vault  string
	budget Budget
	now    func() time.Time

	// refreshing is held across each List of the vault and the Get that follows it, and across a
	// Describe's Get, so the kept items and the last List move together.
	refreshing sync.Mutex

	mu sync.Mutex
	// kept is every item the store has read in full and the last List still answered, by id.
	kept map[string]Item
	// listed is the last List's answer, by id, with what a later Get of an item said of its state.
	listed map[string]Overview
	// listedAt is when the last List answered, the zero time before the first.
	listedAt time.Time
}

// New is the store over client on vault, spending from budget, on the clock now (time.Now where
// nil).
func New(client Client, vault string, budget Budget, now func() time.Time) *Store {
	if now == nil {
		now = time.Now
	}
	return &Store{client: client, vault: vault, budget: budget, now: now, kept: map[string]Item{}, listed: map[string]Overview{}}
}

// ListLag is none: a Describe merges what it reads into what the next List answers, and the
// vault's own List shows a write at once as far as is measured.
func (*Store) ListLag() time.Duration { return 0 }

// errNameRule is ValidName's refusal.
var errNameRule = errors.New("a 1Password secret's title is 1 to 255 bytes")

// ValidName refuses a title of no bytes or more than 255.
func (*Store) ValidName(id string) error {
	if len(id) == 0 || len(id) > maxNameLength {
		return fmt.Errorf("under the prefix it makes a %d-byte title: %w", len(id), errNameRule)
	}
	return nil
}

// list lists the vault and reads in full, in one Get, each item want picks from that List,
// with the item the store keeps of it when it keeps one; then the List's answer is the store's
// last, each item read is kept, and an item the List no longer answers is dropped. The caller
// holds s.refreshing.
func (s *Store) list(ctx context.Context, want func(o Overview, kept Item, held bool) bool) error {
	overviews, err := s.client.List(ctx)
	if err != nil {
		return fmt.Errorf("list 1Password vault %s: %w", s.vault, err)
	}
	at := s.now()
	var ids []string
	s.mu.Lock()
	for _, o := range overviews {
		if kept, held := s.kept[o.ID]; want(o, kept, held) {
			ids = append(ids, o.ID)
		}
	}
	s.mu.Unlock()
	var items []Item
	if len(ids) > 0 {
		items, err = s.client.Get(ctx, ids...)
		if errors.Is(err, secretstore.ErrNotFound) {
			return fmt.Errorf("an item of 1Password vault %s was deleted between its List and its read; the next read lists the vault again", s.vault)
		}
		if err != nil {
			return fmt.Errorf("read the changed items of 1Password vault %s: %w", s.vault, err)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.listed = make(map[string]Overview, len(overviews))
	for _, o := range overviews {
		s.listed[o.ID] = o
	}
	s.listedAt = at
	for _, item := range items {
		s.keep(item)
	}
	for id := range s.kept {
		if _, ok := s.listed[id]; !ok {
			delete(s.kept, id)
		}
	}
	return nil
}

// keep holds item, unless the store keeps a read of it that 1Password changed later. The caller
// holds s.mu.
func (s *Store) keep(item Item) {
	if held, ok := s.kept[item.ID]; ok && held.UpdatedAt.After(item.UpdatedAt) {
		return
	}
	s.kept[item.ID] = item
}

// overview is what the store knows of the item of o's id: o as the last List answered it, or, with
// the title and state the kept item names, where that read of it is later than the List. The
// caller holds s.mu.
func (s *Store) overview(o Overview) Overview {
	if item, held := s.kept[o.ID]; held && item.UpdatedAt.After(o.UpdatedAt) {
		o.Title = item.Title
		if item.State != "" {
			o.State = item.State
		}
	}
	return o
}

// onKey is every listing's OnKey: 1Password encrypts each item with its vault's key, so every
// item of the vault is on the store's key.
func onKey(context.Context) (bool, error) { return true, nil }

// listings answers one Listing for each title match accepts among the items the store knows: the
// active item of that title, the first by id where two or more are, with Duplicates naming each
// active one; or, with opts.IncludeDeleted and no active one, the archived item of that title that
// changed last, with Deleted set. The caller holds s.mu.
func (s *Store) listings(match func(title string) bool, opts secretstore.ReadOptions) []secretstore.Listing {
	byTitle := map[string][]Overview{}
	for _, o := range s.listed {
		if o = s.overview(o); match(o.Title) {
			byTitle[o.Title] = append(byTitle[o.Title], o)
		}
	}
	var listings []secretstore.Listing
	for _, title := range slices.Sorted(maps.Keys(byTitle)) {
		group := byTitle[title]
		slices.SortFunc(group, func(a, b Overview) int { return cmp.Compare(a.ID, b.ID) })
		var active, archived []Overview
		for _, o := range group {
			if o.State == StateActive {
				active = append(active, o)
			} else {
				archived = append(archived, o)
			}
		}
		var chosen Overview
		switch {
		case len(active) > 0:
			chosen = active[0]
		case opts.IncludeDeleted && len(archived) > 0:
			chosen = slices.MaxFunc(archived, func(a, b Overview) int { return a.UpdatedAt.Compare(b.UpdatedAt) })
		default:
			continue
		}
		item, held := s.kept[chosen.ID]
		if !held {
			continue
		}
		ls := s.listing(title, item, chosen.State == StateArchived, opts)
		if len(active) > 1 {
			for _, o := range active {
				ls.Duplicates = append(ls.Duplicates, Source(s.vault, o.ID))
			}
		}
		listings = append(listings, ls)
	}
	return listings
}

// listing is the Listing of item under title: its tags are its cipher fields but the create
// token, its value its credential, and its Version 1Password's count of its changes.
func (s *Store) listing(title string, item Item, archived bool, opts secretstore.ReadOptions) secretstore.Listing {
	tags := make(map[string]string, len(item.Cipher))
	for label, value := range item.Cipher {
		if label != CreateField {
			tags[label] = value
		}
	}
	ls := secretstore.Listing{
		ID: title, Source: Source(s.vault, item.ID), Tags: tags, OnKey: onKey, HasCurrentValue: item.Credential != "",
		Created: item.CreatedAt, LastChanged: item.UpdatedAt,
	}
	if ls.HasCurrentValue {
		ls.Version = strconv.FormatUint(uint64(item.Version), 10)
	}
	if archived {
		ls.Deleted = item.UpdatedAt
	}
	if opts.Versions {
		ls.Versions = map[string][]string{}
		if ls.Version != "" {
			ls.Versions[ls.Version] = []string{"CURRENT"}
		}
	}
	return ls
}

// List lists the vault, reads in full each item under prefix whose UpdatedAt moved since the store
// last read it (an archived one only with opts.IncludeDeleted), and answers one listing a title
// (listings).
func (s *Store) List(ctx context.Context, prefix string, opts secretstore.ReadOptions) ([]secretstore.Listing, error) {
	s.refreshing.Lock()
	defer s.refreshing.Unlock()
	err := s.list(ctx, func(o Overview, kept Item, held bool) bool {
		return strings.HasPrefix(o.Title, prefix) && (o.State == StateActive || opts.IncludeDeleted) && (!held || !kept.UpdatedAt.Equal(o.UpdatedAt))
	})
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.listings(func(title string) bool { return strings.HasPrefix(title, prefix) }, opts), nil
}

// Describe reads in full, in one Get, every item the store knows to hold the title id (archived
// ones only with opts.IncludeDeleted), and merges what it reads into what the next List answers;
// where it knows none, or one of them is gone for good, it lists the vault first and reads the
// items of that title it answers. It answers ErrNotFound for a title no item holds, or, without
// opts.IncludeDeleted, only archived ones; a title two active items hold it answers with
// Duplicates set, as List does, so the policy refuses it by that mark rather than as absent. An
// item of the title that another writer makes after the store's last List is seen at the next
// List.
func (s *Store) Describe(ctx context.Context, id string, opts secretstore.ReadOptions) (secretstore.Listing, error) {
	s.refreshing.Lock()
	defer s.refreshing.Unlock()
	wanted := func(o Overview) bool { return o.Title == id && (o.State == StateActive || opts.IncludeDeleted) }
	var ids []string
	s.mu.Lock()
	for _, o := range s.listed {
		if wanted(s.overview(o)) {
			ids = append(ids, o.ID)
		}
	}
	s.mu.Unlock()
	if len(ids) > 0 {
		items, err := s.client.Get(ctx, ids...)
		switch {
		case errors.Is(err, secretstore.ErrNotFound):
			ids = nil
		case err != nil:
			return secretstore.Listing{}, fmt.Errorf("read %s from 1Password vault %s: %w", id, s.vault, err)
		default:
			s.mu.Lock()
			for _, item := range items {
				s.keep(item)
				if item.State != "" {
					s.listed[item.ID] = Overview{ID: item.ID, Title: item.Title, State: item.State, UpdatedAt: item.UpdatedAt}
				}
			}
			s.mu.Unlock()
		}
	}
	if len(ids) == 0 {
		if err := s.list(ctx, func(o Overview, _ Item, _ bool) bool { return wanted(o) }); err != nil {
			return secretstore.Listing{}, err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	listings := s.listings(func(title string) bool { return title == id }, opts)
	if len(listings) == 0 {
		return secretstore.Listing{}, fmt.Errorf("1Password vault %s holds no item %s: %w", s.vault, id, secretstore.ErrNotFound)
	}
	return listings[0], nil
}

// Read answers the credential of the item source names. Within readHeldFor of the store's last
// List it answers the kept item, with no call, while its UpdatedAt is the one that List answered;
// otherwise it lists the vault, and reads the item in full only where its UpdatedAt moved.
// ErrNotFound for a source of another vault, an item the vault no longer holds, one in its
// Archive, and one with no credential.
func (s *Store) Read(ctx context.Context, source string) (string, error) {
	rest, ok := strings.CutPrefix(source, sourcePrefix)
	vault, id, cut := strings.Cut(rest, "/")
	if !ok || !cut || vault != s.vault {
		return "", fmt.Errorf("%s is no item of 1Password vault %s: %w", source, s.vault, secretstore.ErrNotFound)
	}
	if value, held, err := s.held(id, true); held {
		return value, err
	}
	s.refreshing.Lock()
	defer s.refreshing.Unlock()
	// Another read may have listed the vault while this one waited.
	if value, held, err := s.held(id, true); held {
		return value, err
	}
	err := s.list(ctx, func(o Overview, kept Item, held bool) bool {
		return o.ID == id && (!held || !kept.UpdatedAt.Equal(o.UpdatedAt))
	})
	if err != nil {
		return "", err
	}
	value, _, err := s.held(id, false)
	return value, err
}

// held answers the credential of item id from what the store keeps, and whether it could: with
// fresh, only within readHeldFor of the last List and while the kept item's UpdatedAt is that
// List's; without, from the kept item whatever its age, after a List that read it. ErrNotFound for
// an item that List did not answer, one archived, and one with no credential.
func (s *Store) held(id string, fresh bool) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if fresh && (s.listedAt.IsZero() || s.now().Sub(s.listedAt) >= readHeldFor) {
		return "", false, nil
	}
	o, listed := s.listed[id]
	if !listed {
		return "", true, fmt.Errorf("1Password vault %s holds no item %s: %w", s.vault, id, secretstore.ErrNotFound)
	}
	item, kept := s.kept[id]
	if !kept || fresh && !item.UpdatedAt.Equal(o.UpdatedAt) {
		if fresh {
			return "", false, nil
		}
		return "", true, fmt.Errorf("1Password vault %s answered item %s in its List but not in its read: %w", s.vault, id, secretstore.ErrNotFound)
	}
	if s.overview(o).State != StateActive {
		return "", true, fmt.Errorf("item %s of 1Password vault %s is in its Archive: %w", id, s.vault, secretstore.ErrNotFound)
	}
	if item.Credential == "" {
		return "", true, fmt.Errorf("item %s of 1Password vault %s has no credential: %w", id, s.vault, secretstore.ErrNotFound)
	}
	return item.Credential, true, nil
}
