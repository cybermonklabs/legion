import {
  closeSync,
  constants,
  existsSync,
  fstatSync,
  openSync,
  readFileSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";
import { IDEMPOTENCY_KEY_TTL_MS } from "@legion/contracts";
import {
  askgateState,
  bytesDigest,
  consumeMarker,
  type FileDigest,
  fileOperands,
  gatedWrite,
  operandPath,
} from "./askgate-marker";
import { newAskCount } from "./askgate-new-asks";
import {
  commandHelp,
  commandName,
  commandsHelp,
  dispatchLine,
  inDirectory,
  keyNamedIn,
  type ParsedCommand,
  parseCommand,
  rerunWords,
} from "./dispatch-command";
import { defined } from "./dispatch-commands/context";
import type { ActiveDispatchConfig } from "./dispatch-config";
import type { DispatchHost } from "./dispatch-cwd";
import { checkDispatchCall, dispatchServerUrls, executeDispatchTool } from "./dispatch-execute";
import { CommandKey, DispatchWriteLandedError, type SentWrite } from "./dispatch-http";
import { sessionDirectory, sessionStateRoot } from "./dispatch-session-directory";
import {
  appendResult,
  loadSessionMemory,
  pruneSessions,
  type ResultInput,
  readResendRecord,
  readSessionTitle,
  recordOutcome,
  writeLongOutput,
  writePicture,
  writeResendInput,
  writeResendRecord,
} from "./dispatch-session-state";
import { followedAsk, followNoticeText } from "./dispatch-subscribe";
import { messageFor } from "./errors";
import {
  handedConfigFile,
  sessionConfigFile,
  sessionDispatchSettings,
  sessionEnvoySettings,
} from "./session-settings";
import { ToolInputError } from "./tool-input-errors";
import { createEnvoyClient, EnvoyApiError } from "./transport";

export interface DispatchCliIo {
  /** Where the text the model reads goes: results, refusals and notices alike. */
  stdout(text: string): void;
  /** Where a result's warnings go (`DispatchToolResult.warnings`), one line each, beside it. */
  stderr(text: string): void;
  /** A file's text; `-` is all of stdin. */
  readText(path: string): string;
  readonly cwd: string;
  readonly fetchImpl?: typeof fetch;
}

/** Exit codes, as `skill://dispatch` states them: written or read, a refusal, a usage error, and
 *  a write that may have landed. REFUSED is a call Dispatch itself turned down (a near copy, an
 *  AskGate block, a server refusal); an argument this CLI refuses before any request is USAGE,
 *  whether the argv named a command or not. UNSETTLED (sysexits' EX_TEMPFAIL, as `envoy-send`
 *  exits for a send it cannot confirm) is a call that failed after one of its writes ended with no
 *  definitive answer while an attempt of it may have reached Dispatch (`CommandKey.unsettled`):
 *  the write may have landed, so a caller that ran the call again under a new key could make it
 *  twice, and the CLI names the call's key and the command that sends it again under that key. */
const OK = 0;
const REFUSED = 1;
const USAGE = 2;
const UNSETTLED = 75;
type ExitCode = typeof OK | typeof REFUSED | typeof USAGE | typeof UNSETTLED;

/** The longest value the command line that sends a call again carries as it was given; a longer
 *  one is kept in a file it reads (`rerunWords`), so the line stays far under what a host shows. */
const RESEND_VALUE_MAX = 500;
/** The longest command that sends a call again the CLI prints: far under the 25,000 characters
 *  Claude Code shows (CLAUDE_OUTPUT_MAX), so the whole line, key included, is shown. */
const RESEND_COMMAND_MAX = 8_000;

const HOSTS = ["omp", "claude"] as const satisfies readonly DispatchHost[];

/** Claude Code reads back at most 30,000 characters of a command's output by default
 *  (`BASH_MAX_OUTPUT_LENGTH`). The whole output stays under this, with room to spare, every line
 *  the CLI adds counted (`fitOutput`). */
const CLAUDE_OUTPUT_MAX = 25_000;

/** Why a file that changed after AskGate read it is refused or posts unreviewed. */
const SAME_MESSAGE =
  "AskGate reads the files of a message's calls before any of them runs, so it did not see that change.";

/** What the CLI prints when the call reached Dispatch but its local record could not be written. */
const STATE_WRITE_FAILED = "dispatch: Dispatch took the call, but this session's state";

function stateWriteFailed(problem: string): string {
  return `${STATE_WRITE_FAILED} could not be written: ${problem}`;
}

/** How the line opens that names the key and the resend command of a call that exited UNSETTLED. */
const MAY_HAVE_LANDED = "dispatch: this write may have landed";

/** How the line opens beside the refusal of a call sent again under a key an earlier call named:
 *  this call wrote nothing, and the earlier call's write may still have landed. */
const EARLIER_MAY_HAVE_LANDED = "dispatch: the earlier call's write may have landed";

/** The settings beside the host and the session that a call may find its Dispatch, its login or
 *  its request through (`settingsRead`): its Dispatch section and token, the issue it defaults
 *  to, and what chooses the envoy.json it reads when `ENVOY_CONFIG_FILE` names none
 *  (`envoyConfigPath`). */
const CALL_SETTINGS = [
  "ENVOY_CONFIG_FILE",
  "DISPATCH_URL",
  "DISPATCH_TOKEN",
  "DISPATCH_TOKEN_FILE",
  "DISPATCH_TOKEN_BROKER",
  "LEGION_ISSUE",
  "OMP_PROFILE",
  "PI_CODING_AGENT_DIR",
  "CLAUDE_CONFIG_DIR",
  "XDG_CONFIG_HOME",
  "HOME",
] as const;

/**
 * The settings a call found its Dispatch, its login and its session through, by name: the host,
 * the session's id, and each of CALL_SETTINGS the environment sets. A resend under the call's key
 * must find them as they were, since Dispatch keeps a key for the login that sent it. They are
 * named rather than spelled out: `DISPATCH_TOKEN` is a secret, and AskGate lets no command that
 * opens with an assignment post.
 */
function settingsRead(
  env: Readonly<Partial<Record<string, string>>>,
  host: DispatchHost
): string[] {
  let session = "DISPATCH_SESSION_ID";
  if (host === "claude") {
    session = env.ENVOY_SESSION_ID === undefined ? "CLAUDE_CODE_SESSION_ID" : "ENVOY_SESSION_ID";
  }
  return [
    env.DISPATCH_HOST === undefined ? "CLAUDE_CODE_CHILD_SESSION" : "DISPATCH_HOST",
    session,
    ...CALL_SETTINGS.filter((name) => env[name] !== undefined),
  ];
}

/**
 * What the CLI adds beside the refusal of a call that names the key of a write an earlier call
 * may have landed: this call made no write, and its exit says nothing of the earlier call's.
 */
function earlierWriteLine(key: string, code: ExitCode): string {
  return `${EARLIER_MAY_HAVE_LANDED}: this call named its idempotency key ${key} and ended without writing it (exit ${code}). Read the write back before you send it again; a run without the key may make it twice.`;
}

/** The most swapped requests the line names one by one; it counts the rest. */
const SWAPPED_NAMED = 3;

/**
 * The one line that says the call sent `writes` as its first call sent them, in place of the
 * different requests it built (`CommandKey.swappedWrites`), or undefined for none: what differs
 * was not sent. It names three at most and counts the rest, so it stays short.
 */
function swappedLine(writes: readonly SentWrite[]): string | undefined {
  const [only, ...more] = writes;
  if (only === undefined) return undefined;
  const named = (write: SentWrite): string =>
    `${write.method} ${write.path} under idempotency key ${write.key}`;
  if (more.length === 0) {
    return `dispatch: warning: this call sent ${named(only)} as its first call sent it, not as this call built it, which differs (in its text, its files or the directory it ran in): Dispatch holds the first call's request, and what differs in this call was not sent.`;
  }
  const listed = writes.slice(0, SWAPPED_NAMED).map(named);
  const rest = writes.length - listed.length;
  return `dispatch: warning: this call sent ${writes.length} requests as its first call sent them, not as this call built them, which differ (in their text, their files or the directory it ran in): ${listed.join("; ")}${rest > 0 ? `; and ${rest} more` : ""}. Dispatch holds the first call's requests, and what differs in this call was not sent.`;
}

/** What `unsettledLine` says how to send again a call whose write may have landed from. */
interface Resend {
  readonly commandKey: CommandKey;
  readonly argv: readonly string[];
  /** Whether the argv names the call's key already: a call sent again under it. */
  readonly named: boolean;
  /** Every text the call read, by the path it read it from (`-` for stdin). */
  readonly read: ReadonlyMap<string, string>;
  /** The settings the call found its Dispatch, its login and its session through (`settingsRead`). */
  readonly settings: readonly string[];
  /** The envoy.json the call would read Dispatch's settings from, and whether it was there. */
  readonly configPath: string;
  readonly configRead: boolean;
  /** The directory the call ran in, where a relative path its words hold (a picture) reads. */
  readonly cwd: string;
  /** Keeps the `place`-th text the printed command reads, and answers its path; throws when it
   *  cannot (`writeResendInput`). */
  readonly keep: (text: string, place: number) => string;
}

/**
 * The line, kept whole however the output is cut, that says a write of the call may have landed:
 * the key of each such write, and how to send the call again under its key.
 *
 * Where Dispatch's `/healthz` said it keeps no keys, a resend under one may run the write again,
 * so the line says to read the write back and gives no command. Where the call could not learn
 * whether it keeps them, the line promises nothing and says to read the write back first. The
 * command is the argv with every text it read moved to a kept copy (`rerunWords`) and the key
 * added, run where the settings the call found Dispatch through are as they were, since Dispatch
 * keeps a key for the login that sent it. A command over RESEND_COMMAND_MAX would be cut before its
 * key, and one whose input could not be kept would not send it, so neither is printed: the line
 * says to send the call's own command with the key added, under which Dispatch makes no write
 * twice and refuses one whose request changed.
 */
function unsettledLine(resend: Resend): string {
  const { commandKey } = resend;
  const key = commandKey.key;
  const lost = `${MAY_HAVE_LANDED} (idempotency key ${commandKey.unsettledKeys.join(", ")}); its answer was lost`;
  if (commandKey.keyedLayer === "absent") {
    return `${lost}, and this Dispatch says it keeps no idempotency keys, so sending it again may make it twice: read the write back before you send it again.`;
  }
  const within = `within ${IDEMPOTENCY_KEY_TTL_MS / 3_600_000} hours of the first call that used that key`;
  const config = resend.configRead
    ? `this call read Dispatch's settings from ${resend.configPath}`
    : `this call read no envoy.json (none is at ${resend.configPath})`;
  const where = `from a shell where these are set as they were for this call: ${resend.settings.join(", ")}; ${config}, and Dispatch keeps a key for the login that sent it`;
  const send =
    commandKey.keyedLayer === "runs"
      ? `${lost}. Send the call again under its idempotency key ${key}, ${within}, and Dispatch makes each of its writes at most once; a run without the key may make them twice. Send it ${where}.`
      : `${lost}, and this call could not confirm that this Dispatch keeps idempotency keys: read the write back first, and only if it is not there, send the call again under its idempotency key ${key}, ${within}, ${where}.`;
  let why: string;
  try {
    const words = rerunWords(resend.argv, resend.read, RESEND_VALUE_MAX, resend.keep);
    if (!resend.named) words.push("--idempotency-key", key);
    const line = inDirectory(resend.cwd, dispatchLine(words));
    if (line.length <= RESEND_COMMAND_MAX) return `${send} Run: ${line}`;
    why = "it is too long to print whole";
  } catch (error) {
    why = `what the call read could not be kept (${messageFor(error)})`;
  }
  return `${send} The command is not printed here, because ${why}: in ${resend.cwd}, send this call's own command again with \`--idempotency-key ${key}\` added. Under the key Dispatch makes no write twice: from this session the call sends each request again as its first call sent it, and Dispatch refuses any other request under the key.`;
}

/** How a gated write gets AskGate's marker, as a refusal or a warn-mode warning tells the agent. */
const LITERAL_FORM =
  "AskGate reviews a Dispatch write, and lets it post, only when it is sent as one bash call of literal words: `dispatch <command> <words…>`. Each word is plain text, text in single quotes (also over more than one line), or text in double quotes with no `$`, backtick or `\\`. A `\\` at the end of a line continues the command. Before it, one `cd <path> &&` is permitted; after it, one `2>&1`, one here-document opened with a quoted delimiter (`<<'EOF'`) and one `| head -n N` or `| tail -n N`. For text with `$` or a backtick in it, use `--<field>-file - <<'EOF'`. A variable, `~`, a glob, an assignment, another redirect (`2>/dev/null`, `> file`), another pipe, another command (`;`, `&&`, `||`), a loop, xargs, `bash -c`, a script, an alias, or a process that a program or an eval cell starts carries no marker.";

/** A line that refuses a call before it reaches Dispatch, each at the start of the line:
 *  `dispatch <command> was not called: …` (an argument problem, exit 2, wherever it is found;
 *  AskGate's block of a write wears the same line and exits 1), or a usage refusal
 *  (exit 2), which is `dispatch: ` and its reason. The CLI's other `dispatch: ` lines refuse
 *  nothing: a result's warning
 *  (`dispatch: warning: `), the note that the session's state was not written
 *  (STATE_WRITE_FAILED), the notice of a file that is not read, printed beside a result, and the
 *  lines that name a write that may have landed and its key (MAY_HAVE_LANDED,
 *  EARLIER_MAY_HAVE_LANDED). */
const NOT_CALLED = /^dispatch ([a-z][a-z-]*) was not called: /;
const NOT_A_REFUSAL = [
  "dispatch: warning: ",
  STATE_WRITE_FAILED,
  MAY_HAVE_LANDED,
  EARLIER_MAY_HAVE_LANDED,
  // readEnvoyConfig's warnings: the OpenCode-era `.opencode/envoy.json` beside a checkout.
  /^dispatch: \S+ is not read(?:: | for this profile| under | and is on another filesystem )/,
];

export interface CliRefusal {
  /** The refusal's first line. */
  readonly line: string;
  /** The command a `was not called` line names; null for a usage refusal, which names none. */
  readonly command: string | null;
}

/**
 * Each refusal the dispatch CLI printed in `text`, by its first line: what a reader of the CLI's
 * output (an agent's transcript, where a pipe can hide the exit) needs to tell a refused call from
 * one Dispatch answered. A line counts only at the start of a line of `text`.
 */
export function cliRefusals(text: string): CliRefusal[] {
  const found: CliRefusal[] = [];
  for (const line of text.split("\n")) {
    const command = NOT_CALLED.exec(line)?.[1];
    if (command !== undefined) {
      found.push({ line, command });
      continue;
    }
    if (!line.startsWith("dispatch: ")) continue;
    if (NOT_A_REFUSAL.some((ok) => (typeof ok === "string" ? line.startsWith(ok) : ok.test(line))))
      continue;
    found.push({ line, command: null });
  }
  return found;
}

/** How long a result waits on the listener for the session's subscriptions, after the write
 *  itself has landed: one attempt, no retry, since the read is best effort. */
const SUBSCRIPTIONS_TIMEOUT_MS = 1_500;

/**
 * The output, at most `limit` characters (unbounded for a host that shows all of it). Within the
 * limit it is the result text, the picture lines, then the `kept` lines (the follow notice and the
 * CLI's own notes) as given. Past it, the text and every picture line are written whole through
 * `writeFull`, and the output names that file: the result text is cut first, then picture lines are
 * dropped from the end with a line saying how many, while the kept lines stay whole. Only when the
 * kept lines alone pass the limit is the whole output, naming the file first, cut to it.
 */
export function fitOutput(
  text: string,
  pictures: readonly string[],
  kept: readonly string[],
  limit: number,
  writeFull: (full: string) => string
): string {
  const whole = [text, ...pictures, ...kept].join("\n");
  if (whole.length <= limit) return whole;
  const full = [text, ...pictures].join("\n");
  const marker = `(the full result, ${full.length} characters: ${writeFull(full)})`;
  const droppedLine = (left: number): string =>
    `(${left} more picture line${left === 1 ? "" : "s"} left out: see the full result)`;
  // The tail's length for each number of picture lines shown, from the parts' lengths alone, one
  // newline between parts; the text needs at least the newline before the marker.
  const keptLength = kept.reduce((sum, line) => sum + line.length + 1, 0);
  let pictureLength = pictures.reduce((sum, line) => sum + line.length + 1, 0);
  let shown = pictures.length;
  const tailLength = (): number => {
    const left = pictures.length - shown;
    return (
      marker.length + pictureLength + (left === 0 ? 0 : droppedLine(left).length + 1) + keptLength
    );
  };
  while (shown > 0 && tailLength() + 1 > limit) {
    shown -= 1;
    pictureLength -= (pictures[shown]?.length ?? 0) + 1;
  }
  const left = pictures.length - shown;
  const after = [
    marker,
    ...pictures.slice(0, shown),
    ...(left === 0 ? [] : [droppedLine(left)]),
    ...kept,
  ].join("\n");
  const room = limit - after.length - 1;
  return room >= 0 ? `${text.slice(0, room)}\n${after}` : after.slice(0, limit);
}

/**
 * One `dispatch` command: parses `argv` into a Dispatch tool's arguments, runs that tool once as
 * the host session the environment names, and prints what the model reads. The host plugin sets
 * `DISPATCH_HOST`, `ENVOY_CONFIG_FILE` (the envoy.json it chose) and the session's id in the
 * agent's shell (`CLAUDE_CODE_SESSION_ID` under Claude Code, where claude-envoy's QA override
 * `ENVOY_SESSION_ID` wins over it as it does on the session's claim; `DISPATCH_SESSION_ID`
 * elsewhere); an empty variable counts as unset. With `DISPATCH_HOST` unset,
 * `CLAUDE_CODE_CHILD_SESSION=1`, which Claude Code sets in every Bash and hook process it runs, is
 * host `claude`: a Claude Code subagent's Bash can lack what the bridge's `SessionStart` hook
 * writes to `CLAUDE_ENV_FILE` (Claude Code issue 46696).
 *
 * The envoy.json it reads is the one its session's plugin chose: with `DISPATCH_HOST` set, the file
 * `ENVOY_CONFIG_FILE` names (each plugin sets the two together), else its host's own rule
 * (`envoyConfigPath(host)`); with `DISPATCH_HOST` unset and `CLAUDE_CODE_CHILD_SESSION=1`, Claude
 * Code's rule. Recomputing the file from a shell's other variables would follow ones a launcher or
 * an outer session left there. Memory a long-lived host kept across calls lives in the session's
 * state directory.
 */
export async function runDispatchCli(
  argv: readonly string[],
  rawEnv: Readonly<Record<string, string | undefined>>,
  io: DispatchCliIo
): Promise<number> {
  const print = (text: string): void => io.stdout(text.endsWith("\n") ? text : `${text}\n`);
  const env: Record<string, string> = {};
  for (const [name, value] of Object.entries(rawEnv)) {
    if (value !== undefined && value !== "") env[name] = value;
  }
  // A refusal before the call reaches Dispatch goes to the session's ledger too, whenever this
  // call knows its session, so a reader of the ledger (pi-envoy's run-end check, the
  // skill-scenario rig) sees a call whose output a pipe, `$(…)` or another program swallowed.
  // The host need not be known for the session to be: a Python `subprocess.run(["dispatch", …])`
  // inherits the session id without `DISPATCH_HOST`. A ledger that cannot be written changes
  // nothing here: the refusal is already printed, and the caller is exiting.
  // Under Claude Code the session is the one claude-envoy registers and claims under: its QA
  // override ENVOY_SESSION_ID, else CLAUDE_CODE_SESSION_ID (`claudeSessionId` in claude-envoy).
  const claudeSession = env.ENVOY_SESSION_ID ?? env.CLAUDE_CODE_SESSION_ID;
  // The key of a write an earlier call may have landed, which this call names to send it again:
  // every refusal of such a call says that the earlier call's write may still have landed. It is
  // read from the words alone (`keyNamedIn`), so a call refused whatever it holds says so too.
  const namedKey = keyNamedIn(argv);
  const refuse = (
    text: string,
    code: ExitCode,
    tool?: string,
    askgate?: ResultInput["askgate"]
  ): number => {
    const printed = namedKey === undefined ? text : `${text}\n${earlierWriteLine(namedKey, code)}`;
    print(printed);
    const sessionId = claudeSession ?? env.DISPATCH_SESSION_ID;
    if (sessionId !== undefined) {
      try {
        appendResult(sessionDirectory(env, sessionId), {
          tool: tool ?? "dispatch",
          error: printed.trimEnd(),
          ...(askgate === undefined ? {} : { askgate }),
        });
      } catch {
        // The session's directory is unusable; the printed refusal stands on its own.
      }
    }
    return code;
  };

  // Every text the call read, by the path it read it from (`-` for stdin): a call sent again reads
  // the same texts from copies (`rerunWords`), since stdin is gone and a file can change.
  const read = new Map<string, string>();
  let parsed: ParsedCommand;
  try {
    parsed = parseCommand(argv, {
      readText: (path) => {
        const text = io.readText(path);
        read.set(path, text);
        return text;
      },
    });
  } catch (error) {
    return refuse(`dispatch: ${messageFor(error)}`, USAGE);
  }
  if (parsed.kind === "help") {
    print(parsed.tool === undefined ? commandsHelp() : commandHelp(parsed.tool));
    return OK;
  }
  if (parsed.kind === "refused") {
    return parsed.tool === undefined
      ? refuse(
          [`dispatch: ${parsed.problems.join("\n")}`, "Run dispatch --help."].join("\n"),
          USAGE
        )
      : refuse(
          new ToolInputError(parsed.tool, parsed.problems, defined("aboutAFlag", parsed.aboutAFlag))
            .message,
          USAGE,
          parsed.tool
        );
  }

  const hostName =
    env.DISPATCH_HOST ?? (env.CLAUDE_CODE_CHILD_SESSION === "1" ? "claude" : undefined);
  const host = HOSTS.find((name) => name === hostName);
  if (host === undefined) {
    return refuse(
      hostName === undefined
        ? "dispatch: DISPATCH_HOST is not set. The host plugin sets it in an agent's shell. From an agent's session, send the command as a bash call. To run dispatch by hand, set DISPATCH_HOST, and set DISPATCH_SESSION_ID to the id of your own session."
        : `dispatch: DISPATCH_HOST is ${JSON.stringify(hostName)}, not omp or claude.`,
      USAGE,
      parsed.tool
    );
  }
  const [sessionId, sessionVariable] =
    host === "claude"
      ? [claudeSession, "ENVOY_SESSION_ID or CLAUDE_CODE_SESSION_ID"]
      : [env.DISPATCH_SESSION_ID, "DISPATCH_SESSION_ID"];
  if (sessionId === undefined) {
    return refuse(
      `dispatch: ${sessionVariable} is not set, so this call has no session to act as. The host plugin sets it in an agent's shell.`,
      USAGE,
      parsed.tool
    );
  }
  let dir: string;
  try {
    dir = sessionDirectory(env, sessionId);
  } catch (error) {
    return refuse(messageFor(error), USAGE, parsed.tool);
  }

  // The file the session's plugin handed its shell (each plugin sets DISPATCH_HOST and
  // ENVOY_CONFIG_FILE together), else the host's own rule.
  const file =
    (env.DISPATCH_HOST === undefined ? null : handedConfigFile(env)) ??
    sessionConfigFile(host, env);
  if (parsed.dryRun) {
    // The call's own argument check, so the call refuses no arguments its dry run passed; what
    // the call needs besides (Dispatch's configuration, this session's state, Dispatch's answers)
    // a dry run does not read, and one that passes writes no state of the session's (a refusal is
    // recorded in the ledger, as every refusal is). It runs with Dispatch off
    // too, reading a dashboard-URL ref only against a server it knows. Its output is the JSON
    // alone and whole: on standard output, or in the file `--dry-run-out` names, with standard
    // output empty. Claude Code shows the first CLAUDE_OUTPUT_MAX characters of a command's
    // output, so under it a JSON longer than that also gets one line on standard error naming
    // its size and the same call with `--dry-run-out` and a file of the caller's to read it whole.
    const settings =
      file.path === null
        ? null
        : sessionDispatchSettings(file, env, { cwd: io.cwd, session: sessionId }).dispatch;
    const { problems } = await checkDispatchCall({
      tool: parsed.tool,
      args: parsed.args,
      cwd: io.cwd,
      env,
      serverUrls: settings?.enabled === true ? dispatchServerUrls(settings) : [],
    });
    if (problems.length > 0) {
      return refuse(new ToolInputError(parsed.tool, problems).message, USAGE, parsed.tool);
    }
    const json = `${JSON.stringify(parsed.args, null, 2)}\n`;
    if (parsed.dryRunOut !== undefined) {
      try {
        writeFileSync(resolve(io.cwd, parsed.dryRunOut), json);
      } catch (error) {
        return refuse(
          `dispatch: the dry run's JSON could not be written to ${parsed.dryRunOut}: ${messageFor(error)}`,
          USAGE,
          parsed.tool
        );
      }
      return OK;
    }
    io.stdout(json);
    if (host === "claude" && json.length > CLAUDE_OUTPUT_MAX) {
      const out = join(tmpdir(), `dispatch-dry-run-${crypto.randomUUID()}.json`);
      const again = dispatchLine([...argv, "--dry-run-out", out]);
      // A call that read stdin cannot be repeated by its words alone: its stdin is spent.
      const how = read.has("-")
        ? `run this call's own command again, its here-document included, with \`--dry-run-out ${out}\` added`
        : again.length <= RESEND_COMMAND_MAX
          ? `run: ${again}`
          : `run this call's own command again with \`--dry-run-out ${out}\` added`;
      io.stderr(
        `dispatch: warning: this dry run's JSON is ${json.length.toLocaleString("en-US")} characters, past the ${CLAUDE_OUTPUT_MAX.toLocaleString("en-US")} Claude Code shows; to read it whole, ${how}\n`
      );
    }
    return OK;
  }
  if (file.path === null) {
    return refuse(`dispatch: ${file.error}`, USAGE, parsed.tool);
  }
  // The session this call acts as is the session a broker trade writes under, so the token this
  // call presents names it rather than naming none or another of this machine's.
  const { dispatch: resolution, warnings } = sessionDispatchSettings(file, env, {
    cwd: io.cwd,
    session: sessionId,
  });
  // A checkout's OpenCode-era file, which is not read, is said on every call while it is there.
  for (const warning of warnings) print(`dispatch: ${warning}`);
  if (!resolution.enabled) {
    return refuse(
      resolution.error === null
        ? `dispatch: ${resolution.notice}`
        : `dispatch: dispatch config: ${resolution.error}`,
      USAGE,
      parsed.tool
    );
  }
  const config: ActiveDispatchConfig = resolution;

  // AskGate: a session a live Oh My Pi process records with its gate bound (its AskGate record in
  // this directory) posts a gated write only with one marker the gate minted for this exact argv,
  // whatever ran the CLI. A session no live process records needs none.
  let uploadBytes: Uint8Array | undefined;
  if (gatedWrite(parsed.tool, parsed.args)) {
    const notCalled = `dispatch ${commandName(parsed.tool)} was not called:`;
    try {
      const gate = askgateState(dir);
      if (gate.kind === "broken") {
        return refuse(
          `${notCalled} AskGate's record for this session, ${gate.problem}, so whether AskGate reviewed this write cannot be told.`,
          REFUSED,
          parsed.tool,
          { mode: "block", why: gate.problem }
        );
      }
      if (gate.kind === "on") {
        // Each file the argv names, read once: its digest for the check, and an upload's bytes,
        // which are then the bytes the upload sends, so nothing changes between the check and
        // the send. Only a regular file is read; any other file has no digest, which matches no
        // reviewed marker.
        const files: FileDigest[] = [];
        for (const file of fileOperands(argv)) {
          let bytes: Buffer | undefined;
          try {
            const fd = openSync(
              operandPath(io.cwd, file),
              constants.O_RDONLY | constants.O_NONBLOCK
            );
            try {
              if (fstatSync(fd).isFile()) bytes = readFileSync(fd);
            } finally {
              closeSync(fd);
            }
          } catch {
            // Not there, or not this process's to open: no digest.
          }
          files.push({ ...file, sha256: bytes === undefined ? null : bytesDigest(bytes) });
          if (file.flag === "--path" && file.operand === parsed.args.path) uploadBytes = bytes;
        }
        const spent = consumeMarker(gate.stores, argv, parsed.args, files, Date.now());
        // The asks this write would open for a person, counted from the write's own text: the
        // one number pi-envoy's record of an unmarked write carries, so a session holding a
        // Dispatch token alone can count the asks that went out, or were refused, unreviewed.
        // Each ask the call opens counts one; a `replace`'s `with`, which `newAsks` reads as
        // every string the server might parse it as, counts as the most asks any one of those
        // readings opens (`newAskCount`). The ledger carries the count and never the ask text.
        // An upload's asks are in its file, whose bytes are already read above, and which the dry
        // run's arguments name only by path. A re-upload's blocks are counted again: the write
        // carries them.
        const asksCarried = spent.ok
          ? 0
          : newAskCount(
              parsed.tool,
              uploadBytes === undefined
                ? parsed.args
                : { ...parsed.args, content: new TextDecoder().decode(uploadBytes) }
            );
        if (!spent.ok && gate.mode === "block") {
          return refuse(
            spent.changed === undefined
              ? `${notCalled} AskGate is on for this session, and ${spent.why}. ${LITERAL_FORM} Send the write again in that form, as a bash call of its own.`
              : `${notCalled} AskGate is on for this session, and ${spent.why}. ${SAME_MESSAGE} Send the call again in a later message than the one that changes the file.`,
            REFUSED,
            parsed.tool,
            { mode: "block", why: spent.why, asksCarried }
          );
        }
        if (!spent.ok) {
          // The ledger line that tells pi-envoy this write went out unreviewed is written before
          // the write is sent, on its own: a ledger that cannot take it stops the write here, so
          // no unreviewed write posts unrecorded.
          try {
            appendResult(dir, {
              tool: parsed.tool,
              askgate: { mode: "warn", why: spent.why, asksCarried },
            });
          } catch (error) {
            return refuse(
              `${notCalled} AskGate is on for this session in warn mode, and ${spent.why}. A write that posts unreviewed is first recorded in this session's ledger, and the ledger could not be written: ${messageFor(error)}.`,
              REFUSED,
              parsed.tool
            );
          }
          print(
            spent.changed === undefined
              ? `dispatch: warning: AskGate is on for this session in warn mode, and ${spent.why}, so this write posts unreviewed. ${LITERAL_FORM} Send the next one in that form.`
              : `dispatch: warning: AskGate is on for this session in warn mode, and ${spent.why}, so this write posts unreviewed. ${SAME_MESSAGE} Send a write in a later message than the call that changes its file.`
          );
        }
      }
    } catch (error) {
      return refuse(
        `${notCalled} AskGate's marker for this call could not be checked: ${messageFor(error)}.`,
        REFUSED,
        parsed.tool,
        { mode: "block", why: messageFor(error) }
      );
    }
  }

  // A result names an owner the session is not subscribed to, or the line that subscribes it,
  // only from the listener's record of the session's topics, read (Envoy's settings with it) once,
  // when a result first asks; with Envoy off, or no answer from it, a result says nothing about
  // subscriptions. A session the listener has no record of holds none. The write has landed by
  // the time a result asks, so nothing here may throw: any failure is null, never a refusal.
  let topics: Promise<readonly string[] | null> | undefined;
  const readTopics = async (): Promise<readonly string[] | null> => {
    try {
      const envoy = sessionEnvoySettings(file, env, {
        cwd: io.cwd,
        nats: false,
        session: sessionId,
      });
      if (!envoy.enabled) return null;
      const listener = createEnvoyClient({
        baseUrl: envoy.url,
        fetch: io.fetchImpl ?? fetch,
        credential: envoy.credential,
        timeoutMs: SUBSCRIPTIONS_TIMEOUT_MS,
        retry: false,
      });
      return (await listener.getInterest(sessionId)).topics;
    } catch (error) {
      return error instanceof EnvoyApiError && error.details.status === 404 ? [] : null;
    }
  };
  const subscriptions = (): Promise<readonly string[] | null> => {
    topics ??= readTopics();
    return topics;
  };

  let follows: Set<string>;
  try {
    pruneSessions(sessionStateRoot(env), Date.now());
    follows = loadSessionMemory(dir, sessionId);
  } catch (error) {
    return refuse(`dispatch: ${messageFor(error)}`, USAGE, parsed.tool);
  }
  const { tool, args } = parsed;
  let text: string;
  const pictures: string[] = [];
  const notes: string[] = [];
  const problems: string[] = [];
  let entry: ResultInput;
  let code: ExitCode;
  // The call's key: the one a write that may have landed named, sent again, else a new one. Each
  // write the call makes takes its own key from it, and a call sent again under a key sends each
  // write its first call sent as that call sent it, from the session's record (`CommandKey`).
  let recorded: SentWrite[] = [];
  if (parsed.idempotencyKey !== undefined) {
    try {
      recorded = readResendRecord(dir, parsed.idempotencyKey);
    } catch (error) {
      notes.push(
        `dispatch: warning: ${messageFor(error)}: this call sends each write as it builds it`
      );
    }
  }
  const commandKey = new CommandKey(parsed.idempotencyKey ?? crypto.randomUUID(), recorded);
  // The line that names the call's key, kept whole however the output is cut: how to send again a
  // write that may have landed, or, beside the refusal of a call that sent one again, that the
  // earlier call's write may still have landed.
  let keyLine: string | undefined;
  try {
    const result = await executeDispatchTool({
      tool,
      args,
      cwd: io.cwd,
      ...(uploadBytes === undefined ? {} : { uploadBytes }),
      host,
      sessionId,
      sessionTitle: env.DISPATCH_SESSION_TITLE ?? readSessionTitle(dir),
      config,
      env,
      ...(io.fetchImpl === undefined ? {} : { fetchImpl: io.fetchImpl }),
      subscriptions,
      commandKey,
    });
    text = result.text;
    for (const warning of result.warnings ?? []) {
      io.stderr(`${warning}\n`);
    }
    try {
      for (const image of result.images ?? []) {
        const picture = writePicture(dir, image);
        pictures.push(`- picture: ${picture.path} (${image.mimeType}, ${picture.bytes} bytes)`);
      }
    } catch (error) {
      problems.push(messageFor(error));
    }
    const followed = followedAsk(result.details);
    if (followed !== null && !follows.has(followed.ask)) {
      follows.add(followed.ask);
      notes.push(followNoticeText(followed, await subscriptions()));
    }
    entry = { tool, details: result.details };
    // A write Dispatch refused did nothing, and `skill://dispatch` promises exit 1 for a
    // refusal: a create the near-copy gate stopped exits 1, so a caller that reads the status
    // is never told an issue was filed. Its text is the same either way.
    code = result.refused === true ? REFUSED : OK;
  } catch (error) {
    text = messageFor(error);
    // The command's write landed but its answer was lost: it is done, so the call succeeded, and
    // its one line says to read back what it made. The executor turns a landing of a write that
    // was only a step of its command (a picture, a closing reason) into a failure of its own.
    if (error instanceof DispatchWriteLandedError) {
      entry = { tool, details: { landed_without_answer: error.code } };
      code = OK;
    } else {
      entry = { tool, error: text };
      // An argument the executor refuses (a ref naming another owner, a picture that is not
      // there, an id that matches no ask) is the same usage error as one the parse refuses, and
      // exits 2 alike. `--dry-run` runs the call's own check (checkDispatchCall): the owner,
      // schema, cross-field and picture checks. Those exit 2 from the dry run and from its call,
      // so the two stay one answer. A refusal the executor makes during the call, such as an id
      // of the wrong shape or an ask id prefix matching no ask, only the call reaches.
      // Everything else here is Dispatch's own refusal or failure.
      code = error instanceof ToolInputError ? USAGE : REFUSED;
    }
  }
  // A request the call sent as its first call sent it, in place of a different one it built (a
  // body edited since): the agent is told its change was not sent, in one line however many such
  // requests there are, so the lines that must stay whole always fit.
  const swapped = swappedLine(commandKey.swappedWrites);
  if (swapped !== undefined) notes.push(swapped);
  const withLine = (line: string): ResultInput => ({
    ...entry,
    error: `${entry.error ?? text}\n${line}`,
  });
  if (commandKey.unsettled) {
    // A write of the call ended with no definitive answer after an attempt of it may have reached
    // Dispatch, so it may have landed, whatever the command then returned or threw: a near-copy
    // refusal Dispatch's keyed layer did not judge is one. It is no refusal: a caller that read
    // exit 1 would run the call again under a new key and could make the write twice. The session
    // keeps every request the call sent, for the call sent again under its key to send each as it
    // was first sent (`CommandKey`), and the line says how to send it (`unsettledLine`).
    try {
      writeResendRecord(dir, commandKey.key, commandKey.sentWrites());
    } catch (error) {
      problems.push(messageFor(error));
    }
    keyLine = unsettledLine({
      commandKey,
      argv,
      named: parsed.idempotencyKey !== undefined,
      read,
      settings: settingsRead(env, host),
      configPath: file.path,
      configRead: existsSync(file.path),
      cwd: io.cwd,
      keep: (input, place) => writeResendInput(dir, commandKey.key, place, input),
    });
    entry = withLine(keyLine);
    code = UNSETTLED;
  } else if (namedKey !== undefined && (code === REFUSED || code === USAGE)) {
    // A call that named the key of a write an earlier call may have landed, and was refused or
    // failed: exit 1 or 2 alone would read as though nothing was written at all, so the line says
    // so beside the refusal and in the ledger, whether the command threw or returned it.
    keyLine = earlierWriteLine(namedKey, code);
    entry = withLine(keyLine);
  }
  try {
    recordOutcome(dir, sessionId, follows, entry);
  } catch (error) {
    problems.push(messageFor(error));
  }
  const kept = [
    ...notes,
    ...problems.map(stateWriteFailed),
    ...(keyLine === undefined ? [] : [keyLine]),
  ];
  const writeFull = (full: string): string => {
    try {
      return writeLongOutput(dir, full);
    } catch (error) {
      return `not written, ${messageFor(error)}`;
    }
  };
  print(
    fitOutput(
      text,
      pictures,
      kept,
      host === "claude" ? CLAUDE_OUTPUT_MAX : Number.POSITIVE_INFINITY,
      writeFull
    )
  );
  return code;
}
