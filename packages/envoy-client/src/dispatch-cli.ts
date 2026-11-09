import { closeSync, constants, fstatSync, openSync, readFileSync } from "node:fs";
import {
  type AskgateMode,
  askgateState,
  bytesDigest,
  consumeMarker,
  type FileDigest,
  fileOperands,
  gatedWrite,
  operandPath,
  readRefusal,
} from "./askgate-marker";
import { newAsks } from "./askgate-new-asks";
import {
  commandHelp,
  commandName,
  commandsHelp,
  type ParsedCommand,
  parseCommand,
} from "./dispatch-command";
import type { ActiveDispatchConfig } from "./dispatch-config";
import type { DispatchHost } from "./dispatch-cwd";
import { checkDispatchCall, dispatchServerUrls, executeDispatchTool } from "./dispatch-execute";
import { DispatchWriteLandedError } from "./dispatch-http";
import { sessionDirectory, sessionStateRoot } from "./dispatch-session-directory";
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

/** Exit codes, as `skill://dispatch` states them: written or read, a refusal, and a usage error.
 *  REFUSED is a call Dispatch itself turned down (a near copy, an AskGate block, a server
 *  refusal); an argument this CLI refuses before any request is USAGE, whether the argv named a
 *  command or not. */
const OK = 0;
const REFUSED = 1;
const USAGE = 2;

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

/**
 * What the agent reads when AskGate reviewed this write and did not send it, and the call
 * reached the CLI anyway: a form the gate does not read ran it. The refusal is the review's, so
 * it stands until a review replaces it, and the way to replace it is to go back through the gate.
 */
const REFUSAL_STANDS =
  "This call carries no marker, so no gate read the command that ran it, and AskGate's refusal of these arguments stands for this session. Send the corrected write, or send this one again as one bash call of literal words so AskGate reviews it afresh; a review that allows it replaces the refusal.";

/**
 * What the agent reads when a marker of this very call, or AskGate's record of its refusals,
 * exists and will not say what it holds. A marker is written whole under a temporary name and
 * renamed, owner-only, so no crash, no full disk and no concurrent sweep leaves one unreadable:
 * a file in that state was changed by something else, and one thing it can hold is a refusal.
 */
const MARKER_UNREADABLE =
  "A marker is written whole and owner-only, so one that cannot be read was changed by something other than AskGate, and what it holds may be a refusal of this very write. The write is not sent on that. Read the file named above, remove it if you know what it was, and send the write again as one bash call of literal words so AskGate reviews it.";

/**
 * What the agent reads when the session's AskGate record cannot be read. This refusal, alone
 * among the ones here, is the one a fault on the box rather than anything about the call can
 * bring on, so it names the recovery; the path it names is in the reason before it. Measured
 * benign causes are none: the record is written whole under a temporary name and renamed, mode
 * 0600 in a 0700 directory set explicitly, so neither a crash, a full disk nor a umask leaves
 * one unreadable, and a record that is simply absent is AskGate off rather than this.
 */
const RECORD_RECOVERY =
  "This is AskGate's own record for this session and nothing about the call, but a record that cannot be read may say this write was refused, so it is not sent. Read the path named above: a session whose Oh My Pi process is gone may have it removed, which turns AskGate off for that session, and a live one writes it again at its next call. Tell whoever owns the session if it is live.";

/**
 * What the agent reads when the marker store itself could not be walked. Same standing as
 * RECORD_RECOVERY: AskGate's own state, named with its path in the reason before it, and the one
 * refusal here a permission drift on the box can bring on rather than anything about the call.
 */
const STORE_RECOVERY =
  "This is AskGate's own state for this session and nothing about the call, but a store that cannot be read may hold a refusal of this write, so it is not sent. The path that could not be read is named above. A session whose Oh My Pi process is gone may have its AskGate record removed, which turns AskGate off for that session; tell whoever owns the session if it is live.";

