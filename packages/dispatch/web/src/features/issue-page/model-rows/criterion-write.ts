import { useState } from "react";

import { ApiError, writeRequest } from "../../../api/client";
import type { Body } from "../../../api/paths";
import { retriesUnderItsKey } from "../../writes/keyed-write";
import { endedStopped, useKeyedWrite } from "../../writes/useKeyedWrite";
import { KEY_HOURS } from "../../writes/write-words";
import { ANSWER_LINES, CONTROL_LABELS, CRITERION_STOPPED_WORDS } from "../page-words";
import type { RoomFacts } from "./room-target";

/**
 * A criterion written under a node of the outline: one block appended to the issue's tree
 * artifact, whose `parent` is the node's own `dispatch://` address and whose words are the box's
 * text as typed.
 *
 * The send is one `POST /api/v1/artifacts/{id}/edits` on the tree artifact, a keyed write the
 * line shows the refusal of itself. A refusal is read by its status alone, never by the server's
 * text: `criterionAnswer` answers the key of `ANSWER_LINES` whose sentence the line draws. A
 * write the reader stopped is no refusal: it may have reached Dispatch, so the line says so in
 * the page's own sentence (`criterionStopped`), with a Retry of that very write under its key.
 */

/** Where a criterion is written: the issue it is written into, the node it stands under (`parent`,
 *  the node's own `dispatch://` address) and the issue's tree artifact (`tree`, the tree read's
 *  `tree`), which it is appended to. */
export interface CriterionAim {
  readonly issueKey: string;
  readonly parent: string;
  readonly tree: string;
}

/** The key of `ANSWER_LINES` a refused criterion's line draws. */
export type CriterionAnswer = keyof Pick<
  typeof ANSWER_LINES,
  "criterion_gone" | "criterion_not_written"
>;

/** A line Markdown or pmdoc would open as a block of its own, read once its leading spaces are
 *  off: a heading, a quote, a table row, and a typed block or its fence (any line opening with
 *  `:`, so no typed line opens a block inside the criterion or closes it); a bullet, its item
 *  empty or not; a fence; a setext underline, `---` among them; a thematic break; a table's
 *  delimiter row that opens with a hyphen, which turns the line above it into a table's header;
 *  a link or footnote definition, which goldmark takes out of its paragraph; and a block of HTML
 *  (any line opening with `<`), which the server refuses, the Proof schema holding none
 *  (pmdoc's `html_flow.go`). Each is escaped before its first character, which is ASCII
 *  punctuation, so the backslash is drawn as nothing. */
