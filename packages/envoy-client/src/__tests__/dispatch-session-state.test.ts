import { describe, expect, test } from "bun:test";
import {
  appendFileSync,
  existsSync,
  mkdirSync,
  readFileSync,
  statSync,
  utimesSync,
  writeFileSync,
} from "node:fs";
import { join } from "node:path";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import { askgateStore, MARKER_MAX_AGE_MS, pidNamespace, processStart } from "../askgate-marker";
import { adviceMemory, resetAdviceMemory } from "../dispatch-execute";
import { forgetKeyedLayerMemory, keyedLayerMemory, rememberKeyedLayer } from "../dispatch-http";
import { forgetShownPictures, shownPictures } from "../dispatch-picture-tools";
import { sessionDirectory } from "../dispatch-session-directory";
import {
  appendResult,
  loadSessionMemory,
  pruneSessions,
  readResendRecord,
  readResultsSince,
  readSessionTitle,
  recordOutcome,
  resultsEnd,
  saveSessionMemory,
  writePicture,
  writeResendInput,
  writeResendRecord,
} from "../dispatch-session-state";

const root = temporaryDirectories("dispatch-state");

describe("per-session state", () => {
  test("memory survives a process: what one call saved, the next call loads", () => {
    const env = { DISPATCH_STATE_DIR: root() };
    const dir = sessionDirectory(env, "s1");
    resetAdviceMemory();
    forgetKeyedLayerMemory();
    adviceMemory().add("triage:LEGION-2");
    shownPictures("s1").add("dispatch://LEGION-2/artifact/a@v1");
    const seen = new Date(Date.now() - 60_000).toISOString();
    rememberKeyedLayer({ "https://dispatch.example": seen });
    saveSessionMemory(dir, "s1", new Set(["ask-1"]));
    resetAdviceMemory();
    forgetShownPictures("s1");
    forgetKeyedLayerMemory();
    const follows = loadSessionMemory(dir, "s1");
    expect(adviceMemory().has("triage:LEGION-2")).toBe(true);
    expect(shownPictures("s1").has("dispatch://LEGION-2/artifact/a@v1")).toBe(true);
    expect(follows.has("ask-1")).toBe(true);
    // The sighting keeps its own time, so it still ends five minutes after the answer that showed
    // it, however many calls carry it on.
    const carried = keyedLayerMemory()["https://dispatch.example"];
    expect(Math.abs(Date.parse(carried ?? "") - Date.parse(seen))).toBeLessThan(1_000);
    forgetKeyedLayerMemory();
  });

  test("the ledger returns only what was appended after an offset", () => {
    const dir = sessionDirectory({ DISPATCH_STATE_DIR: root() }, "s2");
    const unexpected = (problem: string) => {
      throw new Error(problem);
    };
    appendResult(dir, { tool: "dispatch_search", details: {} });
    const first = readResultsSince(dir, 0, unexpected);
    appendResult(dir, { tool: "dispatch_ask", details: { ask: "a" } });
    const second = readResultsSince(dir, first.offset, unexpected);
    expect(first.entries.map((e) => e.tool)).toEqual(["dispatch_search"]);
    expect(second.entries.map((e) => e.tool)).toEqual(["dispatch_ask"]);
  });

  test("a reader that starts at the ledger's end reads only what is appended after it", () => {
    const dir = sessionDirectory({ DISPATCH_STATE_DIR: root() }, "s4");
    expect(resultsEnd(dir)).toBe(0);
    appendResult(dir, { tool: "dispatch_search", details: {} });
    const end = resultsEnd(dir);
    appendResult(dir, { tool: "dispatch_ask", details: { ask: "a" } });
    const read = readResultsSince(dir, end, (problem) => {
      throw new Error(problem);
    });
    expect(read.entries.map((e) => e.tool)).toEqual(["dispatch_ask"]);
  });

  test("a picture is written once under its hash", () => {
    const dir = sessionDirectory({ DISPATCH_STATE_DIR: root() }, "s3");
    const png = Buffer.from("89504e470d0a1a0a0000", "hex").toString("base64");
    const a = writePicture(dir, { data: png, mimeType: "image/png" });
    const b = writePicture(dir, { data: png, mimeType: "image/png" });
    expect(a.path).toBe(b.path);
    expect(a.path.endsWith(".png")).toBe(true);
    expect(readFileSync(a.path).subarray(0, 4).toString("hex")).toBe("89504e47");
  });

  // A write that may have landed is sent again by a command the CLI prints. What it read from
  // stdin, or a value too long to print, is kept here for that command to read, named by the key
  // the command is sent with, its place, and its content: a later call under the same key never
  // replaces a copy an earlier printed command reads.
  test("a resend's input is kept whole and owner-only, by its key, place and content", () => {
    const dir = sessionDirectory({ DISPATCH_STATE_DIR: root() }, "s10");
    const key = "0b9c1e7a-55f4-4d2e-9a6b-3c2f1d0e8a71";
    const text = "line one\nEOF\n$HOME `pwd`\n";
    const path = writeResendInput(dir, key, 2, text);
    expect(path).toMatch(new RegExp(`/resend/${key}\\.2\\.[0-9a-f]{16}$`));
    expect(readFileSync(path, "utf-8")).toBe(text);
    expect(statSync(path).mode & 0o777).toBe(0o600);
    expect(writeResendInput(dir, key, 2, text)).toBe(path);
    const other = writeResendInput(dir, key, 2, "another text\n");
    expect(other).not.toBe(path);
    expect(readFileSync(path, "utf-8")).toBe(text);
  });

  // The requests a call whose write may have landed sent, by key: a call sent again under that key
  // sends each as it was first sent. A record never changes what it holds; a later call adds only
  // the writes it holds none of.
  test("a call's requests are kept under its key, each as first sent", () => {
    const dir = sessionDirectory({ DISPATCH_STATE_DIR: root() }, "s11");
    const key = "0b9c1e7a-55f4-4d2e-9a6b-3c2f1d0e8a71";
    expect(readResendRecord(dir, key)).toEqual([]);
    const message = {
      key,
      method: "POST",
      path: "/api/v1/issues/DSP-1/messages",
      type: "application/json",
      content: '{"body":"as first sent"}',
    };
    const upload = {
      key: `${key}.2`,
      method: "POST",
      path: "/api/v1/issues/DSP-1/artifacts",
      type: "multipart/form-data; boundary=x",
      content: new Uint8Array([0, 1, 255]),
    };
    writeResendRecord(dir, key, [message, upload]);
    const third = { ...message, key: `${key}.3` };
    writeResendRecord(dir, key, [{ ...message, content: '{"body":"another"}' }, third]);
    expect(readResendRecord(dir, key)).toEqual([message, upload, third]);
    expect(statSync(join(dir, "resend", `${key}.writes`, `${key}.json`)).mode & 0o777).toBe(0o600);
  });

  // Two calls under one key can keep their requests at the same moment (two resends run together):
  // neither replaces a request the other kept, and each keeps the ones only it sent. Each write is
  // a file of its own, made only when it is not there, so there is no read, merge and write to
  // race.
  test("two processes keeping requests under the same keys at once lose none", async () => {
    const dir = sessionDirectory({ DISPATCH_STATE_DIR: root() }, "s12");
    const keys = Array.from({ length: 300 }, () => crypto.randomUUID());
    const module = join(import.meta.dir, "..", "dispatch-session-state.ts");
    // Both start writing at once: each says it is ready, then waits for the go file the test
    // makes once both have said so.
    const go = join(dir, "..", "go");
    mkdirSync(join(dir, ".."), { recursive: true });
    const writer = (body: string, extra: number) =>
      Bun.spawn(
        [
          process.execPath,
          "-e",
          `import { existsSync } from "node:fs";
import { writeResendRecord } from ${JSON.stringify(module)};
console.log("ready");
while (!existsSync(${JSON.stringify(go)})) {}
for (const key of ${JSON.stringify(keys)}) {
  writeResendRecord(${JSON.stringify(dir)}, key, [
    { key, method: "POST", path: "/m", type: "application/json", content: ${JSON.stringify(body)} },
    { key: key + ".${extra}", method: "POST", path: "/m", type: "application/json", content: "x" },
  ]);
}`,
        ],
        { stdout: "pipe", stderr: "pipe" }
      );
    const children = [writer("from A", 2), writer("from B", 3)];
    for (const child of children) await child.stdout.getReader().read();
    writeFileSync(go, "");
    expect(await Promise.all(children.map((child) => child.exited))).toEqual([0, 0]);
    const lost = keys.filter((key) => readResendRecord(dir, key).length !== 3);
    expect(lost).toEqual([]);
    for (const key of keys) {
      expect(["from A", "from B"]).toContain(readResendRecord(dir, key)[0]?.content as string);
    }
  }, 60_000);

  test("pruning removes sessions idle for 14 days and keeps the rest", () => {
    const r = root();
    const old = join(r, "sessions", "old");
    const fresh = join(r, "sessions", "fresh");
    mkdirSync(old, { recursive: true });
    mkdirSync(fresh, { recursive: true });
    const now = Date.now();
    const fifteenDays = new Date(now - 15 * 86_400_000);
    utimesSync(old, fifteenDays, fifteenDays);
    pruneSessions(r, now);
    expect(existsSync(old)).toBe(false);
    expect(existsSync(fresh)).toBe(true);
  });

  // A crashed process's marker store is inert, since the CLI reads only a live owner's store. The
  // daily pass removes it once its process is gone and nothing touched it for MARKER_MAX_AGE_MS.
  test("pruning removes a marker store whose process is gone, and keeps a live or recent one", () => {
    const r = root();
    const now = Date.now();
    const old = new Date(now - MARKER_MAX_AGE_MS - 60_000);
    const store = (dir: string, touched: Date) => {
      mkdirSync(join(dir, "markers"), { recursive: true });
      writeFileSync(join(dir, "markers", "marker"), "{}");
      utimesSync(join(dir, "markers"), touched, touched);
      utimesSync(dir, touched, touched);
      return dir;
    };
    const ns = pidNamespace();
    const live = store(askgateStore(r, process.pid, processStart(process.pid), ns), old);
    const crashed = store(askgateStore(r, process.pid, "1", ns), old);
    const recent = store(askgateStore(r, process.pid, "2", ns), new Date(now));
    const unknown = store(join(r, "askgate", "notes"), old);
    pruneSessions(r, now);
    expect(existsSync(crashed)).toBe(false);
    expect([live, recent, unknown].map((dir) => existsSync(dir))).toEqual([true, true, true]);
  });

  // A store named before the owner's pid namespace was kept (`<pid>-<start>`) may be a gone
  // process's, or a live process's in another namespace, which an agent box started on an earlier
  // pi-envoy: that one touches its store only when it mints. The pass cannot tell them apart, so it
  // keeps such a store as long as it keeps an idle session directory.
  test("pruning keeps a store named with no pid namespace for 14 days, as it may be another namespace's", () => {
    const r = root();
    const now = Date.now();
    const store = (dir: string, touched: Date) => {
      mkdirSync(join(dir, "markers"), { recursive: true });
      utimesSync(join(dir, "markers"), touched, touched);
      utimesSync(dir, touched, touched);
      return dir;
    };
    const idle = store(askgateStore(r, process.pid, "1"), new Date(now - 3 * 3_600_000));
    const ended = store(askgateStore(r, process.pid, "2"), new Date(now - 15 * 86_400_000));
    pruneSessions(r, now);
    expect(existsSync(idle)).toBe(true);
    expect(existsSync(ended)).toBe(false);
  });

  test("a malformed ledger line is named once, with its file and line, and the reader reads on", () => {
    const dir = sessionDirectory({ DISPATCH_STATE_DIR: root() }, "s5");
    appendResult(dir, { tool: "dispatch_search", details: {} });
    appendFileSync(join(dir, "results.jsonl"), '{"tool":"dispatch_ask","det\n');
    appendResult(dir, { tool: "dispatch_ask", details: { ask: "a" } });
    const problems: string[] = [];

    const read = readResultsSince(dir, 0, (problem) => problems.push(problem));

    expect(read.entries.map((entry) => entry.tool)).toEqual(["dispatch_search", "dispatch_ask"]);
    expect(read.offset).toBe(resultsEnd(dir));
    expect(problems).toHaveLength(1);
    expect(problems[0]).toContain(`${join(dir, "results.jsonl")}:2`);
    // The line is behind the returned offset, so the next read neither returns nor names it again.
    expect(readResultsSince(dir, read.offset, (problem) => problems.push(problem)).entries).toEqual(
      []
    );
    expect(problems).toHaveLength(1);
  });

  test("a ledger line counts from the start of the file even when the read starts later", () => {
    const dir = sessionDirectory({ DISPATCH_STATE_DIR: root() }, "s6");
    appendResult(dir, { tool: "dispatch_search", details: {} });
    appendResult(dir, { tool: "dispatch_search", details: {} });
    const offset = resultsEnd(dir);
    appendFileSync(join(dir, "results.jsonl"), "garbage\n");
    const problems: string[] = [];
    readResultsSince(dir, offset, (problem) => problems.push(problem));
    expect(problems[0]).toContain(`${join(dir, "results.jsonl")}:3`);
  });

  test("a corrupted state.json throws naming the file", () => {
    const dir = sessionDirectory({ DISPATCH_STATE_DIR: root() }, "s7");
    mkdirSync(dir, { recursive: true });
    writeFileSync(join(dir, "state.json"), "{not json");
    expect(() => loadSessionMemory(dir, "s7")).toThrow(join(dir, "state.json"));
  });

  test("a missing title, state and ledger read as empty without creating anything", () => {
    const dir = sessionDirectory({ DISPATCH_STATE_DIR: root() }, "s8");
    expect(readSessionTitle(dir)).toBeUndefined();
    expect(loadSessionMemory(dir, "s8").size).toBe(0);
    expect(resultsEnd(dir)).toBe(0);
    expect(readResultsSince(dir, 0, () => undefined)).toEqual({ entries: [], offset: 0 });
    expect(existsSync(dir)).toBe(false);
  });

  test("one outcome writes one ledger line and the memory together", () => {
    const dir = sessionDirectory({ DISPATCH_STATE_DIR: root() }, "s9");
    recordOutcome(dir, "s9", new Set(["ask-9"]), {
      tool: "dispatch_ask",
      details: { ask: "ask-9" },
    });
    expect(readResultsSince(dir, 0, () => undefined).entries.map((entry) => entry.tool)).toEqual([
      "dispatch_ask",
    ]);
    expect(loadSessionMemory(dir, "s9").has("ask-9")).toBe(true);
  });

  test("pruning runs at most once a day", () => {
    const r = root();
    const now = Date.now();
    pruneSessions(r, now);
    const old = join(r, "sessions", "old");
    mkdirSync(old, { recursive: true });
    const fifteenDays = new Date(now - 15 * 86_400_000);
    utimesSync(old, fifteenDays, fifteenDays);
    pruneSessions(r, now + 3_600_000);
    expect(existsSync(old)).toBe(true);
    pruneSessions(r, now + 2 * 86_400_000);
    expect(existsSync(old)).toBe(false);
  });
});
