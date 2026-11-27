import type { ReactNode } from "react";

import { apiErrorMessage } from "../../api/client";
import { RefusableButton } from "../../components/RefusableButton";
import {
  dangerText,
  surfaceMutedBg,
  textMutedOnCanvas,
  textMutedOnSurfaceMuted,
  textPrimaryOnCanvas,
} from "../../theme/classes";
import { MarkdownBody } from "../refs/MarkdownBody";
import { SendStoppedError } from "../writes/keyed-write";
import { StopTrying } from "../writes/StopTrying";
import { KEYED_SEND_LATE_TEXT } from "../writes/write-words";
import type { HeldSend } from "./held-sends";
import type { SessionSends } from "./useSessionSends";

/**
 * One message of the viewer's to a session that Dispatch has not yet made, as its own turn: the
 * text named as theirs - `Your message`, at a glance and to a screen reader, since it is not yet
 * the session's conversation - and where it stands: held while the viewer's sign-out in another
 * tab is out, saying so; waiting for the message before it, never sent, with Discard; sending;
 * still being tried past its deadline with Stop trying; or refused with why, Retry and Discard.
 * The session's live view and its Agents row both show it, and either's controls act on the
 * same write (`useSessionSends`). It leaves once Dispatch makes it, and the stored message takes
 * its place.
 *
 * The draft is drawn by the renderer, as the message it becomes will be: its line breaks and
 * formatting as the stored message shows them, and a mention the picker wrote as the party's
 * name. Dispatch has stored nothing yet, so the page parses it, as it parses every write the
 * server has not stored; it is no record yet, so a `#id` in it names nothing until it is one.
 */
export function PendingSendTurn({
  held,
  sends,
}: {
  held: HeldSend;
  sends: Pick<SessionSends, "discard" | "retry" | "stop">;
}): ReactNode {
  return (
    <div
      className="mt-4 flex flex-col items-end gap-1"
      data-message-id={`pending:${held.id}`}
      data-testid="agent-send-held"
    >
      <div
        className={`max-w-[80%] rounded-xl px-3 py-2 text-sm opacity-80 ${surfaceMutedBg} ${textPrimaryOnCanvas}`}
      >
        <p className={`text-xs font-medium ${textMutedOnSurfaceMuted}`}>Your message</p>
        <MarkdownBody markdown={held.draft.draft.body} surface="agent-held-send" />
      </div>
      <div className="flex max-w-[80%] flex-wrap items-center justify-end gap-x-3 gap-y-1">
        {held.status === "refused" ? (
          <>
            <p className={`text-sm ${dangerText}`} data-testid="agent-send-error">
              {held.refusal instanceof SendStoppedError
                ? held.refusal.message
                : `Could not send: ${apiErrorMessage(held.refusal, "could not reach the session")}`}
            </p>
            <RefusableButton
              look="secondary-compact"
              onPress={() => sends.retry(held)}
              refusal={null}
            >
              Retry
            </RefusableButton>
            <RefusableButton
              look="secondary-compact"
              onPress={() => sends.discard(held)}
              refusal={null}
            >
              Discard
            </RefusableButton>
          </>
        ) : held.signingOutElsewhere ? (
          <>
            <p className={`text-xs ${textMutedOnCanvas}`} role="status">
              Waiting for a sign-out in another tab
            </p>
            {held.status === "waiting" ? (
              <RefusableButton
                look="secondary-compact"
                onPress={() => sends.discard(held)}
                refusal={null}
              >
                Discard
              </RefusableButton>
            ) : null}
          </>
        ) : held.status === "waiting" ? (
          <>
            <p className={`text-xs ${textMutedOnCanvas}`} role="status">
              Waiting to send
            </p>
            <RefusableButton
              look="secondary-compact"
              onPress={() => sends.discard(held)}
              refusal={null}
            >
              Discard
            </RefusableButton>
          </>
        ) : (
          <>
            <p className={`text-xs ${textMutedOnCanvas}`} role="status">
              {held.status === "late" ? KEYED_SEND_LATE_TEXT : "Sending…"}
            </p>
            {held.status === "late" ? (
              <StopTrying onStop={() => sends.stop(held)} stopping={held.stopping} />
            ) : null}
          </>
        )}
      </div>
    </div>
  );
}
