import { createHash } from "node:crypto";
import { mkdir, readFile, writeFile } from "node:fs/promises";
import { join } from "node:path";
import { parseArgs } from "node:util";
import type { CaseKind, CaseRow } from "./build-corpus";
import {
  answeredTime,
  CannotRunError,
  cutTime,
  listOf,
  newSeed,
  outDirectory,
  pool,
  randomStream,
  readCorpus,
  recordOf,
  stringOf,
  UsageError,
} from "./corpus-io";
import { type DispatchConfig, dispatchConfig, readJson } from "./dispatch-read";
import {
  type BacklogRoots,
  type DrawnSlices,
  type DrawRecord,
  drawPath,
  issueFactsOf,
  rootFactsOf,
  SLICES,
  type SliceName,
  slicesPath,
} from "./draw-io";
import {
  DEFAULT_NEVER_DECIDE_MODEL,
  type IssueFacts,
  type NeverDecideAnswer,
  type NeverDecideKind,
  type NeverDecideOptions,
  neverDecide,
  neverDecideDefinition,
} from "./never-decide";
import { renderCase, renderDocument } from "./render";
import { replayCase } from "./replay";

/**
 * Draws the post-cut cases of a corpus into the four slices the pre-analysis plan's table names:
 * fit, report, anchor and reserve (dispatch://INFRA-2897/artifact/plan-of-analysis-md@v2, "The
 * slices").
 *
 * A case's group is its issue, and a whole group goes into one slice, so two decisions of one
 * subject never land on opposite sides of a split: a document belongs to one issue, so this
 * grouping holds every case of a document together as well. A group whose size would take a
 * slice past its target goes to the next slice, which is why the targets are reached to within
 * one group. The draw is at random under a seed it prints, so the same seed over the same
 * corpus draws the same slices. `draw.json` records the key the groups were taken under and how
 * many groups there were, since a later reading of the slices turns on which key was used.
 *
 * Four kinds of case never reach the draw. A question offering fewer than two options cannot be
 * judged as a choice, and a question whose answer names no option cannot be scored against what
 * the person chose, since set equality with an empty answer is nothing an arm could agree with.
 * A case the never-decide rule holds is one the shipped skill would refuse, and a coverage
 * number counting it would be larger than anything the skill can deliver; the counts and the
 * kinds held are printed and written. And a case whose answer from the rule could not be read
 * is left out as well, because a rule about what must not be decided may not decide by
 * accident; those are counted and named too.
 *
 * An approval case whose answer names no option stops the whole draw instead: every approval
 * case of this corpus carries the option the person chose, so one that does not is the corpus
 * read wrongly and not a case to pass over.
 *
 * What is pinned and what is live. The cases are the corpus's, and each document is read at the
 * version the corpus pins, so those are the same bytes on every run. The one live reading is
 * the list of roots carrying the `legion` label that stand on the backlog or in icebox, which
 * the never-decide rule needs and which Dispatch answers as it stands now: `draw.json` writes
 * that list, with the time it was read, beside its count. `--backlog-roots <file>` reads the
 * list from a file in the shape `draw.json` writes instead of from Dispatch, so a re-run that
 * must reproduce an earlier draw passes both `--seed` and `--backlog-roots <that draw's
 * draw.json>` and then turns on the corpus and the seed alone.
 *
 * The rule itself is a model call per case. `--cache <dir>` keeps those answers between runs,
 * keyed on the prose of the definition among other things, so a redraw of a frozen definition
 * pays for none of them again and a changed definition is asked afresh by construction.
 *
 * The anchor slice is written with a descriptor per case - the ask, the lead document, the
 * version requested and the SHA-256 of that document as rendered - so a later round can prove it
 * scored the same cases over the same bytes.
 */

