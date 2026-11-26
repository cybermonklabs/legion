import { type DispatchToolLabel, dispatchToolSpecs } from "@legion/contracts/dispatch-tools";

import type {
  Actor,
  GoingPlanAnswer,
  GoingPlanRow,
  GoingPlanTick,
  GoingRun,
  GoingUndoLine,
  IssueDetails,
} from "../../api/types";
import { changedWords, type WordPart } from "../doc/change-set";
import { issuePriority } from "../issue/issue-priority";
import { PROPERTY_LABELS } from "../issue-page/page-structure";
import { CONTROL_LABELS, issueStatusWord } from "../issue-page/page-words";
import { laneLabel } from "../project/board-model";
import { sessionOfAddress } from "../refs/routes";
import { clockWords } from "../search/blocks/block-cells";
import { objectOf, textOf } from "./fields";
import type { PlanDraft } from "./going-store";
import { postToolOf } from "./post-tools";

/**
 * What one plan's rows say to the person, and the person's answer to them: the card's title, made
 * from its rows; each row's change as the item, the field in the issue page's words, the value now
 * and the new value as the page shows them; a new issue as its title and its fields; a message as
 * whom it reaches and how; a document change as its sentences and its diff; why a row waits in
 * Auto-review; the ticks, where unticking a row unticks every row that needs it and ticking one
 * ticks every row it needs; and the approval those make. Where each row is drawn is
 * `plan-surfaces.ts`'.
 */

type Arguments = Readonly<Record<string, unknown>>;

/** A row's arguments as an object: what its tool runs with. */
export function argumentsOf(row: GoingPlanRow): Arguments {
  return objectOf(row.arguments);
}

/** The tools that make a new issue, whose row shows on the list of its parent. */
export function makesIssue(row: GoingPlanRow): boolean {
  return row.tool === "dispatch_issue";
}

/** `text` with its first letter a capital. */
function capitalized(text: string): string {
  return text === "" ? text : `${text.charAt(0).toUpperCase()}${text.slice(1)}`;
}

/** `count` and its noun: "1 issue", "3 issues". */
function counted(count: number, one: string, many: string): string {
  return `${count} ${count === 1 ? one : many}`;
}

/** The title of each new issue a plan's rows make, by the row's number, with the person's edits of
 *  it in `draft`: what a value naming that issue shows. */
export function newIssueTitles(
  rows: readonly GoingPlanRow[],
  draft: PlanDraft | undefined
): ReadonlyMap<number, string> {
  return new Map(
    rows
      .filter(makesIssue)
      .map((row) => [row.row, textOf({ ...argumentsOf(row), ...draft?.edits.get(row.row) }.title)])
  );
}

/** A value naming the issue a create of the same plan makes: "$N", N that create's number. */
const NEW_ISSUE = /^\$(\d+)$/u;

/** The arguments of each tool that may name a new issue of the plan as "$N": the ones whose tool
 *  takes an issue key there as a value the change writes, which the server reads "$N" in alone
 *  (going_plan.go `goingNewIssueArguments`). Every other value is text, shown as written. An
 *  update's before and after show in no field of their own: its read names them as the field
 *  rank, whose new value is the move they make (`moveText`), each issue as these show it. */
const NEW_ISSUE_ARGUMENTS: Readonly<Record<string, readonly string[]>> = {
  dispatch_issue: ["parent", "blocked_by"],
  dispatch_issue_update: ["parent", "blocked_by", "before", "after"],
};

const NO_NEW_ISSUES: ReadonlyMap<number, string> = new Map();

/** A string as the row shows it: a value naming a new issue of the plan as its title and "(new)",
 *  any other as it is. */
function issueText(value: string, newIssues: ReadonlyMap<number, string>): string {
  const named = NEW_ISSUE.exec(value);
  const title = named === null ? undefined : newIssues.get(Number(named[1]));
  return title === undefined ? value : `${title} (new)`;
}

/** The name of each field a row writes, as the issue page names it beside its value
 *  (`PROPERTY_LABELS`), and of the fields the page has no row for: a place on the board, the parts
 *  of an ask, a new issue's project. */
const FIELD_WORDS: Readonly<Record<string, string>> = {
  advances: PROPERTY_LABELS.advances,
  assignee: PROPERTY_LABELS.assignee,
  blocked_by: PROPERTY_LABELS.blocked_by,
  claim: PROPERTY_LABELS.claim,
  components: PROPERTY_LABELS.components,
  external_links: PROPERTY_LABELS.external_links,
  labels: PROPERTY_LABELS.labels,
  multiple: "Several answers",
  options: "Options",
  parent: PROPERTY_LABELS.parent,
  priority: PROPERTY_LABELS.priority,
  project: "Project",
  question: "Question",
  rank: "Place on the board",
  repo: PROPERTY_LABELS.repository,
  route: PROPERTY_LABELS.route,
  status: PROPERTY_LABELS.status,
  title: PROPERTY_LABELS.title,
  urgency: "Urgency",
};

/** A field as the row names it: "Priority", "Labels", "Blocked by". */
export function fieldWords(field: string): string {
  return FIELD_WORDS[field] ?? capitalized(field.replaceAll("_", " "));
}

/** The new holder of a claim the person takes: themselves. */
const CLAIMED_BY_YOU = "you";

/** One item of a list as words: a text as it is (an issue key naming a new issue of the plan by
 *  its title and "(new)" where `newIssues` is given), and an object by the first of its title, its
 *  label, its name, its address, its key or its id that it holds. */
function itemWords(item: unknown, newIssues: ReadonlyMap<number, string>): string {
  if (typeof item === "string") return issueText(item, newIssues);
  if (typeof item === "number" || typeof item === "boolean") return String(item);
  const fields = objectOf(item);
  for (const key of ["title", "label", "name", "url", "key", "id"]) {
    const named = fields[key];
    if (typeof named === "string" && named !== "") return named;
  }
  return "";
}

/** The items of a list value: the list itself, or the ids an object holds (the priorities an
 *  issue advances, its components); undefined for a value that is no list. */
function listOf(value: unknown): readonly unknown[] | undefined {
  if (Array.isArray(value)) return value;
  const ids = objectOf(value).ids;
  return Array.isArray(ids) ? ids : undefined;
}

/** An issue's place on its project's board (`issueRankPlace`): the issues it comes after and
 *  before, as a move names them. */
function placeWords(place: Arguments): string {
  const after = textOf(place.after);
  const before = textOf(place.before);
  const sides = [
    ...(after === "" ? [] : [`after ${after}`]),
    ...(before === "" ? [] : [`before ${before}`]),
  ];
  return sides.length === 0 ? "Alone on the board" : sides.join(", ");
}

/**
 * A value as the issue page shows it, in words (design v3, rule 6): a priority as "P2" or "No
 * priority", a status by its word, a list (labels, the issues it waits on, links, an ask's options)
 * with a comma between its items, the priorities an issue advances by their count, a place on the
 * board by the issues beside it, a claim's holder by name, a yes or no, a text as it is, and no
 * value as the page's "None". A value that may name a new issue of the plan ("$N") is given
 * `newIssues`, and shows that issue's title and "(new)", as the issue has no key before the plan
 * runs; without it every text shows as written, so a title "$100" shows "$100". No value shows as
 * its JSON.
 */
export function valueWords(
  field: string,
  value: unknown,
  newIssues: ReadonlyMap<number, string> = NO_NEW_ISSUES
): string {
  if (field === "priority") {
    return laneLabel(issuePriority(typeof value === "number" ? value : null));
  }
  if (field === "claim") {
    if (value === undefined || value === null) return "Nobody";
    if (typeof value === "string") return capitalized(value);
    const holder = objectOf(value);
    return holder.kind === "session" ? "an agent session" : textOf(holder.id);
  }
  if (value === undefined || value === null || value === "") return CONTROL_LABELS.none;
  if (typeof value === "string") {
    if (field === "status") return issueStatusWord(value);
    return field === "urgency" ? capitalized(value) : issueText(value, newIssues);
  }
  if (typeof value === "boolean") return value ? "Yes" : "No";
  if (typeof value === "number") return String(value);
  if (field === "rank") return placeWords(objectOf(value));
  if (field === "components" && objectOf(value).mode === "inherit") {
    return "Inherited from the issue above";
  }
  const list = listOf(value);
  if (list !== undefined) {
    if (list.length === 0) return CONTROL_LABELS.none;
    if (field === "advances") return counted(list.length, "priority", "priorities");
    const items = list.map((item) => itemWords(item, newIssues)).filter((item) => item !== "");
    return items.length === 0 ? counted(list.length, "item", "items") : items.join(", ");
  }
  const named = itemWords(value, newIssues);
  return named === "" ? CONTROL_LABELS.none : named;
}

/** One label of a change of labels: kept, put in, or taken out. */
export interface LabelMark {
  readonly label: string;
  readonly mark: "kept" | "added" | "removed";
}

/** The labels of `value`, each a text. */
function labelsOf(value: unknown): readonly string[] {
  return Array.isArray(value)
    ? value.filter((label): label is string => typeof label === "string")
    : [];
}

/** A change of labels as the row draws it: the new labels in their order, each kept or put in,
 *  then each label it takes out. Two labels that differ only in case are one, as the issue page
 *  holds them (`IssueLabels.tsx`). */
