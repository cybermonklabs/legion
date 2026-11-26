package api

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/legion"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/dispatch/workflowstore"
	"github.com/sjawhar/envoy/internal/testwait"
)

// The live view of one issue's Legion state (dispatch://INFRA-4869's R2, a-c-live): a page that
// subscribes the legion_issue kind is answered GET /api/v1/issues/{key}/legion's body, and then a
// delta for each store that moves it, at the hub of the process that stored it and at a second
// process's hub on the same database. The frames are read as the hub sends them to a socket
// (fakeLiveConn), each through the shared wait: the product promises a delta within 5 seconds, and
// a loaded machine's delay is no verdict of these rows. Every body a page holds after a delta is
// the GET body of that moment (requireLandsOnGET). A view of an issue of a project that names no
// daemon is never refused: its body says so (configured false) and still moves with the issue's
// runs and with the setting.

// legionIssueSubscription is a subscribe frame's view of one issue's Legion state.
func legionIssueSubscription(key string) DeliveryLiveView {
	return DeliveryLiveView{Kind: issueLegionKind, Issue: issueMember(key)}
}

// legionIssueSub is the sub every page of these tests holds its issue's view under.
const legionIssueSub = "i"

// listenerConnected is the note a hub makes once its listener holds its LISTEN.
func listenerConnected(note deliveryHubNote) bool { return note.what == "connected" }

// legionIssuePage is one page's view of an issue at one hub: the hub it subscribed at (named A or
// B in failures), the socket it reads, the key the hub holds its view under, which the hub's notes
// name (deliveryHubNote.view), and the body it holds.
type legionIssuePage struct {
	hub  string
	rig  *hubRig
	conn *fakeLiveConn
	key  string
	view string
	held json.RawMessage
}

// openLegionIssuePage subscribes a page to key's view at rig, and requires its snapshot to be
// GET /api/v1/issues/{key}/legion's body (L-1, L-15) and to name key alone (L-14). The view's key
// is the parse's of the frame the page subscribes with, so unread matches the notes of exactly
// the view the hub holds for it.
func openLegionIssuePage(t *testing.T, hub string, rig *hubRig, key string) *legionIssuePage {
	t.Helper()
	view := legionIssueSubscription(key)
	spec, refusal := parseDeliveryLiveView(view)
	if refusal != nil {
		t.Fatalf("the legion_issue view of %s: refusal = %+v, want none", key, refusal)
	}
	page := &legionIssuePage{hub: hub, rig: rig, conn: newFakeLiveConn(), key: key, view: spec.key}
	rig.hub.Subscribe(page.conn, subscribeFrame(legionIssueSub, view))
	snapshot := page.conn.snapshot(t, legionIssueSub)
	requireLegionNames(t, key, decodeLegionIssue(t, snapshot.Body))
	requireSameBody(t, "hub "+hub+"'s snapshot of "+key, snapshot.Body,
		getBody(t, rig.handler, "/api/v1/issues/"+key+"/legion"))
	page.held = snapshot.Body
	return page
}

// answer is the body the page holds, decoded.
func (p *legionIssuePage) answer(t *testing.T) wireLegionIssue {
	t.Helper()
	return decodeLegionIssue(t, p.held)
}

// what names the page in a failure: its issue and its hub.
func (p *legionIssuePage) what() string { return p.key + " at hub " + p.hub }

