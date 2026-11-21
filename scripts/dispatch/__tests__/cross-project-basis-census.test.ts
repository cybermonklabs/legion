import { afterEach, describe, expect, test } from "bun:test";
import { writeFile } from "node:fs/promises";
import { join } from "node:path";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import fixture from "../__fixtures__/cross-project-basis-census.json" with { type: "json" };
import {
  type CensusReport,
  censusReport,
  crossProjectAsks,
  eventLookups,
  gateOpenings,
  type LogEvent,
  OPENED_LINE,
} from "../cross-project-basis-census.ts";

/** The whole event log of the scratch Dispatch the fixture's `about` describes. */
const events = fixture.events as unknown as readonly LogEvent[];
const script = join(import.meta.dir, "..", "cross-project-basis-census.ts");
const makeDirectory = temporaryDirectories("cross-project-basis-census");
const servers: Array<{ stop: (force?: boolean) => void }> = [];

afterEach(() => {
  for (const server of servers.splice(0)) server.stop(true);
});

/** The projects the census reads on an end-to-end run's log: the roots' and the ancestors'. */
const projects = ["LEGION", "OTHR"];

/** The fixture's event of id `id`. */
function event(id: number): LogEvent {
  const found = events.find((each) => each.id === id);
  if (found === undefined) throw new Error(`the fixture holds no event ${id}`);
  return found;
}

/** The spec-approval asks the fixture's four roots open, by event id. */
const ASK_UNDER_OTHER_PROJECT = { event: 29, id: "96a3d178-76aa-4c5f-a067-b5a0afca7405" };
const ASK_UNDER_SAME_PROJECT = { event: 30, id: "d76b15dd-132c-4dfb-a8b2-80ae6a3b87ae" };
const ASK_UNDER_UNAPPROVED = { event: 31, id: "0257c47b-48bf-452b-baaf-9d64155fab0c" };
const ASK_UNDER_OLDER_VERSION = { event: 32, id: "810694b8-d5da-4d0f-b42e-f8f35de9ed55" };
/** The two messages in basisOpenedMessage's format: on LEGION-2 naming OTHR-1, and on LEGION-3
 * naming LEGION-1. */
const OPENING_ACROSS = { event: 33, id: "e521aec5-6cf1-48c4-9584-15aaa5c294ac" };
const OPENING_WITHIN = { event: 34, id: "481c08b7-5c28-48cd-833a-5d08b2165c71" };

/** The census's whole answer on the fixture: the first ask and the first message, and nothing
 * else. */
const expected: CensusReport = {
  asks: [
    {
      issue: "LEGION-2",
      ask: ASK_UNDER_OTHER_PROJECT.id,
      openedAt: event(ASK_UNDER_OTHER_PROJECT.event).created_at,
      version: 1,
      ancestor: "OTHR-1",
      ancestorVersion: 1,
    },
  ],
  openings: [
    {
      issue: "LEGION-2",
      message: OPENING_ACROSS.id,
      createdAt: event(OPENING_ACROSS.event).created_at,
      ancestor: "OTHR-1",
    },
  ],
  pairs: [],
};

