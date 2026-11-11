import { expect, test } from "bun:test";
import { mkdirSync } from "node:fs";
import path from "node:path";
import { skipWithout } from "@legion/test-fixtures/host-tools";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import { connect } from "nats";

// The recorder reads the bus, so a host with no NATS server runs none of this.
const skip = skipWithout("nats-server");

const work = temporaryDirectory("skill-scenarios-record");

/** A NATS server on a port the kernel picks, and the recorder subscribed to it, writing the run's
 * record. It returns the publisher's connection, a reader of the record and a stop for both.
 *
 * Nothing here is faked: the thing under test is what a subscriber of a real bus sees, which is
 * the reading that was wrong twice. Each check therefore ends by publishing a message of its own
 * after the ones it is about and waiting for that message to be recorded — the server keeps one
 * connection's publishes in order, so the record holding the last one holds every verdict this
 * recorder reached about the ones before it, duplicates dropped and other runs passed over. The
 * wait reads the file until that message is there, which no clock of the test decides. */
async function recording(name: string, prefix: string) {
  const dir = path.join(work, name);
  mkdirSync(dir, { recursive: true });
  const server = Bun.spawn(["nats-server", "-p", "0", "-a", "127.0.0.1"], {
    stdout: "pipe",
    stderr: "pipe",
  });
  const listening = /Listening for client connections on [\d.]+:(\d+)/;
  const reader = server.stderr.getReader();
  let printed = "";
  while (!listening.test(printed)) {
    const { value, done } = await reader.read();
    if (done) throw new Error(`nats-server exited before it listened: ${printed}`);
    printed += new TextDecoder().decode(value);
  }
  const url = `nats://127.0.0.1:${listening.exec(printed)?.[1]}`;
  const file = path.join(dir, "published.jsonl");
  await Bun.write(file, "");
  const recorder = Bun.spawn(
    ["bun", path.join(import.meta.dir, "record-publishes.ts"), file, prefix],
    { env: { ...process.env, SKILL_SCENARIOS_NATS_URL: url }, stdout: "pipe", stderr: "pipe" }
  );
  const printing = recorder.stdout.getReader();
  let said = "";
  while (!said.includes("recording")) {
    const { value, done } = await printing.read();
    if (done) throw new Error(`the recorder exited before it subscribed: ${said}`);
    said += new TextDecoder().decode(value);
  }
  const client = await connect({ servers: url });
  const written = async () =>
    (await Bun.file(file).text())
      .split("\n")
      .filter((line) => line !== "")
      .map((line) => JSON.parse(line) as Record<string, unknown>);
  /** The run's messages, without the recorder's own opening and closing lines. */
  const messages = async () => (await written()).filter((line) => "topic" in line);
  /** Every line of the record once the message naming `last` is one of them. */
  const recorded = async (last: string) => {
    for (;;) {
      const lines = await messages();
      if (lines.some((line) => String(line.message).includes(last))) return lines;
      await Bun.sleep(20);
    }
  };
  /** The record as the rig's stop leaves it: TERM, which the recorder answers with its closing
   * line, and then what the file holds. */
  const stopped = async () => {
    recorder.kill("SIGTERM");
    const status = await recorder.exited;
    return { status, lines: await written() };
  };
  /** The record as the loss of the bus leaves it: the server killed under the recorder, which
   * ends its subscription, and then what the file holds. The recorder makes no reconnect, so
   * this is the end the rig never asks for. */
  const serverLost = async () => {
    server.kill("SIGKILL");
    const status = await recorder.exited;
    return { status, lines: await written(), said: await new Response(recorder.stderr).text() };
  };
  return {
    client,
    recorded,
    written,
    stopped,
    serverLost,
    stop: async () => {
      await client.close();
      recorder.kill();
      server.kill();
    },
  };
}

/** An envelope as the listener publishes one: its own topic, which stays the role topic on every
 * subject a copy travels on, with the message as `payload_summary` (a message of one line) or
 * `payload` (a longer one). */
