import type { BlockItemKind } from "./block-schema.generated";
import { hasControlCharacter } from "./dispatch-href";
import { referenceTargetFits, utf8Bytes } from "./dispatch-text-rules";
import { PROJECT_KEY_PATTERN } from "./dispatch-tools";

const encoder = new TextEncoder();

/**
 * The `#b-` fragment of a block reference: every UTF-8 byte of the block id percent-encoded in
 * upper-case hex except ASCII letters, digits and `-`, so no character a reference ends at or the
 * grammar trims (`'`, `.`, `!`, `*`, `_`, `~`, `)`), and no `@` a pin (`@v<N>` after the
 * fragment) could be read from, is ever written bare. Readers accept any percent-encoding; every
 * writer writes this one. Go's twin is `text.EncodeBlockID`; both are tested against
 * DISPATCH_BLOCK_ID_ENCODINGS, whose JSON copy is
 * `packages/envoy/internal/dispatch/text/testdata/dispatch-block-id-encodings.json`. Each code
 * point is encoded on its own: Bun 1.3's `encode` writes a non-ASCII Latin-1 character of a string
 * past 1024 characters as a NUL, and an id has no length bound.
 */
export function encodeBlockId(id: string): string {
  let encoded = "";
  for (const character of id) {
    for (const byte of encoder.encode(character)) {
      const bare =
        (byte >= 0x30 && byte <= 0x39) ||
        (byte >= 0x41 && byte <= 0x5a) ||
        (byte >= 0x61 && byte <= 0x7a) ||
        byte === 0x2d;
      encoded += bare
        ? String.fromCharCode(byte)
        : `%${byte.toString(16).toUpperCase().padStart(2, "0")}`;
    }
  }
  return encoded;
}

/**
 * The most UTF-8 bytes (`utf8Bytes`) a block id, decoded, may hold for text to cite its block. The
 * reference graph keys a block by its id in Postgres B-tree keys, which hold at most 2704 bytes,
 * so the server makes no graph node of a block carrying a longer one, and text naming one cites
 * nothing: a reader of text (the dashboard's composer and links, as the server's `text.Extract`)
 * holds a block id to it. It decides only what the graph keys: such a block is written and read as
 * any other, and a request naming it, which `decodeBlockId` and `parseBlockFragment` read whatever
 * its length, finds no node. Go's twin is `text.MaxBlockIDBytes`.
 */
export const MAX_BLOCK_ID_BYTES = 256;

/**
 * The block id a `#b-` fragment (`encoded`, after the `b-`) names, of any length, or undefined when
 * it names none: empty, holding a raw `@` (a pin is no part of the id; `encodeBlockId` writes `@`
 * as `%40`), not decoding, or decoding to an empty id or one holding a control character. Any
 * valid percent-encoding is read, as the server's `text` package reads it. A reader of text holds
 * the id to MAX_BLOCK_ID_BYTES as well.
 */
export function decodeBlockId(encoded: string): string | undefined {
  if (encoded === "" || encoded.includes("@")) {
    return undefined;
  }
  let id: string;
  try {
    id = decodeURIComponent(encoded);
  } catch {
    return undefined;
  }
  return id === "" || hasControlCharacter(id) ? undefined : id;
}

/**
 * The largest version a block reference's pin (`@v<N>`) may name: Go's `int` on the 64-bit
 * platforms the server runs on, which `text.parseBlock` reads N into. A pin past it names nothing.
 */
const MAX_BLOCK_PIN = 9223372036854775807n;

/**
 * The block a `#b-` fragment (`encoded`, after the `b-`) names, and the version `pin` it pins when
 * it ends in `@v<N>` (a version of a document, or a revision of a comment; whether the block's
 * holder takes a pin, which an issue message does not, is its caller's to ask), as the server's
 * `text.parseBlock` reads it: a raw `@` only as the start of a well-formed pin (`N` a positive
 * integer with no leading zero, at most MAX_BLOCK_PIN), and the id before it as `decodeBlockId`
 * reads it. Undefined for any other fragment. `pin` is N's decimal digits as written, so a pin past
 * the integers a JavaScript number holds exactly is carried and written back unrounded.
 */
export function parseBlockFragment(encoded: string): { block: string; pin?: string } | undefined {
  const at = encoded.indexOf("@");
  const pin = at === -1 ? undefined : encoded.slice(at + 1).match(/^v([1-9][0-9]*)$/)?.[1];
  if (at !== -1 && (pin === undefined || BigInt(pin) > MAX_BLOCK_PIN)) {
    return undefined;
  }
  const block = decodeBlockId(at === -1 ? encoded : encoded.slice(0, at));
  if (block === undefined) {
    return undefined;
  }
  return pin === undefined ? { block } : { block, pin };
}

export const DISPATCH_BLOCK_ID_ENCODINGS: readonly { id: string; encoded: string }[] = [
  { id: "ship-it", encoded: "ship-it" },
  { id: "q:1", encoded: "q%3A1" },
  { id: "décision:1", encoded: "d%C3%A9cision%3A1" },
  { id: "it's", encoded: "it%27s" },
  { id: "a.b_c~d!e*f(g)", encoded: "a%2Eb%5Fc%7Ed%21e%2Af%28g%29" },
  { id: "x y#z%", encoded: "x%20y%23z%25" },
  { id: "it@v2", encoded: "it%40v2" },
  { id: "📊", encoded: "%F0%9F%93%8A" },
];

/**
 * A block reference as `parseCanonicalBlockReference` reads it: the block's document - `issue`
 * and the document's slug `document` (`spec` for the issue's primary document), or `project` and
 * its document's slug - the block id `block`, decoded, and the pinned version's decimal digits
 * `pin`, when the reference is pinned.
 */
export interface DispatchBlockReference {
  readonly issue?: string;
  readonly project?: string;
  readonly document: string;
  readonly block: string;
  readonly pin?: string;
}

// Go's `text` grammars: a project key (PROJECT_KEY_PATTERN, unanchored), an issue key and a
// document slug.
const PROJECT_KEY = PROJECT_KEY_PATTERN.source.slice(1, -1);
const ISSUE_KEY = `${PROJECT_KEY}-[1-9][0-9]*`;
const DOCUMENT_SLUG = "[a-z0-9]+(?:-[a-z0-9]+)*";
const ISSUE_KEY_ONLY = new RegExp(`^${ISSUE_KEY}$`);
const BLOCK_REFERENCE = new RegExp(
  `^dispatch://(?:(${ISSUE_KEY})/spec|(${ISSUE_KEY}|${PROJECT_KEY})/artifact/(${DOCUMENT_SLUG}))#b-(.*)$`,
  "s"
);
// A part of an ask, or a block of a comment or of an issue message, in the two shapes Go's
// `text.parseDispatch` reads with a `#b-` fragment: `<ISSUE>/ask/<id>`, `<ISSUE>/comment/<id>` or
// `<ISSUE>/message/<id>`, and a project document's `<PROJECT>/artifact/<slug>/ask/<id>` or
// `<PROJECT>/artifact/<slug>/comment/<id>` (a project key only, as Go reads an issue key's
// `artifact/` tail as a document alone). The id is checked by itemBlockKind.
const ITEM_BLOCK_REFERENCE = new RegExp(
  `^dispatch://(?:${ISSUE_KEY}/(ask|comment|message)|${PROJECT_KEY}/artifact/${DOCUMENT_SLUG}/(ask|comment))/([^/?#]+)#b-(.*)$`,
  "s"
);

/**
 * The kind of item (`ask`, `comment` or `message`) whose block `value` names - a part of an ask,
 * as a block of a comment - or undefined when it names no block of an ask, a comment or a message.
 * It applies the rules of Go's `text.parseDispatch` to the whole value: the item id is written
 * with no whitespace (JavaScript's `\s`, as Go's `itemSegment`), then decoded as `decodeBlockId`
 * decodes a block id, which refuses a `%` two hex digits do not follow, a decoding that is not
 * UTF-8 or holds a control character, and an `@`, which Go refuses anywhere before a block's
 * fragment. Go's validator reads the value through `text.ParseRef`, which first ends it at a
 * character no reference holds (`referenceEnd`: `'`, `"`, `<`, `>`, a backtick, `[`, `]`, `|` or
 * whitespace) and trims trailing punctuation, so such a value may be refused here with a different
 * reason than Go gives; both refuse it. A message records no revisions, so a pinned message block
 * is no reference.
 */
function itemBlockKind(value: string): "ask" | "comment" | "message" | undefined {
  const match = ITEM_BLOCK_REFERENCE.exec(value);
  if (match === null) {
    return undefined;
  }
  const kind = (match[1] ?? match[2]) as "ask" | "comment" | "message";
  const id = match[3] ?? "";
  if (/\s/.test(id) || decodeBlockId(id) === undefined) {
    return undefined;
  }
  const fragment = parseBlockFragment(match[4] ?? "");
  if (fragment === undefined || (kind === "message" && fragment.pin !== undefined)) {
    return undefined;
  }
  return kind;
}

