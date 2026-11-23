import { afterEach, describe, expect, test } from "bun:test";
import { mkdirSync, readFileSync, symlinkSync, writeFileSync } from "node:fs";
import { readFile } from "node:fs/promises";
import { dirname, join } from "node:path";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import type { GraphEdge, GraphNode } from "../../../packages/contracts/src/dispatch-api";
import type { CensusReport } from "../check-runs-census.ts";
// A namespace read, so that a build lacking one export fails the rows that read it and no others.
import * as censusModule from "../check-runs-census.ts";

const script = join(import.meta.dir, "..", "check-runs-census.ts");
const fixtures = join(import.meta.dir, "..", "__fixtures__", "check-runs-census");
const makeDirectory = temporaryDirectories("check-runs-census");
const servers: Array<{ stop: (force?: boolean) => void }> = [];

afterEach(() => {
  for (const server of servers.splice(0)) server.stop(true);
});

/** Each row spawns the census, and several plant a checkout: git and a Bun process on a loaded
 *  box take longer than `bun:test`'s own five seconds. */
const SLOW = 120_000;

function fixture(name: string): string {
  return readFileSync(join(fixtures, name), "utf8");
}

/** One document of a stand-in issue: what its documents list names and what its text read answers. */
interface StubDocument {
  readonly slug: string;
  readonly role: string;
  readonly markdown?: string;
}

interface StubIssue {
  readonly status: string;
  readonly documents: StubDocument[];
}

/** The reference the Verification document names as the source of the first list. */
const REFERENCED_BY = "dispatch://INFRA-1196/comment/c0b271c7-fd09-485f-8787-862d45795160";

const CLEAN = fixture("clean-verification.md");

/** How many `todo` issues the stand-in lists: more than one page of the listing's largest page
 *  (250), so a whole run must read a second page. Rows 55 and 300 hold a planted p1. */
const TODO_ISSUES = 300;

/**
 * Every issue of the stand-in project, and the documents each holds. The named issues are the
 * Verification document's own controls; `INFRA-9001` to `INFRA-9300` are the `todo` listing.
 * `unreadable` adds the issue whose named Verification document does not load.
 */
function textsIssues(options: { unreadable?: boolean } = {}): Record<string, StubIssue> {
  const verification = (markdown: string): StubDocument[] => [
    { slug: "spec", role: "" },
    { slug: "verification-md", role: "verification", markdown },
  ];
  const issues: Record<string, StubIssue> = {
    "INFRA-3903": { status: "in_progress", documents: verification(CLEAN) },
    "INFRA-1588": {
      status: "in_progress",
      documents: verification(fixture("owner-rule-own-check.md")),
    },
    "INFRA-3937": {
      status: "in_progress",
      documents: verification(fixture("old-rule-item-2.md")),
    },
    "INFRA-3940": {
      status: "in_progress",
      documents: verification(fixture("old-rule-item-4.md")),
    },
    "INFRA-1152": {
      status: "in_progress",
      documents: [
        ...verification(CLEAN),
        { slug: "plan-md", role: "plan", markdown: fixture("plan-p3.md") },
      ],
    },
    // Its documents list names only its spec, so it counts as read with no checks.
    "INFRA-4538": { status: "in_progress", documents: [{ slug: "spec", role: "" }] },
    "INFRA-3945": {
      status: "done",
      documents: verification(fixture("p1-bold-line-break.md")),
    },
    "INFRA-3939": {
      status: "retro",
      documents: verification(fixture("p1-bold-line-break.md")),
    },
  };
  if (options.unreadable === true) {
    issues["INFRA-1276"] = {
      status: "in_progress",
      documents: [
        { slug: "spec", role: "" },
        { slug: "verification-md", role: "verification" },
      ],
    };
  }
  for (let index = 1; index <= TODO_ISSUES; index++) {
    const planted = index === 55 || index === TODO_ISSUES;
    issues[`INFRA-${9000 + index}`] = {
      status: "todo",
      documents: verification(planted ? fixture("p1-bold-line-break.md") : CLEAN),
    };
  }
  return issues;
}

/** A graph node as the references route answers it, with the facts every node carries. */
function node(named: Pick<GraphNode, "kind" | "id"> & Partial<GraphNode>): GraphNode {
  return {
    made_by: [],
    made_at: null,
    changed_by: [],
    changed_at: null,
    parent: null,
    parent_version: null,
    ...named,
  };
}

function edge(target: GraphNode): GraphEdge {
  return {
    kind: "mentions",
    created_at: "2026-11-20T10:00:00.000Z",
    direction: "in",
    node: target,
    source_seq: null,
  };
}

function artifactEdge(issue: string, slug: string): GraphEdge {
  return edge(
    node({
      kind: "artifact",
      id: `artifact-${issue}-${slug}`,
      issue_key: issue,
      ref: `dispatch://${issue}/artifact/${slug}`,
    })
  );
}

/** The documents the first list names, as `GET /api/v1/references` answers them: an artifact node
 *  for each, and one block node, whose row is its document. */
function referencedEdges(): GraphEdge[] {
  const issues = [
    "INFRA-3903",
    "INFRA-1588",
    "INFRA-3937",
    "INFRA-3940",
    "INFRA-3945",
    "INFRA-3939",
  ];
  return [
    ...issues.map((issue) => artifactEdge(issue, "verification-md")),
    edge(
      node({
        kind: "block",
        id: "dispatch://INFRA-1152/artifact/verification-md#b-one",
        block_id: "b-one",
        issue_key: "INFRA-1152",
        document: "dispatch://INFRA-1152/artifact/verification-md",
      })
    ),
  ];
}

/** What a text control changes in the stand-in's answers. */
interface TextsStub {
  /** The references the first list answers, in place of `referencedEdges()`. */
  readonly edges?: GraphEdge[];
  /** The `todo` listing answers page one's first 50 keys again for its second page. */
  readonly repeatSecondPage?: boolean;
  /** The `todo` listing answers 500 for its second page. */
  readonly failSecondPage?: boolean;
}

/** A stand-in Dispatch for the text census: the first list's references, the project's issues by
 *  status with their totals, each issue's documents list, and each document's text. */
