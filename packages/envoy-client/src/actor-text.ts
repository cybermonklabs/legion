import {
  type Actor,
  actorActText,
  serviceSubjectLabel,
  type WrittenActor,
} from "@legion/contracts";

/** How an event, comment, message or ask author reads in the tools: `<kind> <id>`, plus
 *  ` (as <namespace>/<name>)` — the verified service token's subject through
 *  `serviceSubjectLabel`, which keeps the namespace because every namespace has a `default`
 *  service account — when a service token authenticated the write, or `actorActText`'s act when
 *  an agent made the write for the person it acts for. A claim's holder is named by `claimText`
 *  instead, which follows the dashboard's session label.
 *
 *  The actor is taken in either reading: the served row's (`Actor`), and the one a
 *  `DispatchEvent` payload carries (`WrittenActor`), which no build takes for the other
 *  (`dispatch-api-spellings.test.ts`). */
export function actorText(actor: Actor | WrittenActor): string {
  if (actor.kind === "user") {
    return actorActText(`user ${actor.id}`, actor.act);
  }
  const service = actor.kind === "session" ? actor.service : undefined;
  if (service === undefined) {
    return `${actor.kind} ${actor.id}`;
  }
  return `${actor.kind} ${actor.id} (as ${serviceSubjectLabel(service)})`;
}

/**
 * How every read names a post its author withdrew, in place of its body: who deleted it and
 * when. No read of a deleted post gives any part of its body, so this line stands wherever the
 * body would, and never an empty one. The actor is unknown only on a mark that stores no
 * actor, which no delete of this release writes, and it reads as `its author`.
 *
 * One wording for every surface: every read of a post a tool prints and a reply's quote of a
 * deleted parent (`dispatch-commands/render-item.ts`), an event of one
 * (`dispatch-commands/render-issue.ts`), a graph row naming one
 * (`dispatch-commands/render-graph.ts`), an Inbox thread whose root is one
 * (`dispatch-commands/session.ts`), a card's re-read of a body its event cut (`delivery.ts`),
 * the answers list a session is handed (`session-answers.ts`) and a search hit of one
 * (`search-answer.ts`) all say this sentence.
 */
export function deletedMark(at: string, by: Actor | WrittenActor | null | undefined): string {
  return `Deleted by ${by == null ? "its author" : actorText(by)} at ${at}`;
}

/**
 * How every line names a post its author edited, beside the words it prints of it: `edited` and
 * the time of the last edit. The words a line holds can be ones a later edit replaced, so a line
 * that prints a post's words without this gives a reader stale words with nothing to tell them
 * by. A line joins it to its other fields with ` · `, after the words where it prints any.
 *
 * One wording for every surface: a reply's quote of the block of a parent its author edited
 * (`dispatch-commands/render-item.ts`), an event of a post (`dispatch-commands/render-issue.ts`), a
 * graph row naming one and a read of one of its blocks (`dispatch-commands/render-graph.ts`), an
 * Inbox thread whose root is one (`dispatch-commands/session.ts`), a card's re-read of a body its
 * event cut (`delivery.ts`), the answers list a session is handed (`session-answers.ts`) and a
 * search hit of one (`search-answer.ts`) all print it.
 */
export function editedMark(at: string): string {
  return `edited ${at}`;
}

/**
 * `words`, what a line about a post holds so far, followed by ` · ` and `editedMark(at)` where its
 * author edited the post (`at`, the time of the last edit), and as they are where `at` is null or
 * undefined: the join of the mark to what the line prints before it, as `editedMark` says a line
 * makes it.
 */
export function withEditedMark(words: string, at: string | null | undefined): string {
  return at == null ? words : `${words} · ${editedMark(at)}`;
}
