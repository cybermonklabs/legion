import { afterAll, beforeEach, describe, expect, test } from "bun:test";
import { readdirSync, readFileSync, statSync } from "node:fs";
import { join, relative } from "node:path";
import {
  ASK_IS_FOR_A_PERSON,
  ASK_WAITS_ON_THE_ASSIGNEE,
  AskEventPayloadSchema,
  dispatchToolSpecs,
} from "@legion/contracts";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import { runDispatchCli } from "../dispatch-cli";
import { commandFlags } from "../dispatch-command";
import { resetAdviceMemory } from "../dispatch-execute";
import { forgetKeyedLayerMemory } from "../dispatch-http";

// Who an open ask waits on, from the command line and from the texts an agent reads. A person
// hands an ask on through the web app or `PUT /api/v1/asks/{id}/asked-of`, both person-scoped, so
// the half of this the CLI carries is the issue's assignee and nothing else: the command sets
// that, and no command of the whole surface writes `asked_of`. The rest is text — the two
// sentences every description and skill shares, and the scan that fails a new line telling an
// agent to address or hand on an ask.

const makeDirectory = temporaryDirectories("dispatch-asked-of");

/** The repository root, from this file's own path rather than the working directory, which bun
 *  sets to the package when the check runs `cd packages/envoy-client && bun test`. */
const REPO_ROOT = join(import.meta.dir, "..", "..", "..", "..");

const ASSIGNEE = "bob@theorem.dev";

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

test("the two shared sentences stand in both ask descriptions and in the dispatch skill", () => {
  for (const text of [
    descriptionOf("dispatch_ask"),
    descriptionOf("dispatch_edit_ask"),
    readFileSync(join(REPO_ROOT, "skills", "dispatch", "SKILL.md"), "utf8"),
  ]) {
    expect(text).toContain(ASK_IS_FOR_A_PERSON);
    expect(text).toContain(ASK_WAITS_ON_THE_ASSIGNEE);
  }
});

// The negative half, as a rule a reader can decide: on one line, one of the words that would tell
// an agent to move an ask somewhere, standing within forty characters of `ask` or `asks`. Forty
// characters is a clause: near enough that the two words are about each other, far enough to catch
// "the ask ... hand it to". The scan reads every file under skills/, every packages/*/AGENTS.md
// and every dispatch_* tool description, which is every text that tells an agent how asks work.
//
// A hit is not a defect by itself: most of these lines say "hand the work over" or name an anchor
// called `finding-asks.md#hand-over-a-clear-defect`. So the scan fails on a hit the allowlist
// below does not hold, each entry its source and the half-line that matched, so a later reader
// judges the line rather than a count. An entry that matches nothing fails too: a line that moved
// or changed its wording is re-read rather than left allowed. The shape follows the retired-phrase
// scan in `packages/pi-legion/src/skills-guard.test.ts`.
const NEAR = 40;
const MOVING_WORDS = /\b(?:hand|handed|hands|address|addressed|redirect|redirects)\b/gi;
const AN_ASK = /\basks?\b/gi;

/** Every `[source, excerpt]` the scan may see, with why the line says nothing about who an ask
 *  goes to. The last two are this change's own sentences, which say it and say it right. */
