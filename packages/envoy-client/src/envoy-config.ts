import {
  access,
  chmod,
  type FileHandle,
  lstat,
  mkdtemp,
  open,
  readFile,
  readlink,
  rm,
  stat,
} from "node:fs/promises";
import { homedir, tmpdir } from "node:os";
import * as path from "node:path";
import { z } from "zod";
import { hasErrnoCode, messageFor } from "./errors";
import { jsonErrorPosition } from "./json-position";
import { spawnWatched, type WatchedCommand } from "./process-group";
import { sendable } from "./secret-file";

/** Where a caller runs: one of the three plugin hosts (and the `dispatch` command, which takes its
 * host plugin's rule), or a script outside any of them (`command`). */
export type EnvoyHarness = "omp" | "claude" | "codex" | "command";

/** The variables a read consults, by name, over whatever else the environment holds. */
export type EnvoyConfigEnvironment = {
  readonly HOME?: string | undefined;
  readonly XDG_CONFIG_HOME?: string | undefined;
  readonly ENVOY_CONFIG_FILE?: string | undefined;
  readonly OMP_PROFILE?: string | undefined;
  readonly PI_CODING_AGENT_DIR?: string | undefined;
  readonly CLAUDE_CONFIG_DIR?: string | undefined;
  readonly CLAUDE_CODE_CHILD_SESSION?: string | undefined;
  readonly DISPATCH_URL?: string | undefined;
} & Readonly<Record<string, string | undefined>>;

/** How long a `!` command may run before its field is an error. */
const COMMAND_TIMEOUT_MS = 10_000;

/** A variable's value, with an empty one read as unset: the one meaning of `""` for every variable
 * Dispatch and Envoy read. */
export function nonEmpty(value: string | undefined): string | undefined {
  return value === undefined || value === "" ? undefined : value;
}

/** A path variable's value, refused when relative: it would resolve against whatever directory
 * each process happens to start in, a cloned repository's included. */
function absolute(name: string, value: string): string;
function absolute(name: string, value: string | undefined): string | undefined;
function absolute(name: string, value: string | undefined): string | undefined {
  if (value !== undefined && !path.isAbsolute(value)) {
    throw new Error(`${name} is ${value}, which is not an absolute path`);
  }
  return value;
}

/** `${XDG_CONFIG_HOME:-$HOME/.config}/legion/envoy.json`, the file that belongs to no harness, or
 * the refusal of a relative `XDG_CONFIG_HOME`, which the XDG Base Directory spec makes invalid. */
function neutralConfig(env: EnvoyConfigEnvironment): { path: string } | { error: string } {
  const xdgConfigHome = nonEmpty(env.XDG_CONFIG_HOME);
  if (xdgConfigHome !== undefined && !path.isAbsolute(xdgConfigHome)) {
    return { error: `XDG_CONFIG_HOME is ${xdgConfigHome}, which is not an absolute path` };
  }
  // env is the call's one source of truth: os.homedir() alone is not a seam, since Bun resolves it
  // once at startup and ignores later HOME changes.
  const configHome = xdgConfigHome ?? path.join(nonEmpty(env.HOME) ?? homedir(), ".config");
  return { path: path.resolve(configHome, "legion", "envoy.json") };
}

/** The variable naming each plugin's own directory. Codex has none here: `CODEX_HOME` is the
 * person's own Codex configuration, which Envoy never reads or writes, so a Codex session reads the
 * file `ENVOY_CONFIG_FILE` names, else the neutral one. */
const HARNESS_DIRECTORY_VARIABLE = {
  omp: "PI_CODING_AGENT_DIR",
  claude: "CLAUDE_CONFIG_DIR",
} as const;

type PluginHarness = keyof typeof HARNESS_DIRECTORY_VARIABLE;

const PLUGIN_HARNESSES = Object.keys(HARNESS_DIRECTORY_VARIABLE) as readonly PluginHarness[];

/** The directory a plugin's harness names, as written: Oh My Pi's `PI_CODING_AGENT_DIR` when
 * `OMP_PROFILE` names a profile other than `default`, Claude Code's `CLAUDE_CONFIG_DIR`. */
function namedDirectory(harness: PluginHarness, env: EnvoyConfigEnvironment): string | undefined {
  if (harness === "omp") {
    const profile = nonEmpty(env.OMP_PROFILE);
    if (profile === undefined || profile === "default") return undefined;
  }
  return nonEmpty(env[HARNESS_DIRECTORY_VARIABLE[harness]]);
}

