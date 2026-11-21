import { isIdPrefix } from "@legion/contracts";

import { askProject } from "../features/inbox/list-row";
import { routeItem } from "../features/refs/block-reference";
import {
  buildDispatchReference,
  DECISIONS_DOCUMENT_SLUG,
  parseNodeReference,
} from "../features/refs/routes";
import { issueProject } from "../lib/issue-key";
import {
  agentMessagesQuery,
  architectureSourcesQuery,
  inboxQuery,
  inboxThreadsKey,
  myRepliesKey,
  projectArchitectureSourcesQuery,
  projectDefaultAssigneesQuery,
  projectsQuery,
  userAgentStateQuery,
  userStateQuery,
} from "./queries";
import { answeredNotFound, type RefreshCheck, type RefreshTarget } from "./query-refresh";
import type {
  ArchitectureTree,
  ArchitectureTreeComponent,
  ArchitectureTreeIssueRef,
  Artifact,
  Ask,
  AskRead,
  ChangedReference,
  DispatchEvent,
  DocumentReferenceSummary,
  EventType,
  GraphReferences,
  InboxRow,
  IssueSummary,
  MyRepliesResponse,
  MyReplyRow,
} from "./types";

/**
 * Which cached reads one event of the feed can change, and nothing else: `eventRefreshTargets` is
 * the whole of it, and `api/query-refresh.ts` decides when each read is fetched. Query keys are the
 * literal arrays their readers use (the reference reads' builders live in
 * `features/refs/reference-target.ts`; `__tests__/event-feed.test.ts` holds the two in step).
 *
 * The server says what a write moved where a rule needs it: `changed` and `ancestors` on
 * `issue.created|updated|closed`, `thread_logins` on the writes that can move someone's Replies
 * count, `changed` on `user_state.updated`. An event without the field - written before it
 * existed, or by an older server mid-rollout - falls back to what any value could have moved.
 */

const inboxKey = inboxQuery().queryKey;
const projectsKey = projectsQuery().queryKey;
const pinnedKey = ["issues", "pinned"] as const;

/** One untyped-payload read for the fields the rules take off an event. */
function payloadValue(event: DispatchEvent, key: string): unknown {
  const payload = event.payload;
  return typeof payload === "object" && payload !== null ? Reflect.get(payload, key) : undefined;
}

function payloadString(event: DispatchEvent, key: string): string | undefined {
  const value = payloadValue(event, key);
  return typeof value === "string" ? value : undefined;
}

/** A list field the server writes, or undefined on an event written before it existed. */
function payloadStrings(event: DispatchEvent, key: string): readonly string[] | undefined {
  const value = payloadValue(event, key);
  return Array.isArray(value)
    ? value.filter((item): item is string => typeof item === "string")
    : undefined;
}

/** Whether `login` is the signed-in viewer's, compared as the server compares logins. */
export function isViewer(login: string | undefined, viewer: string | undefined): boolean {
  return (
    login !== undefined &&
    viewer !== undefined &&
    login.trim().toLowerCase() === viewer.trim().toLowerCase()
  );
}

/** A list read's rows, as the reader that holds the query wrote them; none for a query with no
 *  list (an error, or not loaded), or a read of another shape. The caller names the rows' type
 *  (`Row`): a cached answer is the one its own read returned. */
export function rowsOf<Row>(data: unknown): readonly Row[] {
  return Array.isArray(data) ? (data as Row[]) : [];
}

/** The sidebar's pinned list, when it holds `key`: an issue the viewer has not pinned shows in no
 *  row of it. */
function pinnedRow(key: string): RefreshTarget {
  return {
    exact: true,
    queryKey: pinnedKey,
    when: (data) => rowsOf<IssueSummary>(data).some((issue) => issue.key === key),
  };
}

/**
 * The issue summary read behind every reference to `key`: the title and status a link to the
 * issue shows, the name and version of each of its documents, and the one read that says
 * whether a key a body writes bare names an issue at all (`useTextReferences`).
 *
 * A 404 is that last answer - no issue holds the key - and it is final: a key-shaped word a body
 * writes (`UTF-8`, a key of another workspace) names no issue until someone creates one, so an
 * update, a close or a document version asks nothing about it, where each would otherwise cost
 * one request per such word on screen. `issue.created` asks whatever the last answer was, and so
 * does the whole-cache refresh the stream's reconnect runs (`refreshQueries` with no targets).
 */
function issueSummaryOf(key: string, event: DispatchEvent): RefreshTarget {
  const target = { exact: true, queryKey: ["issue-summary", key] } as const;
  return event.type === "issue.created"
    ? target
    : { ...target, when: (_data, query) => !answeredNotFound(query) };
}

/** Every Inbox read (the Inbox and a project's) that holds a row of issue `key`'s open asks. */
function inboxRowsOf(key: string): RefreshTarget {
  return {
    queryKey: inboxKey,
    when: (data) => rowsOf<InboxRow>(data).some((row) => row.issue_key === key),
  };
}

/** Every Inbox read that holds the row of ask `id`. */
function inboxRowOfAsk(id: string): RefreshTarget {
  return {
    queryKey: inboxKey,
    when: (data) => rowsOf<InboxRow>(data).some((row) => row.id === id),
  };
}

/** Whether an ask is an open question nobody handed on: the person it waits on is then its
 *  issue's assignee, or its project's default while nobody holds the issue or the ask is on a
 *  project document (`coalesce(a.asked_of, i.assignee, p.default_assignee)`), so an assignee move
 *  or a default move changes it. An answered or closed ask's card draws no Asked-of badge
 *  (`AskHandOn`, drawn for an open ask alone in `AskCard`), so neither move changes what a read of
 *  it shows. Every target below that tests a read for a question one of those moves carries
 *  tests it with this. */
function openAndNotHandedOn(ask: Pick<Ask, "asked_of" | "state">): boolean {
  return ask.state === "open" && ask.asked_of === null;
}

/** Every Inbox read that holds an open ask of `project` whose person that project's default
 *  decides: the last step of `coalesce(a.asked_of, i.assignee, p.default_assignee)`, so an ask
 *  nobody handed on, on an issue nobody holds or on a project document. The Inbox read names no
 *  assignee (`inboxQuery`), so a reader's list holds every open ask she can see with the person
 *  the server resolved for it (`waiting_on_person`), whoever that is, and the assignee filter and
 *  the Asked-of badge read that field: moving the default moves it in every list holding such a
 *  row, not only in the two the event names. An issue row that does not say who holds its issue
 *  is counted as one nobody holds. */
function inboxRowsDecidedByDefault(project: string): RefreshTarget {
  return {
    queryKey: inboxKey,
    when: (data) =>
      rowsOf<InboxRow>(data).some(
        (row) =>
          openAndNotHandedOn(row) &&
          askProject(row) === project &&
          (row.issue_key === null || (row.issue?.assignee ?? null) === null)
      ),
  };
}

/** Every ask list of an issue of `project` that holds an open question nobody handed on
 *  (`openAndNotHandedOn`): the issue's (`["asks", key]`, which the margin and a document's ask
 *  blocks read) and the Conversation tab's (`["asks", key, "thread"]`, beneath it). Each row
 *  carries the person the server resolved for it (`waiting_on_person`), which the Asked-of badge
 *  on the issue's page reads, so a default move changes it for every such question on an issue
 *  nobody holds. An issue key's project is the part before its first hyphen (`issueProject`).
 *  The rows do not say who holds their issue, so the list of an issue a person holds reads again
 *  too, which answers the same rows. */
function issueAskListsDecidedByDefault(project: string): RefreshTarget {
  return {
    queryKey: ["asks"],
    when: (data, query) => {
      const key = query.queryKey[1];
      return (
        typeof key === "string" &&
        issueProject(key) === project &&
        rowsOf<Ask>(data).some(openAndNotHandedOn)
      );
    },
  };
}

/** Every project document's ask list (`["artifact", id, "asks"]`, which the document's page reads
 *  its ask blocks and its margin's cards from) that holds an open question nobody handed on
 *  (`openAndNotHandedOn`): an ask on a project document waits on that project's default until
 *  somebody hands it on. A document of an issue reads its issue's list instead (`ProofDocument`).
 *  Of any project, since the cache cannot tell which: the key names the document by id alone, a
 *  row names its project only for an anchored ask (`anchor_artifact`), and the document's own
 *  read, which carries it, is another query, which a test of this one cannot see. A list of
 *  another project's document reads again and answers the same rows. */
const DOCUMENT_ASK_LISTS_DECIDED_BY_DEFAULT: RefreshTarget = {
  queryKey: ["artifact"],
  when: (data, query) =>
    query.queryKey.length === 3 &&
    query.queryKey[2] === "asks" &&
    rowsOf<Ask>(data).some(openAndNotHandedOn),
};

/** The project the ask of a cached thread names, or undefined when it names none: its issue's
 *  (`issueProject`), else the project of the document an anchored ask or an approval request sits
 *  on. A block ask on a project document names its document by id alone (`block_artifact`). */
function threadAskProject(ask: AskRead["ask"]): string | undefined {
  if (ask.issue_key !== null) return issueProject(ask.issue_key);
  return ask.anchor_artifact?.project ?? ask.approval_artifact?.project;
}

/** Every cached ask thread (`["ask-thread", id]`, from which the Inbox's Reply-thread card draws
 *  its question's Asked-of badge, and which never goes stale on its own) whose question of
 *  `project` is open with nobody handed on (`openAndNotHandedOn`). The thread does not say who
 *  holds its issue, so the thread of an issue a person holds reads again too, which answers the
 *  same badge. A thread whose ask names no project (`threadAskProject`) reads again whichever
 *  project's it is: its project is in the document's own read, another query, which a test of
 *  this one cannot see. A thread holding no ask (an error, never fetched) draws no badge. */
function askThreadsDecidedByDefault(project: string): RefreshTarget {
  return {
    queryKey: ["ask-thread"],
    when: (data) => {
      const ask = (data as AskRead | undefined)?.ask;
      if (ask === undefined || !openAndNotHandedOn(ask)) return false;
      const named = threadAskProject(ask);
      return named === undefined || named === project;
    },
  };
}

