import { closeSync, mkdirSync, openSync, readFileSync } from "node:fs";
import * as path from "node:path";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import type { Subprocess } from "bun";
import { z } from "zod";

// Destructured: claude-envoy's typecheck, which compiles this file, forbids `process.env.X`.
const { LEGION_E2E_ENVOY_STACK } = process.env;

/**
 * Whether the tests that run a real natsd and Envoy listener run: they build both with Go, which
 * a Bun-only job lacks, so they run only where `LEGION_E2E_ENVOY_STACK=1` says Go is there. With
 * it set, a missing `go` or a failed build fails the test instead of skipping it.
 */
export const envoyStackEnabled = LEGION_E2E_ENVOY_STACK === "1";

/** A natsd with JetStream and an Envoy listener on it, both on loopback ports of their own. */
export interface EnvoyStack {
  /** The listener's base URL, `http://127.0.0.1:<port>`. */
  readonly envoyUrl: string;
  /** natsd's client URL, `nats://127.0.0.1:<port>`. */
  readonly natsUrl: string;
  /** natsd's client address, `127.0.0.1:<port>`, for a proxy to front. */
  readonly natsHostPort: string;
  /** natsd's monitoring base URL, `http://127.0.0.1:<port>` (`/connz`, `/jsz`). */
  readonly monitorUrl: string;
  /** Stops the listener and natsd. Their scratch directory is the caller's, and goes with it. */
  stop(): Promise<void>;
}

/**
 * Reserves the directory one stack works in — its Go binaries, natsd's store, both homes and
 * both logs — and returns its path.
 *
 * Every test file that starts a stack calls this once per stack, at its own module scope, as
 * `const stackDirectory = useEnvoyStackDirectory()`, and hands the path to `startEnvoyStack`.
 * Two calls reserve two directories, which is what a file that starts two stacks needs.
 *
 * The call cannot live in this module's body: `temporaryDirectory` registers the `beforeAll`
 * that makes the directory and the `afterAll` that removes it where it is called, and Bun
 * evaluates an imported module once per process, so a hook here would take the scope of
 * whichever test file imported this one first.
 *
 * Module scope rather than the `describe` body that starts the stack, because Bun runs a
 * scope's `afterAll` hooks in the order they were registered: a directory reserved beside the
 * stack would be removed before the `afterAll` that stops the processes writing into it, where
 * a file's own `afterAll` runs after every `describe` of the file has finished.
 */
export function useEnvoyStackDirectory(): string {
  return temporaryDirectory("envoy-stack");
}

/** What a caller may change about the stack it starts. */
export interface EnvoyStackOptions {
  /**
   * The directory the stack works in, which the caller reserved with
   * {@link useEnvoyStackDirectory} and the fixture removes again. It exists and is empty when
   * `startEnvoyStack` is called, and one stack has it to itself.
   */
  readonly work: string;
  /**
   * Environment for the listener process, merged over the stack's own, so a caller can add a
   * setting or replace one of these. The session-stall rig (test/session-stall.ts) shortens the
   * session windows (`ENVOY_SESSION_LIVE_WINDOW`, `ENVOY_SESSION_GONE_HORIZON`) with it, which
   * is the only way to cross them in a test's seconds rather than the deployment's hour.
   */
  readonly listenerEnv?: Readonly<Record<string, string>>;
}

/** The Go module both rigs build their binaries from. */
export const ENVOY_MODULE = path.resolve(import.meta.dir, "../../envoy");
const READY_TIMEOUT_MS = 60_000;
const TAIL_LINES = 10;
const STOP_GRACE_MS = 10_000;

/** What a rig builds, and how it names itself when the build cannot run. */
export interface GoBuild {
  /** The directory the binaries go in, one named after each package's own directory. */
  readonly bin: string;
  /** The main packages of {@link ENVOY_MODULE} to build. */
  readonly packages: readonly string[];
  /** The rig, as "the <stack> stack" in the error a missing `go` raises. */
  readonly stack: string;
  /** What that same error says the rig would have built ("natsd and the listener"). */
  readonly binaries: string;
}

/** Builds `build.packages` from this checkout into `build.bin`, a binary each. */
export async function buildGoBinaries(build: GoBuild): Promise<void> {
  if (Bun.which("go") === null) {
    throw new Error(`the ${build.stack} stack needs go on PATH to build ${build.binaries}`);
  }
  // An -o ending in a separator names a directory: each main package's binary goes in it, named
  // after the package's directory.
  const spawned = Bun.spawn(["go", "build", "-o", `${build.bin}${path.sep}`, ...build.packages], {
    cwd: ENVOY_MODULE,
    stdout: "pipe",
    stderr: "pipe",
  });
  const [stderr, code] = await Promise.all([new Response(spawned.stderr).text(), spawned.exited]);
  if (code !== 0) {
    throw new Error(
      `go build ${build.packages.join(" ")} in ${ENVOY_MODULE} exited ${code}: ${stderr}`
    );
  }
}

