import { afterEach, beforeAll, beforeEach, describe, expect, spyOn, test } from "bun:test";
import { readFileSync } from "node:fs";
import path from "node:path";
import { restoreEnvironment } from "@legion/test-fixtures/environment";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import { RERUNS_CASES as SCORED_CASES } from "./score";
import type * as seedModule from "./seed";
import type * as worldModule from "./tester-reruns-world";

// e2e/api.ts, which the world and seed.ts call, reads its port when it loads: set it as rig.sh's
// seed_ts does, then load both, in the `beforeAll` below, as seed.test.ts does and for its reason.
// The port is seed.test.ts's own: `bun test` loads e2e/api.ts once for every file it runs, so the
// file that loads it first fixes the port for both.
restoreEnvironment(["DISPATCH_E2E_PORT", "PLAYWRIGHT_BASE_URL"]);
let world: typeof worldModule;
let seed: typeof seedModule;
const origin = "http://127.0.0.1:18777";
const work = temporaryDirectory("skill-scenarios-tester-reruns");
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

interface FakeResult {
  check: string;
  role: string;
  verdict: string;
  commit_sha: string;
  run: string;
  observed: string;
  session: string;
  created_at: string;
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
  test("the world's cases are the three this file reads, as score.ts and rig.sh name them", () => {
    expect(Object.keys(CASES)).toEqual([...world.RERUNS_CASES]);
    expect([...SCORED_CASES]).toEqual([...world.RERUNS_CASES]);
    const rig = readFileSync(path.join(import.meta.dir, "rig.sh"), "utf8");
    expect(/^reruns_cases=\(([^)]*)\)$/m.exec(rig)?.[1]?.split(" ")).toEqual([
      ...world.RERUNS_CASES,
    ]);
  });

  for (const [rerunsCase, expected] of Object.entries(CASES)) {
    const name = rerunsCase as keyof typeof CASES;
    test(
      `tester-reruns ${name}: c1 passes at H0, and at H1 ${expected.passes ? "passes" : "fails"} with the output only H1 prints`,
      () => {
        const dir = freshDir(name);
        const { base, h0, h1, filesChanged } = world.rerunsRepository(
          dir,
          name,
          "ACME-1",
          1_800_000_000
        );
        expect(git(dir, "rev-parse", `${h1}~1`)).toBe(h0);
        expect(git(dir, "rev-parse", `${h0}~1`)).toBe(base);
        expect(git(dir, "diff", "--name-only", h0, h1)).toBe(expected.file);
        expect(filesChanged).toEqual(git(dir, "diff", "--name-only", base, h1).split("\n"));
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
        expect(JSON.parse(handoff?.body ?? "{}").filesChanged).toEqual(fixture.filesChanged);
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
});