/** Every cached ask thread (`["ask-thread", id]`, the Reply-thread card's) whose question is on
 *  issue `key` and is open with nobody handed on (`openAndNotHandedOn`): the issue's assignee is
 *  the person it waits on, so moving the assignee changes its Asked-of badge. */
function askThreadsDecidedByAssignee(key: string): RefreshTarget {
  return {
    queryKey: ["ask-thread"],
    when: (data) => {
      const ask = (data as AskRead | undefined)?.ask;
      return ask !== undefined && ask.issue_key === key && openAndNotHandedOn(ask);
    },
  };
}

/** The thread rows one of the Inbox's two reads holds: its own unpaged list, or the pages of
 *  what the reader has dismissed (`dismissedThreadsQuery`, an infinite read, whose data is its
 *  pages). */
function threadRowsOf(data: unknown): readonly MyReplyRow[] {
  const read = data as { pages?: readonly MyRepliesResponse[]; rows?: unknown } | undefined;
  const pages = read?.pages;
  if (Array.isArray(pages)) return pages.flatMap((page) => rowsOf<MyReplyRow>(page.rows));
  return rowsOf<MyReplyRow>(read?.rows);
}

/** The Inbox's own thread reads (`inboxThreadsQuery` and `dismissedThreadsQuery`, so both the
 *  Inbox and the Dismissed view), when one of their rows is a thread on issue `key`: a Reply row
 *  shows its issue's title and priority, and whether it is listed at all turns on who waits on
 *  the ask (`myRepliesOwner` in the server's replies.go leaves out an open ask on an open issue
 *  that waits on the reader, which the Inbox lists under Waiting on you instead), which an
 *  assignee move changes for every ask nobody has handed on. Built like
 *  `inboxRowsOf`: it needs no field of the event's, so an older server's event reaches it too. */
function inboxThreadRowsOf(key: string): RefreshTarget {
  return {
    queryKey: inboxThreadsKey,
    when: (data) =>
      threadRowsOf(data).some((row) => row.kind !== "agent" && row.issue?.key === key),
  };
}

/** The Architecture page's tree of `project`, when it lists issue `key`: in a component's work
 *  list, which shows the issue's title, status, priority, parent, links and `updated_at`, or among
 *  the unassigned, the not-architectural and the retired links, which show its title and status. */
function architectureListing(project: string, key: string): RefreshTarget {
  const lists = (rows: unknown) =>
    rowsOf<ArchitectureTreeIssueRef>(rows).some((issue) => issue.key === key);
  return {
    exact: true,
    queryKey: ["architecture", project],
    when: (data) => {
      const tree = data as Partial<ArchitectureTree> | undefined;
      return (
        rowsOf<ArchitectureTreeComponent>(tree?.components).some((component) =>
          lists(component.issues)
        ) ||
        lists(tree?.unassigned) ||
        lists(tree?.not_architectural) ||
        lists(tree?.retired_links)
      );
    },
  };
}

/** Every backlink panel (`ReferencedBy`'s `["references", "to", ref]`) whose read `matches`. */
function referencePanels(matches: (references: GraphReferences) => boolean): RefreshTarget {
  return {
    queryKey: ["references", "to"],
    when: (data) => data !== undefined && matches(data as GraphReferences),
  };
}

/** The backlink panel of exactly one reference, as `ReferencedBy` keys it. */
function referencePanel(reference: string): RefreshTarget {
  return { exact: true, queryKey: ["references", "to", reference] };
}

// The document an ask or comment event touches: a block ask names the document that holds its
// block, an anchored ask or comment names the document its mark lives in, and an ask or comment
// with neither touches no document at all.
function eventDocument(event: DispatchEvent): string | undefined {
  switch (event.type) {
    case "ask.opened":
    case "ask.edited":
    case "ask.handed_back":
    case "ask.answered":
    case "ask.resolved":
    case "ask.anchor_refreshed":
      return event.payload.block_artifact?.id ?? event.payload.anchor?.artifact_id;
    case "comment.created":
    case "comment.delivery":
    case "comment.answered":
    case "comment.resolved":
    case "comment.reopened":
    case "comment.edited":
    case "comment.deleted":
    case "comment.anchor_refreshed":
    case "suggestion.accepted":
    case "suggestion.rejected":
      return "anchor" in event.payload ? event.payload.anchor?.artifact_id : undefined;
    default:
      return undefined;
  }
}

// Follower events carry the ask under `ask_id`; every other ask event is the ask itself and
// carries its `id`. Both refresh the ask's read (a reference to it, an ask page) and its thread,
// which lists followers.
function appendAskReads(targets: RefreshTarget[], event: DispatchEvent): void {
  const id =
    event.type === "ask.follower_added" || event.type === "ask.follower_removed"
      ? payloadString(event, "ask_id")
      : payloadString(event, "id");
  if (id !== undefined) {
    targets.push({ queryKey: ["ask", id] }, { queryKey: ["ask-thread", id] });
  }
}

// A comment event names the comment (a delivery receipt as `comment_id`), the root it replies to,
// whose read carries its replies, and the ask whose thread holds it. An edit rewrites the
// comment's blocks, so every read of one of them; a reply that names a block moves that block's
// read, which lists the reply.
function appendCommentReads(targets: RefreshTarget[], event: DispatchEvent): void {
  const id =
    event.type === "comment.delivery" ? event.payload.comment_id : payloadString(event, "id");
  if (id !== undefined) {
    targets.push({ queryKey: ["comment", id] });
    if (event.type === "comment.edited" || event.type === "comment.deleted") {
      targets.push(itemBlockReads("comment", id));
    }
  }
  const root = payloadString(event, "reply_to");
  if (root !== undefined) {
    targets.push({ queryKey: ["comment", root] });
  }
  const askID = payloadString(event, "ask_id");
  if (askID !== undefined) {
    targets.push({ queryKey: ["ask", askID] }, { queryKey: ["ask-thread", askID] });
  }
  appendRepliedBlock(targets, event);
}

/**
 * Every read of a block of comment or issue message `id` (`blockQuery`, which each link to the
 * block and its own panel share, pinned or not), found by its node, `<kind>/<id>#<block id>`.
 * The node carries the block's text, which the read keeps until an event refreshes it
 * (`staleTime: Infinity`), and the server names no block of a post in `references_changed`: the
 * post's own edit does, and its delete, which re-indexes the post with an empty body. A message
 * has both since the post corrections gave it them.
 */
function itemBlockReads(kind: "comment" | "message", id: string): RefreshTarget {
  const prefix = `${kind}/${id}#`;
  return referencePanels(({ node }) => node.kind === "block" && node.id.startsWith(prefix));
}

/** The read of the block of a comment or message a reply names (`reply_block`, its node id),
 *  which lists the reply under the block. */
function appendRepliedBlock(targets: RefreshTarget[], event: DispatchEvent): void {
  const replied = payloadValue(event, "reply_block");
  const node =
    typeof replied === "object" && replied !== null ? Reflect.get(replied, "node") : undefined;
  if (typeof node === "string") {
    targets.push(referencePanels((read) => read.node.kind === "block" && read.node.id === node));
  }
}

/** The signed-in person's answers page lists every answer and every reply on an ask, and prints
 *  each reply's body with its edit and delete marks, so the two events that write one refresh
 *  it, and so do the edit and the delete of a reply on an ask. The query exists only while that
 *  page is open. */
function appendAnswersPage(targets: RefreshTarget[], event: DispatchEvent): void {
  if (
    event.type === "ask.answered" ||
    ((event.type === "comment.created" ||
      event.type === "comment.edited" ||
      event.type === "comment.deleted") &&
      payloadString(event, "ask_id") !== undefined)
  ) {
    targets.push({ queryKey: ["me", "answers"] });
  }
}

// An ask or comment that is written, re-anchored or edited in a document is an `anchored_to` edge
// into it, and one a project document owns an `owned_by` edge: that document's backlink panel
// lists both, each row showing the head of the item's text, which an edit moves and a comment's
// delete clears. A resolution, a reopen, an answer, a hand-back or a suggestion's verdict moves
// neither the edge nor its text.
function appendDocumentPanel(targets: RefreshTarget[], event: DispatchEvent): void {
  if (
    event.type !== "ask.opened" &&
    event.type !== "ask.edited" &&
    event.type !== "ask.anchor_refreshed" &&
    event.type !== "comment.created" &&
    event.type !== "comment.answered" &&
    event.type !== "comment.edited" &&
    event.type !== "comment.deleted" &&
    event.type !== "comment.anchor_refreshed"
  ) {
    return;
  }
  const documents = new Set([eventDocument(event), event.artifact_id ?? undefined]);
  for (const id of documents) {
    if (id !== undefined) {
      targets.push(referencePanels(({ node }) => node.kind === "artifact" && node.id === id));
    }
  }
}

// The ask lifecycle events, listed rather than matched on the `ask.` prefix so a new one falls
// to the default branch instead of silently routing here.
function isAskEvent(event: DispatchEvent): boolean {
  return (
    event.type === "ask.opened" ||
    event.type === "ask.anchor_refreshed" ||
    event.type === "ask.edited" ||
    event.type === "ask.handed_back" ||
    event.type === "ask.asked_of" ||
    event.type === "ask.answered" ||
    event.type === "ask.resolved" ||
    event.type === "ask.follower_added" ||
    event.type === "ask.follower_removed"
  );
}

/** The ask events that open or close an ask: the sidebar's per-project open-ask counts and an
 *  issue's pinned row (`open_asks`) move with them. An edit, a hand-back, a hand-on to another
 *  person, an anchor refresh or a follower change moves neither: the ask stays open, and only
 *  whose list it sits in can change. A block event can open or retract a decision block's ask. */
function movesOpenAsks(event: DispatchEvent): boolean {
  return (
    event.type === "ask.opened" ||
    event.type === "ask.answered" ||
    event.type === "ask.resolved" ||
    event.type === "block.repaired" ||
    event.type === "block.invalid"
  );
}

