// The attributes of a typed block's opening line, the text between its `{` and the `}` that ends
// the line, read as the Dispatch server reads them: pmdoc's parseDirectiveAttributes and
// quotedTypedBlockID (packages/envoy/internal/dispatch/pmdoc/directive.go). The server keeps one
// reader of this grammar, and this module is this package's one copy of it. The one reader of the
// frame around them, askgate-typed-opener.ts, reads them with it, for the ask gate
// (askgate-new-asks.ts) and the stored-spelling check (dispatch-commands/stored-spelling.ts)
// alike, so the two read a line alike, and alike with the server.

import { TYPED_BLOCK_ID_SHORTCUT_EXCLUDED } from "@legion/contracts";

/** directive.go's `directiveNameByte`: a letter, a digit, `_` or `-`. */
export function directiveNameChar(char: string): boolean {
  return /^[A-Za-z0-9_-]$/.test(char);
}

/**
 * The whitespace Go's `unicode.IsSpace` finds in a byte of the attributes: ASCII's. Go also
 * reads the bytes 0x85 and 0xA0 as space, but those are only ever the second byte of a character
 * in UTF-8, and the reader never stands on one.
 */
function attributeSpace(char: string): boolean {
  return char === " " || (char >= "\t" && char <= "\r");
}

/** The key that holds a block's id among a directive's attributes (pmdoc's `BlockIDAttr`). */
const BLOCK_ID_ATTRIBUTE = "blockId";

/** One attribute as it is written (`#id`, `.class`, `name` or `name="value"`), under the name
 *  Dispatch reads it by: `id` for `#…` and `id="…"`, `class` for `.…`, and its key otherwise. */
export interface DirectiveAttribute {
  readonly name: string;
  readonly text: string;
}

/** The attributes of a typed block's opening line, as directiveAttributes reads them. */
export interface DirectiveAttributes {
  /** Each attribute as written, in the order the line writes them. */
  readonly attributes: readonly DirectiveAttribute[];
  /** The whitespace around the attributes, as written: one more than the attributes, the first
   *  before the first attribute and the last after the last one. */
  readonly gaps: readonly string[];
  /** The block's id: what follows `#`, or the value of `id="…"` decoded (decodedId). Undefined
   *  when the line names no id, and when this reader cannot tell what Go decodes. */
  readonly id: string | undefined;
}

/**
 * directive.go's `parseDirectiveAttributes`: the text between a typed block's `{` and the `}` that
 * ends its line, read as the server reads it, or undefined where the server refuses it. The id is
 * written `#id`, which runs to the first character of TYPED_BLOCK_ID_SHORTCUT_EXCLUDED
 * (@legion/contracts, pmdoc's `typedBlockIDShortcutExcluded`), or `id="…"`, which runs to the next
 * quote, is decoded (decodedId) and may not be empty; a block has one id at most. A class is
 * written `.class`, and a block may have more than one. Any other attribute is a key of name
 * characters, alone (an empty value) or followed by `="…"`, and stands once. Every quoted value
 * runs to the next quote, whatever stands before it, so a space inside it splits no attribute,
 * and a backslash escapes nothing: `\"` ends the value. A value may hold any other character,
 * U+2028 and U+2029 among them, which Dispatch writes unescaped. `blockId`, the tree's name for
 * the id, is no attribute of the markdown, and the server refuses it. Each attribute ends at
 * whitespace or at the end of the text. One pass reads the text, so no text costs more than its
 * length.
 */
export function directiveAttributes(value: string): DirectiveAttributes | undefined {
  const attributes: DirectiveAttribute[] = [];
  const gaps: string[] = [];
  const named = new Set<string>();
  let hasId = false;
  let id: string | undefined;
  let offset = 0;
  for (;;) {
    const gap = offset;
    while (offset < value.length && attributeSpace(value.charAt(offset))) offset++;
    gaps.push(value.slice(gap, offset));
    if (offset === value.length) break;
    const start = offset;
    const sigil = value.charAt(start);
    let name: string;
    if (sigil === "#") {
      offset = start + 1;
      while (
        offset < value.length &&
        !TYPED_BLOCK_ID_SHORTCUT_EXCLUDED.includes(value.charAt(offset))
      )
        offset++;
      if (offset === start + 1 || hasId) return undefined;
      hasId = true;
      id = value.slice(start + 1, offset);
      name = "id";
    } else if (sigil === ".") {
      offset = start + 1;
      while (offset < value.length && directiveNameChar(value.charAt(offset))) offset++;
      if (offset === start + 1) return undefined;
      named.add("class");
      name = "class";
    } else {
      while (offset < value.length && directiveNameChar(value.charAt(offset))) offset++;
      name = value.slice(start, offset);
      if (name === "") return undefined;
      if (name === "id") {
        const close = value.startsWith('="', offset) ? value.indexOf('"', offset + 2) : -1;
        if (close === -1 || close === offset + 2 || hasId) return undefined;
        hasId = true;
        id = decodedId(value.slice(offset + 2, close));
        offset = close + 1;
      } else {
        if (name === BLOCK_ID_ATTRIBUTE || named.has(name)) return undefined;
        named.add(name);
        // A name alone is an empty value. The browser editor reads a name up to a space, so a
        // tab after one is part of it, and the server refuses the line.
        if (offset < value.length && value.charAt(offset) !== " ") {
          const close = value.startsWith('="', offset) ? value.indexOf('"', offset + 2) : -1;
          if (close === -1) return undefined;
          offset = close + 1;
        }
      }
    }
    if (offset < value.length && !attributeSpace(value.charAt(offset))) return undefined;
    attributes.push({ name, text: value.slice(start, offset) });
  }
  return { attributes, gaps, id };
}

