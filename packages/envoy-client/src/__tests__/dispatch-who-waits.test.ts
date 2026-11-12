import { afterAll, beforeEach, expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import { runDispatchCli } from "../dispatch-cli";
import { resetAdviceMemory } from "../dispatch-execute";
import { forgetKeyedLayerMemory } from "../dispatch-http";

// Who a question reaches, from the command line and from the texts that state the rule.
//
// A question reaches one person, resolved as it is read (`askedOfPersonExpression` in
// `packages/envoy/internal/dispatch/api/ask_rows.go`): the person it was handed to, else the
// issue's assignee, else the project's default assignee. Every project names a default assignee,
// so no question reaches nobody. The three writes that carry a question answer that person as
// `waiting_on_person`, and the three commands print them, so an agent learns who reads its
// question without a second command.
//
// The rows read either a printed line or a text a person reads, and their titles carry the words
// the two checks of dispatch://INFRA-3090 filter on: `prints the person` and `states the rule`.
// The shape is `dispatch-asked-of.test.ts`'s: one stub Dispatch server answering as the server
// answers, driven through `runDispatchCli`, with a temporary state directory per row.

const makeDirectory = temporaryDirectories("dispatch-who-waits");

/** The repository root, from this file's own path rather than the working directory, which bun
 *  sets to the package when the check runs `cd packages/envoy-client && bun test`. */
const REPO_ROOT = join(import.meta.dir, "..", "..", "..", "..");

/** The issue's own assignee, whom a question on DSP-1 reaches. */
const ASSIGNEE = "bob@theorem.dev";
/** The person CORE defaults to, whom a question on an issue nobody holds reaches. */
const DEFAULT_ASSIGNEE = "dana@theorem.dev";

interface Recorded {
  readonly method: string;
  readonly path: string;
  readonly body: unknown;
}

const requests: Recorded[] = [];

/** DSP-1 as `GET /api/v1/issues/DSP-1` reads it, with one document to approve. */
const ISSUE = {
  key: "DSP-1",
  title: "Native workspace",
  status: "in_progress",
  labels: [],
  priority: null,
  assignee: ASSIGNEE,
  route: null,
  parent: null,
  blocked_by: [],
  components: [],
  repo: null,
  resolved_repo: null,
  closed_at: null,
  external_links: [],
  primary_artifact_id: "artifact-1",
  artifacts: [
    {
      id: "artifact-1",
      slug: "spec",
      name: "spec.md",
      primary: true,
      approval: { state: "draft", latest_version: 3 },
    },
  ],
  open_asks: [],
};

/** What `POST /api/v1/artifacts/artifact-1/approval-requests` answers next, and what the project
 *  create answers: one row sets these before it runs the command. */
let approvalRecorded = true;
let approvalSettled = false;
let projectRefusal: string | null = null;
/** The default the project create answers when the call names none; `undefined` is a Dispatch
 *  older than that answer, which leaves the field out. */
let projectDefault: string | undefined = DEFAULT_ASSIGNEE;

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
    if (pathname === "/api/v1/issues/DSP-1/asks" && request.method === "POST") {
      const sent = body as { question: string };
      return Response.json(
        {
          id: "ask-1",
          issue_key: "DSP-1",
          artifact_id: null,
          kind: "question",
          urgency: "med",
          question: sent.question,
          // The server resolves the person as the ask is read and answers them on the write.
          waiting_on_person: ASSIGNEE,
        },
        { status: 201 }
      );
    }
    if (pathname === "/api/v1/artifacts/artifact-1/blocks" && request.method === "GET") {
      return Response.json([{ id: "p-1", type: "paragraph", from: 0, to: 12 }]);
    }
    if (
      pathname === "/api/v1/artifacts/artifact-1/approval-requests" &&
      request.method === "POST"
    ) {
      const sent = body as { summary: string };
      if (approvalSettled) {
        // The settled branch carries no question, so it names nobody.
        return Response.json({
          ask: null,
          approval: { by: { kind: "user", id: ASSIGNEE }, at: "2026-11-11T09:00:00Z" },
          artifact_id: "artifact-1",
          coverage: null,
          version: 3,
        });
      }
      return Response.json(
        {
          ask: {
            id: "ask-9",
            issue_key: "DSP-1",
            artifact_id: "artifact-1",
            kind: "approval",
            question: `Approve spec.md (version 3)? ${sent.summary}`,
            waiting_on_person: ASSIGNEE,
          },
          approval: { by: null, at: null },
          artifact_id: "artifact-1",
          coverage: null,
          version: 3,
        },
        { status: approvalRecorded ? 201 : 200 }
      );
    }
    if (pathname === "/api/v1/projects" && request.method === "POST") {
      if (projectRefusal !== null) {
        return Response.json({ code: "INVALID_PROJECT", error: projectRefusal }, { status: 400 });
      }
      const sent = body as { key: string; name: string; default_assignee?: string };
      return Response.json(
        {
          key: sent.key,
          name: sent.name,
          open_asks: 0,
          default_assignee: sent.default_assignee ?? projectDefault,
          created_at: "2026-11-12T00:00:00Z",
        },
        { status: 201 }
      );
    }
    if (pathname === "/api/v1/asks/open" && request.method === "GET") {
      // A Dispatch older than the three-step rule carries no waiting_on_person on its rows:
      // the field is absent here, as that server leaves it.
      return Response.json({
        session_id: "ses-1",
        as_of: "2026-11-12T00:00:00Z",
        opened_since: false,
        count: 1,
        waiting_on_human: 1,
        waiting_on_agent: 0,
        asks: [
          {
            id: "ask-7",
            issue_key: "DSP-1",
            ref: "/issues/DSP-1/asks/ask-7",
            kind: "question",
            urgency: "med",
            question: QUESTION,
            created_at: "2026-11-12T00:00:00Z",
            age_seconds: 3600,
            priority: 1,
            owner: { issue: { key: "DSP-1", title: "Native workspace" } },
            human_replied: false,
            last_reply: null,
            waiting_on: "human",
          },
        ],
      });
    }
    return Response.json({ error: `unexpected ${request.method} ${pathname}` }, { status: 500 });
  },
});