function stubTexts(issues: Record<string, StubIssue>, options: TextsStub = {}): string {
  const server = Bun.serve({
    hostname: "127.0.0.1",
    port: 0,
    fetch(request) {
      const url = new URL(request.url);
      const path = url.pathname;
      if (path === "/api/v1/references") {
        if (url.searchParams.get("to") !== REFERENCED_BY) return Response.json({ edges: [] });
        return Response.json({
          node: { kind: "comment", id: "c0b271c7" },
          edges: options.edges ?? referencedEdges(),
        });
      }
      if (path === "/api/v1/issues") {
        const status = url.searchParams.get("status");
        const keys = Object.keys(issues)
          .filter((key) => status === null || issues[key]?.status === status)
          .sort();
        const limit = Number(url.searchParams.get("limit") ?? "0");
        const offset = Number(url.searchParams.get("offset") ?? "0");
        if (limit === 0) return Response.json(keys.map((key) => ({ key })));
        if (status === "todo" && offset > 0 && options.failSecondPage === true) {
          return new Response("listing unavailable", { status: 500 });
        }
        const page =
          status === "todo" && offset > 0 && options.repeatSecondPage === true
            ? keys.slice(0, 50)
            : keys.slice(offset, offset + limit);
        return Response.json({
          issues: page.map((key) => ({ key })),
          total: keys.length,
          limit,
          offset,
        });
      }
      const read = path.match(/^\/api\/v1\/issues\/([^/]+)$/);
      if (read) {
        const issue = issues[read[1] ?? ""];
        if (issue === undefined) return new Response("no issue", { status: 404 });
        return Response.json({
          key: read[1],
          status: issue.status,
          artifacts: issue.documents.map((document) => ({
            id: `artifact-${read[1]}-${document.slug}`,
            slug: document.slug,
            role: document.role,
            name: document.slug,
          })),
        });
      }
      const text = path.match(/^\/api\/v1\/issues\/([^/]+)\/artifacts\/([^/]+)\/text$/);
      if (text) {
        const document = issues[text[1] ?? ""]?.documents.find((row) => row.slug === text[2]);
        if (document?.markdown === undefined) {
          return Response.json({ code: "NOT_FOUND", error: "no such version" }, { status: 404 });
        }
        return Response.json({ markdown: document.markdown, token: "sha256:fixture", version: 3 });
      }
      return new Response("not found", { status: 404 });
    },
  });
  servers.push(server);
  return `http://127.0.0.1:${server.port}`;
}

async function runCensus(
  args: readonly string[],
  environment: Record<string, string>
): Promise<{ stdout: string; stderr: string; exitCode: number }> {
  // DISPATCH_TOKEN_FILE outranks DISPATCH_TOKEN, so a pane's own token file would reach the stub.
  const { DISPATCH_TOKEN_FILE: _paneTokenFile, ...inherited } = process.env;
  const census = Bun.spawn(["bun", script, ...args], {
    env: { ...inherited, ...environment },
    stdout: "pipe",
    stderr: "pipe",
  });
  const [stdout, stderr, exitCode] = await Promise.all([
    new Response(census.stdout).text(),
    new Response(census.stderr).text(),
    census.exited,
  ]);
  return { stdout, stderr, exitCode };
}

async function census(
  args: readonly string[],
  url: string
): Promise<{ report: CensusReport; exitCode: number; stderr: string }> {
  const run = await runCensus([...args, "--json"], {
    DISPATCH_URL: url,
    DISPATCH_TOKEN: "token",
  });
  if (!run.stdout.startsWith("{")) {
    throw new Error(`census printed no report: ${run.stdout}\n${run.stderr}`);
  }
  return {
    report: JSON.parse(run.stdout) as CensusReport,
    exitCode: run.exitCode,
    stderr: run.stderr,
  };
}

/** Each listed row as `<issue> <document> <rules>`, which is what every control names. */
function listed(report: CensusReport): string[] {
  return report.rows.map(
    (row) => `${row.issue} ${row.document ?? ""} ${(row.rules ?? []).join("+")}`
  );
}

const TEXTS = ["texts", "--project", "INFRA", "--referenced-by", REFERENCED_BY];

describe("the text census", () => {
  test(
    "it lists each old sentence of the two sources and stays quiet on an owner's own rule",
    async () => {
      const { report, exitCode } = await census(TEXTS, stubTexts(textsIssues()));

      expect(report.unknown).toEqual([]);
      expect(report.complete).toBe(true);
      // Row 55 is on the first page; row 300 is on the second page of 250.
      expect(listed(report)).toContain("INFRA-9055 verification-md p1");
      expect(listed(report)).toContain("INFRA-9300 verification-md p1");
      // The first list's documents: item 2 by the CI-recording pattern, item 4 by the old-rule one.
      expect(listed(report)).toContain("INFRA-3937 verification-md old-rule+ci-recording");
      expect(listed(report)).toContain("INFRA-3940 verification-md old-rule");
      // The plan of an issue of the first list is read beside its Verification document.
      expect(listed(report)).toContain("INFRA-1152 plan-md p3");
      // An owner's narrower rule for its own check, and the two texts that hold the new words.
      expect(listed(report).join("\n")).not.toContain("INFRA-1588");
      expect(listed(report).join("\n")).not.toContain("INFRA-3903");
      expect(listed(report).join("\n")).not.toContain("INFRA-4538");
      // Every issue whose documents list the census read, the two whose texts it then left for
      // being done or in `retro` among them.
      expect(report.read?.issues).toBe(TODO_ISSUES + 8);
      expect(report.read?.texts).toBe(TODO_ISSUES + 6);
      expect(report.notRead?.map((row) => `${row.issue} ${row.reason}`)).toEqual([
        "INFRA-3939 retro",
        "INFRA-3945 done",
      ]);
      expect(exitCode).toBe(1);

      const row = report.rows.find((listing) => listing.issue === "INFRA-9055");
      expect(row?.version).toBe(3);
      expect(row?.token).toBe("sha256:fixture");
      expect(row?.match).toBe("The tester repeats no check");
      expect(row?.paragraph).toContain(
        "The implementer records each `before_merge` check with no `ci_check` at the head it hands off."
      );
    },
    SLOW
  );

  test(
    "a run whose patterns match nothing misses the two planted items, and reports the break",
    async () => {
      const { report, exitCode } = await census(
        [...TEXTS, "--break-patterns"],
        stubTexts(textsIssues())
      );
      expect(listed(report)).toContain("INFRA-9055 verification-md p1");
      expect(listed(report).join("\n")).not.toContain("INFRA-3937");
      expect(listed(report).join("\n")).not.toContain("INFRA-3940");
      expect(exitCode).toBe(1);
    },
    SLOW
  );

  test(
    "a second document the first list names on one issue is read beside the first",
    async () => {
      const issues = textsIssues();
      issues["INFRA-3903"]?.documents.push({
        slug: "extra-md",
        role: "",
        markdown: fixture("p1-bold-line-break.md"),
      });
      const edges = [...referencedEdges(), artifactEdge("INFRA-3903", "extra-md")];
      const { report, exitCode } = await census(TEXTS, stubTexts(issues, { edges }));
      expect(listed(report)).toContain("INFRA-3903 extra-md p1");
      expect(report.unknown).toEqual([]);
      expect(exitCode).toBe(1);
    },
    SLOW
  );

  test(
    "a first list whose edges name no document reports UNKNOWN",
    async () => {
      const comment = edge(node({ kind: "comment", id: "comment-1", issue_key: "INFRA-3903" }));
      const { report, exitCode } = await census(
        TEXTS,
        stubTexts(textsIssues(), { edges: [comment] })
      );
      expect(report.unknown.map((row) => row.reason).join("\n")).toContain("named no document");
      expect(exitCode).toBe(3);
    },
    SLOW
  );

  test(
    "a text that does not load reports UNKNOWN",
    async () => {
      const { report, exitCode } = await census(
        TEXTS,
        stubTexts(textsIssues({ unreadable: true }))
      );
      expect(report.unknown.map((row) => `${row.issue} ${row.what}`)).toContain(
        "INFRA-1276 verification-md"
      );
      expect(report.complete).toBe(false);
      expect(exitCode).toBe(3);
    },
    SLOW
  );

  test(
    "one page of 50 for each status reports UNKNOWN, because its count is below the total",
    async () => {
      const { report, exitCode } = await census(
        [...TEXTS, "--limit", "50"],
        stubTexts(textsIssues())
      );
      expect(report.unknown.map((row) => row.reason).join("\n")).toContain(`50 of ${TODO_ISSUES}`);
      expect(exitCode).toBe(3);
    },
    SLOW
  );

  test(
    "a second page that repeats issues of the first reports UNKNOWN",
    async () => {
      const { report, exitCode } = await census(
        TEXTS,
        stubTexts(textsIssues(), { repeatSecondPage: true })
      );
      const reasons = report.unknown.map((row) => row.reason).join("\n");
      expect(reasons).toContain(`250 of ${TODO_ISSUES}`);
      expect(reasons).toContain("repeated");
      expect(listed(report).join("\n")).not.toContain("INFRA-9300");
      expect(exitCode).toBe(3);
    },
    SLOW
  );

  test(
    "a page that does not load reports UNKNOWN, and keeps what the run found",
    async () => {
      const { report, exitCode } = await census(
        TEXTS,
        stubTexts(textsIssues(), { failSecondPage: true })
      );
      expect(report.unknown.map((row) => `${row.what} ${row.reason}`).join("\n")).toContain("todo");
      expect(listed(report)).toContain("INFRA-3937 verification-md old-rule+ci-recording");
      expect(exitCode).toBe(3);
    },
    SLOW
  );

  test(
    "a listed issue is read whether it is done or not, and a run with nothing to list exits 0",
    async () => {
      const url = stubTexts(textsIssues());
      const keys = join(makeDirectory(), "keys.txt");
      writeFileSync(keys, "INFRA-3945\n");
      const done = await census(["texts", "--project", "INFRA", "--keys", keys], url);
      expect(listed(done.report)).toEqual(["INFRA-3945 verification-md p1"]);
      expect(done.exitCode).toBe(1);

      writeFileSync(keys, "INFRA-3903\n");
      const quiet = await census(["texts", "--project", "INFRA", "--keys", keys], url);
      expect(quiet.report.rows).toEqual([]);
      expect(quiet.report.unknown).toEqual([]);
      expect(quiet.exitCode).toBe(0);
    },
    SLOW
  );
});

