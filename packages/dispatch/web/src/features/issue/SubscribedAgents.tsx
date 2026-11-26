import { type ReactNode, useState } from "react";

import type { Subscriber } from "../../api/types";
import { usePageWords, useTooltips } from "../../components/tooltips";
import { pageLabelChip } from "../../theme/classes";
import { pageMutedText, textSecondaryOnSurface } from "../../theme/shell-classes";
import { AgentName, useAgentText } from "../refs/AgentName";
import { sessionChipClassName, sessionRemoveButtonClassName } from "../refs/SessionChip";
import { Timestamp } from "../refs/Timestamp";
import { UnsubscribeDialog } from "./UnsubscribeDialog";

/** A session's chip and its remove control inside `NoTooltips`, in the new issue page's colours,
 *  on `SessionChip`'s shapes: the page's label chip, and muted words that turn the failed red
 *  under the pointer, as `main`'s turn the danger red. */
const PAGE_SESSION_CHIP = `flex items-center gap-2 rounded-full px-3 py-1 text-sm ${pageLabelChip}`;
const PAGE_SESSION_REMOVE = `font-medium ${pageMutedText} hover:text-page-failed disabled:cursor-not-allowed disabled:opacity-50`;

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
 * Inside `NoTooltips` (`components/tooltips.tsx`, the new issue page) the session's own id is no
 * tooltip, and a subscription wider than this owner says so in the page's words in place of
 * "via" and its topic, because a disabled Unsubscribe raises no tooltip that could say why.
 */
export function SubscribedAgents({
  onUnsubscribe,
  ownerLabel,
  subscribers,
}: SubscribedAgentsProps): ReactNode {
  const [confirming, setConfirming] = useState<Subscriber | undefined>(undefined);
  const text = useAgentText();
  const tooltips = useTooltips();
  const page = usePageWords();
  // The question names the session as its chip above draws it on this surface.
  const confirmingLabel =
    confirming === undefined ? "" : text.sessionShown(confirming.session_id, confirming.title);

  if (subscribers.length === 0) {
    return null;
  }

  return (
    <section aria-label="Subscribed agents" className="mt-2">
      <h2
        className={`text-sm font-semibold ${page === null ? textSecondaryOnSurface : pageMutedText}`}
      >
        Subscribed agents
      </h2>
      <ul className="mt-2 flex flex-wrap gap-2">
        {subscribers.map((subscriber) => (
          <li
            className={page === null ? sessionChipClassName : PAGE_SESSION_CHIP}
            key={subscriber.session_id}
          >
            <span title={tooltips ? subscriber.session_id : undefined}>
              <AgentName sessionId={subscriber.session_id} storedTitle={subscriber.title} />
            </span>
            <Timestamp at={new Date(subscriber.last_seen).toISOString()} />
            {subscriber.removable ? null : page === null ? (
              <span className="text-xs italic">via {subscriber.via}</span>
            ) : (
              <span className="text-xs italic" {...page.pageChrome}>
                {page.STATE_LINES.subscription_wider}
              </span>
            )}
            <button
              className={page === null ? sessionRemoveButtonClassName : PAGE_SESSION_REMOVE}
              disabled={!subscriber.removable}
              onClick={() => setConfirming(subscriber)}
              title={
                subscriber.removable || !tooltips
                  ? undefined
                  : `Subscribed only via ${subscriber.via}, which also covers other issues and documents — cannot unsubscribe just this one`
              }
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
