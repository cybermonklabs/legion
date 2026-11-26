/** Starts the Dispatch e2e harness: every row of `e2e/harness-listeners.ts` a run needs, in the
 *  table's order (the fakes, then the Dispatch server, then the plain-HTTP proxy). A row whose
 *  port variable is unset serves on a port the OS chose and stays held until it serves — the
 *  server through a socket `scripts/e2e/lib/held-port.py` holds and hands it, each Bun listener by
 *  binding port 0 — and reports it on its listening line, which this module reads. A row whose
 *  variable is set serves on exactly that port, and an occupied one fails with "address already in
 *  use". Each row starts in the environment the rows before it reported, so the server's names the
 *  fakes' ports and the proxy's the server's.
 *
 *  In two-process mode the round-robin proxy starts before Dispatch A and B, whose ports it
 *  forwards to, since both serve its origin. Their ports are held first (`heldAhead` in
 *  e2e/harness-listeners.ts): a `held-port.py keep` per server holds a port the OS chose from
 *  before the first row starts until the harness stops, its port is in the environment of every
 *  row, and the server serves on it through the keeper's socket.
 *
 *  This is the harness's one starter: `e2e/harness-pool.ts` (Playwright's own runner, through
 *  `e2e/global-setup.ts`), `e2e/local-instance.ts` (`bun run dispatch:local`) and the docs media
 *  harness (`docs/site/media/harness.ts`) call it, then set the ports it returns in the
 *  environment of whatever addresses the harness. A run with `PLAYWRIGHT_BASE_URL` starts only
 *  the rows marked `deployed`, the fakes a deployed server is tested with. The pool calls it once
 *  per stack, each call with that stack's database and file-store bucket in `options.env`
 *  (`e2e/harness-ports.ts`), so one process starts a whole harness per Playwright worker.
 *
 *  Nothing here is Bun's, because Playwright's runner is node's: the processes are
 *  `node:child_process`'s (`e2e/local-instance-support.ts`), and their output is read as a node
 *  stream. The listeners themselves are still bun programs, started as `bun <script>`
 *  (`e2e/harness-listeners.ts`). */

