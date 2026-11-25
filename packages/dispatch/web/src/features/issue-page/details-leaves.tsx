import { type ReactNode, type Ref, type RefObject, useEffect, useRef, useState } from "react";
import { flushSync } from "react-dom";

import { pageInkText, pageMutedText, pageQuietText, pageRowBorder } from "../../theme/classes";
import { IssueLine } from "./BlockedByControl";
import { DETAILS_ROW_ATTRIBUTE, DETAILS_ROW_REQUEST_EVENT, type DetailsRow } from "./details-rows";
import { statusGlyph } from "./issue-status";
import { authorText, MARGIN_SECTION_NAMES, type MarginSection, pageChrome } from "./page-structure";
import { CONTROL_LABELS } from "./page-words";
import type { MarginSections } from "./useMarginSections";

/** How many entries a list of unbounded length in Details draws at first, and how many more each
 *  press of the Show more under it draws (`useListPages`). */
export const LIST_PAGE = 20;

/** The controls of an entry a press of Show more can move the focus to, and of the history a
 *  focus can be handed to (`IssueHistory.tsx`). */
export const ENTRY_CONTROLS = "a[href], button:not(:disabled)";

/** The look of Show more, under a list's entries and under the history's (`IssueHistory.tsx`). */
export const SHOW_MORE_CONTROL = `dispatch-inline-link text-xs font-medium underline ${pageMutedText}`;

/** The rows of each section that a control elsewhere on the page opens Details at
 *  (`DetailsRow`), each where it is drawn: the section opens for a request of one of them.
 *  People holds the issue's collaborators, whose place there is `DetailsPeople`. */
const SECTION_ROWS: Readonly<Record<MarginSection, readonly DetailsRow[]>> = {
  development: ["labels", "parent", "route"],
  people: ["collaborators"],
  references: [],
};

/** The issues of a list of them, each its key as a link and its title, where it has one, beside
 *  it, drawn 20 at a time (`useListPages`). */
export function IssueList({
  issues,
}: {
  issues: readonly { readonly key: string; readonly title: string | undefined }[];
}): ReactNode {
  const pages = useListPages<HTMLDivElement>();
  return (
    <div className="flex w-full min-w-0 flex-col gap-1" ref={pages.list}>
      {issues.slice(0, pages.drawn).map((one) => (
        <IssueLine issueKey={one.key} key={one.key} title={one.title} />
      ))}
      {pages.more(issues.length)}
    </div>
  );
}

/** The pages of one list of unbounded length in Details (`useListPages`). */
export interface ListPages<E extends Element> {
  /** How many of the list's entries it draws: its first `LIST_PAGE`, and `LIST_PAGE` more for
   *  each press of its Show more. */
  readonly drawn: number;
  /** The box the list draws its entries in, each entry one child of it in the list's order,
   *  for the list to hand to that box: where a press of Show more finds the first entry it drew
   *  unless the list says otherwise (`more`'s `entry`). */
  readonly list: RefObject<E | null>;
  /** The list's Show more, which the list draws under its entries: for a list of `count`
   *  entries, nothing once every entry is drawn. `entry` is the list's entry at a place of it,
   *  as the page draws it, which the press moves the focus to: by default the child of `list`
   *  at that place. `className` places the control in the list that draws it. */
  readonly more: (
    count: number,
    entry?: (index: number) => Element | null | undefined,
    className?: string
  ) => ReactNode;
}

/**
 * The one paging rule of every list of unbounded length that Details draws but the history: the
 * list draws its first 20 entries and, under them, Show more, which says how many entries are
 * left and draws the next 20, in the words and the look of the history's own control
 * (`IssueHistory.tsx`). A press keeps the entries drawn and reads nothing, and the control goes
 * once no entry is left. The list's entries are one sequence, whatever reads they come from: the
 * entries of a source drawn after another take the places after that one's, share its pages and
 * its control, and count in the `count` the control is given, which is the whole list's.
 *
 * The press draws the next entries before it returns (`flushSync`), and moves the focus to the
 * first of them (`focusEntry`), whether the control stays or goes, rather than let it fall to the
 * page as the last press takes the control away. A control that a change made elsewhere takes
 * away, as the list falls to what it draws, hands nothing on: that loss is not the reader's, and
 * the focus stays on the page's body.
 */
