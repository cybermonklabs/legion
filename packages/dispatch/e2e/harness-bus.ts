/** Each stack's bus in a bus run: one JetStream server per stack, which `e2e/harness-pool.ts`
 *  starts before that stack's harness, hands the stack as `DISPATCH_E2E_NATS_URLS` and stops after
 *  the harness. A run is a bus run when every project it selected is a bus project (`busRunOf`,
 *  e2e/configurations.ts), which `e2e/global-setup.ts` reads.
 *
 *  Both readers of that variable are the stack's own: e2e/run-server.sh hands it to the Dispatch
 *  server as its `NATS_URLS`, which turns the server's outbox on, and e2e/fake-envoy.ts reads the
 *  frames the outbox publishes on `notifications.agent.>` off it and writes the role claims the
 *  outbox reads. One server per stack, as one Postgres per stack: the server creates the
 *  ENVOY_NOTIFICATIONS stream in it at boot, and a stack's fake listener reads every session's
 *  frames, so two stacks on one bus would read each other's.
 *
 *  The server is natsd (packages/envoy/internal/testnats/cmd/natsd): nats-server in one process,
 *  JetStream on and 127.0.0.1 alone, which packages/envoy/scripts/e2e-local.sh and
 *  packages/envoy-client/test/envoy-stack.ts run as their bus too. Its binary is `natsd` in
 *  `DISPATCH_E2E_BROKER_BIN_DIR` when that is set, the directory the binaries job of
 *  .github/workflows/dispatch-browser.yaml builds it into beside the server and the broker, since
 *  the jobs that run the suite run no Go; otherwise this module builds it once per run with the
 *  caller's own toolchain, under a lock, as e2e/run-server.sh builds the server.
 *
 *  It binds ports the kernel chooses (`-p -1 -m -1`, nats-server's RANDOM_PORT: a `-p 0` would be
 *  the default 4222) and logs them, so the client port is read from its own log once it says it
 *  is ready, as e2e-local.sh's `wait_for_nats` and envoy-stack.ts's `natsdAddresses` read it: a
 *  port chosen here and handed to it would be any other process's to take first. Its JetStream
 *  store is under the stack's own directory of the run's, which the pool removes. */
import { execFile } from "node:child_process";
import { accessSync, constants, createWriteStream, mkdirSync } from "node:fs";
import { join } from "node:path";
import type { Readable } from "node:stream";
import { setTimeout as sleep } from "node:timers/promises";
import { fileURLToPath } from "node:url";
import { promisify } from "node:util";

import { Interrupted, Supervisor } from "./local-instance-support";

const execFileAsync = promisify(execFile);

/** The directory whose `natsd` a run starts instead of building one: the binaries job's. */
export const busBinDirVariable = "DISPATCH_E2E_BROKER_BIN_DIR";

/** The envoy module, which natsd is a command of. */
const envoyDir = fileURLToPath(new URL("../../envoy", import.meta.url));

/** Where a run with no `busBinDirVariable` builds natsd, under the module's ignored `out/`. */
const builtDir = join(envoyDir, "out", "dispatch-e2e-bus");

/** How long natsd may take to say it is ready. A JetStream start reads its store directory, which
 *  is empty here, and the server answers in well under a second on a quiet machine. */
const readyMs = 30_000;

/** How many of natsd's last lines a refusal quotes. */
const tailLines = 20;

/** natsd's own readiness line, which it logs once both its listeners are bound. */
const readyLine = "Server is ready";

/** The line natsd names its client port on. */
const clientLine = /Listening for client connections on 127\.0\.0\.1:(\d+)/;

/** One stack's bus. */
export interface StackBus {
  /** `nats://127.0.0.1:<port>`, the stack's `DISPATCH_E2E_NATS_URLS`. */
  readonly url: string;
  /** SIGTERM to natsd, which shuts the server down, and SIGKILL 10 s later if it has not. */
  stop(): Promise<void>;
}

/** The buses of one run, one per stack. */
export interface StackBusProvider {
  /** What a log line and a refusal call the binary every stack's bus runs. */
  readonly name: string;
  /** Stack `index`'s bus, its output in `logDir` as `natsd.log` or, unset, on this process's
   *  stderr as every listener of a one-stack run writes. `signal` is the pool's stop: a Ctrl-C
   *  while this waits ends the wait as `Interrupted`, with natsd stopped. */
  start(index: number, logDir: string | undefined, signal?: AbortSignal): Promise<StackBus>;
}

/** The natsd a run's stacks start: the binaries job's when `busBinDirVariable` names a directory,
 *  refused by name when that directory holds none, else this checkout's, built here. */
