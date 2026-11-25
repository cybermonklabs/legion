import { describe, expect, test } from "bun:test";
import { liveDaemonStates } from "../word-rewrite/daemons.ts";
import { liveDispatch } from "../word-rewrite/dispatch-api.ts";
import { runTool } from "../word-rewrite/main.ts";
import { builtInMap, type MapEntry, parseExtraMap } from "../word-rewrite/name-map.ts";
import { loggingFetch, RequestRate, requestLog } from "../word-rewrite/request-log.ts";
import {
  deps,
  FakeClock,
  FakeDispatch,
  FakeModel,
  fixture,
  INPUTS,
  input,
  NO_ANCESTRY,
  options,
} from "./word-rewrite-fake.ts";

const BASE = "http://dispatch.test";

interface Listed {
  readonly id: string;
  readonly name: string;
}

/** A Dispatch over HTTP that answers each read from the captured fixtures, and a daemon at the
 *  address `legion-daemon-set.json` names. Anything else is a 404. */
function fixtureServer() {
  const artifacts = fixture("issue-artifacts.json") as Listed[];
  const mixed = artifacts.find((artifact) => artifact.name === "mixed.md")?.id ?? "";
  const verification = artifacts.find((artifact) => artifact.name === "verification.md")?.id ?? "";
  const daemon = (fixture("legion-daemon-set.json") as { state_url: string }).state_url;
  const json = (body: unknown, status = 200) =>
    new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
  const routes: [RegExp, (url: URL) => Response][] = [
    [/^\/healthz$/, () => json(fixture("dispatch-healthz.json"))],
    [/^\/api\/v1\/whoami$/, () => json(fixture("whoami-shared.json"))],
    [
      /^\/api\/v1\/projects$/,
      () => json([{ key: "SEED", name: "Seed", gated_steps: [], open_asks: 0 }]),
    ],
    [/^\/api\/v1\/projects\/SEED\/legion\/daemon$/, () => json(fixture("legion-daemon-set.json"))],
    [/^\/api\/v1\/projects\/SEED\/artifacts$/, () => json([])],
    [
      /^\/api\/v1\/projects\/SEED\/artifacts\/[^/]+\/text$/,
      () => json({ code: "NOT_FOUND", error: "none" }, 404),
    ],
    [
      /^\/api\/v1\/issues$/,
      (url) =>
        url.searchParams.has("parent")
          ? json([])
          : json(
              fixture(
                url.searchParams.get("open") === "true" ? "issues-open.json" : "issues-all.json"
              )
            ),
    ],
    [/^\/api\/v1\/issues\/SEED-3\/artifacts$/, () => json(artifacts)],
    [/^\/api\/v1\/issues\/[^/]+\/artifacts$/, () => json([])],
    [/^\/api\/v1\/issues\/SEED-3\/events$/, () => json(fixture("issue-events.json"))],
    [/^\/api\/v1\/issues\/[^/]+\/events$/, () => json([])],
    [
      /^\/api\/v1\/artifacts\/[^/]+\/text$/,
      (url) => {
        if (url.pathname.includes(mixed)) return json(fixture("text-blocks.json"));
        if (url.pathname.includes(verification)) {
          return json(fixture("text-blocks-verification.json"));
        }
        return json({ markdown: "Nothing here.\n", token: "sha256:0", version: 1, blocks: [] });
      },
    ],
    [
      /^\/api\/v1\/artifacts\/[^/]+\/versions\/(\d+)$/,
      (url) => json(fixture(`version-blocks-${url.pathname.split("/").at(-1)}.json`)),
    ],
    [
      /^\/api\/v1\/events\/log$/,
      (url) => {
        const after = Number(url.searchParams.get("after"));
        const limit = Number(url.searchParams.get("limit"));
        const events = (fixture("event-log.json") as { id: number }[]).filter(
          (event) => event.id > after
        );
        return json(events.slice(0, limit));
      },
    ],
    [/^\/legion\/v1\/state$/, () => json(fixture("daemon-state-contract.json"))],
  ];
  const serve = async (input: Parameters<typeof fetch>[0], init?: RequestInit) => {
    const url = new URL(input instanceof Request ? input.url : String(input));
    if (url.origin !== BASE && url.origin !== new URL(daemon).origin) return json({}, 404);
    const method = init?.method ?? "GET";
    if (method !== "GET") return json({ code: "WRITE", error: "a write" }, 418);
    const route = routes.find(([pattern]) => pattern.test(url.pathname));
    return route === undefined
      ? json({ code: "NOT_FOUND", error: url.pathname }, 404)
      : route[1](url);
  };
  return Object.assign(serve, { preconnect: fetch.preconnect });
}

