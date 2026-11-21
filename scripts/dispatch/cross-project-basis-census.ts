import { readFile } from "node:fs/promises";
import { parseArgs } from "node:util";
import { PROJECT_KEY_PATTERN } from "../../packages/contracts/src/dispatch-tools";
import type { ActiveDispatchConfig } from "../../packages/envoy-client/src/dispatch-config";
import { DispatchClient } from "../../packages/envoy-client/src/dispatch-http";
import {
  sessionConfigFile,
  sessionDispatchSettings,
} from "../../packages/envoy-client/src/session-settings";
import { fetchIssueEvents, get, ISSUE_CONCURRENCY } from "./ask-census.ts";

/** The first line of the message the daemon posts on a root whose design gate step an ancestor's
 * approval opened: `basisOpenedLine` in `packages/daemon/internal/workflow/gate_basis.go`. */
export const OPENED_LINE = "Design gate opened on an ancestor's approval.";

/** An issue key: a project key, a dash and the issue's number in that project. */
const ISSUE_KEY = new RegExp(`^(${PROJECT_KEY_PATTERN.source.slice(1, -1)})-[1-9][0-9]*$`);

/** The line of `basisOpenedMessage` that names the ancestor whose approval opened the step. Its
 * first spec reference is the root's own, so the ancestor is read from this line alone. */
const ANCESTOR_LINE = /^- The ancestor is (\S+)\.$/;

/** The page the event log is read in: the route's own maximum. */
const EVENT_LOG_PAGE = 1000;

type DispatchConfig = Pick<ActiveDispatchConfig, "url" | "credential">;

/** An event's actor, or a review's. */
interface LogActor {
  readonly kind: string;
  readonly id: string;
}

/**
 * The payload fields the census reads, by event type. A log spans releases, so each is optional
 * and checked before use.
 */
export interface LogPayload {
  /** issue.created, issue.updated and issue.closed carry the issue as the write left it. */
  readonly key?: string;
  readonly parent?: string | null;
  readonly status?: string;
  readonly primary_artifact_id?: string;
  /** The ask an ask.* event carries, or the message a message.created carries. */
  readonly id?: string;
  /** An ask's kind (`approval` for an approval request), or a review's (`person`). */
  readonly kind?: string;
  /** The document an approval request names, at the version it names. */
  readonly approval?: { readonly artifact_id?: string; readonly version?: number };
  readonly answer?: { readonly selected?: readonly string[] } | null;
  readonly resolution?: { readonly kind?: string } | null;
  /** The document of an artifact.version, artifact.approved or artifact.changes_requested. */
  readonly artifact_id?: string;
  /** The version a review is of (a number), or the version an artifact.version wrote. */
  readonly version?: number | { readonly number?: number };
  /** Who gave a review. */
  readonly actor?: LogActor;
  /** The document an artifact.created made, with its versions. */
  readonly artifact?: {
    readonly id?: string;
    readonly versions?: readonly { readonly number?: number }[];
  };
  /** A message's text. */
  readonly body?: string;
}

/** One event as `GET /api/v1/events/log` and `GET /api/v1/issues/{key}/events` serve it, and as a
 * saved log holds it, one to a line. */
export interface LogEvent {
  readonly id: number;
  readonly seq?: number;
  readonly type: string;
  readonly issue_key: string | null;
  readonly created_at: string;
  readonly actor?: LogActor;
  readonly payload: LogPayload;
}

/** An issue as its last issue.created, issue.updated or issue.closed left it. */
export interface IssueFacts {
  readonly parent: string | null;
  readonly status: string;
  readonly primary: string;
}

/** A document's newest review: a person's when `by` is a user. */
export interface ReviewFacts {
  readonly state: "approved" | "changes_requested";
  readonly version: number;
  readonly by: string;
}

/** A document at one moment: its latest version, and its newest review. */
export interface DocumentFacts {
  readonly latestVersion: number;
  readonly review: ReviewFacts | null;
}

/** A person's answer to an ask, as the log's last ask.answered of it holds it. */
export interface AnswerFacts {
  readonly by: string;
  readonly selected: readonly string[];
  readonly at: string;
}

/** How an ask closed without an answer: retracted, resolved, withdrawn or declined. */
export interface ResolutionFacts {
  readonly kind: string;
  readonly at: string;
}

