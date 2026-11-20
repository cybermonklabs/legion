import type { Issue, IssueDetails, UpdateIssueInput } from "../../api/types";

/** The answer the tests of the page's writes (`shared-writes.test.ts`, `IssueMargin.test.tsx`,
 *  `issue-writes.test.tsx`) give a PATCH of the issue. */

/** The server answers a PATCH with the narrow `Issue`: no artifacts, children or open asks. */
export function narrowIssue(details: IssueDetails): Issue {
  const { artifacts: _artifacts, children: _children, open_asks: _openAsks, ...rest } = details;
  return rest;
}

/** The server's answer to a PATCH of `issue` with `input`. A null `status` or `title` in the
 *  body means "leave it as it is", which is what the server does with it; every other field the
 *  body names is written through, but for `rank`, `components` and `advances`: the body holds
 *  each in the request's own shape - the rank's neighbours, the components' input, a list of
 *  priority ids - which the answer states resolved instead, so each is left to the issue's own. */
export function patchAnswer(issue: IssueDetails, input: UpdateIssueInput): Issue {
  const {
    rank: _rank,
    components: _components,
    advances: _advances,
    advances_revision: _advancesRevision,
    status,
    title,
    ...update
  } = input;
  return narrowIssue({
    ...issue,
    ...update,
    status: status ?? issue.status,
    title: title ?? issue.title,
  });
}
