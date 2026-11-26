import type { Anchor } from "../../../api/types";
import { compareTimestamps, isTimestamp } from "../../../lib/timestamps";
import type { Thread, ThreadComment } from "../../margin/useMarginItems";
import { wordsOf } from "../../refs/tree-fragment";
import { assemble, type IssueTreeReads, type TreeNode, treeWordsOf } from "../issue-tree";
import { storedWords } from "../lenses/lens-drafts";
import type { Draft } from "../tree-drafts";
import { readingOf, treeDraftsFrom, treeIdOf } from "../tree-read/tree-nodes";
import type { TreeAnswer, TreeAnswerNode } from "../tree-read/tree-read";
import { GATES } from "./gates";
import {
  commentAnchorOf,
  ITEM_KINDS,
  type OnTextItem,
  type OnTextReads,
  onTextOf,
  onTextReads,
  placeNodes,
  type ReadComment,
  type ReadMessage,
  type TextPlace,
} from "./on-text";
import { quoteLine } from "./quote-line";
import { matchAt } from "./selection-quote";

/**
 * Each drawn node's threads on text (R2's place, R4's entries, R7's lenses, R9), from the reads
 * the view holds, with no read of its own.
 *
 * The roots are the comments and messages of the answer the view holds that R1's rule answers
 * `root` for (`onTextOf`), so a root on a comments page not yet loaded is no thread until that
 * page loads. A thread stands by the node its words are in (`placeNodes`: an orphaned anchor's
 * typed block, a document anchor's block, a node anchor's node, the item a reply quotes) where the
 * view draws it, else by the node the tree read hangs its root under, other than the issue; a
 * thread that has neither stands in no margin. Its entries are the root and each item whose chain
 * of answers reaches it, each with its body, author, time, resolution, `deleted_at` and
 * `reply_block` from the comments read or the messages read, and from the tree read's node where
 * neither read holds the item (another issue's node, G3c, takes every field from there).
 */

/** One thread on text, as the column and a node's sheet draw it. */
export interface TextThread {
  /** Whether its words stand at its node's first line: they left the node's text (`orphaned`), or
   *  the node's words hold no such match, so the row draws its first line instead. */
  readonly firstLine: boolean;
  /** Whether its root is a comment or a message: `thread.kind`, which the card reads, spelled out
   *  for the readers that name every entry by its kind (`contents-entries.ts`), from the one rule
   *  of `placedThread`. */
  readonly kind: "comment" | "message";
  /** The lenses on each comment of the thread, by the comment's id, each a row of W's with its
   *  findings and risks under it, in the read's order (R7). A comment with none has no entry. */
  readonly lenses: ReadonlyMap<string, readonly TreeNode[]>;
  /** Which match of `quote` in the node's words the thread is on, zero-based: the first for a
   *  document anchor until G3b (decision 7), the stored one for a node anchor and a reply. */
  readonly occurrence: number;
  /** Whether the words left the node's text: an orphaned anchor, or a reply's `removed` block. */
  readonly orphaned: boolean;
  /** The quoted words whole, as the read holds them, which the column finds in the row. */
  readonly quote: string;
  /** Whether the root is a reply with a quote, whose card draws no Resolve or Reopen (R4). */
  readonly replyRoot: boolean;
  /** The thread as the margin's card draws it (`ThreadCard`): its root's anchor and its own hold
   *  the quoted words cut to one line (`quoteLine`), and a thread rooted in a message is of the
   *  kind `"message"`, whose card draws no Resolve or Reopen and whose reply writes a message
   *  (R4). */
  readonly thread: Thread;
}

/**
 * Whether the item `id` names is in `thread`: its root, a reply in it, or a lens under one of its
 * comments, or a finding or a risk under such a lens, by the node's own id or its id in the tree
 * read (`?node=` names a lens by the latter). What an address of R8 and R7's landing name.
 */
export function threadHolds(thread: TextThread, id: string): boolean {
  const card = thread.thread;
  if (card.root.comment.id === id || card.replies.some((reply) => reply.id === id)) return true;
  for (const lenses of thread.lenses.values()) if (nodesHold(lenses, id)) return true;
  return false;
}

/** Whether `id` is the id, or the read id, of one of `nodes` or of a node under one. */
function nodesHold(nodes: readonly TreeNode[], id: string): boolean {
  return nodes.some((node) => node.id === id || node.readId === id || nodesHold(node.children, id));
}

