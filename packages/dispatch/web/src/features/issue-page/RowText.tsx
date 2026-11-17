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
import { NODE_TYPE_NAMES, PRESET_NAMES } from "./page-words";
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
  markdown,
  source,
  tabStops = false,
  tree,
}: {
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
  return tree === undefined ? (
    <StoredText markdown={markdown} tabStops={tabStops} />
  ) : (
    <DrawnText markdown={markdown} source={source} tabStops={tabStops} tree={tree} />
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

/**
 * Keeps every control inside `box` out of the tab order while `out` holds: the ones it is drawn
 * with, and the ones that arrive after - a reference's link once its target answers, a picture
 * once it loads - as they arrive. The outline is one tab stop and its keys reach a row's controls
 * (R9); a view that no tree's keys reach passes `out` false and leaves them where they are.
 */
export function useOutOfTabOrder(box: RefObject<HTMLElement | null>, out: boolean): void {
  useLayoutEffect(() => {
    const element = box.current;
    if (!out || element === null) return;
    const untab = (): void => {
      for (const control of element.querySelectorAll<HTMLElement>(FOCUSABLE)) {
        if (control.tabIndex !== -1) control.tabIndex = -1;
      }
    };
    untab();
    const watching = new MutationObserver(untab);
    watching.observe(element, { childList: true, subtree: true });
    return () => watching.disconnect();
  }, [box, out]);
}

/** A text drawn from its reading, with every control it holds taken out of the tab order unless
 *  `tabStops` holds: the ones it is drawn with, and the ones the reading puts in after - a
 *  reference's link once its target answers, a picture once it loads - as they arrive. */
function DrawnText({
  markdown,
  source,
  tabStops,
  tree,
}: {
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
  useOutOfTabOrder(drawn, !tabStops);
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
