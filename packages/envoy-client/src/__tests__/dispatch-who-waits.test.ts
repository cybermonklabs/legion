import { afterAll, beforeEach, expect, test } from "bun:test";
import { readFileSync, statSync } from "node:fs";
import { join, relative } from "node:path";
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
// the checks of dispatch://INFRA-3090 filter on: `prints the person`, `states the rule`, and
// `no text states the old rule` for the one row that reads the whole tree.
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
// questions of an issue nobody holds. Such an issue is still stored (an issue-update that clears
// its assignee), and its questions reach the project's default assignee, so none of these texts
// may say they reach nobody, reach only the Inbox's Unassigned filter, or that a project's default
// exists only where a person set one. Each phrase below is the false claim one of these texts made
// before INFRA-3090, read over the text with its whitespace collapsed as the dispatch pages above
// are.
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

// The texts the rows above name are the ones a reader is sent to; the replaced rule could stand in
// any other. So this row reads the tree: every text file git tracks under the directories that say
// who a question reaches (the contract, the command line, the server, the web app and its
// end-to-end rows, the skills, the documentation site) and every tracked `AGENTS.md` of the
// repository, all but this file, and fails on each sentence that states the two-step rule - the
// person the ask was handed to, else the issue's assignee, with no project default after them -
// or says a question can reach nobody.
//
// A sentence is read as prose: each line loses its comment marker and the quotes of a string
// concatenated across lines, so a phrase wrapped over two lines of a comment or a tool description
// is still one phrase. The two-step wordings below are the ones this repository used before
// INFRA-3090; each is a step list that a sentence naming the project's default (`default`) carries
// to its third step, so only a sentence without one is a hit. The no-person wordings count only in
// a sentence about a question (an ask, a question, an approval request), since `reaches nobody`
// is also what this repository says of a frame published to an empty room.
//
// The allowlist holds the sentences that say it deliberately, each with its source, an excerpt of
// the sentence and why it stays; an entry that matches no hit fails the row, so a sentence that was
// reworded or moved is read again rather than left allowed (`dispatch-asked-of.test.ts`'s
// `ALLOWED`).
const OLD_RULE_ROOTS = [
  "packages/contracts/src",
  "packages/envoy-client/src",
  "packages/envoy/internal/dispatch",
  "packages/dispatch/web/src",
  "packages/dispatch/e2e",
  "skills",
  "docs/site/src",
];

/** Tracked paths under the roots no reader is sent to for the rule: the recorded HTTP answers
 *  under `packages/dispatch/e2e/__recordings__`, each what a server answered when it was
 *  recorded. The changelogs, their `.changes` notes and `docs/solutions` are histories too, but no
 *  root holds them, so the listing never returns one. What git ignores (dependencies, build
 *  output, generated reference pages, test results) is never listed. */
const OLD_RULE_LEFT_OUT = /(?:^|\/)__recordings__\//;

/** Wordings of the two-step rule. `sentence` is false for a wording that is the two-step rule on
 *  its own, whatever else its sentence says. */
