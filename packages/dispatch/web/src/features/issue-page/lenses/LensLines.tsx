import type { ReactNode } from "react";

import { pageQuietText } from "../../../theme/classes";
import type { TreeNode } from "../issue-tree";
import { authorText, pageChrome } from "../page-structure";
import { LENS_WORDS } from "../page-words";
import { filled, kindWords, type LensChip } from "./lens-drafts";
import { NodeLink } from "./NodeLink";

/** What a line is marked with (`data-lens-line`): the `LENS_WORDS` key of its label, and for a
 *  line with no label, what it draws. */
export type LineName = keyof typeof LENS_WORDS | "kind" | "reference";

/**
 * The lines a row draws under its words from its lens facts, each only where it has something:
 *
 * - a lens: the lenses that triggered it, then the lenses it triggered, each as a chip;
 * - a risk: its early warning, then the checks that prevent it, or the one line saying none does;
 * - a statement a constraint names: those constraints, as chips, in the outline and in the Spec
 *   view (`spec-filter/SpecFilterBody.tsx`) alike;
 * - a constraint: a standard's reference, or where a measurement was taken.
 *
 * With `body` the row also draws a lens's kind's words whole, which its tag cuts, and a risk's
 * prevention. Each label is the page's, from `LENS_WORDS`; each stored string is the author's.
 * The lines are small and quiet, as the row's parent line is, and the words in them wrap.
 */
export function LensLines({
  body,
  node,
  tabStops,
}: {
  body: boolean;
  node: TreeNode;
  tabStops: boolean;
}): ReactNode {
  const lens = node.lens;
  if (lens === undefined) return null;
  const lines: ReactNode[] = [];
  if (node.type === "lens") {
    if (body && filled(lens.kind)) {
      lines.push(
        <Line key="kind" line="kind">
          <Words text={kindWords(lens.kind)} />
        </Line>
      );
    }
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
          <Words text={lens.earlyWarning} />
        </Line>
      );
    }
    if (body && filled(lens.prevention)) {
      lines.push(
        <Line key="prevention" line="prevention">
          <Words text={lens.prevention} />
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
          <Words text={lens.reference} />
        </Line>
      );
    } else if (filled(lens.takenWhere)) {
      lines.push(
        <Line key="taken_at" line="taken_at">
          <Words text={lens.takenWhere} />
        </Line>
      );
    }
  }
  return lines.length === 0 ? null : lines;
}

/** One line: its label, where `LENS_WORDS` holds one for it, then what it holds. Styled as the
 *  row's parent line (`NodeRow.tsx`'s `data-node-parent-line`). */
function Line({ children, line }: { children?: ReactNode; line: LineName }): ReactNode {
  const label = line === "kind" || line === "reference" ? undefined : LENS_WORDS[line];
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
function Words({ text }: { text: string }): ReactNode {
  return (
    <span className="min-w-0 break-words" {...authorText}>
      {text}
    </span>
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
