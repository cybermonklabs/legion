import type { ReactNode } from "react";
import { Link } from "react-router-dom";

import { linkHoverText, pageHeaderControl, pageInkText, pageQuietText } from "../../theme/classes";
import { DETAILS_ATTRIBUTE, useMarginDrawer } from "../margin/margin-slot";
import { CopyRefButton } from "../refs/CopyRefButton";
import { buildIssuePath } from "../refs/routes";
import { useShellNavigation } from "../shell/shell-navigation";
import { type IssueAction, IssueActionsMenu } from "./IssueActionsMenu";
import { authorText, pageChrome } from "./page-structure";
import { CONTROL_LABELS } from "./page-words";

/**
 * Row one of the header (dispatch://INFRA-1196 R13), in R13's order: the breadcrumb - the
 * project, then the parent where the issue has one, each as the way back up to it - the issue's
 * key, the control that copies it, and the one overflow menu that holds the pin, close, reopen
 * and the control that sets the parent. At its right end, Details opens the panel of every other
 * property over the right of the page (R14); below the phone's width the row starts with the
 * `☰` that opens the navigation, where the shell draws no rail (the 390 px picture).
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
    // line, and nothing on the row is ever drawn short (R17).
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
          <span aria-hidden="true" {...pageChrome}>
            ☰
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
            <Link
              className={`dispatch-inline-link min-w-0 underline underline-offset-2 [overflow-wrap:anywhere] ${linkHoverText}`}
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
        <CopyRefButton
          className="size-[22px] min-h-6! min-w-6! md:min-h-0! md:min-w-0!"
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
