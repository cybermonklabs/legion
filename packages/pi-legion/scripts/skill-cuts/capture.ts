/**
 * The capture of one arm of a case (README.md, "The harness strings"): the request body the pinned
 * Oh My Pi 18.18.6 sends for the arm's messages in the case's context, which replay.ts then sends
 * to the gateway. `captureBody(c, arm, options)` runs
 *
 *     omp --profile capture -p --mode json --session-dir <scratch sessions> --model <case model>
 *         --thinking <case thinking> --hook replay-hook.ts [--extension <plugin>]...
 *         [--append-system-prompt <file>] [--tools <the recorded tool names>]
 *         <placeholder prompt>
 *
 * with a scratch HOME (and, for a devbox context with a user-level context file, CLAUDE_CONFIG_DIR
 * at context.ts's scratch Claude Code config directory), in the case's context (context.ts),
 * against a recorder on 127.0.0.1 (recorder.ts) that every model provider of the scratch profile
 * points at. The Envoy plugin defines its tools only when it has a listener to reach, so the run
 * of a profile that installs it, in any context (a devbox one included), gets a stand-in Envoy
 * listener on that recorder and a sink for the NATS server it dials (a subagent's run gets the
 * listener alone), and a Legion pane's run gets both and a stand-in daemon. The hook's `context`
 * handler gives the model the arm's messages and its `tool_call` handler blocks every call. The
 * recorder keeps the turn's request and answers it with a fixed end-of-turn reply, so no tool runs
 * and the turn's request never reaches the gateway. One capture per case and arm: the body, as
 * the harness-string rule below leaves it, and its record are saved under `outDir`, with the body
 * as Oh My Pi sent it byte for byte beside them (`rawBodyFile`), and a capture already saved there
 * is read back rather than made again, with each installed copy its record names that the cache
 * lacks made again first (`restoreCopies`).
 *
 * **The two routes.** A case's model is served on the gateway's Anthropic Messages route (a
 * provider of `api: anthropic-messages` in `--models`) or on its OpenAI Responses route (Oh My
 * Pi's built-in `openai` provider, whose `api` a models.yml leaves out, or `api:
 * openai-responses`; the model listed under `models` or `modelOverrides`). Each provider kept is
 * pointed at the recorder with the path of its own `baseUrl` (`/v1` for the `openai` one, where Oh
 * My Pi posts `/v1/responses`). The recorder answers a Messages turn with the Messages API's end
 * of turn and a Responses turn with the Responses API's (`END_TURN_RESPONSE`), so the body of
 * either is captured the same way (F8: Oh My Pi 18.18.6 sends `openai/gpt-6.1-sol`'s turn to
 * `/v1/responses`, its system prompt as `instructions` and its history as `input` items, with no
 * `anthropic-*` header). Hints come from the hint model, which must be on the Messages route.
 *
 * **The skill listing.** Oh My Pi lists each installed skill in its system prompt under a hint,
 * not under the skill's `description`: a line of at most 160 characters that it asks its `smol`
 * model to write from the name and the description (packages/coding-agent/src/extensibility/
 * skill-descriptions.ts at the pinned tag: a one-message request of the prompt
 * `prompts/skills/compress-description.md`, which ends `Skill: <name>` / `Description: <text>`,
 * 1024 tokens at most, no reasoning, lines 124-144). It asks for temperature 0, but its Anthropic
 * provider sends a temperature only to a model that takes one (packages/ai/src/providers/
 * anthropic.ts, lines 5531-5540), and Opus 4.7 on, Sonnet, Fable and Mythos 5 on and Haiku 5.5 on
 * take none (packages/catalog/src/compat/rules/classes/anthropic.kdl, lines 127-161): their hint
 * request holds no `temperature`, so the model samples at the API's default, 1. It keeps each hint
 * in the profile's `skill-descriptions.db` (`skill_descriptions(key, description)`, the key the
 * SHA-256 of the prompt, the name and the description) and lists a cut-down `description` in its
 * place until the hint is written, which it does in the background once the session exists. So
 * the arm's listing is the hint Oh My Pi would list for the arm's text: each hint is kept in the
 * runner's cache (`<cacheDir>/hints/<key of the Oh My Pi version, the name and the
 * description>.json`) with the key Oh My Pi gave it, and written into the scratch profile's
 * database before the run. A skill the cache lacks needs one run more first, the hint run, in
 * which the recorder sends each hint request to `options.hints` (the real gateway,
 * `gatewayHints`) and holds the turn until Oh My Pi has stored the row of each missing hint in its
 * database, which the recorder reads as it waits; the capture keeps each hint from its row. Every
 * capture of one skill text under one Oh My Pi version therefore lists the same hint. A skill whose
 * frontmatter sets `hide` or `disable-model-invocation` is installed but never listed, so it gets
 * no hint. Neither is a copy whose body and frontmatter are those of a copy of its name that Oh My
 * Pi admitted before it, the bare copy or another namespace's (a profile that installs one text
 * twice, once as `<namespace>/<name>`): Oh My Pi drops it (`listedSkills`), so the capture owes it
 * no hint and lists it nowhere.
 *
 * **The hint run's bounds.** Oh My Pi asks for at most 4 hints at a time and gives a request up
 * after 30 s, storing no hint of it and asking no more for it in that run (skill-descriptions.ts
 * at the pinned tag: `compressionSlots`, line 40, and `AbortSignal.timeout(30_000)` in
 * `createSkillDescriptionCompressor`, line 120). So the turn waits as long as hint requests and
 * answers keep coming, and ends once HINT_IDLE_MS (60 s) pass with neither: Oh My Pi then asks for
 * no more. It is not bounded as a whole: a profile of more than 200 skills asks more than 100
 * hints, at about 1.6 s each with 4 at a time. The run itself is, at its time limit plus 30 s for
 * each 4 hints owed, the longest Oh My Pi's own limits let its hint requests take. A hint Oh My Pi
 * stored no row of fails the capture, which says why from what the recorder saw: no request came,
 * the answer had not come when the turn ended, or it came after Oh My Pi's 30 s. Oh My Pi drops an
 * answer only in those last two ways: it writes each row as soon as it has read its answer
 * (`compressSkill`, lines 539-551), and at exit it waits up to 4 s for requests in flight
 * (`STORE_CLOSE_GRACE_MS`, line 38) and then drops any later answer (`isClosed`, line 543).
 *
 * **A reply Oh My Pi keeps no hint of.** Oh My Pi keeps a reply, trimmed, only when it is not
 * empty, is one line and has at most 160 characters (`validCompression`, lines 191-195). A reply
 * cut off at its token limit is not kept either (lines 150-152 and 537-542); the recorder sends
 * every reply as an end of turn, but one cut off at 1024 tokens is longer than 160 characters, so
 * Oh My Pi keeps no hint of it all the same. For a reply it does not keep, it stores the cut-down
 * description under `<key>:v<revision of that rule>:<provider>/<model id>` (`unusableKey`, lines
 * 197-200, and `compressSkill`, lines 543-552) and goes on listing that cut-down description
 * (`render`, lines 448-477), which is `previewSkillDescription` (lines 174-185): the description
 * on one line when it has at most 100 characters, else its first sentence within the first 99
 * when that sentence has at least 40, else those 99 characters cut at their last space when that
 * space is at index 40 or later, followed by `…`. A later session asks that model no more for that
 * skill text: Oh My Pi asks again only under another model or a new revision of its rule (lines
 * 522-532). So such a reply fails no capture, and the runner asks no more either. The skill is
 * listed under the cut-down description, and that outcome is cached under the same key as a hint,
 * with `kept: false`, the reply, and why Oh My Pi did not keep it (`reason`), which the record's
 * `hints` repeats. Every arm and case of that skill text then lists the same line: a later capture
 * writes Oh My Pi's record of the reply into its scratch database under its own hint model, so Oh
 * My Pi asks that model for no hint either. A capture whose listing is not exactly the profile's
 * listed skills under their hints, or under the cut-down description where Oh My Pi kept none, is
 * refused.
 *
 * **A recorded listing line.** A load test (load.ts) gives the capture the lines of the skill
 * listing its task's session recorded (`options.recordedLines`, RecordedLine); the behaviour
 * commands give none. A listed skill is listed under its recorded line, in place of a hint, where
 * the line has its name and stands for the same skill text: the line names the description the
 * session's copy had and the skill has that description, or the line names none and the context
 * installs the skill from the profile's repository copy, the session's own. Oh My Pi then lists
 * exactly that line, a recorded cut-down description as that cut-down description: the capture
 * writes the line into the scratch profile's database under Oh My Pi's key for the name and the
 * description (`keyFor`: the SHA-256 of the hint prompt, the name and the description), so Oh My
 * Pi asks no model for that skill. The runner reads that key from the cached hint of draw 1 of
 * the skill text, else from its key cache (`<cacheDir>/hint-keys/<key of the Oh My Pi version,
 * the name and the description>.json`). Where neither holds it, the hint run answers Oh My Pi's
 * hint request for the skill with the recorded line itself and asks no model; the capture reads
 * the key off the row Oh My Pi stores, keeps it in the key cache, and writes the line under it.
 * The record's `hints` names each skill's source: `recorded (session)`, `recorded (subagent <path
 * hash>)`, or `drawn (draw <n>)`.
 *
 * **The draw.** A hint of draw 1 is cached under the key of the Oh My Pi version, the name and the
 * description, as above; a hint of draw n > 1 (`options.hintDraw`) under the key of those and
 * `draw <n>`. So each draw asks the hint model once more for each skill text that has no recorded
 * line, and every capture of one draw lists the same hint. Oh My Pi's key is the same in every
 * draw.
 *
 * **A recorded prompt.** A subagent's case carries the system prompt and tools its session
 * recorded (`profile.systemPrompt`, `profile.tools`), and its capture sends those blocks in place
 * of the prompt Oh My Pi builds: the replay hook's `before_agent_start` handler returns them
 * (`SKILL_CUTS_SYSTEM_PROMPT`), with the case skill's line of the recorded skill listing
 * (`- <skill>: <hint>`) changed (`armPromptBlocks`): the hint of the skill the arm lists under
 * that name, written and cached as above, or its recorded line where one applies, where the arm
 * lists one, and the line removed where it does not (`none`, or an arm whose text hides the
 * skill). An arm that lists a copy under its bare name in place of `<namespace>/<name>` (arms.ts's
 * `Arm.relisted`: `none`, once the tested copy of that name is gone) also leaves out the recorded
 * line of `<namespace>/<name>`, as Oh My Pi lists that copy under the bare name alone; where that
 * name is the case skill's, the case skill's line is that copy's hint, in the recorded line's
 * place, which is where Oh My Pi's listing, sorted by name, puts it. Every other byte is sent as
 * recorded. `--tools` restricts Oh My Pi's tools to the recorded names. The body's system prompt
 * must be those blocks byte for byte, or the capture is refused; a recorded tool the body does not
 * define, or one it defines beyond them, is kept in the record (`tools`).
 *
 * **An injected skill.** A skill that reaches the session other than by its listing line and its
 * reads (arms.ts's `Injection`) reaches the capture in the arm's messages, which carry each
 * arm's own text of it: the Envoy plugin's dispatch-first block, which the plugin itself puts in
 * no capture, Dispatch being off here, and a subagent's autoloaded block. The record lists each
 * (`injections`), and beside them whether the case's session had Dispatch and why (`dispatch`,
 * arms.ts's `dispatchOf`), which the plugin's block rests on. A subagent's case whose agent
 * definition (context.ts's `agent`) autoloads the skill, and whose messages hold no autoloaded
 * block of it though no compaction summary opens them, holds the block in a form the runner
 * cannot rebuild: its capture throws InjectionRefused of kind `autoload-no-block`, naming the
 * case, before Oh My Pi runs. A saved capture of an arm with such a block whose record has no
 * `injections` field was made before arms gave it their own text, and is refused
 * (`savedCapture`).
 *
 * **The Legion tool.** A Legion pane (a Legion role other than the controller, which is not
 * reproduced) whose context installs `pi-legion` as an extension sends that plugin's `legion`
 * tool, which the plugin defines at the end of its session start, after its registration with
 * the stand-in daemon; Oh My Pi reports a session-start handler that throws as `Extension error`,
 * and an extension whose module it cannot load as `Failed to load extension` (the plugin refuses
 * to load a bundle whose package.json holds no `legion.daemonApiVersion`, which a pack writes), on
 * stderr, and goes on, so such a run's body can lack the tool. Such a body is refused and the
 * arm captured once more by a fresh run: the same inputs, with no turn, temporary file or pane
 * state of the refused run. The record keeps each attempt (`legionAttempts`: its tool count,
 * whether it defines `legion`, its `[legion]`, `Extension error` and `Failed to load extension` lines of stderr), and its other
 * fields are the last attempt's. When the second body lacks the tool too, the capture throws
 * LegionToolUndefined, naming the case, the arm, the plugin and each attempt's lines, and saves
 * nothing. A context that installs no `pi-legion`, or installs it as skills only, is not refused.
 *
 * **The digests.** The record holds the session's `system-prompt-digest` (`sessionDigest`, from
 * the case) and the capture's own, `promptDigest`, which is Oh My Pi's algorithm
 * (packages/coding-agent/src/session/session-tools.ts, `systemPromptDigest`): a wyhash chain over
 * the system prompt's blocks, seeded with their count, in hex. Oh My Pi hashes its prompt blocks
 * before it sends them; a Messages body's `system` blocks are those blocks less any that is blank
 * or holds a credential it redacts, so the two agree whenever no block is either. A Responses body
 * holds the blocks joined into one `instructions` text, a blank line between, so its digest is
 * of that one text, and differs from the session's whenever the prompt has more than one block.
 * The capture's digest is of the body it saves, after the harness-string rule below.
 *
 * **The harness strings.** Oh My Pi writes into its request strings of the run that captures it,
 * not of the session: the reminder at the start of the first user turn (`Today: <date>; current
 * working directory: '<cwd>'`), whose working directory was the capture's scratch, which names
 * the run, the cut and the skill under test; the path of each context file's block (`<file
 * path="<cwd>/AGENTS.md">`, or the scratch Claude Code config directory's `CLAUDE.md`); and, in a
 * prompt it builds, the workstation's `- OS:` line of the pod. Such a string is a factor of the
 * measure: on one case of a behaviour screen, a working directory that named the skill had its
 * reference read in 12 and 33 of 40 runs, and one that named no skill in 0 and 1 of 40. So every
 * string of a body that comes from the capture and not from the session is replaced, before the
 * body is saved, by the session's recorded value, or by a neutral one where the store holds none
 * (`options.session`, `sessionFacts`): the working directory is the session header's `cwd` (or
 * the one its journaled reminder names), else NEUTRAL_CWD; the reminder is the one Oh My Pi
 * journaled for the turn the capture's reminder goes on (a `date-cwd-reminder` entry of the
 * session's head), byte for byte, else it is rendered from that working directory and the UTC
 * date of the turn it stands for; a context file's path is under that working directory (one of
 * a parent of it, when the profile names a `workingDirectory` below the checkout's root, under the
 * session's checkout: that directory less the `workingDirectory`, `sessionCheckout`), and the
 * user-level `CLAUDE.md` under the configuration's `devboxClaudeConfigDir`, else under
 * NEUTRAL_CLAUDE_CONFIG_DIR; a built prompt's `- OS:` line shows NEUTRAL_OS, the same for every
 * arm (a recorded prompt is sent as recorded, its workstation lines the session's own). Any other
 * mention of the capture's working directory, or of its checkout, names the session's. The
 * stand-in daemons answer with that working directory, made-up ids, and the session id each
 * request to the Envoy listener names. A body that still holds the case's scratch directory or the
 * capture's `scratchDir` after this is refused. The session's head comes from its store
 * (`sessionHeads`, session-head.sql, saved as `<store>-<pathHash>.json` in the directory its
 * caller gives: `<out>/sessions/` for `behaviour` and `load`, `<out>.sessions/` for `build-cases
 * --reprofile`), and the record says what each value is and where it came from (`harness`), and
 * keeps the body as Oh My Pi sent it beside the saved one (`rawBodyFile`). A saved capture whose
 * record holds no such record was made before this rule, and is refused (`savedCapture`).
 */
