import { afterEach, expect, setDefaultTimeout, test } from "bun:test";
import { writeFileSync } from "node:fs";
import { join } from "node:path";
import type {
  Actor,
  CommentWithTrees,
  CreateCommentInput,
  CreateMessageInput,
  IssueAskRow,
  MessageRead,
  MessageWithTrees,
  ReplyBlockWithTrees,
} from "@legion/contracts";
import { HANG_GUARD_MS } from "@legion/test-fixtures/hang-guard";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import type { Server } from "bun";

// The rows wait on processes and local servers, which a loaded machine slows. Their bound is a
// hang guard, not a verdict, for every row of this file that states none of its own.
setDefaultTimeout(HANG_GUARD_MS);

const check = join(import.meta.dir, "dispatch-thread-pages.ts");
const servers: Server<undefined>[] = [];
const scratch = temporaryDirectories("dispatch-thread-pages-test");

afterEach(() => {
  for (const server of servers.splice(0)) server.stop(true);
});

function tokenFile(): string {
  const file = join(scratch(), "token");
  writeFileSync(file, "agent-token\n");
  return file;
}

async function run(...args: string[]): Promise<{ code: number; stderr: string; lines: unknown[] }> {
  const child = Bun.spawn([process.execPath, check, ...args], { stderr: "pipe", stdout: "pipe" });
  const [code, stderr, stdout] = await Promise.all([
    child.exited,
    new Response(child.stderr).text(),
    new Response(child.stdout).text(),
  ]);
  return {
    code,
    lines: stdout
      .split("\n")
      .filter((line) => line !== "")
      .map((line) => JSON.parse(line)),
    stderr,
  };
}

/** The issue the fixture is of: project ACME, whose daemon is legion-daemon:ACME. */
const ISSUE = "ACME-7";
const SCRATCH = "ACME-1";

const ada: Actor = { id: "ada", kind: "user" };
const bob: Actor = { id: "bob", kind: "user" };
const agent: Actor = { id: "s-alpha", kind: "session" };
const otherAgent: Actor = { id: "s-beta", kind: "session" };
const daemon: Actor = { id: "legion-daemon:ACME", kind: "session" };
const otherDaemon: Actor = { id: "legion-daemon:OTHER", kind: "session" };

function uuid(kind: "a" | "c" | "e", n: number): string {
  return `${kind}0000000-0000-4000-8000-${String(n).padStart(12, "0")}`;
}

function at(second: number, fraction = "000000"): string {
  return `2026-11-20T10:00:${String(second).padStart(2, "0")}.${fraction}Z`;
}

function comment(
  fields: Pick<CommentWithTrees, "author" | "created_at" | "id"> & Partial<CommentWithTrees>
): CommentWithTrees {
  return {
    anchor: null,
    answers: null,
    artifact_id: null,
    ask_id: null,
    body: `comment ${fields.id}`,
    deleted_at: null,
    deleted_by: null,
    deliveries: [],
    edited_at: null,
    issue_key: ISSUE,
    mentions: [],
    reply_block: null,
    reply_to: null,
    resolved: false,
    resolved_at: null,
    resolved_by: null,
    suggestion: null,
    turn: null,
    ...fields,
  };
}

function message(
  fields: Pick<MessageWithTrees, "author" | "created_at" | "id"> & Partial<MessageWithTrees>
): MessageWithTrees {
  return {
    body: `message ${fields.id}`,
    broadcast_id: null,
    deleted_at: null,
    deleted_by: null,
    deliveries: [],
    edited_at: null,
    in_reply_to: null,
    issue_key: ISSUE,
    reply_block: null,
    target: null,
    ...fields,
  };
}

function block(fields: Partial<ReplyBlockWithTrees>): ReplyBlockWithTrees {
  return { block_id: "b1", node: "comment/answered#b1", removed: false, text: "alpha", ...fields };
}

function ask(
  fields: Pick<IssueAskRow, "author" | "created_at" | "id"> & Partial<IssueAskRow>,
  replies: CommentWithTrees[] = []
): IssueAskRow {
  return {
    anchor: null,
    answer: null,
    artifact_id: null,
    asked_of: null,
    asked_of_at: null,
    asked_of_by: null,
    block_id: null,
    edited_at: null,
    issue_key: ISSUE,
    kind: "question",
    last_reply: null,
    multiple: false,
    options: [],
    question: `ask ${fields.id}`,
    state: "open",
    thread: {
      answers: [],
      claimant: null,
      edits: [],
      followers: [],
      replies,
      route: null,
      route_holder: null,
      route_status: null,
      route_status_reason: null,
    },
    urgency: "normal",
    ...fields,
  };
}

