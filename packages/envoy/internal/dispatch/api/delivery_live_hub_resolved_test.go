// delivery_live_hub_resolved_test.go holds the delivery views and their live hub to J1 and J3 of
// dispatch://INFRA-5793 (check j-c-delivery). An issue with no priority of its own works at the
// priority of the nearest issue above it that has one, so the views' unowned P0 list and the hub's
// set of the open P0 issues, which marks every view that shows that list when one of them changes,
// hold such an issue when that priority is 0. A write that moves an issue's resolved priority
// appends a quiet issue.updated on it, which wakes nobody and which the hub reads as it reads every
// issue event; and each view recompute reads the set afresh, so a priority written with no event
// shows at the next fresh read.
//
// The tests need DISPATCH_TEST_DATABASE_URL, as every test of this package that opens a store.
package api

import (
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// hubDeliverySeed is j-c-delivery's seed in project HUB, each issue filed through the API by
// alice, whose browser sign-in holds p0: R2, a root at P2, with an open child C1 that has no
// priority of its own; R0, a root at P0, with a middle M2 at its own P2 whose open child M2c has
// none; and R1, a root at P1, with a middle M0 at its own P0 whose open child M0c has none. So C1
// takes P2 from R2, M2c P2 from M2 and M0c P0 from M0, where a rule of the root alone would read
// M2c as a P0 and M0c as a P1. Each child is named by a pull request merged in the last hours,
// which every timeline and measures view of the last days shows.
type hubDeliverySeed struct {
	r2, c1, r0, m2, m2c, r1, m0, m0c string
	names                            issueNames
}

// seedHubDelivery writes the hub tests' delivery settings and hubDeliverySeed, before any view
// subscribes.
func seedHubDelivery(t *testing.T, rig *hubRig) hubDeliverySeed {
	t.Helper()
	seedHubSettings(t, rig.database)
	file := func(fields map[string]any) string {
		t.Helper()
		return createHubIssue(t, rig.handler, fields)
	}
	var seed hubDeliverySeed
	seed.r2 = file(map[string]any{"title": "Retire the old billing jobs", "priority": 2})
	seed.c1 = file(map[string]any{"title": "Drain the nightly invoice queue", "parent": seed.r2})
	seed.r0 = file(map[string]any{"title": "Checkout fails for every cart", "priority": 0})
	seed.m2 = file(map[string]any{"title": "Audit the retry budget", "priority": 2, "parent": seed.r0})
	seed.m2c = file(map[string]any{"title": "Graph the retry counts", "parent": seed.m2})
	seed.r1 = file(map[string]any{"title": "Move search to the new cluster", "priority": 1})
	seed.m0 = file(map[string]any{"title": "Search answers nothing", "priority": 0, "parent": seed.r1})
	seed.m0c = file(map[string]any{"title": "Roll back the index build", "parent": seed.m0})
	now := rig.clock.Now()
	for n, child := range []string{seed.c1, seed.m2c, seed.m0c} {
		upsertPR(t, rig.database, prNaming(n+1, now.Add(-time.Duration(3-n)*time.Hour), child))
	}
	seed.names = issueNames{
		seed.r2: "R2", seed.c1: "C1", seed.r0: "R0", seed.m2: "M2", seed.m2c: "M2c",
		seed.r1: "R1", seed.m0: "M0", seed.m0c: "M0c",
	}
	return seed
}

// subscribeDeliveryViews subscribes conn to the timeline of the last 7 days ("t") and the measures
// of the last 28 ("m"), and answers the snapshot each sub holds and each sub's kind.
func subscribeDeliveryViews(t *testing.T, rig *hubRig, conn *fakeLiveConn) (held map[string]json.RawMessage, kinds map[string]string) {
	t.Helper()
	kinds = map[string]string{"t": deliveryLiveTimeline, "m": deliveryLiveMeasures}
	rig.hub.Subscribe(conn, subscribeFrame("t", presetView(deliveryLiveTimeline, 7, "")))
	rig.hub.Subscribe(conn, subscribeFrame("m", presetView(deliveryLiveMeasures, 28, "")))
	held = map[string]json.RawMessage{}
	for sub, snapshot := range conn.firstSnapshots(t, "t", "m") {
		held[sub] = snapshot.Body
	}
	return held, kinds
}

// deltasQueued is every delta conn holds now, by sub in the order the hub sent them, past the
// freshness verdicts among them. Read after settle, which returns once every frame the hub owed
// its subs is queued on them, it is everything the hub sent for what the test wrote before it.
func (c *fakeLiveConn) deltasQueued(t *testing.T) map[string][]DeliveryLiveDelta {
	t.Helper()
	deltas := map[string][]DeliveryLiveDelta{}
	for {
		select {
		case sent := <-c.frames:
			switch frame := sent.frame.(type) {
			case DeliveryLiveDelta:
				deltas[frame.Sub] = append(deltas[frame.Sub], frame)
			case DeliveryLiveFreshness:
			default:
				t.Fatalf("frame %+v, want a delta or a freshness verdict", sent.frame)
			}
		default:
			return deltas
		}
	}
}

// p0Want is whether an issue is on the unowned P0 list and in the hub's open-P0 set after a write.
type p0Want struct {
	key string
	p0  bool
}

// TestTheDeliveryViewsCountAnInheritedP0 (H1): on the delivery seed, the hub's open-P0 set, the
// unowned P0 list of each view's snapshot and that of GET /api/v1/delivery/measures hold M0c,
// which takes P0 from M0, and not M2c, which takes P2 from M2 under a P0 root. A rule of the root
// alone holds them the other way round.
func TestTheDeliveryViewsCountAnInheritedP0(t *testing.T) {
	t.Parallel()
	rig := newHubRig(t, nil)
	seed := seedHubDelivery(t, rig)
	conn := newFakeLiveConn()
	held, kinds := subscribeDeliveryViews(t, rig, conn)
	// The hub reads its open-P0 set as it takes its event feed, which settle waits for.
	rig.settle(t)
	fresh := freshUnownedP0(t, rig.handler, held["m"])
	for _, want := range []p0Want{{seed.m0c, true}, {seed.m2c, false}} {
		name := seed.names.name(want.key)
		if got := rig.isP0(want.key); got != want.p0 {
			t.Errorf("the hub's open-P0 set holds %s: %t, want %t", name, got, want.p0)
		}
		for _, sub := range []string{"t", "m"} {
			if got := unownedP0Keys(t, held[sub], kinds[sub])[want.key]; got != want.p0 {
				t.Errorf("the %s view's snapshot lists %s on unowned_p0: %t, want %t", kinds[sub], name, got, want.p0)
			}
		}
		if got := fresh[want.key]; got != want.p0 {
			t.Errorf("GET /api/v1/delivery/measures lists %s on unowned_p0: %t, want %t", name, got, want.p0)
		}
	}
}

// TestAnInheritedP0ChangeReachesTheViewsLiveFromQuietEvents (H2): on the delivery seed, under
// alice's browser sign-in, which holds p0: R2's priority raised to 0; then an issue Y with no
// priority, whose open child Yc has none, moved under R0; then M0's own priority cleared, so M0
// and M0c take P1 from R1. After each write a fresh GET /api/v1/delivery/measures, the hub's
// open-P0 set and each view's next deltas count C1; then Y and Yc; then neither M0 nor M0c.
//
// The hub's set is read first with the views' next recompute held at its begin, so it holds what
// the write's events told the hub and nothing a recompute read. The events on the issues the write
// did not write (C1, Yc, M0c) are quiet, so a hub that takes only the events that wake a session
// fails there. The views' deltas come once the recompute is let go, and land on GET /measures.
func TestAnInheritedP0ChangeReachesTheViewsLiveFromQuietEvents(t *testing.T) {
	t.Parallel()
	var pause *notePause
	rig := newHubRig(t, func(hub *deliveryHub) { pause = pauseNotes(hub) })
	seed := seedHubDelivery(t, rig)
	y := createHubIssue(t, rig.handler, map[string]any{"title": "Page the search on-call"})
	yc := createHubIssue(t, rig.handler, map[string]any{"title": "Write the incident timeline", "parent": y})
	seed.names[y], seed.names[yc] = "Y", "Yc"
	conn := newFakeLiveConn()
	held, kinds := subscribeDeliveryViews(t, rig, conn)
	rig.settle(t)

	for _, step := range []struct {
		name  string
		write func()
		wants []p0Want
	}{
		{"R2's priority raised to 0", func() {
			patchHubIssue(t, rig.handler, seed.r2, map[string]any{"priority": 0})
		}, []p0Want{{seed.c1, true}}},
		{"Y moved under R0", func() {
			patchHubIssue(t, rig.handler, y, map[string]any{"parent": seed.r0})
		}, []p0Want{{y, true}, {yc, true}}},
		{"M0's own priority cleared", func() {
			patchHubIssue(t, rig.handler, seed.m0, map[string]any{"priority": nil})
		}, []p0Want{{seed.m0, false}, {seed.m0c, false}}},
	} {
		point := pause.at(recomputeBegin(""))
		t.Cleanup(point.proceed)
		step.write()
		fresh := freshUnownedP0(t, rig.handler, held["m"])
		for _, want := range step.wants {
			if got := fresh[want.key]; got != want.p0 {
				t.Fatalf("after %s, GET /api/v1/delivery/measures lists %s on unowned_p0: %t, want %t", step.name, seed.names.name(want.key), got, want.p0)
			}
		}
		rig.waitForEventCursor(t, eventHead(t, rig.database), "the events of "+step.name)
		for _, want := range step.wants {
			if got := rig.isP0(want.key); got != want.p0 {
				t.Errorf("after %s, with the views' recompute held, the hub's open-P0 set holds %s: %t, want %t, which the write's events tell it", step.name, seed.names.name(want.key), got, want.p0)
			}
		}
		point.proceed()

		rig.settle(t)
		deltas := conn.deltasQueued(t)
		for _, sub := range []string{"t", "m"} {
			kind := kinds[sub]
			if len(deltas[sub]) == 0 {
				t.Fatalf("after %s, the %s view was sent no delta; want the change live", step.name, kind)
			}
			for _, delta := range deltas[sub] {
				held[sub] = applyLiveDelta(t, kind, held[sub], delta)
			}
			live := unownedP0Keys(t, held[sub], kind)
			for _, want := range step.wants {
				if got := live[want.key]; got != want.p0 {
					t.Errorf("after %s, the %s view's live body lists %s on unowned_p0: %t, want %t", step.name, kind, seed.names.name(want.key), got, want.p0)
				}
			}
			want := unownedP0Fields(t, getBody(t, rig.handler, windowTarget(t, deliveryLiveMeasures, held[sub], "")), deliveryLiveMeasures)
			if got := unownedP0Fields(t, held[sub], kind); !reflect.DeepEqual(got, want) {
				t.Fatalf("after %s, the %s view's measures hold %v; GET /measures answers %v", step.name, kind, got, want)
			}
		}
		for _, want := range step.wants {
			if got := rig.isP0(want.key); got != want.p0 {
				t.Errorf("after %s and the views' recompute, the hub's open-P0 set holds %s: %t, want %t", step.name, seed.names.name(want.key), got, want.p0)
			}
		}
	}
}

// TestAPriorityChangeWithNoEventShowsAtTheNextFreshRead (H3): R2's priority set to 0 by direct
// SQL, with no event, as a write that a process of the build before serves leaves C1. A new
// subscription's snapshot, a fresh read, and GET /api/v1/delivery/measures then count C1, and so
// does the hub's open-P0 set, which every view recompute reads afresh: a set kept from events
// alone never hears of C1.
func TestAPriorityChangeWithNoEventShowsAtTheNextFreshRead(t *testing.T) {
	t.Parallel()
	rig := newHubRig(t, nil)
	seed := seedHubDelivery(t, rig)
	conn := newFakeLiveConn()
	rig.hub.Subscribe(conn, subscribeFrame("t", presetView(deliveryLiveTimeline, 7, "")))
	conn.snapshot(t, "t")
	rig.settle(t)

	if _, err := rig.database.Pool.Exec(t.Context(), `update issues set priority = 0 where key = $1`, seed.r2); err != nil {
		t.Fatalf("set R2's priority to 0 by direct SQL: %v", err)
	}
	later := newFakeLiveConn()
	rig.hub.Subscribe(later, subscribeFrame("m", presetView(deliveryLiveMeasures, 28, "")))
	snapshot := later.snapshot(t, "m")
	c1 := seed.names.name(seed.c1)
	if !unownedP0Keys(t, snapshot.Body, deliveryLiveMeasures)[seed.c1] {
		t.Errorf("a new subscription's snapshot does not list %s on unowned_p0, though it takes P0 from R2", c1)
	}
	if !freshUnownedP0(t, rig.handler, snapshot.Body)[seed.c1] {
		t.Errorf("GET /api/v1/delivery/measures does not list %s on unowned_p0, though it takes P0 from R2", c1)
	}
	rig.settle(t)
	if !rig.isP0(seed.c1) {
		t.Errorf("after the new subscription's recompute, the hub's open-P0 set does not hold %s", c1)
	}
}

// p0SetSeed is the issues of HUB a row of the hub's P0 set starts from, each filed through the
// API by alice: root, at its own P0; mid under it and child under mid, neither with a priority of
// its own, so both take P0 from root; other, at its own P1; and low under other, with none, which
// takes P1.
type p0SetSeed struct {
	root, mid, child, other, low string
	names                        issueNames
}

// newP0SetRig starts a hub whose notes the test can hold (pauseNotes) on p0SetSeed, with one view
// subscribed: the measures of the last 28 days. That view shows the unowned P0 list and names no
// issue, so an issue event marks it exactly when the issue is in the hub's P0 set before or after
// the event (deliveryView.showsIssue).
func newP0SetRig(t *testing.T) (*hubRig, *notePause, p0SetSeed) {
	t.Helper()
	var pause *notePause
	rig := newHubRig(t, func(hub *deliveryHub) { pause = pauseNotes(hub) })
	seedHubSettings(t, rig.database)
	var seed p0SetSeed
	seed.root = createHubIssue(t, rig.handler, map[string]any{"title": "Payments are down", "priority": 0})
	seed.mid = createHubIssue(t, rig.handler, map[string]any{"title": "Find the failing deploy", "parent": seed.root})
	seed.child = createHubIssue(t, rig.handler, map[string]any{"title": "Revert the bad config", "parent": seed.mid})
	seed.other = createHubIssue(t, rig.handler, map[string]any{"title": "Tidy the onboarding guide", "priority": 1})
	seed.low = createHubIssue(t, rig.handler, map[string]any{"title": "Fix a broken link", "parent": seed.other})
	seed.names = issueNames{seed.root: "root", seed.mid: "mid", seed.child: "child", seed.other: "other", seed.low: "low"}
	conn := newFakeLiveConn()
	rig.hub.Subscribe(conn, subscribeFrame("m", presetView(deliveryLiveMeasures, 28, "")))
	conn.snapshot(t, "m")
	rig.settle(t)
	return rig, pause, seed
}

// require fails the test unless the hub's P0 set holds key exactly when want does, at the moment
// when names.
func (s p0SetSeed) require(t *testing.T, rig *hubRig, when, key string, want bool) {
	t.Helper()
	if got := rig.isP0(key); got != want {
		t.Fatalf("%s, the hub's P0 set holds %s: %t, want %t", when, s.names.name(key), got, want)
	}
}

// setOwnPriority writes key's own priority by direct SQL and appends no event, so the database
// answers what the event a row then appends says, and a recompute's read agrees with it; or, in a
// row that appends none, holds what only a fresh read can find.
func setOwnPriority(t *testing.T, rig *hubRig, key string, priority *int) {
	t.Helper()
	if _, err := rig.database.Pool.Exec(t.Context(), `update issues set priority = $2 where key = $1`, key, priority); err != nil {
		t.Fatalf("set %s's own priority by direct SQL: %v", key, err)
	}
}

// heardEvent appends event through the server's broker (appendHubEvent) and waits for the hub to
// have applied it: the "event" note of its id, which receive makes once applyEventLocked has run.
func (r *hubRig) heardEvent(t *testing.T, event model.Event) {
	t.Helper()
	since := r.mark()
	appended := appendHubEvent(t, r, event)
	r.waitForEvent(t, since, fmt.Sprintf("the hub applying %s on %s (event %d)", event.Type, deref(event.IssueKey), appended.ID), func(note deliveryHubNote) bool {
		return note.what == "event" && note.eventID == appended.ID
	})
}

// holdRecomputes holds the views' next recompute at its begin until the caller lets it go or the
// test ends, so what the test reads of the hub's P0 set meanwhile is what events told the hub, and
// no recompute's read.
func holdRecomputes(t *testing.T, pause *notePause) *pausePoint {
	t.Helper()
	point := pause.at(recomputeBegin(""))
	t.Cleanup(point.proceed)
	return point
}

// resolvedPayload is an issue event's payload as this change writes it, cut to what the hub
// reads: the key, the status, the own priority, and resolved_priority and priority_from, each
// present, null included.
func resolvedPayload(key, status string, own, resolved *int, from *string) map[string]any {
	return map[string]any{"key": key, "status": status, "priority": own, "resolved_priority": resolved, "priority_from": from}
}

// olderPayload is an issue event's payload as a build before this change writes it, cut the same
// way: no resolved_priority key and no priority_from key.
func olderPayload(key, status string, own *int) map[string]any {
	return map[string]any{"key": key, "status": status, "priority": own}
}

// TestTheHubsP0SetFollowsEachEventAndRead (H4): one subtest per row of the plan's state table "The
// hub's P0 set". Each row's issue event is appended through the server's broker (appendHubEvent),
// with the views' recompute held at its begin (pauseNotes) while the row reads the set, so the set
// holds what the event told the hub; the row first writes by direct SQL what the event says, so a
// recompute that reads later agrees. The rows of a read make one with a merged pull request, which
// marks the measures view.
func TestTheHubsP0SetFollowsEachEventAndRead(t *testing.T) {
	t.Parallel()
	alice := model.Actor{Kind: "user", ID: "alice"}
	updated := func(key string, payload map[string]any) model.Event {
		return issueOwner(key).event("issue.updated", alice, payload)
	}
	for _, row := range []struct {
		name string
		run  func(t *testing.T)
	}{
		{"out, then an issue event with resolved_priority 0 and a status not done: in", func(t *testing.T) {
			rig, pause, seed := newP0SetRig(t)
			seed.require(t, rig, "before the event", seed.low, false)
			setOwnPriority(t, rig, seed.other, new(0))
			holdRecomputes(t, pause)
			rig.heardEvent(t, updated(seed.low, resolvedPayload(seed.low, "todo", nil, new(0), &seed.other)))
			seed.require(t, rig, "after the event", seed.low, true)
		}},
		{"in, then an issue event with resolved_priority 1 to 3 or null: out", func(t *testing.T) {
			for _, resolved := range []*int{new(1), new(2), new(3), nil} {
				level := "null"
				if resolved != nil {
					level = fmt.Sprint(*resolved)
				}
				t.Run("resolved_priority "+level, func(t *testing.T) {
					t.Parallel()
					rig, pause, seed := newP0SetRig(t)
					seed.require(t, rig, "before the event", seed.child, true)
					// root's own priority is what child resolves to, from root, or none at all.
					setOwnPriority(t, rig, seed.root, resolved)
					var from *string
					if resolved != nil {
						from = &seed.root
					}
					holdRecomputes(t, pause)
					rig.heardEvent(t, updated(seed.child, resolvedPayload(seed.child, "todo", nil, resolved, from)))
					seed.require(t, rig, "after the event", seed.child, false)
				})
			}
		}},
		{"in, then an issue event with resolved_priority 0 and status done: out", func(t *testing.T) {
			rig, pause, seed := newP0SetRig(t)
			seed.require(t, rig, "before the close", seed.child, true)
			if _, err := rig.database.Pool.Exec(t.Context(), `update issues set status = 'done', closed_at = now() where key = $1`, seed.child); err != nil {
				t.Fatalf("close child by direct SQL: %v", err)
			}
			holdRecomputes(t, pause)
			rig.heardEvent(t, issueOwner(seed.child).event("issue.closed", alice, resolvedPayload(seed.child, "done", nil, new(0), &seed.root)))
			seed.require(t, rig, "after the close", seed.child, false)
		}},
		{"in, then a quiet event of the resolved fields alone with resolved_priority 0: in, and the views that show it marked", func(t *testing.T) {
			rig, pause, seed := newP0SetRig(t)
			seed.require(t, rig, "before the quiet event", seed.child, true)
			// mid takes a P0 of its own, so child's priority comes from mid rather than root and
			// stays 0: the event names priority_from alone, as a write to mid appends it on child.
			setOwnPriority(t, rig, seed.mid, new(0))
			point := holdRecomputes(t, pause)
			quiet := resolvedPayload(seed.child, "todo", nil, new(0), &seed.mid)
			quiet["changed"] = []string{"priority_from"}
			rig.heardEvent(t, updated(seed.child, quiet))
			seed.require(t, rig, "after the quiet event", seed.child, true)
			point.wait(t, "the measures view's recompute, which the quiet event on child, in the set, marks")
		}},
		{"out, then a payload with no resolved_priority key and its own priority 0, not done: in", func(t *testing.T) {
			rig, pause, seed := newP0SetRig(t)
			seed.require(t, rig, "before the event", seed.low, false)
			setOwnPriority(t, rig, seed.low, new(0))
			holdRecomputes(t, pause)
			rig.heardEvent(t, updated(seed.low, olderPayload(seed.low, "todo", new(0))))
			seed.require(t, rig, "after the event", seed.low, true)
		}},
		{"in, then a payload with no resolved_priority key and its own priority 2: out", func(t *testing.T) {
			rig, pause, seed := newP0SetRig(t)
			seed.require(t, rig, "before the event", seed.child, true)
			setOwnPriority(t, rig, seed.child, new(2))
			holdRecomputes(t, pause)
			rig.heardEvent(t, updated(seed.child, olderPayload(seed.child, "todo", new(2))))
			seed.require(t, rig, "after the event", seed.child, false)
		}},
		{"in, then a payload with no resolved_priority key and its own priority null: in", func(t *testing.T) {
			// A build before this change edits child, which still has no priority of its own: the
			// payload cannot say what child resolves to, so the hub keeps what it knew.
			rig, pause, seed := newP0SetRig(t)
			seed.require(t, rig, "before the event", seed.child, true)
			holdRecomputes(t, pause)
			rig.heardEvent(t, updated(seed.child, olderPayload(seed.child, "todo", nil)))
			seed.require(t, rig, "after the event", seed.child, true)
		}},
		{"out, then a payload with no resolved_priority key and its own priority null: out", func(t *testing.T) {
			rig, pause, seed := newP0SetRig(t)
			seed.require(t, rig, "before the event", seed.low, false)
			holdRecomputes(t, pause)
			rig.heardEvent(t, updated(seed.low, olderPayload(seed.low, "todo", nil)))
			seed.require(t, rig, "after the event", seed.low, false)
		}},
		{"in or out, then a claim or a release: unchanged, and the views marked by the state", func(t *testing.T) {
			rig, pause, seed := newP0SetRig(t)
			seed.require(t, rig, "before the claims", seed.child, true)
			seed.require(t, rig, "before the claims", seed.low, false)
			start := rig.mark()
			for _, eventType := range []string{"issue.claimed", "issue.released"} {
				rig.heardEvent(t, issueOwner(seed.low).event(eventType, alice, map[string]any{"key": seed.low}))
				seed.require(t, rig, "after "+eventType+" on low", seed.low, false)
			}
			rig.settle(t)
			if n := rig.count(start, recomputeBegin("")); n != 0 {
				t.Fatalf("a claim and a release of low, which is out of the set, recomputed the measures view %d times, want none", n)
			}
			for _, eventType := range []string{"issue.claimed", "issue.released"} {
				point := holdRecomputes(t, pause)
				rig.heardEvent(t, issueOwner(seed.child).event(eventType, alice, map[string]any{"key": seed.child}))
				seed.require(t, rig, "after "+eventType+" on child", seed.child, true)
				point.wait(t, "the measures view's recompute, which "+eventType+" on child, in the set, marks")
				point.proceed()
				rig.settle(t)
			}
		}},
		{"out, then a recompute's read that finds it in OpenP0Keys, with no event on it heard since: in", func(t *testing.T) {
			rig, _, seed := newP0SetRig(t)
			seed.require(t, rig, "before the read", seed.low, false)
			setOwnPriority(t, rig, seed.other, new(0))
			start := rig.mark()
			upsertPR(t, rig.database, hubPR(1, rig.clock.Now().Add(-2*time.Hour)))
			rig.settle(t)
			if n := rig.recomputesSince(start); n == 0 {
				t.Fatal("the merge recomputed no view, so no read ran")
			}
			seed.require(t, rig, "after the recompute", seed.low, true)
		}},
		{"in, then a recompute's read that does not find it, with no event on it heard since: out", func(t *testing.T) {
			rig, _, seed := newP0SetRig(t)
			seed.require(t, rig, "before the read", seed.child, true)
			setOwnPriority(t, rig, seed.root, new(2))
			start := rig.mark()
			upsertPR(t, rig.database, hubPR(1, rig.clock.Now().Add(-2*time.Hour)))
			rig.settle(t)
			if n := rig.recomputesSince(start); n == 0 {
				t.Fatal("the merge recomputed no view, so no read ran")
			}
			seed.require(t, rig, "after the recompute", seed.child, false)
			seed.require(t, rig, "after the recompute", seed.root, false)
		}},
		{"out, then an event with resolved_priority 0 heard while a read from before it, which does not find it, is under way: in, the event winning", func(t *testing.T) {
			rig, pause, seed := newP0SetRig(t)
			seed.require(t, rig, "before the read", seed.low, false)
			start := rig.mark()
			point := pause.at(noteIs("recompute read"))
			t.Cleanup(point.proceed)
			upsertPR(t, rig.database, hubPR(1, rig.clock.Now().Add(-2*time.Hour)))
			point.wait(t, "recompute read")
			setOwnPriority(t, rig, seed.other, new(0))
			rig.heardEvent(t, updated(seed.low, resolvedPayload(seed.low, "todo", nil, new(0), &seed.other)))
			seed.require(t, rig, "once the event is heard, while the read from before it is held", seed.low, true)
			point.proceed()
			rig.waitFor(t, start, "the held recompute's end", noteIs("recompute end"))
			seed.require(t, rig, "once the read from before the event, which does not find it, is installed", seed.low, true)
		}},
		{"in, then the feed closing and taken again, with OpenP0Keys not finding it: out", func(t *testing.T) {
			rig, _, seed := newP0SetRig(t)
			seed.require(t, rig, "before the feed closes", seed.child, true)
			setOwnPriority(t, rig, seed.root, new(2))
			start := rig.mark()
			endRelayGeneration(rig.relay)
			rig.waitForEvent(t, start, "the hub's feed taken again", noteIs("events seeded"))
			seed.require(t, rig, "once the feed is taken again", seed.child, false)
			seed.require(t, rig, "once the feed is taken again", seed.root, false)
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			row.run(t)
		})
	}
}
