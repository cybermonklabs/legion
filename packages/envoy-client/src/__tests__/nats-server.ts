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

/** Runs `docker args…` and returns its stdout and stderr, trimmed, throwing on a non-zero exit. */
async function docker(...args: string[]): Promise<{ stdout: string; stderr: string }> {
  const run = Bun.spawn(["docker", ...args], { stdout: "pipe", stderr: "pipe" });
  const [stdout, stderr, exitCode] = await Promise.all([
    new Response(run.stdout).text(),
    new Response(run.stderr).text(),
    run.exited,
  ]);
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
  const name = `legion-nats-test-${crypto.randomUUID()}`;
  const config = path.join(directory, `${name}.conf`);
  writeFileSync(
    config,
    nkeyUser === undefined
      ? "jetstream {}\n"
      : `jetstream {}\nauthorization {\n  users = [ { nkey: ${JSON.stringify(nkeyUser)} } ]\n}\n`,
    { mode: 0o644 }
  );
  // The id `docker run` printed, by which `stop` removes the container: never by its name.
  let id = "";
  const stop = () => {
    // With -v, since the image declares a VOLUME a plain `docker rm -f` leaves on the host
    // (docs/testing.md).
    if (id !== "") {
      Bun.spawnSync(["docker", "rm", "-f", "-v", id], { stdout: "ignore", stderr: "ignore" });
    }
    rmSync(config, { force: true });
  };
  try {
    const run = await docker(
      "run",
      "-d",
      // A container that dies on its own is gone with it, so the readiness loop's `docker logs`
      // fails at once instead of spinning to its timeout (docs/testing.md).
      "--rm",
      "--name",
      name,
      // Capped, as every container the repository's tests start is on a shared host: a test
      // server holds a few small streams.
      "--cpus",
      "1",
      "--memory",
      "512m",
      // No published port: Docker's allocator, not the kernel, would choose it, and it fails the
      // run when another program holds the ports after its last choice. On the host's network
      // the server binds a loopback port the kernel chooses (-p -1) and logs it, as a server
      // that takes no socket must.
      "--network",
      "host",
      "-v",
      `${config}:/etc/nats/test.conf:ro`,
      NATS_IMAGE,
      "-c",
      "/etc/nats/test.conf",
      "-a",
      "127.0.0.1",
      "-p",
      "-1"
    );
    id = run.stdout.trim();
    const deadline = Date.now() + 90_000;
    // nats-server logs to stderr, the client line before the ready line.
    let log = (await docker("logs", name)).stderr;
    while (!log.includes("Server is ready")) {
      if (Date.now() > deadline) throw new Error(`NATS container ${name} never became ready`);
      await Bun.sleep(100);
      log = (await docker("logs", name)).stderr;
    }
    const port = /Listening for client connections on 127\.0\.0\.1:(\d+)/.exec(log)?.[1];
    if (port === undefined) throw new Error(`NATS container ${name} logged no client port: ${log}`);
    return { url: `nats://127.0.0.1:${port}`, stop };
  } catch (error) {
    stop();
    throw error;
  }
}