/** The threads of one drawn node, in the order of their words. */
export interface NodeThreads {
  /** The tree id of the node's stored parent (its read parent, else its nearest drawn ancestor,
   *  else the issue): a node drawn under several parents - a check or a plan task also under each
   *  other requirement it serves - has its threads beside its row under this parent alone. */
  readonly parent: string;
  readonly threads: readonly TextThread[];
}

/** A root of a thread, with where its words are. */
interface Root {
  readonly node: TreeAnswerNode;
  readonly place: TextPlace;
}

/** No node has a thread. */
const NO_THREADS: ReadonlyMap<string, NodeThreads> = new Map();

/** No comment has a lens. */
const NO_LENSES: ReadonlyMap<string, readonly TreeNode[]> = new Map();

/**
 * The threads of each node `drawn` holds, by its tree id (part T's `treeIdOf`): `answer` is the
 * read the view holds, `reads` the page's reads (the comments and messages reads give each entry
 * its fields), `drawn` the tree id of each node the view draws. `idOf` and `onText` are the tree
 * ids and the rule's reads of that answer and those reads, where the caller holds them already.
 */
export function textThreadsOf(
  answer: TreeAnswer,
  reads: IssueTreeReads,
  drawn: ReadonlySet<string>,
  idOf: (readId: string) => string = treeIdOf(answer, reads),
  onText: OnTextReads = onTextReads(answer, reads)
): ReadonlyMap<string, NodeThreads> {
  const key = reads.issue.key;
  const roots = new Map<string, Root>();
  for (const node of onText.nodes.values()) {
    const foreign = node.issue !== undefined && node.issue !== key;
    if (node.state === "gone" || (foreign && !GATES.OTHER_ISSUE_COMMENTS)) continue;
    const found = onTextOf(node, onText);
    if (found?.kind === "root") roots.set(node.id, { node, place: found.place });
  }
  if (roots.size === 0) return NO_THREADS;
  const replies = new Map<string, ThreadComment[]>();
  for (const item of itemsOf(onText)) {
    const found = onTextOf(item, onText);
    if (found?.kind !== "reply" || !roots.has(found.root)) continue;
    const record = recordOf(item, onText, key);
    if (record === undefined) continue;
    const held = replies.get(found.root);
    if (held === undefined) replies.set(found.root, [record]);
    else held.push(record);
  }
  const items = new Set([...roots.keys(), ...[...replies.values()].flat().map(({ id }) => id)]);
  const lenses = lensesOf(answer, reads, onText, new Set(roots.keys()), items);
  const words = new Map<TreeAnswerNode, string>();
  const placed = new Map<string, { node: TreeAnswerNode; rows: Placed[] }>();
  for (const [id, { node, place }] of roots) {
    const at = placedAt(node, place, onText, idOf, drawn, key);
    const record = at === undefined ? undefined : recordOf(node, onText, key);
    if (at === undefined || record === undefined) continue;
    let held = words.get(at);
    if (held === undefined) {
      // The node's words as a stored text's plain words read (`storedWords`): one space between
      // words, each address as its author wrote it, as the server's quote holds it.
      const reading = readingOf(at);
      held =
        reading === undefined
          ? storedWords(at.text)
          : wordsOf(reading).replace(/\s+/gu, " ").trim();
      words.set(at, held);
    }
    const entries = (replies.get(id) ?? []).sort(
      (left, right) =>
        compareTimes(left.created_at, right.created_at) || compareIds(left.id, right.id)
    );
    const row = placedThread(node, place, record, entries, lenses, held);
    const nodeId = idOf(at.id);
    const group = placed.get(nodeId);
    if (group === undefined) placed.set(nodeId, { node: at, rows: [row] });
    else group.rows.push(row);
  }
  const threads = new Map<string, NodeThreads>();
  for (const [nodeId, { node, rows }] of placed) {
    rows.sort(
      (left, right) =>
        left.at - right.at ||
        compareTimes(left.made, right.made) ||
        compareIds(left.thread.thread.key, right.thread.thread.key)
    );
    threads.set(nodeId, {
      parent: storedParentOf(node, onText, idOf, drawn, key),
      threads: rows.map((row) => row.thread),
    });
  }
  return threads;
}

