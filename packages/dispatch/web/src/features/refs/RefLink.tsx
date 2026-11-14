import { type ReactNode, useContext, useLayoutEffect, useState } from "react";

import { borderDefault } from "../../theme/classes";
import { useReferenceHref, useReferenceTarget } from "./reference-target";
import {
  buildDispatchReference,
  type DispatchReferenceRoute,
  nodeRouteFromHref,
  referenceHref,
  referenceRouteFromHref,
} from "./routes";
import {
  SEARCH_HIGHLIGHT_ATTRIBUTE,
  SEARCH_HIGHLIGHT_SELECTOR,
  SearchHighlightClassName,
} from "./search-highlight";
import { readTextReferences, type TextReference, type TextReferenceFacts } from "./text-references";

/** A stretch of a reference's words, as UTF-16 offsets into them: `text.slice(start, end)`. */
export interface WordStretch {
  readonly start: number;
  readonly end: number;
}

/** A reference whose words hold no search mark of their own. */
const NO_STRETCHES: readonly WordStretch[] = [];

export interface ReferenceAnchor {
  /** The element the `RefLink` portal renders into: the `<a>` itself, or whatever a surface put
   *  in its place (a host that is one control keeps no link inside its link). */
  readonly anchor: HTMLElement;
  readonly key: string;
  readonly route: DispatchReferenceRoute;
  /** The words the anchor showed before they were cleared for the portal: a bare reference's
   *  `dispatch://…`, or the text a Markdown link gave it. */
  readonly text: string;
  /** Whether the anchor's words are its own address, which is what reads as the target's
   *  current name: a reference standing on its own, and a link whose words are the address it
   *  names - what a Markdown autolink is stored as. A link whose words its author wrote keeps
   *  those words (`RefLink`). */
  readonly addressed: boolean;
  /** The stretches of `text` the server marked as matching a search, in order and apart, each
   *  under a mark inside the anchor (`collectReferenceAnchors`); none where the anchor's words
   *  held no mark, or where a mark around the anchor already covers all it draws. */
  readonly marked: readonly WordStretch[];
}

/** The 40 px square a picture shows as beside its title, wherever a line holds it: the Artifacts
 *  tab's thumbnail. `not-prose` keeps Markdown's figure margins off it. */
export const pictureThumbnailClassName = `not-prose inline-block h-10 w-10 shrink-0 rounded border object-cover align-middle ${borderDefault}`;

/** What a rendered body's text may be linked to: what is already known about the body
 * (`TextReferenceFacts`, the issue keys that exist, its document's question map and the items
 * its id prefixes resolved to) and the references its own source cites. A candidate the rendered
 * text holds but the source does not - the `#q2` of an escaped `\#q2`, whose backslash markdown
 * drops, or the `&#35;q2` a numeric reference writes - names nothing, so the set is what the
 * link pass is held to. */
export interface ReferenceLinking {
  readonly facts: TextReferenceFacts;
  readonly cited: ReadonlySet<string>;
}

/** Portal content for an inline reference anchor.
 *
 * One rule says what it reads as, the same rule in a document, a comment, an ask, a message and
 * every other record: an address standing on its own, and a link whose words are that same
 * address, read as the target's current name once `useReferenceTarget` has it (`addressed`); a
 * link whose words its author wrote keeps those words, since the author already said what the
 * reference is there to say. A reference the reader may not see is the same case as the words
 * themselves: the words the writer wrote, with no title drawn over them. A reference whose target
 * Dispatch answered does not exist is its words with no link at all (R8), as the document editor
 * draws it: the element it renders into stops being a link (`unlinkMissingReference`).
 *
 * A search hit's marks follow the same rule (`marked`). Wherever the words are drawn, the server's
 * marks stay on them exactly as it sent them; only a title drawn in their place is marked, whole,
 * since none of its characters is one the server matched. A late answer moves them either way:
 * the words keep their marks while the title loads, the title is marked whole once it comes, and
 * the words have their marks back when the target turns out not to exist.
 *
 * Beside the words goes the picture's thumbnail when the reference names an image artifact. It
 * shows no document text, so it reads none. A thumbnail the browser cannot draw (a pinned
 * version the server does not serve) is dropped, leaving the title alone, so a stale or
 * hand-written reference never shows a broken-image glyph. When the element it renders into is
 * a link, its href follows `useReferenceHref`, which a pinned block of a spec resolves only
 * once its issue's summary is read. */
