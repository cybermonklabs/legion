import { type ReactNode, useMemo } from "react";
import { useLocation } from "react-router-dom";

import { pageQuietText } from "../../../theme/classes";
import { usePlainHeadline } from "../../refs/plain-text";
import type { TreeNode } from "../issue-tree";
import { authorText, pageChrome } from "../page-structure";
import { LENS_WORDS } from "../page-words";
import { titleReading } from "../tree-drafts";
import { type FactParts, filled, type LensChip } from "./lens-drafts";
import { lensPlaceOf } from "./lens-reads";
import { NodeLink } from "./NodeLink";

/** What a line is marked with (`data-lens-line`): the `LENS_WORDS` key of its label, and for a
 *  line with no label, what it draws. */
export type LineName = keyof typeof LENS_WORDS | "reference";

/**
 * The lines a row draws under its words from its lens facts, each only where it has something:
 *
 * - a lens: the lenses that triggered it, then the lenses it triggered, each as a chip;
 * - a risk: its early warning, its prevention, then the checks that prevent it, or the one line
 *   saying none does;
 * - a statement a constraint names: those constraints, as chips, in the outline and in the Spec
 *   view (`spec-filter/SpecFilterBody.tsx`) alike;
 * - a constraint: a standard's reference, or where a measurement was taken.
 *
 * A lens's kind's words are its tag's (`NodeRow.tsx`'s `RowMargin`), drawn whole, so a lens has
 * no line of its own for them. Each label is the page's, from `LENS_WORDS`; each stored string is
 * the author's. Each fact line is drawn as its one field holds it (`LensFacts`), so a line that
 * names a lens, a risk, a constraint or a finding the tree read holds draws that name as a link
 * to the node's row, as a chip is drawn (`FactWords`), and its other words through the
 * plain-text projection (`FactString`), so a reference in them reads as its target's name and
 * never as a raw address. The lines are small and quiet, as the row's parent line is, and the
 * words in them wrap.
 */
export function LensLines({ node, tabStops }: { node: TreeNode; tabStops: boolean }): ReactNode {
  const lens = node.lens;
  if (lens === undefined) return null;
  const lines: ReactNode[] = [];
  if (node.type === "lens") {
    if (lens.triggeredBy.length > 0) {
      lines.push(
        <Line key="triggered_by" line="triggered_by">
          <Chips chips={lens.triggeredBy} tabStops={tabStops} />
        </Line>
      );
    }
    if (lens.triggered.length > 0) {
      lines.push(
        <Line key="triggered" line="triggered">
          <Chips chips={lens.triggered} tabStops={tabStops} />
        </Line>
      );
    }
  }
  if (node.type === "risk") {
    if (filled(lens.earlyWarning)) {
      lines.push(
        <Line key="early_warning" line="early_warning">
          <FactWords tabStops={tabStops} words={lens.earlyWarning} />
        </Line>
      );
    }
    if (filled(lens.prevention)) {
      lines.push(
        <Line key="prevention" line="prevention">
          <FactWords tabStops={tabStops} words={lens.prevention} />
        </Line>
      );
    }
    lines.push(
      lens.preventedBy.length === 0 ? (
        <Line key="prevented_by_nothing" line="prevented_by_nothing" />
      ) : (
        <Line key="prevented_by" line="prevented_by">
          <Chips chips={lens.preventedBy} tabStops={tabStops} />
        </Line>
      )
    );
  }
  if (lens.constrainedBy.length > 0) {
    lines.push(
      <Line key="constrained_by" line="constrained_by">
        <Chips chips={lens.constrainedBy} tabStops={tabStops} />
      </Line>
    );
  }
  if (node.type === "constraint") {
    // A standard names its reference; a measurement says where it was taken.
    if (filled(lens.reference)) {
      lines.push(
        <Line key="reference" line="reference">
          <FactWords tabStops={tabStops} words={lens.reference} />
        </Line>
      );
    } else if (filled(lens.takenWhere)) {
      lines.push(
        <Line key="taken_at" line="taken_at">
          <FactWords tabStops={tabStops} words={lens.takenWhere} />
        </Line>
      );
    }
  }
  return lines.length === 0 ? null : lines;
}

