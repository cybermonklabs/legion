/*
 * The three issues R21's row reads (dispatch://INFRA-3940 R21, check `c-scale-whole`, the plan's
 * g14): copies of the largest issues that the census of every INFRA issue found
 * (dispatch://INFRA-5574/artifact/load-measure-md, taken on 2026-11-23 with
 * `python3 $TMPDIR/arch/census.py census.jsonl 6`, GET only), each with its spec and its
 * requirements, in a project of their own, and a second person who never read them signed in
 * (`SCALE_READER`). Only the census's counts are copied: every name and every word is generated
 * here, and none of the text says an issue key, which the server would read as a reference.
 *
 * The census line each count comes from (the document's sections 1 and 1b):
 * - INFRA-663, row 1 of "Top 10, skeleton": 9,255 parts, 3 under "4 role docs", 9,252 comments,
 *   0 asks, 0 messages. Its copy holds the 9,252 comments and three role documents: the spec, the
 *   Verification document and the plan.
 * - INFRA-3149, row 1 of "Top 10 by parts": 69 documents of 45,738 blocks, the largest of 5,600,
 *   39 files, 113 comments, 5 asks, 0 messages. Its copy holds 69 documents of 45,738 blocks, the
 *   three role documents and one of 5,600 blocks among them, 39 files and 113 comments.
 * - INFRA-1098, the line under "Top 10, largest other document": "13 docs, 3,409 blocks, 100
 *   comments, 25 asks, 2 messages, 26 files". Its copy holds 13 documents of 3,409 blocks, the
 *   three role documents among them, 26 files, 100 comments, 25 asks and 2 messages.
 *
 * The census records no children, history events, threads, things that wait on the reader or
 * replies, so R21's floors are added where a count is below them (`FLOORS`):
 * - more than 20 documents: 22 small documents on INFRA-663's copy (25 in all) and 9 on
 *   INFRA-1098's (22 in all); INFRA-3149's 69 pass it as they are;
 * - more than 20 threads: 25 messages a session writes on each copy, each a thread of its own;
 * - more than 20 children: 25 children of each copy;
 * - more than 20 history events: each copy's comments' own `comment.created` events;
 * - more than five things that wait on the reader: six questions asked of the reader on each copy,
 *   INFRA-663's none and INFRA-3149's 5 raised to 6, and 6 of INFRA-1098's 25;
 * - a post with more than 50 replies: 60 replies on the oldest comment of each copy, which are
 *   among its comments.
 *
 * Which writes go by SQL, and why: the comments and their events, and nothing else. Every other
 * record is written through the API (`e2e/api.ts`), as a person or a session writes it, ten at a
 * time where they are many. The copies' 9,465 comments written through the API at about 0.29 s
 * each would take 45 minutes (`attention.e2e.ts` writes 205 in under 60 s), and the API's comment
 * path is the one other source of these rows (`api/comment_create.go`'s `queueIssueComment`). The
 * SQL writes them as the release before the block index does in
 * `docs/graph_previous_release_comment_test.go`'s `commentAsThePreviousRelease` and
 * `docs/block_index_fence_test.go`'s `recordIssueEvent`, in one transaction per issue: it takes its
 * transaction id first (`select pg_current_xact_id()`, the writer invariant `events/broker.go`'s
 * `lockEventOwner` keeps), inserts the comments, moves the issue's `last_seq` on by their count,
 * and inserts one `comment.created` event for each in `seq` order, with no gap from the old
 * `last_seq`, `notify` false and published at once, whose payload is `model.CommentEventPayload`'s
 * fields, `id` among them (`api/events.go` reads it). The comments name no node, as that release's
 * do; the server's block index gives them their nodes and their blocks from those events
 * (`docs/block_index_roll.go`), and the seed waits for it (`awaitBlockIndexDrained`). The block
 * index reads only the events after its own cursor (`readEventsAfterCursor`), which it writes on
 * its first tick whose horizon has proven an event, at that horizon, and which never moves back. So
 * the seed waits for that cursor before its first comment (`awaitBlockIndexCursor`), else the
 * cursor would start past the events, and each transaction moves `events_id_seq` past the cursor
 * before it inserts, as the release before lands its events after the index's cursor: a row's
 * reset (`resetDatabase`, `e2e/seed.ts`) empties the events and restarts their ids, but keeps the
 * cursor where the last row left it, so the ids a row after the first draws are behind it. After
 * each transaction the seed reads back through the API what it wrote, and it stops where a count, a
 * `seq` or an event's comment differs, rather than let the row read a copy it did not write.
 */

