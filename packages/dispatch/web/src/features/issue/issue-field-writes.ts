import type { QueryCacheNotifyEvent, QueryClient, QueryKey } from "@tanstack/react-query";

import { issuePatchRequest, type WriteRequest } from "../../api/client";
import { mergeIssue } from "../../api/issue-cache";
import { inboxQuery, projectDefaultAssigneesQuery } from "../../api/queries";
import type { InboxRow, Issue, IssueDetails, IssuePriority, IssueSummary } from "../../api/types";
import { issueProject } from "../../lib/issue-key";
import { type Shape, shape } from "../writes/write-storage";
import { defineWriteKind, type Write, type Writes, writesOf } from "../writes/write-store";

/** The value each picked field of an issue takes: a priority, an assignee's login, a repository
 *  (owner/name); `null` unsets it. */
export interface IssueFieldValues {
  readonly assignee: string | null;
  readonly priority: IssuePriority | null;
  readonly repo: string | null;
}

export type IssueField = keyof IssueFieldValues;

/** One pick as the write store holds it and a reload restores it: the issue, the field, and the
 *  value picked. */
export type IssueFieldPick<F extends IssueField = IssueField> = {
  readonly [K in F]: {
    readonly field: K;
    readonly issueKey: string;
    readonly value: IssueFieldValues[K];
  };
}[F];

const keptPick = shape.either(
  shape.record({
    field: shape.oneOf("assignee"),
    issueKey: shape.string,
    value: shape.nullable(shape.string),
  }),
  shape.record({
    field: shape.oneOf("priority"),
    issueKey: shape.string,
    value: shape.oneOf(0, 1, 2, 3, null),
  }),
  shape.record({
    field: shape.oneOf("repo"),
    issueKey: shape.string,
    value: shape.nullable(shape.string),
  })
) satisfies Shape<IssueFieldPick>;

/** What the shell's indicator names one pick. */
export function pickLabel(pick: IssueFieldPick): string {
  switch (pick.field) {
    case "assignee":
      return `${pick.issueKey} assigned to ${pick.value ?? "nobody"}`;
    case "priority":
      return `${pick.issueKey} set to ${pick.value === null ? "no priority" : `P${pick.value}`}`;
    case "repo":
      return `${pick.issueKey} on ${pick.value ?? "its project's repository"}`;
  }
}

/** The request one pick makes: the issue's PATCH of that field alone. */
export function pickRequest(pick: IssueFieldPick): WriteRequest<Issue> {
  switch (pick.field) {
    case "assignee":
      return issuePatchRequest(pick.issueKey, { assignee: pick.value });
    case "priority":
      return issuePatchRequest(pick.issueKey, { priority: pick.value });
    case "repo":
      return issuePatchRequest(pick.issueKey, { repo: pick.value });
  }
}

/** Whether `a` and `b` are picks of one field of one issue. */
export function sameField(
  a: Pick<IssueFieldPick, "field" | "issueKey">,
  b: Pick<IssueFieldPick, "field" | "issueKey">
): boolean {
  return a.field === b.field && a.issueKey === b.issueKey;
}

/** The lists of issues a pick shows on beside the issue's detail: each project's
 *  (`["issues", "project", …]`, its board and its table) and each of the Priorities view
 *  (`["issues", "advances", …]`, `/priorities` and `/priorities/<id>`). */
const issueLists: readonly unknown[] = ["project", "advances"];
const inboxKey = inboxQuery().queryKey;

/** The fields of an issue a pick changes, as its detail and its row of each list of issues carry
 *  them. */
type IssueFields = Pick<Issue, "assignee" | "priority" | "repo" | "resolved_repo">;

/** How one field of an issue shows wherever the issue is drawn: the issue's own fields, and, for
 *  the assignee, the person each of its Inbox rows waits on as Dispatch last answered it, by ask
 *  id (`waiting`); without it, that person follows the rule `showOnInboxRow` keeps. */
interface Look {
  readonly fields: Partial<IssueFields>;
  readonly waiting?: ReadonlyMap<string, string | undefined>;
}

