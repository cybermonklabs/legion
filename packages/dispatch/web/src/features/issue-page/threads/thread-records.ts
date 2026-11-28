/**
 * R8's records (dispatch://INFRA-5704/spec#b-r-record): one thread of a route, made into the
 * records Threads lists, opens and counts. `threadRecords` is the one function that holds the rule
 * of a reply with a quote (decision 469), so INFRA-5706's margin reads this same function once it
 * draws such a record beside its post (R16).
 *
 * A thread comes as a route answers it: a row of a page of `comments`, `messages` or `asks` with
 * `order=activity` (INFRA-5703), or the read of one thread by an id that R12's lookups send. The
 * function makes no request and reads no clock.
 */
import type {
  AskThreadRow,
  CommentRead,
  CommentThreadRow,
  MessageRead,
  MessageThreadRow,
} from "@legion/contracts";

import type {
  Actor,
  AskRead,
  AskWithTrees,
  CommentWithTrees,
  MessageWithTrees,
} from "../../../api/types";
import { compareTimestamps } from "../../../lib/timestamps";

/** One thread as a route answers it. A row of a page carries the thread's `last_activity_at`,
 *  and the read of one thread by its id carries none. */
export type ThreadSource =
  | AskRead
  | AskThreadRow
  | CommentRead
  | CommentThreadRow
  | MessageRead
  | MessageThreadRow;

/** One write of a record (R7): an entry, or the answer or the resolution an ask holds. */
export interface RecordWrite {
  /** The id an event names for this write: the comment's or the message's own id, and the ask's
   *  id for the ask, its answer and its resolution. */
  readonly id: string;
  readonly at: string;
  readonly author: Actor;
}

interface RecordFields {
  /** The record's id, which its address `/threads/<id>` names (R12): the ask's id, the id of a
   *  comment thread's first comment or of a message thread's root, or the id of the reply with a
   *  quote that begins the record. */
  readonly id: string;
  /** The id of the thread on the server that the record is part of: the ask's id, else the id of
   *  the thread's first comment or root message. Its `me/replies` row and its read mark go by
   *  this id (R7, R9), so the record a reply with a quote begins shares its post's. */
  readonly thread: string;
  /** The newest time of the record's own writes, a deleted entry's included, and for a record
   *  that is the whole thread the thread's `last_activity_at` where the route gave one (R7). */
  readonly lastActivity: string;
  /** Whether the record is on text: its root holds an anchor, or a reply with a quote begins it
   *  (R8). */
  readonly onText: boolean;
  /** The words a record on text quotes above its root: its anchor's quote, or the quote of the
   *  reply that begins it. */
  readonly quote: string | undefined;
  /** Every write of the record, oldest first. */
  readonly writes: readonly RecordWrite[];
  /** Each person the server keeps a read mark of the record's thread for, once, in the order of
   *  their first write there: the person who wrote a comment, a message or an ask in it, or who
   *  answered it, and never for a resolution alone (migrations `0095_user_threads` and
   *  `0096_user_threads_asks_trigger`). Every record of one thread holds the same
   *  list. */
  readonly threadWriters: readonly Actor[];
  /** The person an open ask waits on (`waiting_on_person`), on each record of that ask's thread,
   *  and undefined for every other thread (R7). */
  readonly waitsOn: string | undefined;
}

/** One record of R8: a whole thread, or the part of one that a reply with a quote begins. */
export type ThreadRecord =
  | (RecordFields & {
      readonly kind: "ask";
      readonly root: AskWithTrees;
      /** The ask's comments, oldest first. */
      readonly replies: readonly CommentWithTrees[];
    })
  | (RecordFields & {
      readonly kind: "comment";
      readonly root: CommentWithTrees;
      /** Every comment after the root, oldest first. */
      readonly replies: readonly CommentWithTrees[];
    })
  | (RecordFields & {
      readonly kind: "message";
      readonly root: MessageWithTrees;
      /** Every message after the root, at any depth, oldest first. */
      readonly replies: readonly MessageWithTrees[];
    });

/** A comment or a message, as the rule below reads one. */
interface Post {
  readonly id: string;
  readonly author: Actor;
  readonly created_at: string;
  readonly reply_block: { readonly quote?: string } | null;
}

