import { createHash } from "node:crypto";
import { readFile } from "node:fs/promises";
import { join, resolve } from "node:path";
import { type Corpus, CorpusError, readCorpus } from "./corpus-io";
import { type BuildChain, type ClaimDraw, claimDrawOf } from "./draw-io";
import {
  CLAIM_RUN_VARIABLE,
  REGISTRATION,
  REPOSITORY_ROOT,
  RESERVE_VARIABLE,
  type Registration,
  reserveAccessOf,
  runDigestOf,
} from "./reserve";
import { TREES_FILE } from "./trees";

/**
 * The start every stage of the claim run after its draw shares (`s10-inputs.ts`, and the claim's
 * analysis, `analyze.ts --claim-s10`), so the admission they stand on has one implementation.
 *
 * The admission comes first, before any file is opened: the read must be one `claim.ts`
 * registers for this run's digest (`reserveAccessOf`, `runDigestOf`), and `APPROVAL_EVAL_CLAIM_RUN`
 * must name a claim run. A run under no read, or under one not registered, is refused in words that
 * name no file and no case, so it never reaches a reader that could meet a held case or print
 * one's id. The run's digest is then taken once, which the stage records. Then the build is read
 * (`readCorpus`, which takes only the build of that claim run under that read), and its three
 * files' sha256 make the chain (`BuildChain`) the draw must name (`claimDrawOf`).
 */

/** The admitted read, the build and the draw a stage of the claim run stands on. */
export interface ClaimStart {
  corpus: Corpus;
  reference: string;
  build: BuildChain;
  draw: ClaimDraw;
  /** This run's digest, taken once, right after the admission and before any file is read. */
  runDigest: string;
}

/** What a test may hand in: its own registration, and the bundle root its digest is taken under. */
export interface ClaimStartOptions {
  registration?: Registration;
  root?: string;
}

/**
 * The claim run's start for a stage reading the build at `corpusDirectory` and the draw at
 * `drawDirectory`, refused with a `CorpusError` where any part of it is not the claim run's.
 */
export async function claimStartOf(
  corpusDirectory: string,
  drawDirectory: string,
  options: ClaimStartOptions = {}
): Promise<ClaimStart> {
  const root = options.root ?? REPOSITORY_ROOT;
  const registration = options.registration ?? REGISTRATION;
  const run = { argv: process.argv, env: process.env };
  const access = await reserveAccessOf(process.env[RESERVE_VARIABLE], registration, run, root);
  if (access.kind === "refused") throw new CorpusError(access.reason);
  if (access.kind !== "read") {
    throw new CorpusError(
      access.kind === "ended"
        ? `the reserve ended at ${access.record}, and the claim run's stages run only under its read`
        : "the claim run's stages run only under its registered read: set " +
            `${RESERVE_VARIABLE} and ${CLAIM_RUN_VARIABLE} as the claim run's build had them`
    );
  }
  const claimRun = process.env[CLAIM_RUN_VARIABLE]?.trim() ?? "";
  if (claimRun === "") {
    throw new CorpusError(
      `the claim run's stages run only in the claim run ${CLAIM_RUN_VARIABLE} names, and it names none`
    );
  }
  const runDigest = await runDigestOf(run, root);
  const corpus = await readCorpus(corpusDirectory, { registration, root });
  if (
    corpus.reserve.readUnder !== access.reference ||
    corpus.snapshot.reserve?.claimRun !== claimRun
  ) {
    throw new CorpusError(
      `${corpus.directory} is not the build of the claim run ${claimRun} under ${access.reference}`
    );
  }
  const treesPath = join(corpus.directory, TREES_FILE);
  const trees = await readFile(treesPath).catch(() => null);
  if (trees === null) {
    throw new CorpusError(`${treesPath} could not be read: --corpus takes the claim run's build`);
  }
  const build: BuildChain = {
    id: claimRun,
    snapshotSha256: corpus.snapshotSha256,
    corpusSha256: corpus.corpusSha256,
    treesSha256: createHash("sha256").update(trees).digest("hex"),
  };
  const draw = await claimDrawOf(resolve(drawDirectory), build, access.reference);
  return { corpus, reference: access.reference, build, draw, runDigest };
}
