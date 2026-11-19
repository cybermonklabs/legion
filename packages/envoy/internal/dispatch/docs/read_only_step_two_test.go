package docs

import (
	"bytes"
	"errors"
	"io"
	"runtime"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"
	"github.com/reearth/ygo/encoding"
	ygsync "github.com/reearth/ygo/sync"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/docs/docstest"
)

// A read-only socket's sync step 2 reaches no hook: ygo answers it, applied only where the room
// already holds all of it, by decoding the whole of it into a document of its own
// (crdt.DocContainsUpdateV1), outside every load slot, on a socket any signed-in person can open.
// So the room answers one past unslottedApplyBytes not applied without decoding it
// (Server.ReadOnlySyncStep2MaxBytes). Bob's page, admitted read-only on a closed issue and written
// to raw so that nothing on the sending side copies the message while the server reads it, sends a
// step 2 of 1,000,000 one-clock delete ranges: it is answered not applied, the socket stays open,
// and what the process allocates from the message's first byte to the answer is ygo's one read of
// the message and no more than the frames' headers besides, where a decode of the step 2 allocates
// several times the message again.
func TestAReadOnlySocketsLongStepTwoIsAnsweredWithoutADecode(t *testing.T) {
	service, artifactID, serverURL := clientIDRoom(t)
	setIssueClosed(t, service, true)
	dialer := *gws.DefaultDialer
	dialer.WriteBufferSize = 1 << 20
	connection, _, err := dialer.Dial(socketURL(serverURL, artifactID)+"&"+contracts.DocumentClientIdParameter+"=", pageHeader("bob"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	if _, named, _, err := docstest.ReadGivenClient(connection); err != nil || !named {
		t.Fatalf("bob's read-only socket was given no client id in its first frame: %v", err)
	}

	stepTwo := deleteRangesFrom(7_100_001, 1_000_000)
	if len(stepTwo) <= unslottedApplyBytes {
		t.Fatalf("the step 2 is %d bytes, at or under the %d a read-only step 2 is decoded at", len(stepTwo), unslottedApplyBytes)
	}
	// The Hocuspocus message: the document's name, the sync message type (0), then the sync step 2.
	message := encoding.EncodeBytes(func(encoder *encoding.Encoder) {
		encoder.WriteVarString(artifactID)
		encoder.WriteVarUint(0)
		encoder.WriteVarUint(ygsync.MsgSyncStep2)
		encoder.WriteVarBytes(stepTwo)
	})
	// What ygo's reader allocates for a message of that length, the one read of it the server
	// needs (provider/websocket/server.go, io.ReadAll): its growth follows the length alone.
	runtime.GC()
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	readStart := stats.TotalAlloc
	if read, err := io.ReadAll(bytes.NewReader(message)); err != nil || len(read) != len(message) {
		t.Fatalf("read the message back: %d bytes, %v", len(read), err)
	}
	runtime.ReadMemStats(&stats)
	ownRead := stats.TotalAlloc - readStart

	runtime.GC()
	runtime.ReadMemStats(&stats)
	sent := stats.TotalAlloc
	started := time.Now()
	if err := connection.WriteMessage(gws.BinaryMessage, message); err != nil {
		t.Fatal(err)
	}
	if err := connection.SetReadDeadline(time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	answer := -1
	for answer < 0 {
		_, read, err := connection.ReadMessage()
		if err != nil {
			var closed *gws.CloseError
			if errors.As(err, &closed) {
				t.Fatalf("the room closed bob's read-only socket %d %s, want its step 2 answered", closed.Code, closed.Text)
			}
			t.Fatalf("bob's read-only socket ended before its step 2 was answered: %v", err)
		}
		decoder := encoding.NewDecoder(read)
		if _, err := decoder.ReadVarString(); err != nil {
			continue
		}
		if kind, err := decoder.ReadVarUint(); err != nil || kind != 8 { // Hocuspocus's SyncStatus
			continue
		}
		if applied, err := decoder.ReadVarUint(); err == nil {
			answer = int(applied)
		}
	}
	runtime.ReadMemStats(&stats)
	allocated := stats.TotalAlloc - sent
	t.Logf("a %d-byte step 2 on a read-only socket in a %d-byte message was answered %d in %s; the process allocated %d bytes from its first byte to the answer, against %d for one read of the message (%.2f x)",
		len(stepTwo), len(message), answer, time.Since(started), allocated, ownRead, float64(allocated)/float64(ownRead))
	if answer != 0 {
		t.Fatalf("bob's read-only step 2 of %d bytes was answered %d, want not applied", len(stepTwo), answer)
	}
	// Past the read, the frames' headers and ygo's own few objects for the message, and what the
	// idle room and its pool allocate meanwhile: well under a mebibyte. A decode of the step 2
	// builds its million ranges, at least 16 bytes each, before it can answer.
	if slack := uint64(4 << 20); allocated > ownRead+slack {
		t.Fatalf("the process allocated %d bytes to answer bob's read-only step 2, past the %d its one read costs by more than %d: something decoded it",
			allocated, ownRead, slack)
	}
	if err := connection.SetReadDeadline(time.Now().Add(300 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	for {
		if _, _, err := connection.ReadMessage(); err != nil {
			var closed *gws.CloseError
			if errors.As(err, &closed) {
				t.Fatalf("the room closed bob's read-only socket %d %s after answering its step 2", closed.Code, closed.Text)
			}
			break
		}
	}
}