export function useListPages<E extends Element = HTMLElement>(): ListPages<E> {
  const [drawn, setDrawn] = useState(LIST_PAGE);
  const list = useRef<E>(null);
  return {
    drawn,
    list,
    more: (count, entry = (index) => list.current?.children[index], className = "") =>
      count <= drawn ? null : (
        <ShowMore
          className={className}
          left={count - drawn}
          onMore={() => {
            flushSync(() => setDrawn(drawn + LIST_PAGE));
            focusEntry(entry(drawn));
          }}
        />
      ),
  };
}

/** The groups of a list Details draws as one sequence of their entries, a step's runs by their
 *  task or Legion's agents by their role, cut where its pages end (`useListPages`): each group
 *  with those of its entries among the first `drawn` of the sequence, and none whose entries are
 *  all after them. */
export function groupsDrawn<G, T>(
  groups: readonly G[],
  entriesOf: (group: G) => readonly T[],
  drawn: number
): readonly { readonly entries: readonly T[]; readonly group: G }[] {
  let left = drawn;
  return groups.flatMap((group) => {
    const entries = entriesOf(group).slice(0, left);
    left -= entries.length;
    return entries.length === 0 ? [] : [{ entries, group }];
  });
}

/** Moves the focus to `entry`, the first entry a press of Show more drew: to its first link or
 *  button, and where it holds none, a label or a step's run, to the entry itself, which then
 *  takes a focus moved to it and stays out of the tab order (`tabindex` -1), as a heading of
 *  Details the focus is handed to does. */
function focusEntry(entry: Element | null | undefined): void {
  if (!(entry instanceof HTMLElement)) return;
  const control = entry.matches(ENTRY_CONTROLS)
    ? entry
    : entry.querySelector<HTMLElement>(ENTRY_CONTROLS);
  if (control !== null) {
    control.focus();
    return;
  }
  if (!entry.hasAttribute("tabindex")) entry.setAttribute("tabindex", "-1");
  entry.focus();
}

/** Show more, on a line of its own under a list's entries: how many entries are left, "Show more
 *  (25 left)" (`ShowMoreWords`), in the look of the history's own control (`IssueHistory.tsx`). */
function ShowMore({
  className,
  left,
  onMore,
}: {
  className: string;
  left: number;
  onMore: () => void;
}): ReactNode {
  return (
    <div className={`w-full ${className}`}>
      <button className={SHOW_MORE_CONTROL} onClick={onMore} type="button" {...pageChrome}>
        <ShowMoreWords count={<span {...authorText}>{left}</span>} />
      </button>
    </div>
  );
}

/** The words of Show more, the list's and the history's: "Show more", and where `count` is given,
 *  the node drawn for how many entries are left, " (25 left)". They are one child of the
 *  control, so the space before the count holds where every button is a flex box (`styles.css`,
 *  below 1280 px), which drops a space between two of its children. */
export function ShowMoreWords({ count }: { count?: ReactNode }): ReactNode {
  return (
    <span>
      {CONTROL_LABELS.show_more}
      {count === undefined ? null : (
        <>
          {" ("}
          {count} {CONTROL_LABELS.show_more_left})
        </>
      )}
    </span>
  );
}

/** One named section of the panel's properties, open or closed by its own one control. A closed
 *  section keeps its rows on the page and hides them (`hidden`: out of view, out of the tab order
 *  and out of the accessibility tree), so what a reader sees is what the page holds, and what a
 *  row holds of its own - a draft, an open form, a refusal said in the row - is still there when
 *  the section opens again, as the panel's is when Details does (`slots/details.tsx`). */
