// Package secretstoretest holds what every secret store's tests share: StoreContract, the meaning
// of each read the broker and the CLI make of a store, which each store runs against its fake and,
// where it has a live tag, against its live service; and the wrappers a test of the policy or the
// request machine puts around a store to count, hold or fail its calls.
package secretstoretest

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/secretstore"
	"github.com/sjawhar/envoy/internal/testwait"
)

// Fixture is one secret a rig holds, as a writer other than the broker made it.
type Fixture struct {
	// Slug is the secret's name under the prefix.
	Slug string
	Tags map[string]string
	// Value is its current value, "" for a secret with no value.
	Value string
	// OffKey holds it off the agent-secrets key.
	OffKey bool
	// Deleted holds it scheduled for deletion.
	Deleted bool
	// Unfilled holds it as a create waiting for its value: it carries secretstore.CreateLabel and
	// holds no version.
	Unfilled bool
	// Outside holds it at a name that holds the prefix's last segment but does not start with
	// the prefix (on AWS and memory "x"+prefix+slug; on GCP the secret id "x"+id prefix+slug).
	Outside bool
}

// StoreRig is one store with the writer a contract case fills it through.
type StoreRig struct {
	Store  secretstore.Store
	Prefix string
	// Put holds f, or replaces the secret of its slug, as another writer would make it.
	Put func(t *testing.T, f Fixture)
	// Remove removes the secret of slug outright.
	Remove func(t *testing.T, slug string)
	// OffKey is whether Put can hold a secret off the key; false: every listing must answer
	// OnKey true.
	OffKey bool
	// Unfilled is whether Put can hold an Unfilled secret; false: no listing may be Unfilled.
	Unfilled bool
}

// mintPrefix opens a type secret's source in the policy (policy.TypeSource); no store's source may
// start with it, or the policy would read a stored secret as a minted one.
const mintPrefix = "mint:"

// settleBound is how long a case waits for a store's eventually consistent metadata to show a
// write: a fake shows it at once, and a live service within it.
const settleBound = 2 * time.Minute

// settlePoll is how often a case asks a store again while it waits, slow enough that a live
// service's read quota does not notice.
const settlePoll = time.Second

// storeCase is one case of StoreContract.
type storeCase struct {
	name string
	run  func(t *testing.T, r StoreRig)
}

// StoreContract runs every case of the store contract on store, a subtest per case named
// <store>/<case>, each on a rig of its own from open.
func StoreContract(t *testing.T, store string, open func(t *testing.T) StoreRig) {
	t.Helper()
	t.Run(store, func(t *testing.T) {
		for _, c := range storeCases {
			t.Run(c.name, func(t *testing.T) { c.run(t, open(t)) })
		}
	})
}

// fixture is a secret of slug with value, owned by a person at the agent tier.
func fixture(slug, value string) Fixture {
	return Fixture{Slug: slug, Tags: map[string]string{"owner": "ada@example.com", "tier": "agent"}, Value: value}
}

// settle waits until read holds, asking at settlePoll for up to settleBound, and answers what it
// read last; it fails t with what it waited for and that reading when the bound passes.
func settle[T any](t *testing.T, what string, read func() (T, bool)) T {
	t.Helper()
	var last T
	if testwait.Holds(testwait.Bounded(t, settleBound, 30*time.Second), settlePoll, func() bool {
		var ok bool
		last, ok = read()
		return ok
	}) {
		return last
	}
	t.Fatalf("waited %s for %s; last read %+v", settleBound, what, last)
	return last
}

// listed waits until a List under r's prefix with opts answers every slug of slugs, and answers
// that listing by ID.
func listed(t *testing.T, r StoreRig, opts secretstore.ReadOptions, slugs ...string) map[string]secretstore.Listing {
	t.Helper()
	return settle(t, fmt.Sprintf("List(%+v) to answer %v", opts, slugs), func() (map[string]secretstore.Listing, bool) {
		listings, err := r.Store.List(context.Background(), r.Prefix, opts)
		if err != nil {
			t.Fatalf("List(%s, %+v): %v", r.Prefix, opts, err)
		}
		byID := map[string]secretstore.Listing{}
		for _, ls := range listings {
			byID[ls.ID] = ls
		}
		for _, slug := range slugs {
			if _, ok := byID[r.Prefix+slug]; !ok {
				return byID, false
			}
		}
		return byID, true
	})
}

