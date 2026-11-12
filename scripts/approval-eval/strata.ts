import { Buffer } from "node:buffer";
import { mkdir, writeFile } from "node:fs/promises";
import { join } from "node:path";
import { parseArgs } from "node:util";
import type { CaseRow } from "./build-corpus";
import {
  CannotRunError,
  count,
  counted,
  num,
  outDirectory,
  readCorpus,
  table,
  UsageError,
} from "./corpus-io";
import { corpusDrawPath, readDraw } from "./draw-io";
import { clopperPearsonUpper, DEFAULT_CONFIDENCE, MINIMUM_DECIDED } from "./stats";

/**
 * The ceiling an oracle could reach over the person's own approvals, from the corpus alone.
 *
 * The round answers "can an arm decide over an unselected stream", and its answer is no. This
 * answers the other question, which no arm is needed for: is there a stratum of the person's
 * requests - one an agent could name before deciding, from facts of the request itself and never
 * from the answer - where the person asks for changes less than once in twenty times. Where such
 * a stratum exists, an arm that approved it and nothing else would hold the rate the person
 * asked for, so the rate of the stratum is the ceiling any arm over it could reach. Where none
 * exists, no selection rule of these facts can reach that rate however good the model is, and
 * the lowest bound any stratum reaches is the strength of that negative.
 *
 * Every number here is a pass over `corpus.jsonl`. It reads no round, no judgement and no model,
 * and makes no call. The bound is the one-sided Clopper-Pearson upper bound of `stats.ts` at 90
 * percent, over the request-changes count of the stratum, and a stratum under `MINIMUM_DECIDED`
 * cases carries its count and no bound.
 *
 * The strata are cut from what a request carries before it is answered: its project, whether it
 * is of a first version, whether changes had already been asked for on that document, how much
 * that document already carries, how many documents the request covers, how large the request
 * is, the hour in the person's own time zone, how much the person had already answered that day,
 * and the ask's urgency. Every pair of those is crossed as well, and a crossing is reported
 * where it reaches a lower bound than either band it was made of.
 *
 * The reading is made twice: over the whole corpus, and over the part of it after the cut the
 * draw used, since a rate that holds only before the cut says nothing about what ships.
 */

const USAGE = `usage: bun scripts/approval-eval/strata.ts --corpus <dir> [--cut <date>] [--out <dir>]

Prints the person's request-changes rate in every stratum of their approval requests that an
agent could name before deciding, with a one-sided 90 percent upper bound and the share of the
approval volume the stratum carries.

Options:
  --corpus <dir>   Required. A directory holding corpus.jsonl and snapshot.json.
  --cut <date>     An ISO 8601 time. The second reading is over the cases answered at or after
                   it. Where no --cut is given, the cut of <corpus>/slices/draw.json is used.
  --out <dir>      Where strata.json is written. It must be an absolute path outside this
                   repository's workspace. Nothing is written where no --out is given.`;

/** The run printed its reading. */
const EXIT_PRINTED = 0;

/** The run could not start at all. */
const EXIT_CANNOT_RUN = 2;

/** The rate the question is asked at: one in twenty. */
export const RATE = 0.05;

/** The question every table of this reading answers, in the words it is asked in. */
export const QUESTION =
  "Does any identifiable stratum hold a request-changes rate under 5 percent, and what share " +
  "of the volume is in it?";

/** How many crossings are printed, the lowest bound first, of however many were found. */
const CROSSINGS_PRINTED = 15;

/** One cut of the approval cases: a name, a line saying what it is, and the band of a case. */
export interface StratumCut {
  name: string;
  /** What the cut is and why it is one a request carries before it is answered. */
  about: string;
  /** The bands in the order the table prints them. An absent order prints by count, largest
   * first, which is what a cut whose bands are data (the projects) takes. */
  order?: readonly string[];
  /** Which band this case falls in, and null for a case the cut cannot place. */
  bandOf: (row: CaseRow) => string | null;
}

