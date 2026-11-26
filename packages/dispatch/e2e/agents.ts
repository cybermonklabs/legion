import { DELIVERY_CAPABILITIES, type LegionRole, roleToken } from "@legion/contracts";
import { type BrowserContext, expect, type Locator, type Page } from "@playwright/test";

import type { BroadcastCreated, CreateBroadcastInput, TmuxPlace } from "../web/src/api/types";
import type { AgentGroup } from "../web/src/features/agents/agent-groups";
import {
  createAgentMessage,
  createAsk,
  createComment,
  createIssue,
  createProject,
  replyToMessageDelivery,
} from "./api";
import { harnessPorts } from "./harness-ports";
import { type KeyedWrite, keyedWrite, watchWrites } from "./keyed-writes";
import { sessionIds } from "./session-ids";
import { expandedState } from "./settled";

export interface FakeSession {
  session_id: string;
  title: string;
  dir?: string;
  machine_id?: string;
  // The session's real machine; the real listener falls back to machine_id when the session's
  // plugin predates this field, which the fake Envoy's asSeen mirrors (fake-envoy.ts).
  host?: string;
  /** Where this session sits in tmux, which `GET /api/v1/agents` answers unchanged and the
   *  machine pages draw. Absent is a session in no pane, as it is on the wire. */
  tmux_place?: TmuxPlace;
  roles?: string[];
  capabilities?: string[];
  last_seen?: number;
  port?: number;
  topics?: string[];
}

/** A request to a fake Envoy, by default this process's own stack's. The harness
 *  (e2e/harness-start.ts) starts that listener for local and deployed runs, so fixture state
 *  reaches whichever Dispatch server the suite targets; `port` is for the one process that
 *  addresses every stack's rather than its own (e2e/global-setup.ts). */
async function fixtureRequest(
  path: string,
  method: "GET" | "PATCH" | "PUT",
  body?: object,
  port: number = harnessPorts().fakeEnvoy.port
): Promise<Response> {
  const response = await fetch(`http://127.0.0.1:${port}${path}`, {
    body: body === undefined ? undefined : JSON.stringify(body),
    headers: body === undefined ? undefined : { "Content-Type": "application/json" },
    method,
  });
  if (!response.ok) {
    throw new Error(`${method} ${path} failed: ${response.status} ${await response.text()}`);
  }
  return response;
}

/** Clears all mutable fake Envoy state between e2e rows. */
export async function resetFakeEnvoy(port?: number): Promise<void> {
  await fixtureRequest("/__fixture/reset", "PUT", undefined, port);
}

export async function setLiveSessions(rows: FakeSession[], port?: number): Promise<void> {
  await fixtureRequest("/__fixture/sessions", "PUT", rows, port);
}

/** A live session for the Agents page to list, seen 5 s ago. Its id is a UUID, which Dispatch's
 *  agent routes require. `last_seen` is taken when the row seeds, never at a module's scope,
 *  which a worker evaluates once and every later spec of that worker ages. */
export function listedSession(sessionID: string, title: string): FakeSession {
  return {
    capabilities: ["aside", "btw"],
    dir: `/workspaces/${title.toLowerCase()}`,
    last_seen: Date.now() - 5_000,
    machine_id: `${title.toLowerCase()}-host`,
    roles: [title.toLowerCase()],
    session_id: sessionID,
    title,
  };
}

/** One unread reply of the session `sessionID` to the reader, which is what keeps a row for a
 *  session that has since left the registry. */
export async function seedUnreadReply(sessionID: string): Promise<void> {
  const asked = await createAgentMessage(sessionID, {
    body: "How far along?",
    delivery: "aside",
  });
  await replyToMessageDelivery(
    asked.id,
    { attempt: 1, body: "Done and gone." },
    { id: sessionID, kind: "session" }
  );
}

/** How the fake listener answers a send to a seeded session until a reseed: `200` takes it. `404`
 *  is "no live session": Dispatch then publishes a targeted message on the session's agent
 *  subject itself and records the attempt queued (`listQueuedMessages`), and a mention fails.
 *  `503` is a listener behind its starting gate, and Dispatch records the attempt failed once its
 *  take-over window has passed. A row that reads a failed targeted message and its Retry stages
 *  `503`. */
