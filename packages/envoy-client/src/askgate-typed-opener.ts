// A typed block's opening line, read as the Dispatch server reads it: pmdoc's
// ParseTypedDirectiveOpen and unsupportedDirectiveReason
// (packages/envoy/internal/dispatch/pmdoc/directive.go). This module is this package's one reader
// of the frame of that line (the fence, the name, the `{` and the `}` that ends the line), and
// directive-attributes.ts its one reader of the attributes between the two braces. The ask gate
// (askgate-new-asks.ts) reads a line here to open an ask block only where the server opens one,
// and to name it by the id the server reads. The stored-spelling check
// (dispatch-commands/stored-spelling.ts) reads a line here to find each attribute as written. So
// each rule here is the server's, and a line the server refuses opens nothing.

import {
  type DirectiveAttributes,
  directiveAttributes,
  directiveNameChar,
} from "./directive-attributes";

/** How many times `char` stands in a row in `text` from `index`. */
export function runOf(text: string, index: number, char: string): number {
  let end = index;
  while (text.charAt(end) === char) end++;
  return end - index;
}

/** A typed block's opening line as the server reads it (typedOpenerAt): the count of its fence's
 *  colons, the block's name, and the attributes between its `{` and the `}` that ends the line. */
export interface TypedOpener extends DirectiveAttributes {
  readonly name: string;
  readonly colons: number;
}

/**
 * The index of the `{` of a typed block's fence that stands at index `at` of `text`: three colons
 * or more, a name of directive.go's name characters (directiveNameChar), then `{` at once. -1
 * where no such fence stands at `at`. It reads no more than the colons and the name.
 */
export function typedFenceBrace(text: string, at: number): number {
  const nameStart = at + runOf(text, at, ":");
  if (nameStart - at < 3) return -1;
  let brace = nameStart;
  while (directiveNameChar(text.charAt(brace))) brace++;
  return brace > nameStart && text.charAt(brace) === "{" ? brace : -1;
}

/**
 * directive.go's `ParseTypedDirectiveOpen`, read from index `at` of `text`: the fence there
 * (typedFenceBrace), and the line ends with `}`, the attributes between the `{` and that `}` read
 * as the server reads them (directiveAttributes). Undefined where no fence stands at `at`, where
 * the line does not end with `}`, and where the server refuses the attributes.
 */
export function typedOpenerAt(text: string, at: number): TypedOpener | undefined {
  const brace = typedFenceBrace(text, at);
  // The `{` is no `}`, so the `}` that ends the line stands after it.
  if (brace === -1 || !text.endsWith("}")) return undefined;
  const attributes = directiveAttributes(text.slice(brace + 1, -1));
  if (attributes === undefined) return undefined;
  const colons = runOf(text, at, ":");
  return { ...attributes, name: text.slice(at + colons, brace), colons };
}

/** `text` read as a typed block's opening line from its start (typedOpenerAt). */
export function typedOpener(text: string): TypedOpener | undefined {
  return typedOpenerAt(text, 0);
}

/** directive.go's `unsupportedDirectiveReason`: a line of directive syntax the server refuses. */
export function refusedDirective(text: string): boolean {
  if (text.startsWith(":::")) return text !== ":::" && typedOpener(text) === undefined;
  if (text.startsWith("::")) return text.length > 2 && directiveNameChar(text.charAt(2));
  return text.length > 1 && directiveNameChar(text.charAt(1)) && text.includes("{");
}
