import { closeSync, constants, fstatSync, openSync, readFileSync } from "node:fs";
import { basename, dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
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
  type ParsedCommand,
  parseCommand,
} from "./dispatch-command";
import { ApiGateRefusal, sendApiCall } from "./dispatch-commands/api";
import { askOnceWithinDryRun, DRY_RUN_DEADLINE_MS, defined } from "./dispatch-commands/context";
import { OK, REFUSED, USAGE } from "./dispatch-commands/exit-codes";
import { parentStatusConflictLines } from "./dispatch-commands/failures";
import {
  hostedDispatch,
  hostSessionDirectory,
  unhostedDispatch,
} from "./dispatch-commands/host-session";
import { printsJson } from "./dispatch-commands/issues";
import { loginCommand } from "./dispatch-commands/login";
import {
  documentBytesRefusal,
  documentFieldRefusal,
  documentFileRead,
  uploadedFile,
} from "./dispatch-commands/stored-text";
import { type DispatchToolName, dispatchCommands } from "./dispatch-commands/table";
import {
  checkDispatchCall,
  dispatchServerUrls,
  dryRunAsksDispatch,
  type ExecuteDispatchToolInput,
  executeDispatchTool,
} from "./dispatch-execute";
import { unstorableBytes } from "./dispatch-file-text";
import { DispatchClient, DispatchWriteLandedError } from "./dispatch-http";
import { LOGIN_COMMAND } from "./dispatch-login-command";
import { runDispatchProxy, socketPathTaken } from "./dispatch-proxy";
import { PROXY_COMMAND, parseProxyArgv, proxyExitLine, proxyHelp } from "./dispatch-proxy-command";
import {
  SESSION_VARIABLES,
  sessionDirectory,
  sessionStateRoot,
} from "./dispatch-session-directory";
import {
  appendResult,
  loadSessionMemory,
  pruneSessions,
  type ResultInput,
  readSessionTitle,
  recordOutcome,
  writeLongOutput,
  writePicture,
} from "./dispatch-session-state";
import { followedAsk, followNoticeText } from "./dispatch-subscribe";
import type { SectionOff } from "./envoy-config";
import { messageFor } from "./errors";
import { nearestAncestorNamed } from "./process-ancestry";
import { sessionDispatchSettings, sessionEnvoySettings } from "./session-settings";
import { ToolInputError } from "./tool-input-errors";
import { createEnvoyClient, EnvoyApiError } from "./transport";

export interface DispatchCliIo {
  /** Where the text the model reads goes: results, refusals and notices alike. */
  stdout(text: string): void;
  /** Where a body that is not Dispatch's JSON goes, byte for byte, and alone on standard output:
   *  `dispatch api`'s answer to a 2xx that is stored content or a body that is not JSON
   *  (`DispatchToolResult.body`), a Markdown download's text among them. Without it the bytes go
   *  to `stdout` decoded as UTF-8, for a caller that reads text alone. */
  stdoutBytes?(bytes: Uint8Array): void;
  /** Where a result's warnings go (`DispatchToolResult.warnings`), one line each, beside it. */
  stderr(text: string): void;
  /** A file's text; `-` is all of stdin. The `dispatch` entry reads it as bytes that must be
   *  UTF-8 (`readFileText`), and throws `NotUtf8Error` for bytes that are not, which the command
   *  refuses with Dispatch's own refusal of them. */
  readText(path: string): string;
  readonly cwd: string;
  readonly fetchImpl?: typeof fetch;
  /** The `User-Agent` every Dispatch request of the call sends (`dispatchCommandUserAgent`). */
  readonly userAgent?: string;
  /** How long a dry run waits on Dispatch for its step's answer (`DRY_RUN_DEADLINE_MS` unless
   *  set); a test sets a shorter one. */
  readonly dryRunDeadlineMs?: number;
  /** Which of `names` this process's nearest ancestor runs as; `/proc` by default, a seam for a
   *  test of a shell that carries two hosts' variables. */
  readonly nearestAncestorNamed?: (names: readonly string[]) => string | undefined;
  /** Waits between two polls of `dispatch login --device`'s code; a timer by default, a seam for
   *  a test that answers the poll at once. */
  readonly sleep?: ((ms: number, signal?: AbortSignal) => Promise<void>) | undefined;
}

/** What the `dispatch` command names itself when no host plugin's bundle runs it: a checkout's
 *  `bin/dispatch.ts`, which no release built. */
export const DISPATCH_COMMAND_SOURCE_USER_AGENT = "dispatch/0.0.0-dev (source)";

/** A `User-Agent` token's characters: printable ASCII with no space, so no manifest can make a
 *  header value that fetch refuses and every call fails on. */