/** The posts of one record while the rule below gathers them. */
interface Part<P extends Post> {
  readonly id: string;
  readonly posts: P[];
  readonly onText: boolean;
  readonly quote: string | undefined;
}

/** The words a reply quotes of the block it answers, or undefined where it quotes none. */
export function quoteOf(post: {
  readonly reply_block: { readonly quote?: string } | null;
}): string | undefined {
  const quote = post.reply_block?.quote ?? "";
  return quote === "" ? undefined : quote;
}

/**
 * The parts of one thread by R8's rule (decision 469). A reply with a quote begins a part of its
 * own, unless the entry it answers is in a part on text already, which it then joins. Every other
 * reply joins the part of the entry it answers, so each entry whose answers lead to a reply with a
 * quote is in that reply's part, and no entry is in two. A reply is read after the entry it
 * answers, so the replies are read oldest first, and one that answers an entry this thread does
 * not hold joins the root's part.
 */
function partsOf<P extends Post>(
  root: Part<P>,
  replies: readonly P[],
  answered: (reply: P) => string
): Part<P>[] {
  const parts = [root];
  const partOf = new Map<string, Part<P>>([[root.id, root]]);
  const oldestFirst = [...replies].sort((one, two) =>
    compareTimestamps(one.created_at, two.created_at)
  );
  for (const reply of oldestFirst) {
    const parent = partOf.get(answered(reply)) ?? root;
    const quote = quoteOf(reply);
    if (quote !== undefined && !parent.onText) {
      const own: Part<P> = { id: reply.id, onText: true, posts: [reply], quote };
      parts.push(own);
      partOf.set(reply.id, own);
    } else {
      parent.posts.push(reply);
      partOf.set(reply.id, parent);
    }
  }
  return parts;
}

function postWrite(post: Post): RecordWrite {
  return { at: post.created_at, author: post.author, id: post.id };
}

/** The people among `authors`, each once, in their order. The server's triggers keep a read mark
 *  for a person alone, keyed by the login lowercased and trimmed (`user_threads_login`): a session
 *  that writes under a person's own token is still a session. */
function people(authors: readonly Actor[]): Actor[] {
  const seen = new Set<string>();
  return authors.filter((actor) => {
    const login = actor.id.trim().toLowerCase();
    if (actor.kind !== "user" || seen.has(login)) return false;
    seen.add(login);
    return true;
  });
}

/** What every record of one thread shares. */
interface ThreadFields {
  readonly thread: string;
  readonly lastActivity: string | undefined;
  readonly threadWriters: readonly Actor[];
  readonly waitsOn: string | undefined;
  /** Whether the thread gives one record alone, which then takes the thread's time. */
  readonly whole: boolean;
}

function fieldsOf<P extends Post>(
  part: Part<P>,
  writes: readonly RecordWrite[],
  shared: ThreadFields
): RecordFields {
  // Every record holds its root's write, so the newest of its writes is never asked of none.
  const newest = writes.reduce(
    (latest, write) => (compareTimestamps(write.at, latest) > 0 ? write.at : latest),
    writes[0]?.at ?? ""
  );
  return {
    id: part.id,
    lastActivity: shared.whole && shared.lastActivity !== undefined ? shared.lastActivity : newest,
    onText: part.onText,
    quote: part.quote,
    thread: shared.thread,
    threadWriters: shared.threadWriters,
    waitsOn: shared.waitsOn,
    writes,
  };
}

function commentRecord(
  part: Part<CommentWithTrees>,
  shared: ThreadFields
): ThreadRecord & { readonly kind: "comment" } {
  const [root, ...replies] = part.posts;
  if (root === undefined) throw new Error(`The record ${part.id} holds no comment.`);
  return {
    ...fieldsOf(part, part.posts.map(postWrite), shared),
    kind: "comment",
    replies,
    root,
  };
}