export function labelMarks(now: unknown, next: unknown): readonly LabelMark[] {
  const before = labelsOf(now);
  const after = labelsOf(next);
  const holds = (labels: readonly string[], label: string) =>
    labels.some((held) => held.toLowerCase() === label.toLowerCase());
  return [
    ...after.map((label) => ({
      label,
      mark: holds(before, label) ? ("kept" as const) : ("added" as const),
    })),
    ...before
      .filter((label) => !holds(after, label))
      .map((label) => ({ label, mark: "removed" as const })),
  ];
}

/** The labels an issue holds once a row adds `added` to `now` (`labels_add`): those it holds, in
 *  their order, then each added label it does not hold already, trimmed, two labels that differ
 *  only in case being one, as the route adds them (`mergeIssueLabels`). */
function withLabelsAdded(now: unknown, added: unknown): readonly string[] {
  const labels = [...labelsOf(now)];
  for (const label of labelsOf(added).map((text) => text.trim())) {
    if (label !== "" && !labels.some((held) => held.toLowerCase() === label.toLowerCase())) {
      labels.push(label);
    }
  }
  return labels;
}

/** The fields a new issue's row names, in the order the row shows them. */
const NEW_ISSUE_FIELDS = ["title", "project", "parent", "priority", "labels"] as const;

/** The arguments of an update that move its issue on its project's board, in the order a move
 *  reads: the update's read names them as the one field rank (going_observe.go
 *  `goingIssueFields`). */
const MOVE_ARGUMENTS = ["after", "before"] as const;

/** The move an update's arguments `args` make, as its rank's new value: "after <key>",
 *  "before <key>", or both, each key as its argument shows it, a new issue of the plan ("$N",
 *  `newIssues`' row N) as its title and "(new)"; undefined for a row that names neither. */
function moveText(args: Arguments, newIssues: ReadonlyMap<number, string>): string | undefined {
  const places = MOVE_ARGUMENTS.filter((place) => args[place] !== undefined).map(
    (place) => `${place} ${valueWords(place, args[place], newIssues)}`
  );
  return places.length === 0 ? undefined : places.join(", ");
}

/** How the person edits a new value: the issue page's priority menu, its label picker, its status
 *  menu, its assignee picker, a box of text, or not at all. */
export type FieldEdit = "priority" | "labels" | "status" | "assignee" | "text" | undefined;

/** One field a row writes: its name, its value now and the value the row writes, the person's
 *  edit where they made one, how the person can edit it, and whether its value may name a new
 *  issue of the plan as "$N" (`NEW_ISSUE_ARGUMENTS`). An update's rank is the move its before and
 *  after make, whose new value is that move's words (`moveText`), which the person cannot edit:
 *  rank is no argument the change takes. A claim's new holder is the person, or nobody. */
export interface RowChange {
  readonly field: string;
  readonly now: unknown;
  readonly next: unknown;
  readonly edit: FieldEdit;
  readonly edited: boolean;
  readonly namesNewIssue: boolean;
}

/** The tools whose row writes the fields its read names, each with a value now: an issue's
 *  fields, an ask's parts and a claim. Every other row shows a field only where its arguments
 *  give it a value. */
const VALUE_TOOLS: Readonly<Record<string, true>> = {
  dispatch_claim: true,
  dispatch_edit_ask: true,
  dispatch_issue_update: true,
};

/** How the person edits `field`, whose new value is `value`. */
function editOf(field: string, value: unknown): FieldEdit {
  if (field === "priority" && (value === null || typeof value === "number")) return "priority";
  if (field === "labels" && Array.isArray(value)) return "labels";
  if (field === "status" && typeof value === "string") return "status";
  if (field === "assignee" && (value === null || typeof value === "string")) return "assignee";
  return typeof value === "string" ? "text" : undefined;
}

/** What a row changes, field by field: the fields its item's read names, each with its value now
 *  and the new value, an update's rank the move its before and after make (`moveText`, which
 *  shows a new issue of the plan by its title in `newIssues`), an update's labels where it adds
 *  labels (`labels_add`) the labels now with those it adds (`withLabelsAdded`), a claim's holder
 *  now and after; a new issue's title, project, parent, priority and labels, none of which has a
 *  value now; an edit of a comment or an issue message its body, whose value now is the body the
 *  plan read (`postRead`), and a delete of one nothing. A row that names no field shows its item
 *  alone. The person's edit of the labels a row adds is the whole set they chose, which the
 *  approval writes as labels in place of labels_add (going_plan.go `goingEditReplaces`). */
export function rowChanges(
  row: GoingPlanRow,
  edits: Arguments | undefined,
  newIssues: ReadonlyMap<number, string>
): RowChange[] {
  const args = { ...argumentsOf(row), ...edits };
  const editedField = (field: string) => edits !== undefined && Object.hasOwn(edits, field);
  const namesNewIssue = (field: string) => NEW_ISSUE_ARGUMENTS[row.tool]?.includes(field) ?? false;
  if (makesIssue(row)) {
    return NEW_ISSUE_FIELDS.filter((field) => args[field] !== undefined).map((field) => ({
      edit: editOf(field, args[field]),
      edited: editedField(field),
      field,
      namesNewIssue: namesNewIssue(field),
      next: args[field],
      now: undefined,
    }));
  }
  if (postToolOf(row.tool) !== undefined) {
    // A post's read is its author and its body, not a value of each field it names: the one
    // field an edit writes is the body, and a delete writes none.
    return args.body === undefined
      ? []
      : [
          {
            edit: editOf("body", args.body),
            edited: editedField("body"),
            field: "body",
            namesNewIssue: false,
            next: args.body,
            now: postRead(row).body,
          },
        ];
  }
  const fields = row.fields ?? [];
  const now = objectOf(row.now);
  return fields
    .filter((field) => VALUE_TOOLS[row.tool] === true || args[field] !== undefined)
    .map((field) => {
      const moves = row.tool === "dispatch_issue_update" && field === "rank";
      const claims = row.tool === "dispatch_claim";
      const adds =
        row.tool === "dispatch_issue_update" &&
        field === "labels" &&
        args.labels === undefined &&
        args.labels_add !== undefined;
      // A field the read names with no value (null) is empty now; one it does not name is the
      // read's whole value where the row writes one field (a claim's holder).
      const valueNow = Object.hasOwn(now, field)
        ? now[field]
        : fields.length === 1
          ? row.now
          : undefined;
      const next = moves
        ? moveText(args, newIssues)
        : claims
          ? args.release === true
            ? null
            : CLAIMED_BY_YOU
          : adds
            ? withLabelsAdded(valueNow, args.labels_add)
            : args[field];
      return {
        edit: moves || claims ? undefined : editOf(field, next),
        edited: editedField(field),
        field,
        namesNewIssue: namesNewIssue(field),
        next,
        now: valueNow,
      };
    });
}

/** Whether a value is there: neither absent nor empty. */
function present(value: unknown): boolean {
  if (value === undefined || value === null || value === "") return false;
  const list = listOf(value);
  return list === undefined || list.length > 0;
}

/** The values a change takes out, each in words (`itemWords`, `valueWords`): the items a list - the
 *  labels, the issues it waits on, its links, the priorities it advances - loses, compared with no
 *  regard to case, or the value it clears. A close, a move and a new issue's fields take out no
 *  value. An item that has no words is "" here. */
function removedValues(change: RowChange): readonly string[] {
  if (change.field === "status" || change.field === "rank" || change.now === undefined) return [];
  const before = listOf(change.now);
  const after = listOf(change.next);
  if (before !== undefined && after !== undefined) {
    const kept = new Set(after.map((item) => itemWords(item, NO_NEW_ISSUES).toLowerCase()));
    return before
      .map((item) => itemWords(item, NO_NEW_ISSUES))
      .filter((words) => !kept.has(words.toLowerCase()));
  }
  return present(change.now) && !present(change.next) ? [valueWords(change.field, change.now)] : [];
}

/** The tools whose every row deletes, removes, closes or declines something. */
const REMOVING_TOOLS: Readonly<Record<string, true>> = {
  dispatch_decline_ask: true,
  dispatch_delete_comment: true,
  dispatch_delete_message: true,
  dispatch_delete_priority: true,
  dispatch_delete_session_group: true,
  dispatch_dismiss_inbox: true,
  dispatch_reject_suggestion: true,
  dispatch_remove_architecture_source: true,
  dispatch_remove_document: true,
  dispatch_resolve_ask: true,
  dispatch_resolve_comment: true,
  dispatch_unset_legion_daemon: true,
  dispatch_unsubscribe_document: true,
  dispatch_unsubscribe_issue: true,
};

/** Whether a row closes, deletes or removes something (design v3, state 6), which the row marks
 *  in red: a tool that only removes, a release of a claim, an unfollow, a retracted result, a
 *  clear of a project's default repository (repo null), and a change of a field that closes the
 *  issue (status done) or takes a value out (`removedValues`), given the row's `changes`. */
export function rowRemoves(row: GoingPlanRow, changes: readonly RowChange[]): boolean {
  const args = argumentsOf(row);
  if (REMOVING_TOOLS[row.tool] === true) return true;
  if (row.tool === "dispatch_claim") return args.release === true;
  if (row.tool === "dispatch_follow") return args.action === "unfollow";
  if (row.tool === "dispatch_result") return args.retract !== undefined;
  if (row.tool === "dispatch_set_default_repository") return args.repo === null;
  return changes.some((change) =>
    change.field === "status" ? change.next === "done" : removedValues(change).length > 0
  );
}