/** The last `lines` lines of a log file, for an error that names why a process failed. */
export function tail(file: string, lines: number): string {
  try {
    return readFileSync(file, "utf8").trimEnd().split("\n").slice(-lines).join("\n");
  } catch {
    return "(no log)";
  }
}

/** How long a rig gives a process to come up, and how much of its log an error quotes. */
export interface ReadyBudget {
  readonly timeoutMs: number;
  readonly tailLines: number;
}

const ENVOY_READY: ReadyBudget = { timeoutMs: READY_TIMEOUT_MS, tailLines: TAIL_LINES };

/** Polls `ready` until it holds, throwing when `child` exits first or the budget passes. */
export async function awaitReady(
  what: string,
  child: Subprocess,
  log: string,
  budget: ReadyBudget,
  ready: () => Promise<boolean>
): Promise<void> {
  const deadline = Date.now() + budget.timeoutMs;
  for (;;) {
    if (child.exitCode !== null || child.signalCode !== null) {
      throw new Error(
        `${what} exited (${child.exitCode ?? child.signalCode}): ${tail(log, budget.tailLines)}`
      );
    }
    if (await ready().catch(() => false)) return;
    if (Date.now() > deadline) {
      throw new Error(
        `${what} was not ready within ${budget.timeoutMs} ms: ${tail(log, budget.tailLines)}`
      );
    }
    await Bun.sleep(100);
  }
}

/** natsd's line naming the client port it bound (`-p -1`), which nats-server logs as it listens. */
export const NATS_CLIENT_PORT_LINE = /Listening for client connections on 127\.0\.0\.1:(\d+)$/m;
/** natsd's line naming the monitoring port it bound (`-m -1`). */
export const NATS_MONITOR_PORT_LINE = /Starting http monitor on 127\.0\.0\.1:(\d+)$/m;
/** The listener's JSON line naming the address it bound (`PORT=0`). */
export const LISTENER_PORT_LINE = /"msg":"envoy-listener listening".*"addr":"127\.0\.0\.1:(\d+)"/;

/**
 * The port `child` reports it bound: the number `line` captures in `log`, which the process writes
 * once it listens on a port the kernel chose for it. It polls as `awaitReady` does, throwing when
 * `child` exits first or the budget passes.
 *
 * A process that binds port 0 itself holds its port from the kernel's choice on, so no other
 * process can take it before the process serves there. A port this process picked free and handed
 * over by number gives no such hold: another process on the machine can bind it in between, and
 * the stack's own process then fails with "address already in use", or a client of the stack
 * reaches that other process instead.
 */
export async function reportedPort(
  what: string,
  child: Subprocess,
  log: string,
  budget: ReadyBudget,
  line: RegExp
): Promise<number> {
  let port = 0;
  await awaitReady(what, child, log, budget, async () => {
    // The whole log; a log not yet written reads as "(no log)", which no line matches.
    const reported = line.exec(tail(log, Infinity))?.[1];
    if (reported === undefined) return false;
    port = Number(reported);
    return true;
  });
  return port;
}

const StatusSchema = z.object({ status: z.string() });
const JszSchema = z.object({
  account_details: z
    .array(z.object({ stream_detail: z.array(z.object({ name: z.string() })).optional() }))
    .optional(),
});

/** `url`'s JSON answer read with `schema`; undefined when it answers no 2xx or another shape. */
async function getJson<T>(url: string, schema: z.ZodType<T>): Promise<T | undefined> {
  const response = await fetch(url, { signal: AbortSignal.timeout(2_000) });
  if (!response.ok) return undefined;
  const parsed = schema.safeParse(await response.json());
  return parsed.success ? parsed.data : undefined;
}

/**
 * Asks `child` to stop with `signal` — the one that process takes as a stop request — and kills
 * it once the grace has passed.
 */
export async function stopProcess(child: Subprocess, signal: "SIGTERM" | "SIGINT"): Promise<void> {
  child.kill(signal);
  const timer = setTimeout(() => child.kill("SIGKILL"), STOP_GRACE_MS);
  await child.exited;
  clearTimeout(timer);
}