const ALLOWED: readonly (readonly [string, string])[] = [
  // Handing over the *work*, never the ask.
  [
    "skills/legion-worker/references/findings.md",
    "## Fix it where you are, else hand it over, and ask only for a",
  ],
  [
    "skills/legion-worker/references/findings.md",
    "not sure of is not step 1: hand it over, or ask when a person must",
  ],
  [
    "skills/legion-worker/references/findings.md",
    "options ask where the work goes, with `Hand it to the owning tree` first",
  ],
  // "by hand", the adverb: a human resolving a review thread themselves.
  [
    "skills/legion-worker/references/review-threads.md",
    "which opens a `dispatch ask` for a human to resolve it by hand; never skip it",
  ],
  // Handing a turn back inside one ask's thread, which moves nobody's list.
  [
    "skills/legion-architect/references/wake-routing.md",
    "request you must revise or hand back, `--reply-to-ask <ask id> --turn agent`,",
  ],
  [
    "skills/legion-controller/references/parked-decisions.md",
    "ask hands the turn to the person. For",
  ],
  [
    "skills/legion-controller/references/parked-decisions.md",
    "same ask, and a session's reply there hands the turn to the person, as",
  ],
  [
    "packages/pi-envoy/AGENTS.md",
    "version moved, which no reply hands back: the client then reads the ask and adds a line naming the",
  ],
  ["packages/envoy/AGENTS.md", "agent replying to a deferred ask hands the turn back without"],
  // A link to a section of a reference file, whose anchor holds the word.
  ["skills/legion-controller/SKILL.md", "finding-asks.md#hand-over-a-clear-defect`"],
  // The web app's own behaviour, written for whoever edits it, never for an agent.
  [
    "packages/dispatch/AGENTS.md",
    "its thread's replies, and an ask link focuses its Conversation turn or redirects to the document whose margin",
  ],
  [
    "packages/dispatch/AGENTS.md",
    "below its document version; a hand-back (`ask.handed_back`, which",
  ],
  [
    "packages/dispatch/AGENTS.md",
    "or resolved elsewhere) or hands its turn the other way (their own ask-back, an agent's progress",
  ],
  // The block-id repair in the server, which hands rows around, not asks.
  [
    "packages/envoy/AGENTS.md",
    "(an id holding a NUL, which a hand-built client can set and no ask row or anchor can store,",
  ],
  ["packages/envoy/AGENTS.md", "since the id repair would hand the held ask's row and answer to"],
  // This change's own sentences: the rule itself, which an agent must read.
  [
    "dispatch_issue_update description",
    "this issue's asks, and every ask on it waits on them until a person hands that ask on",
  ],
  [
    "dispatch_claim description",
    "to it, except in an ask you follow: anyone else's answer, hand-back, resolution or edit of",
  ],
  // The guides of this repository, which state the rule for whoever edits the code rather than
  // telling an agent to use it: who an ask waits on, and what a hand-on moves.
  [
    "packages/contracts/AGENTS.md",
    "Every ask on the issue waits on the assignee until a person hands that ask to somebody else, which no agent may do.",
  ],
  [
    "packages/contracts/AGENTS.md",
    "on an `ask.asked_of` the two people the hand-on moved the ask between",
  ],
  ["packages/dispatch/AGENTS.md", "A hand-on is there for the reason a reassignment is"],
  [
    "packages/dispatch/AGENTS.md",
    "the lowercase email of who an ask on it waits on until somebody hands that ask to another person",
  ],
  [
    "packages/envoy/AGENTS.md",
    "email of the person every ask on it waits on until somebody hands that ask to another person",
  ],
  [
    "packages/envoy/AGENTS.md",
    "the person an ask on the issue waits on until somebody hands it on",
  ],
  [
    "packages/envoy/AGENTS.md",
    "`ask.asked_of` names two people instead, the person the ask waited on and the person it waits on now",
  ],
  [
    "packages/envoy/AGENTS.md",
    "that same rule moves the rows of the asks nobody has handed on out of one Inbox and into the other",
  ],
  [
    "packages/envoy/AGENTS.md",
    "the asks handed to them, and the asks on the issues assigned to them that nobody has handed on",
  ],
  [
    "packages/envoy/AGENTS.md",
    "the person the ask was handed to, and the assignee of its issue until somebody hands it on",
  ],
];

interface Hit {
  readonly source: string;
  readonly line: number;
  readonly text: string;
}

function matches(line: string, pattern: RegExp): { from: number; to: number }[] {
  return [...line.matchAll(pattern)].map((match) => ({
    from: match.index,
    to: match.index + match[0].length,
  }));
}

/** Every line of `text` where one of the moving words stands within `NEAR` characters of `ask`
 *  or `asks`, counting the characters between the two words. */
