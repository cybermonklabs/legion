/** `bun run test:repeat --lane <web|web-cold|harness> [--reruns <k>] [--seed <n>] --out <dir>`:
 *  runs one lane of the Dispatch unit suite with every test run `k` times, and reports each row
 *  that failed (INFRA-1511).
 *
 *    bun run test:repeat --lane web --out /tmp/repeat-web
 *    bun run test:repeat --lane web-cold --reruns 1 --out /tmp/repeat-web-cold
 *    bun run test:repeat --lane harness --reruns 5 --seed 1234 --out /tmp/repeat-harness
 *
 *  The web lane is `bun run test:web web/src`. The web-cold lane is
 *  `bun run test:web:cold web/src`: the web lane with each module the web app loads lazily held
 *  back on each load once a file's rows begin, as web/src/__tests__/hold-lazy-loads.ts lists
 *  them. A rerun of a file keeps the modules its first run loaded, so only the first run of each
 *  file is cold: run that lane with `--reruns 1`. The harness lane is
 *  `bun run test:harness e2e/__tests__`. The tool runs the lane from this package's directory,
 *  whatever directory it starts in. It adds `--seed`, `--rerun-each` and Bun's junit reporter, and
 *  sets DISPATCH_UNIT_REPEAT=1 for Bun's run (`repeatMarker`). It prints the seed before the run
 *  starts, so a run killed midway still names its seed. During the run it prints only Bun's
 *  summary lines. Bun's whole output goes to `<out>/bun.log`.
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
 *  - `<failure>` or `<skipped/>` in a `<testcase>` (a todo row is skipped too). A failure's
 *    message is its `message`. Bun writes none for an error with no message, such as
 *    `new Error("")` or a thrown string, and writes the error's text and stack only as the
 *    failure's body. Where `message` is missing or empty, the reader takes the body instead.
 *    Bun writes no other text between its tags;
 *  - `<properties>` in each suite under GitHub Actions, which the reader passes over;
 *  - no junit file at all when no testcase ran.
 *
 *  A file that fails to load leaves no suite in the junit file. Bun counts it on a ` N error`
 *  line of its summary, and the report names that count as a failure that belongs to no row.
 *
 *  A leg's deadline. A SIGTERM or a SIGINT the tool receives is a stop: the workflow runs the
 *  tool under `exec timeout --foreground --signal=TERM` inside its step's budget, which sends
 *  the tool that SIGTERM at the deadline, and which passes on the SIGINT that GitHub's runner
 *  sends the step's top process when it cancels the step or the step reaches its budget. The
 *  tool adds e2e/tools/unit-repeat-progress.ts to the lane's preloads, which writes to
 *  `<out>/progress.jsonl` where each test file is: which run of it, which row of that run. On a
 *  stop while Bun runs the tool reads that file first, then passes the signal on to Bun. Once Bun
 *  has ended, it runs each running file alone under the seed to read its rows' names, and writes
 *  the report as for any run. The report names each test file a worker was in: the file, its
 *  run, the row of that run by its place in the seed's order and its name, how long since that
 *  file's last event, and the command that runs the file again in that order. A stop that comes
 *  after Bun has ended by itself, while the tool writes its report, is named in the report too,
 *  beside every result Bun wrote. The tool listens for both signals until its report is written,
 *  so a stop never ends it with no report.
 *
 *  A worker, or Bun's test process, killed from outside Bun ends Bun early: Bun gives the file a
 *  `(worker crashed)` testcase, or writes no junit file at all, and names no row. The progress
 *  file still says where each worker was, so the report names each file a worker died in by its
 *  run and its row, as a stop does, and so it does for a worker that died before a stop. The
 *  progress file stays in `<out>` after a stop or a failed run; after a run that passed it is
 *  removed, since the junit file then holds every row's result.
 *
 *  Bun and each run that reads names run in a process group of their own, and the tool kills
 *  each group whole: Bun's when its grace after the stop has passed, a names run's when its bound
 *  has passed, and every group once it has ended, so that no process of a run outlives the tool.
 *  Both bounds are settable (`stopBounds` below), and the report says when one of them ended a
 *  run.
 *
 *  A run that read nothing is refused, not reported as clean. The exit status is 1 when a row
 *  failed, when Bun exited non-zero, when no testcase was read, when a failure belongs to no
 *  row, or when the run was stopped. It is 2 for a bad command line, and 0 otherwise. An `--out`
 *  that cannot be the output directory - a file, a path under a file, a place the tool may not
 *  write - is a bad command line too, refused before Bun starts, and so is a bound's variable
 *  that holds no whole number of milliseconds. e2e/__tests__/unit-repeat.test.ts runs real Bun
 *  and checks the reader and the report on the files Bun writes, a stop of a leg whose rows never
 *  end, a stop whose names run hangs in a hook, a stop that comes while the report is written, a
 *  worker and Bun's test process killed in a row, and the workflow's step cancelled as GitHub's
 *  runner cancels it.
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

/** What the tool sets for the Bun run of every lane, so a row can tell it runs many times over.
 *  Rows that wait out a span of real time skip under it: e2e/__tests__/outside-edge-hold.test.ts's
 *  60 s hold, 15 s release, 10 s flush, 30 s reopen and its rows on short tokens and a short
 *  lapse, each a process of its own on the real clock, and e2e/__tests__/outside-edge.test.ts's
 *  rows that wait out one of the outside driver's own bounds (a page's 30 s, a revoke's list read,
 *  a device login's deadline). Twenty reruns of them would spend about half an hour of a harness
 *  leg waiting, past the 30-minute step dispatch-unit-repeat.yaml gives it, and the waits find no
 *  fault a single run does not; each runs once a run where the rest of its file runs. */
const repeatMarker = { DISPATCH_UNIT_REPEAT: "1" } as const;

const laneNames = Object.keys(lanes);

/** Whether `text` is the name of a lane. */
function isLane(text: string): text is Lane {
  return Object.hasOwn(lanes, text);
}

/** This package's directory. Every lane runs from it. */
const packageDirectory = fileURLToPath(new URL("../..", import.meta.url));

