package docs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/logtest"
)

// A service built with no logger (Deps.Logger nil) writes each line where the default logger
// points as the line is written, so a program that sets the default after it builds its service
// finds the service's lines there.
// A service built with a logger writes to it and not to the default, the review a route queues
// through a ledger joined to the service (Ledger.QueueReview, api's route) included. A review
// queued with no ledger (QueueReview) belongs to no service and writes where the default points.
// Each row sets the default after it builds what it reads, and waits for its lines in the logger
// they must reach; the second row then finds them missing from the default. So a logger resolved
// when the service or the review was built fails a row: a New that keeps slog.Default() as it is
// then fails the first, a Ledger.QueueReview that hands its warning no logger the second, and a
// QueueReview whose warning goes anywhere but the default the third.
func TestAServiceWithNoLoggerWritesWhereTheDefaultPointsAtEachLine(t *testing.T) {
	// Not parallel: it sets the process's default logger (logtest.SetDefault), which is what it tests.
	ctx := context.Background()
	// setDefault makes a log of the row's own the default logger's, at WARN, until the row ends.
	setDefault := func(t *testing.T) *storetest.LockedLog {
		logs, logger := testLogger(slog.LevelWarn)
		logtest.SetDefault(t, logger.Handler())
		return logs
	}
	// failed is the line failRoom writes for the document's room; unhashed is the warning of a
	// review of a version the document never stored (unstored), whose content hash the review
	// insert's trigger leaves null (migration 20261105084017), since no artifact_versions row
	// holds the version's text.
	const unstored = 9
	failed := func(artifactID string) string {
		return `level=WARN msg="dispatch: a document room failed" room=` + artifactID + ` error="a cause the log must name"`
	}
	unhashed := func(t *testing.T, artifactID string, review model.ArtifactReview) string {
		t.Helper()
		if review.ID == "" || review.ContentSHA256 != nil {
			t.Fatalf("the review of a version the document never stored = %#v, want one recorded with no content hash", review)
		}
		return fmt.Sprintf(`level=WARN msg="dispatch: review recorded with no content hash: the version names no stored text" artifact=%s version=%d review=%s`,
			artifactID, unstored, review.ID)
	}

	t.Run("a service built with no logger writes to the default set after it", func(t *testing.T) {
		service, artifactID := newTestService(t)
		if err := service.warmLiveDocument(ctx, artifactID); err != nil {
			t.Fatalf("load live document: %v", err)
		}
		defaults := setDefault(t)
		service.failRoom(artifactID, errors.New("a cause the log must name"))
		logtest.AwaitLogged(t, defaults, "the room's failure in the default set after New", failed(artifactID))
	})

	t.Run("a service built with a logger writes to it, its ledger's review included", func(t *testing.T) {
		logs, logger := testLogger(slog.LevelWarn)
		service, artifactID := newTestServiceTo(t, logger)
		if err := service.warmLiveDocument(ctx, artifactID); err != nil {
			t.Fatalf("load live document: %v", err)
		}
		artifact, err := reviewedArtifact(ctx, service.store.Pool, artifactID)
		if err != nil {
			t.Fatal(err)
		}
		defaults := setDefault(t)
		service.failRoom(artifactID, errors.New("a cause the log must name"))
		var review model.ArtifactReview
		joined(t, service, func(ctx context.Context, _ pgx.Tx) error {
			ledger := ledgerFrom(ctx)
			return ledger.Queue(func(b *store.WriteBatch) error {
				return ledger.QueueReview(b, service.events, artifact, unstored, "approved", ReviewKindPerson,
					model.Actor{Kind: "user", ID: "alice"}, nil, nil, func(written model.ArtifactReview, _ model.Event) {
						review = written
					})
			})
		})
		logtest.AwaitLogged(t, logs, "the room's failure and the review's warning in the service's logger",
			failed(artifactID), unhashed(t, artifactID, review))
		if got := defaults.Lines(artifactID); got != "" {
			t.Errorf("the default holds lines of the document of a service built with a logger:\n%s", got)
		}
	})

	t.Run("a review queued with no ledger writes to the default set after it", func(t *testing.T) {
		database, artifact := reviewHashFixture(t)
		defaults := setDefault(t)
		review, _ := writeHashReview(t, database, artifact, unstored)
		logtest.AwaitLogged(t, defaults, "the review's warning in the default set before it was queued",
			unhashed(t, artifact.ID, review))
	})
}
