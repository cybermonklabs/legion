import {
  Fragment,
  type MouseEvent,
  type ReactNode,
  type RefObject,
  useCallback,
  useLayoutEffect,
  useMemo,
  useRef,
} from "react";
import { useLocation, useNavigate } from "react-router-dom";

import type { DisplayTree } from "../../api/types";
import { MarkdownBody } from "../refs/MarkdownBody";
import { TextAskCard } from "../refs/markdown-cards";
import { isPlainClick } from "../refs/picture-lightbox";
import { type DispatchReferenceRoute, parseNodeReference } from "../refs/routes";
import type { TextSource } from "../refs/text-references";
import { chipWords } from "./node-row-chip";
import { authorText } from "./page-structure";
import { CONTROL_LABELS, NODE_TYPE_NAMES, PRESET_NAMES } from "./page-words";
import { useKnownAmbiguousReferences, useKnownReferenceNames } from "./reference-names";
import {
  knownIssueKeys,
  linkRowReferences,
  ROW_LINK,
  rowReferences,
  rowReferenceWords,
} from "./row-references";

/**
 * A node's own text, as its row in the outline draws it (R16, R20, R34),
 * in the size and the colour of the words it is drawn among.
 *
 * A text whose read sent the server's reading of it (`tree`) is drawn from that reading, with
 * the renderer every other surface draws a stored text with (`MarkdownBody`): a heading, bold
 * words, a list and a code span are drawn as what they are, never as the `#`, `**`, `-` and
 * backticks the author typed, and the browser parses nothing to draw them. A text whose read
 * sent none is drawn as stored, because the only other way to format it would be to parse it
 * here, which R20 refuses.
 *
 * Either way a reference in the words - a `dispatch://` reference, a dashboard address, the key
 * of an issue that exists - is a link to its target, named from the page's one batched read of
 * what its references point at (`row-references.ts`): an issue by its key and title, and a block
 * or a document as a chip in place, named as a tie's chip is, in plain words (`chipName`, part
 * G's R4). The page publishes that read's names as plain words already (`useIssueTree.ts`), so
 * they are drawn with no markdown pass (decision 66). A short reference that two or more targets
 * answer to names no one of them: the page publishes it as ambiguous, apart from the names
 * (`useKnownAmbiguousReferences`), and it is drawn as its author wrote it, with no link
 * (decisions 62 and 89). A reference the read named nothing for and did not read as ambiguous -
 * a gone one, or one the read has not answered yet - keeps its link, by its short form. The row
 * asks the server nothing for it (R21), so the body's own link pass, which reads one target for
 * each reference, is given nothing to link.
 *
 * The words are a reading of the text, as it was when it was drawn as stored: an `ask` block in a
 * document is a node of the tree of its own, where a reader answers it, so the text draws its
 * question and options rather than a second card to answer it in (`TextAskCard`). Whatever the
 * reading draws that takes the focus - a link, a reference, a picture's control - is out of the
 * tab order, as every other control inside a row is: the tree is one tab stop, and a key
 * reaches a row's links instead (R9, `useTreeKeymap.ts`'s `rowStops`). A view that no tree's
 * keys reach - part V's Verification view and part J's Spec view - passes `tabStops`, which
 * keeps them in the tab order, the links a later pass makes included.
 */
export function RowText({
  focused = false,
  markdown,
  source,
  tabStops = false,
  tree,
}: {
  /** Whether the row drawing the text holds the focus: out of the tab order, its scrollers keep
   *  their tab stop, named, so the keyboard can scroll them (decision 317, `useOutOfTabOrder`'s
   *  `keepScrollers`). */
  focused?: boolean;
  /** The text as stored: drawn as it is where `tree` is absent. */
  markdown: string;
  /** The record the text is (`MarkdownBody`'s `source`). */
  source?: TextSource;
  /** Whether the controls inside the text keep their place in the tab order: false in the
   *  outline, whose keys reach them, and true for a view that no tree's keys reach. */
  tabStops?: boolean;
  /** The server's reading of `markdown`, where its read sent one. */
  tree?: DisplayTree;
}): ReactNode {
  // A text drawn as stored is one paragraph of words and links, which holds no scroller.
  return tree === undefined ? (
    <StoredText markdown={markdown} tabStops={tabStops} />
  ) : (
    <DrawnText
      focused={focused}
      markdown={markdown}
      source={source}
      tabStops={tabStops}
      tree={tree}
    />
  );
}

/**
 * The page's word for the whole document `route` names, or undefined where it names no whole
 * document (part G's R4, ruling 5): today's model holds no title for a document and its name is
 * its file name, which no chip draws, so a document is named by the page's word for one, and the
 * issue's spec by the Spec's name.
 */