/** A look of one field of one issue to put on every cache that draws it. */
interface Showing {
  readonly field: IssueField;
  readonly issueKey: string;
  readonly look: Look;
}

/** How `pick` shows. An own repository is also the one the issue resolves to; an inherited one
 *  is left to Dispatch's answer. */
function pickLook(pick: IssueFieldPick): Look {
  switch (pick.field) {
    case "assignee":
      return { fields: { assignee: pick.value } };
    case "priority":
      return { fields: { priority: pick.value } };
    case "repo":
      return {
        fields:
          pick.value === null ? { repo: null } : { repo: pick.value, resolved_repo: pick.value },
      };
  }
}

/** How `field` shows as `issue` - Dispatch's answer, or a cache's copy of it - carries it. */
function issueLook(field: IssueField, issue: IssueFields): Look {
  switch (field) {
    case "assignee":
      return { fields: { assignee: issue.assignee } };
    case "priority":
      return { fields: { priority: issue.priority } };
    case "repo":
      return { fields: { repo: issue.repo, resolved_repo: issue.resolved_repo } };
  }
}

/** Every Inbox row the client holds. */
function inboxRows(client: QueryClient): InboxRow[] {
  return client
    .getQueriesData<InboxRow[]>({ queryKey: inboxKey })
    .flatMap(([, rows]) => (Array.isArray(rows) ? rows : []));
}

/** How `field` of `issueKey` shows in the client's caches now - its detail, else its row of a
 *  list of issues, else an Inbox row of it - or undefined where none holds the issue. */
function cachedLook(client: QueryClient, { field, issueKey }: IssueFieldPick): Look | undefined {
  const issue =
    client.getQueryData<IssueDetails>(["issue", issueKey]) ??
    client
      .getQueryCache()
      .getAll()
      .filter((query) => cacheOf(query.queryKey) === "list")
      .flatMap((query) =>
        Array.isArray(query.state.data) ? (query.state.data as IssueSummary[]) : []
      )
      .find((row) => row.key === issueKey);
  const rows = inboxRows(client).filter((row) => row.issue_key === issueKey);
  const [row] = rows;
  let look: Look | undefined;
  if (issue !== undefined) look = issueLook(field, issue);
  else if (field === "priority" && row !== undefined) look = { fields: { priority: row.priority } };
  else if (field === "assignee" && row?.issue !== undefined) {
    look = { fields: { assignee: row.issue.assignee } };
  }
  if (field !== "assignee" || look === undefined) return look;
  return { ...look, waiting: new Map(rows.map((each) => [each.id, each.waiting_on_person])) };
}

/** `issue` with `fields` over it: `issue` itself where it carries them all already. */
function withFields<T extends IssueFields>(issue: T, fields: Partial<IssueFields>): T {
  const names = Object.keys(fields) as (keyof IssueFields)[];
  return names.every((name) => issue[name] === fields[name]) ? issue : { ...issue, ...fields };
}

/** `row` with `showing` over it, where the row is of its issue: `row` itself where it shows it
 *  already. A row is judged by the person it waits on, which Dispatch answers as
 *  `coalesce(a.asked_of, i.assignee, p.default_assignee)`: so a row whose ask nobody has handed on
 *  follows the assignee here, and one somebody handed to a person keeps that person whoever holds
 *  its issue. An unassign moves a row nobody handed on to its project's default assignee, taken
 *  from the defaults read the Settings page holds (`projectDefaultAssigneesQuery`, `fallback`)
 *  where the client has it; where it does not, the row keeps the person it waited on until the
 *  read again names the default. It never names nobody: every project names a default, so no row
 *  Dispatch answers does, and one written so would read as an ask no person waits on. A look that
 *  names the person a row waited on as Dispatch last answered it (`Look.waiting`) puts that person
 *  back. The Inbox draws no repository. */
