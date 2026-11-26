import { expect, test } from "bun:test";
import {
  chmodSync,
  existsSync,
  mkdirSync,
  readdirSync,
  readFileSync,
  writeFileSync,
} from "node:fs";
import path from "node:path";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import { type OutcomeRecord, parseSkillUseFile } from "./heldout-case";
import {
  type Answer,
  answerOf,
  type Batch,
  buildBatch,
  type CaseRubric,
  F4_ANSWER,
  F4_COMMAND,
  INSTRUCTION,
  labelsOf,
  MAX_TOKENS,
  MODEL,
  main,
  parseCases,
  parseRubric,
  parseSkillUseRubric,
  type RunRecord,
  readRuns,
  requestOf,
  sha256,
  skillUseBatch,
} from "./heldout-reader";

const scratch = temporaryDirectory("heldout-reader", { real: true });

function dir(name: string): string {
  const made = path.join(scratch, name);
  mkdirSync(made, { recursive: true });
  return made;
}

const EXAMPLE = path.join(import.meta.dir, "testdata/heldout-case-example.json");
const SKILL_USE_EXAMPLE = path.join(import.meta.dir, "testdata/skilluse-case-example.json");
const cases = parseCases(readFileSync(EXAMPLE, "utf8"));
const [P05, P10] = cases.cases;
if (P05 === undefined || P10 === undefined) throw new Error("the example holds P05 and P10");
const P05_BAD = P05.traces.bad;
const P10_BAD = P10.traces.bad;
if (P05_BAD === undefined || P10_BAD === undefined) throw new Error("the examples hold bad traces");

/** The frozen rubric of the example's cases, in the shape of a case author's rubric file: the
 * minimal part the reader reads, and keys it does not. */
const RUBRIC_FILE = JSON.stringify({
  version: 1,
  set: "heldout",
  frozen: "2030-01-01T00:00:00Z",
  reader: { model: MODEL, calls: [] },
  replaced: [],
  sent_back: [],
  cases: cases.cases.map((each) => ({
    id: each.id,
    scoreable: true,
    rubric: each.rubric,
    verdicts: { good: "pass", bad: "fail" },
    reason: "Written by hand.",
  })),
});
const RUBRIC: CaseRubric[] = parseRubric(RUBRIC_FILE);

/** `n` UUIDs that no example trace holds. */
const RUN_IDS = [1, 2, 3, 4].map((n) => `00000000-0000-4000-8000-00000000000${n}`);

/** A run of `caseId` that did what `trace` did, under the run id `id`. */
function runOf(caseId: string, trace: RunRecord["outcome"], id: string): RunRecord {
  return { case: caseId, outcome: { ...trace, id }, effects: null };
}

/** Two runs of each example case: one that did what the good trace did, one the bad trace. */
const RUNS: RunRecord[] = [
  runOf(P05.id, P05.traces.good, RUN_IDS[0] ?? ""),
  runOf(P05.id, P05_BAD, RUN_IDS[1] ?? ""),
  runOf(P10.id, P10.traces.good, RUN_IDS[2] ?? ""),
  runOf(P10.id, P10_BAD, RUN_IDS[3] ?? ""),
];

/** The ids of the batch's items in the order they are sent. */
const order = (batch: Batch) => batch.items.map((item) => item.id);

/** The user message of a request. */
function userMessage(request: string): string {
  const body = JSON.parse(request) as { messages: { role: string; content: string }[] };
  return body.messages[0]?.content ?? "";
}

/** Every object key of a JSON value, at any depth. */
function keysOf(value: unknown): string[] {
  if (Array.isArray(value)) return value.flatMap(keysOf);
  if (typeof value !== "object" || value === null) return [];
  return Object.entries(value).flatMap(([key, member]) => [key, ...keysOf(member)]);
}

/** The items a request's user message holds, parsed. */
function itemsOf(request: string): { id: string; case: string; outcome: { lastText: string } }[] {
  const match = /<runs>\n([\s\S]*)\n<\/runs>/.exec(userMessage(request));
  return JSON.parse(match?.[1] ?? "null");
}

/** Whether a batch's item passes a rubric line. */
type Passes = (item: Batch["items"][number], line: string) => boolean;

/** What the reader grades: each item of the batch, each line of its case, passed or failed by
 * `passes`. */
function reply(batch: Batch, passes: Passes, reason = "Decided by the actions."): string {
  const lines = new Map(
    batch.rubric.map((each) => [each.id, each.rubric.map((line) => line.name)])
  );
  return JSON.stringify({
    runs: batch.items.map((item) => ({
      id: item.id,
      lines: (lines.get(item.case) ?? []).map((name) => ({
        name,
        pass: passes(item, name),
        reason,
      })),
    })),
  });
}

function answerWith(text: string): Answer {
  return {
    id: "msg_test",
    model: MODEL,
    usage: { input_tokens: 10, output_tokens: 5 },
    text,
  };
}

const REQUEST = { sha256: "0".repeat(64), seed: 7 };

/** A good trace passes every line and a bad one fails every line. */
const GOOD_LASTS = new Set([P05.traces.good.lastText, P10.traces.good.lastText]);
const byLastText = (item: Batch["items"][number]) => GOOD_LASTS.has(item.outcome.lastText);

test("the frozen rubric is read as its minimal shape, and every other key is left unread", () => {
  expect(RUBRIC.map((each) => each.id)).toEqual(["P05", "P10"]);
  expect(RUBRIC[1]?.rubric.map((line) => line.name)).toEqual(["diagnosis", "scheduling"]);
  expect(Object.keys(RUBRIC[0] ?? {})).toEqual(["id", "rubric"]);

  const minimal = parseRubric(
    JSON.stringify({ cases: [{ id: "X1", rubric: [{ name: "a", text: "A passes." }] }] })
  );
  expect(minimal).toEqual([{ id: "X1", rubric: [{ name: "a", text: "A passes." }] }]);

  const refused = (value: unknown) => () => parseRubric(JSON.stringify(value));
  expect(refused({ cases: [{ id: "X1", rubric: [] }] })).toThrow();
  expect(refused({ cases: [{ id: "X1" }] })).toThrow();
  expect(refused({})).toThrow();
  const line = { name: "a", text: "A passes." };
  expect(
    refused({
      cases: [
        { id: "X1", rubric: [line] },
        { id: "X1", rubric: [line] },
      ],
    })
  ).toThrow(/repeat/);
  expect(refused({ cases: [{ id: "X1", rubric: [line, line] }] })).toThrow(/repeat/);
});

test("a skilluse file gives no control and is refused; a case file's own traces are read", () => {
  expect(() => parseCases(readFileSync(SKILL_USE_EXAMPLE, "utf8"))).toThrow(/no traces/);
  expect(cases.cases.map((each) => [each.id, each.traces.bad !== undefined])).toEqual([
    ["P05", true],
    ["P10", true],
  ]);
});