type PageName = "comments" | "messages-in" | "messages-none" | "asks";

interface Fixture {
  readonly comments: CommentWithTrees[];
  readonly messages: MessageRead[];
  readonly asks: IssueAskRow[];
  /** Each page's threads, newest last activity first, with that last activity, as the stand-in
   *  answers them: the truth the probe's reading of the lists is held to. */
  readonly pages: Record<PageName, [id: string, lastActivity: string][]>;
}

/** One issue holding each kind of thread R2, R4 and R5 tell apart, written so that the order of
 *  last activity differs from the order of creation. */
function fixture(): Fixture {
  const askReply = comment({
    ask_id: uuid("a", 1),
    author: bob,
    created_at: at(10),
    id: uuid("c", 10),
  });
  const askPartReply = comment({
    ask_id: uuid("a", 4),
    author: bob,
    created_at: at(5),
    id: uuid("c", 15),
  });
  // A reply a build before ask_id wrote: its ask's thread holds it, but it is no entry of the ask,
  // so ada's reply here counts for no ask in Mine's choice of login.
  const oldAskReply = comment({
    author: ada,
    created_at: at(40),
    id: uuid("c", 16),
    reply_to: uuid("c", 15),
  });
  const comments = [
    comment({ author: ada, created_at: at(1), id: uuid("c", 1) }),
    comment({ author: agent, created_at: at(2), id: uuid("c", 2) }),
    comment({
      anchor: {
        artifact_id: "f0000000-0000-4000-8000-000000000001",
        block_id: "b2",
        mark_id: "m1",
        orphaned: false,
        quote: "spec words",
        version: 1,
      },
      author: agent,
      created_at: at(3),
      id: uuid("c", 3),
    }),
    comment({ author: agent, created_at: at(4), id: uuid("c", 4) }),
    comment({
      author: otherAgent,
      created_at: at(5),
      id: uuid("c", 5),
      reply_block: block({ occurrence: 0, quote: "alpha" }),
      reply_to: uuid("c", 4),
    }),
    comment({ author: agent, created_at: at(6), id: uuid("c", 6) }),
    // A reply to a block of a post since deleted: its quote, if it stored one, is left out.
    comment({
      author: otherAgent,
      created_at: at(7),
      id: uuid("c", 7),
      reply_block: block({ deleted_at: at(8) }),
      reply_to: uuid("c", 6),
    }),
    comment({ author: agent, created_at: at(8), id: uuid("c", 8) }),
    comment({
      author: otherAgent,
      created_at: at(9),
      id: uuid("c", 9),
      reply_block: block({}),
      reply_to: uuid("c", 8),
    }),
    askReply,
    comment({ author: agent, created_at: at(11), id: uuid("c", 11) }),
    comment({ author: bob, created_at: at(12), id: uuid("c", 12), reply_to: uuid("c", 11) }),
    comment({ author: agent, created_at: at(13), id: uuid("c", 13), reply_to: uuid("c", 1) }),
    comment({ author: ada, created_at: at(13), id: uuid("c", 14) }),
    askPartReply,
    oldAskReply,
  ].sort((a, b) => a.created_at.localeCompare(b.created_at) || a.id.localeCompare(b.id));
  const thread = (root: MessageWithTrees, ...replies: MessageWithTrees[]): MessageRead => ({
    message: root,
    replies,
  });
  const messages = [
    thread(message({ author: daemon, created_at: at(1), id: uuid("e", 1) })),
    thread(
      message({ author: daemon, created_at: at(2), id: uuid("e", 2) }),
      message({ author: ada, created_at: at(3), id: uuid("e", 3), in_reply_to: uuid("e", 2) })
    ),
    thread(message({ author: otherDaemon, created_at: at(4), id: uuid("e", 4) })),
    thread(
      message({ author: agent, created_at: at(5), id: uuid("e", 5), target: "role:reviewer" }),
      message({
        author: otherAgent,
        created_at: at(6),
        id: uuid("e", 6),
        in_reply_to: uuid("e", 5),
      })
    ),
    thread(
      message({ author: agent, created_at: at(7), id: uuid("e", 7), target: "session:s-beta" }),
      message({
        author: otherAgent,
        created_at: at(8),
        id: uuid("e", 8),
        in_reply_to: uuid("e", 7),
      }),
      message({ author: bob, created_at: at(20), id: uuid("e", 9), in_reply_to: uuid("e", 8) })
    ),
    thread(
      message({ author: ada, created_at: at(9), id: uuid("e", 10), target: "session:s-alpha" })
    ),
    thread(message({ author: agent, created_at: at(10), id: uuid("e", 11) })),
    thread(
      message({ author: agent, created_at: at(11), id: uuid("e", 12), target: "role:reviewer" }),
      message({
        author: otherAgent,
        created_at: at(12),
        deleted_at: at(14),
        id: uuid("e", 13),
        in_reply_to: uuid("e", 12),
      })
    ),
  ];
  const asks = [
    ask({ author: agent, created_at: at(1), id: uuid("a", 1) }, [askReply]),
    ask({
      answer: { at: "2026-11-20T10:00:30.123456Z", selected: [], text: "yes", user: "ada" },
      author: agent,
      created_at: at(2),
      id: uuid("a", 2),
      state: "answered",
    }),
    ask({
      author: agent,
      created_at: at(3),
      id: uuid("a", 3),
      // A resolution's time is stored with more digits than a microsecond: it is cut, not rounded.
      resolution: {
        actor: bob,
        at: "2026-11-20T10:00:25.9999999Z",
        kind: "declined",
        reason: "no",
      },
      state: "resolved",
    }),
    ask({ author: ada, created_at: at(4), id: uuid("a", 4) }, [askPartReply, oldAskReply]),
    ask({ author: agent, created_at: at(10), id: uuid("a", 5) }),
  ];
  return {
    asks,
    comments,
    messages,
    pages: {
      asks: [
        [uuid("a", 2), "2026-11-20T10:00:30.123456Z"],
        [uuid("a", 3), "2026-11-20T10:00:25.999999Z"],
        [uuid("a", 5), at(10)],
        [uuid("a", 1), at(10)],
        [uuid("a", 4), at(5)],
      ],
      comments: [
        [uuid("c", 14), at(13)],
        [uuid("c", 1), at(13)],
        [uuid("c", 11), at(12)],
        [uuid("c", 6), at(7)],
        [uuid("c", 4), at(5)],
        [uuid("c", 3), at(3)],
      ],
      "messages-in": [
        [uuid("e", 7), at(20)],
        [uuid("e", 11), at(10)],
        [uuid("e", 10), at(9)],
        [uuid("e", 4), at(4)],
      ],
      "messages-none": [
        [uuid("e", 12), at(12)],
        [uuid("e", 5), at(6)],
      ],
    },
  };
}

