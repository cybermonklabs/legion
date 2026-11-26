import type { AgentStreamResponder } from "./agent-stream";
import type {
  DeliveryMeasuresResponse,
  DeliveryPipelineResponse,
  DeliveryTimelineWithMeasures,
  EvalItemsResponse,
  EvalListResponse,
  LegionBoardResponse,
  LegionIssueResponse,
  ProjectTimelineResponse,
} from "./dispatch-api.generated";
import type { DispatchEvent } from "./dispatch-api-events";

/**
 * The live socket (`/ws/live`): the views a page subscribes to, the resume each kind takes, and
 * every frame Dispatch sends back.
 */

/** A window by preset, whose end stays at the present, or by the exact `from`/`to` of a custom range. */
export type DeliveryLiveWindow =
  | { readonly preset: 7 | 14 | 28 | 84 }
  | { readonly from: string; readonly to: string };

/** One view a page shows. `query` is the query string the page would send to the matching GET route
 *  without `from`/`to` (search and every facet), so the live answer and the GET answer are one. A
 *  legion view names the project whose board it shows and nothing else: the board has no window,
 *  and `project` is all `GET /api/v1/projects/{key}/legion/board` reads. Its snapshot and deltas
 *  carry a `LegionBoardResponse`. A legion_issue view names one issue and nothing else, which is
 *  all `GET /api/v1/issues/{key}/legion` reads, and its snapshot and deltas carry a
 *  `LegionIssueResponse`. It is never refused for a project that names no daemon: its body says
 *  `configured` false. Its one refusal is `NOT_FOUND`, for a key Dispatch holds no issue of. An
 *  evals view names the project whose evals it shows, its window, and the rest of what
 *  `GET /api/v1/projects/{key}/evals` would be sent as its `query`: the compare range
 *  (`compare_from`, `compare_to`), the repositories (`repo`), the kind (`kind`) and the page
 *  (`limit`, `offset`). Its snapshot and deltas carry an `EvalListResponse`, its evals sent by
 *  id. An items view names the project and the `slug` of the eval whose rows it shows, its
 *  window, and the rest of what `GET /api/v1/projects/{key}/evals/{slug}/items` would be sent as
 *  its `query`: the compare range, the series (`series`), the row kind and row (`row_kind`,
 *  `row`), the repositories, the order (`sort`) and the page. Its snapshot and deltas carry an
 *  `EvalItemsResponse`, every field of it sent whole, since its rows carry no id. A
 *  project-timeline view names the project whose timeline rows it shows, its window, and the rest
 *  of what `GET /api/v1/projects/{key}/timeline` would be sent as its `query`: the search, the
 *  facets and `limit`, the rows it holds from the newest, 1 up to the window's total (never
 *  `offset`). Its snapshot and deltas carry a `ProjectTimelineResponse`, its rows sent by id; a
 *  page grows it by subscribing the same `sub` again with a higher `limit` and a resume, and is
 *  sent only the rows it lacks. */
export type DeliveryLiveView =
  | { readonly kind: "timeline"; readonly window: DeliveryLiveWindow; readonly query: string }
  | { readonly kind: "measures"; readonly window: DeliveryLiveWindow; readonly query: string }
  | { readonly kind: "pipeline"; readonly at?: string }
  | { readonly kind: "legion"; readonly project: string }
  | { readonly kind: "legion_issue"; readonly issue: string }
  | {
      readonly kind: "evals";
      readonly project: string;
      readonly window: DeliveryLiveWindow;
      readonly query: string;
    }
  | {
      readonly kind: "items";
      readonly project: string;
      readonly slug: string;
      readonly window: DeliveryLiveWindow;
      readonly query: string;
    }
  | {
      readonly kind: "project-timeline";
      readonly project: string;
      readonly window: DeliveryLiveWindow;
      readonly query: string;
    };

/** Each list a view sends item by item, by id, as its body holds it: the timeline's pull
 *  requests, runs and session groups, an evals view's evals, by eval id, and a project-timeline
 *  view's rows, by a merge's pull request id or a run's id, and its session groups. */
interface DeliveryLiveLists {
  readonly prs: NonNullable<DeliveryTimelineWithMeasures["prs"]>;
  readonly runs: NonNullable<DeliveryTimelineWithMeasures["runs"]>;
  readonly session_groups: NonNullable<DeliveryTimelineWithMeasures["session_groups"]>;
  readonly items:
    | NonNullable<EvalListResponse["items"]>
    | NonNullable<ProjectTimelineResponse["items"]>;
}