/** A thread with where its words start in its node's words, -1 at the node's first line, and when
 *  its root was written, which order it among its node's threads. */
interface Placed {
  readonly at: number;
  readonly made: string;
  readonly thread: TextThread;
}

/**
 * The thread of `root`, whose words are at `place`, as the card draws it (`record`, with
 * `entries` its replies in time order), with where its words start in `words`, its node's words:
 * the match its occurrence names, counted from the start of the quoted block for a reply, and the
 * node's first line for words that left the text or that the node's words do not hold.
 */
function placedThread(
  root: TreeAnswerNode,
  place: TextPlace,
  record: ThreadComment,
  entries: ThreadComment[],
  lenses: ReadonlyMap<string, readonly TreeNode[]>,
  words: string
): Placed {
  const quote = place.kind === "block" ? place.anchor.quote : place.quote;
  const occurrence =
    place.kind !== "block"
      ? place.occurrence
      : GATES.ANCHOR_OCCURRENCE
        ? (place.occurrence ?? 0)
        : 0;
  const orphaned =
    place.kind === "block"
      ? place.anchor.orphaned
      : place.kind === "node"
        ? place.orphaned
        : place.block.removed;
  const from =
    place.kind === "item" ? Math.max(0, words.indexOf(storedWords(place.block.text))) : 0;
  const at = orphaned ? undefined : quotedAt(words, quote, occurrence, from);
  const anchor = cardAnchorOf(place);
  const kind = root.supertag === "message" ? "message" : "comment";
  const own = new Map(
    [record.id, ...entries.map((entry) => entry.id)].flatMap(
      (item): [string, readonly TreeNode[]][] => {
        const held = lenses.get(item);
        return held === undefined ? [] : [[item, held]];
      }
    )
  );
  return {
    at: at ?? -1,
    made: record.created_at,
    thread: {
      firstLine: at === undefined,
      kind,
      lenses: own,
      occurrence,
      orphaned,
      quote,
      replyRoot: place.kind === "item",
      thread: {
        anchor,
        key: record.id,
        ...(kind === "message" ? { kind } : {}),
        lastReplyAt: entries.at(-1)?.created_at,
        replies: entries,
        resolved:
          record.resolved || (record.suggestion !== null && record.suggestion.accepted !== null),
        root: { comment: { ...record, anchor }, kind: "comment" },
      },
    },
  };
}

/** Every comment and message the reads and the answer hold, each once: the comments read's, the
 *  messages read's, then the answer's items neither read holds. */
function itemsOf(onText: OnTextReads): OnTextItem[] {
  const items: OnTextItem[] = [];
  for (const id of onText.comments.keys()) items.push({ id, supertag: "comment" });
  for (const id of onText.messages.keys()) items.push({ id, supertag: "message" });
  for (const node of onText.nodes.values()) {
    if (node.supertag !== "comment" && node.supertag !== "message") continue;
    if (node.state === "gone" || onText.comments.has(node.id) || onText.messages.has(node.id)) {
      continue;
    }
    items.push(node);
  }
  return items;
}

/** An item as the card draws it: the comments read's comment, else the messages read's message,
 *  else the tree read's node; undefined for a node that names no author. */
function recordOf(item: OnTextItem, onText: OnTextReads, key: string): ThreadComment | undefined {
  const comment: ReadComment | undefined = onText.comments.get(item.id);
  if (comment !== undefined) return comment;
  const message = onText.messages.get(item.id);
  if (message !== undefined) return messageRecord(message);
  const node = onText.nodes.get(item.id);
  return node === undefined ? undefined : nodeRecord(node, key);
}

/** A message as the card draws a comment: no anchor, resolution or suggestion, which no route of a
 *  message has, and the message it answers as the comment it answers. */
function messageRecord(message: ReadMessage): ThreadComment {
  return {
    anchor: null,
    answers: message.in_reply_to,
    artifact_id: null,
    ask_id: null,
    author: message.author,
    body: message.body,
    ...(message.body_tree === undefined ? {} : { body_tree: message.body_tree }),
    created_at: message.created_at,
    deleted_at: message.deleted_at,
    deleted_by: message.deleted_by,
    edited_at: message.edited_at,
    id: message.id,
    issue_key: message.issue_key,
    reply_block: message.reply_block,
    reply_to: message.in_reply_to,
    resolved: false,
    resolved_at: null,
    resolved_by: null,
    suggestion: null,
    turn: null,
  };
}

