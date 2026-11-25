import { type RefObject, useLayoutEffect, useMemo, useState } from "react";

import type { IssueDetails } from "../../api/types";
import { shortForm } from "../refs/routes";
import { wordsOf } from "../refs/tree-fragment";
import { drawnLine, firstLineOf, type TreeNode } from "./issue-tree";
import { chipWords } from "./node-row-chip";
import { NODE_TYPE_NAMES, type NodeType } from "./page-words";
import { withoutDirectiveLines } from "./RowText";
import { rowReferences } from "./row-references";
import { treeContext } from "./tree-drafts";

/**
 * What the Contents pane reads once for every entry it draws on one line (R1, R14): how wide its
 * list is, and what an entry needs to say how wide its own words are drawn. The outline's rows
 * draw their words whole; an entry of the list is one of the places the page draws one line for
 * a node.
 *
 * An entry cuts its node's first line at a word to the room its own box leaves it, and no entry
 * may read its own layout to learn that room: that is a layout read per entry, and again on
 * every resize. So the list reads one width - its own, which is every entry's before its indent
 * - with one `ResizeObserver` (`useOneLineBox`), and each entry sums its own parts against it:
 * the parts its classes size in pixels, and its words, measured on a canvas in the face the
 * list sets them in, which lays out nothing on the page.
 */
export interface OneLineBox {
  /** The face the tree's words are set in, as the tree's computed `font-family`. */
  readonly family: string;
  /** How many CSS pixels one `rem` is, which every part of a row its classes size is in. */
  readonly rem: number;
  /** The tree's own width, in CSS pixels. */
  readonly width: number;
  /** How wide `text` is drawn at `size` rem in the tree's face, at `weight` where it is not the
   *  face's regular one, in CSS pixels. */
  readonly widthOf: (text: string, size: number, weight?: number) => number;
}

/**
 * The list's one reading of its own width, for its one-line entries: taken as the list is first
 * laid out, before it is painted, again whenever its box changes size, by one `ResizeObserver`
 * on the list and none on any entry, and again whenever the document's fonts finish loading. A
 * reading that says what it said before leaves the entries alone. Undefined where the page has
 * no canvas to measure words on, which is no browser this page runs in; an entry given none
 * draws its first line uncut.
 *
 * The fonts are read because `styles.css` loads Inter with `font-display: swap`: the list's
 * first frames are drawn in the fallback while Inter's file is on its way, and drawn again in
 * Inter once it arrives, and neither the family the list names nor its width changes then. An
 * entry cut in the first face would keep that cut: where that face is wider it leaves out a
 * word its box has room for, and where it is narrower its line runs past its box. So each
 * `loadingdone` - a face the document was loading has arrived, or failed - makes a new reading
 * whatever the width, and each entry measures its words again in the face that now draws them.
 */
export function useOneLineBox(tree: RefObject<HTMLElement | null>): OneLineBox | undefined {
  const [box, setBox] = useState<OneLineBox | undefined>(undefined);
  useLayoutEffect(() => {
    const element = tree.current;
    if (element === null) return;
    const context = document.createElement("canvas").getContext("2d");
    if (context === null) return;
    let width = element.getBoundingClientRect().width;
    // `faces`: the document's fonts changed, so the same family and width draw other widths.
    const read = (faces: boolean) => {
      const family = window.getComputedStyle(element).fontFamily;
      const rem = Number.parseFloat(window.getComputedStyle(document.documentElement).fontSize);
      if (!(width > 0 && rem > 0)) return;
      setBox((held) => {
        if (!faces && held?.width === width && held.rem === rem && held.family === family) {
          return held;
        }
        // Setting a canvas's font parses it, so a run of words in one size sets it once.
        let set = "";
        return {
          family,
          rem,
          width,
          widthOf: (text, size, weight) => {
            const font = `${weight === undefined ? "" : `${weight} `}${size * rem}px ${family}`;
            if (font !== set) {
              context.font = font;
              set = font;
            }
            return context.measureText(text).width;
          },
        };
      });
    };
    read(false);
    const fonts = "fonts" in document ? document.fonts : undefined;
    const loaded = () => read(true);
    fonts?.addEventListener("loadingdone", loaded);
    const observer =
      typeof ResizeObserver === "undefined"
        ? undefined
        : new ResizeObserver((entries) => {
            const entry = entries[entries.length - 1];
            if (entry === undefined) return;
            width = entry.contentRect.width;
            read(false);
          });
    observer?.observe(element);
    return () => {
      fonts?.removeEventListener("loadingdone", loaded);
      observer?.disconnect();
    };
  }, [tree]);
  return box;
}

