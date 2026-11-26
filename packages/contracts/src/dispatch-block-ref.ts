import type { BlockItemKind } from "./block-schema.generated";
import { readAskBlocks } from "./dispatch-block-reader";
import { hasControlCharacter } from "./dispatch-href";
import { referenceTargetFits, utf8Bytes } from "./dispatch-text-rules";
import { PROJECT_KEY_PATTERN } from "./dispatch-tools";

const encoder = new TextEncoder();

/** A block id of ASCII letters, digits and `-` only, the empty one included: `encodeBlockId`
 *  writes it as it is. */
const PLAIN_BLOCK_ID = /^[A-Za-z0-9-]*$/u;

/** `byte` percent-encoded in upper-case hex. */
function percentEncoded(byte: number): string {
  return `%${byte.toString(16).toUpperCase().padStart(2, "0")}`;
}

/**
 * The `#b-` fragment of a block reference: every UTF-8 byte of the block id percent-encoded in
 * upper-case hex except ASCII letters, digits and `-`, so no character a reference ends at or the
 * grammar trims (`'`, `.`, `!`, `*`, `_`, `~`, `)`), and no `@` a pin (`@v<N>` after the
 * fragment) could be read from, is ever written bare. Readers accept any percent-encoding; every
 * writer writes this one. Go's twin is `text.EncodeBlockID`; both are tested against
 * DISPATCH_BLOCK_ID_ENCODINGS, whose JSON copy is
 * `packages/envoy/internal/dispatch/text/testdata/dispatch-block-id-encodings.json`. Each code
 * point is encoded on its own: Bun 1.3's `encode` writes a non-ASCII Latin-1 character of a string
 * past 1024 characters as a NUL, and an id has no length bound. An id of plain characters
 * (`PLAIN_BLOCK_ID`) is its own encoding, and an ASCII character is its own one UTF-8 byte, so
 * only a code point past ASCII goes through the encoder: a page encodes every block id it links.
 */
