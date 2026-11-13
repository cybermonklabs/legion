import { Buffer } from "node:buffer";
import { createHash } from "node:crypto";
import { parseArgs } from "node:util";
import type { ApprovalDocument, CaseRow } from "./build-corpus";
import {
  CannotRunError,
  CorpusError,
  newSeed,
  pickAtRandom,
  readCorpus,
  recordOf,
  stringOf,
  UsageError,
} from "./corpus-io";
import {
  type DispatchConfig,
  DispatchRefusedError,
  dispatchConfig,
  readJson,
} from "./dispatch-read";

/**
 * Builds one case of a corpus as the person saw it when they answered, and prints it.
 *
 * What the person saw is the version the ask asked about, never the document as it stands now:
 * every document of the case is read at its `requestedVersion` from
 * `GET /api/v1/artifacts/{artifact id}/versions/{version}`, which answers that version's own
 * `markdown`, `size`, `sha256` and `created_at`. Measured against this server, that `size` is
 * the byte length of that `markdown` and that `sha256` is its SHA-256, so the figures printed
 * here are taken from the markdown itself and say the same thing.
 *
 * A version the route refuses is printed as the refusal with its status, and the case is marked
 * as one with no record of what the person saw: a judge that cannot be shown the document the
 * person read is not judging the same case, and the command exits 1 to say so.
 *
 * `replayCase` is the same work as a value, for the recipes that score a case to import.
 */

/** How much of a document is printed when the caller does not ask for the whole of it. */
const HEAD_LINES = 40;
/** How wide a printed label is, so the values line up. */
const LABEL = 26;

const USAGE = `usage: bun scripts/approval-eval/replay.ts --corpus <dir> --case <case id> \\
  [--print-whole]
       bun scripts/approval-eval/replay.ts --corpus <dir> --random-approval [--seed <n>] \\
  [--print-whole]

Prints one case of a corpus as the person saw it when they answered it.

Options:
  --corpus <dir>      Required. A directory holding corpus.jsonl and snapshot.json.
  --case <case id>    The case to replay, as corpus.jsonl names it.
  --random-approval   In place of --case: one approval case at random, with its seed printed.
  --seed <n>          The seed of that pick, so a printed seed picks the same case again.
  --print-whole       Print each document's markdown whole, not its first ${HEAD_LINES} lines.

Exit status: 0 when the whole case was replayed, 1 when a document version could not be read, 2
when the replay could not run.`;

interface Flags {
  corpus: string;
  caseId: string | undefined;
  randomApproval: boolean;
  seed: number | undefined;
  printWhole: boolean;
}

/** One document of an approval case, at the version the ask asked the person to approve. */
export interface ReplayedDocument {
  name: string | null;
  artifactId: string | null;
  /** The version the document stood at when the ask was written, as the corpus holds it. */
  version: number | null;
  /** The version the ask asked about, which is the one read back. */
  requestedVersion: number | null;
  /** The markdown of that version, or null when it could not be read. */
  markdown: string | null;
  /** The byte length of that markdown, and its SHA-256. */
  bytes: number | null;
  sha256: string | null;
  /** When that version was written, as the version route reports it. */
  versionCreatedAt: string | null;
  named: boolean | null;
  summary: string | null;
  /** Why there is no markdown: the route's refusal, with its status. */
  refusal: string | null;
}

/** One case as the person saw it: the row the build wrote, and the documents it pointed at. */
export interface ReplayedCase {
  row: CaseRow;
  documents: ReplayedDocument[];
  /** False when a document could not be read, so there is no record of what the person saw. */
  complete: boolean;
}

/**
 * One case of a corpus as the person saw it, as a value. Every document of the case's approval
 * block is read at the version the ask asked about; a question case has no document and is
 * complete as it stands.
 */
export async function replayCase(row: CaseRow, config?: DispatchConfig): Promise<ReplayedCase> {
  const resolved = config ?? (await dispatchConfig());
  const documents: ReplayedDocument[] = [];
  for (const document of row.approval?.documents ?? []) {
    documents.push(await replayDocument(document, resolved));
  }
  return { row, documents, complete: documents.every((one) => one.refusal === null) };
}

