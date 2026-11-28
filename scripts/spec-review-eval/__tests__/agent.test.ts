import { afterAll, beforeAll, describe, expect, test } from "bun:test";
import { chmod, mkdtemp, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";
import { MODEL } from "../../approval-eval/fresh-agent";
import {
  environmentOf,
  modelProblem,
  probeProblems,
  proveSandbox,
  type Sandbox,
  sandboxArgs,
  writeAgent,
} from "../agent";

const STACK = { url: "http://127.0.0.1:40000", token: "dispatch-stack-token" };

describe("environmentOf", () => {
  test("gives the agent its own TMPDIR and PATH, and takes only the two model variables from the caller", () => {
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

  test("takes no ANTHROPIC_BASE_URL that the caller leaves unset or empty", () => {
    for (const caller of [
      { ANTHROPIC_API_KEY: "a-key" },
      { ANTHROPIC_API_KEY: "a-key", ANTHROPIC_BASE_URL: "" },
    ]) {
      const environment = environmentOf(STACK, "session-1", caller);
      expect(environment.ANTHROPIC_API_KEY).toBe("a-key");
      expect("ANTHROPIC_BASE_URL" in environment).toBe(false);
    }
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

  test("fails a hidden path that the probe gave no line for", () => {
    expect(
      probeProblems("seen /review/SKILL.md\nseen /opt/bin/dispatch\nomp runs\n", own, hidden)
    ).toEqual([
      "the probe gave no line for /srv/legion, so nothing shows that it is hidden",
      "the probe gave no line for /srv/scratch/spec-review-eval-1, so nothing shows that it is hidden",
    ]);
  });
});

describe("modelProblem", () => {
  test("passes a list that holds the model by its id or by its selector", () => {
    expect(
      modelProblem(`{"models":[{"id":"${MODEL}","selector":"anthropic/${MODEL}"}]}`, MODEL)
    ).toBe(null);
    expect(
      modelProblem(
        `{"models":[{"id":"${MODEL}","selector":"anthropic/${MODEL}"}]}`,
        `anthropic/${MODEL}`
      )
    ).toBe(null);
  });

  test("fails a list without the model, as omp prints with no key, and names the key", () => {
    expect(modelProblem('{"models":[]}', MODEL)).toBe(
      `omp in the agent's sandbox lists no model ${MODEL}: the agent gets no models.yml, so omp there lists only Anthropic's models, and those only with ANTHROPIC_API_KEY`
    );
  });

  test("fails an output that is no list of models", () => {
    expect(modelProblem("No models available.\n", MODEL)).toBe(
      `omp in the agent's sandbox printed no list of models for ${MODEL}: No models available.`
    );
  });
});

// The sandbox itself, where bubblewrap can start one on this machine. Under GITHUB_ACTIONS these
// rows run on every machine: the test job installs bubblewrap (ci-job-test.yaml), so there a
// sandbox that cannot start fails them rather than skipping them.
const bwrap = Bun.which("bwrap");
const bwrapRuns =
  bwrap !== null &&
  Bun.spawnSync({ cmd: [bwrap, "--ro-bind", "/", "/", "true"], stderr: "ignore" }).exitCode === 0;

describe.skipIf(!bwrapRuns && process.env.GITHUB_ACTIONS !== "true")("proveSandbox", () => {
  const CALLER = { ANTHROPIC_BASE_URL: "https://gateway.example", ANTHROPIC_API_KEY: "a-key" };
  const EXPECTED = resolve(import.meta.dir, "..", "expected.json");
  let scratch = "";
  let sandbox: Sandbox;
  let own = "";
  let earlier = "";

  beforeAll(async () => {
    scratch = await mkdtemp(join(tmpdir(), "spec-review-eval-sandbox-"));
    const omp = join(scratch, "omp");
    // A stand-in omp that lists the model, as omp does, only where it has the key.
    await writeFile(
      omp,
      [
        "#!/bin/sh",
        'if [ "$3 $4" = "models find" ]; then',
        `  if [ -n "$ANTHROPIC_API_KEY" ] && [ "$5" = "${MODEL}" ]; then echo '{"models":[{"id":"${MODEL}"}]}'; else echo '{"models":[]}'; fi`,
        "fi",
        "exit 0",
        "",
      ].join("\n"),
      "utf8"
    );
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
    await proveSandbox(
      sandbox,
      own,
      [earlier, EXPECTED],
      MODEL,
      environmentOf(STACK, "session-1", CALLER)
    );
  });

  test("refuses the run where the sandbox shows a path that must be hidden", async () => {
    await expect(
      proveSandbox(
        sandbox,
        own,
        [earlier, "/usr/bin/env"],
        MODEL,
        environmentOf(STACK, "session-1", CALLER)
      )
    ).rejects.toThrow("the agent's sandbox shows it /usr/bin/env, which holds the answers");
  });

  test("passes with the key alone, as omp lists Anthropic's models with no gateway", async () => {
    await proveSandbox(
      sandbox,
      own,
      [earlier, EXPECTED],
      MODEL,
      environmentOf(STACK, "session-1", { ANTHROPIC_API_KEY: CALLER.ANTHROPIC_API_KEY })
    );
  });

  test("refuses the run where omp in the sandbox, with the agent's environment, lists no model", async () => {
    await expect(
      proveSandbox(
        sandbox,
        own,
        [earlier, EXPECTED],
        MODEL,
        environmentOf(STACK, "session-1", { ANTHROPIC_BASE_URL: CALLER.ANTHROPIC_BASE_URL })
      )
    ).rejects.toThrow(`omp in the agent's sandbox lists no model ${MODEL}`);
  });

  // The kernel gives a session whose leader is outside the sandbox's process namespace, as the
  // caller's is, the id 0. In a session of its own, the sandbox's first process, id 1, leads it.
  test("starts the agent in a session of its own, which holds no terminal of the caller's", () => {
    const child = Bun.spawnSync({
      cmd: [
        sandbox.bwrap,
        ...sandboxArgs(sandbox, own),
        "/bin/sh",
        "-c",
        'read -r stat < /proc/$$/stat; echo "$stat"',
      ],
      stderr: "pipe",
    });
    const session = child.stdout.toString().trim().split(" ")[5];
    expect(child.exitCode).toBe(0);
    expect(session).toBe("1");
  });
});
