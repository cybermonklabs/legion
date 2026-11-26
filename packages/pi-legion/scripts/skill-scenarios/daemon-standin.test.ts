// daemon-standin.ts as the rig runs it: a real process on a port of its own, with a stub Dispatch
// and a planted label checkout whose `label-texts.sh` is a stub of the entry. Each row starts its
// own stand-in and reads what it answered and what it wrote to its log.
import { expect, setDefaultTimeout, test } from "bun:test";
import { randomUUID } from "node:crypto";
import { existsSync, mkdirSync, readFileSync, writeFileSync } from "node:fs";
import path from "node:path";
import { roleForPhase } from "@legion/contracts";
import { daemonContract } from "@legion/contracts/daemon-contract";
import {
  LegionErrorResponse,
  LegionGrantResponse,
  LegionIssueRecordResponse,
  LegionRegisterResponse,
  type LegionRegistration,
  LegionStateResponse,
} from "@legion/contracts/legion-api";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { skipWithout } from "@legion/test-fixtures/host-tools";
import { loopbackHost } from "@legion/test-fixtures/loopback-host";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import type { Server, Subprocess } from "bun";
import pkg from "../../package.json";
import { PlanRequest, StandinLogLine } from "./scored-runs-contracts";

// Each row starts a Bun process, and the rows of a phase worker ask daemon-pane.go for the role's
// phase, which `go run` compiles: those get a bound of their own, as daemon-pane.test.ts's do.
setDefaultTimeout(60_000);
const GO_BOUND = 600_000;
const skipGo = skipWithout("go");

const STANDIN = path.join(import.meta.dir, "daemon-standin.ts");
const freshDir = temporaryDirectories("daemon-standin");
/** The contract this checkout's plugin speaks, as the stand-in computes it without `--label`. */
const OWN_CONTRACT = daemonContract(pkg, import.meta.dir);

/** A Legion project token: lower-case letters and digits (`isLegionProjectToken`). */
const PROJECT = "skillsstandin";
/** The repository and default branch the stand-in's state puts every issue on: its seventh and
 *  eighth words. */
const REPOSITORY = "example/widgets";
const DEFAULT_BRANCH = "trunk";
const REPO = [REPOSITORY, DEFAULT_BRANCH] as const;

interface Standin {
  readonly url: string;
  readonly log: string;
  readonly bootToken: string;
  readonly process: Subprocess;
}

/** Waits for the stand-in's `listening on` line and answers its URL, or throws with its stderr
 *  when it exits first. */
async function listening(child: Subprocess<"ignore", "pipe", "pipe">): Promise<string> {
  const reader = child.stdout.getReader();
  const decoder = new TextDecoder();
  let text = "";
  for (;;) {
    const { value, done } = await reader.read();
    if (done) {
      const code = await child.exited;
      const stderr = await new Response(child.stderr).text();
      throw new Error(`the stand-in exited ${code} before it listened: ${stderr}`);
    }
    text += decoder.decode(value, { stream: true });
    const match = /^listening on (http:\/\/127\.0\.0\.1:\d+)$/m.exec(text);
    if (match?.[1] !== undefined) {
      reader.releaseLock();
      return match[1];
    }
  }
}

/** Starts daemon-standin.ts on a port the kernel picks, with a boot token file of its own, and
 *  runs `body` with it; the process is stopped when `body` ends. */
async function withStandin(
  dir: string,
  args: readonly string[],
  body: (standin: Standin) => Promise<void>
): Promise<void> {
  const log = path.join(dir, "daemon.log");
  const bootFile = path.join(dir, "boot");
  const bootToken = fixtureSecret("boot");
  writeFileSync(bootFile, `${bootToken}\n`, { mode: 0o600 });
  const child = Bun.spawn([process.execPath, STANDIN, "0", log, bootFile, ...args], {
    stdin: "ignore",
    stdout: "pipe",
    stderr: "pipe",
  });
  try {
    const url = await listening(child);
    await body({ url, log, bootToken, process: child });
  } finally {
    child.kill();
    await child.exited;
  }
}

/** The stand-in's exit and stderr for a command line it must refuse before it listens. */
function refused(dir: string, args: readonly string[]): { code: number; stderr: string } {
  const bootFile = path.join(dir, "boot");
  writeFileSync(bootFile, `${fixtureSecret("boot")}\n`, { mode: 0o600 });
  const result = Bun.spawnSync(
    [process.execPath, STANDIN, "0", path.join(dir, "daemon.log"), bootFile, ...args],
    { stdin: "ignore", stdout: "pipe", stderr: "pipe", timeout: 50_000 }
  );
  return { code: result.exitCode ?? -1, stderr: result.stderr.toString() };
}

function post(standin: Standin, route: string, body: unknown): Promise<Response> {
  return fetch(`${standin.url}${route}`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  });
}

/** Every line of the stand-in's log, each held to C3's shape. */
function logLines(standin: Standin): StandinLogLine[] {
  return readFileSync(standin.log, "utf8")
    .trim()
    .split("\n")
    .map((line) => StandinLogLine.parse(JSON.parse(line)));
}

/** The one log line of `route`, which the row made one request to. */
function lineOf(standin: Standin, route: string): StandinLogLine {
  const [line, ...others] = logLines(standin).filter((each) => each.path === route);
  expect(others).toHaveLength(0);
  if (line === undefined) throw new Error(`the stand-in's log holds no line of ${route}`);
  return line;
}

/** The claim the stand-in registers for the boot token, as the plugin's boot registers it. */
async function register(standin: Standin): Promise<LegionRegistration> {
  const response = await post(standin, "/legion/v1/claims/register", {
    bootToken: standin.bootToken,
  });
  expect(response.status).toBe(200);
  return LegionRegisterResponse.parse(await response.json());
}