describe("the owner's narrower rule", () => {
  const ownChecks = new Set(["c13"]);
  const rulesOf = (sentence: string): string[] =>
    censusModule
      .matchesIn(sentence, ownChecks, censusModule.DEFAULT_PATTERNS)
      .flatMap((found) => found.rules);

  test("a rule on every check is listed", () => {
    expect(rulesOf("The tester does not run any before_merge check again.")).toEqual(["old-rule"]);
  });

  test("a rule restricted to the document's own check stays quiet", () => {
    expect(rulesOf("The tester does not run c13 again: the implementer owns it.")).toEqual([]);
  });

  test("a rule on every check that names an own check as an example is listed", () => {
    expect(rulesOf("The tester does not run any before_merge check again, including c13.")).toEqual(
      ["old-rule"]
    );
  });
});

/** The planted round, as `round.json` holds it, with the changes each control makes. */
interface RoundScene {
  readonly issue: string;
  readonly since: string;
  readonly status: string;
  readonly events: readonly Record<string, unknown>[];
  readonly handoffs: readonly Record<string, unknown>[];
  readonly checks: Record<string, unknown>[];
  readonly outputs: Record<string, string>;
  /** The issue's plan, where the round has one. */
  readonly plan?: string;
}

function roundScene(): RoundScene {
  return JSON.parse(fixture("round.json")) as RoundScene;
}

/** One result of the round's session on `c3` at `H1`, as the controls change it. */
function c3Result(options: {
  session?: string;
  output?: string;
  observed?: string;
}): Record<string, unknown> {
  const session = options.session ?? "ses-tester";
  return {
    id: "c3-round",
    session,
    actor: { kind: "session", id: session, proven: true },
    role: "tester",
    verdict: "pass",
    commit_sha: "H1",
    created_at: "2026-11-24T10:14:00Z",
    run: `bun test fixtures/c3.test.ts (output {stub}/outputs/${options.output ?? "c3-H1.txt"})`,
    observed: options.observed ?? "1 pass.",
  };
}

function withResult(scene: RoundScene, check: string, result: Record<string, unknown>): RoundScene {
  const checks = scene.checks.map((block) =>
    block.block_id === check
      ? { ...block, results: [...(block.results as unknown[]), result] }
      : block
  );
  return { ...scene, checks };
}

function withC3(scene: RoundScene, result: Record<string, unknown>): RoundScene {
  return withResult(scene, "c3", result);
}

/** One result of a round's session on a check, for the second round's reads. */
function passAt(check: string, head: string, at: string): Record<string, unknown> {
  return {
    id: `${check}-${head}`,
    session: "ses-tester",
    actor: { kind: "session", id: "ses-tester", proven: true },
    role: "tester",
    verdict: "pass",
    commit_sha: head,
    created_at: at,
    run: `bun test fixtures/${check}.test.ts (output {stub}/outputs/${check}-${head}.txt)`,
    observed: "1 pass.",
  };
}

/**
 * A second round of the same issue: another move into testing, a second `test` handoff at `H2`,
 * and the runs of that round. The pass of `c3` at `H1` falls inside the first round's window, so
 * the first round holds a run of every check. The second round's `c3` pass is at its own head
 * but was recorded before the move that started it, so that round holds none.
 */
