import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Fragment, type ReactNode, useEffect, useState, useSyncExternalStore } from "react";
import { Link } from "react-router-dom";

import { apiErrorMessage, goingRunUndoRequest } from "../../api/client";
import { goingConversationsQuery, goingPlanQuery } from "../../api/queries";
import type { GoingUndoAnswer } from "../../api/types";
import {
  borderDefault,
  card,
  dangerText,
  focusVisibleRing,
  linkHoverText,
  linkText,
  surfaceMutedHoverBg,
  textPrimaryOnSurface,
} from "../../theme/classes";
import { issueSummaryQuery } from "../refs/reference-target";
import { useKeyedWrite } from "../writes/useKeyedWrite";
import { planTarget } from "./usePlan";

/**
 * The notice at the bottom right of the page (design v3, states 11 and 13): one short line about
 * something the bar did while the person was not looking at it, such as "Filed DEMO-46: A weather
 * station on the roof · Open · Undo", for a few seconds. One notice shows at a time; a newer one
 * takes the place of the one on the screen. It lives in the shell's status corner at the bottom
 * right, so it outlives the bar.
 */

/** One action of a notice: a page it opens (`to`), or a press of its own. Either one lets the
 *  notice go. */
export type NoticeAction =
  | { readonly label: string; readonly to: string }
  | { readonly label: string; readonly press: () => void };

/** What a notice says and offers, and how long it shows. */
export interface NoticeInput {
  /** The line, in the person's words. */
  readonly text: string;
  /** An issue the line names by its key, whose title the notice says after the line once
   *  Dispatch answers the issue's summary: "Filed DEMO-46: A weather station on the roof". Until
   *  then, and for an issue the reader cannot read, the line stands alone. */
  readonly issue?: string;
  readonly actions?: readonly NoticeAction[];
  /** A run of the bar whose changes the notice's Undo reverses (R6,
   *  `POST /api/v1/going/runs/{id}/undo`), and what the notice says once it has. */
  readonly undo?: { readonly run: string; readonly undone: string };
  /** How long the notice shows, in milliseconds. */
  readonly ms: number;
}

/** The notice on the screen: what it was shown with, and which showing it is. */
interface ShownNotice extends NoticeInput {
  readonly id: number;
}

let shown: ShownNotice | undefined;
let shownCount = 0;
const listeners = new Set<() => void>();

function emit(): void {
  for (const listener of listeners) {
    listener();
  }
}

function subscribe(listener: () => void): () => void {
  listeners.add(listener);
  return () => listeners.delete(listener);
}

function readShown(): ShownNotice | undefined {
  return shown;
}

/** Shows `input` at the bottom right of the page, in place of any notice already there. */
export function showNotice(input: NoticeInput): void {
  shownCount += 1;
  shown = { ...input, id: shownCount };
  emit();
}

/** Lets the notice go: the one showing `id` where one is given, else whichever shows. */
export function dropNotice(id?: number): void {
  if (shown !== undefined && (id === undefined || shown.id === id)) {
    shown = undefined;
    emit();
  }
}

/** The notice on the screen now, read by a component and drawn again on each change. */
export function useNotice(): ShownNotice | undefined {
  return useSyncExternalStore(subscribe, readShown, readShown);
}

/** A notice's action (review a13): a small text button, as the bar's own buttons are, in the
 *  link's blue, filled on hover and ringed on focus - never an underlined link inside a line. */
const actionClassName = `-my-0.5 rounded-md px-1.5 py-0.5 text-xs font-medium outline-none focus-visible:ring-2 ${linkText} ${linkHoverText} ${surfaceMutedHoverBg} ${focusVisibleRing}`;

/** The separator between the notice's parts, which a screen reader does not read out. */
const dot = <span aria-hidden="true"> · </span>;

/** Undo in a notice: the run's own undo, through the one keyed path every write of the page
 *  takes. Once it lands the notice says what was undone; a change that stayed says why. */
function NoticeUndo({
  ms,
  onRefused,
  undo,
}: {
  readonly ms: number;
  readonly onRefused: (refusal: string) => void;
  readonly undo: NonNullable<NoticeInput["undo"]>;
}): ReactNode {
  const queryClient = useQueryClient();
  const write = useKeyedWrite<GoingUndoAnswer, string>({
    inline: true,
    label: () => "Undo the command bar's run",
    onError: (error) => onRefused(apiErrorMessage(error, "The undo did not reach Dispatch.")),
    onSuccess: (answered) => {
      void queryClient.invalidateQueries({ queryKey: goingPlanQuery(undo.run).queryKey });
      void queryClient.invalidateQueries({ queryKey: goingConversationsQuery().queryKey });
      const kept = (answered?.lines ?? []).find((line) => !line.outcome.ok);
      showNotice({
        ms,
        text: kept === undefined ? undo.undone : `Not undone: ${kept.outcome.message}`,
      });
    },
    request: (run) => goingRunUndoRequest(run),
    target: planTarget,
  });
  return (
    <button
      className={actionClassName}
      data-testid="going-notice-undo"
      disabled={write.isPending}
      onClick={() => write.mutate(undo.run)}
      type="button"
    >
      Undo
    </button>
  );
}

/** One showing of a notice, with the title of the issue it names, its actions, its Undo and the
 *  refusal of that Undo. */
function NoticeCard({ notice }: { readonly notice: ShownNotice }): ReactNode {
  const [refusal, setRefusal] = useState<string>();
  const title = useQuery({
    ...issueSummaryQuery(notice.issue),
    enabled: notice.issue !== undefined,
  }).data?.title;
  return (
    <div
      className={`flex flex-col gap-1 rounded-lg border px-3 py-2 text-sm shadow-lg ${card} ${borderDefault} ${textPrimaryOnSurface}`}
      data-testid="going-notice"
      role="status"
    >
      <p>
        <span data-testid="going-notice-text">
          {notice.text}
          {title === undefined || title === "" ? null : `: ${title}`}
        </span>
        {(notice.actions ?? []).map((action) => (
          <Fragment key={action.label}>
            {dot}
            {"to" in action ? (
              <Link
                className={actionClassName}
                onClick={() => dropNotice(notice.id)}
                to={action.to}
              >
                {action.label}
              </Link>
            ) : (
              <button
                className={actionClassName}
                onClick={() => {
                  dropNotice(notice.id);
                  action.press();
                }}
                type="button"
              >
                {action.label}
              </button>
            )}
          </Fragment>
        ))}
        {notice.undo === undefined ? null : (
          <>
            {dot}
            <NoticeUndo ms={notice.ms} onRefused={setRefusal} undo={notice.undo} />
          </>
        )}
      </p>
      {refusal === undefined ? null : (
        <p className={`text-xs ${dangerText}`} role="alert">
          {refusal}
        </p>
      )}
    </div>
  );
}

/**
 * The notice on the screen, if any, at the bottom right of the page, where the shell puts it.
 * It goes once its time is up, once one of its actions is pressed, or once a newer notice takes
 * its place.
 */
export function Notice(): ReactNode {
  const notice = useNotice();
  useEffect(() => {
    if (notice === undefined) {
      return;
    }
    const timer = window.setTimeout(() => dropNotice(notice.id), notice.ms);
    return () => window.clearTimeout(timer);
  }, [notice]);
  return notice === undefined ? null : <NoticeCard key={notice.id} notice={notice} />;
}