/** One stratum as the table prints it: how often the person asked for changes, and how much. */
export interface StratumLine {
  name: string;
  cases: number;
  requestChanges: number;
  rate: number;
  /** The one-sided upper bound, and null for a stratum under `MINIMUM_DECIDED` cases. */
  bound: number | null;
  /** The share of this reading's approval volume the stratum carries. */
  share: number;
}

/** One cut's whole table, with the answer the question takes over it. */
export interface StrataTable {
  name: string;
  about: string;
  lines: StratumLine[];
  answer: string;
}

/** One reading: the whole corpus, or the part of it after the cut. */
export interface StrataReading {
  name: string;
  about: string;
  /** The approval cases the reading is over. */
  cases: number;
  requestChanges: number;
  rate: number;
  bound: number | null;
  tables: StrataTable[];
  /** Every pair of bands that reaches a lower bound than either band it was made of. */
  crossings: StrataTable;
  /** The answer over every stratum of this reading, the crossings included. */
  answer: string;
}

/** What one run read and answered, as `strata.json` holds it. */
export interface StrataRecord {
  corpus: string;
  person: string;
  cut: string | null;
  /** Where the cut came from: a flag, the draw of this corpus, or nowhere. */
  cutFrom: string;
  approvalCases: number;
  /** Approval cases carrying no verdict of the person, which no rate is counted over. */
  withoutVerdict: number;
  readings: StrataReading[];
  ranAt: string;
}

/** The flags as the command reads them. */
interface Flags {
  corpus: string;
  cut: string | null;
  out: string | null;
}

/** The band an hour of the person's own day falls in. */
function hourBand(hour: number): string {
  if (hour < 7) return "00 to 06, the night";
  if (hour < 11) return "07 to 10, the morning";
  if (hour < 14) return "11 to 13, the middle of the day";
  if (hour < 18) return "14 to 17, the afternoon";
  return "18 to 23, the evening";
}

/** The band a count of the day's earlier answers falls in. */
function answeredBand(answered: number): string {
  if (answered === 0) return "none yet that day";
  if (answered < 10) return "1 to 9 that day";
  if (answered < 25) return "10 to 24 that day";
  if (answered < 50) return "25 to 49 that day";
  if (answered < 100) return "50 to 99 that day";
  return "100 and more that day";
}

/** The band a request's size falls in. */
function bytesBand(bytes: number): string {
  if (bytes < 400) return "under 400 bytes";
  if (bytes < 700) return "400 to 699 bytes";
  if (bytes < 1100) return "700 to 1,099 bytes";
  return "1,100 bytes and more";
}

/**
 * Every cut the reading is made by. Each reads facts of the request alone: a judge could place a
 * case in its band with the person's answer taken away, which is what makes the stratum one an
 * agent could select on rather than one read backwards from the decision.
 */