test("a batch holds each run under its own id and each trace as a control under a fresh id", () => {
  const batch = buildBatch(RUBRIC, cases, RUNS, 7);
  expect(batch.items).toHaveLength(8);
  expect(batch.controls.size).toBe(4);
  expect([...batch.controls.values()].toSorted()).toEqual(["bad", "bad", "good", "good"]);
  for (const run of RUNS) expect(order(batch)).toContain(run.outcome.id);

  // A control's id is neither its trace's own id nor its case's, and each is a UUID of version 4.
  const own = new Set([
    ...cases.cases.flatMap((each) => [each.id, each.traces.good.id, each.traces.bad?.id]),
  ]);
  for (const id of batch.controls.keys()) {
    expect(own.has(id)).toBe(false);
    expect(id).toMatch(/^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/);
  }
  // A control is an item like a run: the same keys, its outcome without its id.
  for (const item of batch.items) {
    expect(Object.keys(item)).toEqual(["id", "case", "outcome", "effects"]);
    expect(Object.keys(item.outcome).toSorted()).toEqual([
      "actions",
      "dispatchFirstInjected",
      "lastText",
      "skillReads",
    ]);
  }
  // Only the cases the runs name have their controls in the batch.
  const only = buildBatch(RUBRIC, cases, RUNS.slice(0, 2), 7);
  expect(only.controls.size).toBe(2);
  expect(only.rubric.map((each) => each.id)).toEqual(["P05"]);
});

test("the shuffle is the same for the same seed, whatever order the runs arrive in, and not for another", () => {
  const first = order(buildBatch(RUBRIC, cases, RUNS, 7));
  expect(order(buildBatch(RUBRIC, cases, RUNS, 7))).toEqual(first);
  expect(order(buildBatch(RUBRIC, cases, RUNS.toReversed(), 7))).toEqual(first);
  const other = order(buildBatch(RUBRIC, cases, RUNS, 8));
  expect(other).not.toEqual(first);
  // The controls' fresh ids follow the seed too.
  const controlIds = (seed: number) => [...buildBatch(RUBRIC, cases, RUNS, seed).controls.keys()];
  expect(controlIds(7).toSorted()).toEqual(controlIds(7).toSorted());
  expect(controlIds(7).toSorted()).not.toEqual(controlIds(8).toSorted());
  // The order is a shuffle of the items and not their arrival order, for some seed of the first ten.
  const arrival = (seed: number) => {
    const items = order(buildBatch(RUBRIC, cases, RUNS, seed));
    return [...items].sort().join() === items.join();
  };
  expect([...Array(10).keys()].some((seed) => !arrival(seed))).toBe(true);
  // The same request, byte for byte.
  expect(requestOf(buildBatch(RUBRIC, cases, RUNS, 7))).toBe(
    requestOf(buildBatch(RUBRIC, cases, RUNS.toReversed(), 7))
  );
});

test("the request is one stateless message: no tool, no history, no sampling, and no arm, label, route or prompt", () => {
  const runs = RUNS.map((run, index) => ({
    ...run,
    effects:
      index === 2
        ? {
            id: run.outcome.id,
            answers: [],
            phaseBefore: "done" as const,
            phaseAfter: "done" as const,
            productionCheck: { wakeAt: "2030-01-08T12:30:00Z", checkWakeRow: null },
            handoffRecords: [],
            diverged: [],
          }
        : null,
  }));
  const batch = buildBatch(RUBRIC, cases, runs, 7);
  const request = requestOf(batch);
  const body = JSON.parse(request);

  expect(Object.keys(body).toSorted()).toEqual(["max_tokens", "messages", "model", "system"]);
  expect(body.model).toBe("claude-opus-5-5");
  expect(body.max_tokens).toBe(8000);
  expect(MAX_TOKENS).toBe(8000);
  expect(body.system).toBe(INSTRUCTION);
  expect(body.messages).toHaveLength(1);
  expect(body.messages[0].role).toBe("user");

  // The user message holds the rubric and the items, and nothing else.
  const message = userMessage(request);
  expect(message.startsWith("<rubric>\n")).toBe(true);
  const items = itemsOf(request);
  expect(items).toHaveLength(8);
  const rubric = /<rubric>\n([\s\S]*?)\n<\/rubric>/.exec(message)?.[1] ?? "null";
  expect(JSON.parse(rubric)).toEqual(
    batch.rubric.map((each) => ({ case: each.id, lines: each.rubric }))
  );

  // No field named arm, label, route, prompt or token, in the message or in the request as a
  // whole, and none of the words as a key in the instruction's JSON.
  const forbidden = ["arm", "label", "labels", "route", "prompt", "token", "tokens", "usage"];
  expect(keysOf(JSON.parse(rubric)).filter((key) => forbidden.includes(key))).toEqual([]);
  expect(keysOf(items).filter((key) => forbidden.includes(key.toLowerCase()))).toEqual([]);
  expect(request).not.toMatch(/"(arm|label|labels|route|prompt|tokens?)"\s*:/i);
  // No item holds a run's id twice or a case's directory, and an item's effects lost their id.
  const withEffects = items.find((item) => "effects" in item && item.effects !== null) as
    | { effects: Record<string, unknown> }
    | undefined;
  expect(withEffects).toBeDefined();
  expect(Object.keys(withEffects?.effects ?? {})).not.toContain("id");
});

test("a control graded wrong makes the batch INCONCLUSIVE, and none graded wrong makes it CONCLUSIVE", () => {
  const batch = buildBatch(RUBRIC, cases, RUNS, 7);
  const control = (trace: "good" | "bad", caseId: string) => {
    const found = [...batch.controls].find(
      ([id, kind]) => kind === trace && batch.items.find((item) => item.id === id)?.case === caseId
    );
    if (found === undefined) throw new Error(`no ${trace} control of ${caseId}`);
    return found[0];
  };
  const grade = (passes: Passes) => labelsOf(batch, answerWith(reply(batch, passes)), REQUEST);

  // Every good trace passes and every bad trace fails: the reader is right on each control.
  const right = grade((item) => byLastText(item));
  expect(right.batch).toBe("CONCLUSIVE");
  expect(right.controls).toHaveLength(4);
  expect(right.controls.every((each) => each.graded === "right")).toBe(true);
  expect(right.records).toHaveLength(4);
  expect(right.records.map((each) => each.pass)).toHaveLength(4);

  // A bad trace that fails one line only is graded right: it did not pass.
  const partial = grade((item, line) => byLastText(item) || line === "diagnosis");
  expect(partial.batch).toBe("CONCLUSIVE");

  // A good trace failed.
  const goodId = control("good", "P05");
  const goodFailed = grade((item) => item.id !== goodId && byLastText(item));
  expect(goodFailed.batch).toBe("INCONCLUSIVE");
  expect(
    goodFailed.controls.filter((each) => each.graded === "wrong").map((each) => each.id)
  ).toEqual([goodId]);
  // A good trace that fails one line only is failed.
  const goodPartly = grade(
    (item, line) => byLastText(item) && !(item.id === goodId && line === "publish-first")
  );
  expect(goodPartly.batch).toBe("INCONCLUSIVE");

  // A bad trace passed.
  const badId = control("bad", "P10");
  const badPassed = grade((item) => item.id === badId || byLastText(item));
  expect(badPassed.batch).toBe("INCONCLUSIVE");
  expect(
    badPassed.controls.filter((each) => each.graded === "wrong").map((each) => each.id)
  ).toEqual([badId]);

  // A run graded wrong is no control, so it never makes a batch INCONCLUSIVE.
  const runFailed = grade((item) => byLastText(item) && item.id !== RUN_IDS[0]);
  expect(runFailed.batch).toBe("CONCLUSIVE");
  expect(runFailed.records.find((each) => each.id === RUN_IDS[0])?.pass).toBe(false);
});

