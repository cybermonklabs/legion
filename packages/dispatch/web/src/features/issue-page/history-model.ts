import type {
  Actor,
  Artifact,
  Ask,
  AskWithTrees,
  CheckBlock,
  Comment,
  CommentWithTrees,
  DispatchEvent,
  DisplayTree,
  MessageRead,
} from "../../api/types";
import { compareTimestamps } from "../../lib/timestamps";
import { activityDescription, dateKey, dayLabel } from "../activity/activity-model";
import type { SentencePart } from "../activity/SentenceParts";
import type { AgentPart } from "../refs/AgentName";
import { type PostDeletion, postDeletion } from "../refs/post-marks";
import {
  buildIssuePath,
  parseNodeReference,
  referenceHref,
  sessionOfAddress,
} from "../refs/routes";
import { type TextSource, textSourceOf } from "../refs/text-references";
import { agentsOnlyIds } from "./agents-only";
import { CONTROL_LABELS, issueStatusWord, NODE_TYPE_NAMES } from "./page-words";

/**
 * The issue's history, as the panel draws it: every event that is no row of the outline, newest
 * first, by day.
 *
 * It holds the issue's status moves, its claims and releases, its handoffs, its documents added
 * and removed, every other event of the issue itself, and the threads the history holds
 * (`agents-only.ts`) - each thread the Legion daemon began, whoever wrote in it, and each
 * agents' post, a comment thread tied to no text that no person wrote in - each thread with its
 * text, its replies and their reply controls.
 * It holds no event that writes a post: a thread it holds is drawn from its comments and
 * messages, and every other thread is the outline's. It leaves out each event of a row the
 * outline draws - an ask asked, answered, resolved, edited or handed on, what happened to a
 * comment or a message the outline draws, a result recorded against a check, a decision kept, a
 * child's status move - and a new version of a document, which is a version and no upload: a
 * document's versions are in its own toolbar. The upload of any other file or picture, its first
 * version or a later one, is an upload, and a run of uploads is one entry that opens each: one
 * author's newest upload and their uploads made at most a minute before it, with no other
 * author's upload between them.
 *
 * Pure, so the panel and its unit tests read one model.
 */
export interface HistoryDay {
  readonly date: string;
  /** What the day is headed, in the page's words: Today, Yesterday, or the date written out. */
  readonly label: string;
  /** The day's entries, newest first, with the reader's read mark among them where it falls. */
  readonly entries: readonly HistoryEntry[];
}

/** One turn of a thread the history holds (`agents-only.ts`), with the marks of a post its author
 *  corrected: when its author last edited it, and its deletion, which stands in place of its
 *  words (`post-marks.ts`). */
export interface HistoryTurn {
  readonly at: string;
  readonly author: Actor;
  readonly body: string;
  readonly bodyTree?: DisplayTree;
  readonly deleted: PostDeletion | null;
  readonly editedAt: string | null;
  readonly id: string;
  readonly kind: "comment" | "message";
}

