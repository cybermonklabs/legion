import { useMutationState, useQueryClient } from "@tanstack/react-query";

import { api, apiErrorCodeIn, apiErrorMessage, issuePatchRequest } from "../../api/client";
import { landIssueWrite, mergeIssue } from "../../api/issue-cache";
import { userStateQuery } from "../../api/queries";
import type { Issue, IssueDetails, UserIssueState, UserState } from "../../api/types";
import { type SubmitGuard, useSubmitGuard } from "../../hooks/useSubmitGuard";
import { stateForIssue } from "../issue/pins";
import { type IssueDrafts, type IssueUpdateInput, useIssueDrafts } from "../issue/useIssueDrafts";
import { SendStoppedError, stoppedWriteNote } from "../writes/keyed-write";
import {
  type KeyedPress,
  type KeyedWriteOptions,
  type KeyedWriteResult,
  useKeyedWrite,
} from "../writes/useKeyedWrite";
import { PROPERTY_LABELS } from "./page-structure";
import { CONTROL_LABELS, issueStatusWord } from "./page-words";

/** A title Dispatch refuses the same way every time, so its refusal is shown with no Retry. */
const titleRefusals = new Set(["CAP_EXCEEDED", "ISSUE_CLOSED"]);

/** Whether `input` is the Status pill's pick alone: that control shows its write in the shell's
 *  indicator and rolls back on a refusal, where every other header field holds its draft and
 *  says why itself. */
export function statusPickOnly(input: IssueUpdateInput | undefined): boolean {
  return input !== undefined && Object.keys(input).length === 1 && input.status !== undefined;
}

/** What the shell's indicator calls each field the identity block writes: the page's own name
 *  for the property, never the schema's, so a write reads "Parent of CORE-1 saved" and not
 *  "CORE-1: parent saved". */
const WRITTEN_FIELD_NAMES: Readonly<Record<keyof IssueUpdateInput, string>> = {
  parent: PROPERTY_LABELS.parent,
  route: CONTROL_LABELS.route,
  status: PROPERTY_LABELS.status,
  title: PROPERTY_LABELS.title,
};

/**
 * What the shell's indicator names one header write of `key`: the page's own name for every
 * field the write carries, and the page's own word for a status it sets (`issueStatusWord`,
 * "Todo"), never the schema's token for either. A write of two fields names both, because the
 * first key of the input is no more the write than the second.
 *
 * The shell's indicator (`features/writes/PendingWritesIndicator.tsx`) only draws the line it
 * is handed; this hook, the issue page's own, composes it. A word is the page's that composes
 * it, whichever element draws it, so the line says what this page says
 * (`e2e/keyed-writes.e2e.ts` reads it).
 */
export function issueUpdateLabel(key: string, input: IssueUpdateInput): string {
  if (input.status !== undefined) return `${key} set to ${issueStatusWord(input.status)}`;
  const fields = (Object.keys(input) as (keyof IssueUpdateInput)[]).map(
    (field) => WRITTEN_FIELD_NAMES[field]
  );
  return fields.length === 0 ? `${key} saved` : `${fields.join(" and ")} of ${key} saved`;
}

/**
 * Every write the identity block makes, in one place: the issue's own fields (the title, the
 * status, the route, the parent), its labels, its pin and its claim's release, with the drafts
 * those fields hold, and the few facts each row reads back from a write in flight - which status
 * a pick is saving, a parent or a title Dispatch refused.
 *
 * Any later write of the issue's fields from the page (a later part's editor of the issues this
 * one waits on, for example) takes the same landing as the fields here (`landIssueWrite`), so
 * what a write leaves in the cache does not depend on which row drew the control.
 *
 * The rows of the identity block that draw these fields hold no write of their own, so moving
 * one of them from row to row changes where its control is drawn and nothing about how it is
 * saved. Each write is one write of the page's one store (`features/writes/`), so every one of
 * them is keyed, shown in the shell's indicator and kept across a reload.
 */
