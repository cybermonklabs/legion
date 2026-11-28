package api

// rls_sockets_test.go is the state table of the sockets of plan.md (INFRA-6168, "The state table
// of the sockets"): the live socket's events, agent and delivery views and the document socket
// under the viewer their caller resolves to, each made again when an access change moves what
// that caller sees. Each row builds a world of its own (newSocketWorld) on the fixture of
// rls_world_test.go, since most rows change an access list, a group or a table under it.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	gws "github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5"
	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/access"
	"github.com/sjawhar/envoy/internal/dispatch/agentstream"
	"github.com/sjawhar/envoy/internal/dispatch/docs/docstest"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/horizontest"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// rlsMover is a full member whose listing on rlsHush the socket rows write and take away. A
// person an access list's own write names must be an email (access.PersonPrincipal), and the
// world's other people are not, so a row that adds or removes a person through the access
// package's writes, which append project.access_changed and announce store.AccessChangedChannel
// as an owner's write does, names this one.
const rlsMover = "mo@rls.example"

// rlsNewProject is the public project a row makes while a limited member's socket is open.
const rlsNewProject = "NEWP"

// socketHeartbeat is the sockets' heartbeat in these rows (Deps.StreamHeartbeat): each socket
// resolves its caller again on it, which is the backstop for an access change no event or
// notification told it of. The pong deadline runs on a clock nobody moves (liveSocketClock), so a
// heartbeat this short ends no socket a loaded machine is slow to answer.
const socketHeartbeat = 300 * time.Millisecond

// socketQuiet is how long a row waits for a frame or an end that must not come. It is a lower
// bound on quiet: a loaded machine only shortens what it sees, so it never fails a correct server.
const socketQuiet = 5 * socketHeartbeat

// The agent frames a row writes and reads: a session's replay, and a frame it publishes.
const (
	socketReplay = `{"v":1,"kind":"replay","from":"the session's history"}`
	socketFrame  = `{"v":1,"kind":"turn","from":"the session's next batch"}`
)

// The two responder verdicts an agent view opens with.
const (
	agentNotResponding = `{"v":1,"responding":false}`
	agentResponding    = `{"v":1,"responding":true}`
)

// socketWorldOptions is what a row's world is built with.
type socketWorldOptions struct {
	// public is the deployment with no private project and no limited member: rlsOwner and
	// rlsMember, both full members, and rlsOpen and rlsList, both public. Otherwise the world is
	// rls_world_test.go's: its people and group, rlsOpen, rlsHush, rlsVeil and rlsList, with
	// their lists, rlsHush and rlsVeil private, and rlsMover beside them on no list.
	public bool
	// requestTracer sees every statement of the request store (rlsServerConfig.requestTracer).
	requestTracer pgx.QueryTracer
}

// socketWorld is one row's server, on a listener of its own, and the issue and document of each
// of its projects.
type socketWorld struct {
	*rlsServer
	// url is the listener's websocket origin, ws://host.
	url string
	// held gates every write the server makes on a connection its listener accepted (heldWrites),
	// for the row that holds a page's reads.
	held *heldWrites
	// stream is the agent conversation relay the server watches sessions through.
	stream *agentstream.Memory
	// issue is each project's first issue, and spec its primary document.
	issue, spec map[string]string
}

// newSocketWorld builds a row's world: the people, the server, each project with one issue and its
// spec written while every project is public, then the lists and the private projects.
func newSocketWorld(t *testing.T, options socketWorldOptions) *socketWorld {
	t.Helper()
	database := storetest.Open(t)
	projects := []string{rlsOpen, rlsList}
	if options.public {
		seedPeople(t, database, rlsOwner, rlsMember)
	} else {
		seedRLSPeople(t, database)
		seedPeople(t, database, rlsMover)
		projects = []string{rlsOpen, rlsHush, rlsVeil, rlsList}
	}
	stream := agentstream.NewMemory()
	server := newRLSServer(t, database, rlsServerConfig{
		streamHeartbeat: socketHeartbeat, liveSocketClock: clocktest.Held(), agentStream: stream, requestTracer: options.requestTracer,
	})
	url, held := serveSockets(t, server)
	world := &socketWorld{rlsServer: server, url: url, held: held, stream: stream, issue: map[string]string{}, spec: map[string]string{}}
	for _, project := range projects {
		world.asOK(t, rlsOwner, http.MethodPost, "/api/v1/projects", map[string]any{"key": project, "name": "The " + project + " project"}, http.StatusCreated)
		issue := createdIssue(t, "file "+project+"'s issue", world.as(t, rlsOwner, http.MethodPost, "/api/v1/issues", map[string]any{
			"project": project, "title": "The " + rlsWord[project] + " issue", "spec": "# Spec\n\nThe " + rlsWord[project] + " spec.\n", "force": true,
		}))
		world.issue[project], world.spec[project] = issue.Key, issue.PrimaryArtifactID
	}
	if !options.public {
		listOnProject(t, database, rlsHush, access.PrincipalGroup, rlsGroup)
		listOnProject(t, database, rlsVeil, access.PrincipalPerson, rlsListed)
		listOnProject(t, database, rlsList, access.PrincipalPerson, rlsListed)
		makeRLSPrivate(t, database, world.deps.Events, rlsHush)
		makeRLSPrivate(t, database, world.deps.Events, rlsVeil)
	}
	awaitSettledDocuments(t, database)
	world.awaitWork(t)
	return world
}

// serveSockets serves server on a listener of its own whose every accepted connection writes
// through a gate the row can hold (heldWrites), and answers its websocket origin and the gate.
func serveSockets(t *testing.T, server *rlsServer) (string, *heldWrites) {
	t.Helper()
	held := &heldWrites{}
	listener := httptest.NewUnstartedServer(server.handler)
	listener.Listener = held.listen(listener.Listener)
	listener.Start()
	t.Cleanup(listener.Close)
	return "ws://" + strings.TrimPrefix(listener.URL, "http://"), held
}

// dial opens login's live socket and reads it.
func (w *socketWorld) dial(t *testing.T, login string) *socketClient {
	t.Helper()
	return openLiveSocketAt(t, gws.DefaultDialer, w.url+liveSocketPattern, asUser(login)).servedBy(w.database).read()
}

// retitle renames project's issue as its owner, an issue.updated of the project the event feed
// carries, and answers title, which that event's payload holds and no other event's does.
func (w *socketWorld) retitle(t *testing.T, project, title string) string {
	t.Helper()
	w.asOK(t, rlsOwner, http.MethodPatch, "/api/v1/issues/"+w.issue[project], map[string]any{"title": title}, http.StatusOK)
	return title
}

// accessWrite runs write in one transaction of the system store with the server's broker, as an
// owner's write of an access list or a group runs: the access package's writes append their
// event (project.access_changed, access_group.changed) and announce store.AccessChangedChannel in
// it.
func (w *socketWorld) accessWrite(t *testing.T, what string, write func(ctx context.Context, tx pgx.Tx, broker *events.Broker, by model.Actor) error) {
	t.Helper()
	ctx := context.Background()
	tx, err := w.database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin %s: %v", what, err)
	}
	defer tx.Rollback(ctx)
	if err := write(ctx, tx, w.deps.Events, model.Actor{Kind: "user", ID: rlsOwner}); err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit %s: %v", what, err)
	}
}