/** What the census reads besides the events it counts, as of a moment: the moment is an event id,
 * and each answer is what the events before it left. */
export interface CensusLookups {
  issueAt(key: string, before: number): IssueFacts | undefined;
  documentAt(artifactId: string, before: number): DocumentFacts;
  closingOf(askId: string): { answer: AnswerFacts | null; resolution: ResolutionFacts | null };
}

/** A spec-approval ask on a root whose chain, read up to its first issue of another project, reaches
 * an issue whose spec held a person's approval of its latest version when the ask opened. */
export interface CrossProjectAsk {
  readonly issue: string;
  readonly ask: string;
  readonly openedAt: string;
  /** The version of the root's own spec the ask named. */
  readonly version: number | null;
  readonly ancestor: string;
  /** The version of the ancestor's spec a person approved, its latest when the ask opened. */
  readonly ancestorVersion: number;
}

/** A message in basisOpenedMessage's format on an issue, naming the ancestor whose approval opened
 * the issue's step. */
export interface GateOpening {
  readonly issue: string;
  readonly message: string;
  readonly createdAt: string;
  readonly ancestor: string;
}

/** A root and the ancestor of another project a reading names it with. */
export interface Pair {
  readonly root: string;
  readonly ancestor: string;
}

/** One spec-approval ask on a pair's root, with the ancestor as it stood when the ask opened. */
export interface PairAsk {
  readonly ask: string;
  readonly openedAt: string;
  readonly version: number | null;
  /** Whether the ancestor was on the root's chain, read up to its first issue of another project. */
  readonly onChain: boolean;
  readonly ancestorLatestVersion: number;
  readonly ancestorReview: ReviewFacts | null;
  readonly answer: AnswerFacts | null;
  readonly resolution: ResolutionFacts | null;
}

/** A pair's root: each of its spec-approval asks, and each opening on it that names the ancestor. */
export interface PairReading extends Pair {
  readonly asks: readonly PairAsk[];
  readonly openings: readonly GateOpening[];
}

/** What one run prints: the same in both output modes. */
export interface CensusReport {
  readonly asks: readonly CrossProjectAsk[];
  readonly openings: readonly GateOpening[];
  readonly pairs: readonly PairReading[];
}

export interface CensusOptions {
  readonly projects: readonly string[];
  readonly from?: string;
  readonly to?: string;
  readonly pairs: readonly Pair[];
}

/** What a run takes: a saved log, whose window `from` and `to` may narrow, or Dispatch's event log
 * over a window both name. */
type RunOptions = CensusOptions & { readonly json: boolean } & (
    | { readonly eventsFile: string }
    | { readonly eventsFile?: undefined; readonly from: string; readonly to: string }
  );

/** An issue on a chain that the lookups hold no issue event of before the moment read. */
class UnreadIssue extends Error {
  constructor(
    readonly key: string,
    before: number
  ) {
    super(`the log holds no issue event of ${key} before event ${before}`);
  }
}

/** The project an issue key names. */
function projectOf(key: string): string {
  const project = ISSUE_KEY.exec(key)?.[1];
  if (project === undefined) throw new Error(`${key} is not an issue key`);
  return project;
}

/** Whether the issue `key` is of another project than `project`: the census's one comparison of
 * projects, for an ask's chain and for an opening's ancestor alike. */
function ofAnotherProject(key: string, project: string): boolean {
  return projectOf(key) !== project;
}

function date(value: string, name: string): number {
  const parsed = Date.parse(value);
  if (Number.isNaN(parsed)) throw new Error(`${name} must be an RFC 3339 time: ${value}`);
  return parsed;
}

/** The last entry before `before` of entries in event order. */
function lastBefore<T extends { readonly id: number }>(
  entries: readonly T[] | undefined,
  before: number
): T | undefined {
  let found: T | undefined;
  for (const entry of entries ?? []) {
    if (entry.id >= before) break;
    found = entry;
  }
  return found;
}

function pushTo<T>(map: Map<string, T[]>, key: string, value: T): void {
  const list = map.get(key);
  if (list === undefined) map.set(key, [value]);
  else list.push(value);
}

/** The lookups the events themselves give: each issue's facts from its issue events, each
 * document's versions and reviews from its artifact events, and each ask's last answer or
 * resolution. A primary spec's first version is made with its issue and has no event of its own,
 * so a document stands at version 1 until an event names a later one. */