function withSecondRound(scene: RoundScene): RoundScene {
  const checks = scene.checks.map((check) => {
    const id = check.block_id;
    if (id !== "c1" && id !== "c2" && id !== "c3") return check;
    const added =
      id === "c3"
        ? [c3Result({}), passAt("c3", "H2", "2026-11-24T11:30:00Z")]
        : [passAt(String(id), "H2", id === "c1" ? "2026-11-24T12:10:00Z" : "2026-11-24T12:12:00Z")];
    return { ...check, results: [...(check.results as unknown[]), ...added] };
  });
  return {
    ...scene,
    events: [
      ...scene.events,
      {
        id: 5,
        seq: 5,
        type: "issue.updated",
        created_at: "2026-11-24T12:00:00Z",
        payload: { status: "testing", changed: ["status"] },
      },
    ],
    handoffs: [
      ...scene.handoffs,
      {
        id: "handoff-test-2",
        phase: "test",
        version: 2,
        author: { kind: "session", id: "ses-tester" },
        created_at: "2026-11-24T12:50:00Z",
        body: '{"head":"H2","note":"Walked the page at H2."}',
      },
    ],
    checks,
    outputs: {
      ...scene.outputs,
      "c1-H2.txt": "$ bun test fixtures/c1.test.ts\nat H2\n 1 pass\n",
      "c2-H2.txt": "$ bun test fixtures/c2.test.ts\nat H2\n 1 pass\n",
    },
  };
}

function stubRounds(
  scene: RoundScene,
  options: { loseOutput?: boolean; failListing?: boolean } = {}
): string {
  const server = Bun.serve({
    hostname: "127.0.0.1",
    port: 0,
    fetch(request) {
      const url = new URL(request.url);
      const path = url.pathname;
      const origin = `http://127.0.0.1:${server.port}`;
      const fill = (value: string): string => value.replaceAll("{stub}", origin);
      if (path === "/api/v1/issues") {
        if (options.failListing === true) {
          return new Response("listing unavailable", { status: 500 });
        }
        return Response.json([{ key: scene.issue }]);
      }
      if (path === `/api/v1/issues/${scene.issue}`) {
        return Response.json({
          key: scene.issue,
          status: scene.status,
          artifacts:
            scene.plan === undefined
              ? []
              : [{ id: "artifact-plan", slug: "plan-md", role: "plan", name: "plan-md" }],
        });
      }
      const planText = path.match(/^\/api\/v1\/issues\/[^/]+\/artifacts\/plan-md\/text$/);
      if (planText && scene.plan !== undefined) {
        return Response.json({ markdown: scene.plan, token: "sha256:fixture", version: 2 });
      }
      if (path === `/api/v1/issues/${scene.issue}/events`) {
        const after = Number(url.searchParams.get("after") ?? "0");
        return Response.json(scene.events.filter((event) => Number(event.seq) > after));
      }
      if (path === `/api/v1/issues/${scene.issue}/handoffs`) {
        return Response.json(
          [...scene.handoffs].reverse().map(({ body: _body, ...summary }) => summary)
        );
      }
      const handoff = path.match(/^\/api\/v1\/handoffs\/(.+)$/);
      if (handoff) {
        const found = scene.handoffs.find((row) => row.id === handoff[1]);
        if (found === undefined) return new Response("not found", { status: 404 });
        return Response.json({ issue_key: scene.issue, ...found });
      }
      if (path === `/api/v1/issues/${scene.issue}/results`) {
        return Response.json({
          issue: scene.issue,
          verification_artifact_id: "artifact-verification",
          checks: scene.checks.map((check) => ({
            ...check,
            results: (check.results as Record<string, string>[]).map((result) => ({
              ...result,
              run: fill(result.run ?? ""),
              observed: fill(result.observed ?? ""),
            })),
          })),
        });
      }
      const output = path.match(/^\/outputs\/(.+)$/);
      if (output) {
        const body = scene.outputs[output[1] ?? ""];
        if (body === undefined || options.loseOutput === true) {
          return new Response("gone", { status: 404 });
        }
        return new Response(body, { headers: { "content-type": "text/plain" } });
      }
      return new Response("not found", { status: 404 });
    },
  });
  servers.push(server);
  return `http://127.0.0.1:${server.port}`;
}

function gitIn(directory: string, ...args: string[]): string {
  const run = Bun.spawnSync(["git", "-C", directory, ...args], { stdout: "pipe", stderr: "pipe" });
  if (run.exitCode !== 0) {
    throw new Error(`git ${args.join(" ")}: ${run.stderr.toString()}`);
  }
  return run.stdout.toString().trim();
}

function writeFiles(directory: string, files: Record<string, string>): void {
  for (const [path, body] of Object.entries(files)) {
    mkdirSync(join(directory, dirname(path)), { recursive: true });
    writeFileSync(join(directory, path), body);
  }
}

/** A planted checkout whose one commit `H1` holds `files`. */
function plantRepository(files: Record<string, string>): string {
  const directory = makeDirectory();
  writeFiles(directory, files);
  gitIn(directory, "init", "-q", "-b", "main");
  gitIn(directory, "config", "user.email", "census@example.invalid");
  gitIn(directory, "config", "user.name", "Census");
  gitIn(directory, "config", "commit.gpgsign", "false");
  gitIn(directory, "add", "-A");
  gitIn(directory, "commit", "-q", "-m", "plant");
  // Branches, not tags: the box signs an annotated tag, which needs a message of its own. Both
  // planted heads name the one commit, so each round's workflows are read at a commit it holds.
  gitIn(directory, "branch", "H1");
  gitIn(directory, "branch", "H2");
  return directory;
}

/** The ordinary pull request's job that proves `c5`, the check carrying `ci_check: test`. */
const CI_WORKFLOW = `name: ci
on:
  pull_request:
jobs:
  test:
    if: \${{ !github.event.pull_request.draft }}
    runs-on: ubuntu-24.04
    steps:
      - run: bun test fixtures/c5.test.ts
`;

const ROUND_REPOSITORY = { ".github/workflows/ci.yaml": CI_WORKFLOW };

/** A workflow of an ordinary pull request whose one job holds `body` under its `steps:`. */
function pullRequestJob(job: string, body: string): string {
  return `name: ${job}\non:\n  pull_request:\njobs:\n  ${job}:\n    runs-on: ubuntu-24.04\n${body}`;
}

async function rounds(
  scene: RoundScene,
  options: { repository?: Record<string, string>; loseOutput?: boolean } = {}
): Promise<{ report: CensusReport; exitCode: number; keys: string }> {
  const url = stubRounds(scene, { ...(options.loseOutput === true ? { loseOutput: true } : {}) });
  const repository = plantRepository({ ...ROUND_REPOSITORY, ...options.repository });
  const out = makeDirectory();
  const run = await census(
    ["rounds", "--project", "ACME", "--since", scene.since, "--repo", repository, "--out", out],
    url
  );
  return { ...run, keys: await readFile(join(out, "keys.txt"), "utf8") };
}

/** Each listed round row as `<check> <reason>`. */
function roundRows(report: CensusReport): string[] {
  return report.rows.map((row) => `${row.check ?? ""} ${row.reason ?? ""}`);
}

