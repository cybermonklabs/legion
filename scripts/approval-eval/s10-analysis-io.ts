import { createHash } from "node:crypto";
import { mkdir, readdir, readFile, rename, rm, writeFile } from "node:fs/promises";
import { join, resolve } from "node:path";
import {
  type ClaimStartOptions,
  claimRunDigestOf,
  claimStartOf,
  walkThroughPin,
} from "./claim-start";
import { CannotRunError, type Corpus, outDirectory } from "./corpus-io";
import { BUILD_CHAIN_NAMES, chainRefusal, DRAW_CHAIN_NAMES } from "./draw-io";
import { REPOSITORY_ROOT } from "./reserve";
import { type AnalysisInput, type Placed, s10AnalysisOf } from "./s10-analysis";
import { renderS10Analysis } from "./s10-analysis-text";
import {
  AnswerLog,
  INVENTORY_FILE,
  type InventoryLine,
  type InventoryRequest,
  inventoryLinesOf,
  inventoryOf,
  type JudgedMoment,
  type Judging,
  judgedMomentOf,
  STAGE_FILE,
  stageRecordOf,
} from "./s10-inputs-io";
import { JUDGED_FILE, judgedStageOf } from "./s10-judged-io";

/**
 * The reading and the writing of the claim's analysis (`analyze.ts --claim-s10`): the stages of
 * the claim run it reads, each held to the chain, and the log walked through the build's pin
 * (`chainedInput`); the record written into its own `--out` (`writtenTo`); and the command
 * (`claimS10`). What it computes from them is `s10-analysis.ts`'s, and what it prints is
 * `s10-analysis-text.ts`'s; `s10-analysis.ts`'s comment says what is read and why.
 */

/** The file the analysis writes under its `--out`: its stage record, which holds every figure. */
export const ANALYSIS_FILE = "s10-analysis.json";

/** The claim's analysis may not run as the registered text fixes it. */
class S10AnalysisError extends CannotRunError {
  override readonly name = "S10AnalysisError";
}

/** The claim path's flags, as `analyze.ts` reads them. */
export interface ClaimS10Flags {
  kind: "claim-s10";
  corpus: string;
  draw: string;
  judged: string;
  out: string;
  format: "text" | "json";
}

function sha256Of(bytes: Uint8Array | string): string {
  return createHash("sha256").update(bytes).digest("hex");
}

/** The bytes of a file of a stage before this one, refused where they are not there. */
async function stageBytesOf(path: string, what: string): Promise<Buffer> {
  try {
    return await readFile(path);
  } catch {
    throw new S10AnalysisError(`${path} could not be read: ${what}`);
  }
}

/** A judged moment as a refusal names it, or none. */
function momentText(moment: JudgedMoment | null): string {
  return moment === null
    ? "no moment"
    : `event ${moment.eventId} (${moment.type}, ${moment.createdAt})`;
}

/**
 * Refuses a request whose inventory line records a judged moment other than the one the inputs
 * stage would record from this walk (`judgedMomentOf`): the statement that stood when his own
 * answer came, that statement itself, by its event id, type and time; and no moment where the
 * walk places no answer of his, an answer not his own, or no statement before it. Those are the
 * only lines the inputs stage writes with no moment, each a deviation; every later deviation keeps
 * its moment, and a ready line always has one. Two walks of one log place one answer at one
 * moment, so a line this walk does not give, or a ready line with no moment, is of another log.
 */
function heldToItsMoment(line: InventoryLine, judging: Judging | undefined, path: string): void {
  const answer = judging?.answer ?? null;
  const statement = judging?.judged ?? null;
  const moment = answer?.own === true && statement !== null ? judgedMomentOf(statement) : null;
  const recorded = line.judged;
  const readyWithNone = line.state === "ready" && recorded === null;
  const same =
    moment === null || recorded === null
      ? moment === recorded
      : moment.eventId === recorded.eventId &&
        moment.type === recorded.type &&
        moment.createdAt === recorded.createdAt;
  if (same && !readyWithNone) return;
  const placed =
    answer === null
      ? "places no answer of his"
      : `places his answer, event ${answer.eventId} ` +
        `(${answer.settlement?.type ?? "ask.answered"}${answer.own ? "" : ", not his own"}), ` +
        `after ${statement === null ? "no statement" : momentText(judgedMomentOf(statement))}`;
  throw new S10AnalysisError(
    `${path} records the judged moment of ${line.askId} as ${momentText(recorded)}` +
      `${readyWithNone ? " on a ready line" : ""}, and the log through the build's pin ` +
      `${placed}: it is not the log the inputs stage read`
  );
}