const envelope = (topic: string, summary: string, payload = "") => ({
  event_id: `e-${summary.length}-${topic}`,
  source: "agent",
  source_session: "01a0f2cc",
  topic,
  dedupe_key: `publish.${summary.length}`,
  issued_at: Date.UTC(2026, 8, 30, 17, 42, 0),
  payload_summary: summary,
  payload,
});

// Catches: a recorder that reads the subject a copy arrived on rather than the envelope's own
// topic. A role message with a live holder never travels on its role subject — the publish route
// forwards it to that session's agent subject and repeats the forward until the receipt
// (`packages/envoy/cmd/listener/confirmed.go`) — and one with no holder is stored on the role's
// kept subject, so a recorder keyed on the role subject records neither, which is what the run of
// 2026-11-09T13:09Z showed: both agents published, the listener accepted both, and the record was
// empty. Catches too a recorder that writes a repeated forward twice, and one that times a
// message by its own clock: `at` is the envelope's `issued_at`, which is when Envoy accepted the
// message, and both scenarios that read this record order their steps by it — the reviewer's
// wake against its read of the record, the implementer's against the push — so a time taken when
// a line was appended, after a repeat or a drain, would order a run by the recorder's own delays.
test.skipIf(skip)(
  "a role message is recorded once, from the envelope's topic, on whatever subject it arrives",
  async () => {
    const run = await recording("subjects", "legion-proof-lweval-1-");
    try {
      const topic = "notifications.role.legion-proof-lweval-1-reviewer";
      const forwarded = envelope(topic, "the fixes are pushed: head abc1234");
      // The kept lane's copy, the agent subject's copy, and the forward repeated for a receipt.
      run.client.publish(
        "notifications.envoy.kept.role.legion-proof-lweval-1-reviewer",
        JSON.stringify(forwarded)
      );
      run.client.publish("notifications.agent.01a0f2cc", JSON.stringify(forwarded));
      run.client.publish("notifications.agent.01a0f2cc", JSON.stringify(forwarded));
      run.client.publish(
        "notifications.agent.01a0f2cc",
        JSON.stringify(
          envelope("notifications.role.legion-proof-lweval-1-architect", "the loop is closed")
        )
      );
      await run.client.flush();
      const lines = await run.recorded("the loop is closed");
      expect(lines).toHaveLength(2);
      expect(lines[0]).toMatchObject({
        at: new Date(forwarded.issued_at).toISOString(),
        topic,
        message: "the fixes are pushed: head abc1234",
        sender: "01a0f2cc",
      });
      // The record's own two ends, which tell a reader it is whole: the recorder wrote the
      // first when it had subscribed, and the second when the rig stopped it (score.ts's
      // `unscored` reads a record without both as a rig error).
      const whole = await run.stopped();
      expect(whole.lines[0]).toMatchObject({ recording: "legion-proof-lweval-1-" });
      expect(whole.lines.at(-1)).toMatchObject({ closed: "stopped" });
      // A stop is the end this file is written for, so it is no failure: a non-zero status with
      // the same log is how a reader would be told a recorder died when it had not.
      expect(whole.status).toBe(0);
    } finally {
      await run.stop();
    }
  },
  120_000
);

// Catches: a recorder that keeps another run's messages, which would score a wake this run never
// sent — each run of a batch has a Legion project of its own for that reason (rig.sh's
// run_project) — and one that drops the body of a message longer than one line, which the
// listener carries as the envelope's payload while its summary holds the first line alone.
test.skipIf(skip)(
  "another run's topic is not recorded, and a message of more than one line is recorded whole",
  async () => {
    const run = await recording("filter", "legion-mine-lweval-1-");
    try {
      run.client.publish(
        "notifications.agent.01a0f2cc",
        JSON.stringify(
          envelope("notifications.role.legion-other-lweval-1-reviewer", "another run's wake")
        )
      );
      run.client.publish(
        "notifications.agent.01a0f2cc",
        JSON.stringify(
          envelope(
            "notifications.role.legion-mine-lweval-1-reviewer",
            "the fixes are pushed",
            "the fixes are pushed\nthe head to read is def5678"
          )
        )
      );
      await run.client.flush();
      const lines = await run.recorded("def5678");
      expect(lines).toHaveLength(1);
      expect(lines[0]).toMatchObject({
        topic: "notifications.role.legion-mine-lweval-1-reviewer",
        message: "the fixes are pushed\nthe head to read is def5678",
      });
    } finally {
      await run.stop();
    }
  },
  120_000
);

