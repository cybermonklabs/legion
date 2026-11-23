import { rmSync, writeFileSync } from "node:fs";
import * as path from "node:path";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import sessionGrantFixture from "../../contracts/fixtures/session-grant.json";

/**
 * The NATS server image the tests run; CI pulls it before the tests.
 * ghcr.io/cybermonklabs/legion/ci, the copy of nats:2.10 (.github/ci-images.yaml).
 */
const NATS_IMAGE =
  "ghcr.io/cybermonklabs/legion/ci@sha256:5498ba57b9471840be3d15b033a4eec554d1c02fa6c2cc0ca2d888637f6c6e2f";

/**
 * A NATS server container of its own, uniquely named, with JetStream. A test file makes it at
 * the scope that uses it, starts it in a `beforeAll` and stops it in an `afterAll`, so a start
 * that fails leaves whatever it got to for the `afterAll` to remove, rather than spending its
 * own hook's time on the removal. Each docker call either one makes is bounded by the deadline
 * its hook hands it ({@link natsHookDeadline}): no fixed span of this module decides how long the
 * Docker daemon may take.
 */
export interface NatsServer {
  /** The container's name, which every error of `start` and `stop` names. */
  readonly name: string;
  /** The server's client URL, `nats://127.0.0.1:<port>`. It throws until `start` has returned. */
  readonly url: string;
  /**
   * Writes the server's configuration file, runs the container, waits until the server logs that
   * it is ready, and reads its port. Each docker call is killed at `deadline` (a
   * `performance.now()` reading); a start that fails throws naming the container and the step it
   * was in, with the error it wraps, which says whether docker failed or the time ran out.
   */
  start(deadline: number): Promise<void>;
  /**
   * Reads the server's own clients (its monitor's /connz) until none is left, then removes the
   * container with its volume and the configuration file, whatever `start` got to. The read ends
   * {@link NATS_REMOVAL_MS} before `deadline`, so the removal keeps its own bound, and the removal
   * is killed at `deadline`. A removal that exits non-zero, with what docker printed, or is killed
   * throws naming the container. After the removal, a stop whose server still had a client when
   * the read ended throws naming each one: the server listens on 127.0.0.1, on a port Docker takes
   * from the kernel's ephemeral range, so a client a test left open would keep reconnecting to that
   * address after the container is gone, and Docker can publish another test's server on the same
   * port, which the stray client then reaches. Every client a test opens is closed before its
   * server stops.
   *
   * `leftOpen` names clients the caller left open on purpose, as the leak check's control does: the
   * read then ends the moment the server's clients are exactly those, since a client left open never
   * leaves, and the stop still throws naming them. With none named, the read ends when none is left.
   */
  stop(deadline: number, leftOpen?: readonly string[]): Promise<void>;
  /**
   * The server's own log so far (nats-server writes it to stderr), where it names every publish
   * it refused: `Publish Violation`, then the subject. The `docker logs` call is killed at
   * `deadline` (a `performance.now()` reading), and one that fails throws naming the container.
   */
  log(deadline: number): Promise<string>;
}

/**
 * The authorization of a server whose one requester may do anything, as the listener and Dispatch
 * may, and whose session user holds the grant the NATS auth service gives one session. That grant
 * is `natsgrant.Session` itself: packages/envoy/natsgrant's TestTheSessionGrantFixtureIsNatsgrantSession
 * writes it to packages/contracts/fixtures/session-grant.json for a placeholder id and fails when
 * the two drift, and this reads the fixture with the placeholder swapped, token by token, for
 * `sessionID`. Each user is the nkey whose public key is named. A row's requester asks the
 * session on a reply subject {@link sessionReplySubject} makes, and the session answers on
 * whatever reply subject it is handed.
 */