/** The directory `harness` reads its envoy.json from, refused when relative: a plugin's own
 * (`namedDirectory`), or for a command the first one named of Oh My Pi's and Claude Code's, in
 * that order, Claude Code's only under `CLAUDE_CODE_CHILD_SESSION=1`. */
function harnessDirectory(harness: EnvoyHarness, env: EnvoyConfigEnvironment): string | undefined {
  if (harness === "codex") return undefined;
  for (const plugin of harness === "command" ? PLUGIN_HARNESSES : [harness]) {
    // A shell outside Claude Code can export CLAUDE_CONFIG_DIR too; only Claude Code sets
    // CLAUDE_CODE_CHILD_SESSION=1, in every Bash, hook and status line command it runs.
    if (harness === "command" && plugin === "claude" && env.CLAUDE_CODE_CHILD_SESSION !== "1") {
      continue;
    }
    const directory = namedDirectory(plugin, env);
    if (directory !== undefined) return absolute(HARNESS_DIRECTORY_VARIABLE[plugin], directory);
  }
  return undefined;
}

/** The file `ENVOY_CONFIG_FILE` names, the one a plugin hands the commands its session runs, or
 * undefined when it is unset or empty. Throws when it is relative. */
export function handedConfigPath(env: EnvoyConfigEnvironment): string | undefined {
  const handed = nonEmpty(env.ENVOY_CONFIG_FILE);
  return handed === undefined ? undefined : path.resolve(absolute("ENVOY_CONFIG_FILE", handed));
}

/**
 * The envoy.json a caller reads. A plugin reads its own harness's directory (omp: a named
 * profile's `PI_CODING_AGENT_DIR`; Claude Code: `CLAUDE_CONFIG_DIR`) and no other harness's, else
 * the file `ENVOY_CONFIG_FILE` names, else `${XDG_CONFIG_HOME:-$HOME/.config}/legion/envoy.json`.
 * A script (`command`) reads `ENVOY_CONFIG_FILE` first, then the omp and Claude Code directories
 * in that order (Claude Code's only under `CLAUDE_CODE_CHILD_SESSION=1`), then that same neutral
 * file. Throws when the path it would use is relative: `ENVOY_CONFIG_FILE`, the harness
 * directory, or `XDG_CONFIG_HOME` for the neutral file.
 */
export function envoyConfigPath(harness: EnvoyHarness, env: EnvoyConfigEnvironment): string {
  const handedFirst = harness === "command" ? handedConfigPath(env) : undefined;
  if (handedFirst !== undefined) return handedFirst;
  const directory = harnessDirectory(harness, env);
  if (directory !== undefined) return path.resolve(directory, "envoy.json");
  const handed = handedConfigPath(env);
  if (handed !== undefined) return handed;
  const neutral = neutralConfig(env);
  if ("error" in neutral) throw new Error(neutral.error);
  return neutral.path;
}

const httpUrlSchema = z.url({ protocol: /^https?$/, error: "must be an http or https URL" });

// Strict per section, http(s) addresses, tolerant top level. The Dispatch server reads the same
// file and follows these validation rules.
const DispatchSectionSchema = z.strictObject({
  enabled: z.boolean().optional(),
  serverUrl: httpUrlSchema.optional(),
  token: z.string().optional(),
});

const EnvoySectionSchema = z.strictObject({
  enabled: z.boolean().optional(),
  url: httpUrlSchema.optional(),
  natsUrls: z.array(z.string().trim().min(1, "must not be empty")).optional(),
  token: z.string().optional(),
  natsSeed: z.string().optional(),
  // `broker` or `none`, which `envoySettings` reads (`NATS_AUTH_VAR`); another value turns off
  // inbound messages alone, naming it, rather than the whole file.
  natsAuth: z.string().optional(),
});

// A top-level `natsUrls` is refused before this schema runs (`parseFile`): it belongs in
// `envoy.natsUrls`.
const EnvoyFileSchema = z.looseObject({
  $schema: z.string().optional(),
  dispatch: DispatchSectionSchema.optional(),
  envoy: EnvoySectionSchema.optional(),
});

