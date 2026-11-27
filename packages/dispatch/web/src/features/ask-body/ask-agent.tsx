import type { ReactNode } from "react";

import type { Actor, AskResolution } from "../../api/types";
import type { AgentNames } from "../conversation/CachedAuthor";
import { AgentName, AskResolutionLine, askResolutionParts, useAgentText } from "../refs/AgentName";
import { authored, type TextMarks } from "./ask-text";

/** An agent, as the ask body names it (`AgentNames`): `live` draws `AgentName`, `cache` names it
 *  from the registry the cache already holds. A host that hands `marks` gets the cached name
 *  inside the mark of a value (`authored`); `AgentName` marks its own inside `NoTooltips`. */
export function AskAgent({
  actor,
  agentNames,
  inline,
  marks,
}: {
  actor: Actor;
  agentNames: AgentNames;
  inline?: boolean;
  marks?: TextMarks;
}): ReactNode {
  return agentNames === "live" ? (
    <AgentName actor={actor} inline={inline} />
  ) : (
    <CachedAgent actor={actor} marks={marks} />
  );
}

function CachedAgent({ actor, marks }: { actor: Actor; marks: TextMarks | undefined }): ReactNode {
  return authored(marks, useAgentText().label(actor));
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