describe("the rounds census", () => {
  test(
    "it lists the check with no result of the round's session at the head, and nothing else",
    async () => {
      const { report, exitCode, keys } = await rounds(roundScene());
      expect(report.unknown).toEqual([]);
      expect(report.rows.map((row) => row.check)).toEqual(["c3"]);
      expect(roundRows(report)[0]).toContain("no result of the round's session at the head");
      expect(report.rows[0]?.words).toContain("The third row of the page.");
      expect(report.rows[0]?.head).toBe("H1");
      expect(report.rows[0]?.session).toBe("ses-tester");
      // A check whose words give its run to a person and one that is not `before_merge` are out
      // of the round; the one carrying `ci_check` is CI's, by the job its attribute names.
      expect(report.excluded?.map((row) => `${row.check} ${row.reason}`)).toEqual([
        "c4 its words give its run only to another runner",
        "c6 its when is after_merge",
      ]);
      expect(report.ci?.map((row) => `${row.check} ${row.job}`)).toEqual([
        "c5 .github/workflows/ci.yaml test",
      ]);
      // A result whose actor does not carry `proven` is named.
      expect(report.unproven).toEqual(["c2-tester"]);
      expect(keys).toBe("ACME-1\n");
      expect(exitCode).toBe(1);
    },
    SLOW
  );

  test(
    "an updated issue with no move into testing is no round, and the run reports UNKNOWN",
    async () => {
      const { report, exitCode, keys } = await rounds({ ...roundScene(), events: [] });
      expect(report.unknown.map((row) => row.reason).join("\n")).toContain("moved into testing");
      expect(report.complete).toBe(false);
      expect(keys).toBe("");
      expect(exitCode).toBe(3);
    },
    SLOW
  );

  test(
    "a listing that does not load reports UNKNOWN in a report",
    async () => {
      const url = stubRounds(roundScene(), { failListing: true });
      const out = makeDirectory();
      const { report, exitCode } = await census(
        [
          "rounds",
          "--project",
          "ACME",
          "--since",
          roundScene().since,
          "--repo",
          plantRepository(ROUND_REPOSITORY),
          "--out",
          out,
        ],
        url
      );
      expect(report.unknown.map((row) => row.reason).join("\n")).toContain("500");
      expect(exitCode).toBe(3);
    },
    SLOW
  );

  test(
    "a pass of the round's session at the head, with its output there, is a run",
    async () => {
      const { report, exitCode } = await rounds(withC3(roundScene(), c3Result({})));
      expect(report.rows).toEqual([]);
      expect(report.unknown).toEqual([]);
      expect(exitCode).toBe(0);
    },
    SLOW
  );

  test(
    "a second round is read on its own head, and the first round's run counts for the first",
    async () => {
      const { report, exitCode } = await rounds(withSecondRound(roundScene()));
      expect(report.unknown).toEqual([]);
      expect(report.rows.map((row) => `${row.round} ${row.check} ${row.head}`)).toEqual([
        "2 c3 H2",
      ]);
      expect(exitCode).toBe(1);
    },
    SLOW
  );

  test(
    "the same pass from another session is listed",
    async () => {
      const { report, exitCode } = await rounds(
        withC3(roundScene(), c3Result({ session: "ses-other" }))
      );
      expect(report.rows.map((row) => row.check)).toEqual(["c3"]);
      expect(roundRows(report)[0]).toContain("no result of the round's session at the head");
      expect(exitCode).toBe(1);
    },
    SLOW
  );

  test(
    "a pass whose output is of another head is listed",
    async () => {
      const { report, exitCode } = await rounds(
        withC3(roundScene(), c3Result({ output: "c3-H2.txt" }))
      );
      expect(roundRows(report)[0]).toContain("its output shows no run at the head");
      expect(exitCode).toBe(1);
    },
    SLOW
  );

  test(
    "a pass whose output at the head is another check's reports UNKNOWN",
    async () => {
      const { report, exitCode } = await rounds(
        withC3(roundScene(), c3Result({ output: "c1-H1.txt" }))
      );
      expect(report.unknown.map((row) => `${row.what} ${row.reason}`).join("\n")).toContain(
        "c3 at H1 its output does not show the check's procedure"
      );
      expect(report.rows).toEqual([]);
      expect(exitCode).toBe(3);
    },
    SLOW
  );

  test(
    "a pass whose reading says it was carried is listed",
    async () => {
      const { report, exitCode } = await rounds(
        withC3(roundScene(), c3Result({ observed: "1 pass, carried from H2." }))
      );
      expect(roundRows(report)[0]).toContain("its reading carries an earlier run");
      expect(exitCode).toBe(1);
    },
    SLOW
  );

  test(
    "a pass whose output does not load reports UNKNOWN",
    async () => {
      const { report, exitCode } = await rounds(withC3(roundScene(), c3Result({})), {
        loseOutput: true,
      });
      expect(report.unknown.map((row) => `${row.what} ${row.reason}`).join("\n")).toContain("c3");
      expect(report.rows).toEqual([]);
      expect(exitCode).toBe(3);
    },
    SLOW
  );

  test(
    "a check a workflow step records is left out, named with the step",
    async () => {
      const { report, exitCode } = await rounds(roundScene(), {
        repository: {
          ".github/workflows/record-rows.yaml": fixture("workflow-records-c3.yaml"),
        },
      });
      const excluded = report.excluded?.find((row) => row.check === "c3");
      expect(excluded?.reason).toBe("a workflow step records it");
      expect(excluded?.detail).toContain(".github/workflows/record-rows.yaml");
      expect(report.rows).toEqual([]);
      expect(exitCode).toBe(0);
    },
    SLOW
  );

  test(
    "a check with no ci_check whose command a job of an ordinary pull request runs is listed",
    async () => {
      const { report, exitCode } = await rounds(roundScene(), {
        repository: {
          ".github/workflows/rows.yaml": fixture("workflow-runs-c3.yaml"),
          ".github/workflows/rows-job.yaml": fixture("workflow-rows-job.yaml"),
        },
      });
      expect(report.ci?.map((row) => `${row.check} ${row.job}`)).toContain(
        "c3 .github/workflows/rows-job.yaml third-row"
      );
      expect(roundRows(report)).toEqual([
        "c3 a job of an ordinary pull request runs its command, and it carries no ci_check",
      ]);
      expect(exitCode).toBe(1);
    },
    SLOW
  );

  test(
    "a ci_check that names a job which does not run the check's proof is listed",
    async () => {
      const scene = roundScene();
      const checks = scene.checks.map((check) =>
        check.block_id === "c5" ? { ...check, ci_check: "job-that-does-not-exist" } : check
      );
      const { report, exitCode } = await rounds({ ...scene, checks });
      expect(roundRows(report)).toContain(
        "c5 its ci_check names job-that-does-not-exist, and no job of that name runs its command on an ordinary pull request"
      );
      expect(exitCode).toBe(1);
    },
    SLOW
  );

  test(
    "a result a role recorded during the round on a check that now carries ci_check is listed",
    async () => {
      const recorded = {
        id: "c5-implementer",
        session: "ses-implementer",
        actor: { kind: "session", id: "ses-implementer" },
        role: "implementer",
        verdict: "pass",
        commit_sha: "H1",
        created_at: "2026-11-24T10:05:00Z",
        run: "bun test fixtures/c5.test.ts",
        observed: "1 pass.",
      };
      const { report, exitCode } = await rounds(withResult(roundScene(), "c5", recorded));
      expect(roundRows(report)).toContain(
        "c5 a role recorded a result on it during the round, so it carried no ci_check when that result was written"
      );
      expect(exitCode).toBe(1);
    },
    SLOW
  );

  for (const [title, body] of [
    [
      "a job that never runs",
      "    if: false\n    steps:\n      - run: bun test fixtures/c3.test.ts\n",
    ],
    [
      "a step that never runs",
      `    steps:\n      - if: \${{ false }}\n        run: bun test fixtures/c3.test.ts\n`,
    ],
    [
      "a step that only prints the command",
      "    steps:\n      - run: echo 'bun test fixtures/c3.test.ts'\n",
    ],
  ] as const) {
    test(
      `${title} proves nothing, so the round still needs its run`,
      async () => {
        const { report, exitCode } = await rounds(roundScene(), {
          repository: { ".github/workflows/rows.yaml": pullRequestJob("rows", body) },
        });
        expect(report.ci?.map((row) => row.check)).not.toContain("c3");
        expect(roundRows(report)).toEqual(["c3 no result of the round's session at the head"]);
        expect(exitCode).toBe(1);
      },
      SLOW
    );
  }

  test(
    "a job under a condition the census cannot read reports UNKNOWN",
    async () => {
      const body = `    if: \${{ needs.changes.outputs.rows == 'true' }}\n    steps:\n      - run: bun test fixtures/c3.test.ts\n`;
      const { report, exitCode } = await rounds(roundScene(), {
        repository: { ".github/workflows/rows.yaml": pullRequestJob("rows", body) },
      });
      expect(report.unknown.map((row) => row.reason).join("\n")).toContain(
        "needs.changes.outputs.rows"
      );
      expect(exitCode).toBe(3);
    },
    SLOW
  );

  test(
    "a workflow that does not parse reports UNKNOWN",
    async () => {
      const { report, exitCode } = await rounds(roundScene(), {
        repository: { ".github/workflows/broken.yaml": "on: [pull_request\njobs: {\n" },
      });
      expect(report.unknown.map((row) => row.reason).join("\n")).toContain(
        ".github/workflows/broken.yaml"
      );
      expect(exitCode).toBe(3);
    },
    SLOW
  );

  test(
    "a plan that gives the check one run at a head leaves it out, naming the sentence",
    async () => {
      const { report, exitCode } = await rounds({
        ...roundScene(),
        plan: fixture("round-plan-one-run.md"),
      });
      const excluded = report.excluded?.find((row) => row.check === "c3");
      expect(excluded?.reason).toBe("its plan gives it one run at a head");
      expect(excluded?.detail).toContain("`c3` runs once at the head, by the implementer.");
      expect(report.rows).toEqual([]);
      expect(exitCode).toBe(0);
    },
    SLOW
  );
});