const SESSION = "rig-session-1";

/** A grant of the claim, as the `legion` tool mints one before each daemon call. */
async function grant(standin: Standin, claim: LegionRegistration): Promise<string> {
  const response = await post(standin, "/legion/v1/grants", {
    sessionId: SESSION,
    secret: claim.secret,
    tree: claim.tree,
    issue: claim.issue,
  });
  expect(response.status).toBe(200);
  return LegionGrantResponse.parse(await response.json()).grantId;
}

/** A label checkout with the parts the stand-in reads: its plugin's `package.json`, which states
 *  its contract as a number, or, given `released` and `entries`, states none and leaves it to the
 *  checkout's frozen list and its count of entry fixtures (one file beside them is no entry); and a
 *  stub of the entry at the path the rig calls it by. The stub takes exactly
 *  the entry's interface for `plan-check` (exit 2 for a usage error), keeps the request it was
 *  given as `request-seen.json` beside itself, then: with `planted-no-answer` beside it, prints
 *  its PASS line and exits 0 with no answer file; with `planted-answer.json`, answers that; and
 *  otherwise fails as a run whose test did not pass: lines on stderr, the last of them
 *  `planted-failure.txt`'s, and exit 1. */
function plantLabel(
  dir: string,
  contract: number | { readonly released: readonly number[]; readonly entries: number }
): { label: string; entryDir: string } {
  const label = path.join(dir, "label");
  const plugin = path.join(label, "packages", "pi-legion");
  const entryDir = path.join(plugin, "scripts", "skill-scenarios");
  mkdirSync(entryDir, { recursive: true });
  if (typeof contract === "number") {
    writeFileSync(
      path.join(plugin, "package.json"),
      JSON.stringify({ name: "@sjawhar/pi-legion", legion: { daemonApiVersion: contract } })
    );
  } else {
    writeFileSync(
      path.join(plugin, "package.json"),
      JSON.stringify({ name: "@sjawhar/pi-legion" })
    );
    const fixtures = path.join(label, "packages", "contracts", "fixtures", "daemon-api");
    mkdirSync(path.join(fixtures, "contracts"), { recursive: true });
    writeFileSync(
      path.join(fixtures, "released-contracts.json"),
      JSON.stringify(contract.released)
    );
    for (let entry = 0; entry < contract.entries; entry++) {
      writeFileSync(
        path.join(fixtures, "contracts", `2026112500000${entry}_entry-${entry}.json`),
        "{}"
      );
    }
    writeFileSync(path.join(fixtures, "contracts", "README.md"), "no entry\n");
  }
  writeFileSync(
    path.join(entryDir, "label-texts.sh"),
    [
      "#!/usr/bin/env bash",
      "set -euo pipefail",
      'if [ "$#" -ne 3 ] || [ "$1" != plan-check ]; then',
      '  echo "usage: label-texts.sh plan-check <request> <answer>" >&2',
      "  exit 2",
      "fi",
      'here=$(cd "$(dirname "$0")" && pwd)',
      'cp "$2" "$here/request-seen.json"',
      'if [ -f "$here/planted-no-answer" ]; then',
      '  echo "--- PASS: TestRigPlanCheck"',
      "  exit 0",
      "fi",
      'if [ -f "$here/planted-answer.json" ]; then',
      '  cp "$here/planted-answer.json" "$3"',
      '  echo "--- PASS: TestRigPlanCheck"',
      "  exit 0",
      "fi",
      'echo "=== RUN   TestRigPlanCheck" >&2',
      'echo "--- FAIL: TestRigPlanCheck" >&2',
      'cat "$here/planted-failure.txt" >&2',
      "exit 1",
      "",
    ].join("\n"),
    { mode: 0o755 }
  );
  return { label, entryDir };
}

interface SeenRequest {
  readonly method: string;
  readonly path: string;
  readonly query: string;
  readonly authorization: string | null;
}

/** A stub of the run's Dispatch that holds one plan document `artifactId` of `issue` (null: a
 *  document on no issue), at latest version `latest`, with `version`'s markdown and blocks as
 *  Dispatch's version read answers them
 *  (`GET /api/v1/artifacts/{id}/versions/{n}?blocks=true`). With `documentStatus`, the document
 *  read is refused with that status; with `versionAnswer`, the version read answers what it
 *  returns at the time of the read. It records each request. */
function stubDispatch(document: {
  readonly artifactId: string;
  readonly issue: string | null;
  readonly latest: number;
  readonly version: number;
  readonly markdown: string;
  readonly blocks: readonly Record<string, unknown>[];
  readonly documentStatus?: number;
  readonly versionAnswer?: () => Record<string, unknown>;
}): { readonly server: Server<undefined>; readonly url: string; readonly seen: SeenRequest[] } {
  const seen: SeenRequest[] = [];
  const server = Bun.serve({
    hostname: loopbackHost(),
    port: 0,
    fetch(request) {
      const url = new URL(request.url);
      seen.push({
        method: request.method,
        path: url.pathname,
        query: url.search.replace(/^\?/, ""),
        authorization: request.headers.get("authorization"),
      });
      const artifact = `/api/v1/artifacts/${document.artifactId}`;
      if (request.method === "GET" && url.pathname === artifact && url.search === "") {
        if (document.documentStatus !== undefined) {
          return Response.json(
            { code: "UNAVAILABLE", error: "planted refusal of the document read" },
            { status: document.documentStatus }
          );
        }
        return Response.json({
          id: document.artifactId,
          issue_key: document.issue,
          slug: "plan-md",
          approval: { state: "draft", latest_version: document.latest },
        });
      }
      if (
        request.method === "GET" &&
        url.pathname === `${artifact}/versions/${document.version}` &&
        url.search === "?blocks=true"
      ) {
        return Response.json(
          document.versionAnswer?.() ?? {
            markdown: document.markdown,
            number: document.version,
            named: false,
            blocks: document.blocks,
          }
        );
      }
      return Response.json(
        { code: "NOT_FOUND", error: `no route for ${request.method} ${url.pathname}` },
        { status: 404 }
      );
    },
  });
  return { server, url: server.url.origin, seen };
}

