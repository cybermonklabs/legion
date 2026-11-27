import { afterAll, beforeEach, describe, expect, test } from "bun:test";
import {
  ASK_IS_FOR_A_PERSON,
  ASK_WAITS_ON_THE_ASSIGNEE,
  AskEventPayloadSchema,
  dispatchToolSpecs,
} from "@legion/contracts";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import { runDispatchCli } from "../dispatch-cli";
import { commandFlags } from "../dispatch-command";
import { resetAdviceMemory } from "../dispatch-execute";

// Who an open ask waits on, from the command line and from the texts an agent reads. A person
// hands an ask on through the web app or `PUT /api/v1/asks/{id}/asked-of`, both person-scoped, so
// the half of this the CLI carries is the issue's assignee and nothing else: the command sets
// that, and no command of the whole surface writes `asked_of`. The rest is the two sentences both
// ask descriptions compose from their constants.

const makeDirectory = temporaryDirectories("dispatch-asked-of");

/** The Dispatch bearer each command is handed. */
const TOKEN = fixtureSecret("dsp");

const ASSIGNEE = "bob@example.com";

interface Recorded {
  readonly method: string;
  readonly path: string;
  readonly body: unknown;
}

const requests: Recorded[] = [];

/** DSP-1 as `GET /api/v1/issues/DSP-1` reads it: open, with nobody holding it. */
const ISSUE = {
  key: "DSP-1",
  title: "Native workspace",
  status: "in_progress",
  labels: [],
  priority: null,
  assignee: null,
  route: null,
  parent: null,
  blocked_by: [],
  components: [],
  repo: null,
  resolved_repo: null,
  closed_at: null,
  external_links: [],
  artifacts: [],
};

const server = Bun.serve({
  port: 0,
  async fetch(request) {
    const { pathname } = new URL(request.url);
    const text = await request.text();
    const body: unknown = text === "" ? undefined : JSON.parse(text);
    requests.push({ method: request.method, path: pathname, body });
    if (pathname === "/api/v1/issues/DSP-1" && request.method === "GET") {
      return Response.json(ISSUE);
    }
    if (pathname === "/api/v1/issues/DSP-1" && request.method === "PATCH") {
      const sent = (body ?? {}) as { assignee?: string | null };
      return Response.json({ ...ISSUE, assignee: sent.assignee ?? null });
    }
    return Response.json({ error: `unexpected ${request.method} ${pathname}` }, { status: 500 });
  },
});

afterAll(() => server.stop(true));

let stateDir = "";
beforeEach(() => {
  requests.length = 0;
  stateDir = makeDirectory();
  resetAdviceMemory();
});

async function run(argv: readonly string[]): Promise<{
  code: number;
  stdout: string;
  stderr: string;
}> {
  let stdout = "";
  let stderr = "";
  const code = await runDispatchCli(
    argv,
    {
      HOME: stateDir,
      DISPATCH_URL: `http://127.0.0.1:${server.port}`,
      DISPATCH_TOKEN: TOKEN,
      DISPATCH_STATE_DIR: stateDir,
      DISPATCH_HOST: "omp",
      DISPATCH_SESSION_ID: "ses-1",
    },
    {
      stdout: (text) => {
        stdout += text;
      },
      stderr: (text) => {
        stderr += text;
      },
      readText: () => "",
      cwd: stateDir,
    }
  );
  return { code, stdout, stderr };
}

/** What the recorder saw, as `METHOD path`. */
function calls(): string[] {
  return requests.map((recorded) => `${recorded.method} ${recorded.path}`);
}

/** The `assignee` the one PATCH of this run sent, with `undefined` telling a field the body left
 *  out apart from the null that clears it. */
function sentAssignee(): unknown {
  const patch = requests.find((recorded) => recorded.method === "PATCH");
  if (patch === undefined) throw new Error("no PATCH was sent");
  const body = patch.body as Record<string, unknown>;
  return "assignee" in body ? body.assignee : undefined;
}

