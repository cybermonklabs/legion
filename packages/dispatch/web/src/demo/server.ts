// The views demo's stubbed Dispatch (packages/dispatch/e2e/views-demo/README.md): every `/api/`
// and `/auth/` request the page makes, answered in the browser from the fixtures as the real
// server would answer it, the listing's filters included. A write changes this in-memory state,
// which the tab keeps in sessionStorage across a reload, and goes out on the event feed as the
// server's own event would; a write that the daemon would see moves the board at its next read,
// which `DAEMON_READ_MS` after the write stands in for.
import type {
  DispatchEvent,
  Issue,
  IssueSummary,
  LegionBoardResponse,
  UserIssueState,
} from "@legion/contracts";
import {
  type BoardPopulation,
  computeBoard,
  PhaseDone,
  rankOrder,
} from "../../../e2e/views-demo/board-rules";
import type { DemoFixtures } from "./fixtures";

/** The answer of one request: its status and its JSON body. */
export interface DemoAnswer {
  status: number;
  body: unknown;
}

/** How long after a write the daemon's next read moves its cards. */
const DAEMON_READ_MS = 4_000;
const KEPT_STATE = "dispatch-demo.state";
/** The listing's status order (model.IssueStatuses). */
const STATUS_ORDER = [
  "triage",
  "icebox",
  "backlog",
  "todo",
  "in_progress",
  "testing",
  "needs_review",
  "retro",
  "done",
];
const RANK_ALPHABET = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz";

/** What the tab keeps across a reload: every issue a write changed, the reader's issue state,
 *  and the daemon's state as the simulated reads left it. */
interface KeptState {
  issues: Record<string, IssueSummary>;
  userState: Record<string, UserIssueState>;
  population: BoardPopulation | null;
  eventId: number;
}

type Listener = () => void;

export class DemoServer {
  readonly fixtures: DemoFixtures;
  private readonly issues = new Map<string, IssueSummary>();
  private readonly changed = new Set<string>();
  private userState: Record<string, UserIssueState>;
  private population: BoardPopulation;
  private populationMoved = false;
  private board: LegionBoardResponse | null = null;
  private eventId: number;
  private readonly eventListeners = new Set<(id: string, event: DispatchEvent) => void>();
  private readonly boardListeners = new Set<Listener>();
  private daemonRead: ReturnType<typeof setTimeout> | undefined;
  private readonly pendingDaemon = new Map<string, { status?: string; rank?: boolean }>();
  /** Every request no route of the stub answered, for the screenshot run to read. */
  readonly unanswered: string[] = [];

  constructor(fixtures: DemoFixtures) {
    this.fixtures = fixtures;
    for (const issue of fixtures.issues) this.issues.set(issue.key, issue);
    this.userState = { ...fixtures.reader.user_state };
    this.population = fixtures.population;
    this.eventId = 1_000_000;
    const kept = readKept();
    if (kept !== null) {
      for (const [key, issue] of Object.entries(kept.issues)) {
        this.issues.set(key, issue);
        this.changed.add(key);
      }
      this.userState = kept.userState;
      if (kept.population !== null) {
        this.population = kept.population;
        this.populationMoved = true;
      }
      this.eventId = kept.eventId;
    }
  }

  get login(): string {
    return this.fixtures.reader.login;
  }

  get project(): string {
    return this.fixtures.manifest.project;
  }

  eventHead(): string {
    return String(this.eventId);
  }

  onEvent(listener: (id: string, event: DispatchEvent) => void): () => void {
    this.eventListeners.add(listener);
    return () => this.eventListeners.delete(listener);
  }

  onBoard(listener: Listener): () => void {
    this.boardListeners.add(listener);
    return () => this.boardListeners.delete(listener);
  }

  /** The board route's body, computed from the daemon's state and the issues as they stand. */
  legionBoard(): LegionBoardResponse {
    this.board ??= computeBoard(
      this.population,
      [...this.issues.values()].filter((issue) => issue.key.startsWith(`${this.project}-`)),
      goTime(new Date())
    );
    return this.board;
  }

