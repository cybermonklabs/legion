//go:build darwin || linux || windows

// The 1Password Go SDK's client: built for darwin, linux and windows alone, since with
// CGO_ENABLED=0 the SDK compiles for those three only (at v0.4.1 its client_builder.go calls
// internal.GetSharedLibCore, which internal/shared_lib_core.go defines under cgo || windows and
// internal/shared_lib_core_no_cgo.go under !cgo && (darwin || linux)). sdk_other.go stands in for
// it everywhere else, where agent-secrets runs op.

package onepassword

import (
	"context"
	"errors"
	"fmt"
	"slices"

	sdk "github.com/1password/onepassword-sdk-go"

	"github.com/sjawhar/envoy/internal/buildversion"
	"github.com/sjawhar/envoy/internal/cipher/secretstore"
)

// sdkSupported is whether NewSDKClient can start the SDK in this build.
const sdkSupported = true

// integrationName is the name the SDK client gives 1Password for its calls, beside the build's
// version (buildversion.String).
const integrationName = "Cipher"

// sdkClient is Client over the 1Password SDK, on one vault. client holds the SDK's Client itself,
// not only its Items: the SDK releases its core's client when that value is collected.
type sdkClient struct {
	client *sdk.Client
	items  sdk.ItemsAPI
	vault  string
}

// NewSDKClient starts the 1Password SDK signed in with the service account token, on vault. The
// SDK runs its core as WebAssembly in this process (extism on wazero), and its start may call
// 1Password.
func NewSDKClient(ctx context.Context, token, vault string) (Client, error) {
	client, err := sdk.NewClient(ctx, sdk.WithServiceAccountToken(token), sdk.WithIntegrationInfo(integrationName, buildversion.String()))
	if err != nil {
		return nil, fmt.Errorf("start the 1Password SDK: %w", sdkError(err))
	}
	return sdkClient{client: client, items: client.Items(), vault: vault}, nil
}

// sdkError marks err, an SDK call's error, with the class this package reads: ErrRateLimited for
// the SDK's RateLimitExceededError. Its text stays the SDK's.
func sdkError(err error) error {
	var limited *sdk.RateLimitExceededError
	if errors.As(err, &limited) {
		return fmt.Errorf("%w: %w", ErrRateLimited, err)
	}
	return err
}

// List lists the vault's active and archived items.
func (c sdkClient) List(ctx context.Context) ([]Overview, error) {
	listed, err := c.items.List(ctx, c.vault, sdk.NewItemListFilterTypeVariantByState(&sdk.ItemListFilterByStateInner{Active: true, Archived: true}))
	if err != nil {
		return nil, fmt.Errorf("1Password ItemsList: %w", sdkError(err))
	}
	overviews := make([]Overview, 0, len(listed))
	for _, o := range listed {
		state, err := sdkState(o.State)
		if err != nil {
			return nil, fmt.Errorf("1Password ItemsList answered item %s %w", o.ID, err)
		}
		overviews = append(overviews, Overview{ID: o.ID, Title: o.Title, State: state, UpdatedAt: o.UpdatedAt})
	}
	return overviews, nil
}

// sdkState is the State of an SDK ItemState, and an error for one this package does not know.
func sdkState(state sdk.ItemState) (State, error) {
	switch state {
	case sdk.ItemStateActive:
		return StateActive, nil
	case sdk.ItemStateArchived:
		return StateArchived, nil
	}
	return "", fmt.Errorf("in state %q, which this build does not know", state)
}

// Get reads the items with one GetAll, whatever their number: GetAll answers each item that is
// missing with an itemNotFound of its own, which the SDK's Get answers only as a message.
func (c sdkClient) Get(ctx context.Context, ids ...string) ([]Item, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	answer, err := c.items.GetAll(ctx, c.vault, ids)
	if err != nil {
		return nil, fmt.Errorf("1Password ItemsGetAll: %w", sdkError(err))
	}
	if len(answer.IndividualResponses) != len(ids) {
		return nil, fmt.Errorf("1Password ItemsGetAll answered %d items for %d ids", len(answer.IndividualResponses), len(ids))
	}
	items := make([]Item, 0, len(ids))
	for i, r := range answer.IndividualResponses {
		switch {
		case r.Error != nil && r.Error.Type == sdk.ItemsGetAllErrorTypeVariantItemNotFound:
			return nil, fmt.Errorf("1Password has no item %s in vault %s: %w", ids[i], c.vault, secretstore.ErrNotFound)
		case r.Error != nil:
			return nil, fmt.Errorf("1Password ItemsGetAll failed for item %s: %s", ids[i], r.Error.Type)
		case r.Content == nil:
			return nil, fmt.Errorf("1Password ItemsGetAll answered nothing for item %s", ids[i])
		}
		items = append(items, sdkItem(*r.Content))
	}
	return items, nil
}