import type { Actor } from "../web/src/api/types";
import {
  createAsk,
  createIssue,
  createIssueArtifact,
  createMessage,
  createProject,
  getArtifactBlocks,
  getIssue,
  getIssueEvents,
  handAskOn,
  listComments,
  listIssueAsks,
  listIssueMessages,
  uploadIssueFile,
} from "./api";
import { authorLiteral } from "./inbox-threads";
import { asPlanner, pause, plannerActor } from "./issue-layout-seed-support";
import { planDocument, specDocument, verificationDocument } from "./issue-layout-seed-text";
import { ONE_PIXEL_PNG } from "./one-pixel-png";
import { sql } from "./psql";
import { sqlLiteral } from "./seed";

/** The project the three copies stand in, which holds nothing else. */
const PROJECT = "SCALE";

/** The person R21's row signs in as: one who never read the copies, and whom six questions on
 *  each wait on. */
const SCALE_READER = "bob";

/** The person who writes the copies, their documents and files, and their comments' roots; the
 *  replies are the planner session's (`plannerActor`). */
const WRITER: Actor = { id: "alice", kind: "user" };

/** R21's floors where the census counts less, or nothing. */
const FLOORS = {
  /** Children of each copy. */
  children: 25,
  /** Replies on one comment of each copy. */
  replies: 60,
  /** Messages a session writes on each copy, each a thread of its own. */
  threads: 25,
  /** Questions asked of the reader on each copy. */
  waiting: 6,
} as const;

/** The role documents every copy holds: the spec, the Verification document and the plan. */
const ROLE_DOCUMENTS = 3;

/** The requirements of each copy's spec, which its checks and its plan's tasks serve one each. */
const REQUIREMENTS = 8;

/** The blocks of each small document a floor adds: three paragraphs. */
const SMALL_DOCUMENT_BLOCKS = 3;

/** The bytes of every file a copy uploads, decoded once. */
const FIGURE_BYTES = new Uint8Array(Buffer.from(ONE_PIXEL_PNG, "base64"));

/** The most events one page of an issue's event log holds (`maxEventPage`, `api/events.go`). */
const EVENT_PAGE = 200;

/** How often the seed reads whether the block index has taken in the copies: the block index's own
 *  step while a batch is full (`blockIndexStep`, `docs/block_index.go`). */
const DRAIN_POLL_MS = 2_000;

/** How long the seed waits for it. The reconciliation's first tick can wait its whole interval,
 *  30 s (`BlockIndexReconcileInterval`), and each tick after takes 200 comments' blocks
 *  (`blockIndexItemBatch`) and 1,000 events (`blockIndexEventPage`, `docs/block_index_roll.go`)
 *  every 2 s while a batch is full: the copies' 9,465 comments are 48 ticks, 96 s, so about 126 s
 *  in all. The bound is more than twice that, for a loaded Job. */
const DRAIN_BOUND_MS = 300_000;

/** How long the seed waits for the block index's event cursor: the block index writes it on its
 *  first tick whose horizon has proven an event, and an idle server ticks every 30 s
 *  (`BlockIndexReconcileInterval`). The bound is two such ticks. */
const CURSOR_BOUND_MS = 60_000;

