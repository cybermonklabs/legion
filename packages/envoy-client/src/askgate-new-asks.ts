// The asks a gated Dispatch call opens for a person, read from the call's arguments as its
// --dry-run prints them: a `dispatch ask`; each ask block in the markdown a `dispatch doc-edit`
// inserts or writes over a match, a `dispatch issue` spec holds, or a `dispatch artifact` upload
// holds; and each block a `dispatch doc-edit` retypes into an ask. `dispatch edit-ask`, `comment`,
// `message`, `request-approval` and the four corrections, `edit-comment`, `delete-comment`,
// `edit-message` and `delete-message`, open no new ask.
//
// A `replace`'s `with` is read with no front matter read in it: `newAsks` cannot know whether the
// match sits at the document's start, which is what the server's front-matter reading needs, and
// reading front matter where there is none would hide an ask block the server opens. The gate
// fails closed, as `refuseRemovingOpenDecisionBlocks` does: it over-reports where the server
// would refuse the operation (a match in a heading, in a table cell, or in a typed block whose
// rule cannot hold the blocks) or write the text as literal code (a match in a code block).
//
// A `with` is read as every string the server might parse, never as the caller sent it alone.
// The server does not parse that string: `replacementMarkdown` cuts the matched block's own
// marker off the first line, `applyReplace` cuts a leading `[x] ` or `[ ] ` off what that leaves
// where the match opens a task item's text, and `pmdoc.ParseFragment` parses the rest
// (packages/envoy/internal/dispatch/docs/edits.go). The gate reads the call's arguments before
// the call goes out (packages/envoy-client/src/dispatch-cli.ts, and
// packages/pi-envoy/extensions/askgate/breaker.ts:66), so the server's own answer does not exist
// yet and cannot be read. The client therefore scans every string the server might parse and
// takes the union: the `with` as sent, and the `with` with a leading block marker or task
// checkbox taken off its first line. The gate already fails closed and over-reports deliberately;
// a reading that is not the server's can only add an ask to review, or drop an ask holding no
// text where an ask the union holds carries words, so the words a person will be asked are never
// hidden. `LEADING_STRIP` copies the server's own patterns (pmdoc's leadingHeadingMarker,
// leadingOrderedMarker and leadingBulletMarker in
// packages/envoy/internal/dispatch/pmdoc/markers.go:46-48, and leadingTaskCheckbox in
// packages/envoy/internal/dispatch/pmdoc/tasks.go:103), so it is a server rule to keep in step:
// a pattern this copy holds and the server does not costs one more candidate reading, while a
// marker the server learns to strip and this copy does not hides the string the server parses.
// Reading those two sites whenever the strip changes is what holds the copy in step. The server
// strips one marker (`replacementMarkdown`) and then one checkbox over a task item's text
// (`applyReplace`, packages/envoy/internal/dispatch/docs/edits.go); each later strip of the loop
// here adds one more candidate reading rather than a server rule. When the server strips is not
// read here: that needs the document, which the client does not have, so the strip is lexical
// and every reading is scanned.
//
// The Dispatch server decides which lines of markdown are ask blocks, so the scan reads markdown as
// its parser does, through `readAskBlocks` in @legion/contracts (dispatch-block-reader.ts): the one
// reader of a document's blocks in TypeScript, which follows goldmark's block structure
// (github.com/yuin/goldmark v1.8.6), pmdoc's own block parsers
// (packages/envoy/internal/dispatch/pmdoc) and Dispatch's typed blocks (pmdoc's directive.go). Its
// doc comment lists the rules it reads blocks by. The scan gives it the server's reader of an
// opener's attributes (typedOpenerAt in typed-block-opener.ts), so a `#` inside a quoted value is
// no id, and an opener whose attributes the server refuses opens nothing.
// The server's table of inserts and the ask blocks it adds
// (packages/envoy/internal/dispatch/api/testdata/decision-block-inserts.json) holds this reading
// to the server's.

import { readAskBlocks } from "@legion/contracts";
import { typedOpenerAt } from "./typed-block-opener";

