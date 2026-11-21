package grapherase

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// R5: the erase changes only the rows that derive from the node, and a version the erased block
// did not stand in holds none of its text, in the event log as in the versions. The outer block
// reads `Ship it on Friday.` at version 1, as its own words; version 2 nests the inner block
// under it, holding the same words. Each version's artifact.version event carries its diff, as
// the version route writes it. The erase of the inner block rewrites version 2's event, whose
// added line is the inner block's, and leaves version 1's event as written: the inner block did
// not stand in version 1, so the words that event carries are the outer block's own, exactly as
// the erase leaves the outer block's text at version 1
// (TestTheEraseLeavesAnAncestorsTextFromBeforeTheBlockExisted).
func TestTheEraseLeavesAVersionEventFromBeforeTheBlockExisted(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	seeded := seedAgreeingStore(t, database)
	const words = "Ship it on Friday."
	nested := "Plan for the week:\n  - " + words
	artifact := seeded.document.artifactID
	if _, err := database.Pool.Exec(ctx, `
		insert into artifact_versions (artifact_id, number, markdown, authors)
		values ($1::uuid, 2, $2, '[{"kind":"user","id":"alice"}]')`, artifact, "- "+nested); err != nil {
		t.Fatalf("write version 2 of the document: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into document_block_texts (artifact_id, block_id, version, text)
		values ($1::uuid, 'outer', 1, $2), ($1::uuid, 'outer', 2, $3), ($1::uuid, 'inner', 2, $2)`,
		artifact, words, nested); err != nil {
		t.Fatalf("write the blocks' texts at versions 1 and 2: %v", err)
	}
	if tag, err := database.Pool.Exec(ctx, `
		update document_blocks set changed_version = 2, excerpt = case block_id when 'outer' then $2 else $3 end
		where artifact_id = $1::uuid and block_id in ('outer', 'inner')`, artifact, nested, words); err != nil || tag.RowsAffected() != 2 {
		t.Fatalf("write the blocks' excerpts at version 2: %d rows, %v", tag.RowsAffected(), err)
	}
	versionEvent := func(number int, diff string) int64 {
		return appendEvent(t, database, "CORE-1", "artifact.version", fmt.Sprintf(
			`{"artifact_id": %q, "name": "spec.md", "diff": %q, "version": {"number": %d, "named": true, "authors": [{"kind": "user", "id": "alice"}]}}`,
			artifact, diff, number))
	}
	first := versionEvent(1, "--- previous\n+++ current\n@@ -0,0 +1 @@\n+- "+words+"\n")
	second := versionEvent(2, "--- previous\n+++ current\n@@ -1 +1,2 @@\n-- "+words+"\n+- Plan for the week:\n+  - "+words+"\n")

	erasure, err := confirmedErase(t, database, refs.BlockNodeKind, seeded.inner, time.Millisecond)
	if err != nil {
		t.Fatalf("erase %s: %v", seeded.inner, err)
	}
	if !erasure.Applied {
		t.Fatalf("the erase of %s removed nothing: %q", seeded.inner, erasure.Blockers)
	}
	if _, raw := eventPayload(t, database, second); strings.Contains(raw, "+  - "+words) {
		t.Fatalf("after the erase of %s version 2's event still reads the inner block's line %q, so the run redacted no version event and this test holds nothing: %s",
			seeded.inner, "+  - "+words, raw)
	}
	if _, raw := eventPayload(t, database, first); !strings.Contains(raw, words) {
		t.Errorf("after the erase of %s version 1's artifact.version event (%d) reads %s, want its diff as written, holding %q: the inner block did not stand in version 1, so those words are the outer block's own",
			seeded.inner, first, raw, words)
	}
	if listed(erasure, fmt.Sprintf("events %d artifact.version", first)) {
		t.Errorf("the listing names version 1's event %d as one the run redacts, a version the inner block did not stand in:\n%s",
			first, strings.Join(erasure.Listing, "\n"))
	}
}