/** The lists sent item by item, by id. */
export type DeliveryLiveList = keyof DeliveryLiveLists;
/** The maps of a timeline and of a project-timeline view, sent entry by entry, by key. */
export type DeliveryLiveMap =
  | "issue_titles"
  | "session_titles"
  | "session_pr_counts"
  | "components";
/** The two-level count maps of a timeline and of a project-timeline view, sent count by count:
 *  `{facet: {value: count}}`. */
export type DeliveryLiveCounts = "facet_counts" | "color_counts";

/** What a page holds, as revisions (16 hex digits each) in the frame's own sections. */
export interface DeliveryLiveRevs {
  readonly items: Readonly<Partial<Record<DeliveryLiveList, Readonly<Record<string, string>>>>>;
  readonly maps: Readonly<Partial<Record<DeliveryLiveMap, Readonly<Record<string, string>>>>>;
  readonly counts: Readonly<
    Partial<Record<DeliveryLiveCounts, Readonly<Record<string, Readonly<Record<string, string>>>>>>
  >;
  /** Every field sent whole, by its name. */
  readonly fields: Readonly<Record<string, string>>;
}
/** A delta's revision changes, in DeliveryLiveRevs' shape; null for a key that left. */
export interface DeliveryLiveRevChanges {
  readonly items?: Readonly<
    Partial<Record<DeliveryLiveList, Readonly<Record<string, string | null>>>>
  >;
  readonly maps?: Readonly<
    Partial<Record<DeliveryLiveMap, Readonly<Record<string, string | null>>>>
  >;
  readonly counts?: Readonly<
    Partial<
      Record<DeliveryLiveCounts, Readonly<Record<string, Readonly<Record<string, string | null>>>>>
    >
  >;
  readonly fields?: Readonly<Record<string, string>>;
}

/** A delivery view's resume: the page's last applied change and what it holds; Dispatch answers
 *  the delta. */
export interface DeliveryLiveResume {
  readonly seq: string;
  readonly revs: DeliveryLiveRevs;
}

/** The Dispatch event feed, event by event in id order. */
export interface EventsLiveView {
  readonly kind: "events";
}
/** One session's agent stream: whether it is responding, its replay, and each frame it publishes. */
export interface AgentLiveView {
  readonly kind: "agent";
  /** The session's id, a UUID. */
  readonly session_id: string;
}
/** One view a live socket subscription shows. */
export type LiveView = DeliveryLiveView | EventsLiveView | AgentLiveView;

/** An events view's resume: the decimal id of the last event the page applied. */
export interface EventsLiveResume {
  readonly since: string;
}

/** A subscribe frame for a view of type `V`, whose resume is an `R`. */
interface LiveSubscribeTo<V extends LiveView, R> {
  readonly type: "subscribe";
  /** The page's own id for this subscription, echoed on every frame for it. */
  readonly sub: string;
  readonly view: V;
  /** A reconnecting page's resume point, of its view's kind. */
  readonly resume?: R;
}
/** Starts the subscription `sub` names. Its resume is its view's kind's: a `DeliveryLiveResume`
 *  for a delivery view, an `EventsLiveResume` for the events view, and none for an agent view,
 *  which starts from the session's replay. Dispatch refuses any other pairing with an
 *  `INVALID_FRAME` error frame, so the type admits none. */
export type LiveSubscribe =
  | LiveSubscribeTo<DeliveryLiveView, DeliveryLiveResume>
  | LiveSubscribeTo<EventsLiveView, EventsLiveResume>
  | LiveSubscribeTo<AgentLiveView, never>;
export interface LiveUnsubscribe {
  readonly type: "unsubscribe";
  readonly sub: string;
}
export type LiveClientFrame = LiveSubscribe | LiveUnsubscribe;

/** The body a delivery-hub snapshot or delta carries, by the view's kind: the matching GET
 *  route's body, byte for byte. */
export type DeliveryLiveBody =
  | DeliveryTimelineWithMeasures
  | DeliveryMeasuresResponse
  | DeliveryPipelineResponse
  | LegionBoardResponse
  | LegionIssueResponse
  | EvalListResponse
  | EvalItemsResponse
  | ProjectTimelineResponse;