function showOnInboxRow(row: InboxRow, showing: Showing, fallback: string | undefined): InboxRow {
  if (row.issue_key !== showing.issueKey) return row;
  const { fields, waiting } = showing.look;
  if (showing.field === "priority") {
    const { priority } = fields;
    return priority === undefined || row.priority === priority ? row : { ...row, priority };
  }
  const { assignee } = fields;
  if (showing.field !== "assignee" || assignee === undefined || row.issue === undefined) return row;
  const moved =
    row.issue.assignee === assignee ? row : { ...row, issue: { ...row.issue, assignee } };
  if (row.asked_of !== null && row.asked_of !== undefined) return moved;
  const answered = waiting?.has(row.id) === true;
  const person = answered ? waiting?.get(row.id) : (assignee ?? fallback);
  if (!answered && person === undefined) return moved;
  return moved.waiting_on_person === person ? moved : { ...moved, waiting_on_person: person };
}

/** `rows` with each changed by `change`: `rows` itself where none changes. */
function mapRows<R>(rows: readonly R[], change: (row: R) => R): readonly R[] {
  let changed = false;
  const next = rows.map((row) => {
    const after = change(row);
    changed ||= after !== row;
    return after;
  });
  return changed ? next : rows;
}

/** The default assignee of `issueKey`'s project where the client holds the defaults read. */
function defaultAssignee(client: QueryClient, issueKey: string): string | undefined {
  const project = issueProject(issueKey);
  return client
    .getQueryData(projectDefaultAssigneesQuery().queryKey)
    ?.find((entry) => entry.project === project)?.assignee;
}

/** Which of the caches a pick shows on the query under `queryKey` is: an issue's detail, a list
 *  of issues (`issueLists`), or the Inbox; undefined for any other. */
function cacheOf(queryKey: QueryKey): "detail" | "inbox" | "list" | undefined {
  const [head, second] = queryKey;
  if (head === "issue" && queryKey.length === 2) return "detail";
  if (head === "issues" && issueLists.includes(second)) return "list";
  return head === inboxKey[0] ? "inbox" : undefined;
}

/** `data`, which the query under `queryKey` holds, with every one of `showings` over it: the
 *  same value where it shows them all already, or where the query is none of the three a pick
 *  shows on. */
function shownOn(
  client: QueryClient,
  queryKey: QueryKey,
  data: unknown,
  showings: readonly Showing[]
): unknown {
  if (data === undefined || showings.length === 0) return data;
  const cache = cacheOf(queryKey);
  if (cache === "detail") {
    return showings.reduce(
      (issue, showing) =>
        showing.issueKey === queryKey[1] ? withFields(issue, showing.look.fields) : issue,
      data as IssueDetails
    );
  }
  if (!Array.isArray(data)) return data;
  if (cache === "list") {
    return mapRows(data as IssueSummary[], (row) =>
      showings.reduce(
        (issue, showing) =>
          showing.issueKey === issue.key ? withFields(issue, showing.look.fields) : issue,
        row
      )
    );
  }
  if (cache === "inbox") {
    // Looked up once for each assignee shown on this cache, not once a row.
    const fallbacks = showings.map((showing) =>
      showing.field === "assignee" ? defaultAssignee(client, showing.issueKey) : undefined
    );
    return mapRows(data as InboxRow[], (row) =>
      showings.reduce(
        (shown, showing, index) => showOnInboxRow(shown, showing, fallbacks[index]),
        row
      )
    );
  }
  return data;
}

/** Whether this module is writing a cache itself, which its own watch is not to answer: what it
 *  writes is a pick shown, not an answer of Dispatch's. */
let writing = false;

/** Puts `data` under `queryKey`, as this module's own write. */
function put(client: QueryClient, queryKey: QueryKey, data: unknown): void {
  writing = true;
  try {
    client.setQueryData(queryKey, data);
  } finally {
    writing = false;
  }
}

/** Shows every one of `showings` on each cache of `client` that draws its issue, writing only
 *  the caches that do not show it already. */
function showEverywhere(client: QueryClient, showings: readonly Showing[]): void {
  if (showings.length === 0) return;
  for (const query of client.getQueryCache().getAll()) {
    const data = query.state.data;
    const next = shownOn(client, query.queryKey, data, showings);
    if (next !== data) put(client, query.queryKey, next);
  }
}

/** What a client's picks are doing: every live one - joined its line, and neither landed nor
 *  refused, discarded, dropped or stopped - by write id, and each query that was reading when a
 *  pick shown on it landed, by hash, with what to put back over the answer of that read. */
