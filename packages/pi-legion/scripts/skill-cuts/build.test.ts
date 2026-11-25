import { afterAll, beforeAll, describe, expect, mock, test } from "bun:test";
import { createHash } from "node:crypto";
import * as nodeFs from "node:fs";
import {
  chmodSync,
  cpSync,
  existsSync,
  mkdirSync,
  readFileSync,
  symlinkSync,
  writeFileSync,
} from "node:fs";
import path from "node:path";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import { SessionEntry, type SessionMessage, stripSkillHeader } from "../skill-scenarios/transcript";
import { type Arm, buildArm, pathFile, snapshotTag } from "./arms";
import {
  type BuildIo,
  currentBranch,
  DROP_REASONS,
  type DropReason,
  EXCLUDED_READS,
  EXIT,
  fileHash,
  GithubReads,
  ListingMeasure,
  main,
  messageOf,
  partialListing,
  pullTranscript,
  RESTATE_WINDOWS,
  readTranscript,
  registryTarball,
  removeSecrets,
  SECRET_REMOVED,
  SPOOL_CHUNK,
  STOP_AFTER_SESSIONS,
} from "./build";
import { type Case, caseKind, listedName, loadCases, pathHash } from "./cases";
import { GhRetries, MAX_RATE_RETRIES, SECONDARY_WAIT_MS } from "./common";
import {
  FORMER_STAGES_COLUMNS,
  type GhResult,
  INSTALLERS_COLUMNS,
  LOADERS_COLUMNS,
  MANIFESTS_COLUMNS,
  PREFIX_TABLE_COLUMNS,
  REPOSITORIES_COLUMNS,
  SKILL_COPIES_COLUMNS,
  tsv,
} from "./copies";
import { mtimeRange } from "./countsql";
import { blindOrder, stepText } from "./judge";
import { loadProfile, recordedLines } from "./load";
import type { ImageConfig } from "./pod-image";
import { psqlRows } from "./psql-rows";
import { READER_TRIES } from "./readers";
import {
  messageSse,
  type RecordedRequest,
  type Recorder,
  sseResponse,
  startRecorder,
} from "./recorder";
import { LISTING_MEASURE_ITEM } from "./score";

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
/** The start of F1, so of every session built from it. */
const F1_START = "2026-11-20T01:59:41.780Z";
/** The skills directory F1's session read its skills from: its scratch profile's. */
const F1_SKILLS = "/legion/scratch/tmp/acme-1/t1-runs/f1/home/.omp/profiles/fixture/agent/skills";
const PART_CHARS = 200;
const SHA = "0123456789abcdef0123456789abcdef01234567";
/** The commit acme/dots's installer pins acme/pack at (`ensure_vendor`). */
const PACK_PIN = "4444444444444444444444444444444444444444";
/** The commit of the made-up release v1.0.0 that acme/deploy's worker image takes memo from. */
const REL_PIN = "6666666666666666666666666666666666666666";
const DEV_SESSIONS = "/home/fixture/.omp/agent/sessions";
const LEGION_SESSIONS = "/home/fixture/.omp/profiles/legion/agent/sessions";
/** The directory of a made-up root session, whose subagent pinned-subagent is, and the times of
 *  its prompt builds: the last one before the subagent's start is ROOT_BUILD. */
const PINNED_ROOT = `${DEV_SESSIONS}/2026-11-20T00-00-00-000Z_0a1b2c3d-0000-4000-8000-000000000000`;
const ROOT_BUILD = "2026-11-20T01:10:00.000Z";
const ROOT_DIGESTS = ["2026-11-20T00:10:00.000Z", ROOT_BUILD, "2026-11-20T02:30:00.000Z"];
/** acme/powers before power-two came (POWERS_AT): its commit, which GitHub gives at or before a
 *  time before POWERS_AT, and none before the repository was made (POWERS_MADE). */
const POWERS_OLD = "7".repeat(40);
const POWERS_AT = "2026-11-20T01:30:00.000Z";
const POWERS_MADE = "2026-11-20T00:30:00.000Z";
/** The commit of acme/kit's release tag kit-v1.0.0, whose packages/kit ships kit-one alone. */
const KIT_V1 = "8".repeat(40);
/** acme/dots before POWERS_AT: its plugin list pins @acme/kit 1.0.0, and it holds no pool yet. */
const DOTS_OLD = "9".repeat(40);
/** When acme/fresh and acme/newproj got their first commit: after every made-up session's start,
 *  so GitHub gives no commit of either at or before a session's start. */
const FRESH_MADE = "2026-11-20T03:00:00.000Z";
/** The commit of acme/skills a made-up worker image of the project ACME was built from: its
 *  package @acme/pi-acme stages omicron besides alpha and beta. Its first 8 digits, IMAGE_WORD,
 *  are a commit word a comment can name. */
const IMAGE_SHA = "a1".repeat(20);
const IMAGE_WORD = IMAGE_SHA.slice(0, 8);
/** The release tags GitHub lists of each made-up repository. */
const TAGS: Readonly<Record<string, readonly string[]>> = {
  "acme/skills": ["pi-acme-v1.0.0", "acme-image-v2.0.0"],
  "acme/kit": ["kit-v1.0.0", "kit-v2.0.0"],
};
const LINE = "2. Write each step's result on one line.";
/** The limit of a describe's hook that runs builds once for its rows (45 sessions through the
 *  whole pipeline: 111 store calls, each a process, and 89 reader calls). It took 18 s and 28 s on
 *  2026-11-22 and passed 120 s when other runs loaded the pod (a whole file ran 10 times slower
 *  then), so the limit is 20 times the slower measure. */
const HOOK_BUILD_MS = 600_000;
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

/** Puts `count` assistant messages, each of `text(n)`, after F1's message `after` (by default
 *  between its message 6 and its step), each at the time `at`, which is before the next message's. */
function addFillers(
  lines: Record<string, unknown>[],
  count: number,
  text: (n: number) => string,
  { after, at } = { after: 6, at: "2026-11-20T01:59:53.300Z" }
) {
  let previous = lines.filter((line) => line.type === "message")[after] ?? {};
  for (let n = 0; n < count; n += 1) {
    const filler = {
      type: "message",
      id: `f111${String(n).padStart(4, "0")}`,
      timestamp: at,
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

/** A change of F1 that puts a `name` call with `args` (its id `callId`) and its successful result,
 *  whose text is `text`, after F1's prompt: the two lines' ids are `<id>1` and `<id>2`. */
function callAfterPrompt(
  id: string,
  callId: string,
  name: string,
  args: Record<string, unknown>,
  text: string
) {
  return (lines: Record<string, unknown>[]) => {
    const prompt = lines.filter((line) => line.type === "message")[0] ?? {};
    const call = {
      type: "message",
      id: `${id}1`,
      timestamp: "2026-11-20T01:59:45.000Z",
      message: {
        role: "assistant",
        content: [{ type: "toolCall", id: callId, name, arguments: args }],
      },
    };
    const result = {
      type: "message",
      id: `${id}2`,
      timestamp: "2026-11-20T01:59:45.100Z",
      message: {
        role: "toolResult",
        toolCallId: callId,
        toolName: name,
        isError: false,
        content: [{ type: "text", text }],
      },
    };
    insertAfter(lines, prompt, call);
    insertAfter(lines, call, result);
  };
}

/** A change of F1 that puts a read of `url` (`skill://<name>`) and its successful result, which
 *  starts `[Skill file: <header>]` and then holds `text`, after F1's prompt: the two lines' ids are
 *  `<id>1` and `<id>2`. */
function readFirst(id: string, url: string, header: string, text: string) {
  const callId = `toolu_${url.slice("skill://".length)}`;
  return callAfterPrompt(id, callId, "read", { path: url }, `[Skill file: ${header}]\n${text}`);
}

/** pinned-units' change of F1: before alpha, a read of acme/powers' power-one (a package
 *  acme/dots's plugin list installs) and one of pack-one (of the pool acme/dots's installer
 *  clones). */
function readsPinnedUnits(lines: Record<string, unknown>[]): void {
  readFirst("90e0000", "skill://power-one", POWERS_ONE, "# power-one\n")(lines);
  readFirst("9ac0000", "skill://pack-one", PACK_ONE, "# pack-one\n")(lines);
}

/** Where the renamed-plugin session reads zeta: Oh My Pi's cache of the plugin zeta-tools of the
 *  marketplace old-market (`plugins/cache/plugins/<marketplace>___<plugin>___<version>/`). */
const OLD_MARKET_ZETA =
  "/home/dev/.omp/plugins/cache/plugins/old-market___zeta-tools___0.1.0/skills/zeta/SKILL.md";

/** Where the pinned-units session reads power-one and pack-one: a dev box's Oh My Pi plugins
 *  directory, which holds acme/powers under the dependency's name acme-powers, and the clone of
 *  acme/pack in its dotfiles checkout's vendor/acme-pack. */
const POWERS_ONE =
  "/home/dev/.dotfiles/omp/plugins/node_modules/acme-powers/skills/power-one/SKILL.md";
const PACK_ONE = "/home/dev/.dotfiles/vendor/acme-pack/skills/pack-one/SKILL.md";

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
  // A dev-box session that reads, before alpha, a skill of the package acme/dots's plugin list
  // installs from acme/powers as acme-powers (a `github:` dependency), and a skill of the pool
  // acme/dots's installer clones from acme/pack into vendor/acme-pack.
  "pinned-units": {
    store: "store_a",
    change: readsPinnedUnits,
  },
  // pinned-units' reads in a subagent of the root PINNED_ROOT, which built its prompt last at
  // ROOT_BUILD before the subagent's start: acme/powers (a package) is read then, when it had
  // power-one alone (POWERS_OLD), and acme/dots and the pool it clones are read at the subagent's
  // start. Its recorded listing names pack-one, which it reads, so its profile has the pool.
  "pinned-subagent": {
    store: "store_a",
    path: `${PINNED_ROOT}/pinned-subagent.jsonl`,
    change: (lines) => {
      readsPinnedUnits(lines);
      insertAfter(lines, branchHead(lines), {
        type: "session_init",
        id: "5e551019",
        timestamp: "2026-11-20T01:59:43.500Z",
        agent: "task",
        resolvedModel: "anthropic/claude-sonnet-5-5",
        task: "Read the alpha steps.",
        tools: SUBAGENT_TOOLS,
        systemPrompt: [
          (SUBAGENT_PROMPT[0] ?? "").replace(
            "</skills>",
            "- pack-one: Use for the pack-one steps.\n</skills>"
          ),
          ...SUBAGENT_PROMPT.slice(1),
        ],
      });
    },
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
  // A Legion pane in a workspace of acme/proj, whose own skills are in its `.claude/skills/` and
  // `.omp/skills/`; it reads alpha as the legion session does.
  "proj-legion": {
    store: "store_b",
    change: (lines) => {
      SESSIONS.legion?.change?.(lines);
      const header = lines.find((line) => line.type === "session");
      if (header !== undefined) header.cwd = "/legion/workspaces/acme/proj/K-1/";
    },
  },
  // A dev-box session in a checkout of acme/proj, the one repository of the copies output's list
  // named `proj`.
  "proj-devbox": { store: "store_a", change: workingDirectory("/home/dev/src/proj") },
  // A dev-box session under a directory named `skills`, the name of two repositories of the list.
  "two-repos": { store: "store_a", change: workingDirectory("/home/dev/src/skills/notes") },
  // A dev-box session in a directory below the root of a checkout of acme/proj.
  "proj-subdir": {
    store: "store_a",
    change: workingDirectory("/home/dev/src/proj/agents/agentvm"),
  },
  // A dev-box session in a checkout of acme/dual, whose root holds .claude/CLAUDE.md, AGENTS.md
  // and CLAUDE.md.
  "dual-root": { store: "store_a", change: workingDirectory("/home/dev/src/dual") },
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
    path: `${LEGION_SESSIONS}/--legion-workspaces-acme-widgets-acme-12--/claimed.jsonl`,
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

/** A change of F1 that makes the read call at message `index` a read of `target` by its path, and
 *  its result (the next message) a successful one in the form Oh My Pi shows a read by path in
 *  hashline rows: `[<path>#<tag>]`, then `<n>:<line>` for each line of `text`, by default the text
 *  F1's result showed less its `[Skill file: …]` line. */
function readByPathAt(index: number, target: string, text?: string) {
  return (lines: Record<string, unknown>[]) => {
    setReadTarget(lines, index, target);
    const result = messageLines(lines)[index + 1]?.message;
    if (result === undefined) throw new Error(`F1 has no message ${index + 1}`);
    const body = text ?? stripSkillHeader(result.content?.[0]?.text ?? "");
    const rows = body.split("\n").map((line, n) => `${n + 1}:${line}`);
    const header = `[${target.replace(/:[^/]*$/, "")}#${snapshotTag(body)}]`;
    result.content = [{ type: "text", text: [header, ...rows].join("\n") }];
    result.isError = false;
    result.details = { totalLines: rows.length, rowPrefixes: true };
  };
}

/** F1's working directory and the home directory of its scratch profile (testdata/CAPTURE.md):
 *  F1_SKILLS is under the home, at `.omp/profiles/fixture/agent/skills`. */
const F1_CWD = "/legion/scratch/tmp/acme-1/t1-runs/f1/work";
const F1_HOME = "/legion/scratch/tmp/acme-1/t1-runs/f1/home";
/** F1's copy of alpha relative to F1_CWD, and under `~` (F1_HOME). */
const F1_RELATIVE = "../home/.omp/profiles/fixture/agent/skills/alpha";
const F1_TILDE = "~/.omp/profiles/fixture/agent/skills/alpha";

/** A change of F1 that reads alpha's two files by path under `dir` at messages 1 and 3. */
const readsUnder = (dir: string) => (lines: Record<string, unknown>[]) => {
  readByPathAt(1, `${dir}/SKILL.md`)(lines);
  readByPathAt(3, `${dir}/references/x.md`)(lines);
};

/** The made-up sessions that read alpha by its path, in store_a (or their own store) at
 *  `/sessions/<name>.jsonl` (or their own path): each is F1 changed by its entry, as SESSIONS'
 *  are, and none is in the build over every made-up session. F1's reads of alpha (messages 1 and
 *  3) become reads by path: of the copy F1 read, which the prefix table places at the cut's copy,
 *  by its absolute path (path-read), relative to F1's working directory (path-relative-read), or
 *  under `~` in a session whose transcript is under that home's `.omp/` (path-home); the relative
 *  ones in a session whose header names no working directory (path-no-cwd), and so the `@~dev/`
 *  ones, which are relative too (path-at-name), and the `~` ones in one whose transcript's path
 *  names no home (path-no-home); with a read selector (path-range); of acme/other's copy
 *  (path-other); or in a checkout of `skills/alpha` whose path leaves the copies of several
 *  repositories tied, among which the store's context decides: no directory of the path names
 *  any of the three repositories that have that directory, and store_a's context installs one of
 *  them, acme/skills's, the cut's copy (path-tie); or a directory `skills` names both acme/skills
 *  and its fork forker/skills, and store_c's context installs neither, acme/other's alone
 *  (path-unattributed). path-mixed keeps F1's skill:// reads and reads acme/other's copy by path
 *  at message 5; path-form is path-read with a read at message 5 of a file that only the new text
 *  holds, in a form the arms do not answer (NEW_WIDE). */
const BY_PATH: Record<
  string,
  { change: (lines: Record<string, unknown>[]) => void; path?: string; store?: string }
> = {
  "path-read": { change: readsUnder(`${F1_SKILLS}/alpha`) },
  "path-relative-read": { change: readsUnder(F1_RELATIVE) },
  "path-home": {
    change: readsUnder(F1_TILDE),
    path: `${F1_HOME}/.omp/agent/sessions/--work--/path-home.jsonl`,
  },
  "path-no-cwd": {
    change: (lines) => {
      readsUnder(F1_RELATIVE)(lines);
      const header = lines.find((line) => line.type === "session");
      if (header !== undefined) header.cwd = undefined;
    },
  },
  "path-no-home": { change: readsUnder(F1_TILDE) },
  // `@~dev/…` in a session whose header names no working directory: Oh My Pi's read takes `@`
  // off only before `/`, `~` alone or `~/` (cases.ts's resolvedReadPath), so this path is
  // relative to the working directory, not under a home.
  "path-at-name": {
    change: (lines) => {
      readsUnder(F1_TILDE.replace("~", "@~dev"))(lines);
      const header = lines.find((line) => line.type === "session");
      if (header !== undefined) header.cwd = undefined;
    },
  },
  "path-range": {
    change: (lines) => {
      readByPathAt(1, `${F1_SKILLS}/alpha/SKILL.md:1-20`)(lines);
      readByPathAt(3, `${F1_SKILLS}/alpha/references/x.md:raw`)(lines);
    },
  },
  "path-other": { change: readsUnder("/elsewhere/other-skills/alpha") },
  "path-tie": { change: readsUnder("/home/dev/notes/skills/alpha") },
  "path-unattributed": {
    store: "store_c",
    change: readsUnder("/home/dev/src/skills/skills/alpha"),
  },
  "path-mixed": {
    change: readByPathAt(5, "/elsewhere/other-skills/alpha/SKILL.md", "# alpha of acme/other"),
  },
  "path-form": {
    change: (lines) => {
      readsUnder(`${F1_SKILLS}/alpha`)(lines);
      readByPathAt(5, `${F1_SKILLS}/alpha/references/wide.md`, "Line 1.")(lines);
    },
  },
};

/** A file only the new text of alpha holds: 301 lines, more than a read by path shows whole, so
 *  the arms do not answer a read of it (arms.ts's `pathFormFault`). */
const NEW_WIDE = {
  path: "references/wide.md",
  text: Array.from({ length: 301 }, (_, n) => `Line ${n + 1}.`).join("\n"),
};

/** The made-up sessions whose work at the skill's read is on alpha's own text, in store_a at
 *  `/sessions/<name>.jsonl`, each F1 changed by its entry: a session that rewrites the skill and
 *  reads it by skill:// (material-writing), and one in an eval checkout that grades the skill and
 *  reads its files whole by path (material-eval). The stand-in case reader answers `material` for
 *  each; none is in the build over every made-up session. */
const MATERIAL: Record<string, { change: (lines: Record<string, unknown>[]) => void }> = {
  "material-writing": {
    change: (lines) =>
      setText(lines, 0, () => "Rewrite the alpha skill's SKILL.md so that each rule is shorter."),
  },
  "material-eval": {
    change: (lines) => {
      setText(lines, 0, () => "Grade the alpha skill's text against the eval's rubric.");
      readsUnder(`${F1_SKILLS}/alpha`)(lines);
      workingDirectory("/home/dev/evals/alpha-grade")(lines);
    },
  },
};

/** A change of F1 that sets the working directory its header names. */
function workingDirectory(cwd: string) {
  return (lines: Record<string, unknown>[]) => {
    const header = lines.find((line) => line.type === "session");
    if (header !== undefined) header.cwd = cwd;
  };
}

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
const sessionPath = (name: string) =>
  SESSIONS[name]?.path ??
  BY_PATH[name]?.path ??
  (SESSIONS[name]?.store === "store_b"
    ? `${LEGION_SESSIONS}/${name}.jsonl`
    : BY_PATH[name] !== undefined && !["path-tie", "path-unattributed"].includes(name)
      ? `/sessions/${name}.jsonl`
      : `${DEV_SESSIONS}/${name}.jsonl`);

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
/** The messages the nav-step session adds after F1's message 6: its task call is then message 42,
 *  past the case reader's first window (messages 1 to 40) and inside the window that starts after
 *  its read of references/x.md (message 3). */
const NAV_FILLERS = 35;
/** The messages the nav-earlier session adds after F1's message 2, before its message 3's time:
 *  its read of references/x.md is then message 38, inside the case reader's first window (messages
 *  1 to 40) and among the 20 messages shown before its second (messages 41 to 46), and its task
 *  call message 42, inside that second window. */
const EARLIER_FILLERS = 35;

/** Session `name`'s transcript text: F1, changed by `change` (the session's own, unless given). */
function sessionText(
  name: string,
  change: ((lines: Record<string, unknown>[]) => void) | undefined = SESSIONS[name]?.change ??
    BY_PATH[name]?.change ??
    MATERIAL[name]?.change
): string {
  const lines = f1Lines();
  setText(lines, 5, () => `Step 3 (case ${name}).`);
  change?.(lines);
  return lines
    .map((line) => (line.notJson === true ? "{not json" : JSON.stringify(line)))
    .map((line) => `${line}\n`)
    .join("");
}

/** The tasks rows of the stand-in store: a first user message, the day its file is of. */
const TASK_ROWS = [
  {
    dayStart: mtimeRange("2026-11-20").from,
    path: `${DEV_SESSIONS}/task-1.jsonl`,
    text: "Give me the alpha checklist for the fixture project.",
  },
  {
    dayStart: mtimeRange("2026-11-20").from,
    path: `${DEV_SESSIONS}/task-2.jsonl`,
    text: "Fix the beta greeting.",
  },
  {
    dayStart: mtimeRange("2026-11-21").from,
    path: `${DEV_SESSIONS}/task-3.jsonl`,
    text: "Write the Alpha report of step 2.",
  },
  {
    dayStart: mtimeRange("2026-11-21").from,
    path: `${DEV_SESSIONS}/task-4.jsonl`,
    text: "Give me the alpha checklist for the fixture project.",
  },
];

/** The stand-in store command: it answers pull.sql, sessions.sql, tasks.sql, session-head.sql,
 *  session-paths.sql, subagents.sql and digest-times.sql from the store file `storeFile`, and
 *  appends each call to `logFile`, with the times (ms since the epoch) it started and ended. */
const STANDIN_PSQL = (storeFile: string, logFile: string) => `
import { appendFileSync, readFileSync } from "node:fs";
import { createHash } from "node:crypto";
const started = Date.now();
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
} else if (query.includes("json_build_object('path', h.path")) {
  kind = "paths";
  // The path of each file of a path hash the query names, in path order.
  const hashes = new Set(literals(query.slice(query.indexOf("ANY"))));
  rows = Object.keys(store.files)
    .map((p) => ({ path: p, hash: createHash("sha256").update(p).digest("hex") }))
    .filter((row) => hashes.has(row.hash))
    .sort((a, b) => (a.path < b.path ? -1 : 1));
} else if (query.includes("'subagents', r.subagents")) {
  kind = "subagents";
  // For each session and times the query names, the files one level below its directory: the
  // earliest by header timestamp (then path) whose session_init line holds <skills> and whose
  // start is at or after \`from\` and before \`to\`, or none, and counts.
  const wanted = JSON.parse(literals(query.slice(query.indexOf("jsonb_array_elements(")))[0]);
  const sha = (p) => createHash("sha256").update(p).digest("hex");
  const seen = new Set();
  rows = [];
  for (const { hash, from, to } of wanted) {
    const key = [hash, from, to].join("\\u0000");
    if (seen.has(key)) continue;
    seen.add(key);
    const parent = Object.keys(store.files).find((p) => sha(p) === hash && p.endsWith(".jsonl"));
    if (parent === undefined) continue;
    const dir = parent.slice(0, -".jsonl".length) + "/";
    const children = Object.entries(store.files)
      .filter(([p]) => p.startsWith(dir) && !p.slice(dir.length).includes("/") && p.endsWith(".jsonl"))
      .map(([p, text]) => {
        const lines = text.split("\\n");
        const header = lines.find((line) => line.includes('"type":"session"'));
        const init = lines.find((line) => line.includes('"type":"session_init"') && line.includes("<skills>"));
        return { path: p, start: header === undefined ? null : JSON.parse(header).timestamp ?? null, init };
      });
    if (children.length === 0) continue;
    const recorded = children.filter((child) => child.init !== undefined);
    const at = (child) => Date.parse(child.start ?? "");
    const under = recorded
      .filter((child) => (from === null || at(child) >= Date.parse(from)) && (to === null || at(child) < Date.parse(to)))
      .sort((a, b) => ((a.start ?? "") + "\\u0000" + a.path < (b.start ?? "") + "\\u0000" + b.path ? -1 : 1));
    const first = under[0];
    rows.push({
      parent: hash,
      from,
      to,
      hash: first === undefined ? null : sha(first.path),
      start: first === undefined ? null : first.start,
      init: first === undefined ? null : first.init,
      subagents: children.length,
      recorded: recorded.length,
      under: under.length,
    });
  }
  const order = (row) => [row.parent, row.from ?? "", row.to ?? ""].join("\\u0000");
  rows.sort((a, b) => (order(a) < order(b) ? -1 : 1));
} else if (query.includes("AS digest_times")) {
  kind = "digests";
  // For each session the query names, its root (the topmost directory named as a top-level
  // session's, and .jsonl; or the file itself, below no such directory) and the time and digest
  // of each of the root's digest lines, earliest first.
  const hashes = new Set(literals(query.slice(query.indexOf("unnest("))));
  const sha = (p) => createHash("sha256").update(p).digest("hex");
  rows = [];
  for (const file of Object.keys(store.files).filter((p) => hashes.has(sha(p)))) {
    const dir = /^(.*?\\/[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9-]+Z_[0-9a-f-]+)\\//.exec(file)?.[1];
    const root = dir === undefined ? file : dir + ".jsonl";
    const text = store.files[root];
    const digests = [...(text ?? "").matchAll(/"customType":"system-prompt-digest","data":"([^"]*)","id":"[^"]*","parentId":(?:"[^"]*"|null),"timestamp":"([^"]+)"/g)]
      .map((m) => ({ at: m[2], digest: m[1] }))
      .sort((a, b) => Date.parse(a.at) - Date.parse(b.at) || (a.digest < b.digest ? -1 : 1));
    rows.push({ hash: sha(file), root: sha(root), rootHeld: text !== undefined, digests });
  }
  rows.sort((a, b) => (a.hash < b.hash ? -1 : 1));
} else if (query.includes("encode(sha256(convert_to(f.path")) {
  kind = "heads";
  // Each file of a path hash the query names: its header's cwd and timestamp, its first user turn
  // and its first model_change line's runtime version, from its whole text (a made-up file is
  // shorter than the head session-head.sql reads).
  const hashes = new Set(literals(query.slice(query.indexOf("ANY"))));
  rows = Object.entries(store.files)
    .map(([p, text]) => ({ text, hash: createHash("sha256").update(p).digest("hex") }))
    .filter((file) => hashes.has(file.hash))
    .sort((a, b) => (a.hash < b.hash ? -1 : 1))
    .map(({ text, hash }) => {
      const lines = text.split("\\n").flatMap((line) => {
        try {
          return [JSON.parse(line)];
        } catch {
          return [];
        }
      });
      const header = lines.find((line) => line.type === "session");
      const user = lines.find((line) => line.type === "message" && line.message?.role === "user");
      return {
        hash,
        cwd: header?.cwd ?? null,
        start: header?.timestamp ?? null,
        first_user: user === undefined ? null : {
          timestamp: typeof user.message.timestamp === "number" ? user.message.timestamp : null,
          attribution: typeof user.message.attribution === "string" ? user.message.attribution : null,
        },
        reminders: [],
        runtime: lines.find((line) => line.type === "model_change")?.runtime?.version ?? null,
        bytes: Buffer.byteLength(text, "utf8"),
        bad_lines: 0,
      };
    });
} else {
  process.stderr.write("psql: error: not a query of the runner\\n");
  process.exit(3);
}
// A row that needs a pull to take a while (two runs of one --out) sets this delay.
const delay = Number(process.env.SKILL_CUTS_STANDIN_PULL_DELAY_MS ?? 0);
if (kind === "pull" && delay > 0) await Bun.sleep(delay);
appendFileSync(${JSON.stringify(logFile)}, JSON.stringify({ db, kind, rows: rows.length, after, first: rows[0] && [rows[0].o, rows[0].k], limit64: kind !== "pull" || query.includes("LIMIT 64"), started, ended: Date.now() }) + "\\n");
process.stdout.write(" value \\n-------\\n" + rows.map((row) => " " + JSON.stringify(row) + "\\n").join("") + "(" + rows.length + (rows.length === 1 ? " row" : " rows") + ")\\n\\n");
`;

/** The words the `restated` session's prompt adds, which state LINE in other words. */
const RESTATED = "Put the result of each step on a single line.";

/** The criterion the stand-in case reader writes. */
const CRITERION = {
  pass: "The step writes each step's result on one line.",
  fail: "The step spreads a result over more than one line.",
};

/** F1's line of SKILL.md that only sends the agent to a file of the skill (its line 8), which the
 *  stand-in navigation reader calls navigation, and the line of references/x.md (its line 3). */
const NAVIGATION_LINE = "1. Read `references/x.md` before the first step.";
const X_LINE =
  "The alpha steps are counted from one, and each result line starts with its step number.";
/** How F1's read of references/x.md (its message 3) shows in a reader's transcript. */
const X_READ = '[tool call: read] {"path":"skill://alpha/references/x.md"';

/** The sessions whose first proposal is the read of references/x.md under NAVIGATION_LINE. */
const NAV_READS = ["nav-step", "nav-skill-md", "nav-nothing", "nav-earlier"];

/** The proposal of the stand-in case reader for the session named in the transcript: the step is
 *  the message of the window that calls the `task` tool (F1's message 7) under LINE, and a window
 *  that holds none gives no step. But: nav-line's step is under NAVIGATION_LINE; ref-read's step
 *  is the read of references/x.md (F1's message 3, X_READ) under LINE; and NAV_READS's step, where
 *  the window offers SKILL.md's lines, is that read under NAVIGATION_LINE, and where it does not,
 *  nav-step's and nav-earlier's is the task call under X_LINE, nav-skill-md's the task call under
 *  LINE, and nav-nothing gives no step. nav-earlier gives no step until a window holds its task
 *  call. A `material-` session gives the answer that the agent works on the skill's own text. */
function caseAnswer(name: string, transcript: string): unknown {
  const criterion = CRITERION;
  const messages = transcript.split('<message index="');
  const step = messages.find((message) => message.includes("[tool call: task]"))?.split('"')[0];
  const read = messages.find((message) => message.includes(X_READ))?.split('"')[0];
  const proposal = (messageIndex: string, line: { file: string; text: string }) => ({
    step: { messageIndex: Number(messageIndex), what: "calls a tool" },
    line,
    criterion,
    violatingStep: { text: "Step 4.", toolCalls: [VIOLATING_CALL] },
    // Message 0, the user's prompt, comes before the skill's read: the reader sees it only in
    // the messages before the read.
    restatements: name === "restated" ? [{ messageIndex: 0, quote: RESTATED }] : [],
  });
  const noStep = { step: null, reason: "No step of the transcript is decided by the skill." };
  if (name.startsWith("material-")) {
    return { step: null, material: { why: "The agent works on alpha's text (message 0)." } };
  }
  if (name === "nav-earlier" && step === undefined) return noStep;
  if (NAV_READS.includes(name) && read !== undefined && transcript.includes(SKILL_MD_OFFERED)) {
    return proposal(read, { file: "SKILL.md", text: NAVIGATION_LINE });
  }
  if (name === "ref-read" && read !== undefined)
    return proposal(read, { file: "SKILL.md", text: LINE });
  if (name === "no-step" || name === "nav-nothing" || step === undefined) return noStep;
  if (name === "nav-step" || name === "nav-earlier") {
    return proposal(step, { file: "references/x.md", text: X_LINE });
  }
  if (name === "nav-line") return proposal(step, { file: "SKILL.md", text: NAVIGATION_LINE });
  const text = name === "not-in-base" ? "2. Write each step's result on two lines." : LINE;
  return proposal(step, { file: "SKILL.md", text });
}

/** How a case request shows that it offers SKILL.md's lines. */
const SKILL_MD_OFFERED = '<file path="SKILL.md">';

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
const controlPath = (name: string) => `${DEV_SESSIONS}/${name}.jsonl`;

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

/** The first words of the restate, audit, case and navigation prompts, by which the stand-in tells
 *  their requests. */
const RESTATE_PROMPT = "You read part of a coding agent's session and find each message";
const AUDIT_PROMPT = "You decide whether one step of a coding agent's session follows";
const CASE_PROMPT = "You read part of a coding agent's session and propose one case";
const NAVIGATION_PROMPT = "You decide whether one line of a coding agent's skill";

/** The stand-in navigation reader: NAVIGATION_LINE is navigation, with NAVIGATION_REASON, and no
 *  other line is. */
const NAVIGATION_REASON = "The line only sends the agent to references/x.md.";
function navigationAnswer(user: string): unknown {
  return user.includes(`<line>\n${NAVIGATION_LINE}\n</line>`)
    ? { navigation: true, reason: NAVIGATION_REASON }
    : { navigation: false, reason: "The line asks for an act a step shows." };
}

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

/** The first words of the written-case prompt, by which the stand-in tells its requests. */
const WRITTEN_PROMPT = "You write tasks for a behaviour test of a coding agent's skill.";
/** A written task whose following step writes each step's result on a line of its own, LINE. */
const WRITTEN_TASK = {
  task: "Write the alpha report of the fixture project.",
  existing: [] as string[],
  line: { file: "SKILL.md", number: 9, text: LINE },
  criterion: CRITERION,
  followingStep: {
    text: "",
    toolCalls: [{ name: "write", arguments: { path: "report.md", content: "1. done\n2. done" } }],
  },
  violatingStep: {
    text: "",
    toolCalls: [{ name: "write", arguments: { path: "report.md", content: "1. done, 2. done" } }],
  },
};
/** The stand-in written-case reader's tasks where `<navigation>` names no line: one the build
 *  keeps; one whose task states the line in other words (RESTATED), which the stand-in restatement
 *  check finds; one whose task names the audit-breaks session, so the stand-in audit says its
 *  following step breaks the line; one whose line is NAVIGATION_LINE; and one whose following step
 *  reads references/x.md under LINE. */
const WRITTEN_TASKS = [
  WRITTEN_TASK,
  { ...WRITTEN_TASK, task: `Write the alpha report of the fixture project. ${RESTATED}` },
  { ...WRITTEN_TASK, task: "Write the alpha report of the fixture project (case audit-breaks)." },
  {
    ...WRITTEN_TASK,
    task: "Write the alpha report of the fixture project from its notes.",
    line: { file: "SKILL.md", number: 8, text: NAVIGATION_LINE },
  },
  {
    ...WRITTEN_TASK,
    task: "Write the alpha report of the fixture project in its own form.",
    followingStep: {
      text: "",
      toolCalls: [{ name: "read", arguments: { path: "skill://alpha/references/x.md" } }],
    },
  },
];
/** The stand-in written-case reader's one task where `<navigation>` names a line: its line,
 *  X_LINE, is a line of references/x.md, and its history is the task and the skill's read alone
 *  (the agent reads references/x.md itself in the run). */
const WRITTEN_REFERENCE_TASK = {
  task: "Write the alpha report of the fixture project with each step's result.",
  existing: [] as string[],
  line: { file: "references/x.md", number: 3, text: X_LINE },
  criterion: {
    pass: "The step starts each result line with its step number, counted from one.",
    fail: "The step writes a result line without its step number.",
  },
  followingStep: WRITTEN_TASK.followingStep,
  violatingStep: {
    text: "",
    toolCalls: [{ name: "write", arguments: { path: "report.md", content: "done\ndone" } }],
  },
};

/** A test acme/proj's commit SHA does not hold, which its branch did. */
const PROJ_FARM = "tools/tests/test_farm.py";

/** The first message of a context session whose material the stand-in written-case reader's model
 *  refuses: each of its replies stops at refusal, with no text (`refusedReply`). */
const REFUSED_REQUEST = "Write the alpha report of the made-up lock picking kit.";
/** The first message of a context session whose first reply of each READER_TRIES the stand-in
 *  written-case reader's model refuses (`refusedReply`); its others end their turn with
 *  UNWRITTEN, no JSON. */
const PARTLY_REFUSED_REQUEST = "Write the alpha report of the made-up lock notes.";
const UNWRITTEN = "I would rather not write these tasks.";
/** The first message of a context session whose every answer of the stand-in written-case reader
 *  the reader's schema refuses: its one task, MISLINED_TASK, names a line of SKILL.md by a text
 *  that is not that line's. */
const MISLINED_REQUEST = "Write the alpha report of the fixture project's mislaid notes.";
const MISLINED_TASK = {
  ...WRITTEN_TASK,
  line: { file: "SKILL.md", number: 9, text: "No line of alpha says this." },
};
/** The first message of a context session for which the stand-in answers each request of the
 *  written-case reader with a 400, which no try mends: the reader's call fails. */
const ERRED_REQUEST = "Write the alpha report of the fixture project past the gateway.";

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
  if (system.startsWith(NAVIGATION_PROMPT)) return JSON.stringify(navigationAnswer(user));
  if (system.startsWith(WRITTEN_PROMPT)) {
    if (user.includes(`<first-message>\n${PARTLY_REFUSED_REQUEST}\n`)) return UNWRITTEN;
    if (user.includes(`<first-message>\n${MISLINED_REQUEST}\n`)) {
      return JSON.stringify({ tasks: [MISLINED_TASK] });
    }
    // A session whose work is on alpha's own text: no task, as the prompt asks.
    if (user.includes("<first-message>\nRewrite the alpha skill")) {
      return JSON.stringify({
        tasks: [],
        material: { why: "The session rewrites alpha's SKILL.md (its first message)." },
      });
    }
    // A dev-box session in a checkout of acme/proj: tasks that each treat a test the commit lacks
    // as there, until the request names that test as absent from the checkout.
    if (user.includes('root="/home/dev/src/proj" cwd="/home/dev/src/proj"')) {
      return JSON.stringify({
        tasks: user.includes(`<absent>\n${PROJ_FARM}\n</absent>`)
          ? [
              {
                ...WRITTEN_TASK,
                task: "Add the farm's first test, as CLAUDE.md asks.",
                existing: ["CLAUDE.md"],
              },
            ]
          : [1, 2].map((n) => ({
              ...WRITTEN_TASK,
              task: `Make ${PROJ_FARM} pass again (${n}).`,
              existing: [PROJ_FARM],
            })),
      });
    }
    if (user.includes("<navigation>\n<line ")) {
      return JSON.stringify({ tasks: [WRITTEN_REFERENCE_TASK] });
    }
    const count = Number(/<count>(\d+)<\/count>/.exec(user)?.[1] ?? "1");
    return JSON.stringify({ tasks: WRITTEN_TASKS.slice(0, count) });
  }
  if (system.startsWith(CASE_PROMPT)) {
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

/** Whether `body` is a request of the stand-in written-case reader for a context session whose
 *  first message is `request`. */
function writtenRequestFor(body: Parameters<typeof readerAnswer>[0], request: string): boolean {
  if (!(body.system[0]?.text ?? "").startsWith(WRITTEN_PROMPT)) return false;
  const user = body.messages[0]?.content.map((part) => part.text).join("\n") ?? "";
  return user.includes(`<first-message>\n${request}\n`);
}

/** The stand-in written-case reader's replies so far for the context session whose first message
 *  is PARTLY_REFUSED_REQUEST. */
let partlyRefusedReplies = 0;

/** Whether the stand-in's reply to `body` stops at the model's refusal, with no text: each reply of
 *  the written-case reader for a context session whose first message is REFUSED_REQUEST, and the
 *  first of each READER_TRIES for one whose first message is PARTLY_REFUSED_REQUEST. */
function refusedReply(body: Parameters<typeof readerAnswer>[0]): boolean {
  if (writtenRequestFor(body, REFUSED_REQUEST)) return true;
  if (!writtenRequestFor(body, PARTLY_REFUSED_REQUEST)) return false;
  partlyRefusedReplies += 1;
  return partlyRefusedReplies % READER_TRIES === 1;
}

let recorder: Recorder;

/** A made-up skill's SKILL.md. */
const skillMd = (name: string) =>
  `---\nname: ${name}\ndescription: Use when a task asks for ${name}.\n---\n\n# ${name}\n`;
/** A made-up project skill: one of a repository's own `.claude/skills/` or `.omp/skills/`. */
const projectSkillMd = (name: string) =>
  `---\nname: ${name}\ndescription: Use when the project asks for ${name}.\n---\n\n# ${name} of the project\n`;
/** The root CLAUDE.md of acme/proj: the project context file of a dev-box session in it. */
const PROJECT_RULES = "# Made-up project rules\n\nName each file you change.\n";

/** Claude Code's user memory in acme/skills's `.claude/`: the user-level context file of a devbox
 *  session whose units come from acme/skills. */
const USER_RULES = "# Made-up user rules\n\nAnswer each question in one line.\n";

/** How Oh My Pi picks and orders a session's project context files, as a profile's evidence says
 *  it. */
const SLOT_RULE =
  "Oh My Pi 18.18.6 keeps one project context file at each level from the working directory up to the checkout's root (capability/context-file.ts, lines 36-46: one for each depth), the first in its providers' order, highest priority first (capability/index.ts, lines 177-184 and 285-333), and joins the files it keeps the farthest level first (system-prompt.ts, lines 392-398)";
/** How a dev-box session's repository is found from its working directory, as a profile's
 *  evidence says it. */
const CHECKOUT_RULE =
  "a dev-box session's repository is the dev box's own repository (DEVBOX_INSTALLERS_RULE) where the working directory is in its checkout, the directory whose .claude/ the configuration's devboxClaudeConfigDir names as the dev box's CLAUDE_CONFIG_DIR; else the one repository of the copies output's list (repositories.tsv) named as the last part of its working directory, or of the nearest of that directory's parents named as any listed repository when that repository's tree at its commit holds the working directory's path below that parent: a parent's name alone decides nothing; where the tree does not hold that path and the working directory is directly inside that parent, the working directory is a workspace of that repository (a jj or git worktree) and the checkout's root when a file the session read by an absolute path under the working directory is a file of the tree at its path below the working directory";
/** The extension of a dev box's repository that feeds each pool its skills-sources.json names to
 *  Oh My Pi through resources_discover. */
const POOLS_EXTENSION = [
  'import * as fs from "node:fs";',
  "export default function (pi) {",
  '  pi.on("resources_discover", async () => ({',
  '    skillPaths: JSON.parse(fs.readFileSync("skills-sources.json", "utf8")).sources.map(',
  '      (source) => source.root + "/skills"',
  "    ),",
  "  }));",
  "}",
  "",
].join("\n");
/** The made-up repositories as each was at the commit SHA: every file's path and text. acme/skills
 *  holds `older` and no `newer`; its installer links each skill of skills/ into Oh My Pi's user
 *  skills, and its prepack script stages alpha and beta for the package @acme/pi-acme; it holds
 *  Claude Code's settings and user memory in `.claude/`, as a dev box's config repository does.
 *  acme/other holds its own alpha and delta, which its installer links the same way. acme/market
 *  holds zeta and a marketplace that ships it in its plugin zeta-tools, under the marketplace's
 *  name at SHA (old-market; the copies output, read later, names it new-market), and Claude
 *  Code's settings. acme/widgets holds an AGENTS.md. acme/proj holds its own skills: kappa and
 *  beta in `.claude/skills/`, lambda and older in `.omp/skills/` (beta and older are names
 *  acme/skills's installer links too), kappa and mu in `agents/.claude/skills/`, nu in
 *  `agents/agentvm/.omp/skills/` and xi in `other/.claude/skills/`; a CLAUDE.md, and an AGENTS.md
 *  in agents/agentvm/. acme/dual holds .claude/CLAUDE.md, AGENTS.md and CLAUDE.md at its root.
 *  acme/big holds its own skill omega in `.claude/skills/` and an AGENTS.md, and the stand-in gh
 *  cuts its whole tree short (TRUNCATED), as GitHub cuts the tree of a repository of too many
 *  files. acme/dots is made in the shape of a dotfiles repository: its Oh My Pi plugins
 *  package.json installs acme/powers as a `github:` dependency named acme-powers, and its installer
 *  clones acme/pack into vendor/acme-pack at PACK_PIN, a pool its skills-sources.json feeds to Oh
 *  My Pi through its extension. acme/powers holds a package whose `pi` manifest declares its
 *  skills/ (power-one and power-two); acme/pack holds pack-one and pack-later at SHA, and pack-one
 *  and pack-two at PACK_PIN (PINNED). acme/fresh and acme/newproj got their first commit at
 *  FRESH_MADE: acme/fresh holds fresh-one, which a dev box's installer installs from then on, and
 *  acme/newproj holds its own skill nova in `.claude/skills/` and a CLAUDE.md. acme/alpha-set is a
 *  skill set's repository: a package named alpha-set whose `pi` manifest declares its skills/,
 *  which hold its own alpha beside acme/skills's (setTarget). acme/work holds a README.md alone:
 *  F1's working directory is its checkout where the written builds' copies output lists it. */
const INSTALLER = [
  "#!/usr/bin/env bash",
  'for skill in "$REPO"/skills/*/; do',
  '  ln -sfn "$skill" "$HOME/.omp/agent/skills/$(basename "$skill")"',
  "done",
  "",
].join("\n");
/** The mark of a made-up file that the stand-in gh gives as a symbolic link (mode 120000): the
 *  rest of its text is the link's target. */
const LINK_MARK = "\u0000link:";
const link = (target: string) => `${LINK_MARK}${target}`;

const REPOSITORIES: Record<string, Record<string, string>> = {
  // acme/infra is the deployment of the suite's Legion project ACME: its project file alone, so
  // a pod installs the worker image's packages alone. acme/deploy is a deployment in full, which
  // the Legion install test names in place of acme/infra: its project file leaves beta-tools of
  // the catalog out, its role setup installs plugins of the catalog acme/catalog, its program
  // names every-tools as the plugin every role installs, and its worker image installs memo of
  // the release v1.0.0 of acme/rel as an Oh My Pi plugin under /opt/memo.
  "acme/infra": { "legion/projects/10-ACME.yaml": "key: ACME\ninstructions: acme.md\n" },
  "acme/deploy": {
    "legion/projects/10-ACME.yaml": `key: ACME\ninstructions: acme.md\nimage: registry.example/acme/worker@sha256:${"3".repeat(64)}\nverificationSkillsLeaveOut: [beta-tools]\n`,
    "legion/acme-setup.sh": [
      "#!/bin/sh",
      "repo=acme/catalog",
      'omp plugin marketplace add "$repo"',
      'omp plugin install "$p@$catalog" --force',
      "",
    ].join("\n"),
    "legion/main.go": 'package main\n\nvar everyRolePlugins = []string{"every-tools"}\n',
    "legion/worker/Dockerfile": [
      "FROM scratch",
      "ARG MEMO_RELEASE_URL=https://github.com/acme/rel/releases/download",
      "ARG MEMO_VERSION=1.0.0",
      'RUN release="${MEMO_RELEASE_URL}/v${MEMO_VERSION}"; \\',
      '    install -D -m 0644 "$t/skills/memo/SKILL.md" /opt/memo/skills/memo/SKILL.md; \\',
      '    printf \'{"name": "memo", "omp": {}}\\n\' > /opt/memo/package.json',
      "",
    ].join("\n"),
  },
  "acme/catalog": {
    ".claude-plugin/marketplace.json": JSON.stringify({
      name: "acme-catalog",
      plugins: ["every-tools", "alpha-tools", "beta-tools"].map((name) => ({
        name,
        source: `./plugins/${name}`,
      })),
    }),
    "plugins/every-tools/skills/every-one/SKILL.md": skillMd("every-one"),
    "plugins/alpha-tools/skills/alpha-one/SKILL.md": skillMd("alpha-one"),
    // A second alpha, whose text differs from acme/skills' alpha: Oh My Pi lists it as
    // alpha-tools/alpha beside the bare alpha the session read.
    "plugins/alpha-tools/skills/alpha/SKILL.md":
      "---\nname: alpha\ndescription: Use for the catalog's own alpha.\n---\n\n# alpha of the catalog\n",
    "plugins/beta-tools/skills/beta-one/SKILL.md": skillMd("beta-one"),
  },
  "acme/other": {
    "skills/alpha/SKILL.md": skillMd("alpha"),
    "skills/delta/SKILL.md": skillMd("delta"),
    "installers/skills.sh": INSTALLER,
  },
  // A dev box's own repository (it holds an installer and Claude Code's settings): its installer
  // links each skill of skills/ into Oh My Pi's user skills, its Oh My Pi config ignores dropped,
  // and quiet and shy hide themselves from the listing. No session reads any of them.
  "acme/home": {
    "installers/omp.sh": INSTALLER,
    // The dev box's plugin list installs @acme/pi-acme, a second install path of acme/skills'
    // alpha and beta; and acme/home's own alpha, of other text, is another source of the name.
    "omp/plugins/package.json": JSON.stringify({
      name: "omp-plugins",
      dependencies: { "@acme/pi-acme": "1.0.0" },
    }),
    "skills/alpha/SKILL.md":
      "---\nname: alpha\ndescription: Use for the home's own alpha.\n---\n\n# alpha of home\n",
    ".claude/settings.json": "{}\n",
    "omp/config.yml": "skills:\n  ignoredSkills:\n    - dropped\n",
    "skills/unread/SKILL.md": skillMd("unread"),
    "skills/dropped/SKILL.md": skillMd("dropped"),
    "skills/quiet/SKILL.md":
      "---\nname: quiet\ndescription: Use for quiet work.\ndisable-model-invocation: true\n---\n\n# quiet\n",
    // A description that holds `: `, which strict YAML refuses: Oh My Pi reads the block again with
    // it quoted, so `hide: true` is still true (common.ts's readFrontmatter).
    "skills/shy/SKILL.md":
      "---\nname: shy\ndescription: Use when: the work is shy.\nhide: true\n---\n\n# shy\n",
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
  "acme/widgets": { "AGENTS.md": "# Widgets\n" },
  "acme/proj": {
    ".claude/skills/kappa/SKILL.md": projectSkillMd("kappa"),
    ".claude/skills/beta/SKILL.md": projectSkillMd("beta"),
    ".omp/skills/lambda/SKILL.md": projectSkillMd("lambda"),
    ".omp/skills/older/SKILL.md": projectSkillMd("older"),
    "agents/.claude/skills/kappa/SKILL.md": projectSkillMd("kappa"),
    "agents/.claude/skills/mu/SKILL.md": projectSkillMd("mu"),
    "agents/agentvm/.omp/skills/nu/SKILL.md": projectSkillMd("nu"),
    "other/.claude/skills/xi/SKILL.md": projectSkillMd("xi"),
    "agents/agentvm/AGENTS.md": "# Made-up VM rules\n\nStart the VM before each test.\n",
    "CLAUDE.md": PROJECT_RULES,
  },
  "acme/dual": {
    ".claude/CLAUDE.md": "# Made-up rules for Claude\n\nAnswer in one line.\n",
    "AGENTS.md": "# Made-up agent rules\n\nName each file you read.\n",
    "CLAUDE.md": "# Made-up project rules\n\nRun the tests first.\n",
  },
  "acme/big": {
    ".claude/skills/omega/SKILL.md": projectSkillMd("omega"),
    ".claude/skills/omega/references/notes.md": "Omega notes.\n",
    "AGENTS.md": "# Made-up rules of a large repository\n\nRead the index first.\n",
    "data/runs/0001.json": "{}\n",
    "data/runs/0002.json": "{}\n",
  },
  "acme/powers": {
    "package.json": JSON.stringify({ name: "powers-of-acme", pi: { skills: ["./skills"] } }),
    "skills/power-one/SKILL.md": skillMd("power-one"),
    "skills/power-two/SKILL.md": skillMd("power-two"),
    // A pack-two of other text than acme/pack's: the pool's copy keeps the bare name, since Oh My
    // Pi admits a pool an extension names before an extension package's skill, though the session
    // read the package's power-one first.
    "skills/pack-two/SKILL.md":
      "---\nname: pack-two\ndescription: Use for the powers' own pack-two.\n---\n\n# pack-two of powers\n",
  },
  "acme/dots": {
    "omp/plugins/package.json": JSON.stringify({
      name: "omp-plugins",
      dependencies: {
        "acme-powers": "github:acme/powers#v1.2.0",
        // Packages the dev box pins by npm versions, and a clone that is one skill at its root.
        "@acme/kit": "2.0.0",
        "@acme/gone": "3.0.0",
        "acme-one": "github:acme/one-skill#v1",
      },
    }),
    "skills-sources.json": JSON.stringify({ sources: [{ root: "vendor/acme-pack" }] }),
    // The Oh My Pi build the dev box pins (the configuration's ompPin names this file and key).
    "mise.toml": '[tools]\n"acme:omp" = "18.13.3-acme.1"  # the dev box\'s Oh My Pi\n',
    "omp/extensions/skill-pools.ts": POOLS_EXTENSION,
    "installers/omp.sh": [
      "#!/usr/bin/env bash",
      'REPO_DIR="$(cd "$(dirname "$0")/.." && pwd)"',
      'ln -sfn "$REPO_DIR/omp/extensions/skill-pools.ts" "$HOME/.omp/agent/extensions/skill-pools.ts"',
      `ensure_vendor https://github.com/acme/pack.git acme-pack ${PACK_PIN}`,
      "",
    ].join("\n"),
  },
  "acme/pack": {
    "skills/pack-one/SKILL.md": skillMd("pack-one"),
    "skills/pack-later/SKILL.md": skillMd("pack-later"),
  },
  "acme/alpha-set": {
    "package.json": JSON.stringify({ name: "alpha-set", pi: { skills: ["./skills"] } }),
    "skills/alpha/SKILL.md": skillMd("alpha"),
    "skills/alpha/references/x.md": "Step notes.\n",
  },
  // A second dev box's repository in acme/dots's shape, for the twin-pool subagents: its
  // installer clones acme/twin-pack into vendor/twin-pack, a pool its skills-sources.json feeds to
  // Oh My Pi through its extension, and its plugin list installs acme/twin-powers as twin-powers.
  "acme/twin-dots": {
    "omp/plugins/package.json": JSON.stringify({
      name: "omp-plugins",
      dependencies: { "twin-powers": "github:acme/twin-powers#v1.0.0" },
    }),
    "skills-sources.json": JSON.stringify({ sources: [{ root: "vendor/twin-pack" }] }),
    "omp/extensions/skill-pools.ts": POOLS_EXTENSION,
    "installers/omp.sh": [
      "#!/usr/bin/env bash",
      'REPO_DIR="$(cd "$(dirname "$0")/.." && pwd)"',
      'ln -sfn "$REPO_DIR/omp/extensions/skill-pools.ts" "$HOME/.omp/agent/extensions/skill-pools.ts"',
      "ensure_vendor https://github.com/acme/twin-pack.git twin-pack v1.0.0",
      "",
    ].join("\n"),
  },
  "acme/twin-pack": {
    "skills/pack-one/SKILL.md": skillMd("pack-one"),
    "skills/pack-two/SKILL.md": skillMd("pack-two"),
    "skills/twin-only/SKILL.md": skillMd("twin-only"),
  },
  // A package of pack-one and pack-two of other text than the pool's, and power-solo.
  "acme/twin-powers": {
    "package.json": JSON.stringify({ name: "twin-powers", pi: { skills: ["./skills"] } }),
    "skills/pack-one/SKILL.md":
      "---\nname: pack-one\ndescription: Use for the twin powers' own pack-one.\n---\n\n# pack-one of twin-powers\n",
    "skills/pack-two/SKILL.md":
      "---\nname: pack-two\ndescription: Use for the twin powers' own pack-two.\n---\n\n# pack-two of twin-powers\n",
    "skills/power-solo/SKILL.md": skillMd("power-solo"),
  },
  // A checkout whose Claude Code settings turn on the plugin twin-tools of the marketplace
  // acme-twins (acme/twins), which ships pack-one and pack-two of the pool's text, and a
  // power-solo of other text than the package's.
  "acme/twin": {
    ".claude/settings.json": JSON.stringify({ enabledPlugins: { "twin-tools@acme-twins": true } }),
  },
  "acme/twins": {
    ".claude-plugin/marketplace.json": JSON.stringify({
      name: "acme-twins",
      plugins: [{ name: "twin-tools", source: "./plugins/twin-tools" }],
    }),
    "plugins/twin-tools/skills/pack-one/SKILL.md": skillMd("pack-one"),
    "plugins/twin-tools/skills/pack-two/SKILL.md": skillMd("pack-two"),
    "plugins/twin-tools/skills/power-solo/SKILL.md":
      "---\nname: power-solo\ndescription: Use for the twin tools' own power-solo.\n---\n\n# power-solo of twin-tools\n",
  },
  // Two packages: kit tags each release; gone tags none, and the registry's tarball of gone
  // 3.0.0 ships gone-one alone.
  "acme/kit": {
    "packages/kit/package.json": JSON.stringify({ name: "@acme/kit", omp: { skills: ["skills"] } }),
    "packages/kit/skills/kit-one/SKILL.md": skillMd("kit-one"),
    "packages/kit/skills/kit-two/SKILL.md": skillMd("kit-two"),
    "packages/gone/package.json": JSON.stringify({
      name: "@acme/gone",
      omp: { skills: ["skills"] },
    }),
    "packages/gone/skills/gone-one/SKILL.md": skillMd("gone-one"),
    "packages/gone/skills/gone-two/SKILL.md": skillMd("gone-two"),
  },
  // A repository that is one skill, at its root.
  "acme/one-skill": {
    "package.json": JSON.stringify({ name: "acme-one", pi: { skills: ["."] } }),
    "SKILL.md": skillMd("one-skill"),
  },
  "acme/fresh": { "skills/fresh-one/SKILL.md": skillMd("fresh-one") },
  "acme/newproj": {
    ".claude/skills/nova/SKILL.md": projectSkillMd("nova"),
    "CLAUDE.md": "# Made-up rules of a new project\n\nKeep each change small.\n",
  },
  // A repository whose project skills are in each directory Oh My Pi 18.18.6 reads beside
  // .omp/skills and .claude/skills: upsilon in .agent/skills, rho in .agents/skills, sigma in
  // .github/skills, and plain there with no description, which Oh My Pi does not list there;
  // and two links in .claude/skills: tau to skills/tau in the repository, and away to a
  // directory outside it.
  "acme/agents": {
    ".agent/skills/upsilon/SKILL.md": projectSkillMd("upsilon"),
    ".agents/skills/rho/SKILL.md": projectSkillMd("rho"),
    ".github/skills/sigma/SKILL.md": projectSkillMd("sigma"),
    ".github/skills/plain/SKILL.md": "---\nname: plain\n---\n\n# plain\n",
    ".claude/skills/tau": link("../../skills/tau"),
    ".claude/skills/away": link("../../../outside/away"),
    "skills/tau/SKILL.md": projectSkillMd("tau"),
  },
  // A project whose Claude Code settings turn on the plugin plug-tools of the marketplace
  // acme-plugs (acme/plugs), and one whose settings turn on none. plug-tools ships plug-one, a
  // beta of other text than acme/skills' and an alpha of the same text.
  "acme/plug": {
    ".claude/settings.json": JSON.stringify({ enabledPlugins: { "plug-tools@acme-plugs": true } }),
  },
  "acme/unplug": { ".claude/settings.json": "{}\n" },
  "acme/plugs": {
    ".claude-plugin/marketplace.json": JSON.stringify({
      name: "acme-plugs",
      plugins: [{ name: "plug-tools", source: "./plugins/plug-tools" }],
    }),
    "plugins/plug-tools/skills/plug-one/SKILL.md": skillMd("plug-one"),
    "plugins/plug-tools/skills/beta/SKILL.md":
      "---\nname: beta\ndescription: Use for the plug's own beta.\n---\n\n# beta of plug-tools\n",
    "plugins/plug-tools/skills/alpha/SKILL.md": skillMd("alpha"),
  },
  // The repository whose checkout F1's working directory (F1_CWD, named work) is where the
  // written builds' copies output lists it (writtenCopies): a file no profile reads.
  "acme/work": { "README.md": "# Made-up work\n\nThe fixture project's notes.\n" },
};

/** The ref the copies output read acme/pack at, later than the sessions (repositories.tsv). */
const PACK_LATER = "5".repeat(40);

/** The made-up repositories at a commit other than SHA, by `<repository>@<ref>`: the commit the
 *  stand-in gh gives for that ref, and the files there. acme/dots's installer pins acme/pack at
 *  PACK_PIN; the copies output read it later at another ref (repositories.tsv's branch), which
 *  the stand-in answers with SHA. */
const PINNED: Record<string, { sha: string; files: Record<string, string> }> = {
  [`acme/pack@${PACK_PIN}`]: {
    sha: PACK_PIN,
    files: {
      "skills/pack-one/SKILL.md": skillMd("pack-one"),
      "skills/pack-two/SKILL.md": skillMd("pack-two"),
    },
  },
  "acme/rel@v1.0.0": { sha: REL_PIN, files: { "skills/memo/SKILL.md": skillMd("memo") } },
  [`acme/powers@${POWERS_OLD}`]: {
    sha: POWERS_OLD,
    files: {
      "package.json": JSON.stringify({ name: "powers-of-acme", pi: { skills: ["./skills"] } }),
      "skills/power-one/SKILL.md": skillMd("power-one"),
    },
  },
  [`acme/kit@${KIT_V1}`]: {
    sha: KIT_V1,
    files: {
      "packages/kit/package.json": JSON.stringify({
        name: "@acme/kit",
        omp: { skills: ["skills"] },
      }),
      "packages/kit/skills/kit-one/SKILL.md": skillMd("kit-one"),
    },
  },
  "acme/kit@kit-v1.0.0": { sha: KIT_V1, files: {} },
  [`acme/dots@${DOTS_OLD}`]: {
    sha: DOTS_OLD,
    files: {
      ...Object.fromEntries(
        Object.entries(REPOSITORIES["acme/dots"] ?? {}).filter(
          ([file]) => file !== "skills-sources.json"
        )
      ),
      "omp/plugins/package.json": JSON.stringify({
        name: "omp-plugins",
        dependencies: {
          "acme-powers": "github:acme/powers#v1.2.0",
          "@acme/kit": "1.0.0",
          "@acme/gone": "3.0.0",
          "acme-one": "github:acme/one-skill#v1",
        },
      }),
    },
  },
  // acme/skills as a made-up worker image of ACME carries it (IMAGE_SHA, also its tag
  // acme-image-v2.0.0): its prepack stages omicron too.
  [`acme/skills@${IMAGE_SHA}`]: {
    sha: IMAGE_SHA,
    files: {
      ...Object.fromEntries(
        Object.entries(REPOSITORIES["acme/skills"] ?? {}).filter(
          ([file]) => file !== "scripts/pi-plugin-prepack.sh"
        )
      ),
      "skills/omicron/SKILL.md": skillMd("omicron"),
      "scripts/pi-plugin-prepack.sh": (
        REPOSITORIES["acme/skills"]?.["scripts/pi-plugin-prepack.sh"] ?? ""
      ).replace("skills=( alpha beta )", "skills=( alpha beta omicron )"),
    },
  },
  "acme/skills@acme-image-v2.0.0": { sha: IMAGE_SHA, files: {} },
  [`acme/skills@${IMAGE_WORD}`]: { sha: IMAGE_SHA, files: {} },
};

/** A gzipped tar of `files`, each under `package/`, as npm packs a package. */
function tgz(files: Record<string, string>): Uint8Array {
  const encode = (text: string) => new TextEncoder().encode(text);
  const blocks: Uint8Array[] = [];
  for (const [name, text] of Object.entries(files)) {
    const body = encode(text);
    const header = new Uint8Array(512);
    header.set(encode(`package/${name}`), 0);
    header.set(encode(`${body.length.toString(8).padStart(11, "0")}\0`), 124);
    header.set(encode("0"), 156);
    blocks.push(header, body, new Uint8Array((512 - (body.length % 512)) % 512));
  }
  blocks.push(new Uint8Array(1024));
  return Bun.gzipSync(Buffer.concat(blocks));
}

/** The registry's tarballs of the made-up packages, by `<name>@<version>`: gone 3.0.0 ships
 *  gone-one alone. */
const TARBALLS: Readonly<Record<string, () => Uint8Array>> = {
  "@acme/gone@3.0.0": () =>
    tgz({
      "package.json": JSON.stringify({ name: "@acme/gone", omp: { skills: ["skills"] } }),
      "skills/gone-one/SKILL.md": skillMd("gone-one"),
    }),
};
/** The files of `repository` at the commit `id`: SHA's, or a PINNED commit's. */
const filesAt = (repository: string, id: string): Record<string, string> | undefined =>
  id === SHA
    ? REPOSITORIES[repository]
    : Object.entries(PINNED).find(
        ([key, at]) => key.startsWith(`${repository}@`) && at.sha === id
      )?.[1].files;

/** The blob id the stand-in gives a file's text. */
const blobId = (text: string) => createHash("sha1").update(text).digest("hex");
/** A made-up file's mode and blob text: a link's (LINK_MARK) is 120000 and its target. */
const linkMode = (text: string) => (text.startsWith(LINK_MARK) ? "120000" : "100644");
const blobText = (text: string) =>
  text.startsWith(LINK_MARK) ? text.slice(LINK_MARK.length) : text;
/** The one file a batch of blobs gives cut short, so it is read alone. */
const CUT_SHORT = REPOSITORIES["acme/skills"]?.["docker/omp-plugins/package.json"] ?? "";
/** The made-up repositories whose whole tree the stand-in gh gives cut short (`truncated`). */
const TRUNCATED: Readonly<Record<string, true>> = { "acme/big": true };

/** The id the stand-in gives the tree of the directory `dir` of `repo` at SHA: SHA for its root,
 *  since GitHub takes a commit's id for its tree's, else one made from the two. */
const treeId = (repo: string, dir: string) =>
  dir === "" ? SHA : createHash("sha1").update(`tree ${repo} ${dir}`).digest("hex");

/** The entries of the directory of `repo` whose tree id is `id`, one level, as GitHub's tree read
 *  without `recursive` gives them (a subdirectory as an entry of type `tree`); undefined when no
 *  directory of `repo` has that id. */
function treeLevel(repo: string, id: string): Record<string, unknown>[] | undefined {
  const files = Object.entries(REPOSITORIES[repo] ?? {});
  const dirs = new Set(files.length === 0 ? [] : [""]);
  for (const [file] of files) {
    const parts = file.split("/");
    for (let n = 1; n < parts.length; n += 1) dirs.add(parts.slice(0, n).join("/"));
  }
  const dir = [...dirs].find((each) => treeId(repo, each) === id);
  if (dir === undefined) return undefined;
  const prefix = dir === "" ? "" : `${dir}/`;
  const entries = new Map<string, Record<string, unknown>>();
  for (const [file, text] of files) {
    if (!file.startsWith(prefix)) continue;
    const [name = "", ...rest] = file.slice(prefix.length).split("/");
    entries.set(
      name,
      rest.length === 0
        ? { path: name, mode: linkMode(text), type: "blob", sha: blobId(text), size: text.length }
        : { path: name, mode: "040000", type: "tree", sha: treeId(repo, `${prefix}${name}`) }
    );
  }
  return [...entries.values()].sort((a, b) => (String(a.path) < String(b.path) ? -1 : 1));
}

/** The gh calls the stand-in took. */
const ghCalls: string[][] = [];

/** The peak resident memory every build of the suite reads unless a test gives its own: under the
 *  1 GiB at which build-cases stops. */
const PEAK_BYTES = 200 * 1024 ** 2;
/** A peak resident memory above that 1 GiB. */
const OVER_LIMIT = 1024 ** 3 + 1;

/** The stand-in gh: GET reads of commits (SHA, or a PINNED ref's commit), default branches,
 *  acme/infra's legion/projects/10-ACME.yaml, each file of a made-up repository at SHA, the tree
 *  of each made-up repository at SHA or at a PINNED commit (whole, cut short for TRUNCATED's, or
 *  one directory a read by its tree id) and its blobs; and GraphQL reads of blobs in a batch. The
 *  clock is the real one, and the peak resident memory is PEAK_BYTES. */
const io = (lines: string[]): BuildIo => ({
  out: (text) => lines.push(text),
  err: (line) => lines.push(line),
  npm: async (name, version) => TARBALLS[`${name}@${version}`]?.(),
  // The suite's worker images carry the label of the commit its repositories are read at (SHA),
  // since an image no source dates is left out and never read at its repository's default branch.
  imageConfig: async () => ({
    env: [],
    labels: { "org.opencontainers.image.revision": SHA },
    diffIds: [],
  }),
  gh: async (args) => {
    ghCalls.push([...args]);
    const route = args.find((arg) => arg.startsWith("repos/")) ?? "";
    const ok = (stdout: string) => ({ code: 0, stdout, stderr: "" });
    if (args[1] === "graphql") {
      const field = (name: string) =>
        args.find((arg) => arg.startsWith(`${name}=`))?.slice(name.length + 1) ?? "";
      const owner = `${field("owner")}/${field("name")}`;
      const texts = [
        ...Object.values(REPOSITORIES[owner] ?? {}),
        ...Object.entries(PINNED).flatMap(([key, at]) =>
          key.startsWith(`${owner}@`) ? Object.values(at.files) : []
        ),
      ];
      const blobs = [...field("query").matchAll(/(b\d+): object\(oid: "([0-9a-f]{40})"\)/g)].map(
        ([, alias, id]) => {
          const text = texts.find((each) => blobId(each) === id);
          if (text === undefined) return [alias, null];
          const cut = text === CUT_SHORT;
          return [alias, { isBinary: false, isTruncated: cut, text: cut ? null : blobText(text) }];
        }
      );
      return ok(JSON.stringify({ data: { repository: Object.fromEntries(blobs) } }));
    }
    if (/^repos\/[^/]+\/[^/]+$/.test(route)) return ok("main\n");
    const tags = /^repos\/([^/]+\/[^/]+)\/git\/matching-refs\/tags\/(.+)$/.exec(route);
    if (tags !== null) {
      const start = decodeURIComponent(tags[2] ?? "");
      const listed = (TAGS[tags[1] ?? ""] ?? []).filter((tag) => tag.startsWith(start));
      return ok(listed.map((tag) => `refs/tags/${tag}\n`).join(""));
    }
    const commits = /^(repos\/[^/]+\/[^/]+)\/commits\?(.*)$/.exec(route);
    if (commits !== null) {
      const query = new URLSearchParams(commits[2]);
      const ref = query.get("sha");
      const repo = (commits[1] ?? "").slice("repos/".length);
      // acme/pack's later ref, the one repositories.tsv names, was made after the sessions'
      // start: GitHub gives no commit of it at or before then, and `--jq .[0].sha` prints null.
      if (repo === "acme/pack" && ref === PACK_LATER) return ok("null\n");
      // acme/powers was made at POWERS_MADE and had power-one alone (POWERS_OLD) until POWERS_AT.
      const until = Date.parse(query.get("until") ?? "");
      if (repo === "acme/powers" && until < Date.parse(POWERS_MADE)) return ok("null\n");
      if (repo === "acme/powers" && until < Date.parse(POWERS_AT)) return ok(`${POWERS_OLD}\n`);
      if (repo === "acme/dots" && until < Date.parse(POWERS_AT)) return ok(`${DOTS_OLD}\n`);
      // acme/fresh and acme/newproj have no commit before FRESH_MADE.
      const fresh = repo === "acme/fresh" || repo === "acme/newproj";
      if (fresh && until < Date.parse(FRESH_MADE)) return ok("null\n");
      return ok(`${PINNED[`${repo}@${ref}`]?.sha ?? SHA}\n`);
    }
    if (route === "repos/acme/infra/contents/legion/projects/10-ACME.yaml") {
      // The image is the one whose labels `io` gives (the revision SHA): a deployment that names
      // none gives no commit of its repository to read it at.
      return ok(
        `key: ACME\ninstructions: acme.md\nimage: registry.example/acme/worker@sha256:${"3".repeat(64)}\n`
      );
    }
    const [, owned = "", file = ""] = /^repos\/([^/]+\/[^/]+)\/contents\/(.+)$/.exec(route) ?? [];
    const text = REPOSITORIES[owned]?.[decodeURIComponent(file)];
    if (text !== undefined && args.includes(`ref=${SHA}`)) return ok(text);
    const [, repository = "", kind = "", id = ""] =
      /^repos\/([^/]+\/[^/]+)\/git\/(trees|blobs)\/([0-9a-f]{40})(?:\?recursive=1)?$/.exec(route) ??
      [];
    const files = Object.entries(filesAt(repository, id) ?? REPOSITORIES[repository] ?? {});
    if (kind === "trees" && !route.endsWith("?recursive=1")) {
      const level = treeLevel(repository, id);
      if (level !== undefined)
        return ok(JSON.stringify({ sha: id, truncated: false, tree: level }));
    }
    if (kind === "trees" && filesAt(repository, id) !== undefined && files.length > 0) {
      const cut = Object.hasOwn(TRUNCATED, repository);
      return ok(
        JSON.stringify({
          sha: id,
          truncated: cut,
          tree: files.slice(0, cut ? 1 : files.length).map(([file, text]) => ({
            path: file,
            mode: linkMode(text),
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
        JSON.stringify({
          encoding: "base64",
          content: Buffer.from(blobText(blob[1])).toString("base64"),
        })
      );
    }
    return { code: 1, stdout: "", stderr: "gh: Not Found (HTTP 404)\n" };
  },
  now: () => performance.now(),
  peakRssBytes: () => PEAK_BYTES,
});

const setupDir = path.join(scratch, "setup");
const config = path.join(setupDir, "skill-cuts.json");
const keyFile = path.join(setupDir, "key");
const old = path.join(setupDir, "old", "alpha");
/** alpha's directory at the cut's head (`--new`): the old text less LINE, and NEW_WIDE. */
const newDir = path.join(setupDir, "new", "alpha");
const copiesDir = path.join(setupDir, "copies");
const countDir = path.join(setupDir, "count");
const psqlLog = path.join(setupDir, "psql.log");

beforeAll(() => {
  recorder = startRecorder({
    route: (request: RecordedRequest) => {
      if (request.method !== "POST" || request.path !== "/v1/messages") return undefined;
      const body = JSON.parse(request.body);
      if (writtenRequestFor(body, ERRED_REQUEST)) {
        return Response.json(
          {
            type: "error",
            error: { type: "invalid_request_error", message: "the stand-in takes no such request" },
          },
          { status: 400 }
        );
      }
      const refused = refusedReply(body);
      const reply = messageSse({
        id: `msg_standin_${request.n}`,
        model: body.model,
        content: refused ? [] : [{ type: "text", text: readerAnswer(body) }],
        stopReason: "end_turn",
        inputTokens: 100,
        outputTokens: 20,
      });
      // A refused reply's stop, which recorder.ts's messageSse names none of, in its end_turn's place.
      return sseResponse(
        refused ? reply.replace('"stop_reason":"end_turn"', '"stop_reason":"refusal"') : reply,
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
  mkdirSync(path.join(newDir, "references"), { recursive: true });
  writeFileSync(
    path.join(newDir, "SKILL.md"),
    readFileSync(path.join(old, "SKILL.md"), "utf8").replace(`${LINE}\n`, "")
  );
  writeFileSync(
    path.join(newDir, "references", "x.md"),
    readFileSync(path.join(old, "references", "x.md"))
  );
  writeFileSync(path.join(newDir, NEW_WIDE.path), NEW_WIDE.text);

  const files: Record<string, Record<string, string>> = { store_a: {}, store_b: {}, store_c: {} };
  for (const [name, session] of Object.entries(SESSIONS)) {
    const store = files[session.store] ?? {};
    store[sessionPath(name)] = sessionText(name);
  }
  for (const name of [...Object.keys(BY_PATH), ...Object.keys(MATERIAL)]) {
    const store = files[BY_PATH[name]?.store ?? "store_a"] ?? {};
    store[sessionPath(name)] = sessionText(name);
  }
  // A subagent the good session started, one level below its directory, whose recorded prompt
  // lists alpha, beta and older, the names the good case's rebuilt profile lists: the good case's
  // listing is measured against it, and adds no name (score.ts's listing-adds).
  const goodStore = files[SESSIONS.good?.store ?? "store_a"] ?? {};
  goodStore[`${sessionPath("good").slice(0, -".jsonl".length)}/Helper.jsonl`] = sessionText(
    "subagent"
  ).replace(
    "- beta: Use for the beta greeting.\\n",
    "- beta: Use for the beta greeting.\\n- older: Use when a task asks for older.\\n"
  );
  // pinned-subagent's root: its prompt builds' digest lines, the last before the subagent's start
  // at ROOT_BUILD.
  goodStore[`${PINNED_ROOT}.jsonl`] = ROOT_DIGESTS.map(
    (at, n) =>
      `${JSON.stringify({ type: "custom", customType: "system-prompt-digest", data: `d1${n}`, id: `d0${n}`, parentId: n === 0 ? null : `d0${n - 1}`, timestamp: at })}\n`
  ).join("");
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
    [`${DEV_SESSIONS}/task-1.jsonl`]: unlistedSubagentText(),
    [`${DEV_SESSIONS}/task-3.jsonl`]: sessionText("openai"),
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
  const powers = "package:acme-powers (acme/powers:package.json)";
  const pack = "pool:vendor/acme-pack (acme/dots:skills-sources.json)";
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
      // As copies.ts gives them: acme/powers' package under its dependency's name, and the pool
      // of acme/dots's clone of acme/pack at the later ref the copies output read it at.
      ...["power-one", "power-two"].map((name) => [
        `**/node_modules/acme-powers/skills/${name}/`,
        "acme/powers",
        `skills/${name}`,
        name,
        powers,
      ]),
      ...["pack-one", "pack-later"].map((name) => [
        `**/vendor/acme-pack/skills/${name}/`,
        "acme/pack",
        `skills/${name}`,
        name,
        pack,
      ]),
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
      ...["power-one", "power-two"].map((name) =>
        installer(
          "acme/powers",
          "package.json",
          "omp-package",
          "ships",
          "package:acme-powers",
          name,
          `skills/${name}`
        )
      ),
      ...["pack-one", "pack-later"].map((name) => [
        "acme/dots",
        "skills-sources.json",
        "omp-skill-sources",
        "installs",
        "pool:vendor/acme-pack",
        "omp",
        name,
        "acme/pack",
        `skills/${name}`,
        "read by omp/extensions/skill-pools.ts",
      ]),
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
      ["acme/proj", "main", SHA, "no", "no", "", 10, "recursive", 8, ""],
      ["acme/dual", "main", SHA, "no", "no", "", 10, "recursive", 0, ""],
      ["acme/powers", "v1.2.0", SHA, "no", "no", "", 3, "whole", 2, ""],
      ["acme/dots", "main", SHA, "no", "no", "", 4, "whole", 0, ""],
      // Read later at another ref than PACK_PIN, made after the sessions' start.
      ["acme/pack", PACK_LATER, SHA, "no", "no", "", 2, "whole", 2, ""],
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
      ["acme/powers", "skills/power-one", "power-one", 10, "no", "no", "", "", ""],
      ["acme/powers", "skills/power-two", "power-two", 10, "no", "no", "", "", ""],
      ["acme/pack", "skills/pack-one", "pack-one", 10, "no", "no", "", "", ""],
      ["acme/pack", "skills/pack-later", "pack-later", 10, "no", "no", "", "", ""],
    ])
  );
  writeFileSync(path.join(copiesDir, "loaders.tsv"), tsv(LOADERS_COLUMNS, []));
  writeFileSync(path.join(copiesDir, "former-stages.tsv"), tsv(FORMER_STAGES_COLUMNS, []));

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
        calls: { "0123456789abcdef": ["0123456789abcdef"] },
        unpaired: 0,
        unpaired_hashes: [],
        unpaired_calls: {},
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
    writeFileSync(
      path.join(countDir, store, "roots.txt"),
      ` root\n--------\n ${JSON.stringify({ hash: "0123456789abcdef", profile: store === "store_b" ? "legion" : "default" })}\n(1 row)\n`
    );
  }
}, 60_000);
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
    const store = SESSIONS[name]?.store ?? BY_PATH[name]?.store ?? "store_a";
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
    "--new",
    newDir,
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
  stopped: unknown;
  peakRssBytes?: number;
}

/** The summary `build-cases` wrote beside the cases file `out`: a session build's, or `T`. */
const summaryOf = <T = Summary>(out: string): T =>
  JSON.parse(readFileSync(`${out}.summary.json`, "utf8"));

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
    // pinned-units and pinned-subagent, whose packages and pools are held at other commits than
    // SHA, have builds of their own below.
    const own = ["pinned-units", "pinned-subagent"];
    const names = [...Object.keys(SESSIONS).filter((name) => !own.includes(name)), "missing"];
    const sessions = sessionsFile("all.jsonl", names, ["good", "missing"]);
    const from = recorder.requests.length;
    code = await main(buildArgs(sessions, out, "7"), io(printed));
    requests = recorder.requests.slice(from);
    cases = loadCases(out, old);
  }, HOOK_BUILD_MS);

  test("each case's evidence holds its listing measure: a subagent's against its own recorded listing, a top-level session's against a subagent's of it, else not measurable; the summary counts them", () => {
    const measureOf = (name: string) => {
      const items =
        byName(name)?.evidence?.filter((each) => each.item === LISTING_MEASURE_ITEM) ?? [];
      expect(items).toHaveLength(1);
      return ListingMeasure.parse(JSON.parse(items[0]?.source ?? "{}"));
    };
    const listed = (name: string) => (byName(name)?.profile.skills ?? []).map((each) => each.name);
    // The subagent's recorded prompt lists alpha and beta; the measure sets its profile against it.
    const own = measureOf("subagent");
    const names = ["alpha", "beta"];
    expect(own).toMatchObject({
      kind: "session listing",
      recorded: 2,
      missing: names.filter((name) => !listed("subagent").includes(name)),
      added: listed("subagent")
        .filter((name) => !names.includes(name))
        .sort(),
      rebuiltDigest: null,
      subagent: null,
    });
    expect(own.missingShare).toBe(own.missing.length / 2);
    // The good session records only its digest; its subagent Helper's listing, which names older
    // as well, is used.
    const good = measureOf("good");
    const helperNames = [...names, "older"];
    expect(good).toMatchObject({
      kind: "subagent listing",
      recorded: 3,
      missing: helperNames.filter((name) => !listed("good").includes(name)),
      added: [],
      sessionDigest: byName("good")?.sessionDigest ?? null,
      rebuiltDigest: null,
      subagent: {
        pathHash: pathHash(`${sessionPath("good").slice(0, -".jsonl".length)}/Helper.jsonl`),
        agent: "task",
        subagents: 1,
        recorded: 1,
      },
    });
    expect(good.why).toContain("the build makes no prompt");
    // A top-level session with no subagent in its store is not measurable.
    const checkout = measureOf("checkout");
    expect(checkout).toMatchObject({ kind: "not measurable", recorded: null, missing: [] });
    expect(checkout.why).toContain("the store holds no subagent of it");
    const summary = JSON.parse(readFileSync(`${out}.summary.json`, "utf8")) as {
      listing: Record<string, number>;
    };
    expect(summary.listing["session listing"]).toBeGreaterThan(0);
    expect(summary.listing["subagent listing"]).toBe(1);
    expect(summary.listing["not measurable"]).toBeGreaterThan(0);
    expect(summary.listing.none).toBe(0);
    // The predicate the listing rule reads: a measure is of kind `session listing` exactly when
    // its case's profile holds the recorded system prompt that the case's capture sends whole
    // (capture.ts's `c.profile.systemPrompt`), whatever the case; so score.ts's
    // `listingStanding` reads the capture's own choice from the measure alone.
    expect(cases.length).toBeGreaterThan(1);
    for (const each of cases) {
      const item = each.evidence?.find((entry) => entry.item === LISTING_MEASURE_ITEM);
      const { kind } = ListingMeasure.parse(JSON.parse(item?.source ?? "{}"));
      expect([each.id, kind === "session listing"]).toEqual([
        each.id,
        each.profile.systemPrompt !== undefined,
      ]);
    }
  });

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
        "proj-legion",
        "proj-devbox",
        "two-repos",
        "proj-subdir",
        "dual-root",
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
    expect(c?.source).toEqual({
      store: "store_a",
      pathHash: pathHash(sessionPath("good")),
      kind: "session",
    });
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
      contextFiles: [{ repo: "acme/skills", commit: SHA, path: ".claude/CLAUDE.md", user: true }],
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
    const commitCall = ghCalls.flat().find((arg) => arg.startsWith("repos/acme/skills/commits?"));
    expect(commitCall).toContain("until=2026-11-20T01%3A59%3A41.780Z");
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

  test("--reprofile reads session heads, paths and builds in batches, changes only profile and evidence, and calls no reader", async () => {
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
    const logBefore = psqlLogLength();
    expect(await main(args(staleFile, fresh), io(lines))).toBe(0);
    expect(lines.join("")).not.toContain("left out");
    expect(recorder.requests.length).toBe(requestsBefore);
    expect(
      psqlLines(logBefore)
        .map((line) => `${line.db} ${line.kind}`)
        .sort()
    ).toEqual([
      // The prompt builds of each case's root, a subagent's and a top-level session's own, one
      // query a store (digest-times.sql).
      "store_a digests",
      "store_a heads",
      "store_a paths",
      "store_a subagents",
      "store_b digests",
      "store_b heads",
      "store_b paths",
      "store_b subagents",
    ]);

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
      // The session's head gives the working directory the builder read from the transcript's
      // header, so every source, each that names that directory among them, is the builder's.
      const differing = (c.evidence ?? []).filter(
        (each, at) => each.source !== built.evidence?.[at]?.source
      );
      expect(differing.map((each) => each.item)).toEqual([]);
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

  test("--reprofile takes each case's working directory from its session's head, so a case built before the session repository was recorded gets its project skills and root context file; a case whose session the store no longer holds keeps the old rule, and its evidence says so", async () => {
    const devbox = byName("proj-devbox") as Case;
    const legion = byName("proj-legion") as Case;
    // Each as a builder wrote it before the session repository: no project skill, no context file
    // of acme/proj, no session repository or working directory in its profile, and no item naming
    // the session repository.
    const before = (c: Case): Case => ({
      ...c,
      profile: {
        ...c.profile,
        skills: c.profile.skills.filter((skill) => skill.repo !== "acme/proj"),
        contextFiles: c.profile.contextFiles.filter((file) => file.repo !== "acme/proj"),
        sessionRepository: undefined,
        workingDirectory: undefined,
      },
      evidence: (c.evidence ?? []).filter((each) => !each.item.startsWith("session repository ")),
    });
    // A case whose session the store holds no more, its old evidence naming acme/proj.
    const gone: Case = {
      ...legion,
      id: `${legion.id}-gone`,
      source: { ...legion.source, pathHash: pathHash("/sessions/gone.jsonl") },
    };
    const from = path.join(scratch, "heads", "old.jsonl");
    mkdirSync(path.dirname(from), { recursive: true });
    writeFileSync(
      from,
      [before(devbox), before(legion), gone].map((c) => `${JSON.stringify(c)}\n`).join("")
    );
    const fresh = path.join(scratch, "heads", "reprofiled.jsonl");
    const logBefore = psqlLogLength();
    const lines: string[] = [];
    expect(
      await main(
        [
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
          fresh,
        ],
        io(lines)
      )
    ).toBe(0);
    const [d, l, g] = loadCases(fresh, old);
    // The header's working directory gives each its session repository again.
    expect(d?.profile).toEqual(devbox.profile);
    expect(l?.profile).toEqual(legion.profile);
    for (const c of [d, l]) {
      expect(c?.evidence?.map((each) => each.item)).toContain("session repository acme/proj");
      expect(c?.evidence?.map((each) => each.item)).not.toContain("no session head");
    }
    // The one the store no longer holds keeps the old rule: the repository its old evidence names.
    expect(g?.profile).toEqual(legion.profile);
    expect(g?.evidence?.find((each) => each.item === "no session head")?.source).toBe(
      "the store store_b holds no file of the session now (session-head.sql), so its working directory is not known: the session repository is the one the old evidence names"
    );
    // One query a store for each of the heads, paths, prompt builds and subagents.
    // Each head is saved beside the new case file.
    expect(
      psqlLines(logBefore)
        .map((line) => `${line.db} ${line.kind}`)
        .sort()
    ).toEqual([
      "store_a digests",
      "store_a heads",
      "store_a paths",
      "store_a subagents",
      "store_b digests",
      "store_b heads",
      "store_b paths",
      "store_b subagents",
    ]);
    const headFile = `${devbox.source.store}-${devbox.source.pathHash}.json`;
    expect(existsSync(path.join(`${fresh}.sessions`, headFile))).toBe(true);
    const summary = JSON.parse(readFileSync(`${fresh}.summary.json`, "utf8")) as {
      cases: { sessionHead: string }[];
    };
    expect(summary.cases.map((each) => each.sessionHead)).toEqual(["read", "read", "not held"]);
  }, 60_000);

  // Catches: a case whose session the store no longer holds rebuilt at its checkout's root, its
  // working directory lost while its evidence still names it.
  test("--reprofile of a case whose session the store no longer holds keeps the case's working directory below its checkout's root, and that level's project skills", async () => {
    const subdir = byName("proj-subdir") as Case;
    const gone: Case = {
      ...subdir,
      id: `${subdir.id}-gone`,
      source: { ...subdir.source, pathHash: pathHash("/sessions/gone-subdir.jsonl") },
    };
    const from = path.join(scratch, "heads-subdir", "old.jsonl");
    mkdirSync(path.dirname(from), { recursive: true });
    writeFileSync(from, `${JSON.stringify(gone)}\n`);
    const fresh = path.join(scratch, "heads-subdir", "reprofiled.jsonl");
    const args = ["build-cases", "--config", config, "--old", old, "--copies", copiesDir];
    const reprofile = ["--count", countDir, "--reprofile", from, "--out", fresh];
    expect(await main([...args, ...reprofile], io([]))).toBe(0);
    const [c] = loadCases(fresh, old);
    expect(c?.profile.workingDirectory).toBe("agents/agentvm");
    expect(c?.profile).toEqual(subdir.profile);
    expect(c?.evidence?.find((each) => each.item === "no session head")?.source).toContain(
      "the session repository is the one the old evidence names"
    );
  }, 60_000);

  // Catches: a case built before cases saved their Oh My Pi profile, whose session the store holds
  // no more, reprofiled under the profile `unknown`: a Legion case lost its worker image's units,
  // role plugins and AGENTS.md, and a dev-box case its installers.
  test("--reprofile of a case whose session the store no longer holds, and whose old evidence names no Oh My Pi profile, takes the store's: legion for a Legion store, default for a devbox store", async () => {
    const legion = byName("proj-legion") as Case;
    const devbox = byName("proj-devbox") as Case;
    const unsaved = (c: Case, name: string): Case => ({
      ...c,
      id: `${c.id}-unsaved`,
      source: { ...c.source, pathHash: pathHash(`/sessions/${name}.jsonl`) },
      evidence: (c.evidence ?? []).filter((each) => each.item !== "Oh My Pi profile"),
    });
    const from = path.join(scratch, "heads-unsaved", "old.jsonl");
    mkdirSync(path.dirname(from), { recursive: true });
    writeFileSync(
      from,
      [unsaved(legion, "unsaved-legion"), unsaved(devbox, "unsaved-devbox")]
        .map((c) => `${JSON.stringify(c)}\n`)
        .join("")
    );
    const fresh = path.join(scratch, "heads-unsaved", "reprofiled.jsonl");
    const args = ["build-cases", "--config", config, "--old", old, "--copies", copiesDir];
    const reprofile = ["--count", countDir, "--reprofile", from, "--out", fresh];
    const lines: string[] = [];
    const code = await main([...args, ...reprofile], io(lines));
    expect({ code, said: code === 0 ? "" : lines.join("") }).toEqual({ code: 0, said: "" });
    const [l, d] = loadCases(fresh, old);
    expect(l?.profile).toEqual(legion.profile);
    expect(d?.profile).toEqual(devbox.profile);
    expect(
      [l, d].map((c) => c?.evidence?.find((each) => each.item === "Oh My Pi profile")?.source)
    ).toEqual(["legion", "default"]);
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
      { repo: "acme/skills", commit: SHA, path: ".claude/CLAUDE.md", user: true },
    ]);
    const evidence = Object.fromEntries(c?.evidence?.map((each) => [each.item, each.source]) ?? []);
    // Its working directory is no checkout of a repository of the copies output's list, so it has
    // no project context file (`no session repository`, below).
    expect(evidence["no session repository"]).toBeDefined();
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

  test("a Legion pane in a workspace lists the .claude/skills and .omp/skills of the workspace's repository at its commit, each as a project skill, and its profile names that repository at that commit", () => {
    const c = byName("proj-legion");
    expect(c?.context).toBe("legion:ACME:tester");
    // The working directory is the checkout's root, so no skill below it is listed.
    expect(c?.profile.skills).toEqual([
      { repo: "acme/skills", commit: SHA, path: "skills/alpha", name: "alpha" },
      { repo: "acme/skills", commit: SHA, path: "skills/beta", name: "beta" },
      // The project's beta differs from the package's, so Oh My Pi lists it under its provider.
      {
        repo: "acme/proj",
        commit: SHA,
        path: ".claude/skills/beta",
        name: "beta",
        namespace: "claude",
      },
      { repo: "acme/proj", commit: SHA, path: ".claude/skills/kappa", name: "kappa" },
      { repo: "acme/proj", commit: SHA, path: ".omp/skills/lambda", name: "lambda" },
      { repo: "acme/proj", commit: SHA, path: ".omp/skills/older", name: "older" },
    ]);
    expect(c?.profile.sessionRepository).toEqual({ repo: "acme/proj", commit: SHA });
    const evidence = Object.fromEntries(c?.evidence?.map((each) => [each.item, each.source]) ?? []);
    expect(evidence["session repository acme/proj"]).toBe(
      "the session's working directory /legion/workspaces/acme/proj/K-1/ is a Legion workspace of it: /legion/workspaces/<owner>/<name>/"
    );
    // context.ts reads the session's absolute working directory from this item's source by this
    // phrase: the two must not drift.
    expect(
      /\bworking directory (\/[^\s,;]+)/.exec(evidence["session repository acme/proj"] ?? "")?.[1]
    ).toBe("/legion/workspaces/acme/proj/K-1/");
    expect(evidence["skill kappa (acme/proj .claude/skills/kappa)"]).toBe(
      `project skill of acme/proj at ${SHA}: Oh My Pi lists each <name>/SKILL.md under .claude/skills/ of the working directory and of each parent up to the repository's root, the nearest first (discovery/claude.ts, loadSkills, lines 205-224)`
    );
    expect(evidence["skill lambda (acme/proj .omp/skills/lambda)"]).toBe(
      `project skill of acme/proj at ${SHA}: Oh My Pi lists each <name>/SKILL.md that has a description under .omp/skills/ of the working directory and of each parent up to the repository's root, the nearest first (discovery/builtin.ts, loadSkills, lines 314-327, over getProjectAncestorDirs, lines 81-109)`
    );
    // The repository is read at the session's commit, one directory a read where Oh My Pi reads a
    // project (from its root), and never whole: a session repository's whole tree can be more
    // than GitHub gives in one answer.
    expect(ghCalls.some((args) => args.includes(`repos/acme/proj/git/trees/${SHA}`))).toBe(true);
    expect(
      ghCalls.some((args) => args.includes(`repos/acme/proj/git/trees/${SHA}?recursive=1`))
    ).toBe(false);
  });

  test("a dev-box session in a checkout of the one listed repository of its directory's name lists that repository's project skills and gets its root CLAUDE.md beside the user-level file", () => {
    const c = byName("proj-devbox");
    expect(c?.context).toBe("devbox");
    expect(c?.profile.skills).toEqual([
      // The user's older and the project's beta differ from the copies that keep their names.
      { repo: "acme/skills", commit: SHA, path: "skills/older", name: "older", namespace: "agent" },
      { repo: "acme/skills", commit: SHA, path: "skills/alpha", name: "alpha" },
      { repo: "acme/skills", commit: SHA, path: "skills/beta", name: "beta" },
      {
        repo: "acme/proj",
        commit: SHA,
        path: ".claude/skills/beta",
        name: "beta",
        namespace: "claude",
      },
      { repo: "acme/proj", commit: SHA, path: ".claude/skills/kappa", name: "kappa" },
      { repo: "acme/proj", commit: SHA, path: ".omp/skills/lambda", name: "lambda" },
      { repo: "acme/proj", commit: SHA, path: ".omp/skills/older", name: "older" },
    ]);
    expect(c?.profile.contextFiles).toEqual([
      { repo: "acme/skills", commit: SHA, path: ".claude/CLAUDE.md", user: true },
      { repo: "acme/proj", commit: SHA, path: "CLAUDE.md" },
    ]);
    expect(c?.profile.sessionRepository).toEqual({ repo: "acme/proj", commit: SHA });
    const evidence = Object.fromEntries(c?.evidence?.map((each) => [each.item, each.source]) ?? []);
    expect(evidence["session repository acme/proj"]).toBe(
      `${CHECKOUT_RULE}. The working directory /home/dev/src/proj is named proj, which acme/proj alone of the list is named`
    );
    expect(evidence["context file acme/proj CLAUDE.md"]).toBe(
      `${SLOT_RULE}: at the checkout's root, depth 0, acme/proj (the session repository) has CLAUDE.md at ${SHA}, and Oh My Pi keeps CLAUDE.md, priority 10 (discovery/claude-md.ts, lines 20-30, over discovery/helpers.ts's loadStandaloneContextFiles (lines 844-900), registered after agents-md.ts (discovery/index.ts, lines 24-25))`
    );
    // The tree read at that commit gives it; no file of acme/proj is read alone.
    expect(
      ghCalls.some((args) => args.some((arg) => arg.startsWith("repos/acme/proj/contents/")))
    ).toBe(false);
  });

  test("a dev-box directory whose name two listed repositories have, or none has, gives no session repository, and the evidence says which matched", () => {
    for (const name of ["two-repos", "good"]) {
      const c = byName(name);
      expect(c?.profile.skills.map((skill) => skill.name)).toEqual(["alpha", "beta", "older"]);
      expect(c?.profile.skills.every((skill) => skill.repo === "acme/skills")).toBe(true);
      expect(c?.profile.contextFiles).toEqual([
        { repo: "acme/skills", commit: SHA, path: ".claude/CLAUDE.md", user: true },
      ]);
      expect(c?.profile.sessionRepository).toBeUndefined();
    }
    const why = (name: string) =>
      byName(name)?.evidence?.find((each) => each.item === "no session repository")?.source;
    const rule = `${CHECKOUT_RULE}.`;
    expect(why("two-repos")).toBe(
      `${rule} The working directory /home/dev/src/skills/notes is under /home/dev/src/skills, named skills, which 2 repositories of the list are named (acme/skills, forker/skills), so which one it is a checkout of is not known`
    );
    expect(why("good")).toBe(
      `${rule} No part of the working directory /legion/scratch/tmp/acme-1/t1-runs/f1/work is named as a repository of the list`
    );
  });

  test("a project skill and a user or plugin skill of the same name follow Oh My Pi's order: the project's .omp/skills come before ~/.omp/agent/skills, and the project's .claude/skills after those and after extension packages", () => {
    /** Checks that `source` gives `start` and then Oh My Pi's order with its source. */
    const decided = (source: string | undefined, start: string) => {
      expect(source).toContain(
        `${start}: Oh My Pi 18.18.6 keeps under a name the first skill it admits, in its providers' order`
      );
      for (const cited of [
        "capability/index.ts, lines 177-184",
        "extensibility/skills.ts, resolveCollision, lines 141-223",
        "discovery/builtin.ts, lines 81-109 and 314-342",
        "discovery/omp-plugins.ts, lines 136-169",
        "discovery/claude.ts, lines 205-249",
      ]) {
        expect(source).toContain(cited);
      }
    };
    // On the dev box acme/skills's installer links beta and older into ~/.omp/agent/skills: the
    // user's beta comes before the project's .claude/skills/beta, and the project's
    // .omp/skills/older before the user's older. Each later copy differs, so Oh My Pi lists it
    // under a namespace: claude/beta (its provider's, for the dotted .claude/) and agent/older.
    const devbox = Object.fromEntries(
      byName("proj-devbox")?.evidence?.map((each) => [each.item, each.source]) ?? []
    );
    const claudeBeta = devbox["skill claude/beta (acme/proj .claude/skills/beta)"];
    decided(
      claudeBeta,
      "under the name beta Oh My Pi gives the session the copy of acme/skills skills/beta under that name (user-skills:omp links it (acme/skills:installers/skills.sh))"
    );
    expect(claudeBeta).toContain(
      "and this copy's text differs: Oh My Pi 18.18.6 keeps the bare name"
    );
    expect(devbox["skill older (acme/proj .omp/skills/older)"]).toStartWith(
      "project skill of acme/proj"
    );
    decided(
      devbox["skill agent/older (acme/skills skills/older)"],
      "under the name older Oh My Pi gives the session the copy of acme/proj .omp/skills/older under that name (project skill of acme/proj)"
    );
    // In the Legion pane the package @acme/pi-acme, an extension package, ships beta, and it
    // comes before the project's .claude/skills/beta.
    const legion = Object.fromEntries(
      byName("proj-legion")?.evidence?.map((each) => [each.item, each.source]) ?? []
    );
    const legionBeta = legion["skill claude/beta (acme/proj .claude/skills/beta)"];
    expect(legionBeta).toContain(
      "under the name beta Oh My Pi gives the session the copy of acme/skills skills/beta under that name (package:@acme/pi-acme "
    );
    expect(legionBeta).toContain("extension packages come next (90");
  });

  test("a dev-box working directory below its checkout's root lists the .omp/skills and .claude/skills of that directory and of each parent up to the root, the nearer first, and of no other directory", () => {
    const c = byName("proj-subdir");
    // other/ is no level of the working directory, so its xi is not listed.
    expect(c?.profile.skills).toEqual([
      { repo: "acme/skills", commit: SHA, path: "skills/older", name: "older", namespace: "agent" },
      { repo: "acme/skills", commit: SHA, path: "skills/alpha", name: "alpha" },
      { repo: "acme/skills", commit: SHA, path: "skills/beta", name: "beta" },
      {
        repo: "acme/proj",
        commit: SHA,
        path: ".claude/skills/beta",
        name: "beta",
        namespace: "claude",
      },
      { repo: "acme/proj", commit: SHA, path: "agents/.claude/skills/kappa", name: "kappa" },
      { repo: "acme/proj", commit: SHA, path: ".omp/skills/lambda", name: "lambda" },
      { repo: "acme/proj", commit: SHA, path: "agents/.claude/skills/mu", name: "mu" },
      { repo: "acme/proj", commit: SHA, path: "agents/agentvm/.omp/skills/nu", name: "nu" },
      { repo: "acme/proj", commit: SHA, path: ".omp/skills/older", name: "older" },
    ]);
    const evidence = Object.fromEntries(c?.evidence?.map((each) => [each.item, each.source]) ?? []);
    expect(evidence["session repository acme/proj"]).toBe(
      `${CHECKOUT_RULE}. The working directory /home/dev/src/proj/agents/agentvm is under /home/dev/src/proj, named proj, which acme/proj alone of the list is named, and its tree holds agents/agentvm; the working directory is agents/agentvm in the checkout`
    );
    // Of two kappa, the one of the nearer level (agents/) comes first.
    expect(evidence["left out: skill kappa from acme/proj .claude/skills/kappa"]).toStartWith(
      "project skill of acme/proj too; Oh My Pi gives the session the copy of acme/proj agents/.claude/skills/kappa under that name (project skill of acme/proj): Oh My Pi 18.18.6 keeps under a name the first skill it admits"
    );
    expect(c?.profile.workingDirectory).toBe("agents/agentvm");
    expect(c?.profile.sessionRepository).toEqual({ repo: "acme/proj", commit: SHA });
  });

  test("a working directory below its checkout's root gets the context file of each level up to the root, the farthest first, at its path in the repository", () => {
    const c = byName("proj-subdir");
    expect(c?.profile.contextFiles).toEqual([
      { repo: "acme/skills", commit: SHA, path: ".claude/CLAUDE.md", user: true },
      { repo: "acme/proj", commit: SHA, path: "CLAUDE.md" },
      { repo: "acme/proj", commit: SHA, path: "agents/agentvm/AGENTS.md" },
    ]);
    const evidence = Object.fromEntries(c?.evidence?.map((each) => [each.item, each.source]) ?? []);
    expect(evidence["context file acme/proj agents/agentvm/AGENTS.md"]).toBe(
      `${SLOT_RULE}: at agents/agentvm, depth 0, acme/proj (the session repository) has agents/agentvm/AGENTS.md at ${SHA}, and Oh My Pi keeps agents/agentvm/AGENTS.md, priority 10 (discovery/agents-md.ts, lines 20-30, over discovery/helpers.ts's loadStandaloneContextFiles (lines 844-900))`
    );
    expect(evidence["context file acme/proj CLAUDE.md"]).toStartWith(
      `${SLOT_RULE}: at the checkout's root, depth 2, acme/proj (the session repository) has CLAUDE.md`
    );
  });

  test("within one level's slot the context file Oh My Pi's ranking picks is kept: the working directory's .claude/CLAUDE.md (80) before its AGENTS.md and CLAUDE.md (10), and the user-level file stays marked apart", () => {
    const c = byName("dual-root");
    expect(c?.profile.contextFiles).toEqual([
      { repo: "acme/skills", commit: SHA, path: ".claude/CLAUDE.md", user: true },
      { repo: "acme/dual", commit: SHA, path: ".claude/CLAUDE.md" },
    ]);
    const evidence = Object.fromEntries(c?.evidence?.map((each) => [each.item, each.source]) ?? []);
    expect(evidence["context file acme/dual .claude/CLAUDE.md"]).toBe(
      `${SLOT_RULE}: at the checkout's root, depth 0, acme/dual (the session repository) has .claude/CLAUDE.md, AGENTS.md, CLAUDE.md at ${SHA}, and Oh My Pi keeps .claude/CLAUDE.md, priority 80 (discovery/claude.ts, line 45 and lines 72-77 and 180-192: at the working directory alone, an empty file too)`
    );
    expect(evidence["left out: acme/dual AGENTS.md"]).toBe(
      `${SLOT_RULE}: at the checkout's root, depth 0, .claude/CLAUDE.md (priority 80) comes before it (priority 10; discovery/agents-md.ts, lines 20-30, over discovery/helpers.ts's loadStandaloneContextFiles (lines 844-900))`
    );
    expect(evidence["left out: acme/dual CLAUDE.md"]).toStartWith(
      `${SLOT_RULE}: at the checkout's root, depth 0, .claude/CLAUDE.md (priority 80) comes before it (priority 10;`
    );
  });

  test("a Legion pane's root context file follows Oh My Pi's rule as a dev box's does: the workspace repository's AGENTS.md, else its CLAUDE.md", () => {
    // acme/proj holds no AGENTS.md, so its CLAUDE.md is the one Oh My Pi reads.
    expect(byName("proj-legion")?.profile.contextFiles).toEqual([
      { repo: "acme/proj", commit: SHA, path: "CLAUDE.md" },
      { repo: "acme/infra", commit: SHA, path: "legion/instructions/acme.md" },
    ]);
    const evidence = (name: string) =>
      Object.fromEntries(byName(name)?.evidence?.map((each) => [each.item, each.source]) ?? []);
    const rule = `${SLOT_RULE}: at the checkout's root, depth 0, `;
    expect(evidence("proj-legion")["context file acme/proj CLAUDE.md"]).toStartWith(
      `${rule}acme/proj (the session repository) has CLAUDE.md at ${SHA}, and Oh My Pi keeps CLAUDE.md, priority 10`
    );
    // acme/widgets holds an AGENTS.md, which comes first.
    expect(evidence("legion")["context file acme/widgets AGENTS.md"]).toStartWith(
      `${rule}acme/widgets (the session repository) has AGENTS.md at ${SHA}, and Oh My Pi keeps AGENTS.md, priority 10`
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
      "user-skills:omp links it (acme/other:installers/skills.sh) too; the session read the copy of acme/skills skills/alpha (message 4); its SKILL.md is the same, and Oh My Pi drops such a copy (NAMESPACE_RULE)"
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
    // Every session but pinned-units and pinned-subagent, and the missing one.
    expect(summary.sessionsTried).toBe(Object.keys(SESSIONS).length - 2 + 1);
    expect(summary.casesBuilt).toBe(21);
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

  test("a build under the memory limit records its peak resident memory in the summary and prints it in its last line", () => {
    expect(summaryOf(out).peakRssBytes).toBe(PEAK_BYTES);
    expect(printed.at(-1)).toContain(
      `of readers at list price, peak memory 200 MiB; written to ${out} and ${out}.summary.json`
    );
  });
});

describe("build-cases over reads of the skill by path", () => {
  const out = path.join(scratch, "by-path", "cases.jsonl");
  let code = -1;
  let cases: Case[] = [];
  let requests: RecordedRequest[] = [];
  const caseNamed = (name: string) =>
    cases.find((each) => each.source.pathHash === pathHash(sessionPath(name)));
  const record = (name: string) =>
    summaryOf(out).sessions.find((each) => each.hash === fileHash(sessionPath(name)));
  /** The path each read call of `c` reads, by the call's id. */
  const readPaths = (c: Case | undefined) =>
    new Map(
      (c?.messages ?? []).flatMap((message) =>
        (message.content ?? []).flatMap((block): [string, unknown][] =>
          block.type === "toolCall" && block.name === "read"
            ? [[block.id ?? "", Reflect.get(Object(block.arguments), "path")]]
            : []
        )
      )
    );
  beforeAll(async () => {
    const sessions = sessionsFile("by-path.jsonl", Object.keys(BY_PATH));
    const from = recorder.requests.length;
    code = await main(buildArgs(sessions, out, "3"), io([]));
    requests = recorder.requests.slice(from);
    cases = loadCases(out, old);
  }, HOOK_BUILD_MS);

  test("a whole read of the cut's copy by its path builds a case that names the copy, holds the read in skillReads, installs the copy, and hides the read's text from the case reader", () => {
    expect(code).toBe(EXIT.ok);
    const c = caseNamed("path-read");
    // The copy as the read's path gives it, and the session's working directory; its transcript's
    // path names no home.
    expect(c?.pathCopy).toEqual({ dir: `${F1_SKILLS}/alpha`, cwd: F1_CWD });
    expect(c?.skillReads).toEqual([2, 4]);
    const paths = readPaths(c);
    expect(c?.skillReads.map((index) => paths.get(c.messages[index]?.toolCallId ?? ""))).toEqual([
      `${F1_SKILLS}/alpha/SKILL.md`,
      `${F1_SKILLS}/alpha/references/x.md`,
    ]);
    // The line comes from the read by path, less Oh My Pi's rows.
    expect(c?.line.text).toBe(LINE);
    expect(c?.profile.skills).toContainEqual(
      expect.objectContaining({ name: "alpha", repo: "acme/skills", path: "skills/alpha" })
    );
    expect(
      c?.evidence?.some((each) =>
        each.source.startsWith(`message 2 reads ${F1_SKILLS}/alpha/SKILL.md by its path`)
      )
    ).toBe(true);
    expect(c?.evidence?.some((each) => each.item === EXCLUDED_READS)).toBe(false);
    const reader = requests.filter(
      (request) =>
        request.body.includes("(case path-read)") && request.body.includes("propose one case")
    );
    expect(reader.length).toBeGreaterThan(0);
    for (const request of reader) {
      expect(request.body).toContain("[the text of a file of the skill, left out");
      expect(request.body).not.toContain(`[${F1_SKILLS}/alpha/SKILL.md#`);
    }
  });

  test("a whole read of the cut's copy relative to the session's working directory, or under ~ in a session whose transcript names its home, builds a case whose pathCopy resolves it, and each arm answers it at the path the session read", () => {
    for (const [name, dir, dirs] of [
      ["path-relative-read", F1_RELATIVE, { cwd: F1_CWD }],
      ["path-home", F1_TILDE, { cwd: F1_CWD, home: F1_HOME }],
    ] as const) {
      const c = caseNamed(name);
      expect({ name, pathCopy: c?.pathCopy }).toEqual({
        name,
        pathCopy: { dir: `${F1_SKILLS}/alpha`, ...dirs },
      });
      expect({ name, skillReads: c?.skillReads }).toEqual({ name, skillReads: [2, 4] });
      if (c === undefined) continue;
      const target = `${dir}/SKILL.md`;
      const armOf = (arm: "old" | "new" | "none") =>
        buildArm(c, arm, { old, new: newDir }, path.join(scratch, `arm-${name}-${arm}`));
      const textAt = (arm: Arm, index: number) => arm.messages[index]?.content?.[0]?.text ?? "";
      /** The absolute path the arm's record names as the file the read opened. */
      const opened = (arm: Arm, index: number) =>
        (arm.messages[index]?.details as { meta?: { source?: { value?: string } } } | undefined)
          ?.meta?.source?.value;
      // old: the old text's rows under the session's own hashline line, at the path it read.
      const oldArm = armOf("old");
      expect(textAt(oldArm, 2).split("\n")[0]).toStartWith(`[${target}#`);
      expect(textAt(oldArm, 2)).toContain(`:${LINE}`);
      expect(opened(oldArm, 2)).toBe(`${F1_SKILLS}/alpha/SKILL.md`);
      // new: its own text, which has no LINE, at the same path; a new read of it, as replay
      // asks one, is answered the same way.
      const newArm = armOf("new");
      expect(textAt(newArm, 2).split("\n")[0]).toStartWith(`[${target}#`);
      expect(textAt(newArm, 2)).not.toContain(LINE);
      expect(textAt(newArm, 4)).toContain("The alpha steps are counted from one");
      expect(pathFile(newArm, target)).toEqual({ text: textAt(newArm, 2), isError: false });
      // none: Oh My Pi's not-found result for the path as read.
      const noneArm = armOf("none");
      expect(textAt(noneArm, 2)).toBe(`Path '${target}' not found`);
      expect(noneArm.messages[2]?.isError).toBe(true);
    }
  });

  test("a whole read by path in a checkout whose path leaves the copies of several repositories tied goes to the one of them the store's context installs: at the cut's copy, it builds a case that names the copy as read, with the context rule in its evidence", () => {
    const c = caseNamed("path-tie");
    expect(c?.pathCopy).toEqual({
      dir: "/home/dev/notes/skills/alpha",
      cwd: F1_CWD,
      home: "/home/fixture",
    });
    expect(c?.skillReads).toEqual([2, 4]);
    expect(c?.profile.skills).toContainEqual(
      expect.objectContaining({ name: "alpha", repo: "acme/skills", path: "skills/alpha" })
    );
    expect(
      c?.evidence?.find(
        (each) => each.item === "unit user-skills:omp (acme/skills:installers/skills.sh)"
      )?.source
    ).toContain(
      "message 2 reads /home/dev/notes/skills/alpha/SKILL.md by its path, which attribute.ts's context rule places at acme/skills skills/alpha (context: to acme/skills skills/alpha: the path ends with skills/alpha/, the skill directory of 3 repositories (acme/other, acme/skills, forker/skills), and no directory of the path names just one of them, and its context installs that one of them)"
    );
  });

  test("a session that reads alpha by path only with a selector, of another copy, at a path no rule places, or of the cut's copy at a relative or ~ path whose directory the session does not name is dropped under that reason, with no reader call", () => {
    const expected: Record<string, [DropReason, string]> = {
      "path-range": [
        "path-range",
        `message 2 answers the read of ${F1_SKILLS}/alpha/SKILL.md:1-20 (path-range: a read selector after its last /)`,
      ],
      "path-other": [
        "other-copy",
        "message 2 answers the read of /elsewhere/other-skills/alpha/SKILL.md (other-copy: the prefix table rule places it at acme/other skills/alpha)",
      ],
      // A tie the store's context does not decide: store_c's installs acme/other's alpha alone.
      "path-unattributed": [
        "unattributed",
        "message 2 answers the read of /home/dev/src/skills/skills/alpha/SKILL.md (unattributed: the path ends with skills/alpha/, the skill directory of 3 repositories (acme/other, acme/skills, forker/skills), and no directory of the path names just one of them, and its context installs none of them)",
      ],
      "path-no-cwd": [
        "path-relative",
        `message 2 answers the read of ${F1_RELATIVE}/SKILL.md (path-relative: the prefix table rule places it at acme/skills skills/alpha, at ${F1_RELATIVE}, which needs the session's working directory, which its header does not name)`,
      ],
      "path-no-home": [
        "path-relative",
        `message 2 answers the read of ${F1_TILDE}/SKILL.md (path-relative: the prefix table rule places it at acme/skills skills/alpha, at ${F1_TILDE}, which needs the home directory the session ran under, which its transcript's path does not name (no /.omp/ in it))`,
      ],
      "path-at-name": [
        "path-relative",
        `message 2 answers the read of ${F1_TILDE.replace("~", "@~dev")}/SKILL.md (path-relative: the prefix table rule places it at acme/skills skills/alpha, at ${F1_TILDE.replace("~", "@~dev")}, which needs the session's working directory, which its header does not name)`,
      ],
    };
    for (const [name, [reason, detail]] of Object.entries(expected)) {
      expect({ name, reason: record(name)?.reason }).toEqual({ name, reason });
      expect(record(name)?.detail).toContain(detail);
      expect(record(name)?.windows).toBeUndefined();
      expect(requests.some((request) => request.body.includes(`(case ${name})`))).toBe(false);
    }
  });

  test("a session that reads alpha by skill:// and another copy by path builds its case from the skill:// reads, keeps the other read as recorded, and names it in its evidence", () => {
    const c = caseNamed("path-mixed");
    expect(c?.pathCopy).toBeUndefined();
    expect(c?.skillReads).toEqual([2, 4]);
    expect(c?.messages[6]?.content?.[0]?.text).toStartWith(
      "[/elsewhere/other-skills/alpha/SKILL.md#"
    );
    expect(c?.evidence?.find((each) => each.item === EXCLUDED_READS)?.source).toBe(
      "each answered as recorded in every arm: message 6 answers the read of /elsewhere/other-skills/alpha/SKILL.md (other-copy: the prefix table rule places it at acme/other skills/alpha)"
    );
  });

  test("a case whose read by path is of a file the arms do not answer in Oh My Pi's form, in the new text, is dropped as path-form", () => {
    expect(record("path-form")?.reason).toBe("path-form");
    expect(record("path-form")?.detail).toBe(
      `in the new text ${newDir}: ${NEW_WIDE.path} has 301 lines, and a read by path shows 300, then the end of each block past them`
    );
    expect(caseNamed("path-form")).toBeUndefined();
  });

  test("the summary counts each drop by its reason", () => {
    const summary = summaryOf(out);
    expect(summary.drops).toEqual({
      "path-range": 1,
      "other-copy": 1,
      unattributed: 1,
      "path-relative": 3,
      "path-form": 1,
    });
    expect(Object.keys(summary.drops).every((reason) => reason in DROP_REASONS)).toBe(true);
    expect(summary.casesBuilt).toBe(5);
    expect(cases.map((each) => each.source.pathHash).sort()).toEqual(
      ["path-read", "path-relative-read", "path-home", "path-tie", "path-mixed"]
        .map((name) => pathHash(sessionPath(name)))
        .sort()
    );
  });
});

/** `args` less `--new` and its directory: a build before the cut has a text. */
const withoutNew = (args: readonly string[]) =>
  args.filter((arg, at) => arg !== "--new" && args[at - 1] !== "--new");

// A branch's `--set` build runs before the cut has a text, so build-cases takes --new as
// optional.
describe("build-cases without --new", () => {
  const out = path.join(scratch, "no-new", "cases.jsonl");
  let code = -1;
  beforeAll(async () => {
    const sessions = sessionsFile("no-new.jsonl", ["path-form"]);
    code = await main(withoutNew(buildArgs(sessions, out, "3")), io([]));
  }, HOOK_BUILD_MS);

  // Catches: a build that needs a cut text it does not have yet refused, or one that checks a
  // read by path against a text it was not given.
  test("writes new: null in its summary's inputs and checks the old text alone: a read by path of a file only the new text holds builds a case", () => {
    expect(code).toBe(EXIT.ok);
    const summary = JSON.parse(readFileSync(`${out}.summary.json`, "utf8")) as {
      inputs: { new: unknown };
      casesBuilt: number;
    };
    expect(summary.inputs.new).toBeNull();
    expect(summary.casesBuilt).toBe(1);
    expect(loadCases(out, old).map((c) => c.source.pathHash)).toEqual([
      pathHash(sessionPath("path-form")),
    ]);
  });

  // Catches: a resumed build that mixes cases checked against the new text with cases that were
  // not.
  test("a resume with the other choice of --new is refused, naming it, and leaves the files as they were", async () => {
    const sessions = sessionsFile("no-new.jsonl", ["path-form"]);
    const before = readFileSync(`${out}.summary.json`, "utf8");
    const given: string[] = [];
    expect(await main(buildArgs(sessions, out, "3"), io(given))).toBe(EXIT.refused);
    expect(given.join("\n")).toContain(`--new was not given, now ${newDir} (sha256 `);
    expect(readFileSync(`${out}.summary.json`, "utf8")).toBe(before);
    // The reverse: a build given --new, resumed without it.
    const withNew = path.join(scratch, "with-new", "cases.jsonl");
    expect(await main(buildArgs(sessions, withNew, "3"), io([]))).toBe(EXIT.ok);
    const dropped: string[] = [];
    expect(await main(withoutNew(buildArgs(sessions, withNew, "3")), io(dropped))).toBe(
      EXIT.refused
    );
    expect(dropped.join("\n")).toMatch(
      new RegExp(`--new was ${newDir} \\(sha256 [0-9a-f]{12}\\), now not given`)
    );
  }, 60_000);
});

// The architect's rule (2026-11-22T16:58:39Z): a session that reads the skill as material, to
// edit, test, grade or compare it, is no case of the skill's behaviour, since its next step works
// on the text and does not follow it.
test("a skill:// read in a skill-writing session and a whole-file path read in an eval checkout are dropped as skill-as-material on the case reader's answer, and a normal case is kept", async () => {
  const out = path.join(scratch, "material", "cases.jsonl");
  const names = ["material-writing", "material-eval", "good"];
  const sessions = sessionsFile("material.jsonl", names);
  const lines: string[] = [];
  expect(await main(buildArgs(sessions, out, "1"), io(lines))).toBe(EXIT.ok);
  const summary = summaryOf(out);
  for (const name of ["material-writing", "material-eval"]) {
    const record = summary.sessions.find((each) => each.hash === fileHash(sessionPath(name)));
    expect(record?.reason).toBe("skill-as-material");
    expect(record?.detail).toStartWith("the case reader, in window 1 (messages ");
    expect(record?.detail).toEndWith("): The agent works on alpha's text (message 0).");
  }
  expect(summary.drops).toEqual({ "skill-as-material": 2 });
  expect(loadCases(out, old).map((c) => c.source.pathHash)).toEqual([
    pathHash(sessionPath("good")),
  ]);
}, 60_000);

test("a dev-box session that read a skill of a github: dependency's package and one of a cloned pool gets a profile that holds each unit's skills, the pool's at the commit its installer pins at the session's start", async () => {
  const out = path.join(scratch, "pinned-units", "cases.jsonl");
  const lines: string[] = [];
  const callsBefore = ghCalls.length;
  const sessions = sessionsFile("pinned-units.jsonl", ["pinned-units"]);
  expect(await main(buildArgs(sessions, out, "1"), io(lines))).toBe(EXIT.ok);
  const [c] = loadCases(out, old);
  expect(c?.context).toBe("devbox");
  expect(
    c?.profile.skills
      .map((skill) => `${skill.name} ${skill.repo} ${skill.path} ${skill.commit}`)
      .sort()
  ).toEqual([
    `alpha acme/skills skills/alpha ${SHA}`,
    `beta acme/skills skills/beta ${SHA}`,
    `older acme/skills skills/older ${SHA}`,
    `pack-one acme/pack skills/pack-one ${PACK_PIN}`,
    `pack-two acme/pack skills/pack-two ${PACK_PIN}`,
    `pack-two acme/powers skills/pack-two ${SHA}`,
    `power-one acme/powers skills/power-one ${SHA}`,
    `power-two acme/powers skills/power-two ${SHA}`,
  ]);
  // Two pack-two of other texts: the pool's keeps the bare name, and the package's is listed
  // under the name of its directory in the plugins' node_modules.
  expect(c?.profile.skills).toContainEqual({
    repo: "acme/powers",
    commit: SHA,
    path: "skills/pack-two",
    name: "pack-two",
    namespace: "acme-powers",
  });
  // The package at its repository's root is named as Oh My Pi installs it.
  expect(c?.profile.plugins).toContainEqual({
    repo: "acme/powers",
    commit: SHA,
    path: ".",
    name: "acme-powers",
  });
  const evidence = Object.fromEntries(c?.evidence?.map((each) => [each.item, each.source]) ?? []);
  expect(evidence[`commit ${PACK_PIN} of acme/pack`]).toStartWith(
    `the last commit of ${PACK_PIN}, the ref acme/dots:installers/omp.sh pins at the session's start`
  );
  expect(evidence["left out: skill pack-later (acme/pack skills/pack-later)"]).toContain(
    "does not ship it at the session's start"
  );
  // acme/pack's later ref (repositories.tsv's) has no commit at the session's start, so its
  // default branch is read before its installer's pin takes its place.
  expect(
    ghCalls
      .slice(callsBefore)
      .flat()
      .some((arg) => arg.startsWith(`repos/acme/pack/commits?sha=${PACK_LATER}&`))
  ).toBe(true);
  expect(
    ghCalls
      .slice(callsBefore)
      .flat()
      .some((arg) => arg.startsWith("repos/acme/pack/commits?sha=main&"))
  ).toBe(true);
}, 60_000);

test("a subagent's profile reads its packages at its root's last prompt build before its start, and its pools at its own start, and the evidence names both times", async () => {
  const out = path.join(scratch, "pinned-subagent", "cases.jsonl");
  const lines: string[] = [];
  const sessions = sessionsFile("pinned-subagent.jsonl", ["pinned-subagent"]);
  expect(await main(buildArgs(sessions, out, "1"), io(lines))).toBe(EXIT.ok);
  const [c] = loadCases(out, old);
  // acme/powers, made after the root's start and before its last prompt build, is read at that
  // build, when it had power-one alone: power-two, added later but before the subagent's start,
  // is not installed. acme/pack, the pool's clone, is read at the subagent's start.
  expect(
    c?.profile.skills
      .map((skill) => `${skill.name} ${skill.repo} ${skill.commit}`)
      .filter((each) => !each.includes("acme/skills"))
      .sort()
  ).toEqual([
    `pack-one acme/pack ${PACK_PIN}`,
    `pack-two acme/pack ${PACK_PIN}`,
    `power-one acme/powers ${POWERS_OLD}`,
  ]);
  const evidence = Object.fromEntries(c?.evidence?.map((each) => [each.item, each.source]) ?? []);
  expect(evidence["read times"]).toStartWith(
    `the subagent started at ${F1_START}; its root (${pathHash(`${PINNED_ROOT}.jsonl`).slice(0, 12)}) built its prompt last at ${ROOT_BUILD} before it (2 builds), so each repository is read at ${ROOT_BUILD}, and a pool's at the subagent's start ${F1_START}: Oh My Pi 18.18.6 gives a subagent the skills its parent loaded`
  );
  expect(evidence[`commit ${POWERS_OLD} of acme/powers`]).toContain(
    `at or before the root's last prompt build ${ROOT_BUILD}`
  );
  expect(evidence[`commit ${PACK_PIN} of acme/pack`]).toContain(
    `at or before the session's start ${F1_START}`
  );
}, 60_000);

test("a subagent whose dev box pins packages by npm versions gets each package as the version its plugin list pinned at the root's last prompt build ships it, its pool as its own start has it, and a skill at a repository's root with the path .", async () => {
  // The suite's copies output, with acme/dots as the dev box's repository (it holds an installer
  // script and Claude Code's settings) and its installs of acme/kit and acme/one-skill: acme/dots's
  // plugin list installs @acme/kit, whose repository the copies output read; the pinned clone
  // acme/one-skill it names no unit of, so the build reads it at its pin.
  const list = ["acme/dots", "omp/plugins/package.json", "omp-plugins", "installs"];
  const copies = copiesWith("copies-dots", {
    "manifests.tsv": DOTS_MANIFESTS,
    "installers.tsv": [
      [
        ...list,
        "package:@acme/kit",
        "omp",
        "kit-one",
        "acme/kit",
        "packages/kit/skills/kit-one",
        "",
      ],
    ],
    "skill-copies.tsv": [
      ["acme/kit", "packages/kit/skills/kit-one", "kit-one", 10, "no", "no", "", "", ""],
    ],
  });
  const out = path.join(scratch, "npm-pins", "cases.jsonl");
  const sessions = sessionsFile("npm-pins.jsonl", ["pinned-subagent"]);
  const lines: string[] = [];
  const code = await main(buildArgs(sessions, out, "1", "--copies", copies), io(lines));
  expect({ code, said: code === EXIT.ok ? "" : lines.join("") }).toEqual({
    code: EXIT.ok,
    said: "",
  });
  const [c] = loadCases(out, old);
  const skills = (c?.profile.skills ?? []).map(
    (skill) => `${skill.name} ${skill.repo} ${skill.path} ${skill.commit}`
  );
  // The plugin list at the root's build (DOTS_OLD) pins @acme/kit 1.0.0, whose release tag ships
  // kit-one alone; the later list's 2.0.0 does not count. @acme/gone 3.0.0 has no tag, and its
  // tarball ships gone-one alone. acme/one-skill's skill is at its root.
  expect(skills).toEqual(
    expect.arrayContaining([
      `kit-one acme/kit packages/kit/skills/kit-one ${KIT_V1}`,
      `gone-one acme/kit packages/gone/skills/gone-one ${SHA}`,
      `one-skill acme/one-skill . ${SHA}`,
      `pack-one acme/pack skills/pack-one ${PACK_PIN}`,
    ])
  );
  expect(skills.filter((each) => /^(kit-two|gone-two) /.test(each))).toEqual([]);
  const evidence = Object.fromEntries(c?.evidence?.map((each) => [each.item, each.source]) ?? []);
  expect(evidence["read times of acme/dots"]).toStartWith(
    `its plugin lists, installers and settings at ${DOTS_OLD} (the root's last prompt build ${ROOT_BUILD}), its pool directories at ${SHA}`
  );
  // context.ts reads the commit of a subagent's pool directories from this item's source by this
  // phrase: the two must not drift.
  expect(
    /\bits pool directories at ([0-9a-f]{7,40})\b/.exec(
      evidence["read times of acme/dots"] ?? ""
    )?.[1]
  ).toBe(SHA);
  const versionOf = (name: string) =>
    Object.entries(evidence).find(([item]) => item.startsWith(`version of package:${name} `))?.[1];
  expect(versionOf("@acme/kit")).toStartWith(
    `acme/dots:omp/plugins/package.json at ${DOTS_OLD} pins @acme/kit 1.0.0; its release tag kit-v1.0.0 (gh api repos/acme/kit/git/matching-refs/tags/kit-v1.0.0) is acme/kit at ${KIT_V1}`
  );
  expect(versionOf("@acme/gone")).toContain(
    "GitHub lists no tag gone-v3.0.0 of acme/kit (gh api repos/acme/kit/git/matching-refs/tags/gone-v3.0.0), so the registry's tarball (https://registry.npmjs.org/@acme/gone/-/gone-3.0.0.tgz) names the skills it ships (gone-one)"
  );
  // The same session as a load task's: its profile is written with the root skill's path ".",
  // and load reads it.
  const from = path.join(scratch, "npm-pins", "tasks.jsonl");
  writeFileSync(
    from,
    `${JSON.stringify({ id: "r1", text: "Read the alpha steps.", label: "need", source: "session", store: "store_a", pathHash: pathHash(sessionPath("pinned-subagent")) })}\n`
  );
  writeFileSync(`${from}.summary.json`, JSON.stringify({ command: "build-tasks", skill: "alpha" }));
  const dir = path.join(scratch, "npm-pins", "profiles");
  const said: string[] = [];
  const tasksCode = await main(
    reprofileArgs(config, from, dir).map((arg) => (arg === copiesDir ? copies : arg)),
    io(said)
  );
  expect({ tasksCode, said: tasksCode === EXIT.ok ? "" : said.join("") }).toEqual({
    tasksCode: EXIT.ok,
    said: "",
  });
  const written = loadProfile(path.join(dir, "r1.json"), "omp", "alpha").profile;
  expect(written.skills).toContainEqual({
    repo: "acme/one-skill",
    commit: SHA,
    path: ".",
    name: "one-skill",
  });
  // Catches: build-tasks --reprofile reading the root's prompt builds (digest-times.sql) once for
  // each subagent task. Two tasks of the same subagent whose old profile, the one just written,
  // records its prompt: one digest-times.sql query of the store, and the same profile for each.
  const again = path.join(scratch, "npm-pins", "again");
  mkdirSync(again, { recursive: true });
  const fromAgain = path.join(again, "tasks.jsonl");
  const task = JSON.parse(readFileSync(from, "utf8"));
  const oldProfile = path.join(dir, "r1.json");
  writeFileSync(
    fromAgain,
    ["r1", "r2"].map((id) => `${JSON.stringify({ ...task, id, profile: oldProfile })}\n`).join("")
  );
  writeFileSync(`${fromAgain}.summary.json`, readFileSync(`${from}.summary.json`));
  const againDir = path.join(again, "profiles");
  const logged = psqlLogLength();
  expect(
    await main(
      reprofileArgs(config, fromAgain, againDir).map((arg) => (arg === copiesDir ? copies : arg)),
      io([])
    )
  ).toBe(EXIT.ok);
  expect(psqlLines(logged).filter((line) => line.kind === "digests")).toHaveLength(1);
  for (const id of ["r1", "r2"]) {
    const rebuilt = readFileSync(path.join(againDir, `${id}.json`), "utf8");
    expect(rebuilt).toBe(readFileSync(oldProfile, "utf8"));
  }
}, 60_000);

test("a dev-box profile installs the units its dev box's own installers install though the session read none of them, leaves out a skill its Oh My Pi config ignores, and installs a skill that hides itself as not listed", async () => {
  // The copies output of the suite, with acme/home named as the dev box's repository: it holds an
  // installer script and Claude Code's settings.
  const copies = copiesWith("copies-home", { "manifests.tsv": HOME_MANIFESTS });
  const out = path.join(scratch, "devbox-home", "cases.jsonl");
  const sessions = sessionsFile("devbox-home.jsonl", ["good"]);
  const lines: string[] = [];
  expect(await main(buildArgs(sessions, out, "1", "--copies", copies), io(lines))).toBe(EXIT.ok);
  const [c] = loadCases(out, old);
  const names = (c?.profile.skills ?? []).map((skill) => `${skill.name} ${skill.repo}`);
  // The session read none of acme/home's skills; the dev box installs them all but dropped.
  expect(names).toEqual(
    expect.arrayContaining(["unread acme/home", "quiet acme/home", "shy acme/home"])
  );
  expect(names).not.toContain("dropped acme/home");
  const evidence = Object.fromEntries(c?.evidence?.map((each) => [each.item, each.source]) ?? []);
  expect(evidence["unit user-skills:omp (acme/home:installers/omp.sh)"]).toStartWith(
    "a dev box installs every unit the installers of its own repository install into Oh My Pi at the session's start, whether or not the session read from it"
  );
  expect(evidence["left out: skill dropped (acme/home skills/dropped)"]).toContain(
    "acme/home:omp/config.yml at "
  );
  expect(evidence["left out: skill dropped (acme/home skills/dropped)"]).toContain(
    "names dropped in skills.ignoredSkills"
  );
  // quiet and shy are installed, so skill:// reads them, and not listed; unread is listed.
  expect(evidence["not listed: skill quiet"]).toContain("disable-model-invocation: true");
  expect(evidence["not listed: skill shy"]).toContain("hide: true");
  expect(evidence["not listed: skill unread"]).toBeUndefined();
  const measure = ListingMeasure.parse(JSON.parse(evidence[LISTING_MEASURE_ITEM] ?? "{}"));
  expect(measure.added).toContain("unread");
  expect(measure.added).not.toContain("quiet");
  expect(measure.added).not.toContain("shy");
  // A load task's profile of the same session: every copy of the tested source (acme/skills'
  // alpha) is left out, the home's alpha stays under its namespace, and the evidence says the
  // package installs the tested source again (`held once`). One task from the good session's
  // twin /sessions/task-4.jsonl (F1 as the good session has it), rebuilt by --reprofile.
  const from = path.join(scratch, "devbox-home", "tasks.jsonl");
  writeFileSync(
    from,
    `${JSON.stringify({
      id: "t1",
      text: TASK_ROWS[0]?.text,
      label: "need",
      source: "session",
      store: "store_a",
      pathHash: pathHash(`${DEV_SESSIONS}/task-4.jsonl`),
    })}\n`
  );
  writeFileSync(`${from}.summary.json`, JSON.stringify({ command: "build-tasks", skill: "alpha" }));
  const dir = path.join(scratch, "devbox-home", "profiles");
  /** The task built again into `into`, from this test's copies output. */
  const args = (into: string) =>
    reprofileArgs(config, from, into).map((arg) => (arg === copiesDir ? copies : arg));
  expect(await main(args(dir), io([]))).toBe(EXIT.ok);
  const [task] = reprofileSummary(dir).profiles.tasks;
  expect(task?.outcome).toBe("changed");
  const held = task?.differs.added.find((item) => item.item.startsWith("held once: "));
  expect(held?.item).toBe("held once: skill alpha (acme/skills skills/alpha)");
  expect(held?.source).toStartWith(
    "package:@acme/pi-acme ships it (acme/skills:packages/pi-acme/package.json) too, the same copy; a host that had it there at another commit than the first install's lists that one as pi-acme/alpha (NAMESPACE_RULE)"
  );
  const profile = JSON.parse(readFileSync(path.resolve(dir, `${task?.task}.json`), "utf8"));
  const alphas = profile.skills.filter((skill: { name: string }) => skill.name === "alpha");
  expect(alphas).toEqual([
    { repo: "acme/home", commit: SHA, path: "skills/alpha", name: "alpha", namespace: "agent" },
  ]);
  // Catches: skills.ignoredSkills matched against a copy's own name alone, where Oh My Pi 18.18.6
  // matches the name it lists a later copy of a name under too (`<namespace>/<name>`), so a
  // pattern agent/* left the home's alpha listed as agent/alpha. The same task again, with that
  // pattern in the dev box's Oh My Pi config.
  const home = REPOSITORIES["acme/home"] ?? {};
  const homeConfig = home["omp/config.yml"] ?? "";
  home["omp/config.yml"] = "skills:\n  ignoredSkills:\n    - dropped\n    - agent/*\n";
  try {
    const ignoredDir = path.join(scratch, "devbox-home", "profiles-ignored");
    expect(await main(args(ignoredDir), io([]))).toBe(EXIT.ok);
    const [ignored] = reprofileSummary(ignoredDir).profiles.tasks;
    const left = ignored?.differs.added.find(
      (item) => item.item === "left out: skill agent/alpha (acme/home skills/alpha)"
    );
    expect(left?.source).toContain("so Oh My Pi would list it as agent/alpha");
    expect(left?.source).toContain("names agent/* in skills.ignoredSkills");
    const ignoredProfile = JSON.parse(
      readFileSync(path.resolve(ignoredDir, `${ignored?.task}.json`), "utf8")
    );
    expect(
      ignoredProfile.skills.filter((skill: { name: string }) => skill.name === "alpha")
    ).toEqual([]);
  } finally {
    home["omp/config.yml"] = homeConfig;
  }
}, 60_000);

test("a Legion profile installs what its deployment gives each role at its start: the every-role plugin and every other plugin of the catalog its role setup names but those the project leaves out, and the skill its worker image installs from a release", async () => {
  // The suite's copies output, with acme/deploy as the deployment that holds ACME's project file.
  const copies = path.join(setupDir, "copies-deploy");
  mkdirSync(copies, { recursive: true });
  for (const file of [
    "prefix-table.tsv",
    "former-stages.tsv",
    "installers.tsv",
    "loaders.tsv",
    "skill-copies.tsv",
    "repositories.tsv",
  ]) {
    writeFileSync(path.join(copies, file), readFileSync(path.join(copiesDir, file)));
  }
  writeFileSync(
    path.join(copies, "manifests.tsv"),
    readFileSync(path.join(copiesDir, "manifests.tsv"), "utf8").replace(
      "acme/infra\tlegion/projects/10-ACME.yaml",
      "acme/deploy\tlegion/projects/10-ACME.yaml"
    )
  );
  const out = path.join(scratch, "legion-deploy", "cases.jsonl");
  const sessions = sessionsFile("legion-deploy.jsonl", ["legion"]);
  const lines: string[] = [];
  expect(await main(buildArgs(sessions, out, "1", "--copies", copies), io(lines))).toBe(EXIT.ok);
  const [c] = loadCases(out, old);
  const skills = (c?.profile.skills ?? []).map(
    (skill) => `${skill.name} ${skill.repo} ${skill.path} ${skill.commit}`
  );
  expect(skills).toEqual(
    expect.arrayContaining([
      `every-one acme/catalog plugins/every-tools/skills/every-one ${SHA}`,
      `alpha-one acme/catalog plugins/alpha-tools/skills/alpha-one ${SHA}`,
      `memo acme/rel skills/memo ${REL_PIN}`,
    ])
  );
  expect(skills.some((skill) => skill.startsWith("beta-one "))).toBe(false);
  const evidence = Object.fromEntries(c?.evidence?.map((each) => [each.item, each.source]) ?? []);
  expect(
    evidence["unit plugin:alpha-tools@acme-catalog (acme/catalog:.claude-plugin/marketplace.json)"]
  ).toContain(
    "legion/projects/10-ACME.yaml sets verificationSkillsLeaveOut [beta-tools], so each role installs every plugin of the catalog but those, and the every-role plugins (every-tools)"
  );
  expect(evidence["unit image:memo (acme/deploy:legion/worker/Dockerfile)"]).toContain(
    "installs it in the worker image"
  );
  expect(evidence[`commit ${REL_PIN} of acme/rel`]).toStartWith(
    "the last commit of v1.0.0, the ref acme/deploy:legion/worker/Dockerfile pins"
  );
  // The catalog's alpha differs from the alpha the session read: it is installed too, under its
  // plugin's namespace.
  expect(c?.profile.skills).toContainEqual({
    repo: "acme/catalog",
    commit: SHA,
    path: "plugins/alpha-tools/skills/alpha",
    name: "alpha",
    namespace: "alpha-tools",
  });
  expect(
    evidence["skill alpha-tools/alpha (acme/catalog plugins/alpha-tools/skills/alpha)"]
  ).toContain("and this copy's text differs");
}, 60_000);

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
  const logged = psqlLogLength();
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
  expect(psqlLogLength()).toBe(logged);
}, 60_000);

test("build-cases refuses an --out inside a git or jj checkout, its own and --reprofile's, before any read", async () => {
  const from = recorder.requests.length;
  const logged = psqlLogLength();
  const calls = ghCalls.length;
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
  expect(psqlLogLength()).toBe(logged);
  expect(ghCalls.length).toBe(calls);
}, 60_000);

/** The made-up session that reads alpha from the skill set alpha-set: F1 with its reads of alpha
 *  moved to a dev box's Oh My Pi plugins directory, which holds acme/alpha-set under its
 *  dependency's name. setTarget's store holds it; SESSIONS does not. */
const SET_SESSION = "set-read";
const SET_ALPHA_DIR = "/srv/dev/.dotfiles/omp/plugins/node_modules/alpha-set/skills/alpha";

/** The configuration and copies output of a build whose target is the skill set alpha-set
 *  (acme/alpha-set), made once: the configuration names the set and reads the suite's store with
 *  SET_SESSION added; the copies output is the suite's with the set's package added as copies.ts
 *  gives a `github:` dependency's (its prefix row under the dependency's name, its installers row
 *  and its copy, read at the tag a plugins list pins). */
let setFiles: { config: string; copies: string } | undefined;
function setTarget(): { config: string; copies: string } {
  if (setFiles !== undefined) return setFiles;
  const lines = f1Lines();
  setText(lines, 5, () => `Step 3 (case ${SET_SESSION}).`);
  readFrom(SET_ALPHA_DIR)(lines);
  const store = changedStore("set-target", (files) => {
    (files.store_a ?? {})[sessionPath(SET_SESSION)] = lines
      .map((line) => `${JSON.stringify(line)}\n`)
      .join("");
  });
  const setConfig = path.join(setupDir, "set-target", "skill-cuts-sets.json");
  writeFileSync(
    setConfig,
    JSON.stringify({
      ...JSON.parse(readFileSync(store, "utf8")),
      sets: [{ id: "alpha-set", repo: "acme/alpha-set" }],
      dotfilesRepo: "acme/dots",
    })
  );
  const copies = path.join(setupDir, "copies-set");
  mkdirSync(copies, { recursive: true });
  const added: Readonly<Record<string, readonly (readonly (string | number)[])[]>> = {
    "prefix-table.tsv": [
      [
        "**/node_modules/alpha-set/skills/alpha/",
        "acme/alpha-set",
        "skills/alpha",
        "alpha",
        "package:alpha-set (acme/alpha-set:package.json)",
      ],
    ],
    "installers.tsv": [
      [
        "acme/alpha-set",
        "package.json",
        "omp-package",
        "ships",
        "package:alpha-set",
        "omp",
        "alpha",
        "acme/alpha-set",
        "skills/alpha",
        "",
      ],
    ],
    "repositories.tsv": [["acme/alpha-set", "v1.0.0", SHA, "no", "no", "", 3, "whole", 1, ""]],
    "skill-copies.tsv": [["acme/alpha-set", "skills/alpha", "alpha", 10, "no", "no", "", "", ""]],
  };
  for (const file of [
    "former-stages.tsv",
    "installers.tsv",
    "loaders.tsv",
    "manifests.tsv",
    "prefix-table.tsv",
    "repositories.tsv",
    "skill-copies.tsv",
  ]) {
    const rows = (added[file] ?? []).map((row) => `${row.join("\t")}\n`).join("");
    writeFileSync(
      path.join(copies, file),
      `${readFileSync(path.join(copiesDir, file), "utf8")}${rows}`
    );
  }
  setFiles = { config: setConfig, copies };
  return setFiles;
}

/** The arguments of a build-cases run of `sessions` into `out` whose target is the set alpha-set
 *  (setTarget), given as `--set`. */
function setArgs(sessions: string, out: string): string[] {
  const target = setTarget();
  const swap: Readonly<Record<string, string>> = {
    "--cut": "--set",
    "skills-1": "alpha-set",
    [config]: target.config,
    [copiesDir]: target.copies,
  };
  return buildArgs(sessions, out, "1").map((arg) => swap[arg] ?? arg);
}

test("build-cases and --reprofile take a set where they take a cut", async () => {
  const sessions = sessionsFile("set-cases.jsonl", [SET_SESSION]);
  const out = path.join(scratch, "set-cases", "cases.jsonl");
  const built: string[] = [];
  expect(await main(setArgs(sessions, out), io(built))).toBe(EXIT.ok);
  const [c] = loadCases(out, old);
  // The case's cut field holds the set's id, and so do the summary and its inputs.
  expect(c?.cut).toBe("alpha-set");
  expect(c?.id).toStartWith("alpha-set-");
  const summary = JSON.parse(readFileSync(`${out}.summary.json`, "utf8"));
  expect([summary.cut, summary.inputs.cut]).toEqual(["alpha-set", "alpha-set"]);

  // --reprofile finds the case's set by its cut field, and builds its profile again.
  const target = setTarget();
  const reprofile = [
    "build-cases",
    "--config",
    target.config,
    "--old",
    old,
    "--copies",
    target.copies,
    "--count",
    countDir,
    "--reprofile",
    out,
  ];
  const fresh = path.join(scratch, "set-cases", "reprofiled.jsonl");
  const said: string[] = [];
  expect(await main([...reprofile, "--out", fresh], io(said))).toBe(EXIT.ok);
  expect(said.join("")).not.toContain("left out");
  const [again] = loadCases(fresh, old);
  expect(again?.cut).toBe("alpha-set");
  expect(again?.profile.skills).toEqual(c?.profile.skills ?? []);

  // One of --cut and --set is taken: both, neither, an id of the other kind or of no set, and
  // --set with --reprofile are refused before any read.
  const from = recorder.requests.length;
  const logged = psqlLogLength();
  const calls = ghCalls.length;
  const refused = path.join(scratch, "set-refused", "cases.jsonl");
  const swapped = (args: string[], was: string, now: string) =>
    args.map((arg) => (arg === was ? now : arg));
  const rows: [string[], string][] = [
    [
      [...setArgs(sessions, refused), "--cut", "skills-1"],
      "--cut skills-1 and --set alpha-set are both given: give one of the two",
    ],
    [
      setArgs(sessions, refused).filter(
        (arg, at, args) => arg !== "--set" && args[at - 1] !== "--set"
      ),
      "--cut or --set required",
    ],
    [
      swapped(setArgs(sessions, refused), "alpha-set", "skills-1"),
      "--set skills-1 is no set of the configuration (alpha-set)",
    ],
    [
      swapped(setArgs(sessions, refused), "--set", "--cut"),
      "--cut alpha-set is no cut of the configuration (skills-1)",
    ],
    [
      swapped(
        swapped(buildArgs(sessions, refused, "1"), "--cut", "--set"),
        "skills-1",
        "alpha-set"
      ),
      "--set alpha-set is no set of the configuration (none)",
    ],
    [[...reprofile, "--out", refused, "--set", "alpha-set"], "so it takes no --set"],
  ];
  for (const [args, why] of rows) {
    const lines: string[] = [];
    expect(await main(args, io(lines))).toBe(EXIT.refused);
    expect(lines.join("\n")).toContain(why);
  }
  expect(existsSync(refused)).toBe(false);
  expect(recorder.requests.length).toBe(from);
  expect(psqlLogLength()).toBe(logged);
  expect(ghCalls.length).toBe(calls);
}, 60_000);

test("a set target keeps a read placed at the set's copy and drops a Legion fork's read as not-cut-copy", async () => {
  const sessions = sessionsFile("set-target.jsonl", [SET_SESSION, "legion"]);
  const out = path.join(scratch, "set-target", "cases.jsonl");
  expect(await main(setArgs(sessions, out), io([]))).toBe(EXIT.ok);
  const cases = loadCases(out, old);
  expect(cases.map((c) => c.source.pathHash)).toEqual([pathHash(sessionPath(SET_SESSION))]);
  // The profile installs the copy the session read: the set's alpha, not acme/skills's.
  expect(cases[0]?.profile.skills.filter((skill) => skill.name === "alpha")).toEqual([
    { repo: "acme/alpha-set", commit: SHA, path: "skills/alpha", name: "alpha" },
  ]);
  // The legion session read alpha from the package the Legion image carries: acme/skills's fork
  // of the skill, which the prefix table places at acme/skills's copy.
  const record = summaryOf(out).sessions.find(
    (each) => each.hash === fileHash(sessionPath("legion"))
  );
  expect(record).toMatchObject({
    reason: "not-cut-copy",
    detail:
      "the prefix table rule places /opt/legion/pi-acme/dist/skills/alpha/SKILL.md at acme/skills skills/alpha",
    readerUsd: 0,
  });
}, 60_000);

// build-cases --written: cases of a skill from written tasks, each in a recorded session's context.

/** The copies output of the written builds: the suite's, with acme/work listed, so F1's working
 *  directory F1_CWD, named work, is a checkout of acme/work at SHA, and a context session made from
 *  F1 is one the written build knows a checkout of; one in no checkout it knows is passed over
 *  (no-checkout). */
function writtenCopies(): string {
  return copiesWith("copies-written", {
    "repositories.tsv": [["acme/work", "main", SHA, "no", "no", "", 1, "recursive", 0, ""]],
  });
}

/** The arguments of one `build-cases --written 3` run of alpha, and `extra`. */
function writtenArgs(out: string, ...extra: string[]): string[] {
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
    "--copies",
    writtenCopies(),
    "--count",
    countDir,
    "--written",
    "3",
    "--out",
    out,
    ...extra,
  ];
}

/** The navigation lines a session build of alpha found: SKILL.md's line 8, NAVIGATION_LINE. */
const SESSION_NAVIGATION: NavigationCounts["lines"] = [
  { file: "SKILL.md", number: 8, text: NAVIGATION_LINE, dropped: 1 },
];

/** A cases file of one session case of alpha: F1's `task` step, which LINE governs. With
 *  `navigation`, a file of its own, beside which the session build's summary
 *  (`<file>.summary.json`) names those navigation lines. */
function sessionCasesFile(navigation?: NavigationCounts["lines"]): string {
  const messages = f1Messages();
  const file = path.join(
    setupDir,
    navigation === undefined ? "session-cases.jsonl" : "session-cases-navigation.jsonl"
  );
  const c = {
    id: "skills-1-f1",
    cut: "skills-1",
    skill: "alpha",
    baseFile: "SKILL.md",
    line: { text: LINE, lineNumber: 9, lineCount: 10 },
    criterion: CRITERION,
    model: "anthropic/claude-sonnet-5-5",
    thinking: "off",
    context: "devbox",
    profile: {
      skills: [{ repo: "acme/skills", commit: SHA, path: "skills/alpha", name: "alpha" }],
      plugins: [],
      contextFiles: [],
    },
    messages: messages.slice(0, 7),
    skillReads: [2, 4],
    realStep: messages[7],
    violatingStep: { role: "assistant", content: [{ type: "text", text: "Step 4." }] },
    source: { store: "store_a", pathHash: pathHash("/sessions/f1.jsonl"), kind: "session" },
    sessionStart: "2026-11-20T01:59:41.780Z",
    secretsRemoved: 0,
    sessionDigest: null,
  };
  writeFileSync(file, `${JSON.stringify(c)}\n`);
  if (navigation !== undefined) {
    const dropped = navigation.reduce((sum, line) => sum + line.dropped, 0);
    writeFileSync(
      `${file}.summary.json`,
      JSON.stringify({
        command: "build-cases",
        cut: "skills-1",
        skill: "alpha",
        navigation: { lines: navigation, dropped, withReplacement: 0, withoutReplacement: dropped },
      })
    );
  }
  return file;
}

/** A written build's summary, as the tests read it. */
interface WrittenSummary {
  command: string;
  contextSession: {
    store: string;
    hash: string;
    pathHash: string;
    how: string;
    passed: { store: string; hash: string; why: string }[];
  };
  model: { model: string; how: string };
  casesBuilt: number;
  drops: Partial<Record<DropReason, number>>;
  tasks: {
    task: number;
    outcome: string;
    reason?: DropReason;
    detail?: string;
    existing?: string[];
  }[];
  stopped: string | null;
  readerSpend: { models: Record<string, { calls: number }>; usd: number };
  peakRssBytes?: number;
}

/** The user text of each request whose system prompt starts `prompt`. */
const usersOf = (requests: readonly RecordedRequest[], prompt: string): string[] =>
  requests.flatMap((request) => {
    const body = JSON.parse(request.body) as {
      system: { text: string }[];
      messages: { content: { text: string }[] }[];
    };
    return body.system[0]?.text.startsWith(prompt) === true
      ? [body.messages[0]?.content.map((part) => part.text).join("\n") ?? ""]
      : [];
  });

describe("build-cases --written", () => {
  const out = path.join(scratch, "written", "cases.jsonl");
  const printed: string[] = [];
  const contextHash = fileHash(sessionPath("good"));
  const id = `skills-1-${pathHash(sessionPath("good")).slice(0, 12)}-w1`;
  let code = -1;
  let requests: RecordedRequest[] = [];
  beforeAll(async () => {
    const from = recorder.requests.length;
    code = await main(
      writtenArgs(
        out,
        "--context-session",
        `store_a:${contextHash}`,
        "--session-cases",
        sessionCasesFile(SESSION_NAVIGATION)
      ),
      io(printed)
    );
    requests = recorder.requests.slice(from);
  }, 60_000);

  test("a written case has the task, the read and its result, and source.kind written", () => {
    expect({ code, failed: printed.join("").includes("failed") }).toEqual({
      code: 0,
      failed: false,
    });
    const messages = f1Messages();
    // The reader read the base text, the session case's step and criterion, the context session's
    // first user message and the count.
    const written = usersOf(requests, WRITTEN_PROMPT);
    expect(written).toHaveLength(1);
    const [asked] = written;
    expect(asked).toContain(`9: ${LINE}`);
    expect(asked).toContain(
      `<session-cases>\n<case n="1">\n<step>\n${stepText(messages[7]?.content ?? [])}\n</step>\n<criterion>\nPass: ${CRITERION.pass}\nFail: ${CRITERION.fail}\n</criterion>`
    );
    expect(asked).toContain(
      `<first-message>\n${messages[0]?.content?.[0]?.text}\n</first-message>\n\n<count>3</count>`
    );

    const cases = loadCases(out, old);
    expect(cases.map((each) => each.id)).toEqual([id]);
    const [c] = cases;
    expect(c?.source).toEqual({
      store: "store_a",
      pathHash: pathHash(sessionPath("good")),
      kind: "written",
    });
    expect(c === undefined ? undefined : caseKind(c)).toBe("written");
    // The task as the user's message, a read of skill://alpha, and its result: the old text at the
    // path the context session's own read of alpha showed, as F1's read result holds it.
    expect(c?.messages).toHaveLength(3);
    expect(c?.messages[0]).toEqual({
      role: "user",
      content: [{ type: "text", text: WRITTEN_TASK.task }],
    });
    expect(c?.messages[1]).toMatchObject({
      role: "assistant",
      content: [
        {
          type: "toolCall",
          id: "written_read",
          name: "read",
          arguments: { path: "skill://alpha" },
        },
      ],
    });
    expect(c?.messages[1]?.content).toHaveLength(1);
    expect(c?.messages[2]?.content).toEqual(messages[2]?.content);
    expect(c?.messages[2]).toMatchObject({ role: "toolResult", toolCallId: "written_read" });
    expect(c?.skillReads).toEqual([2]);
    // The reader's following step is the real step; its violating step, the violating one.
    expect(c?.realStep).toEqual({
      role: "assistant",
      content: [
        { type: "toolCall", id: "following_1", ...WRITTEN_TASK.followingStep.toolCalls[0] },
      ],
    });
    expect(c?.violatingStep).toEqual({
      role: "assistant",
      content: [
        { type: "toolCall", id: "violating_1", ...WRITTEN_TASK.violatingStep.toolCalls[0] },
      ],
    });
    expect([c?.baseFile, c?.line, c?.criterion]).toEqual([
      "SKILL.md",
      { text: LINE, lineNumber: 9, lineCount: 10 },
      CRITERION,
    ]);
    // The context session's model, thinking level, context, start and digest at its read of the
    // skill, and its profile, as a session case of it has them.
    expect([c?.model, c?.thinking, c?.context, c?.sessionStart, c?.sessionDigest]).toEqual([
      "anthropic/claude-sonnet-5-5",
      "off",
      "devbox",
      "2026-11-20T01:59:41.780Z",
      "46874d907e557998",
    ]);
    expect(c?.profile.skills.filter((skill) => skill.name === "alpha")).toEqual([
      { repo: "acme/skills", commit: SHA, path: "skills/alpha", name: "alpha" },
    ]);
    const items = (c?.evidence ?? []).map((each) => each.item);
    expect(items).toContain(
      `context session read: message 2 reads [Skill file: ${F1_SKILLS}/alpha/SKILL.md]`
    );
    expect(items).toContain("audit of the real step: follows");
    expect(items).toContain(LISTING_MEASURE_ITEM);
    expect(items).toContain(`written case: task 1 of the written-case reader`);
    expect(items).toContain(`written case: context session store_a:${contextHash}`);
    expect(items).toContain("written case: model anthropic/claude-sonnet-5-5");

    const summary = summaryOf<WrittenSummary>(out);
    expect(summary).toMatchObject({
      command: "build-cases --written",
      contextSession: {
        store: "store_a",
        hash: contextHash,
        pathHash: pathHash(sessionPath("good")),
        how: "--context-session",
        passed: [],
      },
      model: { model: "anthropic/claude-sonnet-5-5", how: "the context session's own" },
      casesBuilt: 1,
    });
  });

  test("a written case whose task restates the line or whose following step the audit does not call follows is dropped", () => {
    const summary = summaryOf<WrittenSummary>(out);
    expect(summary.tasks.map((each) => [each.task, each.outcome, each.reason ?? null])).toEqual([
      [1, `case ${id}`, null],
      [2, "dropped", "written-restates"],
      [3, "dropped", "written-not-followed"],
    ]);
    expect(summary.drops).toEqual({ "written-restates": 1, "written-not-followed": 1 });
    expect(summary.tasks[1]?.detail).toContain(RESTATED);
    expect(summary.tasks[2]?.detail).toBe("breaks: The step spreads the result over two lines.");
    // The restatement check read each task and its read call, never the skill's text.
    const restates = usersOf(requests, RESTATE_PROMPT);
    expect(restates).toHaveLength(3);
    for (const [n, user] of restates.entries()) {
      expect(user).toStartWith(
        `<line>\n${LINE}\n</line>\n\n<messages>\n<message index="0" role="user">\n[user]\n${WRITTEN_TASKS[n]?.task}\n</message>`
      );
      expect(user).not.toContain("3. Stop after the third step.");
    }
    // The audit read the line, the history as the judge reads it and the following step: only
    // for the tasks that do not restate the line.
    const audits = usersOf(requests, AUDIT_PROMPT);
    expect(audits).toHaveLength(2);
    expect(audits[0]).toBe(
      `<line>\n${LINE}\n</line>\n\n<before>\n[user]\n${WRITTEN_TASK.task}\n\n[assistant]\n[tool call: read] {"path":"skill://alpha"}\n\n[tool result: read]\n[the text of a file of the skill, left out]\n</before>\n\n<step>\n[tool call: write] ${JSON.stringify(WRITTEN_TASK.followingStep.toolCalls[0]?.arguments)}\n</step>`
    );
  });

  // Catches: a build without --drop-navigation that hands the written-case reader the navigation
  // lines of the session build, or puts a read beyond the skill's into a written case.
  test("without --drop-navigation, the written-case reader's <navigation> is None. though the session build's summary names a navigation line, and the case reads only the skill", () => {
    const [asked] = usersOf(requests, WRITTEN_PROMPT);
    expect(asked).toContain("</skill>\n\n<navigation>\nNone.\n</navigation>\n\n<session-cases>\n");
    expect(asked).not.toContain(`<line file="SKILL.md" number="8">`);
    const [c] = loadCases(out, old);
    expect([c?.id, c?.messages.length, c?.skillReads, c?.baseFile]).toEqual([
      id,
      3,
      [2],
      "SKILL.md",
    ]);
    expect(summaryOf<WrittenSummary>(out).tasks.map((each) => each.existing)).toEqual([[], [], []]);
  });

  test("--reprofile keeps source.kind", async () => {
    const fresh = path.join(scratch, "written", "reprofiled.jsonl");
    const lines: string[] = [];
    const args = [
      "build-cases",
      "--config",
      config,
      "--old",
      old,
      "--copies",
      writtenCopies(),
      "--count",
      countDir,
      "--reprofile",
      out,
      "--out",
      fresh,
    ];
    expect(await main(args, io(lines))).toBe(EXIT.ok);
    const [before] = loadCases(out, old);
    const [after] = loadCases(fresh, old);
    expect(after?.source).toEqual(before?.source);
    expect(after === undefined ? undefined : caseKind(after)).toBe("written");
    // Its profile is built again from the context session's reads its evidence names, which its
    // messages do not hold, so it is the profile the build gave; the written case's evidence stays.
    expect(after?.profile).toEqual(before?.profile);
    const written = (c: Case | undefined) =>
      (c?.evidence ?? []).filter((each) =>
        /^(written case|context session read): /.test(each.item)
      );
    expect(written(after)).toEqual(written(before));
    expect(written(after).length).toBeGreaterThan(0);
  }, 60_000);

  test("a written build under the memory limit records its peak resident memory in the summary and prints it in its last line", () => {
    const summary = summaryOf<WrittenSummary>(out);
    expect(summary.stopped).toBeNull();
    expect(summary.peakRssBytes).toBe(PEAK_BYTES);
    expect(printed.at(-1)).toContain(
      `of readers at list price, peak memory 200 MiB; written to ${out} and ${out}.summary.json`
    );
  });
});

// Catches: a written task whose line only sends the agent to a file of the skill kept as a case
// under --drop-navigation, which the arm with no skill fails by construction; a task dropped only
// because its following step reads a file of the skill; and a navigation line left out of the
// summary.
test("build-cases --written --drop-navigation drops a task whose line the navigation reader calls navigation as written-navigation, lists that line in the summary, and keeps a task whose following step reads a file of the skill under a line that is not one", async () => {
  const out = path.join(scratch, "written-navigation", "cases.jsonl");
  const context = `store_a:${fileHash(sessionPath("good"))}`;
  const args = writtenArgs(out, "--context-session", context, "--drop-navigation");
  args[args.indexOf("--written") + 1] = "5";
  const printed: string[] = [];
  const from = recorder.requests.length;
  expect(await main(args, io(printed))).toBe(EXIT.ok);
  const requests = recorder.requests.slice(from);
  const summary = summaryOf<
    WrittenSummary & { inputs: { dropNavigation: boolean }; navigation: NavigationCounts }
  >(out);
  expect(summary.inputs.dropNavigation).toBe(true);
  const id = `skills-1-${pathHash(sessionPath("good")).slice(0, 12)}`;
  expect(summary.tasks.map((each) => [each.task, each.outcome, each.reason ?? null])).toEqual([
    [1, `case ${id}-w1`, null],
    [2, "dropped", "written-restates"],
    [3, "dropped", "written-not-followed"],
    [4, "dropped", "written-navigation"],
    [5, `case ${id}-w5`, null],
  ]);
  expect(summary.drops).toEqual({
    "written-restates": 1,
    "written-not-followed": 1,
    "written-navigation": 1,
  });
  expect(summary.tasks[3]?.detail).toContain(NAVIGATION_REASON);
  expect(summary.navigation).toEqual({
    lines: [{ file: "SKILL.md", number: 8, text: NAVIGATION_LINE, dropped: 1 }],
    dropped: 1,
    withReplacement: 0,
    withoutReplacement: 1,
  });
  // The kept task's following step reads references/x.md.
  const kept = loadCases(out, old).find((each) => each.id === `${id}-w5`);
  expect(kept?.realStep.content).toContainEqual(
    expect.objectContaining({ name: "read", arguments: { path: "skill://alpha/references/x.md" } })
  );
  // The navigation reader judged each task's line once, before its restatement check: tasks 1, 2,
  // 3 and 5 share SKILL.md's line 9, and take its one verdict.
  expect(usersOf(requests, NAVIGATION_PROMPT)).toEqual([
    `<files>\nSKILL.md\nreferences/x.md\n</files>\n\n<file>\nSKILL.md\n</file>\n\n<line>\n${LINE}\n</line>`,
    `<files>\nSKILL.md\nreferences/x.md\n</files>\n\n<file>\nSKILL.md\n</file>\n\n<line>\n${NAVIGATION_LINE}\n</line>`,
  ]);
  expect(usersOf(requests, RESTATE_PROMPT)).toHaveLength(4);
}, 60_000);

// Catches: a written case whose history holds a read beyond the skill's (a step the agent takes
// itself in the replay, where the skill-read rounds answer it), the session build's navigation
// lines kept from the written-case reader, or a task under a line of a file they send the agent to
// refused.
test("build-cases --written --drop-navigation gives the written-case reader the session build's navigation lines, and a task under a line of references/x.md gives a case whose history holds the task and the read of the skill alone", async () => {
  const out = path.join(scratch, "written-reads", "cases.jsonl");
  const context = `store_a:${fileHash(sessionPath("good"))}`;
  const casesFile = sessionCasesFile(SESSION_NAVIGATION);
  const args = writtenArgs(
    out,
    "--context-session",
    context,
    "--session-cases",
    casesFile,
    "--drop-navigation"
  );
  const printed: string[] = [];
  const from = recorder.requests.length;
  expect(await main(args, io(printed))).toBe(EXIT.ok);
  const requests = recorder.requests.slice(from);
  // The reader read SKILL.md's line 8, the navigation line of the summary beside --session-cases.
  const [asked] = usersOf(requests, WRITTEN_PROMPT);
  expect(asked).toContain(
    `</skill>\n\n<navigation>\n<line file="SKILL.md" number="8">\n${NAVIGATION_LINE}\n</line>\n</navigation>\n\n<session-cases>\n`
  );

  const messages = f1Messages();
  const id = `skills-1-${pathHash(sessionPath("good")).slice(0, 12)}-w1`;
  const cases = loadCases(out, old);
  expect(cases.map((each) => each.id)).toEqual([id]);
  const [c] = cases;
  // The task, the read of skill://alpha and its result, and nothing else: the agent reads
  // references/x.md itself in the replay.
  expect(c?.messages).toHaveLength(3);
  expect(c?.messages[0]).toEqual({
    role: "user",
    content: [{ type: "text", text: WRITTEN_REFERENCE_TASK.task }],
  });
  expect(c?.messages[1]).toMatchObject({
    role: "assistant",
    content: [
      { type: "toolCall", id: "written_read", name: "read", arguments: { path: "skill://alpha" } },
    ],
  });
  expect(c?.messages[1]?.content).toHaveLength(1);
  expect(c?.messages[2]).toMatchObject({ role: "toolResult", toolCallId: "written_read" });
  expect(c?.messages[2]?.content).toEqual(messages[2]?.content);
  expect(c?.skillReads).toEqual([2]);
  expect([c?.baseFile, c?.line, c?.criterion]).toEqual([
    "references/x.md",
    { text: X_LINE, lineNumber: 3, lineCount: 3 },
    WRITTEN_REFERENCE_TASK.criterion,
  ]);
  expect((c?.evidence ?? []).some((each) => each.item.includes(" reads skill://"))).toBe(false);

  // The navigation reader judged the task's line; the restatement check read the task and the
  // read call, never the skill's text; the audit read the read with its text left out.
  expect(usersOf(requests, NAVIGATION_PROMPT)).toEqual([
    `<files>\nSKILL.md\nreferences/x.md\n</files>\n\n<file>\nreferences/x.md\n</file>\n\n<line>\n${X_LINE}\n</line>`,
  ]);
  const [restate] = usersOf(requests, RESTATE_PROMPT);
  expect(restate).toContain(`<message index="1" role="assistant">\n[assistant]\n[tool call: read]`);
  expect(restate?.includes('<message index="2"')).toBe(false);
  const left = "[tool result: read]\n[the text of a file of the skill, left out]";
  expect(usersOf(requests, AUDIT_PROMPT)).toEqual([
    `<line>\n${X_LINE}\n</line>\n\n<before>\n[user]\n${WRITTEN_REFERENCE_TASK.task}\n\n[assistant]\n[tool call: read] {"path":"skill://alpha"}\n\n${left}\n</before>\n\n<step>\n[tool call: write] ${JSON.stringify(WRITTEN_REFERENCE_TASK.followingStep.toolCalls[0]?.arguments)}\n</step>`,
  ]);
  const summary = summaryOf<WrittenSummary>(out);
  expect(summary.tasks.map((each) => [each.task, each.outcome, each.existing])).toEqual([
    [1, `case ${id}`, []],
  ]);
}, 60_000);

/** A written-case answer file of `tasks`, for `--written-answer`. */
function writtenAnswerFile(name: string, tasks: readonly Record<string, unknown>[]): string {
  const file = path.join(setupDir, `${name}.json`);
  writeFileSync(file, JSON.stringify({ tasks }));
  return file;
}

// Catches (the check's negative control): a written task that treats as already there a path its
// context session's checkout does not hold kept as a case, or one that asks the agent to create
// that path refused for it; an answer file that is not read in place of the reader, or that the
// summary does not name.
test("build-cases --written --written-answer drops a task that treats a missing path as there (written-missing-path) and keeps one that asks to create it, with the checkout on the case and the history the skill's read alone", async () => {
  const out = path.join(scratch, "written-answer", "cases.jsonl");
  const context = `store_a:${fileHash(sessionPath("proj-devbox"))}`;
  const farm = "tools/tests/test_farm.py";
  const answer = writtenAnswerFile("written-answer", [
    { ...WRITTEN_TASK, task: `Make ${farm} pass again.`, existing: [farm] },
    {
      ...WRITTEN_TASK,
      task: `Add a test of the farm in a new ${farm}, as CLAUDE.md asks of new tests.`,
      existing: ["CLAUDE.md"],
    },
  ]);
  const args = writtenArgs(out, "--context-session", context, "--written-answer", answer);
  const printed: string[] = [];
  const from = recorder.requests.length;
  expect(await main(args, io(printed))).toBe(EXIT.ok);
  const requests = recorder.requests.slice(from);
  expect(usersOf(requests, WRITTEN_PROMPT)).toEqual([]);
  const summary = summaryOf<
    WrittenSummary & {
      inputs: { writtenAnswer: { path: string; sha256: string } | null };
      tasksFrom: string;
    }
  >(out);
  expect(summary.inputs.writtenAnswer).toEqual({
    path: answer,
    sha256: createHash("sha256").update(readFileSync(answer)).digest("hex"),
  });
  expect(summary.tasksFrom).toBe(`--written-answer ${answer}`);
  const id = `skills-1-${pathHash(sessionPath("proj-devbox")).slice(0, 12)}-w2`;
  expect(summary.tasks.map((each) => [each.task, each.outcome, each.reason ?? null])).toEqual([
    [1, "dropped", "written-missing-path"],
    [2, `case ${id}`, null],
  ]);
  expect(summary.tasks[0]?.detail).toBe(
    `it treats ${farm} as there, and acme/proj at ${SHA} holds no ${farm}`
  );
  expect(summary.drops).toEqual({ "written-missing-path": 1 });
  const [c] = loadCases(out, old);
  // The context session ran no jj.
  expect(c?.checkout).toEqual({
    repo: "acme/proj",
    commit: SHA,
    root: "/home/dev/src/proj",
    cwd: "/home/dev/src/proj",
    jj: false,
  });
  expect(
    c?.messages.flatMap((message) =>
      (message.content ?? []).flatMap((block) => (block.type === "toolCall" ? [block.id] : []))
    )
  ).toEqual(["written_read"]);
  const items = (c?.evidence ?? []).map((each) => each.item);
  expect(items).toContain(`written case: task 2 of --written-answer ${answer}`);
  expect(items).toContain("written case: treats CLAUDE.md as there");
  expect(items).toContain(`written case: checkout acme/proj at ${SHA}`);
}, 60_000);

// Catches: a written case whose context session ran jj recorded as one that ran none (its own
// history, the task and the skill's read, holds no jj call), so its runs answer its jj calls as jj
// does in a checkout that is no jj repository, an error that session never saw.
test("build-cases --written records on the case's checkout whether its context session ran jj: a session with a bash call of jj status records jj true, and its evidence says so", async () => {
  const file = "/sessions/proj-jj.jsonl";
  const text = sessionText("proj-jj", (lines) => {
    workingDirectory("/home/dev/src/proj")(lines);
    callAfterPrompt(
      "b0b0b00",
      "toolu_jj",
      "bash",
      { command: "cd /home/dev/src/proj && jj status" },
      "The working copy has no changes."
    )(lines);
  });
  const store = changedStore("proj-jj", (files) => {
    (files.store_a ?? {})[file] = text;
  });
  const out = path.join(scratch, "written-jj", "cases.jsonl");
  const answer = writtenAnswerFile("written-jj", [
    {
      ...WRITTEN_TASK,
      task: "Add a test of the farm, as CLAUDE.md asks.",
      existing: ["CLAUDE.md"],
    },
  ]);
  const args = writtenArgs(
    out,
    "--context-session",
    `store_a:${fileHash(file)}`,
    "--written-answer",
    answer
  );
  args[args.indexOf("--config") + 1] = store;
  expect(await main(args, io([]))).toBe(EXIT.ok);
  const [c] = loadCases(out, old);
  expect(c?.checkout).toEqual({
    repo: "acme/proj",
    commit: SHA,
    root: "/home/dev/src/proj",
    cwd: "/home/dev/src/proj",
    jj: true,
  });
  expect(
    c?.evidence?.find((each) => each.item === `written case: checkout acme/proj at ${SHA}`)?.source
  ).toContain("a bash call of the context session ran jj, so its checkout was a jj workspace");
}, 60_000);

// Catches: a written-case reader given only the root's entries of a large checkout, so it cannot see
// what exists where it will work (the defect: all 8 tasks of a build on a tree of 115,950 paths were
// dropped written-missing-path).
test("partialListing gives a large tree's directories, shallowest first, then the files under each directory the first message names and under the working directory, each part cut at its cap and said so", () => {
  const data = Array.from({ length: 2_100 }, (_, n) => `data/f${String(n).padStart(4, "0")}.json`);
  const listed = [
    "README.md",
    "proj/",
    "proj/src/",
    "proj/src/app.py",
    "proj/README.md",
    "tools/",
    "tools/farm/",
    "tools/farm/gen.py",
    "tools/farm/README.md",
    "other/",
    "other/deep/",
    "other/deep/x.py",
    "data/",
    ...data,
  ];
  const message = "Fix the farm's batch script, tools/farm/gen-batch.py, and run it.";
  expect(partialListing(listed, true, "proj", message)).toEqual({
    files: [
      "data/",
      "other/",
      "proj/",
      "tools/",
      "other/deep/",
      "proj/src/",
      "tools/farm/",
      "tools/farm/README.md",
      "tools/farm/gen.py",
      "proj/README.md",
      "proj/src/app.py",
    ],
    holds:
      "the tree's directories, shallowest first (7 of 7), then the files under tools/farm, which the first message names, and under the working directory proj (4 of 4)",
  });
  // Past the caps, and from a tree GitHub cut short: each part cut, and the listing says so.
  const dirs = Array.from({ length: 5_100 }, (_, n) => `d${String(n).padStart(4, "0")}/`);
  const files = Array.from({ length: 3_100 }, (_, n) => `f${String(n).padStart(4, "0")}.txt`);
  const cut = partialListing([...dirs, ...files], false, "", "Tidy the repository.");
  expect(cut.files).toEqual([...dirs.slice(0, 5_000), ...files.slice(0, 3_000)]);
  expect(cut.holds).toBe(
    "the tree's directories, shallowest first (5000 of 5100, the rest cut), then the files under the working directory, the checkout's root (3000 of 3100, the rest cut); GitHub cut the tree short at 8200 paths, so both parts may lack paths past them"
  );
});

// Catches: a written build whose every task treats a path the commit lacks as there (the context
// session worked on a branch where it exists), with no second answer that knows it is absent; or
// a --written-answer asked again.
test("build-cases --written asks the written-case reader once more when every task of its answer treats a missing path as there, naming those paths as absent, and keeps the second answer's tasks; the summary records the retry and both answers' outcomes; a --written-answer gets no retry", async () => {
  const out = path.join(scratch, "written-retry", "cases.jsonl");
  const context = `store_a:${fileHash(sessionPath("proj-devbox"))}`;
  const from = recorder.requests.length;
  expect(await main(writtenArgs(out, "--context-session", context), io([]))).toBe(EXIT.ok);
  const asked = usersOf(recorder.requests.slice(from), WRITTEN_PROMPT);
  expect(asked).toHaveLength(2);
  expect(asked[0]).not.toContain("<absent>");
  expect(asked[1]).toContain(
    `</checkout>\n\n<absent>\n${PROJ_FARM}\n</absent>\n\n<first-message>\n`
  );
  const summary = summaryOf<{
    retry: unknown;
    tasksWritten: number;
    tasks: { task: number; answer: number; outcome: string; reason?: string }[];
  }>(out);
  const id = `skills-1-${pathHash(sessionPath("proj-devbox")).slice(0, 12)}-w3`;
  expect(
    summary.tasks.map((each) => [each.task, each.answer, each.outcome, each.reason ?? null])
  ).toEqual([
    [1, 1, "dropped", "written-missing-path"],
    [2, 1, "dropped", "written-missing-path"],
    [3, 2, `case ${id}`, null],
  ]);
  expect({ written: summary.tasksWritten, retry: summary.retry }).toEqual({
    written: 2,
    retry: { absent: [PROJ_FARM], tasksWritten: 1 },
  });
  expect(loadCases(out, old).map((each) => each.id)).toEqual([id]);

  // The same tasks given in a file are not asked again.
  const answer = writtenAnswerFile(
    "written-retry-answer",
    [1, 2].map((n) => ({
      ...WRITTEN_TASK,
      task: `Make ${PROJ_FARM} pass again (${n}).`,
      existing: [PROJ_FARM],
    }))
  );
  const given = path.join(scratch, "written-retry-given", "cases.jsonl");
  const before = recorder.requests.length;
  const args = writtenArgs(given, "--context-session", context, "--written-answer", answer);
  expect(await main(args, io([]))).toBe(EXIT.ok);
  expect(usersOf(recorder.requests.slice(before), WRITTEN_PROMPT)).toEqual([]);
  expect(summaryOf<{ retry: unknown }>(given).retry).toBeNull();
}, 60_000);

// Catches: a build that lists the files of a linked --old (its files symbolic links, as the arms
// take one) other than the behaviour report's line does (cli.test.ts, "report names the screened
// cases whose real step reads a file"), so its navigation reader, its written-case reader and the
// check of its answer miss a file the report counts.
test("build-cases --written --drop-navigation lists the files of an --old whose files are symbolic links, as the behaviour report does: the navigation reader reads both, and the task under a line of the linked reference is a case", async () => {
  const linked = path.join(setupDir, "old-linked", "alpha");
  mkdirSync(path.join(linked, "references"), { recursive: true });
  symlinkSync(path.join(old, "SKILL.md"), path.join(linked, "SKILL.md"));
  symlinkSync(path.join(old, "references", "x.md"), path.join(linked, "references", "x.md"));
  const out = path.join(scratch, "written-linked", "cases.jsonl");
  const context = `store_a:${fileHash(sessionPath("good"))}`;
  const answer = writtenAnswerFile("written-linked", [WRITTEN_REFERENCE_TASK]);
  const args = writtenArgs(
    out,
    "--context-session",
    context,
    "--written-answer",
    answer,
    "--drop-navigation"
  );
  args[args.indexOf("--old") + 1] = linked;
  const printed: string[] = [];
  const from = recorder.requests.length;
  expect(await main(args, io(printed))).toBe(EXIT.ok);
  expect(usersOf(recorder.requests.slice(from), NAVIGATION_PROMPT)).toEqual([
    `<files>\nSKILL.md\nreferences/x.md\n</files>\n\n<file>\nreferences/x.md\n</file>\n\n<line>\n${X_LINE}\n</line>`,
  ]);
  expect(loadCases(out, linked).map((each) => [each.baseFile, each.line.text])).toEqual([
    ["references/x.md", X_LINE],
  ]);
}, 60_000);

// Catches: written tasks taken from a session whose work is on the skill's own text, which tests
// nothing the skill governs.
test("build-cases --written passes over a context session the written-case reader finds works on the skill's own text as skill-as-material, takes the next one, and refuses a --context-session of it", async () => {
  const names = ["material-writing", "good"];
  const sessions = sessionsFile("written-material.jsonl", names);
  const candidates = names.map((name) => ({
    store: "store_a",
    hash: fileHash(sessionPath(name)),
    path: sessionPath(name),
  }));
  const seed = Array.from({ length: 100 }, (_, n) => String(n)).find(
    (each) => blindOrder(candidates, each)[0]?.item.path === sessionPath("material-writing")
  );
  expect(seed).toBeDefined();
  const out = path.join(scratch, "written-material", "cases.jsonl");
  expect(await main(writtenArgs(out, "--sessions", sessions, "--seed", seed ?? ""), io([]))).toBe(
    EXIT.ok
  );
  const summary = summaryOf<WrittenSummary>(out);
  expect(summary.contextSession.hash).toBe(fileHash(sessionPath("good")));
  expect(summary.contextSession.passed).toEqual([
    {
      store: "store_a",
      hash: fileHash(sessionPath("material-writing")),
      why: "skill-as-material: The session rewrites alpha's SKILL.md (its first message).",
    },
  ]);
  expect(loadCases(out, old).length).toBeGreaterThan(0);
  const refused: string[] = [];
  const named = path.join(scratch, "written-material-named", "cases.jsonl");
  const context = `store_a:${fileHash(sessionPath("material-writing"))}`;
  expect(await main(writtenArgs(named, "--context-session", context), io(refused))).toBe(
    EXIT.failed
  );
  expect(refused.join("")).toContain(`--context-session ${context}: skill-as-material: `);
}, 120_000);

/** Why a written build passes a context session over as `reason` when the written-case reader's
 *  answer was refused on each try, for `refusals`, one a try (ReaderFailed's message). */
const writtenReaderWhy = (
  reason: "reader-refused" | "reader-failed",
  refusals: readonly string[]
): string =>
  `${reason}: the written-case reader's answer was refused on each of ${refusals.length} tries: ${refusals.join(" | ")}`;

// Catches: a written build ended (exit 1) by the model's refusal of one context session's material
// (the defect: a skill's written build exited 1, the written-case reader's every reply, on each of
// 3 tries, stopped at refusal), which is that session's, not the build's, while the next session of
// the seeded order gives tasks; and a session only some of whose reader replies stopped at refusal
// (the others end their turn with no JSON) passed over as reader-refused, not as reader-failed.
test("build-cases --written passes over a context session whose every written-case reader reply stopped at refusal as reader-refused, takes the next one, and refuses a --context-session of it; one whose replies did not all stop at refusal is passed over as reader-failed", async () => {
  const refusedFile = "/sessions/written-refused.jsonl";
  const partlyFile = "/sessions/written-partly-refused.jsonl";
  const store = changedStore("written-refused", (files) => {
    const held = files.store_a ?? {};
    held[refusedFile] = sessionText("written-refused", (lines) =>
      setText(lines, 0, () => REFUSED_REQUEST)
    );
    held[partlyFile] = sessionText("written-partly-refused", (lines) =>
      setText(lines, 0, () => PARTLY_REFUSED_REQUEST)
    );
  });
  const goodFile = sessionPath("good");
  const refusal = 'the answer is not one JSON value: "" (the reply stopped at refusal)';
  const why = writtenReaderWhy("reader-refused", Array(READER_TRIES).fill(refusal));
  const printed: string[] = [];
  expect(
    await main(writtenInOrder(store, "written-refused", [refusedFile, goodFile]), io(printed))
  ).toBe(EXIT.ok);
  const out = path.join(scratch, "written-refused", "cases.jsonl");
  const summary = summaryOf<WrittenSummary>(out);
  expect(summary.contextSession.hash).toBe(fileHash(goodFile));
  expect(summary.contextSession.passed).toEqual([
    { store: "store_a", hash: fileHash(refusedFile), why },
  ]);
  expect(printed.join("")).toContain(
    `build-cases: context session place 1 of 2, store_a ${fileHash(refusedFile)}: passed over (${why})\n`
  );
  expect(loadCases(out, old).length).toBeGreaterThan(0);
  // A --context-session of it is refused (exit 1), naming why.
  const refused: string[] = [];
  const named = writtenArgs(
    path.join(scratch, "written-refused-named", "cases.jsonl"),
    "--context-session",
    `store_a:${fileHash(refusedFile)}`
  );
  named[named.indexOf("--config") + 1] = store;
  expect(await main(named, io(refused))).toBe(EXIT.failed);
  expect(refused.join("")).toContain(`--context-session store_a:${fileHash(refusedFile)}: ${why}`);
  // The first reply stopped at refusal, the others ended their turn with no JSON: the session is
  // passed over as reader-failed, each refusal named, and the next one taken.
  const partly: string[] = [];
  expect(
    await main(writtenInOrder(store, "written-partly-refused", [partlyFile, goodFile]), io(partly))
  ).toBe(EXIT.ok);
  const unwritten = `the answer is not one JSON value: ${JSON.stringify(UNWRITTEN)}`;
  const partlyWhy = writtenReaderWhy("reader-failed", [
    refusal,
    ...Array(READER_TRIES - 1).fill(unwritten),
  ]);
  const partlyOut = path.join(scratch, "written-partly-refused", "cases.jsonl");
  const partlySummary = summaryOf<WrittenSummary>(partlyOut);
  expect(partlySummary.contextSession.hash).toBe(fileHash(goodFile));
  expect(partlySummary.contextSession.passed).toEqual([
    { store: "store_a", hash: fileHash(partlyFile), why: partlyWhy },
  ]);
  expect(loadCases(partlyOut, old).length).toBeGreaterThan(0);
}, 120_000);

// Catches: a written build ended (exit 1) by the written-case reader's answers for one context
// session, each refused by the reader's schema (the defect: a skill's written build exited 1, the
// reader's answer refused on each of 3 tries for a task whose line is not the line it names, no
// case written and the sessions after it never tried), which is that session's, not the build's,
// while the next session of the seeded order gives tasks, in the pass over sessions on a served
// model and in the one over those on a model the gateway does not serve, the refused tries' spend
// counted with the build's; and a reader call that fails, which still fails the build.
test("build-cases --written passes over a context session whose written-case reader answers are refused on every try as reader-failed, naming each refusal, in either pass, takes the next one, and refuses a --context-session of it; a reader call that fails still fails the build", async () => {
  const mislinedFile = "/sessions/written-mislined.jsonl";
  const unservedFile = "/sessions/written-mislined-unserved.jsonl";
  const erredFile = "/sessions/written-erred.jsonl";
  const store = changedStore("written-mislined", (files) => {
    const held = files.store_a ?? {};
    held[mislinedFile] = sessionText("written-mislined", (lines) =>
      setText(lines, 0, () => MISLINED_REQUEST)
    );
    held[unservedFile] = sessionText("written-mislined-unserved", (lines) => {
      SESSIONS["not-served"]?.change?.(lines);
      setText(lines, 0, () => MISLINED_REQUEST);
    });
    held[erredFile] = sessionText("written-erred", (lines) =>
      setText(lines, 0, () => ERRED_REQUEST)
    );
  });
  const goodFile = sessionPath("good");
  const refusal = "the answer is refused: tasks.0.line.text: is not line 9 of SKILL.md";
  const why = writtenReaderWhy("reader-failed", Array(READER_TRIES).fill(refusal));
  const printed: string[] = [];
  const from = recorder.requests.length;
  expect(
    await main(writtenInOrder(store, "written-mislined", [mislinedFile, goodFile]), io(printed))
  ).toBe(EXIT.ok);
  const out = path.join(scratch, "written-mislined", "cases.jsonl");
  const summary = summaryOf<WrittenSummary>(out);
  expect(summary.contextSession.hash).toBe(fileHash(goodFile));
  expect(summary.contextSession.passed).toEqual([
    { store: "store_a", hash: fileHash(mislinedFile), why },
  ]);
  expect(printed.join("")).toContain(
    `build-cases: context session place 1 of 2, store_a ${fileHash(mislinedFile)}: passed over (${why})\n`
  );
  expect(loadCases(out, old).length).toBeGreaterThan(0);
  // Each try for the passed-over session was a reader call, and the summary's spend counts it, as
  // it counts every reader call of the build.
  const sent = recorder.requests.slice(from);
  expect(
    usersOf(sent, WRITTEN_PROMPT).filter((user) => user.includes(MISLINED_REQUEST))
  ).toHaveLength(READER_TRIES);
  const calls = Object.values(summary.readerSpend.models).map((each) => each.calls);
  expect(calls.reduce((sum, each) => sum + each, 0)).toBe(sent.length);
  // A --context-session of it is refused (exit 1), naming why.
  const refused: string[] = [];
  const named = writtenArgs(
    path.join(scratch, "written-mislined-named", "cases.jsonl"),
    "--context-session",
    `store_a:${fileHash(mislinedFile)}`
  );
  named[named.indexOf("--config") + 1] = store;
  expect(await main(named, io(refused))).toBe(EXIT.failed);
  expect(refused.join("")).toContain(`--context-session store_a:${fileHash(mislinedFile)}: ${why}`);
  expect(existsSync(named[named.indexOf("--out") + 1] ?? "")).toBe(false);
  // Both sessions are on a model no route serves: the build passes the first over in its second
  // pass, and takes the next one.
  const unserved: string[] = [];
  expect(
    await main(
      writtenInOrder(
        store,
        "written-mislined-unserved",
        [unservedFile, sessionPath("not-served")],
        "--reading-models",
        servedReadingModels("written-mislined")
      ),
      io(unserved)
    )
  ).toBe(EXIT.ok);
  const unservedOut = path.join(scratch, "written-mislined-unserved", "cases.jsonl");
  const second = summaryOf<WrittenSummary>(unservedOut);
  expect(second.contextSession.hash).toBe(fileHash(sessionPath("not-served")));
  expect(second.contextSession.how).toContain("no reading session on a served model");
  expect(second.contextSession.passed).toEqual([
    { store: "store_a", hash: fileHash(unservedFile), why },
  ]);
  expect(unserved.join("")).toContain(
    `build-cases: context session place 1 of 2, store_a ${fileHash(unservedFile)}: passed over (${why})\n`
  );
  expect(loadCases(unservedOut, old).length).toBeGreaterThan(0);
  // A reader call the gateway fails (a 400, on each request) fails the build, as before, and takes
  // no next session.
  const failed: string[] = [];
  expect(
    await main(writtenInOrder(store, "written-erred", [erredFile, goodFile]), io(failed))
  ).toBe(EXIT.failed);
  expect(failed.join("")).toContain(
    "the written-case reader: the model answered 400 invalid_request_error: the stand-in takes no such request"
  );
  expect(existsSync(path.join(scratch, "written-erred", "cases.jsonl"))).toBe(false);
}, 120_000);

// Catches: a written build ended (exit 1) by a context session whose first user message holds no
// text, whose kind of work the written tasks take (the defect: a skill's written build exited 1 on
// such a session after passing over four earlier ones), which is that session's, not the build's,
// while the next session of the seeded order gives tasks, on a served model or on one not served.
test("build-cases --written passes over, before asking the written-case reader, a context session whose first user message holds no text as no-user-text, on a served model or not, takes the next one, and refuses a --context-session of it", async () => {
  const servedFile = "/sessions/written-no-text.jsonl";
  const unservedFile = "/sessions/written-no-text-unserved.jsonl";
  const store = changedStore("written-no-text", (files) => {
    const held = files.store_a ?? {};
    held[servedFile] = sessionText("written-no-text", (lines) => setText(lines, 0, () => ""));
    held[unservedFile] = sessionText("written-no-text-unserved", (lines) => {
      SESSIONS["not-served"]?.change?.(lines);
      setText(lines, 0, () => "");
    });
  });
  const whyOf = (file: string) =>
    `no-user-text: the context session store_a:${fileHash(file)} holds no user message with text, whose kind of work the written tasks take`;
  // On a model no route serves, the session is passed over in the first pass, and the next one,
  // on such a model too, is taken in the second.
  const runs = [
    { name: "written-no-text", file: servedFile, next: sessionPath("good"), extra: [] },
    {
      name: "written-no-text-unserved",
      file: unservedFile,
      next: sessionPath("not-served"),
      extra: ["--reading-models", servedReadingModels("written-no-text")],
    },
  ];
  for (const { name, file, next, extra } of runs) {
    const printed: string[] = [];
    const from = recorder.requests.length;
    expect(await main(writtenInOrder(store, name, [file, next], ...extra), io(printed))).toBe(
      EXIT.ok
    );
    const out = path.join(scratch, name, "cases.jsonl");
    const summary = summaryOf<WrittenSummary>(out);
    expect(summary.contextSession.hash).toBe(fileHash(next));
    expect(summary.contextSession.passed).toEqual([
      { store: "store_a", hash: fileHash(file), why: whyOf(file) },
    ]);
    expect(printed.join("")).toContain(
      `build-cases: context session place 1 of 2, store_a ${fileHash(file)}: passed over (${whyOf(file)})\n`
    );
    // The written-case reader was asked of the chosen session alone.
    expect(usersOf(recorder.requests.slice(from), WRITTEN_PROMPT)).toHaveLength(1);
    expect(loadCases(out, old).length).toBeGreaterThan(0);
  }
  // A --context-session of it is refused (exit 1), naming why, before any reader call.
  const refused: string[] = [];
  const named = writtenArgs(
    path.join(scratch, "written-no-text-named", "cases.jsonl"),
    "--context-session",
    `store_a:${fileHash(servedFile)}`
  );
  named[named.indexOf("--config") + 1] = store;
  const before = recorder.requests.length;
  expect(await main(named, io(refused))).toBe(EXIT.failed);
  expect(refused.join("")).toContain(
    `--context-session store_a:${fileHash(servedFile)}: ${whyOf(servedFile)}`
  );
  expect(recorder.requests.length).toBe(before);
  expect(existsSync(named[named.indexOf("--out") + 1] ?? "")).toBe(false);
}, 120_000);

/** A change of F1 for a session in a checkout of acme/proj whose first message is `request` and
 *  that runs `command` in a bash call after it. */
function readsInProj(request: string, command: string) {
  return (lines: Record<string, unknown>[]) => {
    workingDirectory("/home/dev/src/proj")(lines);
    setText(lines, 0, () => request);
    callAfterPrompt("0d0d000", "toolu_cat", "bash", { command }, "diff --git a/x b/x")(lines);
  };
}

// Catches: written tasks taken from a session whose work reads material outside its checkout (a
// subagent's task naming a diff in a scratch directory), whose runs ask to read that material,
// which the checkout copy answers no read of, so each run is dropped as reads-past-last-round.
test("build-cases --written passes over, before asking the written-case reader, a context session whose first message names a file outside its checkout that it reads, as outside-material, takes the next one, and refuses a --context-session of it; a skill's file named and read, or an outside file named and not read, is no reason", async () => {
  const diff = "/home/dev/scratch/review/delta.diff";
  const outsideFile = "/sessions/outside-read.jsonl";
  const skillFile = "/sessions/outside-skill.jsonl";
  const store = changedStore("written-outside", (files) => {
    const held = files.store_a ?? {};
    held[outsideFile] = sessionText(
      "outside-read",
      readsInProj(`Review the change in ${diff}.`, `cd /home/dev/scratch/review && cat delta.diff`)
    );
    held[skillFile] = sessionText(
      "outside-skill",
      readsInProj(
        `Follow ${F1_SKILLS}/alpha/references/x.md; the old notes in ${diff} need no read.`,
        `cat ${F1_SKILLS}/alpha/references/x.md`
      )
    );
  });
  const candidates = [outsideFile, sessionPath("good")].map((file) => ({
    store: "store_a",
    hash: fileHash(file),
    path: file,
  }));
  const seed = Array.from({ length: 100 }, (_, n) => String(n)).find(
    (each) => blindOrder(candidates, each)[0]?.item.path === outsideFile
  );
  expect(seed).toBeDefined();
  const sessions = path.join(setupDir, "written-outside.jsonl");
  writeFileSync(sessions, candidates.map((each) => `${JSON.stringify(each)}\n`).join(""));
  const why = `outside-material: its first user message names ${diff}, outside its checkout's root /home/dev/src/proj, and message 1 reads it (the bash command "cd /home/dev/scratch/review && cat delta.diff")`;
  const out = path.join(scratch, "written-outside", "cases.jsonl");
  const args = writtenArgs(out, "--sessions", sessions, "--seed", seed ?? "");
  args[args.indexOf("--config") + 1] = store;
  const from = recorder.requests.length;
  const printed: string[] = [];
  expect(await main(args, io(printed))).toBe(EXIT.ok);
  const summary = summaryOf<WrittenSummary>(out);
  expect(summary.contextSession.hash).toBe(fileHash(sessionPath("good")));
  expect(summary.contextSession.passed).toEqual([
    { store: "store_a", hash: fileHash(outsideFile), why },
  ]);
  expect(printed.join("")).toContain(
    `build-cases: context session place 1 of 2, store_a ${fileHash(outsideFile)}: passed over (${why})\n`
  );
  const asked = usersOf(recorder.requests.slice(from), WRITTEN_PROMPT);
  expect(asked.length).toBe(1);
  expect(asked[0]).not.toContain(diff);
  // A --context-session of it is refused; one that reads only a skill's file it names is not.
  const refused: string[] = [];
  const named = writtenArgs(
    path.join(scratch, "written-outside-named", "cases.jsonl"),
    "--context-session",
    `store_a:${fileHash(outsideFile)}`
  );
  named[named.indexOf("--config") + 1] = store;
  expect(await main(named, io(refused))).toBe(EXIT.failed);
  expect(refused.join("")).toContain(`--context-session store_a:${fileHash(outsideFile)}: ${why}`);
  const skill = writtenArgs(
    path.join(scratch, "written-outside-skill", "cases.jsonl"),
    "--context-session",
    `store_a:${fileHash(skillFile)}`,
    "--written-answer",
    writtenAnswerFile("written-outside-skill", [WRITTEN_TASK])
  );
  skill[skill.indexOf("--config") + 1] = store;
  expect(await main(skill, io([]))).toBe(EXIT.ok);
}, 120_000);

// Catches: written cases taken from a context session whose working directory is in no checkout
// the build knows, which record no checkout, so their runs answer no read-only call (the defect:
// verification-before-completion's 7 written cases, and 8 of two other skills, were built so, and
// each run was judged on a read no form answers, every case flooring at 0 of 10 in both arms).
test("build-cases --written passes over, before asking the written-case reader, a context session whose working directory is in no checkout it knows, as no-checkout, takes the next one, whose cases record its checkout, and refuses a --context-session of it", async () => {
  const names = ["two-repos", "good"];
  const sessions = sessionsFile("written-no-checkout.jsonl", names);
  const candidates = names.map((name) => ({
    store: "store_a",
    hash: fileHash(sessionPath(name)),
    path: sessionPath(name),
  }));
  const seed = Array.from({ length: 100 }, (_, n) => String(n)).find(
    (each) => blindOrder(candidates, each)[0]?.item.path === sessionPath("two-repos")
  );
  expect(seed).toBeDefined();
  const out = path.join(scratch, "written-no-checkout", "cases.jsonl");
  const printed: string[] = [];
  const from = recorder.requests.length;
  expect(
    await main(writtenArgs(out, "--sessions", sessions, "--seed", seed ?? ""), io(printed))
  ).toBe(EXIT.ok);
  const hash = fileHash(sessionPath("two-repos"));
  const cwd = "/home/dev/src/skills/notes";
  const why = `no-checkout: its working directory ${cwd} is in no checkout the build knows: its profile names no session repository (${CHECKOUT_RULE}. The working directory ${cwd} is under /home/dev/src/skills, named skills, which 2 repositories of the list are named (acme/skills, forker/skills), so which one it is a checkout of is not known)`;
  const summary = summaryOf<WrittenSummary>(out);
  expect(summary.contextSession.hash).toBe(fileHash(sessionPath("good")));
  expect(summary.contextSession.passed).toEqual([{ store: "store_a", hash, why }]);
  expect(printed.join("")).toContain(
    `build-cases: context session place 1 of 2, store_a ${hash}: passed over (${why})\n`
  );
  // The written-case reader is asked of the chosen session alone, given its checkout.
  const asked = usersOf(recorder.requests.slice(from), WRITTEN_PROMPT);
  expect(asked).toHaveLength(1);
  expect(asked[0]).toContain(`<checkout repo="acme/work" root="${F1_CWD}" cwd="${F1_CWD}"`);
  const cases = loadCases(out, old);
  expect(cases.length).toBeGreaterThan(0);
  for (const c of cases) {
    expect(c.checkout).toEqual({
      repo: "acme/work",
      commit: SHA,
      root: F1_CWD,
      cwd: F1_CWD,
      jj: false,
    });
  }
  // A --context-session of it is refused (exit 1), naming why.
  const refused: string[] = [];
  const named = path.join(scratch, "written-no-checkout-named", "cases.jsonl");
  const context = `store_a:${hash}`;
  expect(await main(writtenArgs(named, "--context-session", context), io(refused))).toBe(
    EXIT.failed
  );
  expect(refused.join("")).toContain(`--context-session ${context}: ${why}`);
  expect(existsSync(named)).toBe(false);
}, 120_000);

/** The arguments of a written build into `name` over the configuration `configFile`, whose seeded
 *  order of the store_a sessions at `files` puts `files[0]` first. */
function writtenInOrder(
  configFile: string,
  name: string,
  files: readonly string[],
  ...extra: string[]
): string[] {
  const candidates = files.map((file) => ({ store: "store_a", hash: fileHash(file), path: file }));
  const seed = Array.from({ length: 100 }, (_, n) => String(n)).find(
    (each) => blindOrder(candidates, each)[0]?.item.path === files[0]
  );
  expect(seed).toBeDefined();
  const sessions = path.join(setupDir, `${name}-sessions.jsonl`);
  writeFileSync(sessions, candidates.map((each) => `${JSON.stringify(each)}\n`).join(""));
  const args = writtenArgs(
    path.join(scratch, name, "cases.jsonl"),
    "--sessions",
    sessions,
    "--seed",
    seed ?? "",
    ...extra
  );
  args[args.indexOf("--config") + 1] = configFile;
  return args;
}

/** A `--reading-models` file `<name>-models.json` whose one model, anthropic/claude-sonnet-5-5, the
 *  gateway serves: the model of a written build whose context session is on one it does not. */
function servedReadingModels(name: string): string {
  const models = path.join(setupDir, `${name}-models.json`);
  writeFileSync(
    models,
    JSON.stringify({
      sessions: 1,
      byModel: [
        { model: "anthropic/claude-sonnet-5-5", served: true, sessions: 1, anthropic: true },
      ],
      noModel: 0,
      notQueried: 0,
      notAnthropic: 0,
      served: 1,
      withModel: 1,
    })
  );
  return models;
}

/** The first message of the two context sessions below, which no reader's request holds when the
 *  build passed them over before asking. */
const MISSES_REQUEST = "Write the alpha notes of the listing-misses session.";
/** A session on a served model and one on a model no route serves, each F1 as the good and the
 *  not-served sessions have it, whose subagent Helper records a listing that holds alpha, beta and
 *  ghost: the rebuilt profile lists alpha, beta and older, so it misses 1 of the 3 recorded names. */
const MISSES_SERVED = "/sessions/listing-misses.jsonl";
const MISSES_UNSERVED = "/sessions/listing-misses-unserved.jsonl";

/** The path of a stand-in store that holds MISSES_SERVED and MISSES_UNSERVED besides the suite's
 *  sessions. */
function listingMissesStore(): string {
  const ghostHelper = sessionText("subagent").replace(
    "- beta: Use for the beta greeting.\\n",
    "- beta: Use for the beta greeting.\\n- ghost: Use for work no repository lists.\\n"
  );
  return changedStore("written-listing-misses", (files) => {
    const held = files.store_a ?? {};
    held[MISSES_SERVED] = sessionText("good", (lines) => setText(lines, 0, () => MISSES_REQUEST));
    held[MISSES_UNSERVED] = sessionText("not-served", (lines) => {
      SESSIONS["not-served"]?.change?.(lines);
      setText(lines, 0, () => MISSES_REQUEST);
    });
    for (const file of [MISSES_SERVED, MISSES_UNSERVED]) {
      held[`${file.slice(0, -".jsonl".length)}/Helper.jsonl`] = ghostHelper;
    }
  });
}

/** Why the screen's rule (score.ts's listingStanding) drops a session whose rebuilt listing
 *  misses ghost of the 3 names its Helper recorded, and adds older to them. */
const MISSES_WHY =
  "listing-misses: its rebuilt listing misses 1 of the 3 names of the recorded subagent listing (33.3%), more than 10%: ghost; listing-adds: its rebuilt listing adds 1 to the 3 names of the recorded subagent listing (33.3%), more than 10%: older";

// Catches: a written build that takes a context session whose cases the screen drops for the
// listing (the defect: a build took a session whose rebuilt listing missed 64 of its 204 recorded
// names, and all 11 written cases dropped at the screen as listing-misses, after the readers'
// spend on their tasks).
test("build-cases --written passes over, before asking the written-case reader, a context session whose rebuilt listing misses more than 10% of its recorded names as listing-misses, takes the next one, and refuses a --context-session of it", async () => {
  const store = listingMissesStore();
  const goodFile = sessionPath("good");
  const printed: string[] = [];
  const from = recorder.requests.length;
  expect(
    await main(
      writtenInOrder(store, "written-listing-misses", [MISSES_SERVED, goodFile]),
      io(printed)
    )
  ).toBe(EXIT.ok);
  const out = path.join(scratch, "written-listing-misses", "cases.jsonl");
  const summary = summaryOf<WrittenSummary>(out);
  const hash = fileHash(MISSES_SERVED);
  expect(summary.contextSession.hash).toBe(fileHash(goodFile));
  expect(summary.contextSession.how).toContain("place 2 of 2");
  expect(summary.contextSession.passed).toEqual([{ store: "store_a", hash, why: MISSES_WHY }]);
  expect(printed.join("")).toContain(
    `build-cases: context session place 1 of 2, store_a ${hash}: passed over (${MISSES_WHY})\n`
  );
  // No request holds the passed-over session's first message: the written-case reader was asked
  // of the chosen session alone.
  const sent = recorder.requests.slice(from);
  expect(sent.some((request) => request.body.includes(MISSES_REQUEST))).toBe(false);
  expect(usersOf(sent, WRITTEN_PROMPT)).toHaveLength(1);
  expect(loadCases(out, old).length).toBeGreaterThan(0);
  // A --context-session of it is refused (exit 1), naming why, before any reader call.
  const refused: string[] = [];
  const named = writtenArgs(
    path.join(scratch, "written-listing-misses-named", "cases.jsonl"),
    "--context-session",
    `store_a:${hash}`
  );
  named[named.indexOf("--config") + 1] = store;
  const before = recorder.requests.length;
  expect(await main(named, io(refused))).toBe(EXIT.failed);
  expect(refused.join("")).toContain(`--context-session store_a:${hash}: ${MISSES_WHY}`);
  expect(recorder.requests.length).toBe(before);
  expect(existsSync(named[named.indexOf("--out") + 1] ?? "")).toBe(false);
});

test("build-cases --written passes over as listing-misses, in its second pass, a context session on a model the gateway does not serve whose rebuilt listing misses more than 10% of its recorded names, before asking the written-case reader, and takes the next one", async () => {
  const store = listingMissesStore();
  const printed: string[] = [];
  const from = recorder.requests.length;
  // Both sessions are on a model no route serves, so the build asks neither in its first pass.
  expect(
    await main(
      writtenInOrder(
        store,
        "written-listing-misses-unserved",
        [MISSES_UNSERVED, sessionPath("not-served")],
        "--reading-models",
        servedReadingModels("written-listing-misses")
      ),
      io(printed)
    )
  ).toBe(EXIT.ok);
  const out = path.join(scratch, "written-listing-misses-unserved", "cases.jsonl");
  const summary = summaryOf<WrittenSummary>(out);
  const hash = fileHash(MISSES_UNSERVED);
  expect(summary.contextSession.hash).toBe(fileHash(sessionPath("not-served")));
  expect(summary.contextSession.how).toContain("no reading session on a served model");
  expect(summary.contextSession.passed).toEqual([{ store: "store_a", hash, why: MISSES_WHY }]);
  expect(printed.join("")).toContain(
    `build-cases: context session place 1 of 2, store_a ${hash}: passed over (${MISSES_WHY})\n`
  );
  const sent = recorder.requests.slice(from);
  expect(sent.some((request) => request.body.includes(MISSES_REQUEST))).toBe(false);
  expect(usersOf(sent, WRITTEN_PROMPT)).toHaveLength(1);
  expect(loadCases(out, old).length).toBeGreaterThan(0);
});

test("build-cases --written takes, as before, a context session whose rebuilt listing misses none of its recorded names, or that is not measurable, and its cases carry that measure", async () => {
  // good's Helper records alpha, beta and older, which its profile lists; openai has no subagent
  // in the store, so its listing is not measurable, and the screen drops no case for it.
  const rows = [
    { name: "good", kind: "subagent listing", recorded: 3 },
    { name: "openai", kind: "not measurable", recorded: null },
  ] as const;
  for (const row of rows) {
    const other = sessionPath(row.name === "good" ? "openai" : "good");
    const printed: string[] = [];
    const name = `written-listing-taken-${row.name}`;
    expect(
      await main(writtenInOrder(config, name, [sessionPath(row.name), other]), io(printed))
    ).toBe(EXIT.ok);
    const out = path.join(scratch, name, "cases.jsonl");
    const summary = summaryOf<WrittenSummary>(out);
    expect(summary.contextSession.hash).toBe(fileHash(sessionPath(row.name)));
    expect(summary.contextSession.how).toContain("place 1 of 2");
    expect(summary.contextSession.passed).toEqual([]);
    expect(printed.join("")).not.toContain("passed over");
    const cases = loadCases(out, old);
    expect(cases.length).toBeGreaterThan(0);
    for (const c of cases) {
      const item = c.evidence?.find((each) => each.item === LISTING_MEASURE_ITEM);
      const measure = ListingMeasure.parse(JSON.parse(item?.source ?? "{}"));
      expect(measure).toMatchObject({ kind: row.kind, recorded: row.recorded, missing: [] });
    }
  }
});

test("the context session is the first served reading session whose rebuilt profile lists the skill", async () => {
  // not-served runs on a model no route serves; other-copy read acme/other's alpha, so its profile
  // installs no alpha of the cut's repository; good and openai are on served models, and list it.
  const names = ["not-served", "other-copy", "good", "openai"];
  const sessions = sessionsFile("written-candidates.jsonl", names);
  const candidates = names.map((name) => ({
    store: "store_a",
    hash: fileHash(sessionPath(name)),
    path: sessionPath(name),
  }));
  const orderOf = (seed: string) => blindOrder(candidates, seed).map(({ item }) => item.path);
  // A seed whose order puts not-served and other-copy first.
  const seed = Array.from({ length: 500 }, (_, n) => String(n)).find((each) => {
    const order = orderOf(each);
    return order[0] === sessionPath("not-served") && order[1] === sessionPath("other-copy");
  });
  expect(seed).toBeDefined();
  const chosen = orderOf(seed ?? "")[2] ?? "";
  const out = path.join(scratch, "written-seeded", "cases.jsonl");
  const lines: string[] = [];
  expect(
    await main(writtenArgs(out, "--sessions", sessions, "--seed", seed ?? ""), io(lines))
  ).toBe(EXIT.ok);
  const summary = summaryOf<WrittenSummary>(out);
  expect(summary.contextSession).toMatchObject({
    store: "store_a",
    hash: fileHash(chosen),
    pathHash: pathHash(chosen),
  });
  expect(summary.contextSession.how).toContain("place 3 of 4");
  expect(summary.contextSession.passed.map((each) => [each.hash, each.why.split(":")[0]])).toEqual([
    [fileHash(sessionPath("not-served")), "model-not-served"],
    [fileHash(sessionPath("other-copy")), "not-cut-copy"],
  ]);
  const cases = loadCases(out, old);
  expect(cases.length).toBeGreaterThan(0);
  expect(cases.every((each) => each.source.pathHash === pathHash(chosen))).toBe(true);
  expect(summary.model.how).toBe("the context session's own");

  // With no served session whose profile lists the skill, the first one whose profile lists it,
  // on the served model with the most reading sessions of the skill (--reading-models).
  const unserved = sessionsFile("written-unserved.jsonl", ["other-copy", "not-served"]);
  const counted = (byModel: { model: string; served: boolean; sessions: number }[]) => ({
    sessions: byModel.reduce((sum, each) => sum + each.sessions, 0),
    byModel: byModel.map((each) => ({ ...each, anthropic: each.model.includes("claude-") })),
    noModel: 0,
    notQueried: 0,
    notAnthropic: 0,
    served: byModel.filter((each) => each.served).reduce((sum, each) => sum + each.sessions, 0),
    withModel: byModel.reduce((sum, each) => sum + each.sessions, 0),
  });
  const modelsFile = (name: string, byModel: Parameters<typeof counted>[0]) => {
    const file = path.join(setupDir, name);
    writeFileSync(file, JSON.stringify(counted(byModel)));
    return file;
  };
  const astra = { model: "openai-codex/gpt-6-astra", served: false, sessions: 4 };
  const skill = modelsFile("written-alpha-models.json", [
    astra,
    { model: "openai/gpt-6.1-sol", served: true, sessions: 2 },
    { model: "anthropic/claude-sonnet-5-5", served: true, sessions: 1 },
  ]);
  const fromSkill = path.join(scratch, "written-unserved", "cases.jsonl");
  const args = (to: string, ...extra: string[]) =>
    writtenArgs(to, "--sessions", unserved, "--seed", "1", ...extra);
  expect(await main(args(fromSkill, "--reading-models", skill), io(lines))).toBe(EXIT.ok);
  const [first] = loadCases(fromSkill, old);
  expect(first?.source.pathHash).toBe(pathHash(sessionPath("not-served")));
  expect(first?.model).toBe("openai/gpt-6.1-sol");
  expect(summaryOf<WrittenSummary>(fromSkill).contextSession.how).toContain(
    "no reading session on a served model"
  );

  // With no served model in the skill's file, the served model with the most reading sessions over
  // the pool's files (the set's skills' models files).
  const none = modelsFile("written-none-models.json", [astra]);
  const poolA = modelsFile("written-pool-a.json", [
    { model: "openai/gpt-6.1-sol", served: true, sessions: 2 },
    { model: "anthropic/claude-opus-5-5", served: true, sessions: 2 },
  ]);
  const poolB = modelsFile("written-pool-b.json", [
    astra,
    { model: "anthropic/claude-opus-5-5", served: true, sessions: 1 },
  ]);
  const fromPool = path.join(scratch, "written-pool", "cases.jsonl");
  const pooled = args(
    fromPool,
    "--reading-models",
    none,
    "--model-pool",
    poolA,
    "--model-pool",
    poolB
  );
  expect(await main(pooled, io(lines))).toBe(EXIT.ok);
  expect(loadCases(fromPool, old)[0]?.model).toBe("anthropic/claude-opus-5-5");
  expect(summaryOf<WrittenSummary>(fromPool).model.how).toContain("--model-pool");

  // With no served model anywhere, the build fails, naming why, and writes no case.
  const failed: string[] = [];
  const nowhere = path.join(scratch, "written-nowhere", "cases.jsonl");
  expect(await main(args(nowhere, "--reading-models", none), io(failed))).toBe(EXIT.failed);
  expect(failed.join("")).toContain("no served model for the written cases");
  expect(existsSync(nowhere)).toBe(false);
}, 120_000);

// Catches: a context session whose only successful read of the skill is a whole read by path of
// the cut's copy passed over as one that never read the skill (`no-read`), though the session build
// builds a case from it; a written case's read of the skill shown at the path as read, not at the
// copy's directory; and a session whose reads by path are of a kind no case is built from passed
// over under another reason than that kind.
describe("build-cases --written over context sessions that read the skill by path", () => {
  const out = path.join(scratch, "written-by-path", "cases.jsonl");
  const names = ["path-range", "path-relative-read"];
  const printed: string[] = [];
  let code = -1;
  beforeAll(async () => {
    const candidates = names.map((name) => ({
      store: "store_a",
      hash: fileHash(sessionPath(name)),
      path: sessionPath(name),
    }));
    // A seed whose order puts path-range first.
    const seed = Array.from({ length: 100 }, (_, n) => String(n)).find(
      (each) => blindOrder(candidates, each)[0]?.item.path === sessionPath("path-range")
    );
    const sessions = sessionsFile("written-by-path.jsonl", names);
    const args = writtenArgs(out, "--sessions", sessions, "--seed", seed ?? "");
    args[args.indexOf("--written") + 1] = "1";
    code = await main(args, io(printed));
  }, 60_000);

  test("a context session whose only successful read of the skill is a whole read by path of the cut's copy is chosen, and its written case reads skill://alpha answered at the copy's directory", () => {
    expect({ code, failed: printed.join("").includes("failed") }).toEqual({
      code: 0,
      failed: false,
    });
    const chosen = sessionPath("path-relative-read");
    const summary = summaryOf<WrittenSummary>(out);
    expect(summary.contextSession).toMatchObject({
      store: "store_a",
      hash: fileHash(chosen),
      pathHash: pathHash(chosen),
    });
    expect(summary.contextSession.how).toContain("place 2 of 2");
    const cases = loadCases(out, old);
    expect(cases.map((each) => each.id)).toEqual([`skills-1-${pathHash(chosen).slice(0, 12)}-w1`]);
    const [c] = cases;
    expect(c?.source).toEqual({ store: "store_a", pathHash: pathHash(chosen), kind: "written" });
    // The task, then a read of skill://alpha answered from the old text under the copy the session
    // read relative to its working directory, resolved: F1's own read result there.
    expect(c?.messages).toHaveLength(3);
    expect(c?.messages[1]).toMatchObject({
      role: "assistant",
      content: [
        {
          type: "toolCall",
          id: "written_read",
          name: "read",
          arguments: { path: "skill://alpha" },
        },
      ],
    });
    expect(c?.messages[2]?.content?.[0]?.text).toStartWith(
      `[Skill file: ${F1_SKILLS}/alpha/SKILL.md]`
    );
    expect(c?.messages[2]?.content).toEqual(f1Messages()[2]?.content);
    expect(c?.skillReads).toEqual([2]);
    expect(c?.pathCopy).toBeUndefined();
    // The model and thinking level at that read, and the profile its reads up to it give, which
    // installs the cut's copy.
    expect([c?.model, c?.thinking]).toEqual(["anthropic/claude-sonnet-5-5", "off"]);
    expect(c?.profile.skills.filter((skill) => skill.name === "alpha")).toEqual([
      { repo: "acme/skills", commit: SHA, path: "skills/alpha", name: "alpha" },
    ]);
    // Its evidence names how the context session read the skill: by path, with the path.
    expect(c?.evidence).toContainEqual({
      item: `context session read: message 2 reads ${F1_RELATIVE}/SKILL.md by its path`,
      source: `a whole read by path of the cut's copy of alpha at ${F1_SKILLS}/alpha in the context session, before the written task: a written case's profile is built from the context session's reads up to its first read of the skill, as a session case's is from the reads before its step, and its read of skill://alpha shows that copy's directory`,
    });
  });

  test("a context session whose only successful reads of the skill are reads by path of a kind no case is built from is passed over under that kind, as the session build drops it", () => {
    const hash = fileHash(sessionPath("path-range"));
    const why = `path-range: the session reads alpha by path only so: message 2 answers the read of ${F1_SKILLS}/alpha/SKILL.md:1-20 (path-range: a read selector after its last /); message 4 answers the read of ${F1_SKILLS}/alpha/references/x.md:raw (path-range: a read selector after its last /)`;
    expect(summaryOf<WrittenSummary>(out).contextSession.passed).toEqual([
      { store: "store_a", hash, why },
    ]);
    expect(printed.join("")).toContain(
      `build-cases: context session place 1 of 2, store_a ${hash}: passed over (${why})\n`
    );
  });

  // Catches: a written case whose context session read the skill by path given, by --reprofile,
  // a profile built from its own messages, since its evidence's read is in no form it reads.
  test("--reprofile builds that written case's profile again from the read by path its evidence names", async () => {
    const fresh = path.join(scratch, "written-by-path", "reprofiled.jsonl");
    const args = [
      ...["build-cases", "--config", config, "--old", old, "--copies", writtenCopies()],
      ...["--count", countDir, "--reprofile", out, "--out", fresh],
    ];
    expect(await main(args, io([]))).toBe(EXIT.ok);
    const [before] = loadCases(out, old);
    const [after] = loadCases(fresh, old);
    expect(after?.profile).toEqual(before?.profile);
    const byPath = (c: Case | undefined) =>
      (c?.evidence ?? []).filter((each) =>
        each.source.startsWith(`message 2 reads ${F1_RELATIVE}/SKILL.md by its path`)
      );
    expect(byPath(before).length).toBeGreaterThan(0);
    expect(byPath(after)).toEqual(byPath(before));
  }, 60_000);
});

// Catches: a --context-session build given --sessions or --seed, which it never reads, that then
// records their digest and seed in its summary as if it had.
test("build-cases --written with --context-session refuses --sessions and --seed, before any read", async () => {
  const sessions = sessionsFile("written-both.jsonl", ["good"]);
  const context = ["--context-session", `store_a:${fileHash(sessionPath("good"))}`];
  const out = path.join(scratch, "written-both", "cases.jsonl");
  const from = recorder.requests.length;
  const logged = psqlLogLength();
  const calls = ghCalls.length;
  const rows: [string[], string][] = [
    [["--sessions", sessions], "--sessions"],
    [["--seed", "1"], "--seed"],
    [["--sessions", sessions, "--seed", "1"], "--sessions"],
  ];
  for (const [extra, flag] of rows) {
    const lines: string[] = [];
    expect(await main(writtenArgs(out, ...context, ...extra), io(lines))).toBe(EXIT.refused);
    expect(lines.join("\n")).toContain(`--written takes no ${flag} with --context-session`);
  }
  expect(existsSync(out)).toBe(false);
  expect(recorder.requests.length).toBe(from);
  expect(psqlLogLength()).toBe(logged);
  expect(ghCalls.length).toBe(calls);
});

// Catches: a written or re-profile build given --new, which neither reads (a written case reads
// no file of the copy by path, and a re-profile builds profiles only), taking it in silence.
test("build-cases --written and --reprofile refuse --new, before any read", async () => {
  const context = ["--context-session", `store_a:${fileHash(sessionPath("good"))}`];
  const dir = path.join(scratch, "written-new");
  const from = recorder.requests.length;
  const logged = psqlLogLength();
  const calls = ghCalls.length;
  const written: string[] = [];
  const out = path.join(dir, "cases.jsonl");
  expect(await main(writtenArgs(out, ...context, "--new", newDir), io(written))).toBe(EXIT.refused);
  expect(written.join("\n")).toContain("--written takes no --new");
  const reprofiled: string[] = [];
  const reprofile = [
    ...["build-cases", "--config", config, "--old", old, "--copies", copiesDir],
    ...["--count", countDir, "--reprofile", out, "--out", path.join(dir, "reprofiled.jsonl")],
  ];
  expect(await main([...reprofile, "--new", newDir], io(reprofiled))).toBe(EXIT.refused);
  expect(reprofiled.join("\n")).toContain("so it takes no --new");
  expect(existsSync(dir)).toBe(false);
  expect(recorder.requests.length).toBe(from);
  expect(psqlLogLength()).toBe(logged);
  expect(ghCalls.length).toBe(calls);
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
    pathHash: pathHash(`${DEV_SESSIONS}/task-1.jsonl`),
  });
  expect(tasks[1]).toEqual({
    id: "t2",
    text: "Write the Alpha report of step 2.",
    label: "near-miss",
    source: "session",
    store: "store_a",
    pathHash: pathHash(`${DEV_SESSIONS}/task-3.jsonl`),
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
      pathHash: pathHash(`${DEV_SESSIONS}/task-3.jsonl`),
    },
  ]);
  const summary = JSON.parse(readFileSync(`${out}.summary.json`, "utf8"));
  expect(summary.model).toBe("openai/gpt-6.1-sol");
  expect(summary.inputs.model).toBe("openai/gpt-6.1-sol");
  expect(summary.messagesOffered).toBe(1);
  expect(summary.candidateDrops.counts).toEqual({ "other-model": 2 });
  expect(summary.candidateDrops.sessions).toEqual(
    [`${DEV_SESSIONS}/task-1.jsonl`, `${DEV_SESSIONS}/task-4.jsonl`].map((file) => ({
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
    .map(
      (line) => JSON.parse(line) as { db: string; kind: string; rows: number; after?: number[] }
    );

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
  }, HOOK_BUILD_MS);

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
  }, 60_000);
});

// build-cases --drop-navigation: a step under a line that only sends the agent to a file of the
// skill is no case, and the case reader goes on after it, offered only the lines of the files
// that step read: its first proposal there is the case, or the session gives none. Catches: a
// navigation line kept as a case's line (the arm with no skill fails its step by construction), a
// step dropped only because it reads a file of the skill, a replacement taken from a file the step
// did not read, a navigation line left out of the summary, and a build without the flag that does
// not build as before.

/** The summary's `navigation`, of either form of build-cases. */
interface NavigationCounts {
  lines: { file: string; number: number; text: string; dropped: number }[];
  dropped: number;
  withReplacement: number;
  withoutReplacement: number;
}

/** A build-cases summary as the navigation tests read it. */
interface NavigationSummary {
  inputs: { dropNavigation: boolean };
  navigation: NavigationCounts;
  drops: Partial<Record<DropReason, number>>;
  sessions: {
    hash: string;
    outcome: string;
    reason?: DropReason;
    detail?: string;
    windows?: number;
    navigation?: unknown[];
    readerUsd: number;
  }[];
  readerSpend: Summary["readerSpend"];
}

describe("build-cases --drop-navigation", () => {
  const replaced = path.join(scratch, "navigation", "replaced", "cases.jsonl");
  const unreplaced = path.join(scratch, "navigation", "unreplaced", "cases.jsonl");
  const plain = path.join(scratch, "navigation", "plain", "cases.jsonl");
  const earlier = path.join(scratch, "navigation", "earlier", "cases.jsonl");
  const printed: Record<string, string[]> = {
    replaced: [],
    unreplaced: [],
    plain: [],
    earlier: [],
  };
  const requests: Record<string, RecordedRequest[]> = {};
  const codes: Record<string, number> = {};
  let store = "";
  /** The sessions file of each build. */
  const sessions: Record<string, string> = {};
  /** The arguments of a build of the sessions of `build` into `out`, and `extra`. */
  const navArgs = (build: string, out: string, ...extra: string[]) =>
    buildArgs(sessions[build] ?? "", out, "3", "--concurrency", "1", ...extra).map((arg) =>
      arg === config ? store : arg
    );
  const recordOf = (out: string, name: string) =>
    summaryOf<NavigationSummary>(out).sessions.find(
      (each) => each.hash === fileHash(sessionPath(name))
    );
  const caseOf = (out: string, name: string) =>
    loadCases(out, old).find((each) => each.source.pathHash === pathHash(sessionPath(name)));
  /** A record's entry for the step at `messageIndex` passed over under NAVIGATION_LINE, with the
   *  id of the case that replaced it, or null. */
  const passedOver = (messageIndex: number, replacement: string | null) => ({
    messageIndex,
    file: "SKILL.md",
    number: 8,
    text: NAVIGATION_LINE,
    reason: NAVIGATION_REASON,
    replacement,
  });
  const NAMES = ["nav-step", "ref-read", "nav-skill-md", "nav-nothing", "nav-line"];
  beforeAll(async () => {
    // nav-step is F1 with NAV_FILLERS fillers after its message 6, nav-earlier F1 with
    // EARLIER_FILLERS fillers after its message 2; each other is F1.
    store = changedStore("navigation-store", (files) => {
      const sessionFiles = files.store_a ?? {};
      for (const name of NAMES) sessionFiles[sessionPath(name)] = sessionText(name);
      sessionFiles[sessionPath("nav-step")] = sessionText("nav-step", (lines) =>
        addFillers(lines, NAV_FILLERS, (n) => `Still reading (${n}).`)
      );
      sessionFiles[sessionPath("nav-earlier")] = sessionText("nav-earlier", (lines) =>
        addFillers(lines, EARLIER_FILLERS, (n) => `Still reading (${n}).`, {
          after: 2,
          at: "2026-11-20T01:59:49.000Z",
        })
      );
    });
    sessions.replaced = sessionsFile("navigation-replaced.jsonl", ["nav-step", "ref-read"]);
    sessions.unreplaced = sessionsFile("navigation-unreplaced.jsonl", NAMES.slice(2));
    sessions.plain = sessionsFile("navigation-plain.jsonl", NAMES);
    sessions.earlier = sessionsFile("navigation-earlier.jsonl", ["nav-earlier"]);
    for (const [build, out, extra] of [
      ["replaced", replaced, ["--drop-navigation"]],
      ["unreplaced", unreplaced, ["--drop-navigation"]],
      ["plain", plain, []],
      ["earlier", earlier, ["--drop-navigation"]],
    ] as const) {
      const from = recorder.requests.length;
      codes[build] = await main(navArgs(build, out, ...extra), io(printed[build] ?? []));
      requests[build] = recorder.requests.slice(from);
    }
  }, 120_000);

  test("with --drop-navigation, a step under a line the navigation reader calls navigation gives no case, and the case reader goes on from the window after it offered only the lines of the file that step read: its first proposal there, a later step under a line of references/x.md, is the case, and the summary names the line and counts one drop with a replacement", () => {
    expect(codes.replaced).toBe(EXIT.ok);
    const c = caseOf(replaced, "nav-step");
    expect(c?.realStep).toEqual(f1Messages()[7] as SessionMessage);
    expect([c?.baseFile, c?.line]).toEqual([
      "references/x.md",
      { text: X_LINE, lineNumber: 3, lineCount: 3 },
    ]);
    expect(recordOf(replaced, "nav-step")).toMatchObject({
      outcome: `case ${c?.id}`,
      windows: 2,
      navigation: [passedOver(3, c?.id ?? "")],
    });
    // The first window (messages 1 to 40) offers both files and ends before the task call, message
    // 42; the second starts after the read of references/x.md, message 3, and offers that file
    // alone.
    const asked = usersOf(requests.replaced ?? [], CASE_PROMPT).filter((user) =>
      user.includes("(case nav-step)")
    );
    const offered = `<file path="references/x.md">\n1: # Alpha reference x\n3: ${X_LINE}\n</file>`;
    const shows = (user: string, index: number) => user.includes(`<message index="${index}"`);
    expect(
      asked.map((user) => [
        user.includes(SKILL_MD_OFFERED),
        user.includes(offered),
        ...[40, 41, 43, 44].map((n) => shows(user, n)),
      ])
    ).toEqual([
      [true, true, true, false, false, false],
      [false, true, true, true, true, false],
    ]);
    // The navigation reader read nav-step's line, its replacement's and ref-read's: each line, the
    // line's file and the skill's files.
    const files = "<files>\nSKILL.md\nreferences/x.md\n</files>";
    expect(usersOf(requests.replaced ?? [], NAVIGATION_PROMPT).sort()).toEqual(
      [
        `${files}\n\n<file>\nSKILL.md\n</file>\n\n<line>\n${NAVIGATION_LINE}\n</line>`,
        `${files}\n\n<file>\nreferences/x.md\n</file>\n\n<line>\n${X_LINE}\n</line>`,
        `${files}\n\n<file>\nSKILL.md\n</file>\n\n<line>\n${LINE}\n</line>`,
      ].sort()
    );
    const summary = summaryOf<NavigationSummary>(replaced);
    expect(summary.inputs.dropNavigation).toBe(true);
    expect(summary.navigation).toEqual({
      lines: [{ file: "SKILL.md", number: 8, text: NAVIGATION_LINE, dropped: 1 }],
      dropped: 1,
      withReplacement: 1,
      withoutReplacement: 0,
    });
    expect(printed.replaced?.at(-1)).toContain(
      "1 step dropped for 1 navigation line (1 with a replacement case, 0 without) (--drop-navigation)"
    );
    // The navigation reader's spend is in readerSpend with the other readers'.
    const calls = Object.values(summary.readerSpend.models).reduce(
      (sum, each) => sum + each.calls,
      0
    );
    expect(calls).toBe(requests.replaced?.length);
  });

  // Catches: a next window that starts after the window when the step dropped for a navigation
  // line is among the messages shown before it, which skips the window's later steps.
  test("with --drop-navigation, a step the case reader proposes among the messages shown before its window, under a navigation line, gives no case, and the next window starts right after that step: a later step of that same window, under a line of the file the step read, is the case", () => {
    expect(codes.earlier).toBe(EXIT.ok);
    const c = caseOf(earlier, "nav-earlier");
    expect(c?.realStep).toEqual(f1Messages()[7] as SessionMessage);
    expect([c?.baseFile, c?.line]).toEqual([
      "references/x.md",
      { text: X_LINE, lineNumber: 3, lineCount: 3 },
    ]);
    expect(recordOf(earlier, "nav-earlier")).toMatchObject({
      outcome: `case ${c?.id}`,
      windows: 3,
      navigation: [passedOver(38, c?.id ?? "")],
    });
    // The first window (messages 1 to 40, after message 0) holds no task call; the second
    // (messages 41 to 46, after messages 21 to 40) holds it, message 42, and the reader proposes
    // the read of references/x.md, message 38, shown before it; the third starts right after that
    // read (messages 39 to 46, after messages 19 to 38) and offers references/x.md alone.
    expect(
      usersOf(requests.earlier ?? [], CASE_PROMPT).map((user) => {
        const shown = [...user.matchAll(/<message index="(\d+)"/g)].map((match) => match[1]);
        return [user.includes(SKILL_MD_OFFERED), Number(shown[0]), Number(shown.at(-1))];
      })
    ).toEqual([
      [true, 0, 40],
      [true, 21, 46],
      [false, 19, 46],
    ]);
    expect(summaryOf<NavigationSummary>(earlier).navigation).toEqual({
      lines: [{ file: "SKILL.md", number: 8, text: NAVIGATION_LINE, dropped: 1 }],
      dropped: 1,
      withReplacement: 1,
      withoutReplacement: 0,
    });
  });

  test("with --drop-navigation, a step that reads references/x.md under a line that is not a navigation line is the case, and nothing is passed over", () => {
    const c = caseOf(replaced, "ref-read");
    expect(c?.realStep).toEqual(f1Messages()[3] as SessionMessage);
    expect([c?.baseFile, c?.line.text]).toEqual(["SKILL.md", LINE]);
    const record = recordOf(replaced, "ref-read");
    expect(record).toMatchObject({ outcome: `case ${c?.id}`, windows: 1 });
    expect(record?.navigation).toBeUndefined();
  });

  test("with --drop-navigation, a navigation drop whose later windows propose only a SKILL.md line, or nothing, or whose step read no file of the skill, gives no case: the session is dropped navigation-no-replacement, naming the line, and the summary counts each drop without a replacement", () => {
    expect(codes.unreplaced).toBe(EXIT.ok);
    expect(loadCases(unreplaced, old)).toEqual([]);
    const line = `SKILL.md:8 ${JSON.stringify(NAVIGATION_LINE)}`;
    // nav-skill-md's later window offers references/x.md alone, so the reader's proposal of a
    // SKILL.md line there is refused; nav-nothing's later window proposes no step, and then the
    // messages run out.
    for (const [name, why] of [
      ["nav-skill-md", "reader-failed: "],
      ["nav-nothing", "the windows ran out (2 windows read, to message 11 of 12): No step"],
    ] as const) {
      const record = recordOf(unreplaced, name);
      expect(record).toMatchObject({
        outcome: "dropped",
        reason: "navigation-no-replacement",
        windows: 2,
        navigation: [passedOver(3, null)],
      });
      expect(record?.detail).toContain(line);
      expect(record?.detail).toContain(why);
      const asked = usersOf(requests.unreplaced ?? [], CASE_PROMPT).filter((user) =>
        user.includes(`(case ${name})`)
      );
      expect(asked[0]).toContain(SKILL_MD_OFFERED);
      expect(asked.slice(1).some((user) => user.includes(SKILL_MD_OFFERED))).toBe(false);
    }
    // nav-line's step, the task call, reads no file of the skill, so nothing can replace it.
    const lineRecord = recordOf(unreplaced, "nav-line");
    expect(lineRecord).toMatchObject({
      outcome: "dropped",
      reason: "navigation-no-replacement",
      windows: 1,
      navigation: [passedOver(7, null)],
    });
    expect(lineRecord?.detail).toContain(line);
    expect(lineRecord?.detail).toContain("read no file of the skill");
    const summary = summaryOf<NavigationSummary>(unreplaced);
    expect(summary.drops).toEqual({ "navigation-no-replacement": 3 });
    expect(summary.navigation).toEqual({
      lines: [{ file: "SKILL.md", number: 8, text: NAVIGATION_LINE, dropped: 3 }],
      dropped: 3,
      withReplacement: 0,
      withoutReplacement: 3,
    });
    expect(printed.unreplaced?.at(-1)).toContain(
      "3 steps dropped for 1 navigation line (0 with a replacement case, 3 without) (--drop-navigation)"
    );
  });

  // Catches: a line judged once per proposal, which a reader at temperature 1 may call navigation
  // in one session and not in another, so that a case stands under a line the summary lists as a
  // navigation line.
  test("with --drop-navigation, sessions that propose the same line take one navigation reader call and its one verdict, its spend counted once", () => {
    // nav-skill-md, nav-nothing and nav-line each propose SKILL.md's line 8 first.
    for (const name of ["nav-skill-md", "nav-nothing", "nav-line"]) {
      expect(recordOf(unreplaced, name)?.navigation).toEqual([
        expect.objectContaining({ file: "SKILL.md", number: 8, reason: NAVIGATION_REASON }),
      ]);
    }
    expect(usersOf(requests.unreplaced ?? [], NAVIGATION_PROMPT)).toEqual([
      `<files>\nSKILL.md\nreferences/x.md\n</files>\n\n<file>\nSKILL.md\n</file>\n\n<line>\n${NAVIGATION_LINE}\n</line>`,
    ]);
    // The sessions' reader dollars sum to the build's: the one call is counted on one session.
    const summary = summaryOf<NavigationSummary>(unreplaced);
    const sessionsUsd = summary.sessions.reduce((total, each) => total + each.readerUsd, 0);
    expect(sessionsUsd).toBeCloseTo(summary.readerSpend.usd, 9);
  });

  test("without --drop-navigation, the same sessions give today's cases, navigation lines and reads of references/x.md among them; no navigation reader is called, and nothing is counted", () => {
    expect(codes.plain).toBe(EXIT.ok);
    const messages = f1Messages();
    expect(
      NAMES.map((name) => {
        const c = caseOf(plain, name);
        return [name, c?.realStep, c?.baseFile, c?.line.text];
      })
    ).toEqual([
      ["nav-step", messages[3], "SKILL.md", NAVIGATION_LINE],
      ["ref-read", messages[3], "SKILL.md", LINE],
      ["nav-skill-md", messages[3], "SKILL.md", NAVIGATION_LINE],
      ["nav-nothing", messages[3], "SKILL.md", NAVIGATION_LINE],
      ["nav-line", messages[7], "SKILL.md", NAVIGATION_LINE],
    ]);
    expect(usersOf(requests.plain ?? [], NAVIGATION_PROMPT)).toEqual([]);
    const summary = summaryOf<NavigationSummary>(plain);
    expect(summary.inputs.dropNavigation).toBe(false);
    expect(summary.navigation).toEqual({
      lines: [],
      dropped: 0,
      withReplacement: 0,
      withoutReplacement: 0,
    });
    expect(summary.sessions.map((each) => each.navigation)).toEqual(NAMES.map(() => undefined));
    expect(printed.plain?.at(-1)).not.toContain("navigation line");
  });

  test("a run that goes on with another --drop-navigation value is refused, naming it, and leaves the files as they were", async () => {
    const before = readFileSync(`${replaced}.summary.json`, "utf8");
    const off: string[] = [];
    expect(await main(navArgs("replaced", replaced), io(off))).toBe(EXIT.refused);
    expect(off.join("\n")).toContain("--drop-navigation was given, now not given");
    expect(readFileSync(`${replaced}.summary.json`, "utf8")).toBe(before);
    const on: string[] = [];
    expect(await main(navArgs("plain", plain, "--drop-navigation"), io(on))).toBe(EXIT.refused);
    expect(on.join("\n")).toContain("--drop-navigation was not given, now given");
  });
});

// Catches: the --new input digest taking regular files alone, so a file --new holds through a
// symbolic link was in no digest: a run went on from an earlier one though that file had changed.
// The build reads --new through its links (pathFormIn), so the digest takes the file a link
// reaches.
test("a build-cases run whose --new reaches a changed file through a symbolic link does not go on from the earlier run: --new's digest holds the file the link reaches", async () => {
  const root = path.join(scratch, "new-link");
  const linked = path.join(root, "alpha");
  const target = path.join(root, "elsewhere", "extra.md");
  mkdirSync(path.dirname(target), { recursive: true });
  writeFileSync(target, "One extra note.\n");
  cpSync(newDir, linked, { recursive: true });
  mkdirSync(path.join(linked, "references"), { recursive: true });
  symlinkSync("../../elsewhere/extra.md", path.join(linked, "references", "extra.md"));
  const out = path.join(root, "cases.jsonl");
  const args = buildArgs(sessionsFile("new-link.jsonl", ["no-step"]), out, "1").map((arg) =>
    arg === newDir ? linked : arg
  );
  expect(await main(args, io([]))).toBe(EXIT.ok);
  writeFileSync(target, "Two extra notes.\n");
  const lines: string[] = [];
  expect(await main(args, io(lines))).toBe(EXIT.refused);
  expect(lines.join("\n")).toContain(`--new was ${linked} (sha256 `);
}, 60_000);

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

// Catches: --first-read reading the empty spool of a pull that returned no row (the store holds no
// file at the path the sessions file names), so the session was dropped as unparseable or as
// having no first request, where build-cases and offeredMessages drop it as pull-failed.
test("build-tasks --first-read drops a session whose pull returns no row as pull-failed, as build-cases does", async () => {
  const absent = "/sessions/c-absent.jsonl";
  const sessions = path.join(setupDir, "control-absent.jsonl");
  writeFileSync(
    sessions,
    `${JSON.stringify({ store: "store_a", hash: fileHash(absent), path: absent })}\n`
  );
  const out = path.join(scratch, "control-absent", "tasks.jsonl");
  await main(controlArgs(sessions, out, "1", "--max", "1"), io([]));
  const summary = JSON.parse(readFileSync(`${out}.summary.json`, "utf8"));
  expect(summary.sessions).toEqual([
    expect.objectContaining({
      hash: fileHash(absent),
      outcome: "pull-failed",
      detail: "the pull returned no row",
    }),
  ]);
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
}, 60_000);

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
  // "\r\n", and a last line still being written. Each is read in chunks of 4096 bytes and of
  // SPOOL_CHUNK: a session longer than 4096 bytes puts a line across a 4096-byte edge. Only the
  // "written" text is read in chunks of 7 bytes too, which put its characters of 2 to 4 bytes
  // across chunk edges; the made-up sessions read so add nothing to that and take seconds.
  const texts = Object.keys(SESSIONS).map((name) => ({ name, text: sessionText(name) }));
  const note = { type: "custom", customType: "note", data: "é 日本 🎉 ".repeat(9) };
  texts.push({
    name: "written",
    text: `${sessionText("good")}${JSON.stringify(note)}\r\n{"type":"message","message":{"ro`,
  });
  // A line crosses a 4096-byte edge (the byte before it is no newline), and a character of more
  // than one byte crosses a 7-byte edge of the "written" text.
  expect(
    texts.some(({ text }) => {
      const bytes = Buffer.from(text);
      return bytes.length > 4096 && bytes[4095] !== 0x0a;
    })
  ).toBe(true);
  let offset = 0;
  let crossed = false;
  for (const char of texts.at(-1)?.text ?? "") {
    const size = Buffer.byteLength(char);
    crossed ||= size > 1 && Math.floor(offset / 7) !== Math.floor((offset + size - 1) / 7);
    offset += size;
  }
  expect(crossed).toBe(true);
  const refusedNames: string[] = [];
  for (const { name, text } of texts) {
    const chunks = name === "written" ? [7, 4096, SPOOL_CHUNK] : [4096, SPOOL_CHUNK];
    const file = path.join(dir, `${name}.jsonl`);
    writeFileSync(file, text);
    let whole: ReturnType<typeof readWhole>;
    try {
      whole = readWhole(text);
    } catch (error) {
      // The broken session: the same line is refused, whatever the chunk.
      for (const chunk of chunks) {
        expect(() => readTranscript(file, chunk)).toThrow((error as Error).message);
      }
      refusedNames.push(name);
      continue;
    }
    for (const chunk of chunks) {
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
}, 60_000);

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
}, 60_000);

/** An io whose peak resident memory is PEAK_BYTES until it prints a line `decided` matches, and
 *  OVER_LIMIT from then on; what it writes to standard error goes to `errors`. */
function overLimitOnce(decided: RegExp, lines: string[], errors: string[]): BuildIo {
  let over = false;
  return {
    ...io(lines),
    out: (text) => {
      lines.push(text);
      if (decided.test(text)) over = true;
    },
    err: (line) => errors.push(line),
    peakRssBytes: () => (over ? OVER_LIMIT : PEAK_BYTES),
  };
}

test("build-cases stops above 1 GiB of peak resident memory once a session is decided: the sessions still open are stopped and not decided, the cases decided before are kept, exit 3; a run under the limit goes on and decides them", async () => {
  const sessions = sessionsFile("memory-stop.jsonl", ["good", "secret", "openai"]);
  const out = path.join(scratch, "memory-stop", "cases.jsonl");
  // All three sessions are open at once, so two are still open when the first is decided.
  const args = buildArgs(sessions, out, "1", "--concurrency", "3");
  const lines: string[] = [];
  const errors: string[] = [];
  const over = overLimitOnce(/^build-cases: \d+\/\d+ /, lines, errors);
  expect(await main(args, over)).toBe(EXIT.stopped);
  const reason =
    "the build's peak resident memory, 1024 MiB, is above 1 GiB, so no new session starts";
  expect(errors.join("\n")).toContain(`build-cases: stopped: ${reason}`);
  const stopped = summaryOf(out);
  expect(stopped.stopped).toBe(reason);
  expect(stopped.peakRssBytes).toBe(OVER_LIMIT);
  const [first, ...open] = stopped.sessions;
  expect(first?.outcome).toStartWith("case ");
  expect(open.map((each) => each.outcome)).toEqual([
    "stopped before its case was built: the peak memory passed 1 GiB",
    "stopped before its case was built: the peak memory passed 1 GiB",
  ]);
  expect(loadCases(out, old).map((c) => `case ${c.id}`)).toEqual([first?.outcome]);
  expect(lines.at(-1)).toContain(`, peak memory 1024 MiB; written to ${out}`);
  expect(existsSync(`${out}.spool`)).toBe(false);

  // A summary written before the peak was recorded holds no peakRssBytes; a run goes on from it.
  const summaryFile = `${out}.summary.json`;
  const older = JSON.parse(readFileSync(summaryFile, "utf8")) as Record<string, unknown>;
  writeFileSync(summaryFile, JSON.stringify({ ...older, peakRssBytes: undefined }));
  const again: string[] = [];
  expect(await main(args, io(again))).toBe(EXIT.ok);
  expect(again.join("")).toContain("1 sessions decided there (1 cases)");
  const done = summaryOf(out);
  expect(done.stopped).toBeNull();
  expect(done.peakRssBytes).toBe(PEAK_BYTES);
  expect(done.sessions.map((each) => each.hash)).toEqual(stopped.sessions.map((each) => each.hash));
  expect(done.sessions.every((each) => each.outcome.startsWith("case "))).toBe(true);
  expect(loadCases(out, old)).toHaveLength(3);
}, 120_000);

test("build-cases --written stops above 1 GiB of peak resident memory once a task is decided, with the cases built so far written, exit 3", async () => {
  const out = path.join(scratch, "written-memory", "cases.jsonl");
  const context = `store_a:${fileHash(sessionPath("good"))}`;
  const args = writtenArgs(
    out,
    "--context-session",
    context,
    "--session-cases",
    sessionCasesFile()
  );
  const lines: string[] = [];
  const errors: string[] = [];
  const over = overLimitOnce(/^build-cases: written task \d+ of /, lines, errors);
  expect(await main(args, over)).toBe(EXIT.stopped);
  const reason =
    "the build's peak resident memory, 1024 MiB, is above 1 GiB, so no new task starts";
  expect(errors.join("\n")).toContain(`build-cases: stopped: ${reason}`);
  const summary = summaryOf<WrittenSummary>(out);
  expect(summary.stopped).toBe(reason);
  expect(summary.peakRssBytes).toBe(OVER_LIMIT);
  // Task 1 is a case, as in the written build above, and no task after it is read.
  const id = `skills-1-${pathHash(sessionPath("good")).slice(0, 12)}-w1`;
  expect(summary.tasks.map((each) => [each.task, each.outcome])).toEqual([[1, `case ${id}`]]);
  expect(loadCases(out, old).map((c) => c.id)).toEqual([id]);
  expect(lines.at(-1)).toContain(`, peak memory 1024 MiB; written to ${out}`);
}, 60_000);

// The task's architect (2026-11-25T18:54:22Z and 19:02:42Z): cases come from Postgres sessions
// alone, so a build that ends with no case says how many of the skill's counted sessions only the
// transcript store holds, which were never case sources; the walk copies that line into the
// skill's no-case report.
test("with --store-only, a build with no case ends its summary line, and its stop line, with the skill's store-only sessions, and with none store-only says nothing of them; a file that is not one is refused before any read, and so is the option with --written", async () => {
  /** Writes `content` (JSON of a value, or a text as it is) as attribute's
   *  `<cut id>/store-only/<skill>.json` of alpha, and returns the file. */
  const storeOnlyFile = (name: string, content: unknown) => {
    const file = path.join(setupDir, name);
    writeFileSync(file, typeof content === "string" ? content : JSON.stringify(content));
    return file;
  };
  const some = storeOnlyFile("store-only-some.json", {
    skill: "alpha",
    counted: 40,
    storeOnly: 3,
    hosts: [
      { host: "host-a", sessions: 2 },
      { host: "host-b", sessions: 1 },
    ],
  });
  const none = storeOnlyFile("store-only-none.json", {
    skill: "alpha",
    counted: 12,
    storeOnly: 0,
    hosts: [],
  });
  const words =
    "; 3 of the skill's 40 counted sessions are store-only, so they were never case sources (host-a: 2, host-b: 1)";

  // A build that ends with no case, not stopped.
  const sessions = absentSessions("store-only-absent.jsonl", 3);
  const ended = async (name: string, file: string) => {
    const out = path.join(scratch, name, "cases.jsonl");
    const lines: string[] = [];
    const args = buildArgs(sessions, out, "1", "--concurrency", "1", "--store-only", file);
    expect(await main(args, io(lines))).toBe(EXIT.ok);
    return {
      lines,
      summary: `build-cases: 0 cases of 3 sessions tried, $0.0000 of readers at list price, peak memory 200 MiB; written to ${out} and ${out}.summary.json`,
    };
  };
  const withSome = await ended("store-only-some", some);
  expect(withSome.lines).toContain(`${withSome.summary}${words}\n`);
  const withNone = await ended("store-only-none", none);
  expect(withNone.lines).toContain(`${withNone.summary}\n`);

  // A build the stop rule stops with no case.
  const stoppedOut = path.join(scratch, "store-only-stopped", "cases.jsonl");
  const stoppedLines: string[] = [];
  const stopArgs = buildArgs(
    absentSessions("store-only-stopped.jsonl", STOP_AFTER_SESSIONS + 5),
    stoppedOut,
    "1",
    "--max",
    "15",
    "--concurrency",
    "1",
    "--store-only",
    some
  );
  expect(await main(stopArgs, io(stoppedLines))).toBe(EXIT.stoppedByCost);
  expect(stoppedLines).toContain(
    `build-cases: stopped by --stop-projected-usd: ${STOP_AFTER_SESSIONS} sessions decided, 0 cases, $0.0000 of readers spent, projected unbounded to --max 15 cases${words}`
  );

  // A file that is not JSON, or not of its shape, is refused before any store read or call, as
  // behaviour report refuses it; the written form, which builds no case from a sessions file,
  // refuses the option.
  const from = recorder.requests.length;
  const logged = readFileSync(psqlLog, "utf8").length;
  const refusedOut = path.join(scratch, "store-only-refused", "cases.jsonl");
  for (const [file, why] of [
    [storeOnlyFile("store-only-not-json.json", "{ counted: 40"), "is not JSON: "],
    [
      storeOnlyFile("store-only-shape.json", { skill: "alpha", counted: 40 }),
      "is not a skill's store-only sessions (attribute's <cut id>/store-only/<skill>.json): storeOnly: ",
    ],
  ] as const) {
    const lines: string[] = [];
    const args = buildArgs(sessions, refusedOut, "1", "--store-only", file);
    expect(await main(args, io(lines))).toBe(EXIT.refused);
    expect(lines.join("\n")).toContain(`--store-only ${file} ${why}`);
  }
  const written: string[] = [];
  const context = ["--context-session", `store_a:${fileHash(sessionPath("good"))}`];
  expect(await main(writtenArgs(refusedOut, ...context, "--store-only", some), io(written))).toBe(
    EXIT.refused
  );
  expect(written.join("\n")).toContain(
    "--written writes the cases of one context session, so it takes no --store-only"
  );
  expect(recorder.requests.length).toBe(from);
  expect(readFileSync(psqlLog, "utf8").length).toBe(logged);
  expect(existsSync(refusedOut)).toBe(false);
}, 60_000);

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
}, 60_000);

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

/** The case of the session `name` (good or legion) among `profileCases`. */
async function profileCase(name: "good" | "legion"): Promise<Case> {
  return (await profileCases()).find(
    (c) => c.source.pathHash === pathHash(sessionPath(name))
  ) as Case;
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
    pathHash: pathHash(`${DEV_SESSIONS}/task-4.jsonl`),
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
      pathHash: pathHash(`${DEV_SESSIONS}/task-1.jsonl`),
      reason: "skill-not-listed",
      detail: "its context subagent:task does not list alpha",
      context: "subagent:task",
    },
  ]);
  expect(existsSync(`${out}.spool`)).toBe(false);
}, 120_000);

// Catches: a context session refused (exit 2) after the store read that found it, where exit 2 is
// a refusal before any store read.
test("build-tasks fails (exit 1) on a --context-session whose rebuilt context does not list the skill, naming why, before any reader call", async () => {
  const out = path.join(scratch, "profiles-unlisted-context", "tasks.jsonl");
  const unlisted = fileHash(`${DEV_SESSIONS}/task-1.jsonl`);
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
  expect(code).toBe(EXIT.failed);
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
}, 60_000);

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
      pathHash: pathHash(`${DEV_SESSIONS}/task-4.jsonl`),
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

// Catches: build-tasks reading --skill's SKILL.md as UTF-8 with a leading byte order mark kept, so
// its frontmatter was not found and the skill was named after its directory, where Oh My Pi drops
// the mark and reads the frontmatter name.
test("build-tasks names a --skill whose SKILL.md starts with a byte order mark by its frontmatter name, as Oh My Pi reads it", async () => {
  const marked = path.join(scratch, "marked-skill", "marked-alpha");
  mkdirSync(marked, { recursive: true });
  writeFileSync(
    path.join(marked, "SKILL.md"),
    `\uFEFF${readFileSync(path.join(old, "SKILL.md"), "utf8")}`
  );
  const out = path.join(scratch, "marked-skill", "tasks.jsonl");
  await main(
    tasksArgs(out).map((arg) => (arg === old ? marked : arg)),
    io([])
  );
  expect(JSON.parse(readFileSync(`${out}.summary.json`, "utf8")).skill).toBe("alpha");
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
        pathHash: pathHash(`${DEV_SESSIONS}/task-1.jsonl`),
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
}, 60_000);

test("build-tasks with no bound writes the pinned bytes: the tasks, the summary, the profiles and the printed lines of a run with --model and of one with --profiles-out", async () => {
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
  // Fixture path hashes changed when the transcripts moved under their real profile roots.
  // The summary records their profiles. The profile files keep the same bytes. The profiles
  // summary counts a subagents.sql query for each of the two session tasks, read before its
  // profile is built (sessionQueries 2); one query of their store after both was counted before.
  // The profiles summary quotes LEGION_IMAGE_RULE, which now states the deployment-built image's
  // steps: the stage its final stage copies the worker's packages from (by COPY, an ADD into one
  // a write of it too, a later RUN that names one too unless readSteps lists its text's hash),
  // else the base it is on; the packages it names are those the profile uses (the prefix rows
  // under /opt/legion/ that an Oh My Pi unit places), not a row only Claude Code units place.
  expect(digests).toEqual({
    "model tasks": "7065771b7768b39f531ae513e501837a0837349affe5672950558cdaf8230f9e",
    "model summary": "830ba5a3c2d161a489386c91ee25396de33d21df9deabfb10e8e915059c49c48",
    "model printed": "8d8a65fe70015b199ecc51a86a5803ef39c5cacbddbe124b22460e6d36a2971b",
    "profiles tasks": "c29cedaadf7a508a1449548b98da93ab14b079a002eb834365c81cc2f70e79f6",
    "profiles summary": "ff7c38df75e14f47f36df06b063050adfcc9a61995c9ddc87d2a718f20c83873",
    "profiles printed": "9ceae9fd265f889f404904ada8fe26a18ca2cbde3e9141337bd67b87b1aa1b0b",
    "profiles profiles": "26bf1079e50926d2c9e1bdc9482bca041b0a71392fb091f4352c437ab81932c4",
  });
}, 120_000);

/** A stand-in store holding the suite's files (by store, then path) as `change` leaves them, and
 *  the path of a configuration whose store command reads it; its calls go to the suite's log. */
function changedStore(
  name: string,
  change: (files: Record<string, Record<string, string>>) => void
): string {
  const dir = path.join(setupDir, name);
  mkdirSync(dir, { recursive: true });
  const stores = JSON.parse(readFileSync(path.join(setupDir, "store.json"), "utf8")) as Record<
    string,
    { files: Record<string, string> }
  >;
  change(Object.fromEntries(Object.entries(stores).map(([db, store]) => [db, store.files])));
  writeFileSync(path.join(dir, "store.json"), JSON.stringify(stores));
  writeFileSync(path.join(dir, "psql.ts"), STANDIN_PSQL(path.join(dir, "store.json"), psqlLog));
  const psql = path.join(dir, "psql");
  writeFileSync(
    psql,
    `#!/bin/sh\nexec '${process.execPath}' '${path.join(dir, "psql.ts")}' "$@"\n`
  );
  chmodSync(psql, 0o755);
  const file = path.join(dir, "skill-cuts.json");
  writeFileSync(
    file,
    JSON.stringify({ ...JSON.parse(readFileSync(config, "utf8")), psqlCommand: psql })
  );
  return file;
}

const POD_BASE = `ghcr.io/acme/worker@sha256:${"1".repeat(64)}`;
const POD_PIN = `registry.example/acme/session@sha256:${"2".repeat(64)}`;
const POD_BUILD = "d5".repeat(20);
/** The rootfs.diff_ids of POD_BASE, and those of POD_PIN, built on it: POD_BASE's and one more. */
const POD_LAYERS = [1, 2, 3].map((n) => `sha256:${String(n).repeat(64)}`);
const POD_PIN_LAYERS = [...POD_LAYERS, `sha256:${"4".repeat(64)}`];

/** A `read` call of `url` (`skill://<name>`) and its successful result, which starts
 *  `[Skill file: <header>]`, as a case's two messages: as `readFirst` puts them in a transcript. */
function skillFileRead(url: string, header: string): SessionMessage[] {
  const callId = `toolu_${url.slice("skill://".length)}`;
  return [
    {
      role: "assistant",
      content: [{ type: "toolCall", id: callId, name: "read", arguments: { path: url } }],
    },
    {
      role: "toolResult",
      toolCallId: callId,
      toolName: "read",
      isError: false,
      content: [{ type: "text", text: `[Skill file: ${header}]\n# ${url}\n` }],
    },
  ];
}

async function podProfile(
  name: string,
  profileName: string,
  {
    task = false,
    label = true,
    legion = false,
    readAt,
    pinLayers = POD_PIN_LAYERS,
    home = "/home/fixture",
    devboxHome,
    reads = [],
    byPath,
    podLinks = ["/opt/legion/pi-acme"],
    manifests = HOME_MANIFESTS,
  }: {
    task?: boolean;
    label?: boolean;
    legion?: boolean;
    readAt?: string;
    /** The rootfs.diff_ids the registry gives of POD_PIN; no config of it when `unread`. */
    pinLayers?: readonly string[] | "unread";
    /** The home directory whose `.omp/` holds the session's transcript. */
    home?: string;
    /** The configuration's devboxHome; the configuration names none when absent. */
    devboxHome?: string;
    /** `[Skill file: …]` reads put after the prompt, in this order, before alpha's. */
    reads?: readonly { readonly url: string; readonly header: string }[];
    /** The directory the session reads alpha's two files from whole by their paths, in place of
     *  its skill:// reads: the case's pathCopy. */
    byPath?: string;
    /** The directories the session pod's Dockerfile links into the profile. */
    podLinks?: readonly string[];
    /** The manifests.tsv rows added to the suite's copies output. */
    manifests?: readonly (readonly (string | number)[])[];
  } = {}
) {
  const file = `${home}/.omp/${profileName === "" ? "" : `profiles/${profileName}/`}agent/sessions/--work--/${name}.jsonl`;
  const db = legion ? "store_b" : "store_a";
  const template = await profileCase(legion ? "legion" : "good");
  const cfg = changedStore(name, (files) => {
    const text = sessionText(legion ? "legion" : "good");
    const target = legion ? "/legion/workspaces/acme/skills/ACME-12" : "/work";
    const oldRoot = legion ? "/opt/legion/pi-acme/dist/skills/alpha" : `${F1_SKILLS}/alpha`;
    const readRoot = legion
      ? `${target}/.omp/skills/alpha`
      : (readAt ?? "/opt/legion/pi-acme/dist/skills/alpha");
    const entries = inDirectory(text, target)
      .replaceAll(oldRoot, readRoot)
      .split("\n")
      .filter((line) => line !== "")
      .map((line) => JSON.parse(line) as Record<string, unknown>);
    if (byPath !== undefined) readsUnder(byPath)(entries);
    // Each read goes right after the prompt, so the last is put first.
    for (const [n, read] of [...reads.entries()].reverse()) {
      readFirst(`b0${n}0000`, read.url, read.header, `# ${read.url}\n`)(entries);
    }
    const store = files[db] ?? {};
    store[file] = entries.map((entry) => `${JSON.stringify(entry)}\n`).join("");
  });
  writeFileSync(
    cfg,
    JSON.stringify({
      ...JSON.parse(readFileSync(cfg, "utf8")),
      sessionPod: {
        repository: "acme/infra",
        pinFile: "pods/Pulumi.prod.yaml",
        pinKey: "pods:image",
        dockerfile: "pods/Dockerfile",
        workerRepository: "acme/skills",
        workerDockerfile: "worker.Dockerfile",
      },
      ...(devboxHome === undefined ? {} : { devboxHome }),
    })
  );
  const copies = copiesWith(`copies-${name}`, { "manifests.tsv": manifests });
  const dir = path.join(scratch, name);
  mkdirSync(dir, { recursive: true });
  const from = path.join(dir, "before.jsonl");
  const out = path.join(dir, "after.jsonl");
  const lines: string[] = [];
  const standin = io(lines);
  const options: BuildIo = {
    ...standin,
    imageConfig: async (image: string) => {
      if (image === POD_PIN)
        return pinLayers === "unread" ? undefined : { env: [], labels: {}, diffIds: pinLayers };
      expect(image).toBe(POD_BASE);
      const labels: Record<string, string> = {};
      if (label) labels["org.opencontainers.image.revision"] = IMAGE_SHA;
      return { env: ["OMP_PROFILE=legion"], labels, diffIds: POD_LAYERS };
    },
    gh: async (args: readonly string[]) => {
      const route = args.find((arg) => arg.startsWith("repos/")) ?? "";
      const answer = (stdout: string): GhResult => ({ code: 0, stdout, stderr: "" });
      if (route === "repos/acme/infra/contents/pods/Pulumi.prod.yaml")
        return answer(
          `config:\n  # The job built commit ${POD_BUILD}.\n  pods:image: ${POD_PIN}\n`
        );
      if (route === "repos/acme/infra/contents/pods/Dockerfile") {
        expect(args).toContain(`ref=${POD_BUILD}`);
        return answer(
          `ARG WORKER=${POD_BASE}\nFROM \${WORKER}\n${podLinks.map((dir) => `RUN omp plugin link ${dir}\n`).join("")}`
        );
      }
      if (route.includes(`sha=${POD_BUILD}`)) return answer(POD_BUILD);
      if (route === "repos/acme/skills/contents/worker.Dockerfile")
        return answer(
          "FROM scratch\nENV OMP_PROFILE=legion\nCOPY --from=plugin /out/pi-acme /opt/legion/pi-acme\n"
        );
      return standin.gh(args);
    },
  };
  const source = { store: db, pathHash: pathHash(file) };
  if (task) {
    writeFileSync(
      from,
      `${JSON.stringify({ id: "t1", text: TASK_ROWS[0]?.text, label: "need", source: "session", ...source })}\n`
    );
    writeFileSync(
      `${from}.summary.json`,
      JSON.stringify({ command: "build-tasks", skill: "alpha" })
    );
    const profiles = path.join(dir, "profiles");
    const args = reprofileArgs(cfg, from, profiles).map((arg) =>
      arg === copiesDir ? copies : arg
    );
    const code = await main(args, options);
    const profileFile = path.join(profiles, "t1.json");
    return {
      code,
      lines,
      profile: existsSync(profileFile)
        ? (JSON.parse(readFileSync(profileFile, "utf8")) as Case["profile"])
        : undefined,
    };
  }
  const messages = template.messages.map((message) =>
    JSON.parse(
      JSON.stringify(message).replaceAll(
        legion ? "/opt/legion/pi-acme/dist/skills/alpha" : `${F1_SKILLS}/alpha`,
        legion
          ? "/legion/workspaces/acme/skills/ACME-12/.omp/skills/alpha"
          : (readAt ?? "/opt/legion/pi-acme/dist/skills/alpha")
      )
    )
  );
  if (byPath !== undefined) {
    readsUnder(byPath)(messages.map((message) => ({ type: "message", message })));
  }
  const added = reads.flatMap((read) => skillFileRead(read.url, read.header));
  messages.splice(1, 0, ...added);
  const skillReads = template.skillReads.map((index) =>
    index >= 1 ? index + added.length : index
  );
  writeFileSync(
    from,
    `${JSON.stringify({
      ...template,
      source,
      messages,
      skillReads,
      ...(byPath === undefined ? {} : { pathCopy: { dir: byPath } }),
    })}\n`
  );
  const code = await main(
    [
      "build-cases",
      "--config",
      cfg,
      "--old",
      old,
      "--copies",
      copies,
      "--count",
      countDir,
      "--reprofile",
      from,
      "--out",
      out,
    ],
    options
  );
  const [built] = code === EXIT.ok ? loadCases(out, old) : [];
  const evidence = Object.fromEntries(
    built?.evidence?.map((each) => [each.item, each.source]) ?? []
  );
  return { code, profile: built?.profile, lines, evidence };
}

test("a default-profile session of a devbox store gets the dev box's installers' units, and a legion-profile one the skills the session pod's image stages, at the image's commit", async () => {
  const pod = await podProfile("pod-case", "legion");
  const dev = await podProfile("dev-case", "");
  expect(dev.profile?.skills.map((skill) => skill.name)).toContain("unread");
  expect(pod.profile?.skills.map(({ name, commit }) => [name, commit])).toEqual(
    ["alpha", "beta", "omicron"].map((name) => [name, IMAGE_SHA])
  );
}, 60_000);

test("a solreview-profile session of a Legion store gets its session repository's project skills and no role plugins", async () => {
  const files = REPOSITORIES["acme/skills"] ?? {};
  files[".omp/skills/alpha/SKILL.md"] = projectSkillMd("alpha");
  try {
    const result = await podProfile("review-case", "solreview", { legion: true });
    expect(result.profile?.plugins).toEqual([]);
    expect(result.profile?.skills.map(({ name, path }) => [name, path])).toEqual([
      ["alpha", ".omp/skills/alpha"],
    ]);
  } finally {
    delete files[".omp/skills/alpha/SKILL.md"];
  }
}, 60_000);

test("a legion-profile pod task gets the skills the session pod's image stages, at the image's commit", async () => {
  const result = await podProfile("pod-task", "legion", { task: true });
  expect(result.profile?.skills.map(({ name, commit }) => [name, commit])).toEqual(
    ["beta", "omicron"].map((name) => [name, IMAGE_SHA])
  );
}, 60_000);

test("a session pod whose base carries no OCI revision label, and whose Dockerfile's pin comment names no commit, is not measurable, naming its Dockerfile and its base", async () => {
  const result = await podProfile("unresolved-base", "legion", { label: false });
  expect(result.code).toBe(EXIT.failed);
  expect(result.lines.join("\n")).toContain(
    `pod profile not measurable: pods/Dockerfile at ${POD_BUILD} pins ${POD_BASE}, which has no OCI revision or commit in the pin history`
  );
}, 60_000);

// Catches: a session pod read at the commit of the base its Dockerfile's ARG default names,
// though a build can pass another base: the pinned image was never read, so an image built on
// another base was taken for one built on that base.
test("a pinned image whose rootfs.diff_ids do not start with its base's is not measurable, naming its digest and the mismatch, one whose config the registry does not give is not measurable, naming why, and one whose diff ids start with its base's gets the image's skills", async () => {
  const pinDigest = `sha256:${"2".repeat(64)}`;
  const other = [POD_LAYERS[0] ?? "", `sha256:${"9".repeat(64)}`, ...POD_PIN_LAYERS.slice(2)];
  const mismatch = await podProfile("pin-other-base", "legion", { pinLayers: other });
  expect(mismatch.code).toBe(EXIT.failed);
  expect(mismatch.lines.join("\n")).toContain(
    `pod profile not measurable: pods/Dockerfile at ${POD_BUILD} names the base ${POD_BASE}, but the 3 rootfs.diff_ids of ${POD_BASE} are not the first 3 of the 4 of ${POD_PIN} (entry 2 differs), so the pinned image ${pinDigest} is not built on it`
  );
  const unread = await podProfile("pin-unread", "legion", { pinLayers: "unread" });
  expect(unread.code).toBe(EXIT.failed);
  expect(unread.lines.join("\n")).toContain(
    `pod profile not measurable: the runner cannot read the OCI config of the pinned image ${pinDigest} (${POD_PIN}) from its registry, so it cannot confirm that the image is built on ${POD_BASE}, the base pods/Dockerfile at ${POD_BUILD} names`
  );
  const matched = await podProfile("pin-same-base", "legion");
  expect(matched.profile?.skills.map(({ name, commit }) => [name, commit])).toEqual(
    ["alpha", "beta", "omicron"].map((name) => [name, IMAGE_SHA])
  );
  expect(matched.evidence?.["session pod profile legion"]).toContain(
    `pods/Dockerfile at that commit uses ${POD_BASE}; the 3 rootfs.diff_ids of ${POD_BASE} are the first 3 of the 4 of ${POD_PIN}, so the pinned image is built on it.`
  );
}, 120_000);

// Catches: vskills-293's porting-released-proofs, whose read of the project's own
// .claude/skills/porting-released-proofs/SKILL.md the context rule placed at a plugin holding a skill
// of that name, so the profile installed that plugin whole (20 of the 21 names it added).
test("a read inside the session checkout's own .claude/skills is that project's skill, and installs no unit that holds a skill of its name", async () => {
  const project = await podProfile("project-skill-read", "", {
    readAt: "/work/.claude/skills/alpha",
  });
  // The read installs no unit, so the profile has no alpha of acme/skills: the case is left out.
  expect(project.code).toBe(EXIT.failed);
  expect(project.lines.join("\n")).toContain(
    "left out: the profile installs no alpha of acme/skills at the session's start"
  );
  // The same read at the plugin's own install location installs it.
  const plugin = await podProfile("plugin-skill-read", "");
  expect((plugin.profile?.skills ?? []).map((skill) => skill.name)).toContain("beta");
}, 60_000);

// The task's architect, 2026-11-25: a session's context follows where its skills came from. A
// named-profile session of a devbox store whose [Skill file: …] headers are under the dev box's
// home ran in a pod that mounts that home, whose Oh My Pi loaded the dev box's skills, so it gets
// the dev box's context; one with no such header gets the session pod's. In every context a
// [Skill file: …] header places its unit, as direct proof; a read by path does so only where pod
// links do not decide.

/** The dev box's home a pod can mount (the configuration's devboxHome), and the home of the pod's
 *  own user, whose named profile holds the pod's transcripts. */
const DEV_HOME = "/home/dev";
const POD_HOME = "/home/agent";
/** acme/skills's alpha where the dev box's Oh My Pi has it, among its user skills. */
const HOME_ALPHA = `${DEV_HOME}/.omp/agent/skills/alpha`;
/** acme/skills's beta where the worker image's package stages it: a pod's own path. */
const POD_BETA = "/opt/legion/pi-acme/dist/skills/beta/SKILL.md";
/** acme/skills's beta among the user skills of a home whose path starts with DEV_HOME's text and
 *  is not in it. */
const OTHER_HOME_BETA = "/home/dev2/.omp/agent/skills/beta/SKILL.md";
/** The evidence items that name the context of a pod that mounts the dev box's home, and that of
 *  such a pod's session that read skill files on the pod's own paths too, each followed by the
 *  profile's name. */
const MOUNTED_HOME = "a pod that mounts the dev box's home, profile ";
const MIXED_HEADERS = "skill files from the dev box's home and the pod's own paths, profile ";
/** The unit by which the dev box's plugin list (acme/dots's) installs acme/powers. */
const DOTS_POWERS = "unit package:acme-powers (acme/dots:omp/plugins/package.json)";

/** The case of a legion-profile session under POD_HOME in the devbox store store_a that reads
 *  pack-one of the dev box's pool (PACK_ONE) and then alpha, as the dev box's Oh My Pi links it
 *  (HOME_ALPHA): its [Skill file: …] headers are under DEV_HOME. acme/dots is the dev box's
 *  repository, and the session pod's image links nothing into the profile. */
const homeMounted = (name: string, options: Parameters<typeof podProfile>[2] = {}) =>
  podProfile(name, "legion", {
    home: POD_HOME,
    readAt: HOME_ALPHA,
    reads: [{ url: "skill://pack-one", header: PACK_ONE }],
    podLinks: [],
    manifests: DOTS_MANIFESTS,
    ...options,
  });

/** The profile's skills of `result`, each as `<name> <repo> <commit>`. */
const skillsOf = (result: { readonly profile?: Case["profile"] | undefined }) =>
  (result.profile?.skills ?? []).map((skill) => `${skill.name} ${skill.repo} ${skill.commit}`);

/** `result`'s exit status, with what it printed where it is no success. */
const outcomeOf = (result: { readonly code: number; readonly lines: readonly string[] }) => ({
  code: result.code,
  said: result.code === EXIT.ok ? "" : result.lines.join("\n"),
});

// Catches: a session of a pod that mounts the dev box's home given the session pod's profile,
// which installs the image's links alone, so it lacks the copy the session read and the case is
// left out.
test("a named-profile session of a devbox store whose [Skill file] headers are under devboxHome gets the dev box's context: its installers' units, the pool a header names at its installer's pin, the copy the case read, and the evidence item that names the context", async () => {
  const result = await homeMounted("home-mounted", { devboxHome: DEV_HOME });
  expect(outcomeOf(result)).toEqual({ code: EXIT.ok, said: "" });
  const skills = skillsOf(result);
  // The copy the case read, as the dev box's Oh My Pi linked it at the session's start.
  expect(skills).toContain(`alpha acme/skills ${SHA}`);
  // The pool the header names, at the commit the dev box's installer pins.
  expect(skills).toContain(`pack-one acme/pack ${PACK_PIN}`);
  // A unit the dev box's installers install, which no header names.
  expect(skills).toContain(`power-two acme/powers ${SHA}`);
  expect(result.evidence?.[DOTS_POWERS]).toStartWith(
    "a dev box installs every unit the installers of its own repository install into Oh My Pi at the session's start"
  );
  expect(result.evidence?.[`${MOUNTED_HOME}legion`]).toStartWith(
    `3 [Skill file: …] headers of the session are under the dev box's home ${DEV_HOME} (the configuration's devboxHome), the first at message 2: ${PACK_ONE}; `
  );
  expect(result.evidence?.["session pod profile legion"]).toBeUndefined();
  expect(result.evidence?.[`${MIXED_HEADERS}legion`]).toBeUndefined();
}, 60_000);

// Guards the session pod's branch: devboxHome changes nothing for a session whose headers are all
// on the pod's own paths.
test("a named-profile session of a devbox store whose [Skill file] headers are all on the pod's own paths gets the session pod's profile", async () => {
  const result = await podProfile("pod-only", "legion", { home: POD_HOME, devboxHome: DEV_HOME });
  expect(outcomeOf(result)).toEqual({ code: EXIT.ok, said: "" });
  expect(result.profile?.skills.map(({ name, commit }) => [name, commit])).toEqual(
    ["alpha", "beta", "omicron"].map((name) => [name, IMAGE_SHA])
  );
  expect(result.evidence?.["session pod profile legion"]).toStartWith("Inference: ");
  expect(result.evidence?.[`${MOUNTED_HOME}legion`]).toBeUndefined();
}, 60_000);

// Catches: a session that read skill files under the dev box's home and on the pod's own paths
// given one context with no word of the other, or the pod path's unit left out of it.
test("a session with [Skill file] headers under devboxHome and on the pod's own paths gets the dev box's context, the unit its pod-path header names, and an evidence item naming a header of each kind", async () => {
  const result = await homeMounted("home-mixed", {
    devboxHome: DEV_HOME,
    reads: [
      { url: "skill://pack-one", header: PACK_ONE },
      { url: "skill://beta", header: POD_BETA },
    ],
  });
  expect(outcomeOf(result)).toEqual({ code: EXIT.ok, said: "" });
  expect(result.evidence?.[`${MOUNTED_HOME}legion`]).toStartWith(
    `3 [Skill file: …] headers of the session are under the dev box's home ${DEV_HOME} (the configuration's devboxHome), the first at message 2: ${PACK_ONE}; `
  );
  expect(result.evidence?.[`${MIXED_HEADERS}legion`]).toStartWith(
    `message 2 reads [Skill file: ${PACK_ONE}], under the dev box's home ${DEV_HOME}, and message 4 reads [Skill file: ${POD_BETA}], outside it; `
  );
  // The worker image's package, which the pod-path header places.
  expect(
    result.evidence?.["unit package:@acme/pi-acme (acme/skills:packages/pi-acme/package.json)"]
  ).toStartWith(
    `message 4 reads [Skill file: ${POD_BETA}], under the prefix-table.tsv row /opt/legion/pi-acme/dist/skills/beta/`
  );
  expect(result.profile?.plugins.map((plugin) => plugin.name)).toContain("pi-acme");
  expect(skillsOf(result)).toContain(`power-two acme/powers ${SHA}`);
  expect(result.evidence?.["session pod profile legion"]).toBeUndefined();
}, 60_000);

// Catches: a [Skill file] header in a session pod's context placing no unit, since the pod's
// links alone decided its units; and a home whose path only starts with devboxHome's text taken
// for the dev box's.
test("a session pod's [Skill file] header outside devboxHome installs the unit it places beside the image's links, and the context stays the session pod's", async () => {
  const result = await podProfile("pod-proof", "legion", {
    home: POD_HOME,
    devboxHome: DEV_HOME,
    reads: [{ url: "skill://beta", header: OTHER_HOME_BETA }],
  });
  expect(outcomeOf(result)).toEqual({ code: EXIT.ok, said: "" });
  const skills = skillsOf(result);
  for (const name of ["alpha", "beta", "omicron"]) {
    expect(skills).toContain(`${name} acme/skills ${IMAGE_SHA}`);
  }
  // acme/skills's user skills, which the header places: older is theirs alone, and the image's
  // package ships none of it.
  expect(skills).toContain(`older acme/skills ${IMAGE_SHA}`);
  expect(result.evidence?.["unit user-skills:omp (acme/skills:installers/skills.sh)"]).toStartWith(
    `message 2 reads [Skill file: ${OTHER_HOME_BETA}], under the prefix-table.tsv row **/agent/skills/beta/`
  );
  expect(result.evidence?.["session pod profile legion"]).toStartWith("Inference: ");
  expect(result.evidence?.[`${MOUNTED_HOME}legion`]).toBeUndefined();
}, 60_000);

// Guards the rule pod links keep: a whole read by path in a session pod's context places no unit,
// even of a copy under devboxHome, and decides no context.
test("a session pod's whole reads by path of a copy the image's links do not give install nothing of it, and reads by path under devboxHome do not make the context the dev box's", async () => {
  const result = await podProfile("pod-by-path", "legion", {
    home: POD_HOME,
    devboxHome: DEV_HOME,
    byPath: HOME_ALPHA,
  });
  expect(outcomeOf(result)).toEqual({ code: EXIT.ok, said: "" });
  expect(result.profile?.skills.map(({ name, commit }) => [name, commit])).toEqual(
    ["alpha", "beta", "omicron"].map((name) => [name, IMAGE_SHA])
  );
  expect(result.evidence?.["unit user-skills:omp (acme/skills:installers/skills.sh)"]).toBe(
    undefined
  );
  expect(result.evidence?.["session pod profile legion"]).toStartWith("Inference: ");
  expect(result.evidence?.[`${MOUNTED_HOME}legion`]).toBeUndefined();
}, 60_000);

// Catches: the dev box's context given with no devboxHome in the configuration, or the session
// pod's context placing no unit of the session's [Skill file] headers.
test("with no devboxHome, a named-profile session whose [Skill file] headers are under the dev box's home keeps the session pod's context, its headers install their units, and the dev box's installers install nothing", async () => {
  const result = await homeMounted("home-unconfigured");
  expect(outcomeOf(result)).toEqual({ code: EXIT.ok, said: "" });
  const skills = skillsOf(result);
  expect(skills.some((skill) => skill.startsWith("alpha acme/skills "))).toBe(true);
  expect(skills.some((skill) => skill.startsWith("pack-one acme/pack "))).toBe(true);
  expect(skills.some((skill) => skill.startsWith("power-two "))).toBe(false);
  expect(result.evidence?.["unit user-skills:omp (acme/skills:installers/skills.sh)"]).toStartWith(
    `message 4 reads [Skill file: ${HOME_ALPHA}/SKILL.md], under the prefix-table.tsv row **/agent/skills/alpha/`
  );
  expect(
    result.evidence?.["unit pool:vendor/acme-pack (acme/dots:skills-sources.json)"]
  ).toStartWith(
    `message 2 reads [Skill file: ${PACK_ONE}], under the prefix-table.tsv row **/vendor/acme-pack/skills/pack-one/`
  );
  expect(result.evidence?.[DOTS_POWERS]).toBeUndefined();
  expect(result.evidence?.["session pod profile legion"]).toStartWith("Inference: ");
  expect(result.evidence?.[`${MOUNTED_HOME}legion`]).toBeUndefined();
}, 60_000);

/** `text`, a session's transcript, with its header's working directory `cwd`. */
const inDirectory = (text: string, cwd: string) =>
  text
    .split("\n")
    .map((line) => {
      if (line === "") return line;
      const entry = JSON.parse(line) as Record<string, unknown>;
      return entry.type === "session" ? JSON.stringify({ ...entry, cwd }) : line;
    })
    .join("\n");

/** The good session's transcript in the working directory `cwd`, with a successful read of the
 *  file `file` by its absolute path after its prompt (its messages 1 and 2). */
function readingIn(cwd: string, file: string): string {
  const lines = sessionText("good")
    .split("\n")
    .filter((line) => line !== "")
    .map((line) => JSON.parse(line) as Record<string, unknown>);
  callAfterPrompt("f11e000", "toolu_file", "read", { path: file }, "# Made-up notes\n")(lines);
  return inDirectory(lines.map((line) => `${JSON.stringify(line)}\n`).join(""), cwd);
}

test("a session repository whose whole tree GitHub cuts short is read one directory at a time, only where Oh My Pi reads a project, so build-cases --reprofile gives its case that repository's project skill and context file", async () => {
  const legion = await profileCase("legion");
  // The legion session, in a workspace of acme/big, whose whole tree the stand-in cuts short.
  const moved = `${LEGION_SESSIONS}/big-legion.jsonl`;
  const big = changedStore("truncated-tree", (files) => {
    (files.store_b ?? {})[moved] = inDirectory(
      sessionText("legion"),
      "/legion/workspaces/acme/big/acme-12"
    );
  });
  const dir = path.join(scratch, "truncated-tree");
  const from = path.join(dir, "old.jsonl");
  mkdirSync(dir, { recursive: true });
  writeFileSync(
    from,
    `${JSON.stringify({ ...legion, source: { ...legion.source, pathHash: pathHash(moved) } })}\n`
  );
  const fresh = path.join(dir, "reprofiled.jsonl");
  const lines: string[] = [];
  const code = await main(
    [
      "build-cases",
      "--config",
      big,
      "--old",
      old,
      "--copies",
      copiesDir,
      "--count",
      countDir,
      "--reprofile",
      from,
      "--out",
      fresh,
    ],
    io(lines)
  );
  expect(lines.join("\n")).not.toContain("cuts the tree");
  expect(code).toBe(EXIT.ok);
  const [c] = loadCases(fresh, old);
  expect(c?.profile.skills).toEqual(
    [
      ...legion.profile.skills,
      { repo: "acme/big", commit: SHA, path: ".claude/skills/omega", name: "omega" },
    ].sort((a, b) => (a.name < b.name ? -1 : 1))
  );
  expect(c?.profile.contextFiles).toEqual([
    { repo: "acme/big", commit: SHA, path: "AGENTS.md" },
    ...legion.profile.contextFiles.filter((file) => file.repo !== "acme/widgets"),
  ]);
  // No whole tree of acme/big is asked for: its root, .claude/, .claude/skills/ and the skill's
  // directory, one level each, and not data/ or the skill's references/.
  expect(
    ghCalls
      .flatMap((args) => args.filter((arg) => arg.startsWith("repos/acme/big/git/trees/")))
      .sort()
  ).toEqual(
    ["", ".claude", ".claude/skills", ".claude/skills/omega"]
      .map((dir) => `repos/acme/big/git/trees/${treeId("acme/big", dir)}`)
      .sort()
  );
}, 120_000);

// Catches: a working directory mapped to a repository by a parent directory's name alone, and the
// dev box's own checkout mapped to the repository named as its parent.
test("a dev-box session in the checkout whose .claude/ devboxClaudeConfigDir names gets the dev box's repository, and one under a parent named as a listed repository whose tree lacks the path below it gets no session repository, saying why", async () => {
  const sessions = {
    [`${DEV_SESSIONS}/dots-home.jsonl`]: "/home/dev/.dotfiles",
    [`${DEV_SESSIONS}/proj-missing.jsonl`]: "/home/dev/src/proj/missing/dir",
  };
  const store = changedStore("checkouts", (files) => {
    for (const [file, cwd] of Object.entries(sessions)) {
      (files.store_a ?? {})[file] = inDirectory(sessionText("good"), cwd);
    }
  });
  writeFileSync(
    store,
    JSON.stringify({
      ...JSON.parse(readFileSync(store, "utf8")),
      devboxClaudeConfigDir: "/home/dev/.dotfiles/.claude",
    })
  );
  const copies = copiesWith("copies-checkouts", {
    "manifests.tsv": HOME_MANIFESTS,
  });
  const file = sessionsFile(
    "checkouts.jsonl",
    [],
    [],
    Object.keys(sessions).map((each) => ({ store: "store_a", hash: fileHash(each), path: each }))
  );
  const out = path.join(scratch, "checkouts", "cases.jsonl");
  const args = buildArgs(file, out, "1", "--copies", copies);
  args[args.indexOf("--config") + 1] = store;
  const lines: string[] = [];
  expect(await main(args, io(lines))).toBe(EXIT.ok);
  const cases = loadCases(out, old);
  const evidenceOf = (file: string) =>
    Object.fromEntries(
      cases
        .find((c) => c.source.pathHash === pathHash(file))
        ?.evidence?.map((each) => [each.item, each.source]) ?? []
    );
  expect(evidenceOf(`${DEV_SESSIONS}/dots-home.jsonl`)["session repository acme/home"]).toBe(
    `${CHECKOUT_RULE}. The working directory /home/dev/.dotfiles is /home/dev/.dotfiles, the checkout of the dev box's repository acme/home, whose .claude/ the configuration's devboxClaudeConfigDir names`
  );
  const missing = evidenceOf(`${DEV_SESSIONS}/proj-missing.jsonl`);
  expect(missing["session repository acme/proj"]).toBeUndefined();
  expect(missing["no session repository"]).toBe(
    `${CHECKOUT_RULE}. The working directory /home/dev/src/proj/missing/dir is under /home/dev/src/proj, named as acme/proj, whose tree at ${SHA} holds no missing/dir, so that name alone does not make the directory a checkout of it; and it is not directly inside /home/dev/src/proj, so it is not taken as a workspace of acme/proj`
  );
  expect(
    cases.find((c) => c.source.pathHash === pathHash(`${DEV_SESSIONS}/proj-missing.jsonl`))?.profile
      .sessionRepository
  ).toBeUndefined();
}, 120_000);

/** A directory named as acme/proj, and three sessions in directories below it that acme/proj's
 *  tree does not hold, each with its working directory and the file it read there by its absolute
 *  path: directly inside it, one read of a file the tree's root holds (CLAUDE.md) and one of a
 *  file the tree holds nowhere (notes.md); two levels below it, a read of CLAUDE.md. */
const PROJ_DIR = "/home/dev/src/proj";
const BELOW_PROJ: Record<string, { readonly cwd: string; readonly read: string }> = {
  [`${DEV_SESSIONS}/lane-held.jsonl`]: {
    cwd: `${PROJ_DIR}/lane-3`,
    read: `${PROJ_DIR}/lane-3/CLAUDE.md`,
  },
  [`${DEV_SESSIONS}/lane-unheld.jsonl`]: {
    cwd: `${PROJ_DIR}/lane-4`,
    read: `${PROJ_DIR}/lane-4/notes.md`,
  },
  [`${DEV_SESSIONS}/lane-deep.jsonl`]: {
    cwd: `${PROJ_DIR}/lanes/lane-5`,
    read: `${PROJ_DIR}/lanes/lane-5/CLAUDE.md`,
  },
};

/** Of the case build-cases built from the session `file` of BELOW_PROJ (one run for all three,
 *  made once), its profile and its evidence by item. */
async function belowProj(file: string) {
  const out = path.join(scratch, "below-proj", "cases.jsonl");
  if (!existsSync(out)) {
    const store = changedStore("below-proj", (files) => {
      for (const [session, { cwd, read }] of Object.entries(BELOW_PROJ)) {
        (files.store_a ?? {})[session] = readingIn(cwd, read);
      }
    });
    const list = sessionsFile(
      "below-proj.jsonl",
      [],
      [],
      Object.keys(BELOW_PROJ).map((each) => ({
        store: "store_a",
        hash: fileHash(each),
        path: each,
      }))
    );
    const args = buildArgs(list, out, "1");
    args[args.indexOf("--config") + 1] = store;
    expect(await main(args, io([]))).toBe(EXIT.ok);
  }
  const c = loadCases(out, old).find((each) => each.source.pathHash === pathHash(file));
  return {
    profile: c?.profile,
    evidence: Object.fromEntries(c?.evidence?.map((each) => [each.item, each.source]) ?? []),
  };
}

// Catches (a dev-box profile-build fault found in a load task's rebuilt context): a session in a
// workspace of a repository, a jj or git worktree `<dir>/<repo>/<workspace>` whose root is the
// working directory, that the tree check refused as a checkout because the repository's tree does
// not hold `<workspace>`, so its profile held none of the repository's project skills.
test("a dev-box session directly inside a directory named as acme/proj whose tree does not hold it, that read a file of the tree there, is in a workspace of acme/proj at its root: it gets acme/proj's root project skills and CLAUDE.md, and the evidence names the read and the file", async () => {
  const { profile, evidence } = await belowProj(`${DEV_SESSIONS}/lane-held.jsonl`);
  expect(profile?.sessionRepository).toEqual({ repo: "acme/proj", commit: SHA });
  expect(profile?.workingDirectory).toBeUndefined();
  // As a session in /home/dev/src/proj gets them.
  expect(profile?.skills).toEqual([
    { repo: "acme/skills", commit: SHA, path: "skills/older", name: "older", namespace: "agent" },
    { repo: "acme/skills", commit: SHA, path: "skills/alpha", name: "alpha" },
    { repo: "acme/skills", commit: SHA, path: "skills/beta", name: "beta" },
    {
      repo: "acme/proj",
      commit: SHA,
      path: ".claude/skills/beta",
      name: "beta",
      namespace: "claude",
    },
    { repo: "acme/proj", commit: SHA, path: ".claude/skills/kappa", name: "kappa" },
    { repo: "acme/proj", commit: SHA, path: ".omp/skills/lambda", name: "lambda" },
    { repo: "acme/proj", commit: SHA, path: ".omp/skills/older", name: "older" },
  ]);
  expect(profile?.contextFiles).toEqual([
    { repo: "acme/skills", commit: SHA, path: ".claude/CLAUDE.md", user: true },
    { repo: "acme/proj", commit: SHA, path: "CLAUDE.md" },
  ]);
  expect(evidence["session repository acme/proj"]).toBe(
    `${CHECKOUT_RULE}. The working directory ${PROJ_DIR}/lane-3 is under ${PROJ_DIR}, named as acme/proj, whose tree at ${SHA} holds no lane-3; it is a workspace of acme/proj named lane-3, a checkout of it at its root: message 2 reads ${PROJ_DIR}/lane-3/CLAUDE.md, and the tree holds CLAUDE.md`
  );
}, 60_000);

test("a dev-box session directly inside a directory named as acme/proj whose tree does not hold it, that read no file of the tree there, gets no session repository, and the evidence says so", async () => {
  const { profile, evidence } = await belowProj(`${DEV_SESSIONS}/lane-unheld.jsonl`);
  expect(profile?.sessionRepository).toBeUndefined();
  expect(profile?.skills.some((skill) => skill.repo === "acme/proj")).toBe(false);
  expect(evidence["no session repository"]).toBe(
    `${CHECKOUT_RULE}. The working directory ${PROJ_DIR}/lane-4 is under ${PROJ_DIR}, named as acme/proj, whose tree at ${SHA} holds no lane-4, so that name alone does not make the directory a checkout of it; and no file the session read by an absolute path under it is a file of that tree at its path there (it read 1 such file), so nothing shows it is a workspace of acme/proj`
  );
}, 60_000);

test("a dev-box session two levels below a directory named as acme/proj whose tree does not hold its path gets no session repository though it read a file of the tree there, and the evidence says why", async () => {
  const { profile, evidence } = await belowProj(`${DEV_SESSIONS}/lane-deep.jsonl`);
  expect(profile?.sessionRepository).toBeUndefined();
  expect(profile?.skills.some((skill) => skill.repo === "acme/proj")).toBe(false);
  expect(evidence["no session repository"]).toBe(
    `${CHECKOUT_RULE}. The working directory ${PROJ_DIR}/lanes/lane-5 is under ${PROJ_DIR}, named as acme/proj, whose tree at ${SHA} holds no lanes/lane-5, so that name alone does not make the directory a checkout of it; and it is not directly inside ${PROJ_DIR}, so it is not taken as a workspace of acme/proj`
  );
}, 60_000);

// Catches: a project skill of .agent/skills, .agents/skills or .github/skills, or one whose
// directory is a symbolic link in .claude/skills, missing from the rebuilt profile, though Oh My
// Pi 18.18.6 lists each.
test("build-cases --reprofile of a session in a workspace of acme/agents lists its .agent/skills, .agents/skills and .github/skills, follows a skill directory linked inside the repository, and names the link out of it and the .github skill with no description as left out", async () => {
  const legion = await profileCase("legion");
  const moved = `${LEGION_SESSIONS}/agents-legion.jsonl`;
  const store = changedStore("agents-dirs", (files) => {
    (files.store_b ?? {})[moved] = inDirectory(
      sessionText("legion"),
      "/legion/workspaces/acme/agents/acme-13"
    );
  });
  const dir = path.join(scratch, "agents-dirs");
  const from = path.join(dir, "old.jsonl");
  mkdirSync(dir, { recursive: true });
  writeFileSync(
    from,
    `${JSON.stringify({ ...legion, source: { ...legion.source, pathHash: pathHash(moved) } })}\n`
  );
  const fresh = path.join(dir, "reprofiled.jsonl");
  const lines: string[] = [];
  const args = ["--old", old, "--copies", copiesDir, "--count", countDir];
  const code = await main(
    ["build-cases", "--config", store, ...args, "--reprofile", from, "--out", fresh],
    io(lines)
  );
  expect({ code, said: code === EXIT.ok ? "" : lines.join("\n") }).toEqual({
    code: EXIT.ok,
    said: "",
  });
  const [c] = loadCases(fresh, old);
  const project = (c?.profile.skills ?? []).filter((skill) => skill.repo === "acme/agents");
  expect(project.map((skill) => `${skill.name} ${skill.path}`).sort()).toEqual([
    "rho .agents/skills/rho",
    "sigma .github/skills/sigma",
    "tau .claude/skills/tau",
    "upsilon .agent/skills/upsilon",
  ]);
  const evidence = Object.fromEntries(c?.evidence?.map((each) => [each.item, each.source]) ?? []);
  expect(evidence["linked: acme/agents .claude/skills/tau -> ../../skills/tau"]).toContain(
    "which Oh My Pi follows"
  );
  expect(evidence["left out: acme/agents .claude/skills/away -> ../../../outside/away"]).toContain(
    "outside the repository"
  );
  expect(evidence["left out: skill plain from acme/agents .github/skills/plain"]).toContain(
    "its SKILL.md has no description"
  );
  expect(evidence["skill rho (acme/agents .agents/skills/rho)"]).toContain(
    "discovery/agents.ts, loadSkills, lines 158-172"
  );
  expect(evidence["skill sigma (acme/agents .github/skills/sigma)"]).toContain(
    "discovery/github.ts, loadSkills, lines 286-296"
  );
}, 120_000);

// Catches: a pool that the dev box's skills-sources.json names at a subagent's start and not at
// its root's last prompt build left out though the subagent listed its skills, or installed
// though the subagent's host had not cloned it yet.
test("a subagent whose dev box's skills-sources.json names a pool at its start and not at its root's last prompt build gets that pool only where its recorded listing names one of the pool's skills, its names marked fromRecorded, and otherwise has it left out, saying why", async () => {
  // Two subagents of PINNED_ROOT that read alpha alone: one's recorded listing names pack-two too.
  const listings: Record<string, string> = {
    "pool-listed": "- pack-two: Use for the pack-two steps.\n",
    "pool-unlisted": "",
  };
  const pathOf = (name: string) => `${PINNED_ROOT}/${name}.jsonl`;
  const store = changedStore("pool-start", (files) => {
    for (const [name, extra] of Object.entries(listings)) {
      const lines = f1Lines();
      setText(lines, 5, () => `Step 3 (case ${name}).`);
      SESSIONS.subagent?.change?.(lines);
      const init = lines.find((line) => line.type === "session_init");
      if (init !== undefined) {
        init.systemPrompt = [
          (SUBAGENT_PROMPT[0] ?? "").replace("</skills>", `${extra}</skills>`),
          ...SUBAGENT_PROMPT.slice(1),
        ];
      }
      (files.store_a ?? {})[pathOf(name)] = lines
        .map((line) => `${JSON.stringify(line)}\n`)
        .join("");
    }
  });
  // acme/dots is the dev box's repository: it holds an installer script and Claude Code's
  // settings. At the root's last build (DOTS_OLD) its skills-sources.json does not exist; at the
  // subagent's start (SHA) it names the pool vendor/acme-pack, a clone of acme/pack.
  const copies = copiesWith("copies-pool-start", {
    "manifests.tsv": DOTS_MANIFESTS,
  });
  const file = sessionsFile(
    "pool-start.jsonl",
    [],
    [],
    Object.keys(listings).map((name) => ({
      store: "store_a",
      hash: fileHash(pathOf(name)),
      path: pathOf(name),
    }))
  );
  const out = path.join(scratch, "pool-start", "cases.jsonl");
  const args = buildArgs(file, out, "1", "--copies", copies);
  args[args.indexOf("--config") + 1] = store;
  const lines: string[] = [];
  const code = await main(args, io(lines));
  expect({ code, said: code === EXIT.ok ? "" : lines.join("\n") }).toEqual({
    code: EXIT.ok,
    said: "",
  });
  const cases = loadCases(out, old);
  const caseOf = (name: string) => cases.find((c) => c.source.pathHash === pathHash(pathOf(name)));
  const evidenceOf = (name: string) =>
    Object.fromEntries(caseOf(name)?.evidence?.map((each) => [each.item, each.source]) ?? []);
  const poolSkills = (name: string) =>
    (caseOf(name)?.profile.skills ?? [])
      .filter((skill) => skill.repo === "acme/pack")
      .map((skill) => `${skill.name} ${skill.commit}`)
      .sort();
  const names = `acme/dots:skills-sources.json names vendor/acme-pack at ${SHA} (the subagent's start ${F1_START}) and not at ${DOTS_OLD} (the root's last prompt build)`;
  // The recorded listing names pack-two, so the host had cloned the pool: it is installed, and
  // each of its names is marked as taken from that listing (POOL_LISTING_RULE, as for a pool of
  // the root's build).
  expect(poolSkills("pool-listed")).toEqual([`pack-one ${PACK_PIN}`, `pack-two ${PACK_PIN}`]);
  const listed = evidenceOf("pool-listed");
  expect(listed["unit pool:vendor/acme-pack (acme/dots:skills-sources.json)"]).toStartWith(
    `pool at the subagent's start: ${names}; the session's recorded listing names 1 of the 2 skills it holds at the session's start (named: pack-two; unnamed: pack-one), so it is installed; ${POOL_INFERENCE}, and the listing measure counts its unnamed skills as added names: Oh My Pi 18.18.6 lists the skills of a pool an extension gives through resources_discover`
  );
  const listedMeasure = ListingMeasure.parse(JSON.parse(listed[LISTING_MEASURE_ITEM] ?? "{}"));
  expect(listedMeasure.fromRecorded).toEqual(["pack-one", "pack-two"]);
  // The recorded listing names none of the pool's skills: the host had not cloned it yet.
  expect(poolSkills("pool-unlisted")).toEqual([]);
  const unlisted = evidenceOf("pool-unlisted");
  expect(unlisted["unit pool:vendor/acme-pack (acme/dots:skills-sources.json)"]).toBeUndefined();
  expect(
    unlisted["left out: the pool pool:vendor/acme-pack (acme/dots:skills-sources.json)"]
  ).toStartWith(
    `pool at the subagent's start: ${names}; the session's recorded listing names none of the 2 skills it holds at the session's start (pack-one, pack-two), so it is not installed; ${POOL_INFERENCE}: Oh My Pi 18.18.6 lists the skills of a pool an extension gives through resources_discover`
  );
  const unlistedMeasure = ListingMeasure.parse(JSON.parse(unlisted[LISTING_MEASURE_ITEM] ?? "{}"));
  expect(unlistedMeasure.fromRecorded).toBeUndefined();
}, 120_000);

// The architect's ruling of 2026-11-25: dev-box sessions that ran one Oh My Pi build list the dev
// box's resources_discover pools in some sessions and not in others, and no file records why. So
// a session that records its listing (its own recorded prompt's, or the subagent's its listing
// measure uses) gets each pool from that listing, a pool its root's build has too; one that
// records none gets them by the Oh My Pi build its dev box pins at its start. Its narrowing of
// the same day: a name that an installed unit outside the pools also ships is no evidence for a
// pool, since the session lists it whether or not the pool is there; only a name no such unit
// ships, or a [Skill file: …] read of the pool, installs it.

/** The root of the listed-pool subagents, which the store does not hold: each repository of their
 *  profiles is read at their start, when acme/dots's skills-sources.json names the pool
 *  vendor/acme-pack (pack-one and pack-two at PACK_PIN), as at its root's build, and its plugin
 *  list installs acme/powers, whose own pack-two (of other text) the pool's shares a name with. */
const UNHELD_ROOT = `${DEV_SESSIONS}/2026-11-19T00-00-00-000Z_0a1b2c3d-0000-4000-8000-0000000000b2`;
/** Each listed-pool subagent, and the pool skills its recorded listing names. pool-read also
 *  reads pack-one of the pool as a `[Skill file: …]`. */
const POOL_LISTINGS: Record<string, readonly string[]> = {
  "pool-all": ["pack-one", "pack-two"],
  "pool-part": ["pack-one"],
  "pool-shared": ["pack-two"],
  "pool-read": ["pack-two"],
  "pool-none": [],
};
/** The pool's unit, as the evidence names it. */
const POOL_KEY = "pool:vendor/acme-pack (acme/dots:skills-sources.json)";
/** The words of the pool's evidence that say the decision is an inference. */
const POOL_INFERENCE =
  "this is an inference from the listing, since no file records why some sessions of one Oh My Pi build list such a pool and others do not";
/** The words of the pool's evidence that say a named skill is no evidence for the pool, followed
 *  by the names of those skills. */
const SHARED_NOTE =
  "named skills that an installed unit outside the pools also ships give no evidence for this pool";

/** The case of the listed-pool subagent `name` (POOL_LISTINGS), all built once with acme/dots
 *  as the dev box's repository: its pool skills (name and commit), its evidence by item and its
 *  listing measure. */
async function listedPool(name: string) {
  const pathOf = (each: string) => `${UNHELD_ROOT}/${each}.jsonl`;
  const out = path.join(scratch, "pool-listed", "cases.jsonl");
  if (!existsSync(out)) {
    const store = changedStore("pool-listed", (files) => {
      for (const [each, names] of Object.entries(POOL_LISTINGS)) {
        const lines = f1Lines();
        setText(lines, 5, () => `Step 3 (case ${each}).`);
        SESSIONS.subagent?.change?.(lines);
        if (each === "pool-read") {
          readFirst("9ac0000", "skill://pack-one", PACK_ONE, "# pack-one\n")(lines);
        }
        const init = lines.find((line) => line.type === "session_init");
        if (init !== undefined) {
          const listed = names.map((skill) => `- ${skill}: Use for the ${skill} steps.\n`).join("");
          init.systemPrompt = [
            (SUBAGENT_PROMPT[0] ?? "").replace("</skills>", `${listed}</skills>`),
            ...SUBAGENT_PROMPT.slice(1),
          ];
        }
        (files.store_a ?? {})[pathOf(each)] = lines
          .map((line) => `${JSON.stringify(line)}\n`)
          .join("");
      }
    });
    const copies = copiesWith("copies-pool-listed", { "manifests.tsv": DOTS_MANIFESTS });
    const file = sessionsFile(
      "pool-listed.jsonl",
      [],
      [],
      Object.keys(POOL_LISTINGS).map((each) => ({
        store: "store_a",
        hash: fileHash(pathOf(each)),
        path: pathOf(each),
      }))
    );
    const args = buildArgs(file, out, "1", "--copies", copies);
    args[args.indexOf("--config") + 1] = store;
    const lines: string[] = [];
    const code = await main(args, io(lines));
    expect({ code, said: code === EXIT.ok ? "" : lines.join("\n") }).toEqual({
      code: EXIT.ok,
      said: "",
    });
  }
  const c = loadCases(out, old).find((each) => each.source.pathHash === pathHash(pathOf(name)));
  expect(c?.context).toBe("subagent:task");
  const evidence = Object.fromEntries(c?.evidence?.map((each) => [each.item, each.source]) ?? []);
  return {
    pool: (c?.profile.skills ?? [])
      .filter((skill) => skill.repo === "acme/pack")
      .map((skill) => `${skill.name} ${skill.commit}`)
      .sort(),
    evidence,
    measure: ListingMeasure.parse(JSON.parse(evidence[LISTING_MEASURE_ITEM] ?? "{}")),
  };
}

// Catches: a pool the dev box's installers install at the root's build installed for a subagent
// whatever its recorded listing names, with no mark that the listing decided its names, or one of
// its named skills that an installed package also ships counted as evidence for the pool.
test("a dev-box subagent whose recorded listing names every skill of a pool its root's build has gets the pool, its names marked fromRecorded, and the evidence counts each named and says which an installed package also ships", async () => {
  const { pool, evidence, measure } = await listedPool("pool-all");
  expect(pool).toEqual([`pack-one ${PACK_PIN}`, `pack-two ${PACK_PIN}`]);
  expect(evidence[`unit ${POOL_KEY}`]).toContain(
    `the session's recorded listing names 2 of the 2 skills it holds at the session's start (named: pack-one, pack-two; unnamed: none); ${SHARED_NOTE} (pack-two), so it is installed on pack-one; ${POOL_INFERENCE}: `
  );
  expect(measure.kind).toBe("session listing");
  expect(measure.fromRecorded).toEqual(["pack-one", "pack-two"]);
  expect(measure.added.filter((name) => name.startsWith("pack-"))).toEqual([]);
}, 120_000);

// Catches: a pool whose skills the recorded listing names in part installed with no record of how
// many it names, or left out, or its unnamed skills kept out of the measure's added names.
test("a dev-box subagent whose recorded listing names some skills of a pool gets the whole pool, the evidence counts its named and unnamed skills, and the unnamed are the measure's added names", async () => {
  const { pool, evidence, measure } = await listedPool("pool-part");
  expect(pool).toEqual([`pack-one ${PACK_PIN}`, `pack-two ${PACK_PIN}`]);
  expect(evidence[`unit ${POOL_KEY}`]).toContain(
    `the session's recorded listing names 1 of the 2 skills it holds at the session's start (named: pack-one; unnamed: pack-two), so it is installed; ${POOL_INFERENCE}, and the listing measure counts its unnamed skills as added names: `
  );
  expect(measure.added).toContain("pack-two");
  expect(measure.added).not.toContain("pack-one");
  expect(measure.fromRecorded).toEqual(["pack-one", "pack-two"]);
}, 120_000);

// Catches: a pool installed because the recorded listing names a skill of it that an installed
// package ships too, which the session lists whether or not the pool is there.
test("a dev-box subagent whose recorded listing names one skill of a pool, which an installed package also ships, and no other, gets none of the pool, and the evidence names the shared skill", async () => {
  const { pool, evidence, measure } = await listedPool("pool-shared");
  expect(pool).toEqual([]);
  expect(evidence[`unit ${POOL_KEY}`]).toBeUndefined();
  expect(evidence[`left out: the pool ${POOL_KEY}`]).toContain(
    `the session's recorded listing names 1 of the 2 skills it holds at the session's start (pack-one, pack-two); ${SHARED_NOTE} (pack-two), so it is not installed; ${POOL_INFERENCE}: `
  );
  // The pool's other skill is not in the profile; pack-two is, as the package's copy.
  expect(measure.added).not.toContain("pack-one");
  expect(measure.added).not.toContain("pack-two");
  expect(measure.missing).not.toContain("pack-two");
  expect(measure.fromRecorded).toBeUndefined();
}, 120_000);

// Catches: a pool whose skill the session read as a `[Skill file: …]` left out because the
// listing names only a skill an installed package ships too, or marked as taken from the listing.
test("a dev-box subagent that reads a skill of a pool as a [Skill file] keeps the pool though its recorded listing names only a skill an installed package also ships, citing the read and not marking its names fromRecorded", async () => {
  const { pool, evidence, measure } = await listedPool("pool-read");
  expect(pool).toEqual([`pack-one ${PACK_PIN}`, `pack-two ${PACK_PIN}`]);
  const unit = evidence[`unit ${POOL_KEY}`] ?? "";
  expect(unit).toContain(
    `the session's recorded listing names 1 of the 2 skills it holds at the session's start (named: pack-two; unnamed: pack-one); ${SHARED_NOTE} (pack-two); `
  );
  expect(unit).toMatch(
    new RegExp(
      `; message \\d+ reads \\[Skill file: ${PACK_ONE.replaceAll(".", "\\.")}\\] of it, so Oh My Pi loaded it, which is direct proof and no inference from the listing, so it is installed and none of its names is marked fromRecorded: `
    )
  );
  expect(evidence[`left out: the pool ${POOL_KEY}`]).toBeUndefined();
  expect(measure.fromRecorded).toBeUndefined();
  // The listing names pack-two alone: the pool's pack-one is a name the profile adds.
  expect(measure.added).toContain("pack-one");
}, 120_000);

// Catches: a pool the dev box's installers install at the root's build installed for a subagent
// whose recorded listing names none of its skills.
test("a dev-box subagent whose recorded listing names no skill of a pool its root's build has gets none of it, and the evidence says why", async () => {
  const { pool, evidence, measure } = await listedPool("pool-none");
  expect(pool).toEqual([]);
  expect(evidence[`unit ${POOL_KEY}`]).toBeUndefined();
  expect(evidence[`left out: the pool ${POOL_KEY}`]).toContain(
    `the session's recorded listing names none of the 2 skills it holds at the session's start (pack-one, pack-two), so it is not installed; ${POOL_INFERENCE}: `
  );
  // The pool's names are not listed; pack-two still is, as acme/powers's copy, which the pool's
  // copy outranks only where the pool is installed (rows above: listed as acme-powers/pack-two).
  expect(measure.added).not.toContain("pack-one");
  expect(measure.added).toContain("pack-two");
  expect(measure.added).not.toContain("acme-powers/pack-two");
  expect(measure.fromRecorded).toBeUndefined();
}, 120_000);

// The architect's ruling of 2026-11-25 (17:06Z): Oh My Pi lists a copy as <namespace>/<name> only
// where it admitted another copy of the name first, and it admits a pool's before every unit's
// outside the pools. So a recorded <namespace>/<name> whose bare name a pool the listing leaves out
// holds is direct proof of that pool, where no installed unit outside the pools that Oh My Pi
// admits before the unit of that namespace ships another copy of the name.

/** The root of the twin-pool subagents, which the store does not hold. Each runs on the dev box
 *  of acme/twin-dots in a checkout of acme/twin, so its profile can install the pool
 *  vendor/twin-pack (pack-one, pack-two and twin-only), the package twin-powers (pack-one and
 *  pack-two of other text, and power-solo) and the Claude Code plugin twin-tools that acme/twin's
 *  settings turn on (pack-one and pack-two of the pool's text, and power-solo of other text). */
const TWIN_ROOT = `${DEV_SESSIONS}/2026-11-19T00-00-00-000Z_0a1b2c3d-0000-4000-8000-0000000000b3`;
/** Each twin-pool subagent, and the names its recorded listing adds to SUBAGENT_PROMPT's. */
const TWIN_LISTINGS: Record<string, readonly string[]> = {
  "twin-proof": ["twin-powers/pack-one", "twin-powers/pack-two"],
  "twin-later": ["twin-tools/pack-one"],
  "twin-unheld": ["twin-powers/power-solo"],
  "twin-named": ["twin-only", "twin-powers/pack-one"],
};
/** The units of the twin-pool subagents' profiles, as the evidence names them. */
const TWIN_POOL = "pool:vendor/twin-pack (acme/twin-dots:skills-sources.json)";
const TWIN_PACKAGE = "package:twin-powers (acme/twin-dots:omp/plugins/package.json)";
const TWIN_PLUGIN = "plugin:twin-tools@acme-twins (acme/twins:.claude-plugin/marketplace.json)";

/** The case of the twin-pool subagent `name` (TWIN_LISTINGS), all built once: the names its
 *  profile lists from acme/twin-pack, acme/twin-powers and acme/twins, each with its repository,
 *  its evidence by item and its listing measure. */
async function twinPool(name: string) {
  const pathOf = (each: string) => `${TWIN_ROOT}/${each}.jsonl`;
  const out = path.join(scratch, "pool-twin", "cases.jsonl");
  if (!existsSync(out)) {
    const store = changedStore("pool-twin", (files) => {
      for (const [each, names] of Object.entries(TWIN_LISTINGS)) {
        const lines = f1Lines();
        setText(lines, 5, () => `Step 3 (case ${each}).`);
        SESSIONS.subagent?.change?.(lines);
        workingDirectory("/home/dev/src/twin")(lines);
        const init = lines.find((line) => line.type === "session_init");
        if (init !== undefined) {
          const listed = names.map((skill) => `- ${skill}: Use for the ${skill} steps.\n`).join("");
          init.systemPrompt = [
            (SUBAGENT_PROMPT[0] ?? "").replace("</skills>", `${listed}</skills>`),
            ...SUBAGENT_PROMPT.slice(1),
          ];
        }
        (files.store_a ?? {})[pathOf(each)] = lines
          .map((line) => `${JSON.stringify(line)}\n`)
          .join("");
      }
    });
    // As copies.ts gives them: acme/twin-dots is the dev box's repository, acme/twin's settings
    // turn twin-tools on, and the marketplace acme-twins (acme/twins) ships its skills.
    const plugin = (
      repo: string,
      manifest: string,
      kind: string,
      action: string,
      skill: string
    ) => [
      repo,
      manifest,
      kind,
      action,
      "plugin:twin-tools@acme-twins",
      "claude-code",
      skill,
      "acme/twins",
      `plugins/twin-tools/skills/${skill}`,
      "",
    ];
    const shipped = ["pack-one", "pack-two", "power-solo"];
    const copies = copiesWith("copies-pool-twin", {
      "manifests.tsv": [
        ["acme/twin-dots", "installers/omp.sh", "installer-script", "tree", 1, "clones", ""],
        ["acme/twin-dots", ".claude/settings.json", "claude-settings", "tree", 0, "none", ""],
      ],
      "repositories.tsv": ["acme/twin", "acme/twins"].map((repo) => [
        repo,
        "main",
        SHA,
        "no",
        "no",
        "",
        3,
        "whole",
        0,
        "",
      ]),
      "installers.tsv": [
        ...shipped.map((skill) =>
          plugin("acme/twin", ".claude/settings.json", "claude-settings", "enables", skill)
        ),
        ...shipped.map((skill) =>
          plugin(
            "acme/twins",
            ".claude-plugin/marketplace.json",
            "claude-marketplace",
            "ships",
            skill
          )
        ),
      ],
      "skill-copies.tsv": shipped.map((skill) => [
        "acme/twins",
        `plugins/twin-tools/skills/${skill}`,
        skill,
        10,
        "no",
        "no",
        "",
        "",
        "",
      ]),
    });
    const file = sessionsFile(
      "pool-twin.jsonl",
      [],
      [],
      Object.keys(TWIN_LISTINGS).map((each) => ({
        store: "store_a",
        hash: fileHash(pathOf(each)),
        path: pathOf(each),
      }))
    );
    const args = buildArgs(file, out, "1", "--copies", copies);
    args[args.indexOf("--config") + 1] = store;
    const lines: string[] = [];
    const code = await main(args, io(lines));
    expect({ code, said: code === EXIT.ok ? "" : lines.join("\n") }).toEqual({
      code: EXIT.ok,
      said: "",
    });
  }
  const c = loadCases(out, old).find((each) => each.source.pathHash === pathHash(pathOf(name)));
  expect(c?.context).toBe("subagent:task");
  const evidence = Object.fromEntries(c?.evidence?.map((each) => [each.item, each.source]) ?? []);
  return {
    listed: (c?.profile.skills ?? [])
      .filter((skill) => skill.repo.startsWith("acme/twin"))
      .map((skill) => `${listedName(skill)} ${skill.repo}`)
      .sort(),
    evidence,
    measure: ListingMeasure.parse(JSON.parse(evidence[LISTING_MEASURE_ITEM] ?? "{}")),
  };
}

// Catches: a pool left out though the recorded listing names its skills as the namespaced copies
// of a package, which Oh My Pi lists so only where a copy it admitted before the package's came
// first, and no unit outside the pools gives one.
test("a dev-box subagent whose recorded listing names a pool's skills only as an installed package's <namespace>/<name>, and no installed unit Oh My Pi admits before the package ships them, gets the pool as direct proof: its names bare, the package's namespaced, the plugin's of the same text dropped, none marked fromRecorded, and the measure misses neither namespaced name", async () => {
  const { listed, evidence, measure } = await twinPool("twin-proof");
  expect(listed).toEqual([
    "pack-one acme/twin-pack",
    "pack-two acme/twin-pack",
    "power-solo acme/twin-powers",
    "twin-only acme/twin-pack",
    "twin-powers/pack-one acme/twin-powers",
    "twin-powers/pack-two acme/twin-powers",
    "twin-tools/power-solo acme/twins",
  ]);
  const proofs = ["pack-one", "pack-two"]
    .map(
      (skill) =>
        `twin-powers/${skill}: ${TWIN_PACKAGE} ships ${skill} under that namespace, and no installed unit outside the pools that Oh My Pi admits before it ships another copy of it`
    )
    .join("; ");
  expect(evidence[`unit ${TWIN_POOL}`]).toContain(
    `; the session's recorded listing names none of the 3 skills it holds at the session's start (named: none; unnamed: pack-one, pack-two, twin-only); the session's recorded listing names twin-powers/pack-one, twin-powers/pack-two (${proofs}), and Oh My Pi lists a copy as <namespace>/<name> only where it admitted another copy of the name first (NAMESPACE_RULE), so the copy it admitted first was this pool's, which is direct proof and no inference from the listing, so it is installed and none of its names is marked fromRecorded: `
  );
  expect(evidence[`left out: the pool ${TWIN_POOL}`]).toBeUndefined();
  for (const skill of ["pack-one", "pack-two"]) {
    expect(
      evidence[`left out: skill ${skill} from acme/twins plugins/twin-tools/skills/${skill}`]
    ).toContain("its SKILL.md is the same, and Oh My Pi drops such a copy");
  }
  expect(measure.missing.filter((each) => each.includes("pack-"))).toEqual([]);
  expect(measure.fromRecorded).toBeUndefined();
}, 120_000);

// Catches: a pool installed on a recorded <namespace>/<name> that a unit outside the pools
// explains: Oh My Pi admits that unit's copy before the namespace's unit's.
test("a dev-box subagent whose recorded listing names a pool's skill only as a Claude Code plugin's <namespace>/<name>, which an installed package Oh My Pi admits before the plugin also ships, gets none of the pool, and the evidence says why that name proves nothing", async () => {
  const { listed, evidence, measure } = await twinPool("twin-later");
  expect(listed.filter((each) => each.endsWith(" acme/twin-pack"))).toEqual([]);
  expect(evidence[`unit ${TWIN_POOL}`]).toBeUndefined();
  expect(evidence[`left out: the pool ${TWIN_POOL}`]).toContain(
    `; the session's recorded listing names none of the 3 skills it holds at the session's start (pack-one, pack-two, twin-only); the session's recorded listing names twin-tools/pack-one, which proves nothing for this pool (twin-tools/pack-one: ${TWIN_PACKAGE}, which Oh My Pi admits before ${TWIN_PLUGIN}, ships another copy of pack-one), so it is not installed; ${POOL_INFERENCE}: `
  );
  // The package's pack-one keeps the bare name, and the plugin's is the listed twin-tools/pack-one.
  expect(listed).toContain("pack-one acme/twin-powers");
  expect(listed).toContain("twin-tools/pack-one acme/twins");
  expect(measure.missing).not.toContain("twin-tools/pack-one");
}, 120_000);

// Catches: a pool installed on a recorded <namespace>/<name> of a name it does not hold, here one
// that the package ships and no unit Oh My Pi admits before the package ships another copy of.
test("a dev-box subagent whose recorded listing names under a namespace only a skill the pool does not hold gets none of the pool, and its evidence names no namespaced name", async () => {
  const { listed, evidence } = await twinPool("twin-unheld");
  expect(listed.filter((each) => each.endsWith(" acme/twin-pack"))).toEqual([]);
  expect(evidence[`unit ${TWIN_POOL}`]).toBeUndefined();
  const leftOut = evidence[`left out: the pool ${TWIN_POOL}`];
  expect(leftOut).toContain(
    `; the session's recorded listing names none of the 3 skills it holds at the session's start (pack-one, pack-two, twin-only), so it is not installed; ${POOL_INFERENCE}: `
  );
  expect(leftOut).not.toContain("twin-powers/power-solo");
}, 120_000);

// Catches: a pool the listing installs on a named skill that no unit outside the pools ships,
// taken instead as installed on a recorded <namespace>/<name> of it, which loses its fromRecorded
// marks.
test("a dev-box subagent whose recorded listing names a skill of a pool that no unit outside the pools ships, and another of its skills under a namespace, gets the pool on the named skill as before, its names marked fromRecorded", async () => {
  const { listed, evidence, measure } = await twinPool("twin-named");
  expect(listed.filter((each) => each.endsWith(" acme/twin-pack"))).toEqual([
    "pack-one acme/twin-pack",
    "pack-two acme/twin-pack",
    "twin-only acme/twin-pack",
  ]);
  const unit = evidence[`unit ${TWIN_POOL}`] ?? "";
  expect(unit).toContain(
    `; the session's recorded listing names 1 of the 3 skills it holds at the session's start (named: twin-only; unnamed: pack-one, pack-two), so it is installed; ${POOL_INFERENCE}, and the listing measure counts its unnamed skills as added names: `
  );
  expect(unit).not.toContain("so the copy it admitted first was this pool's");
  expect(measure.fromRecorded).toEqual(["pack-one", "pack-two", "twin-only"]);
}, 120_000);

/** The case of the top-level dev-box session `name` (`pool-plain`, which reads alpha alone, or
 *  `pool-read`, which reads skill://pack-one of the pool first), built under a configuration whose
 *  ompPin names acme/dots's mise.toml, which pins 18.13.3-acme.1, and `first` as the first build
 *  that loads the pools on a fresh start. Neither starts a subagent, so neither has a listing. */
async function pinnedPool(name: "pool-plain" | "pool-read", first: string) {
  const pathOf = (each: string) => `${DEV_SESSIONS}/${each}.jsonl`;
  const out = path.join(scratch, `pool-pinned-${first}`, "cases.jsonl");
  if (!existsSync(out)) {
    const store = changedStore(`pool-pinned-${first}`, (files) => {
      (files.store_a ?? {})[pathOf("pool-plain")] = sessionText("good");
      (files.store_a ?? {})[pathOf("pool-read")] = sessionText("pinned-units");
    });
    writeFileSync(
      store,
      JSON.stringify({
        ...JSON.parse(readFileSync(store, "utf8")),
        ompPin: { file: "mise.toml", key: ["tools", "acme:omp"], firstPoolVersion: first },
      })
    );
    const copies = copiesWith("copies-pool-pinned", { "manifests.tsv": DOTS_MANIFESTS });
    const file = sessionsFile(
      `pool-pinned-${first}.jsonl`,
      [],
      [],
      ["pool-plain", "pool-read"].map((each) => ({
        store: "store_a",
        hash: fileHash(pathOf(each)),
        path: pathOf(each),
      }))
    );
    const args = buildArgs(file, out, "1", "--copies", copies);
    args[args.indexOf("--config") + 1] = store;
    const lines: string[] = [];
    const code = await main(args, io(lines));
    expect({ code, said: code === EXIT.ok ? "" : lines.join("\n") }).toEqual({
      code: EXIT.ok,
      said: "",
    });
  }
  const c = loadCases(out, old).find((each) => each.source.pathHash === pathHash(pathOf(name)));
  expect(c?.context).toBe("devbox");
  const evidence = Object.fromEntries(c?.evidence?.map((each) => [each.item, each.source]) ?? []);
  return {
    pool: (c?.profile.skills ?? [])
      .filter((skill) => skill.repo === "acme/pack")
      .map((skill) => skill.name)
      .sort(),
    evidence,
    measure: ListingMeasure.parse(JSON.parse(evidence[LISTING_MEASURE_ITEM] ?? "{}")),
  };
}

// Catches: the pools of a session that records no listing installed whatever Oh My Pi build its
// dev box pinned, though a build before the first that loads them on a fresh start lists none.
test("a top-level dev-box session with no listing gets the pools by its dev box's Oh My Pi pin at its start: none before the configuration's first pool-loading build but one a [Skill file] read shows loaded, each at or after it, and the evidence sets the session's runtime version beside the pin", async () => {
  const before = await pinnedPool("pool-plain", "18.16.0");
  expect(before.measure.kind).toBe("not measurable");
  expect(before.pool).toEqual([]);
  expect(before.evidence[`left out: the pool ${POOL_KEY}`]).toContain(
    "; the pinned Oh My Pi 18.13.3-acme.1 is before 18.16.0, so the session listed none of its skills on a fresh start; an inference from the pinned build: "
  );
  expect(before.evidence["Oh My Pi pin of acme/dots"]).toStartWith(
    `acme/dots:mise.toml at ${SHA} (the last commit of main at or before the session's start ${F1_START}) pins tools."acme:omp" at 18.13.3-acme.1, which is before 18.16.0, the first build the configuration's ompPin names as loading resources_discover pools on a fresh start; the session's file records Oh My Pi 18.18.6 (the runtime.version of its first model_change line), which differs from the pin's release 18.13.3: `
  );
  // A [Skill file: …] read of the pool's skill shows Oh My Pi loaded the pool.
  const read = await pinnedPool("pool-read", "18.16.0");
  expect(read.pool).toEqual(["pack-one", "pack-two"]);
  expect(read.evidence[`unit ${POOL_KEY}`]).toMatch(
    new RegExp(
      `; the pinned Oh My Pi 18\\.13\\.3-acme\\.1 is before 18\\.16\\.0, but message \\d+ reads \\[Skill file: ${PACK_ONE.replaceAll(".", "\\.")}\\] of it, so Oh My Pi loaded it: `
    )
  );
  const after = await pinnedPool("pool-plain", "18.13.3-acme.1");
  expect(after.pool).toEqual(["pack-one", "pack-two"]);
  expect(after.evidence[`unit ${POOL_KEY}`]).toContain(
    "; the pinned Oh My Pi 18.13.3-acme.1 is at or after 18.13.3-acme.1, which loads it on a fresh start: "
  );
}, 180_000);

// Catches: the skills of a Claude Code plugin that the session repository's own Claude Code
// settings turn on left out of a dev-box profile, or installed for a project whose settings do
// not turn it on.
test("a dev-box session in a checkout whose .claude/settings.json turns a Claude Code plugin on in enabledPlugins gets the plugin's skills at a Claude Code plugin's rank, a second copy of a name under the plugin's name only where its text differs, and a session in a checkout whose settings turn none on gets none", async () => {
  const sessions = {
    [`${DEV_SESSIONS}/plug-on.jsonl`]: "/home/dev/src/plug",
    [`${DEV_SESSIONS}/plug-off.jsonl`]: "/home/dev/src/unplug",
  };
  const store = changedStore("plugins-enabled", (files) => {
    for (const [file, cwd] of Object.entries(sessions)) {
      (files.store_a ?? {})[file] = inDirectory(sessionText("good"), cwd);
    }
  });
  // As copies.ts gives them: acme/plug's settings turn plug-tools on, and the marketplace
  // acme-plugs (acme/plugs) ships its skills.
  const plugin = "plugin:plug-tools@acme-plugs";
  const row = (repo: string, manifest: string, kind: string, action: string, skill: string) => [
    repo,
    manifest,
    kind,
    action,
    plugin,
    "claude-code",
    skill,
    "acme/plugs",
    `plugins/plug-tools/skills/${skill}`,
    "",
  ];
  const shipped = ["alpha", "beta", "plug-one"];
  const copies = copiesWith("copies-plugins", {
    "repositories.tsv": ["acme/plug", "acme/unplug", "acme/plugs"].map((repo) => [
      repo,
      "main",
      SHA,
      "no",
      "no",
      "",
      3,
      "whole",
      0,
      "",
    ]),
    "installers.tsv": [
      ...shipped.map((skill) =>
        row("acme/plug", ".claude/settings.json", "claude-settings", "enables", skill)
      ),
      ...shipped.map((skill) =>
        row("acme/plugs", ".claude-plugin/marketplace.json", "claude-marketplace", "ships", skill)
      ),
    ],
    "skill-copies.tsv": shipped.map((skill) => [
      "acme/plugs",
      `plugins/plug-tools/skills/${skill}`,
      skill,
      10,
      "no",
      "no",
      "",
      "",
      "",
    ]),
  });
  const file = sessionsFile(
    "plugins-enabled.jsonl",
    [],
    [],
    Object.keys(sessions).map((each) => ({ store: "store_a", hash: fileHash(each), path: each }))
  );
  const out = path.join(scratch, "plugins-enabled", "cases.jsonl");
  const args = buildArgs(file, out, "1", "--copies", copies);
  args[args.indexOf("--config") + 1] = store;
  const lines: string[] = [];
  const code = await main(args, io(lines));
  expect({ code, said: code === EXIT.ok ? "" : lines.join("\n") }).toEqual({
    code: EXIT.ok,
    said: "",
  });
  const cases = loadCases(out, old);
  const caseOf = (file: string) => cases.find((c) => c.source.pathHash === pathHash(file));
  const fromPlugin = (file: string) =>
    (caseOf(file)?.profile.skills ?? [])
      .filter((skill) => skill.repo === "acme/plugs")
      .map((skill) => `${skill.namespace === undefined ? "" : `${skill.namespace}/`}${skill.name}`)
      .sort();
  const on = caseOf(`${DEV_SESSIONS}/plug-on.jsonl`);
  expect(on?.profile.sessionRepository).toEqual({ repo: "acme/plug", commit: SHA });
  // plug-one is the plugin's alone; acme/skills' beta, a unit of a lower rank, keeps the bare
  // name, and the plugin's beta of other text is listed under the plugin's name; its alpha, of
  // the same text as the alpha the session read, is dropped.
  expect(fromPlugin(`${DEV_SESSIONS}/plug-on.jsonl`)).toEqual(["plug-one", "plug-tools/beta"]);
  const evidence = Object.fromEntries(on?.evidence?.map((each) => [each.item, each.source]) ?? []);
  const unit = evidence[`unit ${plugin} (acme/plugs:.claude-plugin/marketplace.json)`];
  expect(unit).toStartWith(
    "Oh My Pi 18.18.6 lists the skills of a Claude Code plugin that Claude Code's registry installs for the project"
  );
  expect(unit).toEndWith(
    `: acme/plug:.claude/settings.json at ${SHA} enables plug-tools@acme-plugs in enabledPlugins, and acme/plugs:.claude-plugin/marketplace.json ships its skills`
  );
  expect(evidence["skill plug-tools/beta (acme/plugs plugins/plug-tools/skills/beta)"]).toContain(
    "under the name beta Oh My Pi gives the session the copy of acme/skills skills/beta under that name"
  );
  expect(evidence["skill plug-tools/beta (acme/plugs plugins/plug-tools/skills/beta)"]).toContain(
    "and this copy's text differs"
  );
  expect(
    evidence["left out: skill alpha from acme/plugs plugins/plug-tools/skills/alpha"]
  ).toContain("its SKILL.md is the same, and Oh My Pi drops such a copy");
  // acme/unplug's settings turn no plugin on: the plugin's skills are no part of its profile.
  const off = caseOf(`${DEV_SESSIONS}/plug-off.jsonl`);
  expect(off?.profile.sessionRepository).toEqual({ repo: "acme/unplug", commit: SHA });
  expect(fromPlugin(`${DEV_SESSIONS}/plug-off.jsonl`)).toEqual([]);
  expect((off?.evidence ?? []).filter((each) => each.item.startsWith(`unit ${plugin}`))).toEqual(
    []
  );
}, 120_000);

// Catches: a Legion pod's worker image read at its repository's default branch at the session's
// start, where the pod ran an image built from an older commit.
test("build-cases --reprofile of a Legion pane reads the worker image's packages at the commit that the deployment's Pulumi config and GitHub's build provenance name: the image digest's, else that of a digest or a tag the comment above its image: line names, its evidence saying which rule decided; with none of them the case is left out, and the packages are not read at the default branch", async () => {
  const digest = (n: number) => String(n).repeat(64);
  const attested = digest(1);
  // A made-up Pulumi config of acme/infra: another project's entry first, whose comment names a
  // tag, and ACME's, whose daemonImage names the attested digest; neither is ACME's image.
  const pulumi = (comment: readonly string[], image: string) =>
    [
      "config:",
      "  legion:projects:",
      "    - key: OTHER",
      "      # The worker image built on acme-image-v2.0.0.",
      `      image: registry.example/acme/worker@sha256:${digest(9)}`,
      "    - key: ACME",
      "      instructions: acme.md",
      ...comment.map((line) => `      # ${line}`),
      `      image: ${image}`,
      `      daemonImage: registry.example/acme/worker@sha256:${attested}`,
      "",
    ].join("\n");
  /** The legion case built again, with gh's answers to the Pulumi config and the provenance. */
  const reprofiled = (name: string, text: string) =>
    reprofiledLegion(name, { pulumi: text, attested: { [attested]: IMAGE_SHA } });
  const config0 = `acme/infra:legion/Pulumi.prod.yaml at ${SHA} gives the project ACME the image`;
  const atImage = `, so the image's units of acme/skills are read at ${IMAGE_SHA}: a Legion pod runs the worker image`;
  const imaged = [`alpha ${IMAGE_SHA}`, `beta ${IMAGE_SHA}`, `omicron ${IMAGE_SHA}`];

  // The image's own digest has provenance: its package, and that package's directory, are read
  // at the commit it names, where the package stages omicron too.
  const own = await reprofiled(
    "own",
    pulumi([], `registry.example/acme/worker@sha256:${attested}`)
  );
  expect(own.skills).toEqual(imaged);
  expect(own.plugins).toContain(`pi-acme ${IMAGE_SHA} packages/pi-acme`);
  expect(own.worker).toStartWith(
    `${config0} registry.example/acme/worker@sha256:${attested}; gh api repos/acme/skills/attestations/sha256:${attested} names git+https://github.com/acme/skills@refs/heads/main at ${IMAGE_SHA}${atImage}`
  );

  // An image with no provenance, whose comment names the base it was built on.
  const plus = `registry.example/acme/worker-plus@sha256:${digest(2)}`;
  const base = await reprofiled(
    "base",
    pulumi(
      ["worker-plus, built on the base", `registry.example/acme/worker@sha256:${attested}.`],
      plus
    )
  );
  expect(base.skills).toEqual(imaged);
  expect(base.worker).toStartWith(
    `${config0} ${plus}, whose digest has no build provenance that names a commit (gh api repos/acme/skills/attestations/sha256:${digest(2)}); the comment right above its image: line names the image digest sha256:${attested}, whose build provenance (gh api repos/acme/skills/attestations/sha256:${attested}) names git+https://github.com/acme/skills@refs/heads/main at ${IMAGE_SHA}${atImage}`
  );

  // An image with no provenance, whose comment names a tag of the repository.
  const tagged = await reprofiled(
    "tag",
    pulumi(["worker-plus, built on acme-image-v2.0.0; before it v0.9.0."], plus)
  );
  expect(tagged.skills).toEqual(imaged);
  expect(tagged.worker).toContain(
    "; the comment right above its image: line names acme-image-v2.0.0, a tag of acme/skills (gh api repos/acme/skills/git/matching-refs/tags/acme-image-v2.0.0), whose last commit at or before "
  );
  expect(tagged.worker).toContain(` is ${IMAGE_SHA}${atImage}`);

  // Neither, and no label: the image's commit is not known, so the case is left out, and the
  // package is not read at the default branch at the session's start.
  const none = await reprofiledLegion("none", {
    pulumi: pulumi(["worker-plus of this week."], plus),
    attested: { [attested]: IMAGE_SHA },
    refused: true,
  });
  expect({ code: none.code, written: none.written }).toEqual({ code: EXIT.failed, written: false });
  expect(none.said).toContain(
    `pod profile not measurable: the worker image's commit of acme/skills is not known, and the repository's default branch is a guess, so the profile is not built: ${config0} ${plus}, whose digest has no build provenance that names a commit (gh api repos/acme/skills/attestations/sha256:${digest(2)}); the comment right above its image: line names no image digest whose build provenance names a commit, no commit and no tag of acme/skills: a Legion pod runs the worker image`
  );
}, 240_000);

// Catches: legion-4447's 4 role cases, whose worker image ghcr.io/…/legion-worker@sha256:25f7f0ab…
// has no build provenance and no comment above its line, read at the head of the repository's main
// at the session's start: a newer pi-envoy and pi-legion than the image held (dispatch-brainstorming
// missing, dispatch-writing-specs and leading-a-work-stream added).
describe("build-cases --reprofile of a Legion pane whose image has no build provenance", () => {
  const digest = "2".repeat(64);
  const pulumiOf = (image: string) =>
    [
      "config:",
      "  legion:projects:",
      "    - key: ACME",
      "      instructions: acme.md",
      `      image: ${image}`,
      "",
    ].join("\n");
  const config0 = `acme/infra:legion/Pulumi.prod.yaml at ${SHA} gives the project ACME the image`;
  const imaged = [`alpha ${IMAGE_SHA}`, `beta ${IMAGE_SHA}`, `omicron ${IMAGE_SHA}`];
  const without = `, whose digest has no build provenance that names a commit (gh api repos/acme/skills/attestations/sha256:${digest})`;
  const atImage = `, so the image's units of acme/skills are read at ${IMAGE_SHA}: a Legion pod runs the worker image`;

  test("the image's own revision label, a commit of the repository, decides the commit its units are read at, though the label's source names no repository the build knows", async () => {
    const image = `registry.example/acme/worker-plus@sha256:${digest}`;
    const built = await reprofiledLegion("label-revision", {
      pulumi: pulumiOf(image),
      attested: {},
      labels: {
        "org.opencontainers.image.revision": IMAGE_SHA,
        "org.opencontainers.image.source": "https://github.com/acme/skills-before-its-rename",
      },
    });
    expect(built.skills).toEqual(imaged);
    expect(built.plugins).toContain(`pi-acme ${IMAGE_SHA} packages/pi-acme`);
    expect(built.registryReads).toContain(image);
    expect(built.worker).toStartWith(
      `${config0} ${image}${without}; the OCI config of ${image}, read from its registry by digest, labels org.opencontainers.image.revision ${IMAGE_SHA}, a commit of acme/skills (gh api repos/acme/skills/commits?sha=${IMAGE_SHA}&until=`
    );
    expect(built.worker).toContain(atImage);
  }, 120_000);

  test("a version label with no revision label is the release tag <name>-v<version> of the repository, the longest name first", async () => {
    const image = `registry.example/acme/acme-image-worker@sha256:${digest}`;
    const built = await reprofiledLegion("label-version", {
      pulumi: pulumiOf(image),
      attested: {},
      labels: { "org.opencontainers.image.version": "2.0.0" },
    });
    expect(built.skills).toEqual(imaged);
    expect(built.worker).toStartWith(
      `${config0} ${image}${without}; the OCI config of ${image}, read from its registry by digest, labels org.opencontainers.image.version 2.0.0, so the release acme-image-v2.0.0, a tag of acme/skills (gh api repos/acme/skills/git/matching-refs/tags/acme-image-v2.0.0), whose last commit at or before `
    );
    expect(built.worker).toContain(atImage);
  }, 120_000);

  test.each([
    [
      "a revision label that is no commit of the repository",
      {
        "org.opencontainers.image.revision": "0b".repeat(20),
      },
      "labels org.opencontainers.image.revision " +
        "0b".repeat(20) +
        ", which name no commit or release tag of acme/skills",
    ],
    ["an image config with neither label", {}, "holds neither label"],
  ] as const)(
    "%s, and no comment above the image: line, leave the case out: it is not read at the repository's default branch",
    async (_name, labels, said) => {
      const image = `registry.example/acme/worker-plus@sha256:${digest}`;
      const built = await reprofiledLegion("label-none", {
        pulumi: pulumiOf(image),
        attested: {},
        labels,
        refused: true,
      });
      expect({ code: built.code, written: built.written }).toEqual({
        code: EXIT.failed,
        written: false,
      });
      expect(built.registryReads).toContain(image);
      expect(built.said).toContain(
        `pod profile not measurable: the worker image's commit of acme/skills is not known, and the repository's default branch is a guess, so the profile is not built: ${config0} ${image}${without}, and the OCI config of ${image}, read from its registry by digest, ${said}, and no comment line is right above its image: line: a Legion pod runs the worker image`
      );
      expect(built.said).not.toContain("is read at its default branch's commit");
    },
    120_000
  );
});

// Catches: an image comment that names the base's commit first and an older release after it
// ("before it …") read at the older release.
test("build-cases --reprofile of a Legion pane whose image has no provenance reads the worker image's packages at the commit the comment above its image: line names first, past a hex word that is no commit of the repository and before a tag it names later", async () => {
  const image = `registry.example/acme/worker-plus@sha256:${"5".repeat(64)}`;
  const pulumi = [
    "config:",
    "  legion:projects:",
    "    - key: ACME",
    // 0badc0de is no commit of acme/skills; pi-acme-v1.0.0, a tag of it, is at SHA.
    `      # worker-plus sha-0badc0de (run 41669921756), on the build of acme/skills ${IMAGE_WORD};`,
    "      # before it the build on pi-acme-v1.0.0.",
    `      image: ${image}`,
    "",
  ].join("\n");
  const built = await reprofiledLegion("commit-word", { pulumi, attested: {} });
  expect(built.skills).toEqual([`alpha ${IMAGE_SHA}`, `beta ${IMAGE_SHA}`, `omicron ${IMAGE_SHA}`]);
  expect(built.worker).toStartWith(
    `acme/infra:legion/Pulumi.prod.yaml at ${SHA} gives the project ACME the image ${image}, whose digest has no build provenance that names a commit (gh api repos/acme/skills/attestations/sha256:${"5".repeat(64)}); the comment right above its image: line names ${IMAGE_WORD}, the commit ${IMAGE_SHA} of acme/skills (gh api repos/acme/skills/commits?sha=${IMAGE_WORD}&until=`
  );
  expect(built.worker).toContain(
    `), so the image's units of acme/skills are read at ${IMAGE_SHA}: a Legion pod runs the worker image`
  );
}, 120_000);

/** GitHub's attestations of an image digest in acme/skills whose build provenance names `commit`
 *  of acme/skills as its first resolved dependency. */
function attestationOf(commit: string): string {
  const statement = {
    predicateType: "https://slsa.dev/provenance/v1",
    predicate: {
      buildDefinition: {
        resolvedDependencies: [
          {
            uri: "git+https://github.com/acme/skills@refs/heads/main",
            digest: { gitCommit: commit },
          },
        ],
      },
    },
  };
  const payload = Buffer.from(JSON.stringify(statement)).toString("base64");
  return JSON.stringify({ attestations: [{ bundle: { dsseEnvelope: { payload } } }] });
}

/** The legion case of the suite built again by build-cases --reprofile under `image-<name>`, with
 *  gh's answers to acme/infra's legion/Pulumi.prod.yaml at SHA (`pulumi`; a 404 when undefined),
 *  to ACME's project file (`project`; the suite's own when undefined) and to each attestation of
 *  acme/skills (`attested`, the commit by digest; a 404 for another digest): the profile's skills
 *  of acme/skills (`<name> <commit>`) and plugins of it (`<name> <commit> <path>`), and the
 *  sources of its evidence items of the worker image's and the daemon image's commits. `labels`
 *  are the OCI config labels the registry gives each image (no config when undefined, as for an
 *  image it does not hold), `images` the whole config it gives an image it names, over `labels`,
 *  `refusing` the images whose registry refuses the token of its tokenCommand (HTTP 403), which
 *  fails their read, and `registryReads` the images asked; `deploymentImages` the configuration's
 *  key of that name (the suite's configuration when undefined); `copies` the copies output (the
 *  suite's when undefined); `gh` answers a route it knows before the suite's answers; `refused`
 *  says the build is expected to leave the case out (exit 1) instead of writing it. */
async function reprofiledLegion(
  name: string,
  answers: {
    readonly pulumi?: string;
    readonly project?: string;
    readonly attested: Readonly<Record<string, string>>;
    readonly labels?: Readonly<Record<string, string>>;
    readonly images?: Readonly<Record<string, ImageConfig>>;
    readonly refusing?: readonly string[];
    readonly deploymentImages?: readonly {
      readonly repository: string;
      readonly dockerfile: string;
      readonly readSteps?: readonly { readonly sha256: string; readonly reason: string }[];
    }[];
    readonly copies?: string;
    readonly gh?: (route: string, args: readonly string[]) => GhResult | undefined;
    readonly refused?: true;
  }
) {
  const legion = await profileCase("legion");
  const dir = path.join(scratch, `image-${name}`);
  mkdirSync(dir, { recursive: true });
  const from = path.join(dir, "old.jsonl");
  writeFileSync(from, `${JSON.stringify(legion)}\n`);
  const fresh = path.join(dir, "reprofiled.jsonl");
  const lines: string[] = [];
  const standin = io(lines);
  const registryReads: string[] = [];
  const missing = { code: 1, stdout: "", stderr: "gh: Not Found (HTTP 404)\n" };
  let configFile = config;
  if (answers.deploymentImages !== undefined) {
    configFile = path.join(dir, "skill-cuts.json");
    writeFileSync(
      configFile,
      JSON.stringify({
        ...JSON.parse(readFileSync(config, "utf8")),
        deploymentImages: answers.deploymentImages,
      })
    );
  }
  const code = await main(
    [
      ...["build-cases", "--config", configFile, "--old", old],
      ...["--copies", answers.copies ?? copiesDir, "--count", countDir],
      ...["--reprofile", from, "--out", fresh],
    ],
    {
      ...standin,
      imageConfig: async (image) => {
        registryReads.push(image);
        // A configured registry's 401 or 403 fails the read (pod-image.ts's registryImageConfig).
        if (answers.refusing?.includes(image)) {
          const host = image.split("/")[0];
          throw new Error(
            `reading ${image}: ${host} refused the token of its tokenCommand (HTTP 403)`
          );
        }
        const own = answers.images?.[image];
        if (own !== undefined) return own;
        return answers.labels === undefined
          ? undefined
          : { env: [], labels: answers.labels, diffIds: [] };
      },
      gh: async (args) => {
        const route = args.find((arg) => arg.startsWith("repos/")) ?? "";
        const known = answers.gh?.(route, args);
        if (known !== undefined) {
          ghCalls.push([...args]);
          return known;
        }
        if (route === "repos/acme/infra/contents/legion/Pulumi.prod.yaml") {
          ghCalls.push([...args]);
          return answers.pulumi !== undefined && args.includes(`ref=${SHA}`)
            ? { code: 0, stdout: answers.pulumi, stderr: "" }
            : missing;
        }
        // ACME's project file names no image unless the test gives one, so the Pulumi config does.
        if (route === "repos/acme/infra/contents/legion/projects/10-ACME.yaml") {
          ghCalls.push([...args]);
          return {
            code: 0,
            stdout: answers.project ?? "key: ACME\ninstructions: acme.md\n",
            stderr: "",
          };
        }
        const digest = /^repos\/acme\/skills\/attestations\/sha256:([0-9a-f]{64})$/.exec(
          route
        )?.[1];
        if (digest !== undefined) {
          ghCalls.push([...args]);
          const commit = answers.attested[digest];
          return commit === undefined
            ? missing
            : { code: 0, stdout: attestationOf(commit), stderr: "" };
        }
        return standin.gh(args);
      },
    }
  );
  if (answers.refused !== true) {
    expect({ code, said: code === EXIT.ok ? "" : lines.join("\n") }).toEqual({
      code: EXIT.ok,
      said: "",
    });
  }
  const [c] = code === EXIT.ok ? loadCases(fresh, old) : [];
  const evidence = Object.fromEntries(c?.evidence?.map((each) => [each.item, each.source]) ?? []);
  return {
    code,
    said: lines.join("\n"),
    // Whether the case file holds a case, whatever the exit code.
    written:
      existsSync(fresh) &&
      readFileSync(fresh, "utf8")
        .split("\n")
        .some((line) => line !== ""),
    registryReads,
    skills: (c?.profile.skills ?? [])
      .filter((skill) => skill.repo === "acme/skills")
      .map((skill) => `${skill.name} ${skill.commit}`)
      .sort(),
    plugins: (c?.profile.plugins ?? [])
      .filter((plugin) => plugin.repo === "acme/skills")
      .map((plugin) => `${plugin.name} ${plugin.commit} ${plugin.path}`),
    worker: evidence["the worker image's commit of acme/skills"] ?? "",
    daemon: evidence["the daemon image's commit of acme/skills"] ?? "",
  };
}

// Catches: a deployment that keeps a project's values in its project file (a top-level image:
// and daemonImage:, where a deployment moved them out of its Pulumi.prod.yaml) read at the default
// branch, as the Pulumi config names no image for the project; and a daemon image of another
// build than the worker image, whose commit the role prompt is not composed at.
describe("build-cases --reprofile of a Legion pane whose project file names its images", () => {
  const image = (digest: string) => `registry.example/acme/worker@sha256:${digest}`;
  /** Image digests: ATTESTED's provenance names IMAGE_SHA, DAEMON's DAEMON_SHA, and PLUS and
   *  NESTED have none. */
  const ATTESTED = "1".repeat(64);
  const PLUS = "2".repeat(64);
  const NESTED = "7".repeat(64);
  const DAEMON = "8".repeat(64);
  /** The made-up commit of acme/skills that ACME's daemon image was built from. */
  const DAEMON_SHA = "d4".repeat(20);
  const attested = { [ATTESTED]: IMAGE_SHA, [DAEMON]: DAEMON_SHA };
  const own = `acme/infra:legion/projects/10-ACME.yaml at ${SHA}`;
  const pulumiAt = `acme/infra:legion/Pulumi.prod.yaml at ${SHA}`;
  /** acme/infra's Pulumi config with ACME's entry holding `fields`. */
  const pulumi = (fields: readonly string[]) =>
    [
      "config:",
      "  legion:projects:",
      "    - key: ACME",
      "      instructions: acme.md",
      ...fields.map((field) => `      ${field}`),
      "",
    ].join("\n");
  const names = (digest: string, commit: string) =>
    `gh api repos/acme/skills/attestations/sha256:${digest} names git+https://github.com/acme/skills@refs/heads/main at ${commit}`;
  const atImage = `, so the image's units of acme/skills are read at ${IMAGE_SHA}: a Legion pod runs the worker image that the deployment names for its project`;
  const imaged = [`alpha ${IMAGE_SHA}`, `beta ${IMAGE_SHA}`, `omicron ${IMAGE_SHA}`];
  const atPlugins =
    ", so the role prompt is composed at the commit the profile reads the plugins of acme/skills at: the Legion daemon";

  test("the project file's top-level image: line names the worker image over the Pulumi config's entry, decided by its digest's provenance, else by the base digest the comment right above it names; with no daemonImage: line, the role prompt is composed at the commit of the profile's plugins", async () => {
    const top = await reprofiledLegion("project-top", {
      project: `key: ACME\ninstructions: acme.md\nimage: ${image(ATTESTED)}\n`,
      pulumi: pulumi([`image: ${image(PLUS)}`]),
      attested,
    });
    expect(top.skills).toEqual(imaged);
    expect(top.plugins).toContain(`pi-acme ${IMAGE_SHA} packages/pi-acme`);
    expect(top.worker).toStartWith(
      `${own} gives the project ACME the image ${image(ATTESTED)}; ${names(ATTESTED, IMAGE_SHA)}${atImage}`
    );
    expect(top.daemon).toStartWith(
      `${own} has no top-level daemonImage: line, and ${pulumiAt} holds no entry - key: ACME with a daemonImage: line${atPlugins}`
    );

    const based = await reprofiledLegion("project-base", {
      project: `key: ACME\n# worker-plus, built on the base ${image(ATTESTED)}.\nimage: ${image(PLUS)}\n`,
      attested,
    });
    expect(based.skills).toEqual(imaged);
    expect(based.worker).toStartWith(
      `${own} gives the project ACME the image ${image(PLUS)}, whose digest has no build provenance that names a commit (gh api repos/acme/skills/attestations/sha256:${PLUS}); the comment right above its image: line names the image digest sha256:${ATTESTED}, whose build provenance (gh api repos/acme/skills/attestations/sha256:${ATTESTED}) names git+https://github.com/acme/skills@refs/heads/main at ${IMAGE_SHA}${atImage}`
    );
    expect(based.daemon).toStartWith(
      `${own} has no top-level daemonImage: line, and ${pulumiAt} does not exist (gh api repos/acme/infra/contents/legion/Pulumi.prod.yaml?ref=${SHA})${atPlugins}`
    );
  }, 60_000);

  test("a project file with no top-level image: line falls back to the Pulumi config's entry, its nested image: and its daemonImage: line not taken for the image; that daemonImage: line, over the entry's, names the daemon image, whose provenance gives the commit the role prompt is composed at", async () => {
    const daemonImage = `registry.example/acme/daemon@sha256:${DAEMON}`;
    const built = await reprofiledLegion("project-daemon", {
      project: `key: ACME\ninstructions: acme.md\nworker:\n  image: ${image(NESTED)}\ndaemonImage: ${daemonImage}\n`,
      pulumi: pulumi([`image: ${image(ATTESTED)}`, `daemonImage: ${image(ATTESTED)}`]),
      attested,
    });
    expect(built.skills).toEqual(imaged);
    expect(built.worker).toStartWith(
      `${pulumiAt} gives the project ACME the image ${image(ATTESTED)}; ${names(ATTESTED, IMAGE_SHA)}${atImage}`
    );
    expect(built.daemon).toStartWith(
      `${own} gives the project ACME the daemonImage ${daemonImage}; ${names(DAEMON, DAEMON_SHA)}, so the role prompt is composed from acme/skills at ${DAEMON_SHA}: the Legion daemon`
    );
  }, 60_000);
});

// Catches: a project image that the deployment built on a worker image in a private registry
// (its labels name the deployment's commit, not the worker repository's) left out as not
// measurable; and a Dockerfile's ARG default taken for the image's base, though a build can pass
// another: the images' rootfs.diff_ids confirm it, not the manifests' layer digests, which differ
// where the builder compressed a layer again.
describe("build-cases --reprofile of a Legion pane whose image the deployment built on a worker image", () => {
  const layer = (n: number) => `sha256:${String(n).repeat(64)}`;
  const REPOSITORY = "registry.example.test/acme/deploy/worker";
  const A = "a".repeat(64);
  const B = "b".repeat(64);
  const IMAGE = `${REPOSITORY}@sha256:${A}`;
  const BASE = `ghcr.io/acme/worker@sha256:${B}`;
  /** The commit of acme/infra (the suite's deployment) the image was built at. */
  const REVISION = "de".repeat(20);
  const VERSION = "sha-dededededede";
  const DOCKERFILE = "images/worker.Dockerfile";
  const RECIPE = `ARG LEGION_WORKER=${BASE}\nFROM \${LEGION_WORKER}\nRUN echo the deployment's layer\n`;
  const LAYERS = [1, 2, 3, 4, 5].map(layer);
  const own = `acme/infra:legion/projects/10-ACME.yaml at ${SHA} gives the project ACME the image ${IMAGE}, whose digest has no build provenance that names a commit (gh api repos/acme/skills/attestations/sha256:${A}), and the OCI config of ${IMAGE}, read from its registry by digest, labels org.opencontainers.image.revision ${REVISION} and org.opencontainers.image.version ${VERSION}, which name no commit or release tag of acme/skills`;
  const step = `the configuration's deploymentImages names ${REPOSITORY}, an image acme/infra builds on a worker image`;
  const labelled = `${step}: the OCI config of ${IMAGE} labels org.opencontainers.image.source https://github.com/acme/infra and org.opencontainers.image.revision ${REVISION}, a commit of acme/infra (gh api repos/acme/infra/commits?sha=${REVISION}&until=`;
  const recipe = `acme/infra:${DOCKERFILE} at ${REVISION} (gh api repos/acme/infra/contents/${DOCKERFILE}?ref=${REVISION}) builds its final stage on ${BASE}`;
  const notKnown =
    "pod profile not measurable: the worker image's commit of acme/skills is not known, and the repository's default branch is a guess, so the profile is not built: ";
  const rule = ": a Legion pod runs the worker image";

  /** The legion case built again with ACME's project file naming IMAGE, the registry giving
   *  IMAGE's config (its source label `source`) and BASE's (its diff ids `baseLayers`, its
   *  revision label IMAGE_SHA), acme/infra holding REVISION and RECIPE at it, and the
   *  configuration's deploymentImages naming `listed` with DOCKERFILE. */
  const built = (
    name: string,
    {
      source = "https://github.com/acme/infra",
      baseLayers = LAYERS.slice(0, 3),
      listed = REPOSITORY,
      refused,
    }: {
      source?: string;
      baseLayers?: readonly string[];
      listed?: string;
      refused?: true;
    } = {}
  ) =>
    reprofiledLegion(`deployed-${name}`, {
      project: `key: ACME\ninstructions: acme.md\nimage: ${IMAGE}\n`,
      attested: {},
      images: {
        [IMAGE]: {
          env: [],
          labels: {
            "org.opencontainers.image.source": source,
            "org.opencontainers.image.revision": REVISION,
            "org.opencontainers.image.version": VERSION,
          },
          diffIds: LAYERS,
        },
        [BASE]: {
          env: [],
          labels: { "org.opencontainers.image.revision": IMAGE_SHA },
          diffIds: baseLayers,
        },
      },
      deploymentImages: [{ repository: listed, dockerfile: DOCKERFILE }],
      gh: (route, args) => {
        const ok = (stdout: string): GhResult => ({ code: 0, stdout, stderr: "" });
        if (route.startsWith(`repos/acme/infra/commits?sha=${REVISION}&`))
          return ok(`${REVISION}\n`);
        if (route === `repos/acme/infra/contents/${DOCKERFILE}` && args.includes(`ref=${REVISION}`))
          return ok(RECIPE);
        return undefined;
      },
      ...(refused === undefined ? {} : { refused }),
    });

  test("an image the configuration lists, whose labels name the deployment's commit, takes the commit of the base its Dockerfile there names, once the base's diff ids are the first of the image's", async () => {
    const result = await built("base");
    expect(result.skills).toEqual([
      `alpha ${IMAGE_SHA}`,
      `beta ${IMAGE_SHA}`,
      `omicron ${IMAGE_SHA}`,
    ]);
    expect(result.plugins).toContain(`pi-acme ${IMAGE_SHA} packages/pi-acme`);
    expect(result.registryReads).toContain(BASE);
    // The built-on step decides: the COPY step is not taken (the rule the evidence quotes after
    // its reads states it), and the version label, which names no commit of acme/skills here,
    // confirms nothing and refutes nothing.
    const reads = result.worker.slice(0, result.worker.indexOf(rule));
    expect(reads).not.toContain("copies the worker's packages from");
    expect(result.worker).toStartWith(`${own}; ${labelled}`);
    expect(result.worker).toContain(
      `); ${recipe}; the 3 rootfs.diff_ids of ${BASE} are the first 3 of the 5 of ${IMAGE}, so the image is built on it; the base's digest has no build provenance that names a commit (gh api repos/acme/skills/attestations/sha256:${B}); the OCI config of ${BASE}, read from its registry by digest, labels org.opencontainers.image.revision ${IMAGE_SHA}, a commit of acme/skills (gh api repos/acme/skills/commits?sha=${IMAGE_SHA}&until=`
    );
    expect(result.worker).toContain(
      `), so the image's units of acme/skills are read at ${IMAGE_SHA}${rule}`
    );
  }, 120_000);

  test("a base whose diff ids are not the first of the image's leaves the case out, naming the mismatch", async () => {
    const result = await built("mismatch", {
      baseLayers: [layer(1), layer(9), layer(3)],
      refused: true,
    });
    expect({ code: result.code, written: result.written }).toEqual({
      code: EXIT.failed,
      written: false,
    });
    expect(result.said).toContain(`${notKnown}${own}; ${labelled}`);
    expect(result.said).toContain(
      `); ${recipe}, but the 3 rootfs.diff_ids of ${BASE} are not the first 3 of the 5 of ${IMAGE} (entry 2 differs), so ${IMAGE} is not built on the base the Dockerfile names${rule}`
    );
  }, 120_000);

  test("an image whose source label names another repository than the deployment leaves the case out, naming the label", async () => {
    const result = await built("source", {
      source: "https://github.com/acme/other",
      refused: true,
    });
    expect({ code: result.code, written: result.written }).toEqual({
      code: EXIT.failed,
      written: false,
    });
    expect(result.said).toContain(
      `${notKnown}${own}; ${step}, but the OCI config of ${IMAGE} labels org.opencontainers.image.source https://github.com/acme/other, not the deployment's https://github.com/acme/infra${rule}`
    );
    expect(result.registryReads).not.toContain(BASE);
  }, 120_000);

  test("an image the configuration does not list is left out, its commit not known: its Dockerfile and its base are not read", async () => {
    const calls = ghCalls.length;
    const result = await built("unlisted", { listed: `${REPOSITORY}-other`, refused: true });
    expect({ code: result.code, written: result.written }).toEqual({
      code: EXIT.failed,
      written: false,
    });
    expect(result.said).toContain(
      `${notKnown}${own}, and no comment line is right above its image: line${rule}`
    );
    expect(result.registryReads).not.toContain(BASE);
    expect(
      ghCalls
        .slice(calls)
        .filter((args) => args.includes(`repos/acme/infra/contents/${DOCKERFILE}`))
    ).toEqual([]);
  }, 120_000);
});

// Catches: an image the deployment built on a toolchain image, whose final stage is FROM that
// toolchain and COPYs the worker's packages from a stage FROM the worker image, left out as not
// measurable: the toolchain was read for the worker, and it names no commit of the worker's
// repository (10 saved cases on 4 such digests lost the commit their pin comment gave them); and
// the same 10 cases left out once the COPY step came after the built-on step, as the toolchain's
// registry refuses the token of its tokenCommand (HTTP 403) and the toolchain was read first. And
// such a COPY taken on the Dockerfile's word alone, though a build can pass the stage another
// worker image than its ARG default: the image's own version label must name the stage's commit.
// And an image built on an older worker image that copies the packages from a newer one read at
// the base's commit, though the COPY wrote the newer packages over the base's; and one whose
// final stage writes into a copied package after its COPY read at the stage's commit. And a RUN
// that writes into a copied package through a directory above it (rm -rf /opt/legion/*) missed,
// one that names a sibling of it (/opt/legion/pi-acme.bak) taken for it, and an ADD into a
// package taken as no write of it: the image read at the stage's commit, or at its base's.
describe("build-cases --reprofile of a Legion pane whose image the deployment built on a toolchain image, copying the worker's packages from a stage built on the worker image", () => {
  const REPOSITORY = "registry.example.test/acme/deploy/worker-lean";
  const A = "c".repeat(64);
  const B = "b".repeat(64);
  const D = "d".repeat(64);
  const T = "e".repeat(64);
  const IMAGE = `${REPOSITORY}@sha256:${A}`;
  const WORKER = `ghcr.io/acme/worker@sha256:${B}`;
  /** An older worker image, whose digest's build provenance names SHA. */
  const OLDER = `ghcr.io/acme/worker@sha256:${D}`;
  /** A toolchain image, whose registry refuses the token of its tokenCommand (HTTP 403), as the
   *  saved cases' toolchain registry does. */
  const TOOLCHAIN = `registry.example.test/acme/toolchain@sha256:${T}`;
  /** The commit of acme/infra (the suite's deployment) the image was built at. */
  const REVISION = "de".repeat(20);
  const DOCKERFILE = "images/worker-lean.Dockerfile";
  const LAYERS = [1, 2, 3, 4, 5].map((n) => `sha256:${String(n).repeat(64)}`);
  /** The release tags of acme/skills the stand-in lists, each at its commit: the worker's release
   *  at IMAGE_SHA, which WORKER's build provenance names too, and an older one at SHA. */
  const RELEASES: Readonly<Record<string, string>> = {
    "skills-v20.25.1": IMAGE_SHA,
    "skills-v20.24.0": SHA,
  };
  /** The final stage's COPY lines: the worker's packages, the profile's pi-acme and pi-legion
   *  among them, from the stage legion. The image's Dockerfile at REVISION has two stages FROM
   *  the worker image, and a final stage FROM the toolchain image (or another) with such lines. */
  const COPIES = [
    "COPY --from=legion /opt/omp /opt/omp",
    "COPY --from=legion --chown=0:0 /opt/legion/pi-envoy /opt/legion/pi-envoy",
    "COPY --from=legion --chown=0:0 /opt/legion/pi-legion /opt/legion/pi-legion",
    "COPY --from=legion --chown=0:0 /opt/legion/pi-acme /opt/legion/pi-acme",
    "COPY --from=legion /opt/legion/bin/ /opt/legion/bin/",
  ];
  /** The unit of the suite's prefix-table.tsv rows under /opt/legion/pi-acme/, an Oh My Pi unit:
   *  installers.tsv installs it for host omp. */
  const PI_ACME = "package:@acme/pi-acme (acme/skills:packages/pi-acme/package.json)";
  /** The prefix-table.tsv row of pi-legion: a copy of alpha that no read of the suite is under,
   *  placed by PI_ACME. */
  const PI_LEGION_ROW = [
    "/opt/legion/pi-legion/dist/skills/alpha/",
    "acme/skills",
    "skills/alpha",
    "alpha",
    PI_ACME,
  ];
  /** The suite's copies output with PI_LEGION_ROW added: the profile's packages in the worker
   *  image are pi-acme and pi-legion. */
  const copies = path.join(scratch, "copies-pi-legion");
  beforeAll(() => {
    cpSync(copiesDir, copies, { recursive: true });
    const table = path.join(copies, "prefix-table.tsv");
    writeFileSync(table, `${readFileSync(table, "utf8")}${PI_LEGION_ROW.join("\t")}\n`);
  });
  /** A Claude Code plugin's unit, as copies.ts gives the plugin a build script generates from a
   *  package's staged skills. */
  const CLAUDE_ACME = "plugin:claude-acme (acme/skills:packages/claude-acme/scripts/build.ts)";
  /** The copies output `copies-copied-<name>`: the suite's with PI_LEGION_ROW, a prefix-table.tsv
   *  row under /opt/legion/claude-acme/ (a copy of alpha that no read of the suite is under)
   *  placed by the units `via`, and the installers.tsv row by which CLAUDE_ACME ships alpha for
   *  Claude Code (host claude-code). */
  const withClaudeAcme = (name: string, via: string) =>
    copiesWith(`copies-copied-${name}`, {
      "prefix-table.tsv": [
        PI_LEGION_ROW,
        [
          "/opt/legion/claude-acme/claude-acme/skills/alpha/",
          "acme/skills",
          "skills/alpha",
          "alpha",
          via,
        ],
      ],
      "installers.tsv": [
        [
          "acme/skills",
          "packages/claude-acme/scripts/build.ts",
          "build-script",
          "ships",
          "plugin:claude-acme",
          "claude-code",
          "alpha",
          "acme/skills",
          "skills/alpha",
          "",
        ],
      ],
    });
  const imaged = [`alpha ${IMAGE_SHA}`, `beta ${IMAGE_SHA}`, `omicron ${IMAGE_SHA}`];
  /** The image's own step, whose labels (the version label `version`) name no commit of
   *  acme/skills. */
  const own = (version?: string) =>
    `acme/infra:legion/projects/10-ACME.yaml at ${SHA} gives the project ACME the image ${IMAGE}, whose digest has no build provenance that names a commit (gh api repos/acme/skills/attestations/sha256:${A}), and the OCI config of ${IMAGE}, read from its registry by digest, labels org.opencontainers.image.revision ${REVISION}${version === undefined ? "" : ` and org.opencontainers.image.version ${version}`}, which name no commit or release tag of acme/skills`;
  const labelled = `the configuration's deploymentImages names ${REPOSITORY}, an image acme/infra builds on a worker image: the OCI config of ${IMAGE} labels org.opencontainers.image.source https://github.com/acme/infra and org.opencontainers.image.revision ${REVISION}, a commit of acme/infra (gh api repos/acme/infra/commits?sha=${REVISION}&until=`;
  /** The Dockerfile, read at REVISION. */
  const recipe = `); acme/infra:${DOCKERFILE} at ${REVISION} (gh api repos/acme/infra/contents/${DOCKERFILE}?ref=${REVISION})`;
  /** The COPY step, which decides where the final stage, FROM `base`, copies into the profile's
   *  packages: the base is not read. */
  const copying = (base = `${TOOLCHAIN} (the default of the ARG TOOLCHAIN)`) =>
    `${recipe}, whose final stage copies into the profile's packages: a COPY writes the copied files over whatever the final stage's base holds at those paths, so where it copies them from decides, and its base, FROM ${base}, is not read; `;
  const packages = "the profile's packages (the prefix-table.tsv rows under /opt/legion/)";
  const fromStage = `the final stage copies /opt/legion/pi-acme, /opt/legion/pi-legion, ${packages}, from the stage legion, FROM ${WORKER} (the default of the ARG LEGION_WORKER)`;
  const fromLegion = (base?: string) =>
    `${copying(base)}${fromStage}; gh api repos/acme/skills/attestations/sha256:${B} names git+https://github.com/acme/skills@refs/heads/main at ${IMAGE_SHA}`;
  const BY_LABEL = "the COPY is confirmed by the image's version label, not by layer content";
  /** The version label's release tag `tag` of acme/skills, up to the time of its read. */
  const release = (tag: string) =>
    `the OCI config of ${IMAGE} labels org.opencontainers.image.version ${tag.slice("skills-v".length)}, so the release ${tag}, a tag of acme/skills (gh api repos/acme/skills/git/matching-refs/tags/${tag}), whose last commit at or before `;
  const notKnown =
    "pod profile not measurable: the worker image's commit of acme/skills is not known, and the repository's default branch is a guess, so the profile is not built: ";
  const rule = ": a Legion pod runs the worker image";

  /** The legion case built again with ACME's project file naming IMAGE; the registry giving
   *  IMAGE's config (its version label `version`, none when undefined), WORKER's and OLDER's, each
   *  with the image's first 3 diff ids, and refusing TOOLCHAIN's read; WORKER's digest's build
   *  provenance naming IMAGE_SHA and OLDER's SHA; acme/infra holding REVISION and at it the
   *  Dockerfile whose final stage is FROM `final` and holds `copyLines`, then `later`; the
   *  configuration's deploymentImages naming REPOSITORY, with `readSteps` where given; and the
   *  copies output `output` (`copies` when undefined). */
  const copied = (
    name: string,
    {
      version,
      final = `\${TOOLCHAIN}`,
      copyLines = COPIES,
      later = [],
      readSteps,
      refused,
      output = copies,
    }: {
      version?: string;
      final?: string;
      copyLines?: readonly string[];
      later?: readonly string[];
      readSteps?: readonly { readonly sha256: string; readonly reason: string }[];
      refused?: true;
      output?: string;
    }
  ) =>
    reprofiledLegion(`copied-${name}`, {
      project: `key: ACME\ninstructions: acme.md\nimage: ${IMAGE}\n`,
      attested: { [B]: IMAGE_SHA, [D]: SHA },
      images: {
        [IMAGE]: {
          env: [],
          labels: {
            "org.opencontainers.image.source": "https://github.com/acme/infra",
            "org.opencontainers.image.revision": REVISION,
            ...(version === undefined ? {} : { "org.opencontainers.image.version": version }),
          },
          diffIds: LAYERS,
        },
        [WORKER]: { env: [], labels: {}, diffIds: LAYERS.slice(0, 3) },
        [OLDER]: { env: [], labels: {}, diffIds: LAYERS.slice(0, 3) },
      },
      refusing: [TOOLCHAIN],
      deploymentImages: [
        {
          repository: REPOSITORY,
          dockerfile: DOCKERFILE,
          ...(readSteps === undefined ? {} : { readSteps }),
        },
      ],
      copies: output,
      gh: (route, args) => {
        const ok = (stdout: string): GhResult => ({ code: 0, stdout, stderr: "" });
        if (route.startsWith(`repos/acme/infra/commits?sha=${REVISION}&`))
          return ok(`${REVISION}\n`);
        if (route === `repos/acme/infra/contents/${DOCKERFILE}` && args.includes(`ref=${REVISION}`))
          return ok(
            `ARG LEGION_WORKER=${WORKER}\nARG TOOLCHAIN=${TOOLCHAIN}\nFROM \${LEGION_WORKER} AS legion\nFROM \${LEGION_WORKER} AS chrome\nFROM ${final}\n${[...copyLines, ...later].join("\n")}\n`
          );
        const listed = /^repos\/acme\/skills\/git\/matching-refs\/tags\/(.+)$/.exec(route)?.[1];
        const tag = decodeURIComponent(listed ?? "");
        if (Object.hasOwn(RELEASES, tag)) return ok(`refs/tags/${tag}\n`);
        const ref = /^repos\/acme\/skills\/commits\?sha=([^&]+)&/.exec(route)?.[1];
        const at = RELEASES[decodeURIComponent(ref ?? "")];
        return at === undefined ? undefined : ok(`${at}\n`);
      },
      ...(refused === undefined ? {} : { refused }),
    });

  test("an image whose final stage is FROM a toolchain image and copies the profile's packages from one stage FROM the worker image takes that stage's commit, by its digest's build provenance, once the image's version label is the release whose tag is at that commit: the toolchain, whose registry refuses its token, is not read, and a later RUN that names no package changes nothing", async () => {
    const result = await copied("release", { version: "20.25.1", later: ["RUN apt-get update"] });
    expect(result.skills).toEqual(imaged);
    expect(result.plugins).toContain(`pi-acme ${IMAGE_SHA} packages/pi-acme`);
    expect(result.registryReads).not.toContain(TOOLCHAIN);
    expect(result.worker).toStartWith(`${own("20.25.1")}; ${labelled}`);
    expect(result.worker).toContain(`${fromLegion()}; ${release("skills-v20.25.1")}`);
    expect(result.worker).toContain(
      ` is ${IMAGE_SHA}, so the label confirms the stage's commit: ${BY_LABEL}, so the image's units of acme/skills are read at ${IMAGE_SHA}${rule}`
    );
  }, 120_000);

  test("a version label sha-<hex> whose hex starts the stage's commit confirms it", async () => {
    const version = `sha-${IMAGE_SHA.slice(0, 12)}`;
    const result = await copied("sha", { version });
    expect(result.skills).toEqual(imaged);
    expect(result.worker).toStartWith(`${own(version)}; ${labelled}`);
    expect(result.worker).toContain(
      `${fromLegion()}; the OCI config of ${IMAGE} labels org.opencontainers.image.version ${version}, whose ${IMAGE_SHA.slice(0, 12)} starts the stage's commit ${IMAGE_SHA}, so the label confirms the stage's commit: ${BY_LABEL}, so the image's units of acme/skills are read at ${IMAGE_SHA}${rule}`
    );
  }, 120_000);

  test.each([
    [
      "a release whose tag is at another commit",
      "20.24.0",
      [`, but ${release("skills-v20.24.0")}`, ` is ${SHA}, not the stage's commit ${IMAGE_SHA}`],
    ],
    [
      "a sha-<hex> of another commit",
      `sha-${SHA.slice(0, 12)}`,
      [
        `, but the OCI config of ${IMAGE} labels org.opencontainers.image.version sha-${SHA.slice(0, 12)}, whose ${SHA.slice(0, 12)} does not start the stage's commit ${IMAGE_SHA}`,
      ],
    ],
    [
      "no version label",
      undefined,
      [
        `, but the OCI config of ${IMAGE} has no org.opencontainers.image.version label, so nothing confirms the stage's commit ${IMAGE_SHA}`,
      ],
    ],
    [
      "a version label that names no commit",
      "nightly",
      [
        `, but the OCI config of ${IMAGE} labels org.opencontainers.image.version nightly, which names no commit (sha-<hex>) and no release tag of acme/skills (skills-vnightly, vnightly, worker-lean-vnightly, worker-vnightly), so nothing confirms the stage's commit ${IMAGE_SHA}`,
      ],
    ],
  ] as const)(
    "%s leaves the case out, naming the label and the stage's commit: neither is preferred",
    async (name, version, said) => {
      const result = await copied(`label-${name.replaceAll(" ", "-")}`, {
        ...(version === undefined ? {} : { version }),
        refused: true,
      });
      expect({ code: result.code, written: result.written }).toEqual({
        code: EXIT.failed,
        written: false,
      });
      expect(result.said).toContain(`${notKnown}${own(version)}; ${labelled}`);
      expect(result.said).toContain(`${fromLegion()}${said[0]}`);
      expect(result.said).toContain(`${said.at(-1)}: ${BY_LABEL}${rule}`);
    },
    120_000
  );

  test.each([
    [
      "copied from another stage",
      COPIES.map((line) =>
        line.includes("pi-legion") ? line.replace("--from=legion", "--from=chrome") : line
      ),
      "/opt/legion/pi-legion from the stage chrome",
    ],
    [
      "not copied",
      COPIES.filter((line) => !line.includes("pi-legion")),
      "/opt/legion/pi-legion not at all",
    ],
  ] as const)(
    "a package of the profile that the final stage %s leaves the case out, naming it",
    async (name, copyLines, where) => {
      const result = await copied(`package-${name.replaceAll(" ", "-")}`, {
        version: "20.25.1",
        copyLines,
        refused: true,
      });
      expect({ code: result.code, written: result.written }).toEqual({
        code: EXIT.failed,
        written: false,
      });
      expect(result.said).toContain(`${notKnown}${own("20.25.1")}; ${labelled}`);
      expect(result.said).toContain(
        `${copying()}the final stage does not copy each of ${packages} from one stage: /opt/legion/pi-acme from the stage legion, ${where}${rule}`
      );
    },
    120_000
  );

  // Catches: a package of the worker image that the profile does not use, one that only a Claude
  // Code unit places (a Claude Code plugin's package, which installs nothing in a pod's Oh My Pi
  // profile), required of the COPY step: an image whose final stage copies the Oh My Pi packages
  // from the stage and the Claude Code plugin's package not at all was left out, naming it.
  test("an image whose final stage copies the profile's packages from one stage and does not copy a package that only a Claude Code unit places takes that stage's commit: that package is no package of the profile", async () => {
    const result = await copied("claude-only", {
      version: "20.25.1",
      output: withClaudeAcme("claude-only", CLAUDE_ACME),
    });
    expect(result.skills).toEqual(imaged);
    expect(result.worker).toStartWith(`${own("20.25.1")}; ${labelled}`);
    expect(result.worker).toContain(`${fromLegion()}; ${release("skills-v20.25.1")}`);
    expect(result.worker).toContain(
      ` is ${IMAGE_SHA}, so the label confirms the stage's commit: ${BY_LABEL}, so the image's units of acme/skills are read at ${IMAGE_SHA}${rule}`
    );
  }, 120_000);

  test("a package that an Oh My Pi unit places as well as a Claude Code unit is one of the profile's: a final stage that does not copy it leaves the case out, naming it", async () => {
    const result = await copied("claude-and-omp", {
      version: "20.25.1",
      output: withClaudeAcme("claude-and-omp", `${CLAUDE_ACME}; ${PI_ACME}`),
      refused: true,
    });
    expect({ code: result.code, written: result.written }).toEqual({
      code: EXIT.failed,
      written: false,
    });
    expect(result.said).toContain(`${notKnown}${own("20.25.1")}; ${labelled}`);
    expect(result.said).toContain(
      `${copying()}the final stage does not copy each of ${packages} from one stage: /opt/legion/claude-acme not at all, /opt/legion/pi-acme from the stage legion, /opt/legion/pi-legion from the stage legion${rule}`
    );
  }, 120_000);

  test("a package of the profile that the final stage copies from another stage leaves the case out beside a package that only a Claude Code unit places, naming the profile's packages alone", async () => {
    const result = await copied("claude-only-another-stage", {
      version: "20.25.1",
      copyLines: COPIES.map((line) =>
        line.includes("pi-acme") ? line.replace("--from=legion", "--from=chrome") : line
      ),
      output: withClaudeAcme("claude-only-another-stage", CLAUDE_ACME),
      refused: true,
    });
    expect({ code: result.code, written: result.written }).toEqual({
      code: EXIT.failed,
      written: false,
    });
    expect(result.said).toContain(`${notKnown}${own("20.25.1")}; ${labelled}`);
    expect(result.said).toContain(
      `${copying()}the final stage does not copy each of ${packages} from one stage: /opt/legion/pi-acme from the stage chrome, /opt/legion/pi-legion from the stage legion${rule}`
    );
  }, 120_000);

  test("an image whose final stage is FROM an older worker image, which the image is built on, and copies the profile's packages from a stage FROM a newer one takes the newer one's commit: the COPY step decides, and the base is not read", async () => {
    const result = await copied("built-on-and-copied", { version: "20.25.1", final: OLDER });
    expect(result.skills).toEqual(imaged);
    expect(result.registryReads).not.toContain(OLDER);
    expect(result.worker).toStartWith(`${own("20.25.1")}; ${labelled}`);
    expect(result.worker).toContain(`${fromLegion(OLDER)}; ${release("skills-v20.25.1")}`);
    expect(result.worker).toContain(
      ` is ${IMAGE_SHA}, so the label confirms the stage's commit: ${BY_LABEL}, so the image's units of acme/skills are read at ${IMAGE_SHA}${rule}`
    );
  }, 120_000);

  test("an image whose final stage is FROM an older worker image, which the image is built on, and copies one of the profile's packages from a stage FROM a newer one and the other not leaves the case out, naming the package not copied: the base is not read", async () => {
    const result = await copied("built-on-and-partly-copied", {
      version: "20.25.1",
      final: OLDER,
      copyLines: COPIES.filter((line) => !line.includes("pi-legion")),
      refused: true,
    });
    expect({ code: result.code, written: result.written, skills: result.skills }).toEqual({
      code: EXIT.failed,
      written: false,
      skills: [],
    });
    expect(result.registryReads).not.toContain(OLDER);
    expect(result.said).toContain(`${notKnown}${own("20.25.1")}; ${labelled}`);
    expect(result.said).toContain(
      `${copying(OLDER)}the final stage does not copy each of ${packages} from one stage: /opt/legion/pi-acme from the stage legion, /opt/legion/pi-legion not at all${rule}`
    );
  }, 120_000);

  test("an image whose final stage is FROM the worker image and copies none of the profile's packages takes the commit of that base, once its diff ids are the first of the image's", async () => {
    const result = await copied("built-on", {
      final: `\${LEGION_WORKER}`,
      copyLines: COPIES.filter((line) => !line.includes("/opt/legion/pi-")),
    });
    expect(result.skills).toEqual(imaged);
    expect(result.registryReads).toContain(WORKER);
    expect(result.worker).toStartWith(`${own()}; ${labelled}`);
    expect(result.worker).toContain(
      `${recipe} builds its final stage on ${WORKER}; the 3 rootfs.diff_ids of ${WORKER} are the first 3 of the 5 of ${IMAGE}, so the image is built on it; gh api repos/acme/skills/attestations/sha256:${B} names git+https://github.com/acme/skills@refs/heads/main at ${IMAGE_SHA}, so the image's units of acme/skills are read at ${IMAGE_SHA}${rule}`
    );
  }, 120_000);

  /** The hash a `readSteps` entry lists for a step of the text `text`: its SHA-256 in hex. */
  const stepSha = (text: string) => createHash("sha256").update(text).digest("hex");
  /** The COPY step's words where the RUN whose text is `text` (`shown` in the evidence), after the
   *  final stage's last COPY of `pkg`, names a path of it and leaves the case out. */
  const laterRefusal = (text: string, shown: string, pkg: string) =>
    `${copying()}${fromStage}, but its step "${shown}", after its last COPY of ${pkg}, names it, a path in it or a directory above it, so the image's ${pkg} need not be the stage's; the SHA-256 of the step's text is ${stepSha(text)}, which the readSteps of the image's deploymentImages entry do not list${rule}`;
  /** A RUN longer than the evidence shows of a step. */
  const LONG_RUN =
    "RUN set -eux; apt-get update; apt-get install -y --no-install-recommends jq ripgrep; rm -rf /var/lib/apt/lists/* /opt/legion/pi-legion/dist/skills/alpha";
  test.each([
    [
      "a RUN that names a path in a copied package",
      "RUN rm -rf /opt/legion/pi-acme/dist/skills/alpha",
      "RUN rm -rf /opt/legion/pi-acme/dist/skills/alpha",
      "/opt/legion/pi-acme",
    ],
    [
      "a long RUN that names a path in a copied package",
      LONG_RUN,
      `${LONG_RUN.slice(0, 120)}…`,
      "/opt/legion/pi-legion",
    ],
    [
      "a RUN that names the copied packages' directory by a glob",
      "RUN rm -rf /opt/legion/*",
      "RUN rm -rf /opt/legion/*",
      "/opt/legion/pi-acme",
    ],
    [
      "a RUN that names a directory above the copied packages",
      "RUN chown -R 0:0 /opt",
      "RUN chown -R 0:0 /opt",
      "/opt/legion/pi-acme",
    ],
  ] as const)(
    "%s after the final stage's COPY lines leaves the case out, naming the step, cut short, the package and the step's hash",
    async (name, step, shown, pkg) => {
      const result = await copied(`later-${name.replaceAll(" ", "-")}`, {
        version: "20.25.1",
        later: [step],
        refused: true,
      });
      expect({ code: result.code, written: result.written }).toEqual({
        code: EXIT.failed,
        written: false,
      });
      expect(result.said).toContain(`${notKnown}${own("20.25.1")}; ${labelled}`);
      expect(result.said).toContain(laterRefusal(step, shown, pkg));
    },
    120_000
  );

  test("a RUN after the final stage's COPY lines that names a sibling of a copied package, another path or / alone changes nothing: paths are compared name by name", async () => {
    const result = await copied("later-sibling", {
      version: "20.25.1",
      later: [
        "RUN rm -rf /opt/legion/pi-acme.bak /opt/legion/pi-acme-old /var/lib/apt/lists/* && ls /",
      ],
    });
    expect(result.skills).toEqual(imaged);
    expect(result.worker).toContain(`${fromLegion()}; ${release("skills-v20.25.1")}`);
    expect(result.worker).toContain(
      ` is ${IMAGE_SHA}, so the label confirms the stage's commit: ${BY_LABEL}, so the image's units of acme/skills are read at ${IMAGE_SHA}${rule}`
    );
  }, 120_000);

  /** A RUN that unpacks an archive into /opt, a directory above the copied packages, continued on
   *  a second line, and its text as pod-image.ts's dockerfileStages gives it, the one a
   *  `readSteps` hash is of: the lines joined, each run of white space one space. */
  const UNPACK = "RUN tar -xzf sdk.tar.gz \\\n    -C /opt";
  const UNPACKED = "RUN tar -xzf sdk.tar.gz -C /opt";
  const READ =
    "unpacks sdk.tar.gz into /opt/sdk, a directory of its own; it writes no path under /opt/legion";

  // Catches: a RUN a person read and found to write no copied package (one that unpacks an
  // archive into a directory above the packages) leaving out the image even once the
  // configuration lists its text's hash, or a case kept by such a listing whose evidence does not
  // say which step the listing cleared and why.
  test("a RUN after the final stage's COPY lines that names a directory above the copied packages, whose text's SHA-256 the image's deploymentImages entry lists in readSteps, changes nothing: the case takes the stage's commit, and the evidence names the step, its hash and the reason", async () => {
    const result = await copied("read-step-listed", {
      version: "20.25.1",
      later: [UNPACK],
      readSteps: [{ sha256: stepSha(UNPACKED), reason: READ }],
    });
    expect(result.skills).toEqual(imaged);
    expect(result.worker).toContain(
      `${copying()}${fromStage}; its step "${UNPACKED}", after its last COPY of /opt/legion/pi-acme and /opt/legion/pi-legion, names a path of each, but a person read it: the readSteps of the image's deploymentImages entry list the SHA-256 of the step's text, ${stepSha(UNPACKED)}, with the reason "${READ}"; gh api repos/acme/skills/attestations/sha256:${B} names git+https://github.com/acme/skills@refs/heads/main at ${IMAGE_SHA}; ${release("skills-v20.25.1")}`
    );
    expect(result.worker).toContain(
      ` is ${IMAGE_SHA}, so the label confirms the stage's commit: ${BY_LABEL}, so the image's units of acme/skills are read at ${IMAGE_SHA}${rule}`
    );
  }, 120_000);

  // Catches: a listed hash taken for a step it is not of (a step edited since a person read it),
  // or a refusal that does not print the hash a person would list once they read the step.
  test.each([
    ["lists no hash", undefined],
    [
      "lists the hash of the step edited by one character",
      [{ sha256: stepSha("RUN tar -xzf sdk.tar.xz -C /opt"), reason: READ }],
    ],
  ] as const)(
    "a RUN after the final stage's COPY lines that names a directory above the copied packages leaves the case out where readSteps %s, naming the step, the package and the step's hash",
    async (name, readSteps) => {
      const result = await copied(`read-step-${name.replaceAll(" ", "-")}`, {
        version: "20.25.1",
        later: [UNPACK],
        ...(readSteps === undefined ? {} : { readSteps }),
        refused: true,
      });
      expect({ code: result.code, written: result.written }).toEqual({
        code: EXIT.failed,
        written: false,
      });
      expect(result.said).toContain(`${notKnown}${own("20.25.1")}; ${labelled}`);
      expect(result.said).toContain(laterRefusal(UNPACKED, UNPACKED, "/opt/legion/pi-acme"));
    },
    120_000
  );

  test.each([
    [
      "an ADD from the build context into the packages' directory after the COPY lines",
      [...COPIES, "ADD --chown=0:0 patch.tar /opt/legion/"],
      "/opt/legion/pi-acme from the stage legion and the build context by ADD, /opt/legion/pi-legion from the stage legion and the build context by ADD",
    ],
    [
      "an ADD of a URL into a copied package before its COPY",
      ["ADD https://example.test/acme/skills.tar /opt/legion/pi-legion/dist/skills/", ...COPIES],
      "/opt/legion/pi-acme from the stage legion, /opt/legion/pi-legion from the stage legion and https://example.test/acme/skills.tar by ADD",
    ],
  ] as const)(
    "%s leaves the case out, naming where the final stage writes each package from",
    async (name, copyLines, where) => {
      const result = await copied(`added-${name.replaceAll(" ", "-")}`, {
        version: "20.25.1",
        copyLines,
        refused: true,
      });
      expect({ code: result.code, written: result.written }).toEqual({
        code: EXIT.failed,
        written: false,
      });
      expect(result.said).toContain(`${notKnown}${own("20.25.1")}; ${labelled}`);
      expect(result.said).toContain(
        `${copying()}the final stage does not copy each of ${packages} from one stage: ${where}${rule}`
      );
    },
    120_000
  );

  test("an image whose final stage is FROM the worker image, copies none of the profile's packages and ADDs into one leaves the case out, naming the ADD: the built-on step does not decide, and the base is not read", async () => {
    const result = await copied("built-on-and-added", {
      version: "20.25.1",
      final: `\${LEGION_WORKER}`,
      copyLines: [
        ...COPIES.filter((line) => !line.includes("/opt/legion/pi-")),
        "ADD https://example.test/acme/pi-acme.tar /opt/legion/pi-acme",
      ],
      refused: true,
    });
    expect({ code: result.code, written: result.written, skills: result.skills }).toEqual({
      code: EXIT.failed,
      written: false,
      skills: [],
    });
    expect(result.registryReads).not.toContain(WORKER);
    expect(result.said).toContain(`${notKnown}${own("20.25.1")}; ${labelled}`);
    expect(result.said).toContain(
      `${copying(`${WORKER} (the default of the ARG LEGION_WORKER)`)}the final stage does not copy each of ${packages} from one stage: /opt/legion/pi-acme from https://example.test/acme/pi-acme.tar by ADD, /opt/legion/pi-legion not at all${rule}`
    );
  }, 120_000);
});

// Catches: a SKILL.md read GitHub refused for its primary rate limit ("API rate limit exceeded
// for installation ID ...", no transient reason) left out of the profile as a fault, in silence:
// one rebuild went from 40 skills to 19 while the limit was spent. And a wait until the reset
// `gh api rate_limit` gives, which names no owner, so the pod's gh answers it from another
// installation than the one that refused the read: where that reset comes sooner, every wait ends
// before the read can be sent, and the profile fails.
describe("build-cases --reprofile of a case whose SKILL.md reads GitHub refuses for its rate limit", () => {
  const PRIMARY = "gh: API rate limit exceeded for installation ID 12345. (HTTP 403)";
  /** The blob of beta's SKILL.md, which the good case's profile installs from acme/skills. */
  const BETA_BLOB = blobId(REPOSITORIES["acme/skills"]?.["skills/beta/SKILL.md"] ?? "");
  type Resets = { readonly core: number; readonly graphql: number };
  /** The answer of acme's installation to a read of its reset (`gh api -i`), as GitHub gives it:
   *  the reset `resets` gives its limit (seconds since the epoch), in the X-RateLimit-Reset
   *  header and, for a GraphQL query, in its `rateLimit.resetAt`. A read routed by another owner
   *  is not found. */
  const resetAnswer = (args: readonly string[], resets: Resets): GhResult => {
    const graphql = args[2] === "graphql";
    const acme = graphql
      ? args.includes("owner=acme") && args.includes("name=skills")
      : args.at(-1) === "repos/acme/skills";
    if (!acme) return { code: 1, stdout: "", stderr: "gh: Not Found (HTTP 404)\n" };
    const reset = graphql ? resets.graphql : resets.core;
    const body = graphql
      ? { data: { rateLimit: { resetAt: new Date(reset * 1000).toISOString() } } }
      : {};
    return {
      code: 0,
      stdout: `HTTP/2.0 200 OK\r\nX-Ratelimit-Reset: ${reset}\r\n\r\n${JSON.stringify(body)}`,
      stderr: "",
    };
  };
  /** The good case built again: each gh call that reads beta's SKILL.md (a GraphQL batch that
   *  holds its blob, or the blob alone) refused for the primary rate limit while `refusing` says
   *  so of the waits made so far; a read of acme's reset answering `resets` (`resetAnswer`), and
   *  `gh api rate_limit`, which names no owner, answering the sooner resets `elsewhere` of another
   *  installation; each wait kept in `waits`, and each read of a reset in `resetReads`. */
  const reprofiled = async (
    name: string,
    refusing: (waits: readonly number[]) => boolean,
    resets: Resets,
    elsewhere: Resets
  ) => {
    const good = await profileCase("good");
    const dir = path.join(scratch, `rate-limited-${name}`);
    mkdirSync(dir, { recursive: true });
    const from = path.join(dir, "old.jsonl");
    writeFileSync(from, `${JSON.stringify(good)}\n`);
    const fresh = path.join(dir, "reprofiled.jsonl");
    const lines: string[] = [];
    const waits: number[] = [];
    const resetReads: string[][] = [];
    const standin = io(lines);
    const limits = JSON.stringify({
      resources: { core: { reset: elsewhere.core }, graphql: { reset: elsewhere.graphql } },
    });
    const code = await main(
      [
        ...["build-cases", "--config", config, "--old", old, "--copies", copiesDir],
        ...["--count", countDir, "--reprofile", from, "--out", fresh],
      ],
      {
        ...standin,
        gh: async (args) => {
          if (args.includes("rate_limit")) {
            ghCalls.push([...args]);
            return { code: 0, stdout: limits, stderr: "" };
          }
          if (args[1] === "-i") {
            ghCalls.push([...args]);
            resetReads.push([...args]);
            return resetAnswer(args, resets);
          }
          if (refusing(waits) && args.some((arg) => arg.includes(BETA_BLOB))) {
            ghCalls.push([...args]);
            return { code: 1, stdout: "", stderr: `${PRIMARY}\n` };
          }
          return standin.gh(args);
        },
        sleep: async (ms) => void waits.push(ms),
      }
    );
    return {
      good,
      code,
      lines,
      waits,
      resetReads,
      cases: existsSync(fresh) ? loadCases(fresh, old) : [],
      summary: JSON.parse(readFileSync(`${fresh}.summary.json`, "utf8")) as {
        casesWritten: number;
        leftOut: { id: string; why: string }[];
      },
    };
  };
  /** The wait until `reset` (seconds since the epoch) and a second, for a read made between the
   *  times `before` and `after` (milliseconds since the epoch): its least and its most. */
  const untilReset = (reset: number, before: number, after: number) => [
    reset * 1000 + 1_000 - after,
    reset * 1000 + 1_000 - before,
  ];
  const now = () => Math.floor(Date.now() / 1000);

  test("a SKILL.md batch refused until the reset of acme's GraphQL limit, read with a rateLimit query routed by acme's owner and name fields, waits until then and not until the sooner reset `gh api rate_limit` gives, is sent again, and the case is written with the skill in its profile and no fault of its SKILL.md in its evidence", async () => {
    const resets = { core: now() + 600, graphql: now() + 90 };
    const before = Date.now();
    const built = await reprofiled("once", (waits) => waits.length === 0, resets, {
      core: now() + 60,
      graphql: now() + 30,
    });
    const after = Date.now();
    expect(built.code).toBe(EXIT.ok);
    expect(built.cases.map((c) => c.id)).toEqual([built.good.id]);
    const [c] = built.cases;
    expect(c?.profile.skills.filter((s) => s.repo === "acme/skills").map((s) => s.name)).toContain(
      "beta"
    );
    // A read taken as done would leave its SKILL.md out as a fault ("SKILL.md read failed").
    expect(
      (c?.evidence ?? []).filter((each) =>
        each.item.startsWith("left out: acme/skills skills/beta/")
      )
    ).toEqual([]);
    expect(built.waits).toHaveLength(1);
    const [least = 0, most = 0] = untilReset(resets.graphql, before, after);
    expect(built.waits[0]).toBeGreaterThanOrEqual(least);
    expect(built.waits[0]).toBeLessThanOrEqual(most);
    expect(
      built.lines.filter((line) =>
        line.startsWith(
          `build-cases: a gh call was refused for its rate limit (${PRIMARY}); waiting `
        )
      )
    ).toHaveLength(1);
    expect(built.resetReads.map((args) => args.slice(0, 7))).toEqual([
      ["api", "-i", "graphql", "-f", "owner=acme", "-f", "name=skills"],
    ]);
    expect(
      built.lines.filter((line) =>
        /^build-cases: acme's GraphQL rate limit resets at \S+; waiting \d+ s, until \S+$/.test(
          line
        )
      )
    ).toHaveLength(1);
  }, 60_000);

  test("a SKILL.md read still refused after MAX_RATE_RETRIES waits, as a batch and alone, fails the command (exit 1) as any read that fails for a transient reason on every try does: the case is left out of the re-profile, naming the read and the limit, and no profile that lacks the skill is written", async () => {
    const resets = { core: now() + 600, graphql: now() + 90 };
    const before = Date.now();
    const built = await reprofiled("always", () => true, resets, {
      core: now() + 60,
      graphql: now() + 30,
    });
    const after = Date.now();
    expect(built.code).toBe(EXIT.failed);
    expect(built.lines).toContain(`build-cases: failed: 1 cases left out: ${built.good.id}`);
    expect(built.cases).toEqual([]);
    expect(built.summary.casesWritten).toBe(0);
    expect(built.summary.leftOut).toHaveLength(1);
    const [left] = built.summary.leftOut;
    expect(left?.id).toBe(built.good.id);
    expect(left?.why).toStartWith(
      `a GitHub read the profile needs failed: gh api repos/acme/skills/git/blobs/${BETA_BLOB} failed: ${MAX_RATE_RETRIES + 1} tries: try 1: ${PRIMARY}`
    );
    // The batch's waits run to the reset of acme's GraphQL limit, the blob's to its REST limit's,
    // each read from acme's installation.
    expect(built.waits).toHaveLength(2 * MAX_RATE_RETRIES);
    const [graphqlLeast = 0, graphqlMost = 0] = untilReset(resets.graphql, before, after);
    const [coreLeast = 0, coreMost = 0] = untilReset(resets.core, before, after);
    for (const [n, wait] of built.waits.entries()) {
      expect(wait).toBeGreaterThanOrEqual(n < MAX_RATE_RETRIES ? graphqlLeast : coreLeast);
      expect(wait).toBeLessThanOrEqual(n < MAX_RATE_RETRIES ? graphqlMost : coreMost);
    }
    expect(built.resetReads.map((args) => args.slice(0, 7).join(" "))).toEqual([
      ...Array<string>(MAX_RATE_RETRIES).fill("api -i graphql -f owner=acme -f name=skills"),
      ...Array<string>(MAX_RATE_RETRIES).fill("api -i --method GET repos/acme/skills"),
    ]);
  }, 60_000);
});

// Catches: one transport error on a GitHub read leaving a case out of a re-profile, as a commit
// read's TLS handshake timeout did on 2026-11-21, where copies.ts sends the read again; and a name
// that did not resolve, which context.ts's reads sent again before the rule was shared.
test.each([
  ["a TLS handshake timeout", "net/http: TLS handshake timeout", "TLS handshake timeout"],
  ["no such host", "dial tcp: lookup api.github.com: no such host", "no such host"],
])(
  "build-cases --reprofile whose commit read fails once with %s sends it again 5 s later and writes the case, and its summary counts the retry",
  async (_, error, reason) => {
    const [c] = await profileCases();
    const dir = path.join(scratch, `transient-commit-${reason.replaceAll(" ", "-")}`);
    const from = path.join(dir, "old.jsonl");
    mkdirSync(dir, { recursive: true });
    writeFileSync(from, `${JSON.stringify(c)}\n`);
    const fresh = path.join(dir, "reprofiled.jsonl");
    const lines: string[] = [];
    const waits: number[] = [];
    const standin = io(lines);
    let failed: string | undefined;
    const flaky: BuildIo = {
      ...standin,
      // The first commit read fails with `error`; every other call goes to the stand-in.
      gh: async (args) => {
        const route = args.find((arg) => arg.startsWith("repos/")) ?? "";
        if (failed !== undefined || !route.includes("/commits?")) return standin.gh(args);
        ghCalls.push([...args]);
        failed = `Get "https://api.github.com/${route}": ${error}`;
        return { code: 1, stdout: "", stderr: `${failed}\n` };
      },
      sleep: async (ms) => void waits.push(ms),
    };
    const code = await main(
      [
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
        fresh,
      ],
      flaky
    );
    expect(lines.join("")).not.toContain("left out");
    expect(code).toBe(EXIT.ok);
    expect(loadCases(fresh, old).map((each) => each.id)).toEqual([c?.id]);
    expect(waits).toEqual([5_000]);
    expect(lines).toContain(
      `build-cases: a gh call failed (${failed}); sending it again in 5 s, retry 1 of 4`
    );
    expect(JSON.parse(readFileSync(`${fresh}.summary.json`, "utf8"))).toMatchObject({
      casesWritten: 1,
      leftOut: [],
      githubRetries: { [reason]: 1 },
    });
  },
  120_000
);

/** The tasks file a build-tasks run with --profiles-out wrote on the suite's store (t1 from the
 *  session /sessions/task-4.jsonl, F1 as the good session has it, and t2 from
 *  /sessions/task-3.jsonl, F1 as the openai session has it; eight written tasks in the legion
 *  session's context), built once. */
async function profiledTasks(): Promise<string> {
  const out = path.join(scratch, "reprofile-from", "tasks.jsonl");
  if (!existsSync(out)) {
    const dir = path.join(scratch, "reprofile-from", "profiles");
    expect(await main(tasksArgs(out, ...profileFlags(dir)), io([]))).toBe(EXIT.ok);
  }
  return out;
}

/** The arguments of a build-tasks --reprofile run of `from` into `dir`, its configuration
 *  `configFile`. */
const reprofileArgs = (configFile: string, from: string, dir: string, ...extra: string[]) => [
  "build-tasks",
  "--config",
  configFile,
  "--copies",
  copiesDir,
  "--count",
  countDir,
  "--reprofile",
  from,
  "--profiles-out",
  dir,
  ...extra,
];

/** One evidence item. */
interface Item {
  item: string;
  source: string;
}

/** What build-tasks --reprofile records of one task. */
interface Reprofiled {
  task: string;
  from: string;
  was: string | null;
  profile: string | null;
  outcome: string;
  differs: { added: Item[]; removed: Item[] };
}

/** The summary build-tasks --reprofile wrote beside the tasks file of `dir`. */
const reprofileSummary = (dir: string) =>
  JSON.parse(readFileSync(path.join(dir, "tasks.jsonl.summary.json"), "utf8")) as {
    command: string;
    counts: Record<string, number>;
    profiles: { contextSession: Record<string, unknown> | null; tasks: Reprofiled[] };
  };

test("build-tasks --reprofile builds each task's profile again as --profiles-out builds it, with no reader call: on the store the tasks were built from, each is byte for byte the old one and says unchanged, and the new tasks file names the new profiles", async () => {
  const from = await profiledTasks();
  const dir = path.join(scratch, "reprofile-same", "profiles");
  const requests = recorder.requests.length;
  const logged = psqlLogLength();
  const lines: string[] = [];
  expect(await main(reprofileArgs(config, from, dir), io(lines))).toBe(EXIT.ok);
  expect(recorder.requests.length).toBe(requests);
  const tasks = tasksOf(from);
  expect(tasks.filter((task) => task.source === "session")).toHaveLength(2);
  expect(tasksOf(path.join(dir, "tasks.jsonl"))).toEqual(
    tasks.map((task) => ({ ...task, profile: path.resolve(dir, `${task.id}.json`) }))
  );
  for (const task of tasks) {
    expect(readFileSync(path.resolve(dir, `${task.id}.json`), "utf8")).toBe(
      readFileSync(String(task.profile), "utf8")
    );
  }
  const summary = reprofileSummary(dir);
  expect(summary.command).toBe("build-tasks --reprofile");
  expect(
    summary.profiles.tasks.map((each) => [each.task, each.from, each.outcome, each.differs])
  ).toEqual(
    tasks.map((task) => [
      String(task.id),
      task.source === "session" ? "session" : "context-session",
      "unchanged",
      { added: [], removed: [] },
    ])
  );
  expect(summary.counts).toEqual({ changed: 0, unchanged: tasks.length, kept: 0, leftOut: 0 });
  expect(summary.profiles.contextSession).toMatchObject({
    store: "store_b",
    hash: fileHash(sessionPath("legion")),
  });
  // The session tasks' paths by their path hashes, one query of their store; the subagents of
  // each session task, one query a session, read as its profile is rebuilt, since their listing
  // decides its pools; the context session the old summary names by its file hash, and its
  // subagents; and one pull of each of the three sessions.
  expect(
    psqlLines(logged)
      .filter((line) => line.kind !== "pull")
      .map((line) => `${line.db} ${line.kind}`)
      .sort()
  ).toEqual([
    "store_a paths",
    "store_a subagents",
    "store_a subagents",
    "store_b sessions",
    "store_b subagents",
  ]);
  expect(transcriptPulls(logged)).toBe(3);
  expect(existsSync(`${dir}.spool`)).toBe(false);

  // Refused before any read: a flag of the search or the reader, a directory that holds files
  // already, and a tasks file with no summary.
  const other = path.join(scratch, "reprofile-same", "other");
  const bare = path.join(scratch, "reprofile-same", "bare.jsonl");
  writeFileSync(bare, readFileSync(from));
  const refusedAt = psqlLogLength();
  const rows: [string[], string][] = [
    [[...reprofileArgs(config, from, other), "--key-file", keyFile], "so it takes no --key-file"],
    [[...reprofileArgs(config, from, other), "--store", "store_a"], "so it takes no --store"],
    [reprofileArgs(config, from, dir), `--profiles-out ${dir} holds files already`],
    [reprofileArgs(config, bare, other), `${bare}.summary.json is not a file`],
  ];
  for (const [args, why] of rows) {
    const refused: string[] = [];
    expect({ why, code: await main(args, io(refused)) }).toEqual({ why, code: EXIT.refused });
    expect(refused.join("\n")).toContain(why);
  }
  expect(psqlLogLength()).toBe(refusedAt);
  expect(existsSync(other)).toBe(false);
}, 120_000);

test("build-tasks --reprofile of a task whose session is now in a checkout of acme/proj gives it a profile that names acme/proj at its commit and lists its project skills and root CLAUDE.md, says changed and lists the evidence items that differ; the other tasks are unchanged", async () => {
  const from = await profiledTasks();
  const task4 = `${DEV_SESSIONS}/task-4.jsonl`;
  const moved = changedStore("reprofile-moved", (files) => {
    (files.store_a ?? {})[task4] = inDirectory(sessionText("good"), "/home/dev/src/proj");
  });
  const dir = path.join(scratch, "reprofile-moved", "profiles");
  const lines: string[] = [];
  expect(await main(reprofileArgs(moved, from, dir), io(lines))).toBe(EXIT.ok);
  const t1 = tasksOf(from).find((task) => task.id === "t1");
  expect(t1?.pathHash).toBe(pathHash(task4));
  const before = JSON.parse(readFileSync(String(t1?.profile), "utf8"));
  expect(JSON.parse(readFileSync(path.resolve(dir, "t1.json"), "utf8"))).toEqual({
    ...before,
    skills: [
      { repo: "acme/skills", commit: SHA, path: "skills/older", name: "older", namespace: "agent" },
      { repo: "acme/skills", commit: SHA, path: "skills/beta", name: "beta" },
      {
        repo: "acme/proj",
        commit: SHA,
        path: ".claude/skills/beta",
        name: "beta",
        namespace: "claude",
      },
      { repo: "acme/proj", commit: SHA, path: ".claude/skills/kappa", name: "kappa" },
      { repo: "acme/proj", commit: SHA, path: ".omp/skills/lambda", name: "lambda" },
      { repo: "acme/proj", commit: SHA, path: ".omp/skills/older", name: "older" },
    ],
    contextFiles: [
      { repo: "acme/skills", commit: SHA, path: ".claude/CLAUDE.md", user: true },
      { repo: "acme/proj", commit: SHA, path: "CLAUDE.md" },
    ],
    sessionRepository: { repo: "acme/proj", commit: SHA },
  });
  const summary = reprofileSummary(dir);
  const record = summary.profiles.tasks.find((each) => each.task === "t1");
  expect(record?.outcome).toBe("changed");
  expect(record?.differs.added.map((each) => each.item)).toEqual(
    expect.arrayContaining([
      "session repository acme/proj",
      "skill kappa (acme/proj .claude/skills/kappa)",
      "skill older (acme/proj .omp/skills/older)",
      "context file acme/proj CLAUDE.md",
    ])
  );
  expect(record?.differs.removed.map((each) => each.item)).toEqual(
    expect.arrayContaining(["no session repository", "skill older (acme/skills skills/older)"])
  );
  expect(
    summary.profiles.tasks.filter((each) => each.task !== "t1").map((each) => each.outcome)
  ).toEqual(Array(9).fill("unchanged"));
  expect(summary.counts).toMatchObject({ changed: 1, unchanged: 9 });
  expect(lines.join("")).toContain("build-tasks: t1: profile changed");
}, 120_000);

// Catches (a dev-box profile-build fault found in a load task's rebuilt context): a session in a
// workspace of a repository whose tree does not hold the workspace's name, rebuilt from every read
// of its branch with no session repository and so none of the repository's project skills.
test("build-tasks --reprofile of a task whose session is now directly inside a directory named as acme/proj whose tree does not hold it, where it read the root's CLAUDE.md, gives it acme/proj's profile at its root, as a workspace of acme/proj", async () => {
  const from = await profiledTasks();
  const lane = `${PROJ_DIR}/lane-3`;
  const moved = changedStore("reprofile-workspace", (files) => {
    (files.store_a ?? {})[`${DEV_SESSIONS}/task-4.jsonl`] = readingIn(lane, `${lane}/CLAUDE.md`);
  });
  const dir = path.join(scratch, "reprofile-workspace", "profiles");
  expect(await main(reprofileArgs(moved, from, dir), io([]))).toBe(EXIT.ok);
  const t1 = tasksOf(from).find((task) => task.id === "t1");
  const before = JSON.parse(readFileSync(String(t1?.profile), "utf8"));
  expect(JSON.parse(readFileSync(path.resolve(dir, "t1.json"), "utf8"))).toEqual({
    ...before,
    skills: [
      { repo: "acme/skills", commit: SHA, path: "skills/older", name: "older", namespace: "agent" },
      { repo: "acme/skills", commit: SHA, path: "skills/beta", name: "beta" },
      {
        repo: "acme/proj",
        commit: SHA,
        path: ".claude/skills/beta",
        name: "beta",
        namespace: "claude",
      },
      { repo: "acme/proj", commit: SHA, path: ".claude/skills/kappa", name: "kappa" },
      { repo: "acme/proj", commit: SHA, path: ".omp/skills/lambda", name: "lambda" },
      { repo: "acme/proj", commit: SHA, path: ".omp/skills/older", name: "older" },
    ],
    contextFiles: [
      { repo: "acme/skills", commit: SHA, path: ".claude/CLAUDE.md", user: true },
      { repo: "acme/proj", commit: SHA, path: "CLAUDE.md" },
    ],
    sessionRepository: { repo: "acme/proj", commit: SHA },
  });
  const record = reprofileSummary(dir).profiles.tasks.find((each) => each.task === "t1");
  expect(
    record?.differs.added.find((each) => each.item === "session repository acme/proj")?.source
  ).toBe(
    `${CHECKOUT_RULE}. The working directory ${lane} is under ${PROJ_DIR}, named as acme/proj, whose tree at ${SHA} holds no lane-3; it is a workspace of acme/proj named lane-3, a checkout of it at its root: message 2 reads ${lane}/CLAUDE.md, and the tree holds CLAUDE.md`
  );
}, 60_000);

test("build-tasks --reprofile gives each written task the profile of the --context-session it is given, in place of the session the tasks file's summary names, and lists the evidence items that differ", async () => {
  const from = await profiledTasks();
  const dir = path.join(scratch, "reprofile-context", "profiles");
  const task4 = fileHash(`${DEV_SESSIONS}/task-4.jsonl`);
  expect(
    await main(reprofileArgs(config, from, dir, "--context-session", `store_a:${task4}`), io([]))
  ).toBe(EXIT.ok);
  const tasks = tasksOf(from);
  const written = tasks.filter((task) => task.source === "written");
  expect(written).toHaveLength(8);
  // task-4's session is F1 as the good session has it: its profile is t1's, not the legion one.
  const good = readFileSync(path.resolve(dir, "t1.json"), "utf8");
  const summary = reprofileSummary(dir);
  for (const task of written) {
    expect(readFileSync(String(task.profile), "utf8")).not.toBe(good);
    expect(readFileSync(path.resolve(dir, `${task.id}.json`), "utf8")).toBe(good);
    const record = summary.profiles.tasks.find((each) => each.task === task.id);
    expect(record?.from).toBe("context-session");
    expect(record?.outcome).toBe("changed");
    expect(record?.differs.added.map((each) => each.item)).toContain("no role");
    expect(record?.differs.removed.map((each) => each.item)).toContain("role tester");
  }
  expect(summary.profiles.contextSession).toMatchObject({
    store: "store_a",
    hash: task4,
    pathHash: pathHash(`${DEV_SESSIONS}/task-4.jsonl`),
    context: "devbox",
  });
  expect(summary.counts).toMatchObject({ changed: 8, unchanged: 2 });
}, 120_000);

/** The manifests.tsv rows that make acme/home the dev box's repository: it holds an installer
 *  script and Claude Code's settings. */
const HOME_MANIFESTS = [
  ["acme/home", "installers/omp.sh", "installer-script", "tree", 4, "links skills", ""],
  ["acme/home", ".claude/settings.json", "claude-settings", "tree", 0, "none", ""],
];

/** The manifests.tsv rows that make acme/dots the dev box's repository, as HOME_MANIFESTS does
 *  acme/home. */
const DOTS_MANIFESTS = [
  ["acme/dots", "installers/omp.sh", "installer-script", "tree", 1, "clones", ""],
  ["acme/dots", ".claude/settings.json", "claude-settings", "tree", 0, "none", ""],
];

/** A copy of the suite's copies output in `<setupDir>/<name>`, with `rows` added at the end of
 *  each file they name. */
function copiesWith(
  name: string,
  rows: Readonly<Record<string, readonly (readonly (string | number)[])[]>>
): string {
  const dir = path.join(setupDir, name);
  mkdirSync(dir, { recursive: true });
  for (const file of [
    "prefix-table.tsv",
    "former-stages.tsv",
    "installers.tsv",
    "manifests.tsv",
    "loaders.tsv",
    "skill-copies.tsv",
    "repositories.tsv",
  ]) {
    const added = (rows[file] ?? []).map((row) => `${row.join("\t")}\n`).join("");
    writeFileSync(
      path.join(dir, file),
      `${readFileSync(path.join(copiesDir, file), "utf8")}${added}`
    );
  }
  return dir;
}

/** The rows of a copies output where acme/home is the dev box's repository and, by the copies
 *  output's time, its installer installs fresh-one from acme/fresh, whose first commit
 *  (FRESH_MADE) came after every session's start. */
const FRESH_TARGET = {
  "manifests.tsv": HOME_MANIFESTS,
  "installers.tsv": [
    [
      "acme/home",
      "installers/omp.sh",
      "installer-script",
      "installs",
      "gh-skill:fresh-one",
      "omp",
      "fresh-one",
      "acme/fresh",
      "skills/fresh-one",
      "",
    ],
  ],
  "skill-copies.tsv": [["acme/fresh", "skills/fresh-one", "fresh-one", 1, "no", "no", "", "", ""]],
  "repositories.tsv": [["acme/fresh", "main", SHA, "no", "no", "", 1, "whole", 1, ""]],
};

/** The evidence item that leaves acme/fresh out of a profile of a session built from F1. */
const FRESH_LEFT_OUT = {
  item: `left out: acme/fresh, which has no commit at or before the session's start ${F1_START}`,
  source: `main, the ref repositories.tsv names, has no commit at or before the session's start; gh api repos/acme/fresh/commits?sha=main&until=${F1_START} gives no commit of main (from gh api repos/acme/fresh): GitHub holds no commit of acme/fresh then, so nothing of it was installed then, and the profile installs nothing of it`,
};

// Catches: a repository the dev box's installers install by the copies output's time, whose first
// commit came after the session's start, failing the whole build: GitHub gives no commit of it at
// or before that start.
test("a dev-box session that started before the first commit of a repository its dev box's installers install later gets its profile: that repository is left out with the evidence, and no profile lists a skill of it", async () => {
  const copies = copiesWith("copies-fresh", FRESH_TARGET);
  const from = await profiledTasks();
  const dir = path.join(scratch, "fresh-target", "profiles");
  const context = `store_a:${fileHash(`${DEV_SESSIONS}/task-4.jsonl`)}`;
  const said: string[] = [];
  const code = await main(
    reprofileArgs(config, from, dir, "--context-session", context).map((arg) =>
      arg === copiesDir ? copies : arg
    ),
    io(said)
  );
  expect({ code, said: code === EXIT.ok ? "" : said.join("\n") }).toEqual({
    code: EXIT.ok,
    said: "",
  });
  const evidence = (reprofileSummary(dir).profiles.contextSession?.evidence ?? []) as Item[];
  expect(evidence).toContainEqual(FRESH_LEFT_OUT);
  expect(evidence.filter((each) => each.item.includes("acme/fresh"))).toEqual([FRESH_LEFT_OUT]);
  // Each session task's profile is built the same way.
  for (const record of reprofileSummary(dir).profiles.tasks) {
    if (record.from === "session") expect(record.differs.added).toContainEqual(FRESH_LEFT_OUT);
  }
  // No profile written holds a skill of acme/fresh; each holds acme/home's linked skills.
  for (const task of tasksOf(path.join(dir, "tasks.jsonl"))) {
    const profile = JSON.parse(readFileSync(String(task.profile), "utf8"));
    const repos = profile.skills.map((skill: { repo: string }) => skill.repo);
    expect(repos).not.toContain("acme/fresh");
    expect(repos).toContain("acme/home");
  }
}, 120_000);

// Catches: the same repository leaving each case of build-cases --reprofile out, as a GitHub read
// the profile needs that failed, where only that repository is left out of the profile.
test("build-cases --reprofile of a dev-box case whose session started before the first commit of a repository its dev box's installers install later writes the case: that repository is left out with the evidence, and the profile holds no skill of it", async () => {
  const good = await profileCase("good");
  const dir = path.join(scratch, "fresh-target-cases");
  const from = path.join(dir, "old.jsonl");
  mkdirSync(dir, { recursive: true });
  writeFileSync(from, `${JSON.stringify(good)}\n`);
  const fresh = path.join(dir, "reprofiled.jsonl");
  const lines: string[] = [];
  const code = await main(
    [
      "build-cases",
      "--config",
      config,
      "--old",
      old,
      "--copies",
      copiesWith("copies-fresh", FRESH_TARGET),
      "--count",
      countDir,
      "--reprofile",
      from,
      "--out",
      fresh,
    ],
    io(lines)
  );
  expect({ code, said: code === EXIT.ok ? "" : lines.join("\n") }).toEqual({
    code: EXIT.ok,
    said: "",
  });
  const [c] = loadCases(fresh, old);
  expect(c?.id).toBe(good.id);
  expect(c?.evidence).toContainEqual(FRESH_LEFT_OUT);
  const repos = c?.profile.skills.map((skill) => skill.repo);
  expect(repos).not.toContain("acme/fresh");
  expect(repos).toContain("acme/home");
}, 120_000);

// Catches: a session repository with no commit at the session's start failing the whole build, or
// a later commit of it read in its place.
test("a session whose working directory is a checkout of a repository with no commit at its start gets its profile without that repository's project skills and context files; the evidence says why, and the listing measure counts the missing project skill", async () => {
  const copies = copiesWith("copies-newproj", {
    "repositories.tsv": [["acme/newproj", "main", SHA, "no", "no", "", 1, "recursive", 1, ""]],
  });
  // The good session in a checkout of acme/newproj, whose first commit (FRESH_MADE) came after
  // the session's start, and a subagent of it whose recorded listing holds acme/newproj's nova.
  const session = `${DEV_SESSIONS}/newproj.jsonl`;
  const store = changedStore("newproj-store", (files) => {
    const storeA = files.store_a ?? {};
    storeA[session] = inDirectory(sessionText("good"), "/home/dev/src/newproj");
    storeA[`${DEV_SESSIONS}/newproj/Helper.jsonl`] = novaSubagent();
  });
  const from = await profiledTasks();
  const dir = path.join(scratch, "newproj-context", "profiles");
  const callsBefore = ghCalls.length;
  const said: string[] = [];
  const code = await main(
    reprofileArgs(store, from, dir, "--context-session", `store_a:${fileHash(session)}`).map(
      (arg) => (arg === copiesDir ? copies : arg)
    ),
    io(said)
  );
  expect({ code, said: code === EXIT.ok ? "" : said.join("\n") }).toEqual({
    code: EXIT.ok,
    said: "",
  });
  const evidence = (reprofileSummary(dir).profiles.contextSession?.evidence ?? []) as Item[];
  const items = evidence.map((each) => each.item);
  expect(items).toContain("session repository acme/newproj");
  expect(evidence).toContainEqual({
    item: `left out: acme/newproj, which has no commit at or before the session's start ${F1_START}`,
    source: `main, the ref repositories.tsv names, has no commit at or before the session's start; gh api repos/acme/newproj/commits?sha=main&until=${F1_START} gives no commit of main (from gh api repos/acme/newproj): GitHub holds no commit of acme/newproj then, so its project skills and context files are not read, and the profile names no session repository`,
  });
  const written = tasksOf(from).find((task) => task.source === "written");
  const profile = JSON.parse(readFileSync(path.resolve(dir, `${written?.id}.json`), "utf8"));
  expect(profile.sessionRepository).toBeUndefined();
  expect(profile.workingDirectory).toBeUndefined();
  expect(
    [...profile.skills, ...profile.contextFiles].filter(
      (each: { repo: string }) => each.repo === "acme/newproj"
    )
  ).toEqual([]);
  // No later commit of acme/newproj is read in its place: only its commit at the start is asked
  // for, and no tree or file of it.
  const asked = ghCalls
    .slice(callsBefore)
    .filter((args) =>
      args.some((arg) => arg.startsWith("repos/acme/newproj/") || arg === "name=newproj")
    );
  expect(asked.length).toBeGreaterThan(0);
  expect(
    asked.every((args) => args.some((arg) => arg.startsWith("repos/acme/newproj/commits?")))
  ).toBe(true);
  // The subagent's recorded listing holds nova, which the profile does not list.
  const measure = ListingMeasure.parse(
    JSON.parse(evidence.find((each) => each.item === LISTING_MEASURE_ITEM)?.source ?? "{}")
  );
  expect(measure.kind).toBe("subagent listing");
  expect(measure.missing).toContain("nova");
}, 120_000);

test("build-tasks --reprofile keeps the old profile of a task whose session its store holds no longer, and of each written task when the context session is gone, each with an evidence item that says so; a store read that fails fails the run", async () => {
  const from = await profiledTasks();
  const task4 = `${DEV_SESSIONS}/task-4.jsonl`;
  const gone = changedStore("reprofile-gone", (files) => {
    delete files.store_a?.[task4];
    delete files.store_b?.[sessionPath("legion")];
  });
  const dir = path.join(scratch, "reprofile-gone", "profiles");
  const lines: string[] = [];
  expect(await main(reprofileArgs(gone, from, dir), io(lines))).toBe(EXIT.ok);
  const tasks = tasksOf(from);
  for (const task of tasks) {
    expect(readFileSync(path.resolve(dir, `${task.id}.json`), "utf8")).toBe(
      readFileSync(String(task.profile), "utf8")
    );
  }
  const summary = reprofileSummary(dir);
  const byTask = new Map(summary.profiles.tasks.map((each) => [each.task, each]));
  // A kept task's evidence says why, and its listing measure is not measurable for that reason,
  // counting the names its old profile lists (a namespaced copy under `<namespace>/<name>`); the
  // measure its old build wrote is the one item it drops.
  const kept = (task: string, gone: Item) => {
    const note = byTask.get(task);
    expect(note?.outcome).toBe("kept");
    expect(note?.differs.added.map((each) => each.item)).toEqual([gone.item, LISTING_MEASURE_ITEM]);
    expect(note?.differs.added[0]).toEqual(gone);
    const measure = ListingMeasure.parse(JSON.parse(note?.differs.added[1]?.source ?? "{}"));
    expect(measure.kind).toBe("not measurable");
    expect(measure.why).toEndWith(gone.source);
    const profile = JSON.parse(
      readFileSync(String(tasks.find((each) => each.id === task)?.profile), "utf8")
    ) as { skills: { name: string; namespace?: string }[] };
    expect(profile.skills.length).toBeGreaterThan(0);
    expect(measure.listed).toBe(new Set(profile.skills.map((each) => listedName(each))).size);
    expect(note?.differs.removed.map((each) => each.item)).toEqual([LISTING_MEASURE_ITEM]);
  };
  kept("t1", {
    item: "no session in the store",
    source: `the store store_a holds no file of the path hash ${pathHash(task4)} now (session-paths.sql), so the task keeps its old profile`,
  });
  expect(byTask.get("t2")?.outcome).toBe("unchanged");
  for (const task of tasks.filter((each) => each.source === "written")) {
    kept(String(task.id), {
      item: "no session in the store",
      source: `the store store_b holds no file of the hash ${fileHash(sessionPath("legion"))} now (sessions.sql), so each written task keeps its old profile`,
    });
  }
  expect(summary.counts).toEqual({ changed: 0, unchanged: 1, kept: 9, leftOut: 0 });

  // A kept profile's namespaced copy is listed under `<namespace>/<name>`, beside the bare copy of
  // its name: t1's old profile given a copy of beta under the namespace agent lists 3 names.
  const namespaced = path.join(scratch, "reprofile-gone-namespaced");
  mkdirSync(namespaced, { recursive: true });
  const t1 = tasks.find((each) => each.id === "t1");
  const t1Profile = JSON.parse(readFileSync(String(t1?.profile), "utf8"));
  const beta = t1Profile.skills.find((each: { name: string }) => each.name === "beta");
  const t1File = path.join(namespaced, "t1.json");
  writeFileSync(
    t1File,
    `${JSON.stringify({ ...t1Profile, skills: [...t1Profile.skills, { ...beta, namespace: "agent" }] }, null, 2)}\n`
  );
  const fromNamespaced = path.join(namespaced, "tasks.jsonl");
  writeFileSync(
    fromNamespaced,
    tasks
      .map((each) => `${JSON.stringify(each.id === "t1" ? { ...each, profile: t1File } : each)}\n`)
      .join("")
  );
  writeFileSync(`${fromNamespaced}.summary.json`, readFileSync(`${from}.summary.json`));
  const namespacedDir = path.join(namespaced, "profiles");
  expect(await main(reprofileArgs(gone, fromNamespaced, namespacedDir), io([]))).toBe(EXIT.ok);
  const t1Note = reprofileSummary(namespacedDir).profiles.tasks.find((each) => each.task === "t1");
  const t1Measure = ListingMeasure.parse(
    JSON.parse(
      t1Note?.differs.added.find((each) => each.item === LISTING_MEASURE_ITEM)?.source ?? "{}"
    )
  );
  expect(t1Measure.listed).toBe(3);

  // A store command that fails fails the run, and no tasks file is written.
  const failing = path.join(setupDir, "reprofile-failing");
  mkdirSync(failing, { recursive: true });
  const command = path.join(failing, "psql");
  writeFileSync(command, "#!/bin/sh\necho 'psql: error: gone' >&2\nexit 3\n");
  chmodSync(command, 0o755);
  const broken = path.join(failing, "skill-cuts.json");
  writeFileSync(
    broken,
    JSON.stringify({ ...JSON.parse(readFileSync(config, "utf8")), psqlCommand: command })
  );
  const failed = path.join(scratch, "reprofile-failing", "profiles");
  const said: string[] = [];
  expect(await main(reprofileArgs(broken, from, failed), io(said))).toBe(EXIT.failed);
  expect(said.join("\n")).toContain("the store command exited 3: psql: error: gone");
  expect(existsSync(path.join(failed, "tasks.jsonl"))).toBe(false);
}, 120_000);

// Catches: a context session whose rebuilt context does not list the skill refused (exit 2) after
// the store read that found it, where exit 2 is a refusal before any store read.
test("build-tasks --reprofile fails (exit 1) on a --context-session whose rebuilt context does not list the skill, naming why, and writes no tasks file", async () => {
  const from = await profiledTasks();
  const unlisted = fileHash(`${DEV_SESSIONS}/task-1.jsonl`);
  const dir = path.join(scratch, "reprofile-unlisted-context", "profiles");
  const said: string[] = [];
  const requests = recorder.requests.length;
  expect(
    await main(
      reprofileArgs(config, from, dir, "--context-session", `store_a:${unlisted}`),
      io(said)
    )
  ).toBe(EXIT.failed);
  expect(said.join("\n")).toContain(
    `the context session store_a:${unlisted}: its rebuilt context (subagent:task) does not list alpha`
  );
  expect(recorder.requests.length).toBe(requests);
  expect(existsSync(path.join(dir, "tasks.jsonl"))).toBe(false);
}, 120_000);

// Catches: a candidate's drop reason taken from the flags given, not from the step that failed.
test("build-tasks drops a candidate whose pull gives no row as pull-failed and one whose transcript does not parse as unparseable, with --model and with --profiles-out", async () => {
  const broken = changedStore("candidate-drops", (files) => {
    const store = files.store_a ?? {};
    store[`${DEV_SESSIONS}/task-1.jsonl`] = `not one entry\n${sessionText("good")}`;
    store[`${DEV_SESSIONS}/task-4.jsonl`] = "";
  });
  const args = (out: string, ...extra: string[]) => [
    "build-tasks",
    ...["--config", broken, "--key-file", keyFile, "--skill", old, "--store", "store_a"],
    ...["--out", out, ...extra],
  ];
  /** Each candidate drop the summary beside `out` records: its path hash and reason. */
  const drops = (out: string) => {
    // The summary build-tasks wrote, read as the file's other tests read one.
    const summary = JSON.parse(readFileSync(`${out}.summary.json`, "utf8")) as {
      candidateDrops: { sessions: { pathHash: string; reason: string }[] };
    };
    return summary.candidateDrops.sessions.map((each) => [each.pathHash, each.reason]);
  };
  const wanted = [
    [pathHash(`${DEV_SESSIONS}/task-1.jsonl`), "unparseable"],
    [pathHash(`${DEV_SESSIONS}/task-4.jsonl`), "pull-failed"],
  ];
  const model = path.join(scratch, "candidate-drops-model", "tasks.jsonl");
  expect(await main(args(model, "--model", "openai/gpt-6.1-sol"), io([]))).toBe(EXIT.ok);
  expect(drops(model)).toEqual(wanted);
  const profiled = path.join(scratch, "candidate-drops-profiles", "tasks.jsonl");
  const profiles = [
    ...["--profiles-out", path.join(scratch, "candidate-drops-profiles", "profiles")],
    ...["--copies", copiesDir, "--count", countDir],
    ...["--context-session", `store_b:${fileHash(sessionPath("legion"))}`],
  ];
  expect(await main(args(profiled, ...profiles), io([]))).toBe(EXIT.ok);
  expect(drops(profiled)).toEqual(wanted);
}, 120_000);

/** The start of the made-up long-lived sessions of the read-time rows: before acme/newproj's first
 *  commit (FRESH_MADE). */
const LONG_START = "2026-11-20T02:50:00.000Z";
/** Their prompt builds' system-prompt-digest lines: the first reply's, one after a reload past
 *  FRESH_MADE, and a later one. */
const LONG_BUILDS = [
  { at: "2026-11-20T02:50:30.000Z", digest: "5e1d000000000001" },
  { at: "2026-11-20T03:10:00.000Z", digest: "5e1d000000000002" },
  { at: "2026-11-20T04:00:00.000Z", digest: "5e1d000000000003" },
];
/** The path of the made-up long-lived session `id`, named as Oh My Pi names a top-level one. */
const longPath = (id: string) =>
  `${DEV_SESSIONS}/newproj/2026-11-20T02-50-00-000Z_0a1b2c3d-0000-4000-8000-0000000000${id}.jsonl`;

/** `text`, a session's transcript, with its header's working directory `cwd` and start `start`,
 *  and with `digests` as its system-prompt-digest lines in place of its own. */
function longSession(
  text: string,
  start: string,
  digests: readonly { at: string; digest: string }[]
): string {
  const lines = text
    .split("\n")
    .filter((line) => line !== "" && !line.includes('"customType":"system-prompt-digest"'))
    .map((line) => JSON.parse(line) as Record<string, unknown>);
  const header = lines.findIndex((line) => line.type === "session");
  lines[header] = { ...lines[header], timestamp: start, cwd: "/home/dev/src/newproj" };
  lines.splice(
    header + 1,
    0,
    ...digests.map(({ at, digest }, n) => ({
      type: "custom",
      customType: "system-prompt-digest",
      data: digest,
      id: `5e1d00${n}0`,
      parentId: null,
      timestamp: at,
    }))
  );
  return lines.map((line) => `${JSON.stringify(line)}\n`).join("");
}

/** The subagent's transcript with acme/newproj's nova added to its recorded listing, after beta. */
function novaSubagent(): string {
  return sessionText("subagent").replace(
    "- beta: Use for the beta greeting.\\n",
    "- beta: Use for the beta greeting.\\n- nova: Use when the project asks for nova.\\n"
  );
}

/** A subagent's transcript that started at `start`, whose recorded listing holds alpha, beta and,
 *  with `nova`, acme/newproj's nova. */
const longSubagent = (start: string, nova: boolean) =>
  longSession(nova ? novaSubagent() : sessionText("subagent"), start, []);

/** The earliest subagent of the long-lived sessions, spawned under their first build, and one
 *  spawned under their second. */
const EARLY_SPAWN = "2026-11-20T02:55:00.000Z";
const LATER_SPAWN = "2026-11-20T03:15:00.000Z";

/** The stand-in store of the read-time rows: session a1 with a subagent under each of its first
 *  two builds, b2 with only the first, and c3 with only the second; each a checkout of
 *  acme/newproj. */
function longStore(): string {
  return changedStore("long-store", (files) => {
    const storeA = files.store_a ?? {};
    for (const id of ["a1", "b2", "c3"]) {
      const file = longPath(id);
      const dir = file.slice(0, -".jsonl".length);
      storeA[file] = longSession(sessionText("good"), LONG_START, LONG_BUILDS);
      if (id !== "c3") storeA[`${dir}/Early.jsonl`] = longSubagent(EARLY_SPAWN, false);
      if (id !== "b2") storeA[`${dir}/Later.jsonl`] = longSubagent(LATER_SPAWN, true);
    }
  });
}

/** The copies output of the read-time rows: the suite's, with acme/newproj listed. */
const longCopies = () =>
  copiesWith("copies-long", {
    "repositories.tsv": [["acme/newproj", "main", SHA, "no", "no", "", 1, "recursive", 1, ""]],
  });

/** The good session's case moved to the long-lived session `id`: its step's message at `step`,
 *  under the prompt whose digest is `digest`. */
async function longCase(id: string, session: string, step: string, digest: string | null) {
  const good = await profileCase("good");
  return {
    ...good,
    id,
    source: { ...good.source, pathHash: pathHash(longPath(session)) },
    sessionStart: LONG_START,
    sessionDigest: digest,
    realStep: { ...good.realStep, timestamp: Date.parse(step) },
  };
}

/** build-cases --reprofile of the read-time rows' cases, run once: their rebuilt cases by id. */
let longCases: Promise<Map<string, Case>> | undefined;
function reprofiledLong(): Promise<Map<string, Case>> {
  longCases ??= (async () => {
    const cases = [
      // Its step after the second line: that build's.
      await longCase("long-second", "a1", "2026-11-20T03:20:00.000Z", LONG_BUILDS[1]?.digest ?? ""),
      // The first reply under the second build, whose line came after its message's time.
      await longCase("long-own", "a1", "2026-11-20T03:05:00.000Z", LONG_BUILDS[1]?.digest ?? ""),
      // Its step before every line.
      await longCase("long-before", "a1", "2026-11-20T02:50:10.000Z", null),
      // Its step after the second line, in a session with no subagent under that build.
      await longCase("long-none", "b2", "2026-11-20T03:20:00.000Z", LONG_BUILDS[1]?.digest ?? ""),
    ];
    const from = path.join(scratch, "long", "old.jsonl");
    mkdirSync(path.dirname(from), { recursive: true });
    writeFileSync(from, cases.map((c) => `${JSON.stringify(c)}\n`).join(""));
    const out = path.join(scratch, "long", "reprofiled.jsonl");
    const lines: string[] = [];
    const args = ["build-cases", "--config", longStore(), "--old", old, "--copies", longCopies()];
    const code = await main(
      [...args, "--count", countDir, "--reprofile", from, "--out", out],
      io(lines)
    );
    expect({ code, said: code === EXIT.ok ? "" : lines.join("") }).toEqual({
      code: EXIT.ok,
      said: "",
    });
    return new Map(loadCases(out, old).map((c) => [c.id, c]));
  })();
  return longCases;
}

/** Case `id`'s evidence, by item. */
const longEvidence = async (id: string) =>
  Object.fromEntries(
    ((await reprofiledLong()).get(id)?.evidence ?? []).map((each) => [each.item, each.source])
  );

/** Case `id`'s listing measure. */
const longMeasure = async (id: string) =>
  ListingMeasure.parse(JSON.parse((await longEvidence(id))[LISTING_MEASURE_ITEM] ?? "{}"));

/** The names of the skills of case `id`'s profile, with their repositories. */
const longSkills = async (id: string) =>
  ((await reprofiledLong()).get(id)?.profile.skills ?? []).map(
    (each) => `${each.name} ${each.repo}`
  );

// Catches: a top-level case of a long-lived session read at the session's start, not at the prompt
// its step ran under, so its profile lacks what the box gained before that build.
test("a top-level case whose step comes after its session's second system-prompt-digest line is read at that line's time, and its listing is measured against the earliest subagent spawned under that build", async () => {
  const [, second, third] = LONG_BUILDS;
  // acme/newproj's first commit came after the session's start and before that build.
  expect(await longSkills("long-second")).toContain("nova acme/newproj");
  expect(await longMeasure("long-second")).toMatchObject({
    kind: "subagent listing",
    subagent: { pathHash: pathHash(`${longPath("a1").slice(0, -6)}/Later.jsonl`) },
  });
  const evidence = await longEvidence("long-second");
  expect(evidence["read times"]).toStartWith(
    `the step's message is at 2026-11-20T03:20:00.000Z, and the last system-prompt-digest line at or before it is at ${second?.at}; so it is the build of line 2 of the session's 3 system-prompt-digest lines, at ${second?.at}, digest ${second?.digest}, and each repository, a pool's too, is read at ${second?.at}: Oh My Pi 18.18.6 writes a system-prompt-digest line`
  );
  expect(evidence[`commit ${SHA} of acme/skills`]).toEndWith(
    `at or before the prompt build its step ran under ${second?.at}: gh api repos/acme/skills/commits?sha=main&until=${second?.at}`
  );
  expect((await longMeasure("long-second")).why).toContain(
    `spawned under the prompt build its profile is read at (from its line, at ${second?.at}, to the next line, at ${third?.at})`
  );
}, 120_000);

// Catches: the first reply under a new build read at the build before it: its message's time comes
// before the line Oh My Pi writes as that reply ends.
test("a top-level case whose step was the first reply under a build, its message before that build's line, is read at that line, which holds the case's digest", async () => {
  const [first, second] = LONG_BUILDS;
  expect(await longSkills("long-own")).toContain("nova acme/newproj");
  const evidence = await longEvidence("long-own");
  expect(evidence["read times"]).toStartWith(
    `the step's message is at 2026-11-20T03:05:00.000Z; the last system-prompt-digest line at or before it, at ${first?.at}, holds ${first?.digest}, not the case's digest ${second?.digest}, and the next, at ${second?.at}, holds it: the step was the first reply under that build; so it is the build of line 2 of the session's 3 system-prompt-digest lines, at ${second?.at}`
  );
}, 120_000);

// Catches: a case whose step precedes every line read at a later build, or measured against a
// subagent of a later build.
test("a top-level case whose step precedes every system-prompt-digest line of its session is read at the session's start, its first build, and measured against a subagent spawned under that build", async () => {
  const [first, second] = LONG_BUILDS;
  const evidence = await longEvidence("long-before");
  expect(evidence["read times"]).toStartWith(
    `the step's message is at 2026-11-20T02:50:10.000Z, before every system-prompt-digest line of the session; so it is the session's first build (its line is the first of the session's 3 system-prompt-digest lines, at ${first?.at}, digest ${first?.digest}), and each repository, a pool's too, is read at the session's start ${LONG_START}`
  );
  // acme/newproj had no commit at the session's start.
  expect(
    evidence[
      `left out: acme/newproj, which has no commit at or before the session's start ${LONG_START}`
    ]
  ).toBeDefined();
  expect(await longSkills("long-before")).not.toContain("nova acme/newproj");
  expect(await longMeasure("long-before")).toMatchObject({
    kind: "subagent listing",
    subagent: { pathHash: pathHash(`${longPath("a1").slice(0, -6)}/Early.jsonl`) },
  });
  expect((await longMeasure("long-before")).why).toContain(
    `spawned under the session's first prompt build, the one its profile is read at (before the second line, at ${second?.at})`
  );
}, 120_000);

// Catches: a top-level session measured against a subagent of another build when none was spawned
// under the build its profile is read at.
test("a top-level case whose session spawned no subagent with a recorded listing under the build its profile is read at is not measurable, and says why; a subagent of an earlier build is not used", async () => {
  const measure = await longMeasure("long-none");
  expect(measure).toMatchObject({ kind: "not measurable", recorded: null, subagent: null });
  expect(measure.why).toEndWith(
    `the store holds no subagent of it whose recorded prompt holds a skill listing spawned under the prompt build its profile is read at (from its line, at ${LONG_BUILDS[1]?.at}, to the next line, at ${LONG_BUILDS[2]?.at}) (subagents.sql: 1 of its 1 subagents record one, 0 of them spawned then), and the listing of a subagent spawned under another build is never used`
  );
}, 120_000);

// Catches: a context session read at its start while its listing measure takes a subagent spawned
// under a later build, so the measure compares two times.
test("a context session is read at the build its measured subagent was spawned under, the earliest whose recorded prompt holds a listing, and its measure compares that build with itself", async () => {
  const from = await profiledTasks();
  const dir = path.join(scratch, "long-context", "profiles");
  const said: string[] = [];
  const code = await main(
    reprofileArgs(
      longStore(),
      from,
      dir,
      "--context-session",
      `store_a:${fileHash(longPath("c3"))}`
    ).map((arg) => (arg === copiesDir ? longCopies() : arg)),
    io(said)
  );
  expect({ code, said: code === EXIT.ok ? "" : said.join("\n") }).toEqual({
    code: EXIT.ok,
    said: "",
  });
  const second = LONG_BUILDS[1];
  const later = pathHash(`${longPath("c3").slice(0, -6)}/Later.jsonl`);
  // acme/newproj, which had its first commit by then, gives the profile nova, which the subagent's
  // recorded listing holds: no name is missing.
  const written = tasksOf(from).find((task) => task.source === "written");
  const profile = JSON.parse(readFileSync(path.resolve(dir, `${written?.id}.json`), "utf8"));
  expect(profile.skills.map((each: { name: string }) => each.name)).toContain("nova");
  const evidence = (reprofileSummary(dir).profiles.contextSession?.evidence ?? []) as Item[];
  const sources = Object.fromEntries(evidence.map((each) => [each.item, each.source]));
  const measure = ListingMeasure.parse(JSON.parse(sources[LISTING_MEASURE_ITEM] ?? "{}"));
  expect(measure).toMatchObject({ kind: "subagent listing", missing: [] });
  expect(measure.subagent?.pathHash).toBe(later);
  expect(sources["read times"]).toStartWith(
    `its subagent ${later}, the earliest whose recorded prompt holds a skill listing and the one its listing measure uses, started at ${LATER_SPAWN}, under the build of the last system-prompt-digest line before then (${second?.at}); so it is the build of line 2 of the session's 3 system-prompt-digest lines, at ${second?.at}, digest ${second?.digest}, and each repository, a pool's too, is read at ${second?.at}`
  );
}, 120_000);

/** The build-tasks run in the reader's mode over a store in which task-4's session started
 *  Helper, whose recorded listing holds alpha and beta: its tasks file, made once. */
let readerSubagentRun: Promise<string> | undefined;
function readerSubagentTasks(): Promise<string> {
  readerSubagentRun ??= (async () => {
    const store = changedStore("reader-subagent", (files) => {
      (files.store_a ?? {})[`${DEV_SESSIONS}/task-4/Helper.jsonl`] = sessionText("subagent");
    });
    const out = path.join(scratch, "reader-subagent", "tasks.jsonl");
    const dir = path.join(scratch, "reader-subagent", "profiles");
    const said: string[] = [];
    const args = tasksArgs(out, ...profileFlags(dir)).map((arg) => (arg === config ? store : arg));
    expect(await main(args, io(said))).toBe(EXIT.ok);
    return out;
  })();
  return readerSubagentRun;
}

// Catches: a task the reader picked from a session measured as if the store held no subagent of
// its session, when the store was never asked: its session's path was not known there.
test("build-tasks in the reader's mode measures a task from a session against the subagent of that session the store gives, and says the store holds none only after asking it", async () => {
  const out = await readerSubagentTasks();
  const summary = JSON.parse(readFileSync(`${out}.summary.json`, "utf8")) as {
    profiles: { tasks: { task: string; evidence?: Item[] }[] };
  };
  const measureOf = (session: string) => {
    const task = tasksOf(out).find((each) => each.pathHash === pathHash(session));
    const note = summary.profiles.tasks.find((each) => each.task === task?.id);
    const item = note?.evidence?.find((each) => each.item === LISTING_MEASURE_ITEM);
    return ListingMeasure.parse(JSON.parse(item?.source ?? "{}"));
  };
  // task-4's session started Helper, whose recorded listing holds alpha and beta.
  expect(measureOf(`${DEV_SESSIONS}/task-4.jsonl`)).toMatchObject({
    kind: "subagent listing",
    recorded: 2,
    subagent: { pathHash: pathHash(`${DEV_SESSIONS}/task-4/Helper.jsonl`) },
  });
  // task-3's session started none, and the store, asked, says so.
  const none = measureOf(`${DEV_SESSIONS}/task-3.jsonl`);
  expect(none.kind).toBe("not measurable");
  expect(none.why).toEndWith(
    "the store holds no subagent of it (subagents.sql, asked for those spawned under the session's first prompt build, the one its profile is read at (no later line))"
  );
}, 120_000);

// Catches: a task's profile that keeps only the names of the listing its measure uses, so a load
// test draws a hint for a skill whose line its session recorded.
test("a task's profile keeps each line of the listing its measure sets it against and where it was recorded: a subagent's, with its path hash, and the copy of the tested skill the session had; and none where the measure has no listing", async () => {
  const out = await readerSubagentTasks();
  const profileOf = (session: string) => {
    const file = tasksOf(out).find((each) => each.pathHash === pathHash(session))?.profile;
    expect(file).toBeString();
    return { file: String(file), profile: JSON.parse(readFileSync(String(file), "utf8")) };
  };
  // task-4's session recorded no prompt: its profile keeps the listing of Helper, the subagent
  // its measure uses, line by line, and the copy of alpha the session read, which its skills
  // leave out.
  const four = profileOf(`${DEV_SESSIONS}/task-4.jsonl`);
  expect(four.profile.recordedListing).toMatchObject({
    source: { kind: "subagent", pathHash: pathHash(`${DEV_SESSIONS}/task-4/Helper.jsonl`) },
    lines: [
      { name: "alpha", text: "Use for the alpha steps." },
      { name: "beta", text: "Use for the beta greeting." },
    ],
    tested: { name: "alpha" },
  });
  expect(four.profile.skills.map((each: { name: string }) => each.name)).not.toContain("alpha");
  // The load test reads it so.
  expect(loadProfile(four.file, "omp", "alpha").profile.recordedListing).toEqual(
    four.profile.recordedListing
  );
  // task-3's session started no subagent and recorded no prompt: its measure has no listing.
  expect(profileOf(`${DEV_SESSIONS}/task-3.jsonl`).profile.recordedListing).toBeUndefined();
}, 120_000);

// Catches: a task's profile of a session that recorded its own prompt (the session listing, the
// most common kind) that keeps no line of it, so a load test draws a hint for each skill; and a
// secret of its listing kept in a line, or counted twice.
test("a task's profile of a session that recorded its own prompt keeps each line of that prompt's listing as the session recorded it, its secrets taken out and counted once, and the copy of the tested skill it had", async () => {
  // task-1's session is a subagent whose recorded prompt lists alpha, and beta under a line that
  // holds a secret: a build of its own.
  const store = changedStore("session-listing", (files) => {
    (files.store_a ?? {})[`${DEV_SESSIONS}/task-1.jsonl`] = sessionText("subagent").replace(
      "- beta: Use for the beta greeting.",
      `- beta: Use for the beta greeting, Bearer ${SECRET_TOKEN}.`
    );
  });
  const out = path.join(scratch, "session-listing", "tasks.jsonl");
  const dir = path.join(scratch, "session-listing", "profiles");
  const said: string[] = [];
  const args = tasksArgs(out, ...profileFlags(dir)).map((arg) => (arg === config ? store : arg));
  expect(await main(args, io(said))).toBe(EXIT.ok);
  const task = tasksOf(out).find(
    (each) => each.pathHash === pathHash(`${DEV_SESSIONS}/task-1.jsonl`)
  );
  expect(task?.profile).toBeString();
  const file = String(task?.profile);
  const profile = JSON.parse(readFileSync(file, "utf8"));
  expect(profile.context).toBe("subagent:task");
  // The profile's recorded prompt holds the beta line with the secret taken out.
  const betaLine = /\n- beta: (.*)\n/.exec(String(profile.systemPrompt?.[0]))?.[1] ?? "";
  expect(betaLine).toStartWith("Use for the beta greeting, Bearer ");
  expect(JSON.stringify(profile)).not.toContain(SECRET_TOKEN);
  const lines = [
    { name: "alpha", text: "Use for the alpha steps." },
    { name: "beta", text: betaLine },
  ];
  expect(profile.recordedListing).toMatchObject({
    source: { kind: "session" },
    lines,
    tested: { name: "alpha" },
  });
  expect(profile.skills.map((each: { name: string }) => each.name)).not.toContain("alpha");
  // The one secret is counted once: taken out of the recorded prompt, not again from its lines.
  const summary = JSON.parse(readFileSync(`${out}.summary.json`, "utf8")) as {
    profiles: { tasks: { task: string; secretsRemoved?: number }[] };
  };
  expect(summary.profiles.tasks.find((each) => each.task === task?.id)?.secretsRemoved).toBe(1);
  // The load test reads it so, and each line it gives a capture names the session as its source,
  // which the capture records as `recorded (session)`.
  const read = loadProfile(file, "omp", "alpha").profile.recordedListing;
  expect(read).toEqual(profile.recordedListing);
  const text = "Use for the alpha steps of the made-up project.";
  expect(recordedLines(read, { name: "alpha", ownName: "alpha", description: text }, text)).toEqual(
    [
      { ...lines[0], from: "session", description: text },
      { ...lines[1], from: "session" },
    ]
  );
}, 120_000);

// Catches: a context session that no subagent places at a build said to be read at its first build
// because "a task runs as its session's first message": a context session has no step and is no
// task's session.
test("a context session no subagent of which records a skill listing is read at its first build, and its read times and listing measure say why: no subagent names the build, the store having been asked for those spawned at any time", async () => {
  const from = await profiledTasks();
  const summary = JSON.parse(readFileSync(`${from}.summary.json`, "utf8")) as {
    profiles: { contextSession: { evidence: Item[] } };
  };
  const sources = Object.fromEntries(
    summary.profiles.contextSession.evidence.map((each) => [each.item, each.source])
  );
  // The legion session: one system-prompt-digest line, and no file below its directory.
  const asked =
    "the store holds no subagent of it (subagents.sql, asked for those spawned at any time)";
  expect(sources["read times"]).toStartWith(
    `a context session has no step, and no subagent names the build it is read at: ${asked}; so it is the session's first build (its line is the first of the session's 1 system-prompt-digest line, at 2026-11-20T01:59:48.959Z, digest 46874d907e557998), and each repository, a pool's too, is read at the session's start ${F1_START}`
  );
  const measure = ListingMeasure.parse(JSON.parse(sources[LISTING_MEASURE_ITEM] ?? "{}"));
  expect(measure.kind).toBe("not measurable");
  expect(measure.why).toEndWith(asked);
}, 120_000);

// Catches: a second build-cases of one --out emptying the spool under a run still pulling into it
// (the first run's session file vanishes mid-read: ENOENT), and a lock left by a run that ended
// blocking the next. The first run builds two sessions at --concurrency 2 with each pull delayed,
// so it is pulling when the second starts: a whole build, hence the explicit timeout.
test("a second build-cases of an --out another run holds is refused (exit 2), naming that run, and the first run's sessions keep their spool files; a lock whose process runs no more is taken over, and said so", async () => {
  const out = path.join(scratch, "spool-lock", "cases.jsonl");
  const sessions = sessionsFile("spool-lock.jsonl", ["good", "legion"]);
  const args = buildArgs(sessions, out, "2", "--concurrency", "2");
  const lock = `${out}.spool/run.lock`;
  process.env.SKILL_CUTS_STANDIN_PULL_DELAY_MS = "150";
  try {
    const first: string[] = [];
    const running = main(args, io(first));
    // The run makes its spool directory and its lock in one step, before its first pull.
    while (!existsSync(`${out}.spool`)) await Bun.sleep(5);
    const second: string[] = [];
    expect(await main(args, io(second))).toBe(EXIT.refused);
    expect(second.join("\n")).toContain(
      `${out}.spool is in use: the run of process ${process.pid} holds its lock ${lock}`
    );
    expect({ code: await running, said: first.join("") }).toEqual({
      code: EXIT.ok,
      said: expect.not.stringContaining("ENOENT"),
    });
    // No session lost its spool file: each gave its case.
    expect(summaryOf(out).sessions.map((each) => each.outcome)).toEqual([
      expect.stringMatching(/^case /),
      expect.stringMatching(/^case /),
    ]);
    expect(existsSync(`${out}.spool`)).toBe(false);
  } finally {
    delete process.env.SKILL_CUTS_STANDIN_PULL_DELAY_MS;
  }
  // A lock of a process that runs no more (a killed build): the next run takes it over. This run
  // goes on from the first's summary, so it pulls nothing.
  const dead = Bun.spawnSync(["true"]).pid;
  mkdirSync(`${out}.spool`, { recursive: true });
  writeFileSync(lock, `${dead} a-killed-run\n`);
  const lines: string[] = [];
  expect(await main(args, io(lines))).toBe(EXIT.ok);
  expect(lines).toContain(
    `the lock ${lock} ("${dead} a-killed-run") is of process ${dead}, which runs no more: this run takes it over`
  );
  expect(existsSync(`${out}.spool`)).toBe(false);
}, 60_000);

// Catches: a spool lock made in two steps, the file first and its pid after, so a run that read it
// between them read no pid, took it for a dead run's lock and shared the spool; and a lock its run
// gave up between another run's failed make and its read, which failed that run (ENOENT). node:fs
// is wrapped for the build (Bun's mock.module updates the bindings build.ts imported): after each
// call that can make or write the lock, the lock's text is kept; and the first make that finds a
// lock there removes that lock before it fails, as a run that gave it up in between.
test("the spool lock never exists without its pid, and a lock given up while a run makes its own is taken by that run, which builds", async () => {
  const out = path.join(scratch, "spool-atomic", "cases.jsonl");
  const lock = `${out}.spool/run.lock`;
  const sessions = sessionsFile("spool-atomic.jsonl", ["good"]);
  mkdirSync(`${out}.spool`, { recursive: true });
  // A lock of this live process: the run must wait for it, unless it is given up.
  const planted = `${process.pid} a-run-still-going\n`;
  writeFileSync(lock, planted);
  const real = { ...nodeFs };
  const seen: string[] = [];
  let givenUp = false;
  const watched =
    <A extends unknown[], R>(call: (...args: A) => R) =>
    (...args: A): R => {
      try {
        return call(...args);
      } catch (error) {
        if (!givenUp && args.includes(lock) && String(error).includes("EEXIST")) {
          givenUp = true;
          real.rmSync(lock);
        }
        throw error;
      } finally {
        if (real.existsSync(lock)) seen.push(real.readFileSync(lock, "utf8"));
      }
    };
  mock.module("node:fs", () => ({
    ...real,
    openSync: watched(real.openSync),
    writeSync: watched(real.writeSync),
    closeSync: watched(real.closeSync),
    writeFileSync: watched(real.writeFileSync),
    linkSync: watched(real.linkSync),
  }));
  const lines: string[] = [];
  let code: number;
  try {
    code = await main(buildArgs(sessions, out, "1"), io(lines));
  } finally {
    mock.module("node:fs", () => real);
  }
  expect({ code, said: lines.join("") }).toEqual({
    code: EXIT.ok,
    said: expect.not.stringContaining("ENOENT"),
  });
  expect(givenUp).toBe(true);
  // The run's own lock was seen, and never without its pid: `<pid> <uuid>`.
  const own = seen.filter((text) => text !== planted);
  expect(own.length).toBeGreaterThan(0);
  expect(own.filter((text) => !/^\d+ [0-9a-f-]{36}\n$/.test(text))).toEqual([]);
  expect(existsSync(`${out}.spool`)).toBe(false);
}, 60_000);

// Catches: two runs that both read one dead lock both taking the spool: each removed the lock it
// found and made its own, so the run that came second removed the first run's live lock. node:fs
// is wrapped for the build: just before the run takes the dead lock
// over (its first remove or rename of the lock), another run's live lock replaces the dead one,
// as a second run that took it over first would leave it.
test("a dead spool lock another run takes over first is not taken from that run: this run is refused (exit 2), and the other run's lock stays", async () => {
  const out = path.join(scratch, "spool-takeover", "cases.jsonl");
  const lock = `${out}.spool/run.lock`;
  const sessions = sessionsFile("spool-takeover.jsonl", ["good"]);
  mkdirSync(`${out}.spool`, { recursive: true });
  const dead = Bun.spawnSync(["true"]).pid;
  writeFileSync(lock, `${dead} a-killed-run\n`);
  // The other run's lock: of this live process, so it holds the spool.
  const other = `${process.pid} the-run-that-took-it-first\n`;
  const real = { ...nodeFs };
  let swapped = false;
  const first =
    <A extends unknown[], R>(call: (...args: A) => R) =>
    (...args: A): R => {
      if (!swapped && args[0] === lock) {
        swapped = true;
        real.writeFileSync(lock, other);
      }
      return call(...args);
    };
  mock.module("node:fs", () => ({
    ...real,
    rmSync: first(real.rmSync),
    renameSync: first(real.renameSync),
  }));
  const lines: string[] = [];
  let code: number;
  try {
    code = await main(buildArgs(sessions, out, "1"), io(lines));
  } finally {
    mock.module("node:fs", () => real);
  }
  expect(swapped).toBe(true);
  expect(code).toBe(EXIT.refused);
  expect(lines.join("")).toContain(
    `${out}.spool is in use: the run of process ${process.pid} holds its lock ${lock} ("${other.trim()}")`
  );
  expect(readFileSync(lock, "utf8")).toBe(other);
}, 60_000);

// Catches: a subagent's digest-times.sql read (the times of its root's prompt builds) made
// outside build-cases' one-query-at-a-time queue, so at --concurrency 2 it ran beside the other
// session's pull. The subagent's pull comes first and each pull takes 400 ms, so its digest read
// is due while the other session pulls: a whole build, hence the explicit timeout.
test("at --concurrency 2 a subagent's digest-times.sql read waits for the other session's pull: no two store queries run at once", async () => {
  const out = path.join(scratch, "one-query", "cases.jsonl");
  // Seed 1 swaps two sessions: the subagent is pulled first.
  const sessions = sessionsFile("one-query.jsonl", ["good", "subagent"]);
  const logged = psqlLogLength();
  process.env.SKILL_CUTS_STANDIN_PULL_DELAY_MS = "400";
  try {
    expect(await main(buildArgs(sessions, out, "1", "--concurrency", "2"), io([]))).toBe(EXIT.ok);
  } finally {
    delete process.env.SKILL_CUTS_STANDIN_PULL_DELAY_MS;
  }
  expect(summaryOf(out).order[0]?.hash).toBe(fileHash(sessionPath("subagent")));
  const calls = psqlLines(logged) as unknown as { kind: string; started: number; ended: number }[];
  expect(calls.map((call) => call.kind)).toContain("digests");
  const together = calls.flatMap((a, index) =>
    calls
      .slice(index + 1)
      .filter((b) => a.started < b.ended && b.started < a.ended)
      .map((b) => `${a.kind} with ${b.kind}`)
  );
  expect(together).toEqual([]);
}, 60_000);

describe("GithubReads", () => {
  const COMMIT = "1111111111111111111111111111111111111111";
  const BLOB = "2222222222222222222222222222222222222222";
  const SKILL = "---\nname: alpha\ndescription: Use for the alpha list.\n---\n";
  const failed = (stderr: string): GhResult => ({ code: 1, stdout: "", stderr: `${stderr}\n` });
  /** gh's answer to the read of the blob BLOB: SKILL. */
  const skillBlob: GhResult = {
    code: 0,
    stdout: JSON.stringify({ content: Buffer.from(SKILL).toString("base64"), encoding: "base64" }),
    stderr: "",
  };
  /** A rule that sends a read again with no wait. */
  const noWait = () => new GhRetries({ sleep: async () => {}, err: () => {} }, "build-cases");
  /** A stand-in gh of `repo`, whose tree at COMMIT holds skills/alpha/SKILL.md, the blob BLOB:
   *  each call kept in `calls`, a GraphQL batch refused (so the blob is read alone), and the blob
   *  read answered by `blob`. */
  const standIn = (repo: string, blob: () => GhResult) => {
    const calls: string[] = [];
    const gh = async (args: readonly string[]): Promise<GhResult> => {
      const endpoint = args[1] ?? "";
      calls.push(endpoint);
      if (endpoint === `repos/${repo}/git/trees/${COMMIT}?recursive=1`) {
        const entry = { path: "skills/alpha/SKILL.md", mode: "100644", type: "blob", sha: BLOB };
        return { code: 0, stdout: JSON.stringify({ sha: COMMIT, tree: [entry] }), stderr: "" };
      }
      if (endpoint === `repos/${repo}/git/blobs/${BLOB}`) return blob();
      return failed("gh: Not Found (HTTP 404)");
    };
    return { gh, calls };
  };

  // Catches: a SKILL.md read that failed for a transient reason on every try kept, as a fault,
  // for every later case at that repository and commit.
  test("a SKILL.md read that fails for a transient reason on every try is not kept: the next read of that commit reads it again", async () => {
    let flaky = true;
    const { gh, calls } = standIn("acme/skills", () =>
      flaky ? failed("net/http: TLS handshake timeout") : skillBlob
    );
    const reads = new GithubReads(gh, new Map(), noWait());
    const first = await reads.at("acme/skills", COMMIT, []).then(() => "read", String);
    expect(first).toContain("TLS handshake timeout");
    flaky = false;
    const again = await reads.at("acme/skills", COMMIT, []);
    expect(again.faults).toEqual([]);
    expect(again.copies.map((copy) => copy.description)).toEqual(["Use for the alpha list."]);
    // Five tries the first time, one the second.
    expect(calls.filter((call) => call.endsWith(`/git/blobs/${BLOB}`))).toHaveLength(6);
  });

  // Catches: a registry read that failed once (an HTTP 5xx, a time-out) kept for the rest of the
  // run, so every later profile that needs that image failed.
  test("an image config read that fails is not kept: the next read of that image reads the registry again", async () => {
    const image = `registry.example.test/acme/worker@sha256:${"a".repeat(64)}`;
    const config: ImageConfig = { env: [], labels: {}, diffIds: [] };
    let tries = 0;
    const reads = new GithubReads(
      standIn("acme/skills", () => skillBlob).gh,
      new Map(),
      noWait(),
      undefined,
      async () => {
        tries++;
        if (tries === 1) throw new Error(`reading ${image}: registry HTTP 502`);
        return config;
      }
    );
    expect(await reads.imageConfig(image).then(() => "read", String)).toBe(
      `Error: reading ${image}: registry HTTP 502`
    );
    expect(await reads.imageConfig(image)).toEqual(config);
    // The read that answered is kept.
    expect(await reads.imageConfig(image)).toEqual(config);
    expect(tries).toBe(2);
  });

  // Catches: a failure taken as transient for a word of the repository's name in its error, so a
  // read that failed for good is sent again for every later case.
  test("a read that fails for good is kept, though the repository's name holds the word timeout", async () => {
    const { gh, calls } = standIn("acme/other", () => failed("gh: Not Found (HTTP 404)"));
    const reads = new GithubReads(gh, new Map(), noWait());
    for (let read = 0; read < 2; read++) {
      const outcome = await reads.at("acme/request-timeout", COMMIT, []).then(() => "read", String);
      expect(outcome).toContain("gh: Not Found (HTTP 404)");
    }
    expect(calls).toEqual([`repos/acme/request-timeout/git/trees/${COMMIT}?recursive=1`]);
  });

  // Catches: a read taken as transient from every try's errors joined, so a read whose first try
  // met a 502 and whose last a 404 rejected the whole repository's read where the file is a fault
  // of it, and was sent again for every later case.
  test("a SKILL.md read whose first try meets a 502 and whose last a 404 failed for good: it is a fault of the read, kept, not a transient failure", async () => {
    let tries = 0;
    const { gh, calls } = standIn("acme/skills", () =>
      failed(tries++ === 0 ? "gh: Bad Gateway (HTTP 502)" : "gh: Not Found (HTTP 404)")
    );
    const reads = new GithubReads(gh, new Map(), noWait());
    const read = await reads.at("acme/skills", COMMIT, []);
    const why = `SKILL.md read failed: gh api repos/acme/skills/git/blobs/${BLOB} failed: 2 tries: try 1: gh: Bad Gateway (HTTP 502); try 2: gh: Not Found (HTTP 404)`;
    expect(read.faults).toEqual([{ file: "skills/alpha/SKILL.md", why }]);
    expect(read.copies.map((copy) => copy.fault)).toEqual([why]);
    // The read is kept: a second read of the commit sends nothing.
    await reads.at("acme/skills", COMMIT, []);
    expect(calls.filter((call) => call.endsWith(`/git/blobs/${BLOB}`))).toHaveLength(2);
  });

  // Catches: a SKILL.md blob decoded with its leading byte order mark kept (`Buffer.toString`,
  // and a GraphQL batch's text as given), so its frontmatter was not found: no description, and
  // `hide: true` not seen; Oh My Pi drops the mark as it reads the file.
  test("a SKILL.md that starts with a byte order mark is read as Oh My Pi reads it, over REST and in a GraphQL batch: its description, and its hide", async () => {
    const marked =
      "\uFEFF---\nname: alpha\ndescription: Use for the alpha list.\nhide: true\n---\n";
    const rest = standIn("acme/skills", () => ({
      code: 0,
      stdout: JSON.stringify({
        content: Buffer.from(marked).toString("base64"),
        encoding: "base64",
      }),
      stderr: "",
    }));
    const batch = { isBinary: false, isTruncated: false, text: marked };
    const graphql = async (args: readonly string[]): Promise<GhResult> =>
      args[1] === "graphql"
        ? { code: 0, stdout: JSON.stringify({ data: { repository: { b0: batch } } }), stderr: "" }
        : rest.gh(args);
    for (const gh of [rest.gh, graphql]) {
      const read = await new GithubReads(gh, new Map(), noWait()).at("acme/skills", COMMIT, []);
      expect(read.copies.map((copy) => [copy.name, copy.nameFrom, copy.description])).toEqual([
        ["alpha", "frontmatter", "Use for the alpha list."],
      ]);
      expect([...read.hidden]).toEqual(["skills/alpha"]);
    }
    // The REST read went to the blob; the GraphQL read did not.
    expect(rest.calls.filter((call) => call.endsWith(`/git/blobs/${BLOB}`))).toHaveLength(1);
  });

  // Catches: a SKILL.md read GitHub refused for its secondary rate limit (an HTTP 403, no
  // transient reason) kept as a fault, so the skill is left out of the profile.
  test("a SKILL.md read refused twice for the secondary rate limit is sent again after SECONDARY_WAIT_MS, then after twice that, with no read of a limit's reset, and is read whole", async () => {
    const SECONDARY =
      "gh: You have exceeded a secondary rate limit. Please wait a few minutes before you try again. (HTTP 403)";
    let refusals = 2;
    const { gh, calls } = standIn("acme/skills", () =>
      refusals-- > 0 ? failed(SECONDARY) : skillBlob
    );
    const waits: number[] = [];
    const lines: string[] = [];
    const retries = new GhRetries(
      { sleep: async (ms) => void waits.push(ms), err: (line) => void lines.push(line) },
      "build-cases"
    );
    const asked: string[][] = [];
    const reads = new GithubReads(
      (args) => {
        asked.push([...args]);
        return gh(args);
      },
      new Map(),
      retries
    );
    const read = await reads.at("acme/skills", COMMIT, []);
    expect(read.faults).toEqual([]);
    expect(read.copies.map((copy) => copy.description)).toEqual(["Use for the alpha list."]);
    expect(waits).toEqual([SECONDARY_WAIT_MS, 2 * SECONDARY_WAIT_MS]);
    expect(lines.map((line) => line.replace(/, until \S+, /, ", until <time>, "))).toEqual(
      [1, 2].map(
        (n) =>
          `build-cases: a gh call was refused for its rate limit (${SECONDARY}); waiting ${(n * SECONDARY_WAIT_MS) / 1000} s, until <time>, before sending it again, refusal ${n} of ${MAX_RATE_RETRIES}`
      )
    );
    // No reset is read: neither `gh api rate_limit` nor an installation's (`gh api -i`).
    expect(asked.some((args) => args.includes("rate_limit") || args.includes("-i"))).toBe(false);
    expect(calls.filter((call) => call.endsWith(`/git/blobs/${BLOB}`))).toHaveLength(3);
  });
});

// Catches: a store command that hangs (a connection that never answers) holding a pull, and with it
// the build, for ever; and a limit on the whole call, which would end a long pull that is still
// streaming its rows.
test("a pull whose store command prints nothing for the quiet limit is ended, naming the command and the limit; one that prints a little at a time within it runs as long as it needs", async () => {
  const root = path.join(scratch, "store-quiet");
  mkdirSync(root, { recursive: true });
  const spool = path.join(root, "pull.jsonl");
  // It reads its query, then waits in a child of its own, which holds its output open.
  const hung = path.join(root, "hung.sh");
  writeFileSync(hung, "#!/bin/sh\ncat >/dev/null\nsleep 5\n", { mode: 0o755 });
  const started = performance.now();
  await expect(pullTranscript(hung, "store_a", "/sessions/x.jsonl", spool, 200)).rejects.toThrow(
    `the store command ${hung} printed nothing for 200 ms on store_a, so it was ended (STORE_QUIET_MS)`
  );
  expect(performance.now() - started).toBeLessThan(3_000);
  // An answer of no row printed in pieces 300 ms apart: 1.8 s in all, past a limit of 1.2 s, but
  // never 1.2 s without a piece (the margin is for a loaded machine).
  const slow = path.join(root, "slow.sh");
  writeFileSync(
    slow,
    "#!/bin/sh\nprintf ' value '\ncat >/dev/null\nfor piece in '\\n' '-------' '\\n' '(0 rows)' '\\n' '\\n'; do sleep 0.3; printf -- \"$piece\"; done\n",
    { mode: 0o755 }
  );
  expect(await pullTranscript(slow, "store_a", "/sessions/x.jsonl", spool, 1_200)).toEqual({
    bytes: 0,
    calls: 1,
  });
}, 60_000);

// Catches: a store command that prints its whole answer, closes its output and does not exit,
// waited for with no limit once its output ended (`child.exited`, then its error text). It reads
// its query, prints an answer of no row, closes its output, and waits in a child of its own, which
// holds its error output open.
test("a store command that closes its output and does not exit is ended at the quiet limit, naming the command and the limit", async () => {
  const root = path.join(scratch, "store-lingers");
  mkdirSync(root, { recursive: true });
  const lingers = path.join(root, "lingers.sh");
  writeFileSync(
    lingers,
    "#!/bin/sh\ncat >/dev/null\nprintf ' value \\n-------\\n(0 rows)\\n\\n'\nexec >&-\nsleep 5\n",
    { mode: 0o755 }
  );
  const started = performance.now();
  await expect(
    pullTranscript(lingers, "store_a", "/sessions/x.jsonl", path.join(root, "pull.jsonl"), 300)
  ).rejects.toThrow(
    `the store command ${lingers} printed nothing for 300 ms on store_a, so it was ended (STORE_QUIET_MS)`
  );
  expect(performance.now() - started).toBeLessThan(3_000);
}, 60_000);

describe("GithubReads.tarball", () => {
  const ALPHA = "https://registry.npmjs.org/@acme/alpha/-/alpha-1.2.3.tgz";
  const BYTES = new Uint8Array([31, 139, 8, 0]);
  /** The time limit of a try here: short, so a row whose try meets it takes well under a second. */
  const LIMIT_MS = 50;
  /** The limit of the row whose answer stops after its headers: long enough that the local
   *  registry's headers, sent at once, come before it however loaded the machine is, so the try is
   *  given up for its body, which never ends. */
  const STALL_LIMIT_MS = 2_000;
  /** One fetch of the registry, given the signal registryTarball gives it. */
  type Fetch = (init: { readonly signal: AbortSignal }) => Promise<Response>;
  /** A local registry (recorder.ts's) whose answers only the time limit ends: `/never` never
   *  answers, and `/stall` answers a 200 of 1000 bytes and sends 3 of them. */
  let registry: Recorder;
  beforeAll(() => {
    registry = startRecorder({
      route: ({ path }) =>
        path === "/stall"
          ? new Response(new ReadableStream({ start: (sent) => sent.enqueue(new Uint8Array(3)) }), {
              headers: { "content-length": "1000" },
            })
          : Promise.withResolvers<Response>().promise,
    });
  });
  afterAll(() => registry.close());
  /** A real fetch of `route` of the local registry. */
  const local =
    (route: string): Fetch =>
    (init) =>
      fetch(`${registry.url}${route}`, init);
  /** The registry's answer `status`, with `body`. */
  const answer =
    (status: number, body: Uint8Array | null = null): Fetch =>
    async () =>
      new Response(body, { status });
  /** A connection that failed as Bun 1.4.4's fetch fails one: a TypeError whose message opens with
   *  its code. */
  const failed =
    (code: string, words: string): Fetch =>
    async () => {
      throw Object.assign(new TypeError(`${code}: ${words}`), { code });
    };
  /** The profile's reads, whose registry read is registryTarball's with the limit LIMIT_MS, its
   *  n-th fetch (from 0) `fetches[n]`, each URL it asks kept in `asked`; a retry waits for no time,
   *  each wait it asks for kept in `waits` and each line it says in `lines`. */
  const reading = (fetches: readonly Fetch[], limitMs = LIMIT_MS) => {
    const asked: string[] = [];
    const waits: number[] = [];
    const lines: string[] = [];
    const retries = new GhRetries(
      {
        sleep: async (ms) => {
          waits.push(ms);
        },
        err: (line) => lines.push(line),
      },
      "build-cases"
    );
    const gh = async (): Promise<GhResult> => ({
      code: 1,
      stdout: "",
      stderr: "no gh call is made here\n",
    });
    const reads = new GithubReads(gh, new Map(), retries, (name, version) =>
      registryTarball(name, version, limitMs, (url, init) => {
        const next = fetches[asked.length];
        asked.push(url);
        if (next === undefined) throw new Error(`fetch ${asked.length} of ${url} was not expected`);
        return next(init);
      })
    );
    return { reads, asked, waits, lines, retries };
  };
  const sentAgain = (wait: number, retry: number, failure: string) =>
    `build-cases: the registry read of ${ALPHA} failed (${failure}); sending it again in ${wait} s, retry ${retry} of 4`;

  // Catches: a registry read with no time limit, whose try a network fault left unanswered, holding
  // the build for ever (a screen sat 15 minutes in one).
  test("a tarball read whose first try never answers is given up after its limit and sent again as a gh read is, its second try's bytes used, the retry counted and said", async () => {
    const { reads, asked, waits, lines, retries } = reading([local("/never"), answer(200, BYTES)]);
    expect(await reads.tarball("@acme/alpha", "1.2.3")).toEqual(BYTES);
    expect(asked).toEqual([ALPHA, ALPHA]);
    expect(retries.record()).toEqual({ "time limit": 1 });
    expect(waits).toEqual([5_000]);
    expect(lines).toEqual([sentAgain(5, 1, `timed out after ${LIMIT_MS} ms`)]);
  });

  // Catches: a limit that covers the answer's headers alone, so a body that stops half-way holds
  // the build for ever.
  test("a tarball whose body stops after its headers is given up after the limit too, and sent again", async () => {
    let headers = false;
    let tried = 0;
    const { reads, asked, waits, lines, retries } = reading(
      [
        async (init) => {
          tried = performance.now();
          const response = await local("/stall")(init);
          headers = true;
          return response;
        },
        answer(200, BYTES),
      ],
      STALL_LIMIT_MS
    );
    expect(await reads.tarball("@acme/alpha", "1.2.3")).toEqual(BYTES);
    // The headers came, and the try was given up at its limit, for its body.
    expect(headers).toBe(true);
    expect(performance.now() - tried).toBeGreaterThanOrEqual(STALL_LIMIT_MS * 0.9);
    expect(asked).toEqual([ALPHA, ALPHA]);
    expect(retries.record()).toEqual({ "time limit": 1 });
    expect(waits).toEqual([5_000]);
    expect(lines).toEqual([sentAgain(5, 1, `timed out after ${STALL_LIMIT_MS} ms`)]);
  }, 60_000);

  // Catches: a version the registry does not have sent again for minutes, or a refusal for good
  // taken as transient for a word of the package's name in its URL.
  test("a tarball the registry does not have (404) is undefined, and a 403 fails at once naming its URL, though the package's name holds the word timeout; neither is sent again", async () => {
    const missing = reading([answer(404)]);
    expect(await missing.reads.tarball("@acme/alpha", "1.2.3")).toBeUndefined();
    expect(missing.asked).toEqual([ALPHA]);
    const forbidden = reading([answer(403)]);
    const outcome = await forbidden.reads
      .tarball("@acme/request-timeout", "1.2.3")
      .then(() => "read", String);
    const url = "https://registry.npmjs.org/@acme/request-timeout/-/request-timeout-1.2.3.tgz";
    expect(outcome).toBe(`Error: ${url}: HTTP 403`);
    expect(forbidden.asked).toEqual([url]);
    for (const each of [missing, forbidden]) {
      expect(each.retries.record()).toEqual({});
      expect(each.waits).toEqual([]);
      expect(each.lines).toEqual([]);
    }
  });

  // Catches: a read that failed for a transient reason on every try failed with its last try's
  // error alone, or its failure kept, so a later caller of that tarball gets it with no read.
  test("a tarball read that fails for a transient reason on every try fails naming each try's failure, and the next read of that tarball reads the registry again", async () => {
    const reset = "The socket connection was closed unexpectedly.";
    const refused = "Unable to connect. Is the computer able to access the url?";
    const { reads, asked, waits, lines, retries } = reading([
      answer(503),
      failed("ECONNRESET", reset),
      answer(429),
      failed("ECONNREFUSED", refused),
      local("/never"),
      answer(200, BYTES),
    ]);
    const outcome = await reads.tarball("@acme/alpha", "1.2.3").then(() => "read", String);
    const tries = [
      "HTTP 503",
      `connection reset (ECONNRESET: ${reset})`,
      "HTTP 429 (too many requests)",
      `connection refused (ECONNREFUSED: ${refused})`,
      `timed out after ${LIMIT_MS} ms`,
    ];
    expect(outcome).toBe(
      `Error: ${ALPHA}: 5 tries: ${tries.map((failure, n) => `try ${n + 1}: ${failure}`).join("; ")}`
    );
    expect(retries.record()).toEqual({
      "connection reset": 1,
      "connection refused": 1,
      "HTTP 503": 1,
      "HTTP 429": 1,
    });
    expect(waits).toEqual([5_000, 15_000, 45_000, 135_000]);
    expect(lines).toEqual(
      [5, 15, 45, 135].map((wait, n) => sentAgain(wait, n + 1, tries[n] ?? ""))
    );
    // The next read of the tarball reads the registry: the failure is not kept.
    expect(await reads.tarball("@acme/alpha", "1.2.3")).toEqual(BYTES);
    expect(asked).toEqual(Array(6).fill(ALPHA));
  });

  // Catches: the registry's tarball of a pinned package version downloaded again for every
  // profile that pins it, or a failed download kept, so a later profile gets the failure unread.
  test("the skills a version's tarball ships are read once and kept, and a read that fails is not kept", async () => {
    const tarball = TARBALLS["@acme/gone@3.0.0"]?.() ?? new Uint8Array();
    const { reads, asked } = reading([answer(403), answer(200, tarball)]);
    const failed = await reads.shippedSkills("@acme/gone", "3.0.0").then(() => "read", String);
    expect(failed).toEndWith("HTTP 403");
    for (let read = 0; read < 2; read++) {
      expect([...((await reads.shippedSkills("@acme/gone", "3.0.0")) ?? [])]).toEqual(["gone-one"]);
    }
    expect(asked).toHaveLength(2);
  });
});
