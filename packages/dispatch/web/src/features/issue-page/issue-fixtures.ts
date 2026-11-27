import { act } from "@testing-library/react";

import { tick } from "../../__tests__/fake-clock";
import { nextFrame } from "../../__tests__/frames";
import { issue } from "../../__tests__/issue-picks";
import type { Issue, IssueClaim, IssueDetails, UpdateIssueInput } from "../../api/types";
import { zeroDelaysOnFakeClock } from "./outline-fixtures";

// What tests of the issue's writes share: the server's answer to a PATCH of the issue
// (`shared-writes.test.ts`, `IssueMargin.test.tsx`, `issue-writes.test.tsx`), the issue the
// claim's tests claim and release, with the claims they put on it, and the change the tests of
// Details' focus make and wait out before they read where a hand-off left it.

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

/** A claim of `claimIssue` by another session than `sessionClaim`'s, made after it: the issue
 *  claimed again elsewhere. */
export const reviewerClaim: IssueClaim = {
  actor: { id: "session-two", kind: "session", origin: { session_title: "Reviewer" } },
  at: "2026-09-24T07:00:00Z",
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

/**
 * Makes `change` on the fake clock and waits it out there: two frames and a task, then two frames
 * and a task more, by which a hand-off or a keeper that waits for a frame or a task after the
 * change has moved the focus, and so has one that waits for a frame or a task after those.
 * Answers what `change` answers.
 *
 * The change is made on the fake clock because the wait is for the timers it sets: the keeper
 * moves the focus a task after the loss it heard (`focus-keeper.ts`), and a read that the focus
 * did not move proves nothing while that task may still be waiting. On the fake clock `tick(0)`
 * runs it; a timer the change set on the real clock would still be waiting after this, and
 * `zeroDelaysOnFakeClock` refuses to start while a zero-delay timer of the real clock waits, which
 * would carry the change's own past every `tick`. A frame comes on a real turn whichever clock is
 * installed: happy-dom runs it as an immediate, which the fake clock does not hold. A press inside
 * `change` is made by `userOnFakeClock()`'s user, and a wait inside it (`waitFor`, `findBy*`)
 * moves the fake clock as it polls but ends with no turn of the event loop: a change that sets off
 * a read whose answer its last wait does not wait for is made inside an `act` of its own
 * (`await act(async () => …)`), whose end turns the loop and lands that answer there, or the
 * answer updates the page outside `act`.
 */
export async function settleFocus<T>(change: () => Promise<T>): Promise<T> {
  const made = Promise.withResolvers<T>();
  await zeroDelaysOnFakeClock(async () => {
    made.resolve(await change());
    for (let round = 0; round < 2; round += 1) {
      await act(async () => {
        await nextFrame();
        await nextFrame();
      });
      await tick(0);
    }
  });
  return made.promise;
}