// Catches: a recorder that passes over a message of this run it cannot read. What it could not
// read is this reader's limit and never an act an agent did not do, so the line goes into the
// record and `unscored` reads the run as a rig error (score.ts), where a silent skip would score
// the message as never sent.
test.skipIf(skip)(
  "a message of this run the recorder cannot read is written as unreadable",
  async () => {
    const run = await recording("unreadable", "legion-odd-lweval-1-");
    try {
      const topic = "notifications.role.legion-odd-lweval-1-reviewer";
      // An envelope with no `issued_at` and no body: a frame of this topic that the listener's own
      // shape does not describe.
      run.client.publish(
        "notifications.agent.01a0f2cc",
        JSON.stringify({ event_id: "e-odd", topic, source_session: "01a0f2cc" })
      );
      run.client.publish(
        "notifications.agent.01a0f2cc",
        JSON.stringify(envelope(topic, "the fixes are pushed: head abc1234"))
      );
      await run.client.flush();
      const lines = await run.recorded("the fixes are pushed");
      expect((await run.written()).some((line) => line.unreadable === topic)).toBe(true);
      expect(lines).toHaveLength(1);
    } finally {
      await run.stop();
    }
  },
  120_000
);

// Catches: a recorder that records a message Envoy accepted but nobody can attribute. A publish
// with no `source_session` is one Envoy takes (`packages/envoy/AGENTS.md`: it acts as no session
// and is not refused), so it is no defect of the listener — but the score reads who sent a
// message by that session against each half's own transcript, so a message without one cannot be
// read as any half's. It is written as `unreadable`, which makes its run a rig error, rather than
// recorded with a blank sender that a transcript without a header would match.
test.skipIf(skip)(
  "a message with no session of its own is unreadable, not a blank sender",
  async () => {
    const run = await recording("no-session", "legion-void-lweval-1-");
    try {
      const topic = "notifications.role.legion-void-lweval-1-reviewer";
      const { source_session, ...sessionless } = envelope(topic, "a wake from no session");
      run.client.publish("notifications.agent.01a0f2cc", JSON.stringify(sessionless));
      run.client.publish(
        "notifications.agent.01a0f2cc",
        JSON.stringify(envelope(topic, "the fixes are pushed: head abc1234"))
      );
      await run.client.flush();
      const lines = await run.recorded("the fixes are pushed");
      expect((await run.written()).some((line) => line.unreadable === topic)).toBe(true);
      expect(lines).toHaveLength(1);
      expect(lines[0]).toMatchObject({ sender: "01a0f2cc" });
    } finally {
      await run.stop();
    }
  },
  120_000
);

// Catches: a recorder that ends quietly when the bus goes away, which is the one end that must
// leave the record unfinished. It makes no reconnect, because core NATS loses whatever was
// published while a connection is down: what the run sent in the gap is unknown, so the record
// must carry no `closed` line and the process must exit non-zero, which is what tells the rig
// its recorder died. A recorder that wrote the closing line here, or exited 0, would have every
// run of that record scored as a loop whose agents sent nothing (score.ts's `unscored`).
test.skipIf(skip)(
  "a bus that goes away ends the recorder with no closing line and a failure",
  async () => {
    const run = await recording("dropped", "legion-lost-lweval-1-");
    try {
      const topic = "notifications.role.legion-lost-lweval-1-reviewer";
      run.client.publish(
        "notifications.agent.01a0f2cc",
        JSON.stringify(envelope(topic, "the fixes are pushed: head abc1234"))
      );
      await run.client.flush();
      await run.recorded("the fixes are pushed");
      const lost = await run.serverLost();
      expect(lost.status).toBe(1);
      expect(lost.lines.some((line) => "closed" in line)).toBe(false);
      expect(lost.lines[0]).toMatchObject({ recording: "legion-lost-lweval-1-" });
      expect(lost.said).toContain("the subscription ended before the run did");
    } finally {
      await run.stop();
    }
  },
  120_000
);