/** An item as the tree read answers it, which the card draws as a comment: its words and their
 *  reading, its first author and its time, under the item the read hangs it under. */
function nodeRecord(node: TreeAnswerNode, key: string): ThreadComment | undefined {
  const author = node.author?.[0];
  if (author === undefined) return undefined;
  const { parent } = node;
  const answered =
    parent !== null && (parent.supertag === "comment" || parent.supertag === "message")
      ? parent.id
      : null;
  return {
    anchor: node.anchor === undefined ? null : commentAnchorOf(node.anchor),
    answers: answered,
    artifact_id: null,
    ask_id: null,
    author,
    body: node.text,
    ...(node.text_tree === undefined ? {} : { body_tree: node.text_tree }),
    created_at: node.made_at ?? "",
    deleted_at: null,
    deleted_by: null,
    edited_at: null,
    id: node.id,
    issue_key: node.issue ?? key,
    reply_block: null,
    reply_to: answered,
    resolved: false,
    resolved_at: null,
    resolved_by: null,
    suggestion: null,
    turn: null,
  };
}

/** The node a root's thread stands by: the first node its place names that the view draws, else
 *  the node the read hangs it under where the view draws that and it is not the issue. */
function placedAt(
  root: TreeAnswerNode,
  place: TextPlace,
  onText: OnTextReads,
  idOf: (readId: string) => string,
  drawn: ReadonlySet<string>,
  key: string
): TreeAnswerNode | undefined {
  for (const node of placeNodes(place, onText)) {
    if (drawn.has(idOf(node.id))) return node;
  }
  const { parent } = root;
  if (parent === null || parent.supertag === "issue" || idOf(parent.id) === key) return undefined;
  const held = onText.nodes.get(parent.id);
  return held !== undefined && drawn.has(idOf(held.id)) ? held : undefined;
}

/** The tree id of `node`'s stored parent as the view draws it: its read parent where drawn, else
 *  its nearest drawn ancestor, else the issue. A chain that runs in a circle ends at the issue. */
function storedParentOf(
  node: TreeAnswerNode,
  onText: OnTextReads,
  idOf: (readId: string) => string,
  drawn: ReadonlySet<string>,
  key: string
): string {
  for (const parent of ancestorsOf(node, onText)) {
    const id = idOf(parent.id);
    if (id === key || drawn.has(id)) return id;
  }
  return key;
}

/** The nodes above `node` in the read, nearest first: its `parent`, then each one's own parent as
 *  the answer holds it. A chain that runs in a circle ends where it comes round. */
function* ancestorsOf(
  node: TreeAnswerNode,
  onText: OnTextReads
): Generator<NonNullable<TreeAnswerNode["parent"]>> {
  const passed = new Set<string>();
  for (
    let parent = node.parent;
    parent !== null && !passed.has(parent.id);
    parent = onText.nodes.get(parent.id)?.parent ?? null
  ) {
    passed.add(parent.id);
    yield parent;
  }
}

/** The anchor the card draws: the place's own, with its quote cut to one line (`quoteLine`). */
function cardAnchorOf(place: TextPlace): Anchor {
  switch (place.kind) {
    case "block": {
      const { anchor } = place;
      return {
        artifact_id: anchor.artifact_id,
        block_id: anchor.block_id,
        mark_id: anchor.mark_id,
        orphaned: anchor.orphaned,
        quote: quoteLine(anchor.quote),
        version: anchor.version,
      };
    }
    case "node":
      return {
        artifact_id: "",
        block_id: null,
        mark_id: "",
        orphaned: place.orphaned,
        quote: quoteLine(place.quote),
        version: 0,
      };
    case "item":
      return {
        artifact_id: "",
        block_id: place.block.block_id,
        mark_id: "",
        orphaned: place.block.removed,
        quote: quoteLine(place.quote),
        version: 0,
      };
  }
}

/** Where match `occurrence` of `quote` starts in `words`, counting from `from` and overlapping
 *  matches included, as `quoteAnchor` counts, with the quote's white space read as `storedWords`
 *  reads it; undefined where there is no such match or the quote holds no words. */
