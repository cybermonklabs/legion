import type { ReactNode } from "react";

import type { IssueDetails, UserState } from "../../api/types";

/** What each of Details' three places below is handed: the issue as the details slot reads it,
 *  and the reader's own state of every issue. */
export interface DetailsMoreProps {
  readonly issue: IssueDetails;
  readonly state: UserState | undefined;
}

/** The people of the issue, under Opened by in Details' People section: its subscribers and its
 *  collaborators. Nothing fills it yet, so it draws nothing. */
export function DetailsPeople(_props: DetailsMoreProps): ReactNode {
  return null;
}

/** The facts of the row the reader's focus is on, between Details' sections and Pinned. Nothing
 *  fills it yet, so it draws nothing. */
export function DetailsFocusedNode(_props: DetailsMoreProps): ReactNode {
  return null;
}

/** The issue's history, under Pinned. Nothing fills it yet, so it draws nothing. */
export function DetailsHistory(_props: DetailsMoreProps): ReactNode {
  return null;
}
