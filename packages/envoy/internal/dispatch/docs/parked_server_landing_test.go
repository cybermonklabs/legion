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
// commit, and is stored before its sender is told it is saved. It is driven through both server
// writes whose commit the room holds against the update its writer stored (written): a catch-up,
// the rows another process stored, and a live write's publish, the update this process's own
// write committed. Either is durable already, so the room stores its commit only where it carries
// more than its writer stored - here the parked frame the commit's drain landed (classifyDrained,
// carriesBeyond) - and resyncs the sender once that store has returned, so the SyncStep2 its
// answer clears the row with comes after the store. Before that rule the server write's commit was
// taken as stored, the landed content was in no row, and the next browser commit's store
// resynced the sender all the same: the row cleared, and the edit was the room's alone.
//
// Bob writes a paragraph that task A's room does not hold yet: from his browser on task B, which
// task A's room has not caught up to, or by a write on task A held after its commit and before its
// publish. Alice's page on task A takes bob's paragraph from the store, as a page that held it
// from another process's room would, and types after it; task A parks her frame behind bob's
// clocks and answers it not applied. Task A then catches up, or bob's write publishes, which lands
// her frame, and carol types on task A, whose store is the one that resynced alice before the
// rule. Once alice's row has cleared, the store holds her edit.
func TestAFrameParkedBehindAServerWritesClocksIsStoredBeforeItsSenderIsAnsweredSaved(t *testing.T) {
	bob := model.Actor{Kind: "user", ID: "bob"}
	for _, server := range []struct {
		name string
		// write has bob write his paragraph, which the store holds and task A's room does not
		// once it returns, and answers what lands the paragraph in that room.
		write func(t *testing.T, pair processPair) (land func(t *testing.T, room *crdt.Doc))
	}{
		{name: "a catch-up", write: func(t *testing.T, pair processPair) func(*testing.T, *crdt.Doc) {
			pair.leaveTaskABehind(t, 2, bob, "Bob's paragraph.\n")
			return func(t *testing.T, room *crdt.Doc) {
				if outcome, err := pair.taskA.catchUpRoom(context.Background(), pair.database.Pool, pair.artifactID, room); err != nil || outcome != catchUpApplied {
					t.Fatalf("task A's catch-up answered %v, %v, want bob's rows applied", outcome, err)
				}
			}
		}},
		{name: "a live write's publish", write: func(t *testing.T, pair processPair) func(*testing.T, *crdt.Doc) {
			committed, publish := make(chan struct{}), make(chan struct{})
			var held, released sync.Once
			release := func() { released.Do(func() { close(publish) }) }
			t.Cleanup(release)
			pair.taskA.checkRendering = func(at string, write *liveWrite) {
				if at == "publish" && write.artifactID == pair.artifactID {
					held.Do(func() {
						close(committed)
						<-publish
					})
				}
			}
			wrote := make(chan error, 1)
			go func() {
				_, err := joinedReplaceText(pair.taskA, pair.artifactID, "First.\n\nSecond.\n\nBob's paragraph.\n", bob)
				wrote <- err
			}()
			select {
			case <-committed:
			case err := <-wrote:
				t.Fatalf("bob's write on task A returned %v without reaching its publish", err)
			case <-time.After(roomWait(t)):
				t.Fatal("bob's write on task A never reached its publish")
			}
			return func(t *testing.T, _ *crdt.Doc) {
				release()
				select {
				case err := <-wrote:
					if err != nil {
						t.Fatalf("bob's write on task A: %v", err)
					}
				case <-time.After(roomWait(t)):
					t.Fatal("bob's write on task A never returned from its publish")
				}
			}
		}},
	} {
		t.Run(server.name, func(t *testing.T) {
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
			land := server.write(t, pair)

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

			land(t, room)
			if parked := room.PendingStats(); parked.Items != 0 {
				t.Fatalf("task A's room still parks %+v after %s supplied bob's clocks", parked, server.name)
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
		})
	}
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
