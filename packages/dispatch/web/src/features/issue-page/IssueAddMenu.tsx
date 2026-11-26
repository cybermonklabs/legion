import {
  type ReactNode,
  type RefObject,
  useCallback,
  useEffect,
  useLayoutEffect,
  useRef,
  useState,
} from "react";

import type { IssueDetails } from "../../api/types";
import { pageHeaderControl, pageMenu, pageMenuItem } from "../../theme/classes";
import {
  type MarkedProperty,
  PROPERTY_CONTROL_ATTRIBUTE,
  propertyControlSelector,
} from "../issue/property-control";
import { roveFocus } from "../shell/roving";
import type { IssueField } from "./IssueFieldForms";
import { useMarginDrawer } from "./margin-slot";
import { PROPERTY_LABELS, pageChrome } from "./page-structure";
import { CONTROL_LABELS } from "./page-words";
import { USABLE_CONTROL_ATTRIBUTES, usableControl } from "./usable-control";

/**
 * The one control that reaches a property this issue has no value for, in Details. Every
 * property that has a value is drawn where it belongs - in the header or in a row of Details -
 * so nothing is hidden behind this; what it lists is exactly what is not there yet, and picking
 * one opens the control that sets it.
 *
 * Six of the fields it lists are edited by a control drawn elsewhere on the page (the assignee,
 * the labels, the issues this one waits on, the repository, the components and the priorities
 * it advances), so the menu presses those rather than holding a second copy of them. It finds
 * each by the mark that control carries, the property it sets and the issue's key
 * (`propertyControl`, `features/issue/property-control.ts`), which the control writes and the menu
 * reads through the one module, so the two sides cannot drift apart. It never finds one by its
 * accessible name: a name changes with the control's state ("Saving labels of CORE-1") and its
 * surface, and the header's assignee, the shared list's single pick, is named through
 * `aria-labelledby` by its list's name and then what it shows, so a lookup by `aria-label` never
 * found it and Add never listed Assignee. It lists one only while that control takes a press
 * (`usableControl`). A press on an entry opens that property's control, and a control that takes
 * no press opens nothing: while the priorities' trigger is held (`aria-disabled`), it takes no
 * press, and an Advances listed then would leave the picker shut and the focus on the body.
 * A control of Details still takes a press while Details is closed, since the reach opens
 * Details first.
 *
 * Which entries can be reached - which decides whether Add is drawn at all, and what its open
 * menu lists - is read from the page, never while the menu renders: during a render the page
 * still holds the previous commit, so a control freed in the same commit as the issue's value
 * still takes no press there. Read during the render, removing an issue's last label drew no
 * Add: in the render where the save landed the labels' control was still busy saving ("Saving
 * labels of CORE-1", `aria-disabled`), that render's commit freed it, and nothing drew the menu
 * again. So it is read after each commit of the menu, and again on each change that can draw,
 * re-mark or remove a control an entry reaches, or change whether it takes a press, which a
 * `MutationObserver` over Details and the header reports: most such changes are a commit of the
 * control's own component alone, which draws the menu no further time. Read after the menu's
 * own commits only, Add would stay gone once a refused priorities' save gave "Add a priority to
 * CORE-1" its press back in a commit of `IssuePriorityLine` alone; the repository's select and
 * the components' picker, each drawn then once its own read answered, were never seen either.
 * The repository's row still swaps its text - "None" or the repository - and its box in commits
 * of its own control alone.
 */
