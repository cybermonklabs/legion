import type { Actor } from "../../../api/types";
import { type NodeState, nodeStateWord } from "../page-words";
import type { DecisionsEntry, NamedEntry } from "./decisions-parts";

/**
 * How one entry of the Decisions filter stands, read once for its two readers so neither works
 * it out again: the row reads `token` for its `data-node-state` and its margin's state, and the
 * panel reads `words` for the facts of the focused node.
 */
interface EntryStanding {
  /** The state's token: an ask's own `state`, and a decision's standing with the reader. */
  readonly token: NodeState;
  /** The page's word for the token (`nodeStateWord`), then a space and the name of the person
   *  who kept the decision where it stands kept: the state as one line. */
  readonly words: string;
}

/**
 * How `entry` stands, each person named by `nameOf` (`AgentText.author`), so a name is never
 * an id (R9, R22). Undefined where the entry stands in no state: the node an address names,
 * which is drawn for its words, and a decision the model gives no state. A keeper is named only
 * where the decision stands kept, which is the one state whose word leads to a name.
 */
export function entryStanding(
  entry: DecisionsEntry | NamedEntry,
  nameOf: (actor: Actor) => string
): EntryStanding | undefined {
  const token =
    entry.kind === "ask"
      ? entry.node.state
      : entry.kind === "decision"
        ? entry.standing
        : undefined;
  if (token === undefined) return undefined;
  const word = nodeStateWord(token);
  const keeper =
    token === "kept" && entry.kind === "decision" && entry.keptBy !== undefined
      ? nameOf(entry.keptBy)
      : undefined;
  return { token, words: keeper === undefined ? word : `${word} ${keeper}` };
}