test("the labels name each line's pass and its reason, in the rubric's order, for runs and controls", () => {
  const batch = buildBatch(RUBRIC, cases, RUNS, 7);
  const labels = labelsOf(
    batch,
    answerWith(reply(batch, (item) => byLastText(item), "  Seq 2 decided it.  ")),
    REQUEST
  );
  expect(labels.version).toBe(1);
  expect(labels.model).toBe(MODEL);
  expect(labels.response_id).toBe("msg_test");
  expect(labels.request_sha256).toBe(REQUEST.sha256);
  expect(labels.seed).toBe(7);
  const p10 = labels.records.find((each) => each.id === RUN_IDS[2]);
  expect(p10?.case).toBe("P10");
  expect(p10?.lines).toEqual([
    { name: "diagnosis", pass: true, reason: "Seq 2 decided it." },
    { name: "scheduling", pass: true, reason: "Seq 2 decided it." },
  ]);
  expect(p10?.pass).toBe(true);
  expect(labels.records.find((each) => each.id === RUN_IDS[3])?.pass).toBe(false);
  // A control carries its trace, and no run carries one. Neither knows an arm.
  expect(labels.controls.map((each) => each.trace).toSorted()).toEqual([
    "bad",
    "bad",
    "good",
    "good",
  ]);
  expect(Object.keys(labels.records[0] ?? {})).toEqual(["id", "case", "pass", "lines"]);
  expect(JSON.stringify(labels)).not.toMatch(/"arm"/);
});

test("a reply is read as a bare object, a fenced block, or an object with words around it", () => {
  const batch = buildBatch(RUBRIC, cases, RUNS, 7);
  const text = reply(batch, (item) => byLastText(item));
  const bare = labelsOf(batch, answerWith(text), REQUEST);
  expect(labelsOf(batch, answerWith(`\`\`\`json\n${text}\n\`\`\``), REQUEST)).toEqual(bare);
  expect(labelsOf(batch, answerWith(`Here are the grades.\n${text}\nDone.`), REQUEST)).toEqual(
    bare
  );
  // A key the instruction did not ask for is left unread.
  const extra = JSON.parse(text);
  extra.note = "extra";
  extra.runs[0].confidence = 0.9;
  expect(labelsOf(batch, answerWith(JSON.stringify(extra)), REQUEST)).toEqual(bare);
});

test("a reply that misses, repeats or invents an item or a line fails the run and names each", () => {
  const batch = buildBatch(RUBRIC, cases, RUNS, 7);
  const whole = JSON.parse(reply(batch, (item) => byLastText(item)));
  const refused = (change: (value: typeof whole) => void) => {
    const value = structuredClone(whole);
    change(value);
    return () => labelsOf(batch, answerWith(JSON.stringify(value)), REQUEST);
  };
  const firstId = whole.runs[0].id;

  expect(refused((value) => value.runs.shift())).toThrow(`the reply does not grade ${firstId}`);
  expect(refused((value) => value.runs.push(structuredClone(value.runs[0])))).toThrow(
    `the reply grades ${firstId} twice`
  );
  expect(refused((value) => (value.runs[0].id = "not-in-the-batch"))).toThrow(
    "the reply grades not-in-the-batch, which the batch does not hold"
  );
  const lineOf = whole.runs[0].lines[0].name;
  expect(refused((value) => value.runs[0].lines.shift())).toThrow(
    `the reply does not grade ${firstId} on ${lineOf}`
  );
  expect(
    refused((value) => value.runs[0].lines.push(structuredClone(value.runs[0].lines[0])))
  ).toThrow(`the reply grades ${firstId} on ${lineOf} twice`);
  expect(refused((value) => (value.runs[0].lines[0].name = "invented"))).toThrow(
    `the reply grades ${firstId} on invented`
  );
  expect(refused((value) => delete value.runs[0].lines[0].reason)).toThrow(
    /not in the asked shape/
  );
  expect(refused((value) => (value.runs[0].lines[0].pass = "yes"))).toThrow(
    /not in the asked shape/
  );
  expect(() => labelsOf(batch, answerWith("I cannot grade these."), REQUEST)).toThrow(
    /holds no JSON object/
  );
  // Every problem is named at once.
  expect(
    refused((value) => {
      value.runs.shift();
      value.runs[0].lines.shift();
    })
  ).toThrow(/does not grade .*; the reply does not grade /);
});

test("a Messages API answer is read for its text, model and usage, and a cut or foreign one is refused", () => {
  const message = {
    id: "msg_01",
    type: "message",
    role: "assistant",
    model: "claude-opus-5-5-20261101",
    content: [
      { type: "text", text: '{"runs":' },
      { type: "text", text: " []}" },
    ],
    stop_reason: "end_turn",
    usage: { input_tokens: 1234, output_tokens: 56, service_tier: "standard" },
  };
  expect(answerOf(JSON.stringify(message))).toEqual({
    id: "msg_01",
    model: "claude-opus-5-5-20261101",
    usage: message.usage,
    text: '{"runs": []}',
  });
  expect(() => answerOf(JSON.stringify({ ...message, stop_reason: "max_tokens" }))).toThrow(
    /cut at max_tokens 8000/
  );
  expect(() => answerOf(JSON.stringify({ type: "error", error: { message: "x" } }))).toThrow(
    /no Messages API message/
  );
  expect(() => answerOf("overloaded")).toThrow(/no Messages API message/);
});

// The end to end run, with the model faked: records on disk, one call, the labels, their hash and
// the record of the call.

/** The fake keys the key file and the key command hold, which the call's header must carry. */
const FILE_KEY = "model-key";
const COMMAND_KEY = "gateway-key";

function key(): string {
  const file = path.join(dir("keys"), "anthropic-key");
  writeFileSync(file, `${FILE_KEY}\n`);
  chmodSync(file, 0o600);
  return file;
}

function io(fetch: (input: string, init: RequestInit) => Promise<Response>) {
  const stdout: string[] = [];
  const stderr: string[] = [];
  return {
    stdout,
    stderr,
    io: {
      env: { HOME: scratch },
      cwd: scratch,
      fetch,
      sleep: async () => {},
      out: (line: string) => stdout.push(line),
      err: (line: string) => stderr.push(line),
    },
  };
}

/** A 200 answer of the faked model whose text is `text`. */
function modelAnswer(text: string): Response {
  return new Response(
    JSON.stringify({
      id: "msg_fake",
      type: "message",
      role: "assistant",
      model: "claude-opus-5-5-fake",
      content: [{ type: "text", text }],
      stop_reason: "end_turn",
      usage: { input_tokens: 4000, output_tokens: 300 },
    }),
    { status: 200, headers: { "content-type": "application/json" } }
  );
}