const TWO_STEP_WORDINGS: readonly { readonly pattern: RegExp; readonly sentence: boolean }[] = [
  { pattern: /coalesce\(\s*(?:a\.)?asked_of,\s*(?:i\.)?assignee\s*\)/, sentence: false },
  { pattern: /asked_of`?,?\s+or\s+(?:the\s+)?(?:issue's\s+)?assignee/i, sentence: true },
  { pattern: /assignee\s+of\s+its\s+issue,?\s+until\s+somebody/i, sentence: true },
  { pattern: /issue's\s+assignee,?\s+until\s+somebody/i, sentence: true },
  { pattern: /waits?\s+on\s+the\s+assignee\s+of\s+its\s+issue/i, sentence: true },
  { pattern: /\b(?:with\s+no|a\s+null|an\s+empty)\s+`?asked_of`?\s+waits\b/i, sentence: true },
  {
    pattern:
      /handed\s+(?:it|the\s+ask)\s+to,?\s+(?:else|or|and|then)\s+the\s+(?:issue's\s+assignee|assignee\s+of\s+its\s+issue)/i,
    sentence: true,
  },
];

/** The project's default, which a sentence that carries the chain to its third step names. */
const THIRD_STEP = /\bdefault/i;

/** Wordings that say a question reaches nobody, counted in a sentence about a question. */
const NO_PERSON_WORDINGS = [
  /\breach(?:es|ed)?\s+(?:nobody|no\s+one)\b/i,
  /\bnobody's\s+Inbox\b/i,
  /\bwaits?\s+on\s+nobody\b/i,
  /\basked\s+of\s+nobody\b/i,
  /\bonly\s+the\s+Inbox's\s+`?Unassigned`?\s+filter\b/i,
  /\bnobody's\s+own\b/i,
  /\bheld\s+by\s+nobody\b/i,
];

const A_QUESTION = /\b(?:asks?|questions?|approval\s+requests?)\b/i;

/** Every `[source, excerpt, reason]` the scan may find: the sentence of the hit holds the excerpt,
 *  and the reason says why the sentence keeps the wording. */
const OLD_RULE_ALLOWED: readonly (readonly [string, string, string])[] = [
  // The new rule, said as what can no longer happen.
  [
    "packages/contracts/src/dispatch-tools.ts",
    "so the third step always names a person and no ask reaches nobody",
    "states the new rule: the third step always names a person",
  ],
  [
    "packages/dispatch/e2e/project-default-assignee.e2e.ts",
    "So no question reaches nobody.",
    "states the new rule at the end of the three steps it lists",
  ],
  [
    "packages/dispatch/e2e/inbox.e2e.ts",
    "so no question the server answers is held by nobody",
    "states the new rule: the Unassigned filter adds no row",
  ],
  [
    "packages/dispatch/e2e/inbox.e2e.ts",
    "so none of these is held by nobody",
    "states the new rule: the Unassigned filter lists nothing",
  ],
  [
    "packages/envoy/internal/dispatch/api/issue_assignee_test.go",
    "is not an issue whose questions sit in nobody's Inbox",
    "states the new rule: those questions reach the project's default assignee",
  ],
  [
    "packages/envoy/internal/dispatch/api/who_waits_test.go",
    "No row waits on nobody",
    "the claim the row beneath it asserts, that every Inbox row names a person",
  ],
  // A closed question, which nobody owes an answer.
  [
    "packages/dispatch/web/src/features/inbox/AskCard.tsx",
    "An ask that is not open waits on nobody",
    "a closed ask has nobody to wait on; the rule decides whom an open question waits on",
  ],
  [
    "packages/dispatch/web/src/features/inbox/AskCard.test.tsx",
    "an answered, a resolved or a withdrawn question waits on nobody",
    "a closed question has nobody to wait on; the rule decides whom an open question waits on",
  ],
  // What a Dispatch older than every project naming a default answers: no person.
  [
    "packages/dispatch/AGENTS.md",
    "a badge reading `Asked of <person>`, or `Asked of nobody`",
    "the card's badge where the server names no person, which only an older Dispatch does",
  ],
  // Not a question: a block copied from another ask, a bus message, a notification.
  [
    "packages/envoy/internal/dispatch/docs/ask_blocks.go",
    "A block taken for a copy opens no ask and reaches nobody's Inbox",
    "a copy of an ask block opens no ask, so it is no question waiting on anyone",
  ],
  [
    "packages/envoy/internal/dispatch/docs/copied_asks_test.go",
    "A block taken for a copy opens no ask and reaches nobody's Inbox",
    "a copy of an ask block opens no ask, so it is no question waiting on anyone",
  ],
  [
    "packages/envoy/AGENTS.md",
    "since it opens no ask and reaches nobody's Inbox",
    "a copy of an ask block opens no ask, so it is no question waiting on anyone",
  ],
  [
    "packages/envoy/AGENTS.md",
    "as a human's unnamed `artifact.version` reaches nobody",
    "a version notification that reaches no follower, not whom a question waits on",
  ],
  [
    "skills/legion-controller/references/finding-asks.md",
    "a reference published then reaches nobody",
    "a bus message published while no controller runs; the asks are the ones a session follows",
  ],
  // History: an applied migration says what was true when it ran, and is never edited.
  [
    "packages/envoy/internal/dispatch/store/migrations/20261109221325_asks_asked_of.up.sql",
    "Null means the ask waits on the assignee of its issue.",
    "the rule as it stood when INFRA-1576 added asked_of",
  ],
  [
    "packages/envoy/internal/dispatch/store/migrations/20261109221325_asks_asked_of.up.sql",
    "One expression in the server, coalesce(a.asked_of, i.assignee), gives the person",
    "the rule as it stood when INFRA-1576 added asked_of",
  ],
  // Tests that name the old rule to catch it.
  [
    "packages/envoy/internal/dispatch/api/who_waits_test.go",
    "a read that reintroduced the two-step `coalesce(a.asked_of, i.assignee)`",
    "the spelling the count beneath it exists to catch",
  ],
  [
    "packages/envoy-client/src/__tests__/dispatch-asked-of.test.ts",
    "the person the ask was handed to, the assignee of its issue until somebody hands it on",
    "an excerpt that row matches in packages/envoy/AGENTS.md, whose sentence goes on to the default",
  ],
];

interface OldRuleHit {
  readonly source: string;
  readonly line: number;
  readonly sentence: string;
}

/** Every text file the scan reads, by its path from the repository root: what git tracks under
 *  the roots and every tracked `AGENTS.md`, beside the untracked files git does not ignore there,
 *  less the paths no reader is sent to and this file, whose allowlist quotes each sentence it
 *  allows a second time. Only a path that is a regular file on disk is read: in a jj workspace
 *  git's index follows the parent commit, so a file the working copy deleted is still listed
 *  (`.github/scripts/ci_filter_coverage.py`'s `repo_files` keeps the same two listings). */
function oldRuleFiles(): string[] {
  const listed = new Set<string>();
  for (const extra of [[], ["--others", "--exclude-standard"]]) {
    const listing = Bun.spawnSync(
      ["git", "ls-files", "-z", ...extra, "--", ...OLD_RULE_ROOTS, ":(glob)**/AGENTS.md"],
      { cwd: REPO_ROOT, stdout: "pipe", stderr: "pipe" }
    );
    if (!listing.success) throw new Error(`git ls-files failed: ${listing.stderr.toString()}`);
    for (const path of listing.stdout.toString().split("\0")) listed.add(path);
  }
  const self = relative(REPO_ROOT, import.meta.path);
  return [...listed].filter(
    (path) =>
      path !== "" &&
      path !== self &&
      !OLD_RULE_LEFT_OUT.test(path) &&
      statSync(join(REPO_ROOT, path), { throwIfNoEntry: false })?.isFile() === true
  );
}

/** A file's text as prose: each line without its comment marker or the quotes of a string that
 *  continues on the next line, the lines still joined by newlines so a line number is kept. */
function prose(text: string): string {
  return text
    .split("\n")
    .map((line) =>
      line
        .replace(/^\s*(?:\/\/+|\/\*+|\*+(?!\/)|--|#+|<!--)?\s*\+?\s*["']?/, "")
        .replace(/(?:\*\/|--!?>|["']\s*\+)\s*$/, "")
    )
    .join("\n");
}

/** The sentence around `at`: from the end of the sentence before it to the end of its own. A
 *  blank line ends a sentence as a full stop does, and so does the end of a line that closes a
 *  string literal (`",`) or a table row (`|`), since neither runs on into the next line; that
 *  closing quote or bar is not part of the sentence. */
function sentenceAt(text: string, at: number): string {
  const end = /[.;!?](?=\s)|\n\s*\n|["'],?\n|\|\s*\n/g;
  let from = 0;
  let to = text.length;
  for (const match of text.matchAll(end)) {
    if (match.index + match[0].length <= at) from = match.index + match[0].length;
    else {
      to = match.index + match[0].length;
      break;
    }
  }
  return text
    .slice(from, to)
    .replace(/\s+/g, " ")
    .trim()
    .replace(/(?:["'],?|\|)$/, "");
}

function oldRuleHitsIn(source: string, text: string): OldRuleHit[] {
  const read = prose(text);
  const hits = new Map<string, OldRuleHit>();
  const record = (at: number, sentence: string) => {
    const line = read.slice(0, at).split("\n").length;
    if (!hits.has(sentence)) hits.set(sentence, { source, line, sentence });
  };
  for (const { pattern, sentence: judged } of TWO_STEP_WORDINGS) {
    for (const match of read.matchAll(new RegExp(pattern.source, `${pattern.flags}g`))) {
      const sentence = sentenceAt(read, match.index);
      if (!judged || !THIRD_STEP.test(sentence)) record(match.index, sentence);
    }
  }
  for (const pattern of NO_PERSON_WORDINGS) {
    for (const match of read.matchAll(new RegExp(pattern.source, `${pattern.flags}g`))) {
      const sentence = sentenceAt(read, match.index);
      if (A_QUESTION.test(sentence)) record(match.index, sentence);
    }
  }
  return [...hits.values()];
}

test("no text states the old rule: a question reaches the project's default assignee, never nobody", () => {
  const files = oldRuleFiles();
  expect(files.length, "the scan read nothing").toBeGreaterThan(0);

  const hits = files.flatMap((source) => {
    const bytes = readFileSync(join(REPO_ROOT, source));
    // A binary file holds no sentence; a NUL byte in its head marks one.
    if (bytes.subarray(0, 8000).includes(0)) return [];
    return oldRuleHitsIn(source, bytes.toString("utf8"));
  });

  const used = new Set<number>();
  const unexplained: string[] = [];
  for (const hit of hits) {
    const at = OLD_RULE_ALLOWED.findIndex(
      ([source, excerpt]) => source === hit.source && hit.sentence.includes(excerpt)
    );
    if (at < 0) unexplained.push(`${hit.source}:${hit.line}: ${hit.sentence}`);
    else used.add(at);
  }
  expect(unexplained).toEqual([]);

  const stale = OLD_RULE_ALLOWED.filter((_, at) => !used.has(at)).map(
    ([source, excerpt]) => `${source}: ${excerpt}`
  );
  expect(stale).toEqual([]);
  // It reads every text file of the tree, which takes more than bun's 5 s default on a loaded
  // pod, and the check runs this file with a bare `bun test` rather than the package's
  // `--timeout 30000`, so the row carries its own bound.
}, 120_000);

// An assignee move names, in `thread_logins`, the person the issue's questions reached on each
// side of the write; a side where the issue holds nobody is the project's default assignee, which
// `issueWriteThreadLogins` resolves (`api/thread_logins.go`). Each guide that describes that
// field for an assignee move says so, or it tells a reader that a move off nobody names one
// person when the server names two.
test.each([
  [
    "packages/envoy/cmd/dispatch/README.md",
    "for an assignee move the assignee the issue was taken from",
  ],
  [
    "packages/contracts/AGENTS.md",
    "moved the status or the assignee (`IssueEventChanges.thread_logins`)",
  ],
])("states the rule: %s names the project's default on an assignee move's side that holds nobody", (path, opening) => {
  const text = readFileSync(join(REPO_ROOT, path), "utf8").replace(/\s+/g, " ");
  const at = text.indexOf(opening);
  expect(at).toBeGreaterThanOrEqual(0);
  const sentence = sentenceAt(text, at);
  expect(sentence).toMatch(/default assignee/);
});
