package api

import (
	"encoding/json"
	"maps"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/evals"
	"github.com/sjawhar/envoy/internal/dispatch/legion"
)

// The newest read of a project's daemon, on its own (dispatch://INFRA-2918, D23): the route
// GET /api/v1/projects/{key}/legion/read and the live view kind legion-read answer the board
// header's read_at, read_ok and read_error through the one statement the header reads
// (readDaemonRead), and the view moves at each record of the daemon the Legion intake announces.

// legionReadSubscription is a subscribe frame's view of the newest read of project's daemon.
func legionReadSubscription(project string) DeliveryLiveView {
	return DeliveryLiveView{Kind: legionReadView, Project: &project}
}

// requireTheBoardsHeaderRead decodes held, the read a page holds, and requires it to be the read
// GET /api/v1/projects/{key}/legion/read answers and the one the board's header gives, field for
// field, at this moment; what names the moment in a failure.
func requireTheBoardsHeaderRead(t *testing.T, handler http.Handler, project, what string, held json.RawMessage) legionReadResponse {
	t.Helper()
	var live legionReadResponse
	if err := json.Unmarshal(held, &live); err != nil {
		t.Fatalf("%s: decode the read the page holds: %v", what, err)
	}
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/projects/"+project+"/legion/read", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("%s: GET the read of %s's daemon: status=%d body=%s", what, project, response.Code, response.Body.String())
	}
	routed := decodeBody[legionReadResponse](t, response)
	header := legionBoardOf(t, handler, project).Header
	board := legionReadResponse{ReadAt: header.ReadAt, OK: header.ReadOK, Error: header.ReadError}
	encoded := map[string]string{}
	for name, read := range map[string]legionReadResponse{"page": live, "route": routed, "board": board} {
		raw, err := json.Marshal(read)
		if err != nil {
			t.Fatalf("%s: encode the %s's read: %v", what, name, err)
		}
		encoded[name] = string(raw)
	}
	if encoded["page"] != encoded["route"] || encoded["route"] != encoded["board"] {
		t.Fatalf("%s: the page holds %s, the route answers %s and the board's header gives %s, want one read",
			what, encoded["page"], encoded["route"], encoded["board"])
	}
	return live
}

// nextReadDelta reads the next delta of sub, requires it to set exactly the members set names, and
// answers the read the page holds once it applies it.
func nextReadDelta(t *testing.T, conn *fakeLiveConn, sub string, held json.RawMessage, set ...string) json.RawMessage {
	t.Helper()
	delta := conn.delta(t, sub)
	if got, want := slices.Sorted(maps.Keys(delta.Set)), slices.Sorted(slices.Values(set)); !slices.Equal(got, want) {
		raw, _ := json.Marshal(delta)
		t.Fatalf("the delta sets %v, want %v: %s", got, want, raw)
	}
	return applyLiveDelta(t, legionReadView, held, delta)
}

// TestTheLegionReadViewAnswersTheBoardsHeaderRead: a legion-read view's snapshot and every delta a
// page applies leave it holding the read the route answers and the board's header gives, through
// the state table's three daemon-read rows: a good whole read, a whole read that failed, and a
// batch of state events the intake applies while that failure owes the read, which moves the
// read's time and keeps the failed read's ok and error (readDaemonRead's read_owed). Each whole
// read announces itself (legion.ReadWhole), and the batch is announced as the intake announces one.
func TestTheLegionReadViewAnswersTheBoardsHeaderRead(t *testing.T) {
	t.Parallel()
	rig := newHubRig(t, nil)
	seedLegionProjects(t, rig.handler, "INFRA")
	daemon := newFakeLegionDaemon(t, rig.handler, "INFRA")
	seedLegionIssue(t, rig.database, "INFRA", 1, "The issue", "a", nil, "")

	conn := newFakeLiveConn()
	rig.hub.Subscribe(conn, subscribeFrame("r", legionReadSubscription("INFRA")))
	held := conn.snapshot(t, "r").Body
	if never := requireTheBoardsHeaderRead(t, rig.handler, "INFRA", "a daemon never read", held); never.ReadAt != nil || never.OK || never.Error != nil {
		t.Fatalf("the read of a daemon never read = %+v, want no time, no success and no failure", never)
	}
	// The listener's connection and the relay's feed each mark the view (dirtyLegionViews,
	// applyFeed): both are answered before the daemon is read, so their recomputes read nothing
	// the rows below write.
	rig.waitFor(t, 0, "the listener's connection", listenerConnected)
	rig.settle(t)

	// A good whole read.
	daemon.read(t, rig.database, legionStateOf("INFRA", `"INFRA-1":{"key":"INFRA-1","phase":"planning","tree":"INFRA-1"}`))
	held = nextReadDelta(t, conn, "r", held, "read_at", "ok")
	if good := requireTheBoardsHeaderRead(t, rig.handler, "INFRA", "a good whole read", held); good.ReadAt == nil || !good.OK || good.Error != nil {
		t.Fatalf("the read after a good whole read = %+v, want its time and its success", good)
	}

	// A whole read that fails, which owes the read.
	daemon.read(t, rig.database, `not json`)
	held = nextReadDelta(t, conn, "r", held, "read_at", "ok", "error")
	failed := requireTheBoardsHeaderRead(t, rig.handler, "INFRA", "a failed whole read", held)
	if failed.OK || failed.Error == nil {
		t.Fatalf("the read after a failed whole read = %+v, want its failure", failed)
	}

	// A batch of state events while the read is owed: a legion_reads row of kind `events` that says
	// ok, at the daemon's commit time, an hour past Dispatch's clock. The read's time is the batch's,
	// and its ok and error are still the failed whole read's.
	batchAt := time.Now().UTC().Add(time.Hour).Truncate(time.Microsecond)
	legionExec(t, rig.database, `insert into legion_reads (project, read_at, ok, bytes, kind) values ('INFRA', $1, true, 512, 'events')`, batchAt)
	notifyChannel(t, rig.database, legion.StateChannel, "INFRA")
	held = nextReadDelta(t, conn, "r", held, "read_at")
	owed := requireTheBoardsHeaderRead(t, rig.handler, "INFRA", "a batch while the read is owed", held)
	if owed.OK || owed.Error == nil || *owed.Error != *failed.Error || owed.ReadAt == nil || !owed.ReadAt.Equal(batchAt) {
		t.Fatalf("the read after a batch while the read is owed = %+v, want the failed whole read's failure %q at the batch's time %s",
			owed, *failed.Error, batchAt.Format(time.RFC3339Nano))
	}
}

