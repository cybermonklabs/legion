// The production check of an issue's thread pages (scripts/e2e/README.md,
// "lib/dispatch-thread-pages.ts"): under order=activity, GET /api/v1/issues/{key}/comments,
// /messages and /asks answer the threads the issue's lists select, newest last activity first;
// All's first pages hold the lists' rows; the seven first pages of Threads answer within 200 ms at
// the 90th percentile; and a reply to a block keeps the words it quotes.
//
//   bun scripts/e2e/lib/dispatch-thread-pages.ts --url <dispatch> --bearer-file <file>
//     --issue <scratch KEY> --walk <KEY>... --weigh <KEY>... --time-comments <KEY>
//     --time-messages <KEY> --time-asks <KEY> [--session <id>]
//
// It names no issue of its own: every key comes from a flag, since this repository is public. It
// prints one JSON line per step, then {"ok":true} and exits 0; a step that fails prints
// {"ok":false,"step":"<step>","read":"<method and path>","why":"<why>"} and exits 1, and a usage
// error exits 2. --issue names a scratch issue: the check leaves a comment and a message there,
// each with a reply, written as the bearer's session (--session, thread-pages-check-<uuid> by
// default).
import { parseArgs } from "node:util";
import type {
  CommentWithTrees,
  CreateCommentInput,
  CreateMessageInput,
  IssueAskRow,
  MessageRead,
  MessageWithTrees,
} from "@legion/contracts";

const USAGE =
  "usage: bun scripts/e2e/lib/dispatch-thread-pages.ts --url <dispatch> --bearer-file <file> --issue <scratch KEY> --walk <KEY> [--walk <KEY>...] --weigh <KEY> [--weigh <KEY>...] --time-comments <KEY> --time-messages <KEY> --time-asks <KEY> [--session <id>]";

/** A busy issue gets writes while the probe reads it, so a read judged against its lists is made
 *  again until two reads of the lists around it agree, this many times at most. */
const TRIES = 3;
const WARM_UP_READS = 5;
const TIMED_READS = 30;
/** R7: each first page of Threads answers within this, at the 90th percentile. */
const BOUND_MS = 200;
/** R7: All's three first pages of one issue together; a measurement here, which fails nothing. */
const ALL_FIRST_PAGES_BYTES = 630_000;
/** R7 reads with the display trees the web client asks for. */
const TREES_HEADER = "Dispatch-Trees";
/** The only block of a body of one paragraph: an item's blocks are named b1, b2, … in order. */
const FIRST_BLOCK = "b1";
/** The word the quote step's block holds twice; no uuid and no time spells it. */
const QUOTED_WORD = "quokka";
const ISSUE_KEY = /^[A-Z][A-Z0-9]{1,9}-[0-9]+$/;
const SINGLE_FLAGS = [
  "bearer-file",
  "issue",
  "session",
  "time-asks",
  "time-comments",
  "time-messages",
  "url",
] as const;

type Step = "walk" | "weigh" | "time" | "quote";
type ListKind = "comments" | "messages" | "asks";

class UsageError extends Error {}

/** A failed step: the read it made, and why that read fails it. */
class StepFailed extends Error {
  constructor(
    readonly read: string,
    why: string
  ) {
    super(why);
  }
}

interface Options {
  readonly bearer: string;
  readonly issue: string;
  readonly session: string;
  readonly timeAsks: string;
  readonly timeComments: string;
  readonly timeMessages: string;
  readonly url: URL;
  readonly walk: readonly string[];
  readonly weigh: readonly string[];
}

/** The run: the step it is in and its last read, so a failure no step names still names both. */
interface Probe {
  readonly options: Options;
  step: Step;
  lastRead: string;
}

/** One answer read whole: the method and path it answered, its bytes and its JSON. */
interface Answer {
  readonly read: string;
  readonly bytes: number;
  readonly body: unknown;
}

/** An issue's lists, read with no `order`: the comment list, the message list and the ask list
 *  with `thread=1`. */
interface ListRows {
  comments: CommentWithTrees[];
  messages: MessageRead[];
  asks: IssueAskRow[];
}
type Lists = Partial<ListRows>;

const LIST_QUERY: Record<ListKind, string> = {
  asks: "asks?thread=1",
  comments: "comments",
  messages: "messages",
};

/** A thread as a page of R1 orders it. `either` marks a comment thread R4 cannot be read for from
 *  the list (commentOnPersonPage), which the page may hold or not. */
interface Selected {
  readonly at: string;
  readonly either: boolean;
  readonly id: string;
}

