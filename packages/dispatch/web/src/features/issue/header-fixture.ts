import { spyOn } from "bun:test";

import { api } from "../../api/client";
import type { IssuePriorityWrite } from "./useIssuePriority";

/** The priority write `IssueHeader` is handed in rows about another part of the header than its
 *  priority: it never runs, and it draws no priority. */
export const idlePriorityWrite: IssuePriorityWrite = {
  failed: false,
  pending: false,
  retry: () => undefined,
  shown: null,
  stopped: undefined,
  submit: () => undefined,
};

/** Answers with nothing the one read the header makes at once that the unit suite's network does
 *  not answer, so it does not reach the network: the project's architecture sources, which the
 *  components line reads at once and which, with none, offer no picker. The repository list reads
 *  the project list, which that network answers (`__tests__/network.ts`). Answers what restores
 *  it. */
export function mockHeaderReads(): () => void {
  const architectureSources = spyOn(api, "listProjectArchitectureSources").mockResolvedValue([]);
  return () => architectureSources.mockRestore();
}