/**
 * Starts natsd (JetStream on) and the listener, each on a loopback port the kernel chose for it and
 * read back from the line it logs once it listens (`reportedPort`), and resolves once the
 * listener's /healthz answers healthy and natsd holds the `ENVOY_NOTIFICATIONS` stream. Throws,
 * naming why, when `go` is missing, a build fails or either process does not come up. The NATS
 * server runs with no authorization at all, so every client may do anything, and the listener
 * holds the grant this checkout's `infra/core.ListenerPermissions` renders, which answers every
 * registration `consumer`.
 *
 * `options.listenerEnv` is merged over the listener's own environment, which is how a caller
 * shortens the session windows the session states are judged by.
 *
 * The stack works in `options.work`, which the caller reserved with
 * {@link useEnvoyStackDirectory}: `stop` leaves it, and the fixture removes it when the file
 * that reserved it is done, whether or not the stack ever reached `stop`.
 */
export async function startEnvoyStack(options: EnvoyStackOptions): Promise<EnvoyStack> {
  const { PATH = "" } = process.env;
  const work = options.work;
  const processes: Subprocess[] = [];
  const stop = async () => {
    for (const child of processes.reverse()) await stopProcess(child, "SIGTERM");
  };
  try {
    const bin = path.join(work, "bin");
    await buildGoBinaries({
      bin,
      packages: ["./internal/testnats/cmd/natsd", "./cmd/listener"],
      stack: "Envoy",
      binaries: "natsd and the listener",
    });
    const natsHome = path.join(work, "nats-home");
    const listenerHome = path.join(work, "listener-home");
    mkdirSync(natsHome);
    mkdirSync(listenerHome);
    const machineID = `envoy-stack-${process.pid}`;
    // -1 is nats-server's "a port the kernel chooses", for the client port and the monitoring one.
    const natsArgs = ["-p", "-1", "-m", "-1", "-store_dir", path.join(work, "nats-store")];
    const natsLog = path.join(work, "natsd.log");
    const natsOut = openSync(natsLog, "a");
    const natsd = Bun.spawn([path.join(bin, "natsd"), ...natsArgs], {
      env: { PATH, HOME: natsHome },
      stdout: natsOut,
      stderr: natsOut,
    });
    closeSync(natsOut);
    processes.push(natsd);
    const natsPort = await reportedPort(
      "natsd's client port",
      natsd,
      natsLog,
      ENVOY_READY,
      NATS_CLIENT_PORT_LINE
    );
    const monitorPort = await reportedPort(
      "natsd's monitoring port",
      natsd,
      natsLog,
      ENVOY_READY,
      NATS_MONITOR_PORT_LINE
    );
    const monitor = `http://127.0.0.1:${monitorPort}`;
    await awaitReady(
      "natsd",
      natsd,
      natsLog,
      ENVOY_READY,
      async () => (await getJson(`${monitor}/healthz`, StatusSchema))?.status === "ok"
    );

    const natsHostPort = `127.0.0.1:${natsPort}`;
    const natsUrl = `nats://${natsHostPort}`;
    const listenerLog = path.join(work, "listener.log");
    const listenerOut = openSync(listenerLog, "a");
    const listener = Bun.spawn([path.join(bin, "listener")], {
      // The caller's environment last, so a test may replace a setting of the stack's own.
      env: {
        PATH,
        HOME: listenerHome,
        // Port 0: the listener binds a port the kernel chooses and logs the address it bound.
        PORT: "0",
        ENVOY_LISTEN_HOST: "127.0.0.1",
        ENVOY_MACHINE_ID: machineID,
        NATS_URLS: natsUrl,
        ...options.listenerEnv,
      },
      stdout: listenerOut,
      stderr: listenerOut,
    });
    closeSync(listenerOut);
    processes.push(listener);
    const listenerPort = await reportedPort(
      "the listener's port",
      listener,
      listenerLog,
      ENVOY_READY,
      LISTENER_PORT_LINE
    );
    const envoyUrl = `http://127.0.0.1:${listenerPort}`;
    await awaitReady(
      "the listener",
      listener,
      listenerLog,
      ENVOY_READY,
      async () => (await getJson(`${envoyUrl}/healthz`, StatusSchema))?.status === "healthy"
    );
    await awaitReady(
      "natsd's ENVOY_NOTIFICATIONS stream",
      natsd,
      natsLog,
      ENVOY_READY,
      async () => {
        const jsz = await getJson(`${monitor}/jsz?streams=true`, JszSchema);
        return (jsz?.account_details ?? []).some((account) =>
          (account.stream_detail ?? []).some((stream) => stream.name === "ENVOY_NOTIFICATIONS")
        );
      }
    );
    return { envoyUrl, natsUrl, natsHostPort, monitorUrl: monitor, stop };
  } catch (error) {
    await stop();
    throw error;
  }
}
