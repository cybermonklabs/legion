/**
 * The rows the history draws of a handoff's body (`HistoryHandoff.tsx`), and the JSON text of a
 * value past the levels it draws as lists and keys.
 *
 * The rows are the ones today's Conversation card reads of a body (`handoffFields`,
 * `features/conversation/conversation-model.ts`, which keeps its own: R5), but for a list or an
 * object, which is the value the body was parsed to rather than its indented text. Any session
 * can record a handoff on any open issue nested as deep as the server's JSON check takes, 10,000
 * levels, and `JSON.stringify` recurses once a level: in Chrome's engine it throws the call
 * stack's size from about 4,500 levels, a body of 9 KB. So the body is parsed once, and nothing
 * here gives `JSON.stringify` a list or an object.
 */

/** What a row holds: words the session wrote (`prose`), a number, a boolean or null as its JSON
 *  text (`value`), or a list or an object as the body was parsed to (`json`). */
type HandoffRowValue =
  | { readonly shape: "prose" | "value"; readonly text: string }
  | { readonly shape: "json"; readonly value: object };

/** One row of a handoff's opened fields: a top-level field of its body, or one entry of its
 *  `proof` list. */
export type HandoffRow = {
  /** The top-level field the row comes from: its own name, or `proof` for a proof entry. */
  readonly field: string;
  /** Which entry of the `proof` list the row is, counted from 1, or undefined for a field. */
  readonly entry: number | undefined;
  /** The field's name, or `proof <n>` for the nth proof entry. */
  readonly label: string;
  /** The JSON Pointer (RFC 6901) of the row's value in the body, which keys the server's reading
   *  of its text in the handoff's `field_trees`: `/summary` for a field, `/proof/0` for the first
   *  proof entry. */
  readonly path: string;
} & HandoffRowValue;

/** What a row of `value` holds. */
function rowValue(value: unknown): HandoffRowValue {
  if (typeof value === "string") return { shape: "prose", text: value };
  if (value !== null && typeof value === "object") return { shape: "json", value };
  return { shape: "value", text: JSON.stringify(value) };
}

/** The rows a handoff's body expands to, in the order its session wrote the fields, with a
 *  `proof` list split into one row per entry. Undefined when the body is not a JSON object,
 *  which the history then draws as the text it is. */
export function handoffRows(body: string): HandoffRow[] | undefined {
  let parsed: unknown;
  try {
    parsed = JSON.parse(body);
  } catch {
    return undefined;
  }
  if (parsed === null || typeof parsed !== "object" || Array.isArray(parsed)) return undefined;
  const rows: HandoffRow[] = [];
  for (const [field, value] of Object.entries(parsed)) {
    if (field === "proof" && Array.isArray(value)) {
      value.forEach((item: unknown, index) => {
        const entry = index + 1;
        rows.push({
          entry,
          field,
          label: `proof ${entry}`,
          path: `/proof/${index}`,
          ...rowValue(item),
        });
      });
    } else {
      // `~` written `~0` and `/` written `~1`, the escapes of the server's pointer to the key.
      const token = field.replaceAll("~", "~0").replaceAll("/", "~1");
      rows.push({ entry: undefined, field, label: field, path: `/${token}`, ...rowValue(value) });
    }
  }
  return rows;
}

/** A list or an object `jsonText` is inside. */
interface OpenValue {
  /** The object's keys, or undefined for a list. */
  readonly keys: readonly string[] | undefined;
  /** The list's items, or the object's values in the order of its keys. */
  readonly members: readonly unknown[];
  /** How many of them are written. */
  written: number;
}

/**
 * `value`'s JSON text, exactly as `JSON.stringify(value)` writes it, for any value `JSON.parse`
 * gives, however deep it nests: a loop with a stack of its own writes each list and object, an
 * object's keys in the order `JSON.stringify` takes them, and `JSON.stringify` writes only a
 * string - each key too, so every escape is its own - a number, a boolean or null.
 */
export function jsonText(value: unknown): string {
  let text = "";
  const open: OpenValue[] = [];
  let next: unknown = value;
  for (;;) {
    if (Array.isArray(next)) {
      text += "[";
      open.push({ keys: undefined, members: next, written: 0 });
    } else if (next !== null && typeof next === "object") {
      text += "{";
      open.push({ keys: Object.keys(next), members: Object.values(next), written: 0 });
    } else {
      text += JSON.stringify(next);
    }
    // Each list and object whose members are all written is closed; the next member to write is
    // the first one left of the innermost still open.
    let inside = open.at(-1);
    while (inside !== undefined && inside.written === inside.members.length) {
      text += inside.keys === undefined ? "]" : "}";
      open.pop();
      inside = open.at(-1);
    }
    if (inside === undefined) return text;
    if (inside.written > 0) text += ",";
    if (inside.keys !== undefined) text += `${JSON.stringify(inside.keys[inside.written])}:`;
    next = inside.members[inside.written];
    inside.written += 1;
  }
}
