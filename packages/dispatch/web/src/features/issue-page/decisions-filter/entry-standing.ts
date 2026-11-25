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
  /** How it stands as one line (`standingWords`). */
  readonly words: string;
}

/** The people a decision's state may name: who kept it, and whose Change it waits on. */
interface StandingPeople {
  readonly changedBy?: Actor | undefined;
  readonly keptBy?: Actor | undefined;
}

/**
 * How a node in the state `token` stands, in its parts: the word, and the person it names after
 * the word, where it names one. A person is named only where a decision stands kept, after "Kept
 * by", or asked to change, after "Changed by" (`DECISIONS_WORDS.changed_by`), which the page draws
 * in place of the change's own word (R2, R3): the two states whose words lead to a name. Every
 * place that draws a decision's state reads this one rule: the Decisions view (`entryStanding`),
 * the Conversation Details panel (`IssueTree.tsx`'s `focusedNodeOf`) and the Spec view's decision
 * line (`spec-filter/SpecFilterBody.tsx`); the outline's row draws the same (`NodeRow.tsx`'s
 * `RowMargin`).
 */
export function standingParts(
  token: NodeState,
  people: StandingPeople
): { readonly person?: Actor; readonly word: string } {
  const person =
    token === "kept" ? people.keptBy : token === "change_requested" ? people.changedBy : undefined;
  if (person === undefined) return { word: nodeStateWord(token) };
  return {
    person,
    word: token === "change_requested" ? DECISIONS_WORDS.changed_by : nodeStateWord(token),
  };
}

/** How a node in the state `token` stands, as one line: the word, then a space and the name of
 *  the person it names (`standingParts`), each person named by `nameOf` (`AgentText.author`), so
 *  a name is never an id (R9, R22). */
export function standingWords(
  token: NodeState,
  people: StandingPeople,
  nameOf: (actor: Actor) => string
): string {
  const { person, word } = standingParts(token, people);
  return person === undefined ? word : `${word} ${nameOf(person)}`;
}

/**
 * How `entry` stands, each person named by `nameOf` (`standingWords`). Undefined where the entry
 * stands in no state: the node an address names, which is drawn for its words, and a decision the
 * model gives no state.
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
  const people =
    entry.kind === "decision" ? { changedBy: entry.change?.by, keptBy: entry.keptBy } : {};
  return { token, words: standingWords(token, people, nameOf) };
}