/** The stand-in pages hold this many threads, so a walk follows `before`. */
const PAGE_SIZE = 2;

interface StandIn {
  readonly url: string;
  /** Every request it took, and its Dispatch-Trees header. */
  readonly requests: { path: string; trees: string | null }[];
}

interface StandInOptions {
  readonly issues: Record<string, Fixture>;
  /** Answers the list to order=activity, as a build before the pages does. */
  readonly bareList?: boolean;
  /** The first read of a comment list leaves out this comment, as a list read before it landed. */
  readonly firstCommentListLacks?: string;
  /** Each page holds a member beside threads, before and remaining. */
  readonly extraMember?: boolean;
  /** The comment page's rows carry another body than the list's. */
  readonly rowDiffers?: boolean;
  /** The Agents page of this issue answers 250 ms late from its `from`-th read on. */
  readonly slowAgents?: { issue: string; from: number };
  /** A reply's read-back holds no quote, as a build that drops the fields answers. */
  readonly quoteDropped?: boolean;
  /** The comment page read with `before` at this thread counts one thread more in `remaining`
   *  than follows it. */
  readonly overcountAfter?: string;
}

function occurrences(text: string, word: string): number {
  let count = 0;
  for (let index = text.indexOf(word); index !== -1; index = text.indexOf(word, index + 1)) {
    count += 1;
  }
  return count;
}

