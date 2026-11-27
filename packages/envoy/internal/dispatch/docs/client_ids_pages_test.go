package docs

import (
	"context"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/docs/docstest"
)

// This file is the one place client_ids_test.go reaches a page of this build through: the same
// tests run against main's build with this file alone swapped for one that opens each page as
// main's editor does, with a client id it drew for itself, which is how they show what main
// refuses and what it does not.

// clientIDRefusedReason is the reason the room closes a socket whose update carries an edit under
// a client id that is not its sender's.
const clientIDRefusedReason = contracts.DocumentClientIdRefusedCloseReason

// pageHeader names user as the person a test's socket is signed in as.
func pageHeader(user string) http.Header {
	return http.Header{"X-Dispatch-User": []string{user}}
}

// openPage opens artifactID's document socket on serverURL as user's page of this build opens a
// document it has not held before: it presents no client id, takes the one the room's admission
// gives it, and makes its document under that id.
func openPage(t *testing.T, serverURL, artifactID, user string) *docstest.Peer {
	t.Helper()
	url := socketURL(serverURL, artifactID) + "&" + contracts.DocumentClientIdParameter + "="
	return docstest.DialGiven(t, url, pageHeader(user), artifactID, func(given crdt.ClientID) *crdt.Doc {
		return crdt.New(crdt.WithClientID(given))
	})
}

// reopenPage opens artifactID's document socket again for kept, the document user's page held on
// a socket that closed: the page presents the client id kept writes under, and the room gives it
// back, so the page carries on under it - its edits made while it was away included.
func reopenPage(t *testing.T, serverURL, artifactID, user string, kept *crdt.Doc) *docstest.Peer {
	t.Helper()
	url := socketURL(serverURL, artifactID) + "&" + contracts.DocumentClientIdParameter + "=" +
		strconv.FormatUint(uint64(kept.ClientID()), 10)
	page := docstest.DialGiven(t, url, pageHeader(user), artifactID, func(crdt.ClientID) *crdt.Doc { return kept })
	if page.Given != kept.ClientID() {
		t.Fatalf("the room gave %s's reopened page client id %d, want the %d it presented", user, page.Given, kept.ClientID())
	}
	return page
}

// requireGiven requires client, a page's id, to be one an admission gives out.
func requireGiven(t *testing.T, client crdt.ClientID) {
	t.Helper()
	if client < mintedClientFloor || client >= mintedClientCeiling {
		t.Fatalf("a page was given client id %d, outside [%d, %d)", client, uint64(mintedClientFloor), uint64(mintedClientCeiling))
	}
}

// storedClientIDs is how many rows store client as someone's in artifactID.
func storedClientIDs(t *testing.T, service *Service, artifactID string, client crdt.ClientID) int {
	t.Helper()
	var rows int
	if err := service.store.Pool.QueryRow(context.Background(),
		`select count(*) from document_client_ids where artifact_id = $1 and client_id = $2`, artifactID, int64(client)).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	return rows
}

// forgetStoredClientIDs deletes artifactID's stored client ids, leaving its history as a build
// before this one stored it: naming client ids no row does.
func forgetStoredClientIDs(t *testing.T, service *Service, artifactID string) {
	t.Helper()
	if _, err := service.store.Pool.Exec(context.Background(), `delete from document_client_ids where artifact_id = $1`, artifactID); err != nil {
		t.Fatal(err)
	}
}

// awaitRefused waits for the room to close page, and requires the close to be the client id
// refusal.
func awaitRefused(t *testing.T, page *docstest.Peer, who string) {
	t.Helper()
	select {
	case <-page.Ended:
	case <-time.After(30 * time.Second):
		t.Fatalf("%s's page stayed connected: the room did not refuse its update", who)
	}
	if closed := page.Closed(); closed == nil || closed.Code != contracts.DocumentTooLargeCloseCode || closed.Text != clientIDRefusedReason {
		t.Fatalf("the room closed %s's page with %v, want %d %s", who, closed, contracts.DocumentTooLargeCloseCode, clientIDRefusedReason)
	}
}

// requireConnected requires page to be connected still, after a short wait for a close in flight.
func requireConnected(t *testing.T, page *docstest.Peer, who string) {
	t.Helper()
	select {
	case <-page.Ended:
		t.Fatalf("the room closed %s's page with %v", who, page.Closed())
	case <-time.After(300 * time.Millisecond):
	}
}

// requireNoClientRefusal requires page, which may have closed for another reason, not to have
// been closed with the client id refusal.
func requireNoClientRefusal(t *testing.T, page *docstest.Peer, who string) {
	t.Helper()
	select {
	case <-page.Ended:
	default:
		return // still connected, so refused by nothing (Closed waits for the end)
	}
	if closed := page.Closed(); closed != nil && closed.Text == clientIDRefusedReason {
		t.Fatalf("the room refused %s's honest page: %v", who, closed)
	}
}
