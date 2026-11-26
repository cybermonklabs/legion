import type { TextAsk } from "@legion/contracts";
import { askOpenerAttributes, askOpenerLines } from "@legion/contracts";

/**
 * A document's live text as `GET /api/v1/artifacts/{id}/text` answers it. A Dispatch of this
 * release answers each ask block's state and answer in `asks`, and its markdown holds none of
 * them; for a document that holds no ask block it sends no `asks`. One of the release before
 * sends no `asks` for any document and writes them on each ask block's opening line instead, as
 * the block's server-owned attributes.
 */
export type LiveText = { readonly markdown: string; readonly asks?: readonly TextAsk[] | null };

/**
 * The asks of `text`, one per ask block in document order: its `asks` member, or, where it sends
 * none, the attributes that carried them on each line that opens an ask block, as a Dispatch of the
 * release before writes them (`askOpenerLines` in packages/contracts, which the web reads them by
 * too). Only a real opener counts: a line that opens the directive past the containers it is in
 * (quotes, list items and footnote definitions), within three columns of them, outside fenced and
 * indented code and outside the front matter a document opens with. A line that quotes an opener
 * in code - a fence, one opened on a footnote's label line too, or a later line of an inline code
 * span that stands four columns or more past its containers - opens no block, and nor does a
 * line of front matter, which is no markdown, so a document's writer cannot make it read as an
 * answer Dispatch recorded. A document of this release with no ask block opens none, and reads no
 * ask.
 */
export function liveTextAsks(text: LiveText): readonly TextAsk[] {
  if (text.asks !== undefined && text.asks !== null) return text.asks;
  // kept for one release: release-before-text
  return openerAsks(askOpenerLines(text.markdown));
}

/** The asks that `lines`, opening lines of ask blocks as `askOpenerLines` gives them, carry on
 *  them, as the release before writes them (`askOpenerAttributes` in packages/contracts, which the
 *  web reads them by too): one per line that names a block id. */
// kept for one release: release-before-text
export function openerAsks(lines: readonly string[]): readonly TextAsk[] {
  return lines.flatMap((line) => {
    const opener = askOpenerAttributes(line);
    return opener === undefined ? [] : [opener];
  });
}

/**
 * One line per answered ask of `asks`, for a live read's trailer:
 * `Answered <block id> by <answerer> at <time>: <answer>`, the words quoted as JSON, so a line
 * break stays on the line, and the options chosen beside them where the answer chose any.
 */
export function answeredAskLines(asks: readonly TextAsk[]): string[] {
  return asks.flatMap((ask) => {
    if (ask.state !== "answered" || ask.answered_by === undefined) return [];
    const words =
      ask.answer === undefined || ask.answer === "" ? undefined : JSON.stringify(ask.answer);
    const chose =
      ask.selected === undefined || ask.selected.length === 0
        ? undefined
        : `chose ${ask.selected.map((option) => JSON.stringify(option)).join(", ")}`;
    const answer =
      words === undefined
        ? (chose ?? "no words")
        : chose === undefined
          ? words
          : `${words} (${chose})`;
    return [
      `Answered ${ask.block_id} by ${ask.answered_by} at ${ask.answered_at ?? "an unrecorded time"}: ${answer}`,
    ];
  });
}