function hitsIn(source: string, text: string): Hit[] {
  const hits: Hit[] = [];
  text.split("\n").forEach((line, index) => {
    const moving = matches(line, MOVING_WORDS);
    const asks = matches(line, AN_ASK);
    const near = moving.some((word) =>
      asks.some(
        (ask) =>
          (word.from >= ask.to
            ? word.from - ask.to
            : ask.from >= word.to
              ? ask.from - word.to
              : 0) <= NEAR
      )
    );
    if (near) hits.push({ source, line: index + 1, text: line });
  });
  return hits;
}

/** Every file under `dir`, at any depth. */
function filesUnder(dir: string): string[] {
  return readdirSync(dir).flatMap((entry) => {
    const full = join(dir, entry);
    return statSync(full).isDirectory() ? filesUnder(full) : [full];
  });
}

/** Every text that tells an agent how an ask works: the skills, the per-package AGENTS.md notes,
 *  and the Dispatch tool descriptions, each under the name the failure prints. */
function scannedTexts(): { source: string; text: string }[] {
  const texts: { source: string; text: string }[] = [];
  for (const file of filesUnder(join(REPO_ROOT, "skills"))) {
    texts.push({ source: relative(REPO_ROOT, file), text: readFileSync(file, "utf8") });
  }
  for (const entry of readdirSync(join(REPO_ROOT, "packages"))) {
    const file = join(REPO_ROOT, "packages", entry, "AGENTS.md");
    if (!statSync(join(REPO_ROOT, "packages", entry)).isDirectory()) continue;
    try {
      texts.push({ source: relative(REPO_ROOT, file), text: readFileSync(file, "utf8") });
    } catch {
      // A package without an AGENTS.md note.
    }
  }
  for (const spec of dispatchToolSpecs) {
    if (!spec.name.startsWith("dispatch_")) continue;
    texts.push({ source: `${spec.name} description`, text: spec.description });
  }
  return texts;
}

test("no skill, package note or tool description tells an agent to hand an ask on", () => {
  const hits = scannedTexts().flatMap(({ source, text }) => hitsIn(source, text));
  expect(hits.length, "the scan read nothing").toBeGreaterThan(0);

  const used = new Set<number>();
  const unexplained: string[] = [];
  for (const hit of hits) {
    const at = ALLOWED.findIndex(
      ([source, excerpt]) => source === hit.source && hit.text.includes(excerpt)
    );
    if (at < 0) unexplained.push(`${hit.source}:${hit.line}: ${hit.text.trim()}`);
    else used.add(at);
  }
  expect(unexplained).toEqual([]);

  const stale = ALLOWED.filter((_, at) => !used.has(at)).map(
    ([source, excerpt]) => `${source}: ${excerpt}`
  );
  expect(stale).toEqual([]);
});

// Every `ask.*` frame an agent receives is this payload. The three stored fields of a hand-on ride
// in it, and the schema strips what it does not declare, so dropping one from the schema would
// take it out of every delivered frame with nothing else in the suite noticing.
test("an ask.asked_of payload keeps its three hand-on fields", () => {
  const parsed = AskEventPayloadSchema.parse({
    opened_event_id: 12,
    question: "Which store do we keep?",
    asked_of: ASSIGNEE,
    asked_of_by: "alice@theorem.dev",
    asked_of_at: "2026-11-02T09:00:00Z",
    thread_logins: ["alice@theorem.dev", ASSIGNEE],
    waiting_on_person: ASSIGNEE,
  });

  expect(parsed).toMatchObject({
    asked_of: ASSIGNEE,
    asked_of_by: "alice@theorem.dev",
    asked_of_at: "2026-11-02T09:00:00Z",
  });
  // Read-time only: the row never stores it, so no frame may carry it.
  expect(parsed).not.toHaveProperty("waiting_on_person");

  // The hand-on back to the issue's own assignee stores null, and null must survive too.
  expect(
    AskEventPayloadSchema.parse({
      opened_event_id: 13,
      asked_of: null,
      asked_of_by: "alice@theorem.dev",
      asked_of_at: "2026-11-02T10:00:00Z",
    })
  ).toMatchObject({ asked_of: null, asked_of_by: "alice@theorem.dev" });
});
