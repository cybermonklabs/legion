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
  /** Removes the container, and fails, naming them, when a client is still connected to it. */
  stop(): Promise<void>;
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
 * The names of the clients still connected to the server whose monitor is at `monitor`
 * (`host:port`), read from its /connz, once none is left or two seconds have passed: a client's
 * close is asynchronous, so one closed a moment ago can still be counted. A client with no name
 * is named by its connection id. It polls a real container's own count, which no fake clock
 * advances, as the readiness loop below polls the container's log.
 */
async function connectedClients(monitor: string): Promise<string[]> {
  const deadline = Date.now() + 2_000;
  for (;;) {
    const response = await fetch(`http://${monitor}/connz`, { signal: AbortSignal.timeout(2_000) });
    if (!response.ok) throw new Error(`http://${monitor}/connz answered ${response.status}`);
    const connz = (await response.json()) as { connections?: { cid: number; name?: string }[] };
    const open = (connz.connections ?? []).map((c) => c.name || `connection ${c.cid}`);
    if (open.length === 0 || Date.now() > deadline) return open;
    await Bun.sleep(100);
  }
}

/**
 * Runs a NATS server container of its own, uniquely named, with JetStream; `nkeyUser`, when
 * given, is the public key of the only nkey user it admits. `stop` removes the container.
 *
 * `directory` is the one the caller reserved with {@link useNatsServerDirectory}; the server's
 * configuration file is written there, and `stop` removes that file.
 *
 * The server listens on 127.0.0.1, on a port Docker takes from the kernel's ephemeral range, so a
 * client a test leaves open would keep reconnecting to that address after the container is gone,
 * and Docker can publish another test's server on the same port: the stray client then reaches
 * that server. So `stop` reads the server's own connections first (its monitor's /connz, published
 * beside it), and after removing the container fails, naming them, when any client is still
 * connected. Every client a test opens is closed before its server stops.
 */
export async function startNatsServer(directory: string, nkeyUser?: string): Promise<NatsServer> {
  const name = `legion-nats-test-${crypto.randomUUID()}`;
  const config = path.join(directory, `${name}.conf`);
  writeFileSync(
    config,
    nkeyUser === undefined
      ? "jetstream {}\nhttp: 8222\n"
      : `jetstream {}\nhttp: 8222\nauthorization {\n  users = [ { nkey: ${JSON.stringify(nkeyUser)} } ]\n}\n`,
    { mode: 0o644 }
  );
  const remove = () => {
    // With -v, since the image declares a VOLUME a plain `docker rm -f` leaves on the host
    // (docs/testing.md).
    Bun.spawnSync(["docker", "rm", "-f", "-v", name], { stdout: "ignore", stderr: "ignore" });
    rmSync(config, { force: true });
  };
  try {
    await docker(
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
      "-p",
      "127.0.0.1::4222",
      "-p",
      "127.0.0.1::8222",
      "-v",
      `${config}:/etc/nats/test.conf:ro`,
      NATS_IMAGE,
      "-c",
      "/etc/nats/test.conf"
    );
    const deadline = Date.now() + 90_000;
    // nats-server logs to stderr.
    while (!(await docker("logs", name)).stderr.includes("Server is ready")) {
      if (Date.now() > deadline) throw new Error(`NATS container ${name} never became ready`);
      await Bun.sleep(100);
    }
    const port = (await docker("port", name, "4222/tcp")).stdout.split("\n")[0];
    const monitor = (await docker("port", name, "8222/tcp")).stdout.split("\n")[0];
    const url = `nats://${port}`;
    const stop = async () => {
      let open: string[];
      try {
        open = await connectedClients(monitor);
      } finally {
        remove();
      }
      if (open.length > 0) {
        throw new Error(
          `the NATS server at ${url} still had ${open.length} client(s) connected when its test stopped it (${open.join(", ")}): ` +
            "a client left open keeps reconnecting to that address after the container is gone, and Docker can publish " +
            "another test's server on the same port; close every client before stopping its server"
        );
      }
    };
    return { url, stop };
  } catch (error) {
    remove();
    throw error;
  }
}
