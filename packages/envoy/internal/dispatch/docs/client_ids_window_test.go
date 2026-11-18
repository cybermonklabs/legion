package docs

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/docs/docstest"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// These tests hold the client id table's lifetime (client_ids.go): the claim window counted from
// the first boot of a build that gives ids out, and the sweep of ids no edit used.

// setClientIDsSince records since as when service's database first served a build that gives
// client ids out, as that boot would have.
func setClientIDsSince(t *testing.T, service *Service, since time.Time) {
	t.Helper()
	if _, err := service.store.Pool.Exec(context.Background(), `
		insert into document_client_ids_since (since) values ($1)
		on conflict (singleton) do update set since = excluded.since
	`, since); err != nil {
		t.Fatal(err)
	}
}

// clientIDLanded is whether the row of client in artifactID says an edit under it landed, and
// whether there is a row at all.
func clientIDLanded(t *testing.T, service *Service, artifactID string, client crdt.ClientID) (landed, stored bool) {
	t.Helper()
	err := service.store.Pool.QueryRow(context.Background(),
		`select landed from document_client_ids where artifact_id = $1 and client_id = $2`, artifactID, int64(client)).Scan(&landed)
	if err != nil {
		return false, false
	}
	return landed, true
}

// Within the 30 days the bound gives, a tab of the earlier build lands its edit under the id it
// drew; once they have passed, the same edit is refused, as the replay of a row saved under such
// an id would be.
func TestAnEarlierBuildsIDIsClaimableWithinTheWindowAndNotAfter(t *testing.T) {
	for _, tc := range []struct {
		name   string
		since  time.Duration
		claims bool
	}{
		{name: "a day after the deploy", since: 24 * time.Hour, claims: true},
		{name: "31 days after the deploy", since: 31 * 24 * time.Hour, claims: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			service, artifactID, serverURL := clientIDRoom(t)
			setClientIDsSince(t, service, time.Now().Add(-tc.since))
			tab := docstest.Dial(t, socketURL(serverURL, artifactID), pageHeader("alice"), artifactID, crdt.New(crdt.WithClientID(1_234_567_101)))
			awaitAnswer(t, tab, "the document")
			sendTyped(t, tab, 0, "o")
			if !tc.claims {
				awaitRefused(t, tab, "the earlier build's tab")
				return
			}
			awaitRendering(t, func() *crdt.Doc { return service.srv.GetDoc(artifactID) }, "# Title\n\noAlice's paragraph.\n", "the room")
			requireConnected(t, tab, "the earlier build's tab")
		})
	}
}

// The date the window counts from is the first boot's: a process that boots later records nothing
// over it.
func TestALaterBootDoesNotMoveWhenClientIDsWereFirstGivenOut(t *testing.T) {
	database := storetest.Open(t)
	first, _ := newTestServiceInstance(t, database)
	if err := first.RecordClientIDsSince(context.Background()); err != nil {
		t.Fatal(err)
	}
	var since time.Time
	if err := database.Pool.QueryRow(context.Background(), `select since from document_client_ids_since`).Scan(&since); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	later, _ := newTestServiceInstance(t, database)
	if err := later.RecordClientIDsSince(context.Background()); err != nil {
		t.Fatal(err)
	}
	var after time.Time
	if err := database.Pool.QueryRow(context.Background(), `select since from document_client_ids_since`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if !after.Equal(since) {
		t.Fatalf("a later boot moved the date from %s to %s", since, after)
	}
}

// An id no edit used and no admission presented for 30 days is swept, and a page presenting it
// after that is given a fresh one; an id an edit landed under is never swept, however long ago its
// page was last admitted, and is given back.
func TestAnIDNoEditUsedIsSweptAndGivenAfreshWhileOneThatWroteIsKept(t *testing.T) {
	service, artifactID, serverURL := clientIDRoom(t)
	reader := openPage(t, serverURL, artifactID, "bob")
	awaitAnswer(t, reader, "the document")
	writer := openPage(t, serverURL, artifactID, "alice")
	awaitAnswer(t, writer, "the document")
	sendTyped(t, writer, 0, "w")
	deadline := time.Now().Add(30 * time.Second)
	for landed, _ := clientIDLanded(t, service, artifactID, writer.Doc.ClientID()); !landed; landed, _ = clientIDLanded(t, service, artifactID, writer.Doc.ClientID()) {
		if time.Now().After(deadline) {
			t.Fatal("the writer's client id was never marked as having landed an edit")
		}
		time.Sleep(20 * time.Millisecond)
	}
	reader.Close()
	writer.Close()
	<-reader.Ended
	<-writer.Ended
	if _, err := service.store.Pool.Exec(context.Background(),
		`update document_client_ids set presented_at = now() - interval '31 days' where artifact_id = $1`, artifactID); err != nil {
		t.Fatal(err)
	}
	pool, err := service.store.Pool.Compaction()
	if err != nil {
		t.Fatal(err)
	}
	swept, err := sweepUnusedClientIDs(context.Background(), pool)
	if err != nil {
		t.Fatal(err)
	}
	if _, stored := clientIDLanded(t, service, artifactID, reader.Doc.ClientID()); stored || swept < 1 {
		t.Fatalf("the reader's unused id is still stored (the sweep deleted %d rows)", swept)
	}
	if landed, stored := clientIDLanded(t, service, artifactID, writer.Doc.ClientID()); !stored || !landed {
		t.Fatal("the sweep deleted the id an edit landed under")
	}
	url := socketURL(serverURL, artifactID) + "&" + contracts.DocumentClientIdParameter + "="
	back := docstest.DialGiven(t, url+strconv.FormatUint(uint64(reader.Doc.ClientID()), 10), pageHeader("bob"), artifactID, func(given crdt.ClientID) *crdt.Doc {
		return crdt.New(crdt.WithClientID(given))
	})
	awaitAnswer(t, back, "the document")
	if back.Given == reader.Doc.ClientID() {
		t.Fatalf("the reader's swept id %d was given back", back.Given)
	}
	requireGiven(t, back.Given)
	again := reopenPage(t, serverURL, artifactID, "alice", writer.Doc)
	awaitAnswer(t, again, "the document")
	sendTyped(t, again, 1, "x")
	awaitRendering(t, func() *crdt.Doc { return service.srv.GetDoc(artifactID) }, "# Title\n\nwxAlice's paragraph.\n", "the room")
}
