package docs

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testcleanup"
	"github.com/sjawhar/envoy/internal/testwait"
)

// A document the reconciliation defers counts its backoff from the moment it is deferred, not from
// the start of the pass that defers it (refs.DeferExcerpts): a pass that ran longer than the
// document's backoff before it got to the document would otherwise leave it due at once, and the
// next pass would read it again. Here the pass's read of the one owed document is held while the
// test moves the service's fake clock two backoffs on, so the deferral comes two backoffs after
// the pass began, and the document must fall due a backoff, and at most a fifth more of jitter,
// after the deferral.
func TestADocumentDeferredAfterALongPassWaitsItsWholeBackoff(t *testing.T) {
	database := storetest.Open(t)
	failing := owedDocuments(t, database, 1)[0]
	persistence := &heldUnreadableStore{VersionedStore: NewPgVersioned(database), document: failing, entered: make(chan struct{}), release: make(chan struct{})}
	testcleanup.Add(t, persistence.letGo)
	passClock := clocktest.New(time.Now())
	service := newExcerptServiceOn(t, database, persistence, passClock)
	base := referenceExcerptSchedule.retryBase

	passed := make(chan error, 1)
	go func() {
		_, _, err := service.ReconcileReferenceExcerpts(context.Background())
		passed <- err
	}()
	testwait.Receive(t, "the pass to reach its read of the owed document", persistence.entered)
	passClock.Advance(2 * base)
	// The moment the deferral reads, as the database stores it (to the microsecond).
	deferred := passClock.Now().Truncate(time.Microsecond)
	persistence.letGo()
	if err := testwait.Receive(t, "the pass to defer the document once its read failed", passed); err != nil {
		t.Fatalf("the pass that deferred the document: %v", err)
	}
	var due time.Time
	if err := database.Pool.QueryRow(context.Background(), `
		select next_attempt_at from reference_excerpt_backoff where artifact_id = $1::uuid
	`, failing).Scan(&due); err != nil {
		t.Fatalf("read the deferred document's retry: %v", err)
	}
	if earliest, latest := deferred.Add(base), deferred.Add(base+base/5+time.Microsecond); due.Before(earliest) || due.After(latest) {
		t.Fatalf("the document deferred %v into its pass is due %v after the deferral; want its backoff of %v from the deferral, and at most a fifth more",
			2*base, due.Sub(deferred), base)
	}
}

// heldUnreadableStore fails every read of document's stamp, holding the first until letGo is
// called, so a pass's read of the document waits where the test wants it: entered closes once the
// first read is held.
type heldUnreadableStore struct {
	VersionedStore
	document         string
	entered, release chan struct{}
	enter, free      sync.Once
}

func (s *heldUnreadableStore) DocumentStamp(ctx context.Context, room string) (DocumentStamp, error) {
	if room != s.document {
		return s.VersionedStore.DocumentStamp(ctx, room)
	}
	s.enter.Do(func() { close(s.entered) })
	<-s.release
	return DocumentStamp{}, errors.New("injected: the store cannot read this document")
}

func (s *heldUnreadableStore) LoadDocument(ctx context.Context, room string) (LoadedDocument, error) {
	if room == s.document {
		return LoadedDocument{}, errors.New("injected: the store cannot read this document")
	}
	return s.VersionedStore.LoadDocument(ctx, room)
}

// letGo lets the held read, and every one after it, fail at once.
func (s *heldUnreadableStore) letGo() { s.free.Do(func() { close(s.release) }) }