export function eventLookups(events: readonly LogEvent[]): CensusLookups {
  const issues = new Map<string, Array<{ id: number; facts: IssueFacts }>>();
  const versions = new Map<string, Array<{ id: number; version: number }>>();
  const reviews = new Map<string, Array<{ id: number; review: ReviewFacts }>>();
  const answers = new Map<string, AnswerFacts>();
  const resolutions = new Map<string, ResolutionFacts>();
  for (const event of [...events].sort((left, right) => left.id - right.id)) {
    const { payload } = event;
    switch (event.type) {
      case "issue.created":
      case "issue.updated":
      case "issue.closed": {
        const key = payload.key ?? event.issue_key;
        if (key === null || key === undefined) break;
        pushTo(issues, key, {
          id: event.id,
          facts: {
            parent: payload.parent ?? null,
            status: payload.status ?? "",
            primary: payload.primary_artifact_id ?? "",
          },
        });
        break;
      }
      case "artifact.created":
        for (const written of payload.artifact?.versions ?? []) {
          if (payload.artifact?.id !== undefined && written.number !== undefined) {
            pushTo(versions, payload.artifact.id, { id: event.id, version: written.number });
          }
        }
        break;
      case "artifact.version":
        if (
          payload.artifact_id !== undefined &&
          typeof payload.version === "object" &&
          payload.version.number !== undefined
        ) {
          pushTo(versions, payload.artifact_id, { id: event.id, version: payload.version.number });
        }
        break;
      case "artifact.approved":
      case "artifact.changes_requested":
        if (payload.artifact_id !== undefined && typeof payload.version === "number") {
          // As the daemon names a decision's author: `<kind>:<id>`.
          const reviewer = payload.actor ?? event.actor;
          pushTo(reviews, payload.artifact_id, {
            id: event.id,
            review: {
              state: event.type === "artifact.approved" ? "approved" : "changes_requested",
              version: payload.version,
              by: reviewer === undefined ? "" : `${reviewer.kind}:${reviewer.id}`,
            },
          });
        }
        break;
      case "ask.answered":
        if (payload.id !== undefined) {
          answers.set(payload.id, {
            by: event.actor === undefined ? "" : `${event.actor.kind}:${event.actor.id}`,
            selected: payload.answer?.selected ?? [],
            at: event.created_at,
          });
        }
        break;
      case "ask.resolved":
      case "ask.declined":
        if (payload.id !== undefined) {
          resolutions.set(payload.id, {
            kind: payload.resolution?.kind ?? event.type,
            at: event.created_at,
          });
        }
        break;
    }
  }
  return {
    issueAt: (key, before) => lastBefore(issues.get(key), before)?.facts,
    documentAt: (artifactId, before) => {
      const review = lastBefore(reviews.get(artifactId), before)?.review ?? null;
      let latestVersion = Math.max(1, review?.version ?? 1);
      for (const written of versions.get(artifactId) ?? []) {
        if (written.id >= before) break;
        latestVersion = Math.max(latestVersion, written.version);
      }
      return { latestVersion, review };
    },
    closingOf: (askId) => ({
      answer: answers.get(askId) ?? null,
      resolution: resolutions.get(askId) ?? null,
    }),
  };
}

function known(lookups: CensusLookups, key: string, before: number): IssueFacts {
  const facts = lookups.issueAt(key, before);
  if (facts === undefined) throw new UnreadIssue(key, before);
  return facts;
}

/** The version of a document a person approved, when the newest review is a person's approval of
 * its latest version; null otherwise. A person is an actor of kind user, as the daemon's basis
 * check reads one. */
function personsApprovalOfLatest(document: DocumentFacts): number | null {
  const { review } = document;
  return review !== null &&
    review.state === "approved" &&
    review.by.startsWith("user:") &&
    review.version === document.latestVersion
    ? review.version
    : null;
}

/** An issue's chain at a moment, read up to its first issue of another project. */
interface Chain {
  /** Each ancestor read, nearest first: the first ancestor of another project last, when one is. */
  readonly ancestors: readonly string[];
  /** The first ancestor of another project, where the read ends; null when the chain has none. */
  readonly other: string | null;
  /** Whether every ancestor of the issue's own project below it is done: a root of its project. */
  readonly root: boolean;
}

