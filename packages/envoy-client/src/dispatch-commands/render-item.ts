import type {
  Actor,
  AskRead,
  AskWithTrees,
  BlockPath,
  Comment,
  CommentRead,
  GraphEdge,
  GraphReferences,
  MessageRead,
  OpenAsk,
  OpenAsksResponse,
  PostHistoryRead,
} from "@legion/contracts";
import { actorActText, encodeBlockId } from "@legion/contracts";
import { actorText, deletedMark } from "../actor-text";
import { askAnswerText, dispatchWordsText, textHead } from "../ask-answer";
import type { DispatchClient } from "../dispatch-http";
import { ownerTitle } from "../dispatch-owner";
import type { ResolvedArtifact } from "./context";
import { dispatchAnswered } from "./failures";
import { edgeLine, graphSectionLines } from "./render-graph";

/**
 * An ask's read. Each option and the answer is a part of the ask in the reference graph, so each
 * is named by its own reference (`dispatch://KEY/ask/<id>#b-<part>`) with, under it, the edges
 * pointing at that part; `askRef` is the ask's own address those are built from. Every other
 * edge stays under the ask, the ones at its question among them: no read carries the question's
 * part id. An ask whose parts carry no id — an older server, or an ask the index has not reached,
 * whose revision is 0 — reads as it did before the parts: its options bare, every edge under the
 * ask.
 */
export function askSummary(
  { ask, replies, answers }: AskRead,
  askRef: string,
  pictures: readonly string[],
  incoming: GraphReferences | string,
  outgoing: GraphReferences | string
): string {
  const answer = ask.answer;
  const earlierAnswers = (answers ?? []).slice(0, -1);
  const parts = askPartEdges(ask, incoming);
  const partRef = (id: string) => `${askRef}#b-${encodeBlockId(id)}`;
  const chain = (replies ?? []).flatMap((reply) => [
    `${reply.id} · ${actorText(reply.author)}`,
    ...postBodyLines(reply),
  ]);
  const resolutionWords = dispatchWordsText(ask.resolution?.dispatch_words);
  return [
    `Question: ${ask.question}`,
    ...anchorLines(ask),
    "Options:",
    ...((ask.options ?? []).length === 0
      ? ["- none"]
      : (ask.options ?? []).flatMap((option) => [
          `- ${option.label}${option.description ? ` — ${option.description}` : ""}${option.id === undefined ? "" : ` (${partRef(option.id)})`}`,
          ...partReferenceLines(parts.at, option.id),
        ])),
    `State: ${ask.state}`,
    // A read can name the ask by its block's id; the writes to it take this one.
    `Ask id: ${ask.id}`,
    "Answer:",
    ...(answer === null
      ? ["- none"]
      : [
          `- By: ${actorActText(answer.user, answer.act)}`,
          `- Selected: ${(answer.selected ?? []).length === 0 ? "none" : (answer.selected ?? []).join(", ")}`,
          ...(answer.text === null ? [] : [`- Text: ${answer.text}`]),
          ...(answer.id === undefined ? [] : [`- Reference: ${partRef(answer.id)}`]),
          ...partReferenceLines(parts.at, answer.id),
        ]),
    ...(earlierAnswers.length === 0
      ? []
      : [
          "Earlier answers:",
          ...earlierAnswers.map(
            (earlier) =>
              `- ${earlier.at} · ${actorActText(earlier.user, earlier.act)} · ${askAnswerText(earlier)}`
          ),
        ]),
    ...(ask.resolution === undefined
      ? []
      : [
          "Resolution:",
          `- By: ${ask.resolution.actor.id}`,
          `- Kind: ${ask.resolution.kind}`,
          // A decline the person wrote no note with holds no reason, so the line is left out
          // rather than printed empty; the sentences Dispatch wrote for the asking agent stand
          // on a bullet of their own, under the label every reader of them names them with
          // (`dispatchWordsText`), so nobody reads them as the person's words.
          ...(ask.resolution.reason === "" ? [] : [`- Reason: ${ask.resolution.reason}`]),
          ...(resolutionWords === undefined ? [] : [`- ${resolutionWords}`]),
        ]),
    "Replies:",
    ...(chain.length === 0 ? ["- none"] : chain),
    ...pictures,
    ...graphSectionLines(parts.rest, outgoing),
  ].join("\n");
}

/** An ask's inbound edges split by the part each points at: `at` holds the edges at one of its
 *  options or at its answer, keyed by that part's id, and `rest` is what the ask's own section
 *  keeps — the edges at the ask itself and at its question. An edge listed under its part names
 *  no block: the line above it is that part. A graph the server could not serve is passed on as
 *  it is, and so is one read of an ask whose parts carry no id. */
