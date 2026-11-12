import { createHash } from "node:crypto";
import { mkdir, readFile, writeFile } from "node:fs/promises";
import { join } from "node:path";
import { parseArgs } from "node:util";
import type { CaseKind, CaseRow } from "./build-corpus";
import {
  CorpusError,
  listOf,
  newSeed,
  OutputPathError,
  outDirectory,
  readCorpus,
  recordOf,
  stringOf,
  UsageError,
} from "./corpus-io";
import {
  type DispatchConfig,
  DispatchConfigError,
  DispatchRefusedError,
  DispatchUnreachableError,
  dispatchConfig,
  readJson,
} from "./dispatch-read";
import {
  type IssueFacts,
  type NeverDecideAnswer,
  type NeverDecideKind,
  neverDecide,
} from "./never-decide";
import { renderCase, renderDocument } from "./render";
import { replayCase } from "./replay";

/**
 * Draws the post-cut cases of a corpus into the four slices the pre-analysis plan's table names:
 * fit, report, anchor and reserve (dispatch://INFRA-2897/artifact/plan-of-analysis-md@v2, "The
 * slices").
 *
 * A case's group is its lead document for an approval request and its issue for a question, and
 * a whole group goes into one slice, so no document is both fitted on and reported. A group whose
 * size would take a slice past its target goes to the next slice, which is why the targets are
 * reached to within one group. The draw is at random under a seed it prints, so the same seed
 * over the same corpus draws the same slices.
 *
 * Three kinds of case never reach the draw. A question offering fewer than two options cannot be
 * judged as a choice, and a question whose answer names no option cannot be scored against what
 * the person chose, since set equality with an empty answer is nothing an arm could agree with.
 * A case the never-decide rule holds is one the shipped skill would refuse, and a coverage
 * number counting it would be larger than anything the skill can deliver; the counts and the
 * kinds held are printed and written.
 *
 * An approval case whose answer names no option stops the whole draw instead: every approval
 * case of this corpus carries the option the person chose, so one that does not is the corpus
 * read wrongly and not a case to pass over.
 *
 * What is pinned and what is live. The cases are the corpus's, and each document is read at the
 * version the corpus pins, so those are the same bytes on every run. The one live reading is
 * the list of parked roots carrying the `legion` label, which the never-decide rule needs and
 * which Dispatch answers as it stands now: `draw.json` writes that list, with the time it was
 * read, beside its count. `--parked-roots <file>` reads the list from a file in the shape
 * `draw.json` writes instead of from Dispatch, so a re-run that must reproduce an earlier draw
 * passes both `--seed` and `--parked-roots <that draw's draw.json>` and then turns on the
 * corpus and the seed alone.
 *
 * The anchor slice is written with a descriptor per case - the ask, the lead document, the
 * version requested and the SHA-256 of that document as rendered - so a later round can prove it
 * scored the same cases over the same bytes.
 */

const USAGE = `usage: bun scripts/approval-eval/draw.ts --corpus <dir> --cut <date> --out <dir> \\
  [--seed <n>] [--parked-roots <file>]

Draws the post-cut cases of a corpus into the fit, report, anchor and reserve slices.

Options:
  --corpus <dir>         Required. A directory holding corpus.jsonl and snapshot.json.
  --cut <date>           Required. An ISO 8601 time, for example 2026-11-05T00:00:00Z. A case
                         answered at or after it is drawn; a case answered before it is
                         training.
  --out <dir>            Required. Where slices.json, anchor-descriptors.json and draw.json are
                         written. An absolute path outside this repository's workspace.
  --seed <n>             The seed of the draw. With none, one is made and printed.
  --parked-roots <file>  The parked roots carrying the legion label, read from this file rather
                         than from Dispatch. The shape is the one draw.json writes, so an
                         earlier draw's draw.json names its own list. With --seed, this makes a
                         re-run reproduce that draw from the corpus alone.

Exit status: 0 when the draw was written, 2 when it could not run.`;

/** How many cases of each kind each slice is drawn for. The rest is the reserve. */
export interface SliceTargets {
  fit: number;
  report: number;
  anchor: number;
}

/** The targets of the plan's table, by kind of case. */
export const TARGETS: Readonly<Record<CaseKind, SliceTargets>> = {
  approval: { fit: 80, report: 250, anchor: 48 },
  question: { fit: 20, report: 50, anchor: 12 },
};

