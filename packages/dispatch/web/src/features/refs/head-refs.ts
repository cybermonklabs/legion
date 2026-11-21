import { type CodeRange, codeRanges } from "./code-ranges";
import { type InlineLink, inlineLinks } from "./party-references";
import { referenceSpans } from "./reference-spans";
import { buildDispatchReference, referenceRouteFromHref } from "./routes";
import { readTextReferences } from "./text-references";

/** One reference a text writes, as a cut reads it (the server's `cutReference`): where a reader
 *  sees it, `[start, end)` in UTF-16 offsets - the whole inline link around it, where one is -
 *  and what it names, as the server's `text.Ref` does: an address's reference, a bare key's issue
 *  (which an address of the same issue names too) and a `#id` as written, which no address names.
 *  Two references at one place are the same where they name the same thing, whichever way each
 *  is written. */
interface Extent {
  readonly start: number;
  readonly end: number;
  readonly names: string;
}

/** The whitespace a cut that moved back drops from the end of its head, line breaks included. */
const TRAILING_SPACE = /[ \t\r\n]+$/u;

/** What a closed code span's content drops from its end before the backticks that close it: a
 *  backtick, which would join them into a longer run that closes nothing, and whitespace, which
 *  would leave them on a line of their own. */
const CLOSING_TRIM = /[` \t\r\n]+$/u;

/** A run of backticks, which opens or closes a code span of its width. */
const BACKTICK_RUN = /`+/gu;

/** The most characters past the bound a cut reads to keep a reference whole: the server's
 *  `longestWholeReferenceRunes` (`packages/envoy/internal/dispatch/text/runes.go`), so the two
 *  copies read the same window of a text and agree on a reference that runs past it. */
const LONGEST_WHOLE_REFERENCE = 1024;

/**
 * `text` cut to at most `limit` characters, `ellipsis` included, at a place no reference crosses:
 * the twin of the server's `text.HeadRefs` (`packages/envoy/internal/dispatch/text/runes.go`),
 * held to the same rows (`testdata/head-refs.json` there), for a head the dashboard cuts itself
 * and shows as its characters - a picker row's words, which the list draws as written until the
 * reader's engine answers and wherever the server sent no tree of them (`useRefPicker.ts`). A
 * cut by length alone would leave half of a reference there: `CORE-12` of `CORE-1234`, or
 * `[@Planner](dispatch://agent/` drawn raw. A text at or under the limit is returned unchanged;
 * one over it is cut to its first `limit` characters less the ellipsis's, and the ellipsis is
 * appended. A character is a code point, as the server counts runes.
 *
 * A reference is what the server's cut reads as one: a `dispatch://` address and a page of the
 * dashboard at `origin` as the shared span reader finds them, a URL of anywhere else left out,
 * and a bare issue key and a `#id` as the shared reader finds them (`readTextReferences`), each
 * widened over the inline link that holds it, a picture's `!` included (`inlineLinks`). A span
 * that is a link's address is that address as the link reads it, since the span would run on
 * over the `)` into whatever follows it: `dispatch://CORE-1)s` of `[CORE-1](dispatch://CORE-1)s`.
 * Where one starts before the cut and ends after it, the cut moves back to its start: the link's
 * `[` where a link wraps it. A code span the cut opens and does not close moves it back to its
 * opening backticks the same way, since the head would leave them as text and read what the span
 * holds - an address included - as prose. The cut moves back to a reference's start too where
 * the head, with the ellipsis after it, does not read the references of the text it holds as the
 * text does: one it reads that the text does not write (`CORE-12` of `CORE-12x4`, which is no
 * key), or one the text writes whole that the ellipsis joins (`dispatch://agent/s1` and `…` read
 * as the session `s1…`). A cut that moved drops the whitespace before it, save where the head
 * then ends after a reference: there it keeps the first whitespace character the text has after
 * that reference, so the ellipsis joins nothing and the walk stops - a line of short addresses
 * separated by spaces is cut after its last whole address and a space, not back past every one
 * of them. A cut never empties the head: where moving back would leave nothing before the
 * ellipsis - a text that opens with a reference, a link or a code span longer than the bound - the
 * cut by length stands, since a head of nothing tells a reader less than half a reference does,
 * and it closes what it leaves open (`closeCut`). Inside an inline link or picture around a
 * reference, the head is the link's opening, its words cut by this same rule to leave room for
 * the rest of the link, and that rest as the text writes it, from the `]` its words end at to its
 * `)`, so the head reads that link. Inside a code span, the head is as much of the span's content
 * as fits with the span's opening backticks written after it, which close it, so what the span
 * holds reads as code; where no character of the content fits, the head is the opening backticks
 * alone. Where the rest of a link does not fit the bound, or the closed link would not read as
 * that link, the cut by length stands as it is. A bare reference has nothing to close, so the cut
 * by length can leave half of one: an excerpt's head of an address whose slug runs past the bound
 * names an artifact of a shorter slug.
 *
 * It reads the text's first `limit` and `LONGEST_WHOLE_REFERENCE` characters, no more, as the
 * server does: a reference or a link that runs to the edge of that window, where the text runs on
 * past it, may run further, so it is none the cut keeps whole, and is cut by length. A code span
 * that runs to that edge, or that the window opens and does not close in the paragraph it ends in,
 * may close past it, so it counts as crossing the cut (`windowSpans`).
 */
export function headRefs(
  text: string,
  limit: number,
  ellipsis: string,
  origin: string = window.location.origin
): string {
  const keep = Math.max(limit - Array.from(ellipsis).length, 0);
  let cut = 0;
  let windowEnd = text.length;
  let over = false;
  let count = 0;
  let at = 0;
  for (const character of text) {
    if (count === keep) cut = at;
    if (count === limit) over = true;
    if (count === limit + LONGEST_WHOLE_REFERENCE) {
      windowEnd = at;
      break;
    }
    count += 1;
    at += character.length;
  }
  if (!over) return text;
  return `${headOf(text.slice(0, windowEnd), windowEnd < text.length, cut, 0, ellipsis, origin)}${ellipsis}`;
}

/** The head of `windowed`, the text's first characters, cut at `cut` (`headRefs`), without its
 *  ellipsis: `windowed` up to where the walk back ends (`headEnd`), or, where that leaves nothing
 *  past `floor`, the cut by length with what it leaves open closed (`closeCut`). `floor` is 0 for
 *  a whole head, and the first character of a link's words where those words are cut to leave
 *  room for the rest of the link. */
function headOf(
  windowed: string,
  truncated: boolean,
  cut: number,
  floor: number,
  ellipsis: string,
  origin: string
): string {
  const written = referenceExtents(windowed, origin).filter(
    (reference) => !truncated || reference.end < windowed.length
  );
  const spans = windowSpans(windowed, truncated);
  const end = headEnd(windowed, written, spans, cut, ellipsis, origin);
  return end > floor
    ? windowed.slice(0, end)
    : closeCut(windowed, written, spans, cut, floor, origin);
}

/** Where the head of `windowed` cut at `cut` ends (`headRefs`): `cut` moved back past every
 *  reference `written` and every code span of `spans` that crosses it, and every reference the
 *  head reads otherwise than the text does, less the whitespace before it but the one a reference
 *  ends at. Each move is back, so it ends; at 0 it leaves nothing. */
function headEnd(
  windowed: string,
  written: readonly Extent[],
  spans: readonly CodeRange[],
  cut: number,
  ellipsis: string,
  origin: string
): number {
  let at = cut;
  while (at > 0) {
    let back = at;
    for (const crossing of [...written, ...spans]) {
      if (crossing.start < at && at < crossing.end) back = Math.min(back, crossing.start);
    }
    const read = referenceExtents(`${windowed.slice(0, at)}${ellipsis}`, origin);
    for (const reference of read) {
      if (reference.start < at && !holds(written, reference)) {
        back = Math.min(back, reference.start);
      }
    }
    for (const reference of written) {
      if (reference.end <= at && !holds(read, reference)) back = Math.min(back, reference.start);
    }
    if (back === at) break;
    at = windowed.slice(0, back).replace(TRAILING_SPACE, "").length;
    // A head that now ends after a reference the text follows with whitespace keeps the first of
    // it, so the ellipsis after the head joins nothing: without it the ellipsis would join that
    // reference and the walk step back past it, and on a line of short addresses back past every
    // one of them.
    if (at < back && written.some((reference) => reference.end === at)) at += 1;
  }
  return at;
}

/** The cut by length of `windowed` at `cut`, which a head falls back to where moving the cut back
 *  leaves nothing past `floor` (`headRefs`), with what it leaves open closed so the head reads what
 *  it holds as the text does: the inline link or picture around a reference the cut is in
 *  (`closeLink`), or else the code span it is in (`closeSpan`). Where it is in neither, or in a
 *  link it cannot close and in no span, it is `windowed` up to `cut`. */
function closeCut(
  windowed: string,
  written: readonly Extent[],
  spans: readonly CodeRange[],
  cut: number,
  floor: number,
  origin: string
): string {
  const link = closeLink(windowed, written, cut, floor, origin);
  if (link !== undefined) return link;
  const span = spans.find((one) => floor <= one.start && one.start < cut && cut < one.end);
  return span === undefined ? windowed.slice(0, cut) : closeSpan(windowed, span, cut);
}

/** The head of `windowed` cut at `cut` inside the inline link or picture around a reference
 *  `written` there, closed: `windowed` up to the link's words, those words cut by `headRefs`' own
 *  rule with the link's `[` as the floor no cut moves back past, so it splits no reference or code
 *  span in them, and the rest of the link as the text writes it, from the `]` its words end at
 *  through its `)`, all of it within the characters of `windowed` up to `cut`. Nothing where no
 *  such link holds the cut, where the rest of the link leaves no room for a word, or where the
 *  head would not read as that link - a bracket the cut words leave open, or a backslash they end
 *  in, reads otherwise. */
function closeLink(
  windowed: string,
  written: readonly Extent[],
  cut: number,
  floor: number,
  origin: string
): string | undefined {
  const link = [...inlineLinks(windowed)]
    .sort((one, other) => linkStart(one) - linkStart(other))
    .find((one) => floor <= linkStart(one) && linkStart(one) < cut && cut < one.close);
  if (link === undefined) return undefined;
  const start = linkStart(link);
  if (!written.some((reference) => reference.start === start && reference.end === link.close)) {
    return undefined;
  }
  const words = link.open + 1;
  const rest = windowed.slice(link.wordsEnd, link.close);
  const room =
    Array.from(windowed.slice(0, cut)).length -
    Array.from(windowed.slice(0, words)).length -
    Array.from(rest).length;
  if (room <= 0) return undefined;
  let wordsCut = words;
  for (let left = room; left > 0; left -= 1) {
    wordsCut += (windowed.codePointAt(wordsCut) ?? 0) > 0xffff ? 2 : 1;
  }
  const head = `${headOf(windowed.slice(0, link.wordsEnd), false, wordsCut, words, "", origin)}${rest}`;
  return inlineLinks(head).some((one) => linkStart(one) === start && one.close === head.length)
    ? head
    : undefined;
}

/** The head of `windowed` cut at `cut` inside `span`, closed: the span's content up to its opening
 *  backticks' width in characters before `cut`, less the backticks and whitespace it ends in, then
 *  a run of that width. The run follows a character of the content on the content's own line, so
 *  it joins no backtick run of the content and opens no fence, and closes the span where the head
 *  ends it. Where no character of the content fits with that run, the head is the span's opening
 *  backticks alone, so what the span holds reads nowhere: cut by length, even a `#q` of `#q12`
 *  would read as a live reference. */
function closeSpan(windowed: string, span: CodeRange, cut: number): string {
  let content = span.start;
  while (windowed[content] === "`") content += 1;
  const opener = windowed.slice(span.start, content);
  let end = cut;
  for (let left = opener.length; left > 0 && end > 0; left -= 1) {
    // A character UTF-16 writes as two ends in a low surrogate, and is one character to step over.
    const unit = windowed.charCodeAt(end - 1);
    end -= end > 1 && unit >= 0xdc00 && unit <= 0xdfff ? 2 : 1;
  }
  const held = end > content ? windowed.slice(content, end).replace(CLOSING_TRIM, "") : "";
  return held === ""
    ? windowed.slice(0, Math.min(cut, content))
    : `${windowed.slice(0, content + held.length)}${opener}`;
}

/** Every code span `windowed` writes, its blocks left out: a cut inside a span the head does not
 *  close leaves its opening backticks as text, and what the span held - an address in it included
 *  - read as prose, where a cut inside a fenced block leaves the rest of the head in the block as
 *  the text has it. A window with no backtick has no span, and is not walked for one.
 *
 *  A window that stops short of the text (`truncated`) is read with a line after it that closes a
 *  span of any width it could open (`closingLine`): the first span the window leaves open in the
 *  paragraph it ends in may close past the window, so the line closes it and it runs past the
 *  window's end, as a span whose closing run ends at that end does, whose run the text may carry
 *  on. Either crosses every cut after its start. A span the window opens in an earlier paragraph
 *  is closed by none, and one the window opens after such a span is inside it, so the line closes
 *  neither. */
function windowSpans(windowed: string, truncated: boolean): CodeRange[] {
  if (!windowed.includes("`")) return [];
  const read = truncated ? `${windowed}${closingLine(windowed)}` : windowed;
  return codeRanges(read).filter((range) => !range.block && range.start < windowed.length);
}

/** A line to read after `windowed` that continues the paragraph it ends in, if one is open there,
 *  and closes the first span it leaves open: an `x`, so the line opens no block of its own and
 *  reads as that paragraph's text, then a run of each width a span of `windowed` could open with,
 *  widest last and each after a space, so no two join. A span opens with a run of backticks or,
 *  after a backslash that takes the first of them, the rest of that run. */
function closingLine(windowed: string): string {
  const widths = new Set<number>();
  for (const run of windowed.matchAll(BACKTICK_RUN)) {
    widths.add(run[0].length);
    widths.add(run[0].length - 1);
  }
  const runs = [...widths]
    .filter((width) => width > 0)
    .sort((one, other) => one - other)
    .map((width) => ` ${"`".repeat(width)}`);
  return `${windowed.endsWith("\n") ? "" : "\n"}x${runs.join("")}`;
}

/** Whether `extents` holds `reference`: a reference at the same place naming the same thing. */
function holds(extents: readonly Extent[], reference: Extent): boolean {
  return extents.some(
    (one) =>
      one.start === reference.start && one.end === reference.end && one.names === reference.names
  );
}

/** Where `link` starts as a reader sees it: at its `[`, or at the `!` of a picture. */
function linkStart(link: InlineLink): number {
  return link.picture ? link.open - 1 : link.open;
}

/** Every reference of `text` as a cut reads it (`headRefs`), none of them resolved against what
 *  Dispatch holds, since a cut that splits a reference changes what the text says whether or not
 *  its target exists: each span the shared span reader finds that is an address of Dispatch or of
 *  the dashboard at `origin`, read as the link's address where it starts inside a link that
 *  writes it there, and each bare key and `#id` the shared reader finds, each widened over the
 *  inline link that holds it and named as `Extent` says. The link that holds one is the first to
 *  open, as the server's link reader lists them. */
function referenceExtents(text: string, origin: string): Extent[] {
  const links = [...inlineLinks(text)].sort((one, other) => linkStart(one) - linkStart(other));
  const extents: Extent[] = [];
  const add = (start: number, end: number, names: string) => {
    const link = links.find((one) => linkStart(one) <= start && start < one.close);
    extents.push(
      link === undefined
        ? { end, names, start }
        : { end: link.close, names, start: linkStart(link) }
    );
  };
  for (const span of referenceSpans(text)) {
    const link = links.find(
      (one) =>
        linkStart(one) < span.start &&
        span.start < one.close &&
        one.addressEnd > one.addressStart &&
        text.startsWith(text.slice(one.addressStart, one.addressEnd), span.start)
    );
    const address =
      link === undefined ? span.value : text.slice(link.addressStart, link.addressEnd);
    const route = address === "" ? undefined : referenceRouteFromHref(address, origin);
    if (route !== undefined) {
      add(span.start, span.start + address.length, buildDispatchReference(route));
    }
  }
  for (const reference of readTextReferences(text, { origin }).references) {
    if (reference.form === "key") {
      add(reference.start, reference.end, `dispatch://${reference.text}`);
    }
    if (reference.form === "question") add(reference.start, reference.end, reference.text);
  }
  return extents;
}