export function sessionGrantConfig(options: {
  readonly sessionID: string;
  readonly sessionKey: string;
  readonly requesterKey: string;
}): string {
  const { sessionID, sessionKey, requesterKey } = options;
  const placeholder = sessionGrantFixture.session_id;
  const list = (subjects: readonly string[]) =>
    subjects
      .map((subject) =>
        JSON.stringify(
          subject
            .split(".")
            .map((token) => (token === placeholder ? sessionID : token))
            .join(".")
        )
      )
      .join(", ");
  // The fixture writes a deny list only where natsgrant.Session sets one.
  const fixture: {
    readonly publish: readonly string[];
    readonly subscribe: readonly string[];
    readonly publish_deny?: readonly string[];
    readonly subscribe_deny?: readonly string[];
  } = sessionGrantFixture;
  const rules = (allow: readonly string[], deny: readonly string[] | undefined) =>
    deny === undefined || deny.length === 0
      ? `{ allow: [${list(allow)}] }`
      : `{ allow: [${list(allow)}], deny: [${list(deny)}] }`;
  const publish = rules(fixture.publish, fixture.publish_deny);
  const subscribe = rules(fixture.subscribe, fixture.subscribe_deny);
  return `jetstream {}
authorization {
  users = [
    { nkey: ${JSON.stringify(requesterKey)} }
    { nkey: ${JSON.stringify(sessionKey)}, permissions: { publish: ${publish}, subscribe: ${subscribe} } }
  ]
}
`;
}

/** The 32 letters and digits of Go's crypto/rand.Text, which the listener's and Dispatch's
 *  reply tokens are written in. */
const TOKEN_ALPHABET = "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567";

/**
 * A reply subject a listener or Dispatch asks session `sessionID` on: `inbox`, then the session's
 * id, then 26 random characters, as `bus.SessionReplySubject` makes it.
 */
export function sessionReplySubject(inbox: string, sessionID: string): string {
  const token = Array.from(
    crypto.getRandomValues(new Uint8Array(26)),
    (byte) => TOKEN_ALPHABET[byte % TOKEN_ALPHABET.length]
  ).join("");
  return `${inbox}.${sessionID}.${token}`;
}

/** Runs `docker args…` and returns its stdout and stderr, trimmed. It throws on a non-zero exit,
 *  naming the exit and what docker printed on stderr, and kills the call once `deadline` (a
 *  `performance.now()` reading) has passed, naming what it ran and that `whose` time ran out. */
async function docker(
  deadline: number,
  whose: string,
  ...args: string[]
): Promise<{ stdout: string; stderr: string }> {
  const run = Bun.spawn(["docker", ...args], {
    stdout: "pipe",
    stderr: "pipe",
    timeout: Math.max(1, Math.ceil(deadline - performance.now())),
    killSignal: "SIGKILL",
  });
  const [stdout, stderr, exitCode] = await Promise.all([
    new Response(run.stdout).text(),
    new Response(run.stderr).text(),
    run.exited,
  ]);
  if (run.signalCode !== null) {
    throw new Error(`docker ${args.join(" ")} had not returned when ${whose} time ran out`);
  }
  if (exitCode !== 0) {
    throw new Error(`docker ${args.join(" ")} exited ${exitCode}: ${stderr.trim()}`);
  }
  return { stdout: stdout.trim(), stderr: stderr.trim() };
}

/** What a failed step's error says, for the message that wraps it. */
function reason(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}

/**
 * Reserves the directory the servers of one test file keep their configuration files in, and
 * returns its path.
 *
 * Every test file that starts a server calls this once, at its own module scope, as
 * `const serverDirectory = useNatsServerDirectory()`, and hands the path to
 * {@link natsServer}. One directory holds every server of the file: each writes a file named
 * after its own container, so two servers started at once do not meet.
 *
 * The call cannot live in this module's body: `temporaryDirectory` registers the `beforeAll`
 * that makes the directory and the `afterAll` that removes it where it is called, and Bun
 * evaluates an imported module once per process, so a hook here would take the scope of
 * whichever test file imported this one first. Module scope rather than the `describe` body
 * that starts the servers, because Bun runs a scope's `afterAll` hooks in the order they were
 * registered, and the configuration a container reads is mounted into it until `stop`.
 */
export function useNatsServerDirectory(): string {
  return temporaryDirectory("nats-server");
}

/**
 * The room a hook keeps after its docker calls' deadline, for its own steps and for a call killed
 * at the deadline to return and throw, so that a call that runs out fails by its own message
 * rather than as the hook's timeout.
 */
const HOOK_ROOM_MS = 10_000;

/**
 * The deadline, a `performance.now()` reading, of the docker calls of a hook whose bound is
 * `hookMs`, read as the hook starts: the hook's own bound, less its room. The calls take all the
 * rest of it, so a slow call spends the hook's bound and one that runs out fails naming itself.
 */