/** `--issues` and the file of a world whose plan document's issue, LWEVAL-8, is a backlog issue of
 *  the tree of LWEVAL-7, the claim of the register_plan rows. */
function plannedIssue(dir: string): string[] {
  const file = path.join(dir, "issues.json");
  writeFileSync(
    file,
    JSON.stringify([{ key: "LWEVAL-8", tree: "LWEVAL-7", phase: null, status: "backlog" }])
  );
  return ["--issues", file];
}

// Catches: a register_plan that answers on its own (a 200, or a body of its own), that reads the
// plan from anywhere but the run's Dispatch, that hands the entry a request in another shape than
// C2's, or that cuts a block's text by character where Dispatch's range counts bytes.
test("register_plan reads the version from Dispatch and answers what the label's check answers", async () => {
  const dir = freshDir();
  const { label, entryDir } = plantLabel(dir, OWN_CONTRACT);
  const marker = randomUUID();
  const planted = {
    status: 409,
    body: JSON.stringify({ code: "PLAN_REFUSED", error: `planted refusal ${marker}` }),
  };
  writeFileSync(path.join(entryDir, "planted-answer.json"), JSON.stringify(planted));

  const artifactId = randomUUID();
  const taskText = `Do the planted work ${marker}.\n\nThen check it.`;
  // The intro holds characters of two and three bytes, so a range read by character is off.
  const markdown = `Intro — é.\n\n:::plan_task{#rig-task steps="implementer reviewer"}\n${taskText}\n:::\n`;
  const from = Buffer.byteLength(markdown.slice(0, markdown.indexOf(":::plan_task")));
  const to = Buffer.byteLength(markdown.slice(0, markdown.lastIndexOf(":::") + 3));
  const dispatch = stubDispatch({
    artifactId,
    issue: "LWEVAL-8",
    latest: 3,
    version: 2,
    markdown,
    blocks: [
      {
        id: "rig-task",
        type: "plan_task",
        from,
        to,
        token: "sha256:a",
        attributes: [{ name: "steps", value: "implementer reviewer", kind: "string" }],
      },
    ],
  });
  const dispatchToken = fixtureSecret("dsp");
  const tokenFile = path.join(dir, "dispatch-token");
  writeFileSync(tokenFile, `${dispatchToken}\n`, { mode: 0o600 });
  try {
    await withStandin(
      dir,
      [
        PROJECT,
        "LWEVAL-7",
        "architect",
        ...REPO,
        "admitted",
        ...plannedIssue(dir),
        "--label",
        label,
        "--dispatch",
        dispatch.url,
        tokenFile,
      ],
      async (standin) => {
        const claim = await register(standin);
        const grantId = await grant(standin, claim);
        const body = { grantId, issue: "LWEVAL-8", artifactId, version: 2 };
        const response = await post(standin, "/legion/v1/plans/register", body);

        expect(response.status).toBe(planted.status);
        expect(await response.text()).toBe(planted.body);
        expect(
          PlanRequest.parse(
            JSON.parse(readFileSync(path.join(entryDir, "request-seen.json"), "utf8"))
          )
        ).toEqual({
          artifactId,
          version: 2,
          latestVersion: 3,
          blocks: [
            {
              id: "rig-task",
              type: "plan_task",
              attributes: { steps: "implementer reviewer" },
              text: taskText,
            },
          ],
        });
        expect(dispatch.seen.map(({ path: route, query }) => ({ route, query }))).toEqual([
          { route: `/api/v1/artifacts/${artifactId}`, query: "" },
          { route: `/api/v1/artifacts/${artifactId}/versions/2`, query: "blocks=true" },
        ]);
        for (const request of dispatch.seen) {
          expect(request.authorization).toBe(`Bearer ${dispatchToken}`);
        }
        expect(lineOf(standin, "/legion/v1/plans/register")).toMatchObject({
          status: planted.status,
          grantId,
          body,
        });
      }
    );
  } finally {
    dispatch.server.stop(true);
  }
});