// listMover names rlsMover on project's list through the access package's write.
func (w *socketWorld) listMover(t *testing.T, project string) {
	t.Helper()
	w.accessWrite(t, "list "+rlsMover+" on "+project, func(ctx context.Context, tx pgx.Tx, broker *events.Broker, by model.Actor) error {
		_, _, err := access.AddPrincipal(ctx, tx, broker, project, access.PrincipalPerson, rlsMover, by)
		return err
	})
}

// unlistMover takes rlsMover off project's list through the access package's write: an owner's
// removal, which appends project.access_changed and announces store.AccessChangedChannel.
func (w *socketWorld) unlistMover(t *testing.T, project string) {
	t.Helper()
	w.accessWrite(t, "take "+rlsMover+" off "+project, func(ctx context.Context, tx pgx.Tx, broker *events.Broker, by model.Actor) error {
		_, _, err := access.RemovePrincipal(ctx, tx, broker, project, access.PrincipalPerson, rlsMover, by)
		return err
	})
}

// unlistMoverBySQL takes rlsMover off project's list by SQL alone, appending no event: with
// announce it notifies store.AccessChangedChannel in the same transaction, a change only the
// notification tells of, and without it nothing tells any socket, a change only a heartbeat finds.
func (w *socketWorld) unlistMoverBySQL(t *testing.T, project string, announce bool) {
	t.Helper()
	ctx := context.Background()
	tx, err := w.database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the removal: %v", err)
	}
	defer tx.Rollback(ctx)
	tag, err := tx.Exec(ctx, `delete from project_access where project_key = $1 and principal_kind = 'person' and principal = $2`, project, rlsMover)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("take %s off %s by SQL: %v (%d rows)", rlsMover, project, err, tag.RowsAffected())
	}
	if announce {
		if err := store.AnnounceAccessChange(ctx, tx); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the removal: %v", err)
	}
}

// breakTheRule makes every read of the visibility rule fail from now on (access.Resolve reads
// project_access for every person) and announces store.AccessChangedChannel, so each socket reads
// its caller's sight again and that read fails.
func (w *socketWorld) breakTheRule(t *testing.T) {
	t.Helper()
	if _, err := w.database.Pool.Exec(context.Background(), `alter table project_access rename to project_access_unreadable`); err != nil {
		t.Fatalf("make the access list unreadable: %v", err)
	}
	notifyChannel(t, w.database, store.AccessChangedChannel, "")
}

// sessionActsIn records that session acted in project, as an act its own credential proved
// records it (session_projects), and announces it on events.SessionAccessChannel in the same
// transaction, as every write the session content rule reads announces itself.
func (w *socketWorld) sessionActsIn(t *testing.T, session, project string) {
	t.Helper()
	ctx := context.Background()
	tx, err := w.database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the act: %v", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `insert into session_projects (session_id, project_key) values ($1, $2)`, session, project); err != nil {
		t.Fatalf("record %s's act in %s: %v", session, project, err)
	}
	if _, err := tx.Exec(ctx, `select `+events.AnnounceSessionAccess); err != nil {
		t.Fatalf("announce %s's act: %v", session, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the act: %v", err)
	}
}

// socketEvent is what a row reads of the event a frame carries.
type socketEvent struct {
	ID         int64           `json:"id"`
	IssueKey   *string         `json:"issue_key"`
	ArtifactID *string         `json:"artifact_id"`
	Project    string          `json:"project"`
	Type       string          `json:"type"`
	Payload    json.RawMessage `json:"payload"`
}

// of reports whether event is one of project's in w: it names the project, one of its issues, or
// its issue's document.
func (w *socketWorld) of(event socketEvent, project string) bool {
	switch {
	case event.Project == project:
		return true
	case event.IssueKey != nil && strings.HasPrefix(*event.IssueKey, project+"-"):
		return true
	}
	return event.ArtifactID != nil && *event.ArtifactID == w.spec[project]
}

// requireNoneOf fails t for each of events of one of projects.
func (w *socketWorld) requireNoneOf(t *testing.T, what string, read []socketEvent, projects ...string) {
	t.Helper()
	for _, event := range read {
		for _, project := range projects {
			if w.of(event, project) {
				t.Errorf("%s: event %d (%s) of %s, which the caller does not see, reached the socket", what, event.ID, event.Type, project)
			}
		}
	}
}

// feedRead is what feedUntil read: every event, in order, and the end that stopped it, nil when
// the marker did.
type feedRead struct {
	events []socketEvent
	ended  *LiveEnded
}

// last is the id of the newest event read, or after when none was.
func (r feedRead) last(after int64) int64 {
	if len(r.events) == 0 {
		return after
	}
	return r.events[len(r.events)-1].ID
}

// feedUntil reads sub's frames on client until the event whose payload holds marker arrives or
// sub ends, waiting on the event feed (eventFeedFrame). Since the feed sends in id order and stops
// at an end, an end read before the marker came before every event the marker follows.
func feedUntil(t *testing.T, client *socketClient, sub, marker string) feedRead {
	t.Helper()
	var read feedRead
	for {
		switch frame := eventFeedFrame(t, client, "the event holding "+strconv.Quote(marker)+" or the end of "+sub).(type) {
		case LiveEventFrame:
			if frame.Sub != sub {
				t.Fatalf("an event of %q reached the socket, want only %q's", frame.Sub, sub)
			}
			var event socketEvent
			if err := json.Unmarshal(frame.Event, &event); err != nil {
				t.Fatalf("decode %s's event %s: %v", sub, frame.ID, err)
			}
			read.events = append(read.events, event)
			if bytes.Contains(event.Payload, []byte(marker)) {
				return read
			}
		case LiveEnded:
			if frame.Sub != sub {
				t.Fatalf("ended %+v, want only %q's frames", frame, sub)
			}
			read.ended = &frame
			return read
		default:
			t.Fatalf("frame = %+v, want %s's events or its end", frame, sub)
		}
	}
}

// requireEndedBehind fails t unless read stopped at sub's end, behind.
func requireEndedBehind(t *testing.T, what string, read feedRead) {
	t.Helper()
	if read.ended == nil {
		t.Fatalf("%s: the socket sent the marker with no ended frame before it, want the subscription ended (behind)", what)
	}
	if read.ended.Reason != liveEndedBehind {
		t.Fatalf("%s: the subscription ended %q, want %q", what, read.ended.Reason, liveEndedBehind)
	}
}

// requireMarker fails t unless read stopped at its marker, with no end before it.
func requireMarker(t *testing.T, what string, read feedRead) {
	t.Helper()
	if read.ended != nil {
		t.Fatalf("%s: the subscription ended %q before the marker, want it to stay open", what, read.ended.Reason)
	}
}

