package onepassword

import (
	"context"
	"fmt"
	"maps"
	"strconv"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/secretstore"
)

// Writer is the 1Password store's writer: the Store over the vault the broker's settings name,
// read and written with the CLI's own sign-in (SignIn), so the vault's permissions decide each
// write. Each write reads the items of its name fresh (Store.titled) before it writes.
type Writer struct {
	*Store
	secretstore.SignIn
	// prefix is the agent secrets' title prefix the broker's settings name, from which a printed
	// command names a secret as a session asks for it.
	prefix string
	// read is Hooks.Read as the writer was opened with it, nil for none.
	read func(ctx context.Context, source string, read func(ctx context.Context, source string) (string, error)) (string, error)
}

var _ secretstore.Writer = (*Writer)(nil)

// Read answers the secret's value as the Store does, through Hooks.Read where one is set.
func (w *Writer) Read(ctx context.Context, source string) (string, error) {
	if w.read != nil {
		return w.read(ctx, source, w.Store.Read)
	}
	return w.Store.Read(ctx, source)
}

// Close holds nothing open: the SDK releases its client when the writer is collected.
func (w *Writer) Close() error { return nil }

// version is an item's Version as a held read's etag.
func version(item Item) string { return strconv.FormatUint(uint64(item.Version), 10) }

// tagsOf is an item's tags: its cipher fields but the create token.
func tagsOf(item Item) map[string]string {
	tags := maps.Clone(item.Cipher)
	if tags == nil {
		tags = map[string]string{}
	}
	delete(tags, CreateField)
	return tags
}

// active is the one active item titled id, read fresh: ErrNotFound where the vault holds none, and
// a refusal naming each where it holds more than one, which no write may choose between.
func (w *Writer) active(ctx context.Context, id string) (Item, error) {
	items, err := w.titled(ctx, id)
	if err != nil {
		return Item{}, err
	}
	var active []Item
	for _, item := range items {
		if item.State == StateActive {
			active = append(active, item)
		}
	}
	switch len(active) {
	case 0:
		return Item{}, fmt.Errorf("1Password vault %s holds no active item %s: %w", w.vault, id, secretstore.ErrNotFound)
	case 1:
		return active[0], nil
	}
	sources := make([]string, 0, len(active))
	for _, item := range active {
		sources = append(sources, Source(w.vault, item.ID))
	}
	return Item{}, fmt.Errorf("%s is the title of %d active items of vault %s (%s), so no write can tell which is the secret: archive all but one in the 1Password app", id, len(active), w.vault, strings.Join(sources, ", "))
}

// put writes item, under secretstore.CallDeadline, its failure worded as what (failure).
func (w *Writer) put(ctx context.Context, what string, item Item) error {
	ctx, cancel := secretstore.WithCallDeadline(ctx)
	defer cancel()
	_, err := w.client.Put(ctx, item)
	return failure(what, err)
}

// Set writes value as the credential of the one active item titled id.
func (w *Writer) Set(ctx context.Context, id, value string) error {
	item, err := w.active(ctx, id)
	if err != nil {
		return err
	}
	item.Credential = value
	return w.put(ctx, "write "+id+"'s value to 1Password vault "+w.vault, item)
}

// CurrentTags reads the item's tags, with its Version as their etag and its source.
func (w *Writer) CurrentTags(ctx context.Context, id string) (secretstore.HeldTags, error) {
	item, err := w.active(ctx, id)
	if err != nil {
		return secretstore.HeldTags{}, err
	}
	return secretstore.HeldTags{Tags: tagsOf(item), Etag: version(item), Source: Source(w.vault, item.ID)}, nil
}

// SetTags writes tags, keeping every other cipher field (writeTags).
func (w *Writer) SetTags(ctx context.Context, id string, held secretstore.HeldTags, tags map[string]string, check secretstore.TagCheck) error {
	return w.writeTags(ctx, id, held, check, func(cipher map[string]string) { maps.Copy(cipher, tags) })
}

// RemoveTags removes keys, keeping every other cipher field (writeTags).
func (w *Writer) RemoveTags(ctx context.Context, id string, held secretstore.HeldTags, keys []string, check secretstore.TagCheck) error {
	return w.writeTags(ctx, id, held, check, func(cipher map[string]string) {
		for _, key := range keys {
			delete(cipher, key)
		}
	})
}

// writeTags reads the item fresh and refuses with ErrTagsChanged, writing nothing, where its
// Version moved since held was read; else it applies change to its cipher fields and Puts the
// item. A Put that 1Password answered with no definite refusal may have landed
// (secretstore.TagsUnknown). Whether 1Password itself refuses a Put of an item whose Version is
// older than the vault's is not measured, so another writer's write between this read and the Put
// can still land beside it: after the Put the tags are read back and held to check, as on a store
// whose tag writes take no condition, and check's refusal, or a read-back that failed, comes after
// the write landed (secretstore.TagsLanded). With no check it reads nothing back.
func (w *Writer) writeTags(ctx context.Context, id string, held secretstore.HeldTags, check secretstore.TagCheck, change func(map[string]string)) error {
	item, err := w.active(ctx, id)
	if err != nil {
		return err
	}
	if held.Etag != "" && version(item) != held.Etag {
		return secretstore.ErrTagsChanged
	}
	if item.Cipher == nil {
		item.Cipher = map[string]string{}
	}
	change(item.Cipher)
	if err := w.put(ctx, "write "+id+"'s tags to 1Password vault "+w.vault, item); err != nil {
		if definiteRefusal(err) {
			return err
		}
		return secretstore.TagsUnknown{Err: err}
	}
	if check == nil {
		return nil
	}
	now, err := w.CurrentTags(ctx, id)
	if err != nil {
		return secretstore.TagsLanded{Err: fmt.Errorf("the retag was written, but reading its tags back failed, so whether another write changed them beside it is unknown: %w", err)}
	}
	if err := check(now.Tags); err != nil {
		return secretstore.TagsLanded{Err: err}
	}
	return nil
}

// Delete moves the one active item titled id to the vault's Archive, from which a person restores
// it, or deletes it for good, in the 1Password app; it answers the zero time, since 1Password
// keeps an archived item with no end.
func (w *Writer) Delete(ctx context.Context, id string) (time.Time, error) {
	item, err := w.active(ctx, id)
	if err != nil {
		return time.Time{}, err
	}
	ctx, cancel := secretstore.WithCallDeadline(ctx)
	defer cancel()
	return time.Time{}, failure("archive "+id+" in 1Password vault "+w.vault, w.client.Archive(ctx, item.ID))
}

// Restore refuses: neither the SDK's ItemsAPI (items.go, v0.4.1) nor op (`op item --help`, 2.41.1)
// has a call that restores an archived item, so a person restores it in the 1Password app.
func (w *Writer) Restore(_ context.Context, id string) error {
	return fmt.Errorf("%w: 1Password has no call that restores an archived item; restore the item %s from the 1Password app's Archive", secretstore.ErrNoRestore, id)
}
