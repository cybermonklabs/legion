import { expect, test } from "bun:test";
import { existsSync, mkdirSync, readdirSync, readFileSync, writeFileSync } from "node:fs";
import path from "node:path";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import { labelsName, main, PROCEDURE, type Reader } from "./heldout-grade";
import { sha256 } from "./heldout-reader";

const fresh = temporaryDirectories("heldout-grade");

/** The three seeds every row fixes before its first read. */
const SEEDS = [101, 202, 303] as const;
const [FIRST, SECOND, THIRD] = SEEDS;
/** The time at which the stand-in reader logs each attempt. */
const AT = "2030-01-01T00:00:00.000Z";

type Verdict = "CONCLUSIVE" | "INCONCLUSIVE";

/** What the stand-in reader does at one call: its exit code, the verdict of the labels it writes at
 * exit 0, and a request of its own in place of the one its seed makes. */
interface Step {
  readonly exit: 0 | 1 | 2;
  readonly batch?: Verdict;
  readonly request?: string;
}

const CONCLUSIVE: Step = { exit: 0, batch: "CONCLUSIVE" };
const INCONCLUSIVE: Step = { exit: 0, batch: "INCONCLUSIVE" };
const NO_LABELS: Step = { exit: 1 };
const REFUSED: Step = { exit: 2 };

/** One call of the stand-in reader: what it was given, what it logged and wrote, and the procedure
 * record as it was on disk at the call (null where there was none). */
interface Call {
  readonly argv: readonly string[];
  readonly seed: number;
  readonly exit: number;
  readonly request: string | null;
  readonly labels: { readonly sha256: string; readonly batch: Verdict | undefined } | null;
  readonly record: unknown;
}

function valueAfter(argv: readonly string[], flag: string): string {
  const value = argv[argv.indexOf(flag) + 1];
  if (!argv.includes(flag) || value === undefined)
    throw new Error(`the stand-in reader was given no ${flag}`);
  return value;
}

/**
 * A reader that acts out `steps`, one a call, as heldout-reader.ts acts: at exit 2 it writes
 * nothing; otherwise it logs {at, seed, request_sha256} to <out>.attempts.json first, and at exit 0
 * it then writes the labels to --out. The request is the same for the same seed, as the reader's
 * request is for the same records and seed. A call past the last step, or one whose output is
 * already there, throws.
 */
function standIn(steps: readonly Step[], record: string, events: string[]) {
  const calls: Call[] = [];
  const reader: Reader = async (argv) => {
    const step = steps[calls.length];
    if (step === undefined) throw new Error(`the stand-in reader has no step ${calls.length + 1}`);
    const seed = Number(valueAfter(argv, "--seed"));
    const out = valueAfter(argv, "--out");
    events.push(`read ${seed}`);
    if (existsSync(out) || existsSync(`${out}.attempts.json`))
      throw new Error(`the read at seed ${seed} writes over ${out}`);
    const onDisk: unknown = existsSync(record) ? JSON.parse(readFileSync(record, "utf8")) : null;
    let request: string | null = null;
    let labels: Call["labels"] = null;
    if (step.exit !== 2) {
      request = sha256(step.request ?? `the batch's request at seed ${seed}`);
      const attempt = { at: AT, seed, request_sha256: request };
      writeFileSync(`${out}.attempts.json`, `${JSON.stringify([attempt], null, 2)}\n`);
    }
    if (step.exit === 0) {
      const text = `${JSON.stringify({ version: 2, batch: step.batch, seed, request_sha256: request }, null, 2)}\n`;
      writeFileSync(out, text);
      labels = { sha256: sha256(text), batch: step.batch };
    }
    calls.push({ argv, seed, exit: step.exit, request, labels, record: onDisk });
    return step.exit;
  };
  return { reader, calls };
}

/** The membership of every world's --records: its case directories and their run directories. */
const MEMBERSHIP = [
  { case: "S01", runs: ["run-a", "run-b"] },
  { case: "S02", runs: ["run-c"] },
];

/** A batch's --records directory, with a file that is no run beside its runs; the grade directory
 * and its procedure record; and the reader's flags, which name files the stand-in never reads. */
