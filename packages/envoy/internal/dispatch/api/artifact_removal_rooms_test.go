package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/docs/docstest"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// Two document services run against one database, as two Dispatch processes do, and both hold a
// room of the same untouched document. The removal goes through the first of them, and both
// rooms close: the removal's own transaction announces it to every process
// (docs.AnnounceDocumentRemoved), so the second process closes its room of a row that has gone
// rather than keeping one whose next append would meet a foreign key that no longer resolves
// (R10). A build that evicts only on the removing process fails here.
//
// A third process then holds a room of a second untouched document with its notification hub
// between connections, which is R10's "every process that holds one" against "every process
// listening at that instant": the announcement reaches that process never, and the room goes
// when its next LISTEN stands. Two passes of that LISTEN each close such a room, and the one
// that reaches it first does: the removal lane's, which reads the rows of this process's rooms
// and finds this one gone (docs.Service.recheckRemovedRooms), and the catch-up lane's sweep,
// which reads a stored history below the room's cursor because the removal took the document's
// stored updates with it (docs.roomCatchUps.sweep). A build with neither keeps a resident room
// over a deleted row for the rest of that process's life.
//
// The close carries no content: the removal is refused for anything but an untouched document,
// which the test reads as the document's one empty version before it removes anything.
func TestARemovedDocumentsRoomClosesOnEveryProcess(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	// Each of the first two processes settles on a fake clock of its own, which nothing moves
	// before both rooms have closed: the wait after the removal then moves each through every
	// deadline armed within reach, which runs every settlement either process still had armed.
	removingClock, holdingClock := clocktest.New(time.Now()), clocktest.New(time.Now())
	handler, database, deps := newTestServer(t, testServerOptions{documentClock: removingClock})
	removing, ok := deps.Docs.(*docs.Service)
	if !ok {
		t.Fatalf("the test server's document service is %T, which holds no rooms to read", deps.Docs)
	}
	runDocumentRoomSync(t, database, removing)
	// The second process: a document service of its own on a second pool, as cmd/dispatch runs
	// one, hearing the first's announcements over the database alone. It settles a settle delay
	// after a write, as the first does (newTestServer's 20 ms), on holdingClock, so no settlement
	// of it runs before the wait after the removal moves that clock. Its shutdown's drain budget
	// runs on that clock too, so the shared wait's context ends a shutdown that waits.
	secondStore := storetest.SecondProcess(t, database)
	holding := docs.New(docs.Deps{
		Store: secondStore, Identity: headerIdentity(secondStore), Settle: 20 * time.Millisecond, Clock: holdingClock,
	})
	t.Cleanup(func() {
		if err := docstest.Shutdown(t, testwait.Context(t), holding); err != nil {
			t.Errorf("shutdown the second process's document service: %v", err)
		}
	})
	runDocumentRoomSync(t, secondStore, holding)

	issue := createInteractionIssue(t, handler, "ROOM", "A document two processes hold", "# Spec\n")
	document := emptyDocument(t, handler, issue.Key, "Room notes")

	// The document is untouched, which is the only document this route removes: it holds one
	// version and that version is empty, so neither room below holds anything a person typed.
	var versions int
	var written string
	if err := database.Pool.QueryRow(ctx, `
		select count(*)::int, coalesce(string_agg(coalesce(markdown, ''), ''), '')
		from artifact_versions where artifact_id = $1::uuid
	`, document.ID).Scan(&versions, &written); err != nil {
		t.Fatalf("read the document's versions: %v", err)
	}
	if versions != 1 || written != "" {
		t.Fatalf("the document holds %d versions carrying %q; want the one empty version typing a title makes", versions, written)
	}

	// A person opens the document on the second process, which loads its room there: the room a
	// removal on the other process must close, and the person R10 leaves holding nothing live
	// over a row that has gone. Opening it types nothing, so the document is still untouched.
	secondServer := httptest.NewServer(http.HandlerFunc(holding.ServeHTTP))
	t.Cleanup(secondServer.Close)
	connection, wsResponse, err := dialLive(t, gws.DefaultDialer,
		"ws"+strings.TrimPrefix(secondServer.URL, "http")+"/ws/doc/"+document.ID,
		http.Header{"X-Dispatch-User": []string{"alice"}})
	if err != nil {
		t.Fatalf("open the document on the second process: response=%#v err=%v", wsResponse, err)
	}
	t.Cleanup(func() { _ = connection.Close() })

	// Each process is read holding a room of the document, so what is said of the two rooms
	// after the removal is said of rooms that existed. The second process's room loads as the
	// socket opens, and the load arms there the settlement the document still owes - the one its
	// creation armed on the removing process waits on removingClock - on holdingClock, which
	// nothing moves before the removal; the read after the wait for the room waits for that load.
	// The removing process loaded its own when the document was made, as the creation's
	// settlement warmed it: the row runs that settlement next, moving removingClock, so the room
	// the removal must close there is the one it warms.
	for _, process := range []struct {
		what    string
		service *docs.Service
		// settles is the clock the row runs the process's settlements on before it reads the room.
		settles *clocktest.Fake
	}{
		{what: "the second process", service: holding},
		{what: "the removing process", service: removing, settles: removingClock},
	} {
		if process.settles != nil {
			awaitSettledOn(t, process.what+" to run the settlements of the issue's and the document's creation",
				func() ([]string, bool) { return process.service.ResidentRooms(), true }, process.settles)
		}
		testwait.Until(t, fmt.Sprintf("%s to hold a room of %s, whose room the removal must close", process.what, document.ID),
			func() ([]string, bool) {
				rooms := process.service.ResidentRooms()
				return rooms, slices.Contains(rooms, document.ID)
			})
		if text, err := process.service.Text(ctx, document.ID); err != nil || text != "" {
			t.Fatalf("%s reads the live document as %q, %v; want the empty document it was made as", process.what, text, err)
		}
	}

	removedDocument(t, handler, document.ID)

	// The second process hears the removal over the database and closes its room. The wait is
	// for the notification's trip, not for a retry: nothing announces this twice.
	testwait.Until(t, fmt.Sprintf("the second process to close its room of the removed document %s, "+
		"whose next append meets a foreign key that no longer resolves", document.ID), func() ([]string, bool) {
		rooms := holding.ResidentRooms()
		return rooms, !slices.Contains(rooms, document.ID)
	})
	if rooms := removing.ResidentRooms(); slices.Contains(rooms, document.ID) {
		t.Fatalf("the removing process still holds the room of %s: %v", document.ID, rooms)
	}
	// The person who held it open on the second process is left holding no live room: the close
	// takes their socket with it, and their browser reconnects to a document that has gone.
	waitForDocumentConnectionClose(t, connection)
	// No append of either room reached the database after the removal, and neither process takes
	// the room up again. What could take it up is a settlement. The removing process had none
	// armed when the removal evicted its room, since the row ran them above, so one there is one
	// armed since, which waits on removingClock. The second process's room had one armed on
	// holdingClock, which its eviction ends, and which the close of its last socket would
	// otherwise run at once (settleLastPeer). The wait moves both clocks through every deadline
	// armed within reach, each move returning once the settlement it fired has, before the
	// appends and the rooms are read. No notification loads a room here: each room-sync handler
	// acts only on a room its process holds (catch-ups, the removal's close, room queries, the
	// issue's open state), but for an awareness update relayed for a reader, which can load an
	// absent room in the moment between its check and its apply (awarenessRelay.received), and
	// which comes only from a process holding a reader of the document: no process but the second.
	awaitSettledOn(t, "both processes to run every settlement armed when "+document.ID+" was removed",
		func() ([][]string, bool) { return [][]string{removing.ResidentRooms(), holding.ResidentRooms()}, true },
		removingClock, holdingClock)
	if appends := documentAppendCount(t, database, document.ID); appends > 0 ||
		slices.Contains(holding.ResidentRooms(), document.ID) ||
		slices.Contains(removing.ResidentRooms(), document.ID) {
		t.Fatalf("after the removal of %s the store holds %d appends of it, the second process the rooms %v and the removing process %v",
			document.ID, appends, holding.ResidentRooms(), removing.ResidentRooms())
	}
	assertNoRowNamesTheDocument(t, database, document.ID)

	// The third case holds the requirement's own words: every process that holds a room, and
	// not every process listening when the removal commits. A third process holds a room of a
	// second untouched document while its notification hub is between connections, so the
	// announcement reaches it never; the passes that run after a LISTEN stands read the room
	// instead, the removal lane's own among them (docs.Service.recheckRemovedRooms), so the
	// room goes when that hub connects again.
	untouched := emptyDocument(t, handler, issue.Key, "Notes a third process holds")
	thirdStore := storetest.SecondProcess(t, database)
	third := docs.New(docs.Deps{Store: thirdStore, Identity: headerIdentity(thirdStore), Settle: stagedAhead})
	t.Cleanup(func() { _ = docstest.Shutdown(t, context.Background(), third) })
	// The hub's first dial fails and every later one waits on through, so this process hears
	// nothing until the test lets it dial: the shape
	// docs.TestAnIssueClosedWhileTheListenerWasDownClosesItsRoomsOnTheReconnect drives the
	// issue-open lane with.
	through := make(chan struct{})
	var dials atomic.Int32
	dial := documentListenerDial(thirdStore, third)
	listening := runDocumentRoomSyncDialing(t, third, func(ctx context.Context) (*pgx.Conn, error) {
		if dials.Add(1) == 1 {
			return nil, errors.New("the listener hub has no connection")
		}
		select {
		case <-through:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return dial(ctx)
	})

	// A person opens the document on the third process, which loads its room there; a cold read
	// would load none. Opening it types nothing, so this is still the untouched document the
	// route removes, which the read below says.
	thirdServer := httptest.NewServer(http.HandlerFunc(third.ServeHTTP))
	t.Cleanup(thirdServer.Close)
	thirdConnection, thirdResponse, err := dialLive(t, gws.DefaultDialer,
		"ws"+strings.TrimPrefix(thirdServer.URL, "http")+"/ws/doc/"+untouched.ID,
		http.Header{"X-Dispatch-User": []string{"alice"}})
	if err != nil {
		t.Fatalf("open the document on the third process: response=%#v err=%v", thirdResponse, err)
	}
	t.Cleanup(func() { _ = thirdConnection.Close() })
	if text, err := third.Text(ctx, untouched.ID); err != nil || text != "" {
		t.Fatalf("the third process reads the live document as %q, %v; want the empty document it was made as", text, err)
	}
	testwait.Until(t, fmt.Sprintf("the third process to hold a room of %s, whose room the removal must close", untouched.ID),
		func() ([]string, bool) {
			rooms := third.ResidentRooms()
			return rooms, slices.Contains(rooms, untouched.ID)
		})

	removedDocument(t, handler, untouched.ID)

	// The announcement was sent while the third process had no connection, so it reached
	// nobody there and is never sent again. Its hub is let through, and its LISTEN is waited
	// for: what closes the room now is a pass that LISTEN runs.
	close(through)
	testwait.Receive(t, "the third process's listener hub to listen after the test let its dial through", listening)
	testwait.Until(t, fmt.Sprintf("the third process, whose hub was between connections when %s was removed, to close "+
		"its room of it after its LISTEN stood again (else it holds a resident room over a deleted row, and its next "+
		"append meets the doc_updates foreign key)", untouched.ID), func() ([]string, bool) {
		rooms := third.ResidentRooms()
		return rooms, !slices.Contains(rooms, untouched.ID)
	})
	assertNoRowNamesTheDocument(t, database, untouched.ID)
}

// documentAppendCount is how many stored appends the database holds of a document's room. After
// the document's removal it is zero: the rows went with the row they are keyed by, and an append
// a room left open over that row made afterwards could not be stored at all.
func documentAppendCount(t *testing.T, database *store.Store, artifactID string) int {
	t.Helper()
	var appends int
	if err := database.Pool.QueryRow(context.Background(),
		`select count(*)::int from doc_updates where artifact_id = $1::uuid`, artifactID).Scan(&appends); err != nil {
		t.Fatalf("count the document's stored appends: %v", err)
	}
	return appends
}