// A checkout's file is the repository's: it may configure Dispatch and no Envoy. Its `envoy` key
// passes this schema unchecked, and `readEnvoyConfig` names it as not read.
const CheckoutFileSchema = z.looseObject({
  $schema: z.string().optional(),
  dispatch: DispatchSectionSchema.optional(),
});

// The session's switch for AskGate, read apart from the rest of the file (`askgateSetting`), so a
// section that does not parse never fails the Dispatch or Envoy read.
const AskgateSectionSchema = z.strictObject({ enabled: z.boolean().optional() });

/** AskGate on or off for a session, and why a section that could not be read leaves it on. */
export type AskgateSetting = { readonly enabled: boolean; readonly error: string | null };

type EnvoyFile = z.infer<typeof EnvoyFileSchema>;

/** A `dispatch` section as written. */
export type DispatchFileSettings = Readonly<z.infer<typeof DispatchSectionSchema>>;

/** An `envoy` section as written, its `natsUrls` trimmed. */
export type EnvoyFileSettings = Readonly<z.infer<typeof EnvoySectionSchema>>;

/** A checkout's own `.envoy/envoy.json`: as written, or why it could not be used. It holds no
 * Envoy settings: an `envoy` section there is `envoyError`. Its failure is its own: Dispatch reads
 * it, Envoy never does. */
export type CheckoutEnvoyConfig =
  | {
      readonly kind: "valid";
      readonly path: string;
      readonly dispatch: DispatchFileSettings;
      /** Set when the file holds an `envoy` section, which a checkout cannot write. */
      readonly envoyError: string | null;
      /** Set when the file holds an `askgate` section, which only the session's file sets. */
      readonly askgateError: string | null;
    }
  | { readonly kind: "invalid"; readonly path: string; readonly error: string };

/** A chosen file that does not exist: a dangling link's target and any old-file notice
 * (`missingConfigNotice`). */
export interface MissingConfig {
  readonly link: string | null;
  readonly oldUserFileNotice?: string;
}

/**
 * The chosen envoy.json and the checkout's file beside it, as written: a `!` value is still its
 * command, which only the section's own resolver runs (`dispatchSettings`, `envoySettings`), and
 * only when that section uses the value. A chosen file that cannot be read or is not valid
 * (`failed`) has no sections at all.
 */
export type EnvoyConfig =
  | { readonly kind: "failed"; readonly path: string; readonly error: string }
  | {
      readonly kind: "read";
      /** The file the harness rule chose (`envoyConfigPath`). */
      readonly path: string;
      /** The chosen file's sections; empty when it does not exist. */
      readonly dispatch: DispatchFileSettings;
      readonly envoy: EnvoyFileSettings;
      /** The checkout's own file, when the working directory has one. */
      readonly checkout: CheckoutEnvoyConfig | null;
      /** Set when the chosen file does not exist. */
      readonly missing: MissingConfig | null;
      /** Files found and not read, each naming its move: a checkout's `.opencode/envoy.json` while
       * the checkout has no `.envoy/envoy.json`. */
      readonly warnings: readonly string[];
    };

/** A chosen file that was read. */
export type ReadEnvoyConfig = Extract<EnvoyConfig, { readonly kind: "read" }>;

/** A section that is off: a `notice` says why when nothing is wrong (a file turns it off, or
 * nothing names it), an `error` names what could not be read or used. */
export type SectionOff =
  | { readonly enabled: false; readonly notice: string; readonly error: null }
  | { readonly enabled: false; readonly notice: null; readonly error: string };

export function offWithNotice(notice: string): SectionOff {
  return { enabled: false, notice, error: null };
}

export function offWithError(error: string): SectionOff {
  return { enabled: false, notice: null, error };
}

/** `value` as an http or https URL, or undefined for anything else. */
export function httpUrl(value: string): URL | undefined {
  let url: URL;
  try {
    url = new URL(value);
  } catch {
    return undefined;
  }
  return url.protocol === "http:" || url.protocol === "https:" ? url : undefined;
}

type ParsedFile<Settings> =
  | { readonly kind: "absent"; readonly link: string | null }
  | { readonly kind: "invalid"; readonly reason: string }
  | { readonly kind: "valid"; readonly settings: Settings };