// Catches: a stand-in that answers a failed check with a status of its own choosing or a 200, that
// names some other line of the entry's stderr, or that answers 200 when it was given no label or
// no Dispatch to ask.
test("register_plan answers 500 naming the entry's exit and its stderr's last line, and never 200 on its own", async () => {
  const dir = freshDir();
  const { label, entryDir } = plantLabel(dir, OWN_CONTRACT);
  const lastLine = `no --- PASS line for TestRigPlanCheck ${randomUUID()}`;
  writeFileSync(path.join(entryDir, "planted-failure.txt"), `${lastLine}\n`);
  const artifactId = randomUUID();
  const markdown = ":::plan_task{#a}\nThe task.\n:::\n";
  const dispatch = stubDispatch({
    artifactId,
    issue: "LWEVAL-8",
    latest: 1,
    version: 1,
    markdown,
    blocks: [{ id: "a", type: "plan_task", from: 0, to: markdown.length - 1, attributes: [] }],
  });
  const tokenFile = path.join(dir, "dispatch-token");
  writeFileSync(tokenFile, `${fixtureSecret("dsp")}\n`, { mode: 0o600 });
  try {
    const flags = [...plannedIssue(dir), "--label", label, "--dispatch", dispatch.url, tokenFile];
    await withStandin(
      dir,
      [PROJECT, "LWEVAL-7", "architect", ...REPO, "admitted", ...flags],
      async (standin) => {
        const claim = await register(standin);
        const body = {
          grantId: await grant(standin, claim),
          issue: "LWEVAL-8",
          artifactId,
          version: 1,
        };
        const response = await post(standin, "/legion/v1/plans/register", body);
        expect(response.status).toBe(500);
        const { error } = LegionErrorResponse.parse(await response.json());
        expect(error).toContain("exited 1");
        expect(error).toContain(lastLine);
        expect(error).not.toContain("--- FAIL: TestRigPlanCheck");
        expect(lineOf(standin, "/legion/v1/plans/register")).toMatchObject({ status: 500, body });
      }
    );

    // A stand-in given no label and no Dispatch cannot ask the label, so it says so.
    const bare = freshDir();
    await withStandin(
      bare,
      [PROJECT, "LWEVAL-7", "architect", ...REPO, "admitted", ...plannedIssue(bare)],
      async (standin) => {
        const claim = await register(standin);
        const body = {
          grantId: await grant(standin, claim),
          issue: "LWEVAL-8",
          artifactId,
          version: 1,
        };
        const response = await post(standin, "/legion/v1/plans/register", body);
        expect(response.status).toBe(500);
        const { error } = LegionErrorResponse.parse(await response.json());
        expect(error).toContain("--label");
        expect(error).toContain("--dispatch");
      }
    );
  } finally {
    dispatch.server.stop(true);
  }
});

/** A stand-in with a planted label and a stub Dispatch of one document of LWEVAL-8 (or of the
 *  issue `dispatchOptions` names) at latest version 3, whose version 2 holds one plan task. `body`
 *  gets a call of register_plan for LWEVAL-8 at one version of it, the stand-in, the requests
 *  Dispatch saw, the entry's directory and the document's id. */
async function withPlanCheck(
  dispatchOptions: {
    readonly issue?: string | null;
    readonly documentStatus?: number;
    readonly versionAnswer?: () => Record<string, unknown>;
  },
  body: (context: {
    readonly registerPlan: (version: number) => Promise<Response>;
    readonly standin: Standin;
    readonly seen: readonly SeenRequest[];
    readonly entryDir: string;
    readonly artifactId: string;
  }) => Promise<void>
): Promise<void> {
  const dir = freshDir();
  const { label, entryDir } = plantLabel(dir, OWN_CONTRACT);
  const artifactId = randomUUID();
  const markdown = ":::plan_task{#a}\nThe task.\n:::\n";
  const dispatch = stubDispatch({
    artifactId,
    issue: "LWEVAL-8",
    latest: 3,
    version: 2,
    markdown,
    blocks: [{ id: "a", type: "plan_task", from: 0, to: markdown.length - 1, attributes: [] }],
    ...dispatchOptions,
  });
  const tokenFile = path.join(dir, "dispatch-token");
  writeFileSync(tokenFile, `${fixtureSecret("dsp")}\n`, { mode: 0o600 });
  const flags = [...plannedIssue(dir), "--label", label, "--dispatch", dispatch.url, tokenFile];
  try {
    await withStandin(
      dir,
      [PROJECT, "LWEVAL-7", "architect", ...REPO, "admitted", ...flags],
      async (standin) => {
        const claim = await register(standin);
        const registerPlan = async (version: number): Promise<Response> =>
          post(standin, "/legion/v1/plans/register", {
            grantId: await grant(standin, claim),
            issue: "LWEVAL-8",
            artifactId,
            version,
          });
        await body({ registerPlan, standin, seen: dispatch.seen, entryDir, artifactId });
      }
    );
  } finally {
    dispatch.server.stop(true);
  }
}

/** The statuses of the stand-in's log lines of register_plan, in order. */
function planStatuses(standin: Standin): number[] {
  return logLines(standin)
    .filter((line) => line.path === "/legion/v1/plans/register")
    .map((line) => line.status);
}

/** An answer the stub entry gives when it runs, so a row sees a check that should not have run. */
const RAN_ANYWAY = JSON.stringify({ status: 200, body: "{}" });

// Catches: a stand-in that answers a failed read of the run's Dispatch with a status of its own
// choosing, or that runs the label's check without the plan it could not read.
test("register_plan answers 502 when the run's Dispatch fails its read, and runs no entry", async () => {
  await withPlanCheck(
    { documentStatus: 503 },
    async ({ registerPlan, standin, entryDir, artifactId }) => {
      writeFileSync(path.join(entryDir, "planted-answer.json"), RAN_ANYWAY);
      const response = await registerPlan(2);
      expect(response.status).toBe(502);
      const refusal = LegionErrorResponse.parse(await response.json());
      expect(refusal.code).toBe("DISPATCH_FAILED");
      expect(refusal.error).toContain(`/api/v1/artifacts/${artifactId}`);
      expect(refusal.error).toContain("503");
      expect(existsSync(path.join(entryDir, "request-seen.json"))).toBe(false);
      expect(planStatuses(standin)).toEqual([502]);
    }
  );
});

