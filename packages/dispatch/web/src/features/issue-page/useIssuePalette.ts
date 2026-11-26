import type { IssuePriorityWrite } from "../issue/useIssuePriority";
import { useKeymap } from "../shell/keymap";

/**
 * The issue actions that are reachable from the command palette alone (`$mod+k`): closing and
 * reopening an issue are too consequential for a single key, and the priority has both the
 * palette and `0`-`3`. Each row runs the same handler and carries the same guard as the control
 * the identity block draws, so a row is offered exactly while that control would take a click.
 *
 * It registers into the `issue` scope the view row pushes (`useIssueKeymap.ts`); it never
 * pushes one of its own.
 */
export function useIssuePalette({
  isClosed,
  priorityWrite,
  requestStatus,
  saving,
}: {
  isClosed: boolean;
  priorityWrite: IssuePriorityWrite;
  /** The header's Close and Reopen (`IssueHeaderWrites.requestStatus`), which ask the rule's
   *  question where Dispatch would refuse the status. */
  requestStatus: (status: string) => void;
  /** Whether a write of the issue's own fields (`updateIssue`) is in flight: Close issue and
   *  Reopen issue, offered while one is, would submit a second, so their menu items refuse it. */
  saving: boolean;
}): void {
  useKeymap("issue", [
    {
      id: "close",
      keys: [],
      label: "Close issue",
      run: () => requestStatus("done"),
      when: () => !isClosed && !saving,
    },
    {
      id: "reopen",
      keys: [],
      label: "Reopen issue",
      run: () => requestStatus("backlog"),
      when: () => isClosed && !saving,
    },
    ...([0, 1, 2, 3] as const).map((level) => ({
      id: `set-p${level}`,
      keys: [],
      label: `Set priority P${level}`,
      run: () => priorityWrite.submit(level),
      // `PriorityEditor`'s `disabled` (`IssuePropertiesRow.tsx`), so the row is offered while
      // the priority takes input. The write goes in the issue's line of the keyed store
      // (`target: issue:<key>`, `useIssueFieldWrite.ts`), so a row run while another write of
      // the issue is in flight is sent after it rather than beside it.
      when: () => !isClosed,
    })),
  ]);
}