export function RefLink({
  addressed,
  anchor,
  marked = NO_STRETCHES,
  route,
  text,
}: {
  /** Whether the words the anchor showed are its own address (`ReferenceAnchor.addressed`). */
  addressed: boolean;
  anchor?: HTMLElement;
  /** The stretches of `text` a search marked (`ReferenceAnchor.marked`); none by default. */
  marked?: readonly WordStretch[];
  route: DispatchReferenceRoute;
  /** The words the anchor showed before they were cleared for this portal: the reference as the
   *  author wrote it, or a Markdown link's own text. */
  text: string;
}): ReactNode {
  const { missing, picture, title } = useReferenceTarget(route, { text: false });
  // A reference opening no page of this dashboard - a person, whom nothing shows (R5), and any
  // path the gate refuses - leaves the words standing where a link would be.
  const href = useReferenceHref(route);
  useLayoutEffect(() => {
    if (anchor instanceof HTMLAnchorElement && href !== undefined && !missing) {
      anchor.setAttribute("href", href);
    }
  }, [anchor, href, missing]);
  useLayoutEffect(() => {
    if (anchor === undefined || !missing) {
      return;
    }
    return unlinkMissingReference(anchor);
  }, [anchor, missing]);
  // The address that failed, not a flag: a plain reference names the latest version, so a version
  // uploaded since gets its own load instead of inheriting the last one's failure.
  const [failedPicture, setFailedPicture] = useState<string>();
  return (
    <>
      {picture === undefined || picture === failedPicture ? null : (
        <>
          <img
            alt=""
            className={pictureThumbnailClassName}
            loading="lazy"
            onError={() => setFailedPicture(picture)}
            src={picture}
          />{" "}
        </>
      )}
      {/* An issue reads as its key then its title, as the project's issues table writes a key
          beside one (`project/IssuesTable.tsx`), so the link's accessible name is `CORE-12 Fix the
          thing`. Until the title resolves, for an issue the reader cannot read, and for a target
          that does not exist, the words the author wrote stand. */}
      {!addressed || missing || title === undefined ? (
        markedWords(text, marked)
      ) : (
        <MarkedWhole marked={marked.length > 0}>
          {route.kind === "issue" ? (
            <>
              <span className="font-semibold whitespace-nowrap">{route.key}</span> {title}
            </>
          ) : (
            title
          )}
        </MarkedWhole>
      )}
    </>
  );
}

/** A search mark React draws: the element the serializer draws over a run the server marked
 *  (`SEARCH_HIGHLIGHT_SELECTOR`), with the class the surface styles its marks with
 *  (`SearchHighlightClassName`). */
function SearchMark({ children }: { children: ReactNode }): ReactNode {
  const className = useContext(SearchHighlightClassName);
  return (
    <mark
      {...{ [SEARCH_HIGHLIGHT_ATTRIBUTE]: "" }}
      className={className === "" ? undefined : className}
    >
      {children}
    </mark>
  );
}

/** `children` under one search mark where `marked`, else as they are. */
function MarkedWhole({ children, marked }: { children: ReactNode; marked: boolean }): ReactNode {
  return marked ? <SearchMark>{children}</SearchMark> : children;
}

/** `text` with each of the `marked` stretches under a search mark of its own. */
function markedWords(text: string, marked: readonly WordStretch[]): ReactNode {
  if (marked.length === 0) {
    return text;
  }
  const drawn: ReactNode[] = [];
  let from = 0;
  for (const { end, start } of marked) {
    if (from < start) {
      drawn.push(text.slice(from, start));
    }
    drawn.push(<SearchMark key={start}>{text.slice(start, end)}</SearchMark>);
    from = end;
  }
  if (from < text.length) {
    drawn.push(text.slice(from));
  }
  return drawn;
}

/** What makes a reference element a link, or a link-styled span a reader and the hover card take
 *  for one (`useRenderedMarkdown`'s `inertLinks`, `RefPreview`'s trigger). */
const LINK_ATTRIBUTES = ["href", "data-dispatch-ref", "data-dispatch-href", "data-markdown-link"];

/** Takes from `anchor` everything that makes it a link, and marks it as a reference whose target
 *  does not exist (`data-dispatch-missing`, holding the reference, which the stylesheet draws as
 *  plain words); answers the function that puts it all back, for a target that comes to exist
 *  while the page is open - an issue created after the comment that named it. */