/** One line: its label, where `LENS_WORDS` holds one for it, then what it holds. Styled as the
 *  row's parent line (`NodeRow.tsx`'s `data-node-parent-line`). */
function Line({ children, line }: { children?: ReactNode; line: LineName }): ReactNode {
  const label = line === "reference" ? undefined : LENS_WORDS[line];
  return (
    <p
      className={`flex min-w-0 items-baseline gap-1 text-xs leading-5 ${pageQuietText}`}
      data-lens-line={line}
    >
      {label === undefined ? null : (
        <span className="shrink-0" {...pageChrome}>
          {label}
        </span>
      )}
      {children}
    </p>
  );
}

/** A stored string, which wraps inside its line. */
function Words({ children }: { children: ReactNode }): ReactNode {
  return (
    <span className="min-w-0 break-words" {...authorText}>
      {children}
    </span>
  );
}

/** A fact line, drawn as `Words`, as its one `LensFacts` field holds it: its words, or, where the
 *  line names a node the tree read holds, its parts (`FactParts`): each string through the
 *  plain-text projection (`FactString`), and each chip as a link to that node's row
 *  (`NodeLink`), as a line's chips are, named as `store-words.ts` named it (decision 388). Drawn
 *  so, the parts read as their words (`factText`), a reference in a string as its target's name. */
function FactWords({
  tabStops,
  words,
}: {
  tabStops: boolean;
  words: string | FactParts;
}): ReactNode {
  return (
    <Words>
      {typeof words === "string" ? (
        <FactString words={words} />
      ) : (
        words.map((part, at) =>
          typeof part === "string" ? (
            // biome-ignore lint/suspicious/noArrayIndexKey: a line's parts are its words, in order.
            <FactString key={at} words={part} />
          ) : (
            // biome-ignore lint/suspicious/noArrayIndexKey: a line's parts are its words, in order.
            <NodeLink chip={part} key={at} tabStops={tabStops} />
          )
        )
      )}
    </Words>
  );
}

/**
 * One stored string of a fact line, drawn through the framework's plain-text projection
 * (`usePlainHeadline`, the reading list's `lens-fact-line`): each reference in it reads as its
 * target's name, by the name the page published for it and else by a read of its own, and never
 * as a raw address (R22). The words are read as one text run (`titleReading`), since the step
 * read them to their plain words already (`storedFacts`), and a role in them by the issue the page
 * is on. The projection trims the words it reads, so the one space a part keeps at either end
 * beside a chip is drawn around them, once for a part that is space alone. Nothing until the
 * projection has answered.
 */
function FactString({ words }: { words: string }): ReactNode {
  const { pathname, search } = useLocation();
  const issue = lensPlaceOf(pathname, search)?.issueKey;
  const reading = useMemo(() => titleReading(words), [words]);
  const { reads, text } = usePlainHeadline(reading, undefined, {
    issue,
    surface: "lens-fact-line",
  });
  if (text === undefined) return reads;
  const before = words.startsWith(" ") ? " " : null;
  const after = words.endsWith(" ") && text.text !== "" ? " " : null;
  return (
    <>
      {reads}
      {before}
      {text.text}
      {after}
    </>
  );
}

/** The chips of one line, which wrap onto as many lines as they need. */
function Chips({ chips, tabStops }: { chips: readonly LensChip[]; tabStops: boolean }): ReactNode {
  return (
    <span className="flex min-w-0 flex-wrap items-baseline gap-x-2">
      {chips.map((chip) => (
        <NodeLink chip={chip} key={chip.address} tabStops={tabStops} />
      ))}
    </span>
  );
}