/** What a row `rowRemoves` marks in red says in words beside the mark (design v3, state 6), given
 *  the row's `changes`: each value they take out, by name, so the person reads before Approve
 *  what the row drops - "Removes board", "Removes board and api" - and "Closes, and removes
 *  board" where it also closes its issue. A row that only closes its issue, or whose tool removes
 *  something and names no value of a field, says nothing more: its red mark and its own words
 *  ("Status: Triage → Done", "Delete the comment") say it, and a lone "Closes" or "Removes" under
 *  them is no sentence (review a1). */
export function removalWords(changes: readonly RowChange[]): string {
  const removed = changes.flatMap(removedValues).filter((words) => words !== "");
  if (removed.length === 0) return "";
  const closes = changes.some((change) => change.field === "status" && change.next === "done");
  return `${closes ? "Closes, and removes" : "Removes"} ${listed(removed)}`;
}

/** The tools whose row sends a message to an agent (R7): on an issue, to one session, and to
 *  several at once. */
const MESSAGE_TOOLS: Readonly<Record<string, true>> = {
  dispatch_broadcast: true,
  dispatch_message: true,
  dispatch_send_to_agent: true,
};

/** The kinds of row a plan card draws, each in its own way (design v3, state 6). */
export type RowKind = "new_issue" | "change" | "claim" | "document" | "message" | "post" | "other";

/** What kind of row `row` is: a new issue, a change of an issue's fields, a claim, a change of a
 *  document's text, a message, an edit or a delete of a comment or an issue message (a post), or
 *  any other change. */
export function rowKind(row: GoingPlanRow): RowKind {
  if (makesIssue(row)) return "new_issue";
  if (MESSAGE_TOOLS[row.tool] === true) return "message";
  if (postToolOf(row.tool) !== undefined) return "post";
  if (documentChange(row, "") !== undefined) return "document";
  if (row.tool === "dispatch_issue_update") return "change";
  return row.tool === "dispatch_claim" ? "claim" : "other";
}

/** Whether a row's change cannot be undone once it runs (R6): a message, which stays sent, and a
 *  delete of a comment or an issue message, which nothing restores. Each says so while it waits
 *  ("Cannot be undone once sent"). */
export function rowIrreversible(row: GoingPlanRow): boolean {
  return rowKind(row) === "message" || postToolOf(row.tool)?.deletes === true;
}

/** What a row that cannot be undone (`rowIrreversible`) is, as the line over the pinned Approve
 *  names it, one of it and more: a message, sent; or a post's deletion. */
function irreversibleNouns(row: GoingPlanRow): { readonly one: string; readonly many: string } {
  const post = postToolOf(row.tool);
  return post?.deletes === true
    ? { many: `${post.kind}s' deletions`, one: `a ${post.kind}'s deletion` }
    : { many: "messages", one: "a message" };
}

/** The rows of `rows` that cannot be undone (`rowIrreversible`), each kind named and counted, in
 *  the order the rows first name it (review A1): "a message", "a message and a comment's
 *  deletion", "2 messages and a comment's deletion"; "" where none of them is one. */
export function irreversibleWords(rows: readonly GoingPlanRow[]): string {
  const kinds = new Map<string, { readonly many: string; count: number }>();
  for (const row of rows.filter(rowIrreversible)) {
    const { many, one } = irreversibleNouns(row);
    const kind = kinds.get(one) ?? { count: 0, many };
    kind.count += 1;
    kinds.set(one, kind);
  }
  return listed(
    [...kinds].map(([one, { count, many }]) => (count === 1 ? one : `${count} ${many}`))
  );
}

/** How a message reaches its session, in the words the row says it with. */
const DELIVERY_WORDS: Readonly<Record<string, string>> = {
  aside: "as an aside",
  btw: "as a BTW",
  steer: "as a steer",
};

/** How a message row's message reaches its sessions: "as a BTW", "as an aside", "as a steer";
 *  "" for a message on an issue, which names no delivery. */
export function deliveryWords(row: GoingPlanRow): string {
  return DELIVERY_WORDS[textOf(argumentsOf(row).delivery)] ?? "";
}

/** The sessions a broadcast row sends to. */
export function broadcastSessions(row: GoingPlanRow): readonly string[] {
  const sessions = argumentsOf(row).session_ids;
  return Array.isArray(sessions)
    ? sessions.filter((session): session is string => typeof session === "string")
    : [];
}

/** Whom a message on an issue's conversation reaches, as far as the page knows: the sessions,
 *  each once - the one the issue's route names or that holds the role it names, the one that
 *  holds its claim, then those that follow the issue; the role its route names where the page
 *  knows no session that holds it; and whether the sessions that follow the issue are among
 *  them, which they are only where the page read them. */
export interface MessageReach {
  readonly sessions: readonly string[];
  readonly role: string | undefined;
  readonly followersKnown: boolean;
}

/**
 * Whom a message on `issue`'s conversation reaches, as Dispatch delivers one (`eventDestinations`,
 * `outbox/publisher.go`): the session or the role the issue sends its messages to (its route),
 * the session that holds its claim, and the sessions that follow the issue (`followers`, by
 * session, undefined where the page could not read them). Until the issue itself is read, the
 * page knows none of them.
 */
export function messageReach(
  issue: Pick<IssueDetails, "claim" | "route" | "route_holder"> | undefined,
  followers: readonly string[] | undefined
): MessageReach {
  if (issue === undefined) return { followersKnown: false, role: undefined, sessions: [] };
  const route = issue.route ?? "";
  // The session a route reaches: the one it names, or the one that holds the role it names.
  const routed = sessionOfAddress(route) ?? (issue.route_holder || undefined);
  const holder = issue.claim?.actor;
  const sessions = new Set<string>();
  if (routed !== undefined) sessions.add(routed);
  if (holder?.kind === "session") sessions.add(holder.id);
  for (const follower of followers ?? []) sessions.add(follower);
  return {
    followersKnown: followers !== undefined,
    role:
      routed === undefined && route.startsWith("role:") ? route.slice("role:".length) : undefined,
    sessions: [...sessions],
  };
}

/** The reason the server gives for a row whose event, once written, reaches an agent session
 *  (`goingWaitsDelivery`, going_modes.go): `outbox.WouldDeliver` found one. */
const DELIVERS_TO_SESSION = "delivers_to_session";

/**
 * Whether a message on an issue's conversation, `row`, reaches no agent session, as far as the
 * page and the server know it (`reach`): the page read the issue and the sessions that follow it,
 * and none of them names a session or a role, and the server's read of the row finds no session
 * its event reaches either (`DELIVERS_TO_SESSION`). The server reads more than the page does: a
 * reply reaches the session that wrote the message it answers, which the page does not read.
 */
export function reachesNoSession(reach: MessageReach, row: GoingPlanRow): boolean {
  return (
    reach.followersKnown &&
    reach.sessions.length === 0 &&
    reach.role === undefined &&
    !(row.waits ?? []).includes(DELIVERS_TO_SESSION)
  );
}

/** The words a row posts or sends, which the row shows whole: a message's, a comment's, a
 *  question's; "" for a row that posts none. */
export function rowText(row: GoingPlanRow): string {
  const args = argumentsOf(row);
  return textOf(args.body) || textOf(args.question);
}

/** Each tool's label, the words the bar shows for its call (`dispatch-tools.ts`), by its name. */
const TOOL_LABELS: ReadonlyMap<string, DispatchToolLabel> = new Map(
  dispatchToolSpecs.map((spec): [string, DispatchToolLabel] => [spec.name, spec.label])
);

/** A comment or an issue message as the row of a tool that edits or deletes it read it
 *  (going_observe.go `goingPostNow`): its author, and its body while it stands. A post deleted
 *  since the plan read it (`postGone`) has no body, as no read of a deleted post gives one; a row
 *  whose plan gives no value now, a withdrawn run's, has neither. */
export interface PostRead {
  readonly author: Actor | undefined;
  readonly body: string | undefined;
}

/** The post a row that edits or deletes one read (`PostRead`). */
export function postRead(row: GoingPlanRow): PostRead {
  const now = objectOf(row.now);
  const author = objectOf(now.author);
  const id = textOf(author.id);
  return {
    author: id === "" ? undefined : { id, kind: textOf(author.kind) },
    body: postGone(row) || typeof now.body !== "string" ? undefined : now.body,
  };
}

/** Whether the comment or the issue message a row edits or deletes is deleted (`deleted`): the
 *  row's value now is the delete's mark and no body. Nothing edits or deletes a deleted post, so
 *  nobody may tick such a row (`409 POST_DELETED`). A delete the row itself ran leaves it so. */
export function postGone(row: GoingPlanRow): boolean {
  return row.deleted === true;
}

/** Whether a row's post was deleted after the plan read it by a write other than the row's own:
 *  it is gone (`postGone`), and the row is no delete that ran. */
export function deletedSince(row: GoingPlanRow): boolean {
  return postGone(row) && !(postToolOf(row.tool)?.deletes === true && row.state === "done");
}

/** What a row that edits or deletes a post does, in its tool's verb, as an order: "Edit the
 *  comment", "Delete the message", and "Edit your comment" where the post is the person's own
 *  (`yours`, review a3); "" for a row of any other tool. */
export function postAction(row: GoingPlanRow, yours = false): string {
  const post = postToolOf(row.tool);
  if (post === undefined) return "";
  const verb = capitalized(TOOL_LABELS.get(row.tool)?.verb ?? "change");
  return `${verb} ${yours ? "your" : "the"} ${post.kind}`;
}