const PROMPTS = "packages/daemon/internal/prompts";

/** The planted daemon module's `prompts.go`: one that compiles, in the shape of the daemon's own,
 *  so that the Go test the reading runs gets from `Compose` the parts this source gives.
 *  `sharedOfWorkers` is the phase workers' shared list, and `afterShared` the lines that follow
 *  it in their case. */
function promptsSource(sharedOfWorkers: string, afterShared = ""): string {
  return `// Package prompts composes the role instructions the daemon gives a pane, from the role
// prompts it embeds.
package prompts

import (
	"bytes"
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

//go:embed roles
var roleParts embed.FS

//go:embed go/*.md
var goParts embed.FS

type Parts struct {
	RolePromptPaths []string
}

type Composer struct {
	sharedDir string
	goDir     string
}

func eachPart(parts embed.FS, dir string, visit func(name string, body []byte) error) error {
	return fs.WalkDir(parts, dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		body, err := parts.ReadFile(path)
		if err != nil {
			return err
		}
		return visit(strings.TrimPrefix(path, dir+"/"), body)
	})
}

func New(stateDir string) (*Composer, error) {
	sharedDir := filepath.Join(stateDir, "prompts", "shared")
	if err := eachPart(roleParts, "roles", func(name string, body []byte) error {
		return syncPrompt(filepath.Join(sharedDir, name), body, "shared role prompt")
	}); err != nil {
		return nil, err
	}
	goDir := filepath.Join(stateDir, "prompts", "go")
	if err := eachPart(goParts, "go", func(name string, body []byte) error {
		return syncPrompt(filepath.Join(goDir, name), body, "Go daemon prompt")
	}); err != nil {
		return nil, err
	}
	return &Composer{sharedDir: sharedDir, goDir: goDir}, nil
}

func syncPrompt(path string, body []byte, kind string) error {
	written, err := os.ReadFile(path)
	if err == nil && bytes.Equal(written, body) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create %s directory %s: %w", kind, filepath.Dir(path), err)
	}
	return os.WriteFile(path, body, 0o600)
}

func (c *Composer) Compose(role claim.Role, isRoot bool) (Parts, error) {
	if c == nil {
		return Parts{}, fmt.Errorf("compose role prompt: nil composer")
	}
	var shared, daemonParts []string
	switch role {
	case claim.RoleArchitect:
		name := "architect.md"
		if isRoot {
			name = "architect-root.md"
		}
		shared, daemonParts = []string{name}, []string{name, "architect-common.md"}
	case claim.RolePlanner, claim.RoleImplementer, claim.RoleTester, claim.RoleReviewer:
		name := string(role)
		shared = []string{${sharedOfWorkers}}
${afterShared}		daemonParts = []string{name + ".md", "worker-common.md"}
	case claim.RoleMerger:
		shared, daemonParts = []string{"mechanics/headless.md", "merger.md"}, []string{"merger.md", "worker-common.md"}
	default:
		return Parts{}, fmt.Errorf("compose role prompt: unsupported role %q", role)
	}

	paths := make([]string, 0, len(shared)+len(daemonParts))
	for _, part := range shared {
		paths = append(paths, filepath.Join(c.sharedDir, part))
	}
	for _, part := range daemonParts {
		paths = append(paths, filepath.Join(c.goDir, part))
	}
	return Parts{RolePromptPaths: slices.Clone(paths)}, nil
}
`;
}

const WORKER_SHARED = `"core/common.md", filepath.Join("core", name+".md"), "mechanics/headless.md", name + ".md"`;