/** The faked model: it grades each item by its last words, as `byLastText` does. */
function fakeModel(bodies: string[]) {
  return async (_url: string, init: RequestInit): Promise<Response> => {
    const request = String(init.body);
    bodies.push(request);
    const items = itemsOf(request);
    const rubric = new Map(
      cases.cases.map((each) => [each.id, each.rubric.map((line) => line.name)])
    );
    const text = JSON.stringify({
      runs: items.map((item) => ({
        id: item.id,
        lines: (rubric.get(item.case) ?? []).map((name) => ({
          name,
          pass: GOOD_LASTS.has(item.outcome.lastText),
          reason: "Decided by the last action.",
        })),
      })),
    });
    return modelAnswer(text);
  };
}

/** A --records directory of the example's runs, and the rubric file to read with it; `cases`, the
 * case file, where it is not the example. */
interface World {
  readonly root: string;
  readonly records: string;
  readonly rubric: string;
  readonly out: string;
  readonly cases?: string;
}

function world(name: string, withEffects = true): World {
  const root = dir(name);
  const records = path.join(root, "records");
  RUNS.forEach((run, index) => {
    const at = path.join(records, run.case, `run-${index}`);
    mkdirSync(at, { recursive: true });
    writeFileSync(path.join(at, "outcome.json"), JSON.stringify(run.outcome));
    if (withEffects && index === 2) {
      writeFileSync(
        path.join(at, "effects.json"),
        JSON.stringify({
          id: run.outcome.id,
          answers: [
            {
              seq: 0,
              at: "2030-01-08T12:00:05Z",
              by: "daemon",
              op: "handoff_complete",
              status: 200,
              code: null,
              body: {},
            },
          ],
          phaseBefore: "done",
          phaseAfter: "done",
          productionCheck: {
            wakeAt: "2030-01-08T12:30:00Z",
            checkWakeRow: { dueAt: "2030-01-08T12:30:00Z" },
          },
          handoffRecords: [],
          diverged: [],
        })
      );
    }
  });
  const rubric = path.join(root, "rubric.json");
  writeFileSync(rubric, RUBRIC_FILE);
  return { root, records, rubric, out: path.join(root, "out", "labels.json") };
}

const flags = (w: World, ...more: string[]) => [
  "--rubric",
  w.rubric,
  "--cases",
  w.cases ?? EXAMPLE,
  "--records",
  w.records,
  "--out",
  w.out,
  "--seed",
  "7",
  ...more,
];

test("a run grades the records in one call, writes the labels and prints their sha256 and the call's record", async () => {
  const w = world("run");
  const bodies: string[] = [];
  const sent: { url: string; init: RequestInit }[] = [];
  const model = fakeModel(bodies);
  const run = io(async (url, init) => {
    sent.push({ url, init });
    return model(url, init);
  });
  const code = await main(flags(w, "--key-file", key()), run.io);
  expect(run.stderr).toEqual([]);
  expect(code).toBe(0);

  // One call, to the model's own endpoint, with the key in x-api-key.
  expect(sent).toHaveLength(1);
  expect(sent[0]?.url).toBe("https://api.anthropic.com/v1/messages");
  expect(sent[0]?.init.headers).toMatchObject({ "x-api-key": FILE_KEY });
  const items = itemsOf(bodies[0] ?? "");
  expect(items).toHaveLength(8);
  // The reader is given each run under its outcome's id, and no directory name.
  expect(JSON.stringify(items)).not.toContain("run-0");
  expect(bodies[0]).not.toContain(w.root);
  const withEffects = items.find((item) => item.id === RUN_IDS[2]) as unknown as {
    effects: { productionCheck: { wakeAt: string } };
  };
  expect(withEffects.effects.productionCheck.wakeAt).toBe("2030-01-08T12:30:00Z");

  // The labels file, and the printed hash of its bytes.
  const text = readFileSync(w.out, "utf8");
  const labels = JSON.parse(text);
  expect(labels.batch).toBe("CONCLUSIVE");
  expect(labels.records.map((each: { id: string }) => each.id).toSorted()).toEqual(RUN_IDS);
  expect(labels.controls).toHaveLength(4);
  const labelsSha = sha256(text);
  expect(run.stdout).toContain(`labels sha256:${labelsSha}`);
  expect(run.stdout).toContain(`request sha256:${sha256(bodies[0] ?? "")}`);
  expect(run.stdout).toContain("batch: CONCLUSIVE (4 of 4 controls graded right)");
  expect(run.stdout).toContain('usage: {"input_tokens":4000,"output_tokens":300}');

  // <out>.run.json: the call's record, with no arm.
  const log = JSON.parse(readFileSync(`${w.out}.run.json`, "utf8"));
  expect(log).toHaveLength(1);
  expect(Object.keys(log[0]).toSorted()).toEqual([
    "at",
    "labels_sha256",
    "model",
    "request_sha256",
    "usage",
  ]);
  expect(log[0].labels_sha256).toBe(labelsSha);
  expect(log[0].request_sha256).toBe(sha256(bodies[0] ?? ""));
  expect(log[0].model).toBe("claude-opus-5-5-fake");
  expect(log[0].usage).toEqual({ input_tokens: 4000, output_tokens: 300 });
  expect(Number.isNaN(Date.parse(log[0].at))).toBe(false);
  // <out>.attempts.json: the attempt, on record before the call, under the call's own time.
  expect(JSON.parse(readFileSync(`${w.out}.attempts.json`, "utf8"))).toEqual([
    { at: log[0].at, seed: 7, request_sha256: log[0].request_sha256 },
  ]);
  expect(readdirSync(path.dirname(w.out)).toSorted()).toEqual([
    "labels.json",
    "labels.json.attempts.json",
    "labels.json.run.json",
  ]);

  // A second run over the same records appends its own record and keeps the first.
  const again = io(fakeModel([]));
  expect(await main(flags(w, "--key-file", key()), again.io)).toBe(0);
  const twice = JSON.parse(readFileSync(`${w.out}.run.json`, "utf8"));
  expect(twice).toHaveLength(2);
  expect(twice[0]).toEqual(log[0]);
  expect(twice[1].request_sha256).toBe(log[0].request_sha256);
  expect(JSON.parse(readFileSync(`${w.out}.attempts.json`, "utf8"))).toHaveLength(2);
});

test("the gateway route posts to <gateway>/v1/messages with the key the key command prints", async () => {
  const w = world("gateway");
  const command = path.join(dir("gateway-key"), "key-command");
  writeFileSync(command, `#!/bin/sh\necho ${COMMAND_KEY}\n`);
  chmodSync(command, 0o700);
  const sent: { url: string; init: RequestInit }[] = [];
  const model = fakeModel([]);
  const run = io(async (url, init) => {
    sent.push({ url, init });
    return model(url, init);
  });
  const code = await main(
    flags(w, "--gateway", "http://gateway.test/anthropic/", "--key-command", command),
    run.io
  );
  expect(run.stderr).toEqual([]);
  expect(code).toBe(0);
  expect(sent).toHaveLength(1);
  expect(sent[0]?.url).toBe("http://gateway.test/anthropic/v1/messages");
  expect(sent[0]?.init.headers).toMatchObject({ "x-api-key": COMMAND_KEY });
});

