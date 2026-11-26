import { expect, test } from "bun:test";
import after from "../fixtures/live/after.json";
import agent from "../fixtures/live/agent.json";
import delta from "../fixtures/live/delta.json";
import ended from "../fixtures/live/ended.json";
import error from "../fixtures/live/error.json";
import evalsAfter from "../fixtures/live/evals_after.json";
import evalsDelta from "../fixtures/live/evals_delta.json";
import evalsSnapshot from "../fixtures/live/evals_snapshot.json";
import event from "../fixtures/live/event.json";
import eventsStart from "../fixtures/live/events_start.json";
import freshness from "../fixtures/live/freshness.json";
import heartbeat from "../fixtures/live/heartbeat.json";
import itemsAfter from "../fixtures/live/items_after.json";
import itemsDelta from "../fixtures/live/items_delta.json";
import itemsSnapshot from "../fixtures/live/items_snapshot.json";
import projectTimelineAfter from "../fixtures/live/project_timeline_after.json";
import projectTimelineDelta from "../fixtures/live/project_timeline_delta.json";
import projectTimelineSnapshot from "../fixtures/live/project_timeline_snapshot.json";
import snapshot from "../fixtures/live/snapshot.json";
import subscribeAgent from "../fixtures/live/subscribe_agent.json";
import subscribeEvals from "../fixtures/live/subscribe_evals.json";
import subscribeEvents from "../fixtures/live/subscribe_events.json";
import subscribeItems from "../fixtures/live/subscribe_items.json";
import subscribeProjectTimeline from "../fixtures/live/subscribe_project_timeline.json";
import subscribeTimeline from "../fixtures/live/subscribe_timeline.json";
import unsubscribe from "../fixtures/live/unsubscribe.json";
import { AGENT_STREAM_PROTOCOL, type AgentStreamResponder } from "./agent-stream";
import {
  type AgentLiveView,
  type DeliveryLiveCounts,
  type DeliveryLiveDelta,
  type DeliveryLiveFreshness,
  type DeliveryLiveItemChanges,
  type DeliveryLiveList,
  type DeliveryLiveMap,
  type DeliveryLiveResume,
  type DeliveryLiveRevChanges,
  type DeliveryLiveRevs,
  type DeliveryLiveSnapshot,
  type DeliveryLiveView,
  type DeliveryTimelineWithMeasures,
  type DispatchEvent,
  type EvalItemRow,
  type EvalItemsResponse,
  type EvalListItem,
  type EvalListResponse,
  type EventsLiveResume,
  type EventsLiveView,
  LIVE_SOCKET_MAX_AGE_CLOSE,
  LIVE_SOCKET_MAX_AGE_MS,
  LIVE_SOCKET_MAX_SUBSCRIPTIONS,
  LIVE_SOCKET_MIN_AGE_MS,
  type LiveAgentFrame,
  type LiveClientFrame,
  type LiveEnded,
  type LiveError,
  type LiveEventFrame,
  type LiveEventsStart,
  type LiveHeartbeat,
  type LiveServerFrame,
  type LiveSubscribe,
  type LiveUnsubscribe,
  type LiveView,
  type ProjectTimelineResponse,
} from "./dispatch-api";

// The frames Dispatch sends, in fixtures/live, are written by the Dispatch server's own diff and
// event feed (TestTheLiveFixturesAreWhatTheDiffWrites, packages/envoy/internal/dispatch/api), so
// each is what the live socket sends; the page's own frames there are read by the socket's own
// decoding (TestTheClientLiveFixturesAreFramesTheSocketReads). These tests hold them to the Live*
// and DeliveryLive* contract key for key: each list below must name every key of its type, or the
// type check fails, so a key the contract gains or loses fails here until the server matches it.

/** `list`, which must hold every member of U. */
function allOf<U extends string>() {
  return <const L extends readonly U[]>(
    list: L & ([Exclude<U, L[number]>] extends [never] ? unknown : never)
  ): L => list;
}

