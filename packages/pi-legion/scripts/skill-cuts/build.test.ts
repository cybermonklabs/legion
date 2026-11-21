import { afterAll, beforeAll, describe, expect, test } from "bun:test";
import { createHash } from "node:crypto";
import { chmodSync, existsSync, mkdirSync, readFileSync, statSync, writeFileSync } from "node:fs";
import path from "node:path";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import { SessionEntry, type SessionMessage, stripSkillHeader } from "../skill-scenarios/transcript";
import {
  type BuildIo,
  currentBranch,
  DROP_REASONS,
  type DropReason,
  EXIT,
  fileHash,
  main,
  messageOf,
  pullTranscript,
  RESTATE_WINDOWS,
  readTranscript,
  removeSecrets,
  SECRET_REMOVED,
  SPOOL_CHUNK,
  STOP_AFTER_SESSIONS,
} from "./build";
import { type Case, loadCases, pathHash } from "./cases";
import {
  INSTALLERS_COLUMNS,
  LOADERS_COLUMNS,
  MANIFESTS_COLUMNS,
  PREFIX_TABLE_COLUMNS,
  REPOSITORIES_COLUMNS,
  SKILL_COPIES_COLUMNS,
  tsv,
} from "./copies";
import { mtimeRange } from "./countsql";
import { blindOrder } from "./judge";
import { psqlRows } from "./psql-rows";
import { READER_TRIES } from "./readers";
import {
  messageSse,
  type RecordedRequest,
  type Recorder,
  sseResponse,
  startRecorder,
} from "./recorder";

// The case and task builders on made-up sessions. Every session is F1 (testdata/F1, an Oh My Pi
// 18.18.6 session of a scratch profile with the made-up skills alpha and beta; CAPTURE.md, "F1"),
// changed in one way per session. F1's messages: 0 the user's prompt, 1 a read of skill://alpha,
// 2 its result, 3 a read of skill://alpha/references/x.md, 4 its result, 5 a read of the missing
// skill://gamma, 6 its result, 7 a `task` call (the step the stand-in reader proposes), 8-11 the
// rest. Message 5's text names the session (`Step 3 (case <name>).`), so the stand-in reader
// knows which session it reads.
//
// The store is a stand-in for the configuration's psqlCommand: a program that reads the query on
// its standard input and prints psql's aligned table of one JSON value per row (F7's shape) from
// a made-up store file, each transcript stored as the store keeps a file it imported and then
// appended to: its first three quarters in the file row, in slices of PART_CHARS characters, and
// the rest in parts of PART_CHARS from the file row's byte length on, so a pull pages.
// The gateway is a stand-in built on recorder.ts; GitHub is a stand-in gh over made-up
// repositories (REPOSITORIES), each as it was at the one commit SHA. The copies output is of a
// later read: it lists a skill `newer` that no repository held at SHA, and not `older`, which
// acme/skills held then. The count is three made-up query outputs, one per store. Every skill,
// repository and store here is made up, every path is made up or F1's (F1_SKILLS, the skills
// directory of F1's scratch profile, testdata/CAPTURE.md), and nothing reaches a network.

const TESTDATA = path.join(import.meta.dir, "testdata");
const F1 = path.join(
  TESTDATA,
  "F1",
  "2026-11-20T01-59-41-780Z_01a1f6ae-51d4-715f-a2a6-59f275bdff80.jsonl"
);
/** The skills directory F1's session read its skills from: its scratch profile's. */
const F1_SKILLS = "/legion/scratch/tmp/acme-1/t1-runs/f1/home/.omp/profiles/fixture/agent/skills";
const PART_CHARS = 200;
const SHA = "0123456789abcdef0123456789abcdef01234567";
const LINE = "2. Write each step's result on one line.";
const scratch = temporaryDirectory("skill-cuts-build", { real: true });

/** F1's lines, parsed, as a fresh copy. */
function f1Lines(): Record<string, unknown>[] {
  return readFileSync(F1, "utf8")
    .split("\n")
    .filter((line) => line.trim() !== "")
    .map((line) => JSON.parse(line) as Record<string, unknown>);
}

/** The `message` entries of `lines`, in order. */
function messageLines(lines: Record<string, unknown>[]): { message: SessionMessage }[] {
  return lines.filter((line) => line.type === "message") as unknown as {
    message: SessionMessage;
  }[];
}

/** F1's messages, in order. */
function f1Messages(): SessionMessage[] {
  return f1Lines().flatMap((line) => {
    const entry = SessionEntry.parse(line);
    return entry.type === "message" && entry.message ? [entry.message] : [];
  });
}

/** Puts `entry` into the branch after `after`, its child now `entry`'s. */
function insertAfter(
  lines: Record<string, unknown>[],
  after: Record<string, unknown>,
  entry: Record<string, unknown>
) {
  const child = lines.find((line) => line.parentId === after.id);
  entry.parentId = after.id;
  if (child !== undefined) child.parentId = entry.id;
  lines.splice(lines.indexOf(after) + 1, 0, entry);
}

/** Puts `count` assistant messages, each of `text(n)`, between F1's message 6 and its step. */
function addFillers(lines: Record<string, unknown>[], count: number, text: (n: number) => string) {
  let previous = lines.filter((line) => line.type === "message")[6] ?? {};
  for (let n = 0; n < count; n += 1) {
    const filler = {
      type: "message",
      id: `f111${String(n).padStart(4, "0")}`,
      timestamp: "2026-11-20T01:59:53.300Z",
      message: { role: "assistant", content: [{ type: "text", text: text(n) }] },
    };
    insertAfter(lines, previous, filler);
    previous = filler;
  }
}

/** Replaces the text of the first block of message `index`. */
function setText(lines: Record<string, unknown>[], index: number, text: (old: string) => string) {
  const block = messageLines(lines)[index]?.message.content?.[0];
  if (block === undefined) throw new Error(`F1 has no message ${index}`);
  block.text = text(block.text ?? "");
}

/** The target of the read call of message `index`. */
function setReadTarget(lines: Record<string, unknown>[], index: number, target: string) {
  const call = messageLines(lines)[index]?.message.content?.find(
    (block) => block.type === "toolCall"
  );
  if (call === undefined) throw new Error(`F1's message ${index} holds no call`);
  call.arguments = { ...(call.arguments as Record<string, unknown>), path: target };
}

/** Sets the model F1's `model_change` line names. */
function setModel(lines: Record<string, unknown>[], model: string) {
  const change = lines.find((line) => line.type === "model_change");
  if (change === undefined) throw new Error("F1 has no model_change line");
  change.model = model;
}

/** Moves F1's reads of alpha to the skill directory `dir`: the `[Skill file: …]` line of each of
 *  its two read results. */
function readFrom(dir: string) {
  return (lines: Record<string, unknown>[]) => {
    for (const index of [2, 4]) {
      setText(lines, index, (text) => text.replace(`${F1_SKILLS}/alpha`, dir));
    }
  };
}

/** A change of F1 that puts a read of `url` (`skill://<name>`) and its successful result, which
 *  starts `[Skill file: <header>]` and then holds `text`, after F1's prompt: the two lines' ids are
 *  `<id>1` and `<id>2`. */
function readFirst(id: string, url: string, header: string, text: string) {
  return (lines: Record<string, unknown>[]) => {
    const prompt = lines.filter((line) => line.type === "message")[0] ?? {};
    const callId = `toolu_${url.slice("skill://".length)}`;
    const call = {
      type: "message",
      id: `${id}1`,
      timestamp: "2026-11-20T01:59:45.000Z",
      message: {
        role: "assistant",
        content: [{ type: "toolCall", id: callId, name: "read", arguments: { path: url } }],
      },
    };
    const result = {
      type: "message",
      id: `${id}2`,
      timestamp: "2026-11-20T01:59:45.100Z",
      message: {
        role: "toolResult",
        toolCallId: callId,
        toolName: "read",
        isError: false,
        content: [{ type: "text", text: `[Skill file: ${header}]\n${text}` }],
      },
    };
    insertAfter(lines, prompt, call);
    insertAfter(lines, call, result);
  };
}

/** Where the renamed-plugin session reads zeta: Oh My Pi's cache of the plugin zeta-tools of the
 *  marketplace old-market (`plugins/cache/plugins/<marketplace>___<plugin>___<version>/`). */
const OLD_MARKET_ZETA =
  "/home/dev/.omp/plugins/cache/plugins/old-market___zeta-tools___0.1.0/skills/zeta/SKILL.md";

const SECRET_TOKEN = fixtureSecret("tok");
const PEM_BODY = fixtureSecret("pem");
const PEM = [
  `-----BEGIN RSA PRIVATE ${"KEY"}-----`,
  PEM_BODY,
  `-----END RSA PRIVATE ${"KEY"}-----`,
].join("\n");

/** Each made-up session: its store, how it changes F1, and its path when not `/sessions/<name>`. */
const SESSIONS: Record<
  string,
  { store: string; change?: (lines: Record<string, unknown>[]) => void; path?: string }
> = {
  good: { store: "store_a" },
  "not-in-base": { store: "store_a" },
  "read-lacks-line": {
    store: "store_a",
    change: (lines) => setText(lines, 2, (text) => text.replace(`${LINE}\n`, "")),
  },
  // The x.md read reads another path, so the session reads only SKILL.md of the skill.
  "skill-md-only": {
    store: "store_a",
    change: (lines) => setReadTarget(lines, 3, "/tmp/notes.md"),
  },
  // The skill's text changed after the session read it: no line of its reads is in the base.
  "no-common": {
    store: "store_a",
    change: (lines) => {
      for (const index of [2, 4]) {
        setText(lines, index, (text) => `${text.split("\n")[0]}\nAn older text of the file.`);
      }
    },
  },
  compacted: {
    store: "store_a",
    change: (lines) => {
      const messages = lines.filter((line) => line.type === "message");
      const kept = messages[5] ?? {};
      insertAfter(lines, messages[6] ?? {}, compaction(kept.id));
    },
  },
  "compacted-before-read": {
    store: "store_a",
    change: (lines) => {
      const first = lines.filter((line) => line.type === "message")[0] ?? {};
      insertAfter(lines, first, compaction(first.id));
    },
  },
  quoted: {
    store: "store_a",
    change: (lines) => setText(lines, 3, () => "Step 2. Write each step's result on one line."),
  },
  // The user's prompt, before the skill's read, states the line in other words.
  restated: {
    store: "store_a",
    change: (lines) => setText(lines, 0, (text) => `${text} ${RESTATED}`),
  },
  // A model whose id the gateway serves, under another provider than Anthropic.
  openai: {
    store: "store_a",
    change: (lines) => setModel(lines, "openai/gpt-6.1-sol"),
  },
  // An Anthropic model behind a gateway's prefix: served, since its id is.
  "gateway-model": {
    store: "store_a",
    change: (lines) => setModel(lines, "ossv-gateway/claude-opus-5-5"),
  },
  // A model no route serves: dropped before any reader reads the session.
  "not-served": {
    store: "store_a",
    change: (lines) => setModel(lines, "openai-codex/gpt-6-astra"),
  },
  // A served model at the skill's read, then one no route serves, before the step.
  "switched-model": {
    store: "store_a",
    change: (lines) =>
      insertAfter(lines, lines.filter((line) => line.type === "message")[6] ?? {}, {
        type: "model_change",
        id: "c0de0001",
        timestamp: "2026-11-20T01:59:53.300Z",
        model: "openai-codex/gpt-6-astra",
      }),
  },
  "by-path": {
    store: "store_a",
    change: (lines) => setReadTarget(lines, 5, `${F1_SKILLS}/alpha/SKILL.md`),
  },
  "no-read": {
    store: "store_a",
    change: (lines) => {
      setReadTarget(lines, 1, "skill://beta");
      setReadTarget(lines, 3, "skill://beta/references/x.md");
    },
  },
  "no-step": { store: "store_a" },
  "other-copy": {
    store: "store_a",
    change: (lines) => {
      for (const index of [2, 4])
        setText(lines, index, (text) =>
          text.replace(`${F1_SKILLS}/alpha`, "/elsewhere/other-skills/alpha")
        );
    },
  },
  // A read in a checkout of the cut's repository: no prefix row maps it, and the path names
  // acme/skills before the skill's directory.
  checkout: { store: "store_a", change: readFrom("/home/dev/acme/skills/skills/alpha") },
  // The same skill directory in a checkout of a fork: the path names forker/skills.
  "checkout-fork": { store: "store_a", change: readFrom("/home/dev/forker/skills/skills/alpha") },
  // A path no prefix row maps and no copy's directory ends: the store's context places it. The
  // count's reads of store_a come from a unit of acme/skills, those of store_c from acme/other's.
  context: { store: "store_a", change: readFrom("/mnt/loaded/alpha") },
  "context-other": { store: "store_c", change: readFrom("/mnt/loaded/alpha") },
  // A read the prefix table places at the cut's copy, through a plugin whose manifest acme/skills
  // did not hold at the session's start: its profile ships no alpha.
  unshipped: { store: "store_a", change: readFrom("/opt/acme-tools/skills/alpha") },
  // Before its read of alpha, the session reads acme/other's delta, whose unit (acme/other's
  // user skills) ships acme/other's alpha too and comes first; the session read acme/skills's
  // alpha, which is the copy its host gave it.
  "read-wins": {
    store: "store_a",
    change: readFirst(
      "de17a00",
      "skill://delta",
      "/elsewhere/other-skills/delta/SKILL.md",
      "# delta\n"
    ),
  },
  // Before its read of alpha, the session reads zeta from Oh My Pi's cache of a plugin of
  // acme/market's marketplace under the name the marketplace had at the session's start
  // (old-market); the copies output's prefix table names the plugin under its later name
  // (new-market), whose unit ships nothing at that commit. acme/market holds Claude Code's
  // settings too.
  "renamed-plugin": {
    store: "store_a",
    change: readFirst("2e7a000", "skill://zeta", OLD_MARKET_ZETA, "# zeta\n"),
  },
  secret: {
    store: "store_a",
    change: (lines) =>
      setText(
        lines,
        0,
        (text) => `${text} Sign in with Bearer ${SECRET_TOKEN} and this key:\n${PEM}`
      ),
  },
  legion: {
    store: "store_b",
    change: (lines) => {
      const header = lines.find((line) => line.type === "session");
      if (header !== undefined) header.cwd = "/legion/workspaces/acme/widgets/acme-12";
      setText(
        lines,
        0,
        () => "Continue Make the widget. Issue: ACME-12. Phase: testing. Handoff: none."
      );
      for (const index of [2, 4])
        setText(lines, index, (text) =>
          text.replace(`${F1_SKILLS}/alpha`, "/opt/legion/pi-acme/dist/skills/alpha")
        );
    },
  },
  "no-role": { store: "store_b" },
  // A Legion root architect, which the daemon starts with "<KEY> is yours and …".
  "root-architect": {
    store: "store_b",
    change: (lines) =>
      setText(
        lines,
        0,
        () =>
          "ACME-12 is yours and its tree is open at generation 1, holding an admission slot while nothing in it runs. Take it up in this turn, as your role says."
      ),
  },
  // A Legion architect of a child issue, whose first message is the daemon's catch-up notice.
  "child-architect": {
    store: "store_b",
    change: (lines) => insertAfter(lines, branchHead(lines), catchUpNotice()),
  },
  // A Legion reviewer an architect's Envoy message woke: its role is the claim it holds, beside
  // the workspace its path names.
  "claimed-reviewer": {
    store: "store_b",
    path: "/sessions/--legion-workspaces-acme-widgets-acme-12--/claimed.jsonl",
    change: (lines) =>
      insertAfter(lines, branchHead(lines), {
        type: "custom",
        customType: "envoy-role-claim",
        id: "c1a1d001",
        timestamp: "2026-11-20T01:59:43.600Z",
        data: { role: "legion-acme-acme-12-reviewer", session_id: "made-up" },
      }),
  },
  // A subagent's session, whose session_init line records its system prompt and tools.
  subagent: {
    store: "store_a",
    change: (lines) =>
      insertAfter(lines, branchHead(lines), {
        type: "session_init",
        id: "5e551017",
        timestamp: "2026-11-20T01:59:43.500Z",
        agent: "task",
        resolvedModel: "anthropic/claude-sonnet-5-5",
        task: "Read the alpha steps.",
        tools: SUBAGENT_TOOLS,
        systemPrompt: SUBAGENT_PROMPT,
      }),
  },
  "bad-init": {
    store: "store_a",
    change: (lines) =>
      insertAfter(lines, branchHead(lines), {
        type: "session_init",
        id: "5e551018",
        timestamp: "2026-11-20T01:59:43.500Z",
        agent: "task",
        tools: SUBAGENT_TOOLS,
      }),
  },
  // FILLERS messages between the skill's read and the step, so the step is in the third window.
  "third-window": {
    store: "store_a",
    change: (lines) => addFillers(lines, FILLERS, (n) => `Still working (${n}).`),
  },
  // The user's prompt states the line, and the step comes late: the case reader, reading the
  // window of the step, does not see the prompt; the restatement check's first window does.
  "late-restated": {
    store: "store_a",
    change: (lines) => {
      setText(lines, 0, (text) => `${text} ${RESTATED}`);
      addFillers(lines, FILLERS, (n) => `Late filler ${n}: ${"x".repeat(4_000)}`);
    },
  },
  // Messages before the step that need more windows of the restatement check than it reads.
  "too-long": {
    store: "store_a",
    change: (lines) => addFillers(lines, 12, (n) => `Long filler ${n}: ${"y".repeat(100_000)}`),
  },
  // The restatement check's reader answers nothing it can read.
  "restate-fails": { store: "store_a" },
  // The audit finds the real step breaks the line, or that the line does not govern it.
  "audit-breaks": { store: "store_a" },
  "audit-not-governed": { store: "store_a" },
  broken: {
    store: "store_a",
    change: (lines) => lines.splice(6, 0, { notJson: true }),
  },
};

/** A compaction entry that keeps the entries from `kept` on. */
function compaction(kept: unknown): Record<string, unknown> {
  return {
    type: "compaction",
    id: "c0ffee01",
    timestamp: "2026-11-20T01:59:45.000Z",
    summary: "The user asked for the alpha steps of the made-up project.",
    firstKeptEntryId: kept,
    tokensBefore: 1200,
  };
}

/** The made-up path of session `name`. */
const sessionPath = (name: string) => SESSIONS[name]?.path ?? `/sessions/${name}.jsonl`;

/** The made-up session case `c` comes from. */
const nameOf = (c: Case) =>
  Object.keys(SESSIONS).find((name) => pathHash(sessionPath(name)) === c.source.pathHash) ?? "";

/** F1's `thinking_level_change` line, after which an added line opens the session's branch. */
function branchHead(lines: Record<string, unknown>[]): Record<string, unknown> {
  const head = lines.find((line) => line.type === "thinking_level_change");
  if (head === undefined) throw new Error("F1 has no thinking_level_change line");
  return head;
}

/** The daemon's catch-up notice to the architect of a child issue, as an Envoy message line. */
function catchUpNotice(): Record<string, unknown> {
  return {
    type: "custom_message",
    customType: "envoy-message",
    id: "e0e0e001",
    timestamp: "2026-11-20T01:59:43.600Z",
    display: true,
    content: [
      "envoy:",
      "  from: agent",
      '  at: "2026-11-20T01:59:43Z"',
      "  summary: catch-up on ACME-12",
      "  message:",
      "    kind: catch-up",
      "    role: architect",
      '    reason: "the issues under ACME-12, which you own, at generation 1 as the daemon records them now; start or resume your issue as your role says"',
      "",
    ].join("\n"),
  };
}

