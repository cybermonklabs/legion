package docs

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// roomRefreshRig is a service whose one session's file is a room alice reads only through the
// session's listener list, and the count of the refreshes the service has run.
type roomRefreshRig struct {
	service   *Service
	database  *store.Store
	file      string
	refreshes *atomic.Int64
}

// newRoomRefreshRig writes the rig: a session tied to nobody, listed with DOC (public), has a file;
// HID is private, so alice, an open viewer who sees DOC and not HID, does not see every document.
// The stored listener answer is an hour past its expires_at, and the refresh handed to the service
// makes it fresh, as the API's does, counting each run.
func newRoomRefreshRig(t *testing.T) roomRefreshRig {
	t.Helper()
	database := storetest.Open(t)
	ctx := context.Background()
	createDocument(t, database, "# First")
	service := newTestServiceOn(t, database)
	const session = "session-listed-untied"
	var file string
	if err := database.Pool.QueryRow(ctx, `
		insert into artifacts (session_id, slug, name, kind, created_by)
		values ($1, 'photo-png', 'photo.png', 'image', jsonb_build_object('kind', 'session', 'id', $1::text))
		returning id::text
	`, session).Scan(&file); err != nil {
		t.Fatalf("store the session's file: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into session_projects_filled default values;
		insert into route_roster (answered, sessions, sent_at, expires_at)
		values (true, '[{"session_id":"`+session+`","projects":["DOC"]}]', now() - interval '2 hours', now() - interval '1 hour');
		insert into projects (key, name, default_assignee) values ('HID', 'Hidden', 'carol@example.com');
		update projects set visibility = 'private' where key = 'HID';
	`); err != nil {
		t.Fatalf("seed the listener's expired answer and a private project: %v", err)
	}
	refreshes := &atomic.Int64{}
	service.SetRosterRefresh(func(ctx context.Context) {
		refreshes.Add(1)
		if _, err := database.Pool.Exec(ctx, `update route_roster set expires_at = now() + interval '1 hour'`); err != nil {
			t.Errorf("refresh the stored listener answer: %v", err)
		}
	})
	return roomRefreshRig{service: service, database: database, file: file, refreshes: refreshes}
}

// TestARoomReadRefreshesAnExpiredListenerAnswerFirst: a session's file follows the session
// content rule (access.DocumentClause through access.PartyClause), which reads a listener list
// only from a stored listener answer that has not passed its expires_at, and judges a listed
// session of an expired one as a session with no list, which a session tied to nobody passes for
// no viewer but its own. So the document service's read of which rooms a person still reads
// (readableRooms: each access re-check, the re-check of a late admission, and a socket's re-ask)
// runs the API's refresh first (SetRosterRefresh, refreshRosterForReader). alice reads the file's
// room. With no project private she reads every document, and the read refreshes nothing (R9).
//
// Controls: readableRooms without its refreshRosterForReader call, and alice does not read the
// file's room; refreshRosterForReader without its readsEveryDocument guard, and the read with no
// project private refreshes once.
func TestARoomReadRefreshesAnExpiredListenerAnswerFirst(t *testing.T) {
	rig := newRoomRefreshRig(t)
	ctx := context.Background()
	probe := []*schemaAdmission{{key: schemaAdmissionKey{room: rig.file}}}
	readable, err := rig.service.readableRooms(ctx, "alice", probe)
	if err != nil {
		t.Fatalf("read which rooms alice reads: %v", err)
	}
	if !readable[rig.file] || rig.refreshes.Load() != 1 {
		t.Errorf("HID private, the stored answer expired: alice reads the file's room %t after %d refreshes, want true after 1: the read must refresh the answer before the content rule reads it",
			readable[rig.file], rig.refreshes.Load())
	}

	if _, err := rig.database.Pool.Exec(ctx, `
		update projects set visibility = 'public' where key = 'HID';
		update route_roster set expires_at = now() - interval '1 hour';
	`); err != nil {
		t.Fatalf("make every project public and expire the answer: %v", err)
	}
	rig.refreshes.Store(0)
	if readable, err = rig.service.readableRooms(ctx, "alice", probe); err != nil {
		t.Fatalf("read which rooms alice reads, every project public: %v", err)
	}
	if !readable[rig.file] || rig.refreshes.Load() != 0 {
		t.Errorf("every project public: alice reads the file's room %t after %d refreshes, want true after none (R9)", readable[rig.file], rig.refreshes.Load())
	}
}

// TestASocketsAskRefreshesTheListenerAnswerOnce: a document socket's ask before its next data
// frame (socketStillReads) reads its person's rooms through readableRooms, which refreshes the
// stored listener answer first (refreshRosterForReader), so one ask runs one refresh, for an open
// person's socket while a project is private and for one that is not open alike, and still reads
// the room.
//
// Control: socketStillReads running the refresh itself before readableRooms (the code before);
// each ask refreshes twice and the test fails.
func TestASocketsAskRefreshesTheListenerAnswerOnce(t *testing.T) {
	for _, open := range []bool{true, false} {
		rig := newRoomRefreshRig(t)
		admission := &schemaAdmission{person: "alice", key: schemaAdmissionKey{room: rig.file}}
		ask := socketAnswer{open: open}
		if reads := rig.service.socketStillReads(context.Background(), admission, &ask); !reads || rig.refreshes.Load() != 1 {
			t.Errorf("HID private, a socket of alice's (open at admission %t) asks its rule: it reads the room %t after %d refreshes, want true after 1",
				open, reads, rig.refreshes.Load())
		}
	}
}