/** The number a line of the probe holds at `member`, where it holds one. */
function numberAt(line: unknown, member: string): number | undefined {
  if (typeof line !== "object" || line === null) return undefined;
  const value: unknown = Reflect.get(line, member);
  return typeof value === "number" ? value : undefined;
}

/** A Dispatch that answers an issue's lists from its fixture and its pages from the fixture's
 *  truth, and keeps the comments and messages written to it. */
function standIn(options: StandInOptions): StandIn {
  const requests: StandIn["requests"] = [];
  const written = { comments: [] as CommentWithTrees[], messages: [] as MessageWithTrees[] };
  let commentListReads = 0;
  let agentsReads = 0;
  const refusal = (status: number, code: string) =>
    Response.json({ code, error: code.toLowerCase() }, { status });
  const page = (rows: [string, string, object][], before: string | null, overcount = 0) => {
    const start = before === null ? 0 : rows.findIndex(([id]) => id === before.split(",")[1]) + 1;
    const threads = rows.slice(start, start + PAGE_SIZE);
    const remaining = rows.length - start - threads.length;
    const last = threads.at(-1);
    return {
      before: remaining > 0 && last !== undefined ? `${last[1]},${last[0]}` : null,
      remaining: remaining + overcount,
      threads: threads.map(([, lastActivity, row]) => ({ ...row, last_activity_at: lastActivity })),
      ...(options.extraMember === true ? { total: rows.length } : {}),
    };
  };
  const readBack = (reply: { reply_block: ReplyBlockWithTrees | null }) =>
    options.quoteDropped === true && reply.reply_block !== null
      ? {
          ...reply,
          reply_block: {
            block_id: reply.reply_block.block_id,
            node: reply.reply_block.node,
            removed: reply.reply_block.removed,
            text: reply.reply_block.text,
          },
        }
      : reply;
  const server = Bun.serve({
    async fetch(request) {
      const url = new URL(request.url);
      requests.push({
        path: url.pathname + url.search,
        trees: request.headers.get("dispatch-trees"),
      });
      if (request.headers.get("authorization") !== "Bearer agent-token") {
        return refusal(401, "NO_IDENTITY");
      }
      const [, api, version, owner, key = "", route, id] = url.pathname.split("/");
      if (api !== "api" || version !== "v1") return refusal(404, "NOT_FOUND");
      if (owner === "comments" && request.method === "GET") {
        const reply = written.comments.find((each) => each.id === key);
        return reply === undefined
          ? refusal(404, "NOT_FOUND")
          : Response.json({ comment: readBack(reply), replies: [] });
      }
      if (owner !== "issues") return refusal(404, "NOT_FOUND");
      if (request.method === "POST" && route === "comments") {
        // The probe under test writes its body to the generated input type.
        const input = (await request.json()) as CreateCommentInput;
        const parent = written.comments.find((each) => each.id === input.reply_to);
        if (typeof input.quote === "string" && parent !== undefined) {
          if (occurrences(parent.body, input.quote) <= (input.occurrence ?? 0)) {
            return refusal(404, "TARGET_NOT_FOUND");
          }
        }
        const made = comment({
          author: input.actor ?? agent,
          body: input.body ?? "",
          created_at: new Date().toISOString(),
          id: crypto.randomUUID(),
          issue_key: key,
          reply_block:
            parent === undefined || typeof input.reply_to_block !== "string"
              ? null
              : block({
                  block_id: input.reply_to_block,
                  node: `comment/${parent.id}#${input.reply_to_block}`,
                  occurrence: input.occurrence ?? undefined,
                  quote: input.quote ?? undefined,
                  text: parent.body,
                }),
          reply_to: input.reply_to ?? null,
        });
        written.comments.push(made);
        return Response.json(made, { status: 201 });
      }
      if (request.method === "POST" && route === "messages") {
        // The probe under test writes its body to the generated input type.
        const input = (await request.json()) as CreateMessageInput;
        const parent = written.messages.find((each) => each.id === input.in_reply_to);
        if (typeof input.quote === "string" && parent !== undefined) {
          if (occurrences(parent.body, input.quote) <= (input.occurrence ?? 0)) {
            return refusal(404, "TARGET_NOT_FOUND");
          }
        }
        const made = message({
          author: input.actor ?? agent,
          body: input.body,
          created_at: new Date().toISOString(),
          id: crypto.randomUUID(),
          in_reply_to: input.in_reply_to ?? null,
          issue_key: key,
          reply_block:
            parent === undefined || typeof input.in_reply_to_block !== "string"
              ? null
              : block({
                  block_id: input.in_reply_to_block,
                  node: `message/${parent.id}#${input.in_reply_to_block}`,
                  occurrence: input.occurrence ?? undefined,
                  quote: input.quote ?? undefined,
                  text: parent.body,
                }),
        });
        written.messages.push(made);
        return Response.json(made, { status: 201 });
      }
      if (route === "messages" && id !== undefined) {
        const reply = written.messages.find((each) => each.id === id);
        return reply === undefined
          ? refusal(404, "NOT_FOUND")
          : Response.json({ message: readBack(reply), replies: [] });
      }
      const data = options.issues[decodeURIComponent(key)];
      if (data === undefined) return refusal(404, "NOT_FOUND");
      if (route === undefined) return Response.json({ key, title: "Threads" });
      if (route === "events") return Response.json({ events: [{ kind: "issue.created", seq: 1 }] });
      const paged = url.searchParams.get("order") === "activity" && options.bareList !== true;
      const before = url.searchParams.get("before");
      if (route === "comments") {
        if (!paged) {
          commentListReads += 1;
          const lacks = commentListReads === 1 ? options.firstCommentListLacks : undefined;
          return Response.json(data.comments.filter((each) => each.id !== lacks));
        }
        const rows = data.pages.comments.map(([thread, lastActivity]): [string, string, object] => {
          const root = data.comments.find((each) => each.id === thread);
          const replies = data.comments.filter(
            (each) => each.reply_to === thread && each.ask_id === null
          );
          const shown =
            options.rowDiffers === true && root !== undefined ? { ...root, body: "changed" } : root;
          return [thread, lastActivity, { comment: shown, replies }];
        });
        const overcount =
          options.overcountAfter !== undefined &&
          before?.endsWith(`,${options.overcountAfter}`) === true
            ? 1
            : 0;
        return Response.json(page(rows, before, overcount));
      }
      if (route === "messages") {
        if (!paged) return Response.json(data.messages);
        const person = url.searchParams.get("person");
        if (options.slowAgents?.issue === key && person === "none") {
          agentsReads += 1;
          // A real delay: the probe times its reads by its own clock, in a process of its own.
          if (agentsReads >= options.slowAgents.from) await Bun.sleep(250);
        }
        const truth = person === "none" ? data.pages["messages-none"] : data.pages["messages-in"];
        const rows = truth.map(([thread, lastActivity]): [string, string, object] => [
          thread,
          lastActivity,
          data.messages.find((each) => each.message.id === thread) ?? {},
        ]);
        return Response.json(page(rows, before));
      }
      if (route === "asks") {
        if (!paged) return Response.json(data.asks);
        const rows = data.pages.asks.map(([thread, lastActivity]): [string, string, object] => [
          thread,
          lastActivity,
          data.asks.find((each) => each.id === thread) ?? {},
        ]);
        return Response.json(page(rows, before));
      }
      return refusal(404, "NOT_FOUND");
    },
    hostname: "127.0.0.1",
    port: 0,
  });
  servers.push(server);
  return { requests, url: `http://127.0.0.1:${server.port}` };
}