/**
 * A block reference as every writer writes one: `dispatch://<ISSUE>/spec` for a block of an
 * issue's primary document, `dispatch://<ISSUE or PROJECT>/artifact/<slug>` for any other document
 * of an issue or a project (a project's document whose slug is `spec` included), then `#b-` and
 * the id as `encodeBlockId` writes it, then `@v<pin>` when it is pinned. Go's twin is
 * `text.FormatBlockRef`.
 */
export function formatBlockReference(reference: DispatchBlockReference): string {
  const document =
    reference.issue !== undefined && reference.document === "spec"
      ? `${reference.issue}/spec`
      : `${reference.issue ?? reference.project}/artifact/${reference.document}`;
  const pin = reference.pin === undefined ? "" : `@v${reference.pin}`;
  return `dispatch://${document}#b-${encodeBlockId(reference.block)}${pin}`;
}

/**
 * The block `value` names when it is one block reference written exactly as
 * `formatBlockReference` writes it - the one way a typed block's `block[]` attribute holds a
 * pointer - or undefined. Its document part is an issue's primary document
 * (`dispatch://<ISSUE>/spec`) or a document of an issue or a project
 * (`dispatch://<ISSUE or PROJECT>/artifact/<slug>`), its fragment one `parseBlockFragment` reads,
 * its id at most MAX_BLOCK_ID_BYTES and its target id at most MAX_REFERENCE_TARGET_BYTES, as text
 * cites a block; and `formatBlockReference` must write it back as `value`, which refuses what a
 * reader alone accepts: lower-case or needless percent-encoding, and an issue's primary document
 * spelled `artifact/spec`. Go's twin is `text.CanonicalBlockRef`.
 */
export function parseCanonicalBlockReference(value: string): DispatchBlockReference | undefined {
  const match = BLOCK_REFERENCE.exec(value);
  if (match === null) {
    return undefined;
  }
  const [, specIssue, owner, slug, fragment] = match;
  const parsed = parseBlockFragment(fragment ?? "");
  if (parsed === undefined || utf8Bytes(parsed.block) > MAX_BLOCK_ID_BYTES) {
    return undefined;
  }
  const key = specIssue ?? owner ?? "";
  const document = specIssue === undefined ? (slug ?? "") : "spec";
  if (!referenceTargetFits(`${key}/${document}#${parsed.block}`)) {
    return undefined;
  }
  const reference: DispatchBlockReference = {
    ...(ISSUE_KEY_ONLY.test(key) ? { issue: key } : { project: key }),
    document,
    ...parsed,
  };
  return formatBlockReference(reference) === value ? reference : undefined;
}

/**
 * The attribute that places a typed block in its issue's tree where its rendering cannot: a
 * `block[]` attribute holding at most one reference, because a node has one parent, and the one
 * attribute whose `items` name the conversation's kinds. Go's twin is `pmdoc.ParentAttribute`.
 */
export const PARENT_ATTRIBUTE = "parent";

/**
 * An issue's ask, comment or message, or a block of one, as an item reference names it: the
 * issue, the item's kind, its whole lower-case uuid and, for a block, the block id, decoded.
 */
export interface DispatchItemBlockReference {
  readonly issue: string;
  readonly item: "ask" | "comment" | "message";
  readonly id: string;
  readonly block?: string;
}

const CANONICAL_ITEM_REFERENCE = new RegExp(
  `^dispatch://(${ISSUE_KEY})/(ask|comment|message)/([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})(?:#b-(.*))?$`,
  "s"
);
// An ask, a comment or a message itself, in the shapes Go's `text.parseDispatch` reads with no
// fragment: `<ISSUE>/<kind>/<id>`, and a project document's `<PROJECT>/artifact/<slug>/ask/<id>`
// or `…/comment/<id>`. The id is checked by wholeItemKind.
const WHOLE_ITEM_REFERENCE = new RegExp(
  `^dispatch://(?:${ISSUE_KEY}/(ask|comment|message)|${PROJECT_KEY}/artifact/${DOCUMENT_SLUG}/(ask|comment))/([^/?#]+)$`
);
const CANONICAL_ISSUE_REFERENCE = new RegExp(`^dispatch://${ISSUE_KEY}$`);
const NO_ITEM_KINDS: readonly BlockItemKind[] = [];

/**
 * An issue's item, or a block of one, as every writer of a placement writes it:
 * `dispatch://<ISSUE>/<ask|comment|message>/<uuid>`, then for a block `#b-` and the id as
 * `encodeBlockId` writes it, never pinned. Go's twin is `text.FormatItemBlockRef`.
 */
export function formatItemBlockReference(reference: DispatchItemBlockReference): string {
  const item = `dispatch://${reference.issue}/${reference.item}/${reference.id}`;
  return reference.block === undefined ? item : `${item}#b-${encodeBlockId(reference.block)}`;
}

/**
 * The item of an issue, or the block of one, `value` names when it is one item reference written
 * exactly as `formatItemBlockReference` writes it, or undefined: an item of an issue, with its
 * whole lower-case uuid, and for a block a fragment `parseBlockFragment` reads with no pin, its id
 * at most MAX_BLOCK_ID_BYTES and its target id at most MAX_REFERENCE_TARGET_BYTES, as text cites
 * a block. Go's twin is `text.CanonicalItemBlockRef`.
 */
export function parseCanonicalItemBlockReference(
  value: string
): DispatchItemBlockReference | undefined {
  const match = CANONICAL_ITEM_REFERENCE.exec(value);
  if (match === null) {
    return undefined;
  }
  const [, issue = "", kind, id = "", fragment] = match;
  const item = kind as DispatchItemBlockReference["item"];
  if (fragment === undefined) {
    return { issue, item, id };
  }
  const parsed = parseBlockFragment(fragment);
  if (
    parsed === undefined ||
    parsed.pin !== undefined ||
    utf8Bytes(parsed.block) > MAX_BLOCK_ID_BYTES ||
    !referenceTargetFits(`${item}/${id}#${parsed.block}`)
  ) {
    return undefined;
  }
  const reference: DispatchItemBlockReference = { issue, item, id, block: parsed.block };
  return formatItemBlockReference(reference) === value ? reference : undefined;
}

/**
 * The kind of item (`ask`, `comment` or `message`) `value` names when it names one itself, as Go's
 * `text.ParseRef` reads an item with no fragment - its id written with no whitespace and decoding
 * to no control character - or undefined.
 */
function wholeItemKind(value: string): "ask" | "comment" | "message" | undefined {
  const match = WHOLE_ITEM_REFERENCE.exec(value);
  const id = match?.[3] ?? "";
  if (match === null || /\s/.test(id)) {
    return undefined;
  }
  let decoded: string;
  try {
    decoded = decodeURIComponent(id);
  } catch {
    return undefined;
  }
  if (decoded === "" || hasControlCharacter(decoded)) {
    return undefined;
  }
  return (match[1] ?? match[2]) as "ask" | "comment" | "message";
}

/**
 * The pointers a typed block's `block[]` attribute declaring `items` holds, as markdown writes
 * it: the empty string for none, else references joined by exactly one space, with none leading
 * or trailing and none repeated, each a canonical block of a document
 * (`parseCanonicalBlockReference`) or an item `items` declares - a canonical block of an issue's
 * ask, comment or message (`parseCanonicalItemBlockReference`), or an issue written
 * `dispatch://<ISSUE>`. Undefined for any other value, which is refused, never normalised, so the
 * editor and the server never disagree on one (`blockPointerValueProblem` says why). Go's twin is
 * `pmdoc.ParseBlockPointerValue`; both are tested against DISPATCH_BLOCK_POINTER_VALUES, whose
 * JSON copy is `packages/envoy/internal/dispatch/pmdoc/testdata/block-pointer-values.json`.
 */
export function parseBlockPointerValue(
  value: string,
  items: readonly BlockItemKind[] = NO_ITEM_KINDS
): string[] | undefined {
  if (blockPointerValueProblem(value, items) !== undefined) {
    return undefined;
  }
  return value === "" ? [] : value.split(" ");
}

/**
 * Why `value` is not a `block[]` attribute declaring `items` as markdown writes it
 * (`parseBlockPointerValue`), following the wording of Go's `pmdoc.ParseBlockPointerValue` (the
 * value quoted with `JSON.stringify` where Go uses `%q`, which escapes a control or non-printing
 * character differently), or undefined when it is one.
 */
export function blockPointerValueProblem(
  value: string,
  items: readonly BlockItemKind[] = NO_ITEM_KINDS
): string | undefined {
  if (value === "") {
    return undefined;
  }
  if (value.startsWith(" ") || value.endsWith(" ") || value.includes("  ")) {
    return `${JSON.stringify(value)} must be block references separated by exactly one space, with none leading or trailing`;
  }
  return blockPointersProblem(value.split(" "), items);
}

/** Whether `value` is a reference a `block[]` attribute declaring `items` holds. */
function acceptedPointer(value: string, items: readonly BlockItemKind[]): boolean {
  if (parseCanonicalBlockReference(value) !== undefined) {
    return true;
  }
  const item = parseCanonicalItemBlockReference(value);
  if (item !== undefined && items.includes(item.item)) {
    return true;
  }
  return items.includes("issue") && CANONICAL_ISSUE_REFERENCE.test(value);
}

