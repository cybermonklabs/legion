import { afterAll, beforeEach, describe, expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import type { IssueAdvance, Priorities } from "@legion/contracts";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import { runDispatchCli } from "../dispatch-cli";
import { commandFlags, fieldFlag } from "../dispatch-command";
import { resetAdviceMemory } from "../dispatch-execute";
import { forgetKeyedLayerMemory } from "../dispatch-http";

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

const ana = { kind: "user", id: "ana@theorem.dev" };

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
      owner: "ana@theorem.dev",
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
        assignee: "ana@theorem.dev",
        claim: null,
        changed_at: "2026-11-10T08:00:00Z",
        priorities: [],
        moved_at: null,
        moved_by: null,
      },
    ],
  },
  priority_work_parked: {
    count: 1,
    by_priority: { "ship-the-daemon": 1 },
    issues: [
      {
        key: "DSP-9",
        title: "Backfill the index",
        project: "DSP",
        status: "backlog",
        assignee: null,
        claim: null,
        changed_at: "2026-10-02T00:00:00Z",
        priorities: ["ship-the-daemon"],
        moved_at: "2026-10-01T00:00:00Z",
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
    if (pathname === "/api/v1/issues/DSP-1" && request.method === "PATCH") {
      if (body.advances !== undefined) ownAdvances = body.advances as string[];
      return Response.json(issueBody());
    }
    if (pathname === "/api/v1/issues/DSP-1" && request.method === "GET") {
      return Response.json(issueBody());
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
  stateDir = makeDirectory();
  resetAdviceMemory();
  forgetKeyedLayerMemory();
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
      DISPATCH_TOKEN: "test-token",
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

  test("dispatch issue-update replaces the set, carrying the revision the read answered", async () => {
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
    // A whole-set write carries the revision of the read it answers, else the server refuses a
    // write that moves two relations (409 ADVANCES_REVISION). It names the relation and nothing
    // else: a key the update did not touch would overwrite what another writer left.
    const patch = sent("PATCH", "/api/v1/issues/DSP-1").body as Record<string, unknown>;
    expect(Object.keys(patch).sort()).toEqual(["actor", "advances", "advances_revision"]);
    expect(patch).toMatchObject({
      advances: ["ship-the-daemon", "cut-the-latency"],
      advances_revision: 7,
    });
    expect(stdout).toContain("DSP-1: advances ship-the-daemon, cut-the-latency");
  });

  // A set this read did not answer is refused 409 ADVANCES_REVISION naming the revision the
  // server holds. The agent answers that refusal with the revision it named, which the call
  // sends in place of the revision of its own read.
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

  test("dispatch issue-update --clear-advances sends the empty list and says so", async () => {
    const { code, stdout } = await run(["issue-update", "--issue", "DSP-1", "--clear-advances"]);

    expect(code).toBe(0);
    expect(sent("PATCH", "/api/v1/issues/DSP-1").body).toMatchObject({
      advances: [],
      advances_revision: 7,
    });
    expect(stdout).toContain("DSP-1: advances cleared");
  });

  // An inherited relation is no write of this issue, so the clear above left it: the read marks
  // which priorities the issue states itself and which come from the issues above it.
  test("an issue read prints every priority the issue serves, each marked", async () => {
    const { code, stdout } = await run(["read", "--issue", "DSP-1"]);

    expect(code).toBe(0);
    expect(stdout).toContain("Advances: ship-the-daemon (own), legion-first (inherited)");
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
        "ship-the-daemon · Ship the daemon · owner ana@theorem.dev",
        "  6 open (backlog 1, todo 2, in_progress 3) · 2 claimed · 1 open ask · 4 closed in the last 7 days · newest event 2026-11-11T09:00:00Z",
        "cut-the-latency · Cut the latency · owner nobody",
        "  no open issues · 0 claimed · 0 open asks · 0 closed in the last 7 days · no events",
        "",
        "Work under no priority (2: CORE 1, DSP 1; showing the newest 1):",
        "- CORE-7 [in_progress] Rotate the broker keys · CORE · ana@theorem.dev · changed 2026-11-10T08:00:00Z",
        "",
        "Priority work parked (1: ship-the-daemon 1):",
        "- DSP-9 [backlog] Backfill the index · DSP · advances ship-the-daemon · parked 2026-10-01T00:00:00Z by ana@theorem.dev · changed 2026-10-02T00:00:00Z",
      ].join("\n")
    );
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
