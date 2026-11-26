import type { ReactNode } from "react";
import type { Actor } from "../../api/types";
import { useAgentText } from "../refs/AgentName";

/** An author's name from the registry the cache already holds, which starts no read: for a
 *  caller of the reply quote that sends no agents read when it draws
 *  (dispatch://INFRA-5702/spec#b-r-reads). */
export function CachedAuthor({ actor }: { actor: Actor }): ReactNode {
  return useAgentText().author(actor);
}
