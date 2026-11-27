#!/usr/bin/env bun
/**
 * The registered grading procedure of one held-out batch. It runs heldout-reader.ts on the batch
 * until a read gives CONCLUSIVE labels or the procedure's bounds are spent, and the first CONCLUSIVE
 * labels are the batch's grade. It knows no arm: it joins nothing to arms, and of the labels it
 * reads only the batch's verdict.
 *
 *   bun heldout-grade.ts --grade-dir <dir> --seeds <first>,<second>,<third> --repeat-wait <seconds>
 *       <the flags of heldout-reader.ts, but --seed and --out>
 *
 * --grade-dir is a new or empty directory. It holds the procedure's record and every read.
 * --seeds are the three shuffle seeds fixed before launch: the first read's seed, then the seeds of
 * the reads after a first and a second INCONCLUSIVE. Each is a whole number below 2^32, and no two
 * are the same.
 * --repeat-wait is the whole number of seconds the program waits before it repeats a read that
 * gave no labels.
 * Every other argument goes to the reader unchanged and in its order. The program gives each read
 * its own --seed and --out, so the caller gives neither. Of the reader's flags it reads only the
 * directory names under --records.
 *
 * Before the first read, the program writes the procedure record, <grade dir>/procedure.json: the
 * batch's membership (each case directory under --records and each run directory in it, by name
 * alone, as the reader lists them; no file of a run is read), the three seeds, the wait and the
 * time. A grade directory that holds a procedure record is refused: a batch is read by one
 * procedure only, and an interrupted procedure is never continued. After each reader invocation
 * the program adds it to the record: its number, its seed, its labels file, the time and request
 * sha256 that the reader logged in <out>.attempts.json before its call, the reader's exit code and,
 * where the reader wrote labels, their sha256 and the batch's verdict. So the record on disk is
 * whole after each invocation. Its `result` is null until the procedure ends.
 *
 * Invocation n writes its labels to <grade dir>/labels-<n>.json, and the reader writes its logs
 * beside them, so no invocation writes over another. The procedure:
 * - An invocation that gives no labels (reader exit 1) is repeated once, after the wait, with the
 *   same seed. The batch has one repeat, not one for each seed. The repeat's request sha256 must be
 *   the repeated invocation's. Where it is not, the procedure fails and names both.
 * - An invocation whose labels are INCONCLUSIVE is followed by one at the next seed. Nothing else
 *   moves the seed.
 * - The first CONCLUSIVE labels end the procedure and are the grade. No invocation follows them.
 * - The procedure makes at most four invocations: one at each seed, and the one repeat.
 * - A reader refusal (exit 2) sent nothing and is no invocation. It ends the procedure.
 * The record's `result` is one of these: `conclusive`, with the grade's labels file and its
 * sha256; `unresolved`, where the bounds are spent with no CONCLUSIVE labels; `failed`, where the
 * record of an invocation cannot be trusted (a repeat with another request sha256, no request
 * logged before the call, labels that cannot be read, or an exit code the reader never gives); and
 * `refused`, where the reader refused an invocation. Only `conclusive` names a grade. The runs of a
 * batch with no grade stay unverified, in the planned count.
 *
 * Exit 0 when the first CONCLUSIVE labels are the grade, and stdout names their file and sha256; 1
 * when the procedure ended with no grade after an invocation; 2 when it was refused before any
 * invocation, the error saying that nothing was sent.
 */
import {
  existsSync,
  mkdirSync,
  readdirSync,
  readFileSync,
  renameSync,
  rmSync,
  statSync,
  writeFileSync,
} from "node:fs";
import path from "node:path";
import { messageFor } from "@legion/envoy-client/errors";
import { z } from "zod";
import { main as readerMain, sha256 } from "./heldout-reader";
import { type CliIo, processIo } from "./production-dispatch";

/** One run of heldout-reader.ts: its `main`, or a test's stand-in with the same signature. */
export type Reader = (argv: readonly string[], io: CliIo) => Promise<number>;

/** The procedure record's name in the grade directory. */
export const PROCEDURE = "procedure.json";

/** The most reader invocations one batch takes: one at each of the three seeds, and the one
 * repeat. */
export const MAX_INVOCATIONS = 4;

/** The labels file of invocation `number` in the grade directory. */
export function labelsName(number: number): string {
  return `labels-${number}.json`;
}

/** A case directory of --records and the run directories in it, by name. */
interface Member {
  readonly case: string;
  readonly runs: readonly string[];
}

type Verdict = "CONCLUSIVE" | "INCONCLUSIVE";

/** One reader invocation as the record holds it. `at` and `request_sha256` are null where the
 * reader logged no attempt, and `labels` is null where it wrote none. */
interface Invocation {
  readonly number: number;
  readonly seed: number;
  readonly out: string;
  readonly at: string | null;
  readonly request_sha256: string | null;
  readonly exit: number;
  readonly labels: { readonly sha256: string; readonly batch: Verdict } | null;
}