const SNAPSHOT_KEYS = allOf<keyof DeliveryLiveSnapshot>()([
  "type",
  "sub",
  "seq",
  "changed_at",
  "body",
  "revs",
]);
const DELTA_KEYS = allOf<keyof DeliveryLiveDelta>()([
  "type",
  "sub",
  "seq",
  "changed_at",
  "set",
  "items",
  "maps",
  "counts",
  "revs",
]);
/** Every frame Dispatch sends, by its type, with the keys it carries: a frame type without a row,
 *  or a row naming a key its type lacks or missing one it has, fails the type check. */
const SERVER_FRAME_KEYS = {
  snapshot: SNAPSHOT_KEYS,
  delta: DELTA_KEYS,
  freshness: allOf<keyof DeliveryLiveFreshness>()(["type", "freshness", "stale"]),
  events_start: allOf<keyof LiveEventsStart>()(["type", "sub", "since", "replay_complete"]),
  event: allOf<keyof LiveEventFrame>()(["type", "sub", "id", "event"]),
  agent: allOf<keyof LiveAgentFrame>()(["type", "sub", "event", "data"]),
  ended: allOf<keyof LiveEnded>()(["type", "sub", "reason"]),
  error: allOf<keyof LiveError>()(["type", "sub", "code", "message"]),
  heartbeat: allOf<keyof LiveHeartbeat>()(["type"]),
} satisfies Record<LiveServerFrame["type"], readonly string[]>;
/** Every frame a page sends, by its type, with the keys it carries. */
const CLIENT_FRAME_KEYS = {
  subscribe: allOf<keyof LiveSubscribe>()(["type", "sub", "view", "resume"]),
  unsubscribe: allOf<keyof LiveUnsubscribe>()(["type", "sub"]),
} satisfies Record<LiveClientFrame["type"], readonly string[]>;
const VIEW_KINDS = allOf<LiveView["kind"]>()([
  "timeline",
  "measures",
  "pipeline",
  "legion",
  "legion_issue",
  "evals",
  "items",
  "project-timeline",
  "events",
  "agent",
]);
const DELIVERY_RESUME_KEYS = allOf<keyof DeliveryLiveResume>()(["seq", "revs"]);
const EVENTS_RESUME_KEYS = allOf<keyof EventsLiveResume>()(["since"]);
const AGENT_EVENTS = allOf<LiveAgentFrame["event"]>()(["responder", "replay", "frame"]);
const RESPONDER_KEYS = allOf<keyof AgentStreamResponder>()(["v", "responding"]);
const ENDED_REASONS = allOf<LiveEnded["reason"]>()(["behind", "dropped"]);
/** Every key of the event an event frame carries. */
const EVENT_KEYS = allOf<keyof DispatchEvent>()([
  "id",
  "issue_key",
  "artifact_id",
  "project",
  "seq",
  "type",
  "actor",
  "notify",
  "created_at",
  "payload",
  "trees",
]);
/** The frames that are not one subscription's: freshness goes to every subscriber of the socket,
 *  heartbeat to the socket itself. */
