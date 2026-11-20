import { act } from "@testing-library/react";

import { nextFrame } from "../../__tests__/frames";
import { issue } from "../../__tests__/issue-picks";
import type { Issue, IssueClaim, IssueDetails, UpdateIssueInput } from "../../api/types";

// What tests of the issue's writes share: the server's answer to a PATCH of the issue
// (`shared-writes.test.ts`, `IssueMargin.test.tsx`, `issue-writes.test.tsx`), the issue the
// claim's tests claim and release, with the claim they put on it, and the wait the tests of
// Details' focus make before they read where a hand-off left it.

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

/** The claim the tests of the claim's chip and of its row of Details (`ClaimChip.test.tsx`,
 *  `IssueClaimControl.test.tsx`) put on `claimIssue`: a session's, which the agent list names. */
export const sessionClaim: IssueClaim = {
  actor: { kind: "session", id: "session-one", origin: { session_title: "Implementer" } },
  at: "2026-09-24T06:00:00Z",
};

/** The issue those tests claim and release: open, in progress and claimed by no one, with no
 *  priority, so the header draws none. */
export const claimIssue: Issue = {
  ...issue,
  created_at: "2026-09-24T05:00:00Z",
  priority: null,
  status: "in_progress",
  title: "Claim the issue before working it",
  updated_at: "2026-09-24T06:00:00Z",
};

/** Waits past two frames and a task, by which a hand-off or a keeper that waits for a frame or a
 *  task would have moved the focus. */
export async function settleFocus(): Promise<void> {
  await act(async () => {
    await nextFrame();
    await nextFrame();
    const task = Promise.withResolvers<void>();
    setTimeout(task.resolve, 0);
    await task.promise;
  });
}
