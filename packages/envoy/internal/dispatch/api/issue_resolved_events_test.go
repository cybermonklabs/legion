// issue_resolved_events_test.go holds Dispatch to J3 of dispatch://INFRA-5793 (check
// j-c-events). When a write to one issue changes the priority another issue resolves to
// (resolved_priority), or the issue that priority comes from (priority_from), in any project -
// a move or a detach of an issue above it, or a change or a clear of the own priority of an issue
// above it - Dispatch appends one issue.updated on that other issue, in the write's own
// transaction. Its changed names each of the two fields that moved and nothing else, its payload
// is the issue whole as the write left it, and its actor is the write's. Such an event is quiet:
// it wakes no session and reaches no route or claimant, while the event log and the event feed
// carry it. Every issue.* event whose payload is the issue carries both fields, and a write of an
// issue's own priority names in changed each of the two it moved. Every write of a priority or a
// parent takes one lock, so two such writes on one tree end with events their reads agree with.
//
// The rows read j-c-events' seed (seedResolvedEvents), each on a server and a seed of its own, and
// read the event log as the store holds it, as events_test.go does.
//
// The plan's decision 20 builds E1's conditional row, a reader who cannot see a project reading
// an event of it with no inherited priority, only where an issue read calls access.Resolve or
// access.VisibleProjectsSQL, INFRA-3862's read rule. No issue read calls either on this change's
// base, so this file holds no such row, as issue_resolved_priority_test.go holds none of R2's, R4's
// and R5's: whichever of this change and that read rule merges second adds them.
//
// The tests need DISPATCH_TEST_DATABASE_URL, as every test of this package that opens a store.
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/testwait"
)

// resolvedFields are the two fields J1 adds beside an issue's own priority, in the order an
// event's changed names them.
var resolvedFields = []string{"resolved_priority", "priority_from"}

// namesOnlyResolvedFields is whether changed names resolved_priority, priority_from or both, and
// nothing else: the changed of a J3 event, as the Legion daemon reads one (resolvedOnly in
// packages/daemon/internal/intake/decode.go). The written issue's own event names at least the
// field its write moved, so it never reads as one.
func namesOnlyResolvedFields(changed []string) bool {
	if len(changed) == 0 {
		return false
	}
	for _, field := range changed {
		if !slices.Contains(resolvedFields, field) {
			return false
		}
	}
	return true
}

// keysNamed is keys as a failure line prints them, each with its name.
func (n issueNames) keysNamed(keys []string) string {
	named := make([]string, len(keys))
	for index, key := range keys {
		named[index] = n.name(key)
	}
	return "[" + strings.Join(named, ", ") + "]"
}

// resolvedEventSeed is j-c-events' seed, by the names the plan's "The test rows" gives its
// issues: in one project, a root R at P2 with a child C (none), a grandchild G under C (none), a
// middle M1 (its own P1) with a child M1c (none), and a child O (none) of a second project under
// R; and a root B at P1 with a middle M0 (its own P0) with a child M0c (none).
type resolvedEventSeed struct {
	issueNames
	r, c, g, m1, m1c, o, b, m0, m0c string
}