/** Describe the first schema-validation issue, naming the file and offending key. */
function describeSchemaIssue(filePath: string, error: z.ZodError): string {
  const issue = error.issues[0];
  if (!issue) return `${filePath}: invalid envoy.json`;
  if (issue.code === "unrecognized_keys") {
    const section = issue.path.join(".");
    const keys = issue.keys.map((key) => `${section}.${key}`).join(", ");
    return `${filePath}: unrecognized ${section} key(s): ${keys}`;
  }
  return `${filePath}: ${issue.path.join(".")}: ${issue.message}`;
}

/** A dangling link's target, for a chosen file that does not exist. */
async function danglingLink(filePath: string): Promise<string | null> {
  try {
    return (await lstat(filePath)).isSymbolicLink() ? await readlink(filePath) : null;
  } catch {
    return null;
  }
}

/** Whether anything is at `file`, as `existsSync` answers it: any error is nothing there. */
async function exists(file: string): Promise<boolean> {
  try {
    await access(file);
    return true;
  } catch {
    return false;
  }
}

async function parseFile<Settings>(
  filePath: string,
  schema: z.ZodType<Settings>,
  checkout: boolean
): Promise<ParsedFile<Settings>> {
  let raw: string;
  try {
    raw = await readFile(filePath, "utf-8");
  } catch (error) {
    if (hasErrnoCode(error, "ENOENT") || hasErrnoCode(error, "ENOTDIR")) {
      // A missing checkout file names no link: only the chosen file's notice reads it.
      return { kind: "absent", link: checkout ? null : await danglingLink(filePath) };
    }
    return { kind: "invalid", reason: `${filePath}: could not be read (${messageFor(error)})` };
  }
  let parsedJson: unknown;
  try {
    parsedJson = JSON.parse(raw);
  } catch {
    const position = jsonErrorPosition(raw);
    return {
      kind: "invalid",
      reason: `${filePath}: invalid JSON${position === undefined ? "" : ` at ${position}`}`,
    };
  }
  if (typeof parsedJson === "object" && parsedJson !== null && "natsUrls" in parsedJson) {
    return {
      kind: "invalid",
      reason: checkout
        ? `${filePath}: the top-level natsUrls is not read, and a checkout's envoy.json holds only a dispatch section`
        : `${filePath}: the top-level natsUrls is not read; move it to envoy.natsUrls`,
    };
  }
  const parsed = schema.safeParse(parsedJson);
  if (!parsed.success) {
    return { kind: "invalid", reason: describeSchemaIssue(filePath, parsed.error) };
  }
  return { kind: "valid", settings: parsed.data };
}

/** What a `!` command came to: its value, or why it gave none, and whether that was its running out
 * of time rather than ending on its own. */
type CommandOutcome =
  | { readonly value: string }
  | { readonly failure: string; readonly timedOut: boolean };

/** A kept outcome, and when it was kept. */
type KeptOutcome = CommandOutcome & { readonly at: number };

/** How long a `!` command's failure that ended on its own is kept before it runs again: a transient
 * cause (a locked keyring, a file not yet written) clears without a restart. A command that ran out
 * of time is kept failed for the life of the process instead: each run holds every read waiting on
 * it for the whole timeout, so a hanging command retried once a minute would stall its session's
 * Dispatch and Envoy reads that often. */
const FAILURE_KEPT_MS = 60_000;

/** The process-wide store of kept outcomes, by command, file and working directory. It lives on
 * `globalThis` under a registered symbol because Oh My Pi loads each extension instance as a fresh
 * module, and every bundle of this module in one process (pi-envoy's, pi-legion's, released
 * separately) shares it; an entry's shape (`KeptOutcome`) is its contract across them. A change to
 * that shape takes a new key (`.v2`), so an older bundle in the same process never misreads an
 * entry a newer one wrote. */
const COMMAND_OUTCOMES: unique symbol = Symbol.for("legion.envoy-config.commands.v1");
/** The runs under way, by the same key, beside the kept outcomes and shared the same way: a read
 * that starts while its command runs, in any bundle of this module at or after this map was added,
 * waits for that run rather than starting another; a bundle from before it starts a run of its own.
 * An entry, a promise of the `KeptOutcome` its run keeps, lives only while the run does; its shape
 * is its contract across bundles, under the same rule as the outcomes'. */
