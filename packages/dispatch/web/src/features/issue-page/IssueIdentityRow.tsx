import type { ReactNode } from "react";
import { Link } from "react-router-dom";

import { pageHeaderControl } from "../../theme/classes";
import { linkHoverText, pageInkText, pageQuietText } from "../../theme/shell-classes";
import { CopyRefButton } from "../refs/CopyRefButton";
import { buildIssuePath } from "../refs/page-routes";
import { type IssueAction, IssueActionsMenu } from "./IssueActionsMenu";
import { DETAILS_ATTRIBUTE, useMarginDrawer } from "./margin-slot";
import { authorText, pageChrome } from "./page-structure";
import { CONTROL_LABELS } from "./page-words";
import { useShellNavigation } from "./shell-navigation";

/**
 * Row one of the header, in this order: the breadcrumb - the project, then the parent where the
 * issue has one, each as the way back up to it - the issue's key, the control that copies it, and
 * the one overflow menu that holds the pin, close, reopen and the control that sets the parent.
 * At its right end, Details opens the panel of every other property over the right of the page;
 * below the phone's width the row starts with the control that opens the navigation, three
 * strokes drawn as a shape, where the shell draws no rail (the 390 px picture).
 *
 * It is the prototype's `.row1` and `.crumb`: the crumbs in the quiet grey, the key in the ink.
 */
export function IssueIdentityRow({
  actions,
  issueKey,
  parent,
  project,
}: {
  actions: readonly IssueAction[];
  issueKey: string;
  /** The issue this one sits under, drawn as the way back up to it. */
  parent: string | null;
  project: string;
}): ReactNode {
  const navigation = useShellNavigation();
  const details = useMarginDrawer();
  return (
    // The row wraps rather than cutting a key: at 320 px the Details control takes a second
    // line, and nothing on the row is ever drawn short.
    <div className="flex min-h-[22px] min-w-0 flex-wrap items-center gap-x-2.5 gap-y-1">
      {navigation === undefined ? null : (
        <button
          aria-expanded={navigation.open}
          aria-haspopup="dialog"
          aria-label="Open navigation"
          className={pageHeaderControl}
          onClick={navigation.onOpen}
          type="button"
        >
          {/* A shape and not the text ☰, which the page's typeface lacks: as text it was drawn in
              a face of the reader's own machine, at that face's width. It hides itself from
              assistive technology and holds no words; its span keeps the control's 20 px line,
              so the control is as tall as its one character made it. */}
          <span>
            <svg
              aria-hidden="true"
              className="inline-block size-[1em] align-middle"
              viewBox="0 0 16 16"
            >
              <path
                d="M2.5 4h11M2.5 8h11M2.5 12h11"
                fill="none"
                stroke="currentColor"
                strokeLinecap="round"
                strokeWidth="1.5"
              />
            </svg>
          </span>
        </button>
      )}
      <div
        className={`flex min-w-0 flex-wrap items-center gap-1.5 text-xs leading-[18px] font-medium ${pageQuietText}`}
      >
        <span className="min-w-0 [overflow-wrap:anywhere]" {...authorText}>
          {project}
        </span>
        <span aria-hidden="true" className="shrink-0" {...pageChrome}>
          ›
        </span>
        {parent === null ? null : (
          <>
            {/* No `dispatch-inline-link`: that class is for a link inside a sentence's line box,
                and this link is a flex item of row one, where the class's 6 px of padding below
                1280 px makes the row 30 px tall and the band 69 px, not the 63 px the band is
                held to. The header's own rule in `styles.css` takes the 44 px floor off it. */}
            <Link
              className={`min-w-0 underline underline-offset-2 [overflow-wrap:anywhere] ${linkHoverText}`}
              to={buildIssuePath({ key: parent, kind: "issue" })}
              {...authorText}
            >
              {parent}
            </Link>
            <span aria-hidden="true" className="shrink-0" {...pageChrome}>
              ›
            </span>
          </>
        )}
        <span className={`shrink-0 font-semibold ${pageInkText}`} {...authorText}>
          {issueKey}
        </span>
        {/* 24 by 24 px at every width, the least a target may be (WCAG 2.5.8) and the height of
            row one's other controls, so the row keeps its height. `!` wins over the shared
            control's own floors, 44 px and 32 px from `md`. */}
        <CopyRefButton
          className="size-6 min-h-6! min-w-6!"
          primary={{ label: `issue key ${issueKey}`, value: issueKey }}
          route={{ key: issueKey, kind: "issue" }}
        />
      </div>
      <IssueActionsMenu actions={actions} />
      <span className="flex-1" />
      {details === undefined ? null : (
        // A press leaves the focus where it is, so Details opens on the node the reader stood
        // on: a button takes no focus from a click in every browser, and the outline reads the
        // focus moving to nothing as the reader leaving it (`IssueTree.tsx`).
        <button
          aria-expanded={details.open}
          className={pageHeaderControl}
          data-testid="issue-details-control"
          {...{ [DETAILS_ATTRIBUTE]: "" }}
          onClick={() => details.setOpen(!details.open)}
          onMouseDown={(event) => event.preventDefault()}
          type="button"
          {...pageChrome}
        >
          {CONTROL_LABELS.details}
        </button>
      )}
    </div>
  );
}