export interface IssueHeaderWrites {
  /** The drafts of the title, the route and the parent, and the submits that save them. */
  readonly drafts: IssueDrafts;
  /** The title, the route and the parent, which the drafts save, and a status sent through the
   *  drafts (`requestStatusSubmit`); the Status pill's pick is a write of its own (`pickStatus`).
   *  Each press names its issue (`IssueFieldsSave`). */
  readonly updateIssue: KeyedWriteResult<Issue, IssueFieldsSave, unknown>;
  /** The labels, which the popover saves as one set when it closes. */
  readonly labels: KeyedWriteResult<Issue, IssueLabelsSave, unknown>;
  /** The pin, applied to the person's state at once and rolled back on a refusal. */
  readonly pin: KeyedWriteResult<UserIssueState, IssuePinSave, PinRollback>;
  /** One press at a time on the pin, whose write is optimistic. */
  readonly pinGuard: SubmitGuard;
  /** The release of the claim on the issue a press names, by its key. */
  readonly releaseClaim: KeyedWriteResult<Issue, string, unknown>;
  /** The status a pick is saving right now, which the pill and the closed line show: the newest
   *  pending write of this issue's fields that holds a status, whichever caller on the page made
   *  it, so the header and Details show one status. A pick waiting in the issue's line behind an
   *  earlier write is pending from the moment it is made (`pickStatus`), so it shows at once. */
  readonly pendingStatus: string | undefined;
  /** Whether a pick is waiting its turn or being saved: `pendingStatus` stands. */
  readonly statusSaving: boolean;
  /**
   * The Status pill's pick: a write of its own, made at once in the issue's line (`issue:<key>`).
   * The line holds it behind a title, route or parent write of the issue still being tried and
   * sends it once that one is answered or refused, so the pick never lands first. It is not one
   * more write through `updateIssue`, for two reasons. The reader may leave the page while the
   * earlier write is out: the write store lives on the query client, not on the page, so it
   * keeps the pick, shows it in the shell's indicator and sends it in its turn with no caller
   * drawn, and a reload keeps it, as it keeps every write. And a title Dispatch refused must stay
   * shown and retryable: `titleRefusal`, `parentError` and `drafts.retry()` read `updateIssue`'s
   * last write, which a status sent through it would replace.
   */
  readonly pickStatus: (next: string) => void;
  /** A parent Dispatch refused, with the reason it gave, or one the reader stopped, which may have
   *  been saved, drawn where the parent is edited. */
  readonly parentSaveFailed: boolean;
  readonly parentError: string;
  /** A title Dispatch refuses every time (too long, or the issue is closed), or null. */
  readonly titleRefusal: string | null;
}

/** One write of the issue's fields: the issue it is of and its change, both taken at the press,
 *  so its request, its line, its label and its landing are of that issue, and so is a Retry of
 *  it, whichever issue the hook is drawn for by then (`useKeyedWrite` runs the newest render's
 *  options over the press's own variables). */
export interface IssueFieldsSave {
  readonly input: IssueUpdateInput;
  readonly issueKey: string;
}

/** One save of an issue's labels: the issue and the whole set, taken at the press. */
export interface IssueLabelsSave {
  readonly issueKey: string;
  readonly labels: string[];
}

/** One pin or unpin of an issue, taken at the press. */
export interface IssuePinSave {
  readonly issueKey: string;
  readonly pinned: boolean;
}

/** What the pin's optimistic write puts back when Dispatch refuses it. */
interface PinRollback {
  readonly previous: UserState | undefined;
}

