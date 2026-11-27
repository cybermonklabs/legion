import {
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
import { useAgentText } from "../refs/AgentName";
import { MarkdownBody } from "../refs/MarkdownBody";
import { TextAskCard } from "../refs/markdown-cards";
import { useLoadedEngine } from "../refs/parsed-references";
import { isPlainClick } from "../refs/picture-lightbox";
import { useReferenceNames } from "../refs/plain-text";
import { useKnownReferences } from "../refs/reference-names";
import { parseNodeReference } from "../refs/routes";
import type { TextSource } from "../refs/text-references";
import { wordsOf } from "../refs/tree-fragment";
import { documentWord } from "./node-row-chip";
import { authorText } from "./page-structure";
import { CONTROL_LABELS } from "./page-words";
import {
  knownIssueKeys,
  linkRowReferences,
  rowReferenceNames,
  rowReferences,
  rowReferenceWords,
} from "./row-references";
import { titleReading } from "./statement-rules";

/**
 * A node's own text, as its row in the outline draws it (R16, R20, R34), and the words of the
 * answer an ask's row carries (`NodeRow.tsx`'s `AskAnswer`), in the size and the colour of the
 * words it is drawn among.
 *
 * A text whose read sent the server's reading of it (`tree`) is drawn from that reading, with
 * the renderer every other surface draws a stored text with (`MarkdownBody`): a heading, bold
 * words, a list and a code span are drawn as what they are, never as the `#`, `**`, `-` and
 * backticks the author typed, and the browser parses nothing to draw them. A text whose read
 * sent none - a server of the build before, during a rolling deploy - is parsed on the page by
 * the shared engine and drawn the same way from that parse (`ParsedText`), as the renderer's
 * other callers draw a text a Dispatch from before trees sent bare, so a row reads the same
 * whether or not its read sent a reading.
 *
 * Either way a reference in the words - a `dispatch://` reference, a dashboard address, the key
 * of an issue that exists - is a link to its target, and a person words with no link (R5), named
 * by the one rule every plain line of a record names a reference by (`useRowNames`): from the
 * page's one batched read of what its references point at, which the page publishes
 * (`refs/reference-names.tsx`), and a party by its own read. An issue reads as its key and
 * title, and a block or a document as a chip in place, named as a tie's chip is, in the plain
 * words the shared projection reads a name to (`tree-drafts.ts`'s `treeContext`, part G's R4),
 * so a numbered step keeps its number (decisions 66 and 88). A short reference that two or more
 * targets answer to names no one of them: the page publishes it as ambiguous, apart from the
 * names (`KnownReferences.ambiguous`), and it is drawn as its author wrote it, with no link
 * (decisions 62 and 89). A reference with no name yet - one the read has not answered, a gone
 * one - keeps its link and reads as the words its author wrote (R8, issue page decision 350).
 * The row asks the server nothing for a reference the page reads (R21), so the body's own link
 * pass, which reads one target for each reference, is given nothing to link.
 *
 * A reference to a post its author corrected reads as the page published it (INFRA-1523, R2 and
 * R3): a post its author edited by its words, cut, then ` · edited` (`rowReferenceWords`,
 * `KnownReferences.edited`); one its author deleted by the deleted line without its time, in
 * the deleted line's italic as a link to it is (`RefLink`), its actor named as the page names an
 * author (`AgentText.author`), as the row's own deleted line names it (`node-line.tsx`'s
 * `DeletedLine`), by issue page decision 342, and not as the published name holds it
 * (`KnownReferences.deleted`).
 *
 * The words are a reading of the text: an `ask` block in a document is a node of the tree of its
 * own, where a reader answers it, so the text draws its question and options rather than a
 * second card to answer it in (`TextAskCard`). Whatever the reading draws that takes the focus -
 * a link, a reference, a picture's control - is out of the tab order, as every other control
 * inside a row is: the tree is one tab stop, and a key reaches a row's links instead (R9,
 * `useTreeKeymap.ts`'s `rowStops`). A view that no tree's keys reach - part V's Verification
 * view and part J's Spec view - passes `tabStops`, which keeps them in the tab order, the links
 * a later pass makes included.
 */
export function RowText({
  focused = false,
  markdown,
  onRedrawn,
  source,
  tabStops = false,
  tree,
}: {
  /** Whether the row drawing the text holds the focus: out of the tab order, its scrollers keep
   *  their tab stop, named, so the keyboard can scroll them (decision 317, `useOutOfTabOrder`'s
   *  `keepScrollers`). */
  focused?: boolean;
  /** The text as stored: parsed on the page where `tree` is absent. */
  markdown: string;
  /** Called once the words the text draws change after they were first drawn - a reference
   *  takes the name that came to it - so the row measures its cut again, as it does for words of
   *  another value (`NodeRow.tsx`'s `useClamp`). */
  onRedrawn?: () => void;
  /** The record the text is (`MarkdownBody`'s `source`). */
  source?: TextSource;
  /** Whether the controls inside the text keep their place in the tab order: false in the
   *  outline, whose keys reach them, and true for a view that no tree's keys reach. */
  tabStops?: boolean;
  /** The server's reading of `markdown`, where its read sent one. */
  tree?: DisplayTree;
}): ReactNode {
  return tree === undefined ? (
    <ParsedText
      focused={focused}
      markdown={markdown}
      onRedrawn={onRedrawn}
      source={source}
      tabStops={tabStops}
    />
  ) : (
    <DrawnText
      focused={focused}
      markdown={markdown}
      onRedrawn={onRedrawn}
      source={source}
      tabStops={tabStops}
      tree={tree}
    />
  );
}

/** A text whose read sent no reading of it, parsed on the page by the shared engine
 *  (`MarkdownEngine.parse`) into the document the server's reading of it would be, and drawn
 *  from that parse as a text with a reading is (`DrawnText`). It is parsed in the render that
 *  draws it, however long it is, so it is drawn in the commit that mounts it, as a reading is.
 *  Nothing is drawn while the engine loads, so no mark the author typed shows raw meanwhile
 *  (`useLoadedEngine`). A text the parser refuses is drawn as one run of its characters
 *  (`titleReading`), and so is every text once the engine's load has failed, which the renderer
 *  then draws as it draws any reading with no engine: as the text's own characters
 *  (`MarkdownBody`). So no row's words are lost from the page. */
function ParsedText({
  focused,
  markdown,
  onRedrawn,
  source,
  tabStops,
}: {
  focused: boolean;
  markdown: string;
  onRedrawn: (() => void) | undefined;
  source: TextSource | undefined;
  tabStops: boolean;
}): ReactNode {
  const { engine, failed } = useLoadedEngine();
  const tree = useMemo(() => {
    if (engine === undefined) return failed ? titleReading(markdown) : undefined;
    return (engine.parse(markdown)?.toJSON() as DisplayTree | undefined) ?? titleReading(markdown);
  }, [engine, failed, markdown]);
  return tree === undefined ? null : (
    <DrawnText
      focused={focused}
      markdown={markdown}
      onRedrawn={onRedrawn}
      source={source}
      tabStops={tabStops}
      tree={tree}
    />
  );
}

/** What a row draws the references of its words by (`useRowNames`). */
interface RowNames {
  readonly ambiguous: ReadonlySet<string>;
  /** The references among `names` whose post its author deleted, which a row draws in the
   *  deleted line's italic (`KnownReferences.deleted`). */
  readonly deleted: ReadonlySet<string>;
  /** The words each reference is drawn in, as one key, which changes only where one of them
   *  does. */
  readonly drawn: string;
  /** The references whose post its author edited, which a row marks ` · edited` after the cut
   *  (`KnownReferences.edited`). */
  readonly edited: ReadonlySet<string>;
  /** The keys the page's batched read named an issue for: a row links one where it stands bare. */
  readonly issues: readonly string[];
  /** The name each reference is drawn by, an issue's title apart from its key, and a block's
   *  first block of words, which a chip in place names it by (`rowReferenceWords`). */
  readonly names: ReadonlyMap<string, string>;
  /** The reads of the references the page reads no name for, which draw nothing. */
  readonly reads: ReactNode;
}

/**
 * What a row draws each reference in `words` as, by the one rule every plain line of a record
 * names a reference by (`useReferenceNames`), so a node's one line and its row draw one
 * reference in the same words: the name the page published from its batched read, and a party's
 * own read, by the issue the record is on (`issue`), where a role is read. The page's names are
 * read once for each of its answers and shared by every row. An issue's name is its title, which
 * a row draws apart from its key. A block and a whole document are drawn as chips in place,
 * named as a tie's chip names them, by the line the page's store projects beside the name
 * (`KnownReferences.lines`): a block by the first block of its words that holds any, and a
 * document by the page's word for it alone, since a chip never draws a document's name, which a
 * link and every plain line read it by (decision 39). Until the store has that line, a document
 * is drawn by the word the store's rule gives it by its address and `issue`
 * (`rowReferenceWords`).
 */
function useRowNames(words: string, issue: string | undefined): RowNames {
  const { ambiguous, deleted: deletions, edited, lines, names: known } = useKnownReferences();
  const { author } = useAgentText();
  const issues = useMemo(() => knownIssueKeys(known), [known]);
  const found = useMemo(() => rowReferences(words, issues, ambiguous), [ambiguous, issues, words]);
  // A document's own read would answer only the name its chip never draws.
  const addresses = useMemo(
    () => [
      ...new Set(
        found
          .filter((one) => documentWord(one.route, issue) === undefined)
          .map((one) => one.reference)
      ),
    ],
    [found, issue]
  );
  const { names: named, reads } = useReferenceNames(addresses, issue);
  // A post the page read as deleted is named by its deleted line, the actor named by the page's
  // rule for an author, in place of the published name, which names the actor as no registry
  // knows it (`rowReferenceNames`).
  const names = useMemo(
    () =>
      rowReferenceNames(
        found,
        { deleted: deletions, lines },
        (reference) => named.get(reference)?.name,
        issue,
        author
      ),
    [author, deletions, found, issue, lines, named]
  );
  const deleted = useMemo(
    () => new Set(found.map((one) => one.reference).filter((one) => deletions.has(one))),
    [deletions, found]
  );
  const drawn = useMemo(
    () =>
      found
        .map((one) => {
          const { key, words: name } = rowReferenceWords(one, names, issue, edited);
          return `${key ?? ""} ${name}`;
        })
        .join("\n"),
    [edited, found, issue, names]
  );
  return { ambiguous, deleted, drawn, edited, issues, names, reads };
}

/** Calls `onRedrawn` after each commit, past the first, in which `drawn`, the words a row's
 *  references are drawn in (`RowNames.drawn`), changed: a reference took the name that came to
 *  it. Words drawn again as they were - a later read's names in new objects - call nothing, so a
 *  row held whole in view keeps its length (decision 261). */
function useRedrawn(drawn: string, onRedrawn: (() => void) | undefined): void {
  const first = useRef(true);
  const called = useRef(onRedrawn);
  called.current = onRedrawn;
  // biome-ignore lint/correctness/useExhaustiveDependencies: what changed is `drawn`, and `onRedrawn` is read from its ref.
  useLayoutEffect(() => {
    if (first.current) {
      first.current = false;
      return;
    }
    called.current?.();
  }, [drawn]);
}

/** The mark a reference in a node's words carries where it is drawn as a chip (`styles.css`). */
const ROW_CHIP = "data-row-chip";

/** The mark the deleted line carries wherever it is drawn (`DeletedBody`, `RefLink`). */
const POST_DELETED = "data-post-deleted";

/** The mark of what a node's words draw in place of the text its editor holds there, which holds
 *  the editor's text for it: a reference's words as its author wrote them (the mark of the same
 *  name `row-references.ts` keeps private, which the reference pass sets), and nothing for what
 *  the editor holds as an atom with no text a caret can enter. A press's count reads this mark and
 *  no list of shapes (`spec-filter/pressed-words.ts`), so a kind the words draw in a new way needs
 *  only its mark. */
export const ROW_WRITTEN = "data-row-written";

/** What the words draw an editor's atom as: inline HTML and a footnote's reference by the schema's
 *  own `toDOM` (Milkdown's `span[data-type="html"]` and `sup[data-type="footnote_reference"]`),
 *  and a picture of an artifact as a link holding the picture, or its caption where it has not
 *  loaded (`markdown-serializer.ts`'s `pictureSerializer`, `DispatchPicture`). */
const DRAWN_ATOMS = '[data-type="html"], [data-type="footnote_reference"], [data-dispatch-picture]';

/** Marks with `ROW_WRITTEN` each of `DRAWN_ATOMS` the words painted into `root` draw, in place of
 *  an atom of the editor's, with no text of the editor's. A `[^label]` the words draw as text -
 *  a call the document defines no note for, an escaped one - is text the editor holds too, and
 *  is left as drawn. Run again after every paint; it finds nothing to do twice. */
function markDrawnAtoms(root: HTMLElement): void {
  for (const atom of root.querySelectorAll(DRAWN_ATOMS)) {
    if (!atom.hasAttribute(ROW_WRITTEN)) atom.setAttribute(ROW_WRITTEN, "");
  }
}

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

/** The class `mermaid-diagrams.ts`'s `drawBlock` gives a drawn diagram's preview, which scrolls
 *  sideways when the diagram is wider than the words (`styles.css`). */
const DIAGRAM_PREVIEW = "dispatch-mermaid-preview";

/** Every element in drawn words that can scroll its own content: the ones `styles.css` gives an
 *  `overflow-x: auto` - a code block's `pre` and a table (in a node's words, a document's reading,
 *  an ask block's footer), and a drawn diagram's preview (`DIAGRAM_PREVIEW`) - and one whose own
 *  `style` sets its overflow. A row's words hold hundreds of elements, and a style read for each
 *  of them cost seconds of a large issue's load, so `useOutOfTabOrder` reads the style only of
 *  these. */
const SCROLLABLE = `pre, table, .${DIAGRAM_PREVIEW}, [style*="overflow"]`;

/** Whether `element`'s overflow lets it scroll its own content, on either axis. Chromium makes
 *  such an element a tab stop of its own once its content runs past its box, while no control
 *  inside it is in the tab order: a wide code block's `pre`, a table's scrolling wrapper. */
function scrolls(element: Element): boolean {
  const { overflowX, overflowY } = getComputedStyle(element);
  return [overflowX, overflowY].some((overflow) => overflow === "auto" || overflow === "scroll");
}

/** The mark a scroller carries while `useOutOfTabOrder` keeps its tab stop, so that a tree's keys
 *  can tell a kept scroller from the row's other controls. Its reader is part I's tree keys,
 *  which leave a key pressed on a kept scroller, or inside one, to the browser (decision 322);
 *  nothing in part G reads it. */
const KEPT_SCROLLER = "data-kept-scroller";

/** The name a scroller carries while it keeps its tab stop, by what it scrolls: a code block; a
 *  table, itself or the one its wrapper holds; a drawn diagram; and anything else by the page's
 *  word for content. */
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
 * either: it leaves the tab order, as every scroller does without `keepScrollers`. Whether a
 * scroller overflows is judged again whenever a scroller's size or the box's width changes (one
 * `ResizeObserver`), which changes no child of the box, so a block that starts to overflow when
 * the window narrows gains its tab stop, and one that stops loses it. A scroller other than a
 * table takes a name only with a role (`region`); a table has its own. When it leaves the tab
 * order again - its content fits, or `keepScrollers` turns off - the name and role it was given
 * go back to what they were.
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
    // With `keepScrollers` a scroller's tab stop rests on whether its content runs past its box,
    // which a change of its size, or of the box's width, can turn either way with no change to the
    // box's children: the pass runs again on either.
    const sized = new Set<HTMLElement>();
    const sizes =
      keepScrollers && typeof ResizeObserver !== "undefined"
        ? new ResizeObserver(() => untab())
        : undefined;
    const untab = (): void => {
      // Every style and every size is read before any tab index is written, so the reads cost
      // one style pass and one layout.
      const scrollers = [...element.querySelectorAll<HTMLElement>(SCROLLABLE)].filter(scrolls);
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
    sizes?.observe(element);
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
 *  reference's link once its target answers, a picture once it loads - as they arrive. A
 *  reference to a corrected post carries its mark (`RowText`). */
function DrawnText({
  focused,
  markdown,
  onRedrawn,
  source,
  tabStops,
  tree,
}: {
  focused: boolean;
  markdown: string;
  onRedrawn: (() => void) | undefined;
  source: TextSource | undefined;
  tabStops: boolean;
  tree: DisplayTree;
}): ReactNode {
  const box = useRef<HTMLDivElement>(null);
  // What the reading's words cite, read from the reading itself: a block's stored text is an
  // excerpt, or plain words with its references left out (`TreeNode.text`).
  const words = useMemo(() => wordsOf(tree), [tree]);
  const issue = source?.issue;
  const { ambiguous, deleted, drawn, edited, issues, names, reads } = useRowNames(words, issue);
  // After every paint of the reading, and again when the references take their names. The
  // row's pass takes its links out of the tab order as it makes them; a view with tab stops
  // puts them back.
  const link = useCallback(() => {
    const element = box.current;
    if (element === null) return;
    dropDirectiveLines(element);
    linkRowReferences(element, names, issues, issue, ambiguous, edited);
    markDrawnAtoms(element);
    for (const anchor of element.querySelectorAll<HTMLAnchorElement>("a[data-row-reference]")) {
      const reference = anchor.getAttribute("data-row-reference") ?? "";
      const route = parseNodeReference(reference);
      if (route !== undefined && route.kind !== "issue") anchor.setAttribute(ROW_CHIP, "");
      // A deleted post's line is in the deleted line's italic, as a link to it is (`RefLink`).
      if (deleted.has(reference)) {
        anchor.classList.add("italic");
        anchor.setAttribute(POST_DELETED, "");
      } else if (anchor.hasAttribute(POST_DELETED)) {
        anchor.classList.remove("italic");
        anchor.removeAttribute(POST_DELETED);
      }
      if (tabStops) anchor.removeAttribute("tabindex");
    }
  }, [ambiguous, deleted, edited, issue, issues, names, tabStops]);
  useLayoutEffect(link, [link]);
  useRedrawn(drawn, onRedrawn);
  useOutOfTabOrder(box, !tabStops, { keepScrollers: focused });
  const follow = useFollowInPlace();
  return (
    // biome-ignore lint/a11y/noStaticElementInteractions: the press is the links' own, followed in place.
    // biome-ignore lint/a11y/useKeyWithClickEvents: a link's Enter is a click, which this follows.
    <div className="min-w-0 break-words" onClick={follow} ref={box} {...authorText}>
      {reads}
      <TextAskCard.Provider value={undefined}>
        {/* The body's own link pass is held to the references of `referenceText`: none, so it
            reads no target, and the row's pass links what the words cite (`linkRowReferences`). */}
        <MarkdownBody
          markdown={markdown}
          onRendered={link}
          referenceText=""
          source={source}
          surface="node-row-words"
          tree={tree}
        />
      </TextAskCard.Provider>
    </div>
  );
}