async function natsdBinary(env: NodeJS.ProcessEnv, signal?: AbortSignal): Promise<string> {
  const given = (env[busBinDirVariable] ?? "").trim();
  if (given !== "") {
    const binary = join(given, "natsd");
    try {
      accessSync(binary, constants.X_OK);
    } catch {
      throw new Error(
        `The Dispatch e2e harness cannot start a bus: ${busBinDirVariable}=${given} holds no ` +
          "executable natsd. That directory is where the binaries job of " +
          ".github/workflows/dispatch-browser.yaml lands what it built, and that job builds " +
          "./internal/testnats/cmd/natsd beside the server; unset it to build natsd here."
      );
    }
    return binary;
  }
  mkdirSync(builtDir, { recursive: true });
  const binary = join(builtDir, "natsd");
  try {
    // Under a lock, so two runs of one checkout starting at once build one binary in turn; Go
    // rewrites it only when the source changed.
    await execFileAsync(
      "flock",
      [join(builtDir, ".build.lock"), "go", "build", "-o", binary, "./internal/testnats/cmd/natsd"],
      { cwd: envoyDir, env, maxBuffer: 16 * 1024 * 1024, signal }
    );
  } catch (error) {
    if (signal?.aborted === true) throw new Interrupted();
    const failure = error as Error & { stderr?: string };
    throw new Error(
      "The Dispatch e2e harness cannot start a bus: `go build ./internal/testnats/cmd/natsd` in " +
        `${envoyDir} failed: ${(failure.stderr ?? "").trim() || failure.message}`
    );
  }
  return binary;
}

/** natsd's output, copied to `log` or this process's stderr and read line by line for its client
 *  port and its readiness. */
function watchOutput(
  stdout: Readable,
  stderr: Readable,
  log: string | undefined
): {
  port: () => number | undefined;
  ready: () => boolean;
  tail: string[];
  drained: Promise<void>;
} {
  const file = log === undefined ? undefined : createWriteStream(log);
  const tail: string[] = [];
  let port: number | undefined;
  let ready = false;
  const see = (line: string) => {
    tail.push(line);
    if (tail.length > tailLines) tail.shift();
    const match = port === undefined ? clientLine.exec(line) : null;
    if (match !== null) port = Number(match[1]);
    if (line.includes(readyLine)) ready = true;
  };
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
  const drained = Promise.allSettled([pump(stdout), pump(stderr)]).then(() => {
    const ended = Promise.withResolvers<void>();
    if (file === undefined) ended.resolve();
    else file.end(ended.resolve);
    return ended.promise;
  });
  return { drained, port: () => port, ready: () => ready, tail };
}

/** The buses of a run whose own directory is `dir`: stack `index`'s JetStream store is
 *  `<dir>/worker-<index>/nats-store`, beside the data directory the binaries Postgres provider
 *  gives that stack (e2e/harness-postgres.ts). natsd is resolved once, by the first stack to
 *  start, and every other stack waits for that one binary. */
export function stackBus(options: {
  readonly dir: string;
  readonly env?: NodeJS.ProcessEnv;
}): StackBusProvider {
  const env = options.env ?? process.env;
  const given = (env[busBinDirVariable] ?? "").trim();
  let binary: Promise<string> | undefined;
  return {
    name: given === "" ? `natsd built in ${builtDir}` : `natsd of ${busBinDirVariable}=${given}`,
    start: async (index, logDir, signal) => {
      binary ??= natsdBinary(env, signal);
      const natsd = await binary;
      const stackDir = join(options.dir, `worker-${index}`);
      const store = join(stackDir, "nats-store");
      mkdirSync(store, { recursive: true });
      const what = `stack ${index}'s bus (${natsd})`;
      const supervisor = new Supervisor();
      const interrupt = () => supervisor.interrupt();
      signal?.addEventListener("abort", interrupt, { once: true });
      if (signal?.aborted === true) interrupt();
      const running = supervisor.startPiped(
        "natsd",
        [natsd, "-p", "-1", "-m", "-1", "-store_dir", store],
        stackDir,
        process.env
      );
      const log = logDir === undefined ? undefined : join(logDir, "natsd.log");
      if (logDir !== undefined) mkdirSync(logDir, { recursive: true });
      const output = watchOutput(running.child.stdout, running.child.stderr, log);
      const stop = async () => {
        await supervisor.stop();
        await output.drained;
      };
      const refusal = (reason: string) => {
        const where = log === undefined ? "" : ` (${log})`;
        return new Error(
          `The Dispatch e2e harness cannot start: ${what} ${reason}${where}. Its last lines:\n` +
            output.tail.join("\n")
        );
      };
      try {
        const deadline = Date.now() + readyMs;
        for (;;) {
          const port = output.port();
          if (output.ready() && port !== undefined) {
            return { stop, url: `nats://127.0.0.1:${port}` };
          }
          const { child } = running;
          if (child.exitCode !== null || child.signalCode !== null) {
            // The last lines are the reason; let the output reach the tail first.
            await Promise.race([output.drained, sleep(1000)]);
            throw refusal(`exited ${child.exitCode ?? child.signalCode} before it was ready`);
          }
          if (Date.now() > deadline) {
            throw refusal(`did not say "${readyLine}" within ${readyMs / 1000} s`);
          }
          await supervisor.untilInterrupted(sleep(100));
        }
      } catch (error) {
        await stop();
        throw error;
      } finally {
        signal?.removeEventListener("abort", interrupt);
      }
    },
  };
}
