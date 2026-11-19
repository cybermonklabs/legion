import type {
  Actor,
  Artifact,
  Ask,
  Comment,
  CommentWithTrees,
  DispatchEvent,
  DisplayTree,
  MessageRead,
} from "../../api/types";
import { compareTimestamps } from "../../lib/timestamps";
import { activityDescription, dateKey, dayLabel } from "../activity/activity-model";
import type { AgentPart } from "../refs/AgentName";
import { askResolutionReason } from "../refs/actor";
import { sessionOfAddress } from "../refs/routes";
import { agentsOnlyIds } from "./agents-only";
import { CONTROL_LABELS, issueStatusWord } from "./page-words";
import { plainWords } from "./tree-drafts";

/**
 * The issue's history, as the panel draws it (dispatch://INFRA-1196 R15, R20): every event that
 * is no row of the outline, newest first, by day.
 *
 * It holds the issue's status moves, its claims and releases, its handoffs, its documents added
 * and removed, every other event of the issue itself, and the comments and messages agents wrote
 * outside every thread a person wrote in, each thread with its text, its replies and their reply
 * controls (`agents-only.ts`). It leaves out each event of a row the outline draws - an ask
 * asked, answered, resolved, edited or handed on, a comment or a message the outline draws and
 * what happened to it, a result recorded against a check, a decision kept, a child's status move
 * - and a new version of a document, which is a version and no upload: a document's versions are
 * in its own toolbar. The upload of any other file or picture, its first version or a later one,
 * is an upload, and one author's uploads within one minute are one entry that opens each.
 *
 * Pure, so the panel and its unit tests read one model.
 */
export interface HistoryDay {
  readonly date: string;
  /** What the day is headed, in the page's words: Today, Yesterday, or the date written out. */
  readonly label: string;
  readonly today: boolean;
  /** The day's entries, newest first, with the reader's read mark among them where it falls. */
  readonly entries: readonly HistoryEntry[];
}

/** One turn of a thread agents wrote among themselves. */
export interface HistoryTurn {
  readonly at: string;
  readonly author: Actor;
  readonly body: string;
  readonly bodyTree?: DisplayTree;
  readonly id: string;
  readonly kind: "comment" | "message";
}

export type HistoryEntry =
  | {
      readonly kind: "event";
      readonly id: string;
      readonly at: string;
      readonly author: Actor;
      /** `main`'s line of the event, in the page's words where it names a delivery's agent or
       *  a status (`historyLine`). */
      readonly description: AgentPart[];
      readonly event: DispatchEvent;
      readonly seq: number;
    }
  | {
      readonly kind: "uploads";
      readonly id: string;
      readonly at: string;
      readonly author: Actor;
      /** The files the run uploaded, oldest first, each by its own address. */
      readonly files: readonly UploadedFile[];
      readonly seq: number;
    }
  | {
      readonly kind: "thread";
      readonly id: string;
      readonly at: string;
      readonly author: Actor;
      readonly root: HistoryTurn;
      /** The thread's replies, oldest first. */
      readonly replies: readonly HistoryTurn[];
      readonly seq: number;
    }
  | { readonly kind: "unread"; readonly id: "unread" };

/** One file an upload entry opens: the upload's event, the file's name and its address. */
export interface UploadedFile {
  readonly id: number;
  readonly name: string;
  readonly slug: string | undefined;
}

export interface HistoryInput {
  readonly artifacts: readonly Artifact[];
  /** The issue's asks, which tell a reply under a question, the outline's, from one under an
   *  approval request (`agents-only.ts`): the outline's own input, the issue's open asks and the
   *  asks read (`tree-drafts.ts`). */
  readonly asks: readonly Pick<Ask, "id" | "kind">[];
  readonly comments: readonly (Comment & Pick<CommentWithTrees, "body_tree">)[] | undefined;
  readonly events: readonly DispatchEvent[];
  /** `user_issue_state.last_read_seq`: the reader's read mark. */
  readonly lastReadSeq: number;
  /** When the oldest event the log has read so far was made, while older pages of the log are
   *  still to be read; undefined once the whole log is in. A thread that began before it is
   *  left out until its page is read: its place among events not read yet is not known, and a
   *  day drawn with that thread alone would count one entry where it holds many. */
  readonly loadedFrom?: string;
  readonly messages: readonly MessageRead[] | undefined;
  /** Local YYYY-MM-DD. */
  readonly today: string;
}