describe("the census of gates opened across projects", () => {
  test("lists the ask under an approved issue of another project and the opening that names one, and nothing else", () => {
    expect(censusReport(events, { projects, pairs: [] })).toEqual(expected);
  });

  test("the two pure functions list the same, given the named projects' events and the log's lookups", () => {
    const named = events.filter(
      (each) => each.issue_key !== null && projects.some((p) => each.issue_key?.startsWith(`${p}-`))
    );
    expect(crossProjectAsks(named, eventLookups(events))).toEqual([...expected.asks]);
    expect(gateOpenings(named)).toEqual([...expected.openings]);
  });

  test("the fixture holds each case the census tells apart, and each pair says why its ask is not listed", () => {
    const report = censusReport(events, {
      projects,
      pairs: [
        { root: "LEGION-2", ancestor: "OTHR-1" },
        { root: "LEGION-3", ancestor: "LEGION-1" },
        { root: "LEGION-4", ancestor: "ACME-1" },
        { root: "LEGION-5", ancestor: "OTHR-2" },
      ],
    });
    const asked = (ask: { event: number; id: string }) => ({
      ask: ask.id,
      openedAt: event(ask.event).created_at,
      version: 1,
      onChain: true,
    });
    expect(report.pairs).toEqual([
      // Listed: a person approved OTHR-1's latest version, the person then approved the root's own
      // spec, and the gate's opening names OTHR-1.
      {
        root: "LEGION-2",
        ancestor: "OTHR-1",
        asks: [
          {
            ...asked(ASK_UNDER_OTHER_PROJECT),
            ancestorLatestVersion: 1,
            ancestorReview: { state: "approved", version: 1, by: "user:alice" },
            answer: { by: "user:alice", selected: ["Approve"], at: event(35).created_at },
            resolution: null,
          },
        ],
        openings: [expected.openings[0]],
      },
      // Not listed: LEGION-1 holds a person's approval of its latest version, and it is of the
      // root's own project. Its opening is in the same format, and is not listed either.
      {
        root: "LEGION-3",
        ancestor: "LEGION-1",
        asks: [
          {
            ...asked(ASK_UNDER_SAME_PROJECT),
            ancestorLatestVersion: 1,
            ancestorReview: { state: "approved", version: 1, by: "user:alice" },
            answer: null,
            resolution: null,
          },
        ],
        openings: [
          {
            issue: "LEGION-3",
            message: OPENING_WITHIN.id,
            createdAt: event(OPENING_WITHIN.event).created_at,
            ancestor: "LEGION-1",
          },
        ],
      },
      // Not listed: no person approved ACME-1; its one review asks for changes.
      {
        root: "LEGION-4",
        ancestor: "ACME-1",
        asks: [
          {
            ...asked(ASK_UNDER_UNAPPROVED),
            ancestorLatestVersion: 1,
            ancestorReview: { state: "changes_requested", version: 1, by: "user:alice" },
            answer: null,
            resolution: null,
          },
        ],
        openings: [],
      },
      // Not listed: a person approved version 1 of OTHR-2's spec, which stood at version 2.
      {
        root: "LEGION-5",
        ancestor: "OTHR-2",
        asks: [
          {
            ...asked(ASK_UNDER_OLDER_VERSION),
            ancestorLatestVersion: 2,
            ancestorReview: { state: "approved", version: 1, by: "user:alice" },
            answer: null,
            resolution: null,
          },
        ],
        openings: [],
      },
    ]);
  });

  test("reads an opening's ancestor from its line, not from its first spec reference", () => {
    const body = event(OPENING_ACROSS.event).payload.body as string;
    const lines = body.split("\n");
    expect(lines[0]).toBe(OPENED_LINE);
    expect(body.indexOf("dispatch://LEGION-2/spec")).toBeLessThan(
      body.indexOf("dispatch://OTHR-1")
    );
    expect(lines).toContain("- The ancestor is OTHR-1.");
  });

  test("an ask whose chain the log cannot read is refused, naming the issue", () => {
    const withoutAncestor = events.filter((each) => each.issue_key !== "OTHR-1");
    expect(() => crossProjectAsks(events, eventLookups(withoutAncestor))).toThrow(
      "the log holds no issue event of OTHR-1"
    );
  });
});

/** Runs the census with `args`, its Dispatch at `environment`. */
async function spawnCensus(
  args: readonly string[],
  environment: Record<string, string> = {}
): Promise<{ stdout: string; stderr: string; exitCode: number }> {
  // DISPATCH_TOKEN_FILE outranks DISPATCH_TOKEN, so a pane's own token file would reach the stub.
  const { DISPATCH_TOKEN_FILE: _paneTokenFile, ...inherited } = process.env;
  const census = Bun.spawn(["bun", script, ...args], {
    env: { ...inherited, ...environment },
    stdout: "pipe",
    stderr: "pipe",
  });
  const [stdout, stderr, exitCode] = await Promise.all([
    new Response(census.stdout).text(),
    new Response(census.stderr).text(),
    census.exited,
  ]);
  return { stdout, stderr, exitCode };
}

/** The fixture as the saved JSONL log c7 keeps, at a path of its own. */
async function savedLog(): Promise<string> {
  const path = join(makeDirectory(), "dispatch-events.jsonl");
  await writeFile(path, `${events.map((each) => JSON.stringify(each)).join("\n")}\n`);
  return path;
}

/** A stub Dispatch serving the fixture's log as the event log route and each issue's events route
 * page it, recording each event log query. */
function stubDispatch(logQueries: string[]): string {
  const server = Bun.serve({
    hostname: "127.0.0.1",
    port: 0,
    fetch(request) {
      const url = new URL(request.url);
      const after = Number(url.searchParams.get("after") ?? "0");
      const limit = Number(url.searchParams.get("limit") ?? "1000");
      if (url.pathname === "/api/v1/events/log") {
        logQueries.push(url.search);
        return Response.json(events.filter((each) => each.id > after).slice(0, limit));
      }
      const issue = /^\/api\/v1\/issues\/([A-Z][A-Z0-9]+-\d+)\/events$/.exec(url.pathname)?.[1];
      if (issue !== undefined) {
        return Response.json(
          events
            .filter((each) => each.issue_key === issue && (each.seq ?? 0) > after)
            .slice(0, limit)
        );
      }
      return new Response("not found", { status: 404 });
    },
  });
  servers.push(server);
  return `http://127.0.0.1:${server.port}`;
}