const COMMAND_RUNS: unique symbol = Symbol.for("legion.envoy-config.command-runs.v1");
const processGlobal = globalThis as typeof globalThis & {
  [COMMAND_OUTCOMES]?: Map<string, KeptOutcome>;
  [COMMAND_RUNS]?: Map<string, Promise<KeptOutcome>>;
};
const commandOutcomes: Map<string, KeptOutcome> = processGlobal[COMMAND_OUTCOMES] ?? new Map();
processGlobal[COMMAND_OUTCOMES] = commandOutcomes;
const commandRuns: Map<string, Promise<KeptOutcome>> = processGlobal[COMMAND_RUNS] ?? new Map();
processGlobal[COMMAND_RUNS] = commandRuns;

/** A file of its own for a command's stdout that no one can name: created in a private directory,
 * which is removed again before the command starts. The command writes it through the descriptor
 * it is handed, this process reads it through its own (`readOutput`), and the kernel frees it once
 * the last of them is closed: no exit of this process or of the command leaves the value on disk
 * under a name. Each step is asynchronous, so a loaded disk holds this read alone, never the
 * process's event loop. */
async function unnamedFile(): Promise<FileHandle> {
  const directory = await mkdtemp(path.join(tmpdir(), "envoy-command-"));
  try {
    await chmod(directory, 0o700);
    return await open(path.join(directory, "stdout"), "wx+", 0o600);
  } finally {
    await rm(directory, { recursive: true, force: true });
  }
}

/** All that was written to `file`, read from its start: the command's writes moved the offset
 * this descriptor shares with it, so each read names its position. */
async function readOutput(file: FileHandle): Promise<string> {
  const output = Buffer.alloc((await file.stat()).size);
  let read = 0;
  while (read < output.length) {
    const { bytesRead } = await file.read(output, read, output.length - read, read);
    if (bytesRead === 0) break;
    read += bytesRead;
  }
  return output.toString("utf-8", 0, read);
}

async function runCommand(
  command: string,
  env: EnvoyConfigEnvironment,
  cwd: string
): Promise<CommandOutcome> {
  const childEnv: Record<string, string> = {};
  for (const [name, variable] of Object.entries(env)) {
    if (variable !== undefined) childEnv[name] = variable;
  }
  // stdout goes to a file, so the wait is for the shell alone: a pipe would be held open by any
  // process the command left running, until the timeout.
  const file = await unnamedFile();
  try {
    let watched: WatchedCommand<number, "ignore">;
    try {
      // Its own process group, so every process the command started is killed with it, and a
      // group that ends with this process, however it ends (`spawnWatched`).
      watched = spawnWatched(["/bin/sh", "-c", command], {
        cwd,
        env: childEnv,
        stdout: file.fd,
        stderr: "ignore",
      });
    } catch (error) {
      return { failure: `could not start: ${messageFor(error)}`, timedOut: false };
    }
    const { child, endGroup } = watched;
    let timedOut = false;
    const timer = setTimeout(() => {
      timedOut = true;
      endGroup();
    }, COMMAND_TIMEOUT_MS);
    try {
      await child.exited;
    } finally {
      clearTimeout(timer);
    }
    // Whatever the outcome, nothing the command started outlives it, and so nothing it left
    // running can change the value read below.
    endGroup();
    if (timedOut) {
      return {
        failure: `did not finish within ${COMMAND_TIMEOUT_MS / 1000} s, and does not run again in this process`,
        timedOut: true,
      };
    }
    if (child.exitCode !== 0) {
      return {
        failure: `exited with ${child.signalCode ?? `status ${child.exitCode}`}`,
        timedOut: false,
      };
    }
    const value = (await readOutput(file)).trim();
    if (value.length === 0) return { failure: "printed nothing", timedOut: false };
    if (!sendable(value)) {
      return {
        failure: "printed more than one line, or whitespace inside its value",
        timedOut: false,
      };
    }
    return { value };
  } finally {
    await file.close();
  }
}

/** Whether reading a `!` value may run its command: `run`, as every read does by default, or
 * `kept`, which only takes the outcome a run already kept in this process and never runs one, for a
 * caller on a deadline that a 10 s command would blow (the per-request feed of people's answers). */
export type CommandMode = "run" | "kept";