/** One copy, as the plan's table under "The sizes from the real system" writes it. */
interface CopyShape {
  /** The issue of INFRA whose size the copy takes. */
  readonly census: string;
  /** How the copy's text names the issue it copies: by its size, and never by its key. */
  readonly label: string;
  /** The census's documents beyond the three role documents, and, where the copy takes them, the
   *  blocks of all its documents together, the role documents' among them, and the blocks of its
   *  largest document. */
  readonly documents: {
    readonly beyondRoles: number;
    readonly blocks?: number;
    readonly largest?: number;
  };
  /** The small documents R21's floor of more than 20 documents adds. */
  readonly floorDocuments: number;
  readonly files: number;
  readonly comments: number;
  /** The questions, of which the first `FLOORS.waiting` are asked of the reader. */
  readonly asks: number;
  /** The messages the census counts, beside the floor's threads. */
  readonly messages: number;
}

const COPIES: readonly CopyShape[] = [
  {
    asks: FLOORS.waiting,
    census: "INFRA-663",
    comments: 9_252,
    documents: { beyondRoles: 0 },
    files: 0,
    floorDocuments: 22,
    label: "the issue with the most comments",
    messages: 0,
  },
  {
    asks: FLOORS.waiting,
    census: "INFRA-3149",
    comments: 113,
    documents: { beyondRoles: 66, blocks: 45_738, largest: 5_600 },
    files: 39,
    floorDocuments: 0,
    label: "the issue with the most parts",
    messages: 0,
  },
  {
    asks: 25,
    census: "INFRA-1098",
    comments: 100,
    documents: { beyondRoles: 10, blocks: 3_409 },
    files: 26,
    floorDocuments: 9,
    label: "the issue at the 99th percentile",
    messages: 2,
  },
];

/** The nodes of one of a copy's lists, by every id an address can name one of them by. */
export interface ScaleNodes {
  /** What the nodes are, as a failure names them: "documents and files". */
  readonly noun: string;
  readonly ids: ReadonlySet<string>;
}

/** One copy as R21's row reads it. */
export interface ScaleCopy {
  /** The issue of INFRA whose size the copy takes. */
  readonly census: string;
  readonly key: string;
  /** The documents and files the issue read lists, which Documents' list draws 20 at a time. */
  readonly listed: number;
  /** The things that wait on the reader, which Dashboard's list draws five at a time. */
  readonly waiting: number;
  /** How many things reference the issue (`referenced_by_count`), which Details' Referenced by
   *  lists 20 at a time. */
  readonly referencedBy: number;
  /** The copy's lists that grow with it: its documents and files but the spec, the Verification
   *  document and the plan, which the outline is built from (`treeDocumentIds`), its children,
   *  comments, questions and messages. */
  readonly nodes: readonly ScaleNodes[];
}

export interface ScaleSeed {
  readonly copies: readonly ScaleCopy[];
  /** The person the row signs in as, who never read a copy. */
  readonly reader: string;
}

/** One comment the SQL wrote, with the `seq` of its `comment.created` event. */
interface WrittenComment {
  readonly id: string;
  readonly seq: number;
}

/** What the seed wrote of one copy, as it checks it. */
interface WrittenCopy {
  readonly asks: readonly string[];
  /** The documents whose blocks the census counts: the role documents and the ones beyond them. */
  readonly census: readonly string[];
  readonly children: readonly string[];
  /** In `seq` order, the oldest first. */
  readonly comments: readonly WrittenComment[];
  readonly key: string;
  readonly messages: readonly string[];
  readonly shape: CopyShape;
}

/** `count` requests, ten at a time, each answered in the order of its index. */
async function tenAtATime<T>(count: number, send: (index: number) => Promise<T>): Promise<T[]> {
  const answered: T[] = [];
  for (let start = 0; start < count; start += 10) {
    answered.push(
      ...(await Promise.all(
        Array.from({ length: Math.min(10, count - start) }, (_value, offset) =>
          send(start + offset)
        )
      ))
    );
  }
  return answered;
}

/**
 * The blocks of each document the copy writes beyond its role documents: the census's first, its
 * largest at their head where the census names one, the rest sharing what is left of its blocks
 * once the role documents' own are counted, then the floor's small ones. A plain paragraph is one
 * block, which `expectCopyWritten` reads back through the census's own read.
 */