export function documentWord(route: DispatchReferenceRoute | undefined): string | undefined {
  if (route === undefined) return undefined;
  if (route.kind === "spec") return route.block === undefined ? PRESET_NAMES.spec : undefined;
  const document =
    route.kind === "artifact" || route.kind === "document" || route.kind === "agent-artifact";
  return document && !("block" in route && route.block !== undefined)
    ? NODE_TYPE_NAMES.document
    : undefined;
}

/** The words a chip draws for the target `route` names, whose name the batched read gave as
 *  `name`: a whole document by the page's word for it (`documentWord`), and anything else, a
 *  block among them, by its first words (`chipWords`). The name is plain words as the page
 *  publishes it (`useIssueTree.ts`), drawn with no markdown pass; a typed block's source line
 *  that still stands in it is left out first. */
export function chipName(route: DispatchReferenceRoute | undefined, name: string): string {
  return documentWord(route) ?? chipWords(withoutDirectiveLines(name));
}

/** The names the batched read brought, with every reference but an issue's named as a tie's chip
 *  is (`chipName`). A document `markdown` cites is named by the page's word for it whether or not
 *  the read has answered yet, so its file name and its slug are never drawn while it is in
 *  flight. The read's own names are named once for each answer of the read (`namedOnce`), never
 *  once for each row: a block's name is its whole reading, and every row draws from the one map. */
function chipNames(
  names: ReadonlyMap<string, string>,
  markdown: string
): ReadonlyMap<string, string> {
  const shared = namedOnce(names);
  let named: Map<string, string> | undefined;
  for (const one of rowReferences(markdown, [])) {
    if (one.route.kind === "issue" || shared.has(one.reference) || named?.has(one.reference)) {
      continue;
    }
    const words = chipName(one.route, "");
    if (words === "") continue;
    named ??= new Map(shared);
    named.set(one.reference, words);
  }
  return named ?? shared;
}

/** Each answer of the batched read, its names named as a tie's chip names them, by the answer:
 *  the page publishes one map for each answer (`usePublishReferenceNames`), so every row of that
 *  answer reads the same names, and the same map. */
const CHIP_NAMES = new WeakMap<ReadonlyMap<string, string>, ReadonlyMap<string, string>>();

function namedOnce(names: ReadonlyMap<string, string>): ReadonlyMap<string, string> {
  const held = CHIP_NAMES.get(names);
  if (held !== undefined) return held;
  const named = new Map(names);
  for (const [reference, name] of names) {
    const route = parseNodeReference(reference);
    if (route !== undefined && route.kind !== "issue") named.set(reference, chipName(route, name));
  }
  CHIP_NAMES.set(names, named);
  return named;
}

/** The mark a reference in a node's words carries where it is drawn as a chip (`styles.css`). */
const ROW_CHIP = "data-row-chip";

/** A typed block's source line in a node's words: its opening `:::requirement{#r1}`, or its
 *  closing `:::`. It is markup and no word of the node (part G's R4, the tester's F20): a
 *  decision's text holds the lines of a block its section quotes, and both its stored text and
 *  its reading carry them as they stand. */
const DIRECTIVE_LINE = /^[ \t]*:{3,}(?:[a-z_][^\n]*)?[ \t]*$\n?/gmu;

/** `text` without a typed block's source lines, and with no blank line left where one stood:
 *  the rule every view that draws a node's words leaves those lines out by (decision 94). The
 *  pattern stays in this file, since its `g` flag carries `lastIndex` from one `test` or `exec`
 *  to the next; `replace` starts from the beginning each time. */
export function withoutDirectiveLines(text: string): string {
  return text.includes(":::") ? text.replace(DIRECTIVE_LINE, "") : text;
}

/** Takes a typed block's source lines out of the words a reading painted into `root`, as the
 *  row's link pass edits them (`linkRowReferences`): every text outside code, and a paragraph
 *  that held nothing else is drawn as none. Run again after every paint; it finds nothing to do
 *  twice. */
function dropDirectiveLines(root: HTMLElement): void {
  const walker = document.createTreeWalker(root, NodeFilter.SHOW_TEXT);
  const texts: Text[] = [];
  for (let node = walker.nextNode(); node !== null; node = walker.nextNode()) {
    if (
      node instanceof Text &&
      node.data.includes(":::") &&
      node.parentElement?.closest("code, pre") == null
    ) {
      texts.push(node);
    }
  }
  for (const text of texts) {
    const kept = withoutDirectiveLines(text.data);
    if (kept === text.data) continue;
    text.data = kept;
    const block = text.parentElement?.closest("p");
    if (block instanceof HTMLElement && (block.textContent ?? "").trim() === "")
      block.hidden = true;
  }
}