afterAll(() => server.stop(true));

let stateDir = "";
beforeEach(() => {
  requests.length = 0;
  approvalRecorded = true;
  approvalSettled = false;
  projectRefusal = null;
  projectDefault = DEFAULT_ASSIGNEE;
  stateDir = makeDirectory();
  resetAdviceMemory();
  forgetKeyedLayerMemory();
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
      DISPATCH_TOKEN: "test-token",
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

const QUESTION =
  "Release 4.2 ships Friday and its migration drops sessions.legacy_token, which nobody " +
  "reviewed. Ship Friday and risk signing out the 1,240 users on legacy tokens, or hold to " +
  "Monday? Recommendation: hold to Monday, since no user waits on 4.2.";

test("dispatch ask prints the person the question waits on", async () => {
  const { code, stdout, stderr } = await run([
    "ask",
    "--issue",
    "DSP-1",
    "--question",
    QUESTION,
    "--urgency",
    "med",
  ]);

  expect(stderr).toBe("");
  expect(code).toBe(0);
  expect(stdout).toContain(`waiting on ${ASSIGNEE}`);
  expect(stdout.split("\n")[0]).toBe(
    `Asked ask-1 on DSP-1 (urgency med), waiting on ${ASSIGNEE}: ${QUESTION}`
  );
});

test("dispatch request-approval prints the person a new request waits on", async () => {
  const { code, stdout, stderr } = await run([
    "request-approval",
    "--issue",
    "DSP-1",
    "--summary",
    "A live sync replaces the nightly export.",
  ]);

  expect(stderr).toBe("");
  expect(code).toBe(0);
  expect(stdout).toStartWith(
    "Approval requested for spec.md (document id artifact-1) at version 3 (ask ask-9)."
  );
  expect(stdout).toContain(`It waits on ${ASSIGNEE}.`);
});

test("dispatch request-approval prints the person for a request it hands back", async () => {
  // The server's 200: the open request already waits on the human, the answer that hands one
  // back. It carries a question, so it names the person that question reaches.
  approvalRecorded = false;
  const { code, stdout, stderr } = await run([
    "request-approval",
    "--issue",
    "DSP-1",
    "--summary",
    "A live sync replaces the nightly export.",
  ]);

  expect(stderr).toBe("");
  expect(code).toBe(0);
  expect(stdout).toContain("already waits on the human");
  expect(stdout).toContain(`It waits on ${ASSIGNEE}.`);
});

test("dispatch request-approval prints the person on no line of an already-approved version", async () => {
  // The settled branch carries no question, so there is no person to name.
  approvalSettled = true;
  const { code, stdout, stderr } = await run([
    "request-approval",
    "--issue",
    "DSP-1",
    "--summary",
    "A live sync replaces the nightly export.",
  ]);

  expect(stderr).toBe("");
  expect(code).toBe(0);
  expect(stdout).toContain("is already approved at version 3");
  expect(stdout).not.toContain("It waits on");
});

test("dispatch project prints the person the new project names", async () => {
  const named = await run([
    "project",
    "--key",
    "CORE",
    "--name",
    "Core",
    "--default-assignee",
    ASSIGNEE,
  ]);

  expect(named.stderr).toBe("");
  expect(named.code).toBe(0);
  expect(requests.map((recorded) => `${recorded.method} ${recorded.path}`)).toEqual([
    "POST /api/v1/projects",
  ]);
  expect(requests[0]?.body).toMatchObject({
    key: "CORE",
    name: "Core",
    default_assignee: ASSIGNEE,
  });
  expect(named.stdout).toContain(
    `Its unowned work lands on ${ASSIGNEE}: a question on an issue nobody holds reaches them.`
  );

  requests.length = 0;
  const unnamed = await run(["project", "--key", "OPS", "--name", "Operations"]);

  expect(unnamed.stderr).toBe("");
  expect(unnamed.code).toBe(0);
  expect(requests[0]?.body).not.toHaveProperty("default_assignee");
  expect(unnamed.stdout).toContain(`Its unowned work lands on ${DEFAULT_ASSIGNEE}:`);
});

test("dispatch project prints the person nowhere when the create answers without one", async () => {
  // A Dispatch older than the answer carrying the default leaves the field out; the command then
  // names nobody rather than printing `undefined` as the person.
  projectDefault = undefined;
  const { code, stdout, stderr } = await run(["project", "--key", "OPS", "--name", "Operations"]);

  expect(stderr).toBe("");
  expect(code).toBe(0);
  expect(stdout).toStartWith("Created project OPS (Operations) at 2026-11-12T00:00:00Z: ");
  expect(stdout).not.toContain("undefined");
  expect(stdout).not.toContain("Its unowned work lands on");
});

test("dispatch project prints the person it could not name, as the refusal's own words", async () => {
  // The server's words for a credential that acts for nobody and names no default assignee
  // (`createProject` in `packages/envoy/internal/dispatch/api/projects.go`).
  projectRefusal =
    "default_assignee is required: this credential acts for no person, so the call must name " +
    "the person a project's unowned work lands on";
  const { code, stdout, stderr } = await run(["project", "--key", "CORE", "--name", "Core"]);

  expect(stderr).toBe("");
  expect(code).toBe(1);
  expect(stdout).toContain(`INVALID_PROJECT: ${projectRefusal}`);
});

test("dispatch open-asks prints the person each row is asked of, and nobody where the server named none", async () => {
  const { code, stdout, stderr } = await run(["open-asks"]);

  expect(stderr).toBe("");
  expect(code).toBe(0);
  // The server this stub stands for is older than the three-step rule and names no person, so
  // the row says nobody rather than printing an empty name or `undefined`.
  expect(stdout).toContain("· asked of nobody ·");
  expect(stdout).not.toContain("asked of undefined");
  expect(stdout).not.toContain("asked of  ·");
});

// The texts that state the rule. Each is read as literal words, never as the constant the code
// under test defines, so a reworded constant that drops a step fails here rather than passing by
// comparing with itself. NO_PERSON_PHRASES are the ways a text says a question can reach nobody:
// none of these texts may hold one.
const THE_CHAIN = "the issue's assignee, else the project's default assignee";
const NO_PERSON_PHRASES = [
  "reaches nobody",
  "waits on nobody",
  "nobody waits on",
  "asked of nobody",
  "else unassigned",
  "leaves it on nobody",
];

/** What `dispatch <command> --help` prints, which is the tool description and its flag help. */
async function help(command: string): Promise<string> {
  const { code, stdout, stderr } = await run([command, "--help"]);
  expect(stderr).toBe("");
  expect(code).toBe(0);
  return stdout;
}

test.each([
  ["ask", [THE_CHAIN, "no agent hands one on"]],
  ["edit-ask", [THE_CHAIN, "no agent hands one on"]],
  ["request-approval", [THE_CHAIN, "so it always reaches a person"]],
  ["issue-update", [THE_CHAIN, "waits on the project's default assignee"]],
  ["issue", [THE_CHAIN, "a new issue always has one"]],
  ["whoami", [THE_CHAIN, "every issue you create has an assignee"]],
])("dispatch %s states the rule: a question reaches the project's default assignee behind the issue's assignee", async (command, phrases) => {
  const text = await help(command);

  for (const phrase of phrases) expect(text).toContain(phrase);
  for (const phrase of NO_PERSON_PHRASES) expect(text).not.toContain(phrase);
});

/** A documentation page under `docs/site/src/content/docs/dispatch/`. */
function page(name: string): string {
  return readFileSync(
    join(REPO_ROOT, "docs", "site", "src", "content", "docs", "dispatch", name),
    "utf8"
  );
}

// The pages say the rule in their own words, for a person rather than for an agent, so each row
// reads the words its page uses. The page's whitespace is collapsed to single blanks before
// either check reads it, so a phrase is written as one line here and re-wrapping a paragraph
// moves no check about meaning; the no-person scan runs over that same collapsed text.
test.each([
  [
    "inbox-and-asks.md",
    [
      "whoever it was handed to, else the assignee of its issue, else the project's default assignee",
      "Every project names a default assignee, so every ask reaches somebody",
    ],
  ],
  [
    "for-agents.md",
    [
      "the person it was handed to, else the issue's assignee, else the project's default assignee",
      "Every project names a default assignee, so every issue has one.",
    ],
  ],
  [
    "issues-and-projects.md",
    [
      "An issue nobody holds leaves its asks on the project's default assignee, so every ask reaches somebody.",
      "Every project names one, so every new issue has an assignee.",
    ],
  ],
])("dispatch/%s states the rule: a question reaches the project's default assignee", (name, phrases) => {
  const text = page(name).replace(/\s+/g, " ");

  for (const phrase of phrases) expect(text).toContain(phrase);
  for (const phrase of NO_PERSON_PHRASES) expect(text).not.toContain(phrase);
});

// The Legion texts that tell a person, or the controller writing to one, what becomes of the
// questions of an issue nobody holds. Such an issue is still stored (a create that names a null
// assignee, an issue-update that clears one), and its questions reach the project's default
// assignee, so none of these texts may say they reach nobody, reach only the Inbox's Unassigned
// filter, or that a project's default exists only where a person set one. Each phrase below is
// the false claim one of these texts made before INFRA-3090, read over the text with its
// whitespace collapsed as the dispatch pages above are.
const UNASSIGNED_REACHES_NOBODY_PHRASES = [
  "reach nobody's Inbox",
  "nobody's Inbox shows",
  "reach only the Inbox's `Unassigned` filter",
  "where a person set one",
];

test.each([
  "docs/site/src/content/docs/legion/using-legion.md",
  "skills/legion-controller/references/triage.md",
  "skills/legion-controller/references/daily-report.md",
])("%s states the rule: an unassigned issue's questions reach the project's default assignee, not nobody", (path) => {
  const text = readFileSync(join(REPO_ROOT, path), "utf8").replace(/\s+/g, " ");

  for (const phrase of UNASSIGNED_REACHES_NOBODY_PHRASES) expect(text).not.toContain(phrase);
});