/** One page read of R1, and what R2, R4 and R5 make of the issue's list for it. */
interface PageRead {
  /** The page's path and query under the issue. */
  readonly page: string;
  readonly list: ListKind;
  /** The id of the thread a page row holds. */
  readonly idOf: (row: Record<string, unknown>) => unknown;
  /** Each thread's row as R2 builds it from the list, by the thread's id. */
  readonly rows: (lists: Lists) => Map<string, unknown>;
  /** The threads the page holds, newest last activity first, the larger id first at a tie. */
  readonly select: (lists: Lists, issue: string) => Selected[];
}

interface PageThread {
  readonly at: string;
  readonly id: string;
  readonly row: Record<string, unknown>;
}

interface Page {
  readonly before: string | null;
  readonly members: string[];
  readonly threads: PageThread[];
}

function write(line: object): void {
  process.stdout.write(`${JSON.stringify(line)}\n`);
}

function messageOf(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}

function isObject(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

/** A value as a failure quotes it, cut short. */
function excerpt(value: unknown): string {
  const text = JSON.stringify(value) ?? "nothing";
  return text.length > 160 ? `${text.slice(0, 160)}…` : text;
}

function issuePath(issue: string, rest: string): string {
  return `/api/v1/issues/${encodeURIComponent(issue)}/${rest}`;
}

function issueKey(value: string | undefined, flag: string): string {
  if (value === undefined || value === "") throw new UsageError(`--${flag} is required`);
  if (!ISSUE_KEY.test(value)) {
    throw new UsageError(`--${flag} names an issue key, <PROJECT>-<number>, not ${value}`);
  }
  return value;
}

function issueKeys(values: string[] | undefined, flag: string): string[] {
  if (values === undefined || values.length === 0) throw new UsageError(`--${flag} is required`);
  return values.map((value) => issueKey(value, flag));
}

function parsedArgs(argv: readonly string[]) {
  try {
    return parseArgs({
      args: [...argv],
      allowPositionals: false,
      options: {
        "bearer-file": { type: "string" },
        issue: { type: "string" },
        session: { type: "string" },
        "time-asks": { type: "string" },
        "time-comments": { type: "string" },
        "time-messages": { type: "string" },
        url: { type: "string" },
        walk: { multiple: true, type: "string" },
        weigh: { multiple: true, type: "string" },
      },
      strict: true,
      tokens: true,
    });
  } catch (error) {
    throw new UsageError(messageOf(error));
  }
}

async function optionsFrom(argv: readonly string[]): Promise<Options> {
  const { tokens, values } = parsedArgs(argv);
  for (const flag of SINGLE_FLAGS) {
    const given = tokens.filter((token) => token.kind === "option" && token.name === flag);
    if (given.length > 1) throw new UsageError(`--${flag} is given once`);
  }
  const rawURL = values.url;
  if (rawURL === undefined || rawURL === "") throw new UsageError("--url is required");
  const bearerFile = values["bearer-file"];
  if (bearerFile === undefined || bearerFile === "") {
    throw new UsageError("--bearer-file is required");
  }
  const issue = issueKey(values.issue, "issue");
  const walk = issueKeys(values.walk, "walk");
  const weigh = issueKeys(values.weigh, "weigh");
  const timeComments = issueKey(values["time-comments"], "time-comments");
  const timeMessages = issueKey(values["time-messages"], "time-messages");
  const timeAsks = issueKey(values["time-asks"], "time-asks");
  let url: URL;
  try {
    url = new URL(rawURL);
  } catch {
    throw new UsageError(`--url is not a URL: ${rawURL}`);
  }
  if (url.protocol !== "http:" && url.protocol !== "https:") {
    throw new UsageError(`--url is Dispatch's http or https address, not ${rawURL}`);
  }
  let bearer: string;
  try {
    bearer = (await Bun.file(bearerFile).text()).trim();
  } catch (error) {
    throw new UsageError(`cannot read --bearer-file ${bearerFile}: ${messageOf(error)}`);
  }
  if (bearer === "") throw new UsageError(`--bearer-file ${bearerFile} holds no token`);
  return {
    bearer,
    issue,
    session: values.session ?? `thread-pages-check-${crypto.randomUUID()}`,
    timeAsks,
    timeComments,
    timeMessages,
    url,
    walk,
    weigh,
  };
}

function refusalOf(body: unknown): string {
  if (!isObject(body)) return "";
  const code = typeof body.code === "string" ? ` ${body.code}` : "";
  const error = typeof body.error === "string" ? `: ${body.error}` : "";
  return `${code}${error}`;
}

/** Sends a GET, or a POST of `body`, and reads its whole answer, which must be a JSON 200 or 201. */
async function send(
  probe: Probe,
  path: string,
  init: { readonly body?: object; readonly trees?: boolean } = {}
): Promise<Answer> {
  const method = init.body === undefined ? "GET" : "POST";
  const read = `${method} ${path}`;
  probe.lastRead = read;
  let status: number;
  let bytes: Uint8Array;
  try {
    const response = await fetch(new URL(path, probe.options.url), {
      body: init.body === undefined ? undefined : JSON.stringify(init.body),
      headers: {
        Authorization: `Bearer ${probe.options.bearer}`,
        ...(init.body === undefined ? {} : { "Content-Type": "application/json" }),
        ...(init.trees === true ? { [TREES_HEADER]: "1" } : {}),
      },
      method,
    });
    status = response.status;
    bytes = new Uint8Array(await response.arrayBuffer());
  } catch (error) {
    throw new StepFailed(read, `the request failed: ${messageOf(error)}`);
  }
  let body: unknown;
  try {
    body = JSON.parse(new TextDecoder().decode(bytes));
  } catch {
    body = undefined;
  }
  const expected = method === "GET" ? 200 : 201;
  if (status !== expected) {
    throw new StepFailed(read, `answered ${status}${refusalOf(body)}, not ${expected}`);
  }
  if (body === undefined) throw new StepFailed(read, `answered ${status} with no JSON body`);
  return { body, bytes: bytes.byteLength, read };
}

async function readLists(
  probe: Probe,
  issue: string,
  kinds: readonly ListKind[],
  trees: boolean
): Promise<Lists> {
  const lists: Lists = {};
  for (const kind of kinds) {
    const answer = await send(probe, issuePath(issue, LIST_QUERY[kind]), { trees });
    const rows = answer.body;
    if (!Array.isArray(rows)) {
      throw new StepFailed(
        answer.read,
        `answered ${excerpt(rows)}, where a read with no order answers the list, a JSON array`
      );
    }
    if (kind === "comments") lists.comments = rows;
    else if (kind === "messages") lists.messages = rows;
    else {
      if (rows.some((row) => !isObject(row) || row.thread === undefined)) {
        throw new StepFailed(answer.read, "answered an ask with no thread under thread=1");
      }
      lists.asks = rows;
    }
  }
  return lists;
}

function listOf<Kind extends ListKind>(lists: Lists, kind: Kind): ListRows[Kind] {
  const rows = lists[kind];
  if (rows === undefined) throw new Error(`the ${kind} list was not read`);
  return rows;
}

/** A time of an answer as R2 sorts it: in UTC, cut (never rounded) to whole microseconds, and
 *  written as `last_activity_at` is, `2026-11-23T22:34:52.010780Z`. */
function microsecondsOf(time: string): string {
  const match = /^(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2})(?:\.(\d+))?(Z|[+-]\d{2}:\d{2})$/.exec(time);
  if (match === null) throw new Error(`${time} is no RFC 3339 time`);
  const [, whole = "", fraction = "", zone = "Z"] = match;
  let utc = whole;
  if (zone !== "Z") {
    const minutes = Number(zone.slice(1, 3)) * 60 + Number(zone.slice(4, 6));
    const offset = (zone.startsWith("-") ? -1 : 1) * minutes * 60_000;
    utc = new Date(Date.parse(`${whole}Z`) - offset).toISOString().slice(0, 19);
  }
  return `${utc}.${fraction.slice(0, 6).padEnd(6, "0")}Z`;
}