export type HistoryEntry =
  | {
      readonly kind: "event";
      readonly id: string;
      readonly at: string;
      readonly author: Actor;
      /** `main`'s line of the event, in the page's words where it names a delivery's agent, a
       *  status or a block (`historyLine`). */
      readonly description: SentencePart[];
      /** The block the line names by its node's one line, where it names one, and which part of
       *  `description` stands for that line, a part of their own, which the panel draws as a link
       *  to the node (`historyLine`). */
      readonly block?: NamedBlock;
      readonly event: DispatchEvent;
      readonly seq: number;
    }
  | {
      readonly kind: "uploads";
      readonly id: string;
      readonly at: string;
      readonly author: Actor;
      /** The files the run uploaded, newest first, each by its own address: one author's newest
       *  upload and their uploads made at most a minute before it, with no other author's upload
       *  between them (`uploadRuns`). The run is named (`id`) by its newest upload. */
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

/** A block a line of the history names (`HistoryInput.blocks`): the node that carries it, as the
 *  page's one-line reader of a node takes it (`node-line.tsx`'s `useNodeLine`) - its id, the
 *  record its words are, its stored text, the server's reading of that text where the read sent
 *  one, and its kind - and the address of that node, where its address reads as one. */
export interface HistoryBlock {
  readonly node: {
    readonly id: string;
    readonly source: TextSource | undefined;
    readonly text: string;
    readonly tree: DisplayTree | undefined;
    readonly type: "ask" | "check";
  };
  readonly path: string | undefined;
}

/** A block a line of the history names by its node's one line (`withBlock`): the block, and the
 *  place in the line's `description` of the part that stands for that line and nothing else,
 *  which holds the page's word for the node's kind and which the panel draws the line in place
 *  of (`history-entries.tsx`'s `BlockLine`). */
export interface NamedBlock {
  readonly block: HistoryBlock;
  readonly part: number;
}

export interface HistoryInput {
  readonly artifacts: readonly Artifact[];
  /** The issue's asks, which tell a reply under a question, the outline's, from one under an
   *  approval request (`agents-only.ts`): the outline's own input, the issue's open asks and the
   *  asks read (`tree-drafts.ts`). */
  readonly asks: readonly Pick<Ask, "id" | "kind">[];
  /** The blocks the issue's nodes carry, by block id (`historyBlocks`), or undefined while a read
   *  they come from has not answered. A line that names a block by a UUID names it by its node's
   *  words instead; where no node here carries that block, by the page's words for a block that
   *  is gone; and while the blocks are not known, by `main`'s word for the block alone, since
   *  whether it is gone is not known yet (`historyLine`). */
  readonly blocks: ReadonlyMap<string, HistoryBlock> | undefined;
  readonly comments: readonly (Comment & Pick<CommentWithTrees, "body_tree">)[] | undefined;
  readonly events: readonly DispatchEvent[];
  /** `user_issue_state.last_read_seq`: the reader's read mark. */
  readonly lastReadSeq: number;
  /** When the oldest event the log has read so far was made, while older pages of the log are
   *  still to be read; undefined once the whole log is in. A thread that began before it is
   *  left out until its page is read: its place among events not read yet is not known. */
  readonly loadedFrom?: string;
  /** When the first event of the whole log past the read mark was made, where the log holds
   *  one; undefined where it holds none, or while that is not known. A thread whose first turn's
   *  event the log read does not hold is new where it began at that time or later, as the
   *  Conversation tab says a post is new (`conversation-model.ts`). */
  readonly unreadFrom?: string;
  readonly messages: readonly MessageRead[] | undefined;
  /** Local YYYY-MM-DD. */
  readonly today: string;
}

/** How long before a run's newest upload its author's other uploads may have been made and
 *  still be of that run: the run's minute is counted back from its newest upload alone. */
const UPLOAD_RUN_MS = 60_000;

/** A block id that is a UUID, which no reader names a block by. A block id that reads as words,
 *  a slug such as `c-units`, is a name its author chose, and a line keeps it. */
const UUID_BLOCK_ID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/iu;

/**
 * The blocks the issue's nodes carry, by block id, as the history names them (`HistoryBlock`):
 * each ask's typed block by its question, at the ask's own address, which puts the focus on its
 * row; each check of the results read by its words, at the address its `ref` reads as, as
 * Details' references link a node (`NodeReferences.tsx`). The panel draws each by its node's one
 * line, as the outline draws a node's, and by its kind's word where its words hold none.
 */
export function historyBlocks(
  issueKey: string,
  asks: readonly (Pick<Ask, "block_id" | "id" | "question"> &
    Partial<Pick<AskWithTrees, "question_tree">>)[],
  checks: readonly Pick<CheckBlock, "artifact_id" | "block_id" | "ref" | "text" | "text_tree">[]
): ReadonlyMap<string, HistoryBlock> {
  const blocks = new Map<string, HistoryBlock>();
  for (const check of checks) {
    const route = parseNodeReference(check.ref);
    blocks.set(check.block_id, {
      node: {
        id: check.block_id,
        source: textSourceOf("artifact", check.artifact_id, issueKey),
        text: check.text,
        tree: check.text_tree,
        type: "check",
      },
      path: route === undefined ? undefined : referenceHref(route),
    });
  }
  for (const ask of asks) {
    if (ask.block_id === null) continue;
    blocks.set(ask.block_id, {
      node: {
        id: ask.id,
        source: textSourceOf("ask", ask.id, issueKey),
        text: ask.question,
        tree: ask.question_tree,
        type: "ask",
      },
      path: buildIssuePath({ id: ask.id, key: issueKey, kind: "ask" }),
    });
  }
  return blocks;
}

/** The event types that write a post of a comment or message thread: its creation, an edit, a
 *  delete, an answer, and a message taken as an agent's turn. None is a line of the history,
 *  whichever thread it is of: a thread the history holds is drawn from its comments and
 *  messages, each turn whole with its marks, and the outline draws every other thread. */
const POST_WRITE_TYPES = [
  "comment.answered",
  "comment.created",
  "comment.deleted",
  "comment.edited",
  "message.accepted",
  "message.answered",
  "message.created",
  "message.deleted",
  "message.edited",
] as const;

const POST_WRITES: ReadonlySet<string> = new Set(POST_WRITE_TYPES);

/** The event types the history's read of the log leaves out (`exclude_types`): the post writes,
 *  so a page of the log holds 200 events the history can draw, and a log of thousands of
 *  comments is read in one request. */
export const HISTORY_EXCLUDED_TYPES = POST_WRITE_TYPES.join(",");

/** The event types of a row the outline draws, which the history leaves out. Every other event
 *  of a comment or a message - a delivery, a resolution, a reopening, an anchor refresh - is left
 *  out where the outline draws that comment or message, and is a line where it is a turn of a
 *  thread the history holds, since no node draws it (`ofOutlineRow`). An ask's are left out for
 *  a question alone: an approval request is no row of the outline - the identity block draws its
 *  step (`ApprovalSteps.tsx`) - so its asking, hand-back and answer are the history's, as are the
 *  followers added to it and removed. */
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
  "comment.delivery": true,
  "comment.reopened": true,
  "comment.resolved": true,
  "decision.kept": true,
  "message.delivery": true,
  "result.recorded": true,
  "result.retracted": true,
  "suggestion.accepted": true,
  "suggestion.rejected": true,
};