// next reads the page's next frame, which must be a delta; requires it to set exactly the members
// set names and nothing of another issue (L-14), and the body it leaves to be the GET body of the
// moment; and answers that body.
func (p *legionIssuePage) next(t *testing.T, set ...string) wireLegionIssue {
	t.Helper()
	delta := p.conn.delta(t, legionIssueSub)
	raw, err := json.Marshal(delta)
	if err != nil {
		t.Fatalf("encode %s's delta: %v", p.what(), err)
	}
	if len(delta.Items) != 0 || len(delta.Maps) != 0 || len(delta.Counts) != 0 ||
		len(delta.Revs.Items) != 0 || len(delta.Revs.Maps) != 0 || len(delta.Revs.Counts) != 0 {
		t.Fatalf("L-14: %s's delta carries a list, a map or a count, which the issue's body has none of: %s", p.what(), raw)
	}
	for _, members := range [][]string{slices.Collect(maps.Keys(delta.Set)), slices.Collect(maps.Keys(delta.Revs.Fields))} {
		for _, member := range members {
			if !slices.Contains(wireLegionIssueMembers, member) {
				t.Fatalf("L-14: %s's delta names %q, which is no member of the issue's body: %s", p.what(), member, raw)
			}
		}
	}
	if got, want := slices.Sorted(maps.Keys(delta.Set)), slices.Sorted(slices.Values(set)); !slices.Equal(got, want) {
		t.Fatalf("%s's delta sets %v, want %v: %s", p.what(), got, want, raw)
	}
	p.held = requireLandsOnGET(t, p.rig.handler, issueLegionKind, p.held, delta, "")
	answer := p.answer(t)
	requireLegionNames(t, p.key, answer)
	return answer
}

// step reads the page's delta of one store, which sets exactly set, and requires the body it
// leaves to hold body; it answers that body.
func (p *legionIssuePage) step(t *testing.T, body legionIssueBody, set ...string) wireLegionIssue {
	t.Helper()
	answer := p.next(t, set...)
	requireLegionBody(t, p.what(), answer, body)
	return answer
}

// runsRise reads the page's delta of a run's write, which sets the runs' revision alone, and
// requires the revision to rise and the rest of the body to hold body.
func (p *legionIssuePage) runsRise(t *testing.T, body legionIssueBody, what string) {
	t.Helper()
	before := p.answer(t).RunsRevision
	if after := p.step(t, body, "runs_revision").RunsRevision; after <= before {
		t.Fatalf("%s: %s's runs revision went from %d to %d, want it to rise", what, p.what(), before, after)
	}
}

// mark settles the page's hub, so nothing a store before it left owed is still to run, and
// answers how many notes the hub has made, which unread counts from.
func (p *legionIssuePage) mark(t *testing.T) int {
	t.Helper()
	p.rig.settle(t)
	return p.rig.mark()
}

// unread requires that row's store, made after start, the page's mark, left the page's hub owing
// its view nothing. The hub routes the store's notification, on channel with payload, on its
// listener's goroutine, and marks every view that notification moves before it notes it routed
// (a "notified" note, onNotify), so the row waits for that note and then reads the hub's own state:
// a hub that took the notification for the view has by then marked it dirty or begun the
// recompute the mark made due, and a recompute begun since start, ended or not, left a "recompute
// begin" note. A store that moves nothing the view shows sends the page nothing even where the hub
// marks the view for it, since the recompute finds no change; so the page's silence alone could
// not tell a hub that filtered the store's notification from one that did not, and the hub's state
// and its notes can.
func (p *legionIssuePage) unread(t *testing.T, start int, row, channel, payload string) {
	t.Helper()
	p.rig.waitFor(t, start, fmt.Sprintf("%s: hub %s to route the notification on %s of %q", row, p.hub, channel, payload), func(note deliveryHubNote) bool {
		return note.what == "notified" && note.channel == channel && note.payload == payload
	})
	if owed := p.rig.owesView(p.view); owed != "" {
		t.Fatalf("%s: once hub %s routed the notification on %s of %q, it owes %s's view (%s) %s, want nothing", row, p.hub, channel, payload, p.key, p.view, owed)
	}
	p.requireNoRecomputeSince(t, start, row)
}

// requireNoRecomputeSince requires that the page's hub began no recompute of the page's view since
// start, the page's mark.
func (p *legionIssuePage) requireNoRecomputeSince(t *testing.T, start int, row string) {
	t.Helper()
	if n := p.rig.count(start, func(note deliveryHubNote) bool {
		return note.what == "recompute begin" && note.view == p.view
	}); n != 0 {
		t.Fatalf("%s: hub %s began %d recomputes of %s's view (%s) since its mark, want 0", row, p.hub, n, p.key, p.view)
	}
}