/** The four slices, in the order a group is offered to them. */
export const SLICES: readonly SliceName[] = ["fit", "report", "anchor", "reserve"];

/** One slice of the draw. */
export type SliceName = "fit" | "report" | "anchor" | "reserve";

/** How many option a question must offer to be judged as a choice at all. */
const LEAST_OPTIONS = 2;

/** How many cases are read from Dispatch at once while the post-cut cases are rendered. */
const RENDER_CONCURRENCY = 8;

/** How many issues one page of the issue list asks for, which is that route's own maximum. */
const ISSUE_PAGE = 250;

/** How many case ids of each slice the command prints. */
const IDS_SHOWN = 10;

interface Flags {
  corpus: string;
  cut: string;
  out: string;
  seed: number | undefined;
  parkedRoots: string | undefined;
}

/** One post-cut case as the draw was given it: the row, the rule's answer and its document. */
export interface DrawCase {
  row: CaseRow;
  /** What the never-decide rule said over this case's rendered evidence. */
  hold: NeverDecideAnswer;
  /** The lead document of an approval request, and null for a question. */
  documentId: string | null;
  /** The version that request asked about. */
  requestedVersion: number | null;
  /** The SHA-256 of the lead document as rendered, or of the rendered case where there is none. */
  sha256: string;
}

/** What a later round reads to prove it scored the same case over the same bytes. */
export interface AnchorDescriptor {
  caseId: string;
  askId: string;
  kind: CaseKind;
  documentId: string | null;
  requestedVersion: number | null;
  sha256: string;
}

/** Why a case the rule did not hold could still not be drawn. */
export type UndrawableKind = "too-few-options" | "no-option-chosen";

/** The undrawable kinds, in the order they are counted and printed. */
const UNDRAWABLE_KINDS: readonly UndrawableKind[] = ["too-few-options", "no-option-chosen"];

/** What each undrawable kind is called where a count of it is printed. */
const UNDRAWABLE_NAMES: Readonly<Record<UndrawableKind, string>> = {
  "too-few-options": "questions with too few options",
  "no-option-chosen": "questions answered with no option",
};

/** The corpus is not what the draw takes it for, so no draw over it would mean anything. */
export class DrawError extends Error {
  override readonly name = "DrawError";
}

/** One case left out before the draw, and why. */
export interface ExcludedCase {
  caseId: string;
  kind: CaseKind;
  reason: string;
  /** The kind of hold, for a case the never-decide rule held. */
  heldKind?: NeverDecideKind;
  /** Which rule left it out, for a case no arm could be scored on. */
  undrawableKind?: UndrawableKind;
}

/** What one draw did: the slices, the counts they reached and what never reached them. */
export interface Draw {
  seed: number;
  targets: Readonly<Record<CaseKind, SliceTargets>>;
  slices: Record<SliceName, string[]>;
  counts: Record<SliceName, Record<CaseKind, number>>;
  /** How many groups each slice took, by kind. */
  groups: Record<CaseKind, number>;
  held: ExcludedCase[];
  heldByKind: Record<string, number>;
  undrawable: ExcludedCase[];
  undrawableByKind: Record<UndrawableKind, number>;
  anchors: AnchorDescriptor[];
}

/** One group of cases, which goes into one slice whole. */
interface Group {
  key: string;
  kind: CaseKind;
  cases: DrawCase[];
}

/** mulberry32: the seeded stream the shuffle draws from, so a seed reproduces a draw. */
function randomStream(seed: number): () => number {
  let state = Math.trunc(seed) >>> 0;
  return () => {
    state = (state + 0x6d2b79f5) >>> 0;
    let drawn = state;
    drawn = Math.imul(drawn ^ (drawn >>> 15), 1 | drawn);
    drawn = (drawn + Math.imul(drawn ^ (drawn >>> 7), 61 | drawn)) ^ drawn;
    return ((drawn ^ (drawn >>> 14)) >>> 0) / 0x1_0000_0000;
  };
}

/** `items` in a random order under `seed`, by a Fisher-Yates walk of that one stream. */
function shuffled<T>(items: readonly T[], seed: number): T[] {
  const order = [...items];
  const random = randomStream(seed);
  for (let index = order.length - 1; index > 0; index -= 1) {
    const swap = Math.floor(random() * (index + 1));
    const here = order[index];
    const there = order[swap];
    if (here === undefined || there === undefined) continue;
    order[index] = there;
    order[swap] = here;
  }
  return order;
}