// Catches: a register_plan that reads a document on no issue (Dispatch's `issue_key` null) as a
// failed read and answers 502 DISPATCH_FAILED, which tells the agent to try again, where the
// label's route answers 404 ARTIFACT_NOT_ON_ISSUE (architect.go).
test("register_plan refuses a document on no issue 404 ARTIFACT_NOT_ON_ISSUE, and runs no entry", async () => {
  await withPlanCheck({ issue: null }, async ({ registerPlan, standin, entryDir, artifactId }) => {
    writeFileSync(path.join(entryDir, "planted-answer.json"), RAN_ANYWAY);
    const response = await registerPlan(2);
    expect(response.status).toBe(404);
    expect(LegionErrorResponse.parse(await response.json())).toMatchObject({
      code: "ARTIFACT_NOT_ON_ISSUE",
      error: `${artifactId} is not a document of LWEVAL-8`,
    });
    expect(existsSync(path.join(entryDir, "request-seen.json"))).toBe(false);
    expect(planStatuses(standin)).toEqual([404]);
  });
});

// Catches: a register_plan that takes another issue's document, or a plan for an issue outside the
// claim's tree, both of which the label's route refuses before it reads a block (api/plan.go
// planOwner and planAt): the agent would read a 200 where the daemon answers it a refusal.
test("register_plan refuses another issue's document 404 ARTIFACT_NOT_ON_ISSUE, and an issue outside the claim's tree 403 ISSUE_OUTSIDE_TREE, and runs no entry for either", async () => {
  const dir = freshDir();
  const { label, entryDir } = plantLabel(dir, OWN_CONTRACT);
  writeFileSync(path.join(entryDir, "planted-answer.json"), RAN_ANYWAY);
  const artifactId = randomUUID();
  const markdown = ":::plan_task{#a}\nThe task.\n:::\n";
  const dispatch = stubDispatch({
    artifactId,
    issue: "LWEVAL-9",
    latest: 1,
    version: 1,
    markdown,
    blocks: [{ id: "a", type: "plan_task", from: 0, to: markdown.length - 1, attributes: [] }],
  });
  const tokenFile = path.join(dir, "dispatch-token");
  writeFileSync(tokenFile, `${fixtureSecret("dsp")}\n`, { mode: 0o600 });
  const issuesFile = path.join(dir, "issues.json");
  writeFileSync(
    issuesFile,
    JSON.stringify([
      { key: "LWEVAL-8", tree: "LWEVAL-7", phase: null, status: "backlog" },
      { key: "LWEVAL-9", tree: "LWEVAL-7", phase: null, status: "backlog" },
      { key: "LWEVAL-30", tree: "LWEVAL-29", phase: null, status: "backlog" },
    ])
  );
  const flags = ["--issues", issuesFile, "--label", label, "--dispatch", dispatch.url, tokenFile];
  try {
    await withStandin(
      dir,
      [PROJECT, "LWEVAL-7", "architect", ...REPO, "admitted", ...flags],
      async (standin) => {
        const claim = await register(standin);
        const registerPlan = async (issue: string): Promise<Response> =>
          post(standin, "/legion/v1/plans/register", {
            grantId: await grant(standin, claim),
            issue,
            artifactId,
            version: 1,
          });
        const foreign = await registerPlan("LWEVAL-8");
        expect(foreign.status).toBe(404);
        expect(LegionErrorResponse.parse(await foreign.json())).toMatchObject({
          code: "ARTIFACT_NOT_ON_ISSUE",
          error: `${artifactId} is not a document of LWEVAL-8`,
        });
        // An issue of another tree, and one the world does not hold.
        for (const issue of ["LWEVAL-30", "LWEVAL-40"]) {
          const outside = await registerPlan(issue);
          expect(outside.status).toBe(403);
          expect(LegionErrorResponse.parse(await outside.json())).toMatchObject({
            code: "ISSUE_OUTSIDE_TREE",
            error: `${issue} is outside the architect tree LWEVAL-7`,
          });
        }
        expect(existsSync(path.join(entryDir, "request-seen.json"))).toBe(false);
        // The document's own issue, in the claim's tree, reaches the label's check.
        expect((await registerPlan("LWEVAL-9")).status).toBe(200);
        expect(existsSync(path.join(entryDir, "request-seen.json"))).toBe(true);
        expect(planStatuses(standin)).toEqual([404, 403, 403, 200]);
      }
    );
  } finally {
    dispatch.server.stop(true);
  }
});

// Catches: a stand-in that cuts blocks out of a version it cannot read (no blocks, another
// version than the one asked for, a range past the markdown, a range that holds no fenced block),
// or runs the label's check on what it cut.
test("register_plan answers 502 for a version whose blocks it cannot read, and runs no entry", async () => {
  const markdown = ":::plan_task{#a}\nThe task.\n:::\n";
  const block = { id: "a", type: "plan_task", from: 0, to: markdown.length - 1, attributes: [] };
  const unreadable: readonly (readonly [string, Record<string, unknown>])[] = [
    ["no blocks", { markdown, number: 2 }],
    ["another version", { markdown, number: 3, blocks: [block] }],
    ["a range past the markdown", { markdown, number: 2, blocks: [{ ...block, to: 99 }] }],
    ["no fenced block", { markdown: "one line", number: 2, blocks: [{ ...block, to: 8 }] }],
  ];
  let answer: Record<string, unknown> = {};
  await withPlanCheck(
    { versionAnswer: () => answer },
    async ({ registerPlan, standin, entryDir }) => {
      writeFileSync(path.join(entryDir, "planted-answer.json"), RAN_ANYWAY);
      for (const [why, planted] of unreadable) {
        answer = planted;
        const response = await registerPlan(2);
        const refusal = LegionErrorResponse.parse(await response.json());
        expect({ why, status: response.status, code: refusal.code }).toEqual({
          why,
          status: 502,
          code: "PLAN_UNREADABLE",
        });
      }
      expect(existsSync(path.join(entryDir, "request-seen.json"))).toBe(false);
      expect(planStatuses(standin)).toEqual([502, 502, 502, 502]);
    }
  );
});

