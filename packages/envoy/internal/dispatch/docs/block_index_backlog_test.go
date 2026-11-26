package docs

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// A build counts as its backlog only the documents it would list: a mark of a higher marking rule
// on a document whose stand is at its version is that rule's to drain, so neither the owed count
// the tick logs nor the debts `census --store --sizing` reads (refs.OwedBlockIndexDebts) counts it
// for a build of a lower rule. A build of rule 1 after a rollback from rule 2 would otherwise
// report the marks of rule 2 as a backlog that only grows, since it never drains them (R13).
func TestABuildsBacklogCountsOnlyTheDocumentsItLists(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database, service := blockIndexService(t)
	rule, higherRule := service.markingRule, service.markingRule+1
	left := createOwedDocument(t, database, service, "CORE-1", "", "left", nestedSpec("left"), "")
	settle(t, database, left)
	drainBlockIndex(t, service, "seed")
	if _, err := database.Pool.Exec(ctx, graphcensus.BlockBatchMarkStatement, []string{left}, higherRule); err != nil {
		t.Fatalf("mark %s at marking rule %d: %v", left, higherRule, err)
	}
	// The documents the build lists: each live ahead of its version, so the tick leaves them owed.
	lists := 2
	for index := range lists {
		createOwedDocument(t, database, service, "CORE-1", "", fmt.Sprintf("listed-%d", index), shipItSpec, "Ship it.\n")
	}
	if listed(t, service, left) {
		t.Fatalf("the build lists %s, which a mark of rule %d holds at its stand: this test proves nothing", left, higherRule)
	}

	tick := reconcileBlockIndex(t, service, "")
	var debts int
	if err := database.Pool.QueryRow(ctx, `select count(*) from (`+refs.OwedBlockIndexDebts+`) owed`, rule).Scan(&debts); err != nil {
		t.Fatalf("read the debts: %v", err)
	}
	for reader, got := range map[string]int{
		"the owed count the tick logs (ReconcileBlockIndex)": tick.OwedDocuments,
		"refs.OwedBlockIndexDebts":                           debts,
	} {
		if got != lists {
			t.Errorf("a build of marking rule %d counts %d owed document(s) in %s, want %d, the documents it lists, which leave out %s, whose mark is of rule %d and whose stand is at its version",
				rule, got, reader, lists, left, higherRule)
		}
	}
}

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
	t.Parallel()
	database, service := blockIndexService(t)
	// Postgres holds a timestamp to the microsecond. The test's own clock is rounded to it, so
	// a nanosecond the database cannot store cannot make the comparisons below fail.
	now := time.Now().UTC().Truncate(time.Microsecond)
	ages := []time.Duration{3 * time.Hour, 2 * time.Hour, time.Hour}
	owed := make([]string, len(ages))
	for index, age := range ages {
		// A document whose live text is ahead of its version is skipped and left owed, so one
		// tick reads this backlog rather than draining it.
		owed[index] = createOwedDocument(t, database, service, "CORE-1", "", fmt.Sprintf("spec-%d", index), shipItSpec, "Ship it.\n")
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

// A document that holds no markdown version is owed a block index until a tick reaches it -
// rebuild-refs marks every document - and no version dates that debt. The backlog dates it by
// the document's own creation, which is when it started being owed (refs.OwedBlockIndexDebts, the
// one text both the tick's logged figure and `census --store --sizing` read). Dating it by nothing
// would leave it counted in the backlog and invisible in the age.
//
// What the tick then does with it is this build's rule (R16): once the owner-locked re-read finds
// no markdown version, one statement clears the document's marks and states it at version 0
// (indexOwedDocument), where a tick of the release before passed over it and left it owed for
// ever. After the tick the document is stated at version 0 by this build's rule, owed by nothing,
// and the backlog holds no debt of it to date.
func TestADocumentWithNoVersionIsDatedByItsOwnCreation(t *testing.T) {
	t.Parallel()
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
	debts := func() (int, *time.Time) {
		t.Helper()
		var count int
		var oldest *time.Time
		if err := database.Pool.QueryRow(ctx, `select count(*), min(owed.owed_at) from (`+refs.OwedBlockIndexDebts+`) owed`,
			service.markingRule).Scan(&count, &oldest); err != nil {
			t.Fatalf("read the block-index debts: %v", err)
		}
		return count, oldest
	}

	if count, oldest := debts(); count != 1 || oldest == nil || !oldest.Equal(made) {
		t.Fatalf("before a tick the backlog holds %d debt(s), the oldest dated %v, want the version-less document's one, dated by its own creation %s",
			count, oldest, made)
	}

	tick := reconcileBlockIndex(t, service, "")
	if tick.Documents != 0 || tick.OwedDocuments != 0 || !tick.OldestOwedDocumentAt.IsZero() {
		t.Fatalf("tick = %#v, want nothing indexed, nothing owed and no debt dated: the tick states a document with no markdown version at version 0", tick)
	}
	if version := blockIndexAt(t, database, artifactID); version == nil || *version != 0 {
		t.Errorf("after the tick the version-less document's blocks stand at %s, want version 0", standsText(version))
	}
	requireStatedByThisRule(t, database, artifactID, 0, "after the tick the version-less document")
	if owedAt, _ := backfillMark(t, database, artifactID); owedAt != nil {
		t.Errorf("after the tick the version-less document holds the mark %v, want it owed by nothing", owedAt)
	}
	if count, _ := debts(); count != 0 {
		t.Errorf("after the tick the backlog holds %d debt(s), want none", count)
	}
}