import { Database } from "bun:sqlite";
import { randomBytes } from "node:crypto";
import {
  existsSync,
  mkdirSync,
  readdirSync,
  readFileSync,
  rmSync,
  statSync,
  writeFileSync,
} from "node:fs";
import path from "node:path";
import { messageFor } from "@legion/envoy-client/errors";
import { consumerDelivery } from "@legion/envoy-client/test/subscribe-answer";
import { linkOmpNatives } from "@legion/pi-shared/test/omp-natives";
import { z } from "zod";
import {
  type Arm,
  type ArmName,
  type DispatchAnswer,
  ENVOY_PLUGIN,
  type Injection,
  InjectionRefused,
} from "./arms";
import type { Case } from "./cases";
import {
  json,
  ompText,
  pastLimit,
  Refused,
  readFrontmatter,
  STORE_QUIET_MS,
  splitFrontmatter,
  textOf,
  timeLimit,
  writeWhole,
} from "./common";
import {
  buildContext,
  type CaseContext,
  type ContextCase,
  type ContextRecord,
  cacheKey,
  type InstalledSkill,
  installedCopies,
  legionRole,
  type RepoSource,
} from "./context";
import { sessionHeadSql } from "./countsql";
import { type Gateway, isResponsesBody, type MessagesBody, type RequestBody } from "./gateway";
import { eachStoreRow } from "./psql-rows";
import { messageSse, type RecordedRequest, sseResponse, startRecorder } from "./recorder";
import { HOOK_LOG_VARIABLE, MESSAGES_VARIABLE, SYSTEM_PROMPT_VARIABLE } from "./replay-hook";

/** The scratch profile's name. */
const PROFILE = "capture";
/** The run's own first message, which the hook replaces with the arm's messages. */
const PLACEHOLDER = "placeholder prompt";
/** The hook file Oh My Pi loads. */
const HOOK = path.join(import.meta.dir, "replay-hook.ts");
/** Oh My Pi's limit on one hint request: it gives the request up after 30 s, stores no hint of it
 *  and asks no more for it in that run (skill-descriptions.ts at the pinned tag,
 *  `createSkillDescriptionCompressor`'s `AbortSignal.timeout(30_000)`). */
const OMP_HINT_TIMEOUT_MS = 30_000;
/** How many hint requests Oh My Pi has in flight at once (its `compressionSlots`). */
const OMP_HINT_SLOTS = 4;
/** How long the hint run's turn waits with no hint request and no hint answer: twice
 *  OMP_HINT_TIMEOUT_MS. While Oh My Pi still owes a hint, each request it has in flight ends
 *  within OMP_HINT_TIMEOUT_MS, answered or given up, and the slot it frees starts the next; so
 *  this long with neither means it asks for no more. */
const HINT_IDLE_MS = 2 * OMP_HINT_TIMEOUT_MS;
/** How often the hint run's turn reads Oh My Pi's database for the rows it is owed. */
const HINT_POLL_MS = 100;
/** The longest one Oh My Pi run may take, by default. */
const RUN_TIMEOUT_MS = 600_000;
/** Oh My Pi's own bound on a hint it keeps (`MAX_COMPRESSED_CHARS`). */
const MAX_HINT_CHARS = 160;
/** Oh My Pi's bounds on the cut-down description it lists where it holds no hint
 *  (`MAX_PREVIEW_CHARS`, `MIN_PREVIEW_CUT`). */
const MAX_PREVIEW_CHARS = 100;
const MIN_PREVIEW_CUT = 40;
/** The key of a hint Oh My Pi keeps in its `skill_descriptions`: a SHA-256 in hex. */
const HINT_ROW = /^[0-9a-f]{64}$/;
/** The key under which Oh My Pi records a reply it kept no hint of: the hint's key, the revision
 *  of its rule and the model's `<provider>/<id>` (`unusableKey`); the match is all but the model. */
const UNKEPT_ROW = /^[0-9a-f]{64}:v\d+:/;
/** Oh My Pi's key of a skill text (`keyFor`) at the start of either row's key. */
const OMP_KEY = /^[0-9a-f]{64}/;
/** What the scratch profile gives Oh My Pi as its provider credential: the recorder reads none,
 *  and no gateway ever gets this one. */
// fixture-secret-next-line: a placeholder the recorder never reads, and never a credential
const RECORDER_CREDENTIAL = "placeholder-the-recorder-reads-none";
/** The made-up tree and issue a Legion capture's pane claims, and how many times its run is made
 *  when it ends before the turn's request. */
const CAPTURE_TREE = "WORK-1";
const CAPTURE_ISSUE = "WORK-2";
const CAPTURE_TRIES = 3;
/** The made-up session, machine and stream ids the stand-in daemons answer with (the Envoy
 *  listener answers the session id a request names, and STAND_IN_SESSION only when it names
 *  none): no name of the runner's, as no answer holds a path of the capture's. */
const STAND_IN_SESSION = "session-1";
const STAND_IN_MACHINE = "machine-1";
const STAND_IN_STREAM = "stream-1";

/** An Oh My Pi run that ended with no model request a capture can take. */
class RunEnded extends Error {}

/** The plugin that defines a Legion pane's `legion` tool, under the name a profile gives it (its
 *  directory's, as arms.ts's ENVOY_PLUGIN names `pi-envoy`), and how many capture runs an arm of a
 *  pane whose context installs it as an extension gets for a body that defines no `legion` tool:
 *  the first, and one more (the file's header, "The Legion tool"). */
const LEGION_PLUGIN = "pi-legion";
const LEGION_ATTEMPTS = 2;

/** How the drop of a behaviour case begins whose capture threw LegionToolUndefined: the rest of
 *  the drop is the error's message. */
export const LEGION_TOOL_UNDEFINED = "legion-tool-undefined";

/** What a capture throws when the body of each of the LEGION_ATTEMPTS attempts of a Legion pane
 *  whose context installs LEGION_PLUGIN as an extension defines no `legion` tool. Its message
 *  names the case, the arm, the plugin and each attempt's `[legion]`, `Extension error` and `Failed to load extension` lines
 *  of stderr. */
export class LegionToolUndefined extends Error {}

/** The lines of a run's stderr that say what the Legion plugin did: its own (`[legion]`), Oh My
 *  Pi's on an extension whose handler threw (`Extension error`), and its on an extension whose
 *  module it could not load (`Failed to load extension`, as when the plugin refuses to load a
 *  bundle whose package.json holds no `legion.daemonApiVersion`, which a pack writes). */
function legionLines(stderr: string): string[] {
  return stderr
    .split("\n")
    .filter(
      (line) =>
        line.startsWith("[legion]") ||
        line.includes("Extension error") ||
        line.includes("Failed to load extension")
    );
}

/** The names of the tools a body defines, in order. */
function bodyToolNames(body: RequestBody): string[] {
  return Array.isArray(body.tools)
    ? body.tools.flatMap((tool: unknown) => {
        const name = typeof tool === "object" && tool !== null ? Reflect.get(tool, "name") : null;
        return typeof name === "string" ? [name] : [];
      })
    : [];
}

/** The tools a body defines and the `xd://` devices its system prompt lists, the Legion and
 *  Envoy plugins' among them, and each plugin tool left out with why. `role` is the Legion role
 *  of the case's context, when it has one. Exported for capture.test.ts. */
export function pluginTools(
  body: RequestBody,
  context: ContextRecord,
  role: { readonly role: string } | undefined,
  stderr: string
): CaptureRecord["pluginTools"] {
  const tools = bodyToolNames(body);
  const devices = systemTexts(body).flatMap((text) =>
    [...text.matchAll(/^- xd:\/\/([A-Za-z0-9_-]+)/gm)].map((match) => match[1] ?? "")
  );
  const leftOut = context.plugins
    .filter((plugin) => plugin.as === "left out")
    .map((plugin) => `${plugin.name}: ${plugin.reason ?? "left out"}`);
  if (role?.role === "controller") {
    leftOut.push("legion: a controller session is not reproduced, so its tool is not defined");
  } else if (role !== undefined && !tools.includes("legion")) {
    leftOut.push(`legion: the plugin defined no tool: ${legionLines(stderr).join(" ")}`);
  }
  return {
    tools,
    devices,
    legion: tools.includes("legion"),
    envoy: devices.filter((name) => name.startsWith("envoy_")),
    leftOut,
  };
}

/** The built-in tools Oh My Pi defines only in a session with memory, which the capture runs
 *  without: its memory instructions call a live service. */
const MEMORY_TOOLS = ["memory_edit", "retain", "recall", "reflect"];

/**
 * The tools a run's `--tools` named that Oh My Pi 18.18.6 refused, each with why, read from its
 * own words on stderr (`validateToolNames`, packages/coding-agent/src/cli/args.ts at the pin):
 * "Unknown tool(s) in --tools: a, b." for a name nothing the capture loads defines, and "Built-in
 * tool(s) unavailable in this session: a, b." for a built-in it refuses in this session (a memory
 * tool in a session without memory). One refusal can hold both lines.
 */
export function refusedTools(stderr: string): { name: string; reason: string }[] {
  const named = (pattern: RegExp): string[] =>
    pattern
      .exec(stderr)?.[1]
      ?.split(", ")
      .filter((name) => name !== "") ?? [];
  return [
    ...named(/Unknown tools? in --tools: ([^\n]*?)\.?(?:\n|$)/).map((name) => ({
      name,
      reason:
        "neither this Oh My Pi nor a plugin the capture loads defines it, and Oh My Pi refuses a run whose --tools names it",
    })),
    ...named(/Built-in tools? unavailable in this session: ([^\n]*?)\.?(?:\n|$)/).map((name) => ({
      name,
      reason: MEMORY_TOOLS.includes(name)
        ? "Oh My Pi defines it, but refuses it in a session without memory, and the capture runs without memory (its memory instructions call a live service)"
        : "Oh My Pi defines it as a built-in, but refuses it in this session",
    })),
  ];
}

/** The counts the hook's log holds: how many messages of its own run it dropped, per request. */
function hookCounts(file: string): number[] {
  if (!existsSync(file)) return [];
  return readFileSync(file, "utf8")
    .split("\n")
    .filter((line) => line !== "")
    .flatMap((line) => {
      const entry: unknown = JSON.parse(line);
      const dropped =
        typeof entry === "object" && entry !== null ? Reflect.get(entry, "dropped") : null;
      return typeof dropped === "number" ? [dropped] : [];
    });
}
/** The providers Oh My Pi would otherwise try to discover models from. */
const DISABLED_PROVIDERS = [
  "amazon-bedrock",
  "bedrock-mantle",
  "google",
  "google-vertex",
  "ollama",
  "llama.cpp",
  "lm-studio",
];

/** A hint a model wrote: its text, the model that answered, and what the call used. */
export interface HintReply {
  readonly text: string;
  readonly model: string;
  readonly usage?: unknown;
}

/** Answers one of Oh My Pi's hint requests: its body as Oh My Pi built it, and the `anthropic-*`
 *  headers Oh My Pi sent with it. */
export type HintWriter = (
  body: MessagesBody,
  headers: Readonly<Record<string, string>>
) => Promise<HintReply>;

/** One line of the skill listing a session recorded, as a capture lists it in place of a hint
 *  (the file's header, "A recorded listing line"): the name and the text the session listed the
 *  skill under, where the line was recorded (`session`, or `subagent <path hash>`), and, where the
 *  skill the line stands for is not the profile's own repository copy (a load test's tested
 *  skill, whose text the arm gives), the description the session's copy had. */
export interface RecordedLine {
  readonly name: string;
  readonly text: string;
  readonly from: string;
  readonly description?: string;
}

/** Where the line a capture lists a skill under came from, as its record's `hints` names it: the
 *  line its session recorded (RecordedLine's `from` in the brackets), or a hint the hint model
 *  wrote in draw `<n>`. */
type HintSource = `recorded (${string})` | `drawn (draw ${number})`;

/** The `anthropic-*` headers of a recorded request: the API version, and the betas the fields of
 *  Oh My Pi's body need (adaptive thinking's `block_binding`, `context_management` and
 *  `output_config.effort` are refused without theirs). */
export function anthropicHeaders(
  headers: Readonly<Record<string, string>>
): Record<string, string> {
  return Object.fromEntries(
    Object.entries(headers).filter(([name]) => name.startsWith("anthropic-"))
  );
}

/** Hint requests answered by the real model through `gateway`, each body sent with the
 *  `anthropic-*` headers Oh My Pi sent beside it, its spend counted with the rest of the gateway's
 *  and kept in the capture's record (`usage`, gateway.ts's `Spent`). */
export function gatewayHints(gateway: Gateway): HintWriter {
  return async (body, headers) => {
    const reply = await gateway.send(body, { headers });
    const text = reply.message.content
      .map((block) => (block.type === "text" && typeof block.text === "string" ? block.text : ""))
      .join("");
    return { text, model: reply.message.model, usage: reply.spent };
  };
}

export interface CaptureOptions {
  /** The Oh My Pi binary: needed only to make a capture, not to read one back. */
  readonly omp?: string;
  /** Where each body and its record are saved. */
  readonly outDir: string;
  /** Where each case gets its scratch directory, left in place after its capture. */
  readonly scratchDir: string;
  /** The cache of hints and of context.ts's builds. */
  readonly cacheDir: string;
  /** Where the context's repositories are read. */
  readonly source: RepoSource;
  /** A `models.yml` of the context's Oh My Pi. Its providers of the case's model and the hint
   *  model are kept, each pointed at the recorder with a placeholder key. Needed only to make a
   *  capture, not to read one back. */
  readonly models?: string;
  /** Writes the hints the cache lacks. */
  readonly hints: HintWriter;
  /** The model that writes hints (the context's `smol` role), the case's model by default. */
  readonly hintModel?: string;
  /** The lines of the skill listing the case's session recorded, each listed in place of a hint
   *  where it applies (the file's header, "A recorded listing line"); none by default. */
  readonly recordedLines?: readonly RecordedLine[];
  /** The draw of the hint model's hints (the file's header, "The draw"), 1 by default. */
  readonly hintDraw?: number;
  /** Oh My Pi settings files laid over the scratch profile's, as `PI_CONFIG_FILES` names them. */
  readonly configFiles?: readonly string[];
  /** The longest one Oh My Pi run may take. The hint run may take OMP_HINT_TIMEOUT_MS more for
   *  each OMP_HINT_SLOTS hints it is owed (the file's header, "The hint run's bounds"). */
  readonly timeoutMs?: number;
  /** How long the hint run's turn waits with no hint request and no hint answer, HINT_IDLE_MS by
   *  default: a test's shorter wait. It must be more than OMP_HINT_TIMEOUT_MS, or the turn can
   *  end while Oh My Pi still waits for an answer. */
  readonly hintIdleMs?: number;
  /** What the body shows of the session the case stands for, in place of the capture's own
   *  values (`sessionFacts`; the file's header, "The harness strings"). */
  readonly session: SessionFacts;
}

