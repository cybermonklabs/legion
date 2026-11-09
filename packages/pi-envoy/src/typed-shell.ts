import { SHELL_COMMAND_REPORT_SEPARATOR } from "@legion/contracts";
import { messageFor } from "@legion/envoy-client/errors";

/**
 * The `!command` a person can send one Oh My Pi session from Dispatch (R4 of dispatch://INFRA-812).
 * A Send or an Aside whose first character is `!` is no prompt: the session runs the rest of it
 * and answers with what it printed. It runs it as typing the same bang command into Oh My Pi's own
 * CLI runs it: the same shell, resolved as the host resolves it, with the host's own login args,
 * its five environment additions and its shell prefix, in the session's own working
 * directory. The mark is one `!` and the rest is the command whatever it starts with, so
 * `!!printf hi` runs `!printf hi` and is answered with that shell's own failure.
 *
 * Nothing else is added to what the CLI does. What this module keeps on top of it, it keeps
 * because the command's answer is a Dispatch reply nobody is watching, and each of those is
 * labelled below as that and not as a rule of its own: the output bound, the time limit, the
 * process group and its kill, the ignored standard input and the absence of a pty.
 *
 * This module runs the command and writes the three texts it becomes; which messages reach it, and
 * where its texts go, is `extensions/envoy.ts`'s (`runTypedInput`). A line typed at the session's
 * own terminal never reaches here, so it keeps the host's own shell mode.
 */

/** Characters of a command's output each of the three texts carries; past it the text is cut and
 *  says so. The CLI shows a command's output in a terminal and bounds nothing; this is what
 *  answering through a Dispatch reply needs, whose body is bounded. */
export const SHELL_OUTPUT_MAX = 8_000;

/** Bytes of a command's output this module keeps, for the same reason the character cut has. Four
 *  times the character cut, so what is kept always holds more characters than the cut keeps,
 *  whatever the encoding costs. Past it every byte is read and thrown away, so a command that
 *  prints without end costs the session this much memory and nothing else at all: no file of its
 *  own, and no room on the session's disk. */
const SHELL_OUTPUT_BYTES = 4 * SHELL_OUTPUT_MAX;

/** How long the reads of a stopped command's two streams are given to finish after its process
 *  group is killed. They end as the pipes close, in milliseconds; this only stops a stream some
 *  process the command left behind still holds from holding the person's reply. */
const SHELL_DRAIN_GRACE_MS = 1_000;

const NOTHING_PRINTED = "The command printed nothing.";

/** What the agent is told before the command and its output: it is the person's command, not a
 *  request of its own. */
const SHELL_COMMAND_HEADING =
  "A person ran this shell command in this session from Dispatch. Nothing is asked of you; this is the command and what it printed.";

/** What a person's `!command` did: what it printed (both streams, in the order their chunks
 *  arrived, cut at `SHELL_OUTPUT_MAX`), and how it ended when it did not end well. The person's
 *  line is not in here: it is theirs as they sent it, and the texts below are handed it with
 *  this, so none of them can write the trimmed command in its place. */
export interface ShellCommandResult {
  readonly output: string;
  readonly cut: boolean;
  /** One sentence naming the exit status of a command that failed, was killed or was stopped;
   *  undefined for one that exited 0. */
  readonly status: string | undefined;
}

/** The command a person's message says to run, or undefined for a message that is no command: the
 *  body whose first character is `!`, without that one character and trimmed, as the CLI's own
 *  bang handler trims it. A body that is a bare `!`, or a `!` and blank space, leaves no command
 *  at all: it is no command, and reaches the model as the prompt it is rather than starting an
 *  empty shell. */
export function shellCommandFrom(body: string): string | undefined {
  if (!body.startsWith("!")) return undefined;
  const command = body.slice(1).trim();
  return command === "" ? undefined : command;
}

/** The directories the host looks in for a shell when `PATH` holds none, in its own order. */
const SHELL_DIRECTORIES = ["/bin", "/usr/bin", "/usr/local/bin", "/opt/homebrew/bin"];

/** The shell a `!command` runs under, resolved as the host resolves its own
 *  (`packages/utils/src/procmgr.ts`): `SHELL` when it names a bash or a zsh this machine can run,
 *  else the first `bash` or `sh` on `PATH`, else the first of those two in the directories above,
 *  else the name `sh` for the spawn itself to resolve. The one part of the host's resolution an
 *  extension cannot read is its `shellPath` setting, which overrides all of this and lives in the
 *  host's own settings, which no extension is handed. */
