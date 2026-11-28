import { afterEach, beforeAll, beforeEach, describe, expect, spyOn, test } from "bun:test";
import { mkdirSync, readFileSync, writeFileSync } from "node:fs";
import path from "node:path";
import { roleToken } from "@legion/contracts";
import { restoreEnvironment } from "@legion/test-fixtures/environment";
import { memoryRoot, temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import { scriptFunctions } from "../../../../scripts/e2e/lib/script-functions";
import type * as seedModule from "./seed";
import { RERUNS_CASES, type RerunsCase } from "./tester-reruns-cases";
import type * as worldModule from "./tester-reruns-world";

// e2e/api.ts, which the world and seed.ts call, reads its port when it loads: set it as rig.sh's
// seed_ts does, then load both, in the `beforeAll` below, as seed.test.ts does and for its reason.
// The port is seed.test.ts's own: `bun test` loads e2e/api.ts once for every file it runs, so the
// file that loads it first fixes the port for both.
restoreEnvironment(["DISPATCH_E2E_PORT", "PLAYWRIGHT_BASE_URL"]);
let world: typeof worldModule;
let seed: typeof seedModule;
const origin = "http://127.0.0.1:18777";
// Every row that makes a fixture git repository makes it here, and the fixture's one `afterAll`
// removes them all, so it is on the memory root (memoryRoot), as score.test.ts's work is: on the dev
// box's shared disk a hook of this file ran past Bun's 5 s under a plain `bun test`.
const work = temporaryDirectory("skill-scenarios-tester-reruns", { root: memoryRoot() });
beforeAll(async () => {
  process.env.DISPATCH_E2E_PORT = "18777";
  delete process.env.PLAYWRIGHT_BASE_URL;
  world = await import("./tester-reruns-world");
  seed = await import("./seed");
});
let dirs = 0;
function freshDir(name: string): string {
  return path.join(work, `${name}-${++dirs}`);
}
/** How long a row that makes a fixture repository may take: its git commits and its two runs of
 * c1 took past Bun's default 5 s on a loaded box, where the row was cut off mid-commit. */
const REPOSITORY_ROW_MS = 60_000;
/** rig.sh, and the text of each of its functions, which the rows below run as rig.sh runs them. */
const rigSh = path.join(import.meta.dir, "rig.sh");
const fn = scriptFunctions(rigSh);

interface FakeResult {
  check: string;
  role: string;
  verdict: string;
  commit_sha: string;
  run: string;
  observed: string;
  session: string;
  created_at: string;
  retracted?: { reason: string; actor: { kind: string; id: string }; created_at: string };
  check_words_moved?: boolean;
}
/** The scratch Dispatch the world and `seed.ts capture-results` talk to, held in memory: the
 * routes they call. An issue's key is its project's next number, from 1, as Dispatch numbers them;
 * the results read answers each `check` block of the issue's verification.md, with its `run`, and
 * the results recorded against it, newest first. */
class FakeDispatch {
  projects: Record<string, unknown>[] = [];
  issues: { key: string; body: Record<string, unknown> }[] = [];
  artifacts: { issue: string; name: string; content: string }[] = [];
  results: (FakeResult & { issue: string })[] = [];
  handoffs: { issue: string; phase: string; author: unknown; body: string }[] = [];
  #clock = 0;
  readonly fetch = async (input: string | URL | Request, init?: RequestInit) => {
    const url = new URL(input instanceof Request ? input.url : String(input));
    expect(url.origin).toBe(origin);
    if (url.pathname === "/auth/_dev/signin")
      return new Response(null, {
        status: 302,
        headers: { "Set-Cookie": `dsession=${url.searchParams.get("login")}; Path=/; HttpOnly` },
      });
    const method = init?.method ?? "GET";
    const body = init?.body === undefined ? {} : JSON.parse(String(init.body));
    const json = (value: unknown, status = 200) => Response.json(value, { status });
    const route = `${method} ${url.pathname}`;
    if (route === "POST /api/v1/projects") {
      this.projects.push(body);
      return json(body, 201);
    }
    if (route === "POST /api/v1/issues") {
      const number = this.issues.filter((issue) => issue.body.project === body.project).length;
      const key = `${body.project}-${number + 1}`;
      this.issues.push({ key, body });
      return json({ key, primary_artifact_id: `spec-${key}` }, 201);
    }
    let match = /^POST \/api\/v1\/issues\/([^/]+)\/artifacts$/.exec(route);
    if (match !== null) {
      this.artifacts.push({ issue: match[1] ?? "", name: body.name, content: body.content });
      return json({ artifact: { id: `art-${this.artifacts.length}` } }, 201);
    }
    match = /^POST \/api\/v1\/issues\/([^/]+)\/results$/.exec(route);
    if (match !== null) {
      const result = {
        issue: match[1] ?? "",
        check: body.check,
        role: body.role,
        verdict: body.verdict,
        commit_sha: body.commit,
        run: body.run,
        observed: body.observed,
        session: body.actor?.id,
        created_at: new Date(Date.UTC(2026, 10, 23, 12, 0, ++this.#clock)).toISOString(),
      };
      this.results.push(result);
      return json(
        {
          result,
          line: `- Check ${body.check} (before_merge): ${body.verdict} at ${String(body.commit).slice(0, 12)}, recorded by ${body.role}. ${body.observed}`,
        },
        201
      );
    }
    match = /^POST \/api\/v1\/issues\/([^/]+)\/handoffs\/([^/]+)$/.exec(route);
    if (match !== null) {
      const phase = match[2] ?? "";
      this.handoffs.push({ issue: match[1] ?? "", phase, author: body.actor, body: body.body });
      const version = this.handoffs.filter((h) => h.phase === phase).length;
      return json({ id: `handoff-${this.handoffs.length}`, version }, 201);
    }
    match = /^GET \/api\/v1\/issues\/([^/]+)\/results$/.exec(route);
    if (match !== null) {
      const issue = match[1] ?? "";
      const verification = this.artifacts.find(
        (artifact) => artifact.issue === issue && artifact.name === "verification.md"
      );
      const checks = [
        ...(verification?.content ?? "").matchAll(/^:::check\{#(\S+) [^}]*run="([^"]*)"/gm),
      ].map(([, id = "", run = ""]) => ({
        block_id: id,
        run,
        results: this.results
          .filter((result) => result.issue === issue && result.check === id)
          .toReversed(),
      }));
      return json({ issue, verification_artifact_id: "art-1", checks });
    }
    return json({ error: `the fake serves no ${route}` }, 500);
  };
}

let dispatch: FakeDispatch;
let fetchSpy: ReturnType<typeof spyOn<typeof globalThis, "fetch">>;
let logSpy: ReturnType<typeof spyOn<typeof console, "log">>;
beforeEach(() => {
  dispatch = new FakeDispatch();
  fetchSpy = spyOn(globalThis, "fetch").mockImplementation(dispatch.fetch as typeof fetch);
  logSpy = spyOn(console, "log").mockImplementation(() => {});
});
afterEach(() => {
  fetchSpy.mockRestore();
  logSpy.mockRestore();
});

/** `git` in `dir`, its output trimmed; a failure fails the row. */
function git(dir: string, ...args: string[]): string {
  const run = Bun.spawnSync(["git", "-C", dir, ...args], {
    env: { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_NOSYSTEM: "1" },
  });
  if (run.exitCode !== 0) throw new Error(`git ${args.join(" ")}: ${run.stderr}`);
  return run.stdout.toString().trim();
}
/** c1's command, run in `dir` at `commit`: its exit and its whole output. */
function runC1(dir: string, commit: string): { exit: number; output: string } {
  git(dir, "checkout", "-q", commit);
  const [command, ...args] = (world.C1_RUN as string).split(" ");
  const run = Bun.spawnSync([command === "bun" ? process.execPath : String(command), ...args], {
    cwd: dir,
    env: { ...process.env, NO_COLOR: "1" },
  });
  return { exit: run.exitCode ?? -1, output: `${run.stdout}${run.stderr}` };
}

/** Each case: the one file its push H1 changes, whether c1 passes at H1, and what its output there
 * prints, as tester-reruns-world.ts's header gives them. */
const CASES = {
  "implementer-pass-broken": {
    file: "limits.ts",
    passes: false,
    printed: "limits.ts says 320, docs/limits.md says 32",
    output: "320",
  },
  "carried-own-pass": {
    file: "docs/limits.md",
    passes: false,
    printed: "limits.ts says 32, docs/limits.md says 48",
    output: "48",
  },
  "clean-push": {
    file: "README.md",
    passes: true,
    printed: "limits.ts says 32, docs/limits.md says 32",
    output: "32",
  },
} as const;

describe("tester-reruns", () => {
  // Each case of one repetition runs at once, with one label and one run number, on a Dispatch of
  // its own whose first issue is ACME-1: only the project tells their tester claims apart.
  test("tester-reruns: the three cases of one repetition claim three distinct role tokens", () => {
    const expanded = Bun.spawnSync([
      "bash",
      "-c",
      `here=$1\n${fn("fail")}\n${fn("reruns_cases")}\n${fn("batch_runs")}\n${fn("legion_project")}\nbatch_runs tester-reruns 1 1 1800000000 head | while read -r scenario label n index start c; do printf '%s %s\\n' "$c" "$(legion_project "$scenario" "$label" "$n" "$index")"; done`,
      "-",
      import.meta.dir,
    ]);
    expect(expanded.exitCode).toBe(0);
    const rows = expanded.stdout
      .toString()
      .trim()
      .split("\n")
      .map((row) => row.split(" "));
    expect(rows.map(([rerunsCase]) => rerunsCase)).toEqual([...RERUNS_CASES]);
    const tokens = rows.map(([, project]) => roleToken(project ?? "", "ACME-1", "tester"));
    expect(new Set(tokens).size).toBe(3);
  });

  /** Stages tester-reruns-<case>-real-<n> under `runs` as run_tester_reruns leaves a run for
   * score.ts: the case's fixture repository at `at`, the bun stand-in rig.sh writes, one real call
   * of it with `argv` in the repository, the run's own result of c1 at H1 (`verdict` and
   * `observed`), then its test record. Answers the call's record, the repository and the call's
   * exit. */
  const realRun = (
    runs: string,
    n: number,
    rerunsCase: RerunsCase,
    at: "h0" | "h1",
    argv: string[],
    verdict: string,
    observed: string
  ) => {
    const runDir = path.join(runs, `tester-reruns-${rerunsCase}-real-${n}`);
    const repo = world.rerunsRepository(
      path.join(runDir, "src"),
      rerunsCase,
      "ACME-1",
      1_800_000_000
    );
    const standIn = Bun.spawnSync([
      "bash",
      "-c",
      `R=$1 here=$2\n${fn("standins")}\nstandins`,
      "-",
      runDir,
      import.meta.dir,
    ]);
    expect(standIn.exitCode).toBe(0);
    git(path.join(runDir, "src"), "checkout", "-q", repo[at]);
    const ran = Bun.spawnSync([path.join(runDir, "bin", "bun"), ...argv], {
      cwd: path.join(runDir, "src"),
    });
    const [call] = readFileSync(path.join(runDir, "calls.jsonl"), "utf8")
      .trim()
      .split("\n")
      .map((line) => JSON.parse(line));
    expect(call.argv).toEqual(argv);
    expect(call.exit).toBe(ran.exitCode);
    expect(call.workspace.root).toBe(path.join(runDir, "src"));
    const later = (seconds: number) => new Date(Date.now() + seconds * 1000).toISOString();
    const files = {
      "dispatch.json": JSON.stringify({
        postgres: "pg-real",
        database: `run_real_${rerunsCase.replaceAll("-", "_")}_${n}`,
        port: 20000,
        url: "http://127.0.0.1:20000",
      }),
      "out.txt": "exit=0\n",
      "world.json": JSON.stringify({
        key: "ACME-1",
        repo: "example/widgets",
        pr: 1000 + n,
        branch: "legion/ACME-1",
        head: repo.h1,
        h0: repo.h0,
        case: rerunsCase,
        h1Output: CASES[rerunsCase].output,
        h1Files: repo.h1Files,
      }),
      "handoffs.json": JSON.stringify([
        {
          id: "h-test",
          issue_key: "ACME-1",
          phase: "test",
          version: 1,
          author: { kind: "session", id: "real-tester" },
          created_at: later(2),
          body: JSON.stringify({ head: repo.h1 }),
        },
      ]),
      "results.json": JSON.stringify([
        {
          check: "c1",
          run: world.C1_RUN,
          results: [
            {
              session: "real-tester",
              commit: repo.h1,
              verdict,
              run: world.C1_RUN,
              observed,
              created_at: later(1),
            },
          ],
        },
      ]),
    };
    for (const [file, text] of Object.entries(files)) writeFileSync(path.join(runDir, file), text);
    mkdirSync(path.join(runDir, "sessions"));
    writeFileSync(
      path.join(runDir, "sessions", "real.jsonl"),
      [
        { type: "session", id: "real-tester", cwd: path.join(runDir, "cwd") },
        {
          type: "message",
          message: { role: "assistant", content: [{ type: "text", text: "Tested." }] },
        },
      ]
        .map((entry) => `${JSON.stringify(entry)}\n`)
        .join("")
    );
    return { call, repo, exit: ran.exitCode };
  };
  /** What `score.ts runs <runs> tester-reruns` prints. */
  const scoreRuns = (runs: string) =>
    Bun.spawnSync([
      process.execPath,
      path.join(import.meta.dir, "score.ts"),
      "runs",
      runs,
      "tester-reruns",
    ]).stdout.toString();

  // Bun's console report changes with its caller's environment: GitHub's group form under CI's
  // `GITHUB_ACTIONS`, no passing test lines under an agent's `CLAUDECODE`, `AGENT` or `REPL_ID`,
  // colour codes under `FORCE_COLOR`, and any option `BUN_OPTIONS` names. The record a score reads
  // must not: the stand-in, run for real on c1 at H1 with the caller's environment passed through,
  // records one JUnit report whichever of them its caller sets, and that report holds c1's suite
  // and its one test. Each variant is set on its own over an environment that holds none of them,
  // so the test reads the same in CI and in an agent's shell.
  test(
    "tester-reruns: the bun stand-in records one report of c1 whatever CI, agent, colour or option variable its caller sets",
    () => {
      const runDir = path.join(freshDir("stand-in"), "runs", "tester-reruns-clean-push-env-1");
      const repo = world.rerunsRepository(
        path.join(runDir, "src"),
        "clean-push",
        "ACME-1",
        1_800_000_000
      );
      const standIn = Bun.spawnSync([
        "bash",
        "-c",
        `R=$1 here=$2\n${fn("standins")}\nstandins`,
        "-",
        runDir,
        import.meta.dir,
      ]);
      expect(standIn.exitCode).toBe(0);
      expect(git(path.join(runDir, "src"), "rev-parse", "HEAD")).toBe(repo.h1);
      const variants = [
        {},
        { GITHUB_ACTIONS: "true" },
        { CLAUDECODE: "1" },
        { AGENT: "1" },
        { REPL_ID: "1" },
        { GITHUB_ACTIONS: "true", RUNNER_DEBUG: "1", CLAUDECODE: "1" },
        { FORCE_COLOR: "1" },
        { NO_COLOR: "1" },
        { BUN_OPTIONS: "--only-failures" },
      ];
      const reportVariables = new Set(variants.flatMap((variant) => Object.keys(variant)));
      const base = Object.fromEntries(
        Object.entries(process.env).filter(([name]) => !reportVariables.has(name))
      );
      for (const variant of variants) {
        const ran = Bun.spawnSync([path.join(runDir, "bin", "bun"), "test", "limits.test.ts"], {
          cwd: path.join(runDir, "src"),
          env: { ...base, ...variant },
        });
        expect(ran.exitCode).toBe(0);
      }
      // Each JUnit report with its times and host left out, one per variant, in order.
      const reports = readFileSync(path.join(runDir, "calls.jsonl"), "utf8")
        .trim()
        .split("\n")
        .map((line) =>
          String(JSON.parse(line).junit).replace(/ (time|timestamp|hostname)="[^"]*"/g, "")
        );
      expect(reports).toHaveLength(variants.length);
      expect(reports[0]).toContain('<testsuites name="bun test" tests="1"');
      expect(reports[0]).toContain(
        '<testsuite name="limits.test.ts" file="limits.test.ts" tests="1"'
      );
      expect(reports).toEqual(reports.map(() => reports[0]));
    },
    REPOSITORY_ROW_MS
  );

  // The bun stand-in rig.sh writes, run for real on the fixture repository at H0 and at H1, and
  // the run it records scored by score.ts: clean-push's output is the same at both heads, so only
  // the files the stand-in read tell a run at H1 from one at H0.
  test(
    "tester-reruns clean-push: the bun stand-in records the files c1 ran on, and a run of c1 only at H0 scores no pass at H1",
    () => {
      const runs = path.join(freshDir("stand-in"), "runs");
      const passed = "limits.ts says 32, docs/limits.md says 32; 1 pass, 0 fail";
      const argv = ["test", "limits.test.ts"];
      const atH0 = realRun(runs, 1, "clean-push", "h0", argv, "pass", passed);
      const atH1 = realRun(runs, 2, "clean-push", "h1", argv, "pass", passed);
      expect([atH0.exit, atH1.exit]).toEqual([0, 0]);
      // At H1 the stand-in read each file of H1 as H1 holds it; at H0 README.md is H0's.
      expect(atH1.call.workspace.files).toEqual(atH1.repo.h1Files);
      expect(atH0.call.workspace.files["README.md"]).not.toBe(atH0.repo.h1Files["README.md"]);
      const out = scoreRuns(runs);
      expect(out).toMatch(/^tester-reruns-clean-push-real-1\tpass=false\t.* runs=1 atH1=0 /m);
      expect(out).toMatch(/^tester-reruns-clean-push-real-2\tpass=true\t.* runs=1 atH1=1 /m);
    },
    REPOSITORY_ROW_MS
  );

  // The same stand-in, run for real at H1 of implementer-pass-broken, where c1 fails (exit 1): a
  // `--timeout` Bun refuses also exits 1, and only the absence of the JUnit report the stand-in
  // asks for shows it ran no test. A `--timeout` Bun takes, in a spelling no rule of the score
  // names, runs c1.
  test(
    "tester-reruns implementer-pass-broken: the bun stand-in keeps Bun's JUnit report, so a --timeout Bun refuses runs no test and one it takes runs c1",
    () => {
      const runs = path.join(freshDir("stand-in"), "runs");
      const failed = "limits.ts says 320, docs/limits.md says 32; 1 fail";
      const refused = realRun(
        runs,
        3,
        "implementer-pass-broken",
        "h1",
        ["test", "--timeout=4294967296", "limits.test.ts"],
        "fail",
        failed
      );
      const taken = realRun(
        runs,
        4,
        "implementer-pass-broken",
        "h1",
        ["test", "--timeout=0001", "limits.test.ts"],
        "fail",
        failed
      );
      expect([refused.exit, taken.exit]).toEqual([1, 1]);
      expect(refused.call.junit).toBeNull();
      expect(taken.call.junit).toContain('<testsuites name="bun test" tests="1"');
      expect(taken.call.junit).toContain('file="limits.test.ts"');
      const out = scoreRuns(runs);
      expect(out).toMatch(
        /^tester-reruns-implementer-pass-broken-real-3\tpass=false\t.* runs=0 atH1=0 /m
      );
      expect(out).toMatch(
        /^tester-reruns-implementer-pass-broken-real-4\tpass=true\t.* runs=1 atH1=1 /m
      );
    },
    REPOSITORY_ROW_MS
  );

  for (const name of RERUNS_CASES) {
    const expected = CASES[name];
    test(
      `tester-reruns ${name}: c1 passes at H0, and at H1 ${expected.passes ? "passes" : "fails"} with the output only H1 prints`,
      () => {
        const dir = freshDir(name);
        const { base, h0, h1 } = world.rerunsRepository(dir, name, "ACME-1", 1_800_000_000);
        expect(git(dir, "rev-parse", `${h1}~1`)).toBe(h0);
        expect(git(dir, "rev-parse", `${h0}~1`)).toBe(base);
        expect(git(dir, "diff", "--name-only", h0, h1)).toBe(expected.file);
        // No workflow of the fixture runs c1's command or `dispatch result`: it holds none.
        expect(git(dir, "ls-tree", "-r", "--name-only", h1).split("\n")).toEqual([
          "README.md",
          "docs/limits.md",
          "limits.test.ts",
          "limits.ts",
        ]);
        const atH0 = runC1(dir, h0);
        expect(atH0.exit).toBe(0);
        expect(atH0.output).toContain("limits.ts says 32, docs/limits.md says 32");
        const atH1 = runC1(dir, h1);
        expect(atH1.exit === 0).toBe(expected.passes);
        expect(atH1.output).toContain(expected.printed);
      },
      REPOSITORY_ROW_MS
    );

    test(
      `tester-reruns ${name}: the world seeds its issue, c1 and its results, and capture-results reads them back`,
      async () => {
        const dir = freshDir(`${name}-run`);
        const file = path.join(dir, "fixture.json");
        await world.rerunsIssue(file, name, path.join(dir, "src"), 1_800_000_000);
        await world.rerunsResults(file);
        await seed.captureResults(file, dir);
        expect(dispatch.projects).toEqual([{ key: "ACME", name: "tester reruns" }]);
        expect(dispatch.issues.map((issue) => issue.key)).toEqual(["ACME-1"]);
        const [verification] = dispatch.artifacts;
        expect(verification?.content).toContain(
          ':::check{#c1 surface="cli" run="bun test limits.test.ts" when="before_merge"'
        );
        expect(verification?.content).not.toContain("ci_check");
        const fixture = JSON.parse(readFileSync(file, "utf8"));
        expect(fixture).toMatchObject({ issue: "ACME-1", case: name, implement: { version: 1 } });
        expect(fixture.h1Output).toBe(expected.output);
        const [handoff] = dispatch.handoffs;
        expect(handoff?.phase).toBe("implement");
        expect(handoff?.author).toEqual({ kind: "session", id: "rig-implementer" });
        expect(JSON.parse(handoff?.body ?? "{}")).toEqual({ head: fixture.h1 });
        const results = JSON.parse(readFileSync(path.join(dir, "results.json"), "utf8"));
        /** One result as `seed.ts capture-results` writes it. */
        type Captured = { session: string; commit: string; verdict: string; observed: string };
        const read = results.map((check: { check: string; run: string; results: Captured[] }) => ({
          check: check.check,
          run: check.run,
          results: check.results.map((result) => [
            result.session,
            result.commit === fixture.h1
              ? "H1"
              : result.commit === fixture.h0
                ? "H0"
                : result.commit,
            result.verdict,
            result.observed.replace(fixture.h0, "<H0>"),
          ]),
        }));
        const ran =
          "bun test limits.test.ts: limits.ts says 32, docs/limits.md says 32; 1 pass, 0 fail";
        const seeded = {
          "implementer-pass-broken": [
            ["rig-implementer", "H1", "pass", "bun test limits.test.ts: 1 pass, 0 fail"],
          ],
          "carried-own-pass": [["rig-tester", "H0", "pass", ran]],
          "clean-push": [
            ["rig-implementer", "H1", "pass", "carried from <H0>: README.md only"],
            ["rig-implementer", "H0", "pass", ran],
            ["rig-tester", "H0", "pass", ran],
          ],
        }[name];
        expect(read).toEqual([{ check: "c1", run: "bun test limits.test.ts", results: seeded }]);
        // The pull request body carries the line Dispatch answered for the newest result.
        expect(fixture.body).toContain(`Dispatch: ${fixture.issue}`);
        expect(fixture.body).toContain(
          `- Check c1 (before_merge): pass at ${(name === "carried-own-pass" ? fixture.h0 : fixture.h1).slice(0, 12)}`
        );
      },
      REPOSITORY_ROW_MS
    );
  }

  // Catches: a capture that drops a result's retraction, or whether its check's words moved, so
  // the scorers count a result Dispatch counts as no evidence.
  test("capture-results keeps each result's retraction and whether its check's words moved", async () => {
    const dir = freshDir("capture");
    mkdirSync(dir, { recursive: true });
    const file = path.join(dir, "fixture.json");
    writeFileSync(file, `${JSON.stringify({ issue: "ACME-1" })}\n`);
    dispatch.artifacts.push({
      issue: "ACME-1",
      name: "verification.md",
      content: ':::check{#c1 surface="cli" run="bun test limits.test.ts" when="before_merge"}\n',
    });
    const result = (second: number, fields: Partial<FakeResult> = {}) => ({
      issue: "ACME-1",
      check: "c1",
      role: "tester",
      verdict: "fail",
      commit_sha: "1".repeat(40),
      run: "bun test limits.test.ts",
      observed: "limits.ts says 320, docs/limits.md says 32; 1 fail",
      session: "rig-tester",
      created_at: `2026-11-23T12:00:0${second}.000Z`,
      ...fields,
    });
    const retracted = {
      reason: "recorded at the wrong head",
      actor: { kind: "session", id: "rig-tester" },
      created_at: "2026-11-23T12:00:05.000Z",
    };
    dispatch.results.push(
      result(1, { retracted }),
      result(2, { check_words_moved: true }),
      result(3, { check_words_moved: false })
    );
    await seed.captureResults(file, dir);
    const captured = (second: number, fields: object) => ({
      session: "rig-tester",
      commit: "1".repeat(40),
      verdict: "fail",
      run: "bun test limits.test.ts",
      observed: "limits.ts says 320, docs/limits.md says 32; 1 fail",
      created_at: `2026-11-23T12:00:0${second}.000Z`,
      ...fields,
    });
    expect(JSON.parse(readFileSync(path.join(dir, "results.json"), "utf8"))).toEqual([
      {
        check: "c1",
        run: "bun test limits.test.ts",
        results: [
          captured(3, { check_words_moved: false }),
          captured(2, { check_words_moved: true }),
          captured(1, { retracted }),
        ],
      },
    ]);
  });
});