/** The label the plan names a row by in place of its tool's, given the row's arguments: a new
 *  issue is filed, as the card's title says; a project's default repository is set to the
 *  repository the row names, or cleared where it names none (repo null), as Settings' Clear says
 *  it, so the person reads before Approve what the press writes. Undefined for every other row,
 *  and for one whose arguments name no repository at all, which its tool's label names. */
function ownLabel(tool: string, args: Arguments): string | undefined {
  if (tool === "dispatch_issue") return "File {title|{issue|a new issue}}";
  if (tool === "dispatch_set_default_repository") {
    if (args.repo === null) return "Clear the default repository of {project}";
    if (typeof args.repo === "string") return "Set the default repository of {project} to {repo}";
  }
  return undefined;
}

/** A `{name}` of a label, with the words shown when the call leaves it out after a `|`
 *  (`dispatch-tools.ts`'s own pattern). */
const LABEL_PLACEHOLDER = /\{([^{}|]*)(?:\|((?:[^{}]|\{[^{}]*\})*))?\}/gu;

/** The arguments a label names an issue by, which may name a new issue of the plan as "$N". */
const LABEL_ISSUES: Readonly<Record<string, true>> = { issue: true, ref: true };

/** A label form with each `{name}` filled: an argument's text as it is (an issue a new issue of
 *  the plan, by its title and "(new)"), a number in digits; an argument the call left out, the
 *  words the label gives for that, and nothing where it gives none. */
function filledLabel(
  form: string,
  args: Arguments,
  newIssues: ReadonlyMap<number, string>
): string {
  return form
    .replace(new RegExp(LABEL_PLACEHOLDER), (_whole, name: string, absent: string | undefined) => {
      const value = args[name];
      if (typeof value === "string" && value !== "") {
        return LABEL_ISSUES[name] === true ? issueText(value, newIssues) : value;
      }
      if (typeof value === "number") return String(value);
      return absent === undefined ? "" : filledLabel(absent, args, newIssues);
    })
    .replace(/\s+/gu, " ")
    .trim();
}

/** The words a change of `tool` with `args` is named by before its arguments fill them: the
 *  plan's own (`ownLabel`), else its tool's label as an order, its base verb in the place of the
 *  present form's first word ("Update {issue}"); undefined for a tool that has no label. */
function changeForm(tool: string, args: Arguments): string | undefined {
  const own = ownLabel(tool, args);
  if (own !== undefined) return own;
  const label = TOOL_LABELS.get(tool);
  if (label === undefined) return undefined;
  const first = label.present.split(" ", 1)[0] ?? "";
  return `${capitalized(label.verb)}${label.present.slice(first.length)}`;
}

/** What a change of `tool` with `args` does, in the words the bar names its step with, as an
 *  order: "Update CORE-3", "Send a message to CORE-1", "File The second half". */
export function changeLabel(
  tool: string,
  args: Arguments,
  newIssues: ReadonlyMap<number, string> = NO_NEW_ISSUES
): string {
  const form = changeForm(tool, args);
  return form === undefined ? "Make a change" : filledLabel(form, args, newIssues);
}

/** The name of a document a row changes, as its arguments give it: a reference, a document's
 *  name or slug; "" where they give none. */
function documentName(args: Arguments): string {
  return textOf(args.ref) || textOf(args.artifact) || textOf(args.name);
}

/** How a row names its change wherever it is drawn - its tick, the line of a change of a document
 *  or of another kind - with the person's `edits` of it: its change and its item, a new issue by
 *  its title, a document by its name, a comment or an issue message by what the row does and the
 *  issue it is on (`postAction`), as its arguments name it by an id, which says nothing to the
 *  person; so each row of a plan is named apart. */
export function rowLabel(
  row: GoingPlanRow,
  edits?: Arguments,
  newIssues: ReadonlyMap<number, string> = NO_NEW_ISSUES
): string {
  const action = postAction(row);
  if (action !== "") return row.issue === "" ? action : `${action} on ${row.issue}`;
  const args = { ...argumentsOf(row), ...edits };
  const document = documentName(args);
  return changeLabel(
    row.tool,
    document === "" || textOf(args.ref) !== "" ? args : { ...args, ref: document },
    newIssues
  );
}

/** The arguments a label names an item by, which an Undo line fills with the item it names. */
const ITEM_ARGUMENTS = [
  "ask",
  "comment",
  "document",
  "group",
  "issue",
  "message",
  "priority",
  "ref",
  "session",
  "thread",
] as const;

/** Whether `args` hold every argument `form` needs: each one it names with no words for its
 *  absence has a value a label prints. */
function fillsLabel(form: string, args: Arguments): boolean {
  for (const [, name = "", absent] of form.matchAll(LABEL_PLACEHOLDER)) {
    const value = args[name];
    const printed = (typeof value === "string" && value !== "") || typeof value === "number";
    if (absent === undefined && !printed) return false;
  }
  return true;
}

/**
 * A change an Undo line names, in the words its plan row named it with. `row` is the row of the
 * run's plan the line undid, where the page holds it: its arguments fill every argument the
 * label names, as the plan's card filled them (`rowLabel`) - "Update CORE-3", "Set the default
 * repository of CORE to acme/infra". A line whose row the page does not hold - the plan is not
 * read, or the row is of an earlier plan of the run - carries its tool and its item alone. The
 * tool's label is then filled with the item, in the argument that names an item, where that is
 * every argument the label needs: "Send a message to CORE-1", "File CORE-9". A label that needs
 * another ("Set the default assignee of {project} to {assignee}") would read with a gap, or with
 * the words for a value the change never left out, so such a line reads "A change to CORE".
 */
export function undoLineLabel(
  line: Pick<GoingUndoLine, "tool" | "item">,
  row: GoingPlanRow | undefined
): string {
  if (row !== undefined) return rowLabel(row);
  const { item, tool } = line;
  const args: Arguments =
    item === "" ? {} : Object.fromEntries(ITEM_ARGUMENTS.map((argument) => [argument, item]));
  const form = changeForm(tool, args);
  if (form === undefined || fillsLabel(form, args)) return changeLabel(tool, args);
  return item === "" ? "A change" : `A change to ${item}`;
}

/** The row of `plan` an Undo line undid: the row of its number, where `plan` is the plan the line
 *  names; undefined where the page holds no such row. */
export function undoLineRow(
  line: Pick<GoingUndoLine, "plan" | "row">,
  plan: GoingPlanAnswer | undefined
): GoingPlanRow | undefined {
  return plan?.plan === line.plan ? plan.rows?.find((row) => row.row === line.row) : undefined;
}

/** The verb a change of an issue's fields is said with in the card's title: a close (status
 *  done), a move (a place on the board or a parent, and nothing else), or a change. */
function updateVerb(row: GoingPlanRow): "close" | "move" | "change" {
  if (argumentsOf(row).status === "done") return "close";
  const fields = row.fields ?? [];
  return fields.length > 0 && fields.every((field) => field === "rank" || field === "parent")
    ? "move"
    : "change";
}

/** One part of a card's title: a verb, the items it names, and how many changes it counts. */
interface TitleClause {
  readonly verb: string;
  readonly one: string;
  readonly many: string;
  /** Whether a clause of one item names it by its key ("Change DEMO-10"). */
  readonly keyed: boolean;
  readonly items: Set<string>;
  count: number;
}

/** The parts of a sentence joined as a list is: "a", "a and b", "a, b and c". */
function listed(parts: readonly string[]): string {
  if (parts.length <= 1) return parts.join("");
  return `${parts.slice(0, -1).join(", ")} and ${parts.at(-1)}`;
}

/** How many changes one part of a title counts: the items it names where it names them by key,
 *  else its rows. */
function clauseCount(clause: TitleClause): number {
  return clause.keyed && clause.items.size > 0 ? clause.items.size : clause.count;
}

/**
 * The plan card's title (design v3, state 6): a sentence made from its rows, "Change DEMO-10",
 * "File 2 issues and move 3", "Send 1 message". Each kind of change is one part, in the order the
 * rows first name it: new issues are filed; changes of an issue's fields change, move or close
 * their issues, an issue named by its key where the part names one; messages are sent; a
 * suggested change to a document is suggested, as its row says ("Suggest a change to the spec of
 * DEMO-15"), and any other change of a document edits it; a comment or an issue message is
 * edited or deleted, in its tool's verb ("Edit 1 comment"); and the rest made. A part that counts
 * the same thing as the one before it leaves out its noun. Past two kinds the sentence no longer
 * fits one line, so it counts instead: "Change 4 items and send 1 message", every change of an
 * item in one count and the messages, which change no item, in their own.
 */