test("a 429 is asked again with the same request, and a reply it cannot read is kept and recorded nowhere as labels", async () => {
  const w = world("retry");
  const bodies: string[] = [];
  const model = fakeModel(bodies);
  let calls = 0;
  const run = io(async (url, init) => {
    calls += 1;
    if (calls === 1) return new Response("slow down", { status: 429 });
    return model(url, init);
  });
  expect(await main(flags(w, "--key-file", key()), run.io)).toBe(0);
  expect(calls).toBe(2);

  const broken = world("broken");
  const unreadable = io(
    async () =>
      new Response(
        JSON.stringify({
          id: "msg_x",
          type: "message",
          model: "claude-opus-5-5-fake",
          content: [{ type: "text", text: "I could not grade these." }],
          stop_reason: "end_turn",
          usage: { input_tokens: 1, output_tokens: 1 },
        }),
        { status: 200 }
      )
  );
  expect(await main(flags(broken, "--key-file", key()), unreadable.io)).toBe(1);
  expect(unreadable.stderr).toHaveLength(1);
  expect(unreadable.stderr[0]).toContain("holds no JSON object");
  expect(unreadable.stderr[0]).toContain(`${broken.out}.reply.txt`);
  expect(readdirSync(path.dirname(broken.out)).toSorted()).toEqual([
    "labels.json.attempts.json",
    "labels.json.reply.txt",
  ]);
});

test("a run whose effects diverged from the daemon, or are another run's, is refused before any call", async () => {
  const sent: string[] = [];
  const quiet = io(async (url) => {
    sent.push(url);
    return new Response("", { status: 500 });
  });
  const effectsOf = (w: World) => path.join(w.records, "P10", "run-2", "effects.json");

  const diverged = world("diverged");
  const record = JSON.parse(readFileSync(effectsOf(diverged), "utf8"));
  record.diverged = [
    { seq: 0, standin: { status: 409, code: "X" }, daemon: { status: 200, code: null } },
  ];
  writeFileSync(effectsOf(diverged), JSON.stringify(record));
  expect(await main(flags(diverged, "--key-file", key()), quiet.io)).toBe(2);
  expect(quiet.stderr[0]).toContain("DIVERGED");
  expect(quiet.stderr[0]).toContain("a harness fault and no result; replace it before its batch");

  const foreign = world("foreign");
  const other = JSON.parse(readFileSync(effectsOf(foreign), "utf8"));
  other.id = "00000000-0000-4000-8000-0000000000ff";
  writeFileSync(effectsOf(foreign), JSON.stringify(other));
  expect(await main(flags(foreign, "--key-file", key()), quiet.io)).toBe(2);
  expect(quiet.stderr[1]).toContain("is the record of 00000000-0000-4000-8000-0000000000ff");
  expect(sent).toEqual([]);
});

test("a bad argument, a missing rubric case, a skilluse file or a bad seed stops the run before any call", async () => {
  const w = world("refused");
  const sent: string[] = [];
  const run = io(async (url) => {
    sent.push(url);
    return new Response("", { status: 500 });
  });
  const refuses = async (argv: string[], message: string) => {
    expect(await main(argv, run.io)).toBe(2);
    expect(run.stderr.at(-1)).toContain(message);
  };
  await refuses(flags(w), "name one model route");
  await refuses(flags(w, "--key-file", key(), "--seed", "7"), "--seed is given twice");
  await refuses(
    flags({ ...w, rubric: w.rubric }, "--key-file", key()).map((arg) => (arg === "7" ? "-1" : arg)),
    "--seed -1 is not a whole number below 2^32"
  );
  await refuses(
    flags(w, "--key-file", key()).map((arg) => (arg === EXAMPLE ? SKILL_USE_EXAMPLE : arg)),
    "gives the batch no control"
  );
  await refuses(
    ["--cases", EXAMPLE, "--seed", "7", "--key-file", key()],
    "--rubric, --records and --out"
  );
  await refuses(["--seed", "7"], "--cases and --seed are required");
  await refuses(
    flags(w, "--capture", w.root, "--key-file", key()),
    "--capture takes no --rubric, --records or --out"
  );

  const partial = path.join(w.root, "partial-rubric.json");
  writeFileSync(partial, JSON.stringify({ cases: [{ id: "P05", rubric: RUBRIC[0]?.rubric }] }));
  await refuses(
    flags(w, "--key-file", key()).map((arg) => (arg === w.rubric ? partial : arg)),
    "the frozen rubric has no case P10"
  );
  expect(sent).toEqual([]);
});

// A skilluse set: the public example's need case P09 and a near miss made from it. A skilluse file
// holds no traces, so its frozen rubric, in the case author's shape, holds each case's lines loaded
// and finished and the good and bad control traces that check the reader. A run with the skill
// withheld is a run of --records like any other, under a directory that names its arm.

const skillUseExample = parseSkillUseFile(readFileSync(SKILL_USE_EXAMPLE, "utf8"));
const P09 = skillUseExample.cases[0];
if (P09 === undefined) throw new Error("the skilluse example holds P09");
const { withheldControl: _withheldControl, ...p09Needless } = P09;
/** A near miss: a run leaves the skill unread and still finishes. */
const P09N = {
  ...p09Needless,
  id: "P09N",
  kind: "near-miss" as const,
  rubric: {
    loaded: "The run leaves skill://legion-retro unread: the task does not need it.",
    finished: P09.rubric.finished,
  },
};
const SKILL_USE_CASES = JSON.stringify({ ...skillUseExample, cases: [P09, P09N] });

/** UUID number `n`, which no example trace holds. */
const uuid = (n: number) => `00000000-0000-4000-8000-${String(n).padStart(12, "0")}`;

/** A skilluse outcome under `id` whose last words are `lastText`: with a read of legion-retro
 * before its first action that resolved or not, or with no read where `resolved` is undefined. */
function skillOutcome(id: string, lastText: string, resolved?: boolean): OutcomeRecord {
  const bytes = resolved ? 4096 : 0;
  const read = { skill: "legion-retro", path: "SKILL.md", bytes, beforeFirstAction: true };
  return {
    id,
    actions: [],
    skillReads: resolved === undefined ? [] : [{ ...read, resolved }],
    dispatchFirstInjected: true,
    lastText,
  };
}

/** The fake reader's rule: a line passes where the item's last words name it. */
const byWords: Passes = (item, line) => item.outcome.lastText.includes(line);

