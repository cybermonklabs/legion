// A typed block's opening line, read as the Dispatch server reads it: pmdoc's
// ParseTypedDirectiveOpen and unsupportedDirectiveReason
// (packages/envoy/internal/dispatch/pmdoc/directive.go), the attributes read by
// directive-attributes.ts. The ask gate (askgate-new-asks.ts) opens an ask block only where the
// server opens one, and names it by the id the server reads, so each rule here is the server's: a
// line the server refuses opens nothing.

import { directiveAttributes, directiveNameChar } from "./directive-attributes";

/** How many times `char` stands in a row in `text` from `index`. */
export function runOf(text: string, index: number, char: string): number {
  let end = index;
  while (text.charAt(end) === char) end++;
  return end - index;
}

/**
 * directive.go's `ParseTypedDirectiveOpen`: three colons or more, a name, then `{` at once, and
 * the line ends with `}`, the attributes between them read as the server reads them
 * (directiveAttributes). Undefined for any other line, and for one whose attributes the server
 * refuses.
 */
export function typedOpener(
  text: string
): { name: string; id: string | undefined; colons: number } | undefined {
  const colons = runOf(text, 0, ":");
  if (colons < 3) return undefined;
  const rest = text.slice(colons);
  let nameEnd = 0;
  while (nameEnd < rest.length && directiveNameChar(rest.charAt(nameEnd))) nameEnd++;
  if (nameEnd === 0 || rest.charAt(nameEnd) !== "{" || !rest.endsWith("}")) return undefined;
  const attributes = directiveAttributes(rest.slice(nameEnd + 1, -1));
  if (attributes === undefined) return undefined;
  return { name: rest.slice(0, nameEnd), id: attributes.id, colons };
}

/** directive.go's `unsupportedDirectiveReason`: a line of directive syntax the server refuses. */
export function refusedDirective(text: string): boolean {
  if (text.startsWith(":::")) return text !== ":::" && typedOpener(text) === undefined;
  if (text.startsWith("::")) return text.length > 2 && directiveNameChar(text.charAt(2));
  return text.length > 1 && directiveNameChar(text.charAt(1)) && text.includes("{");
}