/** How a reference to an item of kind `item` is written where a `block[]` attribute declares it. */
function itemForm(item: BlockItemKind): string {
  return item === "issue" ? "dispatch://<ISSUE>" : `dispatch://<ISSUE>/${item}/<id>[#b-<block id>]`;
}

/** What a refusal adds for an attribute declaring `items`: the form of each, or nothing. */
function declaredItems(items: readonly BlockItemKind[]): string {
  return items.length === 0 ? "" : `, or an item it declares: ${items.map(itemForm).join(", ")}`;
}

/**
 * The item kind `value`, one pointer a `block[]` attribute declaring `items` holds, names: `ask`,
 * `comment` or `message` for one of those or a block of one, `issue` for an issue, and undefined
 * for a block of a document or a value the attribute does not hold. It is what decides whether an
 * editor may create the pointer (an attribute's `item_versions`). Go's twin is `pmdoc.pointerItem`.
 */
export function pointerItemKind(
  value: string,
  items: readonly BlockItemKind[] = NO_ITEM_KINDS
): BlockItemKind | undefined {
  if (parseCanonicalBlockReference(value) !== undefined) {
    return undefined;
  }
  const item = parseCanonicalItemBlockReference(value);
  if (item !== undefined) {
    return items.includes(item.item) ? item.item : undefined;
  }
  return items.includes("issue") && CANONICAL_ISSUE_REFERENCE.test(value) ? "issue" : undefined;
}

/**
 * Why `values` are not the pointers of a `block[]` attribute declaring `items` - one is a part of
 * an ask or a block of a comment or a message of a kind the attribute does not declare, or one
 * written otherwise than `formatItemBlockReference` writes it, one is an ask, a comment or a
 * message of a kind it declares written otherwise, one is otherwise none the attribute holds (a
 * canonical block of a document, or an issue where `items` declares one), or one is named twice -
 * following the wording of Go's `pmdoc.validateBlockPointerValues`, quoted as above, or undefined
 * when they are.
 */
export function blockPointersProblem(
  values: readonly string[],
  items: readonly BlockItemKind[] = NO_ITEM_KINDS
): string | undefined {
  const seen = new Set<string>();
  for (const value of values) {
    if (!acceptedPointer(value, items)) {
      const block = itemBlockKind(value);
      const whole = wholeItemKind(value);
      // The kind takes the article its noun does: an ask, a comment, a message.
      const article = (block ?? whole) === "ask" ? "an" : "a";
      if (block !== undefined && !items.includes(block)) {
        return `${JSON.stringify(value)} is a block of ${article} ${block}: a block[] attribute points only at a block of a document${declaredItems(items)}`;
      }
      if (block !== undefined) {
        return `${JSON.stringify(value)} is not a block of ${article} ${block} as Dispatch writes one: ${itemForm(block)}, the item's whole lower-case uuid, the block id percent-encoded in upper-case hex except ASCII letters, digits and -, and no pin`;
      }
      if (whole !== undefined && items.includes(whole)) {
        return `${JSON.stringify(value)} is not ${article} ${whole} as Dispatch writes one: ${itemForm(whole)}, the item's whole lower-case uuid, and no pin`;
      }
      return `${JSON.stringify(value)} is not a block reference as Dispatch writes one: dispatch://<ISSUE>/spec#b-<id>, or dispatch://<ISSUE or PROJECT>/artifact/<slug>#b-<id>, the id percent-encoded in upper-case hex except ASCII letters, digits and -, optionally pinned @v<N>${declaredItems(items)}`;
    }
    if (seen.has(value)) {
      return `names ${JSON.stringify(value)} twice`;
    }
    seen.add(value);
  }
  return undefined;
}

const atIdBound = `dispatch://CORE-1/spec#b-${"x".repeat(MAX_BLOCK_ID_BYTES)}`;

/**
 * Every value a `block[]` attribute may hold (`ok`) or not, and why; `problem`, where a row has
 * one, is a part of the reason both parsers must give for refusing it. `items`, where a row has
 * them, are the item kinds the attribute declares; a row naming none is read under an attribute
 * that declares none.
 */
