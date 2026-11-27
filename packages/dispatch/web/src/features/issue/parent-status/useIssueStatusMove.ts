import type { IssueDetails } from "../../../api/types";
import {
  isParentStatusConflict,
  issueHasParentOrOpenChild,
  type ParentStatusQuestion,
  type StatusMoveEnd,
  useParentStatusMove,
} from "./useParentStatusMove";

/** What an issue header's status writes take from the rule's question (R24, D13). */
export interface IssueStatusMove {
  /** Makes the header's write of `status` (`patch`, the issue's `PATCH`) through the question:
   *  for an issue the page knows has a parent or an open child, the dry run first, then `patch`,
   *  or the question where Dispatch's rule would refuse it. A pick the rule asks nothing of is
   *  saved even if the header has gone by the time the dry run answers; a question nobody is
   *  left to answer writes nothing. A `patch` that can find the write it asks for not sent - one
   *  that waits behind a write of the issue's fields that Dispatch then refuses
   *  (`requestStatusSubmit`) - answers whether it was sent. */
  // biome-ignore lint/suspicious/noConfusingVoidType: a `patch` that says nothing of whether its write went - the new issue page's, whose header takes no `unwritten`, and its pick, whose write goes in the issue's line at once - hands any callback, as a void callback takes one
  readonly move: (status: string, patch: () => Promise<boolean> | void) => void;
  /** The `onError` of each header write of `key` that can carry a status: where the write set the
   *  status and Dispatch refused it under the rule - the page read the parent or the children
   *  before they changed - it asks the question, as a refused dry run does. Each such write
   *  carries one field, so a write that sets the status sets nothing else. Any other refusal of a
   *  status stays the header's to show, and ends that write with nothing written. */
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
  /** Why the dry run of the newest status write was refused, which put the status back, or why
   *  Dispatch refused a chosen write its question was closed on past that write's deadline
   *  (`left`), until the next status write: `IssueStatusNotes` says it. */
  readonly refusal: unknown;
}

/**
 * The rule's question as an issue header makes its status writes through it: both designs of the
 * issue page's header call this and draw `IssueStatusNotes`, so a fix to the question reaches
 * both. Every status write of the header - its select or pill, Close, Reopen and the palette's
 * rows - goes through `move`, and each write that can carry a status hands its refusal to
 * `onError`. `unwritten`, where a header gives it, is called once for each status write of the
 * header that ends with nothing written: its dry run, or its `PATCH`, refused for another reason
 * than the rule; its `PATCH` never sent (`patch` answering `false`); its question closed
 * unanswered (`kept`), or closed behind a chosen write that Dispatch then refused (`left`); or a
 * move made while another asked (`busy`). A write that ends written - by the `PATCH`, or by a
 * choice the question took - changes the issue, which the header reads.
 */
export function useIssueStatusMove(
  issue: Pick<IssueDetails, "children" | "key" | "parent">,
  unwritten?: () => void
): IssueStatusMove {
  const parentStatus = useParentStatusMove();
  /** Tells `unwritten` of a move or a question that ended with nothing written. A move whose
   *  `PATCH` went (`patched`) ends through `onError` where Dispatch refuses it. */
  const ended = (end: StatusMoveEnd) => {
    if (end.kind === "left") {
      void end.written.catch(() => unwritten?.());
    } else if (end.kind !== "patched" && end.kind !== "changed") {
      unwritten?.();
    }
  };
  return {
    move: (status, patch) => {
      // Whether `patch` answered that its `PATCH` never went.
      let unsent = false;
      void parentStatus
        .move({
          key: issue.key,
          patch: async () => {
            unsent = (await patch()) === false;
          },
          related: issueHasParentOrOpenChild(issue),
          status,
        })
        .then((end) => {
          if (end.kind === "patched" && unsent) unwritten?.();
          else ended(end);
        });
    },
    onError: (error, key, input) => {
      if (input.status === undefined) return;
      if (isParentStatusConflict(error)) {
        void parentStatus.ask({ key, status: input.status }, error).then(ended);
      } else {
        unwritten?.();
      }
    },
    pending: parentStatus.pending?.status,
    question: parentStatus.question,
    refusal: parentStatus.refusal,
  };
}
