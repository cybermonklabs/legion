/** What the unit rows of an issue's picks share (`issue-field-writes.test.tsx`,
 *  `assignee-repo-picks.test.tsx`): the issue picked and its detail, the page's query client
 *  signed in, what each save Dispatch received carried, what the shared list's single picks
 *  show, suggest and pick, and the priority's menu.
 *
 *  This tree's priority is a menu, not a native select (INFRA-2160's R4): its trigger is a button
 *  named by the menu and then by what it shows (`ChoiceMenu`), so it is found by the start of its
 *  name, and a pick is a press of the trigger and then of the choice, as a reader makes it, and
 *  writes only then. */
import type { QueryKey } from "@tanstack/react-query";
import { screen, within } from "@testing-library/react";
import type { UserEvent } from "@testing-library/user-event";

import { DispatchQueryClient } from "../api/query-client";
import type { Issue, IssueDetails } from "../api/types";
import { panelOf, pickNamed } from "./list-picks";

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

/** The words `element` holds that assistive technology reads: its text less what it hides, such
 *  as a badge beside the words that read it or the mark of a picked row. A single pick's trigger
 *  holds what it shows and not its label, so for a trigger these are what its name reads after
 *  the label. */
export function shownBy(element: Element): string {
  const words = element.cloneNode(true) as Element;
  for (const hidden of words.querySelectorAll('[aria-hidden="true"]')) hidden.remove();
  return words.textContent ?? "";
}

/** The repositories the open Repository list in `panel` suggests: the labels of its options after
 *  Inherit, in the list's order, without the mark of the picked one. */
export function repoSuggestions(panel: HTMLElement): string[] {
  return within(panel)
    .getAllByRole("option")
    .map((option) => shownBy(option))
    .filter((label) => label !== "Inherit");
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

/** Picks `repo` from the Repository list `trigger` opens, which writes it; `""` is an Inherit
 *  pick, the list's Inherit row. */
export async function pickRepo(
  user: UserEvent,
  trigger: () => HTMLElement,
  repo: string
): Promise<void> {
  await pickIn(user, trigger, repo === "" ? "Inherit" : repo);
}

/** Every trigger of the menu named `label` (`Priority of CORE-1`) on the screen, in its order. */
export function pickerTriggers(label: string): HTMLElement[] {
  return screen.getAllByRole("button", { name: pickNamed(label) });
}

/** The priority the menu's `trigger` shows, in the words a native `<select>` of it held as its
 *  value: the priority's digit, and `""` where it shows none (Priority). */
export function shownValue(trigger: HTMLElement): string {
  const face = trigger.querySelector("[id$='-face']") ?? trigger;
  return /^P([0-3])$/.exec(face.textContent ?? "")?.[1] ?? "";
}

/** Picks `value` in the priority menu `trigger` opens, as a reader does: a press of the trigger,
 *  then of the choice `P<value>`, or of Unset for `""`. */
export async function pick(user: UserEvent, trigger: HTMLElement, value: string): Promise<void> {
  await user.click(trigger);
  const menu = await screen.findByRole("menu");
  const name = pickNamed(value === "" ? "Unset" : `P${value}`);
  await user.click(within(menu).getByRole("menuitemradio", { name }));
}
