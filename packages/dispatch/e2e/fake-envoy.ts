import { type ExpectsReply, LEGION_ROLES, type LegionRole } from "@legion/contracts";
import { connect, type KV, StringCodec } from "nats";
import type { TmuxPlace } from "../web/src/api/types";
import { harnessPorts } from "./harness-ports";

interface FakeSession {
  readonly session_id: string;
  readonly title: string;
  readonly dir?: string;
  readonly machine_id?: string;
  // The session's real machine, as its own Envoy plugin reports it; the real listener falls
  // back to machine_id when this is absent (an older plugin), which this fixture mirrors below.
  readonly host?: string;
  /** The pane this session sits in, as its own plugin reported it with its registration; the
   *  listener answers it unchanged, which `asSeen` mirrors by spreading the seeded session
   *  whole. Absent for a session in no pane. */
  readonly tmux_place?: TmuxPlace;
  readonly roles?: readonly string[];
  readonly capabilities?: readonly string[];
  readonly last_seen?: number;
}

interface SentMessage {
  readonly target_session: string;
  readonly source: string;
  readonly message: string;
  readonly payload: string;
  readonly idempotency_key: string;
  readonly urgency: string;
  readonly expects_reply: ExpectsReply;
  /** The sender asked for a session that is running now: a stopped target is refused rather
   *  than stored for. Dispatch leaves it out of every send but the live view's model and thinking
   *  switch. */
  readonly connected_only?: boolean;
}

/** A persisted interest row, served as seeded. `updated_at` is the row's own last write;
 *  `last_seen` is the session's last heartbeat as the listener still knows it, which a listener
 *  that restarted since the session ended sends no value for. */
interface FakeInterest {
  session_id: string;
  topics: string[];
  updated_at?: number;
  last_seen?: number;
}

/** What this fixture's session store holds under an id, which is a separate question from
 *  whether the listener's in-memory list currently names it. The real listener answers
 *  `GET /v1/sessions` out of that list alone and `GET /v1/registry/<id>` by reading the session
 *  bucket through, so the two disagree while the cache refills: `present` with the id dropped
 *  from the live set is exactly that window, and only `absent` is a session that has really
 *  ended. `unreadable` is the bucket read that did not answer at all. */
type SessionRecord = "present" | "absent" | "unreadable";

/** The state the listener's own record puts a session in, which it judges from that record's
 *  last registration and carries on every row and per-session answer: `live` within the live
 *  window, `unresponsive` past it and inside the gone horizon. A `gone` session is one no
 *  answer names at all, which this fixture models with `setSessionRecord(id, "absent")`, so it
 *  is not a state a seeded session can be put in. */
type SessionState = "live" | "unresponsive";

const SESSION_STATES: readonly SessionState[] = ["live", "unresponsive"];

/** How the listener answers a send to a session. `200` takes it. `404` is its "no live session"
 *  refusal: Dispatch then publishes a targeted message on the session's agent subject itself and
 *  records the attempt queued, while a mention's attempt fails. `503` is the listener behind its
 *  starting gate (`service starting`, packages/envoy/cmd/listener/starting_gate.go startingGate):
 *  Dispatch tries it again through its take-over window and then records the attempt failed, which
 *  is the failure a row stages to read a failed card and its Retry. */
type SendStatus = 200 | 404 | 503;

const SEND_STATUSES: readonly SendStatus[] = [200, 404, 503];

/** What a send's answer says of its target's connection, in the `state` it carries beside
 *  `recipient_state` (packages/envoy/cmd/listener/api.go, sendHandler): `connected` is a session
 *  whose plugin holds its relay subject, `stopped` one the listener knows and that is not
 *  running - an entry whose probe found no plugin, or no entry and a known-session record. A
 *  message sent to a stopped session is stored for its own consumer, not refused. It is never a
 *  `SessionState`: what the record's age says and whether a plugin is running are two questions.
 *  A per-session read that asks for `stopped` answers a session known only by its record with
 *  `state: "stopped"` too. */
type ConnectionState = "connected" | "stopped";

/** The last heartbeat a stopped session's known-session record carries when its seed sets none.
 *  Fixed, because nothing about a stopped session is live: the record is what the listener kept
 *  after it ended. */
const STOPPED_LAST_SEEN = 1762000000000;

/** How the listener answers its two list reads, `GET /v1/sessions` and `GET /v1/interests/`.
 *  `ok` answers them from the fixture. `refilling` is the real listener's answer while its copy
 *  of a bucket cannot be served (packages/envoy/cmd/listener writeListRefilling): 503,
 *  `Retry-After: 5` and code `refilling`, which Dispatch reports as ENVOY_REFILLING. `down` is a
 *  listener that could not answer the read at all: a 503 that carries no code, which Dispatch
 *  reports as ENVOY_UNAVAILABLE - a plain status without the code is never the refilling answer.
 *  Every other route answers as it does in `ok`. */