/**
 * His first answer to each request (point 7), placed by the inputs stage's own rule (`AnswerLog`)
 * over the one walk of the log both stages make (`walkThroughPin`), and held to the judged moment
 * each inventory line records, or to its having none (`heldToItsMoment`).
 */
async function answersOf(
  corpus: Corpus,
  lines: ReadonlyArray<{ request: InventoryRequest; line: InventoryLine }>,
  path: string
): Promise<{ placed: Placed[]; log: AnalysisInput["log"] }> {
  const log = new AnswerLog(
    corpus.snapshot.person,
    lines.map((one) => one.request.askId)
  );
  const eventsRead = await walkThroughPin(corpus, (event) => log.note(event));
  const placed = lines.map(({ request, line }) => {
    const judging = log.judgingOf(request.askId);
    heldToItsMoment(line, judging, path);
    return { request, line, answer: judging?.answer ?? null };
  });
  return { placed, log: { throughEventId: corpus.snapshot.throughEventId, eventsRead } };
}

/**
 * The inventory the inputs stage wrote, held to the one the draw gives (`inventoryOf`): each line
 * once by ask id, and request for request the same asks, issues, trees and counts of cases; each
 * request with its line, in the inventory's order.
 */
function heldToDraw(
  inventory: readonly InventoryLine[],
  requests: readonly InventoryRequest[],
  path: string
): Array<{ request: InventoryRequest; line: InventoryLine }> {
  const byAsk = new Map(requests.map((request) => [request.askId, request]));
  const seen = new Set<string>();
  const held = inventory.map((line) => {
    if (seen.has(line.askId)) {
      throw new S10AnalysisError(`${path} names the request ${line.askId} twice`);
    }
    seen.add(line.askId);
    const request = byAsk.get(line.askId);
    if (request === undefined) {
      throw new S10AnalysisError(
        `${path} names the request ${line.askId}, which no approval case of the draw's report ` +
          "trees is"
      );
    }
    if (
      line.issueKey !== request.issueKey ||
      line.group !== request.group ||
      line.cases !== request.rows.length
    ) {
      throw new S10AnalysisError(
        `${path} holds the request ${line.askId} in ${line.group} with ${line.cases} cases, ` +
          `and the draw's report trees give it ${request.group} with ${request.rows.length}`
      );
    }
    return { request, line };
  });
  const missing = requests.find((request) => !seen.has(request.askId));
  if (missing !== undefined) {
    throw new S10AnalysisError(
      `${path} holds no line of the request ${missing.askId}, which the draw's report trees hold`
    );
  }
  return held;
}

/**
 * Every stage of the claim run this analysis reads, admitted and held to the chain
 * (`s10-analysis.ts`'s comment says how), with his answers placed over the log, as
 * `s10AnalysisOf` takes them.
 */