/** How far apart one author's uploads may be and still be one entry. */
const UPLOAD_RUN_MS = 60_000;

/** The event types of a row the outline draws, which the history leaves out. A comment's and a
 *  message's are left out where the outline draws that comment or message - one a person took
 *  part in - and for its creation always, since this list draws a thread agents wrote among
 *  themselves from the issue's comments and messages, each turn whole; what happened to such a
 *  thread after - a mention that could not be delivered, a resolution, an edit - is no node's,
 *  so it is a line here (R15, `ofOutlineRow`). An ask's are left out for a question alone: an
 *  approval request is no row of the outline - the identity block draws its step
 *  (`ApprovalSteps.tsx`) - so its asking, hand-back and answer are the history's. */
const OUTLINE_EVENTS: Readonly<Record<string, true>> = {
  "ask.anchor_refreshed": true,
  "ask.answered": true,
  "ask.asked_of": true,
  "ask.edited": true,
  "ask.follower_added": true,
  "ask.follower_removed": true,
  "ask.handed_back": true,
  "ask.opened": true,
  "ask.resolved": true,
  "child.added": true,
  "child.status": true,
  "comment.anchor_refreshed": true,
  "comment.answered": true,
  "comment.created": true,
  "comment.delivery": true,
  "comment.edited": true,
  "comment.reopened": true,
  "comment.resolved": true,
  "decision.kept": true,
  "message.accepted": true,
  "message.answered": true,
  "message.created": true,
  "message.delivery": true,
  "result.recorded": true,
  "result.retracted": true,
  "suggestion.accepted": true,
  "suggestion.rejected": true,
};

type Dated = Exclude<HistoryEntry, { kind: "unread" }>;