/** R2's last activity: the newest of the entries' times. */
function newest(times: readonly string[]): string {
  return times.map(microsecondsOf).reduce((latest, time) => (time > latest ? time : latest));
}

function byActivity(a: Selected, b: Selected): number {
  if (a.at !== b.at) return a.at < b.at ? 1 : -1;
  if (a.id !== b.id) return a.id < b.id ? 1 : -1;
  return 0;
}

interface CommentRow {
  readonly comment: CommentWithTrees;
  readonly replies: CommentWithTrees[];
}

/** R2's comment threads of the issue's comment list: each comment with no reply_to and no ask_id,
 *  with the comments whose reply_to names it, in the list's order, oldest first. A comment with an
 *  ask_id is its ask's entry. */
function commentThreads(comments: readonly CommentWithTrees[]): Map<string, CommentRow> {
  const threads = new Map<string, CommentRow>();
  for (const comment of comments) {
    if (comment.reply_to === null && comment.ask_id === null) {
      threads.set(comment.id, { comment, replies: [] });
    }
  }
  for (const comment of comments) {
    if (comment.reply_to !== null && comment.ask_id === null) {
      threads.get(comment.reply_to)?.replies.push(comment);
    }
  }
  return threads;
}

/** R4 read from the list: a thread with a person's entry, an anchored root, or a reply that shows
 *  its stored quote is on `person=in`. A reply whose answered post is deleted shows no quote, since
 *  every answer leaves it out then (R8), though the page still reads it from the store: such a
 *  thread, with nothing else that puts it on, is `either`. */