// fileNamed files, as a person at a browser, who holds p0, an issue of project under parent (a
// root where parent is "") with priority as its own (none where it is nil), and names it name in
// names.
func fileNamed(t *testing.T, handler http.Handler, names issueNames, name, project, parent string, priority *int) string {
	t.Helper()
	body := map[string]any{"project": project, "title": "The seed's " + name, "force": true}
	if parent != "" {
		body["parent"] = parent
	}
	if priority != nil {
		body["priority"] = *priority
	}
	key := createdIssue(t, "file the seed's "+name, dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", body, "alice")).Key
	names[key] = name
	return key
}

// seedResolvedEvents makes project and other and files the event seed in them through handler:
// every issue in project but O, which is other's.
func seedResolvedEvents(t *testing.T, handler http.Handler, project, other string) resolvedEventSeed {
	t.Helper()
	createTestProject(t, handler, project, other)
	seed := resolvedEventSeed{issueNames: issueNames{}}
	file := func(name, in, parent string, priority *int) string {
		t.Helper()
		return fileNamed(t, handler, seed.issueNames, name, in, parent, priority)
	}
	seed.r = file("R", project, "", new(2))
	seed.c = file("C", project, seed.r, nil)
	seed.g = file("G", project, seed.c, nil)
	seed.m1 = file("M1", project, seed.r, new(1))
	seed.m1c = file("M1c", project, seed.m1, nil)
	seed.o = file("O", other, seed.r, nil)
	seed.b = file("B", project, "", new(1))
	seed.m0 = file("M0", project, seed.b, new(0))
	seed.m0c = file("M0c", project, seed.m0, nil)
	return seed
}

// resolvedEventPayload is what the rows read of the payload of an issue event: the issue as the
// write left it, what the write changed, and the issue's ancestors.
type resolvedEventPayload struct {
	Key              string   `json:"key"`
	Title            string   `json:"title"`
	Status           string   `json:"status"`
	Priority         *int     `json:"priority"`
	ResolvedPriority *int     `json:"resolved_priority"`
	PriorityFrom     *string  `json:"priority_from"`
	Changed          []string `json:"changed"`
	Ancestors        []string `json:"ancestors"`
}

// resolvedLogEvent is an issue.created, issue.updated or issue.closed as the event log stores it,
// with fields, each key its payload holds, so a field the payload leaves out reads apart from one
// it carries as null.
type resolvedLogEvent struct {
	id        int64
	key       string
	kind      string
	actor     model.Actor
	notify    bool
	createdAt time.Time
	payload   resolvedEventPayload
	fields    map[string]json.RawMessage
}

// resolvedLogEventsAfter is every issue.created, issue.updated and issue.closed event of the log
// above the event id after, oldest first, as the store holds them.
func resolvedLogEventsAfter(t *testing.T, database *store.Store, after int64) []resolvedLogEvent {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select id, issue_key, type, actor, notify, created_at, payload from events
		where id > $1 and type in ('issue.created', 'issue.updated', 'issue.closed')
		order by id
	`, after)
	if err != nil {
		t.Fatalf("read the issue events after %d: %v", after, err)
	}
	defer rows.Close()
	var logged []resolvedLogEvent
	for rows.Next() {
		var event resolvedLogEvent
		var actor, payload []byte
		if err := rows.Scan(&event.id, &event.key, &event.kind, &actor, &event.notify, &event.createdAt, &payload); err != nil {
			t.Fatalf("read an issue event: %v", err)
		}
		if err := json.Unmarshal(actor, &event.actor); err != nil {
			t.Fatalf("decode the actor of event %d: %v", event.id, err)
		}
		if err := json.Unmarshal(payload, &event.payload); err != nil {
			t.Fatalf("decode the payload of event %d: %v", event.id, err)
		}
		if err := json.Unmarshal(payload, &event.fields); err != nil {
			t.Fatalf("decode the payload of event %d: %v", event.id, err)
		}
		logged = append(logged, event)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the issue events after %d: %v", after, err)
	}
	return logged
}

// ancestorsOf is key's parent chain as the issue read answers it, parent first up to the root:
// what an event of key carries as its ancestors.
func ancestorsOf(t *testing.T, handler http.Handler, key string) []string {
	t.Helper()
	ancestors := []string{}
	for parent := readResolved(t, handler, key).Parent; parent != nil; parent = readResolved(t, handler, *parent).Parent {
		if len(ancestors) > parentDepthCap {
			t.Fatalf("%s's parent chain runs past %d links: %v", key, parentDepthCap, ancestors)
		}
		ancestors = append(ancestors, *parent)
	}
	return ancestors
}

// requireCarries fails unless event, an event of the issue want names and called what, carries
// that issue whole as its read answers it after the write: its key, title, status and own
// priority, the priority it resolves to and its source as want says, each of those two present in
// the payload, null included, and its parent chain as its ancestors.
func (n issueNames) requireCarries(t *testing.T, handler http.Handler, what string, event resolvedLogEvent, want resolvedWant) {
	t.Helper()
	read := readResolved(t, handler, want.key)
	payload := event.payload
	if payload.Key != want.key || payload.Title != read.Title || payload.Status != read.Status {
		t.Errorf("%s carries key %s, title %q and status %s, want %s's: title %q, status %s",
			what, payload.Key, payload.Title, payload.Status, n.name(want.key), read.Title, read.Status)
	}
	for _, field := range resolvedFields {
		if _, present := event.fields[field]; !present {
			t.Errorf("%s leaves out %s, which an issue event's issue always carries, null included", what, field)
		}
	}
	want.own = read.Priority
	n.requireResolvedAs(t, what, want, payload.Priority, payload.ResolvedPriority, payload.PriorityFrom)
	if ancestors := ancestorsOf(t, handler, want.key); !slices.Equal(payload.Ancestors, ancestors) {
		t.Errorf("%s carries ancestors %s, want %s's parent chain, %s", what, n.keysNamed(payload.Ancestors), n.name(want.key), n.keysNamed(ancestors))
	}
}

// writtenWant is what the own event of an issue a write writes says of the priority it resolves
// to: that priority after the write and the key of the issue it comes from ("" for none), and
// moved, which of resolvedFields the write moved, which its changed names and no other of them.
type writtenWant struct {
	key, from string
	resolved  *int
	moved     []string
}

// resolvedEventWant is the J3 event a write owes an issue other than the ones it writes: the
// issue, the priority it resolves to after the write and the key of the issue that one comes from
// ("" for none), what its changed names, and under, the written issue whose own event it shares
// its transaction and its actor with.
type resolvedEventWant struct {
	key, from, under string
	resolved         *int
	changed          []string
}

// requireResolvedEvents fails unless the issue events appended above the event id head are what
// quiet and written say. The issues other than the written ones that gained an issue.updated whose
// changed names only resolved fields (namesOnlyResolvedFields) are exactly quiet's, each with one
// such event: its changed is want's, it carries the issue whole with want's resolved priority, and
// its actor and created_at are those of the own event of the written issue it is under, as one
// transaction appends both. Each written issue's own event, its newest issue.updated or
// issue.closed whose changed names more than the resolved fields, carries the issue whole with the
// priority it now resolves to, and names in changed each of resolvedFields the write moved and
// neither it kept.
func (n issueNames) requireResolvedEvents(t *testing.T, handler http.Handler, database *store.Store, head int64, written []writtenWant, quiet []resolvedEventWant) {
	t.Helper()
	writes := func(key string) bool {
		return slices.ContainsFunc(written, func(want writtenWant) bool { return want.key == key })
	}
	own := map[string]resolvedLogEvent{}
	gained := map[string][]resolvedLogEvent{}
	for _, event := range resolvedLogEventsAfter(t, database, head) {
		switch {
		case writes(event.key) && event.kind != "issue.created" && !namesOnlyResolvedFields(event.payload.Changed):
			own[event.key] = event
		case !writes(event.key) && event.kind == "issue.updated" && namesOnlyResolvedFields(event.payload.Changed):
			gained[event.key] = append(gained[event.key], event)
		}
	}
	for _, want := range quiet {
		events := gained[want.key]
		delete(gained, want.key)
		if len(events) != 1 {
			t.Errorf("%s gained %d issue.updated naming only resolved_priority and priority_from, want one: %s, changed %v",
				n.name(want.key), len(events), n.reads(resolvedWant{key: want.key, resolved: want.resolved, from: want.from}), want.changed)
			if len(events) == 0 {
				continue
			}
		}
		event := events[0]
		what := fmt.Sprintf("%s's issue.updated of the write", n.name(want.key))
		if !slices.Equal(event.payload.Changed, want.changed) {
			t.Errorf("%s names changed %v, want %v", what, event.payload.Changed, want.changed)
		}
		n.requireCarries(t, handler, what, event, resolvedWant{key: want.key, resolved: want.resolved, from: want.from})
		source, appended := own[want.under]
		if !appended {
			continue
		}
		if !reflect.DeepEqual(event.actor, source.actor) {
			t.Errorf("%s was written by %+v, want the write's actor, %+v, which %s's own event names", what, event.actor, source.actor, n.name(want.under))
		}
		if !event.createdAt.Equal(source.createdAt) {
			t.Errorf("%s was appended at %s and %s's own event at %s: want both in the write's one transaction",
				what, event.createdAt.Format(time.RFC3339Nano), n.name(want.under), source.createdAt.Format(time.RFC3339Nano))
		}
	}
	for _, key := range slices.Sorted(maps.Keys(gained)) {
		for _, event := range gained[key] {
			t.Errorf("%s gained an issue.updated naming only %v (resolved_priority %s from %s), want none: the write moved neither field of it",
				n.name(key), event.payload.Changed, priorityName(event.payload.ResolvedPriority), n.keyName(deref(event.payload.PriorityFrom)))
		}
	}
	for _, want := range written {
		event, appended := own[want.key]
		if !appended {
			t.Errorf("the write appended no issue.updated or issue.closed on %s, which it wrote", n.name(want.key))
			continue
		}
		what := fmt.Sprintf("%s's own %s", n.name(want.key), event.kind)
		n.requireCarries(t, handler, what, event, resolvedWant{key: want.key, resolved: want.resolved, from: want.from})
		for _, field := range resolvedFields {
			switch moved, named := slices.Contains(want.moved, field), slices.Contains(event.payload.Changed, field); {
			case moved && !named:
				t.Errorf("%s names changed %v, and the write moved its %s: want it named", what, event.payload.Changed, field)
			case named && !moved:
				t.Errorf("%s names changed %v, and the write left its %s as it was: want it not named", what, event.payload.Changed, field)
			}
		}
	}
}

// TestAWriteAppendsAQuietEventOnEachIssueWhoseResolvedPriorityItChanged (E1): on the event seed,
// with an issue Y (none) whose child Yc has none beside it, a person at a browser writes in turn:
// R's priority from 2 to 0; an own priority of 0 on C, then a clear of it; a move of Y under R; a
// clear of M0's own priority; a PATCH that sets no parent on Y; and B's status change to done with
// unlink_children. After each, the issues other than the written ones that gained an issue.updated
// naming only resolved_priority and priority_from are exactly, with its changed: C, G and O, 0 from
// R, [resolved_priority]; G, 0 from C, [priority_from]; G, 0 from R, [priority_from]; and, naming
// both fields, Yc, 0 from R; M0c, 1 from B; Yc, none; M0c, none. M1 and M1c gain none. A
// rule that appends an event only where resolved_priority moves fails the two rows of G; a detach
// that skips the walk under the issue it writes fails the last two. Each such event has the
// actor and the created_at of the written issue's own event, and carries the issue whole, and the
// written issue's own event carries both fields and names each that moved.
func TestAWriteAppendsAQuietEventOnEachIssueWhoseResolvedPriorityItChanged(t *testing.T) {
	t.Parallel()
	handler, database := newTestHandlerWithStore(t)
	seed := seedResolvedEvents(t, handler, "ACME", "OTHER")
	y := fileNamed(t, handler, seed.issueNames, "Y", "ACME", "", nil)
	yc := fileNamed(t, handler, seed.issueNames, "Yc", "ACME", y, nil)
	patch := func(key string, body map[string]any) func(*testing.T) *httptest.ResponseRecorder {
		return func(t *testing.T) *httptest.ResponseRecorder {
			t.Helper()
			return dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+key, body, "alice")
		}
	}
	resolvedOnly, fromOnly := []string{"resolved_priority"}, []string{"priority_from"}
	for _, step := range []struct {
		name    string
		send    func(*testing.T) *httptest.ResponseRecorder
		written []writtenWant
		quiet   []resolvedEventWant
	}{
		{
			name:    "R's priority from 2 to 0",
			send:    patch(seed.r, map[string]any{"priority": 0}),
			written: []writtenWant{{key: seed.r, resolved: new(0), moved: resolvedOnly}},
			quiet: []resolvedEventWant{
				{key: seed.c, resolved: new(0), from: seed.r, changed: resolvedOnly, under: seed.r},
				{key: seed.g, resolved: new(0), from: seed.r, changed: resolvedOnly, under: seed.r},
				{key: seed.o, resolved: new(0), from: seed.r, changed: resolvedOnly, under: seed.r},
			},
		},
		{
			name:    "an own priority of 0 set on C",
			send:    patch(seed.c, map[string]any{"priority": 0}),
			written: []writtenWant{{key: seed.c, resolved: new(0), moved: fromOnly}},
			quiet:   []resolvedEventWant{{key: seed.g, resolved: new(0), from: seed.c, changed: fromOnly, under: seed.c}},
		},
		{
			name:    "a clear of C's own priority",
			send:    patch(seed.c, map[string]any{"priority": nil}),
			written: []writtenWant{{key: seed.c, resolved: new(0), from: seed.r, moved: fromOnly}},
			quiet:   []resolvedEventWant{{key: seed.g, resolved: new(0), from: seed.r, changed: fromOnly, under: seed.c}},
		},
		{
			name:    "a move of Y, whose child Yc has none, under R",
			send:    patch(y, map[string]any{"parent": seed.r}),
			written: []writtenWant{{key: y, resolved: new(0), from: seed.r, moved: resolvedFields}},
			quiet:   []resolvedEventWant{{key: yc, resolved: new(0), from: seed.r, changed: resolvedFields, under: y}},
		},
		{
			name:    "a clear of M0's own priority",
			send:    patch(seed.m0, map[string]any{"priority": nil}),
			written: []writtenWant{{key: seed.m0, resolved: new(1), from: seed.b, moved: resolvedFields}},
			quiet:   []resolvedEventWant{{key: seed.m0c, resolved: new(1), from: seed.b, changed: resolvedFields, under: seed.m0}},
		},
		{
			name:    "a PATCH that sets no parent on Y",
			send:    patch(y, map[string]any{"parent": nil}),
			written: []writtenWant{{key: y, moved: resolvedFields}},
			quiet:   []resolvedEventWant{{key: yc, changed: resolvedFields, under: y}},
		},
		{
			name: "B's status change to done with unlink_children",
			send: func(t *testing.T) *httptest.ResponseRecorder {
				t.Helper()
				return dispatchRequest(t, handler, http.MethodPost, legionStatusChangePath(seed.b), map[string]any{
					"status": "done", "unlink_children": true,
				}, "alice")
			},
			written: []writtenWant{{key: seed.b, resolved: new(1)}, {key: seed.m0, moved: resolvedFields}},
			quiet:   []resolvedEventWant{{key: seed.m0c, changed: resolvedFields, under: seed.m0}},
		},
	} {
		t.Run(step.name, func(t *testing.T) {
			head := eventHead(t, database)
			requireTaken(t, step.name, step.send(t), http.StatusOK)
			seed.requireResolvedEvents(t, handler, database, head, step.written, step.quiet)
		})
	}
}

// quietOf is the quiet each envelope the outbox handed p for the event id said, by the topic it
// was handed on.
func (p *recordingOutboxPublisher) quietOf(t *testing.T, id int64) map[string]bool {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	quiet := map[string]bool{}
	for _, item := range p.items {
		var envelope struct {
			ID    int64 `json:"id"`
			Quiet bool  `json:"quiet"`
		}
		if err := json.Unmarshal([]byte(item.Payload), &envelope); err != nil {
			t.Fatalf("decode the envelope handed on %s: %v", item.Topic, err)
		}
		if envelope.ID == id {
			quiet[item.Topic] = envelope.Quiet
		}
	}
	return quiet
}

// publishedDestinations is every topic the outbox published the event id to.
func publishedDestinations(t *testing.T, database *store.Store, id int64) []string {
	t.Helper()
	var topics []string
	if err := database.Pool.QueryRow(context.Background(), `select published_destinations from events where id = $1`, id).Scan(&topics); err != nil {
		t.Fatalf("read the destinations of event %d: %v", id, err)
	}
	return topics
}

// issueLogHolds is whether GET /api/v1/issues/{key}/events, the issue's event log as a person
// reads it, carries the event id.
func issueLogHolds(t *testing.T, handler http.Handler, key string, id int64) bool {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, fmt.Sprintf("/api/v1/issues/%s/events?ids=%d", key, id), nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/issues/%s/events?ids=%d: status=%d body=%s", key, id, response.Code, response.Body.String())
	}
	type logged struct {
		ID int64 `json:"id"`
	}
	return slices.ContainsFunc(decodeBody[[]logged](t, response), func(event logged) bool { return event.ID == id })
}

// TestAResolvedPriorityEventWakesNoSession (E2): C's route names a session and another session
// claims C. Of the events of a person's write of R's priority from 2 to 0, each issue.updated it
// appends on C, G and O, naming only the resolved fields, is stored with notify false although a
// person wrote it; the outbox publishes it to neither the session that holds C's route nor C's
// claimant, and hands every envelope of it with quiet true; and C's, G's and O's event logs and
// the event feed carry it. A Quiet that answers false for such an event fails each of those but
// the logs. R's own issue.updated is not quiet.
func TestAResolvedPriorityEventWakesNoSession(t *testing.T) {
	t.Parallel()
	rig := newLiveSocketRig(t, testServerOptions{}, nil)
	seed := seedResolvedEvents(t, rig.handler, "ACME", "OTHER")
	const routed, claimant = "ses-route-of-c", "ses-claimant-of-c"
	requireTaken(t, "route C to a session", dispatchRequest(t, rig.handler, http.MethodPatch, "/api/v1/issues/"+seed.c, map[string]any{
		"route": "session:" + routed,
	}, "alice"), http.StatusOK)
	requireTaken(t, "a session's claim of C", sessionRequest(t, rig.handler, http.MethodPost, "/api/v1/issues/"+seed.c+"/claim", map[string]any{
		"actor": claimActorBody(claimant, "Implementer"),
	}), http.StatusOK)
	head := eventHead(t, rig.database)
	client := rig.dial(t, asUser("alice"))
	subscribeEvents(t, client, "feed", &head)
	requireTaken(t, "a person's write of R's priority to 0", dispatchRequest(t, rig.handler, http.MethodPatch, "/api/v1/issues/"+seed.r, map[string]any{
		"priority": 0,
	}, "alice"), http.StatusOK)

	var own *resolvedLogEvent
	quiet := map[string]resolvedLogEvent{}
	for _, event := range resolvedLogEventsAfter(t, rig.database, head) {
		switch {
		case event.key == seed.r:
			own = &event
		case event.kind == "issue.updated" && namesOnlyResolvedFields(event.payload.Changed):
			quiet[event.key] = event
		}
	}
	if own == nil {
		t.Fatalf("a person's write of R's priority to 0 appended no event on %s", seed.name(seed.r))
	}

	published := &recordingOutboxPublisher{}
	runOutboxBeside(t, rig.served.deps, published)
	testwait.UntilPolling(t, 10*time.Millisecond, "the outbox to publish every event of the write (the count still unpublished)", func() (int64, bool) {
		unpublished := countRows(t, rig.database, `select count(*) from events where id > $1 and published_at is null`, head)
		return unpublished, unpublished == 0
	})
	ownQuiet := published.quietOf(t, own.id)
	if len(ownQuiet) == 0 {
		t.Errorf("the outbox handed on no envelope of %s's own %s", seed.name(seed.r), own.kind)
	}
	for topic, isQuiet := range ownQuiet {
		if isQuiet {
			t.Errorf("%s's own %s, a person's write of its own priority, was handed on %s with quiet true, want false", seed.name(seed.r), own.kind, topic)
		}
	}

	// The feed sends the events above head in id order, so it has carried every event of the
	// write once it has sent the last of them.
	carried := map[int64]bool{}
	var last int64
	for _, event := range quiet {
		last = max(last, event.id)
	}
	if last > 0 {
		for range maxLiveReplay {
			_, event := nextEvent(t, client, "feed")
			carried[event.ID] = true
			if event.ID >= last {
				break
			}
		}
	}

	for _, key := range []string{seed.c, seed.g, seed.o} {
		event, appended := quiet[key]
		if !appended {
			t.Errorf("a person's write of R's priority to 0 appended no issue.updated naming only resolved_priority and priority_from on %s, which it moved from 2 to 0", seed.name(key))
			continue
		}
		what := fmt.Sprintf("%s's issue.updated of R's write", seed.name(key))
		if event.notify {
			t.Errorf("%s is stored with notify true: a person wrote it, and it wakes nobody all the same", what)
		}
		destinations := publishedDestinations(t, rig.database, event.id)
		for _, session := range []string{routed, claimant} {
			if slices.Contains(destinations, contracts.LegacyAgentSubject(session)) {
				t.Errorf("%s was published to %s's own topic (%v), the session that holds C's route or its claim, want neither", what, session, destinations)
			}
		}
		envelopes := published.quietOf(t, event.id)
		if len(envelopes) == 0 {
			t.Errorf("the outbox handed on no envelope of %s", what)
		}
		for topic, isQuiet := range envelopes {
			if !isQuiet {
				t.Errorf("%s was handed on %s with quiet false, want quiet true", what, topic)
			}
		}
		if !issueLogHolds(t, rig.handler, key, event.id) {
			t.Errorf("GET /api/v1/issues/%s/events does not carry %s (event %d)", key, what, event.id)
		}
		if !carried[event.id] {
			t.Errorf("the event feed does not carry %s (event %d)", what, event.id)
		}
	}
}

// issueEventsWithTheIssue are the issue.* event types whose payload is the issue itself
// (model.IssueEventPayload, which embeds model.Issue), and so carries resolved_priority and
// priority_from beside the issue's own priority (the plan's decision 13).
var issueEventsWithTheIssue = []string{"issue.closed", "issue.created", "issue.updated"}

// issueEventsWithNoIssue are the issue.* event types whose payload is no issue, each with its
// reason. The list is closed: TestEveryIssueEventThatCarriesTheIssueCarriesTheResolvedPriority
// fails by name for an issue.* type of the contracts' event list on neither list.
var issueEventsWithNoIssue = map[string]string{
	"issue.claimed":                          "its payload is the claim now in force and the one it replaced (model.IssueClaimEventPayload), with the issue's key, status and repository and no priority",
	"issue.released":                         "its payload is the claim it ended (model.IssueClaimEventPayload), with the issue's key, status and repository and no priority",
	"issue.deployed":                         "its payload is the deploy run that carried the issue's pull request to production (delivery.IssueDeployedEventPayload)",
	"issue.pull_request_attribution_changed": "its payload is the pull request whose issue changed, with the issue it left and the one it reached (delivery.IssuePullRequestAttributionEventPayload)",
	"issue.role_holders_changed":             "its payload is the sessions the Legion daemon gave the issue's roles, and the ones before them (issueRoleHoldersChange)",
}

// TestEveryIssueEventThatCarriesTheIssueCarriesTheResolvedPriority (E3) is the inventory of the
// plan's decision 13. It takes every issue.* type of the contracts' event list
// (contracts.DispatchEventTypes) and holds each to one side. issue.created, issue.updated and
// issue.closed, each made by a person's write of an issue with no priority of its own under a
// parent at P1 and read from the issue's event log, carry resolved_priority 1 and priority_from
// the parent, so a payload that leaves either out, or carries it null, fails. The types of the
// closed list issueEventsWithNoIssue carry no issue, each for its reason. A type on neither side
// fails by name, and so does a type of either list that the contracts' list no longer holds.
func TestEveryIssueEventThatCarriesTheIssueCarriesTheResolvedPriority(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	createTestProject(t, handler, "ACME")
	names := issueNames{}
	parent := fileNamed(t, handler, names, "the parent at P1", "ACME", "", new(1))
	child := fileNamed(t, handler, names, "its child, with no priority of its own", "ACME", parent, nil)
	requireTaken(t, "a person's retitle of the child", dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+child, map[string]any{
		"title": "The child, retitled",
	}, "alice"), http.StatusOK)
	requireTaken(t, "a person's close of the child", dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+child, map[string]any{
		"status": "done",
	}, "alice"), http.StatusOK)
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+child+"/events", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/issues/%s/events: status=%d body=%s", child, response.Code, response.Body.String())
	}
	// The newest event of each type in the child's log.
	logged := map[string]json.RawMessage{}
	for _, event := range decodeBody[[]struct {
		Type    string          `json:"type"`
		Payload json.RawMessage `json:"payload"`
	}](t, response) {
		logged[event.Type] = event.Payload
	}

	for _, eventType := range contracts.DispatchEventTypes {
		if !strings.HasPrefix(eventType, "issue.") {
			continue
		}
		if reason, listed := issueEventsWithNoIssue[eventType]; listed {
			t.Logf("%s carries no issue: %s", eventType, reason)
			continue
		}
		if !slices.Contains(issueEventsWithTheIssue, eventType) {
			t.Errorf("%s is an issue event of the contracts' event list that this test holds to neither side: add it to issueEventsWithTheIssue, with a write that appends it, where its payload is the issue, or to issueEventsWithNoIssue with its reason", eventType)
			continue
		}
		raw, appended := logged[eventType]
		if !appended {
			t.Errorf("GET /api/v1/issues/%s/events holds no %s, which the writes of %s append", child, eventType, names.name(child))
			continue
		}
		var carried resolvedEventPayload
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &carried); err != nil {
			t.Fatalf("decode the %s of %s: %v", eventType, names.name(child), err)
		}
		if err := json.Unmarshal(raw, &fields); err != nil {
			t.Fatalf("decode the %s of %s: %v", eventType, names.name(child), err)
		}
		for _, field := range resolvedFields {
			if _, present := fields[field]; !present {
				t.Errorf("the %s of %s leaves out %s, which an issue event's issue always carries, null included", eventType, names.name(child), field)
			}
		}
		names.requireResolvedAs(t, "the "+eventType+" in the event log", resolvedWant{key: child, resolved: new(1), from: parent},
			carried.Priority, carried.ResolvedPriority, carried.PriorityFrom)
	}
	for _, eventType := range slices.Concat(issueEventsWithTheIssue, slices.Sorted(maps.Keys(issueEventsWithNoIssue))) {
		if !slices.Contains(contracts.DispatchEventTypes, eventType) {
			t.Errorf("%s is on a list of this test and not in the contracts' event list: take it off", eventType)
		}
	}
}

// TestAnOwnPriorityWriteNamesTheResolvedFieldsThatMoved (E4): on the event seed, a person's write
// of C's own priority from none, which inherits 2 from R, to 1 names changed [priority
// resolved_priority priority_from]; a write of M1's own priority from 1 to 3 names [priority
// resolved_priority], and not priority_from, which stays none; and a retitle of C names [title],
// neither resolved field.
func TestAnOwnPriorityWriteNamesTheResolvedFieldsThatMoved(t *testing.T) {
	t.Parallel()
	handler, database := newTestHandlerWithStore(t)
	seed := seedResolvedEvents(t, handler, "ACME", "OTHER")
	for _, write := range []struct {
		name string
		key  string
		body map[string]any
		want []string
	}{
		{"C's own priority from none, inheriting 2 from R, to 1", seed.c, map[string]any{"priority": 1}, []string{"priority", "resolved_priority", "priority_from"}},
		{"M1's own priority from 1 to 3", seed.m1, map[string]any{"priority": 3}, []string{"priority", "resolved_priority"}},
		{"a retitle of C", seed.c, map[string]any{"title": "C, retitled"}, []string{"title"}},
	} {
		requireTaken(t, write.name, dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+write.key, write.body, "alice"), http.StatusOK)
		if changed := newestEventPayload[issueEventFields](t, database, "issue.updated", write.key).Changed; !slices.Equal(changed, write.want) {
			t.Errorf("%s: %s's issue.updated names changed %v, want %v", write.name, seed.name(write.key), changed, write.want)
		}
	}
}

// TestAWriteOverALargeSubtreeAppendsOneEventEachInOneTransaction (E5): a root at P2 with 2,600
// issues under it, made by direct SQL in one statement, none with a priority of its own. A
// person's write of the root's priority to 0 answers 200, and each of the 2,600 issues has
// exactly one new issue.updated, stored with notify false, naming changed [resolved_priority] and
// carrying resolved_priority 0 from the root, each with the created_at of the root's own event.
// No time bound is asserted (docs/testing.md); the write's time is logged for the pull request.
func TestAWriteOverALargeSubtreeAppendsOneEventEachInOneTransaction(t *testing.T) {
	t.Parallel()
	const descendants = 2600
	ctx := context.Background()
	handler, database := newTestHandlerWithStore(t)
	createTestProject(t, handler, "WIDE")
	root := createdIssue(t, "file the root at P2", dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "WIDE", "title": "A root at P2 over a large tree", "priority": 2, "force": true,
	}, "alice"))
	if want := fmt.Sprintf("WIDE-%d", root.Number); root.Key != want {
		t.Fatalf("the root's key is %s, want %s: the statement below names each issue by its number", root.Key, want)
	}
	// The tree is a binary heap under the root: the issue at place i, the root's being 1, is
	// numbered root.Number+i-1 and stands under the one at place i/2, so the 2,600 issues stand at
	// most 11 links under the root, inside the walk's cap.
	if _, err := database.Pool.Exec(ctx, `
		insert into issues (key, project_key, number, title, parent_key, created_by, rank)
		select 'WIDE-' || ($1::int + i - 1), 'WIDE', $1::int + i - 1, 'Under the root ' || i,
		       'WIDE-' || ($1::int + i / 2 - 1), '{"kind":"user","id":"alice"}', lpad(($1::int + i - 1)::text, 8, '0')
		from generate_series(2, $2::int + 1) i
		order by i
	`, root.Number, descendants); err != nil {
		t.Fatalf("insert %d issues under %s: %v", descendants, root.Key, err)
	}

	head := eventHead(t, database)
	began := time.Now()
	written := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+root.Key, map[string]any{"priority": 0}, "alice")
	took := time.Since(began)
	requireTaken(t, "a person's write of the root's priority to 0", written, http.StatusOK)
	t.Logf("a person's write of %s's priority from 2 to 0, over the %d issues under it, answered in %s", root.Key, descendants, took)

	var rootEvent time.Time
	if err := database.Pool.QueryRow(ctx, `
		select created_at from events where id > $1 and issue_key = $2 and type = 'issue.updated' order by id desc limit 1
	`, head, root.Key).Scan(&rootEvent); err != nil {
		t.Fatalf("read the created_at of %s's own issue.updated: %v", root.Key, err)
	}
	rows, err := database.Pool.Query(ctx, `
		select i.key, count(e.id),
		       count(e.id) filter (where not e.notify
		           and e.payload -> 'changed' = '["resolved_priority"]'::jsonb
		           and e.payload -> 'resolved_priority' = '0'::jsonb
		           and e.payload ->> 'priority_from' = $2
		           and e.created_at = $3)
		from issues i
		left join events e on e.issue_key = i.key and e.id > $1 and e.type = 'issue.updated'
		where i.project_key = 'WIDE' and i.key <> $2
		group by i.key, i.number
		order by i.number
	`, head, root.Key, rootEvent)
	if err != nil {
		t.Fatalf("read the events of the issues under %s: %v", root.Key, err)
	}
	defer rows.Close()
	var read int
	var missing, several, wrong []string
	for rows.Next() {
		var key string
		var events, quiet int
		if err := rows.Scan(&key, &events, &quiet); err != nil {
			t.Fatalf("read the events of an issue under %s: %v", root.Key, err)
		}
		read++
		switch {
		case events == 0:
			missing = append(missing, key)
		case events > 1:
			several = append(several, key)
		case quiet == 0:
			wrong = append(wrong, key)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the events of the issues under %s: %v", root.Key, err)
	}
	if read != descendants {
		t.Fatalf("project WIDE holds %d issues under %s, want the %d the statement made", read, root.Key, descendants)
	}
	first := func(keys []string) []string { return keys[:min(len(keys), 5)] }
	if len(missing) > 0 {
		t.Errorf("%d of the %d issues under %s gained no issue.updated from the write, %v first among them: each read 2 from %s and now reads 0",
			len(missing), descendants, root.Key, first(missing), root.Key)
	}
	if len(several) > 0 {
		t.Errorf("%d of the %d issues under %s gained more than one issue.updated from the one write, %v first among them",
			len(several), descendants, root.Key, first(several))
	}
	if len(wrong) > 0 {
		t.Errorf("%d of the %d issues under %s gained an issue.updated that is not the write's quiet event, %v first among them: want notify false, changed [resolved_priority], resolved_priority 0 from %s, and created_at %s, that of %s's own event",
			len(wrong), descendants, root.Key, first(wrong), root.Key, rootEvent.Format(time.RFC3339Nano), root.Key)
	}
}

// secondWrite is what TestTwoPriorityWritesOnOneTreeEndWithRightEvents last read of its second
// write while the first is held: the write's answer, once it has one, or whether a backend of the
// test's own database waits on a lock it has not been granted.
type secondWrite struct {
	answer  *httptest.ResponseRecorder
	waiting bool
}

func (s secondWrite) String() string {
	if s.answer != nil {
		return fmt.Sprintf("answered %d", s.answer.Code)
	}
	return fmt.Sprintf("no answer, a lock waiter: %t", s.waiting)
}

// lockWaitInOwnDatabase is whether a backend of database waits on a lock it has not been granted.
// The predicate names the test's own database, since the tests beside it take the same lock keys
// in databases cloned from the same template (docs/testing.md).
func lockWaitInOwnDatabase(t *testing.T, database *store.Store) bool {
	t.Helper()
	var waiting bool
	if err := database.Pool.QueryRow(context.Background(), `
		select exists (
			select 1 from pg_locks
			where not granted and database = (select oid from pg_database where datname = current_database())
		)
	`).Scan(&waiting); err != nil {
		t.Fatalf("read pg_locks: %v", err)
	}
	return waiting
}

// TestTwoPriorityWritesOnOneTreeEndWithRightEvents (E6): a middle M, at its own P2, under a root
// R at P1, with a child Mc (none). A person's clear of M's own priority is held once it has read M
// under M's owner lock (server.afterIssueLoad), and a person's write of R's priority to 0 is sent
// beside it: it must not answer until the clear is released, since every write of a priority or a
// parent takes one lock (the plan's decision 9). The wait for it ends on the first of its answer,
// which fails the row, or a lock of the test's own database it waits on, which releases the clear;
// no clock bounds it. Once both answer, each of R, M and Mc reads as the two writes in that order
// leave it, R 0 its own and M and Mc 0 from R, and its newest event says the priority its read
// answers. Two writes that each read the tree as it was before the other leave Mc's newest event
// at 1 from R.
func TestTwoPriorityWritesOnOneTreeEndWithRightEvents(t *testing.T) {
	t.Parallel()
	release, let := heldUntilReleased(t)
	var holding atomic.Pointer[string]
	reached := make(chan struct{})
	reach := sync.OnceFunc(func() { close(reached) })
	handler, database, _, _ := newRegisteredTestServer(t, testServerOptions{
		afterIssueLoad: func(ctx context.Context, key string) {
			held := holding.Load()
			if held == nil || *held != key || !holding.CompareAndSwap(held, nil) {
				return
			}
			reach()
			select {
			case <-release:
			case <-ctx.Done():
			}
		},
	})
	createTestProject(t, handler, "ACME")
	names := issueNames{}
	r := fileNamed(t, handler, names, "R", "ACME", "", new(1))
	m := fileNamed(t, handler, names, "M", "ACME", r, new(2))
	mc := fileNamed(t, handler, names, "Mc", "ACME", m, nil)
	// Both writes run on goroutines of their own. The cleanup lets the held one go and waits for
	// both, before the server's cleanups close its database under them.
	var running sync.WaitGroup
	t.Cleanup(func() {
		let()
		running.Wait()
	})
	holding.Store(&m)
	cleared := make(chan *httptest.ResponseRecorder, 1)
	running.Go(func() {
		cleared <- dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+m, map[string]any{"priority": nil}, "alice")
	})
	testwait.Receive(t, "the clear of M's own priority to reach its hold, once it has read M under M's owner lock", reached)
	raised := make(chan *httptest.ResponseRecorder, 1)
	running.Go(func() {
		raised <- dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+r, map[string]any{"priority": 0}, "alice")
	})
	first := testwait.UntilPolling(t, 5*time.Millisecond, "the write of R's priority to 0 to wait on a lock of the test's database, or to answer (what was last read)", func() (secondWrite, bool) {
		select {
		case answer := <-raised:
			return secondWrite{answer: answer}, true
		default:
		}
		waiting := lockWaitInOwnDatabase(t, database)
		return secondWrite{waiting: waiting}, waiting
	})
	if first.answer != nil {
		t.Errorf("the write of R's priority to 0 answered %d while the clear of M's own priority was held under M's owner lock: it did not wait for the clear, so each write read the tree as it stood before the other",
			first.answer.Code)
	}
	let()
	requireTaken(t, "the clear of M's own priority", testwait.Receive(t, "the clear of M's own priority to answer once released", cleared), http.StatusOK)
	second := first.answer
	if second == nil {
		second = testwait.Receive(t, "the write of R's priority to 0 to answer once the clear is released", raised)
	}
	requireTaken(t, "the write of R's priority to 0", second, http.StatusOK)

	newest := map[string]resolvedLogEvent{}
	for _, event := range resolvedLogEventsAfter(t, database, 0) {
		newest[event.key] = event
	}
	for _, want := range []resolvedWant{
		{key: r, own: new(0), resolved: new(0)},
		{key: m, resolved: new(0), from: r},
		{key: mc, resolved: new(0), from: r},
	} {
		read := readResolved(t, handler, want.key)
		names.requireResolvedAs(t, "GET /api/v1/issues/{key} after both writes", want, read.Priority, read.ResolvedPriority, read.PriorityFrom)
		event := newest[want.key]
		if priorityName(event.payload.ResolvedPriority) != priorityName(read.ResolvedPriority) || deref(event.payload.PriorityFrom) != deref(read.PriorityFrom) {
			t.Errorf("%s's newest event, its %s, says resolved_priority %s from %s, and its read answers %s from %s: a reader that follows its events holds a priority the tree no longer gives it",
				names.name(want.key), event.kind, priorityName(event.payload.ResolvedPriority), names.keyName(deref(event.payload.PriorityFrom)),
				priorityName(read.ResolvedPriority), names.keyName(deref(read.PriorityFrom)))
		}
	}
}
