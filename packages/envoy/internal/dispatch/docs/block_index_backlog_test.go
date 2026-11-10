package docs

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// dateOwedVersion dates the stored version of a document owed a block index, which is the write
// that owed it one and what the tick measures its debt from. A test writes a backlog of known
// ages with it; nothing in production moves a version's created_at.
func dateOwedVersion(t *testing.T, database *store.Store, artifactID string, at time.Time) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(),
		`update artifact_versions set created_at = $2 where artifact_id = $1`, artifactID, at); err != nil {
		t.Fatalf("date the version of %s: %v", artifactID, err)
	}
}

// A backlog nobody can read is one nobody will notice, so a tick reports both figures: how many
// documents are still owed a block index, and when the oldest of those debts was incurred. The
// age must follow the oldest owed document and not the newest, since a backlog dated by its
// newest arrival reads as fresh however long its oldest has waited.
func TestTheBlockIndexTickReportsTheBacklogAndTheAgeOfItsOldestDocument(t *testing.T) {
	database, service := blockIndexService(t)
	// Postgres holds a timestamp to the microsecond. The test's own clock is rounded to it, so
	// a nanosecond the database cannot store cannot make the comparisons below fail.
	now := time.Now().UTC().Truncate(time.Microsecond)
	ages := []time.Duration{3 * time.Hour, 2 * time.Hour, time.Hour}
	owed := make([]string, len(ages))
	for index, age := range ages {
		// A document whose live text is ahead of its version is skipped and left owed, so one
		// tick reads this backlog rather than draining it.
		owed[index] = createOwedDocument(t, database, service, "", fmt.Sprintf("spec-%d", index), shipItSpec, "Ship it.\n")
		dateOwedVersion(t, database, owed[index], now.Add(-age))
	}

	tick := reconcileBlockIndex(t, service, "")
	if tick.OwedDocuments != len(ages) || tick.LiveAhead != len(ages) {
		t.Fatalf("tick = %#v, want %d documents owed and all of them skipped as live ahead", tick, len(ages))
	}
	oldest, newest := now.Add(-ages[0]), now.Add(-ages[len(ages)-1])
	if !tick.OldestOwedDocumentAt.Equal(oldest) {
		t.Fatalf("the oldest owed document is dated %s, want %s, the oldest of the three rather than the newest (%s)",
			tick.OldestOwedDocumentAt.UTC(), oldest, newest)
	}
	if age := tick.OldestOwedDocumentAge(now); age != ages[0] {
		t.Fatalf("the oldest debt is %s old, want %s", age, ages[0])
	}

	// The figures clear with the backlog: a settled document is no longer ahead of its version,
	// so the next tick indexes all three and there is no debt left to date.
	for _, document := range owed {
		settle(t, database, document)
	}
	drained := reconcileBlockIndex(t, service, "")
	if drained.Documents != len(ages) || drained.OwedDocuments != 0 {
		t.Fatalf("tick = %#v, want all %d documents indexed and none owed", drained, len(ages))
	}
	if !drained.OldestOwedDocumentAt.IsZero() || drained.OldestOwedDocumentAge(now) != 0 {
		t.Fatalf("the drained tick dates its oldest debt at %s, age %s, want no debt at all",
			drained.OldestOwedDocumentAt, drained.OldestOwedDocumentAge(now))
	}
}

// A document that holds no markdown version is owed a block index all the same - rebuild-refs
// marks every document, and a tick passes over one with no version to index - so no version
// dates its debt. It is dated by its own creation, which is when it started being owed. Dating
// it by nothing would leave it counted in the backlog and invisible in the age, which is the
// one row whose age an operator most needs: no tick will ever settle it.
func TestADocumentWithNoVersionIsDatedByItsOwnCreation(t *testing.T) {
	database, service := blockIndexService(t)
	ctx := context.Background()
	made := time.Now().UTC().Truncate(time.Microsecond).Add(-5 * time.Hour)
	var artifactID string
	if err := database.Pool.QueryRow(ctx, `
		insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by, created_at)
		values ('CORE-1', 'CORE', 'empty', 'empty.md', 'doc', false, '{"kind":"user","id":"alice"}', $1)
		returning id::text
	`, made).Scan(&artifactID); err != nil {
		t.Fatalf("create a document with no version: %v", err)
	}
	// The mark rebuild-refs leaves on every document (refs.Rebuild), this one included.
	if _, err := database.Pool.Exec(ctx, `
		insert into document_block_versions (artifact_id) select id from artifacts where kind = 'doc'
		on conflict (artifact_id) do update set version = null
	`); err != nil {
		t.Fatalf("mark every document owed a block index: %v", err)
	}

	tick := reconcileBlockIndex(t, service, "")
	if tick.Documents != 0 || tick.OwedDocuments != 1 {
		t.Fatalf("tick = %#v, want the version-less document still owed and nothing indexed", tick)
	}
	if !tick.OldestOwedDocumentAt.Equal(made) {
		t.Fatalf("the oldest owed document is dated %s, want the document's own creation %s",
			tick.OldestOwedDocumentAt.UTC(), made)
	}
}