// owesView names what the hub will do of its own accord to the view whose key is key (viewOwes):
// empty when nothing, a recompute under way, or a mark that makes one due.
func (r *hubRig) owesView(key string) string {
	now := r.clock.Now()
	r.hub.mu.Lock()
	defer r.hub.mu.Unlock()
	if r.hub.generation == nil {
		return "no generation running"
	}
	view, held := r.hub.generation.views[key]
	if !held {
		return "no view held under its key"
	}
	return viewOwes(view, now, r.clock.standsStill())
}

// announce sends the notification one read of project's daemon sends (legion.StateChannel), as the
// sampler does after it records the read.
func announce(t *testing.T, rig *hubRig, project string) {
	t.Helper()
	notifyChannel(t, rig.database, legion.StateChannel, project)
}

// TestAnIssuesLegionLiveView is the plan's live rows, one after another on one view of LIVE-1 and
// one of BARE-1, each at hub A and at hub B: L-1 to L-12 on an issue of a project with a daemon,
// L-15 to L-19 on an issue of a project whose daemon was removed and is named again and removed
// again, and L-13, the listener of hub A cut and back, last. L-14 holds every frame of the test
// (next, openLegionIssuePage).
func TestAnIssuesLegionLiveView(t *testing.T) {
	t.Parallel()
	// Hub A's listener's own connections, so L-13 ends the one it listens on by its backend's pid
	// (terminateListener), and a switch that keeps it from dialing again until the test says.
	var blind atomic.Bool
	var dials listenerDials
	rig := newHubRig(t, func(hub *deliveryHub) {
		dial := hub.dial
		hub.dial = dials.wrap(func(ctx context.Context) (*pgx.Conn, error) {
			if blind.Load() {
				return nil, context.DeadlineExceeded
			}
			return dial(ctx)
		})
		hub.backoff = func(int) time.Duration { return 50 * time.Millisecond }
	})
	// Hub B is a second Dispatch process on the same database: a pool, a server and a hub of its
	// own, which hears what hub A's process writes only through Postgres.
	handlerB, databaseB, depsB := newTestServer(t, testServerOptions{store: storetest.SecondProcess(t, rig.database)})
	rigB := hubOn(t, handlerB, databaseB, depsB, newNoonHubClock(), nil)

	seedLegionProjects(t, rig.handler, "LIVE", "OTHER", "BARE")
	for _, project := range []string{"LIVE", "OTHER", "BARE"} {
		nameLegionDaemon(t, rig.handler, project, "http://legion-"+strings.ToLower(project)+".internal:13370")
	}
	for number, title := range map[int]string{
		1: "The issue the page shows", 2: "Another issue of its project",
		3: "The first root in the line", 4: "The second root in the line",
	} {
		seedLegionIssue(t, rig.database, "LIVE", number, title, string(rune('a'+number)), nil, "")
	}
	seedLegionIssue(t, rig.database, "OTHER", 1, "An issue of another project", "a", nil, "")
	seedLegionIssue(t, rig.database, "BARE", 1, "An issue of a project that names no daemon", "a", nil, "")
	at := time.Date(2026, 11, 23, 9, 0, 0, 0, time.UTC)
	tick := func() time.Time {
		at = at.Add(time.Minute)
		return at
	}
	observe := func(project, key, field, value string, when time.Time) {
		t.Helper()
		seedLegionObservation(t, rig.database, project, key, field, value, when)
	}
	claim := legion.ClaimFieldPrefix
	started := tick()
	observe("LIVE", "LIVE-1", legion.FieldPhase, `"planning"`, started)
	observe("LIVE", "LIVE-1", claim+"architect", `"working"`, started)
	observe("LIVE", "LIVE-1", claim+"tester", `"idle"`, started)
	for _, key := range []string{"LIVE-2", "LIVE-3", "LIVE-4"} {
		observe("LIVE", key, legion.FieldPhase, `"admitted"`, started)
	}
	observe("LIVE", legion.AdmissionKey, legion.FieldAdmission,
		`{"cap": 2, "active": [], "waiting": ["LIVE-3", "LIVE-4", "LIVE-1"], "blocked": []}`, started)
	observe("OTHER", "OTHER-1", legion.FieldPhase, `"planning"`, started)
	// BARE's daemon recorded BARE-1 and was removed, which leaves its rows.
	observe("BARE", "BARE-1", legion.FieldPhase, `"implementing"`, started)
	observe("BARE", "BARE-1", claim+"implementer", `"working"`, started)
	observe("BARE", legion.AdmissionKey, legion.FieldAdmission,
		`{"cap": 1, "active": ["BARE-1"], "waiting": [], "blocked": []}`, started)
	firstRead := tick()
	for _, project := range []string{"LIVE", "OTHER", "BARE"} {
		seedLegionRead(t, rig.database, project, firstRead, "")
	}
	if removed := dispatchRequest(t, rig.handler, http.MethodDelete, "/api/v1/projects/BARE/legion/daemon", nil, "alice"); removed.Code != http.StatusNoContent {
		t.Fatalf("remove BARE's daemon: status=%d body=%s, want 204", removed.Code, removed.Body.String())
	}

	// L-1: the snapshot of LIVE-1 at each hub is the GET body, and holds what was seeded.
	state := legionIssueWant{
		phase: "planning", since: started,
		roles: []wireLegionRole{{"architect", "working"}, {"tester", "idle"}},
		line:  &wireLegionWaitingLine{Place: 3, Length: 3}, blockedBy: []string{},
	}
	body := legionIssueBody{configured: true, state: &state, read: &wireLegionRead{At: firstRead, OK: true}}
	pages := []*legionIssuePage{openLegionIssuePage(t, "A", rig, "LIVE-1"), openLegionIssuePage(t, "B", rigB, "LIVE-1")}
	for _, page := range pages {
		requireLegionBody(t, page.what(), page.answer(t), body)
	}
	// No store below comes before each hub's listener holds its LISTEN, so none is sent unheard,
	// and none before each hub ran the recompute that connection and its event feed mark every
	// Legion view for, so no store of two rows is read half made by that recompute.
	rig.waitFor(t, 0, "hub A's listener's first connection", listenerConnected)
	rigB.waitFor(t, 0, "hub B's listener's first connection", listenerConnected)
	rig.settle(t)
	rigB.settle(t)

	// L-2: a new phase, stored with a good read, and its notify.
	state.phase, state.since, state.seen = "implementing", tick(), true
	observe("LIVE", "LIVE-1", legion.FieldPhase, `"implementing"`, state.since)
	body.read = &wireLegionRead{At: tick(), OK: true}
	seedLegionRead(t, rig.database, "LIVE", body.read.At, "")
	announce(t, rig, "LIVE")
	for _, page := range pages {
		page.step(t, body, "read", "state")
	}

	// L-3: a role's claim moves.
	state.roles = []wireLegionRole{{"architect", "working"}, {"tester", "working"}}
	observe("LIVE", "LIVE-1", claim+"tester", `"working"`, tick())
	announce(t, rig, "LIVE")
	for _, page := range pages {
		page.step(t, body, "state")
	}

	// L-4: the issue takes a slot.
	state.slot = new(1)
	observe("LIVE", "LIVE-1", legion.FieldSlot, `1`, tick())
	announce(t, rig, "LIVE")
	for _, page := range pages {
		page.step(t, body, "state")
	}

	// L-5: an admission row that moves only this issue, from third to second.
	state.line = &wireLegionWaitingLine{Place: 2, Length: 3}
	observe("LIVE", legion.AdmissionKey, legion.FieldAdmission,
		`{"cap": 2, "active": [], "waiting": ["LIVE-3", "LIVE-1", "LIVE-4"], "blocked": []}`, tick())
	announce(t, rig, "LIVE")
	for _, page := range pages {
		page.step(t, body, "state")
	}

	// L-6: a failed read.
	failure := "the Legion daemon could not be read: context deadline exceeded"
	body.read = &wireLegionRead{At: tick(), OK: false, Error: &failure}
	seedLegionRead(t, rig.database, "LIVE", body.read.At, failure)
	announce(t, rig, "LIVE")
	for _, page := range pages {
		page.step(t, body, "read")
	}

	// L-7: a good read that moves nothing else.
	body.read = &wireLegionRead{At: tick(), OK: true}
	seedLegionRead(t, rig.database, "LIVE", body.read.At, "")
	announce(t, rig, "LIVE")
	for _, page := range pages {
		page.step(t, body, "read")
	}

	// L-8: a run's step write through hub A's process.
	putStepOK(t, rig.handler, "LIVE-1", uuid.NewString(), implementerStart(daemonSessionOf("LIVE")))
	for _, page := range pages {
		page.runsRise(t, body, "L-8, a step write that makes a run")
	}

	// L-9: a rating write alone, which makes a retro's record.
	putRatingOK(t, rig.handler, "LIVE-1", uuid.NewString(), withFields(retroIdentity(daemonSessionOf("LIVE"), 1),
		map[string]any{"rating": 4, "rating_reason": "The plan fit the work."}))
	for _, page := range pages {
		page.runsRise(t, body, "L-9, a rating write that makes a retro's record")
	}

	// L-10: a stored change of another project, and its notify. Each hub settles first; then, once
	// each hub has routed the notify, it owes LIVE-1's view nothing and began no recompute of it
	// (unread), which the page's silence alone does not show.
	marks := map[*legionIssuePage]int{}
	for _, page := range pages {
		marks[page] = page.mark(t)
	}
	observe("OTHER", "OTHER-1", legion.FieldPhase, `"testing"`, tick())
	seedLegionRead(t, rig.database, "OTHER", tick(), "")
	announce(t, rig, "OTHER")
	for _, page := range pages {
		page.unread(t, marks[page], "L-10, a notify of another project", legion.StateChannel, "OTHER")
	}

	// L-11: a run of another issue of the same project, read as L-10 is once each hub has routed
	// the run's notify (workflowstore.RunsChannel).
	for _, page := range pages {
		marks[page] = page.mark(t)
	}
	putStepOK(t, rig.handler, "LIVE-2", uuid.NewString(), implementerStart(daemonSessionOf("LIVE")))
	for _, page := range pages {
		page.unread(t, marks[page], "L-11, a run of another issue", workflowstore.RunsChannel, "LIVE-2")
	}

	// L-12: two run writes of the issue that overlap. Run A's write is made the way the route
	// makes it (lockRecordedIssue's shared lock, then workflowstore.PutStep) and held open; run B
	// is written and committed through the route meanwhile; then A commits. Each commit brings a
	// new revision.
	ctx := t.Context()
	held, err := rig.database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("L-12: begin run A's write: %v", err)
	}
	defer func() { _ = held.Rollback(context.Background()) }()
	if err := store.LockOwnersShared(ctx, held, store.IssueOwner("LIVE-1")); err != nil {
		t.Fatalf("L-12: lock LIVE-1 for run A's write: %v", err)
	}
	if _, err := workflowstore.PutStep(ctx, held, workflowstore.StepWrite{
		ID: uuid.NewString(), Issue: "LIVE-1", Project: new("LIVE"), Task: new("default"), Place: new(3),
		Kind: new(workflowstore.StepKindStep), Role: new("tester"), Type: new("testing"),
		Agent: new("legion-live-live-1-tester"), Generation: new(int64(1)), StartedAt: new(stepStart),
	}, model.Actor{Kind: "session", ID: legionDaemonSessionPrefix + "LIVE"}, time.Now()); err != nil {
		t.Fatalf("L-12: write run A: %v", err)
	}
	putStepOK(t, rig.handler, "LIVE-1", uuid.NewString(), implementerStart(daemonSessionOf("LIVE")))
	for _, page := range pages {
		page.runsRise(t, body, "L-12, run B's write, committed while run A's is open")
	}
	if err := held.Commit(ctx); err != nil {
		t.Fatalf("L-12: commit run A's write: %v", err)
	}
	for _, page := range pages {
		page.runsRise(t, body, "L-12, run A's write, which began first and committed last")
	}

	// L-15: the view of an issue of a project that names no daemon, which an earlier daemon left
	// rows of, is that GET body, and no refusal.
	bare := legionIssueBody{configured: false}
	barePages := []*legionIssuePage{openLegionIssuePage(t, "A", rig, "BARE-1"), openLegionIssuePage(t, "B", rigB, "BARE-1")}
	for _, page := range barePages {
		answer := page.answer(t)
		requireLegionBody(t, page.what(), answer, bare)
		if answer.RunsRevision != 0 {
			t.Fatalf("L-15: %s's runs revision is %d, want 0: no run of it was written", page.what(), answer.RunsRevision)
		}
	}

	// L-16: a run of it.
	putStepOK(t, rig.handler, "BARE-1", uuid.NewString(), implementerStart(daemonSessionOf("BARE")))
	for _, page := range barePages {
		page.runsRise(t, bare, "L-16, a run of an issue of a project that names no daemon")
	}

	// L-17: its project names a daemon again (PUT .../legion/daemon, which announces itself), and
	// the rows the earlier daemon left are its state and its read.
	nameLegionDaemon(t, rig.handler, "BARE", "http://legion-bare.internal:13370")
	// The PUT announces itself: its notify is what the pages' deltas below answer.
	named := legionIssueBody{configured: true, state: &legionIssueWant{
		phase: "implementing", since: started, roles: []wireLegionRole{{"implementer", "working"}}, blockedBy: []string{},
	}, read: &wireLegionRead{At: firstRead, OK: true}}
	for _, page := range barePages {
		page.step(t, named, "configured", "read", "state")
	}

	// L-18: the daemon is removed again (DELETE .../legion/daemon, which announces itself): the
	// view says so and keeps its runs' revision, and is not refused.
	revisions := map[*legionIssuePage]int64{}
	for _, page := range barePages {
		revisions[page] = page.answer(t).RunsRevision
	}
	if removed := dispatchRequest(t, rig.handler, http.MethodDelete, "/api/v1/projects/BARE/legion/daemon", nil, "alice"); removed.Code != http.StatusNoContent {
		t.Fatalf("L-18: remove BARE's daemon: status=%d body=%s, want 204", removed.Code, removed.Body.String())
	}
	for _, page := range barePages {
		if after := page.step(t, bare, "configured", "read", "state").RunsRevision; after != revisions[page] {
			t.Fatalf("L-18: %s's runs revision went from %d to %d as its daemon was removed, want it kept", page.what(), revisions[page], after)
		}
	}

	// L-19: another run, with no daemon again.
	putStepOK(t, rig.handler, "BARE-1", uuid.NewString(), implementerStart(daemonSessionOf("BARE")))
	for _, page := range barePages {
		page.runsRise(t, bare, "L-19, a run once the daemon was removed again")
	}

	// L-13: hub A's listener loses its connection and cannot dial again; a phase is stored with
	// no notify while it is down, once its backend is gone (terminateListener); the listener comes
	// back, and the view sends the phase it missed. The hub settles first, so no recompute still
	// owed reads the phase as though the listener had heard it.
	rig.settle(t)
	start := rig.mark()
	blind.Store(true)
	terminateListener(t, rig.database, &dials)
	state.phase, state.since = "testing", tick()
	observe("LIVE", "LIVE-1", legion.FieldPhase, `"testing"`, state.since)
	// No notification of the phase is sent, and terminateListener has returned with no dial of the
	// listener's in flight, so the hub's listener can hear nothing until the reconnection below, and
	// nothing the hub holds says whether it hears the phase some other way. A hub that marked the
	// view by the phase's store would begin its recompute within viewMaxWait of that mark (dueAt):
	// the span is exactly viewMaxWait from the store, and the recompute count after it catches a
	// recompute begun in it whose delta is not sent yet. The reconnection is what makes the hub
	// hear the phase, so the later action whose effect would prove the absence is the one that ends
	// it.
	testwait.Quiet(t, "the frames of "+pages[0].what()+" while hub A's listener is down", pages[0].conn.frames, viewMaxWait,
		"L-13: no state of the hub tells a phase it cannot hear from one it hears some other way, and the only later "+
			"action whose effect would, the listener's reconnection, is what makes it hear the phase; the span is "+
			"viewMaxWait from the store, the latest a hub that marked the view by then would begin its recompute")
	pages[0].requireNoRecomputeSince(t, start, "L-13, while hub A's listener is down")
	blind.Store(false)
	rig.waitFor(t, start, "L-13, hub A's listener's reconnection", listenerConnected)
	pages[0].step(t, body, "state")
}