export const CUTS: readonly StratumCut[] = [
  {
    name: "project",
    about: "The project the request's issue belongs to.",
    bandOf: (row) => row.project ?? "no project",
  },
  {
    name: "version",
    about: "Whether the request is of a first version of its document or of a later one.",
    order: ["a first version", "a later version", "no version"],
    bandOf: (row) => {
      const version = row.confounds.version;
      if (version === null) return "no version";
      return version <= 1 ? "a first version" : "a later version";
    },
  },
  {
    name: "changes already requested",
    about: "Whether the person had already asked for changes on this document.",
    order: ["changes already requested", "no changes requested yet"],
    bandOf: (row) =>
      row.confounds.changesAlreadyRequested
        ? "changes already requested"
        : "no changes requested yet",
  },
  {
    name: "earlier cases on this document",
    about: "How many earlier decisions of the person this document already carries.",
    order: ["none", "one", "two and more"],
    bandOf: (row) => {
      const prior = row.confounds.priorCasesOnDocument;
      if (prior <= 0) return "none";
      return prior === 1 ? "one" : "two and more";
    },
  },
  {
    name: "documents covered",
    about: "How many documents the request asks the person to approve at once.",
    order: ["one document", "two documents", "three and more", "no document"],
    bandOf: (row) => {
      const documents = row.approval?.documents.length ?? 0;
      if (documents <= 0) return "no document";
      if (documents === 1) return "one document";
      return documents === 2 ? "two documents" : "three and more";
    },
  },
  {
    name: "request bytes",
    about:
      "How large the request is. The corpus carries no document body, so this is the bytes of " +
      "the request's own words, which is what it carries of the lead document.",
    order: ["under 400 bytes", "400 to 699 bytes", "700 to 1,099 bytes", "1,100 bytes and more"],
    bandOf: (row) => bytesBand(Buffer.byteLength(row.question ?? "", "utf8")),
  },
  {
    name: "hour of the day where the person is",
    about: "The hour the person answered in, in their own time zone.",
    order: [
      "00 to 06, the night",
      "07 to 10, the morning",
      "11 to 13, the middle of the day",
      "14 to 17, the afternoon",
      "18 to 23, the evening",
    ],
    bandOf: (row) => hourBand(row.confounds.hourPacific),
  },
  {
    name: "decisions already answered that day",
    about: "How many decisions the person had already answered on the day this one reached them.",
    order: [
      "none yet that day",
      "1 to 9 that day",
      "10 to 24 that day",
      "25 to 49 that day",
      "50 to 99 that day",
      "100 and more that day",
    ],
    bandOf: (row) => answeredBand(row.confounds.answersThatDayBefore),
  },
  {
    name: "urgency",
    about:
      "The corpus carries no kind of its own for the issue, so the nearest fact it carries of " +
      "what sort of request this is is the urgency the ask was raised at.",
    bandOf: (row) => row.urgency ?? "no urgency",
  },
];

/** The bound as a cell, or the words that say why a stratum this small carries none. */
function bound(line: StratumLine): string {
  if (line.bound !== null) return `at most ${num(line.bound)}`;
  return `no bound at this count (${counted(line.cases, "case")}, ${MINIMUM_DECIDED} needed)`;
}

/** One stratum read: the person's request-changes rate over it, bounded where it is large enough. */
export function stratumLine(
  name: string,
  cases: readonly CaseRow[],
  volume: number,
  confidence: number = DEFAULT_CONFIDENCE
): StratumLine {
  const requestChanges = cases.filter((row) => row.verdict === "request_changes").length;
  return {
    name,
    cases: cases.length,
    requestChanges,
    rate: cases.length === 0 ? 0 : requestChanges / cases.length,
    bound:
      cases.length >= MINIMUM_DECIDED
        ? clopperPearsonUpper(requestChanges, cases.length, confidence)
        : null,
    share: volume === 0 ? 0 : cases.length / volume,
  };
}

/** The cases of each band of one cut, in the order the cut's table prints them. */
function bandsOf(cut: StratumCut, cases: readonly CaseRow[]): Map<string, CaseRow[]> {
  const bands = new Map<string, CaseRow[]>();
  for (const row of cases) {
    const band = cut.bandOf(row);
    if (band === null) continue;
    const held = bands.get(band);
    if (held === undefined) bands.set(band, [row]);
    else held.push(row);
  }
  if (cut.order === undefined) {
    return new Map([...bands].sort((one, two) => two[1].length - one[1].length));
  }
  const order = cut.order;
  return new Map(
    [...bands].sort((one, two) => {
      const first = order.indexOf(one[0]);
      const second = order.indexOf(two[0]);
      return (first === -1 ? order.length : first) - (second === -1 ? order.length : second);
    })
  );
}

/** What the strata of one answer are called, in the one and in the many. */
export interface StratumNames {
  one: string;
  many: string;
}

/**
 * The sentence an answer ends with where some of its lines hold too few cases to carry a bound:
 * how many of them there are and why they carry none, and nothing at all where every line
 * carries one. `consensus.ts` ends its own answer with this same sentence, over its subsets, so
 * this tail is worded in one place rather than two. `MINIMUM_DECIDED` itself is read wherever a
 * bound is decided, here and there both.
 */
