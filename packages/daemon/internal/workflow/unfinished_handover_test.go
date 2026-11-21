package workflow

import (
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/record"
)

// seedMirrorChild writes what Dispatch says of a child filed under parent: its status and the
// parent that makes it one of parent's open children (record.ChildWaits reads the pair).
func seedMirrorChild(t *testing.T, pool *pgxpool.Pool, key, status, parent string) {
	t.Helper()
	seedRecord(t, pool, func(tx pgx.Tx) error {
		return record.NewStore().PutDispatchIssue(t.Context(), tx, record.DispatchReading{Key: key, Project: "LEGION", Seq: 1,
			Status: record.Carry(status), Parent: record.Carry(parent), Blockers: record.Carry[[]string](nil)})
	})
}

// The tree both of the rule's fixtures are built from: a root at generation 7 with the three
// members under it, and one person's decision standing on each of the two members a close can
// hand over. The references are the asks those decisions were made on.
const (
	handsOverRoot             = "LEGION-208"
	handsOverMergeReady       = "LEGION-209"
	handsOverParkedAtTheMerge = "LEGION-210"
	handsOverWorking          = "LEGION-211"
	handsOverGeneration       = 7
	handsOverMergeReadyRef    = "dispatch://LEGION-209/ask/ask-1"
	handsOverWorkingRef       = "dispatch://LEGION-211/ask/ask-2"
)