/** The preload that writes where each test file is, which a stop reads. */
const progressPreload = fileURLToPath(new URL("./unit-repeat-progress.ts", import.meta.url));

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
/** The two bounds of a stop, each a hang guard that decides no verdict on its own: `grace`, how
 *  long Bun gets to end after the stop is passed on to it, before the tool kills Bun's whole
 *  process group; and `names`, how long the run that reads a running file's rows' names gets
 *  before its process group is killed. The report says when either of them ended a run.
 *
 *  Each default is at least three times the largest reading of its kind. The readings come from
 *  nine stopped legs, three of each lane, on 2026-11-20. Each leg ran under the workflow's own
 *  `timeout` on 4 CPUs with 8 busy loops on them, with the box at a load of 383 to 671, and was
 *  stopped before its lane ended:
 *  - Bun ended 86 to 966 ms after the stop. 3 x 966 ms is 2.9 s, and the grace is 10 s.
 *  - 31 names runs, 3 or 4 at once in each leg, took 57 ms to 4,178 ms. 3 x 4,178 ms is 12.5 s,
 *    and the bound is 20 s.
 *  Together they keep the report inside the 60 s that the step's `timeout --kill-after` gives the
 *  tool after the stop. .github/ci-notes/dispatch-unit-repeat.md ("The two bounds of a stop")
 *  gives every reading. A caller sets either bound with its variable, a whole number of
 *  milliseconds, as the stop rows of e2e/__tests__/unit-repeat.test.ts do, so that their verdicts
 *  rest on their own marks. */
const stopBounds = {
  grace: { variable: "DISPATCH_UNIT_REPEAT_STOP_GRACE_MS", defaultMs: 10_000 },
  names: { variable: "DISPATCH_UNIT_REPEAT_NAMES_BOUND_MS", defaultMs: 20_000 },
} as const;
/** How long Bun's pipes get to close after Bun ended on a stop, before the tool stops reading
 *  them: a process Bun left behind can hold a pipe open, and the report must not wait on it. */
const PIPE_GRACE_MS = 2_000;
/** The name of the testcase Bun 1.4.4's junit file gives a file whose `--parallel` worker ended
 *  before it reported, with the message "worker process crashed before reporting results". */
const WORKER_CRASHED = "(worker crashed)";

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
  /** The failure's message or, where Bun wrote none, the failure's body: the error's text and
   *  stack. It is empty unless the testcase failed. */
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

/** The text between two tags of the junit file, with its entities decoded. */
interface TextBetween {
  readonly kind: "text";
  readonly text: string;
}

const entities: Readonly<Record<string, string>> = {
  amp: "&",
  apos: "'",
  gt: ">",
  lt: "<",
  quot: '"',
};

/** An attribute value or a text with its five named entities and its numeric references decoded.
 *  A `&` that starts none of them is refused. */