// The events that change a comment thread: creation, deliveries, lifecycle, suggestion
// verdicts, and a cascaded anchor refresh (a document version orphaned or re-anchored it).
function isCommentLikeEvent(event: DispatchEvent): boolean {
  return (
    event.type === "comment.created" ||
    event.type === "comment.delivery" ||
    event.type === "comment.answered" ||
    event.type === "comment.resolved" ||
    event.type === "comment.reopened" ||
    event.type === "comment.edited" ||
    event.type === "comment.deleted" ||
    event.type === "comment.anchor_refreshed" ||
    event.type === "suggestion.accepted" ||
    event.type === "suggestion.rejected"
  );
}

/** What an Inbox row reads of an ask or comment event: every ask event moves its ask's row (a
 *  follower, a hand-back between sections, an edit), and a reply in an ask's thread moves its
 *  `last_reply` and `waiting_on`. A comment outside every ask thread, and every message, moves no
 *  row: both write paths file a reply under the ask at the head of its thread (`ask_id`), and a
 *  delivery receipt names that ask too. A citation of an ask moves its row's backlink count, which
 *  the targets the event names restore (`changedReferenceTargets`). */
function movesInboxRows(event: DispatchEvent): boolean {
  return (
    isAskEvent(event) ||
    (isCommentLikeEvent(event) && payloadString(event, "ask_id") !== undefined) ||
    event.type === "artifact.approved" ||
    event.type === "artifact.changes_requested" ||
    event.type === "block.repaired" ||
    event.type === "block.invalid"
  );
}

// The Agents page holds one conversation per session; `session:<id>` with an id after the prefix
// is the only target shape that names one. Every other target (a role, a name that never
// resolved) belongs to no conversation on that page.
function agentConversationKey(target: string | undefined): readonly unknown[] | undefined {
  if (target?.startsWith("session:") && target.length > "session:".length) {
    return agentMessagesQuery(target.slice("session:".length)).queryKey;
  }
  return undefined;
}

function appendAgentConversation(targets: RefreshTarget[], target: string | undefined): void {
  const queryKey = agentConversationKey(target);
  if (queryKey !== undefined) {
    targets.push({ queryKey });
  }
}

function isMessageEvent(event: DispatchEvent): boolean {
  return (
    event.type === "message.created" ||
    event.type === "message.delivery" ||
    event.type === "message.accepted" ||
    event.type === "message.answered" ||
    event.type === "message.edited" ||
    event.type === "message.deleted"
  );
}

// The writes that fire a `user_threads` trigger - a comment, a session's reply to a mention
// (`comment.answered`), an issue message or a reply to one, an answer - and an ask opening
// (settlement restoring one) or being resolved, which decides whether the Inbox lists its thread
// instead. A resolution of a comment, a delivery or a claim moves no row.
//
// An edit or a delete of a comment or an issue message (`comment.edited`, `comment.deleted`,
// `message.edited`, `message.deleted`) moves no row either, but it changes what a row prints: a
// Reply row draws its thread's root (`subject`, with the root's edit time and its delete marks,
// `ThreadTitle`) and its newest reply (`reply.first_line`), so an Inbox that did not read its rows
// again would go on printing a post's old words after its edit, and a deleted post's words after
// its delete (R2 and R3 of dispatch://INFRA-1523). The correction's `thread_logins` decides whose
// rows read again, as a create's does, and every viewer's do where it carries none.
//
// A hand-on (`ask.asked_of`) is here for the same reason an assignee move is: the Inbox leaves
// out the Reply row of an open ask it already lists (`myRepliesOwner` in the server's
// replies.go, over `coalesce(a.asked_of, i.assignee, p.default_assignee)`), so handing the ask
// from one person to another makes that row appear for the person who gave it up and disappear
// for the person who took it. The event names both of them in its `thread_logins`, which is
// what decides whose list refreshes below.
//
// An issue-less `message.created` or `message.answered` is here for the Inbox's Agent rows: a
// one-off message to a session, and that session's reply to one.
//
// A project's default assignee (`settings.project_default_assignee.updated`) is here for the
// third step of the same expression: an open ask on an issue nobody holds reaches that person
// (`coalesce(a.asked_of, i.assignee, p.default_assignee)`), and the Replies list leaves out an
// ask the Inbox already shows the reader, so moving the default moves those threads from one
// person's Reply rows into the other's. Its `thread_logins` names the previous default and the
// new one.
const INBOX_THREAD_WRITES: Partial<Record<EventType, true>> = {
  "comment.created": true,
  "comment.answered": true,
  "comment.edited": true,
  "comment.deleted": true,
  "message.created": true,
  "message.answered": true,
  "message.edited": true,
  "message.deleted": true,
  "ask.opened": true,
  "ask.answered": true,
  "ask.asked_of": true,
  "ask.resolved": true,
  "settings.project_default_assignee.updated": true,
};

/**
 * Whether the viewer's Inbox thread rows (`["me", "replies"]`: both states of the unpaged read,
 * the unread count and every owner's unread set) can have moved, or changed what they print (a
 * correction of a post a row draws): a write above, or an issue closing, reopening or being
 * reassigned. A reassignment, a hand-on (`ask.asked_of`, among the writes above) and a project's
 * default moving all move the same rows for the same reason: whom an open ask waits on is one
 * expression, `coalesce(a.asked_of, i.assignee, p.default_assignee)`, and the Reply rows leave out
 * an ask the Inbox already lists for the viewer (`myRepliesOwner` in the server's replies.go), so
 * moving that person moves the ask's thread into one person's Reply rows and out of the other's.
 *
 * A project-default write moves the person without touching the thread or naming an issue, so
 * it reads it for the two named people's whole Inbox as well, and every other reader's Inbox
 * tests the rows it holds (`inboxRowsDecidedByDefault`, from the project-default branch of
 * `ownerTargets`). An assignee write needs no such read: every reader's Inbox holds every open
 * ask of the issue, so the test against its rows (`inboxRowsOf`) finds each list it moves.
 *
 * `thread_logins` names whose rows the write moved or reprinted - for an issue-less message the
 * thread root's author, which is the person whose Agent row it is, for a hand-on the person it
 * moved from and the one it moved to, and for a project default the previous default and the new
 * one - so only theirs, and the writer's own, refresh. Without it every one of those writes
 * refreshes, except an `issue.updated` that says it moved neither status nor assignee: the server
 * names the logins on every status move, a close and a reopen among them, and on every assignee
 * move.
 *
 * The known limit: an older server mid-deploy writes no `thread_logins` on a reassignment, so a
 * Reply row that should appear here - an open ask on an issue assigned away from the viewer -
 * does not until the next list read. The conditional target beside this rule
 * (`inboxThreadRowsOf`) covers the other direction, where a listed row carries the issue's key.
 */
function movesInboxThreads(event: DispatchEvent, viewer: string | undefined): boolean {
  const issueWrite = event.type === "issue.closed" || event.type === "issue.updated";
  if (INBOX_THREAD_WRITES[event.type] !== true && !issueWrite) {
    return false;
  }
  const logins = payloadStrings(event, "thread_logins");
  if (logins === undefined) {
    if (event.type !== "issue.updated") return true;
    const changes = issueChanges(event);
    return changes === undefined || changes.has("status") || changes.has("assignee");
  }
  if (viewer === undefined) {
    return true;
  }
  return (
    logins.some((login) => isViewer(login, viewer)) ||
    (event.actor.kind === "user" && isViewer(event.actor.id, viewer))
  );
}

/** The fields an `issue.created|updated|closed` write moved, or undefined when the event does not
 *  say, in which case any of them may have. */
function issueChanges(event: DispatchEvent): ReadonlySet<string> | undefined {
  const changed = payloadStrings(event, "changed");
  return changed === undefined ? undefined : new Set(changed);
}

/**
 * Every event whose server path rewrites the issue's own reference edges: a document version,
 * upload or title correction, which moves the title every reference to the document shows; an
 * ask's creation or edit (a settled ask block among them), a comment or message body -
 * an answer's reply body is indexed like any other - an anchor refresh, a follower change, a child
 * link, and an issue write that moved a structural edge (its parent, its blockers, its components)
 * or the title every one of them shows as its excerpt.
 */
function changesReferenceGraph(event: DispatchEvent): boolean {
  switch (event.type) {
    case "artifact.created":
    case "artifact.renamed":
    case "artifact.version":
    case "ask.anchor_refreshed":
    case "ask.edited":
    case "ask.follower_added":
    case "ask.follower_removed":
    case "ask.opened":
    case "child.added":
    case "child.removed":
    case "comment.answered":
    case "comment.anchor_refreshed":
    case "comment.created":
    case "comment.edited":
    case "comment.deleted":
    case "issue.created":
    case "message.answered":
    case "message.created":
    case "message.edited":
    case "message.deleted":
      return true;
    case "issue.updated": {
      const changes = issueChanges(event);
      return (
        changes === undefined ||
        ["parent", "blocked_by", "components", "title"].some((field) => changes.has(field))
      );
    }
    default:
      return false;
  }
}

/**
 * What an `issue.created`, `issue.updated` or `issue.closed` of `key` refreshes beyond what every
 * event of the issue does.
 */