/** The lead document of an approval request: the first it names an artifact and a version for. */
export function leadDocumentOf(
  row: CaseRow
): { artifactId: string; requestedVersion: number } | null {
  for (const document of row.approval?.documents ?? []) {
    if (document.artifactId !== null && document.requestedVersion !== null) {
      return { artifactId: document.artifactId, requestedVersion: document.requestedVersion };
    }
  }
  return null;
}

/**
 * The group a case belongs to: its lead document for an approval request, its issue for a
 * question. A case naming neither is its own group, since nothing else is known to share it.
 */
function groupKeyOf(drawCase: DrawCase): string {
  const row = drawCase.row;
  if (row.kind === "approval") {
    return `document:${drawCase.documentId ?? row.askId}`;
  }
  return `issue:${row.issueKey ?? row.askId}`;
}

/** The slices and the counts each starts at. */
function emptyDraw(seed: number): Draw {
  const counts = {} as Record<SliceName, Record<CaseKind, number>>;
  const slices = {} as Record<SliceName, string[]>;
  for (const slice of SLICES) {
    counts[slice] = { approval: 0, question: 0 };
    slices[slice] = [];
  }
  return {
    seed,
    targets: TARGETS,
    slices,
    counts,
    groups: { approval: 0, question: 0 },
    held: [],
    heldByKind: {},
    undrawable: [],
    undrawableByKind: { "too-few-options": 0, "no-option-chosen": 0 },
    anchors: [],
  };
}

/**
 * The draw itself: the cases the rule holds and the questions no arm could be scored on are left
 * out, the rest are grouped, the groups are shuffled under `seed`, and each group goes into the
 * first slice its whole size still fits.
 *
 * It throws `DrawError` over an approval case whose answer names no option, which is the corpus
 * read wrongly rather than a case to leave out.
 */
export function drawSlices(cases: readonly DrawCase[], seed: number): Draw {
  const draw = emptyDraw(seed);
  const drawable: DrawCase[] = [];
  const leaveOut = (row: CaseRow, undrawableKind: UndrawableKind, reason: string): void => {
    draw.undrawable.push({ caseId: row.caseId, kind: row.kind, reason, undrawableKind });
    draw.undrawableByKind[undrawableKind] += 1;
  };
  for (const drawCase of cases) {
    const row = drawCase.row;
    if (row.kind === "approval" && row.selected.length === 0) {
      throw new DrawError(
        `the approval case ${row.caseId} names no option the person chose, which the corpus ` +
          "build cannot produce: the draw stops rather than passing over it"
      );
    }
    if (drawCase.hold.held) {
      const heldKind = drawCase.hold.kind;
      draw.held.push({
        caseId: row.caseId,
        kind: row.kind,
        reason: drawCase.hold.reason,
        ...(heldKind === null ? {} : { heldKind }),
      });
      const name = heldKind ?? "unnamed";
      draw.heldByKind[name] = (draw.heldByKind[name] ?? 0) + 1;
      continue;
    }
    if (row.kind === "question" && row.options.length < LEAST_OPTIONS) {
      leaveOut(
        row,
        "too-few-options",
        `a question offering ${row.options.length} options cannot be judged as a choice`
      );
      continue;
    }
    if (row.kind === "question" && row.selected.length === 0) {
      leaveOut(
        row,
        "no-option-chosen",
        "a question the person answered with no option cannot be scored by set equality with " +
          "what they chose"
      );
      continue;
    }
    drawable.push(drawCase);
  }
  const groups = new Map<string, Group>();
  // Sorted by case id, then grouped by sorted key: the draw turns on the seed alone and not on
  // the order `corpus.jsonl` happens to hold its cases in.
  for (const drawCase of [...drawable].sort((one, other) =>
    one.row.caseId < other.row.caseId ? -1 : 1
  )) {
    const key = groupKeyOf(drawCase);
    const group = groups.get(key);
    if (group === undefined) {
      groups.set(key, { key, kind: drawCase.row.kind, cases: [drawCase] });
    } else {
      group.cases.push(drawCase);
    }
  }
  const ordered = [...groups.values()].sort((one, other) => (one.key < other.key ? -1 : 1));
  for (const group of shuffled(ordered, seed)) {
    draw.groups[group.kind] += 1;
    const targets = TARGETS[group.kind];
    let taken: SliceName = "reserve";
    for (const slice of SLICES) {
      if (slice === "reserve") break;
      const room = targets[slice] - draw.counts[slice][group.kind];
      if (group.cases.length <= room) {
        taken = slice;
        break;
      }
    }
    draw.counts[taken][group.kind] += group.cases.length;
    for (const drawCase of group.cases) {
      draw.slices[taken].push(drawCase.row.caseId);
      if (taken !== "anchor") continue;
      draw.anchors.push({
        caseId: drawCase.row.caseId,
        askId: drawCase.row.askId,
        kind: drawCase.row.kind,
        documentId: drawCase.documentId,
        requestedVersion: drawCase.requestedVersion,
        sha256: drawCase.sha256,
      });
    }
  }
  return draw;
}