export function natsHookDeadline(hookMs: number): number {
  return performance.now() + hookMs - HOOK_ROOM_MS;
}

/**
 * The bound of a hook that starts a file's servers, side by side, and the starts' only bound. It
 * is a hang guard, which no correct start comes near: on the shared development machine on
 * 2026-11-21, at a one-minute load of 1,531 to 1,620, `docker run -d` of this image took 113.4 s,
 * 81.5 s and 6.3 s, and each server was ready at the first `docker logs` after it (0.12 s to
 * 0.16 s later), so the time is the Docker daemon's, not the server's. A start has 500 s of it,
 * 4.4 times the slowest.
 */
export const NATS_START_HOOK_MS = 510_000;

/**
 * The bound of a removal: a hang guard. `docker rm -f -v` took 133.1 s, 26.1 s and 8.9 s after
 * those same three starts, and a removal has 270 s, twice the slowest.
 */
const NATS_REMOVAL_MS = 270_000;

/**
 * The bound of a stop's read of the server's clients, a hang guard that no correct close comes
 * near. A client a test closed leaves the server's /connz as soon as the server reads its close;
 * one left open never does, so the read runs to this bound and the stop fails naming it. On the
 * shared development machine on 2026-11-22, at a one-minute load of 1,091 to 1,114, 600 clients
 * of this image's server, each closed as a test closes it, left its /connz 0.3 ms after the close
 * returned at the median and 10.8 ms at the slowest; the read has 30 s, over 2,700 times that.
 */
const NATS_CLIENTS_READ_MS = 30_000;

/**
 * The bound of a hook that stops a file's servers, side by side, and the stops' only bound: the
 * read of each server's clients, then its removal, then the hook's room.
 */
export const NATS_STOP_HOOK_MS = NATS_CLIENTS_READ_MS + NATS_REMOVAL_MS + HOOK_ROOM_MS;

/**
 * The names of the clients still connected to the server whose monitor is at `monitor`
 * (`host:port`), read from its /connz once they are exactly `leftOpen` (none, unless a caller left
 * clients open on purpose) or once `window` aborts. A client's close is asynchronous, so one closed
 * a moment ago can still be counted: the read polls the server's own count, which no fake clock
 * advances, until it is what the caller left or the window ends. A client with no name is named by
 * its connection id. `stop` passes a window that ends {@link NATS_REMOVAL_MS} before its deadline.
 *
 * Each poll's request is cut when `window` aborts. A poll the window cuts after an earlier poll
 * answered gives that earlier answer, so a client left open is named rather than lost to "could not
 * be read"; a first poll that fails throws. It is exported for the row that cuts a poll on purpose
 * (nats-server.test.ts).
 */
export async function connectedClients(
  monitor: string,
  window: AbortSignal,
  leftOpen: readonly string[]
): Promise<string[]> {
  const expected = [...leftOpen].sort().join("\n");
  let last: string[] | undefined;
  for (;;) {
    let open: string[];
    try {
      const response = await fetch(`http://${monitor}/connz`, { signal: window });
      if (!response.ok) throw new Error(`http://${monitor}/connz answered ${response.status}`);
      const connz = (await response.json()) as { connections?: { cid: number; name?: string }[] };
      open = (connz.connections ?? []).map((c) => c.name || `connection ${c.cid}`);
    } catch (error) {
      if (last !== undefined && window.aborted) return last;
      throw error;
    }
    if ([...open].sort().join("\n") === expected || window.aborted) return open;
    last = open;
    await Bun.sleep(100);
  }
}

/**
 * A NATS server container, not started, whose only user is the nkey user `nkeyUser` when given;
 * otherwise it asks for no credential.
 *
 * `directory` is the one the caller reserved with {@link useNatsServerDirectory}; `start` writes
 * the server's configuration file there, and `stop` removes that file.
 */
export function natsServer(directory: string, nkeyUser?: string): NatsServer {
  return natsServerWithConfig(
    directory,
    nkeyUser === undefined
      ? "jetstream {}\n"
      : `jetstream {}\nauthorization {\n  users = [ { nkey: ${JSON.stringify(nkeyUser)} } ]\n}\n`
  );
}

/**
 * A NATS server container as {@link natsServer} makes, whose whole configuration is
 * `configuration`: a row that needs users with grants of their own writes their `authorization`
 * block.
 */