const SOCKET_WIDE: Partial<Record<LiveServerFrame["type"], true>> = {
  freshness: true,
  heartbeat: true,
};
const TIMELINE_KEYS = allOf<keyof DeliveryTimelineWithMeasures>()([
  "window",
  "prs",
  "waiting",
  "runs",
  "facet_counts",
  "color_counts",
  "components",
  "issue_titles",
  "freshness",
  "measures",
  "session_groups",
  "session_titles",
  "session_pr_counts",
]);
const REVS_KEYS = allOf<keyof DeliveryLiveRevs>()(["items", "maps", "counts", "fields"]);
const REV_CHANGE_KEYS = allOf<keyof DeliveryLiveRevChanges>()([
  "items",
  "maps",
  "counts",
  "fields",
]);
const ITEM_CHANGE_KEYS = allOf<keyof DeliveryLiveItemChanges<DeliveryLiveList>>()([
  "upsert",
  "remove",
]);
const LISTS = allOf<DeliveryLiveList>()(["prs", "runs", "session_groups", "items"]);
/** The timeline's lists; an evals view's one list is `items`. */
const TIMELINE_LISTS = [
  "prs",
  "runs",
  "session_groups",
] as const satisfies readonly DeliveryLiveList[];
const EVALS_KEYS = allOf<keyof EvalListResponse>()([
  "items",
  "total",
  "limit",
  "offset",
  "window",
  "compare",
  "freshness",
  "delivery_freshness",
]);
/** An evals body's fields sent whole: every one but its list. */
const EVALS_WHOLE = EVALS_KEYS.filter((key) => key !== "items");
const EVAL_ITEM_KEYS = allOf<keyof EvalListItem>()([
  "id",
  "definition",
  "owner",
  "series",
  "verdict",
]);
const MAPS = allOf<DeliveryLiveMap>()([
  "issue_titles",
  "session_titles",
  "session_pr_counts",
  "components",
]);
const COUNTS = allOf<DeliveryLiveCounts>()(["facet_counts", "color_counts"]);
/** An items body's fields, every one sent whole: its rows carry no id. */
const ITEMS_KEYS = allOf<keyof EvalItemsResponse>()([
  "items",
  "total",
  "limit",
  "offset",
  "window",
  "compare",
]);
const ITEM_ROW_KEYS = allOf<keyof EvalItemRow>()([
  "row_key",
  "row_kind",
  "cols",
  "measured_at",
  "source",
  "href",
  "links",
  "range",
]);
const PROJECT_TIMELINE_KEYS = allOf<keyof ProjectTimelineResponse>()([
  "items",
  "total",
  "limit",
  "offset",
  "window",
  "waiting",
  "facet_counts",
  "color_counts",
  "components",
  "issue_titles",
  "session_groups",
  "session_titles",
  "session_pr_counts",
  "freshness",
]);
/** A project timeline's lists: its rows and its session groups. */
const PROJECT_TIMELINE_LISTS = [
  "items",
  "session_groups",
] as const satisfies readonly DeliveryLiveList[];
/** A project timeline's fields sent whole, and its window, sent with any change that moves it. */
const PROJECT_TIMELINE_FIELDS = ["limit", "offset", "total", "waiting", "window"];
const PROJECT_TIMELINE_ROW_KINDS = ["merge", "deploy", "failed_run"];

const REVISION = /^[0-9a-f]{16}$/;

const sorted = (keys: readonly string[]) => [...keys].sort();
const keysOf = (value: object) => sorted(Object.keys(value));

/** Every leaf of a revisions section, `depth` levels of keys deep. */
function leaves(section: unknown, depth: number): unknown[] {
  if (depth === 0) return [section];
  return Object.values(section as Record<string, unknown>).flatMap((value) =>
    leaves(value, depth - 1)
  );
}

test("a snapshot frame is the timeline's GET body with a revision for every key it holds", () => {
  for (const frame of [snapshot, after]) {
    expect(keysOf(frame)).toEqual(sorted(SNAPSHOT_KEYS));
    expect(frame.type).toBe("snapshot");
    expect(frame.seq).toMatch(/^\d+$/);
    expect(keysOf(frame.body)).toEqual(sorted(TIMELINE_KEYS));
    expect(keysOf(frame.revs)).toEqual(sorted(REVS_KEYS));
    expect(keysOf(frame.revs.items)).toEqual(sorted(TIMELINE_LISTS));
    expect(keysOf(frame.revs.maps)).toEqual(sorted(MAPS));
    expect(keysOf(frame.revs.counts)).toEqual(sorted(COUNTS));
    for (const list of TIMELINE_LISTS) {
      const ids = frame.body[list].map((item: { id: string | number }) => String(item.id));
      expect(sorted(ids)).toEqual(keysOf(frame.revs.items[list]));
    }
    for (const map of MAPS) expect(keysOf(frame.revs.maps[map])).toEqual(keysOf(frame.body[map]));
    for (const counts of COUNTS) {
      expect(keysOf(frame.revs.counts[counts])).toEqual(keysOf(frame.body[counts]));
    }
    expect(keysOf(frame.revs.fields)).toEqual(["measures", "waiting", "window"]);
    for (const revision of [
      ...leaves(frame.revs.items, 2),
      ...leaves(frame.revs.maps, 2),
      ...leaves(frame.revs.counts, 3),
      ...leaves(frame.revs.fields, 1),
    ]) {
      expect(revision).toMatch(REVISION);
    }
  }
});