function documentSizes(shape: CopyShape, roleBlocks: number): number[] {
  const { beyondRoles, blocks, largest } = shape.documents;
  const sizes: number[] = [];
  if (blocks === undefined) {
    for (let index = 0; index < beyondRoles; index += 1) sizes.push(SMALL_DOCUMENT_BLOCKS);
  } else {
    if (largest !== undefined) sizes.push(largest);
    const shared = beyondRoles - sizes.length;
    const rest = blocks - roleBlocks - (largest ?? 0);
    if (rest < shared) {
      throw new Error(
        `the copy of ${shape.census} has ${rest} blocks left for ${shared} documents once its role documents' ${roleBlocks} are counted`
      );
    }
    for (let index = 0; index < shared; index += 1) {
      sizes.push(Math.floor(rest / shared) + (index < rest % shared ? 1 : 0));
    }
  }
  for (let index = 0; index < shape.floorDocuments; index += 1) sizes.push(SMALL_DOCUMENT_BLOCKS);
  return sizes;
}

/** A generated document of `blocks` paragraphs, each one block. */
function generatedDocument(number: number, blocks: number): string {
  const paragraphs = Array.from(
    { length: blocks },
    (_value, index) => `Paragraph ${index + 1} of record ${number} of the copy.`
  );
  return `${paragraphs.join("\n\n")}\n`;
}

/**
 * The copy's comments and their `comment.created` events, in one transaction (the file's head
 * says why by SQL): the roots by the writer, a millisecond apart, then the floor's 60 replies by
 * the planner session on the oldest of them, after every root, each naming it as the comment it
 * replies to and answers. The events take the issue's next `seq`s in the comments' order, and the
 * statement answers each comment with its event's `seq`, the oldest first. After its transaction
 * id and before its insert, the transaction moves `events_id_seq` past the block index's cursor,
 * so the events' ids are after it (the file's head says why).
 */
async function writeComments(key: string, shape: CopyShape): Promise<WrittenComment[]> {
  const count = shape.comments;
  const roots = count - FLOORS.replies;
  const issue = sqlLiteral(key);
  const at = (index: string) => `now() - (${count} - (${index})) * interval '1 millisecond'`;
  // `model.CommentEventPayload`'s fields of the comment `w` holds, as the release before the
  // block index marshals them (`commentAsThePreviousRelease`), and on a reply the thread's root,
  // as the comment route writes it (`thread_root_id`).
  const payload = `jsonb_build_object(
      'id', w.id::text, 'issue_key', ${issue}, 'artifact_id', null, 'author', w.author,
      'body', w.body, 'anchor', null, 'reply_to', w.reply_to::text, 'answers', w.answers::text,
      'ask_id', null, 'turn', null, 'resolved', false, 'resolved_by', null, 'resolved_at', null,
      'edited_at', null, 'deleted_at', null, 'deleted_by', null, 'suggestion', null,
      'created_at', to_char(w.created_at at time zone 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
      'mentions', '[]'::jsonb, 'deliveries', '[]'::jsonb, 'reply_block', null,
      'artifact_name', '', 'suppress_route', false
    ) || case when w.reply_to is null then '{}'::jsonb
              else jsonb_build_object('thread_root_id', w.reply_to::text) end`;
  const out = await sql(
    "begin",
    "select pg_current_xact_id()",
    `select setval('events_id_seq', greatest(nextval('events_id_seq'),
       (select coalesce(max(event_id), 0) + 1 from block_index_event_cursors)))`,
    `with roots as (
       insert into comments (issue_key, author, body, created_at)
       select ${issue}, ${authorLiteral(WRITER)},
              ${sqlLiteral("Comment ")} || g || ${sqlLiteral(` on the copy of ${shape.label}.`)},
              ${at("g")}
       from generate_series(1, ${roots}) g
       returning id, author, body, reply_to, answers, created_at
     ),
     head as (select id from roots order by created_at limit 1),
     replies as (
       insert into comments (issue_key, author, body, reply_to, answers, created_at)
       select ${issue}, ${authorLiteral(plannerActor)},
              ${sqlLiteral("Reply ")} || g || ${sqlLiteral(` to the first comment on the copy of ${shape.label}.`)},
              head.id, head.id, ${at(`${roots} + g`)}
       from head cross join generate_series(1, ${FLOORS.replies}) g
       returning id, author, body, reply_to, answers, created_at
     ),
     written as (select * from roots union all select * from replies),
     bumped as (
       update issues set last_seq = last_seq + ${count} where key = ${issue}
       returning last_seq - ${count} as before
     ),
     logged as (
       insert into events (issue_key, seq, type, actor, payload, notify, published_at)
       select ${issue}, ordered.seq, 'comment.created', ordered.author, ordered.payload, false, now()
       from (
         select bumped.before + row_number() over (order by w.created_at, w.id) as seq,
                w.author, ${payload} as payload
         from written w cross join bumped
         order by 1
       ) ordered
       returning seq, payload->>'id' as id
     )
     select seq || '|' || id from logged order by seq`,
    "commit"
  );
  return out.split("\n").flatMap((line) => {
    const written = /^(\d+)\|([0-9a-f-]{36})$/u.exec(line.trim());
    return written === null ? [] : [{ id: written[2] ?? "", seq: Number(written[1]) }];
  });
}