export function encodeBlockId(id: string): string {
  if (PLAIN_BLOCK_ID.test(id)) return id;
  let encoded = "";
  for (const character of id) {
    const code = character.charCodeAt(0);
    if (code >= 0x80) {
      for (const byte of encoder.encode(character)) encoded += percentEncoded(byte);
    } else {
      const bare =
        (code >= 0x30 && code <= 0x39) ||
        (code >= 0x41 && code <= 0x5a) ||
        (code >= 0x61 && code <= 0x7a) ||
        code === 0x2d;
      encoded += bare ? character : percentEncoded(code);
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

/**
 * A typed block's opening line as pmdoc's ParseTypedDirectiveOpen frames it: three colons or
 * more, a name of letters, digits, `_` and `-`, then `{` at once, and a line that ends with `}`.
 * Its attributes are not read: a stored document holds no opener whose attributes pmdoc refuses.
 */
const TYPED_OPENER = /^(:{3,})([A-Za-z0-9_-]+)\{.*\}$/s;

/**
 * Each line of `markdown` that opens an ask block, from the opener's first colon on, as pmdoc's
 * parser reads the canonical rendering a text route serves: a typed block named `ask` (three
 * colons or more, `ask{`, and a line that ends with `}`, as TYPED_OPENER frames it), outside
 * fenced and indented code and outside the front matter a document opens with. It reads the
 * document's blocks with readAskBlocks (dispatch-block-reader.ts), the one reader of a document's
 * blocks, which follows goldmark and pmdoc. packages/envoy-client's askgate-new-asks.ts reads its
 * ask blocks with the same reader.
 *
 * The web's reader of a live text of the release before, and the CLI's reader of a live text and
 * its check of a version for open ask blocks before an approval request, all read openers here.
 * So none takes a line that a document's writer quoted for a state or an answer that Dispatch
 * recorded. Go's twin is refs.questionBlocks.
 */
export function askOpenerLines(markdown: string): string[] {
  const lines = markdown.split("\n").map((line) => line.replace(/\r$/, ""));
  const typedOpener = (text: string) => {
    // Both groups are in each match, so they are undefined only where `text` does not match.
    const [, colons, name] = TYPED_OPENER.exec(text) ?? [];
    return colons === undefined || name === undefined ? undefined : { colons: colons.length, name };
  };
  return readAskBlocks(lines, { frontMatter: true, bodies: false, typedOpener }).map(
    (block) => block.opener
  );
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
    while (
      end < attributes.length &&
      !TYPED_BLOCK_ID_SHORTCUT_EXCLUDED.includes(attributes.charAt(end))
    ) {
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

/** The named character references an opener's quoted value reads back, each with its character. */
// kept for one release: release-before-text
const NAMED_ATTRIBUTE_REFERENCES: Readonly<Record<string, string>> = {
  amp: "&",
  apos: "'",
  gt: ">",
  lt: "<",
  quot: '"',
};

/**
 * A quoted attribute value with its character references read back, in one pass, so a character
 * a reference decodes to starts no reference of its own (`&amp;#x22;` is `&#x22;`): every numeric
 * reference, hexadecimal or decimal (`&#x22;`, `&#xA;`, `&#xD;`, `&#34;`), and the five named
 * ones (NAMED_ATTRIBUTE_REFERENCES). Any other named reference stays as written. A numeric
 * reference that names no character - zero, a surrogate, one past U+10FFFF - is U+FFFD, as it is
 * to Go's `html.UnescapeString`, which pmdoc's directive reader decodes an attribute with
 * (`pmdoc.ParseTypedDirectiveOpen`). Those are the references the renderer writes and a
 * browser's parser reads back; Go's reader holds the whole HTML named-reference table besides,
 * which no renderer writes into an opener.
 */
// kept for one release: release-before-text
function openerAttributeValue(raw: string): string {
  return raw.replace(
    /&(?:#([xX][0-9a-fA-F]+|[0-9]+)|(amp|apos|gt|lt|quot));/g,
    (whole: string, number: string | undefined, name: string | undefined) => {
      if (number === undefined) return NAMED_ATTRIBUTE_REFERENCES[name ?? ""] ?? whole;
      const codePoint =
        number.startsWith("x") || number.startsWith("X")
          ? Number.parseInt(number.slice(1), 16)
          : Number.parseInt(number, 10);
      return codePoint === 0 || codePoint > 0x10ffff || (codePoint >= 0xd800 && codePoint <= 0xdfff)
        ? "\uFFFD"
        : String.fromCodePoint(codePoint);
    }
  );
}

/**
 * The quoted attributes `line`, an ask block's opener as `askOpenerLines` gives it, carries, each
 * name with its value read back (`openerAttributeValue`): what a Dispatch of the release before
 * writes on an opener from the store, `state`, `answered_by`, `answered_at`, `selected`,
 * `answer`, `copied_from` and `copied_from_document`. A name written twice keeps its last value.
 * Empty where the line has no braces. The block's id is read by `askOpenerBlockId`. The web's
 * reader of a live text of the release before and the CLI's read opener attributes here, so the
 * two never read one opener differently.
 */
// kept for one release: release-before-text
export function askOpenerAttributes(line: string): ReadonlyMap<string, string> {
  const values = new Map<string, string>();
  const start = line.indexOf("{");
  const end = line.lastIndexOf("}");
  if (start === -1 || end <= start) return values;
  for (const [, name = "", raw = ""] of line
    .slice(start + 1, end)
    .matchAll(/([\w-]+)="([^"]*)"/g)) {
    values.set(name, openerAttributeValue(raw));
  }
  return values;
}

/** The lines of `openers`, a version's ask block openers as `askOpenerLines` gives them, that
 *  open the ask block `blockId`: the lines whose id (`askOpenerBlockId`) is `blockId`. The web's
 *  and the CLI's readers of one block's opening line both take it here. */
export function askOpeners(openers: readonly string[], blockId: string): string[] {
  return openers.filter((line) => askOpenerBlockId(line) === blockId);
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