/** The subagent session's recorded system prompt and tools. */
const SUBAGENT_PROMPT = [
  "You are a made-up subagent.\n<skills>\n- alpha: Use for the alpha steps.\n- beta: Use for the beta greeting.\n</skills>\n",
  "<project-context>\nA made-up project.\n</project-context>",
];
const SUBAGENT_TOOLS = ["read", "bash", "task"];

/** The subagent session's transcript with a recorded prompt whose skill listing has no line of
 *  alpha, so a load test of alpha in its context tests nothing. */
const unlistedSubagentText = () =>
  sessionText("subagent").replace("- alpha: Use for the alpha steps.\\n", "");
/** The messages the third-window session adds between the skill's read and its step. */
const FILLERS = 90;

/** Session `name`'s transcript text. */
function sessionText(name: string): string {
  const lines = f1Lines();
  setText(lines, 5, () => `Step 3 (case ${name}).`);
  SESSIONS[name]?.change?.(lines);
  return lines
    .map((line) => (line.notJson === true ? "{not json" : JSON.stringify(line)))
    .map((line) => `${line}\n`)
    .join("");
}

/** The tasks rows of the stand-in store: a first user message, the day its file is of. */
const TASK_ROWS = [
  {
    dayStart: mtimeRange("2026-11-20").from,
    path: "/sessions/task-1.jsonl",
    text: "Give me the alpha checklist for the fixture project.",
  },
  {
    dayStart: mtimeRange("2026-11-20").from,
    path: "/sessions/task-2.jsonl",
    text: "Fix the beta greeting.",
  },
  {
    dayStart: mtimeRange("2026-11-21").from,
    path: "/sessions/task-3.jsonl",
    text: "Write the Alpha report of step 2.",
  },
  {
    dayStart: mtimeRange("2026-11-21").from,
    path: "/sessions/task-4.jsonl",
    text: "Give me the alpha checklist for the fixture project.",
  },
];

/** The stand-in store command: it answers pull.sql, sessions.sql and tasks.sql from STORE. */
const STANDIN_PSQL = (storeFile: string, logFile: string) => `
import { appendFileSync, readFileSync } from "node:fs";
import { createHash } from "node:crypto";
const db = process.argv[2] ?? "";
const query = await Bun.stdin.text();
const store = JSON.parse(readFileSync(${JSON.stringify(storeFile)}, "utf8"))[db];
if (store === undefined) {
  process.stderr.write("psql: error: database " + db + " does not exist\\n");
  process.exit(2);
}
const literals = (text) => [...text.matchAll(/'((?:[^']|'')*)'/g)].map((m) => m[1].replaceAll("''", "'"));
let kind;
let rows;
let after;
const place = /\\(pc\\.o, s\\.k\\) > \\((-?\\d+), (-?\\d+)\\)/.exec(query);
if (query.includes("omp_session_files_parts pt") && place) {
  kind = "pull";
  const filePath = literals(/WHERE f\\.path = ('(?:[^']|'')*')/.exec(query)[1])[0];
  after = [Number(place[1]), Number(place[2])];
  const limit = Number(/LIMIT (\\d+)/.exec(query)[1]);
  const text = store.files[filePath];
  const pieces = [];
  if (text !== undefined) {
    // Stored as a file the store imported and then appended to: the file row holds the first
    // three quarters, the parts the rest from the file row's byte length on. Slices and parts are
    // of PART_CHARS, and of more for a long file, so a file is never more than 100 rows.
    const size = Math.max(${PART_CHARS}, Math.ceil(text.length / 100));
    const row = text.slice(0, Math.floor((text.length * 3) / 4));
    for (let at = 0, k = 0; at < row.length || k === 0; at += size, k += 1) {
      pieces.push({ o: -1, k, c: row.slice(at, at + size) });
    }
    let offset = Buffer.byteLength(row, "utf8");
    for (let at = row.length; at < text.length; at += size) {
      const c = text.slice(at, at + size);
      pieces.push({ o: offset, k: 0, c });
      offset += Buffer.byteLength(c, "utf8");
    }
  }
  rows = pieces
    .filter((piece) => piece.o > after[0] || (piece.o === after[0] && piece.k > after[1]))
    .slice(0, limit);
} else if (query.includes("left(md5(f.path), 16) = ANY")) {
  kind = "sessions";
  const hashes = new Set(literals(query.slice(query.indexOf("ANY"))));
  rows = Object.keys(store.files)
    .map((p) => ({ path: p, hash: createHash("md5").update(p).digest("hex").slice(0, 16) }))
    .filter((row) => hashes.has(row.hash));
} else if (query.includes("first user message")) {
  kind = "tasks";
  const dayStart = Number(/f\\.mtime_ms >= (\\d+)/.exec(query)[1]);
  // The terms of the ARRAY after unnest(, less the '' the query compares each term to.
  const terms = literals(query.slice(query.indexOf("unnest("))).filter((t) => t !== "");
  rows = store.tasks
    .filter((row) => row.dayStart === dayStart && terms.some((t) => row.text.toLowerCase().includes(t.toLowerCase())))
    .map((row) => ({ path: row.path, text: row.text }));
} else {
  process.stderr.write("psql: error: not a query of the runner\\n");
  process.exit(3);
}
appendFileSync(${JSON.stringify(logFile)}, JSON.stringify({ db, kind, rows: rows.length, after, first: rows[0] && [rows[0].o, rows[0].k], limit64: !query.includes("omp_session_files_parts pt") || query.includes("LIMIT 64") }) + "\\n");
process.stdout.write(" value \\n-------\\n" + rows.map((row) => " " + JSON.stringify(row) + "\\n").join("") + "(" + rows.length + (rows.length === 1 ? " row" : " rows") + ")\\n\\n");
`;

/** The words the `restated` session's prompt adds, which state LINE in other words. */
const RESTATED = "Put the result of each step on a single line.";

/** The criterion the stand-in case reader writes. */
const CRITERION = {
  pass: "The step writes each step's result on one line.",
  fail: "The step spreads a result over more than one line.",
};

/** The proposal of the stand-in case reader for the session named in the transcript: the step is
 *  the message of the window that calls the `task` tool (F1's message 7), and a window that holds
 *  none gives no step. */
function caseAnswer(name: string, transcript: string): unknown {
  const criterion = CRITERION;
  const step = transcript
    .split('<message index="')
    .find((message) => message.includes("[tool call: task]"))
    ?.split('"')[0];
  if (name === "no-step" || step === undefined)
    return { step: null, reason: "No step of the transcript is decided by the skill." };
  const text = name === "not-in-base" ? "2. Write each step's result on two lines." : LINE;
  return {
    step: { messageIndex: Number(step), what: "calls the task tool" },
    line: { file: "SKILL.md", text },
    criterion,
    violatingStep: { text: "Step 4.", toolCalls: [VIOLATING_CALL] },
    // Message 0, the user's prompt, comes before the skill's read: the reader sees it only in
    // the messages before the read.
    restatements: name === "restated" ? [{ messageIndex: 0, quote: RESTATED }] : [],
  };
}

/** The tool call of the stand-in reader's violating step. */
const VIOLATING_CALL = { name: "read", arguments: { path: "skill://beta" } };

/** A need task's statement that the skill governs its first step. */
const GOVERNS = { firstStep: { governed: true, reason: "the checklist comes first" } };

/** A written task of the stand-in tasks reader. */
const written = (label: string, n: number) => ({
  label,
  from: "written",
  text: `A made-up ${label} task ${n}.`,
  why: "made up",
  ...(label === "need" ? GOVERNS : {}),
});

/** The text of a first request whose first step the stand-in tasks reader says the skill does
 *  not govern. */
const UNGOVERNED = "(open the notes first)";

/** The stand-in tasks reader's answer in its control mode: each message a need task, whose first
 *  step the skill governs unless the message holds UNGOVERNED. */
function controlAnswer(user: string): unknown {
  const tasks = [...user.matchAll(/<message id="(m\d+)">\n([\s\S]*?)\n<\/message>/g)].map(
    ([, id, text]) => ({
      label: "need",
      from: "session",
      messageId: id,
      why: "asks for the checklist",
      firstStep: (text ?? "").includes(UNGOVERNED)
        ? { governed: false, reason: "a file is opened first" }
        : GOVERNS.firstStep,
    })
  );
  return { terms: [], tasks };
}

/** The made-up sessions of the positive control's tasks (build-tasks --first-read), in store_a at
 *  `/sessions/<name>.jsonl`: each is F1 with its first request replaced; c-late's first reply
 *  reads a notes file instead of the skill, and c-same asks what c-one asks. */
const CONTROL_SESSIONS: Record<string, { request: string; firstRead: boolean }> = {
  "c-one": { request: "Give me the alpha checklist for the release.", firstRead: true },
  "c-same": { request: "Give me the alpha checklist for the release.", firstRead: true },
  "c-secret": {
    request: `Run the alpha checklist on the API.\n> Authorization: Bearer ${SECRET_TOKEN}`,
    firstRead: true,
  },
  "c-late": { request: "Write the alpha report.", firstRead: false },
  "c-ungoverned": { request: `Fix the alpha checklist ${UNGOVERNED}.`, firstRead: true },
  "c-two": { request: "Walk the alpha checklist for the fixture project.", firstRead: true },
};

/** The made-up path of control session `name`. */
const controlPath = (name: string) => `/sessions/${name}.jsonl`;

/** Control session `name`'s transcript text. */
function controlText(name: string): string {
  const lines = f1Lines();
  const session = CONTROL_SESSIONS[name];
  setText(lines, 0, () => session?.request ?? "");
  if (session?.firstRead === false) setReadTarget(lines, 1, "/tmp/notes.md");
  return lines.map((line) => `${JSON.stringify(line)}\n`).join("");
}

/** The transcript text of the control session c-other-model, which no row of CONTROL_SESSIONS
 *  holds: F1 with its own first request, run on openai/gpt-6.1-sol, whose first reply reads
 *  alpha. */
function otherModelControlText(): string {
  const lines = f1Lines();
  setText(lines, 0, () => "Run the alpha checklist on the other model.");
  setModel(lines, "openai/gpt-6.1-sol");
  return lines.map((line) => `${JSON.stringify(line)}\n`).join("");
}

/** The first words of the restate and audit prompts, by which the stand-in tells their requests. */
const RESTATE_PROMPT = "You read part of a coding agent's session and find each message";
const AUDIT_PROMPT = "You decide whether one step of a coding agent's session follows";

/** The stand-in restatement check: each message of the window that holds RESTATED, quoted; and,
 *  for the restate-fails session, no JSON at all. */
function restateAnswer(name: string, window: string): string {
  if (name === "restate-fails") return "I could not read the window.";
  const restatements = window
    .split('<message index="')
    .slice(1)
    .filter((message) => message.includes(RESTATED))
    .map((message) => ({ messageIndex: Number(message.split('"')[0]), quote: RESTATED }));
  return JSON.stringify({ restatements });
}

/** The stand-in audit: the step follows the line, but in the audit-<verdict> sessions. */
function auditAnswer(name: string): unknown {
  if (name === "audit-breaks") {
    return { verdict: "breaks", reason: "The step spreads the result over two lines." };
  }
  if (name === "audit-not-governed") {
    return { verdict: "not-governed", reason: "The step writes no result." };
  }
  return { verdict: "follows", reason: "The step writes the result on one line." };
}

/** The stand-in reader's answer to `body`. */
function readerAnswer(body: {
  system: { text: string }[];
  messages: { content: { text: string }[] }[];
}): string {
  const system = body.system[0]?.text ?? "";
  const user = body.messages[0]?.content.map((part) => part.text).join("\n") ?? "";
  const name = /\(case ([a-z-]+)\)/.exec(user)?.[1] ?? "";
  if (system.startsWith(RESTATE_PROMPT)) return restateAnswer(name, user);
  if (system.startsWith(AUDIT_PROMPT)) return JSON.stringify(auditAnswer(name));
  if (system.startsWith("You read part of a coding agent's session")) {
    return JSON.stringify(caseAnswer(name, user));
  }
  if (system.startsWith("You pick the tasks of a load test")) {
    if (user.startsWith("<control>")) return JSON.stringify(controlAnswer(user));
    const fives = (label: string) => [1, 2, 3, 4, 5].map((n) => written(label, n));
    if (user.includes("None.")) {
      return JSON.stringify({ terms: ["alpha"], tasks: [...fives("need"), ...fives("near-miss")] });
    }
    // m1 is a need task, and m2, when it is offered, a near-miss one; else a fifth written one.
    const nearMiss = user.includes('<message id="m2">')
      ? [
          {
            label: "near-miss",
            from: "session",
            messageId: "m2",
            why: "a report, not the checklist",
          },
        ]
      : [written("near-miss", 5)];
    return JSON.stringify({
      terms: ["alpha"],
      tasks: [
        {
          label: "need",
          from: "session",
          messageId: "m1",
          why: "asks for the checklist",
          ...GOVERNS,
        },
        ...nearMiss,
        ...[1, 2, 3, 4].map((n) => written("need", n)),
        ...[1, 2, 3, 4].map((n) => written("near-miss", n)),
        // A need task whose first step the skill does not govern: the build leaves it out.
        {
          ...written("need", 5),
          firstStep: { governed: false, reason: "the notes are read first" },
        },
      ],
    });
  }
  return "{}";
}

let recorder: Recorder;

/** A made-up skill's SKILL.md. */
const skillMd = (name: string) =>
  `---\nname: ${name}\ndescription: Use when a task asks for ${name}.\n---\n\n# ${name}\n`;

/** Claude Code's user memory in acme/skills's `.claude/`: the user-level context file of a devbox
 *  session whose units come from acme/skills. */
const USER_RULES = "# Made-up user rules\n\nAnswer each question in one line.\n";

/** The made-up repositories as each was at the commit SHA: every file's path and text. acme/skills
 *  holds `older` and no `newer`; its installer links each skill of skills/ into Oh My Pi's user
 *  skills, and its prepack script stages alpha and beta for the package @acme/pi-acme; it holds
 *  Claude Code's settings and user memory in `.claude/`, as a dev box's config repository does.
 *  acme/other holds its own alpha and delta, which its installer links the same way. acme/market
 *  holds zeta and a marketplace that ships it in its plugin zeta-tools, under the marketplace's
 *  name at SHA (old-market; the copies output, read later, names it new-market), and Claude
 *  Code's settings. */
const INSTALLER = [
  "#!/usr/bin/env bash",
  'for skill in "$REPO"/skills/*/; do',
  '  ln -sfn "$skill" "$HOME/.omp/agent/skills/$(basename "$skill")"',
  "done",
  "",
].join("\n");
const REPOSITORIES: Record<string, Record<string, string>> = {
  "acme/other": {
    "skills/alpha/SKILL.md": skillMd("alpha"),
    "skills/delta/SKILL.md": skillMd("delta"),
    "installers/skills.sh": INSTALLER,
  },
  "acme/skills": {
    "skills/alpha/SKILL.md": skillMd("alpha"),
    "skills/alpha/references/x.md": "Step notes.\n",
    "skills/beta/SKILL.md": skillMd("beta"),
    "skills/older/SKILL.md": skillMd("older"),
    ".claude/settings.json": "{}\n",
    ".claude/CLAUDE.md": USER_RULES,
    "installers/skills.sh": INSTALLER,
    "packages/pi-acme/package.json": JSON.stringify({
      name: "@acme/pi-acme",
      omp: { skills: ["dist/skills"] },
    }),
    "scripts/pi-plugin-prepack.sh": [
      "#!/usr/bin/env bash",
      "plugin() {",
      '  case "$1" in',
      "  @acme/pi-acme)",
      "    skills=( alpha beta )",
      "    ;;",
      "  esac",
      "}",
      'cp -R "$root/skills/$skill" "$dest/$skill"',
      "stage_skills dist/skills",
      "",
    ].join("\n"),
    "docker/omp-plugins/package.json": JSON.stringify({
      name: "omp-plugins",
      dependencies: { "@acme/codegraph": "1.0.0" },
    }),
  },
  "acme/market": {
    "skills/zeta/SKILL.md": skillMd("zeta"),
    ".claude-plugin/marketplace.json": JSON.stringify({
      name: "old-market",
      owner: { name: "acme" },
      plugins: [{ name: "zeta-tools", source: "./" }],
    }),
    ".claude/settings.json": "{}\n",
  },
};

/** The blob id the stand-in gives a file's text. */
const blobId = (text: string) => createHash("sha1").update(text).digest("hex");
/** The one file a batch of blobs gives cut short, so it is read alone. */
const CUT_SHORT = REPOSITORIES["acme/skills"]?.["docker/omp-plugins/package.json"] ?? "";

/** The gh calls the stand-in took. */
const ghCalls: string[][] = [];

/** The stand-in gh: GET reads of commits, default branches, two files, each file of a made-up
 *  repository at SHA, and the tree and the blobs of each made-up repository at SHA; and GraphQL
 *  reads of blobs in a batch. The clock is the real one. */
const io = (lines: string[]): BuildIo => ({
  out: (text) => lines.push(text),
  err: (line) => lines.push(line),
  gh: async (args) => {
    ghCalls.push([...args]);
    const route = args.find((arg) => arg.startsWith("repos/")) ?? "";
    const ok = (stdout: string) => ({ code: 0, stdout, stderr: "" });
    if (args[1] === "graphql") {
      const field = (name: string) =>
        args.find((arg) => arg.startsWith(`${name}=`))?.slice(name.length + 1) ?? "";
      const texts = Object.values(REPOSITORIES[`${field("owner")}/${field("name")}`] ?? {});
      const blobs = [...field("query").matchAll(/(b\d+): object\(oid: "([0-9a-f]{40})"\)/g)].map(
        ([, alias, id]) => {
          const text = texts.find((each) => blobId(each) === id);
          if (text === undefined) return [alias, null];
          const cut = text === CUT_SHORT;
          return [alias, { isBinary: false, isTruncated: cut, text: cut ? null : text }];
        }
      );
      return ok(JSON.stringify({ data: { repository: Object.fromEntries(blobs) } }));
    }
    if (/^repos\/[^/]+\/[^/]+$/.test(route)) return ok("main\n");
    if (route.endsWith("/commits")) return ok(`${SHA}\n`);
    if (route === "repos/acme/widgets/contents/AGENTS.md") return ok("# Widgets\n");
    if (route === "repos/acme/infra/contents/legion/projects/10-ACME.yaml") {
      return ok("key: ACME\ninstructions: acme.md\n");
    }
    const [, owned = "", file = ""] = /^repos\/([^/]+\/[^/]+)\/contents\/(.+)$/.exec(route) ?? [];
    const text = REPOSITORIES[owned]?.[decodeURIComponent(file)];
    if (text !== undefined && args.includes(`ref=${SHA}`)) return ok(text);
    const [, repository = "", kind = "", id = ""] =
      /^repos\/([^/]+\/[^/]+)\/git\/(trees|blobs)\/([0-9a-f]{40})(?:\?recursive=1)?$/.exec(route) ??
      [];
    const files = Object.entries(REPOSITORIES[repository] ?? {});
    if (kind === "trees" && id === SHA && files.length > 0) {
      return ok(
        JSON.stringify({
          sha: SHA,
          truncated: false,
          tree: files.map(([file, text]) => ({
            path: file,
            mode: "100644",
            type: "blob",
            sha: blobId(text),
            size: text.length,
          })),
        })
      );
    }
    const blob = files.find(([, text]) => blobId(text) === id);
    if (kind === "blobs" && blob !== undefined) {
      return ok(
        JSON.stringify({ encoding: "base64", content: Buffer.from(blob[1]).toString("base64") })
      );
    }
    return { code: 1, stdout: "", stderr: "gh: Not Found (HTTP 404)\n" };
  },
  now: () => performance.now(),
});