function issueWriteTargets(
  event: DispatchEvent,
  key: string,
  project: string | undefined
): RefreshTarget[] {
  const changes = issueChanges(event);
  const moved = (field: string) => changes === undefined || changes.has(field);
  const created = event.type === "issue.created";
  const closed = event.type === "issue.closed";
  const ancestors = payloadStrings(event, "ancestors");
  const targets: RefreshTarget[] = [];

  // Issue details. Components, the repository and the parent chain are resolved on read and
  // inherited downward, so moving one changes every descendant's detail, which gets no event of
  // its own: every loaded detail refreshes. Otherwise every write moves the issue's `updated_at`,
  // the newest of which each ancestor's Children tab shows as its child's activity (`active_at`
  // of the whole subtree), and a status rolls up into every ancestor's progress and child counts:
  // every ancestor's detail refreshes. A new issue has no descendants.
  if (changes === undefined || ancestors === undefined) {
    targets.push({ queryKey: ["issue"] });
  } else if (!created && (moved("parent") || moved("components") || moved("repo"))) {
    targets.push({ queryKey: ["issue"] });
  } else {
    for (const ancestor of ancestors) {
      targets.push({ exact: true, queryKey: ["issue", ancestor] });
    }
  }

  const titleOrStatus = moved("title") || moved("status");
  // A reference to the issue, or to one of its documents, shows its title and status.
  if (titleOrStatus) {
    targets.push(issueSummaryOf(key, event));
  }
  // The Reference picker's project list and the Agents page's open-issue picker list titles and
  // whether an issue is open.
  if (created || closed || titleOrStatus) {
    if (project !== undefined) {
      targets.push({ exact: true, queryKey: ["issues", project] });
    }
    targets.push({ exact: true, queryKey: ["agents", "issue-picker"] });
  }
  if (!created && titleOrStatus) {
    targets.push(pinnedRow(key));
  }
  // An Inbox thread row shows its thread's issue title and its priority, and an assignee move
  // decides whether the thread of an open ask nobody has handed on is listed here at all. The
  // assignee rule is the conditional half of the belt `movesInboxThreads` describes: it needs no
  // field the server only recently writes, so it catches a reassignment an older server's event
  // says nothing of, in the one direction a listed row can be tested for.
  if (!created && (moved("title") || moved("priority") || moved("assignee"))) {
    targets.push(inboxThreadRowsOf(key));
  }

  // An Inbox row shows its issue's title, status, priority and assignee, and lists an open ask
  // only while its issue is open. A close drops the issue's rows, which a test against them finds.
  // A reopen (`closed_at` moved, the issue left open) puts its open asks back, and the Inbox holds
  // no row of a closed issue to test, so that refreshes it whole.
  if (!created) {
    if (moved("closed_at") && !closed) {
      targets.push({ queryKey: inboxKey });
    } else if (
      closed ||
      moved("title") ||
      moved("status") ||
      moved("priority") ||
      moved("assignee")
    ) {
      targets.push(inboxRowsOf(key));
    }
  }

  // The Reply-thread card draws a question's Asked-of badge from its thread (`["ask-thread", id]`,
  // never stale on its own), and an open question of the issue nobody handed on waits on its
  // assignee.
  if (!created && moved("assignee")) {
    targets.push(askThreadsDecidedByAssignee(key));
  }

  // The sidebar's per-project open-ask count counts the open asks of open issues: only a close and
  // a reopen move it. A new issue has no asks.
  if (closed || (created ? changes === undefined : moved("closed_at"))) {
    targets.push({ queryKey: projectsKey });
  }
  // A status move, a new attachment or a new parent moves a count in the component tree. Any
  // other write moves at least the issue's `updated_at`, which the tree shows where it lists the
  // issue.
  if (project !== undefined) {
    if (moved("status") || moved("parent") || moved("components")) {
      targets.push({ queryKey: ["components", project] }, { queryKey: ["architecture", project] });
    } else if (!created) {
      targets.push(architectureListing(project, key));
    }
  }

  // The Priorities page's rows and the rail's two counts (`["priorities"]`, both the full body
  // and the summary under it), and the work listed under one priority (`["issues", "advances",
  // <id>]`, every scoped issues table's read). What the work under a priority is moves with the
  // relation itself, with the status each row counts by, and with the parent, which is what an
  // issue inherits its priorities down. The scoped listing is refreshed by its prefix rather
  // than by the priority the event names: an issue carries its own relations and the ones it
  // inherits, so a write can move the work under a priority the payload never names. The
  // relation is no edge of the reference graph (`changesReferenceGraph` leaves `advances` out),
  // so no backlink panel reads it.
  if (moved("advances") || moved("status") || moved("parent")) {
    targets.push({ queryKey: ["priorities"] }, { queryKey: ["issues", "advances"] });
  }

  // A `child_of` edge into the parent and a `blocked_by` edge into each blocker are rows of their
  // backlink panels: the new parent and blockers are the payload's, the old ones are the panels
  // that still list an edge from this issue.
  if (moved("parent") || moved("blocked_by")) {
    const parent = payloadString(event, "parent");
    const blockers = payloadStrings(event, "blocked_by") ?? [];
    targets.push(
      referencePanels(
        ({ edges, node }) =>
          node.kind === "issue" &&
          (node.id === parent ||
            blockers.includes(node.id) ||
            (edges ?? []).some(
              (edge) =>
                edge.node.kind === "issue" &&
                edge.node.id === key &&
                (edge.kind === "child_of" || edge.kind === "blocked_by")
            ))
      )
    );
  }
  return targets;
}

/**
 * What one write of priority `id` changes, by which of the three writes it was. Every one moves
 * the priorities reads (`["priorities"]`: the Priorities page's body `["priorities", "full"]`,
 * the rail's two counts `["priorities", "summary"]`, and the priority page's margin
 * `["priorities", "history", id]`), which is also where every page reads a priority's title and
 * its place in the order from. A create, a rename, a move or a description moves nothing else:
 * an issue row carries a priority by its id alone (`{id, inherited}`), so no issue listing
 * reads again on them, and the description's autosave refetches no fault list.
 *
 * A delete takes the priority out of every relation, and the server writes no issue event for
 * the issues it leaves, so it also moves:
 *
 * - the work listed under it (`["issues", "advances", id]`), which the priority page's work
 *   table and its scope links read, now empty;
 * - the work under no priority (`["issues", "advances", "none"]`): an issue that advanced only
 *   this priority joins it, and the cache cannot tell which issues those are, since no row of
 *   that listing names the priority;
 * - every read that holds an issue advancing it, by its own relation or one it inherits
 *   (`advances` lists both): an issue read (`["issue", key]`; an issue's references or its
 *   collaborators under that prefix carry no `advances`), and a list of issue rows (every
 *   `["issues", ...]` listing and the Agents page's issue picker). That is the parked fault list
 *   (`advances=any`), whose rows under this priority alone leave it and whose other rows drop
 *   it, every other issue listing that draws the row, and above all each open issue read, whose
 *   header would otherwise keep naming the priority and send it back in the next whole-set write
 *   of its `advances`, which the server refuses (`400 ADVANCES_INPUT`, a priority that is gone).
 *   A row of `advances=any` that does not name it stays, since it still advances another.
 */
function priorityWriteTargets(
  event: Extract<DispatchEvent, { type: "priority.updated" }>
): RefreshTarget[] {
  const { change, id } = event.payload;
  const targets: RefreshTarget[] = [{ queryKey: ["priorities"] }];
  if (change !== "deleted") {
    return targets;
  }
  const advancesIt = (row: unknown): boolean =>
    typeof row === "object" &&
    row !== null &&
    ((row as Pick<IssueSummary, "advances">).advances ?? []).some((entry) => entry.id === id);
  const when: RefreshCheck = (data) =>
    Array.isArray(data) ? data.some(advancesIt) : advancesIt(data);
  targets.push(
    { queryKey: ["issues", "advances", id] },
    { queryKey: ["issues", "advances", "none"] },
    { queryKey: ["issue"], when },
    { queryKey: ["issues"], when },
    { exact: true, queryKey: ["agents", "issue-picker"], when }
  );
  return targets;
}

/** Every question map a reader holds (`["references", "questions", <kind>, <id>]`, the read
 *  behind a `#id` in any text). The read is keyed by the source whose text was read, and
 *  neither a version nor an ask names those sources, so the prefix is refreshed rather than one
 *  key. */
const EVERY_QUESTION_MAP: RefreshTarget = { queryKey: ["references", "questions"] };

/** Whether the event can change what a `#id` names: a new version of a document, which decides
 *  which `:::ask{#id}` blocks the latest version declares, or an ask settling into a block,
 *  leaving one or moving - every ask event but a follower's arrival or departure, which moves
 *  no block. */
function changesQuestionMaps(event: DispatchEvent): boolean {
  return (
    event.type === "artifact.version" ||
    (isAskEvent(event) &&
      event.type !== "ask.follower_added" &&
      event.type !== "ask.follower_removed")
  );
}

/**
 * Whether the event can change who the issue's collaborators are, the server's own list of what
 * makes one (`collaboratorEventReasons` in
 * packages/envoy/internal/dispatch/api/issue_collaborators.go): a claim, a
 * comment, a message, an ask, a document version, and an answer to any of them. An issue
 * document's events carry the issue's key, so the version here is that issue's.
 *
 * The assignee is a collaborator with no write of their own, so an `issue.updated` that moved
 * `assignee` - or that does not say what it moved - changes the list too.
 */
function changesCollaborators(event: DispatchEvent): boolean {
  switch (event.type) {
    case "artifact.version":
    case "ask.answered":
    case "ask.opened":
    case "comment.answered":
    case "comment.created":
    case "issue.claimed":
    case "message.answered":
    case "message.created":
      return true;
    case "issue.updated": {
      const changes = issueChanges(event);
      return changes === undefined || changes.has("assignee");
    }
    default:
      return false;
  }
}

/**
 * Whether the event comes with the Legion daemon writing a step record of the issue. A step write
 * appends no event of its own (packages/envoy/internal/dispatch/api/steps.go), so the Steps tab
 * is refreshed by what the daemon writes beside it as a phase moves: a claim or a release, as a
 * role's run starts or ends; a handoff, the report a role writes as its step ends; a close, which
 * ends the merge; and an issue write that moved the status or the route, or that does not say
 * what it moved.
 */
function changesSteps(event: DispatchEvent): boolean {
  switch (event.type) {
    case "handoff.recorded":
    case "issue.claimed":
    case "issue.closed":
    case "issue.released":
      return true;
    case "issue.updated": {
      const changes = issueChanges(event);
      return changes === undefined || changes.has("status") || changes.has("route");
    }
    default:
      return false;
  }
}

/** What a project document's event refreshes: an ask, a comment, a version or a review on a
 *  document that belongs to no issue. */