test("a delta frame carries lists by id, maps by key, counts two levels deep, and null for a key that left", () => {
  expect(keysOf(delta)).toEqual(sorted(DELTA_KEYS));
  expect(delta.type).toBe("delta");
  expect(delta.seq).toMatch(/^\d+$/);
  for (const [list, changes] of Object.entries(delta.items)) {
    expect(LISTS).toContain(list as DeliveryLiveList);
    expect(keysOf(changes)).toEqual(sorted(ITEM_CHANGE_KEYS));
  }
  for (const map of Object.keys(delta.maps)) expect(MAPS).toContain(map as DeliveryLiveMap);
  for (const counts of Object.keys(delta.counts)) {
    expect(COUNTS).toContain(counts as DeliveryLiveCounts);
  }
  for (const count of leaves(delta.counts, 3)) {
    expect(count === null || Number.isInteger(count)).toBe(true);
  }
  // This fixture's change moves at least one of each, so every section is exercised.
  expect(delta.maps.issue_titles).toEqual({ "ACME-2": null });
  // octocat's count holds at 2 (#1 left, #4 merged), so only hubot's, which fell to none, is sent.
  expect(delta.counts.facet_counts.author).toEqual({ hubot: null });
  expect(delta.items.prs.remove).toEqual(["acme/widgets#1", "acme/widgets#2"]);
  expect(Object.keys(delta.set).sort()).toEqual(["measures", "waiting", "window"]);

  for (const section of Object.keys(delta.revs)) {
    expect(REV_CHANGE_KEYS).toContain(section as keyof DeliveryLiveRevChanges);
  }
  for (const revision of [
    ...leaves(delta.revs.items, 2),
    ...leaves(delta.revs.maps, 2),
    ...leaves(delta.revs.counts, 3),
  ]) {
    expect(revision === null || REVISION.test(String(revision))).toBe(true);
  }
  for (const revision of leaves(delta.revs.fields, 1)) expect(revision).toMatch(REVISION);
});

test("an evals snapshot is the list read's body, its evals by id and every other field whole", () => {
  for (const frame of [evalsSnapshot, evalsAfter]) {
    expect(keysOf(frame)).toEqual(sorted(SNAPSHOT_KEYS));
    expect(frame.type).toBe("snapshot");
    expect(frame.seq).toMatch(/^\d+$/);
    expect(keysOf(frame.body)).toEqual(sorted(EVALS_KEYS));
    expect(keysOf(frame.revs)).toEqual(sorted(REVS_KEYS));
    expect(keysOf(frame.revs.items)).toEqual(["items"]);
    expect(frame.revs.maps).toEqual({});
    expect(frame.revs.counts).toEqual({});
    // An eval's id is its eval id, `<project>/<slug>`, which its definition carries too.
    for (const item of frame.body.items) {
      expect(keysOf(item)).toEqual(sorted(EVAL_ITEM_KEYS));
      expect(item.id).toBe(item.definition.eval_id);
    }
    const ids = frame.body.items.map((item) => item.id);
    expect(sorted(ids)).toEqual(keysOf(frame.revs.items.items));
    expect(keysOf(frame.revs.fields)).toEqual(sorted(EVALS_WHOLE));
    for (const revision of [...leaves(frame.revs.items, 2), ...leaves(frame.revs.fields, 1)]) {
      expect(revision).toMatch(REVISION);
    }
  }
});

test("an evals delta upserts the evals that moved by id and sets the fields that moved whole", () => {
  expect(keysOf(evalsDelta)).toEqual(sorted(DELTA_KEYS));
  expect(keysOf(evalsDelta.items)).toEqual(["items"]);
  expect(keysOf(evalsDelta.items.items)).toEqual(sorted(ITEM_CHANGE_KEYS));
  // A reading moved pr-size's numbers and lead-time was registered: each sent whole, in the
  // body's order; the total counts lead-time, and the folded rows' new age moved the freshness.
  expect(evalsDelta.items.items.upsert.map((item) => item.id)).toEqual([
    "ACME/lead-time",
    "ACME/pr-size",
  ]);
  expect(evalsDelta.items.items.remove).toEqual([]);
  expect(evalsDelta.maps).toEqual({});
  expect(evalsDelta.counts).toEqual({});
  expect(keysOf(evalsDelta.set)).toEqual(["freshness", "total"]);
  for (const revision of [
    ...leaves(evalsDelta.revs.items, 2),
    ...leaves(evalsDelta.revs.fields, 1),
  ]) {
    expect(revision).toMatch(REVISION);
  }
});

