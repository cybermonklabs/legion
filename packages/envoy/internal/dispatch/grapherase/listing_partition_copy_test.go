package grapherase

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// While the boot's fill copies embeddings into embeddings_partitioned (the window between this
// release's migrations and its swap, storetest.OpenUnpartitioned), every write of embeddings is
// written to the copy too, by the mirror's trigger in the writer's transaction, so the copy holds
// the erased comment's windows beside embeddings. The erase's listing names those windows once,
// under embeddings, where the run removes them, and names no row of embeddings_partitioned at
// all: listed under the copy's own name a window would read as a row the run leaves, which the
// run in fact removes, since the mirror removes it with the row of embeddings. After the
// confirmed run neither table holds a window of the comment.
func TestTheEraseListsTheCopysRowsUnderEmbeddingsWhileTheFillCopies(t *testing.T) {
	database := storetest.OpenUnpartitioned(t)
	ctx := context.Background()
	seeded := seedAgreeingStore(t, database)
	windowKind, ownerKey := refs.ItemComment.WindowKind(), refs.ItemComment.WindowOwnerKey(seeded.comment)
	windowTheComment(t, database, seeded.comment)
	copied := func() int64 {
		t.Helper()
		var rows int64
		if err := database.Pool.QueryRow(ctx, `select count(*) from embeddings_partitioned where kind = $1 and split_part(id, '#', 1) = $2`,
			windowKind, ownerKey).Scan(&rows); err != nil {
			t.Fatalf("count the comment's windows in embeddings_partitioned: %v", err)
		}
		return rows
	}
	if copied() == 0 {
		t.Fatal("the copy holds no window of the comment before the erase, so this row proves nothing of what the listing says of it")
	}
	var partitioned bool
	if err := database.Pool.QueryRow(ctx, `select relkind = 'p' from pg_class where oid = 'embeddings'::regclass`).Scan(&partitioned); err != nil || partitioned {
		t.Fatalf("embeddings is partitioned already (%v), so this is not the copy's window", err)
	}

	listing := dryRun(t, database, commentNodeKind, seeded.comment)
	var listedUnderEmbeddings bool
	for _, line := range listing.Listing {
		if strings.Contains(line, "embeddings_partitioned") {
			t.Errorf("the listing names a row of the copy, which the run removes with its row of embeddings: %s", line)
		}
		if strings.Contains(line, "embeddings") && strings.Contains(line, ownerKey) && !strings.HasPrefix(strings.TrimSpace(line), "leave") {
			listedUnderEmbeddings = true
		}
	}
	if !listedUnderEmbeddings {
		t.Errorf("the listing names no window of the comment as a row of embeddings the run removes:\n%s", strings.Join(listing.Listing, "\n"))
	}

	if _, err := Erase(ctx, database.Pool.Config().ConnString(), commentNodeKind, seeded.comment, listing.Digest, time.Millisecond); err != nil {
		t.Fatalf("confirm the erase: %v", err)
	}
	if left := copied(); left != 0 {
		t.Errorf("after the confirmed run the copy holds %d windows of the comment, want none", left)
	}
	if left := itemWindows(t, database, windowKind, ownerKey); left != 0 {
		t.Errorf("after the confirmed run embeddings holds %d windows or window states of the comment, want none", left)
	}
}
