package docs

import (
	"bytes"
	"errors"
	"io"
	"runtime"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"
	"github.com/reearth/ygo/crdt"
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
	writeIssueClosed(t, service.store, "DOC-1", true)
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

// A read-only step 2 at or under unslottedApplyBytes is still decoded to answer it, and its
// decode stops at the apply's own value limit (Server.ReadOnlySyncStep2MaxValues,
// maxDecodedValuesPerApply), so a frame dense in values costs a read-only socket no more than a
// browser's unslotted apply may. Bob's read-only page sends a 2,000,016-byte step 2 of one struct
// holding 2,000,000 null values: it is answered not applied, and the process allocates a few
// mebibytes from its first byte to the answer. With the limit unset the same frame allocated
// 68,640,080 bytes (4,627,528 with it, measured 2026-11-19 in a scratch test that set the field
// both ways on the same build).
func TestAReadOnlySocketsStepTwoDenseInValuesDecodesNoFurtherThanTheApplysValueLimit(t *testing.T) {
	service, artifactID, serverURL := clientIDRoom(t)
	writeIssueClosed(t, service.store, "DOC-1", true)
	connection, _, err := gws.DefaultDialer.Dial(socketURL(serverURL, artifactID)+"&"+contracts.DocumentClientIdParameter+"=", pageHeader("bob"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	if _, named, _, err := docstest.ReadGivenClient(connection); err != nil || !named {
		t.Fatalf("bob's read-only socket was given no client id in its first frame: %v", err)
	}

	values := 2_000_000
	doc := crdt.New()
	array := doc.GetArray("a")
	doc.Transact(func(txn *crdt.Transaction) { array.Insert(txn, 0, make([]any, values)) })
	stepTwo := crdt.EncodeStateAsUpdateV1(doc, nil)
	if len(stepTwo) > unslottedApplyBytes || values <= maxDecodedValuesPerApply {
		t.Fatalf("the step 2 is %d bytes of %d values; want it at or under the %d bytes a read-only step 2 is decoded at, and past the %d-value limit",
			len(stepTwo), values, unslottedApplyBytes, maxDecodedValuesPerApply)
	}
	message := encoding.EncodeBytes(func(encoder *encoding.Encoder) {
		encoder.WriteVarString(artifactID)
		encoder.WriteVarUint(0)
		encoder.WriteVarUint(ygsync.MsgSyncStep2)
		encoder.WriteVarBytes(stepTwo)
	})
	runtime.GC()
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	sent := stats.TotalAlloc
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
	t.Logf("a %d-byte step 2 of %d values on a read-only socket was answered %d; the process allocated %d bytes from its first byte to the answer",
		len(stepTwo), values, answer, allocated)
	if answer != 0 {
		t.Fatalf("bob's read-only step 2 was answered %d, want not applied", answer)
	}
	// The read of the message is 2 MB; a decode of its 2,000,000 values allocates past 60 MB.
	if bound := uint64(16 << 20); allocated > bound {
		t.Fatalf("the process allocated %d bytes to answer bob's read-only step 2, past %d: its decode passed the value limit", allocated, bound)
	}
}