export async function setSessionSendStatus(
  sessionID: string,
  status: 200 | 404 | 503
): Promise<void> {
  await fixtureRequest(`/__fixture/sessions/${encodeURIComponent(sessionID)}`, "PATCH", {
    send_status: status,
  });
}

/** Holds every send to the session `ms` before the fake Envoy answers it, as a listener slow to
 *  take it would; 0 answers at once again. Dispatch's listener client gives up on an answer at
 *  5 s, so a hold that is to end in a delivery stays under that. */
export async function setSessionSendDelay(sessionID: string, ms: number): Promise<void> {
  await fixtureRequest(`/__fixture/sessions/${encodeURIComponent(sessionID)}`, "PATCH", {
    send_delay_ms: ms,
  });
}

/** What the fake listener's session store holds for an id, the question its per-session route
 *  answers. Only `absent` is a session that has ended. */
export type SessionRecord = "present" | "absent" | "unreadable";

/** Takes a seeded session out of the listener's live list, or puts it back, without reseeding: a
 *  reseed clears every fixture send status with it. This is the refilling-cache state and
 *  nothing more - the list is the listener's in-memory cache, while its per-session route reads
 *  the session store through - so the session still resolves 200 there and is still delivered
 *  to. A row that needs a session to have genuinely ended uses `setSessionRecord(id, "absent")`.
 */
export async function setSessionLive(sessionID: string, live: boolean): Promise<void> {
  await fixtureRequest(`/__fixture/sessions/${encodeURIComponent(sessionID)}`, "PATCH", { live });
}

/** Says what the listener's session store holds for a seeded session: `present` is a record the
 *  per-session route answers 200 from, and answering it puts the id back in the live list as the
 *  real listener's read-through warms its cache; `absent` ends the session with no record at all,
 *  in the registry or the known-session store, which drops it from the live list and makes every
 *  answer about it - the per-session route, the list and a send - report no live session, so
 *  Dispatch queues a targeted message to it itself; `unreadable` is the store read that did not
 *  answer at all, a 503 the caller must retry rather than read as an ended session. */
export async function setSessionRecord(sessionID: string, record: SessionRecord): Promise<void> {
  await fixtureRequest(`/__fixture/sessions/${encodeURIComponent(sessionID)}`, "PATCH", { record });
}

/** Stops a seeded session, or starts it again. A stopped session is one the listener knows only
 *  by its known-session record: it holds no registry entry, so it is out of the live list, a
 *  registry read that does not ask for `stopped` in its `include=` list gets the 404, and a send
 *  to it is stored for it and answered `state: "stopped"` with `recipient_state: "gone"`, which
 *  Dispatch records as a queued attempt. */
export async function setSessionStopped(sessionID: string, stopped: boolean): Promise<void> {
  await fixtureRequest(`/__fixture/sessions/${encodeURIComponent(sessionID)}`, "PATCH", {
    stopped,
  });
}

/** What a send to this session answers its connection was, whatever a registry read of it shows:
 *  `stopped` without stopping it is the session whose registry entry was still in place when
 *  Dispatch resolved it and whose process had gone by the send, which the listener's probe finds
 *  and answers with `recipient_state` still that entry's own. */
export async function setSessionSendState(
  sessionID: string,
  sendState: "connected" | "stopped"
): Promise<void> {
  await fixtureRequest(`/__fixture/sessions/${encodeURIComponent(sessionID)}`, "PATCH", {
    send_state: sendState,
  });
}

/** The state the fake listener's record puts a seeded session in. `gone` is not one of them: a
 *  session the listener holds no record of at all is `setSessionRecord(id, "absent")`. */
export type SessionState = "live" | "unresponsive";

/** Puts a seeded session past the listener's live window, or back inside it, without reseeding.
 *  An `unresponsive` session is still listed, still holds its roles and claims and is still
 *  delivered to; every answer about it carries that state, and `GET /v1/sessions` and the
 *  per-session route answer for it only when the caller asks with `include=unresponsive`, as
 *  Dispatch's own listener client always does (dispatch://INFRA-392). Pair it with
 *  `setSessionLastSeen` wherever the row's age matters: Dispatch dates the silence by that
 *  heartbeat and adds its claim grace to it. */
