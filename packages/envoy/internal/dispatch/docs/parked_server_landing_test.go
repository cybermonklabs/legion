package docs

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"
	ygsync "github.com/reearth/ygo/sync"

	"github.com/sjawhar/envoy/internal/dispatch/docs/docstest"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// A browser's frame parked behind clocks a server write then supplies lands in that write's
// commit, and is stored before its sender is told it is saved. The write here is a catch-up, the
// rows another process stored: they are durable already, so the room stores their commit only
// where it carries more than they hold - here the parked frame the commit's drain landed
// (classifyDrained, carriesBeyond) - and resyncs the sender once that store has returned, so the
// SyncStep2 its answer clears the row with comes after the store. Before that rule the catch-up's
// commit was taken as stored, the landed content was in no row, and the next browser commit's
// store resynced the sender all the same: the row cleared, and the edit was the room's alone.
//
// Bob writes a paragraph on task B, which task A's room has not caught up to. Alice's page on task
// A takes bob's paragraph from the store, as a page that held it from another process's room
// would, and types after it; task A parks her frame behind bob's clocks and answers it not
// applied. Task A then catches up, which lands her frame, and carol types on task A, whose store
// is the one that resynced alice before the rule. Once alice's row has cleared, the store holds
// her edit.
func TestAFrameParkedBehindAServerWritesClocksIsStoredBeforeItsSenderIsAnsweredSaved(t *testing.T) {
	pair := newProcessPair(t)
	ctx := context.Background()
	var lines []string
	var linesMu sync.Mutex
	t.Cleanup(func() {
		if t.Failed() {
			linesMu.Lock()
			defer linesMu.Unlock()
			t.Logf("the drive:\n%s", strings.Join(lines, "\n"))
		}
	})
	start := time.Now()
	alice := lcDial(t, pair.serverA.URL, pair.artifactID, "alice", nil, start, &lines, &linesMu)
	<-alice.synced
	if !lcWait(t, "alice's first sync to be answered", 5*time.Second, func() bool { return len(alice.heard()) > 0 }) {
		t.Fatal("alice's first SyncStep2 was never answered")
	}
	pair.leaveTaskABehind(t, 2, model.Actor{Kind: "user", ID: "bob"}, "Bob's paragraph.\n")

	loaded, err := NewPgVersioned(pair.database).Load(ctx, pair.artifactID)
	if err != nil {
		t.Fatalf("load the stored document: %v", err)
	}
	if err := crdt.ApplyUpdateV1(alice.doc, loaded.Update, "remote"); err != nil {
		t.Fatalf("give alice's page bob's paragraph: %v", err)
	}
	typed := docstest.Transact(alice.doc, func(txn *crdt.Transaction) {
		texts := paragraphTextsOf(txn.GetXmlFragment(fragmentName))
		last := texts[len(texts)-1]
		last.Insert(txn, last.Len(), " Alice's ending.", nil)
	})
	alice.mu.Lock()
	alice.nextOrdinal++
	alice.rows = append(alice.rows, lcRow{ordinal: alice.nextOrdinal, update: typed})
	alice.mu.Unlock()
	alice.logf("types after bob's paragraph (%d bytes)", len(typed))
	alice.write(ygsync.EncodeUpdate(typed), &lcSent{update: typed, what: "Update after bob's paragraph"})
	if !lcWait(t, "alice's frame to be answered", 5*time.Second, func() bool { return len(alice.heard()) > 1 }) {
		t.Fatal("task A never answered alice's frame")
	}
	if heard := alice.heard(); heard[len(heard)-1] != "Update after bob's paragraph applied=false" {
		t.Fatalf("alice's frame was answered %q, want it parked and answered not applied", heard[len(heard)-1])
	}
	room := pair.taskA.srv.GetDoc(pair.artifactID)
	if parked := room.PendingStats(); parked.Items == 0 {
		t.Fatalf("task A's room parks %+v, want alice's frame behind bob's clocks", parked)
	}

	if outcome, err := pair.taskA.catchUpRoom(ctx, pair.database.Pool, pair.artifactID, room); err != nil || outcome != catchUpApplied {
		t.Fatalf("task A's catch-up answered %v, %v, want bob's rows applied", outcome, err)
	}
	if parked := room.PendingStats(); parked.Items != 0 {
		t.Fatalf("task A's room still parks %+v after the catch-up that supplies bob's clocks", parked)
	}
	carol := lcDial(t, pair.serverA.URL, pair.artifactID, "carol", nil, start, &lines, &linesMu)
	<-carol.synced
	carol.typeAt(0, "C")

	if !lcWait(t, "alice's row to clear", 10*time.Second, func() bool { return alice.rowsHeld() == 0 }) {
		t.Fatalf("alice's row never cleared: she heard %v", alice.heard())
	}
	own := alice.doc.ClientID()
	if stored, wrote := storedClock(t, pair, own), alice.doc.StateVector().Clock(own); stored < wrote {
		t.Fatalf("alice's row cleared, answered saved, while the store holds her client %d to clock %d of %d: her edit is the room's alone",
			own, stored, wrote)
	}
	t.Logf("alice heard %v, and the store holds her edit", alice.heard())
}

// storedClock is the clock the stored document holds client to.
func storedClock(t *testing.T, pair processPair, client crdt.ClientID) uint64 {
	t.Helper()
	loaded, err := NewPgVersioned(pair.database).Load(context.Background(), pair.artifactID)
	if err != nil {
		t.Fatalf("load the stored document: %v", err)
	}
	stored := newDocumentCopy()
	if err := crdt.ApplyUpdateV1(stored, loaded.Update, nil); err != nil {
		t.Fatalf("decode the stored document: %v", err)
	}
	return stored.StateVector().Clock(client)
}
