import type { CaseRow } from "./build-corpus";
import { CannotRunError, leadDocumentOf, recordOf, stringOf } from "./corpus-io";
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
 *    case's own answer, or an `Approval: approved v<n>` with `n` at or above the case's own lead
 *    `requestedVersion`.
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
    const version =
      document.requestedVersion === null ? "" : ` (version ${document.requestedVersion})`;
    const heading = `## Document ${index + 1} of ${documents.length}: ${document.name ?? "unnamed"}`;
    lines.push("", `${heading}${version}`, "");
    if (document.markdown === null) {
      lines.push(`(this document could not be read: ${document.refusal ?? "no markdown"})`);
      return;
    }
    lines.push(renderDocument(document.markdown, row.answeredAt));
  });
  return `${lines.join("\n")}\n`;
}

/**
 * Holds a rendered case to the two rules that can be read off the text itself, and throws
 * `LeakError` naming the case and the offending text when one is broken. The round refuses to
 * start on a case that fails this. The version rule reads `leadDocumentOf`, the document the
 * draw grouped the case by, so the version guarded is the version asked about.
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
  const lead = leadDocumentOf(row)?.requestedVersion ?? null;
  if (lead === null) return;
  for (const match of text.matchAll(APPROVED_VERSION)) {
    const version = Number(match[1]);
    if (Number.isFinite(version) && version >= lead) {
      throw new LeakError(
        row.caseId,
        match[0],
        `an approval of version ${version}, at or above the requested version ${lead}`
      );
    }
  }
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
