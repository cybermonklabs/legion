import { useCallback } from "react";

import { useStoredUserPreference } from "../shell/userPreference";

/** Which page an issue's address draws for the reader: `outline` is the new issue page, `tabs`
 *  is today's. */
export type IssuePageChoice = "outline" | "tabs";

/** The stored choice. Nobody signed in, nothing stored, and a stored word that is neither page's
 *  all read as today's page. */
const readChoice = (stored: string | null): IssuePageChoice =>
  stored === "outline" ? "outline" : "tabs";

const writeChoice = (choice: IssuePageChoice): string => choice;

/**
 * The reader's issue page and the one way to change it, kept in this browser for the signed-in
 * login (`dispatch.issue.page:<login>`) through `useStoredUserPreference`; Dispatch holds nothing
 * of it. With nobody signed in the choice is `tabs`. Nothing else reads the stored value:
 * whatever needs the choice calls this hook.
 */
export function useIssuePageChoice(): [IssuePageChoice, (next: IssuePageChoice) => void] {
  const [choice, change] = useStoredUserPreference("issue.page", readChoice, writeChoice);
  const choose = useCallback((next: IssuePageChoice) => change(() => next), [change]);
  return [choice, choose];
}
