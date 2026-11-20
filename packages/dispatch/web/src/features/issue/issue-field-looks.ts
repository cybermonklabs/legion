/**
 * How a field of an issue that a pick changes - its priority, its assignee, its repository - shows
 * on each cache that draws the issue (`cacheOf`: its detail, its row of each list of issues, each
 * Inbox row of it, each ask of it the one read's Inbox answer lists), as a pick sets it
 * (`pickLook`) or an answer of Dispatch's carries it (`issueLook`, `answeredLooks`), and each such
 * cache with a look put over it (`shownOn`). It keeps nothing: what is live, what Dispatch last
 * answered and when are `issue-field-writes.ts`'s.
 */
import type { QueryClient, QueryKey } from "@tanstack/react-query";

import {
  inboxQuery,
  meInboxQuery,
  projectDefaultAssigneesQuery,
  whoAmIQuery,
} from "../../api/queries";
import type {
  InboxRow,
  Issue,
  IssueDetails,
  IssueSummary,
  MeInboxEntry,
  MeInboxResponse,
} from "../../api/types";
import { issueProject } from "../../lib/issue-key";
import { meInboxAskEntry } from "../inbox/one-read-cache";
import type { IssueField, IssueFieldPick } from "./issue-field-writes";

/** Every field of an issue a pick changes. */
export const everyField: readonly IssueField[] = ["assignee", "priority", "repo"];

/** The lists of issues a pick shows on beside the issue's detail: each project's
 *  (`["issues", "project", …]`, its board and its table) and each of the Priorities view
 *  (`["issues", "advances", …]`, `/priorities` and `/priorities/<id>`). */
const issueLists: readonly unknown[] = ["project", "advances"];

/** The key of the Inbox's read, every row of which a pick of its issue shows on. */
export const inboxKey = inboxQuery().queryKey;

/** The key of the one read's Inbox answer (`meInboxQuery`), which the Inbox draws the reader's own
 *  asks from behind its opt-in (`useInboxAsks`): each ask it lists a pick of its issue shows on, as
 *  on an Inbox row. The one read's other reads, the unread count and the dismissed pages, list no
 *  ask. */
const oneReadKey = meInboxQuery().queryKey;

/** The fields of an issue a pick changes, as its detail and its row of each list of issues carry
 *  them. */
type IssueFields = Pick<Issue, "assignee" | "priority" | "repo" | "resolved_repo">;

/** How one field of an issue shows wherever the issue is drawn: the issue's own fields, and, for
 *  the assignee, the person each of its Inbox rows waits on as Dispatch last answered it, by ask
 *  id (`waiting`); without it, that person follows the rule `showOnInboxRow` keeps. */
export interface Look {
  readonly fields: Partial<IssueFields>;
  readonly waiting?: ReadonlyMap<string, string | undefined>;
}

/** A look of one field of one issue to put on every cache that draws it. */
export interface Showing {
  readonly field: IssueField;
  readonly issueKey: string;
  readonly look: Look;
}

/** How `pick` shows. An own repository is also the one the issue resolves to; an inherited one
 *  is left to Dispatch's answer. */
