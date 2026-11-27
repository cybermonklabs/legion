import { afterAll, beforeEach, describe, expect, setSystemTime, test } from "bun:test";
import { execFileSync } from "node:child_process";
import { existsSync, mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import type { ArtifactBlock } from "@legion/contracts";
import {
  DISPATCH_BLOCK_UNITS,
  DISPATCH_READ_ONLY_TOKEN,
  IDEMPOTENCY_KEY_HEADER,
  MARKDOWN_DOCUMENT_MAX_BYTES,
  PROJECT_KEY_RULE,
  TEXT_REFUSAL_HEADER,
} from "@legion/contracts";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import { forgetBrokerClients } from "../broker-credential";
import { cliRefusals, type DispatchCliIo, fitOutput, runDispatchCli } from "../dispatch-cli";
import { resetAdviceMemory } from "../dispatch-execute";
import { readFileText } from "../dispatch-file-text";
import { forgetShownPictures } from "../dispatch-picture-tools";

interface Recorded {
  readonly method: string;
  readonly path: string;
  readonly body: unknown;
}

const makeDirectory = temporaryDirectories("dispatch-cli");

/** The Dispatch bearer each command is handed. */
const TOKEN = fixtureSecret("dsp");

const PNG = Buffer.from("89504e470d0a1a0a0000000d49484452", "hex");
const requests: Recorded[] = [];
let triage = false;
/** The length of the reply body the long message thread carries. */
let replyLength = 1;
const LONG_THREAD = "11111111-1111-4111-8111-111111111111";
const human = { kind: "human", id: "h" };
/** The line the fake Dispatch answers a result's dry run with. */
const RESULT_DRY_RUN_LINE =
  "- Check k1 (before_merge): pass at 1111111111111111111111111111111111111111, recorded by implementer. open-items v14 lists every row.";

/** How the Dispatch server refuses a body whose block `block`, a paragraph, is `units` UTF-16
 *  units long, past the bound it holds each block to, worded as `itemblocks.CapExceeded` words
 *  it. The fake server below answers it, and the command prints it as it came. */
function blockCapRefusal(block: number, units: number): string {
  return (
    `body block ${block} (paragraph) is ${units - DISPATCH_BLOCK_UNITS} characters over the ` +
    `${DISPATCH_BLOCK_UNITS}-character block limit (${units}/${DISPATCH_BLOCK_UNITS})`
  );
}

/** The same refusal as the command prints it: a refusal Dispatch gives a code is printed with
 *  that code ahead of its words, so the person reads which refusal it is. */
const printedBlockCapRefusal = (block: number, units: number) =>
  `CAP_EXCEEDED: ${blockCapRefusal(block, units)}`;

/** How the Dispatch server refuses a body whose block `block` is an ask block, worded as
 *  `itemblocks.DecisionBlock` words it: the card such a block draws is one nobody can answer
 *  where it stands, so the body is refused however short it is (INFRA-1201). */
function decisionBlockRefusal(block: number): string {
  return (
    `body block ${block} (ask) holds an ask block, which nobody can answer where it stands: ` +
    "ask the question in the issue's own document, as an ask block there, or open an ask " +
    "with `dispatch ask`; to show an ask block as an example, put it in a fenced code block"
  );
}

/** The server's refusal of `body`: an ask block in it (400 DECISION_BLOCK_IN_BODY), else a
 *  paragraph over the bound Dispatch holds each block to (400 CAP_EXCEEDED), else undefined.
 *  Dispatch bounds a body block by block and never as a whole, so a body of any length in
 *  paragraphs posts and one long paragraph does not. */
function bodyRefusal(body: string): { code: string; error: string } | undefined {
  for (const [index, block] of body.split(/\n[ \t]*\n/).entries()) {
    if (block.startsWith(":::ask")) {
      return { code: "DECISION_BLOCK_IN_BODY", error: decisionBlockRefusal(index + 1) };
    }
    if (block.length > DISPATCH_BLOCK_UNITS) {
      return { code: "CAP_EXCEEDED", error: blockCapRefusal(index + 1, block.length) };
    }
  }
  return undefined;
}

/** A body of exactly `length` UTF-16 units made of paragraphs, each short enough for the bound
 *  Dispatch holds one block to, so the body passes that bound however long the whole text is.
 *  The line number opens every line, so the text changes along its whole span and a body cut
 *  anywhere fails the equality, not only one of another length. */
function paragraphsOf(length: number): string {
  let text = "";
  for (let line = 0; text.length < length; line += 1) {
    text += `${line}: the quick brown fox jumps over the lazy dog.\n\n`;
  }
  return text.slice(0, length);
}

/** Fifteen JSON records, one per line, as a records upload holds them: the text `dispatch
 *  artifact --path` must send unchanged, and the text `--content-file` refuses to store as a
 *  Markdown document, which would join the lines into one paragraph. */
const rows = `${Array.from(
  { length: 15 },
  (_, index) => `{"row_id": ${index}, "file_name": "wave_${index}.json", "note": "*kept*"}`
).join("\n")}\n`;

/** The Envoy listener's answer for session ses-1's subscriptions. */
let interests: () => Response = () => Response.json({ error: "unset" }, { status: 500 });

const issue = {
  key: "DSP-1",
  title: "Native workspace",
  status: "in_progress",
  labels: [],
  route: null,
  parent: null,
  external_links: [],
  artifacts: [],
  advances: [],
  advances_revision: 0,
};

/** DSP-2's spec, whose blocks `dispatch doc-read --blocks` lists. "é" is two bytes in UTF-8, so
 *  every block after the heading starts at a byte offset past its index in the string. */
const intro = `${"x".repeat(59)}😀 then more words past the cut.`;
const spec = [
  "# Café plan",
  "",
  intro,
  "",
  "- First item",
  "- Second item",
  "",
  "| # | Line | |",
  "| --- | --- | --- |",
  "| 1 | Backfill | extra |",
  "",
].join("\n");

/** Where `text` first stands in `spec`, in UTF-8 bytes, as Dispatch's /blocks route reports it. */
function byteRange(text: string): { from: number; to: number } {
  const at = spec.indexOf(text);
  if (at < 0) throw new Error(`the spec holds no ${JSON.stringify(text)}`);
  const from = Buffer.byteLength(spec.slice(0, at));
  return { from, to: from + Buffer.byteLength(text) };
}

const unanchored = { comments: 0, asks: 0 };
const specBlocks: ArtifactBlock[] = [
  {
    id: "h-1",
    type: "heading",
    ...byteRange("# Café plan"),
    token: "sha256:h",
    references: unanchored,
  },
  {
    id: "p-1",
    type: "paragraph",
    ...byteRange(intro),
    token: "sha256:p",
    references: { comments: 1, asks: 0 },
  },
  {
    id: "l-1",
    type: "bullet_list",
    ...byteRange("- First item\n- Second item"),
    token: "sha256:l",
    references: unanchored,
  },
  {
    id: "i-1",
    type: "list_item",
    ...byteRange("- First item"),
    token: "sha256:i1",
    references: unanchored,
  },
  {
    id: "ip-1",
    type: "paragraph",
    ...byteRange("First item"),
    token: "sha256:ip1",
    references: unanchored,
  },
  {
    id: "i-2",
    type: "list_item",
    ...byteRange("- Second item"),
    token: "sha256:i2",
    references: unanchored,
  },
  {
    id: "t-1",
    type: "table",
    ...byteRange("| # | Line | |\n| --- | --- | --- |\n| 1 | Backfill | extra |"),
    token: "sha256:t",
    references: { comments: 0, asks: 1 },
    table: {
      rows: [
        {
          id: "r-0",
          index: 0,
          cells: [
            { id: "c-0-0", row: 0, column: 0, header: "#", text: "#" },
            { id: "c-0-1", row: 0, column: 1, header: "Line", text: "Line" },
            { id: "c-0-2", row: 0, column: 2, header: "", text: "" },
          ],
        },
        {
          id: "",
          index: 1,
          cells: [
            { id: "c-1-0", row: 1, column: 0, header: "#", text: "1" },
            { id: "", row: 1, column: 1, header: "Line", text: "Backfill" },
            { id: "c-1-2", row: 1, column: 2, header: "", text: "extra" },
          ],
        },
      ],
    },
  },
];

const blocksIssue = {
  ...issue,
  key: "DSP-2",
  primary_artifact_id: "doc-2",
  open_asks: [],
  artifacts: [
    { id: "doc-2", slug: "spec", name: "spec.md", kind: "doc", primary: true, versions: [] },
    {
      id: "pic-2",
      slug: "chart-png",
      name: "chart.png",
      kind: "image",
      primary: false,
      versions: [{ number: 1, mime: "image/png", size: PNG.length }],
    },
  ],
};

/** DSP-5's spec as Dispatch stores it: every code block fenced, holding the whitespace-only
 *  lines its engine keeps in it, so the text `dispatch doc-read` prints is text whose line ends
 *  matter. `packages/envoy/internal/dispatch/pmdoc/writer_spelling_test.go` holds the writer to
 *  this spelling; here it is the bytes the command must print as they came. */
const storedCode =
  "> - ```\n>   code\n>   \n>   note\n>   ```\n\nBetween.\n\n- a\n\n  ```\n      x\n    \n  ```\n- b\n\nAfter.\n\n```\n      y\n    \n   \n```\n\nEnd.\n";

const codeIssue = {
  ...issue,
  key: "DSP-5",
  title: "Indented code whitespace lines",
  primary_artifact_id: "doc-5",
  open_asks: [],
  artifacts: [
    { id: "doc-5", slug: "spec", name: "spec.md", kind: "doc", primary: true, versions: [] },
  ],
};

/** DSP-3 as a Dispatch server from before the repository fields and before priorities answers
 *  it: no repo, resolved_repo, advances or advances_revision at all. */
const olderServerIssue = {
  key: "DSP-3",
  title: "Read from an older server",
  status: "todo",
  priority: null,
  assignee: null,
  claim: null,
  components: { mode: "inherit", ids: [], unknown: [], reason: null, inherited_from: null },
  route: null,
  open_asks: [],
  last_seq: 0,
  external_links: [],
  labels: [],
};

/** How the DSP-2 routes answer: a Dispatch older than the table listing omits `table`. */
let blocksServer = { tableRows: true };
/** DSP-4's event log: the issue reads `last_seq` `last`, and the events route serves seq 1 to
 *  `served` (past `last` once events were written after the issue was read), each a message, by
 *  `after` and `limit` as Dispatch pages them. `queries` records each events request's query. */
let eventLog = { last: 40, served: 40, queries: [] as string[] };
/** DSP-1's handoffs as the handoff routes store them, oldest first: each write the next version
 *  of its phase, written by the actor it names, its body kept as the text sent. */
const handoffs: {
  id: string;
  issue_key: string;
  phase: string;
  version: number;
  author: unknown;
  created_at: string;
  body: string;
}[] = [];
/** The store ends every document with a line feed, so a text without one comes back with one. */
const keptText = (markdown: string): Response =>
  Response.json({ markdown: markdown.endsWith("\n") ? markdown : `${markdown}\n` });
/** How the two round trips answer the text they are sent - the markdown inline, a file's bytes as
 *  text - and the body that carried it: as the store keeps it, unless a test sets another answer. */
let roundTrip: (
  markdown: string,
  body: { readonly new_document?: unknown }
) => Response | Promise<Response> = keptText;
/** What DSP-1 holds, which an upload reads to refuse a name that exists: no document, unless a
 *  test sets another answer. */
let issueArtifacts: readonly object[] = [];
/** How an upload to DSP-1 is refused, or undefined for an upload stored as version 1: as Dispatch
 *  refuses it, where a test sets a refusal. */
let uploadRefusal: (() => Response) | undefined;

const server = Bun.serve({
  port: 0,
  async fetch(request) {
    const { pathname, search, searchParams } = new URL(request.url);
    // An upload is multipart where it carries a file and JSON where it carries inline content,
    // as the route takes both: a file's part is recorded as the text it carried, since the
    // recorder below parses every body as JSON and would throw on those bytes
    // (`dispatch-askgate.test.ts` reads an upload the same way).
    if (pathname === "/api/v1/issues/DSP-1/artifacts" && request.method === "POST") {
      const sent = request.headers.get("content-type")?.startsWith("multipart/")
        ? (await request.formData()).get("file")
        : await request.json();
      requests.push({
        method: request.method,
        path: pathname,
        body: sent instanceof Blob ? await sent.text() : sent,
      });
      if (uploadRefusal !== undefined) return uploadRefusal();
      return Response.json({
        artifact: {
          id: "artifact-1",
          issue_key: "DSP-1",
          name: "rows.jsonl",
          slug: "rows-jsonl",
          kind: "file",
          primary: false,
        },
        version: { number: 1 },
      });
    }
    // What the issue holds, which an upload reads to refuse a name that exists: DSP-1 holds no
    // document here unless a test says it does (`issueArtifacts`), so every other upload below
    // writes a first version. It is recorded with the rest.
    if (pathname === "/api/v1/issues/DSP-1/artifacts" && request.method === "GET") {
      requests.push({ method: request.method, path: pathname, body: undefined });
      return Response.json(issueArtifacts);
    }
    const text = await request.text();
    const body = text === "" ? undefined : JSON.parse(text);
    requests.push({ method: request.method, path: pathname, body });
    // Inline text travels as `markdown` to its round trip, a file's bytes as base64 in `file` to
    // the file round trip.
    if (pathname === ROUND_TRIP && request.method === "POST") {
      return roundTrip(body.markdown, body);
    }
    if (pathname === FILE_ROUND_TRIP && request.method === "POST") {
      return roundTrip(Buffer.from(body.file as string, "base64").toString("utf8"), body);
    }
    if (pathname === "/api/v1/issues/DSP-2" && request.method === "GET") {
      return Response.json(blocksIssue);
    }
    if (pathname === "/api/v1/artifacts/doc-2/text") {
      const listed = blocksServer.tableRows
        ? specBlocks
        : specBlocks.map(({ table: _, ...block }) => block);
      return Response.json({
        markdown: spec,
        version: 3,
        token: "sha256:doc",
        // The blocks come from the same answer as the markdown they index, as Dispatch serves
        // them: `?blocks=true` is the one read `doc-read --blocks` makes.
        ...(searchParams.get("blocks") === "true" ? { blocks: listed } : {}),
      });
    }
    if (pathname === "/api/v1/issues/DSP-5" && request.method === "GET") {
      return Response.json(codeIssue);
    }
    if (pathname === "/api/v1/artifacts/doc-5/text") {
      return Response.json({ markdown: storedCode, version: 1, token: "sha256:code" });
    }
    if (pathname === "/api/v1/issues/DSP-5/comments") {
      return Response.json([]);
    }
    if (pathname === "/api/v1/issues/DSP-1/asks" && request.method === "POST") {
      return Response.json({
        id: "ask-a",
        issue_key: "DSP-1",
        question: body.question,
        urgency: "med",
        advice: { issue_status: "in_progress", session_writes_since_human: 0, your_open_asks: [] },
      });
    }
    if (pathname === "/api/v1/issues/DSP-1/messages" && request.method === "POST") {
      const refusal = bodyRefusal(body.body as string);
      if (refusal !== undefined) return Response.json(refusal, { status: 400 });
      return Response.json({
        id: "message-1",
        issue_key: "DSP-1",
        advice: {
          issue_status: triage ? "triage" : "in_progress",
          session_writes_since_human: 0,
          your_open_asks: [],
        },
      });
    }
    if (pathname === "/api/v1/issues/DSP-1/results/dry-run" && request.method === "POST") {
      return Response.json({ line: RESULT_DRY_RUN_LINE });
    }
    if (pathname === "/api/v1/issues/DSP-1/comments" && request.method === "POST") {
      const refusal = bodyRefusal(body.body as string);
      if (refusal !== undefined) return Response.json(refusal, { status: 400 });
      return Response.json({
        id: "comment-1",
        issue_key: "DSP-1",
        advice: { issue_status: "in_progress", session_writes_since_human: 0, your_open_asks: [] },
      });
    }
    if (pathname === "/api/v1/issues/DSP-3" && request.method === "GET") {
      return Response.json(olderServerIssue);
    }
    if (pathname === "/api/v1/issues/DSP-3/events") {
      return Response.json([]);
    }
    if (pathname === "/api/v1/issues/DSP-4" && request.method === "GET") {
      return Response.json({
        ...olderServerIssue,
        key: "DSP-4",
        title: "A long history",
        last_seq: eventLog.last,
        repo: null,
        resolved_repo: null,
      });
    }
    if (pathname === "/api/v1/issues/DSP-4/events") {
      eventLog.queries.push(search);
      const after = Number(searchParams.get("after"));
      const through = Math.min(after + Number(searchParams.get("limit")), eventLog.served);
      const seqs = Array.from(
        { length: Math.max(0, through - after) },
        (_, index) => after + index + 1
      );
      return Response.json(
        seqs.map((seq) => ({
          id: `event-${seq}`,
          issue_key: "DSP-4",
          seq,
          type: "message.created",
          actor: human,
          payload: { body: `Event ${seq}` },
          created_at: "2026-10-25T00:00:00Z",
        }))
      );
    }
    if (
      pathname === "/api/v1/issues/DSP-3/references" ||
      pathname === "/api/v1/issues/DSP-4/references" ||
      pathname === "/api/v1/references"
    ) {
      return Response.json({ node: { kind: "issue", id: "DSP-3" }, edges: [], members: [] });
    }
    if (pathname === "/api/v1/issues/DSP-1" && request.method === "GET") {
      return Response.json(issue);
    }
    if (pathname === "/api/v1/projects" && request.method === "POST") {
      if (body.key === "TAKEN") {
        return Response.json(
          { code: "PROJECT_EXISTS", error: "a project with this key already exists" },
          { status: 409 }
        );
      }
      return Response.json(
        { key: body.key, name: body.name, open_asks: 0, created_at: "2026-10-11T00:00:00Z" },
        { status: 201 }
      );
    }
    if (pathname === "/api/v1/projects" && request.method === "GET") {
      return Response.json([
        { key: "CORE", name: "Core", open_asks: 0, created_at: "2026-09-01T00:00:00Z" },
        { key: "SMOKE684", name: "Smoke", open_asks: 0, created_at: "2026-10-11T00:00:00Z" },
      ]);
    }
    if (pathname === "/api/v1/agents/ses-pic/artifacts/shot-png") {
      return Response.json({
        id: "pic-1",
        issue_key: null,
        project: "",
        session_id: "ses-pic",
        slug: "shot-png",
        name: "shot.png",
        kind: "image",
        versions: [{ number: 1, mime: "image/png", size: PNG.length }],
      });
    }
    if (pathname === "/api/v1/artifacts/pic-1/versions/1") {
      return new Response(PNG, { headers: { "Content-Type": "image/png" } });
    }
    if (pathname === `/api/v1/messages/${LONG_THREAD}`) {
      return Response.json({
        message: {
          id: LONG_THREAD,
          issue_key: null,
          author: human,
          body: "![shot.png](dispatch://agent/ses-pic/artifact/shot-png@v1)",
          target: null,
          in_reply_to: null,
          deliveries: [],
          created_at: "2026-10-08T00:00:00Z",
        },
        replies: [
          {
            id: "22222222-2222-4222-8222-222222222222",
            issue_key: null,
            author: human,
            body: "x".repeat(replyLength),
            target: null,
            in_reply_to: LONG_THREAD,
            deliveries: [],
            created_at: "2026-10-08T00:01:00Z",
          },
        ],
      });
    }
    const handoffWrite = pathname.match(/^\/api\/v1\/issues\/DSP-1\/handoffs\/([a-z-]+)$/);
    if (handoffWrite !== null && request.method === "POST") {
      const phase = handoffWrite[1] ?? "";
      const record = {
        id: `00000000-0000-4000-8000-00000000000${handoffs.length + 1}`,
        issue_key: "DSP-1",
        phase,
        version: handoffs.filter((handoff) => handoff.phase === phase).length + 1,
        author: body.actor,
        created_at: `2026-10-26T00:00:0${handoffs.length + 1}Z`,
        body: body.body,
      };
      handoffs.push(record);
      const { body: _, ...summary } = record;
      return Response.json(summary, { status: 201 });
    }
    const handoffLatest = pathname.match(/^\/api\/v1\/issues\/DSP-1\/handoffs\/([a-z-]+)\/latest$/);
    if (handoffLatest !== null) {
      const latest = handoffs.filter((handoff) => handoff.phase === handoffLatest[1]).at(-1);
      return latest === undefined
        ? Response.json(
            { code: "HANDOFF_NOT_FOUND", error: `issue DSP-1 has no ${handoffLatest[1]} handoff` },
            { status: 404 }
          )
        : Response.json(latest);
    }
    if (pathname === "/api/v1/issues/DSP-1/handoffs") {
      return Response.json(handoffs.map(({ body: _, ...summary }) => summary).reverse());
    }
    const byId = handoffs.find((handoff) => pathname === `/api/v1/handoffs/${handoff.id}`);
    if (byId !== undefined) return Response.json(byId);
    if (pathname === "/v1/interests/ses-1") return interests();
    // A Dispatch build older than the answers route, which it therefore lacks.
    if (pathname.endsWith("/answers")) {
      return Response.json(
        { code: "NOT_FOUND", error: `no route for ${request.method} ${pathname}` },
        { status: 404 }
      );
    }
    return Response.json({ error: `unexpected ${request.method} ${pathname}` }, { status: 500 });
  },
});

afterAll(() => server.stop(true));

let stateDir = "";
beforeEach(() => {
  requests.length = 0;
  triage = false;
  blocksServer = { tableRows: true };
  eventLog = { last: 40, served: 40, queries: [] };
  handoffs.length = 0;
  interests = () => Response.json({ error: "unset" }, { status: 500 });
  roundTrip = keptText;
  issueArtifacts = [];
  uploadRefusal = undefined;
  stateDir = makeDirectory();
  // Each run is its own process in production: nothing the library remembers carries over.
  resetAdviceMemory();
});

function environment(
  extra: Record<string, string | undefined>
): Record<string, string | undefined> {
  return {
    HOME: stateDir,
    DISPATCH_URL: `http://127.0.0.1:${server.port}`,
    DISPATCH_TOKEN: TOKEN,
    DISPATCH_STATE_DIR: stateDir,
    ...extra,
  };
}

async function run(
  argv: readonly string[],
  extra: Record<string, string | undefined>,
  stdin = "",
  seams: Partial<
    Pick<DispatchCliIo, "fetchImpl" | "nearestAncestorNamed" | "dryRunDeadlineMs" | "readText">
  > = {}
): Promise<{ code: number; stdout: string; stderr: string }> {
  let stdout = "";
  let stderr = "";
  const code = await runDispatchCli(argv, environment(extra), {
    stdout: (text) => {
      stdout += text;
    },
    stderr: (text) => {
      stderr += text;
    },
    readText: (path) => (path === "-" ? stdin : readFileSync(path, "utf-8")),
    cwd: stateDir,
    ...seams,
  });
  return { code, stdout, stderr };
}

/** A fresh process for the same session: the in-memory memories are gone, the state dir stays. */
function nextProcess(sessionId: string): void {
  resetAdviceMemory();
  forgetShownPictures(sessionId);
}

/**
 * The test server behind Dispatch's keyed layer: every answer to a write carries its
 * `Idempotency-Key` back, and the first `failing` message posts fail in transport before they
 * reach it. A `/healthz` read is recorded in `probes` and never answers within any bound, as under
 * the load that stalls a write.
 */
function keyedDispatch(failing: number): {
  readonly fetchImpl: typeof fetch;
  readonly keys: string[];
  readonly probes: string[];
} {
  const keys: string[] = [];
  const probes: string[] = [];
  let left = failing;
  const fetchImpl = (async (input: string | URL | Request, init?: RequestInit) => {
    const request = new Request(input, init);
    const { pathname } = new URL(request.url);
    if (pathname === "/healthz") {
      probes.push(pathname);
      throw new DOMException("The operation timed out.", "TimeoutError");
    }
    const key = request.headers.get(IDEMPOTENCY_KEY_HEADER);
    if (request.method === "POST" && pathname === "/api/v1/issues/DSP-1/messages") {
      keys.push(key ?? "");
      if (left-- > 0) throw new TypeError("fetch failed");
    }
    const answer = await fetch(request);
    if (key === null) return answer;
    const headers = new Headers(answer.headers);
    headers.set(IDEMPOTENCY_KEY_HEADER, key);
    return new Response(answer.body, { status: answer.status, headers });
  }) as typeof fetch;
  return { fetchImpl, keys, probes };
}

function ledger(sessionId: string): { tool: string; error?: string }[] {
  const path = join(stateDir, "sessions", sessionId, "results.jsonl");
  return readFileSync(path, "utf-8")
    .split("\n")
    .filter((line) => line !== "")
    .map((line) => JSON.parse(line));
}

const omp = { DISPATCH_HOST: "omp", DISPATCH_SESSION_ID: "ses-1" };

/** Runs `argv` as a dry run and then as a call, each reading files and stdin's bytes `stdin` as the
 *  `dispatch` entry reads them (`readFileText`), and expects the dry run to exit 1 printing
 *  `stdout` alone, the call to answer as the dry run did, and neither to send anything. `name`
 *  heads each failure's label. Each runs in `env`, Dispatch on unless a test turns it off. */
async function refusedAlike(
  argv: readonly string[],
  stdin: Uint8Array,
  stdout: string,
  name: string,
  env: Record<string, string | undefined> = omp
): Promise<void> {
  const reader = { readText: (path: string) => readFileText(path, () => stdin) };
  const named = `${name}: ${argv.join(" ")}`;
  requests.length = 0;
  const dry = await run([...argv, "--dry-run"], env, "", reader);
  expect(dry, named).toEqual({ code: 1, stdout, stderr: "" });
  const call = await run(argv, env, "", reader);
  expect(call, named).toEqual(dry);
  expect(requests, named).toEqual([]);
}

/** Writes `body` to `file`, and runs `dispatch comment --body-file` reading it from `file` and then
 *  from stdin, each as the `dispatch` entry reads them (`readFileText`): expects the dry run to
 *  print the arguments with `body` as read and send nothing, and the call to post `body` as read. */
async function commentPostedAsRead(file: string, body: string): Promise<void> {
  writeFileSync(file, body);
  for (const [path, stdin] of [
    [file, Buffer.of()],
    ["-", Buffer.from(body)],
  ] as const) {
    const argv = ["comment", "--issue", "DSP-1", "--body-file", path];
    const reader = { readText: (read: string) => readFileText(read, () => stdin) };
    requests.length = 0;
    const dry = await run([...argv, "--dry-run"], omp, "", reader);
    expect(dry, path).toEqual({
      code: 0,
      stdout: `${JSON.stringify({ issue: "DSP-1", body }, null, 2)}\n`,
      stderr: "",
    });
    expect(requests, path).toEqual([]);
    const call = await run(argv, omp, "", reader);
    expect(call.code, call.stdout).toBe(0);
    const posted = requests.find((request) => request.path === "/api/v1/issues/DSP-1/comments");
    expect(posted?.body, path).toMatchObject({ body });
  }
}

/** Bytes an upload refuses, named by `name`, and the refusal printed for them under `field`. */
interface RefusedBytes {
  readonly name: string;
  readonly bytes: Uint8Array;
  readonly stdout: (field: string) => string;
}

/** For each row, expects `dispatch artifact` to refuse its bytes alike (`refusedAlike`) in `env`,
 *  read from `--content-file <file>` and from stdin under `content`, given inline as `--content`
 *  under `content` where the bytes are UTF-8, and from a markdown `--path` file under `file`. The
 *  inline text is an in-process caller's: argv carries no U+0000 on any kernel, and no argument
 *  past MAX_ARG_STRLEN, 32 pages, which is 128 KiB on 4 KiB pages, so with such pages no text past
 *  1 MiB, but a caller of `runDispatchCli` can hand it either, whatever the page size. */
async function uploadRefusedEveryWay(
  rows: readonly RefusedBytes[],
  env: Record<string, string | undefined> = omp
): Promise<void> {
  const file = join(stateDir, "text");
  const upload = ["artifact", "--issue", "DSP-1", "--name", "notes.md"];
  const utf8 = new TextDecoder("utf-8", { fatal: true });
  for (const row of rows) {
    writeFileSync(file, row.bytes);
    writeFileSync(join(stateDir, "x.md"), row.bytes);
    const inline = row.stdout("content");
    await refusedAlike([...upload, "--content-file", file], Buffer.of(), inline, row.name, env);
    await refusedAlike(UPLOAD, row.bytes, inline, row.name, env);
    let text: string | undefined;
    try {
      text = utf8.decode(row.bytes);
    } catch {
      // Bytes that are not UTF-8 are no text, so no caller can give them inline.
    }
    if (text !== undefined) {
      await refusedAlike([...upload, "--content", text], Buffer.of(), inline, row.name, env);
    }
    await refusedAlike(
      [...upload, "--path", "x.md"],
      Buffer.of(),
      row.stdout("file"),
      row.name,
      env
    );
  }
}

/** An inline upload, its text read from stdin, and the route its stored-text check asks. */
const UPLOAD = ["artifact", "--issue", "DSP-1", "--name", "notes.md", "--content-file", "-"];
const NOTES = "# Notes\n\n* one\n";
const NOTES_JSON = `${JSON.stringify({ issue: "DSP-1", name: "notes.md", content: NOTES }, null, 2)}\n`;
/** The notes.md DSP-1 holds in a test that sets it (`issueArtifacts`), which an upload under that
 *  name without --force is refused for. */
const NOTES_HELD = {
  id: "artifact-notes",
  issue_key: "DSP-1",
  name: "notes.md",
  slug: "notes-md",
  kind: "doc",
  created_by: { kind: "user", id: "alice" },
  versions: [{ number: 1, authors: [{ kind: "user", id: "alice" }] }],
};
const ROUND_TRIP = "/api/v1/markdown/round-trip";
const FILE_ROUND_TRIP = "/api/v1/markdown/file-round-trip";
/** The round trip's body for inline text a call without --force uploads: the text in `markdown`,
 *  asking for a new document's refusals (`new_document`), since that call makes a new document. */
const markdownBody = (markdown: string): { markdown: string; new_document: true } => ({
  markdown,
  new_document: true,
});
/** The file round trip's body for a `--path` file's bytes: base64 in `file`, asking for a new
 *  document's refusals as `markdownBody` does. */
const fileBody = (bytes: string | Uint8Array): { file: string; new_document: true } => ({
  file: Buffer.from(bytes).toString("base64"),
  new_document: true,
});
/** An ask block holding a paragraph after its options, which its content rule refuses, and
 *  Dispatch's refusal of it, its upload's and its round trip's alike. */
const ASK_AFTER_OPTIONS =
  ':::ask{#a1 urgency="med" multiple="false" state="open"}\nWhich database?\n\n- Postgres\n- SQLite\n\nPick one by Friday.\n:::\n';
const ASK_AFTER_OPTIONS_REFUSED = {
  code: "INVALID_ASK_BLOCK",
  error:
    'ask block "a1", asking "Which database?", holds a paragraph after its options where its content rule, paragraph+ bullet_list?, allows one or more paragraphs and then at most one bullet list, last',
};
/** Block HTML, which the parser refuses, and Dispatch's refusal of it. */
const BLOCK_HTML = "<details>x</details>";
const BLOCK_HTML_REFUSED = {
  code: "INVALID_MARKDOWN",
  error:
    "markdown is not a Proof document: outside Proof schema: block HTML is not accepted by Proof",
};

/** A text past the bytes one document holds, counted in UTF-8 as Dispatch counts them: each "é"
 *  is one UTF-16 unit and two bytes, so the text's length is under the limit and its bytes are
 *  past it. */
const PAST_DOCUMENT_LIMIT = `# Notes\n\n${"é".repeat(MARKDOWN_DOCUMENT_MAX_BYTES / 2)}\n`;
/** The refusal of such a text, in Dispatch's own words, as the call and its dry run print it. */
const DOCUMENT_TOO_LARGE =
  "CAP_EXCEEDED: a markdown document is at most 1 MiB; a larger file is stored as a binary artifact under another content type";
/** Dispatch's refusals of a byte that is not UTF-8 and of a NUL in `field`, at `character`, as the
 *  call and its dry run print them. */
const notUtf8 = (field: string, byte: string, character: number): string =>
  `INVALID_UTF8: ${field} holds a byte that is not UTF-8 (${byte}) at character ${character}, which Dispatch cannot store\n`;
const nul = (field: string, character: number): string =>
  `NUL_CHARACTER: ${field} holds a NUL character (U+0000) at character ${character}, which Dispatch cannot store\n`;
/** 12,000 list items, 48 KB: more elements than one document may hold. */
const MANY_ITEMS = "- a\n".repeat(12_000);
/** Dispatch's 413 for those items, its upload's and its round trip's alike, in the words the
 *  server's parse gives it (markdown_round_trip_test.go measures the two equal). */
const TOO_MANY_ELEMENTS = {
  code: "CAP_EXCEEDED",
  error:
    "document too large to store: this write's markdown makes more than 65536 elements, passing that at line 9362; a block weighs 3 elements, the empty block an empty container is read as holding 3, a table cell 4, a hard line break 3, an image 3, a piece of inline HTML 2, an autolink 2, a footnote reference 2, an escape 1, and each piece of inline syntax, mark and line of text 1. Shorten the change, or split the document; an upload of data rather than prose can go up as a file of another content type",
};
/** 20,000 paragraphs of a no-break space, which Go's strings.TrimSpace empties, so the text is
 *  blank, and which make more elements than one document may hold. */
const BLANK_PAST_ELEMENTS = "\u00a0\n\n".repeat(20_000);
/** Dispatch's 413 for those paragraphs, in the words markdown_round_trip_test.go measured. */
const BLANK_TOO_MANY_ELEMENTS = {
  code: "CAP_EXCEEDED",
  error: TOO_MANY_ELEMENTS.error.replace("line 9362", "line 32769"),
};
/** Markdown whose rendering no reader reads back, and Dispatch's refusal of it, its upload's and
 *  its round trip's alike, in the words markdown_round_trip_test.go measured. */
const UNREADABLE_RENDERING = "2. [^N]:\n       12. *\n       --\n";
const UNREADABLE_RENDERING_REFUSED = {
  code: "INVALID_MARKDOWN",
  error:
    "markdown is not a Proof document: the markdown the document would be stored as cannot be read back: outside Proof schema: an empty list item followed by a blank line inside another list item, where goldmark ends the outer item and the browser editor does not",
};
/** The round trip's refusal of a text, as Dispatch answers it: `answer` at `status`, marked as a
 *  refusal of the text (TEXT_REFUSAL_HEADER, which Dispatch values `text`). */
const textRefusal = (answer: { code: string; error: string }, status: number): Response =>
  Response.json(answer, { status, headers: { [TEXT_REFUSAL_HEADER]: "text" } });

/** The `hint` Dispatch's answer to a path no route serves carries (routes/router.go's
 *  staticHandler), which the CLI prints after the message (`messageFor`). */
const NO_ROUTE_HINT = "GET /api/v1 lists every route";

/** The stderr line of an upload's dry run against a Dispatch with no round-trip route. */
const routeAbsent = (answer: string): string =>
  `dispatch: warning: this Dispatch cannot check the text: POST ${ROUND_TRIP} failed with ${answer}. The call uploads the text unchecked.\n`;

/** The stderr line of an upload's dry run whose round trip could not run, for `why`. */
const unasked = (why: string): string =>
  `dispatch: warning: the dry run could not ask Dispatch about the text: ${why}. The call refuses the upload until Dispatch answers.\n`;

/** A `dispatch result` record of check k1 of DSP-1, its arguments as the command parses them. */
const RESULT_ARGS = {
  issue: "DSP-1",
  check: "k1",
  role: "implementer",
  verdict: "pass",
  commit: "1111111111111111111111111111111111111111",
};
const RESULT_ARGV = [
  "result",
  ...Object.entries(RESULT_ARGS).flatMap(([field, value]) => [`--${field}`, value]),
];

describe("the dispatch CLI", () => {
  test("refuses without DISPATCH_HOST, naming it, and sends nothing", async () => {
    const { code, stdout } = await run(["message", "--issue", "DSP-1", "--body", "x"], {
      DISPATCH_SESSION_ID: "ses-1",
    });
    expect(code).toBe(2);
    expect(stdout).toContain("DISPATCH_HOST");
    expect(requests).toEqual([]);
  });

  test("Claude Code's own session id wins over an inherited DISPATCH_SESSION_ID", async () => {
    const { code } = await run(["message", "--issue", "DSP-1", "--body", "hello"], {
      DISPATCH_HOST: "claude",
      DISPATCH_SESSION_ID: "a",
      CLAUDE_CODE_SESSION_ID: "b",
    });
    expect(code).toBe(0);
    const posted = requests.find((request) => request.path === "/api/v1/issues/DSP-1/messages");
    expect(posted?.body).toMatchObject({ actor: { id: "b" } });
  });

  test("under Claude Code, claude-envoy's ENVOY_SESSION_ID override is the session, as on its claim", async () => {
    const { code } = await run(["message", "--issue", "DSP-1", "--body", "hello"], {
      DISPATCH_HOST: "claude",
      ENVOY_SESSION_ID: "qa-ses",
      CLAUDE_CODE_SESSION_ID: "native-ses",
    });
    expect(code).toBe(0);
    const posted = requests.find((request) => request.path === "/api/v1/issues/DSP-1/messages");
    expect(posted?.body).toMatchObject({ actor: { id: "qa-ses" } });
    expect(ledger("qa-ses").map((line) => line.tool)).toEqual(["dispatch_message"]);
    expect(existsSync(join(stateDir, "sessions", "native-ses"))).toBe(false);

    requests.length = 0;
    const unset = await run(["message", "--issue", "DSP-1", "--body", "x"], {
      DISPATCH_HOST: "claude",
    });
    expect(unset.code).toBe(2);
    expect(unset.stdout).toContain("ENVOY_SESSION_ID or CLAUDE_CODE_SESSION_ID is not set");
    expect(requests).toEqual([]);
  });

  test("handoff-write records the body file's text as written, and handoff-read prints it back exactly", async () => {
    // Spacing, key order and a trailing newline a re-serialization would lose.
    const text = `{ "schemaVersion": 1,\n  "b": [2, 1], "a": "é\\u0041" }\n`;
    const file = join(stateDir, "DSP-1-implement.json");
    writeFileSync(file, text);
    const first = await run(
      ["handoff-write", "DSP-1", "--phase", "implement", "--body-file", file],
      omp
    );
    expect(first).toMatchObject({ code: 0, stderr: "" });
    expect(first.stdout).toBe(
      "Recorded the implement handoff of DSP-1, version 1 (id 00000000-0000-4000-8000-000000000001)\n"
    );
    const second = await run(
      ["handoff-write", "DSP-1", "--phase", "implement", "--body-file", "-"],
      omp,
      '{"round":2}'
    );
    expect(second.stdout).toContain("version 2 (id 00000000-0000-4000-8000-000000000002)");
    const posted = requests.find((request) => request.method === "POST");
    expect(posted?.body).toMatchObject({ actor: { kind: "session", id: "ses-1" }, body: text });

    const latest = await run(["handoff-read", "DSP-1", "--phase", "implement"], omp);
    expect(latest.code).toBe(0);
    expect(latest.stdout).toBe(
      'DSP-1 implement handoff, version 2 (id 00000000-0000-4000-8000-000000000002), written by session ses-1 at 2026-10-26T00:00:02Z\n{"round":2}\n'
    );
    const versionOne = await run(
      ["handoff-read", "DSP-1", "--phase", "implement", "--version", "1"],
      omp
    );
    expect(versionOne.code).toBe(0);
    const [line, ...rest] = versionOne.stdout.split("\n");
    expect(line).toBe(
      "DSP-1 implement handoff, version 1 (id 00000000-0000-4000-8000-000000000001), written by session ses-1 at 2026-10-26T00:00:01Z"
    );
    expect(rest.join("\n")).toBe(text);

    const missing = await run(
      ["handoff-read", "DSP-1", "--phase", "implement", "--version", "3"],
      omp
    );
    expect(missing.code).toBe(1);
    expect(missing.stdout).toContain(
      "HANDOFF_NOT_FOUND: issue DSP-1 has no implement handoff version 3; its newest implement handoff is version 2"
    );
  });

  test("handoff-write refuses a body that is not one JSON object, dry run included, sending nothing", async () => {
    for (const argv of [
      ["handoff-write", "DSP-1", "--phase", "plan", "--body-file", "-"],
      ["handoff-write", "DSP-1", "--phase", "plan", "--body-file", "-", "--dry-run"],
    ]) {
      const { code, stdout } = await run(argv, omp, "[1, 2]\n");
      expect(code).toBe(2);
      expect(stdout.split("\n")[0]).toBe("dispatch handoff-write was not called: 1 problem");
      expect(stdout).toContain("body is not one JSON object");
    }
    expect(requests).toEqual([]);
  });

  test("an empty session id counts as unset", async () => {
    const { code, stdout } = await run(["message", "--issue", "DSP-1", "--body", "x"], {
      DISPATCH_HOST: "omp",
      DISPATCH_SESSION_ID: "",
    });
    expect(code).toBe(2);
    expect(stdout).toContain("DISPATCH_SESSION_ID");
    expect(requests).toEqual([]);
  });

  // The CLI asks a route no build answered three more times, 1, 2 and 4 s apart (jittered), before
  // it says so: the test waits those seconds.
  test("answers against a Dispatch without the route says its build is older than the answers route, exit 1", async () => {
    const { code, stdout } = await run(["answers"], omp);
    expect(code).toBe(1);
    expect(stdout).toBe(
      `This Dispatch (http://127.0.0.1:${server.port}) has no answers route yet: its build is older than the answers route.\n`
    );
    expect(requests.map((request) => request.path)).toEqual(
      Array(4).fill("/api/v1/agents/ses-1/answers")
    );
  }, 20_000);

  test("an unknown flag is refused in command syntax, with an example", async () => {
    const { code, stdout } = await run(["message", "--issue", "X", "--isue", "Y"], omp);
    expect(code).toBe(2);
    expect(stdout.split("\n")[0]).toBe("dispatch message was not called: 1 problem");
    expect(stdout).toContain("- Example: dispatch message");
    expect(requests).toEqual([]);
  });

  // `--run -` would send the one character "-" where the caller meant stdin, so a text flag's bare
  // `-` is a usage error before any request, dry run included. When nothing else is wrong, the
  // refusal names the `-file` twin of each such flag and lists no flags.
  test("a bare - given to text flags is refused, one line per flag, listing no flags", async () => {
    const argv = [
      "result",
      "--issue",
      "DSP-1",
      "--check",
      "k1",
      "--role",
      "implementer",
      "--verdict",
      "pass",
      "--commit",
      "1111111111111111111111111111111111111111",
      "--run",
      "-",
      "--observed",
      "-",
      "--negative-control",
      "-",
    ];
    const refusal = [
      "dispatch result was not called: 3 problems",
      '- --run - sends the one character "-". --run-file <path> reads --run from a file, and --run-file - reads it from stdin. One flag of a call may read stdin.',
      '- --observed - sends the one character "-". --observed-file <path> reads --observed from a file, and --observed-file - reads it from stdin. One flag of a call may read stdin.',
      '- --negative-control - sends the one character "-". --negative-control-file <path> reads --negative-control from a file, and --negative-control-file - reads it from stdin. One flag of a call may read stdin.',
      "- Example: dispatch result --issue DSP-1 --check k1 --role implementer --verdict pass --commit 1111111111111111111111111111111111111111 --negative-control 'Removed the guard and the test failed.'",
      "",
    ].join("\n");
    for (const call of [argv, [...argv, "--dry-run"]]) {
      expect(await run(call, omp), call.join(" ")).toEqual({
        code: 2,
        stdout: refusal,
        stderr: "",
      });
    }
    expect(requests).toEqual([]);

    expect(await run(["issue", "--project", "DSP", "--title", "-"], omp)).toEqual({
      code: 2,
      stdout: [
        "dispatch issue was not called: 1 problem",
        '- --title - sends the one character "-". --title-file <path> reads --title from a file, and --title-file - reads it from stdin. One flag of a call may read stdin.',
        "- Example: dispatch issue --project DSP --title 'Native workspace'",
        "",
      ].join("\n"),
      stderr: "",
    });
    expect(requests).toEqual([]);
  });

  test("a bare - beside an unknown flag is refused with both, listing the flags", async () => {
    const { code, stdout } = await run(
      ["issue", "--project", "DSP", "--title", "-", "--titel", "x"],
      omp
    );
    expect(code).toBe(2);
    const lines = stdout.split("\n");
    expect(lines.slice(0, 3)).toEqual([
      "dispatch issue was not called: 2 problems",
      '- --title - sends the one character "-". --title-file <path> reads --title from a file, and --title-file - reads it from stdin. One flag of a call may read stdin.',
      "- unknown flag --titel",
    ]);
    expect(lines[3]).toStartWith("- Allowed flags: --project, ");
    expect(lines[4]).toStartWith("- Example: dispatch issue ");
    expect(requests).toEqual([]);
  });

  // `skill://dispatch` says "Exit 2 is a usage error" and exit 1 any other failure, so a call
  // Dispatch never saw because an argument is missing exits 2: a script reading the code alone
  // tells it from a write Dispatch refused (a near copy, an AskGate block), which exits 1.
  test("a command missing a required argument is a usage error, exit 2", async () => {
    const { code, stdout } = await run(["issue", "--project", "DSP"], omp);
    expect(code).toBe(2);
    expect(stdout.split("\n")[0]).toBe("dispatch issue was not called: 1 problem");
    expect(stdout).toContain("title");
    expect(requests).toEqual([]);
  });

  test("--dry-run prints the arguments and sends nothing", async () => {
    const { code, stdout } = await run(
      ["message", "--dry-run", "--issue", "DSP-1", "--body-file", "-"],
      omp,
      "line one\nline two\n"
    );
    expect(code).toBe(0);
    expect(JSON.parse(stdout)).toEqual({ issue: "DSP-1", body: "line one\nline two\n" });
    expect(requests).toEqual([]);
  });

  // --reason is the close's note: a reopen (or any update but a close) that carries one is
  // refused by the argument check, in a dry run as in the call, naming that rule, before any
  // request, so no stray note is ever posted.
  test("--reason without --status done is refused by the argument check, dry run included", async () => {
    const argv = ["issue-update", "--issue", "DSP-1", "--status", "in_progress", "--reason", "x"];
    const rule =
      "- reason goes only with status done: it is the note posted on the issue as it closes, and status in_progress closes nothing. Drop reason, or set status done to close the issue.";
    for (const call of [[...argv, "--dry-run"], argv]) {
      const { code, stdout } = await run(call, omp);
      expect(code, call.join(" ")).toBe(2);
      expect(stdout.split("\n").slice(0, 2)).toEqual([
        "dispatch issue-update was not called: 1 problem",
        rule,
      ]);
    }
    expect(requests).toEqual([]);
  });

  // A record's dry run asks Dispatch, on a path of its own, for the pull-request body line the
  // record would print, and prints it after the arguments: the line is Dispatch's to write, so
  // the CLI prints the one Dispatch answered and builds none. Nothing is recorded, and this
  // session's ledger keeps no entry, as for every dry run that passed.
  test("dispatch result --dry-run prints the pull-request body line Dispatch answers and records nothing", async () => {
    const observed = "DSP-1's open-items v14 lists every row.";
    const { code, stdout } = await run([...RESULT_ARGV, "--observed", observed, "--dry-run"], omp);
    expect(code, stdout).toBe(0);
    const args = { ...RESULT_ARGS, observed };
    expect(stdout).toBe(
      `${JSON.stringify(args, null, 2)}\n\nNothing was recorded: this is a dry run.\n\nPull request body line:\n${RESULT_DRY_RUN_LINE}\n`
    );
    expect(requests.map((request) => `${request.method} ${request.path}`)).toEqual([
      "POST /api/v1/issues/DSP-1/results/dry-run",
    ]);
    const { issue: _, ...body } = args;
    expect(requests[0]?.body).toMatchObject(body);
    expect(existsSync(join(stateDir, "sessions", "ses-1", "results.jsonl"))).toBe(false);
  });

  // The dry run needs Dispatch as the record does, so with Dispatch off it is refused in the
  // record's own words and exit code, and sends nothing.
  test("dispatch result --dry-run with Dispatch off is refused as the record is", async () => {
    const off = { ...omp, DISPATCH_URL: undefined, DISPATCH_TOKEN: undefined };
    const record = await run(RESULT_ARGV, off);
    const dry = await run([...RESULT_ARGV, "--dry-run"], off);
    expect(record.code).toBe(2);
    expect(record.stdout).toStartWith("dispatch: ");
    expect(dry).toEqual(record);
    expect(requests).toEqual([]);
  });

  test("dispatch result --retract --dry-run prints the arguments and sends nothing", async () => {
    const { code, stdout } = await run(
      ["result", "--retract", "res-2", "--reason", "The run was wrong.", "--dry-run"],
      omp
    );
    expect(code, stdout).toBe(0);
    expect(JSON.parse(stdout)).toEqual({ retract: "res-2", reason: "The run was wrong." });
    expect(requests).toEqual([]);
  });

  // No length check stands between the body and Dispatch: the whole text is posted, and what the
  // person reads is the server's refusal, printed as it came.
  test("a body with a block over the server's bound is posted and refused in the server's words", async () => {
    const body = "x".repeat(DISPATCH_BLOCK_UNITS + 1);

    const { code, stdout } = await run(
      ["comment", "--issue", "DSP-1", "--body-file", "-"],
      omp,
      body
    );

    expect(code).toBe(1);
    expect(stdout.trim()).toBe(printedBlockCapRefusal(1, DISPATCH_BLOCK_UNITS + 1));
    const posted = requests.find((request) => request.path === "/api/v1/issues/DSP-1/comments");
    expect(posted?.body).toMatchObject({ body });
  });

  test("a dry run of the same body prints the arguments and posts nothing", async () => {
    const body = "x".repeat(DISPATCH_BLOCK_UNITS + 1);

    const { code, stdout } = await run(
      ["comment", "--dry-run", "--issue", "DSP-1", "--body-file", "-"],
      omp,
      body
    );

    expect(code).toBe(0);
    expect(JSON.parse(stdout)).toEqual({ issue: "DSP-1", body });
    expect(requests).toEqual([]);
  });

  // --content and --content-file store their text as a Markdown document, and a create with
  // neither makes an empty one, so under a .jsonl name the call is refused before any request,
  // dry run included, naming --path.
  test("artifact refuses --content-file under a name that is not Markdown, naming --path", async () => {
    const argv = ["artifact", "--issue", "DSP-1", "--name", "rows.jsonl", "--content-file", "-"];
    for (const call of [[...argv, "--dry-run"], argv]) {
      const { code, stdout } = await run(call, omp, '{"row_id": 1}\n{"row_id": 2}\n');
      expect(code, call.join(" ")).toBe(2);
      expect(stdout.split("\n").slice(0, 2)).toEqual([
        "dispatch artifact was not called: 1 problem",
        `- a document holds Markdown, which rewrites other text (a JSONL file's lines come back joined into one paragraph), and "rows.jsonl" is not a Markdown name. Upload the file under the same name with path (--path <file>), which stores a file whose path does not end in .md or .markdown as its bytes, unchanged; a document takes a name ending in .md or .markdown or with no extension.`,
      ]);
    }
    expect(requests).toEqual([]);
  });

  // --path is what the refusal above names, so it must do what the refusal promises: the file's
  // bytes go to Dispatch as they are, the fifteen lines of a JSONL file among them.
  test("artifact --path sends the file's bytes unchanged", async () => {
    writeFileSync(join(stateDir, "rows.jsonl"), rows);

    const { code, stdout } = await run(
      ["artifact", "--issue", "DSP-1", "--name", "rows.jsonl", "--path", "rows.jsonl"],
      omp
    );

    expect(code, stdout).toBe(0);
    expect(stdout).toContain("Uploaded rows.jsonl as version 1");
    // The upload itself, past the read of what the issue holds that runs before it.
    const uploaded = requests.filter(
      (request) => request.path.endsWith("/artifacts") && request.method === "POST"
    );
    expect(uploaded.map((request) => request.body)).toEqual([rows]);
  });

  // INFRA-370: `dispatch comment` and `dispatch message` carry no length of their own. A body of
  // 40,000 characters in paragraphs posts whole, because Dispatch bounds a body block by block,
  // and the command sends the text it was handed, not a cut of it.
  test("a comment and a message of 40,000 characters in paragraphs are posted whole", async () => {
    const whole = paragraphsOf(40_000);

    for (const command of ["comment", "message"] as const) {
      const { code, stdout } = await run([command, "--issue", "DSP-1", "--body", whole], omp);
      expect(code, stdout).toBe(0);
    }

    const posted = requests.filter((request) => request.method === "POST");
    expect(posted.map((request) => request.path)).toEqual([
      "/api/v1/issues/DSP-1/comments",
      "/api/v1/issues/DSP-1/messages",
    ]);
    for (const request of posted) {
      expect(request.body, request.path).toMatchObject({ body: whole });
    }
  });

  // A dry run runs the call's own pre-send check, so a call it refuses is refused alike, before
  // any request: the owner resolved from LEGION_ISSUE or left missing, a ref naming another
  // owner, a picture file that is not there.
  test("--dry-run refuses exactly what the call refuses before sending", async () => {
    const cases: { argv: string[]; env?: Record<string, string> }[] = [
      { argv: ["issue-update", "--status", "done"], env: { LEGION_ISSUE: "DSP-1" } },
      { argv: ["issue-update", "--status", "done"] },
      { argv: ["comment", "--issue", "DSP-2", "--ref", "dispatch://DSP-1", "--body", "x"] },
      { argv: ["message", "--issue", "DSP-1", "--body", "x", "--image", "missing.png"] },
    ];
    for (const { argv, env } of cases) {
      const call = await run(argv, { ...omp, ...env });
      const dry = await run([...argv, "--dry-run"], { ...omp, ...env });
      expect(call.code, argv.join(" ")).toBe(2);
      expect(call.stdout, argv.join(" ")).toStartWith("dispatch ");
      expect(dry, argv.join(" ")).toEqual(call);
    }
    expect(requests).toEqual([]);
  });

  // What the call fills before its check (the issue from LEGION_ISSUE, the document a ref names,
  // a dashboard URL on this server as its dispatch:// form) passes a dry run too, which prints
  // the caller's own arguments.
  test("--dry-run passes what the call resolves, printing the caller's arguments", async () => {
    const dashboard = `http://127.0.0.1:${server.port}/issues/DSP-1/spec`;
    const cases: { argv: string[]; env?: Record<string, string>; args: object }[] = [
      { argv: ["message", "--body", "hi"], env: { LEGION_ISSUE: "DSP-1" }, args: { body: "hi" } },
      ...["dispatch://DSP-1/spec", dashboard].map((ref) => ({
        argv: ["comment", "--ref", ref, "--quote", "old", "--body", "why"],
        args: { ref, quote: "old", body: "why" },
      })),
    ];
    for (const { argv, env, args } of cases) {
      const { code, stdout } = await run([...argv, "--dry-run"], { ...omp, ...env });
      expect(code, `${argv.join(" ")}\n${stdout}`).toBe(0);
      expect(JSON.parse(stdout)).toEqual(args);
    }
    expect(requests).toEqual([]);
  });

  // A dry run of a command with no dry-run step reads nothing from Dispatch, so it checks and
  // prints with Dispatch off too, and prints the JSON alone.
  test("--dry-run checks and prints with Dispatch off", async () => {
    const off = { ...omp, DISPATCH_URL: undefined, DISPATCH_TOKEN: undefined };
    const passed = await run(["message", "--issue", "DSP-1", "--body", "hi", "--dry-run"], off);
    expect(passed.code).toBe(0);
    expect(JSON.parse(passed.stdout)).toEqual({ issue: "DSP-1", body: "hi" });
    const refused = await run(
      ["issue-update", "--issue", "DSP-1", "--status", "in_progress", "--reason", "x", "--dry-run"],
      off
    );
    expect(refused.code).toBe(2);
    expect(refused.stdout).toStartWith(
      "dispatch issue-update was not called: 1 problem\n- reason goes only with status done"
    );
    expect(requests).toEqual([]);
  });

  // INFRA-4224 R3: an upload's dry run asks Dispatch how it would store its inline text, as the
  // call does, and is refused for the text's spelling exactly as the call is: the same exit, the
  // same refusal on the same stream. It stores nothing.
  test("an upload's dry run refuses a text Dispatch would respell, exactly as the call does", async () => {
    roundTrip = (markdown) => Response.json({ markdown: markdown.replace("* one", "- one") });
    const dry = await run([...UPLOAD, "--dry-run"], omp, NOTES);
    expect(requests).toEqual([{ method: "POST", path: ROUND_TRIP, body: markdownBody(NOTES) }]);
    requests.length = 0;

    const call = await run(UPLOAD, omp, NOTES);
    expect(call.code).toBe(2);
    expect(call.stdout).toStartWith(
      'dispatch artifact was not called: 1 problem\n- Dispatch stores this text under another spelling: line 3 reads the word "*" at character 1, where Dispatch stores the word "-".'
    );
    expect(dry).toEqual(call);
    // The call refused it before its upload, as the dry run did.
    expect(requests.filter((request) => request.method === "POST")).toEqual([
      { method: "POST", path: ROUND_TRIP, body: markdownBody(NOTES) },
    ]);
  });

  // A text past the bytes one document holds (PAST_DOCUMENT_LIMIT). The call refuses it in
  // Dispatch's own words before it sends anything, the read of whether the issue holds the name
  // included, and its dry run alike: the same exit, the same stdout, and the same line in this
  // session's ledger.
  test("an upload's dry run refuses a text past the document limit in UTF-8 bytes as the call does, sending nothing", async () => {
    expect(PAST_DOCUMENT_LIMIT.length).toBeLessThan(MARKDOWN_DOCUMENT_MAX_BYTES);
    expect(Buffer.byteLength(PAST_DOCUMENT_LIMIT, "utf8")).toBeGreaterThan(
      MARKDOWN_DOCUMENT_MAX_BYTES
    );

    const dry = await run([...UPLOAD, "--dry-run"], omp, PAST_DOCUMENT_LIMIT);
    expect(requests).toEqual([]);
    const call = await run(UPLOAD, omp, PAST_DOCUMENT_LIMIT);
    expect(call).toEqual({ code: 1, stdout: `${DOCUMENT_TOO_LARGE}\n`, stderr: "" });
    expect(dry).toEqual(call);
    expect(requests).toEqual([]);
    expect(ledger("ses-1").map(({ tool, error }) => ({ tool, error }))).toEqual([
      { tool: "dispatch_artifact", error: DOCUMENT_TOO_LARGE },
      { tool: "dispatch_artifact", error: DOCUMENT_TOO_LARGE },
    ]);
  });

  // Dispatch refuses an upload past the document limit whatever name it carries, so the call
  // refuses such a text for its size before it reads whether the issue holds the name, and
  // answers it under a held name as its dry run, which reads no held name, does: the same exit and
  // the same stdout, and neither sends anything. A text within the limit under that name is
  // refused for the name, which shows the issue holds it.
  test("an upload past the document limit under a name the issue holds is refused for its size, by the call as by its dry run", async () => {
    issueArtifacts = [NOTES_HELD];
    const named = await run(UPLOAD, omp, NOTES);
    expect(named.code).toBe(2);
    expect(named.stdout).toStartWith(
      "dispatch artifact was not called: 1 problem\n- issue DSP-1 already holds notes.md"
    );
    requests.length = 0;

    const dry = await run([...UPLOAD, "--dry-run"], omp, PAST_DOCUMENT_LIMIT);
    expect(requests).toEqual([]);
    const call = await run(UPLOAD, omp, PAST_DOCUMENT_LIMIT);
    expect(call).toEqual({ code: 1, stdout: `${DOCUMENT_TOO_LARGE}\n`, stderr: "" });
    expect(dry).toEqual(call);
    expect(requests).toEqual([]);
  });

  // A text whose round trip Dispatch marks as its refusal of the text (TEXT_REFUSAL_HEADER): here
  // its refusal of markdown whose rendering no reader reads back, which its upload answers too.
  // The call refuses the text with Dispatch's own refusal, printed as it prints the upload's
  // (`INVALID_MARKDOWN: <message>`), under a name that keeps the bytes too, asking once, and its dry
  // run alike, with the same exit and the same stdout. Neither uploads.
  test("an upload's dry run refuses a text whose round trip Dispatch marks as refused as the call does", async () => {
    roundTrip = () => textRefusal(UNREADABLE_RENDERING_REFUSED, 400);
    for (const name of ["notes.md", "notes"]) {
      const argv = ["artifact", "--issue", "DSP-1", "--name", name, "--content-file", "-"];
      requests.length = 0;
      const dry = await run([...argv, "--dry-run"], omp, UNREADABLE_RENDERING);
      expect(requests, name).toEqual([
        { method: "POST", path: ROUND_TRIP, body: markdownBody(UNREADABLE_RENDERING) },
      ]);
      requests.length = 0;
      const call = await run(argv, omp, UNREADABLE_RENDERING);
      expect(call, name).toEqual({
        code: 1,
        stdout: `${UNREADABLE_RENDERING_REFUSED.code}: ${UNREADABLE_RENDERING_REFUSED.error}\n`,
        stderr: "",
      });
      expect(dry, name).toEqual(call);
      expect(
        requests.filter((request) => request.method === "POST"),
        name
      ).toEqual([{ method: "POST", path: ROUND_TRIP, body: markdownBody(UNREADABLE_RENDERING) }]);
    }
  });

  // An answer the round trip does not mark says nothing of the text, whatever its status: here the
  // route's own 413 for a body past its bound, which no text within the document's bytes reaches.
  // The call refuses the upload until Dispatch answers, as for any failure of the check, and its
  // dry run passes, saying so on stderr.
  test("an upload's dry run passes a round trip refusal Dispatch does not mark, which the call reads as no answer", async () => {
    roundTrip = () =>
      Response.json(
        { code: "CAP_EXCEEDED", error: "request body exceeds 6292480 bytes" },
        { status: 413 }
      );
    const failure = `POST ${ROUND_TRIP} failed with CAP_EXCEEDED: request body exceeds 6292480 bytes`;
    const dry = await run([...UPLOAD, "--dry-run"], omp, NOTES);
    expect(dry).toEqual({ code: 0, stdout: NOTES_JSON, stderr: unasked(failure) });
    const call = await run(UPLOAD, omp, NOTES);
    expect(call).toEqual({
      code: 1,
      stdout: `Dispatch could not say how it would store this text, so nothing was uploaded: ${failure}. Send the upload again once that call is answered.\n`,
      stderr: "",
    });
    expect(requests.filter((request) => request.method === "POST")).toEqual([
      { method: "POST", path: ROUND_TRIP, body: markdownBody(NOTES) },
      { method: "POST", path: ROUND_TRIP, body: markdownBody(NOTES) },
    ]);
  });

  // A markdown file sent with --path is stored as a document, so Dispatch refuses one past the
  // bytes one document holds with the same CAP_EXCEEDED as inline text (verified on a local
  // stack: the call exits 1 with DOCUMENT_TOO_LARGE). Its dry run refuses it with the call's code
  // and message, as R3 of INFRA-4224 asks for a text the call refuses for its size, and uploads
  // nothing. The tester found at c66bcb82 that the dry run printed the arguments and exited 0.
  test("an upload's dry run refuses a markdown --path file past the document limit with the call's code and message", async () => {
    writeFileSync(join(stateDir, "big.md"), PAST_DOCUMENT_LIMIT);
    const argv = ["artifact", "--issue", "DSP-1", "--name", "notes.md", "--path", "big.md"];

    const dry = await run([...argv, "--dry-run"], omp);
    expect(dry).toEqual({ code: 1, stdout: `${DOCUMENT_TOO_LARGE}\n`, stderr: "" });
    expect(requests.filter((request) => request.method === "POST")).toEqual([]);
  });

  // The same file under a name the issue holds: the call refuses it for its size before it reads
  // whether the issue holds the name, as it refuses inline text, so it sends no request at all,
  // and its dry run, which reads no held name, answers it alike.
  test("a markdown --path file past the document limit under a name the issue holds is refused for its size by the call and its dry run, the call sending nothing", async () => {
    issueArtifacts = [NOTES_HELD];
    writeFileSync(join(stateDir, "big.md"), PAST_DOCUMENT_LIMIT);
    const argv = ["artifact", "--issue", "DSP-1", "--name", "notes.md", "--path", "big.md"];

    const call = await run(argv, omp);
    expect(call).toEqual({ code: 1, stdout: `${DOCUMENT_TOO_LARGE}\n`, stderr: "" });
    expect(requests).toEqual([]);
    const dry = await run([...argv, "--dry-run"], omp);
    expect(dry).toEqual(call);
    expect(requests).toEqual([]);
  });

  // INFRA-4224 R3: 12,000 list items, 48 KB, make more elements than one document may hold.
  // Dispatch's upload refuses them 413 CAP_EXCEEDED, and so does its round trip, in the upload's
  // own words (markdownRoundTrip in packages/envoy/internal/dispatch/api), which this fake
  // answers. The call refuses the inline text with that refusal before its upload, and its dry run
  // alike: the same exit and the same stdout.
  test("an upload's dry run refuses inline text past the element limit with Dispatch's refusal, as the call does, and neither uploads", async () => {
    roundTrip = () => textRefusal(TOO_MANY_ELEMENTS, 413);
    const dry = await run([...UPLOAD, "--dry-run"], omp, MANY_ITEMS);
    const call = await run(UPLOAD, omp, MANY_ITEMS);
    expect(call).toEqual({
      code: 1,
      stdout: `CAP_EXCEEDED: ${TOO_MANY_ELEMENTS.error}\n`,
      stderr: "",
    });
    expect(dry).toEqual(call);
    expect(requests.filter((request) => request.method === "POST")).toEqual([
      { method: "POST", path: ROUND_TRIP, body: markdownBody(MANY_ITEMS) },
      { method: "POST", path: ROUND_TRIP, body: markdownBody(MANY_ITEMS) },
    ]);
  });

  // The same items in a markdown file sent with --path: Dispatch stores it as a document and
  // refuses its upload with that refusal, and its file round trip alike, in the upload's own words
  // (markdown_round_trip_test.go measures the upload's refusal and the round trip's equal). The dry
  // run and the call each ask the round trip about the file's bytes and refuse it with that
  // refusal, the same stdout and exit 1, before anything is uploaded: this fake's upload would
  // take the file, and neither reaches it.
  test("an upload's dry run refuses a markdown --path file past the element limit with Dispatch's refusal, as the call does, and neither uploads", async () => {
    roundTrip = () => textRefusal(TOO_MANY_ELEMENTS, 413);
    writeFileSync(join(stateDir, "items.md"), MANY_ITEMS);
    const argv = ["artifact", "--issue", "DSP-1", "--name", "notes.md", "--path", "items.md"];

    const dry = await run([...argv, "--dry-run"], omp);
    expect(dry).toEqual({
      code: 1,
      stdout: `CAP_EXCEEDED: ${TOO_MANY_ELEMENTS.error}\n`,
      stderr: "",
    });
    expect(requests).toEqual([
      { method: "POST", path: FILE_ROUND_TRIP, body: fileBody(MANY_ITEMS) },
    ]);
    requests.length = 0;
    const call = await run(argv, omp);
    expect(call).toEqual(dry);
    expect(requests).toEqual([
      { method: "POST", path: FILE_ROUND_TRIP, body: fileBody(MANY_ITEMS) },
    ]);
  });

  // A markdown file is stored as the store spells it, with no refusal for its spelling: the dry
  // run and the call ask the round trip only for a refusal, so a file the store would respell
  // passes the dry run, and the call uploads its bytes.
  test("an upload's dry run of a markdown --path file Dispatch would respell passes, as the call stores it", async () => {
    roundTrip = (markdown) => Response.json({ markdown: markdown.replace("* one", "- one") });
    writeFileSync(join(stateDir, "notes.md"), NOTES);
    const argv = ["artifact", "--issue", "DSP-1", "--name", "notes.md", "--path", "notes.md"];

    const dry = await run([...argv, "--dry-run"], omp);
    expect(dry).toEqual({
      code: 0,
      stdout: `${JSON.stringify({ issue: "DSP-1", name: "notes.md", path: "notes.md" }, null, 2)}\n`,
      stderr: "",
    });
    expect(requests).toEqual([{ method: "POST", path: FILE_ROUND_TRIP, body: fileBody(NOTES) }]);
    requests.length = 0;
    const call = await run(argv, omp);
    expect(call.code, call.stdout).toBe(0);
    expect(requests.filter((request) => request.method === "POST")).toEqual([
      { method: "POST", path: FILE_ROUND_TRIP, body: fileBody(NOTES) },
      { method: "POST", path: "/api/v1/issues/DSP-1/artifacts", body: NOTES },
    ]);
  });

  // INFRA-4224 R3 (spec v24): Dispatch's round trip refuses, in its upload's own status, code and
  // words, every text that upload refuses for the text alone (markdown_round_trip_test.go measures
  // the two equal, and these are the words it measured). A markdown --path file holding one that
  // the command does not refuse itself - its size, a U+0000 and a byte that is not UTF-8, which it
  // refuses before it asks (below) - is refused by the dry run and by the call alike, each from the
  // file round trip it sends the file's bytes to, with the same stdout and exit 1. The call
  // refuses it before it reads whether the issue holds the name and before its upload, which this
  // fake would take: it sends nothing else.
  for (const refusal of [
    {
      holding: "an ask block that breaks its content rule",
      bytes: Buffer.from(ASK_AFTER_OPTIONS),
      ...ASK_AFTER_OPTIONS_REFUSED,
    },
    {
      holding: "markdown the parser refuses",
      bytes: Buffer.from(BLOCK_HTML),
      ...BLOCK_HTML_REFUSED,
    },
  ]) {
    test(`a markdown --path file holding ${refusal.holding} is refused by the call as by its dry run, and neither uploads`, async () => {
      const answer = { code: refusal.code, error: refusal.error };
      roundTrip = () => textRefusal(answer, 400);
      writeFileSync(join(stateDir, "refused.md"), refusal.bytes);
      const argv = ["artifact", "--issue", "DSP-1", "--name", "notes.md", "--path", "refused.md"];

      const dry = await run([...argv, "--dry-run"], omp);
      expect(dry).toEqual({ code: 1, stdout: `${refusal.code}: ${refusal.error}\n`, stderr: "" });
      expect(requests).toEqual([
        { method: "POST", path: FILE_ROUND_TRIP, body: fileBody(refusal.bytes) },
      ]);
      requests.length = 0;
      const call = await run(argv, omp);
      expect(call).toEqual(dry);
      expect(requests).toEqual([
        { method: "POST", path: FILE_ROUND_TRIP, body: fileBody(refusal.bytes) },
      ]);
    });
  }

  // INFRA-4224 R3 (spec v27): the command reads every file and stdin a flag names as bytes
  // (`readFileText`, the `dispatch` entry's reader, run here with stdin's bytes handed to it), and
  // refuses bytes that are not UTF-8, which a lenient read stored as U+FFFD, with Dispatch's own
  // refusal of such a byte (`unstorableText`'s INVALID_UTF8): the field the flag sets, the first
  // such byte, and the character it stands at, counted from 1 in UTF-16 units. A U+0000 before
  // that byte is refused first, and an upload's text past the bytes one document holds before
  // either (the next test). The dry run and the call refuse it alike, for any command: exit 1, the
  // same stdout, and nothing sent. A byte order mark and multi-byte UTF-8 read as before, the mark
  // kept as U+FEFF.
  test("a file or stdin holding a byte that is not UTF-8 is refused with Dispatch's INVALID_UTF8 by the dry run as by the call, sending nothing, and UTF-8 reads as before", async () => {
    const file = join(stateDir, "text");
    const refused: { argv: string[]; file?: Buffer; stdin?: Buffer; stdout: string }[] = [
      // A byte order mark, a two-byte and a four-byte character, and a space before the byte: one,
      // one, two and one UTF-16 units.
      {
        argv: UPLOAD,
        stdin: Buffer.concat([Buffer.from("\uFEFFé😀 "), Buffer.from([0xff])]),
        stdout: notUtf8("content", "0xFF", 6),
      },
      {
        argv: ["comment", "--issue", "DSP-1", "--body-file", file],
        file: Buffer.concat([Buffer.from("Looks good.\n"), Buffer.from([0xff])]),
        stdout: notUtf8("body", "0xFF", 13),
      },
      // A sequence cut short is refused at the byte that opens it, as Go's utf8.DecodeRune reads it.
      {
        argv: ["message", "--issue", "DSP-1", "--body-file", "-"],
        stdin: Buffer.concat([Buffer.from("ab"), Buffer.from([0xe2, 0x82]), Buffer.from("c")]),
        stdout: notUtf8("body", "0xE2", 3),
      },
    ];
    for (const row of refused) {
      if (row.file !== undefined) writeFileSync(file, row.file);
      await refusedAlike(row.argv, row.stdin ?? Buffer.of(), row.stdout, "not UTF-8");
    }

    await commentPostedAsRead(file, "\uFEFFCafé 😀 naïve\n");
  });

  /** UTF-8 holding U+0000, and a text holding a byte that is not UTF-8, each refused at character
   *  11: two rows of packages/envoy/internal/dispatch/api/testdata/text-refusals.json. */
  const utf8WithNulRow: RefusedBytes = {
    name: "UTF-8 holding U+0000",
    bytes: Buffer.from("# Notes\n\nA\u0000B\n"),
    stdout: (field) => nul(field, 11),
  };
  const notUtf8Row: RefusedBytes = {
    name: "a file holding a byte that is not UTF-8",
    bytes: Buffer.concat([Buffer.from("# Notes\n\nA"), Buffer.from([0xff]), Buffer.from("B\n")]),
    stdout: (field) => notUtf8(field, "0xFF", 11),
  };

  // INFRA-4224 R3 (spec v27): for the same bytes, `dispatch artifact --content-file <file>`,
  // `--content-file -` (stdin), `--content <text>` where the bytes are UTF-8, and a markdown
  // `--path` file get the same refusal, in the order the `--path` file's upload refuses it
  // (`documentBytesRefusal`): the size first, then the first U+0000 or byte that is not UTF-8,
  // whichever comes first in the bytes, whether or not the bytes are UTF-8. Dispatch's words name
  // the field the upload names: `content` for inline text, `file` for a `--path` file. The dry run
  // and the call print the same stdout and exit 1, and neither sends anything: no round trip is
  // asked. Inline text holding U+0000, which only an in-process caller can give, is checked as a
  // text read is (review round 13 of INFRA-4224): its dry run once passed it, since this fake's
  // round trip keeps the text. The first three texts are the rows of
  // packages/envoy/internal/dispatch/api/testdata/text-refusals.json whose `command` is
  // ["inline"], and the fourth its row of UTF-8 holding U+0000. Any other command's field is no
  // document: a comment's body is refused at its first U+0000 or byte that is not UTF-8 only for
  // bytes that are not UTF-8, and its UTF-8 holding U+0000 the command does not refuse. The dry run
  // passes it, and the call sends it to Dispatch as read, which answers it.
  test("an upload's bytes from --content-file, stdin, inline --content or a markdown --path file are refused alike, the size first, then the first U+0000 or byte that is not UTF-8, by the dry run as by the call, sending nothing", async () => {
    const nulFirst = Buffer.from([0x41, 0x00, 0x42, 0xff]);
    await uploadRefusedEveryWay([
      {
        name: "a file holding U+0000 before a byte that is not UTF-8",
        bytes: nulFirst,
        stdout: (field) => nul(field, 2),
      },
      {
        name: "a file past the bytes one document holds holding a byte that is not UTF-8",
        bytes: Buffer.concat([
          Buffer.from("# Notes\n\nA"),
          Buffer.from([0xff]),
          Buffer.alloc(MARKDOWN_DOCUMENT_MAX_BYTES, "word "),
        ]),
        stdout: () => `${DOCUMENT_TOO_LARGE}\n`,
      },
      notUtf8Row,
      utf8WithNulRow,
    ]);

    const file = join(stateDir, "text");
    writeFileSync(file, nulFirst);
    const comment = ["comment", "--issue", "DSP-1", "--body-file", file];
    await refusedAlike(comment, Buffer.of(), nul("body", 2), "a comment");
    await commentPostedAsRead(file, "Looks good.\u0000\n");
  });

  // INFRA-4224 R3: with Dispatch off (no Dispatch in the environment and no envoy.json), the dry
  // run and the call refuse the same bytes from `--content-file <file>`, `--content-file -`
  // (stdin), `--content <text>` where the bytes are UTF-8, and a markdown `--path` file alike, as
  // they do with Dispatch on: UTF-8 holding U+0000, a text of UTF-8 past the bytes one document
  // holds, and bytes that are not UTF-8, each with exit 1 and the same stdout but for the field,
  // nothing on stderr, and no request. Each is refused once the flags are read, before anything
  // that needs Dispatch, so no client is built and the call is never refused for Dispatch being
  // off; a file these checks pass is (the test of the dry run's warning with Dispatch off). The
  // inline text is an in-process caller's (review round 13 of INFRA-4224): its call once exited 2
  // for Dispatch being off where its dry run refused the size with exit 1, or passed the U+0000
  // with a warning.
  test("with Dispatch off an upload's bytes from --content-file, stdin, inline --content or a markdown --path file are refused alike by the dry run and the call", async () => {
    const off = { ...omp, DISPATCH_URL: undefined, DISPATCH_TOKEN: undefined };
    await uploadRefusedEveryWay(
      [
        utf8WithNulRow,
        {
          name: "a text past the bytes one document holds",
          bytes: Buffer.from(PAST_DOCUMENT_LIMIT),
          stdout: () => `${DOCUMENT_TOO_LARGE}\n`,
        },
        notUtf8Row,
      ],
      off
    );
  });

  // A markdown --path file the command cannot read is no refusal of its text, so the check made
  // once the flags are read leaves it to the argument check: a dry run naming both a path and
  // content is refused for that, with exit 2, and sends nothing.
  test("an upload's dry run naming content and a markdown --path file that is not there is refused by the argument check", async () => {
    const argv = ["artifact", "--issue", "DSP-1", "--name", "a.md", "--content", "# A"];
    const dry = await run([...argv, "--path", "gone.md", "--dry-run"], omp);
    expect(dry.code).toBe(2);
    expect(dry.stdout).toStartWith(
      "dispatch artifact was not called: 1 problem\n- Give at most one of path and content."
    );
    expect(dry.stderr).toBe("");
    expect(requests).toEqual([]);
  });

  // With DISPATCH_DRY_RUN_LOCAL=1 an upload's dry run runs no step, so a markdown --path file that
  // is not there, which the check made once the flags are read leaves, is never read: the dry run
  // prints the arguments and sends nothing.
  test("with DISPATCH_DRY_RUN_LOCAL=1 an upload's dry run of a markdown --path file that is not there prints the arguments", async () => {
    const argv = ["artifact", "--issue", "DSP-1", "--name", "a.md", "--path", "gone.md"];
    const dry = await run([...argv, "--dry-run"], { ...omp, DISPATCH_DRY_RUN_LOCAL: "1" });
    expect(dry).toEqual({
      code: 0,
      stdout: `${JSON.stringify({ issue: "DSP-1", name: "a.md", path: "gone.md" }, null, 2)}\n`,
      stderr: "",
    });
    expect(requests).toEqual([]);
  });

  // A markdown --path file that is neither a regular file nor a directory is not read, as a file
  // Dispatch stores as its bytes is not (review round 13 of INFRA-4224): a FIFO stats with size 0,
  // and the check made once the flags are read waited forever on its read, so AskGate's own dry run
  // (DISPATCH_DRY_RUN_LOCAL=1) hung where it had returned at once. With the variable and without
  // it, the dry run prints the arguments, writes nothing on stderr and sends no request: the dry
  // run's step asks no file round trip about a file it holds no bytes of. A read that waits fails
  // the test by its timeout.
  test("an upload's dry run of a markdown --path file that is a FIFO prints the arguments without reading it, with DISPATCH_DRY_RUN_LOCAL=1 and without", async () => {
    execFileSync("mkfifo", [join(stateDir, "f.md")]);
    const argv = ["artifact", "--issue", "DSP-1", "--name", "a.md", "--path", "f.md", "--dry-run"];
    const json = `${JSON.stringify({ issue: "DSP-1", name: "a.md", path: "f.md" }, null, 2)}\n`;
    for (const local of ["1", undefined]) {
      requests.length = 0;
      const named = `DISPATCH_DRY_RUN_LOCAL=${local ?? "unset"}`;
      const dry = await run(argv, { ...omp, DISPATCH_DRY_RUN_LOCAL: local });
      expect(dry, named).toEqual({ code: 0, stdout: json, stderr: "" });
      expect(requests, named).toEqual([]);
    }
  });

  // INFRA-4224: the call of a markdown --path file asks the file round trip about the bytes it
  // would send, with and without --force, as its dry run does, and throws Dispatch's refusal
  // before it uploads. This fake's upload refuses the file in other words, which neither prints.
  test("the call of a markdown --path file whose file round trip Dispatch refuses prints that refusal as its dry run does, and uploads nothing", async () => {
    roundTrip = () => textRefusal(BLOCK_HTML_REFUSED, 400);
    uploadRefusal = () =>
      Response.json(
        { code: "INVALID_MARKDOWN", error: "the upload's own words, which the call never reaches" },
        { status: 400 }
      );
    writeFileSync(join(stateDir, "refused.md"), BLOCK_HTML);
    const argv = ["artifact", "--issue", "DSP-1", "--name", "notes.md", "--path", "refused.md"];
    for (const [call, asked] of [
      [argv, fileBody(BLOCK_HTML)],
      [[...argv, "--force"], { file: Buffer.from(BLOCK_HTML).toString("base64") }],
    ] as const) {
      requests.length = 0;
      const dry = await run([...call, "--dry-run"], omp);
      const called = await run(call, omp);
      expect(called, call.join(" ")).toEqual({
        code: 1,
        stdout: `${BLOCK_HTML_REFUSED.code}: ${BLOCK_HTML_REFUSED.error}\n`,
        stderr: "",
      });
      expect(dry, call.join(" ")).toEqual(called);
      // The dry run's round trip, then the call's, which is all the call sends: no read of the
      // held name, and no upload.
      expect(requests, call.join(" ")).toEqual([
        { method: "POST", path: FILE_ROUND_TRIP, body: asked },
        { method: "POST", path: FILE_ROUND_TRIP, body: asked },
      ]);
    }
  });

  // INFRA-4224: without --force the call reads whether the issue holds the name, and it reads it
  // only after its check of the text, the check its dry run makes. So a text that check refuses,
  // inline or in a markdown --path file, under a name the issue holds, gets the text's refusal
  // from the call, which reads no held name for it, as from its dry run, which reads none at all:
  // the same stdout and exit. A text the check passes is still refused for the name by the call,
  // read after the check, and passed by the dry run, which cannot know the name.
  test("a text refused for the text alone under a name the issue holds gets the text's refusal from the call, as from its dry run, not the name's", async () => {
    issueArtifacts = [NOTES_HELD];
    writeFileSync(join(stateDir, "refused.md"), BLOCK_HTML);
    writeFileSync(join(stateDir, "notes.md"), NOTES);
    const owner = ["artifact", "--issue", "DSP-1", "--name", "notes.md"];
    const refusedText = () => textRefusal(BLOCK_HTML_REFUSED, 400);
    const textRefused = `${BLOCK_HTML_REFUSED.code}: ${BLOCK_HTML_REFUSED.error}\n`;
    const cases: {
      argv: string[];
      stdin: string;
      answer: typeof roundTrip;
      asked: { path: string; body: unknown };
      code: number;
      refusal: string;
    }[] = [
      {
        argv: [...owner, "--content-file", "-"],
        stdin: BLOCK_HTML,
        answer: refusedText,
        asked: { path: ROUND_TRIP, body: markdownBody(BLOCK_HTML) },
        code: 1,
        refusal: textRefused,
      },
      {
        argv: [...owner, "--path", "refused.md"],
        stdin: "",
        answer: refusedText,
        asked: { path: FILE_ROUND_TRIP, body: fileBody(BLOCK_HTML) },
        code: 1,
        refusal: textRefused,
      },
      // Refused for its spelling, which exits 2 as the name's refusal does, in other words.
      {
        argv: [...owner, "--content-file", "-"],
        stdin: NOTES,
        answer: (markdown) => Response.json({ markdown: markdown.replace("* one", "- one") }),
        asked: { path: ROUND_TRIP, body: markdownBody(NOTES) },
        code: 2,
        refusal:
          'dispatch artifact was not called: 1 problem\n- Dispatch stores this text under another spelling: line 3 reads the word "*" at character 1, where Dispatch stores the word "-".',
      },
    ];
    for (const { argv, stdin, answer, asked, code, refusal } of cases) {
      roundTrip = answer;
      requests.length = 0;
      const call = await run(argv, omp, stdin);
      expect({ code: call.code, stderr: call.stderr }, argv.join(" ")).toEqual({
        code,
        stderr: "",
      });
      expect(call.stdout, argv.join(" ")).toStartWith(refusal);
      expect(call.stdout, argv.join(" ")).not.toContain("already holds");
      expect(requests, argv.join(" ")).toEqual([{ method: "POST", ...asked }]);
      expect(await run([...argv, "--dry-run"], omp, stdin), argv.join(" ")).toEqual(call);
    }

    roundTrip = keptText;
    for (const [argv, stdin, asked] of [
      [[...owner, "--content-file", "-"], NOTES, ROUND_TRIP],
      [[...owner, "--path", "notes.md"], "", FILE_ROUND_TRIP],
    ] as const) {
      requests.length = 0;
      const call = await run(argv, omp, stdin);
      expect(call.code, argv.join(" ")).toBe(2);
      expect(call.stdout, argv.join(" ")).toStartWith(
        "dispatch artifact was not called: 1 problem\n- issue DSP-1 already holds notes.md"
      );
      expect(
        requests.map((request) => `${request.method} ${request.path}`),
        argv.join(" ")
      ).toEqual([`POST ${asked}`, "GET /api/v1/issues/DSP-1/artifacts"]);
      expect((await run([...argv, "--dry-run"], omp, stdin)).code, argv.join(" ")).toBe(0);
    }
  });

  // The same ask block and markdown sent inline: the call asks the round trip before it uploads,
  // and is refused there as its dry run is, with the same stdout and exit 1; neither uploads.
  for (const refusal of [
    {
      holding: "an ask block that breaks its content rule",
      text: ASK_AFTER_OPTIONS,
      ...ASK_AFTER_OPTIONS_REFUSED,
    },
    { holding: "markdown the parser refuses", text: BLOCK_HTML, ...BLOCK_HTML_REFUSED },
  ]) {
    test(`inline text holding ${refusal.holding} is refused by the dry run as by the call`, async () => {
      roundTrip = () => textRefusal({ code: refusal.code, error: refusal.error }, 400);
      const dry = await run([...UPLOAD, "--dry-run"], omp, refusal.text);
      const call = await run(UPLOAD, omp, refusal.text);
      expect(call).toEqual({ code: 1, stdout: `${refusal.code}: ${refusal.error}\n`, stderr: "" });
      expect(dry).toEqual(call);
      expect(requests.filter((request) => request.method === "POST")).toEqual([
        { method: "POST", path: ROUND_TRIP, body: markdownBody(refusal.text) },
        { method: "POST", path: ROUND_TRIP, body: markdownBody(refusal.text) },
      ]);
    });
  }

  // INFRA-4224 R3 (spec v26): under --force the upload may replace a document the owner holds, and
  // a replacement is judged against the document it replaces, by looser rules than a new
  // document's seed: an ask the write leaves unchanged is not held to its content rule. So under
  // --force the round trip asks for none of a new document's refusals (no `new_document`), and
  // Dispatch answers the text as the most permissive replacement takes it, with the rendering that
  // replacement stores, as it answers every client that does not ask: the call checks that
  // spelling and uploads the text, and the dry run passes with no warning, inline and for a --path
  // file alike. This fake refuses the text only where the request asks.
  test("under --force the round trip asks for no new document's refusals: the call uploads and the dry run passes", async () => {
    roundTrip = (markdown, body) =>
      body.new_document === true ? textRefusal(ASK_AFTER_OPTIONS_REFUSED, 400) : keptText(markdown);
    writeFileSync(join(stateDir, "asks.md"), ASK_AFTER_OPTIONS);
    const owner = ["artifact", "--issue", "DSP-1", "--name", "notes.md", "--force"];
    for (const [argv, stdin, args, asked, upload] of [
      [
        [...owner, "--content-file", "-"],
        ASK_AFTER_OPTIONS,
        { issue: "DSP-1", name: "notes.md", force: true, content: ASK_AFTER_OPTIONS },
        { path: ROUND_TRIP, body: { markdown: ASK_AFTER_OPTIONS } },
        expect.objectContaining({ content: ASK_AFTER_OPTIONS }),
      ],
      [
        [...owner, "--path", "asks.md"],
        "",
        { issue: "DSP-1", name: "notes.md", force: true, path: "asks.md" },
        {
          path: FILE_ROUND_TRIP,
          body: { file: Buffer.from(ASK_AFTER_OPTIONS).toString("base64") },
        },
        ASK_AFTER_OPTIONS,
      ],
    ] as const) {
      requests.length = 0;
      const dry = await run([...argv, "--dry-run"], omp, stdin);
      expect({ code: dry.code, stderr: dry.stderr }, argv.join(" ")).toEqual({
        code: 0,
        stderr: "",
      });
      expect(JSON.parse(dry.stdout), argv.join(" ")).toEqual(args);
      expect(requests, argv.join(" ")).toEqual([{ method: "POST", ...asked }]);
      requests.length = 0;
      const call = await run(argv, omp, stdin);
      expect(call.code, `${argv.join(" ")}\n${call.stdout}`).toBe(0);
      expect(
        requests
          .filter((request) => request.path === "/api/v1/issues/DSP-1/artifacts")
          .map((request) => request.body),
        argv.join(" ")
      ).toEqual([upload]);
    }
    // Without --force the call makes a new document, so the same text is refused by the dry run
    // and by the call alike, from a round trip that asks for a new document's refusals.
    requests.length = 0;
    const dry = await run([...UPLOAD, "--dry-run"], omp, ASK_AFTER_OPTIONS);
    const call = await run(UPLOAD, omp, ASK_AFTER_OPTIONS);
    expect(call).toEqual({
      code: 1,
      stdout: `${ASK_AFTER_OPTIONS_REFUSED.code}: ${ASK_AFTER_OPTIONS_REFUSED.error}\n`,
      stderr: "",
    });
    expect(dry).toEqual(call);
    expect(requests.filter((request) => request.method === "POST")).toEqual([
      { method: "POST", path: ROUND_TRIP, body: markdownBody(ASK_AFTER_OPTIONS) },
      { method: "POST", path: ROUND_TRIP, body: markdownBody(ASK_AFTER_OPTIONS) },
    ]);
  });

  // A refusal every upload makes before its write, a NUL character here, holds under --force too:
  // the dry run and the call each refuse it once the flags are read, before they ask anything,
  // inline text read from stdin and a --path file alike, with the same stdout and exit 1, naming
  // the field each upload names (`content`, `file`). This fake's round trip keeps the text and its
  // upload stores what it is sent, so a call that reached either would pass.
  test("under --force an upload's refusal is still the call's: the dry run and the call refuse a NUL alike", async () => {
    const text = "# Notes\n\nA\u0000B\n";
    writeFileSync(join(stateDir, "nul.md"), text);
    const owner = ["artifact", "--issue", "DSP-1", "--name", "notes.md", "--force"];
    for (const [argv, stdin, field] of [
      [[...owner, "--content-file", "-"], text, "content"],
      [[...owner, "--path", "nul.md"], "", "file"],
    ] as const) {
      requests.length = 0;
      const dry = await run([...argv, "--dry-run"], omp, stdin);
      expect(dry, argv.join(" ")).toEqual({ code: 1, stdout: nul(field, 11), stderr: "" });
      expect(await run(argv, omp, stdin), argv.join(" ")).toEqual(dry);
      expect(requests, argv.join(" ")).toEqual([]);
    }
  });

  // The dry run's client stops at the dry run's deadline and takes the same budget as its own
  // deadline, so a credential is given that budget less the request's reserve
  // (`credentialDeadline`): of 600 ms, it waits 300. A broker credential whose renewal the broker
  // holds answers with the live token in hand once its wait ends, and the round trip still has its
  // reserve to be sent and answered. A client stopped by the dry run's deadline alone, with the
  // client's own minute as its budget, gives the credential the whole 600 ms, and the round trip
  // then goes out on a deadline already spent.
  test("an upload's dry run leaves the round trip its reserve after a credential that answers only when its wait ends", async () => {
    let trades = 0;
    const broker = Bun.serve({
      port: 0,
      hostname: "127.0.0.1",
      fetch: async () => {
        trades += 1;
        // The first trade's token is good for 200 s, so the client renews it at its half life
        // (`renewalAt`): the test moves the clock past that before the second dry run, while the
        // token is still live; the broker holds each renewal.
        if (trades > 1) return await new Promise<Response>(() => {});
        return Response.json({
          access_token: fixtureSecret("live"),
          issued_token_type: "urn:ietf:params:oauth:token-type:access_token",
          token_type: "DPoP",
          expires_in: 200,
        });
      },
    });
    const projected = join(stateDir, "projected-token");
    writeFileSync(projected, `${fixtureSecret("proj")}\n`);
    const pod = {
      ...omp,
      DISPATCH_TOKEN: undefined,
      AGENT_SECRETS_URL: `http://127.0.0.1:${broker.port}`,
      AGENT_SECRETS_MACHINE_IDENTITY: "kubernetes",
      AGENT_SECRETS_MACHINE_TOKEN_FILE: projected,
    };
    const passed = { code: 0, stdout: NOTES_JSON, stderr: "" };
    try {
      // The first dry run trades for the token the second one holds; moving the clock 150 s on
      // puts the second dry run past the token's half-life renewal while it is still live.
      expect(await run([...UPLOAD, "--dry-run"], pod, NOTES)).toEqual(passed);
      setSystemTime(new Date(Date.now() + 150_000));
      expect(await run([...UPLOAD, "--dry-run"], pod, NOTES, { dryRunDeadlineMs: 600 })).toEqual(
        passed
      );
      expect(trades).toBe(2);
      expect(requests).toEqual([
        { method: "POST", path: ROUND_TRIP, body: markdownBody(NOTES) },
        { method: "POST", path: ROUND_TRIP, body: markdownBody(NOTES) },
      ]);
    } finally {
      setSystemTime();
      forgetBrokerClients();
      broker.stop(true);
    }
  });

  test("an upload's dry run passes a text Dispatch keeps, printing the arguments alone", async () => {
    // The store adds the final line feed this text lacks, which is no other spelling.
    const content = "# Notes\n\n- one";
    const dry = await run([...UPLOAD, "--dry-run"], omp, content);
    expect(dry).toEqual({
      code: 0,
      stdout: `${JSON.stringify({ issue: "DSP-1", name: "notes.md", content }, null, 2)}\n`,
      stderr: "",
    });
    expect(requests).toEqual([{ method: "POST", path: ROUND_TRIP, body: markdownBody(content) }]);
  });

  // A typed block whose attributes differ from what Dispatch stores only in their order is no
  // other spelling: the call uploads the text as Dispatch stores it (storedUpload) and names the
  // block, so its dry run, which refuses only what the call refuses, passes it.
  test("an upload's dry run passes a text whose block attributes Dispatch stores in another order, as the call uploads it", async () => {
    const document = (attributes: string) =>
      `# Verification\n\n:::check{#c1 ${attributes}}\nThe block is stored in Dispatch's order.\n:::\n`;
    const content = document(
      'surface="cli" ci_check="lint" when="before_merge" negative_control="no"'
    );
    const stored = document(
      'surface="cli" when="before_merge" negative_control="no" ci_check="lint"'
    );
    roundTrip = () => Response.json({ markdown: stored });
    const dry = await run([...UPLOAD, "--dry-run"], omp, content);
    expect(dry).toEqual({
      code: 0,
      stdout: `${JSON.stringify({ issue: "DSP-1", name: "notes.md", content }, null, 2)}\n`,
      stderr: "",
    });
    requests.length = 0;
    const call = await run(UPLOAD, omp, content);
    expect(call.code, call.stdout).toBe(0);
    expect(call.stdout).toContain(
      "in the order Dispatch stores: id, surface, when, negative_control, ci_check."
    );
    expect(
      requests
        .filter(
          (request) =>
            request.method === "POST" && request.path === "/api/v1/issues/DSP-1/artifacts"
        )
        .map((request) => request.body)
    ).toEqual([expect.objectContaining({ content: stored })]);
  });

  // A Dispatch older than the round trip answers no route for it, as the server answers every path
  // no route serves, with its hint (routes/router.go's staticHandler), and the call then uploads
  // the text unchecked. The dry run asks the route once, with no waits before asking again, so
  // that answer reaches it well inside its 5 s bound, and its warning carries Dispatch's hint.
  test("an upload's dry run against a Dispatch with no round-trip route passes within its bound, saying the call uploads unchecked", async () => {
    roundTrip = () =>
      Response.json(
        { code: "NOT_FOUND", error: `no route for POST ${ROUND_TRIP}`, hint: NO_ROUTE_HINT },
        { status: 404 }
      );
    const started = performance.now();
    const dry = await run([...UPLOAD, "--dry-run"], omp, NOTES);
    expect(performance.now() - started).toBeLessThan(5_000);
    expect(dry).toEqual({
      code: 0,
      stdout: NOTES_JSON,
      stderr: routeAbsent(`NOT_FOUND: no route for POST ${ROUND_TRIP}; ${NO_ROUTE_HINT}`),
    });
    expect(requests).toEqual([{ method: "POST", path: ROUND_TRIP, body: markdownBody(NOTES) }]);
  });

  // A Dispatch that has the inline round trip and not the file one - main's server, and the old
  // pods of a rolling deploy - answers the file's check 404, as its own JSON with its hint. The
  // dry run of a markdown --path file then passes, saying this Dispatch cannot check the file and
  // that the call asks once more. The call asks the same route as its dry run asks it - once, with
  // no wait before asking a route again - and uploads the file once that route is absent too: one
  // file round trip, then the upload, inside the dry run's 5 s bound.
  test("a markdown --path file's dry run against a Dispatch with no file round trip passes, saying it cannot check the file, and the call asks once and uploads", async () => {
    roundTrip = (markdown, body) =>
      "file" in body
        ? Response.json(
            {
              code: "NOT_FOUND",
              error: `no route for POST ${FILE_ROUND_TRIP}`,
              hint: NO_ROUTE_HINT,
            },
            { status: 404 }
          )
        : keptText(markdown);
    writeFileSync(join(stateDir, "notes.md"), NOTES);
    const argv = ["artifact", "--issue", "DSP-1", "--name", "notes.md", "--path", "notes.md"];
    const asked = { method: "POST", path: FILE_ROUND_TRIP, body: fileBody(NOTES) };

    const dry = await run([...argv, "--dry-run"], omp);
    expect(dry).toEqual({
      code: 0,
      stdout: `${JSON.stringify({ issue: "DSP-1", name: "notes.md", path: "notes.md" }, null, 2)}\n`,
      stderr: `dispatch: warning: this Dispatch cannot check the file: POST ${FILE_ROUND_TRIP} failed with NOT_FOUND: no route for POST ${FILE_ROUND_TRIP}; ${NO_ROUTE_HINT}. The call asks once more, and uploads the file unchecked if it gets no answer either.\n`,
    });
    expect(requests).toEqual([asked]);
    requests.length = 0;
    const started = performance.now();
    const call = await run(argv, omp);
    expect(performance.now() - started).toBeLessThan(5_000);
    expect(call.code, call.stdout).toBe(0);
    expect(requests.filter((request) => request.method === "POST")).toEqual([
      asked,
      { method: "POST", path: "/api/v1/issues/DSP-1/artifacts", body: NOTES },
    ]);
  });

  // Every other answer of the file round trip that is not a refusal Dispatch marks says nothing
  // of the file, and the call uploads it, leaving its refusal to the upload: a Dispatch without
  // the route answering 405, the route's own 413 for a body past its bound, which carries no mark,
  // and a 503, which the call does not ask again, as its dry run does not, since the file goes up
  // whatever the round trip failed with.
  test("the call of a markdown --path file uploads it when the file round trip gives no answer about it", async () => {
    writeFileSync(join(stateDir, "notes.md"), NOTES);
    const argv = ["artifact", "--issue", "DSP-1", "--name", "notes.md", "--path", "notes.md"];
    for (const [status, answer] of [
      [405, { code: "METHOD_NOT_ALLOWED", error: "method not allowed" }],
      [413, { code: "CAP_EXCEEDED", error: "request body exceeds 6292480 bytes" }],
      [503, { code: "UNAVAILABLE", error: "the database is restarting" }],
    ] as const) {
      roundTrip = () => Response.json(answer, { status });
      requests.length = 0;
      const call = await run(argv, omp);
      expect(call.code, `${status}: ${call.stdout}`).toBe(0);
      expect(
        requests.filter((request) => request.method === "POST"),
        String(status)
      ).toEqual([
        { method: "POST", path: FILE_ROUND_TRIP, body: fileBody(NOTES) },
        { method: "POST", path: "/api/v1/issues/DSP-1/artifacts", body: NOTES },
      ]);
    }
  });

  // A dry run whose round trip cannot run passes and says why on stderr: an AskGate that runs the
  // call after a failed dry run would post the write unreviewed. The JSON stays alone on stdout.
  test("an upload's dry run passes when the round trip fails, naming the failure on stderr", async () => {
    const cases = [
      {
        status: 405,
        answer: { code: "METHOD_NOT_ALLOWED", error: "method not allowed" },
        stderr: routeAbsent("METHOD_NOT_ALLOWED: method not allowed"),
      },
      {
        status: 500,
        answer: { code: "INTERNAL", error: "the document service is down" },
        stderr: unasked(`POST ${ROUND_TRIP} failed with INTERNAL: the document service is down`),
      },
    ];
    for (const { status, answer, stderr } of cases) {
      roundTrip = () => Response.json(answer, { status });
      const dry = await run([...UPLOAD, "--dry-run"], omp, NOTES);
      expect(dry, String(status)).toEqual({ code: 0, stdout: NOTES_JSON, stderr });
    }
  });

  test("an upload's dry run passes when Dispatch gives no answer within its bound, saying so", async () => {
    const held = Promise.withResolvers<Response>();
    roundTrip = () => held.promise;
    try {
      const dry = await run([...UPLOAD, "--dry-run"], omp, NOTES, { dryRunDeadlineMs: 200 });
      expect(dry).toEqual({
        code: 0,
        stdout: NOTES_JSON,
        stderr: unasked("Dispatch gave no answer within 0.2 s"),
      });
    } finally {
      held.resolve(keptText(NOTES));
    }
  });

  // With Dispatch off the call is refused with the Dispatch-off message, exit 2, whatever it
  // uploads - text, blank text, a Markdown file - and its dry run passes, saying so, and sends
  // nothing.
  test("an upload's dry run with Dispatch off says the call is refused for the same reason, and sends nothing", async () => {
    const off = { ...omp, DISPATCH_URL: undefined, DISPATCH_TOKEN: undefined };
    writeFileSync(join(stateDir, "notes.md"), NOTES);
    const owner = ["artifact", "--issue", "DSP-1", "--name", "notes.md"];
    for (const [argv, stdin, subject] of [
      [[...owner, "--content-file", "-"], NOTES, "the text"],
      [[...owner, "--content-file", "-"], " \n\t", "the text"],
      [[...owner, "--path", "notes.md"], "", "the file"],
    ] as const) {
      const call = await run(argv, off, stdin);
      expect(call.code, argv.join(" ")).toBe(2);
      const why = call.stdout.replace(/^dispatch: /, "").trimEnd();
      const dry = await run([...argv, "--dry-run"], off, stdin);
      expect(dry.code, argv.join(" ")).toBe(0);
      expect(dry.stderr, argv.join(" ")).toStartWith(
        `dispatch: warning: the dry run could not ask Dispatch about ${subject}: ${why}`
      );
      expect(dry.stderr, argv.join(" ")).toEndWith(" The call is refused for the same reason.\n");
    }
    expect(requests).toEqual([]);
  });

  // AskGate reads the arguments alone: with DISPATCH_DRY_RUN_LOCAL=1 the dry run asks Dispatch
  // nothing, and the call refuses a respelled text itself.
  test("with DISPATCH_DRY_RUN_LOCAL=1 an upload's dry run checks the arguments alone, sending nothing", async () => {
    roundTrip = (markdown) => Response.json({ markdown: markdown.replace("* one", "- one") });
    const dry = await run([...UPLOAD, "--dry-run"], { ...omp, DISPATCH_DRY_RUN_LOCAL: "1" }, NOTES);
    expect(dry).toEqual({ code: 0, stdout: NOTES_JSON, stderr: "" });
    expect(requests).toEqual([]);
  });

  // The call asks the round trip nothing for the empty string, and its dry run alike. A file
  // Dispatch stores as its bytes, one whose path is not Markdown, is no document, so its dry run
  // asks nothing about it (a Markdown file's asks for a refusal alone, above).
  test("an upload's dry run of a file stored as bytes, of empty text and of a name alone asks Dispatch nothing", async () => {
    writeFileSync(join(stateDir, "rows.jsonl"), rows);
    const owner = ["artifact", "--issue", "DSP-1", "--name", "notes.md"];
    const cases: { argv: string[]; args: object }[] = [
      {
        argv: ["artifact", "--issue", "DSP-1", "--name", "rows.jsonl", "--path", "rows.jsonl"],
        args: { issue: "DSP-1", name: "rows.jsonl", path: "rows.jsonl" },
      },
      {
        argv: [...owner, "--content", ""],
        args: { issue: "DSP-1", name: "notes.md", content: "" },
      },
      { argv: owner, args: { issue: "DSP-1", name: "notes.md" } },
    ];
    for (const { argv, args } of cases) {
      const dry = await run([...argv, "--dry-run"], omp);
      expect(dry, argv.join(" ")).toEqual({
        code: 0,
        stdout: `${JSON.stringify(args, null, 2)}\n`,
        stderr: "",
      });
    }
    expect(requests).toEqual([]);
  });

  // Any other blank text (Go's strings.TrimSpace empties it) is asked, for a refusal alone, never
  // for its spelling: the round trip answers "" for it, which is no spelling the call refuses, so a
  // short blank text passes the dry run, as the call uploads it.
  test("an upload's dry run of a short blank text asks the round trip and passes", async () => {
    // The round trip answers the empty markdown for blank text it does not refuse.
    roundTrip = () => Response.json({ markdown: "" });
    const blank = " \n\t";
    const dry = await run([...UPLOAD, "--dry-run"], omp, blank);
    expect(dry).toEqual({
      code: 0,
      stdout: `${JSON.stringify({ issue: "DSP-1", name: "notes.md", content: blank }, null, 2)}\n`,
      stderr: "",
    });
    expect(requests).toEqual([{ method: "POST", path: ROUND_TRIP, body: markdownBody(blank) }]);
  });

  // Blank text the upload refuses: 20,000 paragraphs of a no-break space, which Go's
  // strings.TrimSpace empties and which make more elements than one document may hold. Dispatch's
  // round trip refuses them with the upload's 413, marked as a refusal of the text, and the call
  // and its dry run each refuse them with it, with the same stdout and exit 1. Neither uploads.
  test("an upload's dry run refuses blank text past the element limit with Dispatch's refusal, as the call does", async () => {
    roundTrip = () => textRefusal(BLANK_TOO_MANY_ELEMENTS, 413);
    const dry = await run([...UPLOAD, "--dry-run"], omp, BLANK_PAST_ELEMENTS);
    const call = await run(UPLOAD, omp, BLANK_PAST_ELEMENTS);
    expect(call).toEqual({
      code: 1,
      stdout: `CAP_EXCEEDED: ${BLANK_TOO_MANY_ELEMENTS.error}\n`,
      stderr: "",
    });
    expect(dry).toEqual(call);
    expect(requests.filter((request) => request.method === "POST")).toEqual([
      { method: "POST", path: ROUND_TRIP, body: markdownBody(BLANK_PAST_ELEMENTS) },
      { method: "POST", path: ROUND_TRIP, body: markdownBody(BLANK_PAST_ELEMENTS) },
    ]);
  });

  test("artifact's help says its dry run asks Dispatch how it would store the text, and another command's that it sends nothing", async () => {
    const artifact = await run(["artifact", "--help"], omp);
    const dryRun = artifact.stdout.slice(artifact.stdout.indexOf("  --dry-run\n")).split("\n");
    expect(dryRun[0]).toBe("  --dry-run");
    expect(dryRun[1]).toContain(
      "it asks Dispatch how it would store the text (POST /api/v1/markdown/round-trip)"
    );
    const message = await run(["message", "--help"], omp);
    expect(message.stdout).toContain(
      "  --dry-run\n      Checks the arguments as the call would, then prints them as JSON and sends nothing.\n"
    );
  });

  test("the follow notice is printed once per ask across processes", async () => {
    const ask = ["ask", "--issue", "DSP-1", "--question", "Which way?", "--option", "A"];
    const first = await run(ask, omp);
    nextProcess("ses-1");
    const second = await run(ask, omp);
    expect(first.code).toBe(0);
    expect(second.code).toBe(0);
    expect(first.stdout).toContain("Following ask ask-a");
    expect(second.stdout).not.toContain("Following ask ask-a");
  });

  test("the triage line is printed once per issue across processes", async () => {
    triage = true;
    const message = ["message", "--issue", "DSP-1", "--body", "Deliverable landed."];
    const first = await run(message, omp);
    nextProcess("ses-1");
    const second = await run(message, omp);
    expect(first.stdout).toContain("DSP-1 is still in triage");
    expect(second.stdout).not.toContain("is still in triage");
  });

  // Every `dispatch` command is a process of its own and carries nothing of Dispatch's keyed layer
  // from an earlier call. Every Dispatch that carries `b1c42d1d7d` runs that layer, so a later
  // call's write whose attempt fails is asked again under its key with no /healthz read, which the
  // load that stalled the write can keep from answering in time. The state.json here is one an
  // earlier release wrote, whose `keyedLayer` remembered a sighting of the layer too old for that
  // release to trust: it is not read, and the call's save writes no such member back.
  test("a failed write is asked again under its key with no /healthz read, whatever an earlier call of the session saw", async () => {
    const origin = `http://127.0.0.1:${server.port}`;
    const file = join(stateDir, "sessions", "ses-1", "state.json");
    mkdirSync(join(stateDir, "sessions", "ses-1"), { recursive: true });
    const keyedLayer = { [origin]: new Date(Date.now() - 10 * 60_000).toISOString() };
    writeFileSync(file, JSON.stringify({ advice: [], pictures: [], follows: [], keyedLayer }));
    nextProcess("ses-1");
    const stalled = keyedDispatch(1);

    const { code } = await run(
      ["message", "--issue", "DSP-1", "--body", "Deliverable landed."],
      omp,
      "",
      { fetchImpl: stalled.fetchImpl }
    );

    expect(code).toBe(0);
    expect(stalled.keys).toHaveLength(2);
    expect(stalled.keys[1]).toBe(stalled.keys[0] ?? "");
    expect(stalled.probes).toEqual([]);
    expect(JSON.parse(readFileSync(file, "utf-8"))).not.toHaveProperty("keyedLayer");
  });

  test("a read from a Dispatch server older than the repository fields answers, warning on stderr", async () => {
    const { code, stdout, stderr } = await run(["read", "--issue", "DSP-3"], omp);
    expect(code).toBe(0);
    expect(stdout).toContain(
      "Repository: unknown (this Dispatch server predates repository fields; upgrade it)\n"
    );
    expect(stderr).toContain(
      "dispatch: warning: Dispatch issue is missing resolved_repo: this Dispatch server predates repository fields, so DSP-3's repository is unknown; upgrade the server\n"
    );
  });

  // The plugin and the server ship separately, so a plugin that reads priorities meets a server
  // that answers none: its issue read prints the issue with its priorities unknown, never none,
  // and warns, as it does for the repository fields.
  test("a read from a Dispatch server older than priorities answers, warning on stderr", async () => {
    const { code, stdout, stderr } = await run(["read", "--issue", "DSP-3"], omp);
    expect(code, stderr).toBe(0);
    expect(stdout).toContain("Title: Read from an older server\nKey: DSP-3\n");
    expect(stdout).toContain(
      "Advances: unknown (this Dispatch server predates priorities; upgrade it)\n"
    );
    expect(stderr).toBe(
      [
        "dispatch: warning: Dispatch issue is missing resolved_repo: this Dispatch server predates repository fields, so DSP-3's repository is unknown; upgrade the server",
        "dispatch: warning: Dispatch issue is missing advances: this Dispatch server predates priorities, so the priorities DSP-3 advances are unknown; upgrade the server",
        "",
      ].join("\n")
    );
  });

  // A server from before priorities refuses a write naming `advances`, so the update says so from
  // its read and sends nothing: not the set, not the rest of the update, not the closing reason.
  test("an advances write to a Dispatch server older than priorities sends nothing", async () => {
    for (const named of [[], ["--advances-revision", "7"]]) {
      requests.length = 0;
      const argv = [
        "issue-update",
        "--issue",
        "DSP-3",
        "--advance",
        "ship-the-daemon",
        "--status",
        "done",
        "--reason",
        "Shipped.",
        ...named,
      ];
      const { code, stdout } = await run(argv, omp);
      expect(code, stdout).toBe(1);
      expect(stdout).toContain(
        "Dispatch read DSP-3 without its advances: this Dispatch server predates priorities, so nothing was sent; upgrade the server"
      );
      expect(requests.map(({ method, path }) => `${method} ${path}`)).toEqual([
        "GET /api/v1/issues/DSP-3",
      ]);
    }
  });

  // An issue read lists the issue's last 10 events unless it is asked for more: --events N lists
  // the last N and --all-events every one, read 200 at a time, the most one events request returns.
  describe("read --events and --all-events", () => {
    const listed = (stdout: string) => stdout.split("\n").filter((line) => /^- #\d+ /.test(line));

    test("--events 25 on an issue whose last_seq is 40 asks for the events after 15 through 40 and prints 25", async () => {
      const { code, stdout } = await run(["read", "--issue", "DSP-4", "--events", "25"], omp);
      expect(code, stdout).toBe(0);
      expect(eventLog.queries).toEqual(["?after=15&limit=25"]);
      const events = listed(stdout);
      expect(events).toHaveLength(25);
      expect(events[0]).toStartWith("- #16 message.created · human h · ");
      expect(events.at(-1)).toStartWith("- #40 message.created · human h · ");
    });

    test("without either flag the read lists the last 10, as before", async () => {
      const { code, stdout } = await run(["read", "--issue", "DSP-4"], omp);
      expect(code, stdout).toBe(0);
      expect(eventLog.queries).toEqual(["?after=30&limit=10"]);
      expect(listed(stdout).map((line) => line.split(" ")[1])).toEqual(
        Array.from({ length: 10 }, (_, index) => `#${31 + index}`)
      );
    });

    test("--all-events and an --events count over one page read every page up to last_seq", async () => {
      eventLog = { last: 450, served: 450, queries: [] };
      const all = await run(["read", "--issue", "DSP-4", "--all-events"], omp);
      expect(all.code, all.stdout).toBe(0);
      expect(eventLog.queries).toEqual([
        "?after=0&limit=200",
        "?after=200&limit=200",
        "?after=400&limit=50",
      ]);
      expect(listed(all.stdout).map((line) => line.split(" ")[1])).toEqual(
        Array.from({ length: 450 }, (_, index) => `#${index + 1}`)
      );

      eventLog.queries.length = 0;
      const last = await run(["read", "--issue", "DSP-4", "--events", "250"], omp);
      expect(last.code, last.stdout).toBe(0);
      expect(eventLog.queries).toEqual(["?after=200&limit=200", "?after=400&limit=50"]);
      const events = listed(last.stdout);
      expect(events).toHaveLength(250);
      expect(events[0]).toStartWith("- #201 ");
      expect(events.at(-1)).toStartWith("- #450 ");
    });

    test("an --events count past the issue's history lists every event through the captured last_seq", async () => {
      const { code, stdout } = await run(["read", "--issue", "DSP-4", "--events", "500"], omp);
      expect(code, stdout).toBe(0);
      expect(eventLog.queries).toEqual(["?after=0&limit=40"]);
      expect(listed(stdout)).toHaveLength(40);
    });

    test("events written after the issue was read are left to the next read", async () => {
      eventLog.served = 45;
      const last = await run(["read", "--issue", "DSP-4", "--events", "25"], omp);
      expect(last.code, last.stdout).toBe(0);
      expect(listed(last.stdout).at(-1)).toStartWith("- #40 ");
      const all = await run(["read", "--issue", "DSP-4", "--all-events"], omp);
      expect(all.code, all.stdout).toBe(0);
      expect(listed(all.stdout)).toHaveLength(40);
      expect(listed(all.stdout).at(-1)).toStartWith("- #40 ");
      const oversized = await run(["read", "--issue", "DSP-4", "--events", "500"], omp);
      expect(oversized.code, oversized.stdout).toBe(0);
      expect(eventLog.queries).toEqual([
        "?after=15&limit=25",
        "?after=0&limit=40",
        "?after=0&limit=40",
      ]);
      expect(listed(oversized.stdout)).toHaveLength(40);
      expect(listed(oversized.stdout).at(-1)).toStartWith("- #40 ");
    });

    test("--all-events on an issue with no events sends no events request", async () => {
      eventLog = { last: 0, served: 0, queries: [] };
      const { code, stdout } = await run(["read", "--issue", "DSP-4", "--all-events"], omp);
      expect(code, stdout).toBe(0);
      expect(eventLog.queries).toEqual([]);
      expect(stdout).toContain("\nEvents:\n- none\n");
    });

    test("--no-all-events beside --events keeps the count", async () => {
      const argv = ["read", "--issue", "DSP-4", "--events", "25", "--no-all-events"];
      const { code, stdout } = await run(argv, omp);
      expect(code, stdout).toBe(0);
      expect(eventLog.queries).toEqual(["?after=15&limit=25"]);
    });

    test("the issue's ref and its log read the same count", async () => {
      for (const argv of [
        ["--ref", "dispatch://DSP-4", "--events", "25"],
        ["--ref", "dispatch://DSP-4/log", "--events", "25"],
      ]) {
        eventLog.queries.length = 0;
        const { code, stdout } = await run(["read", ...argv], omp);
        expect(code, stdout).toBe(0);
        expect(eventLog.queries, argv.join(" ")).toEqual(["?after=15&limit=25"]);
        expect(listed(stdout), argv.join(" ")).toHaveLength(25);
      }
      eventLog.queries.length = 0;
      const log = await run(["read", "--ref", "dispatch://DSP-4/log", "--all-events"], omp);
      expect(log.code, log.stdout).toBe(0);
      expect(eventLog.queries).toEqual(["?after=0&limit=40"]);
      expect(listed(log.stdout)).toHaveLength(40);
    });

    test("--help names both flags", async () => {
      const { code, stdout } = await run(["read", "--help"], omp);
      expect(code).toBe(0);
      expect(stdout).toContain("\n  --events <n>\n");
      expect(stdout).toContain("\n  --all-events\n");
    });

    // A read that lists no events would drop the count without a word, so it is refused before any
    // request, naming what the read prints instead.
    test("two counts, a count that is not a positive integer, or one the read would drop is refused before any request", async () => {
      const id = "6f1d2c3b-4a5e-4f60-8b7c-9d0e1f2a3b4c";
      const dropped = (flag: string, read: string) =>
        `${flag} lists an issue's events, which dispatch read prints only for an issue or its dispatch://KEY/log, not for ${read}: drop ${flag}`;
      const cases: [string[], string][] = [
        [
          ["--issue", "DSP-4", "--events", "5", "--all-events"],
          "--events and --all-events both say how many events the read lists: name one, --events N for the last N or --all-events for every one",
        ],
        [["--issue", "DSP-4", "--events", "0"], "--events: Too small: expected number to be >=1"],
        [["--issue", "DSP-4", "--events", "2.5"], "--events must be an integer, not 2.5"],
        [["--message", id, "--events", "5"], dropped("--events", "a message's conversation")],
        [
          ["--issue", "DSP-4", "--artifact", "spec", "--all-events"],
          dropped("--all-events", "a document summary"),
        ],
        [
          ["--project", "CORE", "--artifact", "runbook", "--events", "5"],
          dropped("--events", "a document summary"),
        ],
        [
          ["--ref", "dispatch://DSP-4/spec", "--events", "5"],
          dropped("--events", "a document summary"),
        ],
        [
          ["--ref", "dispatch://CORE/artifact/runbook", "--all-events"],
          dropped("--all-events", "a document summary"),
        ],
        [
          ["--ref", "dispatch://DSP-4/children", "--events", "5"],
          dropped("--events", "the issue's children"),
        ],
        [
          ["--ref", "dispatch://DSP-4/comments", "--events", "5"],
          dropped("--events", "the issue's comments"),
        ],
        [["--ref", `dispatch://DSP-4/ask/${id}`, "--events", "5"], dropped("--events", "an ask")],
        [
          ["--ref", `dispatch://CORE/artifact/runbook/ask/${id}`, "--events", "5"],
          dropped("--events", "an ask"),
        ],
        [
          ["--ref", `dispatch://CORE/artifact/runbook/comment/${id}`, "--all-events"],
          dropped("--all-events", "a comment"),
        ],
        [
          ["--ref", `dispatch://DSP-4/comment/${id}`, "--all-events"],
          dropped("--all-events", "a comment"),
        ],
        [
          ["--ref", `dispatch://DSP-4/message/${id}`, "--events", "5"],
          dropped("--events", "a message's conversation"),
        ],
      ];
      for (const [flags, problem] of cases) {
        const { code, stdout } = await run(["read", ...flags], omp);
        expect(code, flags.join(" ")).toBe(2);
        expect(stdout, flags.join(" ")).toContain(`\n- ${problem}\n`);
      }
      expect(requests).toEqual([]);
    });

    // The log lists events, so beside an --artifact the log ref cannot use, only the artifact is
    // refused: dropping it, as the refusal says, reads the log with the count.
    test("an --artifact beside dispatch://KEY/log is refused alone, keeping the count", async () => {
      const { code, stdout } = await run(
        ["read", "--ref", "dispatch://DSP-4/log", "--artifact", "spec", "--events", "5"],
        omp
      );
      expect(code).toBe(2);
      expect(stdout).toContain("drop --artifact to read the ref");
      expect(stdout).not.toContain("drop --events");
      expect(requests).toEqual([]);
    });
  });

  test("a picture is written to a file the agent opens", async () => {
    const { code, stdout } = await run(
      ["doc-read", "--ref", "dispatch://agent/ses-pic/artifact/shot-png@v1"],
      { DISPATCH_HOST: "omp", DISPATCH_SESSION_ID: "ses-2" }
    );
    expect(code).toBe(0);
    const line = stdout.split("\n").find((candidate) => candidate.startsWith("- picture: "));
    const path = line?.slice("- picture: ".length).split(" (")[0] ?? "";
    expect(existsSync(path)).toBe(true);
    expect(readFileSync(path).subarray(0, 4).toString("hex")).toBe("89504e47");
  });

  test("every spec command that reaches Dispatch appends one ledger line naming its tool", async () => {
    await run(["message", "--issue", "DSP-1", "--body", "Deliverable landed."], omp);
    await run(["read", "--message", "missing"], omp);
    expect(ledger("ses-1").map((entry) => entry.tool)).toEqual([
      "dispatch_message",
      "dispatch_read",
    ]);
    expect(ledger("ses-1")[1]?.error).toBeDefined();
  });

  test("Claude Code's whole output stays under its read-back limit, picture lines kept, and names the full result", async () => {
    const limit = 25_000;
    const read = ["read", "--message", LONG_THREAD];
    // The result's text without the reply body, measured on a host that never shortens output.
    replyLength = 1;
    const short = await run(read, { DISPATCH_HOST: "omp", DISPATCH_SESSION_ID: "ses-long-1" });
    const textOf = (stdout: string): string => stdout.slice(0, stdout.indexOf("\n- picture: "));
    const overhead = textOf(short.stdout).length - 1;
    // The text alone is 10 characters under the limit; the picture line carries the output past it.
    replyLength = limit - overhead - 10;
    const full = await run(read, { DISPATCH_HOST: "omp", DISPATCH_SESSION_ID: "ses-long-2" });
    const text = textOf(full.stdout);
    expect(text.length).toBe(limit - 10);
    expect(full.stdout.length).toBeGreaterThan(limit);

    const claude = await run(read, {
      DISPATCH_HOST: "claude",
      CLAUDE_CODE_SESSION_ID: "ses-long-3",
    });
    expect(claude.code).toBe(0);
    // print() ends the output with one newline.
    expect(claude.stdout.length).toBeLessThanOrEqual(limit + 1);
    const pictureLine = /\n(- picture: [^\n]*)/.exec(claude.stdout)?.[1];
    expect(pictureLine).toBeDefined();
    // The file holds the whole result, its picture lines included.
    const marker = /\(the full result, (\d+) characters: (\S+)\)/.exec(claude.stdout);
    const written = `${text}\n${pictureLine}`;
    expect(marker?.[1]).toBe(String(written.length));
    expect(readFileSync(marker?.[2] ?? "", "utf-8")).toBe(written);
  });

  test("a state write that fails after a cut result still leaves Claude Code's output under the limit", async () => {
    const limit = 25_000;
    const read = ["read", "--message", LONG_THREAD];
    replyLength = limit;
    const dir = join(stateDir, "sessions", "ses-long-4");
    mkdirSync(dir, { recursive: true });
    // The ledger cannot be written, so the CLI adds its own diagnostic line after the result.
    mkdirSync(join(dir, "results.jsonl"));
    const claude = await run(read, {
      DISPATCH_HOST: "claude",
      CLAUDE_CODE_SESSION_ID: "ses-long-4",
    });
    expect(claude.code).toBe(0);
    expect(claude.stdout).toContain("dispatch: Dispatch took the call, but this session's state");
    expect(claude.stdout).toContain("(the full result, ");
    expect(claude.stdout.length).toBeLessThanOrEqual(limit + 1);
  });
  test("a corrupted state.json is refused with exit 2, naming the file, and sends nothing", async () => {
    const dir = join(stateDir, "sessions", "ses-1");
    mkdirSync(dir, { recursive: true });
    writeFileSync(join(dir, "state.json"), "{not json");
    const { code, stdout } = await run(["message", "--issue", "DSP-1", "--body", "x"], omp);
    expect(code).toBe(2);
    expect(stdout).toStartWith("dispatch: ");
    expect(stdout).toContain(join(dir, "state.json"));
    expect(requests).toEqual([]);
  });

  test("a ledger the call cannot be recorded in still reports the call, which Dispatch took", async () => {
    mkdirSync(join(stateDir, "sessions", "ses-1", "results.jsonl"), { recursive: true });
    const { code, stdout } = await run(["message", "--issue", "DSP-1", "--body", "x"], omp);
    expect(code).toBe(0);
    expect(requests.map((request) => request.path)).toContain("/api/v1/issues/DSP-1/messages");
    expect(stdout).toContain("message-1");
    expect(stdout).toContain("dispatch: Dispatch took the call, but this session's state");
  });

  test("a picture that cannot be saved still prints the result text, naming the failure", async () => {
    const dir = join(stateDir, "sessions", "ses-2");
    mkdirSync(dir, { recursive: true });
    writeFileSync(join(dir, "pictures"), "a file where the directory goes");
    const { code, stdout } = await run(
      ["doc-read", "--ref", "dispatch://agent/ses-pic/artifact/shot-png@v1"],
      { DISPATCH_HOST: "omp", DISPATCH_SESSION_ID: "ses-2" }
    );
    expect(code).toBe(0);
    expect(stdout).toContain("Picture shot.png");
    expect(stdout).not.toContain("- picture: ");
    expect(stdout).toContain("dispatch: Dispatch took the call, but this session's state");
  });

  // What an agent reads of a document is the markdown Dispatch stored, byte for byte, with the
  // trailer after a blank line and nothing of the command's own between them: a document whose
  // code blocks hold whitespace-only lines is text whose line ends matter, and a trailer run
  // onto its last line would read as part of it.
  test("doc-read prints the stored markdown, a blank line, then the document's token", async () => {
    const { code, stdout } = await run(["doc-read", "--issue", "DSP-5", "--artifact", "spec"], omp);

    expect(code, stdout).toBe(0);
    const trailer = stdout.indexOf("Document token:");
    expect(trailer, stdout).toBeGreaterThan(-1);
    // The stored markdown, its own line ending, then the blank line before the trailer.
    expect(stdout.slice(0, trailer)).toBe(`${storedCode}\n\n`);
    expect(stdout.slice(trailer)).toBe("Document token: sha256:code\n");
  });

  describe("doc-read --blocks", () => {
    const listing = [
      "Blocks of spec.md, in document order, each indented under the block holding it: id, type, (token; anchored comments and asks), opening words, then a typed block's serves and needs. A table lists its rows by the index delete_row takes and its cells by the row and the column delete_column takes.",
      "h-1 heading (sha256:h; 0 comments, 0 asks): # Café plan",
      // The 60th character is an emoji, two UTF-16 units: a cut by code unit would split it.
      `p-1 paragraph (sha256:p; 1 comment, 0 asks): ${"x".repeat(59)}😀…`,
      "l-1 bullet_list (sha256:l; 0 comments, 0 asks): - First item - Second item",
      "  i-1 list_item (sha256:i1; 0 comments, 0 asks): - First item",
      "    ip-1 paragraph (sha256:ip1; 0 comments, 0 asks): First item",
      "  i-2 list_item (sha256:i2; 0 comments, 0 asks): - Second item",
      "t-1 table (sha256:t; 0 comments, 1 ask): | # | Line | | | --- | --- | --- | | 1 | Backfill | extra |",
    ];
    // One read: the text route carries the blocks, so no write between two reads can split the
    // listing. The recorder keeps each request's path without its query.
    const reads = ["/api/v1/issues/DSP-2", "/api/v1/artifacts/doc-2/text"];

    test("lists every block's id, type, token, anchors and opening words, and each table's rows and cells", async () => {
      const { code, stdout } = await run(["doc-read", "--issue", "DSP-2", "--blocks"], omp);
      expect(code).toBe(0);
      expect(stdout).toBe(
        [
          ...listing,
          "  row 0, id r-0",
          '    row 0 column 0, id c-0-0, under "#": #',
          '    row 0 column 1, id c-0-1, under "Line": Line',
          "    row 0 column 2, id c-0-2: (empty)",
          "  row 1, no id yet",
          '    row 1 column 0, id c-1-0, under "#": 1',
          '    row 1 column 1, no id yet, under "Line": Backfill',
          "    row 1 column 2, id c-1-2: extra",
          "",
          "Document token: sha256:doc",
          "Document version: 3",
          "",
        ].join("\n")
      );
      expect(requests.map((request) => request.path)).toEqual(reads);
    });

    test("says the Dispatch lists no rows when its table entry carries none, and reads no markdown for them", async () => {
      blocksServer.tableRows = false;
      const { code, stdout } = await run(["doc-read", "--issue", "DSP-2", "--blocks"], omp);
      expect(code).toBe(0);
      expect(stdout).toBe(
        [
          ...listing,
          "  This Dispatch does not report table rows yet, so no row or column index is listed.",
          "",
          "Document token: sha256:doc",
          "Document version: 3",
          "",
        ].join("\n")
      );
      expect(requests.map((request) => request.path)).toEqual(reads);
    });

    test("refuses an uploaded picture, naming it, and reads none of its bytes", async () => {
      const { code, stdout } = await run(
        ["doc-read", "--issue", "DSP-2", "--artifact", "chart.png", "--blocks"],
        omp
      );
      expect(code).toBe(1);
      expect(stdout).toBe(
        "chart.png is an uploaded image, not a document, so it has no blocks. Read it with dispatch doc-read without --blocks.\n"
      );
      expect(requests.map((request) => request.path)).toEqual(["/api/v1/issues/DSP-2"]);
    });

    test("refuses a conversation's upload, naming it, as it refuses an issue's", async () => {
      const { code, stdout } = await run(
        ["doc-read", "--ref", "dispatch://agent/ses-pic/artifact/shot-png@v1", "--blocks"],
        omp
      );
      expect(code).toBe(1);
      expect(stdout).toBe(
        "shot.png is an uploaded image, not a document, so it has no blocks. Read it with dispatch doc-read without --blocks.\n"
      );
      expect(requests.map((request) => request.path)).toEqual([
        "/api/v1/agents/ses-pic/artifacts/shot-png",
      ]);
    });

    test("refuses a version, since only the live document's blocks are what an edit addresses", async () => {
      const { code, stdout } = await run(
        ["doc-read", "--issue", "DSP-2", "--blocks", "--version", "2"],
        omp
      );
      expect(code).toBe(2);
      expect(stdout).toContain(
        "- blocks lists the live document, the one dispatch doc-edit changes; name no version"
      );
      expect(requests).toEqual([]);
    });
  });

  // A replace with no with was sent and read as an empty one, which deleted the text its find
  // matched; one whose new text stood under markdown did the same. Both exit 2 before anything is
  // sent, naming the operation and the field.
  describe("doc-edit replace", () => {
    test("refuses a replace with no with before it sends anything", async () => {
      const { code, stdout } = await run(
        [
          "doc-edit",
          "--issue",
          "DSP-2",
          "--ops-json",
          JSON.stringify([{ op: "replace", find: "old" }]),
        ],
        omp
      );
      expect(code).toBe(2);
      expect(stdout).toContain(
        'ops[0] is a replace with no with, which would delete the text find matches: write the new text in with, or with "" to delete it on purpose.'
      );
      expect(requests).toEqual([]);
    });

    test("refuses a replace carrying a field replace does not take, naming it", async () => {
      const ops = [
        { op: "replace", find: "old", with: "new" },
        { op: "replace", find: "old", markdown: "new" },
      ];
      const { code, stdout } = await run(
        ["doc-edit", "--issue", "DSP-2", "--ops-json", JSON.stringify(ops)],
        omp
      );
      expect(code).toBe(2);
      expect(stdout).toContain(
        "ops[1] is a replace carrying markdown, which replace does not take: it takes find, with and occurrence, and its new text goes in with."
      );
      expect(stdout).toContain("ops[1] is a replace with no with");
      expect(stdout).not.toContain("ops[0]");
      expect(requests).toEqual([]);
    });
  });

  test("dispatch project prints the created project, and a taken key exits 1 naming PROJECT_EXISTS", async () => {
    const created = await run(["project", "--key", "SMOKE684", "--name", "Smoke"], omp);
    expect(created.code).toBe(0);
    expect(created.stdout).toContain("Created project SMOKE684 (Smoke)");
    expect(requests).toEqual([
      {
        method: "POST",
        path: "/api/v1/projects",
        body: expect.objectContaining({
          key: "SMOKE684",
          name: "Smoke",
          actor: expect.objectContaining({ kind: "session", id: "ses-1" }),
        }),
      },
    ]);

    const taken = await run(["project", "--key", "TAKEN", "--name", "Smoke"], omp);
    expect(taken.code).toBe(1);
    expect(taken.stdout).toContain("PROJECT_EXISTS: a project with this key already exists");
  });

  test("dispatch search refuses a project that is not a key in the server's wording, sending nothing", async () => {
    const { code, stdout } = await run(["search", "--query", "ok", "--project", "lower"], omp);
    expect(code).toBe(2);
    expect(stdout).toContain(`project is not a project key: ${PROJECT_KEY_RULE}`);
    expect(requests).toEqual([]);
  });

  test("dispatch projects prints each project's key and name, keys first", async () => {
    const { code, stdout } = await run(["projects"], omp);
    expect(code).toBe(0);
    expect(stdout).toBe("CORE Core\nSMOKE684 Smoke\n");
    expect(requests).toEqual([{ method: "GET", path: "/api/v1/projects", body: undefined }]);
  });

  // An insert's into, the block its markdown goes inside, reaches Dispatch as the caller wrote it,
  // and `doc-edit --help` names the key with its spelling and a reading inserted into an ask.
  test("dispatch doc-edit sends an insert's into as given, and its help names into", async () => {
    const op = {
      op: "insert",
      into: "block:a1",
      markdown: ":::reading{#r1}\nThe agent reads the answer as REST for every service.\n:::",
    };
    let sent: { ops?: unknown } | undefined;
    const fetchImpl = (async (input: string | URL | Request, init?: RequestInit) => {
      const request = new Request(input, init);
      const { pathname } = new URL(request.url);
      if (request.method === "POST" && pathname === "/api/v1/artifacts/doc-2/edits") {
        sent = await request.json();
        return Response.json({ applied: 1, version: { number: 4 }, token: "sha256:read" });
      }
      return fetch(request);
    }) as typeof fetch;
    const { code, stdout } = await run(
      ["doc-edit", "--issue", "DSP-2", "--ops-json", JSON.stringify([op])],
      omp,
      "",
      { fetchImpl }
    );
    expect(code, stdout).toBe(0);
    expect(stdout).toContain("Applied 1 ops (version 4)");
    expect(sent?.ops).toEqual([op]);

    const help = await run(["doc-edit", "--help"], omp);
    expect(help.code).toBe(0);
    expect(help.stdout).toContain(
      '\n        into (text): Insert inside this block, after its last child: "block:<id>" is the one form it takes.'
    );
    expect(help.stdout).toContain(
      '{"op":"insert","into":"block:a1","markdown":":::reading{#r1}\\nThe agent reads the answer as REST for every service.\\n:::"}'
    );
  });

  // Dispatch refuses a read-only token's write with READ_ONLY_TOKEN before anything runs. The
  // command prints that refusal as a line of its own, its code first and then Dispatch's message,
  // and exits 1, as for every write Dispatch refused. The credential is the read trade
  // DISPATCH_TOKEN_BROKER_READ asks for, over the DISPATCH_TOKEN the shell also holds.
  test("a write command under a read-only token prints Dispatch's READ_ONLY_TOKEN refusal and exits 1", async () => {
    const readToken = fixtureSecret("read");
    const trades: URLSearchParams[] = [];
    const broker = Bun.serve({
      port: 0,
      hostname: "127.0.0.1",
      fetch: async (request) => {
        trades.push(new URLSearchParams(await request.text()));
        return Response.json({
          access_token: readToken,
          issued_token_type: "urn:ietf:params:oauth:token-type:access_token",
          token_type: "DPoP",
          expires_in: 600,
          scope: "read",
        });
      },
    });
    const projected = join(stateDir, "projected-token");
    writeFileSync(projected, `${fixtureSecret("proj")}\n`);
    const refusal =
      "this request's credential is a read-only token, which may call only the GET routes Dispatch classes as reads: nothing ran";
    const sent: { method: string; path: string; authorization: string }[] = [];
    // The stand-in Dispatch: the test server answers every read, and every write is refused as
    // Dispatch refuses a read-only token's.
    const fetchImpl = (async (input: string | URL | Request, init?: RequestInit) => {
      const request = new Request(input, init);
      sent.push({
        method: request.method,
        path: new URL(request.url).pathname,
        authorization: request.headers.get("Authorization") ?? "",
      });
      if (request.method === "GET") return await fetch(request);
      return Response.json({ code: DISPATCH_READ_ONLY_TOKEN, error: refusal }, { status: 403 });
    }) as typeof fetch;
    let ran: { code: number; stdout: string };
    try {
      ran = await run(
        ["message", "--issue", "DSP-1", "--body", "hello"],
        {
          ...omp,
          DISPATCH_TOKEN_BROKER_READ: "1",
          AGENT_SECRETS_URL: `http://127.0.0.1:${broker.port}`,
          AGENT_SECRETS_MACHINE_IDENTITY: "kubernetes",
          AGENT_SECRETS_MACHINE_TOKEN_FILE: projected,
        },
        "",
        { fetchImpl }
      );
    } finally {
      forgetBrokerClients();
      broker.stop(true);
    }

    expect(ran.stdout.split("\n")).toContain(`${DISPATCH_READ_ONLY_TOKEN}: ${refusal}`);
    expect(ran.code).toBe(1);
    expect(trades.map((form) => form.get("scope"))).toEqual(["read"]);
    expect(sent.filter((one) => one.method !== "GET").map((one) => one.path)).toEqual([
      "/api/v1/issues/DSP-1/messages",
    ]);
    // Every request presented the read-only token, never the bearer the shell holds.
    expect(sent.map((one) => one.authorization)).toEqual(sent.map(() => `DPoP ${readToken}`));
  });
});

// `dispatch api`: one request of a route under /api/v1 (a GET here; its writes, its answers and
// the rest of its refusals are in dispatch-api.test.ts), with the session's own credential,
// Dispatch's JSON answer printed as `jq` reads it. Its own Dispatch records the Authorization each
// request carried, which the server above does not, and a broker of its own answers a pod's trade.
describe("dispatch api", () => {
  interface Asked {
    readonly method: string;
    readonly path: string;
    readonly authorization: string;
  }
  const asked: Asked[] = [];
  const dispatch = Bun.serve({
    port: 0,
    hostname: "127.0.0.1",
    fetch(request) {
      const { pathname, search } = new URL(request.url);
      asked.push({
        method: request.method,
        path: `${pathname}${search}`,
        authorization: request.headers.get("Authorization") ?? "",
      });
      if (pathname === "/api/v1/issues/DSP-1") {
        return Response.json({
          key: "DSP-1",
          claim: { actor: { kind: "session", id: "ses-architect" }, at: "2026-11-18T05:00:00Z" },
        });
      }
      return Response.json(
        { code: "NOT_FOUND", error: `issue ${pathname.split("/")[4]} not found` },
        { status: 404 }
      );
    },
  });
  /** The session each trade asked the broker to write under, as its `session` form field. */
  const trades: string[] = [];
  /** The access token the broker answered each trade with, in the order of `trades`. */
  const accessTokens: string[] = [];
  const broker = Bun.serve({
    port: 0,
    hostname: "127.0.0.1",
    async fetch(request) {
      trades.push(new URLSearchParams(await request.text()).get("session") ?? "no session");
      const accessToken = fixtureSecret("access");
      accessTokens.push(accessToken);
      return Response.json({
        access_token: accessToken,
        issued_token_type: "urn:ietf:params:oauth:token-type:access_token",
        token_type: "DPoP",
        expires_in: 600,
        credential_id: "cred-1",
      });
    },
  });
  afterAll(() => {
    dispatch.stop(true);
    broker.stop(true);
  });
  beforeEach(() => {
    asked.length = 0;
    trades.length = 0;
    accessTokens.length = 0;
    forgetBrokerClients();
  });
  const session = { ...omp, DISPATCH_URL: `http://127.0.0.1:${dispatch.port}` };

  test("reads a route with the session's bearer and prints Dispatch's JSON", async () => {
    const { code, stdout } = await run(["api", "--path", "/api/v1/issues/DSP-1?events=0"], session);
    expect(code).toBe(0);
    expect(JSON.parse(stdout).claim.actor.id).toBe("ses-architect");
    expect(asked).toEqual([
      { method: "GET", path: "/api/v1/issues/DSP-1?events=0", authorization: `Bearer ${TOKEN}` },
    ]);
  });

  test("under DISPATCH_TOKEN_BROKER=1, reads a route with a broker token traded for this session", async () => {
    const projected = join(stateDir, "projected-token");
    writeFileSync(projected, "projected.service.account.token\n");
    // A pod launched under the setting: the bearer the environment still holds is never read.
    const { code, stdout } = await run(["api", "--path", "/api/v1/issues/DSP-1"], {
      ...session,
      DISPATCH_TOKEN_BROKER: "1",
      AGENT_SECRETS_URL: `http://127.0.0.1:${broker.port}`,
      AGENT_SECRETS_MACHINE_IDENTITY: "kubernetes",
      AGENT_SECRETS_MACHINE_TOKEN_FILE: projected,
    });
    expect(code).toBe(0);
    expect(JSON.parse(stdout).key).toBe("DSP-1");
    expect(trades).toEqual(["ses-1"]);
    expect(asked).toEqual([
      {
        method: "GET",
        path: "/api/v1/issues/DSP-1",
        authorization: `DPoP ${accessTokens[0]}`,
      },
    ]);
  });

  // Two paths dispatch-api.test.ts's refusals hold no row of: an encoded dot segment in mixed
  // case, and a backslash after /api/v1.
  test("refuses a path outside /api/v1, naming it, exit 2, and sends nothing", async () => {
    const outside = ["/api/v1/%2E%2e/healthz", "/api/v1/issues\\..\\..\\healthz"];
    for (const path of outside) {
      const { code, stdout } = await run(["api", "--path", path], session);
      expect(code, path).toBe(2);
      expect(stdout, path).toStartWith("dispatch api was not called: 1 problem\n");
      expect(stdout, path).toContain(`path ${JSON.stringify(path)} `);
    }
    expect(asked).toEqual([]);
  });
});

// Two refusals an agent meets as the exit status of a command and the words on its own stdout,
// which a call inside this process cannot make: the command is its own process in production,
// and `packages/dispatch/e2e/dispatch-cli.ts:6-8` spawns this same entry for that reason. The
// child's environment is written rather than inherited, as that module writes it:
// `DISPATCH_TOKEN_FILE` wins over `DISPATCH_TOKEN` in this command, and a Legion pane exports
// one, so an inherited environment would send another Dispatch's token - or point the command at
// another Dispatch entirely. `HOME` and `DISPATCH_STATE_DIR` are the row's own temporary
// directory, so the command reads no developer's `envoy.json`.
describe("the dispatch entry as its own process", () => {
  const entry = join(import.meta.dir, "..", "..", "bin", "dispatch.ts");

  /** The entry run as the shell runs it, under the same Bun as this test. */
  async function spawnDispatch(
    argv: readonly string[]
  ): Promise<{ code: number; stdout: string; stderr: string }> {
    const child = Bun.spawn([process.execPath, entry, ...argv], {
      cwd: stateDir,
      env: {
        PATH: process.env.PATH,
        HOME: stateDir,
        DISPATCH_STATE_DIR: stateDir,
        DISPATCH_HOST: "omp",
        DISPATCH_SESSION_ID: "ses-1",
        DISPATCH_URL: `http://127.0.0.1:${server.port}`,
        DISPATCH_TOKEN: TOKEN,
      },
      stdout: "pipe",
      stderr: "pipe",
    });
    const [stdout, stderr, code] = await Promise.all([
      new Response(child.stdout).text(),
      new Response(child.stderr).text(),
      child.exited,
    ]);
    return { code, stdout, stderr };
  }

  // Exit 2, not 1: an argument this CLI refuses before it calls Dispatch is a usage error, and 1
  // is any other failure, a write Dispatch refused among them (`skill://dispatch`, "Exit 2 is a
  // usage error"). The browser row this replaced asserted 1 until cybermonklabs/legion#2781 made
  // the codes distinct.
  //
  // The 30 s limit is a hang guard, not a budget: the row runs the entry as a child process, and
  // bun's unwritten 5,000 ms per-test default is a limit on that work rather than on a hang.
  // Measured in a 2-CPU pod: 0.37 s idle; under 16 busy loops (8 x nproc), 3.98 s at most over 23
  // whole-suite runs and 2.98 s with this file alone, past half that default. It fires only if the
  // child never exits.
  test("exits 2 on a .jsonl name under --content-file, naming --path, and sends nothing", async () => {
    writeFileSync(join(stateDir, "rows.jsonl"), rows);

    const { code, stdout, stderr } = await spawnDispatch([
      "artifact",
      "--issue",
      "DSP-1",
      "--name",
      "rows.jsonl",
      "--content-file",
      "rows.jsonl",
    ]);

    expect({ code, stderr }, stdout).toEqual({ code: 2, stderr: "" });
    expect(stdout).toContain('"rows.jsonl" is not a Markdown name');
    expect(stdout).toContain("--path <file>");
    expect(requests).toEqual([]);
  }, 30_000);

  // The 30 s limit is a hang guard, not a budget: the row runs the entry as a child process twice
  // over, once per command, and bun's unwritten 5,000 ms per-test default is a limit on that work
  // rather than on a hang. Under 16 busy loops (8 x nproc) in a 2-CPU pod it timed out at
  // 5,261.76 ms, 1 run of 3 with this file alone, where it costs 0.70 s idle and 6.81 s at most
  // over 23 whole-suite runs under those loops. It fires only if a child never exits.
  test("exits nonzero on the server's block-cap refusal, printing it and nothing of its own", async () => {
    const refusal = printedBlockCapRefusal(1, DISPATCH_BLOCK_UNITS + 1);
    const body = "x".repeat(DISPATCH_BLOCK_UNITS + 1);

    for (const command of ["comment", "message"] as const) {
      const { code, stdout, stderr } = await spawnDispatch([
        command,
        "--issue",
        "DSP-1",
        "--body",
        body,
      ]);
      expect(code, `${command}: ${stdout}${stderr}`).not.toBe(0);
      expect(stdout.trim(), command).toBe(refusal);
    }
  }, 30_000);

  // A body holding an ask block is refused whatever its length (400 DECISION_BLOCK_IN_BODY,
  // INFRA-1201), and what the writer reads is the whole of the server's message, which says
  // where the question belongs. The refusal now opens with the server's code, because
  // `skill://dispatch` has every agent branch on the code and the command printed none
  // (dispatch://INFRA-1523); the message that INFRA-1201's row was written for follows it
  // unchanged. The 30 s limit is the hang guard the row above carries, for the reason written
  // there.
  test("exits nonzero on the server's decision-block refusal, printing its code and its whole message", async () => {
    const refusal = `DECISION_BLOCK_IN_BODY: ${decisionBlockRefusal(2)}`;
    const body = 'The mirror is behind.\n\n:::ask{urgency="med"}\nWhich region first?\n:::';

    for (const command of ["comment", "message"] as const) {
      const { code, stdout, stderr } = await spawnDispatch([
        command,
        "--issue",
        "DSP-1",
        "--body",
        body,
      ]);
      expect(code, `${command}: ${stdout}${stderr}`).not.toBe(0);
      expect(stdout.trim(), command).toBe(refusal);
      expect(stdout, command).toContain(decisionBlockRefusal(2));
    }
  }, 30_000);
});

describe("the dispatch CLI's envoy.json", () => {
  /** No Dispatch in the environment: the command reads it from a file. */
  const fromFile = { ...omp, DISPATCH_URL: undefined, DISPATCH_TOKEN: undefined };

  /** Writes an envoy.json naming the test server into `dir`, returning its path. */
  function serverConfig(dir: string): string {
    mkdirSync(dir, { recursive: true });
    const file = join(dir, "envoy.json");
    writeFileSync(
      file,
      JSON.stringify({
        dispatch: {
          enabled: true,
          serverUrl: `http://127.0.0.1:${server.port}`,
          token: TOKEN,
        },
      })
    );
    return file;
  }

  /** A directory whose envoy.json is not JSON, so a command that reads it exits 2. */
  function brokenConfigDirectory(): string {
    const dir = makeDirectory("broken");
    writeFileSync(join(dir, "envoy.json"), "{not json");
    return dir;
  }

  // Each plugin sets DISPATCH_HOST and ENVOY_CONFIG_FILE together in its session's shell: that pair
  // is the session's own hand-off, read over whatever harness variables the shell also inherited.
  test("with DISPATCH_HOST set, reads the file ENVOY_CONFIG_FILE names, whatever the harness variables name", async () => {
    const handed = serverConfig(join(stateDir, "handed"));
    // An omp session on the default profile started from a named profile's shell: omp leaves the
    // parent's OMP_PROFILE and PI_CODING_AGENT_DIR in its shell, and its plugin read the neutral file.
    const nestedDefault = await run(["projects"], {
      ...fromFile,
      OMP_PROFILE: "other",
      PI_CODING_AGENT_DIR: brokenConfigDirectory(),
      ENVOY_CONFIG_FILE: handed,
    });
    expect(nestedDefault.stdout).toBe("CORE Core\nSMOKE684 Smoke\n");
    expect(nestedDefault.code).toBe(0);
    // A named Claude Code profile whose shell's rc file exported another CLAUDE_CONFIG_DIR over
    // the profile's: the hook handed the profile's file.
    const namedClaude = await run(["projects"], {
      ...fromFile,
      DISPATCH_HOST: "claude",
      CLAUDE_CODE_SESSION_ID: "ses-1",
      CLAUDE_CONFIG_DIR: brokenConfigDirectory(),
      ENVOY_CONFIG_FILE: handed,
    });
    expect(namedClaude.stdout).toBe("CORE Core\nSMOKE684 Smoke\n");
  });

  test("with DISPATCH_HOST set and no ENVOY_CONFIG_FILE, each host takes its own harness's directory and no other's, else the neutral file", async () => {
    const claude = { DISPATCH_HOST: "claude", CLAUDE_CODE_SESSION_ID: "ses-1" };
    const profile = join(stateDir, "profile");
    serverConfig(profile);
    const ompRun = await run(["projects"], {
      ...fromFile,
      OMP_PROFILE: "work",
      PI_CODING_AGENT_DIR: profile,
      CLAUDE_CONFIG_DIR: brokenConfigDirectory(),
    });
    expect(ompRun.stdout).toBe("CORE Core\nSMOKE684 Smoke\n");

    const claudeDirectory = join(stateDir, "claude-own");
    serverConfig(claudeDirectory);
    const claudeRun = await run(["projects"], {
      ...fromFile,
      ...claude,
      CLAUDE_CONFIG_DIR: claudeDirectory,
      OMP_PROFILE: "work",
      PI_CODING_AGENT_DIR: brokenConfigDirectory(),
    });
    expect(claudeRun.stdout).toBe("CORE Core\nSMOKE684 Smoke\n");
    // An empty ENVOY_CONFIG_FILE is no hand-off: the host's own rule, not an inherited profile's.
    const emptyHanded = await run(["projects"], {
      ...fromFile,
      ...claude,
      CLAUDE_CONFIG_DIR: claudeDirectory,
      ENVOY_CONFIG_FILE: "",
      OMP_PROFILE: "work",
      PI_CODING_AGENT_DIR: brokenConfigDirectory(),
    });
    expect(emptyHanded.stdout).toBe("CORE Core\nSMOKE684 Smoke\n");

    serverConfig(join(stateDir, ".config", "legion"));
    const neutral = await run(["projects"], {
      ...fromFile,
      CLAUDE_CONFIG_DIR: brokenConfigDirectory(),
    });
    expect(neutral.stdout).toBe("CORE Core\nSMOKE684 Smoke\n");
  });

  // An omp session started from a Claude Code Bash inherits CLAUDE_CODE_CHILD_SESSION=1, and its
  // plugin sets DISPATCH_HOST=omp: the command acts as the omp session on omp's file.
  test("DISPATCH_HOST wins over CLAUDE_CODE_CHILD_SESSION=1", async () => {
    const profile = join(stateDir, "profile");
    serverConfig(profile);
    const { code, stdout } = await run(["message", "--issue", "DSP-1", "--body", "hello"], {
      ...fromFile,
      OMP_PROFILE: "work",
      PI_CODING_AGENT_DIR: profile,
      CLAUDE_CODE_CHILD_SESSION: "1",
      CLAUDE_CODE_SESSION_ID: "claude-ses",
      CLAUDE_CONFIG_DIR: brokenConfigDirectory(),
    });
    expect(stdout).not.toStartWith("dispatch: ");
    expect(code).toBe(0);
    const posted = requests.find((request) => request.path === "/api/v1/issues/DSP-1/messages");
    expect(posted?.body).toMatchObject({ actor: { id: "ses-1" } });
  });

  // Claude Code sets CLAUDE_CODE_CHILD_SESSION=1 in every Bash it runs, and a subagent's Bash can
  // get none of what the bridge's SessionStart hook writes: no DISPATCH_HOST, and whatever
  // ENVOY_CONFIG_FILE an outer omp session exported.
  test("a Claude Code shell with no DISPATCH_HOST is host claude, reading CLAUDE_CONFIG_DIR's file ahead of an inherited ENVOY_CONFIG_FILE", async () => {
    const claude = join(stateDir, "claude");
    serverConfig(claude);
    const { code, stdout } = await run(["message", "--issue", "DSP-1", "--body", "hello"], {
      DISPATCH_URL: undefined,
      DISPATCH_TOKEN: undefined,
      CLAUDE_CODE_CHILD_SESSION: "1",
      CLAUDE_CODE_SESSION_ID: "subagent-1",
      CLAUDE_CONFIG_DIR: claude,
      ENVOY_CONFIG_FILE: join(brokenConfigDirectory(), "envoy.json"),
      OMP_PROFILE: "work",
      PI_CODING_AGENT_DIR: brokenConfigDirectory(),
    });
    expect(stdout).not.toStartWith("dispatch: ");
    expect(code).toBe(0);
    const posted = requests.find((request) => request.path === "/api/v1/issues/DSP-1/messages");
    expect(posted?.body).toMatchObject({ actor: { id: "subagent-1" } });
  });

  // Codex sets CODEX_THREAD_ID in every shell command it runs and lets no plugin export a variable
  // into those shells, so a Codex shell with no DISPATCH_HOST is host codex, acting as that thread.
  test("a Codex shell with no DISPATCH_HOST is host codex, acting as CODEX_THREAD_ID", async () => {
    serverConfig(join(stateDir, ".config", "legion"));
    const { code, stdout } = await run(["message", "--issue", "DSP-1", "--body", "hello"], {
      ...fromFile,
      DISPATCH_HOST: undefined,
      DISPATCH_SESSION_ID: undefined,
      CODEX_THREAD_ID: "codex-thread-1",
    });
    expect(stdout).not.toStartWith("dispatch: ");
    expect(code).toBe(0);
    const posted = requests.find((request) => request.path === "/api/v1/issues/DSP-1/messages");
    expect(posted?.body).toMatchObject({ actor: { id: "codex-thread-1" } });
  });

  // A Codex session started from a Claude Code Bash inherits DISPATCH_HOST=claude,
  // CLAUDE_CODE_CHILD_SESSION=1 and the Claude session's id; the Codex plugin's shim sets
  // DISPATCH_HOST=codex, so the command acts as the Codex thread and never as the Claude session.
  test("DISPATCH_HOST=codex acts as CODEX_THREAD_ID over an inherited Claude Code session, reading no CODEX_HOME file", async () => {
    serverConfig(join(stateDir, ".config", "legion"));
    const codexHome = brokenConfigDirectory();
    const { code, stdout } = await run(["message", "--issue", "DSP-1", "--body", "hello"], {
      ...fromFile,
      DISPATCH_HOST: "codex",
      DISPATCH_SESSION_ID: undefined,
      CLAUDE_CODE_CHILD_SESSION: "1",
      CLAUDE_CODE_SESSION_ID: "claude-ses",
      CLAUDE_CONFIG_DIR: brokenConfigDirectory(),
      CODEX_HOME: codexHome,
      CODEX_THREAD_ID: "codex-thread-2",
    });
    expect(stdout).not.toStartWith("dispatch: ");
    expect(code).toBe(0);
    const posted = requests.find((request) => request.path === "/api/v1/issues/DSP-1/messages");
    expect(posted?.body).toMatchObject({ actor: { id: "codex-thread-2" } });
  });

  // A Codex session started from a Claude Code shell inherits that shell's DISPATCH_HOST=claude,
  // CLAUDE_CODE_CHILD_SESSION=1 and Claude session id, and a Claude Code session started from a
  // Codex shell inherits CODEX_THREAD_ID: the nearer of the two processes decides.
  test("a shell carrying both hosts' variables acts as the nearer of codex and claude", async () => {
    serverConfig(join(stateDir, ".config", "legion"));
    const both = {
      ...fromFile,
      DISPATCH_HOST: "claude",
      DISPATCH_SESSION_ID: undefined,
      CLAUDE_CODE_CHILD_SESSION: "1",
      CLAUDE_CODE_SESSION_ID: "claude-ses",
      CODEX_THREAD_ID: "codex-thread-3",
    };
    const argv = ["message", "--issue", "DSP-1", "--body", "hello"];
    const underCodex = await run(argv, both, "", { nearestAncestorNamed: () => "codex" });
    expect(underCodex.code).toBe(0);
    const underClaude = await run(argv, both, "", { nearestAncestorNamed: () => "claude" });
    expect(underClaude.code).toBe(0);
    const unknown = await run(argv, both, "", { nearestAncestorNamed: () => undefined });
    expect(unknown.code).toBe(0);
    const actors = requests
      .filter((request) => request.path === "/api/v1/issues/DSP-1/messages")
      .map((request) => (request.body as { actor: { id: string } }).actor.id);
    expect(actors).toEqual(["codex-thread-3", "claude-ses", "claude-ses"]);
  });

  test("a Codex shell started from an Oh My Pi shell acts as the nearer of codex and omp", async () => {
    serverConfig(join(stateDir, ".config", "legion"));
    const both = { ...fromFile, CODEX_THREAD_ID: "codex-thread-4" };
    const argv = ["message", "--issue", "DSP-1", "--body", "hello"];
    expect((await run(argv, both, "", { nearestAncestorNamed: () => "codex" })).code).toBe(0);
    expect((await run(argv, both, "", { nearestAncestorNamed: () => "omp" })).code).toBe(0);
    const actors = requests
      .filter((request) => request.path === "/api/v1/issues/DSP-1/messages")
      .map((request) => (request.body as { actor: { id: string } }).actor.id);
    expect(actors).toEqual(["codex-thread-4", "ses-1"]);
  });

  test("DISPATCH_HOST=codex with no CODEX_THREAD_ID refuses naming it, sending nothing", async () => {
    const { code, stdout } = await run(["projects"], {
      ...fromFile,
      DISPATCH_HOST: "codex",
      DISPATCH_SESSION_ID: "ses-1",
    });
    expect(stdout).toStartWith("dispatch: CODEX_THREAD_ID is not set");
    expect(code).toBe(2);
    expect(requests).toEqual([]);
  });

  test("with neither DISPATCH_HOST nor CLAUDE_CODE_CHILD_SESSION=1 it refuses naming DISPATCH_HOST, whatever else Claude Code left", async () => {
    const claude = join(stateDir, "claude");
    serverConfig(claude);
    for (const child of [{}, { CLAUDE_CODE_CHILD_SESSION: "0" }]) {
      const { code, stdout } = await run(["projects"], {
        DISPATCH_URL: undefined,
        DISPATCH_TOKEN: undefined,
        CLAUDE_CODE_SESSION_ID: "subagent-1",
        CLAUDE_CONFIG_DIR: claude,
        ...child,
      });
      expect(stdout).toStartWith("dispatch: DISPATCH_HOST is not set.");
      // An agent in a Python cell reads this first. With a made-up session id, the CLI finds no
      // AskGate record, so the write posts with no review.
      expect(stdout).toContain("From an agent's session, send the command as a bash call.");
      expect(stdout).toContain("set DISPATCH_SESSION_ID to the id of your own session");
      expect(code).toBe(2);
    }
    expect(requests).toEqual([]);
  });

  test("a file location that cannot be worked out is refused naming it, exit 2, sending nothing", async () => {
    for (const [variables, refusal] of [
      [{ XDG_CONFIG_HOME: "rel" }, "XDG_CONFIG_HOME is rel"],
      [{ ENVOY_CONFIG_FILE: "envoy.json" }, "ENVOY_CONFIG_FILE is envoy.json"],
    ] as const) {
      const { code, stdout } = await run(["projects"], { ...fromFile, ...variables });
      expect(stdout).toBe(`dispatch: ${refusal}, which is not an absolute path\n`);
      expect(code).toBe(2);
    }
    expect(requests).toEqual([]);
  });

  test("a missing file prints the notice and exits 2, sending nothing", async () => {
    const { code, stdout } = await run(["projects"], fromFile);
    expect(stdout).toBe(
      `dispatch: no envoy.json at ${join(stateDir, ".config", "legion", "envoy.json")}: Dispatch is off\n`
    );
    expect(code).toBe(2);
    expect(requests).toEqual([]);
  });

  test("a missing neutral file beside the OpenCode-era file names the move and exits 2, running and sending nothing", async () => {
    const marker = join(stateDir, "old-token-ran");
    const old = join(stateDir, ".config", "opencode", "envoy.json");
    mkdirSync(join(stateDir, ".config", "opencode"), { recursive: true });
    writeFileSync(
      old,
      JSON.stringify({
        dispatch: {
          enabled: true,
          serverUrl: `http://127.0.0.1:${server.port}`,
          token: `!touch ${marker}; echo old-token`,
        },
      })
    );

    const { code, stdout } = await run(["projects"], fromFile);

    expect(stdout).toBe(
      `dispatch: no envoy.json at ${join(stateDir, ".config", "legion", "envoy.json")}: Dispatch is off; ${old} is not read: mkdir -p ~/.config/legion && mv ~/.config/opencode/envoy.json ~/.config/legion/\n`
    );
    expect(code).toBe(2);
    expect(existsSync(marker)).toBe(false);
    expect(requests).toEqual([]);
  });

  test("a checkout's .opencode/envoy.json is named on every spec command's call, and its server is never used", async () => {
    serverConfig(join(stateDir, ".config", "legion"));
    const old = join(stateDir, ".opencode", "envoy.json");
    mkdirSync(join(stateDir, ".opencode"), { recursive: true });
    writeFileSync(
      old,
      JSON.stringify({
        dispatch: { enabled: true, serverUrl: "http://127.0.0.1:1", token: fixtureSecret("file") },
      })
    );

    const { code, stdout } = await run(["projects"], fromFile);

    expect(stdout).toBe(
      `dispatch: ${old} is not read: mkdir -p ${join(stateDir, ".envoy")} && mv ${old} ${join(stateDir, ".envoy", "envoy.json")}\nCORE Core\nSMOKE684 Smoke\n`
    );
    expect(code).toBe(0);
  });

  test("a profile's missing file tells dispatch where the old file belongs without running its token", async () => {
    const profile = join(stateDir, "profile");
    mkdirSync(profile);
    const marker = join(stateDir, "old-token-ran");
    const old = join(stateDir, ".config", "opencode", "envoy.json");
    mkdirSync(join(stateDir, ".config", "opencode"), { recursive: true });
    writeFileSync(
      old,
      JSON.stringify({
        dispatch: {
          enabled: true,
          serverUrl: `http://127.0.0.1:${server.port}`,
          token: `!touch ${marker}; echo old-token`,
        },
      })
    );

    const { code, stdout } = await run(["projects"], {
      ...fromFile,
      OMP_PROFILE: "work",
      PI_CODING_AGENT_DIR: profile,
    });

    const chosen = join(profile, "envoy.json");
    expect(stdout).toBe(
      `dispatch: no envoy.json at ${chosen}: Dispatch is off; ${old} is not read for this profile; copy or link it to ${chosen}, or write that file\n`
    );
    expect(code).toBe(2);
    expect(existsSync(marker)).toBe(false);
    expect(requests).toEqual([]);
  });

  test("a file that turns Dispatch off says so and exits 2, sending nothing", async () => {
    const dir = join(stateDir, "off");
    mkdirSync(dir, { recursive: true });
    const file = join(dir, "envoy.json");
    writeFileSync(file, JSON.stringify({ dispatch: { enabled: false } }));
    const { code, stdout } = await run(["projects"], { ...fromFile, ENVOY_CONFIG_FILE: file });
    expect(stdout).toBe(`dispatch: ${file} sets dispatch.enabled to false: Dispatch is off\n`);
    expect(code).toBe(2);
    expect(requests).toEqual([]);
  });

  test("a file that names no Dispatch server, and no DISPATCH_URL, names the file and exits 2, sending nothing", async () => {
    const dir = join(stateDir, "envoy-only");
    mkdirSync(dir, { recursive: true });
    const file = join(dir, "envoy.json");
    for (const contents of [
      { envoy: { enabled: true, natsUrls: ["nats://nats.internal.example:4222"] } },
      { dispatch: { token: fixtureSecret("file") } },
    ]) {
      writeFileSync(file, JSON.stringify(contents));
      const { code, stdout } = await run(["projects"], { ...fromFile, ENVOY_CONFIG_FILE: file });
      expect(stdout).toBe(
        `dispatch: ${file} names no Dispatch server, and DISPATCH_URL is not set: Dispatch is off\n`
      );
      expect(code).toBe(2);
    }
    expect(requests).toEqual([]);
  });

  // A Legion pod has no envoy.json: its Dispatch is DISPATCH_URL with DISPATCH_TOKEN_FILE, which
  // the session's one long-lived shell keeps until a command unsets or empties DISPATCH_URL.
  test("DISPATCH_TOKEN_FILE with no DISPATCH_URL says the shell lost it, not that envoy.json is missing, and exits 2, sending nothing", async () => {
    const tokenFile = join(stateDir, "DISPATCH_TOKEN");
    writeFileSync(tokenFile, `${TOKEN}\n`);
    const chosen = join(stateDir, "profile", "envoy.json");
    for (const variables of [
      { DISPATCH_TOKEN_FILE: tokenFile },
      { DISPATCH_TOKEN_FILE: tokenFile, DISPATCH_URL: "" },
    ]) {
      const { code, stdout } = await run(["whoami"], {
        ...fromFile,
        ENVOY_CONFIG_FILE: chosen,
        ...variables,
      });
      expect(stdout).toBe(
        "dispatch: DISPATCH_TOKEN_FILE is set and DISPATCH_URL is not, so Dispatch is off: this shell lost the DISPATCH_URL it started with. Export DISPATCH_URL again, set to the Dispatch server that token is for\n"
      );
      expect(code).toBe(2);
    }
    expect(requests).toEqual([]);
  });
});

describe("the dispatch CLI's subscription note", () => {
  const note =
    "(not subscribed to DSP-1; envoy_subscribe notifications.dispatch.issue.DSP-1.> for every event on it)";
  const withListener = { ...omp, ENVOY_URL: `http://127.0.0.1:${server.port}` };
  const post = () => run(["message", "--issue", "DSP-1", "--body", "hello"], withListener);
  const listed = (topics: readonly string[]) => () =>
    Response.json({ session_id: "ses-1", machine_id: "m", dir: "/", topics });

  test("a write names its issue only when the listener says none of the session's topics covers it", async () => {
    for (const [label, answer, noted] of [
      ["its own topic alone", listed(["notifications.agent.ses-1"]), true],
      [
        "the issue",
        listed(["notifications.agent.ses-1", "notifications.dispatch.issue.DSP-1.>"]),
        false,
      ],
      ["every Dispatch event", listed(["notifications.dispatch.>"]), false],
      // The listener holds no record of the session: it is subscribed to nothing.
      ["no record", () => Response.json({ error: "not found" }, { status: 404 }), true],
      // The listener cannot answer: the write says nothing about subscriptions.
      ["a listener error", () => Response.json({ error: "down" }, { status: 500 }), false],
    ] as const) {
      interests = answer;
      requests.length = 0;
      const { code, stdout } = await post();
      expect(code).toBe(0);
      expect([label, stdout.includes(note), stdout.includes("not subscribed")]).toEqual([
        label,
        noted,
        noted,
      ]);
      expect(requests.some((request) => request.path === "/v1/interests/ses-1")).toBe(true);
    }
  });

  test("with Envoy off, a write says nothing about subscriptions and asks no listener", async () => {
    const { code, stdout } = await run(["message", "--issue", "DSP-1", "--body", "hello"], omp);
    expect(code).toBe(0);
    expect(stdout).not.toContain("not subscribed");
    expect(requests.some((request) => request.path.startsWith("/v1/interests/"))).toBe(false);
  });

  test("an ask names the subscribe line, in its result and its follow notice, only when no subscription covers the issue, reading the listener once", async () => {
    const ask = ["ask", "--issue", "DSP-1", "--question", "Which way?", "--option", "A"];
    const line = "For every event on DSP-1: envoy_subscribe notifications.dispatch.issue.DSP-1.>";
    for (const [label, answer, named] of [
      ["its own topic alone", listed(["notifications.agent.ses-1"]), true],
      ["the issue", listed(["notifications.dispatch.issue.DSP-1.>"]), false],
      ["a listener error", () => Response.json({ error: "down" }, { status: 500 }), false],
    ] as const) {
      interests = answer;
      requests.length = 0;
      stateDir = makeDirectory();
      nextProcess("ses-1");
      const { code, stdout } = await run(ask, withListener);
      expect(code).toBe(0);
      expect(stdout).toContain("Following ask ask-a");
      // Both the result's line and the notice's, or neither.
      expect([label, stdout.split(line).length - 1]).toEqual([label, named ? 2 : 0]);
      // One read, shared by the result and the notice, and no retry after a 5xx.
      expect(requests.filter((request) => request.path === "/v1/interests/ses-1")).toHaveLength(1);
    }
  });

  test("a listener read that throws leaves the landed write reported as written", async () => {
    // The listener's fetch throws a value no transport wraps: the read is the only thing it breaks.
    const fetchImpl = (async (input: RequestInfo | URL, init?: RequestInit) => {
      if (String(input).includes("/v1/interests/")) throw "listener exploded";
      return fetch(input, init);
    }) as typeof fetch;
    let stdout = "";
    const code = await runDispatchCli(
      ["message", "--issue", "DSP-1", "--body", "hello"],
      environment(withListener),
      {
        stdout: (text) => {
          stdout += text;
        },
        stderr: () => {},
        readText: () => "",
        cwd: stateDir,
        fetchImpl,
      }
    );
    expect(code).toBe(0);
    expect(stdout).toStartWith("Posted message message-1");
    expect(stdout).not.toContain("not subscribed");
    expect(requests.some((request) => request.path === "/api/v1/issues/DSP-1/messages")).toBe(true);
  });

  // A write that landed before its answer was lost (409 KEY_COMMITTED) is done: the call succeeds,
  // and its one line says the write was made and to read it back, since there is nothing to print.
  test("a write answered KEY_COMMITTED exits 0 with one line saying to read it back", async () => {
    const fetchImpl = (async (input: RequestInfo | URL, init?: RequestInit) => {
      if (String(input).endsWith("/api/v1/issues/DSP-1/messages")) {
        return Response.json(
          { code: "KEY_COMMITTED", error: "the write landed and its answer was lost" },
          { status: 409 }
        );
      }
      return fetch(input, init);
    }) as typeof fetch;
    let stdout = "";
    const code = await runDispatchCli(
      ["message", "--issue", "DSP-1", "--body", "hello"],
      environment(withListener),
      {
        stdout: (text) => {
          stdout += text;
        },
        stderr: () => {},
        readText: () => "",
        cwd: stateDir,
        fetchImpl,
      }
    );

    expect(code).toBe(0);
    expect(stdout.trim().split("\n")).toEqual([
      expect.stringMatching(
        /^POST http:\/\/127\.0\.0\.1:\d+\/api\/v1\/issues\/DSP-1\/messages was made, but its answer was lost on the way back \(KEY_COMMITTED\), so this command cannot print what it made: read it back with `dispatch read`, or with the matching list for something it created\.$/
      ),
    ]);
  });

  // A replayed 2xx reading ANSWER_NOT_KEPT says Dispatch gives no copy of the first answer. The
  // issue create wrote, so it is the create's landing, which exits 0 and says to read it back; the
  // related-item check wrote nothing, so the command asks it under a second key, and when that is
  // answered the same way it exits 1 saying the call wrote nothing and to run it again.
  test("an answer Dispatch kept no copy of is a landing for a write, and for a call that writes nothing a line to run it again", async () => {
    const notKept = (status: number) =>
      Response.json(
        { code: "ANSWER_NOT_KEPT", error: "Dispatch kept no copy of this answer" },
        { status, headers: { "Idempotency-Replayed": "?1" } }
      );
    const checkKeys: (string | null)[] = [];
    const fetchImpl = (async (input: RequestInfo | URL, init?: RequestInit) => {
      const request = new Request(input, init);
      const { pathname } = new URL(request.url);
      if (request.method === "POST" && pathname === "/api/v1/issues") return notKept(201);
      if (request.method === "POST" && pathname === "/api/v1/projects/DSP/related") {
        checkKeys.push(request.headers.get(IDEMPOTENCY_KEY_HEADER));
        return notKept(200);
      }
      return fetch(request);
    }) as typeof fetch;

    const created = await run(
      ["issue", "--project", "DSP", "--title", "Retry webhook deliveries"],
      withListener,
      "",
      { fetchImpl }
    );
    expect(created.code).toBe(0);
    expect(created.stdout.trim().split("\n")).toEqual([
      expect.stringMatching(
        /^POST http:\/\/127\.0\.0\.1:\d+\/api\/v1\/issues was made, but Dispatch gives no copy of its answer, which was too large to keep or holds a comment or a message that was deleted, or a document version name a delete replaced \(ANSWER_NOT_KEPT\), so this command cannot print what it made: read it back with `dispatch read`, or with the matching list for something it created\.$/
      ),
    ]);

    const checked = await run(
      ["related", "--project", "DSP", "--title", "Retry webhook deliveries"],
      withListener,
      "",
      { fetchImpl }
    );
    expect(checked.code).toBe(1);
    expect(checked.stdout.trim().split("\n")).toEqual([
      expect.stringMatching(
        /^POST http:\/\/127\.0\.0\.1:\d+\/api\/v1\/projects\/DSP\/related wrote nothing, and Dispatch kept no copy of its answer \(ANSWER_NOT_KEPT\), so this command has no answer to print: run it again, which asks under a new key\.$/
      ),
    ]);
    expect(checkKeys).toHaveLength(2);
    expect(new Set(checkKeys).size).toBe(2);
  });

  // `skill://dispatch` promises exit 1 for a refusal, and a create the near-copy gate stops made
  // nothing: a caller that reads the status must not be told an issue was filed.
  test("a create the near-copy gate refuses exits 1, and a create it lets through exits 0", async () => {
    const candidates = [
      {
        key: "DSP-9",
        title: "Nightly export retries for ever",
        status: "in_progress",
        snippet: "<mark>Nightly</mark> export",
        shared_terms: 4,
        href: "/issues/DSP-9",
      },
    ];
    const creating = (refuse: boolean) =>
      (async (input: RequestInfo | URL, init?: RequestInit) => {
        const { pathname } = new URL(String(input));
        if (pathname === "/api/v1/projects/DSP/architecture-sources") {
          return Response.json({ sources: [] });
        }
        if (pathname === "/api/v1/issues" && init?.method === "POST") {
          return refuse
            ? Response.json(
                { code: "POSSIBLE_DUPLICATE", error: "possible duplicate of DSP-9", candidates },
                { status: 409 }
              )
            : Response.json(
                {
                  key: "DSP-10",
                  title: "Nightly export retries for ever",
                  project: "DSP",
                  components: { mode: "inherit", ids: [], unknown: [], reason: null },
                },
                { status: 201 }
              );
        }
        return fetch(input, init);
      }) as typeof fetch;
    const create = ["issue", "--project", "DSP", "--title", "Nightly export retries for ever"];

    const refused = await run(create, omp, "", { fetchImpl: creating(true) });
    expect(refused.code).toBe(1);
    expect(refused.stdout).toContain(
      'Not created: "Nightly export retries for ever" is a near copy of an open issue.'
    );
    expect(refused.stdout).toContain("DSP-9 [in_progress] Nightly export retries for ever");

    const made = await run(create, omp, "", { fetchImpl: creating(false) });
    expect(made.code).toBe(0);
    expect(made.stdout).toContain("Created DSP-10");
  });

  test("a read asks no listener", async () => {
    interests = listed([]);
    const { code } = await run(["projects"], withListener);
    expect(code).toBe(0);
    expect(requests.some((request) => request.path.startsWith("/v1/interests/"))).toBe(false);
  });
});

// A reader of a transcript, the skill-scenario scorer among them, finds a refusal the CLI printed
// even when a pipe hid its exit: each path below is the real CLI's, and its first line is the one
// refusal cliRefusals reads. The CLI's other `dispatch:` lines refuse nothing.
describe("cliRefusals", () => {
  const fromFile = { ...omp, DISPATCH_URL: undefined, DISPATCH_TOKEN: undefined };
  /** The one refusal `stdout` holds: its first line, and the command it names. */
  const refusal = (stdout: string, command: string | null) => [
    { line: stdout.split("\n")[0], command },
  ];

  test("an argument check's refusal names its command, in the call and in its dry run (exit 2)", async () => {
    for (const argv of [
      ["message", "--issue", "DSP-1", "--isue", "X"],
      ["issue-update", "--status", "done"],
      ["issue-update", "--status", "done", "--dry-run"],
    ]) {
      const { code, stdout } = await run(argv, omp);
      expect(code, argv.join(" ")).toBe(2);
      expect(cliRefusals(stdout), argv.join(" ")).toEqual(refusal(stdout, argv[0] ?? ""));
    }
    expect(requests).toEqual([]);
  });

  test("each usage refusal is a refusal of no command (exit 2)", async () => {
    const off = join(stateDir, "off.json");
    writeFileSync(off, JSON.stringify({ dispatch: { enabled: false } }));
    const cases: [string, readonly string[], Record<string, string | undefined>][] = [
      ["unknown command", ["frobnicate"], omp],
      ["no host", ["projects"], { DISPATCH_SESSION_ID: "ses-1" }],
      ["another host", ["projects"], { DISPATCH_HOST: "vim", DISPATCH_SESSION_ID: "ses-1" }],
      ["no omp session", ["projects"], { DISPATCH_HOST: "omp" }],
      ["no Claude Code session", ["projects"], { DISPATCH_HOST: "claude" }],
      ["a session id of two segments", ["projects"], { ...omp, DISPATCH_SESSION_ID: "a/b" }],
      ["a relative ENVOY_CONFIG_FILE", ["projects"], { ...fromFile, ENVOY_CONFIG_FILE: "e.json" }],
      ["a relative XDG_CONFIG_HOME", ["projects"], { ...fromFile, XDG_CONFIG_HOME: "rel" }],
      ["no envoy.json", ["projects"], fromFile],
      ["a file that turns Dispatch off", ["projects"], { ...fromFile, ENVOY_CONFIG_FILE: off }],
      ["DISPATCH_URL with no token", ["projects"], { ...omp, DISPATCH_TOKEN: undefined }],
    ];
    for (const [name, argv, env] of cases) {
      const { code, stdout } = await run(argv, env);
      expect(code, name).toBe(2);
      expect(stdout, name).toStartWith("dispatch: ");
      expect(cliRefusals(stdout), name).toEqual(refusal(stdout, null));
    }
    expect(requests).toEqual([]);
  });

  // The whole line is pinned, not its `dispatch: ` prefix: `opencode` was an accepted host until
  // it was removed, so the refusal naming it, and naming omp, claude and codex as the ones accepted, is the
  // user-visible behaviour that removal produces. A prefix-only assertion passes again the moment
  // the host comes back, or the message stops naming the hosts it accepts.
  test("a host the command does not accept is refused by name, with the three it accepts", async () => {
    const { code, stdout } = await run(["projects"], {
      DISPATCH_HOST: "opencode",
      DISPATCH_SESSION_ID: "ses-1",
    });
    expect(code).toBe(2);
    expect(stdout).toBe('dispatch: DISPATCH_HOST is "opencode", not omp, claude or codex.\n');
    expect(requests).toEqual([]);
  });

  test("a session state the CLI cannot read is a refusal (exit 2)", async () => {
    const corrupt = join(stateDir, "sessions", "ses-1");
    mkdirSync(corrupt, { recursive: true });
    writeFileSync(join(corrupt, "state.json"), "{not json");
    const unreadable = await run(["projects"], omp);
    expect(unreadable.code).toBe(2);
    expect(cliRefusals(unreadable.stdout)).toEqual(refusal(unreadable.stdout, null));

    stateDir = makeDirectory();
    writeFileSync(join(stateDir, "sessions"), "a file where the directory goes");
    const unlisted = await run(["projects"], omp);
    expect(unlisted.code).toBe(2);
    expect(unlisted.stdout).toStartWith("dispatch: ENOTDIR: ");
    expect(cliRefusals(unlisted.stdout)).toEqual(refusal(unlisted.stdout, null));
    expect(requests).toEqual([]);
  });

  test("a warning, the note that the state was not written, and a moved file's notice refuse nothing", async () => {
    const warned = await run(["read", "--issue", "DSP-3"], omp);
    expect(warned.code).toBe(0);
    expect(warned.stderr).toStartWith("dispatch: warning: ");
    expect(cliRefusals(`${warned.stderr}${warned.stdout}`)).toEqual([]);

    stateDir = makeDirectory();
    mkdirSync(join(stateDir, "sessions", "ses-1", "results.jsonl"), { recursive: true });
    const unrecorded = await run(["message", "--issue", "DSP-1", "--body", "x"], omp);
    expect(unrecorded.code).toBe(0);
    expect(unrecorded.stdout).toContain("\ndispatch: Dispatch took the call, but this session's");
    expect(cliRefusals(unrecorded.stdout)).toEqual([]);

    stateDir = makeDirectory();
    mkdirSync(join(stateDir, ".config", "legion"), { recursive: true });
    writeFileSync(
      join(stateDir, ".config", "legion", "envoy.json"),
      JSON.stringify({
        dispatch: { enabled: true, serverUrl: `http://127.0.0.1:${server.port}`, token: "t" },
      })
    );
    mkdirSync(join(stateDir, ".opencode"), { recursive: true });
    writeFileSync(join(stateDir, ".opencode", "envoy.json"), "{}");
    const moved = await run(["projects"], fromFile);
    expect(moved.code).toBe(0);
    expect(moved.stdout).toStartWith("dispatch: ");
    expect(cliRefusals(moved.stdout)).toEqual([]);
  });

  // A refusal the CLI prints before it reaches Dispatch, but `dispatch proxy`'s or
  // `dispatch login`'s, is logged too, whenever the call knows its session: a reader of the ledger
  // (pi-envoy's run-end check, the skill-scenario rig) sees a call whose output a pipe, `$(…)` or
  // another program swallowed. The host and the command need not be known for the session to be:
  // a Python `subprocess.run(["dispatch", …])` inherits the session id without `DISPATCH_HOST`.
  test("an early refusal is logged when the call knows its session, with no request", async () => {
    const cases: [string, readonly string[], Record<string, string | undefined>, string][] = [
      ["an unknown flag", ["message", "--issue", "X", "--isue", "Y"], omp, "dispatch_message"],
      ["an unknown command", ["frobnicate"], omp, "dispatch"],
      // The CLI checks the command's name before its flags, so a help call of an unknown command
      // is refused, and a dry run with a bad argument is refused before it is a dry run. The
      // skill-scenario scorer matches each ledger line to its command on this.
      ["an unknown command's help", ["frobnicate", "--help"], omp, "dispatch"],
      [
        "a dry run's bad argument",
        ["message", "--issue", "X", "--isue", "Y", "--dry-run"],
        omp,
        "dispatch_message",
      ],
      ["no host", ["projects"], { DISPATCH_SESSION_ID: "ses-1" }, "dispatch_projects"],
      [
        "another host",
        ["projects"],
        { DISPATCH_HOST: "vim", DISPATCH_SESSION_ID: "ses-1" },
        "dispatch_projects",
      ],
      [
        "Dispatch turned off",
        ["projects"],
        { ...omp, DISPATCH_URL: undefined, DISPATCH_TOKEN: undefined },
        "dispatch_projects",
      ],
    ];
    for (const [name, argv, env, tool] of cases) {
      stateDir = makeDirectory();
      const { code, stdout } = await run(argv, env);
      expect(code, name).toBeGreaterThan(0);
      const logged = ledger("ses-1");
      expect(
        logged.map((line) => line.tool),
        name
      ).toEqual([tool]);
      expect(logged[0]?.error, name).toBe(stdout.trimEnd());
      expect(cliRefusals(`${logged[0]?.error}\n`), name).toHaveLength(1);
    }
    expect(requests).toEqual([]);
  });

  // Catches: a crash, or an empty ledger file, when no session id is set at all, and a help or dry
  // run logged as a refusal.
  test("a call with no session logs nothing, and help and a dry run log no refusal", async () => {
    const { code } = await run(["projects"], { DISPATCH_HOST: "omp" });
    expect(code).toBe(2);
    expect(existsSync(join(stateDir, "sessions"))).toBe(false);

    stateDir = makeDirectory();
    await run(["doc-edit", "--help"], omp);
    await run(["message", "--issue", "DSP-1", "--body", "hi", "--dry-run"], omp);
    expect(existsSync(join(stateDir, "sessions", "ses-1", "results.jsonl"))).toBe(false);
  });

  test("help, a dry run's arguments, and a refusal quoted inside a line refuse nothing", async () => {
    const help = await run(["doc-edit", "--help"], omp);
    const dry = await run(["message", "--issue", "DSP-1", "--body", "hi", "--dry-run"], omp);
    for (const { code, stdout } of [help, dry]) {
      expect(code).toBe(0);
      expect(cliRefusals(stdout)).toEqual([]);
    }
    expect(cliRefusals("It said: dispatch message was not called: 1 problem")).toEqual([]);
  });
});

describe("fitOutput", () => {
  const write = (text: string): string => `/state/out/${text.length}.md`;

  test("leaves an output under the limit as it is", () => {
    expect(fitOutput("text", ["- picture: a"], ["note"], 100, write)).toBe(
      "text\n- picture: a\nnote"
    );
  });

  test("cuts the result text first, keeping every line after it and naming the full result", () => {
    const out = fitOutput("x".repeat(200), ["- picture: a"], ["follow notice", "note"], 120, write);
    expect(out.length).toBeLessThanOrEqual(120);
    expect(out).toContain("(the full result, 213 characters: /state/out/213.md)");
    expect(out).toEndWith("\n- picture: a\nfollow notice\nnote");
  });

  test("when the picture lines and notice alone pass the limit, drops picture lines and names them", () => {
    const pictures = Array.from(
      { length: 10 },
      (_, index) => `- picture: /p/${index} (${"y".repeat(30)})`
    );
    const out = fitOutput("x".repeat(50), pictures, ["follow notice", "note"], 200, write);
    expect(out.length).toBeLessThanOrEqual(200);
    expect(out).toEndWith("\nfollow notice\nnote");
    expect(out).toMatch(/\n\(\d+ more picture lines? left out: see the full result\)\n/);
    expect(out).toContain("(the full result, ");
  });

  test("when the kept lines alone pass the limit, the output is cut to it and names the file first", () => {
    const out = fitOutput("x".repeat(50), ["- picture: a"], ["n".repeat(500)], 200, write);
    expect(out.length).toBe(200);
    expect(out).toStartWith("(the full result, 63 characters: /state/out/63.md)");
  });

  test("an unbounded limit returns every line as given", () => {
    expect(fitOutput("text", ["- picture: a"], ["note"], Number.POSITIVE_INFINITY, write)).toBe(
      "text\n- picture: a\nnote"
    );
  });
});