/** A text drawn as stored, each reference in it the link the batched read names, but for one
 *  the read answered as ambiguous, which stays as written, and no typed block's source line in
 *  it. */
function StoredText({
  markdown: stored,
  tabStops,
}: {
  markdown: string;
  tabStops: boolean;
}): ReactNode {
  const markdown = useMemo(() => withoutDirectiveLines(stored), [stored]);
  const read = useKnownReferenceNames();
  const ambiguous = useKnownAmbiguousReferences();
  const names = useMemo(() => chipNames(read, markdown), [markdown, read]);
  const found = useMemo(
    () => rowReferences(markdown, knownIssueKeys(names), ambiguous),
    [ambiguous, markdown, names]
  );
  const follow = useFollowInPlace();
  let at = 0;
  return (
    // biome-ignore lint/a11y/useKeyWithClickEvents: a link's Enter is a click, which this follows.
    <p className="whitespace-pre-wrap break-words" onClick={follow} {...authorText}>
      {found.map((one) => {
        const before = markdown.slice(at, one.start);
        at = one.end;
        const { key, words } = rowReferenceWords(one, names);
        return (
          <Fragment key={`${one.start}:${one.reference}`}>
            {before}
            <a
              className={ROW_LINK}
              data-row-chip={one.route.kind === "issue" ? undefined : ""}
              data-row-reference={one.reference}
              href={one.path}
              tabIndex={tabStops ? undefined : -1}
            >
              {key === undefined ? (
                words
              ) : (
                <>
                  <span className="font-semibold whitespace-nowrap">{key}</span>
                  {words === "" ? "" : ` ${words}`}
                </>
              )}
            </a>
          </Fragment>
        );
      })}
      {markdown.slice(at)}
    </p>
  );
}

/**
 * Follows an address of this application in a node's words in place, on a plain click, as a link
 * the router draws does: the words are drawn as HTML (`MarkdownBody`, `linkRowReferences`), and
 * their links would load the whole page again. A click with a key held or with another button, a
 * link to a download, to another window, to another origin or to a path the server answers
 * itself (`SERVER_ROOT`), and a click a picture already answered (`DispatchPicture`) are the
 * browser's. A link to the address the reader is at replaces it, as the router's `Link` does, so
 * Back leaves the page as it does from a tie's chip to that address.
 */
function useFollowInPlace(): (event: MouseEvent<HTMLElement>) => void {
  const navigate = useNavigate();
  const here = useLocation();
  const current = `${here.pathname}${here.search}${here.hash}`;
  return useCallback(
    (event: MouseEvent<HTMLElement>) => {
      if (event.defaultPrevented || !isPlainClick(event.nativeEvent)) return;
      const anchor = event.target instanceof Element ? event.target.closest("a[href]") : null;
      if (!(anchor instanceof HTMLAnchorElement) || !event.currentTarget.contains(anchor)) return;
      if (anchor.hasAttribute("download") || anchor.getAttribute("target") !== null) return;
      const url = new URL(anchor.href, window.location.href);
      if (url.origin !== window.location.origin || SERVER_ROOT.test(url.pathname)) return;
      event.preventDefault();
      const path = `${url.pathname}${url.search}${url.hash}`;
      navigate(path, { replace: path === current });
    },
    [current, navigate]
  );
}

/** The path roots the server answers itself and the page has no route for: its API, sign-in,
 *  sockets, health and built assets (`serverRoots` and `assetRoots` of
 *  `packages/envoy/internal/dispatch/routes/router.go`). */
const SERVER_ROOT = /^\/(?:api|v1|auth|ws|healthz|assets)(?:\/|$)/u;

/** What takes the focus in a drawn text: every control a reading can hold. */
export const FOCUSABLE = "a[href], area[href], button, input, select, textarea, iframe, [tabindex]";

/** Whether `element`'s overflow lets it scroll its own content, on either axis. Chromium makes
 *  such an element a tab stop of its own once its content runs past its box, while no control
 *  inside it is in the tab order: a wide code block's `pre`, a table's scrolling wrapper. */
function scrolls(element: Element): boolean {
  const { overflowX, overflowY } = getComputedStyle(element);
  return [overflowX, overflowY].some((overflow) => overflow === "auto" || overflow === "scroll");
}

/** The class `mermaid-diagrams.ts`'s `drawBlock` gives a drawn diagram's preview, which scrolls
 *  sideways when the diagram is wider than the words (`styles.css`). */
