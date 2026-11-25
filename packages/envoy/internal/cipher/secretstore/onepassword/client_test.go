//go:build darwin || linux || windows

package onepassword

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	sdk "github.com/1password/onepassword-sdk-go"

	"github.com/sjawhar/envoy/internal/cipher/secretstore"
)

// testVault is the vault id the client tests write to: 26 lowercase letters and digits, as
// 1Password's ids are, and no real vault's.
const testVault = "vault0000000000000000000aa"

// served is the item the SDK answers for params, as the service makes it: an id, a first
// version and the times of its creation.
func served(params sdk.ItemCreateParams, at time.Time) sdk.Item {
	return sdk.Item{
		ID: "item000000000000000000000a", Title: params.Title, Category: params.Category, VaultID: params.VaultID,
		Fields: params.Fields, Sections: params.Sections, Version: 1, CreatedAt: at, UpdatedAt: at,
	}
}

// TestSDKItemRoundTrips: an Item mapped into the SDK's ItemCreateParams and back, as the service
// answers the create, is the same Item; and an Item read from the SDK, changed, and mapped into
// the SDK Item a Put sends, reads back as the changed Item, with every field Cipher does not own
// kept as it was: the built-in username and type fields and a field of another section.
func TestSDKItemRoundTrips(t *testing.T) {
	at := time.Date(2026, 11, 23, 9, 30, 0, 0, time.UTC)
	made := Item{
		Title:      "cipher-deel-api-key",
		Credential: "deel-value",
		Cipher:     map[string]string{"owner": "ada@example.com", "tier": "agent", "type": "github-app", CreateField: "0123456789abcdef0123456789abcdef"},
	}
	params := sdkCreateParams(testVault, made)
	if params.Category != sdk.ItemCategoryAPICredentials || params.VaultID != testVault {
		t.Fatalf("create params category %q vault %q, want %q in %q", params.Category, params.VaultID, sdk.ItemCategoryAPICredentials, testVault)
	}
	back := sdkItem(served(params, at))
	want := made
	want.ID, want.Version, want.CreatedAt, want.UpdatedAt = "item000000000000000000000a", 1, at, at
	if got := withoutBase(back); !reflect.DeepEqual(got, want) {
		t.Fatalf("an Item through ItemCreateParams and back:\n got %+v\nwant %+v", got, want)
	}

	other, cipher := "Section_m4k2p7q9", "Section_c1p2h3e4"
	read := sdk.Item{
		ID: "item000000000000000000000b", Title: "cipher-other-key", Category: sdk.ItemCategoryAPICredentials, VaultID: testVault,
		Version: 4, CreatedAt: at, UpdatedAt: at.Add(time.Hour),
		Sections: []sdk.ItemSection{{ID: other, Title: "notes"}, {ID: cipher, Title: "cipher"}},
		Fields: []sdk.ItemField{
			{ID: "username", Title: "username", FieldType: sdk.ItemFieldTypeText, Value: "deploy-bot"},
			{ID: "credential", Title: "credential", FieldType: sdk.ItemFieldTypeConcealed, Value: "old-value"},
			{ID: "type", Title: "type", FieldType: sdk.ItemFieldTypeMenu, Value: "bearer"},
			{ID: "kept", Title: "kept", SectionID: &other, FieldType: sdk.ItemFieldTypeText, Value: "a person's note"},
			{ID: "owner-field", Title: "owner", SectionID: &cipher, FieldType: sdk.ItemFieldTypeText, Value: "ada@example.com"},
			{ID: "tier-field", Title: "tier", SectionID: &cipher, FieldType: sdk.ItemFieldTypeText, Value: "agent"},
		},
	}
	item := sdkItem(read)
	if want := map[string]string{"owner": "ada@example.com", "tier": "agent"}; item.Credential != "old-value" || !reflect.DeepEqual(item.Cipher, want) {
		t.Fatalf("read credential %q cipher %v, want %q and %v", item.Credential, item.Cipher, "old-value", want)
	}
	item.Credential = "new-value"
	item.Cipher = map[string]string{"owner": "grace@example.com", "requesters": "ada@example.com"}
	put := sdkPutItem(testVault, item)
	if put.Version != 4 || put.ID != read.ID {
		t.Errorf("the Put's item is %s at version %d, want %s at the held version 4", put.ID, put.Version, read.ID)
	}
	for _, id := range []string{"username", "type", "kept"} {
		if !hasField(put, id, read) {
			t.Errorf("the Put's item lost or changed field %s, which Cipher does not own: %+v", id, put.Fields)
		}
	}
	got := withoutBase(sdkItem(put))
	want = withoutBase(item)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("a changed Item through the Put's SDK Item and back:\n got %+v\nwant %+v", got, want)
	}
}

// hasField reports whether put holds the field id exactly as read held it.
func hasField(put sdk.Item, id string, read sdk.Item) bool {
	for _, before := range read.Fields {
		if before.ID != id {
			continue
		}
		for _, after := range put.Fields {
			if reflect.DeepEqual(after, before) {
				return true
			}
		}
	}
	return false
}

// fakeItems is the SDK's ItemsAPI with GetAll answering each id its Items hold and itemNotFound
// for any other, and every other call unimplemented.
type fakeItems struct {
	sdk.ItemsAPI
	items map[string]sdk.Item
}

func (f fakeItems) GetAll(_ context.Context, _ string, ids []string) (sdk.ItemsGetAllResponse, error) {
	var answer sdk.ItemsGetAllResponse
	for _, id := range ids {
		if item, ok := f.items[id]; ok {
			answer.IndividualResponses = append(answer.IndividualResponses, sdk.Response[sdk.Item, sdk.ItemsGetAllError]{Content: &item})
			continue
		}
		answer.IndividualResponses = append(answer.IndividualResponses, sdk.Response[sdk.Item, sdk.ItemsGetAllError]{Error: &sdk.ItemsGetAllError{Type: sdk.ItemsGetAllErrorTypeVariantItemNotFound}})
	}
	return answer, nil
}

// TestSDKGetOfAMissingItemIsNotFound: the SDK client's Get answers each item in the order of its
// ids, and secretstore.ErrNotFound, naming the id, when GetAll answers itemNotFound for one.
func TestSDKGetOfAMissingItemIsNotFound(t *testing.T) {
	held := sdk.Item{ID: "item000000000000000000000a", Title: "cipher-deel-api-key", Version: 2}
	client := sdkClient{items: fakeItems{items: map[string]sdk.Item{held.ID: held}}, vault: testVault}
	items, err := client.Get(context.Background(), held.ID)
	if err != nil || len(items) != 1 || items[0].ID != held.ID || items[0].Version != 2 {
		t.Fatalf("Get(%s) = %+v, %v; want the held item", held.ID, items, err)
	}
	missing := "item000000000000000000000z"
	_, err = client.Get(context.Background(), held.ID, missing)
	if !errors.Is(err, secretstore.ErrNotFound) {
		t.Fatalf("Get(%s, %s) = %v, want ErrNotFound", held.ID, missing, err)
	}
	if want := "1Password has no item " + missing + " in vault " + testVault + ": " + secretstore.ErrNotFound.Error(); err.Error() != want {
		t.Errorf("Get's error is %q, want %q", err, want)
	}
}
