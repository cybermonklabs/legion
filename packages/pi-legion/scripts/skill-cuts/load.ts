/**
 * The load test (README.md, "load"): whether a host's agent loads a skill for a task,
 * given what the host lists for the skill. `skill-cuts load` (cli.ts) runs it:
 *
 *     bun scripts/skill-cuts/cli.ts load --config <file> --key-file <file> --skill <dir>
 *         [--name <name>] --tasks <file> --host omp|claude --profile <file> [--runs 3] --out <dir>
 *
 * - `--skill` is the skill's directory as written: its description is the one tested. The skill
 *   is tested under `--name` when one is given, else under its frontmatter `name` (its
 *   directory's name when the frontmatter names none). `--name` changes the frontmatter `name`
 *   and the directory's name in a scratch copy (`renamedSkill`), and, for a profile that carries
 *   a subagent's recorded prompt, the name of that prompt's listing line of the skill
 *   (capture.ts's `renamedListingBlocks`), and nothing else: the skill's own text, its
 *   references, every other line of a recorded prompt and every other file that names the skill
 *   stay as they are, and the command says so.
 * - The tasks file (`loadTasks`) holds one JSON object per line (Task): `{id, text, label,
 *   source: "session", store, pathHash}` for a real session's first request (its store, and
 *   cases.ts's `pathHash` of the transcript's path, never the path), or `{id, text, label,
 *   source: "written"}`; `label` is `need` or `near-miss`. Either may name its `profile`: the
 *   profile file of the session the task came from (absolute, or relative to the tasks file's
 *   directory), so that the task runs in that session's rebuilt context.
 * - A profile file (`loadProfile`) is one JSON object: cases.ts's `Profile` (the other skills
 *   installed beside it, each a `ProfileSkill`; the Oh My Pi plugins; the context files; an
 *   appended system prompt), the `context` (a case's: `devbox`, the default,
 *   `legion:<project>:<role>` or `subagent:<agent>`), and for Claude Code `claude`, the plugin
 *   layout (ClaudeLayout). `--profile` is the profile of each task that names none; a task that
 *   names none when no `--profile` is given is refused.
 *
 * For each task the host's first request is captured once, in the rebuilt context of the task's
 * own profile (its skills, its plugins with capture.ts's stand-ins, its context files, its
 * appended prompt and its context), against the recorder (recorder.ts), so no host process
 * reaches the gateway or runs a tool:
 *
 * - Oh My Pi: capture.ts's `captureBody`, with the task's text as a history of one user message.
 *   Oh My Pi lists each skill under a hint that it has a model write from the name and the
 *   description, so what is tested is that hint; the capture writes it once per Oh My Pi version,
 *   name and description, through the gateway, and keeps it in its cache. A `subagent:<agent>`
 *   profile is captured as capture.ts captures a subagent's case: its recorded prompt with only
 *   the skill's listing line changed, its recorded tools (each one nothing defines left out and
 *   recorded), and the stand-in Envoy listener.
 * - Claude Code: for each profile, a scratch `CLAUDE_CONFIG_DIR` and HOME, into which `claude
 *   plugin install` installs a local marketplace that holds the profile's skills and the tested
 *   skill under its name, each in its plugin; then `claude -p --output-format stream-json
 *   --verbose --model <id> -- <task>` in the context's working directory, with
 *   `ANTHROPIC_BASE_URL` at the recorder. A `devbox` profile's context file is the user-level one
 *   its dev box's host read (build.ts), so it goes in as Claude Code's user memory, `CLAUDE.md` in
 *   the scratch `CLAUDE_CONFIG_DIR`, from its repository at its commit, and not into the working
 *   directory; the record names it (`userMemory`). The first request that holds tools and the
 *   task's text is kept. Claude Code lists each plugin skill as `<plugin>:<name>` with its
 *   description (`claudeListing`). Thinking `off` sets `CLAUDE_CODE_DISABLE_THINKING`, a level of
 *   `claude --effort` goes as that flag, and `auto` leaves Claude Code's own default.
 *
 * Then that one body is sent `--runs` times to the gateway (gateway.ts), with the `anthropic-*`
 * headers the host sent beside it, on the route of its API: an `anthropic/<id>` model's body goes
 * to the Messages route, and an `openai/<id>` model's (Oh My Pi only; its hints still come from an
 * Anthropic `--hint-model`) to the Responses route. A run loads the skill when the first assistant
 * message reads it (`firstMessageLoad`, over the message's tool calls in either API: `tool_use`
 * blocks, or `function_call` items with their arguments parsed): for Oh My Pi, a `read` of
 * `skill://<name>`, `skill://<name>/SKILL.md` or a path ending `/<name>/SKILL.md`; for Claude
 * Code, a `Skill` call naming `<name>` or `<plugin>:<name>`, or a `Read` of a path ending
 * `/<name>/SKILL.md`. These are the reads count.sql counts: a read selector after the last `/`
 * (`:raw`, `:1-40`) is taken off first, and a read of a reference is no load. One tool call of
 * the message that reads the skill is enough, whatever comes before it. A run never goes past that
 * first message: no tool call is answered, so a skill that another skill sends the agent to is
 * never counted as loaded by its own description.
 *
 * A capture shows its task's session, not the run that captured it (capture.ts, "The harness
 * strings"): a task from a session stands for that session, and a written task for the session
 * `--context-session` names, whose context its profile was rebuilt from. That session's head is
 * read from its store once (`<out>/sessions/<pathHash>.json`); the capture shows its working
 * directory and the reminder Oh My Pi journaled for its first user turn (else one rendered for
 * that turn's UTC date), and Claude Code's body its working directory, its config directory's
 * memory path, that date and a neutral OS line (`claudeHarnessRules`). A written task with no
 * `--context-session` names no session: its values are neutral, its date the capture's own.
 *
 * The runs of a profile's tasks are sent together, once each task of it has its capture, in one
 * seeded order (`sendRuns`): no task's runs go as a batch of their own. Each run goes with a
 * session id of its own (gateway.ts's `freshIdentity`), its body otherwise its capture's, since a
 * captured body holds the one id of the host run that made it, and a gateway that routes or
 * caches by that id makes it a factor of every run that sends it.
 *
 * Under `--out`: `summary.json` (the test's settings, what the host lists for the skill, each
 * task's profile file with the SHA-256 of its bytes, its loads and verdict, the skill's verdict,
 * the spend), `runs/<task id>.jsonl` (one JSON line per run, LoadRun, which names its task's
 * profile file and that SHA-256 too, its session id and its place in its send order),
 * `captures/` (each task's body, the body as the host sent it, and its record) and `sessions/`.
 * A task whose run file holds its runs is skipped, so a command started again goes on from what
 * was saved; an `--out` written by a test of other settings (another skill text, name, host,
 * model, thinking level, run count, hint model, or one whose runs were sent before each had its
 * own session id), or holding a capture or runs of a task of the same id with another text,
 * under a profile of other bytes, or that its summary does not list (so the text they were made
 * for is not known), is refused. So is an `openai/<id>` model on Oh My Pi with no `--hint-model`
 * when no task's capture is saved: the hint model would be that model, and hints are written on
 * the Messages route only. The verdicts are score.ts's: a task's is `taskFault` and the skill's
 * `descriptionFault`, given only once every task has its runs and they are LOAD_RUNS. Each
 * task's line ends with the seconds of its phases: its capture (its context build, and the
 * GitHub reads in it with their count), the runs of its profile's tasks, its writes, the rest,
 * and the total.
 */
import { createHash } from "node:crypto";
import {
  cpSync,
  existsSync,
  mkdirSync,
  readFileSync,
  rmSync,
  statSync,
  writeFileSync,
} from "node:fs";
import path from "node:path";
import { messageFor } from "@legion/envoy-client/errors";
import { z } from "zod";
import type { Arm } from "./arms";
import {
  anthropicHeaders,
  type CaptureCase,
  type CaptureRecord,
  captureBody,
  capturePaths,
  type HarnessRecord,
  type HarnessRule,
  type HintWriter,
  rawBodyFile,
  renamedListingBlocks,
  rewriteBody,
  type SavedHead,
  type SessionFacts,
  type Sourced,
  savedCapture,
  sessionFacts,
  sessionHeads,
  userTurnKey,
} from "./capture";
import { Case, Profile, type ProfileSkill, type Thinking } from "./cases";
import { json, Refused, Stopped, schemaFaults, writeWhole } from "./common";
import {
  buildContext,
  type ContextRecord,
  cacheKey,
  type RepoSource,
  readFrontmatter,
  skillFrontmatter,
} from "./context";
import {
  BudgetSpent,
  type ContentBlock,
  freshIdentity,
  type Gateway,
  type MessagesBody,
  PRICES,
  type Reply,
  type RequestBody,
  SpentRecord,
  type ToolCall,
  toolCalls,
} from "./gateway";
import { newSeed } from "./judge";
import { type RecordedRequest, startRecorder } from "./recorder";
import { sendOrder } from "./replay";
import {
  descriptionFault,
  LOAD_RUNS,
  NEAR_MISS_MAX_LOADS,
  NEED_MIN_LOADS,
  TaskLabel,
  taskFault,
} from "./score";

/** The hosts a load test runs in. */
export const Host = z.enum(["omp", "claude"]);
export type Host = z.infer<typeof Host>;
/** A thinking level, as `omp --thinking` takes it (cases.ts's `Thinking`). */
type ThinkingLevel = z.infer<typeof Thinking>;

/** The model a load test runs unless `--model` says otherwise: the one the pinned hosts' fixture
 *  runs used (testdata/CAPTURE.md, F1 to F3). */