/**
 * A value from the chosen file: as written, or, for one starting with `!`, its command's trimmed
 * stdout. The command runs under `/bin/sh -c` with `env`, in the directory of the file that names
 * it (never the session's working directory, so a relative command or a directory-configured shim
 * resolves against that file), in its own process group, for at most 10 s, while the process goes
 * on serving its timers and connections: the read awaits it and nothing else does, and a read that
 * starts while it runs awaits that same run. Its value is kept for the process, shared by every
 * copy of this module loaded in it, keyed by the command, its file and its working directory; so
 * is a run out of time, while a failure that ended on its own (a non-zero exit, empty output, more
 * than one line) is kept for 60 s, then the command runs again.
 * Its exit status decides, and the wait is for the shell alone: once it exits or runs out of time,
 * every process of its group still running is killed, then its output is read. However this
 * process ends while the command runs, a SIGKILL included, the group ends with it
 * (`spawnWatched`), and the output was never in a file anyone can name (`unnamedFile`). A
 * non-zero exit, a timeout or empty output rejects naming the field and the file. A
 * value with whitespace or a control character inside it, as written or as printed, rejects too,
 * since it cannot be sent as one bearer. No error carries the value or either stream.
 *
 * With `commands: "kept"` the command never runs: the kept outcome answers, a kept failure rejects
 * as above however old it is, and a command with no kept outcome rejects saying it has not run.
 */
export async function fileValue(
  value: string,
  field: string,
  filePath: string,
  env: EnvoyConfigEnvironment,
  commands: CommandMode = "run"
): Promise<string> {
  if (!value.startsWith("!")) {
    if (!sendable(value)) {
      throw new Error(`${filePath}: ${field} holds whitespace or a control character`);
    }
    return value;
  }
  const command = value.slice(1);
  const cwd = path.dirname(path.resolve(filePath));
  const key = JSON.stringify([command, filePath, cwd]);
  let outcome = commandOutcomes.get(key);
  if (commands === "kept") {
    if (outcome === undefined) {
      throw new Error(
        `${filePath}: ${field}'s command has not run in this process yet, and this read does not run it`
      );
    }
  } else if (
    outcome === undefined ||
    ("failure" in outcome && !outcome.timedOut && Date.now() - outcome.at >= FAILURE_KEPT_MS)
  ) {
    let run = commandRuns.get(key);
    if (run === undefined) {
      run = runCommand(command, env, cwd)
        .then((ran) => {
          const kept = { ...ran, at: Date.now() };
          commandOutcomes.set(key, kept);
          return kept;
        })
        .finally(() => commandRuns.delete(key));
      commandRuns.set(key, run);
    }
    outcome = await run;
  }
  if ("failure" in outcome) throw new Error(`${filePath}: ${field}'s command ${outcome.failure}`);
  return outcome.value;
}

/** The device a directory is on, as `stat` gives it, or undefined when it cannot be read. */
export type DeviceOf = (directory: string) => Promise<number | undefined>;

async function statDevice(directory: string): Promise<number | undefined> {
  try {
    return (await stat(directory)).dev;
  } catch {
    return undefined;
  }
}

/** What to do with `oldFile`, which is not read, so that `newFile` holds it: `move`, the `mv`
 * command, unless the old file's directory and the nearest existing ancestor of the new file's
 * directory are on different filesystems (their devices differ, as for a directory an agent box
 * bind-mounts from its host). There a `mv` would move the host's file into the box's own
 * filesystem, so the notice names the file to create and says to move the old one on the machine
 * that owns it, with no command. */
async function moveNotice(
  oldFile: string,
  newFile: string,
  move: string,
  deviceOf: DeviceOf
): Promise<string> {
  const oldDevice = await deviceOf(path.dirname(oldFile));
  let ancestor = path.dirname(newFile);
  let newDevice = await deviceOf(ancestor);
  while (newDevice === undefined && path.dirname(ancestor) !== ancestor) {
    ancestor = path.dirname(ancestor);
    newDevice = await deviceOf(ancestor);
  }
  if (oldDevice !== undefined && newDevice !== undefined && oldDevice !== newDevice) {
    return `${oldFile} is not read, and is on another filesystem than ${newFile} (a mount): create ${newFile}, and move ${oldFile} on the machine that owns it`;
  }
  return `${oldFile} is not read: ${move}`;
}

/** What a missing chosen file says about the OpenCode-era user file,
 * `$HOME/.config/opencode/envoy.json`, which nothing reads, while that file is still there. The
 * chosen file's path decides, not the rule that chose it: the user's default file at
 * `$HOME/.config/legion/envoy.json` names the move (`moveNotice`); a profile's own file names the
 * file to copy or link it to; the neutral file under a relocated `XDG_CONFIG_HOME` (a Legion
 * pane's, under the daemon's state directory) says it is not read there, without asking anyone to
 * move it. Any other file says nothing of it. */