export function useIssueHeaderWrites(
  issue: IssueDetails,
  state: UserIssueState
): IssueHeaderWrites {
  const queryClient = useQueryClient();
  // Every write here states its method - `write` with its own, a `request` with the request's -
  // and takes the issue it is of from its press, never from the issue this hook is drawn for
  // when a callback or a Retry runs. `write` comes first: its answer and its variables are what
  // the callbacks below read.
  const releaseClaim = useKeyedWrite({
    // A release made again after one that landed needs no `removedEvenIfGone`: Dispatch answers a
    // release of an issue nobody holds with the issue, as it answers the first.
    method: "DELETE",
    write: (issueKey: string, bounds) => api.releaseIssueClaim(issueKey, bounds),
    inline: true,
    label: (issueKey) => `Claim on ${issueKey} released`,
    onSuccess: (next) => {
      if (next !== null) mergeIssue(queryClient, next);
      void queryClient.invalidateQueries({ queryKey: ["issues"] });
    },
  });
  // A title, route, parent or close the header shows refused itself, beside its draft; a status
  // alone is a control's, so its refusal is the shell's (`PendingWritesIndicator`), and it rolls
  // back. The Status pill's pick is a write of its own (`statusWrite`, below). Every field write
  // goes in the issue's line (`issue:<key>`), so an older one still being tried never lands after
  // a newer one, whatever left the page meanwhile. A landing with no answer to give is merged by
  // reading the issue again. Each carries the issue's mutation key, which every caller of this
  // hook on the page reads its pending status by (`pendingStatuses`, below).
  const mutationKey = ["issue-update", issue.key];
  // What the two writes of the issue's fields share, written once so the pick can never part
  // from `updateIssue`'s key, line, request or landing.
  const fieldWrite: KeyedWriteOptions<Issue, IssueFieldsSave, unknown> = {
    mutationKey,
    label: ({ input, issueKey }) => issueUpdateLabel(issueKey, input),
    target: ({ issueKey }) => `issue:${issueKey}`,
    request: ({ input, issueKey }) => issuePatchRequest(issueKey, input),
    onSuccess: (next, { input, issueKey }) => {
      landIssueWrite(queryClient, issueKey, next);
      drafts.onIssueSuccess(next ?? issue, input);
    },
  };
  const updateIssue = useKeyedWrite({
    ...fieldWrite,
    inline: ({ input }) => !statusPickOnly(input),
    onSettled: (_data, error) => {
      drafts.onIssueSettled(error);
    },
  });
  // The Status pill's pick (`IssueHeaderWrites.pickStatus` says why it is not `updateIssue`'s).
  // It is a control, so its refusal is the shell's and the pick rolls back (`statusPickOnly`). It
  // takes `updateIssue`'s line and mutation key, so it waits behind a title, route or parent write
  // still being tried, and `pendingStatus` reads it. It lets go of no guard and drains no draft
  // (`drafts.onIssueSettled`): the guard and the drafts are `updateIssue`'s, and a drain here,
  // after a title Dispatch refused, would send that title again.
  const statusWrite = useKeyedWrite({ ...fieldWrite, inline: false });
  // The labels are a field of the issue too, so they land as every field write does: a landing
  // with no answer reads the issue again rather than leave the old labels showing
  // (`landIssueWrite`).
  const labels = useKeyedWrite({
    label: ({ issueKey }: IssueLabelsSave) => `Labels of ${issueKey}`,
    target: ({ issueKey }) => `issue:${issueKey}`,
    request: ({ issueKey, labels: next }) => issuePatchRequest(issueKey, { labels: next }),
    onSuccess: (next, { issueKey }) => {
      landIssueWrite(queryClient, issueKey, next);
    },
  });
  // The drafts save the fields of the issue this hook is drawn for, so each of their presses
  // names that issue, and reads back the change its newest press carried.
  const drafts = useIssueDrafts(issue, {
    mutate: (input) => updateIssue.mutate({ input, issueKey: issue.key }),
    retry: updateIssue.retry,
    variables: updateIssue.variables?.input,
  });
  const pinGuard = useSubmitGuard();
  const pin = useKeyedWrite({
    label: ({ issueKey, pinned }: IssuePinSave) => `${issueKey} ${pinned ? "pinned" : "unpinned"}`,
    method: "PUT",
    write: ({ issueKey, pinned }, bounds) => api.putIssueState(issueKey, { pinned }, bounds),
    onSettled: () => {
      pinGuard.release();
      void queryClient.invalidateQueries({ queryKey: userStateQuery().queryKey });
      // The sidebar lists the pinned issues from their own read, which follows the pin however
      // its write settled: with an answer, landed with none (`null`), or refused.
      void queryClient.invalidateQueries({ exact: true, queryKey: ["issues", "pinned"] });
    },
    onMutate: async ({ issueKey, pinned }) => {
      await queryClient.cancelQueries({ queryKey: userStateQuery().queryKey });
      const previous = queryClient.getQueryData<UserState>(userStateQuery().queryKey);
      // The press's issue shows pinned at once: from the state this hook is handed where the
      // press is of its issue, else as the person's state read holds that issue.
      const shown = issueKey === issue.key ? state : stateForIssue(previous, issueKey);
      queryClient.setQueryData<UserState>(userStateQuery().queryKey, (current) => ({
        ...current,
        [issueKey]: { ...shown, pinned },
      }));
      return { previous };
    },
    onError: (_error, { issueKey }, context) => {
      queryClient.setQueryData<UserState>(userStateQuery().queryKey, (current) => ({
        ...current,
        [issueKey]: stateForIssue(context?.previous, issueKey),
      }));
    },
    onSuccess: (next, { issueKey }) => {
      if (next === null) return;
      queryClient.setQueryData<UserState>(userStateQuery().queryKey, (current) => ({
        ...current,
        [issueKey]: next,
      }));
    },
  });

  // Every pending write of this issue's fields on the page - `updateIssue`'s and `statusWrite`'s,
  // which carry one mutation key, of every caller of this hook - in the order they were made, so
  // the last that holds a status is the newest pick. A pick waiting its turn in the issue's line
  // is pending from its press, so it shows at once. The mutation cache holds each as the press
  // its keyed write made (`KeyedPress`), with the save inside it.
  const pendingStatuses = useMutationState({
    filters: { exact: true, mutationKey, status: "pending" },
    select: (mutation) => {
      const press = mutation.state.variables as KeyedPress<IssueFieldsSave> | undefined;
      return press?.variables.input.status;
    },
  });
  const pickStatus = (next: string) => {
    statusWrite.mutate({ input: { status: next }, issueKey: issue.key });
  };
  const pendingStatus = pendingStatuses.filter((status) => status !== undefined).at(-1);
  const statusSaving = pendingStatus !== undefined;
  return {
    drafts,
    labels,
    // A refused parent save (PARENT_INPUT, ISSUE_CLOSED) renders the server's reason inline,
    // like a route validation error, instead of the generic update failure line. One the reader
    // stopped may have been saved; the editor has closed, and saving it again is a press of its
    // own (`requestParentSubmit`).
    parentError:
      updateIssue.error instanceof SendStoppedError
        ? stoppedWriteNote("the new parent", "saved", {
            anew: "Saving it again sends it as a new write.",
          })
        : apiErrorMessage(updateIssue.error, "Could not save parent."),
    parentSaveFailed: updateIssue.isError && updateIssue.variables?.input.parent !== undefined,
    pendingStatus,
    pickStatus,
    pin,
    pinGuard,
    releaseClaim,
    statusSaving,
    // A title Dispatch refused (CAP_EXCEEDED past ISSUE_TITLE_MAX, ISSUE_CLOSED) shows its reason
    // with no Retry, since the same title is refused every time; any other failure keeps the
    // generic line and its Retry.
    titleRefusal:
      updateIssue.isError &&
      updateIssue.variables?.input.title !== undefined &&
      apiErrorCodeIn(updateIssue.error, titleRefusals)
        ? updateIssue.error.message
        : null,
    updateIssue,
  };
}