const BLOCK_STARTS: readonly RegExp[] = [
  /^[#>|:]/,
  /^[-*+](?:[ \t]|$)/,
  /^(?:`{3}|~{3})/,
  /^(?:=+|-+)[ \t]*$/,
  /^([-*_])(?:[ \t]*\1){2,}[ \t]*$/,
  /^-[-: \t]*\|[-:| \t]*$/,
  /^\[[^\]]+\]:/,
  /^</,
];

/** An ordered list item's digits: it is escaped before the `.` or `)` after them, since a
 *  backslash before a digit would be drawn, a digit being no punctuation Markdown escapes. */
const ORDERED_ITEM = /^\d{1,9}(?=[.)](?:[ \t]|$))/;

/** One typed line as a paragraph's line: its leading spaces off, since a paragraph draws none of
 *  them and four would open code, and one backslash where it would open another block. A blank
 *  line stays blank, a paragraph break. */
function paragraphLine(typed: string): string {
  const line = typed.replace(/^[ \t]+/, "");
  const digits = ORDERED_ITEM.exec(line)?.[0];
  if (digits !== undefined) return `${digits}\\${line.slice(digits.length)}`;
  return BLOCK_STARTS.some((start) => start.test(line)) ? `\\${line}` : line;
}

/** `value` as a directive attribute's quoted value holds it. Both parsers read the value to the
 *  next `"` and decode the character references in it, giving a backslash no meaning (pmdoc's
 *  `parseDirectiveAttributes`, micromark-extension-directive), so `&`, `"` and a line ending are
 *  written as references, as `typedBlockIdAttribute` (`@legion/contracts`) writes an id's, and a
 *  backslash stands as itself. */
function attributeValue(value: string): string {
  return value
    .replaceAll("&", "&amp;")
    .replaceAll('"', "&#x22;")
    .replaceAll("\n", "&#xA;")
    .replaceAll("\r", "&#xD;");
}

/**
 * The criterion `text` makes under the node `parent` (its whole `dispatch://` address): its
 * directive with no block id, which the server mints, each line of the text written so the block
 * holds `paragraph+` and its words read back as typed (`paragraphLine`), and the closing fence. A
 * blank line stays a paragraph break and a single line break a soft break, as a comment's body
 * keeps them. The text's trailing whitespace is dropped; a text that is blank is the caller's to
 * refuse (`useCriterionLine`'s `send`).
 */
export function criterionMarkdown(parent: string, text: string): string {
  return [
    `:::criterion{parent="${attributeValue(parent)}"}`,
    ...text.trimEnd().split("\n").map(paragraphLine),
    ":::",
  ].join("\n");
}

/** The edits route's body that appends `markdown` to an artifact: one insert at its end. The
 *  page's write sends it, and a seed that writes a criterion as the page does sends the same
 *  body, typed as the route's own. */
export function criterionEdits(markdown: string): Body<"/api/v1/artifacts/{id}/edits", "post"> {
  return { ops: [{ after: "end", markdown, op: "insert" }] };
}

/** `POST /api/v1/artifacts/{id}/edits` on the tree artifact `tree`, with `criterionEdits`'s body
 *  for `markdown`. */
export function criterionRequest(tree: string, markdown: string) {
  return writeRequest(
    "post",
    "/api/v1/artifacts/{id}/edits",
    { id: tree },
    criterionEdits(markdown)
  );
}

/** What the line says of a refused criterion, by the refusal's status alone: a 404 says the row
 *  it was written under is gone, and any other refusal that it was not written - a parent
 *  comment deleted meanwhile answers `400 INVALID_MARKDOWN` (pmdoc's `itemReach`), which says
 *  the same. A write the reader stopped is no refusal, and the line never reads it here: it says
 *  `criterionStopped`'s sentence in its place (`useCriterionLine`'s `said`). */
export function criterionAnswer(error: unknown): CriterionAnswer {
  return error instanceof ApiError && error.status === 404
    ? "criterion_gone"
    : "criterion_not_written";
}

/** What the line says of `write` where the reader stopped it, in the page's own sentence
 *  (`CRITERION_STOPPED_WORDS`): while its Retry still goes under the write's key
 *  (`retriesUnderItsKey`), the sentence cut around the hours Dispatch keeps a key (`KEY_HOURS`),
 *  and once Dispatch holds the key no longer, the one that says a Retry writes anew. Undefined
 *  where the newest press did not end stopped (`endedStopped`). The box says a file a reader
 *  stopped the same way (`MentionComposer.tsx`' `outlineUploadMessage`). */
export function criterionStopped(write: Parameters<typeof endedStopped>[0]): string | undefined {
  if (!endedStopped(write)) return undefined;
  if (!retriesUnderItsKey(write.error)) return CRITERION_STOPPED_WORDS.expired;
  const { after, before } = CRITERION_STOPPED_WORDS.unconfirmed;
  return `${before}${KEY_HOURS}${after}`;
}

/** Whether a node of this issue offers the act that writes a criterion: only where the issue's
 *  tree read answers a `tree` and no `view_of`. No facts - an issue whose tree read the cache
 *  does not hold - offer none. */
export function criterionOffered(facts: RoomFacts | undefined): boolean {
  return facts !== undefined && facts.tree !== null && facts.viewOf === null;
}

/** A criterion's aim, and who hears of its write: the box, which holds the line's aim while the
 *  write is out (`OutlineWriter.holdCriterion`), and the line, which lands the reader on the new
 *  criterion and then lets its text go. */
export interface CriterionWriteOptions extends CriterionAim {
  /** Told of a landing: a 200, or `null` for one Dispatch made and kept no answer for. */
  readonly onLanded: () => void;
  /** Told `true` when a send fires the write, and `false` when the write settles, landed or
   *  refused; on a landing, before `onLanded`, so the line's own end of its aim (`clear`) is no
   *  longer held by its write. */
  readonly onOut: (out: boolean) => void;
}

/**
 * The criterion's keyed write: each `mutate` is one press of the line's markdown, in press order
 * with every other criterion written under the same node of the same tree
 * (`criterion:<tree>:<parent>`), shown by the pending-writes indicator only while it is out,
 * since the line shows its refusal itself, and the Retry of a write the reader stopped, which the
 * indicator then no longer holds (`inline`). It is told to `onOut` as it fires and as it settles,
 * and a landing then to `onLanded`. The edits route's path names the tree artifact and no issue,
 * so the write names its issue (`issue`): the issue's tree read is held while it is out and read
 * again once it settles, by the event its landing sends or, with the live feed down, by the
 * settle itself (`tree-read.ts`' `subscribeTreeFreshness`), which draws the new criterion that
 * `onLanded` takes the reader to.
 */
export function useCriterionWrite({
  issueKey,
  onLanded,
  onOut,
  parent,
  tree,
}: CriterionWriteOptions) {
  return useKeyedWrite({
    inline: true,
    issue: issueKey,
    label: () => CONTROL_LABELS.write_criterion,
    onError: () => onOut(false),
    onMutate: () => onOut(true),
    onSuccess: () => {
      onOut(false);
      onLanded();
    },
    request: (markdown: string) => criterionRequest(tree, markdown),
    target: () => `criterion:${tree}:${parent}`,
  });
}

/** The state of the line that writes a criterion. */
export interface CriterionLineState {
  /** The box's text, kept while the write is out, after a refusal and while a stopped write
   *  waits for its Retry. */
  readonly text: string;
  /** Whether the text is blank: such a line sends nothing, by Enter or by Send, whose refusal
   *  says why. */
  readonly blank: boolean;
  /** Sets the text, and lets the last refusal's sentence go. While a stopped write waits for its
   *  Retry it changes nothing: the text is that write's own (`retry`). */
  readonly setText: (text: string) => void;
  /** Writes the text as a criterion, unless it is `blank` or a write of it is still out. While a
   *  stopped write waits, it makes that write again under its key, as `retry` does. */
  readonly send: () => void;
  /** Whether the criterion's write is out. */
  readonly pending: boolean;
  /** What the line says of its last write. Where the reader stopped it: Dispatch has not
   *  confirmed the criterion, which may be written, and what its Retry does (`criterionStopped`).
   *  Else the last refusal's sentence (`criterionAnswer`'s key of `ANSWER_LINES`), until the next
   *  change of the text or the next send. `undefined` while there is neither. */
  readonly said: string | undefined;
  /** Makes the stopped write again: the same request under its own key, which Dispatch answers
   *  with what that write did there where it landed (`KeyedWriteResult.retry`). Present only
   *  while the last write ended stopped, so the line offers its Retry and holds its text exactly
   *  then. */
  readonly retry: (() => void) | undefined;
}

/**
 * The state of the box's criterion line: its text, which stays while the write is out and after
 * a refusal, and the refusal's answer, which goes at the next change of the text or the next send
 * (a new press clears the write's error). The write's going out and its settling are told to
 * `onOut`; a landing, a 200 or `null`, is then told to `onLanded` and lets the text go.
 *
 * A write the reader stopped may have reached Dispatch, and an insert at the end of the tree
 * matches nothing it already holds, so a second write under a new key would write the criterion
 * twice. Until its Retry the line holds that write: its text takes no change, and a send makes
 * that write again under its key rather than a new one (`useKeyedWrite`'s `retry`), so while the
 * line keeps its aim on the node the stopped write is never written twice. Nothing is out after a
 * stop, so the box no longer holds that aim (`OutlineWriter.holdCriterion`): Escape, the target
 * line's control and any other target - a Reply, a criterion under another row - end it.
 * `IssueComposer.tsx` keys the line by its node, so each unmounts this hook, and the stopped write
 * and its Retry with it; a criterion the reader then writes under that node goes under a new key,
 * and may be written twice.
 */
export function useCriterionLine({ onLanded, ...aim }: CriterionWriteOptions): CriterionLineState {
  const [text, setTyped] = useState("");
  const write = useCriterionWrite({
    ...aim,
    onLanded: () => {
      onLanded();
      setTyped("");
    },
  });
  const pending = write.isPending;
  const blank = text.trim() === "";
  const stopped = criterionStopped(write);
  const retry = stopped === undefined ? undefined : write.retry;
  return {
    blank,
    pending,
    retry,
    said:
      stopped ?? (write.error === null ? undefined : ANSWER_LINES[criterionAnswer(write.error)]),
    send: () => {
      if (pending || blank) return;
      if (retry !== undefined) retry();
      else write.mutate(criterionMarkdown(aim.parent, text));
    },
    setText: (next) => {
      if (retry !== undefined) return;
      // A refused write is settled, so letting its error go leaves nothing out.
      if (write.error !== null) write.reset();
      setTyped(next);
    },
    text,
  };
}
