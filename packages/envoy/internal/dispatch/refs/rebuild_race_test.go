package refs

import (
	"cmp"
	"context"
	"reflect"
	"slices"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// A rebuild and an edit of the same source race unless they are ordered: the rebuild reads a
// source's text, an editor writes new text and the edges of that new text, and the rebuild then
// replaces those edges with the ones it built from the text it read, so the edit's own index is
// lost until something re-indexes that source (stored-document finding 9 of
// dispatch://INFRA-63/artifact/ha-audit-md, dispatch://INFRA-3285).
//
// This row drives that interleaving rather than hoping for it. The edit runs while the rebuild's
// transaction is open and after it has read the text, which `for no key update` is what makes
// deterministic: the editor's own write of the row blocks on the lock the rebuild holds, so the
// edit lands after the rebuild's replacement and its edges are the stored ones. Without that lock
// the editor goes first, the rebuild overwrites its edges with the old text's, and the assertion
// below reads the old target.
//
// The interleaving is forced by the editor's own write rather than by a sleep, so the row is
// deterministic in both directions: it failed before the lock was taken and passes with it.
func TestARebuildBesideAnEditKeepsTheEditsReferences(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	source := resolverSource(2)
	rebuildFixture(t, database, source, "ACME-1 is the issue this comment names.")

	// A second issue for the edit to name, so the two texts have different edges.
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the second issue: %v", err)
	}
	insertResolverIssue(t, tx, "ACME-2", 2)
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the second issue: %v", err)
	}

	// The edit, run while the rebuild is inside its own transaction: it writes the new text and
	// the edges of that new text, as a live writer does.
	edited := make(chan error, 1)
	go func() {
		edited <- func() error {
			tx, err := database.Pool.Begin(ctx)
			if err != nil {
				return err
			}
			defer tx.Rollback(ctx)
			body := "ACME-2 is the issue this comment names now."
			if _, err := tx.Exec(ctx, `update comments set body = $2 where id = $1::uuid`, source, body); err != nil {
				return err
			}
			located := NewReader(ctx, tx, "comment", source, resolverServer).Mentions(body)
			if _, err := replaceRefs(ctx, tx, "comment", source, text.Refs(located), nil); err != nil {
				return err
			}
			return tx.Commit(ctx)
		}()
	}()

	if _, err := RebuildAll(ctx, database.Pool, resolverServer); err != nil {
		t.Fatalf("rebuild the references: %v", err)
	}
	if err := <-edited; err != nil {
		t.Fatalf("the edit beside the rebuild: %v", err)
	}

	// Whichever order the two transactions took, the stored edges are the edited text's: the
	// rebuild either read the new text, or held the row until the edit followed it.
	want := []Target{{"issue", "ACME-2"}}
	slices.SortFunc(want, func(a, b Target) int {
		return cmp.Or(cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.ID, b.ID))
	})
	if got := storedRefs(t, database.Pool, "comment", source); !reflect.DeepEqual(got, want) {
		t.Errorf("after a rebuild beside an edit the comment stored %v; want %v, the edited text's", got, want)
	}
}