/** How a gated write gets AskGate's marker, as a refusal or a warn-mode warning tells the agent. */
const LITERAL_FORM =
  "AskGate reviews a Dispatch write, and lets it post, only when it is sent as one bash call of literal words: `dispatch <command> <words…>`. Each word is plain text, text in single quotes (also over more than one line), or text in double quotes with no `$`, backtick or `\\`. A `\\` at the end of a line continues the command. Before it, one `cd <path> &&` is permitted; after it, one `2>&1`, one here-document opened with a quoted delimiter (`<<'EOF'`) and one `| head -n N` or `| tail -n N`. For text with `$` or a backtick in it, use `--<field>-file - <<'EOF'`. A variable, `~`, a glob, an assignment, another redirect (`2>/dev/null`, `> file`), another pipe, another command (`;`, `&&`, `||`), a loop, xargs, `bash -c`, a script, an alias, or a process that a program or an eval cell starts carries no marker.";

/** A line that refuses a call before it reaches Dispatch, each at the start of the line:
 *  `dispatch <command> was not called: …` (an argument problem, exit 2, wherever it is found;
 *  AskGate's block of a write wears the same line and exits 1), or a usage refusal
 *  (exit 2), which is `dispatch: ` and its reason. The CLI's other `dispatch: ` lines refuse
 *  nothing: a result's warning
 *  (`dispatch: warning: `), the note that the session's state was not written
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
 * writes to `CLAUDE_ENV_FILE` (anthropics/claude-code#46696).
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
  /**
   * A warning about a write that posted with no review behind it, on both streams. The literal
   * grammar AskGate reads blesses a trailing `| head -n N` or `| tail -n N`, and either drops
   * the warning when it is on standard output alone, which is where both AskGate warnings used
   * to go — with the ledger broken as well, a reader was left with no account of the write at
   * all. Nothing keys on the stream: `cliRefusals` passes over a `dispatch: warning: ` line
   * wherever it stands, and the gate's own note reads the whole result, which carries both.
   */
  const warn = (text: string): void => {
    const line = text.endsWith("\n") ? text : `${text}\n`;
    io.stdout(line);
    io.stderr(line);
  };
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
  const refuse = (
    text: string,
    code: number,
    tool?: string,
    askgate?: ResultInput["askgate"]
  ): number => {
    print(text);
    const sessionId = claudeSession ?? env.DISPATCH_SESSION_ID;
    if (sessionId !== undefined) {
      try {
        appendResult(sessionDirectory(env, sessionId), {
          tool: tool ?? "dispatch",
          error: text.trimEnd(),
          ...(askgate === undefined ? {} : { askgate }),
        });
      } catch {
        // The session's directory is unusable; the printed refusal stands on its own.
      }
    }
    return code;
  };

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
      : refuse(new ToolInputError(parsed.tool, parsed.problems).message, USAGE, parsed.tool);
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
    // a dry run does not read. It runs with Dispatch off too, reading a dashboard-URL ref only
    // against a server it knows, and its standard output is the JSON alone.
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
    print(JSON.stringify(parsed.args, null, 2));
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
  //
  // One decision point, and one invariant (dispatch://INFRA-2079, row 3.5): a refusal is only
  // ever the verdict of a review that ran, and a write posts unreviewed only where the CLI can
  // positively establish that no verdict exists. Absence counts as a statement only when the
  // evidence store was readable and complete; cannot tell is never nothing there.
  //
  // That second half is the correction. The gate mints nothing when its review rejects
  // (askgate-core's `mints`), so a missing marker is three statements at once: never reviewed,
  // reviewed and rejected, or reviewed and rejected with the evidence unreadable. Reading the
  // gap as "nobody looked" let a rejected write post by being sent again through any form the
  // gate does not read. So a rejection now writes itself down (`readRefusal`), an expired marker
  // is no longer consumed by the refusal it causes, and evidence that exists and cannot be read
  // is its own answer.
  let uploadBytes: Uint8Array | undefined;
  if (gatedWrite(parsed.tool, parsed.args)) {
    const command = commandName(parsed.tool);
    const notCalled = `dispatch ${command} was not called:`;
    /**
     * What the stores say about AskGate's review of this call, and the whole of what decides
     * this branch.
     *
     * - `refused`: AskGate reviewed this write and did not send it. Refuses in either mode: a
     *   verdict is not a mode's to overrule.
     * - `stale`: a review ran and this write is not what its verdict covers. Refuses where the
     *   session chose block, as it always has.
     * - `unknown`: evidence exists and cannot be read, so a refusal may be what it holds.
     *   Refuses where the session chose block, and where no mode could be read at all.
     * - `none`: the stores were read whole and hold neither a marker nor a refusal. The only
     *   state that posts, and the one the fleet meets: a subagent, an eval cell, a script, any
     *   call the gate never read.
     *
     * `why` is the short clause the session's ledger carries, `reason` the one the agent reads,
     * and `advice` what the agent does about it: `refused` where the write was held back,
     * `posted` where it went out, which differ only in what they ask the agent to send next.
     */
    let finding:
      | {
          readonly verdict: "refused" | "stale" | "unknown" | "none";
          readonly mode: AskgateMode;
          readonly why: string;
          readonly reason: string;
          readonly advice: { readonly refused: string; readonly posted: string };
        }
      | undefined;
    try {
      const gate = askgateState(dir);
      if (gate.kind === "broken") {
        finding = {
          verdict: "unknown",
          // No owner file could be read, so the session's own choice is unreadable with it: the
          // stricter of the two stands while its record is broken.
          mode: "block",
          why: gate.problem,
          reason: `AskGate's record for this session, ${gate.problem}, so whether AskGate refused this write cannot be told`,
          advice: { refused: RECORD_RECOVERY, posted: RECORD_RECOVERY },
        };
      } else if (gate.kind === "on") {
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
        if (!spent.ok && spent.verdict === "stale") {
          finding = {
            verdict: "stale",
            mode: gate.mode,
            why: spent.why,
            reason: `AskGate is on for this session, and ${spent.why}`,
            advice:
              spent.changed === undefined
                ? {
                    refused: `${LITERAL_FORM} Send the write again in that form, as a bash call of its own.`,
                    posted: `${LITERAL_FORM} Send the next one in that form.`,
                  }
                : {
                    refused: `${SAME_MESSAGE} Send the call again in a later message than the one that changes the file.`,
                    posted: `${SAME_MESSAGE} Send a write in a later message than the call that changes its file.`,
                  },
          };
        } else if (!spent.ok && spent.verdict === "unknown") {
          finding = {
            verdict: "unknown",
            mode: gate.mode,
            why: spent.why,
            reason: spent.why,
            advice: { refused: MARKER_UNREADABLE, posted: MARKER_UNREADABLE },
          };
        } else if (!spent.ok) {
          // No marker, which AskGate mints for no call it refuses either. Its refusals are kept
          // where nothing sweeps them, and are read by the tool and the arguments rather than by
          // the words of the command, so the same write sent in another spelling meets the same
          // no.
          const refusal = readRefusal(gate.stores, parsed.tool, parsed.args);
          finding =
            refusal === "unknown"
              ? {
                  verdict: "unknown",
                  mode: gate.mode,
                  why: "AskGate's record of its refusals cannot be read",
                  reason: "AskGate's record of its refusals for this session cannot be read",
                  advice: { refused: MARKER_UNREADABLE, posted: MARKER_UNREADABLE },
                }
              : refusal === undefined
                ? {
                    verdict: "none",
                    mode: gate.mode,
                    why: spent.why,
                    reason: `AskGate is on for this session, and ${spent.why}`,
                    advice: {
                      refused: `${LITERAL_FORM} Send the write again in that form, as a bash call of its own.`,
                      posted: `${LITERAL_FORM} Send the next one in that form.`,
                    },
                  }
                : {
                    verdict: "refused",
                    mode: gate.mode,
                    why: "AskGate reviewed this write and did not send it",
                    reason: `AskGate reviewed this write and did not send it: ${refusal.reason}`,
                    advice: { refused: REFUSAL_STANDS, posted: REFUSAL_STANDS },
                  };
        }
      }
    } catch (error) {
      finding = {
        verdict: "unknown",
        mode: "block",
        why: messageFor(error),
        reason: `AskGate's marker for this call could not be checked: ${messageFor(error)}`,
        advice: { refused: STORE_RECOVERY, posted: STORE_RECOVERY },
      };
    }
    if (finding !== undefined) {
      // The asks this write would open for a person, counted from the write's own text: the
      // one number pi-envoy's record of an unmarked write carries, so a session holding a
      // Dispatch token alone can count the asks that went out, or were refused, unreviewed.
      // The ledger carries the count and never the ask text. An upload's asks are in its file,
      // which the dry run's arguments name only by path, so the count reads the bytes the
      // marker check already read — on the two arms that never reached that check, a record
      // that could not be read and a check that threw, there are none, and an upload is counted
      // by its arguments alone. A re-upload's blocks are counted again: the write carries them.
      const asksCarried = newAsks(
        parsed.tool,
        uploadBytes === undefined
          ? parsed.args
          : { ...parsed.args, content: new TextDecoder().decode(uploadBytes) }
      ).length;
      // The one decision. A verdict of no is the session's mode to record and not to overrule;
      // everything short of a verdict is refused only where the session chose block; and only a
      // store read whole and holding nothing lets the write out.
      if (finding.verdict === "refused" || (finding.verdict !== "none" && finding.mode === "block"))
        return refuse(
          `${notCalled} ${finding.reason}. ${finding.advice.refused}`,
          REFUSED,
          parsed.tool,
          {
            mode: "block",
            why: finding.why,
            asksCarried,
          }
        );
      // The ledger line that tells pi-envoy this write went out unreviewed is written before the
      // write is sent, so a reader of the session counts what posted with no review behind it. A
      // ledger that cannot take it no longer stops the write: holding a write back for a record
      // that failed is a refusal no review stands behind, which is the thing this branch exists
      // to prevent. The warnings then stand in for the record, which is why they go to both
      // streams: a trailing `| tail -n 1`, which AskGate's own grammar blesses, takes standard
      // output alone, and a reader of a broken-ledger write would otherwise be left with
      // nothing at all.
      let unrecorded: string | undefined;
      try {
        appendResult(dir, {
          tool: parsed.tool,
          // `mode` says what became of the write and not what the session chose: `warn` is a
          // write that posted, `block` one that was refused, which is what pi-envoy's entry and
          // the operator report read it as. The session's own choice rides beside it, since a
          // write that posts unreviewed in a session that chose block is the row an operator
          // most wants to find.
          askgate: { mode: "warn", chose: finding.mode, why: finding.why, asksCarried },
        });
      } catch (error) {
        unrecorded = messageFor(error);
      }
      warn(
        finding.verdict === "none"
          ? `dispatch: warning: dispatch ${command} posted without review: ${finding.reason}. No review ran for this call, so AskGate refused nothing. ${finding.advice.posted}`
          : `dispatch: warning: AskGate is on for this session in warn mode, and ${finding.why}, so this write posts unreviewed. ${finding.advice.posted}`
      );
      if (unrecorded !== undefined)
        warn(
          `dispatch: warning: dispatch ${command} could not be recorded as unreviewed in this session's ledger: ${unrecorded}. The write posts all the same, and this warning, on both this command's output and its standard error, is its only account.`
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
  let code: number;
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
  try {
    recordOutcome(dir, sessionId, follows, entry);
  } catch (error) {
    problems.push(messageFor(error));
  }
  const kept = [...notes, ...problems.map(stateWriteFailed)];
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