interface World {
  readonly root: string;
  readonly records: string;
  readonly grade: string;
  readonly record: string;
  readonly readerFlags: readonly string[];
}

function world(): World {
  const root = fresh();
  const records = path.join(root, "records");
  for (const run of ["S01/run-a", "S01/run-b", "S02/run-c"]) {
    mkdirSync(path.join(records, run), { recursive: true });
  }
  writeFileSync(path.join(records, "S01", "notes.txt"), "no run\n");
  const readerFlags = [
    "--rubric",
    path.join(root, "rubric.json"),
    "--cases",
    path.join(root, "cases.json"),
    "--records",
    records,
    "--key-file",
    path.join(root, "key"),
  ];
  const grade = path.join(root, "grade");
  return { root, records, grade, record: path.join(grade, PROCEDURE), readerFlags };
}

/** The procedure's arguments, its own flags among the reader's. */
function argv(
  w: World,
  { seeds = SEEDS.join(","), wait = "0", reader = w.readerFlags } = {}
): string[] {
  return [
    "--grade-dir",
    w.grade,
    ...reader.slice(0, 4),
    "--seeds",
    seeds,
    ...reader.slice(4),
    "--repeat-wait",
    wait,
  ];
}

/** An io whose waits are recorded in `events` and return at once. */
function io(w: World, events: string[]) {
  const stdout: string[] = [];
  const stderr: string[] = [];
  return {
    stdout,
    stderr,
    io: {
      env: {},
      cwd: w.root,
      fetch: async (): Promise<Response> => {
        throw new Error("the procedure makes no HTTP call");
      },
      sleep: async (ms: number) => {
        events.push(`wait ${ms}`);
      },
      out: (line: string) => stdout.push(line),
      err: (line: string) => stderr.push(line),
    },
  };
}

const read = (seed: number) => `read ${seed}`;
const WAIT = "wait 0";

interface Row {
  readonly name: string;
  readonly steps: readonly Step[];
  /** The stand-in's reads by seed, and each wait, in order. */
  readonly events: string[];
  readonly invocations: number;
  readonly result: "conclusive" | "unresolved" | "failed" | "refused";
  /** The invocation whose labels are the grade. */
  readonly grade?: number;
  readonly exit: number;
}

const ROWS: Row[] = [
  {
    name: "conclusive at the first read",
    steps: [CONCLUSIVE],
    events: [read(FIRST)],
    invocations: 1,
    result: "conclusive",
    grade: 1,
    exit: 0,
  },
  {
    name: "no labels, then the identical repeat is conclusive",
    steps: [NO_LABELS, CONCLUSIVE],
    events: [read(FIRST), WAIT, read(FIRST)],
    invocations: 2,
    result: "conclusive",
    grade: 2,
    exit: 0,
  },
  {
    name: "no labels twice",
    steps: [NO_LABELS, NO_LABELS],
    events: [read(FIRST), WAIT, read(FIRST)],
    invocations: 2,
    result: "unresolved",
    exit: 1,
  },
  {
    name: "inconclusive, then the second seed is conclusive",
    steps: [INCONCLUSIVE, CONCLUSIVE],
    events: [read(FIRST), read(SECOND)],
    invocations: 2,
    result: "conclusive",
    grade: 2,
    exit: 0,
  },
  {
    name: "inconclusive three times",
    steps: [INCONCLUSIVE, INCONCLUSIVE, INCONCLUSIVE],
    events: [read(FIRST), read(SECOND), read(THIRD)],
    invocations: 3,
    result: "unresolved",
    exit: 1,
  },
  {
    name: "no labels, repeat inconclusive, second seed inconclusive, third seed conclusive",
    steps: [NO_LABELS, INCONCLUSIVE, INCONCLUSIVE, CONCLUSIVE],
    events: [read(FIRST), WAIT, read(FIRST), read(SECOND), read(THIRD)],
    invocations: 4,
    result: "conclusive",
    grade: 4,
    exit: 0,
  },
  {
    name: "inconclusive twice, then no labels twice at the third seed",
    steps: [INCONCLUSIVE, INCONCLUSIVE, NO_LABELS, NO_LABELS],
    events: [read(FIRST), read(SECOND), read(THIRD), WAIT, read(THIRD)],
    invocations: 4,
    result: "unresolved",
    exit: 1,
  },
  {
    name: "a second no-labels after the one repeat is spent",
    steps: [NO_LABELS, INCONCLUSIVE, NO_LABELS],
    events: [read(FIRST), WAIT, read(FIRST), read(SECOND)],
    invocations: 3,
    result: "unresolved",
    exit: 1,
  },
  {
    name: "a refusal at the first read",
    steps: [REFUSED],
    events: [read(FIRST)],
    invocations: 0,
    result: "refused",
    exit: 2,
  },
  {
    name: "a refusal after an inconclusive read",
    steps: [INCONCLUSIVE, REFUSED],
    events: [read(FIRST), read(SECOND)],
    invocations: 1,
    result: "refused",
    exit: 1,
  },
  {
    name: "a repeat whose request sha256 differs",
    steps: [NO_LABELS, { ...CONCLUSIVE, request: "a request of other records" }],
    events: [read(FIRST), WAIT, read(FIRST)],
    invocations: 2,
    result: "failed",
    exit: 1,
  },
];