export function PropertySection({
  children,
  drawerBox,
  headingRef,
  name,
  sections,
}: {
  children: ReactNode;
  /** The drawer's `aside` the panel is placed in, which a request for a row is sent to. */
  drawerBox: HTMLElement | null;
  /** Takes the section's heading, its own control, so a focus can be handed to it. */
  headingRef?: Ref<HTMLButtonElement>;
  name: MarginSection;
  sections: MarginSections;
}): ReactNode {
  const open = sections.isOpen(name);
  // A control elsewhere on the page that opens Details at one of this section's rows asks the
  // drawer for that row before it looks for it (`useOpenDetailsAt` sends
  // `DETAILS_ROW_REQUEST_EVENT`). A closed section opens for it as its heading opens it, so the
  // reader's kept choice follows, and in the same task, so the row is no longer hidden when the
  // control looks for it and its control takes the focus.
  const latest = useRef(sections);
  latest.current = sections;
  useEffect(() => {
    if (drawerBox === null) return;
    const openForRow = (event: Event) => {
      if (
        !(event instanceof CustomEvent) ||
        !SECTION_ROWS[name].includes(event.detail) ||
        latest.current.isOpen(name)
      ) {
        return;
      }
      flushSync(() => latest.current.toggle(name));
    };
    drawerBox.addEventListener(DETAILS_ROW_REQUEST_EVENT, openForRow);
    return () => drawerBox.removeEventListener(DETAILS_ROW_REQUEST_EVENT, openForRow);
  }, [drawerBox, name]);
  return (
    <section data-margin-section={name}>
      <h2>
        <button
          aria-expanded={open}
          className={`flex min-h-11 w-full items-center justify-between gap-2 text-xs leading-4 font-semibold tracking-[0.08em] uppercase md:min-h-8 ${pageQuietText}`}
          onClick={() => sections.toggle(name)}
          ref={headingRef}
          type="button"
          {...pageChrome}
        >
          <span>{MARGIN_SECTION_NAMES[name]}</span>
          <span aria-hidden="true">{open ? "−" : "+"}</span>
        </button>
      </h2>
      <div className="flex min-w-0 flex-col" hidden={!open}>
        {children}
      </div>
    </section>
  );
}

/** The one text glyph of Details' rows drawn smaller than the row's text, so that it fits its
 *  14 px column (`Row`'s `w-3.5`): the page's typeface (`Dispatch Inter`, `styles.css`) draws ⊘
 *  19 px wide at the row's 14 px, to 2 px from the row's name, and 13 px wide at 9 px. Every
 *  other text glyph fits at the row's size, and each icon is drawn to the column. */
export const SMALL_GLYPH = "⊘";

/** The Status row's shape: that of a status in motion, as the prototype draws the row, which the
 *  page's one status icon draws (`StatusGlyph`). It is named through `statusGlyph`, since the
 *  character that names it is no character the page draws as text. */
export const STATUS_ROW_SHAPE = statusGlyph("in_progress");

/**
 * One row of Details (the prototype's `.frow`): a column of 112 px with the property's shape
 * and its name, and the value beside it; a `wide` row is one column, for a property whose own
 * control names it. The shape is an icon of `details-icons.tsx` where the page's typeface does
 * not hold the character, and otherwise that character as text, one the typeface holds and that
 * no font draws as an emoji unless asked to. `row` names the row a control elsewhere opens Details
 * at (`details-rows.ts`), and `valueRef` takes the box of the row's value.
 */
export function Row({
  children,
  glyph,
  label,
  row,
  testId,
  valueRef,
  wide = false,
}: {
  children: ReactNode;
  glyph?: ReactNode;
  label?: ReactNode;
  row?: DetailsRow;
  /** The row's `data-testid`, by which the page's tests and its browser tests find it. */
  testId?: string;
  valueRef?: Ref<HTMLDivElement>;
  wide?: boolean;
}): ReactNode {
  return (
    <div
      className={`grid min-w-0 gap-2.5 border-b py-2 text-sm leading-5 ${
        wide ? "grid-cols-1" : "grid-cols-[112px_minmax(0,1fr)]"
      } ${pageRowBorder}`}
      data-testid={testId}
      {...(row === undefined ? {} : { [DETAILS_ROW_ATTRIBUTE]: row })}
    >
      {label === undefined ? null : (
        <div className={`flex min-w-0 items-start gap-[7px] ${pageQuietText}`}>
          <span
            aria-hidden="true"
            className={`w-3.5 shrink-0 text-center ${glyph === SMALL_GLYPH ? "text-[9px]" : ""}`}
          >
            {glyph}
          </span>
          <span className="min-w-0" {...pageChrome}>
            {label}
          </span>
        </div>
      )}
      <div
        className={`flex min-w-0 flex-wrap items-center gap-x-1.5 gap-y-1 [overflow-wrap:break-word] ${pageInkText}`}
        ref={valueRef}
      >
        {children}
      </div>
    </div>
  );
}