export function IssueAddMenu({
  drawerBox,
  isClosed,
  issue,
  onOpenField,
  panel,
}: {
  /** The Details drawer's `aside` the panel is placed in (`slots/details.tsx`), or `null` while
   *  the panel is in none. */
  drawerBox: HTMLElement | null;
  isClosed: boolean;
  issue: IssueDetails;
  onOpenField: (field: IssueField) => void;
  /** The panel's element (`IssueMarginPanel`), under which every control of Details an entry
   *  reaches is drawn. */
  panel: RefObject<HTMLElement | null>;
}): ReactNode {
  const [open, setOpen] = useState(false);
  // The labels of the entries whose control takes a press, in `emptyFields`' order, as `read`
  // last found them: none until the first commit, which the layout effect below reads before the
  // page is painted. A read that finds the list as it was keeps the array held, so it draws the
  // menu no further time, and the observer below, which also sees the menu's own drawing, cannot
  // loop.
  const [reachable, setReachable] = useState<readonly string[]>([]);
  const drawer = useMarginDrawer();
  const wrapper = useRef<HTMLDivElement>(null);
  const trigger = useRef<HTMLButtonElement>(null);
  const menu = useRef<HTMLDivElement>(null);
  useEffect(() => {
    if (!open) return;
    // The menu opens with the focus on its first entry, so a reader on the keyboard is in it.
    menu.current?.querySelector<HTMLElement>('[role="menuitem"]')?.focus();
    const closeOutside = (event: PointerEvent) => {
      if (!(event.target instanceof Node) || wrapper.current?.contains(event.target) !== true) {
        setOpen(false);
      }
    };
    // Escape belongs to whatever the reader is in, as an anchored panel takes it
    // (`components/AnchoredPanel.tsx`): the menu takes a press from inside it or from Add,
    // closes and gives the focus back to Add. It reads the press in the capture phase and marks
    // it taken, so neither the phone's Details sheet, which holds the menu, nor Details' own
    // Escape at every other width closes Details on it. A press a layer over the menu took
    // first - the hover card's, whose listener the shell adds as it mounts, so it hears the press
    // before the menu's (`RefPreview.tsx`) - is that layer's, and leaves the menu open.
    const closeOnEscape = (event: KeyboardEvent) => {
      if (
        event.key !== "Escape" ||
        event.defaultPrevented ||
        !(event.target instanceof Node) ||
        wrapper.current?.contains(event.target) !== true
      ) {
        return;
      }
      event.preventDefault();
      event.stopPropagation();
      trigger.current?.focus();
      setOpen(false);
    };
    document.addEventListener("pointerdown", closeOutside);
    document.addEventListener("keydown", closeOnEscape, true);
    return () => {
      document.removeEventListener("pointerdown", closeOutside);
      document.removeEventListener("keydown", closeOnEscape, true);
    };
  }, [open]);

  const fields = emptyFields(issue);
  // The fields of the latest commit, which `read` reads: which entries the issue lists and the
  // key their controls are marked with both follow the issue, and the observer that calls `read`
  // is not set up again on each render, so `read` takes them from here rather than from the
  // render it was made in.
  const committed = useRef(fields);
  const read = useCallback(() => {
    const found = committed.current
      .filter((field) => field.reachable())
      .map((field) => field.label);
    setReachable((held) =>
      held.length === found.length && held.every((label, index) => label === found[index])
        ? held
        : found
    );
  }, []);
  // A render reads the previous commit, so the page is read once a commit is on it: after every
  // commit of the menu, since the issue's value and whether a control takes a press can change in
  // the same one, and on each change the observer reports, since a control drawn, re-marked,
  // removed, disabled or hidden by another component's own commit draws the menu no further time:
  // a refused save of the priorities gives their control back in a commit of `IssuePriorityLine`
  // alone. Every control an entry reaches is under the panel or the header (the assignee's), so
  // those two are watched, for the controls' mark and each attribute `usableControl` reads.
  // `usableControl` reads `hidden` and `inert` on every element around a control
  // (`canHoldFocus`), and `aria-hidden` from a control of Details up to Details' own `aside`, and
  // the `aside` itself, which is hidden while Details is closed; an element between them can be
  // hidden too, as the phone's sheet hides the box that holds the panel while a margin thread is
  // open (`features/margin/MarginSheet.tsx`). So where an `aside` holds the panel (`drawerBox`),
  // the whole `aside` is watched in the panel's place. An element above the watched box is not
  // watched: a hide or an inert set there is read at Add's next commit. Nothing above Details'
  // `aside` sets either today: in the shell (`app.tsx`, `features/shell`) and the margin
  // (`features/margin`), `hidden` is set on that `aside`, on elements inside it and on the
  // margin's column, which is drawn only where Details is not, and `inert` on nothing.
  useLayoutEffect(() => {
    committed.current = fields;
    read();
  });
  useEffect(() => {
    const watching = new MutationObserver(read);
    let header: Element | null = null;
    const watch = () => {
      header = document.querySelector(ISSUE_HEADER);
      watching.disconnect();
      for (const root of [drawerBox ?? panel.current, header]) {
        if (root !== null) watching.observe(root, WATCHED);
      }
    };
    // The panel is drawn before any `aside` holds it, and can be moved to another: the watch
    // starts again for each `drawerBox`, and the page is read once it is on, so a change made
    // before it started is not missed.
    watch();
    read();
    // The header can be drawn after Details, or drawn again as another element, and only an
    // element drawn can be watched: so the body is watched for drawn and removed elements alone,
    // and the header is watched again, and the page read, whenever the one found has changed.
    // That is one query per change, so the rest of the page's drawing reads no entry.
    const arriving = new MutationObserver(() => {
      if (document.querySelector(ISSUE_HEADER) === header) return;
      watch();
      read();
    });
    arriving.observe(document.body, { childList: true, subtree: true });
    return () => {
      watching.disconnect();
      arriving.disconnect();
    };
  }, [drawerBox, panel, read]);
  // The entries of this render's fields the page was last read to reach. A field that got a
  // value in this render is gone from them at once; one that lost it joins once the commit is read.
  const entries = fields.filter((field) => reachable.includes(field.label));

  // Add is drawn nowhere on a closed issue or with no entry to list, and the menu shuts with it,
  // set during the render as React has a prop's change reset state. Left open, it would come back
  // drawn open when the issue reopens or a property loses its value again, with the focus not in
  // it, since the effect above puts the focus on its first entry only as it opens, and its
  // listeners on the document would stay on meanwhile. A focus on Add or in its open menu goes
  // with it to the page's body, and stays there, whatever took Add away: another session setting
  // the last property it lists or closing the issue, a read taking its last entry's control away,
  // the command palette's Close issue, and a save of the reader's own that lands while they have
  // tabbed onto Add and sets the last property it lists. None of these is a press live on Add or
  // its menu; a press in its menu closes the menu with Add still drawn, and the control it opens
  // takes the focus. An entry a change made elsewhere takes out of the open menu leaves the focus
  // on the body alike.
  const drawn = !isClosed && entries.length > 0;
  if (!drawn) {
    if (open) setOpen(false);
    return null;
  }
  return (
    <div className="relative flex shrink-0" ref={wrapper}>
      <button
        aria-expanded={open}
        aria-haspopup="menu"
        aria-label={CONTROL_LABELS.add}
        className={pageHeaderControl}
        onClick={() => setOpen((shown) => !shown)}
        ref={trigger}
        type="button"
        {...pageChrome}
      >
        {CONTROL_LABELS.add}
      </button>
      {open ? (
        <div
          aria-label={CONTROL_LABELS.add}
          className={`${pageMenu} left-0`}
          // The arrow keys walk the entries, as they walk the page's other lists
          // (`shell/roving.ts`); the press is the menu's, and no key of the page's sees it.
          onKeyDown={(event) => {
            if (event.key !== "ArrowDown" && event.key !== "ArrowUp") return;
            event.preventDefault();
            roveFocus(
              [...event.currentTarget.querySelectorAll<HTMLElement>('[role="menuitem"]')],
              event.target instanceof HTMLElement ? event.target : null,
              event.key === "ArrowDown" ? 1 : -1
            );
          }}
          ref={menu}
          role="menu"
        >
          {entries.map((field) => (
            <button
              className={pageMenuItem}
              key={field.label}
              onClick={() => {
                setOpen(false);
                field.open();
              }}
              role="menuitem"
              type="button"
              {...pageChrome}
            >
              {field.label}
            </button>
          ))}
        </div>
      ) : null}
    </div>
  );

  /** Every property of `issue` with no value, each with the control that sets it. */
  function emptyFields(details: IssueDetails): readonly AddableField[] {
    // A control drawn elsewhere on the page is found by its mark, the property it sets and this
    // issue's key (`controlOf`): the control writes the mark and this reads it through the one
    // module, so the entry cannot go missing on a word nobody meant to change, nor on a name the
    // control's state or its surface changes. A control of the header - the assignee - is
    // reached with Details closed, since on a phone Details covers it; a control of Details is
    // reached with Details open, since shut it takes neither a press nor the focus. Either is
    // reached only while it takes a press (`controlOf`): a press on an entry opens that
    // property's control, and a control that would not take it opens nothing, so its entry
    // is not listed and a press finds nothing to reach. The control is scrolled into view
    // first, since its popover is placed from the control's place on the screen. The
    // repository's box is focused rather than clicked, since a script's click puts no focus in a
    // text box; every other control is a button, which the click opens.
    const atControl = (property: MarkedProperty) => ({
      open: () => {
        const control = controlOf(property, details.key);
        const reach = () => {
          control?.scrollIntoView({ block: "nearest" });
          if (control instanceof HTMLInputElement) {
            control.focus();
          } else {
            control?.click();
          }
        };
        const inHeader = control?.closest(ISSUE_HEADER) != null;
        const shut = control?.closest("[hidden]") != null;
        if (drawer !== undefined && (inHeader ? drawer.open : shut && !drawer.open)) {
          drawer.setOpen(!inHeader);
          requestAnimationFrame(() => requestAnimationFrame(reach));
          return;
        }
        reach();
      },
      reachable: () => controlOf(property, details.key) !== null,
    });
    const fields: AddableField[] = [];
    if (details.parent === null) {
      fields.push({
        label: PROPERTY_LABELS.parent,
        open: () => onOpenField("parent"),
        reachable: () => true,
      });
    }
    if (details.assignee === null) {
      fields.push({ label: PROPERTY_LABELS.assignee, ...atControl("assignee") });
    }
    if ((details.labels ?? []).length === 0) {
      fields.push({ label: PROPERTY_LABELS.labels, ...atControl("labels") });
    }
    if (details.route === null || details.route === "") {
      fields.push({
        label: PROPERTY_LABELS.route,
        open: () => onOpenField("route"),
        reachable: () => true,
      });
    }
    if ((details.blocked_by ?? []).length === 0) {
      fields.push({ label: PROPERTY_LABELS.blocked_by, ...atControl("blocked_by") });
    }
    if (details.repo === null) {
      // Inside `NoTooltips` the row of an issue with no repository of its own draws text, whose
      // press draws the repository's box with the focus in it: "None" while the issue resolves to
      // no repository, else the repository it inherits. While the box is open or holds a draft or
      // a write that is out, the row draws the box itself. The text and the box carry the one
      // mark and the row draws one of them at a time, so the entry presses the text where it is
      // drawn and focuses the box where that is.
      fields.push({ label: PROPERTY_LABELS.repository, ...atControl("repository") });
    }
    if ((details.components.ids ?? []).length === 0) {
      fields.push({ label: PROPERTY_LABELS.components, ...atControl("components") });
    }
    if ((details.advances ?? []).length === 0) {
      fields.push({ label: PROPERTY_LABELS.advances, ...atControl("advances") });
    }
    return fields;
  }
}