const setupDir = path.join(scratch, "setup");
const config = path.join(setupDir, "skill-cuts.json");
const keyFile = path.join(setupDir, "key");
const old = path.join(setupDir, "old", "alpha");
const copiesDir = path.join(setupDir, "copies");
const countDir = path.join(setupDir, "count");
const psqlLog = path.join(setupDir, "psql.log");

beforeAll(() => {
  recorder = startRecorder({
    route: (request: RecordedRequest) => {
      if (request.method !== "POST" || request.path !== "/v1/messages") return undefined;
      const body = JSON.parse(request.body);
      return sseResponse(
        messageSse({
          id: `msg_standin_${request.n}`,
          model: body.model,
          content: [{ type: "text", text: readerAnswer(body) }],
          stopReason: "end_turn",
          inputTokens: 100,
          outputTokens: 20,
        }),
        `req_standin_${request.n}`
      );
    },
  });
  mkdirSync(path.join(old, "references"), { recursive: true });
  const messages = f1Messages();
  writeFileSync(
    path.join(old, "SKILL.md"),
    `${stripSkillHeader(messages[2]?.content?.[0]?.text ?? "")}\n`
  );
  writeFileSync(
    path.join(old, "references", "x.md"),
    `${stripSkillHeader(messages[4]?.content?.[0]?.text ?? "")}\n`
  );

  const files: Record<string, Record<string, string>> = { store_a: {}, store_b: {}, store_c: {} };
  for (const [name, session] of Object.entries(SESSIONS)) {
    const store = files[session.store] ?? {};
    store[sessionPath(name)] = sessionText(name);
  }
  for (const name of Object.keys(CONTROL_SESSIONS)) {
    const store = files.store_a ?? {};
    store[controlPath(name)] = controlText(name);
  }
  // The sessions the tasks query's rows come from: task-1's is a subagent whose recorded listing
  // lacks alpha; task-3's is F1 as the openai session has it, on openai/gpt-6.1-sol; each other is
  // F1 as the good session has it, so its rebuilt profile is the good case's (and task-3's is
  // too: only its model differs). The control session c-unlisted is one like task-1's, and
  // c-other-model one whose first request ran on openai/gpt-6.1-sol.
  const storeA = files.store_a ?? {};
  const taskSessions: Record<string, string> = {
    "/sessions/task-1.jsonl": unlistedSubagentText(),
    "/sessions/task-3.jsonl": sessionText("openai"),
  };
  for (const row of TASK_ROWS) storeA[row.path] = taskSessions[row.path] ?? sessionText("good");
  storeA[controlPath("c-unlisted")] = unlistedSubagentText();
  storeA[controlPath("c-other-model")] = otherModelControlText();
  writeFileSync(
    path.join(setupDir, "store.json"),
    JSON.stringify({
      store_a: { files: files.store_a, tasks: TASK_ROWS },
      store_b: { files: files.store_b, tasks: [] },
      store_c: { files: files.store_c, tasks: [] },
    })
  );
  writeFileSync(
    path.join(setupDir, "psql.ts"),
    STANDIN_PSQL(path.join(setupDir, "store.json"), psqlLog)
  );
  const psql = path.join(setupDir, "psql");
  writeFileSync(
    psql,
    `#!/bin/sh\nexec '${process.execPath}' '${path.join(setupDir, "psql.ts")}' "$@"\n`
  );
  chmodSync(psql, 0o755);

  const fixture = JSON.parse(readFileSync(path.join(TESTDATA, "F7-skill-cuts.json"), "utf8"));
  writeFileSync(
    config,
    JSON.stringify({
      ...fixture,
      stores: [
        ...fixture.stores,
        { name: "store_b", context: "legion:ACME" },
        { name: "store_c", context: "devbox" },
      ],
      servedModels: [...fixture.servedModels, "anthropic/claude-opus-5-5"],
      psqlCommand: psql,
      gatewayBaseUrl: recorder.url,
    })
  );
  writeFileSync(keyFile, fixtureSecret("gw"), { mode: 0o600 });

  mkdirSync(copiesDir, { recursive: true });
  const userSkills = "user-skills:omp (acme/skills:installers/skills.sh)";
  const otherSkills = "user-skills:omp (acme/other:installers/skills.sh)";
  const pkg = "package:@acme/pi-acme (acme/skills:packages/pi-acme/package.json)";
  const market = "plugin:zeta-tools@new-market (acme/market:.claude-plugin/marketplace.json)";
  writeFileSync(
    path.join(copiesDir, "prefix-table.tsv"),
    tsv(PREFIX_TABLE_COLUMNS, [
      ["**/agent/skills/alpha/", "acme/skills", "skills/alpha", "alpha", userSkills],
      ["**/agent/skills/beta/", "acme/skills", "skills/beta", "beta", userSkills],
      ["**/agent/skills/newer/", "acme/skills", "skills/newer", "newer", userSkills],
      ["/elsewhere/other-skills/alpha/", "acme/other", "skills/alpha", "alpha", otherSkills],
      ["/elsewhere/other-skills/delta/", "acme/other", "skills/delta", "delta", otherSkills],
      ["/opt/legion/pi-acme/dist/skills/alpha/", "acme/skills", "skills/alpha", "alpha", pkg],
      ["/opt/legion/pi-acme/dist/skills/beta/", "acme/skills", "skills/beta", "beta", pkg],
      ["/opt/legion/pi-acme/dist/skills/newer/", "acme/skills", "skills/newer", "newer", pkg],
      [
        "/opt/acme-tools/skills/alpha/",
        "acme/skills",
        "skills/alpha",
        "alpha",
        "plugin:acme-tools (acme/skills:plugins/acme-tools/.claude-plugin/plugin.json)",
      ],
      [
        "**/plugins/cache/plugins/new-market___zeta-tools___*/skills/zeta/",
        "acme/market",
        "skills/zeta",
        "zeta",
        market,
      ],
    ])
  );
  const installer = (
    repo: string,
    manifest: string,
    kind: string,
    action: string,
    unit: string,
    skill: string,
    copy: string,
    note = ""
  ) => [repo, manifest, kind, action, unit, "omp", skill, skill === "" ? "" : repo, copy, note];
  writeFileSync(
    path.join(copiesDir, "installers.tsv"),
    tsv(INSTALLERS_COLUMNS, [
      installer(
        "acme/skills",
        "installers/skills.sh",
        "installer-script",
        "links",
        "user-skills:omp",
        "alpha",
        "skills/alpha"
      ),
      installer(
        "acme/skills",
        "installers/skills.sh",
        "installer-script",
        "links",
        "user-skills:omp",
        "beta",
        "skills/beta"
      ),
      installer(
        "acme/skills",
        "installers/skills.sh",
        "installer-script",
        "links",
        "user-skills:omp",
        "newer",
        "skills/newer"
      ),
      installer(
        "acme/skills",
        "packages/pi-acme/package.json",
        "omp-package",
        "ships",
        "package:@acme/pi-acme",
        "newer",
        "skills/newer"
      ),
      installer(
        "acme/other",
        "installers/skills.sh",
        "installer-script",
        "links",
        "user-skills:omp",
        "alpha",
        "skills/alpha"
      ),
      installer(
        "acme/skills",
        "packages/pi-acme/package.json",
        "omp-package",
        "ships",
        "package:@acme/pi-acme",
        "alpha",
        "skills/alpha"
      ),
      installer(
        "acme/skills",
        "packages/pi-acme/package.json",
        "omp-package",
        "ships",
        "package:@acme/pi-acme",
        "beta",
        "skills/beta"
      ),
      installer(
        "acme/skills",
        "docker/omp-plugins/package.json",
        "omp-plugins",
        "installs",
        "package:@acme/codegraph",
        "",
        "",
        "no package @acme/codegraph with skills was read"
      ),
      installer(
        "acme/market",
        ".claude-plugin/marketplace.json",
        "claude-marketplace",
        "ships",
        "plugin:zeta-tools@new-market",
        "zeta",
        "skills/zeta"
      ),
    ])
  );
  writeFileSync(
    path.join(copiesDir, "manifests.tsv"),
    tsv(MANIFESTS_COLUMNS, [
      [
        "acme/infra",
        "legion/projects/10-ACME.yaml",
        "legion-project",
        "tree",
        0,
        "project ACME: verificationSkillsLeaveOut none",
        "",
      ],
    ])
  );
  writeFileSync(
    path.join(copiesDir, "repositories.tsv"),
    tsv(REPOSITORIES_COLUMNS, [
      ["acme/skills", "main", SHA, "no", "no", "", 10, "recursive", 3, ""],
      ["acme/other", "main", SHA, "no", "no", "", 10, "recursive", 1, ""],
      ["forker/skills", "main", SHA, "yes", "no", "acme/skills", 10, "recursive", 1, ""],
      ["acme/market", "main", SHA, "no", "no", "", 10, "recursive", 1, ""],
    ])
  );
  writeFileSync(
    path.join(copiesDir, "skill-copies.tsv"),
    tsv(SKILL_COPIES_COLUMNS, [
      ["acme/other", "skills/alpha", "alpha", 10, "no", "no", "", "", ""],
      ["acme/other", "skills/delta", "delta", 10, "no", "no", "", "", ""],
      ["acme/skills", "skills/alpha", "alpha", 10, "no", "no", "", "", ""],
      ["acme/skills", "skills/beta", "beta", 10, "no", "no", "", "", ""],
      ["acme/skills", "skills/newer", "newer", 10, "no", "no", "", "", ""],
      ["forker/skills", "skills/alpha", "alpha", 10, "yes", "no", "acme/skills", "", ""],
      ["acme/market", "skills/zeta", "zeta", 10, "no", "no", "", "", ""],
    ])
  );
  writeFileSync(path.join(copiesDir, "loaders.tsv"), tsv(LOADERS_COLUMNS, []));

  // The count: each store's reads of alpha, by the copy path the count gives them. store_a's come
  // from acme/skills's user skills, store_b's from the package the image carries, store_c's from
  // acme/other's user skills; so the context of store_a and store_b installs acme/skills's alpha,
  // and that of store_c acme/other's.
  for (const [store, copy] of [
    ["store_a", `${F1_SKILLS}/alpha/SKILL.md`],
    ["store_b", "/opt/legion/pi-acme/dist/skills/alpha/SKILL.md"],
    ["store_c", "/elsewhere/other-skills/alpha/SKILL.md"],
  ] as const) {
    const rows = [
      {
        row: "count",
        store,
        day: "2026-11-20",
        skill: "alpha",
        copy,
        kind: "read",
        files: 1,
        hashes: ["0123456789abcdef"],
        unpaired: 0,
        unpaired_hashes: [],
      },
      {
        row: "summary",
        store,
        mtime_from: 0,
        mtime_to: 0,
        files: 1,
        parts: 1,
        bytes: 1,
        lines_parsed: 1,
        bad_lines: 0,
        shared_call_ids: 0,
      },
    ];
    mkdirSync(path.join(countDir, store), { recursive: true });
    writeFileSync(
      path.join(countDir, store, "2026-11-20.txt"),
      [
        " total",
        "-".repeat(40),
        ...rows.map((row) => ` ${JSON.stringify(row)}`),
        `(${rows.length} rows)`,
        "",
      ].join("\n")
    );
  }
});
afterAll(async () => {
  await recorder.close();
});

/** A sessions file of `names` (each with its path but `withoutPath`'s) and `extra` lines. */
function sessionsFile(
  file: string,
  names: readonly string[],
  withoutPath: readonly string[] = [],
  extra: unknown[] = []
) {
  const lines = names.map((name) => {
    const store = SESSIONS[name]?.store ?? "store_a";
    const hash = fileHash(sessionPath(name));
    return withoutPath.includes(name) ? { store, hash } : { store, hash, path: sessionPath(name) };
  });
  writeFileSync(
    path.join(setupDir, file),
    [...lines, ...extra].map((line) => `${JSON.stringify(line)}\n`).join("")
  );
  return path.join(setupDir, file);
}

/** The arguments of one build-cases run. */
function buildArgs(sessions: string, out: string, seed: string, ...extra: string[]): string[] {
  return [
    "build-cases",
    "--config",
    config,
    "--key-file",
    keyFile,
    "--cut",
    "skills-1",
    "--skill",
    "alpha",
    "--old",
    old,
    "--sessions",
    sessions,
    "--seed",
    seed,
    "--copies",
    copiesDir,
    "--count",
    countDir,
    "--out",
    out,
    ...extra,
  ];
}

interface Summary {
  seed: string;
  order: { store: string; hash: string }[];
  sessionsTried: number;
  casesBuilt: number;
  drops: Partial<Record<DropReason, number>>;
  sessions: {
    hash: string;
    outcome: string;
    reason?: DropReason;
    detail?: string;
    pulls?: number;
    context?: string;
    model?: string;
    readerUsd: number;
    windows?: number;
    restatementCheck?: { read: number; before: number; windows: number };
    audit?: { verdict: string; reason: string };
  }[];
  readerSpend: { usd: number; models: Record<string, { calls: number }> };
  byModel: Record<string, { tried: number; drops: Partial<Record<DropReason, number>> }>;
}

const summaryOf = (out: string): Summary => JSON.parse(readFileSync(`${out}.summary.json`, "utf8"));

