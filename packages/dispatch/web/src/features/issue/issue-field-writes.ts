import {
  type Mutation,
  matchMutation,
  type Query,
  type QueryCacheNotifyEvent,
  type QueryClient,
  type QueryKey,
} from "@tanstack/react-query";

import { issuePatchRequest, type WriteRequest } from "../../api/client";
import { mergeIssue } from "../../api/issue-cache";
import { boardMoveMutationKey, inboxQuery, projectDefaultAssigneesQuery } from "../../api/queries";
import type { InboxRow, Issue, IssueDetails, IssuePriority, IssueSummary } from "../../api/types";
import { issueProject } from "../../lib/issue-key";
import type { BoardMoveVariables } from "../project/board-moves";
import { type Shape, shape } from "../writes/write-storage";
import {
  defineWriteKind,
  isLive,
  type Write,
  type WriteOutcome,
  type Writes,
  writesOf,
} from "../writes/write-store";

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
export type IssueFieldPick = {
  readonly [K in IssueField]: {
    readonly field: K;
    readonly issueKey: string;
    readonly value: IssueFieldValues[K];
  };
}[IssueField];

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

/** How `field` shows as `issue` - an issue's detail or its row of a list of issues - carries it,
 *  else as the first of `rows`, the issue's Inbox rows, does; for the assignee, with the person
 *  each of those rows waits on. Undefined where neither carries the field. */
