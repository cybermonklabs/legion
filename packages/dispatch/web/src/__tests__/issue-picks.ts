/** What the unit rows of an issue's picks share (`issue-field-writes.test.tsx`,
 *  `assignee-repo-picks.test.tsx`): the issue picked and its detail, the page's query client
 *  signed in, what each save Dispatch received carried, the repository box's suggestions and
 *  picks, and a pick from the shared list's single pick. */
import type { QueryKey } from "@tanstack/react-query";
import { within } from "@testing-library/react";
import type { UserEvent } from "@testing-library/user-event";

import { DispatchQueryClient } from "../api/query-client";
import type { Issue, IssueDetails } from "../api/types";
import { panelOf } from "./list-picks";

/** CORE-1, the issue every row picks a field of, as Dispatch answers a save of it. */
export const issue: Issue = {
  advances: [],
  advances_revision: 0,
  assignee: "alice",
  blocked_by: [],
  claim: null,
  closed_at: null,
  components: { mode: "inherit", ids: [], unknown: [], reason: null, inherited_from: null },
  created_at: "2026-09-09T00:00:00Z",
  created_by: { id: "alice", kind: "user" },
  decisions_artifact_id: "",
  external_links: [],
  key: "CORE-1",
  labels: [],
  last_seq: 1,
  number: 1,
  parent: null,
  plan_artifact_id: "",
  primary_artifact_id: "artifact-1",
  priority: 1,
  project: "CORE",
  rank: "U",
  repo: null,
  resolved_repo: null,
  route: null,
  status: "todo",
  title: "Review the spec",
  updated_at: "2026-09-09T00:00:00Z",
  verification_artifact_id: "",
};

/** CORE-1 as Dispatch answers a read of its detail. */
export const details: IssueDetails = {
  holder_state: null,
  last_seen: null,
  route_holder: null,
  route_status: null,
  route_status_reason: null,
  takeable_at: null,
  ...issue,
  artifacts: [],
  children: [],
  design_gates: [],
  open_asks: [],
  progress: { tasks: null, children: null },
  referenced_by_count: 0,
};

/** The page's query client, signed in as alice, holding what Dispatch last answered under each
 *  of `cached`'s keys. */
export function signedInHolding(
  ...cached: readonly (readonly [QueryKey, unknown])[]
): DispatchQueryClient {
  const client = new DispatchQueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  client.setQueryData(["whoami"], { event_head: "0", kind: "user", login: "alice" });
  for (const [queryKey, data] of cached) client.setQueryData(queryKey, data);
  return client;
}

/** What each save Dispatch received carried. */
export function bodies(patchIssue: { mock: { calls: unknown[][] } }): unknown[] {
  return patchIssue.mock.calls.map((call) => call[1]);
}

/** The repositories `box` suggests: its `<datalist>`'s option values, in the box's order. */
export function repoSuggestions(box: HTMLInputElement): string[] {
  return Array.from(box.list?.options ?? [], (option) => option.value);
}

/** Picks the row named `name` of the single pick `trigger` opens, which writes it, opening its
 *  list unless it stands open. The list is portalled out of the control's container, so the row
 *  is found in the panel the trigger names. */
export async function pickIn(
  user: UserEvent,
  trigger: () => HTMLElement,
  name: string
): Promise<void> {
  if (trigger().getAttribute("aria-expanded") !== "true") await user.click(trigger());
  await user.click(await within(panelOf(trigger())).findByRole("option", { name }));
}

/** Empties `box`, types `repo` and presses Enter, which writes it; `""` is an Inherit pick. */
export async function typeRepo(
  user: UserEvent,
  box: () => HTMLInputElement,
  repo: string
): Promise<void> {
  await user.clear(box());
  if (repo !== "") {
    await user.click(box());
    await user.paste(repo);
  }
  await user.keyboard("{Enter}");
}