function commentOnPersonPage(row: CommentRow): "on" | "off" | "either" {
  const entries = [row.comment, ...row.replies];
  if (
    entries.some((entry) => entry.author.kind === "user") ||
    row.comment.anchor !== null ||
    entries.some((entry) => typeof entry.reply_block?.quote === "string")
  ) {
    return "on";
  }
  return entries.some((entry) => typeof entry.reply_block?.deleted_at === "string")
    ? "either"
    : "off";
}

function messageEntries(row: MessageRead): MessageWithTrees[] {
  return [row.message, ...(row.replies ?? [])];
}

/** R5's Legion daemon of the issue: `legion-daemon:` and the issue's project key. */
function daemonOf(issue: string): string {
  return `legion-daemon:${issue.slice(0, issue.lastIndexOf("-"))}`;
}

/** R5: a thread whose root the issue's daemon wrote is on neither page; any other thread is on
 *  `person=in` where a person wrote an entry or its root has no target, and on `person=none`
 *  otherwise. */
function messagePerson(row: MessageRead, issue: string): "in" | "none" | "neither" {
  const root = row.message;
  if (root.author.kind === "session" && root.author.id === daemonOf(issue)) return "neither";
  return root.target === null || messageEntries(row).some((entry) => entry.author.kind === "user")
    ? "in"
    : "none";
}

/** R2's last activity of an ask: the ask, each comment whose ask_id names it, and the answer and
 *  the resolution it holds now. */
function askActivity(row: IssueAskRow): string {
  const times = [row.created_at];
  for (const reply of row.thread?.replies ?? []) {
    if (reply.ask_id === row.id) times.push(reply.created_at);
  }
  const answered = row.answer?.at;
  if (answered !== undefined) times.push(answered);
  const resolved = row.resolution?.at;
  if (resolved !== undefined) times.push(resolved);
  return newest(times);
}

const COMMENTS_PERSON_IN: PageRead = {
  idOf: (row) => (isObject(row.comment) ? row.comment.id : undefined),
  list: "comments",
  page: "comments?order=activity&person=in",
  rows: (lists) => commentThreads(listOf(lists, "comments")),
  select: (lists) =>
    [...commentThreads(listOf(lists, "comments"))]
      .flatMap(([id, row]): Selected[] => {
        const on = commentOnPersonPage(row);
        if (on === "off") return [];
        const at = newest([row.comment, ...row.replies].map((entry) => entry.created_at));
        return [{ at, either: on === "either", id }];
      })
      .sort(byActivity),
};

function messagePage(person: "in" | "none"): PageRead {
  return {
    idOf: (row) => (isObject(row.message) ? row.message.id : undefined),
    list: "messages",
    page: `messages?order=activity&person=${person}`,
    rows: (lists) => new Map(listOf(lists, "messages").map((row) => [row.message.id, row])),
    select: (lists, issue) =>
      listOf(lists, "messages")
        .filter((row) => messagePerson(row, issue) === person)
        .map((row) => ({
          at: newest(messageEntries(row).map((entry) => entry.created_at)),
          either: false,
          id: row.message.id,
        }))
        .sort(byActivity),
  };
}

const MESSAGES_PERSON_IN = messagePage("in");
const MESSAGES_PERSON_NONE = messagePage("none");

const ASKS_THREADS: PageRead = {
  idOf: (row) => row.id,
  list: "asks",
  page: "asks?order=activity&thread=1",
  rows: (lists) => new Map(listOf(lists, "asks").map((row) => [row.id, row])),
  select: (lists) =>
    listOf(lists, "asks")
      .map((row) => ({ at: askActivity(row), either: false, id: row.id }))
      .sort(byActivity),
};

const WALKED_PAGES = [COMMENTS_PERSON_IN, MESSAGES_PERSON_IN, MESSAGES_PERSON_NONE, ASKS_THREADS];
/** All's three pages, the ones R7 weighs. */
const ALL_PAGES = [COMMENTS_PERSON_IN, MESSAGES_PERSON_IN, ASKS_THREADS];

/** An answer to a page read of R1: `{threads, before, remaining}`, each row naming its thread. */
function pageOf(answer: Answer, page: PageRead): Page {
  const { body, read } = answer;
  if (Array.isArray(body)) {
    throw new StepFailed(
      read,
      "answered the bare list, a JSON array, where order=activity answers the page {threads, before, remaining}: this build does not serve the thread pages"
    );
  }
  const before = isObject(body) && typeof body.before === "string" ? body.before : null;
  if (
    !isObject(body) ||
    !Array.isArray(body.threads) ||
    (body.before !== null && before === null) ||
    typeof body.remaining !== "number"
  ) {
    throw new StepFailed(
      read,
      `answered ${excerpt(body)}, which is no page {threads, before, remaining}`
    );
  }
  const threads: PageThread[] = [];
  for (const [index, row] of body.threads.entries()) {
    const id = isObject(row) ? page.idOf(row) : undefined;
    if (!isObject(row) || typeof id !== "string" || typeof row.last_activity_at !== "string") {
      throw new StepFailed(read, `row ${index + 1} names no thread id and last_activity_at`);
    }
    threads.push({ at: row.last_activity_at, id, row });
  }
  return { before, members: Object.keys(body).sort(), threads };
}