import type { ChildProcess } from "node:child_process";
import {
  createWriteStream,
  existsSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  rmSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import type { Readable } from "node:stream";
import { text } from "node:stream/consumers";
import { setTimeout as sleep } from "node:timers/promises";
import { fileURLToPath } from "node:url";

import { type HarnessListener, targetedListeners } from "./harness-listeners";
import { harnessPorts, harnessVariables } from "./harness-ports";
import {
  announceWait,
  Interrupted,
  interruptedBefore,
  type PipedRunning,
  Supervisor,
} from "./local-instance-support";

/** What this module's own lines are prefixed with, as every starter in `e2e/` names itself. */
const named = "e2e/harness-start.ts";

const dispatchPackage = fileURLToPath(new URL("..", import.meta.url));
const heldPortScript = fileURLToPath(
  new URL("../../../scripts/e2e/lib/held-port.py", import.meta.url)
);
/** How many of a process's last lines a refusal quotes. */
const tailLines = 20;

export interface HarnessProcess {
  readonly listener: HarnessListener;
  /** The process, the leader of a process group of its own, as `node:child_process` returns it:
   *  `exitCode` and `signalCode` say how it ended. */
  readonly child: ChildProcess;
  /** The file its output goes to under `logDir`, or undefined when it goes to this process's
   *  stderr. */
  readonly log: string | undefined;
  /** Settles once the process has closed its output and all of it is written. */
  readonly drained: Promise<void>;
}

export interface Harness {
  /** The port each started listener serves on, by its variable (`DISPATCH_E2E_PORT`,
   *  `FAKE_ENVOY_PORT`, `FAKE_GITHUB_PORT`, `FAKE_BROKER_PORT`, `PLAIN_HTTP_PORT`, and in
   *  two-process mode `DISPATCH_E2E_PORT_A` and `DISPATCH_E2E_PORT_B`): the environment a process
   *  that addresses the harness needs. */
  readonly env: Readonly<Record<string, string>>;
  readonly processes: readonly HarnessProcess[];
  /** Rejects as soon as one of the processes exits by itself, naming it. */
  firstExit(): Promise<never>;
  /** SIGTERM to every process group, SIGKILL to whatever still runs 10 s later; resolves once
   *  every leader has exited. */
  stop(): Promise<void>;
}

export interface HarnessOptions {
  /** Variables every process this harness starts gets on top of this process's own, and which
   *  its ports resolve through: one stack's database and file-store bucket
   *  (`e2e/harness-pool.ts`), so one process can start a harness per stack. Absent, the harness
   *  is this process's environment, which is what every other starter gives it. */
  readonly env?: Readonly<Record<string, string>>;
  /** The stack this harness starts for, as the pool numbers it (`stack 2`), which every line its
   *  waits write carries (`announceWait`): the pool starts its stacks at once, and their lines
   *  interleave. Unset, the lines name no stack, which is what a caller that starts one harness
   *  needs. */
  readonly label?: string;
  /** A directory for each process's output, as `<id>.log`; unset, it goes to this process's
   *  stderr. */
  readonly logDir?: string;
  /** Aborting it while the harness starts stops every process started so far, and the start
   *  rejects with `Interrupted`. */
  readonly signal?: AbortSignal;
}

interface Watched extends HarnessProcess {
  /** The port on its listening line, once the line has been read. */
  readonly reported: () => number | undefined;
  /** Its last lines of output. */
  readonly tail: readonly string[];
  /** What starting it failed with, once that is known (`startFailure`). */
  readonly failedToStart: () => Error | undefined;
}

/** What starting `running` failed with, once its `exited` has rejected: a command that could not
 *  be started at all, such as one that is not on PATH. That rejection is the one sign of it under
 *  both runtimes: Node marks such a child with an `exitCode` of -2, but Bun leaves `exitCode` and
 *  `signalCode` null, and a wait that read only those would wait for a report that cannot come. */
function startFailure(running: PipedRunning): () => Error | undefined {
  let failure: Error | undefined;
  running.exited.catch((error: unknown) => {
    failure = error instanceof Error ? error : new Error(String(error));
  });
  return () => failure;
}

/** Copies the process's output to `logDir` or this process's stderr, and reads each line for the
 *  listener's report. */
function watch(running: PipedRunning, listener: HarnessListener, logDir?: string): Watched {
  const log = logDir === undefined ? undefined : join(logDir, `${listener.id}.log`);
  const file = log === undefined ? undefined : createWriteStream(log);
  const tail: string[] = [];
  let port: number | undefined;
  const see = (line: string) => {
    tail.push(line);
    if (tail.length > tailLines) tail.shift();
    const match = port === undefined ? listener.report.exec(line) : null;
    if (match !== null) port = Number(match[1]);
  };
  // The process's own bytes reach the log file or this process's stderr unchanged, and the same
  // bytes are cut into lines for `see`, which is what finds the listener's reported port. The
  // stream is `node:child_process`'s, so it is iterated rather than read through a web reader;
  // its chunks are Buffers, which `write` and `TextDecoder` both take as they are.
  const pump = async (stream: Readable) => {
    const decoder = new TextDecoder();
    let partial = "";
    for await (const chunk of stream as AsyncIterable<Uint8Array>) {
      if (file === undefined) process.stderr.write(chunk);
      else file.write(chunk);
      const lines = (partial + decoder.decode(chunk, { stream: true })).split("\n");
      partial = lines.pop() ?? "";
      for (const line of lines) see(line);
    }
    partial += decoder.decode();
    if (partial !== "") see(partial);
  };
  const { child } = running;
  // Both streams write the one file, so it ends only once the process has closed both.
  const drained = Promise.allSettled([pump(child.stdout), pump(child.stderr)]).then(
    () => new Promise<void>((resolve) => (file === undefined ? resolve() : file.end(resolve)))
  );
  return {
    child,
    drained,
    failedToStart: startFailure(running),
    listener,
    log,
    reported: () => port,
    tail,
  };
}

/** The refusal for a listener that did not come up, quoting its last lines. An explicitly set port
 *  it could not bind is named as such. */
function refusal(watched: Watched, given: number, what: string): Error {
  const { listener, log, tail } = watched;
  const lines = tail.join("\n");
  const inUse = given !== 0 && /EADDRINUSE|address already in use/.test(lines);
  const head = inUse
    ? `${listener.name} cannot listen on ${harnessVariables[listener.key]}=${given}: address already in use`
    : `${listener.name} ${what}`;
  const where = log === undefined ? "" : ` (${log})`;
  return new Error(
    `The Dispatch e2e harness cannot start: ${head}${where}. Its last lines:\n${lines}`
  );
}

/** ` (pid N)` for a process that started, and nothing for one that could not be started, whose
 *  `pid` Bun leaves undefined. */
function pidOf(child: ChildProcess): string {
  return child.pid === undefined ? "" : ` (pid ${child.pid})`;
}

/** Waits for the listener's report and, for a listener with a health path, for that path to
 *  answer 200, and returns the port it serves on. It fails as soon as the process exits, so a
 *  server whose build failed is reported at once.
 *
 *  The wait has no bound of its own. How long a process takes to start and print its line is
 *  the machine's, and a loaded machine can hold a start past any fixed bound: a bun listener's
 *  start held 35 s failed the 30 s each listener was once given, under a test whose own deadline
 *  was 30 minutes away. So the wait ends on the report, the 200, the process's exit or its failure
 *  to start at all (`failedToStart`), or the caller's abort (`signal`), and the caller's deadline is
 *  the only hang guard: a test's bounded child (`e2e/__tests__/bounded-subprocess.ts`), a CI job's
 *  limit, a person's Ctrl-C. An abort names the process the start was waiting for
 *  (`interruptedBefore`), and each wait writes when it begins and ends, as `who` (`announceWait`). */
async function serving(
  watched: Watched,
  given: number,
  supervisor: Supervisor,
  who: string
): Promise<number> {
  const { child, listener } = watched;
  const since = Date.now();
  const check = async (fact: string) => {
    const failed = watched.failedToStart();
    if (failed !== undefined) {
      throw refusal(watched, given, `could not be started: ${failed.message}`);
    }
    if (child.exitCode !== null || child.signalCode !== null) {
      // The last lines are the reason; let the output reach the tail first.
      await Promise.race([watched.drained, sleep(1000)]);
      throw refusal(
        watched,
        given,
        `exited ${child.exitCode ?? child.signalCode} before it ${fact}`
      );
    }
    try {
      await supervisor.untilInterrupted(sleep(100));
    } catch (error) {
      if (error instanceof Interrupted) throw interruptedBefore(`${listener.name} ${fact}`, since);
      throw error;
    }
  };
  let port = watched.reported();
  if (port === undefined) {
    const waited = announceWait(who, `${listener.name}${pidOf(child)} reports its port`);
    try {
      while (port === undefined) {
        await check("reported its port");
        port = watched.reported();
      }
    } finally {
      waited();
    }
  }
  if (listener.health === undefined) return port;
  const url = `http://127.0.0.1:${port}${listener.health}`;
  const answered = `answered 200 at ${url}`;
  const waited = announceWait(who, `${listener.name} answers 200 at ${url}`);
  try {
    for (;;) {
      try {
        const response = await supervisor.untilInterrupted(
          fetch(url, { signal: AbortSignal.timeout(5000) })
        );
        if (response.ok) return port;
      } catch {
        if (supervisor.signal.aborted) {
          throw interruptedBefore(`${listener.name} ${answered}`, since);
        }
        // not answering yet
      }
      await check(answered);
    }
  } finally {
    waited();
  }
}

/** A port held for a `heldAhead` row, and the unix socket its keeper hands it out on. */
interface Kept {
  readonly port: number;
  readonly socket: string;
}

/** Starts a `held-port.py keep` in `dir` that holds a port the OS chose for `listener` until the
 *  harness stops, and returns once the keeper has written the port. Like `serving`, it waits with
 *  no bound of its own: until the port file, the keeper's exit or its failure to start at all, or
 *  the caller's abort, and writes when that wait begins and ends, as `who`. */
async function keepPort(
  listener: HarnessListener,
  dir: string,
  supervisor: Supervisor,
  environment: NodeJS.ProcessEnv,
  who: string
): Promise<Kept> {
  const name = `the port keeper for ${listener.name}`;
  const portFile = join(dir, `${listener.id}.port`);
  const socket = join(dir, `${listener.id}.sock`);
  const running = supervisor.startPiped(
    name,
    ["python3", heldPortScript, "keep", "--port-file", portFile, "--socket", socket],
    dispatchPackage,
    environment
  );
  const { child } = running;
  const failedToStart = startFailure(running);
  // Both streams are read to their end, so a full pipe never stops the keeper; its stderr is the
  // reason it failed. They are node streams, so `node:stream/consumers`' `text` collects them
  // rather than `new Response(...)`, which takes no node stream.
  const stderr = text(child.stderr).catch(() => "");
  text(child.stdout).catch(() => "");
  const since = Date.now();
  const waited = announceWait(who, `${name}${pidOf(child)} holds a port`);
  try {
    // The keeper writes the file through a rename once it serves its socket, so a file that
    // exists holds the whole port.
    while (!existsSync(portFile)) {
      const failed = failedToStart();
      if (failed !== undefined) {
        throw new Error(
          `The Dispatch e2e harness cannot start: ${name} could not be started: ${failed.message}`
        );
      }
      if (child.exitCode !== null || child.signalCode !== null) {
        throw new Error(
          `The Dispatch e2e harness cannot start: ${name} exited ` +
            `${child.exitCode ?? child.signalCode}: ${(await stderr).trim()}`
        );
      }
      try {
        await supervisor.untilInterrupted(sleep(100));
      } catch (error) {
        if (error instanceof Interrupted) throw interruptedBefore(`${name} held a port`, since);
        throw error;
      }
    }
  } finally {
    waited();
  }
  return { port: Number(readFileSync(portFile, "utf8").trim()), socket };
}

/** Starts the harness (see the module comment) and returns once every listener serves. A failure
 *  stops every process it started before it throws. */
export async function startHarness(options: HarnessOptions = {}): Promise<Harness> {
  // This harness's environment: this process's, under the stack's own variables when a caller
  // starts more than one harness (`options.env`). Every port below resolves through it, and
  // every process started inherits it.
  const environment: NodeJS.ProcessEnv = { ...process.env, ...options.env };
  // Refuses a malformed or duplicated port before anything starts.
  const given = harnessPorts(environment);
  const listeners = targetedListeners();
  if (options.logDir !== undefined) mkdirSync(options.logDir, { recursive: true });
  // What each wait's lines are written as: this module, and the stack where the caller named one.
  const who = options.label === undefined ? named : `${named}: ${options.label}`;
  const supervisor = new Supervisor();
  const interrupt = () => supervisor.interrupt();
  options.signal?.addEventListener("abort", interrupt, { once: true });
  if (options.signal?.aborted) interrupt();
  let keeperDir: string | undefined;
  const stop = async () => {
    await supervisor.stop();
    if (keeperDir !== undefined) rmSync(keeperDir, { force: true, recursive: true });
  };
  const env: Record<string, string> = {};
  const kept = new Map<HarnessListener, Kept>();
  const processes: Watched[] = [];
  try {
    for (const listener of listeners) {
      if (listener.heldAhead === undefined || given[listener.key].port !== 0) continue;
      keeperDir ??= mkdtempSync(join(tmpdir(), "dispatch-e2e-held-"));
      const held = await keepPort(listener, keeperDir, supervisor, environment, who);
      kept.set(listener, held);
      env[harnessVariables[listener.key]] = String(held.port);
    }
    for (const listener of listeners) {
      const held = kept.get(listener);
      const heldInputs =
        listener.heldAhead === undefined
          ? {}
          : held === undefined
            ? { [listener.heldAhead.portEnv]: String(given[listener.key].port) }
            : { [listener.heldAhead.keeperEnv]: held.socket };
      const running = supervisor.startPiped(listener.name, listener.argv, dispatchPackage, {
        ...environment,
        ...env,
        ...listener.env,
        ...heldInputs,
      });
      const watched = watch(running, listener, options.logDir);
      processes.push(watched);
      env[harnessVariables[listener.key]] = String(
        await serving(watched, given[listener.key].port, supervisor, who)
      );
    }
  } catch (error) {
    await stop();
    throw error;
  } finally {
    options.signal?.removeEventListener("abort", interrupt);
  }
  return {
    env,
    firstExit: () => supervisor.firstExit(),
    processes,
    stop,
  };
}