// awaitEnded reads sub's frames until it ends, answering the events read before the end. Nothing
// but the server's own recheck ends it, so the wait's one bound is the test binary's deadline.
func awaitEnded(t *testing.T, client *socketClient, sub string) feedRead {
	t.Helper()
	var read feedRead
	for read.ended == nil {
		switch frame := client.next(t, "the end of "+sub).(type) {
		case LiveEventFrame:
			var event socketEvent
			if err := json.Unmarshal(frame.Event, &event); err != nil {
				t.Fatalf("decode %s's event %s: %v", sub, frame.ID, err)
			}
			read.events = append(read.events, event)
		case LiveEnded:
			if frame.Sub != sub {
				t.Fatalf("ended %+v, want %q's", frame, sub)
			}
			read.ended = &frame
		default:
			t.Fatalf("frame = %+v while waiting for the end of %s", frame, sub)
		}
	}
	return read
}

// requireNothingOfAfterResubscribe subscribes sub again after the end, resuming after since as the
// page does, and fails t when any event of hidden reaches it, in the replay or live: hidden is
// written to once more, and the marker, of marked, a project the caller still sees, is read.
func (w *socketWorld) requireNothingOfAfterResubscribe(t *testing.T, client *socketClient, sub string, since int64, hidden, marked, row string) {
	t.Helper()
	subscribeEventsStart(t, client, sub, &since)
	w.retitle(t, hidden, row+": "+hidden+" retitled after the resubscribe")
	marker := w.retitle(t, marked, row+": "+marked+" retitled after the resubscribe")
	read := feedUntil(t, client, sub, marker)
	requireMarker(t, row+" after the resubscribe", read)
	w.requireNoneOf(t, row+" after the resubscribe", read.events, hidden)
}

// viewerStatements counts the statements recorder saw that set a viewer's settings (plan D2): the
// first statement of every read and write the request pool runs under a viewer.
func viewerStatements(recorder *storetest.StatementRecorder) int {
	count := 0
	for _, statement := range recorder.Statements() {
		if strings.Contains(statement, "dispatch.viewer_") {
			count++
		}
	}
	return count
}

// roomURL is the document websocket of document's room on w's listener.
func (w *socketWorld) roomURL(document string) string {
	return w.url + "/ws/doc/" + document + "?schema_version=" + strconv.Itoa(pmdoc.SchemaVersion())
}

// openRoom connects login to document's room and waits for the room's answer to the peer's sync
// step 1: the socket is admitted.
func (w *socketWorld) openRoom(t *testing.T, login, document string) *docstest.Peer {
	t.Helper()
	peer := docstest.Dial(t, w.roomURL(document), asUser(login), document, crdt.New())
	testwait.Receive(t, login+"'s answer from the room of "+document, peer.Answers)
	return peer
}

// roomRefusal dials document's room as login and answers the status and body the upgrade was
// refused with, failing t when the room admits the socket.
func (w *socketWorld) roomRefusal(t *testing.T, login, document string) (int, string) {
	t.Helper()
	conn, response, err := dialLive(t, gws.DefaultDialer, w.roomURL(document), asUser(login))
	if err == nil {
		_ = conn.Close()
		t.Fatalf("%s was admitted to the room of %s, want the upgrade refused", login, document)
	}
	if response == nil {
		t.Fatalf("dial the room of %s: %v, with no response", document, err)
	}
	body, _ := io.ReadAll(response.Body)
	return response.StatusCode, strings.TrimSpace(string(body))
}

// requireOpenRoom fails t when peer's socket has ended.
func requireOpenRoom(t *testing.T, what string, peer *docstest.Peer) {
	t.Helper()
	select {
	case <-peer.Ended:
		t.Fatalf("%s: the room closed the socket (close frame %d), want it open", what, peer.CloseFrame())
	default:
	}
}

// closedWith waits for peer's socket to end, its one bound the test binary's deadline, and answers
// the code of the close frame the room sent, 0 for none.
func closedWith(t *testing.T, what string, peer *docstest.Peer) int {
	t.Helper()
	testwait.Receive(t, what, peer.Ended)
	return peer.CloseFrame()
}

// hubViewCount is how many views the delivery hub holds, each shared by every sub that names it.
func hubViewCount(hub *deliveryHub) int {
	hub.mu.Lock()
	defer hub.mu.Unlock()
	if hub.generation == nil {
		return 0
	}
	return len(hub.generation.views)
}

// deliveryAnswer reads client's frames until sub's first answer, a snapshot or a refusal, passing
// over the freshness frames that follow another sub's snapshot.
func deliveryAnswer(t *testing.T, client *socketClient, sub string) any {
	t.Helper()
	for {
		switch frame := client.next(t, "the answer to "+sub).(type) {
		case DeliveryLiveFreshness:
		case DeliveryLiveSnapshot:
			if frame.Sub == sub {
				return frame
			}
		case LiveError:
			if frame.Sub == sub {
				return frame
			}
		default:
			t.Fatalf("frame = %+v while waiting for the answer to %s", frame, sub)
		}
	}
}

// snapshotOf is sub's snapshot, failing t on a refusal.
func snapshotOf(t *testing.T, client *socketClient, sub string) DeliveryLiveSnapshot {
	t.Helper()
	snapshot, ok := deliveryAnswer(t, client, sub).(DeliveryLiveSnapshot)
	if !ok {
		t.Fatalf("%s was refused, want its snapshot", sub)
	}
	return snapshot
}

// prIDs is the pull requests a snapshot holds, by item id.
func prIDs(snapshot DeliveryLiveSnapshot) []string {
	return slices.Sorted(maps.Keys(snapshot.Revs.Items["prs"]))
}

// seedTimeline writes the delivery settings and two merged pull requests: acme/widgets#1 of
// rlsOpen's issue and acme/widgets#2 of rlsHush's, and answers the timeline view of the last two
// days that holds both.
func (w *socketWorld) seedTimeline(t *testing.T) DeliveryLiveView {
	t.Helper()
	seedHubSettings(t, w.database)
	now := time.Now().UTC()
	w.upsertIssuePR(t, 1, rlsOpen, now.Add(-3*time.Hour))
	if _, private := w.issue[rlsHush]; private {
		w.upsertIssuePR(t, 2, rlsHush, now.Add(-2*time.Hour))
	}
	return timelineRange(now.Add(-48*time.Hour), now.Add(time.Hour), "")
}

// upsertIssuePR writes acme/widgets#n, merged at merged, as the pull request of project's issue.
func (w *socketWorld) upsertIssuePR(t *testing.T, n int, project string, merged time.Time) {
	t.Helper()
	pr := hubPR(n, merged)
	key := w.issue[project]
	pr.IssueKey = &key
	upsertPR(t, w.database, pr)
}