/** What a one-line entry draws of its node's first line, and where it draws it. */
export interface OneLineCut {
  /** Whether the node's text carries on past `line`, which the entry draws its ellipsis for
   *  where the line ends inside a sentence (`drawnLine`). */
  readonly cut: boolean;
  readonly line: string;
  /** Whether `line` ends at its sentence's end (`firstLineOf`'s `sentence`), after which the
   *  entry draws no ellipsis (`drawnLine`); a line cut at a word inside it ends at none. */
  readonly sentence: boolean;
  /** Whether the line is drawn on a line of its own under the row's other words, because
   *  beside them it has less room than its first word takes. */
  readonly under: boolean;
  /** Whether even that line of its own holds less than the first word, so the word wraps
   *  inside itself rather than have its tail hidden. */
  readonly wraps: boolean;
}

/** How far a sum of a row's parts may be from the browser's own layout of them, in pixels: each
 *  box is laid out to a fraction of a pixel the canvas does not round to. */
const ROUNDING = 1;

/**
 * The node's first line as a one-line entry draws it: `firstLineOf`'s line, cut again at a word
 * to the pixels the entry's own box leaves it (R1). The line is the longest run of its own
 * words, measured as it is drawn (`drawnLine`: with its ellipsis where the text carries on past
 * a line that ends inside a sentence), that fits beside the entry's other parts. Where those
 * leave it less than its first word it goes on a line of its own under them, where it has the
 * whole width; a first word no such line holds - a token as long as `firstLineOf` lets one be -
 * is drawn whole, wrapping inside itself, since a box that cut its tail would cut inside a
 * word, which is the one thing R1 bans.
 *
 * `widthOf` is how wide a run of the line's words is drawn; `before` and `after` are the pixels
 * the entry's other parts take on either side of the line, the gaps to it included; `room` is
 * the width the entry's parts share.
 */
export function oneLineCut({
  after,
  before,
  room,
  text,
  widthOf,
}: {
  after: number;
  before: number;
  room: number;
  text: string;
  widthOf: (text: string) => number;
}): OneLineCut {
  const first = firstLineOf(text);
  if (first.line === "") return { ...first, under: false, wraps: false };
  const ends: number[] = [];
  for (let at = first.line.indexOf(" "); at > 0; at = first.line.indexOf(" ", at + 1)) {
    ends.push(at);
  }
  // The longest of the line's own words that fits `budget`. A run of more words is never
  // narrower than a run of fewer, so the runs are searched by halves; the whole line is read on
  // its own first, since it carries no ellipsis where the text ends with it or it is a whole
  // sentence. A run cut at a word inside the line ends inside its sentence (`sentence: false`),
  // so it is measured as `drawnLine` draws such a line: with its ellipsis, unless the author's
  // own ends it.
  const fitting = (
    budget: number
  ): { cut: boolean; line: string; sentence: boolean } | undefined => {
    if (widthOf(drawnLine(first)) <= budget) return first;
    let found: number | undefined;
    let low = 0;
    let high = ends.length - 1;
    while (low <= high) {
      const middle = Math.floor((low + high) / 2);
      const end = ends[middle] ?? 0;
      if (
        widthOf(drawnLine({ cut: true, line: first.line.slice(0, end), sentence: false })) <= budget
      ) {
        found = end;
        low = middle + 1;
      } else {
        high = middle - 1;
      }
    }
    return found === undefined
      ? undefined
      : { cut: true, line: first.line.slice(0, found), sentence: false };
  };
  const beside = fitting(room - before - after - ROUNDING);
  if (beside !== undefined) return { ...beside, under: false, wraps: false };
  const under = fitting(room - after - ROUNDING);
  if (under !== undefined) return { ...under, under: true, wraps: false };
  const word = ends[0];
  return word === undefined
    ? { ...first, under: true, wraps: true }
    : { cut: true, line: first.line.slice(0, word), sentence: false, under: true, wraps: true };
}

/** A text's words with each reference it cites unnamed while a line is cut from them, and the
 *  names put in after the cut (`referenceStandIns`). */
