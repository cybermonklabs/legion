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

export interface NatsServer {
  readonly url: string;
  /** The server's own log so far (nats-server writes it to stderr), where it names every publish
   *  it refused: `Publish Violation`, then the subject. */
  log(): Promise<string>;
  stop(): void;
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
  const publish = list(sessionGrantFixture.publish);
  const subscribe = list(sessionGrantFixture.subscribe);
  return `jetstream {}
authorization {
  users = [
    { nkey: ${JSON.stringify(requesterKey)} }
    { nkey: ${JSON.stringify(sessionKey)}, permissions: { publish: { allow: [${publish}] }, subscribe: { allow: [${subscribe}] } } }
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
  return startNatsServerWithConfig(
    directory,
    nkeyUser === undefined
      ? "jetstream {}\n"
      : `jetstream {}\nauthorization {\n  users = [ { nkey: ${JSON.stringify(nkeyUser)} } ]\n}\n`
  );
}

/**
 * Runs a NATS server container as {@link startNatsServer} does, whose whole configuration is
 * `config`: a row that needs users with grants of their own writes their `authorization` block.
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
    // `docker logs` splits the container's output into the two streams it was written to.
    const log = async () => {
      const { stdout, stderr } = await docker("logs", name);
      return `${stdout}\n${stderr}`;
    };
    return { url: `nats://${port}`, log, stop };
  } catch (error) {
    stop();
    throw error;
  }
}