function cutTime(cut: string): number {
  const time = Date.parse(cut);
  if (Number.isNaN(time)) {
    throw new UsageError(`--cut takes an ISO 8601 time, and ${cut} is not one`);
  }
  return time;
}

function answeredAt(row: CaseRow): number {
  const time = Date.parse(row.answeredAt);
  if (Number.isNaN(time)) {
    throw new CorpusError(`case ${row.caseId} was answered at ${row.answeredAt}, which is no time`);
  }
  return time;
}

/**
 * The issue facts of one issue, as the issue list route answers them (`primary_artifact_id`) or
 * as a pinned list holds them (`primaryArtifactId`), so one reader serves both.
 */
function issueFactsOf(value: unknown): { key: string; facts: IssueFacts } | null {
  const issue = recordOf(value);
  const key = stringOf(issue?.key);
  const status = stringOf(issue?.status);
  if (key === null || status === null) return null;
  return {
    key,
    facts: {
      parent: stringOf(issue?.parent),
      labels: listOf(issue?.labels)
        .map(stringOf)
        .filter((label): label is string => label !== null),
      status,
      primaryArtifactId: stringOf(issue?.primary_artifact_id) ?? stringOf(issue?.primaryArtifactId),
    },
  };
}

/** The parked roots carrying the `legion` label, as `draw.json` writes them and reads them. */
export interface ParkedRoots {
  /** When the list was read from Dispatch, as the run that read it stamped it. */
  readAt: string;
  /** Where this run took it from: `dispatch`, or the file it was pinned from. */
  source: string;
  /** One entry per issue: its key, and the facts the never-decide rule reads. */
  issues: Array<{ key: string } & IssueFacts>;
}

/**
 * Every issue carrying the `legion` label at a parked status, read now. That is the fact the
 * never-decide rule's parked-root kind needs and the rendered case does not carry; it is read
 * here, in the command, and handed to the rule as a value. Dispatch's own list answers it in one
 * page per status, which is why no case is read for on its own. The list is sorted by key, so
 * what `draw.json` writes of one reading is the same bytes however the pages came back.
 */
async function parkedLegionRoots(config: DispatchConfig): Promise<ParkedRoots> {
  const issues: Array<{ key: string } & IssueFacts> = [];
  const seen = new Set<string>();
  for (const status of ["backlog", "icebox"]) {
    for (let offset = 0; ; offset += ISSUE_PAGE) {
      const path = `/api/v1/issues?label=legion&status=${status}&limit=${ISSUE_PAGE}&offset=${offset}`;
      const answer = recordOf(await readJson<unknown>(path, config));
      const page = listOf(answer?.issues);
      for (const value of page) {
        const read = issueFactsOf(value);
        if (read === null || seen.has(read.key)) continue;
        seen.add(read.key);
        issues.push({ key: read.key, ...read.facts });
      }
      const total = answer?.total;
      if (page.length < ISSUE_PAGE || typeof total !== "number" || offset + ISSUE_PAGE >= total) {
        break;
      }
    }
  }
  issues.sort((one, other) => (one.key < other.key ? -1 : 1));
  return { readAt: new Date().toISOString(), source: "dispatch", issues };
}

/**
 * The same list, pinned: read from a file in the shape `draw.json` writes, which is either that
 * whole file or the `parkedLegionRoots` object inside it. The reading time the file carries is
 * kept, because it is when the list was true; the source becomes the file.
 */
