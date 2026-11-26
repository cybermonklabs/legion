import { afterEach, expect, test } from "bun:test";
import { existsSync, mkdtempSync, readFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { makeRecorder, PROBE_HEADER, renderDelivery, takeMessage } from "./envoy-inbox";

const cleanups: (() => void)[] = [];
afterEach(() => {
  for (const cleanup of cleanups.splice(0)) cleanup();
});

function workDir() {
  const dir = mkdtempSync(join(tmpdir(), "envoy-inbox-test."));
  cleanups.push(() => rmSync(dir, { recursive: true, force: true }));
  return dir;
}

test("renders an answer as the session's deliverer does: reply header, message, topic", () => {
  expect(
    renderDelivery({
      topic: "notifications.agent.rig-asker",
      in_reply_to: "evt-1",
      payload: "the answer",
      payload_summary: "the summary",
    })
  ).toBe("In Reply To: evt-1\nMessage:\nthe answer\n\nTopic: notifications.agent.rig-asker");
});

test("renders an envelope that answers nothing and carries only a summary", () => {
  expect(
    renderDelivery({ topic: "notifications.legion.alf1.controller", payload_summary: "a" })
  ).toBe("Message:\na\n\nTopic: notifications.legion.alf1.controller");
});

test("records one line per envelope, naming the session the subject was watched for", () => {
  const record = join(workDir(), "inbox.jsonl");
  const write = makeRecorder(record);
  expect(
    write(
      "rig-asker",
      JSON.stringify({
        topic: "notifications.agent.rig-asker",
        dedupe_key: "agent.rig-asker.1",
        in_reply_to: "evt-1",
        payload: "the answer",
      })
    )
  ).toBe(true);
  const [line] = readFileSync(record, "utf8").trim().split("\n");
  const entry = JSON.parse(line ?? "") as { at: string; session: string; text: string };
  expect(entry.session).toBe("rig-asker");
  expect(entry.text).toBe(
    "In Reply To: evt-1\nMessage:\nthe answer\n\nTopic: notifications.agent.rig-asker"
  );
  expect(Number.isNaN(Date.parse(entry.at))).toBe(false);
});

test("writes an envelope's dedupe key once, and each other envelope on its own line", () => {
  const record = join(workDir(), "inbox.jsonl");
  const write = makeRecorder(record);
  const envelope = (key: string, message: string) =>
    JSON.stringify({ topic: "notifications.agent.rig-asker", dedupe_key: key, payload: message });
  expect(write("rig-asker", envelope("agent.rig-asker.1", "once"))).toBe(true);
  expect(write("rig-asker", envelope("agent.rig-asker.1", "once"))).toBe(false);
  expect(write("rig-controller", envelope("agent.rig-asker.1", "once"))).toBe(false);
  expect(write("rig-asker", envelope("agent.rig-asker.2", "twice"))).toBe(true);
  const texts = readFileSync(record, "utf8")
    .trim()
    .split("\n")
    .map((line) => {
      const entry = JSON.parse(line) as { text: string };
      return entry.text;
    });
  expect(texts).toEqual([
    "Message:\nonce\n\nTopic: notifications.agent.rig-asker",
    "Message:\ntwice\n\nTopic: notifications.agent.rig-asker",
  ]);
});

// Proof run 43996451813 (the deployment repository, at cb8eb857): the listener accepted twelve
// notices on notifications.legion.alp22215.controller and the controller stand-in, registered for
// that lane, recorded one. Each daemon keys a notice by its own outbox row (legion-outbox:<row>), and
// the run's two daemons have a database each, so the second one's keys repeat the first one's on
// another lane. A dedupe key names one envelope on one topic: the same key on another topic is
// another message, and recording it is what the criteria read.
test("records an envelope whose dedupe key another topic already used", () => {
  const record = join(workDir(), "inbox.jsonl");
  const write = makeRecorder(record);
  const notice = (lane: string, message: string) =>
    JSON.stringify({
      topic: `notifications.legion.${lane}.controller`,
      dedupe_key: "legion-outbox:12",
      payload: message,
    });
  expect(write("rig-controller", notice("alf4242", "the first daemon's"))).toBe(true);
  expect(write("rig-controller", notice("alp4242", "the second daemon's"))).toBe(true);
  expect(write("rig-controller", notice("alp4242", "the second daemon's"))).toBe(false);
  expect(readFileSync(record, "utf8").trim().split("\n")).toHaveLength(2);
});

test("records nothing for a frame that is not an envelope", () => {
  const record = join(workDir(), "inbox.jsonl");
  const write = makeRecorder(record);
  expect(write("rig-asker", "not json at all")).toBe(false);
  expect(existsSync(record)).toBe(false);
});

/** A relay frame as the listener publishes it: the stored envelope's bytes, on a reply subject. */
function frame(envelope: object, reply = "_INBOX.relay.1") {
  return { data: new TextEncoder().encode(JSON.stringify(envelope)), reply };
}

test("answers a relay frame done once it is recorded, and a repeat of it done too", () => {
  const record = join(workDir(), "inbox.jsonl");
  const write = makeRecorder(record);
  const answers: [string, string][] = [];
  const answer = (reply: string, word: string) => answers.push([reply, word]);
  const envelope = {
    topic: "notifications.agent.rig-asker",
    dedupe_key: "agent.rig-asker.1",
    payload: "the answer",
  };
  takeMessage("rig-asker", frame(envelope, "_INBOX.relay.1"), write, answer);
  // The relay offers a frame again after a restart at its consumer's ack floor: recorded once,
  // answered each time, so the listener acknowledges it rather than offering it for ever.
  takeMessage("rig-asker", frame(envelope, "_INBOX.relay.2"), write, answer);
  expect(answers).toEqual([
    ["_INBOX.relay.1", "done"],
    ["_INBOX.relay.2", "done"],
  ]);
  expect(readFileSync(record, "utf8").trim().split("\n")).toHaveLength(1);
});

test("answers the listener's connection probe done and records nothing of it", () => {
  const record = join(workDir(), "inbox.jsonl");
  const answers: [string, string][] = [];
  const probe = {
    data: new Uint8Array(),
    reply: "_INBOX.probe.1",
    headers: { get: (name: string) => (name === PROBE_HEADER ? "1" : "") },
  };
  takeMessage("rig-asker", probe, makeRecorder(record), (reply, word) =>
    answers.push([reply, word])
  );
  expect(answers).toEqual([["_INBOX.probe.1", "done"]]);
  expect(existsSync(record)).toBe(false);
});

test("answers a frame that is no envelope done, and a message with no reply subject not at all", () => {
  const record = join(workDir(), "inbox.jsonl");
  const write = makeRecorder(record);
  const answers: [string, string][] = [];
  const answer = (reply: string, word: string) => answers.push([reply, word]);
  takeMessage(
    "rig-asker",
    { data: new TextEncoder().encode("not json"), reply: "_INBOX.x" },
    write,
    answer
  );
  takeMessage("rig-asker", { data: new TextEncoder().encode("{}") }, write, answer);
  expect(answers).toEqual([["_INBOX.x", "done"]]);
});

test("leaves a frame whose record failed unanswered, so its consumer offers it again", () => {
  const answers: string[] = [];
  const failing = () => {
    throw new Error("disk full");
  };
  expect(() =>
    takeMessage("rig-asker", frame({ topic: "t" }), failing, (reply) => answers.push(reply))
  ).toThrow("disk full");
  expect(answers).toEqual([]);
});