function askPartEdges(
  ask: AskWithTrees,
  incoming: GraphReferences | string
): { at: Map<string, GraphEdge[]>; rest: GraphReferences | string } {
  const ids = new Set(
    [...(ask.options ?? []).map((option) => option.id), ask.answer?.id].filter(
      (id): id is string => id !== undefined
    )
  );
  const at = new Map<string, GraphEdge[]>();
  if (typeof incoming === "string" || ids.size === 0) return { at, rest: incoming };
  const rest: GraphEdge[] = [];
  for (const edge of incoming.edges ?? []) {
    const part = edge.block?.block_id;
    if (part === undefined || !ids.has(part)) {
      rest.push(edge);
      continue;
    }
    const { block: _block, ...atPart } = edge;
    const held = at.get(part);
    if (held === undefined) at.set(part, [atPart]);
    else held.push(atPart);
  }
  return { at, rest: { node: incoming.node, edges: rest } };
}

/** The edges at one part of an ask, indented under the line naming that part. A part nothing
 *  points at, and one the ask carries no id for, print nothing. */
function partReferenceLines(at: Map<string, GraphEdge[]>, id: string | undefined): string[] {
  const edges = id === undefined ? undefined : at.get(id);
  if (edges === undefined || edges.length === 0) return [];
  return ["  Referenced by:", ...edges.map((edge) => `  ${edgeLine(edge)}`)];
}

function openAskAge(ageSeconds: number): string {
  const seconds = Math.max(0, Math.floor(ageSeconds));
  if (seconds < 60) return `${seconds}s`;
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return `${minutes}m${seconds % 60 === 0 ? "" : ` ${seconds % 60}s`}`;
  const hours = Math.floor(minutes / 60);
  if (hours < 24) return `${hours}h${minutes % 60 === 0 ? "" : ` ${minutes % 60}m`}`;
  const days = Math.floor(hours / 24);
  return `${days}d${hours % 24 === 0 ? "" : ` ${hours % 24}h`}`;
}

/** The person an open ask is asked of, as the server resolved it (`waiting_on_person`: whoever
 *  the ask was handed to, and the assignee of its issue until somebody hands it on), and
 *  `nobody` where the server names none — an ask on a project document, or on an issue nobody
 *  holds, that nobody was handed. */
function openAskAskedOf(ask: OpenAsk): string {
  const person = ask.waiting_on_person;
  return person === undefined || person === "" ? "nobody" : person;
}

/** One row of `dispatch open-asks`, in both of the listing's groups: the person it is asked of
 *  is on every row, since one row renderer is one rule. The word is the ask card's own, `asked
 *  of`, rather than `waiting on`, because a row under `Waiting on agent (N):` waits on the
 *  agent - the next move there is the agent's, and naming the person must not claim otherwise. */
function openAskLine(ask: OpenAsk, baseUrl: string): string {
  const priority = ask.priority === null ? "" : `P${ask.priority} · `;
  return `- ${openAskAge(ask.age_seconds)} · ${priority}${ownerTitle(ask.owner)} · asked of ${openAskAskedOf(ask)} · ${ask.question} · ${new URL(ask.ref, baseUrl).toString()}`;
}

export function formatOpenAsksSummary(response: OpenAsksResponse, baseUrl: string): string {
  const scope = "active open asks you authored on open issues and project documents";
  if (response.count === 0) {
    return ["There are no unanswered asks for this session.", `Scope: ${scope}.`].join("\n");
  }
  const waitingOnHuman = (response.asks ?? []).filter((ask) => ask.waiting_on === "human");
  const waitingOnAgent = (response.asks ?? []).filter((ask) => ask.waiting_on === "agent");
  return [
    `${response.count} unanswered ${response.count === 1 ? "ask" : "asks"} you authored on active issues and project documents.`,
    "",
    `Waiting on human (${waitingOnHuman.length}):`,
    ...waitingOnHuman.map((ask) => openAskLine(ask, baseUrl)),
    "",
    `Waiting on agent (${waitingOnAgent.length}):`,
    ...waitingOnAgent.map((ask) => openAskLine(ask, baseUrl)),
  ].join("\n");
}
/** A post as a read prints it: the deleted mark alone for a withdrawn post, else the body with
 *  the time of the last edit above it where the author replaced it. */
function postBodyLines(post: {
  readonly body: string;
  readonly edited_at?: string | null;
  readonly deleted_at?: string | null;
  readonly deleted_by?: Actor | null;
}): string[] {
  if (post.deleted_at != null) return [deletedMark(post.deleted_at, post.deleted_by)];
  return [...(post.edited_at == null ? [] : [`Edited: ${post.edited_at}`]), `Body: ${post.body}`];
}

