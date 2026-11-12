import { afterAll, beforeEach, expect, test } from "bun:test";
import { bearerCredential } from "../packages/envoy-client/src/bearer";
import { priorityDescription, rowMarker, seedPriorities } from "./priorities-seed";

/** One priority as the fake Dispatch below holds it. */
interface HeldPriority {
  id: string;
  title: string;
  description: string;
  owner: string;
  rank: string;
  state: string;
}

/** One issue as the fake Dispatch below holds it: its status and its own `advances` relations,
 *  with the revision of the newest write to them. */
interface HeldIssue {
  status: string;
  advances: { id: string; inherited: boolean }[];
  revision: number;
}

interface Recorded {
  readonly method: string;
  readonly path: string;
  readonly body: unknown;
}

const DOCUMENT = "dispatch://TBL-1/artifact/priorities-md";

/** The route the seed reads that document's table through, as Dispatch writes it. */
const DOCUMENT_PATH = "/api/v1/issues/TBL-1/artifacts/priorities-md/text";

/**
 * The fixture table. It holds a row whose `Where` cell begins with `DONE`, a row naming a key
 * that is `done`, a row naming a key in `icebox`, a row naming no key at all, a row whose owner
 * cell names several owners, a row carrying a bare key and a `dispatch://` reference, a key no
 * issue carries, and two rows naming keys of two projects.
 */
const TABLE = [
  "# The table a person keeps by hand",
  "",
  "| \\# | Priority | Owner | Where |",
  "| --- | --- | --- | --- |",
  "| 1 | Legion works reliably first | INFRA controller | TEST-1 is implementing |",
  "| 2 | Uninstall the old tracker | CHIEF | DONE: TEST-9 removed it from mise |",
  "| 3 | Ship the gateway | OSSV controller | TEST-2 finished it |",
  "| 4 | Nothing a person asked for is parked | INFRA controller (enforcement), BET controller " +
    "(its own list) | TEST-3 sits in icebox |",
  "| 5 | Hourly self-check against this list | CHIEF | nothing to point at yet |",
  "| 6 | One cross-project view of priorities | INFRA fan | TEST-4, and the spec at " +
    "dispatch://TEST-5/artifact/spec-md |",
  "| 7 | Chase the applications | OSSV controller | TEST-404 names no issue |",
  "| 8 | Rocq and Lean speed work restarted | LINUX controller | OTHER-7 and OTHER-8 |",
  "",
  "INFRA holds every slot, so starting new work means ranking it above something.",
  "",
].join("\n");

/** The same rows, moved: row 8 first and rows 4 and 5 swapped. */
const MOVED_TABLE = (() => {
  const lines = TABLE.split("\n");
  const [one, two, three, four, five, six, seven, eight] = lines.slice(4, 12);
  return [
    ...lines.slice(0, 4),
    eight,
    one,
    two,
    three,
    five,
    four,
    six,
    seven,
    ...lines.slice(12),
  ].join("\n");
})();

const priorities: HeldPriority[] = [];
const issues = new Map<string, HeldIssue>();
const requests: Recorded[] = [];
let document = { markdown: TABLE, version: 1 };
/** Whether the fake refuses a write whose body names no session, as the real server refuses
 *  one from a bearer that proves no session of its own (server.go, bearerSessionActor: 400
 *  ACTOR_KIND "bearer callers require actor.kind session"). The shared token every pod carries
 *  is such a bearer. */
let requireActor = false;

/** The id the server writes for a title, as `priorityIDFromTitle` does: the title as a slug. */
function slug(title: string): string {
  return title
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, "-")
    .replace(/^-|-$/g, "");
}

function issueBody(key: string, issue: HeldIssue): object {
  return {
    key,
    status: issue.status,
    advances: issue.advances,
    advances_revision: issue.revision,
  };
}

/**
 * A Dispatch holding one document, the issues above and the priorities a run writes. It answers
 * the three routes the seed calls and refuses what the real server refuses: a priority id no row
 * carries, a second create of one id, and a whole-set `advances` write whose revision is behind.
 */