// read is the value the store answers for source, failing t on an error.
func read(t *testing.T, r StoreRig, source string) string {
	t.Helper()
	value, err := r.Store.Read(context.Background(), source)
	if err != nil {
		t.Fatalf("Read(%s): %v", source, err)
	}
	return value
}

// onKey is ls.OnKey's answer, failing t on an error.
func onKey(t *testing.T, ls secretstore.Listing) bool {
	t.Helper()
	on, err := ls.OnKey(context.Background())
	if err != nil {
		t.Fatalf("OnKey(%s): %v", ls.ID, err)
	}
	return on
}

var storeCases = []storeCase{
	{"List answers each secret under the prefix with its tags, value state and source", func(t *testing.T, r StoreRig) {
		fixtures := []Fixture{fixture("deel-api-key", "deel-value"), fixture("other-key", "other-value")}
		for _, f := range fixtures {
			r.Put(t, f)
		}
		byID := listed(t, r, secretstore.ReadOptions{}, "deel-api-key", "other-key")
		if len(byID) != len(fixtures) {
			t.Errorf("List answered %d secrets, want the %d under the prefix: %+v", len(byID), len(fixtures), byID)
		}
		for _, f := range fixtures {
			ls := byID[r.Prefix+f.Slug]
			if !maps.Equal(ls.Tags, f.Tags) {
				t.Errorf("%s: tags %v, want %v", ls.ID, ls.Tags, f.Tags)
			}
			if !ls.HasCurrentValue || ls.Version == "" {
				t.Errorf("%s: HasCurrentValue %t, Version %q; want a current value with a version", ls.ID, ls.HasCurrentValue, ls.Version)
			}
			if got := read(t, r, ls.Source); got != f.Value {
				t.Errorf("Read(%s) answered %d bytes, want the %d of its value", ls.Source, len(got), len(f.Value))
			}
		}
	}},
	{"List leaves out a secret whose name only contains the prefix", func(t *testing.T, r StoreRig) {
		outside := fixture("outside-key", "v")
		outside.Outside = true
		r.Put(t, outside)
		r.Put(t, fixture("inside-key", "v"))
		byID := listed(t, r, secretstore.ReadOptions{}, "inside-key")
		for id := range byID {
			if !strings.HasPrefix(id, r.Prefix) {
				t.Errorf("List(%s) answered %s, which does not start with the prefix", r.Prefix, id)
			}
		}
		if len(byID) != 1 {
			t.Errorf("List answered %d secrets, want the one under the prefix: %+v", len(byID), byID)
		}
	}},
	{"a secret scheduled for deletion is absent unless the read asks for it", func(t *testing.T, r StoreRig) {
		gone := fixture("gone-key", "v")
		gone.Deleted = true
		r.Put(t, gone)
		r.Put(t, fixture("kept-key", "v"))
		id := r.Prefix + gone.Slug
		ctx := context.Background()
		all := listed(t, r, secretstore.ReadOptions{IncludeDeleted: true}, gone.Slug, "kept-key")
		deleted := all[id]
		if deleted.Deleted.IsZero() {
			t.Errorf("List with IncludeDeleted answered %s with no Deleted time", id)
		}
		described, err := r.Store.Describe(ctx, id, secretstore.ReadOptions{IncludeDeleted: true})
		if err != nil || described.Deleted.IsZero() {
			t.Errorf("Describe(%s) with IncludeDeleted = %+v, %v; want it with its Deleted time", id, described, err)
		}
		if ls, held := listed(t, r, secretstore.ReadOptions{}, "kept-key")[id]; held {
			t.Errorf("List answered %s, which is scheduled for deletion: %+v", id, ls)
		}
		if ls, err := r.Store.Describe(ctx, id, secretstore.ReadOptions{}); !errors.Is(err, secretstore.ErrNotFound) {
			t.Errorf("Describe(%s) = %+v, %v; want ErrNotFound for a secret scheduled for deletion", id, ls, err)
		}
		if _, err := r.Store.Read(ctx, deleted.Source); !errors.Is(err, secretstore.ErrNotFound) {
			t.Errorf("Read(%s) = %v; want ErrNotFound for a secret scheduled for deletion", deleted.Source, err)
		}
	}},
	{"a secret with no value has no current value and no version", func(t *testing.T, r StoreRig) {
		r.Put(t, fixture("empty-key", ""))
		ls := listed(t, r, secretstore.ReadOptions{}, "empty-key")[r.Prefix+"empty-key"]
		if ls.HasCurrentValue || ls.Version != "" {
			t.Errorf("%s: HasCurrentValue %t, Version %q; want no value and no version", ls.ID, ls.HasCurrentValue, ls.Version)
		}
		if _, err := r.Store.Read(context.Background(), ls.Source); !errors.Is(err, secretstore.ErrNotFound) {
			t.Errorf("Read(%s) = %v; want ErrNotFound for a secret with no value", ls.Source, err)
		}
	}},
	{"Describe answers what List answers for one name", func(t *testing.T, r StoreRig) {
		r.Put(t, fixture("deel-api-key", "deel-value"))
		ls := listed(t, r, secretstore.ReadOptions{}, "deel-api-key")[r.Prefix+"deel-api-key"]
		d, err := r.Store.Describe(context.Background(), ls.ID, secretstore.ReadOptions{})
		if err != nil {
			t.Fatalf("Describe(%s): %v", ls.ID, err)
		}
		if d.ID != ls.ID || d.Source != ls.Source || !maps.Equal(d.Tags, ls.Tags) || d.HasCurrentValue != ls.HasCurrentValue || d.Version != ls.Version {
			t.Errorf("Describe(%s) = %+v; List answered %+v", ls.ID, d, ls)
		}
	}},
	{"Describe of a name no secret carries is ErrNotFound", func(t *testing.T, r StoreRig) {
		id := r.Prefix + "no-such-key"
		if ls, err := r.Store.Describe(context.Background(), id, secretstore.ReadOptions{}); !errors.Is(err, secretstore.ErrNotFound) {
			t.Errorf("Describe(%s) = %+v, %v; want ErrNotFound", id, ls, err)
		}
	}},
	{"Read of a source the store no longer holds is ErrNotFound", func(t *testing.T, r StoreRig) {
		r.Put(t, fixture("removed-key", "v"))
		source := listed(t, r, secretstore.ReadOptions{}, "removed-key")[r.Prefix+"removed-key"].Source
		r.Remove(t, "removed-key")
		settle(t, "Read("+source+") to answer ErrNotFound once it is removed", func() (error, bool) {
			_, err := r.Store.Read(context.Background(), source)
			return err, errors.Is(err, secretstore.ErrNotFound)
		})
	}},
	{"a new value moves Version", func(t *testing.T, r StoreRig) {
		ctx := context.Background()
		r.Put(t, fixture("deel-api-key", "first-value"))
		id := r.Prefix + "deel-api-key"
		first := listed(t, r, secretstore.ReadOptions{}, "deel-api-key")[id]
		r.Put(t, fixture("deel-api-key", "second-value"))
		settle(t, "Read("+first.Source+") to answer the new value", func() (int, bool) {
			value, err := r.Store.Read(ctx, first.Source)
			return len(value), err == nil && value == "second-value"
		})
		second, err := r.Store.Describe(ctx, id, secretstore.ReadOptions{})
		if err != nil {
			t.Fatalf("Describe(%s): %v", id, err)
		}
		if second.Version == "" || second.Version == first.Version {
			t.Errorf("Version %q after a new value, %q before; want it moved", second.Version, first.Version)
		}
	}},
	{"OnKey answers the agent-secrets key", func(t *testing.T, r StoreRig) {
		r.Put(t, fixture("on-key", "v"))
		slugs := []string{"on-key"}
		if r.OffKey {
			off := fixture("off-key", "v")
			off.OffKey = true
			r.Put(t, off)
			slugs = append(slugs, "off-key")
		}
		byID := listed(t, r, secretstore.ReadOptions{}, slugs...)
		for id, ls := range byID {
			if want := id != r.Prefix+"off-key"; onKey(t, ls) != want {
				t.Errorf("%s: OnKey %t, want %t", id, !want, want)
			}
		}
	}},
	{"Unfilled is a create that waits for its value", func(t *testing.T, r StoreRig) {
		r.Put(t, fixture("plain-empty-key", ""))
		r.Put(t, fixture("plain-key", "v"))
		slugs := []string{"plain-empty-key", "plain-key"}
		if r.Unfilled {
			waiting := fixture("waiting-key", "")
			waiting.Unfilled = true
			r.Put(t, waiting)
			slugs = append(slugs, "waiting-key")
		}
		for id, ls := range listed(t, r, secretstore.ReadOptions{}, slugs...) {
			want := id == r.Prefix+"waiting-key"
			if ls.Unfilled != want {
				t.Errorf("%s: Unfilled %t, want %t", id, ls.Unfilled, want)
			}
			if want && ls.HasCurrentValue {
				t.Errorf("%s: an Unfilled secret answers HasCurrentValue", id)
			}
		}
	}},
	{"Versions answers the current version when asked", func(t *testing.T, r StoreRig) {
		ctx := context.Background()
		r.Put(t, fixture("deel-api-key", "v"))
		id := listed(t, r, secretstore.ReadOptions{}, "deel-api-key")[r.Prefix+"deel-api-key"].ID
		plain, err := r.Store.Describe(ctx, id, secretstore.ReadOptions{})
		if err != nil {
			t.Fatalf("Describe(%s): %v", id, err)
		}
		if plain.Versions != nil {
			t.Errorf("Describe(%s) with no Versions option answered versions %v; want none", id, plain.Versions)
		}
		withVersions, err := r.Store.Describe(ctx, id, secretstore.ReadOptions{Versions: true})
		if err != nil {
			t.Fatalf("Describe(%s) with Versions: %v", id, err)
		}
		if _, ok := withVersions.Versions[withVersions.Version]; withVersions.Version == "" || !ok {
			t.Errorf("Describe(%s) with Versions answered version %q and versions %v; want the version among them", id, withVersions.Version, withVersions.Versions)
		}
	}},
	{"ValidName refuses a name past the store's limit and accepts every listed name", func(t *testing.T, r StoreRig) {
		r.Put(t, fixture("deel-api-key", "v"))
		r.Put(t, fixture("empty-key", ""))
		if err := r.Store.ValidName(r.Prefix + strings.Repeat("a", 10000)); err == nil {
			t.Error("ValidName accepted a name of 10000 bytes under the prefix")
		}
		for id := range listed(t, r, secretstore.ReadOptions{}, "deel-api-key", "empty-key") {
			if err := r.Store.ValidName(id); err != nil {
				t.Errorf("ValidName(%s), a listed name: %v", id, err)
			}
		}
	}},
	{"no source starts with " + mintPrefix, func(t *testing.T, r StoreRig) {
		r.Put(t, fixture("deel-api-key", "v"))
		r.Put(t, fixture("empty-key", ""))
		for id, ls := range listed(t, r, secretstore.ReadOptions{IncludeDeleted: true}, "deel-api-key", "empty-key") {
			if strings.HasPrefix(ls.Source, mintPrefix) {
				t.Errorf("%s: source %q starts with %s, which opens a minted secret's source", id, ls.Source, mintPrefix)
			}
		}
	}},
}
