package docs

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/testcleanup"
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
// take, so none of them is answered. The eviction is held here until every reader has made its
// read, then let go.
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
	service.failRoom(artifactID, errors.New("test: the room failed"))

	type answer struct {
		text string
		err  error
		took time.Duration
	}
	answers := make(chan answer, readers)
	pinned := make(chan struct{}, readers)
	for range readers {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			ctx, end := store.PinConnection(ctx)
			defer end()
			var one int
			if err := service.store.Pool.QueryRow(ctx, `select 1`).Scan(&one); err != nil {
				pinned <- struct{}{}
				answers <- answer{err: fmt.Errorf("take the read's pinned connection: %w", err)}
				return
			}
			pinned <- struct{}{}
			started := time.Now()
			text, _, err := service.TextWithToken(ctx, artifactID)
			answers <- answer{text: text, err: err, took: time.Since(started)}
		}()
	}
	for range readers {
		<-pinned
	}
	time.Sleep(300 * time.Millisecond)
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
