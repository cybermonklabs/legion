package docs

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/persistence"

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
	if err := joinedWrite(service, artifactID, func(ctx context.Context) error {
		tx, _ := txFromContext(ctx)
		if err := lockDocumentRoom(ctx, tx, artifactID); err != nil {
			return err
		}
		if _, _, err := service.persistence.AppendRoomUpdatesTx(ctx, tx, artifactID, nil, []BrowserUpdate{
			{Update: browserUpdateBytes(), ContentChanged: true, Credit: credit, Observed: time.Now()},
		}); err != nil {
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
	if err := joinedWrite(service, artifactID, func(ctx context.Context) error {
		tx, _ := txFromContext(ctx)
		if err := lockDocumentRoom(ctx, tx, artifactID); err != nil {
			return err
		}
		if _, _, err := service.persistence.AppendRoomUpdatesTx(ctx, tx, artifactID, nil, []BrowserUpdate{
			{Update: browserUpdateBytes(), ContentChanged: true, Credit: credit, Observed: time.Now()},
		}); err != nil {
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

// A pending author this transaction has queued reads back in it as the table reads once the
// write phase has run (transactionPendingAuthors, readPendingAuthors): Proven only when every row
// of that author is, the rows the table held before and the rows the transaction queued alike.
// That holds for two rows of one author queued at two updates, in one statement or in two; a row
// queued at the update a row already queued holds, which the upsert merges
// (pendingAuthorsStatement); and a queued row beside, or over, a row the table holds, in either
// order. The read inside the transaction and the read once it has committed must both give the
// one author, so an overlay that kept the later row's mark as it stood fails where the table does
// not.
func TestAQueuedPendingAuthorIsProvenOnlyWhenEveryRowIs(t *testing.T) {
	service, artifactID := newTestService(t)
	ctx := context.Background()
	claimed := model.Actor{Kind: "session", ID: "judge-session"}
	proven := model.Actor{Kind: "session", ID: "judge-session", Proven: true}
	at := func(actor model.Actor, version persistence.Version) pendingAuthor {
		return pendingAuthor{actor: actor, writtenThrough: version}
	}
	for _, tc := range []struct {
		name string
		// stored is what the table holds before the transaction; queued is each upsert the
		// transaction queues, in order.
		stored []pendingAuthor
		queued [][]pendingAuthor
		want   model.Actor
	}{
		{"a claimed row and a proven one queued in one statement", nil, [][]pendingAuthor{{at(claimed, 5), at(proven, 6)}}, claimed},
		{"a proven row and a claimed one queued in one statement", nil, [][]pendingAuthor{{at(proven, 5), at(claimed, 6)}}, claimed},
		{"a proven row, then a claimed one queued at another update", nil, [][]pendingAuthor{{at(proven, 5)}, {at(claimed, 6)}}, claimed},
		{"a proven row, then a claimed one queued at the same update", nil, [][]pendingAuthor{{at(proven, 5)}, {at(claimed, 5)}}, claimed},
		{"a claimed row, then a proven one queued at the same update", nil, [][]pendingAuthor{{at(claimed, 5)}, {at(proven, 5)}}, claimed},
		{"two proven rows queued", nil, [][]pendingAuthor{{at(proven, 5)}, {at(proven, 5), at(proven, 6)}}, proven},
		{"a stored proven row, then a claimed one queued beside it", []pendingAuthor{at(proven, 4)}, [][]pendingAuthor{{at(claimed, 5)}}, claimed},
		{"a stored claimed row, then a proven one queued beside it", []pendingAuthor{at(claimed, 4)}, [][]pendingAuthor{{at(proven, 5)}}, claimed},
		{"a stored proven row, then a claimed one queued over it", []pendingAuthor{at(proven, 4)}, [][]pendingAuthor{{at(claimed, 4)}}, claimed},
		{"a stored claimed row, then a proven one queued over it", []pendingAuthor{at(claimed, 4)}, [][]pendingAuthor{{at(proven, 4)}}, claimed},
		{"a stored proven row, then proven ones queued beside and over it", []pendingAuthor{at(proven, 4)}, [][]pendingAuthor{{at(proven, 4), at(proven, 5)}}, proven},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := service.store.Pool.Exec(ctx, `delete from doc_pending_authors where artifact_id = $1`, artifactID); err != nil {
				t.Fatalf("clear the pending authors: %v", err)
			}
			if len(tc.stored) > 0 {
				tx, err := service.store.Pool.Begin(ctx)
				if err != nil {
					t.Fatalf("begin: %v", err)
				}
				defer tx.Rollback(ctx)
				if err := upsertPendingAuthors(ctx, tx, artifactID, tc.stored); err != nil {
					t.Fatalf("store %+v: %v", tc.stored, err)
				}
				if err := tx.Commit(ctx); err != nil {
					t.Fatalf("commit the stored rows: %v", err)
				}
			}
			var owed map[string]model.Actor
			if err := joinedWrite(service, artifactID, func(ctx context.Context) error {
				tx, _ := txFromContext(ctx)
				for _, authors := range tc.queued {
					if err := upsertPendingAuthors(ctx, tx, artifactID, authors); err != nil {
						return err
					}
				}
				// The queued rows are the ledger's alone until the write phase runs, so the read
				// below takes them from its overlay.
				var rows int
				if err := tx.QueryRow(ctx, `select count(*) from doc_pending_authors where artifact_id = $1`, artifactID).Scan(&rows); err != nil {
					return err
				}
				if rows != len(tc.stored) {
					return fmt.Errorf("the table holds %d rows before the write phase, want the %d stored", rows, len(tc.stored))
				}
				var err error
				owed, err = transactionPendingAuthors(ctx, tx, artifactID)
				return err
			}); err != nil {
				t.Fatalf("queue %+v and read the pending authors back: %v", tc.queued, err)
			}
			want := map[string]model.Actor{actorKey(tc.want): tc.want}
			if !reflect.DeepEqual(owed, want) {
				t.Fatalf("the transaction reads the pending authors as %+v, want %+v", owed, want)
			}
			stored, err := readPendingAuthors(ctx, service.store.Pool, artifactID)
			if err != nil {
				t.Fatalf("read the pending authors back: %v", err)
			}
			if !reflect.DeepEqual(stored, want) {
				t.Fatalf("doc_pending_authors after the commit reads %+v, want %+v", stored, want)
			}
		})
	}
}
