/** `bun run test:repeat --lane <web|web-cold|harness> [--reruns <k>] [--seed <n>] --out <dir>`:
 *  runs one lane of the Dispatch unit suite with every test run `k` times, and reports each row
 *  that failed (INFRA-1511).
 *
 *    bun run test:repeat --lane web --out /tmp/repeat-web
 *    bun run test:repeat --lane web-cold --reruns 1 --out /tmp/repeat-web-cold
 *    bun run test:repeat --lane harness --reruns 5 --seed 1234 --out /tmp/repeat-harness
 *
 *  The web lane is `bun run test:web web/src`. The web-cold lane is
 *  `bun run test:web:cold web/src`: the web lane with every module the web app loads lazily held
 *  back on each load (web/src/__tests__/hold-lazy-loads.ts). A rerun of a file keeps the modules
 *  its first run loaded, so only the first run of each file is cold: run that lane with
 *  `--reruns 1`. The harness lane is `bun run test:harness e2e/__tests__`. The tool runs the lane
 *  from this package's directory, whatever directory it starts in. It adds `--seed`,
 *  `--rerun-each` and Bun's junit reporter. It prints the seed before the run starts, so a run
 *  killed midway still names its seed. During the run it prints only Bun's summary lines. Bun's
 *  whole output goes to `<out>/bun.log`.
 *
 *  `<out>` then holds `junit.xml`, `bun.log`, `report.json` and `report.md`. Where
 *  `GITHUB_STEP_SUMMARY` is set, `report.md` is also appended to the file it names. Under GitHub
 *  Actions the tool prints one `::error` per failure that belongs to no row, then one per failing
 *  row, and then one `::notice` with the totals. GitHub keeps at most 10 errors a step, so when
 *  there are more, the tenth error says how many more failures report.md lists.
 *
 *  While Bun runs, the tool reads MemTotal and MemAvailable from /proc/meminfo, once at the start
 *  and then every 5 s. The report gives MemTotal, the lowest MemAvailable, and the difference,
 *  which is the most memory in use that a reading saw. That is the whole machine's memory: it
 *  counts every process on the machine, not Bun's alone. Where /proc/meminfo cannot be read, as
 *  on a machine that is not Linux, the report says that memory was not measured.
 *
 *  The reader reads the junit file Bun 1.4.4 writes, and only that grammar. It was measured on
 *  real runs:
 *
 *  - one top-level `<testsuite>` per file. Its `file` is the path from the directory Bun ran in,
 *    which is packages/dispatch for every lane;
 *  - one nested `<testsuite>` per `describe`. A rerun may open it again or add to the one before,
 *    so the reader knows a row by its describe names, not by the element;
 *  - every rerun of a file in that file's one top-level suite, after the run before it, so the
 *    runs of a row are its `<testcase>`s in document order;
 *  - `<failure message=...>` or `<skipped/>` in a `<testcase>` (a todo row is skipped too);
 *  - `<properties>` in each suite under GitHub Actions, which the reader passes over;
 *  - no junit file at all when no testcase ran.
 *
 *  A file that fails to load leaves no suite in the junit file. Bun counts it on a ` N error`
 *  line of its summary, and the report names that count as a failure that belongs to no row.
 *
 *  A run that read nothing is refused, not reported as clean. The exit status is 1 when a row
 *  failed, when Bun exited non-zero, when no testcase was read, or when a failure belongs to no
 *  row. It is 2 for a bad command line, and 0 otherwise. An `--out` that cannot be the output
 *  directory - a file, a path under a file, a place the tool may not write - is a bad command
 *  line too, refused before Bun starts. e2e/__tests__/unit-repeat.test.ts runs real Bun and
 *  checks the reader and the report on the files Bun writes.
 */
