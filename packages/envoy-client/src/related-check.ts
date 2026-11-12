import {
  relatedCheckFormOf,
  relatedCheckSearchWords,
  type SearchAsk,
  type Suggestions,
  type WriteAdvice,
  type WriteSuggestion,
} from "@legion/contracts";
import { messageFor } from "./errors";
import { askStateLines } from "./search-answer";

/** What a not-made line needs to name the search to run by hand, and what an item's link is made
 *  absolute against. */
export interface RelatedCheckContext {
  /** The project the check searched, for `dispatch search --project`. */
  readonly project: string;
  /** The text the check read (a title, a question), the source of the search words when the
   *  server sent no `search_query`; empty when there is none. */
  readonly text: string;
  /** The Dispatch URL item links are made absolute against. */
  readonly configUrl: string;
}

/**
 * The related-item check's lines. They hold exactly one of three forms, so a check
 * that never ran cannot read as one that found nothing: the items it found and how it searched,
 * that it searched and found nothing, or that no check was made, why, and the `dispatch search` to
 * run instead. The likely duplicate an unforced create was nudged about, and the near copies a
 * forced create went past, follow the form.
 *
 * A check whose write is a question also carries `other_projects`: answered question asks of
 * projects other than the one that asks. They print under a heading of their own, after every
 * item of the asking project, each row naming the project it is in. The heading says where the
 * items come from and says nothing about whether they answer the question: the server ranks them
 * by meaning alone, and the reader decides. A server that sends none prints no heading.
 *
 * A server predating the forms sends less, and each of its shapes maps to one form: no result at
 * all, `missing`, items with no `outcome`, and nothing at all with no `outcome`. A server
 * predating `other_projects` sends none, and one that still sends a field this client no longer
 * reads has it ignored.
 *
 * It never throws: it rides on a write that already succeeded, so a result shaped as this client
 * does not expect still prints a form rather than costing the reader the write's own line. Each
 * block after the form holds its own failure the same way.
 */
export function relatedCheckLines(
  suggestions: Suggestions | null | undefined,
  context: RelatedCheckContext
): string[] {
  if (suggestions === null || suggestions === undefined) {
    return [notMadeLine("the server sent no check result", context)];
  }
  let lines: string[];
  try {
    lines = relatedCheckForm(suggestions, context);
  } catch (error) {
    lines = [
      notMadeLine(`this client could not read the check result (${messageFor(error)})`, context),
    ];
  }
  try {
    lines.push(...otherProjectLines(suggestions, context.configUrl));
  } catch (error) {
    lines.push(`The answers of other projects could not be shown: ${messageFor(error)}`);
  }
  try {
    const likely = suggestions.likely_duplicate;
    if (likely !== undefined && likely !== null) {
      lines.push(
        `Likely duplicate: ${likely.key} [${likely.status}] ${likely.title} → ${absoluteHref(likely.href, context.configUrl)}`
      );
      lines.push(
        "Read it. If it is the same work, fold this issue into it by the duplicate rule of skill://dispatch-first."
      );
    }
    if (suggestions.near_copy_check !== undefined && suggestions.near_copy_check !== "") {
      lines.push(`No near-copy check was made: ${suggestions.near_copy_check}.`);
    }
    for (const match of suggestions.title_matches ?? []) {
      lines.push(
        `Near copy you forced past: ${match.key} [${match.status}] ${match.title} → ${absoluteHref(match.href, context.configUrl)}`
      );
    }
  } catch (error) {
    lines.push(`The check's near copies could not be shown: ${messageFor(error)}`);
  }
  return lines;
}

/**
 * A document edit's lines about the decision blocks it added: `Decision block <id>:`
 * and that block's check form, in document order, each searched by its own question. A server
 * predating the checks only counts the new blocks, so the edit gets one not-made line for all of
 * them, searched by `context.text`. An edit that added none, or a server predating even the
 * count, prints nothing. Never throws, for the reason `relatedCheckLines` gives.
 */
export function decisionBlockLines(
  advice: WriteAdvice | undefined,
  context: RelatedCheckContext
): string[] {
  try {
    const checks = advice?.decision_block_checks;
    if (checks === undefined || checks === null) {
      const added = advice?.decision_blocks_added ?? 0;
      if (added === 0) return [];
      return [
        `${added} new decision ${added === 1 ? "block" : "blocks"}:`,
        notMadeLine("the server sent no check result", context),
      ];
    }
    return checks.flatMap((check) => [
      `Decision block ${check.block_id}:`,
      ...relatedCheckLines(check.suggestions, { ...context, text: check.question ?? "" }),
    ]);
  } catch (error) {
    return [
      notMadeLine(
        `this client could not read the decision blocks' checks (${messageFor(error)})`,
        context
      ),
    ];
  }
}

/**
 * What a write that opens a question is told about the asks of its issue a person declined to
 * answer (`declined_asks`): a line saying how many there are, and for each, oldest first, its
 * question, that it was declined, the person's note where they wrote one, and a link to the ask
 * — its `/issues/<key>/asks/<id>` page, which is the ask's own `dispatch://KEY/ask/<id>` address
 * (`dispatch-href.ts`), made absolute as every other link here is. The issue is an argument
 * rather than part of the check's context because the server sends `declined_asks` only for a
 * write that stands on an issue (`api/asks.go` and `api/artifacts.go` guard the advice on the
 * owner having an issue key), so every caller that has asks to print knows the key.
 *
 * The question and the note are printed as the server sent them, cut mark and all: the server
 * cuts both to the length this line shows (`declinedAskHeadUnits` in
 * `packages/envoy/internal/dispatch/api/write_advice.go`, which is this package's
 * `QUESTION_HEAD`), so a note running to 32,000 units costs the write nothing and the whole of
 * it stands at the ask's link. A second cut here would only take a reader's text away twice.
 *
 * No line says the person answered, because they did not: a declined ask holds no answer, and
 * the reminder is there so the same question is not put to them again unless the facts changed.
 *
 * It prints nothing when the server sent no `declined_asks`: a server predating the field, a
 * read of it that failed, and an issue holding no declined ask all read the same, and none of
 * them is something to tell the writer about. Never throws, for the reason `relatedCheckLines`
 * gives in its own doc comment.
 */