test.each(ROWS)("$name", async (row) => {
  const w = world();
  const events: string[] = [];
  const reader = standIn(row.steps, w.record, events);
  const run = io(w, events);
  expect(await main(argv(w), run.io, reader.reader)).toBe(row.exit);

  // The seeds in order, with the wait before the one repeat alone, and every step read.
  expect(events).toEqual(row.events);
  expect(reader.calls).toHaveLength(row.steps.length);
  // Each read gets the reader's flags unchanged, then a --seed and an --out of its own.
  for (const [index, call] of reader.calls.entries()) {
    const out = path.join(w.grade, labelsName(index + 1));
    expect(call.argv).toEqual([...w.readerFlags, "--seed", String(call.seed), "--out", out]);
  }

  // The record on disk names every invocation as the reader logged and wrote it. A refusal is none.
  const onDisk = JSON.parse(readFileSync(w.record, "utf8"));
  const made = reader.calls.filter((call) => call.exit !== 2);
  expect(made).toHaveLength(row.invocations);
  expect(onDisk.invocations).toEqual(
    made.map((call, index) => ({
      number: index + 1,
      seed: call.seed,
      out: labelsName(index + 1),
      at: AT,
      request_sha256: call.request,
      exit: call.exit,
      labels: call.labels,
    }))
  );
  // Before each read the record was on disk and whole: the membership, the seeds and each earlier
  // invocation, and no result yet.
  expect(onDisk.membership).toEqual(MEMBERSHIP);
  expect(onDisk.seeds).toEqual([...SEEDS]);
  for (const [index, call] of reader.calls.entries()) {
    expect(call.record).toEqual({
      ...onDisk,
      invocations: onDisk.invocations.slice(0, index),
      result: null,
    });
  }

  expect(onDisk.result.status).toBe(row.result);
  if (row.grade === undefined) {
    expect(onDisk.result.labels).toBeUndefined();
    expect(run.stdout.filter((line) => line.startsWith("grade:"))).toEqual([]);
  } else {
    const labels = path.join(w.grade, labelsName(row.grade));
    const labelsSha = sha256(readFileSync(labels, "utf8"));
    expect(onDisk.result).toEqual({
      status: "conclusive",
      labels: labelsName(row.grade),
      labels_sha256: labelsSha,
    });
    expect(run.stdout).toContain(`grade: ${labels} sha256:${labelsSha}`);
  }
  const said = run.stderr.join("\n");
  if (row.result === "refused") expect(said).toContain("nothing was sent");
  // A failed repeat names both request digests.
  if (row.result === "failed") for (const call of made) expect(said).toContain(call.request ?? "");
});

