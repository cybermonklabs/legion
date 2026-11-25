import type { TextAsk } from "@legion/contracts";
import { askOpenerBlockId, askOpenerLines } from "@legion/contracts";

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
 *  them, as the release before writes them: one per line that names a block id. */
// kept for one release: release-before-text
export function openerAsks(lines: readonly string[]): readonly TextAsk[] {
  return lines.flatMap((line) => {
    const opener = openerOf(line);
    return opener === undefined ? [] : [opener];
  });
}

/** The ask block `line`, a line past its containers' markers that opens the directive, opens, its
 *  attributes read as the release before wrote them, or undefined where it names no block id. Its
 *  id is read as `askOpeners` reads it (`askOpenerBlockId`). */
// kept for one release: release-before-text
function openerOf(line: string): TextAsk | undefined {
  const end = line.lastIndexOf("}");
  const start = line.indexOf("ask{");
  if (end <= start) return undefined;
  const attributes = line.slice(start + "ask{".length, end);
  const values = new Map<string, string>();
  for (const [, name = "", raw = ""] of attributes.matchAll(/([\w-]+)="([^"]*)"/g)) {
    values.set(name, attributeValue(raw));
  }
  const id = askOpenerBlockId(line);
  if (id === undefined) return undefined;
  const selected = selectionOf(values.get("selected"));
  const words = values.get("answer");
  return {
    block_id: id,
    ...optional("state", values.get("state") as TextAsk["state"]),
    ...optional("answered_by", values.get("answered_by")),
    ...optional("answered_at", values.get("answered_at")),
    ...optional("selected", selected),
    ...optional("answer", words),
    ...optional("copied_from", values.get("copied_from")),
    ...optional("copied_from_document", values.get("copied_from_document")),
  };
}

/** `{ [name]: value }`, or nothing where value is undefined. */
// kept for one release: release-before-text
function optional<K extends string, V>(name: K, value: V | undefined): { [key in K]?: V } {
  return value === undefined ? {} : ({ [name]: value } as { [key in K]?: V });
}

/** A quoted attribute value with its character references read back: the renderer writes `"`
 *  as `&#x22;`, `&` as `&amp;`, and a line break as `&#xA;`. */
// kept for one release: release-before-text
function attributeValue(raw: string): string {
  const named: Record<string, string> = { amp: "&", quot: '"', lt: "<", gt: ">", apos: "'" };
  return raw.replace(/&(#x[0-9a-fA-F]+|#[0-9]+|[a-z]+);/g, (whole, reference: string) => {
    if (reference.startsWith("#x"))
      return String.fromCodePoint(Number.parseInt(reference.slice(2), 16));
    if (reference.startsWith("#"))
      return String.fromCodePoint(Number.parseInt(reference.slice(1), 10));
    return named[reference] ?? whole;
  });
}

/** The options a `selected` attribute names, a JSON list of strings, or undefined where it holds
 *  none or no such list. */
// kept for one release: release-before-text
function selectionOf(value: string | undefined): string[] | undefined {
  if (value === undefined) return undefined;
  try {
    const parsed: unknown = JSON.parse(value);
    return Array.isArray(parsed) && parsed.every((item) => typeof item === "string")
      ? parsed
      : undefined;
  } catch {
    return undefined;
  }
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