type ListsState = "ok" | "refilling" | "down";

const LISTS_STATES: readonly ListsState[] = ["ok", "refilling", "down"];

/** The listener's list answer for a state other than `ok`. */
function listRefusal(lists: Exclude<ListsState, "ok">): Response {
  if (lists === "refilling") {
    return Response.json(
      { code: "refilling", error: "envoy: the agent list is refilling; try again in a moment" },
      { headers: { "Retry-After": "5" }, status: 503 }
    );
  }
  return Response.json(
    { error: "envoy: the message bus did not answer: nats: no responders available" },
    { status: 503 }
  );
}

/** One Legion agent registry entry as the daemon writes it (`packages/envoy/internal/legionagents`
 *  `Entry`). `recorded_at` is the listener's own stamp, defaulted to `started_at` when a seed
 *  leaves it out. */
interface FakeLegionAgent {
  readonly session_id: string;
  readonly legion_project: string;
  readonly issue: string;
  readonly role: LegionRole;
  readonly role_token: string;
  readonly title: string;
  readonly harness: "omp" | "claude";
  readonly started_at: string;
  readonly recorded_at?: string;
}

/** How this listener answers the three Legion agent registry reads, which is a question of its
 *  own rather than part of `lists`: the registry is a bucket behind its own routes, and the real
 *  listener serves every other route while it alone is unavailable
 *  (`packages/envoy/cmd/listener/legion_agents.go`, `legionAgentRegistry`). Refusing the list
 *  reads instead would take the sessions and the interests down with it, so nothing could show
 *  what Dispatch does with a registry it cannot read while the rest of the listener answers.
 *  `ok` answers from the seeded entries. `unopened` is the listener whose bucket never opened,
 *  the missing NATS grant being the usual cause: a 503 naming that, with no `Retry-After`.
 *  `unreadable` is a read the open bucket did not answer: a 503 with `Retry-After: 1`
 *  (`writeLegionAgentError`). Dispatch answers `legion_registry: "unavailable"` for either. */
type RegistryState = "ok" | "unopened" | "unreadable";

const REGISTRY_STATES: readonly RegistryState[] = ["ok", "unopened", "unreadable"];

/** The listener's registry answer for a state other than `ok`, as `listRefusal` is for the list
 *  reads. */
function registryRefusal(registry: Exclude<RegistryState, "ok">): Response {
  if (registry === "unopened") {
    return Response.json(
      {
        error:
          "envoy: the legion agent registry is unavailable: could not open the legion_agents bucket (a missing grant is the usual cause): nats: permissions violation for JetStream API",
      },
      { status: 503 }
    );
  }
  return Response.json(
    {
      error: "envoy: the legion agent registry did not answer: nats: no responders available",
    },
    { headers: { "Retry-After": "1" }, status: 503 }
  );
}

/** Every piece of fixture state this listener holds. `PUT /__fixture/reset` replaces it whole, so
 *  a field added here is cleared with the rest without being listed anywhere else; the session
 *  reseed replaces it whole too, so a field that is to outlive a reseed - the Legion agent
 *  registry is one - is carried over by name in that handler. A handler reads its request body
 *  before it touches `state`: a write through a `state` read before that await lands in an object
 *  a reseed may already have dropped. */
interface FakeEnvoyState {
  readonly sessions: FakeSession[];
  readonly liveSessions: Set<string>;
  /** What the session store holds per id; an id named by no entry here is `present`, the state
   *  a seeded session starts in. */
  readonly records: Map<string, SessionRecord>;
  /** The state the listener's record puts each session in; an id named by no entry here is
   *  `live`, the state a seeded session starts in. */
  readonly states: Map<string, SessionState>;
  /** The ids the listener knows only by a known-session record: it holds no registry entry for
   *  any of them, so a per-session read with `stopped` in its `include=` list answers the record,
   *  every other read answers the 404, and a send is stored for it and answered `stopped`. */
  readonly stoppedSessions: Set<string>;
  /** What a send to a session with a registry entry answers of its connection, for the one case
   *  a registry read cannot show: an entry still in place when Dispatch resolved it whose process
   *  had gone by the send, which the listener's probe finds. An id named by no entry here is
   *  `connected`. */
  readonly sendStates: Map<string, ConnectionState>;
  readonly sendStatuses: Map<string, SendStatus>;
  /** How long a send to a session is held before it is answered, in milliseconds, as a listener
   *  slow to take it would; an id named by no entry here is answered at once. */
  readonly sendDelays: Map<string, number>;
  interests: FakeInterest[];
  /** The Legion agent registry's entries, which outlive a session reseed as the real bucket
   *  outlives a session ending. */
  legionAgents: FakeLegionAgent[];
  /** How the three registry reads answer, which a session reseed keeps as it keeps the entries. */
  legionRegistry: RegistryState;
  readonly sentMessages: SentMessage[];
  /** (idempotency key, recipient) pairs this stand-in has already published, the stream's own
   *  duplicate window for the life of the fixture. Cleared with the session seed. */
  readonly publishedKeys: Set<string>;
  readonly unsubscribeCalls: unknown[];
  lists: ListsState;
}