const USER_AGENT_TOKEN = /^[\x21-\x7e]+$/;

/**
 * The `User-Agent` of the `dispatch` command whose module is at `moduleUrl` (its
 * `import.meta.url`): `dispatch/<version> (<package>)` of the host plugin that ships it, read from
 * the `package.json` beside the plugin's `dist/dispatch.js`, where both hosts' builds write the
 * bundle (`scripts/build-dispatch-cli.sh`, and claude-envoy's `scripts/build.ts`). Any other
 * module, `bin/dispatch.ts` run from a checkout, or a bundle whose manifest gives no name and
 * version, is `DISPATCH_COMMAND_SOURCE_USER_AGENT`. It never throws: no call is refused for how it
 * names itself.
 */
export function dispatchCommandUserAgent(moduleUrl: string): string {
  try {
    const bundle = fileURLToPath(moduleUrl);
    const dist = dirname(bundle);
    if (basename(bundle) !== "dispatch.js" || basename(dist) !== "dist") {
      return DISPATCH_COMMAND_SOURCE_USER_AGENT;
    }
    const manifest: unknown = JSON.parse(readFileSync(join(dist, "..", "package.json"), "utf8"));
    if (typeof manifest !== "object" || manifest === null)
      return DISPATCH_COMMAND_SOURCE_USER_AGENT;
    const name: unknown = Reflect.get(manifest, "name");
    const version: unknown = Reflect.get(manifest, "version");
    return typeof name === "string" &&
      typeof version === "string" &&
      USER_AGENT_TOKEN.test(name) &&
      USER_AGENT_TOKEN.test(version)
      ? `dispatch/${version} (${name})`
      : DISPATCH_COMMAND_SOURCE_USER_AGENT;
  } catch {
    return DISPATCH_COMMAND_SOURCE_USER_AGENT;
  }
}

/** Claude Code reads back at most 30,000 characters of a command's output by default
 *  (`BASH_MAX_OUTPUT_LENGTH`), and Codex cuts the middle out of a long command output, so under
 *  either the whole output stays under this, with room to spare, every line the CLI adds counted
 *  (`fitOutput`), and the rest goes to a file the output names. */
const CLAUDE_OUTPUT_MAX = 25_000;

/** Why a file that changed after AskGate read it is refused or posts unreviewed. */
const SAME_MESSAGE =
  "AskGate reads the files of a message's calls before any of them runs, so it did not see that change.";

/** What the CLI prints when the call reached Dispatch but its local record could not be written. */
const STATE_WRITE_FAILED = "dispatch: Dispatch took the call, but this session's state";

function stateWriteFailed(problem: string): string {
  return `${STATE_WRITE_FAILED} could not be written: ${problem}`;
}

/** How a gated write gets AskGate's marker, as a refusal or a warn-mode warning tells the agent. */
const LITERAL_FORM =
  "AskGate reviews a Dispatch write, and lets it post, only when it is sent as one bash call of literal words: `dispatch <command> <words…>`. Each word is plain text, text in single quotes (also over more than one line), or text in double quotes with no `$`, backtick or `\\`. A `\\` at the end of a line continues the command. Before it, one `cd <path> &&` is permitted; after it, one `2>&1`, one here-document opened with a quoted delimiter (`<<'EOF'`) and one `| head -n N` or `| tail -n N`. For text with `$` or a backtick in it, use `--<field>-file - <<'EOF'`. A variable, `~`, a glob, an assignment, another redirect (`2>/dev/null`, `> file`), another pipe, another command (`;`, `&&`, `||`), a loop, xargs, `bash -c`, a script, an alias, or a process that a program or an eval cell starts carries no marker.";

/** A line that refuses a call before it reaches Dispatch, each at the start of the line:
 *  `dispatch <command> was not called: …` (an argument problem of a spec command or of
 *  `dispatch login`, exit 2, wherever it is found; AskGate's block of a write wears the same line
 *  and exits 1), or a usage refusal (exit 2), which is `dispatch: ` and its reason: a spec
 *  command's, and `dispatch proxy`'s refusal of its session or of Dispatch's configuration. The
 *  proxy's own refusals (`dispatch proxy: …`) and `dispatch login`'s answers at the broker
 *  (`dispatch login: …`) are of neither form. The CLI's other `dispatch: ` lines refuse nothing:
 *  a result's warning (`dispatch: warning: `), the note that the session's state was not written
 *  (STATE_WRITE_FAILED), and the notice of a file that is not read, printed beside a result. */
const NOT_CALLED = /^dispatch ([a-z][a-z-]*) was not called: /;
const NOT_A_REFUSAL = [
  "dispatch: warning: ",
  STATE_WRITE_FAILED,
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
 * Each refusal of those two forms the dispatch CLI printed in `text`, by its first line: what a
 * reader of the CLI's output (an agent's transcript, where a pipe can hide the exit) needs to tell
 * a refused call from one Dispatch answered. A line counts only at the start of a line of `text`.
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