/** What the harness-string rule did to a captured body: what it shows of its session and where
 *  each value came from (SessionFacts), the workstation's OS line of a built prompt (null for a
 *  recorded prompt, which is sent as recorded), how many strings each rule replaced, and the file
 *  of the body as the host sent it. */
export interface HarnessRecord extends SessionFacts {
  readonly os: Sourced | null;
  readonly replaced: readonly { readonly what: string; readonly count: number }[];
  readonly rawBody: string;
}

/** The file a capture keeps the body in as the host sent it, beside the body it saves. */
export function rawBodyFile(bodyFile: string): string {
  return bodyFile.replace(/\.body\.json$/, ".raw.body.json");
}

/** What a capture used and saw. */
export interface CaptureRecord {
  readonly case: string;
  readonly arm: ArmName;
  readonly skill: string;
  readonly capturedAt: string;
  readonly omp: { readonly binary: string; readonly version: string };
  readonly model: string;
  readonly thinking: string;
  readonly hintModel: string;
  readonly configFiles: readonly string[];
  /** The `anthropic-*` headers Oh My Pi sent with the turn's request, which a replay of the body
   *  sends too: none for a Responses body. */
  readonly requestHeaders: Readonly<Record<string, string>>;
  /** The session's digest, from the case, and the capture's own (`promptDigest`). */
  readonly sessionDigest: string | null;
  readonly captureDigest: string;
  /** The case's scratch directory, as the last capture of it left it. */
  readonly scratch: string;
  /** Each skill the body lists, with the text it lists it under. */
  readonly listing: readonly { readonly name: string; readonly hint: string }[];
  /** For a case whose profile carries a recorded system prompt: how many blocks it holds, what
   *  became of the case skill's listing line in this arm, and the file of the blocks sent; or
   *  null. */
  readonly recordedPrompt: {
    readonly blocks: number;
    readonly line: ListingLine;
    readonly file: string;
  } | null;
  /** For a case whose profile carries recorded tool names: those names, the ones `--tools` named
   *  (`sent`, the body's), each recorded one the body does not define, each the body defines
   *  beyond them, and each recorded one left out of `--tools` because nothing the capture loads
   *  defines it (Oh My Pi refuses a run that names one); or null. */
  readonly tools: {
    readonly recorded: readonly string[];
    readonly sent: readonly string[];
    readonly missing: readonly string[];
    readonly extra: readonly string[];
    readonly leftOut: readonly { readonly name: string; readonly reason: string }[];
  } | null;
  /** Each listed skill's line (an installed skill that is not hidden): the text the body lists it
   *  under, the runner's cache key, Oh My Pi's key, whether this capture had it written, and where
   *  the line came from (`source`; the file's header, "A recorded listing line"). For a recorded
   *  line the cache key is that of the key cache, and `written` is false: no model wrote it. Where
   *  Oh My Pi kept no hint of the model's reply (`kept` false), the text is the cut-down
   *  description, Oh My Pi's key is that of its record of the reply, and `reply` and `reason` say
   *  what the model wrote and why Oh My Pi did not keep it. `kept` is absent for a recorded line,
   *  and `kept` and `source` from a capture saved before each field. */
  readonly hints: readonly {
    readonly skill: string;
    readonly hint: string;
    readonly key: string;
    readonly ompKey: string;
    readonly written: boolean;
    readonly source?: HintSource;
    readonly kept?: boolean;
    readonly reply?: string;
    readonly reason?: string;
  }[];
  /** Each hint request sent to `options.hints`. */
  readonly hintCalls: readonly {
    readonly skill: string;
    readonly model: string;
    readonly usage?: unknown;
  }[];
  /** Requests that were neither the turn nor a hint request; each was refused. */
  readonly otherRequests: readonly { readonly path: string; readonly bytes: number }[];
  /** Oh My Pi's own lines on extensions that failed to load. */
  readonly extensionErrors: readonly string[];
  readonly context: ContextRecord;
  /** For a Legion context, the pane environment the capture gave Oh My Pi (made-up keys, every
   *  endpoint a listener of this process on 127.0.0.1), or null. */
  readonly legion: {
    readonly role: string;
    readonly tree: string;
    readonly issue: string;
    readonly endpoints: Readonly<Record<string, string>>;
    readonly dispatch: string;
  } | null;
  /** The endpoints of the stand-in Envoy listener the capture gave Oh My Pi (`ENVOY_URL`, and
   *  `ENVOY_NATS_URL` where the run takes inbound messages), each a listener of this process on
   *  127.0.0.1; null when it gave none. */
  readonly envoy: { readonly endpoints: Readonly<Record<string, string>> } | null;
  /** Every request the capture's listeners took, as `<method> <path>` (the query left out), and
   *  how many connections the NATS sink took: all of them went to 127.0.0.1. */
  readonly connections: { readonly http: readonly string[]; readonly nats: number };
  /** The tools the body defines (`tools`) and the `xd://` devices its system prompt lists, the
   *  plugins' among them, and each plugin tool left out with why. */
  readonly pluginTools: {
    readonly tools: readonly string[];
    readonly devices: readonly string[];
    readonly legion: boolean;
    readonly envoy: readonly string[];
    readonly leftOut: readonly string[];
  };
  /** For a Legion pane whose context installs LEGION_PLUGIN as an extension: each capture run
   *  whose body was checked for the `legion` tool, in order (the file's header, "The Legion
   *  tool"), with how many tools its body defines, whether `legion` is among them, and its
   *  `[legion]`, `Extension error` and `Failed to load extension` lines of stderr. Every other field of the record is the last
   *  attempt's, whose body is saved. Null for any other context; absent from a capture saved
   *  before this field. */
  readonly legionAttempts?:
    | readonly {
        readonly tools: number;
        readonly legion: boolean;
        readonly stderr: readonly string[];
      }[]
    | null;
  /** How many messages the run itself added that the hook dropped, at each model request. */
  readonly hookDropped: readonly number[];
  /** How the capture run ended: its exit code or signal, and why when it was not 0. A run that
   *  ends after the recorder stored the turn's request still gives that request. */
  readonly exit: {
    readonly code: number | null;
    readonly signal: string | null;
    readonly reason: string;
  };
  /** How many Oh My Pi runs the capture took, and how long each part took, in milliseconds. */
  readonly runs: number;
  readonly timings: {
    readonly contextMs: number;
    readonly hintRunMs: number;
    readonly captureRunMs: number;
    readonly totalMs: number;
  };
  /** The harness-string rule's record (HarnessRecord). */
  readonly harness: HarnessRecord;
  /** Each block of the case's skill that reaches the session beside its listing line and its
   *  reads, and where this arm's messages hold it (arms.ts's `Injection`). */
  readonly injections: readonly Injection[];
  /** Whether the case's session had Dispatch, and why (the arm's `dispatch`, arms.ts's
   *  `dispatchOf`): what the plugin's block of `injections` rests on. Null for an arm built from
   *  no case (a load test's); absent from a capture saved before this field. */
  readonly dispatch?: DispatchAnswer | null;
}

/** A saved capture: the body, its record, and their files; `made` is true when this call made it,
 *  false when it read a saved one back. */
export interface Capture {
  readonly body: RequestBody;
  readonly record: CaptureRecord;
  readonly bodyFile: string;
  readonly recordFile: string;
  readonly made: boolean;
}

/** A hint in the runner's cache: the text Oh My Pi lists the skill under, and Oh My Pi's key of
 *  it. Where Oh My Pi kept no hint of the model's reply (`kept` false), the text is the cut-down
 *  description, the key is that of Oh My Pi's record of the reply, and `reply` and `reason` say
 *  what the model wrote and why Oh My Pi did not keep it. A hint cached before `kept` has none: it
 *  was kept. */
interface CachedHint {
  readonly omp: string;
  readonly name: string;
  readonly description: string;
  readonly ompKey: string;
  readonly hint: string;
  readonly model: string;
  readonly writtenAt: string;
  readonly kept?: boolean;
  readonly reply?: string;
  readonly reason?: string;
  /** The draw it was written in, absent for draw 1 (the file's header, "The draw"). */
  readonly draw?: number;
}

/** Oh My Pi's key of a skill text, as the runner's key cache keeps it (`<cacheDir>/hint-keys/`;
 *  the file's header, "A recorded listing line"): the Oh My Pi version, the name, the
 *  description, the key, and when the capture read it off the row Oh My Pi stored. */
interface CachedKey {
  readonly omp: string;
  readonly name: string;
  readonly description: string;
  readonly ompKey: string;
  readonly readAt: string;
}

/** The runner's cache key of the hint of `skill` in draw `draw` under the Oh My Pi `version`
 *  (the file's header, "The draw"): of the version, the name and the description, and of
 *  `draw <n>` too beyond draw 1. */
function hintKey(
  version: string,
  skill: Pick<InstalledSkill, "name" | "description">,
  draw: number
): string {
  return draw === 1
    ? cacheKey(version, skill.name, skill.description)
    : cacheKey(version, skill.name, skill.description, `draw ${draw}`);
}

/** The line of `lines` that the listed skill `skill` is listed under (the file's header, "A
 *  recorded listing line"), or undefined: the line of its name, where that line names the
 *  skill's description, or names none and `from`, the context record's source of the skill, is a
 *  repository copy of the profile. */
function recordedLineOf(
  skill: InstalledSkill,
  from: ContextRecord["skills"][number]["from"] | undefined,
  lines: readonly RecordedLine[]
): RecordedLine | undefined {
  const line = lines.find((each) => each.name === skill.name);
  if (line === undefined) return undefined;
  if (line.description !== undefined) {
    return line.description === skill.description ? line : undefined;
  }
  return typeof from === "object" ? line : undefined;
}

/** Oh My Pi's key of a skill text as the runner read it before (the file's header, "A recorded
 *  listing line"): from the skill text's cached hint of draw 1 (`hintFile`), else from its key
 *  cache entry (`keyFile`); undefined where neither holds it. */
function knownOmpKey(hintFile: string, keyFile: string): string | undefined {
  for (const file of [hintFile, keyFile]) {
    if (!existsSync(file)) continue;
    const saved: unknown = JSON.parse(readFileSync(file, "utf8"));
    const key = typeof saved === "object" && saved !== null ? Reflect.get(saved, "ompKey") : null;
    const found = typeof key === "string" ? OMP_KEY.exec(key)?.[0] : undefined;
    if (found !== undefined) return found;
  }
  return undefined;
}

/** One listed skill's line as a capture chooses it: the line its session recorded (`recorded`),
 *  with Oh My Pi's key of the skill text once known (`ompKey`) and the key cache entry (`key`,
 *  `file`); or a hint of the capture's draw, with its cache entry (`key`, `file`), the cached
 *  hint (`cached`) and whether this capture had it written. */
interface ListedHint {
  readonly skill: InstalledSkill;
  readonly key: string;
  readonly file: string;
  readonly recorded: RecordedLine | undefined;
  ompKey: string | undefined;
  cached: CachedHint | undefined;
  written: boolean;
}

/** The text a capture lists `entry`'s skill under: its recorded line, else its cached hint. */
function listedText(entry: ListedHint | undefined): string | undefined {
  return entry?.recorded?.text ?? entry?.cached?.hint;
}

/** What a capture lists `entry`'s skill under, as a refused body's fault names it. */
function listedUnder(entry: ListedHint): string {
  if (entry.recorded !== undefined) return "the line its session recorded";
  if (entry.cached?.kept === false) {
    return "the cut-down description Oh My Pi lists in place of a hint";
  }
  return "its hint";
}

/** Writes each `[key, description]` of `rows`, in order, into the `skill_descriptions` table of
 *  the Oh My Pi database file `database` (made, and the table, when missing), over any row of the
 *  same key. */
function putDescriptions(database: string, rows: readonly (readonly [string, string])[]): void {
  const db = new Database(database, { create: true });
  try {
    db.run(
      "CREATE TABLE IF NOT EXISTS skill_descriptions (key TEXT PRIMARY KEY, description TEXT NOT NULL)"
    );
    const put = db.prepare("INSERT OR REPLACE INTO skill_descriptions VALUES (?, ?)");
    for (const [key, description] of rows) put.run(key, description);
    put.finalize();
  } finally {
    db.close();
  }
}

/** A row of the `skill_descriptions` table of an Oh My Pi database. */
interface DescriptionRow {
  key: string;
  description: string;
}

/** A connection to an Oh My Pi database file, and its read of the rows of the
 *  `skill_descriptions` table (`rows`); `close` ends it. */
interface DescriptionReader {
  readonly rows: () => DescriptionRow[];
  readonly close: () => void;
}

/** The reader of the Oh My Pi database file `database`: opened once for a hint run, its read
 *  prepared once. Not read-only: a read-only connection can fail to open a database in WAL mode.
 *  The read is a `query` statement, which `close` finalizes, so that the connection is released
 *  at once: one `prepare` makes would keep it open until the statement is collected. */
function descriptionReader(database: string): DescriptionReader {
  const db = new Database(database);
  try {
    const select = db.query<DescriptionRow, []>("SELECT key, description FROM skill_descriptions");
    return { rows: () => select.all(), close: () => db.close() };
  } catch (error) {
    db.close();
    throw error;
  }
}

/**
 * The installed skills Oh My Pi lists, in `context.skills`' order: each that is not hidden, less
 * each copy it drops. Oh My Pi admits the copies of `skillsDir` first, then those of each
 * namespaced directory in the profile's order (`skills.customDirectories`), and drops a copy whose
 * body and frontmatter are those of a copy it admitted before under the same name, which it then
 * neither lists nor asks a hint of (packages/coding-agent/src/extensibility/skills.ts of
 * cybermonklabs/oh-my-pi at v18.18.6-sami.20261119-162325, the build the runner pins, arms.ts,
 * `resolveCollision`: "Any other candidate identical to a registered copy → silently dropped").
 * It compares what discovery/helpers.ts's `scanSkillsFromDir` keeps of each copy (skills.ts and
 * `scanSkillsFromDir` are the same there as at rig.sh's v18.18.6-sami.20261117-013345): the body
 * and the frontmatter of `parseFrontmatter` with its defaults, which are common.ts's
 * `splitFrontmatter` with `repair` (leading HTML comments dropped, the body trimmed) and
 * `readFrontmatter` (its keys camelCase at every depth), compared with
 * `Bun.deepEquals` as Oh My Pi compares them. So a profile that installs one text twice, once
 * under `<namespace>/<name>`, lists it once, under the copy admitted first. Only a name installed
 * more than once can lose a copy, so only the copies of such a name are read.
 */