// requireNoHiddenDelivery fails t when frame, one frame a delivery sub was sent, holds a pull
// request or an issue of rlsHush.
func (w *socketWorld) requireNoHiddenDelivery(t *testing.T, what string, frame any) {
	t.Helper()
	encoded, err := json.Marshal(frame)
	if err != nil {
		t.Fatalf("encode %T: %v", frame, err)
	}
	for _, mark := range []string{"acme/widgets#2", "acme/widgets#3", w.issue[rlsHush]} {
		if bytes.Contains(encoded, []byte(mark)) {
			t.Errorf("%s holds %s, of %s, which the caller does not see: %.300s", what, mark, rlsHush, encoded)
		}
	}
}

// TestLiveAndDocumentSocketsEnforceViewerAndRecheckOnAccessChange (plan R4, D11): the live socket
// sends each view only what its caller sees, and the document socket admits only a room its
// caller reads; an access change, told by its event, by store.AccessChangedChannel or
// events.SessionAccessChannel, or found at a heartbeat, makes each socket read its caller's sight
// again and end what that sight no longer covers. With no private project and no limited member
// every socket behaves as on main (R5): rows L7, D7, A3 and V3.
func TestLiveAndDocumentSocketsEnforceViewerAndRecheckOnAccessChange(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name string
		run  func(t *testing.T)
	}{
		{"L1_a_viewer_who_does_not_see_priv_is_sent_no_priv_event", socketRowL1},
		{"L2_removed_from_priv_ends_with_behind", socketRowL2},
		{"L3_added_to_priv_ends_with_behind_then_is_sent_priv", socketRowL3},
		{"L4_heartbeat_finds_a_missed_change_and_ends_with_behind", socketRowL4},
		{"L4b_access_notification_ends_with_behind", socketRowL4b},
		{"L5_a_resume_replays_no_hidden_event", socketRowL5},
		{"L6_limited_member_is_sent_no_event_of_a_new_unlisted_project", socketRowL6},
		{"L7_all_public_frames_as_on_main_with_no_per_batch_read", socketRowL7},
		{"L8_a_failed_resolve_closes_the_socket", socketRowL8},
		{"L9_session_access_notification_ends_the_agent_view", socketRowL9},
		{"L10_taken_out_of_the_group_ends_with_behind", socketRowL10},
		{"L11_an_overflow_is_caught_up_past_a_hidden_newest_event", socketRowL11},
		{"D1_hidden_room_answers_as_a_room_nobody_holds", socketRowD1},
		{"D2_a_room_the_viewer_reads_is_admitted", socketRowD2},
		{"D3_lost_project_closes_with_4412", socketRowD3},
		{"D4_a_change_that_keeps_the_room_stays_open", socketRowD4},
		{"D5_missed_notification_closes_at_the_heartbeat", socketRowD5},
		{"D6_a_failed_recheck_read_closes", socketRowD6},
		{"D7_all_public_stays_open_with_no_room_read", socketRowD7},
		{"A1_hidden_session_answers_as_one_nobody_holds", socketRowA1},
		{"A2_session_acting_in_a_hidden_project_ends_before_the_next_batch", socketRowA2},
		{"A3_all_public_agent_view_as_on_main", socketRowA3},
		{"A4_removed_from_priv_ends_the_agent_view_with_behind", socketRowA4},
		{"V1_hidden_project_or_issue_answers_as_missing", socketRowV1},
		{"V2_two_sights_get_their_own_bodies", socketRowV2},
		{"V3_all_public_one_shared_view", socketRowV3},
		{"V4_removed_from_priv_ends_the_view_with_behind", socketRowV4},
		{"V5_missed_notification_ends_the_view_at_the_heartbeat", socketRowV5},
	} {
		t.Run(row.name, row.run)
	}
}

// L1: bob, who sees rlsOpen and not rlsHush, subscribes; a write of rlsHush and then one of
// rlsOpen commit. The feed sends rlsOpen's and nothing of rlsHush's before it.
func socketRowL1(t *testing.T) {
	w := newSocketWorld(t, socketWorldOptions{})
	client := w.dial(t, rlsMember)
	subscribeEvents(t, client, "e", nil)
	w.retitle(t, rlsHush, "L1: HUSH retitled where bob cannot see it")
	marker := w.retitle(t, rlsOpen, "L1: OPEN retitled for bob")
	read := feedUntil(t, client, "e", marker)
	requireMarker(t, "bob's feed", read)
	w.requireNoneOf(t, "bob's feed", read.events, rlsHush, rlsVeil)
}

// L2: rlsMover, on rlsHush's list, subscribes and is sent rlsHush's write. The owner takes him off
// the list (project.access_changed); the subscription ends (behind) before any later event, and
// nothing of rlsHush reaches him then or after his resubscribe.
func socketRowL2(t *testing.T) {
	w := newSocketWorld(t, socketWorldOptions{})
	w.listMover(t, rlsHush)
	client := w.dial(t, rlsMover)
	since := subscribeEvents(t, client, "e", nil)
	seen := feedUntil(t, client, "e", w.retitle(t, rlsHush, "L2: HUSH retitled while mo sees it"))
	requireMarker(t, "mo's feed while he sees HUSH", seen)
	w.unlistMover(t, rlsHush)
	w.retitle(t, rlsHush, "L2: HUSH retitled after mo lost it")
	after := feedUntil(t, client, "e", w.retitle(t, rlsOpen, "L2: OPEN retitled after mo lost HUSH"))
	requireEndedBehind(t, "mo's feed after he lost HUSH", after)
	w.requireNoneOf(t, "mo's feed after he lost HUSH", after.events, rlsHush)
	w.requireNothingOfAfterResubscribe(t, client, "e", after.last(seen.last(since)), rlsHush, rlsOpen, "L2")
}

// L3: rlsMover, on no list of rlsHush's, subscribes; the owner names him on rlsHush's list. The
// subscription ends (behind) before any later event, and after his resubscribe rlsHush's next
// write reaches him.
func socketRowL3(t *testing.T) {
	w := newSocketWorld(t, socketWorldOptions{})
	client := w.dial(t, rlsMover)
	since := subscribeEvents(t, client, "e", nil)
	w.listMover(t, rlsHush)
	read := feedUntil(t, client, "e", w.retitle(t, rlsOpen, "L3: OPEN retitled after mo was listed on HUSH"))
	requireEndedBehind(t, "mo's feed after he was listed on HUSH", read)
	since = read.last(since)
	subscribeEventsStart(t, client, "e", &since)
	sent := feedUntil(t, client, "e", w.retitle(t, rlsHush, "L3: HUSH retitled now mo sees it"))
	requireMarker(t, "mo's feed after the resubscribe", sent)
}

// L4: rlsMover, on rlsHush's list, subscribes; his listing is deleted by SQL alone, which appends
// no event and announces nothing. The heartbeat reads his sight again and ends the subscription
// (behind); nothing of rlsHush reaches him after his resubscribe.
func socketRowL4(t *testing.T) {
	socketRowMissedChange(t, false)
}

// L4b: as L4, with store.AccessChangedChannel announced by the removal and no event read: the
// notification ends the subscription (behind).
func socketRowL4b(t *testing.T) {
	socketRowMissedChange(t, true)
}

