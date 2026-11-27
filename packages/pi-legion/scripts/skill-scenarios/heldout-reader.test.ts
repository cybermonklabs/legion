import { expect, test } from "bun:test";
import {
  chmodSync,
  cpSync,
  existsSync,
  mkdirSync,
  readdirSync,
  readFileSync,
  rmSync,
  writeFileSync,
} from "node:fs";
import path from "node:path";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import { type OutcomeRecord, parseOutcome, parseSkillUseFile } from "./heldout-case";
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
  OBSERVED_INSTRUCTION,
  parseCases,
  parseRubric,
  parseSkillUseRubric,
  type RunRecord,
  readControls,
  readRuns,
  requestOf,
  sha256,
  skillUseBatch,
} from "./heldout-reader";
import {
  type Disposition,
  gradedOf,
  parseEffects,
  parseReplay,
  replayOf,
} from "./heldout-reader-receipt";

const scratch = temporaryDirectory("heldout-reader", { real: true });

function dir(name: string): string {
  const made = path.join(scratch, name);
  mkdirSync(made, { recursive: true });
  return made;
}

const EXAMPLE = path.join(import.meta.dir, "testdata/heldout-case-example.json");
const SKILL_USE_EXAMPLE = path.join(import.meta.dir, "testdata/skilluse-case-example.json");
/** Lane D's records of the example's controls, as teardown-effects.sh --controls wrote them in the
 * proof Job (its .command beside it says how): <CONTROLS>/<case id>/<good or bad>/. */
const CONTROLS = path.join(
  import.meta.dir,
  "../../../daemon/internal/daemon/testdata/teardown-effects/controls"
);
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
const RUBRIC: CaseRubric[] = parseRubric(RUBRIC_FILE, cases);

/** `n` UUIDs that no example trace holds. */
const RUN_IDS = [1, 2, 3, 4].map((n) => `00000000-0000-4000-8000-00000000000${n}`);