// Catches: a stand-in that asks Dispatch for the blocks of a version above the latest, or keeps
// that version from the label's check, whose own route refuses it before it reads any block.
test("register_plan gives the label's check a version above the latest with no blocks, and reads no version from Dispatch", async () => {
  await withPlanCheck({}, async ({ registerPlan, seen, entryDir, artifactId }) => {
    const planted = {
      status: 409,
      body: JSON.stringify({ code: "VERSION_NOT_IN_DISPATCH", error: `planted ${randomUUID()}` }),
    };
    writeFileSync(path.join(entryDir, "planted-answer.json"), JSON.stringify(planted));
    const response = await registerPlan(4);
    expect(response.status).toBe(409);
    expect(await response.text()).toBe(planted.body);
    expect(
      PlanRequest.parse(JSON.parse(readFileSync(path.join(entryDir, "request-seen.json"), "utf8")))
    ).toEqual({ artifactId, version: 4, latestVersion: 3, blocks: [] });
    expect(seen.map((request) => request.path)).toEqual([`/api/v1/artifacts/${artifactId}`]);
  });
});

// Catches: a stand-in that takes an entry's exit 0 for an answer when the entry left none.
test("register_plan answers 500 when the entry exits 0 and leaves no answer", async () => {
  await withPlanCheck({}, async ({ registerPlan, standin, entryDir }) => {
    writeFileSync(path.join(entryDir, "planted-no-answer"), "");
    const response = await registerPlan(2);
    expect(response.status).toBe(500);
    const refusal = LegionErrorResponse.parse(await response.json());
    expect(refusal.code).toBe("PLAN_CHECK_FAILED");
    expect(refusal.error).toContain("exited 0");
    expect(existsSync(path.join(entryDir, "request-seen.json"))).toBe(true);
    expect(planStatuses(standin)).toEqual([500]);
  });
});

// Catches: a stand-in that hands the tool a status no response carries (one below 200, or a body
// on a status that has none), which the server then fails in place of an answer.
test("register_plan answers 500 for a label answer whose status no response carries", async () => {
  await withPlanCheck({}, async ({ registerPlan, standin, entryDir }) => {
    for (const status of [101, 204]) {
      writeFileSync(
        path.join(entryDir, "planted-answer.json"),
        JSON.stringify({ status, body: "{}" })
      );
      const response = await registerPlan(2);
      const refusal = LegionErrorResponse.parse(await response.json());
      expect({ status, answered: response.status, code: refusal.code }).toEqual({
        status,
        answered: 500,
        code: "PLAN_CHECK_FAILED",
      });
      expect(refusal.error).toContain(`status ${status}`);
    }
    expect(planStatuses(standin)).toEqual([500, 500]);
  });
});

// Catches: a sign-off or a move back answered 404, logged without the body the scorer reads
// (score.ts's acceptance clause names W from it), or a body that does not parse answered 200 or
// left out of the log.
test("a sign_off and a move back are logged with their bodies", async () => {
  const dir = freshDir();
  await withStandin(
    dir,
    [PROJECT, "LWEVAL-7", "architect", ...REPO, "accepting"],
    async (standin) => {
      const claim = await register(standin);
      const grantId = await grant(standin, claim);
      const signOff = { grantId, issue: "LWEVAL-7" };
      const signed = await post(standin, "/legion/v1/signoff", signOff);
      expect(signed.status).toBe(200);
      expect(await signed.json()).toEqual({});
      const backward = {
        grantId,
        to: "implementing",
        reason: `planted ${randomUUID()}`,
        issue: "LWEVAL-7",
      };
      const moved = await post(standin, "/legion/v1/phase/backward", backward);
      expect(moved.status).toBe(200);
      expect(await moved.json()).toEqual({});
      expect(lineOf(standin, "/legion/v1/signoff")).toMatchObject({
        status: 200,
        grantId,
        body: signOff,
      });
      expect(lineOf(standin, "/legion/v1/phase/backward")).toMatchObject({
        status: 200,
        grantId,
        body: backward,
      });

      // A body the schema refuses: a field LegionSignOffRequest does not name.
      const odd = await post(standin, "/legion/v1/signoff", { ...signOff, extra: "x" });
      expect(odd.status).toBe(400);
      expect(LegionErrorResponse.parse(await odd.json()).error).toContain("LegionSignOffRequest");
      const signOffLines = logLines(standin).filter((line) => line.path === "/legion/v1/signoff");
      expect(signOffLines.map((line) => line.status)).toEqual([200, 400]);
    }
  );
});

// Catches: an activity report answered 404 (the rig's end of a run waits on it), its log line
// without the report's backgroundWork and phase.nextReminderAt, or a line that keeps the claim
// token, the session id or the secret in its body.
test("an activity report is answered and logged with its backgroundWork", async () => {
  const dir = freshDir();
  await withStandin(
    dir,
    [PROJECT, "LWEVAL-7", "architect", ...REPO, "admitted"],
    async (standin) => {
      const claim = await register(standin);
      const activity = {
        at: new Date().toISOString(),
        backgroundWork: { running: true, kinds: ["async-job"] },
        phase: { nextReminderAt: "2026-11-24T12:05:00Z" },
      };
      const report = {
        claimToken: claim.claimToken,
        sessionId: SESSION,
        secret: claim.secret,
        activity,
      };
      const answered = await post(standin, "/legion/v1/claims/activity", report);
      expect(answered.status).toBe(204);
      expect(await answered.text()).toBe("");

      const line = lineOf(standin, "/legion/v1/claims/activity");
      expect(line.status).toBe(204);
      expect(line.body).toEqual(activity);
      expect(JSON.stringify(line.body)).not.toContain(claim.secret);
      expect(JSON.stringify(line.body)).not.toContain(claim.claimToken);
      expect(JSON.stringify(line.body)).not.toContain(SESSION);

      // Another session's secret is refused, and a report without its activity does not parse.
      const foreign = await post(standin, "/legion/v1/claims/activity", {
        ...report,
        secret: fixtureSecret("other"),
      });
      expect(foreign.status).toBe(403);
      const { activity: _, ...bare } = report;
      const empty = await post(standin, "/legion/v1/claims/activity", bare);
      expect(empty.status).toBe(400);
      expect(LegionErrorResponse.parse(await empty.json()).error).toContain(
        "LegionActivityRequest"
      );
    }
  );
});

