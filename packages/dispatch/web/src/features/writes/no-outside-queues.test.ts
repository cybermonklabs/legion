import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { join } from "node:path";

import { shippedSources } from "../../__tests__/source-scan";

/**
 * No write of a row is ordered by a queue outside the store (dispatch://INFRA-299/spec#b-r8).
 * Three engines write one row each - `useIssueFieldWrite`, `useIssueDrafts` and
 * `useCommentActionQueue` - and each writes through the store's line (`issue:<KEY>`,
 * `comment:<id>`) rather than ordering its own presses. So this file reads those three modules
 * and names any queue of their own in one - a pending slot, a chained snapshot, a drain of
 * pending submits, a per-row action map, a press held in hand - and it reads every
 * `useSubmitGuard` the app ships and holds that set to the sixteen controls that refuse a
 * second press of their own control, which is a refusal and not an order.
 *
 * Three mechanisms order writes of different rows, which no line of one row can, and the rows
 * that prove them are their own features':
 * - the issues table's bounded fan-out across rows: `a bulk action keeps at most eight writes in
 *   flight` (`features/project/IssuesTable.test.tsx`);
 * - the board's mutation scope, which keeps two drops of different cards in press order: `two
 *   rapid moves on one board run one after the other, the second from the optimistic state`
 *   (`features/project/IssueBoard.test.tsx`);
 * - `features/issue/pin-write.ts`'s re-base on a stale answer: `a pin Dispatch refuses as stale
 *   is written again over the row it answered with` (`features/issue/pin-write.test.ts`).
 *
 * Two behaviours of the comment list - an action on another comment waits for the one in flight,
 * and a failure clears the actions still waiting - are read from the store's own rows in
 * `features/margin/useMarginItems.test.ts`.
 */

/** `web/src`, which every path below is read from. */
const webSrc = join(import.meta.dir, "..", "..");

function sourceOf(path: string): string {
  return readFileSync(join(webSrc, path), "utf8");
}

/** The three engines that write one row, none of which orders those writes itself. */
const engines = [
  "features/issue/useIssueDrafts.ts",
  "features/issue/useIssueFieldWrite.ts",
  "features/margin/useCommentActionQueue.ts",
];

/** What a queue of one row reads like inside one of those engines. */
const queues: readonly { readonly name: string; readonly pattern: RegExp }[] = [
  { name: "a submit guard", pattern: /useSubmitGuard/ },
  { name: "a drain of pending submits", pattern: /drainPendingSubmits/ },
  { name: "a pending reference", pattern: /Pending(?:Ref|Submit)\b/ },
  { name: "a pending slot", pattern: /setPending[A-Z]/ },
  { name: "an action map", pattern: /new Map\b/ },
  { name: "a press held in hand", pattern: /inFlight\b/ },
  { name: "a chained snapshot", pattern: /chainedSnapshot\b/ },
];

test("the three engines that write one row keep no queue of their own", () => {
  const found = engines.flatMap((path) => {
    const source = sourceOf(path);
    return queues
      .filter(({ pattern }) => pattern.test(source))
      .map(({ name }) => `${path} holds ${name}`);
  });
  expect(found).toEqual([]);
});

// What the deleted queues did, the store's line does: each press is a write of the row it writes,
// and two writes of one row go out in press order.
test("each engine names the line of the row it writes", () => {
  expect(sourceOf("features/issue/useIssueFieldWrite.ts")).toMatch(
    /target: `issue:\$\{issueKey\}`/
  );
  expect(sourceOf("features/margin/useCommentActionQueue.ts")).toMatch(
    /target: `comment:\$\{input\.id\}`/
  );
  // Every draft save is a press of the one write the header owns, on the issue's own line.
  expect(sourceOf("features/issue/useIssueDrafts.ts")).toContain("updateIssue.mutateAsync(input)");
  expect(sourceOf("features/issue/IssueHeader.tsx")).toMatch(
    /target: \(\) => `issue:\$\{issue\.key\}`/
  );
});

// A guard that refuses a second press of its own control while its write is out is a refusal and
// not an order, so these sixteen stay as they are. One module holds two of them:
// `DocumentWayBack.tsx`'s title correction and its removal each guard their own press.
test("the sixteen controls that refuse a second press of their own keep their guard", () => {
  const guarded = shippedSources().filter(
    (path) => path !== "hooks/useSubmitGuard.ts" && sourceOf(path).includes("useSubmitGuard()")
  );
  expect(guarded).toEqual([
    "features/agents/BroadcastSends.tsx",
    "features/artifacts/ArtifactUpload.tsx",
    "features/artifacts/CreateDocument.tsx",
    "features/artifacts/DocumentWayBack.tsx",
    "features/conversation/ConversationTab.tsx",
    "features/credentials/MachineLoginPage.tsx",
    "features/credentials/RevocableList.tsx",
    "features/delivery/DeliverySettingsForm.tsx",
    "features/doc/ApprovalChip.tsx",
    "features/inbox/DeclineControl.tsx",
    "features/inbox/useAskAnswerForm.ts",
    "features/issue/CreateIssueDialog.tsx",
    "features/issue/IssueHeader.tsx",
    "features/settings/ArchitectureSourcesSection.tsx",
    "features/settings/ProjectsSection.tsx",
  ]);
});