  hasLegion(project: string): boolean {
    return this.fixtures.reader.legion_settings.some((setting) => setting.project === project);
  }

  /** The answer to one request, or `undefined` where no route of the stub answers it. */
  handle(method: string, url: URL, body: unknown): DemoAnswer | undefined {
    for (const route of this.routes) {
      if (route.method !== method) continue;
      const match = route.pattern.exec(url.pathname);
      if (match !== null)
        return route.answer(match.slice(1).map(decodeURIComponent), url.searchParams, body);
    }
    return undefined;
  }

  /** Every route the stub answers: its method, its path, and its answer from the path's
   *  captures, the query and the body. */
  private readonly routes: {
    method: string;
    pattern: RegExp;
    answer: (captures: string[], q: URLSearchParams, body: unknown) => DemoAnswer;
  }[] = [
    {
      method: "GET",
      pattern: /^\/auth\/whoami$/,
      answer: () => ok({ kind: "user", login: this.login, event_head: this.eventHead() }),
    },
    {
      method: "GET",
      pattern: /^\/api\/v1\/whoami$/,
      answer: () =>
        ok({
          kind: "user",
          login: this.login,
          scopes: ["person", "agent"],
          session: null,
          project: null,
        }),
    },
    { method: "GET", pattern: /^\/api\/v1\/issues$/, answer: (_, q) => ok(this.listIssues(q)) },
    {
      method: "GET",
      pattern: /^\/api\/v1\/issues\/([^/]+)$/,
      answer: ([key]) => {
        const issue = this.issues.get(key);
        return issue === undefined ? notFound("ISSUE_NOT_FOUND") : ok(this.fullIssue(issue));
      },
    },
    {
      method: "PATCH",
      pattern: /^\/api\/v1\/issues\/([^/]+)$/,
      answer: ([key], _, body) => this.patchIssue(key, body as IssuePatch),
    },
    // An issue's conversation, which no production read of the demo holds: none.
    { method: "GET", pattern: /^\/api\/v1\/issues\/[^/]+\/comments$/, answer: () => ok([]) },
    { method: "GET", pattern: /^\/api\/v1\/issues\/[^/]+\/asks$/, answer: () => ok([]) },
    { method: "GET", pattern: /^\/api\/v1\/projects$/, answer: () => ok(this.fixtures.projects) },
    {
      method: "GET",
      pattern: /^\/api\/v1\/projects\/([^/]+)$/,
      answer: ([key]) => {
        const project = this.fixtures.projects.find((one) => one.key === key);
        return project === undefined ? notFound("PROJECT_NOT_FOUND") : ok(project);
      },
    },
    {
      method: "GET",
      pattern: /^\/api\/v1\/projects\/([^/]+)\/legion\/board$/,
      answer: ([key]) => (this.hasLegion(key) ? ok(this.legionBoard()) : legionNotConfigured()),
    },
    // No project of the demo has an architecture source, which the list answers as [].
    {
      method: "GET",
      pattern: /^\/api\/v1\/projects\/[^/]+\/architecture-sources$/,
      answer: () => ok([]),
    },
    {
      method: "GET",
      pattern: /^\/api\/v1\/settings\/legion$/,
      answer: () => ok(this.fixtures.reader.legion_settings),
    },
    { method: "GET", pattern: /^\/api\/v1\/me\/state$/, answer: () => ok(this.userState) },
    {
      method: "PUT",
      pattern: /^\/api\/v1\/me\/issues\/([^/]+)\/state$/,
      answer: ([key], _, body) => this.putIssueState(key, (body ?? {}) as Partial<UserIssueState>),
    },
    {
      method: "GET",
      pattern: /^\/api\/v1\/schema\/blocks$/,
      answer: () => ok(this.fixtures.schema),
    },
    { method: "GET", pattern: /^\/api\/v1\/agents$/, answer: () => ok(this.fixtures.agents) },
    // The reader's own inbox, replies and agents: nothing waits on them in the demo.
    { method: "GET", pattern: /^\/api\/v1\/inbox$/, answer: () => ok([]) },
    {
      method: "GET",
      pattern: /^\/api\/v1\/me\/replies$/,
      answer: (_, q) =>
        ok({
          rows: [],
          total: 0,
          limit: Number(q.get("limit") ?? 50),
          offset: 0,
          as_of: goTime(new Date()),
        }),
    },
    { method: "GET", pattern: /^\/api\/v1\/me\/agents\/state$/, answer: () => ok({}) },
    // No secrets broker: the pending list answers null, as Dispatch with none does.
    { method: "GET", pattern: /^\/api\/v1\/credential-requests$/, answer: () => ok(null) },
    { method: "POST", pattern: /^\/auth\/logout$/, answer: () => ({ status: 204, body: null }) },
  ];

