import { rmSync, writeFileSync } from "node:fs";
import * as path from "node:path";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";

/**
 * The NATS server image the tests run; CI pulls it before the tests.
 * ghcr.io/cybermonklabs/legion/ci, the copy of nats:2.10 (.github/ci-images.yaml).
 */
const NATS_IMAGE =
  "ghcr.io/cybermonklabs/legion/ci@sha256:5498ba57b9471840be3d15b033a4eec554d1c02fa6c2cc0ca2d888637f6c6e2f";

export interface NatsServer {
  readonly url: string;
  stop(): void;
}

/** Runs `docker args…` and returns its stdout and stderr, trimmed. It throws on a non-zero exit,
 *  and kills the call once `deadline` (a `performance.now()` reading) has passed, naming what it
 *  ran and that the start's time ran out. */
async function docker(
  deadline: number,
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
    throw new Error(`docker ${args.join(" ")} had not returned when the start's time ran out`);
  }
  if (exitCode !== 0) throw new Error(`docker ${args.join(" ")} exited ${exitCode}: ${stderr}`);
  return { stdout: stdout.trim(), stderr: stderr.trim() };
}

/**
 * Reserves the directory the servers of one test file keep their configuration files in, and
 * returns its path.
 *
 * Every test file that starts a server calls this once, at its own module scope, as
 * `const serverDirectory = useNatsServerDirectory()`, and hands the path to
 * {@link startNatsServer}. One directory holds every server of the file: each writes a file
 * named after its own container, so two servers started at once do not meet.
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
 * Runs a NATS server container of its own, uniquely named, with JetStream; `nkeyUser`, when
 * given, is the public key of the only nkey user it admits. `stop` removes the container.
 *
 * `directory` is the one the caller reserved with {@link useNatsServerDirectory}; the server's
 * configuration file is written there, and `stop` removes that file.
 */
export async function startNatsServer(directory: string, nkeyUser?: string): Promise<NatsServer> {
  return startNatsServerWithConfig(
    directory,
    nkeyUser === undefined
      ? "jetstream {}\n"
      : `jetstream {}\nauthorization {\n  users = [ { nkey: ${JSON.stringify(nkeyUser)} } ]\n}\n`
  );
}

/**
 * How long one container's start may take, from `docker run` until the server logs that it is
 * ready. On the shared development machine on 2026-11-21, at a one-minute load of 1,531 to 1,620,
 * `docker run -d` of this image took 113.4 s, 81.5 s and 6.3 s, and each server was ready at the
 * first `docker logs` after it (0.12 s to 0.16 s later): the time is the Docker daemon's, not the
 * server's. Twice the slowest, rounded up to 10 s, is 230 s.
 */
export const NATS_START_MS = 230_000;

/**
 * How long one container's removal (`docker rm -f -v`) may take: 133.1 s, 26.1 s and 8.9 s after
 * those same three starts. Twice the slowest, rounded up to 10 s, is 270 s. A removal that runs
 * past it is killed, and `stop` throws naming the container.
 */
export const NATS_STOP_MS = 270_000;

/**
 * The room a hook keeps beside its docker calls' bounds, for its own steps and for a call killed
 * at its bound to return and throw, so that a call that runs out fails by its own message rather
 * than as the hook's timeout.
 */
const HOOK_ROOM_MS = 10_000;

/**
 * The bound of a hook that starts servers side by side: one start, and the removal of its
 * container when that start runs out. It is a hang guard: no row's verdict is its time.
 */
export const NATS_START_HOOK_MS = NATS_START_MS + NATS_STOP_MS + HOOK_ROOM_MS;

/** The bound of a hook that removes `servers` containers one after another. A hang guard too. */
export function natsStopHookMs(servers: number): number {
  return servers * NATS_STOP_MS + HOOK_ROOM_MS;
}

/**
 * Runs a NATS server container as {@link startNatsServer} does, whose whole configuration is
 * `config`: a row that needs users with grants of their own writes their `authorization` block.
 *
 * The whole start, `docker run` until the server logs that it is ready, is bounded by
 * {@link NATS_START_MS}; a start that runs out throws naming the container and the step it was
 * in, after removing the container. `stop` removes the container, bounded by
 * {@link NATS_STOP_MS}, and throws naming it when the removal runs out.
 */
export async function startNatsServerWithConfig(
  directory: string,
  configuration: string
): Promise<NatsServer> {
  const name = `legion-nats-test-${crypto.randomUUID()}`;
  const config = path.join(directory, `${name}.conf`);
  writeFileSync(config, configuration, { mode: 0o644 });
  const stop = () => {
    // With -v, since the image declares a VOLUME a plain `docker rm -f` leaves on the host
    // (docs/testing.md).
    const removal = Bun.spawnSync(["docker", "rm", "-f", "-v", name], {
      stdout: "ignore",
      stderr: "ignore",
      timeout: NATS_STOP_MS,
      killSignal: "SIGKILL",
    });
    rmSync(config, { force: true });
    if (removal.exitedDueToTimeout) {
      throw new Error(
        `NATS container ${name}: its docker rm had not returned within ${NATS_STOP_MS / 1000} s`
      );
    }
  };
  const deadline = performance.now() + NATS_START_MS;
  // Each step's failure names the container and the step; the error it wraps says whether the
  // step's docker call failed or the start's bound ran out.
  const bound = `the start's bound is ${NATS_START_MS / 1000} s`;
  try {
    await docker(
      deadline,
      "run",
      "-d",
      // A container that dies on its own is gone with it, so the readiness loop's `docker logs`
      // fails at once instead of spinning to its bound (docs/testing.md).
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
      throw new Error(`NATS container ${name}: its docker run failed (${bound}): ${error}`);
    });
    // nats-server logs to stderr.
    for (let logs = 1; ; logs += 1) {
      const read = await docker(deadline, "logs", name).catch((error: unknown) => {
        throw new Error(
          `NATS container ${name} was not ready after ${logs} reads of its logs (${bound}): ${error}`
        );
      });
      if (read.stderr.includes("Server is ready")) break;
      await Bun.sleep(100);
    }
    const port = (
      await docker(deadline, "port", name, "4222/tcp").catch((error: unknown) => {
        throw new Error(`NATS container ${name}: its docker port failed (${bound}): ${error}`);
      })
    ).stdout.split("\n")[0];
    return { url: `nats://${port}`, stop };
  } catch (error) {
    try {
      stop();
    } catch (removal) {
      throw new Error(`${error}; and then ${removal}`);
    }
    throw error;
  }
}