export async function setSessionState(sessionID: string, state: SessionState): Promise<void> {
  await fixtureRequest(`/__fixture/sessions/${encodeURIComponent(sessionID)}`, "PATCH", { state });
}

/** The heartbeat every answer reports for a seeded session, in epoch milliseconds, in place of
 *  the read's own time: what an unresponsive session's silence is dated from, and what Dispatch
 *  adds `ISSUE_CLAIM_UNRESPONSIVE_GRACE_MS` to for the moment its claim comes free. */
export async function setSessionLastSeen(sessionID: string, lastSeen: number): Promise<void> {
  await fixtureRequest(`/__fixture/sessions/${encodeURIComponent(sessionID)}`, "PATCH", {
    last_seen: lastSeen,
  });
}

/** How the fake listener answers its two list reads (`GET /v1/sessions`, `GET /v1/interests/`):
 *  `ok` from the fixture; `refilling` with the real listener's refilling answer (503,
 *  `Retry-After: 5`, code `refilling`), which Dispatch reports as `ENVOY_REFILLING`; `down` with a
 *  503 that carries no code, which Dispatch reports as `ENVOY_UNAVAILABLE`. Every other route
 *  answers as before, and a session reseed keeps the state; `resetFakeEnvoy` puts it back to
 *  `ok`. */
export type ListsState = "ok" | "refilling" | "down";

/** Sets the fake listener's list answer. `retryAfterSeconds` is the `Retry-After` a refilling
 *  answer carries, the real listener's 5 when it is left out: the page's two retries of a refused
 *  read each wait that long and up to as much again (`queryRetryDelay`,
 *  `web/src/api/query-client.ts`), so a row that waits through several refilling reads sets 1. */
export async function setListsState(
  state: ListsState,
  options: { retryAfterSeconds?: number } = {}
): Promise<void> {
  await fixtureRequest("/__fixture/lists", "PUT", {
    state,
    ...(options.retryAfterSeconds === undefined
      ? {}
      : { retry_after_seconds: options.retryAfterSeconds }),
  });
}

/** Makes every page `context` opens from now on poll the agent list every `pollMs` in place of the
 *  shell's 15 s (`AGENT_REGISTRY_POLL_MS`, read through `agentRegistryPollMs` in
 *  `web/src/features/refs/agent-registry.ts`), so a row that waits for a change of the list to
 *  reach an open page waits one short poll. Set before the page loads, as the poller reads it.
 *  `Reflect.set` rather than the property itself: the `Window` field is declared in that web
 *  module, which the docs site's program, which reads this file for its screenshots, does not
 *  load. */
export async function pollAgentListEvery(context: BrowserContext, pollMs: number): Promise<void> {
  await context.addInitScript((ms) => {
    Reflect.set(window, "__dispatchAgentRegistryPollMs", ms);
  }, pollMs);
}

/** One Legion agent registry entry as the daemon writes it to the listener
 *  (`packages/envoy/internal/legionagents` `Entry`); `role_token` is the claim token of the
 *  session's agent, `legion-<legion_project>-<issue>-<agent>`, and `role` the role of the step that
 *  agent runs, which need not be its token's last part: a token names its agent, not the role of
 *  its step. */
export interface FakeLegionAgent {
  session_id: string;
  legion_project: string;
  issue: string;
  role: LegionRole;
  role_token: string;
  title: string;
  harness: "omp" | "claude";
  started_at: string;
  recorded_at?: string;
}

/** One registry entry as the daemon writes it for the agent named for its role: `omp` harness,
 *  the role's own token under the Legion project (`roleToken`, the claim token of that agent),
 *  and the title the daemon stamps. A spec that seeds the registry builds its rows here so the
 *  `role_token` a read is checked against is always the one the daemon would have written for
 *  that project, issue and agent. */
