package docs

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/testcleanup"
	"github.com/sjawhar/envoy/internal/testwait"
)

// A read of a document whose room failed is answered with the document once the room recovers,
// even when it reaches the room while the failed room's eviction is still running, and even when
// such reads hold every connection of the request pool. Every ordinary API read is pinned
// (store.PinConnection), the /text routes among them, so each of these readers holds one
// request-pool connection from its first statement on, as an API read does; the eviction flushes
// and compacts the document on a request-pool connection of its own (PgVersioned.withRoomLock).
//
// Main at 55ccc1ec answers such a read with the document: the read waits for the recovery
// (rebuild_test.go's TestAHistoryThatCannotLoadReadsAsDocumentUnloadable reads the document
// right after the room failed). A read refused ErrServiceUnavailable at once instead reaches its
// caller as 503 DOC_SERVICE_UNAVAILABLE, which carries no Retry-After and which the dispatch
// client does not wait out, so the caller gets no document; and a read that waits while it keeps
// its pinned connection leaves a full pool of such readers with no connection for the eviction to
// take, so none of them is answered. The eviction is held here until every reader has been
// answered or is waiting for the room's recovery (beforeRecoveryWait), then let go: a reader that
// reached the room only after the eviction ended would be answered whatever a pinned read does.
func TestPinnedReadsOfAFailedRoomWhoseEvictionIsStillRunningAnswerTheDocument(t *testing.T) {
	t.Parallel()
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "before\n")
	want, _, err := service.TextWithToken(context.Background(), artifactID)
	if err != nil || want != "before\n" {
		t.Fatalf("read the document before its room fails = %q (%v), want %q", want, err, "before\n")
	}
	readers := int(service.store.Pool.Config().MaxConns)

	hold := make(chan struct{})
	release := sync.OnceFunc(func() { close(hold) })
	testcleanup.Add(t, release)
	service.beforeFailedEviction = func(room string) {
		if room == artifactID {
			<-hold
		}
	}
	// A reader counts once it is answered or once it waits for the room's recovery, a wait that,
	// with the eviction held, ends only when the test lets the eviction go or the reader's deadline
	// passes.
	var waiting, answered atomic.Int64
	service.beforeRecoveryWait = func(room string) {
		if room == artifactID {
			waiting.Add(1)
		}
	}
	service.failRoom(artifactID, errors.New("test: the room failed"))

	type answer struct {
		text string
		err  error
		took time.Duration
	}
	answers := make(chan answer, readers)
	for range readers {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			ctx, end := store.PinConnection(ctx)
			defer end()
			var one int
			if err := service.store.Pool.QueryRow(ctx, `select 1`).Scan(&one); err != nil {
				answered.Add(1)
				answers <- answer{err: fmt.Errorf("take the read's pinned connection: %w", err)}
				return
			}
			started := time.Now()
			text, _, err := service.TextWithToken(ctx, artifactID)
			answered.Add(1)
			answers <- answer{text: text, err: err, took: time.Since(started)}
		}()
	}
	testwait.Until(t, "every reader to be answered or waiting for the failed room's recovery", func() (string, bool) {
		w, a := waiting.Load(), answered.Load()
		return fmt.Sprintf("%d waiting and %d answered of %d readers", w, a, readers), w+a >= int64(readers)
	})
	release()

	var refused, wrong int
	var first error
	for range readers {
		got := <-answers
		switch {
		case got.err != nil:
			refused++
			if first == nil {
				first = fmt.Errorf("after %s: %w", got.took.Round(time.Millisecond), got.err)
			}
		case got.text != want:
			wrong++
		}
	}
	if refused > 0 || wrong > 0 {
		t.Fatalf("%d pinned readers of a failed room whose eviction was still running: %d refused, %d answered other text; want each answered %q once the room recovered; first refusal: %v",
			readers, refused, wrong, want, first)
	}
}