/** The issue's history by day, newest first (`HistoryDay`). */
export function buildHistory(input: HistoryInput): HistoryDay[] {
  const artifacts = new Map(input.artifacts.map((artifact) => [artifact.id, artifact]));
  const agentsOnly = agentsOnlyIds(input.comments, input.messages, input.asks);
  const entries: Dated[] = [];
  let previousStatus: string | undefined;
  // The upload entry one author's uploads are gathering in, and its place among `entries`.
  let run:
    | { author: Actor; at: string; entry: Extract<Dated, { kind: "uploads" }>; index: number }
    | undefined;
  const ordered = [...input.events].sort((left, right) => left.seq - right.seq);
  const seqOf = new Map<string, number>();
  for (const event of ordered) {
    if (event.type === "comment.created" || event.type === "message.created") {
      seqOf.set(event.payload.id, event.seq);
    }
    const status =
      event.type === "issue.created" ||
      event.type === "issue.updated" ||
      event.type === "issue.closed"
        ? event.payload.status
        : undefined;
    const upload = uploadOf(event, artifacts);
    if (ofOutlineRow(event, agentsOnly) || isDocumentVersion(event, artifacts)) {
      if (status !== undefined) previousStatus = status;
      continue;
    }
    if (upload !== undefined) {
      const near =
        run !== undefined &&
        run.author.kind === event.actor.kind &&
        run.author.id === event.actor.id &&
        Date.parse(event.created_at) - Date.parse(run.at) <= UPLOAD_RUN_MS;
      if (near && run !== undefined) {
        const files = [...run.entry.files, upload];
        const grown = { ...run.entry, at: event.created_at, files, seq: event.seq };
        entries[run.index] = grown;
        run = { ...run, entry: grown };
      } else {
        const entry = {
          at: event.created_at,
          author: event.actor,
          files: [upload],
          id: `uploads:${event.id}`,
          kind: "uploads" as const,
          seq: event.seq,
        };
        run = { at: event.created_at, author: event.actor, entry, index: entries.length };
        entries.push(entry);
      }
      continue;
    }
    entries.push({
      at: event.created_at,
      author: event.actor,
      description: historyLine(event, previousStatus),
      event,
      id: `event:${event.id}`,
      kind: "event",
      seq: event.seq,
    });
    if (status !== undefined) previousStatus = status;
  }
  entries.push(...threadsOf(input, agentsOnly, seqOf));

  // Newest first, by when each happened; one instant by the log's own order.
  entries.sort((one, other) => compareTimestamps(other.at, one.at) || other.seq - one.seq);
  const read = entries.some((entry) => entry.seq > 0 && entry.seq <= input.lastReadSeq);
  const unread = entries.some((entry) => entry.seq > input.lastReadSeq);
  let marked = !(read && unread);
  const days: { date: string; entries: HistoryEntry[] }[] = [];
  for (const entry of entries) {
    // The line that says what is new stands after the oldest unread entry, in that entry's own
    // day: where the reader last read through an earlier day, it closes today's list, which is
    // open, rather than heading a day the list folds.
    if (!marked && entry.seq > 0 && entry.seq <= input.lastReadSeq) {
      days.at(-1)?.entries.push({ id: "unread", kind: "unread" });
      marked = true;
    }
    const date = dateKey(entry.at);
    let day = days.at(-1);
    if (day?.date !== date) {
      day = { date, entries: [] };
      days.push(day);
    }
    day.entries.push(entry);
  }
  // Today and yesterday are headed in the page's words (decision 14), an older day by its date
  // as the Activity tab writes it out. Yesterday is read back from today's noon, so no change of
  // the clocks moves it to another day.
  const noon = new Date(`${input.today}T12:00:00`);
  noon.setDate(noon.getDate() - 1);
  const yesterday = dateKey(noon.toISOString());
  return days.map((day) => ({
    date: day.date,
    entries: day.entries,
    label:
      day.date === input.today
        ? CONTROL_LABELS.history_today
        : day.date === yesterday
          ? CONTROL_LABELS.history_yesterday
          : dayLabel(day.date, input.today),
    today: day.date === input.today,
  }));
}

/** Each thread agents wrote among themselves, as one entry at its first turn: every one that
 *  began inside what the log has read (`HistoryInput.loadedFrom`). A comment under an ask names
 *  the ask (`ask_id`), where the server's climb to a thread's head stops, so the thread of an
 *  approval request is every comment naming it (`agents-only.ts`), whatever its `reply_to`
 *  says: one entry per ask, at its earliest comment, the rest its replies. */
function threadsOf(
  input: HistoryInput,
  ids: ReadonlySet<string>,
  seqOf: ReadonlyMap<string, number>
): Dated[] {
  const threads: Dated[] = [];
  const comments = (input.comments ?? []).filter((comment) => ids.has(comment.id));
  const byAsk = new Map<string, typeof comments>();
  for (const comment of comments) {
    if (comment.ask_id === null) continue;
    const thread = byAsk.get(comment.ask_id);
    if (thread === undefined) byAsk.set(comment.ask_id, [comment]);
    else thread.push(comment);
  }
  for (const thread of byAsk.values()) {
    const [root, ...replies] = thread.sort((one, other) =>
      compareTimestamps(one.created_at, other.created_at)
    );
    if (root === undefined) continue;
    threads.push(threadEntry(commentTurn(root), replies.map(commentTurn), seqOf));
  }
  const unasked = comments.filter((comment) => comment.ask_id === null);
  const repliesTo = new Map<string, typeof comments>();
  for (const comment of unasked) {
    if (comment.reply_to === null) continue;
    const replies = repliesTo.get(comment.reply_to);
    if (replies === undefined) repliesTo.set(comment.reply_to, [comment]);
    else replies.push(comment);
  }
  for (const root of unasked) {
    if (root.reply_to !== null) continue;
    const replies = (repliesTo.get(root.id) ?? []).sort((one, other) =>
      compareTimestamps(one.created_at, other.created_at)
    );
    threads.push(threadEntry(commentTurn(root), replies.map(commentTurn), seqOf));
  }
  for (const thread of input.messages ?? []) {
    if (!ids.has(thread.message.id)) continue;
    threads.push(
      threadEntry(messageTurn(thread.message), (thread.replies ?? []).map(messageTurn), seqOf)
    );
  }
  // A thread stands at its first turn's time (`threadEntry`), so one whose first turn is older
  // than every event read so far is left out whole, its replies too however recent, until the
  // page that reaches back to that turn is read: drawn before, it would stand among days the log
  // has not read, and a day drawn with it alone would count one entry where it holds many. The
  // address of one of its turns reads the log on until it is in (`IssueHistory.tsx`).
  const from = input.loadedFrom;
  return from === undefined
    ? threads
    : threads.filter((thread) => compareTimestamps(thread.at, from) >= 0);
}

