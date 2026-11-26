package docs

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc/pmdoctest"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc/schemanodes"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testcleanup"
)

// moveStoreAboveTheBuild applies, in one transaction, a type migration of the release after this
// build: it raises every block type's version to one past this build's, adds the typed block
// future, which that version introduces, and adds the typed block claim, which declares no since
// and so is read from version 1. The commit is the one moment the store changes, and the triggers
// of 20261109051232 announce it once.
func moveStoreAboveTheBuild(t *testing.T, database *store.Store, built int) {
	t.Helper()
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the type migration: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	const writer = `{"kind": "system", "id": "docs-schema-switch-test"}`
	for _, statement := range []struct {
		sql  string
		args []any
	}{
		{`insert into graph_nodes (kind, id, supertag, visibility, state, attributes, made_at, made_by, source_event)
		values
			('supertag', 'future', 'supertag', 'project', 'live',
				jsonb_build_object('block_type', true, 'content', 'paragraph+', 'render', 'host',
					'version', $2::int, 'position', 98, 'since', $2::int, 'creatable_since', $2::int),
				now(), $1::jsonb, 0),
			('supertag', 'claim', 'supertag', 'project', 'live',
				jsonb_build_object('block_type', true, 'content', 'paragraph+', 'render', 'host',
					'version', $2::int, 'position', 99),
				now(), $1::jsonb, 0)`, []any{writer, built + 1}},
		{`update graph_nodes set attributes = jsonb_set(attributes, '{version}', to_jsonb($1::int))
		where kind = $2 and coalesce((attributes->>'block_type')::boolean, false)`, []any{built + 1, schemanodes.KindSupertag}},
	} {
		if _, err := tx.Exec(ctx, statement.sql, statement.args...); err != nil {
			t.Fatalf("write the type migration: %v", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the type migration: %v", err)
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
			t.Fatalf("%s ended with %v (last frame %q), want the close %d the browser reads as a schema switch", what, err, message, documentSchemaCloseCode)
		}
		return
	}
}

// A store a newer build migrated, above the version this build was made for, is installed at this
// build's own version, with the elements that version reads and nothing else, and the process
// keeps serving (c-schema-holder). The install moved what the process serves - the claim type the
// same migration brought, which this build reads - so the switch closes the document socket that
// was admitted on the schema before it, with the code a browser reads as "the schema moved", and
// the socket the browser opens again is admitted. The installer runs as cmd/dispatch's main runs
// it - read once at boot (schemanodes.InstallFromStore), then again on each wake by an Installer
// whose OnSwitch closes the document sockets (Service.CloseDocumentSockets) - over a real socket
// of this package's own server.
func TestAStoreSchemaAboveTheBuildInstallsItsOwnVersionAndClosesTheDocumentSockets(t *testing.T) {
	ctx := context.Background()
	// The installed schema is the process's, not this test's: the hook puts back the schema
	// installed before the test once it is over, after the server's own cleanup has stopped it.
	pmdoctest.UseBlockSchema(t, installedSchema(t))
	database := storetest.Open(t)
	service, server := newTestServiceInstance(t, database)
	artifactID := createDocuments(t, database, 1)[0]
	built := pmdoc.BuiltSchemaVersion()

	// The boot's read, as the server makes it beside its migrations.
	if err := schemanodes.InstallFromStore(ctx, database.Pool); err != nil {
		t.Fatalf("install the store's schema at boot: %v", err)
	}
	if pmdoc.SchemaVersion() != built {
		t.Fatalf("after the boot's read the process serves version %d, want this build's %d", pmdoc.SchemaVersion(), built)
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
	testcleanup.Add(t, func() {
		stopInstalling()
		<-stopped
	})

	moveStoreAboveTheBuild(t, database, built)
	wake <- struct{}{}

	requireClosedForTheSchema(t, open, "the socket opened before the store moved above this build")
	if pmdoc.SchemaVersion() != built {
		t.Errorf("with the store at version %d the installed schema is version %d, want this build's own %d", built+1, pmdoc.SchemaVersion(), built)
	}
	if !pmdoc.IsTypedBlock("claim") {
		t.Errorf("with the store at version %d the claim type, read from version 1, is not installed", built+1)
	}
	if pmdoc.IsTypedBlock("future") {
		t.Errorf("with the store at version %d the typed block future, which that version introduces, is installed by a build of version %d", built+1, built)
	}

	// The browser connects again, and the process admits it: a store above the build takes
	// nothing out of service.
	admitSocket(t, server.URL, artifactID, "a socket opened once the store is above this build")
}

// A schema switch that comes after a socket's admission read its document and before the socket
// is held where the switch closes it (holdDocumentSocket) finds nothing to close, so the socket is
// closed as it is held: its admission counted the switches before it read the document, and a
// count that moved closes it with the code a browser reads as "the schema moved". The switch is
// run in that gap through the service's hook (afterSocketAdmitted), once; the socket opened after
// it is admitted.
func TestASchemaSwitchBetweenASocketsAdmissionAndItsHoldClosesTheSocket(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	service, server := newTestServiceInstance(t, database)
	artifactID := createDocuments(t, database, 1)[0]
	var switched atomic.Bool
	service.afterSocketAdmitted = func(string) {
		if switched.CompareAndSwap(false, true) {
			if closed := service.CloseDocumentSockets(); closed != 0 {
				t.Errorf("the switch in the gap closed %d sockets, want none: the socket under test is not held yet", closed)
			}
		}
	}

	overtaken, response, err := documentDialer(t).Dial(socketURL(server.URL, artifactID), http.Header{"X-Dispatch-User": []string{"alice"}})
	if err != nil {
		status := "no response"
		if response != nil {
			status = response.Status
		}
		t.Fatalf("open the socket a switch overtakes: %s, %v; want an upgrade the server closes", status, err)
	}
	testcleanup.Add(t, func() { _ = overtaken.Close() })
	requireClosedForTheSchema(t, overtaken, "the socket whose admission a switch overtook")
	if !switched.Load() {
		t.Fatalf("the switch never ran in the gap: the hook was not reached")
	}

	admitSocket(t, server.URL, artifactID, "a socket opened after the switch")
}

// installedSchema is the schema this process serves now, decoded.
func installedSchema(t *testing.T) pmdoc.BlockSchema {
	t.Helper()
	var value pmdoc.BlockSchema
	if err := json.Unmarshal(pmdoc.SchemaJSON(), &value); err != nil {
		t.Fatalf("decode the installed schema: %v", err)
	}
	return value
}
