import { afterAll, beforeEach, describe, expect, jest, test } from "bun:test";
import { existsSync, mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import type { ArtifactBlock } from "@legion/contracts";
import {
  DISPATCH_BLOCK_UNITS,
  IDEMPOTENCY_KEY_HEADER,
  PROJECT_KEY_RULE,
  SPEC_CHECK_OBSERVED_CUT_NOTE,
  SPEC_CHECK_OBSERVED_END,
  SPEC_CHECK_OBSERVED_MAX,
  SPEC_CHECK_OBSERVED_START,
} from "@legion/contracts";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import type { Socket } from "bun";
import { keyedLayer } from "../../test/keyed-layer";
import { routeAnswer } from "../../test/route-answers";
import { type StubProxy, startStubProxy } from "../../test/stub-proxy";
import {
  cliRefusals,
  DISPATCH_TOKEN_CHECK_WAIT_MS,
  fitOutput,
  type ResultBound,
  runDispatchCli,
} from "../dispatch-cli";
import { resetAdviceMemory } from "../dispatch-execute";
import { DISPATCH_WRITE_RETRY } from "../dispatch-http";
import { forgetShownPictures } from "../dispatch-picture-tools";

interface Recorded {
  readonly method: string;
  readonly path: string;
  readonly body: unknown;
}

const makeDirectory = temporaryDirectories("dispatch-cli");

/** The Dispatch bearer each command is handed. */
const TOKEN = fixtureSecret("dsp");

/**
 * What each child `bun` a row starts is given beside its own environment: no transpiler cache.
 * Its HOME is the row's folder (`makeDirectory`), where Bun keeps that cache, about 155 files,
 * and the fixture's afterEach removes the folder. With the cache off the removal reads only what
 * the command itself wrote, not 155 more files on a loaded machine.
 */
const CHILD_BUN = { BUN_RUNTIME_TRANSPILER_CACHE_PATH: "0" } as const;

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

/** How the Dispatch server refuses a body whose block `block` is a decision block, worded as
 *  `itemblocks.DecisionBlock` words it: the card such a block draws is one nobody can answer
 *  where it stands, so the body is refused however short it is (INFRA-1201). */
function decisionBlockRefusal(block: number): string {
  return (
    `body block ${block} (ask) holds a decision block, which nobody can answer where it stands: ` +
    "ask the question in the issue's own document, as a decision block there, or open an ask " +
    "with `dispatch ask`; to show a decision block as an example, put it in a fenced code block"
  );
}

/** The server's refusal of `body`: a decision block in it (400 DECISION_BLOCK_IN_BODY), else a
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

/** The commit a CI job tested, which every result of a check of ACME's spec files below is for. */
const SPEC_COMMIT = "1111111111111111111111111111111111111111";

/** One result a CI job stored for check p-1 of ACME's spec files at SPEC_COMMIT, as Dispatch
 *  serves it: Dispatch's record number and an in-toto statement of the job's run. */
function storedSpecResult(seq: number, verdict: string, leg: string, attempt: number) {
  const workflow = "example-org/example-repo/.github/workflows/ci-results.yaml";
  return {
    id: `00000000-0000-4000-8000-0000000000${seq}`,
    seq,
    kind: "run",
    attestation: {
      _type: "https://in-toto.io/Statement/v1",
      subject: [
        {
          name: "git+https://github.com/example-org/example-repo",
          digest: { gitCommit: SPEC_COMMIT },
        },
      ],
      predicateType: "https://github.com/cybermonklabs/legion/attestations/spec-check-result/v1",
      predicate: {
        project: "ACME",
        check: "p-1",
        procedure_digest: "sha256:5f1d9c0e",
        test: { runner: "go", selector: "example.com/m/p TestP", job: "go-checks" },
        verdict,
        observed: verdict === "pass" ? "--- PASS: TestP (0.01s)" : "--- FAIL: TestP (0.02s)",
        job: "go-checks",
        leg,
        run: {
          repository: "example-org/example-repo",
          workflow_ref: `${workflow}@refs/heads/feat/probe`,
          ref: "refs/heads/feat/probe",
          event_name: "push",
          run_id: "4242",
          attempt,
          check_run_id: "7001",
          runner_environment: "github-hosted",
        },
        recorder: {
          session: `ci-results:${workflow}`,
          principal: `workflow-results:${workflow}`,
        },
        recorded_at: "2026-11-16T10:00:00Z",
      },
    },
  };
}

/** What ACME's read of SPEC_COMMIT answers, newest first, and the query of each such read. */
const storedSpecResults = [
  storedSpecResult(42, "fail", "linux", 2),
  storedSpecResult(41, "pass", "", 1),
];
const specResultQueries: string[] = [];
/** The query of each search the stand-in was asked. */
const searchQueries: string[] = [];

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

// Behind Dispatch's keyed layer: each 2xx to a write names the write's key back.
const server = Bun.serve({
  port: 0,
  fetch: keyedLayer(async (request: Request) => {
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
    // document here, so every upload below writes a first version. It is recorded with the rest.
    if (pathname === "/api/v1/issues/DSP-1/artifacts" && request.method === "GET") {
      requests.push({ method: request.method, path: pathname, body: undefined });
      return Response.json([]);
    }
    const text = await request.text();
    const body = text === "" ? undefined : JSON.parse(text);
    requests.push({ method: request.method, path: pathname, body });
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
    if (pathname === "/api/v1/projects/ACME/results" && request.method === "GET") {
      specResultQueries.push(search);
      return Response.json({ results: storedSpecResults });
    }
    if (pathname === "/api/v1/search" && request.method === "GET") {
      searchQueries.push(search);
      return Response.json(routeAnswer("GET /api/v1/search", {}, 200));
    }
    return Response.json({ error: `unexpected ${request.method} ${pathname}` }, { status: 500 });
  }),
});

afterAll(() => server.stop(true));

let stateDir = "";
beforeEach(() => {
  requests.length = 0;
  triage = false;
  blocksServer = { tableRows: true };
  eventLog = { last: 40, served: 40, queries: [] };
  handoffs.length = 0;
  specResultQueries.length = 0;
  searchQueries.length = 0;
  interests = () => Response.json({ error: "unset" }, { status: 500 });
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
  fetchImpl?: typeof fetch,
  nearestAncestorNamed?: (names: readonly string[]) => string | undefined
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
    ...(fetchImpl === undefined ? {} : { fetchImpl }),
    ...(nearestAncestorNamed === undefined ? {} : { nearestAncestorNamed }),
  });
  return { code, stdout, stderr };
}

/** A fresh process for the same session: the in-memory memories are gone, the state dir stays. */
function nextProcess(sessionId: string): void {
  resetAdviceMemory();
  forgetShownPictures(sessionId);
}

/**
 * The test server behind Dispatch's keyed layer, which judged every answer to a write and names
 * its `Idempotency-Key` back on each: the first `failing` message posts fail in transport before
 * they reach it. A `/healthz` read is recorded in `probes` and never answers within any bound, as
 * under the load that stalls a write.
 */