export function planTitle(rows: readonly GoingPlanRow[]): string {
  // One part for each verb and the noun it counts: an edit of a document and one of a comment
  // are two parts.
  const clauses = new Map<string, TitleClause>();
  const add = (verb: string, one: string, many: string, keyed: boolean, item: string) => {
    const clause = clauses.get(`${verb} ${one}`) ?? {
      count: 0,
      items: new Set(),
      keyed,
      many,
      one,
      verb,
    };
    clause.count += 1;
    if (item !== "") clause.items.add(item);
    clauses.set(`${verb} ${one}`, clause);
  };
  for (const row of rows) {
    const item = row.item || row.issue;
    switch (rowKind(row)) {
      case "new_issue":
        add("file", "issue", "issues", false, "");
        break;
      case "change":
        add(updateVerb(row), "issue", "issues", true, item);
        break;
      case "claim":
        add("change", "issue", "issues", true, item);
        break;
      case "document":
        if (row.tool === "dispatch_suggest") {
          add("suggest", "change to a document", "changes to documents", false, "");
        } else {
          add("edit", "document", "documents", false, "");
        }
        break;
      case "message":
        add("send", "message", "messages", false, "");
        break;
      case "post": {
        const kind = postToolOf(row.tool)?.kind ?? "post";
        add(TOOL_LABELS.get(row.tool)?.verb ?? "change", kind, `${kind}s`, false, "");
        break;
      }
      default:
        add("make", "change", "changes", false, "");
    }
  }
  if (clauses.size > 2) {
    const sent = clauses.get("send message");
    const changed = [...clauses.values()]
      .filter((clause) => clause !== sent)
      .reduce((sum, clause) => sum + clauseCount(clause), 0);
    return capitalized(
      listed([
        ...(changed === 0 ? [] : [`change ${counted(changed, "item", "items")}`]),
        ...(sent === undefined ? [] : [`send ${counted(sent.count, "message", "messages")}`]),
      ])
    );
  }
  let noun = "";
  const parts = [...clauses.values()].map((clause, index) => {
    const [only] = clause.items;
    if (clause.keyed && clause.items.size === 1 && only !== undefined) {
      noun = "";
      return `${clause.verb} ${only}`;
    }
    const count = clauseCount(clause);
    const other = clause.verb === "make" && index > 0 ? "other " : "";
    const said =
      clause.one === noun
        ? `${clause.verb} ${count}`
        : `${clause.verb} ${counted(count, `${other}${clause.one}`, `${other}${clause.many}`)}`;
    noun = clause.one;
    return said;
  });
  return capitalized(listed(parts)) || "Change nothing";
}

/** The states of a row the person's approval, or Auto-review, sent to run: waiting its turn,
 *  running, run, failed, or skipped because a change it needs did not run. */
const SENT_STATES: Readonly<Partial<Record<GoingPlanRow["state"], true>>> = {
  approved: true,
  done: true,
  failed: true,
  running: true,
  skipped: true,
};

/** The states of a sent row that has come to an end. */
const FINISHED_STATES: Readonly<Partial<Record<GoingPlanRow["state"], true>>> = {
  done: true,
  failed: true,
  skipped: true,
};

/** What an Undo did, in its card's title (design v2, state 7): "Undone · 3 changes reverted", or
 *  how many of the changes it could revert where another writer's value stayed or a revert was
 *  refused. A message and a delete of a post are never reverted, and count in neither. */
export function undoneTitle(lines: readonly GoingUndoLine[]): string {
  const reversible = lines.filter((line) => line.undo !== "not_undone");
  const reverted = reversible.filter((line) => line.outcome.ok).length;
  if (reversible.length === 0) return "Undone · nothing to revert";
  return reverted === reversible.length
    ? `Undone · ${counted(reverted, "change", "changes")} reverted`
    : `Undone · ${reverted} of ${counted(reversible.length, "change", "changes")} reverted`;
}

/**
 * What Dispatch's record of a run says of its Undo (`GoingRun`), which is all the page says of
 * it where it holds no answer of a press: when an undo of the run landed, in this page's clock
 * (`undone_at`, the newest undo of any one of its changes; undefined where none did), how many
 * of its changes Undo can still take back (`undoable_count`), and how many stay as the run left
 * them - those, and the ones an Undo tried and could not take back, a change its undo is refused
 * for or a value another writer left (`standing_count`). A message, and any change Dispatch
 * does not undo, is sent, not undone, and counts in neither. No count of a plan's rows says any
 * of it: a row that ran can be a setting Undo leaves as it is.
 */
export interface RunUndoRecord {
  readonly at: number | undefined;
  readonly undoable: number;
  readonly stays: number;
}

/** What `record` says of its run's Undo (`RunUndoRecord`). */
export function undoRecord(
  record: Pick<GoingRun, "standing_count" | "undoable_count" | "undone_at">
): RunUndoRecord {
  const landed = record.undone_at === null ? Number.NaN : Date.parse(record.undone_at);
  return {
    at: Number.isNaN(landed) ? undefined : landed,
    stays: record.undoable_count + record.standing_count,
    undoable: record.undoable_count,
  };
}

/** How a run an undo landed on reads under its answer, by how many of its changes stay
 *  (`RunUndoRecord.stays`): "Undone" only where none does, else "Partly undone · 1 change stays",
 *  in the word a kept value's row uses ("Title stays Bob's dome"). */
export function undoneStateWords(stays: number): string {
  return stays === 0
    ? "Undone"
    : `Partly undone · ${counted(stays, "change stays", "changes stay")}`;
}

/** What the card's Undo says its press takes back, "Undo 2 changes": the changes Dispatch's
 *  record says Undo can still take back (`RunUndoRecord.undoable`), so a message, which stays
 *  sent (R6), and a setting Undo leaves as it is count in none. */
export function undoButtonWords(undoable: number): string {
  return `Undo ${counted(undoable, "change", "changes")}`;
}

/**
 * The plan card's title as the plan stands (design v2, states 6 and 7): while it waits, the
 * sentence its rows make (`planTitle`); while its rows run, "Applying 1 of 3…", counting the row
 * that runs now among the rows sent to run; once they ran, "Applied 3 changes · 16:42", the time
 * the last row ran (`appliedAt`) where the page knows it, and how many of them ran where some did
 * not; once its run was undone (`undone`, what Undo answered), "Undone · 3 changes reverted"; for
 * a run whose Undo this page holds no answer of, as Dispatch says (`landed`): "Undone · 11:35 AM",
 * when an undo of it landed, where no change of it stays, and "Partly undone · 11:35 AM" where
 * one does; and "Discarded" once the person discarded it.
 */
export function cardTitle(
  plan: GoingPlanAnswer,
  undone: readonly GoingUndoLine[] | undefined,
  appliedAt: number | undefined,
  landed?: { readonly at: number; readonly stays: number }
): string {
  const rows = plan.rows ?? [];
  if (undone !== undefined) return undoneTitle(undone);
  if (landed !== undefined) {
    return `${landed.stays > 0 ? "Partly undone" : "Undone"} · ${clockWords(landed.at)}`;
  }
  const sent = rows.filter((row) => SENT_STATES[row.state] === true);
  switch (plan.state) {
    case "waiting":
      return planTitle(rows);
    case "rejected":
      return "Discarded · nothing ran";
    case "approved":
    case "running": {
      const finished = sent.filter((row) => FINISHED_STATES[row.state] === true).length;
      return `Applying ${Math.min(finished + 1, sent.length)} of ${sent.length}…`;
    }
    default: {
      const done = sent.filter((row) => row.state === "done").length;
      const applied =
        sent.length === 0
          ? "Nothing applied"
          : done === sent.length
            ? `Applied ${counted(done, "change", "changes")}`
            : `Applied ${done} of ${counted(sent.length, "change", "changes")}`;
      return appliedAt === undefined ? applied : `${applied} · ${clockWords(appliedAt)}`;
    }
  }
}

/**
 * What a run did with its plan once the plan's rows ran, for the line its steps fold into (design
 * v3, state 7): "applied 3 changes", counting the rows that ran as the card's title does, since
 * that is the last thing the run did; undefined while the plan waits or runs, once it was
 * discarded, and where no row ran, where "proposed a plan" stays what the run did. The line is
 * the run's work, so an Undo since leaves it as it is: the card, its rows and the line under the
 * answer say what was undone.
 */
export function appliedWords(plan: GoingPlanAnswer): string | undefined {
  if (plan.state !== "done") return undefined;
  const done = (plan.rows ?? []).filter((row) => row.state === "done").length;
  return done === 0 ? undefined : `applied ${counted(done, "change", "changes")}`;
}

/** How a row stands in its card (design v2, state 7): waiting for the person, sent and waiting
 *  its turn, running (a spinner), run (a check), failed, not run, or run and then taken back
 *  whole by Undo. */
export type RowMark = "waits" | "turn" | "running" | "done" | "failed" | "not_run" | "reverted";

/** Each row state's mark. */
export const ROW_MARKS: Readonly<Record<GoingPlanRow["state"], RowMark>> = {
  approved: "turn",
  done: "done",
  failed: "failed",
  running: "running",
  skipped: "not_run",
  unticked: "not_run",
  waiting: "waits",
};

/** The words of each mark, which a screen reader hears in its place. */
export const MARK_WORDS: Readonly<Record<RowMark, string>> = {
  done: "Done",
  failed: "Failed",
  not_run: "Not run",
  reverted: "Reverted",
  running: "Running",
  turn: "Waits its turn",
  waits: "Waits for you",
};

/** What a message row's result says (R7): sent, queued until the session can take it, or refused
 *  with the reason in words. */
function deliveryResult(row: GoingPlanRow): string | undefined {
  switch (row.result?.delivery) {
    case "sent":
      return "Sent";
    case "queued":
      return "Queued: Dispatch delivers it when the session can take it";
    case "refused": {
      const reason = rowResultMessage(row);
      return `Refused: ${reason === "" ? "the session did not take it" : reason}`;
    }
    default:
      return undefined;
  }
}

/** What a row says once its plan no longer waits (design v2, state 7), and whether that is a
 *  failure, which offers "Say what to do instead". */
export interface RowOutcome {
  readonly text: string;
  readonly failed: boolean;
}

/** What a delete of a comment or an issue message says once it ran, and once its run's Undo
 *  answered (R6): nothing restores a deleted post (going_undo.go `goingDeletedNotUndone`). */
const DELETED_NOT_UNDONE = "Deleted, not undone";