async function pinnedParkedRoots(path: string): Promise<ParkedRoots> {
  let parsed: unknown;
  try {
    parsed = JSON.parse(await readFile(path, "utf8"));
  } catch {
    throw new UsageError(`--parked-roots ${path} could not be read as JSON`);
  }
  const record = recordOf(parsed);
  const held = recordOf(record?.parkedLegionRoots) ?? record;
  const listed = listOf(held?.issues);
  if (held === null || listed.length === 0) {
    throw new UsageError(
      `--parked-roots ${path} names no parked roots: it takes the shape draw.json writes, ` +
        "which is an object with `issues` or a whole draw.json holding `parkedLegionRoots`"
    );
  }
  const issues: Array<{ key: string } & IssueFacts> = [];
  for (const value of listed) {
    const read = issueFactsOf(value);
    if (read === null) throw new UsageError(`--parked-roots ${path} holds an entry with no key`);
    issues.push({ key: read.key, ...read.facts });
  }
  issues.sort((one, other) => (one.key < other.key ? -1 : 1));
  return { readAt: stringOf(held.readAt) ?? "unknown", source: path, issues };
}

/** The facts by issue key, which is how the rule is handed them. */
function factsByKey(parked: ParkedRoots): Map<string, IssueFacts> {
  const facts = new Map<string, IssueFacts>();
  for (const issue of parked.issues) {
    facts.set(issue.key, {
      parent: issue.parent,
      labels: issue.labels,
      status: issue.status,
      primaryArtifactId: issue.primaryArtifactId,
    });
  }
  return facts;
}

/** One post-cut case rendered and put to the rule, which is what the draw is given. */
async function drawCaseOf(
  row: CaseRow,
  issues: ReadonlyMap<string, IssueFacts>,
  config: DispatchConfig
): Promise<{ drawCase: DrawCase; unread: number }> {
  const replayed = row.kind === "approval" ? await replayCase(row, config) : null;
  const documents = replayed?.documents ?? [];
  const text = renderCase(row, documents);
  // The descriptor's digest is over the lead document as the judge is shown it, so a later
  // round that renders other bytes sees it. A case with no document is digested whole.
  const lead = documents.find((document) => document.markdown !== null) ?? null;
  const digested =
    lead === null || lead.markdown === null ? text : renderDocument(lead.markdown, row.answeredAt);
  const sha256 = createHash("sha256").update(digested, "utf8").digest("hex");
  const issue = row.issueKey === null ? null : (issues.get(row.issueKey) ?? null);
  return {
    drawCase: {
      row,
      hold: neverDecide({ text, row, issue }),
      documentId: lead?.artifactId ?? null,
      requestedVersion: lead?.requestedVersion ?? null,
      sha256,
    },
    unread: documents.filter((document) => document.refusal !== null).length,
  };
}

/** The post-cut cases, rendered and put to the rule, `RENDER_CONCURRENCY` of them at a time. */
async function drawCasesOf(
  rows: readonly CaseRow[],
  issues: ReadonlyMap<string, IssueFacts>,
  config: DispatchConfig
): Promise<{ cases: DrawCase[]; unread: number }> {
  const cases = new Array<DrawCase>(rows.length);
  let unread = 0;
  let next = 0;
  const worker = async (): Promise<void> => {
    for (let index = next++; index < rows.length; index = next++) {
      const row = rows[index];
      if (row === undefined) return;
      const one = await drawCaseOf(row, issues, config);
      cases[index] = one.drawCase;
      unread += one.unread;
    }
  };
  await Promise.all(
    Array.from({ length: Math.min(RENDER_CONCURRENCY, Math.max(1, rows.length)) }, worker)
  );
  return { cases, unread };
}

