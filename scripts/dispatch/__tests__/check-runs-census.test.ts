import { afterEach, describe, expect, test } from "bun:test";
import { mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { readFile } from "node:fs/promises";
import { dirname, join } from "node:path";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import type { GraphEdge, GraphNode } from "../../../packages/contracts/src/dispatch-api";
import type { CensusReport } from "../check-runs-census.ts";

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
  readonly documents: readonly StubDocument[];
}

/** The reference the Verification document names as the source of the first list. */
const REFERENCED_BY = "dispatch://INFRA-1196/comment/c0b271c7-fd09-485f-8787-862d45795160";

const CLEAN = fixture("clean-verification.md");

/** Every issue of the stand-in project, and the documents each holds. The named issues are the
 *  Verification document's own controls; `INFRA-9001` to `INFRA-9060` make the `todo` listing
 *  longer than one page, and row 55 of it holds the planted p1. `unreadable` adds the issue
 *  whose named Verification document does not load.
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
  Object.assign(issues, {
    "INFRA-3945": {
      status: "done",
      documents: verification(fixture("p1-bold-line-break.md")),
    },
    "INFRA-3939": {
      status: "retro",
      documents: verification(fixture("p1-bold-line-break.md")),
    },
  });
  for (let index = 1; index <= 60; index++) {
    const key = `INFRA-${9000 + index}`;
    issues[key] = {
      status: "todo",
      documents: verification(index === 55 ? fixture("p1-bold-line-break.md") : CLEAN),
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

/** The documents the first list names, as `GET /api/v1/references` answers them: an artifact node
 *  for each, and one block node, whose row is its document. */
function referencedEdges(): GraphEdge[] {
  const artifacts = [
    "INFRA-3903",
    "INFRA-1588",
    "INFRA-3937",
    "INFRA-3940",
    "INFRA-3945",
    "INFRA-3939",
  ];
  const edges = artifacts.map((issue) =>
    edge(
      node({
        kind: "artifact",
        id: `artifact-${issue}`,
        issue_key: issue,
        ref: `dispatch://${issue}/artifact/verification-md`,
      })
    )
  );
  edges.push(
    edge(
      node({
        kind: "block",
        id: "dispatch://INFRA-1152/artifact/verification-md#b-one",
        block_id: "b-one",
        issue_key: "INFRA-1152",
        document: "dispatch://INFRA-1152/artifact/verification-md",
      })
    )
  );
  return edges;
}

/** A stand-in Dispatch for the text census: the first list's references, the project's issues by
 *  status with their totals, each issue's documents list, and each document's text. */
