package docs

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// artifactsInTreeRow is a document's document_block_versions row as
// TestAVersionWrittenAfterTheTickListedItsDocumentStaysOwed reads it, each column as text, "null"
// where it holds none.
type artifactsInTreeRow struct{ version, owedAt, owedRule, statedVersion, statedRule string }

func (row artifactsInTreeRow) String() string {
	return fmt.Sprintf("version=%s store_owed_at=%s store_owed_rule=%s stated_version=%s stated_rule=%s",
		row.version, row.owedAt, row.owedRule, row.statedVersion, row.statedRule)
}

// keeps reports whether the row still owes its index under the mark it held, mark being that
// mark's store_owed_at as the row read it: a null version, that same store_owed_at, and no pass's
// stated version or rule. A tick that cleared the row and a later write that owed it again
// (oweDocumentIndex) leaves a mark of another time, and the clear's stated columns.
func (row artifactsInTreeRow) keeps(mark string) bool {
	return row.version == "null" && row.owedAt == mark && row.statedVersion == "null" && row.statedRule == "null"
}

// statedAt reports whether the row stands at version, stated at it by this build's marking rule,
// with no mark.
func (row artifactsInTreeRow) statedAt(version int) bool {
	at := strconv.Itoa(version)
	return row.version == at && row.statedVersion == at && row.statedRule == strconv.Itoa(refs.BlockIndexMarkingRule) && row.owedAt == "null"
}

func artifactsInTreeReadRow(t *testing.T, database *store.Store, artifactID string) artifactsInTreeRow {
	t.Helper()
	var row artifactsInTreeRow
	if err := database.Pool.QueryRow(context.Background(), `
		select coalesce(version::text, 'null'), coalesce(store_owed_at::text, 'null'), coalesce(store_owed_rule::text, 'null'),
		       coalesce(stated_version::text, 'null'), coalesce(stated_rule::text, 'null')
		from document_block_versions where artifact_id = $1
	`, artifactID).Scan(&row.version, &row.owedAt, &row.owedRule, &row.statedVersion, &row.statedRule); err != nil {
		t.Fatalf("read the document_block_versions row of %s: %v", artifactID, err)
	}
	return row
}

// artifactsInTreeNewestVersion is the number of the newest markdown version of the document, 0
// where it holds none.
func artifactsInTreeNewestVersion(t *testing.T, database *store.Store, artifactID string) int {
	t.Helper()
	var newest int
	if err := database.Pool.QueryRow(context.Background(), `
		select coalesce(max(number), 0) from artifact_versions where artifact_id = $1 and markdown is not null
	`, artifactID).Scan(&newest); err != nil {
		t.Fatalf("read the newest markdown version of %s: %v", artifactID, err)
	}
	return newest
}

// artifactsInTreeBlockNodes counts the document's block rows, and of them the rows that name their
// node (node_kind and node_address set) where the store holds that node.
func artifactsInTreeBlockNodes(t *testing.T, database *store.Store, artifactID string) (rows, named int) {
	t.Helper()
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*)::int,
		       count(*) filter (where d.node_kind is not null and d.node_address is not null and exists (
		         select 1 from graph_nodes n where n.kind = d.node_kind and n.id = d.node_address and n.state <> 'gone'))::int
		from document_blocks d where d.artifact_id = $1
	`, artifactID).Scan(&rows, &named); err != nil {
		t.Fatalf("count the block rows of %s and the nodes they name: %v", artifactID, err)
	}
	return rows, named
}

// artifactsInTreeOwed is the owed count of c-artifacts-in-tree, `select count(*) from
// document_block_versions where version is null or store_owed_at is not null`, and each document it
// counts with its row.
func artifactsInTreeOwed(t *testing.T, database *store.Store) (int, []string) {
	t.Helper()
	ctx := context.Background()
	var count int
	if err := database.Pool.QueryRow(ctx, `select count(*) from document_block_versions where version is null or store_owed_at is not null`).Scan(&count); err != nil {
		t.Fatalf("read the owed count: %v", err)
	}
	rows, err := database.Pool.Query(ctx, `select artifact_id::text from document_block_versions where version is null or store_owed_at is not null order by artifact_id`)
	if err != nil {
		t.Fatalf("read the documents the owed count counts: %v", err)
	}
	var owed []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("read the documents the owed count counts: %v", err)
		}
		owed = append(owed, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the documents the owed count counts: %v", err)
	}
	for index, id := range owed {
		owed[index] = id + " (" + artifactsInTreeReadRow(t, database, id).String() + ")"
	}
	return count, owed
}

// artifactsInTreeHookBound is the bound of each wait of the test for a tick to reach a hook, in the
// seconds its failure prints.
var artifactsInTreeHookBound = int(fenceWait / time.Second)

// artifactsInTreeHold is a hold the test keeps a tick in at a hook until it opens it: the tick
// waits there for the test, bounded by fenceWait, and the test opens it once, whichever of its
// paths opens it first, so every path out of the test releases every tick it holds.
type artifactsInTreeHold struct {
	released chan struct{}
	once     sync.Once
}

func newArtifactsInTreeHold() *artifactsInTreeHold {
	return &artifactsInTreeHold{released: make(chan struct{})}
}

// wait is the tick's side: it waits at hook until the test opens the hold, and answers an error
// once fenceWait has passed, which leaves the tick's pass there.
func (h *artifactsInTreeHold) wait(hook string) error {
	select {
	case <-h.released:
		return nil
	case <-time.After(fenceWait):
		return fmt.Errorf("the test did not release the tick held at %s within %s", hook, fenceWait)
	}
}

// open releases the tick held, once.
func (h *artifactsInTreeHold) open() {
	h.once.Do(func() { close(h.released) })
}