async function oldUserFileNotice(
  filePath: string,
  env: EnvoyConfigEnvironment,
  deviceOf: DeviceOf
): Promise<string | undefined> {
  const home = nonEmpty(env.HOME) ?? homedir();
  const old = path.join(home, ".config", "opencode", "envoy.json");
  if (!(await exists(old))) return undefined;

  const selected = path.resolve(filePath);
  const userFile = path.resolve(home, ".config", "legion", "envoy.json");
  if (selected === userFile) {
    return await moveNotice(
      old,
      userFile,
      "mkdir -p ~/.config/legion && mv ~/.config/opencode/envoy.json ~/.config/legion/",
      deviceOf
    );
  }

  // A relative directory chose no file here (`harnessDirectory` refuses it), so it is skipped.
  const profileFile = PLUGIN_HARNESSES.map((harness) => namedDirectory(harness, env)).some(
    (directory) =>
      directory !== undefined &&
      path.isAbsolute(directory) &&
      selected === path.resolve(directory, "envoy.json")
  );
  if (profileFile) {
    return `${old} is not read for this profile; copy or link it to ${filePath}, or write that file`;
  }

  const configHome = nonEmpty(env.XDG_CONFIG_HOME);
  const neutral = neutralConfig(env);
  if (
    configHome !== undefined &&
    path.isAbsolute(configHome) &&
    "path" in neutral &&
    selected === neutral.path
  ) {
    return `${old} is not read under XDG_CONFIG_HOME=${configHome}`;
  }
  return undefined;
}

/** The notice for a chosen file that does not exist, saying what is `off` for want of it, and
 * naming the link's target and any unread old user file (`readEnvoyConfig`). */
export function missingConfigNotice(filePath: string, missing: MissingConfig, off: string): string {
  const link = missing.link === null ? "" : ` (a link to ${missing.link}, which does not exist)`;
  const old = missing.oldUserFileNotice === undefined ? "" : `; ${missing.oldUserFileNotice}`;
  return `no envoy.json at ${filePath}${link}: ${off}${old}`;
}

/**
 * Read the envoy.json at `filePath` (`envoyConfigPath`) and the checkout's `.envoy/envoy.json`
 * under `cwd`, whose `dispatch` section Dispatch's resolver reads beside the chosen file's, without
 * holding the event loop: a read on a loaded disk waits alone. Runs no command. A missing chosen
 * file sets `missing`; a chosen file that cannot be read or is not valid is `failed`, naming the
 * file and the key. The checkout's file keeps its own outcome: one that cannot be read or is not
 * valid is an `invalid` checkout, which only Dispatch reads, and an `envoy` section in it sets
 * `checkout.envoyError` rather than configuring anything.
 *
 * The OpenCode-era files are never read. While the chosen file is missing, its notice names
 * `$HOME/.config/opencode/envoy.json` when that file is there (`oldUserFileNotice`); while the
 * checkout has no `.envoy/envoy.json`, `warnings` names its `.opencode/envoy.json` and the rename.
 * Neither gives a `mv` across filesystems (`moveNotice`); `deviceOf` says which filesystem a
 * directory is on, `stat`'s device by default.
 */
