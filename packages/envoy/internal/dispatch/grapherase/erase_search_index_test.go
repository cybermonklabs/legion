package grapherase

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/startup"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// An erase that deletes a version of the block's document lists and restates that document's row
// of document_search while the table stands (TestTheEraseOfABlockAVersionHeldEndsThatVersionsNode).
// Over a database where the boot step that retires document search (startup.SearchIndexRetirement)
// has dropped it, the erase reads that it is gone and names it nowhere: it erases the block, ends
// the version it deletes, lists no row of document_search, and the census reads zero.
func TestTheEraseOfABlockAVersionHeldSucceedsOnceTheOldSearchIndexIsGone(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	seeded := seedAgreeingStore(t, database)
	if _, err := database.Pool.Exec(ctx, `
		insert into artifact_versions (artifact_id, number, markdown, authors)
		values ($1::uuid, 2, 'Written before the store existed.'||chr(10)||chr(10)||'Block inner.', '[{"kind":"user","id":"alice"}]')
	`, seeded.document.artifactID); err != nil {
		t.Fatalf("write version 2 of %s, holding the inner block's text: %v", seeded.document.refKey, err)
	}
	if _, err := graphcensus.Backfill(ctx, database, io.Discard); err != nil {
		t.Fatalf("backfill version 2's node: %v", err)
	}
	startup.Retire(ctx, database.Pool, "c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0", true, slog.New(slog.NewTextHandler(io.Discard, nil)),
		[]startup.Retirement{startup.SearchIndexRetirement})
	var standing bool
	if err := database.Pool.QueryRow(ctx, `select to_regclass('document_search') is not null`).Scan(&standing); err != nil {
		t.Fatal(err)
	}
	if standing {
		t.Fatal("the boot step left document_search standing")
	}

	erasure, err := confirmedErase(t, database, refs.BlockNodeKind, seeded.inner, time.Millisecond)
	if err != nil {
		t.Fatalf("erase %s:%s with document_search gone: %v", refs.BlockNodeKind, seeded.inner, err)
	}
	if !erasure.Applied || len(erasure.Versions) != 1 || erasure.Versions[0].Version != 2 {
		t.Fatalf("the erase answered applied %t and the versions %+v (blockers %q), want it applied ending version 2 alone",
			erasure.Applied, erasure.Versions, erasure.Blockers)
	}
	for _, line := range erasure.Listing {
		if strings.Contains(line, "document_search") {
			t.Errorf("the listing names document_search, which is gone: %s", line)
		}
	}
	requireCensusZero(t, database, "after the erase")
}