export function legionAgentEntry({
  issue,
  legionProject,
  role,
  sessionId,
  startedAt,
}: {
  issue: string;
  legionProject: string;
  role: LegionRole;
  sessionId: string;
  startedAt: string;
}): FakeLegionAgent {
  return {
    harness: "omp",
    issue,
    legion_project: legionProject,
    role,
    role_token: roleToken(legionProject, issue, role),
    session_id: sessionId,
    started_at: startedAt,
    title: `Legion ${role} · ${issue}`,
  };
}

/** Replaces the fake listener's Legion agent registry, which the three registry reads answer from
 *  with `live`, `replaced_by` and `replaced_at` derived at the read. A session reseed keeps it. */
export async function setLegionAgents(rows: FakeLegionAgent[]): Promise<void> {
  await fixtureRequest("/__fixture/legion-agents", "PUT", rows);
}

/** How the fake listener answers the three Legion agent registry reads, which is its own
 *  question rather than part of `ListsState`: the real listener serves every other route while
 *  the registry's bucket alone is unavailable (`packages/envoy/cmd/listener/legion_agents.go`),
 *  so a row that refused the list reads instead would take the sessions down with the entries.
 *  `ok` answers from the seeded entries; `unopened` is the bucket that never opened (503, no
 *  `Retry-After`) and `unreadable` a read the open bucket did not answer (503, `Retry-After: 1`).
 *  Dispatch answers either with `legion_registry: "unavailable"`, keeping the issue's people and
 *  the sessions its events name. A session reseed keeps the state; `resetFakeEnvoy` puts it back
 *  to `ok`. */
export type RegistryState = "ok" | "unopened" | "unreadable";

export async function setLegionRegistryState(state: RegistryState): Promise<void> {
  await fixtureRequest("/__fixture/legion-registry", "PUT", { state });
}

export async function getSentMessages(): Promise<Record<string, unknown>[]> {
  return (await fixtureRequest("/__fixture/sends", "GET")).json() as Promise<
    Record<string, unknown>[]
  >;
}

/** Whether a body the page posted is a broadcast's input. */
export function isBroadcastInput(body: unknown): body is CreateBroadcastInput {
  return (
    typeof body === "object" &&
    body !== null &&
    "body" in body &&
    typeof body.body === "string" &&
    "delivery" in body &&
    DELIVERY_CAPABILITIES.some((mode) => mode === body.delivery) &&
    "session_ids" in body &&
    Array.isArray(body.session_ids) &&
    body.session_ids.every((id) => typeof id === "string")
  );
}

/** Every broadcast request the page posts from here on, in order, as the body it sent and the
 *  `Idempotency-Key` it sent it under - a route that fulfils a request itself still lists it. */
export function postedBroadcasts(page: Page): KeyedWrite<CreateBroadcastInput>[] {
  return watchWrites(page, "POST", "/api/v1/broadcasts", isBroadcastInput);
}

/** Every broadcast the page sends from here on, in order: each press, Retry and Restore draft, as
 *  the body and key it sent. A repeat is not a send of its own: Dispatch answers a broadcast
 *  before it delivers, and while that answer still shows a recipient whose one attempt is pending,
 *  the page asks again under the same key (`broadcast-queue.ts`), so the POSTs of one send
 *  outnumber it. A POST under the key of an answer that left a recipient pending is that repeat,
 *  and is not listed; `postedBroadcasts` lists every POST. */
export function broadcastSends(page: Page): KeyedWrite<CreateBroadcastInput>[] {
  const sends: KeyedWrite<CreateBroadcastInput>[] = [];
  const owed = new Set<string | undefined>();
  const isBroadcastPost = (request: { method(): string; url(): string }) =>
    request.method() === "POST" && new URL(request.url()).pathname === "/api/v1/broadcasts";
  page.on("request", (request) => {
    if (!isBroadcastPost(request)) return;
    const send = keyedWrite(request, isBroadcastInput);
    if (!owed.delete(send.key)) sends.push(send);
  });
  page.on("response", async (response) => {
    if (!isBroadcastPost(response.request()) || !response.ok()) return;
    const created = (await response.json()) as BroadcastCreated;
    const pending = (created.recipients ?? []).some(
      ({ message: { deliveries } }) =>
        deliveries?.length === 1 && deliveries[0]?.state === "pending"
    );
    if (pending) owed.add(response.request().headers()["idempotency-key"]);
  });
  return sends;
}