export async function readEnvoyConfig(
  filePath: string,
  env: EnvoyConfigEnvironment,
  options: { readonly cwd: string; readonly deviceOf?: DeviceOf | undefined }
): Promise<EnvoyConfig> {
  const deviceOf = options.deviceOf ?? statDevice;
  const resolvedChosen = path.resolve(filePath);
  const checkoutPath = path.resolve(options.cwd, ".envoy", "envoy.json");
  const oldCheckout = path.resolve(options.cwd, ".opencode", "envoy.json");
  // Three reads that wait on the disk and on nothing of each other, so they run at once: this read
  // comes before every Dispatch call a session makes.
  const [chosen, checkoutFile, oldCheckoutThere] = await Promise.all([
    parseFile(filePath, EnvoyFileSchema, false),
    checkoutPath === resolvedChosen
      ? Promise.resolve({ kind: "absent" as const, link: null })
      : parseFile(checkoutPath, CheckoutFileSchema, true),
    oldCheckout === resolvedChosen ? Promise.resolve(false) : exists(oldCheckout),
  ]);
  if (chosen.kind === "invalid") return { kind: "failed", path: filePath, error: chosen.reason };
  const own: EnvoyFile = chosen.kind === "valid" ? chosen.settings : {};
  let checkout: CheckoutEnvoyConfig | null = null;
  if (checkoutFile.kind === "invalid") {
    checkout = { kind: "invalid", path: checkoutPath, error: checkoutFile.reason };
  } else if (checkoutFile.kind === "valid") {
    checkout = {
      kind: "valid",
      path: checkoutPath,
      dispatch: checkoutFile.settings.dispatch ?? {},
      envoyError: Object.hasOwn(checkoutFile.settings, "envoy")
        ? `${checkoutPath}: envoy is not read from a checkout's envoy.json; put it in ${filePath}`
        : null,
      askgateError: Object.hasOwn(checkoutFile.settings, "askgate")
        ? `askgate in ${checkoutPath} is not read; set it in ${filePath}`
        : null,
    };
  }
  // Whether the checkout's own file is there: what its read found, or, for one that could not be
  // read or parsed, whether anything is at its path.
  const checkoutMissing =
    checkoutPath === resolvedChosen
      ? chosen.kind === "absent"
      : checkoutFile.kind === "absent" ||
        (checkoutFile.kind === "invalid" && !(await exists(checkoutPath)));
  const warnings =
    oldCheckout !== resolvedChosen && checkoutMissing && oldCheckoutThere
      ? [
          await moveNotice(
            oldCheckout,
            checkoutPath,
            `mkdir -p ${path.dirname(checkoutPath)} && mv ${oldCheckout} ${checkoutPath}`,
            deviceOf
          ),
        ]
      : [];
  const unreadOldUserFile =
    chosen.kind === "absent" ? await oldUserFileNotice(filePath, env, deviceOf) : undefined;
  return {
    kind: "read",
    path: filePath,
    dispatch: own.dispatch ?? {},
    envoy: own.envoy ?? {},
    checkout,
    missing:
      chosen.kind === "absent"
        ? {
            link: chosen.link,
            ...(unreadOldUserFile === undefined ? {} : { oldUserFileNotice: unreadOldUserFile }),
          }
        : null,
    warnings,
  };
}

/**
 * AskGate's switch in the session's envoy.json at `filePath`, read now, without holding the event
 * loop. No file, no `askgate` section, or `enabled: true` leaves it on; `enabled: false` turns it
 * off. A section that does not parse, or a file that cannot be read or is not JSON, leaves it on
 * with an `error` that names the file and `askgate.enabled`.
 */
export async function askgateSetting(filePath: string): Promise<AskgateSetting> {
  const stayOn = (reason: string): AskgateSetting => ({
    enabled: true,
    error: `${reason}; AskGate stays on`,
  });
  let raw: string;
  try {
    raw = await readFile(filePath, "utf-8");
  } catch (error) {
    if (hasErrnoCode(error, "ENOENT") || hasErrnoCode(error, "ENOTDIR")) {
      return { enabled: true, error: null };
    }
    return stayOn(
      `${filePath}: could not be read (${messageFor(error)}); askgate.enabled is not read`
    );
  }
  let parsedJson: unknown;
  try {
    parsedJson = JSON.parse(raw);
  } catch {
    const position = jsonErrorPosition(raw);
    const at = position === undefined ? "" : ` at ${position}`;
    return stayOn(`${filePath}: invalid JSON${at}; askgate.enabled is not read`);
  }
  if (typeof parsedJson !== "object" || parsedJson === null || !("askgate" in parsedJson)) {
    return { enabled: true, error: null };
  }
  const parsed = z.object({ askgate: AskgateSectionSchema }).safeParse(parsedJson);
  if (!parsed.success) {
    // An unknown key is named as the other sections name one, and then the one key the section
    // takes; any other issue is the switch's type.
    const unknownKey = parsed.error.issues[0]?.code === "unrecognized_keys";
    return stayOn(
      unknownKey
        ? `${describeSchemaIssue(filePath, parsed.error)}; the one askgate key is askgate.enabled, true or false`
        : `${filePath}: askgate.enabled must be true or false`
    );
  }
  return { enabled: parsed.data.askgate.enabled ?? true, error: null };
}