const server = Bun.serve({
  port: 0,
  async fetch(request) {
    const { pathname } = new URL(request.url);
    const text = await request.text();
    const body = text === "" ? undefined : JSON.parse(text);
    requests.push({ method: request.method, path: pathname, body });
    const actor = (body as { actor?: { kind?: string; id?: string } } | undefined)?.actor;
    if (requireActor && request.method !== "GET" && (actor?.kind !== "session" || !actor.id)) {
      return Response.json(
        { code: "ACTOR_KIND", error: "bearer callers require actor.kind session" },
        { status: 400 }
      );
    }
    if (request.method === "GET" && pathname === DOCUMENT_PATH) {
      return Response.json({ ...document, token: "sha256:document" });
    }
    const key = /^\/api\/v1\/issues\/([A-Z][A-Z0-9]*-[0-9]+)$/.exec(pathname)?.[1];
    if (key !== undefined) {
      const issue = issues.get(key);
      if (issue === undefined) {
        return Response.json(
          { code: "ISSUE_NOT_FOUND", error: `no issue carries the key ${key}` },
          { status: 404 }
        );
      }
      if (request.method === "GET") return Response.json(issueBody(key, issue));
      if (request.method === "PATCH") {
        const write = body as { advances: string[]; advances_revision?: number };
        for (const id of write.advances) {
          if (!priorities.some((priority) => priority.id === id)) {
            return Response.json(
              { code: "ADVANCES_INPUT", error: `no priority carries the id ${id}` },
              { status: 400 }
            );
          }
        }
        if (write.advances_revision !== undefined && write.advances_revision < issue.revision) {
          return Response.json(
            { code: "ADVANCES_REVISION", error: `the revision is ${issue.revision}` },
            { status: 409 }
          );
        }
        const held = issue.advances.filter((entry) => entry.inherited);
        const changes = Math.abs(issue.advances.length - held.length - write.advances.length);
        issue.advances = [...held, ...write.advances.map((id) => ({ id, inherited: false }))];
        issue.revision += Math.max(changes, 1);
        return Response.json(issueBody(key, issue));
      }
    }
    if (pathname === "/api/v1/priorities") {
      if (request.method === "GET") {
        return Response.json({ priorities: priorities.filter((one) => one.state === "live") });
      }
      if (request.method === "POST") {
        const write = body as { title: string; description: string; owner: string };
        const id = slug(write.title);
        if (priorities.some((priority) => priority.id === id)) {
          return Response.json(
            { code: "PRIORITY_EXISTS", error: `a priority with the id ${id} is already written` },
            { status: 409 }
          );
        }
        const priority = {
          id,
          title: write.title,
          description: write.description,
          owner: write.owner,
          rank: String(priorities.length + 1).padStart(4, "0"),
          state: "live",
        };
        priorities.push(priority);
        return Response.json(priority, { status: 201 });
      }
    }
    return Response.json({ code: "NOT_FOUND", error: pathname }, { status: 404 });
  },
});

afterAll(() => server.stop(true));

beforeEach(() => {
  priorities.length = 0;
  requests.length = 0;
  document = { markdown: TABLE, version: 1 };
  requireActor = false;
  issues.clear();
  for (const [key, status] of [
    ["TEST-1", "in_progress"],
    ["TEST-2", "done"],
    ["TEST-3", "icebox"],
    ["TEST-4", "backlog"],
    ["TEST-5", "todo"],
    ["TEST-9", "done"],
    ["OTHER-7", "in_progress"],
    ["OTHER-8", "needs_review"],
  ]) {
    issues.set(key ?? "", { status: status ?? "", advances: [], revision: 0 });
  }
});

const config = {
  url: `http://127.0.0.1:${server.port}`,
  credential: bearerCredential("token"),
};

/** The priorities each issue advances by a relation of its own, in the order they were written. */
function advanced(key: string): string[] {
  return (issues.get(key)?.advances ?? [])
    .filter((entry) => !entry.inherited)
    .map((entry) => entry.id);
}

