import { afterAll, beforeAll, beforeEach, describe, expect, test } from "bun:test";
import { createHash } from "node:crypto";
import { mkdtemp, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { CannotRunError } from "../corpus-io";
import { type DispatchConfig, DispatchRefusedError } from "../dispatch-read";
import {
  pickedRoot,
  RESERVE_DOCUMENT,
  RESERVE_LIST_FILE,
  RESERVE_RULE_VERSION,
  ruleVersionsOf,
} from "../reserve";
import { askIdOf, decideAsk, listOf, main, UndecidedError } from "../reserve-ask";

/**
 * `reserve-ask.ts`, the one live decision under the reserve, against a fake Dispatch that serves
 * the reads it makes in the shapes the real routes answer: the registration document's text, one
 * ask (`{ask, answers, ...}`), each issue with its `parent` now, each issue's own log
 * (`/events?after=<seq>&limit=`, rows with the global `id`, `seq`, `type` and the issue as
 * payload), and version 3's list as bytes. Its trees use a neutral project key: ACME-1 and ACME-2
 * are roots version 2's hash picks, ACME-3 and ACME-4 roots it does not, and the list holds ACME-2
 * and ACME-4. Every ask opens at event 50; three issues move to another root at event 60.
 */

const TOKEN = "fake-bearer";
const LIST = "ACME-2\nACME-4\n";
const LIST_SHA256 = createHash("sha256").update(LIST, "utf8").digest("hex");
const OPENED_EVENT = 50;

const VERSIONS = [
  "# The reserve",
  "## Version 2: the tree is the unit",
  "## Version 1 released",
  "## Version 3: hold only the trees that can give the final test",
  "### The rule",
].join("\n\n");

/** Each issue's parent over time: the event that set it, and the parent it set. */
const HISTORY: Record<string, [number, unknown][]> = {
  "ACME-1": [[10, null]],
  "ACME-2": [[11, null]],
  "ACME-3": [[12, null]],
  "ACME-4": [[13, null]],
  "ACME-11": [[20, "ACME-1"]],
  "ACME-12": [[21, "ACME-4"]],
  "ACME-13": [
    [22, "ACME-1"],
    [60, "ACME-2"],
  ],
  "ACME-14": [
    [23, "ACME-3"],
    [60, "ACME-4"],
  ],
  "ACME-15": [
    [24, "ACME-4"],
    [60, "ACME-3"],
  ],
  "ACME-16": [
    [29, "ACME-1"],
    [60, "ACME-3"],
  ],
  "ACME-20": [[25, "ACME-21"]],
  "ACME-21": [[26, "ACME-20"]],
  "ACME-30": [[27, "ACME-99"]],
  "ACME-40": [[28, 5]],
};

/** An issue's own log, as the route pages it. */
function logOf(key: string, after: number, limit: number): unknown[] {
  return (HISTORY[key] ?? [])
    .map(([id, parent], index) => ({
      id,
      seq: index + 1,
      issue_key: key,
      type: index === 0 ? "issue.created" : "issue.updated",
      payload: { key, parent, title: "an issue" },
    }))
    .filter((row) => row.seq > after)
    .slice(0, limit);
}

const BEFORE_V2 = "2026-11-15T12:00:00Z";
const V2 = "2026-11-17T12:00:00Z";
const V3 = "2026-11-18T06:00:00Z";

/** Each ask the fake serves, by an id of the ask's shape. */
const ASKS: Record<string, Record<string, unknown>> = {};
/** An ask on `issue`, first opened at `opened`, with `fields` set over the usual ones or left out. */
function askFor(
  issue: string | null,
  opened: string,
  fields: Record<string, unknown> = {},
  leaveOut?: "issue_key" | "opened_event_id"
): string {
  const id = `00000000-0000-4000-8000-${String(Object.keys(ASKS).length + 1).padStart(12, "0")}`;
  const ask: Record<string, unknown> = {
    id,
    issue_key: issue,
    created_at: opened,
    opened_event_id: OPENED_EVENT,
    question: "the request's own words",
    ...fields,
  };
  if (leaveOut !== undefined) delete ask[leaveOut];
  ASKS[id] = ask;
  return id;
}
const BEFORE_V2_ON_PICKED = askFor("ACME-11", BEFORE_V2);
const V2_ON_PICKED = askFor("ACME-11", V2);
const V2_ON_PICKED_LISTED = askFor("ACME-2", V2);
const V2_ON_UNPICKED = askFor("ACME-3", V2);
const V2_MOVED_UNDER_LISTED = askFor("ACME-13", V2);
const V2_MOVED_UNDER_UNPICKED = askFor("ACME-16", V2);
const V3_UNDER_LISTED = askFor("ACME-12", V3);
const V3_ON_UNLISTED = askFor("ACME-3", V3);
const V3_UNDER_UNLISTED = askFor("ACME-11", V3);
const V3_MOVED_UNDER_LISTED = askFor("ACME-14", V3);
const V3_MOVED_OUT_OF_LISTED = askFor("ACME-15", V3);
const V3_ON_NO_ISSUE = askFor(null, V3);
const V3_WITH_NO_ISSUE_FIELD = askFor("ACME-12", V3, {}, "issue_key");
const V3_WITH_NO_OPENING_EVENT = askFor("ACME-14", V3, {}, "opened_event_id");
const V3_WITH_TEXT_OPENING_EVENT = askFor("ACME-14", V3, { opened_event_id: "50" });
const V3_IN_CYCLE = askFor("ACME-20", V3);
const V3_UNDER_MISSING = askFor("ACME-30", V3);
const V3_UNDER_BAD_PARENT = askFor("ACME-40", V3);

/** The registration's text the fake serves, which a test may change. */
let markdown = VERSIONS;
/** Each path the fake served, in order. */
const seen: string[] = [];

let server: { stop(closeActiveConnections?: boolean): void | Promise<void> } | undefined;
let directory: string | undefined;
let config: DispatchConfig;
const before = { url: process.env.DISPATCH_URL, tokenFile: process.env.DISPATCH_TOKEN_FILE };

beforeAll(async () => {
  const documentPath = `/api/v1/projects/${RESERVE_DOCUMENT.project}/artifacts/${RESERVE_DOCUMENT.slug}/text`;
  const listPath =
    `/api/v1/projects/${RESERVE_LIST_FILE.project}/artifacts/${RESERVE_LIST_FILE.slug}` +
    `/versions/${RESERVE_LIST_FILE.version}`;
  const started = Bun.serve({
    hostname: "127.0.0.1",
    port: 0,
    fetch(request) {
      const url = new URL(request.url);
      const path = url.pathname;
      seen.push(path);
      if (request.headers.get("authorization") !== `Bearer ${TOKEN}`) {
        return new Response("no bearer", { status: 401 });
      }
      if (path === documentPath) return Response.json({ markdown, token: "t", version: 21 });
      if (path === listPath)
        return new Response(LIST, { headers: { "content-type": "text/plain" } });
      const ask = /^\/api\/v1\/asks\/([^/]+)$/.exec(path)?.[1];
      if (ask !== undefined && Object.hasOwn(ASKS, ask)) {
        return Response.json({ ask: ASKS[ask], answers: [{ text: "his answer" }] });
      }
      const log = /^\/api\/v1\/issues\/([^/]+)\/events$/.exec(path)?.[1];
      if (log !== undefined && Object.hasOwn(HISTORY, log)) {
        const after = Number(url.searchParams.get("after") ?? "0");
        const limit = Number(url.searchParams.get("limit") ?? "200");
        return Response.json(logOf(log, after, limit));
      }
      const issue = /^\/api\/v1\/issues\/([^/]+)$/.exec(path)?.[1];
      if (issue !== undefined && Object.hasOwn(HISTORY, issue)) {
        // The parent now is the last one the issue's history set.
        return Response.json({ key: issue, parent: HISTORY[issue]?.at(-1)?.[1] });
      }
      return Response.json({ code: "NOT_FOUND", error: "not found" }, { status: 404 });
    },
  });
  server = started;
  config = { baseUrl: `http://127.0.0.1:${started.port}`, token: TOKEN };
  directory = await mkdtemp(join(tmpdir(), "reserve-ask-"));
  const tokenFile = join(directory, "token");
  await writeFile(tokenFile, `${TOKEN}\n`);
  process.env.DISPATCH_URL = config.baseUrl;
  process.env.DISPATCH_TOKEN_FILE = tokenFile;
});

afterAll(async () => {
  await server?.stop(true);
  if (directory !== undefined) await rm(directory, { recursive: true, force: true });
  for (const [name, value] of [
    ["DISPATCH_URL", before.url],
    ["DISPATCH_TOKEN_FILE", before.tokenFile],
  ] as const) {
    if (value === undefined) delete process.env[name];
    else process.env[name] = value;
  }
});

beforeEach(() => {
  markdown = VERSIONS;
  seen.length = 0;
});

const decide = (ask: string) => decideAsk(ask, config, LIST_SHA256);

describe("the fake's trees", () => {
  test("ACME-1 and ACME-2 are roots version 2's hash picks, ACME-3 and ACME-4 are not", () => {
    expect(["ACME-1", "ACME-2", "ACME-3", "ACME-4"].map(pickedRoot)).toEqual([
      true,
      true,
      false,
      false,
    ]);
  });
});

describe("decideAsk from version 3's cut", () => {
  test("an ask is kept only when its root at the opening and its root now are both listed", async () => {
    expect(await decide(V3_UNDER_LISTED)).toMatchObject({
      issue: "ACME-12",
      opening_root: "ACME-4",
      root_now: "ACME-4",
      decision: "kept",
    });
    expect((await decide(V3_ON_UNLISTED)).decision).toBe("held");
    // Version 2's hash picks no root from version 3's cut: an unlisted root is held either way.
    expect((await decide(V3_UNDER_UNLISTED)).decision).toBe("held");
  });

  test("a tree that moved under a listed root since the opening stays held", async () => {
    expect(await decide(V3_MOVED_UNDER_LISTED)).toMatchObject({
      opening_root: "ACME-3",
      root_now: "ACME-4",
      opening_listed: false,
      now_listed: true,
      decision: "held",
    });
  });

  test("a tree that moved out of a listed root since the opening is held", async () => {
    expect(await decide(V3_MOVED_OUT_OF_LISTED)).toMatchObject({
      opening_root: "ACME-4",
      root_now: "ACME-3",
      decision: "held",
    });
  });
});

describe("decideAsk before version 3's cut", () => {
  test("version 2's hash holds a picked root, and a listed root at the opening releases", async () => {
    expect((await decide(V2_ON_PICKED)).decision).toBe("held");
    expect((await decide(V2_ON_PICKED_LISTED)).decision).toBe("kept");
    expect((await decide(V2_ON_UNPICKED)).decision).toBe("kept");
  });

  test("a listed root now releases nothing when the root at the opening was not listed", async () => {
    expect(await decide(V2_MOVED_UNDER_LISTED)).toMatchObject({
      opening_root: "ACME-1",
      root_now: "ACME-2",
      decision: "held",
    });
  });

  test("a root now that the hash does not pick frees nothing the root at the opening holds", async () => {
    expect(await decide(V2_MOVED_UNDER_UNPICKED)).toMatchObject({
      opening_root: "ACME-1",
      root_now: "ACME-3",
      opening_listed: false,
      now_listed: false,
      decision: "held",
    });
  });

  test("an ask that opened before version 2's cut is kept", async () => {
    expect((await decide(BEFORE_V2_ON_PICKED)).decision).toBe("kept");
  });
});

describe("decideAsk on what it cannot read", () => {
  test("an ask on no issue is kept, and no issue is read", async () => {
    expect(await decide(V3_ON_NO_ISSUE)).toMatchObject({
      issue: null,
      opening_root: null,
      root_now: null,
      decision: "kept",
    });
    expect(seen.some((path) => path.startsWith("/api/v1/issues/"))).toBe(false);
  });

  test("an ask whose answer has no issue field decides nothing, not an ask on no issue", async () => {
    await expect(decide(V3_WITH_NO_ISSUE_FIELD)).rejects.toBeInstanceOf(UndecidedError);
  });

  test("an ask with no opening event, or one given as text, decides nothing", async () => {
    // Without the opening event the walk would read the whole log, and the root at the opening
    // would become the root now: ACME-14's root now is listed, its root at the opening is not.
    await expect(decide(V3_WITH_NO_OPENING_EVENT)).rejects.toBeInstanceOf(UndecidedError);
    await expect(decide(V3_WITH_TEXT_OPENING_EVENT)).rejects.toBeInstanceOf(UndecidedError);
  });

  test("a parent now that is not a key, a cycle or a refused read decides nothing", async () => {
    await expect(decide(V3_UNDER_BAD_PARENT)).rejects.toBeInstanceOf(UndecidedError);
    await expect(decide(V3_IN_CYCLE)).rejects.toBeInstanceOf(UndecidedError);
    await expect(decide(V3_UNDER_MISSING)).rejects.toBeInstanceOf(DispatchRefusedError);
  });

  test("the answer carries the rule's facts and no text of the ask or its answer", async () => {
    const decision = await decide(V3_ON_UNLISTED);
    expect(Object.keys(decision).sort()).toEqual(
      [
        "ask",
        "decision",
        "issue",
        "now_listed",
        "opened_at",
        "opening_listed",
        "opening_root",
        "root_now",
        "rule_version",
      ].sort()
    );
    expect(decision.rule_version).toBe(RESERVE_RULE_VERSION);
    expect(JSON.stringify(decision)).not.toContain("his answer");
    expect(JSON.stringify(decision)).not.toContain("own words");
  });
});

describe("decideAsk against the registration", () => {
  test("a registration that names a newer rule version decides nothing and reads no ask", async () => {
    markdown = `${VERSIONS}\n\n## Version ${RESERVE_RULE_VERSION + 1}: a later rule`;
    await expect(decide(V3_UNDER_LISTED)).rejects.toBeInstanceOf(UndecidedError);
    expect(seen.some((path) => path.startsWith("/api/v1/asks/"))).toBe(false);
  });

  test("a registration whose text names no rule version decides nothing", async () => {
    markdown = "# The reserve\n\n### Version 3 is named in a lower heading only";
    await expect(decide(V3_UNDER_LISTED)).rejects.toBeInstanceOf(UndecidedError);
  });

  test("a list whose bytes are not the registered ones decides nothing", async () => {
    await expect(decideAsk(V3_UNDER_LISTED, config, "0".repeat(64))).rejects.toBeInstanceOf(
      UndecidedError
    );
  });
});

describe("main", () => {
  test("a refusal exits 2, which a scorer takes as held", async () => {
    markdown = `${VERSIONS}\n\n## Version ${RESERVE_RULE_VERSION + 1}: a later rule`;
    expect(await main(["--ask", `dispatch://ACME-12/ask/${V3_UNDER_LISTED}`])).toBe(2);
  });

  test("the real command takes only the registered list, which the fake does not serve", async () => {
    expect(await main(["--ask", V3_UNDER_LISTED])).toBe(2);
  });

  test("an argument that names no ask exits 2", async () => {
    expect(await main(["--ask", "not-an-ask"])).toBe(2);
    expect(await main([])).toBe(2);
  });
});

describe("the small readers", () => {
  test("askIdOf takes a bare id or a dispatch reference, and nothing else", () => {
    expect(askIdOf(V2_ON_PICKED)).toBe(V2_ON_PICKED);
    expect(askIdOf(`dispatch://ACME-11/ask/${V2_ON_PICKED}`)).toBe(V2_ON_PICKED);
    expect(askIdOf(`dispatch://ACME-11/comment/${V2_ON_PICKED}`)).toBeNull();
  });

  test("ruleVersionsOf reads only second-level Version headings", () => {
    expect(ruleVersionsOf(VERSIONS)).toEqual([2, 1, 3]);
    expect(ruleVersionsOf("### Version 9\n\n## Versions 8\n\n## Version 4: x")).toEqual([4]);
  });

  test("listOf refuses bytes with another digest, and every refusal is a CannotRunError", () => {
    expect([...listOf(LIST, LIST_SHA256)]).toEqual(["ACME-2", "ACME-4"]);
    expect(() => listOf(`${LIST}ACME-5\n`, LIST_SHA256)).toThrow(UndecidedError);
    expect(new UndecidedError("x")).toBeInstanceOf(CannotRunError);
  });
});