interface Watch {
  readonly live: Map<number, Write<IssueFieldPick>>;
  readonly held: Map<string, Map<string, Showing>>;
  readonly unsubscribe: () => void;
}

const watches = new WeakMap<QueryClient, Watch>();

/** The field of an issue a pick or a showing is of, as one name. */
function fieldName(of: Pick<IssueFieldPick, "field" | "issueKey">): string {
  return `${of.issueKey} ${of.field}`;
}

/** Whether `write` may still reach Dispatch: waiting its turn, or out, within its deadline or
 *  past it and still being tried. A refused or discarded write is not. */
function isLive(write: Write): boolean {
  return write.status === "queued" || write.status === "sending" || write.status === "late";
}

/** What the request of `write`, a write on the line of `issueKey` of any kind - a pick, a board
 *  move to another lane, a key of the issue page - sets `field` to, as a pick of it; undefined
 *  where it sets no such field. */
function pickIn(
  writes: Writes,
  write: Write,
  issueKey: string,
  field: IssueField
): IssueFieldPick | undefined {
  if (write.target !== `issue:${issueKey}`) return undefined;
  const body = writes.requestOf(write.id)?.body;
  if (typeof body !== "object" || body === null || !Object.hasOwn(body, field)) return undefined;
  const pick: unknown = { field, issueKey, value: Reflect.get(body, field) };
  return keptPick(pick) ? pick : undefined;
}

/** Whether `writes` holds a live write on `pick`'s line, of any kind, that joined it after
 *  `pick` and sets the same field: the one Dispatch will store last. */
export function newerWriteOf(writes: Writes, pick: Write<IssueFieldPick>): boolean {
  const { field, issueKey } = pick.detail;
  return writes
    .rows()
    .some(
      (row) =>
        row.joined > pick.joined &&
        isLive(row) &&
        pickIn(writes, row, issueKey, field) !== undefined
    );
}

/** How `field` of `issueKey` shows while writes of it are live: as the newest of them, which
 *  Dispatch will store last, whichever control made it - of this client's live picks (`live`),
 *  and of every other live write on the issue's line whose request sets the field. Undefined
 *  while none is live. */
function newestWrite(
  client: QueryClient,
  live: ReadonlyMap<number, Write<IssueFieldPick>>,
  issueKey: string,
  field: IssueField
): Showing | undefined {
  let newest: { readonly joined: number; readonly pick: IssueFieldPick } | undefined;
  for (const write of live.values()) {
    if (!sameField(write.detail, { field, issueKey })) continue;
    if (newest === undefined || write.joined > newest.joined) {
      newest = { joined: write.joined, pick: write.detail };
    }
  }
  const writes = writesOf(client);
  for (const row of writes.rows()) {
    // A pick is read from `live` alone: the store shows it a moment before its kind is told of
    // it (`started`), which records first what the cache showed before it.
    if (row.kind === KIND || !isLive(row) || (newest !== undefined && row.joined < newest.joined)) {
      continue;
    }
    const pick = pickIn(writes, row, issueKey, field);
    if (pick !== undefined) newest = { joined: row.joined, pick };
  }
  return newest === undefined ? undefined : { field, issueKey, look: pickLook(newest.pick) };
}

/** How each field of an issue that has a live pick shows - of `issueKey`'s fields alone, where
 *  it is given: as the newest live write of that field (`newestWrite`). A field no pick of which
 *  is live is left as each answer has it, whatever other write of it is out. */
function newestPicks(client: QueryClient, watch: Watch, issueKey?: string): Showing[] {
  const fields = new Map<string, IssueFieldPick>();
  for (const { detail } of watch.live.values()) {
    if (issueKey === undefined || detail.issueKey === issueKey)
      fields.set(fieldName(detail), detail);
  }
  return [...fields.values()].flatMap((pick) => {
    const showing = newestWrite(client, watch.live, pick.issueKey, pick.field);
    return showing === undefined ? [] : [showing];
  });
}