export function noBoundTail(lines: readonly { bound: number | null }[]): string {
  const small = lines.filter((line) => line.bound === null).length;
  if (small === 0) return "";
  return (
    ` ${count(small)} of ${count(lines.length)} ${small === 1 ? "holds" : "hold"} fewer than ` +
    `${MINIMUM_DECIDED} cases and so ${small === 1 ? "carries" : "carry"} no bound.`
  );
}

/**
 * The answer the question takes over a set of strata: the ones that hold the rate, or, where
 * none does, the lowest bound any of them reaches and the count behind it, since that is the
 * whole strength of the negative.
 */
export function answerOf(lines: readonly StratumLine[], what: StratumNames): string {
  const under = lines
    .filter((line) => line.bound !== null && line.bound < RATE)
    .sort((one, two) => two.share - one.share);
  const tail = noBoundTail(lines);
  if (under.length > 0) {
    const said = under
      .map(
        (line) =>
          `${line.name} (${count(line.cases)} cases, ${count(line.requestChanges)} of them ` +
          `request changes, rate ${num(line.rate)}, bound at most ${num(line.bound ?? 0)}, ` +
          `${num(line.share)} of the approval volume)`
      )
      .join("; ");
    return (
      `Yes. ${count(under.length)} of ${count(lines.length)} ${what.many} hold a ` +
      `request-changes rate under 5 percent by their bound: ${said}.${tail}`
    );
  }
  const bounded = lines.filter((line) => line.bound !== null);
  if (bounded.length === 0) {
    return (
      `No. No ${what.one} holds ${MINIMUM_DECIDED} cases, so none of them carries a bound ` +
      `at all.${tail}`
    );
  }
  const lowest = bounded.reduce((one, two) => ((two.bound ?? 1) < (one.bound ?? 1) ? two : one));
  return (
    `No. No ${what.one} holds a request-changes rate under 5 percent by its bound. The lowest ` +
    `bound any of them reaches is ${num(lowest.bound ?? 1)}, over ${lowest.name}, at ` +
    `${count(lowest.cases)} cases of which ${count(lowest.requestChanges)} request changes ` +
    `(rate ${num(lowest.rate)}, ${num(lowest.share)} of the approval volume).${tail}`
  );
}

/** One cut's table: a line per band, and the answer the question takes over them. */
export function strataTable(
  cut: StratumCut,
  cases: readonly CaseRow[],
  volume: number
): StrataTable {
  const lines = [...bandsOf(cut, cases)].map(([band, rows]) => stratumLine(band, rows, volume));
  const answer = answerOf(lines, { one: `band of ${cut.name}`, many: `bands of ${cut.name}` });
  return { name: cut.name, about: cut.about, lines, answer };
}

/**
 * Every pair of bands of two different cuts that holds `MINIMUM_DECIDED` cases and reaches a
 * lower bound than either band it was made of. A crossing that beats neither of its own bands is
 * not a stratum worth naming: the single cut already said it.
 */
export function crossings(
  cuts: readonly StratumCut[],
  cases: readonly CaseRow[],
  volume: number
): StrataTable {
  const byCut = cuts.map((cut) => ({
    cut,
    bands: [...bandsOf(cut, cases)].map(([band, rows]) => ({
      band,
      rows,
      line: stratumLine(band, rows, volume),
    })),
  }));
  const lines: StratumLine[] = [];
  for (let first = 0; first < byCut.length; first += 1) {
    for (let second = first + 1; second < byCut.length; second += 1) {
      const one = byCut[first];
      const two = byCut[second];
      if (one === undefined || two === undefined) continue;
      for (const left of one.bands) {
        const held = new Set(left.rows.map((row) => row.caseId));
        for (const right of two.bands) {
          const both = right.rows.filter((row) => held.has(row.caseId));
          if (both.length < MINIMUM_DECIDED) continue;
          const name = `${one.cut.name} = ${left.band} + ${two.cut.name} = ${right.band}`;
          const line = stratumLine(name, both, volume);
          const cross = line.bound ?? 1;
          if (cross >= (left.line.bound ?? 1) || cross >= (right.line.bound ?? 1)) continue;
          lines.push(line);
        }
      }
    }
  }
  lines.sort((one, two) => (one.bound ?? 1) - (two.bound ?? 1) || two.cases - one.cases);
  return {
    name: "crossings",
    about:
      "Every pair of bands of two different cuts that holds " +
      `${MINIMUM_DECIDED} cases and reaches a lower bound than either band it was made of, the ` +
      `lowest bound first. ${counted(lines.length, "pair does", "pairs do")}` +
      `, of which the first ${Math.min(CROSSINGS_PRINTED, lines.length)} are printed.`,
    lines,
    answer: answerOf(lines, { one: "crossing", many: "crossings" }),
  };
}

