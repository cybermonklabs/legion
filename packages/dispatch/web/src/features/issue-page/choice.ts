import { useQuery } from "@tanstack/react-query";
import { useCallback } from "react";

import { whoAmIQuery } from "../../api/queries";
import { useUserPreference } from "../shell/userPreference";

/** Which page an issue's address draws for the reader: `outline` is the new issue page, `tabs`
 *  is today's. */
export type IssuePageChoice = "outline" | "tabs";

/** The choice as it is stored. Module-level, so the setter `useUserPreference` returns keeps one
 *  identity across renders. */
function writeChoice(choice: IssuePageChoice): string {
  return choice;
}

/**
 * The reader's issue page and the one way to change it, kept in this browser for the signed-in
 * login (`dispatch.issue.page:<login>`); Dispatch holds nothing of it. Whatever needs the choice
 * calls this hook, and `useUserPreference` gives a choice made through one caller to every
 * caller at once, and one made in another tab too.
 *
 * With nobody signed in the choice is `tabs` and a choice changes nothing. A write the browser
 * refuses (private mode, a full quota) changes nothing either, as `recordRecent` in
 * `features/search/recents.ts` does.
 */
export function useIssuePageChoice(): [IssuePageChoice, (next: IssuePageChoice) => void] {
  const login = useQuery(whoAmIQuery()).data?.login;
  // Nothing stored, and a stored word that is neither page's, read as today's page.
  const [choice, store] = useUserPreference<IssuePageChoice>(
    "issue.page",
    (stored) => (stored === "outline" ? "outline" : "tabs"),
    writeChoice
  );
  const choose = useCallback(
    (next: IssuePageChoice) => {
      if (login === undefined) return;
      try {
        store(next);
      } catch {
        // The browser kept nothing, so every caller keeps the page it has.
      }
    },
    [login, store]
  );
  return [choice, choose];
}