function emptyState(): FakeEnvoyState {
  return {
    interests: [],
    legionAgents: [],
    legionRegistry: "ok",
    lists: "ok",
    liveSessions: new Set(),
    publishedKeys: new Set(),
    records: new Map(),
    sendDelays: new Map(),
    sendStates: new Map(),
    sendStatuses: new Map(),
    sentMessages: [],
    sessions: [],
    states: new Map(),
    stoppedSessions: new Set(),
    unsubscribeCalls: [],
  };
}

let state = emptyState();

/** A row in the real registry always carries a recent `last_seen` (the heartbeat). A seed that
 *  omits it means "live now", so it is stamped on every read rather than once at seeding: a
 *  session seeded into a long-lived harness (one started by hand and reused with
 *  `DISPATCH_E2E_REUSE_SERVERS=1`) would otherwise fall under `Quiet` ten minutes later. Only a
 *  seed that sets `last_seen` can be stale. One read stamps every row with the same `now`, so
 *  sessions seeded together tie, as the server's sorts by `last_seen` expect of one read. */
function asSeen(session: FakeSession, now: number): FakeSession {
  return { last_seen: now, ...session, host: session.host ?? session.machine_id };
}

/** A session as an answer carries it: seen, and with the state the listener judged it to be in,
 *  which every row and every per-session answer of the real listener names. */
function asAnswered(session: FakeSession, now: number): FakeSession & { state: SessionState } {
  return { ...asSeen(session, now), state: stateOf(session.session_id) };
}

function stateOf(sessionID: string): SessionState {
  return state.states.get(sessionID) ?? "live";
}

/** Whether this listener names a session as running. The live set is the listener's in-memory
 *  cache, which a test drops a session from to model that cache refilling; a session whose record
 *  is `absent` has really ended, and one the test stopped is known only by its known-session
 *  record, and then no answer may name it as running however the live set stands. An
 *  unresponsive session is named too: it has missed its heartbeats, it has not ended, and the
 *  listener still holds its record (dispatch://INFRA-392). */
function isLive(sessionID: string): boolean {
  return (
    state.liveSessions.has(sessionID) &&
    state.records.get(sessionID) !== "absent" &&
    !state.stoppedSessions.has(sessionID)
  );
}

/** What `GET /v1/registry/<id>?include=unresponsive,stopped` answers for a session the listener
 *  knows only by its known-session record: that record's own fields and nothing it does not hold.
 *  It carries no port and no host, not even empty ones, because a session that is not running has
 *  no address to be reached at. */
function stoppedBody(session: FakeSession): object {
  return {
    capabilities: session.capabilities ?? [],
    last_seen: session.last_seen ?? STOPPED_LAST_SEEN,
    session_id: session.session_id,
    state: "stopped",
    title: session.title,
    topics: [],
  };
}

/** What a send to this session answers of its connection: `stopped` for a session known only by
 *  its record, else what the test said of its entry, else `connected`. */
function sendConnection(sessionID: string): ConnectionState {
  if (state.stoppedSessions.has(sessionID)) return "stopped";
  return state.sendStates.get(sessionID) ?? "connected";
}

/** Whether the `include` selector asks for `asked`, as the real listener reads it
 *  (`cmd/listener/api.go` includeUnresponsive): a comma-separated list, and an unknown value is
 *  ignored rather than refused. Both session routes and the role lookup take `unresponsive`, and
 *  without it an unresponsive session is answered for by none of them, as a caller that never
 *  heard of the state would expect. Only the per-session route takes `stopped`, and without it a
 *  session known only by its record is the 404 it always was: Dispatch asks for it on a
 *  delivery's read alone. */
function includes(url: URL, asked: "unresponsive" | "stopped"): boolean {
  return (url.searchParams.get("include") ?? "").split(",").includes(asked);
}

/** What a Legion agent registry read answers as `live`: the session's own registration state, as
 *  `sessionIsLive` judges it through `session.StateOf`, and never the row's membership of this
 *  listener's session cache, which is what `isLive` above models for the session routes. So a
 *  seeded session this listener's record calls `unresponsive` reads `live: false` here while it
 *  is not gone, and a session the listener holds no record of at all - ended with
 *  `setSessionRecord(id, "absent")`, or never seeded - is never live, nor is one it knows only by
 *  its known-session record (`setSessionStopped`), which holds no registration. */
function registrationIsLive(sessionID: string): boolean {
  if (state.records.get(sessionID) === "absent" || state.stoppedSessions.has(sessionID)) {
    return false;
  }
  if (!state.sessions.some((session) => session.session_id === sessionID)) return false;
  return stateOf(sessionID) === "live";
}