/**
 * The named character references decodedId knows, each as Go's `html` table holds it: with its
 * `;`, and, for each but `apos`, without it too.
 */
const NAMED_REFERENCES: ReadonlyMap<string, string> = new Map([
  ["amp;", "&"],
  ["amp", "&"],
  ["lt;", "<"],
  ["lt", "<"],
  ["gt;", ">"],
  ["gt", ">"],
  ["quot;", '"'],
  ["quot", '"'],
  ["apos;", "'"],
  ["nbsp;", "\u00a0"],
  ["nbsp", "\u00a0"],
]);

/** Go's `html` replacementTable: the characters a numeric reference to 0x80-0x9F decodes to. */
const WINDOWS_1252 =
  "\u20ac\u0081\u201a\u0192\u201e\u2026\u2020\u2021\u02c6\u2030\u0160\u2039\u0152\u008d\u017d\u008f" +
  "\u0090\u2018\u2019\u201c\u201d\u2022\u2013\u2014\u02dc\u2122\u0161\u203a\u0153\u009d\u017e\u0178";

/**
 * The id an `id="…"` value names, decoded as Go's `html.UnescapeString` decodes it (its
 * `unescapeEntity`), or undefined where this reader cannot tell what Go decodes. An id Dispatch
 * writes (`TypedBlockIDAttribute`, @legion/contracts' typedBlockIdAttribute) holds its `&`, `"`,
 * line feeds and carriage returns as `&amp;`, `&#x22;`, `&#xA;` and `&#xD;`, and this reads each
 * of them back.
 *
 * A numeric reference is read as Go reads it, its quirks included: it ends at its first character
 * that is no digit, and a `;` there is taken with it; one decimal digit with no `;` after it, and
 * `&#` with one character after it at the value's end, stay as written; a number past 32 bits
 * wraps as Go's rune does; 0x80 to 0x9F decode to Windows-1252's characters (WINDOWS_1252); and 0,
 * a surrogate, a negative number or one past U+10FFFF decode to U+FFFD.
 *
 * An `&` with no letter or digit after it, one followed by a single letter or digit, and one
 * followed by a digit first stay as written, as no name in Go's table is one of those. Of the
 * named references, only NAMED_REFERENCES are decoded. Any other `&` followed by letters makes
 * the value read as no id. The limit is on purpose: Go's table holds about 2,230 names, and Go
 * decodes a name it holds at the start of a longer one too, so only the whole table tells a name
 * Go decodes from text it keeps as written, and this package keeps no copy of it. An ask whose id
 * is not read is still reported, without its id.
 */
function decodedId(raw: string): string | undefined {
  let id = "";
  let at = 0;
  for (let amp = raw.indexOf("&"); amp !== -1; amp = raw.indexOf("&", at)) {
    id += raw.slice(at, amp);
    const s = raw.slice(amp);
    let i = 1;
    if (s.charAt(1) === "#") {
      const hex = s.charAt(2) === "x" || s.charAt(2) === "X";
      let x = 0;
      i = hex ? 3 : 2;
      while (s.length > 3 && i < s.length) {
        const c = s.charAt(i++);
        const digit = (hex ? /^[0-9A-Fa-f]$/ : /^[0-9]$/).test(c) ? Number.parseInt(c, 16) : -1;
        if (digit >= 0) {
          x = (Math.imul(x, hex ? 16 : 10) + digit) | 0;
          continue;
        }
        if (c !== ";") i--;
        break;
      }
      if (s.length <= 3 || i <= 3) {
        id += "&";
        i = 1;
      } else if (x >= 0x80 && x <= 0x9f) {
        id += WINDOWS_1252.charAt(x - 0x80);
      } else if (x <= 0 || (x >= 0xd800 && x <= 0xdfff) || x > 0x10ffff) {
        id += "\ufffd";
      } else {
        id += String.fromCodePoint(x);
      }
    } else {
      while (i < s.length && /^[A-Za-z0-9]$/.test(s.charAt(i))) i++;
      if (s.charAt(i) === ";") i++;
      const name = s.slice(1, i);
      const decoded = NAMED_REFERENCES.get(name);
      if (decoded !== undefined) id += decoded;
      else if (name.replace(/;$/, "").length > 1 && /^[A-Za-z]/.test(name)) return undefined;
      else id += s.slice(0, i);
    }
    at = amp + i;
  }
  return id + raw.slice(at);
}
