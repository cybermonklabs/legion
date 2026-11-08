import { useQuery, useQueryClient } from "@tanstack/react-query";
import type { ReactNode } from "react";

import { newIssueSettingsQuery } from "../../api/queries";
import { useWrites, type Writes } from "../writes/write-store";
import { type FiledIssue, fileNewIssue, type NewIssueWrite } from "./new-issue";
import { barNoteClassName, type Offer, type OfferRow, refusalText, useWordsState } from "./offer";

/** The group the offer's one row sits in, and the row's own id, which the bar's arrows and
 *  `aria-activedescendant` read. */
export const NEW_ISSUE_GROUP_ID = "search-group-new-issue";
export const NEW_ISSUE_OPTION_ID = "search-option-new-issue";

/** Where the offer stands once a press has been made: the writes out, the issue made (`filed`
 *  absent for one Dispatch had no answer naming, and carrying the write onto it that Dispatch
 *  refused where there was one), or the create itself refused, in the person's words. Nothing
 *  pressed is no state at all (`useWordsState`). */
type OfferState =
  | { readonly kind: "making"; readonly words: string }
  | { readonly filed: FiledIssue | undefined; readonly kind: "made"; readonly words: string }
  | { readonly kind: "refused"; readonly refusal: string; readonly words: string };

/** How the offer writes; the tests pass their own. */
export type FileNewIssue = (
  writes: Writes,
  write: NewIssueWrite
) => Promise<FiledIssue | undefined>;

/**
 * The bar's offer of the words a person typed as a new issue, for words its search matched
 * nothing with. Offering is not making: nothing is written until the person presses the row.
 *
 * The press writes the issue (`fileNewIssue`) and then: with a session configured and every
 * write landed, opens the issue, since a conversation is running on it; with no session named,
 * says so and leaves the row as the way to open it, because an issue nobody was asked about is
 * one the person decides what to do with; with a write onto the issue refused, says which write
 * it was and leaves the row as the way to open the issue, which is made either way.
 */
export function useNewIssueOffer({
  file = fileNewIssue,
  offered,
  onOpen,
  project,
  words,
}: {
  /** Write seam for tests; defaults to the real sequence. */
  readonly file?: FileNewIssue;
  /** Whether the words matched nothing, so there is an offer to make at all. */
  readonly offered: boolean;
  /** Opens the issue the press made. */
  readonly onOpen: (key: string) => void;
  /** The project of the page the bar was opened on. */
  readonly project: string | undefined;
  /** The words in the box, as the person typed them. */
  readonly words: string;
}): Offer | undefined {
  const queryClient = useQueryClient();
  const writes = useWrites();
  // The two settings decide what the press writes, so the row waits for them: a press made
  // without them would file an issue carrying neither the label nor the session the deployment
  // asked for. A read that failed leaves the bar offering nothing, as it does with no project.
  const settings = useQuery({
    ...newIssueSettingsQuery(),
    enabled: offered && project !== undefined,
  });
  const [here, setState] = useWordsState<OfferState>(words);
  if (!offered || project === undefined || settings.data === undefined) {
    return undefined;
  }
  const filed = here?.kind === "made" ? here.filed : undefined;
  const accept = () => {
    const settled = settings.data;
    setState({ kind: "making", words });
    file(writes, { project, settings: settled, words }).then(
      (made) => {
        void queryClient.invalidateQueries({ queryKey: ["issues"] });
        setState({ filed: made, kind: "made", words });
        // Opening the issue closes the bar, so it waits on a filing nothing was refused in: a
        // refusal is read here, against the issue it is about.
        if (made !== undefined && made.session !== "" && made.refused === undefined) {
          onOpen(made.issue.key);
        }
      },
      (error: unknown) => {
        setState({
          kind: "refused",
          refusal: refusalText(error, "the write did not reach Dispatch."),
          words,
        });
      }
    );
  };
  // Pressing the row once the issue is made opens it, which is the one thing left to do with
  // it: the create landed, so nothing here creates again.
  const openRow: OfferRow | undefined =
    filed === undefined
      ? undefined
      : {
          id: NEW_ISSUE_OPTION_ID,
          kind: "offer",
          label: `Open ${filed.issue.key}`,
          select: () => onOpen(filed.issue.key),
        };
  return {
    // The row is listed from the two states a press can still create from - nothing pressed,
    // and a create Dispatch refused - and as `Open …` once an issue is made. While the writes
    // are out, and for a create Dispatch landed with no answer naming, there is no row: a press
    // there would make a second issue.
    row:
      here === undefined || here.kind === "refused"
        ? {
            id: NEW_ISSUE_OPTION_ID,
            kind: "offer",
            label: `Make a new issue from “${words}”`,
            select: accept,
          }
        : openRow,
    said: <NewIssueSaid state={here} />,
  };
}

/** The one line the bar says about the issue it is making, made, or could not make. An issue
 *  it made that a later write was refused on says which write that was, against the key of the
 *  issue it has: the create landed, so there is an issue whatever the rest of the filing did. */
function NewIssueSaid({ state }: { readonly state: OfferState | undefined }): ReactNode {
  if (state === undefined) {
    return null;
  }
  if (state.kind === "making") {
    return (
      <p className={barNoteClassName} role="status">
        Making the issue…
      </p>
    );
  }
  if (state.kind === "refused") {
    return (
      <p className={barNoteClassName} data-testid="search-new-issue-said" role="alert">
        Could not make the issue: {state.refusal}
      </p>
    );
  }
  const filed = state.filed;
  if (filed === undefined) {
    return (
      <p className={barNoteClassName} data-testid="search-new-issue-said" role="status">
        The issue is made, and Dispatch had no answer naming it.
      </p>
    );
  }
  if (filed.refused !== undefined) {
    return (
      <p className={barNoteClassName} data-testid="search-new-issue-said" role="alert">
        {`${filed.issue.key} is made; ${filed.refused.what} was refused: ${filed.refused.why}`}
      </p>
    );
  }
  return (
    <p className={barNoteClassName} data-testid="search-new-issue-said" role="status">
      {filed.session === ""
        ? `${filed.issue.key} is made. No conversation was started: this Dispatch names no session for a new issue.`
        : `${filed.issue.key} is made, and its first message went to the session ${filed.session}.`}
    </p>
  );
}