/** Fails, naming what differs, unless the API reads back each comment and event the SQL wrote on
 *  the copy: as many as it was asked for, each event at the `seq` after the one before it and
 *  naming its comment, the comments read listing every one of them, 60 replies on the first. */
async function expectCommentsWritten(copy: WrittenCopy): Promise<void> {
  const { comments, key, shape } = copy;
  const where = `the copy of ${shape.census} (${key})`;
  const before = (comments[0]?.seq ?? 1) - 1;
  const outOfOrder = comments.filter((comment, index) => comment.seq !== before + index + 1);
  if (comments.length !== shape.comments || outOfOrder.length > 0) {
    throw new Error(
      `${where}: the SQL wrote ${comments.length} comment.created events where ${shape.comments} were asked for, ${outOfOrder.length} of them out of seq order after seq ${before}`
    );
  }
  // The check above proved the events' seqs run on from `before` with no gap, and a seq is the
  // issue's own, so each page's `after` is known before any page answers.
  const pages = await tenAtATime(Math.ceil(comments.length / EVENT_PAGE), (page) =>
    getIssueEvents(key, { after: before + page * EVENT_PAGE, limit: EVENT_PAGE })
  );
  const events = pages.flat().map((event) => {
    const payload: unknown = event.payload;
    const id =
      typeof payload === "object" && payload !== null && "id" in payload ? payload.id : undefined;
    return { id, seq: event.seq, type: event.type };
  });
  const differ = comments.flatMap((comment, index) => {
    const event = events[index];
    return event?.seq === comment.seq && event.type === "comment.created" && event.id === comment.id
      ? []
      : [`seq ${comment.seq} wrote comment ${comment.id} and reads ${JSON.stringify(event)}`];
  });
  if (differ.length > 0) {
    throw new Error(
      `${where}: GET /api/v1/issues/${key}/events after seq ${before} differs from what the SQL wrote at ${differ.length} of ${comments.length} events: ${differ.slice(0, 3).join("; ")}`
    );
  }
  const listed = await listComments(key);
  const ids = new Set(listed.map((comment) => comment.id));
  const missing = comments.filter((comment) => !ids.has(comment.id)).length;
  const replies = listed.filter((comment) => comment.reply_to === comments[0]?.id).length;
  if (listed.length !== shape.comments || missing > 0 || replies !== FLOORS.replies) {
    throw new Error(
      `${where}: GET /api/v1/issues/${key}/comments lists ${listed.length} comments, ${missing} of the ${shape.comments} the SQL wrote missing, and ${replies} replies on the first, where the SQL wrote ${FLOORS.replies}`
    );
  }
  const issue = await getIssue(key);
  if (issue.last_seq < before + shape.comments) {
    throw new Error(
      `${where}: the issue read's last_seq is ${issue.last_seq}, behind the seq ${before + shape.comments} the SQL's last event took`
    );
  }
}