// TestAnIssuesLegionLiveViewOfAKeyDispatchDoesNotHoldIsRefusedAsTheRouteRefusesIt: a view of a key
// Dispatch holds no issue of is told what GET /api/v1/issues/{key}/legion answers it, NOT_FOUND in
// its words, once, and nothing after.
func TestAnIssuesLegionLiveViewOfAKeyDispatchDoesNotHoldIsRefusedAsTheRouteRefusesIt(t *testing.T) {
	t.Parallel()
	rig := newHubRig(t, nil)
	seedLegionProjects(t, rig.handler, "LIVE")
	nameLegionDaemon(t, rig.handler, "LIVE", "http://legion-live.internal:13370")
	conn := newFakeLiveConn()
	rig.hub.Subscribe(conn, subscribeFrame(legionIssueSub, legionIssueSubscription("LIVE-999")))
	refusal := conn.refusal(t, legionIssueSub, "NOT_FOUND")
	rig.requireNothingSent(t, conn, "the refusal of a key Dispatch does not hold")
	routed := dispatchRequest(t, rig.handler, http.MethodGet, "/api/v1/issues/LIVE-999/legion", nil, "alice")
	var answer refusalAnswer
	if err := json.Unmarshal(routed.Body.Bytes(), &answer); err != nil || routed.Code != http.StatusNotFound || answer.Code != "NOT_FOUND" {
		t.Fatalf("GET /api/v1/issues/LIVE-999/legion: status=%d body=%s, want 404 NOT_FOUND", routed.Code, routed.Body.String())
	}
	if refusal.Message != answer.Error {
		t.Errorf("the refusal's message = %q, want the route's %q", refusal.Message, answer.Error)
	}
}