function unlinkMissingReference(anchor: HTMLElement): () => void {
  const saved = LINK_ATTRIBUTES.flatMap((name) => {
    const value = anchor.getAttribute(name);
    return value === null ? [] : [[name, value] as const];
  });
  const reference =
    anchor.getAttribute("data-dispatch-ref") ?? anchor.getAttribute("data-dispatch-href") ?? "";
  for (const name of LINK_ATTRIBUTES) {
    anchor.removeAttribute(name);
  }
  anchor.setAttribute("data-dispatch-missing", reference);
  return () => {
    anchor.removeAttribute("data-dispatch-missing");
    for (const [name, value] of saved) {
      anchor.setAttribute(name, value);
    }
  };
}

const excludedRefAncestorTags: Record<string, true> = { A: true, CODE: true, PRE: true };

/** Whether the link pass reads what `node` holds: no link, code span or code block of `root`
 *  holds it, `node` itself included. A text node is read where this answers for it. */
function linkPassReads(node: Node, root: Node): boolean {
  for (let current: Node | null = node; current !== null && current !== root; ) {
    if (current instanceof HTMLElement && excludedRefAncestorTags[current.tagName] === true) {
      return false;
    }
    current = current.parentNode;
  }
  return true;
}

/** Whether the link pass links `reference`: the one filter both its passes read. */
function linksReference(reference: TextReference, cited: ReadonlySet<string>): boolean {
  return (
    reference.reference !== undefined &&
    cited.has(reference.reference) &&
    // A reference that opens no page of this dashboard - a person, whom nothing in Dispatch
    // shows - stays the words its author wrote: there would be nothing behind the link.
    // The same gate `collectReferenceAnchors` writes the href behind, so the two agree.
    (reference.route === undefined || referenceHref(reference.route) !== undefined)
  );
}

/** One node of a run of words a search mark can cut: a text node, or a search mark holding
 *  nothing but text, which is how the serializer draws the highlight over a run (it is the
 *  schema's last mark, so its element is the innermost over the words). */
interface RunPiece {
  readonly node: ChildNode;
  readonly text: string;
  readonly marked: boolean;
}

function runPiece(node: ChildNode): RunPiece | undefined {
  if (node instanceof Text) {
    return { marked: false, node, text: node.data };
  }
  if (
    node instanceof HTMLElement &&
    node.matches(SEARCH_HIGHLIGHT_SELECTOR) &&
    Array.from(node.childNodes).every((child) => child instanceof Text)
  ) {
    return { marked: true, node, text: node.textContent ?? "" };
  }
  return undefined;
}

/** The link the link pass wraps `reference` in, holding `words`. Its href is the reference as
 *  resolved, so a prefix links to the whole id it names. */
function referenceLink(reference: TextReference, ...words: Node[]): HTMLAnchorElement {
  const anchor = document.createElement("a");
  anchor.setAttribute("href", reference.reference ?? "");
  // The reference stands on its own here, whatever it was written as: a `dispatch://` address,
  // an issue key, or a `#` and a question block's id. Each reads as the target's current name,
  // which `collectReferenceAnchors` cannot tell from the href alone - a key and its address are
  // not the same characters.
  anchor.setAttribute("data-dispatch-bare", "");
  anchor.append(...words);
  return anchor;
}

/**
 * Links each reference the link pass links where a search mark falls in it, with the server's
 * marks inside the link where the server put them. Postgres's `ts_headline` marks the words it
 * matched, and it cuts an address or a key into its own words to do so: a search for `CORE-1`
 * answers `dispatch://<b>CORE</b><b>-1</b>` and `<b>CORE</b><b>-1</b>`, and one for `CORE`
 * answers `<b>CORE</b>-1` (measured against the pod's Postgres, 2026-11-13, INFRA-1702). The
 * snippet's tree then holds the reference in more than one text node, and the link pass reads one
 * text node at a time, so it would find no reference there and the hit would show the raw address.
 *
 * So each run of adjacent text nodes and search marks the link pass reads is read as the one
 * text it is, with the link pass's own reader, facts and filter (`linksReference`). A reference
 * it would link that holds a marked character becomes the link the link pass makes of one
 * (`referenceLink`), holding the server's own pieces of it - its text nodes and its marks, cut
 * where the reference starts and ends - and the rest of the run stays the server's pieces too;
 * the link pass then leaves those links alone, as it leaves every link. No mark grows: which
 * characters read as marked is `RefLink`'s to draw, from the marks `collectReferenceAnchors`
 * finds inside the link, and it keeps them on the words wherever it draws the words, marking
 * only a title drawn in their place whole. A mark over a word the link pass does not link -
 * `SHA-256`, `UTF-8`, `#general`, `#1234`, the key of an issue that does not exist - stays exactly
 * where the server put it, and a run whose marks touch no linked reference, or a body with no
 * search mark at all, is left as it was.
 */