type Dated = Exclude<HistoryEntry, { kind: "unread" }>;

/** A line of the history (`historyLine`): its parts, and the block it names by its node's words,
 *  where it names one. */
type HistoryLine = Pick<Extract<Dated, { kind: "event" }>, "block" | "description">;

/** The issue's history by day, newest first (`HistoryDay`). */
export function buildHistory(input: HistoryInput): HistoryDay[] {
  const artifacts = new Map(input.artifacts.map((artifact) => [artifact.id, artifact]));
  const agentsOnly = agentsOnlyIds(input.comments, input.messages, input.asks);
  const approvals = new Set(input.asks.flatMap((ask) => (ask.kind === "approval" ? [ask.id] : [])));
  const entries: Dated[] = [];
  let previousStatus: string | undefined;
  // Each upload's event and file, oldest first, gathered into runs once every event is read.
  const uploads: { event: DispatchEvent; file: UploadedFile }[] = [];
  const ordered = [...input.events].sort((left, right) => left.seq - right.seq);
  for (const event of ordered) {
    const status =
      event.type === "issue.created" ||
      event.type === "issue.updated" ||
      event.type === "issue.closed"
        ? event.payload.status
        : undefined;
    const upload = uploadOf(event, artifacts);
    if (
      POST_WRITES.has(event.type) ||
      ofOutlineRow(event, agentsOnly, approvals) ||
      isDocumentVersion(event, artifacts)
    ) {
      if (status !== undefined) previousStatus = status;
      continue;
    }
    if (upload !== undefined) {
      uploads.push({ event, file: upload });
      continue;
    }
    entries.push({
      at: event.created_at,
      author: event.actor,
      ...historyLine(event, previousStatus, input.blocks),
      event,
      id: `event:${event.id}`,
      kind: "event",
      seq: event.seq,
    });
    if (status !== undefined) previousStatus = status;
  }
  entries.push(...uploadRuns(uploads.reverse()), ...threadsOf(input, agentsOnly));

  // Newest first, by when each happened; one instant by the log's own order, then by id.
  entries.sort((one, other) => comparePlaces(other, one));
  // Whether an entry is new to the reader: by its place in the log where the log read holds it,
  // past the read mark or not; else - a thread whose first turn's event the log read does not
  // hold, `seq` 0 - by when it began (`unreadFrom`), and neither while that is not known.
  const isNew = (entry: Dated): boolean | undefined => {
    if (entry.seq > 0) return entry.seq > input.lastReadSeq;
    if (input.unreadFrom === undefined) return undefined;
    return compareTimestamps(entry.at, input.unreadFrom) >= 0;
  };
  const read = entries.some((entry) => isNew(entry) === false);
  const unread = entries.some((entry) => isNew(entry) === true);
  let marked = !(read && unread) || entries.some((entry) => isNew(entry) === undefined);
  const days: { date: string; entries: HistoryEntry[] }[] = [];
  for (const entry of entries) {
    // The line that says what is new stands after the oldest unread entry, in that entry's own
    // day, which it closes where the reader last read through an earlier day.
    if (!marked && isNew(entry) === false) {
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
  // Today and yesterday are headed in the page's words, an older day by its date
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
  }));
}