function documentTargets(event: DispatchEvent, viewer: string | undefined): RefreshTarget[] {
  if (event.artifact_id === null || event.artifact_id === undefined) {
    throw new Error("document event is missing its artifact id");
  }
  if (event.project === undefined) {
    throw new Error("document event is missing its project");
  }
  const id = event.artifact_id;
  const targets: RefreshTarget[] = [
    { queryKey: ["artifact", id] },
    { queryKey: ["project", event.project, "artifacts"] },
    // The document page reads the document by `project/slug`, which the event does not name; its
    // id is the one the event does.
    {
      queryKey: ["artifact-ref"],
      when: (data) => data === undefined || (data as Artifact).id === id,
    },
  ];
  if (changesQuestionMaps(event)) {
    targets.push(EVERY_QUESTION_MAP);
  }
  // A reference to the document shows its name, kind and latest version. A correction moves the
  // name it shows. A new document can be the one a reference that found nothing named (no data
  // to test), so it refreshes those too.
  if (
    event.type === "artifact.version" ||
    event.type === "artifact.created" ||
    event.type === "artifact.renamed"
  ) {
    const created = event.type === "artifact.created";
    targets.push({
      queryKey: ["doc-summary", event.project],
      when: (data) =>
        (created && data === undefined) ||
        (data as DocumentReferenceSummary | undefined)?.id === id,
    });
  }
  if (isAskEvent(event)) {
    appendAskReads(targets, event);
  }
  if (isCommentLikeEvent(event)) {
    appendCommentReads(targets, event);
  }
  if (movesInboxRows(event)) {
    targets.push({ queryKey: inboxKey });
  }
  if (movesOpenAsks(event)) {
    targets.push({ queryKey: projectsKey });
  }
  if (movesInboxThreads(event, viewer)) {
    targets.push({ queryKey: myRepliesKey });
  }
  appendAnswersPage(targets, event);
  appendDocumentPanel(targets, event);
  if (event.type === "subscription.removed") {
    targets.push({ queryKey: ["subscribers", id] });
  }
  return targets;
}

