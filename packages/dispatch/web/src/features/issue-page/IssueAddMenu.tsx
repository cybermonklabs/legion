import { type ReactNode, useEffect, useLayoutEffect, useRef, useState } from "react";

import type { IssueDetails } from "../../api/types";
import { pageHeaderControl, pageMenu, pageMenuItem } from "../../theme/classes";
import { assigneeControlName } from "../issue/AssigneeControl";
import { componentsControlName } from "../issue/IssueComponentsLine";
import { labelsControlName } from "../issue/IssueLabels";
import { repoControlName } from "../issue/RepoControl";
import { addPriorityControlName } from "../priorities/IssuePriorityLine";
import { roveFocus } from "../shell/roving";
import { blockedByControlName } from "./BlockedByControl";
import type { IssueField } from "./IssueFieldForms";
import { useMarginDrawer } from "./margin-slot";
import { PROPERTY_LABELS, pageChrome } from "./page-structure";
import { ANSWER_LINES, CONTROL_LABELS } from "./page-words";

/**
 * The one control that reaches a property this issue has no value for (dispatch://INFRA-1196
 * R13), in Details. Every property that has a value is drawn where it belongs - in the header or
 * in a row of Details - so nothing is hidden behind this; what it lists is exactly what is not
 * there yet, and picking one opens the control that sets it.
 *
 * Six of the fields it lists are edited by a control drawn elsewhere on the page (the assignee,
 * the labels, the issues this one waits on, the repository, the components and the priorities
 * it advances), so the menu presses those rather than holding a second copy of them. It finds
 * each by the name that control exports - `assigneeControlName`, `labelsControlName`,
 * `blockedByControlName`, `repoControlName`, `componentsControlName`, `addPriorityControlName` -
 * so the two sides cannot drift apart, and lists one only while the page draws it: a project
 * that maps no repository offers no repository to set.
 *
 * Whether any entry can be reached, which decides whether Add is drawn at all, is read after each
 * commit of the menu rather than while it renders: during a render the page still holds the
 * previous commit, so a control renamed in the same commit as the issue's value is not found by
 * its new name. Read during the render, removing an issue's last label drew no Add: in the render
 * where the save landed the labels' control was still "Saving labels of CORE-1", that render's
 * commit renamed it "Add a label to CORE-1", and nothing drew the menu again
 * (dispatch://INFRA-3945 R5). The open menu's entries are read while it renders, which comes
 * after a commit, since it opens on a press.
 */
export function IssueAddMenu({
  isClosed,
  issue,
  onOpenField,
}: {
  isClosed: boolean;
  issue: IssueDetails;
  onOpenField: (field: IssueField) => void;
}): ReactNode {
  const [open, setOpen] = useState(false);
  // Whether any entry can be reached, as the layout effect below reads it after a commit: false
  // until the first one, which that effect corrects before the page is painted.
  const [anyReachable, setAnyReachable] = useState(false);
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
    // Escape at every other width closes Details on it.
    const closeOnEscape = (event: KeyboardEvent) => {
      if (
        event.key !== "Escape" ||
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
  // After every commit of the menu, since any of them can draw, rename or remove a control an
  // entry reaches. The answer is a boolean, so a commit that leaves it as it was sets nothing and
  // draws the menu no further time.
  useLayoutEffect(() => {
    setAnyReachable(fields.some((field) => field.reachable()));
  });

  if (isClosed || !anyReachable) return null;
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
          {fields
            .filter((field) => field.reachable())
            .map((field) => (
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
    // The name of a control the panel draws is that control's own, built by the function it
    // exports: neither side spells it, so renaming one renames both and the entry cannot go
    // missing on a word nobody meant to change. A control of the header - the assignee - is
    // reached with Details closed, since on a phone Details covers it; a control of Details is
    // reached with Details open, since shut it takes neither a press nor the focus.
    const atControl = (name: string) => ({
      open: () => {
        const control = pageControl(name);
        const reach = () => {
          if (control instanceof HTMLSelectElement) control.focus();
          else control?.click();
        };
        const inHeader = control?.closest("[data-issue-header]") != null;
        const shut = control?.closest("[hidden]") != null;
        if (drawer !== undefined && (inHeader ? drawer.open : shut && !drawer.open)) {
          drawer.setOpen(!inHeader);
          requestAnimationFrame(() => requestAnimationFrame(reach));
          return;
        }
        reach();
      },
      reachable: () => pageControl(name) !== null,
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
      fields.push({
        label: PROPERTY_LABELS.assignee,
        ...atControl(assigneeControlName(details.key)),
      });
    }
    if ((details.labels ?? []).length === 0) {
      // Listed only while the issue has no label, so the control is found by the name it has
      // then: the words it draws and the issue's key ("Add a label to CORE-1"). A save in flight
      // renames it ("Saving labels of CORE-1"), and the busy control is not reached, so the name
      // is the one it has with no save in flight (`saving: false`).
      fields.push({
        label: PROPERTY_LABELS.labels,
        ...atControl(
          labelsControlName(details.key, {
            labels: [],
            saving: false,
            words: { ANSWER_LINES, CONTROL_LABELS },
          })
        ),
      });
    }
    if (details.route === null || details.route === "") {
      fields.push({
        label: PROPERTY_LABELS.route,
        open: () => onOpenField("route"),
        reachable: () => true,
      });
    }
    if ((details.blocked_by ?? []).length === 0) {
      fields.push({
        label: PROPERTY_LABELS.blocked_by,
        ...atControl(blockedByControlName(details.key, 0)),
      });
    }
    if (details.repo === null) {
      fields.push({
        label: PROPERTY_LABELS.repository,
        ...atControl(repoControlName(details.key)),
      });
    }
    if ((details.components.ids ?? []).length === 0) {
      // Listed only while the issue names no component, so the picker's trigger is found by the
      // name it has then, from the attachment the components line draws: the words it draws
      // and the issue's key ("Set components of CORE-1" on an issue not attached, or attached
      // to none). A save in flight renames it, and the busy trigger is not reached, so the name
      // is the one it has with no save in flight (`saving: false`).
      fields.push({
        label: PROPERTY_LABELS.components,
        ...atControl(
          componentsControlName(details.key, {
            components: details.components,
            saving: false,
            words: { ANSWER_LINES, CONTROL_LABELS },
          })
        ),
      });
    }
    if ((details.advances ?? []).length === 0) {
      fields.push({
        label: PROPERTY_LABELS.advances,
        ...atControl(addPriorityControlName(details.key)),
      });
    }
    return fields;
  }
}

/** One property with no value: what it is called, whether its control is on the page at all, and
 *  how to open it. */
interface AddableField {
  readonly label: string;
  readonly open: () => void;
  readonly reachable: () => boolean;
}

/** The control named `name`, anywhere on the page. The fields the panel draws are edited by
 *  their own controls; this is how the add menu reaches one instead of drawing it twice. */
function pageControl(name: string): HTMLElement | null {
  return document.querySelector<HTMLElement>(`[aria-label="${CSS.escape(name)}"]`);
}
