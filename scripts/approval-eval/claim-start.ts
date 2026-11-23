import { createHash } from "node:crypto";
import { readFile } from "node:fs/promises";
import { join, resolve } from "node:path";
import { type Corpus, CorpusError, readCorpus } from "./corpus-io";
import { type DispatchEvent, pageEventLog } from "./dispatch-read";
import { type BuildChain, type ClaimDraw, claimDrawOf } from "./draw-io";
import {
  CLAIM_RUN_VARIABLE,
  DigestError,
  REGISTRATION,
  REPOSITORY_ROOT,
  RESERVE_VARIABLE,
  type Registration,
  reserveAccessOf,
  runDigestOf,
} from "./reserve";
import { TREES_FILE } from "./trees";

/**
 * The start the two stages of the claim run that read its build share: the inputs stage
 * (`s10-inputs.ts`) and the claim's analysis (`analyze.ts --claim-s10`), so the admission they
 * stand on has one implementation; and their one walk of the log through the build's pin
 * (`walkThroughPin`). The judging stage (`s10_judge.py`) reads no build, and admits through
 * `s10-judge-admit.ts`.
 *
 * The admission comes first, before any file is opened: the read must be one `claim.ts`
 * registers for this run's digest (`reserveAccessOf`, `runDigestOf`), and `APPROVAL_EVAL_CLAIM_RUN`
 * must name a claim run. A run under no read, or under one not registered, is refused in words that
 * name no file and no case, so it never reaches a reader that could meet a held case or print
 * one's id. The digest the stage records is then taken and held to the registration: it must be
 * one the registration names under the read the admission found, so the record names a digest the
 * admission admits. Then the build is read (`readCorpus`, which takes only the build of that claim
 * run under that read), whose snapshot must record the events its build read, and its three files'
 * sha256 make the chain (`BuildChain`) the draw must name (`claimDrawOf`).
 */

/** The admitted read, the build and the draw a stage of the claim run stands on. */
export interface ClaimStart {
  corpus: Corpus;
  reference: string;
  build: BuildChain;
  draw: ClaimDraw;
  /** This run's digest, which the registration names under the read the admission found. */
  runDigest: string;
}

/** What a test may hand in: its own registration, and the bundle root its digest is taken under. */
export interface ClaimStartOptions {
  registration?: Registration;
  root?: string;
}

/**
 * This run's digest now (`runDigestOf`, over `process.argv` and `process.env`), refused with a
 * `CorpusError` where a file it covers cannot be read for it.
 */
export async function claimRunDigestOf(root: string): Promise<string> {
  try {
    return await runDigestOf({ argv: process.argv, env: process.env }, root);
  } catch (error) {
    if (!(error instanceof DigestError)) throw error;
    throw new CorpusError(`this run's digest cannot be taken: ${error.message}`);
  }
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
  const runDigest = await claimRunDigestOf(root);
  const registered = registration.reads.some(
    (read) => read.reference === access.reference && read.run === runDigest
  );
  if (!registered) {
    throw new CorpusError(
      `this run's digest is ${runDigest} now, which claim.ts does not register under ` +
        `${access.reference}: something the digest covers changed since the admission`
    );
  }
  // `readCorpus` admits the run again, and takes only the build of this claim run under this read.
  const corpus = await readCorpus(corpusDirectory, { registration, root });
  if (corpus.snapshot.eventsRead <= 0) {
    // `readCorpus` reads a snapshot that records none as having read none; a build of the claim
    // run always records it, so no walk of the log can show it reads the build's log without it.
    throw new CorpusError(
      `${join(corpus.directory, "snapshot.json")} records no events read, so no walk of the log ` +
        "here can show it is the log the build read"
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

/**
 * Walks Dispatch's event log through the build's pin into `note`, oldest event first, as both
 * stages that place his answers walk it; refused unless the walk reads as many events as the build
 * read there, so it is the log the build read. `corpus` is a build `claimStartOf` took, whose
 * snapshot records the events it read. Gives back how many events the walk read.
 */
export async function walkThroughPin(
  corpus: Corpus,
  note: (event: DispatchEvent) => void
): Promise<number> {
  const { throughEventId, eventsRead } = corpus.snapshot;
  const walk = await pageEventLog({
    through: throughEventId,
    onPage: (events) => {
      for (const event of events) note(event);
    },
  });
  if (walk.eventsRead !== eventsRead) {
    throw new CorpusError(
      `the log through event ${throughEventId} reads ${walk.eventsRead} events, and the build ` +
        `read ${eventsRead} there: it is not the log the build read`
    );
  }
  return walk.eventsRead;
}