function threadEntry(
  root: HistoryTurn,
  replies: readonly HistoryTurn[],
  seqOf: ReadonlyMap<string, number>
): Extract<Dated, { kind: "thread" }> {
  return {
    at: root.at,
    author: root.author,
    id: `thread:${root.id}`,
    kind: "thread",
    replies,
    root,
    seq: seqOf.get(root.id) ?? 0,
  };
}

/**
 * The line of `event`, as today's Activity tab writes it (`activityDescription`), with what that
 * line names by a token in the page's words (dispatch://INFRA-3948 D6, R22 of
 * dispatch://INFRA-1196): a delivery's target by the agent it went to, never its address or
 * `session:<id>`; the status a reopened issue went into by its word; and a stored text it
 * quotes, an approval request's question or the reason it was resolved with, by its plain words,
 * as the outline names an ask (`plainWords`, decision 95), never the markup its author typed or a
 * bare `dispatch://` address, which reads as its short form. Every other text a line of the
 * history holds is a name, a key or the server's own words, none of which is markdown.
 */
function historyLine(event: DispatchEvent, previousStatus: string | undefined): AgentPart[] {
  const line = activityDescription(event, previousStatus);
  switch (event.type) {
    case "comment.delivery":
      return withPart(
        line,
        event.payload.target,
        recipient(event.payload.session_id, undefined, event.payload.target)
      );
    // Only a queued message's line names where it went.
    case "message.delivery":
      return event.payload.state === "queued"
        ? withPart(
            line,
            event.payload.target ?? `session:${event.payload.session_id}`,
            recipient(event.payload.session_id, event.payload.title, event.payload.target)
          )
        : line;
    // The line names a status only for a reopen, which `activityDescription` tells by the
    // status before it.
    case "issue.updated":
      return previousStatus === "done" && event.payload.status !== "done"
        ? withPart(line, event.payload.status, issueStatusWord(event.payload.status))
        : line;
    // Every ask event the history keeps is an approval request's (`ofOutlineRow`), whose
    // question is markdown its agent wrote, citing addresses.
    case "ask.opened":
    case "ask.anchor_refreshed":
    case "ask.edited":
    case "ask.handed_back":
    case "ask.asked_of":
    case "ask.answered":
      return withWords(line, event.payload.question);
    case "ask.resolved":
      return withWords(line, askResolutionReason(event.payload.resolution));
    default:
      return line;
  }
}

/** `line` with each reading of `token` in its text, the last of each part, drawn as `part`. */
function withPart(line: AgentPart[], token: string | undefined, part: AgentPart): AgentPart[] {
  if (token === undefined || token === "") return line;
  return line.flatMap((piece) => {
    if (typeof piece !== "string") return [piece];
    const at = piece.lastIndexOf(token);
    return at < 0
      ? [piece]
      : [piece.slice(0, at), part, piece.slice(at + token.length)].filter((one) => one !== "");
  });
}