export interface ReferenceStandIns {
  /** A line cut from words `standIn` gave, with each reference in it named and each private-use
   *  character its author wrote drawn as written. */
  readonly named: (line: string) => string;
  /** `text` as the words a line is cut from: its typed blocks' source lines left out, and each
   *  reference it cites and each private-use character it holds stood in for by its token. */
  readonly standIn: (text: string) => string;
}

/** The first character a token is made of: the Basic Multilingual Plane's private-use
 *  characters, U+E000 to U+F8FF, which are no space, letter, digit or mark. */
const STAND_IN = 0xe000;

/** How many tokens one holder makes, one private-use character each: one for each reference it
 *  stands in for, and one for each private-use character its texts hold of their own. */
const STAND_INS = 0xf8ff - STAND_IN + 1;

/** One private-use character, which an author may write of their own: an icon pasted from a
 *  terminal or an icon font. */
const PRIVATE_USE = /[\ue000-\uf8ff]/gu;

/** One token in a line `named` is given: a run of one private-use character, with the ellipsis a
 *  cut drew right after it where that ellipsis ends the line. */
const TOKEN = /(([\ue000-\uf8ff])\2*)(…$)?/gu;

/** How a name ends that a line draws no ellipsis after: with an ellipsis of its own, after which
 *  `drawnLine` draws none. */
const NAME_END = /…$/u;

/** What a private-use character the author wrote is drawn as where its holder has no token left
 *  for it: U+FFFD, the replacement character. */
const NO_TOKEN = "\ufffd";

/**
 * How the page draws a plain line that cites references: with every reference named in plain
 * words, and named only after the line is cut. A plain line is no link, so an address, a block id
 * or a fence drawn in it is one a reader can neither follow nor read: a typed block's source
 * lines are markup and no word of the text, so they are left out, and each `dispatch://`
 * reference the row's reader finds (`rowReferences`) is named by the words for what it names -
 * an issue's key; else the page's name for it (`nameOf`, `useReferenceNames`); else its short
 * form, as a link to it reads until its title resolves (`shortForm`). A one-line place names a
 * reference by the page's name wherever the page knows one, and by its short form only where it
 * does not. So the words `standIn` is given hold each bare address as its author wrote it
 * (`lineWordsRaw`, and a person's quote's `firstBlockWordsRaw`), since one already written short
 * leaves it nothing to name.
 * A statement's name opens with its label where its spec numbers its statements ("R2. Every
 * address…"), so a cut made after the naming ended its author's sentence at that label, inside
 * the name.
 *
 * `standIn` stands in for each reference by a token as long as its name: one private-use
 * character of its own, repeated, so a token holds no space, letter, digit or sentence mark.
 * Every cut is made on those words - a sentence end, a leading label, `firstLineOf`'s limit, a
 * row's width - and `named` puts each name in the place of its token. So no cut falls inside a
 * name, and a name counts against `firstLineOf`'s 100 characters as the characters the line
 * draws of it, so the drawn line keeps to R1's limit. A limit that falls inside a token, which
 * only a token run into the words before it with no space can make, draws the whole name. One
 * holder may stand in for the references of several texts whose lines are read together, as a
 * summary's runs are; past the 6,400 tokens the private-use characters make, a reference is
 * named in its place, with each private-use character of its name stood in for as the author's
 * own are, so `named` reads none of them as a token.
 *
 * A private-use character the author wrote is stood in for as a reference is, by a token whose
 * name is that character, so `named` draws it as written and never as a reference's name. The
 * name is one character long, as the character is, so the line's length and its cuts are the
 * text's own. Every use of one character in the holder's texts takes the same token, and a run
 * of that token draws the character as many times as the run is long. Where the holder has no
 * token left for a character it has not met before, the character is drawn as U+FFFD, the
 * replacement character, since drawn as written it would be read as a token.
 *
 * A cut's ellipsis is decided on the line before the names go in (`drawnLine`), and a line cut
 * right after a reference ends in its token, not in its name's own last character. So `named`
 * decides again, by `drawnLine`'s own rule, on an ellipsis that ends the line right after a
 * token: where the name ends with an ellipsis of its own ("R2. Every address…", which
 * `chipWords` cut), the line draws no second after it. A name that ends with a sentence's mark
 * keeps the cut's ellipsis, since a line cut inside its sentence draws one whatever its last
 * character. An ellipsis anywhere else stays as written.
 *
 * A line no cut is made in is `named(standIn(text))`.
 */