/** How many entries the history draws at first, and how many more each press of its control
 *  draws: a thread with every turn of it is one entry, a run of uploads is one, and the line that
 *  says what is new is none (`historyDownTo`). */
export const HISTORY_PAGE = 20;

/** Where an entry stands in the history, newest first: when it happened, for one instant its
 *  place in the log (`buildHistory`), and for one place its id - two threads whose first turns'
 *  events the log read does not hold share place 0. The panel draws the history down to the
 *  place of the last entry it draws, so an entry that arrives above that place is drawn and
 *  takes none of the entries drawn out. */
export interface HistoryPlace {
  readonly at: string;
  readonly id: string;
  readonly seq: number;
}

/** How `one` stands to `other` in the history: above it (more than 0) where it is newer, at the
 *  same place (0), or under it (less than 0). */
export function comparePlaces(one: HistoryPlace, other: HistoryPlace): number {
  return (
    compareTimestamps(one.at, other.at) ||
    one.seq - other.seq ||
    (one.id < other.id ? -1 : one.id > other.id ? 1 : 0)
  );
}

/** The entries of `days` in the order the history draws them, newest first, with no read mark. */
function datedIn(days: readonly HistoryDay[]): Dated[] {
  return days.flatMap((day) =>
    day.entries.filter((entry): entry is Dated => entry.kind !== "unread")
  );
}

function placeOf(entry: Dated): HistoryPlace {
  return { at: entry.at, id: entry.id, seq: entry.seq };
}

/**
 * The place of the entry `count` entries under `from` in `days`, the entry at `from` not counted,
 * or of the history's `count`th entry where `from` is undefined: of its last entry where fewer
 * stand there, and `from` itself where none does. Undefined only where `days` holds no entry and
 * `from` is undefined.
 */
export function placeUnder(
  days: readonly HistoryDay[],
  from: HistoryPlace | undefined,
  count: number
): HistoryPlace | undefined {
  const under = datedIn(days).filter(
    (entry) => from === undefined || comparePlaces(entry, from) < 0
  );
  const last = under.slice(0, count).at(-1);
  return last === undefined ? from : placeOf(last);
}

/** The place of the entry of the thread that holds the turn `id`, its root or a reply, where
 *  `days` holds that thread. */