const USAGE = `usage: bun scripts/approval-eval/draw.ts --corpus <dir> --cut <date> --out <dir> \\
  [--seed <n>] [--backlog-roots <file>] [--cache <dir>]

Draws the post-cut cases of a corpus into the fit, report, anchor and reserve slices.

Options:
  --corpus <dir>          Required. A directory holding corpus.jsonl and snapshot.json.
  --cut <date>            Required. An ISO 8601 time, for example 2026-11-05T00:00:00Z. A case
                          answered at or after it is drawn; a case answered before it is
                          training.
  --out <dir>             Required. Where slices.json, anchor-descriptors.json and draw.json
                          are written. An absolute path outside this repository's workspace.
  --seed <n>              The seed of the draw. With none, one is made and printed.
  --backlog-roots <file>  The roots carrying the legion label that stand on the backlog or in
                          icebox, read from this file rather than from Dispatch. The shape is
                          the one draw.json writes, so an earlier draw's draw.json names its
                          own list. With --seed, this makes a re-run reproduce that draw from
                          the corpus alone.
  --cache <dir>           Where the never-decide rule's answers are kept between runs. Without
                          it every case is asked of the model again.

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

/** The three slices every number of a round is read over, in the order they are printed. */
export const SCORED_SLICES = ["fit", "report", "anchor"] as const;

/** One of the three slices a round scores and a manifest is written for. */
export type ScoredSlice = (typeof SCORED_SLICES)[number];

/** The kinds of case, in the order every count of them is held and printed. */
export const KINDS: readonly CaseKind[] = ["approval", "question"];

/** What a case is grouped by, which `draw.json` records and a later reading of it reads. */
export const GROUP_KEY = "issue";

/** How many option a question must offer to be judged as a choice at all. */
const LEAST_OPTIONS = 2;

/**
 * How many cases are read from Dispatch and put to the never-decide rule at once. The rule is
 * one model call per case, so this is what the whole draw's wall time turns on.
 */
const RENDER_CONCURRENCY = 16;

/** How many issues one page of the issue list asks for, which is that route's own maximum. */
const ISSUE_PAGE = 250;

/** How many case ids of each slice the command prints. */
const IDS_SHOWN = 10;

interface Flags {
  corpus: string;
  cut: string;
  out: string;
  seed: number | undefined;
  backlogRoots: string | undefined;
  cache: string | undefined;
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
export type UndrawableKind = "too-few-options" | "no-option-chosen" | "rule-unreadable";

/** The undrawable kinds, in the order they are counted and printed. */
const UNDRAWABLE_KINDS: readonly UndrawableKind[] = [
  "too-few-options",
  "no-option-chosen",
  "rule-unreadable",
];

/** What each undrawable kind is called where a count of it is printed. */
const UNDRAWABLE_NAMES: Readonly<Record<UndrawableKind, string>> = {
  "too-few-options": "questions with too few options",
  "no-option-chosen": "questions answered with no option",
  "rule-unreadable": "unreadable never-decide answers",
};

/** The corpus is not what the draw takes it for, so no draw over it would mean anything. */
export class DrawError extends CannotRunError {
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
  slices: DrawnSlices;
  counts: Record<SliceName, Record<CaseKind, number>>;
  /** What the cases were grouped by, a whole group going into one slice. */
  groupKey: string;
  /** How many groups there were under that key. */
  groupCount: number;
  /** How many of those groups hold a case of each kind, a group being able to hold both. */
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
  /** How many cases of each kind the group holds, which is what a slice must have room for. */
  counts: Record<CaseKind, number>;
  cases: DrawCase[];
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

/**
 * The group a case belongs to: its issue, whether it is an approval request or a question. A
 * document belongs to one issue, so this key holds together every case the lead document held
 * together and the cases of two documents of one issue besides. A case naming no issue is its
 * own group, since nothing else is known to share it.
 */
function groupKeyOf(drawCase: DrawCase): string {
  const row = drawCase.row;
  return `${GROUP_KEY}:${row.issueKey ?? row.askId}`;
}

/** The slices and the counts each starts at. */
function emptyDraw(seed: number): Draw {
  const counts = {} as Record<SliceName, Record<CaseKind, number>>;
  const slices = {} as DrawnSlices;
  for (const slice of SLICES) {
    counts[slice] = { approval: 0, question: 0 };
    slices[slice] = [];
  }
  return {
    seed,
    targets: TARGETS,
    slices,
    counts,
    groupKey: GROUP_KEY,
    groupCount: 0,
    groups: { approval: 0, question: 0 },
    held: [],
    heldByKind: {},
    undrawable: [],
    undrawableByKind: { "too-few-options": 0, "no-option-chosen": 0, "rule-unreadable": 0 },
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
    if (drawCase.hold.fault !== null) {
      // Neither held nor decided: a rule about what must not be decided may not decide by
      // accident, so the case leaves the draw and is named with what came back.
      leaveOut(
        row,
        "rule-unreadable",
        `the never-decide answer could not be read: ${drawCase.hold.fault}`
      );
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
      groups.set(key, {
        key,
        counts: { approval: 0, question: 0, [drawCase.row.kind]: 1 },
        cases: [drawCase],
      });
    } else {
      group.counts[drawCase.row.kind] += 1;
      group.cases.push(drawCase);
    }
  }
  const ordered = [...groups.values()].sort((one, other) => (one.key < other.key ? -1 : 1));
  for (const group of shuffled(ordered, seed)) {
    draw.groupCount += 1;
    for (const kind of KINDS) if (group.counts[kind] > 0) draw.groups[kind] += 1;
    let taken: SliceName = "reserve";
    for (const slice of SLICES) {
      if (slice === "reserve") break;
      // A group holding both kinds goes where both still fit: one group, one slice, whole.
      const fits = KINDS.every(
        (kind) => group.counts[kind] <= TARGETS[kind][slice] - draw.counts[slice][kind]
      );
      if (fits) {
        taken = slice;
        break;
      }
    }
    for (const kind of KINDS) draw.counts[taken][kind] += group.counts[kind];
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

/**
 * Every issue carrying the `legion` label that stands on the backlog or in icebox, read now.
 * That is the fact the never-decide rule's waiting-root kind needs and the rendered case does
 * not carry; it is read here, in the command, and handed to the rule as a value. Dispatch's own
 * list answers it in one page per status, which is why no case is read for on its own. The list
 * is sorted by key, so what `draw.json` writes of one reading is the same bytes however the
 * pages came back.
 */
async function backlogLegionRoots(config: DispatchConfig): Promise<BacklogRoots> {
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
 * whole file or the `backlogLegionRoots` object inside it. The reading time the file carries is
 * kept, because it is when the list was true; the source becomes the file.
 *
 * This reads a draw-shaped file itself rather than through `draw-io.ts`, and stays that way.
 * What it reads is not a corpus's own draw but a file a person named on the command line, so
 * every way it can fail is a usage error about `--backlog-roots` and says what that flag takes.
 * Reading it through the owner of `draw.json` would answer a corpus error over a flag, and a
 * refusal that names the wrong thing is a refusal that has lost its words.
 */
async function pinnedBacklogRoots(path: string): Promise<BacklogRoots> {
  let parsed: unknown;
  try {
    parsed = JSON.parse(await readFile(path, "utf8"));
  } catch {
    throw new UsageError(`--backlog-roots ${path} could not be read as JSON`);
  }
  const record = recordOf(parsed);
  const held = recordOf(record?.backlogLegionRoots) ?? record;
  const listed = listOf(held?.issues);
  if (held === null || listed.length === 0) {
    throw new UsageError(
      `--backlog-roots ${path} names no such roots: it takes the shape draw.json writes, ` +
        "which is an object with `issues` or a whole draw.json holding `backlogLegionRoots`"
    );
  }
  const issues: Array<{ key: string } & IssueFacts> = [];
  for (const value of listed) {
    const read = issueFactsOf(value);
    if (read === null) throw new UsageError(`--backlog-roots ${path} holds an entry with no key`);
    issues.push({ key: read.key, ...read.facts });
  }
  issues.sort((one, other) => (one.key < other.key ? -1 : 1));
  return { readAt: stringOf(held.readAt) ?? "unknown", source: path, issues };
}

/** One post-cut case rendered and put to the rule, which is what the draw is given. */
async function drawCaseOf(
  row: CaseRow,
  issues: ReadonlyMap<string, IssueFacts>,
  config: DispatchConfig,
  options: NeverDecideOptions
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
      hold: await neverDecide(
        {
          text,
          row,
          issue,
          documentId: lead?.artifactId ?? null,
          documentVersion: lead?.requestedVersion ?? null,
        },
        options
      ),
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
  config: DispatchConfig,
  options: NeverDecideOptions
): Promise<{ cases: DrawCase[]; unread: number }> {
  const cases = new Array<DrawCase>(rows.length);
  let unread = 0;
  // Over the indices, so each case is written back where it was read from and the order of
  // `cases` is the order of `rows`, whichever worker got there first.
  await pool([...rows.keys()], RENDER_CONCURRENCY, async (index) => {
    const row = rows[index] as CaseRow;
    const one = await drawCaseOf(row, issues, config, options);
    cases[index] = one.drawCase;
    unread += one.unread;
  });
  return { cases, unread };
}

function readFlags(argv: readonly string[]): Flags {
  let values: {
    corpus?: string;
    cut?: string;
    out?: string;
    seed?: string;
    "backlog-roots"?: string;
    cache?: string;
  };
  try {
    ({ values } = parseArgs({
      args: [...argv],
      options: {
        corpus: { type: "string" },
        cut: { type: "string" },
        out: { type: "string" },
        seed: { type: "string" },
        "backlog-roots": { type: "string" },
        cache: { type: "string" },
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
    backlogRoots: values["backlog-roots"]?.trim() || undefined,
    cache: values.cache?.trim() || undefined,
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
  const post = corpus.rows.filter((row) => answeredTime(row) >= cut);
  const seed = flags.seed ?? newSeed();
  console.log(`corpus            ${corpus.directory}`);
  console.log(`cut               ${new Date(cut).toISOString()}`);
  console.log(`post-cut cases    ${post.length}`);
  console.log(`seed              ${seed} (--seed ${seed} draws these slices again)`);
  const roots =
    flags.backlogRoots === undefined
      ? await backlogLegionRoots(config)
      : await pinnedBacklogRoots(flags.backlogRoots);
  const issues = rootFactsOf(roots);
  console.log(`legion roots on the backlog or in icebox  ${roots.issues.length}`);
  console.log(
    roots.source === "dispatch"
      ? `                  from Dispatch at ${roots.readAt}`
      : `                  pinned from ${roots.source}, read at ${roots.readAt}`
  );
  const definition = await neverDecideDefinition();
  const ruleOptions: NeverDecideOptions = {
    model: DEFAULT_NEVER_DECIDE_MODEL,
    cacheDirectory: flags.cache ?? null,
    definition,
  };
  console.log(`the rule          ${DEFAULT_NEVER_DECIDE_MODEL}, one call per case`);
  console.log(
    `cache             ${flags.cache ?? "none, so every case is asked of the model again"}`
  );
  const rendered = await drawCasesOf(post, issues, config, ruleOptions);
  const drawn = drawSlices(rendered.cases, seed);
  console.log("");
  console.log(countsTable(drawn));
  console.log("");
  console.log(
    `groups            ${drawn.groupCount} under the key ${drawn.groupKey}, of which ` +
      `${drawn.groups.approval} hold an approval case and ${drawn.groups.question} a question`
  );
  console.log(`held by the never-decide rule  ${drawn.held.length}`);
  for (const [kind, count] of Object.entries(drawn.heldByKind).sort()) {
    console.log(`  ${kind.padEnd(22)}${count}`);
  }
  console.log(`undrawable                     ${drawn.undrawable.length}`);
  for (const kind of UNDRAWABLE_KINDS) {
    console.log(`  ${UNDRAWABLE_NAMES[kind].padEnd(35)}${drawn.undrawableByKind[kind]}`);
  }
  const holds = rendered.cases.map((one) => one.hold);
  const cached = holds.filter((one) => one.source === "cache").length;
  const asked = holds.filter((one) => one.source === "model").length;
  console.log(
    `the rule's calls  ${asked} asked, ${cached} from the cache, ` +
      `${holds.length - asked - cached} settled by the issue's facts`
  );
  console.log(
    `the rule's tokens ${holds.reduce((total, one) => total + (one.inputTokens ?? 0), 0)} read, ` +
      `${holds.reduce((total, one) => total + (one.outputTokens ?? 0), 0)} written`
  );
  console.log(`documents that could not be read ${rendered.unread}`);
  console.log("");
  for (const slice of SLICES) {
    const ids = drawn.slices[slice].slice(0, IDS_SHOWN);
    console.log(`first ${ids.length} of ${slice}`);
    for (const id of ids) console.log(`  ${id}`);
  }
  await mkdir(directory, { recursive: true });
  const write = async (path: string, value: unknown): Promise<void> => {
    await writeFile(path, `${JSON.stringify(value, null, 2)}\n`, "utf8");
  };
  const anchors = join(directory, "anchor-descriptors.json");
  await write(slicesPath(directory), drawn.slices);
  await write(anchors, drawn.anchors);
  const record: DrawRecord = {
    corpus: corpus.directory,
    cut: new Date(cut).toISOString(),
    seed,
    targets: drawn.targets,
    counts: drawn.counts,
    groupKey: drawn.groupKey,
    groupCount: drawn.groupCount,
    groups: drawn.groups,
    postCutCases: post.length,
    documentsUnread: rendered.unread,
    // The one input of the draw that is not the corpus: the labelled roots on the backlog or in
    // icebox, which the never-decide rule reads. The list is written whole, with the time it
    // was read, so `--backlog-roots <this file>` draws these slices again whatever Dispatch
    // shows later.
    backlogLegionRootsRead: roots.issues.length,
    backlogLegionRoots: roots,
    // What the rule was, for this draw: the prose it applied and what applying it cost. A
    // change of one byte of that prose changes every cache key, so a later draw over the same
    // corpus and seed with another definition is a different reading and says so here.
    rule: {
      model: DEFAULT_NEVER_DECIDE_MODEL,
      definitionSha256: createHash("sha256").update(definition, "utf8").digest("hex"),
      cache: flags.cache ?? null,
      asked,
      cached,
      inputTokens: holds.reduce((total, one) => total + (one.inputTokens ?? 0), 0),
      outputTokens: holds.reduce((total, one) => total + (one.outputTokens ?? 0), 0),
    },
    excluded: {
      held: { total: drawn.held.length, byKind: drawn.heldByKind, cases: drawn.held },
      undrawable: {
        total: drawn.undrawable.length,
        byKind: drawn.undrawableByKind,
        cases: drawn.undrawable,
      },
    },
  };
  await write(drawPath(directory), record);
  console.log("");
  console.log(`written           ${slicesPath(directory)}`);
  console.log(`                  ${anchors}`);
  console.log(`                  ${drawPath(directory)}`);
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
    if (error instanceof CannotRunError) {
      console.error(`${error.name}: ${error.message}`);
      return 2;
    }
    throw error;
  }
}

// Only when run: a test that imports `drawSlices` must not run a command.
if (import.meta.main) process.exit(await main(Bun.argv.slice(2)));