import {
  appendFileSync,
  closeSync,
  mkdirSync,
  openSync,
  readFileSync,
  rmSync,
  writeFileSync,
  writeSync,
} from "node:fs";
import { join, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { parseArgs } from "node:util";

export type Lane = "web" | "web-cold" | "harness";

interface LaneScript {
  /** The script of package.json that runs the lane. */
  readonly script: string;
  /** The directory the lane tests. */
  readonly target: string;
}

const lanes: Readonly<Record<Lane, LaneScript>> = {
  web: { script: "test:web", target: "web/src" },
  "web-cold": { script: "test:web:cold", target: "web/src" },
  harness: { script: "test:harness", target: "e2e/__tests__" },
};

const laneNames = Object.keys(lanes);

/** Whether `text` is the name of a lane. */
function isLane(text: string): text is Lane {
  return Object.hasOwn(lanes, text);
}

/** This package's directory. Every lane runs from it. */
const packageDirectory = fileURLToPath(new URL("../..", import.meta.url));

/** The largest seed Bun 1.4.4 accepts. Its `--seed` takes a decimal number from 0 to 2^32 - 1. */
const MAX_SEED = 2 ** 32 - 1;
const DEFAULT_RERUNS = 20;
/** How many characters of a failure's message the report keeps. */
const MESSAGE_LIMIT = 500;
/** How many lines of bun.log the report shows when a failure belongs to no row. */
const LOG_TAIL_LINES = 40;
/** The most `::error` annotations GitHub keeps for one step. GitHub's REST "Check runs" page says:
 *  "GitHub Actions are limited to 10 warning annotations and 10 error annotations per step". */
const ERROR_LIMIT = 10;
/** How often the tool reads /proc/meminfo while Bun runs. */
const MEMORY_SAMPLE_MS = 5_000;

/** The value `--lane` takes, as the usage writes it: `<web|web-cold|harness>`. */
const laneChoice = `<${laneNames.join("|")}>`;

/** The lanes as a refusal names them: `web, web-cold or harness`. */
const laneList = `${laneNames.slice(0, -1).join(", ")} or ${laneNames.at(-1)}`;

const usage =
  `usage: bun run test:repeat --lane ${laneChoice} [--reruns <k>] [--seed <n>] --out <dir>\n` +
  `  --lane ${laneChoice}\n                        ` +
  Object.entries(lanes)
    .map(([lane, { script, target }]) => `${lane} runs \`bun run ${script} ${target}\``)
    .join(";\n                        ") +
  "\n" +
  `  --reruns <k>          how many times Bun runs each test, at least 1 (default ${DEFAULT_RERUNS})\n` +
  `  --seed <n>            the seed of Bun's random order, from 0 to ${MAX_SEED}\n` +
  "                        (default: a random one)\n" +
  "  --out <dir>           where junit.xml, bun.log, report.json and report.md go;\n" +
  "                        made if missing. A relative path is read from the directory the\n" +
  "                        tool runs in, which `bun run` makes packages/dispatch.";

export interface RepeatOptions {
  readonly lane: Lane;
  readonly reruns: number;
  readonly seed: number;
  readonly out: string;
}

/** The options of a good command line, or what is wrong with a bad one. */
export type Parsed = { readonly options: RepeatOptions } | { readonly problem: string };

/** A random seed from the whole range Bun accepts. */
function randomSeed(): number {
  return crypto.getRandomValues(new Uint32Array(1))[0];
}

/** The number `text` spells in decimal digits, or undefined for anything else. */
function wholeNumber(text: string): number | undefined {
  if (!/^[0-9]+$/.test(text)) return undefined;
  const value = Number(text);
  return Number.isSafeInteger(value) ? value : undefined;
}

function errorText(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}

/** Reads the command line. `seed` gives the seed when the command line names none. */
export function parseArguments(argv: readonly string[], seed: () => number = randomSeed): Parsed {
  let values: { lane?: string; reruns?: string; seed?: string; out?: string };
  try {
    ({ values } = parseArgs({
      args: [...argv],
      options: {
        lane: { type: "string" },
        out: { type: "string" },
        reruns: { type: "string" },
        seed: { type: "string" },
      },
      strict: true,
    }));
  } catch (error) {
    return { problem: errorText(error) };
  }
  const { lane, out } = values;
  if (lane === undefined) return { problem: "--lane is missing" };
  if (!isLane(lane)) {
    return { problem: `--lane is ${laneList}, not ${JSON.stringify(lane)}` };
  }
  if (out === undefined || out === "") return { problem: "--out is missing" };
  const reruns = wholeNumber(values.reruns ?? String(DEFAULT_RERUNS));
  if (reruns === undefined || reruns < 1) {
    return { problem: `--reruns is a whole number of at least 1, not ${values.reruns}` };
  }
  const chosen = values.seed === undefined ? seed() : wholeNumber(values.seed);
  if (chosen === undefined || chosen > MAX_SEED) {
    return { problem: `--seed is a whole number from 0 to ${MAX_SEED}, not ${values.seed}` };
  }
  return { options: { lane, out, reruns, seed: chosen } };
}

export type Outcome = "passed" | "failed" | "skipped";

/** One run of one row, as one `<testcase>` of the junit file says it. */
export interface Testcase {
  readonly file: string;
  /** The names of the `describe`s the row is in, outermost first. */
  readonly describe: readonly string[];
  readonly name: string;
  readonly outcome: Outcome;
  /** The failure's message. It is empty unless the testcase failed. */
  readonly message: string;
}

export interface Junit {
  /** Each file the junit file holds a suite for, once, in document order. */
  readonly files: readonly string[];
  /** Every testcase, in document order. */
  readonly testcases: readonly Testcase[];
}

/** One tag of the junit file. */
interface Tag {
  readonly name: string;
  readonly attributes: ReadonlyMap<string, string>;
  /** `open` for `<name>`, `close` for `</name>`, `empty` for `<name/>`. */
  readonly kind: "open" | "close" | "empty";
}

const entities: Readonly<Record<string, string>> = {
  amp: "&",
  apos: "'",
  gt: ">",
  lt: "<",
  quot: '"',
};

/** An attribute value with its five named entities and its numeric references decoded. A `&`
 *  that starts none of them is refused. */
function decodeEntities(text: string): string {
  return text.replace(
    /&(?:#x([0-9A-Fa-f]+);|#([0-9]+);|(amp|apos|gt|lt|quot);)?/g,
    (_, hex?: string, decimal?: string, named?: string) => {
      if (named !== undefined) return entities[named];
      if (hex !== undefined) return String.fromCodePoint(Number.parseInt(hex, 16));
      if (decimal !== undefined) return String.fromCodePoint(Number(decimal));
      throw new Error(`an attribute value holds a bare &: ${JSON.stringify(text)}`);
    }
  );
}

const NAME = "[A-Za-z_][-A-Za-z0-9_.:]*";

/** Every tag of `xml`, in order. It skips the XML declaration and comments, and the text between
 *  tags, which Bun writes only as a failure's body. Bun quotes every attribute value with `"`. */
function* tags(xml: string): Generator<Tag> {
  const opening = new RegExp(`<(/?)(${NAME})`, "y");
  const attribute = new RegExp(`\\s+(${NAME})="([^"<]*)"`, "y");
  const ending = /\s*(\/?)>/y;
  let at = 0;
  for (;;) {
    const start = xml.indexOf("<", at);
    if (start === -1) return;
    const skip = xml.startsWith("<?", start) ? "?>" : xml.startsWith("<!--", start) ? "-->" : "";
    if (skip !== "") {
      const end = xml.indexOf(skip, start);
      if (end === -1) throw new Error(`the markup at offset ${start} never ends`);
      at = end + skip.length;
      continue;
    }
    opening.lastIndex = start;
    const open = opening.exec(xml);
    if (open === null) throw new Error(`no tag can start at offset ${start}`);
    at = opening.lastIndex;
    const attributes = new Map<string, string>();
    for (;;) {
      attribute.lastIndex = at;
      const pair = attribute.exec(xml);
      if (pair === null) break;
      attributes.set(pair[1], decodeEntities(pair[2]));
      at = attribute.lastIndex;
    }
    ending.lastIndex = at;
    const end = ending.exec(xml);
    if (end === null) throw new Error(`the tag <${open[2]}> at offset ${start} does not end`);
    at = ending.lastIndex;
    const closing = open[1] === "/";
    if (closing && (attributes.size > 0 || end[1] === "/")) {
      throw new Error(`the closing tag </${open[2]}> at offset ${start} is malformed`);
    }
    yield {
      name: open[2],
      attributes,
      kind: closing ? "close" : end[1] === "/" ? "empty" : "open",
    };
  }
}

function attributeOf(tag: Tag, name: string): string {
  const value = tag.attributes.get(name);
  if (value === undefined) throw new Error(`a <${tag.name}> has no ${name}`);
  return value;
}

interface OpenTestcase {
  file: string;
  describe: readonly string[];
  name: string;
  outcome: Outcome;
  message: string;
}

/** The files and testcases of a junit file Bun 1.4.4 wrote. It throws on anything else: a tag in
 *  a place Bun puts none, a tag left open, or a file with no `<testsuites>`. */
function readJunit(xml: string): Junit {
  const files = new Set<string>();
  const testcases: Testcase[] = [];
  const open: string[] = [];
  const describe: string[] = [];
  let file: string | undefined;
  let testcase: OpenTestcase | undefined;
  let rooted = false;

  const enter = (tag: Tag) => {
    const parent = open.at(-1);
    const misplaced = () => new Error(`a <${tag.name}> inside <${parent ?? "nothing"}>`);
    switch (tag.name) {
      case "testsuites":
        if (parent !== undefined || rooted) throw misplaced();
        rooted = true;
        break;
      case "testsuite":
        if (parent === "testsuites") {
          file = attributeOf(tag, "file");
          files.add(file);
        } else if (parent === "testsuite") {
          describe.push(attributeOf(tag, "name"));
        } else {
          throw misplaced();
        }
        break;
      case "testcase":
        if (parent !== "testsuite" || file === undefined) throw misplaced();
        testcase = {
          file: tag.attributes.get("file") ?? file,
          describe: [...describe],
          name: attributeOf(tag, "name"),
          outcome: "passed",
          message: "",
        };
        break;
      case "failure":
      case "skipped":
        if (parent !== "testcase" || testcase === undefined) throw misplaced();
        if (tag.name === "skipped") {
          if (testcase.outcome === "passed") testcase.outcome = "skipped";
        } else if (testcase.outcome !== "failed") {
          testcase.outcome = "failed";
          testcase.message = tag.attributes.get("message") ?? "";
        }
        break;
      default:
        // `<properties>`, its `<property>`s, and anything else Bun adds outside a row.
        if (parent === undefined) throw misplaced();
    }
    open.push(tag.name);
  };

  const leave = (name: string) => {
    const expected = open.pop();
    if (expected !== name) throw new Error(`a </${name}> closes <${expected ?? "nothing"}>`);
    if (name === "testcase" && testcase !== undefined) {
      testcases.push(testcase);
      testcase = undefined;
    } else if (name === "testsuite") {
      if (open.at(-1) === "testsuites") file = undefined;
      else describe.pop();
    }
  };

  for (const tag of tags(xml)) {
    if (tag.kind === "close") {
      leave(tag.name);
      continue;
    }
    enter(tag);
    if (tag.kind === "empty") leave(tag.name);
  }
  if (open.length > 0) throw new Error(`the <${open.at(-1)}> is never closed`);
  if (!rooted) throw new Error("the file holds no <testsuites>");
  return { files: [...files], testcases };
}

/** The junit file's text, or why there is none to read. */
export type JunitText = { readonly text: string } | { readonly unreadable: string };

/** Reads the junit file at `path`. */
export function junitAt(path: string): JunitText {
  try {
    return { text: readFileSync(path, "utf8") };
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === "ENOENT") {
      return { unreadable: `Bun wrote no junit file: ${path} is missing.` };
    }
    return { unreadable: `The junit file ${path} cannot be read: ${errorText(error)}` };
  }
}

/** What one run of a lane left behind. */
export interface LaneRun {
  readonly lane: Lane;
  /** What the lane tested: its directory, or the narrower path a smoke run gives `runLane`. */
  readonly target: string;
  readonly seed: number;
  readonly reruns: number;
  readonly wallMs: number;
  /** Bun's exit code. It is null when a signal ended Bun. */
  readonly exitCode: number | null;
  readonly signal: string | null;
  readonly junit: JunitText;
  /** Bun's whole stdout and stderr, as bun.log holds them. */
  readonly log: string;
  /** The variables GitHub Actions sets. `runLane` passes `process.env`. */
  readonly environment: Readonly<Record<string, string | undefined>>;
  /** The readings of /proc/meminfo taken while Bun ran. It is empty where none could be read. */
  readonly memory: readonly MemorySample[];
}

/** One reading of /proc/meminfo, in KiB, which the file writes as `kB`. */
export interface MemorySample {
  readonly memTotalKiB: number;
  readonly memAvailableKiB: number;
}

export interface GithubRun {
  readonly event: string | null;
  readonly commit: string | null;
  readonly runId: string | null;
  readonly attempt: string | null;
  readonly url: string | null;
}

export interface OneRun {
  readonly files: number;
  readonly rows: number;
  readonly skippedRows: number;
}

export interface Testcases {
  readonly passed: number;
  readonly failed: number;
  readonly skipped: number;
}

export interface FailingRow {
  readonly file: string;
  /** The names of the row's `describe`s and its own name, joined with ` > ` as Bun prints them. */
  readonly name: string;
  readonly fails: number;
  readonly runs: number;
  /** The run the row first failed on, counted from 1. */
  readonly firstFailedRun: number;
  /** The first failure's message, cut to MESSAGE_LIMIT characters. */
  readonly message: string;
  /** The command, run from packages/dispatch, that runs the row's file again in the same order. */
  readonly reproduce: string;
}

export type RowlessKind =
  | "bun-exit"
  | "bun-errors"
  | "junit-unreadable"
  | "no-testcase"
  | "uneven-file";

/** A failure that belongs to no row. */
export interface RowlessFailure {
  readonly kind: RowlessKind;
  readonly text: string;
}

/** The memory of the whole machine while Bun ran, in MiB. It counts every process on the
 *  machine, not Bun's alone. */
export interface MachineMemory {
  readonly memTotalMiB: number;
  /** The lowest MemAvailable of the readings. */
  readonly lowestAvailableMiB: number;
  /** MemTotal minus the lowest MemAvailable: the most memory in use that a reading saw. */
  readonly mostInUseMiB: number;
  /** How many readings of /proc/meminfo were taken. */
  readonly samples: number;
}

export interface Report {
  readonly github: GithubRun;
  readonly lane: Lane;
  readonly seed: number;
  readonly reruns: number;
  /** The lane's command, without the reporter flags. */
  readonly command: string;
  /** Bun's exit code, or the signal that ended it. */
  readonly bunExit: string;
  readonly wallSeconds: number;
  /** Each total divided by the number of reruns. */
  readonly oneRun: OneRun;
  /** The testcases of every run together. */
  readonly testcases: Testcases;
  readonly failingRows: readonly FailingRow[];
  readonly rowlessFailures: readonly RowlessFailure[];
  /** The last lines of bun.log. It is empty unless a failure belongs to no row. */
  readonly bunLogTail: readonly string[];
  /** The whole machine's memory while Bun ran. It is null where no reading could be taken. */
  readonly machineMemory: MachineMemory | null;
  readonly exitCode: 0 | 1;
}

function githubRun(environment: LaneRun["environment"]): GithubRun {
  const value = (name: string) => {
    const set = environment[name];
    return set === undefined || set === "" ? null : set;
  };
  const server = value("GITHUB_SERVER_URL");
  const repository = value("GITHUB_REPOSITORY");
  const runId = value("GITHUB_RUN_ID");
  return {
    event: value("GITHUB_EVENT_NAME"),
    commit: value("GITHUB_SHA"),
    runId,
    attempt: value("GITHUB_RUN_ATTEMPT"),
    url:
      server !== null && repository !== null && runId !== null
        ? `${server}/${repository}/actions/runs/${runId}`
        : null,
  };
}

/** A count of one run: a whole number, or two decimals when the reruns do not divide it. */
function perRun(total: number, reruns: number): number {
  return Math.round((total / reruns) * 100) / 100;
}

/** The number of errors outside any test that Bun's summary counts, from its last ` N error`
 *  line. */
function bunErrors(log: string): number {
  let count = 0;
  for (const [, errors] of log.matchAll(/^ (\d+) errors?$/gm)) count = Number(errors);
  return count;
}

/** The last `count` lines of `text`, without the empty one after a final newline. */
function lastLines(text: string, count: number): string[] {
  const lines = text.split("\n");
  if (lines.at(-1) === "") lines.pop();
  return lines.slice(-count);
}

/** The most memory in use that the readings saw: MemTotal minus the lowest MemAvailable, in MiB.
 *  It answers null for no reading, so that a run with no reading never shows a 0. */
function machineMemory(samples: readonly MemorySample[]): MachineMemory | null {
  if (samples.length === 0) return null;
  let lowest = samples[0];
  for (const sample of samples) {
    if (sample.memAvailableKiB < lowest.memAvailableKiB) lowest = sample;
  }
  const memTotalMiB = Math.round(lowest.memTotalKiB / 1024);
  const lowestAvailableMiB = Math.round(lowest.memAvailableKiB / 1024);
  return {
    memTotalMiB,
    lowestAvailableMiB,
    mostInUseMiB: memTotalMiB - lowestAvailableMiB,
    samples: samples.length,
  };
}

/** The report of one run of a lane. */
export function summarise(run: LaneRun): Report {
  const { script } = lanes[run.lane];
  const rowless: RowlessFailure[] = [];
  let junit: Junit = { files: [], testcases: [] };
  if ("unreadable" in run.junit) {
    rowless.push({ kind: "junit-unreadable", text: run.junit.unreadable });
  } else {
    try {
      junit = readJunit(run.junit.text);
      if (junit.testcases.length === 0) {
        rowless.push({
          kind: "no-testcase",
          text: "The junit file holds no testcase, so this run tested nothing.",
        });
      }
    } catch (error) {
      rowless.push({
        kind: "junit-unreadable",
        text: `The junit file is not the junit Bun 1.4.4 writes: ${errorText(error)}`,
      });
    }
  }

  const rows = new Map<string, Testcase[]>();
  const perFile = new Map<string, number>(junit.files.map((file) => [file, 0]));
  for (const testcase of junit.testcases) {
    const key = JSON.stringify([testcase.file, testcase.describe, testcase.name]);
    const runs = rows.get(key);
    if (runs === undefined) rows.set(key, [testcase]);
    else runs.push(testcase);
    perFile.set(testcase.file, (perFile.get(testcase.file) ?? 0) + 1);
  }
  for (const [file, count] of perFile) {
    if (count % run.reruns === 0) continue;
    rowless.push({
      kind: "uneven-file",
      text:
        `${file} holds ${counted(count, "testcase")}, which is not a whole multiple of ` +
        `${run.reruns} reruns, so not every row of it ran every time.`,
    });
  }

  const failingRows: FailingRow[] = [];
  for (const runs of rows.values()) {
    const failed = runs.filter((each) => each.outcome === "failed");
    if (failed.length === 0) continue;
    const first = runs.findIndex((each) => each.outcome === "failed") + 1;
    const { file, describe, name } = runs[0];
    // The message of a failed `expect` ends with a newline. The report would show an empty line.
    const message = failed[0].message.trimEnd();
    // Bun runs a file's rows in an order that depends only on the seed and the file, and it
    // keeps that order on every rerun (measured on Bun 1.4.4). So the file alone with the seed
    // and `--rerun-each=n` runs as it did up to run n, the run this row first failed on.
    const rerun = first > 1 ? ` --rerun-each=${first}` : "";
    failingRows.push({
      file,
      name: [...describe, name].join(" > "),
      fails: failed.length,
      runs: runs.length,
      firstFailedRun: first,
      message: message.length <= MESSAGE_LIMIT ? message : `${message.slice(0, MESSAGE_LIMIT)}…`,
      reproduce: `bun run ${script} ${file} --seed=${run.seed}${rerun}`,
    });
  }

  const count = (outcome: Outcome) =>
    junit.testcases.filter((each) => each.outcome === outcome).length;
  const testcases = { passed: count("passed"), failed: count("failed"), skipped: count("skipped") };
  const bunExit = run.signal !== null ? `signal ${run.signal}` : String(run.exitCode);
  if (run.exitCode !== 0 && testcases.failed === 0) {
    rowless.push({
      kind: "bun-exit",
      text: `Bun exited with status ${bunExit}, and no testcase failed.`,
    });
  }
  const errors = bunErrors(run.log);
  if (errors > 0) {
    rowless.push({
      kind: "bun-errors",
      text:
        `Bun counted ${counted(errors, "error")} outside any test, ` +
        "such as a file that failed to load.",
    });
  }

  return {
    github: githubRun(run.environment),
    lane: run.lane,
    seed: run.seed,
    reruns: run.reruns,
    command: `bun run ${script} ${run.target} --seed=${run.seed} --rerun-each=${run.reruns}`,
    bunExit,
    wallSeconds: Math.round(run.wallMs / 100) / 10,
    oneRun: {
      files: junit.files.length,
      rows: perRun(junit.testcases.length, run.reruns),
      skippedRows: perRun(testcases.skipped, run.reruns),
    },
    testcases,
    failingRows,
    rowlessFailures: rowless,
    bunLogTail: rowless.length === 0 ? [] : lastLines(run.log, LOG_TAIL_LINES),
    machineMemory: machineMemory(run.memory),
    // A non-zero exit of Bun leaves a failing row or a `bun-exit` failure, and a run with no
    // testcase leaves a `junit-unreadable` or a `no-testcase` one, so these two hold every failure.
    exitCode: failingRows.length > 0 || rowless.length > 0 ? 1 : 0,
  };
}

function longestBacktickRun(text: string): number {
  return Math.max(0, ...(text.match(/`+/g) ?? []).map((run) => run.length));
}

/** `text` as inline code on one line, with its line breaks written as `\n`. */
function code(text: string): string {
  const flat = text.replaceAll("\r", "\\r").replaceAll("\n", "\\n");
  const ticks = "`".repeat(longestBacktickRun(flat) + 1);
  const pad = flat.startsWith("`") || flat.endsWith("`") ? " " : "";
  return `${ticks}${pad}${flat}${pad}${ticks}`;
}

/** `text` as a fenced code block. */
function fenced(text: string, language: string): string[] {
  const fence = "`".repeat(Math.max(3, longestBacktickRun(text) + 1));
  return [`${fence}${language}`, text, fence];
}

/** `count` with its noun: `one` when the count is 1, `many` otherwise. */
function counted(count: number, one: string, many = `${one}s`): string {
  return `${count} ${count === 1 ? one : many}`;
}

function oneRunText(report: Report): string {
  const { files, rows, skippedRows } = report.oneRun;
  return `${counted(files, "file")}, ${counted(rows, "row")}, ${counted(skippedRows, "skipped row")}`;
}

function testcasesText(report: Report): string {
  const { passed, failed, skipped } = report.testcases;
  return `${passed} passed, ${failed} failed and ${skipped} skipped testcases`;
}

function failuresText(report: Report): string {
  return (
    `${counted(report.failingRows.length, "row")} failed. ` +
    `${counted(report.rowlessFailures.length, "failure belongs", "failures belong")} to no row.`
  );
}

function memoryText(memory: MachineMemory | null): string {
  if (memory === null) return "not measured, because /proc/meminfo could not be read";
  const { memTotalMiB, lowestAvailableMiB, mostInUseMiB, samples } = memory;
  return (
    `MemTotal ${memTotalMiB} MiB, lowest MemAvailable ${lowestAvailableMiB} MiB, so ` +
    `${mostInUseMiB} MiB in use at the most, from ${counted(samples, "reading")} of ` +
    `/proc/meminfo, one at the start and then one every ${MEMORY_SAMPLE_MS / 1000} s`
  );
}

/** The report as markdown, for report.md and the step summary. */
export function markdown(report: Report): string {
  const lines = [`## Repeated unit tests: ${report.lane} lane`, ""];
  const { github } = report;
  if (github.event !== null) lines.push(`- Event: ${code(github.event)}`);
  if (github.commit !== null) lines.push(`- Commit: ${code(github.commit)}`);
  if (github.runId !== null) {
    const label = `run ${github.runId}${github.attempt === null ? "" : `, attempt ${github.attempt}`}`;
    lines.push(`- Run: ${github.url === null ? label : `[${label}](${github.url})`}`);
  }
  lines.push(
    `- Lane: ${code(report.lane)}, run as ${code(report.command)}`,
    `- Seed: ${code(String(report.seed))}`,
    `- Reruns: ${report.reruns}`,
    `- Bun's exit status: ${report.bunExit}`,
    `- Wall time: ${report.wallSeconds.toFixed(1)} s`,
    `- One run: ${oneRunText(report)}`,
    `- All runs: ${testcasesText(report)}`,
    `- Machine memory, which counts every process on the machine and not Bun's alone: ` +
      memoryText(report.machineMemory),
    ""
  );

  const failing = report.failingRows;
  lines.push(
    failing.length === 0 ? "### No row failed" : `### ${counted(failing.length, "row")} failed`,
    ""
  );
  for (const row of failing) {
    const first = row.firstFailedRun > 1 ? `, first on run ${row.firstFailedRun}` : "";
    lines.push(
      `#### ${code(row.file)}: ${code(row.name)}`,
      "",
      `Failed ${row.fails}/${row.runs} runs${first}. The first failure:`,
      "",
      ...fenced(row.message, "text"),
      "",
      "To run its file again in the order it failed in, run this from `packages/dispatch`. A",
      "failure that came from the lane's load can pass there: packages/dispatch/AGENTS.md",
      '("## Commands") says what it needs.',
      "",
      ...fenced(row.reproduce, "sh"),
      ""
    );
  }

  const rowless = report.rowlessFailures;
  if (rowless.length > 0) {
    lines.push(
      `### ${counted(rowless.length, "failure belongs", "failures belong")} to no row`,
      "",
      ...rowless.map((failure) => `- ${failure.text}`),
      "",
      `The last ${report.bunLogTail.length} lines of \`bun.log\`:`,
      "",
      ...fenced(report.bunLogTail.join("\n"), "text"),
      ""
    );
  }
  return lines.join("\n");
}

/** GitHub's escaping of a workflow command's message. */
function escapeData(text: string): string {
  return text.replaceAll("%", "%25").replaceAll("\r", "%0D").replaceAll("\n", "%0A");
}

/** GitHub's escaping of a workflow command's property. */
function escapeProperty(text: string): string {
  return escapeData(text).replaceAll(":", "%3A").replaceAll(",", "%2C");
}

function workflowCommand(
  command: "error" | "notice",
  properties: Readonly<Record<string, string>>,
  message: string
): string {
  const list = Object.entries(properties)
    .map(([name, value]) => `${name}=${escapeProperty(value)}`)
    .join(",");
  return `::${command} ${list}::${escapeData(message)}`;
}

/** The GitHub Actions annotations of the report: one `::error` per failure that belongs to no
 *  row, then one per failing row, and one `::notice` with the totals. GitHub keeps ERROR_LIMIT
 *  errors a step. So where there are more, the last of the ERROR_LIMIT errors takes the place of
 *  the rest and says how many more failures report.md lists. */
export function annotations(report: Report): string[] {
  const rowless = report.rowlessFailures.map((failure) =>
    workflowCommand(
      "error",
      { title: "Failure outside any test" },
      `${failure.text} (seed ${report.seed})`
    )
  );
  const rows = report.failingRows.map((row) =>
    workflowCommand(
      "error",
      {
        file: `packages/dispatch/${row.file}`,
        title: row.fails === row.runs ? "Failing unit test" : "Flaky unit test",
      },
      `${row.name} failed ${row.fails}/${row.runs} runs (seed ${report.seed}): ${row.message}`
    )
  );
  const lines = [...rowless, ...rows];
  if (lines.length > ERROR_LIMIT) {
    const shown = ERROR_LIMIT - 1;
    const rowlessLeft = Math.max(0, rowless.length - shown);
    const rowsLeft = lines.length - shown - rowlessLeft;
    const left: string[] = [];
    if (rowlessLeft > 0) {
      left.push(
        `${counted(rowlessLeft, "more failure that belongs", "more failures that belong")} to no row`
      );
    }
    if (rowsLeft > 0) left.push(counted(rowsLeft, "more failing row"));
    lines.length = shown;
    lines.push(
      workflowCommand(
        "error",
        { title: "More failures in report.md" },
        `report.md lists ${left.join(" and ")}: GitHub keeps ${ERROR_LIMIT} errors a step ` +
          `(seed ${report.seed}).`
      )
    );
  }
  lines.push(
    workflowCommand(
      "notice",
      { title: `Repeated unit tests: ${report.lane} lane` },
      `Seed ${report.seed}, ${report.reruns} reruns, ${report.wallSeconds.toFixed(1)} s. ` +
        `One run: ${oneRunText(report)}. All runs: ${testcasesText(report)}. ` +
        `${failuresText(report)} Machine memory, which counts every process and not Bun's ` +
        `alone: ${memoryText(report.machineMemory)}.`
    )
  );
  return lines;
}

/** Bun's summary lines: the seed, the counts, and the `Ran N tests` line. */
const summaryLine =
  /^(?: --seed=\d+| \d+ (?:pass|fail|skip|todo|errors?)|Ran \d+ tests? across \d+ files?\. \[[^\]]*\])$/;

/** Copies one of Bun's streams into bun.log as it arrives, and prints Bun's summary lines. */
async function relay(stream: ReadableStream<Uint8Array>, log: number): Promise<void> {
  const reader = stream.getReader();
  const decoder = new TextDecoder();
  let pending = "";
  for (let chunk = await reader.read(); !chunk.done; chunk = await reader.read()) {
    writeSync(log, chunk.value);
    pending += decoder.decode(chunk.value, { stream: true });
    const lines = pending.split("\n");
    pending = lines.pop() ?? "";
    for (const line of lines) if (summaryLine.test(line)) console.log(line);
  }
  pending += decoder.decode();
  if (summaryLine.test(pending)) console.log(pending);
}

/** MemTotal and MemAvailable from /proc/meminfo. It answers undefined where the file or either
 *  field cannot be read, as on a machine that is not Linux. */
function readMeminfo(): MemorySample | undefined {
  let text: string;
  try {
    text = readFileSync("/proc/meminfo", "utf8");
  } catch {
    return undefined;
  }
  const total = /^MemTotal:\s+(\d+) kB$/m.exec(text);
  const available = /^MemAvailable:\s+(\d+) kB$/m.exec(text);
  if (total === null || available === null) return undefined;
  return { memTotalKiB: Number(total[1]), memAvailableKiB: Number(available[1]) };
}

/** Prints `problem` and the usage on stderr, and answers the exit status of a bad command line. */
function refuse(problem: string): 2 {
  console.error(`unit-repeat: ${problem}\n${usage}`);
  return 2;
}

/** Runs the lane once with `options` and writes its report. Answers the exit status: 2, with
 *  nothing run, where `options.out` cannot be the output directory, as for any bad command line.
 *  `target` is the path the lane tests, its whole directory unless a caller names a narrower one:
 *  that is how a smoke run drives the real path over one file, in a pod where a whole lane may
 *  not run. */
export async function runLane(
  options: RepeatOptions,
  target: string = lanes[options.lane].target
): Promise<0 | 1 | 2> {
  const { lane, reruns, seed } = options;
  const { script } = lanes[lane];
  const out = resolve(options.out);
  const junitPath = join(out, "junit.xml");
  const logPath = join(out, "bun.log");
  // The tool's first writes to `out`, before it prints or starts anything: where one fails, `out`
  // cannot be the output directory, and the command line that named it is refused.
  let log: number;
  try {
    mkdirSync(out, { recursive: true });
    // A junit file an earlier run left here would be read as this run's.
    rmSync(junitPath, { force: true });
    log = openSync(logPath, "w");
  } catch (error) {
    return refuse(`--out ${options.out} cannot be the output directory: ${errorText(error)}`);
  }
  console.log(`seed=${seed} lane=${lane} reruns=${reruns}`);
  console.log(`running bun run ${script} ${target} in ${packageDirectory}; its output: ${logPath}`);

  const started = performance.now();
  const child = Bun.spawn(
    [
      process.execPath,
      "run",
      script,
      target,
      `--seed=${seed}`,
      `--rerun-each=${reruns}`,
      "--reporter=junit",
      `--reporter-outfile=${junitPath}`,
    ],
    { cwd: packageDirectory, stdin: "ignore", stdout: "pipe", stderr: "pipe" }
  );
  const memory: MemorySample[] = [];
  const sample = () => {
    const reading = readMeminfo();
    if (reading !== undefined) memory.push(reading);
  };
  sample();
  const sampler = setInterval(sample, MEMORY_SAMPLE_MS);
  try {
    await Promise.all([relay(child.stdout, log), relay(child.stderr, log)]);
    await child.exited;
  } finally {
    clearInterval(sampler);
    closeSync(log);
  }
  const wallMs = performance.now() - started;

  const report = summarise({
    lane,
    target,
    seed,
    reruns,
    wallMs,
    exitCode: child.exitCode,
    signal: child.signalCode,
    junit: junitAt(junitPath),
    log: readFileSync(logPath, "utf8"),
    environment: process.env,
    memory,
  });
  const text = markdown(report);
  writeFileSync(join(out, "report.json"), `${JSON.stringify(report, null, 2)}\n`);
  writeFileSync(join(out, "report.md"), text);
  const stepSummary = process.env.GITHUB_STEP_SUMMARY;
  if (stepSummary !== undefined && stepSummary !== "") appendFileSync(stepSummary, text);
  if (process.env.GITHUB_ACTIONS === "true") {
    for (const line of annotations(report)) console.log(line);
  }
  console.log(
    `${failuresText(report)} Wall time ${report.wallSeconds.toFixed(1)} s. ` +
      `The report: ${join(out, "report.md")}`
  );
  return report.exitCode;
}

async function main(): Promise<number> {
  const parsed = parseArguments(process.argv.slice(2));
  if ("problem" in parsed) return refuse(parsed.problem);
  return runLane(parsed.options);
}

if (import.meta.main) process.exit(await main());