export const LOAD_MODEL = "anthropic/claude-sonnet-5-5";
/** Each host's thinking level unless `--thinking` says otherwise, as those fixture runs set it:
 *  Oh My Pi's `--thinking off` (F1, F2), and Claude Code's own default (F3), `auto` here. */
export const LOAD_THINKING: Readonly<Record<Host, ThinkingLevel>> = { omp: "off", claude: "auto" };
/** The fewest tasks of each label a load test is meant to have. A smaller test still gets its
 *  verdict, with a note that says so. */
const MIN_TASKS_PER_LABEL = 5;

/** The longest one Claude Code run may take, by default. */
const RUN_TIMEOUT_MS = 600_000;
/** What a Claude Code run is given as its provider credential: the recorder reads none, and no
 *  gateway ever gets this one. */
const RECORDER_CREDENTIAL = "not-a-secret-the-recorder-reads-none";
/** Claude Code's line before its skill listing. */
const CLAUDE_LISTING = "The following skills are available for use with the Skill tool:";
/** The arm a load test's capture is: the skill's text as written, the description the test
 *  tests (arms.ts's `old`). */
const ARM = "old";
/** A skill, plugin or marketplace name: what can name a directory and a listing entry. */
const NAME = /^[A-Za-z0-9][A-Za-z0-9._-]*$/;
/** A skill file's frontmatter, its text in group 1. */
const FRONTMATTER = /^---\r?\n([\s\S]*?)\r?\n---(\r?\n|$)/;
/** The levels `claude --effort` takes. */
const CLAUDE_EFFORTS: readonly ThinkingLevel[] = ["low", "medium", "high", "xhigh", "max"];
/** The directory under `--out` that keeps each session head a capture read (capture.ts's
 *  `sessionHeads`), one file per path hash. */
const SESSIONS_DIR = "sessions";

const Name = z.string().regex(NAME, { error: "a name of letters, digits, '.', '_' and '-'" });
/** A SHA-256 in hex. */
const Sha256 = z.string().regex(/^[0-9a-f]{64}$/, { error: "a SHA-256 in hex" });

/** Where a task comes from. */
const TaskSource = z.enum(["session", "written"]);

const TaskFields = {
  id: z.string().min(1),
  text: z.string().refine((text) => text.trim() !== "", "is blank"),
  label: TaskLabel,
  /** The profile file of the session the task came from (loadProfile): absolute, or relative to
   *  the tasks file's directory. A task that names none runs under `--profile`. */
  profile: z.string().min(1).optional(),
};
/** One task of a tasks file: a real session's first request, with its store and the hash of its
 *  transcript's path, or a task the reader wrote. */
export const Task = z.discriminatedUnion("source", [
  z.strictObject({
    ...TaskFields,
    source: z.literal("session"),
    store: z.string().min(1),
    pathHash: Sha256,
  }),
  z.strictObject({ ...TaskFields, source: z.literal("written") }),
]);
export type Task = z.infer<typeof Task>;

/** Where Claude Code installs the skills: a local marketplace named `marketplace` whose `plugins`
 *  each hold the profile's skills they name, and `skillPlugin`, the plugin the tested skill goes
 *  in (one of `plugins`, or a plugin of its own). */
const ClaudeLayout = z.strictObject({
  marketplace: Name,
  plugins: z.array(z.strictObject({ name: Name, skills: z.array(Name) })),
  skillPlugin: Name,
});

/** A load test's profile: what the host installs beside the tested skill, and the context it runs
 *  in (a case's; `devbox` unless it names one). */
const LoadProfile = Profile.extend({
  context: Case.shape.context.default("devbox"),
  claude: ClaudeLayout.optional(),
});
type LoadProfile = z.infer<typeof LoadProfile>;

/** One run of one task: whether the first assistant message the gateway gave for the task's body
 *  loaded the skill and by which call, the names of its tool calls in order, its stop reason, the
 *  request id, what it used, what the host lists for the skill (Oh My Pi's hint, or Claude Code's
 *  listed description), the profile file of its task's context with the SHA-256 of its bytes,
 *  the session id it went with (its own: `metadata.user_id`, or a Responses body's
 *  `prompt_cache_key`), and the seed of the send order of its profile's tasks and its place in
 *  that order (from 0). */
export const LoadRun = z.strictObject({
  task: z.string().min(1),
  run: z.int().positive(),
  label: TaskLabel,
  source: TaskSource,
  host: Host,
  name: z.string().min(1),
  loaded: z.boolean(),
  loadedBy: z.string().nullable(),
  toolCalls: z.array(z.string()),
  stopReason: z.string().nullable(),
  requestId: z.string().nullable(),
  usage: SpentRecord,
  listed: z.string(),
  profile: z.string().min(1),
  profileDigest: Sha256,
  userId: z.string().nullable(),
  sendSeed: z.string().min(1),
  sendPlace: z.int().nonnegative(),
});
export type LoadRun = z.infer<typeof LoadRun>;

/** The skill a load test looks for in a reply: its name, and for Claude Code its plugin. */
export interface TestedSkill {
  readonly name: string;
  readonly plugin?: string;
}

/** What the first assistant message of a run did: whether it loaded the skill, the call that did
 *  (`read skill://…`, `Skill <plugin>:<name>`, `Read <path>`), and its tool calls' names. */
interface FirstMessage {
  readonly loaded: boolean;
  readonly loadedBy: string | null;
  readonly toolCalls: readonly string[];
}

/** One task's runs so far: how many were made, how many loaded the skill and which, whether it
 *  has the runs the test asks for, and its verdict (score.ts's `taskFault`), null until it has
 *  them or when they are not LOAD_RUNS. */
interface TaskOutcome {
  readonly made: number;
  readonly loads: number;
  readonly loadedRuns: readonly number[];
  readonly complete: boolean;
  readonly fault: boolean | null;
}

/** The skill's verdict: a description fault (score.ts's `descriptionFault`), none, or no verdict
 *  yet (null); and the reason in words. */
interface LoadVerdict {
  readonly descriptionFault: boolean | null;
  readonly reason: string;
}

/** The tasks of `file`. Throws one refusal that names each line it refuses and why (a line
 *  that is not a task, or an id given twice), or that the file holds no task. */
function loadTasks(file: string): Task[] {
  let text: string;
  try {
    text = readFileSync(file, "utf8");
  } catch (error) {
    throw new Refused(`cannot read the tasks file ${file}: ${messageFor(error)}`);
  }
  const tasks: Task[] = [];
  const refusals: string[] = [];
  for (const [index, line] of text.split("\n").entries()) {
    if (line.trim() === "") continue;
    const where = `${file}:${index + 1}`;
    let raw: unknown;
    try {
      raw = JSON.parse(line);
    } catch (error) {
      refusals.push(`${where} is not JSON: ${messageFor(error)}`);
      continue;
    }
    const parsed = Task.safeParse(raw);
    if (!parsed.success) {
      refusals.push(`${where} is not a task: ${schemaFaults(parsed.error)}`);
      continue;
    }
    if (tasks.some((task) => task.id === parsed.data.id)) {
      refusals.push(`${where}: the task id ${parsed.data.id} is given twice`);
      continue;
    }
    tasks.push(parsed.data);
  }
  if (refusals.length > 0) throw new Refused(`refused tasks:\n${refusals.join("\n")}`);
  if (tasks.length === 0) throw new Refused(`the tasks file ${file} holds no task`);
  return tasks;
}

/** A profile file as a load test read it: its absolute path, the SHA-256 of its bytes, its
 *  profile, and the skill the test looks for under it (for Claude Code, in the plugin its layout
 *  puts the tested skill in). */
interface TaskProfile {
  readonly file: string;
  readonly digest: string;
  readonly profile: LoadProfile;
  readonly tested: TestedSkill;
}

/** The profile of `file`, checked for a test of the skill `name` in `host`. Throws a refusal
 *  naming each fault: a file that is not a profile; a profile skill named `name` (the tested skill
 *  is not one of the other skills) or a name given twice; and for Claude Code a profile with no
 *  plugin layout, or one that puts a profile skill in no plugin or in two, names a skill the
 *  profile does not install, or names a plugin twice. */
function loadProfile(file: string, host: Host, name: string): TaskProfile {
  let bytes: Uint8Array;
  let raw: unknown;
  try {
    bytes = readFileSync(file);
    raw = JSON.parse(new TextDecoder().decode(bytes));
  } catch (error) {
    throw new Refused(`cannot read the profile ${file} as JSON: ${messageFor(error)}`);
  }
  const parsed = LoadProfile.safeParse(raw);
  if (!parsed.success) {
    throw new Refused(`the profile ${file} is refused: ${schemaFaults(parsed.error)}`);
  }
  const profile = parsed.data;
  const faults: string[] = [];
  const names = profile.skills.map((skill) => skill.name);
  if (names.includes(name)) {
    faults.push(`it installs a skill named ${name}, the name the skill is tested under`);
  }
  for (const [index, skill] of names.entries()) {
    if (names.indexOf(skill) !== index) faults.push(`it installs ${skill} twice`);
  }
  const layout = profile.claude;
  if (host === "claude" && layout === undefined) {
    faults.push("it has no `claude` plugin layout, which the Claude Code host needs");
  } else if (host === "claude" && layout !== undefined) {
    const held = layout.plugins.flatMap((plugin) => plugin.skills);
    for (const skill of names) {
      const count = held.filter((each) => each === skill).length;
      if (count !== 1) faults.push(`its layout puts ${skill} in ${count} plugins, not 1`);
    }
    for (const skill of new Set(held)) {
      if (!names.includes(skill))
        faults.push(`its layout names ${skill}, which it does not install`);
    }
    const plugins = layout.plugins.map((plugin) => plugin.name);
    for (const [index, plugin] of plugins.entries()) {
      if (plugins.indexOf(plugin) !== index) faults.push(`its layout names ${plugin} twice`);
    }
  }
  if (faults.length > 0) {
    throw new Refused(`the profile ${file} is refused: ${faults.join("; ")}`);
  }
  const plugin = host === "claude" ? layout?.skillPlugin : undefined;
  return {
    file,
    digest: createHash("sha256").update(bytes).digest("hex"),
    profile,
    tested: plugin === undefined ? { name } : { name, plugin },
  };
}