/** One property with no value: what it is called, whether its control takes a press, and how
 *  to open it. */
interface AddableField {
  readonly label: string;
  readonly open: () => void;
  readonly reachable: () => boolean;
}

/** The issue page's header, which draws the assignee's control. */
const ISSUE_HEADER = "[data-issue-header]";

/** What Add watches under Details and the header: each element drawn or removed, and each
 *  attribute that marks a control (`propertyControl`) or that `usableControl` reads. */
const WATCHED: MutationObserverInit = {
  attributeFilter: [PROPERTY_CONTROL_ATTRIBUTE, ...USABLE_CONTROL_ATTRIBUTES],
  attributes: true,
  childList: true,
  subtree: true,
};

/** The control that sets `property` of the issue `issueKey`, found anywhere on the page by the
 *  mark it carries (`propertyControl`), while it takes a press (`usableControl`), else `null`:
 *  the one lookup both an entry's listing and its press read, so Add lists an entry exactly while
 *  a press on it opens its control. The issue's key in the mark keeps another issue's control out
 *  of reach, as the key in the control's name did. A control in the closed drawer still takes a
 *  press, and the press opens Details first. The fields the panel draws are edited by their own
 *  controls; this is how the add menu reaches one instead of drawing it twice. */
function controlOf(property: MarkedProperty, issueKey: string): HTMLElement | null {
  const control = document.querySelector<HTMLElement>(propertyControlSelector(property, issueKey));
  return usableControl(control) ? control : null;
}