describe("build-cases over every made-up session", () => {
  const out = path.join(scratch, "all", "cases.jsonl");
  const printed: string[] = [];
  let code = -1;
  let cases: Case[] = [];
  const byName = (name: string) =>
    cases.find((each) => each.source.pathHash === pathHash(sessionPath(name)));

  /** The requests the stand-in gateway took for this build. */
  let requests: RecordedRequest[] = [];
  beforeAll(async () => {
    const names = [...Object.keys(SESSIONS), "missing"];
    const sessions = sessionsFile("all.jsonl", names, ["good", "missing"]);
    const from = recorder.requests.length;
    code = await main(buildArgs(sessions, out, "7"), io(printed));
    requests = recorder.requests.slice(from);
    cases = loadCases(out, old);
  }, 120_000);

  test("exits 0 and writes cases that loadCases loads", () => {
    expect(printed.join("")).not.toContain("failed");
    expect(code).toBe(0);
    expect(cases.map((each) => each.source.pathHash).sort()).toEqual(
      [
        "good",
        "compacted-before-read",
        "secret",
        "legion",
        "root-architect",
        "child-architect",
        "claimed-reviewer",
        "subagent",
        "third-window",
        "openai",
        "gateway-model",
        "skill-md-only",
        "checkout",
        "context",
        "read-wins",
        "renamed-plugin",
      ]
        .map((name) => pathHash(sessionPath(name)))
        .sort()
    );
  });

  test("a case of F1 holds the messages before the step, the step, the skill's reads, the model and thinking", () => {
    const c = byName("good");
    const messages = f1Messages();
    expect(c?.messages).toHaveLength(7);
    expect(c?.messages[0]).toEqual(messages[0] as SessionMessage);
    expect(c?.messages[2]).toEqual(messages[2] as SessionMessage);
    expect(c?.realStep).toEqual(messages[7] as SessionMessage);
    // The reader's violating step, as an assistant message: its text, then its tool call.
    expect(c?.violatingStep).toEqual({
      role: "assistant",
      content: [
        { type: "text", text: "Step 4." },
        { type: "toolCall", id: "violating_1", ...VIOLATING_CALL },
      ],
    });
    expect(c?.skillReads).toEqual([2, 4]);
    expect(c?.model).toBe("anthropic/claude-sonnet-5-5");
    expect(c?.thinking).toBe("off");
    expect(c?.line).toEqual({ text: LINE, lineNumber: 9, lineCount: 10 });
    expect(c?.baseFile).toBe("SKILL.md");
    expect(c?.context).toBe("devbox");
    expect(c?.sessionStart).toBe("2026-11-20T01:59:41.780Z");
    expect(c?.sessionDigest).toBe("46874d907e557998");
    expect(c?.source).toEqual({ store: "store_a", pathHash: pathHash(sessionPath("good")) });
    expect(c?.secretsRemoved).toBe(0);
    expect(c?.id).toBe(`skills-1-${pathHash(sessionPath("good")).slice(0, 12)}`);
    // No path text in the case beyond the transcript's own messages.
    expect(JSON.stringify({ ...c, messages: [], realStep: {} })).not.toContain(sessionPath("good"));
  });

  test("the case reader reads the messages before the skill's read too, and a restatement there drops the case", () => {
    const summary = summaryOf(out);
    const record = (name: string) =>
      summary.sessions.find((each) => each.hash === fileHash(sessionPath(name)));
    expect(record("restated")).toMatchObject({
      reason: "history-restates-line",
      detail:
        "the case reader finds the line's instruction stated in message 0 of the session, before the step at message 7",
    });
    // Its own finding drops the case before the restatement check runs.
    expect(record("restated")?.restatementCheck).toBeUndefined();
    // The reader's request held message 0 with its index.
    const asked = recorder.requests.find((request) => request.body.includes("(case restated)"));
    expect(asked?.body).toContain('<message index=\\"0\\" role=\\"user\\">');
    expect(byName("restated")).toBeUndefined();
  });

  test("the restatement check reads every message the case keeps before the step, less the skill's reads, and a restatement in its first window drops a late step's case", () => {
    const summary = summaryOf(out);
    const record = (name: string) =>
      summary.sessions.find((each) => each.hash === fileHash(sessionPath(name)));
    // F1's 7 messages before the step, less the skill's two read results, in one window.
    expect(record("good")?.restatementCheck).toEqual({ read: 5, before: 5, windows: 1 });
    expect(record("third-window")?.restatementCheck).toEqual({
      read: 5 + FILLERS,
      before: 5 + FILLERS,
      windows: 1,
    });
    // The late step's case reader window held no message 0; the check's first window did.
    const late = record("late-restated");
    expect(late).toMatchObject({ reason: "history-restates-line" });
    expect(late?.detail).toStartWith(
      "the restatement check finds the line's instruction stated in message 0 of the case, before the step (window 1 of "
    );
    expect(late?.detail).toEndWith(`: 0: ${JSON.stringify(RESTATED)}`);
    expect(late?.restatementCheck?.windows).toBeGreaterThan(1);
    expect(late?.restatementCheck?.read).toBeLessThan(late?.restatementCheck?.before ?? 0);
    const restating = requests.filter(
      (request) => request.body.includes(RESTATE_PROMPT) && request.body.includes("Late filler")
    );
    expect(restating).toHaveLength(1);
    // Every kept case's check read every message it must.
    for (const c of cases) {
      const check = summary.sessions.find(
        (each) => each.hash === fileHash(sessionPath(nameOf(c)))
      )?.restatementCheck;
      expect(check?.before).toBeNumber();
      expect(check?.read).toBe(check?.before as number);
    }
  });

  test("a check that cannot read every message before the step drops the case, whether its messages need too many windows or a window's reader fails", () => {
    const summary = summaryOf(out);
    const record = (name: string) =>
      summary.sessions.find((each) => each.hash === fileHash(sessionPath(name)));
    const tooLong = record("too-long");
    expect(tooLong).toMatchObject({ reason: "restatement-check-incomplete" });
    expect(tooLong?.detail).toContain(`more than ${RESTATE_WINDOWS}`);
    expect(tooLong?.restatementCheck?.windows).toBeGreaterThan(RESTATE_WINDOWS);
    expect(tooLong?.restatementCheck?.read).toBe(0);
    // No restatement call was made for it: its windows were counted first.
    expect(
      requests.filter(
        (request) => request.body.includes(RESTATE_PROMPT) && request.body.includes("Long filler")
      )
    ).toHaveLength(0);
    const fails = record("restate-fails");
    expect(fails).toMatchObject({ reason: "restatement-check-incomplete" });
    expect(fails?.detail).toStartWith("window 1 of 1 of the restatement check was not read");
    expect(fails?.restatementCheck).toEqual({ read: 0, before: 5, windows: 1 });
    expect(
      requests.filter(
        (request) =>
          request.body.includes(RESTATE_PROMPT) && request.body.includes("(case restate-fails)")
      )
    ).toHaveLength(READER_TRIES);
  });

  test("the audit reads the line, the messages before the step and the real step, and no criterion; only a step that follows the line is kept", () => {
    const summary = summaryOf(out);
    const record = (name: string) =>
      summary.sessions.find((each) => each.hash === fileHash(sessionPath(name)));
    expect(record("audit-breaks")).toMatchObject({
      reason: "audit-breaks",
      detail: "The step spreads the result over two lines.",
      audit: { verdict: "breaks", reason: "The step spreads the result over two lines." },
    });
    expect(record("audit-not-governed")).toMatchObject({ reason: "audit-not-governed" });
    const evidence = byName("good")?.evidence?.find((each) => each.item.startsWith("audit "));
    expect(evidence).toEqual({
      item: "audit of the real step: follows",
      source:
        "the audit reader, given the line, the messages before the step as the judge reads them and the real step, and no criterion: The step writes the result on one line.",
    });
    const audits = requests.filter((request) => request.body.includes(AUDIT_PROMPT));
    // One audit for each kept case and each case the audit dropped.
    expect(audits).toHaveLength(cases.length + 2);
    for (const request of audits) {
      expect(request.body).toContain(LINE);
      expect(request.body).not.toContain(CRITERION.pass);
      expect(request.body).not.toContain(CRITERION.fail);
    }
  });

  test("the profile is what the read's unit shipped at the commit of the session's start, not what the later copies output lists, each skill with its evidence", () => {
    const c = byName("good");
    expect(c?.profile).toEqual({
      skills: [
        { repo: "acme/skills", commit: SHA, path: "skills/alpha", name: "alpha" },
        { repo: "acme/skills", commit: SHA, path: "skills/beta", name: "beta" },
        { repo: "acme/skills", commit: SHA, path: "skills/older", name: "older" },
      ],
      plugins: [],
      contextFiles: [{ repo: "acme/skills", commit: SHA, path: ".claude/CLAUDE.md" }],
    });
    const evidence = Object.fromEntries(c?.evidence?.map((each) => [each.item, each.source]) ?? []);
    expect(evidence["unit user-skills:omp (acme/skills:installers/skills.sh)"]).toContain(
      `message 2 reads [Skill file: ${F1_SKILLS}/alpha/SKILL.md]`
    );
    expect(evidence[`commit ${SHA} of acme/skills`]).toContain("at or before the session's start");
    expect(evidence[`the skills and manifests of acme/skills at ${SHA}`]).toContain(
      "its 3 SKILL.md files and 5 manifest files"
    );
    expect(evidence["skill alpha (acme/skills skills/alpha)"]).toBe(
      `acme/skills at ${SHA}: user-skills:omp links it (acme/skills:installers/skills.sh); message 2 reads this copy`
    );
    // The copies output lists `newer`, which acme/skills did not hold at the commit, and not
    // `older`, which it held.
    expect(evidence["left out: skill newer (acme/skills skills/newer)"]).toContain(
      "user-skills:omp does not ship it at the session's start"
    );
    expect(evidence["skill older (acme/skills skills/older)"]).toContain(
      "installers.tsv, which the copies output read later, does not list it"
    );
    // A package ships what its prepack script lists at the commit.
    expect(byName("legion")?.profile.skills.map((skill) => skill.name)).toEqual(["alpha", "beta"]);
    // Every skill of every profile is in its repository at the profile's commit.
    for (const each of cases) {
      for (const skill of each.profile.skills) {
        expect(skill.commit).toBe(SHA);
        expect(REPOSITORIES[skill.repo]?.[`${skill.path}/SKILL.md`]).toBeDefined();
      }
    }
    const commitCall = ghCalls.find((args) => args.includes("repos/acme/skills/commits"));
    expect(commitCall).toContain("until=2026-11-20T01:59:41.780Z");
    expect(ghCalls.every((args) => args[0] === "api" && !args.includes("POST"))).toBe(true);
    // A repository's files are read in one batch; the one the batch gives cut short is read alone.
    expect(ghCalls.some((args) => args[1] === "graphql")).toBe(true);
    const alone = ghCalls.flatMap((args) => args.filter((arg) => arg.includes("/git/blobs/")));
    expect([...new Set(alone)]).toEqual([`repos/acme/skills/git/blobs/${blobId(CUT_SHORT)}`]);
  });

  test("a read is placed as attribute.ts places it: a read in a checkout of the cut's repository and one the store's context puts at its copy are kept; a checkout of a fork and a read the store's context puts at another copy drop, with the rule named", () => {
    const record = (name: string) =>
      summaryOf(out).sessions.find((each) => each.hash === fileHash(sessionPath(name)));
    const unit = (name: string) =>
      byName(name)?.evidence?.find(
        (each) => each.item === "unit user-skills:omp (acme/skills:installers/skills.sh)"
      )?.source;
    const checkout = (repository: string) =>
      `checkout: to ${repository} skills/alpha: the path ends with skills/alpha/, the skill directory of 3 repositories, and the path names ${repository}`;
    const context = (repository: string) =>
      `context: to ${repository} skills/alpha, the one copy of alpha its context installs`;
    expect(unit("checkout")).toContain(
      `message 2 reads [Skill file: /home/dev/acme/skills/skills/alpha/SKILL.md], which attribute.ts's checkout rule places at acme/skills skills/alpha (${checkout("acme/skills")})`
    );
    expect(unit("context")).toContain(
      `message 2 reads [Skill file: /mnt/loaded/alpha/SKILL.md], which attribute.ts's context rule places at acme/skills skills/alpha (${context("acme/skills")})`
    );
    // The profile of a read no prefix row maps is what the units that ship its copy shipped.
    for (const name of ["checkout", "context"]) {
      expect(byName(name)?.profile.skills.map((skill) => skill.name)).toEqual([
        "alpha",
        "beta",
        "older",
      ]);
    }
    expect(record("checkout-fork")).toMatchObject({
      reason: "not-cut-copy",
      detail: `the checkout rule places /home/dev/forker/skills/skills/alpha/SKILL.md at forker/skills skills/alpha (${checkout("forker/skills")})`,
    });
    expect(record("context-other")).toMatchObject({
      reason: "not-cut-copy",
      detail: `the context rule places /mnt/loaded/alpha/SKILL.md at acme/other skills/alpha (${context("acme/other")})`,
    });
    expect(record("other-copy")).toMatchObject({
      reason: "not-cut-copy",
      detail:
        "the prefix table rule places /elsewhere/other-skills/alpha/SKILL.md at acme/other skills/alpha",
    });
    for (const name of ["checkout-fork", "context-other", "other-copy"]) {
      expect(record(name)?.windows).toBeUndefined();
    }
  });

  test("--reprofile builds each profile again from the case alone: the new cases differ from the old ones only in profile and evidence, and no session is read and no reader called", async () => {
    // The case file as a builder that took the copies output's list wrote it: each profile holds
    // `newer`, which acme/skills did not hold at the commit, and not `older`.
    const stale = cases.map((c) => ({
      ...c,
      profile: {
        ...c.profile,
        skills: [
          ...c.profile.skills.filter((skill) => skill.name !== "older"),
          { repo: "acme/skills", commit: SHA, path: "skills/newer", name: "newer" },
        ],
      },
      evidence: (c.evidence ?? []).map((each) =>
        each.item === "skill older (acme/skills skills/older)"
          ? { item: "skill newer (acme/skills skills/newer)", source: "installers.tsv lists it" }
          : each
      ),
    }));
    const staleFile = path.join(scratch, "all", "stale.jsonl");
    writeFileSync(staleFile, stale.map((c) => `${JSON.stringify(c)}\n`).join(""));
    const fresh = path.join(scratch, "all", "reprofiled.jsonl");
    const args = (from: string, to: string) => [
      "build-cases",
      "--config",
      config,
      "--old",
      old,
      "--copies",
      copiesDir,
      "--count",
      countDir,
      "--reprofile",
      from,
      "--out",
      to,
    ];
    const lines: string[] = [];
    const requestsBefore = recorder.requests.length;
    const logBefore = statSync(psqlLog).size;
    expect(await main(args(staleFile, fresh), io(lines))).toBe(0);
    expect(lines.join("")).not.toContain("left out");
    expect(recorder.requests.length).toBe(requestsBefore);
    expect(statSync(psqlLog).size).toBe(logBefore);

    const rebuilt = loadCases(fresh, old);
    expect(rebuilt.map((c) => c.id)).toEqual(cases.map((c) => c.id));
    const rest = (c: Case) => ({ ...c, profile: undefined, evidence: undefined });
    for (const [index, c] of rebuilt.entries()) {
      const built = cases[index] as Case;
      // Its messages, line, steps and every other field are the old file's.
      expect(rest(c)).toEqual(rest(stale[index] as Case));
      // Its profile is the one the builder gives the session: no `newer`.
      expect(c.profile).toEqual(built.profile);
      expect(c.evidence?.map((each) => each.item)).toEqual(
        built.evidence?.map((each) => each.item)
      );
      // The working directory is not in a case, so only a workspace's AGENTS.md is said otherwise.
      const differing = (c.evidence ?? []).filter(
        (each, at) => each.source !== built.evidence?.[at]?.source
      );
      expect(differing.map((each) => each.item)).toEqual(
        c.profile.contextFiles.some((file) => file.path === "AGENTS.md")
          ? ["context file acme/widgets AGENTS.md"]
          : []
      );
    }
    const summary = JSON.parse(readFileSync(`${fresh}.summary.json`, "utf8")) as {
      casesRead: number;
      casesWritten: number;
      leftOut: unknown[];
      cases: { id: string; added: string[]; removed: string[] }[];
    };
    expect(summary).toMatchObject({
      casesRead: cases.length,
      casesWritten: cases.length,
      leftOut: [],
    });
    expect(summary.cases.every((each) => each.removed.join() === "newer")).toBe(true);
    expect(summary.cases.find((each) => each.id === byName("good")?.id)?.added).toEqual(["older"]);

    // It takes no session and no reader, and writes a new file.
    expect(await main([...args(staleFile, fresh), "--sessions", staleFile], io(lines))).toBe(2);
    expect(lines.join("\n")).toContain("so it takes no --sessions");
    expect(await main(args(staleFile, staleFile), io(lines))).toBe(2);
    expect(lines.join("\n")).toContain("is the file --reprofile reads");
  }, 60_000);

  test("a Legion session's role comes from its first user message, and its profile adds the image's plugin and the context files", () => {
    const c = byName("legion");
    expect(c?.context).toBe("legion:ACME:tester");
    expect(c?.profile.plugins).toEqual([
      { repo: "acme/skills", commit: SHA, path: "packages/pi-acme", name: "pi-acme" },
    ]);
    expect(c?.profile.contextFiles).toEqual([
      { repo: "acme/widgets", commit: SHA, path: "AGENTS.md" },
      { repo: "acme/infra", commit: SHA, path: "legion/instructions/acme.md" },
    ]);
    const evidence = Object.fromEntries(c?.evidence?.map((each) => [each.item, each.source]) ?? []);
    expect(evidence["role tester"]).toContain("first user message");
    expect(evidence["left out: the plugin package:@acme/codegraph"]).toContain("installs it");
  });

  test("a root architect's role comes from the daemon's start, a child's architect's from its catch-up notice, and a reviewer's from its claim beside its workspace; a session of no form is dropped with its first words", () => {
    const evidence = (name: string) =>
      Object.fromEntries(byName(name)?.evidence?.map((each) => [each.item, each.source]) ?? []);
    expect(byName("root-architect")?.context).toBe("legion:ACME:architect-root");
    expect(evidence("root-architect")["role architect-root"]).toContain('"ACME-12 is yours and …"');
    expect(byName("child-architect")?.context).toBe("legion:ACME:architect");
    expect(evidence("child-architect")["role architect"]).toContain(
      '"the issues under ACME-12, which you own …"'
    );
    // The catch-up notice is the first message the child's architect read.
    expect(byName("child-architect")?.messages[0]?.role).toBe("custom");
    expect(byName("claimed-reviewer")?.context).toBe("legion:ACME:reviewer");
    expect(evidence("claimed-reviewer")["role reviewer"]).toContain("legion-acme-acme-12-reviewer");
    const noRole = summaryOf(out).sessions.find(
      (each) => each.hash === fileHash(sessionPath("no-role"))
    );
    const words = (f1Messages()[0]?.content?.[0]?.text ?? "").replace(/\s+/g, " ").slice(0, 80);
    expect(words).toHaveLength(80);
    expect(noRole).toMatchObject({
      reason: "no-role",
      detail: `no form names its role; its first user message starts: ${JSON.stringify(words)}`,
    });
  });

  test("a subagent's case carries its recorded system prompt and tools, and no context file", () => {
    const c = byName("subagent");
    expect(c?.context).toBe("subagent:task");
    expect(c?.profile.systemPrompt).toEqual(SUBAGENT_PROMPT);
    expect(c?.profile.tools).toEqual(SUBAGENT_TOOLS);
    expect(c?.profile.contextFiles).toEqual([]);
    expect(c?.profile.skills.map((skill) => skill.name)).toEqual(["alpha", "beta", "older"]);
    const items = c?.evidence?.map((each) => each.item) ?? [];
    expect(items).toContain("subagent task");
    expect(items).toContain("the recorded system prompt and tools");
    expect(items).toContain("no context files");
    // The session_init line is no message: the case's messages are F1's first seven.
    const messages = f1Messages();
    expect(c?.messages.map((message) => message.role)).toEqual(
      messages.slice(0, 7).map((message) => message.role)
    );
    expect(c?.messages[2]).toEqual(messages[2] as SessionMessage);
  });

  test("a top-level devbox session's profile holds Oh My Pi's user-level context file, the .claude/CLAUDE.md of the one repository of its units that holds Claude Code's settings, at the session's start; a subagent's holds none beside its recorded prompt", () => {
    const c = byName("good");
    expect(c?.context).toBe("devbox");
    expect(c?.profile.contextFiles).toEqual([
      { repo: "acme/skills", commit: SHA, path: ".claude/CLAUDE.md" },
    ]);
    const evidence = Object.fromEntries(c?.evidence?.map((each) => [each.item, each.source]) ?? []);
    expect(evidence["left out: the working directory's context files"]).toBe(
      "a devbox session's working directory names no repository"
    );
    expect(evidence["context file acme/skills .claude/CLAUDE.md"]).toBe(
      `Oh My Pi's user-level context file is CLAUDE.md in Claude Code's config directory, on a dev box the .claude/ of the repository that installs its units: of the repositories of the profile's units (acme/skills), acme/skills alone holds Claude Code's settings .claude/settings.json at ${SHA}, and it has .claude/CLAUDE.md at ${SHA}`
    );
    // It is read at the commit of the session's start.
    expect(
      ghCalls.some(
        (args) =>
          args.includes("repos/acme/skills/contents/.claude/CLAUDE.md") &&
          args.includes(`ref=${SHA}`)
      )
    ).toBe(true);
    // A devbox session two of whose units' repositories hold Claude Code's settings has none, and
    // its evidence says why.
    const renamed = byName("renamed-plugin");
    expect(renamed?.profile.contextFiles).toEqual([]);
    expect(
      renamed?.evidence?.find(
        (each) => each.item === "left out: Oh My Pi's user-level context file"
      )?.source
    ).toBe(
      "2 repositories (acme/market, acme/skills) of the profile's units (acme/market, acme/skills) hold Claude Code's settings .claude/settings.json at the session's start, so which is the dev box's Claude Code config directory, whose CLAUDE.md Oh My Pi reads at user level, is not known"
    );
    // A subagent's recorded prompt holds the context files its session was given.
    const subagent = byName("subagent");
    expect(subagent?.profile.systemPrompt).toEqual(SUBAGENT_PROMPT);
    expect(subagent?.profile.contextFiles).toEqual([]);
    expect(subagent?.evidence?.map((each) => each.item)).not.toContain(
      "context file acme/skills .claude/CLAUDE.md"
    );
  });

  test("a read whose unit the copies output names by a later name ships nothing at the session's start, so the prefix rows its manifest gives at that commit place it at the unit that installed it then", () => {
    const c = byName("renamed-plugin");
    expect(c?.profile.skills.find((skill) => skill.name === "zeta")).toEqual({
      repo: "acme/market",
      commit: SHA,
      path: "skills/zeta",
      name: "zeta",
    });
    const evidence = Object.fromEntries(c?.evidence?.map((each) => [each.item, each.source]) ?? []);
    const later = "plugin:zeta-tools@new-market (acme/market:.claude-plugin/marketplace.json)";
    const then = "plugin:zeta-tools@old-market (acme/market:.claude-plugin/marketplace.json)";
    expect(evidence[`unit ${later}`]).toContain(
      `message 2 reads [Skill file: ${OLD_MARKET_ZETA}], which attribute.ts's checkout rule places at acme/market skills/zeta`
    );
    expect(evidence[`left out: the skills of ${later}`]).toBe(
      `acme/market at ${SHA}: by copies.ts's rules .claude-plugin/marketplace.json ships no skill there`
    );
    expect(evidence[`unit ${then}`]).toBe(
      `message 2 reads [Skill file: ${OLD_MARKET_ZETA}], under the prefix row **/plugins/cache/plugins/old-market___zeta-tools___*/skills/zeta/ that the manifests of acme/market at ${SHA} give by copies.ts's rules; no unit installers.tsv names for the read ships zeta at that commit`
    );
    expect(evidence["skill zeta (acme/market skills/zeta)"]).toBe(
      `acme/market at ${SHA}: plugin:zeta-tools@old-market ships it (acme/market:.claude-plugin/marketplace.json); installers.tsv, which the copies output read later, does not list it; message 2 reads this copy`
    );
  });

  test("a step in the third window is found, and the restatement check reads every message before it", () => {
    const c = byName("third-window");
    const messages = f1Messages();
    expect(c?.realStep).toEqual(messages[7] as SessionMessage);
    expect(c?.messages).toHaveLength(7 + FILLERS);
    const record = summaryOf(out).sessions.find(
      (each) => each.hash === fileHash(sessionPath("third-window"))
    );
    // Windows of 40 from the read's call (message 1): 1-40, 41-80, then 81 on.
    expect(record?.windows).toBe(3);
    expect(record?.restatementCheck).toEqual({
      read: 5 + FILLERS,
      before: 5 + FILLERS,
      windows: 1,
    });
    // One case reader call a window, each holding some of the session's added messages.
    expect(
      requests.filter(
        (request) =>
          request.body.includes("propose one case") && request.body.includes("Still working (")
      )
    ).toHaveLength(3);
  });

  test("a compaction before the skill's read leaves the summary, the entries it kept, and those after it", () => {
    const c = byName("compacted-before-read");
    const messages = f1Messages();
    expect(c?.messages.map((message) => message.role)).toEqual([
      "compactionSummary",
      "user",
      "assistant",
      "toolResult",
      "assistant",
      "toolResult",
      "assistant",
      "toolResult",
    ]);
    expect(c?.messages[0]?.summary).toBe(
      "The user asked for the alpha steps of the made-up project."
    );
    expect(c?.messages[1]).toEqual(messages[0] as SessionMessage);
    expect(c?.skillReads).toEqual([3, 5]);
  });

  test("a secret in a message is taken out of the case and counted", () => {
    const c = byName("secret");
    const text = readFileSync(out, "utf8");
    expect(text).not.toContain(SECRET_TOKEN);
    expect(text).not.toContain(PEM_BODY);
    expect(c?.secretsRemoved).toBe(2);
    expect(c?.messages[0]?.content?.[0]?.text).toContain(`Bearer ${SECRET_REMOVED}`);
  });

  test("the case reader is offered only the base text's lines the session's own reads hold, each with its number in the file, and no file the session did not read", () => {
    /** The `<skill>` part of the case reader's first request for session `name`. */
    const offered = (name: string) => {
      const request = requests.find(
        (each) => each.body.includes("propose one case") && each.body.includes(`(case ${name})`)
      );
      const body = JSON.parse(request?.body ?? "{}") as {
        messages?: { content?: { text?: string }[] }[];
      };
      const text = (body.messages?.[0]?.content ?? []).map((part) => part.text ?? "").join("\n");
      return /<skill name="alpha">[\s\S]*?<\/skill>/.exec(text)?.[0] ?? "";
    };
    const base = {
      "SKILL.md": readFileSync(path.join(old, "SKILL.md"), "utf8").split("\n"),
      "references/x.md": readFileSync(path.join(old, "references", "x.md"), "utf8").split("\n"),
    };
    /** Each `<number>: <line>` of a `<skill>` part's files, by file. */
    const shownLines = (skill: string) =>
      [...skill.matchAll(/<file path="([^"]+)">\n([\s\S]*?)\n<\/file>/g)].map((match) => ({
        file: match[1] ?? "",
        lines: (match[2] ?? "").split("\n").map((line) => {
          const shown = /^(\d+): (.*)$/.exec(line);
          if (shown === null) throw new Error(`a line shown with no number: ${line}`);
          return { number: Number(shown[1]), text: shown[2] };
        }),
      }));
    const lineNumber = base["SKILL.md"].indexOf(LINE) + 1;

    const good = offered("good");
    expect(good).toContain(`\n${lineNumber}: ${LINE}\n`);
    expect(shownLines(good).map((each) => each.file)).toEqual(["SKILL.md", "references/x.md"]);
    // Every line shown is the base file's line at its number.
    for (const name of ["good", "read-lacks-line", "skill-md-only"]) {
      for (const { file, lines } of shownLines(offered(name))) {
        for (const { number, text } of lines) {
          expect(base[file as keyof typeof base][number - 1]).toBe(text);
        }
      }
    }
    // The read lacks the line, so it is not offered; the reader's proposal of it still drops.
    const lacking = offered("read-lacks-line");
    expect(lacking).not.toBe("");
    expect(lacking).not.toContain(LINE);
    expect(shownLines(offered("skill-md-only")).map((each) => each.file)).toEqual(["SKILL.md"]);
  });

  test("a session the profile check drops makes no reader request: the profile at the session's start is checked before the first reader call", () => {
    const record = summaryOf(out).sessions.find(
      (each) => each.hash === fileHash(sessionPath("unshipped"))
    );
    expect(record).toMatchObject({
      reason: "not-cut-copy",
      detail: "the profile installs no alpha of acme/skills at the session's start",
      readerUsd: 0,
    });
    expect(record?.windows).toBeUndefined();
    expect(requests.some((each) => each.body.includes("(case unshipped)"))).toBe(false);
  });

  test("the copy of a skill the session read is the profile's copy of its name, ahead of another unit's copy that comes first", () => {
    const c = byName("read-wins");
    expect(c?.profile.skills).toEqual([
      { repo: "acme/skills", commit: SHA, path: "skills/alpha", name: "alpha" },
      { repo: "acme/skills", commit: SHA, path: "skills/beta", name: "beta" },
      { repo: "acme/other", commit: SHA, path: "skills/delta", name: "delta" },
      { repo: "acme/skills", commit: SHA, path: "skills/older", name: "older" },
    ]);
    const evidence = Object.fromEntries(c?.evidence?.map((each) => [each.item, each.source]) ?? []);
    // acme/other's unit comes first, from the read of delta at message 2.
    const units = (c?.evidence ?? []).filter((each) => each.item.startsWith("unit "));
    expect(units.map((each) => each.item)).toEqual([
      "unit user-skills:omp (acme/other:installers/skills.sh)",
      "unit user-skills:omp (acme/skills:installers/skills.sh)",
    ]);
    expect(evidence["left out: skill alpha from acme/other skills/alpha"]).toBe(
      "user-skills:omp links it (acme/other:installers/skills.sh) too; the session read the copy of acme/skills skills/alpha (message 4)"
    );
    expect(evidence["skill alpha (acme/skills skills/alpha)"]).toContain(
      "; message 4 reads this copy"
    );
  });

  test("a session whose reads hold no line of the base text is dropped as no-common-line, with no reader call", () => {
    const record = summaryOf(out).sessions.find(
      (each) => each.hash === fileHash(sessionPath("no-common"))
    );
    expect(record).toMatchObject({
      reason: "no-common-line",
      detail:
        "the session's successful reads of SKILL.md, references/x.md hold no whole line of the base text",
      readerUsd: 0,
    });
    expect(record?.windows).toBeUndefined();
    expect(requests.some((each) => each.body.includes("(case no-common)"))).toBe(false);
  });

  test("a session whose model id the gateway serves is kept on its own model, whatever its prefix, and one on a model no route serves is dropped, before any reader when none of its models is served", () => {
    expect(byName("openai")?.model).toBe("openai/gpt-6.1-sol");
    expect(byName("gateway-model")?.model).toBe("ossv-gateway/claude-opus-5-5");
    const summary = summaryOf(out);
    const record = (name: string) =>
      summary.sessions.find((each) => each.hash === fileHash(sessionPath(name)));
    expect(record("not-served")).toMatchObject({
      reason: "model-not-served",
      model: "openai-codex/gpt-6-astra",
      detail:
        "openai-codex/gpt-6-astra (id gpt-6-astra): no route of the gateway serves it; no servedModels entry has its id",
    });
    expect(record("not-served")?.windows).toBeUndefined();
    expect(record("not-served")?.readerUsd).toBe(0);
    // Served at the read, so the reader reads it; the step's model is not served.
    expect(record("switched-model")).toMatchObject({
      reason: "model-not-served",
      model: "openai-codex/gpt-6-astra",
      windows: 1,
    });
    expect(summary.byModel["openai-codex/gpt-6-astra"]).toEqual({
      tried: 2,
      drops: { "model-not-served": 2 },
    });
    expect(summary.byModel["openai/gpt-6.1-sol"]).toEqual({ tried: 1, drops: {} });
    expect(summary.byModel["ossv-gateway/claude-opus-5-5"]).toEqual({ tried: 1, drops: {} });
    // Each session tried and each drop is under one model.
    const rows = Object.values(summary.byModel);
    expect(rows.reduce((total, row) => total + row.tried, 0)).toBe(summary.sessionsTried);
    for (const [reason, count] of Object.entries(summary.drops)) {
      const counted = rows.reduce(
        (total, row) => total + (row.drops[reason as DropReason] ?? 0),
        0
      );
      expect(counted).toBe(count);
    }
    expect(summary.byModel.none?.tried).toBeGreaterThan(0);
  });

  test("each drop is counted by its reason", () => {
    const summary = summaryOf(out);
    expect(summary.drops).toEqual({
      "line-not-in-base": 1,
      "line-not-in-read": 1,
      "read-before-compaction": 1,
      "line-quoted": 1,
      "history-restates-line": 2,
      "restatement-check-incomplete": 2,
      "audit-breaks": 1,
      "audit-not-governed": 1,
      "model-not-served": 2,
      "read-by-path": 1,
      "no-read": 1,
      "no-step": 1,
      "no-common-line": 1,
      "not-cut-copy": 4,
      "no-role": 1,
      "bad-session-init": 1,
      unparseable: 1,
      "not-in-store": 1,
    });
    expect(Object.keys(summary.drops).every((reason) => reason in DROP_REASONS)).toBe(true);
    expect(summary.sessionsTried).toBe(Object.keys(SESSIONS).length + 1);
    expect(summary.casesBuilt).toBe(16);
    const reasonOf = (name: string) =>
      summary.sessions.find((each) => each.hash === fileHash(sessionPath(name)))?.reason;
    expect(reasonOf("quoted")).toBe("line-quoted");
    expect(reasonOf("restated")).toBe("history-restates-line");
    expect(reasonOf("late-restated")).toBe("history-restates-line");
    expect(reasonOf("too-long")).toBe("restatement-check-incomplete");
    expect(reasonOf("restate-fails")).toBe("restatement-check-incomplete");
    expect(reasonOf("by-path")).toBe("read-by-path");
    expect(reasonOf("other-copy")).toBe("not-cut-copy");
    expect(reasonOf("compacted")).toBe("read-before-compaction");
    expect(reasonOf("bad-init")).toBe("bad-session-init");
    expect(reasonOf("not-served")).toBe("model-not-served");
    expect(reasonOf("switched-model")).toBe("model-not-served");
    expect(reasonOf("read-lacks-line")).toBe("line-not-in-read");
    expect(reasonOf("no-common")).toBe("no-common-line");
    expect(reasonOf("unshipped")).toBe("not-cut-copy");
    // Every reader call is counted: one case reader call a window of each session the reader
    // reached, one restatement call a window of each case checked (READER_TRIES for the window
    // whose answer was refused, none for the case with too many windows, up to the window of a
    // restatement), and one audit of each case checked whole.
    expect(summary.readerSpend.models["claude-opus-5-5"]?.calls).toBe(requests.length);
    const count = (prompt: string) =>
      requests.filter((request) => request.body.includes(prompt)).length;
    const sum = (values: number[]) => values.reduce((total, each) => total + each, 0);
    expect(count("propose one case")).toBe(sum(summary.sessions.map((each) => each.windows ?? 0)));
    const restateCalls = summary.sessions.map((each) => {
      if (each.restatementCheck === undefined) return 0;
      if (each.hash === fileHash(sessionPath("restate-fails"))) return READER_TRIES;
      if (each.hash === fileHash(sessionPath("too-long"))) return 0;
      if (each.hash === fileHash(sessionPath("late-restated"))) return 1;
      return each.restatementCheck.windows;
    });
    expect(count(RESTATE_PROMPT)).toBe(sum(restateCalls));
    expect(count(AUDIT_PROMPT)).toBe(summary.sessions.filter((each) => each.audit).length);
    expect(summary.readerSpend.usd).toBeGreaterThan(0);
  });

  test("a pull pages at 64 rows a call, inside the file row's slices too, and a hash without a path is resolved with sessions.sql", () => {
    const log = readFileSync(psqlLog, "utf8")
      .split("\n")
      .filter((line) => line !== "")
      .map(
        (line) =>
          JSON.parse(line) as {
            kind: string;
            rows: number;
            after?: number[];
            first?: number[];
            limit64: boolean;
          }
      );
    expect(log.every((each) => each.limit64)).toBe(true);
    expect(log.filter((each) => each.kind === "sessions")).toHaveLength(1);
    expect(log.some((each) => each.kind === "pull" && each.rows === 64)).toBe(true);
    // A call that starts after a slice of the file row and returns the next one: a page that
    // ended inside the file row.
    expect(
      log.some((each) => each.kind === "pull" && each.after?.[0] === -1 && each.first?.[0] === -1)
    ).toBe(true);
    const good = summaryOf(out).sessions.find(
      (each) => each.hash === fileHash(sessionPath("good"))
    );
    expect(good?.pulls).toBeGreaterThan(1);
  });
});

