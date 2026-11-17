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
 * the four reads it makes in the shapes the real routes answer: the registration document's text,
 * one ask (`{ask, answers, ...}`), each issue with its `parent`, and version 3's list as bytes.
 * Its trees use a neutral project key: ACME-1 and ACME-2 are roots version 2's hash picks, ACME-3
 * and ACME-4 roots it does not, and the list holds ACME-2 and ACME-4.
 */

const TOKEN = "fake-bearer";
const LIST = "ACME-2\nACME-4\n";
const LIST_SHA256 = createHash("sha256").update(LIST, "utf8").digest("hex");

const VERSIONS = [
  "# The reserve",
  "## Version 2: the tree is the unit",
  "## Version 1 released",
  "## Version 3: hold only the trees that can give the final test",
  "### The rule",
].join("\n\n");

const PARENTS: Record<string, string | null> = {
  "ACME-1": null,
  "ACME-2": null,
  "ACME-3": null,
  "ACME-4": null,
  "ACME-11": "ACME-1",
  "ACME-12": "ACME-4",
  "ACME-20": "ACME-21",
  "ACME-21": "ACME-20",
  "ACME-30": "ACME-99",
};

const BEFORE_V2 = "2026-11-15T12:00:00Z";
const V2 = "2026-11-17T12:00:00Z";
const V3 = "2026-11-18T06:00:00Z";

/** Each ask the fake serves: its issue and its first opening, by an id of the ask's shape. */
const ASKS: Record<string, { issue: string | null; opened: string }> = {};
function askFor(issue: string | null, opened: string): string {
  const id = `00000000-0000-4000-8000-${String(Object.keys(ASKS).length + 1).padStart(12, "0")}`;
  ASKS[id] = { issue, opened };
  return id;
}
const BEFORE_V2_ON_PICKED = askFor("ACME-11", BEFORE_V2);
const V2_ON_PICKED = askFor("ACME-11", V2);
const V2_ON_PICKED_LISTED = askFor("ACME-2", V2);
const V2_ON_UNPICKED = askFor("ACME-3", V2);
const V3_UNDER_LISTED = askFor("ACME-12", V3);
const V3_ON_UNLISTED = askFor("ACME-3", V3);
const V3_UNDER_UNLISTED = askFor("ACME-11", V3);
const V3_ON_NO_ISSUE = askFor(null, V3);
const V3_IN_CYCLE = askFor("ACME-20", V3);
const V3_UNDER_MISSING = askFor("ACME-30", V3);

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
      const path = new URL(request.url).pathname;
      seen.push(path);
      if (request.headers.get("authorization") !== `Bearer ${TOKEN}`) {
        return new Response("no bearer", { status: 401 });
      }
      if (path === documentPath) return Response.json({ markdown, token: "t", version: 21 });
      if (path === listPath)
        return new Response(LIST, { headers: { "content-type": "text/plain" } });
      const ask = /^\/api\/v1\/asks\/([^/]+)$/.exec(path)?.[1];
      const known = ask === undefined ? undefined : ASKS[ask];
      if (ask !== undefined && known !== undefined) {
        return Response.json({
          ask: {
            id: ask,
            issue_key: known.issue,
            created_at: known.opened,
            question: "the request's own words",
          },
          answers: [{ text: "his answer" }],
        });
      }
      const issue = /^\/api\/v1\/issues\/([^/]+)$/.exec(path)?.[1];
      if (issue !== undefined && Object.hasOwn(PARENTS, issue)) {
        return Response.json({ key: issue, parent: PARENTS[issue] });
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

describe("decideAsk", () => {
  test("from version 3's cut an ask is kept only when its root now is in the list", async () => {
    const under = await decideAsk(V3_UNDER_LISTED, config, LIST_SHA256);
    expect(under).toMatchObject({
      issue: "ACME-12",
      root: "ACME-4",
      listed: true,
      decision: "kept",
    });
    const on = await decideAsk(V3_ON_UNLISTED, config, LIST_SHA256);
    expect(on).toMatchObject({ root: "ACME-3", listed: false, decision: "held" });
    // Version 2's hash picks no root from version 3's cut: an unlisted root is held either way.
    const picked = await decideAsk(V3_UNDER_UNLISTED, config, LIST_SHA256);
    expect(picked).toMatchObject({ root: "ACME-1", listed: false, decision: "held" });
  });

  test("before version 3's cut version 2's hash holds, and a listed root releases", async () => {
    expect((await decideAsk(V2_ON_PICKED, config, LIST_SHA256)).decision).toBe("held");
    expect((await decideAsk(V2_ON_PICKED_LISTED, config, LIST_SHA256)).decision).toBe("kept");
    expect((await decideAsk(V2_ON_UNPICKED, config, LIST_SHA256)).decision).toBe("kept");
  });

  test("an ask that opened before version 2's cut, or on no issue, is kept", async () => {
    expect((await decideAsk(BEFORE_V2_ON_PICKED, config, LIST_SHA256)).decision).toBe("kept");
    const loose = await decideAsk(V3_ON_NO_ISSUE, config, LIST_SHA256);
    expect(loose).toMatchObject({ issue: null, root: null, listed: null, decision: "kept" });
    expect(seen.some((path) => path.startsWith("/api/v1/issues/"))).toBe(false);
  });

  test("the answer carries the rule's facts and no text of the ask or its answer", async () => {
    const decision = await decideAsk(V3_ON_UNLISTED, config, LIST_SHA256);
    expect(Object.keys(decision).sort()).toEqual(
      ["ask", "decision", "issue", "listed", "opened_at", "root", "rule_version"].sort()
    );
    expect(decision.rule_version).toBe(RESERVE_RULE_VERSION);
    expect(JSON.stringify(decision)).not.toContain("his answer");
    expect(JSON.stringify(decision)).not.toContain("own words");
  });

  test("a registration that names a newer rule version decides nothing and reads no ask", async () => {
    markdown = `${VERSIONS}\n\n## Version ${RESERVE_RULE_VERSION + 1}: a later rule`;
    await expect(decideAsk(V3_UNDER_LISTED, config, LIST_SHA256)).rejects.toBeInstanceOf(
      UndecidedError
    );
    expect(seen.some((path) => path.startsWith("/api/v1/asks/"))).toBe(false);
  });

  test("a registration whose text names no rule version decides nothing", async () => {
    markdown = "# The reserve\n\n### Version 3 is mentioned in a lower heading only";
    await expect(decideAsk(V3_UNDER_LISTED, config, LIST_SHA256)).rejects.toBeInstanceOf(
      UndecidedError
    );
  });

  test("a list whose bytes are not the registered ones decides nothing", async () => {
    await expect(decideAsk(V3_UNDER_LISTED, config, "0".repeat(64))).rejects.toBeInstanceOf(
      UndecidedError
    );
  });

  test("a root that cannot be named decides nothing, so the scorer holds the ask", async () => {
    await expect(decideAsk(V3_IN_CYCLE, config, LIST_SHA256)).rejects.toBeInstanceOf(
      UndecidedError
    );
    await expect(decideAsk(V3_UNDER_MISSING, config, LIST_SHA256)).rejects.toBeInstanceOf(
      DispatchRefusedError
    );
  });
});

describe("main", () => {
  test("a refusal exits 2, which a scorer takes as held", async () => {
    markdown = `${VERSIONS}\n\n## Version ${RESERVE_RULE_VERSION + 1}: a later rule`;
    expect(await main(["--ask", `dispatch://ACME-12/ask/${V3_UNDER_LISTED}`])).toBe(2);
  });

  test("the registered list's digest is not the fake's, so the real command decides nothing", async () => {
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
