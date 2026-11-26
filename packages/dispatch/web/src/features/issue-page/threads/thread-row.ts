/**
 * What one row of Threads says of its record (dispatch://INFRA-5704/spec#b-r-row): its kind, its
 * first line, who is in it and its last activity (R7). Its mark is `recordMark`'s
 * (`thread-marks.ts`). Every word comes from the page's list (`THREADS_WORDS`, R17), and a name
 * comes from the page's own naming of an author, which the view hands in (`useAgentText()`,
 * root R9).
 */
import type { Actor } from "../../../api/types";
import { partyOfAddress } from "../../refs/routes";
import { FIRST_LINE_LIMIT } from "../first-line";
import { THREADS_WORDS } from "../page-words";
import type { ThreadRecord } from "./thread-records";

/** How the view names an author. */
export interface RowNames {
  /** The page's name of an author (`useAgentText().author`). */
  readonly author: (actor: Actor) => string;
  /** The key of the issue's project, whose Legion daemon's session is
   *  `legion-daemon:<PROJECT>`. */
  readonly project: string;
}

/** What a row draws of its record, but its mark. */
export interface RowText {
  /** The words of the record's kind. */
  readonly kind: string;
  /** The root's text to its first line break, cut at a word with an ellipsis where it is longer
   *  than a row holds. */
  readonly firstLine: string;
  /** Each author of the record once, in the order of their first write. */
  readonly people: readonly string[];
  /** The record's last activity, a server time. */
  readonly activity: string;
}

/** An author's name on a row: the Legion daemon by the page's words and its project's key, which
 *  the page never draws as its session's id, and every other author as the page names one. */
function nameOf(actor: Actor, names: RowNames): string {
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

/**
 * The text of `record`'s root to its first line break, and where that is longer than a row
 * holds (`FIRST_LINE_LIMIT`, the characters a row's first line holds on the new page), cut in
 * script after the last word that fits, with an ellipsis (R7), never by a style. A line whose
 * first word alone is longer is cut after that word.
 */
function firstLineOf(text: string): string {
  const line = (text.split("\n")[0] ?? "").trim();
  if (line.length <= FIRST_LINE_LIMIT) return line;
  const space = line.slice(0, FIRST_LINE_LIMIT + 1).search(/\s\S*$/u);
  if (space > 0) return `${line.slice(0, space).trimEnd()}…`;
  const word = line.search(/\s/u);
  return word < 0 ? line : `${line.slice(0, word)}…`;
}

/** What the row of `record` draws but its mark (R7). */
export function threadRow(record: ThreadRecord, names: RowNames): RowText {
  const words = THREADS_WORDS.kinds;
  const kind =
    record.kind === "ask"
      ? words.ask
      : record.kind === "comment"
        ? record.onText
          ? words.text
          : words.comment
        : record.root.target === null
          ? words.message
          : `${words.message_to} ${targetName(record.root.target, record, names)}`;
  const people: string[] = [];
  for (const write of record.writes) {
    const name = nameOf(write.author, names);
    if (!people.includes(name)) people.push(name);
  }
  return {
    activity: record.lastActivity,
    firstLine: firstLineOf(record.kind === "ask" ? record.root.question : record.root.body),
    kind,
    people,
  };
}
