// register_plan of daemon-standin.ts as the rig runs it: a real process on a port of its own, with a
// stub Dispatch and a planted label checkout whose `label-texts.sh` is a stub of the entry. Each row
// starts its own stand-in and reads what it answered and what it wrote to its log.
import { expect, setDefaultTimeout, test } from "bun:test";
import { randomUUID } from "node:crypto";
import { existsSync, readFileSync, writeFileSync } from "node:fs";
import path from "node:path";
import { LegionErrorResponse } from "@legion/contracts/legion-api";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { loopbackHost } from "@legion/test-fixtures/loopback-host";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import type { Server } from "bun";
import {
  grant,
  lineOf,
  logLines,
  OWN_CONTRACT,
  PROJECT,
  plantLabel,
  post,
  REPO,
  register,
  type Standin,
  withStandin,
} from "./daemon-standin-harness";
import { PlanRequest } from "./scored-runs-contracts";

setDefaultTimeout(60_000);

const freshDir = temporaryDirectories("daemon-standin-plan");

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