function readFlags(argv: readonly string[]): Flags {
  let values: {
    corpus?: string;
    cut?: string;
    out?: string;
    seed?: string;
    "parked-roots"?: string;
  };
  try {
    ({ values } = parseArgs({
      args: [...argv],
      options: {
        corpus: { type: "string" },
        cut: { type: "string" },
        out: { type: "string" },
        seed: { type: "string" },
        "parked-roots": { type: "string" },
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
    seed,
    parkedRoots: values["parked-roots"]?.trim() || undefined,
  };
}

function countsTable(draw: Draw): string {
  const lines = [`  ${"slice".padEnd(10)}${"approval".padStart(10)}${"question".padStart(10)}`];
  for (const slice of SLICES) {
    const counts = draw.counts[slice];
    lines.push(
      `  ${slice.padEnd(10)}${String(counts.approval).padStart(10)}` +
        `${String(counts.question).padStart(10)}`
    );
  }
  for (const kind of ["approval", "question"] as const) {
    const targets = TARGETS[kind];
    lines.push(
      `  target ${kind.padEnd(10)}fit ${targets.fit}, report ${targets.report}, ` +
        `anchor ${targets.anchor}, reserve the rest`
    );
  }
  return lines.join("\n");
}

async function draw(flags: Flags): Promise<number> {
  const corpus = await readCorpus(flags.corpus);
  const directory = await outDirectory(flags.out);
  const cut = cutTime(flags.cut);
  const config = await dispatchConfig();
  const post = corpus.rows.filter((row) => answeredAt(row) >= cut);
  const seed = flags.seed ?? newSeed();
  console.log(`corpus            ${corpus.directory}`);
  console.log(`cut               ${new Date(cut).toISOString()}`);
  console.log(`post-cut cases    ${post.length}`);
  console.log(`seed              ${seed} (--seed ${seed} draws these slices again)`);
  const parked =
    flags.parkedRoots === undefined
      ? await parkedLegionRoots(config)
      : await pinnedParkedRoots(flags.parkedRoots);
  const issues = factsByKey(parked);
  console.log(`parked legion roots read  ${parked.issues.length}`);
  console.log(
    parked.source === "dispatch"
      ? `                  from Dispatch at ${parked.readAt}`
      : `                  pinned from ${parked.source}, read at ${parked.readAt}`
  );
  const rendered = await drawCasesOf(post, issues, config);
  const drawn = drawSlices(rendered.cases, seed);
  console.log("");
  console.log(countsTable(drawn));
  console.log("");
  console.log(
    `groups            ${drawn.groups.approval} approval, ${drawn.groups.question} question`
  );
  console.log(`held by the never-decide rule  ${drawn.held.length}`);
  for (const [kind, count] of Object.entries(drawn.heldByKind).sort()) {
    console.log(`  ${kind.padEnd(22)}${count}`);
  }
  console.log(`undrawable                     ${drawn.undrawable.length}`);
  for (const kind of UNDRAWABLE_KINDS) {
    console.log(`  ${UNDRAWABLE_NAMES[kind].padEnd(35)}${drawn.undrawableByKind[kind]}`);
  }
  console.log(`documents that could not be read ${rendered.unread}`);
  console.log("");
  for (const slice of SLICES) {
    const ids = drawn.slices[slice].slice(0, IDS_SHOWN);
    console.log(`first ${ids.length} of ${slice}`);
    for (const id of ids) console.log(`  ${id}`);
  }
  await mkdir(directory, { recursive: true });
  const write = async (name: string, value: unknown): Promise<void> => {
    await writeFile(join(directory, name), `${JSON.stringify(value, null, 2)}\n`, "utf8");
  };
  await write("slices.json", drawn.slices);
  await write("anchor-descriptors.json", drawn.anchors);
  await write("draw.json", {
    corpus: corpus.directory,
    cut: new Date(cut).toISOString(),
    seed,
    targets: drawn.targets,
    counts: drawn.counts,
    groups: drawn.groups,
    postCutCases: post.length,
    documentsUnread: rendered.unread,
    // The one input of the draw that is not the corpus: the parked labelled roots, which the
    // never-decide rule reads. The list is written whole, with the time it was read, so
    // `--parked-roots <this file>` draws these slices again whatever Dispatch shows later.
    parkedLegionRootsRead: parked.issues.length,
    parkedLegionRoots: parked,
    excluded: {
      held: { total: drawn.held.length, byKind: drawn.heldByKind, cases: drawn.held },
      undrawable: {
        total: drawn.undrawable.length,
        byKind: drawn.undrawableByKind,
        cases: drawn.undrawable,
      },
    },
  });
  console.log("");
  console.log(`written           ${join(directory, "slices.json")}`);
  console.log(`                  ${join(directory, "anchor-descriptors.json")}`);
  console.log(`                  ${join(directory, "draw.json")}`);
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
    return await draw(flags);
  } catch (error) {
    if (
      error instanceof UsageError ||
      error instanceof OutputPathError ||
      error instanceof CorpusError ||
      error instanceof DrawError ||
      error instanceof DispatchConfigError ||
      error instanceof DispatchRefusedError ||
      error instanceof DispatchUnreachableError
    ) {
      console.error(`${error.name}: ${error.message}`);
      return 2;
    }
    throw error;
  }
}

// Only when run: a test that imports `drawSlices` must not run a command.
if (import.meta.main) process.exit(await main(Bun.argv.slice(2)));