export function pickLook(pick: IssueFieldPick): Look {
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
export function issueLook(field: IssueField, issue: Partial<IssueFields>): Look {
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

/** What an answer that is an issue carries as its own: its key, each field of it a pick changes,
 *  and the repository it resolves to. */
const answerNames: readonly string[] = ["key", ...everyField, "resolved_repo"];

/** `answer`, Dispatch's answer to a write, as an issue: an object whose `key` names the issue and
 *  that carries each field of it a pick changes as its own. Undefined for any other answer - for
 *  `null`, a landing Dispatch kept no answer for, among them. */
export function issueAnswer(answer: unknown): (IssueFields & Pick<Issue, "key">) | undefined {
  if (typeof answer !== "object" || answer === null) return undefined;
  const carried = answerNames.every((name) => Object.hasOwn(answer, name));
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
export function fieldLook(look: Look, field: IssueField): Look | undefined {
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
export function cachedLook(
  client: QueryClient,
  { field, issueKey }: IssueFieldPick
): Look | undefined {
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
export function answeredLooks(queryKey: QueryKey, data: unknown): Map<string, Look> {
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
 *  of issues (`issueLists`), the Inbox, or the one read's Inbox answer (`oneReadKey`); undefined
 *  for any other. */
export function cacheOf(queryKey: QueryKey): "detail" | "inbox" | "list" | "oneRead" | undefined {
  const [head, second] = queryKey;
  if (head === "issue" && queryKey.length === 2) return "detail";
  if (head === "issues" && issueLists.includes(second)) return "list";
  if (head === inboxKey[0]) return "inbox";
  const oneRead =
    queryKey.length === oneReadKey.length &&
    oneReadKey.every((part, index) => queryKey[index] === part);
  return oneRead ? "oneRead" : undefined;
}

/** Shows every one of `showings` on an Inbox row (`showOnInboxRow`), with the default assignee
 *  of each assignee shown looked up once, not once a row. */
function onInboxRow(
  client: QueryClient,
  showings: readonly Showing[]
): (row: InboxRow) => InboxRow {
  const fallbacks = showings.map((showing) =>
    showing.field === "assignee" ? defaultAssignee(client, showing.issueKey) : undefined
  );
  return (row) =>
    showings.reduce(
      (shown, showing, index) => showOnInboxRow(shown, showing, fallbacks[index]),
      row
    );
}

/**
 * `answer`, the one read's Inbox answer, with every one of `showings` over each ask it lists, as
 * on an Inbox row of it (`showOnInboxRow`). The one read lists the asks that wait on the reader,
 * whoever is signed in (`whoAmIQuery`): an ask a showing moves off the reader leaves it, and each
 * Inbox row an assignee shown makes wait on the reader joins it where it lists no entry of that
 * ask. Without that, taking an unassigned ask would take its row out of the Inbox read's list and
 * put it in no other (`useInboxAsks`, which draws the reader's own asks from the one read alone),
 * and an assignee put back would leave in the answer a row that no longer waits on the reader.
 * Each ask that leaves or joins moves `total` with it. `answer` itself where it shows them all
 * already.
 */
function shownOnOneRead(
  client: QueryClient,
  answer: MeInboxResponse,
  showings: readonly Showing[]
): MeInboxResponse {
  const show = onInboxRow(client, showings);
  const viewer = client.getQueryData(whoAmIQuery().queryKey)?.login;
  const entries = answer.entries ?? [];
  const listed = new Set<string>();
  let next: MeInboxEntry[] | undefined;
  for (const [index, entry] of entries.entries()) {
    const { ask } = entry;
    if (ask === undefined || ask === null) {
      next?.push(entry);
      continue;
    }
    listed.add(ask.id);
    const shown = show(ask);
    if (shown === ask) {
      next?.push(entry);
      continue;
    }
    next ??= entries.slice(0, index);
    const left =
      viewer !== undefined &&
      ask.waiting_on_person === viewer &&
      shown.waiting_on_person !== viewer;
    if (!left) next.push({ ...entry, ask: shown });
  }
  const assigned = new Set(
    showings.flatMap((showing) => (showing.field === "assignee" ? [showing.issueKey] : []))
  );
  const joined: MeInboxEntry[] = [];
  if (viewer !== undefined && assigned.size > 0) {
    for (const row of inboxRows(client)) {
      if (row.issue_key === null || !assigned.has(row.issue_key) || listed.has(row.id)) continue;
      const shown = show(row);
      if (shown.waiting_on_person !== viewer) continue;
      listed.add(row.id);
      joined.push(meInboxAskEntry(shown));
    }
  }
  if (next === undefined && joined.length === 0) return answer;
  const kept = next ?? entries;
  return {
    ...answer,
    entries: [...kept, ...joined],
    total: Math.max(0, answer.total - (entries.length - kept.length) + joined.length),
  };
}

/** `data`, which the query under `queryKey` holds, with every one of `showings` over it: the
 *  same value where it shows them all already, or where the query is none of the four a pick
 *  shows on. */
export function shownOn(
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
  if (cache === "oneRead") {
    if (typeof data !== "object" || data === null) return data;
    return shownOnOneRead(client, data as MeInboxResponse, showings);
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
  if (cache === "inbox") return mapRows(data as InboxRow[], onInboxRow(client, showings));
  return data;
}
