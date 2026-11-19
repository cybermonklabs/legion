package docs

// Review 8's repro, kept as a regression test. Design F claims "saved" means stored: a frame's
// answer waits for the store of the commit it made (Server.AnswerOnceStored). A frame whose apply
// committed nothing - a SyncStep2 that carries nothing the room lacks because the room already
// holds the browser's edit in memory - waited on no store, since only a commit armed the wait, and
// was answered applied at once, while the commit that held the edit was still waiting for its
// store. The editor clears every row recorded before such a SyncStep2 (pending-sync.ts, the lcPage
// model), so the edit showed saved before it was stored, and stayed shown saved when that store
// failed. Under theorem-labs/ygo v1.55.1-theorem.10 a frame that commits nothing is answered once
// every commit the room had made when it read the frame is stored, and the test holds that alice's
// reconnect does not clear her row for her keystroke while the store holds her below the clock she
// wrote, that store then failing.

import (
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"
)

func TestRV8AReconnectsSyncStep2IsAnsweredSavedBeforeTheCommitItCoversIsStored(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = 2 * time.Second
	var hold atomic.Bool
	release := make(chan struct{})
	service.beforeRoomAppend = func(string) error {
		if hold.CompareAndSwap(true, false) {
			select {
			case <-release:
			case <-time.After(15 * time.Second):
			}
			return errors.New("the test: the database refused the append")
		}
		return nil
	}
	seedServiceText(t, service, artifactID, "# Title\n\nParagraph.\n")
	serverURL := newTestServer(t, service).URL
	var lines []string
	var linesMu sync.Mutex
	t.Cleanup(func() {
		for _, line := range lines {
			t.Log(line)
		}
	})
	start := time.Now()
	stored := func(client crdt.ClientID) uint64 {
		loaded, err := service.persistence.Load(t.Context(), artifactID)
		if err != nil {
			t.Fatal(err)
		}
		return copyOf(t, loaded.Update).StateVector().Clock(client)
	}

	alice := lcDial(t, serverURL, artifactID, "alice", nil, start, &lines, &linesMu)
	bob := lcDial(t, serverURL, artifactID, "bob", nil, start, &lines, &linesMu)
	<-alice.synced
	<-bob.synced
	lcWait(t, "first syncs answered", 5*time.Second, func() bool {
		alice.mu.Lock()
		defer alice.mu.Unlock()
		return len(alice.sent) == 0
	})
	aliceID := alice.doc.ClientID()

	hold.Store(true)
	alice.typeAt(4, "L")
	if !lcWait(t, "bob to be relayed alice's L", 5*time.Second, func() bool { return strings.Contains(lcText(bob.doc), "L") }) {
		t.Fatal("the room never relayed alice's L")
	}
	// Alice's socket drops before her keystroke's answer (it waits on the held store).
	_ = alice.conn.Close()
	<-alice.ended
	if rows := alice.rowsHeld(); rows != 1 {
		t.Fatalf("alice holds %d rows before her reconnect, want her L kept", rows)
	}
	alice2 := lcDial(t, serverURL, artifactID, "alice", alice, start, &lines, &linesMu)
	<-alice2.synced
	answered := lcWait(t, "alice's reconnect SyncStep2 to be answered", 5*time.Second, func() bool {
		alice2.mu.Lock()
		defer alice2.mu.Unlock()
		return len(alice2.sent) == 0
	})
	rows := alice2.rowsHeld()
	storedAt := stored(aliceID)
	wrote := alice2.doc.StateVector().Clock(aliceID)
	t.Logf("while L's store is held: SyncStep2 answered=%t, alice's rows %d, store holds alice to clock %d of %d", answered, rows, storedAt, wrote)
	close(release)
	lcWait(t, "the failed room to close alice", 10*time.Second, func() bool {
		select {
		case <-alice2.ended:
			return true
		default:
			return false
		}
	})
	after := stored(aliceID)
	t.Logf("after the held store failed: store holds alice to clock %d of %d; alice's rows %d", after, wrote, alice2.rowsHeld())
	if rows == 0 && storedAt < wrote {
		t.Fatalf("alice's editor cleared her row for L (shown saved) while the store held her to clock %d of %d; the store then failed and holds %d", storedAt, wrote, after)
	}
}