function listedSkills(
  context: Pick<CaseContext, "skills" | "namespacedDirs">
): readonly InstalledSkill[] {
  const bareName = (skill: InstalledSkill) => skill.name.split("/").at(-1) ?? skill.name;
  const copies = new Map<string, number>();
  for (const skill of context.skills) {
    copies.set(bareName(skill), (copies.get(bareName(skill)) ?? 0) + 1);
  }
  const rank = (skill: InstalledSkill) =>
    context.namespacedDirs.findIndex((dir) => skill.dir.startsWith(`${dir}${path.sep}`));
  const admitted: { name: string; body: string; frontmatter: Record<string, unknown> }[] = [];
  const dropped = new Set<InstalledSkill>();
  // A stable sort: the copies of `skillsDir` (rank -1) first, each namespaced directory's after.
  const twice = context.skills.filter((skill) => (copies.get(bareName(skill)) ?? 0) > 1);
  for (const skill of twice.toSorted((a, b) => rank(a) - rank(b))) {
    const text = ompText(readFileSync(path.join(skill.dir, "SKILL.md")));
    const copy = {
      name: bareName(skill),
      body: splitFrontmatter(text, true).body,
      frontmatter: readFrontmatter(text),
    };
    const same = admitted.some(
      (each) =>
        each.name === copy.name &&
        each.body === copy.body &&
        Bun.deepEquals(each.frontmatter, copy.frontmatter)
    );
    if (same) dropped.add(skill);
    else admitted.push(copy);
  }
  return context.skills.filter((skill) => !skill.hidden && !dropped.has(skill));
}

/** The files of the capture of `caseId`'s arm `arm` under `outDir`. */
export function capturePaths(
  outDir: string,
  caseId: string,
  arm: ArmName
): { readonly body: string; readonly record: string } {
  const stem = path.join(outDir, `${encodeURIComponent(caseId)}.${arm}`);
  return { body: `${stem}.body.json`, record: `${stem}.capture.json` };
}

/** The capture saved at `files` (`capturePaths`), read back, or undefined when either file is
 *  missing. Throws for a capture whose record holds no harness-string record: it was made before
 *  that rule, so its body holds the capture's own paths and date; and, given the `arm` it stands
 *  for, for a capture whose record has no `injections` field while the arm has an injected
 *  block: it was made before arms gave such a block their own text. */
export function savedCapture(
  files: { readonly body: string; readonly record: string },
  arm?: Pick<Arm, "injections">
): Capture | undefined {
  let record: string;
  let body: string;
  try {
    record = readFileSync(files.record, "utf8");
    body = readFileSync(files.body, "utf8");
  } catch (error) {
    if (Reflect.get(Object(error), "code") === "ENOENT") return undefined;
    throw error;
  }
  const parsed = JSON.parse(record) as CaptureRecord;
  if (!Object.hasOwn(parsed, "harness")) {
    throw new Error(
      `the capture ${files.body} was made before the harness-string rule, so its body holds the capture's own paths and date: make it again under a new --out`
    );
  }
  if ((arm?.injections ?? []).length > 0 && !Object.hasOwn(parsed, "injections")) {
    throw new Error(
      `the capture ${files.body} was made before each arm gave its own text to a block of the skill a plugin or an agent definition puts into the session, so its body holds the recorded text: make it again under a new --out`
    );
  }
  return {
    body: JSON.parse(body) as RequestBody,
    record: parsed,
    bodyFile: files.body,
    recordFile: files.record,
    made: false,
  };
}

/**
 * Makes again each installed copy that the record of `capture`, a saved capture of `c`'s arm
 * `arm`, names (context.ts's `installedCopies`) and `options.cacheDir` lacks, before a replay
 * answers a read of another skill from it: the cache is not kept with the captures, so a machine
 * that starts without it holds none of them. A capture whose copies are all in the cache makes
 * nothing and reads no repository. Else the copies are made as the capture made them: the case's
 * context for the arm is built again (context.ts's `buildContext`) under a scratch root of its
 * own, `<scratchDir>/copies/<key>`, removed after, which fills the cache. Refuses (Refused, naming
 * the case, the arm and the first copy that differs) a capture whose copies are not the ones that
 * build gives now (the case's profile names another commit of a copy, say): a replay of it would
 * answer a read of a skill from another copy than the one the capture was made with.
 */
export async function restoreCopies(
  c: ContextCase,
  arm: Arm,
  capture: Pick<Capture, "record" | "recordFile">,
  options: Pick<CaptureOptions, "source" | "cacheDir" | "scratchDir">
): Promise<void> {
  const saved = capture.record.context;
  const absent = () =>
    installedCopies(saved, options.cacheDir).filter(
      (copy) => statSync(copy.dir, { throwIfNoEntry: false })?.isDirectory() !== true
    );
  if (absent().length === 0) return;
  const root = path.join(options.scratchDir, "copies", cacheKey(c.id, arm.name).slice(0, 16));
  let built: ContextRecord;
  try {
    ({ record: built } = await buildContext(c, arm, {
      source: options.source,
      cacheDir: options.cacheDir,
      root,
    }));
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
  // Each repository copy a record names, in the order the context installed them.
  const copies = (record: Pick<ContextRecord, "skills">) =>
    record.skills.flatMap(({ name, from }) =>
      typeof from === "string"
        ? []
        : [`${name} from ${from.path} of ${from.repo} at ${from.commit} (cache key ${from.key})`]
    );
  const was = copies(saved);
  const now = copies(built);
  for (let at = 0; at < Math.max(was.length, now.length); at += 1) {
    if (was[at] === now[at]) continue;
    throw new Refused(
      `case ${c.id} arm ${arm.name}: the saved capture ${capture.recordFile} installed ${was[at] ?? "no copy"} where the case's profile now gives ${now[at] ?? "no copy"}, so a replay of it would answer a read of that skill from another copy than the one it was made with: make it again under a new --out`
    );
  }
  const still = absent()[0];
  if (still !== undefined) {
    throw new Error(
      `case ${c.id} arm ${arm.name}: the installed copy of ${still.name}, ${still.dir}, is not in the cache though the case's context was built again`
    );
  }
}

/** The texts of a body's system prompt, in order: a Messages body's `system` blocks; a Responses
 *  body's `instructions` (the one text Oh My Pi joins its prompt's blocks into), then the text of
 *  each `developer` or `system` message of its `input`. */
function systemTexts(body: unknown): string[] {
  const field = (name: string): unknown =>
    typeof body === "object" && body !== null ? Reflect.get(body, name) : undefined;
  const texts = (content: unknown): string[] => {
    if (typeof content === "string") return [content];
    if (!Array.isArray(content)) return [];
    return content.flatMap((block: unknown) => {
      const text = typeof block === "object" && block !== null ? Reflect.get(block, "text") : null;
      return typeof text === "string" ? [text] : [];
    });
  };
  const system = field("system");
  if (system !== undefined) return texts(system);
  const instructions = field("instructions");
  const input = field("input");
  return [
    ...(typeof instructions === "string" ? [instructions] : []),
    ...(Array.isArray(input) ? input : []).flatMap((item: unknown) => {
      const role = typeof item === "object" && item !== null ? Reflect.get(item, "role") : null;
      return role === "developer" || role === "system"
        ? texts(Reflect.get(Object(item), "content"))
        : [];
    }),
  ];
}

/** Oh My Pi's `system-prompt-digest` of the prompt a body's `system` blocks carry. */
export function promptDigest(body: unknown): string {
  const blocks = systemTexts(body);
  let hash = BigInt(blocks.length);
  for (const block of blocks) hash = BigInt(Bun.hash.wyhash(block, hash));
  return hash.toString(16);
}

/** The skills the `<skills>` listing of the system prompt blocks `blocks` lists (`- <name>:
 *  <text>`), each with the text it is listed under, in order; a name listed twice keeps its first
 *  line. build.ts reads a recorded prompt's listing with it. */
export function listingOf(blocks: readonly string[]): Map<string, string> {
  const listing = new Map<string, string>();
  for (const text of blocks) {
    const block = /<skills>\n([\s\S]*?)<\/skills>/.exec(text)?.[1];
    for (const line of block?.split("\n") ?? []) {
      const match = /^- ([^:\s]+): (.*)$/.exec(line);
      if (match?.[1] !== undefined && match[2] !== undefined && !listing.has(match[1]))
        listing.set(match[1], match[2]);
    }
  }
  return listing;
}

/** The skills a body's system prompt lists (`listingOf` of its system blocks). */
export function skillListing(body: unknown): Map<string, string> {
  return listingOf(systemTexts(body));
}

/** What became of the case skill's line of a recorded skill listing in an arm's prompt. */
export type ListingLine = "replaced" | "removed" | "not listed";

/** The recorded system prompt `blocks` with the first `<skills>` listing line of `skill`
 *  (`- <skill>: <text>`) put through `change`, which gets the line's text and gives the line to
 *  put in its place, or undefined to leave it out; every other byte as recorded. Undefined when no
 *  block lists a line of `skill`. */
function changedListingLine(
  blocks: readonly string[],
  skill: string,
  change: (text: string) => string | undefined
): string[] | undefined {
  const prefix = `- ${skill}: `;
  for (const [index, text] of blocks.entries()) {
    const section = /<skills>\n([\s\S]*?)<\/skills>/.exec(text);
    if (section?.[1] === undefined) continue;
    const start = section.index + "<skills>\n".length;
    const lines = section[1].split("\n");
    const at = lines.findIndex((line) => line.startsWith(prefix));
    if (at === -1) continue;
    const line = change((lines[at] ?? "").slice(prefix.length));
    if (line === undefined) lines.splice(at, 1);
    else lines[at] = line;
    const changed = text.slice(0, start) + lines.join("\n") + text.slice(start + section[1].length);
    return blocks.map((each, n) => (n === index ? changed : each));
  }
  return undefined;
}

/** The recorded system prompt `blocks` as an arm sends them: the first `<skills>` listing line of
 *  `skill` (`- <skill>: <text>`) under `hint`, or left out when `hint` is undefined (the arm lists
 *  no such skill); every other byte as recorded. A recording that lists no line of `skill` is
 *  given back as it is (`not listed`). */
export function armPromptBlocks(
  blocks: readonly string[],
  skill: string,
  hint: string | undefined
): { blocks: string[]; line: ListingLine } {
  const changed = changedListingLine(blocks, skill, () =>
    hint === undefined ? undefined : `- ${skill}: ${hint}`
  );
  if (changed === undefined) return { blocks: [...blocks], line: "not listed" };
  return { blocks: changed, line: hint === undefined ? "removed" : "replaced" };
}

/** The recorded system prompt `blocks` with the first `<skills>` listing line of `from` naming
 *  `to` in its place, its text as recorded, and every other byte as recorded; or the blocks as
 *  recorded when none lists `from`. The pinned Oh My Pi's line is `- <name>: <text>` and names no
 *  path, so the name is all that changes. A load test of a skill under another name (load.ts,
 *  `--name`) gives a recorded prompt this way, and its capture then lists that name under the
 *  hint written for it (`armPromptBlocks`). */
export function renamedListingBlocks(
  blocks: readonly string[],
  from: string,
  to: string
): string[] {
  return changedListingLine(blocks, from, (text) => `- ${to}: ${text}`) ?? [...blocks];
}

/** The recorded system prompt `blocks` less the first `<skills>` listing line of each name of
 *  `names`, every other byte as recorded. An arm that lists a copy under its bare name in place of
 *  `<namespace>/<name>` (arms.ts's `Arm.relisted`, which holds `<namespace>/<name>`) sends its
 *  recording less that line, as Oh My Pi lists that copy under the bare name alone. */
function withoutListingLines(
  blocks: readonly string[],
  names: readonly string[]
): readonly string[] {
  let kept = blocks;
  for (const name of names) kept = changedListingLine(kept, name, () => undefined) ?? kept;
  return kept;
}

// The harness-string rule: what a captured body shows of the session it stands for.

/** The working directory a capture shows when the store holds neither the session's header
 *  `cwd` nor a reminder of it, or when the case or task names no session. */
export const NEUTRAL_CWD = "/work";
/** Claude Code's config directory a capture shows when the configuration names none for the dev
 *  box (`devboxClaudeConfigDir`), or the context is no dev box's. */
export const NEUTRAL_CLAUDE_CONFIG_DIR = "/work/.claude";
/** What a built prompt's workstation `- OS:` line shows: the platform alone, the same for every
 *  arm, where the store records no value of the session's. */
export const NEUTRAL_OS = "linux";

/** Oh My Pi 18.18.6's date and working-directory reminder
 *  (prompts/system/date-cwd-reminder.md), which it puts first in a request's first user turn. */
export function renderReminder(date: string, cwd: string): string {
  return `<system-reminder>\nToday: ${date}; current working directory: '${cwd}'. Do not repeat this information in your reply.\n</system-reminder>`;
}

/** The working directory a reminder names, in group 1. */
const REMINDER_CWD = /current working directory: '([^']*)'\. Do not repeat/;

/** A session's head as session-head.sql reads it from its store and the runner saves it
 *  (`<store>-<pathHash>.json`, `sessionHeadFile`): whether the store holds a file of the hash; the
 *  header's `cwd` and `timestamp` (`start`); the first user message's `timestamp` and
 *  `attribution`; each `date-cwd-reminder` entry Oh My Pi journaled, in file order, its `turn` the
 *  key of the turn it went on (`user:<timestamp>:<attribution>`); the Oh My Pi version the file
 *  records it ran (`runtime`: the `runtime.version` of its first `model_change` line, null for none;
 *  absent from a head saved before it was read, which stays readable); the bytes read (of the
 *  file's first 262144 characters at most) and the lines of them that did not parse (the last, cut
 *  where the head ends); and when it was read. */
export const SessionHead = z.strictObject({
  store: z.string().min(1),
  pathHash: z.string().regex(/^[0-9a-f]{64}$/),
  held: z.boolean(),
  cwd: z.string().nullable(),
  start: z.string().nullable(),
  firstUser: z
    .strictObject({ timestamp: z.number(), attribution: z.string().nullable() })
    .nullable(),
  reminders: z.array(
    z.strictObject({ turn: z.string(), reminder: z.string(), placement: z.string() })
  ),
  runtime: z.string().nullable().optional(),
  bytes: z.int().nonnegative(),
  badLines: z.int().nonnegative(),
  readAt: z.string(),
});
export type SessionHead = z.infer<typeof SessionHead>;

/** A head as the runner holds it: the head and its file. */
export interface SavedHead {
  readonly head: SessionHead;
  readonly file: string;
}

