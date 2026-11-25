import type { ReactNode } from "react";

import type { Actor, AskResolution } from "../../api/types";
import { AgentName, AskResolutionLine, askResolutionParts, useAgentText } from "../refs/AgentName";

/**
 * How the ask body names an agent. `live`, on today's surfaces and in the Inbox, draws
 * `AgentName`, as `main` does: its session form reads the live agent registry, which starts the
 * agents poll. `cache` names the agent from the registry the cache already holds
 * (`useAgentText`), and starts no read, for a caller that sends no agents read on its load.
 */
export type AskAgentNames = "live" | "cache";

/** An agent, as the ask body names it. */
export function AskAgent({
  actor,
  agentNames,
  inline,
}: {
  actor: Actor;
  agentNames: AskAgentNames;
  inline?: boolean;
}): ReactNode {
  return agentNames === "live" ? (
    <AgentName actor={actor} inline={inline} />
  ) : (
    <CachedAgent actor={actor} />
  );
}

function CachedAgent({ actor }: { actor: Actor }): ReactNode {
  return useAgentText().label(actor);
}

/** The actor sentence of a resolution (`AskResolutionLine`), as the ask body names its agent. */
export function AskResolutionActor({
  agentNames,
  resolution,
}: {
  agentNames: AskAgentNames;
  resolution: AskResolution;
}): ReactNode {
  return agentNames === "live" ? (
    <AskResolutionLine resolution={resolution} />
  ) : (
    <CachedResolution resolution={resolution} />
  );
}

function CachedResolution({ resolution }: { resolution: AskResolution }): ReactNode {
  const agentText = useAgentText();
  // The sentence alone: the record draws the reason after it itself, as it does after
  // `AskResolutionLine`.
  return askResolutionParts({ ...resolution, reason: "" })
    .map((part) => agentText.part(part))
    .join("");
}
