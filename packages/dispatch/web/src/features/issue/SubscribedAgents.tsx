import { type ReactNode, useState } from "react";

import type { Subscriber } from "../../api/types";
import { useTooltips } from "../../components/tooltips";
import { textSecondaryOnSurface } from "../../theme/classes";
import { AgentName, useAgentText } from "../refs/AgentName";
import { sessionChipClassName, sessionRemoveButtonClassName } from "../refs/SessionChip";
import { Timestamp } from "../refs/Timestamp";
import { UnsubscribeDialog } from "./UnsubscribeDialog";

export interface SubscribedAgentsProps {
  onUnsubscribe: (sessionId: string) => void;
  /** The issue key, or `<project>/<slug>` for a project document — named in the confirm
   * dialog's message and nowhere else, so callers pass a precomputed string rather than an
   * issue/document union this component would have to branch on. */
  ownerLabel: string;
  subscribers: readonly Subscriber[];
}

/**
 * The "Subscribed agents" header section shared by an issue's header and a project
 * document's page: sessions whose persisted Envoy interests include this owner's topic
 * family, each drawn by `AgentName` (its name, its state from the agent registry - running,
 * unresponsive or not running - and a link to its live conversation), how long ago it was last
 * seen, and an Unsubscribe control that confirms before telling the session it was removed.
 * That state is the registry's alone: the subscribers endpoint's `live` (`Subscriber.live`) is
 * a second snapshot taken at another time, and it knows only two states where the registry
 * knows three, so it is deliberately ignored and one agent never shows two states on one page.
 *
 * The session's own id is a tooltip on the surfaces that raise one, and nothing on a surface
 * that does not (`components/tooltips.tsx`), as every other shared control here reads it: the
 * issue page draws this section inside `NoTooltips`, because a reader on a phone or on the
 * keyboard never sees a tooltip (dispatch://INFRA-1196 R21).
 */
export function SubscribedAgents({
  onUnsubscribe,
  ownerLabel,
  subscribers,
}: SubscribedAgentsProps): ReactNode {
  const [confirming, setConfirming] = useState<Subscriber | undefined>(undefined);
  const text = useAgentText();
  const tooltips = useTooltips();
  const confirmingLabel =
    confirming === undefined ? "" : text.sessionText(confirming.session_id, confirming.title);

  if (subscribers.length === 0) {
    return null;
  }

  return (
    <section aria-label="Subscribed agents" className="mt-2">
      <h2 className={`text-sm font-semibold ${textSecondaryOnSurface}`}>Subscribed agents</h2>
      <ul className="mt-2 flex flex-wrap gap-2">
        {subscribers.map((subscriber) => (
          <li className={sessionChipClassName} key={subscriber.session_id}>
            <span title={tooltips ? subscriber.session_id : undefined}>
              <AgentName sessionId={subscriber.session_id} storedTitle={subscriber.title} />
            </span>
            <Timestamp at={new Date(subscriber.last_seen).toISOString()} />
            {/* Why Unsubscribe is refused, drawn rather than carried as the control's `title`:
                a disabled control takes no focus and raises no tooltip on a phone, so the
                reason reached nobody who could not already press it (R21, the panel draws this
                section inside `NoTooltips`). */}
            {subscriber.removable ? null : (
              <span className="text-xs italic">
                via {subscriber.via}, which covers other issues and documents too
              </span>
            )}
            <button
              className={sessionRemoveButtonClassName}
              disabled={!subscriber.removable}
              onClick={() => setConfirming(subscriber)}
              type="button"
            >
              Unsubscribe
            </button>
          </li>
        ))}
      </ul>
      {confirming === undefined ? null : (
        <UnsubscribeDialog
          message={`Unsubscribe ${confirmingLabel} from ${ownerLabel}? They will be told.`}
          onCancel={() => setConfirming(undefined)}
          onConfirm={() => {
            onUnsubscribe(confirming.session_id);
            setConfirming(undefined);
          }}
        />
      )}
    </section>
  );
}
