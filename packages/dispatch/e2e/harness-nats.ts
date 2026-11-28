/** Each stack's NATS server, from the nats-server binary a caller names.
 *
 *  A stack's Dispatch server owns the notification stream on the NATS it is given and binds three
 *  durables there, the two delivery intakes' and the Legion state intake's, each under a hold it
 *  takes in its OWN database (packages/envoy/internal/dispatch/delivery/held_durable.go). Two
 *  stacks on one NATS server are therefore two fleets on one durable, each holding a hold of its
 *  own: the stack whose server binds a durable first keeps it, and the other finds it bound to a
 *  deliver subject its hold never recorded and stands by for as long as the run lasts
 *  (`boundElsewhere` there). That stack's intakes deliver nothing and take no work
 *  (`HeldDurable.Queue`), so its Legion intake makes no starting read of a daemon a row names
 *  (`Intake.SettingsChanged`, legion/intake.go), and the state messages its rows publish reach the
 *  other stack's server, which holds no setting of their project. So a stack owns its NATS as it
 *  owns its Postgres (`e2e/harness-postgres.ts`), and the pool starts one per stack.
 *
 *  `DISPATCH_E2E_NATS_BIN` is that binary: `go build -o <path> github.com/nats-io/nats-server/v2`
 *  in packages/envoy builds the version the module links. Each stack's server runs JetStream, with
 *  its store under the stack's own directory of the run (`worker-<index>/nats`, which goes with
 *  the run), on 127.0.0.1 at a port it picks itself (`-p -1`) and reports on its listening line,
 *  so no port is found free and then taken by another process before the server binds it. Its
 *  output goes where its stack's other processes' does: `nats.log` in the stack's log directory,
 *  or this process's stderr for a run of one stack. The pool hands its URL to that stack's
 *  processes as `DISPATCH_E2E_NATS_URLS` (`e2e/run-server.sh`, `e2e/fake-envoy.ts`) and puts it
 *  in the stack's record of the manifest, where a worker reads it (`stackValue`,
 *  `e2e/legion-state-feed.ts`).
 *
 *  A caller's own `DISPATCH_E2E_NATS_URLS` is one server: a run of one stack uses it as it always
 *  has and starts none, and a run of more than one refuses it (`refuseStackConflicts`,
 *  `e2e/harness-ports.ts`). */
import { createWriteStream, existsSync, mkdirSync } from "node:fs";
import { join } from "node:path";
import type { Readable } from "node:stream";

import { Interrupted, Supervisor } from "./local-instance-support";

/** A nats-server binary, which the pool starts once per stack. */
export const natsBinVariable = "DISPATCH_E2E_NATS_BIN";
/** The NATS a stack's processes publish and read through (`e2e/run-server.sh` hands it the server
 *  as its `NATS_URLS`). */
export const natsUrlsVariable = "DISPATCH_E2E_NATS_URLS";
/** The directory under a stack's directory of the run that holds its server's JetStream store. */
export const natsStoreDirectory = "nats";

/** How long a stack's server may take to bind its port and finish its start. */
const readyMs = 30_000;
/** How many of the server's last lines a refusal quotes. */
const tailLines = 20;

/** The line nats-server writes once its client listener is bound, whose group is the port it
 *  picked (`AcceptLoop`, server/server.go). Its start enables JetStream before it binds that
 *  listener. */
const listeningLine = /\[INF\] Listening for client connections on 127\.0\.0\.1:(\d+)$/;
/** The last line of its start (`Start`, server/server.go). */
const readyLine = /\[INF\] Server is ready$/;

/** One stack's NATS server. */
export interface StackNats {
  /** The `DISPATCH_E2E_NATS_URLS` of the stack's processes. */
  readonly url: string;
  /** Rejects as soon as the server exits by itself, naming it. */
  firstExit(): Promise<never>;
  /** Stops the server, and returns once its output is all written. */
  stop(): Promise<void>;
}

/** The binary a run starts every stack's NATS server from. */
export interface StackNatsServers {
  /** What a log line calls it. */
  readonly name: string;
  /** Stack `index`'s server, its output in `logDir` when one is given. `signal` is the pool's own
   *  stop: a Ctrl-C while this waits for the server ends the wait as `Interrupted`. */
  start(index: number, logDir?: string, signal?: AbortSignal): Promise<StackNats>;
}

