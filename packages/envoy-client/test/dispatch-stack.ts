import { closeSync, mkdirSync, openSync } from "node:fs";
import * as path from "node:path";
import { bunCacheOff } from "@legion/test-fixtures/bun-child-environment";
import { ownDatabase } from "@legion/test-fixtures/postgres";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import type { Subprocess } from "bun";
import { awaitReady, buildGoBinaries, ENVOY_MODULE, freePorts, stopProcess } from "./envoy-stack";

// Destructured: claude-envoy's typecheck, which compiles this package's test files, forbids
// `process.env.X`.
const { LEGION_E2E_DISPATCH_STACK, PATH: SHELL_PATH = "" } = process.env;

/**
 * Whether the tests that run a real Dispatch server run: they build `./cmd/dispatch` with Go and
 * write to the Postgres `DISPATCH_TEST_DATABASE_URL` names, neither of which a Bun-only job has,
 * so they run only where `LEGION_E2E_DISPATCH_STACK=1` says both are there. With it set, a
 * missing `go`, a missing DSN, a failed build and a server that never answers each fail the test
 * instead of skipping it.
 */
export const dispatchStackEnabled = LEGION_E2E_DISPATCH_STACK === "1";

/** A real Dispatch server, built from this checkout, on a database of its own. */
export interface DispatchStack {
  /** The server's base URL, `http://127.0.0.1:<port>`, as `DISPATCH_URL` takes it. */
  readonly url: string;
  /** The bearer token the server accepts for an agent, as `DISPATCH_TOKEN` takes it. */
  readonly token: string;
  /** Stops the server and drops its database. Its scratch directory is the caller's, and goes
   *  with it. */
  stop(): Promise<void>;
}

/**
 * Reserves the directory one stack works in — its Go binary, the server's home and its log — and
 * returns its path.
 *
 * Every test file that starts a stack calls this once per stack, at its own module scope, as
 * `const stackDirectory = useDispatchStackDirectory()`, and hands the path to `startDispatchStack`.
 * The call cannot live in this module's body, and belongs at module scope rather than in the
 * `describe` body that starts the stack, for the reasons `useEnvoyStackDirectory` states.
 */
export function useDispatchStackDirectory(): string {
  return temporaryDirectory("dispatch-stack");
}

/** What a caller may change about the stack it starts. */
export interface DispatchStackOptions {
  /**
   * The directory the stack works in, which the caller reserved with
   * {@link useDispatchStackDirectory} and the fixture removes again. It exists and is empty when
   * `startDispatchStack` is called, and one stack has it to itself.
   */
  readonly work: string;
}

const WEB_DIST = path.resolve(import.meta.dir, "../../dispatch/web/dist");

/**
 * Starts a real Dispatch server, built from this checkout, on a loopback port and a database of
 * its own: a copy of template1 that `@legion/test-fixtures/postgres` makes on the server
 * `DISPATCH_TEST_DATABASE_URL` names, which the server migrates itself as it boots. It resolves
 * once `GET /api/v1` answers 200, and throws, naming why, when `go` or the DSN is missing, the
 * build fails, or the server exits before it answers.
 *
 * The server runs with NATS off and with header identity on (`X-Dispatch-User`), as
 * `startScratchDispatchOn` runs it for the daemon's own tests: no event leaves the process, and a
 * person's write is the header alone. Nothing of the caller's Dispatch, Envoy or NATS environment
 * reaches it, since the child is given the environment below and nothing else.
 *
 * The stack works in `options.work`, which the caller reserved with
 * {@link useDispatchStackDirectory}: `stop` leaves it, and the fixture removes it when the file
 * that reserved it is done, whether or not the stack ever reached `stop`.
 */
export async function startDispatchStack(options: DispatchStackOptions): Promise<DispatchStack> {
  const work = options.work;
  // The server applies its migrations into this database at boot.
  const database = await ownDatabase("dispatch_stack", "DISPATCH_TEST_DATABASE_URL");
  let child: Subprocess | undefined;
  const stop = async (): Promise<void> => {
    if (child !== undefined) {
      const stopping = child;
      child = undefined;
      // SIGINT is the stop the server takes: it closes its listener and its pool on it.
      await stopProcess(stopping, "SIGINT");
    }
    // Dropped with force: the server's pool is gone with the process above, and a connection the
    // run left behind must not keep the database on the shared test server.
    await database.drop();
  };
  try {
    const bin = path.join(work, "bin");
    await buildGoBinaries({
      bin,
      packages: ["./cmd/dispatch"],
      stack: "Dispatch",
      binaries: "./cmd/dispatch",
    });
    const [port] = await freePorts(1);
    const url = `http://127.0.0.1:${port}`;
    const token = `dispatch-stack-${crypto.randomUUID()}`;
    const home = path.join(work, "home");
    mkdirSync(home);
    const log = path.join(work, "dispatch.log");
    const out = openSync(log, "a");
    child = Bun.spawn([path.join(bin, "dispatch")], {
      cwd: ENVOY_MODULE,
      env: {
        ...bunCacheOff,
        PATH: SHELL_PATH,
        HOME: home,
        DATABASE_URL: database.url,
        DISPATCH_AGENT_TOKEN: token,
        DISPATCH_IDENTITY: "header:X-Dispatch-User",
        DISPATCH_IDENTITY_HEADER_TRUSTED: "1",
        DISPATCH_NATS_DISABLED: "1",
        DISPATCH_LISTEN_HOST: "127.0.0.1",
        DISPATCH_PORT: String(port),
        DISPATCH_SERVER_URL: url,
        DISPATCH_WEB_DIST: WEB_DIST,
      },
      stdout: out,
      stderr: out,
    });
    closeSync(out);
    await awaitReady("Dispatch", child, log, async () => {
      const response = await fetch(`${url}/api/v1`);
      return response.ok;
    });
    return { url, token, stop };
  } catch (error) {
    await stop();
    throw error;
  }
}