// Create makes item as an API Credential item of the vault.
func (c sdkClient) Create(ctx context.Context, item Item) (Item, error) {
	made, err := c.items.Create(ctx, sdkCreateParams(c.vault, item))
	if err != nil {
		return Item{}, fmt.Errorf("1Password ItemsCreate: %w", sdkError(err))
	}
	return sdkItem(made), nil
}

// Put writes item onto the SDK item it was read as (sdkPutItem).
func (c sdkClient) Put(ctx context.Context, item Item) (Item, error) {
	put, err := c.items.Put(ctx, sdkPutItem(c.vault, item))
	if err != nil {
		return Item{}, fmt.Errorf("1Password ItemsPut: %w", sdkError(err))
	}
	return sdkItem(put), nil
}

// Archive moves the item to the vault's Archive.
func (c sdkClient) Archive(ctx context.Context, id string) error {
	if err := c.items.Archive(ctx, c.vault, id); err != nil {
		return fmt.Errorf("1Password ItemsArchive: %w", sdkError(err))
	}
	return nil
}

// sdkFields reads and makes the SDK's fields.
var sdkFields = fieldOps[sdk.ItemField]{
	isCredential: func(f sdk.ItemField) bool { return f.ID == credentialField && sdkSection(f) == "" },
	sectionOf:    sdkSection,
	labelOf:      func(f sdk.ItemField) string { return f.Title },
	valueOf:      func(f sdk.ItemField) string { return f.Value },
	withValue: func(f sdk.ItemField, value string) sdk.ItemField {
		f.Value = value
		return f
	},
	newCredential: func(value string) sdk.ItemField {
		return sdk.ItemField{ID: credentialField, Title: credentialField, FieldType: sdk.ItemFieldTypeConcealed, Value: value}
	},
	newCipher: func(section, label, value string) sdk.ItemField {
		return sdk.ItemField{ID: cipherFieldPrefix + label, Title: label, SectionID: &section, FieldType: sdk.ItemFieldTypeText, Value: value}
	},
}

// sdkSection is the id of the section f is in, "" for none.
func sdkSection(f sdk.ItemField) string {
	if f.SectionID == nil {
		return ""
	}
	return *f.SectionID
}

// sdkCipherSections is the ids of the sections titled cipherSection.
func sdkCipherSections(sections []sdk.ItemSection) []string {
	var ids []string
	for _, s := range sections {
		if s.Title == cipherSection {
			ids = append(ids, s.ID)
		}
	}
	return ids
}

// sdkItem is the Item an SDK item is, with the SDK item as its base. Its State is "": the SDK's
// Item carries none.
func sdkItem(item sdk.Item) Item {
	credential, cipher := readFields(item.Fields, sdkCipherSections(item.Sections), sdkFields)
	return Item{
		ID: item.ID, Title: item.Title, Version: item.Version, CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt,
		Credential: credential, Cipher: cipher, base: item,
	}
}

// sdkCreateParams is the create of item as an API Credential item of vault: its credential, and
// a section titled cipher holding its cipher fields.
func sdkCreateParams(vault string, item Item) sdk.ItemCreateParams {
	sections, fields := sdkWrite(nil, nil, item)
	return sdk.ItemCreateParams{Category: sdk.ItemCategoryAPICredentials, VaultID: vault, Title: item.Title, Sections: sections, Fields: fields}
}

// sdkPutItem is the SDK item a Put of item sends: the SDK item item was read as (an API
// Credential item of vault with item's id where none was), with item's title and Version, and its
// credential and cipher fields written over that item's own (mergeFields).
func sdkPutItem(vault string, item Item) sdk.Item {
	base, ok := item.base.(sdk.Item)
	if !ok {
		base = sdk.Item{ID: item.ID, Category: sdk.ItemCategoryAPICredentials, VaultID: vault}
	}
	base.Title, base.Version = item.Title, item.Version
	base.Sections, base.Fields = sdkWrite(base.Sections, base.Fields, item)
	return base
}

// sdkWrite is sections and fields with item's credential and cipher fields written over them,
// with a section titled cipher added where item has cipher fields and sections none.
func sdkWrite(sections []sdk.ItemSection, fields []sdk.ItemField, item Item) ([]sdk.ItemSection, []sdk.ItemField) {
	ids := sdkCipherSections(sections)
	if len(ids) == 0 && len(item.Cipher) > 0 {
		taken := make([]string, 0, len(sections))
		for _, s := range sections {
			taken = append(taken, s.ID)
		}
		id := newSectionID(taken)
		sections = append(slices.Clone(sections), sdk.ItemSection{ID: id, Title: cipherSection})
		ids = []string{id}
	}
	section := ""
	if len(ids) > 0 {
		section = ids[0]
	}
	return sections, mergeFields(fields, ids, section, item, sdkFields)
}
