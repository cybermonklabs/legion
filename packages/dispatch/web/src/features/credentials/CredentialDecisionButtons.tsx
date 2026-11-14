import type { UseMutationResult } from "@tanstack/react-query";
import type { ReactNode } from "react";

import { apiErrorMessage } from "../../api/client";
import type { DecisionResponse, StateResponse } from "../../api/types";
import type { SubmitGuard } from "../../hooks/useSubmitGuard";
import {
  dangerText,
  primaryButtonBg,
  primaryButtonDisabled,
  primaryButtonEnabledHoverBg,
  secondaryButtonBorder,
  secondaryButtonHoverBorder,
  secondaryButtonText,
} from "../../theme/classes";
import { SendStoppedError, stoppedWriteNote } from "../writes/keyed-write";

/**
 * The Approve/Deny button pair `MachineLoginPage` renders for a pending machine login: that page
 * builds its own `approve`/`deny` mutations, whose POST bodies carry the code the operator typed,
 * then hands them here for the shared disabled-guard, click-wiring, and verbatim broker-error
 * rendering. With no `approve`, only Deny shows: a record nobody may approve any more, such as a
 * service's machine login from before services logged in with their tokens.
 */
export function CredentialDecisionButtons({
  approve,
  deny,
  submitGuard,
}: {
  approve?: UseMutationResult<DecisionResponse | null, Error, void>;
  deny: UseMutationResult<StateResponse | null, Error, void>;
  submitGuard: SubmitGuard;
}): ReactNode {
  const deciding = approve?.isPending === true || deny.isPending;
  return (
    <div className="space-y-2">
      <div className="flex gap-2">
        {approve === undefined ? null : (
          <button
            className={`min-h-11 rounded-lg px-4 py-2 text-sm font-medium ${primaryButtonBg} ${primaryButtonEnabledHoverBg} ${primaryButtonDisabled}`}
            disabled={deciding}
            onClick={() => submitGuard.guard(() => approve.mutate())}
            type="button"
          >
            Approve
          </button>
        )}
        <button
          className={`min-h-11 rounded-lg border px-4 py-2 text-sm font-medium ${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonHoverBorder}`}
          disabled={deciding}
          onClick={() => submitGuard.guard(() => deny.mutate())}
          type="button"
        >
          Deny
        </button>
      </div>
      {/* A decision the reader stopped may have been made, and each button is a press of its own,
          under a key of its own: none makes the stopped decision again. */}
      {approve?.isError ? (
        <p className={dangerText}>
          {approve.error instanceof SendStoppedError
            ? stoppedWriteNote("the approval", "made", {
                anew: "Pressing Approve again sends a new approval.",
              })
            : apiErrorMessage(approve.error, "Could not approve this request.")}
        </p>
      ) : null}
      {deny.isError ? (
        <p className={dangerText}>
          {deny.error instanceof SendStoppedError
            ? stoppedWriteNote("the denial", "made", {
                anew: "Pressing Deny again sends a new denial.",
              })
            : apiErrorMessage(deny.error, "Could not deny this request.")}
        </p>
      ) : null}
    </div>
  );
}