/**
 * What a row that no longer waits says (design v2, state 7; R4, R6, R7): a message row its
 * result, "Sent", "Queued: …" or "Refused: ‹the reason›"; a delete of a post that ran, "Deleted,
 * not undone"; a failed row its reason in words; a row skipped because a change it needs did not
 * run, "Not run: it needs the change above"; an unticked row, "Not run: you left it unticked". Any
 * other row that waits its turn, runs or ran says nothing past its mark. Undefined where it says
 * nothing.
 */
export function rowOutcome(row: GoingPlanRow): RowOutcome | undefined {
  const delivery = rowKind(row) === "message" ? deliveryResult(row) : undefined;
  if (delivery !== undefined) return { failed: row.state === "failed", text: delivery };
  if (row.state === "done" && postToolOf(row.tool)?.deletes === true) {
    return { failed: false, text: DELETED_NOT_UNDONE };
  }
  switch (row.state) {
    case "failed": {
      const reason = rowResultMessage(row);
      return {
        failed: true,
        text: reason === "" ? "Dispatch refused this change, and nothing was changed." : reason,
      };
    }
    case "skipped":
      return { failed: false, text: "Not run: it needs the change above" };
    case "unticked":
      return { failed: false, text: "Not run: you left it unticked" };
    default:
      return undefined;
  }
}

/** One field of an Undo line: how its undo went. */
export type UndoField = NonNullable<GoingUndoLine["fields"]>[number];

/** A field Undo kept because another writer changed it after the run, as the server sends it as
 *  data: its value now and who wrote it; undefined for any other field, and for a field of an
 *  answer that names the value in its message alone. */
export function keptValue(
  field: UndoField
): { readonly now: unknown; readonly by: UndoField["by"] } | undefined {
  return field.ok || !Object.hasOwn(field, "now") ? undefined : { by: field.by, now: field.now };
}

/** What became of a row's change once its run was undone (design v2, state 7; R6), where no field
 *  of it was kept as another writer left it: "Reverted", "Sent, not undone" for a message,
 *  "Deleted, not undone" for a delete of a post, or why it could not be reverted. */
export function undoWords(line: GoingUndoLine): string {
  if (line.undo === "not_undone") {
    return postToolOf(line.tool)?.deletes === true ? DELETED_NOT_UNDONE : "Sent, not undone";
  }
  return line.outcome.ok ? "Reverted" : `Not reverted: ${line.outcome.message}`;
}

/** Whether Undo took a row's change back whole (design v3, state 7): no field of it kept as
 *  another writer left it, nothing refused, and no message, which is sent, not undone. Such a row
 *  reads reverted - the revert arrow in place of its check, its change still drawn as the plan
 *  card drew it, and "Reverted" under it - where a change that stayed keeps its check and says
 *  why. */
export function wasReverted(line: GoingUndoLine): boolean {
  return (
    line.undo !== "not_undone" &&
    line.outcome.ok &&
    (line.fields ?? []).every((field) => field.ok || field.message === "")
  );
}

/** The approval modes, as the person's setting names them (`CommandBarSection.tsx`). */
export const MODE_WORDS: Readonly<Record<GoingPlanAnswer["mode"], string>> = {
  ask: "Ask",
  auto_review: "Auto-review",
};

/** Why a row waits for the person in every mode, by the reason the server sends
 *  (`goingRowWaits`, going_modes.go): its class, where what it does is the person's own word, a
 *  setting or a message, and the two reasons beside a class. */
const WAIT_REASONS: Readonly<Record<string, string>> = {
  ask_answer: "it answers a question in your name",
  broadcast: "it sends a message to several agent sessions",
  claimed_by_session: "an agent session holds this issue's claim",
  decision: "it keeps a decision in your name",
  delivers_to_session: "Dispatch hands this change to an agent session at once",
  message: "it sends a message",
  result: "it records a check result in your name",
  review: "it gives a review in your name",
  setting: "it changes a setting of Dispatch",
};

/** Why Auto-review never runs a row of a class it does not let through (going_modes.go
 *  `goingAutoReviewClasses`: fields, links, claims, and new issues with a title alone). */
const CLASS_WAITS: Readonly<Record<string, string>> = {
  create_text: "it makes an item with text, which Auto-review never runs",
  suggestion: "it suggests a change of text, which Auto-review never runs",
  text: "it changes text, which Auto-review never runs",
};

/** The classes Auto-review may let through (going_modes.go `goingAutoReviewClasses`). */
const AUTO_REVIEW_CLASSES: Readonly<Record<string, true>> = {
  claim: true,
  create_plain: true,
  field: true,
  link: true,
};

/**
 * What a row says of Auto-review (R5) while its plan waits: a row the review let through, which
 * has run, runs or is about to, "Ran · passed review"; a row that waits for the person, why, in
 * words: each reason the server sends, else the class Auto-review never runs, else that the
 * person's words did not ask for it. In Ask every row waits and no reason shows, so it says
 * nothing; nor once the plan no longer waits.
 */
export function reviewWords(plan: GoingPlanAnswer, row: GoingPlanRow): string | undefined {
  if (plan.mode !== "auto_review" || plan.state !== "waiting") return undefined;
  switch (row.state) {
    case "done":
      return "Ran · passed review";
    case "running":
      return "Running · passed review";
    case "approved":
      return "Passed review · runs next";
    case "waiting":
      break;
    default:
      return undefined;
  }
  const reasons = (row.waits ?? [])
    .map((reason) => WAIT_REASONS[reason])
    .filter((reason): reason is string => reason !== undefined);
  if (reasons.length > 0) return `Waits for you: ${reasons.join("; ")}`;
  if (AUTO_REVIEW_CLASSES[row.class] !== true) {
    return `Waits for you: ${CLASS_WAITS[row.class] ?? "Auto-review never runs this kind of change"}`;
  }
  return "Waits for you: your words did not ask for this change";
}

/** What a row whose item changed after the plan showed says of the value now: the one field's
 *  value, or each field by its name. */
export function valuesNowWords(changes: readonly RowChange[]): string {
  const shown = changes.filter((change) => change.now !== undefined);
  const [only] = shown;
  if (shown.length === 1 && only !== undefined) return valueWords(only.field, only.now);
  return shown
    .map((change) => `${fieldWords(change.field)} ${valueWords(change.field, change.now)}`)
    .join("; ");
}

/** The longest the sentences shown above a new issue's whole spec run, in characters. */
const SPEC_SENTENCES_MAX = 300;

/** A Markdown heading's line: one to six `#` marks, then a space or the line's end. */
const HEADING_LINE = /^\s{0,3}#{1,6}(?:\s|$)/u;

/** A line of Markdown as plain words: a heading's `#` marks, a list, task or quote mark,
 *  emphasis and code marks and a link's address left out, a link read as its words. */