function chainAt(key: string, issue: IssueFacts, before: number, lookups: CensusLookups): Chain {
  const project = projectOf(key);
  const ancestors: string[] = [];
  let root = true;
  let next = issue.parent;
  while (next !== null) {
    if (next === key || ancestors.includes(next)) {
      throw new Error(`the chain of ${key} at event ${before} returns to ${next}`);
    }
    ancestors.push(next);
    if (ofAnotherProject(next, project)) return { ancestors, other: next, root };
    const above = known(lookups, next, before);
    if (above.status !== "done") root = false;
    next = above.parent;
  }
  return { ancestors, other: null, root };
}

/** A spec-approval ask: an ask.opened of an approval request naming the primary document of the
 * issue it is on, with the issue as the events before it left it. */
function specApprovalOpened(
  event: LogEvent,
  lookups: CensusLookups
): { issue: string; facts: IssueFacts; ask: string } | undefined {
  const { payload } = event;
  if (event.type !== "ask.opened" || payload.kind !== "approval" || event.issue_key === null) {
    return undefined;
  }
  if (payload.id === undefined) return undefined;
  const facts = known(lookups, event.issue_key, event.id);
  if (payload.approval?.artifact_id !== facts.primary) return undefined;
  return { issue: event.issue_key, facts, ask: payload.id };
}

function askRow(event: LogEvent, lookups: CensusLookups): CrossProjectAsk | undefined {
  const opened = specApprovalOpened(event, lookups);
  if (opened === undefined) return undefined;
  const chain = chainAt(opened.issue, opened.facts, event.id, lookups);
  if (!chain.root || chain.other === null) return undefined;
  const ancestor = known(lookups, chain.other, event.id);
  const approved = personsApprovalOfLatest(lookups.documentAt(ancestor.primary, event.id));
  if (approved === null) return undefined;
  return {
    issue: opened.issue,
    ask: opened.ask,
    openedAt: event.created_at,
    version: event.payload.approval?.version ?? null,
    ancestor: chain.other,
    ancestorVersion: approved,
  };
}

/** The first ask.opened of each ask among `events`, in event order: a later one restores the ask. */
function firstOpenings(events: readonly LogEvent[]): LogEvent[] {
  const seen = new Set<string>();
  return [...events]
    .sort((left, right) => left.id - right.id)
    .filter((event) => {
      if (event.type !== "ask.opened" || event.payload.id === undefined) return false;
      if (seen.has(event.payload.id)) return false;
      seen.add(event.payload.id);
      return true;
    });
}

/**
 * Each spec-approval ask among `events` on a root whose chain, read up to its first issue of
 * another project, reaches an issue whose spec held a person's approval of its latest version when
 * the ask opened. A root is an issue whose every ancestor of its own project is done. `events` are
 * the asks counted, those of the named projects in the window; `lookups` read the chain and the
 * approvals as of each ask, and an issue on a chain that they hold nothing of is refused.
 */
export function crossProjectAsks(
  events: readonly LogEvent[],
  lookups: CensusLookups
): CrossProjectAsk[] {
  return firstOpenings(events).flatMap((event) => askRow(event, lookups) ?? []);
}

/** A message in basisOpenedMessage's format: its first line, and the ancestor its line names. */
function openingOf(event: LogEvent): GateOpening | undefined {
  const { body, id } = event.payload;
  if (event.type !== "message.created" || event.issue_key === null) return undefined;
  if (typeof body !== "string" || id === undefined) return undefined;
  const lines = body.split("\n");
  if (lines[0] !== OPENED_LINE) return undefined;
  for (const line of lines) {
    const ancestor = ANCESTOR_LINE.exec(line)?.[1];
    if (ancestor !== undefined && ISSUE_KEY.test(ancestor)) {
      return { issue: event.issue_key, message: id, createdAt: event.created_at, ancestor };
    }
  }
  return undefined;
}

/**
 * Each message among `events` whose first line is exactly `OPENED_LINE` and whose line
 * `- The ancestor is KEY.` names an issue of another project than the one the message is on.
 */
export function gateOpenings(events: readonly LogEvent[]): GateOpening[] {
  return events.flatMap((event) => {
    const opening = openingOf(event);
    return opening !== undefined && ofAnotherProject(opening.ancestor, projectOf(opening.issue))
      ? [opening]
      : [];
  });
}