function ownerTargets(event: DispatchEvent, viewer: string | undefined): RefreshTarget[] {
  if (
    event.type === "project.created" ||
    event.type === "project.updated" ||
    event.type === "settings.repo_project.updated"
  ) {
    if (event.project === undefined) {
      throw new Error("project event is missing its project");
    }
    return [
      { queryKey: projectsKey },
      { queryKey: ["project", event.project] },
      { queryKey: ["issues", "project", event.project] },
      { queryKey: ["repo-projects"] },
      // The project list carries no default assignee: a new project's is in the defaults read
      // alone, which the Settings table reads it from and was answered before it existed.
      ...(event.type === "project.created"
        ? [{ queryKey: projectDefaultAssigneesQuery().queryKey }]
        : []),
      // A mapping write can change the repository every issue of its project resolves to (a new
      // default, one removed or moved away), which the issue header reads from the issue's
      // detail. The detail's key names no project, so every loaded detail refreshes; mapping
      // writes are rare settings changes.
      ...(event.type === "settings.repo_project.updated" ? [{ queryKey: ["issue"] }] : []),
    ];
  }
  // The Settings table always, and the Inbox, issue, document and Reply-thread reads of a question
  // the move carries: a project's default is the last step of the chain that decides who a
  // question reaches, so every open ask of the project nobody handed on, on an issue nobody holds
  // or on a project document, now waits on the new default. Every reader's Inbox holds those rows
  // with the person they wait on, which its assignee filter and Asked-of badge read, so a reader
  // whose list holds one reads it again whoever she is (`inboxRowsDecidedByDefault`). The issue
  // page and the document page draw the same badge from their own ask lists, which read again
  // when they hold such a question (`issueAskListsDecidedByDefault`,
  // `DOCUMENT_ASK_LISTS_DECIDED_BY_DEFAULT`), and the Inbox's Reply-thread card draws it from the
  // question's thread, which reads again when it holds one (`askThreadsDecidedByDefault`). The
  // two people `thread_logins` names (`movesInboxThreads`) read their whole Inbox and their Reply
  // rows as well, since the move carries those threads between their Replies, and a reader whose
  // reads hold no such question and whom the write does not name sends no read.
  if (event.type === "settings.project_default_assignee.updated") {
    const targets: RefreshTarget[] = [
      { queryKey: projectDefaultAssigneesQuery().queryKey },
      inboxRowsDecidedByDefault(event.payload.project),
      issueAskListsDecidedByDefault(event.payload.project),
      DOCUMENT_ASK_LISTS_DECIDED_BY_DEFAULT,
      askThreadsDecidedByDefault(event.payload.project),
    ];
    if (movesInboxThreads(event, viewer)) {
      targets.push({ queryKey: inboxKey }, { queryKey: myRepliesKey });
    }
    return targets;
  }
  if (
    event.type === "settings.architecture_source.updated" ||
    event.type === "architecture.synced" ||
    event.type === "architecture.sync_failed"
  ) {
    if (event.project === undefined) {
      throw new Error("project event is missing its project");
    }
    return [
      { queryKey: architectureSourcesQuery().queryKey },
      { queryKey: projectArchitectureSourcesQuery(event.project).queryKey },
      { queryKey: ["components", event.project] },
      // A re-import changes which components exist, so every count in the tree.
      { queryKey: ["architecture", event.project] },
    ];
  }
  if (event.type === "user_state.updated") {
    // A pin, a read mark, a dismissal or a snooze in another tab or device of this viewer: the
    // state's own read always (its `seq` moved), the pinned list only for a pin. Of these the Inbox
    // reads only a snooze, its row's `snoozed_until`, from which its Later band, its banner and the
    // Needs-you badge are all counted: every Inbox read, since the event names no ask (every
    // signed-in feed carries it). An event without `changed` predates snooze events.
    if (!isViewer(payloadString(event, "login"), viewer)) {
      return [];
    }
    const changed = payloadStrings(event, "changed");
    const targets: RefreshTarget[] = [{ queryKey: userStateQuery().queryKey }];
    if (changed === undefined || changed.includes("pinned")) {
      targets.push({ exact: true, queryKey: pinnedKey });
    }
    if (changed?.includes("snoozed") === true) {
      targets.push({ queryKey: inboxKey });
    }
    return targets;
  }
  if (event.type === "user_agent_state.updated") {
    // Another tab or device of this viewer read or cleared a conversation: refresh the badge,
    // and the Inbox's thread rows with it - an Agent row's unread replies are that state's, so
    // a read made on the Agents page or in the live view takes the row out of the list here.
    return isViewer(payloadString(event, "login"), viewer)
      ? [{ queryKey: userAgentStateQuery().queryKey }, { queryKey: myRepliesKey }]
      : [];
  }
  if (event.type === "user_thread_state.updated") {
    // Another tab or device of this viewer read a thread: drop its row and its count here too.
    // The event names one thread, or none at all - a Mark all read names its project and no
    // thread - and either way every one of the viewer's thread reads is refreshed.
    return isViewer(payloadString(event, "login"), viewer) ? [{ queryKey: myRepliesKey }] : [];
  }
  if (event.type === "priority.updated") {
    // A create, a rename, a move, a description or a delete of a priority, by anybody.
    return priorityWriteTargets(event);
  }
  if (event.type === "subscription.remove_requested") {
    return [];
  }
  if (event.issue_key === null && isMessageEvent(event)) {
    const queryKey = agentConversationKey(payloadString(event, "target"));
    if (queryKey === undefined) {
      throw new Error("issue-less message event is missing its session target");
    }
    // Every recipient of a broadcast holds an issue-less targeted message, so every event
    // that moves a broadcast's recipient table arrives here. The open broadcast views are
    // refreshed by key prefix rather than by threading a broadcast id through the message,
    // delivery and reply payloads; nothing else is keyed under ["broadcast"], and the
    // queries only exist while a broadcast page is mounted. A reply in a direct conversation can
    // be unread for its asker, and the count lives in the viewer's per-agent state; every reply
    // row is written with a message.answered event, so only that event can move the count.
    //
    // The Inbox's Agent rows are the same two writes: the message that opens one and the reply
    // that makes it unread. `thread_logins` names the thread root's author, whose row it is.
    const targets: RefreshTarget[] = [{ queryKey }, { queryKey: ["broadcast"] }];
    if (event.type === "message.answered") {
      targets.push({ queryKey: userAgentStateQuery().queryKey });
    }
    if (movesInboxThreads(event, viewer)) {
      targets.push({ queryKey: myRepliesKey });
    }
    return targets;
  }
  if (event.issue_key === null) {
    return documentTargets(event, viewer);
  }

  const key = event.issue_key;
  const project = event.project;
  const issueReference = buildDispatchReference({ key, kind: "issue" });
  const targets: RefreshTarget[] = [
    // The issue's own detail; a read under it (`["issue", key, "references"]`) has its own rule.
    { exact: true, queryKey: ["issue", key] },
    { queryKey: ["events", key] },
    // Every event an issue owns moves its `last_seq`, which a project list's unread dot reads.
    { queryKey: project === undefined ? ["issues", "project"] : ["issues", "project", project] },
  ];
  if (movesInboxThreads(event, viewer)) {
    targets.push({ queryKey: myRepliesKey });
  }
  if (changesReferenceGraph(event)) {
    targets.push({ exact: true, queryKey: ["issue", key, "references"] });
  }
  if (changesQuestionMaps(event)) {
    targets.push(EVERY_QUESTION_MAP);
  }
  if (changesCollaborators(event)) {
    // The Collaborators tab's read and the composer's "Not running" options, named here because
    // every issue target is `exact` and because the branches below return before this point - a
    // claim's among them, and a claim is what makes a Legion agent a collaborator.
    targets.push({ exact: true, queryKey: ["issue", key, "collaborators"] });
  }
  if (changesSteps(event)) {
    // The Steps tab's read, named here for the same two reasons as the collaborators above.
    targets.push({ exact: true, queryKey: ["issue", key, "steps"] });
  }
  if (event.type === "decision.kept") {
    // A Keep moves one decision's row of the issue's Decisions view and nothing else: it writes
    // no document version, no ask and no issue field.
    targets.push({ exact: true, queryKey: ["decisions", key] });
    return targets;
  }

  if (
    event.type === "issue.created" ||
    event.type === "issue.updated" ||
    event.type === "issue.closed"
  ) {
    targets.push(...issueWriteTargets(event, key, project));
    return targets;
  }
  if (
    event.type === "issue.claimed" ||
    event.type === "issue.released" ||
    event.type === "issue.deployed"
  ) {
    // A claim changes who holds the issue, and its holder is a collaborator from then on: the
    // detail, the log, the project lists that render the claim, and the collaborators and the
    // steps the branches above named. No rollup, ask count or component attachment moves with it.
    // A deploy is a line in the issue's log and nothing the lists or rollups read.
    return targets;
  }
  if (event.type === "artifact.human_edited") {
    // The notice a person's settled edits send to the document's agents, after each edit's own
    // `artifact.version` refreshed what it changed: a line in the issue's log and nothing more.
    return targets;
  }
  if (event.type === "artifact.created" || event.type === "artifact.version") {
    targets.push(
      { queryKey: ["artifacts", key] },
      { queryKey: ["comments", key] },
      // A reference to one of the issue's documents shows its name, kind and latest version.
      issueSummaryOf(key, event),
      // A version of the spec moves which requirements it marks and what serves them, and a
      // version of the Verification document moves which checks there are: the two reads hang
      // under the issue's key but are named here, since every issue target is `exact`.
      { exact: true, queryKey: ["issue", key, "coverage"] },
      { exact: true, queryKey: ["issue", key, "results"] }
    );
    if (event.type === "artifact.version") {
      targets.push({ queryKey: ["artifact", event.payload.artifact_id] });
      // The Decisions view reads two of the issue's documents - its decisions.md for the
      // decisions and its spec for each link's target - and the event names its document by
      // name alone, which does not say which the spec is, so every version of the issue
      // refreshes it. The query exists only while that tab is open.
      targets.push({ exact: true, queryKey: ["decisions", key] });
    } else {
      // An uploaded document is an `attached_to` edge into its issue.
      targets.push(referencePanel(issueReference));
      // An issue's decisions.md sends this event alone on its first upload, no version, and the
      // outline and the margin beside the spec read the decisions under events alone
      // (`EVENT_FRESH`), so without this they draw none of them until a reload. The server names
      // that document by its role, which it writes as it creates the row; a server built before
      // roles sends none, and names it by its slug alone.
      const { role, slug } = event.payload.artifact;
      if (role === "decisions" || (role === undefined && slug === DECISIONS_DOCUMENT_SLUG)) {
        targets.push({ exact: true, queryKey: ["decisions", key] });
      }
    }
    return targets;
  }
  if (event.type === "artifact.renamed" || event.type === "artifact.removed") {
    // A correction moves the title the issue's document list, the document's own read and every
    // reference to it show; a removal takes the row out of them. Both name the document itself,
    // and both move the `attached_to` edge the issue's backlink panel lists. Neither touches a
    // version, so the coverage and the results stand.
    targets.push(
      { queryKey: ["artifacts", key] },
      { queryKey: ["artifact", event.payload.artifact.id] },
      issueSummaryOf(key, event),
      referencePanel(issueReference)
    );
    return targets;
  }
  if (event.type === "artifact.approved" || event.type === "artifact.changes_requested") {
    targets.push(
      { queryKey: ["artifacts", key] },
      { queryKey: ["artifact", event.payload.artifact_id] },
      { queryKey: ["asks", key] },
      { queryKey: inboxKey }
    );
    if (event.payload.ask_id !== null) {
      targets.push(
        { queryKey: ["ask", event.payload.ask_id] },
        { queryKey: ["ask-thread", event.payload.ask_id] }
      );
    }
    return targets;
  }
  // A result is recorded against a check of this issue's Verification document, by the issue the
  // payload names, which is this one only when the result was recorded here. Both issues' results
  // reads move: the check's issue lists the new result under its check, and the recording issue
  // lists the results it recorded. Nothing else reads a result — no document, ask, inbox or
  // issue detail carries one — and this switch has a default, so the branch is deliberate and
  // its test pins it.
  //
  // A retraction moves the same two reads: the result stays in both listings and stops counting
  // as evidence there, so a page that did not refetch would keep showing it as proof.
  if (event.type === "result.recorded" || event.type === "result.retracted") {
    targets.push({ exact: true, queryKey: ["issue", key, "results"] });
    if (event.payload.issue !== key) {
      targets.push({ exact: true, queryKey: ["issue", event.payload.issue, "results"] });
    }
    return targets;
  }
  // A pull request that moves to another issue changes what this issue's results are compared
  // with, and the event is on each of the two issues, so each refetches its own results read.
  if (event.type === "issue.pull_request_attribution_changed") {
    targets.push({ exact: true, queryKey: ["issue", key, "results"] });
    return targets;
  }
  if (isAskEvent(event)) {
    targets.push({ queryKey: inboxKey });
    appendAskReads(targets, event);
    if (event.type === "ask.follower_added" || event.type === "ask.follower_removed") {
      return targets;
    }
    targets.push({ queryKey: ["asks", key] });
    // The view lists the spec's open questions beside the decisions, and a decision's turn is
    // its ask's.
    targets.push({ exact: true, queryKey: ["decisions", key] });
    if (movesOpenAsks(event)) {
      targets.push({ queryKey: projectsKey }, pinnedRow(key));
    }
    const document = eventDocument(event);
    if (document !== undefined) {
      targets.push({ queryKey: ["artifact", document] });
    }
    appendAnswersPage(targets, event);
    appendDocumentPanel(targets, event);
    return targets;
  }
  if (event.type === "block.repaired" || event.type === "block.invalid") {
    targets.push(
      { queryKey: ["asks", key] },
      { queryKey: projectsKey },
      { queryKey: inboxKey },
      pinnedRow(key)
    );
    return targets;
  }
  if (isCommentLikeEvent(event)) {
    targets.push({ queryKey: ["comments", key] });
    // A reply on an ask moves its `waiting_on`; the issue's ask list carries it, and a decision
    // block reads its turn from that list rather than from the Inbox. The Conversation's card
    // for that ask carries the reply itself.
    if (payloadString(event, "ask_id") !== undefined) {
      targets.push({ queryKey: ["asks", key] }, { queryKey: inboxKey });
    }
    const document = eventDocument(event);
    if (document !== undefined) {
      targets.push({ queryKey: ["artifact", document] });
    }
    appendCommentReads(targets, event);
    appendAnswersPage(targets, event);
    appendDocumentPanel(targets, event);
    return targets;
  }
  if (isMessageEvent(event)) {
    const target = payloadString(event, "target");
    appendAgentConversation(targets, target);
    // The Agents page groups a conversation under its thread root, so a reply belongs to that
    // root's conversation and not to a target of its own: a session answering a message aimed
    // at itself names no target at all, and the event carries the root's as `thread_target`.
    const threadTarget = payloadString(event, "thread_target");
    if (threadTarget !== target) {
      appendAgentConversation(targets, threadTarget);
    }
    // Every message read of the issue (a reference to a message, which shows the thread under
    // its root): a reply changes its root's read, and a payload need not name the root.
    targets.push({ queryKey: ["issue-messages", key] }, { queryKey: ["issue-message", key] });
    // An edit and a delete rewrite the message's blocks, so every read of one of them, as a
    // comment's own edit does (`itemBlockReads`).
    if (event.type === "message.edited" || event.type === "message.deleted") {
      const id = payloadString(event, "id");
      if (id !== undefined) {
        targets.push(itemBlockReads("message", id));
      }
    }
    appendRepliedBlock(targets, event);
    return targets;
  }
  if (event.type === "handoff.recorded") {
    // A handoff write lands in the issue's handoff list; its log line is the `["events", key]`
    // target above, and the step it ended the steps target above.
    targets.push({ queryKey: ["handoffs", key] });
    return targets;
  }
  if (event.type === "subscription.removed") {
    targets.push({ queryKey: ["subscribers", key] });
    return targets;
  }
  if (event.type === "child.added" || event.type === "child.removed") {
    // The child's `child_of` edge into this issue is a row of its backlink panel. Its detail
    // (the Children tab, the rollups), above, is all a child's status move changes here: the
    // child's own event names the ancestors further up.
    targets.push(referencePanel(issueReference));
    return targets;
  }
  if (event.type === "child.status") {
    return targets;
  }
  // The branches above name every type an issue owns, so only a type added later reaches here,
  // and it refreshes what any issue write could have moved.
  targets.push({ queryKey: ["issue"] }, { queryKey: projectsKey }, { queryKey: inboxKey });
  return targets;
}

/** What the server says one write moved in the reference graph, read the same way on every
 *  event; an event recorded before the field existed carries nothing. */
function changedReferences(event: DispatchEvent): readonly ChangedReference[] {
  const changed = payloadValue(event, "references_changed");
  return Array.isArray(changed) ? (changed as ChangedReference[]) : [];
}

/** Every read that holds a count or a panel: what an event that says it was cut without saying
 *  which kinds refreshes, as a server written before `references_changed_truncated_kinds` says. */
const EVERY_REFERENCE_READ: readonly RefreshTarget[] = [
  { queryKey: ["issue"] },
  { queryKey: ["asks"] },
  { queryKey: ["ask-thread"] },
  { queryKey: ["artifact"] },
  { queryKey: inboxKey },
  { queryKey: ["references", "to"] },
];

/** The reads that carry the backlinks of every target of one kind, which an event that cut that
 *  kind from `references_changed` refreshes in place of the targets it no longer names: every
 *  issue's detail and references tab and every issue panel; every ask's read and thread, every
 *  ask list (an issue's, and a document's, the `["artifact", id, "asks"]` read whose rows are
 *  asks), the Inbox and every ask panel; every document panel; every block's graph read and every
 *  document panel, which lists the edges at its blocks. */
