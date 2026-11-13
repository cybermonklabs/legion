import { type ReactNode, useLayoutEffect, useState } from "react";

import { borderDefault } from "../../theme/classes";
import { useReferenceHref, useReferenceTarget } from "./reference-target";
import {
  buildDispatchReference,
  type DispatchReferenceRoute,
  nodeRouteFromHref,
  referenceHref,
  referenceRouteFromHref,
} from "./routes";
import { SEARCH_HIGHLIGHT_SELECTOR, searchMark } from "./search-highlight";
import { readTextReferences, type TextReference, type TextReferenceFacts } from "./text-references";

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
 * reference is there to say. A reference whose target Dispatch cannot resolve, or that the
 * reader may not see, is the same case as the words themselves: the words the writer wrote,
 * with no title drawn over them.
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
  route,
  text,
}: {
  /** Whether the words the anchor showed are its own address (`ReferenceAnchor.addressed`). */
  addressed: boolean;
  anchor?: HTMLElement;
  route: DispatchReferenceRoute;
  /** The words the anchor showed before they were cleared for this portal: the reference as the
   *  author wrote it, or a Markdown link's own text. */
  text: string;
}): ReactNode {
  const { picture, title } = useReferenceTarget(route, { text: false });
  // A reference opening no page of this dashboard - a person, whom nothing shows (R5), and any
  // path the gate refuses - leaves the words standing where a link would be.
  const href = useReferenceHref(route);
  useLayoutEffect(() => {
    if (anchor instanceof HTMLAnchorElement && href !== undefined) {
      anchor.setAttribute("href", href);
    }
  }, [anchor, href]);
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
          thing`. Until the title resolves, and for an issue the reader cannot read, the words
          the author wrote stand. */}
      {!addressed || title === undefined ? (
        text
      ) : route.kind === "issue" ? (
        <>
          <span className="font-semibold whitespace-nowrap">{route.key}</span> {title}
        </>
      ) : (
        title
      )}
    </>
  );
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
  readonly text: string;
  readonly marked: boolean;
}

function runPiece(node: ChildNode): RunPiece | undefined {
  if (node instanceof Text) {
    return { marked: false, text: node.data };
  }
  if (
    node instanceof HTMLElement &&
    node.matches(SEARCH_HIGHLIGHT_SELECTOR) &&
    Array.from(node.childNodes).every((child) => child instanceof Text)
  ) {
    return { marked: true, text: node.textContent ?? "" };
  }
  return undefined;
}

/**
 * Puts each reference the link pass will link, where a search mark falls in it, under one mark
 * of its own, whole. Postgres's `ts_headline` marks the words it matched, and it cuts an address
 * or a key into its own words to do so: a search for `CORE-1` answers
 * `dispatch://<b>CORE</b><b>-1</b>` and `<b>CORE</b><b>-1</b>`, and one for `CORE` answers
 * `<b>CORE</b>-1` (measured against the pod's Postgres, 2026-11-13, INFRA-1702). The snippet's
 * tree then holds the reference in more than one text node, and the link pass reads one text
 * node at a time, so it would find no reference there and the hit would show the raw address.
 *
 * So each run of adjacent text nodes and search marks the link pass reads is read as the one
 * text it is, with the link pass's own reader, facts and filter (`linksReference`). A reference
 * it would link that holds a marked character is marked from its first character to its last,
 * and the run is written again as one node per stretch of marked or unmarked words. Nothing else
 * moves: a mark over a word the link pass does not link - `SHA-256`, `UTF-8`, `#general`,
 * `#1234`, the key of an issue that does not exist - stays exactly where the server put it, and
 * a run whose marks touch no linked reference, or a body with no search mark at all, is left as
 * it was.
 */
function markLinkedReferencesWhole(root: HTMLElement, { cited, facts }: ReferenceLinking): void {
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
    let run: ChildNode[] = [];
    for (const child of Array.from(container.childNodes)) {
      if (runPiece(child) === undefined) {
        markRunReferencesWhole(container, run, child, cited, facts);
        run = [];
      } else {
        run.push(child);
      }
    }
    markRunReferencesWhole(container, run, null, cited, facts);
  }
}

/** `markLinkedReferencesWhole` over one run of `container`'s children, the run ending before
 *  `next` (`null` at the container's end). */
function markRunReferencesWhole(
  container: HTMLElement,
  run: readonly ChildNode[],
  next: ChildNode | null,
  cited: ReadonlySet<string>,
  facts: TextReferenceFacts
): void {
  const pieces = run.flatMap((node) => runPiece(node) ?? []);
  if (!pieces.some((piece) => piece.marked)) {
    return;
  }
  const text = pieces.map((piece) => piece.text).join("");
  // One entry per UTF-16 unit, which is what a reference's `start` and `end` count.
  const marked = pieces.flatMap((piece) =>
    Array.from({ length: piece.text.length }, () => piece.marked)
  );
  let widened = false;
  for (const reference of readTextReferences(text, facts).references) {
    if (
      linksReference(reference, cited) &&
      marked.slice(reference.start, reference.end).includes(true)
    ) {
      marked.fill(true, reference.start, reference.end);
      widened = true;
    }
  }
  if (!widened) {
    return;
  }
  const fragment = document.createDocumentFragment();
  for (let from = 0; from < text.length; ) {
    let to = from + 1;
    while (to < text.length && marked[to] === marked[from]) {
      to += 1;
    }
    const words = document.createTextNode(text.slice(from, to));
    fragment.append(marked[from] === true ? searchMark(words) : words);
    from = to;
  }
  for (const node of run) {
    node.remove();
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
 * A search mark the server put inside a reference it will link is widened to the whole
 * reference first (`markLinkedReferencesWhole`), so a reference a search split is still one
 * text node when this pass reads it.
 */
export function linkifyDispatchRefs(root: HTMLElement, linking: ReferenceLinking): void {
  const { cited, facts } = linking;
  if (cited.size === 0) {
    return;
  }
  markLinkedReferencesWhole(root, linking);
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
      const anchor = document.createElement("a");
      anchor.setAttribute("href", reference.reference ?? "");
      // The reference stands on its own here, whatever it was written as: a `dispatch://`
      // address, an issue key, or a `#` and a question block's id. Each reads as the target's
      // current name, which `collectReferenceAnchors` cannot tell from the href alone - a key
      // and its address are not the same characters.
      anchor.setAttribute("data-dispatch-bare", "");
      anchor.textContent = reference.text;
      fragment.appendChild(anchor);
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
 * becomes its words as plain text, as a bare one already renders, with no link to click and no
 * hover trigger. A link its author wrote names its target explicitly, and drawing or opening it
 * is a read, so its href is read as the server reads a request (`nodeRouteFromHref`), whatever the
 * length of its names. An external link, or an author's href that fails to parse as a reference,
 * is left untouched, and so is a picture's link (`data-dispatch-picture`, the engine's
 * `pictureSerializer`), whose content is a `DispatchPicture` rather than a title.
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
        anchor.replaceWith(document.createTextNode(text));
      }
      continue;
    }
    // A reference that opens no page of this dashboard is its words, as one that cites nothing
    // is: a person, whom nothing in Dispatch shows (R5), and anything that does not resolve to
    // this origin, which no route writes today and none may start writing unnoticed.
    const path = referenceHref(route);
    if (path === undefined) {
      anchor.replaceWith(document.createTextNode(text));
      continue;
    }
    anchor.setAttribute("href", path);
    const reference = buildDispatchReference(route);
    anchor.setAttribute("data-dispatch-ref", reference);
    anchor.replaceChildren();
    targets.push({ addressed, anchor, key: `${reference}:${index}`, route, text });
    index += 1;
  }
  return targets;
}
