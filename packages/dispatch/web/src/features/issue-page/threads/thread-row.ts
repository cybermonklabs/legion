/**
 * What one row of Threads says of its record (dispatch://INFRA-5704/spec#b-r-row): its kind, who
 * is in it and its last activity (R7). Its first line is the page's one line of a node, drawn by
 * the row itself through the shared projection (`ThreadRow.tsx`), and its mark is `recordMark`'s
 * (`thread-marks.ts`). Every word comes from the page's list (`THREADS_WORDS`, R17), and a name
 * comes from the page's own naming of an author on a row, which the view hands in
 * (`useRowNames` in `ThreadList.tsx`, root R9).
 */
import type { Actor } from "../../../api/types";
import { partyOfAddress } from "../../refs/routes";
import { THREADS_WORDS } from "../page-words";
import type { ThreadRecord } from "./thread-records";

/** How the view names an author. */
export interface RowNames {
  /** The page's name of an author on a row (`useRowNames`): a person as `useAgentText()` names
   *  them, and an agent by its role's word. */
  readonly author: (actor: Actor) => string;
  /** The key of the issue's project, whose Legion daemon's session is
   *  `legion-daemon:<PROJECT>`. */
  readonly project: string;
}

/** What a row draws of its record, but its first line and its mark. */
export interface RowText {
  /** The words of the record's kind. */
  readonly kind: string;
  /** For a message whose root targets an agent, that agent's name, which ends `kind`. A row
   *  draws it as a value of the page, not a word of its list (`data-author-text`). */
  readonly agent: string | undefined;
  /** Each author of the record once, in the order of their first write. */
  readonly people: readonly string[];
  /** The record's last activity, a server time. */
  readonly activity: string;
}

/** An author's name on a row and on an entry of an opened record (R7, R9): the Legion daemon by
 *  the page's words and its project's key, which the page never draws as its session's id, and
 *  every other author as the page names one. */
export function nameOf(actor: Actor, names: RowNames): string {
  return actor.kind === "session" && actor.id === `legion-daemon:${names.project}`
    ? `${THREADS_WORDS.daemon} · ${names.project}`
    : names.author(actor);
}

/** The agent a message's root targets: a role by its token, and a session as the row names the
 *  author of that session, from a write of the record where it wrote one. */
function targetName(target: string, record: ThreadRecord, names: RowNames): string {
  const party = partyOfAddress(target);
  if (party?.kind === "role") return party.role;
  const session = party?.kind === "agent-session" ? party.session : target;
  const author = record.writes.find(
    (write) => write.author.kind === "session" && write.author.id === session
  )?.author;
  return nameOf(author ?? { id: session, kind: "session" }, names);
}

/** What the row of `record` draws but its first line and its mark (R7). */
export function threadRow(record: ThreadRecord, names: RowNames): RowText {
  const words = THREADS_WORDS.kinds;
  const agent =
    record.kind === "message" && record.root.target !== null
      ? targetName(record.root.target, record, names)
      : undefined;
  const kind =
    record.kind === "ask"
      ? words.ask
      : record.kind === "comment"
        ? record.onText
          ? words.text
          : words.comment
        : agent === undefined
          ? words.message
          : `${words.message_to} ${agent}`;
  const people: string[] = [];
  for (const write of record.writes) {
    const name = nameOf(write.author, names);
    if (!people.includes(name)) people.push(name);
  }
  return { activity: record.lastActivity, agent, kind, people };
}
