import { afterAll, beforeAll, describe, expect, test } from "bun:test";
import { chmod, mkdtemp, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";
import { environmentOf, probeProblems, proveSandbox, type Sandbox, writeAgent } from "../agent";

const STACK = { url: "http://127.0.0.1:40000", token: "dispatch-stack-token" };

describe("environmentOf", () => {
  test("gives the agent its own TMPDIR and PATH, and takes only the two gateway variables from the caller", () => {
    const environment = environmentOf(STACK, "session-1", {
      PATH: "/home/someone/tools/bin",
      TMPDIR: "/srv/scratch",
      HOME: "/home/someone",
      GH_TOKEN: "a-token",
      ANTHROPIC_BASE_URL: "https://gateway.example",
      ANTHROPIC_API_KEY: "a-key",
    });
    expect(environment).toEqual({
      HOME: "/review/home",
      PATH: "/opt/bin:/usr/local/bin:/usr/bin:/bin",
      TMPDIR: "/tmp",
      DISPATCH_URL: STACK.url,
      DISPATCH_TOKEN: STACK.token,
      DISPATCH_HOST: "omp",
      DISPATCH_SESSION_ID: "session-1",
      DISPATCH_STATE_DIR: "/review/state",
      ANTHROPIC_BASE_URL: "https://gateway.example",
      ANTHROPIC_API_KEY: "a-key",
    });
  });
});

describe("probeProblems", () => {
  const own = ["/review/SKILL.md", "/opt/bin/dispatch"];
  const hidden = ["/srv/legion", "/srv/scratch/spec-review-eval-1"];

  test("passes a probe that sees each own path, no hidden one, and runs omp", () => {
    expect(
      probeProblems(
        "seen /review/SKILL.md\nseen /opt/bin/dispatch\nhidden /srv/legion\nhidden /srv/scratch/spec-review-eval-1\nomp runs\n",
        own,
        hidden
      )
    ).toEqual([]);
  });

  test("fails a hidden path it sees, an own path it does not, and an omp that does not run", () => {
    expect(
      probeProblems(
        "seen /review/SKILL.md\nhidden /opt/bin/dispatch\nseen /srv/legion\nhidden /srv/scratch/spec-review-eval-1\n",
        own,
        hidden
      )
    ).toEqual([
      "the agent's sandbox does not show it /opt/bin/dispatch",
      "the agent's sandbox shows it /srv/legion, which holds the answers",
      "omp does not run in the agent's sandbox",
    ]);
  });

  // A path with no line saw nothing, so it proves nothing hidden.
  test("fails a hidden path that the probe gave no line for", () => {
    expect(
      probeProblems("seen /review/SKILL.md\nseen /opt/bin/dispatch\nomp runs\n", own, hidden)
    ).toEqual([
      "the probe gave no line for /srv/legion, so nothing shows that it is hidden",
      "the probe gave no line for /srv/scratch/spec-review-eval-1, so nothing shows that it is hidden",
    ]);
  });
});

// The sandbox itself, where bubblewrap can start one on this machine.
const bwrap = Bun.which("bwrap");
const bwrapRuns =
  bwrap !== null &&
  Bun.spawnSync({ cmd: [bwrap, "--ro-bind", "/", "/", "true"], stderr: "ignore" }).exitCode === 0;

describe.skipIf(!bwrapRuns)("proveSandbox", () => {
  const EXPECTED = resolve(import.meta.dir, "..", "expected.json");
  let scratch = "";
  let sandbox: Sandbox;
  let own = "";
  let earlier = "";

  beforeAll(async () => {
    scratch = await mkdtemp(join(tmpdir(), "spec-review-eval-sandbox-"));
    const omp = join(scratch, "omp");
    await writeFile(omp, "#!/bin/sh\nexit 0\n", "utf8");
    await chmod(omp, 0o755);
    const dispatch = join(scratch, "dispatch");
    await writeFile(dispatch, "#!/bin/sh\nexit 0\n", "utf8");
    sandbox = { bwrap: bwrap ?? "bwrap", omp, dispatch };
    own = join(scratch, "agent");
    await writeAgent(
      own,
      join(import.meta.dir, "..", "..", "..", "skills", "spec-statement-review", "SKILL.md")
    );
    // An earlier run's transcript in the caller's TMPDIR, where a default --out goes.
    earlier = join(scratch, "spec-review-eval-earlier", "run-1", "1-principle", "events.jsonl");
    await Bun.write(earlier, "{}\n");
  });
  afterAll(async () => {
    await rm(scratch, { recursive: true, force: true });
  });

  test("hides an earlier run's transcript in the caller's TMPDIR and the checkout's expected.json", async () => {
    await proveSandbox(sandbox, own, [earlier, EXPECTED]);
  });

  test("refuses the run where the sandbox shows a path that must be hidden", async () => {
    await expect(proveSandbox(sandbox, own, [earlier, "/usr/bin/env"])).rejects.toThrow(
      "the agent's sandbox shows it /usr/bin/env, which holds the answers"
    );
  });
});