export function turnPlace(days: readonly HistoryDay[], id: string): HistoryPlace | undefined {
  const entry = datedIn(days).find(
    (one) =>
      one.kind === "thread" && (one.root.id === id || one.replies.some((reply) => reply.id === id))
  );
  return entry === undefined ? undefined : placeOf(entry);
}

/**
 * The history as the panel draws it down to `end`: each day with its entries that stand at `end`
 * or above it, and with the line that says what is new where the entry under that line is drawn
 * too; a day with none of its entries drawn is left out, and nothing is drawn where `end` is
 * undefined. `left` is how many entries stand under `end`, which the panel's control draws next.
 */
export function historyDownTo(
  days: readonly HistoryDay[],
  end: HistoryPlace | undefined
): { readonly days: readonly HistoryDay[]; readonly left: number } {
  const dated = datedIn(days);
  // The history is newest first, so the entries at `end` or above it are its first ones.
  const under = end === undefined ? 0 : dated.findIndex((entry) => comparePlaces(entry, end) < 0);
  const count = under === -1 ? dated.length : under;
  let seen = 0;
  const drawn: HistoryDay[] = [];
  for (const day of days) {
    const entries = day.entries.filter((entry) => {
      if (entry.kind === "unread") return seen < count;
      seen += 1;
      return seen <= count;
    });
    if (entries.length > 0) drawn.push({ ...day, entries });
  }
  return { days: drawn, left: dated.length - count };
}

/**
 * The runs of `uploads`, given newest first, as entries newest first. A run is its newest upload
 * and the uploads its author made at most a minute (`UPLOAD_RUN_MS`) before that one, with no
 * other author's upload between them: walked newest first, each upload joins the run above it
 * or begins the next. The minute is counted from the run's newest upload, never from the upload
 * before, so no run spans more than a minute however many uploads it holds. The log read further
 * back only walks on past the uploads already read: an older page extends the oldest run or
 * adds runs under it, and moves no file of a run drawn. A newer upload within a minute of the
 * newest run's newest upload joins that run and splits off its uploads more than a minute older
 * than itself into a run of their own, and the panel draws the newer part on under the key the
 * run had (`history-entries.tsx`'s `runKeys`). A run lists its files newest first, as the history
 * lists every entry, and is named by its newest upload, so an older page that extends it adds
 * files under the ones the reader is on and leaves its name: the entry is neither moved nor drawn
 * anew.
 */
function uploadRuns(
  uploads: readonly { readonly event: DispatchEvent; readonly file: UploadedFile }[]
): Extract<Dated, { kind: "uploads" }>[] {
  const runs: Extract<Dated, { kind: "uploads" }>[] = [];
  // The run being gathered: its author, its files and when its newest upload was made.
  let run:
    | { readonly at: number; readonly author: Actor; readonly files: UploadedFile[] }
    | undefined;
  for (const { event, file } of uploads) {
    if (
      run !== undefined &&
      run.author.kind === event.actor.kind &&
      run.author.id === event.actor.id &&
      run.at - Date.parse(event.created_at) <= UPLOAD_RUN_MS
    ) {
      run.files.push(file);
      continue;
    }
    run = { at: Date.parse(event.created_at), author: event.actor, files: [file] };
    runs.push({
      at: event.created_at,
      author: event.actor,
      files: run.files,
      id: `uploads:${event.id}`,
      kind: "uploads",
      seq: event.seq,
    });
  }
  return runs;
}

/** Each thread the history holds (`agents-only.ts`), as one entry at its first turn: every one
 *  that began inside what the log has read (`HistoryInput.loadedFrom`). A comment under an ask
 *  names the ask (`ask_id`), where the server's climb to a thread's head stops, so the thread of
 *  an approval request is every comment naming it (`agents-only.ts`), whatever its `reply_to`
 *  says: one entry per ask, at its earliest comment, the rest its replies. */
