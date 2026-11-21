package docs

import (
	"context"
	"io"
	"net/http"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// A cold socket whose admission loaded its document, and whose room's own load then finds the
// rooms pool busy, is refused 503 as a socket its admission refused is: the pool being busy is
// load the server cannot serve now, and a browser retries a 503. ygo answers a room load that
// failed with its own 500 "room unavailable" (provider/websocket/server.go:2094), which is what
// a browser met 3,761 times in the tester's round-38 memory step (10x, 1,000 editors, a restart),
// each after 9 to 11 s, while the rooms pool was full: the load's pending-settlement read
// (readOwedSettlement, on the rooms pool) ran out of its acquire bound after the admission had
// already read the document.
func TestASocketWhoseRoomLoadFindsTheRoomsPoolBusyIsRefused503(t *testing.T) {
	database := storetest.Open(t)
	service, server := newTestServiceInstance(t, database)
	artifactID := createDocuments(t, database, 1)[0]
	rooms, err := database.Pool.Rooms()
	if err != nil {
		t.Fatalf("open the rooms pool: %v", err)
	}
	// The admission has read and rendered the document when this runs (admitDocument); from here
	// until the handshake is answered, every connection of the rooms pool is held, so the room's
	// own load meets a full pool.
	held := make(chan []*pgxpool.Conn, 1)
	service.admissionRendered = func(room string) {
		if room != artifactID {
			return
		}
		var conns []*pgxpool.Conn
		for range store.RoomsPoolSize {
			conn, err := rooms.Acquire(context.Background())
			if err != nil {
				t.Errorf("hold the rooms pool: %v", err)
				break
			}
			conns = append(conns, conn)
		}
		held <- conns
	}
	t.Cleanup(func() {
		select {
		case conns := <-held:
			for _, conn := range conns {
				conn.Release()
			}
		default:
		}
	})

	dialer := gws.Dialer{HandshakeTimeout: 60 * time.Second}
	start := time.Now()
	connection, response, err := dialer.Dial(socketURL(server.URL, artifactID), http.Header{"X-Dispatch-User": []string{"alice"}})
	took := time.Since(start)
	if connection != nil {
		_ = connection.Close()
	}
	if response == nil {
		t.Fatalf("the socket's handshake had no answer after %s: %v", took, err)
	}
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	t.Logf("TESTER the socket's handshake was answered %d %q after %s", response.StatusCode, body, took.Round(time.Millisecond))
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("a socket whose room load found the rooms pool busy was answered %d %q, want the 503 of a refused socket", response.StatusCode, body)
	}
}