function linkMarkedReferences(root: HTMLElement, linking: ReferenceLinking): void {
  if (root.querySelector(SEARCH_HIGHLIGHT_SELECTOR) === null) {
    return;
  }
  const walker = document.createTreeWalker(root, NodeFilter.SHOW_ELEMENT);
  const containers: HTMLElement[] = [root];
  for (let node = walker.nextNode(); node !== null; node = walker.nextNode()) {
    if (node instanceof HTMLElement && linkPassReads(node, root)) {
      containers.push(node);
    }
  }
  for (const container of containers) {
    let run: RunPiece[] = [];
    for (const child of Array.from(container.childNodes)) {
      const piece = runPiece(child);
      if (piece === undefined) {
        linkRunReferences(container, run, child, linking);
        run = [];
      } else {
        run.push(piece);
      }
    }
    linkRunReferences(container, run, null, linking);
  }
}

/** `linkMarkedReferences` over one run of `container`'s children, the run ending before `next`
 *  (`null` at the container's end). */
function linkRunReferences(
  container: HTMLElement,
  run: readonly RunPiece[],
  next: ChildNode | null,
  { cited, facts }: ReferenceLinking
): void {
  if (!run.some((piece) => piece.marked)) {
    return;
  }
  // Where each piece's words start and end in the run's text, in UTF-16 units, which is what a
  // reference's `start` and `end` count.
  let length = 0;
  const placed = run.map((piece) => {
    const start = length;
    length += piece.text.length;
    return { end: length, piece, start };
  });
  const text = run.map((piece) => piece.text).join("");
  const linked = readTextReferences(text, facts).references.filter(
    (reference) =>
      linksReference(reference, cited) &&
      placed.some(
        ({ end, piece, start }) => piece.marked && start < reference.end && reference.start < end
      )
  );
  if (linked.length === 0) {
    return;
  }
  /** The server's pieces of `text.slice(from, to)`: a piece that lies inside it whole is moved,
   *  and one it cuts is cut, a mark into a copy of itself holding the words it keeps. */
  const piecesOf = (from: number, to: number): Node[] =>
    placed.flatMap(({ end, piece, start }) => {
      const cutStart = Math.max(from, start);
      const cutEnd = Math.min(to, end);
      if (cutStart >= cutEnd) {
        return [];
      }
      if (cutStart === start && cutEnd === end) {
        return [piece.node];
      }
      const words = document.createTextNode(piece.text.slice(cutStart - start, cutEnd - start));
      if (!piece.marked) {
        return [words];
      }
      const mark = piece.node.cloneNode(false);
      mark.appendChild(words);
      return [mark];
    });
  const fragment = document.createDocumentFragment();
  let from = 0;
  for (const reference of linked) {
    fragment.append(
      ...piecesOf(from, reference.start),
      referenceLink(reference, ...piecesOf(reference.start, reference.end))
    );
    from = reference.end;
  }
  fragment.append(...piecesOf(from, text.length));
  // A piece moved whole has left the container already; one that was cut, or held no words, is
  // still in it.
  for (const { node } of run) {
    if (node.parentNode === container) {
      node.remove();
    }
  }
  container.insertBefore(fragment, next);
}