function userShell(): string {
  const named = Bun.env.SHELL;
  if (named && (named.includes("bash") || named.includes("zsh"))) {
    const runnable = Bun.which(named);
    if (runnable) return runnable;
  }
  for (const name of ["bash", "sh"]) {
    const found = Bun.which(name);
    if (found) return found;
  }
  for (const name of ["bash", "sh"]) {
    for (const directory of SHELL_DIRECTORIES) {
      const found = Bun.which(`${directory}/${name}`);
      if (found) return found;
    }
  }
  return "sh";
}

/** What the host spawns for a bang command, built as its own shell config builds it: the resolved
 *  shell, a login shell unless `PI_BASH_NO_LOGIN` or `CLAUDE_BASH_NO_LOGIN` says otherwise, the
 *  session's environment with the five the host adds over it, and `PI_SHELL_PREFIX` (or
 *  `CLAUDE_CODE_SHELL_PREFIX`) before the command when one is set. */
function shellInvocation(command: string): {
  readonly argv: string[];
  readonly env: Record<string, string | undefined>;
} {
  const shell = userShell();
  const args = Bun.env.PI_BASH_NO_LOGIN || Bun.env.CLAUDE_BASH_NO_LOGIN ? ["-c"] : ["-l", "-c"];
  const prefix = Bun.env.PI_SHELL_PREFIX || Bun.env.CLAUDE_CODE_SHELL_PREFIX;
  const noCI = Bun.env.PI_BASH_NO_CI || Bun.env.CLAUDE_BASH_NO_CI;
  return {
    argv: [shell, ...args, prefix ? `${prefix} ${command}` : command],
    env: {
      ...process.env,
      SHELL: shell,
      GIT_EDITOR: "true",
      GPG_TTY: "not a tty",
      OMPCODE: "1",
      CLAUDECODE: "1",
      ...(noCI ? {} : { CI: "true" }),
    },
  };
}

/** The shell this module starts: no standard input and no pty, and a pipe of its own for each
 *  output stream, which this module reads to its end so the command is never blocked by a full
 *  one. The CLI gives its bang command a pty; these two are what answering through a Dispatch
 *  reply needs, where the answer is text and there is no terminal and nobody at one. */
type ShellSubprocess = Bun.Subprocess<"ignore", "pipe", "pipe">;

/** Kills every process the command started, which its own process group holds. */
function killGroup(pid: number): void {
  try {
    process.kill(-pid, "SIGKILL");
  } catch {
    // The group is already gone.
  }
}

/** The process groups of the commands running now. A command's group is its own, so nothing but
 *  a kill ends it: this process holds the only handle on it, and a group left behind when the
 *  process goes is a `!while :; do :; done` nobody stops. The kill runs as this process exits,
 *  for every way out that is not a signal it cannot handle. The CLI leaves a bang command in the
 *  session's own group, under the eye of the person who typed it; the group and this kill are
 *  what answering through a Dispatch reply needs, where a command nobody is watching must not
 *  outlive the session or fill the pod's disk. */
const runningGroups = new Set<number>();
let killsGroupsOnExit = false;
function killGroupsOnExit(): void {
  if (killsGroupsOnExit) return;
  killsGroupsOnExit = true;
  process.on("exit", () => {
    for (const pid of runningGroups) killGroup(pid);
  });
}

/**
 * The first `SHELL_OUTPUT_BYTES` the command printed, from both of its streams in the order they
 * arrived. Everything past that is read and thrown away rather than kept or written anywhere: a
 * command that prints without end must cost the session a bounded amount of memory, no file of
 * its own, and no room on its disk, and must still not be blocked by a pipe nobody empties.
 */
class BoundedOutput {
  readonly #kept = new Uint8Array(SHELL_OUTPUT_BYTES);
  #length = 0;

  take(chunk: Uint8Array): void {
    const room = this.#kept.length - this.#length;
    if (room <= 0) return;
    const keep = chunk.length <= room ? chunk : chunk.subarray(0, room);
    this.#kept.set(keep, this.#length);
    this.#length += keep.length;
  }