/** Writes one copy: the issue and its spec, then through the API its children, its Verification
 *  document and its plan, its other documents, its files, its questions, six of them asked of the
 *  reader, and its messages, then its comments by SQL (`writeComments`). */
async function writeCopy(shape: CopyShape): Promise<WrittenCopy> {
  const copyOf = `the copy of ${shape.label}`;
  const issue = await createIssue({
    force: true,
    project: PROJECT,
    spec: specDocument({ openQuestion: false, requirements: REQUIREMENTS, summary: true }),
    title: `A copy of the size of ${shape.label}`,
  });
  const key = issue.key;
  const children = await tenAtATime(FLOORS.children, (index) =>
    createIssue({
      force: true,
      parent: key,
      project: PROJECT,
      title: `Child ${index + 1} of ${copyOf}`,
    })
  );
  const verification = await createIssueArtifact(key, {
    content: verificationDocument({
      checks: REQUIREMENTS,
      issueKey: key,
      requirements: REQUIREMENTS,
    }),
    name: "verification.md",
  });
  const plan = await createIssueArtifact(key, {
    content: planDocument({ issueKey: key, planTasks: REQUIREMENTS, requirements: REQUIREMENTS }),
    name: "plan.md",
  });
  const roles = [issue.primary_artifact_id ?? "", verification.artifact.id, plan.artifact.id];
  // Each role document is far under the 2,000 blocks a write indexes in its own request
  // (`deferBlockIndexBlocks`, `docs/reference_excerpts.go`), so its blocks are read here at once.
  const roleBlocks = (await Promise.all(roles.map((id) => getArtifactBlocks(id)))).reduce(
    (sum, blocks) => sum + blocks.length,
    0
  );
  const sizes = documentSizes(shape, roleBlocks);
  // INFRA-3149's copy's largest, of 5,600 blocks, is above that bound, so its blocks reach the
  // index through the block index's reconciliation, which `awaitBlockIndexDrained` waits for.
  const documents = await tenAtATime(sizes.length, (index) =>
    createIssueArtifact(key, {
      content: generatedDocument(index + 1, sizes[index] ?? SMALL_DOCUMENT_BLOCKS),
      name: `record-${String(index + 1).padStart(2, "0")}.md`,
    })
  );
  await tenAtATime(shape.files, (index) =>
    uploadIssueFile(key, {
      bytes: FIGURE_BYTES,
      contentType: "image/png",
      name: `figure-${String(index + 1).padStart(2, "0")}.png`,
    })
  );
  const asks = await tenAtATime(shape.asks, async (index) => {
    const ask = await createAsk(
      key,
      { question: `Which way does part ${index + 1} of ${copyOf} go?` },
      asPlanner
    );
    if (index < FLOORS.waiting) await handAskOn(ask.id, SCALE_READER);
    return ask.id;
  });
  const messages = await tenAtATime(shape.messages + FLOORS.threads, (index) =>
    createMessage(key, { body: `Thread ${index + 1} a session opens on ${copyOf}.` }, asPlanner)
  );
  const comments = await writeComments(key, shape);
  return {
    asks,
    census: [
      ...roles,
      ...documents.slice(0, shape.documents.beyondRoles).map(({ artifact }) => artifact.id),
    ],
    children: children.map((child) => child.key),
    comments,
    key,
    messages: messages.map((message) => message.id),
    shape,
  };
}

/** Waits, reading every `DRAIN_POLL_MS` within `CURSOR_BOUND_MS`, until the server's block index
 *  has written its event cursor (`block_index_event_cursors`), and answers how long it took: the
 *  events a copy's comments write before it would start behind it (the file's head says why). */
async function awaitBlockIndexCursor(): Promise<number> {
  const started = Date.now();
  for (;;) {
    const read = (await sql("select count(*) from block_index_event_cursors")).trim();
    if (!/^\d+$/u.test(read)) {
      throw new Error(`the block index's cursors read ${JSON.stringify(read)}, not a count`);
    }
    if (Number(read) > 0) return Date.now() - started;
    if (Date.now() - started > CURSOR_BOUND_MS) {
      throw new Error(
        `the block index wrote no event cursor ${CURSOR_BOUND_MS / 1000} s after the seed began, so the copies' comments would start behind it`
      );
    }
    await pause(DRAIN_POLL_MS);
  }
}