/**
 * What `dispatch read --history` prints: each body an edit or a delete of the post replaced,
 * newest first, under the time of that write and the actor that made it. A post nobody edited
 * lists none. A deleted post gives no part of its body to any read, so its list carries the
 * deleted mark and the times alone.
 */
export function postHistorySummary(
  kind: "Comment" | "Message",
  ref: string,
  read: PostHistoryRead
): string {
  const entries = read.entries ?? [];
  const deleted = read.deleted_at != null;
  return [
    `${kind} history: ${ref}`,
    ...(read.deleted_at == null ? [] : [deletedMark(read.deleted_at, read.deleted_by)]),
    ...(entries.length === 0
      ? ["- none"]
      : entries.flatMap((entry) => [
          `${entry.edited_at} · ${actorText(entry.edited_by)}`,
          ...(deleted ? [] : [`Body: ${entry.body}`]),
        ])),
  ].join("\n");
}

export function commentSummary(
  { comment, replies }: CommentRead,
  graph: readonly string[]
): string {
  const root = [
    `${comment.id} · ${actorText(comment.author)}`,
    ...anchorLines(comment),
    ...replyBlockLines(comment),
    ...postBodyLines(comment),
  ];
  const chain = (replies ?? []).flatMap((reply) => [
    `${reply.id} · ${actorText(reply.author)}`,
    ...anchorLines(reply),
    ...replyBlockLines(reply),
    ...(reply.answers != null && reply.answers !== comment.id ? [`Answers: ${reply.answers}`] : []),
    ...postBodyLines(reply),
  ]);
  return [
    "Comment:",
    ...root,
    "Reply chain:",
    ...(chain.length === 0 ? ["- none"] : chain),
    ...graph,
  ].join("\n");
}

/** The block of its parent a reply answers: its id and the head of its text, that no current
 *  revision of the parent holds it, or, where the parent is a post its author deleted, the
 *  deleted line every read prints for one (`deletedMark`) in place of the text the delete
 *  cleared. A comment or message that answers no block prints nothing. */
function replyBlockLines(record: Pick<Comment, "reply_block">): string[] {
  const block = record.reply_block;
  if (block === undefined || block === null) return [];
  if (block.deleted_at != null) {
    return [
      `Re: block ${block.block_id} (parent deleted): ${deletedMark(block.deleted_at, block.deleted_by)}`,
    ];
  }
  return [
    block.removed
      ? `Re: block ${block.block_id} (removed)`
      : `Re: block ${block.block_id}: ${textHead(block.text)}`,
  ];
}

/** An anchored record's quote, then where its block stands: the position, or why the read could
 *  not place it. A record without an anchor prints neither. */
function anchorLines(
  record: Pick<Comment, "anchor" | "anchor_block" | "anchor_block_error">
): string[] {
  return [
    ...(record.anchor?.quote === undefined ? [] : [`> ${record.anchor.quote}`]),
    ...(record.anchor_block === undefined
      ? []
      : [`Position: ${positionText(record.anchor_block)}`]),
    ...(record.anchor_block_error === undefined
      ? []
      : [`Position: unavailable (${record.anchor_block_error})`]),
  ];
}

/** Where an anchor's block stands. Every node from the top-level block down reads `type[index]`;
 *  in a table the row and cell read instead as `row 5 (Red-teamer loop), column Due`: the row's
 *  index (0 is the header), labelled by its cells before the anchored column — blank cells and
 *  bare numbers dropped, since a `#` column repeats the index — and the column's header, or its
 *  index where no header cell covers it. */
export function positionText(block: BlockPath): string {
  const { table, path } = block;
  const segments = (path ?? []).map((entry) => `${entry.type}[${entry.index}]`);
  if (table === undefined || table.row === null) return segments.join(" › ");
  const tableAt = (path ?? []).findIndex((entry) => entry.type === "table");
  const cells = table.cells ?? [];
  const label = (table.column === null ? cells : cells.slice(0, table.column))
    .map((cell) => cell.trim())
    .filter((cell) => cell !== "" && !/^\d+$/.test(cell))
    .join(" · ");
  const row = label === "" ? `row ${table.row}` : `row ${table.row} (${label})`;
  const header = table.header === null || table.header === "" ? String(table.column) : table.header;
  return [
    ...segments.slice(0, tableAt + 1),
    table.column === null ? row : `${row}, column ${header}`,
  ].join(" › ");
}

