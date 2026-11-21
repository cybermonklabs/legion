import { afterAll, beforeAll, describe, expect, test } from "bun:test";
import { randomUUID } from "node:crypto";
import { mkdirSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { skipWithout } from "@legion/test-fixtures/host-tools";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import { connect, type NatsConnection } from "nats";
import { readLast } from "./nats-stream";

// The NATS server these rows read is a container this file starts, so a host with no docker runs
// none of them.
const skip = skipWithout("docker");
const root = join(import.meta.dir, "..", "..", "..");
const container = `legion-nats-stream-${randomUUID()}`;
const stream = `NATS_STREAM_${randomUUID().replaceAll("-", "_")}`;
const configDirectory = temporaryDirectory("nats-stream-test");
const config = join(configDirectory, "envoy.json");
const subjectCount = 10_000;
const publishBatchSize = 25;
const publishTimeoutMilliseconds = 30_000;
const budgetSeconds = 5;
const subsecondBudgetSeconds = 0.1;
let serverUrl = "";
// The server's monitor, `host:port`, which the leak check reads (connectedClients).
let monitor = "";
let expectedSequence = 0;
let nc: NatsConnection | undefined;
// The container's removal, set the moment `docker run` returns and run by cleanup; `-v` goes with
// it, since a plain `docker rm -f` leaves the image's declared volume on the host (docs/testing.md).
let removeContainer: (() => void) | undefined;

function docker(...args: string[]): string {
  const result = Bun.spawnSync(["docker", ...args], { cwd: root });
  const stdout = result.stdout.toString();
  const stderr = result.stderr.toString();
  if (result.exitCode !== 0) throw new Error(`docker ${args.join(" ")}: ${stdout}${stderr}`);
  return stdout;
}

async function startNats(): Promise<void> {
  docker(
    "run",
    "--detach",
    // --rm, so a container this run stops is reclaimed by docker itself rather than by a later
    // teardown that a killed test never reaches.
    "--rm",
    "--name",
    container,
    "--cpus",
    "0.5",
    "--memory",
    "512m",
    "-p",
    "127.0.0.1::4222",
    "-p",
    "127.0.0.1::8222",
    // ghcr.io/cybermonklabs/legion/ci, the copy of nats:2.10 (.github/ci-images.yaml)
    "ghcr.io/cybermonklabs/legion/ci@sha256:5498ba57b9471840be3d15b033a4eec554d1c02fa6c2cc0ca2d888637f6c6e2f",
    "-js",
    "-m",
    "8222"
  );
  removeContainer = () => {
    Bun.spawnSync(["docker", "rm", "-f", "-v", container], { cwd: root });
  };
  try {
    const endpoint = docker("port", container, "4222/tcp").trim();
    serverUrl = `nats://${endpoint}`;
    monitor = docker("port", container, "8222/tcp").trim();

    nc = await connect({
      servers: serverUrl,
      timeout: 10_000,
      waitOnFirstConnect: true,
      reconnectTimeWait: 50,
      maxReconnectAttempts: -1,
    });
  } catch (error) {
    removeContainer();
    removeContainer = undefined;
    throw error;
  }
}

function runLast(subject: string, seconds = budgetSeconds) {
  // PATH, a home of the test's own and the config this run wrote: no NATS_NKEY_SEED or
  // NATS_NKEY_SEED_FILE of the caller's reaches the child, which this server would refuse.
  const env = {
    PATH: process.env.PATH ?? "",
    HOME: configDirectory,
    ENVOY_CONFIG_FILE: config,
  };
  const started = performance.now();
  const result = Bun.spawnSync(
    [
      "bun",
      join(import.meta.dir, "nats-stream.ts"),
      "last",
      serverUrl,
      stream,
      subject,
      String(seconds),
    ],
    { cwd: root, env }
  );
  return {
    elapsedMilliseconds: performance.now() - started,
    exitCode: result.exitCode,
    stderr: result.stderr.toString(),
    stdout: result.stdout.toString(),
  };
}

async function runReadLast(subject: string, seconds: number) {
  const client = await connect({ servers: serverUrl, timeout: 10_000, maxReconnectAttempts: 0 });
  const started = performance.now();
  try {
    const result = await readLast(client, stream, subject, seconds);
    return { elapsedMilliseconds: performance.now() - started, result };
  } finally {
    void client.close();
  }
}

function expectWithinBudget(elapsedMilliseconds: number, seconds: number) {
  expect(elapsedMilliseconds).toBeLessThanOrEqual(seconds * 1_000 + 500);
}

/**
 * The names of the clients still connected to this file's server, read from its monitor's /connz,
 * once none is left or two seconds have passed: a client's close is asynchronous, so one closed a
 * moment ago can still be counted. It polls a real container's own count, which no fake clock
 * advances. packages/envoy-client/src/__tests__/nats-server.ts holds the same check for that
 * package's servers.
 */
async function connectedClients(): Promise<string[]> {
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

// The server listens on 127.0.0.1, on a port Docker takes from the kernel's ephemeral range, so a
// client this file left open would keep reconnecting to that address after the container is gone,
// and Docker can publish another test's server on the same port. So cleanup closes this file's own
// client, then fails, naming them, when any client is still connected, after removing the container.
async function cleanup(): Promise<void> {
  let open: string[] = [];
  try {
    await nc?.close();
    if (monitor) open = await connectedClients();
  } finally {
    removeContainer?.();
    removeContainer = undefined;
  }
  if (open.length > 0) {
    throw new Error(
      `the NATS server at ${serverUrl} still had ${open.length} client(s) connected when this file stopped it (${open.join(", ")}): ` +
        "a client left open keeps reconnecting to that address after the container is gone; close every client first"
    );
  }
}

beforeAll(async () => {
  if (skip) return;
  try {
    await startNats();
    mkdirSync(configDirectory, { recursive: true });
    writeFileSync(config, JSON.stringify({ envoy: { enabled: true, natsUrls: [serverUrl] } }));

    if (!nc) throw new Error("NATS connection was not established");
    const manager = await nc.jetstreamManager();
    await manager.streams.add({ name: stream, subjects: ["events.>"] });
    const js = nc.jetstream();
    for (let start = 0; start < subjectCount; start += publishBatchSize) {
      const end = Math.min(start + publishBatchSize, subjectCount);
      await Promise.all(
        Array.from({ length: end - start }, (_, index) =>
          js.publish(`events.subject.${start + index}`, new Uint8Array(), {
            timeout: publishTimeoutMilliseconds,
          })
        )
      );
    }
    expectedSequence = (
      await js.publish("events.newest", new Uint8Array(), { timeout: publishTimeoutMilliseconds })
    ).seq;
  } catch (error) {
    await cleanup();
    throw error;
  }
}, 120_000);

afterAll(async () => {
  // With no docker nothing was started, and the fixture's own hook removes the scratch directory
  // this file's module scope made.
  if (skip) return;
  await cleanup();
}, 120_000);

describe.skipIf(skip)("nats-stream last", () => {
  test("reads the newest matching message without draining every subject", () => {
    const result = runLast("events.>");
    if (result.exitCode !== 0) {
      throw new Error(
        `last exited ${result.exitCode} after ${Math.round(result.elapsedMilliseconds)}ms: ${result.stderr}`
      );
    }
    expect(result.stderr).toBe("");
    expect(JSON.parse(result.stdout)).toEqual({
      subject: "events.newest",
      time: expect.any(String),
      seq: expectedSequence,
    });
    console.info(
      `nats-stream last ${subjectCount + 1} subjects: ${Math.round(result.elapsedMilliseconds)}ms`
    );
  }, 120_000);

  test("reports the existing no-message error for an unmatched filter", () => {
    const result = runLast("events.missing.>", subsecondBudgetSeconds);
    expect(result.exitCode).toBe(1);
    expect(result.stdout).toBe("");
    expect(result.stderr).toBe(
      `nats-stream: stream ${stream} holds no message on events.missing.>\n`
    );
  }, 120_000);

  test("returns the newest matching message within its budget", async () => {
    const read = await runReadLast("events.>", budgetSeconds);
    expect(read.result).toEqual({
      message: {
        subject: "events.newest",
        time: expect.any(String),
        seq: expectedSequence,
      },
      timedOut: false,
    });
    expectWithinBudget(read.elapsedMilliseconds, budgetSeconds);
  }, 120_000);

  test("finishes an unmatched sub-second read at its deadline", async () => {
    const read = await runReadLast("events.missing.>", subsecondBudgetSeconds);
    expect(read.result).toEqual({ message: undefined, timedOut: true });
    expectWithinBudget(read.elapsedMilliseconds, subsecondBudgetSeconds);
  }, 120_000);

  // The leak check's control: a client left connected, reconnecting forever as a forgotten client
  // does, is named by the check cleanup runs, and once closed is not.
  test("the leak check names a client still connected, and not one closed", async () => {
    const leftover = await connect({
      servers: serverUrl,
      name: "nats-stream-leftover",
      maxReconnectAttempts: -1,
      reconnectTimeWait: 50,
    });
    try {
      expect(await connectedClients()).toContain("nats-stream-leftover");
    } finally {
      await leftover.close();
    }
    expect(await connectedClients()).not.toContain("nats-stream-leftover");
  }, 120_000);
});