const REQUIRED = [
  "bearer-file",
  "issue",
  "walk",
  "weigh",
  "time-comments",
  "time-messages",
  "time-asks",
] as const;

/** The probe's flags against `dispatch`: every issue the fixture's, but where `flags` names one. */
function flagsFor(
  dispatch: StandIn,
  flags: Partial<Record<(typeof REQUIRED)[number], string>> = {}
) {
  const chosen = {
    "bearer-file": tokenFile(),
    issue: SCRATCH,
    "time-asks": ISSUE,
    "time-comments": ISSUE,
    "time-messages": ISSUE,
    walk: ISSUE,
    weigh: ISSUE,
    ...flags,
  };
  return [
    "--url",
    dispatch.url,
    ...Object.entries(chosen).flatMap(([flag, value]) => [`--${flag}`, value]),
  ];
}

const comments = "comments?order=activity&person=in";
const messagesIn = "messages?order=activity&person=in";
const messagesNone = "messages?order=activity&person=none";
const asks = "asks?order=activity&thread=1";

test("the probe passes on a Dispatch whose pages hold the threads and rows its lists give", async () => {
  const dispatch = standIn({ issues: { [ISSUE]: fixture() } });
  const { code, lines } = await run(...flagsFor(dispatch));
  const timed = (view: string, page: string) => ({
    bound_ms: 200,
    issue: ISSUE,
    p90_ms: expect.any(Number),
    page,
    step: "time",
    view,
  });
  expect(lines).toEqual([
    // The comment thread whose only reply answers a block of a deleted post is on the page here,
    // and the probe accepts it there or not: the list shows no quote for it.
    { either_way: 1, issue: ISSUE, page: comments, pages: 3, step: "walk", threads: 6, tries: 1 },
    { issue: ISSUE, page: messagesIn, pages: 2, step: "walk", threads: 4, tries: 1 },
    { issue: ISSUE, page: messagesNone, pages: 1, step: "walk", threads: 2, tries: 1 },
    { issue: ISSUE, page: asks, pages: 3, step: "walk", threads: 5, tries: 1 },
    {
      bytes: expect.any(Number),
      issue: ISSUE,
      page: comments,
      step: "weigh",
      threads: 2,
      tries: 1,
    },
    {
      bytes: expect.any(Number),
      issue: ISSUE,
      page: messagesIn,
      step: "weigh",
      threads: 2,
      tries: 1,
    },
    { bytes: expect.any(Number), issue: ISSUE, page: asks, step: "weigh", threads: 2, tries: 1 },
    {
      all_first_pages_bytes: expect.any(Number),
      bound_bytes: 630_000,
      events_bytes: expect.any(Number),
      issue: ISSUE,
      issue_bytes: expect.any(Number),
      over_bound: false,
      step: "weigh",
    },
    timed("All", comments),
    timed("All", messagesIn),
    timed("All", asks),
    // ada and bob each wrote three comments of the list, and ada is the smaller login; ada wrote
    // two messages and bob one; bob holds three of the asks' entries and ada two.
    timed("Mine", `${comments}&author=ada`),
    timed("Mine", `${messagesIn}&author=ada`),
    timed("Mine", `${asks}&author=bob`),
    timed("Agents", messagesNone),
    {
      occurrence: 1,
      quote: "quokka",
      read: expect.stringMatching(/^GET \/api\/v1\/comments\/[0-9a-f-]{36}$/),
      step: "quote",
    },
    {
      occurrence: 1,
      quote: "quokka",
      read: expect.stringMatching(/^GET \/api\/v1\/issues\/ACME-1\/messages\/[0-9a-f-]{36}$/),
      step: "quote",
    },
    { ok: true },
  ]);
  expect(code).toBe(0);
  const read = (query: string, trees: string | null) => ({
    path: `/api/v1/issues/${ISSUE}/${query}`,
    trees,
  });
  const lists = (trees: string | null) =>
    ["comments", "messages", "asks?thread=1"].map((query) => read(query, trees));
  const after = (query: string, lastActivity: string, id: string) =>
    `${query}&before=${encodeURIComponent(`${lastActivity},${id}`)}`;
  const walked = [
    comments,
    after(comments, at(13), uuid("c", 1)),
    after(comments, at(7), uuid("c", 6)),
    messagesIn,
    after(messagesIn, at(10), uuid("e", 11)),
    messagesNone,
    asks,
    after(asks, "2026-11-20T10:00:25.999999Z", uuid("a", 3)),
    after(asks, at(10), uuid("a", 1)),
  ];
  const timedPages = [
    comments,
    messagesIn,
    asks,
    `${comments}&author=ada`,
    `${messagesIn}&author=ada`,
    `${asks}&author=bob`,
    messagesNone,
  ];
  const scratchPath = `/api/v1/issues/${SCRATCH}`;
  expect(dispatch.requests).toEqual([
    // The walk reads its lists and its 9 pages with no display trees.
    ...lists(null),
    ...walked.map((query) => read(query, null)),
    ...lists(null),
    // The weigh reads its lists, All's 3 first pages and the issue's other first-load reads with
    // them, as R7 weighs the pages.
    ...lists("1"),
    ...[comments, messagesIn, asks].map((query) => read(query, "1")),
    ...lists("1"),
    { path: `/api/v1/issues/${ISSUE}`, trees: "1" },
    read("events?order=desc&limit=200", "1"),
    // Mine's logins, from each route's list, then the 7 reads of R7, 35 each, with display trees.
    ...lists(null),
    ...timedPages.flatMap((query) => Array.from({ length: 35 }, () => read(query, "1"))),
    // The quote step's comment, reply, message and reply, then the two read-backs.
    { path: `${scratchPath}/comments`, trees: null },
    { path: `${scratchPath}/comments`, trees: null },
    { path: `${scratchPath}/messages`, trees: null },
    { path: `${scratchPath}/messages`, trees: null },
    { path: expect.stringMatching(/^\/api\/v1\/comments\/[0-9a-f-]{36}$/), trees: null },
    {
      path: expect.stringMatching(/^\/api\/v1\/issues\/ACME-1\/messages\/[0-9a-f-]{36}$/),
      trees: null,
    },
  ]);
});

