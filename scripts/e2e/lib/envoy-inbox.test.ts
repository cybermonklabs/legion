import { afterEach, expect, test } from "bun:test";
import { existsSync, mkdtempSync, readFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { makeRecorder, renderDelivery } from "./envoy-inbox";

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

test("records nothing for a frame that is not an envelope", () => {
  const record = join(workDir(), "inbox.jsonl");
  const write = makeRecorder(record);
  expect(write("rig-asker", "not json at all")).toBe(false);
  expect(existsSync(record)).toBe(false);
});