function pairAsk(event: LogEvent, pair: Pair, lookups: CensusLookups): PairAsk | undefined {
  const opened = specApprovalOpened(event, lookups);
  if (opened === undefined || opened.issue !== pair.root) return undefined;
  const chain = chainAt(opened.issue, opened.facts, event.id, lookups);
  const ancestor = known(lookups, pair.ancestor, event.id);
  const document = lookups.documentAt(ancestor.primary, event.id);
  return {
    ask: opened.ask,
    openedAt: event.created_at,
    version: event.payload.approval?.version ?? null,
    onChain: chain.ancestors.includes(pair.ancestor),
    ancestorLatestVersion: document.latestVersion,
    ancestorReview: document.review,
    ...lookups.closingOf(opened.ask),
  };
}

function pairReading(events: readonly LogEvent[], pair: Pair, lookups: CensusLookups): PairReading {
  const onRoot = events.filter((event) => event.issue_key === pair.root);
  return {
    ...pair,
    asks: firstOpenings(onRoot).flatMap((event) => pairAsk(event, pair, lookups) ?? []),
    openings: onRoot.flatMap((event) => {
      const opening = openingOf(event);
      return opening?.ancestor === pair.ancestor ? [opening] : [];
    }),
  };
}

function inWindow(event: LogEvent, from: number, to: number): boolean {
  const at = date(event.created_at, "an event's created_at");
  return at >= from && at < to;
}

/** The events of the window: every event at or after `from` and before `to`. */
function windowOf(events: readonly LogEvent[], options: CensusOptions): LogEvent[] {
  const from = options.from === undefined ? -Infinity : date(options.from, "--from");
  const to = options.to === undefined ? Infinity : date(options.to, "--to");
  return events.filter((event) => inWindow(event, from, to));
}

/**
 * The census of `events`: the cross-project asks and the gate openings on the named projects'
 * issues in the window, and each pair's root, read with `lookups`, which are the events' own unless
 * the caller read more history than the window holds.
 */
export function censusReport(
  events: readonly LogEvent[],
  options: CensusOptions,
  lookups: CensusLookups = eventLookups(events)
): CensusReport {
  const window = windowOf(events, options);
  const named = window.filter(
    (event) => event.issue_key !== null && options.projects.includes(projectOf(event.issue_key))
  );
  return {
    asks: crossProjectAsks(named, lookups),
    openings: gateOpenings(named),
    pairs: options.pairs.map((pair) => pairReading(window, pair, lookups)),
  };
}

/** A saved log: one event to a line, as the event log route serves each. */
function parseEventsFile(text: string, path: string): LogEvent[] {
  return text.split("\n").flatMap((line, index) => {
    if (line.trim() === "") return [];
    let event: unknown;
    try {
      event = JSON.parse(line);
    } catch {
      throw new Error(`${path}:${index + 1} is not JSON`);
    }
    const { id, type, created_at: createdAt } = (event ?? {}) as Partial<LogEvent>;
    if (typeof id !== "number" || typeof type !== "string" || typeof createdAt !== "string") {
      throw new Error(
        `${path}:${index + 1} is no event: it needs a numeric id, a type and a created_at`
      );
    }
    return [event as LogEvent];
  });
}

/** The id of the first event the event log holds after `after`, or undefined when it holds none. */
async function nextEventId(config: DispatchConfig, after: number): Promise<number | undefined> {
  const [next] = await get<LogEvent[]>(config, `/api/v1/events/log?after=${after}&limit=1`);
  if (next !== undefined && next.id <= after) {
    throw new Error(`GET /api/v1/events/log returned no id after ${after}`);
  }
  return next?.id;
}

/**
 * The id of the log's last event as the log stands when the read begins, or 0 for an empty log.
 * The route reads forward by id and takes no `before`, no order and no time, so a doubling and then
 * a halving search finds it, one event a read: whether an event lies after an id falls with the id
 * alone, whatever the events' times.
 */
async function lastEventId(config: DispatchConfig): Promise<number> {
  // `last` is an event's id and no event lies after `beyond`, so the log's last id is between them.
  let last = await nextEventId(config, 0);
  if (last === undefined) return 0;
  let beyond = last * 2;
  for (let next = await nextEventId(config, beyond); next !== undefined; ) {
    last = next;
    beyond = next * 2;
    next = await nextEventId(config, beyond);
  }
  while (last < beyond) {
    const middle = Math.floor((last + beyond) / 2);
    const next = await nextEventId(config, middle);
    if (next === undefined) beyond = middle;
    else last = next;
  }
  return last;
}