/** One read of an issue judged against the issue's list of its route. */
interface Bracket<Result, Line> {
  readonly page: PageRead;
  readonly read: () => Promise<Result>;
  /** What of the list the read is judged on: two reads of the list agree where this is equal. */
  readonly view: (lists: Lists, result: Result) => unknown;
  /** The step's line for the read; throws StepFailed where the read differs from the list. */
  readonly judge: (lists: Lists, result: Result) => Line;
}

/** Reads the lists the pending reads are judged on, then each read, then the lists again, up to
 *  TRIES times. A read is judged in the first try whose two reads of its list agree, so a write
 *  between them is never read as the page's fault; the step fails where no try agrees. */
async function bracketed<Result, Line>(
  probe: Probe,
  issue: string,
  trees: boolean,
  brackets: readonly Bracket<Result, Line>[]
): Promise<(Line & { tries: number })[]> {
  const lines: (Line & { tries: number })[] = [];
  let pending = brackets.map((_, index) => index);
  for (let tries = 1; tries <= TRIES && pending.length > 0; tries += 1) {
    const kinds = [...new Set(pending.map((index) => brackets[index].page.list))];
    const before = await readLists(probe, issue, kinds, trees);
    const results: Result[] = [];
    for (const index of pending) results.push(await brackets[index].read());
    const after = await readLists(probe, issue, kinds, trees);
    const next: number[] = [];
    for (const [position, index] of pending.entries()) {
      const bracket = brackets[index];
      const result = results[position];
      if (!Bun.deepEquals(bracket.view(before, result), bracket.view(after, result))) {
        next.push(index);
        continue;
      }
      lines[index] = { ...bracket.judge(after, result), tries };
    }
    pending = next;
  }
  const [unjudged] = pending;
  if (unjudged !== undefined) {
    const { page } = brackets[unjudged];
    throw new StepFailed(
      `GET ${issuePath(issue, page.page)}`,
      `in each of ${TRIES} tries, ${issue}'s ${page.list} list read before this read and the one read after it differ where this read is judged, so no try could be compared`
    );
  }
  return lines;
}

interface WalkedPage {
  readonly read: string;
  readonly threads: { readonly at: string; readonly id: string }[];
}

/** Every page of one read, from the first through each `before`. */
async function walkPages(probe: Probe, issue: string, page: PageRead): Promise<WalkedPage[]> {
  const pages: WalkedPage[] = [];
  const positions = new Set<string>();
  let before: string | null = null;
  do {
    const query: string =
      before === null ? page.page : `${page.page}&before=${encodeURIComponent(before)}`;
    const answer = await send(probe, issuePath(issue, query));
    const read = pageOf(answer, page);
    pages.push({ read: answer.read, threads: read.threads.map(({ at, id }) => ({ at, id })) });
    before = read.before;
    if (before !== null) {
      if (positions.has(before)) {
        throw new StepFailed(answer.read, `answered before ${before} a second time`);
      }
      positions.add(before);
    }
  } while (before !== null);
  return pages;
}

/** The walk holds exactly the threads the lists select, in their order, each with its last
 *  activity; a comment thread R4 cannot be read for is in its place where the walk holds it. */
function judgeWalk(selected: readonly Selected[], pages: readonly WalkedPage[]) {
  const threads = pages.flatMap((page, index) =>
    page.threads.map((thread) => ({ ...thread, page: index }))
  );
  const onWalk = new Set(threads.map((thread) => thread.id));
  const selectedIds = new Set(selected.map((thread) => thread.id));
  const expected = selected.filter((thread) => !thread.either || onWalk.has(thread.id));
  for (let place = 0; place < Math.max(expected.length, threads.length); place += 1) {
    const want = expected[place];
    const got = threads[place];
    if (want !== undefined && got !== undefined && want.id === got.id && want.at === got.at) {
      continue;
    }
    const { read } = pages[got?.page ?? pages.length - 1];
    let why: string;
    if (got === undefined) {
      why = `the walk ends after ${place} threads, where the lists select ${expected.length}: it leaves out ${want.id} (last activity ${want.at})`;
    } else if (want === undefined) {
      why = `place ${place + 1} of the walk holds ${got.id}, past the ${expected.length} threads the lists select`;
    } else if (want.id === got.id) {
      why = `thread ${got.id} reads last_activity_at ${got.at}, where its entries in the lists give ${want.at}`;
    } else {
      why = `place ${place + 1} of the walk holds ${got.id} (last activity ${got.at}), where the lists put ${want.id} (last activity ${want.at})`;
      if (!onWalk.has(want.id)) why += `; the walk leaves out ${want.id}`;
      if (!selectedIds.has(got.id)) why += `; the lists do not select ${got.id}`;
    }
    throw new StepFailed(read, why);
  }
  return { pages: pages.length, threads: threads.length };
}