/** Why the session's Dispatch is off, in the words the call is refused with after `dispatch: `. */
function dispatchOffText(off: SectionOff): string {
  return off.error === null ? off.notice : `dispatch config: ${off.error}`;
}

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
 * One spec command: parses `argv` into a Dispatch tool's arguments, runs that tool once as
 * the host session the environment names, and prints what the model reads. The host plugin sets
 * `DISPATCH_HOST`, `ENVOY_CONFIG_FILE` (the envoy.json it chose) and the session's id in the
 * agent's shell (`CLAUDE_CODE_SESSION_ID` under Claude Code, where claude-envoy's QA override
 * `ENVOY_SESSION_ID` wins over it as it does on the session's claim; `DISPATCH_SESSION_ID`
 * elsewhere); an empty variable counts as unset. With `DISPATCH_HOST` unset,
 * `CLAUDE_CODE_CHILD_SESSION=1`, which Claude Code sets in every Bash and hook process it runs, is
 * host `claude`: a Claude Code subagent's Bash can lack what the bridge's `SessionStart` hook
 * writes to `CLAUDE_ENV_FILE` (Claude Code issue 46696). Under Codex the session's id is
 * `CODEX_THREAD_ID`, which Codex sets in every shell command it runs; no Codex hook can export a
 * variable into those shells, so the Codex plugin's own `dispatch` shim sets `DISPATCH_HOST=codex`,
 * and with `DISPATCH_HOST` unset a shell that carries `CODEX_THREAD_ID` and not
 * `CLAUDE_CODE_CHILD_SESSION=1` is host `codex` too (`fallbackHost`).
 *
 * The envoy.json it reads is the one its session's plugin chose: with `DISPATCH_HOST` set, the file
 * `ENVOY_CONFIG_FILE` names (each plugin sets the two together), else its host's own rule
 * (`envoyConfigPath(host)`); with `DISPATCH_HOST` unset and `CLAUDE_CODE_CHILD_SESSION=1`, Claude
 * Code's rule. Recomputing the file from a shell's other variables would follow ones a launcher or
 * an outer session left there. Memory a long-lived host kept across calls lives in the session's
 * state directory.
 */
/**
 * The host this command runs under. `DISPATCH_HOST` names it, else Claude Code's
 * `CLAUDE_CODE_CHILD_SESSION=1` or the Codex CLI's `CODEX_THREAD_ID`, which each sets in every
 * shell command it runs. A shell that carries Codex's variable beside another host's (a
 * `DISPATCH_HOST` of `claude` or `omp` that host's plugin exported, or
 * `CLAUDE_CODE_CHILD_SESSION=1`) belongs to whichever of `codex`, `claude` and `omp` is the
 * command's nearest ancestor, since one was started from the other's shell and inherited its
 * variables: a Codex session started from a Claude Code or Oh My Pi shell must not act as that
 * session. Where the walk finds none of them, the other host's variables win, as they did before
 * Codex was a host.
 */
function commandHost(
  env: Readonly<Record<string, string>>,
  nearest: (names: readonly string[]) => string | undefined
): string | undefined {
  const inherited =
    (env.DISPATCH_HOST !== undefined && env.DISPATCH_HOST !== "codex") ||
    env.CLAUDE_CODE_CHILD_SESSION === "1";
  if (env.CODEX_THREAD_ID !== undefined && env.DISPATCH_HOST !== "codex") {
    if (!inherited) return "codex";
    if (nearest(["codex", "claude", "omp"]) === "codex") return "codex";
  }
  if (env.DISPATCH_HOST !== undefined) return env.DISPATCH_HOST;
  return env.CLAUDE_CODE_CHILD_SESSION === "1" ? "claude" : undefined;
}

/**
 * `dispatch proxy --socket <path> -- <command>`: its own arguments (`parseProxyArgv`), then the
 * host, the session, its directory and Dispatch resolved as every spec command resolves them
 * (`hostSessionDirectory`, `hostedDispatch`), under `DISPATCH_TOKEN_BROKER=1` with a DPoP
 * credential, then the proxy itself (`runDispatchProxy`). It reads the settings a spec command
 * reads, but it never runs on the token files: it starts only under `DISPATCH_TOKEN_BROKER=1`,
 * which the files refuse beside them, so it trades its own token at the broker. Every line of its
 * own goes to standard error, since standard output belongs to its command, and none goes to the
 * session's records: it calls neither `refuse` nor `recordOutcome`. A run that cannot start ends
 * with the exit line, as every run does; `--help`, `--dry-run` and a refusal of its own arguments
 * print none. `--dry-run` checks what a run checks before it trades and prints the Dispatch
 * server, the session and the socket on standard output.
 */