/** The reason `refuseBroadcasts` gives, which a refused row shows after `Could not send to N
 *  agents: `. */
export const BROADCAST_REFUSED = "no selected session can receive this message";

/** The sentence a person reads for an attempt Dispatch records `queued`, for a session named
 *  `name`: the listener stored the message, or Dispatch queued it on the session's agent subject
 *  itself. It is spelt out here rather than imported from the SPA, so a change to the SPA's
 *  wording fails the rows that read it rather than passing with it. */
export const QUEUED = (name: string) =>
  `Queued: ${name} is stopped; it gets the message if it runs again within 72 hours`;

/** Refuses every broadcast POST as Dispatch does one no selected session can receive (400
 *  `BROADCAST_EMPTY`), until `allow()`. A broadcast tries a failed fetch, a 5xx or a 429 again
 *  under its key until Dispatch answers, so a refusal is a 4xx of Dispatch's own. The selected
 *  sessions stay live, so the page's 15 s agent poll cannot empty the list mid-row. */
export async function refuseBroadcasts(page: Page): Promise<{ allow: () => void }> {
  let refusing = true;
  await page.route("**/api/v1/broadcasts", (route) =>
    refusing && route.request().method() === "POST"
      ? route.fulfill({
          body: JSON.stringify({ code: "BROADCAST_EMPTY", error: BROADCAST_REFUSED }),
          contentType: "application/json",
          status: 400,
        })
      : route.fallback()
  );
  return {
    allow: () => {
      refusing = false;
    },
  };
}

/** A persisted interest row to seed, served by the fake listener as given. `updated_at` is the
 *  row's own last write; `last_seen` is the session's last heartbeat as the listener still knows
 *  it, absent when the listener has lost it. */
export interface FakeInterest {
  session_id: string;
  topics: string[];
  updated_at?: number;
  last_seen?: number;
}

export async function setInterests(rows: FakeInterest[]): Promise<void> {
  await fixtureRequest("/__fixture/interests", "PUT", rows);
}

export async function getUnsubscribeCalls(): Promise<{ session_id: string; topics: string[] }[]> {
  return (await fixtureRequest("/__fixture/unsubscribe-calls", "GET")).json() as Promise<
    { session_id: string; topics: string[] }[]
  >;
}

// The Agents page's keyboard rows, in `keyboard-agents.e2e.ts`, `keyboard-palette.e2e.ts` and the
// picker spec the WebKit and Firefox projects also run, share one page: two live sessions, each
// with Dispatch activity of its own, so both sit under `Active` rather than under `Idle` or
// `Quiet`.
// The Planner's open ask waits on the viewer, which puts it under `Needs you`, above the
// Reviewer. Neither carries a `last_seen`: the fake Envoy answers every read of a session seeded
// without one with the current time, so a shared seed never ages into `Quiet`. A time computed
// here would: this module is evaluated once per Playwright worker, at the first spec that
// imports it, and the time would age with every spec the worker runs after that, until the
// sessions fell under `Quiet`.
export const plannerSession: FakeSession = {
  capabilities: ["aside", "btw"],
  dir: "/srv/planner",
  machine_id: "box-1",
  roles: ["planner"],
  session_id: sessionIds.planner,
  title: "Planner",
};
export const reviewerSession: FakeSession = {
  capabilities: ["aside", "btw"],
  dir: "/srv/reviewer",
  machine_id: "box-1",
  roles: ["reviewer"],
  session_id: sessionIds.reviewer,
  title: "Reviewer",
};

/** What a pick of one of those two sessions writes into a draft, and what a channel of its own
 *  seeds there: the mention that names the party (`partyLink`), which is the
 *  whole record of who the write addresses. */
export const plannerMention = `[@${plannerSession.title}](dispatch://agent/${plannerSession.session_id})`;
export const reviewerMention = `[@${reviewerSession.title}](dispatch://agent/${reviewerSession.session_id})`;