  text(): string {
    return new TextDecoder().decode(this.#kept.subarray(0, this.#length));
  }
}

/** Reads one of the command's streams to its end into `output`. It never throws: a stream the
 *  kill broke is a stream with nothing left to say, and the command's own answer is what the
 *  person is waiting for. */
async function drain(stream: ReadableStream<Uint8Array>, output: BoundedOutput): Promise<void> {
  try {
    for await (const chunk of stream) output.take(chunk);
  } catch {
    // The pipe ended under us; what was read before it did is what the command said.
  }
}

/** Waits for both streams to end, and gives up after `SHELL_DRAIN_GRACE_MS` so a stream some
 *  process the command left behind still holds cannot hold the person's reply. */
async function drained(reads: readonly Promise<void>[]): Promise<void> {
  const { promise, resolve } = Promise.withResolvers<void>();
  const give = setTimeout(resolve, SHELL_DRAIN_GRACE_MS);
  try {
    await Promise.race([Promise.all(reads), promise]);
  } finally {
    clearTimeout(give);
  }
}

/**
 * Runs one `!command` as the CLI runs it - the shell `userShell` resolves, its login args, the
 * host's five environment additions and its shell prefix (`shellInvocation`) - in `cwd`, and
 * answers with what it printed and how it ended.
 *
 * It is stopped after `limitMs` with what it printed kept, which is what answering through a
 * Dispatch reply needs: nobody watches a terminal for this command and nothing else would end
 * one that never ends. It runs in a process group of its own for the same reason, so nothing it
 * left running outlives it or holds the session: the group is killed when the command ends, when
 * `stop` is called, and as this process exits, which is the only handle anything has on a
 * detached group.
 *
 * Both streams are pipes this module reads to their end, keeping the first `SHELL_OUTPUT_BYTES`
 * and throwing the rest away, so `!yes` or `!cat /dev/urandom` costs the session those bytes of
 * memory and nothing more. It writes no file: an earlier shape pointed both streams at one, which
 * bounded what was read back and not what was written, and a command printing without end filled
 * the disk the session runs on. The order of the two streams against each other is the order
 * their chunks arrive in rather than the order the shell wrote them.
 */
export async function runShellCommand(
  command: string,
  options: {
    readonly cwd: string;
    /** How long the command is given before its group is killed, in milliseconds. */
    readonly limitMs: number;
    /** Handed the kill of the command's whole process group, before this call's first await, so
     *  a session that must stop the command before its limit - one shutting down - holds it from
     *  the command's first moment. What it printed up to then is still its answer. */
    readonly started?: (stop: () => void) => void;
  }
): Promise<ShellCommandResult> {
  let child: ShellSubprocess;
  const invocation = shellInvocation(command);
  try {
    killGroupsOnExit();
    child = Bun.spawn(invocation.argv, {
      cwd: options.cwd,
      detached: true,
      env: invocation.env,
      stderr: "pipe",
      stdin: "ignore",
      stdout: "pipe",
    });
  } catch (error) {
    return {
      cut: false,
      output: "",
      status: `The command could not start: ${messageFor(error)}.`,
    };
  }
  runningGroups.add(child.pid);
  // The reads start before the wait: a pipe nobody empties fills, and a command blocked on a
  // full one would never reach its own end.
  const printed = new BoundedOutput();
  const reads = [drain(child.stdout, printed), drain(child.stderr, printed)];
  let ended: "limit" | "stop" | undefined;
  const kill = (why: "limit" | "stop"): void => {
    ended ??= why;
    child.kill("SIGKILL");
    killGroup(child.pid);
  };
  const limit = setTimeout(() => kill("limit"), options.limitMs);
  options.started?.(() => kill("stop"));
  try {
    await child.exited;
  } finally {
    clearTimeout(limit);
    // Whatever the outcome, nothing the command started outlives it - and the pipes close with
    // the last process holding them, which is what ends the reads.
    killGroup(child.pid);
    runningGroups.delete(child.pid);
  }
  await drained(reads);
  const output = printable(printed.text());
  const cut = output.length > SHELL_OUTPUT_MAX;
  return {
    cut,
    output: output.slice(0, SHELL_OUTPUT_MAX).trimEnd(),
    status: statusOf(child, ended, options.limitMs),
  };
}

/**
 * What is kept of what a command printed: every control character but the line break and the tab
 * is dropped. Dispatch cannot store U+0000 at all and refuses a whole reply holding one
 * (400 NUL_CHARACTER), so `!head -c 64 /dev/urandom` or `!find . -print0` would leave the person
 * with no answer; the rest go with it because none of them shows as anything a person reads, and
 * the three texts this output becomes are all read as text.
 */
function printable(output: string): string {
  return output.replace(/\p{Cc}/gu, (control) =>
    control === "\n" || control === "\t" ? control : ""
  );
}

/** How a command ended, as the one sentence the three texts carry, or undefined for one that
 *  exited 0. A command this session stopped says so rather than naming the signal that stopped
 *  it, which is the session's own doing and not the command's. */
function statusOf(
  child: ShellSubprocess,
  ended: "limit" | "stop" | undefined,
  limitMs: number
): string | undefined {
  if (ended === "limit") return `The command was stopped after ${limitMs} ms.`;
  if (ended === "stop") return "The command was stopped.";
  if (child.signalCode !== null) return `The command was killed by ${child.signalCode}.`;
  if (child.exitCode !== 0) return `The command ended with exit status ${child.exitCode}.`;
  return undefined;
}

/**
 * What the command printed, as a fenced code block: every surface this text reaches reads it as
 * Markdown - the live view's conversation, the reply's body on Dispatch, and the agent's own
 * transcript - and unfenced output is read as whatever it happens to look like. A `git diff`'s
 * `---` and `+++` lines become headings, an `ls` holding `*` or `_` loses them, a line of pipes
 * becomes a table, and a `dispatch://` in the output becomes a reference. The fence is longer
 * than the longest run of backticks in the output, so output that is itself fenced stays whole,
 * and it marks where the output ends: what follows it is the session's own words and not the
 * command's.
 */
function fenced(output: string): string {
  let longest = 0;
  for (const run of output.match(/`+/g) ?? []) longest = Math.max(longest, run.length);
  const fence = "`".repeat(Math.max(3, longest + 1));
  return `${fence}\n${output}\n${fence}`;
}

/**
 * The person's line, what the command printed and how it ended: the one text the live view, the
 * reply on Dispatch and the agent's transcript all carry. A command that printed nothing says so,
 * so every `!` message has an answer. The person's line is the first paragraph and the separator
 * follows it, which is the rule the Agents page reads this text back by
 * (`isShellCommandReport`, `@legion/contracts`).
 *
 * `line` is that message as the person sent it and as Dispatch stored it, the mark and every
 * space around it kept: `! ls` opens with `! ls` and `!ls\t` with `!ls\t`, while `ls` is what
 * ran (`shellCommandFrom` trims the command, as the CLI does). The page reads this text back
 * against the stored body, so a text opening with the trimmed command instead matches nothing
 * and the view draws the person's line twice.
 */
export function shellCommandReport(line: string, result: ShellCommandResult): string {
  const parts = [line, result.output === "" ? NOTHING_PRINTED : fenced(result.output)];
  if (result.cut) parts.push(`The output was cut at ${SHELL_OUTPUT_MAX} characters.`);
  if (result.status !== undefined) parts.push(result.status);
  return parts.join(SHELL_COMMAND_REPORT_SEPARATOR);
}

/** The report as the agent reads it in its transcript, under the line that says whose command it
 *  was. The report's own fence is what marks where the output ends here too: a file or a `curl`
 *  response that prints a line like the heading above would otherwise reach the agent as if
 *  Dispatch had written it. */
export function shellCommandMessage(report: string): string {
  return `${SHELL_COMMAND_HEADING}${SHELL_COMMAND_REPORT_SEPARATOR}${report}`;
}

/** The answer a `!` nothing ran for: the person's line as they sent it, the separator, and the
 *  one sentence saying why nothing ran. The two refusals below are this text and a reason, so
 *  the shape a refusal has is written here alone. */
function shellCommandRefusal(line: string, why: string): string {
  return `${line}${SHELL_COMMAND_REPORT_SEPARATOR}The command was not run: ${why}`;
}

/** The answer a `!` gets while this session still runs one: the line, and the command it waits
 *  on. One command runs for a session at a time, and a second is refused rather than queued. */
export function shellCommandBusyReport(line: string, running: string): string {
  return shellCommandRefusal(line, `this session is still running !${running}.`);
}

/** The answer a `!` gets once this session has begun shutting down: nothing was run, and nothing
 *  will be. A command started now would outlive the session that was asked for it. */
export function shellCommandShutdownReport(line: string): string {
  return shellCommandRefusal(line, "this session is shutting down.");
}
