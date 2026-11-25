import { afterAll, beforeEach, describe, expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import type { IssueAdvance, Priorities } from "@legion/contracts";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import { runDispatchCli } from "../dispatch-cli";
import { commandFlags, fieldFlag } from "../dispatch-command";
import { resetAdviceMemory } from "../dispatch-execute";
import { asksWhoami, WHOAMI_NO_CUT } from "./whoami-stand-in";

// The priority surface of the `dispatch` command: the relation an issue states (`--advance`,
// repeated, and `--clear-advances`, both derived from the one `advances` field of the tool
// specs), the read that lists every priority with the work under it, and the two faults the same
// read answers. One fake Dispatch serves the whole file, as `dispatch-cli.test.ts` does, and each
// case drives the real CLI through `runDispatchCli` and reads what it sent and what it printed.

interface Recorded {
  readonly method: string;
  readonly path: string;
  readonly search: string;
  readonly body: unknown;
}

const requests: Recorded[] = [];
const makeDirectory = temporaryDirectories("dispatch-priorities");
/** The Dispatch bearer each command is handed. */
const TOKEN = fixtureSecret("dsp");

const ana = { kind: "user", id: "ana@example.com" };

/** Every lifecycle status with no issue standing in it, as the server answers a priority's work:
 *  it writes each status and 0 where none stands there, so a reader adds the columns it draws. */
const noneStanding = {
  triage: 0,
  icebox: 0,
  backlog: 0,
  todo: 0,
  in_progress: 0,
  testing: 0,
  needs_review: 0,
  retro: 0,
  done: 0,
};

/** The body `GET /api/v1/priorities` answers in this file: two live priorities, one with work
 *  under it and one with none, and one issue in each fault list. */
const recorded: Priorities = {
  priorities: [
    {
      id: "ship-the-daemon",
      title: "Ship the daemon",
      description: "Every tree runs under the daemon.",
      owner: "ana@example.com",
      rank: "m",
      state: "live",
      made_at: "2026-11-01T00:00:00Z",
      made_by: ana,
      work: {
        by_status: { ...noneStanding, backlog: 1, todo: 2, in_progress: 3 },
        claimed: 2,
        open_asks: 1,
        closed_last_7_days: 4,
        newest_event_at: "2026-11-11T09:00:00Z",
      },
    },
    {
      id: "cut-the-latency",
      title: "Cut the latency",
      description: "",
      owner: "",
      rank: "s",
      state: "live",
      made_at: "2026-11-02T00:00:00Z",
      made_by: ana,
      work: {
        by_status: { ...noneStanding },
        claimed: 0,
        open_asks: 0,
        closed_last_7_days: 0,
        newest_event_at: null,
      },
    },
  ],
  work_under_no_priority: {
    count: 2,
    by_project: { CORE: 1, DSP: 1 },
    issues: [
      {
        key: "CORE-7",
        title: "Rotate the broker keys",
        project: "CORE",
        status: "in_progress",
        assignee: "ana@example.com",
        claim: null,
        changed_at: "2026-11-10T08:00:00Z",
        advances: [],
        moved_at: null,
        moved_by: null,
      },
    ],
  },
  priority_work_parked: {
    count: 2,
    by_priority: { "ship-the-daemon": 2, "legion-first": 1 },
    issues: [
      {
        key: "DSP-9",
        title: "Backfill the index",
        project: "DSP",
        status: "backlog",
        assignee: null,
        claim: null,
        changed_at: "2026-10-02T00:00:00Z",
        advances: [
          { id: "legion-first", inherited: true },
          { id: "ship-the-daemon", inherited: false },
        ],
        moved_at: "2026-10-01T00:00:00Z",
        moved_by: ana,
      },
      // In the icebox, so the row names the status it was moved to rather than one word for
      // both (INFRA-2947, Verification v73).
      {
        key: "DSP-10",
        title: "Retire the old ledger",
        project: "DSP",
        status: "icebox",
        assignee: null,
        claim: null,
        changed_at: "2026-10-04T00:00:00Z",
        advances: [{ id: "ship-the-daemon", inherited: false }],
        moved_at: "2026-10-03T00:00:00Z",
        moved_by: ana,
      },
    ],
  },
};

/** The sentence a Dispatch holding no live priority answers, written by the server
 *  (`priorityNoneWritten` in `internal/dispatch/api/priority_queries.go`) and printed as it came. */
const NONE_WRITTEN =
  "No priority is live now, so the list is empty. Write one, then say which work advances it.";

/** What the priorities route answers next: the recorded body, or the empty deployment's. */
let priorities: Priorities = recorded;

/** DSP-1's own relations, which a PATCH replaces; `legion-first` comes from the issue above it,
 *  so no write of DSP-1 states or retracts it. */
let ownAdvances = ["ship-the-daemon"];
const inherited: IssueAdvance = { id: "legion-first", inherited: true };
const advancesOf = (): IssueAdvance[] => [
  ...ownAdvances.map((id) => ({ id, inherited: false })),
  inherited,
];

/** What the next PATCH of DSP-1 answers in place of the issue, when a case sets it: Dispatch's
 *  refusal of the write, with the code and the sentence the server writes. */
let patchRefusal: { readonly code: string; readonly error: string } | undefined;

/** What the next read of DSP-1 answers in place of the fields `issueBody` writes, when a case
 *  sets it: a field set to undefined is left out of the answer. */
let readFields: Record<string, unknown> = {};

/** The sentence the server refuses a write of DSP-1's advances with when the write carries no
 *  revision and moves two relations, as `refuseAdvancesRevision`
 *  (internal/dispatch/api/issue_advances.go) writes it, naming revision 8, the one another
 *  session's write left after the command's read. */
const SET_WITH_NO_REVISION_REFUSED =
  "this write changes 2 of the issue's advances relations and carries no revision: a write with no revision asserts or retracts one relation at a time, and a whole-set write carries the revision the read it is answering returned, which is 8 here";

/** What the command adds to that refusal: how the caller answers it, with the revision it names. */
const READ_THEN_SEND_8 =
  "read the issue with dispatch read --issue DSP-1, then send the whole set you mean with --advances-revision 8";

/** DSP-1 as every read of it answers: the fields `dispatch read` insists on, and the two
 *  priority fields every issue read carries. */
function issueBody() {
  return {
    key: "DSP-1",
    project: "DSP",
    title: "Native workspace",
    status: "in_progress",
    priority: null,
    assignee: null,
    claim: null,
    labels: [],
    components: { mode: "inherit", ids: [], inherited_from: null, unknown: [] },
    route: null,
    parent: null,
    blocked_by: [],
    external_links: [],
    artifacts: [],
    open_asks: [],
    repo: null,
    resolved_repo: null,
    last_seq: 0,
    advances: advancesOf(),
    advances_revision: 7,
  };
}

const server = Bun.serve({
  port: 0,
  async fetch(request) {
    const { pathname, search, searchParams } = new URL(request.url);
    // Every read command reads whoami first (`whoami-stand-in.ts`); it is not recorded.
    if (asksWhoami(pathname)) return Response.json(WHOAMI_NO_CUT);
    const text = await request.text();
    const body = text === "" ? undefined : JSON.parse(text);
    requests.push({ method: request.method, path: pathname, search, body });
    if (pathname === "/api/v1/priorities" && request.method === "GET") {
      // The route narrows its counts by the projects the request repeated; this fake answers the
      // one recorded body whatever it was asked, and the test reads the query it sent.
      searchParams.getAll("project");
      return Response.json(priorities);
    }
    if (pathname === "/api/v1/issues" && request.method === "POST") {
      const advances = (body.advances ?? []) as string[];
      return Response.json(
        {
          ...issueBody(),
          key: "DSP-12",
          title: body.title,
          advances: advances.map((id) => ({ id, inherited: false })),
        },
        { status: 201 }
      );
    }
    if (pathname === "/api/v1/issues/DSP-1/messages" && request.method === "POST") {
      return Response.json({ id: "message-1", issue_key: "DSP-1" });
    }
    if (pathname === "/api/v1/issues/DSP-1" && request.method === "PATCH") {
      if (patchRefusal !== undefined) return Response.json(patchRefusal, { status: 409 });
      if (body.advances !== undefined) ownAdvances = body.advances as string[];
      return Response.json(issueBody());
    }
    if (pathname === "/api/v1/issues/DSP-1" && request.method === "GET") {
      return Response.json({ ...issueBody(), ...readFields });
    }
    if (pathname === "/api/v1/issues/DSP-1/events") return Response.json([]);
    if (pathname === "/api/v1/issues/DSP-1/coverage") return Response.json({ gaps: [] });
    if (pathname === "/api/v1/issues/DSP-1/references" || pathname === "/api/v1/references") {
      return Response.json({ node: { kind: "issue", id: "DSP-1" }, edges: [], members: [] });
    }
    return Response.json({ error: `unexpected ${request.method} ${pathname}` }, { status: 500 });
  },
});

afterAll(() => server.stop(true));

let stateDir = "";
beforeEach(() => {
  requests.length = 0;
  priorities = recorded;
  ownAdvances = ["ship-the-daemon"];
  patchRefusal = undefined;
  readFields = {};
  stateDir = makeDirectory();
  resetAdviceMemory();
});

async function run(argv: readonly string[]): Promise<{ code: number; stdout: string }> {
  let stdout = "";
  const code = await runDispatchCli(
    argv,
    {
      HOME: stateDir,
      DISPATCH_HOST: "omp",
      DISPATCH_SESSION_ID: "ses-1",
      DISPATCH_URL: `http://127.0.0.1:${server.port}`,
      DISPATCH_TOKEN: TOKEN,
      DISPATCH_STATE_DIR: stateDir,
    },
    {
      stdout: (text) => {
        stdout += text;
      },
      stderr: () => {},
      readText: (path) => readFileSync(path, "utf-8"),
      cwd: stateDir,
    }
  );
  return { code, stdout };
}

/** The one request of `method` to `path`, which must be the only one the run made. */
function sent(method: string, path: string): Recorded {
  const matching = requests.filter((request) => request.method === method && request.path === path);
  expect(
    matching.map((request) => request.search),
    `${method} ${path}`
  ).toHaveLength(1);
  return matching[0] as Recorded;
}

describe("the priorities an issue advances", () => {
  // The flags are derived from the `advances` field of the tool specs, never written by hand:
  // `skill://dispatch` and the Legion skills name these spellings, so a renamed field that
  // changes them fails here rather than in a skill nobody runs.
  test("the generator derives --advance, repeated, and --clear-advances from the field", () => {
    for (const tool of ["dispatch_issue", "dispatch_issue_update"] as const) {
      expect(fieldFlag(tool, "advances"), tool).toBe("--advance");
    }
    expect(commandFlags("dispatch_issue")).toContain("--advance");
    expect(commandFlags("dispatch_issue")).toContain("--clear-advances");
    expect(commandFlags("dispatch_issue_update")).toContain("--advance");
    expect(commandFlags("dispatch_issue_update")).toContain("--clear-advances");
    expect(fieldFlag("dispatch_priorities", "project")).toBe("--project");
  });

  test("dispatch issue sends every priority its repeated flag named", async () => {
    const { code, stdout } = await run([
      "issue",
      "--project",
      "DSP",
      "--title",
      "Native workspace",
      "--advance",
      "ship-the-daemon",
      "--advance",
      "cut-the-latency",
    ]);

    expect(code).toBe(0);
    expect(stdout).toContain("Created DSP-12");
    expect(sent("POST", "/api/v1/issues").body).toMatchObject({
      advances: ["ship-the-daemon", "cut-the-latency"],
    });
  });

  test("dispatch issue-update sends the set with no revision when the caller names none", async () => {
    const { code, stdout } = await run([
      "issue-update",
      "--issue",
      "DSP-1",
      "--advance",
      "ship-the-daemon",
      "--advance",
      "cut-the-latency",
    ]);

    expect(code).toBe(0);
    // Only the caller's own read answers a whole set, so the call sends no revision of its own
    // read: the server takes the write one relation at a time and refuses one that moves two
    // (409 ADVANCES_REVISION). It names the relation and nothing else: a key the update did not
    // touch would overwrite what another writer left.
    const patch = sent("PATCH", "/api/v1/issues/DSP-1").body as Record<string, unknown>;
    expect(Object.keys(patch).sort()).toEqual(["actor", "advances"]);
    expect(patch).toMatchObject({ advances: ["ship-the-daemon", "cut-the-latency"] });
    expect(stdout).toContain("DSP-1: advances ship-the-daemon, cut-the-latency");
  });

  // Another session moved the set after the command's read, to revision 8, so the server refuses
  // a write with no revision that moves two relations, naming the revision it holds. The error
  // keeps the server's sentence and says how to answer it.
  test("a refused set says to read the issue and send the set with the revision named", async () => {
    patchRefusal = { code: "ADVANCES_REVISION", error: SET_WITH_NO_REVISION_REFUSED };

    const { code, stdout } = await run([
      "issue-update",
      "--issue",
      "DSP-1",
      "--advance",
      "ship-the-daemon",
      "--advance",
      "cut-the-latency",
    ]);

    expect(code, stdout).toBe(1);
    expect(stdout).toContain(
      `ADVANCES_REVISION: ${SET_WITH_NO_REVISION_REFUSED}; ${READ_THEN_SEND_8}`
    );
  });

  // The reason of a close is posted before the PATCH, so a refused set leaves it on the issue:
  // the error says how to answer the refusal and where the reason already is.
  test("a refused set after the closing reason names the reason's message too", async () => {
    patchRefusal = { code: "ADVANCES_REVISION", error: SET_WITH_NO_REVISION_REFUSED };

    const { code, stdout } = await run([
      "issue-update",
      "--issue",
      "DSP-1",
      "--status",
      "done",
      "--reason",
      "Shipped.",
      "--advance",
      "ship-the-daemon",
      "--advance",
      "cut-the-latency",
    ]);

    expect(code, stdout).toBe(1);
    expect(stdout).toContain(
      `ADVANCES_REVISION: ${SET_WITH_NO_REVISION_REFUSED}; ${READ_THEN_SEND_8}; the reason already landed as message message-1 `
    );
  });

  // Only that refusal tells the caller to send a revision: any other reads as Dispatch wrote it.
  test("another refusal of the update reads as Dispatch wrote it", async () => {
    patchRefusal = { code: "ISSUE_CLOSED", error: "issue is closed" };

    const { code, stdout } = await run([
      "issue-update",
      "--issue",
      "DSP-1",
      "--advance",
      "ship-the-daemon",
      "--advance",
      "cut-the-latency",
    ]);

    expect(code, stdout).toBe(1);
    expect(stdout).toBe("ISSUE_CLOSED: issue is closed\n");
  });

  // A set this read did not answer is refused 409 ADVANCES_REVISION naming the revision the
  // server holds. The agent answers that refusal with the revision it named, which the call
  // sends as it was named.
  test("dispatch issue-update sends the revision the caller named", async () => {
    expect(commandFlags("dispatch_issue_update")).toContain("--advances-revision");
    expect(commandFlags("dispatch_issue")).not.toContain("--advances-revision");

    const { code, stdout } = await run([
      "issue-update",
      "--issue",
      "DSP-1",
      "--advance",
      "ship-the-daemon",
      "--advance",
      "cut-the-latency",
      "--advances-revision",
      "9",
    ]);

    expect(code).toBe(0);
    const patch = sent("PATCH", "/api/v1/issues/DSP-1").body as Record<string, unknown>;
    expect(Object.keys(patch).sort()).toEqual(["actor", "advances", "advances_revision"]);
    expect(patch).toMatchObject({
      advances: ["ship-the-daemon", "cut-the-latency"],
      advances_revision: 9,
    });
    expect(stdout).toContain("DSP-1: advances ship-the-daemon, cut-the-latency");
  });

  // One relation moved needs no revision, so a clear of the one relation DSP-1 states sends none.
  test("dispatch issue-update --clear-advances sends the empty list and says so", async () => {
    const { code, stdout } = await run(["issue-update", "--issue", "DSP-1", "--clear-advances"]);

    expect(code).toBe(0);
    const patch = sent("PATCH", "/api/v1/issues/DSP-1").body as Record<string, unknown>;
    expect(Object.keys(patch).sort()).toEqual(["actor", "advances"]);
    expect(patch).toMatchObject({ advances: [] });
    expect(stdout).toContain("DSP-1: advances cleared");
  });

  // The read marks which priorities the issue states itself and which come from the issues above
  // it, so the clear above left the inherited one. A change of two or more priorities carries the
  // revision of the read it answers, so the read prints that revision beside the set, for
  // --advances-revision. A read that carries no revision prints the set alone, never a number the
  // issue does not hold.
  test.each([
    [
      "the revision beside the priorities",
      {},
      "Advances: ship-the-daemon (own), legion-first (inherited); revision 7",
    ],
    [
      "the revision beside no priority",
      { advances: [], advances_revision: 0 },
      "Advances: no priority; revision 0",
    ],
    [
      "the priorities alone when the read carries no revision",
      { advances_revision: undefined },
      "Advances: ship-the-daemon (own), legion-first (inherited)",
    ],
  ] as const)("an issue read prints %s", async (_, fields, line) => {
    readFields = fields;

    const { code, stdout } = await run(["read", "--issue", "DSP-1"]);

    expect(code).toBe(0);
    expect(stdout).toContain(`\n${line}\n`);
  });
});

describe("dispatch priorities", () => {
  test("prints every priority, the work under it, and the two faults", async () => {
    const { code, stdout } = await run(["priorities"]);

    expect(code).toBe(0);
    // No project was named, so the read asks for the whole deployment.
    expect(sent("GET", "/api/v1/priorities").search).toBe("");
    expect(stdout).toContain(
      [
        "2 priorities.",
        "ship-the-daemon · Ship the daemon · owner ana@example.com",
        "  6 open (backlog 1, todo 2, in_progress 3) · 2 claimed · 1 open ask · 4 closed in the last 7 days · newest event 2026-11-11T09:00:00Z",
        "cut-the-latency · Cut the latency · owner nobody",
        "  no open issues · 0 claimed · 0 open asks · 0 closed in the last 7 days · no events",
        "",
        "Work under no priority (2: CORE 1, DSP 1; showing the newest 1):",
        "- CORE-7 [in_progress] Rotate the broker keys · CORE · ana@example.com · changed 2026-11-10T08:00:00Z",
        "",
        "Priority work in backlog or icebox (2: ship-the-daemon 2, legion-first 1):",
        "- DSP-9 [backlog] Backfill the index · DSP · advances legion-first (inherited), ship-the-daemon (own) · moved to backlog 2026-10-01T00:00:00Z by ana@example.com · changed 2026-10-02T00:00:00Z",
        "- DSP-10 [icebox] Retire the old ledger · DSP · advances ship-the-daemon (own) · moved to icebox 2026-10-03T00:00:00Z by ana@example.com · changed 2026-10-04T00:00:00Z",
      ].join("\n")
    );
    // INFRA-2947 check e911206c (Verification v73): no line printed holds the word the person
    // refused for work in backlog or icebox, in any case. The bracket keeps the word itself out
    // of this file, as check 1a3e1452 reads it.
    expect(stdout.split("\n").filter((line) => /(?<![sS])p[a]rk/i.test(line))).toEqual([]);
  });

  test("--project is repeated on the query, once per key", async () => {
    const { code } = await run(["priorities", "--project", "DSP", "--project", "CORE"]);

    expect(code).toBe(0);
    expect(sent("GET", "/api/v1/priorities").search).toBe("?project=DSP&project=CORE");
  });

  test("a deployment that has written no priority prints the server's sentence, exit 0", async () => {
    priorities = {
      priorities: [],
      work_under_no_priority: { count: 0, by_project: {}, issues: [] },
      priority_work_parked: { count: 0, by_priority: {}, issues: [] },
      empty: NONE_WRITTEN,
    };

    const { code, stdout } = await run(["priorities"]);

    expect(code).toBe(0);
    expect(stdout.trim()).toBe(NONE_WRITTEN);
  });
});