/** A new ask a call opens for a person. */
export interface NewAsk {
  /** A `dispatch ask`, an ask block in markdown the call writes, or a block it retypes into one. */
  kind: "ask" | "block" | "retype";
  /**
   * What the person is asked: a `dispatch ask`'s question, then its options as a bullet list; an
   * ask block's body as written; empty for a retype, whose block's text is in the document.
   */
  text: string;
  /** An ask block's own `#id`, or the block a retype names. */
  id?: string;
  /** The text's first words, which name the ask in a refusal. */
  opening: string;
}

/** How many code points of an ask's text name it: as many as `dispatch doc-read --blocks` shows. */
const OPENING_CODE_POINTS = 60;

/** The new asks in a call's arguments. */
export function newAsks(toolName: string, input: Readonly<Record<string, unknown>>): NewAsk[] {
  switch (toolName) {
    case "dispatch_ask":
      return [questionAsk(input)];
    case "dispatch_issue":
      return typeof input.spec === "string" ? askBlocks(input.spec) : [];
    case "dispatch_artifact":
      return typeof input.content === "string" ? askBlocks(input.content) : [];
    case "dispatch_doc_edit":
      return editOps(input).flatMap((op): NewAsk[] => {
        const { op: kind, markdown, with: replacement, type, block } = op;
        // Inserted text reads a closed front-matter block as front matter only where it lands at
        // the document's start (pmdoc.ParseFragment's opensDocument); a "start" anchor is that.
        if (kind === "insert" && typeof markdown === "string") {
          return scanAsks(markdown, op.before === "start" || op.after === "start");
        }
        // A `with` is read as an `insert`'s markdown is, over each string the server might parse
        // it as: the module comment says why the scan reads it that way and what that
        // over-reports.
        if (kind === "replace" && typeof replacement === "string") {
          return replacementAsks(replacement);
        }
        if (kind === "retype" && type === "ask" && typeof block === "string") {
          return [{ kind: "retype", id: block, text: "", opening: "" }];
        }
        return [];
      });
    default:
      return [];
  }
}

/**
 * How many asks a call opens for a person, which is what a record of the call reports. Each entry
 * `newAsks` reports counts one, except a `replace`'s: `newAsks` reports the asks of every string
 * the server might parse one `with` as (`replacementReadings`), and those readings are
 * alternatives of one text, of which the server parses exactly one. A `replace` therefore counts
 * as the most asks any one of its readings opens (`scanAsks` over each reading). The gate reads
 * the call before it goes out and cannot know which reading the server takes; the largest reading
 * can only over-count the asks the write carries, where any smaller one could hide some of them.
 * Nothing is grouped by `id` or by `opening`: two asks the call writes count two, whatever they
 * share.
 */
export function newAskCount(toolName: string, input: Readonly<Record<string, unknown>>): number {
  if (toolName !== "dispatch_doc_edit") return newAsks(toolName, input).length;
  let count = 0;
  for (const op of editOps(input)) {
    count +=
      op.op === "replace" && typeof op.with === "string"
        ? Math.max(...replacementReadings(op.with).map((text) => scanAsks(text, false).length))
        : newAsks(toolName, { ops: [op] }).length;
  }
  return count;
}

/** A `dispatch doc-edit`'s operations, each an object; anything else in `ops` is no operation. */
function editOps(input: Readonly<Record<string, unknown>>): Record<string, unknown>[] {
  return (Array.isArray(input.ops) ? input.ops : []).filter(
    (op: unknown): op is Record<string, unknown> => typeof op === "object" && op !== null
  );
}

/** A `dispatch ask`: its question, then its options as the bullet list an ask block holds them in. */
function questionAsk(input: Readonly<Record<string, unknown>>): NewAsk {
  const question = typeof input.question === "string" ? input.question.trim() : "";
  const options = (Array.isArray(input.options) ? input.options : []).flatMap((option: unknown) => {
    if (typeof option !== "object" || option === null) return [];
    const { label, description } = option as Record<string, unknown>;
    return [
      typeof description === "string" && description !== ""
        ? `- ${String(label ?? "")}: ${description}`
        : `- ${String(label ?? "")}`,
    ];
  });
  return {
    kind: "ask",
    text: options.length === 0 ? question : `${question}\n\n${options.join("\n")}`,
    opening: openingOf(question),
  };
}

