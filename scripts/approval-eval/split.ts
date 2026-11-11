import { mkdir, writeFile } from "node:fs/promises";
import { join } from "node:path";
import { parseArgs } from "node:util";
import type { CaseKind, CaseRow } from "./build-corpus";
import {
  type Corpus,
  CorpusError,
  newSeed,
  OutputPathError,
  outDirectory,
  pickAtRandom,
  readCorpus,
  UsageError,
} from "./corpus-io";

/**
 * Cuts a corpus in two at a date, and holds the dating rule the recipes judge under.
 *
 * The cut is what a recipe may learn from: the cases answered before it are the training side,
 * the cases answered at or after it are the side a recipe is scored on. `before.json` and
 * `after.json` hold the case ids of each side, written outside this repository's workspace.
 *
 * The dating rule is `evidenceFor`: judging one case, a recipe may be shown a case only when
 * that case was answered strictly earlier than the case being judged and is on the training
 * side of the cut. A case answered at the same moment is out, since a decision taken at the
 * same moment was not available to be learned from, and the case itself is out of its own
 * evidence. Dropping the time test (`--no-dating-rule`, or `datingRule: false`) lets later
 * decisions through: that is a negative control, a run that should score higher because it
 * cheats, and it is there for that one check and for nothing else.
 */

const USAGE = `usage: bun scripts/approval-eval/split.ts --corpus <dir> --cut <date> --out <dir> \\
  [--no-dating-rule] [--case <case id>] [--seed <n>]

Cuts a corpus at a date and shows the dating rule the recipes judge under.

Options:
  --corpus <dir>     Required. A directory holding corpus.jsonl and snapshot.json.
  --cut <date>       Required. An ISO 8601 time, for example 2026-11-05T00:00:00Z. A case
                     answered before it is on the training side.
  --out <dir>        Required. Where before.json and after.json are written. An absolute path
                     outside this repository's workspace.
  --no-dating-rule   Drop the time test, so later decisions come through. The negative control.
  --case <case id>   The case the evidence counts are shown for. With none, one at random.
  --seed <n>         The seed of that pick, so a printed seed picks the same case again.

Exit status: 0 when the split was written, 2 when it could not run.`;

/** How many of the cases the control lets through and the rule does not are named. */
const LET_THROUGH_SHOWN = 5;

interface Flags {
  corpus: string;
  cut: string;
  out: string;
  datingRule: boolean;
  caseId: string | undefined;
  seed: number | undefined;
}

/** How `evidenceFor` is told where the cut is, and whether the dating rule is in force. */
export interface EvidenceOptions {
  /** The cut, as an ISO 8601 time or a `Date`. A case answered at or after it is never evidence. */
  cut: string | Date;
  /** False drops the time test: the negative control, which lets later decisions through. */
  datingRule?: boolean;
}

/** The two sides of the cut, as the lists of case ids written to `--out`. */
interface Split {
  before: CaseRow[];
  after: CaseRow[];
}

/** When a case was answered, in milliseconds. */
function timeOf(row: CaseRow): number {
  const time = Date.parse(row.answeredAt);
  if (Number.isNaN(time)) {
    throw new CorpusError(`case ${row.caseId} was answered at ${row.answeredAt}, which is no time`);
  }
  return time;
}

function cutTime(cut: string | Date): number {
  const time = cut instanceof Date ? cut.getTime() : Date.parse(cut);
  if (Number.isNaN(time)) {
    throw new UsageError(`--cut takes an ISO 8601 time, and ${String(cut)} is not one`);
  }
  return time;
}

/**
 * The cases of `corpus` that may be shown when `caseRow` is judged: every case answered
 * strictly earlier than it and on the training side of the cut. The case itself is never its
 * own evidence. With `datingRule: false` the time test is dropped and the whole training side
 * comes through, later decisions included, which is the negative control and not a recipe.
 */
export function evidenceFor(
  corpus: readonly CaseRow[],
  caseRow: CaseRow,
  options: EvidenceOptions
): CaseRow[] {
  const cut = cutTime(options.cut);
  const datingRule = options.datingRule !== false;
  const judgedAt = timeOf(caseRow);
  const evidence: CaseRow[] = [];
  for (const row of corpus) {
    if (row.caseId === caseRow.caseId) continue;
    const at = timeOf(row);
    if (at >= cut) continue;
    if (datingRule && at >= judgedAt) continue;
    evidence.push(row);
  }
  return evidence;
}

function splitOf(corpus: Corpus, cut: number): Split {
  const before: CaseRow[] = [];
  const after: CaseRow[] = [];
  for (const row of corpus.rows) (timeOf(row) < cut ? before : after).push(row);
  return { before, after };
}

function countOf(rows: readonly CaseRow[], kind: CaseKind): number {
  return rows.filter((row) => row.kind === kind).length;
}

function sideBySide(split: Split): string {
  const row = (label: string, before: number, after: number): string =>
    `  ${label.padEnd(16)}${String(before).padStart(8)}${String(after).padStart(10)}`;
  return [
    `  ${"".padEnd(16)}${"before".padStart(8)}${"after".padStart(10)}`,
    row("cases", split.before.length, split.after.length),
    row("  approval", countOf(split.before, "approval"), countOf(split.after, "approval")),
    row("  question", countOf(split.before, "question"), countOf(split.after, "question")),
  ].join("\n");
}