export interface DeliveryLiveSnapshot {
  readonly type: "snapshot";
  readonly sub: string;
  /** The change log position this answer reflects; a decimal string, since it is a bigint. */
  readonly seq: string;
  /** When the newest change it reflects was recorded; null when the log is empty. */
  readonly changed_at: string | null;
  /** Exactly the matching GET route's body for the resolved window. */
  readonly body: DeliveryLiveBody;
  readonly revs: DeliveryLiveRevs;
}
/** One list's changes: its items that are new or changed, whole, and the ids of those that left. */
export interface DeliveryLiveItemChanges<L extends DeliveryLiveList> {
  readonly upsert: DeliveryLiveLists[L];
  readonly remove: readonly string[];
}
export interface DeliveryLiveDelta {
  readonly type: "delta";
  readonly sub: string;
  readonly seq: string;
  readonly changed_at: string | null;
  /** Fields replaced whole. A timeline's or measures view's `window` is here whenever it moved,
   *  and never alone. */
  readonly set: Readonly<Record<string, unknown>>;
  readonly items: { readonly [L in DeliveryLiveList]?: DeliveryLiveItemChanges<L> };
  /** A key's new value, or null when it left. */
  readonly maps: {
    readonly [M in DeliveryLiveMap]?: Readonly<
      Record<string, NonNullable<DeliveryTimelineWithMeasures[M]>[string] | null>
    >;
  };
  /** A count's new value, or null when it left. */
  readonly counts: Readonly<
    Partial<
      Record<DeliveryLiveCounts, Readonly<Record<string, Readonly<Record<string, number | null>>>>>
    >
  >;
  readonly revs: DeliveryLiveRevChanges;
}
/** Sent to every subscriber when the freshness row's verdict changes, and to a subscription right
 *  after its first answer, so a page that was not subscribed at a change still learns it; never
 *  otherwise. */
export interface DeliveryLiveFreshness {
  readonly type: "freshness";
  readonly freshness: DeliveryTimelineWithMeasures["freshness"];
  /** The sources past their threshold: "prs", "runs", "ci", "events". */
  readonly stale: readonly string[];
}
/** Sent once at the start of every events subscription, before any `event`: the id it starts
 *  after, which is the page's `since` clamped to the feed's head, or the head when the page sent
 *  none. A page holding no cursor adopts it, so a page that has applied no event still resumes
 *  without a gap. */
export interface LiveEventsStart {
  readonly type: "events_start";
  readonly sub: string;
  /** A decimal event id. */
  readonly since: string;
  /** True when the page resumed from an id at or below the feed's head: the subscription replays
   *  every event after it before it goes live, so the page missed nothing and owes no refresh for
   *  the gap. False with no resume, and for a resume above the head (an id of a log since
   *  replaced), which start at the head and replay nothing the page missed. */
  readonly replay_complete: boolean;
}
/** One event of the feed. */
export interface LiveEventFrame {
  readonly type: "event";
  readonly sub: string;
  /** The event's decimal id, which an `EventsLiveResume` names as `since`. */
  readonly id: string;
  readonly event: DispatchEvent;
}
interface LiveAgentEvent<E extends string, D> {
  readonly type: "agent";
  readonly sub: string;
  readonly event: E;
  readonly data: D;
}
/** One message of a session's agent stream; `event` names what `data` is. The responder verdict
 *  is Dispatch's own. A replay's or a frame's data is the session's own JSON, which Dispatch
 *  relays and never parses, so it is `unknown` here: meant to be an `AgentStreamReplay` or an
 *  `AgentStreamFrame`, it is whatever the session published, and a page checks it before it
 *  renders it. */
export type LiveAgentFrame =
  | LiveAgentEvent<"responder", AgentStreamResponder>
  | LiveAgentEvent<"replay", unknown>
  | LiveAgentEvent<"frame", unknown>;
/** Dispatch ended this one subscription: it fell behind (`behind`), or the relay dropped the
 *  viewer (`dropped`). The page subscribes it again at once. */
export interface LiveEnded {
  readonly type: "ended";
  readonly sub: string;
  readonly reason: "behind" | "dropped";
}
/** A refusal: of the subscription `sub` names, or, without `sub`, of a frame Dispatch could not
 *  read. */
export interface LiveError {
  readonly type: "error";
  readonly sub?: string;
  readonly code: string;
  readonly message: string;
}
/** Sent every heartbeat interval beside the socket's own ping, carrying nothing: a browser never
 *  shows a ping to the page, so this is how the page tells a quiet socket from a dead one. */
export interface LiveHeartbeat {
  readonly type: "heartbeat";
}
export type LiveServerFrame =
  | DeliveryLiveSnapshot
  | DeliveryLiveDelta
  | DeliveryLiveFreshness
  | LiveEventsStart
  | LiveEventFrame
  | LiveAgentFrame
  | LiveEnded
  | LiveError
  | LiveHeartbeat;