  private putIssueState(key: string, input: Partial<UserIssueState>): DemoAnswer {
    const held = this.userState[key] ?? { dismissed: [], last_read_seq: 0, pinned: false, seq: 0 };
    const next: UserIssueState = {
      dismissed: input.dismissed ?? held.dismissed,
      last_read_seq: input.last_read_seq ?? held.last_read_seq,
      pinned: input.pinned ?? held.pinned,
      seq: held.seq + 1,
    };
    this.userState = { ...this.userState, [key]: next };
    this.keep();
    return ok(next);
  }

  /** Records a request no route answered. */
  missed(method: string, url: URL): void {
    const entry = `${method} ${url.pathname}${url.search}`;
    this.unanswered.push(entry);
    console.warn(`[views demo] no stub route answers ${entry}`);
  }

  /** `GET /api/v1/issues`, with every filter the server's listing statement applies. */
  private listIssues(q: URLSearchParams): IssueSummary[] | object {
    const project = q.get("project")?.trim() ?? "";
    const status = q.get("status")?.trim() ?? "";
    const parent = q.get("parent")?.trim() ?? "";
    const labels = q.getAll("label").map((label) => label.trim().toLowerCase());
    const updatedSince = q.has("updated_since") ? q.get("updated_since") : null;
    const closedSince = q.has("closed_since") ? time(q.get("closed_since")) : null;
    const closedBefore = q.has("closed_before") ? time(q.get("closed_before")) : null;
    const priorities = q
      .getAll("priority")
      .filter((value) => value !== "none")
      .map(Number);
    const unsetPriority = q.getAll("priority").includes("none");
    const routeStatus = q.get("route_status") ?? "";
    const open = q.get("open") === "true" || routeStatus !== "";
    const pinned = q.get("pinned") === "true";
    const rows = [...this.issues.values()].filter((issue) => {
      if (project !== "" && !issue.key.startsWith(`${project}-`)) return false;
      if (status !== "" && issue.status !== status) return false;
      if (parent !== "" && issue.parent !== parent) return false;
      if (updatedSince !== null && time(issue.updated_at) < time(updatedSince)) return false;
      if (labels.length > 0) {
        const held = new Set((issue.labels ?? []).map((label) => label.toLowerCase()));
        if (!labels.every((label) => held.has(label))) return false;
      }
      if (open && (issue.closed_at !== null || issue.status === "done")) return false;
      if (priorities.length > 0 || unsetPriority) {
        const priority = issue.priority;
        if (!(priority === null ? unsetPriority : priorities.includes(priority))) return false;
      }
      if (closedSince !== null && (issue.closed_at === null || time(issue.closed_at) < closedSince))
        return false;
      if (
        closedBefore !== null &&
        (issue.closed_at === null || time(issue.closed_at) >= closedBefore)
      )
        return false;
      if (routeStatus !== "" && issue.route_status !== routeStatus) return false;
      if (pinned && this.userState[issue.key]?.pinned !== true) return false;
      return true;
    });
    rows.sort(
      (a, b) => STATUS_ORDER.indexOf(a.status) - STATUS_ORDER.indexOf(b.status) || rankOrder(a, b)
    );
    if (!q.has("limit") && !q.has("offset")) return rows;
    const limit = Number(q.get("limit") ?? 50);
    const offset = Number(q.get("offset") ?? 0);
    return { issues: rows.slice(offset, offset + limit), total: rows.length, limit, offset };
  }

