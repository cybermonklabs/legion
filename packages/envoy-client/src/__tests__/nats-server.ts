import { rmSync, writeFileSync } from "node:fs";
import * as path from "node:path";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";

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
   * Removes the container with its volume and the configuration file, whatever `start` got to.
   * The removal is killed at `deadline`; one that exits non-zero, with what docker printed, or is
   * killed throws naming the container.
   */
  stop(deadline: number): Promise<void>;
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
 * The bound of a hook that stops a file's servers, side by side, and the removals' only bound: a
 * hang guard too. `docker rm -f -v` took 133.1 s, 26.1 s and 8.9 s after those same three starts,
 * and a removal has 270 s of it, twice the slowest.
 */
export const NATS_STOP_HOOK_MS = 280_000;

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
        "-v",
        `${config}:/etc/nats/test.conf:ro`,
        NATS_IMAGE,
        "-c",
        "/etc/nats/test.conf"
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
    },
    async stop(deadline) {
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
    },
  };
}