/**
 * Every event of the window: the whole event log, read a page at a time by id from its first event
 * through its last when the read began, with each event filtered by its time. Dispatch stamps an
 * event with its transaction's start, and a transaction that started earlier can append later
 * (`events.Broker.Append`), so ids and times are not in one order: no event's time says where the
 * window's other events lie.
 */
async function readWindow(config: DispatchConfig, from: string, to: string): Promise<LogEvent[]> {
  const fromTime = date(from, "--from");
  const toTime = date(to, "--to");
  const end = await lastEventId(config);
  const events: LogEvent[] = [];
  for (let after = 0; after < end; ) {
    const page = await get<LogEvent[]>(
      config,
      `/api/v1/events/log?after=${after}&limit=${EVENT_LOG_PAGE}`
    );
    const last = page.at(-1);
    // No event lies after `after` any longer: nothing the read began with is left.
    if (last === undefined) break;
    if (last.id <= after) throw new Error(`GET /api/v1/events/log returned no id after ${after}`);
    events.push(...page.filter((event) => event.id <= end && inWindow(event, fromTime, toTime)));
    after = last.id;
  }
  return events;
}

/** Each issue's whole history, `ISSUE_CONCURRENCY` at a time, each through a client of its own, so
 * each issue's reads get the client's deadline to themselves. */
async function readHistories(config: DispatchConfig, keys: readonly string[]): Promise<LogEvent[]> {
  const histories: LogEvent[][] = [];
  let next = 0;
  const reader = async (): Promise<void> => {
    while (next < keys.length) {
      const index = next++;
      const client = new DispatchClient(config.url, config.credential);
      histories[index] = (await fetchIssueEvents(client, keys[index])) as unknown as LogEvent[];
    }
  };
  await Promise.all(Array.from({ length: Math.min(ISSUE_CONCURRENCY, keys.length) }, reader));
  return histories.flat();
}

/** The issues `lookups` hold nothing of that the census of `window` reads, each with the refusal
 * that names it: each step of the reading runs as the census runs it, ask by ask. */
function unreadIssues(
  window: readonly LogEvent[],
  options: CensusOptions,
  lookups: CensusLookups
): Map<string, string> {
  const unread = new Map<string, string>();
  const step = (read: () => unknown) => {
    try {
      read();
    } catch (error) {
      if (!(error instanceof UnreadIssue)) throw error;
      unread.set(error.key, error.message);
    }
  };
  for (const event of firstOpenings(window)) {
    if (event.issue_key !== null && options.projects.includes(projectOf(event.issue_key))) {
      step(() => askRow(event, lookups));
    }
    for (const pair of options.pairs) {
      if (event.issue_key === pair.root) step(() => pairAsk(event, pair, lookups));
    }
  }
  return unread;
}

/**
 * The census of the window Dispatch's event log holds now. The asks and the openings are the
 * window's; the chains and the approvals they are read against are each issue's whole history, so
 * a parent set or an approval given before the window is read as it stood.
 */
async function liveCensus(
  config: DispatchConfig,
  options: CensusOptions & { readonly from: string; readonly to: string }
): Promise<CensusReport> {
  const window = await readWindow(config, options.from, options.to);
  const read = new Map<number, LogEvent>();
  const histories = new Set<string>();
  let fresh = [...new Set(options.pairs.flatMap((pair) => [pair.root, pair.ancestor]))];
  while (true) {
    for (const key of fresh) histories.add(key);
    for (const event of await readHistories(config, fresh)) read.set(event.id, event);
    const unread = unreadIssues(window, options, eventLookups([...read.values()]));
    fresh = [...unread.keys()].filter((key) => !histories.has(key));
    if (fresh.length > 0) continue;
    // An issue still unread is one whose whole history holds no issue event before the moment
    // the reading needs it.
    if (unread.size > 0) throw new Error([...unread.values()].join("; "));
    break;
  }
  for (const event of window) read.set(event.id, event);
  return censusReport(window, options, eventLookups([...read.values()]));
}