async function chainedInput(
  flags: ClaimS10Flags,
  options: ClaimStartOptions
): Promise<AnalysisInput> {
  const { corpus, reference, build, draw, runDigest } = await claimStartOf(
    flags.corpus,
    flags.draw,
    options
  );
  const judgedDirectory = resolve(flags.judged);
  const stage = await judgedStageOf(judgedDirectory);
  const judgedPath = join(judgedDirectory, JUDGED_FILE);
  if (stage.record.claimRun !== build.id) {
    throw new S10AnalysisError(
      `${judgedPath} was written by the claim run ${stage.record.claimRun}, and this run's is ` +
        `${build.id}: the analysis takes only its own claim run's judged stage`
    );
  }
  if (stage.record.reference !== reference) {
    throw new S10AnalysisError(
      `${judgedPath} was written under ${stage.record.reference}, and this run reads under ${reference}`
    );
  }
  const inputsPath = join(stage.record.inputs.directory, STAGE_FILE);
  const inputsBytes = await stageBytesOf(inputsPath, "the judged stage names it");
  if (sha256Of(inputsBytes) !== stage.record.inputs.stageSha256) {
    throw new S10AnalysisError(
      `${inputsPath} is not the inputs stage ${judgedPath} judged: its sha256 is ` +
        `${sha256Of(inputsBytes)}, and the judged stage names ${stage.record.inputs.stageSha256}`
    );
  }
  const inputs = stageRecordOf(inputsBytes.toString("utf8"), inputsPath);
  const refusal =
    chainRefusal(inputs.claimRun, build, BUILD_CHAIN_NAMES) ??
    chainRefusal(inputs, { reference }, { reference: "read" }) ??
    chainRefusal(inputs.draw, draw, DRAW_CHAIN_NAMES);
  if (refusal !== null) {
    throw new S10AnalysisError(
      `${inputsPath} is not this claim run's inputs stage over the build and the draw it reads: ` +
        `${refusal}`
    );
  }
  if (inputs.state !== "written" || inputs.inventorySha256 === null) {
    throw new S10AnalysisError(`${inputsPath} records an inputs stage that never finished`);
  }
  const inventoryPath = join(stage.record.inputs.directory, INVENTORY_FILE);
  const inventoryBytes = await stageBytesOf(inventoryPath, "the inputs stage names it");
  const inventorySha256 = sha256Of(inventoryBytes);
  const judgedRead = stage.record.inputs.files[INVENTORY_FILE];
  if (inventorySha256 !== inputs.inventorySha256 || inventorySha256 !== judgedRead) {
    throw new S10AnalysisError(
      `${inventoryPath} is not the inventory the inputs stage wrote and the judging read: its ` +
        `sha256 is ${inventorySha256}, and the records name ${inputs.inventorySha256} and ` +
        `${judgedRead ?? "none"}`
    );
  }
  const lines = heldToDraw(
    inventoryLinesOf(inventoryBytes.toString("utf8"), inventoryPath),
    inventoryOf(corpus.byCaseId, draw.trees, draw.report),
    inventoryPath
  );
  const { placed, log } = await answersOf(corpus, lines, inventoryPath);
  // The digest the admission admitted is the one recorded: a file of the bundle, a file an
  // argument names or a server changed since then would make it another run's.
  const now = await claimRunDigestOf(options.root ?? REPOSITORY_ROOT);
  if (now !== runDigest) {
    throw new S10AnalysisError(
      `this run's digest was ${runDigest} at its admission and is ${now} now: something it covers ` +
        "changed while the analysis ran, so no registration names this run"
    );
  }
  return {
    claimRun: build,
    reference,
    runDigest,
    build: corpus.directory,
    draw,
    inputs: {
      directory: stage.record.inputs.directory,
      stageSha256: stage.record.inputs.stageSha256,
      inventorySha256,
    },
    judged: { directory: judgedDirectory, stage },
    byCaseId: corpus.byCaseId,
    placed,
    log,
  };
}

/**
 * Writes the record's `text` into `out`, or keeps the one there (`s10-analysis.ts`'s comment, "Its
 * own `--out`"): this process's own partial write of this run's record is removed, and every other
 * file refused.
 */
async function writtenTo(out: string, text: string): Promise<"written" | "kept"> {
  await mkdir(out, { recursive: true });
  const path = join(out, ANALYSIS_FILE);
  const partial = `${path}.${process.pid}.partial`;
  for (const name of await readdir(out)) {
    const at = join(out, name);
    if (name === ANALYSIS_FILE) continue;
    const own = at === partial && text.startsWith(await readFile(at, "utf8"));
    if (!own) {
      throw new S10AnalysisError(
        `${at} is no file this run writes, so no run here can show it is its own: remove it ` +
          "once no run writes it"
      );
    }
    await rm(at, { force: true });
  }
  const found = await readFile(path, "utf8").catch(() => null);
  if (found !== null) {
    if (found !== text) {
      throw new S10AnalysisError(
        `${path} is not the record this run gives over the same stages: a run takes only an ` +
          "--out that is empty or holds its own record, byte for byte"
      );
    }
    return "kept";
  }
  await writeFile(partial, text, "utf8");
  await rename(partial, path);
  return "written";
}

/** The claim's analysis as a command: the stages, the record written or kept, the text printed. */
export async function claimS10(
  flags: ClaimS10Flags,
  options: ClaimStartOptions = {}
): Promise<number> {
  const out = await outDirectory(flags.out);
  const analysis = await s10AnalysisOf(await chainedInput(flags, options));
  const done = await writtenTo(out, `${JSON.stringify(analysis, null, 2)}\n`);
  console.log(
    flags.format === "json"
      ? JSON.stringify(analysis, null, 2)
      : renderS10Analysis(analysis).join("\n")
  );
  console.error(`${done} ${join(out, ANALYSIS_FILE)}`);
  return 0;
}