/** A run of `caseId` that did what `trace` did, under the run id `id`. */
function runOf(caseId: string, trace: RunRecord["outcome"], id: string): RunRecord {
  return { case: caseId, outcome: { ...trace, id }, replay: null };
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
const byLastText = (item: Batch["items"][number]) => GOOD_LASTS.has(item.outcome.lastText ?? "");

test("the frozen rubric is read as its minimal shape, and every other key is left unread", () => {
  expect(RUBRIC.map((each) => each.id)).toEqual(["P05", "P10"]);
  expect(RUBRIC[1]?.rubric.map((line) => line.name)).toEqual(["diagnosis", "scheduling"]);
  expect(Object.keys(RUBRIC[0] ?? {})).toEqual(["id", "rubric"]);

  const minimal = parseRubric(
    JSON.stringify({ cases: [{ id: "X1", rubric: [{ name: "a", text: "A passes." }] }] }),
    cases
  );
  expect(minimal).toEqual([{ id: "X1", rubric: [{ name: "a", text: "A passes." }] }]);

  const refused = (value: unknown) => () => parseRubric(JSON.stringify(value), cases);
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

test("a frozen rubric that lacks a line heldout-case.ts requires of its case is refused, naming each", () => {
  // A production-check case is graded on diagnosis and scheduling (heldout-case.ts CHECK_RUBRIC),
  // the line that reads the effect record's next look.
  const lacking = (drop: (name: string) => boolean) => {
    const frozen = JSON.parse(RUBRIC_FILE);
    for (const each of frozen.cases)
      each.rubric = each.rubric.filter((line: { name: string }) => !drop(line.name));
    return () => parseRubric(JSON.stringify(frozen), cases);
  };
  expect(lacking((name) => name === "scheduling")).toThrow("P10 scheduling");
  const other = JSON.parse(RUBRIC_FILE);
  other.cases[1].rubric = [{ name: "order", text: "The look reports before it ends its turn." }];
  expect(() => parseRubric(JSON.stringify(other), cases)).toThrow("P10 diagnosis, P10 scheduling");
  // heldout-case.ts requires no line of an implementer case, so the rubric may grade it on others.
  const renamed = JSON.parse(RUBRIC_FILE);
  renamed.cases[0].rubric = [{ name: "order", text: "The run publishes before it hands off." }];
  expect(parseRubric(JSON.stringify(renamed), cases)[0]?.rubric.map((line) => line.name)).toEqual([
    "order",
  ]);
  // A case the rubric does not hold is refused by the batch that grades it, not here.
  const partial = JSON.parse(RUBRIC_FILE);
  partial.cases.pop();
  expect(parseRubric(JSON.stringify(partial), cases).map((each) => each.id)).toEqual(["P05"]);
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
    replay:
      index === 2
        ? {
            verdict: "ok" as const,
            effects: {
              id: run.outcome.id,
              answers: [],
              phaseBefore: "done" as const,
              phaseAfter: "done" as const,
              productionCheck: { wakeAt: "2030-01-08T12:30:00Z", checkWakeRow: null },
              handoffRecords: [],
              diverged: [],
            },
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
  expect(labels.version).toBe(2);
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
  // Null is the grade of a line only a later action could show, on a record that stops short.
  expect(refused((value) => (value.runs[0].lines[0].pass = null))).toThrow(
    `the reply leaves ${firstId} on ${lineOf} null, and its record is whole`
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
 * case file, where it is not the example; `controls`, lane D's records of the controls, for a batch
 * of cases lane D replays. */
interface World {
  readonly root: string;
  readonly records: string;
  readonly rubric: string;
  readonly out: string;
  readonly cases?: string;
  readonly controls?: string;
}

// Lane D's receipts as the Go driver writes them (teardown_effects_record_test.go): replay.json for
// every run it replayed, and effects.json beside it unless the replay is HARNESS.

/** The route of every daemon action of the example's traces. */
const COMPLETE = "POST /legion/v1/handoff/complete";
/** The state a replayed call ran in, which the reader checks no part of. */
const STATE = {
  phase: "done",
  tree: "ACME-21",
  treeRuns: true,
  parent: "",
  standing: "",
  branch: "",
  pulls: [],
};

/** An answer's status and refusal code. */
interface Said {
  readonly status: number;
  readonly code: string | null;
}

/** A run's replay.json, and its effects.json (null for none). */
interface Receipts {
  readonly replay: Record<string, unknown>;
  readonly effects: Record<string, unknown> | null;
}

/** What the legion tool answered a request the daemon answered `said`. */
function toolAnswer(said: Said) {
  return said.status < 400
    ? { isError: false, text: "{}" }
    : { isError: true, text: `${said.status} ${said.code}: refused` };
}

/**
 * The receipts of a replay of `outcome`'s run that made each of its daemon actions again as one
 * legion call, answered by the real daemon as the stand-in answered it unless `real` gives the real
 * daemon's answer to that action. The first call answered otherwise is the replay's last: the run
 * is DIVERGED, and no later call is replayed.
 */
function receiptsOf(
  outcome: OutcomeRecord,
  real: ReadonlyMap<number, Said> = new Map(),
  productionCheck: unknown = null
): Receipts {
  const calls: unknown[] = [];
  const answers: unknown[] = [];
  const diverged: unknown[] = [];
  const notReplayed: string[] = [];
  for (const action of outcome.actions) {
    if (action.kind !== "daemon") continue;
    const call = `toolu_${action.seq}`;
    if (diverged.length > 0) {
      notReplayed.push(call);
      continue;
    }
    const standin = { status: action.answer.code === null ? 200 : 400, code: action.answer.code };
    const daemon = real.get(action.seq) ?? standin;
    const seq = answers.length;
    const body = daemon.status < 400 ? {} : { code: daemon.code, error: "refused" };
    const { at, op } = action;
    answers.push({ seq, at, by: "daemon", op, ...daemon, body });
    answers.push({
      seq: seq + 1,
      at,
      by: "client",
      op,
      status: 0,
      code: null,
      body: toolAnswer(daemon),
    });
    calls.push({
      call,
      op,
      at,
      before: [],
      state: STATE,
      standin: [{ outcomeSeq: action.seq, route: COMPLETE, ...standin }],
      daemon: [{ seq, route: COMPLETE, ...daemon }],
      client: { run: toolAnswer(standin), replay: toolAnswer(daemon) },
      phaseAfter: "done",
    });
    if (daemon.status !== standin.status || daemon.code !== standin.code)
      diverged.push({ seq, route: COMPLETE, call, outcomeSeq: action.seq, standin, daemon });
  }
  const verdict = diverged.length > 0 ? "DIVERGED" : "ok";
  const replay = { id: outcome.id, verdict, seed: [], calls, after: [], notReplayed };
  const effects = {
    id: outcome.id,
    answers,
    phaseBefore: "done",
    phaseAfter: "done",
    productionCheck,
    handoffRecords: [],
    diverged,
  };
  return { replay, effects };
}

/** The reason the HARNESS receipts of these tests give, which no request may hold. */
const HARNESS_REASON = "the replay stopped before its first call";

/** The HARNESS receipt of `outcome`'s run, whose typed cause is `fault`, and no effects.json. */
function harnessReceipts(outcome: OutcomeRecord, fault: Record<string, unknown>): Receipts {
  const replay = {
    id: outcome.id,
    verdict: "HARNESS",
    reason: HARNESS_REASON,
    fault,
    seed: [],
    calls: [],
    after: [],
    notReplayed: [],
  };
  return { replay, effects: null };
}

/** Writes the run directory <records>/<caseId>/<name>: its outcome.json and its receipts. */
function writeRun(
  records: string,
  caseId: string,
  name: string,
  outcome: OutcomeRecord,
  receipts: Receipts | null
): string {
  const at = path.join(records, caseId, name);
  mkdirSync(at, { recursive: true });
  writeFileSync(path.join(at, "outcome.json"), JSON.stringify(outcome));
  if (receipts !== null)
    writeFileSync(path.join(at, "replay.json"), JSON.stringify(receipts.replay));
  if (receipts?.effects)
    writeFileSync(path.join(at, "effects.json"), JSON.stringify(receipts.effects));
  return at;
}

/** A --records directory of RUNS, each with the ok receipts of its replay, and the rubric file. */
function world(name: string): World {
  const root = dir(name);
  const records = path.join(root, "records");
  const due = "2030-01-08T12:30:00Z";
  const wake = { wakeAt: due, checkWakeRow: { dueAt: due } };
  RUNS.forEach((run, index) => {
    const receipts = receiptsOf(run.outcome, new Map(), index === 2 ? wake : null);
    writeRun(records, run.case, `run-${index}`, run.outcome, receipts);
  });
  const rubric = path.join(root, "rubric.json");
  writeFileSync(rubric, RUBRIC_FILE);
  return { root, records, rubric, out: path.join(root, "out", "labels.json"), controls: CONTROLS };
}

const flags = (w: World, ...more: string[]) => [
  "--rubric",
  w.rubric,
  "--cases",
  w.cases ?? EXAMPLE,
  "--records",
  w.records,
  ...(w.controls === undefined ? [] : ["--controls", w.controls]),
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
  // Each run's disposition by its own id: lane D replayed it, the reader graded its outcome and its
  // effects whole, and its grade is the reader's.
  type Row = { id: string; lane: string; replay: string; graded: string; status: string };
  expect(
    labels.runs.map((each: Row) => [each.id, each.lane, each.replay, each.graded, each.status])
  ).toEqual(
    RUN_IDS.map((id, index) => [id, "D", "ok", "outcome-and-effects", index % 2 ? "fail" : "pass"])
  );
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

// Lane D's receipts at the reader: what of a run lane D replayed the reader is given, and how its
// grade reads. The runs are the example's P05 and P10 traces under ids of their own, with the
// receipts receiptsOf and harnessReceipts write.

/** Run id `n` of the lane D tests, which no example trace holds. */
const laneDId = (n: number) => `00000000-0000-4000-8000-${String(n).padStart(12, "0")}`;
/** The role's session, whose legion calls a run holds. */
const ROLE_SESSION = "7d1e5a12-0c05-4a12-9b12-a5120c05a512";

/** A --records directory of `runs`, [case, directory, outcome, receipts] each, and the rubric. */
function laneDWorld(
  name: string,
  runs: readonly (readonly [string, string, OutcomeRecord, Receipts | null])[]
): World {
  const root = dir(name);
  const records = path.join(root, "records");
  for (const [caseId, run, outcome, receipts] of runs)
    writeRun(records, caseId, run, outcome, receipts);
  const rubric = path.join(root, "rubric.json");
  writeFileSync(rubric, RUBRIC_FILE);
  return { root, records, rubric, out: path.join(root, "out", "labels.json"), controls: CONTROLS };
}

/** The fault replay.json types the role's own Dispatch write with, as outcome.json's action `seq`. */
function roleWrite(outcome: OutcomeRecord, seq: number): Record<string, unknown> {
  return {
    kind: "role-write",
    source: "dispatch-readback.json event 9",
    event: "issue.updated",
    at: outcome.actions.find((action) => action.seq === seq)?.at,
    outcomeSeq: seq,
    author: { kind: "session", id: ROLE_SESSION },
    session: ROLE_SESSION,
    call: "toolu_5",
  };
}

test("a lane D run with no replay receipt, with an older capture alone or with another run's receipt is refused before any call", async () => {
  const sent: string[] = [];
  const quiet = io(async (url) => {
    sent.push(url);
    return new Response("", { status: 500 });
  });
  const refuses = async (w: World, message: string) => {
    expect(await main(flags(w, "--key-file", key()), quiet.io)).toBe(2);
    expect(quiet.stderr.at(-1)).toContain("refused before any model call; nothing was sent");
    expect(quiet.stderr.at(-1)).toContain(message);
    expect(existsSync(`${w.out}.attempts.json`)).toBe(false);
  };
  const good = { ...P05.traces.good, id: laneDId(1) };
  const ok = receiptsOf(good);
  const missing = "replay.json is missing: lane D replays every run of case P05";
  await refuses(laneDWorld("no-receipt", [["P05", "run-1", good, null]]), missing);
  // An effects.json from before lane D wrote replay.json is no receipt either.
  const older = laneDWorld("effects-alone", [["P05", "run-1", good, null]]);
  const at = path.join(older.records, "P05", "run-1");
  writeFileSync(path.join(at, "effects.json"), JSON.stringify(ok.effects));
  await refuses(older, "its effects.json alone is an older capture");
  const other = laneDId(99);
  const foreign = { ...ok, replay: { ...ok.replay, id: other } };
  await refuses(
    laneDWorld("foreign-receipt", [["P05", "run-1", good, foreign]]),
    `replay.json is the receipt of ${other}, not of ${good.id}`
  );
  const foreignEffects = { ...ok, effects: { ...ok.effects, id: other } };
  await refuses(
    laneDWorld("foreign-effects", [["P05", "run-1", good, foreignEffects]]),
    `effects.json is the record of ${other}, not of ${good.id}`
  );
  expect(sent).toEqual([]);
});

test("a replay receipt that is malformed, or at odds with itself or with the run's outcome, is refused", () => {
  const good = { ...P05.traces.good, id: laneDId(3) };
  const ok = receiptsOf(good);
  const effects: Record<string, unknown> = ok.effects ?? {};
  const answers = effects.answers as Record<string, unknown>[];
  const disagreeing = [{ ...answers[0], status: 409 }, ...answers.slice(1)];
  const difference = {
    seq: 0,
    route: COMPLETE,
    call: "toolu_5",
    outcomeSeq: 5,
    standin: { status: 200, code: null },
    daemon: { status: 409, code: "HANDOFF_NO_PULL_REQUEST" },
  };
  const harness = harnessReceipts(good, { kind: "harness-record" });
  const { fault: _fault, ...older } = harness.replay;
  const foreignAuthor = { ...roleWrite(good, 3), author: { kind: "session", id: "ses-other" } };
  interface Row {
    readonly name: string;
    readonly receipts: Receipts;
    readonly message: string;
  }
  const rows: Row[] = [
    {
      name: "a null list, which the Go driver never writes",
      receipts: { ...ok, replay: { ...ok.replay, calls: null } },
      message: "at calls",
    },
    {
      name: "an ok receipt that names a difference",
      receipts: { ...ok, effects: { ...effects, diverged: [difference] } },
      message: "replay.json is ok, and effects.json names a difference",
    },
    {
      name: "answers that are not the ones the calls name",
      receipts: { ...ok, effects: { ...effects, answers: disagreeing } },
      message: "effects.json and replay.json disagree",
    },
    {
      name: "a daemon write no replayed call sent",
      receipts: { replay: { ...ok.replay, calls: [] }, effects: { ...effects, answers: [] } },
      message: "replayed no call that sent outcome.json's daemon write 5",
    },
    {
      name: "a HARNESS receipt with no typed fault",
      receipts: { replay: older, effects: null },
      message: "names no reason or no typed fault",
    },
    {
      name: "a HARNESS receipt beside an effects.json",
      receipts: { ...harness, effects },
      message: "lane D writes for no HARNESS replay",
    },
    {
      name: "a role's write that outcome.json does not score as a Dispatch write",
      receipts: harnessReceipts(good, roleWrite(good, 5)),
      message: "outcome.json holds no Dispatch write as action 5",
    },
    {
      name: "a role's write by another session",
      receipts: harnessReceipts(good, foreignAuthor),
      message: "its author ses-other is not the role's session",
    },
    {
      name: "a first difference of two acceptances, which lane D records as no difference",
      receipts: receiptsOf(good, new Map([[5, { status: 201, code: null }]])),
      message: "two acceptances, which lane D records as the same answer",
    },
  ];
  rows.forEach((row, index) => {
    const w = laneDWorld(`receipt-${index}`, [["P05", "run-1", good, row.receipts]]);
    expect(() => readRuns(w.records, cases)).toThrow(row.message);
  });
});

/** An item of a lane D batch, as the fake reader reads it. */
interface LaneDItem {
  readonly id: string;
  readonly case: string;
  readonly outcome: {
    readonly lastText?: string;
    readonly actions: readonly {
      readonly seq: number;
      readonly kind: string;
      readonly args?: Record<string, unknown>;
      readonly answer?: { readonly code: string | null; readonly body: unknown };
    }[];
  };
  readonly effects: Record<string, unknown> | null;
  readonly observedThrough?: number;
}

/**
 * The fake reader's grade of a lane D item, by the rule the reader is given. P05's line passes by
 * the run's last words. P10's lines fail on a pass or a fail report, and pass on a pending report
 * (diagnosis) and on a report the daemon took with a wake_at (scheduling); a line the record shows
 * neither way fails a whole record, and is null on a record that stops short (`observedThrough`).
 */
function laneDLines(item: LaneDItem): { name: string; pass: boolean | null; reason: string }[] {
  const reason = "Decided by the actions.";
  if (item.case === "P05") {
    const pass = GOOD_LASTS.has(item.outcome.lastText ?? "");
    return [{ name: "publish-first", pass, reason }];
  }
  const reports = item.outcome.actions.filter((action) => action.kind === "daemon");
  const wrong = reports.some((a) => a.args?.verdict === "pass" || a.args?.verdict === "fail");
  const pending = reports.some((a) => a.args?.verdict === "pending");
  const woken = reports.some((a) => a.args?.wake_at !== undefined && a.answer?.code === null);
  const unshown = item.observedThrough === undefined ? false : null;
  const shows = (seen: boolean) => (wrong ? false : seen || unshown);
  return [
    { name: "diagnosis", pass: shows(pending), reason },
    { name: "scheduling", pass: shows(woken), reason },
  ];
}

/** The faked reader of a lane D batch: it grades each item as laneDLines does, and fails every
 * line of an item whose last words are `wrong`. */
function laneDModel(bodies: string[], wrong?: string) {
  return async (_url: string, init: RequestInit): Promise<Response> => {
    const request = String(init.body);
    bodies.push(request);
    const runs = (itemsOf(request) as unknown as LaneDItem[]).map((item) => {
      const flipped = wrong !== undefined && item.outcome.lastText === wrong;
      const lines = laneDLines(item).map((line) => (flipped ? { ...line, pass: false } : line));
      return { id: item.id, lines };
    });
    return modelAnswer(JSON.stringify({ runs }));
  };
}

/** P10's good trace with its action `seq` reporting `verdict`. */
function p10Reporting(seq: number, verdict: string): OutcomeRecord["actions"] {
  return P10.traces.good.actions.map((action) => {
    if (action.kind !== "daemon" || action.seq !== seq) return action;
    return { ...action, args: { ...action.args, verdict } };
  });
}

/** P10's good trace as the stand-in took its first report: no refusal, and no retry after it. */
function p10Took(): OutcomeRecord["actions"] {
  const actions: OutcomeRecord["actions"] = [];
  for (const action of P10.traces.good.actions) {
    if (action.seq >= 3) break;
    if (action.kind === "daemon") actions.push({ ...action, answer: { code: null, body: {} } });
    else actions.push(action);
  }
  return actions;
}

test("a DIVERGED run is graded through its first difference's call with the real answer, and a HARNESS run by its typed cause", async () => {
  const p10Of = (n: number, actions: OutcomeRecord["actions"]) => ({
    ...P10.traces.good,
    id: laneDId(n),
    actions,
  });
  // (b) The real daemon took the pending report without wake_at that the stand-in refused. The
  // run's retry after it is a fail report, which nothing grades: the record stops at that call.
  const acceptedClean = p10Of(11, p10Reporting(3, "fail"));
  // (b) again, the refused report itself a pass report, which the case forbids.
  const acceptedDirty = p10Of(12, p10Reporting(2, "pass"));
  // (a) The stand-in took the report without wake_at, and the real daemon refused it: with
  // CHECK_WAKE_REQUIRED, after which the case allows a corrected retry, and with a code it does not.
  const refusedAllowed = p10Of(13, p10Took());
  const refusedOther = p10Of(14, p10Took());
  // (a) again: a real answer of 500 is a refusal too, and the case allows no retry after it.
  const refusedFailing = p10Of(18, p10Took());
  // HARNESS by the role's own write, on a good and a bad record, and by the harness's own records.
  const roleGood = { ...P05.traces.good, id: laneDId(15) };
  const roleBad = { ...P05_BAD, id: laneDId(16) };
  const records = { ...P05.traces.good, id: laneDId(17) };
  const taken = new Map([[2, { status: 200, code: null }]]);
  const wakeRequired = new Map([[2, { status: 400, code: "CHECK_WAKE_REQUIRED" }]]);
  const stale = new Map([[2, { status: 409, code: "HANDOFF_STALE_ENTRY" }]]);
  const failing = new Map([[2, { status: 500, code: null }]]);
  const w = laneDWorld("diverged-and-harness", [
    ["P10", "b-clean", acceptedClean, receiptsOf(acceptedClean, taken)],
    ["P10", "b-dirty", acceptedDirty, receiptsOf(acceptedDirty, taken)],
    ["P10", "a-allowed", refusedAllowed, receiptsOf(refusedAllowed, wakeRequired)],
    ["P10", "a-other", refusedOther, receiptsOf(refusedOther, stale)],
    ["P10", "a-500", refusedFailing, receiptsOf(refusedFailing, failing)],
    ["P05", "role-good", roleGood, harnessReceipts(roleGood, roleWrite(roleGood, 3))],
    ["P05", "role-bad", roleBad, harnessReceipts(roleBad, roleWrite(roleBad, 1))],
    ["P05", "records", records, harnessReceipts(records, { kind: "harness-record" })],
  ]);
  const bodies: string[] = [];
  const run = io(laneDModel(bodies));
  expect(await main(flags(w, "--key-file", key()), run.io)).toBe(0);
  expect(run.stderr).toEqual([]);

  // A batch that holds a record that stops short adds the observed instruction to the reader's.
  expect(JSON.parse(bodies[0] ?? "").system).toBe(`${INSTRUCTION}\n\n${OBSERVED_INSTRUCTION}`);
  // The reader is given every run but the one the harness's own records made HARNESS, and the
  // four controls of P05 and P10.
  const items = itemsOf(bodies[0] ?? "") as unknown as LaneDItem[];
  const ids = items.map((item) => item.id);
  expect(ids).toHaveLength(11);
  expect(ids).not.toContain(records.id);
  // A DIVERGED run: its record through the call of its first difference, seq 2, nothing later,
  // and the real daemon's answer joined to the original action by that seq.
  const observed = items.filter((item) => item.observedThrough !== undefined);
  expect(observed.map((item) => item.id).toSorted()).toEqual([11, 12, 13, 14, 18].map(laneDId));
  for (const item of observed) {
    expect(Object.keys(item)).toEqual(["id", "case", "outcome", "effects", "observedThrough"]);
    expect(item.observedThrough).toBe(2);
    expect(Object.keys(item.outcome)).toEqual(["actions", "skillReads", "dispatchFirstInjected"]);
    expect(item.outcome.actions.map((action) => action.seq)).toEqual([1, 2]);
    expect(Object.keys(item.effects ?? {})).not.toContain("diverged");
  }
  const answered = (n: number) =>
    observed.find((item) => item.id === laneDId(n))?.outcome.actions[1]?.answer;
  expect(answered(11)).toEqual({ code: null, body: {} });
  expect(answered(13)).toEqual({
    code: "CHECK_WAKE_REQUIRED",
    body: { code: "CHECK_WAKE_REQUIRED", error: "refused" },
  });
  // HARNESS by the role's own write: its whole lane R record and null effects.
  const role = items.find((item) => item.id === roleGood.id);
  expect(Object.keys(role ?? {})).toEqual(["id", "case", "outcome", "effects"]);
  expect(role?.effects).toBeNull();
  expect(role?.outcome.lastText).toBe(P05.traces.good.lastText);
  // No verdict, cause, reason, disposition, call id or run directory reaches the reader, and no
  // difference: an ok record's list of them is empty, and a DIVERGED record's is taken out.
  const hidden = [
    "DIVERGED",
    "HARNESS",
    '"diverged":[{',
    "standin",
    "notReplayed",
    "role-write",
    "harness-record",
    "unverified",
    "remake",
    "toolu_",
    HARNESS_REASON,
    "b-clean",
    "role-good",
  ];
  for (const text of hidden) expect(bodies[0]).not.toContain(text);

  // Every run's disposition by its own id, the one the reader was not given among them.
  const labels = JSON.parse(readFileSync(w.out, "utf8")) as {
    batch: string;
    records: { id: string; pass: boolean | null; lines: { pass: boolean | null }[] }[];
    runs: Disposition[];
  };
  expect(labels.batch).toBe("CONCLUSIVE");
  const runIds = [11, 12, 13, 14, 15, 16, 17, 18].map(laneDId);
  expect(labels.runs.map((each) => each.id)).toEqual(runIds);
  const byId = new Map(labels.runs.map((each) => [each.id, each]));
  const row = (n: number) => {
    const each = byId.get(laneDId(n));
    return [
      each?.graded,
      each?.status,
      each?.remake,
      each?.difference?.branch ?? each?.harness?.cause,
    ];
  };
  // (b), clean through the call: unverified, and one whole remake grades it. The line only a
  // later action could show is null: the clean record is no pass.
  expect(row(11)).toEqual(["observed", "unverified", "whole", "accepted"]);
  const clean = labels.records.find((each) => each.id === laneDId(11));
  expect(clean?.pass).toBeNull();
  expect(clean?.lines.map((line) => line.pass)).toEqual([true, null]);
  // (b), a forbidden report in the record through the call: a failure, which no remake replaces.
  expect(row(12)).toEqual(["observed", "fail", "none", "accepted"]);
  // (a): unverified where the case allows a corrected retry after the real refusal, and a failure
  // where it allows none, though no line failed; a remake only beside the run.
  expect(row(13)).toEqual(["observed", "unverified", "beside", "refused"]);
  expect(row(14)).toEqual(["observed", "fail", "beside", "refused"]);
  expect(byId.get(laneDId(14))?.why).toContain("the case allows no corrected retry after it");
  expect(byId.get(laneDId(13))?.difference).toEqual({
    call: "toolu_2",
    outcomeSeq: 2,
    seq: 0,
    route: COMPLETE,
    standin: { status: 200, code: null },
    daemon: { status: 400, code: "CHECK_WAKE_REQUIRED" },
    branch: "refused",
    retry: "allowed",
    observedThrough: 2,
  });
  // A real 500 is a refusal: branch (a), with no retry the case allows after it.
  expect(row(18)).toEqual(["observed", "fail", "beside", "refused"]);
  expect(byId.get(laneDId(18))?.difference).toMatchObject({
    daemon: { status: 500, code: null },
    retry: "not-allowed",
  });
  // The role's own write: its clean lane R record is unverified, and its failure stands.
  expect(row(15)).toEqual(["outcome", "unverified", "beside", "role-write"]);
  expect(row(16)).toEqual(["outcome", "fail", "beside", "role-write"]);
  // The harness's own records: no behavioral grade.
  expect(row(17)).toEqual(["none", "unverified", "beside", "harness-record"]);
  expect(byId.get(laneDId(17))?.harness).toEqual({
    cause: "harness-record",
    reason: HARNESS_REASON,
  });

  // Where a control is graded wrong, no label grades a run: each is unverified, and the clean
  // record of (b) permits no remake.
  const again = io(laneDModel([], P10.traces.good.lastText));
  expect(await main(flags(w, "--key-file", key()), again.io)).toBe(0);
  const inconclusive = JSON.parse(readFileSync(w.out, "utf8")) as {
    batch: string;
    runs: Disposition[];
  };
  expect(inconclusive.batch).toBe("INCONCLUSIVE");
  expect(inconclusive.runs.map((each) => each.status)).toEqual(Array(8).fill("unverified"));
  expect(inconclusive.runs.map((each) => each.remake)).toEqual([
    "none",
    "none",
    "beside",
    "beside",
    "beside",
    "beside",
    "beside",
    "beside",
  ]);
});

/** The public development fixtures of lane D's replay, which teardown_effects_*_test.go read. */
const TEARDOWN_EFFECTS = path.join(
  import.meta.dir,
  "../../../daemon/internal/daemon/testdata/teardown-effects"
);

test("replay.json is read in the Go driver's own format, and a role's write is bound to the outcome action it names", () => {
  // The three records TestTeardownEffectsReplayRecordJSON holds its encoding to, as compact JSON:
  // an ok replay of no call, the role's own write and the harness's own records.
  const exampleFile = path.join(TEARDOWN_EFFECTS, "replay-example.json");
  const examples: { reason?: string }[] = JSON.parse(readFileSync(exampleFile, "utf8"));
  const [ok = "", write = "", records = ""] = examples.map((each) => JSON.stringify(each));
  const lists = '"seed":[],"calls":[],"end":[],"after":[],"notReplayed":[]';
  const reasonOf = (index: number): string => {
    const reason = examples[index]?.reason;
    if (reason === undefined) throw new Error(`${exampleFile} record ${index} names no reason`);
    return reason;
  };
  expect(parseReplay(ok)).toMatchObject({ verdict: "ok", calls: [], notReplayed: [] });

  // The control's own outcome.json scores the role's result as action 4 at that time: a status
  // write there is the role's own, and the run is graded on its lane R record.
  const outcomeFile = path.join(TEARDOWN_EFFECTS, "local-only-head", "outcome.json");
  const outcome = parseOutcome(readFileSync(outcomeFile, "utf8"));
  expect(replayOf(outcome, parseReplay(write), null, P05)).toEqual({
    verdict: "HARNESS",
    cause: "role-write",
    reason: reasonOf(1),
    write: { event: "issue.updated", outcomeSeq: 4, call: "toolu_05published_accepted" },
  });
  expect(replayOf(outcome, parseReplay(records), null, P05)).toEqual({
    verdict: "HARNESS",
    cause: "harness-record",
    reason: reasonOf(2),
  });
  // A role's write bound to an action that is no Dispatch write, or at another time, is refused.
  const elsewhere = write.replace('"outcomeSeq":4', '"outcomeSeq":5');
  expect(() => replayOf(outcome, parseReplay(elsewhere), null, P05)).toThrow(
    "outcome.json holds no Dispatch write as action 5"
  );
  const later = write.replace("15:00:40.260834969Z", "15:00:41Z");
  expect(() => replayOf(outcome, parseReplay(later), null, P05)).toThrow(
    "outcome.json holds no Dispatch write as action 4"
  );
  // A null list, a key the Go driver does not write, and a fault on an ok receipt are refused.
  expect(() => parseReplay(ok.replace('"calls":[]', '"calls":null'))).toThrow("at calls");
  expect(() => parseReplay(ok.replace('"verdict":"ok"', '"verdict":"ok","note":1'))).toThrow();
  const typed = ok.replace(lists, `"fault":{"kind":"harness-record"},${lists}`);
  expect(() => replayOf(outcome, parseReplay(typed), null, P05)).toThrow(
    "which only a HARNESS receipt names"
  );
});

// Lane D's control records: CONTROLS is what teardown-effects.sh --controls wrote of the example in
// the proof Job. In a lane D batch a control is given with the effect record of its trace, in the
// shape of an ok run, and a control whose record is missing or not ok refuses the batch.

/** A copy of CONTROLS under `name`, changed by `change` (given the copy's directory). */
function controlsCopy(name: string, change: (at: string) => void): string {
  const copy = dir(name);
  cpSync(CONTROLS, copy, { recursive: true });
  change(copy);
  return copy;
}

test("a lane D batch gives each control the effect record lane D wrote of its trace, as it gives an ok run", () => {
  const traced = readControls(CONTROLS, cases);
  expect(
    traced.cases.map((each) => [each.id, each.records?.good?.verdict, each.records?.bad?.verdict])
  ).toEqual([
    ["P05", "ok", "ok"],
    ["P10", "ok", "ok"],
  ]);
  const w = world("controls-batch");
  const runs = readRuns(w.records, cases);
  const batch = buildBatch(RUBRIC, traced, runs, 7);
  expect(batch.controls.size).toBe(4);
  const controls = batch.items.filter((item) => batch.controls.has(item.id));
  const run = batch.items.find((item) => !batch.controls.has(item.id));
  const keys = (value: unknown) => Object.keys(value ?? {}).toSorted();
  // Every item holds an effect record, and a control's has the keys an ok run's has.
  for (const item of batch.items) {
    expect(Object.keys(item)).toEqual(["id", "case", "outcome", "effects"]);
    expect(item.effects).not.toBeNull();
    expect(keys(item.effects)).toEqual(keys(run?.effects));
  }
  // The good control of P10 shows the next look its trace set, which the line scheduling reads.
  const p10Good = controls.find(
    (item) => item.case === "P10" && batch.controls.get(item.id) === "good"
  );
  expect(p10Good?.effects?.productionCheck).toEqual({
    wakeAt: "2030-01-08T12:30:00Z",
    checkWakeRow: { dueAt: "2030-01-08T12:30:00Z" },
  });
  expect(
    p10Good?.effects?.answers.map((answer) => [answer.by, answer.status, answer.code])
  ).toEqual([
    ["daemon", 400, "CHECK_WAKE_REQUIRED"],
    ["client", 0, null],
    ["daemon", 200, null],
    ["client", 0, null],
  ]);

  // Nothing in the request tells a control from a run: no trace's own id, no record's directory,
  // no call of the replay, and no key that names a trace, a control or a verdict.
  const request = requestOf(batch);
  const traceIds = cases.cases.flatMap((each) => [each.traces.good.id, each.traces.bad?.id ?? ""]);
  for (const text of [...traceIds, CONTROLS, "trace, action", "replay.json", '"verdict":"ok"'])
    expect(request).not.toContain(text);
  const hidden = ["trace", "control", "good", "bad", "replay", "standin", "calls"];
  expect(keysOf(itemsOf(request)).filter((key) => hidden.includes(key))).toEqual([]);
  // The labels grade each control by the same rule as before.
  const labels = labelsOf(batch, answerWith(reply(batch, byLastText)), REQUEST);
  expect(labels.batch).toBe("CONCLUSIVE");
  expect(labels.controls.map((each) => each.graded)).toEqual(Array(4).fill("right"));

  // A receipt written before the driver held `end` holds none, and still reads.
  const older = readFileSync(path.join(CONTROLS, "P05", "good", "replay.json"), "utf8");
  expect(parseReplay(older.replace(/"end": \[\],\n\s*/, "")).end).toBeUndefined();
});

/** Another actor's Dispatch write after the seed, which outcome.ts keeps among `otherWrites`. */
const OTHER_WRITER = { kind: "session", id: "ses-acme-12-reviewer" };

/**
 * `outcome` with every audit record outcome.ts keeps beside the scored actions: the writer
 * Dispatch names of each Dispatch write (the role's session), the change gh-standin kept of each
 * GitHub call, and a write of another actor.
 */
function audited(outcome: OutcomeRecord): OutcomeRecord {
  const actions = outcome.actions.map((action) => {
    if (action.kind === "dispatch")
      return { ...action, actor: { kind: "session", id: ROLE_SESSION } };
    if (action.kind === "github")
      return { ...action, change: { action: "open", number: 1, head: "legion/ACME-12" } };
    return action;
  });
  const otherWrites = [
    {
      at: "2030-01-07T15:03:00Z",
      command: "comment.created",
      target: "ACME-12",
      fields: { body: "Reviewed by another session." },
      actor: OTHER_WRITER,
    },
  ];
  return parseOutcome(JSON.stringify({ ...outcome, actions, otherWrites }));
}

test("a run and a control reach the reader through one projection, which no audit record outcome.ts keeps can tell apart", () => {
  // Lane D's batch of the example's runs with its controls: as old records, and with every audit
  // record outcome.ts keeps now on the runs and on the traces. The request is the same, byte for
  // byte, so a record that holds none of them is sent as it was.
  const traced = readControls(CONTROLS, cases);
  const runs = readRuns(world("projection").records, cases);
  const auditedRuns = runs.map((run) => ({ ...run, outcome: audited(run.outcome) }));
  const auditedCases = traced.cases.map((each) => ({
    ...each,
    traces: {
      good: each.traces.good && audited(each.traces.good),
      bad: each.traces.bad && audited(each.traces.bad),
    },
  }));
  const request = requestOf(buildBatch(RUBRIC, traced, runs, 7));
  expect(requestOf(buildBatch(RUBRIC, traced, auditedRuns, 7))).toBe(request);
  const batch = buildBatch(RUBRIC, { cases: auditedCases }, auditedRuns, 7);
  const sent = requestOf(batch);
  expect(sent).toBe(request);
  // The same for a batch whose controls carry no effects, as a lane R run carries none.
  const laneR = RUNS.map((run) => ({ ...run, outcome: audited(run.outcome) }));
  const laneRCases = auditedCases.map(({ id, traces }) => ({ id, traces }));
  expect(requestOf(buildBatch(RUBRIC, { cases: laneRCases }, laneR, 7))).toBe(
    requestOf(buildBatch(RUBRIC, cases, RUNS, 7))
  );
  // No audit record reaches the reader: no writer, no other actor's write, no change kept.
  const audit = ["otherWrites", "actor", "change"];
  expect(keysOf(itemsOf(sent)).filter((key) => audit.includes(key))).toEqual([]);
  for (const text of [ROLE_SESSION, OTHER_WRITER.id, "another session"])
    expect(sent).not.toContain(text);

  // Each run did what a trace did, and is given the content its control is given: the same
  // outcome, the same keys of each action, and effects of the same keys. The effects themselves
  // are each replay's own, and the two ids are each item's own.
  const keys = (value: unknown) => Object.keys(value ?? {});
  for (const run of batch.items.filter((item) => !batch.controls.has(item.id))) {
    const control = batch.items.find(
      (item) =>
        batch.controls.has(item.id) &&
        item.case === run.case &&
        item.outcome.lastText === run.outcome.lastText
    );
    expect(control).toBeDefined();
    expect(keys(control)).toEqual(keys(run));
    expect(JSON.stringify(control?.outcome)).toBe(JSON.stringify(run.outcome));
    expect(control?.outcome.actions.map(keys)).toEqual(run.outcome.actions.map(keys));
    expect(keys(control?.effects)).toEqual(keys(run.effects));
    expect(control?.id).not.toBe(run.id);
  }

  // A DIVERGED run, given through its first difference's call, takes the same projection: P10's
  // first report, which the real daemon refused CHECK_WAKE_REQUIRED, after its pull request edit.
  const p10 = { ...P10.traces.good, id: laneDId(21), actions: p10Took() };
  const refused = new Map([[2, { status: 400, code: "CHECK_WAKE_REQUIRED" }]]);
  const observed = (outcome: OutcomeRecord) => {
    const receipts = receiptsOf(outcome, refused);
    const receipt = parseReplay(JSON.stringify(receipts.replay));
    const effects = parseEffects(JSON.stringify(receipts.effects));
    return gradedOf(outcome, replayOf(outcome, receipt, effects, P10));
  };
  const plain = observed(p10);
  expect(plain?.observedThrough).toBe(2);
  expect(plain?.outcome.actions.map((action) => action.kind)).toEqual(["github", "daemon"]);
  expect(JSON.stringify(observed(audited(p10)))).toBe(JSON.stringify(plain));
});

test("a lane D batch whose control has no effect record, or one at odds with its trace, is refused, naming each", async () => {
  const runs = readRuns(world("controls-refused").records, cases);
  const removed = (name: string, ...files: string[]) =>
    controlsCopy(name, (at) => {
      for (const file of files) rmSync(path.join(at, file));
    });
  // A control with no replay.json has no record: the batch of its case names each one.
  const missing = readControls(
    removed(
      "controls-missing",
      "P05/good/replay.json",
      "P05/good/effects.json",
      "P10/bad/replay.json"
    ),
    cases
  );
  expect(() => buildBatch(RUBRIC, missing, runs, 7)).toThrow(
    "P05 good (lane D wrote no replay.json of it); P10 bad (lane D wrote no replay.json of it)"
  );
  // A batch of P10's runs alone needs P10's records alone.
  const p05Gone = readControls(removed("controls-p05-gone", "P05/bad/replay.json"), cases);
  const p10Runs = runs.filter((each) => each.case === "P10");
  expect(buildBatch(RUBRIC, p05Gone, p10Runs, 7).controls.size).toBe(2);
  // A HARNESS replay of a trace leaves its control no effect record.
  const harness = controlsCopy("controls-harness", (at) => {
    rmSync(path.join(at, "P05", "bad", "effects.json"));
    const receipt = { ...harnessReceipts(P05_BAD, { kind: "harness-record" }).replay, end: [] };
    writeFileSync(path.join(at, "P05", "bad", "replay.json"), JSON.stringify(receipt));
  });
  expect(() => buildBatch(RUBRIC, readControls(harness, cases), runs, 7)).toThrow(
    `P05 bad (its replay is HARNESS: ${HARNESS_REASON})`
  );
  // A record with no effects.json, or another trace's, is refused as a run's receipt is.
  const noEffects = removed("controls-no-effects", "P10/good/effects.json");
  expect(() => readControls(noEffects, cases)).toThrow("the run holds no effects.json");
  const swapped = controlsCopy("controls-swapped", (at) => {
    for (const file of ["replay.json", "effects.json"])
      cpSync(path.join(at, "P05", "good", file), path.join(at, "P05", "bad", file));
  });
  expect(() => readControls(swapped, cases)).toThrow(
    `replay.json is the receipt of ${P05.traces.good.id}, not of ${P05_BAD.id}`
  );

  // Through the command line, each refusal comes before any call, and puts no attempt on record.
  const sent: string[] = [];
  const quiet = io(async (url) => {
    sent.push(url);
    return new Response("", { status: 500 });
  });
  const refuses = async (w: World, message: string) => {
    expect(await main(flags(w, "--key-file", key()), quiet.io)).toBe(2);
    expect(quiet.stderr.at(-1)).toContain("refused before any model call; nothing was sent");
    expect(quiet.stderr.at(-1)).toContain(message);
    expect(existsSync(`${w.out}.attempts.json`)).toBe(false);
  };
  const w = world("controls-command");
  await refuses(
    { ...w, controls: undefined },
    "--controls is required: lane D replays the cases P05, P10"
  );
  await refuses(
    { ...w, controls: removed("controls-command-missing", "P10/bad/replay.json") },
    "P10 bad (lane D wrote no replay.json of it)"
  );
  const frozen = JSON.parse(RUBRIC_FILE);
  frozen.cases[1].rubric.pop();
  writeFileSync(w.rubric, JSON.stringify(frozen));
  await refuses(
    w,
    "the frozen rubric lacks a line heldout-case.ts requires of its case: P10 scheduling"
  );
  await refuses(
    { ...skillWorld("controls-skilluse"), controls: CONTROLS },
    "lane D does not replay a skilluse set"
  );
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
    flags({ ...w, controls: undefined }, "--key-file", key()).map((arg) =>
      arg === EXAMPLE ? SKILL_USE_EXAMPLE : arg
    ),
    "gives the batch no control"
  );
  await refuses(
    ["--cases", EXAMPLE, "--seed", "7", "--key-file", key()],
    "--rubric, --records and --out"
  );
  await refuses(["--seed", "7"], "--cases and --seed are required");
  await refuses(
    flags(w, "--capture", w.root, "--key-file", key()),
    "--capture takes no --rubric, --records, --controls or --out"
  );
  await refuses(
    ["--capture", w.root, "--cases", EXAMPLE, "--seed", "7", "--controls", CONTROLS],
    "--capture takes no --rubric, --records, --controls or --out"
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
const byWords: Passes = (item, line) => (item.outcome.lastText ?? "").includes(line);

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
    [{ case: "P09", outcome: SKILLED, replay: null }],
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
  expect(bodies).toEqual([requestOf(skillUseBatch(frozen, file, readRuns(w.records, file), 7))]);
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
  // Each run is lane R's alone: its disposition is the reader's grade of its outcome.
  expect(
    labels.runs.map((each: Disposition) => [each.id, each.lane, each.replay, each.status])
  ).toEqual([
    [uuid(11), "R", null, "pass"],
    [uuid(12), "R", null, "fail"],
    [uuid(13), "R", null, "pass"],
  ]);
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

test("a skilluse run, which lane D does not replay, that holds a replay receipt is refused before any call", async () => {
  const sent: string[] = [];
  const quiet = io(async (url) => {
    sent.push(url);
    return new Response("", { status: 500 });
  });
  const w = skillWorld("skilluse-receipt");
  const at = path.join(w.records, "P09", "arm-skilled");
  writeFileSync(path.join(at, "replay.json"), JSON.stringify(receiptsOf(SKILLED).replay));
  expect(await main(flags(w, "--key-file", key()), quiet.io)).toBe(2);
  expect(quiet.stderr.at(-1)).toContain("case P09 runs in lane R alone");
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