describe("the census command", () => {
  test("--events-file --json prints the report the functions give, as one object and nothing else", async () => {
    const run = await spawnCensus([
      "--events-file",
      await savedLog(),
      "--project",
      "LEGION",
      "--project",
      "OTHR",
      "--json",
    ]);
    expect(run.stderr).toBe("");
    expect(run.exitCode).toBe(0);
    expect(run.stdout.split("\n").filter((line) => line !== "")).toHaveLength(1);
    const report = JSON.parse(run.stdout) as CensusReport;
    expect(Object.keys(report)).toEqual(["asks", "openings", "pairs"]);
    expect(report).toEqual(expected);
  });

  test("--from and --to narrow a saved log, while its whole history still reads each chain", async () => {
    const run = await spawnCensus([
      "--events-file",
      await savedLog(),
      "--project",
      "LEGION",
      "--from",
      event(ASK_UNDER_OTHER_PROJECT.event).created_at,
      "--to",
      event(OPENING_ACROSS.event).created_at,
      "--json",
    ]);
    expect(run.stderr).toBe("");
    expect(run.exitCode).toBe(0);
    expect(JSON.parse(run.stdout)).toEqual({ ...expected, openings: [] });
  });

  test("without --events-file it pages the event log from the first event at or after --from, and reads each chain's history", async () => {
    const logQueries: string[] = [];
    const url = stubDispatch(logQueries);
    const run = await spawnCensus(
      [
        "--project",
        "LEGION",
        "--project",
        "OTHR",
        "--from",
        event(ASK_UNDER_OTHER_PROJECT.event).created_at,
        "--to",
        "2026-11-22T00:00:00Z",
        "--pair",
        "LEGION-2=OTHR-1",
        "--json",
      ],
      { DISPATCH_URL: url, DISPATCH_TOKEN: "token" }
    );
    expect(run.stderr).toBe("");
    expect(run.exitCode).toBe(0);
    const report = JSON.parse(run.stdout) as CensusReport;
    // The approvals and the parents all precede the window, and the issues' own histories hold them.
    expect(report.asks).toEqual(expected.asks);
    expect(report.openings).toEqual(expected.openings);
    expect(report.pairs.map((pair) => pair.asks.map((ask) => ask.answer?.selected))).toEqual([
      [["Approve"]],
    ]);
    // The window's pages start after the event before the first one at or after --from.
    expect(logQueries.find((query) => query.endsWith("limit=1000"))).toBe(
      `?after=${ASK_UNDER_OTHER_PROJECT.event - 1}&limit=1000`
    );
  });

  test("prints a table of each list without --json", async () => {
    const run = await spawnCensus([
      "--events-file",
      await savedLog(),
      "--project",
      "LEGION",
      "--pair",
      "LEGION-5=OTHR-2",
    ]);
    expect(run.exitCode).toBe(0);
    expect(run.stdout).toContain(
      "Spec-approval asks on a root under an approved issue of another project"
    );
    expect(run.stdout).toContain(ASK_UNDER_OTHER_PROJECT.id);
    expect(run.stdout).toContain("Design gates opened on an ancestor of another project");
    expect(run.stdout).toContain(OPENING_ACROSS.id);
    expect(run.stdout).toContain("LEGION-5 under OTHR-2");
    expect(run.stdout).toContain(ASK_UNDER_OLDER_VERSION.id);
  });

  test("refuses an invocation it cannot run, naming why", async () => {
    const log = await savedLog();
    const refusals: Array<[readonly string[], string]> = [
      [["--events-file", log], "--project is required: name each project to read"],
      [["--events-file", log, "--project", "acme"], "--project acme is not a project key"],
      [["--project", "LEGION"], "--from and --to are required without --events-file"],
      [
        ["--project", "LEGION", "--from", "2026-11-22T00:00:00Z", "--to", "2026-11-21T00:00:00Z"],
        "--from must be before --to",
      ],
      [
        ["--events-file", log, "--project", "LEGION", "--pair", "LEGION-2"],
        "--pair takes ROOT=ANCESTOR, two issue keys: LEGION-2",
      ],
      [["--events-file", log, "--project", "LEGION", "--json=true"], "--json"],
    ];
    const answered = await Promise.all(
      refusals.map(async ([args, refusal]) => ({ refusal, refused: await spawnCensus(args) }))
    );
    for (const { refusal, refused } of answered) {
      expect(refused.stderr).toContain(refusal);
      expect(refused.exitCode).toBe(1);
      expect(refused.stdout).toBe("");
    }
  });
});