const linesOf = (each: { rubric: { loaded: string; finished: string } }) => [
  { name: "loaded", text: each.rubric.loaded },
  { name: "finished", text: each.rubric.finished },
];
/** The rubric's own reason for its verdicts, which no request may hold. */
const VERDICT_REASON = "The author's reader passed the good trace and failed the bad one.";
/** The frozen rubric of the skilluse set, in the case author's shape. */
const SKILL_USE_RUBRIC = {
  version: 1,
  set: "skilluse",
  frozen: true,
  reader: { model: MODEL, calls: [] },
  replaced: [],
  sent_back: [],
  cases: [
    {
      id: "P09",
      scoreable: true,
      rubric: linesOf(P09),
      verdicts: { good: "pass", bad: "fail", withheld: "fail" },
      reason: VERDICT_REASON,
      controlTraces: {
        good: skillOutcome(uuid(21), "loaded finished (P09 good)", true),
        bad: skillOutcome(uuid(22), "loaded (P09 bad)", true),
        withheld: skillOutcome(uuid(23), "withheld trace (P09)", false),
      },
    },
    {
      id: "P09N",
      scoreable: true,
      rubric: linesOf(P09N),
      verdicts: { good: "pass", bad: "fail" },
      reason: VERDICT_REASON,
      controlTraces: {
        good: skillOutcome(uuid(24), "loaded finished (P09N good)"),
        bad: skillOutcome(uuid(25), "finished (P09N bad)", true),
      },
    },
  ],
};
const SKILL_USE_RUBRIC_FILE = JSON.stringify(SKILL_USE_RUBRIC);

/** The skilled run of P09, which passes both lines. */
const SKILLED = skillOutcome(uuid(11), "loaded finished (run 11)", true);
/** The skilluse records: each run's case, the directory that names its arm, and its outcome. No
 * arm may reach the reader. */
const SKILL_RUNS: readonly (readonly [string, string, OutcomeRecord])[] = [
  ["P09", "arm-skilled", SKILLED],
  ["P09", "arm-withheld", skillOutcome(uuid(12), "The read failed (run 12)", false)],
  ["P09N", "arm-candidate", skillOutcome(uuid(13), "loaded finished (run 13)")],
];

/** A --records directory of SKILL_RUNS, with the skilluse case file and `rubric` beside it. */
function skillWorld(name: string, rubric = SKILL_USE_RUBRIC_FILE): World {
  const root = dir(name);
  const records = path.join(root, "records");
  for (const [caseId, arm, outcome] of SKILL_RUNS) {
    mkdirSync(path.join(records, caseId, arm), { recursive: true });
    writeFileSync(path.join(records, caseId, arm, "outcome.json"), JSON.stringify(outcome));
  }
  const cases = path.join(root, "skilluse-cases.json");
  writeFileSync(cases, SKILL_USE_CASES);
  writeFileSync(path.join(root, "rubric.json"), rubric);
  return {
    root,
    records,
    rubric: path.join(root, "rubric.json"),
    out: path.join(root, "out", "labels.json"),
    cases,
  };
}

/** The faked reader of a skilluse batch: it grades each line as `byWords` does, and fails every
 * line of an item whose last words hold `wrong`. */
function fakeSkillModel(bodies: string[], wrong?: string) {
  return async (_url: string, init: RequestInit): Promise<Response> => {
    const request = String(init.body);
    bodies.push(request);
    const runs = itemsOf(request).map((item) => {
      const words = item.outcome.lastText;
      const wrongly = wrong !== undefined && words.includes(wrong);
      return {
        id: item.id,
        lines: ["loaded", "finished"].map((name) => ({
          name,
          pass: words.includes(name) && !wrongly,
          reason: "Decided by the last words.",
        })),
      };
    });
    return modelAnswer(JSON.stringify({ runs }));
  };
}

test("a skilluse rubric gives each case its lines loaded and finished and its good and bad control traces", () => {
  const frozen = parseSkillUseRubric(SKILL_USE_RUBRIC_FILE);
  expect(frozen.map((each) => each.id)).toEqual(["P09", "P09N"]);
  for (const each of frozen) {
    expect(Object.keys(each)).toEqual(["id", "rubric", "traces"]);
    expect(each.rubric.map((line) => line.name)).toEqual(["loaded", "finished"]);
  }
  // The rubric's own good and bad traces; its withheld trace is left unread.
  expect(frozen[0]?.traces).toEqual({
    good: SKILL_USE_RUBRIC.cases[0]?.controlTraces.good,
    bad: SKILL_USE_RUBRIC.cases[0]?.controlTraces.bad,
  });
});

test("a skilluse batch holds its run and its case's two frozen controls, and its request no verdict, world, control kind or arm", () => {
  const batch = skillUseBatch(
    parseSkillUseRubric(SKILL_USE_RUBRIC_FILE),
    parseSkillUseFile(SKILL_USE_CASES),
    [{ case: "P09", outcome: SKILLED, effects: null }],
    7
  );
  expect(batch.items).toHaveLength(3);
  expect([...batch.controls.values()].toSorted()).toEqual(["bad", "good"]);
  expect(batch.rubric).toEqual([{ id: "P09", rubric: linesOf(P09) }]);

  // The request of any batch: the reader's model, settings and instruction, and the two lines.
  const request = requestOf(batch);
  const body = JSON.parse(request);
  expect(Object.keys(body).toSorted()).toEqual(["max_tokens", "messages", "model", "system"]);
  expect([body.model, body.max_tokens, body.system]).toEqual([MODEL, MAX_TOKENS, INSTRUCTION]);
  const rubricPart = /<rubric>\n([\s\S]*?)\n<\/rubric>/.exec(userMessage(request))?.[1];
  const rubric = JSON.parse(rubricPart ?? "");
  expect(rubric).toEqual([{ case: "P09", lines: linesOf(P09) }]);
  // No key or text of the rubric's verdicts, of the case's world, task or withheld control, or of
  // a control's kind or own id: the reader cannot tell a control from a run.
  const hidden = [
    "arm",
    "trace",
    "kind",
    "verdicts",
    "scoreable",
    "reason",
    "controlTraces",
    "withheld",
    "withheldControl",
    "world",
    "task",
    "source",
    "good",
    "bad",
  ];
  const keys = [...keysOf(rubric), ...keysOf(itemsOf(request))];
  expect(keys.filter((key) => hidden.includes(key))).toEqual([]);
  for (const text of [VERDICT_REASON, P09.task, "withheld", uuid(21), uuid(22), uuid(23)]) {
    expect(request).not.toContain(text);
  }

  // Each line is graded by itself. A frozen control graded wrong, or a grade the reply leaves out,
  // gives no CONCLUSIVE labels.
  const grade = (passes: Passes) => labelsOf(batch, answerWith(reply(batch, passes)), REQUEST);
  const right = grade(byWords);
  expect(right.batch).toBe("CONCLUSIVE");
  const reason = "Decided by the actions.";
  expect(right.records).toEqual([
    {
      id: SKILLED.id,
      case: "P09",
      pass: true,
      lines: [
        { name: "loaded", pass: true, reason },
        { name: "finished", pass: true, reason },
      ],
    },
  ]);
  const graded = right.controls.map((each) => [each.trace, ...each.lines.map((line) => line.pass)]);
  expect(graded.toSorted()).toEqual([
    ["bad", true, false],
    ["good", true, true],
  ]);
  const controlId = new Map([...batch.controls].map(([id, trace]) => [trace, id]));
  const goodId = controlId.get("good");
  const goodFailed = grade(
    (item, line) => byWords(item, line) && (item.id !== goodId || line === "loaded")
  );
  expect(goodFailed.batch).toBe("INCONCLUSIVE");
  const badPassed = grade((item, line) => item.id === controlId.get("bad") || byWords(item, line));
  expect(badPassed.batch).toBe("INCONCLUSIVE");
  const missing = JSON.parse(reply(batch, byWords));
  missing.runs.find((run: { id: string }) => run.id === SKILLED.id).lines.shift();
  expect(() => labelsOf(batch, answerWith(JSON.stringify(missing)), REQUEST)).toThrow(
    `the reply does not grade ${SKILLED.id} on loaded`
  );
});

