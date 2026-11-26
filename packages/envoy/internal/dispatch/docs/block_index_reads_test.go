package docs

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// No read waits on a block-index pass. The pass's transaction reads artifact_versions (its newest
// markdown version, indexOwedDocument) and writes the document's block index under row locks of
// its own, and holds what it took until it commits; a plain read takes ACCESS SHARE on each table
// and no row lock, which conflicts with neither. Held at its commit (afterBlockIndexWrite), the
// pass leaves every read the coverage route makes of the document - its newest markdown version,
// its block-index row, its blocks - answering at once under a lock_timeout of 200 ms. What it does
// hold off is a lock of the whole table: the e2e harness's reset (e2e/seed.ts, resetDatabaseOnce)
// takes ACCESS EXCLUSIVE on artifact_versions, which the pass's ACCESS SHARE refuses until the pass
// commits; that refusal is the reset's own "could not lock artifact_versions to truncate it within
// 5 s". Once the pass has committed, the same lock is taken.
func TestAHeldBlockIndexPassHoldsOffNoReadAndOnlyALockOfTheWholeTable(t *testing.T) {
	ctx := context.Background()
	database, service := blockIndexService(t)
	document := createOwedDocument(t, database, service, "CORE-1", "", "held", raceVersion1, "")
	settle(t, database, document)

	entered, release := make(chan struct{}), make(chan struct{})
	service.afterBlockIndexWrite = func(artifactID string) error {
		if artifactID == document {
			close(entered)
			<-release
		}
		return nil
	}
	defer func() { service.afterBlockIndexWrite = nil }()
	ticked := make(chan error, 1)
	go func() {
		_, err := service.ReconcileBlockIndex(ctx, BlockIndexCursors{}, liveAheadDocuments{})
		ticked <- err
	}()
	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()
	select {
	case <-entered:
	case <-time.After(fenceWait):
		t.Fatalf("the pass did not reach its commit within %s", fenceWait)
	}

	probe, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the reads: %v", err)
	}
	defer probe.Rollback(ctx)
	if _, err := probe.Exec(ctx, `select set_config('lock_timeout', '200ms', true)`); err != nil {
		t.Fatalf("bound the reads' lock waits: %v", err)
	}
	for _, read := range []struct{ what, sql string }{
		{"the newest markdown version", `select count(*) from artifact_versions where artifact_id = $1 and markdown is not null`},
		{"the block-index row", `select count(*) from document_block_versions where artifact_id = $1 and (version is null or store_owed_at is not null)`},
		{"the blocks", `select count(*) from document_blocks where artifact_id = $1`},
	} {
		var count int
		began := time.Now()
		if err := probe.QueryRow(ctx, read.sql, document).Scan(&count); err != nil {
			t.Fatalf("the read of %s while the pass holds its locks failed: %v", read.what, err)
		}
		if took := time.Since(began); took > time.Second {
			t.Errorf("the read of %s took %s while the pass held its locks, want no wait", read.what, took)
		}
	}
	if err := probe.Rollback(ctx); err != nil {
		t.Fatalf("end the reads: %v", err)
	}

	lockWhole := func() error {
		tx, err := database.Pool.Begin(ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback(ctx)
		_, err = tx.Exec(ctx, `lock table artifact_versions in access exclusive mode nowait`)
		return err
	}
	var refused *pgconn.PgError
	if err := lockWhole(); !errors.As(err, &refused) || refused.Code != "55P03" {
		t.Fatalf("a lock of the whole of artifact_versions while the pass holds its locks answered %v, want 55P03 lock_not_available", err)
	}

	close(release)
	released = true
	if err := <-ticked; err != nil {
		t.Fatalf("the pass: %v", err)
	}
	if err := lockWhole(); err != nil {
		t.Fatalf("a lock of the whole of artifact_versions once the pass committed answered %v, want it taken", err)
	}
}