/** One role token's sessions oldest first, by start, then by when they were recorded, then by
 *  id - the listener's `oldestFirst`. */
function oldestFirst(left: FakeLegionAgent, right: FakeLegionAgent): number {
  return (
    Date.parse(left.started_at) - Date.parse(right.started_at) ||
    (left.recorded_at ?? left.started_at).localeCompare(right.recorded_at ?? right.started_at) ||
    left.session_id.localeCompare(right.session_id)
  );
}

/** Where a role token sits in the listener's order: its role in workflow order, then the token.
 *  A role no `LEGION_ROLES` names, and a token with no session, sort last rather than ahead of
 *  every real role, which is where `indexOf`'s -1 would put them. */
function tokenOrder(sessions: readonly FakeLegionAgent[]): [number, string] {
  const first = sessions[0];
  if (first === undefined) return [LEGION_ROLES.length, ""];
  const place = LEGION_ROLES.indexOf(first.role);
  return [place === -1 ? LEGION_ROLES.length : place, first.role_token];
}

/** A registry read's answer for `agents`, holding the selected entries: each with `live` from the
 *  session's own registration (`registrationIsLive`) and, within its role token ordered by start,
 *  the session that replaced it and when (`replaced_by`/`replaced_at`, null for the newest), as
 *  the listener derives them at the read (`listenerwire.LegionAgentAnswer`,
 *  `legionagents.succession`). Newest first within a role token; across tokens by role in workflow
 *  order, then by token.
 *
 *  Succession is derived over every entry of each token the registry holds and the selection is
 *  applied after, so the three reads never answer one session two ways: an issue read of a token
 *  whose entries were recorded under two issues would otherwise call its newest entry unreplaced
 *  while the token's own read named its replacement. */
function legionAnswer(selected: readonly FakeLegionAgent[]): Record<string, unknown>[] {
  const chosen = new Set(selected.map((agent) => agent.session_id));
  const tokens = new Set(selected.map((agent) => agent.role_token));
  const byToken = new Map<string, FakeLegionAgent[]>();
  for (const agent of state.legionAgents) {
    if (!tokens.has(agent.role_token)) continue;
    const token = byToken.get(agent.role_token);
    if (token === undefined) byToken.set(agent.role_token, [agent]);
    else token.push(agent);
  }
  return [...byToken.values()]
    .sort((left, right) => {
      const [leftRole, leftToken] = tokenOrder(left);
      const [rightRole, rightToken] = tokenOrder(right);
      return leftRole - rightRole || leftToken.localeCompare(rightToken);
    })
    .flatMap((sessions) => {
      const ordered = [...sessions].sort(oldestFirst);
      return ordered
        .flatMap((agent, index) => {
          if (!chosen.has(agent.session_id)) return [];
          const next = ordered[index + 1];
          return [
            {
              ...agent,
              live: registrationIsLive(agent.session_id),
              recorded_at: agent.recorded_at ?? agent.started_at,
              replaced_at: next?.started_at ?? null,
              replaced_by: next?.session_id ?? null,
            },
          ];
        })
        .reverse();
    });
}

/** The live holder of `role`, as this listener's role lookup names it: the first seeded session
 *  that is running and lists the role. */
function liveHolder(role: string): FakeSession | undefined {
  return state.sessions.find(
    (session) => state.liveSessions.has(session.session_id) && session.roles?.includes(role)
  );
}

/** The bus the harness's Dispatch server publishes through: DISPATCH_E2E_NATS_URLS, which
 *  e2e/run-server.sh hands that server as its NATS_URLS, and which this listener inherits from the
 *  harness that starts both. Empty for a run with no bus. */
const busServers = (process.env.DISPATCH_E2E_NATS_URLS ?? "")
  .split(",")
  .map((server) => server.trim())
  .filter((server) => server !== "");

/** The two buckets of the real listener's that Dispatch's outbox reads itself rather than asking
 *  this listener (packages/envoy/internal/roles, Buckets): the role claims, keyed by role, and the
 *  session registrations, keyed by session id (packages/envoy/internal/bus, RoleClaimBucket and
 *  SessionBucket). The outbox resolves a role route's holder from them, never from
 *  `GET /v1/roles/{role}`, so a role this listener names a holder for is still read unclaimed
 *  there until its claim is written on the bus too. Opened, and created where the bus holds
 *  neither, on the first claim a test asks for. */
let busBuckets: Promise<{ roles: KV; sessions: KV }> | undefined;

function roleBuckets(): Promise<{ roles: KV; sessions: KV }> {
  busBuckets ??= (async () => {
    const js = (await connect({ servers: busServers })).jetstream();
    return {
      roles: await js.views.kv("envoy_roles"),
      sessions: await js.views.kv("envoy_sessions"),
    };
  })();
  return busBuckets;
}

const codec = StringCodec();

/** The session a role's claim on the bus names, "" for a role the bus holds no claim of or a
 *  claim that names no holder. */
