import type { IssueDrafts } from "../issue/useIssueDrafts";
import type { IssuePriorityWrite } from "../issue/useIssuePriority";
import { useKeymap } from "../shell/keymap";

/**
 * The issue actions that have no default key (`$mod+k` offers each as a row, until a person gives
 * it a key): closing and reopening an issue are too consequential for a single key, and the
 * priority has both the rows and `0`-`3`. Each row runs the same handler and carries the same
 * guard as the control the identity block draws, so a row is offered exactly while that control
 * would take a click.
 *
 * It registers into the `issue` scope the view row pushes (`useIssueKeymap.ts`); it never
 * pushes one of its own.
 */
export function useIssuePalette({
  drafts,
  isClosed,
  priorityWrite,
  saving,
}: {
  drafts: Pick<IssueDrafts, "requestStatusSubmit">;
  isClosed: boolean;
  priorityWrite: IssuePriorityWrite;
  /** Whether a write of the issue's own fields (`updateIssue`) is in flight: Close issue and
   *  Reopen issue, offered while one is, would submit a second, so their menu items refuse it. */
  saving: boolean;
}): void {
  useKeymap("issue", [
    {
      action: "issue.close",
      run: () => drafts.requestStatusSubmit("done"),
      when: () => !isClosed && !saving,
    },
    {
      action: "issue.reopen",
      run: () => drafts.requestStatusSubmit("backlog"),
      when: () => isClosed && !saving,
    },
    ...([0, 1, 2, 3] as const).map((level) => ({
      action: `issue.set-p${level}` as const,
      run: () => priorityWrite.submit(level),
      // `PriorityEditor`'s `disabled` (`IssuePropertiesRow.tsx`), so the row is offered while
      // the priority takes input. The write goes in the issue's line of the keyed store
      // (`target: issue:<key>`, `useIssueFieldWrite.ts`), so a row run while another write of
      // the issue is in flight is sent after it rather than beside it.
      when: () => !isClosed,
    })),
  ]);
}