function stubTexts(issues: Record<string, StubIssue>): string {
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
          edges: referencedEdges(),
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
        return Response.json({
          issues: keys.slice(offset, offset + limit).map((key) => ({ key })),
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

describe("the text census", () => {
  test(
    "it lists each old sentence of the two sources and stays quiet on an owner's own rule",
    async () => {
      const url = stubTexts(textsIssues());
      const { report, exitCode } = await census(
        ["texts", "--project", "INFRA", "--referenced-by", REFERENCED_BY],
        url
      );

      expect(report.unknown).toEqual([]);
      expect(report.complete).toBe(true);
      // Row 55 of `todo` is past the first page, so a run that reads one page misses its planted p1.
      expect(listed(report)).toContain("INFRA-9055 verification-md p1");
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
      expect(report.read?.issues).toBe(68);
      expect(report.read?.texts).toBe(66);
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
      const url = stubTexts(textsIssues());
      const { report, exitCode } = await census(
        ["texts", "--project", "INFRA", "--referenced-by", REFERENCED_BY, "--break-patterns"],
        url
      );

      expect(listed(report)).toContain("INFRA-9055 verification-md p1");
      expect(listed(report).join("\n")).not.toContain("INFRA-3937");
      expect(listed(report).join("\n")).not.toContain("INFRA-3940");
      expect(exitCode).toBe(1);
    },
    SLOW
  );

  test(
    "a text that does not load reports UNKNOWN",
    async () => {
      const issues = textsIssues({ unreadable: true });
      const url = stubTexts(issues);
      const { report, exitCode } = await census(
        ["texts", "--project", "INFRA", "--referenced-by", REFERENCED_BY],
        url
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
      const url = stubTexts(textsIssues());
      const { report, exitCode } = await census(
        ["texts", "--project", "INFRA", "--referenced-by", REFERENCED_BY, "--limit", "50"],
        url
      );
      expect(report.unknown.map((row) => row.reason).join("\n")).toContain("50 of 60");
      expect(exitCode).toBe(3);
    },
    SLOW
  );

  test(
    "a listed issue is read whether it is done or not, and a run with nothing to list exits 0",
    async () => {
      const url = stubTexts(textsIssues());
      const directory = makeDirectory();
      const keys = join(directory, "keys.txt");
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

/** One result of the round's session on `c3` at `H1`, as the three controls change it. */
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

function withC3(scene: RoundScene, result: Record<string, unknown>): RoundScene {
  const checks = scene.checks.map((check) =>
    check.block_id === "c3"
      ? { ...check, results: [...(check.results as unknown[]), result] }
      : check
  );
  return { ...scene, checks };
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

function stubRounds(scene: RoundScene, options: { loseOutput?: boolean } = {}): string {
  const server = Bun.serve({
    hostname: "127.0.0.1",
    port: 0,
    fetch(request) {
      const url = new URL(request.url);
      const path = url.pathname;
      const origin = `http://127.0.0.1:${server.port}`;
      const fill = (value: string): string => value.replaceAll("{stub}", origin);
      if (path === "/api/v1/issues") return Response.json([{ key: scene.issue }]);
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

/** A planted checkout whose one commit `H1` holds `files`. */
function plantRepository(files: Record<string, string>): string {
  const directory = makeDirectory();
  for (const [path, body] of Object.entries(files)) {
    mkdirSync(join(directory, dirname(path)), { recursive: true });
    writeFileSync(join(directory, path), body);
  }
  const git = (...args: string[]): void => {
    const run = Bun.spawnSync(["git", "-C", directory, ...args], {
      stdout: "pipe",
      stderr: "pipe",
    });
    if (run.exitCode !== 0) {
      throw new Error(`git ${args.join(" ")}: ${run.stderr.toString()}`);
    }
  };
  git("init", "-q", "-b", "main");
  git("config", "user.email", "census@example.invalid");
  git("config", "user.name", "Census");
  git("add", "-A");
  git("commit", "-q", "-m", "plant");
  // A branch, not a tag: the box signs an annotated tag, which needs a message of its own.
  git("branch", "H1");
  return directory;
}

const ROUND_REPOSITORY = {
  ".github/workflows/unrelated.yaml":
    "name: unrelated\non:\n  pull_request:\njobs:\n  idle:\n    runs-on: ubuntu-24.04\n    steps:\n      - run: echo nothing\n",
};

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
      // A check whose words give its run to a person, one carrying `ci_check`, and one that is not
      // `before_merge` are all out of the round.
      expect(report.excluded?.map((row) => `${row.check} ${row.reason}`)).toEqual([
        "c4 its words give its run only to another runner",
        "c5 it carries ci_check",
        "c6 its when is after_merge",
      ]);
      // A result whose actor does not carry `proven` is named.
      expect(report.unproven).toEqual(["c2-tester"]);
      expect(keys).toBe("ACME-1\n");
      expect(exitCode).toBe(1);
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
    "a check whose command a job of an ordinary pull request runs is printed as CI's",
    async () => {
      const { report, exitCode } = await rounds(roundScene(), {
        repository: {
          ".github/workflows/rows.yaml": fixture("workflow-runs-c3.yaml"),
          ".github/workflows/rows-job.yaml": fixture("workflow-rows-job.yaml"),
        },
      });
      expect(report.ci?.map((row) => `${row.check} ${row.job}`)).toEqual([
        "c3 .github/workflows/rows-job.yaml third-row",
      ]);
      expect(report.rows).toEqual([]);
      expect(exitCode).toBe(0);
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

/** A planted checkout for the reading of main: the prompt files the three greps read, one commit
 *  for each side of the order step, and a stand-in `gh` that answers the merge commit of the
 *  pull request the reading names. */
function plantMainRead(): {
  repository: string;
  afterMerge: string;
  merged: string;
  sibling: string;
  path: string;
} {
  const directory = makeDirectory();
  const prompts = "packages/daemon/internal/prompts";
  const files: Record<string, string> = {
    [`${prompts}/roles/tester.md`]:
      "You run each of your `before_merge` checks at each head you test.\n",
    [`${prompts}/roles/core/tester.md`]:
      "Start skeptical: the work is broken until your own run proves otherwise.\n",
    [`${prompts}/go/tester.md`]:
      "Record each fail you see as a fail result at the head your record names.\n",
    [`${prompts}/go/reviewer.md`]: "Read the states Dispatch publishes.\n",
    [`${prompts}/go/architect-common.md`]: "Read the results at each open head.\n",
    [`${prompts}/roles/architect.md`]: "Read the results at each open head.\n",
    [`${prompts}/roles/architect-root.md`]: "Read the results at each open head.\n",
    [`${prompts}/roles/implementer.md`]: "Read the results at each open head.\n",
    [`${prompts}/roles/core/implementer.md`]: "Read the results at each open head.\n",
    [`${prompts}/roles/reviewer.md`]: "Read the results at each open head.\n",
    [`${prompts}/roles/core/reviewer.md`]: "checks green at the head you review.\n",
    [`${prompts}/go/worker-common.md`]:
      "Retract a result of your own that observed wrongly: `dispatch result --retract <result id> --reason '<why>'`.\n",
  };
  for (const [path, body] of Object.entries(files)) {
    mkdirSync(join(directory, dirname(path)), { recursive: true });
    writeFileSync(join(directory, path), body);
  }
  const git = (...args: string[]): string => {
    const run = Bun.spawnSync(["git", "-C", directory, ...args], {
      stdout: "pipe",
      stderr: "pipe",
    });
    if (run.exitCode !== 0) throw new Error(`git ${args.join(" ")}: ${run.stderr.toString()}`);
    return run.stdout.toString().trim();
  };
  git("init", "-q", "-b", "main");
  git("config", "user.email", "census@example.invalid");
  git("config", "user.name", "Census");
  git("add", "-A");
  git("commit", "-q", "-m", "the prompts");
  const root = git("rev-parse", "HEAD");
  git("commit", "-q", "--allow-empty", "-m", "the merge of the pull request the reading names");
  const afterMerge = git("rev-parse", "HEAD");
  git("commit", "-q", "--allow-empty", "-m", "the merge of this work");
  const merged = git("rev-parse", "HEAD");
  git("checkout", "-q", "-b", "sibling", root);
  git("commit", "-q", "--allow-empty", "-m", "a merge that does not hold the other");
  const sibling = git("rev-parse", "HEAD");
  git("checkout", "-q", "main");

  const path = makeDirectory();
  const gh = join(path, "gh");
  writeFileSync(gh, `#!/bin/sh\necho '{"mergeCommit":{"oid":"${afterMerge}"}}'\n`, { mode: 0o755 });
  return { repository: directory, afterMerge, merged, sibling, path };
}

describe("the reading of main", () => {
  test(
    "it runs the three greps, reports each Go test the packages lack as not run, and writes the files",
    async () => {
      const planted = plantMainRead();
      const out = makeDirectory();
      const run = await runCensus(
        [
          "main-read",
          "--merge",
          planted.merged,
          "--after",
          "4230",
          "--repo",
          planted.repository,
          "--out",
          out,
        ],
        { PATH: `${planted.path}:${process.env.PATH ?? ""}` }
      );
      expect(run.exitCode).toBe(0);
      const report = JSON.parse(await readFile(join(out, "report.json"), "utf8")) as CensusReport;
      const verdicts = Object.fromEntries(
        (report.steps ?? []).map((step) => [step.name, step.verdict])
      );
      expect(verdicts.order).toBe("pass");
      expect(verdicts["v-tester-failures step 1"]).toBe("pass");
      expect(verdicts["v-prompt-readers step 1"]).toBe("pass");
      expect(verdicts["v-retract step 1"]).toBe("pass");
      expect(verdicts.TestJudgeStates).toBe("not run");
      expect(verdicts.TestCIResultRulesLiveInRolePrompts).toBe("not run");
      expect(verdicts.TestNoRolePartAsksAPassOfNamedRoles).toBe("not run");
      const notRun = (report.steps ?? []).find(
        (step) => step.name === "TestNoRolePartAsksAPassOfNamedRoles"
      );
      expect(notRun?.reason).toContain("no package under packages/daemon holds it");
      expect(report.rows).toEqual([]);
      expect(report.unknown).toEqual([]);
      const written = await readFile(
        join(out, "files", "packages/daemon/internal/prompts/go/worker-common.md"),
        "utf8"
      );
      expect(written).toContain("dispatch result --retract");
    },
    SLOW
  );

  test(
    "a merge that does not hold the other pull request's merge fails the order step",
    async () => {
      const planted = plantMainRead();
      const out = makeDirectory();
      const run = await runCensus(
        [
          "main-read",
          "--merge",
          planted.sibling,
          "--after",
          "4230",
          "--repo",
          planted.repository,
          "--out",
          out,
        ],
        { PATH: `${planted.path}:${process.env.PATH ?? ""}` }
      );
      expect(run.exitCode).toBe(1);
      const report = JSON.parse(await readFile(join(out, "report.json"), "utf8")) as CensusReport;
      const order = (report.steps ?? []).find((step) => step.name === "order");
      expect(order?.verdict).toBe("fail");
      expect(report.rows.map((row) => row.reason).join("\n")).toContain(
        "is no ancestor of the merge read"
      );
    },
    SLOW
  );
});
