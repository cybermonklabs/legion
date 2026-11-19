import { type ReactNode, useEffect, useRef, useState } from "react";

import type { IssueDetails } from "../../api/types";
import { pageHeaderControl } from "../../theme/classes";
import { useMarginDrawer } from "../margin/margin-slot";
import { addPriorityControlName } from "../priorities/IssuePriorityLine";
import { assigneeControlName } from "./AssigneeControl";
import { blockedByControlName } from "./BlockedByControl";
import { componentsControlName } from "./IssueComponentsLine";
import type { IssueField } from "./IssueFieldForms";
import { MENU_CLASS, MENU_ITEM_CLASS } from "./NodeMenu";
import { PROPERTY_LABELS, pageChrome } from "./page-structure";
import { CONTROL_LABELS } from "./page-words";
import { repoControlName } from "./RepoControl";

/**
 * The one control that reaches a property this issue has no value for (dispatch://INFRA-1196
 * R13), in Details. Every property that has a value is drawn where it belongs - in the header or
 * in a row of Details - so nothing is hidden behind this; what it lists is exactly what is not
 * there yet, and picking one opens the control that sets it.
 *
 * Six of the fields it lists are edited by a control drawn elsewhere on the page (the assignee,
 * the labels, the issues this one waits on, the repository, the components and the priorities
 * it advances), so the menu presses those rather than holding a second copy of them. It finds
 * each by the name that control exports - `assigneeControlName`, `blockedByControlName`,
 * `repoControlName`, `componentsControlName`, `addPriorityControlName` - so the two sides
 * cannot drift apart, and lists one only while the page draws it: a project that maps no
 * repository offers no repository to set.
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
  const drawer = useMarginDrawer();
  const wrapper = useRef<HTMLDivElement>(null);
  useEffect(() => {
    if (!open) return;
    const closeOutside = (event: PointerEvent) => {
      if (!(event.target instanceof Node) || wrapper.current?.contains(event.target) !== true) {
        setOpen(false);
      }
    };
    const closeOnEscape = (event: KeyboardEvent) => {
      if (event.key === "Escape") setOpen(false);
    };
    document.addEventListener("pointerdown", closeOutside);
    document.addEventListener("keydown", closeOnEscape);
    return () => {
      document.removeEventListener("pointerdown", closeOutside);
      document.removeEventListener("keydown", closeOnEscape);
    };
  }, [open]);

  const empty = emptyFields(issue).filter((field) => field.reachable());
  if (isClosed || empty.length === 0) return null;
  return (
    <div className="relative flex shrink-0" ref={wrapper}>
      <button
        aria-expanded={open}
        aria-haspopup="menu"
        aria-label={CONTROL_LABELS.add}
        className={pageHeaderControl}
        onClick={() => setOpen((shown) => !shown)}
        type="button"
        {...pageChrome}
      >
        {CONTROL_LABELS.add}
      </button>
      {open ? (
        <div aria-label={CONTROL_LABELS.add} className={`${MENU_CLASS} left-0`} role="menu">
          {empty.map((field) => (
            <button
              className={MENU_ITEM_CLASS}
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
      // The labels popover spells its own name inline (`IssueLabels.tsx`), so this one is typed
      // here until that control exports it too.
      fields.push({ label: PROPERTY_LABELS.labels, ...atControl("Edit labels") });
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
      fields.push({
        label: PROPERTY_LABELS.components,
        ...atControl(componentsControlName(details.key, null)),
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
