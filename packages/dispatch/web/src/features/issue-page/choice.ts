import { useQuery } from "@tanstack/react-query";
import { useCallback } from "react";

import { whoAmIQuery } from "../../api/queries";
import {
  type IssuePageChoice,
  issuePageChoiceChanged,
  issuePageChoiceKey,
  useStoredIssuePageChoice,
} from "./stored-choice";

/**
 * The reader's issue page and the one way to change it, kept in this browser for the signed-in
 * login (`dispatch.issue.page:<login>`); Dispatch holds nothing of it. The choice is read from
 * the one store of the tab (`useStoredIssuePageChoice` in `stored-choice.ts`, which says why it
 * is not `useUserPreference`), and a write here tells every reader of that store at once.
 *
 * With nobody signed in the choice is `tabs` and a write stores nothing. A write the browser
 * refuses (private mode, a full quota) changes nothing, as `recordRecent` in
 * `features/search/recents.ts` does.
 */
export function useIssuePageChoice(): [IssuePageChoice, (next: IssuePageChoice) => void] {
  const login = useQuery(whoAmIQuery()).data?.login;
  const choice = useStoredIssuePageChoice(login);
  const choose = useCallback(
    (next: IssuePageChoice) => {
      if (login === undefined) return;
      try {
        window.localStorage.setItem(issuePageChoiceKey(login), next);
      } catch {
        // The browser kept nothing, so every caller keeps the page it has.
        return;
      }
      issuePageChoiceChanged();
    },
    [login]
  );
  return [choice, choose];
}