/** A task and the profile it runs under. */
interface ProfiledTask {
  readonly task: Task;
  readonly profile: TaskProfile;
}

/** Each task of `tasks` in order, with its profile: the file the task names (absolute, or
 *  relative to the tasks file's directory), else `o.profileFile` (`--profile`); each file read
 *  once (`loadProfile`). Throws a refusal naming each task that names no profile when no
 *  `--profile` is given, or a profile's refusal. */
function taskProfiles(
  tasks: readonly Task[],
  o: Pick<LoadOptions, "tasksFile" | "profileFile" | "host">,
  name: string
): ProfiledTask[] {
  const fallback = o.profileFile === undefined ? undefined : path.resolve(o.profileFile);
  const read = new Map<string, TaskProfile>();
  const profiled: ProfiledTask[] = [];
  const lacking: string[] = [];
  for (const task of tasks) {
    const file =
      task.profile === undefined ? fallback : path.resolve(path.dirname(o.tasksFile), task.profile);
    if (file === undefined) {
      lacking.push(task.id);
      continue;
    }
    let profile = read.get(file);
    if (profile === undefined) {
      profile = loadProfile(file, o.host, name);
      read.set(file, profile);
    }
    profiled.push({ task, profile });
  }
  if (lacking.length > 0) {
    const one = lacking.length === 1;
    throw new Refused(
      `${one ? "task" : "tasks"} ${lacking.join(", ")} of ${o.tasksFile} ${one ? "names" : "name"} no profile, and no --profile is given for a task that names none`
    );
  }
  return profiled;
}

/** The frontmatter `name` of the skill in `dir`, read as Oh My Pi reads a skill's frontmatter
 *  (context.ts's `readFrontmatter`, which repairs a plain value that holds `: `), or the
 *  directory's own name when it gives none. */
export function skillName(dir: string): string {
  const name = readFrontmatter(readFileSync(path.join(dir, "SKILL.md"), "utf8")).name;
  return typeof name === "string" && name.trim() !== "" ? name.trim() : path.basename(dir);
}

/**
 * A copy of the skill in `dir` under the name `name`, at `<scratch>/<name>`, replacing one there:
 * the same files byte for byte, but for its SKILL.md's frontmatter `name` line, which becomes
 * `name: <name>` (added at the frontmatter's top when it has none). Nothing else that names the
 * skill changes: its own text, its references, and every file outside it. Throws a refusal
 * when the SKILL.md has no frontmatter.
 */
export function renamedSkill(dir: string, name: string, scratch: string): string {
  const file = path.join(dir, "SKILL.md");
  const text = readFileSync(file, "utf8");
  const open = /^---\r?\n/.exec(text)?.[0];
  const block = FRONTMATTER.exec(text)?.[1];
  if (open === undefined || block === undefined) {
    throw new Refused(`${file} has no frontmatter, so it has no name to change`);
  }
  const named = /^name:.*$/m.test(block)
    ? block.replace(/^name:.*$/m, `name: ${name}`)
    : `name: ${name}\n${block}`;
  const dest = path.join(scratch, name);
  rmSync(dest, { recursive: true, force: true });
  mkdirSync(scratch, { recursive: true });
  cpSync(dir, dest, { recursive: true, verbatimSymlinks: true });
  writeFileSync(
    path.join(dest, "SKILL.md"),
    `${open}${named}${text.slice(open.length + block.length)}`
  );
  return dest;
}

/** Whether `target` (a path or a `skill://` URL) reads the SKILL.md of skill `name`, as count.sql
 *  counts a read: a read selector after the last `/` is taken off first. */
function readsSkillFile(target: string, name: string): boolean {
  const bare = target.replace(/:[^/]*$/, "");
  return (
    bare === `skill://${name}` ||
    bare === `skill://${name}/` ||
    bare === `${name}/SKILL.md` ||
    bare.endsWith(`/${name}/SKILL.md`)
  );
}

/** How the tool call `call` of a reply loads `skill` in `host` (`read <path>`, `Skill <name>`,
 *  `Read <path>`), or undefined when it does not. */
function loadingCall(host: Host, call: ToolCall, skill: TestedSkill): string | undefined {
  const { input } = call;
  const field = (key: string): string | undefined => {
    const value = typeof input === "object" && input !== null ? Reflect.get(input, key) : null;
    return typeof value === "string" ? value : undefined;
  };
  if (host === "omp") {
    const target = call.name === "read" ? field("path") : undefined;
    return target !== undefined && readsSkillFile(target, skill.name)
      ? `read ${target}`
      : undefined;
  }
  if (call.name === "Skill") {
    const named = field("skill") ?? field("command");
    const names = [skill.name];
    if (skill.plugin !== undefined) names.push(`${skill.plugin}:${skill.name}`);
    return named !== undefined && names.includes(named.replace(/^\/+/, ""))
      ? `Skill ${named}`
      : undefined;
  }
  const file = call.name === "Read" ? field("file_path") : undefined;
  return file !== undefined && readsSkillFile(file, skill.name) ? `Read ${file}` : undefined;
}

/** What the first assistant message of a run, the reply's `content` in either API, did with
 *  `skill` in `host` (see the file's header). */
export function firstMessageLoad(
  host: Host,
  content: readonly ContentBlock[],
  skill: TestedSkill
): FirstMessage {
  const calls = toolCalls(content);
  const loadedBy = calls.map((call) => loadingCall(host, call, skill)).find((how) => how) ?? null;
  return {
    loaded: loadedBy !== null,
    loadedBy,
    toolCalls: calls.map((call) => call.name ?? ""),
  };
}

/** The outcome of a task labelled `label` from its `runs`, of the `wanted` the test asks for. */
export function taskOutcome(
  label: TaskLabel,
  runs: readonly LoadRun[],
  wanted: number
): TaskOutcome {
  const loadedRuns = runs.filter((run) => run.loaded).map((run) => run.run);
  const complete = runs.length >= wanted;
  return {
    made: runs.length,
    loads: loadedRuns.length,
    loadedRuns,
    complete,
    fault: complete && wanted === LOAD_RUNS ? taskFault({ label, loads: loadedRuns.length }) : null,
  };
}

/** The skill's verdict from its tasks' outcomes, of `wanted` runs a task. */
export function skillOutcome(
  tasks: readonly {
    readonly id: string;
    readonly label: TaskLabel;
    readonly outcome: TaskOutcome;
  }[],
  wanted: number
): LoadVerdict {
  const lacking = tasks.filter((task) => !task.outcome.complete).map((task) => task.id);
  if (lacking.length > 0) {
    return {
      descriptionFault: null,
      reason: `no verdict yet: tasks ${lacking.join(", ")} lack runs`,
    };
  }
  if (wanted !== LOAD_RUNS) {
    return {
      descriptionFault: null,
      reason: `no verdict: the load test's rule takes ${LOAD_RUNS} runs a task, and this test runs ${wanted}`,
    };
  }
  const fault = descriptionFault(
    tasks.map((task) => ({ label: task.label, loads: task.outcome.loads }))
  );
  const faulty = tasks.filter((task) => task.outcome.fault).map((task) => task.id);
  return {
    descriptionFault: fault,
    reason: fault
      ? `a description fault: tasks ${faulty.join(", ")}`
      : `no description fault in ${tasks.length} tasks`,
  };
}

/** Every text of a request body: its system blocks, and each message's text parts. */
function bodyTexts(body: unknown): string[] {
  const texts: string[] = [];
  const take = (content: unknown): void => {
    if (typeof content === "string") texts.push(content);
    if (!Array.isArray(content)) return;
    for (const part of content) {
      const text = typeof part === "object" && part !== null ? Reflect.get(part, "text") : null;
      if (typeof text === "string") texts.push(text);
    }
  };
  if (typeof body !== "object" || body === null) return texts;
  take(Reflect.get(body, "system"));
  const messages: unknown = Reflect.get(body, "messages");
  for (const message of Array.isArray(messages) ? messages : []) {
    take(typeof message === "object" && message !== null ? Reflect.get(message, "content") : null);
  }
  return texts;
}

/** The skills a Claude Code request body lists for its Skill tool, each name (`<plugin>:<name>`
 *  for a plugin's skill) with its listed description, in order. A description's further lines,
 *  up to the next entry, are kept with it. */
export function claudeListing(body: unknown): Map<string, string> {
  const listing = new Map<string, string>();
  for (const text of bodyTexts(body)) {
    const start = text.indexOf(CLAUDE_LISTING);
    if (start < 0) continue;
    let last: string | undefined;
    const lines = text
      .slice(start + CLAUDE_LISTING.length)
      .replace(/^\n+/, "")
      .split("\n");
    for (const line of lines) {
      if (line.trim() === "") break;
      const entry = /^- ([^\s:]+(?::[^\s:]+)?): (.*)$/.exec(line);
      if (entry?.[1] !== undefined && entry[2] !== undefined) {
        last = entry[1];
        listing.set(last, entry[2]);
      } else if (last !== undefined) {
        listing.set(last, `${listing.get(last)}\n${line}`);
      }
    }
  }
  return listing;
}

/** What a Claude Code capture's `OS Version:` line shows: the system alone, the same for every
 *  task, where the store records no value of the session's. */