/** One reading over one set of approval cases: every cut, every crossing, and the answer. */
export function readStrata(
  name: string,
  about: string,
  cases: readonly CaseRow[],
  cuts: readonly StratumCut[] = CUTS
): StrataReading {
  const volume = cases.length;
  const whole = stratumLine("every approval case", cases, volume);
  const tables = cuts.map((cut) => strataTable(cut, cases, volume));
  const crossed = crossings(cuts, cases, volume);
  const every = [...tables.flatMap((table) => table.lines), ...crossed.lines];
  return {
    name,
    about,
    cases: whole.cases,
    requestChanges: whole.requestChanges,
    rate: whole.rate,
    bound: whole.bound,
    tables,
    crossings: crossed,
    answer: answerOf(every, { one: "stratum", many: "strata" }),
  };
}

/** The approval cases a rate can be counted over: the person answered them one way or the other. */
export function approvalCases(rows: readonly CaseRow[]): CaseRow[] {
  return rows.filter(
    (row) =>
      row.kind === "approval" && (row.verdict === "approve" || row.verdict === "request_changes")
  );
}

/** One table of one reading, with the question it answers printed over its answer. */
function tableLines(one: StrataTable, printed: number): string[] {
  return [
    `### ${one.name}`,
    "",
    one.about,
    "",
    QUESTION,
    "",
    one.answer,
    "",
    ...(one.lines.length === 0
      ? ["No case falls in any band of this cut."]
      : table(
          ["stratum", "cases", "request changes", "rate", "bound", "share of volume"],
          one.lines
            .slice(0, printed)
            .map((line) => [
              line.name,
              count(line.cases),
              count(line.requestChanges),
              num(line.rate),
              bound(line),
              num(line.share),
            ])
        )),
    "",
  ];
}

/** One whole reading, in the order it is printed. */
function readingLines(reading: StrataReading): string[] {
  return [
    `## ${reading.name}`,
    "",
    reading.about,
    "",
    `${count(reading.cases)} approval cases, of which ${count(reading.requestChanges)} asked ` +
      `for changes: a rate of ${num(reading.rate)}` +
      `${reading.bound === null ? "" : `, bound at most ${num(reading.bound)}`}. Every share of ` +
      "volume below is a share of those cases.",
    "",
    "Over every stratum of this reading, the crossings included:",
    "",
    QUESTION,
    "",
    reading.answer,
    "",
    ...reading.tables.flatMap((one) => tableLines(one, Number.POSITIVE_INFINITY)),
    ...tableLines(reading.crossings, CROSSINGS_PRINTED),
  ];
}

/** The whole record, in the order it is printed and written. */
export function renderStrata(record: StrataRecord): string[] {
  return [
    "# The ceiling an oracle could reach, from the corpus alone",
    "",
    `corpus            ${record.corpus}`,
    `person            ${record.person}`,
    `cut               ${record.cut ?? "none"} (${record.cutFrom})`,
    `approval cases    ${count(record.approvalCases)}`,
    `without a verdict ${count(record.withoutVerdict)}, which no rate below is counted over`,
    `bound             one-sided Clopper-Pearson at ${num(100 * DEFAULT_CONFIDENCE, 0)} percent`,
    `read at           ${record.ranAt}`,
    "",
    "This reading is a pass over the corpus's own rows. It reads no round, no judgement and no " +
      "model: for each stratum an agent could name before deciding, how often the person asked " +
      "for changes in it, and how much of their approval volume it carries.",
    "",
    ...record.readings.flatMap(readingLines),
  ];
}

