import { afterAll, beforeAll, describe, expect, spyOn, test } from "bun:test";
import { randomUUID } from "node:crypto";
import { mkdirSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { skipWithout } from "@legion/test-fixtures/host-tools";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import { connect, type NatsConnection } from "nats";
import { type LastRead, readLast } from "./nats-stream";
import { withinRow } from "./within-row";

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
// A budget whose fetch the server holds for an hour, which no row reaches: a read under it returns
// on the message, or at a deadline the row lets pass, never at a time the machine's speed sets.
const hourSeconds = 3_600;
// The bound of each of this file's two hooks, and the deadline the hook's steps share: each docker
// call and each wait on the NATS server takes what is left of it, less reportMilliseconds, the
// time the hook keeps to throw that step's failure, named, before Bun's own guard of the hook ends
// it without a name. Bun's guard sends a spawnSync child one SIGTERM and then waits for it: a
// `docker port` against a daemon that did not answer outlived it, and the start hook ran out at
// 187 s, 67 s past this bound, naming no step.
const hookMilliseconds = 120_000;
const reportMilliseconds = 1_000;
let serverUrl = "";
let expectedSequence = 0;
let nc: NatsConnection | undefined;
// The container's removal, set the moment `docker run` returns or is killed at its deadline, and
// run by cleanup; `-v` goes with it, since a plain `docker rm -f` leaves the image's declared volume
// on the host (docs/testing.md). It stays set until a removal answers, so a start hook whose
// deadline left it no time leaves it to the cleanup hook.
let removeContainer: ((deadline: number) => void) | undefined;

// The deadline of a hook that starts now, in performance.now() milliseconds.
function hookDeadline(): number {
  return performance.now() + hookMilliseconds - reportMilliseconds;
}

// What is left before DEADLINE for STEP, or a failure naming STEP when nothing is.
function left(deadline: number, step: string): number {
  const milliseconds = Math.floor(deadline - performance.now());
  if (milliseconds <= 0) {
    throw new Error(`${step}: not started, its hook's ${hookMilliseconds} ms had run out`);
  }
  return milliseconds;
}

// One docker command, killed at DEADLINE: SIGKILL, since the CLI may not end on SIGTERM while its
// daemon does not answer. A command killed there fails naming itself and the time it had.
function dockerRun(deadline: number, args: string[]) {
  const step = `docker ${args.join(" ")}`;
  const timeout = left(deadline, step);
  const result = Bun.spawnSync(["docker", ...args], { cwd: root, timeout, killSignal: "SIGKILL" });
  if (result.exitedDueToTimeout) {
    throw new Error(`${step}: no answer in the ${timeout} ms its hook's deadline left it; killed`);
  }
  return { step, result };
}

function docker(deadline: number, ...args: string[]): string {
  const { step, result } = dockerRun(deadline, args);
  const stdout = result.stdout.toString();
  const stderr = result.stderr.toString();
  if (result.exitCode !== 0) throw new Error(`${step}: ${stdout}${stderr}`);
  return stdout;
}

async function startNats(deadline: number): Promise<void> {
  try {
    docker(
      deadline,
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
  } finally {
    // A run killed at its deadline may have made the container all the same. Its exit status is
    // not read: the container a killed run never made is no such container.
    removeContainer = (removalDeadline) => {
      dockerRun(removalDeadline, ["rm", "-f", "-v", container]);
    };
  }
  const endpoint = docker(deadline, "port", container, "4222/tcp").trim();
  serverUrl = `nats://${endpoint}`;
  nc = await connect({
    servers: serverUrl,
    timeout: left(deadline, `the connection to ${serverUrl}`),
    waitOnFirstConnect: true,
    reconnectTimeWait: 50,
    maxReconnectAttempts: -1,
  });
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

/**
 * Starts readLast against the server with its deadline in the row's hands. readLast arms its
 * deadline first thing, before its first await: that one timer is captured rather than armed, so
 * the deadline passes only when the row calls `expire`, and the row reads the delay readLast asked
 * for. Every later timer, the NATS client's own among them, is armed as usual. The read is the
 * row's (withinRow), and its client closes once it settles.
 */
async function startReadLast(subject: string, seconds: number) {
  const client = await withinRow(
    connect({ servers: serverUrl, timeout: 10_000, maxReconnectAttempts: 0 })
  );
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
  return { read: withinRow(closed), deadline };
}

// Closes the connection and removes the container, the removal killed at DEADLINE and failing by
// name there.
async function cleanup(deadline: number): Promise<void> {
  try {
    await nc?.close();
  } finally {
    removeContainer?.(deadline);
    removeContainer = undefined;
  }
}

// A wait on the NATS server, failing with STEP's name when the server refuses it or does not
// answer in the time the hook's deadline gave it.
async function named<T>(step: string, wait: Promise<T>): Promise<T> {
  try {
    return await wait;
  } catch (error) {
    throw new Error(`${step}: ${error instanceof Error ? error.message : String(error)}`, {
      cause: error,
    });
  }
}

beforeAll(async () => {
  if (skip) return;
  const deadline = hookDeadline();
  try {
    await startNats(deadline);
    mkdirSync(configDirectory, { recursive: true });
    writeFileSync(config, JSON.stringify({ envoy: { enabled: true, natsUrls: [serverUrl] } }));

    if (!nc) throw new Error("NATS connection was not established");
    const manager = await nc.jetstreamManager({ timeout: left(deadline, "the stream's manager") });
    await named(
      `adding the stream ${stream}`,
      manager.streams.add({ name: stream, subjects: ["events.>"] })
    );
    const js = nc.jetstream();
    for (let start = 0; start < subjectCount; start += publishBatchSize) {
      const end = Math.min(start + publishBatchSize, subjectCount);
      const step = `publishing events.subject.${start} to ${end - 1}`;
      const timeout = left(deadline, step);
      await named(
        step,
        Promise.all(
          Array.from({ length: end - start }, (_, index) =>
            js.publish(`events.subject.${start + index}`, new Uint8Array(), { timeout })
          )
        )
      );
    }
    const timeout = left(deadline, "publishing events.newest");
    expectedSequence = (
      await named(
        "publishing events.newest",
        js.publish("events.newest", new Uint8Array(), { timeout })
      )
    ).seq;
  } catch (error) {
    // What is left of this hook's deadline goes to the removal; a removal it leaves undone stays
    // registered for the cleanup hook, and the failure that ended the start is the one thrown.
    await cleanup(deadline).catch(() => {});
    throw error;
  }
}, hookMilliseconds);

afterAll(async () => {
  // With no docker nothing was started, and the fixture's own hook removes the scratch directory
  // this file's module scope made.
  if (skip) return;
  await cleanup(hookDeadline());
}, hookMilliseconds);

describe.skipIf(skip)("nats-stream last", () => {
  test("reads the newest matching message without draining every subject", () => {
    // An hour's budget: at the 5 s budget the row had, a read that a loaded machine kept past
    // 5 s printed no message and failed the row on the machine's speed.
    const result = runLast("events.>", hourSeconds);
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

  test("returns the newest matching message on the message, never on its deadline", async () => {
    // The row never lets the deadline pass, and the server holds the fetch for an hour, so the
    // read returns on the message alone.
    const { read, deadline } = await startReadLast("events.>", hourSeconds);
    expect(deadline.delay).toBe(hourSeconds * 1_000);
    expect(await read).toEqual({
      message: {
        subject: "events.newest",
        time: expect.any(String),
        seq: expectedSequence,
      },
      timedOut: false,
    });
  }, 120_000);

  // The CLI row above reads a sub-second budget against the server; this one holds the read to
  // its deadline. The budget is an hour, which is also how long the server holds the read's fetch,
  // and the row lets the deadline pass once that fetch waits at the server: a read that went on
  // waiting for the fetch rather than finishing at its deadline does not finish, and the row's
  // guard ends it. The elapsed time the row compared with the budget in place of this was the
  // machine's speed as much as the read's.
  test("finishes an unmatched read at its deadline, not at its fetch's expiry", async () => {
    const filter = "events.missing.at-the-deadline.>";
    const { read, deadline } = await startReadLast(filter, hourSeconds);
    expect(deadline.delay).toBe(hourSeconds * 1_000);
    const manager = await withinRow((nc as NatsConnection).jetstreamManager());
    // A poll of the server's own count of the fetches waiting on the read's consumer: the fact the
    // deadline must pass after, read every 50 ms until it holds.
    const waiting = async () => {
      for await (const info of manager.consumers.list(stream)) {
        const filters = info.config.filter_subjects ?? [info.config.filter_subject];
        if (filters.includes(filter) && info.num_waiting > 0) return true;
      }
      return false;
    };
    while (!(await withinRow(waiting()))) await withinRow(Bun.sleep(50));
    deadline.expire();
    expect(await read).toEqual({ message: undefined, timedOut: true });
  }, 120_000);
});