function lookOf(
  field: IssueField,
  issue: IssueFields | undefined,
  rows: readonly InboxRow[]
): Look | undefined {
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

/** The row of `issueKey` in the first list of issues the client holds that has one. */
function listedIssue(client: QueryClient, issueKey: string): IssueSummary | undefined {
  for (const query of client.getQueryCache().getAll()) {
    if (cacheOf(query.queryKey) !== "list" || !Array.isArray(query.state.data)) continue;
    const row = (query.state.data as IssueSummary[]).find((each) => each.key === issueKey);
    if (row !== undefined) return row;
  }
  return undefined;
}

/** How `field` of `issueKey` shows in the client's caches now - its detail, else its row of a
 *  list of issues, else an Inbox row of it - or undefined where none holds the issue. */
function cachedLook(client: QueryClient, { field, issueKey }: IssueFieldPick): Look | undefined {
  const issue =
    client.getQueryData<IssueDetails>(["issue", issueKey]) ?? listedIssue(client, issueKey);
  return lookOf(
    field,
    issue,
    inboxRows(client).filter((row) => row.issue_key === issueKey)
  );
}

/** How `field` of `issueKey` shows in `data`, which the query under `queryKey` holds - an
 *  issue's detail, a list of issues or the Inbox - or undefined where it does not hold the
 *  issue. */
function lookIn(
  queryKey: QueryKey,
  data: unknown,
  { field, issueKey }: Pick<IssueFieldPick, "field" | "issueKey">
): Look | undefined {
  const cache = cacheOf(queryKey);
  if (cache === "detail") {
    return queryKey[1] === issueKey && data !== undefined
      ? lookOf(field, data as IssueDetails, [])
      : undefined;
  }
  if (!Array.isArray(data)) return undefined;
  if (cache === "list") {
    const row = (data as IssueSummary[]).find((each) => each.key === issueKey);
    return row === undefined ? undefined : lookOf(field, row, []);
  }
  if (cache !== "inbox") return undefined;
  const rows = (data as InboxRow[]).filter((row) => row.issue_key === issueKey);
  return lookOf(field, undefined, rows);
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

/** `rows` with each changed by `change`: `rows` itself where none changes, copied from the first
 *  that does. */
function mapRows<R>(rows: readonly R[], change: (row: R) => R): readonly R[] {
  let next: R[] | undefined;
  for (const [index, row] of rows.entries()) {
    const after = change(row);
    if (next === undefined && after !== row) next = rows.slice(0, index);
    next?.push(after);
  }
  return next ?? rows;
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

/** Runs `change`, a write of this module's own to the client's cache. */
function asOwnWrite(change: () => void): void {
  writing = true;
  try {
    change();
  } finally {
    writing = false;
  }
}

/** Puts `data` under `queryKey`, as this module's own write. */
function put(client: QueryClient, queryKey: QueryKey, data: unknown): void {
  asOwnWrite(() => client.setQueryData(queryKey, data));
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
 *  refused, discarded, dropped or stopped - by write id; each live write of another kind that sets
 *  a field the watch shows (`follow`), by write id, and each board move not yet answered that sets
 *  one, each with what it sets; the newest place in a line it has seen; and each query that was
 *  reading when a write of a field shown on it landed (`holdReads`), by hash, with what to put
 *  back over the answer of that read. */
interface Watch {
  readonly live: Map<number, Write<IssueFieldPick>>;
  readonly followed: Map<number, readonly IssueFieldPick[]>;
  readonly moves: Map<BoardMove, readonly IssueFieldPick[]>;
  lastJoin: number;
  readonly held: Map<string, Map<string, Showing>>;
  readonly unsubscribe: () => void;
}

const watches = new WeakMap<QueryClient, Watch>();

/** The field of an issue a pick or a showing is of, as one name. */
function fieldName(of: Pick<IssueFieldPick, "field" | "issueKey">): string {
  return `${of.issueKey} ${of.field}`;
}

/** Every field `watch` shows, as a pick of it: of each live pick, each followed write and each
 *  followed board move. */
function watchedFields(watch: Watch): IssueFieldPick[] {
  return [
    ...[...watch.live.values()].map((write) => write.detail),
    ...[...watch.followed.values()].flat(),
    ...[...watch.moves.values()].flat(),
  ];
}

/** What `body`, a request's or a board move's change of `issueKey`, sets `field` to, as a pick of
 *  it; undefined where it sets no such field. */
function fieldIn(body: unknown, issueKey: string, field: IssueField): IssueFieldPick | undefined {
  if (typeof body !== "object" || body === null || !Object.hasOwn(body, field)) return undefined;
  const pick: unknown = { field, issueKey, value: Reflect.get(body, field) };
  return keptPick(pick) ? pick : undefined;
}

/** What the request of `write`, a write on the line of `of`'s issue of any kind - a pick, a board
 *  move to another lane, a key of the issue page - sets `of`'s field to, as a pick of it;
 *  undefined where it sets no such field. */
function pickIn(
  writes: Writes,
  write: Write,
  of: Pick<IssueFieldPick, "field" | "issueKey">
): IssueFieldPick | undefined {
  if (write.target !== `issue:${of.issueKey}`) return undefined;
  return fieldIn(writes.requestOf(write.id)?.body, of.issueKey, of.field);
}

/** A move of a card on a project's board (`useBoardMoves`): a mutation of its board's scope,
 *  whose variables name the issue and its change (`BoardMoveVariables`, `moveChange`). */
type BoardMove = Mutation;

/** The key every board move carries (`boardMoveMutationKey`), whatever its project. */
const boardMoves = boardMoveMutationKey("").slice(0, 1);

/** When each board move was made, as the newest place in a line of the store then: a write that
 *  joined at or before it was made before the move. Recorded as the move is made while a watch
 *  is on (`Watch.lastJoin`); a move made while none was on was made before every pick live now. */
const movesMade = new WeakMap<BoardMove, number>();

/** The newest place any write the store holds has in its line. */
function newestJoin(writes: Writes): number {
  return writes.rows().reduce((newest, row) => Math.max(newest, row.joined), 0);
}

/** What `move` changes, as its variables name it: the issue (`key`) and its change (`input`);
 *  undefined where they name no issue. */
function moveChange(
  move: BoardMove
): { readonly input: unknown; readonly issueKey: string } | undefined {
  const variables = move.state.variables;
  if (typeof variables !== "object" || variables === null) return undefined;
  const { input, key } = variables as { readonly [name in keyof BoardMoveVariables]?: unknown };
  return typeof key === "string" ? { input, issueKey: key } : undefined;
}

/** What `move` sets each field of its issue to, as picks of them. */
function movePicks(move: BoardMove): IssueFieldPick[] {
  const change = moveChange(move);
  if (change === undefined) return [];
  return (["assignee", "priority", "repo"] as const).flatMap((field) => {
    const pick = fieldIn(change.input, change.issueKey, field);
    return pick === undefined ? [] : [pick];
  });
}

/** Every board move of the client not yet answered: waiting its turn in its board's scope - its
 *  write not yet in the store (`state.isPaused`) - or out. */
function pendingMoves(client: QueryClient): BoardMove[] {
  return client.getMutationCache().findAll({ mutationKey: boardMoves, status: "pending" });
}

/** A board move's place in the person's order: its drop - after every write that had joined a
 *  line by then (`movesMade`), before the next, and after an earlier move. One made while no watch
 *  was on was made before every pick live now. */
function moveRank(move: BoardMove): number {
  return (movesMade.get(move) ?? -1) + 0.5;
}

/** Of `moves`, the one whose write `row` is: a move of the row's issue whose change is the row's
 *  request. */
function moveOfRow(moves: readonly BoardMove[], writes: Writes, row: Write): BoardMove | undefined {
  const body = JSON.stringify(writes.requestOf(row.id)?.body);
  return moves.find((move) => {
    const change = moveChange(move);
    return (
      change !== undefined &&
      row.target === `issue:${change.issueKey}` &&
      JSON.stringify(change.input) === body
    );
  });
}

/** One write of a field of an issue: what it sets the field to, its place in the person's order,
 *  whether it is a pick, whether it may still reach Dispatch, and the store's write of it - none
 *  for a board move still waiting its turn on its board. */
export interface FieldWrite {
  readonly id: number | undefined;
  readonly live: boolean;
  readonly ofPick: boolean;
  readonly pick: IssueFieldPick;
  readonly rank: number;
}

/** Every write of `of`'s field the client holds, of any kind, at its place in the person's order:
 *  each write on the issue's line whose request sets the field - a pick, a key of the issue page -
 *  at its place in the line, and each board move of the issue, at its drop (`moveRank`), whether
 *  its write has joined the line or it still waits its turn in its board's scope behind another
 *  card's move. So a move made after a pick is newer than it from its drop, and a pick made while
 *  a move still waits is newer than the move although it goes out first, until the move lands. */
export function fieldWritesOf(
  client: QueryClient,
  of: Pick<IssueFieldPick, "field" | "issueKey">
): FieldWrite[] {
  const writes = writesOf(client);
  const moves = pendingMoves(client);
  const found: FieldWrite[] = [];
  for (const row of writes.rows()) {
    const pick = pickIn(writes, row, of);
    if (pick === undefined) continue;
    const ofPick = row.kind === KIND;
    const move = ofPick ? undefined : moveOfRow(moves, writes, row);
    const rank = move === undefined ? row.joined : moveRank(move);
    found.push({ id: row.id, live: isLive(row), ofPick, pick, rank });
  }
  for (const move of moves) {
    const pick = movePicks(move).find((each) => sameField(each, of));
    if (!move.state.isPaused || pick === undefined) continue;
    found.push({ id: undefined, live: true, ofPick: false, pick, rank: moveRank(move) });
  }
  return found;
}

/** Whether the client holds a live write of `pick`'s field, of any kind - a pick, a board move to
 *  another lane, out or still waiting its turn on its board - newer than `pick` in the person's
 *  order (`fieldWritesOf`): the one the screen shows. */
export function newerWriteOf(client: QueryClient, pick: Write<IssueFieldPick>): boolean {
  return fieldWritesOf(client, pick.detail).some((write) => write.live && write.rank > pick.joined);
}

/** How `of`'s field shows while writes of it are live: as the newest of them
 *  (`fieldWritesOf`), whichever control made it - of the watch's live picks, and of every other
 *  live write of the field, a board move among them. Undefined while none is live. */
function newestWrite(
  client: QueryClient,
  watch: Watch | undefined,
  of: Pick<IssueFieldPick, "field" | "issueKey">
): Showing | undefined {
  let newest: { readonly pick: IssueFieldPick; readonly rank: number } | undefined;
  for (const write of watch?.live.values() ?? []) {
    if (!sameField(write.detail, of)) continue;
    if (newest === undefined || write.joined > newest.rank) {
      newest = { pick: write.detail, rank: write.joined };
    }
  }
  for (const write of fieldWritesOf(client, of)) {
    // A pick is read from the watch's live picks alone: the store shows it a moment before its
    // kind is told of it (`started`), which records first what the cache showed before it. Of
    // two moves dropped between the same two joins, the later is newer.
    if (write.ofPick || !write.live || (newest !== undefined && write.rank < newest.rank)) {
      continue;
    }
    newest = write;
  }
  if (newest === undefined) return undefined;
  return { field: of.field, issueKey: of.issueKey, look: pickLook(newest.pick) };
}

/** How each field of an issue the watch shows (`watchedFields`) shows - of `issueKey`'s fields
 *  alone, where it is given: as the newest live write of that field (`newestWrite`), until the
 *  last pick, followed write or followed move of it ends. A field of none is left as each answer
 *  has it, whatever other write of it is out. */
function newestPicks(client: QueryClient, watch: Watch, issueKey?: string): Showing[] {
  const fields = new Map<string, IssueFieldPick>();
  for (const pick of watchedFields(watch)) {
    if (issueKey === undefined || pick.issueKey === issueKey) fields.set(fieldName(pick), pick);
  }
  return [...fields.values()].flatMap((pick) => {
    const showing = newestWrite(client, watch, pick);
    return showing === undefined ? [] : [showing];
  });
}

/** Records, as the value Dispatch last confirmed, how `data` - the answer of a read of `query` -
 *  shows each field the watch shows (`watchedFields`), where that answer is no older than what is
 *  recorded for the field: a read is as new as its request (`sentAt`), so one sent before a
 *  write of the field landed is older than that write's answer, whenever it arrives. A pick a
 *  reload or a sign-in after a lapse takes up starts with nothing recorded - its page holds
 *  nothing yet, or the lapse let go of every value confirmed (`reset`) - so only answers since
 *  count. Read before the pick is shown over the answer, which keeps the two apart. */
function confirmFrom(client: QueryClient, watch: Watch, query: Query, data: unknown): void {
  const at = sentAt.get(query) ?? 0;
  const looks = confirmedOf(client);
  for (const pick of watchedFields(watch)) {
    const name = fieldName(pick);
    const recorded = looks.get(name);
    if (recorded !== undefined && recorded.at > at) continue;
    const look = lookIn(query.queryKey, data, pick);
    if (look !== undefined) looks.set(name, { at, look });
  }
}

/** What one change of the client's cache does to its picks. An answer of a read, or a merge of
 *  another write's answer, on a cache a pick shows on has each field a pick of which is live shown
 *  again over it as the newest live write of that field - a read's answer first recorded as what
 *  Dispatch last confirmed where it is the newest answer (`confirmFrom`); an answer of a read that
 *  was out when a write of a field shown on it landed (`holdReads`) has that landed value shown
 *  again over it, and ends its hold, as a new read of that query, its failure or its cancelling
 *  does. */
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
  // A read cancelled (`cancelQueries`) is given up: the query takes back the state it had before
  // the read, idle, and the read's answer never lands.
  const givenUp = action.type === "setState" && query.state.fetchStatus === "idle";
  const held = watch.held.get(query.queryHash);
  if (
    held !== undefined &&
    (fetched || givenUp || action.type === "fetch" || action.type === "error")
  ) {
    watch.held.delete(query.queryHash);
  }
  if (action.type === "success" && cacheOf(query.queryKey) !== undefined) {
    const data = query.state.data;
    if (fetched) confirmFrom(client, watch, query, data);
    const showings = new Map(fetched && held !== undefined ? held : []);
    for (const showing of newestPicks(client, watch)) showings.set(fieldName(showing), showing);
    const next = shownOn(client, query.queryKey, data, [...showings.values()]);
    if (next !== data) put(client, query.queryKey, next);
  }
  release(client, watch);
}

/** Follows each live write of another kind that sets a field the watch shows - a board move to
 *  another lane, once its turn on its board has come and it has joined the issue's line - and
 *  each board move not yet answered that sets one, while it may still wait its turn: the pick's
 *  write shows each, as the newest write of that field, on caches the board does not own - the
 *  issue's detail, the Inbox. A followed write's end, which the store tells in the order of its
 *  line, shows each such field again (`ended`); a followed move is let go once it is answered. */
function follow(client: QueryClient, watch: Watch): void {
  const fields = new Map<string, IssueFieldPick>();
  for (const pick of watchedFields(watch)) fields.set(fieldName(pick), pick);
  const writes = writesOf(client);
  const moves = pendingMoves(client);
  for (const row of writes.rows()) {
    if (row.kind === KIND || !isLive(row) || watch.followed.has(row.id)) continue;
    const sets = [...fields.values()].flatMap((of) => {
      const pick = pickIn(writes, row, of);
      return pick === undefined ? [] : [pick];
    });
    if (sets.length > 0) watch.followed.set(row.id, sets);
  }
  for (const move of moves) {
    if (watch.moves.has(move)) continue;
    const sets = movePicks(move).filter((pick) => fields.has(fieldName(pick)));
    if (sets.length > 0) watch.moves.set(move, sets);
  }
}

/** What the end of a followed write does to each field it set, the store having let it go:
 *  landed, it is what Dispatch last confirmed for the field, which shows unless a newer live
 *  write of it does, and where none does, over the answer of each read out then as well
 *  (`holdReads`); refused, discarded or stopped, the field shows as its newest live write, or
 *  else as Dispatch last confirmed it, and the issue is read again (`putBack`). */
function ended(client: QueryClient, watch: Watch, outcome: WriteOutcome): void {
  const followed = watch.followed.get(outcome.write.id);
  if (followed === undefined) return;
  watch.followed.delete(outcome.write.id);
  for (const pick of followed) {
    if (outcome.kind !== "landed") {
      putBack(client, watch, pick);
      continue;
    }
    const look = confirmLanding(client, pick, outcome.answer);
    if (showNewestOr(client, watch, pick, look)) {
      holdReads(client, watch, { field: pick.field, issueKey: pick.issueKey, look });
    }
  }
  release(client, watch);
}

/** The client's watch, begun if it has none - and with the first, the stamping of each read of
 *  the client as it is sent (`stampReads`), which outlives every watch: of its cache; of its
 *  store, since a write of another kind that sets a field a pick shows can join the issue's line
 *  after the surface that made it has written its own cache, and of what becomes of each write it
 *  follows; and of its board moves - when each is made (`movesMade`), and each change of one,
 *  which shows the newest write of each field again. */
function watchOf(client: QueryClient): Watch {
  const current = watches.get(client);
  if (current !== undefined) return current;
  stampReads(client);
  const writes = writesOf(client);
  const watch: Watch = {
    followed: new Map(),
    held: new Map(),
    lastJoin: newestJoin(writes),
    live: new Map(),
    moves: new Map(),
    unsubscribe: () => {
      fromCache();
      fromStore();
      fromOutcomes();
      fromMoves();
    },
  };
  const fromCache = client.getQueryCache().subscribe((event) => answered(client, watch, event));
  const fromStore = writes.subscribe(() => {
    watch.lastJoin = Math.max(watch.lastJoin, newestJoin(writes));
    if (!writing) showEverywhere(client, newestPicks(client, watch));
    follow(client, watch);
    // A followed write the store let go of with no outcome - a sign-out, another person's
    // sign-in - is let go of here once the change that dropped it is done: the store tells an
    // outcome within the change that ends a write.
    queueMicrotask(() => {
      const held = new Set(writes.rows().map((row) => row.id));
      for (const id of watch.followed.keys()) if (!held.has(id)) watch.followed.delete(id);
      release(client, watch);
    });
  });
  const fromOutcomes = writes.onOutcome((outcome) => ended(client, watch, outcome));
  const fromMoves = client.getMutationCache().subscribe((event) => {
    // An observer's change of options names no mutation while the observer has made none,
    // whatever the event's type says.
    const move: BoardMove | undefined = event.mutation;
    if (move === undefined || !matchMutation({ mutationKey: boardMoves }, move)) return;
    if (event.type === "added") {
      // Made now: its variables are set as it starts, in the change that follows.
      movesMade.set(move, Math.max(watch.lastJoin, newestJoin(writes)));
      return;
    }
    if (event.type !== "updated" && event.type !== "removed") return;
    // Answered, its end was its write's, which the store told first (`ended`).
    if (event.type === "removed" || move.state.status !== "pending") watch.moves.delete(move);
    else follow(client, watch);
    if (!writing) showEverywhere(client, newestPicks(client, watch));
    release(client, watch);
  });
  watches.set(client, watch);
  return watch;
}

/** Ends the client's watch once it has no live pick, follows no write and no board move, and
 *  holds no read. */
function release(client: QueryClient, watch: Watch): void {
  const following = watch.followed.size > 0 || watch.moves.size > 0;
  if (watch.live.size > 0 || following || watch.held.size > 0) return;
  if (watches.get(client) !== watch) return;
  watches.delete(client);
  watch.unsubscribe();
}

/** The moments of the client's answers, in one order: each read's request, stamped as it is sent
 *  (`sentAt`) from the client's first pick on, whether a pick is live then or not (`stampReads`),
 *  and each landing of a write of a field. A read sent before the client's first pick has no
 *  stamp: it counts as sent before every landing, as it was. */
let clock = 0;
const sentAt = new WeakMap<Query, number>();

/** The clients whose reads are stamped as they are sent (`stampReads`). */
const stamped = new WeakSet<QueryClient>();

/** Stamps each read of the client with the moment it is sent (`sentAt`), from now on and for as
 *  long as the client lives - not with a watch, which ends once nothing is live: before the app's
 *  own read after a landing goes out (its `invalidate` comes first), and before every read sent
 *  while no pick is live. */
function stampReads(client: QueryClient): void {
  if (stamped.has(client)) return;
  stamped.add(client);
  client.getQueryCache().subscribe((event) => {
    if (event.type === "updated" && event.action.type === "fetch") {
      sentAt.set(event.query, ++clock);
    }
  });
}

/** How a field of an issue shows as Dispatch last confirmed it, with the moment of the answer it
 *  comes from (`clock`). */
interface Confirmed {
  readonly at: number;
  readonly look: Look;
}

/** How each field of each issue shows as Dispatch last confirmed it, per client: taken from the
 *  cache at the first pick of a run of picks of that field, as new as what was recorded before
 *  it, unless the cache shows a live write of the field of another kind there - a board move,
 *  which Dispatch has not stored (`started`); then from the newest answer, of a read of the
 *  issue, a list of issues or the Inbox while a watch is on (`confirmFrom`), or of a write of the
 *  field that landed (`confirmLanding`). A refused, discarded or dropped pick or followed write
 *  that leaves no live write of its field puts this back. */
let confirmed = new WeakMap<QueryClient, Map<string, Confirmed>>();

function confirmedOf(client: QueryClient): Map<string, Confirmed> {
  let looks = confirmed.get(client);
  if (looks === undefined) {
    looks = new Map();
    confirmed.set(client, looks);
  }
  return looks;
}

/** Records `answer`, Dispatch's answer to a write of `pick`'s field that landed, as the value it
 *  last confirmed for that field, newer than every read sent before now, and answers it; a
 *  landing Dispatch kept no answer for stored the write's value all the same. */
function confirmLanding(client: QueryClient, pick: IssueFieldPick, answer: unknown): Look {
  const looks = confirmedOf(client);
  const name = fieldName(pick);
  const look =
    answer === null
      ? { fields: { ...looks.get(name)?.look.fields, ...pickLook(pick).fields } }
      : issueLook(pick.field, answer as Issue);
  looks.set(name, { at: ++clock, look });
  return look;
}

/** Has the client read again what a pick of `issueKey` showed on, as Dispatch now holds it. */
function readAgain(client: QueryClient, issueKey: string): void {
  void client.invalidateQueries({ queryKey: ["issue", issueKey] });
  void client.invalidateQueries({ queryKey: ["issues"] });
  void client.invalidateQueries({ queryKey: inboxKey });
}

/** Holds each read out now on a cache a pick shows on, which may have been composed before
 *  `landed`, the answer of a write of its field that landed, and so answer with the value from
 *  before it: over that answer the landed value shows again (`answered`). The client's watch,
 *  begun where there is none, holds them until each answers or is cancelled. */
function holdReads(client: QueryClient, watch: Watch | undefined, landed: Showing): void {
  const reading = client
    .getQueryCache()
    .getAll()
    .filter((query) => query.state.fetchStatus !== "idle" && cacheOf(query.queryKey) !== undefined);
  if (reading.length === 0) return;
  const holding = watch ?? watchOf(client);
  const name = fieldName(landed);
  for (const query of reading) {
    const showings = holding.held.get(query.queryHash) ?? new Map<string, Showing>();
    showings.set(name, landed);
    holding.held.set(query.queryHash, showings);
  }
}

/** Shows `of`'s field as its newest live write, or, where none is left, as `fallback` where it
 *  is given; each other field of the issue a pick of which is live shows as its newest live write
 *  too. Whether no live write of the field is left. */
function showNewestOr(
  client: QueryClient,
  watch: Watch | undefined,
  of: Pick<IssueFieldPick, "field" | "issueKey">,
  fallback: Look | undefined
): boolean {
  const live = watch === undefined ? [] : newestPicks(client, watch, of.issueKey);
  const newest = newestWrite(client, watch, of)?.look;
  const look = newest ?? fallback;
  showEverywhere(
    client,
    look === undefined ? live : [...live, { field: of.field, issueKey: of.issueKey, look }]
  );
  return newest === undefined;
}

/** Shows `of`'s field as its newest live write, or, where none is left, as Dispatch last
 *  confirmed it, and reads the issue again then; each other field of the issue a pick of which is
 *  live shows as its newest live write too. What a pick or a followed write that ended without
 *  landing leaves. */
function putBack(
  client: QueryClient,
  watch: Watch | undefined,
  of: Pick<IssueFieldPick, "field" | "issueKey">
): void {
  const look = confirmed.get(client)?.get(fieldName(of))?.look;
  if (showNewestOr(client, watch, of, look)) readAgain(client, of.issueKey);
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
 * then on, while a pick of a field is live, the field shows as the newest live write of it in the
 * person's order (`fieldWritesOf`), whichever control made it - this picker, another, the issue
 * page's keys, a board move to another lane, which counts from its drop although it may wait its
 * turn on its board behind another card's move (`newestWrite`) - over every answer that lands on
 * one of those caches, a read's or another write's merged (`mergeIssue`), the first after a
 * reload among them, and again whenever the store's writes or the board's moves change. Nothing
 * waits and no read is cancelled: each answer is taken whole, and only the field is put back over
 * it. What this module writes itself is no answer (`writing`), and it writes nothing where the
 * cache already shows the value. A live write of another kind that sets such a field, a board
 * move above all, is followed (`follow`) until it ends, since it shows on caches its surface does
 * not own: landed, its answer is the value Dispatch last confirmed, which shows where no live
 * write of the field is left, over the answer of each read out at the landing too; refused,
 * discarded or stopped, the field shows as its newest live write, or else as Dispatch last
 * confirmed it, and the issue is read again (`ended`).
 *
 * Landed, it merges Dispatch's answer and records it as the value Dispatch last confirmed. Where
 * a live write of the field is left, the newest of them shows. Where none is, the landed value
 * shows and the issue, the lists and the Inbox are read again; a read that was out at the landing
 * may have been composed before it, and its answer can still come back with the old value
 * (`DispatchQueryClient` cancels no read, and reads again once it is in), so over the answer of
 * each read out at the landing the landed value shows again, and a read that begins after the
 * landing answers for itself. Refused, discarded or dropped (`undone`), the newest live write of
 * the field shows, so a pick a newer one took out puts nothing back; where none is left, the
 * value Dispatch last confirmed (`confirmed`) shows and the caches are read again (`putBack`).
 * That value is the field in the newest answer Dispatch gave, kept apart from the pick shown over
 * it: what the caches showed at the first pick of a run, unless a live write of the field of
 * another kind put it there, then each newer answer of a read (`confirmFrom`) or of a write of
 * the field that landed (`confirmLanding`), a read being as new as its request, whether a pick
 * was live when it was sent or not, so one sent before a landing is older than it whenever it
 * arrives. After a reload, or a lapse that let go of every value confirmed, the caches hold
 * nothing at the first pick, so only the answers since count. The watch ends once no pick is
 * live, no board move is followed and no read is held. Whoever is signed in changing lets go of
 * every value confirmed (`reset`).
 */
export const issueFieldWrite = defineWriteKind<IssueFieldPick>({
  kept: keptPick,
  landed: (client, write, answer) => {
    const watch = watches.get(client);
    watch?.live.delete(write.id);
    const { detail } = write;
    const look = confirmLanding(client, detail, answer);
    if (answer !== null) asOwnWrite(() => mergeIssue(client, answer as Issue));
    if (showNewestOr(client, watch, detail, look)) {
      holdReads(client, watch, { field: detail.field, issueKey: detail.issueKey, look });
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
    // last answered, as new as anything recorded before - unless it is what a live write of the
    // field of another kind sets, a board move, which put it there and which Dispatch has not
    // stored: what is recorded then stays, and where nothing is, the first read that answers is
    // (`confirmFrom`). A write already live - one a lapse put back in its line - begins no run.
    if (![...watch.live.values()].some((live) => sameField(live.detail, detail))) {
      const look = cachedLook(client, detail);
      const shown = look?.fields[detail.field];
      const unsaved = fieldWritesOf(client, detail).some(
        (other) => other.live && !other.ofPick && other.pick.value === shown
      );
      const looks = confirmedOf(client);
      const name = fieldName(detail);
      if (look !== undefined && !unsaved) looks.set(name, { at: looks.get(name)?.at ?? 0, look });
    }
    watch.live.set(write.id, write);
    follow(client, watch);
    showEverywhere(client, newestPicks(client, watch, detail.issueKey));
  },
  undone: (client, write) => {
    const watch = watches.get(client);
    watch?.live.delete(write.id);
    putBack(client, watch, write.detail);
    if (watch !== undefined) release(client, watch);
  },
});