/**
 * Wraps every reference root's text writes into a real `<a>`, so `collectReferenceAnchors` can
 * then resolve it like any other link: a `dispatch://` reference or a dashboard address, an
 * issue key on its own, and a `#` and a question block's id. The one reader decides
 * (`text-references.ts`), and only where it says the reference resolved: an unknown key, an
 * unknown question and a prefix no item was found for stay the words the author wrote, with no
 * link to click and no hover trigger. The anchor's href is the reference as resolved, so a
 * prefix links to the whole id it names.
 *
 * Text already inside a link, inline code span, or code block is left alone (its ancestor
 * already parsed as a distinct node, so there is no need to re-tokenize raw Markdown to find
 * code/link boundaries by hand): the server indexes a key, a `#id` and a reference in code, and
 * no renderer links one there. A bare `http(s)://` dashboard URL needs no pass of its own in
 * practice - remark-gfm autolinks it into a real link mark during Markdown parsing - but is read
 * here too wherever it reaches the page as text.
 *
 * What a body's rendered text holds is not always what its source held: Markdown drops the
 * backslash of an escaped `\#q2` and resolves the `&#35;` that writes a `#`. So a candidate
 * links only while the body's own source cites it (`ReferenceLinking.cited`), which is what the
 * server indexed for the same body.
 *
 * A reference a search mark falls in is linked first (`linkMarkedReferences`), since a reference
 * a search split is no longer one text node for this pass to read; its link holds the server's
 * marks where the server put them.
 */
export function linkifyDispatchRefs(root: HTMLElement, linking: ReferenceLinking): void {
  const { cited, facts } = linking;
  if (cited.size === 0) {
    return;
  }
  linkMarkedReferences(root, linking);
  const walker = document.createTreeWalker(root, NodeFilter.SHOW_TEXT);
  const candidates: Text[] = [];
  for (let node = walker.nextNode(); node !== null; node = walker.nextNode()) {
    if (node instanceof Text && linkPassReads(node, root)) {
      candidates.push(node);
    }
  }
  for (const textNode of candidates) {
    const value = textNode.data;
    const found = readTextReferences(value, facts).references.filter((reference) =>
      linksReference(reference, cited)
    );
    if (found.length === 0) {
      continue;
    }
    const fragment = document.createDocumentFragment();
    let lastIndex = 0;
    for (const reference of found) {
      fragment.appendChild(document.createTextNode(value.slice(lastIndex, reference.start)));
      fragment.appendChild(referenceLink(reference, document.createTextNode(reference.text)));
      lastIndex = reference.end;
    }
    fragment.appendChild(document.createTextNode(value.slice(lastIndex)));
    textNode.replaceWith(fragment);
  }
}

/**
 * Whether an anchor's target is a URL its author left as text: what GFM autolinks, a bare
 * `https://…` (a bare `www.…`, whose target gains `http://`) or one in angle brackets
 * (`<dispatch://…>` too), and the anchor `linkifyDispatchRefs` wraps around a bare
 * `dispatch://…`, all show their target as their words. A Markdown link whose words are its own
 * target (`[dispatch://X](dispatch://X)`, the form a document stores an autolink in) reads the
 * same, and is the same text to the server. `target` is the anchor's own target: Proof's
 * serializer moves a `dispatch://` target into `data-dispatch-href` and empties `href`.
 */
function isTextUrl(target: string, text: string): boolean {
  return target === text || target === `http://${text}`;
}

/**
 * Finds every already-rendered `<a>` in root whose href is a dispatch:// reference or a
 * same-origin dashboard path — a link `linkifyDispatchRefs` just inserted, an ordinary Markdown
 * link a user wrote by hand, or a bare `http(s)://` URL remark-gfm autolinked — rewrites its href
 * to the SPA route via `buildReferencePath`, and clears its text so the caller can
 * portal a `RefLink` in to render the resolved title. A URL its author left as text
 * (`isTextUrl`) is read as the server's `text.Extract` reads text (`referenceRouteFromHref`): one
 * whose block is past the bounds the index keys names its document, as the server cites it; an
 * `http(s)://` one past them stays the link it is; and a `dispatch://` one that names nothing
 * becomes its words as they were drawn, as a bare one already renders, with no link to click and
 * no hover trigger. A link its author wrote names its target explicitly, and drawing or opening it
 * is a read, so its href is read as the server reads a request (`nodeRouteFromHref`), whatever the
 * length of its names. An external link, or an author's href that fails to parse as a reference,
 * is left untouched, and so is a picture's link (`data-dispatch-picture`, the engine's
 * `pictureSerializer`), whose content is a `DispatchPicture` rather than a title.
 *
 * Before it clears an anchor, it notes which of the anchor's words a search mark inside it
 * covers (`ReferenceAnchor.marked`, `markedStretches`), so `RefLink` can draw the server's marks
 * again wherever it draws those words. An anchor that becomes its words keeps its own nodes, and
 * with them the marks inside it.
 *
 * `@legion/proof-editor`'s Markdown link serializer sanitizes a `dispatch://` href to `""`
 * (Milkdown's link sanitizer only allows http/https/mailto/tel/ftp — a document strangers can
 * edit should never render an attacker-chosen non-http scheme as a clickable href), but tags the
 * anchor with `data-dispatch-href` carrying the original target. `linkifyDispatchRefs`'s own
 * freshly-wrapped anchors, and a same-origin dashboard URL a remark-gfm autolinked, still carry
 * their real value in `href` — `data-dispatch-href` takes priority when present.
 */
