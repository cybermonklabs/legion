import { messageFor } from "@legion/envoy-client/errors";

/**
 * The `!command` a person can send one Oh My Pi session from Dispatch (R4 of dispatch://INFRA-812).
 * A Send or an Aside whose first character is `!` is no prompt: the session runs the rest of it
 * under `/bin/sh -c` in its own working directory and answers with what it printed. The mark is
 * one `!` and the rest is the command whatever it starts with, so `!!printf hi` runs `!printf hi`
 * and is answered with that shell's own failure.
 *
 * This module runs the command and writes the three texts it becomes; which messages reach it, and
 * where its texts go, is `extensions/envoy.ts`'s (`runTypedInput`). A line typed at the session's
 * own terminal never reaches here, so it keeps the host's own shell mode.
 */

/** Characters of a command's output each of the three texts carries; past it the text is cut and
 *  says so. */
export const SHELL_OUTPUT_MAX = 8_000;

/** Bytes of a command's output this module keeps. Four times the character cut, so what is kept
 *  always holds more characters than the cut keeps, whatever the encoding costs. Past it every
 *  byte is read and thrown away, so a command that prints without end costs the session this
 *  much memory and nothing else at all: no file of its own, and no room on the session's disk. */
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

/** What a person's `!command` did: the command without its mark, what it printed (both streams, in
 *  the order their chunks arrived, cut at `SHELL_OUTPUT_MAX`), and how it ended when it did not
 *  end well. */
export interface ShellCommandResult {
  readonly command: string;
  readonly output: string;
  readonly cut: boolean;
  /** One sentence naming the exit status of a command that failed, was killed or was stopped;
   *  undefined for one that exited 0. */
  readonly status: string | undefined;
}

/** The command a person's message says to run, or undefined for a message that is no command: the
 *  body whose first character is `!`, without that one character. */
export function shellCommandFrom(body: string): string | undefined {
  return body.startsWith("!") ? body.slice(1) : undefined;
}

/** The shell this module starts: no standard input, and a pipe of its own for each output
 *  stream, which this module reads to its end so the command is never blocked by a full one. */
type ShellSubprocess = Bun.Subprocess<"ignore", "pipe", "pipe">;

/** Kills every process the command started, which its own process group holds. */
function killGroup(pid: number): void {
  try {
    process.kill(-pid, "SIGKILL");
  } catch {
    // The group is already gone.
  }
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
 * Runs one `!command` under `/bin/sh -c` in `cwd` with this session's own environment, and answers
 * with what it printed and how it ended. It is stopped after `limitMs` with what it printed kept,
 * and runs in a process group of its own, so nothing it left running outlives it or holds the
 * session.
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
  options: { readonly cwd: string; readonly limitMs: number }
): Promise<ShellCommandResult> {
  let child: ShellSubprocess;
  try {
    child = Bun.spawn(["/bin/sh", "-c", command], {
      cwd: options.cwd,
      detached: true,
      env: process.env,
      stderr: "pipe",
      stdin: "ignore",
      stdout: "pipe",
    });
  } catch (error) {
    return {
      command,
      cut: false,
      output: "",
      status: `The command could not start: ${messageFor(error)}.`,
    };
  }
  // The reads start before the wait: a pipe nobody empties fills, and a command blocked on a
  // full one would never reach its own end.
  const printed = new BoundedOutput();
  const reads = [drain(child.stdout, printed), drain(child.stderr, printed)];
  let stopped = false;
  const limit = setTimeout(() => {
    stopped = true;
    child.kill("SIGKILL");
    killGroup(child.pid);
  }, options.limitMs);
  try {
    await child.exited;
  } finally {
    clearTimeout(limit);
    // Whatever the outcome, nothing the command started outlives it - and the pipes close with
    // the last process holding them, which is what ends the reads.
    killGroup(child.pid);
  }
  await drained(reads);
  const output = printed.text();
  const cut = output.length > SHELL_OUTPUT_MAX;
  return {
    command,
    cut,
    output: output.slice(0, SHELL_OUTPUT_MAX).trimEnd(),
    status: statusOf(child, stopped, options.limitMs),
  };
}

/** How a command ended, as the one sentence the three texts carry, or undefined for one that
 *  exited 0. A command this session stopped says so rather than naming the signal that stopped
 *  it, which is the session's own doing and not the command's. */
function statusOf(child: ShellSubprocess, stopped: boolean, limitMs: number): string | undefined {
  if (stopped) return `The command was stopped after ${limitMs} ms.`;
  if (child.signalCode !== null) return `The command was killed by ${child.signalCode}.`;
  if (child.exitCode !== 0) return `The command ended with exit status ${child.exitCode}.`;
  return undefined;
}

/**
 * The person's line, what the command printed and how it ended: the one text the live view, the
 * reply on Dispatch and the agent's transcript all carry. A command that printed nothing says so,
 * so every `!` message has an answer.
 */
export function shellCommandReport(result: ShellCommandResult): string {
  const parts = [`!${result.command}`, result.output === "" ? NOTHING_PRINTED : result.output];
  if (result.cut) parts.push(`The output was cut at ${SHELL_OUTPUT_MAX} characters.`);
  if (result.status !== undefined) parts.push(result.status);
  return parts.join("\n\n");
}

/** The report as the agent reads it in its transcript, under the line that says whose command it
 *  was. */
export function shellCommandMessage(report: string): string {
  return `${SHELL_COMMAND_HEADING}\n\n${report}`;
}

/** The answer a `!` gets while this session still runs one: the line, and the command it waits
 *  on. One command runs for a session at a time, and a second is refused rather than queued. */
export function shellCommandBusyReport(command: string, running: string): string {
  return `!${command}\n\nThe command was not run: this session is still running !${running}.`;
}