  private fullIssue(issue: IssueSummary): Issue {
    const {
      open_asks: _asks,
      progress: _progress,
      route_holder: _holder,
      route_status: _status,
      route_status_reason: _reason,
      ...rest
    } = issue;
    const [project, number] = issue.key.split("-");
    return {
      ...rest,
      project,
      number: Number(number),
      external_links: [],
      verification_artifact_id: "",
      plan_artifact_id: "",
      decisions_artifact_id: "",
    };
  }

  private patchIssue(key: string, input: IssuePatch): DemoAnswer {
    const held = this.issues.get(key);
    if (held === undefined) return notFound("ISSUE_NOT_FOUND");
    const now = goTime(new Date());
    const next: IssueSummary = { ...held };
    const changed: string[] = [];
    if (input.status !== undefined && input.status !== held.status) {
      next.status = input.status;
      changed.push("status");
      if (input.status === "done") next.closed_at = now;
      else if (held.status === "done") next.closed_at = null;
      if ((input.status === "done") !== (held.status === "done")) changed.push("closed_at");
    }
    if (input.rank !== undefined) {
      next.rank = this.rankFor(key, input.rank);
      changed.push("rank");
    }
    for (const field of ["priority", "assignee", "title", "labels", "parent"] as const) {
      if (input[field] !== undefined) {
        (next as Record<string, unknown>)[field] = input[field];
        changed.push(field);
      }
    }
    next.updated_at = now;
    next.last_seq = held.last_seq + 1;
    this.issues.set(key, next);
    this.changed.add(key);
    this.board = null;
    this.keep();
    this.publish(next, changed);
    if (Object.hasOwn(this.population.issues, key)) {
      this.pendingDaemon.set(key, {
        ...this.pendingDaemon.get(key),
        ...(changed.includes("status") ? { status: next.status } : {}),
        ...(changed.includes("rank") ? { rank: true } : {}),
      });
      clearTimeout(this.daemonRead);
      this.daemonRead = setTimeout(() => this.readDaemon(), DAEMON_READ_MS);
    }
    for (const listener of this.boardListeners) listener();
    return ok(this.fullIssue(next));
  }

  /** The rank a `{before, after}` input places the issue at (api/issue_rank.go). */
  private rankFor(key: string, input: { before?: string | null; after?: string | null }): string {
    const ranks = [...this.issues.values()]
      .filter((issue) => issue.key !== key && issue.key.split("-")[0] === key.split("-")[0])
      .map((issue) => issue.rank)
      .sort();
    let previous = input.after ? (this.issues.get(input.after)?.rank ?? "") : "";
    let next = input.before ? (this.issues.get(input.before)?.rank ?? "") : "";
    if (previous !== "") {
      next = ranks.find((rank) => rank > previous && (next === "" || rank < next)) ?? next;
    } else if (next !== "") {
      previous = ranks.filter((rank) => rank < next).at(-1) ?? "";
    } else {
      previous = ranks.at(-1) ?? "";
    }
    return rankBetween(previous, next);
  }

  /** The daemon's next read after a write: a closed issue leaves its card and its slot, and a
   *  root the line names takes the place its new rank gives it among the line's roots. */
  private readDaemon(): void {
    const population = structuredClone(this.population);
    const admission = population.admission;
    for (const [key, change] of this.pendingDaemon) {
      const observed = population.issues[key];
      if (change.status === "done" && observed !== undefined) {
        observed.phase = PhaseDone;
        observed.slot = null;
        observed.roles = [];
        if (admission !== null) {
          admission.active = admission.active?.filter((one) => one !== key) ?? null;
          admission.waiting = admission.waiting?.filter((one) => one !== key) ?? null;
        }
      }
      const waiting = admission?.waiting;
      if (change.rank && waiting?.includes(key)) {
        const rank = this.issues.get(key)?.rank ?? "";
        const rest = waiting.filter((one) => one !== key);
        const at = rest.findIndex((one) => (this.issues.get(one)?.rank ?? "") > rank);
        rest.splice(at === -1 ? rest.length : at, 0, key);
        if (admission !== null) admission.waiting = rest;
      }
    }
    this.pendingDaemon.clear();
    if (population.read !== null) population.read = { ...population.read, at: goTime(new Date()) };
    this.population = population;
    this.populationMoved = true;
    this.board = null;
    this.keep();
    for (const listener of this.boardListeners) listener();
  }