/** Both sessions live, both with Dispatch activity, and two open issues for the picker to offer:
 *  a keyboard reader has to be able to pass the first to reach the second. Returns the first. */
export async function seedAgents(): Promise<string> {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Keyboard issue" });
  await createIssue({ project: "CORE", title: "Rollout plan" });
  await createAsk(
    issue.key,
    { question: "Which release?" },
    { actor: { id: plannerSession.session_id, kind: "session" }, as: "agent" }
  );
  await createComment(
    issue.key,
    { body: "Reviewing the diff." },
    { actor: { id: reviewerSession.session_id, kind: "session" }, as: "agent" }
  );
  await setLiveSessions([plannerSession, reviewerSession]);
  return issue.key;
}

/** The keymap binds only once sign-in resolves (`AuthGate` renders a skeleton until
 *  `/auth/whoami` answers), so a key pressed before the page renders reaches no handler. */
export async function openAgents(page: Page): Promise<void> {
  await page.goto("/agents");
  await expect(page.getByRole("heading", { name: "Agents", level: 1 })).toBeVisible();
  await page.locator("body").focus();
}

/** The rows a reader sees: the ones the window drew, less any it keeps mounted and hidden. The
 *  list is windowed, so this counts what is on the page and never what the filters match - that
 *  is each group heading's own count - and a long list draws only part of itself. Counting,
 *  indexing or ordering the page's rows goes through this. */
export function shownAgentRows(page: Page): Locator {
  return page.locator("[data-agent-row]:not([hidden])");
}

/** One session's row, wherever the window has it: drawn, or mounted and hidden because the
 *  reader opened it and a fold or a filter now hides it. A row nobody has opened that the window
 *  has not drawn is in no part of the page, so an assertion says which it expects
 *  (`toBeVisible`, `toBeHidden` with `toHaveCount(1)`, or `toHaveCount(0)`). */
export function agentRow(page: Page, sessionID: string): Locator {
  return page.locator(`[data-agent-row="${sessionID}"]`);
}

/** One of the page's four group headings (INFRA-812, R7), whose accessible name is the group's
 *  name and its count: `Idle (3)`, or `Idle (3, 1 selected)` once a row of it is selected. */
export function agentGroupToggle(page: Page, group: AgentGroup): Locator {
  return page.getByTestId(`agents-group-toggle-${group}`);
}

/** Collapses `group` if it is open, or opens it if it is closed: every group is open when the
 *  page loads, so a row that needs one shut says so. The heading says which it is, and is read
 *  once the page has drawn it rather than at the instant it attaches. */
export async function toggleAgentGroup(page: Page, group: AgentGroup): Promise<void> {
  const toggle = agentGroupToggle(page, group);
  const fold = await expandedState(`the ${group} group's heading`, toggle);
  await toggle.click();
  await expect(toggle).toHaveAttribute("aria-expanded", fold === "open" ? "false" : "true");
}

/** The group, machine or role filter's trigger. Each of the three is a `MultiSelect` (R7), named
 *  by what it filters rather than by what is ticked in it. */
export function agentFilter(page: Page, filter: "group" | "machine" | "role"): Locator {
  return page.getByRole("button", { name: `Filter by ${filter}` });
}

/** Opens a filter's panel if it is shut. Its trigger's `aria-expanded` says which it is, and is
 *  read once the page has drawn the trigger rather than at the instant it attaches. */
export async function openAgentFilter(
  page: Page,
  filter: "group" | "machine" | "role"
): Promise<void> {
  const trigger = agentFilter(page, filter);
  if ((await expandedState(`the ${filter} filter`, trigger)) === "shut") await trigger.click();
}

/** Ticks or unticks one option of a filter and closes its popover, so the portalled panel never
 *  covers the control the row drives next. */
export async function pickAgentFilter(
  page: Page,
  filter: "group" | "machine" | "role",
  option: string
): Promise<void> {
  await openAgentFilter(page, filter);
  await page.getByRole("option", { exact: true, name: option }).click();
  await page.keyboard.press("Escape");
  await expect(page.getByRole("listbox")).toHaveCount(0);
}