/** Every write the fake Dispatch was asked for. */
function writes(): Recorded[] {
  return requests.filter((recorded) => recorded.method !== "GET");
}

test("writes one priority for each row but the DONE one, in the table's order", async () => {
  const report = await seedPriorities(config, DOCUMENT);

  expect(priorities.map((priority) => priority.title)).toEqual([
    "Legion works reliably first",
    "Ship the gateway",
    "Nothing a person asked for is parked",
    "Hourly self-check against this list",
    "One cross-project view of priorities",
    "Chase the applications",
    "Rocq and Lean speed work restarted",
  ]);
  expect(report.rows.map((row) => row.row)).toEqual(["1", "3", "4", "5", "6", "7", "8"]);
  expect(report.passed).toEqual(["2"]);
  expect(report.version).toBe(1);
  expect(report.document).toBe(DOCUMENT);
  // The owner cell verbatim, several owners included.
  expect(priorities[2]?.owner).toBe(
    "INFRA controller (enforcement), BET controller (its own list)"
  );
  expect(priorities[0]?.owner).toBe("INFRA controller");
  // Nothing of the DONE row is read: no priority, and no read of the key it names.
  expect(requests.some((recorded) => recorded.path.endsWith("/TEST-9"))).toBe(false);
});

test("relates every key that is not done, the icebox one included", async () => {
  await seedPriorities(config, DOCUMENT);

  expect(advanced("TEST-1")).toEqual(["legion-works-reliably-first"]);
  expect(advanced("TEST-3")).toEqual(["nothing-a-person-asked-for-is-parked"]);
  expect(advanced("TEST-4")).toEqual(["one-cross-project-view-of-priorities"]);
  // A key inside a `dispatch://` reference is a key the row names.
  expect(advanced("TEST-5")).toEqual(["one-cross-project-view-of-priorities"]);
  // Two projects: the eighth row's keys are another project's.
  expect(advanced("OTHER-7")).toEqual(["rocq-and-lean-speed-work-restarted"]);
  expect(advanced("OTHER-8")).toEqual(["rocq-and-lean-speed-work-restarted"]);
  // The `done` key is related by nothing, and the row naming no key relates nothing.
  expect(advanced("TEST-2")).toEqual([]);
  expect(writes().filter((write) => write.path === "/api/v1/issues/TEST-2")).toEqual([]);
});

test("records the key it did not relate with its status, and the key no issue carries", async () => {
  const report = await seedPriorities(config, DOCUMENT);

  const done = report.rows.find((row) => row.row === "3");
  expect(done?.related).toEqual([]);
  expect(done?.excluded).toEqual([{ key: "TEST-2", reason: "done" }]);
  expect(priorities[1]?.description).toContain("Not advanced: TEST-2 (done).");
  const unknown = report.rows.find((row) => row.row === "7");
  expect(unknown?.related).toEqual([]);
  expect(unknown?.excluded).toEqual([{ key: "TEST-404", reason: "no issue carries this key" }]);
  expect(priorities[5]?.description).toContain(
    "Not advanced: TEST-404 (no issue carries this key)."
  );
  const none = report.rows.find((row) => row.row === "5");
  expect(none?.related).toEqual([]);
  expect(none?.excluded).toEqual([]);
  expect(priorities[3]?.description).toContain("Advances: none.\nNot advanced: none.");
});

test("writes the row's own text, its number and the version it read as the description", async () => {
  await seedPriorities(config, DOCUMENT);

  expect(priorities[4]?.description).toBe(
    [
      "One cross-project view of priorities",
      "",
      "TEST-4, and the spec at dispatch://TEST-5/artifact/spec-md",
      "",
      "---",
      "",
      `Table row 6 of ${DOCUMENT}, read at version 1.`,
      "Advances: TEST-4, TEST-5.",
      "Not advanced: none.",
    ].join("\n")
  );
  expect(rowMarker(priorities[4]?.description ?? "")).toEqual({ row: "6", document: DOCUMENT });
  expect(
    priorityDescription(
      { number: "6", title: "One cross-project view of priorities", owner: "INFRA fan", where: "" },
      DOCUMENT,
      null,
      [],
      []
    )
  ).toContain(`Table row 6 of ${DOCUMENT}, read at version none.`);
});