const CLAUDE_NEUTRAL_OS = "Linux";

/** `dir` as Claude Code names its project directory under `<config>/projects/`: each character
 *  that is no letter or digit a `-`. */
function claudeProjectName(dir: string): string {
  return dir.replace(/[^A-Za-z0-9]/g, "-");
}

/** `text` as a regular expression that matches it alone, everywhere. */
function literally(text: string): RegExp {
  return new RegExp(text.replace(/[.*+?^${}()|[\]\\]/g, "\\$&"), "g");
}

/**
 * The harness rules of a Claude Code capture (capture.ts, "The harness strings") that ran in
 * `workspace` with its config directory at `configDir`, for the session `facts`: its memory
 * directory's project path (`<config>/projects/<the working directory, its non-alphanumerics as
 * -s>`) names the session's config directory and working directory; its `Today's date is …`
 * line the session's date; its `OS Version:` line CLAUDE_NEUTRAL_OS; and every other mention of
 * the config directory or the working directory the session's.
 */
export function claudeHarnessRules(
  workspace: string,
  configDir: string,
  facts: SessionFacts
): HarnessRule[] {
  const config = facts.claudeConfigDir.value;
  const cwd = facts.cwd.value;
  return [
    ...(configDir === ""
      ? []
      : [
          {
            what: "the memory directory's project path",
            pattern: literally(`${configDir}/projects/${claudeProjectName(workspace)}`),
            to: () => `${config}/projects/${claudeProjectName(cwd)}`,
          },
        ]),
    {
      what: "the date line",
      pattern: /Today's date is \d{4}-\d{2}-\d{2}\./g,
      to: () => `Today's date is ${facts.date.value}.`,
    },
    {
      what: "the OS version line",
      pattern: /( - OS Version: )[^\n]*/g,
      to: (_match, lead = "") => `${lead}${CLAUDE_NEUTRAL_OS}`,
    },
    ...(configDir === ""
      ? []
      : [{ what: "the config directory", pattern: literally(configDir), to: () => config }]),
    { what: "the working directory", pattern: literally(workspace), to: () => cwd },
  ];
}

/** The files of the capture of task `taskId` in `host` under `out`. */
export function loadCapturePaths(
  out: string,
  host: Host,
  taskId: string
): { readonly body: string; readonly record: string } {
  const dir = path.join(out, "captures");
  if (host === "omp") return capturePaths(dir, taskId, ARM);
  const stem = path.join(dir, `${encodeURIComponent(taskId)}.claude`);
  return { body: `${stem}.body.json`, record: `${stem}.capture.json` };
}

/** The run file of task `taskId` under `out`. */
export function loadRunFile(out: string, taskId: string): string {
  return path.join(out, "runs", `${encodeURIComponent(taskId)}.jsonl`);
}

/** What a Claude Code capture used and saw. */
interface ClaudeCaptureRecord {
  readonly task: string;
  readonly capturedAt: string;
  readonly claude: { readonly binary: string; readonly version: string };
  readonly model: string;
  readonly thinking: ThinkingLevel;
  readonly skill: TestedSkill;
  /** The profile file the capture's context was built from, and the SHA-256 of its bytes. */
  readonly profile: string;
  readonly profileDigest: string;
  /** The request's path, and the `anthropic-*` headers Claude Code sent with it, which each run
   *  sends with the body. */
  readonly requestPath: string;
  readonly requestHeaders: Readonly<Record<string, string>>;
  /** The tested skill's listed description, and every skill the body lists. */
  readonly listed: string;
  readonly listing: readonly { readonly name: string; readonly description: string }[];
  /** The other requests the run sent, each answered by the recorder. */
  readonly otherRequests: readonly { readonly path: string; readonly bytes: number }[];
  readonly context: ContextRecord;
  /** The profile's user-level context file Claude Code read as user memory (`CLAUDE.md` in its
   *  config directory), or null when the profile has none. */
  readonly userMemory: UserMemory | null;
  readonly runMs: number;
  /** The harness-string rule's record (capture.ts's HarnessRecord). */
  readonly harness: HarnessRecord;
}

/** A task's captured body, the headers it goes with, what the host lists for the skill, and the
 *  milliseconds its context build took (0 when the capture was read back, or the context was
 *  built for an earlier task). */
interface Captured {
  readonly body: RequestBody;
  readonly headers: Readonly<Record<string, string>>;
  readonly listed: string;
  readonly contextMs: number;
}

/** The scratch Claude Code home of one test: the environment each run gets, the working
 *  directory, the appended system prompt, Claude Code's version, the context's record, the user
 *  memory file written, and the milliseconds its context build took. */
interface ClaudeSetup {
  readonly env: Record<string, string>;
  readonly workspace: string;
  readonly prompt: string | undefined;
  readonly version: string;
  readonly context: ContextRecord;
  readonly userMemory: UserMemory | null;
  readonly contextMs: number;
}

/** The context file of a profile that Claude Code reads as user memory: where it comes from. */
interface UserMemory {
  readonly repo: string;
  readonly commit: string;
  readonly path: string;
}

/** The milliseconds each phase of a task took: its capture (made or read back), the context
 *  build within it, the GitHub reads made in it (how many, and the milliseconds one ran), the
 *  runs of its profile's tasks, sent together, and the writes of its run file and the summary. */
interface TaskPhases {
  readonly captureMs: number;
  readonly contextMs: number;
  readonly github: { readonly calls: number; readonly ms: number };
  readonly runsMs: number;
  writeMs: number;
}

/** A task whose capture is made, ready for its runs: the task, its profile, its runs saved so
 *  far, its capture, and its capture's phases. */
interface ReadyTask {
  readonly task: Task;
  readonly profile: TaskProfile;
  readonly saved: readonly LoadRun[];
  readonly captured: Captured;
  readonly captureMs: number;
  readonly github: { readonly calls: number; readonly ms: number };
}

/** What a load test reads and writes beside its inputs. */
interface LoadIo {
  /** Writes `text` to standard output as it is. */
  out(text: string): void;
  /** Writes one line to standard error. */
  err(line: string): void;
  /** The process's peak resident memory so far, in bytes. */
  peakRssBytes(): number;
}

interface LoadOptions {
  readonly io: LoadIo;
  readonly host: Host;
  /** The skill's directory as written. */
  readonly skillDir: string;
  /** A digest of that directory's files, which the settings of `--out` hold. */
  readonly skillDigest: string;
  /** The name it is tested under, when not its own. */
  readonly name?: string;
  readonly tasksFile: string;
  /** The profile of each task that names none (`--profile`), when one is given. */
  readonly profileFile?: string;
  readonly runs: number;
  readonly out: string;
  /** The model (`anthropic/<id>`, or `<id>`) and the thinking level. */
  readonly model: string;
  readonly thinking: ThinkingLevel;
  /** Where the runs go: counted and logged. */
  readonly gateway: Gateway;
  /** Writes Oh My Pi's hints (capture.ts's `gatewayHints`). */
  readonly hints: HintWriter;
  /** Where the profile's skills, plugins and context files are read. */
  readonly source: RepoSource;
  /** The scratch directory, and the cache of context.ts's builds and of the hints. */
  readonly scratch: string;
  readonly cacheDir: string;
  /** Oh My Pi: its binary (only where a capture is made: none is given when every one is read
   *  back); the `models.yml` a capture needs (a missing capture fails without it); the model that
   *  writes hints; settings files laid over the scratch profile's. */
  readonly omp?: string;
  readonly models?: string;
  readonly hintModel?: string;
  readonly configFiles: readonly string[];
  /** Claude Code's binary. */
  readonly claude: string;
  /** The request log, named in the summary. */
  readonly requestLog: string;
  /** The most the peak resident memory may reach: a task that ends above it stops the test. */
  readonly memoryLimitBytes: number;
  /** The store command (config.ts's `psqlCommand`) and the stores it may read: a capture reads
   *  the head of its task's session through it (capture.ts's `sessionHeads`). */
  readonly psqlCommand: string;
  readonly stores: readonly string[];
  /** The dev box's CLAUDE_CONFIG_DIR (config.ts's `devboxClaudeConfigDir`), when the
   *  configuration names one. */
  readonly devboxClaudeConfigDir?: string;
  /** The session a written task stands for (`--context-session`): the session whose context
   *  its profile was rebuilt from. */
  readonly contextSession?: { readonly store: string; readonly pathHash: string };
}

/** What the test came to: each task or run that failed. */
interface LoadResult {
  readonly failures: readonly string[];
}

/** How a test's runs are sent, as its settings record it: each run with a session id of its own,
 *  and the runs of a profile's tasks in one seeded order. An `--out` whose settings do not record
 *  it holds runs that went with their capture's one id, task after task, so it is refused. */
const SENDS = "each run its own session id; a profile's tasks interleaved in one seeded order";

/** The settings of a test that an `--out` holds: a test of other settings is refused there. */
interface Settings {
  readonly skillText: string;
  readonly name: string;
  readonly renamedFrom: string | null;
  readonly host: Host;
  readonly model: string;
  readonly thinking: ThinkingLevel;
  readonly runs: number;
  readonly hintModel: string | null;
  readonly sends: typeof SENDS;
}

/** A test's summary, as `summary.json` holds it. Only what a later command checks is read back. */
const SavedSummary = z.looseObject({
  settings: z.unknown(),
  tasks: z.array(
    z.looseObject({ id: z.string(), text: z.string(), profileDigest: z.string().optional() })
  ),
});

/** Runs `argv` to its end with `env` and returns its standard output; throws, naming it and its
 *  last output lines, when it fails. */
