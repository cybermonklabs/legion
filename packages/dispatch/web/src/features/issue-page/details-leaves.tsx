import {
  type FocusEventHandler,
  type ReactNode,
  type Ref,
  type RefObject,
  useEffect,
  useRef,
  useState,
} from "react";
import { flushSync } from "react-dom";

import { pageInkText, pageQuietText, pageRowBorder } from "../../theme/classes";
import { IssueLine } from "./BlockedByControl";
import { DETAILS_ROW_ATTRIBUTE, DETAILS_ROW_REQUEST_EVENT, type DetailsRow } from "./details-rows";
import { statusGlyph } from "./issue-status";
import { authorText, MARGIN_SECTION_NAMES, type MarginSection, pageChrome } from "./page-structure";
import { CONTROL_LABELS } from "./page-words";
import type { MarginSections } from "./useMarginSections";

/** How many entries a list of unbounded length draws before it folds behind a count that opens
 *  the rest. */
export const FOLD_AFTER = 10;

/** The rows of each section that a control elsewhere on the page opens Details at
 *  (`DetailsRow`), each where it is drawn: the section opens for a request of one of them.
 *  People holds the issue's collaborators, whose place there is `DetailsPeople`. */
const SECTION_ROWS: Readonly<Record<MarginSection, readonly DetailsRow[]>> = {
  development: ["labels", "parent", "route"],
  people: ["collaborators"],
  references: [],
};

/** The issues of a list of them, each its key as a link and its title beside it, folded after
 *  ten behind a count that opens the rest. */
export function IssueList({
  issues,
}: {
  issues: readonly { readonly key: string; readonly title: string }[];
}): ReactNode {
  const [open, setOpen] = useState(false);
  const list = useRef<HTMLDivElement>(null);
  const shown = open ? issues : issues.slice(0, FOLD_AFTER);
  return (
    <div className="flex w-full min-w-0 flex-col gap-1" ref={list}>
      {shown.map((one) => (
        <IssueLine issueKey={one.key} key={one.key} title={one.title} />
      ))}
      {issues.length > FOLD_AFTER && !open ? (
        <ShowAll
          className="self-start"
          count={issues.length}
          list={list}
          onShow={() => setOpen(true)}
        />
      ) : null}
    </div>
  );
}

/** The count a folded list is drawn behind, which opens the rest of it. `className` places it
 *  in the list that draws it, and `list` is that list, each of its children one entry. The
 *  count goes with its press, so the focus goes on to the link of the first entry it opened,
 *  the list's entry `FOLD_AFTER`, rather than falling to the page. */
export function ShowAll({
  className,
  count,
  list,
  onShow,
}: {
  className: string;
  count: number;
  list: RefObject<HTMLElement | null>;
  onShow: () => void;
}): ReactNode {
  return (
    <button
      className={`${className} text-xs font-semibold underline underline-offset-[3px] ${pageInkText}`}
      onClick={() => {
        flushSync(onShow);
        list.current?.children[FOLD_AFTER]?.querySelector<HTMLElement>("a[href]")?.focus();
      }}
      type="button"
      {...pageChrome}
    >
      {/* One child, so the space before the count holds where every button is a flex box
          (`styles.css`, below 1280 px), which drops a space between two of its children. */}
      <span>
        {CONTROL_LABELS.show_all} <span {...authorText}>{count}</span>
      </span>
    </button>
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
  onBlur,
  onFocus,
  sections,
}: {
  children: ReactNode;
  /** The drawer's `aside` the panel is placed in, which a request for a row is sent to. */
  drawerBox: HTMLElement | null;
  /** Takes the section's heading, its own control, so a focus can be handed to it. */
  headingRef?: Ref<HTMLButtonElement>;
  name: MarginSection;
  /** Hears each focus that leaves an element of the section, one its rows draw in a portal
   *  included. */
  onBlur?: FocusEventHandler<HTMLElement>;
  /** Hears each focus that goes to an element of the section, one its rows draw in a portal
   *  included. */
  onFocus?: FocusEventHandler<HTMLElement>;
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
    // biome-ignore lint/a11y/noStaticElementInteractions: nothing here is made interactive - the handlers only read where the focus has gone and what it has left, so the section needs no role of its own.
    <section data-margin-section={name} onBlur={onBlur} onFocus={onFocus}>
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
