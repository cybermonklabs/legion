import { createHash } from "node:crypto";
import { readFile } from "node:fs/promises";
import { join, resolve } from "node:path";
import { parseArgs } from "node:util";
import { CannotRunError, CorpusError, outDirectory, UsageError } from "./corpus-io";
import {
  CLAIM_RUN_VARIABLE,
  REGISTRATION,
  REPOSITORY_ROOT,
  RESERVE_VARIABLE,
  type Registration,
  reserveAccessOf,
  runDigestOf,
} from "./reserve";
import {
  INVENTORY_FILE,
  inventoryLinesOf,
  PAGES_DIRECTORY,
  pagePath,
  pageRecordOf,
  REQUESTS_DIRECTORY,
  requestInputsOf,
  requestPath,
  STAGE_FILE,
  stageRecordOf,
} from "./s10-inputs-io";

/**
 * The admission of the claim's judging stage (`s10_judge.py`), which that Python driver runs
 * before it reads anything, so the claim has one implementation of who may read the reserve.
 *
 * It takes `s10_judge.py`'s own arguments and answers, on one line of JSON, whether that run may
 * judge: the run's digest is the digest of `s10_judge.py` run with those arguments
 * (`runDigestOf`, whose `FILE_OPTIONS` takes the bytes of its `--code`, `--skill` and
 * `--never-decide`), and the read is admitted as every stage of the claim admits one
 * (`reserveAccessOf`, under `APPROVAL_EVAL_RESERVE`). Only a read is admitted: the judging runs
 * under the claim run's registered read alone, and a reserve held or ended judges nothing.
 *
 * Then `--inputs` must be the inputs stage that claim run wrote under that read: its record
 * (`s10-inputs.json`) written, its read the one admitted, its claim run the one
 * `APPROVAL_EVAL_CLAIM_RUN` names; and each file it names that the judging reads, the inventory,
 * each ready request's file and each page its search used, there with the bytes it names and in
 * the shape `s10-inputs-io.ts` reads back. The answer names those files by the sha256 of their
 * bytes, and the driver reads each only with that sha256.
 *
 * `--out` must be an absolute path outside the repository's workspace, as every stage's is.
 */

const USAGE = `usage: bun scripts/approval-eval/s10-judge-admit.ts <s10_judge.py's own arguments>

Prints, on one line of JSON, the admission of s10_judge.py run with these arguments: the read it
judges under, the claim run, its digest, and the inputs files it may read, each by its sha256.
Exit status: 0 when the run is admitted, 2 when it is not.`;

/** The command whose run this admits, as its digest names it. */
export const JUDGE_COMMAND = join(import.meta.dir, "s10_judge.py");

/** The judging may not run as the registered text fixes it. */
export class S10JudgeAdmissionError extends CannotRunError {
  override readonly name = "S10JudgeAdmissionError";
}

/** What an admitted run may read, and the chain it records. */
export interface Admission {
  reference: string;
  claimRun: string;
  runDigest: string;
  out: string;
  inputs: {
    directory: string;
    /** The sha256 of the inputs stage's record. */
    stageSha256: string;
    /** Each file the judging may read, by its path under the directory, with its sha256. */
    files: Record<string, string>;
  };
}

/** What a test may hand in beside the arguments: its own registration, root and environment. */
export interface AdmissionOptions {
  registration?: Registration;
  root?: string;
  env?: Readonly<Record<string, string | undefined>>;
}

function sha256Of(bytes: Uint8Array): string {
  return createHash("sha256").update(bytes).digest("hex");
}

/** `s10_judge.py`'s flags, as it reads them: each is required. */
function flagsOf(args: readonly string[]): { inputs: string; out: string } {
  let values: Record<string, string | undefined>;
  try {
    ({ values } = parseArgs({
      args: [...args],
      options: {
        inputs: { type: "string" },
        out: { type: "string" },
        code: { type: "string" },
        skill: { type: "string" },
        "never-decide": { type: "string" },
        image: { type: "string" },
      },
      strict: true,
      allowPositionals: false,
    }));
  } catch {
    throw new UsageError(USAGE);
  }
  const named = ["inputs", "out", "code", "skill", "never-decide", "image"];
  if (named.some((name) => !values[name]?.trim())) throw new UsageError(USAGE);
  return { inputs: (values.inputs as string).trim(), out: (values.out as string).trim() };
}

/** The bytes of one file of the inputs stage, refused where it is not there. */
async function bytesOf(path: string): Promise<Buffer> {
  try {
    return await readFile(path);
  } catch {
    throw new S10JudgeAdmissionError(`${path} could not be read: --inputs takes the inputs stage`);
  }
}

/** A file of the inputs stage, read and held to the sha256 its record names. */
async function heldBytes(path: string, sha256: string | null | undefined): Promise<Buffer> {
  const bytes = await bytesOf(path);
  if (sha256Of(bytes) !== sha256) {
    throw new S10JudgeAdmissionError(
      `${path} is not the file the inputs stage's record names: its sha256 is ${sha256Of(bytes)}`
    );
  }
  return bytes;
}

