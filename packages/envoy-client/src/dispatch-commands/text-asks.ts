import type { TextAsk } from "@legion/contracts";

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
 * none, each line opening an ask block read for the attributes that carried them, which reads no
 * ask for a document of this release with no ask block, since its markdown opens none. A line
 * quoting an opener (in code, say) is read too, as the release before's readers read it, so it
 * can add a block but never hide one.
 */
export function liveTextAsks(text: LiveText): readonly TextAsk[] {
  if (text.asks !== undefined && text.asks !== null) return text.asks;
  return text.markdown.split("\n").flatMap((line) => {
    const opener = openerOf(line);
    return opener === undefined ? [] : [opener];
  });
}

/** The ask block `line` opens, its attributes read as the release before wrote them, or undefined
 *  where it opens none. */
function openerOf(line: string): TextAsk | undefined {
  const start = line.indexOf("ask{");
  const end = line.lastIndexOf("}");
  if (start < 0 || end <= start) return undefined;
  const attributes = line.slice(start + "ask{".length, end);
  const values = new Map<string, string>();
  for (const [, name = "", raw = ""] of attributes.matchAll(/([\w-]+)="([^"]*)"/g)) {
    values.set(name, attributeValue(raw));
  }
  const id = /^#([^\s}]+)/.exec(attributes)?.[1] ?? values.get("id");
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
function optional<K extends string, V>(name: K, value: V | undefined): { [key in K]?: V } {
  return value === undefined ? {} : ({ [name]: value } as { [key in K]?: V });
}

/** A quoted attribute value with its character references read back: the renderer writes `"`
 *  as `&#x22;`, `&` as `&amp;`, and a line break as `&#xA;`. */
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