// TestTheLegionReadOfAProjectWithNoDaemonIsRefusedAsTheBoardIs: the daemon's read refuses what the
// board refuses, in its codes and its words: a project that names no daemon is LEGION_NOT_CONFIGURED
// at the route and at the live view, which is what draws no line in the Evals tab's header; a
// project that does not exist is PROJECT_NOT_FOUND, and a key that is no project key
// INVALID_PROJECT.
func TestTheLegionReadOfAProjectWithNoDaemonIsRefusedAsTheBoardIs(t *testing.T) {
	t.Parallel()
	rig := newHubRig(t, nil)
	seedLegionProjects(t, rig.handler, "INFRA")
	for _, refused := range []struct {
		project string
		status  int
		code    string
	}{
		{"INFRA", http.StatusNotFound, legionNotConfiguredCode},
		{"NOPE", http.StatusNotFound, "PROJECT_NOT_FOUND"},
		{"infra", http.StatusBadRequest, "INVALID_PROJECT"},
	} {
		board := dispatchRequest(t, rig.handler, http.MethodGet, "/api/v1/projects/"+refused.project+"/legion/board", nil, "alice")
		read := dispatchRequest(t, rig.handler, http.MethodGet, "/api/v1/projects/"+refused.project+"/legion/read", nil, "alice")
		boardAnswer, readAnswer := decodeBody[refusalAnswer](t, board), decodeBody[refusalAnswer](t, read)
		if read.Code != refused.status || readAnswer.Code != refused.code || readAnswer != boardAnswer || read.Code != board.Code {
			t.Errorf("GET the read of %s: %d %+v, want the board's %d %+v, %d %s",
				refused.project, read.Code, readAnswer, board.Code, boardAnswer, refused.status, refused.code)
		}
	}
	conn := newFakeLiveConn()
	rig.hub.Subscribe(conn, subscribeFrame("r", legionReadSubscription("INFRA")))
	if refusal := conn.refusal(t, "r", legionNotConfiguredCode); refusal.Message != legionNotConfiguredMessage {
		t.Errorf("the view of a project with no daemon is refused %q, want the route's %q", refusal.Message, legionNotConfiguredMessage)
	}
	rig.requireNothingSent(t, conn, "the refusal of a project with no daemon")
}

// TestAStateNotifyMarksTheLegionReadView: the Legion intake announces every batch of state events
// it applies and every whole read, failed or not, on legion.StateChannel with the project, and that
// announcement marks the project's legion-read view as it marks its board. It marks no other
// project's read, and no evals view, the project's legion kind among them: the read is a view of
// its own, so a daemon's batches recompute no eval figure. Each connection of the hub's listener
// marks every read view, as it marks every Legion view (dirtyLegionViews).
func TestAStateNotifyMarksTheLegionReadView(t *testing.T) {
	t.Parallel()
	hub := &deliveryHub{clock: newStillHubClock()}
	legionEvals := evalsViewOf("INFRA", 28, time.Time{}, time.Time{})
	legionEvals.spec.query = url.Values{"kind": {evals.KindLegion}}
	g := &hubGeneration{
		views: map[string]*deliveryView{
			"read":       {spec: deliveryLiveSpec{kind: legionReadView, project: "INFRA"}},
			"other read": {spec: deliveryLiveSpec{kind: legionReadView, project: "LINUX"}},
			"board":      boardViewShowing("INFRA"),
			"evals":      legionEvals,
		},
		wakeWork: make(chan struct{}, 1),
	}
	marked := func() map[string]bool {
		dirty := map[string]bool{}
		for key, view := range g.views {
			dirty[key] = view.dirty
			view.dirty = false
		}
		return dirty
	}

	hub.onNotify(g, legion.StateChannel, "INFRA")
	if got, want := marked(), map[string]bool{"read": true, "other read": false, "board": true, "evals": false}; !reflect.DeepEqual(got, want) {
		t.Errorf("an announcement of INFRA's daemon marks %v, want %v", got, want)
	}
	hub.dirtyLegionViews(g)
	if got, want := marked(), map[string]bool{"read": true, "other read": true, "board": true, "evals": false}; !reflect.DeepEqual(got, want) {
		t.Errorf("a connection of the listener marks %v, want %v", got, want)
	}
}