// socketRowMissedChange is L4 and L4b: an access change no event carries, announced or not.
func socketRowMissedChange(t *testing.T, announce bool) {
	w := newSocketWorld(t, socketWorldOptions{})
	w.listMover(t, rlsHush)
	client := w.dial(t, rlsMover)
	since := subscribeEvents(t, client, "e", nil)
	seen := feedUntil(t, client, "e", w.retitle(t, rlsHush, "HUSH retitled while mo sees it"))
	requireMarker(t, "mo's feed while he sees HUSH", seen)
	w.unlistMoverBySQL(t, rlsHush, announce)
	ended := awaitEnded(t, client, "e")
	if ended.ended.Reason != liveEndedBehind {
		t.Fatalf("mo's feed ended %q after he lost HUSH, want %q", ended.ended.Reason, liveEndedBehind)
	}
	w.requireNoneOf(t, "mo's feed after he lost HUSH", ended.events, rlsHush)
	row := "L4"
	if announce {
		row = "L4b"
	}
	w.requireNothingOfAfterResubscribe(t, client, "e", ended.last(seen.last(since)), rlsHush, rlsOpen, row)
}

// L5: while bob is away, rlsHush and then rlsOpen are written; his resume from before both is
// replayed rlsOpen's write and nothing of rlsHush's.
func socketRowL5(t *testing.T) {
	w := newSocketWorld(t, socketWorldOptions{})
	head := eventHead(t, w.database)
	w.retitle(t, rlsHush, "L5: HUSH retitled in bob's gap")
	marker := w.retitle(t, rlsOpen, "L5: OPEN retitled in bob's gap")
	client := w.dial(t, rlsMember)
	subscribeEventsStart(t, client, "e", &head)
	read := feedUntil(t, client, "e", marker)
	requireMarker(t, "bob's replay", read)
	w.requireNoneOf(t, "bob's replay", read.events, rlsHush, rlsVeil)
}

// L6: liam, a limited member whose list names rlsVeil and rlsList, subscribes; the owner makes a
// public project and an issue in it. Nothing of that project reaches him (no list names him on
// it), and rlsList's next write does.
func socketRowL6(t *testing.T) {
	w := newSocketWorld(t, socketWorldOptions{})
	client := w.dial(t, rlsListed)
	subscribeEvents(t, client, "e", nil)
	w.asOK(t, rlsOwner, http.MethodPost, "/api/v1/projects", map[string]any{"key": rlsNewProject, "name": "A new public project"}, http.StatusCreated)
	w.asOK(t, rlsOwner, http.MethodPost, "/api/v1/issues", map[string]any{"project": rlsNewProject, "title": "The new project's issue"}, http.StatusCreated)
	read := feedUntil(t, client, "e", w.retitle(t, rlsList, "L6: LIST retitled for liam"))
	requireMarker(t, "liam's feed", read)
	w.requireNoneOf(t, "liam's feed", read.events, rlsNewProject, rlsOpen, rlsHush)
}

// L7: with no private project and no limited member, bob's feed on the server production builds
// sends, frame for frame, what the baseline server (main's behaviour, storetest.RequestAsOwner)
// sends, and relays every event with no read of the request store under his viewer: the events
// are written straight to the log, so the request store runs nothing else meanwhile, and the
// heartbeat's viewer read runs on the system store.
func socketRowL7(t *testing.T) {
	recorder := &storetest.StatementRecorder{}
	w := newSocketWorld(t, socketWorldOptions{public: true, requestTracer: recorder})
	baseline := newRLSServer(t, w.database, rlsServerConfig{asOwner: true, streamHeartbeat: socketHeartbeat, liveSocketClock: clocktest.Held()})
	baselineURL, _ := serveSockets(t, baseline)
	clients := []*socketClient{
		w.dial(t, rlsMember),
		openLiveSocketAt(t, gws.DefaultDialer, baselineURL+liveSocketPattern, asUser(rlsMember)).servedBy(w.database).read(),
	}
	for _, client := range clients {
		subscribeEvents(t, client, "e", nil)
	}
	before := viewerStatements(recorder)
	seedEvents(t, w.database, w.issue[rlsOpen], 3, 0)
	head := seedEvents(t, w.database, w.issue[rlsList], 3, 0)
	var frames [][]string
	for _, client := range clients {
		var sent []string
		for last := ""; last != strconv.FormatInt(head, 10); {
			frame, ok := eventFeedFrame(t, client, "the seeded events").(LiveEventFrame)
			if !ok {
				t.Fatalf("frame = %+v, want the seeded events", frame)
			}
			encoded, err := json.Marshal(frame)
			if err != nil {
				t.Fatalf("encode the frame: %v", err)
			}
			sent, last = append(sent, string(encoded)), frame.ID
		}
		frames = append(frames, sent)
	}
	if !slices.Equal(frames[0], frames[1]) {
		t.Errorf("bob's feed differs from main's:\n%v\nwant\n%v", frames[0], frames[1])
	}
	if len(frames[0]) != 6 {
		t.Errorf("bob was sent %d frames of the 6 events written, want every one", len(frames[0]))
	}
	if read := viewerStatements(recorder) - before; read != 0 {
		t.Errorf("relaying 6 events to bob read the request store under a viewer %d times, want none: every viewer sees every project", read)
	}
}

// L8: bob, who does not see every project, subscribes; the rule's read then fails, and an access
// change is announced. His socket's read of his sight fails, which closes the socket.
func socketRowL8(t *testing.T) {
	w := newSocketWorld(t, socketWorldOptions{})
	client := w.dial(t, rlsMember)
	subscribeEvents(t, client, "e", nil)
	w.breakTheRule(t)
	client.awaitEnd(t)
}

// L9: rlsMover, on rlsHush's list, watches a session that acted in rlsHush, and is sent its
// responder and replay. The session acts in rlsVeil, which he does not see, announced on
// events.SessionAccessChannel; the view ends before the session's next batch, which never reaches
// his socket.
func socketRowL9(t *testing.T) {
	w := newSocketWorld(t, socketWorldOptions{})
	w.listMover(t, rlsHush)
	session := uuid.NewString()
	w.sessionActsIn(t, session, rlsHush)
	w.stream.SetReplay(session, agentstream.Frame(socketReplay))
	client := w.dial(t, rlsMover)
	requireAgentView(t, client, "a", session)
	w.sessionActsIn(t, session, rlsVeil)
	w.stream.Publish(session, agentstream.Frame(socketFrame))
	requireEndedBeforeTheBatch(t, client, "a", "mo's view of a session that acted in VEIL")
}

// requireAgentView subscribes sub to session's conversation and fails t unless the view opens as
// one of a session the caller reads: the responder verdict, then the session's replay.
func requireAgentView(t *testing.T, client *socketClient, sub, session string) {
	t.Helper()
	client.subscribeAgent(t, sub, session)
	if data := client.agentFrame(t, sub, agentEventResponder); data != agentResponding {
		t.Fatalf("the view of %s opens with %s, want %s", session, data, agentResponding)
	}
	if data := client.agentFrame(t, sub, agentEventReplay); data != socketReplay {
		t.Fatalf("the view of %s replays %s, want %s", session, data, socketReplay)
	}
}