export function collectReferenceAnchors(
  root: HTMLElement,
  appOrigin: string = window.location.origin
): ReferenceAnchor[] {
  const targets: ReferenceAnchor[] = [];
  let index = 0;
  for (const anchor of root.querySelectorAll<HTMLAnchorElement>("a:not([data-dispatch-picture])")) {
    const href = anchor.getAttribute("data-dispatch-href") ?? anchor.getAttribute("href");
    if (href === null) {
      continue;
    }
    const text = anchor.textContent ?? "";
    // What reads as the target's current name: an address standing on its own, which the link
    // pass marks as it wraps one (`data-dispatch-bare`), and a link whose words are its own
    // address (`isTextUrl`), the form a document stores an autolink in. A link whose words its
    // author wrote keeps those words.
    const textUrl = isTextUrl(href, text);
    const addressed = anchor.hasAttribute("data-dispatch-bare") || textUrl;
    const route = textUrl
      ? referenceRouteFromHref(href, appOrigin)
      : nodeRouteFromHref(href, appOrigin);
    if (route === undefined) {
      // A `dispatch://` address that cites nothing - past the bounds the index keys, or a name
      // no kind of node is written under - is the words the writer wrote, with no link (R8),
      // whether those words are the address itself or the author's own: a `dispatch://` href
      // opens no page in any browser, so leaving it live is a dead link in the page. A bare one
      // already renders that way (`linkifyDispatchRefs`), and Proof's serializer leaves a
      // worded one `href=""` with `data-dispatch-href`, which would click to the current page
      // and open a hover card for a reference the server never recorded.
      if (href.startsWith("dispatch://")) {
        anchor.replaceWith(...anchor.childNodes);
      }
      continue;
    }
    // A reference that opens no page of this dashboard is its words, as one that cites nothing
    // is: a person, whom nothing in Dispatch shows (R5), and anything that does not resolve to
    // this origin, which no route writes today and none may start writing unnoticed.
    const path = referenceHref(route);
    if (path === undefined) {
      anchor.replaceWith(...anchor.childNodes);
      continue;
    }
    anchor.setAttribute("href", path);
    const reference = buildDispatchReference(route);
    anchor.setAttribute("data-dispatch-ref", reference);
    const marked = markedStretches(anchor);
    anchor.replaceChildren();
    targets.push({ addressed, anchor, key: `${reference}:${index}`, marked, route, text });
    index += 1;
  }
  return targets;
}

/** The stretches of `anchor`'s words (its `textContent`) that sit under a search mark inside it,
 *  in order, with stretches that touch merged into one. A mark around the anchor covers every
 *  character it draws, title or words alike, so it is no stretch of the anchor's own. */
function markedStretches(anchor: HTMLElement): readonly WordStretch[] {
  if (anchor.querySelector(SEARCH_HIGHLIGHT_SELECTOR) === null) {
    return NO_STRETCHES;
  }
  const stretches: { start: number; end: number }[] = [];
  const walker = document.createTreeWalker(anchor, NodeFilter.SHOW_TEXT);
  let offset = 0;
  for (let node = walker.nextNode(); node !== null; node = walker.nextNode()) {
    const start = offset;
    offset += node.textContent?.length ?? 0;
    const mark = node.parentElement?.closest(SEARCH_HIGHLIGHT_SELECTOR);
    if (offset === start || mark === null || mark === undefined || !anchor.contains(mark)) {
      continue;
    }
    const last = stretches.at(-1);
    if (last !== undefined && last.end === start) {
      last.end = offset;
    } else {
      stretches.push({ end: offset, start });
    }
  }
  return stretches;
}