test("the wait before the one repeat is the given seconds, and no other read waits", async () => {
  const w = world();
  const events: string[] = [];
  const reader = standIn([INCONCLUSIVE, NO_LABELS, CONCLUSIVE], w.record, events);
  expect(await main(argv(w, { wait: "45" }), io(w, events).io, reader.reader)).toBe(0);
  expect(events).toEqual([read(FIRST), read(SECOND), "wait 45000", read(SECOND)]);
});

/** Each file in `dir` with its text, or null where `dir` is absent. */
function contents(dir: string): Record<string, string> | null {
  if (!existsSync(dir)) return null;
  return Object.fromEntries(
    readdirSync(dir).map((name) => [name, readFileSync(path.join(dir, name), "utf8")])
  );
}

/** A record an earlier procedure left in `w`'s grade directory, with `result`. */
function earlierRecord(w: World, result: unknown): void {
  mkdirSync(w.grade, { recursive: true });
  const request = sha256("an earlier request");
  const invocation = {
    number: 1,
    seed: FIRST,
    out: labelsName(1),
    at: AT,
    request_sha256: request,
    exit: 1,
    labels: null,
  };
  const record = {
    version: 1,
    at: AT,
    records: w.records,
    membership: MEMBERSHIP,
    seeds: [...SEEDS],
    repeat_wait_seconds: 0,
    invocations: [invocation],
    result,
  };
  writeFileSync(w.record, `${JSON.stringify(record, null, 2)}\n`);
  const attempt = { at: AT, seed: FIRST, request_sha256: request };
  writeFileSync(path.join(w.grade, `${labelsName(1)}.attempts.json`), JSON.stringify([attempt]));
}

interface Refusal {
  readonly name: string;
  readonly setup?: (w: World) => void;
  readonly argv: (w: World) => string[];
  /** A word of the refusal that names what it refuses. */
  readonly names: string;
}

const REFUSALS: Refusal[] = [
  {
    name: "a grade directory that holds an interrupted procedure's record",
    setup: (w) => earlierRecord(w, null),
    argv,
    names: PROCEDURE,
  },
  {
    name: "a grade directory that holds an ended procedure's record",
    setup: (w) => earlierRecord(w, { status: "unresolved", why: "no labels twice" }),
    argv,
    names: PROCEDURE,
  },
  {
    name: "a grade directory that holds another file",
    setup: (w) => {
      mkdirSync(w.grade, { recursive: true });
      writeFileSync(path.join(w.grade, labelsName(1)), "{}\n");
    },
    argv,
    names: labelsName(1),
  },
  {
    name: "a --seed among the reader's flags",
    argv: (w) => [...argv(w), "--seed", "7"],
    names: "--seed",
  },
  {
    name: "an --out among the reader's flags",
    argv: (w) => [...argv(w), "--out", path.join(w.root, "labels.json")],
    names: "--out",
  },
  { name: "two seeds", argv: (w) => argv(w, { seeds: "101,202" }), names: "--seeds" },
  { name: "a seed given twice", argv: (w) => argv(w, { seeds: "101,202,101" }), names: "--seeds" },
  {
    name: "a seed of 2^32",
    argv: (w) => argv(w, { seeds: `101,202,${2 ** 32}` }),
    names: "--seeds",
  },
  {
    name: "a wait that is not a whole number of seconds",
    argv: (w) => argv(w, { wait: "1.5" }),
    names: "--repeat-wait",
  },
  {
    name: "no --records",
    argv: (w) =>
      argv(w, { reader: w.readerFlags.filter((_, index) => index !== 4 && index !== 5) }),
    names: "--records",
  },
];

test.each(REFUSALS)("$name is refused before any read", async (row) => {
  const w = world();
  row.setup?.(w);
  const before = contents(w.grade);
  const events: string[] = [];
  // A stand-in with no step: any read throws, and fails the row.
  const reader = standIn([], w.record, events);
  const run = io(w, events);
  expect(await main(row.argv(w), run.io, reader.reader)).toBe(2);
  expect(events).toEqual([]);
  expect(run.stderr.at(-1)).toContain("nothing was sent");
  expect(run.stderr.at(-1)).toContain(row.names);
  // Nothing is written, and an earlier record stays as it was.
  expect(contents(w.grade)).toEqual(before);
});
