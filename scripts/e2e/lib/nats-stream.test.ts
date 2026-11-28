import { afterAll, beforeAll, expect, test } from "bun:test";
import { randomUUID } from "node:crypto";
import { mkdirSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { hostGate } from "@legion/test-fixtures/host-tools";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import { connect, type NatsConnection } from "nats";
import { type LastRead, readLast } from "./nats-stream";
import { rowWaitMarginMs, rowWaits } from "./row-waits";

// The NATS server these rows read is a container this file starts, so a host with no docker runs
// none of them.
const host = hostGate({ programs: ["docker"] });
const root = join(import.meta.dir, "..", "..", "..");
const container = `legion-nats-stream-${randomUUID()}`;
const stream = `NATS_STREAM_${randomUUID().replaceAll("-", "_")}`;
const configDirectory = temporaryDirectory("nats-stream-test");
const config = join(configDirectory, "envoy.json");
const subjectCount = 10_000;
const publishBatchSize = 25;
// A read that must find its message gets the end of the row's waits as its budget (lib/row-waits.ts):
// how soon a loaded host's server answers is not the row's subject, and readLast's own deadline then
// bounds it. A read whose subject is its deadline gets a short one, under a second.
const rowBoundMs = 120_000;
const waits = rowWaits(rowBoundMs);
const subsecondBudgetSeconds = 0.1;
let serverUrl = "";
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
    // ghcr.io/cybermonklabs/legion/ci, the copy of nats:2.10 (.github/ci-images.yaml)
    "ghcr.io/cybermonklabs/legion/ci@sha256:5498ba57b9471840be3d15b033a4eec554d1c02fa6c2cc0ca2d888637f6c6e2f",
    "-js"
  );
  removeContainer = () => {
    Bun.spawnSync(["docker", "rm", "-f", "-v", container], { cwd: root });
  };
  try {
    const endpoint = docker("port", container, "4222/tcp").trim();
    serverUrl = `nats://${endpoint}`;

    nc = await connect({
      servers: serverUrl,
      timeout: hookLeft(),
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

function runLast(subject: string, seconds: number) {
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

// runReadLast reads with a client of its own. HOLD, when given, runs once the client has connected
// and before the read, and what it answers runs once the read has ended, whatever its end. The
// client is closed whatever fails, and a release that fails does not hide the read's own failure;
// after a read that succeeded it fails the row, since the server it left held would fail the next.
async function runReadLast(subject: string, seconds: number, hold?: () => () => void) {
  const client = await connect({
    servers: serverUrl,
    timeout: Math.max(1, Math.ceil(waits.end() - performance.now())),
    maxReconnectAttempts: 0,
  });
  let release: (() => void) | undefined;
  try {
    let read: { result: LastRead } | undefined;
    let failure: { error: unknown } | undefined;
    try {
      release = hold?.();
      const result = await readLast(client, stream, subject, seconds);
      read = { result };
    } catch (error) {
      failure = { error };
    }
    try {
      release?.();
    } catch (error) {
      failure ??= { error };
    }
    if (failure) throw failure.error;
    return read as { result: LastRead };
  } finally {
    void client.close();
  }
}

// The hook's own end, 20 s before its bound, as a row's (lib/row-waits.ts): each of its waits on
// the server (the connect, the stream's creation, each publish) gives up there and not at a
// timeout of its own, nats.js's 5 s for a JetStream API request included.
let hookEnd = 0;
const hookLeft = () => Math.max(1, Math.ceil(hookEnd - performance.now()));

async function cleanup(): Promise<void> {
  try {
    await nc?.close();
  } finally {
    removeContainer?.();
    removeContainer = undefined;
  }
}

beforeAll(async () => {
  if (host.missing) return;
  try {
    hookEnd = performance.now() + rowBoundMs - rowWaitMarginMs;
    await startNats();
    mkdirSync(configDirectory, { recursive: true });
    writeFileSync(config, JSON.stringify({ envoy: { enabled: true, natsUrls: [serverUrl] } }));

    if (!nc) throw new Error("NATS connection was not established");
    const manager = await nc.jetstreamManager({ timeout: hookLeft() });
    await manager.streams.add({ name: stream, subjects: ["events.>"] });
    const js = nc.jetstream({ timeout: hookLeft() });
    for (let start = 0; start < subjectCount; start += publishBatchSize) {
      const end = Math.min(start + publishBatchSize, subjectCount);
      await Promise.all(
        Array.from({ length: end - start }, (_, index) =>
          js.publish(`events.subject.${start + index}`, new Uint8Array(), {
            timeout: hookLeft(),
          })
        )
      );
    }
    expectedSequence = (
      await js.publish("events.newest", new Uint8Array(), { timeout: hookLeft() })
    ).seq;
  } catch (error) {
    await cleanup();
    throw error;
  }
}, rowBoundMs);

afterAll(async () => {
  // With no docker nothing was started, and the fixture's own hook removes the scratch directory
  // this file's module scope made.
  if (host.missing) return;
  await cleanup();
}, 120_000);

host.describe("nats-stream last", () => {
  test(
    "reads the newest matching message without draining every subject",
    () => {
      const result = runLast("events.>", waits.secondsLeft());
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
    },
    rowBoundMs
  );

  test(
    "reports a read with no message by its budget as unknown, exit 3, never as a stream that holds none",
    () => {
      const result = runLast("events.missing.>", subsecondBudgetSeconds);
      expect(result.stderr).toBe(
        `nats-stream: no message on events.missing.> from stream ${stream} within ${subsecondBudgetSeconds} s: unknown whether it holds one, since a stream with none and a server that did not answer read the same\n`
      );
      expect(result.exitCode).toBe(3);
      expect(result.stdout).toBe("");
    },
    rowBoundMs
  );

  test(
    "returns the newest matching message within its budget",
    async () => {
      const read = await runReadLast("events.>", waits.secondsLeft());
      expect(read.result).toEqual({
        message: {
          subject: "events.newest",
          time: expect.any(String),
          seq: expectedSequence,
        },
        timedOut: false,
      });
    },
    rowBoundMs
  );

  // The server is paused before the read and resumed only once the read has ended, so nothing it
  // sends can end the read: only readLast's own deadline can, and a read that waited out the
  // server instead would fail when its JetStream request gave up, a second after its budget.
  test(
    "finishes a sub-second read the server does not answer at its own deadline",
    async () => {
      const read = await runReadLast("events.missing.>", subsecondBudgetSeconds, () => {
        docker("pause", container);
        return () => docker("unpause", container);
      });
      expect(read.result).toEqual({ message: undefined, timedOut: true });
    },
    rowBoundMs
  );
});