test("an items snapshot is the items route's body, every field of it whole", () => {
  for (const frame of [itemsSnapshot, itemsAfter]) {
    expect(keysOf(frame)).toEqual(sorted(SNAPSHOT_KEYS));
    expect(frame.type).toBe("snapshot");
    expect(frame.seq).toMatch(/^\d+$/);
    expect(keysOf(frame.body)).toEqual(sorted(ITEMS_KEYS));
    for (const row of frame.body.items) expect(keysOf(row)).toEqual(sorted(ITEM_ROW_KEYS));
    expect(keysOf(frame.revs)).toEqual(sorted(REVS_KEYS));
    expect(frame.revs.items).toEqual({});
    expect(frame.revs.maps).toEqual({});
    expect(frame.revs.counts).toEqual({});
    expect(keysOf(frame.revs.fields)).toEqual(sorted(ITEMS_KEYS));
    for (const revision of leaves(frame.revs.fields, 1)) expect(revision).toMatch(REVISION);
  }
});

test("an items delta sets the rows whole, with the fields that moved beside them", () => {
  expect(keysOf(itemsDelta)).toEqual(sorted(DELTA_KEYS));
  // A job failed again and another failed for the first time: the rows go whole, in the body's
  // order, with the total that counts them, and no list is sent item by item.
  expect(itemsDelta.items).toEqual({});
  expect(itemsDelta.maps).toEqual({});
  expect(itemsDelta.counts).toEqual({});
  expect(keysOf(itemsDelta.set)).toEqual(["items", "total"]);
  expect(itemsDelta.set.items.map((row) => row.row_key)).toEqual([
    "acme/widgets/build / unit",
    "acme/widgets/lint",
  ]);
  expect(itemsDelta.set.items).toEqual(itemsAfter.body.items);
  for (const revision of leaves(itemsDelta.revs.fields, 1)) expect(revision).toMatch(REVISION);
});

test("a project timeline snapshot is its route's body, rows newest first by id, and no measures", () => {
  for (const frame of [projectTimelineSnapshot, projectTimelineAfter]) {
    expect(keysOf(frame)).toEqual(sorted(SNAPSHOT_KEYS));
    expect(frame.type).toBe("snapshot");
    expect(keysOf(frame.body)).toEqual(sorted(PROJECT_TIMELINE_KEYS));
    expect(keysOf(frame.revs.items)).toEqual(sorted(PROJECT_TIMELINE_LISTS));
    expect(keysOf(frame.revs.maps)).toEqual(sorted(MAPS));
    expect(keysOf(frame.revs.counts)).toEqual(sorted(COUNTS));
    expect(keysOf(frame.revs.fields)).toEqual(PROJECT_TIMELINE_FIELDS);
    const ids = frame.body.items.map((row: { id: string | number }) => String(row.id));
    expect(sorted(ids)).toEqual(keysOf(frame.revs.items.items));
    const times: number[] = [];
    for (const row of frame.body.items as readonly Record<string, unknown>[]) {
      expect(PROJECT_TIMELINE_ROW_KINDS).toContain(row.kind as string);
      expect(row).not.toHaveProperty("measures");
      // A run's row names its repository and when GitHub created it, which orders it.
      if (row.kind !== "merge")
        expect(Object.keys(row)).toEqual(expect.arrayContaining(["repo", "created_at"]));
      times.push(Date.parse(String(row.kind === "merge" ? row.merged_at : row.created_at)));
    }
    expect(times).toEqual([...times].sort((a, b) => b - a));
    for (const revision of [
      ...leaves(frame.revs.items, 2),
      ...leaves(frame.revs.maps, 2),
      ...leaves(frame.revs.counts, 3),
      ...leaves(frame.revs.fields, 1),
    ]) {
      expect(revision).toMatch(REVISION);
    }
  }
  // The page of two rows does not reach the window's start, so it holds no waiting line; grown to
  // every row, it does.
  expect(projectTimelineSnapshot.body.waiting).toBeNull();
  expect(projectTimelineAfter.body.waiting).not.toBeNull();
});