function keyedDispatch(failing: number): {
  readonly fetchImpl: typeof fetch;
  readonly keys: string[];
  readonly probes: string[];
} {
  const keys: string[] = [];
  const probes: string[] = [];
  let left = failing;
  const fetchImpl = keyedLayer(
    async (input: string | URL | Request, init?: RequestInit) => {
      const request = new Request(input, init);
      const { pathname } = new URL(request.url);
      if (pathname === "/healthz") {
        probes.push(pathname);
        throw new DOMException("The operation timed out.", "TimeoutError");
      }
      if (request.method === "POST" && pathname === "/api/v1/issues/DSP-1/messages") {
        keys.push(request.headers.get(IDEMPOTENCY_KEY_HEADER) ?? "");
        if (left-- > 0) throw new TypeError("fetch failed");
      }
      return fetch(request);
    },
    { judged: () => true }
  ) as typeof fetch;
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

  // An empty key or id is refused before any request, naming the flag: sent, an empty `--issue`
  // asked `PATCH /api/v1/issues/`, which no route serves, and came back seven seconds later as a
  // refusal about a rolling release.
  test("an empty --issue is refused before any request, naming the flag", async () => {
    const { code, stdout } = await run(
      ["issue-update", "--issue", "", "--status", "in_progress"],
      omp
    );
    expect({ code, lines: stdout.split("\n").slice(0, 2) }).toEqual({
      code: 2,
      lines: [
        "dispatch issue-update was not called: 1 problem",
        "- --issue is empty, so the call names no issue: a variable that gives it may be empty or unset",
      ],
    });
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
      "- Example: dispatch result --issue DSP-1 --check k1 --role implementer --verdict pass --commit 1111111111111111111111111111111111111111",
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

  // `skill://dispatch` says "Exit 1 is a refusal, 2 a usage error", so a call Dispatch never saw
  // because an argument is missing exits 2: a script reading the code alone tells it from a write
  // Dispatch refused (a near copy, an AskGate block), which exits 1.
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

  // A dry run reads nothing from Dispatch, so it checks and prints with Dispatch off too, and
  // prints the JSON alone.
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

  // Every `dispatch` command is a process of its own, and its write whose attempt fails is asked
  // again under its key on its own: Dispatch's keyed layer answers the repeat from the key, so the
  // call needs nothing an earlier call saw, and the session's state keeps nothing of the layer.
  test("a failed write is asked again under its key in a call of its own, and the state keeps nothing of the keyed layer", async () => {
    const message = ["message", "--issue", "DSP-1", "--body", "Deliverable landed."];
    const stalled = keyedDispatch(1);
    const { code, stdout } = await run(message, omp, "", stalled.fetchImpl);
    expect(code, stdout).toBe(0);
    expect(stalled.keys).toHaveLength(2);
    expect(stalled.keys[1]).toBe(stalled.keys[0] ?? "");
    const state = JSON.parse(
      readFileSync(join(stateDir, "sessions", "ses-1", "state.json"), "utf-8")
    );
    expect(Object.keys(state).sort()).toEqual(["advice", "follows", "pictures"]);
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
      stalled.fetchImpl
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

  test("every call that reaches Dispatch appends one ledger line naming its tool", async () => {
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

  // Sent, an empty `--project` of a search searched every project, and said nothing of it. An
  // empty variable of a step is how one is written, so it is refused before any request, naming
  // the flag; a search of every project leaves the flag out.
  test("dispatch search refuses an empty --project, naming the flag, and searches every project without one", async () => {
    const empty = await run(["search", "--query", "broker token", "--project", ""], omp);
    expect({ code: empty.code, lines: empty.stdout.split("\n").slice(0, 2) }).toEqual({
      code: 2,
      lines: [
        "dispatch search was not called: 1 problem",
        "- --project is empty, so the call names no project: a variable that gives it may be empty or unset",
      ],
    });
    expect(requests).toEqual([]);
    const every = await run(["search", "--query", "broker token"], omp);
    expect({ code: every.code, searched: searchQueries }).toEqual({
      code: 0,
      searched: ["?q=broker+token"],
    });
  });

  test("dispatch projects prints each project's key and name, keys first", async () => {
    const { code, stdout } = await run(["projects"], omp);
    expect(code).toBe(0);
    expect(stdout).toBe("CORE Core\nSMOKE684 Smoke\n");
    expect(requests).toEqual([{ method: "GET", path: "/api/v1/projects", body: undefined }]);
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
        ...CHILD_BUN,
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
  // is a write Dispatch refused (`skill://dispatch`, "Exit 1 is a refusal, 2 a usage error"). The
  // browser row this replaced asserted 1 until cybermonklabs/legion#2781 made the codes distinct.
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

  // The row's folder is the child's HOME, and the fixture's afterEach removes it: what the child
  // leaves there is all that removal reads.
  test("leaves no Bun cache in the row's folder, which is its HOME", async () => {
    const { code } = await spawnDispatch(["projects"]);
    expect({ code, cache: existsSync(join(stateDir, ".bun")) }).toEqual({ code: 0, cache: false });
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

  // A body holding a decision block is refused whatever its length (400 DECISION_BLOCK_IN_BODY,
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

// `dispatch result --project` is a CI job's own write, and the job has no session: it runs under
// `agent-secrets result-grant`, which gives it the airlock's placeholder of its run as
// DISPATCH_RESULTS_TOKEN, and the airlock proxy puts the run's results token in its place. The job
// trades nothing. The stand-ins below are the runner's token endpoint and the broker's token
// endpoint, which the job must ask nothing, and Dispatch's two project results routes.
describe("dispatch result --project, as a CI job writes it", () => {
  const entry = join(import.meta.dir, "..", "..", "bin", "dispatch.ts");
  /** The airlock's placeholder of the job's run, the run's OIDC token, the bearer the runner's
   *  endpoint takes, and a results token: none of them may reach the command's output or its
   *  argument list. */
  const PLACEHOLDER = "agent-secrets-placeholder-result-Run4242abcdefghijklmnopqrstu";
  const OIDC_TOKEN = fixtureSecret("oidc");
  const REQUEST_TOKEN = fixtureSecret("runner");
  const RESULTS_TOKEN = fixtureSecret("results");
  const SECRETS = [PLACEHOLDER, OIDC_TOKEN, REQUEST_TOKEN, RESULTS_TOKEN];

  interface Seen {
    readonly method: string;
    readonly path: string;
    readonly authorization: string | null;
    readonly dpop: string | null;
    readonly body: unknown;
  }
  const runnerAsked: { authorization: string | null; audience: string | null }[] = [];
  const trades: { form: URLSearchParams; proof: string }[] = [];
  const written: Seen[] = [];
  let brokerAnswer: () => Response | Promise<Response>;
  let dispatchAnswer: () => Response | Promise<Response>;
  /** Whether the stand-in answers the write naming no key back, as something in Dispatch's place
   *  does: Dispatch's own answer to a keyed write always names it. */
  let unkeyed = false;
  /** The result Dispatch stores for the job's write, as the route answers it. */
  const result = storedSpecResult(43, "pass", "", 1);
  const stored = routeAnswer<typeof result>("POST /api/v1/projects/{key}/results", result, 201);
  /** The answer of the write in Dispatch's own form, `body`, the stored result by default: the
   *  keyed layer the stand-in Dispatch answers behind names the write's key back on it. */
  const storedAnswer = (body: unknown = stored) => Response.json(body, { status: 201 });

  const runner = Bun.serve({
    port: 0,
    hostname: "127.0.0.1",
    fetch: async (request) => {
      runnerAsked.push({
        authorization: request.headers.get("Authorization"),
        audience: new URL(request.url).searchParams.get("audience"),
      });
      asked();
      return Response.json({ count: 1, value: OIDC_TOKEN });
    },
  });
  const broker = Bun.serve({
    port: 0,
    hostname: "127.0.0.1",
    fetch: async (request) => {
      trades.push({
        form: new URLSearchParams(await request.text()),
        proof: request.headers.get("DPoP") ?? "",
      });
      asked();
      return brokerAnswer();
    },
  });
  // Behind Dispatch's keyed layer, which names the write's key back on each 2xx and each 409 (its
  // KEY_COMMITTED, and a handler's refusal), unless the row says it answers as none does.
  const dispatch = Bun.serve({
    port: 0,
    hostname: "127.0.0.1",
    fetch: keyedLayer(
      async (request: Request) => {
        const text = await request.text();
        written.push({
          method: request.method,
          path: new URL(request.url).pathname,
          authorization: request.headers.get("Authorization"),
          dpop: request.headers.get("DPoP"),
          body: text === "" ? undefined : JSON.parse(text),
        });
        asked();
        return dispatchAnswer();
      },
      { judged: (response) => response.ok || response.status === 409, namesNoKey: () => unkeyed }
    ),
  });
  afterAll(() => {
    for (const stub of [runner, broker, dispatch]) stub.stop(true);
  });
  // A row on the fake clock that never ended left its timers faked: the next row starts real.
  const realSetTimeout = globalThis.setTimeout;
  beforeEach(() => {
    globalThis.setTimeout = realSetTimeout;
    jest.useRealTimers();
    runnerAsked.length = 0;
    trades.length = 0;
    written.length = 0;
    brokerAnswer = () =>
      Response.json(
        Object.fromEntries([
          ["access_token", RESULTS_TOKEN],
          ["issued_token_type", "urn:ietf:params:oauth:token-type:access_token"],
          ["token_type", "DPoP"],
          ["expires_in", 600],
        ])
      );
    dispatchAnswer = () => storedAnswer();
    unkeyed = false;
  });

  const dispatchUrl = `http://127.0.0.1:${dispatch.port}`;
  /** A job's environment as `agent-secrets result-grant` gives its command: no DISPATCH_HOST and
   *  no session, Dispatch's URL and the airlock's placeholder, and the broker's and the runner's
   *  variables of the step it runs in, of which the command reads none. */
  const jobEnvironment = (): Record<string, string | undefined> => ({
    PATH: process.env.PATH,
    HOME: stateDir,
    DISPATCH_STATE_DIR: stateDir,
    DISPATCH_URL: dispatchUrl,
    DISPATCH_RESULTS_TOKEN: PLACEHOLDER,
    AGENT_SECRETS_URL: `http://127.0.0.1:${broker.port}`,
    AGENT_SECRETS_MACHINE_AUDIENCE: "example-audience",
    ACTIONS_ID_TOKEN_REQUEST_URL: `http://127.0.0.1:${runner.port}/_apis/idtoken?api-version=2.0`,
    ACTIONS_ID_TOKEN_REQUEST_TOKEN: REQUEST_TOKEN,
  });
  const record = [
    "result",
    "--project",
    "ACME",
    "--check",
    "p-1",
    "--verdict",
    "pass",
    "--commit",
    SPEC_COMMIT,
  ];

  /** The command as its own process, as a job's step runs it. */
  async function spawnJob(
    argv: readonly string[],
    env: Record<string, string | undefined>
  ): Promise<{ code: number; stdout: string; stderr: string }> {
    const child = Bun.spawn([process.execPath, entry, ...argv], {
      cwd: stateDir,
      env: { ...env, ...CHILD_BUN },
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

  /** The command in this process, under exactly `env`, with its one bound `bound` where a test
   *  shortens it, and the write client's `fetchImpl` where a test watches its answers. */
  async function runJob(
    argv: readonly string[],
    env: Record<string, string | undefined>,
    bound?: ResultBound,
    fetchImpl?: typeof fetch
  ): Promise<{ code: number; stdout: string; stderr: string }> {
    let stdout = "";
    let stderr = "";
    const code = await runDispatchCli(argv, env, {
      stdout: (text) => {
        stdout += text;
      },
      stderr: (text) => {
        stderr += text;
      },
      readText: (path) => readFileSync(path, "utf-8"),
      cwd: stateDir,
      ...(bound === undefined ? {} : { resultBound: bound }),
      ...(fetchImpl === undefined ? {} : { fetchImpl }),
    });
    return { code, stdout, stderr };
  }

  /** Each row's wait on what the stand-ins were asked (`arrived`); every stand-in calls `asked`
   *  once it has recorded a request, and so does the write client's fetch once an answer's
   *  headers came (`answersHeard`). */
  const watches = new Set<{ readonly met: () => boolean; readonly resolve: () => void }>();
  let answersHeard = 0;
  function asked(): void {
    for (const watch of watches) {
      if (!watch.met()) continue;
      watches.delete(watch);
      watch.resolve();
    }
  }
  /** Settles once `met` holds of what the stand-ins were asked. */
  function arrived(met: () => boolean): Promise<void> {
    if (met()) return Promise.resolve();
    const { promise, resolve } = Promise.withResolvers<void>();
    watches.add({ met, resolve });
    return promise;
  }

  /** Lets the event loop run `count` turns, moving no clock. */
  async function turns(count: number): Promise<void> {
    for (let turn = 0; turn < count; turn++) {
      const { promise, resolve } = Promise.withResolvers<void>();
      setImmediate(resolve);
      await promise;
    }
  }

  /** The fake clock a row moves (`onFakeClock`). */
  interface FakeClock {
    /** Moves the clock `ms` on, firing each timer due by then. */
    readonly advance: (ms: number) => void;
    /** The delay of the timer the code sets `index`th (from 0) since the clock started, once it
     *  has set it: each wait the call takes (`sleep`, a write's pause, its 20 s silence cut). */
    readonly timerSet: (index: number) => Promise<number>;
    /** Settles once the call has ended. */
    readonly ended: Promise<void>;
    /** How far the clock has moved since the call started. */
    readonly elapsed: () => number;
  }

  /**
   * `runJob` on the test runner's fake timers, with how far that clock had moved when the call
   * ended (`ms`, undefined where it had not ended). The command's bound and each wait of the call
   * run on a clock that moves only when `drive` moves it, so a row judges when on that clock the
   * call ended, which the machine's load cannot change: on the real clock the 60 s bound once
   * read 246,969 ms on a loaded host. The stand-ins answer on real sockets, and the clock stands
   * still while they do, so `drive` moves it only once the call waits on it: a stand-in holds its
   * request (`arrived`), or the call has set the timer of a wait (`timerSet`). After `drive`, the
   * call has the event loop's turns to end in, and no time. `proxy`, where a row gives one, is the
   * proxy every request of the call goes through, as HTTPS_PROXY sends it in a process of its own.
   */
  async function onFakeClock(
    argv: readonly string[],
    env: Record<string, string | undefined>,
    bound: ResultBound | undefined,
    drive: (clock: FakeClock) => Promise<void> = (clock) => clock.ended,
    proxy?: string
  ): Promise<{ code: number; stdout: string; stderr: string; ms: number | undefined }> {
    jest.useFakeTimers();
    const faked = globalThis.setTimeout;
    const delays: number[] = [];
    const timerWatches = new Set<{ readonly index: number; readonly resolve: () => void }>();
    globalThis.setTimeout = ((handler: TimerHandler, delay?: number, ...rest: unknown[]) => {
      delays.push(delay ?? 0);
      for (const watch of timerWatches) {
        if (watch.index >= delays.length) continue;
        timerWatches.delete(watch);
        watch.resolve();
      }
      return faked(handler, delay, ...rest);
    }) as unknown as typeof setTimeout;
    answersHeard = 0;
    const heard = (async (input: RequestInfo | URL, init?: RequestInit) => {
      const response = await fetch(input, proxy === undefined ? init : { ...init, proxy });
      answersHeard += 1;
      asked();
      return response;
    }) as typeof fetch;
    const started = performance.now();
    const elapsed = () => performance.now() - started;
    let ms: number | undefined;
    const job = runJob(argv, env, bound, heard).finally(() => {
      ms = elapsed();
    });
    try {
      await drive({
        advance: (step) => jest.advanceTimersByTime(step),
        timerSet: async (index) => {
          if (index >= delays.length) {
            const { promise, resolve } = Promise.withResolvers<void>();
            timerWatches.add({ index, resolve });
            await promise;
          }
          return delays[index] as number;
        },
        ended: job.then(() => undefined),
        elapsed,
      });
      const ran = await Promise.race([job, turns(50).then(() => undefined)]);
      return ran === undefined
        ? { code: Number.NaN, stdout: "", stderr: "", ms: undefined }
        : { ...ran, ms };
    } finally {
      globalThis.setTimeout = faked;
      jest.useRealTimers();
    }
  }

  /** Moves the clock over each wait the call sets, as it sets it, until the call ends: a wait is
   *  every timer but the write's 20 s silence cut, which no row here reaches. */
  async function eachWait(clock: FakeClock): Promise<void> {
    for (let index = 0; ; index++) {
      const delay = await Promise.race([clock.ended.then(() => undefined), clock.timerSet(index)]);
      if (delay === undefined) return;
      if (delay !== DISPATCH_WRITE_RETRY.silenceMs) clock.advance(delay);
    }
  }

  /** Nothing went to the runner, the broker, Dispatch or the session's Dispatch. */
  const sentNothing = () => {
    expect(runnerAsked).toEqual([]);
    expect(trades).toEqual([]);
    expect(written).toEqual([]);
    expect(requests).toEqual([]);
  };

  /** No token is in what the command printed, nor in the argument list it ran with. */
  const namesNoToken = (argv: readonly string[], stdout: string, stderr: string) => {
    for (const secret of SECRETS) {
      expect(stdout, secret).not.toContain(secret);
      expect(stderr, secret).not.toContain(secret);
      expect(argv.join(" "), secret).not.toContain(secret);
    }
  };

  // The 30 s limit is the hang guard of the rows below that run the entry as a child process.
  test("a CI job's result goes out with the placeholder as its DPoP token and no proof of its own", async () => {
    // A test's output opens with `---`, so the job gives it in the `=` form, which reads no flag.
    const argv = [...record, "--observed=--- PASS: TestP (0.01s)"];
    const { code, stdout, stderr } = await spawnJob(argv, jobEnvironment());

    expect({ code, stderr }, stdout).toEqual({ code: 0, stderr: "" });
    expect(JSON.parse(stdout)).toEqual(stored);
    expect(written.map((seen) => [seen.method, seen.path, seen.authorization, seen.dpop])).toEqual([
      ["POST", "/api/v1/projects/ACME/results", `DPoP ${PLACEHOLDER}`, null],
    ]);
    expect({ runner: runnerAsked.length, broker: trades.length }).toEqual({ runner: 0, broker: 0 });
    namesNoToken(argv, stdout, stderr);
  }, 30_000);

  // The proxy block of the profile DISPATCH_RESULTS_TOKEN allows the results write to Dispatch's
  // host and nothing else, so the write asks Dispatch nothing first and nothing after: the
  // stand-in counts every request.
  test("the job's write sends one POST and nothing else to Dispatch's host", async () => {
    const { code, stdout } = await runJob(record, jobEnvironment());
    expect(code, stdout).toBe(0);
    expect(written.map((seen) => `${seen.method} ${seen.path}`)).toEqual([
      "POST /api/v1/projects/ACME/results",
    ]);
    expect(requests).toEqual([]);
  });

  test("with no DISPATCH_RESULTS_TOKEN it refuses before any request, naming agent-secrets result-grant", async () => {
    const { DISPATCH_RESULTS_TOKEN: _placeholder, ...none } = jobEnvironment();
    for (const env of [none, { ...none, DISPATCH_RESULTS_TOKEN: "" }]) {
      const { code, stdout } = await runJob(record, env);
      expect({ code, stdout }).toEqual({
        code: 2,
        stdout:
          "dispatch: dispatch result --project runs under `agent-secrets result-grant -- …`, which gives it DISPATCH_RESULTS_TOKEN; this environment holds none, so nothing was sent\n",
      });
    }
    // A value that cannot be a header's is refused by its variable, and quoted nowhere.
    const spaced = await runJob(record, { ...none, DISPATCH_RESULTS_TOKEN: `${PLACEHOLDER} x` }); // fixture-secret: invalid placeholder refused before any client can hold it.
    expect({ code: spaced.code, stdout: spaced.stdout }).toEqual({
      code: 2,
      stdout:
        "dispatch: DISPATCH_RESULTS_TOKEN holds white space or a control character, so it is no placeholder `agent-secrets result-grant` gives, and nothing was sent\n",
    });
    sentNothing();
  });

  // A job's step environment names the broker and the identity agent-secrets traded, and the
  // command it runs inherits them: the write reads none of them, and trades nothing.
  test("AGENT_SECRETS_MACHINE_IDENTITY=github-actions asks the broker nothing", async () => {
    const { code, stdout } = await runJob(record, {
      ...jobEnvironment(),
      AGENT_SECRETS_MACHINE_IDENTITY: "github-actions",
    });
    expect(code, stdout).toBe(0);
    expect({
      broker: trades.length,
      runner: runnerAsked.length,
      sent: written.map((seen) => [seen.authorization, seen.dpop]),
    }).toEqual({ broker: 0, runner: 0, sent: [[`DPoP ${PLACEHOLDER}`, null]] });
  });

  test("a refusal names no credential either", async () => {
    const refusal = `The files of .dispatch/spec at ${SPEC_COMMIT} hold no check p-9.`;
    dispatchAnswer = () =>
      Response.json({ code: "CHECK_NOT_IN_FILES", error: refusal }, { status: 409 });
    const argv = record.map((word) => (word === "p-1" ? "p-9" : word));
    const dispatchRefused = await spawnJob(argv, jobEnvironment());
    expect(dispatchRefused.code, dispatchRefused.stdout).toBe(1);
    expect(dispatchRefused.stdout.trim()).toBe(`CHECK_NOT_IN_FILES: ${refusal}`);
    namesNoToken(argv, dispatchRefused.stdout, dispatchRefused.stderr);
  }, 30_000);

  // A job's step writes `--project "$PROJECT" --check "$CHECK" --commit "$SHA"`, and an empty
  // variable is the honest way to send one empty: it is refused before any write, naming the flag.
  test("an empty --project, --check or --commit is refused before any request, naming the flag", async () => {
    for (const [flag, noun] of [
      ["--project", "project"],
      ["--check", "check"],
      ["--commit", "commit"],
    ] as const) {
      const argv = [...record];
      argv[argv.indexOf(flag) + 1] = "";
      const { code, stdout, stderr } = await runJob(argv, jobEnvironment());
      expect({ code, stderr, lines: stdout.split("\n").slice(0, 2) }, flag).toEqual({
        code: 2,
        stderr: "",
        lines: [
          "dispatch result was not called: 1 problem",
          `- ${flag} is empty, so the call names no ${noun}: a variable that gives it may be empty or unset`,
        ],
      });
    }
    sentNothing();
  });

  /** Dispatch's `409 KEY_COMMITTED` to the write it answers now, which its keyed layer names the
   *  write's key back on, or the key `other` names where a test gives another request's. */
  const keyCommitted = (other?: string) =>
    Response.json(
      { code: "KEY_COMMITTED", error: "the write landed and its answer was lost" },
      { status: 409, headers: other === undefined ? {} : { "Idempotency-Key": other } }
    );

  // A write whose answer was lost after Dispatch stored it (409 KEY_COMMITTED) is done, and the
  // job's log is read by a program: it prints the JSON the session path records for the same
  // case and exits 0. It gives no advice, since a results token reads nothing back.
  test("a result stored with its answer lost prints landed_without_answer as JSON and exits 0", async () => {
    dispatchAnswer = () => keyCommitted();
    const { code, stdout, stderr } = await runJob(record, jobEnvironment());
    expect({ code, stdout, stderr }).toEqual({
      code: 0,
      stdout: `${JSON.stringify({ landed_without_answer: "KEY_COMMITTED" })}\n`,
      stderr: "",
    });
    expect(written.map((seen) => [seen.method, seen.path])).toEqual([
      ["POST", "/api/v1/projects/ACME/results"],
    ]);
  });

  // A landing needs the key Dispatch names back, on a 409 as on a 2xx: a KEY_COMMITTED that names
  // none, or another request's, is no verdict of Dispatch's on this write, so the result may or
  // may not be stored, and a second run is safe.
  test("a 409 KEY_COMMITTED that does not name the write's key back prints write_outcome_unknown and exits 1", async () => {
    for (const named of ["", "another-request-key"]) {
      written.length = 0;
      // One names no key back, as something in Dispatch's place answers; the other names another
      // request's.
      unkeyed = named === "";
      dispatchAnswer = () => keyCommitted(named === "" ? undefined : named);
      const { code, stdout, stderr } = await runJob(record, jobEnvironment());
      expect({ code, stdout, stderr }, named).toEqual({
        code: 1,
        stdout: `${JSON.stringify({ write_outcome_unknown: "HTTP_409" })}\n`,
        stderr: "",
      });
      expect(written.map((seen) => [seen.method, seen.path])).toEqual([
        ["POST", "/api/v1/projects/ACME/results"],
      ]);
    }
  });

  /** A proxy in front of the stand-in Dispatch that sends each request on, and closes the job's
   *  connection in place of passing on the answer of each results write that `drops` names (by
   *  its number, from 1), and of every other request when `dropOthers`: the write lands, and the
   *  job hears nothing back. */
  function lossyProxy(drops: (write: number) => boolean, dropOthers: boolean) {
    let writes = 0;
    interface Link {
      upstream: Promise<Socket<undefined>>;
      drop: boolean;
    }
    const proxy = Bun.listen<Link>({
      hostname: "127.0.0.1",
      port: 0,
      socket: {
        open(client) {
          client.data = {
            drop: false,
            upstream: Bun.connect({
              hostname: "127.0.0.1",
              port: Number(dispatch.port),
              socket: {
                data(_upstream, answer) {
                  if (client.data.drop) client.end();
                  else client.write(answer);
                },
                close() {
                  client.end();
                },
              },
            }),
          };
        },
        // A request's first chunk opens with its request line; the rest of it is its body.
        async data(client, request) {
          const line = request.subarray(0, 64).toString("latin1");
          if (/^[A-Z]+ \//.test(line)) {
            const write = line.startsWith("POST /api/v1/projects/");
            if (write) writes += 1;
            client.data.drop = write ? drops(writes) : dropOthers;
          }
          (await client.data.upstream).write(request);
        },
        close(client) {
          void client.data.upstream.then((upstream) => upstream.end());
        },
      },
    });
    return { url: `http://127.0.0.1:${proxy.port}`, stop: () => proxy.stop(true) };
  }

  // A write that went out and heard nothing back may have stored the result, here it did: the
  // proxy drops the answer of the write. The command's bound leaves no time for a pause
  // (`oneAttempt`), so the write is not asked again. The job's step still fails, since the write
  // is not confirmed, and its one line says so to a program.
  test("a write that went out and heard no answer prints write_outcome_unknown and exits 1", async () => {
    const proxy = lossyProxy(() => true, false);
    try {
      const ran = await runJob(
        record,
        { ...jobEnvironment(), DISPATCH_URL: proxy.url },
        oneAttempt
      );
      expect(ran).toEqual({
        code: 1,
        stdout: `${JSON.stringify({ write_outcome_unknown: "ECONNRESET" })}\n`,
        stderr: "",
      });
      expect(written.map((seen) => [seen.method, seen.path])).toEqual([
        ["POST", "/api/v1/projects/ACME/results"],
      ]);
    } finally {
      proxy.stop();
    }
  });

  // The control: a connection refused before any request went out reached nothing, so the write
  // stored nothing, and the command prints the transport error and no write_outcome_unknown.
  test("a write whose connection was refused prints no write_outcome_unknown", async () => {
    const closed = Bun.serve({ port: 0, hostname: "127.0.0.1", fetch: () => new Response() });
    const url = `http://127.0.0.1:${closed.port}`;
    closed.stop(true);
    const { code, stdout, stderr } = await runJob(
      record,
      { ...jobEnvironment(), DISPATCH_URL: url },
      oneAttempt
    );
    expect({ code, stderr, unknown: stdout.includes("write_outcome_unknown") }).toEqual({
      code: 1,
      stderr: "",
      unknown: false,
    });
    expect(stdout).toContain("is unreachable: ECONNREFUSED.");
    expect(written).toEqual([]);
  });

  // A landed write whose first answer the proxy drops is asked again under its key, and Dispatch
  // answers KEY_COMMITTED: the old line, byte for byte, and exit 0.
  test("a landed write whose answer is lost and asked again still prints landed_without_answer", async () => {
    const proxy = lossyProxy((write) => write === 1, false);
    dispatchAnswer = () =>
      written.filter((seen) => seen.method === "POST").length === 1
        ? storedAnswer()
        : keyCommitted();
    try {
      const ran = await runJob(record, { ...jobEnvironment(), DISPATCH_URL: proxy.url });
      expect(ran).toEqual({
        code: 0,
        stdout: `${JSON.stringify({ landed_without_answer: "KEY_COMMITTED" })}\n`,
        stderr: "",
      });
      expect(written.map((seen) => [seen.method, seen.path])).toEqual([
        ["POST", "/api/v1/projects/ACME/results"],
        ["POST", "/api/v1/projects/ACME/results"],
      ]);
    } finally {
      proxy.stop();
    }
  }, 30_000);

  /** A bound of the command short enough for a test, on the fake clock each of its rows runs on
   *  (`onFakeClock`): no round starts later than `reserveMs` before `totalMs`. */
  const shortBound = (totalMs: number, reserveMs: number): ResultBound => ({ totalMs, reserveMs });
  /** A bound whose window to ask a write again (its total less its reserve, 0.5 s) ends before the
   *  write's first pause of 1 s, so a write that gets no definitive answer makes its one attempt. */
  const oneAttempt = shortBound(60_000, 59_500);

  // Dispatch answers 201 only after it stored the result, and names the request's key back on it
  // (`TestEveryKeyedAnswerNamesItsKey`), but a body that does not arrive whole is no answer the job
  // can read (the write table, write-outcome.ts): a body cut short or broken off is asked again
  // under the key (`body-broken`, `body-malformed`), and one that stalls ends at the command's own
  // bound (B4 of the review of 2f29345b8d, its two rows). The bound here ends before the first
  // pause, so the job prints write_outcome_unknown, which a second run makes safe, and exits 1,
  // within the bound. The stand-in Dispatch is a raw socket, so the status line and the first bytes
  // of the body surely reach the job before the connection closes or goes silent.
  // A body cut short, or one that is not the JSON it says it is, ends the write by itself: no ask
  // fits in the bound's window, so nothing waits on the clock, and its row leaves the fake clock
  // (`onFakeClock`) where it is. The line then names the body's own failure, a plain `Error`. A
  // row that moved the clock once the 201's headers came would race the bound against the body:
  // the bound won on an idle machine and the cut on a loaded one, so that row tested the bound and
  // failed under load. Only the body that stalls waits for the bound, so only its row moves the
  // clock, once the 201's headers came, and its line names the bound, `TimeoutError`.
  const bodyStart = '{"id":"00000000-0000-4000-8000-';
  /** The `Idempotency-Key` a raw request carries, which a stand-in of Dispatch on a raw socket names
   *  back itself: `keyedLayer` wraps a fetch-shaped answer, and a socket gives none. */
  const keyOf = (request: Buffer): string =>
    /\r\nidempotency-key: *([^\r\n]*)/i.exec(request.toString("latin1"))?.[1]?.trim() ?? "";
  for (const [name, length, close, endedBy] of [
    ["is cut", 400, true, "Error"],
    ["stalls past the bound", 400, false, "TimeoutError"],
    ["is not the JSON it says it is", bodyStart.length, false, "Error"],
  ] as const) {
    test(`a 201 whose body ${name} prints write_outcome_unknown and exits 1`, async () => {
      let posts = 0;
      const lost = Bun.listen({
        hostname: "127.0.0.1",
        port: 0,
        socket: {
          data(socket, request) {
            if (!request.subarray(0, 5).toString("latin1").startsWith("POST ")) return;
            posts += 1;
            socket.write(
              "HTTP/1.1 201 Created\r\nContent-Type: application/json\r\n" +
                `Idempotency-Key: ${keyOf(request)}\r\n` +
                `Content-Length: ${length}\r\n\r\n${bodyStart}`
            );
            if (close) socket.end();
          },
        },
      });
      try {
        const ran = await onFakeClock(
          record,
          { ...jobEnvironment(), DISPATCH_URL: `http://127.0.0.1:${lost.port}` },
          shortBound(1_000, 200),
          endedBy === "TimeoutError"
            ? async (clock) => {
                await Promise.race([clock.ended, arrived(() => answersHeard >= 1)]);
                clock.advance(1_000);
                await clock.ended;
              }
            : (clock) => clock.ended
        );
        expect({
          code: ran.code,
          stdout: ran.stdout,
          stderr: ran.stderr,
          posts,
          withinTheBound: ran.ms !== undefined && ran.ms <= 1_000,
          clockStill: ran.ms === 0,
        }).toEqual({
          code: 1,
          stdout: `${JSON.stringify({ write_outcome_unknown: endedBy })}\n`,
          stderr: "",
          posts: 1,
          withinTheBound: true,
          clockStill: endedBy === "Error",
        });
      } finally {
        lost.stop(true);
      }
    }, 30_000);
  }

  /** A stand-in Dispatch on a raw socket that answers every POST with `status`, `type` and the
   *  whole of `body`, naming back the request's key itself (`keyOf`, since no fetch-shaped layer
   *  wraps a socket) as Dispatch does (`this`), none, or another request's (`other`), counting the
   *  posts. */
  function answeringWhole(
    status: string,
    type: string,
    body: string,
    echo: "this" | "none" | "other" = "this"
  ) {
    const seen = { posts: 0 };
    const server = Bun.listen({
      hostname: "127.0.0.1",
      port: 0,
      socket: {
        data(socket, request) {
          if (!request.subarray(0, 5).toString("latin1").startsWith("POST ")) return;
          seen.posts += 1;
          const key = keyOf(request);
          const named =
            echo === "none"
              ? ""
              : `Idempotency-Key: ${echo === "this" ? key : `${key}-another`}\r\n`;
          socket.write(
            `HTTP/1.1 ${status}\r\nContent-Type: ${type}\r\n${named}` +
              `Content-Length: ${Buffer.byteLength(body)}\r\n\r\n${body}`
          );
          socket.end();
        },
      },
    });
    return { seen, server };
  }

  // A 201 whose body comes whole and still gives no stored result: JSON of another shape (D-1 of
  // the acceptance of 8ae6f5bcb4) and JSON null are a landing, since the client reads a write's 2xx
  // into the row its callers read (`dispatch-write-rows.ts`), and an answer that is not that row is
  // ANSWER_LOST, which the reporter reads as stored. It printed `Cannot destructure property
  // 'predicate' …` and exited 1. A body that is no JSON under another type, or empty, is no answer
  // Dispatch writes (`foreign-2xx`, write-outcome.ts), so its outcome is unknown.
  for (const [name, type, body, printed, code] of [
    [
      "is no JSON, typed text/html",
      "text/html",
      "<html><body>not json</body></html>",
      { write_outcome_unknown: "Error" },
      1,
    ],
    ["is empty, typed JSON", "application/json", "", { write_outcome_unknown: "Error" }, 1],
    ["is the JSON {}", "application/json", "{}", { landed_without_answer: "ANSWER_LOST" }, 0],
    ["is JSON null", "application/json", "null", { landed_without_answer: "ANSWER_LOST" }, 0],
  ] as const) {
    test(`a 201 whose whole body ${name} prints ${Object.keys(printed)[0]} and exits ${code}`, async () => {
      const { seen, server } = answeringWhole("201 Created", type, body);
      try {
        const ran = await onFakeClock(
          record,
          { ...jobEnvironment(), DISPATCH_URL: `http://127.0.0.1:${server.port}` },
          shortBound(1_000, 200)
        );
        expect({
          code: ran.code,
          stdout: ran.stdout,
          stderr: ran.stderr,
          posts: seen.posts,
        }).toEqual({
          code,
          stdout: `${JSON.stringify(printed)}\n`,
          stderr: "",
          posts: 1,
        });
      } finally {
        server.stop(true);
      }
    }, 30_000);
  }

  // The controls of the rows above: the true stored result, on the same raw socket, is printed
  // whole with its id; and a refusal, whatever its body, stays a refusal that stored nothing.
  test("a 201 whose whole body is the stored result prints it with its id and exits 0", async () => {
    const { seen, server } = answeringWhole(
      "201 Created",
      "application/json",
      JSON.stringify(stored)
    );
    try {
      const ran = await onFakeClock(
        record,
        { ...jobEnvironment(), DISPATCH_URL: `http://127.0.0.1:${server.port}` },
        shortBound(1_000, 200)
      );
      expect({ code: ran.code, stderr: ran.stderr, posts: seen.posts }).toEqual({
        code: 0,
        stderr: "",
        posts: 1,
      });
      expect(JSON.parse(ran.stdout)).toEqual(stored);
      expect(JSON.parse(ran.stdout).id).toBe(stored.id);
    } finally {
      server.stop(true);
    }
  }, 30_000);

  for (const [name, type, body] of [
    [
      "Dispatch's own refusal",
      "application/json",
      '{"code":"CHECK_NOT_IN_FILES","error":"no such check"}',
    ],
    ["a page that is no JSON", "text/html", "<html><body>refused</body></html>"],
    ["an empty body", "application/json", ""],
    ["the JSON {}", "application/json", "{}"],
  ] as const) {
    test(`a 409 whose body is ${name} stays a refusal, exit 1, and no landing`, async () => {
      const { server } = answeringWhole("409 Conflict", type, body);
      try {
        const ran = await onFakeClock(
          record,
          { ...jobEnvironment(), DISPATCH_URL: `http://127.0.0.1:${server.port}` },
          shortBound(1_000, 200)
        );
        expect({ code: ran.code, landed: ran.stdout.includes("landed_without_answer") }).toEqual({
          code: 1,
          landed: false,
        });
        if (name === "Dispatch's own refusal") {
          expect(ran.stdout).toBe("CHECK_NOT_IN_FILES: no such check\n");
        }
      } finally {
        server.stop(true);
      }
    }, 30_000);
  }

  // A 2xx that is not Dispatch's answer to the write - it names no key back and gives no stored
  // result, or names another request's key back - came from whatever answers in Dispatch's place
  // (a proxy's page, a server that is not Dispatch), which may have passed the write on first:
  // the job prints write_outcome_unknown and exits 1, never landed_without_answer, which its
  // reporter reads as stored (B5 of the review of 355f8ada68). Each runs the command as its own
  // process, as a job's step runs it.
  for (const [name, status, type, body, echo, code] of [
    [
      "a 200 page that names no key back",
      "200 OK",
      "text/html",
      "<html><body>OK</body></html>",
      "none",
      "HTTP_200",
    ],
    [
      "a 200 with the stored result that names another request's key back",
      "200 OK",
      "application/json",
      JSON.stringify(stored),
      "other",
      "HTTP_200",
    ],
    ["a 204 that names no key back", "204 No Content", "application/json", "", "none", "HTTP_204"],
    [
      "a 201 of JSON from a server that is no Dispatch",
      "201 Created",
      "application/json",
      '{"ok":true}',
      "none",
      "HTTP_201",
    ],
  ] as const) {
    test(`${name} prints write_outcome_unknown and exits 1`, async () => {
      const { seen, server } = answeringWhole(status, type, body, echo);
      try {
        const ran = await spawnJob(record, {
          ...jobEnvironment(),
          DISPATCH_URL: `http://127.0.0.1:${server.port}`,
        });
        expect({ code: ran.code, stdout: ran.stdout, posts: seen.posts }).toEqual({
          code: 1,
          stdout: `${JSON.stringify({ write_outcome_unknown: code })}\n`,
          posts: 1,
        });
      } finally {
        server.stop(true);
      }
    }, 30_000);
  }

  // Dispatch's keyed layer names the key back on every answer of every POST and PATCH route, so a
  // 201 that names no key back is no answer of Dispatch's to this write, whatever result it holds:
  // this call's own, another run's of the same check, verdict and commit (the hardening note H1 of the
  // deep audit of 7735fa52b4), or another call's of another project, check, verdict or commit (a
  // proxy or a cache that answers one write with another's). The write may have reached Dispatch
  // behind what answered: write_outcome_unknown, exit 1. No content of the answer is compared.
  const { predicate, subject } = stored.attestation;
  /** Another call's stored result: the job's own, its attestation changed as `change` says. */
  const another = (change: Partial<typeof stored.attestation>): typeof stored => ({
    ...stored,
    attestation: { ...stored.attestation, ...change },
  });
  for (const [held, body] of [
    ["this call's own stored result", stored],
    [
      "another run's stored result of the same check, verdict and commit",
      routeAnswer(
        "POST /api/v1/projects/{key}/results",
        storedSpecResult(44, "pass", "macos", 2),
        201
      ),
    ],
    [
      "another call's stored result, of another project",
      another({ predicate: { ...predicate, project: "OTHER" } }),
    ],
    [
      "another call's stored result, of another check",
      another({ predicate: { ...predicate, check: "p-2" } }),
    ],
    [
      "another call's stored result, of another verdict",
      another({ predicate: { ...predicate, verdict: "fail" } }),
    ],
    [
      "another call's stored result, of another commit",
      another({
        subject: subject.map((item) => ({ ...item, digest: { gitCommit: "2".repeat(40) } })),
      }),
    ],
  ] as const) {
    test(`a 201 with ${held} that names no key back prints write_outcome_unknown and exits 1`, async () => {
      const { seen, server } = answeringWhole(
        "201 Created",
        "application/json",
        JSON.stringify(body),
        "none"
      );
      try {
        const ran = await spawnJob(record, {
          ...jobEnvironment(),
          DISPATCH_URL: `http://127.0.0.1:${server.port}`,
        });
        expect({ code: ran.code, stdout: ran.stdout, posts: seen.posts }).toEqual({
          code: 1,
          stdout: `${JSON.stringify({ write_outcome_unknown: "HTTP_201" })}\n`,
          posts: 1,
        });
      } finally {
        server.stop(true);
      }
    }, 30_000);
  }

  // Dispatch's own 204 to the write would name the key back: a body that gives no stored result
  // under it is the landing, as for its 201.
  test("a 204 that names the key back prints landed_without_answer and exits 0", async () => {
    const { seen, server } = answeringWhole("204 No Content", "application/json", "");
    try {
      const ran = await spawnJob(record, {
        ...jobEnvironment(),
        DISPATCH_URL: `http://127.0.0.1:${server.port}`,
      });
      expect({ code: ran.code, stdout: ran.stdout, posts: seen.posts }).toEqual({
        code: 0,
        stdout: `${JSON.stringify({ landed_without_answer: "ANSWER_LOST" })}\n`,
        posts: 1,
      });
    } finally {
      server.stop(true);
    }
  }, 30_000);

  // Dispatch never answers a write with a redirect; a proxy in front of it sends one to its
  // sign-in page. The write follows none: the 302 is the answer, a refusal that names it with
  // exit 1, and the page is never asked for, so its 200 is never read as the write's.
  test("a 302 to a sign-in page is not followed, and is a refusal naming it", async () => {
    const seen = { posts: 0, pages: 0 };
    const proxy = Bun.serve({
      port: 0,
      fetch(request) {
        if (request.method === "POST") {
          seen.posts += 1;
          return new Response(null, { status: 302, headers: { Location: "/login" } });
        }
        seen.pages += 1;
        return new Response("<html><body>Sign in</body></html>", {
          headers: { "Content-Type": "text/html" },
        });
      },
    });
    try {
      const ran = await spawnJob(record, {
        ...jobEnvironment(),
        DISPATCH_URL: `http://127.0.0.1:${proxy.port}`,
      });
      expect({ code: ran.code, posts: seen.posts, pages: seen.pages }).toEqual({
        code: 1,
        posts: 1,
        pages: 0,
      });
      expect(ran.stdout).toStartWith(
        `HTTP_302: POST http://127.0.0.1:${proxy.port}/api/v1/projects/ACME/results answered 302`
      );
    } finally {
      proxy.stop(true);
    }
  }, 30_000);

  /** The results writes Dispatch got so far, the one it answers now included. */
  const posts = (): number => written.filter((seen) => seen.method === "POST").length;
  /** Dispatch's two answers of its check of a broker token that judged nothing, before any
   *  handler ran (`brokerTokenActor`): a spent bucket of checks, and a broker that answered
   *  nothing. */
  const tokenNotChecked = {
    limited: () =>
      Response.json(
        {
          code: "BROKER_TOKEN_RATE_LIMITED",
          error: "too many access tokens checked from this address; try again in 200ms",
        },
        { status: 429, headers: { "Retry-After": "1" } }
      ),
    unavailable: () =>
      Response.json(
        {
          code: "BROKER_UNAVAILABLE",
          error: "the secrets broker cannot be reached; try again in a minute",
        },
        { status: 503 }
      ),
  };
  /** The ways a call is turned away before anything is stored: Dispatch's check of the token, its
   *  429 and its 503 for a broker that answered nothing. Each sets the stand-in up to turn away
   *  `times` calls, then to answer as usual. */
  const turnedAway: readonly (readonly [name: string, setUp: (times: number) => void])[] = [
    [
      "Dispatch's 429 of its token check",
      (times) => {
        dispatchAnswer = () => (posts() <= times ? tokenNotChecked.limited() : storedAnswer());
      },
    ],
    [
      "Dispatch's 503 for a broker that answered nothing",
      (times) => {
        dispatchAnswer = () => (posts() <= times ? tokenNotChecked.unavailable() : storedAnswer());
      },
    ],
  ];

  // Dispatch's check of the job's token answers before any handler runs, so a write it turns away
  // did nothing: it is asked again after the answer's Retry-After, or a second, and one result is
  // stored.
  test("a call Dispatch's token check turns away is asked again, and the result is stored", async () => {
    const defaults = { dispatchAnswer };
    for (const [name, setUp] of turnedAway) {
      ({ dispatchAnswer } = defaults);
      written.length = 0;
      setUp(2);
      const { code, stdout, stderr, ms } = await onFakeClock(
        record,
        jobEnvironment(),
        shortBound(5_000, 1_000),
        eachWait
      );
      expect(
        {
          code,
          stderr,
          waited: ms !== undefined && ms >= 2_000,
          stored: code === 0 ? JSON.parse(stdout) : stdout,
        },
        name
      ).toEqual({ code: 0, stderr: "", waited: true, stored });
      expect(posts(), name).toBe(3);
    }
  }, 30_000);

  /** The one line a call turned away for its whole bound of `seconds` ends with: which turned
   *  it away, then that answer's own words. */
  const turnedAwayLines = (seconds: number): Record<string, string> => ({
    "Dispatch's 429 of its token check": `Dispatch's check of the job's token turned the write away, and the ${seconds} s this command may take leave no time to ask again: BROKER_TOKEN_RATE_LIMITED: too many access tokens checked from this address; try again in 200ms`,
    "Dispatch's 503 for a broker that answered nothing": `Dispatch's check of the job's token turned the write away, and the ${seconds} s this command may take leave no time to ask again: BROKER_UNAVAILABLE: the secrets broker cannot be reached; try again in a minute`,
  });

  // Turned away for ever, the call ends at its one bound with one line that says which turned it
  // away, and no write_outcome_unknown: no write ran.
  test("a call Dispatch's token check turns away for ever ends at the one bound with one line", async () => {
    const defaults = { dispatchAnswer };
    const lines = turnedAwayLines(3);
    for (const [name, setUp] of turnedAway) {
      ({ dispatchAnswer } = defaults);
      written.length = 0;
      setUp(Number.POSITIVE_INFINITY);
      const { code, stdout, stderr, ms } = await onFakeClock(
        record,
        jobEnvironment(),
        shortBound(3_000, 500),
        eachWait
      );
      expect(
        {
          code,
          stderr,
          lines: stdout.split("\n").length,
          withinTheBound: ms !== undefined && ms < 3_000,
        },
        name
      ).toEqual({
        code: 1,
        stderr: "",
        lines: 2,
        withinTheBound: true,
      });
      expect(stdout, name).toStartWith(lines[name] ?? "");
    }
  }, 30_000);

  // The case of the fresh acceptance of f60429ff0d (its D-A): the write client asks again under
  // the key inside each call, while Dispatch's check of the token turns every attempt away. The
  // client's deadline and the command's waits are one bound on one clock: the call ends inside it
  // with the refusal that names the last code, and no write_outcome_unknown, since no handler ever
  // ran. Before the one bound the client's own 60 s deadline, counted apart from the command's
  // waits, ended the call first, and an attempt it ended before it was sent read as one that may
  // have stored.
  for (const [name, answer, code] of [
    ["503 BROKER_UNAVAILABLE", tokenNotChecked.unavailable, "BROKER_UNAVAILABLE"],
    ["429 BROKER_TOKEN_RATE_LIMITED", tokenNotChecked.limited, "BROKER_TOKEN_RATE_LIMITED"],
  ] as const) {
    test(`Dispatch's token check turning every write away with ${name}, against the keyed layer, ends at the one bound naming ${code}`, async () => {
      dispatchAnswer = answer;
      const ran = await onFakeClock(record, jobEnvironment(), shortBound(8_000, 1_000), eachWait);
      expect(
        {
          code: ran.code,
          stderr: ran.stderr,
          lines: ran.stdout.split("\n").length,
          namesTheCause: ran.stdout.startsWith(
            "Dispatch's check of the job's token turned the write away"
          ),
          namesTheCode: ran.stdout.includes(`: ${code}: `),
          outcomeUnknown: ran.stdout.includes("write_outcome_unknown"),
          askedAgainUnderTheKey: posts() > 2,
          withinTheBound: ran.ms !== undefined && ran.ms < 8_000,
        },
        ran.stdout
      ).toEqual({
        code: 1,
        stderr: "",
        lines: 2,
        namesTheCause: true,
        namesTheCode: true,
        outcomeUnknown: false,
        askedAgainUnderTheKey: true,
        withinTheBound: true,
      });
    }, 90_000);
  }

  // The client's deadline is the command's bound: a Dispatch that never answers ends the call at
  // the bound, as a write whose outcome is unknown, since the attempt the bound cut was sent.
  test("a Dispatch that never answers ends the call at the one bound as an outcome unknown", async () => {
    const { promise: never } = Promise.withResolvers<Response>();
    dispatchAnswer = () => never;
    const ran = await onFakeClock(
      record,
      jobEnvironment(),
      shortBound(3_000, 500),
      async (clock) => {
        await arrived(() => written.length >= 1);
        clock.advance(3_000);
        // The bound aborts the write's request, which ends on the event loop's own time.
        await clock.ended;
      }
    );
    expect({
      code: ran.code,
      stdout: ran.stdout,
      stderr: ran.stderr,
      atTheBound: ran.ms === 3_000,
    }).toEqual({
      code: 1,
      stdout: `${JSON.stringify({ write_outcome_unknown: "TimeoutError" })}\n`,
      stderr: "",
      atTheBound: true,
    });
  }, 90_000);

  /** Dispatch's public host as the job's write names it behind the airlock. No resolver here
   *  answers it, so the write reaches nothing but the proxy. */
  const airlocked = "https://dispatch.ci-results.test";
  /** The proxy's URL as `agent-secrets result-grant` writes it in HTTPS_PROXY: the placeholder is
   *  its password, so no line the command prints may quote it. */
  const proxyUrl = (proxy: StubProxy): string =>
    proxy.url.replace("http://", `http://agent-secrets:${PLACEHOLDER}@`);
  /** The headers the airlock proxy writes on an answer of its own. */
  const AIRLOCK_FORWARDED = "Cipher-Proxy-Forwarded";
  const AIRLOCK_CODE = "Cipher-Proxy-Code";

  // The airlock proxy answers a CONNECT whose password names no live grant with 407, and Bun tells
  // the caller its status line alone (ERR_PROXY_TUNNEL). Nothing went on to Dispatch, so the write
  // ends at once and says so, with no write_outcome_unknown: asked again, the proxy answers the
  // same. The command runs as its own process, with the proxy in the HTTPS_PROXY its step has.
  test("a CONNECT the proxy refuses with 407 ends the write at once: nothing was sent, no write_outcome_unknown", async () => {
    const proxy = startStubProxy(() => ({
      status: 407,
      reason: "Proxy Authentication Required",
      code: "GRANT_NOT_LIVE",
    }));
    try {
      const ran = await spawnJob(record, {
        ...jobEnvironment(),
        DISPATCH_URL: airlocked,
        HTTPS_PROXY: proxyUrl(proxy),
      });
      expect({
        code: ran.code,
        stdout: ran.stdout,
        stderr: ran.stderr,
        connects: proxy.connects,
      }).toEqual({
        code: 1,
        stdout:
          "AIRLOCK_TUNNEL_REFUSED: the airlock proxy refused the connection (407 Proxy Authentication Required); nothing was sent to Dispatch\n",
        stderr: "",
        connects: ["dispatch.ci-results.test:443"],
      });
      expect(written).toEqual([]);
      namesNoToken(record, ran.stdout, ran.stderr);
    } finally {
      proxy.stop();
    }
  }, 30_000);

  // A 503 at the CONNECT is a connection that never opened, as a refused one is: the write asks
  // again within its bound, on the schedule of every write, and then ends with that refusal, its
  // status named, and no write_outcome_unknown. It runs on the fake clock, through the proxy as
  // fetch's own option, the form HTTPS_PROXY takes in a process of its own.
  test("a CONNECT the proxy refuses with 503 is asked again within the bound, and ends naming 503 and nothing sent", async () => {
    const proxy = startStubProxy(() => ({
      status: 503,
      reason: "Service Unavailable",
      code: "GRANT_UNREADABLE",
    }));
    try {
      const ran = await onFakeClock(
        record,
        { ...jobEnvironment(), DISPATCH_URL: airlocked },
        shortBound(5_000, 1_000),
        eachWait,
        proxyUrl(proxy)
      );
      expect({
        code: ran.code,
        stdout: ran.stdout,
        stderr: ran.stderr,
        askedAgain: proxy.connects.length >= 2,
        withinTheBound: ran.ms !== undefined && ran.ms < 5_000,
      }).toEqual({
        code: 1,
        stdout:
          "AIRLOCK_TUNNEL_REFUSED: the airlock proxy refused the connection (503 Service Unavailable); nothing was sent to Dispatch\n",
        stderr: "",
        askedAgain: true,
        withinTheBound: true,
      });
      expect(written).toEqual([]);
    } finally {
      proxy.stop();
    }
  }, 30_000);

  // The tunnel's refusal says nothing of an earlier attempt that may have reached Dispatch: here
  // the proxy opens the first tunnel and breaks it after the write went into it, and refuses the
  // next CONNECT 407. The write may be stored, so the command prints write_outcome_unknown, never
  // that nothing was sent.
  test("a CONNECT refused after an attempt that may have reached Dispatch prints write_outcome_unknown", async () => {
    const proxy = startStubProxy((connect) =>
      connect === 1
        ? "open-then-close"
        : { status: 407, reason: "Proxy Authentication Required", code: "GRANT_NOT_LIVE" }
    );
    try {
      const ran = await onFakeClock(
        record,
        { ...jobEnvironment(), DISPATCH_URL: airlocked },
        shortBound(5_000, 1_000),
        eachWait,
        proxyUrl(proxy)
      );
      expect({
        code: ran.code,
        stdout: ran.stdout,
        stderr: ran.stderr,
        connects: proxy.connects.length,
      }).toEqual({
        code: 1,
        stdout: `${JSON.stringify({ write_outcome_unknown: "AIRLOCK_TUNNEL_REFUSED" })}\n`,
        stderr: "",
        connects: 2,
      });
    } finally {
      proxy.stop();
    }
  }, 30_000);

  /** The airlock proxy's own refusal inside an open connection, in the proxy's words. */
  const airlockRefusal = (status: number, code: string, words: string) => () =>
    new Response(words, {
      status,
      headers: {
        [AIRLOCK_FORWARDED]: "false",
        [AIRLOCK_CODE]: code,
        "Content-Type": "text/plain; charset=utf-8",
      },
    });

  // Inside a connection that was open, the proxy answers a request it refuses before it dials
  // anything, and says so (`Cipher-Proxy-Forwarded: false`): the answer reached no service and
  // stored nothing. It is final, printed with its code and the proxy's words whole, each piece of
  // the placeholder redacted, and is no write_outcome_unknown.
  test("the airlock's refusal inside an open connection prints its code and words and says nothing was stored", async () => {
    const words =
      "airlock: a request to this host carries the grant's placeholder as its one Authorization header, DPoP <placeholder>; this one does not, so nothing was sent (answer 7f3a)";
    dispatchAnswer = airlockRefusal(403, "NO_PLACEHOLDER", `${words} ${PLACEHOLDER}`);
    const { code, stdout, stderr } = await runJob(record, jobEnvironment());
    expect({ code, stdout, stderr, posts: posts() }).toEqual({
      code: 1,
      stdout: `NO_PLACEHOLDER: ${words} [redacted]; nothing was stored\n`,
      stderr: "",
      posts: 1,
    });
    namesNoToken(record, stdout, stderr);
  });

  /** The airlock proxy's own answer written after the write may have left it for Dispatch
   *  (`Cipher-Proxy-Forwarded: unknown`): a connection that broke after the write was sent, a
   *  grant that ended while Dispatch held the call. */
  const airlockAfterSend = (status: number, code: string, words: string) => () =>
    new Response(words, {
      status,
      headers: {
        [AIRLOCK_FORWARDED]: "unknown",
        [AIRLOCK_CODE]: code,
        "Content-Type": "text/plain; charset=utf-8",
      },
    });

  // The proxy's own answer after the write may have reached Dispatch says so
  // (`Cipher-Proxy-Forwarded: unknown`): Dispatch may have stored it, so the write is asked again
  // under its key, which settles it, and ends write_outcome_unknown when nothing settles it within
  // the bound, never "nothing was stored". The same answer said `false`, written before anything
  // left the proxy, is the control: final at once, nothing stored.
  test("the airlock's own answer after the write may have left is asked again under the key, and ends write_outcome_unknown", async () => {
    const broke = airlockAfterSend(
      502,
      "UPSTREAM_UNREACHABLE",
      "airlock: dispatch.example: stream error: stream ID 1; INTERNAL_ERROR; received from peer"
    );
    const cut = airlockAfterSend(
      407,
      "GRANT_NOT_LIVE",
      "airlock: grant is expired, revoked, or its session ended (answer 7f3a)"
    );
    const rows: readonly (readonly [
      name: string,
      answer: () => Response | Promise<Response>,
      code: number,
      stdout: (out: string) => boolean,
      askedAgain: boolean,
    ])[] = [
      [
        "a 502 after the write was sent, then stored",
        () => (posts() <= 1 ? broke() : storedAnswer()),
        0,
        (out) => JSON.stringify(JSON.parse(out)) === JSON.stringify(stored),
        true,
      ],
      [
        "a 502 after the write was sent, for the whole bound",
        broke,
        1,
        (out) => out === `${JSON.stringify({ write_outcome_unknown: "UPSTREAM_UNREACHABLE" })}\n`,
        true,
      ],
      [
        "a 407 of a grant that ended while Dispatch held the write, then stored",
        () => (posts() <= 1 ? cut() : storedAnswer()),
        0,
        (out) => JSON.stringify(JSON.parse(out)) === JSON.stringify(stored),
        true,
      ],
      [
        "a 407 of a grant that ended while Dispatch held the write, for the whole bound",
        cut,
        1,
        (out) => out === `${JSON.stringify({ write_outcome_unknown: "GRANT_NOT_LIVE" })}\n`,
        true,
      ],
      [
        "the same 502 written before anything left the proxy, the control",
        airlockRefusal(
          502,
          "UPSTREAM_UNREACHABLE",
          "airlock: dispatch.example: dial tcp: connection refused"
        ),
        1,
        (out) =>
          out ===
          "UPSTREAM_UNREACHABLE: airlock: dispatch.example: dial tcp: connection refused; nothing was stored\n",
        false,
      ],
    ];
    const seen: unknown[] = [];
    for (const [name, answer, wanted, printed, again] of rows) {
      written.length = 0;
      dispatchAnswer = answer;
      const ran = await onFakeClock(record, jobEnvironment(), shortBound(5_000, 1_000), eachWait);
      let matched = false;
      try {
        matched = printed(ran.stdout);
      } catch {
        // A row that wants the stored result parses what was printed, which a refusal is not.
      }
      seen.push({
        name,
        code: ran.code === wanted ? true : ran.code,
        printed: matched ? true : ran.stdout,
        askedAgain: posts() > 1 === again ? true : posts(),
      });
      namesNoToken(record, ran.stdout, ran.stderr);
    }
    expect(seen).toEqual(
      rows.map(([name]) => ({ name, code: true, printed: true, askedAgain: true }))
    );
  }, 30_000);

  // A grant the proxy could not read this moment (its 503 GRANT_UNREADABLE) may be readable at the
  // next attempt, so the write asks again under its key, and the result is stored. Turned away so
  // for the whole bound, it ends with the proxy's refusal and no write_outcome_unknown, since no
  // attempt reached a service. Dispatch's own 503, the control, may come after it committed, and
  // ends as an outcome unknown.
  test("the airlock's GRANT_UNREADABLE inside an open connection is asked again within the bound", async () => {
    const unreadable = airlockRefusal(
      503,
      "GRANT_UNREADABLE",
      "airlock: the grant could not be read"
    );
    const rows: readonly (readonly [
      name: string,
      answer: () => Response | Promise<Response>,
      code: number,
      stdout: (out: string) => boolean,
    ])[] = [
      [
        "twice, then stored",
        () => (posts() <= 2 ? unreadable() : storedAnswer()),
        0,
        (out) => JSON.stringify(JSON.parse(out)) === JSON.stringify(stored),
      ],
      [
        "for the whole bound",
        unreadable,
        1,
        (out) =>
          out === "GRANT_UNREADABLE: airlock: the grant could not be read; nothing was stored\n",
      ],
      [
        "Dispatch's own 503, the control",
        () => Response.json({ code: "UNAVAILABLE", error: "internal" }, { status: 503 }),
        1,
        (out) => out === `${JSON.stringify({ write_outcome_unknown: "UNAVAILABLE" })}\n`,
      ],
    ];
    for (const [name, answer, wanted, printed] of rows) {
      written.length = 0;
      dispatchAnswer = answer;
      const ran = await onFakeClock(record, jobEnvironment(), shortBound(5_000, 1_000), eachWait);
      expect(
        {
          code: ran.code,
          stderr: ran.stderr,
          printed: printed(ran.stdout),
          askedAgain: posts() >= 3,
        },
        `${name}: ${ran.stdout}`
      ).toEqual({ code: wanted, stderr: "", printed: true, askedAgain: true });
    }
  }, 30_000);

  // The reserve of the one bound is set from the wait of Dispatch's check of a token on the broker,
  // which the command copies: the copy is held to Go's own constant, so a change of that wait
  // changes the reserve with it.
  test("the reserve's copy of Dispatch's token-check wait is Go's introspectTimeout", () => {
    const go = readFileSync(
      join(import.meta.dir, "..", "..", "..", "envoy", "internal", "ciphertoken", "introspect.go"),
      "utf8"
    );
    const seconds = /^const introspectTimeout = (\d+) \* time\.Second$/m.exec(go)?.[1];
    expect(seconds === undefined ? undefined : Number(seconds) * 1_000).toBe(
      DISPATCH_TOKEN_CHECK_WAIT_MS
    );
  });

  // A job's log is read by its runner, which ends a line at \n and at \r, and reads a workflow
  // command from a line that starts with "::" once its leading white space is trimmed and from a
  // "##[" anywhere in a line. The call prints text it did not write: its arguments, its
  // environment, and the answers of the broker and of Dispatch. Each such value is printed on its
  // line, escaped by the rule of Go's runnerlog.Printable (runner-log.ts), so no value starts a
  // line of its own and no line is a workflow command. A row that shows the value also shows it
  // in the form the rule prints it in: as text in a line of words, as JSON in a JSON answer.
  test("no value the call prints starts a line of its own or is a workflow command", async () => {
    const long = "y".repeat(100_000);
    /** A value, and how it prints in a line of words and in a JSON answer. */
    const values: readonly (readonly [name: string, value: string, text: string, json: string])[] =
      [
        ["a line break", "p\n::error::line", "p\\n::error::line", '"p\\n::error::line"'],
        ["a carriage return", "p\r::warning::cr", "p\\r::warning::cr", '"p\\r::warning::cr"'],
        [
          "control characters",
          "p\u001b[2K\u000b\u0085::error::c",
          "p\\x1b[2K\\v\\u0085::error::c",
          '"p\\u001b[2K\\u000b\\u0085::error::c"',
        ],
        ["an error command", "::error::x", "::error::x", '"::error::x"'],
        ["a command of the second form", "##[group]x", "##\\x5bgroup]x", '"##\\u005bgroup]x"'],
        ["a mask command", "::add-mask::x", "::add-mask::x", '"::add-mask::x"'],
        [
          "a stop of command processing",
          "::stop-commands::t",
          "::stop-commands::t",
          '"::stop-commands::t"',
        ],
        [
          "a very long line",
          `${long}\n::error::long`,
          `${long}\\n::error::long`,
          `"${long}\\n::error::long"`,
        ],
      ];
    /** The arguments of the job's call with `value` after `flag`. */
    const withValue = (flag: string, value: string): string[] => {
      const argv = [...record];
      argv[argv.indexOf(flag) + 1] = value;
      return argv;
    };
    /** The body of the last write Dispatch got, as the stand-in reads it to word its refusal. */
    const sent = (): Record<string, string> =>
      (written.at(-1)?.body ?? {}) as Record<string, string>;
    const refused = (code: string, error: string) =>
      Response.json({ code, error }, { status: code === "COMMIT_NOT_THE_RUNS" ? 403 : 409 });
    /** Each place a value reaches a print: whether the output shows the value, and in which
     *  form, and how one call puts `value` there. */
    const places: readonly (readonly [
      place: string,
      shows: "text" | "json" | "none",
      run: (value: string) => Promise<{ code: number; stdout: string; stderr: string }>,
    ])[] = [
      [
        "the project key",
        "none",
        (value) => runJob(withValue("--project", value), jobEnvironment()),
      ],
      [
        "the check id, in a dry run",
        "json",
        (value) => runJob([...withValue("--check", value), "--dry-run"], jobEnvironment()),
      ],
      [
        "the check id, in Dispatch's refusal",
        "text",
        (value) => {
          dispatchAnswer = () =>
            refused(
              "CHECK_NOT_THIS_JOBS",
              `check ${sent().check} names the job "lint", and this token's job is "go-checks" (check run 7001, "go-checks"): a job writes results only for the checks that name it`
            );
          return runJob(withValue("--check", value), jobEnvironment());
        },
      ],
      [
        "the commit, in Dispatch's refusal",
        "text",
        (value) => {
          dispatchAnswer = () =>
            refused(
              "COMMIT_NOT_THE_RUNS",
              `the result is for commit ${sent().commit}, and this token's run tested commit ${SPEC_COMMIT}: a job writes results only for the commit that it tested`
            );
          return runJob(withValue("--commit", value), jobEnvironment());
        },
      ],
      [
        "the observed text, refused as a flag",
        "text",
        (value) => runJob([...record, "--observed", `--- ${value}`], jobEnvironment()),
      ],
      [
        "the observed file's name, refused as unreadable",
        "text",
        (value) => runJob([...record, "--observed-file", `${stateDir}/${value}`], jobEnvironment()),
      ],
      [
        "Dispatch's error text",
        "text",
        (value) => {
          dispatchAnswer = () => refused("CHECK_NOT_IN_FILES", value);
          return runJob(record, jobEnvironment());
        },
      ],
      [
        "Dispatch's error code",
        "none",
        (value) => {
          dispatchAnswer = () => refused(value, "the result is refused");
          return runJob(record, jobEnvironment());
        },
      ],
      [
        "a body that is not Dispatch's JSON",
        "none",
        (value) => {
          dispatchAnswer = () =>
            new Response(value, { status: 400, headers: { "Content-Type": "text/plain" } });
          return runJob(record, jobEnvironment());
        },
      ],
      [
        "a spec file's name in Dispatch's refusal",
        "text",
        (value) => {
          dispatchAnswer = () =>
            refused(
              "SPEC_FILES_REFUSED",
              `the spec files of example-org/example-repo at ${SPEC_COMMIT} have 1 problem, so they hold no check to write a result for. The first: .dispatch/spec/${value}.md:1: a-lonely-statement: leaf-without-check: no statement is under it and no check serves it`
            );
          return runJob(record, jobEnvironment());
        },
      ],
      [
        "the stored result's observed text",
        "json",
        (value) => {
          const kept = { ...stored.attestation.predicate, observed: value };
          dispatchAnswer = () =>
            storedAnswer({ ...stored, attestation: { ...stored.attestation, predicate: kept } });
          return runJob(record, jobEnvironment());
        },
      ],
      [
        "the airlock proxy's own refusal",
        "text",
        (value) => {
          dispatchAnswer = () =>
            new Response(value, {
              status: 403,
              headers: { [AIRLOCK_FORWARDED]: "false", [AIRLOCK_CODE]: "NO_PLACEHOLDER" },
            });
          return runJob(record, jobEnvironment());
        },
      ],
      [
        "Dispatch's code of a write it may have stored",
        "json",
        (value) => {
          dispatchAnswer = () => Response.json({ code: value, error: "internal" }, { status: 500 });
          return runJob(record, jobEnvironment(), oneAttempt);
        },
      ],
      [
        // `dispatch results --project` runs as a session, so its output may be read in a job's
        // log too: a listed result's leg, in its line and as --json.
        "a listed result's leg, in the listing and as --json",
        "text",
        async (value) => {
          dispatchAnswer = () =>
            Response.json({ results: [storedSpecResult(44, "fail", value, 1)] });
          const session = {
            PATH: process.env.PATH,
            HOME: stateDir,
            DISPATCH_STATE_DIR: stateDir,
            DISPATCH_URL: dispatchUrl,
            DISPATCH_HOST: "omp",
            DISPATCH_SESSION_ID: "ses-results-listing",
            DISPATCH_TOKEN: fixtureSecret("session"),
          };
          const listing = ["results", "--project", "ACME", "--commit", SPEC_COMMIT];
          const text = await runJob(listing, session);
          const json = await runJob([...listing, "--json"], session);
          return {
            code: text.code || json.code,
            stdout: `${text.stdout}${json.stdout}`,
            stderr: `${text.stderr}${json.stderr}`,
          };
        },
      ],
    ];
    const defaults = { dispatchAnswer };
    /** One line per row that failed: its place, its value, and what was wrong with the output. */
    const failed: string[] = [];
    for (const [place, shows, run] of places) {
      for (const [name, value, text, json] of values) {
        ({ dispatchAnswer } = defaults);
        const { code, stdout, stderr } = await run(value);
        const out = `${stdout}${stderr}`;
        // Split wherever a runner or a log viewer can end a line, so no line can hide in another.
        const lines = out.split(/\r\n|[\n\r\u0085\u2028\u2029]/);
        const commands = lines.filter(
          (line) => line.trimStart().startsWith("::") || line.includes("##[")
        );
        // The command's own line ends are its one \n each; any other line break, control
        // character, format character or separator came from a value.
        const unescaped = [...out.matchAll(/[\p{Cc}\p{Cf}\p{Zl}\p{Zp}]/gu)]
          .map(([character]) => character)
          .filter((character) => character !== "\n");
        const shown = shows === "none" || out.includes(shows === "text" ? text : json);
        if (commands.length > 0 || unescaped.length > 0 || !shown) {
          const characters = unescaped.map((character) => character.codePointAt(0)?.toString(16));
          failed.push(
            `${place}, ${name}: exit ${code}, ${commands.length} workflow command lines, ` +
              `unescaped [${characters.join(" ")}], value shown ${shown}, printed ` +
              JSON.stringify(out.slice(0, 160))
          );
        }
      }
    }
    expect(failed).toEqual([]);
  }, 120_000);

  test("--issue and --project are exclusive, refused before any request", async () => {
    for (const env of [jobEnvironment(), { ...jobEnvironment(), ...omp }]) {
      const { code, stdout } = await runJob([...record, "--issue", "DSP-1"], env);
      expect(code, stdout).toBe(2);
      expect(stdout).toContain("exactly one of issue and project is required");
    }
    sentNothing();
  });

  test("an argument of an issue's record is refused with --project", async () => {
    for (const [flag, value, field] of [
      ["--role", "implementer", "role"],
      ["--surface", "api", "surface"],
      ["--run", "go test ./...", "run"],
      ["--negative-control", "The test without its check.", "negative_control"],
      ["--failed", "0", "failed"],
      ["--skipped", "0", "skipped"],
      ["--excluded", "0", "excluded"],
      ["--screenshot", "dispatch://DSP-1/artifact/shot-png", "screenshots"],
      ["--retract", "00000000-0000-4000-8000-000000000041", "retract"],
    ] as const) {
      const { code, stdout } = await runJob([...record, flag, value], jobEnvironment());
      expect(code, `${flag}: ${stdout}`).toBe(2);
      expect(stdout, flag).toContain(`This call also carries ${field}.`);
    }
    sentNothing();
  });

  test("posts the body {commit, check, verdict, observed} and nothing else", async () => {
    // A person's bearer in the job's environment is not what the job writes with.
    const env = { ...jobEnvironment(), DISPATCH_TOKEN: fixtureSecret("person") };
    const observed = "panic: test timed out after 10m0s";
    const withObserved = await runJob(
      [...record.map((word) => (word === "pass" ? "could_not_run" : word)), "--observed", observed],
      env
    );
    expect(withObserved.code, withObserved.stdout).toBe(0);
    const bare = await runJob(record, env);
    expect(bare.code, bare.stdout).toBe(0);

    expect(written.map((seen) => seen.body)).toStrictEqual([
      { commit: SPEC_COMMIT, check: "p-1", verdict: "could_not_run", observed },
      { commit: SPEC_COMMIT, check: "p-1", verdict: "pass" },
    ]);
    expect(written.map((seen) => seen.authorization)).toEqual([
      `DPoP ${PLACEHOLDER}`,
      `DPoP ${PLACEHOLDER}`,
    ]);
    expect(requests).toEqual([]);
  });

  test("sends what a result keeps of observed: its start and its end around the note, with what Dispatch cannot store replaced", async () => {
    // A failing test's log over 1 MiB is ordinary, and Dispatch takes no request that large. A
    // result keeps SPEC_CHECK_OBSERVED_MAX characters, so that is what the command sends: the
    // first SPEC_CHECK_OBSERVED_START of them, the note that says the middle was cut, and the last
    // SPEC_CHECK_OBSERVED_END. A text of the bound goes whole, and a character outside the basic
    // plane, two UTF-16 units, is one character of the count. A NUL, which Dispatch cannot store,
    // and each byte that is not UTF-8 are sent as U+FFFD, so the result is still written.
    const start = SPEC_CHECK_OBSERVED_START;
    const end = SPEC_CHECK_OBSERVED_END;
    const note = SPEC_CHECK_OBSERVED_CUT_NOTE;
    const rows: [name: string, content: string | Buffer, sent: string][] = [
      [
        "a log of 1 MiB",
        `=== RUN   TestP\n${"x".repeat(1024 * 1024)}--- FAIL: TestP\n`,
        `=== RUN   TestP\n${"x".repeat(start - 16)}${note}${"x".repeat(end - 16)}--- FAIL: TestP\n`,
      ],
      [
        "a text of the bound",
        "é".repeat(SPEC_CHECK_OBSERVED_MAX),
        "é".repeat(SPEC_CHECK_OBSERVED_MAX),
      ],
      [
        "a text one past the bound",
        `s${"é".repeat(SPEC_CHECK_OBSERVED_MAX - 1)}e`,
        `s${"é".repeat(start - 1)}${note}${"é".repeat(end - 1)}e`,
      ],
      [
        "a text outside the basic plane",
        "🙂".repeat(2500),
        `${"🙂".repeat(start)}${note}${"🙂".repeat(end)}`,
      ],
      [
        "a NUL and bytes that are not UTF-8",
        Buffer.from([0x61, 0x00, 0x62, 0xff, 0x63]),
        "a\uFFFDb\uFFFDc",
      ],
    ];
    for (const [name, content] of rows) {
      const file = join(stateDir, `observed-${name.replaceAll(" ", "-")}.txt`);
      writeFileSync(file, content);
      const ran = await runJob([...record, "--observed-file", file], jobEnvironment());
      expect(ran.code, `${name}: ${ran.stdout}`).toBe(0);
    }

    const sent = written.map(({ body }) =>
      body !== null &&
      typeof body === "object" &&
      "observed" in body &&
      typeof body.observed === "string"
        ? body.observed
        : ""
    );
    expect(sent).toEqual(rows.map(([, , kept]) => kept));
    for (const kept of sent.slice(0, 4)) expect([...kept].length).toBe(SPEC_CHECK_OBSERVED_MAX);
  });

  test("a failing test's log of 10,000 characters sends its first line and its --- FAIL line, with the note between them", async () => {
    // A test's output names the test in its first line and says why it failed at its end.
    const first = "=== RUN   TestX\n";
    const last = "--- FAIL: TestX (0.01s)\n";
    const log = join(stateDir, "observed-failing.txt");
    writeFileSync(log, `${first}${"x\n".repeat((10_000 - first.length - last.length) / 2)}${last}`);
    const ran = await runJob([...record, "--observed-file", log], jobEnvironment());
    expect(ran.code, ran.stdout).toBe(0);
    const body = written[0]?.body as { observed?: string } | undefined;
    const sent = body?.observed ?? "";
    const note = sent.indexOf(SPEC_CHECK_OBSERVED_CUT_NOTE);
    expect({
      characters: [...sent].length,
      first: sent.startsWith(first),
      last: sent.endsWith(last),
      noteBetween:
        note >= first.length &&
        note + SPEC_CHECK_OBSERVED_CUT_NOTE.length <= sent.length - last.length,
    }).toEqual({ characters: SPEC_CHECK_OBSERVED_MAX, first: true, last: true, noteBetween: true });
  });

  test("dispatch results --project lists each stored result of the commit, one line each, newest first", async () => {
    const listed = await run(["results", "--project", "ACME", "--commit", SPEC_COMMIT], omp);
    expect(listed.code, listed.stdout).toBe(0);
    expect(listed.stdout).toBe(
      [
        `Results of ACME at ${SPEC_COMMIT}, newest first:`,
        `- #42 p-1: fail. Job go-checks, leg linux, run 4242 attempt 2, commit ${SPEC_COMMIT}.`,
        `- #41 p-1: pass. Job go-checks, no leg, run 4242 attempt 1, commit ${SPEC_COMMIT}.`,
        "",
      ].join("\n")
    );

    const rows = await run(
      ["results", "--project", "ACME", "--commit", SPEC_COMMIT, "--check", "p-1", "--json"],
      omp
    );
    expect(rows.code, rows.stdout).toBe(0);
    expect(JSON.parse(rows.stdout)).toEqual(storedSpecResults);
    expect(specResultQueries).toEqual([
      `?commit=${SPEC_COMMIT}`,
      `?commit=${SPEC_COMMIT}&check=p-1`,
    ]);
  });

  // The same job records its result of a check of an issue's Verification document whose
  // ci_check names it, on --issue, under the same `agent-secrets result-grant`: the airlock's
  // placeholder in its environment makes the call the job's, and the body it sends names what the
  // run observed and nothing about who the job is, which Dispatch reads from the results token the
  // proxy puts in the placeholder's place, and from GitHub. The broker mints a placeholder at
  // random, so these rows take theirs from the fixture producer (`fixtureSecret`), fresh each run:
  // the command reads only that the variable is set and not empty, and holds its value as a secret.
  const ISSUE_PLACEHOLDER = fixtureSecret("result");
  /** The job's environment of the rows above, with this section's placeholder. */
  const issueJobEnvironment = (): Record<string, string | undefined> => ({
    ...jobEnvironment(),
    DISPATCH_RESULTS_TOKEN: ISSUE_PLACEHOLDER,
  });
  /** What the job's path prints and a session's never does: its rule, its refusals of the
   *  environment, and the trade's command. */
  const JOB_PATH_WORDS = ["a CI job's result", "DISPATCH_RESULTS_TOKEN", "result-grant"];
  /** Fails unless `ran` is the session path's refusal that holds `refusal`, with none of the job
   *  path's words, and nothing was sent to the job's route, traded or asked of the runner. */
  const sessionRefused = (ran: { code: number; stdout: string }, refusal: string) => {
    expect(ran.code, ran.stdout).toBe(2);
    expect(ran.stdout).toContain(refusal);
    for (const words of JOB_PATH_WORDS) expect(ran.stdout).not.toContain(words);
    expect({ written, trades, runner: runnerAsked }).toEqual({
      written: [],
      trades: [],
      runner: [],
    });
  };
  /** The session path's refusal of a call that names DISPATCH_URL and no session token. */
  const NO_SESSION_TOKEN_REFUSAL = "dispatch config: DISPATCH_URL is set and no token is";
  const issueRecord = [
    "result",
    "--issue",
    "DSP-1",
    "--check",
    "c1",
    "--verdict",
    "pass",
    "--commit",
    SPEC_COMMIT,
  ];
  /** The issue route's answer for the job's result of c1, held to the route's declared answer
   *  (`routeAnswer`). */
  const recordedOnIssue = routeAnswer(
    "POST /api/v1/issues/{key}/results",
    {
      line: `- Check c1 (before_merge): pass at ${SPEC_COMMIT}, recorded by ci. ok`,
      result: {
        id: "00000000-0000-4000-8000-000000000051",
        check_block_id: "c1",
        role: "ci",
        verdict: "pass",
        commit_sha: SPEC_COMMIT,
        branch: "legion/DSP-1",
      },
    },
    201
  );

  test("with DISPATCH_RESULTS_TOKEN, dispatch result --issue is a CI job's call: it posts {check, commit, verdict, observed} to the issue's results route with the placeholder alone", async () => {
    dispatchAnswer = () => storedAnswer(recordedOnIssue);
    // A person's bearer and a session in the job's environment are not what the job writes with.
    const env = { ...issueJobEnvironment(), ...omp, DISPATCH_TOKEN: fixtureSecret("person") };
    const withObserved = await runJob([...issueRecord, "--observed", "ok"], env);
    expect(withObserved.code, withObserved.stdout).toBe(0);
    expect(JSON.parse(withObserved.stdout)).toEqual(recordedOnIssue);
    const bare = await runJob(issueRecord, env);
    expect(bare.code, bare.stdout).toBe(0);

    expect(written.map((seen) => [seen.method, seen.path])).toEqual([
      ["POST", "/api/v1/issues/DSP-1/results"],
      ["POST", "/api/v1/issues/DSP-1/results"],
    ]);
    expect(written.map((seen) => seen.body)).toStrictEqual([
      { check: "c1", commit: SPEC_COMMIT, verdict: "pass", observed: "ok" },
      { check: "c1", commit: SPEC_COMMIT, verdict: "pass" },
    ]);
    expect(written.map((seen) => [seen.authorization, seen.dpop])).toEqual([
      [`DPoP ${ISSUE_PLACEHOLDER}`, null],
      [`DPoP ${ISSUE_PLACEHOLDER}`, null],
    ]);
    // The job trades nothing, and the session's own Dispatch is sent nothing.
    expect({ runner: runnerAsked, trades, session: requests }).toEqual({
      runner: [],
      trades: [],
      session: [],
    });
  });

  // The controls of the row above (Task 5.4 of the plan). Without the placeholder the same call is
  // a session's role record: the job's argument check does not judge it, and the session path
  // refuses it for its own reason, here that the environment names Dispatch and no session token,
  // and sends the job's route nothing. Each row asserts the session path's own refusal, so a fault
  // that reads the call as the job's fails it: the job's path refuses too, for its placeholder.
  test("with AGENT_SECRETS_MACHINE_IDENTITY=github-actions and no DISPATCH_RESULTS_TOKEN, it is not", async () => {
    // The identity variable that named a CI job before the placeholder did: the session path reads
    // it as the machine's own identity, which github-actions is no kind of, and refuses for that.
    const { DISPATCH_RESULTS_TOKEN: _placeholder, ...noPlaceholder } = issueJobEnvironment();
    const ran = await runJob(issueRecord, {
      ...noPlaceholder,
      ...omp,
      AGENT_SECRETS_MACHINE_IDENTITY: "github-actions",
    });
    sessionRefused(
      ran,
      "dispatch config: AGENT_SECRETS_MACHINE_IDENTITY names no identity this machine can prove"
    );
  });

  test("with DISPATCH_RESULTS_TOKEN set and empty, --issue is a session's record and not the job's", async () => {
    // Set and not empty is the job's: an empty variable, as a step writes for an unset one, is no
    // placeholder, in this process and in the command as its own process.
    const env = { ...issueJobEnvironment(), ...omp, DISPATCH_RESULTS_TOKEN: "" };
    sessionRefused(await runJob(issueRecord, env), NO_SESSION_TOKEN_REFUSAL);
    sessionRefused(await spawnJob(issueRecord, env), NO_SESSION_TOKEN_REFUSAL);
  }, 30_000);

  test("without the placeholder, --issue is a session's record and not the job's", async () => {
    const { DISPATCH_RESULTS_TOKEN: _placeholder, ...noPlaceholder } = issueJobEnvironment();
    sessionRefused(
      await runJob(issueRecord, { ...noPlaceholder, ...omp }),
      NO_SESSION_TOKEN_REFUSAL
    );
  });

  test("under the placeholder, an argument of a role's record is refused on --issue, naming its flag, before any request", async () => {
    for (const [flag, value] of [
      ["--role", "tester"],
      ["--surface", "api"],
      ["--run", "go test ./..."],
      ["--negative-control", "The test without its check."],
      ["--failed", "0"],
      ["--skipped", "0"],
      ["--excluded", "0"],
      ["--screenshot", "dispatch://DSP-1/artifact/shot-png"],
      ["--retract", "00000000-0000-4000-8000-000000000041"],
    ] as const) {
      const { code, stdout } = await runJob([...issueRecord, flag, value], issueJobEnvironment());
      expect(code, `${flag}: ${stdout}`).toBe(2);
      expect(stdout, flag).toContain(`This call also carries ${flag}.`);
      // The rule names the placeholder the call is the job's by.
      expect(stdout, flag).toContain(
        "Under agent-secrets result-grant, with the CI job's DISPATCH_RESULTS_TOKEN, the airlock's placeholder of its run, dispatch result --issue records"
      );
    }
    const missing = await runJob(
      issueRecord.filter((word, at) => word !== "--verdict" && issueRecord[at - 1] !== "--verdict"),
      issueJobEnvironment()
    );
    expect(missing.code, missing.stdout).toBe(2);
    expect(missing.stdout).toContain("a CI job's result needs --verdict.");
    sentNothing();
  });

  test("under the placeholder, --issue sends what a result keeps of observed: its start and its end around the note", async () => {
    dispatchAnswer = () => storedAnswer(recordedOnIssue);
    const first = "=== RUN   TestX\n";
    const last = "--- FAIL: TestX (0.01s)\n";
    const log = join(stateDir, "observed-issue-failing.txt");
    writeFileSync(log, `${first}${"x\n".repeat((10_000 - first.length - last.length) / 2)}${last}`);
    const ran = await runJob([...issueRecord, "--observed-file", log], issueJobEnvironment());
    expect(ran.code, ran.stdout).toBe(0);
    const body = written[0]?.body as { observed?: string } | undefined;
    const sent = body?.observed ?? "";
    expect({
      characters: [...sent].length,
      first: sent.startsWith(first),
      last: sent.endsWith(last),
      note: sent.includes(SPEC_CHECK_OBSERVED_CUT_NOTE),
    }).toEqual({ characters: SPEC_CHECK_OBSERVED_MAX, first: true, last: true, note: true });
  });

  // A job's step writes `--issue "$ISSUE" --check "$CHECK" --commit "$SHA"`, and an empty variable
  // is the honest way to send one empty: the issue form refuses it before any write, naming the
  // flag, as the project form does.
  test("under the placeholder, an empty --issue, --check or --commit is refused before any request, naming the flag", async () => {
    for (const [flag, noun] of [
      ["--issue", "issue"],
      ["--check", "check"],
      ["--commit", "commit"],
    ] as const) {
      const emptied = [...issueRecord];
      emptied[emptied.indexOf(flag) + 1] = "";
      const { code, stdout, stderr } = await runJob(emptied, issueJobEnvironment());
      expect({ code, stderr, lines: stdout.split("\n").slice(0, 2) }, flag).toEqual({
        code: 2,
        stderr: "",
        lines: [
          "dispatch result was not called: 1 problem",
          `- ${flag} is empty, so the call names no ${noun}: a variable that gives it may be empty or unset`,
        ],
      });
    }
    sentNothing();
  });

  // A 2xx answer that is not Dispatch's - it names no key back and gives no row of the issue route
  // - came from a server that answers in Dispatch's place, which may have passed the write on
  // first: the job prints write_outcome_unknown and exits 1, as for a write that heard nothing,
  // never landed_without_answer. The control is the first row of the table below: Dispatch's own
  // answer prints as the stored result.
  test("under the placeholder, a 2xx answer that is not Dispatch's prints write_outcome_unknown and exits 1", async () => {
    // Such a server runs no keyed layer, so its page names no key back.
    unkeyed = true;
    dispatchAnswer = () =>
      new Response("Welcome to Bun! To get started, return a Response object.", {
        status: 200,
        headers: { "Content-Type": "text/plain;charset=utf-8" },
      });
    const ran = await runJob(issueRecord, issueJobEnvironment());
    expect(ran).toEqual({
      code: 1,
      stdout: `${JSON.stringify({ write_outcome_unknown: "HTTP_200" })}\n`,
      stderr: "",
    });
    expect(written.map((seen) => [seen.method, seen.path])).toEqual([
      ["POST", "/api/v1/issues/DSP-1/results"],
    ]);
  });

  // The issue form reads its answer as the project form does: an answer that names the write's
  // key back is Dispatch's to this write, so such a 201 prints the stored result, or a landing
  // where its body gives none once the asking under the key is spent (ANSWER_LOST), and such a
  // 409 KEY_COMMITTED prints a landing. An answer that names no key back, or another request's,
  // whatever it holds, is no answer of Dispatch's to it, and leaves the outcome unknown: the
  // result may be stored, and a second run is safe. The rows that name the key back are the
  // controls of the rest.
  for (const [name, answer, echo, code, printed] of [
    ["a 201 with the stored result that names the key back", "result", "this", 0, recordedOnIssue],
    [
      "a 201 with the stored result that names no key back",
      "result",
      "none",
      1,
      { write_outcome_unknown: "HTTP_201" },
    ],
    [
      "a 201 with the stored result that names another request's key back",
      "result",
      "other",
      1,
      { write_outcome_unknown: "HTTP_201" },
    ],
    [
      "a 201 of {} that names the key back",
      "{}",
      "this",
      0,
      { landed_without_answer: "ANSWER_LOST" },
    ],
    ["a 201 of {} that names no key back", "{}", "none", 1, { write_outcome_unknown: "HTTP_201" }],
    [
      "a 409 KEY_COMMITTED that names the key back",
      "committed",
      "this",
      0,
      { landed_without_answer: "KEY_COMMITTED" },
    ],
    [
      "a 409 KEY_COMMITTED that names no key back",
      "committed",
      "none",
      1,
      { write_outcome_unknown: "HTTP_409" },
    ],
    [
      "a 409 KEY_COMMITTED that names another request's key back",
      "committed",
      "other",
      1,
      { write_outcome_unknown: "HTTP_409" },
    ],
  ] as const) {
    const prints =
      "write_outcome_unknown" in printed
        ? "write_outcome_unknown"
        : "landed_without_answer" in printed
          ? "landed_without_answer"
          : "the stored result";
    test(`under the placeholder, --issue answered ${name} prints ${prints} and exits ${code}`, async () => {
      // The stand-in answers behind the keyed layer, which names the write's key back (`this`),
      // as no keyed layer answers (`none`), or naming another request's key itself (`other`).
      const other = echo === "other" ? "another-request-key" : undefined;
      unkeyed = echo === "none";
      dispatchAnswer = () =>
        answer === "committed"
          ? keyCommitted(other)
          : Response.json(answer === "result" ? recordedOnIssue : {}, {
              status: 201,
              headers: other === undefined ? {} : { [IDEMPOTENCY_KEY_HEADER]: other },
            });
      const ran = await onFakeClock(issueRecord, issueJobEnvironment(), undefined, eachWait);
      expect(
        { code: ran.code, stderr: ran.stderr, printed: JSON.parse(ran.stdout) },
        ran.stdout
      ).toEqual({ code, stderr: "", printed });
      const results = written.filter((seen) => seen.path === "/api/v1/issues/DSP-1/results");
      expect({
        posted: results.length >= 1,
        allPosts: results.every((seen) => seen.method === "POST"),
        placeholder: results.every((seen) => seen.authorization === `DPoP ${ISSUE_PLACEHOLDER}`),
      }).toEqual({ posted: true, allPosts: true, placeholder: true });
    }, 30_000);
  }
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
    const underCodex = await run(argv, both, "", undefined, () => "codex");
    expect(underCodex.code).toBe(0);
    const underClaude = await run(argv, both, "", undefined, () => "claude");
    expect(underClaude.code).toBe(0);
    const unknown = await run(argv, both, "", undefined, () => undefined);
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
    expect((await run(argv, both, "", undefined, () => "codex")).code).toBe(0);
    expect((await run(argv, both, "", undefined, () => "omp")).code).toBe(0);
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

  test("a checkout's .opencode/envoy.json is named on every call, and its server is never used", async () => {
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

  // A write that landed before its answer was lost (409 KEY_COMMITTED, naming its key back as the
  // keyed layer does) is done: the call succeeds, and its one line says the write was made and to
  // read it back, since there is nothing to print.
  test("a write answered KEY_COMMITTED exits 0 with one line saying to read it back", async () => {
    const fetchImpl = keyedLayer(
      async (input: RequestInfo | URL, init?: RequestInit) =>
        String(input).endsWith("/api/v1/issues/DSP-1/messages")
          ? Response.json(
              { code: "KEY_COMMITTED", error: "the write landed and its answer was lost" },
              { status: 409 }
            )
          : fetch(input, init),
      { judged: (response) => response.ok || response.status === 409 }
    ) as typeof fetch;
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
    // Behind Dispatch's keyed layer: the create's 201 names the write's key back.
    const creating = (refuse: boolean) =>
      keyedLayer(async (input: RequestInfo | URL, init?: RequestInit) => {
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

    const refused = await run(create, omp, "", creating(true));
    expect(refused.code).toBe(1);
    expect(refused.stdout).toContain(
      'Not created: "Nightly export retries for ever" is a near copy of an open issue.'
    );
    expect(refused.stdout).toContain("DSP-9 [in_progress] Nightly export retries for ever");

    const made = await run(create, omp, "", creating(false));
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

  // A refusal the CLI prints before it reaches Dispatch is logged too, whenever the call knows its
  // session: a reader of the ledger (pi-envoy's run-end check, the skill-scenario rig) sees a call
  // whose output a pipe, `$(…)` or another program swallowed. The host and the command need not be
  // known for the session to be: a Python `subprocess.run(["dispatch", …])` inherits the session id
  // without `DISPATCH_HOST`.
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

  test("a host the command does not accept is refused by name, with the three it accepts", async () => {
    const { code, stdout } = await run(["projects"], {
      DISPATCH_HOST: "opencode",
      DISPATCH_SESSION_ID: "ses-1",
    });
    expect(code).toBe(2);
    expect(stdout).toBe('dispatch: DISPATCH_HOST is "opencode", not omp, claude or codex.\n');
    expect(requests).toEqual([]);
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