test("a comment page that leaves out the thread the lists cannot read R4 for passes the walk", async () => {
  // The only reply of thread c6 answers a block of a post since deleted, so the list shows no
  // quote of it: the page may hold the thread, as the stored quote puts it on text, or not.
  const data = fixture();
  const pages = {
    ...data.pages,
    comments: data.pages.comments.filter(([id]) => id !== uuid("c", 6)),
  };
  const dispatch = standIn({ issues: { [ISSUE]: { ...data, pages } } });
  const { code, lines } = await run(...flagsFor(dispatch));
  expect(lines[0]).toEqual({
    either_way: 1,
    issue: ISSUE,
    page: comments,
    pages: 3,
    step: "walk",
    threads: 5,
    tries: 1,
  });
  expect(lines.at(-1)).toEqual({ ok: true });
  expect(code).toBe(0);
});

test("a page whose remaining is not the count of the threads after it fails the walk", async () => {
  const dispatch = standIn({ issues: { [ISSUE]: fixture() }, overcountAfter: uuid("c", 1) });
  const { code, lines } = await run(...flagsFor(dispatch));
  const position = encodeURIComponent(`${at(13)},${uuid("c", 1)}`);
  expect(lines).toEqual([
    {
      ok: false,
      read: `GET /api/v1/issues/${ISSUE}/${comments}&before=${position}`,
      step: "walk",
      why: expect.stringContaining("remaining 3, where the walk reads 2 threads after it"),
    },
  ]);
  expect(code).toBe(1);
});

