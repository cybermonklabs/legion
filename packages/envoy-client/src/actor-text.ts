import { type Actor, actorActText, serviceSubjectLabel } from "@legion/contracts";

/** How an event, comment, message or ask author reads in the tools: `<kind> <id>`, plus
 *  ` (as <namespace>/<name>)` — the verified service token's subject through
 *  `serviceSubjectLabel`, which keeps the namespace because every namespace has a `default`
 *  service account — when a service token authenticated the write, or `actorActText`'s act when
 *  an agent made the write for the person it acts for. A claim's holder is named by `claimText`
 *  instead, which follows the dashboard's session label. */
export function actorText(actor: Actor): string {
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
 * when. A deleted post gives no part of its body to any read, so this line stands wherever the
 * body would, and never an empty one. The actor is unknown only on an older record that kept
 * the time alone, which reads as `its author`.
 *
 * One wording for every surface: the post reads a tool prints (`dispatch-execute.ts`) and the
 * answers list a session is handed (`session-answers.ts`) both say this sentence.
 */
export function deletedMark(at: string, by: Actor | null | undefined): string {
  return `Deleted by ${by == null ? "its author" : actorText(by)} at ${at}`;
}