/** The binary `env` names, which starts a NATS server per stack under the run's own directory
 *  `dir`, or undefined where it names none. A name that is no file is refused before anything
 *  starts. */
export function stackNats(options: {
  /** The run's own temporary directory, which it removes when it ends. */
  readonly dir: string;
  readonly env?: NodeJS.ProcessEnv;
}): StackNatsServers | undefined {
  const bin = ((options.env ?? process.env)[natsBinVariable] ?? "").trim();
  if (bin === "") return undefined;
  if (!existsSync(bin)) {
    throw new Error(
      `The Dispatch e2e harness cannot start: ${natsBinVariable}=${bin} names no file. It must ` +
        "name a nats-server binary, as `go build -o <path> github.com/nats-io/nats-server/v2` " +
        "in packages/envoy builds one."
    );
  }
  return {
    name: `the nats-server ${bin}`,
    start: (index, logDir, signal) => startServer(bin, options.dir, index, logDir, signal),
  };
}

async function startServer(
  bin: string,
  dir: string,
  index: number,
  logDir: string | undefined,
  signal: AbortSignal | undefined
): Promise<StackNats> {
  const stackDir = join(dir, `worker-${index}`);
  const store = join(stackDir, natsStoreDirectory);
  mkdirSync(store, { recursive: true });
  if (logDir !== undefined) mkdirSync(logDir, { recursive: true });
  const what = `stack ${index}'s NATS server`;
  const supervisor = new Supervisor();
  const running = supervisor.startPiped(
    what,
    [bin, "-js", "-a", "127.0.0.1", "-p", "-1", "-sd", store],
    stackDir,
    process.env
  );

  const log = logDir === undefined ? undefined : join(logDir, "nats.log");
  const file = log === undefined ? undefined : createWriteStream(log);
  const tail: string[] = [];
  let port: number | undefined;
  const ready = Promise.withResolvers<number>();
  const see = (line: string): void => {
    tail.push(line);
    if (tail.length > tailLines) tail.shift();
    const listening = port === undefined ? listeningLine.exec(line) : null;
    if (listening !== null) port = Number(listening[1]);
    else if (port !== undefined && readyLine.test(line)) ready.resolve(port);
  };
  // The server's own bytes reach the log file or this process's stderr unchanged, and the same
  // bytes are cut into lines for `see`, as `e2e/harness-start.ts` reads a listener's.
  const pump = async (stream: Readable): Promise<void> => {
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
  // Both streams write the one file, so it ends only once the server has closed both.
  const drained = Promise.allSettled([pump(child.stdout), pump(child.stderr)]).then(() => {
    if (file === undefined) return;
    const closed = Promise.withResolvers<void>();
    file.end(closed.resolve);
    return closed.promise;
  });
  const stop = async (): Promise<void> => {
    await supervisor.stop();
    await drained;
  };
  const refusal = (why: string): Error =>
    new Error(
      `The Dispatch e2e harness cannot start: ${what} ${why}` +
        `${log === undefined ? "" : ` (${log})`}. Its last lines:\n${tail.join("\n")}`
    );

  // What ends the wait first: the start's last line, the server's exit or a failed spawn, the
  // deadline, or the pool's stop.
  const ended = running.exited.then(
    async (code): Promise<never> => {
      // The last lines are the reason, so they reach the tail first.
      await drained;
      throw refusal(`exited ${code ?? child.signalCode} before its start ended`);
    },
    (error: Error): never => {
      throw refusal(`could not be started: ${error.message}`);
    }
  );
  const late = Promise.withResolvers<never>();
  const deadline = setTimeout(
    () => late.reject(refusal(`did not end its start within ${readyMs / 1000} s`)),
    readyMs
  );
  const stopped = Promise.withResolvers<never>();
  const onAbort = (): void => stopped.reject(new Interrupted("interrupted"));
  signal?.addEventListener("abort", onAbort, { once: true });
  if (signal?.aborted === true) onAbort();
  for (const loser of [ended, late.promise, stopped.promise]) loser.catch(() => {});
  try {
    const served = await Promise.race([ready.promise, ended, late.promise, stopped.promise]);
    return {
      firstExit: () => supervisor.firstExit(),
      stop,
      url: `nats://127.0.0.1:${served}`,
    };
  } catch (error) {
    await stop();
    throw error;
  } finally {
    clearTimeout(deadline);
    signal?.removeEventListener("abort", onAbort);
  }
}