test("a project timeline's growth sends the rows the page lacked and the fields that moved", () => {
  expect(keysOf(projectTimelineDelta)).toEqual(sorted(DELTA_KEYS));
  expect(keysOf(projectTimelineDelta.items)).toEqual(["items"]);
  // The page held the two newest rows; the growth upserts the two older ones, in the body's
  // order, and removes none.
  expect(projectTimelineDelta.items.items.upsert.map((row) => row.id)).toEqual([
    "acme/widgets#2",
    "acme/widgets#1",
  ]);
  expect(projectTimelineDelta.items.items.remove).toEqual([]);
  expect(projectTimelineDelta.maps).toEqual({});
  expect(projectTimelineDelta.counts).toEqual({});
  expect(keysOf(projectTimelineDelta.set)).toEqual(["limit", "waiting"]);
});

test("an error frame names its code and its message", () => {
  expect(keysOf(error)).toEqual(sorted(SERVER_FRAME_KEYS.error));
  expect(error).toEqual({
    type: "error",
    sub: "timeline-1",
    code: "DELIVERY_NOT_CONFIGURED",
    message:
      "delivery is not configured: no project has delivery on; turn it on through PUT /api/v1/projects/{key}/settings/delivery",
  });
});

test("a heartbeat frame carries its type and nothing else", () => {
  expect(keysOf(heartbeat)).toEqual(sorted(SERVER_FRAME_KEYS.heartbeat));
  expect(heartbeat).toEqual({ type: "heartbeat" });
  expect(Object.keys(SERVER_FRAME_KEYS)).toContain(heartbeat.type);
});

test("a freshness frame carries the freshness object a delivery body carries, and the stale sources", () => {
  expect(keysOf(freshness)).toEqual(sorted(SERVER_FRAME_KEYS.freshness));
  expect(freshness.type).toBe("freshness");
  expect(keysOf(freshness.freshness)).toEqual(keysOf(snapshot.body.freshness));
  for (const source of freshness.stale) expect(typeof source).toBe("string");
});

test("an events_start frame names the decimal id its subscription starts after and whether its replay covers the gap", () => {
  expect(keysOf(eventsStart)).toEqual(sorted(SERVER_FRAME_KEYS.events_start));
  expect(eventsStart.type).toBe("events_start");
  expect(eventsStart.since).toMatch(/^\d+$/);
  expect(typeof eventsStart.replay_complete).toBe("boolean");
});

test("an event frame carries the event's decimal id and the event itself", () => {
  expect(keysOf(event)).toEqual(sorted(SERVER_FRAME_KEYS.event));
  expect(event.type).toBe("event");
  expect(event.id).toBe(String(event.event.id));
  expect(keysOf(event.event)).toEqual(sorted(EVENT_KEYS));
});

test("an ended frame names its subscription and why Dispatch ended it", () => {
  expect(keysOf(ended)).toEqual(sorted(SERVER_FRAME_KEYS.ended));
  expect(ended.type).toBe("ended");
  expect(ENDED_REASONS).toContain(ended.reason as LiveEnded["reason"]);
});

test("an agent frame names its event, and its responder verdict is the one Dispatch writes", () => {
  expect(keysOf(agent)).toEqual(sorted(SERVER_FRAME_KEYS.agent));
  expect(agent.type).toBe("agent");
  expect(AGENT_EVENTS).toContain(agent.event as LiveAgentFrame["event"]);
  // A replay's or a frame's data is the session's own JSON, which Dispatch relays unread; the
  // responder verdict is the one agent frame whose data Dispatch writes itself.
  expect(agent.event).toBe("responder");
  expect(keysOf(agent.data)).toEqual(sorted(RESPONDER_KEYS));
  expect(agent.data).toEqual({ v: AGENT_STREAM_PROTOCOL, responding: true });
});

test("every frame names its type, and every frame of one subscription names the subscription", () => {
  for (const [type, keys] of Object.entries({ ...SERVER_FRAME_KEYS, ...CLIENT_FRAME_KEYS })) {
    expect(keys).toContain("type");
    if (SOCKET_WIDE[type as LiveServerFrame["type"]] !== true) expect(keys).toContain("sub");
  }
  for (const list of [
    ...Object.values(SERVER_FRAME_KEYS),
    ...Object.values(CLIENT_FRAME_KEYS),
    VIEW_KINDS,
    AGENT_EVENTS,
    ENDED_REASONS,
  ]) {
    expect(new Set(list).size).toBe(list.length);
  }
});