async function replayDocument(
  document: ApprovalDocument,
  config: DispatchConfig
): Promise<ReplayedDocument> {
  const unread: ReplayedDocument = {
    name: document.name,
    artifactId: document.artifactId,
    version: document.version,
    requestedVersion: document.requestedVersion,
    markdown: null,
    bytes: null,
    sha256: null,
    versionCreatedAt: null,
    named: null,
    summary: null,
    refusal: null,
  };
  if (document.artifactId === null || document.requestedVersion === null) {
    return { ...unread, refusal: "the case names no artifact and version for this document" };
  }
  const path =
    `/api/v1/artifacts/${encodeURIComponent(document.artifactId)}` +
    `/versions/${document.requestedVersion}`;
  let body: unknown;
  try {
    body = await readJson<unknown>(path, config);
  } catch (error) {
    if (!(error instanceof DispatchRefusedError)) throw error;
    return { ...unread, refusal: `${error.status} on GET ${path}` };
  }
  const version = recordOf(body);
  const markdown = stringOf(version?.markdown);
  if (markdown === null) return { ...unread, refusal: `GET ${path} answered no markdown` };
  return {
    ...unread,
    markdown,
    bytes: Buffer.byteLength(markdown, "utf8"),
    sha256: createHash("sha256").update(markdown, "utf8").digest("hex"),
    versionCreatedAt: stringOf(version?.created_at),
    named: typeof version?.named === "boolean" ? version.named : null,
    summary: stringOf(version?.summary),
  };
}

function readFlags(argv: readonly string[]): Flags {
  let values: {
    corpus?: string;
    case?: string;
    "random-approval"?: boolean;
    seed?: string;
    "print-whole"?: boolean;
  };
  try {
    ({ values } = parseArgs({
      args: [...argv],
      options: {
        corpus: { type: "string" },
        case: { type: "string" },
        "random-approval": { type: "boolean" },
        seed: { type: "string" },
        "print-whole": { type: "boolean" },
      },
      strict: true,
      allowPositionals: false,
    }));
  } catch {
    // parseArgs names what it refused, so its message is never repeated.
    throw new UsageError(USAGE);
  }
  const corpus = values.corpus?.trim();
  const caseId = values.case?.trim();
  const randomApproval = values["random-approval"] === true;
  if (!corpus) throw new UsageError(USAGE);
  if (!caseId === !randomApproval) {
    throw new UsageError(`name either --case or --random-approval, and not both\n\n${USAGE}`);
  }
  let seed: number | undefined;
  if (values.seed !== undefined) {
    seed = Number(values.seed.trim());
    if (!Number.isSafeInteger(seed) || seed < 0) {
      throw new UsageError(`--seed takes a whole number, zero or above\n\n${USAGE}`);
    }
  }
  return {
    corpus,
    caseId,
    randomApproval,
    seed,
    printWhole: values["print-whole"] === true,
  };
}

function field(label: string, value: string | number | boolean | null): string {
  const shown = value === null ? "none" : String(value);
  return `  ${label.padEnd(LABEL)}${shown}`;
}

function documentLines(
  document: ReplayedDocument,
  at: number,
  of: number,
  printWhole: boolean
): string[] {
  const lines = [
    "",
    `document ${at} of ${of}: ${document.name ?? "unnamed"}`,
    field("artifact", document.artifactId),
    field("version when asked", document.version),
    field("requested version", document.requestedVersion),
  ];
  if (document.refusal !== null || document.markdown === null) {
    lines.push(field("refused", document.refusal ?? "the version could not be read"));
    lines.push("  no record of what the person saw for this document");
    return lines;
  }
  lines.push(field("version written", document.versionCreatedAt));
  lines.push(field("named version", document.named));
  lines.push(field("version summary", document.summary));
  lines.push(field("bytes", document.bytes));
  lines.push(field("sha256", document.sha256));
  const markdown = document.markdown.split("\n");
  const shown = printWhole ? markdown : markdown.slice(0, HEAD_LINES);
  lines.push(
    printWhole
      ? `  markdown (${markdown.length} lines, whole):`
      : `  markdown (first ${shown.length} of ${markdown.length} lines):`
  );
  for (const line of shown) lines.push(`  | ${line}`);
  if (shown.length < markdown.length) {
    lines.push(`  | ... ${markdown.length - shown.length} more lines`);
  }
  return lines;
}