function parsePair(value: string): Pair {
  const [root, ancestor, extra] = value.split("=");
  if (
    root === undefined ||
    ancestor === undefined ||
    extra !== undefined ||
    !ISSUE_KEY.test(root) ||
    !ISSUE_KEY.test(ancestor)
  ) {
    throw new Error(`--pair takes ROOT=ANCESTOR, two issue keys: ${value}`);
  }
  return { root, ancestor };
}

function parseArguments(argv: readonly string[]): RunOptions {
  const { values } = parseArgs({
    args: [...argv],
    options: {
      project: { type: "string", multiple: true },
      from: { type: "string" },
      to: { type: "string" },
      pair: { type: "string", multiple: true },
      "events-file": { type: "string" },
      json: { type: "boolean" },
    },
    strict: true,
  });
  const { from, to, "events-file": eventsFile } = values;
  const projects = values.project ?? [];
  if (projects.length === 0) throw new Error("--project is required: name each project to read");
  for (const project of projects) {
    if (!PROJECT_KEY_PATTERN.test(project)) {
      throw new Error(`--project ${project} is not a project key`);
    }
  }
  const fromTime = from === undefined ? undefined : date(from, "--from");
  const toTime = to === undefined ? undefined : date(to, "--to");
  if (fromTime !== undefined && toTime !== undefined && fromTime >= toTime) {
    throw new Error("--from must be before --to");
  }
  const census = {
    projects,
    pairs: (values.pair ?? []).map(parsePair),
    json: values.json === true,
  };
  if (eventsFile === undefined) {
    if (from === undefined || to === undefined) {
      throw new Error("--from and --to are required without --events-file");
    }
    return { ...census, from, to };
  }
  if (eventsFile === "") throw new Error("--events-file requires a file path");
  return {
    ...census,
    eventsFile,
    ...(from === undefined ? {} : { from }),
    ...(to === undefined ? {} : { to }),
  };
}

/** The resolved Dispatch of this session, as every command resolves it. */
function dispatchConfig(): DispatchConfig {
  const { dispatch: resolution, warnings } = sessionDispatchSettings(
    sessionConfigFile("command", process.env),
    process.env,
    { cwd: process.cwd() }
  );
  for (const warning of warnings) console.error(`dispatch: ${warning}`);
  if (!resolution.enabled) {
    throw new Error(
      resolution.error === null
        ? `Dispatch is not configured: ${resolution.notice}`
        : `dispatch config: ${resolution.error}`
    );
  }
  return resolution;
}

/** A pair's ask as one table row: its nested facts in words. */
function printedPairAsk(ask: PairAsk): Record<string, string | number | boolean | null> {
  const review = ask.ancestorReview;
  return {
    ask: ask.ask,
    openedAt: ask.openedAt,
    version: ask.version,
    onChain: ask.onChain,
    ancestorLatestVersion: ask.ancestorLatestVersion,
    ancestorReview: review === null ? null : `${review.state} v${review.version} by ${review.by}`,
    answer:
      ask.answer === null
        ? null
        : `${ask.answer.selected.join(", ")} by ${ask.answer.by} at ${ask.answer.at}`,
    resolution: ask.resolution === null ? null : `${ask.resolution.kind} at ${ask.resolution.at}`,
  };
}

function printCensus(report: CensusReport): void {
  console.log("Spec-approval asks on a root under an approved issue of another project");
  console.table(report.asks);
  console.log("Design gates opened on an ancestor of another project");
  console.table(report.openings);
  for (const pair of report.pairs) {
    console.log(`${pair.root} under ${pair.ancestor}: spec-approval asks`);
    console.table(pair.asks.map(printedPairAsk));
    console.log(`${pair.root} under ${pair.ancestor}: openings that name ${pair.ancestor}`);
    console.table(pair.openings);
  }
}

async function run(options: RunOptions): Promise<void> {
  const report =
    options.eventsFile === undefined
      ? await liveCensus(dispatchConfig(), options)
      : censusReport(
          parseEventsFile(await readFile(options.eventsFile, "utf8"), options.eventsFile),
          options
        );
  // One object and nothing else on stdout under --json, so a program can read the whole of it.
  if (options.json) console.log(JSON.stringify(report));
  else printCensus(report);
}

if (import.meta.main) await run(parseArguments(Bun.argv.slice(2)));