const TRUNCATED_KIND_READS: Readonly<Partial<Record<string, readonly RefreshTarget[]>>> = {
  artifact: [referencePanels(({ node }) => node.kind === "artifact")],
  ask: [
    { queryKey: ["ask"] },
    { queryKey: ["ask-thread"] },
    { queryKey: ["asks"] },
    {
      queryKey: ["artifact"],
      when: (data) => rowsOf<{ question?: unknown }>(data).some((row) => "question" in row),
    },
    { queryKey: inboxKey },
    referencePanels(({ node }) => node.kind === "ask"),
  ],
  block: [referencePanels(({ node }) => node.kind === "block" || node.kind === "artifact")],
  issue: [{ queryKey: ["issue"] }, referencePanels(({ node }) => node.kind === "issue")],
};

/**
 * The reads that carry a changed target's backlinks, which `refs.CountedChanges` names in the
 * transaction that appends the event: an issue's detail (its count), its references tab and its
 * panel; an ask's read, its thread, the list its row lives in (its issue's asks, or its
 * document's), the Inbox row showing its count, and its panel; a document's panel; a block's
 * graph read, which its panel and every link to it share (pinned or not), and its document's
 * panel, which lists the edges at its blocks. Past the server's cap of one kind the event names
 * none of that kind's targets and lists the kind in `references_changed_truncated_kinds`, so every
 * read of that kind refreshes beside the targets of the other kinds; an event that is truncated
 * without the list (an older server), or lists a kind this build does not know, refreshes every
 * read that holds a count or a panel.
 */
function changedReferenceTargets(event: DispatchEvent): RefreshTarget[] {
  const truncatedKinds = payloadStrings(event, "references_changed_truncated_kinds");
  if (
    (truncatedKinds === undefined &&
      payloadValue(event, "references_changed_truncated") === true) ||
    truncatedKinds?.some((kind) => TRUNCATED_KIND_READS[kind] === undefined) === true
  ) {
    return [...EVERY_REFERENCE_READ];
  }
  const targets: RefreshTarget[] = (truncatedKinds ?? []).flatMap(
    (kind) => TRUNCATED_KIND_READS[kind] ?? []
  );
  for (const target of changedReferences(event)) {
    if (target.kind === "issue") {
      targets.push(
        { exact: true, queryKey: ["issue", target.id] },
        { exact: true, queryKey: ["issue", target.id, "references"] },
        referencePanel(buildDispatchReference({ key: target.id, kind: "issue" }))
      );
      continue;
    }
    if (target.kind === "artifact") {
      // A cited document's panel: the one opened on the address the server names (`ref`, as
      // `ReferencedBy` keys it), and one opened on another spelling of the same document, or any
      // when the entry names no address, found by the node its read reports.
      if (target.ref !== undefined) {
        targets.push(referencePanel(target.ref));
      }
      const id = target.id;
      targets.push(referencePanels(({ node }) => node.kind === "artifact" && node.id === id));
      continue;
    }
    if (target.kind === "block") {
      // The read opened on the address the server names, and every read of the same block node
      // (a pinned reference's, or another spelling), found by the node its read reports.
      if (target.ref !== undefined) {
        targets.push(referencePanel(target.ref));
      }
      const id = target.id;
      targets.push(referencePanels(({ node }) => node.kind === "block" && node.id === id));
      const document = target.artifact_id;
      if (document !== undefined && document !== null) {
        targets.push(
          referencePanels(({ node }) => node.kind === "artifact" && node.id === document)
        );
      }
      continue;
    }
    const id = target.id;
    // An answered card reads its ask from the thread, which is fetched once (staleTime
    // Infinity): without it a citation added after the answer never moves its control.
    targets.push({ queryKey: ["ask", id] }, { queryKey: ["ask-thread", id] }, inboxRowOfAsk(id));
    if (target.issue_key !== undefined && target.issue_key !== null) {
      targets.push(
        { queryKey: ["asks", target.issue_key] },
        referencePanel(buildDispatchReference({ id, key: target.issue_key, kind: "ask" }))
      );
      continue;
    }
    // An ask on a project document is referenced under the document's slug, which the event
    // does not carry: its panel is found by the node it reads.
    if (target.artifact_id !== undefined && target.artifact_id !== null) {
      targets.push({ queryKey: ["artifact", target.artifact_id, "asks"] });
    }
    targets.push(referencePanels(({ node }) => node.kind === "ask" && node.id === id));
  }
  return targets;
}

/**
 * The issue whose every cached ask thread an event moves, or null. Each open ask card lists who a
 * person's answer reaches through the ask's issue, read from its thread (`["ask-thread", id]`):
 * the session holding the issue's claim and the session its route reaches. A claim or a release
 * moves the claimant. A close that releases a claim appends its own `issue.released`, so an update
 * or a close moves the threads only when it changed the route (one PATCH that sets the route and
 * the status `done` emits `issue.closed` alone). Without `changed` (an older server) every update
 * and close may have. A deploy moves neither.
 */
export function askThreadsIssue(event: DispatchEvent): string | null {
  switch (event.type) {
    case "issue.claimed":
    case "issue.released":
      return event.issue_key;
    case "issue.updated":
    case "issue.closed": {
      const changes = issueChanges(event);
      return changes === undefined || changes.has("route") ? event.issue_key : null;
    }
    default:
      return null;
  }
}

/**
 * Every cached ask thread whose ask is on `issue`. The thread key holds only the ask id, so the
 * test reads the thread: one still on its first load is judged on the response that lands
 * (`refreshQueries`), and a thread that holds nothing to read (an error) refreshes too.
 */
function askThreadsOn(issue: string): RefreshTarget {
  return {
    queryKey: ["ask-thread"],
    when: (data) => {
      const thread = data as AskRead | undefined;
      return thread === undefined || thread.ask.issue_key === issue;
    },
  };
}

/** The kind of post each correction is of: the segment an address names it under
 *  (`/comment/<id>`, `/message/<id>`).
 *
 *  The search palette keeps each answer under the words it was asked for and the request it
 *  sent, never under the posts it holds, for its `staleTime` (`useTieredSearch.ts`, `["search",
 *  words, request]`), and an answer holds a post's text or a block of it with the post's marks.
 *  So an edit or a delete of a post reads every kept answer again: the post may be in any of
 *  them, and its words may be gone. A create needs none: a kept answer that misses a post written
 *  since shows no text of it. The names reads (`referenceTargetsQuery`) hold a post's words and
 *  marks too, but under the addresses they name, so a correction reads again only those whose
 *  key names an address of the issue or the project document the post is on, or names the post
 *  by its id under any other owner (`referenceNameTargets`). */
const POST_CORRECTIONS: Partial<Record<EventType, "comment" | "message">> = {
  "comment.edited": "comment",
  "comment.deleted": "comment",
  "message.edited": "message",
  "message.deleted": "message",
};

const SEARCH_ANSWERS: RefreshTarget = { queryKey: ["search"] };

/**
 * Whether `address` is `owner` or an address under it: `owner`, then the end of the address or
 * one of `next`, the characters that may stand after an owner of that kind in an address
 * (`buildDispatchReference`), so that `dispatch://CORE-1` is never read as the start of
 * `dispatch://CORE-12`.
 */
function addressUnder(address: string, owner: string, next: string): boolean {
  return (
    address.startsWith(owner) &&
    (address.length === owner.length || next.includes(address.charAt(owner.length)))
  );
}

/** The slug of the project document `event` is on, where its payload carries it: a created,
 *  renamed or removed document's own, an edited or a deleted comment's document's
 *  (`artifact_slug`, which the read of a deleted comment's event keeps beside its delete marks),
 *  and an edited ask's document's, the one its anchor quotes (`anchor_artifact`), for an edit
 *  through the ask's own route the one that holds its typed block (`block_artifact`), or for an
 *  approval request's rewording the one it asks approval of (`approval_artifact`, which the
 *  server's read of the event log attaches to every ask event carrying `approval`), each the
 *  document the ask is on. Four events carry none, and name the document by its id alone, which
 *  no address is written with: a new version, a comment's edit written before `artifact_slug`
 *  existed, the edit of a question asked of the document as a whole, with no anchor, no block
 *  and no approval, and a document version's rewording of a block ask, whose event the
 *  document's settlement writes without `block_artifact`. */
function projectDocumentSlug(event: DispatchEvent): string | undefined {
  switch (event.type) {
    case "artifact.created":
    case "artifact.removed":
    case "artifact.renamed":
      return event.payload.artifact.slug;
    case "ask.edited":
      return (
        event.payload.anchor_artifact?.slug ??
        event.payload.block_artifact?.slug ??
        event.payload.approval_artifact?.slug
      );
    case "comment.edited":
    case "comment.deleted":
      return event.payload.artifact_slug;
    default:
      return undefined;
  }
}

/**
 * Which addresses `event` can change the words or the name under, or undefined for an event on
 * neither an issue nor a project document. An issue's event is on the issue or on something it
 * owns - a comment, an ask, a message, a document and its blocks - each written under
 * `dispatch://<KEY>`, with `/` or `#` next. The issue's own address, `dispatch://<KEY>`, is
 * named by the issue's title and by nothing else, since the names read names an issue by its
 * title alone and every other target by its own words, a document by its name and a block by its
 * text (the server's `refs.TargetNames`). So a write that changes the title (`issue.updated`) is
 * on that one address, and every other event is on the addresses under it and never on that one,
 * since none of them changes the title: a child's page cites its parent by its bare address, and
 * a new version of the parent's spec leaves the name of that address as it was. A project
 * document's event is on the document or on an ask, a comment or a block of it, each written
 * under `dispatch://<PROJECT>/artifact/<slug>`, with `@`, `/` or `#` next. An event whose payload
 * carries that slug (`projectDocumentSlug`) is on those addresses, and one that names the
 * document by its id alone, as `projectDocumentSlug` lists, is on the addresses of every document
 * of the project.
 */
