// heads-world.ts's ci-pass-and-fail world against a stubbed Dispatch, GitHub stand-in and NATS: each
// request the two modes make and the fixture they write, for each of its three roles.
//
//   bun test heads-world.test.ts

import { beforeAll, expect, mock, setDefaultTimeout, spyOn, test } from "bun:test";
import { mkdirSync, readFileSync, writeFileSync } from "node:fs";
import path from "node:path";
import { restoreEnvironment } from "@legion/test-fixtures/environment";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import type * as headsModule from "./heads-world";
import { fileRowLimit } from "./test-limit";

// Each row runs the two modes against stubs in this process; a loaded machine stretches even that,
// so each test and hook gets the limit test-limit.ts computes from the bounds of what they wait for.
setDefaultTimeout(fileRowLimit());

// e2e/api.ts reads its port when it loads, and harness-ports.ts reads FAKE_GITHUB_PORT: both are
// set in the hook below, before heads-world.ts loads, and go back to what the caller held after.
restoreEnvironment([
  "DISPATCH_E2E_PORT",
  "FAKE_GITHUB_PORT",
  "DISPATCH_E2E_NATS_URLS",
  "PLAYWRIGHT_BASE_URL",
]);
const dirOf = temporaryDirectories("heads-world");
let heads: typeof headsModule;

/** What a run's NATS was asked: the subjects published, in order. */
const published: string[] = [];
beforeAll(async () => {
  process.env.DISPATCH_E2E_PORT = "18777";
  process.env.FAKE_GITHUB_PORT = "18778";
  process.env.DISPATCH_E2E_NATS_URLS = "nats://127.0.0.1:1";
  delete process.env.PLAYWRIGHT_BASE_URL;
  mock.module("nats", () => ({
    connect: async () => ({
      publish: (subject: string) => published.push(subject),
      flush: async () => {},
      close: async () => {},
    }),
    StringCodec: () => ({ encode: (text: string) => text }),
  }));
  heads = await import("./heads-world");
});

interface Call {
  method: string;
  path: string;
  body: Record<string, unknown>;
}

/** Runs heads-issue and heads-events of a world against the stubs, and answers each call. */
async function world(role: string | undefined, scenarioName = "ci-pass-and-fail") {
  const calls: Call[] = [];
  published.length = 0;
  let handoffs = 0;
  spyOn(globalThis, "fetch").mockImplementation((async (
    input: string | URL | Request,
    init?: RequestInit
  ) => {
    const url = new URL(input instanceof Request ? input.url : String(input));
    const body = init?.body === undefined ? {} : JSON.parse(String(init.body));
    calls.push({ method: init?.method ?? "GET", path: url.pathname, body });
    const json = (value: unknown, status = 200) => Response.json(value, { status });
    if (url.pathname === "/api/v1/issues") return json({ key: "LWEVAL-7" }, 201);
    if (url.pathname.endsWith("/artifacts"))
      return json({ artifact: { id: "art-verification" } }, 201);
    if (url.pathname.includes("/handoffs/"))
      return json({ id: `h-${++handoffs}`, version: handoffs }, 201);
    // A recorded result: Dispatch answers the line its pull request's body carries.
    if (url.pathname.endsWith("/results")) return json({ line: "- Check k1" }, 201);
    return json({});
  }) as typeof fetch);
  const dir = dirOf(`${scenarioName}-${role ?? "one"}`);
  const file = path.join(dir, "fixture.json");
  const headsFile = path.join(dir, "heads.json");
  writeFileSync(headsFile, JSON.stringify({ "702": "a".repeat(40), "703": "b".repeat(40) }));
  const scenario = heads.headsWorld(scenarioName, role);
  if (scenario === undefined) throw new Error(`no world of ${scenarioName} ${role}`);
  await heads.headsIssue(file, scenario);
  await heads.headsEvents(file, headsFile, scenario);
  return { calls, fixture: JSON.parse(readFileSync(file, "utf8")) };
}