function readFlags(argv: readonly string[]): Flags {
  let values: {
    corpus?: string;
    cut?: string;
    out?: string;
    "no-dating-rule"?: boolean;
    case?: string;
    seed?: string;
  };
  try {
    ({ values } = parseArgs({
      args: [...argv],
      options: {
        corpus: { type: "string" },
        cut: { type: "string" },
        out: { type: "string" },
        "no-dating-rule": { type: "boolean" },
        case: { type: "string" },
        seed: { type: "string" },
      },
      strict: true,
      allowPositionals: false,
    }));
  } catch {
    // parseArgs names what it refused, so its message is never repeated.
    throw new UsageError(USAGE);
  }
  const corpus = values.corpus?.trim();
  const cut = values.cut?.trim();
  const out = values.out?.trim();
  if (!corpus || !cut || !out) throw new UsageError(USAGE);
  let seed: number | undefined;
  if (values.seed !== undefined) {
    seed = Number(values.seed.trim());
    if (!Number.isSafeInteger(seed) || seed < 0) {
      throw new UsageError(`--seed takes a whole number, zero or above\n\n${USAGE}`);
    }
  }
  return {
    corpus,
    cut,
    out,
    datingRule: values["no-dating-rule"] !== true,
    caseId: values.case?.trim(),
    seed,
  };
}

/** The case the evidence counts are shown for: the one named, or one of the training side. */
function caseToShow(corpus: Corpus, split: Split, flags: Flags): CaseRow {
  if (flags.caseId !== undefined) {
    const row = corpus.byCaseId.get(flags.caseId);
    if (row === undefined)
      throw new UsageError(`${corpus.directory} holds no case ${flags.caseId}`);
    return row;
  }
  // The training side is where the two differ: for a case past the cut, the cut already leaves
  // out every case answered later, so the rule and the control allow the very same cases.
  const from = split.before.length > 0 ? split.before : corpus.rows;
  const seed = flags.seed ?? newSeed();
  const row = pickAtRandom(from, seed);
  if (row === undefined) throw new CorpusError(`${corpus.directory} holds no case`);
  console.log(`seed ${seed} (--seed ${seed} picks this case again)`);
  return row;
}

async function split(flags: Flags): Promise<number> {
  const corpus = await readCorpus(flags.corpus);
  const directory = await outDirectory(flags.out);
  const cut = cutTime(flags.cut);
  const sides = splitOf(corpus, cut);
  console.log(`corpus            ${corpus.directory}`);
  console.log(`cut               ${new Date(cut).toISOString()}`);
  console.log(`rule in force     ${flags.datingRule ? "the dating rule" : "no dating rule"}`);
  console.log("");
  console.log(sideBySide(sides));
  console.log("");
  await mkdir(directory, { recursive: true });
  const write = async (name: string, rows: readonly CaseRow[]): Promise<void> => {
    const ids = rows.map((row) => row.caseId);
    await writeFile(join(directory, name), `${JSON.stringify(ids, null, 2)}\n`, "utf8");
  };
  await write("before.json", sides.before);
  await write("after.json", sides.after);
  console.log(`written           ${join(directory, "before.json")}`);
  console.log(`                  ${join(directory, "after.json")}`);
  const shown = caseToShow(corpus, sides, flags);
  const options: EvidenceOptions = { cut: flags.cut };
  const byRule = evidenceFor(corpus.rows, shown, options);
  const byControl = evidenceFor(corpus.rows, shown, { ...options, datingRule: false });
  const ruled = new Set(byRule.map((row) => row.caseId));
  const letThrough = byControl.filter((row) => !ruled.has(row.caseId));
  console.log("");
  console.log(`evidence for      ${shown.caseId}`);
  console.log(`  answered        ${shown.answeredAt}`);
  console.log(`  side of the cut ${timeOf(shown) < cut ? "training" : "judged"}`);
  console.log(`  the dating rule allows   ${byRule.length}`);
  console.log(`  the control allows       ${byControl.length}`);
  console.log(`  the control lets through ${letThrough.length} the rule does not:`);
  for (const row of letThrough.slice(0, LET_THROUGH_SHOWN)) {
    console.log(`    ${row.caseId}  answered ${row.answeredAt}`);
  }
  if (letThrough.length > LET_THROUGH_SHOWN) {
    console.log(`    and ${letThrough.length - LET_THROUGH_SHOWN} more`);
  }
  return 0;
}

async function main(argv: readonly string[]): Promise<number> {
  let flags: Flags;
  try {
    flags = readFlags(argv);
  } catch (error) {
    if (!(error instanceof UsageError)) throw error;
    console.error(error.message);
    return 2;
  }
  try {
    return await split(flags);
  } catch (error) {
    if (
      error instanceof UsageError ||
      error instanceof OutputPathError ||
      error instanceof CorpusError
    ) {
      console.error(`${error.name}: ${error.message}`);
      return 2;
    }
    throw error;
  }
}

// Only when run: a recipe that imports `evidenceFor` must not run a command.
if (import.meta.main) process.exit(await main(Bun.argv.slice(2)));