// Catches: a header that states the rig's own plugin's contract when the run's label is another
// checkout, which is the contract the label's plugin speaks and `read_build` reads.
test("the health header states the label's contract, not the rig's", async () => {
  const dir = freshDir();
  const contract = OWN_CONTRACT + 7;
  const { label } = plantLabel(dir, contract);
  await withStandin(
    dir,
    [PROJECT, "LWEVAL-7", "architect", ...REPO, "admitted", "--label", label],
    async (standin) => {
      const health = await fetch(`${standin.url}/healthz`);
      expect(health.status).toBe(200);
      expect(health.headers.get("Legion-Daemon-Contract")).toBe(String(contract));
      expect(health.headers.get("Legion-Daemon-Contract")).not.toBe(String(OWN_CONTRACT));
      expect(await health.json()).toEqual({ status: "ok", project: PROJECT });
    }
  );
});

// Catches: a header that reads a label's contract from its manifest alone, which a checkout's
// manifest leaves out from the daemon API contract cutover on, where the label's plugin speaks the
// frozen list's greatest number plus its checkout's count of entry fixtures.
test("the health header states the contract a label's checkout counts when its manifest states none", async () => {
  const dir = freshDir();
  const last = OWN_CONTRACT + 20;
  const { label } = plantLabel(dir, { released: [last - 3, last], entries: 4 });
  await withStandin(
    dir,
    [PROJECT, "LWEVAL-7", "architect", ...REPO, "admitted", "--label", label],
    async (standin) => {
      const health = await fetch(`${standin.url}/healthz`);
      expect(health.status).toBe(200);
      expect(health.headers.get("Legion-Daemon-Contract")).toBe(String(last + 4));
    }
  );
});

// Catches: a state that holds the claim's issue alone, gives an issue its own key as its tree when
// the world names another, records an issue the world holds in no phase, which a daemon never
// records, or puts an issue on another repository or default branch than the command line names.
test("the state lists the world's issues with their tree", async () => {
  const dir = freshDir();
  const issuesFile = path.join(dir, "issues.json");
  writeFileSync(
    issuesFile,
    JSON.stringify([
      { key: "LWEVAL-1", tree: "LWEVAL-1", phase: "implementing", status: "in_progress" },
      { key: "LWEVAL-2", tree: "LWEVAL-1", phase: "implementing", status: "in_progress" },
      { key: "LWEVAL-3", tree: "LWEVAL-1", phase: null, status: "backlog" },
    ])
  );
  await withStandin(
    dir,
    [
      PROJECT,
      "LWEVAL-2",
      "architect",
      ...REPO,
      "implementing",
      "--tree",
      "LWEVAL-1",
      "--issues",
      issuesFile,
    ],
    async (standin) => {
      const claim = await register(standin);
      expect(claim.tree).toBe("LWEVAL-1");
      const answered = await fetch(`${standin.url}/legion/v1/state`);
      expect(answered.status).toBe(200);
      const state = LegionStateResponse.parse(await answered.json());
      const issues = state.issues ?? {};
      expect(Object.keys(issues).sort()).toEqual(["LWEVAL-1", "LWEVAL-2"]);
      for (const key of ["LWEVAL-1", "LWEVAL-2"]) {
        expect(issues[key]).toMatchObject({
          key,
          tree: "LWEVAL-1",
          phase: "implementing",
          repo: REPOSITORY,
          defaultBranch: DEFAULT_BRANCH,
        });
      }
      expect(state.admission?.trees).toEqual(["LWEVAL-1"]);
      const unrecorded = await fetch(`${standin.url}/legion/v1/issues/LWEVAL-3/record`);
      expect(unrecorded.status).toBe(404);
    }
  );
});

// Catches: an --issues file that would make the claim's own issue stand in another phase or tree
// than the stand-in's command line gives it, which the run's reminder check and owner read.
test("an --issues file that gives the claim's issue another phase or tree is refused, naming both", () => {
  const dir = freshDir();
  const issuesFile = path.join(dir, "issues.json");
  writeFileSync(
    issuesFile,
    JSON.stringify([
      { key: "LWEVAL-7", tree: "LWEVAL-7", phase: "implementing", status: "in_progress" },
    ])
  );
  const phase = refused(dir, [
    PROJECT,
    "LWEVAL-7",
    "architect",
    ...REPO,
    "accepting",
    "--issues",
    issuesFile,
  ]);
  expect(phase.code).toBe(2);
  expect(phase.stderr).toContain("implementing");
  expect(phase.stderr).toContain("accepting");
  const tree = refused(dir, [
    PROJECT,
    "LWEVAL-7",
    "architect",
    ...REPO,
    "implementing",
    "--tree",
    "LWEVAL-1",
    "--issues",
    issuesFile,
  ]);
  expect(tree.code).toBe(2);
  expect(tree.stderr).toContain("LWEVAL-1");
  expect(tree.stderr).toContain("LWEVAL-7");
});