/**
 * Whether `s10_judge.py` run with `args` may judge, and what it may read: refused with an
 * `S10JudgeAdmissionError` or a `CorpusError` otherwise.
 */
export async function admissionOf(
  args: readonly string[],
  options: AdmissionOptions = {}
): Promise<Admission> {
  const env = options.env ?? process.env;
  const flags = flagsOf(args);
  const out = await outDirectory(flags.out);
  const root = options.root ?? REPOSITORY_ROOT;
  const run = { argv: ["python3", JUDGE_COMMAND, ...args], env };
  const access = await reserveAccessOf(
    env[RESERVE_VARIABLE],
    options.registration ?? REGISTRATION,
    run,
    root
  );
  if (access.kind === "refused") throw new S10JudgeAdmissionError(access.reason);
  if (access.kind !== "read") {
    throw new S10JudgeAdmissionError(
      access.kind === "ended"
        ? `the reserve ended at ${access.record}, and the claim's judging runs only under its read`
        : `the claim's judging runs only under its registered read: set ${RESERVE_VARIABLE} and ` +
            `${CLAIM_RUN_VARIABLE} as the inputs stage had them`
    );
  }
  const directory = resolve(flags.inputs);
  const recordPath = join(directory, STAGE_FILE);
  const recordBytes = await bytesOf(recordPath);
  const record = stageRecordOf(recordBytes.toString("utf8"), recordPath);
  if (record.state !== "written") {
    throw new S10JudgeAdmissionError(`${recordPath} records an inputs stage that never finished`);
  }
  if (record.reference !== access.reference) {
    throw new S10JudgeAdmissionError(
      `${recordPath} was written under ${record.reference}, and this run reads under ${access.reference}`
    );
  }
  const claimRun = env[CLAIM_RUN_VARIABLE]?.trim() ?? "";
  if (record.claimRun.id !== claimRun) {
    throw new S10JudgeAdmissionError(
      `${recordPath} was written by the claim run ${record.claimRun.id}, and ${CLAIM_RUN_VARIABLE} ` +
        `${claimRun === "" ? "is not set" : `names ${claimRun}`}: the judging takes only its own ` +
        "claim run's inputs"
    );
  }
  const files: Record<string, string> = {};
  const inventoryPath = join(directory, INVENTORY_FILE);
  const inventoryBytes = await heldBytes(inventoryPath, record.inventorySha256);
  files[INVENTORY_FILE] = sha256Of(inventoryBytes);
  const seen = new Set<string>();
  for (const line of inventoryLinesOf(inventoryBytes.toString("utf8"), inventoryPath)) {
    if (seen.has(line.askId)) {
      throw new CorpusError(`${inventoryPath} names the request ${line.askId} twice`);
    }
    seen.add(line.askId);
    if (line.state !== "ready") continue;
    if (record.requests[line.askId] !== line.requestSha256) {
      throw new S10JudgeAdmissionError(
        `${inventoryPath} and ${recordPath} name two files of the request ${line.askId}`
      );
    }
    const path = requestPath(directory, line.askId);
    const bytes = await heldBytes(path, line.requestSha256);
    const request = requestInputsOf(bytes.toString("utf8"), path);
    if (request.askId !== line.askId || request.issueKey !== line.issueKey) {
      throw new CorpusError(`${path} is the inputs of another request than its line's`);
    }
    files[`${REQUESTS_DIRECTORY}/${line.askId}.json`] = sha256Of(bytes);
    const queries = request.search === null ? [] : [request.search.title, request.search.question];
    for (const page of queries.flatMap((query) => query.pages)) {
      const key = `${PAGES_DIRECTORY}/${page.sha256}.json`;
      if (Object.hasOwn(files, key)) continue;
      const pageFile = pagePath(directory, page.sha256);
      const pageBytes = await heldBytes(pageFile, record.pages[page.sha256]);
      if (pageRecordOf(pageBytes.toString("utf8"), pageFile).sha256 !== page.sha256) {
        throw new CorpusError(`${pageFile} records another page than its name`);
      }
      files[key] = sha256Of(pageBytes);
    }
  }
  return {
    reference: access.reference,
    claimRun,
    runDigest: await runDigestOf(run, root),
    out,
    inputs: { directory, stageSha256: sha256Of(recordBytes), files },
  };
}

/** The command: the admission on one line of JSON, and the exit status it takes. */
export async function main(args: readonly string[]): Promise<number> {
  try {
    console.log(JSON.stringify(await admissionOf(args)));
    return 0;
  } catch (error) {
    if (error instanceof CannotRunError) {
      console.error(`${error.name}: ${error.message}`);
      return 2;
    }
    throw error;
  }
}

if (import.meta.main) process.exit(await main(Bun.argv.slice(2)));
