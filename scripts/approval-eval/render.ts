import type { ApprovalDocument, CaseRow } from "./build-corpus";
import { CannotRunError, recordOf, stringOf } from "./corpus-io";
import { type DispatchConfig, DispatchRefusedError, readJson } from "./dispatch-read";
import type { ReplayedDocument } from "./replay";

/**
 * One case as the judge is shown it, with the person's own answer kept out of it.
 *
 * The rendered evidence carries the issue key, its title and its project, each document of the
 * approval request at the version the request named, and the question with its options. It
 * carries no verdict, none of the person's words and no `selected` of this case's own answer:
 * what the judge is asked for is the answer, so none of it may be in the question.
 *
 * Four rules keep the answer out, from the pre-analysis plan's section "What the judge is shown,
 * and what is kept out of it" (dispatch://INFRA-2897/artifact/plan-of-analysis-md@v2):
 *
 * 1. Each document is the markdown at its own `requestedVersion`, which `replayCase` reads, and
 *    never the document as it stands now.
 * 2. Dispatch's own trailer lines are stripped: the run of lines at the end of the markdown
 *    beginning `Approval:` or `Document token:`. Measured against this server: of the 855
 *    document versions the post-cut approval cases name, 29 such lines sit in that trailing run
 *    and 10 sit inside a document's body, quoted in its own text, where they are not Dispatch's
 *    trailer and are left alone.
 * 3. An ask block inside a document keeps its answer only when that answer is dated strictly
 *    earlier than this case's own `answeredAt`. Otherwise its `answered_by`, `answered_at`,
 *    `selected` and `answer` attributes go and its `state` is written `open`.
 * 4. `assertNoLeak` refuses a rendered case that still carries an `answered_at` at or after the
 *    case's own answer, or an `Approval: approved v<n>` with `n` at or above the version the
 *    stretch of the rendering it stands in was asked about. Versions count separately for each
 *    artifact, so each document's section of the rendering is held to that document's own
 *    `requestedVersion` and the head of it, which belongs to no document, to the lowest of them.
 *
 * The ask fence's syntax is measured, not guessed: document 68c5417b-68ac-4686-a530-a39188245b36
 * at version 45 carries, on one line,
 * `:::ask{#86f6c8d5-15a1-462e-ac82-24205dcdbbd3 urgency="med" multiple="false" state="answered"`
 * ` answered_by="<the login of whoever answered>" answered_at="2026-11-04T18:40:07.694801Z"`
 * ` selected="[&#x22;Leave them out&#x22;]"}`, which is the block id as a bare `#` token and then
 * `name="value"` pairs whose values are HTML-escaped, so no value holds a raw quotation mark.
 * `GET /api/v1/schema/blocks` declares those four attributes, with `answer`, as server-written.
 */

/** A rendered case still carries the answer the judge is being asked for. */
export class LeakError extends CannotRunError {
  override readonly name = "LeakError";
  /** The case whose rendering broke a rule. */
  readonly caseId: string;
  /** The text that broke it, as it stands in the rendering. */
  readonly offending: string;

  constructor(caseId: string, offending: string, why: string) {
    super(`case ${caseId}: ${why}: ${offending}`);
    this.caseId = caseId;
    this.offending = offending;
  }
}

/** What the rendering is told beside the case itself. */
export interface RenderOptions {
  /** The issue's title, which `issueTitleOf` reads. Absent, the rendering says it is unknown. */
  issueTitle?: string | null;
}

/**
 * The attributes of an ask block Dispatch writes when it is answered, and never a person. One
 * pattern over the four of them, compiled once: a fence is rewritten for every document of
 * every judgement, and a pattern built inside that call is built again for each one.
 */
const ANSWER_ATTRIBUTES = /\s*(?:answered_by|answered_at|selected|answer)="[^"]*"/g;

/** The `answered_at` attribute of one fence, read on its own. */
const ANSWERED_AT_ATTRIBUTE = /answered_at="([^"]*)"/;

/** The `state` attribute of one fence, which a stripped answer is reopened by. */
const STATE_ATTRIBUTE = /\sstate="[^"]*"/;