/** How the procedure ended. */
type Result =
  | { readonly status: "conclusive"; readonly labels: string; readonly labels_sha256: string }
  | { readonly status: "unresolved" | "failed" | "refused"; readonly why: string };

const Sha256 = z.string().regex(/^[0-9a-f]{64}$/, "a sha256");

/** <out>.attempts.json as the reader writes it: each attempt, logged before its call. */
const Attempts = z.array(
  z.looseObject({ at: z.string(), seed: z.number(), request_sha256: Sha256 })
);

/** The one part of a labels file that the procedure reads: the batch's verdict. */
const LabelsVerdict = z.looseObject({ batch: z.enum(["CONCLUSIVE", "INCONCLUSIVE"]) });

const USAGE =
  "usage: heldout-grade.ts --grade-dir <dir> --seeds <n>,<n>,<n> --repeat-wait <seconds> <the flags of heldout-reader.ts, but --seed and --out>";

interface Arguments {
  readonly gradeDir: string;
  readonly seeds: readonly number[];
  readonly repeatWait: number;
  readonly records: string;
  readonly reader: readonly string[];
}

const OWN_FLAGS: readonly string[] = ["--grade-dir", "--seeds", "--repeat-wait"];

/** The reader's flags that the program gives each invocation itself. */
const GIVEN_FLAGS: readonly string[] = ["--seed", "--out"];

/** The arguments, or the refusal that names the bad one. */
function parseArguments(argv: readonly string[]): Arguments | string {
  const own = new Map<string, string>();
  const reader: string[] = [];
  for (let index = 0; index < argv.length; index++) {
    const token = argv[index] ?? "";
    if (GIVEN_FLAGS.includes(token))
      return `${token} is the program's own: it gives each invocation its ${token}. ${USAGE}`;
    if (!OWN_FLAGS.includes(token)) {
      reader.push(token);
      continue;
    }
    index++;
    const value = argv[index];
    if (value === undefined || value === "") return `${token} needs a value. ${USAGE}`;
    if (own.has(token)) return `${token} is given twice`;
    own.set(token, value);
  }
  const gradeDir = own.get("--grade-dir");
  const seedList = own.get("--seeds");
  const wait = own.get("--repeat-wait");
  if (gradeDir === undefined || seedList === undefined || wait === undefined)
    return `--grade-dir, --seeds and --repeat-wait are required. ${USAGE}`;
  const seeds = seedList.split(",");
  if (seeds.length !== 3 || seeds.some((seed) => !/^\d+$/.test(seed) || Number(seed) >= 2 ** 32))
    return `--seeds ${seedList} is not three whole numbers below 2^32, split by commas`;
  if (new Set(seeds.map(Number)).size !== seeds.length)
    return `--seeds ${seedList} gives one seed twice`;
  if (!/^\d+$/.test(wait)) return `--repeat-wait ${wait} is not a whole number of seconds`;
  const at = reader.flatMap((token, index) => (token === "--records" ? [index] : []));
  if (at.length > 1) return "--records is given twice";
  const records = at[0] === undefined ? undefined : reader[at[0] + 1];
  if (records === undefined || records === "")
    return `--records <dir> is required: the record holds the batch's membership from it. ${USAGE}`;
  return { gradeDir, seeds: seeds.map(Number), repeatWait: Number(wait), records, reader };
}

/** The batch's membership: each case directory under `records` and the run directories in it,
 * sorted, as heldout-reader.ts's readRuns lists them. It reads directory names alone. */
function membershipOf(records: string): Member[] {
  const directories = (parent: string) =>
    readdirSync(parent)
      .sort()
      .filter((name) => statSync(path.join(parent, name)).isDirectory());
  return directories(records).map((id) => ({
    case: id,
    runs: directories(path.join(records, id)),
  }));
}

/** The one attempt that the reader logged in `file` before its call at `seed`. */
function attemptOf(file: string, seed: number): { at: string; request_sha256: string } {
  const attempts = Attempts.parse(JSON.parse(readFileSync(file, "utf8")));
  const [attempt] = attempts;
  if (attempt === undefined || attempts.length !== 1)
    throw new Error(`${file} holds ${attempts.length} attempts, not one`);
  if (attempt.seed !== seed) throw new Error(`${file} logs seed ${attempt.seed}, not ${seed}`);
  return { at: attempt.at, request_sha256: attempt.request_sha256 };
}

/** Invocation `number` as the record holds it, from what the reader left in `dir`, and why its
 * record cannot be trusted, or null. */
function invocationOf(
  dir: string,
  number: number,
  seed: number,
  exit: number
): [Invocation, string | null] {
  const out = labelsName(number);
  const file = path.join(dir, out);
  const unlogged = { number, seed, out, at: null, request_sha256: null, exit, labels: null };
  if (exit !== 0 && exit !== 1)
    return [
      unlogged,
      `the reader ended invocation ${number} with exit ${exit}, which it never gives`,
    ];
  let logged: Invocation;
  try {
    logged = { ...unlogged, ...attemptOf(`${file}.attempts.json`, seed) };
  } catch (error) {
    return [
      unlogged,
      `invocation ${number} logged no request before its call: ${messageFor(error)}`,
    ];
  }
  if (exit === 1) return [logged, null];
  try {
    const text = readFileSync(file, "utf8");
    const { batch } = LabelsVerdict.parse(JSON.parse(text));
    return [{ ...logged, labels: { sha256: sha256(text), batch } }, null];
  } catch (error) {
    return [
      logged,
      `invocation ${number} ended with exit 0, and its labels cannot be read: ${messageFor(error)}`,
    ];
  }
}