function caseLines(replayed: ReplayedCase, printWhole: boolean): string[] {
  const row = replayed.row;
  const confounds = row.confounds;
  const lines = [
    `case ${row.caseId}`,
    field("ask", row.askId),
    field("kind", row.kind),
    field("issue", row.issueKey === null ? null : `${row.issueKey} (project ${row.project})`),
    field("urgency", row.urgency),
    field("more than one option", row.multiple),
    field("ask created", row.askCreatedAt),
    field("answered", row.answeredAt),
    field("answer event", `${row.eventId} recorded ${row.eventCreatedAt}`),
    field("verdict", row.verdict),
    field("selected", row.selected.length === 0 ? null : row.selected.join(", ")),
    field("documents", replayed.documents.length),
    "",
    "question",
    `  ${row.question ?? "none"}`,
  ];
  if (row.options.length > 0) {
    lines.push("", "options");
    for (const option of row.options) {
      const description = option.description === null ? "" : `: ${option.description}`;
      lines.push(`  - ${option.label ?? "unnamed"} [${option.id ?? "no id"}]${description}`);
    }
  }
  lines.push("", "the person's words");
  lines.push(row.hasWords && row.words !== null ? `  ${row.words}` : "  none");
  replayed.documents.forEach((document, index) => {
    lines.push(...documentLines(document, index + 1, replayed.documents.length, printWhole));
  });
  lines.push("", "confounds (what the words alone must not be judged beside)");
  lines.push(field("version", confounds.version));
  lines.push(field("answers that day before", confounds.answersThatDayBefore));
  lines.push(field("hour utc", confounds.hourUtc));
  lines.push(field("hour pacific", confounds.hourPacific));
  lines.push(field("changes already asked", confounds.changesAlreadyRequested));
  lines.push(field("prior cases on document", confounds.priorCasesOnDocument));
  lines.push(field("case kind", confounds.caseKind));
  if (!replayed.complete) {
    const unread = replayed.documents.filter((document) => document.refusal !== null).length;
    lines.push(
      "",
      `no record of what the person saw: ${unread} of ${replayed.documents.length} documents` +
        " could not be read at the version the ask asked about"
    );
  }
  return lines;
}

async function replay(flags: Flags): Promise<number> {
  const corpus = await readCorpus(flags.corpus);
  let row: CaseRow | undefined;
  if (flags.caseId !== undefined) {
    row = corpus.byCaseId.get(flags.caseId);
    if (row === undefined) {
      throw new UsageError(`${corpus.directory} holds no case ${flags.caseId}`);
    }
  } else {
    const approvals = corpus.rows.filter((one) => one.kind === "approval");
    const seed = flags.seed ?? newSeed();
    row = pickAtRandom(approvals, seed);
    if (row === undefined) throw new CorpusError(`${corpus.directory} holds no approval case`);
    console.log(`seed ${seed} (--seed ${seed} picks this case again)`);
  }
  const replayed = await replayCase(row);
  console.log(caseLines(replayed, flags.printWhole).join("\n"));
  return replayed.complete ? 0 : 1;
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
    return await replay(flags);
  } catch (error) {
    if (error instanceof CannotRunError) {
      console.error(`${error.name}: ${error.message}`);
      return 2;
    }
    throw error;
  }
}

// Only when run: a recipe that imports `replayCase` must not run a command.
if (import.meta.main) process.exit(await main(Bun.argv.slice(2)));