function eventAddresses(event: DispatchEvent): ((address: string) => boolean) | undefined {
  if (event.issue_key !== null) {
    const issue = buildDispatchReference({ key: event.issue_key, kind: "issue" });
    if (event.type === "issue.updated") {
      return (address) => address === issue;
    }
    return (address) => address !== issue && addressUnder(address, issue, "/#");
  }
  const project = event.project;
  if (event.artifact_id === null || event.artifact_id === undefined || project === undefined) {
    return undefined;
  }
  const slug = projectDocumentSlug(event);
  if (slug === undefined) {
    const documents = buildDispatchReference({ kind: "document", project, slug: "" });
    return (address) => address.startsWith(documents);
  }
  const document = buildDispatchReference({ kind: "document", project, slug });
  return (address) => addressUnder(address, document, "@/#");
}

/** A post's id as Dispatch stores it and every read of it writes it: a uuid, canonical and in
 *  lower case. */
const POST_ID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;

/**
 * Whether an address names the post a correction (`POST_CORRECTIONS`) is of, the post itself or a
 * block of it, as GET /api/v1/reference-targets reads the address; undefined for every other
 * event, and for a correction whose payload names no post by an id Dispatch stores (`id`,
 * `POST_ID`), which no address can name. The read names a comment and a message by the id the
 * address writes, whatever owner it is written under: in the issue's form
 * (`dispatch://<KEY>/comment/<id>`, `dispatch://<KEY>/message/<id>`), in a project document's
 * (`dispatch://<PROJECT>/artifact/<slug>/comment/<id>`, with or without a version after the
 * slug), and as a block's (`…#b-<block id>`, pinned or not). That id is the post's whole id as it
 * is stored: the server looks the post up by that text, so an id spelled any other way, in upper
 * case or without its hyphens, names nothing. An id written short - a hex digit, then seven or
 * more hex digits or hyphens, read without case, and no whole id (`isIdPrefix`) - names the post
 * only in an address under what the post is on (`eventAddresses`): the server resolves a short id
 * among the items of the owner the address names and no other, so the same prefix under another
 * owner names one of that owner's items or nothing. An address that writes no post's id - an
 * issue, a document, a block of a document - names no post. A dashboard URL, which the server
 * reads too, is not read here: the issue page names every read by its `dispatch://` address.
 */
export function correctedPostAddresses(
  event: DispatchEvent
): ((address: string) => boolean) | undefined {
  const kind = POST_CORRECTIONS[event.type];
  const id = payloadString(event, "id");
  if (kind === undefined || id === undefined || !POST_ID.test(id)) {
    return undefined;
  }
  const owned = eventAddresses(event);
  return (address) => {
    const route = parseNodeReference(address);
    const item = route === undefined ? undefined : routeItem(route);
    if (item?.kind !== kind) {
      return false;
    }
    return (
      item.id === id ||
      (isIdPrefix(item.id) && id.startsWith(item.id.toLowerCase()) && owned?.(address) === true)
    );
  };
}

/**
 * Every names read whose key (`["reference-targets", <sorted addresses>]`) holds an address
 * `event` can change the words under (`eventAddresses`), one that names the post a correction is
 * of under any owner (`correctedPostAddresses`), or one written in a form other than
 * `dispatch://` and its owner, which could name anything and so is refreshed for rather than
 * missed. An event on neither an issue nor a project document refreshes every names read.
 */
function namesReadsNaming(event: DispatchEvent): RefreshTarget {
  const touched = eventAddresses(event);
  if (touched === undefined) {
    return { queryKey: ["reference-targets"] };
  }
  const post = correctedPostAddresses(event);
  return {
    queryKey: ["reference-targets"],
    when: (_data, query) =>
      rowsOf<unknown>(query.queryKey[1]).some(
        (address) =>
          typeof address === "string" &&
          (!address.startsWith("dispatch://") || touched(address) || post?.(address) === true)
      ),
  };
}

/**
 * Which names reads (`referenceTargetsQuery`, `["reference-targets", <sorted addresses>]`) an
 * event refreshes: those whose key holds an address it changes the words under, on an edit or a
 * delete of a comment or an issue message (its body, which a delete empties, and the marks the
 * read answers for the post, `edited_at`, `deleted_at` and `deleted_by`), an ask's edit (its
 * question) and a document's new version (its blocks' words), and none on any other event. The
 * issue page's names reads are kept fresh by events (`reference-target.ts`'s `EVENT_FRESH`),
 * never by their age, and a names read's key is the set of addresses it names, so a page that
 * comes to hold another set reads again under a key of its own: only a change of words under the
 * same address needs a rule.
 * An issue's write that changes its title (`issue.updated` naming `title` among its changes, or
 * naming none, as an older server sends it) renames the issue's own address, since a names read
 * names an issue by its title, so it refreshes the reads naming that address, and only those
 * (`eventAddresses`).
 * A document's creation (`artifact.created`) gives words to an address a page cited before the
 * document existed, its rename (`artifact.renamed`) moves the name the read answers for an
 * address to the document, and its removal (`artifact.removed`) makes every address to it and to
 * its blocks name nothing (`gone`), so each refreshes them too.
 *
 * Of the names reads the app holds, each event refreshes those whose key holds an address it can
 * change: one of the issue or the project document it is on (`eventAddresses`), and, for a
 * post's correction, one that names the post by its id under any owner
 * (`correctedPostAddresses`). The test reads the query's key, not the page's issue, since a
 * page's words can cite another issue's target. Every address the issue page names a read by is
 * written `dispatch://` and its owner (`treeReferences`): a node's own, a chip's, each one its
 * words cite, a bare key asked as its issue's own address, and an item id or a block id written
 * short still under its owner. So the key alone ties each address to what owns it, but for a
 * comment's or a message's whole id: the server names a post by that id whatever owner the
 * address is written under, so a page on CORE-2 whose words cite a CORE-1 comment as
 * `dispatch://CORE-2/comment/<id>` holds that comment's words under a CORE-2 address. A comment
 * or ask event on neither an issue nor a project document never reaches this rule:
 * `documentTargets` refuses it first, and the feed then refreshes every read the app holds. A
 * message correction always carries its issue, since the server refuses to correct any other
 * message (`message_corrections.go`).
 *
 * A person's edit of a document (`artifact.human_edited`) is none of them: it is the notice sent
 * once the document has been quiet after those edits, each of which wrote a version that sent
 * its own `artifact.version` (the notice's branch of `ownerTargets`), so it changes no text. A
 * post's correction is on what owns the post: a comment's on its issue, or on the project
 * document it belongs to, whose slug its payload carries (`projectDocumentSlug`), and a
 * message's on its issue, since the server corrects an issue message and refuses any other. So
 * each reaches every names read whose key names anything of that owner, the post and its blocks
 * among them, and every one whose key names the post or a block of it by its whole id under
 * another owner, and no other: an id written short under another owner names one of that
 * owner's items or nothing.
 * The search palette's answers, which a post's words are kept under too, are keyed by no
 * address, so a correction reads every one of them again (`POST_CORRECTIONS`).
 *
 * The feed is the workspace's (`api/event-feed.ts`): these events arrive for every issue and
 * project document of the workspace, a document's new version above all, and one that can
 * change no address a names read the app holds names costs nothing. One that can change an
 * address a read's key holds - an event of the open issue itself, whose tree holds its
 * addresses, or of an issue or a project document its words cite - costs one read per such
 * query the app holds and renders (one request per batch of its addresses, which no ordinary
 * issue passes), at most two for an open issue page - the tree's own and the one naming what a
 * typed block's words cite (`useIssueTree`) - and marks stale one the app holds and renders not.
 * A project document's event that names the document by its id alone (`projectDocumentSlug`)
 * reaches a read naming any document of its project, since it does not carry the slug its
 * addresses are written with (`eventAddresses`). It refreshes through the same `refreshQueries`
 * every other rule does, so a read already out, or begun under a second ago, is fetched again
 * once those rules allow; since the rule tests the read's key against the answer that lands, a
 * first read out at the event that then fails holds no answer to test and is not read again by
 * it, and window focus or a later event reads it again (`refetchFailedOnFocus`). A refetch that
 * fails keeps the answer it would have replaced, so a post's correction writes its own marks
 * into each answer the cache holds as it arrives (`markCorrectedPost`): a read that then fails
 * names a deleted post by the deleted line and an edited one with its mark. An answer that a
 * read already out at an event lands was composed before it, and a refetch of it that fails
 * keeps it, which stands for every refresh but a correction's own facts: the feed puts each
 * answer it marks over every read of that query still out, and reads the query again
 * (`replaceQueryData`), and keeps a correction while a read out at its arrival of a query it
 * marked nothing in is out, writing its marks into the answer that read lands (`keepCorrection`
 * in `corrected-post.ts`).
 */
function referenceNameTargets(event: DispatchEvent): RefreshTarget[] {
  switch (event.type) {
    case "artifact.created":
    case "artifact.removed":
    case "artifact.renamed":
    case "artifact.version":
    case "ask.edited":
    case "comment.edited":
    case "comment.deleted":
    case "message.edited":
    case "message.deleted":
      return [namesReadsNaming(event)];
    case "issue.updated": {
      const changes = issueChanges(event);
      return changes === undefined || changes.has("title") ? [namesReadsNaming(event)] : [];
    }
    default:
      return [];
  }
}

/**
 * The reads one event of the feed can change, each a query key (a prefix unless `exact`) and, where
 * only some of a family's reads can have moved, a test of the read's data. `signedInLogin` is the
 * viewer's: the per-viewer events and the Replies rule read it.
 */
export function eventRefreshTargets(event: DispatchEvent, signedInLogin?: string): RefreshTarget[] {
  const targets = [...ownerTargets(event, signedInLogin), ...changedReferenceTargets(event)];
  targets.push(...referenceNameTargets(event));
  const threadsIssue = askThreadsIssue(event);
  if (threadsIssue !== null) {
    targets.push(askThreadsOn(threadsIssue));
  }
  if (POST_CORRECTIONS[event.type] !== undefined) {
    targets.push(SEARCH_ANSWERS);
  }
  return targets;
}