function readFlags(argv: readonly string[]): Flags {
  let values: { corpus?: string; cut?: string; out?: string };
  try {
    ({ values } = parseArgs({
      args: [...argv],
      options: {
        corpus: { type: "string" },
        cut: { type: "string" },
        out: { type: "string" },
      },
      strict: true,
      allowPositionals: false,
    }));
  } catch {
    // parseArgs names what it refused, so its message is never repeated.
    throw new UsageError(USAGE);
  }
  const corpus = values.corpus?.trim();
  if (!corpus) throw new UsageError(USAGE);
  const cut = values.cut?.trim() || null;
  if (cut !== null && Number.isNaN(Date.parse(cut))) {
    throw new UsageError(`--cut takes an ISO 8601 time, and ${cut} is no time\n\n${USAGE}`);
  }
  return { corpus, cut, out: values.out?.trim() || null };
}

/** The record one run makes: the whole corpus, and the part of it after the cut. */
export function strataRecord(
  rows: readonly CaseRow[],
  options: { corpus: string; person: string; cut: string | null; cutFrom: string; ranAt: string }
): StrataRecord {
  const cases = approvalCases(rows);
  const readings = [
    readStrata(
      "The whole corpus",
      "Every approval case the person answered, before the cut and after it.",
      cases
    ),
  ];
  if (options.cut !== null) {
    const at = Date.parse(options.cut);
    const post = cases.filter((row) => {
      const answered = Date.parse(row.answeredAt);
      return !Number.isNaN(answered) && answered >= at;
    });
    readings.push(
      readStrata(
        "After the cut",
        `The approval cases answered at or after ${options.cut}, which is the part of the ` +
          "corpus a rule shipped now would meet.",
        post
      )
    );
  }
  return {
    corpus: options.corpus,
    person: options.person,
    cut: options.cut,
    cutFrom: options.cutFrom,
    approvalCases: cases.length,
    withoutVerdict: rows.filter((row) => row.kind === "approval").length - cases.length,
    readings,
    ranAt: options.ranAt,
  };
}

/** The run as the command makes it: the real corpus, and nothing else read. */
async function strata(flags: Flags): Promise<number> {
  const out = flags.out === null ? null : await outDirectory(flags.out);
  const corpus = await readCorpus(flags.corpus);
  // The cut the draw of this corpus used, where the corpus carries a draw that names one.
  const drawn =
    flags.cut === null ? ((await readDraw(corpusDrawPath(corpus.directory)))?.cut ?? null) : null;
  const cut = flags.cut ?? drawn;
  const cutFrom =
    flags.cut !== null
      ? "--cut"
      : drawn !== null
        ? "the cut of slices/draw.json"
        : "no --cut and no draw in this corpus, so only the whole corpus is read";
  const record = strataRecord(corpus.rows, {
    corpus: corpus.directory,
    person: corpus.snapshot.person,
    cut,
    cutFrom,
    ranAt: new Date().toISOString(),
  });
  for (const line of renderStrata(record)) console.log(line);
  if (out !== null) {
    await mkdir(out, { recursive: true });
    const path = join(out, "strata.json");
    await writeFile(path, `${JSON.stringify(record, null, 2)}\n`, "utf8");
    console.log("");
    console.log(`written           ${path}`);
  }
  return EXIT_PRINTED;
}

/** The command: the flags, the reading, and the exit status it takes. */
export async function main(argv: readonly string[]): Promise<number> {
  let flags: Flags;
  try {
    flags = readFlags(argv);
  } catch (error) {
    if (!(error instanceof UsageError)) throw error;
    console.error(error.message);
    return EXIT_CANNOT_RUN;
  }
  try {
    return await strata(flags);
  } catch (error) {
    if (error instanceof CannotRunError) {
      console.error(`${error.name}: ${error.message}`);
      return EXIT_CANNOT_RUN;
    }
    throw error;
  }
}

// Only when run: a test that imports the reading must not run a command.
if (import.meta.main) process.exit(await main(Bun.argv.slice(2)));