test("the seed fixes the order the sessions are taken in, and --max stops at that many cases", async () => {
  const names = ["good", "secret", "legion", "no-step"];
  const sessions = sessionsFile("seeded.jsonl", names);
  const runs = [];
  for (const [run, seed] of [
    ["a", "11"],
    ["b", "11"],
    ["c", "12"],
  ] as const) {
    const out = path.join(scratch, `seeded-${run}`, "cases.jsonl");
    expect(await main(buildArgs(sessions, out, seed, "--max", "1"), io([]))).toBe(0);
    runs.push({ summary: summaryOf(out), cases: loadCases(out, old) });
  }
  const candidates = names.map((name) => ({
    store: SESSIONS[name]?.store ?? "",
    hash: fileHash(sessionPath(name)),
  }));
  const expected = (seed: string) => blindOrder(candidates, seed).map(({ item }) => item);
  expect(runs[0]?.summary.order).toEqual(expected("11"));
  expect(runs[1]?.summary.order).toEqual(runs[0]?.summary.order ?? []);
  expect(runs[2]?.summary.order).toEqual(expected("12"));
  // The two seeds give two orders, so the runs above can tell whether the seed is read.
  expect(expected("12")).not.toEqual(expected("11"));
  for (const run of runs) {
    expect(run.cases).toHaveLength(1);
    const first = run.summary.sessions.find((each) => each.outcome.startsWith("case "));
    expect(run.cases[0]?.source.pathHash).toBe(
      pathHash(sessionPath(names.find((name) => fileHash(sessionPath(name)) === first?.hash) ?? ""))
    );
  }
}, 60_000);

test("build-cases refuses an unknown cut and a sessions line whose path is not its hash, before any read", async () => {
  const from = recorder.requests.length;
  const logLength = () => (existsSync(psqlLog) ? readFileSync(psqlLog, "utf8").length : 0);
  const logged = logLength();
  const lines: string[] = [];
  const out = path.join(scratch, "refused", "cases.jsonl");
  const sessions = sessionsFile("good.jsonl", ["good"]);
  const unknownCut = buildArgs(sessions, out, "1").map((arg) =>
    arg === "skills-1" ? "skills-9" : arg
  );
  expect(await main(unknownCut, io(lines))).toBe(2);
  expect(lines.join("\n")).toContain("--cut skills-9 is no cut of the configuration");
  const noCount = buildArgs(sessions, out, "1").filter(
    (arg, at, args) => arg !== "--count" && args[at - 1] !== "--count"
  );
  expect(await main(noCount, io(lines))).toBe(2);
  expect(lines.join("\n")).toContain("--count required");
  const emptyCount = path.join(scratch, "refused", "count");
  mkdirSync(emptyCount, { recursive: true });
  const noOutputs = buildArgs(sessions, out, "1").map((arg, at, args) =>
    args[at - 1] === "--count" ? emptyCount : arg
  );
  expect(await main(noOutputs, io(lines))).toBe(2);
  expect(lines.join("\n")).toContain("holds no query output of a store of the configuration");
  const wrongHash = sessionsFile(
    "wrong.jsonl",
    [],
    [],
    [{ store: "store_a", hash: "0000000000000000", path: sessionPath("good") }]
  );
  expect(await main(buildArgs(wrongHash, out, "1"), io(lines))).toBe(2);
  expect(lines.join("\n")).toContain(
    `its path's hash is ${fileHash(sessionPath("good"))}, not 0000000000000000`
  );
  expect(recorder.requests.length).toBe(from);
  expect(logLength()).toBe(logged);
});

test("build-cases refuses an --out inside a git or jj checkout, its own and --reprofile's, before any read", async () => {
  const from = recorder.requests.length;
  const logLength = () => (existsSync(psqlLog) ? readFileSync(psqlLog, "utf8").length : 0);
  const logged = logLength();
  const sessions = sessionsFile("checkout.jsonl", ["good"]);
  for (const marker of [".git", ".jj"]) {
    const checkout = path.join(scratch, `checkout${marker}`);
    mkdirSync(path.join(checkout, marker), { recursive: true });
    const out = path.join(checkout, "cases", "cases.jsonl");
    const reprofile = [
      "build-cases",
      "--config",
      config,
      "--old",
      old,
      "--copies",
      copiesDir,
      "--count",
      countDir,
      "--reprofile",
      sessions,
      "--out",
      out,
    ];
    for (const args of [buildArgs(sessions, out, "1"), reprofile]) {
      const lines: string[] = [];
      expect(await main(args, io(lines))).toBe(EXIT.refused);
      expect(lines.join("\n")).toContain(`is inside the checkout ${checkout}`);
      expect(existsSync(out)).toBe(false);
      expect(existsSync(`${out}.spool`)).toBe(false);
    }
  }
  expect(recorder.requests.length).toBe(from);
  expect(logLength()).toBe(logged);
});

test("build-tasks proposes terms, reads first user messages per store and day, and writes labelled tasks", async () => {
  const skillDir = path.join(setupDir, "old", "alpha");
  const out = path.join(scratch, "tasks", "tasks.jsonl");
  const lines: string[] = [];
  const code = await main(
    [
      "build-tasks",
      "--config",
      config,
      "--key-file",
      keyFile,
      "--skill",
      skillDir,
      "--store",
      "store_a",
      "--out",
      out,
    ],
    io(lines)
  );
  expect(lines.join("")).toContain("build-tasks: 10 tasks (2 from sessions)");
  expect(code).toBe(0);
  const tasks = readFileSync(out, "utf8")
    .split("\n")
    .filter((line) => line !== "")
    .map((line) => JSON.parse(line));
  expect(tasks[0]).toEqual({
    id: "t1",
    text: "Give me the alpha checklist for the fixture project.",
    label: "need",
    source: "session",
    store: "store_a",
    pathHash: pathHash("/sessions/task-1.jsonl"),
  });
  expect(tasks[1]).toEqual({
    id: "t2",
    text: "Write the Alpha report of step 2.",
    label: "near-miss",
    source: "session",
    store: "store_a",
    pathHash: pathHash("/sessions/task-3.jsonl"),
  });
  expect(tasks[2]).toEqual({
    id: "t3",
    text: "A made-up need task 1.",
    label: "need",
    source: "written",
  });
  expect(tasks.filter((task) => task.label === "need")).toHaveLength(5);
  expect(tasks.filter((task) => task.label === "near-miss")).toHaveLength(5);
  const summary = JSON.parse(readFileSync(`${out}.summary.json`, "utf8"));
  expect(summary.terms).toEqual(["alpha"]);
  expect(summary.queries).toEqual([
    { store: "store_a", day: "2026-11-20", rows: 1 },
    { store: "store_a", day: "2026-11-21", rows: 2 },
  ]);
  // The same first message on two sessions is offered once.
  expect(summary.messagesFound).toBe(3);
  expect(summary.messagesOffered).toBe(2);
  // The need task whose first step the skill does not govern is left out, with its reason.
  expect(summary.droppedNeed).toBe(1);
  expect(summary.dropped).toEqual([
    {
      source: "written",
      text: "A made-up need task 5.",
      why: "made up",
      reason: "the notes are read first",
    },
  ]);
  expect(summary.tasks[0].firstStep).toEqual(GOVERNS.firstStep);
}, 30_000);

test("build-tasks --model offers only the first user messages of sessions whose first request ran on that model, drops and counts each other session as other-model, and the summary names the model", async () => {
  const out = path.join(scratch, "tasks-model", "tasks.jsonl");
  const args = (model: string) => [
    "build-tasks",
    "--config",
    config,
    "--key-file",
    keyFile,
    "--skill",
    old,
    "--store",
    "store_a",
    "--out",
    out,
    "--model",
    model,
  ];
  const lines: string[] = [];
  expect(await main(args("openai/gpt-6.1-sol"), io(lines))).toBe(EXIT.ok);
  // task-3's session ran on openai/gpt-6.1-sol and is offered; task-1's and task-4's, of the other
  // text, ran on F1's model, and each is dropped.
  expect(tasksOf(out).filter((task) => task.source === "session")).toEqual([
    {
      id: "t1",
      text: "Write the Alpha report of step 2.",
      label: "need",
      source: "session",
      store: "store_a",
      pathHash: pathHash("/sessions/task-3.jsonl"),
    },
  ]);
  const summary = JSON.parse(readFileSync(`${out}.summary.json`, "utf8"));
  expect(summary.model).toBe("openai/gpt-6.1-sol");
  expect(summary.inputs.model).toBe("openai/gpt-6.1-sol");
  expect(summary.messagesOffered).toBe(1);
  expect(summary.candidateDrops.counts).toEqual({ "other-model": 2 });
  expect(summary.candidateDrops.sessions).toEqual(
    ["/sessions/task-1.jsonl", "/sessions/task-4.jsonl"].map((file) => ({
      store: "store_a",
      pathHash: pathHash(file),
      reason: "other-model",
      detail: "its first reply ran on anthropic/claude-sonnet-5-5, not openai/gpt-6.1-sol",
    }))
  );
  expect(lines.join("")).toContain(
    "2 candidate sessions dropped (2 other-model, not on openai/gpt-6.1-sol)"
  );
  expect(existsSync(`${out}.spool`)).toBe(false);
  // The model is one of the inputs: a run that goes on with another is refused.
  const other: string[] = [];
  expect(await main(args("anthropic/claude-sonnet-5-5"), io(other))).toBe(EXIT.refused);
  expect(other.join("\n")).toContain(
    "--model was openai/gpt-6.1-sol, now anthropic/claude-sonnet-5-5"
  );
}, 60_000);