  private publish(issue: IssueSummary, changed: string[]): void {
    this.eventId += 1;
    const full = this.fullIssue(issue);
    const ancestors: string[] = [];
    for (let parent = issue.parent; parent !== null && parent !== undefined; ) {
      ancestors.push(parent);
      parent = this.issues.get(parent)?.parent ?? null;
      if (ancestors.length > 20) break;
    }
    const event = {
      id: this.eventId,
      issue_key: issue.key,
      project: full.project,
      seq: issue.last_seq,
      type: "issue.updated",
      actor: { kind: "user", id: this.login },
      notify: true,
      created_at: issue.updated_at,
      payload: {
        ...full,
        changed,
        ancestors,
        ...(changed.includes("status") ? { thread_logins: [] } : {}),
      },
    } as unknown as DispatchEvent;
    for (const listener of this.eventListeners) listener(String(this.eventId), event);
  }

  private keep(): void {
    const kept: KeptState = {
      issues: Object.fromEntries(
        [...this.changed].flatMap((key) => {
          const issue = this.issues.get(key);
          return issue === undefined ? [] : [[key, issue] as const];
        })
      ),
      userState: this.userState,
      population: this.populationMoved ? this.population : null,
      eventId: this.eventId,
    };
    try {
      sessionStorage.setItem(KEPT_STATE, JSON.stringify(kept));
    } catch {
      // A tab that cannot keep it starts from the fixtures again on a reload.
    }
  }
}

interface IssuePatch {
  status?: string;
  rank?: { before?: string | null; after?: string | null };
  priority?: number | null;
  assignee?: string | null;
  title?: string;
  labels?: string[];
  parent?: string | null;
}

function readKept(): KeptState | null {
  try {
    const raw = sessionStorage.getItem(KEPT_STATE);
    return raw === null ? null : (JSON.parse(raw) as KeptState);
  } catch {
    return null;
  }
}

function ok(body: unknown): DemoAnswer {
  return { status: 200, body };
}

function notFound(code: string): DemoAnswer {
  return { status: 404, body: { code, error: `${code.toLowerCase()} (views demo)` } };
}

function legionNotConfigured(): DemoAnswer {
  return {
    status: 404,
    body: { code: "LEGION_NOT_CONFIGURED", error: "this project has no Legion daemon" },
  };
}

/** A time as the server writes it (RFC 3339 with trailing fractional zeros dropped). */
export function goTime(date: Date): string {
  return date.toISOString().replace(/\.?0+Z$/, "Z");
}

function time(value: string | null): number {
  return value === null ? Number.NaN : Date.parse(value);
}

/** rank.Between (internal/dispatch/rank): a base62 key strictly between two others. */
export function rankBetween(previous: string, next: string): string {
  const digit = (char: string) => RANK_ALPHABET.indexOf(char);
  const after = (prev: string): string => {
    for (let i = 0; i < prev.length; i++) {
      const d = digit(prev[i]);
      if (d < RANK_ALPHABET.length - 1) return prev.slice(0, i) + RANK_ALPHABET[d + 1];
    }
    return `${prev}U`;
  };
  const before = (upper: string): string => {
    if (upper === "") return "U";
    const d = digit(upper[0]);
    if (d > 1) return RANK_ALPHABET[Math.floor(d / 2)];
    return RANK_ALPHABET[0] + before(upper.slice(1));
  };
  if (previous === "") return before(next);
  if (next === "") return after(previous);
  let common = 0;
  while (common < previous.length && common < next.length && previous[common] === next[common]) {
    common++;
  }
  if (common === previous.length) return previous + before(next.slice(common));
  const low = digit(previous[common]);
  const high = digit(next[common]);
  if (high - low > 1)
    return previous.slice(0, common) + RANK_ALPHABET[Math.floor((low + high) / 2)];
  return previous.slice(0, common + 1) + after(previous.slice(common + 1));
}