/** Waits, reading every `DRAIN_POLL_MS` within `DRAIN_BOUND_MS`, until the server's block index
 *  has taken in what the copies wrote: no document version owes its blocks, no comment of the
 *  copies owes its own (`comment_block_state.owed_at`), and every one of them names its node.
 *  Answers how long it took. */
async function awaitBlockIndexDrained(keys: readonly string[]): Promise<number> {
  const issues = `array[${keys.map(sqlLiteral).join(", ")}]`;
  const started = Date.now();
  for (;;) {
    const read = await sql(`
      select (select count(*) from document_block_versions where version is null)
        || '|' || (select count(*) from comment_block_state s join comments c on c.id = s.comment_id
                   where c.issue_key = any(${issues}) and s.owed_at is not null)
        || '|' || (select count(*) from comments where issue_key = any(${issues}) and node_kind is null)
    `);
    const owed = read.split("|").map(Number);
    if (owed.length !== 3 || owed.some((count) => !Number.isInteger(count))) {
      throw new Error(`the block index's backlog read ${JSON.stringify(read)}, not three counts`);
    }
    if (owed.every((count) => count === 0)) return Date.now() - started;
    if (Date.now() - started > DRAIN_BOUND_MS) {
      throw new Error(
        `the block index has not taken in the copies ${DRAIN_BOUND_MS / 1000} s after they were written: ${owed[0]} document versions owe their blocks, ${owed[1]} comments owe theirs and ${owed[2]} name no node`
      );
    }
    await pause(DRAIN_POLL_MS);
  }
}

/** Fails, naming the count, unless the API reads the copy as the census and the floors size it:
 *  the census's blocks and its largest document, read as the census read them
 *  (`GET /api/v1/artifacts/{id}/blocks`), its documents, files, children, questions and the six of
 *  them that wait on the reader, and its messages. Answers the copy as the row reads it. */
async function expectCopyWritten(copy: WrittenCopy): Promise<ScaleCopy> {
  const { key, shape } = copy;
  const where = `the copy of ${shape.census} (${key})`;
  const blocks = (
    await tenAtATime(copy.census.length, (index) => getArtifactBlocks(copy.census[index] ?? ""))
  ).map((read) => read.length);
  const total = blocks.reduce((sum, count) => sum + count, 0);
  const largest = Math.max(...blocks);
  if (shape.documents.blocks !== undefined && total !== shape.documents.blocks) {
    throw new Error(
      `${where} holds ${total} blocks in the ${blocks.length} documents the census counts, where the census counts ${shape.documents.blocks}`
    );
  }
  if (shape.documents.largest !== undefined && largest !== shape.documents.largest) {
    throw new Error(
      `${where}'s largest document holds ${largest} blocks, where the census's holds ${shape.documents.largest}`
    );
  }
  const [issue, asks, messageList] = await Promise.all([
    getIssue(key),
    listIssueAsks(key),
    listIssueMessages(key),
  ]);
  const artifacts = issue.artifacts ?? [];
  const documents = artifacts.filter((artifact) => artifact.kind === "doc").length;
  const files = artifacts.filter((artifact) => artifact.kind === "image").length;
  const wanted = ROLE_DOCUMENTS + shape.documents.beyondRoles + shape.floorDocuments;
  const children = (issue.children ?? []).length;
  if (documents !== wanted || files !== shape.files || children !== FLOORS.children) {
    throw new Error(
      `${where}'s issue read lists ${documents} documents, ${files} files and ${children} children, where the seed wrote ${wanted}, ${shape.files} and ${FLOORS.children}`
    );
  }
  const waiting = asks.filter((ask) => ask.waiting_on_person === SCALE_READER).length;
  const messages = messageList.length;
  if (
    asks.length !== shape.asks ||
    waiting !== FLOORS.waiting ||
    messages !== shape.messages + FLOORS.threads
  ) {
    throw new Error(
      `${where} reads ${asks.length} questions, ${waiting} of them waiting on ${SCALE_READER}, and ${messages} messages, where the seed wrote ${shape.asks}, ${FLOORS.waiting} and ${shape.messages + FLOORS.threads}`
    );
  }
  const tree = new Set(
    [
      issue.primary_artifact_id,
      issue.verification_artifact_id,
      issue.plan_artifact_id,
      issue.decisions_artifact_id,
    ].filter((id): id is string => typeof id === "string")
  );
  console.log(
    `SCALE seed ${shape.census} as ${key}: ${documents} documents (${total} blocks in the census's ${blocks.length}, the largest ${largest}), ${files} files, ${shape.comments} comments (${FLOORS.replies} replies on one), ${asks.length} questions (${waiting} waiting on ${SCALE_READER}), ${messages} messages, ${children} children, ${issue.referenced_by_count} references to the issue`
  );
  return {
    census: shape.census,
    key,
    listed: artifacts.length,
    nodes: [
      {
        ids: new Set(
          artifacts
            .filter((artifact) => !tree.has(artifact.id))
            .flatMap((artifact) => [artifact.id, artifact.slug])
        ),
        noun: "documents and files",
      },
      { ids: new Set(copy.children), noun: "children" },
      { ids: new Set(copy.comments.map((comment) => comment.id)), noun: "comments" },
      { ids: new Set(copy.asks), noun: "questions" },
      { ids: new Set(copy.messages), noun: "messages" },
    ],
    referencedBy: issue.referenced_by_count,
    waiting,
  };
}