function decodeEntities(text: string): string {
  return text.replace(
    /&(?:#x([0-9A-Fa-f]+);|#([0-9]+);|(amp|apos|gt|lt|quot);)?/g,
    (_, hex?: string, decimal?: string, named?: string) => {
      if (named !== undefined) return entities[named];
      if (hex !== undefined) return String.fromCodePoint(Number.parseInt(hex, 16));
      if (decimal !== undefined) return String.fromCodePoint(Number(decimal));
      throw new Error(`a value holds a bare &: ${JSON.stringify(text)}`);
    }
  );
}

const NAME = "[A-Za-z_][-A-Za-z0-9_.:]*";

/** Every tag of `xml`, and every text between two tags that is not white space alone, in order.
 *  It skips the XML declaration and comments. Bun writes such text only as a failure's body, and
 *  quotes every attribute value with `"`. */
function* tags(xml: string): Generator<Tag | TextBetween> {
  const opening = new RegExp(`<(/?)(${NAME})`, "y");
  const attribute = new RegExp(`\\s+(${NAME})="([^"<]*)"`, "y");
  const ending = /\s*(\/?)>/y;
  let at = 0;
  for (;;) {
    const start = xml.indexOf("<", at);
    const text = xml.slice(at, start === -1 ? xml.length : start);
    if (/\S/.test(text)) yield { kind: "text", text: decodeEntities(text) };
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

type OpenTestcase = { -readonly [K in keyof Testcase]: Testcase[K] };

/** The files and testcases of a junit file Bun 1.4.4 wrote. It throws on anything else: a tag or
 *  a text in a place Bun puts none, a tag left open, or a file with no `<testsuites>`. */
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
          // Bun writes no `message` for an error with no message, and writes the error's text and
          // stack only as the failure's body, which `read` then takes as the message.
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

  const read = (text: string) => {
    const parent = open.at(-1);
    if (parent !== "failure") {
      throw new Error(`a text inside <${parent ?? "nothing"}>: ${JSON.stringify(text)}`);
    }
    if (testcase !== undefined && testcase.message === "") testcase.message = text;
  };

  for (const token of tags(xml)) {
    if (token.kind === "text") {
      read(token.text);
      continue;
    }
    if (token.kind === "close") {
      leave(token.name);
      continue;
    }
    enter(token);
    if (token.kind === "empty") leave(token.name);
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
  /** The stop the tool received, while Bun ran or while it wrote its report after, or null where
   *  none came. */
  readonly stop: Stop | null;
  /** Where Bun's workers were when Bun ended early with no stop while it ran, or null where it
   *  did not (`endedEarly`). */
  readonly early: EarlyEnd | null;
}

/** The progress file as the tool read it once Bun had ended early, and the names of the rows of
 *  each file a worker was in, which it read after. */
export interface EarlyEnd {
  /** When the tool read it, in milliseconds since the epoch, the clock the progress file writes. */
  readonly at: number;
  readonly progress: { readonly text: string } | { readonly unreadable: string };
  readonly names: Readonly<Record<string, RowNames>>;
  readonly namesRuns: Readonly<Record<string, NamesRunEnd>>;
}

/** Whether Bun ended early, by no stop of the tool's: it ended by a signal, wrote no junit file
 *  the reader can read, or gave a file a `(worker crashed)` testcase, which Bun 1.4.4 writes for
 *  a file whose worker died before it reported. Then the junit file does not hold every row of
 *  the leg, and the progress file names the row each dead worker was in. */
export function endedEarly(junit: JunitText, signal: string | null): boolean {
  if (signal !== null || "unreadable" in junit) return true;
  try {
    return readJunit(junit.text).testcases.some(workerCrashed);
  } catch {
    return true;
  }
}

/** Whether `testcase` is the one Bun 1.4.4 writes for a file whose worker died before it
 *  reported. */
function workerCrashed(testcase: Testcase): boolean {
  return testcase.name === WORKER_CRASHED && testcase.describe.length === 0;
}

/** A SIGTERM or SIGINT the tool received. Where it came while Bun ran: the progress file as it
 *  read at that moment, before the tool passed the signal on to Bun, and the names of the rows of
 *  each file that was running, which it read once Bun had ended. */
export interface Stop {
  readonly signal: string;
  /** When it came, in milliseconds since the epoch, the clock the progress file writes. */
  readonly at: number;
  /** How long after Bun started it came, in milliseconds. */
  readonly afterMs: number;
  /** Whether Bun had ended by itself when it came, while the tool wrote its report. Then no file
   *  was running, every result Bun wrote stands, and the progress file is not read: `progress`
   *  is empty, and `alive`, `names` and `namesRuns` are too. */
  readonly afterBun: boolean;
  /** The text of `<out>/progress.jsonl`, or why it could not be read. */
  readonly progress: { readonly text: string } | { readonly unreadable: string };
  /** The pids of the workers the progress file names that were alive at the stop. */
  readonly alive: readonly number[];
  /** For each running file that had begun a row, its rows' names in the order Bun runs them under
   *  the leg's seed, or why they could not be read. */
  readonly names: Readonly<Record<string, RowNames>>;
  /** How Bun ended after the stop, or null where no Bun ran, as in a report built by hand. */
  readonly bunEnd: BoundedEnd | null;
  /** How each run that read a running file's names ended, by file. */
  readonly namesRuns: Readonly<Record<string, NamesRunEnd>>;
}

/** How a process group the tool started ended against its bound: its time, and whether the
 *  bound passed first and the tool killed the group. */
export interface BoundedEnd {
  readonly ms: number;
  readonly boundMs: number;
  readonly killed: boolean;
}

/** How a run that read a file's names ended: as a group against its bound, and whether the tool
 *  ended it once it had read the names it needed, before the run ended by itself. */
export interface NamesRunEnd extends BoundedEnd {
  readonly read: boolean;
}

/** A file's rows by name, in the order Bun runs them, or why they could not be read. */
export type RowNames = { readonly names: readonly string[] } | { readonly unreadable: string };

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
  | "uneven-file"
  | "stopped"
  | "worker-died";

/** A test file a worker of Bun was in when the leg was stopped, or when that worker or Bun's
 *  test process died with a run of the file not ended, as the progress file says. */
export interface RunningFile {
  readonly file: string;
  /** The run of the file it was in, counted from 1. */
  readonly run: number;
  /** Where in that run it was:
   *  - `loading`: the run had not begun, so the file's own code ran, which Bun runs again for
   *    each run of it;
   *  - `before-first-row`: the run had begun and no row of it, so a `beforeAll` hook ran;
   *  - `row`: row `rowsBegun` had begun and not ended, so its body ran or a hook after it (the
   *    preload's `end` comes after the file's own `afterEach` hooks, and a hook that throws skips
   *    it);
   *  - `after-row`: row `rowsBegun` had ended, so a hook ran, or the code around the rows;
   *  - `rows-at-once`: the file runs its rows at once (`describe.concurrent` and its kin), and
   *    `rowsRunning` of them had begun and not ended, so the row in hand is not told. */
  readonly place: "loading" | "before-first-row" | "row" | "after-row" | "rows-at-once";
  /** How many rows of that run had begun: in a file that runs one row at a time, the row in hand
   *  is this one in the seed's order. */
  readonly rowsBegun: number;
  /** In a file that runs its rows at once, how many rows had begun and not ended; 0 otherwise. */
  readonly rowsRunning: number;
  /** The name of row `rowsBegun`, its `describe`s first as Bun prints them. Null where no row had
   *  begun, where the file runs its rows at once, or where the names could not be read. */
  readonly rowName: string | null;
  /** Why `rowName` is null although a row had begun, or null. */
  readonly nameUnread: string | null;
  /** Seconds from the file's last event in the progress file to the stop, or to Bun's end where
   *  no stop came first. */
  readonly quietSeconds: number;
  /** The command, run from packages/dispatch, that runs the file again in the same order up to
   *  that run. */
  readonly reproduce: string;
}

/** What the report says of a stop. */
export interface StopReport {
  readonly signal: string;
  readonly afterSeconds: number;
  /** Whether Bun had ended by itself when the stop came, while the tool wrote its report. */
  readonly afterBun: boolean;
  readonly running: readonly RunningFile[];
  /** Why the progress file could not be read, or null where it was read. */
  readonly progressUnreadable: string | null;
  /** The pids of the workers the progress file names that were alive at the stop. */
  readonly alive: readonly number[];
  /** The files Bun gave a `(worker crashed)` testcase: it had not taken in their results when
   *  the stop came. */
  readonly unreported: readonly string[];
  /** How Bun ended after the stop, against the grace. */
  readonly bunEnd: BoundedEnd | null;
  /** How each run that read a running file's names ended, against its bound, by file. */
  readonly namesRuns: Readonly<Record<string, NamesRunEnd>>;
}

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
  /** The stop that came, while Bun ran or while the report was written, or null where none came. */
  readonly stop: StopReport | null;
  /** Each test file whose worker, or Bun's test process, died with a run of it not ended, before
   *  a stop or when Bun ended early with no stop, by the place and the row it was in. */
  readonly died: readonly RunningFile[];
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

/** One line of the progress file, as e2e/tools/unit-repeat-progress.ts writes it. */
type ProgressLine =
  | {
      readonly event: "load";
      readonly load: string;
      readonly at: number;
      readonly file: string;
      readonly pid: number;
      readonly atOnce: boolean;
    }
  | {
      readonly event: "run" | "start" | "end" | "done";
      readonly load: string;
      readonly at: number;
    };

/** The line `text` holds, or undefined for one that is not a line the preload writes: the last
 *  line of a file read while a worker wrote to it can be cut short. */
function progressLine(text: string): ProgressLine | undefined {
  let value: unknown;
  try {
    value = JSON.parse(text);
  } catch {
    return undefined;
  }
  if (typeof value !== "object" || value === null) return undefined;
  if (!("event" in value && "load" in value && "at" in value)) return undefined;
  const { event, load, at } = value;
  if (typeof load !== "string" || typeof at !== "number") return undefined;
  if (event === "run" || event === "start" || event === "end" || event === "done") {
    return { event, load, at };
  }
  if (event !== "load" || !("file" in value && "pid" in value)) return undefined;
  const { file, pid } = value;
  const atOnce = "atOnce" in value && value.atOnce === true;
  return typeof file === "string" && typeof pid === "number"
    ? { event, load, at, file, pid, atOnce }
    : undefined;
}

/** Where one load of a test file had got to, as its lines in the progress file say. */
interface LoadState {
  readonly file: string;
  /** Whether the file runs its rows at once. */
  readonly atOnce: boolean;
  runs: number;
  done: number;
  /** The rows begun and ended in its latest run. */
  begun: number;
  ended: number;
  lastAt: number;
}

/** Each worker's latest load, from the progress file's `text`, by the worker's pid.
 *
 *  A row's `end` is written by the preload's `afterEach`, which Bun 1.4.4 runs after the file's
 *  own `afterEach` hooks, and which a throwing one of them skips (both measured on 2026-11-20).
 *  So an `end` can be missing for a row that did end. In a file that runs one row at a time, the
 *  next row's `start` shows that the row before it ended, and counts it so. In a file that runs
 *  its rows at once, a `start` shows nothing of the rows beside it, and only `end`s count. */
function latestLoads(text: string): Map<number, LoadState> {
  const loads = new Map<string, LoadState>();
  const latest = new Map<number, LoadState>();
  for (const line of text.split("\n")) {
    const read = progressLine(line);
    if (read === undefined) continue;
    if (read.event === "load") {
      const state = {
        file: read.file,
        atOnce: read.atOnce,
        runs: 0,
        done: 0,
        begun: 0,
        ended: 0,
        lastAt: read.at,
      };
      loads.set(read.load, state);
      latest.set(read.pid, state);
      continue;
    }
    const state = loads.get(read.load);
    if (state === undefined) continue;
    state.lastAt = read.at;
    if (read.event === "run") {
      state.runs += 1;
      state.begun = 0;
      state.ended = 0;
    } else if (read.event === "start") {
      if (!state.atOnce) state.ended = state.begun;
      state.begun += 1;
    } else if (read.event === "end") state.ended = Math.min(state.begun, state.ended + 1);
    else state.done += 1;
  }
  return latest;
}

/** The pids of the workers the progress file's `text` names that are alive now, which the stop
 *  reads before it passes the signal on to Bun. A worker that died before the stop, a crash of
 *  its own, is not among them. */
function liveWorkers(text: string): number[] {
  const alive: number[] = [];
  for (const pid of latestLoads(text).keys()) {
    try {
      process.kill(pid, 0);
      alive.push(pid);
    } catch {
      // Gone: ESRCH. A worker of this run is this user's, so EPERM does not arise.
    }
  }
  return alive;
}

type LaneOf = { readonly reruns: number; readonly seed: number; readonly script: string };

/** The test file each worker alive at the stop was in, from the progress file the stop read:
 *  that worker's latest load, unless every run of that file had ended. Each row it names takes
 *  its name from the stop's `names`. The file quiet the longest comes first, since it is the one
 *  most likely to hang. */
export function runningFiles(stop: Stop, lane: LaneOf): RunningFile[] {
  if ("unreadable" in stop.progress) return [];
  return filesAt(stop.progress.text, (pid) => stop.alive.includes(pid), stop.at, stop.names, lane);
}

/** The test file each worker that died was in, with a run of it not ended, from the progress
 *  file. After a stop while Bun ran, these are the workers not alive at the stop: each died
 *  before it, a crash of its own or a kill from outside Bun. After an early end of Bun with no
 *  stop (`endedEarly`), every worker has died, and each one's latest load that had not ended is
 *  where Bun's end, or that worker's own death, cut it short. */
export function diedFiles(run: Pick<LaneRun, "stop" | "early">, lane: LaneOf): RunningFile[] {
  const { stop, early } = run;
  if (stop !== null && !stop.afterBun) {
    if ("unreadable" in stop.progress) return [];
    return filesAt(
      stop.progress.text,
      (pid) => !stop.alive.includes(pid),
      stop.at,
      stop.names,
      lane
    );
  }
  if (early === null || "unreadable" in early.progress) return [];
  return filesAt(early.progress.text, () => true, early.at, early.names, lane);
}

/** The latest load of each worker `taken` picks from the progress file's `text`, unless every run
 *  of that file had ended, named by its place at `at` and, for a row, by its name in `names`. */
function filesAt(
  text: string,
  taken: (pid: number) => boolean,
  at: number,
  names: Readonly<Record<string, RowNames>>,
  lane: LaneOf
): RunningFile[] {
  const running: RunningFile[] = [];
  for (const [pid, state] of latestLoads(text)) {
    if (!taken(pid) || state.done >= lane.reruns) continue;
    const inRun = state.runs > state.done;
    const number = inRun ? state.runs : state.done + 1;
    const rowsBegun = inRun ? state.begun : 0;
    const read = names[state.file];
    let rowName: string | null = null;
    let nameUnread: string | null = null;
    if (rowsBegun > 0) {
      if (read === undefined) nameUnread = "no run read the names of its rows";
      else if ("unreadable" in read) nameUnread = read.unreadable;
      else if (rowsBegun > read.names.length) {
        nameUnread = `the run that read its rows' names read ${counted(read.names.length, "row")}`;
      } else rowName = read.names[rowsBegun - 1];
    }
    const place: RunningFile["place"] = !inRun
      ? "loading"
      : state.begun === 0
        ? "before-first-row"
        : state.atOnce
          ? state.begun > state.ended
            ? "rows-at-once"
            : "after-row"
          : state.begun > state.ended
            ? "row"
            : "after-row";
    running.push({
      file: state.file,
      run: number,
      place,
      rowsBegun,
      rowsRunning: place === "rows-at-once" ? state.begun - state.ended : 0,
      rowName: place === "rows-at-once" ? null : rowName,
      nameUnread: place === "rows-at-once" ? null : nameUnread,
      quietSeconds: Math.round((at - state.lastAt) / 100) / 10,
      // As a failing row's command: the file alone with the seed and `--rerun-each=n` runs as it
      // did up to run n.
      reproduce: `bun run ${lane.script} ${state.file} --seed=${lane.seed}${number > 1 ? ` --rerun-each=${number}` : ""}`,
    });
  }
  return running.sort((one, other) => other.quietSeconds - one.quietSeconds);
}

/** The failures that follow from a stop alone: Bun was stopped before it wrote its junit file
 *  and before every run of every file ended, and it exits non-zero on the signal. */
const followsFromStop: ReadonlySet<RowlessKind> = new Set([
  "bun-exit",
  "junit-unreadable",
  "no-testcase",
  "uneven-file",
]);

/** Where a running file was, in words:
 *  `row 3, "a > b", or a hook after it, of run 2 of 20, 61.2 s since it began`. */
function placeText(file: RunningFile, reruns: number): string {
  const run = `run ${file.run} of ${reruns}`;
  const quiet = `${file.quietSeconds.toFixed(1)} s`;
  const row = `row ${file.rowsBegun}, ${
    file.rowName === null
      ? `whose name was not read (${file.nameUnread})`
      : JSON.stringify(file.rowName)
  },`;
  switch (file.place) {
    case "row":
      return `${row} or a hook after it, of ${run}, ${quiet} since it began`;
    case "after-row":
      return `${run}, after ${row} ended (a hook, or the code around its rows), ${quiet} since`;
    case "rows-at-once":
      return (
        `${run}, in ${counted(file.rowsRunning, "row")} of the ${file.rowsBegun} begun, which ` +
        `the file runs at once, so the row in hand is not told, ${quiet} since its last event`
      );
    case "before-first-row":
      return `${run}, before its first row (a beforeAll hook), ${quiet} since the run began`;
    case "loading":
      return `${run}, before it began: the file's own code, which Bun runs for each run, ${quiet} since its last event`;
  }
}

/** The report of one run of a lane. */
export function summarise(run: LaneRun): Report {
  const { script } = lanes[run.lane];
  // A stop that came while Bun ran. One that came after Bun had ended by itself leaves Bun's
  // results as they are: no file was running, and its junit file and exit are its own.
  const during = run.stop === null || run.stop.afterBun ? null : run.stop;
  const lane = { reruns: run.reruns, seed: run.seed, script };
  const running = during === null ? [] : runningFiles(during, lane);
  // Each file a worker died in with a run of it not ended, before a stop or in an early end of
  // Bun: Bun's `(worker crashed)` testcase of such a file is named by the place and the row the
  // worker was in, and makes no failing row of its own.
  const died = diedFiles(run, lane);
  const diedIn = new Set(died.map((file) => file.file));
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
  const lost: string[] = [];
  // Bun 1.4.4 writes its junit file on a stop too, with a `(worker crashed)` testcase for each
  // file whose results it had not taken in when the stop came: the file in hand, and also a file
  // whose worker had ended it, or had ended its rows and exited, a moment before (both measured
  // on 2026-11-20). That testcase says the stop came first, not that a row failed. So after a
  // stop each such file is named as a file whose results Bun did not report, and no failing row
  // is made of it, unless its worker had died before the stop in a run of it: that file is one
  // the worker died in. With no stop, the testcase stays a failing row of its file unless the
  // progress file names the place the worker died in.
  junit = {
    files: junit.files,
    testcases: junit.testcases.filter((each) => {
      if (!workerCrashed(each)) return true;
      if (diedIn.has(each.file)) return false;
      if (during === null) return true;
      lost.push(each.file);
      return false;
    }),
  };
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
  if (died.length > 0) {
    rowless.unshift({
      kind: "worker-died",
      text:
        `${counted(died.length, "test file was", "test files were")} cut short ` +
        `${during === null ? "when Bun ended early" : "before the stop"}: the worker in each, or ` +
        "Bun's test process, died with a run of it not ended, by a crash or a kill from outside " +
        "Bun such as the out-of-memory killer. The report names each below with the run and the " +
        "row it was in.",
    });
  }

  let stop: StopReport | null = null;
  let failures = rowless;
  if (run.stop !== null && during === null) {
    // Bun had ended by itself, and the stop came while the tool wrote this report: the leg ran
    // past its deadline in the tool's own work. It still fails, as the step's `timeout` fails it.
    stop = {
      signal: run.stop.signal,
      afterSeconds: Math.round(run.stop.afterMs / 100) / 10,
      afterBun: true,
      running: [],
      progressUnreadable: null,
      alive: [],
      unreported: [],
      bunEnd: null,
      namesRuns: {},
    };
    failures = [
      {
        kind: "stopped",
        text:
          `The leg was stopped by ${stop.signal} after ${stop.afterSeconds.toFixed(1)} s, after ` +
          "Bun had ended by itself, while the tool wrote this report: no test file was running, " +
          "and every result Bun wrote stands.",
      },
      ...rowless,
    ];
  } else if (during !== null) {
    const { progress } = during;
    stop = {
      signal: during.signal,
      afterSeconds: Math.round(during.afterMs / 100) / 10,
      afterBun: false,
      running,
      progressUnreadable: "unreadable" in progress ? progress.unreadable : null,
      alive: during.alive,
      unreported: lost,
      bunEnd: during.bunEnd,
      namesRuns: during.namesRuns,
    };
    const named =
      stop.progressUnreadable !== null
        ? `the progress file could not be read (${stop.progressUnreadable}), so no test file is named`
        : stop.running.length === 0
          ? "the progress file names no test file a worker was in, so every worker was between files"
          : `${counted(stop.running.length, "test file was", "test files were")} running, which the ` +
            "report names below with the run and the row each was in";
    const unreported =
      lost.length === 0
        ? ""
        : ` Bun had not reported the results of ${counted(lost.length, "file")} when the stop came: ${lost.join(", ")}.`;
    const { bunEnd } = stop;
    const graceKilled =
      bunEnd?.killed === true
        ? ` Bun did not end within its ${bunEnd.boundMs / 1000} s grace after the stop, so the tool killed its process group, and its junit file can lack rows that ended.`
        : "";
    failures = [
      {
        kind: "stopped",
        text: `The leg was stopped by ${stop.signal} after ${stop.afterSeconds.toFixed(1)} s, before Bun ended: ${named}.${unreported}${graceKilled}`,
      },
      ...rowless.filter((failure) => !followsFromStop.has(failure.kind)),
    ];
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
    rowlessFailures: failures,
    bunLogTail: failures.length === 0 ? [] : lastLines(run.log, LOG_TAIL_LINES),
    machineMemory: machineMemory(run.memory),
    stop,
    died,
    // A non-zero exit of Bun leaves a failing row or a `bun-exit` failure, a run with no
    // testcase leaves a `junit-unreadable` or a `no-testcase` one, and a stop leaves a `stopped`
    // one, so these two hold every failure.
    exitCode: failingRows.length > 0 || failures.length > 0 ? 1 : 0,
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

/** How a process group ended against its bound, in words. */
function endText(end: BoundedEnd): string {
  return end.killed
    ? `was killed at its ${(end.boundMs / 1000).toFixed(1)} s bound`
    : `ended after ${(end.ms / 1000).toFixed(1)} s of its ${(end.boundMs / 1000).toFixed(1)} s bound`;
}

/** The summary lines of a stop: when it came, how Bun ended after it, and how each run that read
 *  a running file's names ended. */
function stopLines(stop: StopReport): string[] {
  return [
    `- Stopped: by ${stop.signal} after ${stop.afterSeconds.toFixed(1)} s, ${stop.afterBun ? "after Bun had ended by itself, while the tool wrote this report" : "before Bun ended"}`,
    ...(stop.bunEnd === null ? [] : [`- Bun, after the stop, ${endText(stop.bunEnd)}`]),
    ...Object.entries(stop.namesRuns).map(
      ([file, end]) =>
        `- The run that read the names of ${code(file)}'s rows ${
          end.read
            ? `was ended once it had printed the names needed, after ${(end.ms / 1000).toFixed(1)} s of its ${(end.boundMs / 1000).toFixed(1)} s bound`
            : endText(end)
        }`
    ),
  ];
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
    ...(report.stop === null ? [] : stopLines(report.stop)),
    `- One run: ${oneRunText(report)}`,
    `- All runs: ${testcasesText(report)}`,
    `- Machine memory, which counts every process on the machine and not Bun's alone: ` +
      memoryText(report.machineMemory),
    ""
  );

  const placed = (file: RunningFile) => [
    `#### ${code(file.file)}`,
    "",
    `In ${placeText(file, report.reruns)}. The row is counted in the order Bun ran the`,
    "file's rows under the seed. To run the file again in that order up to that run, run this",
    "from `packages/dispatch`:",
    "",
    ...fenced(file.reproduce, "sh"),
    "",
  ];
  const { stop } = report;
  if (stop !== null) {
    lines.push(
      stop.afterBun
        ? "### The leg was stopped after Bun had ended, and no test file was running"
        : stop.running.length === 0
          ? "### The leg was stopped, and no test file is named"
          : `### The leg was stopped while ${counted(stop.running.length, "test file was", "test files were")} running`,
      ""
    );
    for (const file of stop.running) lines.push(...placed(file));
  }
  if (report.died.length > 0) {
    lines.push(
      `### ${counted(report.died.length, "test file was", "test files were")} cut short when a worker or Bun's test process died`,
      ""
    );
    for (const file of report.died) lines.push(...placed(file));
  }

  const failing = report.failingRows;
  const read = report.testcases.passed + report.testcases.failed + report.testcases.skipped;
  lines.push(
    failing.length > 0
      ? `### ${counted(failing.length, "row")} failed`
      : stop === null
        ? "### No row failed"
        : read === 0
          ? "### No row's result was read from the junit file"
          : "### No row that ended before the stop failed",
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
      "failure that came from the lane's load can pass there: docs/testing.md (\"A Dispatch unit",
      'test runs in random order") says what it needs.',
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
 *  row, then one per test file that was running when the leg was stopped, then one per test file
 *  a worker died in, then one per failing row, and one `::notice` with the totals. GitHub keeps
 *  ERROR_LIMIT errors a step. So where there are more, the last of the ERROR_LIMIT errors takes
 *  the place of the rest and says how many more failures report.md lists. */
export function annotations(report: Report): string[] {
  const rowless = report.rowlessFailures.map((failure) =>
    workflowCommand(
      "error",
      { title: "Failure outside any test" },
      `${failure.text} (seed ${report.seed})`
    )
  );
  const { stop } = report;
  const running =
    stop === null
      ? []
      : stop.running.map((file) =>
          workflowCommand(
            "error",
            { file: `packages/dispatch/${file.file}`, title: "Running when the leg was stopped" },
            `${file.file}: in ${placeText(file, report.reruns)}, when ${stop.signal} stopped the ` +
              `leg after ${stop.afterSeconds.toFixed(1)} s (seed ${report.seed}). Run it again ` +
              `from packages/dispatch: ${file.reproduce}`
          )
        );
  const died = report.died.map((file) =>
    workflowCommand(
      "error",
      { file: `packages/dispatch/${file.file}`, title: "Running when its worker died" },
      `${file.file}: in ${placeText(file, report.reruns)}, when its worker or Bun's test process ` +
        `died (seed ${report.seed}). Run it again from packages/dispatch: ${file.reproduce}`
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
  const groups = [
    {
      lines: rowless,
      more: (left: number) =>
        `${counted(left, "more failure that belongs", "more failures that belong")} to no row`,
    },
    {
      lines: running,
      more: (left: number) =>
        `${counted(left, "more test file", "more test files")} running when the leg was stopped`,
    },
    {
      lines: died,
      more: (left: number) =>
        `${counted(left, "more test file", "more test files")} a worker died in`,
    },
    { lines: rows, more: (left: number) => counted(left, "more failing row") },
  ];
  const lines = groups.flatMap((group) => group.lines);
  if (lines.length > ERROR_LIMIT) {
    const shown = ERROR_LIMIT - 1;
    const left: string[] = [];
    let before = 0;
    for (const group of groups) {
      const kept = Math.min(group.lines.length, Math.max(0, shown - before));
      if (kept < group.lines.length) left.push(group.more(group.lines.length - kept));
      before += group.lines.length;
    }
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

/** Copies one of Bun's streams into bun.log as it arrives, and prints Bun's summary lines. It
 *  ends at the stream's end, or when the caller cancels `reader`. */
async function relay(reader: ReadableStreamDefaultReader<Uint8Array>, log: number): Promise<void> {
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

/** A heading of Bun's output, the file whose results follow: `<file>:`, or `::group::<file>:`
 *  under GitHub Actions. */
const fileHeading = /^(?:::group::)?(\S.*\.(?:test|spec)\.[cm]?[jt]sx?):$/;
/** A failed row of Bun's output: `(fail) <name> [<time>]`, its `describe`s first. */
const failedRow = /^\(fail\) (.*?)(?: \[\d+(?:\.\d+)?m?s\])?$/;

/** Sends SIGKILL to every process of the group `leader` leads: a child the tool spawned with
 *  `detached`, which makes it the leader of a group of its own, and every process it started
 *  that stayed in that group, Bun's test process and its workers among them. A group that has
 *  already ended is no error. */
function killGroup(leader: number): void {
  try {
    process.kill(-leader, "SIGKILL");
  } catch {
    // ESRCH: no process of the group is left.
  }
}

/** The names of `file`'s rows, in the order Bun runs them under `seed`: one run of the file alone
 *  through the lane's script, in which the progress preload fails every row in its first
 *  `beforeEach`, before the row's body, so a row that hung runs no code of its own here. Bun runs
 *  a file's rows in an order that depends only on the seed and the file, the same on every rerun,
 *  so the names of this one run are the names of every run of the leg's. Bun prints each failed
 *  row on stderr, under its file's heading, as the row fails. The tool needs the first `wanted`
 *  of them, so it ends the run once it has read that many: the rest of the file, its hooks
 *  included, can take as long as it likes, or hang. The run runs the file's own code and hooks,
 *  so it runs in a process group of its own, which the tool kills whole once it has read the
 *  names it needs, when `boundMs` passes if it has not, and again once the run has ended, so that
 *  no process of it outlives the report. */
async function rowNamesOf(
  script: string,
  file: string,
  seed: number,
  wanted: number,
  boundMs: number
): Promise<{ readonly names: RowNames; readonly end: NamesRunEnd }> {
  const environment = Object.fromEntries(
    // Under GitHub Actions Bun would print each of this run's failures as an annotation too, and
    // they are none of the lane's.
    Object.entries(process.env).filter(
      ([name]) => name !== "DISPATCH_UNIT_REPEAT_PROGRESS" && name !== "GITHUB_ACTIONS"
    )
  );
  const started = performance.now();
  const child = Bun.spawn(
    [process.execPath, "run", script, file, `--seed=${seed}`, "--preload", progressPreload],
    {
      cwd: packageDirectory,
      detached: true,
      env: { ...environment, DISPATCH_UNIT_REPEAT_NAMES_ONLY: "1" },
      stdin: "ignore",
      stdout: "ignore",
      stderr: "pipe",
    }
  );
  const reader = child.stderr.getReader();
  let killed = false;
  let read = false;
  let cancel: Timer | undefined;
  // The group is killed, at the bound or once the names are read: a holder of the pipe that left
  // the group gets PIPE_GRACE_MS more before the pipe is read no further.
  const end = () => {
    killGroup(child.pid);
    cancel ??= setTimeout(() => reader.cancel().catch(() => {}), PIPE_GRACE_MS);
  };
  const bound = setTimeout(() => {
    if (read) return;
    killed = true;
    end();
  }, boundMs);
  const decoder = new TextDecoder();
  const names: string[] = [];
  let heading: string | null = null;
  let partial = "";
  const take = (line: string) => {
    const headed = fileHeading.exec(line);
    if (headed !== null) heading = headed[1];
    const row = failedRow.exec(line);
    if (row !== null && heading === file) names.push(row[1]);
  };
  try {
    for (let chunk = await reader.read(); !chunk.done; chunk = await reader.read()) {
      const lines = (partial + decoder.decode(chunk.value, { stream: true })).split("\n");
      partial = lines.pop() ?? "";
      for (const line of lines) take(line);
      if (!read && !killed && names.length >= wanted) {
        read = true;
        end();
      }
    }
    take(partial + decoder.decode());
    await child.exited;
  } finally {
    clearTimeout(bound);
    clearTimeout(cancel);
    killGroup(child.pid);
  }
  const runEnd = { ms: Math.round(performance.now() - started), boundMs, killed, read };
  if (names.length > 0) return { names: { names }, end: runEnd };
  const ended = killed
    ? `was killed at its ${boundMs / 1000} s bound`
    : `exited ${child.exitCode ?? child.signalCode}`;
  return {
    names: { unreadable: `the run that reads its rows' names ${ended} and printed no row of it` },
    end: runEnd,
  };
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

/** The progress file's text, as a stop reads it. A file that is not there yet is empty: no
 *  worker has loaded a test file. */
function progressAt(path: string): Stop["progress"] {
  try {
    return { text: readFileSync(path, "utf8") };
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === "ENOENT") return { text: "" };
    return { unreadable: errorText(error) };
  }
}

/** Prints `problem` and the usage on stderr, and answers the exit status of a bad command line. */
function refuse(problem: string): 2 {
  console.error(`unit-repeat: ${problem}\n${usage}`);
  return 2;
}

/** The bound of `bound` in milliseconds: its variable's whole number where it is set, else its
 *  default; or the problem with a value that is no whole number of at least 1. */
function boundMs(bound: (typeof stopBounds)[keyof typeof stopBounds]): number | string {
  const text = process.env[bound.variable];
  if (text === undefined || text === "") return bound.defaultMs;
  const value = wholeNumber(text);
  return value !== undefined && value >= 1
    ? value
    : `${bound.variable} is a whole number of milliseconds, at least 1, not ${JSON.stringify(text)}`;
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
  const graceMs = boundMs(stopBounds.grace);
  const namesBoundMs = boundMs(stopBounds.names);
  if (typeof graceMs === "string") return refuse(graceMs);
  if (typeof namesBoundMs === "string") return refuse(namesBoundMs);
  const out = resolve(options.out);
  const junitPath = join(out, "junit.xml");
  const logPath = join(out, "bun.log");
  const progressPath = join(out, "progress.jsonl");
  // The tool's first writes to `out`, before it prints or starts anything: where one fails, `out`
  // cannot be the output directory, and the command line that named it is refused.
  let log: number;
  try {
    mkdirSync(out, { recursive: true });
    // A junit or progress file an earlier run left here would be read as this run's.
    rmSync(junitPath, { force: true });
    rmSync(progressPath, { force: true });
    log = openSync(logPath, "w");
  } catch (error) {
    return refuse(`--out ${options.out} cannot be the output directory: ${errorText(error)}`);
  }
  console.log(`seed=${seed} lane=${lane} reruns=${reruns}`);
  console.log(`running bun run ${script} ${target} in ${packageDirectory}; its output: ${logPath}`);

  const started = performance.now();
  // `detached` makes Bun's `bun run` the leader of a process group of its own, which Bun's test
  // process and its workers stay in, so that the tool can kill all of them at once, at the
  // grace's end after a stop and once Bun has ended, and none of them outlives the report.
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
      "--preload",
      progressPreload,
    ],
    {
      cwd: packageDirectory,
      detached: true,
      env: { ...process.env, DISPATCH_UNIT_REPEAT_PROGRESS: progressPath, ...repeatMarker },
      stdin: "ignore",
      stdout: "pipe",
      stderr: "pipe",
    }
  );
  // Read before the stop handler is registered, which cancels them at the grace's end.
  const readers = [child.stdout.getReader(), child.stderr.getReader()];
  const relays = Promise.all(readers.map((reader) => relay(reader, log)));
  // A holder, not a `let`: the stop is set in a signal listener, which the compiler's narrowing of
  // a local to its first value does not see.
  const held: { stop: Stop | null } = { stop: null };
  const stopped = Promise.withResolvers<void>();
  let killer: Timer | undefined;
  let graceKilled = false;
  let stoppedAt = 0;
  // Set once Bun has ended. A stop after that comes while the tool writes its report: there is
  // nothing left to stop, and the report is written again to name it.
  let bunEnded = false;
  const onStop = (signal: NodeJS.Signals) => {
    if (held.stop !== null) return;
    stoppedAt = performance.now();
    const afterMs = stoppedAt - started;
    if (bunEnded) {
      held.stop = {
        signal,
        at: Date.now(),
        afterMs,
        afterBun: true,
        progress: { text: "" },
        alive: [],
        names: {},
        bunEnd: null,
        namesRuns: {},
      };
      console.log(
        `unit-repeat: ${signal} after ${(afterMs / 1000).toFixed(1)} s, after Bun had ended by ` +
          "itself: the report names the stop"
      );
      return;
    }
    // Read first, before Bun hears of the stop: a row its worker's end cuts off must still read
    // as begun and not ended.
    const progress = progressAt(progressPath);
    const alive = "text" in progress ? liveWorkers(progress.text) : [];
    held.stop = {
      signal,
      at: Date.now(),
      afterMs,
      afterBun: false,
      progress,
      alive,
      names: {},
      bunEnd: null,
      namesRuns: {},
    };
    console.log(
      `unit-repeat: ${signal} after ${(afterMs / 1000).toFixed(1)} s: Bun is stopped, and the ` +
        "report names what was running"
    );
    // To `bun run` alone, which passes it on to Bun's test process; that process ends its
    // workers, prints the files still running and writes its junit file, and `bun run` can end
    // before it does. So Bun's end is the end of its pipes, which its test process holds. At the
    // grace's end the tool kills the whole group, and a holder of a pipe that left the group gets
    // PIPE_GRACE_MS more before the pipes are read no further.
    child.kill(signal);
    killer = setTimeout(() => {
      graceKilled = true;
      killGroup(child.pid);
      killer = setTimeout(() => {
        for (const reader of readers) reader.cancel().catch(() => {});
      }, PIPE_GRACE_MS);
    }, graceMs);
    stopped.resolve();
  };
  // The listeners stay until the report is written: a signal with none would end the tool at
  // once, with no report, and a second signal during the names runs would too.
  process.on("SIGTERM", onStop);
  process.on("SIGINT", onStop);
  try {
    const memory: MemorySample[] = [];
    const sample = () => {
      const reading = readMeminfo();
      if (reading !== undefined) memory.push(reading);
    };
    sample();
    const sampler = setInterval(sample, MEMORY_SAMPLE_MS);
    try {
      await Promise.race([relays, stopped.promise]);
      const stop = held.stop;
      if (stop === null) {
        // Bun's pipes have closed: it has ended by itself.
        bunEnded = true;
      } else {
        await relays;
        const bunEnd = {
          ms: Math.round(performance.now() - stoppedAt),
          boundMs: graceMs,
          killed: graceKilled,
        };
        held.stop = { ...stop, bunEnd };
      }
      await child.exited;
    } finally {
      bunEnded = true;
      clearTimeout(killer);
      clearInterval(sampler);
      closeSync(log);
      killGroup(child.pid);
    }
    const wallMs = performance.now() - started;
    let stop = held.stop;
    const junit = junitAt(junitPath);
    // With no stop while Bun ran, Bun can still have ended early: a worker or its test process
    // killed from outside, as the out-of-memory killer does. The junit file then lacks the row
    // each was in, and the progress file names it.
    const early =
      (stop === null || stop.afterBun) && endedEarly(junit, child.signalCode)
        ? { at: Date.now(), progress: progressAt(progressPath), names: {}, namesRuns: {} }
        : null;
    // Bun has ended, so a run of each file a worker was in alone, the run that reads its rows'
    // names, races nothing of the leg's: each file running at the stop, and each a worker died in.
    const laneOf = { reruns, seed, script };
    const begun = [
      ...(stop === null ? [] : runningFiles(stop, laneOf)),
      ...diedFiles({ stop, early }, laneOf),
    ].filter((file) => file.rowsBegun > 0 && file.place !== "rows-at-once");
    // Each file's names are read up to the furthest row a worker of it had begun.
    const toRead = new Map<string, number>();
    for (const file of begun) {
      toRead.set(file.file, Math.max(toRead.get(file.file) ?? 0, file.rowsBegun));
    }
    if (toRead.size > 0) {
      console.log(`unit-repeat: reading the rows' names of ${counted(toRead.size, "file")}`);
    }
    const reads = await Promise.all(
      [...toRead].map(
        async ([file, wanted]) =>
          [file, await rowNamesOf(script, file, seed, wanted, namesBoundMs)] as const
      )
    );
    const names = Object.fromEntries(reads.map(([file, read]) => [file, read.names]));
    const namesRuns = Object.fromEntries(reads.map(([file, read]) => [file, read.end]));
    if (stop !== null) stop = { ...stop, names, namesRuns };
    const earlyRead = early === null ? null : { ...early, names, namesRuns };

    const bunLog = readFileSync(logPath, "utf8");
    const write = (reported: Stop | null): { report: Report; text: string } => {
      const report = summarise({
        lane,
        target,
        seed,
        reruns,
        wallMs,
        exitCode: child.exitCode,
        signal: child.signalCode,
        junit,
        log: bunLog,
        environment: process.env,
        memory,
        stop: reported,
        early: earlyRead,
      });
      const text = markdown(report);
      writeFileSync(join(out, "report.json"), `${JSON.stringify(report, null, 2)}\n`);
      writeFileSync(join(out, "report.md"), text);
      return { report, text };
    };
    let written = write(stop);
    // A signal that lands while the report is built waits for the event loop, which the work
    // above never yields to. One turn lets its listener run: on Bun 1.4.4 a SIGTERM that another
    // process sent during synchronous work reached its listener before the first `setImmediate`
    // after the work, in 30 of 30 runs. Such a stop came after Bun had ended, and the report is
    // written again to name it.
    const turn = Promise.withResolvers<void>();
    setImmediate(turn.resolve);
    await turn.promise;
    if (stop === null && held.stop !== null) {
      stop = held.stop;
      written = write(stop);
    }
    const { report, text } = written;
    // A leg that passed with no stop has its every row's result in the junit file, so the
    // progress file says nothing more. After a stop or a failure it is the record of where each
    // file was, and it stays.
    if (stop === null && report.exitCode === 0) rmSync(progressPath, { force: true });
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
  } finally {
    process.off("SIGTERM", onStop);
    process.off("SIGINT", onStop);
  }
}

async function main(): Promise<number> {
  const parsed = parseArguments(process.argv.slice(2));
  if ("problem" in parsed) return refuse(parsed.problem);
  return runLane(parsed.options);
}

if (import.meta.main) process.exit(await main());