test("a resume's keys tell a delivery view's resume from an events view's", () => {
  // Dispatch refuses a resume whose shape is not its view's kind, so the two share no key.
  for (const key of DELIVERY_RESUME_KEYS) expect(EVENTS_RESUME_KEYS).not.toContain(key);
});

test("the page's frames carry every key of their types, and the socket reads each", () => {
  const timelineViewKeys = allOf<keyof Extract<DeliveryLiveView, { kind: "timeline" }>>()([
    "kind",
    "window",
    "query",
  ]);
  const evalsViewKeys = allOf<keyof Extract<DeliveryLiveView, { kind: "evals" }>>()([
    "kind",
    "project",
    "window",
    "query",
  ]);
  const itemsViewKeys = allOf<keyof Extract<DeliveryLiveView, { kind: "items" }>>()([
    "kind",
    "project",
    "slug",
    "window",
    "query",
  ]);
  const projectTimelineViewKeys = allOf<
    keyof Extract<DeliveryLiveView, { kind: "project-timeline" }>
  >()(["kind", "project", "window", "query"]);
  const eventsViewKeys = allOf<keyof EventsLiveView>()(["kind"]);
  const agentViewKeys = allOf<keyof AgentLiveView>()(["kind", "session_id"]);
  const subscribeKeys = sorted(CLIENT_FRAME_KEYS.subscribe);

  expect(keysOf(subscribeTimeline)).toEqual(subscribeKeys);
  expect(keysOf(subscribeTimeline.view)).toEqual(sorted(timelineViewKeys));
  expect(keysOf(subscribeTimeline.resume)).toEqual(sorted(DELIVERY_RESUME_KEYS));
  expect(keysOf(subscribeTimeline.resume.revs)).toEqual(sorted(REVS_KEYS));
  expect(keysOf(subscribeEvals)).toEqual(subscribeKeys);
  expect(keysOf(subscribeEvals.view)).toEqual(sorted(evalsViewKeys));
  expect(keysOf(subscribeEvals.resume)).toEqual(sorted(DELIVERY_RESUME_KEYS));
  expect(keysOf(subscribeItems)).toEqual(subscribeKeys);
  expect(keysOf(subscribeItems.view)).toEqual(sorted(itemsViewKeys));
  expect(keysOf(subscribeItems.resume)).toEqual(sorted(DELIVERY_RESUME_KEYS));
  expect(keysOf(subscribeProjectTimeline)).toEqual(subscribeKeys);
  expect(keysOf(subscribeProjectTimeline.view)).toEqual(sorted(projectTimelineViewKeys));
  expect(keysOf(subscribeProjectTimeline.resume)).toEqual(sorted(DELIVERY_RESUME_KEYS));
  expect(keysOf(subscribeEvents)).toEqual(subscribeKeys);
  expect(keysOf(subscribeEvents.view)).toEqual(sorted(eventsViewKeys));
  expect(keysOf(subscribeEvents.resume)).toEqual(sorted(EVENTS_RESUME_KEYS));
  // An agent view takes no resume.
  expect(keysOf(subscribeAgent)).toEqual(subscribeKeys.filter((key) => key !== "resume"));
  expect(keysOf(subscribeAgent.view)).toEqual(sorted(agentViewKeys));
  expect(keysOf(unsubscribe)).toEqual(sorted(CLIENT_FRAME_KEYS.unsubscribe));
});

// The values themselves, not a range: the server, the page and the e2e probe read these, and a
// change to any of them is a change to the wire both sides must ship together.
test("a socket ends at an age between thirty minutes and an hour, closing 4001, and holds 64 subscriptions", () => {
  expect(LIVE_SOCKET_MIN_AGE_MS).toBe(30 * 60 * 1000);
  expect(LIVE_SOCKET_MAX_AGE_MS).toBe(60 * 60 * 1000);
  // An application code, apart from 4000, the page's own close of a silent socket.
  expect(LIVE_SOCKET_MAX_AGE_CLOSE).toBe(4001);
  expect(LIVE_SOCKET_MAX_SUBSCRIPTIONS).toBe(64);
});
