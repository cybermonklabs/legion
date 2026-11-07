package docs

import (
	"context"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// An append's writes are queued on the transaction's write phase, so the tables they write hold
// nothing of them until that phase runs, and a read the same transaction makes afterwards reads
// the ledger's overlay beside the table (transactionPendingAuthors,
// transactionRecordedAskAuthors). Each table's insert is queued in one place, which records the
// overlay with it (queuePendingAuthors, queueAskAuthors), so the append's rows are in the overlay
// as the writers that queue the same statements on their own put theirs there.
//
// No production read consults the durable ask authors after an append of its own transaction
// today: the one settlement that reads them reconciles before it flushes the room's updates
// (settleRoomWithin). These tests are at the level that does exercise both halves - the real
// append, then the real read, in one joined transaction - so a queue site that records no overlay
// is caught where it is made rather than when a caller's order changes.

// browserUpdateBytes is an encoded document update, what a room hands its store for an append.
func browserUpdateBytes() []byte {
	doc := crdt.New()
	doc.GetXmlFragment(fragmentName)
	return crdt.EncodeStateAsUpdateV1(doc, nil)
}

// appendCredit is one browser update's in-flight credit of artifactID's room, as the room's
// update observer leaves it for the update's append (newUpdateCredit): authors, whom the edit
// credits, and askAuthors, the ask blocks its render first showed with the author named for each.
// The credit lands at the end of the test, which takes its record out of the room's in-flight
// credits as a committed append does (UpdateCredit.landed).
func appendCredit(t *testing.T, service *Service, artifactID string, authors []model.Actor, askAuthors map[string]model.Actor) *UpdateCredit {
	t.Helper()
	state := service.lockState(artifactID)
	record := &inflightCredit{seq: state.creditSeq.Add(1), authors: make(map[string]model.Actor, len(authors))}
	for _, author := range authors {
		record.authors[actorKey(author)] = author
	}
	state.inflight[record.seq] = record
	service.unlockState(artifactID, state)
	credit := &UpdateCredit{service: service, room: artifactID, state: state, record: record, askAuthors: askAuthors}
	t.Cleanup(credit.landed)
	return credit
}

// The append of an update whose render first showed an ask block queues that block's author
// (doc_ask_block_authors) among its own writes. A read of the durable ask authors in the same
// transaction - the read a settlement makes of them (transactionRecordedAskAuthors) - finds the
// author the append queued, which the table holds no row of until the write phase has run, and
// the commit leaves the row.
func TestAnAppendsAskBlockAuthorIsReadBackInItsOwnTransaction(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "First.\n")
	bob := model.Actor{Kind: "user", ID: "bob"}
	credit := appendCredit(t, service, artifactID, nil, map[string]model.Actor{"bob-ask": bob})

	var recorded map[string]model.Actor
	if err := joinedWrite(service, func(ctx context.Context) error {
		tx, _ := txFromContext(ctx)
		if err := lockDocumentRoom(ctx, tx, artifactID); err != nil {
			return err
		}
		if _, err := service.persistence.AppendRoomUpdateTx(ctx, tx, artifactID, nil, browserUpdateBytes(), true, credit, time.Now()); err != nil {
			return err
		}
		var err error
		recorded, err = transactionRecordedAskAuthors(ctx, tx, artifactID)
		return err
	}); err != nil {
		t.Fatalf("append the update and read its ask block authors back: %v", err)
	}

	if recorded["bob-ask"] != bob {
		t.Fatalf("the transaction reads the durable ask authors as %+v, want bob-ask as bob: its own append queued that row", recorded)
	}
	if stored := recordedAskAuthors(t, service.store, artifactID); stored["bob-ask"] != bob {
		t.Fatalf("doc_ask_block_authors after the commit = %+v, want bob-ask as bob", stored)
	}
}

// The append of a credited update queues the edit's pending authors (doc_pending_authors) among
// its own writes. A read of R in the same transaction - the read a version makes of it after it
// has stored the room's updates (transactionPendingAuthors) - finds the author the append queued,
// which the table holds no row of until the write phase has run, and the commit leaves the row.
func TestAnAppendsPendingAuthorIsReadBackInItsOwnTransaction(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "First.\n")
	bob := model.Actor{Kind: "user", ID: "bob"}
	credit := appendCredit(t, service, artifactID, []model.Actor{bob}, nil)

	var owed map[string]model.Actor
	if err := joinedWrite(service, func(ctx context.Context) error {
		tx, _ := txFromContext(ctx)
		if err := lockDocumentRoom(ctx, tx, artifactID); err != nil {
			return err
		}
		if _, err := service.persistence.AppendRoomUpdateTx(ctx, tx, artifactID, nil, browserUpdateBytes(), true, credit, time.Now()); err != nil {
			return err
		}
		var err error
		owed, err = transactionPendingAuthors(ctx, tx, artifactID)
		return err
	}); err != nil {
		t.Fatalf("append the update and read its pending authors back: %v", err)
	}

	if owed[actorKey(bob)] != bob {
		t.Fatalf("the transaction reads the pending authors as %+v, want bob: its own append queued his row", owed)
	}
	stored, err := readPendingAuthors(context.Background(), service.store.Pool, artifactID)
	if err != nil {
		t.Fatalf("read the pending authors back: %v", err)
	}
	if stored[actorKey(bob)] != bob {
		t.Fatalf("doc_pending_authors after the commit = %+v, want bob", stored)
	}
}
