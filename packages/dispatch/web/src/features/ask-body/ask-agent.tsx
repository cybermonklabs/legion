import type { ReactNode } from "react";

import type { Actor, AskResolution } from "../../api/types";
import type { AgentNames } from "../conversation/CachedAuthor";
import { AgentName, AskResolutionLine, askResolutionParts, useAgentText } from "../refs/AgentName";

/** An agent, as the ask body names it (`AgentNames`): `live` draws `AgentName`, `cache` names it
 *  from the registry the cache already holds. */
export function AskAgent({
  actor,
  agentNames,
  inline,
}: {
  actor: Actor;
  agentNames: AgentNames;
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
  agentNames: AgentNames;
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