test("a second run writes no second priority and no second relation", async () => {
  await seedPriorities(config, DOCUMENT);
  const written = priorities.map((priority) => ({ ...priority }));
  requests.length = 0;

  const second = await seedPriorities(config, DOCUMENT);

  expect(writes()).toEqual([]);
  expect(priorities).toEqual(written);
  expect(second.rows.every((row) => !row.written)).toBe(true);
  expect(second.rows.flatMap((row) => row.relations)).toEqual([]);
  // The rows the second run read are the same rows, with the relations they already carry.
  expect(second.rows.map((row) => row.related)).toEqual([
    ["TEST-1"],
    [],
    ["TEST-3"],
    [],
    ["TEST-4", "TEST-5"],
    [],
    ["OTHER-7", "OTHER-8"],
  ]);
});

test("a table whose rows moved keeps the priorities it wrote and their order", async () => {
  await seedPriorities(config, DOCUMENT);
  const order = priorities.map((priority) => priority.id);
  requests.length = 0;
  document = { markdown: MOVED_TABLE, version: 2 };

  const moved = await seedPriorities(config, DOCUMENT);

  expect(writes()).toEqual([]);
  expect(priorities.map((priority) => priority.id)).toEqual(order);
  expect(moved.rows.map((row) => row.row)).toEqual(["8", "1", "3", "5", "4", "6", "7"]);
  expect(moved.rows.every((row) => !row.written)).toBe(true);
});

// The create sends no id, so Dispatch derives one from the title: two titles deriving one id
// would be refused 409 PRIORITY_EXISTS on the second create, with the rows above it already
// written. The run is refused before the first write instead, naming both rows.
test("two rows whose titles Dispatch writes under one id are refused before any write", async () => {
  document = {
    markdown: [
      "| \\# | Priority | Owner | Where |",
      "| --- | --- | --- | --- |",
      "| 1 | Ship the gateway | CHIEF | TEST-1 is implementing |",
      "| 2 | Ship the Gateway! | OSSV controller | nothing to point at yet |",
      "",
    ].join("\n"),
    version: 3,
  };

  await expect(seedPriorities(config, DOCUMENT)).rejects.toThrow(
    "rows 1 and 2 name priorities Dispatch writes under the one id ship-the-gateway: " +
      '"Ship the gateway" and "Ship the Gateway!" differ only where an id does not'
  );
  expect(writes()).toEqual([]);
  expect(priorities).toEqual([]);
});

test("run as a command with the shared bearer, the seed names its session on every write", async () => {
  // The seed is run once, from a session whose Dispatch credential is the shared bearer every
  // pod carries. That bearer proves no session, so Dispatch refuses each write whose body does
  // not name one, and the seed must name the session it runs in (DISPATCH_SESSION_ID).
  requireActor = true;
  const session = "019a1c01-0d7b-7a2a-8e93-4a5b6c7d8e01";
  const env: Record<string, string | undefined> = {
    ...process.env,
    DISPATCH_URL: config.url,
    DISPATCH_TOKEN: "token",
    DISPATCH_SESSION_ID: session,
  };
  delete env.DISPATCH_TOKEN_FILE;
  delete env.ENVOY_CONFIG_FILE;
  const child = Bun.spawn(["bun", `${import.meta.dir}/priorities-seed.ts`, DOCUMENT], {
    env,
    stderr: "pipe",
    stdout: "pipe",
  });
  const [code, stderr] = await Promise.all([child.exited, new Response(child.stderr).text()]);
  expect(stderr).not.toContain("ACTOR_KIND");
  expect(code).toBe(0);
  expect(priorities).toHaveLength(7);
  expect(writes().length).toBeGreaterThan(7);
  for (const write of writes()) {
    expect(write.body).toMatchObject({ actor: { kind: "session", id: session } });
  }
});
