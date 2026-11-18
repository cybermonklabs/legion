import { afterAll, beforeAll, describe, expect, spyOn, test } from "bun:test";
import { randomUUID } from "node:crypto";
import { mkdirSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { bunCacheOff } from "@legion/test-fixtures/bun-child-environment";
import { skipWithout } from "@legion/test-fixtures/host-tools";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import { settled, timeLeft, until } from "@legion/test-fixtures/wait";
import { connect, type NatsConnection } from "nats";
import { type LastRead, readLast } from "./nats-stream";

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
const budgetSeconds = 5;
const subsecondBudgetSeconds = 0.1;
/** A budget whose fetch the server holds for an hour, which no row reaches. */
const hourSeconds = 3_600;
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
      timeout: timeLeft(),
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
    ...bunCacheOff,
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

/**
 * Starts readLast against the server with its deadline in the row's hands. readLast arms its
 * deadline first thing, before its first await: that one timer is captured rather than armed, so
 * the deadline passes only when the row calls `expire`, and the row reads the delay readLast asked
 * for. Every later timer, the NATS client's own among them, is armed as usual.
 */
async function startReadLast(subject: string, seconds: number) {
  const client = await connect({
    servers: serverUrl,
    timeout: timeLeft(),
    maxReconnectAttempts: 0,
  });
  const captured: { deadline?: { readonly delay: number; readonly expire: () => void } } = {};
  const timers = spyOn(globalThis, "setTimeout").mockImplementationOnce(((
    expire: () => void,
    delay = 0
  ) => {
    captured.deadline = { delay, expire };
    return 0;
  }) as unknown as typeof setTimeout);
  let read: Promise<LastRead>;
  try {
    read = readLast(client, stream, subject, seconds);
  } finally {
    timers.mockRestore();
  }
  const closed = read.finally(() => void client.close());
  // Handled here as well, so a read that fails before the row awaits it does not end the run.
  void closed.catch(() => {});
  const { deadline } = captured;
  if (deadline === undefined) throw new Error("readLast armed no deadline before its first await");
  return { read: closed, deadline };
}

async function cleanup(): Promise<void> {
  try {
    await nc?.close();
  } finally {
    removeContainer?.();
    removeContainer = undefined;
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
            timeout: timeLeft(),
          })
        )
      );
    }
    expectedSequence = (
      await js.publish("events.newest", new Uint8Array(), { timeout: timeLeft() })
    ).seq;
  } catch (error) {
    await cleanup();
    throw error;
  }
});

afterAll(async () => {
  // With no docker nothing was started, and the fixture's own hook removes the scratch directory
  // this file's module scope made.
  if (skip) return;
  await cleanup();
});

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
  });

  test("reports the existing no-message error for an unmatched filter", () => {
    const result = runLast("events.missing.>", subsecondBudgetSeconds);
    expect(result.exitCode).toBe(1);
    expect(result.stdout).toBe("");
    expect(result.stderr).toBe(
      `nats-stream: stream ${stream} holds no message on events.missing.>\n`
    );
  });

  test("returns the newest matching message on the message, never on its deadline", async () => {
    // The row never lets the deadline pass, so the read returns on the message or on the
    // server's end of the fetch, which comes at the same budget and holds no message.
    const { read, deadline } = await startReadLast("events.>", budgetSeconds);
    expect(deadline.delay).toBe(budgetSeconds * 1_000);
    expect(await settled("readLast's answer", read)).toEqual({
      message: {
        subject: "events.newest",
        time: expect.any(String),
        seq: expectedSequence,
      },
      timedOut: false,
    });
  });

  // The CLI row above reads a sub-second budget against the server; this one holds the read to
  // its deadline. The budget is an hour, which is also how long the server holds the read's fetch,
  // and the row lets the deadline pass once that fetch waits at the server: a read that went on
  // waiting for the fetch rather than finishing at its deadline does not finish before the row's
  // own deadline, and the row's wait for it names it.
  test("finishes an unmatched read at its deadline, not at its fetch's expiry", async () => {
    const filter = "events.missing.at-the-deadline.>";
    const { read, deadline } = await startReadLast(filter, hourSeconds);
    expect(deadline.delay).toBe(hourSeconds * 1_000);
    const manager = await (nc as NatsConnection).jetstreamManager();
    await until("readLast's fetch to wait at the server", async () => {
      for await (const info of manager.consumers.list(stream)) {
        const filters = info.config.filter_subjects ?? [info.config.filter_subject];
        if (filters.includes(filter) && info.num_waiting > 0) return true;
      }
      return false;
    });
    deadline.expire();
    expect(await settled("readLast to finish once its deadline passes", read)).toEqual({
      message: undefined,
      timedOut: true,
    });
  });
});