/** `line` with the stored text `stored` it quotes drawn by its plain words (`plainWords`). */
function withWords(line: AgentPart[], stored: string | undefined): AgentPart[] {
  if (stored === undefined) return line;
  const words = plainWords(stored);
  return words === stored ? line : withPart(line, stored, words);
}

/** Who a delivery went to, as the page names an agent (`AgentName`): the session the attempt
 *  reached, with the title the listener gave it, else the session its target names, else the
 *  page's word for an agent nothing names, where the target is a role no session held. The
 *  history's lines and the focused row's deliveries (`FocusedNodeFacts.tsx`) name it alike. */
export function recipient(
  sessionId: string | null,
  title: string | undefined,
  target: string | undefined
): AgentPart {
  const reached = sessionId === null || sessionId === "" ? undefined : sessionId;
  const session = reached ?? (target === undefined ? undefined : sessionOfAddress(target));
  if (session === undefined || session === "") return CONTROL_LABELS.unnamed_recipient;
  return {
    sessionId: session,
    storedTitle: reached === undefined || title === undefined || title === "" ? undefined : title,
  };
}

function commentTurn(comment: Comment & Pick<CommentWithTrees, "body_tree">): HistoryTurn {
  return {
    at: comment.created_at,
    author: comment.author,
    body: comment.body,
    ...(comment.body_tree === undefined ? {} : { bodyTree: comment.body_tree }),
    id: comment.id,
    kind: "comment",
  };
}

function messageTurn(message: MessageRead["message"]): HistoryTurn {
  return {
    at: message.created_at,
    author: message.author,
    body: message.body,
    ...(message.body_tree === undefined ? {} : { bodyTree: message.body_tree }),
    id: message.id,
    kind: "message",
  };
}

/** The comment or message an event is about, where it is one of a comment's or a message's
 *  that the history draws for a thread agents wrote among themselves: every one but the
 *  creation, which the thread's own turn draws. */
function turnOf(event: DispatchEvent): string | undefined {
  switch (event.type) {
    case "comment.delivery":
      return event.payload.comment_id;
    case "message.accepted":
    case "message.delivery":
      return event.payload.message_id;
    case "comment.answered":
    case "comment.edited":
    case "comment.reopened":
    case "comment.resolved":
    case "message.answered":
      return event.payload.id;
    default:
      return undefined;
  }
}

/** Whether `event` is of a row the outline draws, and so no line of the history. What happened
 *  to a comment or a message of a thread agents wrote among themselves (`agentsOnly`) is no
 *  row's, and an approval request's events are the history's. */
function ofOutlineRow(event: DispatchEvent, agentsOnly: ReadonlySet<string>): boolean {
  if (OUTLINE_EVENTS[event.type] !== true) return false;
  const turn = turnOf(event);
  if (turn !== undefined && agentsOnly.has(turn)) return false;
  const payload: unknown = event.payload;
  return !(
    event.type.startsWith("ask.") &&
    typeof payload === "object" &&
    payload !== null &&
    "kind" in payload &&
    payload.kind === "approval"
  );
}

/** Whether `event` writes a version of a document: a version, never an upload. */
function isDocumentVersion(
  event: DispatchEvent,
  artifacts: ReadonlyMap<string, Artifact>
): boolean {
  if (event.type === "artifact.human_edited") return true;
  return (
    event.type === "artifact.version" && artifacts.get(event.payload.artifact_id)?.kind === "doc"
  );
}

/** The file `event` uploaded, where it uploaded one that is no document. */
function uploadOf(
  event: DispatchEvent,
  artifacts: ReadonlyMap<string, Artifact>
): UploadedFile | undefined {
  if (event.type === "artifact.created") {
    const artifact = event.payload.artifact;
    return artifact.kind === "doc"
      ? undefined
      : { id: event.id, name: artifact.name, slug: artifact.slug };
  }
  if (event.type === "artifact.version") {
    const artifact = artifacts.get(event.payload.artifact_id);
    return artifact === undefined || artifact.kind === "doc"
      ? undefined
      : { id: event.id, name: event.payload.name, slug: artifact.slug };
  }
  return undefined;
}
