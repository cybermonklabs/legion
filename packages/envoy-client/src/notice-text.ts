import { encode } from "@toon-format/toon";

/**
 * A notice that a plugin writes into its own session: the facts of one event, in the form that
 * TOON's `encode` gives an object, as a card is rendered (`delivery.ts`). The lines sit under the
 * key that names whose notice it is: `envoy` for Envoy's notices, `legion` for Legion's. The first
 * field names the kind (`notice: <kind>`). Each other field is one that the kind declares, stated
 * once, in the order of the declaration. A field holds a fact of the event: a word, a number, an
 * id or a command. No field holds words that are the same in every notice of its kind. The step a
 * reader takes on one of Envoy's own notices is in the description of the tool it reads with
 * (`envoy_inbox`, `envoy_whoami`: `tool-contract.ts`), keyed by the notice's kind, not in the
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

/** The value of one field. `undefined` leaves the field out of this notice. */
export type NoticeValue = string | number | boolean | undefined;

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
 * quotes a value that holds a new line or would read as structure, so a value that a sender
 * controls (a summary, an error) never starts a field of its own.
 */
export function noticeText<Field extends string>(
  kind: NoticeKind<Field>,
  values: { readonly [Name in Field]: NoticeValue }
): string {
  const fields: Record<string, string | number | boolean> = { notice: kind.kind };
  for (const field of kind.fields) {
    const value = values[field];
    if (value !== undefined) fields[field] = value;
  }
  return encode({ [kind.root]: fields });
}