async function mustRun(argv: readonly string[], env: Record<string, string>): Promise<string> {
  const child = Bun.spawn([...argv], { env, stdin: "ignore", stdout: "pipe", stderr: "pipe" });
  const [stdout, stderr, code] = await Promise.all([
    new Response(child.stdout).text(),
    new Response(child.stderr).text(),
    child.exited,
  ]);
  if (code !== 0) {
    const tail = `${stdout}\n${stderr}`.trim().split("\n").slice(-12).join("\n");
    throw new Error(`\`${argv.join(" ")}\` failed with exit ${code}: ${tail}`);
  }
  return stdout;
}

/** The dollars the hint calls of an Oh My Pi capture's record cost (each call's `usage` is
 *  gateway.ts's `Spent`). */
function hintUsd(record: CaptureRecord): number {
  return record.hintCalls.reduce((sum, call) => {
    const usd: unknown =
      typeof call.usage === "object" && call.usage !== null ? Reflect.get(call.usage, "usd") : 0;
    return sum + (typeof usd === "number" ? usd : 0);
  }, 0);
}

/** One load test, as `runLoad` runs it: its options, settings, scratch root and the tested
 *  skill's directory. */
class LoadTest {
  /** Claude Code's scratch home of each profile, by the SHA-256 of the profile's file. */
  private readonly claudeSetups = new Map<string, Promise<ClaudeSetup>>();

  constructor(
    readonly o: LoadOptions,
    readonly settings: Settings,
    readonly root: string,
    readonly skillDir: string
  ) {}

  say(line: string): void {
    this.o.io.out(`load: ${line}\n`);
  }

  /** The case a capture of `id` reads in the context of `profile`, and the arm it installs: the
   *  profile's skills and the tested skill from its directory, with `text` as the one message,
   *  and the rest of the profile as it is (its plugins, context files, appended prompt, and a
   *  subagent's recorded prompt and tools), but that a recorded prompt of a skill tested under
   *  another name lists it under that name (`renamedListingBlocks`), as the scratch copy's
   *  frontmatter and directory name it. The tested skill's entry names the local directory:
   *  context.ts installs an arm's own skill from `skillDir` and reads no repository for it. */
  caseOf(id: string, text: string, profile: LoadProfile): { c: CaptureCase; arm: Arm } {
    const { name, renamedFrom, model, thinking } = this.settings;
    const own: ProfileSkill = {
      repo: "local/skill-directory",
      commit: this.settings.skillText,
      path: this.skillDir,
      name,
    };
    const { context, claude: _claude, systemPrompt, ...rest } = profile;
    const recorded =
      systemPrompt === undefined || renamedFrom === null
        ? systemPrompt
        : renamedListingBlocks(systemPrompt, renamedFrom, name);
    const caseProfile: Profile = {
      ...rest,
      ...(recorded === undefined ? {} : { systemPrompt: recorded }),
      skills: [...profile.skills, own],
    };
    return {
      c: {
        id,
        skill: name,
        model,
        thinking,
        context,
        profile: caseProfile,
        sessionDigest: null,
      },
      arm: {
        name: ARM,
        skill: name,
        skillDir: this.skillDir,
        installedDir: path.join("/skills", name),
        messages: [
          {
            role: "user",
            content: [{ type: "text", text }],
            attribution: "user",
            timestamp: Date.now(),
          },
        ],
        profileSkills: caseProfile.skills,
      },
    };
  }

  /** What a capture of `task` shows of the session it stands for (capture.ts's `sessionFacts`):
   *  the head of the task's own session, or for a written task of `--context-session`, read once
   *  from its store and saved under `<out>/sessions/`; the reminder Oh My Pi journaled for that
   *  session's first user turn, else one rendered for that turn's UTC date (the session's start's
   *  when its head holds no user turn). A written task with no `--context-session` names no
   *  session, so its values are neutral and its date the capture's own. Throws when the store is
   *  not the configuration's, or the head cannot be read. */
  async sessionOf(task: Task, profile: TaskProfile): Promise<SessionFacts> {
    const named = task.source === "session" ? task : this.o.contextSession;
    let saved: SavedHead | undefined;
    if (named !== undefined) {
      if (!this.o.stores.includes(named.store)) {
        throw new Error(`the configuration names no store ${named.store}`);
      }
      const heads = await sessionHeads(
        this.o.psqlCommand,
        named.store,
        [named.pathHash],
        path.join(this.o.out, SESSIONS_DIR)
      );
      saved = heads.get(named.pathHash);
    }
    const first = saved?.head.firstUser ?? null;
    const start = saved?.head.start ?? null;
    const utc = (at: number | string) => new Date(at).toISOString().slice(0, 10);
    const date: Sourced =
      first !== null
        ? { value: utc(first.timestamp), from: "the UTC date of the session's first user turn" }
        : start !== null
          ? {
              value: utc(start),
              from: "the UTC date of the session's start (its head holds no user turn)",
            }
          : {
              value: utc(Date.now()),
              from:
                saved === undefined
                  ? "neutral: the capture's own date, the task names no session"
                  : "neutral: the capture's own date, the store holds no start of the session",
            };
    return sessionFacts(saved, {
      turn: first === null ? undefined : userTurnKey(first),
      date,
      devbox: profile.profile.context === "devbox",
      devboxClaudeConfigDir: this.o.devboxClaudeConfigDir,
    });
  }

  /** Task `task`'s body through Oh My Pi in the context of `profile`, captured or read back. */
  async ompCapture(task: Task, profile: TaskProfile): Promise<Captured> {
    const files = loadCapturePaths(this.o.out, "omp", task.id);
    const { models } = this.o;
    let capture = savedCapture(files);
    if (capture === undefined && models !== undefined) {
      const { c, arm } = this.caseOf(task.id, task.text, profile.profile);
      capture = await captureBody(c, arm, {
        omp: this.o.omp,
        outDir: path.dirname(files.body),
        scratchDir: path.join(this.root, "captures"),
        cacheDir: this.o.cacheDir,
        source: this.o.source,
        models,
        hints: this.o.hints,
        ...(this.o.hintModel === undefined ? {} : { hintModel: this.o.hintModel }),
        configFiles: this.o.configFiles,
        session: await this.sessionOf(task, profile),
      });
    }
    if (capture === undefined) {
      throw new Error(
        `it has no capture in ${path.dirname(files.body)}, and no --models is given to make one`
      );
    }
    const { name } = profile.tested;
    const listed = capture.record.listing.find((entry) => entry.name === name)?.hint;
    if (listed === undefined) throw new Error(`its capture does not list ${name}`);
    return {
      body: capture.body,
      headers: capture.record.requestHeaders,
      listed,
      contextMs: capture.made ? capture.record.timings.contextMs : 0,
    };
  }

  /** The scratch Claude Code home of `profile` (ClaudeSetup), made once per profile: its skills
   *  and the tested one laid out by context.ts, and a local marketplace of its layout's plugins
   *  installed into a scratch CLAUDE_CONFIG_DIR. A devbox profile's context files are the
   *  user-level file its dev box's host read (build.ts: a devbox session's working directory names
   *  no repository, so it has no other), which Claude Code reads as user memory, CLAUDE.md in its
   *  config directory: it is written there from the profile's repository at its commit, and left
   *  out of the working directory. */
  claudeSetup(profile: TaskProfile): Promise<ClaudeSetup> {
    const made = this.claudeSetups.get(profile.digest);
    if (made !== undefined) return made;
    const setup = (async () => {
      const layout = profile.profile.claude;
      if (layout === undefined) throw new Error(`the profile ${profile.file} has no Claude layout`);
      const root = path.join(this.root, "claude", "profiles", profile.digest.slice(0, 16));
      rmSync(root, { recursive: true, force: true });
      const { c, arm } = this.caseOf("claude-profile", "", profile.profile);
      const devbox = c.context === "devbox";
      const [userMemory, ...more] = devbox ? c.profile.contextFiles : [];
      if (more.length > 0) {
        throw new Error(
          `the devbox profile ${profile.file} holds ${more.length + 1} context files; Claude Code has one user memory file`
        );
      }
      // Oh My Pi's plugins are no Claude Code plugins: only the skills and context files go in.
      const contextStarted = performance.now();
      const context = await buildContext(
        {
          ...c,
          profile: { ...c.profile, plugins: [], ...(devbox ? { contextFiles: [] } : {}) },
        },
        arm,
        { source: this.o.source, cacheDir: this.o.cacheDir, root: path.join(root, "context") }
      );
      const memory =
        userMemory === undefined
          ? undefined
          : await this.o.source.file(userMemory.repo, userMemory.commit, userMemory.path);
      const contextMs = Math.round(performance.now() - contextStarted);
      const plugins = layout.plugins.map((plugin) => ({ ...plugin, skills: [...plugin.skills] }));
      const own = plugins.find((plugin) => plugin.name === layout.skillPlugin);
      if (own === undefined) {
        plugins.push({ name: layout.skillPlugin, skills: [profile.tested.name] });
      } else own.skills.push(profile.tested.name);
      const market = path.join(root, "marketplace");
      writeWhole(
        path.join(market, ".claude-plugin", "marketplace.json"),
        json({
          name: layout.marketplace,
          owner: { name: layout.marketplace },
          plugins: plugins.map((plugin) => ({
            name: plugin.name,
            source: `./plugins/${plugin.name}`,
          })),
        })
      );
      for (const plugin of plugins) {
        const dir = path.join(market, "plugins", plugin.name);
        writeWhole(
          path.join(dir, ".claude-plugin", "plugin.json"),
          json({
            name: plugin.name,
            version: "0.0.0",
            description: `The ${plugin.name} plugin of a skill-cuts load test's profile.`,
          })
        );
        for (const name of plugin.skills) {
          const installed = context.skills.find((skill) => skill.name === name);
          if (installed === undefined) throw new Error(`the context installs no skill ${name}`);
          cpSync(installed.dir, path.join(dir, "skills", name), {
            recursive: true,
            verbatimSymlinks: true,
          });
        }
      }
      const home = path.join(root, "home");
      const tmp = path.join(root, "tmp");
      const config = path.join(root, "config");
      for (const dir of [home, tmp, config]) mkdirSync(dir, { recursive: true });
      if (memory !== undefined) writeFileSync(path.join(config, "CLAUDE.md"), memory);
      const env: Record<string, string> = {
        HOME: home,
        PATH: "/usr/local/bin:/usr/bin:/bin",
        TMPDIR: tmp,
        TZ: "UTC",
        CLAUDE_CONFIG_DIR: config,
        DISABLE_AUTOUPDATER: "1",
        DISABLE_TELEMETRY: "1",
        DISABLE_ERROR_REPORTING: "1",
        CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC: "1",
      };
      const version = (await mustRun([this.o.claude, "--version"], env)).trim();
      await mustRun([this.o.claude, "plugin", "marketplace", "add", market], env);
      for (const plugin of plugins) {
        await mustRun(
          [this.o.claude, "plugin", "install", `${plugin.name}@${layout.marketplace}`],
          env
        );
      }
      return {
        env,
        workspace: context.workspace,
        prompt:
          context.appendSystemPrompt === undefined
            ? undefined
            : readFileSync(context.appendSystemPrompt, "utf8"),
        version,
        context: context.record,
        userMemory: userMemory ?? null,
        contextMs,
      };
    })();
    this.claudeSetups.set(profile.digest, setup);
    return setup;
  }