describe("dispatch issue-update sets who the issue's asks wait on", () => {
  test("--assignee sends the person, and --clear-assignee sends null", async () => {
    const set = await run(["issue-update", "--issue", "DSP-1", "--assignee", ASSIGNEE]);

    expect(set.stderr).toBe("");
    expect(set.code).toBe(0);
    expect(calls()).toEqual(["GET /api/v1/issues/DSP-1", "PATCH /api/v1/issues/DSP-1"]);
    expect(sentAssignee()).toBe(ASSIGNEE);
    expect(set.stdout).toContain(`assignee -> ${ASSIGNEE}`);

    requests.length = 0;
    const cleared = await run(["issue-update", "--issue", "DSP-1", "--clear-assignee"]);

    expect(cleared.stderr).toBe("");
    expect(cleared.code).toBe(0);
    expect(calls()).toEqual(["GET /api/v1/issues/DSP-1", "PATCH /api/v1/issues/DSP-1"]);
    expect(sentAssignee()).toBeNull();
    expect(cleared.stdout).toContain("assignee cleared");
  });

  test("--help names both flags", async () => {
    const { code, stdout, stderr } = await run(["issue-update", "--help"]);

    expect(stderr).toBe("");
    expect(code).toBe(0);
    expect(stdout).toContain("--assignee");
    expect(stdout).toContain("--clear-assignee");
    expect(calls()).toEqual([]);
  });
});

// Handing an ask on is a person's move: the route is person-scoped, and an agent token is refused
// there. So no Dispatch command may carry a flag that writes it — not on the ask commands, not on
// any other. The flag set is the command surface's own (`commandFlags`), the enumeration
// `tool-input-errors.test.ts` walks, so a flag added to any spec is seen here the turn it lands.
const FORBIDDEN_FLAGS = ["asked-of", "asked_of", "ask-recipient", "addressee", "hand-on"];

test("no dispatch command has a flag that writes the person an ask waits on", () => {
  const offending: string[] = [];
  for (const spec of dispatchToolSpecs) {
    for (const flag of commandFlags(spec.name)) {
      // `--clear-<field>` and `--no-<field>` write the same field as `--<field>`.
      const field = flag.replace(/^--(?:clear-|no-)?/, "");
      if (FORBIDDEN_FLAGS.includes(field)) offending.push(`${spec.name} ${flag}`);
    }
  }
  expect(offending).toEqual([]);
});

/** The description of the Dispatch tool `tool`, which carries the two shared sentences. */
function descriptionOf(tool: string): string {
  const spec = dispatchToolSpecs.find((candidate) => candidate.name === tool);
  if (spec === undefined) throw new Error(`no spec named ${tool}`);
  return spec.description;
}

// Both ask descriptions compose the two shared sentences from their constants.
test("both ask descriptions carry the two shared sentences", () => {
  for (const text of [descriptionOf("dispatch_ask"), descriptionOf("dispatch_edit_ask")]) {
    expect(text).toContain(ASK_IS_FOR_A_PERSON);
    expect(text).toContain(ASK_WAITS_ON_THE_ASSIGNEE);
  }
});

// Every `ask.*` frame an agent receives is this payload. The three stored fields of a hand-on ride
// in it, and the schema strips what it does not declare, so dropping one from the schema would
// take it out of every delivered frame with nothing else in the suite noticing.
test("an ask.asked_of payload keeps its three hand-on fields", () => {
  const parsed = AskEventPayloadSchema.parse({
    opened_event_id: 12,
    question: "Which store do we keep?",
    asked_of: ASSIGNEE,
    asked_of_by: "alice@example.com",
    asked_of_at: "2026-11-02T09:00:00Z",
    thread_logins: ["alice@example.com", ASSIGNEE],
    waiting_on_person: ASSIGNEE,
  });

  expect(parsed).toMatchObject({
    asked_of: ASSIGNEE,
    asked_of_by: "alice@example.com",
    asked_of_at: "2026-11-02T09:00:00Z",
  });
  // Read-time only: the row never stores it, so no frame may carry it.
  expect(parsed).not.toHaveProperty("waiting_on_person");

  // The hand-on back to the issue's own assignee stores null, and null must survive too.
  expect(
    AskEventPayloadSchema.parse({
      opened_event_id: 13,
      asked_of: null,
      asked_of_by: "alice@example.com",
      asked_of_at: "2026-11-02T10:00:00Z",
    })
  ).toMatchObject({ asked_of: null, asked_of_by: "alice@example.com" });
});