test("a build that answers the bare list to order=activity fails the walk, naming that read", async () => {
  const dispatch = standIn({ bareList: true, issues: { [ISSUE]: fixture() } });
  const { code, lines } = await run(...flagsFor(dispatch));
  expect(lines.at(-1)).toEqual({
    ok: false,
    read: `GET /api/v1/issues/${ISSUE}/${comments}`,
    step: "walk",
    why: expect.stringContaining("answered the bare list"),
  });
  expect(code).toBe(1);
});

test("a page that leaves out a thread the lists select fails the walk", async () => {
  const data = fixture();
  const left = uuid("c", 11);
  const pages = { ...data.pages, comments: data.pages.comments.filter(([id]) => id !== left) };
  const dispatch = standIn({ issues: { [ISSUE]: { ...data, pages } } });
  const { code, lines } = await run(...flagsFor(dispatch));
  const position = encodeURIComponent(`${at(13)},${uuid("c", 1)}`);
  expect(lines).toEqual([
    {
      ok: false,
      read: `GET /api/v1/issues/${ISSUE}/${comments}&before=${position}`,
      step: "walk",
      why: expect.stringContaining(`the walk leaves out ${left}`),
    },
  ]);
  expect(code).toBe(1);
});

test("lists that change between the reads of one try are read again, and agree in the next", async () => {
  // The first comment list misses the newest reply of the oldest thread, which moves that thread.
  const dispatch = standIn({
    firstCommentListLacks: uuid("c", 13),
    issues: { [ISSUE]: fixture() },
  });
  const { code, lines } = await run(...flagsFor(dispatch));
  expect(lines[0]).toEqual({
    either_way: 1,
    issue: ISSUE,
    page: comments,
    pages: 3,
    step: "walk",
    threads: 6,
    tries: 2,
  });
  expect(lines.at(-1)).toEqual({ ok: true });
  expect(code).toBe(0);
});