/** A session-head.sql row. */
const HeadRow = z.strictObject({
  hash: z.string(),
  cwd: z.string().nullable(),
  start: z.string().nullable(),
  first_user: z
    .strictObject({ timestamp: z.number().nullable(), attribution: z.string().nullable() })
    .nullable(),
  reminders: z.array(
    z.strictObject({
      turn: z.string().nullable(),
      reminder: z.string().nullable(),
      placement: z.string().nullable(),
    })
  ),
  bytes: z.number().int().nonnegative(),
  bad_lines: z.number().int().nonnegative(),
  runtime: z.string().nullable().optional(),
});

/** The file `dir` keeps the head of the session `pathHash` of the store `store` in, named by both:
 *  two stores can each hold a session at one path, and one `dir` keeps every store's heads. */
export function sessionHeadFile(dir: string, store: string, pathHash: string): string {
  return path.join(dir, `${store}-${pathHash}.json`);
}

/**
 * The heads of the sessions `pathHashes` of the store `store`, by hash: each saved head in `dir`
 * read back, and the others read in one session-head.sql query through the store command
 * `command` (config.ts's `psqlCommand`, which runs it read-only) and each saved there whole
 * before it is given. A head an earlier run saved under its path hash alone (`<pathHash>.json`) is
 * read back too when it is this store's head; another store's is not this one's, so this store
 * reads its own. A hash the store holds no file of gets a head with `held: false`. Throws when the
 * command prints nothing for `quietMs` (it is then ended, and the error names it and the limit),
 * when it fails, when a row or a file saved under this store's name is no head, and when a saved
 * file of either name is not JSON (`JSON.parse`'s error, naming no file).
 */
export async function sessionHeads(
  command: string,
  store: string,
  pathHashes: readonly string[],
  dir: string,
  quietMs = STORE_QUIET_MS
): Promise<Map<string, SavedHead>> {
  const heads = new Map<string, SavedHead>();
  const missing: string[] = [];
  for (const hash of new Set(pathHashes)) {
    const file = sessionHeadFile(dir, store, hash);
    if (existsSync(file)) {
      const parsed = SessionHead.safeParse(JSON.parse(readFileSync(file, "utf8")));
      if (!parsed.success || parsed.data.store !== store || parsed.data.pathHash !== hash) {
        throw new Error(`${file} is not the head of the session ${hash} of the store ${store}`);
      }
      heads.set(hash, { head: parsed.data, file });
      continue;
    }
    const unnamed = path.join(dir, `${hash}.json`);
    const earlier = existsSync(unnamed)
      ? SessionHead.safeParse(JSON.parse(readFileSync(unnamed, "utf8")))
      : undefined;
    if (earlier?.success && earlier.data.store === store && earlier.data.pathHash === hash) {
      heads.set(hash, { head: earlier.data, file: unnamed });
      continue;
    }
    missing.push(hash);
  }
  if (missing.length === 0) return heads;
  // Read as every store read is (psql-rows.ts's `eachStoreRow`): ended once it prints nothing for
  // `quietMs` (STORE_QUIET_MS), each chunk of its answer starting the time again; its exit waited
  // for under the same limit, and its error text only when it exits other than 0.
  const rows = new Map<string, z.infer<typeof HeadRow>>();
  await eachStoreRow(
    command,
    store,
    sessionHeadSql(missing),
    (row) => {
      const parsed = HeadRow.safeParse(row);
      if (!parsed.success)
        throw new Error(`a session-head.sql row is no head: ${JSON.stringify(row)}`);
      rows.set(parsed.data.hash, parsed.data);
    },
    quietMs,
    "read of the session heads"
  );
  const readAt = new Date().toISOString();
  for (const hash of missing) {
    const row = rows.get(hash);
    const first = row?.first_user;
    const head: SessionHead = {
      store,
      pathHash: hash,
      held: row !== undefined,
      cwd: row?.cwd ?? null,
      start: row?.start ?? null,
      firstUser:
        first === undefined || first === null || first.timestamp === null
          ? null
          : { timestamp: first.timestamp, attribution: first.attribution },
      reminders: (row?.reminders ?? []).flatMap(({ turn, reminder, placement }) =>
        turn === null || reminder === null || placement === null
          ? []
          : [{ turn, reminder, placement }]
      ),
      ...(row?.runtime === undefined ? {} : { runtime: row.runtime }),
      bytes: row?.bytes ?? 0,
      badLines: row?.bad_lines ?? 0,
      readAt,
    };
    const file = sessionHeadFile(dir, store, hash);
    writeWhole(file, json(head));
    heads.set(hash, { head, file });
  }
  return heads;
}

/** The key Oh My Pi gives a user turn: `user:<timestamp>:<attribution>`. */
export function userTurnKey(turn: {
  readonly timestamp?: number | undefined;
  readonly attribution?: string | null | undefined;
}): string {
  return `user:${turn.timestamp}:${turn.attribution ?? ""}`;
}

/** One value a capture shows of its session, and where it came from. */
export interface Sourced {
  readonly value: string;
  readonly from: string;
}

/** What a capture shows of the session it stands for, in place of its own run's values: the
 *  working directory, the reminder of the turn it stands for, the date that turn names, and
 *  Claude Code's config directory; each with where it came from, and the head they were read
 *  from (null when the case or task names no session). */
export interface SessionFacts {
  readonly head: {
    readonly store: string;
    readonly pathHash: string;
    readonly file: string;
    readonly held: boolean;
  } | null;
  readonly cwd: Sourced;
  readonly reminder: Sourced;
  readonly date: Sourced;
  readonly claudeConfigDir: Sourced;
}

/**
 * The facts a capture shows of its session (SessionFacts), from its head `saved` (undefined when
 * no session is named). The working directory is the header's `cwd`, else the one the turn's
 * journaled reminder names, else NEUTRAL_CWD. The reminder is the one Oh My Pi journaled for
 * `turn` (its key, `userTurnKey`), byte for byte; else it is rendered from that working directory
 * and `date`, the UTC date of the turn the reminder stands for (`date.value`, with where it came
 * from). Claude Code's config directory is `devboxClaudeConfigDir` for a devbox context that the
 * configuration gives one for, else NEUTRAL_CLAUDE_CONFIG_DIR.
 */
export function sessionFacts(
  saved: SavedHead | undefined,
  options: {
    readonly turn: string | undefined;
    readonly date: Sourced;
    readonly devbox: boolean;
    readonly devboxClaudeConfigDir: string | undefined;
  }
): SessionFacts {
  const head = saved?.head;
  const placed = head?.reminders.find(
    (each) => each.placement === "prefix" && each.turn === options.turn
  );
  const named = placed === undefined ? undefined : REMINDER_CWD.exec(placed.reminder)?.[1];
  const none =
    head === undefined
      ? "neutral: no session is named"
      : head.held
        ? "neutral: the session's head holds neither a header cwd nor a reminder of the turn"
        : "neutral: the store holds no session of this hash";
  const cwd: Sourced =
    head?.cwd !== null && head?.cwd !== undefined
      ? { value: head.cwd, from: "the session header's cwd" }
      : named !== undefined
        ? { value: named, from: "the session's journaled reminder of the turn" }
        : { value: NEUTRAL_CWD, from: none };
  const reminder: Sourced =
    placed !== undefined
      ? { value: placed.reminder, from: `the session's journaled reminder of ${placed.turn}` }
      : {
          value: renderReminder(options.date.value, cwd.value),
          from: `rendered from the working directory (${cwd.from}) and the date (${options.date.from})`,
        };
  const dated =
    placed === undefined ? undefined : /Today: (\d{4}-\d{2}-\d{2});/.exec(placed.reminder)?.[1];
  const config = options.devbox ? options.devboxClaudeConfigDir : undefined;
  return {
    head:
      saved === undefined
        ? null
        : {
            store: saved.head.store,
            pathHash: saved.head.pathHash,
            file: saved.file,
            held: saved.head.held,
          },
    cwd,
    reminder,
    date:
      dated === undefined
        ? options.date
        : { value: dated, from: "the session's journaled reminder of the turn" },
    claudeConfigDir:
      config === undefined
        ? {
            value: NEUTRAL_CLAUDE_CONFIG_DIR,
            from: options.devbox
              ? "neutral: the configuration names no devboxClaudeConfigDir"
              : "neutral: the context is no dev box's",
          }
        : { value: config, from: "the configuration's devboxClaudeConfigDir" },
  };
}

/** `text` as a regular expression that matches it alone. */
function literal(text: string): string {
  return text.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
}

/** One rule of the harness-string rewrite: what it replaces, the pattern, and its replacement. */
export interface HarnessRule {
  readonly what: string;
  readonly pattern: RegExp;
  readonly to: (match: string, ...groups: string[]) => string;
}

/** What a rewrite did: the body, each rule with how many strings it replaced, and each string
 *  that still holds a path of `forbidden`, as `<where>: <path>`. */
export interface Rewritten {
  readonly body: RequestBody;
  readonly replaced: readonly { readonly what: string; readonly count: number }[];
  readonly faults: readonly string[];
}

/**
 * `body` with `rules` applied, in order, to each string inside it, every other byte kept; a part
 * `kept` says (a recorded prompt's blocks) is left as it is. Then each string, a kept part's
 * among them, that still holds a path of `forbidden` is a fault. A rule's replacement is made by
 * a function, so no `$` of a session's text is read as a pattern.
 */
export function rewriteBody(
  body: RequestBody,
  rules: readonly HarnessRule[],
  forbidden: readonly string[],
  kept: (where: readonly (string | number)[], value: unknown) => boolean = () => false
): Rewritten {
  const counts = rules.map(() => 0);
  const faults: string[] = [];
  const walk = (value: unknown, where: (string | number)[], keep: boolean): unknown => {
    const keepHere = keep || kept(where, value);
    if (typeof value === "string") {
      let text = value;
      if (!keepHere) {
        for (const [index, rule] of rules.entries()) {
          text = text.replace(rule.pattern, (match, ...rest: unknown[]) => {
            counts[index] = (counts[index] ?? 0) + 1;
            // The arguments after the groups are the match's offset and the whole text.
            const groups = rest
              .slice(0, -2)
              .map((group) => (typeof group === "string" ? group : ""));
            return rule.to(match, ...groups);
          });
        }
      }
      for (const each of forbidden) {
        if (each !== "" && text.includes(each)) faults.push(`${where.join(".")}: ${each}`);
      }
      return text;
    }
    if (Array.isArray(value)) {
      return value.map((item, index) => walk(item, [...where, index], keepHere));
    }
    if (typeof value === "object" && value !== null) {
      return Object.fromEntries(
        Object.entries(value).map(([key, item]) => [key, walk(item, [...where, key], keepHere)])
      );
    }
    return value;
  };
  const rewritten = walk(body, [], false) as RequestBody;
  return {
    body: rewritten,
    replaced: rules.map((rule, index) => ({ what: rule.what, count: counts[index] ?? 0 })),
    faults,
  };
}

/** Whether the part of a body at `where` is its system prompt: a Messages body's `system`, a
 *  Responses body's `instructions`, and each `developer` or `system` item of its `input`. */
function isSystemPart(where: readonly (string | number)[], value: unknown): boolean {
  if (where.length === 1) return where[0] === "system" || where[0] === "instructions";
  if (where.length !== 2 || where[0] !== "input") return false;
  const role = typeof value === "object" && value !== null ? Reflect.get(value, "role") : undefined;
  return role === "developer" || role === "system";
}

/** The root of the session repository's checkout that holds the session's working directory
 *  `cwd`, given `workingDirectory`, the profile's path of that directory inside the checkout:
 *  `cwd` less that path; or, when `cwd` does not end with it (a neutral working directory), the
 *  directory as many levels above `cwd` as the path has parts. */
export function sessionCheckout(cwd: string, workingDirectory: string): string {
  const dir = cwd.length > 1 ? cwd.replace(/\/+$/, "") : cwd;
  const tail = `/${workingDirectory}`;
  if (dir.endsWith(tail)) return dir.slice(0, -tail.length) || "/";
  return path.posix.resolve(dir, ...workingDirectory.split("/").map(() => ".."));
}

/** A capture's checkout of the session repository (context.ts) that its working directory is in,
 *  below its root: the checkout's directory, and the profile's `workingDirectory` in it. */
export interface CaptureCheckout {
  readonly dir: string;
  readonly workingDirectory: string;
}

/** The rule's pattern for each mention of the capture's checkout `dir`, with the `/` after it when
 *  one follows, and its replacement: the session's checkout `root` there, so a `root` of `/` takes
 *  no second `/`. */
export function checkoutMention(dir: string, root: string): Pick<HarnessRule, "pattern" | "to"> {
  const base = root === "/" ? "" : root;
  return {
    pattern: new RegExp(`${literal(dir)}(/?)`, "g"),
    to: (_match, slash = "") => `${base}${slash}` || "/",
  };
}

/**
 * The harness rules of an Oh My Pi capture that ran in `workspace`, with the scratch Claude Code
 * config directory `claudeConfigDir` (a devbox context's), for the session `facts`: the date and
 * working-directory reminder (any date) becomes the session's; a context file's block path under
 * the working directory, or in the config directory, names the session's; with `builtPrompt`
 * (a prompt Oh My Pi built, not a recorded one), the workstation's `- OS:` line shows NEUTRAL_OS;
 * and every other mention of the working directory names the session's. When the working
 * directory is below the root of the capture's `checkout`, a context file's block path in that
 * checkout, and every other mention of it, names the session's checkout (`sessionCheckout`).
 */
export function ompHarnessRules(
  workspace: string,
  claudeConfigDir: string | undefined,
  facts: SessionFacts,
  builtPrompt: boolean,
  checkout?: CaptureCheckout
): HarnessRule[] {
  const root =
    checkout === undefined ? "" : sessionCheckout(facts.cwd.value, checkout.workingDirectory);
  return [
    {
      what: "the date and working-directory reminder",
      pattern: new RegExp(
        `<system-reminder>\\nToday: \\d{4}-\\d{2}-\\d{2}; current working directory: '${literal(workspace)}'\\. Do not repeat this information in your reply\\.\\n</system-reminder>`,
        "g"
      ),
      to: () => facts.reminder.value,
    },
    {
      what: "a context file's path in the working directory",
      pattern: new RegExp(`<file path="${literal(workspace)}/`, "g"),
      to: () => `<file path="${facts.cwd.value}/`,
    },
    ...(checkout === undefined
      ? []
      : [
          {
            what: "a context file's path in the checkout",
            pattern: new RegExp(`<file path="${literal(checkout.dir)}/`, "g"),
            to: () => `<file path="${root === "/" ? "" : root}/`,
          },
        ]),
    ...(claudeConfigDir === undefined
      ? []
      : [
          {
            what: "the user-level context file's path",
            pattern: new RegExp(`<file path="${literal(claudeConfigDir)}/`, "g"),
            to: () => `<file path="${facts.claudeConfigDir.value}/`,
          },
        ]),
    ...(builtPrompt
      ? [
          {
            what: "the workstation's OS line",
            pattern: /(<workstation>\n- OS: )[^\n]*/g,
            to: (_match: string, line = "") => `${line}${NEUTRAL_OS}`,
          },
        ]
      : []),
    {
      what: "another mention of the working directory",
      pattern: new RegExp(literal(workspace), "g"),
      to: () => facts.cwd.value,
    },
    ...(checkout === undefined
      ? []
      : [
          {
            what: "another mention of the checkout",
            ...checkoutMention(checkout.dir, root),
          },
        ]),
  ];
}

