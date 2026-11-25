// Package onepassword is the secret store over one 1Password vault. Each agent secret is one item
// of the category API Credential: its title is the secret's full name, its value the item's
// built-in credential field, and its tags the text fields of the item's section titled "cipher",
// one field a tag, labelled with the tag's name. The store reads and writes the vault through
// Client, written once over five calls, which the 1Password Go SDK (NewSDKClient) and the op CLI
// (NewOpClient) both implement; its fake is onepasswordtest.Fake.
package onepassword

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"time"
)

// State is an item's state in its vault.
type State string

const (
	// StateActive is an item the vault holds in the open.
	StateActive State = "active"
	// StateArchived is an item in the vault's Archive: a secret scheduled for deletion, which a
	// person restores, or deletes for good, in the 1Password app.
	StateArchived State = "archived"
)

// Overview is one item as a List answers it: what 1Password says of an item without reading its
// fields.
type Overview struct {
	ID, Title string
	State     State
	UpdatedAt time.Time
}

// Item is one item read in full, or one to write.
type Item struct {
	ID, Title string
	// State is the item's state, "" where the client's answer names none: the SDK's Item carries
	// no state, so the store reads it from a List.
	State State
	// Version is 1Password's count of the item's changes, which each write moves.
	Version              uint32
	CreatedAt, UpdatedAt time.Time
	// Credential is the value of the API Credential item's built-in credential field.
	Credential string
	// Cipher is the text fields of the item's section titled "cipher", by label: the secret's
	// tags, and CreateField, the token of the create that made it.
	Cipher map[string]string
	// base is the client's own record of the item as it read it, which Put writes the item's
	// credential and cipher fields onto, so a field Cipher does not own survives the write; nil
	// for an item no client read.
	base any
}

// CreateField is the field of the cipher section a create writes a token of its own into, so a
// create that lost its answer finds its own item when it tries again. It is no tag of the
// secret's.
const CreateField = "create"

// Client is the five calls the 1Password store makes of one vault.
type Client interface {
	// List answers every item of the vault, active and archived, without its fields.
	List(ctx context.Context) ([]Overview, error)
	// Get reads the items ids name in full, in the order of ids, in one call; an id the vault does
	// not hold answers an error wrapping secretstore.ErrNotFound.
	Get(ctx context.Context, ids ...string) ([]Item, error)
	// Create makes item, of the category API Credential, and answers it as the vault holds it.
	Create(ctx context.Context, item Item) (Item, error)
	// Put writes item's title, credential and cipher fields onto the item of its ID, and answers
	// it as the vault holds it.
	Put(ctx context.Context, item Item) (Item, error)
	// Archive moves the item to the vault's Archive.
	Archive(ctx context.Context, id string) error
}

// ErrDenied is what a client's error wraps when 1Password refused the sign-in the call: the
// vault's permissions do not let it read or write there.
var ErrDenied = errors.New("1Password refused this sign-in the call")

// ErrRateLimited is what a client's error wraps when 1Password refused the call at its own
// request limit (the SDK's RateLimitExceeded), before it acted.
var ErrRateLimited = errors.New("1Password refused the call at its request limit")

const (
	// cipherSection is the title of the section whose text fields are a secret's tags, and the
	// id of the one a write adds to an item that has none of that title.
	cipherSection = "cipher"
	// credentialField is the id of the API Credential item's built-in credential field.
	credentialField = "credential"
	// cipherFieldPrefix opens the id of each field a write adds to the cipher section: a field id
	// of its own, since the API Credential item's built-in field ids include type, which is also a
	// tag's name.
	cipherFieldPrefix = "cipher-"
)

// newSectionID is the id a write gives the cipher section it adds to an item whose sections, of
// the ids taken, hold none titled cipher: cipherSection, or, where a section of another title holds
// that id, the first of cipher-2, cipher-3 and on that none does.
func newSectionID(taken []string) string {
	id := cipherSection
	for n := 2; slices.Contains(taken, id); n++ {
		id = cipherSection + "-" + strconv.Itoa(n)
	}
	return id
}

// fieldOps reads and makes one client's fields, for readFields and mergeFields.
type fieldOps[F any] struct {
	// isCredential is whether f is the built-in credential field.
	isCredential func(f F) bool
	// sectionOf is the id of the section f is in, "" for none.
	sectionOf func(f F) string
	// labelOf is f's label.
	labelOf func(f F) string
	// valueOf is f's value.
	valueOf func(f F) string
	// withValue is f with value.
	withValue func(f F, value string) F
	// newCredential is a credential field holding value.
	newCredential func(value string) F
	// newCipher is a text field of the section section, labelled label, holding value.
	newCipher func(section, label, value string) F
}

// readFields answers the value of the credential field among fields, and the label and value of
// each field of a section cipherSections holds, the first field of a label winning.
func readFields[F any](fields []F, cipherSections []string, ops fieldOps[F]) (credential string, cipher map[string]string) {
	credentialSeen := false
	for _, f := range fields {
		switch {
		case ops.isCredential(f) && !credentialSeen:
			credential, credentialSeen = ops.valueOf(f), true
		case slices.Contains(cipherSections, ops.sectionOf(f)):
			if cipher == nil {
				cipher = map[string]string{}
			}
			if _, held := cipher[ops.labelOf(f)]; !held {
				cipher[ops.labelOf(f)] = ops.valueOf(f)
			}
		}
	}
	return credential, cipher
}

// mergeFields writes item's credential and cipher fields onto fields, an item's fields as a client
// read them. The credential field takes item.Credential; a field of a section cipherSections holds
// takes the value item.Cipher holds for its label, and one whose label it does not hold, or a
// second field of one label, is dropped; every other field is kept as it is. A credential no
// field held is added, and so is a field for each label of item.Cipher no field held, in the order
// of the labels, in section, the item's first cipher section.
func mergeFields[F any](fields []F, cipherSections []string, section string, item Item, ops fieldOps[F]) []F {
	merged := make([]F, 0, len(fields)+len(item.Cipher)+1)
	credentialSeen, written := false, map[string]bool{}
	for _, f := range fields {
		switch {
		case ops.isCredential(f) && !credentialSeen:
			merged, credentialSeen = append(merged, ops.withValue(f, item.Credential)), true
		case slices.Contains(cipherSections, ops.sectionOf(f)):
			label := ops.labelOf(f)
			if value, ok := item.Cipher[label]; ok && !written[label] {
				merged, written[label] = append(merged, ops.withValue(f, value)), true
			}
		default:
			merged = append(merged, f)
		}
	}
	if !credentialSeen {
		merged = append(merged, ops.newCredential(item.Credential))
	}
	labels := make([]string, 0, len(item.Cipher))
	for label := range item.Cipher {
		if !written[label] {
			labels = append(labels, label)
		}
	}
	slices.Sort(labels)
	for _, label := range labels {
		merged = append(merged, ops.newCipher(section, label, item.Cipher[label]))
	}
	return merged
}