export const DISPATCH_BLOCK_POINTER_VALUES: readonly {
  value: string;
  items?: readonly BlockItemKind[];
  ok: boolean;
  why: string;
  problem?: string;
}[] = [
  {
    value: "dispatch://CORE-1/spec#b-x",
    ok: true,
    why: "a block of an issue's primary document",
  },
  {
    value: "dispatch://CORE-1/artifact/notes-md#b-x",
    ok: true,
    why: "a block of an issue's document",
  },
  {
    value: "dispatch://CORE/artifact/handbook-md#b-x",
    ok: true,
    why: "a block of a project's document",
  },
  {
    value: "dispatch://CORE/artifact/spec#b-x",
    ok: true,
    why: "a project's document whose slug is spec",
  },
  { value: "dispatch://CORE-1/spec#b-d%C3%A9cision%3A1", ok: true, why: "a Unicode id" },
  { value: "dispatch://CORE-1/spec#b-x@v2", ok: true, why: "a pinned block" },
  { value: "dispatch://CORE-1/spec#b-x@v9223372036854775807", ok: true, why: "the largest pin" },
  {
    value: "dispatch://CORE-1/spec#b-x dispatch://CORE/artifact/handbook-md#b-y",
    ok: true,
    why: "two references separated by one space",
  },
  { value: "", ok: true, why: "no pointers" },
  { value: atIdBound, ok: true, why: "an id of exactly 256 bytes" },
  {
    value: "dispatch://CORE-1/artifact/spec#b-x",
    ok: false,
    why: "an issue's primary document is written /spec",
  },
  { value: "dispatch://CORE-1/spec", ok: false, why: "a document, not a block" },
  {
    value: "dispatch://agent/s1/artifact/x#b-y",
    ok: false,
    why: "an agent's document has no blocks to point at",
  },
  { value: "dispatch://CORE-1/spec#b-d%c3%a9", ok: false, why: "lower-case hex" },
  { value: "dispatch://CORE-1/spec#b-%41", ok: false, why: "an over-encoded ASCII letter" },
  { value: "dispatch://CORE-1/spec#b-x%2Dy", ok: false, why: "an over-encoded -" },
  { value: "dispatch://CORE-1/spec#b-%FF", ok: false, why: "invalid UTF-8" },
  { value: "dispatch://CORE-1/spec#b-%0A", ok: false, why: "a control character" },
  { value: "dispatch://CORE-1/spec#x", ok: false, why: "a fragment that is not b-" },
  { value: "dispatch://CORE-1/spec#c-x", ok: false, why: "a fragment that is not b-" },
  { value: "dispatch://CORE-1/spec#b-", ok: false, why: "an empty id" },
  { value: `${atIdBound}x`, ok: false, why: "an id over 256 bytes" },
  { value: "dispatch://CORE-1/artifact/notes-md@v2#b-x", ok: false, why: "a versioned document" },
  { value: "dispatch://CORE-1#b-x", ok: false, why: "an issue names no document" },
  {
    value: "dispatch://CORE-1/spec#b-x@v9223372036854775808",
    ok: false,
    why: "a pin past the largest",
  },
  { value: "dispatch://CORE-1/spec#b-x@v01", ok: false, why: "a pin with a leading zero" },
  { value: "dispatch://CORE-1/spec#b-x@v0", ok: false, why: "a pin of zero" },
  {
    value: "https://dispatch.example/issues/CORE-1/spec#b-x",
    ok: false,
    why: "a dashboard URL of a block",
  },
  { value: "dispatch://CORE-1/spec#b-x.", ok: false, why: "trailing punctuation" },
  { value: "dispatch://CORE-1/spec#b-x)", ok: false, why: "a trailing parenthesis" },
  {
    value: "dispatch://CORE-1/spec#b-x,dispatch://CORE-1/spec#b-y",
    ok: false,
    why: "references joined by a comma",
  },
  {
    value: "dispatch://CORE-1/spec#b-x dispatch://CORE-1/spec#b-x",
    ok: false,
    why: "a repeated reference",
  },
  {
    value: "dispatch://CORE-1/spec#b-x  dispatch://CORE-1/spec#b-y",
    ok: false,
    why: "a doubled space",
  },
  { value: " dispatch://CORE-1/spec#b-x", ok: false, why: "a leading space" },
  { value: "dispatch://CORE-1/spec#b-x ", ok: false, why: "a trailing space" },
  {
    value: "dispatch://CORE-1/spec#b-x\tdispatch://CORE-1/spec#b-y",
    ok: false,
    why: "a tab between references",
  },
  {
    value: "dispatch://CORE-1/spec#b-x\uFEFFdispatch://CORE-1/spec#b-y",
    ok: false,
    why: "U+FEFF between references",
  },
  {
    value: "dispatch://CORE-1/comment/11111111-1111-4111-8111-111111111111#b-b2",
    ok: false,
    why: "a block of an issue's comment",
    problem: "is a block of a comment: a block[] attribute points only at a block of a document",
  },
  {
    value: "dispatch://CORE-1/comment/11111111-1111-4111-8111-111111111111#b-b2@v1",
    ok: false,
    why: "a block of a revision of an issue's comment",
    problem: "is a block of a comment: a block[] attribute points only at a block of a document",
  },
  {
    value: "dispatch://CORE/artifact/handbook-md/comment/88888888-8888-4888-8888-888888888888#b-b2",
    ok: false,
    why: "a block of a project document's comment",
    problem: "is a block of a comment: a block[] attribute points only at a block of a document",
  },
  {
    value: "dispatch://CORE-1/message/33333333-3333-4333-8333-333333333333#b-b2",
    ok: false,
    why: "a block of an issue message",
    problem: "is a block of a message: a block[] attribute points only at a block of a document",
  },
  {
    value: "dispatch://CORE-1/ask/22222222-2222-4222-8222-222222222222#b-b2",
    ok: false,
    why: "a part of an issue's ask",
    problem: "is a block of an ask: a block[] attribute points only at a block of a document",
  },
  {
    value: "dispatch://CORE-1/ask/22222222-2222-4222-8222-222222222222#b-b2@v1",
    ok: false,
    why: "a part of a revision of an issue's ask",
    problem: "is a block of an ask: a block[] attribute points only at a block of a document",
  },
  {
    value: "dispatch://CORE/artifact/handbook-md/ask/99999999-9999-4999-8999-999999999999#b-b2",
    ok: false,
    why: "a part of a project document's ask",
    problem: "is a block of an ask: a block[] attribute points only at a block of a document",
  },
  {
    value: "dispatch://CORE/artifact/handbook-md/ask/99999999-9999-4999-8999-999999999999#b-b2@v1",
    ok: false,
    why: "a part of a revision of a project document's ask",
    problem: "is a block of an ask: a block[] attribute points only at a block of a document",
  },
  {
    value: "dispatch://CORE-1/artifact/notes-md/ask/a1#b-b2",
    ok: false,
    why: "an issue document's ask, a shape no reference has",
    problem: "is not a block reference as Dispatch writes one",
  },
  {
    value: "dispatch://CORE-1/artifact/notes-md/comment/c1#b-x",
    ok: false,
    why: "an issue document's comment, a shape no reference has",
    problem: "is not a block reference as Dispatch writes one",
  },
  {
    value: "dispatch://CORE-1/comment/%ZZ#b-x",
    ok: false,
    why: "a comment id that does not decode",
    problem: "is not a block reference as Dispatch writes one",
  },
  {
    value:
      "dispatch://CORE-1/spec#b-x dispatch://CORE-1/message/33333333-3333-4333-8333-333333333333#b-b2",
    ok: false,
    why: "a document's block, then an issue message's",
    problem: "is a block of a message: a block[] attribute points only at a block of a document",
  },
  {
    value: "dispatch://CORE-1/comment/11111111-1111-4111-8111-111111111111#b-b2",
    items: ["ask", "comment", "message"],
    ok: true,
    why: "a block of an issue's comment, where a parent is written",
  },
  {
    value: "dispatch://CORE-1/message/33333333-3333-4333-8333-333333333333#b-b2",
    items: ["ask", "comment", "message"],
    ok: true,
    why: "a block of an issue message, where a parent is written",
  },
  {
    value: "dispatch://CORE-1/ask/22222222-2222-4222-8222-222222222222#b-b2",
    items: ["ask", "comment", "message"],
    ok: true,
    why: "a part of an issue's ask, where a parent is written",
  },
  {
    value: "dispatch://CORE-1/comment/4f1c2d3e-5a6b-4c7d-8e9f-0a1b2c3d4e5f#b-d%C3%A9cision%3A1",
    items: ["comment"],
    ok: true,
    why: "a comment's block with a Unicode id, under an attribute declaring comments",
  },
  {
    value: "dispatch://CORE-1/spec#b-x",
    items: ["ask", "comment", "message"],
    ok: true,
    why: "a block of a document, where a parent is written",
  },
  {
    value: "dispatch://CORE-1/message/33333333-3333-4333-8333-333333333333#b-b2",
    items: ["comment"],
    ok: false,
    why: "a kind of item the attribute does not declare",
    problem:
      "is a block of a message: a block[] attribute points only at a block of a document, or an item it declares: dispatch://<ISSUE>/comment/<id>[#b-<block id>]",
  },
  {
    value: "dispatch://CORE-1/comment/11111111-1111-4111-8111-111111111111#b-b2@v1",
    items: ["ask", "comment", "message"],
    ok: false,
    why: "a block of a revision of an issue's comment: a parent is a place, not a quotation",
    problem: "is not a block of a comment as Dispatch writes one",
  },
  {
    value: "dispatch://CORE-1/ask/22222222-2222-4222-8222-222222222222#b-b2@v1",
    items: ["ask", "comment", "message"],
    ok: false,
    why: "a part of a revision of an issue's ask",
    problem: "is not a block of an ask as Dispatch writes one",
  },
  {
    value: "dispatch://CORE-1/comment/4F1C2D3E-5A6B-4C7D-8E9F-0A1B2C3D4E5F#b-b2",
    items: ["ask", "comment", "message"],
    ok: false,
    why: "an upper-case uuid",
    problem: "is not a block of a comment as Dispatch writes one",
  },
  {
    value: "dispatch://CORE-1/comment/11111111#b-b2",
    items: ["ask", "comment", "message"],
    ok: false,
    why: "a prefix of a uuid",
    problem: "is not a block of a comment as Dispatch writes one",
  },
  {
    value: "dispatch://CORE/artifact/handbook-md/comment/88888888-8888-4888-8888-888888888888#b-b2",
    items: ["ask", "comment", "message"],
    ok: false,
    why: "a block of a project document's comment, which holds no conversation of an issue",
    problem: "is not a block of a comment as Dispatch writes one",
  },
  {
    value: "dispatch://CORE-1/comment/11111111-1111-4111-8111-111111111111#b-%62",
    items: ["ask", "comment", "message"],
    ok: false,
    why: "an over-encoded block id of a comment",
    problem: "is not a block of a comment as Dispatch writes one",
  },
  {
    value: "dispatch://CORE-1/comment/11111111-1111-4111-8111-111111111111",
    items: ["ask", "comment", "message"],
    ok: true,
    why: "an issue's comment itself, where a parent is written",
  },
  {
    value: "dispatch://CORE-1/ask/22222222-2222-4222-8222-222222222222",
    items: ["ask", "comment", "message"],
    ok: true,
    why: "an issue's ask itself, where a parent is written",
  },
  {
    value: "dispatch://CORE-1/message/33333333-3333-4333-8333-333333333333",
    items: ["ask", "comment", "message"],
    ok: true,
    why: "an issue message itself, where a parent is written",
  },
  {
    value: "dispatch://CORE-1/comment/11111111-1111-4111-8111-111111111111@v2",
    items: ["ask", "comment", "message"],
    ok: false,
    why: "a pinned comment: a parent is a place, not a quotation",
    problem:
      "is not a comment as Dispatch writes one: dispatch://<ISSUE>/comment/<id>[#b-<block id>]",
  },
  {
    value: "dispatch://CORE-1/ask/22222222",
    items: ["ask", "comment", "message"],
    ok: false,
    why: "an ask named by a prefix of its uuid",
    problem: "is not an ask as Dispatch writes one",
  },
  {
    value: "dispatch://CORE-1/message/4F1C2D3E-5A6B-4C7D-8E9F-0A1B2C3D4E5F",
    items: ["ask", "comment", "message"],
    ok: false,
    why: "a message named by an upper-case uuid",
    problem: "is not a message as Dispatch writes one",
  },
  {
    value: "dispatch://CORE/artifact/handbook-md/comment/88888888-8888-4888-8888-888888888888",
    items: ["ask", "comment", "message"],
    ok: false,
    why: "a project document's comment, which is no conversation of an issue",
    problem: "is not a comment as Dispatch writes one",
  },
  {
    value: "dispatch://CORE-1/message/33333333-3333-4333-8333-333333333333",
    items: ["comment"],
    ok: false,
    why: "an issue message, where an attribute declares comments alone",
    problem:
      "is not a block reference as Dispatch writes one: dispatch://<ISSUE>/spec#b-<id>, or dispatch://<ISSUE or PROJECT>/artifact/<slug>#b-<id>, the id percent-encoded in upper-case hex except ASCII letters, digits and -, optionally pinned @v<N>, or an item it declares: dispatch://<ISSUE>/comment/<id>[#b-<block id>]",
  },
  {
    value: "dispatch://CORE-1/comment/11111111-1111-4111-8111-111111111111",
    ok: false,
    why: "an issue's comment, where an attribute declares no item",
    problem: "is not a block reference as Dispatch writes one",
  },
  {
    value:
      "dispatch://CORE-1/comment/11111111-1111-4111-8111-111111111111#b-b2 dispatch://CORE-1/comment/11111111-1111-4111-8111-111111111111#b-b2",
    items: ["ask", "comment", "message"],
    ok: false,
    why: "a repeated block of a comment",
    problem: "twice",
  },
  {
    value: "dispatch://CORE-2",
    items: ["issue"],
    ok: true,
    why: "an issue, where a needs names one",
  },
  {
    value: "dispatch://CORE-1/spec#b-x dispatch://CORE-2",
    items: ["issue"],
    ok: true,
    why: "a plan task and an issue, where a needs names both",
  },
  {
    value: "dispatch://CORE-2",
    ok: false,
    why: "an issue, where an attribute declares none",
    problem: "is not a block reference as Dispatch writes one",
  },
  {
    value: "dispatch://CORE-2/",
    items: ["issue"],
    ok: false,
    why: "an issue with a trailing slash",
    problem: "is not a block reference as Dispatch writes one",
  },
  {
    value: "dispatch://CORE",
    items: ["issue"],
    ok: false,
    why: "a project, not an issue",
    problem: "or an item it declares: dispatch://<ISSUE>",
  },
  {
    value: "dispatch://CORE-1/comment/11111111-1111-4111-8111-111111111111#b-b2",
    items: ["issue"],
    ok: false,
    why: "a needs never names a conversation",
    problem:
      "is a block of a comment: a block[] attribute points only at a block of a document, or an item it declares: dispatch://<ISSUE>",
  },
];