async function runProxyCommand(
  rest: readonly string[],
  env: Readonly<Record<string, string>>,
  rawEnv: Readonly<Record<string, string | undefined>>,
  io: DispatchCliIo,
  hostName: () => string | undefined
): Promise<number> {
  const say = (text: string): void => io.stderr(`${text}\n`);
  const parsed = parseProxyArgv(rest, io.cwd);
  if (parsed.kind === "help") {
    io.stdout(proxyHelp());
    return OK;
  }
  if (parsed.kind === "refused") {
    say(`dispatch proxy: ${parsed.problem} Run dispatch proxy --help.`);
    return USAGE;
  }
  const { dryRun, socket, command } = parsed;
  const cannotStart = (text: string): number => {
    say(text);
    if (!dryRun) say(proxyExitLine(0, 0, 0));
    return USAGE;
  };

  const hosted = hostSessionDirectory(hostName(), env);
  if ("problem" in hosted) return cannotStart(hosted.problem);
  const { sessionId, file } = hosted;
  // Read before the configuration, whose refusal without the setting would name a bearer the
  // proxy never presents. A value other than 1 is refused there too, unquoted.
  if ((env.DISPATCH_TOKEN_BROKER ?? "").trim() !== "1") {
    return cannotStart(
      "dispatch proxy: DISPATCH_TOKEN_BROKER is not 1, so this session holds no broker token for the proxy to serve with. The proxy runs only where the deployment trades this session's Dispatch credential at the secrets broker, and never with a bearer."
    );
  }
  const resolved = await hostedDispatch(file, env, { cwd: io.cwd, session: sessionId });
  for (const line of resolved.warnings) say(line);
  if ("problem" in resolved) return cannotStart(resolved.problem);
  const { config } = resolved;
  if (config.credential.scheme !== "DPoP") {
    return cannotStart(
      "dispatch proxy: this session's Dispatch credential is not a DPoP-bound broker token, and the proxy serves only with one."
    );
  }

  if (dryRun) {
    const taken = socketPathTaken(socket);
    if (taken !== undefined) return cannotStart(taken);
    io.stdout(
      [`Dispatch server: ${config.url}`, `Session: ${sessionId}`, `Socket: ${socket}`, ""].join(
        "\n"
      )
    );
    return OK;
  }
  // The command's environment is the one the proxy was given, an empty variable kept as it is.
  const commandEnv: Record<string, string> = {};
  for (const [name, value] of Object.entries(rawEnv)) {
    if (value !== undefined) commandEnv[name] = value;
  }
  return await runDispatchProxy({
    socket,
    command,
    env,
    commandEnv,
    cwd: io.cwd,
    config,
    sessionId,
    stderr: (text) => io.stderr(text),
  });
}

