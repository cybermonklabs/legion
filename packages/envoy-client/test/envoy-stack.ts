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

/**
 * A server a stack starts on a port the stack chooses and holds. natsd is none: it binds ports the
 * kernel chooses for it.
 */
export type StackServer = "dispatch" | "listener";

/**
 * Called with each port a stack chooses, once it has chosen the port for `server` and before it
 * starts `server`: a test takes the port here, as any other program on the machine could, to show
 * that the stack starts its servers all the same.
 */
export type PortChosen = (server: StackServer, port: number) => Promise<void>;

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
  /** Called with each port the stack chooses, before the server it is for starts. */
  readonly portChosen?: PortChosen;
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

/** How a stack starts one of its servers. */
interface ServerStart {
  readonly command: readonly string[];
  readonly env: Readonly<Record<string, string>>;
  /** The file the server's output is appended to. */
  readonly log: string;
  readonly cwd?: string;
}

/**
 * Starts a stack's server with its output appended to `start.log`. With `held`, the server
 * inherits the descriptor `held.fd` as its own descriptor 3, which `held.fdEnv` names.
 */
function startServer(
  start: ServerStart,
  held?: { readonly fd: number; readonly fdEnv: string }
): Subprocess {
  const out = openSync(start.log, "a");
  try {
    return Bun.spawn([...start.command], {
      // Left out when unset: claude-envoy compiles this file with exactOptionalPropertyTypes.
      ...(start.cwd === undefined ? {} : { cwd: start.cwd }),
      // stdio entry 3 is the server's descriptor 3.
      env: held === undefined ? { ...start.env } : { ...start.env, [held.fdEnv]: "3" },
      stdio: held === undefined ? ["ignore", out, out] : ["ignore", out, out, held.fd],
    });
  } finally {
    closeSync(out);
  }
}

/**
 * Starts `server` on a loopback port the kernel chooses and the stack holds from that moment until
 * the server serves on it, so no other program can take the port in between: the stacks' form of
 * the Go tests' `cmdtest.HoldPort` and `Pass` (packages/envoy/internal/cmdtest). The stack opens a
 * listening socket on port 0, shows its port to `portChosen`, and starts the server with what
 * `start` makes of the port; the server inherits the socket as its descriptor 3, which `fdEnv`
 * names (DISPATCH_LISTEN_FD, ENVOY_LISTEN_FD), and serves on it without binding anything.
 *
 * A port found free, released and bound later can be taken in that gap, and the server then exits
 * with `bind: address already in use`; many stacks starting at one time on one machine meet that.
 */