/** A line opening an ask block: `:::ask{` its attributes `}`. */
const ASK_FENCE = /^:::ask\{(.*)\}[ \t]*$/;

/** What Dispatch prints under a document, which says how that document's approval went. */
const TRAILER = /^(Approval:|Document token:)/;

/** An `answered_at` attribute, as a fence writes it. */
const ANSWERED_AT = /answered_at="([^"]*)"/g;

/** The approval of a version, as Dispatch's own trailer names it. */
const APPROVED_VERSION = /Approval: approved v(\d+)/g;

/**
 * What every heading of a document opens with. `headingOf` writes it and `guardedSectionsOf`
 * reads it, so a line that is one of these headings and a line that is not are told apart in
 * one place.
 */
const DOCUMENT_HEADING = "## Document ";

/** The time of an ISO 8601 stamp, or null when it is no time. */
function timeOf(at: string | null | undefined): number | null {
  if (at === null || at === undefined) return null;
  const time = Date.parse(at);
  return Number.isNaN(time) ? null : time;
}

/**
 * One ask block's fence, with its answer kept only when that answer is dated strictly earlier
 * than `answeredBefore`. A block whose answer carries no readable date is stripped too: a date
 * that cannot be read cannot be shown to be earlier.
 */
function fenceWithoutLaterAnswer(attributes: string, answeredBefore: number): string {
  const answeredAt = timeOf(ANSWERED_AT_ATTRIBUTE.exec(attributes)?.[1]);
  if (answeredAt !== null && answeredAt < answeredBefore) return attributes;
  let hadState = false;
  let kept = attributes.replace(ANSWER_ATTRIBUTES, "").replace(STATE_ATTRIBUTE, () => {
    hadState = true;
    return ' state="open"';
  });
  if (!hadState) kept = `${kept.trimEnd()} state="open"`;
  return kept;
}

/**
 * One document as the judge is shown it: the markdown at the version the request named, with
 * Dispatch's trailer lines stripped and every ask block's later answer taken out of it.
 */
export function renderDocument(markdown: string, answeredAt: string): string {
  const answeredBefore = timeOf(answeredAt) ?? Number.NEGATIVE_INFINITY;
  const lines = markdown.split("\n");
  while (lines.length > 0) {
    const last = lines[lines.length - 1] ?? "";
    if (last.trim() !== "" && !TRAILER.test(last)) break;
    lines.pop();
  }
  return lines
    .map((line) => {
      const fence = ASK_FENCE.exec(line);
      if (fence === null) return line;
      return `:::ask{${fenceWithoutLaterAnswer(fence[1] ?? "", answeredBefore)}}`;
    })
    .join("\n");
}

/**
 * The head of one document's heading: where its section stands in a rendering and how many
 * sections stand there, which is the rendering's own and not the document's.
 */
function headingHead(index: number, of: number): string {
  return `${DOCUMENT_HEADING}${index + 1} of ${of}: `;
}

/**
 * The tail of one document's heading: its name and the version it was asked about, which the
 * document carries wherever in a rendering its section stands.
 */
function headingTail(document: { name: string | null; requestedVersion: number | null }): string {
  const version =
    document.requestedVersion === null ? "" : ` (version ${document.requestedVersion})`;
  return `${document.name ?? "unnamed"}${version}`;
}

/**
 * The heading one document's section of a rendering is written under. `renderCase` writes it
 * and `guardedSectionsOf` cuts the rendering at it, so the two read the same line, and
 * `openedBy` matches a standing line against the same two pieces this builds it from.
 */
function headingOf(
  document: { name: string | null; requestedVersion: number | null },
  index: number,
  of: number
): string {
  return `${headingHead(index, of)}${headingTail(document)}`;
}

/**
 * The evidence text of one case: what the judge reads, and nothing the person answered. The
 * documents are what `replayCase` read for this case, each rendered by `renderDocument`.
 */