/**
 * Each character a typed block's written `#id` cannot hold. The browser editor's parser
 * (micromark-extension-directive) ends the shortcut at a space, a tab, a line ending, `#`, `.` or
 * `}`, and reads a line whose shortcut holds a quote, `<`, `=`, `>` or a backtick as no directive.
 * An `&` ends nothing there, but that parser decodes a character reference in the shortcut and
 * Go's reader refuses one holding `&`, so the two never read one back alike. Go's twin is
 * `pmdoc.typedBlockIDShortcutExcluded`.
 */
export const TYPED_BLOCK_ID_SHORTCUT_EXCLUDED = "\t\n\r \"#&'.<=>`}";

/**
 * How a typed block's opening line writes the block's id, as Go's renderer
 * (`pmdoc.TypedBlockIDAttribute`) writes it: `#id` when the id is not empty and holds no
 * TYPED_BLOCK_ID_SHORTCUT_EXCLUDED character, else `id="…"` with its `&`, `"`, line feeds and
 * carriage returns as character references (`q:1` is written `#q:1`, `a b` `id="a b"`, `R&D`
 * `id="R&amp;D"`). A reader finding a block's opener in a version's markdown looks for this
 * spelling. Both are tested against DISPATCH_TYPED_BLOCK_ID_SPELLINGS, whose JSON copy is
 * `packages/envoy/internal/dispatch/pmdoc/testdata/typed-block-id-spellings.json`.
 */
export function typedBlockIdAttribute(id: string): string {
  if (id !== "" && ![...TYPED_BLOCK_ID_SHORTCUT_EXCLUDED].some((stop) => id.includes(stop))) {
    return `#${id}`;
  }
  // `&` first, so no reference written after it is written again.
  const value = id
    .replaceAll("&", "&amp;")
    .replaceAll('"', "&#x22;")
    .replaceAll("\n", "&#xA;")
    .replaceAll("\r", "&#xD;");
  return `id="${value}"`;
}

/** A line that opens or closes a document's front matter. */
const FRONT_MATTER_FENCE = /^---[ \t]*$/;

/**
 * Each line of `markdown` that opens an ask block, from the opener's first colon on, as pmdoc's
 * parser reads the canonical rendering a text route serves: a typed block named `ask` (three
 * colons or more, `ask{`, and a line that ends with `}`), outside fenced and indented code and
 * outside the front matter a document opens with. The scan reads the document's blocks as
 * goldmark (github.com/yuin/goldmark v1.8.6) and pmdoc's own block parsers
 * (packages/envoy/internal/dispatch/pmdoc) read them, as packages/envoy-client's
 * askgate-new-asks.ts does:
 *
 * - A line first goes past the containers it continues: a quote's `>` and one column after it, a
 *   list item's content columns, and a footnote definition's four columns. A blank line continues
 *   a list item and a footnote definition, but not a quote.
 * - A block opens only within three columns of those containers. A line four columns or more past
 *   them continues the paragraph before it, or else opens indented code. So the later line of an
 *   inline code span, which keeps its spaces, opens no block there.
 * - A container opens within three columns too: a `>`, a list marker (`-`, `*`, `+`, or one to
 *   nine digits and `.` or `)`) followed by a space, a tab or the line's end, or a footnote label
 *   (footnoteLabelEnd). Every space and tab after a label belongs to it.
 * - A fence is three or more backticks or tildes, and a backtick fence's info string holds no
 *   backtick. A run of the same character at least as long closes it, within three columns of its
 *   containers and with only spaces and tabs after it. A container that ends ends the fence in it.
 * - A typed block closes at a line of as many colons or more.
 * - A line that a container does not continue, and that opens no block, continues the paragraph
 *   in that container (a lazy line), and the container stays open.
 *
 * The web's reader of a live text of the release before, and the CLI's reader of a live text and
 * its check of a version for open ask blocks before an approval request, all read openers here.
 * So none takes a line that a document's writer quoted for a state or an answer that Dispatch
 * recorded. Go's twin is refs.questionBlocks.
 */
export function askOpenerLines(markdown: string): string[] {
  const lines = markdown.split("\n").map((line) => line.replace(/\r$/, ""));
  const frontMatter = frontMatterEnd(lines);
  const scan: OpenerScan = {
    lines: lines.slice(frontMatter.end),
    index: 0,
    opened: [],
    openers: [],
    emptyItemWithBlankLines: false,
    skipList: false,
    unclosedFrontMatter: frontMatter.unclosed,
  };
  const document: ScanBlock = {
    kind: "document",
    parent: undefined,
    previous: undefined,
    children: 0,
    last: undefined,
  };
  for (const [index, text] of scan.lines.entries()) {
    scan.index = index;
    readScanLine(scan, document, new ScanLine(text));
  }
  return scan.openers;
}

/** The character references `typedBlockIdAttribute` writes in a quoted id, each with its character. */
const ID_REFERENCES: Readonly<Record<string, string>> = {
  "&amp;": "&",
  "&#x22;": '"',
  "&#xA;": "\n",
  "&#xD;": "\r",
};

/**
 * The block id that `line`, an ask block's opener as `askOpenerLines` gives it, names: the
 * attribute right after the directive's `{`, written as `typedBlockIdAttribute` writes an id, then
 * a space or the closing brace. A `#id` runs to the first TYPED_BLOCK_ID_SHORTCUT_EXCLUDED
 * character, so a no-break space, a line separator (U+2028) or an ideographic space stays in the
 * id. An `id="…"` names the id that its `&amp;`, `&#x22;`, `&#xA;` and `&#xD;` read back to, where
 * the renderer writes that id so. Undefined where the line writes its id in no spelling that the
 * renderer writes. `askOpeners` reads ids here, and so does the CLI's reader of an opener's
 * attributes.
 */
