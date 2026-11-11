/** Head length, in UTF-16 units, for one-line summaries of event text. */
const HEAD_LENGTH = 120;

/** The first `length` units of `text` on one line (whitespace collapsed), with an ellipsis when
 *  cut. `length` defaults to the event summaries' `HEAD_LENGTH`; a surface whose line is shorter,
 *  such as `dispatch inbox`'s rows, names its own. */
export function textHead(text: string, length: number = HEAD_LENGTH): string {
  const flat = text.replace(/\s+/g, " ").trim();
  return flat.length > length ? `${flat.slice(0, length)}…` : flat;
}

/** Head length, in UTF-16 units, for an ask's question where a surface quotes it: enough to tell
 *  one question from another. The answers list reads it (`renderSessionAnswerItem`); the
 *  declined-ask reminder (`declinedAskLines`) prints a question the server cut before it sent it,
 *  to this same number (`declinedAskHeadUnits` in
 *  `packages/envoy/internal/dispatch/api/write_advice.go`). The two are kept equal, so a question
 *  is cut to the same length wherever a reader meets it. */
export const QUESTION_HEAD = 200;

/** The label naming whose the sentences under it are, so nobody reads what Dispatch wrote for
 *  the asking agent as something the person wrote. */
const DISPATCH_WORDS_LABEL = "Dispatch's words:";

/**
 * The sentences Dispatch wrote for the asking agent beside a person's own words (a decline),
 * under the label that names them; `undefined` where the record holds none, which is every
 * resolution but a decline and a Dispatch older than the field.
 *
 * Every surface that shows a closed ask reads it: the delivery card's `resolved` field, a line
 * of the search answer, a bullet of `dispatch_ask_read`'s detail block and a line of the answers
 * list. They draw it differently, so this gives the label and the words and never the layout -
 * the caller puts the one line where its own reader expects it. `render` shapes the words first
 * for a reader that needs it, as the search answer indents what runs under its hit.
 */
export function dispatchWordsText(
  words: string | null | undefined,
  render?: (words: string) => string
): string | undefined {
  if (words === null || words === undefined || words === "") return undefined;
  return `${DISPATCH_WORDS_LABEL} ${render === undefined ? words : render(words)}`;
}

/**
 * How an answer reads as one line: the text alone when nothing was selected, the selected
 * options alone when nothing was typed, and `<options> - <text>` when a human did both.
 */
export function askAnswerText(
  answer:
    | {
        readonly selected?: readonly string[] | null | undefined;
        readonly text?: string | null | undefined;
      }
    | null
    | undefined
): string {
  if (answer === null || answer === undefined) return "";
  const selected = (answer.selected ?? []).join(", ");
  const text = answer.text ?? "";
  if (text === "") return selected;
  if (selected === "") return text;
  return `${selected} - ${text}`;
}