// Catches: a world whose k1 is no check CI proves (a role's result would then decide it), a CI run
// that has concluded (no not_completed), a second pull request, a role's result of a check only CI
// records, and records that do not match the role's task (rig.sh's prompts read them).
test("ci-pass-and-fail: k1 is go-checks' check, whose run at #702's head is in progress, and each role gets its records", async () => {
  const records: Record<string, string[]> = {};
  for (const role of ["reviewer", "tester", "architect"]) {
    const { calls, fixture } = await world(role);
    const verification = calls.find((call) => call.path === "/api/v1/issues/LWEVAL-7/artifacts");
    expect(String(verification?.body.content)).toContain(
      ':::check{#k1 surface="cli" serves="dispatch://LWEVAL-7/spec#b-r1" ci_check="go-checks" when="before_merge" negative_control="Drop the line from docs/usage.md and the go-checks job fails."}'
    );
    const seeds = calls
      .filter((call) => call.path === "/__fixture/repos")
      .map((call) => call.body["example/widgets"]);
    expect(seeds.at(-1)).toMatchObject({
      pulls: { "702": { head_sha: "a".repeat(40) } },
      check_runs: {
        ["a".repeat(40)]: [
          { name: "go-checks", status: "in_progress", conclusion: null, completed_at: null },
        ],
      },
    });
    expect(seeds.at(-1)).toHaveProperty("pulls", { "702": expect.anything() });
    expect(published).toEqual(["notifications.github.example.widgets.pr.702"]);
    expect(calls.filter((call) => call.path.endsWith("/results"))).toEqual([]);
    records[role] = calls
      .filter((call) => call.path.includes("/handoffs/"))
      .map((call) => `${call.path.split("/").at(-1)} by ${JSON.stringify(call.body.actor)}`);
    expect(fixture.verification).toBe("art-verification");
  }
  const by = (id: string) => JSON.stringify({ kind: "session", id });
  expect(records).toEqual({
    reviewer: [`test by ${by("rig-tester")}`],
    tester: [`implement by ${by("rig-implementer")}`],
    architect: [`test by ${by("rig-tester")}`, `review by ${by("rig-reviewer")}`],
  });
});

// Catches: a check k1 that serves no requirement or whose run proves less than R1: the first item 3
// runs of architect-pass-and-fail sent the issue back for that gap in every label, so the control
// could not score below head.
test("architect-pass-and-fail: k1 serves R1, its words are R1's, and its run matches R1's line alone", async () => {
  // The ports the hook above sets, which restoreEnvironment puts back after each test.
  process.env.DISPATCH_E2E_PORT = "18777";
  process.env.FAKE_GITHUB_PORT = "18778";
  const calls: { path: string; body: Record<string, unknown> }[] = [];
  spyOn(globalThis, "fetch").mockImplementation((async (
    input: string | URL | Request,
    init?: RequestInit
  ) => {
    const url = new URL(input instanceof Request ? input.url : String(input));
    calls.push({
      path: url.pathname,
      body: init?.body === undefined ? {} : JSON.parse(String(init.body)),
    });
    if (url.pathname === "/api/v1/issues")
      return Response.json({ key: "LWEVAL-7" }, { status: 201 });
    if (url.pathname.endsWith("/artifacts"))
      return Response.json({ artifact: { id: "art-verification" } }, { status: 201 });
    return Response.json({});
  }) as typeof fetch);
  const file = path.join(dirOf("architect"), "fixture.json");
  const scenario = heads.headsWorld("architect-pass-and-fail", undefined);
  if (scenario === undefined) throw new Error("no world of architect-pass-and-fail");
  await heads.headsIssue(file, scenario);
  const spec = String(calls.find((call) => call.path === "/api/v1/issues")?.body.spec);
  const content = String(calls.find((call) => call.path.endsWith("/artifacts"))?.body.content);
  expect(spec).toContain(":::requirement{#r1}");
  const run = /run="([^"]+)"/.exec(content)?.[1] ?? "";
  expect(content).toContain(
    `:::check{#k1 surface="cli" serves="dispatch://LWEVAL-7/spec#b-r1" run="${run}"`
  );
  expect(content).toContain("`docs/usage.md` names `widgets --help`, which lists every command.");
  const grep = (text: string) => {
    const dir = dirOf(`grep-${text.length}`);
    mkdirSync(path.join(dir, "docs"), { recursive: true });
    writeFileSync(path.join(dir, "docs/usage.md"), text);
    return Bun.spawnSync(["bash", "-c", run], { cwd: dir }).stdout.toString().trim();
  };
  // The fixture's line (rig.sh heads_fixture), and a line that names the CLI but not R1.
  expect(grep("# Usage\n\nRun `widgets --help` to list every command of the widgets CLI.\n")).toBe(
    "1"
  );
  expect(grep("# Usage\n\nThe widgets CLI has commands.\n")).toBe("0");
});