export function natsServerWithConfig(directory: string, configuration: string): NatsServer {
  const name = `legion-nats-test-${crypto.randomUUID()}`;
  const config = path.join(directory, `${name}.conf`);
  let url: string | undefined;
  // The server's monitor, `host:port`, which `stop` reads its clients from; set once `start` read it.
  let monitor: string | undefined;
  return {
    name,
    get url() {
      if (url === undefined) throw new Error(`NATS container ${name} has not started`);
      return url;
    },
    async start(deadline) {
      writeFileSync(config, configuration, { mode: 0o644 });
      // Each step's failure names the container and the step; the error it wraps says whether
      // the step's docker call failed or the start's time ran out.
      await docker(
        deadline,
        "the start's",
        "run",
        "-d",
        // A container that dies on its own is gone with it, so the readiness loop's `docker logs`
        // fails at once instead of spinning to its deadline (docs/testing.md).
        "--rm",
        "--name",
        name,
        // Capped, as every container the repository's tests start is on a shared host: a test
        // server holds a few small streams.
        "--cpus",
        "1",
        "--memory",
        "512m",
        "-p",
        "127.0.0.1::4222",
        "-p",
        "127.0.0.1::8222",
        "-v",
        `${config}:/etc/nats/test.conf:ro`,
        NATS_IMAGE,
        "-c",
        "/etc/nats/test.conf",
        // The monitoring endpoint on 8222, whatever the configuration says, for `stop`'s read.
        "-m",
        "8222"
      ).catch((error: unknown) => {
        throw new Error(`NATS container ${name}: its docker run failed: ${reason(error)}`);
      });
      // nats-server logs to stderr.
      for (let logs = 1; ; logs += 1) {
        const read = await docker(deadline, "the start's", "logs", name).catch((error: unknown) => {
          throw new Error(
            `NATS container ${name} was not ready after ${logs} reads of its logs: ${reason(error)}`
          );
        });
        if (read.stderr.includes("Server is ready")) break;
        await Bun.sleep(100);
      }
      const port = (
        await docker(deadline, "the start's", "port", name, "4222/tcp").catch((error: unknown) => {
          throw new Error(`NATS container ${name}: its docker port failed: ${reason(error)}`);
        })
      ).stdout.split("\n")[0];
      url = `nats://${port}`;
      monitor = (
        await docker(deadline, "the start's", "port", name, "8222/tcp").catch((error: unknown) => {
          throw new Error(
            `NATS container ${name}: its docker port of the monitor failed: ${reason(error)}`
          );
        })
      ).stdout.split("\n")[0];
    },
    async stop(deadline, leftOpen = []) {
      // The read ends before the removal's own bound, and the removal runs whatever the read gave.
      const clients =
        monitor === undefined
          ? []
          : await connectedClients(
              monitor,
              AbortSignal.timeout(
                Math.max(1, Math.ceil(deadline - NATS_REMOVAL_MS - performance.now()))
              ),
              leftOpen
            ).catch(
              (error: unknown) => new Error(`its clients could not be read: ${reason(error)}`)
            );
      try {
        // With -v, since the image declares a VOLUME a plain `docker rm -f` leaves on the host
        // (docs/testing.md). With -f, a container that is not there, one a start never made or
        // `--rm` already removed, is no error: docker exits 0 for it.
        await docker(deadline, "the removal's", "rm", "-f", "-v", name);
      } catch (error) {
        throw new Error(`NATS container ${name}: its docker rm failed: ${reason(error)}`);
      } finally {
        rmSync(config, { force: true });
      }
      if (clients instanceof Error) throw new Error(`NATS container ${name}: ${clients.message}`);
      if (clients.length > 0) {
        throw new Error(
          `the NATS server at ${url} still had ${clients.length} client(s) connected when its test stopped it (${clients.join(", ")}): ` +
            "a client left open keeps reconnecting to that address after the container is gone, and Docker can publish " +
            "another test's server on the same port; close every client before stopping its server"
        );
      }
    },
    async log(deadline) {
      // `docker logs` splits the container's output into the two streams it was written to.
      const { stdout, stderr } = await docker(deadline, "the log read's", "logs", name).catch(
        (error: unknown) => {
          throw new Error(`NATS container ${name}: its docker logs failed: ${reason(error)}`);
        }
      );
      return `${stdout}\n${stderr}`;
    },
  };
}