/** What one change of the client's cache does to its picks. An answer of a read, or a merge of
 *  another write's answer, on a cache a pick shows on has each field a pick of which is live shown
 *  again over it as the newest live write of that field; an answer of a read that was out when a
 *  pick shown on it landed has that landed value shown again over it, and ends its hold, as a new
 *  read of that query or its failure does. */
function answered(client: QueryClient, watch: Watch, event: QueryCacheNotifyEvent): void {
  if (writing) return;
  const { query } = event;
  if (event.type === "removed") {
    watch.held.delete(query.queryHash);
    release(client, watch);
    return;
  }
  if (event.type !== "updated") return;
  const { action } = event;
  const fetched = action.type === "success" && action.manual !== true;
  const held = watch.held.get(query.queryHash);
  if (held !== undefined && (fetched || action.type === "fetch" || action.type === "error")) {
    watch.held.delete(query.queryHash);
  }
  if (action.type === "success" && cacheOf(query.queryKey) !== undefined) {
    const showings = new Map(fetched && held !== undefined ? held : []);
    for (const showing of newestPicks(client, watch)) showings.set(fieldName(showing), showing);
    const data = query.state.data;
    const next = shownOn(client, query.queryKey, data, [...showings.values()]);
    if (next !== data) put(client, query.queryKey, next);
  }
  release(client, watch);
}

/** The client's watch, begun if it has none: of its cache, and of its store, since a write of
 *  another kind that sets a field a pick shows - a board move to another lane - can join the
 *  issue's line after the surface that made it has written its own cache. */
function watchOf(client: QueryClient): Watch {
  const current = watches.get(client);
  if (current !== undefined) return current;
  const watch: Watch = {
    held: new Map(),
    live: new Map(),
    unsubscribe: () => {
      fromCache();
      fromStore();
    },
  };
  const fromCache = client.getQueryCache().subscribe((event) => answered(client, watch, event));
  const fromStore = writesOf(client).subscribe(() => {
    if (!writing) showEverywhere(client, newestPicks(client, watch));
  });
  watches.set(client, watch);
  return watch;
}

/** Ends the client's watch once it has no live pick and holds no read. */
function release(client: QueryClient, watch: Watch): void {
  if (watch.live.size > 0 || watch.held.size > 0 || watches.get(client) !== watch) return;
  watches.delete(client);
  watch.unsubscribe();
}

/** How each field of each issue shows as Dispatch last confirmed it, per client: taken from the
 *  cache at the first pick of a run of picks of that field, and from Dispatch's answer at each
 *  landing. A refused, discarded or dropped pick that leaves no live write of its field puts
 *  this back. */
let confirmed = new WeakMap<QueryClient, Map<string, Look>>();

function confirmedOf(client: QueryClient): Map<string, Look> {
  let looks = confirmed.get(client);
  if (looks === undefined) {
    looks = new Map();
    confirmed.set(client, looks);
  }
  return looks;
}

/** Has the client read again what a pick of `issueKey` showed on, as Dispatch now holds it. */
function readAgain(client: QueryClient, issueKey: string): void {
  void client.invalidateQueries({ queryKey: ["issue", issueKey] });
  void client.invalidateQueries({ queryKey: ["issues"] });
  void client.invalidateQueries({ queryKey: inboxKey });
}

/** The name of this kind of write, which a reload finds it by. */
const KIND = "issue-field";

