import type { ReactNode } from "react";

import type { IssueDetails } from "../../api/types";
import { safeExternalHref } from "../../lib/external-href";
import { focusVisibleRing, pageInkText, pageMutedText } from "../../theme/classes";
import { authorText, pageChrome } from "./page-structure";
import { PULL_REQUEST_STATE_WORDS } from "./page-words";

/**
 * The header's one link to the pull request `pull_request_heads` attributes to the issue (part
 * N's R22, `IssuePropertiesRow.tsx`): drawn on the properties line after the assignee and before
 * the children's progress and the count of what waits on the reader, with its number and its
 * state in the page's own words. There it stands on the line of the status, the priority and the
 * assignee, which at 390 px has room for it on each seed `c-pull-request-link` reads; after the
 * progress it shared the line of the count and the labels, and on a long title's issue with no
 * approval request waiting it pushed the labels onto a line of their own. An issue with none, or
 * with an address that is neither `http:` nor `https:`, draws nothing.
 */
export function PullRequestLink({
  pullRequest,
}: {
  pullRequest: IssueDetails["pull_request"];
}): ReactNode {
  if (pullRequest === undefined) return null;
  const href = safeExternalHref(pullRequest.url);
  if (href === null) return null;
  return (
    <a
      className={`inline-flex min-h-6 shrink-0 items-center gap-1 text-xs leading-5 font-medium underline ${pageInkText} ${focusVisibleRing}`}
      data-testid="issue-pull-request"
      href={href}
      rel="noreferrer"
      target="_blank"
      {...pageChrome}
    >
      <span {...authorText}>{`#${pullRequest.number}`}</span>{" "}
      <span className={`font-normal ${pageMutedText}`}>
        {PULL_REQUEST_STATE_WORDS[pullRequest.state]}
      </span>
    </a>
  );
}