export function renderCase(
  row: CaseRow,
  documents: readonly ReplayedDocument[],
  options: RenderOptions = {}
): string {
  const title = options.issueTitle?.trim();
  const lines: string[] = [
    row.kind === "approval" ? "# An approval request" : "# A question",
    "",
    `Issue: ${row.issueKey ?? "none"}${title ? ` — ${title}` : ""}`,
    `Project: ${row.project ?? "none"}`,
    `Urgency: ${row.urgency ?? "none"}`,
    `More than one option may be chosen: ${row.multiple ? "yes" : "no"}`,
    "",
    "## The question",
    "",
    row.question?.trim() === "" || row.question === null
      ? "(the ask carries no text)"
      : row.question,
    "",
    "## The options",
    "",
  ];
  if (row.options.length === 0) {
    lines.push("(the ask offered no option)");
  } else {
    for (const option of row.options) {
      const description = option.description?.trim();
      lines.push(`- ${option.label ?? "unnamed"}${description ? `: ${description}` : ""}`);
    }
  }
  documents.forEach((document, index) => {
    lines.push("", headingOf(document, index, documents.length), "");
    if (document.markdown === null) {
      lines.push(`(this document could not be read: ${document.refusal ?? "no markdown"})`);
      return;
    }
    lines.push(renderDocument(document.markdown, row.answeredAt));
  });
  return `${lines.join("\n")}\n`;
}

/**
 * The lowest version any document of the request asked about, and null where none names one.
 * It is what the stretches of a rendering that belong to no one document are held to.
 */
function lowestRequestedVersion(documents: readonly ApprovalDocument[]): number | null {
  let lowest: number | null = null;
  for (const document of documents) {
    const version = document.requestedVersion;
    if (version === null) continue;
    if (lowest === null || version < lowest) lowest = version;
  }
  return lowest;
}

/** One stretch of a rendering, with the version an approval named in it may not be at or above. */
interface GuardedSection {
  /** The version it is held to, and null where it is held to none. */
  guarded: number | null;
  /**
   * The document whose own requested version holds it, and null where that version is the
   * lowest the request asked about: the head of the rendering, and the section of a document
   * the request names no version for.
   */
  document: string | null;
  text: string;
}

/**
 * The document each `## Document ` line standing in a rendering opens the section of, in the
 * order those lines stand, and null where the lines are not the row's own headings, each once.
 *
 * A document is matched by the heading it would be written under at that place in the
 * rendering, which is `headingHead` over the position the line stands at and `headingTail`
 * over the row's own document: the index a heading carries is of the rendering and the rest of
 * it - the document's name and the version it was asked about - is the document's own. So the
 * row's documents may be rendered in any order, which is what `run-round.ts` does per
 * judgement, and each section is still held to the version of the document whose heading opens
 * it.
 *
 * Null, which drops the whole rendering to the lowest version, where a standing line is no
 * heading of the row's, where one of the row's headings stands twice or not at all (the counts
 * then differ, or a line finds no document left to take), and where one line could open the
 * section of two documents the request holds to different versions: which section is which is
 * then not in the text, and a guess is not a guard.
 */
function openedBy(
  documents: readonly ApprovalDocument[],
  standing: readonly string[]
): ApprovalDocument[] | null {
  if (standing.length !== documents.length) return null;
  // The tails are built once for the whole rendering and the head once per line, and a line is
  // read against the two rather than written out as a heading for each document it could open
  // the section of: this runs over every heading line of every rendering the round guards,
  // which is one per judgement and one per case, 27,540 judgements in the recorded round.
  const tails = documents.map(headingTail);
  const opened: ApprovalDocument[] = [];
  const taken = new Set<number>();
  for (const [at, line] of standing.entries()) {
    const head = headingHead(at, documents.length);
    // No heading of the row's can stand here, whatever the document, so the line is none.
    if (!line.startsWith(head)) return null;
    let first: ApprovalDocument | null = null;
    let took = -1;
    for (let which = 0; which < documents.length; which += 1) {
      const tail = tails[which];
      if (taken.has(which) || line.length !== head.length + tail.length) continue;
      if (!line.startsWith(tail, head.length)) continue;
      const document = documents[which];
      if (first === null) {
        first = document;
        took = which;
        continue;
      }
      // One line opening the section of two documents held to different versions: which
      // section is which is not in the text, and a guess is not a guard.
      if (document.requestedVersion !== first.requestedVersion) return null;
    }
    if (first === null) return null;
    taken.add(took);
    opened.push(first);
  }
  return opened;
}