const DIAGRAM_PREVIEW = "dispatch-mermaid-preview";

/** The mark a scroller carries while `useOutOfTabOrder` keeps its tab stop, so that a tree's keys
 *  can tell a kept scroller from the row's other controls. Its reader is part I's tree keys,
 *  which leave a key pressed on a kept scroller, or inside one, to the browser (decision 322);
 *  nothing in part G reads it. */
const KEPT_SCROLLER = "data-kept-scroller";

/** The name a scroller carries while it keeps its tab stop, by what it scrolls: a code block; a
 *  table, itself or the one its wrapper holds; a drawn diagram; and anything else by the page's
 *  word for content, with no direction, since it can scroll down as well as sideways. */
function scrollerName(scroller: HTMLElement): string {
  if (scroller.localName === "pre") return CONTROL_LABELS.scrolls_code;
  if (scroller.localName === "table" || scroller.querySelector(":scope > table") !== null) {
    return CONTROL_LABELS.scrolls_table;
  }
  if (scroller.classList.contains(DIAGRAM_PREVIEW)) return CONTROL_LABELS.scrolls_diagram;
  return CONTROL_LABELS.scrolls_content;
}

/**
 * Keeps every control inside `box` out of the tab order while `out` holds: the ones it is drawn
 * with, and the ones that arrive after - a reference's link once its target answers, a picture
 * once it loads - as they arrive. An element that scrolls its own content is one too, since
 * Chromium gives it a tab stop (`scrolls`). The outline is one tab stop and its keys reach a
 * row's controls (R9); a view that no tree's keys reach passes `out` false and leaves them where
 * they are.
 *
 * `keepScrollers` keeps in the tab order every scroller whose content runs past its box, named
 * by what it scrolls (`scrollerName`), so a reader on the keyboard can scroll it sideways: the
 * focused row's (decision 317). A scroller whose content fits its box - a one-line code block, a
 * narrow table - has nowhere to scroll and Chromium gives it no tab stop, so it takes none here
 * either: it leaves the tab order, as every scroller does without `keepScrollers`. Each scroller
 * is judged again on every change of its width (one `ResizeObserver`), which changes no child of
 * the box, so a block that starts to overflow when the window narrows gains its tab stop, and one
 * that stops loses it. A scroller other than a table takes a name only with a role (`region`); a
 * table has its own. When it leaves the tab order again - its content fits, or `keepScrollers`
 * turns off - the name and role it was given go back to what they were.
 * While it keeps its tab stop, a scroller carries `data-kept-scroller` (`KEPT_SCROLLER`); it
 * loses the mark whenever it leaves the tab order or the hook lets it go.
 */