/** The first `OPENING_CODE_POINTS` code points of `text` with its whitespace runs made one space. */
export function openingOf(text: string): string {
  return Array.from(text.replace(/\s+/g, " ").trim()).slice(0, OPENING_CODE_POINTS).join("");
}

/** Each ask block in `markdown`, in document order, as the Dispatch server reads a document. */
export function askBlocks(markdown: string): NewAsk[] {
  return scanAsks(markdown, true);
}

/** Each ask block in `markdown`, its front matter read first when `frontMatter` is set. */
function scanAsks(markdown: string, frontMatter: boolean): NewAsk[] {
  // A write's CR LF or lone carriage return is stored as a line feed.
  const lines = markdown.replace(/\r\n?/g, "\n").split("\n");
  const typedOpener = (text: string) => typedOpenerAt(text, 0);
  return readAskBlocks(lines, { frontMatter, bodies: true, typedOpener }).map(({ frame, body }) => {
    const { id } = frame;
    const text = body.join("\n").trim();
    return { kind: "block", ...(id === undefined ? {} : { id }), text, opening: openingOf(text) };
  });
}

/**
 * The ask blocks a `replace`'s `with` opens, over every string the server might parse it as
 * (replacementReadings), merged by the whole ask and not by its id alone: two readings that open
 * one id with different text are two variants of what a person would be asked, and both are
 * reported, since the client cannot know which reading the server parses and the gate may not
 * hide the words the server writes into the ask. The gate over-reports deliberately: a variant
 * the server never writes costs the rules model a second reading of nearly the same words, where
 * a hidden one would let words no one reviewed reach a person.
 *
 * The one variant dropped is an empty one standing beside words, which gives the rules model
 * nothing to match on. An ask that carries an id drops its empty variant only where another
 * variant of that same id carries text; an ask with no id, whose empty reading carries nothing to
 * pair it on, is dropped where any reported ask carries text. Where every reading opens an empty
 * ask and nothing else, it is kept, so an ask block written empty still reaches review.
 *
 * The order is the order the readings opened the asks in, which for the `with` as sent is
 * document order.
 */
function replacementAsks(replacement: string): NewAsk[] {
  // An ask's `kind` and `opening` follow from its id and its text, so the two are the whole
  // variant. JSON holds them apart, where a separator character an id may itself hold would let
  // two different pairs read as one key.
  const variants = new Map<string, NewAsk>();
  for (const reading of replacementReadings(replacement)) {
    for (const ask of scanAsks(reading, false)) {
      const key = JSON.stringify([ask.id ?? null, ask.text]);
      if (!variants.has(key)) variants.set(key, ask);
    }
  }
  const found = [...variants.values()];
  const idsWithText = new Set(
    found.flatMap((ask) => (ask.text !== "" && ask.id !== undefined ? [ask.id] : []))
  );
  const anyText = found.some((ask) => ask.text !== "");
  return found.filter(
    (ask) => ask.text !== "" || (ask.id === undefined ? !anyText : !idsWithText.has(ask.id))
  );
}

/**
 * Every string the server might parse a `with` as: the `with` as sent, then what each strip of a
 * leading block marker or task checkbox off its first line leaves. Each strip takes at least two
 * characters, so the readings are few and the loop ends.
 */
function replacementReadings(replacement: string): string[] {
  const readings = [replacement];
  let text = replacement;
  for (let strip = LEADING_STRIP.exec(text); strip !== null; strip = LEADING_STRIP.exec(text)) {
    text = text.slice(strip[0].length);
    readings.push(text);
  }
  return readings;
}

/**
 * A leading list or heading marker (pmdoc's `LeadingBlockMarker`, whose leading spaces and tabs
 * belong to the marker) or a leading task checkbox (pmdoc's `LeadingTaskCheckbox`), which the
 * server cuts off a `with` whose first line carries the matched block's own.
 */
const LEADING_STRIP = /^(?:[ \t]*(?:#{1,6}|[0-9]{1,9}[.)]|[-*+])[ \t]+|\[[ xX]\][ \t]+)/u;
