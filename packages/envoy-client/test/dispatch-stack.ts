import { closeSync, mkdirSync, openSync } from "node:fs";
import * as path from "node:path";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import { SQL, type Subprocess } from "bun";
import {
  awaitReady,
  buildGoBinaries,
  ENVOY_MODULE,
  freePorts,
  type ReadyBudget,
  stopProcess,
} from "./envoy-stack";

// Destructured: claude-envoy's typecheck, which compiles this package's test files, forbids
// `process.env.X`.
const {
  LEGION_E2E_DISPATCH_STACK,
  DISPATCH_TEST_DATABASE_URL,
  PATH: SHELL_PATH = "",
} = process.env;

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
  /** The stack's own database, for a row that sets up a state no route reaches: a delivery
   *  attempt bound to a session, which the server writes only once a listener resolved it. */
  readonly databaseUrl: string;
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
  /**
   * Settings of the server's own beyond the stack's, such as `DISPATCH_WRITING_CHECK=on`. They
   * are laid over the stack's environment, so a name the stack sets is the caller's to change.
   */
  readonly env?: Readonly<Record<string, string>>;
}

const WEB_DIST = path.resolve(import.meta.dir, "../../dispatch/web/dist");
// The server migrates a database of its own as it boots, and a cold `go build` of ./cmd/dispatch
// comes first, so it is given twice the Envoy stack's budget; 20 lines of its log say why a boot
// that never answered stopped.
const DISPATCH_READY: ReadyBudget = { timeoutMs: 120_000, tailLines: 20 };

/** The test server's DSN, or the error that says the run has none. */
function testDatabaseServer(): string {
  if (DISPATCH_TEST_DATABASE_URL === undefined || DISPATCH_TEST_DATABASE_URL === "") {
    throw new Error(
      "the Dispatch stack needs DISPATCH_TEST_DATABASE_URL, the Postgres it makes its own database on"
    );
  }
  return DISPATCH_TEST_DATABASE_URL;
}

/**
 * Starts a real Dispatch server, built from this checkout, on a loopback port and a database of
 * its own: a copy of template1 on the server `DISPATCH_TEST_DATABASE_URL` names, which the server
 * migrates itself as it boots. It resolves once `GET /api/v1` answers 200, and throws, naming why,
 * when `go` or the DSN is missing, the build fails, or the server does not come up.
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
  const postgres = testDatabaseServer();
  const work = options.work;
  const database = `dispatch_stack_${crypto.randomUUID().replaceAll("-", "")}`;
  const databaseUrl = new URL(postgres);
  databaseUrl.pathname = `/${database}`;
  const admin = new SQL(postgres);
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
    await admin.unsafe(`drop database if exists ${database} with (force)`);
    await admin.close();
  };
  try {
    // A copy of template1's files, as the Go tests' own fixture makes a scratch database
    // (testdb.create): the server applies its migrations into it at boot.
    await admin.unsafe(`create database ${database} strategy file_copy`);
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
        PATH: SHELL_PATH,
        HOME: home,
        DATABASE_URL: databaseUrl.toString(),
        DISPATCH_AGENT_TOKEN: token,
        DISPATCH_IDENTITY: "header:X-Dispatch-User",
        DISPATCH_IDENTITY_HEADER_TRUSTED: "1",
        DISPATCH_NATS_DISABLED: "1",
        DISPATCH_LISTEN_HOST: "127.0.0.1",
        DISPATCH_PORT: String(port),
        DISPATCH_SERVER_URL: url,
        DISPATCH_WEB_DIST: WEB_DIST,
        ...options.env,
      },
      stdout: out,
      stderr: out,
    });
    closeSync(out);
    await awaitReady("Dispatch", child, log, DISPATCH_READY, async () => {
      const response = await fetch(`${url}/api/v1`, { signal: AbortSignal.timeout(2_000) });
      return response.ok;
    });
    return { url, token, databaseUrl: databaseUrl.toString(), stop };
  } catch (error) {
    await stop();
    throw error;
  }
}