// requireEndedBeforeTheBatch fails t unless sub's next frame but a heartbeat is its end, before
// the batch the session published.
func requireEndedBeforeTheBatch(t *testing.T, client *socketClient, sub, what string) {
	t.Helper()
	switch frame := client.next(t, what).(type) {
	case LiveEnded:
		if frame.Sub != sub {
			t.Fatalf("%s: ended %+v, want %s's end", what, frame, sub)
		}
	case LiveAgentFrame:
		t.Fatalf("%s: the session's next batch reached the socket (%s), want the view ended before it", what, frame.Data)
	default:
		t.Fatalf("%s: frame = %+v, want the view's end", what, frame)
	}
}

// L10: lena, a limited member whom rlsHush's list admits through the group rlsGroup and whom the
// row names on rlsList's, subscribes and is sent rlsHush's write. The group's owner takes her out
// of the group (access_group.changed); the subscription ends (behind) before any later event, and
// nothing of rlsHush reaches her after her resubscribe.
func socketRowL10(t *testing.T) {
	w := newSocketWorld(t, socketWorldOptions{})
	listOnProject(t, w.database, rlsList, access.PrincipalPerson, rlsGrouped)
	client := w.dial(t, rlsGrouped)
	since := subscribeEvents(t, client, "e", nil)
	seen := feedUntil(t, client, "e", w.retitle(t, rlsHush, "L10: HUSH retitled while lena is in the group"))
	requireMarker(t, "lena's feed while she sees HUSH", seen)
	w.accessWrite(t, "take lena out of "+rlsGroup, func(ctx context.Context, tx pgx.Tx, broker *events.Broker, by model.Actor) error {
		_, _, err := access.PutGroup(ctx, tx, broker, rlsGroup, rlsOwner, nil, by)
		return err
	})
	w.retitle(t, rlsHush, "L10: HUSH retitled after lena left the group")
	after := feedUntil(t, client, "e", w.retitle(t, rlsList, "L10: LIST retitled after lena left the group"))
	requireEndedBehind(t, "lena's feed after she left the group", after)
	w.requireNoneOf(t, "lena's feed after she left the group", after.events, rlsHush)
	w.requireNothingOfAfterResubscribe(t, client, "e", after.last(seen.last(since)), rlsHush, rlsList, "L10")
}

// L11: the newest event of the log is rlsHush's, which bob does not see, when he subscribes: his
// subscription starts (events_start) and is sent nothing of it. His page then stops reading
// (heldWrites) while three commits land: rlsOpen's, rlsHush's and rlsOpen's again, more than the
// relay holds for one subscriber and fewer than the replay's bound. The relay drops him, and once
// his page reads again he is caught up on every event of rlsOpen's with nothing of rlsHush's and no
// end: neither the hidden newest event nor the hidden events in the overflow read as a reset of
// the log.
func socketRowL11(t *testing.T) {
	w := newSocketWorld(t, socketWorldOptions{})
	w.retitle(t, rlsHush, "L11: HUSH holds the newest event")
	client := w.dial(t, rlsMember)
	subscribeEvents(t, client, "e", nil)
	w.held.hold(t)
	const each = 80
	seedEvents(t, w.database, w.issue[rlsOpen], each, 0)
	seedEvents(t, w.database, w.issue[rlsHush], 20, 0)
	head := seedEvents(t, w.database, w.issue[rlsOpen], each, 0)
	if head > maxLiveReplay {
		t.Fatalf("the backlog reaches event %d, past the replay's bound of %d", head, maxLiveReplay)
	}
	horizontest.PollInProcess(t, "the relay drops bob's subscription, which fell behind", w.database, func() bool {
		return relaySubscribers(w.served.eventRelay) == 0
	})
	w.held.release()
	var open, hidden int
	for last := int64(0); last < head; {
		switch frame := eventFeedFrame(t, client, "bob's caught-up events").(type) {
		case LiveEventFrame:
			var event socketEvent
			if err := json.Unmarshal(frame.Event, &event); err != nil {
				t.Fatalf("decode event %s: %v", frame.ID, err)
			}
			if event.ID <= last {
				t.Fatalf("event %d after %d: the catch-up sent an event twice or out of order", event.ID, last)
			}
			last = event.ID
			switch {
			case w.of(event, rlsHush):
				hidden++
			case w.of(event, rlsOpen):
				open++
			}
		case LiveEnded:
			t.Fatalf("bob's subscription ended %q in the overflow, want it caught up: the hidden events read as a reset or a stall", frame.Reason)
		default:
			t.Fatalf("frame = %+v, want bob's caught-up events", frame)
		}
	}
	if hidden != 0 {
		t.Errorf("bob was sent %d events of HUSH, which he does not see, in the catch-up", hidden)
	}
	if want := 2 * each; open != want {
		t.Errorf("bob was caught up on %d of OPEN's %d events", open, want)
	}
}

// D1: bob dials the room of rlsHush's spec: the upgrade answers exactly what a room nobody holds
// answers.
func socketRowD1(t *testing.T) {
	w := newSocketWorld(t, socketWorldOptions{})
	status, body := w.roomRefusal(t, rlsMember, w.spec[rlsHush])
	wantStatus, wantBody := w.roomRefusal(t, rlsMember, uuid.NewString())
	if status != wantStatus || body != wantBody {
		t.Fatalf("bob's upgrade to HUSH's document answered %d %q, and a room nobody holds %d %q, want one answer", status, body, wantStatus, wantBody)
	}
}

// D2: bob dials the room of rlsOpen's spec and is admitted.
func socketRowD2(t *testing.T) {
	w := newSocketWorld(t, socketWorldOptions{})
	requireOpenRoom(t, "bob in OPEN's document", w.openRoom(t, rlsMember, w.spec[rlsOpen]))
}

// D3: rlsMover, on rlsHush's list, is admitted to the room of rlsHush's spec; the owner takes him
// off the list, which announces store.AccessChangedChannel. The room closes his socket with
// contracts.DocumentAdmissionRevokedCloseCode.
func socketRowD3(t *testing.T) {
	w := newSocketWorld(t, socketWorldOptions{})
	w.listMover(t, rlsHush)
	peer := w.openRoom(t, rlsMover, w.spec[rlsHush])
	w.unlistMover(t, rlsHush)
	if code := closedWith(t, "the room's close of mo's socket once he lost HUSH", peer); code != contracts.DocumentAdmissionRevokedCloseCode {
		t.Fatalf("mo's socket of HUSH's document closed with %d, want %d", code, contracts.DocumentAdmissionRevokedCloseCode)
	}
}