async function reportRun(extraEntries: readonly MapEntry[] = []) {
  const requestLines: string[] = [];
  const log = requestLog((line) => requestLines.push(line));
  const clock = new FakeClock();
  const fetchImpl = loggingFetch(fixtureServer(), log, new RequestRate(clock, 0));
  const credential = {
    scheme: "Bearer",
    headers: () => ({ Authorization: "Bearer t" }),
    secret: () => "t",
  } as const;
  const { reads } = liveDispatch({ url: BASE, credential }, fetchImpl);
  const recorded = new FakeDispatch();
  const model = new FakeModel(() => {
    throw new Error("report mode called the model");
  });
  const lines: string[] = [];
  const code = await runTool(options({ tree: "SEED-1", projects: ["SEED"] }), {
    reads,
    writes: recorded.writes(),
    model,
    daemonStates: liveDaemonStates(fetchImpl),
    ancestry: NO_ANCESTRY,
    clock,
    contract: 220,
    extraEntries,
    out: (line) => lines.push(line),
    requests: () => log.count(),
  });
  return { code, lines, requestLines, recorded, model };
}

describe("report mode", () => {
  test("calls no write and no model: the fakes record every call", async () => {
    const run = await reportRun();
    expect(run.code).toBe(0);
    expect(run.recorded.writeCalls).toEqual([]);
    expect(run.model.prompts).toEqual([]);
    expect(run.lines.at(-1)).toBe("result done");
  });

  test("its request log holds only GET lines, with no host and no header", async () => {
    const run = await reportRun();
    const lines = run.requestLines;
    expect(lines.length).toBeGreaterThan(10);
    for (const line of lines) expect(line).toMatch(/^GET \/\S* \d{3}$/);
    expect(lines).toContain("GET /api/v1/whoami 200");
    expect(lines).toContain("GET /legion/v1/state 200");
    expect(lines.some((line) => line.includes("/subscribers"))).toBe(false);
  });

  test("lists each occurrence with its place, counted by kind and project and by reason", async () => {
    const { lines } = await reportRun();
    const health = fixture("dispatch-healthz.json") as { commit: string };
    expect(lines).toContain(`dispatch commit=${health.commit} holds-merge=no`);
    expect(lines).toContain("daemon SEED source=dispatch contract=188");
    const occurrences = lines.filter((line) => line.startsWith("occurrence "));
    expect(
      occurrences.some(
        (line) =>
          line.includes("issue=SEED-3 document=mixed-md") && line.endsWith("reason=ask-block")
      )
    ).toBe(true);
    expect(
      occurrences.some((line) => line.includes("kind=title") && line.includes("not-judged"))
    ).toBe(true);
    expect(occurrences.some((line) => line.includes("byte=name reason=name-or-slug"))).toBe(true);
    expect(
      lines.some((line) => /^count issue-document SEED occurrences=\d+ not-judged=\d+$/.test(line))
    ).toBe(true);
    expect(lines).toContain("reason check-block 2");
    expect(lines).toContain("owner SEED-3 subscribers=unknown");
    expect(lines.some((line) => /^pace writes=\d+ run-time-s=\d+$/.test(line))).toBe(true);
    expect(
      lines.some((line) =>
        /^state verdicts=0 held=0 waiting=\d+ not-judged=\d+ bytes=\d+ projected-bytes=\d+ bound=921600$/.test(
          line
        )
      )
    ).toBe(true);
  });

  test("lists each open issue's check block that names an old name of the map, counted (spec version 19)", async () => {
    // verification.md's check block runs a test the built-in map renames. The Go test's name
    // mixed.md's check block runs, which holds the word, no row of the built-in map names, so it
    // is listed only once a row of the run's map does.
    const text = fixture("text-blocks.json") as { markdown: string };
    const testName = /Test[A-Za-z]+/.exec(text.markdown)?.[0] ?? "";
    const verificationLine =
      "check-name issue=SEED-3 document=verification-md block=k1 old=TestBootReleasesParkedClaims";
    const bare = await reportRun();
    expect(bare.lines.filter((line) => line.startsWith("check-name "))).toEqual([verificationLine]);
    expect(bare.lines).toContain("check-names 1 approved-latest=0");
    const extra = {
      old: testName,
      new: "TestTheRenamedTest",
      codeName: true,
      from: "merged" as const,
    };
    const { lines } = await reportRun([extra]);
    expect(lines.filter((line) => line.startsWith("check-name "))).toEqual([
      `check-name issue=SEED-3 document=mixed-md block=check-word old=${testName}`,
      verificationLine,
    ]);
    expect(lines).toContain("check-names 2 approved-latest=0");
  });

  /** A report run over a fake Dispatch, its printed lines kept. */
  async function fakeReport(dispatch: FakeDispatch, extraEntries: readonly MapEntry[] = []) {
    const run = deps(dispatch, { extraEntries });
    expect(await runTool(options(), run)).toBe(0);
    return run.lines.filter((line) => line.startsWith("check-name"));
  }

  test("a check block that names an old name of the map is listed under that name", async () => {
    const dispatch = new FakeDispatch();
    const issue = dispatch.issue("SEED", "Checks");
    dispatch.document({ issue: issue.key }, "verification.md", input("check-names"));
    const olds = INPUTS["check-names"];
    expect(await fakeReport(dispatch)).toEqual([
      ...olds.map((old) => `check-name issue=SEED-1 document=verification-md block=k1 old=${old}`),
      `check-names ${olds.length} approved-latest=0`,
    ]);
    const mapped = builtInMap(220).map((entry) => entry.old);
    expect(olds.every((old) => mapped.includes(old))).toBe(true);
  });

  test("a check block of a document a person approved at its latest version is marked approved=latest and counted apart", async () => {
    const dispatch = new FakeDispatch();
    const approved = dispatch.issue("SEED", "Approved checks");
    const document = dispatch.document(
      { issue: approved.key },
      "verification.md",
      input("check-names")
    );
    document.approval = { state: "approved", kind: "person", latest_version: 1, version: 1 };
    const draft = dispatch.issue("SEED", "Draft checks");
    dispatch.document({ issue: draft.key }, "verification.md", input("check-names"));
    const [first, second] = INPUTS["check-names"];
    expect(await fakeReport(dispatch)).toEqual([
      `check-name issue=SEED-1 document=verification-md block=k1 old=${first} approved=latest`,
      `check-name issue=SEED-1 document=verification-md block=k1 old=${second} approved=latest`,
      `check-name issue=SEED-2 document=verification-md block=k1 old=${first}`,
      `check-name issue=SEED-2 document=verification-md block=k1 old=${second}`,
      "check-names 4 approved-latest=2",
    ]);
  });

  test("an --extra-map row's old name is matched in a check block too", async () => {
    const dispatch = new FakeDispatch();
    const issue = dispatch.issue("SEED", "Checks of a child's names");
    dispatch.document(
      { issue: issue.key },
      "verification.md",
      ':::check{#k1 run="kubectl get cronjob/old-child-name"}\nThe job runs.\n:::\n'
    );
    const extra = parseExtraMap(
      '{"old": "old-child-name", "new": "new-child-name", "codeName": true, "from": "merged"}\n'
    );
    expect(await fakeReport(dispatch)).toEqual(["check-names 0 approved-latest=0"]);
    expect(await fakeReport(dispatch, extra)).toEqual([
      "check-name issue=SEED-1 document=verification-md block=k1 old=old-child-name",
      "check-names 1 approved-latest=0",
    ]);
  });
});