/** The lines of the stand-in store's log since `from` characters, parsed. */
const psqlLines = (from: number) =>
  readFileSync(psqlLog, "utf8")
    .slice(from)
    .split("\n")
    .filter((line) => line !== "")
    .map((line) => JSON.parse(line) as { kind: string; rows: number; after?: number[] });

/** The characters of the stand-in store's log: none before its first call. */
const psqlLogLength = () => (existsSync(psqlLog) ? readFileSync(psqlLog, "utf8").length : 0);

/** The transcripts the stand-in store started to pull since `from` characters of its log: one
 *  pull call from the place before every row (PULL_START) for each. */
const transcriptPulls = (from: number) =>
  psqlLines(from).filter((line) => line.kind === "pull" && line.after?.[0] === -2).length;

/** A clock for `BuildIo.now` that moves one minute with each transcript the stand-in store starts
 *  to pull, so a search's own time is the same in every run. */
const pullClock = () => transcriptPulls(0) * 60_000;

/** `summary` with each dollar figure of its spend rounded, since a run that goes on adds its
 *  spend to the earlier run's, which can differ from one running sum in the last bit. */
function roundedSpend(summary: Record<string, unknown>): Record<string, unknown> {
  const spend = summary.readerSpend as { usd: number; models: Record<string, { usd: number }> };
  const round = (usd: number) => Number(usd.toFixed(9));
  return {
    ...summary,
    readerSpend: {
      usd: round(spend.usd),
      models: Object.fromEntries(
        Object.entries(spend.models).map(([model, each]) => [
          model,
          { ...each, usd: round(each.usd) },
        ])
      ),
    },
  };
}

/** An io whose `out` throws once `after` sessions are decided, as a kill after them would end the
 *  run: the next session's work is done, and its outcome is not written. */
function killedAfter(after: number, lines: string[]): BuildIo {
  let decided = 0;
  return {
    ...io(lines),
    out: (text) => {
      lines.push(text);
      if (/^build-cases: \d+\/\d+ /.test(text) && ++decided > after) throw new Error("killed");
    },
  };
}

describe("build-cases goes on from where a run stopped", () => {
  const names = [
    "good",
    "quoted",
    "secret",
    "legion",
    "no-step",
    "read-wins",
    "unshipped",
    "openai",
  ];
  const once = path.join(scratch, "resume", "once", "cases.jsonl");
  const killed = path.join(scratch, "resume", "killed", "cases.jsonl");
  let sessions = "";
  const lines: string[] = [];
  let resumedRequests: RecordedRequest[] = [];
  let resumedPulls: { kind: string; rows: number }[] = [];
  const codes: number[] = [];
  beforeAll(async () => {
    sessions = sessionsFile("resume.jsonl", names);
    const args = (out: string) => buildArgs(sessions, out, "5", "--concurrency", "1");
    codes.push(await main(args(once), io([])));
    codes.push(await main(args(killed), killedAfter(3, lines)));
    const from = recorder.requests.length;
    const logged = readFileSync(psqlLog, "utf8").length;
    codes.push(await main(args(killed), io(lines)));
    resumedRequests = recorder.requests.slice(from);
    resumedPulls = psqlLines(logged);
  }, 120_000);

  test("a build killed after 3 sessions and run again gives the cases and summary of one uninterrupted run, with no pull and no reader call for those 3", () => {
    expect(codes).toEqual([0, 1, 0]);
    expect(lines.join("")).toContain("going on from");
    expect(readFileSync(killed, "utf8")).toBe(readFileSync(once, "utf8"));
    const summary = (out: string) =>
      roundedSpend(
        JSON.parse(readFileSync(`${out}.summary.json`, "utf8")) as Record<string, unknown>
      );
    expect(summary(killed)).toEqual(summary(once));
    // The first 3 sessions of the seeded order were decided before the kill.
    const order = summaryOf(once).order.map(
      (each) => names.find((name) => fileHash(sessionPath(name)) === each.hash) ?? ""
    );
    for (const name of order.slice(0, 3)) {
      expect(resumedRequests.some((request) => request.body.includes(`(case ${name})`))).toBe(
        false
      );
    }
    expect(resumedRequests.some((request) => request.body.includes(`(case ${order[3]})`))).toBe(
      true
    );
    const later = summaryOf(once).sessions.slice(3);
    expect(resumedPulls.filter((each) => each.kind === "pull")).toHaveLength(
      later.reduce((total, each) => total + (each.pulls ?? 0), 0)
    );
  });

  test("a run that goes on from other inputs is refused, naming each difference, and leaves the files as they were", async () => {
    const before = readFileSync(`${killed}.summary.json`, "utf8");
    const seedRefused: string[] = [];
    expect(await main(buildArgs(sessions, killed, "6"), io(seedRefused))).toBe(2);
    expect(seedRefused.join("\n")).toContain("--seed was 5, now 6");
    const other = sessionsFile("resume-other.jsonl", names.slice(1));
    const sessionsRefused: string[] = [];
    expect(await main(buildArgs(other, killed, "5"), io(sessionsRefused))).toBe(2);
    expect(sessionsRefused.join("\n")).toContain(`--sessions was ${sessions}`);
    expect(sessionsRefused.join("\n")).toContain("holds a build of other inputs");
    expect(readFileSync(`${killed}.summary.json`, "utf8")).toBe(before);
  });
});

test("--concurrency 3 keeps the same cases, in the same order, as one session at a time", async () => {
  const names = [
    "good",
    "quoted",
    "secret",
    "legion",
    "no-step",
    "read-wins",
    "openai",
    "subagent",
  ];
  const sessions = sessionsFile("concurrency.jsonl", names);
  const outs = [1, 3].map((n) => path.join(scratch, `concurrency-${n}`, "cases.jsonl"));
  for (const [index, n] of [1, 3].entries()) {
    const out = outs[index] ?? "";
    expect(
      await main(buildArgs(sessions, out, "9", "--max", "3", "--concurrency", `${n}`), io([]))
    ).toBe(0);
  }
  const [one, three] = outs.map((out) => summaryOf(out));
  expect(readFileSync(outs[1] ?? "", "utf8")).toBe(readFileSync(outs[0] ?? "", "utf8"));
  expect(three?.order).toEqual(one?.order ?? []);
  expect(three?.casesBuilt).toBe(3);
  // The sessions decided are the same; a session still open when the third case was decided is
  // recorded as not needed, after them.
  const decided = (summary: Summary | undefined) =>
    (summary?.sessions ?? []).filter((each) => each.outcome !== "not-needed");
  expect(decided(three)).toEqual(decided(one));
  const notNeeded = (three?.sessions ?? []).slice(decided(three).length);
  expect(notNeeded.every((each) => each.outcome === "not-needed")).toBe(true);
  expect(notNeeded.length).toBeLessThanOrEqual(2);
}, 120_000);

test("build-tasks killed after its first query and run again gives the tasks and summary of one uninterrupted run, with no terms call and only the query left", async () => {
  const skillDir = path.join(setupDir, "old", "alpha");
  const args = (out: string) => [
    "build-tasks",
    "--config",
    config,
    "--key-file",
    keyFile,
    "--skill",
    skillDir,
    "--store",
    "store_a",
    "--out",
    out,
  ];
  const once = path.join(scratch, "tasks-once", "tasks.jsonl");
  const killed = path.join(scratch, "tasks-killed", "tasks.jsonl");
  expect(await main(args(once), io([]))).toBe(0);
  const lines: string[] = [];
  const kill: BuildIo = {
    ...io(lines),
    out: (text) => {
      lines.push(text);
      if (/^build-tasks: store_a \d{4}-\d{2}-\d{2}: /.test(text)) throw new Error("killed");
    },
  };
  expect(await main(args(killed), kill)).toBe(1);
  const from = recorder.requests.length;
  const logged = readFileSync(psqlLog, "utf8").length;
  expect(await main(args(killed), io(lines))).toBe(0);
  // Only the tasks reader's pick of tasks: its terms were recorded.
  expect(recorder.requests.slice(from)).toHaveLength(1);
  expect(psqlLines(logged)).toHaveLength(1);
  expect(readFileSync(killed, "utf8")).toBe(readFileSync(once, "utf8"));
  const summary = (out: string) =>
    roundedSpend(
      JSON.parse(readFileSync(`${out}.summary.json`, "utf8")) as Record<string, unknown>
    );
  expect(summary(killed)).toEqual(summary(once));
  expect(existsSync(`${killed}.found.jsonl`)).toBe(false);
  // A finished build is left as it is.
  const again: string[] = [];
  expect(await main(args(killed), io(again))).toBe(0);
  expect(again.join("")).toContain("nothing to do");
}, 60_000);

/** A sessions file of the control sessions `names`, each with its path but `withoutPath`'s. */
function controlSessionsFile(file: string, names: readonly string[], withoutPath: string[] = []) {
  writeFileSync(
    path.join(setupDir, file),
    names
      .map((name) => {
        const line = { store: "store_a", hash: fileHash(controlPath(name)) };
        return withoutPath.includes(name) ? line : { ...line, path: controlPath(name) };
      })
      .map((line) => `${JSON.stringify(line)}\n`)
      .join("")
  );
  return path.join(setupDir, file);
}

/** The arguments of one build-tasks --first-read run. */
const controlArgs = (sessions: string, out: string, seed: string, ...extra: string[]) => [
  "build-tasks",
  "--first-read",
  "--config",
  config,
  "--key-file",
  keyFile,
  "--skill",
  old,
  "--sessions",
  sessions,
  "--seed",
  seed,
  ...extra,
  "--out",
  out,
];

/** The lines of a tasks file, parsed. */
const tasksOf = (file: string): Record<string, unknown>[] =>
  readFileSync(file, "utf8")
    .split("\n")
    .filter((line) => line !== "")
    .map((line) => JSON.parse(line));

/** The control session whose transcript path's hash is `hash`. */
const controlNamed = (hash: unknown) =>
  Object.keys(CONTROL_SESSIONS).find((name) => fileHash(controlPath(name)) === hash);

test("build-tasks --first-read takes the first requests of sessions whose first reply read the skill, once each, and leaves out one whose first step the skill does not govern", async () => {
  const names = Object.keys(CONTROL_SESSIONS);
  const sessions = controlSessionsFile("control.jsonl", names, ["c-two"]);
  const out = path.join(scratch, "control", "tasks.jsonl");
  const lines: string[] = [];
  const from = recorder.requests.length;
  expect(await main(controlArgs(sessions, out, "1", "--max", "10"), io(lines))).toBe(0);
  // One reader call judged every request found.
  expect(recorder.requests.slice(from)).toHaveLength(1);
  const order = blindOrder(names, "1").map(({ item }) => item);
  const firstOfSame = order.find((name) => name === "c-one" || name === "c-same");
  const kept = order.filter((name) => [firstOfSame, "c-secret", "c-two"].includes(name));
  const tasks = tasksOf(out);
  expect(tasks.map((task) => task.pathHash)).toEqual(
    kept.map((name) => pathHash(controlPath(name)))
  );
  expect(tasks.map((task) => task.id)).toEqual(["t1", "t2", "t3"]);
  expect(
    tasks.every(
      (task) => task.label === "need" && task.source === "session" && task.store === "store_a"
    )
  ).toBe(true);
  const secret = tasks.find((task) => task.pathHash === pathHash(controlPath("c-secret")));
  expect(secret?.text).toBe(
    `Run the alpha checklist on the API.\n> Authorization: Bearer ${SECRET_REMOVED}`
  );
  const summary = JSON.parse(readFileSync(`${out}.summary.json`, "utf8"));
  expect(summary.done).toBe(true);
  const outcomes = Object.fromEntries(
    (summary.sessions as { hash: string; outcome: string }[]).map((each) => [
      controlNamed(each.hash),
      each.outcome,
    ])
  );
  expect(outcomes).toEqual({
    "c-one": firstOfSame === "c-one" ? "first request" : "same request",
    "c-same": firstOfSame === "c-same" ? "first request" : "same request",
    "c-secret": "first request",
    "c-late": "not first",
    "c-ungoverned": "first request",
    "c-two": "first request",
  });
  expect(summary.droppedNeed).toBe(1);
  expect(summary.dropped).toEqual([
    {
      store: "store_a",
      pathHash: pathHash(controlPath("c-ungoverned")),
      reason: "a file is opened first",
    },
  ]);
  expect(summary.tasks.map((task: { secretsRemoved: number }) => task.secretsRemoved)).toEqual(
    kept.map((name) => (name === "c-secret" ? 1 : 0))
  );
  // c-two's path came from sessions.sql.
  expect(summary.sessionQueries).toBe(1);
  expect(lines.join("\n")).toContain("the sessions gave 3 control tasks, fewer than --max 10");
  expect(existsSync(`${out}.found.jsonl`)).toBe(false);
}, 60_000);

test("build-tasks --first-read stops once --max requests are kept, and killed after its first session and run again gives the tasks and summary of one uninterrupted run", async () => {
  const names = Object.keys(CONTROL_SESSIONS);
  const sessions = controlSessionsFile("control-max.jsonl", names);
  const once = path.join(scratch, "control-once", "tasks.jsonl");
  const killed = path.join(scratch, "control-killed", "tasks.jsonl");
  const fromOnce = recorder.requests.length;
  expect(await main(controlArgs(sessions, once, "2", "--max", "2"), io([]))).toBe(0);
  const onceCalls = recorder.requests.length - fromOnce;
  // The sessions tried: the order up to the second request the skill governs the first step of.
  const seen = new Set<string>();
  let governed = 0;
  const tried: string[] = [];
  for (const name of blindOrder(names, "2").map(({ item }) => item)) {
    if (governed === 2) break;
    tried.push(name);
    const session = CONTROL_SESSIONS[name];
    if (session === undefined || !session.firstRead || seen.has(session.request)) continue;
    seen.add(session.request);
    if (!session.request.includes(UNGOVERNED)) governed += 1;
  }
  const summary = (out: string) =>
    roundedSpend(
      JSON.parse(readFileSync(`${out}.summary.json`, "utf8")) as Record<string, unknown>
    );
  const onceSummary = summary(once) as { sessions: { hash: string }[] };
  expect(onceSummary.sessions.length).toBeLessThan(names.length);
  expect(onceSummary.sessions.map((each) => controlNamed(each.hash))).toEqual(tried);
  expect(tasksOf(once)).toHaveLength(2);

  const lines: string[] = [];
  const kill: BuildIo = {
    ...io(lines),
    out: (text) => {
      lines.push(text);
      if (/^build-tasks: 1\/\d+ /.test(text)) throw new Error("killed");
    },
  };
  expect(await main(controlArgs(sessions, killed, "2", "--max", "2"), kill)).toBe(1);
  const from = recorder.requests.length;
  const logged = readFileSync(psqlLog, "utf8").length;
  expect(await main(controlArgs(sessions, killed, "2", "--max", "2"), io(lines))).toBe(0);
  // The first session is not pulled again.
  const pulls = (out: string) =>
    (summary(out) as { sessions: { pulls?: number }[] }).sessions
      .slice(1)
      .reduce((total, each) => total + (each.pulls ?? 0), 0);
  expect(psqlLines(logged).filter((line) => line.kind === "pull")).toHaveLength(pulls(killed));
  // The killed run made no reader call, so the run that went on makes them all.
  expect(recorder.requests.length - from).toBe(onceCalls);
  expect(readFileSync(killed, "utf8")).toBe(readFileSync(once, "utf8"));
  expect(summary(killed)).toEqual(summary(once));
  expect(existsSync(`${killed}.found.jsonl`)).toBe(false);
  const again: string[] = [];
  expect(await main(controlArgs(sessions, killed, "2", "--max", "2"), io(again))).toBe(0);
  expect(again.join("")).toContain("nothing to do");
  // Another seed and --max at the same --out are refused, naming each.
  const other: string[] = [];
  expect(await main(controlArgs(sessions, killed, "3", "--max", "3"), io(other))).toBe(2);
  expect(other.join("\n")).toContain("--seed was 2, now 3; --max was 2, now 3");
}, 60_000);

test("build-tasks --first-read --model drops and counts each session whose first request ran on another model as other-model, keeps those on it, and reports how many on it read the skill in their first reply", async () => {
  const names = [...Object.keys(CONTROL_SESSIONS), "c-other-model"];
  const sessions = controlSessionsFile("control-model.jsonl", names);
  const named = (hash: unknown) => names.find((name) => fileHash(controlPath(name)) === hash);
  const run = async (model: string, dir: string) => {
    const out = path.join(scratch, dir, "tasks.jsonl");
    const lines: string[] = [];
    const args = controlArgs(sessions, out, "1", "--max", "10", "--model", model);
    expect(await main(args, io(lines))).toBe(EXIT.ok);
    const summary = JSON.parse(readFileSync(`${out}.summary.json`, "utf8"));
    const outcomes = Object.fromEntries(
      (summary.sessions as { hash: string; outcome: string; detail?: string }[]).map((each) => [
        named(each.hash),
        each.outcome === "other-model" ? each.detail : each.outcome,
      ])
    );
    return { tasks: tasksOf(out), summary, outcomes, printed: lines.join("\n") };
  };

  const openai = await run("openai/gpt-6.1-sol", "control-model-openai");
  expect(openai.tasks.map((task) => task.pathHash)).toEqual([
    pathHash(controlPath("c-other-model")),
  ]);
  expect(openai.outcomes).toEqual({
    ...Object.fromEntries(
      Object.keys(CONTROL_SESSIONS).map((name) => [
        name,
        "its first reply ran on anthropic/claude-sonnet-5-5, not openai/gpt-6.1-sol",
      ])
    ),
    "c-other-model": "first request",
  });
  expect(openai.summary.inputs.model).toBe("openai/gpt-6.1-sol");
  expect(openai.summary.model).toEqual({ name: "openai/gpt-6.1-sol", firstRead: 1, otherModel: 6 });
  expect(openai.printed).toContain(
    "build-tasks: 1 sessions tried on openai/gpt-6.1-sol read alpha in their first reply; 6 sessions on another model dropped (other-model)"
  );
  expect(openai.printed).toContain(
    "build-tasks: 1 sessions on openai/gpt-6.1-sol read alpha in their first reply, fewer than 5"
  );

  // On F1's model: c-one, c-same, c-secret, c-ungoverned and c-two read alpha in their first
  // reply (c-same asks what c-one asks), c-late does not, and c-other-model is dropped.
  const sonnet = await run("anthropic/claude-sonnet-5-5", "control-model-sonnet");
  const order = blindOrder(names, "1").map(({ item }) => item);
  const firstOfSame = order.find((name) => name === "c-one" || name === "c-same");
  expect(sonnet.tasks.map((task) => task.pathHash)).toEqual(
    order
      .filter((name) => [firstOfSame, "c-secret", "c-two"].includes(name))
      .map((name) => pathHash(controlPath(name)))
  );
  expect(sonnet.outcomes["c-other-model"]).toBe(
    "its first reply ran on openai/gpt-6.1-sol, not anthropic/claude-sonnet-5-5"
  );
  expect(sonnet.outcomes["c-late"]).toBe("not first");
  expect(sonnet.summary.model).toEqual({
    name: "anthropic/claude-sonnet-5-5",
    firstRead: 5,
    otherModel: 1,
  });
  expect(sonnet.printed).not.toContain("in their first reply, fewer than 5");
}, 60_000);