function threadsOf(input: HistoryInput, ids: ReadonlySet<string>): Dated[] {
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
    threads.push(threadEntry(commentTurn(root), replies.map(commentTurn)));
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
    threads.push(threadEntry(commentTurn(root), replies.map(commentTurn)));
  }
  for (const thread of input.messages ?? []) {
    if (!ids.has(thread.message.id)) continue;
    threads.push(
      threadEntry(messageTurn(thread.message), (thread.replies ?? []).map(messageTurn))
    );
  }
  // A thread stands at its first turn's time (`threadEntry`), so one whose first turn is older
  // than every event read so far is left out whole, its replies too however recent, until the
  // page that reaches back to that turn is read: drawn before, it would stand among entries the
  // log has not read. The address of one of its turns reads the log on until it is in
  // (`IssueHistory.tsx`).
  const from = input.loadedFrom;
  return from === undefined
    ? threads
    : threads.filter((thread) => compareTimestamps(thread.at, from) >= 0);
}

function threadEntry(
  root: HistoryTurn,
  replies: readonly HistoryTurn[]
): Extract<Dated, { kind: "thread" }> {
  return {
    at: root.at,
    author: root.author,
    id: `thread:${root.id}`,
    kind: "thread",
    replies,
    root,
    seq: 0,
  };
}

/**
 * The line of `event`, as today's Activity tab writes it (`activityDescription`), with what that
 * line names by a token in the page's words, as an address is no name: a delivery's target by
 * the agent it went to, never its address or `session:<id>`; the status a reopened issue went
 * into by its word; and a block it names by a UUID by a part holding the page's word for the
 * node's kind as a stand-in, with that block and the part's place (`withBlock`), which
 * `history-entries.tsx`'s `BlockLine` replaces with the node's one line (`BlockName`), never by
 * that id. A stored text the line quotes, an approval request's question or the reason it
 * was resolved with, is a part of its own, which the renderer draws (`SentenceParts`). Every
 * other text a line of the history holds is a name, a key or the server's own words, none of
 * which is markdown.
 */
function historyLine(
  event: DispatchEvent,
  previousStatus: string | undefined,
  blocks: ReadonlyMap<string, HistoryBlock> | undefined
): HistoryLine {
  const line = activityDescription(event, previousStatus);
  switch (event.type) {
    case "comment.delivery":
      return {
        description: withPart(
          line,
          event.payload.target,
          recipient(event.payload.session_id, undefined, event.payload.target)
        ),
      };
    // Only a queued message's line names where it went.
    case "message.delivery":
      return {
        description:
          event.payload.state === "queued"
            ? withPart(
                line,
                event.payload.target ?? `session:${event.payload.session_id}`,
                recipient(event.payload.session_id, event.payload.title, event.payload.target)
              )
            : line,
      };
    // The line names a status only for a reopen, which `activityDescription` tells by the
    // status before it.
    case "issue.updated":
      return {
        description:
          previousStatus === "done" && event.payload.status !== "done"
            ? withPart(line, event.payload.status, issueStatusWord(event.payload.status))
            : line,
      };
    // `main`'s line names a block by its id after its word for the block: a question's block is
    // a decision there, and a check's block a check.
    case "block.repaired":
      return withBlock(line, "block", event.payload.block_id, blocks);
    case "block.invalid":
      return withBlock(
        line,
        event.payload.block_type === "check" ? "check" : "decision",
        event.payload.block_id,
        blocks
      );
    default:
      return { description: line };
  }
}

/** `line` with each reading of `token` in its text, the last of each part, drawn as `parts`. */
function withPart(
  line: SentencePart[],
  token: string | undefined,
  ...parts: AgentPart[]
): SentencePart[] {
  return withPartAt(line, token, parts).line;
}

/** `line` with each reading of `token` in its text, the last of each part, drawn as `parts`
 *  (`withPart`), and the place in that new line where the parts drawn for the first reading
 *  begin, or -1 where `token` reads nowhere. An empty text, of `parts` or either side of a
 *  reading, is drawn as no part. */
