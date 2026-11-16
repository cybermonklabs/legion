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

/** Every field of an issue a pick changes. */
const everyField: readonly IssueField[] = ["assignee", "priority", "repo"];

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

/** How `field` shows as `issue` - Dispatch's answer, a cache's copy of it, or what an answer kept
 *  carries of it (`pickedFields`) - carries it. */
function issueLook(field: IssueField, issue: Partial<IssueFields>): Look {
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

/** The fields of `issue` a pick changes, copied apart from the rest of it, which a kept answer
 *  then does not hold. */
function pickedFields({ assignee, priority, repo, resolved_repo }: IssueFields): IssueFields {
  return { assignee, priority, repo, resolved_repo };
}

/** `answer`, Dispatch's answer to a write, as an issue: an object whose `key` names the issue and
 *  that carries each field of it a pick changes as its own. Undefined for any other answer - for
 *  `null`, a landing Dispatch kept no answer for, among them. */
function issueAnswer(answer: unknown): (IssueFields & Pick<Issue, "key">) | undefined {
  if (typeof answer !== "object" || answer === null) return undefined;
  const carried = ["key", "assignee", "priority", "repo", "resolved_repo"].every((name) =>
    Object.hasOwn(answer, name)
  );
  if (!carried || typeof Reflect.get(answer, "key") !== "string") return undefined;
  return answer as IssueFields & Pick<Issue, "key">;
}

/** The person each of `rows`, the Inbox rows of one issue, waits on, by the row's id. */
function waitingOf(rows: readonly InboxRow[]): ReadonlyMap<string, string | undefined> {
  return new Map(rows.map((each) => [each.id, each.waiting_on_person]));
}

/** How `rows`, the Inbox rows of one issue, show the fields of the issue a pick changes: as the
 *  first of them carries them - its priority, and its issue's assignee where it names its issue -
 *  with the person each of them waits on. Undefined where there is no row. */
function inboxLook(rows: readonly InboxRow[]): Look | undefined {
  const [row] = rows;
  if (row === undefined) return undefined;
  const { issue, priority } = row;
  return {
    fields: issue === undefined ? { priority } : { assignee: issue.assignee, priority },
    waiting: waitingOf(rows),
  };
}

/** `look`, of each field of an issue a pick changes that it carries, cut to `field`; for the
 *  assignee, with the person each Inbox row waits on where `look` names them. Undefined where
 *  `look` does not carry the field. */
function fieldLook(look: Look, field: IssueField): Look | undefined {
  if (!Object.hasOwn(look.fields, field)) return undefined;
  const cut = issueLook(field, look.fields);
  return field === "assignee" ? { ...cut, waiting: look.waiting } : cut;
}

/** How `field` shows as `issue` - an issue's detail or its row of a list of issues - carries it,
 *  else as the first of `rows`, the issue's Inbox rows, does (`inboxLook`); for the assignee,
 *  with the person each of those rows waits on. Undefined where neither carries the field. */
function lookOf(
  field: IssueField,
  issue: IssueFields | undefined,
  rows: readonly InboxRow[]
): Look | undefined {
  if (issue === undefined) {
    const look = inboxLook(rows);
    return look === undefined ? undefined : fieldLook(look, field);
  }
  const look = issueLook(field, issue);
  if (field !== "assignee") return look;
  return { ...look, waiting: waitingOf(rows) };
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

/** How each issue `data` holds shows the fields a pick changes, by issue key, where `data` is
 *  what the query under `queryKey` holds: an issue's detail; a list of issues, each issue as its
 *  first row there carries them; or the Inbox, each issue as its rows carry them (`inboxLook`).
 *  All a put-back takes from Dispatch's answer of that read (`confirmedOf`), kept apart from the
 *  rest of it. */
function answeredLooks(queryKey: QueryKey, data: unknown): Map<string, Look> {
  const looks = new Map<string, Look>();
  const cache = cacheOf(queryKey);
  const [, detailOf] = queryKey;
  if (cache === "detail") {
    if (typeof detailOf === "string" && typeof data === "object" && data !== null) {
      looks.set(detailOf, { fields: pickedFields(data as IssueDetails) });
    }
    return looks;
  }
  if (!Array.isArray(data)) return looks;
  if (cache === "list") {
    for (const row of data as IssueSummary[]) {
      if (!looks.has(row.key)) looks.set(row.key, { fields: pickedFields(row) });
    }
    return looks;
  }
  if (cache !== "inbox") return looks;
  const rows = new Map<string, InboxRow[]>();
  for (const row of data as InboxRow[]) {
    if (row.issue_key === null) continue;
    const ofIssue = rows.get(row.issue_key);
    if (ofIssue === undefined) rows.set(row.issue_key, [row]);
    else ofIssue.push(row);
  }
  for (const [issueKey, ofIssue] of rows) {
    const look = inboxLook(ofIssue);
    if (look !== undefined) looks.set(issueKey, look);
  }
  return looks;
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

/** What a client's picks are doing, for as long as the client lives: every live one - joined its
 *  line, and neither landed nor refused, discarded, dropped or stopped - by write id; each live
 *  write of another kind that sets a field the watch shows (`follow`), by write id, and each
 *  board move not yet answered that sets one, each with what it sets; each query whose read out
 *  is older than the value Dispatch last confirmed for a field shown on it, as a landing of a
 *  write of the field or a put back left it (`holdReads`), by hash, with what to put back over
 *  the answer of that read; and the store whose writes these are. */
interface Watch {
  readonly followed: Map<number, readonly IssueFieldPick[]>;
  readonly held: Map<string, Map<string, Showing>>;
  readonly live: Map<number, Write<IssueFieldPick>>;
  readonly moves: Map<BoardMove, readonly IssueFieldPick[]>;
  readonly writes: Writes;
}

const watches = new WeakMap<QueryClient, Watch>();

/** Whether the watch has a field to show: a pick is live, or a write or a board move is
 *  followed. */
function watching(watch: Watch): boolean {
  return watch.live.size > 0 || watch.followed.size > 0 || watch.moves.size > 0;
}

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
  return everyField.flatMap((field) => {
    const pick = fieldIn(change.input, change.issueKey, field);
    return pick === undefined ? [] : [pick];
  });
}

/** Every board move of the client not yet answered, in the order they were made: waiting its
 *  turn in its board's scope - its write not yet in the store (`state.isPaused`) - or out. */
function pendingMoves(client: QueryClient): BoardMove[] {
  return client.getMutationCache().findAll({ mutationKey: boardMoves, status: "pending" });
}

/** The place in the person's order of a board move still waiting its turn on its board: after
 *  every write on its issue's line, whose places count up from one (`Write.joined`). */
const WAITING = Number.POSITIVE_INFINITY;

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

/** Every write of `of`'s field the client holds, of any kind, each at its place in the person's
 *  order: each write on the issue's line whose request sets the field - a pick, a key of the
 *  issue page, a board move whose turn on its board has come - at its place in the line
 *  (`Write.joined`); then each board move of the issue still waiting its turn in its board's
 *  scope behind another move (`state.isPaused`), after every write on the line (`WAITING`), in
 *  the order they were made. So a move made after a pick is newer than it from its drop, and of
 *  two waiting moves the later made is newer. A pick made while a move of its issue still waits
 *  is older than that move too: the pick goes out first, and Dispatch stores the move's lane
 *  last. */
export function fieldWritesOf(
  client: QueryClient,
  of: Pick<IssueFieldPick, "field" | "issueKey">
): FieldWrite[] {
  const { writes } = watchOf(client);
  const found: FieldWrite[] = [];
  for (const row of writes.rows()) {
    const pick = pickIn(writes, row, of);
    if (pick === undefined) continue;
    const ofPick = row.kind === KIND;
    found.push({ id: row.id, live: isLive(row), ofPick, pick, rank: row.joined });
  }
  for (const move of pendingMoves(client)) {
    if (!move.state.isPaused) continue;
    const pick = movePicks(move).find((each) => sameField(each, of));
    if (pick === undefined) continue;
    found.push({ id: undefined, live: true, ofPick: false, pick, rank: WAITING });
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
  watch: Watch,
  of: Pick<IssueFieldPick, "field" | "issueKey">
): Showing | undefined {
  let newest: { readonly pick: IssueFieldPick; readonly rank: number } | undefined;
  for (const write of watch.live.values()) {
    if (!sameField(write.detail, of)) continue;
    if (newest === undefined || write.joined > newest.rank) {
      newest = { pick: write.detail, rank: write.joined };
    }
  }
  for (const write of fieldWritesOf(client, of)) {
    // A pick is read from the watch's live picks alone: the store shows it a moment before its
    // kind is told of it (`started`), which records first what the cache showed before it. Of
    // two moves still waiting, which share one place, the later made is newer.
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

/** The moments of the client's answers, in one order: each read's request, stamped as it is sent
 *  (`sentAt`) for as long as the client's watch lives, whether a pick is live then or not, and
 *  each landing of a write (`confirmLanding`). A read sent before the watch began has no stamp:
 *  it counts as sent before every landing, as it was. */
let clock = 0;
const sentAt = new WeakMap<Query, number>();

/** A read of an issue's detail, a list of issues or the Inbox as Dispatch answered it, cut to how
 *  each issue it holds shows the fields a pick changes (`answeredLooks`), with the moment its
 *  request went out (`sentAt`). */
interface Read {
  readonly at: number;
  readonly looks: ReadonlyMap<string, Look>;
}

/** How a field of an issue shows as Dispatch last confirmed it, with the moment of the answer it
 *  comes from (`clock`). */
interface Confirmed {
  readonly at: number;
  readonly look: Look;
}

/** What Dispatch answered a client, kept apart from the picks shown over it: what a put-back
 *  takes from the last answer of each read of an issue's detail, a list of issues or the Inbox -
 *  of each issue it holds, the fields a pick changes, and of the Inbox, the person each row waits
 *  on (`answeredLooks`) - by the hash of its query, in the order they answered, for as long as the
 *  client holds that query (`keepAnswer`); the answer of the last write that landed carrying each
 *  field, of any kind and whatever field it set, by field (`confirmLanding`); and how each field
 *  showed in the caches at the first pick of a run of picks of it (`started`), which stands in
 *  for an answer only where the client keeps none (`confirmedOf`). */
interface Answers {
  readonly landings: Map<string, Confirmed>;
  readonly reads: Map<string, Read>;
  readonly shown: Map<string, Look>;
}

let answers = new WeakMap<QueryClient, Answers>();

function answersOf(client: QueryClient): Answers {
  let kept = answers.get(client);
  if (kept === undefined) {
    kept = { landings: new Map(), reads: new Map(), shown: new Map() };
    answers.set(client, kept);
  }
  return kept;
}

/** Keeps what a put-back takes from the answer of the read of `query` that just landed, on a
 *  cache a pick shows on (`answeredLooks`), with the moment its request went out. The answer is
 *  the one the read returned (`query.promise`, which the fetch settles before it puts the answer
 *  in the cache), not the query's data, which keeps it apart from the picks shown over it: the
 *  issue's query keeps the issue it holds, picks and all, over an answer older than that issue
 *  (`issueQuery`). That promise hands the answer on a moment later, so the moment and the record
 *  it goes in are taken now: an answer given before whoever is signed in changed is not kept for
 *  the person after (`reset`). */
function keepAnswer(client: QueryClient, query: Query): void {
  const at = sentAt.get(query) ?? 0;
  const { reads } = answersOf(client);
  const { queryHash, queryKey } = query;
  void query.promise?.then((data) => {
    if (client.getQueryCache().get(queryHash) !== query) return;
    // Last in the order the reads answered: of two with no stamp, the later is the newer.
    reads.delete(queryHash);
    reads.set(queryHash, { at, looks: answeredLooks(queryKey, data) });
  });
}

/** How `of`'s field shows as Dispatch last confirmed it: the field in the newest answer Dispatch
 *  gave the client for the issue - the answer of the last write that landed carrying the field,
 *  whatever field that write set, or of a read that holds the issue, a read being as new as its
 *  request, so one sent before a write landed is older than that write's answer, whenever it
 *  arrived. An assignee comes with the person each Inbox row of the issue waits on as the newest
 *  Inbox answer for the issue names them, whichever answer the assignee comes from, where that
 *  Inbox answer holds the same assignee: Dispatch's answer for a row nobody handed on, which
 *  waits on the project's default while the issue has no assignee. Where the client keeps neither
 *  - its caches were written by hand - what they showed at the first pick of the run; undefined
 *  where not even that is known. */
function confirmedOf(
  client: QueryClient,
  of: Pick<IssueFieldPick, "field" | "issueKey">
): Confirmed | undefined {
  const kept = answers.get(client);
  if (kept === undefined) return undefined;
  const name = fieldName(of);
  let newest = kept.landings.get(name);
  // Of the answers that hold the issue, only the Inbox's name who its rows wait on (`waiting`).
  let inbox: Confirmed | undefined;
  for (const { at, looks } of kept.reads.values()) {
    const answered = looks.get(of.issueKey);
    if (answered === undefined) continue;
    const waits = of.field === "assignee" && answered.waiting !== undefined;
    if (waits && (inbox === undefined || at >= inbox.at)) inbox = { at, look: answered };
    if (newest !== undefined && at < newest.at) continue;
    const look = fieldLook(answered, of.field);
    if (look !== undefined) newest = { at, look };
  }
  if (newest === undefined) {
    const shown = kept.shown.get(name);
    return shown === undefined ? undefined : { at: 0, look: shown };
  }
  if (
    inbox === undefined ||
    newest.look.waiting !== undefined ||
    inbox.look.fields.assignee !== newest.look.fields.assignee
  ) {
    return newest;
  }
  return { ...newest, look: { ...newest.look, waiting: inbox.look.waiting } };
}

/** Keeps what the landing of a write - of any kind, whatever field of an issue it set - told the
 *  client, as the answer of the last write that landed carrying each field it tells, newer than
 *  every read sent before now; and answers what that landing confirmed of each of `sets`, fields
 *  the write set, in their order. `answer`, where it is an issue (`issueAnswer`), tells each field
 *  of it a pick changes; a landing that tells nothing of a field of `sets` - one Dispatch kept no
 *  answer for, `null` - stored the value set all the same. */
function confirmLanding(
  client: QueryClient,
  answer: unknown,
  sets: readonly IssueFieldPick[]
): Confirmed[] {
  const at = ++clock;
  const { landings } = answersOf(client);
  const issue = issueAnswer(answer);
  if (issue !== undefined) {
    for (const field of everyField) {
      const name = fieldName({ field, issueKey: issue.key });
      landings.set(name, { at, look: issueLook(field, issue) });
    }
  }
  return sets.map((pick) => {
    if (issue?.key === pick.issueKey) return { at, look: issueLook(pick.field, issue) };
    const fields = { ...confirmedOf(client, pick)?.look.fields, ...pickLook(pick).fields };
    const landing = { at, look: { fields } };
    landings.set(fieldName(pick), landing);
    return landing;
  });
}

/** Has the client read again what a pick of `issueKey` showed on, as Dispatch now holds it. */
function readAgain(client: QueryClient, issueKey: string): void {
  void client.invalidateQueries({ queryKey: ["issue", issueKey] });
  void client.invalidateQueries({ queryKey: ["issues"] });
  void client.invalidateQueries({ queryKey: inboxKey });
}

/** Holds each read out now on a cache a pick shows on that was sent before the answer `record`
 *  comes from, the value Dispatch last confirmed for `of`'s field - a landing's answer, or a
 *  read's: such a read is older than that answer, whenever it arrives, and can answer with the
 *  value from before it, so over its answer the confirmed value shows again (`answered`), until
 *  it answers or is given up. */
function holdReads(
  client: QueryClient,
  watch: Watch,
  of: Pick<IssueFieldPick, "field" | "issueKey">,
  record: Confirmed
): void {
  const name = fieldName(of);
  const showing: Showing = { field: of.field, issueKey: of.issueKey, look: record.look };
  for (const query of client.getQueryCache().getAll()) {
    if (
      query.state.fetchStatus === "idle" ||
      cacheOf(query.queryKey) === undefined ||
      (sentAt.get(query) ?? 0) >= record.at
    ) {
      continue;
    }
    const showings = watch.held.get(query.queryHash) ?? new Map<string, Showing>();
    showings.set(name, showing);
    watch.held.set(query.queryHash, showings);
  }
}

/** Shows `of`'s field as its newest live write, or, where none is left, as `fallback` where it
 *  is given; each other field of the issue a pick of which is live shows as its newest live write
 *  too. Whether no live write of the field is left. */
function showNewestOr(
  client: QueryClient,
  watch: Watch,
  of: Pick<IssueFieldPick, "field" | "issueKey">,
  fallback: Look | undefined
): boolean {
  const live = newestPicks(client, watch, of.issueKey);
  const newest = newestWrite(client, watch, of)?.look;
  const look = newest ?? fallback;
  showEverywhere(
    client,
    look === undefined ? live : [...live, { field: of.field, issueKey: of.issueKey, look }]
  );
  return newest === undefined;
}

/** Shows `of`'s field as its newest live write, or, where none is left, as Dispatch last
 *  confirmed it (`confirmedOf`) - over the answer of each read out then that is older than that
 *  value as well (`holdReads`) - and reads the issue again then; each other field of the issue a
 *  pick of which is live shows as its newest live write too. What a pick or a followed write
 *  that ended without landing leaves. */
function putBack(
  client: QueryClient,
  watch: Watch,
  of: Pick<IssueFieldPick, "field" | "issueKey">
): void {
  const record = confirmedOf(client, of);
  if (!showNewestOr(client, watch, of, record?.look)) return;
  if (record !== undefined) holdReads(client, watch, of, record);
  readAgain(client, of.issueKey);
}

/** What one change of the client's cache does. Each read is stamped as it is sent (`sentAt`),
 *  and the answer of each read of a cache a pick shows on is kept as Dispatch gave it
 *  (`keepAnswer`), whether a pick is live or not; a query the client lets go of takes its answer
 *  and its hold with it. While the watch shows a field or holds a read: an answer of a read, or a
 *  merge of another write's answer, on such a cache has each field a pick of which is live shown
 *  again over it as the newest live write of that field; an answer of a read older than the
 *  value a landing or a put back left for a field shown on it (`holdReads`) has that value shown
 *  again over it, and ends its hold, as a new read of that query, its failure or its cancelling
 *  does. */
function answered(client: QueryClient, watch: Watch, event: QueryCacheNotifyEvent): void {
  if (writing) return;
  const { query } = event;
  if (event.type === "removed") {
    watch.held.delete(query.queryHash);
    answers.get(client)?.reads.delete(query.queryHash);
    return;
  }
  if (event.type !== "updated") return;
  const { action } = event;
  if (action.type === "fetch") sentAt.set(query, ++clock);
  const fetched = action.type === "success" && action.manual !== true;
  const drawn = action.type === "success" && cacheOf(query.queryKey) !== undefined;
  if (fetched && drawn) keepAnswer(client, query);
  if (watch.held.size === 0 && !watching(watch)) return;
  // A read cancelled (`cancelQueries`) is given up: the query takes back the state it had before
  // the read, idle, and the read's answer never lands. Its hold ends here only to free it: the
  // query's next read, sent from idle, ends it as it is sent (`fetch`), before any answer lands.
  const givenUp = action.type === "setState" && query.state.fetchStatus === "idle";
  const held = watch.held.get(query.queryHash);
  if (
    held !== undefined &&
    (fetched || givenUp || action.type === "fetch" || action.type === "error")
  ) {
    watch.held.delete(query.queryHash);
  }
  if (!drawn) return;
  const data = query.state.data;
  const showings = new Map(fetched && held !== undefined ? held : []);
  for (const showing of newestPicks(client, watch)) showings.set(fieldName(showing), showing);
  const next = shownOn(client, query.queryKey, data, [...showings.values()]);
  if (next !== data) put(client, query.queryKey, next);
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
  const { writes } = watch;
  for (const row of writes.rows()) {
    if (row.kind === KIND || !isLive(row) || watch.followed.has(row.id)) continue;
    const sets = [...fields.values()].flatMap((of) => {
      const pick = pickIn(writes, row, of);
      return pick === undefined ? [] : [pick];
    });
    if (sets.length > 0) watch.followed.set(row.id, sets);
  }
  for (const move of pendingMoves(client)) {
    if (watch.moves.has(move)) continue;
    const sets = movePicks(move).filter((pick) => fields.has(fieldName(pick)));
    if (sets.length > 0) watch.moves.set(move, sets);
  }
}

/** What the end of a write does, the store having let it go. Landed, what its landing told the
 *  client is kept, whatever field of an issue it set (`confirmLanding`) - a pick's was kept as it
 *  landed (`landed`), before the store told its end - and each field the watch followed it for is
 *  what Dispatch last confirmed for the field, which shows unless a newer live write of it does,
 *  and where none does, over the answer of each read out then as well (`holdReads`); refused,
 *  discarded or stopped, each such field shows as its newest live write, or else as Dispatch last
 *  confirmed it, over each older read out then as well, and the issue is read again
 *  (`putBack`). */
function ended(client: QueryClient, watch: Watch, outcome: WriteOutcome): void {
  const followed = watch.followed.get(outcome.write.id) ?? [];
  watch.followed.delete(outcome.write.id);
  if (outcome.kind !== "landed") {
    for (const pick of followed) putBack(client, watch, pick);
    return;
  }
  if (outcome.write.kind === KIND) return;
  const landings = confirmLanding(client, outcome.answer, followed);
  for (const [index, pick] of followed.entries()) {
    const landing = landings[index];
    if (showNewestOr(client, watch, pick, landing.look)) holdReads(client, watch, pick, landing);
  }
}

/** What the landing of `move`, a board move, with no answer leaves - one Dispatch kept no answer
 *  for, whose end in the store tells nothing of what it set, the store having let go of its
 *  request: each field the move set is stored all the same, as the last write of it that landed
 *  (`confirmLanding`) - but for a field the watch followed it for, whose landing the end of its
 *  write in the store left first (`ended`). A landing with Dispatch's answer was kept as the end
 *  of the move's write (`ended`). */
function landedMove(client: QueryClient, watch: Watch, move: BoardMove): void {
  const followed = watch.moves.get(move) ?? [];
  const sets = movePicks(move).filter((pick) => !followed.some((each) => sameField(each, pick)));
  confirmLanding(client, null, sets);
}

/** Begins the watch of `client`, whose store `writes` is, where it has none, for as long as the
 *  client lives: of its cache - each read's request and answer, and what each answer does to the
 *  picks shown (`answered`); of its store, since a write of another kind that sets a field a pick
 *  shows can join the issue's line after the surface that made it has written its own cache, and
 *  of what becomes of each write - what its landing told, and each field it was followed for
 *  (`ended`); and of its board moves, each change of which shows the newest write of each field
 *  again, and each landing with no answer of which stored each field it set (`landedMove`). Its
 *  listeners hear every change of the app, so while the watch shows no field and holds no read
 *  each one returns at once - but for the cache's, which still stamps a read sent and keeps an
 *  answer given, the store's outcomes, which still keep what a landing told, and the board
 *  moves', which still keep a move's landing with no answer. */
function begin(client: QueryClient, writes: Writes): Watch {
  const begun = watches.get(client);
  if (begun !== undefined) return begun;
  const watch: Watch = {
    followed: new Map(),
    held: new Map(),
    live: new Map(),
    moves: new Map(),
    writes,
  };
  watches.set(client, watch);
  client.getQueryCache().subscribe((event) => answered(client, watch, event));
  writes.subscribe(() => {
    if (!watching(watch)) return;
    if (!writing) showEverywhere(client, newestPicks(client, watch));
    follow(client, watch);
    if (watch.followed.size === 0) return;
    // A followed write the store let go of with no outcome - a sign-out, another person's
    // sign-in - is let go of here once the change that dropped it is done: the store tells an
    // outcome within the change that ends a write.
    queueMicrotask(() => {
      const held = new Set(writes.rows().map((row) => row.id));
      for (const id of watch.followed.keys()) if (!held.has(id)) watch.followed.delete(id);
    });
  });
  writes.onOutcome((outcome) => ended(client, watch, outcome));
  client.getMutationCache().subscribe((event) => {
    // An observer's change of options names no mutation while the observer has made none,
    // whatever the event's type says.
    const move: BoardMove | undefined = event.mutation;
    if (move === undefined || (event.type !== "updated" && event.type !== "removed")) return;
    const unanswered =
      event.type === "updated" && event.action.type === "success" && event.action.data === null;
    if (!unanswered && !watching(watch)) return;
    if (!matchMutation({ mutationKey: boardMoves }, move)) return;
    if (unanswered) landedMove(client, watch, move);
    if (!watching(watch)) return;
    // Answered, its end was its write's, which the store told first (`ended`).
    if (event.type === "removed" || move.state.status !== "pending") watch.moves.delete(move);
    else follow(client, watch);
    if (!writing) showEverywhere(client, newestPicks(client, watch));
  });
  return watch;
}

/** The client's watch: the one its store began as it was made (`opened`), or, for a store made
 *  before this kind was defined, one begun now. */
function watchOf(client: QueryClient): Watch {
  return watches.get(client) ?? begin(client, writesOf(client));
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
 * page's keys, a board move to another lane, which is the newest while it still waits its turn on
 * its board behind another move and counts from its place in the issue's line once it has joined
 * it - over every answer that lands on one of those caches, a read's or another write's merged
 * (`mergeIssue`), the first after a reload among them, and again whenever the store's writes or
 * the board's moves change. Nothing waits and no read is cancelled: each answer is taken whole,
 * and only the field is put back over it. What this module writes itself is no answer
 * (`writing`), and it writes nothing where the cache already shows the value. A live write of
 * another kind that sets such a field, a board move above all, is followed (`follow`) until it
 * ends, since it shows on caches its surface does not own: landed, its answer is the value
 * Dispatch last confirmed, which shows where no live write of the field is left, over the answer
 * of each read out at the landing too; refused, discarded or stopped, the field shows as its
 * newest live write, or else as Dispatch last confirmed it, over the answer of each read out
 * then that is older than that value too, and the issue is read again (`ended`).
 *
 * Landed, it merges Dispatch's answer, the whole issue, and keeps it as the answer of the last
 * write that landed carrying each field of the issue a pick changes, its own and the others.
 * Where a live write of the field is left, the newest of them shows. Where none is, the landed
 * value shows and the issue, the lists and the Inbox are read again; a read that was out at the
 * landing may have been composed before it, and its answer can still come back with the old
 * value (`DispatchQueryClient` cancels no read, and reads again once it is in), so over the
 * answer of each read out at the landing the landed value shows again, and a read that begins
 * after the landing answers for itself. Refused, discarded or dropped (`undone`), a pick that
 * was shown - it had joined its line, and no earlier refusal of it put a value back - leaves the
 * newest live write of the field showing, so a pick a newer one took out puts nothing back; where
 * none is left, the value Dispatch last confirmed shows, over the answer of each read out then
 * that was sent before the answer that value comes from too, and the caches are read again
 * (`putBack`). A pick that never joined its line - one a reload kept refused, for the reader to
 * Retry or Discard - was never shown, and puts nothing back when it goes.
 *
 * The value Dispatch last confirmed is the field in the newest answer Dispatch gave the client
 * for the issue, kept apart from the pick shown over it (`confirmedOf`): the answer of the last
 * write that landed carrying the field, of any kind and whatever field it set - a pick of another
 * field, a save of the title in the issue's header, a board move, a claim released, an edit from
 * the issues table - which is the issue (`confirmLanding`, `ended`), and of a write of the field
 * that landed with no such answer, the value it set (`landedMove` for a board move); or of a read
 * of the issue's detail, a list of issues or the Inbox that holds the issue, of which the fields
 * a pick changes are kept as the read returned them, before the issue's query keeps an older one
 * out or a pick is shown over it, from the moment the client's store is made (`opened`,
 * `keepAnswer`) - a read being as new as its request, whether a pick was live when it was sent
 * or not, so one sent before a landing is older than it whenever it arrives. An assignee put back
 * comes with the person each Inbox row of the issue waits on as the newest Inbox answer named
 * them, where that answer holds the same assignee. Only where the client keeps no such answer -
 * its caches were written by hand - it is what the caches showed at the first pick of a run,
 * unless a live write of the field of another kind put it there. After a reload the client holds
 * no answer yet, and whoever is signed in changing lets go of every answer kept (`reset`) - once
 * the picks that change drops have each put back what Dispatch last confirmed - so only the
 * answers since count.
 */
export const issueFieldWrite = defineWriteKind<IssueFieldPick>({
  kept: keptPick,
  landed: (client, write, answer) => {
    const watch = watchOf(client);
    watch.live.delete(write.id);
    const { detail } = write;
    const [landing] = confirmLanding(client, answer, [detail]);
    if (answer !== null) asOwnWrite(() => mergeIssue(client, answer as Issue));
    if (showNewestOr(client, watch, detail, landing.look)) {
      holdReads(client, watch, detail, landing);
      readAgain(client, detail.issueKey);
    }
  },
  name: KIND,
  opened: begin,
  reset: () => {
    answers = new WeakMap();
  },
  started: (client, write) => {
    const watch = watchOf(client);
    const { detail } = write;
    // The first pick of a run of picks of this field: what the cache shows now stands for what
    // Dispatch last confirmed where the client keeps no answer of Dispatch's for the issue
    // (`confirmedOf`) - unless it is what a live write of the field of another kind sets, a board
    // move, which put it there and which Dispatch has not stored: what was shown at an earlier
    // run then stays. A write already live - one a lapse put back in its line - begins no run.
    if (![...watch.live.values()].some((live) => sameField(live.detail, detail))) {
      const look = cachedLook(client, detail);
      const shown = look?.fields[detail.field];
      const unsaved = fieldWritesOf(client, detail).some(
        (other) => other.live && !other.ofPick && other.pick.value === shown
      );
      if (look !== undefined && !unsaved) answersOf(client).shown.set(fieldName(detail), look);
    }
    watch.live.set(write.id, write);
    follow(client, watch);
    showEverywhere(client, newestPicks(client, watch, detail.issueKey));
  },
  undone: (client, write) => {
    const watch = watchOf(client);
    // Only a pick the watch showed has a value to put back: one a reload kept refused never
    // joined its line, and one refused before was put back then.
    if (!watch.live.delete(write.id)) return;
    putBack(client, watch, write.detail);
  },
});