export async function startOnHeldPort(
  server: StackServer,
  fdEnv: string,
  portChosen: PortChosen | undefined,
  start: (port: number) => ServerStart
): Promise<{ readonly child: Subprocess; readonly port: number }> {
  // Bun.listen binds and listens before it returns, so the port is held from the moment it is
  // known, on a close-on-exec socket that no other process this one starts inherits.
  const listener = Bun.listen({ hostname: "127.0.0.1", port: 0, socket: { data() {} } });
  try {
    // Bun's types leave the listener's descriptor out; Bun 1.4 has it as `fd`.
    if (!("fd" in listener) || typeof listener.fd !== "number") {
      throw new Error(
        `Bun.listen gave the socket held for ${server} no descriptor (fd) to pass on`
      );
    }
    const fd = listener.fd;
    await portChosen?.(server, listener.port);
    return { child: startServer(start(listener.port), { fd, fdEnv }), port: listener.port };
  } finally {
    // This process's copy closes once the server has started or failed to, as cmdtest.Start
    // closes a socket Pass handed on: from then on the server alone holds it, and its exit frees
    // the port. Nothing waits between the start and this close, so this process accepts no
    // connection on the socket.
    listener.stop(true);
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
 * Starts natsd (JetStream on), provisions it with the buckets a listener copies (the
 * fixturebuckets command; no listener creates one) and starts the listener on loopback ports, and
 * resolves once the listener's /healthz answers healthy and natsd holds the `ENVOY_NOTIFICATIONS`
 * stream. Throws, naming why, when `go` is missing, a build fails, the provisioning fails or either
 * process does not come up. The NATS server runs with no authorization at all, so every client may
 * do anything, and the listener holds the grant this checkout's `infra/core.ListenerPermissions`
 * renders, which answers every registration `consumer`.
 *
 * `options.listenerEnv` is merged over the listener's own environment, which is how a caller
 * shortens the session windows the session states are judged by.
 *
 * The stack works in `options.work`, which the caller reserved with
 * {@link useEnvoyStackDirectory}: `stop` leaves it, and the fixture removes it when the file
 * that reserved it is done, whether or not the stack ever reached `stop`.
 *
 * No port is free between its choice and its server's bind. natsd takes no socket, so it binds
 * ports the kernel chooses (`-p -1 -m -1`, nats-server's RANDOM_PORT) and the stack reads them
 * from natsd's own log; the listener serves on a socket the stack holds ({@link startOnHeldPort}).
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
      packages: [
        "./internal/testnats/cmd/natsd",
        "./internal/testnats/cmd/fixturebuckets",
        "./cmd/listener",
      ],
      stack: "Envoy",
      binaries: "natsd, fixturebuckets and the listener",
    });
    const natsHome = path.join(work, "nats-home");
    const listenerHome = path.join(work, "listener-home");
    mkdirSync(natsHome);
    mkdirSync(listenerHome);
    const natsLog = path.join(work, "natsd.log");
    // -1 is nats-server's RANDOM_PORT: natsd binds client (-p) and monitoring (-m) ports the
    // kernel chooses, and logs them. A -p of 0 would mean the default 4222, and an -m of 0 no
    // monitoring at all.
    const natsArgs = ["-p", "-1", "-m", "-1", "-store_dir", path.join(work, "nats-store")];
    const natsd = startServer({
      command: [path.join(bin, "natsd"), ...natsArgs],
      env: { PATH, HOME: natsHome },
      log: natsLog,
    });
    processes.push(natsd);
    await awaitReady("natsd", natsd, natsLog, ENVOY_READY, async () => {
      const reported = natsdAddresses(natsLog);
      if (reported === undefined) return false;
      return (await getJson(`http://${reported.monitor}/healthz`, StatusSchema))?.status === "ok";
    });
    // Ready means natsd reported both addresses, and its log only grows.
    const addresses = natsdAddresses(natsLog);
    if (addresses === undefined) throw new Error(`natsd reported no ports: ${tail(natsLog, 10)}`);
    const natsHostPort = addresses.client;
    const natsUrl = `nats://${natsHostPort}`;
    const monitor = `http://${addresses.monitor}`;
    // No listener creates a bucket it copies: the stack's own natsd is given them first.
    const provision = Bun.spawn([path.join(bin, "fixturebuckets"), "-url", natsUrl], {
      env: { PATH },
      stdout: "ignore",
      stderr: "pipe",
    });
    const [provisionError, provisionCode] = await Promise.all([
      new Response(provision.stderr).text(),
      provision.exited,
    ]);
    if (provisionCode !== 0) {
      throw new Error(`fixturebuckets -url ${natsUrl} exited ${provisionCode}: ${provisionError}`);
    }

    const listenerLog = path.join(work, "listener.log");
    const { child: listener, port: listenerPort } = await startOnHeldPort(
      "listener",
      "ENVOY_LISTEN_FD",
      options.portChosen,
      (port) => ({
        command: [path.join(bin, "listener")],
        // The caller's environment last, so a test may replace a setting of the stack's own.
        env: {
          PATH,
          HOME: listenerHome,
          // With ENVOY_LISTEN_FD these name the held socket's address, which the listener checks.
          PORT: String(port),
          ENVOY_LISTEN_HOST: "127.0.0.1",
          ENVOY_MACHINE_ID: `envoy-stack-${process.pid}`,
          NATS_URLS: natsUrl,
          ...options.listenerEnv,
        },
        log: listenerLog,
      })
    );
    processes.push(listener);
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

/**
 * The client and monitoring addresses natsd reports in `log` once it has bound them and is ready,
 * each as `127.0.0.1:<port>`, or undefined before then. It logs the ports it bound, which are the
 * kernel's choice, "Starting http monitor on 127.0.0.1:<port>" and "Listening for client
 * connections on 127.0.0.1:<port>", and then "Server is ready", so a log that holds that last line
 * holds both addresses whole.
 */
function natsdAddresses(log: string): { client: string; monitor: string } | undefined {
  // startServer creates the log before natsd starts, so it is there to read.
  const text = readFileSync(log, "utf8");
  if (!text.includes("Server is ready")) return undefined;
  const client = /Listening for client connections on (\S+:\d+)/.exec(text)?.[1];
  const monitor = /Starting http monitor on (\S+:\d+)/.exec(text)?.[1];
  return client === undefined || monitor === undefined ? undefined : { client, monitor };
}