export function askOpenerBlockId(line: string): string | undefined {
  // Each line opens the directive (`:::ask{`), so its first brace is the directive's.
  const attributes = line.slice(line.indexOf("{") + 1);
  let id: string;
  let end: number;
  if (attributes.startsWith("#")) {
    end = 1;
    while (end < attributes.length && !TYPED_BLOCK_ID_SHORTCUT_EXCLUDED.includes(attributes[end])) {
      end++;
    }
    id = attributes.slice(1, end);
  } else if (attributes.startsWith('id="')) {
    end = attributes.indexOf('"', 'id="'.length) + 1;
    if (end === 0) return undefined;
    id = attributes
      .slice('id="'.length, end - 1)
      .replace(/&(?:amp|#x22|#xA|#xD);/g, (reference) => ID_REFERENCES[reference] ?? reference);
  } else {
    return undefined;
  }
  const after = attributes.charAt(end);
  if (after !== " " && after !== "}") return undefined;
  return typedBlockIdAttribute(id) === attributes.slice(0, end) ? id : undefined;
}

/** The lines of `openers`, a version's ask block openers as `askOpenerLines` gives them, that
 *  open the ask block `blockId`: the lines whose id (`askOpenerBlockId`) is `blockId`. The web's
 *  and the CLI's readers of one block's opening line both take it here. */
export function askOpeners(openers: readonly string[], blockId: string): string[] {
  return openers.filter((line) => askOpenerBlockId(line) === blockId);
}

/**
 * Where `lines` go on past the front matter a document opens with, as pmdoc's
 * parseFrontmatterBlock reads it: a first line of `---` and any spaces or tabs opens it, and the
 * first later line that is the same closes it. pmdoc keeps it word for word, so no line of it opens
 * a block. `end` is 0 where the document opens with none, or with an opener nothing closes, which
 * the renderer never writes: a rule opening a document is written `***`. Past such an opener
 * (`unclosed`), no quote, list or footnote definition opens at the document's level (pmdoc's
 * frontmatterAttempt).
 */
function frontMatterEnd(lines: readonly string[]): { end: number; unclosed: boolean } {
  if (!FRONT_MATTER_FENCE.test(lines[0] ?? "")) return { end: 0, unclosed: false };
  const close = lines.findIndex((line, index) => index > 0 && FRONT_MATTER_FENCE.test(line));
  return close < 0 ? { end: 0, unclosed: true } : { end: close + 1, unclosed: false };
}

/** What the opener scan carries from line to line, as goldmark's parser context carries it. */
interface OpenerScan {
  /** The markdown's lines past its front matter, and the index of the one being read. */
  readonly lines: readonly string[];
  index: number;
  /** The blocks open, outermost first, as `pc.OpenedBlocks()`; the document is not among them. */
  readonly opened: ScanBlock[];
  /** Each ask block opener found so far, from its first colon on. */
  readonly openers: string[];
  /** goldmark's `emptyListItemWithBlankLines`. */
  emptyItemWithBlankLines: boolean;
  /** goldmark's `skipListParserKey`: a list item closed at a sibling's marker. */
  skipList: boolean;
  /** A front-matter opener that no later line closes (frontMatterEnd). */
  readonly unclosedFrontMatter: boolean;
}

interface ScanBlockBase {
  readonly parent: ScanBlock | undefined;
  /** The block opened before it in its parent (goldmark's `PreviousSibling`). */
  readonly previous: ScanBlock | undefined;
  /** How many blocks opened inside this one, and the last of them. */
  children: number;
  last: ScanBlock | undefined;
}

/** A block as the scan holds it open. A `leaf` is one that closes at the next line. */
type ScanBlock =
  | (ScanBlockBase & { readonly kind: "document" | "quote" | "footnote" | "leaf" })
  | (ScanBlockBase & {
      readonly kind: "paragraph";
      /** The index of its last line, and whether a line after its first is a table's delimiter row. */
      lastLine: number;
      table: boolean;
    })
  | (ScanBlockBase & {
      readonly kind: "code";
      /** The index of its last line that is not blank, and whether a blank line came before it. */
      lastLine: number;
      readonly blankBefore: boolean;
    })
  | (ScanBlockBase & { readonly kind: "list"; readonly marker: string })
  | (ScanBlockBase & {
      readonly kind: "item";
      /** The columns that the item's content stands past its container's. */
      readonly offset: number;
    })
  | (ScanBlockBase & {
      readonly kind: "typed";
      /** The colons it opened with: only a line of as many or more closes it. */
      readonly fence: number;
      /** The columns its opening line's text stands past its container's, taken off its lines. */
      readonly indent: number;
    })
  | (ScanBlockBase & { readonly kind: "fence"; readonly char: string; readonly length: number });

/** What a block's continuation does with a line, as goldmark's `State`. */
type Continuation = "close" | "continue" | "children";

/** One line of markdown, read from the column that the containers around a block have taken. */
class ScanLine {
  readonly text: string;
  /** The column each character starts at, a tab reaching the next multiple of four, then the end's. */
  readonly #starts: number[];
  #pos = 0;
  #column = 0;

  constructor(text: string) {
    this.text = text;
    const starts = [0];
    let column = 0;
    for (let index = 0; index < text.length; index++) {
      column += text[index] === "\t" ? 4 - (column % 4) : 1;
      starts.push(column);
    }
    this.#starts = starts;
  }

  /** The column taken so far. */
  get column(): number {
    return this.#column;
  }

  /** The column that the character at `index` starts at. */
  columnAt(index: number): number {
    return this.#starts[index];
  }

  /** The index of the first character from `index` on that is no space or tab. */
  #afterSpaces(index: number): number {
    let end = index;
    while (end < this.text.length && (this.text[end] === " " || this.text[end] === "\t")) end++;
    return end;
  }

  /** The columns of the spaces and tabs that start at `index`. */
  spacesAt(index: number): number {
    return this.columnAt(this.#afterSpaces(index)) - this.columnAt(index);
  }

  /** The columns of space and tab from the column taken, and the index of the character after them. */
  indent(): { width: number; at: number } {
    const at = this.#afterSpaces(this.#pos);
    return { width: this.columnAt(at) - this.#column, at };
  }

  /** Whether nothing but spaces and tabs is left. */
  blank(): boolean {
    return this.indent().at === this.text.length;
  }

  /** Takes `columns` columns of the spaces and tabs ahead, a tab partly if it spans more. */
  skip(columns: number): void {
    this.#column += columns;
    while (this.#pos < this.text.length && this.columnAt(this.#pos + 1) <= this.#column) {
      this.#pos++;
    }
  }

  /** Takes the line up to the character at `index`. */
  advanceTo(index: number): void {
    this.#pos = index;
    this.#column = this.columnAt(index);
  }

  /** What is left of the line, a tab partly taken written as the columns it has left. */
  rest(): string {
    const start = this.columnAt(this.#pos);
    if (this.#pos < this.text.length && start < this.#column) {
      return (
        " ".repeat(this.columnAt(this.#pos + 1) - this.#column) + this.text.slice(this.#pos + 1)
      );
    }
    return this.text.slice(this.#pos);
  }
}

/** One line through the open blocks, then the blocks it opens: goldmark's `parseBlocks` loop body. */
function readScanLine(scan: OpenerScan, document: ScanBlock, line: ScanLine): void {
  if (scan.opened.length === 0) {
    if (!line.blank()) openScanBlocks(scan, document, line);
    return;
  }
  const opened = [...scan.opened];
  const lastIndex = opened.length - 1;
  for (const [index, block] of opened.entries()) {
    // A paragraph is continued by openScanBlocks, when the line opens no block that interrupts it.
    if (block.kind !== "paragraph") {
      const continuation = continueScanBlock(scan, block, line);
      if (continuation === "children" && index === lastIndex) {
        openScanBlocks(scan, block, line);
        return;
      }
      if (continuation !== "close") continue;
    }
    // The block closes here, or the line continues the paragraph last opened lazily.
    if (openScanBlocks(scan, opened[index - 1] ?? document, line) !== "paragraph") {
      scan.opened.splice(index, lastIndex - index + 1);
    }
    return;
  }
}

/** goldmark's `openBlocks`: the blocks `line` opens inside `parent`, else a paragraph's continuation. */
function openScanBlocks(
  scan: OpenerScan,
  parent: ScanBlock,
  line: ScanLine
): "opened" | "paragraph" | "none" {
  const continuable = scan.opened.at(-1)?.kind === "paragraph";
  let result: "opened" | "none" = "none";
  for (let into = parent; ; ) {
    const { width, at } = line.indent();
    if (at === line.text.length) break;
    const char = line.text.charAt(at);
    const interrupting = continuable && result === "none";
    let opened: { block: ScanBlock; children: boolean } | undefined;
    for (const parser of SCAN_PARSERS) {
      if (parser.triggers !== undefined && !parser.triggers.includes(char)) continue;
      if (interrupting && !parser.interrupts) continue;
      // No block but indented code opens four columns or more past its container.
      if (width > 3 && !parser.indented) continue;
      opened = parser.open(scan, into, line, width, at);
      if (opened !== undefined) break;
    }
    if (opened === undefined) break;
    into.children++;
    into.last = opened.block;
    scan.opened.push(opened.block);
    result = "opened";
    if (!opened.children) break;
    into = opened.block;
  }
  const last = scan.opened.at(-1);
  if (result === "none" && last?.kind === "paragraph" && !line.blank()) {
    if (TABLE_DELIMITER_ROW.test(line.rest())) last.table = true;
    last.lastLine = scan.index;
    return "paragraph";
  }
  return result;
}

/** A block parser: goldmark's `BlockParser` as the scan needs it. */
interface ScanParser {
  /** The characters a line's text starts with for this parser to try it; any, when undefined. */
  readonly triggers?: string;
  /** Whether the block may open where it would interrupt a paragraph (`CanInterruptParagraph`). */
  readonly interrupts: boolean;
  /** Whether the block may open four columns or more into its container (`CanAcceptIndentedLine`). */
  readonly indented: boolean;
  open(
    scan: OpenerScan,
    parent: ScanBlock,
    line: ScanLine,
    width: number,
    at: number
  ): { block: ScanBlock; children: boolean } | undefined;
}

/** A block's place in the tree, empty, inside `parent`. */
function under(parent: ScanBlock): ScanBlockBase {
  return { parent, previous: parent.last, children: 0, last: undefined };
}

/** A block that holds nothing and closes at the next line: a heading, a rule, a refused directive. */
function leafAt(parent: ScanBlock, line: ScanLine): { block: ScanBlock; children: boolean } {
  line.advanceTo(line.text.length);
  return { block: { kind: "leaf", ...under(parent) }, children: false };
}

/** Whether an unclosed front-matter opener keeps a list, quote or footnote definition from opening
 *  in `parent` (frontMatterEnd). */
function frontMatterHolds(scan: OpenerScan, parent: ScanBlock): boolean {
  return scan.unclosedFrontMatter && parent.kind === "document";
}

/**
 * A typed block's opening line as pmdoc's ParseTypedDirectiveOpen frames it: three colons or
 * more, a name of letters, digits, `_` and `-`, then `{` at once, and a line that ends with `}`.
 * Its attributes are not read: a stored document holds no opener whose attributes pmdoc refuses.
 */
const TYPED_OPENER = /^(:{3,})([A-Za-z0-9_-]+)\{.*\}$/s;

// goldmark's default block parsers and Dispatch's own, in goldmark's order of priority: setext
// heading (100), thematic break (200), list (300), list item (400), indented code (500), ATX
// heading (600), fenced code (700), blockquote (800), Dispatch's refused directive (900) and typed
// block (950), footnote definition (999), paragraph (1000).
const SCAN_PARSERS: readonly ScanParser[] = [
  {
    triggers: "-=",
    interrupts: true,
    indented: false,
    open(scan, parent, line, _width, at) {
      const last = scan.opened.at(-1);
      if (last?.kind !== "paragraph" || last.parent !== parent) return undefined;
      return setextUnderline(line.text.slice(at)) ? leafAt(parent, line) : undefined;
    },
  },
  {
    triggers: "-*_",
    interrupts: true,
    indented: false,
    open: (_scan, parent, line, _width, at) =>
      thematicBreak(line.text.slice(at)) ? leafAt(parent, line) : undefined,
  },
  {
    triggers: "-+*0123456789",
    interrupts: true,
    indented: false,
    open(scan, parent, line) {
      const last = scan.opened.at(-1);
      if (last?.kind === "list" || scan.skipList) {
        scan.skipList = false;
        return undefined;
      }
      const marker = listMarker(line);
      if (marker === undefined || frontMatterHolds(scan, parent)) return undefined;
      // An empty item, or one numbered other than a lone `1`, opens no list where it would
      // interrupt a paragraph or an indented code block (pmdoc's emptyItemGuard, stricter than
      // goldmark, which lets `01.` interrupt a paragraph and anything interrupt code).
      if (marker.empty || (marker.number !== undefined && marker.number !== "1")) {
        if (last?.kind === "paragraph" && last.parent === parent) return undefined;
        if (interruptsOpenBlock(scan, parent)) return undefined;
      }
      scan.emptyItemWithBlankLines = false;
      return { block: { kind: "list", marker: marker.char, ...under(parent) }, children: true };
    },
  },
  {
    triggers: "-+*0123456789",
    interrupts: true,
    indented: false,
    open(scan, parent, line) {
      if (parent.kind !== "list") return undefined;
      const previous = parent.last?.kind === "item" ? parent.last.offset : 0;
      const marker = listMarker(line);
      if (marker === undefined || marker.indent - previous > 3) return undefined;
      scan.emptyItemWithBlankLines = false;
      // The content's columns past the marker; one, when the item opens with indented code.
      const after = line.spacesAt(marker.end);
      const spacing = marker.empty || after > 4 ? 1 : after;
      const offset = line.columnAt(marker.end) - line.column + spacing;
      const item: ScanBlock = { kind: "item", offset, ...under(parent) };
      if (marker.empty) return { block: item, children: false };
      line.advanceTo(marker.end);
      line.skip(spacing);
      return { block: item, children: true };
    },
  },
  {
    interrupts: false,
    indented: true,
    open(scan, parent, line, width) {
      if (width < 4) return undefined;
      line.skip(4);
      const previous = scan.lines[scan.index - 1];
      const block: ScanBlock = {
        kind: "code",
        lastLine: scan.index,
        blankBefore: previous === undefined || /^[ \t]*$/.test(previous),
        ...under(parent),
      };
      return { block, children: false };
    },
  },
  {
    triggers: "#",
    interrupts: true,
    indented: false,
    open: (_scan, parent, line, _width, at) =>
      /^#{1,6}(?:[ \t]|$)/.test(line.text.slice(at)) ? leafAt(parent, line) : undefined,
  },
  {
    triggers: "`~",
    interrupts: true,
    indented: false,
    open(_scan, parent, line, _width, at) {
      const char = line.text.charAt(at);
      const length = runOf(line.text, at, char);
      if (length < 3) return undefined;
      // A backtick fence's info string holds no backtick: such a line is inline code.
      if (char === "`" && line.text.slice(at + length).includes("`")) return undefined;
      line.advanceTo(line.text.length);
      return { block: { kind: "fence", char, length, ...under(parent) }, children: false };
    },
  },
  {
    triggers: ">",
    interrupts: true,
    indented: false,
    open: (scan, parent, line) =>
      !frontMatterHolds(scan, parent) && quoteMarker(line)
        ? { block: { kind: "quote", ...under(parent) }, children: true }
        : undefined,
  },
  {
    triggers: ":",
    interrupts: true,
    indented: false,
    open: (_scan, parent, line, _width, at) =>
      refusedDirective(line.text.slice(at)) ? leafAt(parent, line) : undefined,
  },
  {
    triggers: ":",
    interrupts: true,
    indented: false,
    open(scan, parent, line, width, at) {
      const text = line.text.slice(at);
      const opener = TYPED_OPENER.exec(text);
      if (opener === null) return undefined;
      if (opener[2] === "ask") scan.openers.push(text);
      line.advanceTo(line.text.length);
      const block: ScanBlock = {
        kind: "typed",
        fence: opener[1].length,
        indent: width,
        ...under(parent),
      };
      return { block, children: true };
    },
  },
  {
    triggers: "[",
    interrupts: true,
    indented: false,
    open(scan, parent, line, _width, at) {
      const label = footnoteLabelEnd(line.text, at);
      if (label === undefined || frontMatterHolds(scan, parent)) return undefined;
      // Every space and tab after the label's `]:` belongs to the definition's prefix, so a block
      // written on the same line starts at column zero inside it (pmdoc's
      // footnoteDefinitionParser.Open). A line holding nothing but whitespace after the label
      // keeps it.
      const content = line.text.slice(label).search(/[^ \t]/);
      line.advanceTo(content < 0 ? label : label + content);
      return { block: { kind: "footnote", ...under(parent) }, children: true };
    },
  },
  {
    interrupts: false,
    indented: false,
    open: (scan, parent, line) => {
      const block: ScanBlock = {
        kind: "paragraph",
        lastLine: scan.index,
        table: false,
        ...under(parent),
      };
      line.advanceTo(line.text.length);
      return { block, children: false };
    },
  },
];

/**
 * pmdoc's interruptsOpenBlock: whether a list opening in `parent` interrupts a block the browser
 * editor's parser holds open there, other than a paragraph `parent` holds: a paragraph that ends on
 * the line before, reached across the containers the line opens first and forming no table, or an
 * indented code block with only blank lines after it, unless that code stands right after a list,
 * or right after a quote with no blank line between them.
 */
function interruptsOpenBlock(scan: OpenerScan, parent: ScanBlock): boolean {
  let previous = parent.last;
  const opensContainers = previous === undefined;
  let node: ScanBlock | undefined = parent;
  while (previous === undefined && node !== undefined && node.kind !== "document") {
    previous = node.previous;
    node = node.parent;
  }
  if (previous?.kind === "paragraph") {
    return opensContainers && !previous.table && scan.index - previous.lastLine <= 1;
  }
  if (previous?.kind !== "code") return false;
  if (previous.previous?.kind === "list") return false;
  if (previous.previous?.kind === "quote" && !previous.blankBefore) return false;
  return scan.lines.slice(previous.lastLine + 1, scan.index).every((row) => /^[ \t>]*$/.test(row));
}

/** A table's delimiter row, which makes the paragraph above it a table (GFM's tables). */
const TABLE_DELIMITER_ROW =
  /^(?=.*\|)[ \t]*\|?[ \t]*:?-+:?[ \t]*(?:\|[ \t]*:?-+:?[ \t]*)*\|?[ \t]*$/;

/** goldmark's `Continue` of each block the scan holds open. */
function continueScanBlock(scan: OpenerScan, block: ScanBlock, line: ScanLine): Continuation {
  switch (block.kind) {
    case "quote":
      return quoteMarker(line) ? "children" : "close";
    case "list":
      return continueList(scan, block, line);
    case "item":
      return continueItem(scan, block, line);
    case "footnote": {
      if (line.blank()) return "children";
      const { width } = line.indent();
      if (width < 4) return "close";
      line.skip(4);
      return "children";
    }
    case "typed": {
      const { width, at } = line.indent();
      // A line of at least the fence's colons closes the block, whatever block inside it the line
      // would otherwise continue (directive.go, typedDirectiveParser.Continue).
      if (width < 4 && at < line.text.length && fenceColons(line.text.slice(at)) >= block.fence) {
        line.advanceTo(line.text.length);
        return "close";
      }
      if (at < line.text.length) line.skip(Math.min(width, block.indent));
      return "children";
    }
    case "fence": {
      const { width, at } = line.indent();
      if (width < 4) {
        const length = runOf(line.text, at, block.char);
        if (length >= block.length && /^[ \t]*$/.test(line.text.slice(at + length))) {
          line.advanceTo(line.text.length);
          return "close";
        }
      }
      line.advanceTo(line.text.length);
      return "continue";
    }
    case "code": {
      if (line.blank()) return "continue";
      if (line.indent().width < 4) return "close";
      block.lastLine = scan.index;
      line.advanceTo(line.text.length);
      return "continue";
    }
    default:
      // A heading, a rule or a refused directive holds one line; the document never closes.
      return block.kind === "document" ? "children" : "close";
  }
}

/** goldmark's `listParser.Continue`. */
function continueList(
  scan: OpenerScan,
  list: ScanBlock & { kind: "list" },
  line: ScanLine
): Continuation {
  const last = list.last;
  const lastIsEmpty = last === undefined || last.children === 0;
  if (line.blank()) {
    if (lastIsEmpty) scan.emptyItemWithBlankLines = true;
    return "children";
  }
  const offset = last?.kind === "item" ? last.offset : 0;
  const { width, at } = line.indent();
  if (width < offset || lastIsEmpty) {
    if (width < 4) {
      const marker = listMarker(line);
      if (marker !== undefined && marker.indent - offset < 4) {
        if (marker.char !== list.marker) return "close";
        // A thematic break outranks a list item, unless it underlines a paragraph as a heading.
        const fromMarker = line.text.slice(at);
        const heading = scan.opened.at(-1)?.kind === "paragraph" && setextUnderline(fromMarker);
        if (thematicBreak(fromMarker) && !heading) return "close";
        return "children";
      }
    }
    if (!lastIsEmpty) return "close";
  }
  if (lastIsEmpty && width < offset) return "close";
  // Content cannot follow an empty item and blank lines in the same list.
  if (scan.emptyItemWithBlankLines) return "close";
  return "children";
}

/** goldmark's `listItemParser.Continue`, a blank line taking no more than the item's columns. */
function continueItem(
  scan: OpenerScan,
  item: ScanBlock & { kind: "item" },
  line: ScanLine
): Continuation {
  const { width } = line.indent();
  if (line.blank()) {
    line.skip(Math.min(width, item.offset));
    return "children";
  }
  const isEmpty = item.children === 0 && scan.emptyItemWithBlankLines;
  if ((isEmpty || width < item.offset) && width < 4) {
    if (listMarker(line) !== undefined) {
      scan.skipList = true;
      return "close";
    }
    if (!isEmpty) return "close";
  }
  line.skip(Math.min(width, item.offset));
  return "children";
}

/** A list item's marker opening what is left of a line. */
interface ListMarker {
  /** The columns of indentation before it. */
  readonly indent: number;
  /** The index after it. */
  readonly end: number;
  /** The character a list continues only with: the bullet, or the ordered delimiter. */
  readonly char: string;
  /** An ordered marker's number as written. */
  readonly number: string | undefined;
  /** Whether nothing but spaces and tabs follows it. */
  readonly empty: boolean;
}

/** goldmark's `parseListItem`, indentation measured in the columns its tabs span (pmdoc's tabIndented). */
function listMarker(line: ScanLine): ListMarker | undefined {
  const { width, at } = line.indent();
  if (width > 3) return undefined;
  const { text } = line;
  const first = text.charAt(at);
  let end = at;
  let number: string | undefined;
  if (first === "-" || first === "*" || first === "+") {
    end = at + 1;
  } else {
    while (end < text.length && end - at <= 9 && /[0-9]/.test(text.charAt(end))) end++;
    const delimiter = text.charAt(end);
    if (end === at || end - at > 9 || (delimiter !== "." && delimiter !== ")")) return undefined;
    number = text.slice(at, end);
    end++;
  }
  const after = text.charAt(end);
  if (end < text.length && after !== " " && after !== "\t") return undefined;
  return {
    indent: width,
    end,
    char: text.charAt(end - 1),
    number,
    empty: /^[ \t]*$/.test(text.slice(end)),
  };
}

/** goldmark's blockquote `process`: a `>`, then one column of the space or tab after it. */
function quoteMarker(line: ScanLine): boolean {
  const { width, at } = line.indent();
  if (width > 3 || line.text.charAt(at) !== ">") return false;
  line.advanceTo(at + 1);
  const next = line.text.charAt(at + 1);
  if (next === " " || next === "\t") line.skip(1);
  return true;
}

/** An ASCII punctuation character, which a backslash before it escapes (goldmark's IsPunct). */
const ASCII_PUNCTUATION = /^[\x21-\x2f\x3a-\x40\x5b-\x60\x7b-\x7e]$/;

/**
 * The index after the `]:` of the footnote label at index `at` of `text`, as goldmark's footnote
 * parser reads one (extension/footnote.go, through util.FindClosure): `[^`, the label, `]`, then
 * `:` at once. A backslash before an ASCII punctuation character escapes that character, so `\]`
 * and `\[` are part of the label. An unescaped `[` makes the line no label, and so does a label
 * of nothing but spaces and tabs. Undefined where no label stands at `at`.
 */
function footnoteLabelEnd(text: string, at: number): number | undefined {
  if (!text.startsWith("[^", at)) return undefined;
  let close = at + 2;
  for (; close < text.length && text[close] !== "]"; close++) {
    if (text[close] === "[") return undefined;
    if (text[close] === "\\" && ASCII_PUNCTUATION.test(text.charAt(close + 1))) close++;
  }
  if (text.charAt(close + 1) !== ":" || /^[ \t\r]*$/.test(text.slice(at + 2, close))) {
    return undefined;
  }
  return close + 2;
}

/**
 * pmdoc's unsupportedDirectiveReason: a line of directive syntax that the server refuses, which
 * opens a block of one line. A stored document holds none.
 */
function refusedDirective(text: string): boolean {
  if (text.startsWith(":::")) return text !== ":::" && !TYPED_OPENER.test(text);
  if (text.startsWith("::")) return /^::[A-Za-z0-9_-]/.test(text);
  return /^:[A-Za-z0-9_-]/.test(text) && text.includes("{");
}

/** goldmark's `isThematicBreak`: three or more of one of `*`, `-`, `_`, with spaces between. */
function thematicBreak(text: string): boolean {
  const marks = text.replace(/[ \t]/g, "");
  return marks.length >= 3 && /^(?:\*+|-+|_+)$/.test(marks);
}

/** goldmark's `matchesSetextHeadingBar`: a run of `=` or of `-`, then spaces. */
function setextUnderline(text: string): boolean {
  return /^(?:=+|-+)[ \t]*$/.test(text);
}

/** directive.go's `fenceColons`: the colons `text` holds when they are all it holds but spaces. */
function fenceColons(text: string): number {
  const trimmed = text.replace(/^[ \t]+|[ \t]+$/g, "");
  return /^:+$/.test(trimmed) ? trimmed.length : 0;
}

/** How many times `char` stands in a row in `text` from `index`. */
function runOf(text: string, index: number, char: string): number {
  let end = index;
  while (text.charAt(end) === char) end++;
  return end - index;
}

/** Each typed block id and how its opening line writes it (`typedBlockIdAttribute`). */
export const DISPATCH_TYPED_BLOCK_ID_SPELLINGS: readonly { id: string; written: string }[] = [
  { id: "callout-1", written: "#callout-1" },
  { id: "q:1", written: "#q:1" },
  { id: "décision", written: "#décision" },
  { id: "à", written: "#à" },
  { id: "a{b", written: "#a{b" },
  { id: "a\\b", written: "#a\\b" },
  { id: "a\u00a0b", written: "#a\u00a0b" },
  { id: "a\u2028b", written: "#a\u2028b" },
  { id: "a\u3000b", written: "#a\u3000b" },
  { id: "a\uFFFDb", written: "#a\uFFFDb" },
  { id: "a b", written: 'id="a b"' },
  { id: " a ", written: 'id=" a "' },
  { id: "a\tb", written: 'id="a\tb"' },
  { id: "a.b", written: 'id="a.b"' },
  { id: "a#b", written: 'id="a#b"' },
  { id: "a}b", written: 'id="a}b"' },
  { id: "a'b", written: `id="a'b"` },
  { id: "a<b", written: 'id="a<b"' },
  { id: "a=b", written: 'id="a=b"' },
  { id: "a>b", written: 'id="a>b"' },
  { id: "a`b", written: 'id="a`b"' },
  { id: "a<b>=`c`", written: 'id="a<b>=`c`"' },
  { id: 'a"b', written: 'id="a&#x22;b"' },
  { id: 'a\\"b', written: 'id="a\\&#x22;b"' },
  { id: "R&D", written: 'id="R&amp;D"' },
  { id: "&amp;", written: 'id="&amp;amp;"' },
  { id: "x&amp;y", written: 'id="x&amp;amp;y"' },
  { id: "x\ny", written: 'id="x&#xA;y"' },
  { id: "x\ry", written: 'id="x&#xD;y"' },
  { id: "x\r\ny", written: 'id="x&#xD;&#xA;y"' },
  { id: "", written: 'id=""' },
];