/**
 * Writes the copies of `shapes` into a project of their own, once the block index has written
 * its event cursor (`awaitBlockIndexCursor`), reads each copy's comments and events back after
 * its transaction (`expectCommentsWritten`), waits for the block index to take them all in
 * (`awaitBlockIndexDrained`; the drain's line names them as `drained`), and reads every count
 * back through the API (`expectCopyWritten`) before the row's first load. The reader signed in
 * as each copy's questions were asked of them (`handAskOn`), and has read none of the copies.
 */
async function seedCopies(shapes: readonly CopyShape[], drained: string): Promise<ScaleSeed> {
  await createProject({ key: PROJECT, name: "The largest issues" });
  const cursor = await awaitBlockIndexCursor();
  console.log(`SCALE cursor: the block index's event cursor stood after ${cursor / 1000} s`);
  const written: WrittenCopy[] = [];
  for (const shape of shapes) {
    const copy = await writeCopy(shape);
    await expectCommentsWritten(copy);
    written.push(copy);
  }
  const took = await awaitBlockIndexDrained(written.map((copy) => copy.key));
  console.log(`SCALE drain: the block index took in ${drained} in ${took / 1000} s`);
  const copies: ScaleCopy[] = [];
  for (const copy of written) copies.push(await expectCopyWritten(copy));
  return { copies, reader: SCALE_READER };
}

/** Writes the three copies (`seedCopies`). */
export function seedScale(): Promise<ScaleSeed> {
  return seedCopies(COPIES, "the copies");
}

/**
 * Writes the copy of INFRA-3149 alone, the largest of the three, as `seedScale` writes each copy
 * and checks it, for a row that reads one copy: the header's reading of its labels (the plan's
 * g24, check `c-header-labels`), of which the other two copies, minutes more of writing and of the
 * block index's drain, change nothing. The reader has read none of it.
 */
export async function seedLargestScaleCopy(): Promise<{
  readonly copy: ScaleCopy;
  readonly reader: string;
}> {
  const shape = COPIES.find((each) => each.census === "INFRA-3149");
  if (shape === undefined) throw new Error("the scale seed holds no copy of INFRA-3149");
  const {
    copies: [copy],
    reader,
  } = await seedCopies([shape], `the copy of ${shape.census}`);
  if (copy === undefined) throw new Error("the scale seed wrote no copy of INFRA-3149");
  return { copy, reader };
}
