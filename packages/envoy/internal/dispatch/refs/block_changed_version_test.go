package refs

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// writeStoredVersion writes version number of the document as the version write stores it: its
// markdown, under the database's clock at the statement (clock_timestamp, so two versions one
// transaction writes read two times), and answers that time.
func writeStoredVersion(t *testing.T, tx pgx.Tx, artifact string, number int, markdown string) time.Time {
	t.Helper()
	var at time.Time
	if err := tx.QueryRow(context.Background(), `
		insert into artifact_versions (artifact_id, number, markdown, authors, created_at)
		values ($1::uuid, $2, $3, '[{"kind":"user","id":"alice"}]', clock_timestamp())
		returning created_at
	`, artifact, number, markdown).Scan(&at); err != nil {
		t.Fatalf("write version %d: %v", number, err)
	}
	return at
}

// blockChanges is every block row's changed_version and changed_at (blockChange), by block id,
// read in the fixture's transaction.
func blockChanges(t *testing.T, tx pgx.Tx, artifact string) map[string]blockChange {
	t.Helper()
	rows, err := tx.Query(context.Background(), `
		select block_id, changed_version, changed_at from document_blocks where artifact_id = $1::uuid
	`, artifact)
	if err != nil {
		t.Fatalf("read the block rows: %v", err)
	}
	defer rows.Close()
	changes := map[string]blockChange{}
	for rows.Next() {
		var id string
		var change blockChange
		if err := rows.Scan(&id, &change.version, &change.at); err != nil {
			t.Fatalf("read a block row: %v", err)
		}
		changes[id] = change
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the block rows: %v", err)
	}
	return changes
}

// One block-index pass can cover more than one version: a document past the deferral bound, or
// versions a build before wrote during the roll, is indexed by the reconciliation at its latest
// owed version alone (docs' indexOwedDocument). A block's text-change time is read off its row
// (c-spec-tie), so the row dates the change to the version that made it,
// and not to the version the pass indexed. Version 1 is indexed; version 2 rewrites b1, version 3
// leaves b1 alone and rewrites b2, and one pass indexes version 3. b1's row then reads version 2
// and version 2's time, and its text is recorded at version 2, which is the text read at version
// 3; b2's reads version 3 and its time. A pass that took every changed block's version from the
// version it indexed dates b1's rewrite to version 3.
func TestAPassOverSeveralVersionsDatesATextChangeToTheVersionThatMadeIt(t *testing.T) {
	ctx := context.Background()
	tx := windowFixture(t)
	artifact, _ := createReferenceArtifact(t, tx, "WIND-1", "WIND", "spec-md")
	first, firstBlocks := documentOf([]part{{"b1", "paragraph", "One."}, {"b2", "paragraph", "Two."}})
	firstAt := writeStoredVersion(t, tx, artifact, 1, first)
	if _, _, _, err := IndexBlocks(ctx, tx, artifact, 1, firstAt, first, firstBlocks, nil, nil, TypedFacts{}, ""); err != nil {
		t.Fatalf("index version 1: %v", err)
	}
	second, _ := documentOf([]part{{"b1", "paragraph", "One, rewritten."}, {"b2", "paragraph", "Two."}})
	secondAt := writeStoredVersion(t, tx, artifact, 2, second)
	third, thirdBlocks := documentOf([]part{{"b1", "paragraph", "One, rewritten."}, {"b2", "paragraph", "Two, rewritten."}})
	thirdAt := writeStoredVersion(t, tx, artifact, 3, third)
	if !secondAt.Before(thirdAt) {
		t.Fatalf("version 2 was written at %s and version 3 at %s, want two times of the database's clock in order", secondAt, thirdAt)
	}
	if _, won, _, err := IndexBlocks(ctx, tx, artifact, 3, thirdAt, third, thirdBlocks, nil, nil, TypedFacts{}, ""); err != nil || !won {
		t.Fatalf("index version 3 in one pass over versions 2 and 3: won %t, %v", won, err)
	}

	changes := blockChanges(t, tx, artifact)
	if got := changes["b1"]; got.version != 2 || !got.at.Equal(secondAt) {
		t.Errorf("b1, which version 2 rewrote and version 3 left alone, changed at version %d, %s, want version 2, %s",
			got.version, got.at, secondAt)
	}
	if got := changes["b2"]; got.version != 3 || !got.at.Equal(thirdAt) {
		t.Errorf("b2, which version 3 rewrote, changed at version %d, %s, want version 3, %s", got.version, got.at, thirdAt)
	}
	var recorded int
	var text string
	if err := tx.QueryRow(ctx, `
		select version, text from document_block_texts
		where artifact_id = $1::uuid and block_id = 'b1' and version <= 3
		order by version desc limit 1
	`, artifact).Scan(&recorded, &text); err != nil {
		t.Fatalf("read b1's text at version 3: %v", err)
	}
	if recorded != 2 || text != "One, rewritten." {
		t.Errorf("b1's text at version 3 is the row of version %d holding %q, want version 2's, %q", recorded, text, "One, rewritten.")
	}
}
