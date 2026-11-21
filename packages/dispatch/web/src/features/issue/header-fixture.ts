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

/** Answers with nothing the two reads the header makes at once, so neither reaches the network:
 *  the project's repository mappings, read for an issue that resolves to no repository - a
 *  project that maps none still shows the repository item, with nothing to suggest - and its
 *  architecture sources, which the components line reads the same way and which, with none,
 *  offer no picker. Answers what restores both. */
export function mockHeaderReads(): () => void {
  const mappings = spyOn(api, "listRepoProjects").mockResolvedValue([]);
  const architectureSources = spyOn(api, "listProjectArchitectureSources").mockResolvedValue([]);
  return () => {
    mappings.mockRestore();
    architectureSources.mockRestore();
  };
}