async function walk(probe: Probe, issue: string): Promise<void> {
  const lines = await bracketed(
    probe,
    issue,
    false,
    WALKED_PAGES.map(
      (page): Bracket<WalkedPage[], Record<string, number>> => ({
        judge: (lists, pages) => {
          const selected = page.select(lists, issue);
          const either = selected.filter((thread) => thread.either).length;
          return {
            ...judgeWalk(selected, pages),
            ...(page === COMMENTS_PERSON_IN ? { either_way: either } : {}),
          };
        },
        page,
        read: () => walkPages(probe, issue, page),
        view: (lists) => page.select(lists, issue),
      })
    )
  );
  for (const [index, page] of WALKED_PAGES.entries()) {
    write({ step: "walk", issue, page: page.page, ...lines[index] });
  }
}

/** Where `page` first differs from `lists`, as a path and both values; undefined where equal. */
function firstDifference(page: unknown, lists: unknown, path: string): string | undefined {
  if (Bun.deepEquals(page, lists)) return undefined;
  if (Array.isArray(page) && Array.isArray(lists) && page.length === lists.length) {
    for (const [index, item] of page.entries()) {
      const found = firstDifference(item, lists[index], `${path}[${index}]`);
      if (found !== undefined) return found;
    }
  }
  if (isObject(page) && isObject(lists)) {
    for (const key of new Set([...Object.keys(page), ...Object.keys(lists)])) {
      const found = firstDifference(page[key], lists[key], path === "" ? key : `${path}.${key}`);
      if (found !== undefined) return found;
    }
  }
  return `${path === "" ? "the row" : path}: the page holds ${excerpt(page)}, the lists ${excerpt(lists)}`;
}

interface Weighed {
  readonly answer: Answer;
  readonly page: Page;
}

/** The page holds `threads`, `before` and `remaining` and no other member, and each row is the
 *  row R2 builds for its thread from the lists, but for `last_activity_at`. */
function judgeWeigh(page: PageRead, lists: Lists, { answer, page: read }: Weighed) {
  if (read.members.join(",") !== "before,remaining,threads") {
    throw new StepFailed(
      answer.read,
      `the page holds the members ${read.members.join(", ")}, where it holds threads, before and remaining and no other`
    );
  }
  const rows = page.rows(lists);
  for (const [index, thread] of read.threads.entries()) {
    const built = rows.get(thread.id);
    if (built === undefined) {
      throw new StepFailed(answer.read, `row ${index + 1}, thread ${thread.id}, is in no list`);
    }
    const row = Object.fromEntries(
      Object.entries(thread.row).filter(([member]) => member !== "last_activity_at")
    );
    const difference = firstDifference(row, built, "");
    if (difference !== undefined) {
      throw new StepFailed(
        answer.read,
        `row ${index + 1}, thread ${thread.id}, differs from the row R2 builds from the lists at ${difference}`
      );
    }
  }
  return { bytes: answer.bytes, threads: read.threads.length };
}

async function weigh(probe: Probe, issue: string): Promise<void> {
  const lines = await bracketed(
    probe,
    issue,
    true,
    ALL_PAGES.map(
      (page): Bracket<Weighed, { bytes: number; threads: number }> => ({
        judge: (lists, weighed) => judgeWeigh(page, lists, weighed),
        page,
        read: async () => {
          const answer = await send(probe, issuePath(issue, page.page), { trees: true });
          return { answer, page: pageOf(answer, page) };
        },
        view: (lists, weighed) => {
          const rows = page.rows(lists);
          return weighed.page.threads.map((thread) => rows.get(thread.id) ?? null);
        },
      })
    )
  );
  let total = 0;
  for (const [index, page] of ALL_PAGES.entries()) {
    total += lines[index].bytes;
    write({ step: "weigh", issue, page: page.page, ...lines[index] });
  }
  const issueRead = await send(probe, `/api/v1/issues/${encodeURIComponent(issue)}`, {
    trees: true,
  });
  const events = await send(probe, issuePath(issue, "events?order=desc&limit=200"), {
    trees: true,
  });
  write({
    step: "weigh",
    issue,
    all_first_pages_bytes: total,
    bound_bytes: ALL_FIRST_PAGES_BYTES,
    over_bound: total > ALL_FIRST_PAGES_BYTES,
    issue_bytes: issueRead.bytes,
    events_bytes: events.bytes,
  });
}

