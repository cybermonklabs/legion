package secretstoretest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"maps"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/secretstore"
)

// WriterRig is one store's writer with the hooks a writer contract case needs.
type WriterRig struct {
	Writer secretstore.Writer
	Prefix string
	// LoseNextAnswer makes the next call of kind call ("create", "set", "tags", "delete" or
	// "restore") apply and then lose its answer, as a connection that drops after the store acted.
	LoseNextAnswer func(call string)
	// DenyNext makes the next call of kind call refused as access denied, the service's own text
	// being message.
	DenyNext func(call, message string)
	// Retag is another writer's tag write: it sets tags on the secret id, keeping its other tags.
	Retag func(t *testing.T, id string, tags map[string]string)
}

// writerCase is one case of WriterContract.
type writerCase struct {
	name string
	run  func(t *testing.T, r WriterRig)
}

// WriterContract runs every case of the writer contract on store, a subtest per case named
// <store>/<case>, each on a rig of its own from open.
func WriterContract(t *testing.T, store string, open func(t *testing.T) WriterRig) {
	t.Helper()
	t.Run(store, func(t *testing.T) {
		for _, c := range writerCases {
			t.Run(c.name, func(t *testing.T) { c.run(t, open(t)) })
		}
	})
}

// agentTags are the tags a contract case creates a secret with.
func agentTags() map[string]string {
	return map[string]string{"owner": "ada@example.com", "tier": "agent"}
}

// create runs a whole create of id with value through w, failing t on any error.
func create(t *testing.T, w secretstore.Writer, id, value string) {
	t.Helper()
	ctx := context.Background()
	c := w.PrepareCreate(id, agentTags())
	if _, err := c.Begin(ctx); err != nil {
		t.Fatalf("create %s: Begin: %v", id, err)
	}
	if err := c.Store(ctx, value); err != nil {
		t.Fatalf("create %s: Store: %v", id, err)
	}
}

// readValue is the current value of the secret id, failing t when it cannot be read. It reads by
// Describe, which every store answers read-after-write consistent, so it waits for no list lag.
func readValue(t *testing.T, w secretstore.Writer, id string) string {
	t.Helper()
	ctx := context.Background()
	ls, err := w.Describe(ctx, id, secretstore.ReadOptions{})
	if err != nil || !ls.HasCurrentValue {
		t.Fatalf("Describe(%s) = %+v, %v; want it with a current value", id, ls, err)
	}
	value, err := w.Read(ctx, ls.Source)
	if err != nil {
		t.Fatalf("Read(%s): %v", ls.Source, err)
	}
	return value
}

// held is how many secrets of id the store holds, those scheduled for deletion included. A store
// whose names are not unique lists a name more than one secret holds once, with Duplicates naming
// each (secretstore.Listing.Duplicates), so that listing counts each secret it names.
func held(t *testing.T, w secretstore.Writer, prefix, id string) int {
	t.Helper()
	listings, err := w.List(context.Background(), prefix, secretstore.ReadOptions{IncludeDeleted: true})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	n := 0
	for _, ls := range listings {
		if ls.ID == id {
			n += max(1, len(ls.Duplicates))
		}
	}
	return n
}

// refuseUnheld is a TagCheck that refuses a tag value the held read did not hold, as the CLI's
// retag guard refuses a race.
func refuseUnheld(held map[string]string, set map[string]string) secretstore.TagCheck {
	return func(after map[string]string) error {
		for k, v := range after {
			if _, ours := set[k]; ours {
				continue
			}
			if held[k] != v {
				return secretstore.ErrTagsChanged
			}
		}
		return nil
	}
}

// canary is a random word no store's own text holds.
func canary(t *testing.T) string {
	t.Helper()
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	return "canary-" + hex.EncodeToString(b[:])
}