export function messageSummary(
  { message, replies }: MessageRead,
  graph: readonly string[]
): string {
  const root = [
    `${message.id} · ${actorText(message.author)}`,
    ...replyBlockLines(message),
    ...postBodyLines(message),
  ];
  const chain = (replies ?? []).flatMap((reply) => [
    `${reply.id} · ${actorText(reply.author)}`,
    ...replyBlockLines(reply),
    ...postBodyLines(reply),
  ]);
  return [
    "Message:",
    ...root,
    "Reply chain:",
    ...(chain.length === 0 ? ["- none"] : chain),
    ...graph,
  ].join("\n");
}

/** Each open thread anchored on the document, in full, as dispatch_doc_read prints it: an open ask
 *  with its question and every reply under it (a comment whose `ask_id` names the ask), then each
 *  open comment thread with its root and every reply in order. A reply carries no anchor, so the
 *  threads are read from the owner's comments unfiltered and grouped by their ask or root. A
 *  resolved or answered ask and a resolved thread are not printed, nor are their replies. */
export async function openAnchoredThreads(
  client: DispatchClient,
  resolved: ResolvedArtifact
): Promise<string[]> {
  const asksPromise =
    resolved.owner.kind === "project"
      ? client.getArtifactAsks(resolved.artifact.id)
      : Promise.resolve(resolved.issue?.open_asks ?? []);
  const commentsPromise =
    resolved.owner.kind === "project"
      ? client.getArtifactComments(resolved.artifact.id)
      : client.getComments(resolved.issue?.key ?? "");
  const commentsResultPromise = commentsPromise.then(
    (value) => ({ status: "fulfilled" as const, value }),
    (reason) => ({ status: "rejected" as const, reason })
  );
  // An ask error takes precedence over a comment error. Capture a concurrent comment failure
  // so it is handled without delaying an ask failure.
  const asks = (await asksPromise).filter(
    (ask) => ask.state === "open" && ask.anchor?.artifact_id === resolved.artifact.id
  );
  const commentsResult = await commentsResultPromise;
  if (commentsResult.status === "rejected" && !dispatchAnswered(commentsResult.reason, 404)) {
    throw commentsResult.reason;
  }
  const comments = commentsResult.status === "fulfilled" ? commentsResult.value : [];
  const askReplies = new Map<string, Comment[]>(asks.map((ask) => [ask.id, []]));
  for (const comment of comments) {
    if (comment.ask_id) askReplies.get(comment.ask_id)?.push(comment);
  }
  const askThreads = asks.map((ask) =>
    [
      `ask ${ask.id} · ${actorText(ask.author)} · ${ask.created_at}`,
      ...anchorLines(ask),
      `Question: ${ask.question}`,
      ...threadReplyLines(askReplies.get(ask.id) ?? []),
    ].join("\n")
  );
  const byId = new Map(comments.map((comment) => [comment.id, comment]));
  const roots = comments.filter(
    (comment) =>
      comment.reply_to === null &&
      !comment.ask_id &&
      !comment.resolved &&
      comment.anchor?.artifact_id === resolved.artifact.id
  );
  const replies = new Map<string, Comment[]>(roots.map((root) => [root.id, []]));
  for (const comment of comments) {
    const root = comment.reply_to === null ? undefined : threadRootId(comment, byId);
    if (root !== undefined) replies.get(root)?.push(comment);
  }
  const commentThreads = roots.map((root) =>
    [
      `comment ${root.id} · ${actorText(root.author)} · ${root.created_at}`,
      ...anchorLines(root),
      ...postBodyLines(root),
      ...threadReplyLines(replies.get(root.id) ?? []),
    ].join("\n")
  );
  return [...askThreads, ...commentThreads];
}

/** A thread's replies as dispatch_doc_read prints them under their ask or root comment. */
function threadReplyLines(replies: readonly Comment[]): string[] {
  return replies.flatMap((reply) => [
    `reply ${reply.id} · ${actorText(reply.author)} · ${reply.created_at}`,
    ...postBodyLines(reply),
  ]);
}

/** The root of a reply's thread: Dispatch points `reply_to` at the root, and an older row may point
 *  at another reply, so the walk follows it up. undefined for a chain that leaves the list or loops. */
function threadRootId(comment: Comment, byId: ReadonlyMap<string, Comment>): string | undefined {
  const seen = new Set<string>();
  let current = comment;
  while (current.reply_to !== null) {
    if (seen.has(current.id)) return undefined;
    seen.add(current.id);
    const parent = byId.get(current.reply_to);
    if (parent === undefined) return undefined;
    current = parent;
  }
  return current.id;
}
