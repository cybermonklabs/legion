package docs

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc/schemanodes"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// setStoreSchemaVersion moves the version every block type in the store carries, which is the
// version the store's schema reads back as (schemanodes.Read).
func setStoreSchemaVersion(t *testing.T, database *store.Store, version int) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), `
		update graph_nodes
		set attributes = jsonb_set(attributes, '{version}', to_jsonb($1::int))
		where kind = $2 and coalesce((attributes->>'block_type')::boolean, false)
	`, version, schemanodes.KindSupertag); err != nil {
		t.Fatalf("set the store's schema version to %d: %v", version, err)
	}
}

// requireClosedForTheSchema reads a socket until it ends and fails unless it ends with the close
// a browser reads as "the schema moved" (documentSchemaCloseCode) within the wait. Frames the
// room sent before the close - its sync, an awareness update - are read past.
func requireClosedForTheSchema(t *testing.T, connection *gws.Conn, what string) {
	t.Helper()
	_ = connection.SetReadDeadline(time.Now().Add(roomWait(t)))
	for {
		_, message, err := connection.ReadMessage()
		if err == nil {
			continue
		}
		var closed *gws.CloseError
		if !errors.As(err, &closed) || closed.Code != documentSchemaCloseCode {
			t.Fatalf("%s ended with %q (%v), want the close %d a browser reads as the schema having moved", what, message, err, documentSchemaCloseCode)
		}
		return
	}
}

// A store schema above the version this build was made for installs nothing, and the process
// then serves no schema: it closes the document sockets it holds and refuses a new one, since a
// socket's admission was decided on a schema the process no longer serves. The installer runs
// as cmd/dispatch's main runs it - read once at boot (schemanodes.InstallFromStore), then again
// on each wake by an Installer whose OnSwitch closes the document sockets
// (Service.CloseDocumentSockets) - over a real socket of this package's own server.
//
// The socket is open before the installer starts, so whichever of its reads finds the store
// above this build - its first, or the one the wake makes - is the read that switches, and the
// switch is the one thing that can close the socket.
func TestAStoreSchemaAboveTheBuildClosesTheDocumentSocketsAndRefusesANewOne(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	service, server := newTestServiceInstance(t, database)
	artifactID := createDocuments(t, database, 1)[0]

	// The installed schema is the process's, not this test's: every test after this one reads
	// it, so the value installed before the test is put back, and the process is ready again.
	built := pmdoc.BuiltSchemaVersion()
	var before pmdoc.BlockSchema
	if err := json.Unmarshal(pmdoc.SchemaJSON(), &before); err != nil {
		t.Fatalf("decode the installed schema: %v", err)
	}
	t.Cleanup(func() {
		setStoreSchemaVersion(t, database, built)
		if err := pmdoc.Install(before); err != nil {
			t.Errorf("put the schema installed before this test back: %v", err)
		}
		if reason := pmdoc.NotReadyReason(); reason != "" {
			t.Errorf("the process is still not ready (%q) once the schema it served before this test is back", reason)
		}
	})

	// The boot's read, as the server makes it beside its migrations.
	if err := schemanodes.InstallFromStore(ctx, database.Pool); err != nil {
		t.Fatalf("install the store's schema at boot: %v", err)
	}
	if reason := pmdoc.NotReadyReason(); reason != "" || pmdoc.SchemaVersion() != built {
		t.Fatalf("after the boot's read the process serves version %d and is not ready because %q, want this build's version %d, ready",
			pmdoc.SchemaVersion(), reason, built)
	}
	open := admitSocket(t, server.URL, artifactID, "a socket opened on the schema this build serves")

	// The installer, wired as the server wires it.
	wake := make(chan struct{}, 1)
	installing, stopInstalling := context.WithCancel(ctx)
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		(&schemanodes.Installer{
			Store:    database.Pool,
			Wake:     wake,
			OnSwitch: func() { service.CloseDocumentSockets() },
		}).Run(installing)
	}()
	// Registered after the restore above, so it runs first: the installer is stopped before the
	// schema it would otherwise read again is put back.
	t.Cleanup(func() {
		stopInstalling()
		<-stopped
	})

	// A supertag write moves the store above this build, and announces itself.
	setStoreSchemaVersion(t, database, built+1)
	wake <- struct{}{}

	requireClosedForTheSchema(t, open, "the socket opened before the store moved above this build")
	reason := pmdoc.NotReadyReason()
	if !strings.Contains(reason, strconv.Itoa(built+1)) {
		t.Errorf("the process is not ready because %q, want a reason naming the store's version %d", reason, built+1)
	}
	if pmdoc.SchemaVersion() != built {
		t.Errorf("the installed schema is version %d, want this build's %d still: a schema above the build installs nothing", pmdoc.SchemaVersion(), built)
	}

	// A new socket is upgraded only to be closed with the same code, before any of the
	// document is sent.
	refused, response, err := gws.DefaultDialer.Dial(socketURL(server.URL, artifactID), http.Header{"X-Dispatch-User": []string{"alice"}})
	if err != nil {
		status := "no response"
		if response != nil {
			status = response.Status
		}
		t.Fatalf("open a socket while the store is above this build: %s, %v; want an upgrade the server closes", status, err)
	}
	t.Cleanup(func() { _ = refused.Close() })
	_ = refused.SetReadDeadline(time.Now().Add(roomWait(t)))
	_, message, err := refused.ReadMessage()
	var closed *gws.CloseError
	if !errors.As(err, &closed) || closed.Code != documentSchemaCloseCode {
		t.Errorf("the first frame of a socket opened while the store is above this build is %q (%v), want the close %d before any sync",
			message, err, documentSchemaCloseCode)
	}
}
