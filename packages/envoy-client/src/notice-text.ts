import { encode, encodeLines } from "@toon-format/toon";
import { UNESCAPED_LINE_BREAKS } from "./line-text";

/**
 * A notice that a plugin writes into its own session: the facts of one event, in the form that
 * TOON's `encode` gives an object, as a card is rendered (`delivery.ts`). The lines sit under the
 * key that names whose notice it is: `envoy` for Envoy's notices, `legion` for Legion's. The first
 * field names the kind (`notice: <kind>`). Each other field is one that the kind declares, stated
 * once, in the order of the declaration. A field holds a fact of the event: a word, a number, an
 * id or a command, or a list of such facts, which TOON writes as a list (`topics[2]: a,b`, its
 * count and then its items). No field holds words that are the same in every notice of its kind.
 * The step a reader takes on one of Envoy's own notices is in the description of the tool it reads
 * with (`envoy_inbox`, `envoy_whoami`: `tool-contract.ts`), keyed by the notice's kind, not in the
 * notice. The kind ends with that tool's name in parentheses (`messages may be missing
 * (envoy_inbox)`): a host that mounts a tool as a device, as Oh My Pi mounts an extension's tool
 * at `xd://<name>`, shows the model only the first line of its description until the model opens
 * the device, so the kind line says which one holds the step.
 *
 * Each kind is declared once, where its notice is built (`noticeKind`), and `noticeText` writes a
 * notice of it from a value for each field the kind declares.
 */
export interface NoticeKind<Field extends string> {
  /** The key the notice's lines sit under. */
  readonly root: "envoy" | "legion";
  /** The kind, which the first field states (`notice: <kind>`). */
  readonly kind: string;
  /** The fields a notice of the kind states after its kind, in the order it states them. */
  readonly fields: readonly Field[];
}

/** The value of one field: one fact, or a list of facts (`readonly string[]`), which the notice
 *  states as a list. `undefined` leaves the field out of this notice. */
export type NoticeValue = string | number | boolean | readonly string[] | undefined;

/** Declares a notice kind: the one place that names its root, its kind and its fields. */
export function noticeKind<const Field extends string>(
  root: "envoy" | "legion",
  kind: string,
  fields: readonly Field[]
): NoticeKind<Field> {
  return { root, kind, fields };
}

/**
 * The text of one notice of `kind`. `values` names every field the kind declares, so a builder
 * cannot leave a fact out by accident; a field whose value is `undefined` is not stated. TOON
 * quotes a value that holds a new line or would read as structure, and the notice is written as
 * a card's fields are (`encodeFields`), so a value that a sender controls (a summary, an error)
 * never starts a field of its own, NEXT LINE, LINE SEPARATOR or PARAGRAPH SEPARATOR in it
 * included.
 */
export function noticeText<Field extends string>(
  kind: NoticeKind<Field>,
  values: { readonly [Name in Field]: NoticeValue }
): string {
  const fields: Record<string, Exclude<NoticeValue, undefined>> = { notice: kind.kind };
  for (const field of kind.fields) {
    const value = values[field];
    if (value !== undefined) fields[field] = value;
  }
  return encodeFields({ [kind.root]: fields });
}

/**
 * `fields` as TOON, as every text a plugin lays out in fields writes them: a notice
 * (`noticeText`), and a card's fields (`renderInbound`, `delivery.ts`), the card that shows its
 * payload as text, or a frame that is no JSON, included. TOON quotes and escapes a new line, a
 * carriage return and a tab in a value, but writes NEXT LINE, LINE SEPARATOR and PARAGRAPH
 * SEPARATOR (`UNESCAPED_LINE_BREAKS`) as they are, where a reader that splits lines the Unicode
 * way breaks the line, so the words after one would read as a field of their own. Each string
 * value, at any depth, is written with one space for each of those (`fieldsOnTheirLines`); keys
 * are written as they are, and so are the words of a block, which is no field.
 */
export function encodeFields(fields: object): string {
  return encode(fieldsOnTheirLines(fields));
}

/** `encodeFields` one line at a time (`encodeLines`): a caller that bounds a layout stops reading
 *  once the lines pass its bound. */
export function encodeFieldLines(fields: object): Iterable<string> {
  return encodeLines(fieldsOnTheirLines(fields));
}

/**
 * `value` with each string in it, at any depth, holding one space for each character of
 * `UNESCAPED_LINE_BREAKS` in it. Nothing of `value` is written to: an object or a list that holds
 * such a string, or holds one that does at any depth, is copied, and every other object or list is
 * itself, copied nowhere. A copy has the prototype of what it copies (none for an object made with
 * none), and holds its own fields alone, in their order, each defined rather than assigned
 * (`defineOwn`); a list's copy keeps its length. One pass, depth first: the objects and lists the
 * walk is inside wait on a stack of its own, as in `parseFrameJSON`, so the walk takes time linear
 * in the value's size at any depth, and a value too deep for the encoder fails in the encoder.
 */
function fieldsOnTheirLines(value: object): object {
  // The objects and lists that hold `at`, from `value` down.
  const holders: Open[] = [];
  let at = opened(value, "");
  for (;;) {
    const field = at.fields[at.next];
    if (field === undefined) {
      const written = at.copy ?? at.from;
      const holder = holders.pop();
      if (holder === undefined) return written;
      write(holder, at.key, at.from, written);
      at = holder;
      continue;
    }
    const [key, item] = field;
    if (typeof item === "object" && item !== null) {
      holders.push(at);
      at = opened(item, key);
    } else {
      const written = typeof item === "string" ? item.replace(UNESCAPED_LINE_BREAKS, " ") : item;
      write(at, key, item, written);
    }
  }
}

/** An object or a list the walk of `fieldsOnTheirLines` is in: the field of its holder it is
 *  (`key`), its own fields in their order, the index of the next one to read, and its copy, made
 *  once a field of it is written as a value other than its own. */
interface Open {
  readonly from: object;
  readonly key: string;
  readonly fields: readonly (readonly [string, unknown])[];
  next: number;
  copy: object | undefined;
}

function opened(from: object, key: string): Open {
  return { from, key, fields: Object.entries(from), next: 0, copy: undefined };
}

/** Gives the field `key` of `at`, which held `item`, the value `written`, and moves past it. The
 *  first value that is not the one it replaces copies `at`, with the fields before it as they
 *  are. */
function write(at: Open, key: string, item: unknown, written: unknown): void {
  if (at.copy === undefined && written !== item) {
    const copy: object = Array.isArray(at.from)
      ? new Array(at.from.length)
      : Object.create(Object.getPrototypeOf(at.from));
    for (let index = 0; index < at.next; index++) {
      const [before, kept] = at.fields[index] ?? [];
      if (before !== undefined) defineOwn(copy, before, kept);
    }
    at.copy = copy;
  }
  if (at.copy !== undefined) defineOwn(at.copy, key, written);
  at.next++;
}

/** Sets `key` of `target` to `value` as an own field of it, defined and not assigned: an
 *  assignment of `__proto__` would set the prototype of `target`. A list's items defined in their
 *  order keep its length. */
export function defineOwn(target: object, key: string, value: unknown): void {
  Object.defineProperty(target, key, {
    value,
    enumerable: true,
    writable: true,
    configurable: true,
  });
}