test("build-tasks refuses --sessions, --seed and --max without --first-read, and --store with it, before any read", async () => {
  const sessions = controlSessionsFile("control-refused.jsonl", ["c-one"]);
  const out = path.join(scratch, "control-refused", "tasks.jsonl");
  const from = recorder.requests.length;
  const logged = readFileSync(psqlLog, "utf8").length;
  const pick = ["build-tasks", "--config", config, "--key-file", keyFile, "--skill", old];
  for (const [flag, value] of [
    ["--sessions", sessions],
    ["--seed", "1"],
    ["--max", "5"],
  ]) {
    const lines: string[] = [];
    expect(await main([...pick, "--store", "store_a", flag, value, "--out", out], io(lines))).toBe(
      2
    );
    expect(lines.join("\n")).toContain(`${flag} goes with --first-read only`);
  }
  const lines: string[] = [];
  expect(await main([...controlArgs(sessions, out, "1"), "--store", "store_a"], io(lines))).toBe(2);
  expect(lines.join("\n")).toContain("--first-read takes its sessions from --sessions");
  expect(recorder.requests.length).toBe(from);
  expect(readFileSync(psqlLog, "utf8").length).toBe(logged);
  expect(existsSync(out)).toBe(false);
});

test("removeSecrets takes out each secret shape and counts it, and leaves plain text alone", () => {
  const token = fixtureSecret("tok");
  const cases: [string, string, number][] = [
    // A request header as `curl -v` prints it in a tool result: no command line gives it.
    [
      `> Authorization: Bearer ${token}\n> Accept: */*`,
      `> Authorization: Bearer ${SECRET_REMOVED}\n> Accept: */*`,
      1,
    ],
    [PEM, SECRET_REMOVED, 1],
    [
      `postgres://reader:${token}@db.example/x`,
      `postgres://reader:${SECRET_REMOVED}@db.example/x`,
      1,
    ],
    [`export ACME_API_KEY=${token}9`, `export ACME_API_KEY=${SECRET_REMOVED}`, 1],
    [`"password": "${token}7"`, `"password": "${SECRET_REMOVED}"`, 1],
    ["max_tokens: 16000 and token=short1", "max_tokens: 16000 and token=short1", 0],
    ["const token = createToken(session);", "const token = createToken(session);", 0],
  ];
  for (const [text, expected, removed] of cases) {
    expect(removeSecrets(text)).toEqual({ text: expected, removed });
  }
});

/** The entries of `text` and the messages of its branch, read from the whole text at once: what a
 *  transcript read from its spool, line by line, must give. */
function readWhole(text: string): { entries: SessionEntry[]; messages: SessionMessage[] } {
  const lines = text.split("\n");
  const entries: SessionEntry[] = [];
  for (const [index, line] of lines.entries()) {
    if (line.trim() === "") continue;
    let value: Record<string, unknown>;
    try {
      value = JSON.parse(line);
    } catch {
      if (index === lines.length - 1) break;
      throw new Error(`line ${index + 1} is not JSON`);
    }
    const message = value.message as { content?: unknown } | undefined;
    if (typeof message?.content === "string") {
      message.content = [{ type: "text", text: message.content }];
    }
    entries.push(SessionEntry.parse(value));
  }
  const messages = currentBranch(entries)
    .map(messageOf)
    .filter((message): message is SessionMessage => message !== undefined);
  return { entries, messages };
}

test("a transcript read from its spool, in chunks of any size, gives the entries, branch and messages its whole text gives, and holds no message's text", () => {
  const dir = path.join(scratch, "spools");
  mkdirSync(dir, { recursive: true });
  // Every made-up session, and one with a line of characters of 2 to 4 bytes, a line that ends
  // "\r\n", and a last line still being written.
  const texts = Object.keys(SESSIONS).map((name) => ({ name, text: sessionText(name) }));
  const note = { type: "custom", customType: "note", data: "é 日本 🎉 ".repeat(9) };
  texts.push({
    name: "written",
    text: `${sessionText("good")}${JSON.stringify(note)}\r\n{"type":"message","message":{"ro`,
  });
  const refusedNames: string[] = [];
  for (const { name, text } of texts) {
    const file = path.join(dir, `${name}.jsonl`);
    writeFileSync(file, text);
    let whole: ReturnType<typeof readWhole>;
    try {
      whole = readWhole(text);
    } catch (error) {
      // The broken session: the same line is refused, whatever the chunk.
      for (const chunk of [7, SPOOL_CHUNK]) {
        expect(() => readTranscript(file, chunk)).toThrow((error as Error).message);
      }
      refusedNames.push(name);
      continue;
    }
    for (const chunk of [7, 4096, SPOOL_CHUNK]) {
      const t = readTranscript(file, chunk);
      expect({ name, chunk, messages: t.items.map((_, index) => t.message(index)) }).toEqual({
        name,
        chunk,
        messages: whole.messages,
      });
      expect(t.branch.map((entry) => entry.id)).toEqual(
        currentBranch(whole.entries).map((entry) => entry.id)
      );
      // Each entry of the branch reads back whole; the entries held hold no message's text.
      const branch = currentBranch(whole.entries);
      expect(branch.map((_, at) => t.entry(at))).toEqual(branch);
      expect(t.entries).toHaveLength(whole.entries.length);
      expect(JSON.stringify(t.entries)).not.toContain(LINE);
      expect(JSON.stringify(t.items)).not.toContain(LINE);
    }
  }
  expect(refusedNames).toEqual(["broken"]);
});

/** A sessions file of `count` sessions no store holds, each with no path. */
function absentSessions(file: string, count: number): string {
  const lines = Array.from({ length: count }, (_, n) => ({
    store: "store_a",
    hash: fileHash(`/sessions/absent-${n}.jsonl`),
  }));
  writeFileSync(
    path.join(setupDir, file),
    lines.map((line) => `${JSON.stringify(line)}\n`).join("")
  );
  return path.join(setupDir, file);
}

interface Stopped {
  rule: string;
  sessions: number;
  cases: number;
  spentUsd: number;
  projectedUsd: number | null;
}

const stoppedOf = (out: string): Stopped | null =>
  (JSON.parse(readFileSync(`${out}.summary.json`, "utf8")) as { stopped: Stopped | null }).stopped;

test("the stop rule stops a build with no case once --stop-after-sessions are decided (unbounded projection), and a run that goes on stops again at once, reading nothing", async () => {
  const sessions = absentSessions("absent.jsonl", STOP_AFTER_SESSIONS + 5);
  const out = path.join(scratch, "stop-sessions", "cases.jsonl");
  const lines: string[] = [];
  const args = buildArgs(sessions, out, "1", "--max", "15", "--concurrency", "1");
  expect(await main(args, io(lines))).toBe(EXIT.stoppedByCost);
  const stopped = {
    rule: "stop-projected-usd",
    sessions: STOP_AFTER_SESSIONS,
    cases: 0,
    spentUsd: 0,
    projectedUsd: null,
  };
  expect(stoppedOf(out)).toEqual(stopped);
  const summary = summaryOf(out);
  expect(summary.sessions).toHaveLength(STOP_AFTER_SESSIONS);
  expect(summary.drops).toEqual({ "not-in-store": STOP_AFTER_SESSIONS });
  expect(lines.join("\n")).toContain(
    `build-cases: stopped by --stop-projected-usd: ${STOP_AFTER_SESSIONS} sessions decided, 0 cases, $0.0000 of readers spent, projected unbounded to --max 15 cases`
  );
  expect(existsSync(`${out}.spool`)).toBe(false);

  const from = recorder.requests.length;
  const logged = readFileSync(psqlLog, "utf8").length;
  const again: string[] = [];
  expect(await main(args, io(again))).toBe(EXIT.stoppedByCost);
  expect(again.join("")).toContain(`${STOP_AFTER_SESSIONS} sessions decided there (0 cases)`);
  expect(recorder.requests.length).toBe(from);
  expect(readFileSync(psqlLog, "utf8").length).toBe(logged);
  expect(stoppedOf(out)).toEqual(stopped);
  expect(summaryOf(out).sessions).toHaveLength(STOP_AFTER_SESSIONS);
}, 60_000);

test("the stop rule stops a build with no case as soon as --stop-zero-case-usd is spent, before --stop-after-sessions are decided", async () => {
  const names = ["no-step", "audit-breaks", "audit-not-governed", "no-common", "not-served"];
  const sessions = sessionsFile("stop-usd.jsonl", names);
  const out = path.join(scratch, "stop-usd", "cases.jsonl");
  const lines: string[] = [];
  const args = buildArgs(
    sessions,
    out,
    "1",
    "--concurrency",
    "1",
    "--stop-zero-case-usd",
    "0.0001"
  );
  expect(await main(args, io(lines))).toBe(EXIT.stoppedByCost);
  const summary = summaryOf(out);
  const stopped = stoppedOf(out);
  // It stops after the first session that spent, a drop like those before it.
  const spentAt = summary.sessions.findIndex((each) => each.readerUsd > 0);
  expect(spentAt).toBe(summary.sessions.length - 1);
  expect(summary.sessions.length).toBeLessThan(names.length);
  expect(summary.sessions.every((each) => each.outcome === "dropped")).toBe(true);
  expect(stopped).toEqual({
    rule: "stop-zero-case-usd",
    sessions: summary.sessions.length,
    cases: 0,
    spentUsd: summary.readerSpend.usd,
    projectedUsd: null,
  });
  expect(stopped?.spentUsd).toBeGreaterThanOrEqual(0.0001);
}, 60_000);

test("the stop rule stops a build whose cost projected to --max cases passes --stop-projected-usd, and leaves one under the bound alone", async () => {
  const sessions = sessionsFile("stop-projected.jsonl", ["good", "secret", "skill-md-only"]);
  const bounds = (usd: string) => [
    "--max",
    "5",
    "--concurrency",
    "1",
    "--stop-after-sessions",
    "2",
    "--stop-projected-usd",
    usd,
  ];
  const over = path.join(scratch, "stop-over", "cases.jsonl");
  expect(await main(buildArgs(sessions, over, "1", ...bounds("0.0001")), io([]))).toBe(
    EXIT.stoppedByCost
  );
  const summary = summaryOf(over);
  const spentUsd = summary.readerSpend.usd;
  expect(stoppedOf(over)).toEqual({
    rule: "stop-projected-usd",
    sessions: 2,
    cases: 2,
    spentUsd,
    projectedUsd: (5 * spentUsd) / 2,
  });
  expect(summary.casesBuilt).toBe(2);
  expect(loadCases(over, old)).toHaveLength(2);

  const under = path.join(scratch, "stop-under", "cases.jsonl");
  expect(await main(buildArgs(sessions, under, "1", ...bounds("1000")), io([]))).toBe(EXIT.ok);
  expect(stoppedOf(under)).toBeNull();
  expect(summaryOf(under).casesBuilt).toBe(3);
}, 60_000);

test("build-cases refuses a stop bound that is no number, before any read", async () => {
  const sessions = sessionsFile("stop-refused.jsonl", ["good"]);
  const out = path.join(scratch, "stop-refused", "cases.jsonl");
  const from = recorder.requests.length;
  for (const [flag, value] of [
    ["--stop-projected-usd", "-1"],
    ["--stop-zero-case-usd", "x"],
    ["--stop-after-sessions", "2.5"],
  ]) {
    const lines: string[] = [];
    expect(await main(buildArgs(sessions, out, "1", `${flag}=${value}`), io(lines))).toBe(
      EXIT.refused
    );
    expect(lines.join("\n")).toContain(`${flag} ${value} is not`);
  }
  expect(recorder.requests.length).toBe(from);
  expect(existsSync(out)).toBe(false);
});

test("the pull reads the store command's output row by row with psqlRows' checks and faults, and reports a failing command first", async () => {
  const dir = path.join(scratch, "pull-output");
  mkdirSync(dir, { recursive: true });
  const row = (o: number, c: string, k = 0) => ` ${JSON.stringify({ o, k, c })}`;
  const outputs = [
    ["part", "-----", row(-1, ""), row(0, "a\n"), "(2 rows)", ""],
    ["part", "-----", row(-1, ""), row(0, "a\n"), "(2 rows)"],
    ["no rule here", "(0 rows)"],
    ["-----", row(-1, ""), "(1 row)"],
    [" a | b", "---+---", row(-1, ""), "(1 row)"],
    ["part", "-----", row(-1, ""), "(2 rows)"],
    ["part", "-----", row(-1, ""), "(1 row)", "more"],
    ["part", "-----", " nope", "(1 row)"],
    ["part", "-----", "", row(-1, ""), "(1 row)"],
    ["part", "-----", row(-1, ""), "", ""],
  ].map((lines) => lines.join("\n"));
  /** A store command that prints `output`, named `name`. */
  const printing = (name: string, output: string) => {
    const file = path.join(dir, `${name}.txt`);
    writeFileSync(file, output);
    const command = path.join(dir, `${name}.sh`);
    writeFileSync(command, `#!/bin/sh\ncat '${file}'\n`);
    chmodSync(command, 0o755);
    return command;
  };
  const parsing: number[] = [];
  for (const [n, output] of outputs.entries()) {
    const command = printing(String(n), output);
    const spool = path.join(dir, `${n}.jsonl`);
    let fault: string | undefined;
    try {
      psqlRows(output);
    } catch (error) {
      fault = (error as Error).message;
    }
    if (fault === undefined) {
      parsing.push(n);
      expect(await pullTranscript(command, "store_a", "/sessions/x.jsonl", spool)).toEqual({
        bytes: 2,
        calls: 1,
      });
      expect(readFileSync(spool, "utf8")).toBe("a\n");
    } else {
      expect({
        n,
        error: await pullTranscript(command, "store_a", "/x", spool).catch(String),
      }).toEqual({ n, error: `Error: ${fault}` });
    }
  }
  // Only the two whole tables parse, with a last newline and without; every other output is a
  // fault, so the pull is checked on both.
  expect(parsing).toEqual([0, 1]);
  // An imported file appended to: the file row's two slices, then a part at the file row's byte
  // length, whose DEL psql prints as \x7F; and a part at 0 after them, which overlaps the file row.
  const head = ["part", "-----", row(-1, "ab"), row(-1, "c\n", 1)];
  const imported = printing(
    "imported",
    [...head, String.raw` {"o" : 4, "k" : 0, "c" : "d\x7F\n"}`, "(3 rows)", ""].join("\n")
  );
  const spool = path.join(dir, "imported.jsonl");
  expect(await pullTranscript(imported, "store_a", "/x", spool)).toEqual({ bytes: 7, calls: 1 });
  expect(readFileSync(spool, "utf8")).toBe("abc\nd\u007f\n");
  const overlap = printing("overlap", [...head, row(0, "d\n"), "(3 rows)", ""].join("\n"));
  expect(await pullTranscript(overlap, "store_a", "/x", spool).catch(String)).toBe(
    "Error: pull rows: the part at offset 0 does not start where the text before it ends (4)"
  );
  // A command that fails is reported as failing, whatever it printed.
  const failing = path.join(dir, "failing.sh");
  writeFileSync(failing, "#!/bin/sh\necho 'not a table'\necho 'psql: error: gone' >&2\nexit 3\n");
  chmodSync(failing, 0o755);
  expect(
    await pullTranscript(failing, "store_a", "/x", path.join(dir, "failing.jsonl")).catch(String)
  ).toBe("Error: the store command exited 3: psql: error: gone");
});

/** The profile load.ts's load test takes for a task from session `name`: build-cases' profile of
 *  that session's case, less the tested skill, with the case's context. */
function caseLoadProfile(cases: readonly Case[], name: string): Record<string, unknown> {
  const c = cases.find((each) => each.source.pathHash === pathHash(sessionPath(name)));
  if (c === undefined) throw new Error(`build-cases built no case of ${name}`);
  return {
    ...c.profile,
    skills: c.profile.skills.filter((each) => each.name !== "alpha"),
    context: c.context,
  };
}

/** The cases build-cases builds of the good and legion sessions, for the profile rows. */
async function profileCases(): Promise<Case[]> {
  const out = path.join(scratch, "profile-cases", "cases.jsonl");
  if (!existsSync(out)) {
    const sessions = sessionsFile("profile-cases.jsonl", ["good", "legion"]);
    expect(await main(buildArgs(sessions, out, "1"), io([]))).toBe(EXIT.ok);
  }
  return loadCases(out, old);
}

/** The profile files of the tasks of `out`, by task id. */
function profilesOf(out: string): Record<string, unknown> {
  return Object.fromEntries(
    tasksOf(out).map((task) => [
      String(task.id),
      JSON.parse(readFileSync(String(task.profile), "utf8")) as unknown,
    ])
  );
}

test("build-tasks --profiles-out gives each session task build-cases' profile of its session, less the tested skill, and each written task the --context-session's", async () => {
  const cases = await profileCases();
  const dir = path.join(scratch, "profiles-pick");
  const out = path.join(scratch, "profiles-pick-tasks", "tasks.jsonl");
  const legion = fileHash(sessionPath("legion"));
  const args = [
    "build-tasks",
    "--config",
    config,
    "--key-file",
    keyFile,
    "--skill",
    old,
    "--store",
    "store_a",
    "--out",
    out,
    "--profiles-out",
    dir,
    "--copies",
    copiesDir,
    "--count",
    countDir,
    "--context-session",
    `store_b:${legion}`,
  ];
  const lines: string[] = [];
  expect(await main(args, io(lines))).toBe(EXIT.ok);
  const tasks = tasksOf(out);
  expect(tasks.filter((task) => task.source === "session")).toHaveLength(2);
  expect(tasks.filter((task) => task.source === "written")).toHaveLength(8);
  // task-1's session, a subagent whose listing lacks alpha, is dropped and counted, and task-4's,
  // the next session with the same first message, is offered and taken in its place.
  expect(tasks[0]).toMatchObject({
    text: "Give me the alpha checklist for the fixture project.",
    source: "session",
    pathHash: pathHash("/sessions/task-4.jsonl"),
  });
  expect(lines.join("")).toContain("1 candidate sessions dropped (1 skill-not-listed)");
  for (const task of tasks) expect(task.profile).toBe(path.resolve(dir, `${task.id}.json`));
  const good = caseLoadProfile(cases, "good");
  const written = caseLoadProfile(cases, "legion");
  expect(good).not.toEqual(written);
  expect(profilesOf(out)).toEqual(
    Object.fromEntries(
      tasks.map((task) => [String(task.id), task.source === "session" ? good : written])
    )
  );
  const summary = JSON.parse(readFileSync(`${out}.summary.json`, "utf8"));
  expect(summary.profiles.contextSession).toMatchObject({
    store: "store_b",
    hash: legion,
    pathHash: pathHash(sessionPath("legion")),
    context: written.context,
  });
  expect(
    summary.profiles.tasks.map((each: { task: string; from: string }) => [each.task, each.from])
  ).toEqual(
    tasks.map((task) => [task.id, task.source === "session" ? "session" : "context-session"])
  );
  expect(summary.candidateDrops.counts).toEqual({ "skill-not-listed": 1 });
  expect(summary.candidateDrops.sessions).toEqual([
    {
      store: "store_a",
      pathHash: pathHash("/sessions/task-1.jsonl"),
      reason: "skill-not-listed",
      detail: "its context subagent:task does not list alpha",
      context: "subagent:task",
    },
  ]);
  expect(existsSync(`${out}.spool`)).toBe(false);
}, 120_000);