/**
 * The rendering cut into the stretches the version rule is held over, each with the version it
 * is held to.
 *
 * Versions count separately for each artifact: an approval of v13 of this document is the
 * answer to a request asking about v13 of it and says nothing about another document asked
 * about at v7. So each document's section of the rendering is held to that document's own
 * `requestedVersion`, and the head - the issue, the question and the options, which belong to
 * no document - is held to the lowest version the request asked about, since nothing says which
 * document a version named up there is a version of.
 *
 * The cut is made over the rendering rather than over each document's markdown before it is
 * rendered, because the rendering is what a prompt carries: a rule read off each document
 * alone would leave the head of the rendering, and every stretch belonging to no document,
 * unguarded. Each prompt hands the guard the bytes it carries - `fresh-agent.ts`'s `promptOf`
 * and `run-round.ts`'s - and `run-round.ts` guards one rendering more, once per case before
 * any arm is asked and carried by no prompt, which refuses a case that cannot be rendered
 * cleanly before it is paid for.
 *
 * The cut is made at the heading `renderCase` writes for each document, which both are built by
 * `headingOf` so neither can drift from the other. A document is found by the heading that
 * opens its section and not by where that section stands, because the order of the documents
 * is the judgement's own: `run-round.ts` renders them in each judgement's seed order, so the
 * row's second document opens the first section as readily as the second. `openedBy` reads
 * which document each heading opens.
 *
 * Where the headings standing in the rendering are not the row's own, each exactly once, the
 * whole rendering is one stretch held to the lowest version: the old whole-text rule, and
 * never a weaker one. Four readings reach that: a rendering of other documents than the row
 * names - `fresh-agent.ts` takes the row from the corpus and the documents from a round's
 * cache, which checks the case id and the cache version and not the names - a line of a
 * document's own body that reads like one of these headings, a heading of the row's standing
 * twice or not at all, and two documents of one request writing the same heading while being
 * held to different versions, where which section is which cannot be read off the text at all.
 * Holding a stretch to one document's version alone is the weaker rule, and it is applied only
 * where the text says which document each stretch is, never where that has to be guessed at.
 *
 * A document the request names no `requestedVersion` for is held to that lowest version too.
 * Its own section names no version to be held to, and holding it to nothing would be weaker than
 * the rule it replaces. `requestedVersion` is read off every document, naming an artifact or
 * not, because `ApprovalDocument` carries the two fields apart and the corpus holds requests
 * whose version-bearing document names no artifact; reading the version off the lead document
 * alone would leave such a case with no version rule at all.
 */
function guardedSectionsOf(row: CaseRow, text: string): GuardedSection[] {
  const documents = row.approval?.documents ?? [];
  const lowest = lowestRequestedVersion(documents);
  const lines = text.split("\n");
  const opened = openedBy(
    documents,
    lines.filter((line) => line.startsWith(DOCUMENT_HEADING))
  );
  if (opened === null) return [{ guarded: lowest, document: null, text }];
  const sections: GuardedSection[] = [];
  let guarded = lowest;
  let named: string | null = null;
  let held: string[] = [];
  let next = 0;
  for (const line of lines) {
    const document = opened[next];
    if (document !== undefined && line.startsWith(DOCUMENT_HEADING)) {
      sections.push({ guarded, document: named, text: held.join("\n") });
      guarded = document.requestedVersion ?? lowest;
      named = document.requestedVersion === null ? null : (document.name ?? "unnamed");
      held = [];
      next += 1;
    }
    held.push(line);
  }
  sections.push({ guarded, document: named, text: held.join("\n") });
  return sections;
}