export async function runDispatchCli(
  argv: readonly string[],
  rawEnv: Readonly<Record<string, string | undefined>>,
  io: DispatchCliIo
): Promise<number> {
  // `dispatch api`'s lines from before its request (`note`), held until its answer says where they
  // go: before text they are printed first, as every command prints them, and beside an answer
  // written as bytes, which takes standard output alone, they go to standard error.
  const held: string[] = [];
  const print = (text: string): void => {
    for (const line of held.splice(0)) io.stdout(`${line}\n`);
    io.stdout(text.endsWith("\n") ? text : `${text}\n`);
  };
  /** Writes `bytes` to standard output as they came (`DispatchCliIo.stdoutBytes`), and the held
   *  lines, then `notes`, the CLI's own, to standard error. */
  const writeBody = (bytes: Uint8Array, notes: readonly string[] = []): void => {
    for (const line of [...held.splice(0), ...notes]) io.stderr(`${line}\n`);
    if (io.stdoutBytes === undefined) io.stdout(new TextDecoder().decode(bytes));
    else io.stdoutBytes(bytes);
  };
  const env: Record<string, string> = {};
  for (const [name, value] of Object.entries(rawEnv)) {
    if (value !== undefined && value !== "") env[name] = value;
  }
  // `dispatch login` logs the machine in, before any session of it exists: a person runs it in a
  // terminal, so it reads no host, no session and no envoy.json (`dispatch-commands/login.ts`).
  // It refuses while either token file is set, after its own `--help`; `tokenFilesTarget` in
  // `dispatch-config.ts` says when a call below runs on the files.
  if (argv[0] === LOGIN_COMMAND) {
    return await loginCommand(argv.slice(1), env, {
      print,
      fetchImpl: io.fetchImpl,
      sleep: io.sleep,
    });
  }
  // A refusal before the call reaches Dispatch, but `dispatch proxy`'s, goes to the session's
  // ledger too, whenever this call knows its session, so a reader of the ledger (pi-envoy's
  // run-end check, the skill-scenario rig) sees a call whose output a pipe, `$(…)` or another
  // program swallowed.
  // The host need not be known for the session to be: a Python `subprocess.run(["dispatch", …])`
  // inherits the session id without `DISPATCH_HOST`. A ledger that cannot be written changes
  // nothing here: the refusal is already printed, and the caller is exiting.
  // The host, worked out once and only when asked: the walk of the parent processes it may take
  // runs at most once per call, for the refusal ledger and the call alike.
  let resolvedHost: { readonly name: string | undefined } | undefined;
  const ledgerHost = (): string | undefined => {
    resolvedHost ??= {
      name: commandHost(env, (names) => (io.nearestAncestorNamed ?? nearestAncestorNamed)(names)),
    };
    return resolvedHost.name;
  };
  /** Appends `text`, a refusal made before the call reached Dispatch, to the ledger of the
   *  session this call knows, if any: Codex's thread under Codex, else the first id of
   *  `SESSION_VARIABLES` that is set. */
  const ledgerRefusal = (text: string, tool?: string, askgate?: ResultInput["askgate"]): void => {
    const sessionId =
      ledgerHost() === "codex"
        ? env.CODEX_THREAD_ID
        : SESSION_VARIABLES.map((variable) => env[variable]).find((id) => id !== undefined);
    if (sessionId === undefined) return;
    try {
      appendResult(sessionDirectory(env, sessionId), {
        tool: tool ?? "dispatch",
        error: text.trimEnd(),
        ...(askgate === undefined ? {} : { askgate }),
      });
    } catch {
      // The session's directory is unusable; the printed refusal stands on its own.
    }
  };
  const refuse = (
    text: string,
    code: number,
    tool?: string,
    askgate?: ResultInput["askgate"]
  ): number => {
    print(text);
    ledgerRefusal(text, tool, askgate);
    return code;
  };

  // No Dispatch tool spec defines `dispatch proxy`: its own parser takes `--` and the command
  // after it, and it prints every line of its own on standard error, since its command's
  // standard output is the command's alone.
  if (argv[0] === PROXY_COMMAND) {
    return await runProxyCommand(argv.slice(1), env, rawEnv, io, ledgerHost);
  }

  let parsed: ParsedCommand;
  try {
    parsed = parseCommand(argv, io);
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
  // The local checks of a text alone run here, once the flags are read: before the arguments are
  // checked and before anything that needs Dispatch (its configuration, the refusal of a Dispatch
  // that is off, any client), so the dry run and the call refuse a text from this one step alike,
  // with Dispatch on or off and whether or not DISPATCH_DRY_RUN_LOCAL is set, in Dispatch's own
  // refusal and with its exit code, and send nothing. The command's table entry names the fields
  // held to a Markdown document's rule (`DispatchCommand.documentFields`, `documentPathField`).
  // Such a field's text, given inline or read from a file or stdin, is refused as the upload of a
  // Markdown `--path` file of the same bytes refuses it, the size first, then the first U+0000 or
  // byte that is not UTF-8 (`documentBytesRefusal`), whether or not the bytes read are UTF-8; a
  // text the strict read decoded, and one given inline, can fail only for its size or a U+0000
  // (`documentFieldRefusal`). A text given in argv holds no U+0000 on any kernel. Linux holds one
  // argument to MAX_ARG_STRLEN, 32 pages, which is 128 KiB on 4 KiB pages and 2 MiB on 64 KiB
  // pages, so with 4 KiB pages argv carries no text past 1 MiB, and with 64 KiB pages it can. An
  // in-process caller's text can be either on any kernel. Inline text is checked here however it
  // is given, so nothing here depends on the page size: such a text is refused as a text read from
  // a file is, and the dry run and the call refuse it alike with Dispatch off too.
  // Any other field read from a file or stdin is no document, so it is refused only for bytes that
  // are not UTF-8 (an `unstorable` parse), at their first U+0000 or byte that is not UTF-8
  // (`unstorableBytes`); a U+0000 in its UTF-8 goes to Dispatch, which answers it. Bytes that are
  // not UTF-8 hold such a byte, so each finds a refusal. The file the command uploads is checked
  // as its upload is (`documentFileRead`), which reads it once, only where Dispatch stores it as a
  // Markdown document, and refuses it in the same order: past the bytes one document holds by its
  // size, then by the bytes read, then at the first U+0000 or byte that is not UTF-8. The bytes it
  // read are handed on (`uploadBytes`). The dry run's step checks those bytes and does not read the
  // file again. The call checks and sends them too, save where AskGate reviews it (`gatedWrite`,
  // with AskGate on for this session): AskGate's check reads the file a second time for its
  // digest, and the call sends the bytes that read took in place of these, which
  // `documentFileBytes` checks again before they are sent. A file it cannot read is no refusal of
  // its text, so it is left to what comes after: the argument check, and then the dry run's step,
  // where it runs, and the call, which read the file and throw the read's own error. It does not
  // read a FIFO, a device or a socket, whose read can wait forever, so the dry run's step asks
  // Dispatch nothing about it, and the call uploads it unchecked, for Dispatch's upload to judge.
  const command = dispatchCommands[parsed.tool as DispatchToolName];
  const documentFields = command?.documentFields ?? [];
  if (parsed.kind === "unstorable") {
    const { tool, field, bytes } = parsed;
    const refusal = (documentFields.includes(field) ? documentBytesRefusal : unstorableBytes)(
      field,
      bytes
    );
    return refuse(messageFor(refusal), REFUSED, tool);
  }
  for (const field of documentFields) {
    const text = parsed.args[field];
    if (typeof text !== "string") continue;
    const refusal = documentFieldRefusal(field, text);
    if (refusal !== undefined) return refuse(messageFor(refusal), REFUSED, parsed.tool);
  }
  const uploadPath =
    command?.documentPathField === undefined ? undefined : parsed.args[command.documentPathField];
  let pathBytes: Uint8Array<ArrayBuffer> | undefined;
  if (typeof uploadPath === "string") {
    const uploadName = parsed.args.name;
    const read = await documentFileRead(
      uploadedFile(io.cwd, uploadPath, typeof uploadName === "string" ? uploadName : undefined)
    );
    if (read.kind === "refused") return refuse(messageFor(read.refusal), REFUSED, parsed.tool);
    if (read.kind === "passed") pathBytes = read.bytes;
  }

  // `dispatch api` also runs where no host is found (`commandHost`), as in a GitHub Actions job,
  // like `dispatch login`: it reads Dispatch from the environment alone (`unhostedDispatch`),
  // keeps no session's state, and runs the call as the contract's command does (`sendApiCall`,
  // dispatch-commands/api.ts) once the check every call has passed. No session runs it, so there
  // is no session ledger: only the refusals made before a credential is resolved, its usage
  // errors and AskGate's refusal of a write (with AskGate's mark), go to the ledger of the session
  // the environment names, if any, under its tool; a failure of the credential, the broker's
  // refusal of a trade among them, or of the request records nothing.
  if (parsed.tool === "dispatch_api" && ledgerHost() === undefined) {
    const { problems, args } = await checkDispatchCall({
      tool: parsed.tool,
      args: parsed.args,
      cwd: io.cwd,
      env,
      serverUrls: [],
    });
    if (problems.length > 0) {
      return refuse(new ToolInputError(parsed.tool, problems).message, USAGE, parsed.tool);
    }
    if (parsed.dryRun) {
      print(JSON.stringify(parsed.args, null, 2));
      return OK;
    }
    const resolved = await unhostedDispatch(env);
    if ("problem" in resolved) return refuse(resolved.problem, USAGE, parsed.tool);
    const { url, credential } = resolved.config;
    try {
      const client = new DispatchClient(url, credential, io.fetchImpl ?? fetch, {
        userAgent: io.userAgent,
      });
      const result = await sendApiCall(args, env, client);
      if (result.body === undefined) print(result.text);
      else writeBody(result.body);
      return OK;
    } catch (error) {
      if (error instanceof ToolInputError) return refuse(error.message, USAGE, parsed.tool);
      if (error instanceof ApiGateRefusal) {
        return refuse(error.message, REFUSED, parsed.tool, error.askgate);
      }
      print(messageFor(error));
      return REFUSED;
    }
  }

  const hosted = hostSessionDirectory(ledgerHost(), env);
  if ("problem" in hosted) return refuse(hosted.problem, USAGE, parsed.tool);
  const { host, sessionId, file, dir } = hosted;

  if (parsed.dryRun && !dryRunAsksDispatch(parsed.tool, parsed.args)) {
    // The call's own argument check, so the call refuses no arguments its dry run passed. It runs
    // with Dispatch off too, reading a dashboard-URL ref only against a server it knows. A call
    // whose dry run Dispatch answers (`dryRunAsksDispatch`: a `dispatch result` record) goes on
    // below, as the call itself does.
    const resolved = await sessionDispatchSettings(file, env, {
      cwd: io.cwd,
      session: sessionId,
      tokenFiles: true,
    });
    const settings = resolved.dispatch;
    const checked = await checkDispatchCall({
      tool: parsed.tool,
      args: parsed.args,
      cwd: io.cwd,
      env,
      serverUrls: settings.enabled ? dispatchServerUrls(settings) : [],
    });
    if (checked.problems.length > 0) {
      return refuse(new ToolInputError(parsed.tool, checked.problems).message, USAGE, parsed.tool);
    }
    // A command whose call asks Dispatch a question before it writes asks it here too, from the
    // session's Dispatch as the call resolves it, and is refused as the call is, with its output
    // and exit code (`DispatchCommand.dryRun`): `dispatch artifact` asks how Dispatch would store
    // its inline text, and whether Dispatch refuses a Markdown file's bytes, and refuses a text
    // the call refuses for its spelling or for the text alone. With
    // DISPATCH_DRY_RUN_LOCAL=1 it asks nothing, for a caller that reads the arguments alone
    // (AskGate's own read). Nothing else the call reads (this session's state, Dispatch's other
    // answers) is read here. Every line besides a refusal goes to stderr, so the standard output
    // of a dry run that passes is the JSON alone.
    const step = command?.dryRun;
    if (step !== undefined && env.DISPATCH_DRY_RUN_LOCAL !== "1") {
      const deadlineMs = io.dryRunDeadlineMs ?? DRY_RUN_DEADLINE_MS;
      const deadline = AbortSignal.timeout(deadlineMs);
      const verdict = await step({
        tool: parsed.tool,
        args: checked.args,
        cwd: io.cwd,
        ...defined("uploadBytes", pathBytes),
        deadline,
        deadlineMs,
        client: () => {
          for (const warning of resolved.warnings) io.stderr(`dispatch: ${warning}\n`);
          if (!settings.enabled) {
            throw new Error(file.path === null ? file.error : dispatchOffText(settings));
          }
          // The client stops at the dry run's deadline and asks once within it
          // (askOnceWithinDryRun).
          return new DispatchClient(settings.url, settings.credential, io.fetchImpl ?? fetch, {
            signal: deadline,
            ...askOnceWithinDryRun(deadlineMs),
            userAgent: io.userAgent,
          });
        },
      });
      // The call prints what it throws as `messageFor` prints it, and exits 2 on a
      // `ToolInputError` and 1 on any other error, and so does its dry run.
      if (verdict.kind === "refused") {
        return refuse(
          messageFor(verdict.refusal),
          verdict.refusal instanceof ToolInputError ? USAGE : REFUSED,
          parsed.tool
        );
      }
      if (verdict.warning !== undefined) io.stderr(`${verdict.warning}\n`);
    }
    print(JSON.stringify(parsed.args, null, 2));
    return OK;
  }
  // The session this call acts as is the session a broker trade writes under, and the command
  // takes the token files (hostedDispatch).
  const resolved = await hostedDispatch(file, env, { cwd: io.cwd, session: sessionId });
  // A call whose whole output is one JSON object (`printsJson`) prints the CLI's own lines on
  // standard error, so its standard output is the object alone. `dispatch api`'s are held until
  // its answer is known (`held`).
  const wholeJson = printsJson(parsed.tool, parsed.args);
  const note = (line: string): void => {
    if (wholeJson) io.stderr(`${line}\n`);
    else if (parsed.tool === "dispatch_api") held.push(line);
    else print(line);
  };
  // A checkout's OpenCode-era file, which is not read, is said on every call while it is there.
  for (const line of resolved.warnings) note(line);
  if ("problem" in resolved) return refuse(resolved.problem, USAGE, parsed.tool);
  const { config } = resolved;
  // The fields the two executeDispatchTool calls below share, the dry run's and the record's,
  // named once so that a field added here reaches both. A function rather than a value, so the
  // session's title file is read as each call is built: inside that call's try, which makes a
  // file that cannot be read the call's refusal (and the record's ledger entry), and for the
  // record only after its AskGate check and its session prune, which can return first or remove
  // the session's directory.
  const callFields = (): Pick<
    ExecuteDispatchToolInput,
    "cwd" | "host" | "sessionId" | "sessionTitle" | "config" | "env" | "fetchImpl" | "userAgent"
  > => ({
    cwd: io.cwd,
    host,
    sessionId,
    sessionTitle: env.DISPATCH_SESSION_TITLE ?? readSessionTitle(dir),
    config,
    env,
    ...(io.fetchImpl === undefined ? {} : { fetchImpl: io.fetchImpl }),
    ...(io.userAgent === undefined ? {} : { userAgent: io.userAgent }),
  });

  // A `dispatch result` record's dry run (`dryRunAsksDispatch`) prints the JSON, then the
  // pull-request body line the record would print, which Dispatch writes and records nothing
  // for. It needs what the record needs and is refused as the record is, so it runs here, after
  // the same configuration, and through the record's own command. Like every dry run that
  // passed, it leaves no entry in this session's ledger.
  if (parsed.dryRun) {
    try {
      const result = await executeDispatchTool({
        ...callFields(),
        tool: parsed.tool,
        args: parsed.args,
        dryRun: true,
      });
      print(`${JSON.stringify(parsed.args, null, 2)}\n\n${result.text}`);
      return OK;
    } catch (error) {
      return refuse(
        messageFor(error),
        error instanceof ToolInputError ? USAGE : REFUSED,
        parsed.tool
      );
    }
  }

  // AskGate: a session a live Oh My Pi process records with its gate bound (its AskGate record in
  // this directory) posts a gated write only with one marker the gate minted for this exact argv,
  // whatever ran the CLI. A session no live process records needs none.
  let uploadBytes: Uint8Array | undefined;
  if (gatedWrite(parsed.tool, parsed.args, io.cwd)) {
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
      const envoy = await sessionEnvoySettings(file, env, {
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
  // The body of an answer written as bytes (`DispatchToolResult.body`), which no line joins.
  let body: Uint8Array | undefined;
  const pictures: string[] = [];
  const notes: string[] = [];
  const problems: string[] = [];
  let entry: ResultInput;
  let code: number;
  try {
    const result = await executeDispatchTool({
      ...callFields(),
      tool,
      args,
      // AskGate's bytes, where its check read the file, are the bytes it digested, so they are sent
      // in place of those read once the flags were read.
      ...defined("uploadBytes", uploadBytes ?? pathBytes),
      subscriptions,
    });
    text = result.text;
    body = result.body;
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
    // A 409 PARENT_STATUS_CONFLICT names each issue that holds the rule and what the refused
    // command's caller does next (`parentStatusConflictLines`), as a near copy's refusal names
    // each open issue.
    text = (parentStatusConflictLines(error, tool, args) ?? [messageFor(error)]).join("\n");
    // The command's write landed but its answer was lost: it is done, so the call succeeded, and
    // its one line says to read back what it made. The executor turns a landing of a write that
    // was only a step of its command (a picture, a closing reason) into a failure of its own. A
    // call that writes nothing whose answer Dispatch kept no copy of (`DispatchAnswerNotKeptError`)
    // made nothing, so it is no landing: it exits 1 below, its line saying to run it again.
    if (error instanceof DispatchWriteLandedError) {
      entry = { tool, details: { landed_without_answer: error.code } };
      code = OK;
    } else {
      // `dispatch api`'s refusal of a write in a session AskGate gates carries AskGate's mark, as
      // the CLI's own refusal of a gated write does, so pi-envoy counts it.
      entry = {
        tool,
        error: text,
        ...(error instanceof ApiGateRefusal ? { askgate: error.askgate } : {}),
      };
      // An argument the executor refuses (a ref naming another owner, a picture that is not
      // there, an id that matches no ask) is the same usage error as one the parse refuses, and
      // exits 2 alike. `--dry-run` runs the call's own check (checkDispatchCall): the owner,
      // schema, cross-field and picture checks. Those exit 2 from the dry run and from its call,
      // so the two stay one answer. A refusal the executor makes during the call, such as an id
      // of the wrong shape or an ask id prefix matching no ask, only the call reaches.
      // Everything else here exits 1. That is Dispatch's own refusal or failure, a Dispatch the
      // command cannot reach, a trade at the secrets broker that fails, and a call that writes
      // nothing whose answer Dispatch kept no copy of.
      code = error instanceof ToolInputError ? USAGE : REFUSED;
    }
  }
  try {
    recordOutcome(dir, sessionId, follows, entry);
  } catch (error) {
    problems.push(messageFor(error));
  }
  const kept = [...notes, ...problems.map(stateWriteFailed)];
  // An answer written as bytes takes standard output alone, as it came: nothing cuts it or moves
  // it to a file, and the CLI's own lines go to standard error. Its ledger line holds its details.
  if (body !== undefined) {
    writeBody(body, kept);
    return code;
  }
  const writeFull = (full: string): string => {
    try {
      return writeLongOutput(dir, full);
    } catch (error) {
      return `not written, ${messageFor(error)}`;
    }
  };
  // A cut JSON object is no JSON object, so a call whose output is one is printed whole on every
  // host, and its kept lines go to standard error.
  if (wholeJson) for (const line of kept) note(line);
  print(
    fitOutput(
      text,
      pictures,
      wholeJson ? [] : kept,
      (host === "claude" || host === "codex") && !wholeJson
        ? CLAUDE_OUTPUT_MAX
        : Number.POSITIVE_INFINITY,
      writeFull
    )
  );
  return code;
}