test("build-tasks refuses a --context-session whose rebuilt context does not list the skill, with exit 2, before any reader call", async () => {
  const out = path.join(scratch, "profiles-unlisted-context", "tasks.jsonl");
  const unlisted = fileHash("/sessions/task-1.jsonl");
  const from = recorder.requests.length;
  const lines: string[] = [];
  const code = await main(
    [
      "build-tasks",
      "--config",
      config,
      "--key-file",
      keyFile,
      "--skill",
      old,
      "--store",
      "store_a",
      "--out",
      out,
      "--profiles-out",
      path.join(scratch, "profiles-unlisted-context"),
      "--copies",
      copiesDir,
      "--count",
      countDir,
      "--context-session",
      `store_a:${unlisted}`,
    ],
    io(lines)
  );
  expect(code).toBe(EXIT.refused);
  expect(lines.join("\n")).toContain(
    `--context-session store_a:${unlisted}: its rebuilt context (subagent:task) does not list alpha`
  );
  expect(recorder.requests.length).toBe(from);
  expect(existsSync(out)).toBe(false);
}, 60_000);

test("build-tasks --first-read --profiles-out gives each control task build-cases' profile of its session, less the tested skill", async () => {
  const cases = await profileCases();
  const names = [...Object.keys(CONTROL_SESSIONS), "c-unlisted"];
  const sessions = controlSessionsFile("profiles-control.jsonl", names, ["c-two"]);
  const dir = path.join(scratch, "profiles-control");
  const out = path.join(scratch, "profiles-control-tasks", "tasks.jsonl");
  const args = [
    ...controlArgs(sessions, out, "1", "--max", "10"),
    "--profiles-out",
    dir,
    "--copies",
    copiesDir,
    "--count",
    countDir,
  ];
  expect(await main(args, io([]))).toBe(EXIT.ok);
  const tasks = tasksOf(out);
  expect(tasks).toHaveLength(3);
  // c-unlisted's first reply reads alpha, but its recorded listing lacks it: dropped and counted.
  const summary = JSON.parse(readFileSync(`${out}.summary.json`, "utf8"));
  expect(summary.skillNotListed).toBe(1);
  expect(
    (summary.sessions as { hash: string; outcome: string; detail?: string }[]).find(
      (each) => each.hash === fileHash(controlPath("c-unlisted"))
    )
  ).toMatchObject({
    outcome: "skill-not-listed",
    detail: "its context subagent:task does not list alpha",
  });
  // Each control session is F1 with its first request replaced, so its profile is the good case's.
  const good = caseLoadProfile(cases, "good");
  expect(profilesOf(out)).toEqual(Object.fromEntries(tasks.map((task) => [String(task.id), good])));
  expect(summary.profiles.contextSession).toBeNull();
  expect(existsSync(`${out}.spool`)).toBe(false);
}, 120_000);

test("build-tasks refuses --profiles-out without --copies and --count, or without --context-session in the reader's mode, the profile flags without --profiles-out, and --context-session with --first-read, before any read", async () => {
  const out = path.join(scratch, "profiles-refused", "tasks.jsonl");
  const pick = ["build-tasks", "--config", config, "--key-file", keyFile, "--skill", old];
  const sessions = controlSessionsFile("profiles-refused.jsonl", ["c-one"]);
  const from = recorder.requests.length;
  const logged = readFileSync(psqlLog, "utf8").length;
  const rows: [string[], string][] = [
    [
      [...pick, "--store", "store_a", "--out", out, "--profiles-out", out],
      "--profiles-out needs --copies and --count",
    ],
    [
      [...pick, "--store", "store_a", "--out", out, "--profiles-out", out, "--copies", copiesDir],
      "--profiles-out needs --copies and --count",
    ],
    [
      [
        ...pick,
        "--store",
        "store_a",
        "--out",
        out,
        "--profiles-out",
        out,
        "--copies",
        copiesDir,
        "--count",
        countDir,
      ],
      "--profiles-out needs --context-session <store>:<hash>",
    ],
    [
      [...pick, "--store", "store_a", "--out", out, "--copies", copiesDir],
      "--copies goes with --profiles-out only",
    ],
    [
      [...pick, "--store", "store_a", "--out", out, "--count", countDir],
      "--count goes with --profiles-out only",
    ],
    [
      [
        ...pick,
        "--store",
        "store_a",
        "--out",
        out,
        "--context-session",
        "store_a:0123456789abcdef",
      ],
      "--context-session goes with --profiles-out only",
    ],
    [
      [
        ...pick,
        "--store",
        "store_a",
        "--out",
        out,
        "--profiles-out",
        out,
        "--copies",
        copiesDir,
        "--count",
        countDir,
        "--context-session",
        "store_b:nothex",
      ],
      "--context-session store_b:nothex is not <store>:<16 hex characters>",
    ],
    [
      [
        ...controlArgs(sessions, out, "1"),
        "--profiles-out",
        out,
        "--copies",
        copiesDir,
        "--count",
        countDir,
        "--context-session",
        `store_a:${fileHash(controlPath("c-one"))}`,
      ],
      "--first-read writes no written task, so it takes no --context-session",
    ],
  ];
  for (const [args, why] of rows) {
    const lines: string[] = [];
    expect({ why, code: await main(args, io(lines)) }).toEqual({ why, code: EXIT.refused });
    expect(lines.join("\n")).toContain(why);
  }
  expect(recorder.requests.length).toBe(from);
  expect(readFileSync(psqlLog, "utf8").length).toBe(logged);
  expect(existsSync(out)).toBe(false);
});

/** The arguments of a build-tasks run in the reader's mode on store_a, with `extra`. */
const tasksArgs = (out: string, ...extra: string[]) => [
  "build-tasks",
  "--config",
  config,
  "--key-file",
  keyFile,
  "--skill",
  old,
  "--store",
  "store_a",
  "--out",
  out,
  ...extra,
];

/** The flags that write each task's profile under `dir`, a written task taking the legion
 *  session's. */
const profileFlags = (dir: string) => [
  "--profiles-out",
  dir,
  "--copies",
  copiesDir,
  "--count",
  countDir,
  "--context-session",
  `store_b:${fileHash(sessionPath("legion"))}`,
];

/** An io that throws on the first printed text `pattern` matches, as a kill then would end the
 *  run: what was saved before the text was printed stays. */
const killedOn = (pattern: RegExp, lines: string[], now = () => performance.now()): BuildIo => ({
  ...io(lines),
  out: (text) => {
    lines.push(text);
    if (pattern.test(text)) throw new Error("killed");
  },
  now,
});

test("build-tasks --max-candidates ends the candidate search once that many candidates are decided; the reader gets the messages accepted by then and writes the rest of the tasks in the --context-session's context, and the summary says the bound ended the search", async () => {
  const dir = path.join(scratch, "bound-candidates");
  const out = path.join(dir, "tasks.jsonl");
  const lines: string[] = [];
  const logged = psqlLogLength();
  expect(
    await main(
      tasksArgs(out, ...profileFlags(path.join(dir, "profiles")), "--max-candidates", "2"),
      io(lines)
    )
  ).toBe(EXIT.ok);
  // task-1's session is dropped (skill-not-listed) and task-4's, of the same text, accepted: two
  // candidates decided, so task-3's, which a search with no bound takes too, is never pulled. The
  // context session and those two are.
  expect(transcriptPulls(logged)).toBe(3);
  const tasks = tasksOf(out);
  expect(tasks.filter((task) => task.source === "session")).toEqual([
    expect.objectContaining({
      text: "Give me the alpha checklist for the fixture project.",
      pathHash: pathHash("/sessions/task-4.jsonl"),
    }),
  ]);
  expect(tasks.filter((task) => task.source === "written")).toHaveLength(9);
  const summary = JSON.parse(readFileSync(`${out}.summary.json`, "utf8"));
  expect(summary.messagesOffered).toBe(1);
  expect(summary.search).toEqual({
    maxCandidates: 2,
    maxSearchMinutes: null,
    ended: "--max-candidates",
    decided: 2,
    accepted: 1,
    seconds: expect.any(Number),
  });
  expect(summary.candidateDrops.counts).toEqual({ "skill-not-listed": 1 });
  expect(
    summary.profiles.tasks.filter((each: { from: string }) => each.from === "context-session")
  ).toHaveLength(9);
  expect(lines.join("")).toContain(
    "build-tasks: the candidate search ended (--max-candidates): 2 candidates decided, 1 accepted, 1 dropped, "
  );
  expect(existsSync(`${out}.candidates.jsonl`)).toBe(false);
}, 120_000);

test("build-tasks --max-search-minutes ends the candidate search at the first candidate after the search's own time reaches it, and the summary says the bound ended the search", async () => {
  const out = path.join(scratch, "bound-minutes", "tasks.jsonl");
  const lines: string[] = [];
  // Each candidate's pull moves the clock a minute, so 1.5 minutes are reached after the second.
  expect(
    await main(tasksArgs(out, "--model", "openai/gpt-6.1-sol", "--max-search-minutes", "1.5"), {
      ...io(lines),
      now: pullClock,
    })
  ).toBe(EXIT.ok);
  const summary = JSON.parse(readFileSync(`${out}.summary.json`, "utf8"));
  expect(summary.search).toEqual({
    maxCandidates: null,
    maxSearchMinutes: 1.5,
    ended: "--max-search-minutes",
    decided: 2,
    accepted: 0,
    seconds: 120,
  });
  // task-1's and task-4's sessions ran on another model; task-3's, on the model, is not reached.
  expect(summary.candidateDrops.counts).toEqual({ "other-model": 2 });
  expect(summary.messagesOffered).toBe(0);
  expect(tasksOf(out).every((task) => task.source === "written")).toBe(true);
  expect(lines.join("")).toContain(
    "build-tasks: the candidate search ended (--max-search-minutes): 2 candidates decided, 0 accepted, 2 dropped, 120 s of search"
  );
}, 60_000);

test("build-tasks with bounds, killed mid-search and run again, pulls no decided candidate again and ends with the tasks and summary of one unkilled run, the search's seconds summed over the runs", async () => {
  const bounds = [
    "--model",
    "openai/gpt-6.1-sol",
    "--max-candidates",
    "60",
    "--max-search-minutes",
    "90",
  ];
  const once = path.join(scratch, "bound-once", "tasks.jsonl");
  const killed = path.join(scratch, "bound-killed", "tasks.jsonl");
  let logged = psqlLogLength();
  expect(await main(tasksArgs(once, ...bounds), { ...io([]), now: pullClock })).toBe(EXIT.ok);
  expect(transcriptPulls(logged)).toBe(3);
  const lines: string[] = [];
  expect(
    await main(
      tasksArgs(killed, ...bounds),
      killedOn(/^build-tasks: candidate .*: dropped /, lines, pullClock)
    )
  ).toBe(EXIT.failed);
  // The candidate decided before the kill is saved, after the bounds.
  expect(
    readFileSync(`${killed}.candidates.jsonl`, "utf8")
      .split("\n")
      .filter((line) => line !== "")
      .map((line) => JSON.parse(line))
  ).toEqual([
    { bounds: { maxCandidates: 60, maxSearchMinutes: 90 } },
    {
      dropped: {
        store: "store_a",
        pathHash: pathHash("/sessions/task-1.jsonl"),
        reason: "other-model",
        detail: "its first reply ran on anthropic/claude-sonnet-5-5, not openai/gpt-6.1-sol",
      },
      seconds: 60,
    },
  ]);
  logged = psqlLogLength();
  const from = recorder.requests.length;
  expect(await main(tasksArgs(killed, ...bounds), { ...io(lines), now: pullClock })).toBe(EXIT.ok);
  expect(lines.join("")).toContain("build-tasks: going on from 1 candidates decided in ");
  // task-1's session is not pulled again; task-4's and task-3's are.
  expect(transcriptPulls(logged)).toBe(2);
  // The terms and the queries were recorded: the one reader call is the pick of the tasks.
  expect(recorder.requests.length - from).toBe(1);
  expect(readFileSync(killed, "utf8")).toBe(readFileSync(once, "utf8"));
  const summary = (out: string) =>
    roundedSpend(
      JSON.parse(readFileSync(`${out}.summary.json`, "utf8")) as Record<string, unknown>
    );
  expect(summary(killed)).toEqual(summary(once));
  expect(summary(killed).search).toEqual({
    maxCandidates: 60,
    maxSearchMinutes: 90,
    ended: "candidates ran out",
    decided: 3,
    accepted: 1,
    seconds: 180,
  });
  expect(existsSync(`${killed}.candidates.jsonl`)).toBe(false);
}, 120_000);

test("build-tasks refuses (exit 2) to go on from a candidate search saved under other bounds, naming both values, and leaves its files as they were; an --out whose search decided nothing takes any bounds", async () => {
  const model = ["--model", "openai/gpt-6.1-sol"];
  const bounds = [...model, "--max-candidates", "60", "--max-search-minutes", "90"];
  const out = path.join(scratch, "bound-refused", "tasks.jsonl");
  expect(await main(tasksArgs(out, ...bounds), killedOn(/^build-tasks: candidate /, []))).toBe(
    EXIT.failed
  );
  const files = [`${out}.candidates.jsonl`, `${out}.summary.json`, `${out}.found.jsonl`];
  const before = files.map((file) => readFileSync(file, "utf8"));
  const from = recorder.requests.length;
  const logged = psqlLogLength();
  const rows: [string[], string][] = [
    [["--max-candidates", "30", "--max-search-minutes", "90"], "--max-candidates was 60, now 30"],
    [["--max-candidates", "60"], "--max-search-minutes was 90, now not given"],
    [[], "--max-candidates was 60, now not given; --max-search-minutes was 90, now not given"],
  ];
  for (const [extra, why] of rows) {
    const lines: string[] = [];
    expect({ why, code: await main(tasksArgs(out, ...model, ...extra), io(lines)) }).toEqual({
      why,
      code: EXIT.refused,
    });
    expect(lines.join("\n")).toContain(
      `${out}.candidates.jsonl holds a candidate search made under other bounds (1 candidates decided)`
    );
    expect(lines.join("\n")).toContain(why);
  }
  expect(files.map((file) => readFileSync(file, "utf8"))).toEqual(before);
  expect(recorder.requests.length).toBe(from);
  expect(psqlLogLength()).toBe(logged);

  // A build killed at its first query has no search file; this one holds only the bounds, as a
  // run killed after its search started and before its first decision leaves it. A run with
  // other bounds goes on from it.
  const queried = path.join(scratch, "bound-queried", "tasks.jsonl");
  expect(
    await main(
      tasksArgs(queried, ...bounds),
      killedOn(/^build-tasks: store_a \d{4}-\d{2}-\d{2}: /, [])
    )
  ).toBe(EXIT.failed);
  writeFileSync(
    `${queried}.candidates.jsonl`,
    `${JSON.stringify({ bounds: { maxCandidates: 60, maxSearchMinutes: 90 } })}\n`
  );
  expect(await main(tasksArgs(queried, ...model, "--max-candidates", "2"), io([]))).toBe(EXIT.ok);
  expect(JSON.parse(readFileSync(`${queried}.summary.json`, "utf8")).search).toMatchObject({
    maxCandidates: 2,
    maxSearchMinutes: null,
    ended: "--max-candidates",
    decided: 2,
  });
}, 120_000);

test("build-tasks refuses a bound that is no number, a bound with no candidate search to bound, and a bound with --first-read, before any read", async () => {
  const out = path.join(scratch, "bound-values", "tasks.jsonl");
  const sessions = controlSessionsFile("bound-values.jsonl", ["c-one"]);
  const model = ["--model", "openai/gpt-6.1-sol"];
  const from = recorder.requests.length;
  const logged = psqlLogLength();
  const rows: [string[], string][] = [
    [
      tasksArgs(out, ...model, "--max-candidates", "0"),
      "--max-candidates 0 is not a whole number of 1 or more",
    ],
    [
      tasksArgs(out, ...model, "--max-candidates", "2.5"),
      "--max-candidates 2.5 is not a whole number of 1 or more",
    ],
    [
      tasksArgs(out, ...model, "--max-search-minutes", "0"),
      "--max-search-minutes 0 is not a number of minutes above 0",
    ],
    [
      tasksArgs(out, ...model, "--max-search-minutes", "soon"),
      "--max-search-minutes soon is not a number of minutes above 0",
    ],
    [
      tasksArgs(out, "--max-candidates", "60"),
      "--max-candidates and --max-search-minutes bound the candidate search, which runs only with --model or --profiles-out",
    ],
    [
      [...controlArgs(sessions, out, "1"), "--max-search-minutes", "90"],
      "--first-read takes no --max-search-minutes: it stops once --max requests are kept",
    ],
  ];
  for (const [args, why] of rows) {
    const lines: string[] = [];
    expect({ why, code: await main(args, io(lines)) }).toEqual({ why, code: EXIT.refused });
    expect(lines.join("\n")).toContain(why);
  }
  expect(recorder.requests.length).toBe(from);
  expect(psqlLogLength()).toBe(logged);
  expect(existsSync(out)).toBe(false);
});

test("build-tasks with no bound writes byte for byte what it wrote before the bounds: the tasks, the summary, the profiles and the printed lines of a run with --model and of one with --profiles-out", async () => {
  /** The SHA-256 of `text`, the suite's scratch directory in it written `<scratch>`. */
  const digest = (text: string) =>
    createHash("sha256").update(text.replaceAll(scratch, "<scratch>")).digest("hex");
  const runs: Record<string, string[]> = {
    model: ["--model", "openai/gpt-6.1-sol"],
    profiles: profileFlags(path.join(scratch, "no-bound-profiles", "profiles")),
  };
  const digests: Record<string, string> = {};
  for (const [name, extra] of Object.entries(runs)) {
    const out = path.join(scratch, `no-bound-${name}`, "tasks.jsonl");
    const lines: string[] = [];
    expect(await main(tasksArgs(out, ...extra), io(lines))).toBe(EXIT.ok);
    expect(existsSync(`${out}.candidates.jsonl`)).toBe(false);
    digests[`${name} tasks`] = digest(readFileSync(out, "utf8"));
    digests[`${name} summary`] = digest(readFileSync(`${out}.summary.json`, "utf8"));
    digests[`${name} printed`] = digest(lines.join(""));
    const profiles = tasksOf(out).flatMap((task) =>
      task.profile === undefined ? [] : [readFileSync(String(task.profile), "utf8")]
    );
    if (profiles.length > 0) digests[`${name} profiles`] = digest(profiles.join(""));
  }
  // The digests of the files and printed lines a run of the code before the bounds gave on the
  // same input. A change to what build-tasks writes changes them.
  expect(digests).toEqual({
    "model tasks": "ffa3d7637184a555b27e1b14afee9b1dd58e697112f82f98d3819afda80652d9",
    "model summary": "fd2cf470c0eaa72f67f8a326ecfaeafd58a63ed82764f9aae296cfffc84bff8b",
    "model printed": "bf894d108dd96d68557fc836907c898f504618a57f906e073f786c51c1144dc3",
    "profiles tasks": "6978ce55a25eb77c1f126e1b0e35e0f4899d63975797aee26b9e32d53ff3dea0",
    "profiles summary": "fa41da9e62a9f60d1a7f75f4b2f29bf7a79251643e4348ada6e11d2899b0e28e",
    "profiles printed": "6e4a4b3f3da89a2faa7e4e21ab71ad7d844a05da52f0aa836ebc41b6baee1532",
    "profiles profiles": "c749ba1f82683dc6045da91d28a90b0238a5d74211f3ef2244d3df0e2af7fbfd",
  });
}, 120_000);
