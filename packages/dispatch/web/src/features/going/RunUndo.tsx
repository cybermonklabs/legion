import { useQueryClient } from "@tanstack/react-query";
import { type ReactNode, useState } from "react";

import { apiErrorMessage, goingRunUndoRequest } from "../../api/client";
import { goingConversationsQuery, goingPlanQuery } from "../../api/queries";
import type { GoingUndoAnswer, GoingUndoLine } from "../../api/types";
import {
  dangerText,
  secondaryButtonCompact,
  textMutedOnSurface,
  textPrimaryOnSurface,
} from "../../theme/classes";
import { useKeyedWrite } from "../writes/useKeyedWrite";
import { actionLabel } from "./action-names";
import { planTarget } from "./usePlan";

/** What became of one change an Undo reversed, in the person's words: undone; a message, which
 *  was sent and is not undone; a value another writer changed after the run, which Undo kept and
 *  names; or the refusal that stopped it. */
function lineText(line: GoingUndoLine): string {
  const what = actionLabel(line.tool, line.item);
  if (line.undo === "not_undone") {
    return `${what}: sent, not undone`;
  }
  const kept = (line.fields ?? []).filter((field) => !field.ok);
  if (kept.length > 0) {
    return `${what}: ${kept.map((field) => `${field.field} kept, ${field.message}`).join("; ")}`;
  }
  return line.outcome.ok ? `${what}: undone` : `${what}: ${line.outcome.message}`;
}

/**
 * Undo of one run (`POST /api/v1/going/runs/{id}/undo`), offered on the result of a run that
 * changed something while its conversation is in History: it writes each old value back, newest
 * first, closes each issue the run filed, rejects each of its suggestions still open, and names a
 * message sent, not undone. The answer's lines say what became of each change.
 */
export function RunUndo({ run }: { readonly run: string }): ReactNode {
  const queryClient = useQueryClient();
  const [lines, setLines] = useState<readonly GoingUndoLine[]>();
  const [refusal, setRefusal] = useState<string>();
  const undo = useKeyedWrite<GoingUndoAnswer, string>({
    inline: true,
    label: () => "Undo the command bar's run",
    onError: (error) => setRefusal(apiErrorMessage(error, "The undo did not reach Dispatch.")),
    onSuccess: (answered) => {
      setLines(answered?.lines ?? []);
      void queryClient.invalidateQueries({ queryKey: goingPlanQuery(run).queryKey });
      void queryClient.invalidateQueries({ queryKey: goingConversationsQuery().queryKey });
    },
    request: (id) => goingRunUndoRequest(id),
    target: planTarget,
  });
  return (
    <div className="space-y-1" data-run={run} data-testid="going-undo">
      {lines === undefined ? (
        <button
          className={secondaryButtonCompact}
          data-testid="going-undo-button"
          disabled={undo.isPending}
          onClick={() => {
            setRefusal(undefined);
            undo.mutate(run);
          }}
          type="button"
        >
          Undo
        </button>
      ) : (
        <ul className={`text-xs ${textPrimaryOnSurface}`} data-testid="going-undo-lines">
          {lines.length === 0 ? (
            <li className={textMutedOnSurface}>Nothing of this run was left to undo.</li>
          ) : (
            lines.map((line, index) => (
              // One row can have two lines - a reason sent, not undone, and its close written
              // back - and the lines are one answer's, drawn once in Dispatch's order, so a line
              // is its place in that answer.
              // biome-ignore lint/suspicious/noArrayIndexKey: a line's place in the answer never changes
              <li data-testid="going-undo-line" key={index}>
                {lineText(line)}
              </li>
            ))
          )}
        </ul>
      )}
      {refusal === undefined ? null : (
        <p className={`text-xs ${dangerText}`} role="alert">
          {refusal}
        </p>
      )}
    </div>
  );
}
