import { useQuery } from "@tanstack/react-query";
import type { ReactNode } from "react";

import { issueQuery } from "../../api/queries";
import type { IssueDetails } from "../../api/types";
import { claimHoldingFromRead } from "../issue/ClaimChip";
import { useAgentText } from "../refs/AgentName";
import { type AgentRegistry, useAgentRegistry } from "../refs/agent-registry";
import { useWrites } from "../writes/write-store";
import { barNoteClassName, type Offer, refusalText, useWordsState } from "./offer";
import { type SendToAgent, sendToAgent } from "./send-to-agent";

/** The group the offer's one row sits in, and the row's own id, which the bar's arrows and
 *  `aria-activedescendant` read. */
export const SEND_TO_AGENT_GROUP_ID = "search-group-send-to-agent";
export const SEND_TO_AGENT_OPTION_ID = "search-option-send-to-agent";

/** Where the offer stands once a press has been made: the send out, the message sent, or a
 *  refusal in the person's words. Nothing pressed is no state at all (`useWordsState`). */
type OfferState =
  | { readonly kind: "sending"; readonly words: string }
  | { readonly agent: string; readonly kind: "sent"; readonly words: string }
  | { readonly kind: "refused"; readonly refusal: string; readonly words: string };

/**
 * The session a live agent holds this issue's claim with, or undefined where no such agent does:
 * no claim, a person's claim, a holder that has stopped or missed its heartbeats, or a registry
 * this browser could not read. It is the judgement the chip on the issue itself makes
 * (`claimHolding`), from the issue read's own verdict on the holder where it gave one and from
 * the live registry otherwise, so the bar and the chip never disagree about who is on the issue.
 */
function liveHolderOf(
  issue: IssueDetails | undefined,
  registry: Pick<AgentRegistry, "state" | "titles">
): string | undefined {
  if (issue === undefined) {
    return undefined;
  }
  const claim = issue.claim;
  if (claim === null || claim.actor.kind !== "session") {
    return undefined;
  }
  return claimHoldingFromRead(claim, registry, issue) === "holds" ? claim.actor.id : undefined;
}

/**
 * The bar's offer of the words a person typed as a message to the agent already on this issue,
 * one row among the actions the bar lists rather than a surface of its own.
 *
 * It always asks: the row is what sends, whatever any confidence in the words would be, because
 * a send spends that agent a turn (dispatch://INFRA-1042/ask/4310b2c0-df6f-424b-b0b6-b75e23a60fb6).
 * Nothing is sent until the person presses it, and one press sends one message.
 *
 * The issue and the block are the chip's rather than the page's (`barTargets` in
 * `bar-scope.ts`): the row is listed only while the chip names an issue or a block of one, and
 * a block it names is named in the message, so what is sent is what the bar says it acts on.
 */
export function useSendToAgentOffer({
  block,
  issue,
  send = sendToAgent,
  words,
}: {
  /** The block of the issue the chip names, in the chip's own words, and undefined where the
   *  chip names the issue itself. */
  readonly block: string | undefined;
  /** The issue the chip names, or undefined where it names no issue. */
  readonly issue: string | undefined;
  /** Write seam for tests; defaults to the real send. */
  readonly send?: SendToAgent;
  /** The words in the box, as the person typed them. */
  readonly words: string;
}): Offer | undefined {
  const writes = useWrites();
  const detail = useQuery({ ...issueQuery(issue), enabled: issue !== undefined && words !== "" });
  const claim = detail.data?.claim;
  const registry = useAgentRegistry(claim?.actor.kind === "session");
  const text = useAgentText();
  const [here, setState] = useWordsState<OfferState>(words);
  const holder = liveHolderOf(detail.data, registry);
  // Nothing to offer and nothing to say: no words, or no live agent on the issue. Once a press
  // has been made the offer stays, so a poll that finds the agent gone does not take back the
  // line saying what was already sent.
  if (issue === undefined || words === "" || (holder === undefined && here === undefined)) {
    return undefined;
  }
  const approve = (session: string) => {
    setState({ kind: "sending", words });
    send(writes, { block, issue, session, words }).then(
      (undelivered) => {
        setState(
          undelivered === undefined
            ? { agent: text.label({ id: session, kind: "session" }), kind: "sent", words }
            : { kind: "refused", refusal: undelivered, words }
        );
      },
      (error: unknown) => {
        setState({
          kind: "refused",
          refusal: refusalText(error, "the message did not reach Dispatch."),
          words,
        });
      }
    );
  };
  // The session to send to, carried rather than tested twice: the row is offered only while an
  // agent is live on the issue and no send of these words is out. The row is gone from the
  // press onward: one approval sends one message, so there is no second press to make, and the
  // line below says what became of it. A refused send puts the row back, because then nothing
  // was sent.
  const sendable =
    holder !== undefined && (here === undefined || here.kind === "refused") ? holder : undefined;
  return {
    row:
      sendable === undefined
        ? undefined
        : {
            id: SEND_TO_AGENT_OPTION_ID,
            kind: "offer",
            label: `Send this to the agent on ${issue}`,
            select: () => approve(sendable),
          },
    said: <SendToAgentSaid issue={issue} state={here} />,
  };
}

/** The one line the bar says about the message it is sending, sent, or could not send. */
function SendToAgentSaid({
  issue,
  state,
}: {
  readonly issue: string;
  readonly state: OfferState | undefined;
}): ReactNode {
  if (state === undefined) {
    return null;
  }
  if (state.kind === "sending") {
    return (
      <p className={barNoteClassName} role="status">
        Sending to the agent on {issue}…
      </p>
    );
  }
  if (state.kind === "refused") {
    return (
      <p className={barNoteClassName} data-testid="search-send-to-agent-said" role="alert">
        Could not send it to the agent on {issue}: {state.refusal}
      </p>
    );
  }
  return (
    <p className={barNoteClassName} data-testid="search-send-to-agent-said" role="status">
      Sent to {state.agent}, the agent on {issue}.
    </p>
  );
}