export function useOutOfTabOrder(
  box: RefObject<HTMLElement | null>,
  out: boolean,
  { keepScrollers = false }: { readonly keepScrollers?: boolean } = {}
): void {
  useLayoutEffect(() => {
    const element = box.current;
    if (!out || element === null) return;
    // Each scroller this run named, with its own name and role as they stood before.
    const named = new Map<HTMLElement, { label: string | null; role: string | null }>();
    // Takes the mark off a scroller, and gives one this run named its own name and role back.
    const giveBack = (scroller: HTMLElement): void => {
      scroller.removeAttribute(KEPT_SCROLLER);
      const before = named.get(scroller);
      if (before === undefined) return;
      named.delete(scroller);
      for (const [attribute, value] of [
        ["aria-label", before.label],
        ["role", before.role],
      ] as const) {
        if (value === null) scroller.removeAttribute(attribute);
        else scroller.setAttribute(attribute, value);
      }
    };
    const untab = (): void => {
      // Every style and every size is read before any tab index is written, so the reads cost
      // one style pass and one layout.
      const scrollers = [...element.querySelectorAll<HTMLElement>("*")].filter(scrolls);
      const kept = new Map<HTMLElement, string>(
        keepScrollers
          ? scrollers
              .filter(
                (scroller) =>
                  scroller.scrollWidth > scroller.clientWidth ||
                  scroller.scrollHeight > scroller.clientHeight
              )
              .map((scroller) => [scroller, scrollerName(scroller)] as const)
          : []
      );
      for (const control of element.querySelectorAll<HTMLElement>(FOCUSABLE)) {
        if (!kept.has(control) && control.tabIndex !== -1) control.tabIndex = -1;
      }
      // An element that is no control reads `tabIndex` -1 with no `tabindex` of its own, and a
      // scroller can too, so a scroller is read by its attribute.
      for (const scroller of scrollers) {
        const name = kept.get(scroller);
        if (name === undefined) {
          if (scroller.getAttribute("tabindex") !== "-1") scroller.tabIndex = -1;
          giveBack(scroller);
          continue;
        }
        if (!named.has(scroller)) {
          named.set(scroller, {
            label: scroller.getAttribute("aria-label"),
            role: scroller.getAttribute("role"),
          });
        }
        if (scroller.getAttribute("tabindex") !== "0") scroller.tabIndex = 0;
        if (!scroller.hasAttribute(KEPT_SCROLLER)) scroller.setAttribute(KEPT_SCROLLER, "");
        if (scroller.getAttribute("aria-label") !== name) scroller.setAttribute("aria-label", name);
        if (scroller.localName !== "table" && scroller.getAttribute("role") !== "region") {
          scroller.setAttribute("role", "region");
        }
      }
      // Watches each scroller of the box once, since watching one again reports it again, and
      // lets go of one that has left the box.
      if (sizes === undefined) return;
      for (const scroller of sized) {
        if (element.contains(scroller)) continue;
        sizes.unobserve(scroller);
        sized.delete(scroller);
      }
      for (const scroller of scrollers) {
        if (sized.has(scroller)) continue;
        sizes.observe(scroller);
        sized.add(scroller);
      }
    };
    // With `keepScrollers` a scroller's tab stop rests on whether its content runs past its box,
    // which a change of its width can turn either way with no change to the box's children: the
    // pass runs again when a scroller changes size.
    const sized = new Set<HTMLElement>();
    const sizes =
      keepScrollers && typeof ResizeObserver !== "undefined"
        ? new ResizeObserver(untab)
        : undefined;
    untab();
    const watching = new MutationObserver(untab);
    watching.observe(element, { childList: true, subtree: true });
    return () => {
      watching.disconnect();
      sizes?.disconnect();
      for (const scroller of named.keys()) giveBack(scroller);
    };
  }, [box, keepScrollers, out]);
}

/** A text drawn from its reading, with every control it holds taken out of the tab order unless
 *  `tabStops` holds: the ones it is drawn with, and the ones the reading puts in after - a
 *  reference's link once its target answers, a picture once it loads - as they arrive. */
function DrawnText({
  focused,
  markdown,
  source,
  tabStops,
  tree,
}: {
  focused: boolean;
  markdown: string;
  source: TextSource | undefined;
  tabStops: boolean;
  tree: DisplayTree;
}): ReactNode {
  const drawn = useRef<HTMLDivElement>(null);
  const read = useKnownReferenceNames();
  const ambiguous = useKnownAmbiguousReferences();
  const names = useMemo(() => chipNames(read, markdown), [markdown, read]);
  // After every paint of the reading, and again when the batched read brings the names. The
  // row's pass takes its links out of the tab order as it makes them; a view with tab stops
  // puts them back.
  const link = useCallback(() => {
    const element = drawn.current;
    if (element === null) return;
    dropDirectiveLines(element);
    linkRowReferences(element, names, ambiguous);
    for (const anchor of element.querySelectorAll<HTMLAnchorElement>("a[data-row-reference]")) {
      const reference = anchor.getAttribute("data-row-reference") ?? "";
      const route = parseNodeReference(reference);
      if (route !== undefined && route.kind !== "issue") {
        anchor.setAttribute(ROW_CHIP, "");
        // A whole document is named by the page's word for one, as `chipNames` names it, also
        // where only the reading cites it: a statement's stored words leave its references out.
        const word = documentWord(route);
        if (word !== undefined && anchor.textContent !== word) anchor.textContent = word;
      }
      if (tabStops) anchor.removeAttribute("tabindex");
    }
  }, [ambiguous, names, tabStops]);
  useLayoutEffect(link, [link]);
  useOutOfTabOrder(drawn, !tabStops, { keepScrollers: focused });
  const follow = useFollowInPlace();
  return (
    // biome-ignore lint/a11y/noStaticElementInteractions: the press is the links' own, followed in place.
    // biome-ignore lint/a11y/useKeyWithClickEvents: a link's Enter is a click, which this follows.
    <div className="min-w-0 break-words" onClick={follow} ref={drawn} {...authorText}>
      <TextAskCard.Provider value={undefined}>
        {/* The body's own link pass is held to the references of `referenceText`: none, so it
            reads no target, and the row's pass links what the words cite (`linkRowReferences`). */}
        <MarkdownBody
          markdown={markdown}
          onRendered={link}
          referenceText=""
          source={source}
          tree={tree}
        />
      </TextAskCard.Provider>
    </div>
  );
}
