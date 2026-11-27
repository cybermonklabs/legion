import { Fragment, type ReactNode } from "react";
import { Link } from "react-router-dom";

import type { IssueChild } from "../../api/types";
import { pageKindTag } from "../../theme/classes";
import { pageInkText, pageQuietText } from "../../theme/shell-classes";
import { AgentName } from "../refs/AgentName";
import { buildIssuePath } from "../refs/page-routes";
import { authorText, PROPERTY_LABELS, pageChrome } from "./page-structure";
import { issueStatusWord } from "./page-words";

/**
 * What a child issue's row of the outline draws beside its title (R12, the
 * architect's ruling 5, the prototype's `.kr`): the children are one group of rows of the outline,
 * each child a row the reader focuses and moves through as any other, and no section above the
 * outline. The row's words are the child's title (`tree-drafts.ts`'s `addChildren`); after them,
 * in the margin's small grey, how it stands, what blocks it and who holds it
 * (`ChildIssueFacts`); and its key is the row's tag, a link to its own page (`ChildIssueTag`).
 * The header keeps the children's bar and both counts (R13).
 *
 * Every colour is the page's own palette: the key in the child kind's colour, the keys that
 * block it in the ink with the bullets' grey under them, as a link in a node's words is drawn,
 * and its holder in the row's grey, as words with no link (R5). Each link is out of the tab
 * order, as every control inside a row of the outline is, unless `tabStops` holds: a view that
 * no tree's keys reach keeps them in it (R9).
 */

/** The key of a child issue as its row's tag: the child's own page, in the child kind's colour
 *  (the prototype's `tagH`). */
export function ChildIssueTag({
  child,
  tabStops = false,
}: {
  child: IssueChild;
  tabStops?: boolean;
}): ReactNode {
  return (
    <Link
      className={`dispatch-inline-link font-semibold whitespace-nowrap ${pageKindTag.child}`}
      data-child-key=""
      tabIndex={tabStops ? undefined : -1}
      to={buildIssuePath({ key: child.key, kind: "issue" })}
      {...authorText}
    >
      {child.key}
    </Link>
  );
}

/**
 * How a child stands, what blocks it and who holds it, after its title on its row, each in the
 * page's own words and the child's own values. A part a child has no value for is not drawn:
 * the issue read leaves `needs` out for a child that waits on nothing and `claim` out for one
 * nobody holds. The holder is named as the page names an author (`AgentName`'s `author`): by a
 * session's title or a person's login, never by a session's id, and in no link, since the row's
 * one link to the child is its key. The keys and the holder can hold a run no space breaks, so a
 * part breaks inside a word only where no space will do (`overflow-wrap: anywhere`), and the page
 * never scrolls sideways (R17).
 */
export function ChildIssueFacts({
  child,
  tabStops = false,
}: {
  child: IssueChild;
  tabStops?: boolean;
}): ReactNode {
  const blockers = child.needs ?? [];
  return (
    <span
      className={`ml-1.5 text-xs leading-6 [overflow-wrap:anywhere] ${pageQuietText}`}
      data-child-facts=""
      data-child-status={child.status}
    >
      <span {...pageChrome}>{issueStatusWord(child.status)}</span>
      {blockers.length === 0 ? null : (
        // Two keys run together read as one key ("CORE-2CORE-3"), so each follows the last
        // after a comma and a space, as a reader reads a list of keys in a line of text.
        <span data-testid="issue-child-blocked-by">
          <span {...pageChrome}> · </span>
          <span {...pageChrome}>{PROPERTY_LABELS.blocked_by}</span>{" "}
          {blockers.map((key, index) => (
            <Fragment key={key}>
              {index === 0 ? null : ", "}
              <Link
                className={`dispatch-inline-link underline decoration-page-bullet underline-offset-[3px] ${pageInkText}`}
                tabIndex={tabStops ? undefined : -1}
                to={buildIssuePath({ key, kind: "issue" })}
                {...authorText}
              >
                {key}
              </Link>
            </Fragment>
          ))}
        </span>
      )}
      {/* `== null`: a Dispatch older than this contract answers a child nobody holds with
          `claim: null`, and during a rollout two builds answer one load balancer
          (`e2e/two-builds.multiprocess.e2e.ts`). */}
      {child.claim == null ? null : (
        <span>
          <span {...pageChrome}> · </span>
          <span {...pageChrome}>{PROPERTY_LABELS.claim}</span>{" "}
          <span data-child-holder="" {...authorText}>
            <AgentName actor={child.claim.actor} author kind="static" />
          </span>
        </span>
      )}
    </span>
  );
}