// D4: rlsMover, on rlsHush's list, is admitted to the room of rlsHush's spec; an access change
// that keeps his room (rlsVeil's list names the group rlsGroup) is announced. His socket stays
// open over the heartbeats that follow.
func socketRowD4(t *testing.T) {
	w := newSocketWorld(t, socketWorldOptions{})
	w.listMover(t, rlsHush)
	peer := w.openRoom(t, rlsMover, w.spec[rlsHush])
	w.accessWrite(t, "list "+rlsGroup+" on "+rlsVeil, func(ctx context.Context, tx pgx.Tx, broker *events.Broker, by model.Actor) error {
		_, _, err := access.AddPrincipal(ctx, tx, broker, rlsVeil, access.PrincipalGroup, rlsGroup, by)
		return err
	})
	time.Sleep(socketQuiet)
	requireOpenRoom(t, "mo in HUSH's document after a change that keeps it", peer)
}

// D5: rlsMover, on rlsHush's list, is admitted to the room of rlsHush's spec; his listing is
// deleted by SQL alone, announced nowhere. The heartbeat closes his socket with
// contracts.DocumentAdmissionRevokedCloseCode.
func socketRowD5(t *testing.T) {
	w := newSocketWorld(t, socketWorldOptions{})
	w.listMover(t, rlsHush)
	peer := w.openRoom(t, rlsMover, w.spec[rlsHush])
	w.unlistMoverBySQL(t, rlsHush, false)
	if code := closedWith(t, "the heartbeat's close of mo's socket once he lost HUSH", peer); code != contracts.DocumentAdmissionRevokedCloseCode {
		t.Fatalf("mo's socket of HUSH's document closed with %d, want %d", code, contracts.DocumentAdmissionRevokedCloseCode)
	}
}

// D6: rlsMover, on rlsHush's list, is admitted to the room of rlsHush's spec; the rule's read then
// fails, and an access change is announced. The recheck's read fails, which closes the socket.
func socketRowD6(t *testing.T) {
	w := newSocketWorld(t, socketWorldOptions{})
	w.listMover(t, rlsHush)
	peer := w.openRoom(t, rlsMover, w.spec[rlsHush])
	w.breakTheRule(t)
	closedWith(t, "the close of mo's socket once the recheck's read failed", peer)
}

// D7: with no private project and no limited member, bob is admitted to the room of rlsOpen's
// spec; access changes are announced and heartbeats pass. His socket stays open, and the request
// store reads nothing under a viewer meanwhile: no room is read again.
func socketRowD7(t *testing.T) {
	recorder := &storetest.StatementRecorder{}
	w := newSocketWorld(t, socketWorldOptions{public: true, requestTracer: recorder})
	peer := w.openRoom(t, rlsMember, w.spec[rlsOpen])
	before := viewerStatements(recorder)
	notifyChannel(t, w.database, store.AccessChangedChannel, "")
	time.Sleep(socketQuiet / 2)
	notifyChannel(t, w.database, store.AccessChangedChannel, "")
	time.Sleep(socketQuiet / 2)
	requireOpenRoom(t, "bob in OPEN's document", peer)
	if read := viewerStatements(recorder) - before; read != 0 {
		t.Errorf("the notifications and heartbeats read the request store under a viewer %d times, want no room read: every viewer sees every project", read)
	}
}

// A1: bob opens the agent view of a session that acted in rlsHush, which he does not see, and
// which answers as a streaming session does. The view answers what the view of a session nobody
// holds answers, the relay never tells the session a viewer is attached, and a frame it publishes
// never reaches his socket.
func socketRowA1(t *testing.T) {
	w := newSocketWorld(t, socketWorldOptions{})
	hidden, nobodys := uuid.NewString(), uuid.NewString()
	w.sessionActsIn(t, hidden, rlsHush)
	w.stream.SetReplay(hidden, agentstream.Frame(socketReplay))
	client := w.dial(t, rlsMember)
	client.subscribeAgent(t, "n", nobodys)
	want := client.agentFrame(t, "n", agentEventResponder)
	if want != agentNotResponding {
		t.Fatalf("the view of a session nobody holds opens with %s, want %s: the comparison proves nothing", want, agentNotResponding)
	}
	client.subscribeAgent(t, "a", hidden)
	if got := client.agentFrame(t, "a", agentEventResponder); got != want {
		t.Fatalf("bob's view of a session that acted in HUSH opens with %s, want %s, as a session nobody holds", got, want)
	}
	if watches := w.stream.Watches(hidden); watches != 0 {
		t.Errorf("the relay told the hidden session %d times that a viewer is attached, want never", watches)
	}
	w.stream.Publish(hidden, agentstream.Frame(socketFrame))
	client.quiet(t, socketQuiet, "bob's view of a session that acted in HUSH")
}

// A2: bob watches a session that acted in rlsOpen alone; the session acts in rlsVeil, which he
// does not see. The view ends before the session's next batch, which never reaches his socket.
func socketRowA2(t *testing.T) {
	w := newSocketWorld(t, socketWorldOptions{})
	session := uuid.NewString()
	w.sessionActsIn(t, session, rlsOpen)
	w.stream.SetReplay(session, agentstream.Frame(socketReplay))
	client := w.dial(t, rlsMember)
	requireAgentView(t, client, "a", session)
	w.sessionActsIn(t, session, rlsVeil)
	w.stream.Publish(session, agentstream.Frame(socketFrame))
	requireEndedBeforeTheBatch(t, client, "a", "bob's view of a session that acted in VEIL")
}

// A3: with no private project, bob watches a session that acted in rlsOpen: the responder, the
// replay and the next frame it publishes, as on main.
func socketRowA3(t *testing.T) {
	w := newSocketWorld(t, socketWorldOptions{public: true})
	session := uuid.NewString()
	w.sessionActsIn(t, session, rlsOpen)
	w.stream.SetReplay(session, agentstream.Frame(socketReplay))
	client := w.dial(t, rlsMember)
	requireAgentView(t, client, "a", session)
	w.stream.Publish(session, agentstream.Frame(socketFrame))
	if data := client.agentFrame(t, "a", agentEventFrame); data != socketFrame {
		t.Fatalf("bob's view sends %s, want the session's frame %s", data, socketFrame)
	}
}

// A4: rlsMover, on rlsHush's list, watches a session that acted in rlsHush; the owner takes him
// off the list, which announces store.AccessChangedChannel. The view ends (behind), and the next
// frame the session publishes never reaches his socket.
func socketRowA4(t *testing.T) {
	w := newSocketWorld(t, socketWorldOptions{})
	w.listMover(t, rlsHush)
	session := uuid.NewString()
	w.sessionActsIn(t, session, rlsHush)
	w.stream.SetReplay(session, agentstream.Frame(socketReplay))
	client := w.dial(t, rlsMover)
	requireAgentView(t, client, "a", session)
	w.unlistMover(t, rlsHush)
	if reason := client.endedAs(t, "a"); reason != liveEndedBehind {
		t.Fatalf("mo's view of a session of HUSH ended %q once he lost HUSH, want %q", reason, liveEndedBehind)
	}
	w.stream.Publish(session, agentstream.Frame(socketFrame))
	client.quiet(t, socketQuiet, "mo's ended view of a session of HUSH")
}