// Catches: tasks/stop, release_children (its route is POST /legion/v1/waves/release) or
// gates/register answered 404, answered with something other than {}, or logged without its body,
// a refused one included.
test("tasks/stop, release_children's waves/release and gates/register answer {} with a 200 and log the body", async () => {
  const dir = freshDir();
  await withStandin(
    dir,
    [PROJECT, "LWEVAL-7", "architect", ...REPO, "admitted"],
    async (standin) => {
      const claim = await register(standin);
      const grantId = await grant(standin, claim);
      const bodies = {
        "/legion/v1/tasks/stop": { grantId, issue: "LWEVAL-8", task: "rig-task" },
        "/legion/v1/waves/release": { grantId, issues: ["LWEVAL-8"] },
        "/legion/v1/gates/register": {
          grantId,
          issue: "LWEVAL-7",
          artifactId: randomUUID(),
          version: 2,
          step: "spec",
        },
      };
      for (const [route, body] of Object.entries(bodies)) {
        const response = await post(standin, route, body);
        expect(response.status).toBe(200);
        expect(await response.json()).toEqual({});
        expect(lineOf(standin, route)).toMatchObject({ status: 200, grantId, body });
      }

      // A grant the stand-in never minted is refused, and its line still keeps the body.
      const unknown = { grantId: randomUUID(), issue: "LWEVAL-8", task: "rig-task" };
      expect((await post(standin, "/legion/v1/tasks/stop", unknown)).status).toBe(403);
      expect(
        logLines(standin)
          .filter((line) => line.path === "/legion/v1/tasks/stop")
          .at(-1)
      ).toMatchObject({ status: 403, body: unknown });
    }
  );
});

// Catches: a record route that answers the claim's issue in another phase than the one its role
// works, or moves it, so the phase reminder's record check (pi-shared/src/phase-reminder.ts) drops
// each reminder a deployment's worker would get while its phase is open.
test.skipIf(skipGo)(
  "issues/<key>/record keeps the worker's phase while it is open",
  async () => {
    const dir = freshDir();
    const issuesFile = path.join(dir, "issues.json");
    writeFileSync(
      issuesFile,
      JSON.stringify([
        { key: "LWEVAL-4", tree: "LWEVAL-4", phase: "testing", status: "in_progress" },
        { key: "LWEVAL-9", tree: "LWEVAL-9", phase: "implementing", status: "in_progress" },
      ])
    );
    await withStandin(
      dir,
      [PROJECT, "LWEVAL-4", "tester", ...REPO, "--issues", issuesFile],
      async (standin) => {
        const phaseNow = async (): Promise<string> => {
          const answered = await fetch(`${standin.url}/legion/v1/issues/LWEVAL-4/record`);
          expect(answered.status).toBe(200);
          return LegionIssueRecordResponse.parse(await answered.json()).phase;
        };
        expect(await phaseNow()).toBe("testing");
        expect(roleForPhase(await phaseNow())).toBe("tester");

        const claim = await register(standin);
        const report = {
          claimToken: claim.claimToken,
          sessionId: SESSION,
          secret: claim.secret,
          activity: {
            at: new Date().toISOString(),
            backgroundWork: { running: false, kinds: [] },
            phase: { nextReminderAt: "2026-11-24T12:05:00Z" },
          },
        };
        expect((await post(standin, "/legion/v1/claims/activity", report)).status).toBe(204);
        // A completion that names no test record is refused, so the phase stays open.
        const completion = await post(standin, "/legion/v1/handoff/complete", {
          grantId: await grant(standin, claim),
          summary: "the tests pass",
          verdict: "pass",
        });
        expect(completion.status).toBe(400);
        expect(LegionErrorResponse.parse(await completion.json()).code).toBe(
          "HANDOFF_RECORD_REQUIRED"
        );
        expect(await phaseNow()).toBe("testing");
      }
    );
  },
  GO_BOUND
);

// Catches: a change that breaks rig.sh's tester call (run_tester_proof: eight arguments, the
// world's repository and default branch last, and no option), which serves a tester's one issue in
// its own tree, on that repository and branch, with the rig's own contract; and a call that leaves
// out the repository and branch served, where the stand-in must refuse it.
test.skipIf(skipGo)(
  "rig.sh's eight-argument tester call serves a tester's claim on its repository, with the rig's own contract",
  async () => {
    const dir = freshDir();
    const short = refused(freshDir(), [PROJECT, "LWEVAL-5", "tester"]);
    expect(short.code).toBe(2);
    expect(short.stderr).toContain("<repository> <default branch>");
    await withStandin(dir, [PROJECT, "LWEVAL-5", "tester", ...REPO], async (standin) => {
      const claim = await register(standin);
      expect(claim).toMatchObject({ tree: "LWEVAL-5", issue: "LWEVAL-5" });
      const state = LegionStateResponse.parse(
        await (await fetch(`${standin.url}/legion/v1/state`)).json()
      );
      expect(Object.keys(state.issues ?? {})).toEqual(["LWEVAL-5"]);
      expect(state.issues?.["LWEVAL-5"]).toMatchObject({
        tree: "LWEVAL-5",
        phase: "testing",
        repo: REPOSITORY,
        defaultBranch: DEFAULT_BRANCH,
      });
      const health = await fetch(`${standin.url}/healthz`);
      expect(health.headers.get("Legion-Daemon-Contract")).toBe(String(OWN_CONTRACT));
      expect(existsSync(standin.log)).toBe(true);
    });
  },
  GO_BOUND
);