/**
 * Holds a rendered case to the two rules that can be read off the text itself, and throws
 * `LeakError` naming the case and the offending text when one is broken. The round refuses to
 * start on a case that fails this. The version rule runs over the stretches `guardedSectionsOf`
 * cuts the rendering into, each held to the version that stretch was asked about, so no version
 * the request put to the person may be named as approved and an approval a document quotes of
 * an earlier version of itself is left alone.
 */
export function assertNoLeak(row: CaseRow, text: string): void {
  const answeredAt = timeOf(row.answeredAt);
  if (answeredAt !== null) {
    for (const match of text.matchAll(ANSWERED_AT)) {
      const at = timeOf(match[1]);
      if (at !== null && at >= answeredAt) {
        throw new LeakError(
          row.caseId,
          match[0],
          `an answer dated at or after the case's own answer of ${row.answeredAt}`
        );
      }
    }
  }
  for (const section of guardedSectionsOf(row, text)) {
    const guarded = section.guarded;
    if (guarded === null) continue;
    for (const match of section.text.matchAll(APPROVED_VERSION)) {
      const version = Number(match[1]);
      if (!Number.isFinite(version) || version < guarded) continue;
      const held =
        section.document === null
          ? `the lowest version the request asked about, ${guarded}`
          : `the version ${guarded} of ${section.document} the request asked about`;
      throw new LeakError(
        row.caseId,
        match[0],
        `an approval of version ${version}, at or above ${held}`
      );
    }
  }
}

/** Whether a caller wants its rendering held to `assertNoLeak` before it is used. */
export type RenderGuard = "assert-no-leak" | "none";

/** A rendered case and what was done to that text. */
export interface CheckedRendering {
  /** The evidence, as `renderCase` wrote it. */
  text: string;
  /**
   * What was done to it, as a sentence a record can carry. It is written by the branch that
   * did what it says, so it moves when this function does.
   */
  check: string;
}

/** What is said of a text `assertNoLeak` returned over, which is both of that guard's rules. */
const HELD_TO_GUARD =
  "The text put to the rule was held to `assertNoLeak`: no answer dated at or after the " +
  "case's own answer stands in it, and no approval names a version at or above the one the " +
  "stretch of the rendering it stands in was asked about.";

/**
 * What is said of a text no guard was run over, with the reason for each of the two rules
 * `assertNoLeak` holds a text to.
 */
const NOT_HELD_TO_GUARD =
  "The text put to the rule was not held to `assertNoLeak`, as the text put to a judge of the " +
  "person's own decision is. Neither of that guard's two rules bears on what this rule is " +
  "asked, which is what a document requests - a destructive act, a credential, an owner-only " +
  "setting, an outside message or account, or a change of a skill. An approval quoting a " +
  "version answers none of that, so an approval in the text cannot decide this rule's answer; " +
  "and an answer dated at or after the case's own answer carries what the person decided of " +
  "the ask, which this rule is not asked and reads nothing off.";

/**
 * One case rendered, held to `assertNoLeak` where the caller asks for that, and said of: the
 * text and a sentence stating what was done to it.
 *
 * The two sentences are written here, each in the branch that does the thing it states, and a
 * caller records what it is handed rather than a statement of its own. The guarded branch
 * reaches its sentence only where `assertNoLeak` returned, and that guard throws over a text
 * breaking either of its rules, so a record saying a text was held to the guard cannot be had
 * for a text no guard ran over.
 */
export function renderChecked(
  row: CaseRow,
  documents: readonly ReplayedDocument[],
  guard: RenderGuard,
  options: RenderOptions = {}
): CheckedRendering {
  const text = renderCase(row, documents, options);
  if (guard === "none") return { text, check: NOT_HELD_TO_GUARD };
  assertNoLeak(row, text);
  return { text, check: HELD_TO_GUARD };
}

/** The title of one issue, which the rendering heads a case with. Null when it cannot be read. */
export async function issueTitleOf(
  issueKey: string,
  config?: DispatchConfig
): Promise<string | null> {
  const path = `/api/v1/issues/${encodeURIComponent(issueKey)}`;
  try {
    return stringOf(recordOf(await readJson<unknown>(path, config))?.title);
  } catch (error) {
    if (error instanceof DispatchRefusedError) return null;
    throw error;
  }
}