// V1: bob subscribes the Legion board of rlsHush and the Legion state of its issue: each answers
// exactly what the same view naming a project or an issue nobody holds answers.
func socketRowV1(t *testing.T) {
	w := newSocketWorld(t, socketWorldOptions{})
	client := w.dial(t, rlsMember)
	issueView := func(key string) DeliveryLiveView {
		return DeliveryLiveView{Kind: legionIssueView, Issue: json.RawMessage(strconv.Quote(key))}
	}
	for _, view := range []struct {
		what                         string
		hidden, missing              DeliveryLiveView
		hiddenName, missingName, sub string
	}{
		{"the Legion board", legionView(rlsHush), legionView(rlsMissingProject), rlsHush, rlsMissingProject, "board"},
		{"the Legion state of an issue", issueView(w.issue[rlsHush]), issueView(rlsMissingIssue), w.issue[rlsHush], rlsMissingIssue, "issue"},
	} {
		client.send(t, deliverySubscribe(view.sub+"-missing", view.missing))
		missing, ok := deliveryAnswer(t, client, view.sub+"-missing").(LiveError)
		if !ok {
			t.Fatalf("%s of %s was answered a snapshot, want a refusal: the comparison proves nothing", view.what, view.missingName)
		}
		client.send(t, deliverySubscribe(view.sub+"-hidden", view.hidden))
		hidden, ok := deliveryAnswer(t, client, view.sub+"-hidden").(LiveError)
		want := strings.ReplaceAll(missing.Message, view.missingName, view.hiddenName)
		if !ok || hidden.Code != missing.Code || hidden.Message != want {
			t.Errorf("bob's %s of %s answered %+v (refused: %t), want %s %q, as %s answers", view.what, view.hiddenName, hidden, ok, missing.Code, want, view.missingName)
		}
	}
}

// V2: alice, who sees rlsHush, and bob, who does not, subscribe the same timeline. Each is sent a
// body of its own: alice's holds both pull requests, bob's holds rlsOpen's alone, and no later
// frame bob is sent holds a pull request or an issue of rlsHush's, while rlsOpen's next one
// reaches him.
func socketRowV2(t *testing.T) {
	w := newSocketWorld(t, socketWorldOptions{})
	view := w.seedTimeline(t)
	alice, bob := w.dial(t, rlsOwner), w.dial(t, rlsMember)
	alice.send(t, deliverySubscribe("t", view))
	if got := prIDs(snapshotOf(t, alice, "t")); !slices.Equal(got, []string{"acme/widgets#1", "acme/widgets#2"}) {
		t.Fatalf("alice's timeline holds %v, want both pull requests: the row proves nothing", got)
	}
	bob.send(t, deliverySubscribe("t", view))
	snapshot := snapshotOf(t, bob, "t")
	if got := prIDs(snapshot); !slices.Equal(got, []string{"acme/widgets#1"}) {
		t.Errorf("bob's timeline holds %v, want OPEN's pull request alone", got)
	}
	w.requireNoHiddenDelivery(t, "bob's snapshot", snapshot)
	now := time.Now().UTC()
	w.upsertIssuePR(t, 3, rlsHush, now.Add(-time.Hour))
	w.upsertIssuePR(t, 4, rlsOpen, now.Add(-time.Hour))
	for reached := false; !reached; {
		switch frame := bob.next(t, "OPEN's next pull request in bob's timeline").(type) {
		case DeliveryLiveDelta:
			w.requireNoHiddenDelivery(t, "a delta of bob's timeline", frame)
			reached = slices.Contains(liveUpsertIDs(t, frame, "prs"), "acme/widgets#4")
		case DeliveryLiveFreshness:
		default:
			t.Fatalf("bob's timeline sent %+v, want its deltas", frame)
		}
	}
}

// V3: with no private project and no limited member, alice and bob subscribe the same timeline:
// the hub holds one view for both, and each is sent the GET route's body.
func socketRowV3(t *testing.T) {
	w := newSocketWorld(t, socketWorldOptions{public: true})
	view := w.seedTimeline(t)
	var bodies []json.RawMessage
	for _, login := range []string{rlsOwner, rlsMember} {
		client := w.dial(t, login)
		client.send(t, deliverySubscribe("t", view))
		bodies = append(bodies, snapshotOf(t, client, "t").Body)
	}
	if views := hubViewCount(w.served.deliveryHub); views != 1 {
		t.Errorf("the hub holds %d views of one timeline two subscribers name, want one shared view", views)
	}
	requireSameBody(t, "bob's snapshot beside alice's of one shared view", bodies[1], bodies[0])
	requireSameBody(t, "the shared snapshot", bodies[1], getBody(t, w.handler, windowTarget(t, deliveryLiveTimeline, bodies[1], "")))
}

// V4: rlsMover, on rlsHush's list, subscribes the timeline and is sent both pull requests; the
// owner takes him off the list, which announces store.AccessChangedChannel. The view ends
// (behind), no frame holding rlsHush's pull request or issue reaches him meanwhile, and his
// resubscribe's snapshot holds rlsOpen's alone.
func socketRowV4(t *testing.T) {
	socketRowViewLost(t, func(w *socketWorld) { w.unlistMover(t, rlsHush) })
}

// V5: as V4, with the listing deleted by SQL alone and announced nowhere: the heartbeat ends the
// view.
func socketRowV5(t *testing.T) {
	socketRowViewLost(t, func(w *socketWorld) { w.unlistMoverBySQL(t, rlsHush, false) })
}

// socketRowViewLost is V4 and V5: rlsMover loses rlsHush by change while his timeline is open.
func socketRowViewLost(t *testing.T, change func(w *socketWorld)) {
	w := newSocketWorld(t, socketWorldOptions{})
	w.listMover(t, rlsHush)
	view := w.seedTimeline(t)
	client := w.dial(t, rlsMover)
	client.send(t, deliverySubscribe("t", view))
	if got := prIDs(snapshotOf(t, client, "t")); !slices.Equal(got, []string{"acme/widgets#1", "acme/widgets#2"}) {
		t.Fatalf("mo's timeline holds %v while he sees HUSH, want both pull requests: the row proves nothing", got)
	}
	change(w)
	for ended := false; !ended; {
		switch frame := client.next(t, "the end of mo's timeline").(type) {
		case LiveEnded:
			if frame.Sub != "t" || frame.Reason != liveEndedBehind {
				t.Fatalf("ended %+v, want the timeline's end, %s", frame, liveEndedBehind)
			}
			ended = true
		case DeliveryLiveDelta:
			w.requireNoHiddenDelivery(t, "a delta of mo's timeline after he lost HUSH", frame)
		case DeliveryLiveFreshness:
		default:
			t.Fatalf("mo's timeline sent %+v, want its end", frame)
		}
	}
	client.send(t, deliverySubscribe("t", view))
	snapshot := snapshotOf(t, client, "t")
	if got := prIDs(snapshot); !slices.Equal(got, []string{"acme/widgets#1"}) {
		t.Errorf("mo's timeline after the resubscribe holds %v, want OPEN's pull request alone", got)
	}
	w.requireNoHiddenDelivery(t, "mo's snapshot after the resubscribe", snapshot)
}