/** Writes `text` to `file` through a temporary file beside it, so a reader never sees half. */
function writeWhole(file: string, text: string): void {
  const partial = `${file}.${process.pid}.tmp`;
  try {
    writeFileSync(partial, text);
    renameSync(partial, file);
  } finally {
    rmSync(partial, { force: true });
  }
}

/** The whole procedure. Returns the exit status: 0 (a grade), 1 (no grade) or 2 (refused). */
export async function main(
  argv: readonly string[],
  io: CliIo,
  reader: Reader = readerMain
): Promise<number> {
  const refuse = (why: string) => {
    io.err(`heldout-grade: refused before any reader invocation; nothing was sent: ${why}`);
    return 2;
  };
  const args = parseArguments(argv);
  if (typeof args === "string") return refuse(args);
  const dir = path.resolve(args.gradeDir);
  const record = path.join(dir, PROCEDURE);
  const invocations: Invocation[] = [];
  let save: (result: Result | null) => void;
  try {
    const held = existsSync(dir) ? readdirSync(dir).sort() : [];
    if (held.includes(PROCEDURE))
      return refuse(
        `${record} exists: a batch is read by one procedure only, and an earlier procedure is never continued`
      );
    if (held.length > 0)
      return refuse(
        `${dir} holds ${held.join(", ")}: a procedure starts in a new or empty directory`
      );
    const records = path.resolve(args.records);
    const started = {
      version: 1,
      at: new Date().toISOString(),
      records,
      membership: membershipOf(records),
      seeds: args.seeds,
      repeat_wait_seconds: args.repeatWait,
    };
    save = (result) =>
      writeWhole(record, `${JSON.stringify({ ...started, invocations, result }, null, 2)}\n`);
    mkdirSync(dir, { recursive: true });
    // The membership and the seeds on record before the first invocation.
    save(null);
  } catch (error) {
    return refuse(messageFor(error));
  }
  io.out(`procedure: ${record}`);

  const finish = (result: Result): number => {
    save(result);
    if (result.status === "conclusive") {
      io.out(`grade: ${path.join(dir, result.labels)} sha256:${result.labels_sha256}`);
      return 0;
    }
    io.err(
      `heldout-grade: ${result.status}: ${result.why}. No labels are the grade, and the batch's runs stay unverified. The record is ${record}.`
    );
    return invocations.length === 0 ? 2 : 1;
  };

  let seedIndex = 0;
  // The invocation that the next one repeats, after it gave no labels.
  let repeating: Invocation | undefined;
  let repeatSpent = false;
  let unresolved = `the batch's ${MAX_INVOCATIONS} reader invocations are spent`;
  for (let number = 1; number <= MAX_INVOCATIONS; number++) {
    const seed = args.seeds[seedIndex];
    if (repeating !== undefined) await io.sleep(args.repeatWait * 1000);
    const out = path.join(dir, labelsName(number));
    const exit = await reader([...args.reader, "--seed", String(seed), "--out", out], io);
    if (exit === 2)
      return finish({
        status: "refused",
        why: `the reader refused the read at seed ${seed} before any model call, so nothing was sent for it. It counts as no invocation`,
      });
    const [invocation, failure] = invocationOf(dir, number, seed, exit);
    invocations.push(invocation);
    save(null);
    const verdict = invocation.labels?.batch ?? "no labels";
    io.out(`invocation ${number}: seed ${seed}, exit ${exit}, ${verdict}`);
    if (failure !== null) return finish({ status: "failed", why: failure });
    if (repeating !== undefined && invocation.request_sha256 !== repeating.request_sha256)
      return finish({
        status: "failed",
        why: `invocation ${number} repeats invocation ${repeating.number}, and its request sha256 ${invocation.request_sha256} is not ${repeating.request_sha256}`,
      });
    repeating = undefined;
    const labels = invocation.labels;
    if (labels === null) {
      if (repeatSpent) {
        unresolved = `invocation ${number} gave no labels, and the batch's one repeat is spent`;
        break;
      }
      repeatSpent = true;
      repeating = invocation;
      continue;
    }
    if (labels.batch === "CONCLUSIVE")
      return finish({ status: "conclusive", labels: invocation.out, labels_sha256: labels.sha256 });
    if (seedIndex === args.seeds.length - 1) {
      unresolved = `invocation ${number} was INCONCLUSIVE at the last fixed seed`;
      break;
    }
    seedIndex++;
  }
  return finish({ status: "unresolved", why: unresolved });
}

if (import.meta.main) {
  process.exit(await main(Bun.argv.slice(2), processIo()));
}