function quotedAt(
  words: string,
  quote: string,
  occurrence: number,
  from: number
): number | undefined {
  const wanted = quote.replace(/\s+/gu, " ").trim();
  if (wanted === "") return undefined;
  return matchAt(words, wanted, occurrence, from, words.length);
}

/** How two times stand, by the instants they name, and by their text where either is no
 *  timestamp. */
function compareTimes(left: string, right: string): number {
  return isTimestamp(left) && isTimestamp(right)
    ? compareTimestamps(left, right)
    : compareIds(left, right);
}

/** Two ids in byte order. */
function compareIds(left: string, right: string): number {
  return left === right ? 0 : left < right ? -1 : 1;
}

/**
 * The lenses on each item of `items`, the threads' comments and messages, by the item's id (R7):
 * each a row of W's, drafted by W's own steps (`lensDrafts`, `lensChips`, `storeWords`) in a draft
 * of the answer where each thread's root, and so each item of its thread, is a drafted row, so W's
 * `parentsOf` places each lens under its item or under the item a block of it is of; then built by
 * T's `assemble`, so each lens holds its findings and risks. A view whose threads carry no lens
 * drafts nothing.
 */
function lensesOf(
  answer: TreeAnswer,
  reads: IssueTreeReads,
  onText: OnTextReads,
  roots: ReadonlySet<string>,
  items: ReadonlySet<string>
): ReadonlyMap<string, readonly TreeNode[]> {
  if (!hasThreadLens(onText, items)) return NO_LENSES;
  const words = treeWordsOf(reads);
  const messageAsRow = (message: ReadMessage): ReadMessage =>
    roots.has(message.id) ? { ...message, reply_block: null } : message;
  const rows = treeDraftsFrom(
    {
      ...answer,
      nodes: (answer.nodes ?? []).map((node) => {
        if (!roots.has(node.id)) return node;
        const { anchor: _anchor, ...rest } = node;
        return rest;
      }),
    },
    {
      ...reads,
      // A root of the comments read with nothing that puts it on text: no anchor, no node anchor
      // (G2) and no block it quotes (G4).
      comments: reads.comments?.map((comment) => {
        if (!roots.has(comment.id)) return comment;
        const { node_anchor: _nodeAnchor, ...rest }: ReadComment & { node_anchor?: unknown } =
          comment;
        return { ...rest, anchor: null, reply_block: null };
      }),
      messages: reads.messages?.map((thread) => ({
        ...thread,
        message: messageAsRow(thread.message),
        replies: thread.replies?.map(messageAsRow) ?? null,
      })),
    },
    words
  );
  const under = new Map<string, Draft[]>();
  for (const draft of rows.values()) {
    for (const parent of new Set(draft.parents)) {
      const held = under.get(parent);
      if (held === undefined) under.set(parent, [draft]);
      else held.push(draft);
    }
  }
  const kept = new Map<string, Draft>();
  const keepUnder = (id: string): void => {
    for (const child of under.get(id) ?? []) {
      if (kept.has(child.id)) continue;
      kept.set(child.id, child);
      keepUnder(child.id);
    }
  };
  for (const item of items) {
    const draft = rows.get(item);
    const lenses = (under.get(item) ?? []).filter((child) => child.type === "lens");
    if (draft === undefined || lenses.length === 0) continue;
    kept.set(item, { ...draft, parents: [] });
    for (const lens of lenses) {
      if (kept.has(lens.id)) continue;
      kept.set(lens.id, lens);
      keepUnder(lens.id);
    }
  }
  const tree = assemble(reads.issue, [...kept.values()], words.chipOf);
  return new Map(
    tree.children.flatMap((child): [string, readonly TreeNode[]][] =>
      items.has(child.id) ? [[child.id, child.children.filter((one) => one.type === "lens")]] : []
    )
  );
}

/** Whether a lens of the answer stands on an item of `items`, or on a block of one: the nearest
 *  ask, comment or message above it in the read is one of them. */
function hasThreadLens(onText: OnTextReads, items: ReadonlySet<string>): boolean {
  for (const node of onText.nodes.values()) {
    if (node.supertag !== "lens") continue;
    for (const parent of ancestorsOf(node, onText)) {
      if (ITEM_KINDS[parent.supertag] === true) {
        if (items.has(parent.id)) return true;
        break;
      }
    }
  }
  return false;
}