/** The rest of the planted daemon module: its `go.mod`, with a `go` line any Go this suite runs
 *  on accepts, and the role package `Compose` takes a role of. */
const DAEMON_MODULE: Record<string, string> = {
  "packages/daemon/go.mod": "module github.com/sjawhar/legion/daemon\n\ngo 1.22\n",
  "packages/daemon/internal/claim/claim.go": `// Package claim names the roles a claim can be on.
package claim

// Role is a role a claim can be on.
type Role string

const (
	RoleArchitect   Role = "architect"
	RolePlanner     Role = "planner"
	RoleImplementer Role = "implementer"
	RoleTester      Role = "tester"
	RoleReviewer    Role = "reviewer"
	RoleMerger      Role = "merger"
)
`,
};

const CLEAN_PROMPTS: Record<string, string> = {
  ...DAEMON_MODULE,
  [`${PROMPTS}/prompts.go`]: promptsSource(WORKER_SHARED),
  [`${PROMPTS}/roles/core/common.md`]: "Read the task.\n",
  [`${PROMPTS}/roles/core/tester.md`]:
    "Start skeptical: the work is broken until your own run proves otherwise.\n",
  [`${PROMPTS}/roles/mechanics/headless.md`]: "You run with no person watching.\n",
  [`${PROMPTS}/roles/tester.md`]:
    "You run each of your `before_merge` checks at each head you test.\n",
  [`${PROMPTS}/go/tester.md`]:
    "Record each fail you see as a fail result at the head your record names.\n",
  [`${PROMPTS}/go/reviewer.md`]: "Read the states Dispatch publishes.\n",
  [`${PROMPTS}/go/architect-common.md`]: "Read the results at each open head.\n",
  [`${PROMPTS}/roles/architect.md`]: "Read the results at each open head.\n",
  [`${PROMPTS}/roles/architect-root.md`]: "Read the results at each open head.\n",
  [`${PROMPTS}/roles/implementer.md`]: "Read the results at each open head.\n",
  [`${PROMPTS}/roles/core/implementer.md`]: "Read the results at each open head.\n",
  [`${PROMPTS}/roles/reviewer.md`]: "Read the results at each open head.\n",
  [`${PROMPTS}/roles/core/reviewer.md`]: "checks green at the head you review.\n",
  [`${PROMPTS}/go/worker-common.md`]:
    "Retract a result of your own that observed wrongly: `dispatch result --retract <result id> --reason '<why>'`.\n",
};

/** The six parts `Compose` gives the tester in the clean planting, in its order. */
const CLEAN_TESTER_PARTS = [
  `${PROMPTS}/roles/core/common.md`,
  `${PROMPTS}/roles/core/tester.md`,
  `${PROMPTS}/roles/mechanics/headless.md`,
  `${PROMPTS}/roles/tester.md`,
  `${PROMPTS}/go/tester.md`,
  `${PROMPTS}/go/worker-common.md`,
];

/** A planted main for the reading: the prompts, the merge of the pull request the reading names
 *  (`afterMerge`), this work's merge, which puts a removed field back in the tester's prompt
 *  (`merged`), a later main that takes it out again (`later`, the checkout's HEAD), a merge that
 *  holds none of them (`sibling`), and a stand-in `gh` that answers `afterMerge` as the merge
 *  commit of the named pull request. */
function plantReading(prompts: Record<string, string> = CLEAN_PROMPTS): {
  repository: string;
  afterMerge: string;
  merged: string;
  later: string;
  sibling: string;
  path: string;
} {
  const directory = plantRepository(prompts);
  const root = gitIn(directory, "rev-parse", "HEAD");
  gitIn(directory, "commit", "-q", "--allow-empty", "-m", "the merge the reading names");
  const afterMerge = gitIn(directory, "rev-parse", "HEAD");
  writeFiles(directory, { [`${PROMPTS}/roles/tester.md`]: "Fill `implementerProof` in.\n" });
  gitIn(directory, "commit", "-q", "-a", "-m", "the merge of this work");
  const merged = gitIn(directory, "rev-parse", "HEAD");
  writeFiles(directory, {
    [`${PROMPTS}/roles/tester.md`]: prompts[`${PROMPTS}/roles/tester.md`] ?? "",
  });
  gitIn(directory, "commit", "-q", "-a", "-m", "a later main");
  const later = gitIn(directory, "rev-parse", "HEAD");
  gitIn(directory, "checkout", "-q", "-b", "sibling", root);
  gitIn(directory, "commit", "-q", "--allow-empty", "-m", "a merge that holds none of them");
  const sibling = gitIn(directory, "rev-parse", "HEAD");
  gitIn(directory, "checkout", "-q", "main");

  const path = makeDirectory();
  writeFileSync(join(path, "gh"), `#!/bin/sh\necho '{"mergeCommit":{"oid":"${afterMerge}"}}'\n`, {
    mode: 0o755,
  });
  return { repository: directory, afterMerge, merged, later, sibling, path };
}

async function mainRead(
  planted: { repository: string; path: string },
  merge: string,
  path = `${planted.path}:${process.env.PATH ?? ""}`
): Promise<{ report: CensusReport; exitCode: number; out: string }> {
  const out = makeDirectory();
  const run = await runCensus(
    ["main-read", "--merge", merge, "--after", "4230", "--repo", planted.repository, "--out", out],
    { PATH: path }
  );
  const report = JSON.parse(await readFile(join(out, "report.json"), "utf8")) as CensusReport;
  return { report, exitCode: run.exitCode, out };
}

/** A PATH that holds every command the reading starts but `go`: Bun, git, tar and the planted
 *  `gh`, each a link in a folder of its own. */
function pathWithoutGo(planted: { path: string }): string {
  const bin = makeDirectory();
  for (const [name, target] of [
    ["bun", process.execPath],
    ["git", Bun.which("git")],
    ["tar", Bun.which("tar")],
  ] as const) {
    if (target === null) throw new Error(`no ${name} on this PATH to link`);
    symlinkSync(target, join(bin, name));
  }
  return `${planted.path}:${bin}`;
}

/** Each UNKNOWN of the reading that says it cannot tell the tester's parts. */
function cannotTell(report: CensusReport): string[] {
  return report.unknown
    .filter((row) => row.what === "the tester's parts")
    .map((row) => row.reason)
    .filter((reason) => reason.startsWith("cannot tell the tester's parts: "));
}

function verdicts(report: CensusReport): Record<string, string> {
  return Object.fromEntries((report.steps ?? []).map((step) => [step.name, step.verdict]));
}

