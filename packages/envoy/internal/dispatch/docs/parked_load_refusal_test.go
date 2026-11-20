package docs

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// parkedRowClient is the client of the rows an earlier build stored of content that parked: none
// of the document's own clients, so what its rows carry waits on clocks the document never
// receives.
const parkedRowClient = 8

// A stored history whose decode parks one struct past the room's cap (maxParkedStructs) is one
// no room loads, so every read that judges whether it loads answers it as one that cannot
// (parkedPastRoomCaps), and the rebuild takes it from its own text.
func TestAHistoryParkingPastTheRoomsStructCapIsUnloadableUntilItsRebuildKeepsItsText(t *testing.T) {
	parks := maxParkedStructs + 1
	requireParkedPastCapUnloadable(t, parkedGCFrame(parkedRowClient, parks), &parkedPastCap{parked: parkedKindStructs, most: maxParkedStructs, parks: parks})
}

// The same for a stored history whose decode parks one delete range past the room's cap for
// them (maxParkedDeleteRanges).
func TestAHistoryParkingPastTheRoomsDeleteRangeCapIsUnloadableUntilItsRebuildKeepsItsText(t *testing.T) {
	parks := maxParkedDeleteRanges + 1
	requireParkedPastCapUnloadable(t, deleteRangesFrom(parkedRowClient, parks), &parkedPastCap{parked: parkedKindDeleteRanges, most: maxParkedDeleteRanges, parks: parks})
}