function askRecords(
  ask: AskWithTrees,
  comments: readonly CommentWithTrees[],
  lastActivity: string | undefined
): ThreadRecord[] {
  const answerer: Actor | undefined =
    ask.answer === null ? undefined : { id: ask.answer.user, kind: "user" };
  const head: Part<CommentWithTrees> = {
    id: ask.id,
    onText: ask.anchor !== null,
    posts: [],
    quote: ask.anchor?.quote,
  };
  // An ask's comment answers the ask, or the comment its `answers` names.
  const parts = partsOf(head, comments, (comment) => comment.answers ?? ask.id);
  const shared: ThreadFields = {
    lastActivity,
    thread: ask.id,
    threadWriters: people([
      ask.author,
      ...comments.map((comment) => comment.author),
      ...(answerer === undefined ? [] : [answerer]),
    ]),
    waitsOn: ask.state === "open" ? ask.waiting_on_person : undefined,
    whole: parts.length === 1,
  };
  const [, ...quoted] = parts;
  const writes: RecordWrite[] = [
    { at: ask.created_at, author: ask.author, id: ask.id },
    ...head.posts.map(postWrite),
    ...(answerer === undefined || ask.answer === null
      ? []
      : [{ at: ask.answer.at, author: answerer, id: ask.id }]),
    ...(ask.resolution === undefined
      ? []
      : [{ at: ask.resolution.at, author: ask.resolution.actor, id: ask.id }]),
  ].sort((one, two) => compareTimestamps(one.at, two.at));
  const record: ThreadRecord = {
    ...fieldsOf(head, writes, shared),
    kind: "ask",
    replies: head.posts,
    root: ask,
  };
  return [record, ...quoted.map((part) => commentRecord(part, shared))];
}

function commentRecords(
  head: CommentWithTrees,
  replies: readonly CommentWithTrees[],
  lastActivity: string | undefined
): ThreadRecord[] {
  const headQuote = quoteOf(head);
  const root: Part<CommentWithTrees> = {
    id: head.id,
    onText: head.anchor !== null || headQuote !== undefined,
    posts: [head],
    quote: head.anchor?.quote ?? headQuote,
  };
  // A reply stores its thread's head in `reply_to` and the comment it answers in `answers`.
  const parts = partsOf(root, replies, (reply) => reply.answers ?? head.id);
  const shared: ThreadFields = {
    lastActivity,
    thread: head.id,
    threadWriters: people([head, ...replies].map((comment) => comment.author)),
    waitsOn: undefined,
    whole: parts.length === 1,
  };
  return parts.map((part) => commentRecord(part, shared));
}

function messageRecords(
  first: MessageWithTrees,
  replies: readonly MessageWithTrees[],
  lastActivity: string | undefined
): ThreadRecord[] {
  const root: Part<MessageWithTrees> = {
    id: first.id,
    onText: false,
    posts: [first],
    quote: undefined,
  };
  const parts = partsOf(root, replies, (reply) => reply.in_reply_to ?? first.id);
  const shared: ThreadFields = {
    lastActivity,
    thread: first.id,
    threadWriters: people([first, ...replies].map((message) => message.author)),
    waitsOn: undefined,
    whole: parts.length === 1,
  };
  return parts.map((part) => {
    const [own, ...rest] = part.posts;
    if (own === undefined) throw new Error(`The record ${part.id} holds no message.`);
    return {
      ...fieldsOf(part, part.posts.map(postWrite), shared),
      kind: "message",
      replies: rest,
      root: own,
    };
  });
}

/**
 * The records of one thread (R8), the newest first and the larger id first on the same time, as
 * All draws them together at the thread's place (R3). An ask's record is the ask, its comments,
 * its answer and its resolution. A comment thread's is its first comment and each comment whose
 * `reply_to` names it. A message thread's is its root and each message under it, at any depth. A
 * comment or a message that answers an entry by a block with a quote begins a record of its own,
 * unless the entry it answers is in a record on text already, which it then joins (decision 469).
 * The entries that answer such a reply are in its record and left out of the post's, so each
 * record counts only its own writes.
 */
export function threadRecords(source: ThreadSource): readonly ThreadRecord[] {
  const records =
    "comment" in source
      ? commentRecords(
          source.comment,
          source.replies ?? [],
          "last_activity_at" in source ? source.last_activity_at : undefined
        )
      : "message" in source
        ? messageRecords(
            source.message,
            source.replies ?? [],
            "last_activity_at" in source ? source.last_activity_at : undefined
          )
        : "ask" in source
          ? askRecords(source.ask, source.replies ?? [], undefined)
          : askRecords(source, source.thread?.replies ?? [], source.last_activity_at);
  return records.sort(
    (one, two) =>
      compareTimestamps(two.lastActivity, one.lastActivity) || (two.id > one.id ? 1 : -1)
  );
}