// TestAnIssuesLegionLiveViewOfAnIssueTheDaemonDoesNotRecordAnswersNullThenItsState: a view of an
// issue of a project with a daemon, which the daemon does not record yet, is answered a snapshot
// that is configured with a null state, and a delta with the state once a phase row and its notify
// land.
func TestAnIssuesLegionLiveViewOfAnIssueTheDaemonDoesNotRecordAnswersNullThenItsState(t *testing.T) {
	t.Parallel()
	rig := newHubRig(t, nil)
	seedLegionProjects(t, rig.handler, "LIVE")
	nameLegionDaemon(t, rig.handler, "LIVE", "http://legion-live.internal:13370")
	key := seedLegionIssue(t, rig.database, "LIVE", 5, "An issue the daemon has not recorded", "a", nil, "")
	read := &wireLegionRead{At: time.Date(2026, 11, 23, 9, 0, 0, 0, time.UTC), OK: true}
	seedLegionRead(t, rig.database, "LIVE", read.At, "")

	page := openLegionIssuePage(t, "A", rig, key)
	requireLegionBody(t, page.what(), page.answer(t), legionIssueBody{configured: true, read: read})
	rig.waitFor(t, 0, "the listener's first connection", listenerConnected)

	phased := read.At.Add(time.Minute)
	seedLegionObservation(t, rig.database, "LIVE", key, legion.FieldPhase, `"planning"`, phased)
	announce(t, rig, "LIVE")
	page.step(t, legionIssueBody{configured: true, read: read, state: &legionIssueWant{
		phase: "planning", since: phased, roles: []wireLegionRole{}, blockedBy: []string{},
	}}, "state")
}