test("ci-pass-and-fail takes the three roles and no other case", () => {
  expect(
    ["reviewer", "tester", "architect", "merger", undefined].map((role) =>
      heads.headsWorld("ci-pass-and-fail", role)
    )
  ).toEqual([
    "ci-pass-and-fail-reviewer",
    "ci-pass-and-fail-tester",
    "ci-pass-and-fail-architect",
    undefined,
    undefined,
  ]);
});

// Catches: a no-tester world in which the reviewer cannot learn who made #702's head. Its pass is
// the implementer's, and the reviewer's prompt finds the head's maker through the `implement`
// handoff whose `head` names the head it reviews, so the implementer's session records both: the
// pass of k1 at #702's head and that handoff, before the tester's `test` record.
test("reviewer-own-head no-tester: the implementer's session records the pass and the implement handoff of #702's head", async () => {
  // The ports the hook above sets, which restoreEnvironment puts back after each test.
  process.env.DISPATCH_E2E_PORT = "18777";
  process.env.FAKE_GITHUB_PORT = "18778";
  process.env.DISPATCH_E2E_NATS_URLS = "nats://127.0.0.1:1";
  const { calls, fixture } = await world("no-tester", "reviewer-own-head");
  const writes = calls
    .filter((call) => call.path.endsWith("/results") || call.path.includes("/handoffs/"))
    .map((call) =>
      call.path.endsWith("/results")
        ? `result ${call.body.verdict} of ${call.body.check} at ${call.body.commit} by ${JSON.stringify(call.body.actor)}`
        : `${call.path.split("/").at(-1)} at ${JSON.parse(String(call.body.body)).head} by ${JSON.stringify(call.body.actor)}`
    );
  const by = (id: string) => JSON.stringify({ kind: "session", id });
  const head = "a".repeat(40);
  expect(writes).toEqual([
    `result pass of k1 at ${head} by ${by("rig-implementer")}`,
    `implement at ${head} by ${by("rig-implementer")}`,
    `test at ${head} by ${by("rig-tester")}`,
  ]);
  expect(fixture.implement).toEqual({ id: "h-1", version: 1 });
});

// The words the pull-request line Dispatch prints for a result keeps (plainWord,
// packages/envoy/internal/dispatch/api/results.go): letters, digits and .,;:!?'"-()/.
const PLAIN_WORD = /^[\p{L}\p{N}.,;:!?'"\-()/]+$/u;

// Catches: a run or a reading of k1 with a word the line leaves out. Each pull request's body
// carries that line, so it then quoted another command than the one that ran: `widgets
// --help.+every command` printed as `grep -cE 'widgets command'`, which prints 0 at the head, and a
// reviewer of reviewer-own-head requested changes on that body line instead of judging the gate.
test("every run and reading of k1 the heads worlds record holds only words the pull-request line keeps", async () => {
  // The ports the hook above sets, which restoreEnvironment puts back after each test.
  process.env.DISPATCH_E2E_PORT = "18777";
  process.env.FAKE_GITHUB_PORT = "18778";
  process.env.DISPATCH_E2E_NATS_URLS = "nats://127.0.0.1:1";
  const worlds: [string | undefined, string][] = [
    ...["sibling-fails", "ci-only", "no-implementer", "no-tester"].map(
      (which): [string, string] => [which, "reviewer-own-head"]
    ),
    [undefined, "reviewer-pass-and-fail"],
    [undefined, "architect-pass-and-fail"],
  ];
  const dropped: string[] = [];
  let readings = 0;
  for (const [which, scenario] of worlds) {
    const { calls } = await world(which, scenario);
    for (const call of calls.filter((each) => each.path.endsWith("/results"))) {
      for (const field of ["run", "observed"]) {
        readings++;
        for (const word of String(call.body[field]).split(/\s+/).filter(Boolean))
          if (!PLAIN_WORD.test(word)) dropped.push(`${scenario} ${which ?? ""} ${field}: ${word}`);
      }
    }
  }
  expect({ dropped, recorded: readings > 0 }).toEqual({ dropped: [], recorded: true });
});