test("a skilluse run grades need and near-miss records in one blind call, checked on each case's frozen controls", async () => {
  const w = skillWorld("skilluse-run");
  const bodies: string[] = [];
  const run = io(fakeSkillModel(bodies));
  expect(await main(flags(w, "--key-file", key()), run.io)).toBe(0);
  expect(run.stderr).toEqual([]);
  // The one request is the batch of the case file, the rubric and the records under the seed, and
  // it holds no arm, no key and nothing of the rubric but its lines.
  const frozen = parseSkillUseRubric(SKILL_USE_RUBRIC_FILE);
  const file = parseSkillUseFile(SKILL_USE_CASES);
  expect(bodies).toEqual([requestOf(skillUseBatch(frozen, file, readRuns(w.records), 7))]);
  expect(itemsOf(bodies[0] ?? "")).toHaveLength(7);
  for (const hidden of ["arm-", "skilled", "withheld", FILE_KEY, w.root, VERDICT_REASON]) {
    expect(bodies[0]).not.toContain(hidden);
  }

  // Each record keeps both its judgments, and every frozen control was graded right.
  const text = readFileSync(w.out, "utf8");
  const labels = JSON.parse(text);
  expect(labels.batch).toBe("CONCLUSIVE");
  expect(labels.controls).toHaveLength(4);
  type Label = { id: string; case: string; lines: { name: string; pass: boolean }[] };
  const rows = labels.records.map((each: Label) => [
    each.id,
    [each.case, ...each.lines.map((line) => `${line.name} ${line.pass}`)],
  ]);
  expect(Object.fromEntries(rows)).toEqual({
    [uuid(11)]: ["P09", "loaded true", "finished true"],
    [uuid(12)]: ["P09", "loaded false", "finished false"],
    [uuid(13)]: ["P09N", "loaded true", "finished true"],
  });
  expect(run.stdout).toContain("batch: CONCLUSIVE (4 of 4 controls graded right)");
  // The labels know no arm: the join to arms comes after them.
  expect(text).not.toMatch(/arm-|skilled|withheld/);

  // A frozen control graded wrong leaves the batch INCONCLUSIVE, so none of its records can pass.
  const wrong = skillWorld("skilluse-wrong-control");
  const flipped = io(fakeSkillModel([], "(P09 good)"));
  expect(await main(flags(wrong, "--key-file", key()), flipped.io)).toBe(0);
  expect(flipped.stdout).toContain("batch: INCONCLUSIVE (3 of 4 controls graded right)");
  expect(JSON.parse(readFileSync(wrong.out, "utf8")).batch).toBe("INCONCLUSIVE");
});

test("a skilluse run whose rubric lacks a control, a case or a line, or holds a bad one, stops before any call", async () => {
  const sent: string[] = [];
  const run = io(async (url) => {
    sent.push(url);
    return new Response("", { status: 500 });
  });
  const refuses = async (w: World, message: string) => {
    expect(await main(flags(w, "--key-file", key()), run.io)).toBe(2);
    expect(run.stderr.at(-1)).toContain("refused before any model call; nothing was sent");
    expect(run.stderr.at(-1)).toContain(message);
    // A refusal is no call: no attempt is on record.
    expect(existsSync(`${w.out}.attempts.json`)).toBe(false);
  };
  const base = JSON.parse(SKILL_USE_RUBRIC_FILE);
  const changed = (name: string, change: (rubric: typeof base) => unknown) => {
    const rubric = structuredClone(base);
    change(rubric);
    return skillWorld(name, JSON.stringify(rubric));
  };
  const good = base.cases[0].controlTraces.good;
  await refuses(
    changed("no-bad-control", (r) => Object.assign(r.cases[0], { controlTraces: { good } })),
    "cases[0].controlTraces.bad"
  );
  await refuses(
    changed("bad-control", (r) => Object.assign(r.cases[1].controlTraces.good, { id: "P09N" })),
    "cases[1].controlTraces.good.id"
  );
  await refuses(
    changed("no-rubric-case", (r) => r.cases.pop()),
    "the frozen rubric has no case P09N"
  );
  await refuses(
    changed("no-finished-line", (r) => r.cases[0].rubric.pop()),
    "no line finished"
  );
  await refuses(
    changed("loaded-twice", (r) => r.cases[1].rubric.push(r.cases[1].rubric[0])),
    "cases[1].rubric[2].name"
  );
  await refuses(
    changed("other-line", (r) => Object.assign(r.cases[0].rubric[1], { name: "diagnosis" })),
    "not a skilluse line"
  );
  await refuses(
    changed("heldout-set", (r) => Object.assign(r, { set: "heldout" })),
    'its set is "heldout", not skilluse'
  );

  // A run of a case that --cases does not hold.
  const stray = skillWorld("skilluse-stray-case");
  mkdirSync(path.join(stray.records, "P99", "arm-x"), { recursive: true });
  writeFileSync(
    path.join(stray.records, "P99", "arm-x", "outcome.json"),
    JSON.stringify(skillOutcome(uuid(14), "loaded finished"))
  );
  await refuses(stray, "no case of --cases is P99");

  // --capture grades a case file's own traces, and a skilluse file holds none.
  const capture = ["--capture", dir("skilluse-capture"), "--cases", SKILL_USE_EXAMPLE];
  expect(await main([...capture, "--seed", "7", "--key-file", key()], run.io)).toBe(2);
  expect(run.stderr.at(-1)).toContain(
    "a skilluse file holds no traces, so it gives the batch no control"
  );
  expect(sent).toEqual([]);
});