describe("the reading of main", () => {
  test(
    "it reads one commit: its greps, its files and the Go tests the packages lack as not run",
    async () => {
      const planted = plantReading();
      const { report, exitCode, out } = await mainRead(planted, planted.merged);
      expect(report.unknown).toEqual([]);
      expect(report.rows).toEqual([]);
      expect(exitCode).toBe(0);
      expect(report.subject?.read).toBe(planted.later);
      const steps = verdicts(report);
      expect(steps.order).toBe("pass");
      expect(steps["the read commit holds the merge"]).toBe("pass");
      expect(steps["v-tester-failures step 1"]).toBe("pass");
      expect(steps["v-prompt-readers step 1"]).toBe("pass");
      expect(steps["v-retract step 1"]).toBe("pass");
      expect(steps.TestJudgeStates).toBe("not run");
      expect(steps.TestCIResultRulesLiveInRolePrompts).toBe("not run");
      expect(steps.TestNoRolePartAsksAPassOfNamedRoles).toBe("not run");
      const notRun = (report.steps ?? []).find(
        (step) => step.name === "TestNoRolePartAsksAPassOfNamedRoles"
      );
      expect(notRun?.reason).toContain("no package under packages/daemon holds it");
      // The tester's parts come from `Compose` at the read commit, and its source goes with them.
      expect(report.files?.tester).toEqual([
        `${PROMPTS}/roles/core/common.md`,
        `${PROMPTS}/roles/core/tester.md`,
        `${PROMPTS}/roles/mechanics/headless.md`,
        `${PROMPTS}/roles/tester.md`,
        `${PROMPTS}/go/tester.md`,
        `${PROMPTS}/go/worker-common.md`,
      ]);
      expect(report.files?.written).toContain(`${PROMPTS}/prompts.go`);
      // The files written are the read commit's, not the merge's.
      const tester = await readFile(join(out, "files", PROMPTS, "roles/tester.md"), "utf8");
      expect(tester).not.toContain("implementerProof");
    },
    SLOW
  );

  test(
    "a checkout at the merge reads the merge, and its grep fails there",
    async () => {
      const planted = plantReading();
      gitIn(planted.repository, "checkout", "-q", "--detach", planted.merged);
      const { report, exitCode, out } = await mainRead(planted, planted.merged);
      expect(report.subject?.read).toBe(planted.merged);
      expect(verdicts(report)["v-tester-failures step 1"]).toBe("fail");
      const tester = await readFile(join(out, "files", PROMPTS, "roles/tester.md"), "utf8");
      expect(tester).toContain("implementerProof");
      expect(exitCode).toBe(1);
    },
    SLOW
  );

  test(
    "a checkout whose tracked files differ from its commit reports UNKNOWN",
    async () => {
      const planted = plantReading();
      writeFiles(planted.repository, {
        [`${PROMPTS}/roles/tester.md`]: "An edit not committed.\n",
      });
      const { report, exitCode } = await mainRead(planted, planted.merged);
      expect(report.unknown.map((row) => row.reason).join("\n")).toContain("differ from");
      expect(exitCode).toBe(3);
    },
    SLOW
  );

  test(
    "a part Compose names that the read commit lacks reports UNKNOWN",
    async () => {
      const planted = plantReading({
        ...CLEAN_PROMPTS,
        [`${PROMPTS}/prompts.go`]: promptsSource(`${WORKER_SHARED}, "mechanics/extra.md"`),
      });
      const { report, exitCode } = await mainRead(planted, planted.merged);
      expect(report.files?.tester).toHaveLength(7);
      expect(report.unknown.map((row) => row.what).join("\n")).toContain(
        `${PROMPTS}/roles/mechanics/extra.md`
      );
      expect(exitCode).toBe(3);
    },
    SLOW
  );

  test(
    "a part Compose appends after the list literal is one of the tester's parts",
    async () => {
      const planted = plantReading({
        ...CLEAN_PROMPTS,
        [`${PROMPTS}/prompts.go`]: promptsSource(
          WORKER_SHARED,
          '\t\tshared = append(shared, "mechanics/extra.md")\n'
        ),
      });
      const { report, exitCode } = await mainRead(planted, planted.merged);
      const extra = `${PROMPTS}/roles/mechanics/extra.md`;
      expect(report.files?.tester).toEqual([
        ...CLEAN_TESTER_PARTS.slice(0, 4),
        extra,
        ...CLEAN_TESTER_PARTS.slice(4),
      ]);
      expect(cannotTell(report)).toEqual([]);
      // The planted commit lacks the appended part, which is UNKNOWN by name.
      expect(report.unknown.map((row) => row.what)).toEqual([extra]);
      expect(report.files?.missing).toContain(extra);
      expect(exitCode).toBe(3);
    },
    SLOW
  );

  test(
    "a prompts package that does not compile at the read commit cannot tell the tester's parts",
    async () => {
      const planted = plantReading({
        ...CLEAN_PROMPTS,
        [`${PROMPTS}/prompts.go`]: promptsSource(
          WORKER_SHARED,
          "\t\tshared = append(shared, aPartNothingDeclares)\n"
        ),
      });
      const { report, exitCode } = await mainRead(planted, planted.merged);
      const reasons = cannotTell(report);
      expect(reasons).toHaveLength(1);
      expect(reasons[0]).toContain("go test answered");
      expect(reasons[0]).toContain("aPartNothingDeclares");
      expect(report.files?.tester).toEqual([]);
      expect(exitCode).toBe(3);
    },
    SLOW
  );

  test(
    "a machine with no go cannot tell the tester's parts, and runs no pin test",
    async () => {
      const planted = plantReading({
        ...CLEAN_PROMPTS,
        [`${PROMPTS}/judge_test.go`]:
          'package prompts\n\nimport "testing"\n\nfunc TestJudgeStates(t *testing.T) {}\n',
      });
      const path = pathWithoutGo(planted);
      // The control on the planting: this PATH finds git and no go.
      expect(Bun.which("git", { PATH: path })).not.toBeNull();
      expect(Bun.which("go", { PATH: path })).toBeNull();
      const { report, exitCode } = await mainRead(planted, planted.merged, path);
      const reasons = cannotTell(report);
      expect(reasons).toHaveLength(1);
      expect(reasons[0]).toContain('"go"');
      expect(report.files?.tester).toEqual([]);
      const judge = (report.steps ?? []).find((step) => step.name === "TestJudgeStates");
      expect(judge?.verdict).toBe("not run");
      expect(judge?.reason).toContain("go did not start");
      expect(exitCode).toBe(3);
    },
    SLOW
  );

  test(
    "a merge that does not hold the other pull request's merge fails the order step",
    async () => {
      const planted = plantReading();
      const { report, exitCode } = await mainRead(planted, planted.sibling);
      expect(verdicts(report).order).toBe("fail");
      expect(report.rows.map((row) => row.reason).join("\n")).toContain(
        "is no ancestor of the merge read"
      );
      expect(exitCode).toBe(1);
    },
    SLOW
  );
});