// requireParkedPastCapUnloadable stores a document's history as an earlier build's room stored
// it - its first write, an edit typed after its saved version that no version holds, and parked,
// a row of content that parked, which the room of that build stored - and opens and reads it
// twice. The history decodes under the service's own cap, and parks what want says, past the
// room's cap: the room's own decode refuses it.
//
// Each time, a read of the document (/text's, Text) answers ErrDocumentUnloadable naming the cap,
// and logs parkedPastCapReadLine; a socket opened on it is answered 503 at its admission, as a
// socket on a history that does not decode is, never ygo's 500 for a room whose load failed; a
// room's load (a settlement's warm-up) fails with crdt.ErrPendingCap and logs
// parkedPastCapLoadLine; an agent's write is refused ErrDocumentUnloadable; no room is resident;
// and the stored rows are as stored, byte for byte and each under the transaction that wrote it.
// Under the service's cap alone, every read served the document and every socket that opened it
// met ygo's 500 with nothing logged, and the rebuild was refused ErrDocumentLoads
// (dispatch://INFRA-179, round 10).
//
// The rebuild then takes it, with the cap and what the history parks as its reason, from the
// history's own text: the rebuilt document holds every edit the history rendered, the unsettled
// one included, and nothing it parked. A socket then opens on it and takes an edit that a second
// read sees.
func requireParkedPastCapUnloadable(t *testing.T, parked []byte, want *parkedPastCap) {
	logs := captureLogs(t)
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	ctx := context.Background()
	versioned := service.persistence.pgVersioned()
	writer := newProofHistory(t, 7)
	appendUpdates(t, versioned, artifactID,
		writer.write("# First\n"),
		writer.write("# First\n\nTyped after the saved version.\n"),
		parked)
	stored, err := versioned.Load(ctx, artifactID)
	if err != nil {
		t.Fatalf("load the stored history: %v", err)
	}
	history := newDocumentCopy()
	if err := decodeStoredState(history, stored.Update); err != nil {
		t.Fatalf("the stored history does not decode under the service's own cap, so nothing here is tested: %v", err)
	}
	stats := history.PendingStats()
	if parks := map[string]int{parkedKindStructs: stats.Items, parkedKindDeleteRanges: stats.DeleteRanges}[want.parked]; parks != want.parks || parks <= want.most {
		t.Fatalf("the stored history parks %d %s, want %d, past the room's %d: nothing here is tested", parks, want.parked, want.parks, want.most)
	}
	rendered, err := renderDocument(history)
	if err != nil || !strings.Contains(rendered, "Typed after the saved version.") {
		t.Fatalf("the stored history renders %q (%v), want the edit typed after the saved version", rendered, err)
	}
	asStored := storedRows(t, service.store, artifactID)
	if len(asStored) != 3 {
		t.Fatalf("the document holds %d rows, want the 3 appended", len(asStored))
	}
	server := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
	t.Cleanup(server.Close)
	readLine := fmt.Sprintf("level=WARN msg=%q room=%s cap=%q most=%d parks=%d\n", parkedPastCapReadLine, artifactID, "parked "+want.parked, want.most, want.parks)
	loadLine := fmt.Sprintf("level=ERROR msg=%q room=%s cap=%q most=%d parks=%d\n", parkedPastCapLoadLine, artifactID, "parked "+want.parked, want.most, want.parks)
	for access := 1; access <= 2; access++ {
		text, err := service.Text(ctx, artifactID)
		var refused *parkedPastCap
		if !errors.Is(err, ErrDocumentUnloadable) || errors.Is(err, ErrServiceUnavailable) || !errors.As(err, &refused) || *refused != *want || !errors.Is(err, crdt.ErrPendingCap) {
			t.Fatalf("access %d: a read of the document = %q (%v), want ErrDocumentUnloadable for the history parking past %+v", access, text, err, want)
		}
		requireLogged(t, logs, parkedPastCapReadLine, readLine, access)

		awaitRecovered(t, service, artifactID)
		opened := time.Now()
		connection, err := dialDocument(server.URL, artifactID)
		if connection != nil {
			_ = connection.Close()
		}
		if err == nil || !strings.HasPrefix(err.Error(), "503 ") {
			t.Fatalf("access %d: a socket opened on the document = %v, want the admission's 503", access, err)
		}
		if took := time.Since(opened); took > recoveryBound {
			t.Fatalf("access %d: the socket was answered after %s, want it within %s", access, took, recoveryBound)
		}

		awaitRecovered(t, service, artifactID)
		if err := service.warmLiveDocument(ctx, artifactID); !errors.Is(err, crdt.ErrPendingCap) {
			t.Fatalf("access %d: a room's load of the document = %v, want the room's own pending cap refusal", access, err)
		}
		requireLogged(t, logs, parkedPastCapLoadLine, loadLine, access)

		awaitRecovered(t, service, artifactID)
		_, err = joinedApplyOps(service, artifactID, []model.EditOp{{Op: "replace", Find: "Typed", With: "Retyped"}},
			model.Actor{Kind: "session", ID: "session-0123456789abcdef"}, nil)
		if !errors.Is(err, ErrDocumentUnloadable) || !errors.As(err, &refused) || *refused != *want {
			t.Fatalf("access %d: an agent's write to the document = %v, want ErrDocumentUnloadable for the history parking past %+v", access, err, want)
		}

		if service.srv.GetDoc(artifactID) != nil {
			t.Fatalf("access %d: a room holds the document, want none", access)
		}
		requireRowsAsStored(t, service.store, artifactID, asStored)
	}

	awaitRecovered(t, service, artifactID)
	report, _, err := rebuildDocument(t, service, artifactID, nil)
	if err != nil {
		t.Fatalf("rebuild the document: %v, want the rebuild to take a history no room loads", err)
	}
	if report.ValidationError != want.Error() {
		t.Fatalf("the rebuild says it rebuilt the history because %q, want %q", report.ValidationError, want.Error())
	}
	rebuilt := storedRows(t, service.store, artifactID)
	if len(rebuilt) != 1 || rebuilt[0].version <= asStored[len(asStored)-1].version {
		t.Fatalf("the rebuilt document holds rows at versions %v, want the rebuild's one row past %d", rowVersions(rebuilt), asStored[len(asStored)-1].version)
	}
	seed := newDocumentCopy()
	if err := decodeStoredState(seed, rebuilt[0].update); err != nil {
		t.Fatalf("decode the rebuilt document: %v", err)
	}
	if stats := seed.PendingStats(); stats.Items != 0 || stats.DeleteRanges != 0 {
		t.Fatalf("the rebuilt document parks %d structs and %d delete ranges, want none of what the history parked", stats.Items, stats.DeleteRanges)
	}
	if text := documentText(t, service, artifactID); text != rendered {
		t.Fatalf("the rebuilt document reads %q, want the %q the history rendered", text, rendered)
	}

	alice := connectPeer(t, server.URL, artifactID, "alice")
	if err := alice.WaitForText(peerMarkdown, rendered, 10*time.Second); err != nil {
		t.Fatalf("the rebuilt document's socket: %v", err)
	}
	typeOver(t, alice, "First", "Rebuilt")
	edited := strings.Replace(rendered, "# First", "# Rebuilt", 1)
	waitFor(t, 10*time.Second, "a second read to see the edit", func() bool {
		text, err := service.Text(ctx, artifactID)
		return err == nil && text == edited
	})
}

// requireLogged requires logs to hold access lines with msg, each line, the line from its level
// on.
func requireLogged(t *testing.T, logs *storetest.LockedLog, msg, line string, access int) {
	t.Helper()
	logged := logs.Lines(fmt.Sprintf("msg=%q", msg))
	lines := slices.Collect(strings.Lines(logged))
	if len(lines) != access {
		t.Fatalf("access %d: the log holds %d lines %q, want %d:\n%s", access, len(lines), msg, access, logged)
	}
	for _, logged := range lines {
		if !strings.HasSuffix(logged, line) {
			t.Fatalf("access %d: the log line %q, want it to end %q", access, logged, line)
		}
	}
}