/** The logins of the people who wrote on the issue's list of `kind`, an entry each. */
function peopleOf(lists: Lists, kind: ListKind): string[] {
  const logins: string[] = [];
  const person = (actor: { id: string; kind: string } | undefined) => {
    if (actor?.kind === "user") logins.push(actor.id);
  };
  if (kind === "comments") for (const comment of listOf(lists, kind)) person(comment.author);
  if (kind === "messages") {
    for (const row of listOf(lists, kind)) {
      for (const entry of messageEntries(row)) person(entry.author);
    }
  }
  if (kind === "asks") {
    for (const row of listOf(lists, kind)) {
      person(row.author);
      for (const reply of row.thread?.replies ?? []) person(reply.author);
      if (row.answer?.user !== undefined) logins.push(row.answer.user);
      person(row.resolution?.actor);
    }
  }
  return logins;
}

/** Mine's `author`: the person with the most entries on the issue's list of `kind`, or, where no
 *  person wrote there, on its other two lists; the smaller login at a tie. */
async function personOf(probe: Probe, issue: string, kind: ListKind): Promise<string> {
  const kinds = [
    kind,
    ...(["comments", "messages", "asks"] as const).filter((each) => each !== kind),
  ];
  for (const each of kinds) {
    const counts = new Map<string, number>();
    for (const login of peopleOf(await readLists(probe, issue, [each], false), each)) {
      counts.set(login, (counts.get(login) ?? 0) + 1);
    }
    let chosen: [string, number] | undefined;
    for (const entry of counts) {
      if (
        chosen === undefined ||
        entry[1] > chosen[1] ||
        (entry[1] === chosen[1] && entry[0] < chosen[0])
      ) {
        chosen = entry;
      }
    }
    if (chosen !== undefined) return chosen[0];
  }
  throw new StepFailed(
    `GET ${issuePath(issue, LIST_QUERY[kind])}`,
    `no person (an author of the kind user) wrote on ${issue}'s comments, messages or asks, so Mine has no login to read with`
  );
}

/** The time to the first byte of one read, which must answer 200. */
async function timeToFirstByte(probe: Probe, path: string): Promise<number> {
  const read = `GET ${path}`;
  probe.lastRead = read;
  let elapsed: number;
  let status: number;
  try {
    const start = performance.now();
    const response = await fetch(new URL(path, probe.options.url), {
      headers: { Authorization: `Bearer ${probe.options.bearer}`, [TREES_HEADER]: "1" },
    });
    elapsed = performance.now() - start;
    status = response.status;
    await response.arrayBuffer();
  } catch (error) {
    throw new StepFailed(read, `the request failed: ${messageOf(error)}`);
  }
  if (status !== 200) throw new StepFailed(read, `answered ${status}, not 200`);
  return elapsed;
}

/** The nearest-rank percentile: the smallest sample at least `percent` of the samples reach. */
function nearestRank(samples: readonly number[], percent: number): number {
  const sorted = [...samples].sort((a, b) => a - b);
  return sorted[Math.ceil((percent / 100) * sorted.length) - 1] ?? Number.NaN;
}

async function time(probe: Probe): Promise<void> {
  const { timeAsks, timeComments, timeMessages } = probe.options;
  const mine = async (issue: string, page: PageRead) =>
    `${page.page}&author=${encodeURIComponent(await personOf(probe, issue, page.list))}`;
  const reads = [
    { issue: timeComments, page: COMMENTS_PERSON_IN, query: COMMENTS_PERSON_IN.page, view: "All" },
    { issue: timeMessages, page: MESSAGES_PERSON_IN, query: MESSAGES_PERSON_IN.page, view: "All" },
    { issue: timeAsks, page: ASKS_THREADS, query: ASKS_THREADS.page, view: "All" },
    {
      issue: timeComments,
      page: COMMENTS_PERSON_IN,
      query: await mine(timeComments, COMMENTS_PERSON_IN),
      view: "Mine",
    },
    {
      issue: timeMessages,
      page: MESSAGES_PERSON_IN,
      query: await mine(timeMessages, MESSAGES_PERSON_IN),
      view: "Mine",
    },
    {
      issue: timeAsks,
      page: ASKS_THREADS,
      query: await mine(timeAsks, ASKS_THREADS),
      view: "Mine",
    },
    {
      issue: timeMessages,
      page: MESSAGES_PERSON_NONE,
      query: MESSAGES_PERSON_NONE.page,
      view: "Agents",
    },
  ];
  let slow: { read: string; p90: number } | undefined;
  for (const { issue, page, query, view } of reads) {
    const path = issuePath(issue, query);
    // The first warm-up read is read whole, so a build without the pages fails here, not later.
    pageOf(await send(probe, path, { trees: true }), page);
    for (let read = 1; read < WARM_UP_READS; read += 1) await timeToFirstByte(probe, path);
    const samples: number[] = [];
    for (let read = 0; read < TIMED_READS; read += 1) {
      samples.push(await timeToFirstByte(probe, path));
    }
    const p90 = Math.round(nearestRank(samples, 90) * 10) / 10;
    write({ step: "time", view, issue, page: query, p90_ms: p90, bound_ms: BOUND_MS });
    if (p90 > BOUND_MS && slow === undefined) slow = { p90, read: `GET ${path}` };
  }
  if (slow !== undefined) {
    throw new StepFailed(
      slow.read,
      `the 90th percentile of ${TIMED_READS} timed reads' time to the first byte is ${slow.p90} ms, above ${BOUND_MS} ms`
    );
  }
}