/**
 * One pick of one field of one issue - its priority, its assignee or its repository - as a write
 * of the page's store, on the issue's line (`issue:<KEY>`), so it goes out in the order it was
 * made behind every other write of that issue, is kept across a reload, and is sent although the
 * picker that made it is gone (`useIssueFieldWrite`).
 *
 * The pick's write, not its picker, keeps it on screen. As it joins its line (`started`: a press,
 * a Retry, a reload) it shows on the issue's detail, its row of each list of issues (each
 * project's board and table, each table of the Priorities view) and each Inbox row of it. From
 * then on, while a pick of a field is live, the field shows as the newest live write of it on
 * the issue's line, whichever control made it - this picker, another, the issue page's keys, a
 * board move to another lane (`newestWrite`) - over every answer that lands on one of those
 * caches, a read's or another write's merged (`mergeIssue`), the first after a reload among them,
 * and again whenever the store's writes change. Nothing waits and no read is cancelled: each
 * answer is taken whole, and only the field is put back over it. What this module writes itself
 * is no answer (`writing`), and it writes nothing where the cache already shows the value.
 *
 * Landed, it merges Dispatch's answer and records it as the value Dispatch last confirmed. Where
 * a newer live write of the field is left, that one shows. Where none is, the landed value shows
 * and the issue, the lists and the Inbox are read again; a read that was out at the landing may
 * have been composed before it, and its answer can still come back with the old value
 * (`DispatchQueryClient` cancels no read, and reads again once it is in), so over the answer of
 * each read out at the landing the landed value shows again, and a read that begins after the
 * landing answers for itself. Refused, discarded or dropped (`undone`), the newest live write of
 * the field shows, so a pick a newer one took out puts nothing back; where none is left, the
 * value Dispatch last confirmed (`confirmed`) shows and the caches are read again. The watch ends
 * once no pick is live and no read is held. Whoever is signed in changing lets go of every value
 * confirmed (`reset`).
 */
export const issueFieldWrite = defineWriteKind<IssueFieldPick>({
  kept: keptPick,
  landed: (client, write, answer) => {
    const watch = watches.get(client);
    watch?.live.delete(write.id);
    const { detail } = write;
    const name = fieldName(detail);
    const looks = confirmedOf(client);
    // A landing Dispatch kept no answer for stored the pick all the same.
    const look =
      answer === null
        ? { fields: { ...looks.get(name)?.fields, ...pickLook(detail).fields } }
        : issueLook(detail.field, answer as Issue);
    looks.set(name, look);
    if (answer !== null) {
      writing = true;
      try {
        mergeIssue(client, answer as Issue);
      } finally {
        writing = false;
      }
    }
    const live = watch === undefined ? [] : newestPicks(client, watch, detail.issueKey);
    const newer = newestWrite(client, watch?.live ?? new Map(), detail.issueKey, detail.field);
    if (newer !== undefined) {
      showEverywhere(client, [...live, newer]);
    } else {
      const landed: Showing = { field: detail.field, issueKey: detail.issueKey, look };
      showEverywhere(client, [...live, landed]);
      const reading = client
        .getQueryCache()
        .getAll()
        .filter(
          (query) => query.state.fetchStatus !== "idle" && cacheOf(query.queryKey) !== undefined
        );
      if (reading.length > 0) {
        const held = watch ?? watchOf(client);
        for (const query of reading) {
          const showings = held.held.get(query.queryHash) ?? new Map<string, Showing>();
          showings.set(name, landed);
          held.held.set(query.queryHash, showings);
        }
      }
      readAgain(client, detail.issueKey);
    }
    const current = watches.get(client);
    if (current !== undefined) release(client, current);
  },
  name: KIND,
  reset: () => {
    confirmed = new WeakMap();
  },
  started: (client, write) => {
    const watch = watchOf(client);
    const { detail } = write;
    // The first pick of a run of picks of this field: what the cache shows now is what Dispatch
    // last gave it. A write already live - one a lapse put back in its line - begins no run.
    if (![...watch.live.values()].some((live) => sameField(live.detail, detail))) {
      const look = cachedLook(client, detail);
      if (look !== undefined) confirmedOf(client).set(fieldName(detail), look);
    }
    watch.live.set(write.id, write);
    showEverywhere(client, newestPicks(client, watch, detail.issueKey));
  },
  undone: (client, write) => {
    const watch = watches.get(client);
    watch?.live.delete(write.id);
    const { detail } = write;
    const live = watch === undefined ? [] : newestPicks(client, watch, detail.issueKey);
    const newer = newestWrite(client, watch?.live ?? new Map(), detail.issueKey, detail.field);
    if (newer !== undefined) {
      showEverywhere(client, [...live, newer]);
    } else {
      const look = confirmed.get(client)?.get(fieldName(detail));
      showEverywhere(
        client,
        look === undefined
          ? live
          : [...live, { field: detail.field, issueKey: detail.issueKey, look }]
      );
      readAgain(client, detail.issueKey);
    }
    if (watch !== undefined) release(client, watch);
  },
});
