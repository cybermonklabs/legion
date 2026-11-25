import type { IssueDetails } from "../../../api/types";
import {
  isParentStatusConflict,
  issueHasParentOrOpenChild,
  type ParentStatusQuestion,
  useParentStatusMove,
} from "./useParentStatusMove";

/** What an issue header's status writes take from the rule's question (R24, D13). */
export interface IssueStatusMove {
  /** Makes the header's write of `status` (`patch`, the issue's `PATCH`) through the question:
   *  for an issue the page knows has a parent or an open child, the dry run first, then `patch`,
   *  or the question where Dispatch's rule would refuse it. A pick the rule asks nothing of is
   *  saved even if the header has gone by the time the dry run answers; a question nobody is
   *  left to answer writes nothing. */
  readonly move: (status: string, patch: () => unknown) => void;
  /** The `onError` of each header write of `key` that can carry a status: where the write set the
   *  status and Dispatch refused it under the rule - the page read the parent or the children
   *  before they changed - it asks the question, as a refused dry run does. Each such write
   *  carries one field, so a write that sets the status sets nothing else. Any other refusal
   *  stays the header's to show. */
  readonly onError: (
    error: unknown,
    key: string,
    input: { readonly status?: string | undefined }
  ) => void;
  /** The status the question is checking, asking about or writing: the header shows it as the
   *  issue's pending one. */
  readonly pending: string | undefined;
  /** The question while it is open, which `IssueStatusNotes` draws. */
  readonly question: ParentStatusQuestion | undefined;
  /** Why the dry run of the newest status write was refused, which put the status back, until
   *  the next one: `IssueStatusNotes` says it. */
  readonly refusal: unknown;
}

/**
 * The rule's question as an issue header makes its status writes through it: both designs of the
 * issue page's header call this and draw `IssueStatusNotes`, so a fix to the question reaches
 * both. Every status write of the header - its select or pill, Close, Reopen and the palette's
 * rows - goes through `move`, and each write that can carry a status hands its refusal to
 * `onError`.
 */
export function useIssueStatusMove(
  issue: Pick<IssueDetails, "children" | "key" | "parent">
): IssueStatusMove {
  const parentStatus = useParentStatusMove();
  return {
    move: (status, patch) => {
      void parentStatus.move({
        key: issue.key,
        patch,
        related: issueHasParentOrOpenChild(issue),
        status,
      });
    },
    onError: (error, key, input) => {
      if (input.status !== undefined && isParentStatusConflict(error)) {
        void parentStatus.ask({ key, status: input.status }, error);
      }
    },
    pending: parentStatus.pending?.status,
    question: parentStatus.question,
    refusal: parentStatus.refusal,
  };
}
