package onepassword

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"maps"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/secretstore"
)

// PrepareCreate is the create of id with tags. 1Password lets two items hold one title, so a
// create cannot leave the refusal of a second one to the vault: it writes a token of its own, 16
// random bytes in hex, into the cipher section's create field, by which it knows its own item
// when an answer was lost.
func (w *Writer) PrepareCreate(id string, tags map[string]string) secretstore.Create {
	var b [16]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never fails (its documentation, Go 1.24 and on)
	return &create{w: w, id: id, tags: maps.Clone(tags), token: hex.EncodeToString(b[:])}
}

// create is one 1Password create: Begin refuses a held title before the value is read, and Store
// makes the item, value and all.
type create struct {
	w     *Writer
	id    string
	tags  map[string]string
	token string
	// lost is set when Store's answer was lost and the read after it could not settle what the
	// vault holds, so Unsettled names what to check.
	lost bool
}

// held answers this create's own item among items, an earlier try's whose answer was lost, or the
// refusal of a title another item holds: an active one, or an archived one, which a person
// restores, or deletes for good, in the 1Password app.
func (c *create) held(items []Item) (own *Item, err error) {
	for i := range items {
		if items[i].Cipher[CreateField] == c.token {
			return &items[i], nil
		}
	}
	for _, item := range items {
		if item.State == StateActive {
			return nil, secretstore.Failure{Call: "create", Words: c.id + " is already an item of vault " + c.w.vault}
		}
	}
	if len(items) > 0 {
		return nil, secretstore.Failure{Call: "create", Words: c.id + " is an archived item of vault " + c.w.vault + "; restore it, or delete it for good, from the 1Password app's Archive"}
	}
	return nil, nil
}

// Begin refuses a title the vault holds, before the value is read; it makes nothing.
func (c *create) Begin(ctx context.Context) (secretstore.DiscardOutcome, error) {
	ctx, cancel := secretstore.WithCallDeadline(ctx)
	defer cancel()
	items, err := c.w.titled(ctx, c.id)
	if err != nil {
		return secretstore.MadeNothing, err
	}
	_, err = c.held(items)
	return secretstore.MadeNothing, err
}

// Store reads the items of the title, finishes with this create's own item where it finds one,
// refuses a title another item holds, and otherwise makes the item with value, under
// secretstore.CallDeadline. A Create that 1Password answered with no definite refusal may have
// made the item, so the create reads the title once more, after waiting the store's ListLag where
// the deadline leaves room for it, and finishes with its own item when it finds it; that read can
// make no second item, and where it cannot settle what the vault holds the error stands and
// Unsettled names the command that checks.
func (c *create) Store(ctx context.Context, value string) error {
	ctx, cancel := secretstore.WithCallDeadline(ctx)
	defer cancel()
	items, err := c.w.titled(ctx, c.id)
	if err != nil {
		return err
	}
	own, err := c.held(items)
	if err != nil {
		return err
	}
	if own != nil {
		return c.finish(ctx, *own, value)
	}
	cipher := maps.Clone(c.tags)
	if cipher == nil {
		cipher = map[string]string{}
	}
	cipher[CreateField] = c.token
	_, err = c.w.client.Create(ctx, Item{Title: c.id, Credential: value, Cipher: cipher})
	if err == nil {
		return nil
	}
	made := failure("create "+c.id+" in 1Password vault "+c.w.vault, err)
	if definiteRefusal(err) {
		return made
	}
	if lag := c.w.ListLag(); lag > 0 {
		deadline, _ := ctx.Deadline()
		if time.Until(deadline) <= lag {
			return c.unsettled(made)
		}
		select {
		case <-time.After(lag):
		case <-ctx.Done():
			return c.unsettled(made)
		}
	}
	items, err = c.w.titled(ctx, c.id)
	if err != nil {
		return c.unsettled(made)
	}
	for _, item := range items {
		if item.Cipher[CreateField] == c.token {
			return c.finish(ctx, item, value)
		}
	}
	return made
}

// unsettled is made, the failure of a Create whose outcome the read after it could not settle,
// naming what the vault may hold and the command that says which (Unsettled): a store whose create
// holds the value says it in the create's own error, which the CLI prints.
func (c *create) unsettled(made error) error {
	c.lost = true
	return fmt.Errorf("%w; %s", made, c.Unsettled())
}

// finish completes this create's own item: it writes value where the item holds another.
func (c *create) finish(ctx context.Context, own Item, value string) error {
	if own.Credential == value {
		return nil
	}
	own.Credential = value
	return c.w.put(ctx, "write "+c.id+"'s value to 1Password vault "+c.w.vault, own)
}

// Discard has nothing to remove: Begin made nothing.
func (c *create) Discard(context.Context) (secretstore.DiscardOutcome, error) {
	return secretstore.MadeNothing, nil
}

// Unsettled names what a create whose answer was lost, and could not be settled, may have left,
// and the command that says which.
func (c *create) Unsettled() string {
	if !c.lost {
		return ""
	}
	name := policy.DisplayName(c.w.prefix, c.id)
	return "1Password vault " + c.w.vault + " may hold " + name + ", made with its value, or no " + name + " at all; `" + secretstore.Command(c.w.RerunPrefix(), "secret", "show", name) + "` says which"
}

// MadeBeforeValue is false: a 1Password create makes the item with its value.
func (c *create) MadeBeforeValue() bool { return false }