/** `model` with its provider, `anthropic` when it names none. */
function qualified(model: string): string {
  return model.includes("/") ? model : `anthropic/${model}`;
}

/** The skill a request asks a hint for, when it is one of Oh My Pi's hint requests: no system
 *  prompt, no tools, one user message ending `Skill: <name>` and `Description: <text>`. */
function hintSkill(body: Record<string, unknown>): string | undefined {
  if (body.system !== undefined || body.tools !== undefined) return undefined;
  const messages = body.messages;
  if (!Array.isArray(messages) || messages.length !== 1) return undefined;
  const content: unknown = messages[0]?.content;
  const text =
    typeof content === "string"
      ? content
      : Array.isArray(content)
        ? content.map((part) => (typeof part?.text === "string" ? part.text : "")).join("")
        : "";
  return /\nSkill: ([^\n]+)\nDescription: /.exec(text)?.[1];
}

/** Why Oh My Pi keeps no hint of the reply `text`, or undefined when it keeps one: it keeps the
 *  reply, trimmed, when that is not empty, is one line and has at most MAX_HINT_CHARS characters
 *  (skill-descriptions.ts at the pinned tag, `validCompression`). */
function unkeptReason(text: string): string | undefined {
  const line = text.trim();
  if (line === "") return "the reply is empty";
  if (/[\r\n]/.test(line)) return "the reply is more than one line";
  if (line.length > MAX_HINT_CHARS) {
    return `the reply is ${line.length} characters long, and Oh My Pi keeps a hint of at most ${MAX_HINT_CHARS}`;
  }
  return undefined;
}

/** The cut-down description Oh My Pi lists a skill under where it holds no hint of it
 *  (skill-descriptions.ts at the pinned tag, `previewSkillDescription`; the file's header, "A reply
 *  Oh My Pi keeps no hint of"). */
function previewDescription(description: string): string {
  const text = description.replace(/\s+/g, " ").trim();
  if (text.length <= MAX_PREVIEW_CHARS) return text;
  const boundary = text.slice(0, MAX_PREVIEW_CHARS - 1);
  const sentence = /^.*?[.!?](?=\s|$)/.exec(boundary)?.[0];
  if (sentence !== undefined && sentence.length >= MIN_PREVIEW_CUT) return sentence;
  const space = boundary.lastIndexOf(" ");
  const cut = space >= MIN_PREVIEW_CUT ? boundary.slice(0, space).trimEnd() : boundary;
  return `${cut}…`;
}

const versions = new Map<string, Promise<string>>();

/** The longest `omp --version` may run: it took 0.10 s on the pod (Oh My Pi 18.18.6, measured
 *  2026-11-23), so a minute is 600 times that, room for a cold start on a loaded pod. */
export const OMP_VERSION_TIMEOUT_MS = 60_000;

/** What `omp --version` prints for `binary`. A run past `timeoutMs` (OMP_VERSION_TIMEOUT_MS) is
 *  ended (common.ts's `timeLimit`) and fails naming the command and the limit (`pastLimit`).
 *  Exported for capture.test.ts. */
export function ompVersion(binary: string, timeoutMs = OMP_VERSION_TIMEOUT_MS): Promise<string> {
  let version = versions.get(binary);
  if (version === undefined) {
    version = (async () => {
      const child = Bun.spawn([binary, "--version"], { stdout: "pipe", stderr: "ignore" });
      const limit = timeLimit(child, timeoutMs);
      const [text, code] = await Promise.all([textOf(child.stdout, limit), child.exited]);
      limit.clear();
      if (limit.timedOut()) throw new Error(pastLimit(`${binary} --version`, timeoutMs));
      if (code !== 0) throw new Error(`${binary} --version failed with exit ${code}`);
      return text.trim();
    })();
    versions.set(binary, version);
  }
  return version;
}

/** The route a models.yml provider serves its models on, or undefined for neither of the
 *  gateway's: `messages` for `api: anthropic-messages`; `responses` for `api: openai-responses`,
 *  and for the `openai` provider with no `api`, Oh My Pi's built-in one of that API. */
function providerRoute(provider: string, entry: unknown): "messages" | "responses" | undefined {
  const api: unknown = Reflect.get(Object(entry), "api");
  if (api === "anthropic-messages") return "messages";
  if (api === "openai-responses" || (api === undefined && provider === "openai")) {
    return "responses";
  }
  return undefined;
}

/** The scratch profile's `models.yml` and `config.yml` (each JSON, which YAML reads): the
 *  providers of `models` that serve the case's model and the hint model, each pointed at `url`
 *  with the path of its own base URL, and the model roles. Throws when `models` lists no such
 *  model, when the case's model is on neither route, and when the hint model is not on the
 *  Messages route. */
function writeProfile(
  agentDir: string,
  options: {
    readonly url: string;
    readonly models: string;
    readonly model: string;
    readonly hintModel: string;
    readonly skillsDir: string;
    /** The skills directories of the profile's namespaced copies, after `skillsDir`. */
    readonly namespacedDirs: readonly string[];
  }
): void {
  const parsed: unknown = Bun.YAML.parse(readFileSync(options.models, "utf8"));
  const providers: unknown =
    typeof parsed === "object" && parsed !== null ? Reflect.get(parsed, "providers") : undefined;
  if (typeof providers !== "object" || providers === null) {
    throw new Error(`${options.models} holds no providers`);
  }
  const kept: Record<string, unknown> = {};
  for (const [wanted, hint] of [
    [options.model, false],
    [options.hintModel, true],
  ] as const) {
    const [provider = "", id = ""] = wanted.split(/\/(.*)/s);
    const entry: unknown = Reflect.get(providers, provider);
    const models: unknown = Reflect.get(Object(entry), "models");
    const overrides: unknown = Reflect.get(Object(entry), "modelOverrides");
    const listed =
      (Array.isArray(models) && models.some((model) => Reflect.get(Object(model), "id") === id)) ||
      (typeof overrides === "object" && overrides !== null && Object.hasOwn(overrides, id));
    if (!listed) throw new Error(`${options.models} defines no model ${wanted}`);
    const route = providerRoute(provider, entry);
    if (route === undefined) {
      throw new Error(
        `the model ${wanted} is served on neither the Anthropic Messages route nor the OpenAI Responses route`
      );
    }
    if (hint && route !== "messages") {
      throw new Error(
        `the hint model ${wanted} is not served on the Anthropic Messages route, where hints are written: give a hint model that is (--hint-model anthropic/<id>)`
      );
    }
    const { headers: _headers, ...rest } = entry as Record<string, unknown>;
    const base: unknown = rest.baseUrl;
    const sub =
      typeof base === "string" && URL.canParse(base)
        ? new URL(base).pathname.replace(/\/+$/, "")
        : "";
    kept[provider] = {
      ...rest,
      baseUrl: `${options.url}${sub}`,
      auth: "apiKey",
      apiKey: RECORDER_CREDENTIAL,
    };
  }
  mkdirSync(agentDir, { recursive: true });
  writeFileSync(
    path.join(agentDir, "models.yml"),
    `${JSON.stringify({ providers: kept }, null, 2)}\n`
  );
  writeFileSync(
    path.join(agentDir, "config.yml"),
    `${JSON.stringify(
      {
        modelRoles: { default: options.model, smol: options.hintModel },
        disabledProviders: DISABLED_PROVIDERS,
        skills: { customDirectories: [options.skillsDir, ...options.namespacedDirs] },
      },
      null,
      2
    )}\n`
  );
}

/** Oh My Pi's lines on extensions that failed to load: from its log files, the files directly
 *  under `<profileDir>/logs` (a directory there, such as the `http-400-requests/` Oh My Pi writes
 *  a refused request into, is passed over), and from `stderr`. */
export function extensionErrors(profileDir: string, stderr: string): string[] {
  const errors = stderr.split("\n").filter((line) => /extension.*fail|fail.*extension/i.test(line));
  const logs = path.join(profileDir, "logs");
  for (const entry of existsSync(logs) ? readdirSync(logs, { withFileTypes: true }) : []) {
    if (!entry.isFile()) continue;
    for (const line of readFileSync(path.join(logs, entry.name), "utf8").split("\n")) {
      if (line.includes('"Failed to load extension"')) errors.push(line);
    }
  }
  return errors;
}

/** What a capture reads of a case: its id, skill, model and thinking level, its context and
 *  profile, the session's digest, and the evidence context.ts reads whose session it is. A load
 *  test's task (load.ts) is one of these too. */
export type CaptureCase = Pick<
  Case,
  "id" | "skill" | "model" | "thinking" | "context" | "profile" | "sessionDigest" | "evidence"
>;

/**
 * Captures the request body of `c`'s arm `arm`, or reads back the capture saved already. Throws
 * when no capture is saved and `options` gives no Oh My Pi binary or no `models.yml` to make one,
 * when Oh My Pi fails or sends no turn, when a hint cannot be written (the error `options.hints`
 * threw, as it is, the moment it throws: the gateway's BudgetSpent stays one), when the body
 * does not list exactly the arm's installed skills under their recorded lines or their hints, or
 * under the cut-down description where Oh My Pi kept no hint of the model's reply, or, as
 * LegionToolUndefined, when a Legion pane whose context installs LEGION_PLUGIN as an extension
 * defines no `legion` tool in either attempt (the file's header, "The Legion tool"). A reply Oh
 * My Pi does not keep fails no capture (the file's header, "A reply Oh My Pi keeps no hint of").
 */
