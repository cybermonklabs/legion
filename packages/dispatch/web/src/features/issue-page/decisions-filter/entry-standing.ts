import type { Actor } from "../../../api/types";
import { DECISIONS_WORDS, type NodeState, nodeStateWord } from "../page-words";
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
   *  who kept the decision where it stands kept; and "Changed by" (`DECISIONS_WORDS.changed_by`)
   *  and the name of the person whose Change it waits on where a person asked to change it: the
   *  state as one line. */
  readonly words: string;
}

/**
 * How `entry` stands, each person named by `nameOf` (`AgentText.author`), so a name is never
 * an id (R9, R22). Undefined where the entry stands in no state: the node an address names,
 * which is drawn for its words, and a decision the model gives no state. A person is named only
 * where the decision stands kept, after "Kept by", or asked to change, after "Changed by", which
 * the page draws in place of the change's own word (R2, R3): the two states whose words lead to
 * a name.
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
  const person =
    entry.kind !== "decision"
      ? undefined
      : token === "kept"
        ? entry.keptBy
        : token === "change_requested"
          ? entry.change?.by
          : undefined;
  const word =
    token === "change_requested" && person !== undefined
      ? DECISIONS_WORDS.changed_by
      : nodeStateWord(token);
  return { token, words: person === undefined ? word : `${word} ${nameOf(person)}` };
}