  /** Task `task`'s body through Claude Code in the context of `profile`, captured or read back;
   *  a saved one whose record holds no harness-string record was made before that rule, and is
   *  refused. Before it is saved, the body's strings of the capture's run are replaced by the
   *  session's (`claudeHarnessRules`), and a body that still holds the test's scratch is
   *  refused. */
  async claudeCapture(task: Task, profile: TaskProfile): Promise<Captured> {
    const files = loadCapturePaths(this.o.out, "claude", task.id);
    if (existsSync(files.body) && existsSync(files.record)) {
      const record = JSON.parse(readFileSync(files.record, "utf8")) as ClaudeCaptureRecord;
      if (!Object.hasOwn(record, "harness")) {
        throw new Error(
          `the capture ${files.body} was made before the harness-string rule, so its body holds the capture's own paths and date: make it again under a new --out`
        );
      }
      return {
        body: JSON.parse(readFileSync(files.body, "utf8")) as MessagesBody,
        headers: record.requestHeaders,
        listed: record.listed,
        contextMs: 0,
      };
    }
    const facts = await this.sessionOf(task, profile);
    // The context is built by the first task of its profile, which counts its time.
    const setUpNow = !this.claudeSetups.has(profile.digest);
    const setup = await this.claudeSetup(profile);
    const { thinking } = this.settings;
    const model = this.settings.model.replace(/^anthropic\//, "");
    const logs = path.join(this.root, "claude", "runs", cacheKey(task.id).slice(0, 16));
    mkdirSync(logs, { recursive: true });
    const stderrFile = path.join(logs, "stderr.txt");
    const recorder = startRecorder();
    const started = performance.now();
    let requests: RecordedRequest[];
    try {
      const child = Bun.spawn(
        [
          this.o.claude,
          "-p",
          "--output-format",
          "stream-json",
          "--verbose",
          "--model",
          model,
          ...(CLAUDE_EFFORTS.includes(thinking) ? ["--effort", thinking] : []),
          ...(setup.prompt === undefined ? [] : ["--append-system-prompt", setup.prompt]),
          // After `--`, so a task's text that starts with `-` is read as the prompt.
          "--",
          task.text,
        ],
        {
          cwd: setup.workspace,
          env: {
            ...setup.env,
            ANTHROPIC_BASE_URL: recorder.url,
            ANTHROPIC_API_KEY: RECORDER_CREDENTIAL,
            // A request for a smaller model asks for the tested one; the recorder answers each.
            ANTHROPIC_DEFAULT_HAIKU_MODEL: model,
            ANTHROPIC_SMALL_FAST_MODEL: model,
            ...(thinking === "off" ? { CLAUDE_CODE_DISABLE_THINKING: "1" } : {}),
          },
          stdin: "ignore",
          stdout: Bun.file(path.join(logs, "stream.jsonl")),
          stderr: Bun.file(stderrFile),
        }
      );
      const timer = setTimeout(() => child.kill("SIGKILL"), RUN_TIMEOUT_MS);
      const code = await child.exited;
      clearTimeout(timer);
      if (code !== 0) {
        const tail = readFileSync(stderrFile, "utf8").trim().split("\n").slice(-12).join("\n");
        throw new Error(`Claude Code ended with ${child.signalCode ?? `exit ${code}`}: ${tail}`);
      }
      requests = [...recorder.requests];
    } finally {
      await recorder.close();
    }
    const runMs = Math.round(performance.now() - started);
    const turn = requests.find((request) => {
      if (request.method !== "POST" || request.path.split("?")[0] !== "/v1/messages") return false;
      const body: unknown = JSON.parse(request.body);
      const tools = typeof body === "object" && body !== null ? Reflect.get(body, "tools") : null;
      return Array.isArray(tools) && bodyTexts(body).some((text) => text.includes(task.text));
    });
    if (turn === undefined) throw new Error("Claude Code sent no model request that holds it");
    const raw = JSON.parse(turn.body) as MessagesBody;
    const configDir = setup.env.CLAUDE_CONFIG_DIR ?? "";
    const rewritten = rewriteBody(raw, claudeHarnessRules(setup.workspace, configDir, facts), [
      this.root,
    ]);
    if (rewritten.faults.length > 0) {
      throw new Error(
        `its captured body is refused: it still holds the test's scratch after the harness-string rule, at ${rewritten.faults.join("; ")}`
      );
    }
    const body = rewritten.body as MessagesBody;
    const listing = claudeListing(body);
    const key = `${profile.tested.plugin}:${profile.tested.name}`;
    const listed = listing.get(key);
    if (listed === undefined) {
      const names = [...listing.keys()].join(", ") || "no skill";
      throw new Error(`its captured body is refused: Claude Code lists ${names}, not ${key}`);
    }
    const record: ClaudeCaptureRecord = {
      task: task.id,
      capturedAt: new Date().toISOString(),
      claude: { binary: this.o.claude, version: setup.version },
      model,
      thinking,
      skill: profile.tested,
      profile: profile.file,
      profileDigest: profile.digest,
      requestPath: turn.path,
      requestHeaders: anthropicHeaders(turn.headers),
      listed,
      listing: [...listing].map(([name, description]) => ({ name, description })),
      otherRequests: requests
        .filter((request) => request !== turn)
        .map((request) => ({ path: request.path, bytes: request.body.length })),
      context: setup.context,
      userMemory: setup.userMemory,
      runMs,
      harness: {
        ...facts,
        os: { value: CLAUDE_NEUTRAL_OS, from: "neutral: the store records no OS of the session" },
        replaced: rewritten.replaced,
        rawBody: rawBodyFile(files.body),
      },
    };
    writeWhole(rawBodyFile(files.body), turn.body);
    writeWhole(files.body, JSON.stringify(body));
    writeWhole(files.record, json(record));
    return {
      body,
      headers: record.requestHeaders,
      listed,
      contextMs: setUpNow ? setup.contextMs : 0,
    };
  }

  /** The runs task `id` has saved. Throws a refusal when its file holds a line that is not
   *  its next run, or a run made under a profile of other bytes than `profile`. */
  savedRuns(id: string, profile: TaskProfile): LoadRun[] {
    const file = loadRunFile(this.o.out, id);
    if (!existsSync(file)) return [];
    const runs: LoadRun[] = [];
    for (const [index, line] of readFileSync(file, "utf8").split("\n").entries()) {
      if (line.trim() === "") continue;
      let raw: unknown;
      try {
        raw = JSON.parse(line);
      } catch {
        raw = undefined;
      }
      const parsed = LoadRun.safeParse(raw);
      if (!parsed.success || parsed.data.task !== id || parsed.data.run !== runs.length + 1) {
        throw new Refused(`${file}:${index + 1} is not run ${runs.length + 1} of task ${id}`);
      }
      const run = parsed.data;
      if (run.profileDigest !== profile.digest) {
        throw new Refused(
          `${file}:${index + 1}: run ${run.run} of task ${id} ran under the profile ${run.profile} (SHA-256 ${run.profileDigest}), not ${profile.file} (SHA-256 ${profile.digest}): give the task that profile again, or use another --out`
        );
      }
      runs.push(run);
    }
    return runs;
  }

  /** The run of `task` numbered `run` that `reply` is, in the context of `profile`, as its run
   *  file keeps it, with the session id it went with and its place in the send order of `seed`. */
  runOf(
    task: Task,
    run: number,
    reply: Reply,
    listed: string,
    profile: TaskProfile,
    sent: { readonly userId: string | null; readonly seed: string; readonly place: number }
  ): LoadRun {
    const first = firstMessageLoad(this.o.host, reply.message.content, profile.tested);
    return {
      task: task.id,
      run,
      label: task.label,
      source: task.source,
      host: this.o.host,
      name: profile.tested.name,
      loaded: first.loaded,
      loadedBy: first.loadedBy,
      toolCalls: [...first.toolCalls],
      stopReason: reply.message.stop_reason,
      requestId: reply.requestId ?? null,
      usage: { ...reply.spent },
      listed,
      profile: profile.file,
      profileDigest: profile.digest,
      userId: sent.userId,
      sendSeed: sent.seed,
      sendPlace: sent.place,
    };
  }

  /** Task `task`'s capture in the context of `profile` (made, or read back), ready for the runs
   *  it lacks beside `saved`, with the time it took and the GitHub reads made in it. */
  async captureTask(
    task: Task,
    profile: TaskProfile,
    saved: readonly LoadRun[]
  ): Promise<ReadyTask> {
    const readsBefore = this.o.source.reads?.() ?? { calls: 0, ms: 0 };
    const started = performance.now();
    const captured =
      this.o.host === "omp"
        ? await this.ompCapture(task, profile)
        : await this.claudeCapture(task, profile);
    const readsAfter = this.o.source.reads?.() ?? { calls: 0, ms: 0 };
    return {
      task,
      profile,
      saved,
      captured,
      captureMs: performance.now() - started,
      github: { calls: readsAfter.calls - readsBefore.calls, ms: readsAfter.ms - readsBefore.ms },
    };
  }

  /**
   * The runs the tasks of `ready` (one profile's) lack, sent in one order, replay.ts's
   * `sendOrder` under a fresh seed, never one task's batch after another's; each run with a
   * session id of its own (gateway.ts's `freshIdentity`), its body otherwise its capture's. The
   * runs start in that order, all at once but for one wait: the first run of each task in the
   * order goes alone among its task's, the next send in the order starting only once it has its
   * reply, so that the prefix it wrote to the cache serves its task's later runs; the waits change
   * no place of the order. Gives each task's runs, its saved ones first and the new ones in the
   * order sent, each run that failed, whether the budget refused one, the seed and the time.
   */
  async sendRuns(ready: readonly ReadyTask[]): Promise<{
    readonly runs: ReadonlyMap<string, LoadRun[]>;
    readonly failures: string[];
    readonly budgetSpent: boolean;
    readonly seed: string;
    readonly runsMs: number;
  }> {
    const seed = newSeed();
    const byId = new Map(ready.map((each) => [each.task.id, each]));
    const order = sendOrder(
      ready.map((each) => ({ key: each.task.id, runs: this.settings.runs - each.saved.length })),
      seed
    );
    const started = new Set<string>();
    const sends: { id: string; place: number; userId: string | null; reply: Promise<Reply> }[] = [];
    const runsStarted = performance.now();
    for (const [place, { key }] of order.entries()) {
      const each = byId.get(key);
      if (each === undefined) throw new Error(`no task ${key}`);
      const own = freshIdentity(each.captured.body);
      const reply = this.o.gateway.send(own.body, { headers: each.captured.headers });
      // Its outcome is read once every run has started; until then a failure is held, not thrown.
      const settledOne = reply.then(
        () => undefined,
        () => undefined
      );
      sends.push({ id: key, place, userId: own.id, reply });
      if (!started.has(key)) {
        started.add(key);
        await settledOne;
      }
    }
    const settled = await Promise.allSettled(sends.map((send) => send.reply));
    const runsMs = performance.now() - runsStarted;
    const runs = new Map(ready.map((each) => [each.task.id, [...each.saved]]));
    const failures: string[] = [];
    let budgetSpent = false;
    for (const [index, { id, place, userId }] of sends.entries()) {
      const outcome = settled[index];
      const each = byId.get(id);
      const made = runs.get(id);
      if (outcome === undefined || each === undefined || made === undefined) continue;
      if (outcome.status === "fulfilled") {
        made.push(
          this.runOf(
            each.task,
            made.length + 1,
            outcome.value,
            each.captured.listed,
            each.profile,
            {
              userId,
              seed,
              place,
            }
          )
        );
      } else if (outcome.reason instanceof BudgetSpent) budgetSpent = true;
      else failures.push(`task ${id}: a run failed: ${messageFor(outcome.reason)}`);
    }
    return { runs, failures, budgetSpent, seed, runsMs };
  }
}

/** The seconds of the phases of a task that took `totalMs` in all, as its line ends them: its
 *  capture (the context build and the GitHub reads within it apart), its runs, its writes, and
 *  the rest. */
function phaseWords(phases: TaskPhases, totalMs: number): string {
  const s = (ms: number) => (ms / 1000).toFixed(1);
  const other = Math.max(0, totalMs - phases.captureMs - phases.runsMs - phases.writeMs);
  return `seconds: capture ${s(phases.captureMs)} (context ${s(phases.contextMs)}; ${phases.github.calls} GitHub reads, ${s(phases.github.ms)}), runs ${s(phases.runsMs)}, write ${s(phases.writeMs)}, other ${s(other)}, total ${s(totalMs)}`;
}

/** Task `task`'s verdict in words. */
function verdictWords(task: Task, outcome: TaskOutcome, wanted: number): string {
  if (!outcome.complete) {
    return `${outcome.made} of ${wanted} runs made; run the command again for the rest`;
  }
  if (outcome.fault === null)
    return `no verdict: the load test's rule takes ${LOAD_RUNS} runs a task`;
  if (!outcome.fault) return "no fault";
  return task.label === "need"
    ? `a fault: a need task loads it in fewer than ${NEED_MIN_LOADS} of ${LOAD_RUNS} runs`
    : `a fault: a near-miss task loads it in more than ${NEAR_MISS_MAX_LOADS} of ${LOAD_RUNS} runs`;
}

/** Runs the load test `o` names (see the file's header). Throws a refusal before any model call
 *  when an input is refused, and a Stopped, after saving, when the budget is spent or the
 *  peak memory passes `o.memoryLimitBytes`. */
export async function runLoad(o: LoadOptions): Promise<LoadResult> {
  const given = path.resolve(o.skillDir);
  if (!statSync(path.join(given, "SKILL.md"), { throwIfNoEntry: false })?.isFile()) {
    throw new Refused(`--skill ${o.skillDir} holds no SKILL.md`);
  }
  const own = skillName(given);
  const name = o.name ?? own;
  if (!NAME.test(name)) {
    throw new Refused(`${JSON.stringify(name)} is not a name a skill can be listed under`);
  }
  if (skillFrontmatter(given).hidden) {
    throw new Refused(
      `${o.skillDir} sets hide or disable-model-invocation, so no host lists it for its agent to load`
    );
  }
  if (!Number.isInteger(o.runs) || o.runs < 1) {
    throw new Refused(`--runs ${o.runs} is not a whole number of 1 or more`);
  }
  const slash = o.model.indexOf("/");
  const provider = slash < 0 ? "anthropic" : o.model.slice(0, slash);
  const modelId = o.model.slice(slash + 1);
  if (provider !== "anthropic" && provider !== "openai") {
    throw new Refused(
      `--model ${o.model}: the runs go to the gateway's Anthropic Messages route (anthropic/<id>) or its OpenAI Responses route (openai/<id>)`
    );
  }
  if (provider === "openai" && o.host === "claude") {
    throw new Refused(
      `--model ${o.model}: Claude Code calls the Anthropic Messages API only, so its model is anthropic/<id>`
    );
  }
  if (PRICES[modelId] === undefined) {
    throw new Refused(
      `gateway.ts has no list price for ${modelId}, so the runs' dollars could not be counted`
    );
  }
  if (o.host === "claude" && o.thinking === "minimal") {
    throw new Refused(
      `Claude Code has no thinking level minimal: it takes off, auto, or ${CLAUDE_EFFORTS.join(", ")} (--effort)`
    );
  }
  const tasks = loadTasks(o.tasksFile);
  const profiled = taskProfiles(tasks, o, name);
  // With no --hint-model the hint model is the model itself, and Oh My Pi's hints are written on
  // the Messages route only, so each capture to be made would fail.
  if (
    o.host === "omp" &&
    provider === "openai" &&
    o.hintModel === undefined &&
    !tasks.some((task) => {
      const files = loadCapturePaths(o.out, "omp", task.id);
      return existsSync(files.body) && existsSync(files.record);
    })
  ) {
    throw new Refused(
      `--model ${o.model} is given with no --hint-model, so the hint model is ${o.model}, and Oh My Pi's hints are written on the Anthropic Messages route only: each task's capture would fail. Give --hint-model anthropic/<id>`
    );
  }
  const settings: Settings = {
    skillText: o.skillDigest,
    name,
    renamedFrom: name === own ? null : own,
    host: o.host,
    model: `${provider}/${modelId}`,
    thinking: o.thinking,
    runs: o.runs,
    hintModel: o.host === "omp" ? (o.hintModel ?? null) : null,
    sends: SENDS,
  };

  const summaryFile = path.join(o.out, "summary.json");
  if (existsSync(summaryFile)) {
    let raw: unknown;
    try {
      raw = JSON.parse(readFileSync(summaryFile, "utf8"));
    } catch {
      raw = undefined;
    }
    const saved = SavedSummary.safeParse(raw);
    if (!saved.success) throw new Refused(`${summaryFile} is not a load test's summary`);
    if (JSON.stringify(saved.data.settings) !== JSON.stringify(settings)) {
      throw new Refused(
        `${o.out} holds a load test of other settings (${JSON.stringify(saved.data.settings)}) than this one's (${JSON.stringify(settings)}): use another --out`
      );
    }
    for (const { task, profile } of profiled) {
      const before = saved.data.tasks.find((each) => each.id === task.id);
      if (before !== undefined && before.text !== task.text) {
        throw new Refused(
          `${o.out} holds runs of a task ${task.id} with another text: give the new text another id, or use another --out`
        );
      }
      // A capture or a run made under a profile of other bytes would not be this task's.
      const made =
        existsSync(loadCapturePaths(o.out, o.host, task.id).body) ||
        existsSync(loadRunFile(o.out, task.id));
      if (made && before === undefined) {
        throw new Refused(
          `${o.out} holds a capture or runs of task ${task.id}, which its summary does not list, so the text and the profile they were made for cannot be checked: give the task another id, or use another --out`
        );
      }
      if (made && before !== undefined && before.profileDigest !== profile.digest) {
        throw new Refused(
          `${o.out} holds task ${task.id} made under a profile of other bytes (SHA-256 ${before.profileDigest ?? "not recorded"}) than ${profile.file} (SHA-256 ${profile.digest}): give the task that profile again, or use another --out`
        );
      }
    }
  }

  const root = path.join(o.scratch, `load-${cacheKey(JSON.stringify(settings)).slice(0, 16)}`);
  const test = new LoadTest(
    o,
    settings,
    root,
    settings.renamedFrom === null ? given : renamedSkill(given, name, path.join(root, "skill"))
  );
  const runsOf = new Map(
    profiled.map(({ task, profile }) => [task.id, test.savedRuns(task.id, profile)])
  );
  /** A task's outcome from the runs it has so far. */
  const outcomeOf = (task: Task): TaskOutcome =>
    taskOutcome(task.label, runsOf.get(task.id) ?? [], o.runs);
  let peak = 0;
  // Each task's hint dollars, from its capture's record: read at the start, and again when the
  // task has run, since its capture may have been made then.
  const hintDollars = new Map<string, number>();
  const readHintDollars = (task: Task): void => {
    if (o.host !== "omp") return;
    const record = loadCapturePaths(o.out, "omp", task.id).record;
    if (!existsSync(record)) return;
    hintDollars.set(task.id, hintUsd(JSON.parse(readFileSync(record, "utf8")) as CaptureRecord));
  };
  for (const task of tasks) readHintDollars(task);
  /** Writes the summary as the runs stand; returns the skill's verdict and the test's dollars so
   *  far at list price, its runs' and its hints'. */
  const writeSummary = (): { verdict: LoadVerdict; usd: number } => {
    const verdict = skillOutcome(
      tasks.map((task) => ({ id: task.id, label: task.label, outcome: outcomeOf(task) })),
      o.runs
    );
    const runs = [...runsOf.values()].flat();
    const runsUsd = runs.reduce((sum, run) => sum + run.usage.usd, 0);
    let hintsUsd = 0;
    for (const usd of hintDollars.values()) hintsUsd += usd;
    writeWhole(
      summaryFile,
      json({
        settings,
        skillDir: given,
        tested: {
          name,
          renamedFrom: settings.renamedFrom,
          changed:
            settings.renamedFrom === null
              ? "nothing"
              : "the frontmatter name and the directory's name, in a scratch copy, and a recorded prompt's listing line of the skill; nothing else that names the skill",
        },
        listed: runs[0]?.listed ?? null,
        tasks: profiled.map(({ task, profile }) => ({
          ...task,
          profile: profile.file,
          profileDigest: profile.digest,
          ...outcomeOf(task),
        })),
        verdict,
        spend: { runsUsd, hintsUsd, usd: runsUsd + hintsUsd },
        peakRssBytes: peak,
        requestLog: o.requestLog,
        updatedAt: new Date().toISOString(),
      })
    );
    return { verdict, usd: runsUsd + hintsUsd };
  };
  writeSummary();

  const count = (label: TaskLabel) => tasks.filter((task) => task.label === label).length;
  test.say(
    `the skill ${name} on ${o.host === "omp" ? "Oh My Pi" : "Claude Code"}, model ${settings.model}, thinking ${o.thinking}: ${count("need")} need and ${count("near-miss")} near-miss tasks, ${o.runs} runs each`
  );
  const byFile = new Map<string, { profile: TaskProfile; ids: string[] }>();
  for (const { task, profile } of profiled) {
    const entry = byFile.get(profile.file);
    if (entry === undefined) byFile.set(profile.file, { profile, ids: [task.id] });
    else entry.ids.push(task.id);
  }
  for (const { profile, ids } of byFile.values()) {
    test.say(
      `${ids.length === 1 ? "task" : "tasks"} ${ids.join(", ")} run in the context ${profile.profile.context} of the profile ${profile.file} (SHA-256 ${profile.digest})`
    );
  }
  if (settings.renamedFrom !== null) {
    test.say(
      `${own} is tested under the name ${name}: its frontmatter name and its directory's name are changed in a scratch copy, and so is the name of a recorded prompt's listing line of it, and nothing else that names the skill changes (its own text, its references, other skills and code still say ${own})`
    );
  }
  const failures: string[] = [];
  const listedSaid = new Set<string>();
  const what = (task: Task) =>
    `task ${task.id} (${task.label}, ${task.source === "session" ? "from a session" : "written"})`;
  // Each profile's tasks are captured one by one, then the runs they lack are sent together in
  // one seeded order (`sendRuns`), so no task's runs go as a batch of their own.
  for (const group of byFile.values()) {
    const startedAt = new Map<string, number>();
    const ready: ReadyTask[] = [];
    for (const { task, profile } of profiled) {
      if (profile.file !== group.profile.file) continue;
      const saved = runsOf.get(task.id) ?? [];
      if (saved.length >= o.runs) {
        const outcome = outcomeOf(task);
        test.say(
          `${what(task)}: its run file is complete, skipped: loaded in ${outcome.loads} of ${outcome.made} runs: ${verdictWords(task, outcome, o.runs)}`
        );
        continue;
      }
      startedAt.set(task.id, performance.now());
      try {
        ready.push(await test.captureTask(task, profile, saved));
      } catch (error) {
        // The budget refused a hint call of the task's capture: the test stops as it does when
        // it refuses a run.
        if (error instanceof BudgetSpent) {
          peak = Math.max(peak, o.io.peakRssBytes());
          writeSummary();
          throw new Stopped(
            `the budget is spent, so no new model call starts, and task ${task.id} has no capture; the runs so far are saved under ${o.out}`
          );
        }
        failures.push(`task ${task.id}: ${messageFor(error)}`);
        o.io.err(`load: ${what(task)}: ${messageFor(error)}`);
      }
      readHintDollars(task);
    }
    if (ready.length === 0) continue;
    const sent = await test.sendRuns(ready);
    const writeMs = new Map<string, number>();
    for (const { task } of ready) {
      const runs = sent.runs.get(task.id) ?? [];
      const writeStarted = performance.now();
      writeWhole(
        loadRunFile(o.out, task.id),
        runs.map((run) => `${JSON.stringify(run)}\n`).join("")
      );
      writeMs.set(task.id, performance.now() - writeStarted);
      runsOf.set(task.id, runs);
    }
    peak = Math.max(peak, o.io.peakRssBytes());
    const summaryStarted = performance.now();
    writeSummary();
    const summaryMs = performance.now() - summaryStarted;
    test.say(
      `the runs of ${ready.length === 1 ? "task" : "tasks"} ${ready.map((each) => each.task.id).join(", ")} in the context of ${group.profile.file} went in one order (seed ${sent.seed}), each with a session id of its own`
    );
    for (const { task, profile, captured, captureMs, github } of ready) {
      const runs = sent.runs.get(task.id) ?? [];
      const outcome = outcomeOf(task);
      const listed = runs[0]?.listed;
      if (listed !== undefined && !listedSaid.has(profile.digest)) {
        listedSaid.add(profile.digest);
        test.say(
          o.host === "omp"
            ? `Oh My Pi lists ${name} under the hint ${JSON.stringify(listed)} in the context of ${profile.file}`
            : `Claude Code lists ${profile.tested.plugin}:${name} as ${JSON.stringify(listed)} in the context of ${profile.file}`
        );
      }
      const phases: TaskPhases = {
        captureMs,
        contextMs: captured.contextMs,
        github,
        runsMs: sent.runsMs,
        writeMs: (writeMs.get(task.id) ?? 0) + summaryMs,
      };
      const each = runs.map((run) => `run ${run.run} ${run.loaded ? "loads" : "does not"}`);
      test.say(
        `${what(task)}: loaded in ${outcome.loads} of ${outcome.made} runs (${each.join(", ")}): ${verdictWords(task, outcome, o.runs)}; ${phaseWords(phases, performance.now() - (startedAt.get(task.id) ?? 0))}`
      );
    }
    for (const failure of sent.failures) {
      failures.push(failure);
      o.io.err(`load: ${failure}`);
    }
    if (sent.budgetSpent) {
      throw new Stopped(
        `the budget is spent, so no new model call starts; the runs so far are saved under ${o.out}`
      );
    }
    if (peak > o.memoryLimitBytes) {
      const named = ready.map((each) => each.task.id).join(", ");
      throw new Stopped(
        `the runner's peak resident memory, ${Math.round(peak / 2 ** 20)} MiB, is above its limit of ${Math.round(o.memoryLimitBytes / 2 ** 20)} MiB, so the test stops after ${ready.length === 1 ? "task" : "tasks"} ${named}; their runs are saved under ${o.out}`
      );
    }
  }
  peak = Math.max(peak, o.io.peakRssBytes());
  const { verdict, usd } = writeSummary();
  const small =
    count("need") < MIN_TASKS_PER_LABEL || count("near-miss") < MIN_TASKS_PER_LABEL
      ? ` (a load test has at least ${MIN_TASKS_PER_LABEL} need and ${MIN_TASKS_PER_LABEL} near-miss tasks; this one has ${count("need")} and ${count("near-miss")})`
      : "";
  test.say(
    `the skill ${name}: ${verdict.reason}${small}; the test's runs and hints so far cost $${usd.toFixed(4)} at list price; written to ${summaryFile}`
  );
  return { failures };
}