test("All's first pages over 630,000 bytes are printed as a measurement and fail nothing", async () => {
  const heavy = fixture();
  const comments = heavy.comments.map((each) =>
    each.id === uuid("c", 1) ? { ...each, body: "x".repeat(700_000) } : each
  );
  const dispatch = standIn({ issues: { [ISSUE]: fixture(), "ACME-9": { ...heavy, comments } } });
  const { code, lines } = await run(...flagsFor(dispatch, { weigh: "ACME-9" }));
  expect(lines).toContainEqual({
    all_first_pages_bytes: expect.any(Number),
    bound_bytes: 630_000,
    events_bytes: expect.any(Number),
    issue: "ACME-9",
    issue_bytes: expect.any(Number),
    over_bound: true,
    step: "weigh",
  });
  const total = lines.find((line) => numberAt(line, "all_first_pages_bytes") !== undefined);
  expect(numberAt(total, "all_first_pages_bytes")).toBeGreaterThan(630_000);
  expect(lines.at(-1)).toEqual({ ok: true });
  expect(code).toBe(0);
});

test("a page with a member beside threads, before and remaining, or a row the lists do not give, fails the weigh", async () => {
  const member = await run(
    ...flagsFor(standIn({ extraMember: true, issues: { [ISSUE]: fixture() } }))
  );
  expect(member.lines.at(-1)).toEqual({
    ok: false,
    read: `GET /api/v1/issues/${ISSUE}/${comments}`,
    step: "weigh",
    why: expect.stringContaining("the page holds the members before, remaining, threads, total"),
  });
  expect(member.code).toBe(1);
  const row = await run(...flagsFor(standIn({ issues: { [ISSUE]: fixture() }, rowDiffers: true })));
  expect(row.lines.at(-1)).toEqual({
    ok: false,
    read: `GET /api/v1/issues/${ISSUE}/${comments}`,
    step: "weigh",
    why: expect.stringContaining("differs from the row R2 builds from the lists at comment.body"),
  });
  expect(row.code).toBe(1);
});

test("a timed read slower than 200 ms at the 90th percentile fails the time step", async () => {
  // Of the Agents page's 35 reads, the first 5 warm up and the last 4 answer late: the 27th
  // fastest of the 30 timed, the nearest-rank 90th percentile, is a late one.
  const dispatch = standIn({
    issues: { [ISSUE]: fixture(), "ACME-8": fixture() },
    slowAgents: { from: 32, issue: "ACME-8" },
  });
  const { code, lines } = await run(...flagsFor(dispatch, { "time-messages": "ACME-8" }));
  expect(lines.at(-2)).toEqual({
    bound_ms: 200,
    issue: "ACME-8",
    p90_ms: expect.any(Number),
    page: messagesNone,
    step: "time",
    view: "Agents",
  });
  expect(numberAt(lines.at(-2), "p90_ms")).toBeGreaterThan(200);
  expect(lines.at(-1)).toEqual({
    ok: false,
    read: `GET /api/v1/issues/ACME-8/${messagesNone}`,
    step: "time",
    why: expect.stringContaining("above 200 ms"),
  });
  expect(code).toBe(1);
});

test("a reply whose read-back holds no quote fails the quote step", async () => {
  const dispatch = standIn({ issues: { [ISSUE]: fixture() }, quoteDropped: true });
  const { code, lines } = await run(...flagsFor(dispatch));
  expect(lines.at(-1)).toEqual({
    ok: false,
    read: expect.stringMatching(/^GET \/api\/v1\/comments\/[0-9a-f-]{36}$/),
    step: "quote",
    why: expect.stringContaining("holds no quote"),
  });
  expect(code).toBe(1);
});

test("a probe without a required flag is a usage error", async () => {
  const dispatch = standIn({ issues: { [ISSUE]: fixture() } });
  for (const flag of ["url", ...REQUIRED]) {
    const args = flagsFor(dispatch);
    const index = args.indexOf(`--${flag}`);
    const { code, stderr } = await run(...args.slice(0, index), ...args.slice(index + 2));
    expect({ code, flag }).toEqual({ code: 2, flag });
    expect(stderr).toContain(`--${flag} is required`);
  }
  expect(dispatch.requests).toEqual([]);
});