var writerCases = []writerCase{
	{"a create stores its first value with its tags", func(t *testing.T, r WriterRig) {
		id := r.Prefix + "deel-api-key"
		create(t, r.Writer, id, "first-value")
		if got := readValue(t, r.Writer, id); got != "first-value" {
			t.Errorf("Read after create answered %d bytes, want the created value", len(got))
		}
		ls, err := r.Writer.Describe(context.Background(), id, secretstore.ReadOptions{})
		if err != nil || !maps.Equal(ls.Tags, agentTags()) {
			t.Errorf("Describe(%s) = %+v, %v; want the created tags %v", id, ls.Tags, err, agentTags())
		}
	}},
	{"a create of a held name is refused and changes nothing", func(t *testing.T, r WriterRig) {
		ctx := context.Background()
		id := r.Prefix + "deel-api-key"
		create(t, r.Writer, id, "first-value")
		c := r.Writer.PrepareCreate(id, map[string]string{"owner": "eve@example.com", "tier": "human"})
		_, beginErr := c.Begin(ctx)
		var storeErr error
		if beginErr == nil {
			storeErr = c.Store(ctx, "second-value")
		}
		if beginErr == nil && storeErr == nil {
			t.Fatal("a second create of a held name answered no error")
		}
		if n := held(t, r.Writer, r.Prefix, id); n != 1 {
			t.Errorf("the store holds %d secrets named %s, want 1", n, id)
		}
		if got := readValue(t, r.Writer, id); got != "first-value" {
			t.Errorf("the refused create changed the value: it reads %d bytes", len(got))
		}
		if ls, err := r.Writer.Describe(ctx, id, secretstore.ReadOptions{}); err != nil || !maps.Equal(ls.Tags, agentTags()) {
			t.Errorf("the refused create changed the tags: %v, %v", ls.Tags, err)
		}
	}},
	{"a create of a name scheduled for deletion is refused", func(t *testing.T, r WriterRig) {
		ctx := context.Background()
		id := r.Prefix + "deel-api-key"
		create(t, r.Writer, id, "first-value")
		if _, err := r.Writer.Delete(ctx, id); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		c := r.Writer.PrepareCreate(id, agentTags())
		_, beginErr := c.Begin(ctx)
		var storeErr error
		if beginErr == nil {
			storeErr = c.Store(ctx, "second-value")
		}
		if beginErr == nil && storeErr == nil {
			t.Fatal("a create of a name scheduled for deletion answered no error")
		}
		if n := held(t, r.Writer, r.Prefix, id); n != 1 {
			t.Errorf("the store holds %d secrets named %s, want the 1 scheduled for deletion", n, id)
		}
	}},
	// A create whose answer is lost never leaves two secrets of its name. It may leave one - then
	// either it answered nil and its value reads back, or it answered an error naming what is left
	// (Unsettled) - or none, where a store that makes the secret before its value read its own
	// empty secret back and removed it, which it answers as an error: there is no create to finish.
	{"a create whose answer is lost makes no second secret", func(t *testing.T, r WriterRig) {
		ctx := context.Background()
		id := r.Prefix + "deel-api-key"
		r.LoseNextAnswer("create")
		c := r.Writer.PrepareCreate(id, agentTags())
		_, err := c.Begin(ctx)
		if err == nil {
			err = c.Store(ctx, "first-value")
		}
		n := held(t, r.Writer, r.Prefix, id)
		switch {
		case n > 1:
			t.Fatalf("after a create whose answer was lost the store holds %d secrets named %s, want at most 1", n, id)
		case n == 0 && err == nil:
			t.Fatalf("the create answered nil and the store holds no secret named %s", id)
		case n == 1 && err == nil:
			if got := readValue(t, r.Writer, id); got != "first-value" {
				t.Errorf("the create answered nil and its value reads %d bytes", len(got))
			}
		case n == 1 && c.Unsettled() == "":
			t.Errorf("the create answered %v, left %s, and names nothing unsettled", err, id)
		}
	}},
	{"a discard after begin leaves nothing", func(t *testing.T, r WriterRig) {
		ctx := context.Background()
		id := r.Prefix + "deel-api-key"
		c := r.Writer.PrepareCreate(id, agentTags())
		if _, err := c.Begin(ctx); err != nil {
			t.Fatalf("Begin: %v", err)
		}
		outcome, err := c.Discard(ctx)
		want := secretstore.MadeNothing
		if c.MadeBeforeValue() {
			want = secretstore.DeletedEmpty
		}
		if err != nil || outcome != want {
			t.Fatalf("Discard = %v, %v; want %v", outcome, err, want)
		}
		if ls, err := r.Writer.Describe(ctx, id, secretstore.ReadOptions{}); !errors.Is(err, secretstore.ErrNotFound) {
			t.Errorf("after the discard Describe(%s) = %+v, %v; want ErrNotFound", id, ls, err)
		}
	}},
	{"Set writes a new value and moves Version", func(t *testing.T, r WriterRig) {
		ctx := context.Background()
		id := r.Prefix + "deel-api-key"
		create(t, r.Writer, id, "first-value")
		before, err := r.Writer.Describe(ctx, id, secretstore.ReadOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if err := r.Writer.Set(ctx, id, "second-value"); err != nil {
			t.Fatalf("Set: %v", err)
		}
		if got := readValue(t, r.Writer, id); got != "second-value" {
			t.Errorf("Read after Set answered %d bytes, want the new value", len(got))
		}
		after, err := r.Writer.Describe(ctx, id, secretstore.ReadOptions{})
		if err != nil || after.Version == "" || after.Version == before.Version {
			t.Errorf("Version %q after Set, %q before (%v); want it moved", after.Version, before.Version, err)
		}
	}},
	{"a tag write keeps the other tags", func(t *testing.T, r WriterRig) {
		ctx := context.Background()
		id := r.Prefix + "deel-api-key"
		create(t, r.Writer, id, "first-value")
		heldTags, err := r.Writer.CurrentTags(ctx, id)
		if err != nil {
			t.Fatalf("CurrentTags: %v", err)
		}
		if value, err := r.Writer.Read(ctx, heldTags.Source); err != nil || value != "first-value" {
			t.Errorf("Read(HeldTags.Source) = %d bytes, %v; want the value", len(value), err)
		}
		set := map[string]string{"judge": "600"}
		if err := r.Writer.SetTags(ctx, id, heldTags, set, refuseUnheld(heldTags.Tags, set)); err != nil {
			t.Fatalf("SetTags: %v", err)
		}
		heldTags, err = r.Writer.CurrentTags(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		want := agentTags()
		want["judge"] = "600"
		if !maps.Equal(heldTags.Tags, want) {
			t.Fatalf("after SetTags the tags are %v, want %v", heldTags.Tags, want)
		}
		if err := r.Writer.RemoveTags(ctx, id, heldTags, []string{"judge"}, refuseUnheld(heldTags.Tags, nil)); err != nil {
			t.Fatalf("RemoveTags: %v", err)
		}
		if after, err := r.Writer.CurrentTags(ctx, id); err != nil || !maps.Equal(after.Tags, agentTags()) {
			t.Errorf("after RemoveTags the tags are %v, %v; want %v", after.Tags, err, agentTags())
		}
	}},
	{"a raced tag write says so", func(t *testing.T, r WriterRig) {
		ctx := context.Background()
		id := r.Prefix + "deel-api-key"
		create(t, r.Writer, id, "first-value")
		heldTags, err := r.Writer.CurrentTags(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		r.Retag(t, id, map[string]string{"tier": "human"})
		set := map[string]string{"judge": "600"}
		err = r.Writer.SetTags(ctx, id, heldTags, set, refuseUnheld(heldTags.Tags, set))
		if !errors.Is(err, secretstore.ErrTagsChanged) {
			t.Fatalf("SetTags after another writer's retag = %v, want ErrTagsChanged", err)
		}
		after, readErr := r.Writer.CurrentTags(ctx, id)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if _, landed := after.Tags["judge"]; landed && !secretstore.Landed(err) {
			t.Errorf("the raced write landed (%v) and its error does not say so", after.Tags)
		}
	}},
	{"a tag write whose answer is lost is unknown", func(t *testing.T, r WriterRig) {
		ctx := context.Background()
		id := r.Prefix + "deel-api-key"
		create(t, r.Writer, id, "first-value")
		heldTags, err := r.Writer.CurrentTags(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		r.LoseNextAnswer("tags")
		set := map[string]string{"judge": "600"}
		err = r.Writer.SetTags(ctx, id, heldTags, set, refuseUnheld(heldTags.Tags, set))
		if err == nil || !secretstore.Unknown(err) || r.Writer.DefiniteRefusal(err) {
			t.Errorf("SetTags whose answer was lost = %v; want an Unknown error that is no definite refusal", err)
		}
	}},
	{"a delete hides the secret and a restore brings it back", func(t *testing.T, r WriterRig) {
		ctx := context.Background()
		id := r.Prefix + "deel-api-key"
		create(t, r.Writer, id, "first-value")
		if _, err := r.Writer.Delete(ctx, id); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		if ls, err := r.Writer.Describe(ctx, id, secretstore.ReadOptions{}); !errors.Is(err, secretstore.ErrNotFound) {
			t.Fatalf("after Delete Describe(%s) = %+v, %v; want ErrNotFound", id, ls, err)
		}
		err := r.Writer.Restore(ctx, id)
		if errors.Is(err, secretstore.ErrNoRestore) {
			if _, still := r.Writer.Describe(ctx, id, secretstore.ReadOptions{IncludeDeleted: true}); still != nil {
				t.Errorf("a store with no restore lost the deleted secret: %v", still)
			}
			return
		}
		if err != nil {
			t.Fatalf("Restore: %v", err)
		}
		if got := readValue(t, r.Writer, id); got != "first-value" {
			t.Errorf("after Restore the value reads %d bytes, want the value it held", len(got))
		}
	}},
	{"an access denial prints no service text", func(t *testing.T, r WriterRig) {
		ctx := context.Background()
		id := r.Prefix + "deel-api-key"
		create(t, r.Writer, id, "first-value")
		word := canary(t)
		r.DenyNext("set", "the service says "+word)
		err := r.Writer.Set(ctx, id, "second-value")
		if err == nil || !r.Writer.AccessDenied(err) || !r.Writer.DefiniteRefusal(err) {
			t.Fatalf("a denied Set = %v; want an access denial that is a definite refusal", err)
		}
		if strings.Contains(err.Error(), word) {
			t.Errorf("a denied Set prints the service's text: %q", err)
		}
	}},
}