function plainLine(line: string): string {
  const words = HEADING_LINE.test(line)
    ? line.replace(/^\s{0,3}#{1,6}/u, "").replace(/\s#+\s*$/u, "")
    : line;
  return words
    .replace(/^\s{0,3}(?:[-*+]\s+(?:\[[ xX]\]\s+)?|\d+[.)]\s+|>\s?)/u, "")
    .replace(/!?\[([^\]]*)\]\([^)]*\)/gu, "$1")
    .replace(/[*_`]+/gu, "")
    .trim();
}

/** The first two or three sentences of a new issue's spec, as plain words: its headings left
 *  out, its other lines as `plainLine` reads them and run together, and the third sentence,
 *  then the second, left out where they run past `SPEC_SENTENCES_MAX`. */
export function specSentences(spec: string): string {
  const plain = spec
    .split("\n")
    .filter((line) => !HEADING_LINE.test(line))
    .map(plainLine)
    .filter((line) => line !== "")
    .join(" ");
  const sentences = plain.match(/[^.!?]+(?:[.!?]+|$)/gu)?.map((sentence) => sentence.trim()) ?? [];
  for (const count of [3, 2]) {
    const said = sentences.slice(0, count).join(" ");
    if (said.length <= SPEC_SENTENCES_MAX) return said;
  }
  const first = sentences[0] ?? "";
  return first.length <= SPEC_SENTENCES_MAX ? first : `${first.slice(0, SPEC_SENTENCES_MAX)}…`;
}

/** One piece of a document change: the text it takes out and the text it puts in. */
export interface ChangePiece {
  readonly removed: string;
  readonly added: string;
}

/** A run between two changes that holds nothing but space. */
const ONLY_SPACE = /^\s+$/u;

/**
 * The changes of an edit as `changedWords` finds them, with changes that only spaces part joined
 * into one run taken out and one put in: "~~once a month~~every two weeks", which reads as the
 * one edit it is, rather than "~~once~~every ~~a~~two ~~month~~weeks", which reads as three. The
 * spaces between two joined changes stay on each side that holds words on both sides of them.
 */
function joinedChanges(parts: readonly WordPart[]): WordPart[] {
  const joined: WordPart[] = [];
  let out = "";
  let into = "";
  let space = "";
  let outGap = "";
  let intoGap = "";
  const flush = () => {
    if (out !== "") joined.push({ kind: "removed", value: out });
    if (into !== "") joined.push({ kind: "added", value: into });
    if (space !== "") joined.push({ kind: "same", value: space });
    out = "";
    into = "";
    space = "";
    outGap = "";
    intoGap = "";
  };
  for (const part of parts) {
    if (part.kind === "same") {
      if ((out !== "" || into !== "") && ONLY_SPACE.test(part.value)) {
        space += part.value;
        continue;
      }
      flush();
      joined.push(part);
      continue;
    }
    outGap += space;
    intoGap += space;
    space = "";
    if (part.kind === "removed") {
      out += (out === "" ? "" : outGap) + part.value;
      outGap = "";
    } else {
      into += (into === "" ? "" : intoGap) + part.value;
      intoGap = "";
    }
  }
  flush();
  return joined;
}

/** Where each sentence of `text` ends: after its full stop, question mark or exclamation mark and
 *  any closing quote or bracket, where a space or the text's end comes next; and at its end. */
function sentenceEnds(text: string): number[] {
  const ends = [...text.matchAll(/[.!?]+["'’”)\]]*(?=\s|$)/gu)].map(
    (stop) => stop.index + stop[0].length
  );
  if (ends.at(-1) !== text.length) ends.push(text.length);
  return ends;
}

/** How many characters before an edit's first change the preview of a sentence too long for it
 *  starts: the few words that place the change in its sentence. */
const PREVIEW_LEAD = 40;

/** `body` as plain words: each of its lines as `plainLine` reads it, a heading's words kept, since
 *  an edit can change a heading, and its empty lines left out. */
function plainWords(body: string): string {
  return body
    .split("\n")
    .map(plainLine)
    .filter((line) => line !== "")
    .join("\n");
}

/**
 * The preview of a long edit of a post's body (review m1): two or three of its sentences in plain
 * words (`plainWords`), its lines run together, the words the edit takes out struck and the words
 * it puts in marked, as the diff folded under it marks them (`changedWords`), so the person reads
 * what changes without opening the diff. They are the body's first sentences where those hold the
 * edit's first change and fit `SPEC_SENTENCES_MAX`, as a new issue's spec shows
 * (`specSentences`); else the sentence that holds it and those after it, led by "…"; a sentence
 * too long for the preview shows from `PREVIEW_LEAD` characters before the change. A preview that
 * stops before the body ends ends with "…". Changes that only spaces part read as one run out and
 * one in (`joinedChanges`). An edit of the Markdown's marks alone, such as a link's address,
 * leaves the two bodies' plain words the same, so its preview marks the changed words of the body
 * as written instead, and still shows that something changes.
 */
export function editPreview(now: string, next: string): WordPart[] {
  const plainNow = plainWords(now);
  const plainNext = plainWords(next);
  const [before, after] = plainNow === plainNext ? [now, next] : [plainNow, plainNext];
  const parts = joinedChanges(changedWords(before, after)).map((part) => ({
    kind: part.kind,
    value: part.value.replace(/\s+/gu, " "),
  }));
  const text = parts.map((part) => part.value).join("");
  const ends = sentenceEnds(text);
  const starts = [0, ...ends.slice(0, -1)];
  const changeAt = parts.findIndex((part) => part.kind !== "same");
  const first =
    changeAt === -1
      ? 0
      : parts.slice(0, changeAt).reduce((length, part) => length + part.value.length, 0);
  const held = Math.max(
    0,
    ends.findIndex((end) => end > first)
  );
  const span = (from: number, to: number): readonly [number, number] => [
    starts[from] ?? 0,
    ends[Math.min(to, ends.length) - 1] ?? text.length,
  ];
  const windows = [
    ...(held < 3 ? [span(0, 3)] : []),
    ...(held < 2 ? [span(0, 2)] : []),
    span(held, held + 3),
    span(held, held + 2),
    span(held, held + 1),
  ];
  // A sentence too long for the preview: from the word `PREVIEW_LEAD` characters before the
  // change, to the last whole word the preview's length holds.
  const lead = Math.max(
    starts[held] ?? 0,
    text.lastIndexOf(" ", Math.max(0, first - PREVIEW_LEAD)) + 1
  );
  const stop = text.lastIndexOf(" ", lead + SPEC_SENTENCES_MAX);
  const [from, to] = windows.find(
    ([start, end]) => text.slice(start, end).trim().length <= SPEC_SENTENCES_MAX
  ) ?? [lead, Math.min(ends[held] ?? text.length, stop > first ? stop : lead + SPEC_SENTENCES_MAX)];
  const shown: WordPart[] = [];
  let at = 0;
  for (const part of parts) {
    const start = Math.max(from, at);
    const end = Math.min(to, at + part.value.length);
    if (start < end) shown.push({ kind: part.kind, value: part.value.slice(start - at, end - at) });
    at += part.value.length;
  }
  const head = shown[0];
  if (head !== undefined) head.value = head.value.trimStart();
  const last = shown.at(-1);
  if (last !== undefined) last.value = last.value.trimEnd();
  const preview = shown.filter((part) => part.value !== "");
  if (text.slice(0, from).trim() !== "") preview.unshift({ kind: "same", value: "…" });
  if (text.slice(to).trim() !== "") preview.push({ kind: "same", value: "…" });
  return preview;
}

/** A change of a document's text, as the plan shows it first: two or three sentences that say what
 *  it changes, and the pieces of its diff, which the plan keeps closed under them. */
export interface DocumentChange {
  readonly sentences: string;
  readonly pieces: readonly ChangePiece[];
}

/** The document change a row makes, or undefined for a row of any other kind: a suggestion
 *  (`dispatch_suggest`, its quote and its replacement), an edit (`dispatch_doc_edit`, each
 *  operation's text out and in) and an upload over a document (`dispatch_artifact`, the content now
 *  and the content it writes). Its sentences are the ones the model wrote for it - the
 *  suggestion's rationale, the version's summary - or else the plan's own summary. */
export function documentChange(row: GoingPlanRow, summary: string): DocumentChange | undefined {
  const args = argumentsOf(row);
  const said = (value: unknown) =>
    typeof value === "string" && value.trim() !== "" ? value : summary;
  switch (row.tool) {
    case "dispatch_suggest":
      return {
        pieces: [{ added: textOf(args.replace_with), removed: textOf(args.quote) }],
        sentences: said(args.body),
      };
    case "dispatch_doc_edit": {
      const ops = Array.isArray(args.ops) ? args.ops : [];
      return {
        pieces: ops.map((op: unknown) => {
          const fields = objectOf(op);
          return { added: textOf(fields.with ?? fields.markdown), removed: textOf(fields.find) };
        }),
        sentences: said(args.summary),
      };
    }
    case "dispatch_artifact":
      return typeof args.content === "string"
        ? {
            pieces: [{ added: args.content, removed: textOf(objectOf(row.now).content) }],
            sentences: said(args.summary),
          }
        : undefined;
    default:
      return undefined;
  }
}

/** What a document row does, in the page's own words (review N4): the words before the issue
 *  whose document it changes, and that issue ("" for a project's document), which the row draws
 *  as its chip after them. */
export interface DocumentNaming {
  readonly words: string;
  readonly issue: string;
}

/** Each document tool's verb, and the word that joins the document to the issue it is of. */
const DOCUMENT_ACTIONS: Readonly<
  Partial<Record<string, { readonly verb: string; readonly join: string }>>
> = {
  dispatch_artifact: { join: "to", verb: "Upload" },
  dispatch_doc_edit: { join: "of", verb: "Edit the text of" },
  dispatch_suggest: { join: "of", verb: "Suggest a change to" },
};

/** The document a `dispatch://` reference names: "spec" for an issue's spec, the slug of an
 *  artifact; "" for any other reference. */
const DOCUMENT_REF = /^dispatch:\/\/[^/]+\/(?:(spec)|artifact\/([^/#@?]+))(?:[/#@?]|$)/u;

/**
 * How a document row names what it does (`DocumentNaming`), drawn from the row's own data - its
 * tool, the document's name, the issue it is of - and never from the tool's label, whose words
 * the server and the model share (review N4): an issue's spec is "the spec", any other document
 * its name or slug, and the issue follows: "Suggest a change to the spec of" DEMO-15, "Edit the
 * text of notes.md of" DEMO-15, "Upload design.md to" DEMO-16. A project's document ends with its
 * project in words, "Upload design.md to DEMO", or with its name where the row names none.
 */
export function documentNaming(row: GoingPlanRow): DocumentNaming {
  const args = argumentsOf(row);
  const action = DOCUMENT_ACTIONS[row.tool] ?? { join: "of", verb: "Change" };
  const referred = DOCUMENT_REF.exec(textOf(args.ref));
  const name = textOf(args.artifact) || textOf(args.name) || referred?.[1] || referred?.[2] || "";
  const document = name === "" ? "a document" : name === "spec" ? "the spec" : name;
  const named = `${action.verb} ${document}`;
  const issue = row.issue || textOf(args.issue);
  if (issue !== "") return { issue, words: `${named} ${action.join}` };
  const project = textOf(args.project);
  return { issue: "", words: project === "" ? named : `${named} ${action.join} ${project}` };
}

/** The refusals of an upload whose document moved after the plan read its name: a document made
 *  under the name (`DOCUMENT_EXISTS`), text other than the text read (`DOCUMENT_CHANGED`), or a
 *  version written past the token read (`PRECONDITION_FAILED`). The route writes each for a caller
 *  holding the `dispatch` command, naming the command that reads a token and the field that sends
 *  it; the person who approved the row runs no command. */
const UPLOAD_MOVED_CODES: ReadonlySet<string> = new Set([
  "DOCUMENT_CHANGED",
  "DOCUMENT_EXISTS",
  "PRECONDITION_FAILED",
]);

/** What the row says of an upload its route refused because the document moved after the plan
 *  read it. */
export const UPLOAD_MOVED_REFUSAL =
  "The document under this name changed after the plan read it, so the upload wrote nothing. Read what it holds now before you ask for this upload again.";

/** What the row says of an edit of an approval request (`ASK_KIND_FIXED`), whose question and
 *  options are fixed: the route tells the agent that asked to hand the request back with its
 *  command, and what the person can do instead is change the document the request is about. */
export const ASK_KIND_FIXED_REFUSAL =
  "An approval request's question and options cannot be edited, so nothing was changed. Change the document it asks about instead.";

/** What the row says of an edit that writes a question into a decision whose question is empty
 *  and which holds a paragraph that is no text (`ASK_BLOCK_TEXT`, the document service's "has an
 *  empty question"): the route tells the agent to upload the document again with its command,
 *  and the person can write the question in the document editor. */
export const ASK_EMPTY_QUESTION_REFUSAL =
  "This decision has no question, and writing one here would remove what one of its paragraphs holds, so nothing was changed. Write the question beside that paragraph in the document editor.";

/** A refusal whose words name a command of the `dispatch` command-line tool - written for an
 *  agent that runs it - which the person who approved the row cannot run. */
const NAMES_A_COMMAND = /\bdispatch [a-z][a-z-]*/u;

/** What a row's result says to the person: the page's own words for an upload refused because
 *  its document moved (`UPLOAD_MOVED_REFUSAL`), an edit of an approval request
 *  (`ASK_KIND_FIXED_REFUSAL`) and a question written into a decision that cannot take one there
 *  (`ASK_EMPTY_QUESTION_REFUSAL`); any other refusal of a failed row whose words name a `dispatch`
 *  command, in whatever form the server writes it (backticked, followed by its flags, or in
 *  parentheses beside its arguments), in the page's general words, so no route's advice to run a
 *  command reaches the person; and every other result as Dispatch wrote it, a waiting or skipped
 *  row's words, which can name an item's title, among them. */
export function rowResultMessage(row: GoingPlanRow): string {
  const result = row.result;
  if (result === null) {
    return "";
  }
  if (
    row.tool === "dispatch_artifact" &&
    row.state === "failed" &&
    UPLOAD_MOVED_CODES.has(result.code)
  ) {
    return UPLOAD_MOVED_REFUSAL;
  }
  if (result.code === "ASK_KIND_FIXED") {
    return ASK_KIND_FIXED_REFUSAL;
  }
  if (
    result.code === "ASK_BLOCK_TEXT" &&
    result.message.includes("writing a question into it would delete")
  ) {
    return ASK_EMPTY_QUESTION_REFUSAL;
  }
  return row.state === "failed" && NAMES_A_COMMAND.test(result.message)
    ? "Dispatch refused this change with advice for its command-line tool, which this page does not run, so nothing was changed. Open the item to change it here."
    : result.message;
}

/** The rows that need `row`, and the rows that need those, by number. */
function rowsNeeding(rows: readonly GoingPlanRow[], row: number): Set<number> {
  const needing = new Set<number>();
  let grew = true;
  while (grew) {
    grew = false;
    for (const candidate of rows) {
      const needs = candidate.needs ?? [];
      if (!needing.has(candidate.row) && needs.some((need) => need === row || needing.has(need))) {
        needing.add(candidate.row);
        grew = true;
      }
    }
  }
  return needing;
}

/** The rows `row` needs, and the rows those need, by number. */
function rowsNeeded(rows: readonly GoingPlanRow[], row: number): Set<number> {
  const byNumber = new Map(rows.map((candidate) => [candidate.row, candidate]));
  const needed = new Set<number>();
  const visit = (at: number) => {
    for (const need of byNumber.get(at)?.needs ?? []) {
      if (!needed.has(need)) {
        needed.add(need);
        visit(need);
      }
    }
  };
  visit(row);
  return needed;
}

/** The draft with `row` ticked or unticked: unticking it unticks each row that needs it, and
 *  ticking it ticks each row it needs, so the draft never approves a row without what it needs. */
export function toggledDraft(
  draft: PlanDraft,
  rows: readonly GoingPlanRow[],
  row: number
): PlanDraft {
  const unticked = new Set(draft.unticked);
  if (unticked.has(row)) {
    unticked.delete(row);
    for (const need of rowsNeeded(rows, row)) unticked.delete(need);
  } else {
    unticked.add(row);
    for (const needing of rowsNeeding(rows, row)) unticked.add(needing);
  }
  return { ...draft, unticked };
}

/** Whether `row` is unticked because a row it needs is, among the rows `unticked` names
 *  (`untickedRows`): what it says, "Needs the change above". */
export function needsUnticked(unticked: ReadonlySet<number>, row: GoingPlanRow): boolean {
  return unticked.has(row.row) && (row.needs ?? []).some((need) => unticked.has(need));
}

/** The rows of `plan` the draft leaves out: each the person unticked, each waiting row whose post
 *  is deleted (`postGone`), which nobody may tick, and each row that needs one of those, as the
 *  approval leaves out a row whose need it leaves out (going_plan.go `approveGoingPlan`). The
 *  draft's own set where no waiting row's post is deleted. */
export function untickedRows(plan: GoingPlanAnswer, draft: PlanDraft): ReadonlySet<number> {
  const rows = plan.rows ?? [];
  const gone = rows.filter((row) => rowWaits(row) && postGone(row));
  if (gone.length === 0) return draft.unticked;
  const unticked = new Set(draft.unticked);
  for (const row of gone) {
    unticked.add(row.row);
    for (const needing of rowsNeeding(rows, row.row)) unticked.add(needing);
  }
  return unticked;
}

/** The draft as the plan card holds it: the person's answer, and the change of each row's item
 *  after the plan showed that has unticked its row already, so each such change unticks its row
 *  once and the person's tick after it holds. */
interface CardDraft extends PlanDraft {
  readonly changesSeen?: ReadonlySet<string>;
}

/** The draft with each waiting row whose item changed after the plan showed (R4) unticked, with
 *  each row that needs it, once for each value its item changed to: such a row waits to be
 *  ticked again. The same draft where no row changed since it was last read. */
export function withChangesUnticked(draft: PlanDraft, plan: GoingPlanAnswer): PlanDraft {
  const seen = (draft as CardDraft).changesSeen ?? new Set<string>();
  const rows = plan.rows ?? [];
  const fresh = rows
    .filter((row) => rowWaits(row) && row.changed)
    .map((row) => ({ key: `${row.row}:${JSON.stringify(row.now)}`, row: row.row }))
    .filter((change) => !seen.has(change.key));
  if (fresh.length === 0) return draft;
  const unticked = new Set(draft.unticked);
  for (const change of fresh) {
    unticked.add(change.row);
    for (const needing of rowsNeeding(rows, change.row)) unticked.add(needing);
  }
  const next: CardDraft = {
    ...draft,
    changesSeen: new Set([...seen, ...fresh.map((change) => change.key)]),
    unticked,
  };
  return next;
}

/** The draft with the person's edit of `field` of `row`: a text, a priority, a set of labels. */
export function editedDraft(
  draft: PlanDraft,
  row: number,
  field: string,
  value: unknown
): PlanDraft {
  const edits = new Map(draft.edits);
  edits.set(row, { ...edits.get(row), [field]: value });
  return { ...draft, edits };
}

/** Whether the row waits for the person: the rows a tick or an edit can change. */
export function rowWaits(row: GoingPlanRow): boolean {
  return row.state === "waiting";
}

/** How many changes the draft approves of `plan`: its waiting rows it leaves ticked
 *  (`untickedRows`). */
export function tickedCount(plan: GoingPlanAnswer, draft: PlanDraft): number {
  const unticked = untickedRows(plan, draft);
  return (plan.rows ?? []).filter((row) => rowWaits(row) && !unticked.has(row.row)).length;
}

/** Whether `plan` waits for its person's answer: it is read, it waits, and its run is not
 *  withdrawn - a run that read a comment or an issue message deleted since, whose plan Dispatch
 *  approves and revises for nobody (`409 GOING_RUN_WITHDRAWN`). */
export function planWaits(plan: GoingPlanAnswer | undefined): boolean {
  return plan !== undefined && plan.state === "waiting" && plan.withdrawn !== true;
}

/** Whether the person can approve `plan` with `draft` now, by Approve or by Enter in the box,
 *  which both ask this: the plan waits (`planWaits`), no answer to it is out (`pending`), and the
 *  draft leaves a waiting row ticked. An approval with no row ticked runs nothing, and a second
 *  one while the first is out names a plan that no longer waits. */
export function approvable(plan: GoingPlanAnswer, draft: PlanDraft, pending: boolean): boolean {
  return planWaits(plan) && !pending && tickedCount(plan, draft) > 0;
}

/** The approval the draft makes of `plan` (`POST /api/v1/going/runs/{id}/plan/approve`): the
 *  version of the plan the person was shown, which Dispatch refuses the approval over once the
 *  plan has changed since, each waiting row's tick (`untickedRows`), a row whose post is deleted
 *  unticked, and the values the person edited. */
export function approvalOf(
  plan: GoingPlanAnswer,
  draft: PlanDraft
): { plan: number; rows: GoingPlanTick[]; version: GoingPlanAnswer["version"] } {
  const unticked = untickedRows(plan, draft);
  const rows: GoingPlanTick[] = (plan.rows ?? []).filter(rowWaits).map((row) => {
    const values = draft.edits.get(row.row);
    return {
      row: row.row,
      tick: !unticked.has(row.row),
      ...(values === undefined ? {} : { values: { ...values } }),
    };
  });
  return { plan: plan.plan, rows, version: plan.version };
}