function withPartAt(
  line: SentencePart[],
  token: string | undefined,
  parts: readonly AgentPart[]
): { readonly at: number; readonly line: SentencePart[] } {
  if (token === undefined || token === "") return { at: -1, line };
  const drawn: SentencePart[] = [];
  let first = -1;
  for (const piece of line) {
    const at = typeof piece === "string" ? piece.lastIndexOf(token) : -1;
    if (typeof piece !== "string" || at < 0) {
      drawn.push(piece);
      continue;
    }
    const before = piece.slice(0, at);
    if (before !== "") drawn.push(before);
    if (first < 0) first = drawn.length;
    for (const part of [...parts, piece.slice(at + token.length)]) {
      if (part !== "") drawn.push(part);
    }
  }
  return { at: first, line: drawn };
}

/** `line` with its mention of the block `id`, which is `noun`, `main`'s word for the block, then
 *  the id, in the page's words where the id is a UUID (`UUID_BLOCK_ID`): that word and, in quotes
 *  as `main` quotes a question, a part that stands for the one line of the node that carries the
 *  block, where a node of the issue carries it (`blocks`), with that block and the place of that
 *  part; the page's words for a block that is gone where none does; and that word alone after
 *  "a", as in "a block", while the blocks are not known, since whether the block is gone is not
 *  known then either. A block id that reads as words stays as `main` writes it. The part holds
 *  the page's word for the node's kind: the panel draws the node's line in its place, read as
 *  the outline reads a node's line, so no address its words cite is drawn as typed. */
function withBlock(
  line: SentencePart[],
  noun: string,
  id: string,
  blocks: ReadonlyMap<string, HistoryBlock> | undefined
): HistoryLine {
  if (!UUID_BLOCK_ID.test(id)) return { description: line };
  const mention = `${noun} ${id}`;
  if (blocks === undefined) return { description: withPart(line, mention, `a ${noun}`) };
  const block = blocks.get(id);
  if (block === undefined) {
    return { description: withPart(line, mention, CONTROL_LABELS.history_gone_block) };
  }
  const named = withPartAt(line, mention, [`${noun} “`, NODE_TYPE_NAMES[block.node.type], "”"]);
  // The node's line stands in the part after the word for the block and its opening quote.
  return named.at < 0
    ? { description: named.line }
    : { block: { block, part: named.at + 1 }, description: named.line };
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
    deleted: postDeletion(comment),
    editedAt: comment.edited_at,
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
    deleted: postDeletion(message),
    editedAt: message.edited_at,
    id: message.id,
    kind: "message",
  };
}

/** The comment or message an event of `OUTLINE_EVENTS` is about, where it is one of a comment's
 *  or a message's: a delivery, a resolution, a reopening or an anchor refresh. */
function turnOf(event: DispatchEvent): string | undefined {
  switch (event.type) {
    case "comment.delivery":
      return event.payload.comment_id;
    case "message.delivery":
      return event.payload.message_id;
    case "comment.anchor_refreshed":
    case "comment.reopened":
    case "comment.resolved":
      return event.payload.id;
    default:
      return undefined;
  }
}

/** Whether `event` is of a row the outline draws, and so no line of the history. What happened
 *  to a comment or a message of a thread the history holds (`agentsOnly`) is no row's, and an
 *  approval request's events are the history's: an event that carries the ask says its kind,
 *  and a follower's, which names the ask alone, is an approval request's where the issue's asks
 *  hold that ask as one (`approvals`, by id). */
function ofOutlineRow(
  event: DispatchEvent,
  agentsOnly: ReadonlySet<string>,
  approvals: ReadonlySet<string>
): boolean {
  if (OUTLINE_EVENTS[event.type] !== true) return false;
  const turn = turnOf(event);
  if (turn !== undefined && agentsOnly.has(turn)) return false;
  if (event.type === "ask.follower_added" || event.type === "ask.follower_removed") {
    return !approvals.has(event.payload.ask_id);
  }
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