function idOf(answer: Answer): string {
  if (isObject(answer.body) && typeof answer.body.id === "string") return answer.body.id;
  throw new StepFailed(answer.read, `answered ${excerpt(answer.body)}, which names no id`);
}

/** The read-back of a reply holds the quote it wrote, `reply_block.quote` and `.occurrence`. */
function quoteOf(answer: Answer, member: "comment" | "message") {
  const item = isObject(answer.body) ? answer.body[member] : undefined;
  const block = isObject(item) ? item.reply_block : undefined;
  if (!isObject(block)) {
    throw new StepFailed(
      answer.read,
      `the reply's reply_block is ${excerpt(block)}, not the block it answers`
    );
  }
  if (typeof block.quote !== "string") {
    throw new StepFailed(
      answer.read,
      `the reply's reply_block holds no quote, where the reply quoted "${QUOTED_WORD}" at occurrence 1`
    );
  }
  if (block.quote !== QUOTED_WORD || block.occurrence !== 1) {
    throw new StepFailed(
      answer.read,
      `the reply's reply_block holds quote ${excerpt(block.quote)} at occurrence ${excerpt(block.occurrence)}, where the reply quoted "${QUOTED_WORD}" at occurrence 1`
    );
  }
  return { quote: block.quote, occurrence: block.occurrence };
}

async function quote(probe: Probe): Promise<void> {
  const { issue, session } = probe.options;
  const actor = { id: session, kind: "session" };
  const check = crypto.randomUUID();
  const body = `Thread pages check ${check} at ${new Date().toISOString()}: one ${QUOTED_WORD}, then a second ${QUOTED_WORD}.`;
  const reply = `Thread pages check ${check}: a reply to the second ${QUOTED_WORD} of that block.`;
  const comments = issuePath(issue, "comments");
  const comment = idOf(
    await send(probe, comments, { body: { actor, body } satisfies CreateCommentInput })
  );
  const commentReply = idOf(
    await send(probe, comments, {
      body: {
        actor,
        body: reply,
        occurrence: 1,
        quote: QUOTED_WORD,
        reply_to: comment,
        reply_to_block: FIRST_BLOCK,
      } satisfies CreateCommentInput,
    })
  );
  const messages = issuePath(issue, "messages");
  const message = idOf(
    await send(probe, messages, { body: { actor, body } satisfies CreateMessageInput })
  );
  const messageReply = idOf(
    await send(probe, messages, {
      body: {
        actor,
        body: reply,
        in_reply_to: message,
        in_reply_to_block: FIRST_BLOCK,
        occurrence: 1,
        quote: QUOTED_WORD,
      } satisfies CreateMessageInput,
    })
  );
  const commentRead = await send(probe, `/api/v1/comments/${encodeURIComponent(commentReply)}`);
  write({ step: "quote", read: commentRead.read, ...quoteOf(commentRead, "comment") });
  const messageRead = await send(
    probe,
    issuePath(issue, `messages/${encodeURIComponent(messageReply)}`)
  );
  write({ step: "quote", read: messageRead.read, ...quoteOf(messageRead, "message") });
}

async function main(argv: readonly string[]): Promise<number> {
  let probe: Probe | undefined;
  try {
    probe = { lastRead: "", options: await optionsFrom(argv), step: "walk" };
    for (const issue of probe.options.walk) await walk(probe, issue);
    probe.step = "weigh";
    for (const issue of probe.options.weigh) await weigh(probe, issue);
    probe.step = "time";
    await time(probe);
    probe.step = "quote";
    await quote(probe);
    write({ ok: true });
    return 0;
  } catch (error) {
    if (error instanceof UsageError) {
      process.stderr.write(`dispatch-thread-pages: ${error.message}\n${USAGE}\n`);
      return 2;
    }
    write({
      ok: false,
      step: probe?.step ?? "walk",
      read: error instanceof StepFailed ? error.read : (probe?.lastRead ?? ""),
      why: messageOf(error),
    });
    return 1;
  }
}

process.exit(await main(Bun.argv.slice(2)));