export function declinedAskLines(
  advice: WriteAdvice | undefined,
  issueKey: string,
  configUrl: string
): string[] {
  try {
    const declined = advice?.declined_asks;
    if (declined === undefined || declined === null || declined.length === 0) return [];
    const count = declined.length;
    const lines = [
      count === 1
        ? "This issue holds a question a person declined to answer. Ask it again only if the facts have changed:"
        : `This issue holds ${count} questions a person declined to answer. Ask one again only if the facts have changed:`,
    ];
    for (const ask of declined) {
      const href = absoluteHref(
        `/issues/${encodeURIComponent(issueKey)}/asks/${encodeURIComponent(ask.id)}`,
        configUrl
      );
      lines.push(`- Declined: ${ask.question} → ${href}`);
      if (ask.note !== "") lines.push(`  Their note: ${ask.note}`);
    }
    return lines;
  } catch (error) {
    return [`The issue's declined asks could not be shown: ${messageFor(error)}`];
  }
}

/**
 * One of the three forms as the CLI words it. Which form the result holds is the contract's
 * judgement (`relatedCheckFormOf`), never this module's: the dashboard reads the same answer.
 */
function relatedCheckForm(suggestions: Suggestions, context: RelatedCheckContext): string[] {
  const form = relatedCheckFormOf(suggestions);
  if (form.form === "not_made") {
    return [notMadeLine(form.reason, context, form.searchQuery)];
  }
  if (form.form === "items") {
    // A server predating the forms names no search mode, so its header names none either.
    const header =
      form.mode === undefined ? "Related items:" : `Related items (searched by ${form.mode}):`;
    return [header, ...form.related.flatMap((item) => relatedItemLines(item, context.configUrl))];
  }
  return [
    form.mode === undefined
      ? "Related-item check searched and found nothing."
      : `Related-item check searched (${form.mode}) and found nothing.`,
  ];
}

/**
 * The answered questions of other projects, under a heading of their own, after the items of the
 * asking project. A server that offered none — the field absent, null, or an empty list — prints
 * nothing, the heading with it, so a check that offered nothing reads as one.
 */
function otherProjectLines(suggestions: Suggestions, configUrl: string): string[] {
  const items = suggestions.other_projects ?? [];
  if (items.length === 0) return [];
  return [
    // The heading names where the items come from. It claims nothing about whether they answer
    // the question the check read: the server ranks them by meaning, and the reader decides.
    "Answered questions in other projects:",
    ...items.flatMap((item) => relatedItemLines(item, configUrl, true)),
  ];
}

/**
 * One item's lines: the line that names it and links it, and, for an ask, where the ask stands.
 * `namesProject` puts the item's project first, which an item of another project needs and an
 * item of the asking project does not. An item whose owner names no project prints without one.
 */
function relatedItemLines(
  item: WriteSuggestion,
  configUrl: string,
  namesProject = false
): string[] {
  try {
    const project = item.owner.project;
    const label =
      namesProject && project !== undefined && project !== ""
        ? `${project}: ${suggestionLabel(item)}`
        : suggestionLabel(item);
    const lines = [`- ${label} → ${absoluteHref(item.href, configUrl)}`];
    if (item.kind === "ask") {
      lines.push(...askLines(item.ask));
    }
    return lines;
  } catch (error) {
    // One item the client cannot read costs only that item, never the form.
    const href = (item as { readonly href?: unknown } | null)?.href;
    return [`- ${String(href)} (this client could not show it: ${messageFor(error)})`];
  }
}

/**
 * Where an ask stands, under the line that names it, as under a search hit (`askStateLines`), so
 * an ask with no state says the server did not report one. Never throws: a state this client
 * cannot read costs only these lines, never the line above them.
 */
function askLines(ask: SearchAsk | null | undefined): string[] {
  try {
    const lines = askStateLines(ask ?? undefined);
    if (!Array.isArray(lines)) throw new Error(`unknown ask state ${String(ask?.state)}`);
    return lines;
  } catch (error) {
    return [`  This client could not show where the ask stands: ${messageFor(error)}`];
  }
}

function suggestionLabel(item: WriteSuggestion): string {
  if (item.owner.kind === "issue") {
    return `${item.owner.key} [${item.owner.status}] ${item.owner.title}`;
  }
  return `${item.owner.name} (${item.owner.project}/${item.owner.slug})`;
}

/** The link as an absolute URL, or as the server sent it when it does not parse as one. */
function absoluteHref(href: string, configUrl: string): string {
  try {
    return new URL(href, configUrl).toString();
  } catch {
    return String(href);
  }
}

/**
 * `No related-item check was made: <reason>.` and the search to run by hand: `searchQuery` when
 * the server chose the words, else the first three words of four or more letters in the text the
 * check read. With no such word the command names what to put in `--query` instead of guessing.
 */
function notMadeLine(reason: string, context: RelatedCheckContext, searchQuery?: string): string {
  const words = searchQuery?.trim() || relatedCheckSearchWords(context.text);
  const query =
    words === ""
      ? " with two or three of its words as --query"
      : ` --query '${words.replaceAll("'", "'\\''")}'`;
  return `No related-item check was made: ${reason}. Search before you rely on this being new: dispatch search --project ${context.project}${query}`;
}