test("a call that gives no labels has its attempt on record before the send, keeps its reply apart and writes no labels", async () => {
  const w = skillWorld("skilluse-no-labels");
  const attemptsFile = `${w.out}.attempts.json`;
  const sent: string[] = [];
  const onRecord: unknown[] = [];
  // The first answer refuses to grade; the repeat's answer misses each item's line finished.
  const model = async (_url: string, init: RequestInit) => {
    onRecord.push(JSON.parse(readFileSync(attemptsFile, "utf8")));
    const request = String(init.body);
    sent.push(request);
    if (sent.length === 1) return modelAnswer("I will not grade these runs.");
    const runs = itemsOf(request).map((item) => ({
      id: item.id,
      lines: [{ name: "loaded", pass: true, reason: "Read first." }],
    }));
    return modelAnswer(JSON.stringify({ runs }));
  };
  const first = io(model);
  expect(await main(flags(w, "--key-file", key()), first.io)).toBe(1);
  // At the send the attempt was on record: its time, seed and request sha256, and no grade and no
  // byte of the request.
  const attempt = { at: expect.any(String), seed: 7, request_sha256: sha256(sent[0] ?? "") };
  expect(onRecord).toEqual([[attempt]]);
  expect(first.stderr).toHaveLength(1);
  expect(first.stderr[0]).toContain("no labels were written");
  expect(first.stderr[0]).toContain("holds no JSON object");
  expect(first.stderr[0]).not.toContain("nothing was sent");
  // The reply is kept apart, and there are no labels and no record of any.
  expect(readdirSync(path.dirname(w.out)).toSorted()).toEqual([
    "labels.json.attempts.json",
    "labels.json.reply.txt",
  ]);
  expect(readFileSync(`${w.out}.reply.txt`, "utf8")).toContain("I will not grade these runs.");

  // The one repeat, at the same seed on the same records, sends the same request. An answer that
  // misses a grade gives no labels either, and the first attempt stays on record.
  const second = io(model);
  expect(await main(flags(w, "--key-file", key()), second.io)).toBe(1);
  expect(sent[1]).toBe(sent[0]);
  expect(second.stderr[0]).toMatch(/the reply does not grade \S+ on finished/);
  const attempts = JSON.parse(readFileSync(attemptsFile, "utf8"));
  expect(onRecord[1]).toEqual(attempts);
  expect(attempts).toEqual([attempt, attempt]);
  expect(attempts[0]).toEqual((onRecord[0] as unknown[])[0]);
  expect(readFileSync(attemptsFile, "utf8")).not.toContain(INSTRUCTION.slice(0, 60));
  expect(readFileSync(`${w.out}.reply.txt`, "utf8")).toContain("Read first.");
  expect(existsSync(w.out)).toBe(false);
  expect(existsSync(`${w.out}.run.json`)).toBe(false);
});

// Fixture F4: the raw answer of the one real stateless call that `--capture` made on T0's example,
// the example cases' good and bad traces as the only items (its seed is in the .command file). It
// was read from the gateway as `claude-opus-5-5` answered it, thinking block and all.

const F4 = readFileSync(path.join(import.meta.dir, "testdata", F4_ANSWER), "utf8");
const F4_COMMAND_TEXT = readFileSync(path.join(import.meta.dir, "testdata", F4_COMMAND), "utf8");
const F4_SEED = Number(/--seed (\d+)/.exec(F4_COMMAND_TEXT)?.[1]);
const F4_REQUEST_SHA = /sha256 is ([0-9a-f]{64})/.exec(F4_COMMAND_TEXT)?.[1];

/** The batch F4 answered: the example's own rubric lines and traces, under the captured seed. */
const F4_BATCH = buildBatch(
  cases.cases.map(({ id, rubric }) => ({ id, rubric })),
  cases,
  [],
  F4_SEED
);

test("fixture F4's command names its seed and the request's sha256, and no gateway and no key", () => {
  expect(Number.isInteger(F4_SEED)).toBe(true);
  expect(F4_COMMAND_TEXT).toContain("--capture");
  expect(F4_COMMAND_TEXT).toContain("--key-command <a program that prints the gateway key>");
  expect(F4_COMMAND_TEXT).not.toMatch(/https?:\/\//);
  expect(F4).not.toMatch(/x-api-key/i);
  // The request this code builds is the request the fixture answered: a change to the instruction
  // or to the batch's shape means F4 is captured again.
  expect(sha256(requestOf(F4_BATCH))).toBe(F4_REQUEST_SHA ?? "");
});

test("fixture F4 parses: the answer's text, model and usage, and the labels of its four traces", () => {
  const raw = JSON.parse(F4);
  expect(raw.type).toBe("message");
  expect(raw.stop_reason).toBe("end_turn");
  // The model answered with a thinking block before its text: only the text is the reply.
  expect(raw.content.map((block: { type: string }) => block.type)).toEqual(["thinking", "text"]);

  const answer = answerOf(F4);
  expect(answer.model).toBe("claude-opus-5-5");
  expect(answer.id).toBe(raw.id);
  expect(answer.usage).toEqual(raw.usage);
  expect(answer.usage).toMatchObject({ input_tokens: 3861, output_tokens: 947 });

  const labels = labelsOf(F4_BATCH, answer, { sha256: F4_REQUEST_SHA ?? "", seed: F4_SEED });
  expect(labels.records).toEqual([]);
  expect(labels.controls).toHaveLength(4);
  expect(labels.batch).toBe("CONCLUSIVE");
  expect(labels.controls.every((control) => control.graded === "right")).toBe(true);
  // Each trace is graded on each line of its own case, in the rubric's order.
  const linesOf = (caseId: string) =>
    (cases.cases.find((each) => each.id === caseId)?.rubric ?? []).map((line) => line.name);
  for (const control of labels.controls) {
    expect(control.lines.map((line) => line.name)).toEqual(linesOf(control.case));
    for (const line of control.lines) expect(line.reason.length).toBeGreaterThan(0);
  }
  const byTrace = (trace: string) => labels.controls.filter((control) => control.trace === trace);
  expect(byTrace("good").every((control) => control.pass)).toBe(true);
  expect(byTrace("bad").every((control) => !control.pass)).toBe(true);
});

test("--capture makes one call on the case file's traces and writes the raw answer and its command", async () => {
  const out = dir("capture");
  const command = path.join(dir("capture-key"), "key-command");
  writeFileSync(command, "#!/bin/sh\necho gateway-key\n");
  chmodSync(command, 0o700);
  const sent: { url: string; init: RequestInit }[] = [];
  const run = io(async (url, init) => {
    sent.push({ url, init });
    return new Response(F4, { status: 200, headers: { "content-type": "application/json" } });
  });
  const code = await main(
    [
      "--capture",
      out,
      "--cases",
      EXAMPLE,
      "--seed",
      String(F4_SEED),
      "--gateway",
      "https://gateway.test/",
      "--key-command",
      command,
    ],
    run.io
  );
  expect(run.stderr).toEqual([]);
  expect(code).toBe(0);
  expect(sent).toHaveLength(1);
  expect(sent[0]?.url).toBe("https://gateway.test/v1/messages");
  // The request is the one F4 answered, and its answer is kept byte for byte.
  expect(sha256(String(sent[0]?.init.body))).toBe(F4_REQUEST_SHA ?? "");
  expect(readFileSync(path.join(out, F4_ANSWER), "utf8")).toBe(F4);
  const written = readFileSync(path.join(out, F4_COMMAND), "utf8");
  expect(written).toContain(`the request's sha256 is ${F4_REQUEST_SHA}`);
  expect(written).toContain(`--seed ${F4_SEED} --gateway <model gateway URL>`);
  expect(written).not.toContain("gateway.test");
  expect(written).not.toContain(command);
  expect(readdirSync(out).toSorted()).toEqual([F4_COMMAND, F4_ANSWER].toSorted());
  expect(run.stdout).toEqual([
    `F4: ${path.join(out, F4_ANSWER)} sha256:${sha256(F4)}`,
    `request sha256:${F4_REQUEST_SHA}`,
    "batch: CONCLUSIVE (4 of 4 controls graded right)",
    `usage: ${JSON.stringify(answerOf(F4).usage)}`,
  ]);
});
