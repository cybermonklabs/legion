import { screen, within } from "@testing-library/react";
import type { UserEvent } from "@testing-library/user-event";

import { userPreferenceStorageKey } from "../features/shell/userPreference";

/** Where alice's browser keeps today's issue header's Details open (`issue.details`). A row that
 *  opens Details removes it when it ends, so the next row of its file, in whatever order the
 *  file runs, starts with Details closed, as a reader's first visit does. */
export const ALICE_DETAILS_KEY = userPreferenceStorageKey("alice", "issue.details");

/** Opens today's issue header's Details as a reader does, by its button, unless it is open
 *  already, and answers the panel. */
export async function openIssueDetails(user: UserEvent): Promise<HTMLElement> {
  const header = await screen.findByTestId("issue-header");
  const details = within(header).getByRole("button", { name: "Details" });
  if (details.getAttribute("aria-expanded") !== "true") await user.click(details);
  return within(header).findByTestId("issue-details");
}