export function referenceStandIns(
  nameOf: (reference: string) => string | undefined
): ReferenceStandIns {
  const names: string[] = [];
  // The token each private-use character the author wrote is stood in for by, one per character.
  const owned = new Map<string, string>();
  // The holder's next token, one private-use character, which `named` draws as `name`.
  const tokenOf = (name: string): string => {
    const token = String.fromCharCode(STAND_IN + names.length);
    names.push(name);
    return token;
  };
  // `words` with each private-use character the author wrote stood in for by its token.
  const ownStandIns = (words: string): string =>
    words.replace(PRIVATE_USE, (character) => {
      let token = owned.get(character);
      if (token === undefined && names.length < STAND_INS) {
        token = tokenOf(character);
        owned.set(character, token);
      }
      return token ?? NO_TOKEN;
    });
  return {
    named: (line) =>
      names.length === 0
        ? line
        : line.replace(TOKEN, (whole: string, token: string, character: string, cut?: string) => {
            const name = names[character.charCodeAt(0) - STAND_IN];
            if (name === undefined) return whole;
            // A reference's token stands in a line once, whole or cut inside it, and draws its
            // whole name once; an author's character's token is one character long, and a run
            // of it draws the character as many times as the run is long.
            const drawn = name.repeat(Math.ceil(token.length / name.length));
            return cut === undefined || NAME_END.test(name) ? drawn : `${drawn}${cut}`;
          }),
    standIn: (text) => {
      const words = withoutDirectiveLines(text);
      let held = "";
      let at = 0;
      for (const one of rowReferences(words, [])) {
        // The author's words before the reference are stood in for first, so the holder's tokens
        // are taken in the order the text holds them.
        held += ownStandIns(words.slice(at, one.start));
        const name =
          one.route.kind === "issue"
            ? one.route.key
            : (nameOf(one.reference) ?? shortForm(one.route));
        held += names.length < STAND_INS ? tokenOf(name).repeat(name.length) : ownStandIns(name);
        at = one.end;
      }
      return `${held}${ownStandIns(words.slice(at))}`;
    },
  };
}

/**
 * How the page names a reference that a plain line cites (`referenceStandIns`), read once over
 * the whole tree: by the words of the first chip of the tree that names the same node, as a chip
 * draws them (`chipWords`); else, for a node the tree holds, by its kind's word; else not at
 * all. These are the names a chip draws, the page's one naming rule, so no markdown is read
 * here. `nodeIdOf` is how the issue addresses a
 * node (`treeContext`), so two addresses of one block are named alike.
 */
export function useReferenceNames(
  root: TreeNode,
  nodeIdOf: (reference: string) => string | undefined
): (reference: string) => string | undefined {
  return useMemo(() => {
    const types = new Map<string, NodeType>();
    const names = new Map<string, string>();
    const walk = (node: TreeNode): void => {
      types.set(node.id, node.type);
      for (const chip of node.chips) {
        if (chip.gone || chip.name === "") continue;
        const id = nodeIdOf(chip.ref);
        if (id === undefined || names.has(id)) continue;
        const words = chipWords(chip.name);
        if (words !== "") names.set(id, words);
      }
      for (const child of node.children) walk(child);
    };
    walk(root);
    return (reference: string) => {
      const id = nodeIdOf(reference);
      if (id === undefined) return undefined;
      const type = types.get(id);
      return names.get(id) ?? (type === undefined ? undefined : NODE_TYPE_NAMES[type]);
    };
  }, [nodeIdOf, root]);
}

/**
 * How the page names a reference that a plain line cites, for one issue's tree
 * (`useReferenceNames`), and how that issue addresses a node, which a caller that resolves a
 * decision's tie reads too. Only the addressing of the issue's context is read and no chip's
 * name, so the line that context names a block by is the whole reading's words (`wordsOf`),
 * which nothing here reads.
 */
export function useIssueNames(
  issue: IssueDetails,
  root: TreeNode
): {
  readonly nameOf: (reference: string) => string | undefined;
  readonly nodeIdOf: (reference: string) => string | undefined;
} {
  const nodeIdOf = useMemo(() => treeContext({ issue }, wordsOf).nodeIdOf, [issue]);
  const nameOf = useReferenceNames(root, nodeIdOf);
  return { nameOf, nodeIdOf };
}