async function claimedHolder(roles: KV, role: string): Promise<string> {
  const entry = await roles.get(role);
  if (entry === null || entry.operation !== "PUT") return "";
  const claim: unknown = JSON.parse(codec.decode(entry.value));
  return typeof claim === "object" &&
    claim !== null &&
    "holder_session_id" in claim &&
    typeof claim.holder_session_id === "string"
    ? claim.holder_session_id
    : "";
}

// FAKE_ENVOY_PORT, or with it unset port 0, so the OS chooses; the line at the end reports the
// port bound, which e2e/harness-start.ts reads.
const server = Bun.serve({
  hostname: "127.0.0.1",
  port: harnessPorts().fakeEnvoy.port,
  async fetch(request) {
    const url = new URL(request.url);
    if (request.method === "GET" && url.pathname === "/v1/sessions") {
      if (state.lists !== "ok") return listRefusal(state.lists);
      const now = Date.now();
      const unresponsive = includes(url, "unresponsive");
      return Response.json(
        state.sessions
          .filter(
            (session) =>
              isLive(session.session_id) && (unresponsive || stateOf(session.session_id) === "live")
          )
          .map((session) => asAnswered(session, now))
      );
    }
    if (request.method === "GET" && url.pathname.startsWith("/v1/roles/")) {
      const role = decodeURIComponent(url.pathname.slice("/v1/roles/".length));
      const holder = liveHolder(role);
      if (holder === undefined)
        return Response.json({ error: `no holder for role ${role}` }, { status: 404 });
      const holderState = stateOf(holder.session_id);
      // An unresponsive holder keeps the role: the lookup refuses to answer with it unless the
      // caller asked for that state, and it releases nothing either way, as the real listener's
      // holder_lapsed refusal says (packages/envoy/cmd/listener/api.go).
      if (holderState === "unresponsive" && !includes(url, "unresponsive")) {
        return Response.json(
          {
            claim_released: false,
            code: "holder_lapsed",
            error: `role ${role} is held by a session that is not live`,
            holder: holder.session_id,
            holder_state: holderState,
            last_seen: asSeen(holder, Date.now()).last_seen,
          },
          { status: 404 }
        );
      }
      return Response.json({
        ...asAnswered(holder, Date.now()),
        capabilities: holder.capabilities ?? [],
        holder: holder.session_id,
        holder_state: holderState,
        role,
        roles: holder.roles ?? [],
      });
    }
    // The real listener's per-session read goes through to the session bucket rather than to the
    // cache behind `GET /v1/sessions` (packages/envoy/cmd/listener/api.go, sessionEntryHandler),
    // so a session the cache has dropped is still answered here - and answering it warms the
    // cache, which is why a hit adds the id back to the live set: the send that follows the read
    // then finds it, as it does against the real listener. A session known only by its
    // known-session record is the one hit that does not: it is answered only to a read with
    // `stopped` in its `include=` list, it is not running, and nothing a read does starts it.
    if (request.method === "GET" && url.pathname.startsWith("/v1/registry/")) {
      const sessionID = decodeURIComponent(url.pathname.slice("/v1/registry/".length));
      const record = state.records.get(sessionID) ?? "present";
      if (record === "unreadable") {
        return Response.json(
          { error: "envoy: the message bus did not answer: nats: no responders available" },
          { headers: { "Retry-After": "1" }, status: 503 }
        );
      }
      const session = state.sessions.find((item) => item.session_id === sessionID);
      const stopped = state.stoppedSessions.has(sessionID);
      if (record === "absent" || session === undefined || (stopped && !includes(url, "stopped"))) {
        return Response.json({ error: `no live session ${sessionID}` }, { status: 404 });
      }
      if (stopped) return Response.json(stoppedBody(session));
      const answered = asAnswered(session, Date.now());
      // A session past the live window is the 404 it always was unless the caller asked for it
      // with include=unresponsive, and that refusal keeps the "no live session <id>" prefix
      // Dispatch classifies on, with the state and the heartbeat added beside it.
      if (answered.state === "unresponsive" && !includes(url, "unresponsive")) {
        return Response.json(
          {
            error: `no live session ${sessionID}`,
            last_seen: answered.last_seen,
            state: answered.state,
          },
          { status: 404 }
        );
      }
      state.liveSessions.add(sessionID);
      return Response.json(answered);
    }
    // The Legion agent registry's three reads (packages/envoy/cmd/listener/api.go,
    // legionAgentsHandler, legionAgentRoleHandler and legionAgentSessionHandler). A registry
    // state other than `ok` refuses all three, after the read's own argument check, which is
    // where the real handlers reach their registry (`serving`).
    if (request.method === "GET" && url.pathname === "/v1/legion/agents") {
      const issue = url.searchParams.get("issue");
      if (issue === null || issue === "") {
        return Response.json({ error: "issue is required", field: "issue" }, { status: 400 });
      }
      if (state.legionRegistry !== "ok") return registryRefusal(state.legionRegistry);
      return Response.json({
        entries: legionAnswer(state.legionAgents.filter((agent) => agent.issue === issue)),
      });
    }
    if (request.method === "GET" && url.pathname.startsWith("/v1/legion/agents/roles/")) {
      const token = decodeURIComponent(url.pathname.slice("/v1/legion/agents/roles/".length));
      if (state.legionRegistry !== "ok") return registryRefusal(state.legionRegistry);
      return Response.json({
        entries: legionAnswer(state.legionAgents.filter((agent) => agent.role_token === token)),
      });
    }
    if (request.method === "GET" && url.pathname.startsWith("/v1/legion/agents/sessions/")) {
      const sessionID = decodeURIComponent(
        url.pathname.slice("/v1/legion/agents/sessions/".length)
      );
      if (state.legionRegistry !== "ok") return registryRefusal(state.legionRegistry);
      const agent = state.legionAgents.find((item) => item.session_id === sessionID);
      if (agent === undefined) {
        return Response.json({ error: `unknown legion agent ${sessionID}` }, { status: 404 });
      }
      const [answer] = legionAnswer([agent]);
      return Response.json(answer);
    }
    if (request.method === "POST" && url.pathname === "/v1/messages/send") {
      const message = (await request.json()) as SentMessage;
      state.sentMessages.push(message);
      const delay = state.sendDelays.get(message.target_session) ?? 0;
      if (delay > 0) await Bun.sleep(delay);
      const sendStatus = state.sendStatuses.get(message.target_session);
      // A listener behind its starting gate answers every send so, whatever the session.
      if (sendStatus === 503) {
        return Response.json({ error: "service starting" }, { status: 503 });
      }
      const seeded = state.sessions.find(
        (session) => session.session_id === message.target_session
      );
      const known = state.stoppedSessions.has(message.target_session);
      if (
        sendStatus === 404 ||
        seeded === undefined ||
        (!known && !isLive(message.target_session))
      ) {
        return Response.json(
          { error: `no live session ${message.target_session}` },
          { status: 404 }
        );
      }
      const connection = sendConnection(message.target_session);
      // A send that asked for a running session is refused rather than stored, which is what
      // the live view's model and thinking switch asks for: a line that applied at an unknown
      // later time is worse than none.
      if (connection === "stopped" && message.connected_only === true) {
        return Response.json({ error: "the session is stopped" }, { status: 409 });
      }
      // The real listener publishes under a JetStream MsgId built from the idempotency key and
      // the recipient, so a repeat of a key the stream already holds is suppressed and answered
      // `duplicate`. Modelling that here is what lets an e2e see the duplicate wording.
      const streamKey = `${message.idempotency_key}@${message.target_session}`;
      const duplicate = state.publishedKeys.has(streamKey);
      state.publishedKeys.add(streamKey);
      // recipient_state is what the recipient's record said when the send was accepted: a
      // session that has missed its heartbeats is still sent to, and its message waits. One known
      // only by its known-session record has no registry entry, so it reads `gone`. `state` is its
      // connection beside that: a stopped session's message is stored for it and handed over when
      // it runs again, which Dispatch records as a queued attempt, and `last_seen` is then the
      // heartbeat of whichever the answer came from, the record or the entry.
      return Response.json({
        event_id: `envelope-${state.sentMessages.length}`,
        recipient: message.target_session,
        recipient_state: known ? "gone" : stateOf(message.target_session),
        state: connection,
        ...(duplicate ? { duplicate: true } : {}),
        ...(connection === "stopped"
          ? {
              last_seen: known
                ? (seeded.last_seen ?? STOPPED_LAST_SEEN)
                : asSeen(seeded, Date.now()).last_seen,
            }
          : {}),
      });
    }
    if (request.method === "PUT" && url.pathname === "/__fixture/reset") {
      state = emptyState();
      return Response.json({ ok: true });
    }
    // A reseed starts the sessions over (their live set, send statuses, sends and duplicate
    // window) and keeps the persisted subscriptions, the list answer and the Legion agent
    // registry - its entries and whether it can be read at all - which a test sets on their own.
    if (request.method === "PUT" && url.pathname === "/__fixture/sessions") {
      const sessions = (await request.json()) as FakeSession[];
      const ids = sessions.map((session) => session.session_id);
      state = {
        ...emptyState(),
        interests: state.interests,
        legionAgents: state.legionAgents,
        legionRegistry: state.legionRegistry,
        lists: state.lists,
        liveSessions: new Set(ids),
        sendStatuses: new Map(ids.map((id) => [id, 200] as const)),
        sessions,
        unsubscribeCalls: state.unsubscribeCalls,
      };
      return Response.json({ ok: true });
    }
    // The claim of a role on the bus, as the real listener writes it when a session claims the
    // role (packages/envoy/internal/store, RoleClaim): the holder is the live holder this
    // listener's own lookup names, whose registration is written beside it, live from now, so
    // Dispatch's outbox forwards a role route to that session and records it as the role's
    // holder. A second claim of the same role, once a test has moved the lookup to another
    // holder, names the first as previous_session_id. A role with no live holder is 404, and a
    // listener started with no bus (DISPATCH_E2E_NATS_URLS unset) is 409, writing nothing.
    if (request.method === "PUT" && url.pathname.startsWith("/__fixture/role-claims/")) {
      const role = decodeURIComponent(url.pathname.slice("/__fixture/role-claims/".length));
      const holder = liveHolder(role);
      if (holder === undefined) {
        return Response.json({ error: `no live holder for role ${role}` }, { status: 404 });
      }
      if (busServers.length === 0) {
        return Response.json(
          { error: "this listener has no bus: DISPATCH_E2E_NATS_URLS is unset" },
          { status: 409 }
        );
      }
      const { roles, sessions } = await roleBuckets();
      const previous = await claimedHolder(roles, role);
      const now = Date.now();
      await sessions.put(
        holder.session_id,
        codec.encode(
          JSON.stringify({
            capabilities: holder.capabilities ?? [],
            dir: holder.dir ?? "",
            host: holder.host ?? holder.machine_id,
            machine_id: holder.machine_id ?? "",
            port: 0,
            title: holder.title,
            updated_at: now,
          })
        )
      );
      await roles.put(
        role,
        codec.encode(
          JSON.stringify({
            claimed_at: now,
            holder_session_id: holder.session_id,
            previous_session_id: previous,
            renewed_at: now,
          })
        )
      );
      return Response.json({ holder: holder.session_id, previous, role });
    }
    // The release of a role's claim on the bus and of the registration of the session it named,
    // which a test that claimed a role makes before it ends: the claims are the bus's and outlive
    // this listener's fixture, so neither a reseed nor a reset removes them.
    if (request.method === "DELETE" && url.pathname.startsWith("/__fixture/role-claims/")) {
      const role = decodeURIComponent(url.pathname.slice("/__fixture/role-claims/".length));
      if (busServers.length === 0) {
        return Response.json(
          { error: "this listener has no bus: DISPATCH_E2E_NATS_URLS is unset" },
          { status: 409 }
        );
      }
      const { roles, sessions } = await roleBuckets();
      const released = await claimedHolder(roles, role);
      if (released !== "") {
        await roles.delete(role);
        await sessions.delete(released);
      }
      return Response.json({ released, role });
    }
    // The registry's entries, replaced whole; the daemon is the real bucket's only writer.
    if (request.method === "PUT" && url.pathname === "/__fixture/legion-agents") {
      const agents = (await request.json()) as FakeLegionAgent[];
      state.legionAgents = agents;
      return Response.json({ ok: true });
    }
    // Whether the registry can be read at all, the question the entries are behind: the three
    // registry reads answer the refusal while this is not `ok`, and every other route serves as
    // the real listener's do when its bucket is the only one it has lost.
    if (request.method === "PUT" && url.pathname === "/__fixture/legion-registry") {
      const body = (await request.json()) as { state?: RegistryState };
      if (body.state === undefined || !REGISTRY_STATES.includes(body.state)) {
        return Response.json(
          { error: "state must be ok, unopened or unreadable" },
          { status: 400 }
        );
      }
      state.legionRegistry = body.state;
      return Response.json({ state: state.legionRegistry });
    }
    if (request.method === "PUT" && url.pathname === "/__fixture/lists") {
      const body = (await request.json()) as { state?: ListsState };
      if (body.state === undefined || !LISTS_STATES.includes(body.state)) {
        return Response.json({ error: "state must be ok, refilling or down" }, { status: 400 });
      }
      state.lists = body.state;
      return Response.json({ state: state.lists });
    }
    if (request.method === "PATCH" && url.pathname.startsWith("/__fixture/sessions/")) {
      const sessionID = decodeURIComponent(url.pathname.slice("/__fixture/sessions/".length));
      const body = (await request.json()) as {
        live?: boolean;
        record?: SessionRecord;
        send_delay_ms?: number;
        send_state?: ConnectionState;
        send_status?: SendStatus;
        state?: SessionState;
        stopped?: boolean;
        last_seen?: number;
      };
      if (!state.sessions.some((session) => session.session_id === sessionID)) {
        return Response.json({ error: `unknown fixture session ${sessionID}` }, { status: 404 });
      }
      if (body.live !== undefined) {
        if (body.live) state.liveSessions.add(sessionID);
        else state.liveSessions.delete(sessionID);
      }
      if (body.send_status !== undefined) {
        if (!SEND_STATUSES.includes(body.send_status)) {
          return Response.json({ error: "send_status must be 200, 404 or 503" }, { status: 400 });
        }
        state.sendStatuses.set(sessionID, body.send_status);
      }
      if (body.send_delay_ms !== undefined) {
        if (!Number.isInteger(body.send_delay_ms) || body.send_delay_ms < 0) {
          return Response.json(
            { error: "send_delay_ms must be a whole number of milliseconds, 0 or more" },
            { status: 400 }
          );
        }
        state.sendDelays.set(sessionID, body.send_delay_ms);
      }
      if (body.record !== undefined) {
        if (!["absent", "present", "unreadable"].includes(body.record)) {
          return Response.json(
            { error: "record must be present, absent or unreadable" },
            { status: 400 }
          );
        }
        state.records.set(sessionID, body.record);
        // An ended session is gone from the cache as well as from the store, so a test that
        // asks for the genuine absence gets both at once rather than having to say it twice.
        if (body.record === "absent") state.liveSessions.delete(sessionID);
      }
      if (body.stopped !== undefined) {
        // A session that is not running is out of the listener's in-memory list as well as
        // out of its registry, so a test that stops one gets both at once.
        if (body.stopped) {
          state.stoppedSessions.add(sessionID);
          state.liveSessions.delete(sessionID);
        } else {
          state.stoppedSessions.delete(sessionID);
          state.liveSessions.add(sessionID);
        }
      }
      if (body.send_state !== undefined) {
        if (body.send_state !== "connected" && body.send_state !== "stopped") {
          return Response.json(
            { error: "send_state must be connected or stopped" },
            { status: 400 }
          );
        }
        state.sendStates.set(sessionID, body.send_state);
      }
      if (body.state !== undefined) {
        if (!SESSION_STATES.includes(body.state)) {
          return Response.json({ error: "state must be live or unresponsive" }, { status: 400 });
        }
        state.states.set(sessionID, body.state);
      }
      if (body.last_seen !== undefined) {
        // The heartbeat an answer reports, which is what a reader of an unresponsive session
        // dates its silence by and what Dispatch adds its claim grace to. Writing it on the
        // seeded row keeps `asSeen` from stamping the read's own time over it.
        if (!Number.isInteger(body.last_seen) || body.last_seen <= 0) {
          return Response.json(
            { error: "last_seen must be a positive whole number of epoch milliseconds" },
            { status: 400 }
          );
        }
        const index = state.sessions.findIndex((session) => session.session_id === sessionID);
        const seeded = state.sessions[index];
        if (seeded !== undefined) {
          state.sessions[index] = { ...seeded, last_seen: body.last_seen };
        }
      }
      return Response.json({
        last_seen: state.sessions.find((session) => session.session_id === sessionID)?.last_seen,
        live: state.liveSessions.has(sessionID),
        record: state.records.get(sessionID) ?? "present",
        send_delay_ms: state.sendDelays.get(sessionID) ?? 0,
        send_state: sendConnection(sessionID),
        send_status: state.sendStatuses.get(sessionID) ?? 200,
        session_id: sessionID,
        state: stateOf(sessionID),
        stopped: state.stoppedSessions.has(sessionID),
      });
    }
    if (request.method === "GET" && url.pathname === "/__fixture/sends") {
      return Response.json(state.sentMessages);
    }
    if (request.method === "GET" && url.pathname === "/v1/interests/") {
      if (state.lists !== "ok") return listRefusal(state.lists);
      return Response.json(state.interests);
    }
    if (request.method === "PUT" && url.pathname === "/__fixture/interests") {
      const interests = (await request.json()) as FakeInterest[];
      state.interests = interests;
      return Response.json({ ok: true });
    }
    // The real Dispatch server (packages/envoy/internal/dispatch/envoy/client.go's
    // Interest method) looks up one session's persisted topics before unsubscribing
    // it, so this must resolve even though only the bare list is seeded directly.
    if (request.method === "GET" && url.pathname.startsWith("/v1/interests/")) {
      const sessionId = decodeURIComponent(url.pathname.slice("/v1/interests/".length));
      const interest = state.interests.find((item) => item.session_id === sessionId);
      if (interest === undefined) {
        return new Response("not found", { status: 404 });
      }
      return Response.json(interest);
    }
    if (request.method === "POST" && url.pathname === "/v1/interests/unsubscribe") {
      const body = (await request.json()) as { session_id: string; topics: string[] };
      state.unsubscribeCalls.push(body);
      const interest = state.interests.find((item) => item.session_id === body.session_id);
      if (interest !== undefined) {
        interest.topics = interest.topics.filter((topic) => !body.topics.includes(topic));
      }
      return Response.json({ removed: body.topics });
    }
    if (request.method === "GET" && url.pathname === "/__fixture/unsubscribe-calls") {
      return Response.json(state.unsubscribeCalls);
    }
    if (url.pathname === "/healthz") return Response.json({ ok: true });
    return new Response("not found", { status: 404 });
  },
});

console.log(`fake envoy listener on 127.0.0.1:${server.port}`);