export async function captureBody(
  c: CaptureCase,
  arm: Arm,
  options: CaptureOptions
): Promise<Capture> {
  const role = legionRole(c.context);
  // A controller session is not reproduced: it claims with a controller secret, not a boot.
  const pane = role !== undefined && role.role !== "controller" ? role : undefined;
  const issue = pane?.isRoot === true ? CAPTURE_TREE : CAPTURE_ISSUE;
  const files = capturePaths(options.outDir, c.id, arm.name);
  const saved = savedCapture(files, arm);
  if (saved !== undefined) {
    await restoreCopies(c, arm, saved, options);
    return saved;
  }
  const { omp, models } = options;
  if (omp === undefined || models === undefined) {
    throw new Error(
      `case ${c.id} arm ${arm.name}: no capture is saved in ${options.outDir}, and making one needs an Oh My Pi binary and a models.yml, which are not given`
    );
  }
  const started = performance.now();
  const model = qualified(c.model);
  const hintModel = qualified(options.hintModel ?? c.model);
  const version = await ompVersion(omp);
  // One directory per case, whatever the arm, so every path Oh My Pi shows is the same in each.
  const root = path.join(options.scratchDir, cacheKey(c.id).slice(0, 16));
  const contextStarted = performance.now();
  const context = await buildContext(c, arm, {
    source: options.source,
    cacheDir: options.cacheDir,
    root,
  });
  const contextMs = Math.round(performance.now() - contextStarted);
  // A subagent whose agent definition autoloads the case's skill got a block of it before its
  // task (arms.ts). Messages that hold none the arm found, and that no compaction summary opens
  // (one that would have dropped it), hold it in a form the runner cannot rebuild: every arm would
  // send the recorded text.
  const agent = context.record.agent;
  if (
    agent?.autoloadSkills.includes(c.skill) === true &&
    !(arm.injections ?? []).some((each) => each.by === "autoload") &&
    arm.messages[0]?.role !== "compactionSummary"
  ) {
    throw new InjectionRefused(
      `case ${c.id}: its agent ${agent.name} (${agent.path} of ${agent.repo} at ${agent.commit}) autoloads ${c.skill}, and its messages hold no autoloaded block of it in the form Oh My Pi writes one, though no compaction summary opens them, so its arms cannot each give it their own text`,
      "autoload-no-block"
    );
  }
  const home = path.join(root, "home");
  const profileDir = path.join(home, ".omp", "profiles", PROFILE);
  const agentDir = path.join(profileDir, "agent");
  rmSync(profileDir, { recursive: true, force: true });
  mkdirSync(agentDir, { recursive: true });
  // Hardlinks to the binary's one copy of its natives (omp-natives.ts), made again for each arm.
  rmSync(path.join(home, ".omp", "natives"), { recursive: true, force: true });
  await linkOmpNatives(omp, home);
  const messagesFile = path.join(root, "messages.json");
  writeWhole(messagesFile, JSON.stringify(arm.messages));

  // Each listed skill's line: the line its session recorded where one applies (`recordedLineOf`;
  // the file's header, "A recorded listing line"), else the hint of this capture's draw, from the
  // cache where it holds one, or the cut-down description where Oh My Pi kept no hint. A hidden
  // skill is not listed, so Oh My Pi writes it no hint, and neither is a copy whose text is that
  // of a copy of its name Oh My Pi admitted first (`listedSkills`).
  const draw = options.hintDraw ?? 1;
  const hintDir = path.join(options.cacheDir, "hints");
  const keyDir = path.join(options.cacheDir, "hint-keys");
  const listed = listedSkills(context);
  const hints = listed.map((skill): ListedHint => {
    const from = context.record.skills.find((each) => each.name === skill.name)?.from;
    const recorded = recordedLineOf(skill, from, options.recordedLines ?? []);
    if (recorded !== undefined) {
      const key = hintKey(version, skill, 1);
      const file = path.join(keyDir, `${key}.json`);
      const ompKey = knownOmpKey(path.join(hintDir, `${key}.json`), file);
      return { skill, key, file, recorded, ompKey, cached: undefined, written: false };
    }
    const key = hintKey(version, skill, draw);
    const file = path.join(hintDir, `${key}.json`);
    const cached = existsSync(file)
      ? (JSON.parse(readFileSync(file, "utf8")) as CachedHint)
      : undefined;
    return { skill, key, file, recorded, ompKey: undefined, cached, written: false };
  });
  const database = path.join(agentDir, "skill-descriptions.db");
  const seeds: [string, string][] = [];
  for (const { recorded, ompKey, cached } of hints) {
    // A recorded line under Oh My Pi's key of its skill text, where the runner knows that key: Oh
    // My Pi lists it and asks no model for it.
    if (recorded !== undefined && ompKey !== undefined) seeds.push([ompKey, recorded.text]);
    if (cached === undefined) continue;
    // Oh My Pi's record of a reply it kept no hint of names the model that wrote it. This run
    // records it under its own hint model, so Oh My Pi asks that model for no hint either.
    const unkept = cached.kept === false ? UNKEPT_ROW.exec(cached.ompKey)?.[0] : undefined;
    seeds.push([unkept === undefined ? cached.ompKey : `${unkept}${hintModel}`, cached.hint]);
  }
  putDescriptions(database, seeds);
  const seeded = new Set(seeds.map(([key]) => key));

  // The stand-in Envoy listener the capture run gets, as the variables that name it. The Envoy
  // plugin defines its tools only when it has a listener to reach, and the real session had one.
  // A Legion pane, and any other context whose profile installs the Envoy plugin (a devbox one
  // among them), get the listener and the NATS sink. A subagent runs inside its parent's process,
  // whose Envoy plugin is on and takes no inbound message for it: its capture gets the listener's
  // URL alone, so the plugin defines its tools and registers nothing. Other contexts get none.
  const subagent = c.context.startsWith("subagent:");
  const envoyPlugin = context.record.plugins.some(
    (plugin) => plugin.name === ENVOY_PLUGIN && plugin.as === "extension"
  );
  const envoyVariables: readonly ("ENVOY_URL" | "ENVOY_NATS_URL")[] = subagent
    ? ["ENVOY_URL"]
    : pane !== undefined || envoyPlugin
      ? ["ENVOY_URL", "ENVOY_NATS_URL"]
      : [];
  // A Legion pane whose context installs the Legion plugin as an extension (the file's header,
  // "The Legion tool"): its body must define that plugin's `legion` tool.
  const legionPane = context.record.plugins.some(
    (plugin) => plugin.name === LEGION_PLUGIN && plugin.as === "extension"
  )
    ? pane
    : undefined;

  // What the recorder does with each request, by the run it is in. The hint run owes each drawn
  // hint the cache lacks, and each recorded line whose key the runner does not know.
  const wanted = new Set(
    hints
      .filter((h) => (h.recorded === undefined ? h.cached === undefined : h.ompKey === undefined))
      .map((h) => h.skill.name)
  );
  const written = new Map<string, HintReply>();
  // Each owed hint whose request reached the recorder, how long its answer took from then, and
  // when the last hint request came or answer went (performance.now()): the hint run's turn ends
  // once `hintIdleMs` pass after that (HINT_IDLE_MS).
  const requested = new Set<string>();
  const answerMs = new Map<string, number>();
  let lastHintEvent = 0;
  const hintIdleMs = options.hintIdleMs ?? HINT_IDLE_MS;
  const hintCalls: CaptureRecord["hintCalls"][number][] = [];
  const otherRequests: CaptureRecord["otherRequests"][number][] = [];
  const turns: { readonly body: string; readonly headers: Record<string, string> }[] = [];
  let hintRun = false;
  /** The hint run's read of Oh My Pi's database (`descriptionReader`), open while the run is. */
  let hintReader: DescriptionReader | undefined;
  // The first error a hint write gave. It ends the capture at once as itself (a BudgetSpent stays
  // one), so that neither Oh My Pi nor the hint wait is blamed for it: the turn waits no more and
  // the run in flight (`ompRun`) is ended.
  let hintFailure: { readonly error: unknown } | undefined;
  let ompRun: { kill(signal: "SIGKILL"): void } | undefined;
  /** Each owed hint the recorder has answered whose row is among `rows` of Oh My Pi's database,
   *  by skill name, with its answer: the row of the text Oh My Pi lists for that answer (`shown`:
   *  the answer trimmed, under a hint's key, where Oh My Pi keeps it; else the cut-down
   *  description, under the key of a reply it kept no hint of, with why it kept none), neither
   *  seeded nor another skill's. */
  const storedRows = (rows: readonly DescriptionRow[]) => {
    const taken = new Set(seeded);
    const found = new Map<
      string,
      {
        readonly row: { key: string; description: string };
        readonly reply: HintReply;
        readonly reason: string | undefined;
        readonly shown: string;
      }
    >();
    for (const entry of hints) {
      const reply = written.get(entry.skill.name);
      if (!wanted.has(entry.skill.name) || reply === undefined) continue;
      // A reply Oh My Pi keeps, it stores under the skill's key. Of any other it keeps no hint,
      // and it records the reply under that key, its rule's revision and the model, with the
      // cut-down description it lists instead. The row it wrote shows which it did.
      const reason = unkeptReason(reply.text);
      const shown =
        reason === undefined ? reply.text.trim() : previewDescription(entry.skill.description);
      const rowKey = reason === undefined ? HINT_ROW : UNKEPT_ROW;
      const row = rows.find(
        (r) => r.description === shown && rowKey.test(r.key) && !taken.has(r.key)
      );
      if (row === undefined) continue;
      taken.add(row.key);
      found.set(entry.skill.name, { row, reply, reason, shown });
    }
    return found;
  };
  /** Whether Oh My Pi has stored the row of every hint the hint run owes. */
  const allStored = (): boolean => {
    if (written.size < wanted.size || hintReader === undefined) return false;
    try {
      return storedRows(hintReader.rows()).size === wanted.size;
    } catch {
      // A read that meets Oh My Pi's write is made again at the next poll.
      return false;
    }
  };
  /** Why the hint run left the hint of `entry` unstored, from what the recorder saw. */
  const unstored = (entry: ListedHint): string => {
    const name = entry.skill.name;
    const reply = written.get(name);
    if (!requested.has(name)) {
      return `Oh My Pi asked for no hint of skill ${name}, and the hint run's turn ended after ${hintIdleMs} ms with no hint request or answer`;
    }
    if (reply === undefined) {
      return `Oh My Pi asked for the hint of skill ${name}, and its answer had not come when the hint run's turn ended, ${hintIdleMs} ms after the last hint request or answer; Oh My Pi gives a hint request up after ${OMP_HINT_TIMEOUT_MS} ms`;
    }
    const took = answerMs.get(name) ?? 0;
    const wrote =
      entry.recorded === undefined
        ? `the model wrote ${JSON.stringify(reply.text)}`
        : `the recorder answered with its recorded line ${JSON.stringify(reply.text)}`;
    if (took > OMP_HINT_TIMEOUT_MS) {
      return `Oh My Pi stored no hint of skill ${name}: ${wrote} ${took} ms after Oh My Pi asked, past the ${OMP_HINT_TIMEOUT_MS} ms Oh My Pi waits for a hint before it gives the request up and asks no more for it in that run`;
    }
    const reason = unkeptReason(reply.text);
    return reason === undefined
      ? `Oh My Pi stored no hint of skill ${name}: ${wrote}, a hint Oh My Pi keeps, ${took} ms after it asked, and its database holds no row of it`
      : `Oh My Pi stored no record of the reply it kept no hint of for skill ${name}: ${wrote} (${reason}) ${took} ms after it asked, and its database holds no row of the cut-down description ${JSON.stringify(previewDescription(entry.skill.description))} under the skill's key and a model`;
  };
  const route = async (request: RecordedRequest): Promise<Response | undefined> => {
    // The stand-in daemon: a made-up registration and its ready, and an empty turn outcome.
    // Every other route of every endpoint is refused.
    if (request.path === "/legion/v1/claims/register" && pane !== undefined) {
      return Response.json({
        claimToken: `claim-${issue.toLowerCase()}-${pane.role}`,
        tree: CAPTURE_TREE,
        issue,
        role: pane.role,
        generation: 1,
        secret: randomBytes(18).toString("base64url"),
        promptAgents: [],
      });
    }
    if (
      request.path === "/legion/v1/claims/ready" ||
      request.path === "/legion/v1/claims/turn-outcome"
    ) {
      return new Response(null, { status: 204 });
    }
    const endpoint = request.path.split("?")[0];
    // The stand-in Envoy listener, for a run given its URL (`envoyVariables`): the session's
    // registration, which the Envoy plugin of a session that takes inbound messages makes as it
    // starts, and which the claim boot of a pane whose plugins claim its role before the first
    // model request made too (cybermonklabs/legion before the claim moved to the session's first
    // turn), is answered with the session's interest and a consumer for it, which every client
    // version parses (one that predates session consumers reads it as core delivery), and the
    // role claim with that interest. The consumer is
    // `@legion/envoy-client/test/subscribe-answer`'s, the one every stand-in listener of this
    // repository answers, so it never drifts from what the client accepts. Its answers name the
    // session's working directory, the session id the request names (the capture run's own, else
    // STAND_IN_SESSION) and made-up machine and stream ids, no path or name of the capture's, so
    // they hold no harness string. Every other Envoy route is refused.
    if (
      envoyVariables.length > 0 &&
      request.method === "POST" &&
      (endpoint === "/envoy/v1/interests/subscribe" || endpoint === "/envoy/v1/roles/set")
    ) {
      const asked: unknown = request.body === "" ? {} : JSON.parse(request.body);
      const named =
        typeof asked === "object" && asked !== null ? Reflect.get(asked, "session_id") : undefined;
      const session = typeof named === "string" ? named : STAND_IN_SESSION;
      return Response.json({
        session_id: session,
        machine_id: STAND_IN_MACHINE,
        dir: options.session.cwd.value,
        topics: [],
        ...(endpoint === "/envoy/v1/interests/subscribe"
          ? { delivery: consumerDelivery(session, { streamID: STAND_IN_STREAM }) }
          : {}),
      });
    }
    if (
      request.method !== "POST" ||
      (endpoint !== "/v1/messages" && endpoint !== "/v1/responses")
    ) {
      return new Response("refused by the capture's stand-in", { status: 503 });
    }
    // A Messages or a Responses body: the turn is one of either API, and a hint request (no
    // system prompt, no tools) is a Messages one, the hint model being on that route.
    const body = JSON.parse(request.body) as MessagesBody;
    const skill = hintSkill(body);
    if (body.tools !== undefined || body.system !== undefined || body.instructions !== undefined) {
      if (!hintRun) {
        turns.push({ body: request.body, headers: anthropicHeaders(request.headers) });
        return undefined;
      }
      // The hint run's turn is held until Oh My Pi has stored the row of each hint it is owed, and
      // ends as soon as it has: the rows themselves, read from its database, say the hints are
      // in. It ends sooner when a hint write fails or the run ends, and once `hintIdleMs` pass
      // with no hint request and no answer, when Oh My Pi asks for no more (HINT_IDLE_MS): the
      // check after the run names each hint left unstored, and why.
      lastHintEvent = performance.now();
      while (
        hintRun &&
        hintFailure === undefined &&
        performance.now() - lastHintEvent < hintIdleMs &&
        !allStored()
      ) {
        await Bun.sleep(HINT_POLL_MS);
      }
      return undefined;
    }
    if (hintRun && skill !== undefined && wanted.has(skill)) {
      // A skill listed under its recorded line is answered with that line, and no model is asked:
      // the row Oh My Pi stores of it gives Oh My Pi's key of its text.
      const line = hints.find((entry) => entry.skill.name === skill)?.recorded;
      const askedAt = performance.now();
      requested.add(skill);
      lastHintEvent = askedAt;
      try {
        const reply: HintReply =
          line === undefined
            ? await options.hints(body, anthropicHeaders(request.headers))
            : { text: line.text, model: body.model };
        if (line === undefined) hintCalls.push({ skill, model: reply.model, usage: reply.usage });
        written.set(skill, reply);
        lastHintEvent = performance.now();
        answerMs.set(skill, Math.round(lastHintEvent - askedAt));
        return sseResponse(
          messageSse({
            id: `msg_hint_${written.size}`,
            model: reply.model,
            content: [{ type: "text", text: reply.text }],
            stopReason: "end_turn",
          })
        );
      } catch (error) {
        hintFailure ??= { error };
        ompRun?.kill("SIGKILL");
        return new Response(`the hint could not be written: ${messageFor(error)}`, {
          status: 502,
        });
      }
    }
    otherRequests.push({ path: request.path, bytes: request.body.length });
    return new Response("the recorder answers only the turn and the hints it is owed", {
      status: 400,
    });
  };

  const recorder = startRecorder({ route });
  // The NATS servers the Envoy plugin dials: a sink that counts each connection and ends it.
  let natsConnections = 0;
  const nats = Bun.listen({
    hostname: "127.0.0.1",
    port: 0,
    socket: {
      open(socket) {
        natsConnections += 1;
        socket.end();
      },
      data() {},
    },
  });
  const endpoints = {
    LEGION_DAEMON_URL: recorder.url,
    ENVOY_URL: `${recorder.url}/envoy`,
    ENVOY_NATS_URL: `nats://127.0.0.1:${nats.port}`,
  };
  // The Envoy endpoints of `envoyVariables`, and the environment of the plugins' stand-ins, each
  // value made up for this capture: for a Legion pane the pane environment a Legion role's Oh My
  // Pi has, and for any other context those Envoy endpoints alone, with no Legion claim.
  const envoyEndpoints: Record<string, string> = Object.fromEntries(
    envoyVariables.map((name) => [name, endpoints[name]])
  );
  /** The pane's state directory, LEGION_STATE_DIR of a Legion pane's capture run. */
  const legionState = path.join(root, "legion-state");
  const standInEnvironment: Record<string, string> =
    pane === undefined
      ? envoyEndpoints
      : {
          LEGION_ROLE: pane.role,
          LEGION_TREE: CAPTURE_TREE,
          LEGION_ISSUE: issue,
          LEGION_STATE_DIR: legionState,
          LEGION_BOOT_TOKEN: randomBytes(18).toString("base64url"),
          DISPATCH_SESSION_ID: STAND_IN_SESSION,
          ...endpoints,
        };
  let stderr = "";
  let runs = 0;
  let exit: CaptureRecord["exit"] = { code: 0, signal: null, reason: "" };
  let hookLog = "";
  // A recorded prompt and tools (a subagent's case): the capture run sends the arm's blocks and
  // only the recorded tools. The blocks file is written once the arm's hint is known.
  const recordedTools = c.profile.tools;
  // What `--tools` names: the recorded tools less each one no loaded part defines.
  let sentToolNames = recordedTools;
  const toolsLeftOut: { readonly name: string; readonly reason: string }[] = [];
  let promptFile: string | undefined;
  /** One Oh My Pi run: the hint run (`hintRun`) or the capture run. The hint run holds no
   *  stand-in environment: the skill listing does not depend on it, and a pane's role claim would
   *  end the run before the hints are written. It holds no recorded prompt or tools either, so Oh
   *  My Pi lists, and asks a hint for, each skill. */
  const runOmp = async (): Promise<number> => {
    const runStarted = performance.now();
    runs += 1;
    const tmp = path.join(root, "tmp");
    const sessions = path.join(root, "sessions");
    rmSync(sessions, { recursive: true, force: true });
    for (const dir of [tmp, sessions]) mkdirSync(dir, { recursive: true });
    const out = path.join(root, `omp-${runs}.out.jsonl`);
    const err = path.join(root, `omp-${runs}.err.txt`);
    hookLog = path.join(root, `hook-${runs}.jsonl`);
    // Each arm of a case runs in the case's one directory: its log holds this run's lines only.
    rmSync(hookLog, { force: true });
    const child = Bun.spawn(
      [
        omp,
        "--profile",
        PROFILE,
        "-p",
        "--mode",
        "json",
        // A Legion pane refuses a session with no transcript on disk; a scratch one serves.
        "--session-dir",
        sessions,
        "--model",
        model,
        "--thinking",
        c.thinking,
        "--hook",
        HOOK,
        ...context.extensions.flatMap((dir) => ["--extension", dir]),
        ...(context.appendSystemPrompt === undefined
          ? []
          : ["--append-system-prompt", context.appendSystemPrompt]),
        ...(sentToolNames === undefined || hintRun ? [] : ["--tools", sentToolNames.join(",")]),
        PLACEHOLDER,
      ],
      {
        cwd: context.workspace,
        env: {
          HOME: home,
          PATH: "/usr/local/bin:/usr/bin:/bin",
          TMPDIR: tmp,
          TZ: "UTC",
          PI_NO_TITLE: "1",
          [MESSAGES_VARIABLE]: messagesFile,
          [HOOK_LOG_VARIABLE]: hookLog,
          ...(options.configFiles === undefined || options.configFiles.length === 0
            ? {}
            : { PI_CONFIG_FILES: options.configFiles.join(":") }),
          ...(hintRun ? {} : standInEnvironment),
          // A devbox context's user-level context file, where Oh My Pi reads one (context.ts).
          ...(context.claudeConfigDir === undefined
            ? {}
            : { CLAUDE_CONFIG_DIR: context.claudeConfigDir }),
          ...(promptFile === undefined || hintRun ? {} : { [SYSTEM_PROMPT_VARIABLE]: promptFile }),
        },
        stdin: "ignore",
        stdout: Bun.file(out),
        stderr: Bun.file(err),
      }
    );
    ompRun = child;
    // The hint run may take OMP_HINT_TIMEOUT_MS more for each OMP_HINT_SLOTS hints it is owed: the
    // longest Oh My Pi's own limits let its hint requests take, so the limit never ends a hint run
    // that is still going. It is far above any run measured: a profile's 115 hints took 181 s on
    // the pod (2026-11-24), where this gives 600 s + 29 x 30 s = 1470 s.
    const timeoutMs =
      (options.timeoutMs ?? RUN_TIMEOUT_MS) +
      (hintRun ? Math.ceil(wanted.size / OMP_HINT_SLOTS) * OMP_HINT_TIMEOUT_MS : 0);
    let timedOut = false;
    const timer = setTimeout(() => {
      timedOut = true;
      child.kill("SIGKILL");
    }, timeoutMs);
    const code = await child.exited;
    clearTimeout(timer);
    ompRun = undefined;
    if (hintFailure !== undefined) throw hintFailure.error;
    stderr = existsSync(err) ? readFileSync(err, "utf8") : "";
    const ended = `${child.signalCode ?? `exit ${code}`}${timedOut ? ` after ${timeoutMs} ms, its time limit` : ""}`;
    const tail = stderr.trim().split("\n").slice(-6).join("\n");
    exit = {
      code: child.signalCode === null ? code : null,
      signal: child.signalCode,
      reason: code === 0 ? "" : tail,
    };
    // A run that ended after the recorder stored the turn's request has given that request.
    if (code !== 0 && (hintRun || turns.length === 0)) {
      throw new RunEnded(`case ${c.id} arm ${arm.name}: Oh My Pi ended with ${ended}: ${tail}`);
    }
    return Math.round(performance.now() - runStarted);
  };

  try {
    writeProfile(agentDir, {
      url: recorder.url,
      models,
      model,
      hintModel,
      skillsDir: context.skillsDir,
      namespacedDirs: context.namespacedDirs,
    });
    let hintRunMs = 0;
    if (wanted.size > 0) {
      const reader = descriptionReader(database);
      let rows: DescriptionRow[];
      hintReader = reader;
      hintRun = true;
      try {
        hintRunMs = await runOmp();
        rows = reader.rows();
      } finally {
        hintRun = false;
        hintReader = undefined;
        reader.close();
      }
      const stored = storedRows(rows);
      // Each recorded line whose key the hint run gave, under that key.
      const learned: [string, string][] = [];
      // Each owed hint Oh My Pi stored no row of. Every hint it did store is kept first, so that
      // a run made again asks for the others alone.
      const unstoredHints: ListedHint[] = [];
      for (const entry of hints) {
        const name = entry.skill.name;
        if (!wanted.has(name)) continue;
        const found = stored.get(name);
        if (found === undefined) {
          unstoredHints.push(entry);
          continue;
        }
        const { row, reply, reason, shown } = found;
        if (entry.recorded !== undefined) {
          // Oh My Pi's key of the skill text, which the key cache keeps; the line goes under it
          // below.
          const ompKey = OMP_KEY.exec(row.key)?.[0] ?? row.key;
          const known: CachedKey = {
            omp: version,
            name,
            description: entry.skill.description,
            ompKey,
            readAt: new Date().toISOString(),
          };
          writeWhole(entry.file, json(known));
          entry.ompKey = ompKey;
          learned.push([ompKey, entry.recorded.text]);
          continue;
        }
        const cached: CachedHint = {
          omp: version,
          name,
          description: entry.skill.description,
          ompKey: row.key,
          hint: shown,
          model: reply.model,
          writtenAt: new Date().toISOString(),
          ...(reason === undefined ? { kept: true } : { kept: false, reply: reply.text, reason }),
          ...(draw === 1 ? {} : { draw }),
        };
        writeWhole(entry.file, json(cached));
        entry.cached = cached;
        entry.written = true;
      }
      const [firstUnstored, ...otherUnstored] = unstoredHints;
      if (firstUnstored !== undefined) {
        const others =
          otherUnstored.length === 0
            ? ""
            : `; and ${otherUnstored.length} more owed hints are unstored (${otherUnstored.map((entry) => entry.skill.name).join(", ")}), each hint stored being kept in the cache`;
        throw new Error(`case ${c.id} arm ${arm.name}: ${unstored(firstUnstored)}${others}`);
      }
      // Each recorded line whose key the hint run gave goes under that key, in place of the row Oh
      // My Pi stored, so Oh My Pi lists exactly that line.
      if (learned.length > 0) putDescriptions(database, learned);
    }
    // A recorded prompt: its blocks with the case skill's listing line as this arm lists it, less
    // the line of each name the arm lists under its bare name in place of the recorded one.
    const recorded =
      c.profile.systemPrompt === undefined
        ? undefined
        : armPromptBlocks(
            withoutListingLines(c.profile.systemPrompt, arm.relisted ?? []),
            c.skill,
            listedText(hints.find((entry) => entry.skill.name === c.skill))
          );
    if (recorded !== undefined) {
      promptFile = path.join(root, `system-prompt.${arm.name}.json`);
      writeWhole(promptFile, JSON.stringify(recorded.blocks));
    }
    // A Legion pane's run can end before its turn's request (its role claim exits the process
    // when it fails), so such a run is made again, at most CAPTURE_TRIES times in all. A run Oh
    // My Pi refuses for its `--tools` (`refusedTools`) is made again without the names it
    // refused, each recorded as left out with why; that refusal counts as no try.
    const captureRun = async (): Promise<number> => {
      for (let tries = 1; ; ) {
        try {
          return await runOmp();
        } catch (error) {
          if (!(error instanceof RunEnded)) throw error;
          const sent = sentToolNames;
          const dropped = refusedTools(stderr).filter(({ name }) => sent?.includes(name));
          if (sent !== undefined && dropped.length > 0) {
            sentToolNames = sent.filter((name) => !dropped.some((tool) => tool.name === name));
            toolsLeftOut.push(...dropped);
            continue;
          }
          tries += 1;
          if (pane === undefined || tries > CAPTURE_TRIES) throw error;
        }
      }
    };
    // The Legion tool (the file's header): a `legionPane` body with no `legion` tool is refused,
    // and the arm is captured again by a fresh run, LEGION_ATTEMPTS attempts in all, each one
    // recorded; the last one's refusal ends the capture.
    const legionAttempts: NonNullable<CaptureRecord["legionAttempts"]>[number][] = [];
    let captureRunMs = await captureRun();
    for (;;) {
      const sent = turns[0];
      if (legionPane === undefined || sent === undefined) break;
      const names = bodyToolNames(JSON.parse(sent.body) as RequestBody);
      const legion = names.includes("legion");
      legionAttempts.push({ tools: names.length, legion, stderr: legionLines(stderr) });
      if (legion) break;
      if (legionAttempts.length === LEGION_ATTEMPTS) {
        const said = legionAttempts.map(
          (each, index) =>
            `attempt ${index + 1}: its body defines ${each.tools} tools, legion not among them, and its stderr's [legion], Extension error and Failed to load extension lines are ${each.stderr.length === 0 ? "none" : each.stderr.map((line) => JSON.stringify(line)).join(" ")}`
        );
        throw new LegionToolUndefined(
          `case ${c.id} arm ${arm.name}: the captured body is refused: a Legion ${legionPane.role} pane whose context installs ${LEGION_PLUGIN} as an extension must send ${LEGION_PLUGIN}'s legion tool, and the body of none of its ${LEGION_ATTEMPTS} attempts defines it: ${said.join("; ")}`
        );
      }
      // The next attempt gets the scratch a first run gets: no turn of this one, and none of its
      // temporary files or pane state (`runOmp` makes its sessions directory again).
      turns.length = 0;
      for (const dir of [path.join(root, "tmp"), legionState]) {
        rmSync(dir, { recursive: true, force: true });
      }
      captureRunMs = await captureRun();
    }
    const turn = turns[0];
    if (turn === undefined) {
      throw new Error(`case ${c.id} arm ${arm.name}: Oh My Pi sent no model request for the turn`);
    }
    const body = JSON.parse(turn.body) as RequestBody;

    // A recorded prompt must be sent as the arm's blocks, byte for byte (in a Responses body, as
    // Oh My Pi joins them into its `instructions`: the blocks not blank, a blank line between).
    // Otherwise the listing must be the installed skills Oh My Pi lists, each under its recorded
    // line or its hint, or under its cut-down description where Oh My Pi kept no hint.
    const listing = skillListing(body);
    const faults: string[] = [];
    if (recorded !== undefined) {
      const sent = systemTexts(body);
      const blocks = isResponsesBody(body)
        ? [recorded.blocks.filter((block) => block.trim() !== "").join("\n\n")]
        : recorded.blocks;
      if (sent.length !== blocks.length) {
        faults.push(`it sends ${sent.length} system blocks, not the ${blocks.length} recorded`);
      }
      for (const [index, block] of blocks.entries()) {
        if (sent[index] !== undefined && sent[index] !== block) {
          faults.push(`its system block ${index} is not the arm's recorded block`);
        }
      }
    } else {
      for (const entry of hints) {
        const { skill } = entry;
        const want = listedText(entry);
        const shown = listing.get(skill.name);
        if (shown === undefined) faults.push(`it does not list ${skill.name}`);
        else if (shown !== want) {
          faults.push(
            `it lists ${skill.name} as ${JSON.stringify(shown)}, not under ${listedUnder(entry)} ${JSON.stringify(want)}`
          );
        }
      }
      const names = new Set(listed.map((skill) => skill.name));
      for (const name of listing.keys()) {
        if (!names.has(name)) faults.push(`it lists ${name}, which the profile does not list`);
      }
    }
    if (faults.length > 0) {
      throw new Error(
        `case ${c.id} arm ${arm.name}: the captured body is refused: ${faults.join("; ")}`
      );
    }
    // The harness-string rule: the body shows the session's working directory, reminder and
    // user-level path, not the capture's, and holds no path of its scratch.
    const facts = options.session;
    const rewritten = rewriteBody(
      body,
      ompHarnessRules(
        context.workspace,
        context.claudeConfigDir,
        facts,
        recorded === undefined,
        c.profile.workingDirectory === undefined
          ? undefined
          : { dir: context.checkout, workingDirectory: c.profile.workingDirectory }
      ),
      [root, options.scratchDir],
      recorded === undefined ? undefined : isSystemPart
    );
    if (rewritten.faults.length > 0) {
      throw new Error(
        `case ${c.id} arm ${arm.name}: the captured body is refused: it still holds the capture's scratch after the harness-string rule, at ${rewritten.faults.join("; ")}`
      );
    }
    const sentBody = rewritten.body;
    const rawFile = rawBodyFile(files.body);
    const defined = pluginTools(sentBody, context.record, role, stderr);
    const sentTools = defined.tools;
    const record: CaptureRecord = {
      case: c.id,
      arm: arm.name,
      skill: c.skill,
      capturedAt: new Date().toISOString(),
      omp: { binary: omp, version },
      model,
      thinking: c.thinking,
      hintModel,
      configFiles: options.configFiles ?? [],
      requestHeaders: turn.headers,
      sessionDigest: c.sessionDigest,
      captureDigest: promptDigest(sentBody),
      scratch: root,
      listing: [...listing].map(([name, hint]) => ({ name, hint })),
      recordedPrompt:
        recorded === undefined || promptFile === undefined
          ? null
          : { blocks: recorded.blocks.length, line: recorded.line, file: promptFile },
      tools:
        recordedTools === undefined
          ? null
          : {
              recorded: recordedTools,
              sent: sentTools,
              missing: recordedTools.filter((name) => !sentTools.includes(name)),
              extra: sentTools.filter((name) => !recordedTools.includes(name)),
              leftOut: toolsLeftOut,
            },
      hints: hints.map(({ skill, key, recorded: line, ompKey, cached, written: was }) =>
        line !== undefined
          ? {
              skill: skill.name,
              hint: line.text,
              key,
              ompKey: ompKey ?? "",
              written: false,
              source: `recorded (${line.from})` as const,
            }
          : {
              skill: skill.name,
              hint: cached?.hint ?? "",
              key,
              ompKey: cached?.ompKey ?? "",
              written: was,
              source: `drawn (draw ${draw})` as const,
              kept: cached?.kept !== false,
              ...(cached?.kept === false ? { reply: cached.reply, reason: cached.reason } : {}),
            }
      ),
      hintCalls,
      otherRequests,
      extensionErrors: extensionErrors(profileDir, stderr),
      context: context.record,
      legion:
        pane === undefined
          ? null
          : {
              role: pane.role,
              tree: CAPTURE_TREE,
              issue,
              endpoints,
              dispatch:
                "off: its read of people's answers on each request would put its failure into the request",
            },
      envoy: envoyVariables.length === 0 ? null : { endpoints: envoyEndpoints },
      connections: {
        http: recorder.requests.map((request) => `${request.method} ${request.path.split("?")[0]}`),
        nats: natsConnections,
      },
      pluginTools: {
        ...defined,
        leftOut: [
          ...defined.leftOut,
          ...toolsLeftOut.map(({ name, reason }) => `${name}: ${reason}`),
        ],
      },
      legionAttempts: legionPane === undefined ? null : legionAttempts,
      hookDropped: hookCounts(hookLog),
      injections: arm.injections ?? [],
      dispatch: arm.dispatch ?? null,
      exit,
      runs,
      timings: {
        contextMs,
        hintRunMs,
        captureRunMs,
        totalMs: Math.round(performance.now() - started),
      },
      harness: {
        ...facts,
        os:
          recorded === undefined
            ? {
                value: NEUTRAL_OS,
                from: "neutral: the store records no workstation of a session whose prompt Oh My Pi builds",
              }
            : null,
        replaced: rewritten.replaced,
        rawBody: rawFile,
      },
    };
    writeWhole(rawFile, turn.body);
    writeWhole(files.body, JSON.stringify(sentBody));
    writeWhole(files.record, json(record));
    return { body: sentBody, record, bodyFile: files.body, recordFile: files.record, made: true };
  } finally {
    await recorder.close();
    nats.stop(true);
  }
}
