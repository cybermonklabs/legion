import type { Actor, AskResolution } from "../../api/types";

/** Whether an actor is the signed-in viewer: a human whose login is theirs, in any casing, as
 *  the server matches it. */
export function isViewer(actor: Actor, viewerLogin: string | undefined): boolean {
  return (
    actor.kind === "user" &&
    viewerLogin !== undefined &&
    actor.id.toLowerCase() === viewerLogin.toLowerCase()
  );
}

/** The free-text reason a resolution summary shows after its actor, or `undefined` where there
 *  is none to show: a withdrawal, whose server-written reason only repeats what the actor
 *  sentence says, and a decline the person wrote no note with, which holds an empty reason
 *  (a retraction and a self-resolution each still carry one). */
export function askResolutionReason(resolution: AskResolution): string | undefined {
  if (resolution.kind === "withdrawn" || resolution.reason === "") return undefined;
  return resolution.reason;
}

/**
 * The word for each way an ask is closed, as `askResolutionBadge` below spells it: the badge the
 * card draws and the verb of its resolution line. The list of the new issue page's words reads
 * this table (`NODE_STATE_WORDS.ask` in `features/issue-page/page-words.ts`), so an ask's row
 * there says of a closed ask what its card says.
 */
export const ASK_RESOLUTION_WORDS = {
  declined: "Declined",
  resolved: "Resolved",
  retracted: "Retracted",
  withdrawn: "Withdrawn",
} as const;

/** The badge naming how an ask was resolved, which is also the verb of its resolution line. */
export function askResolutionBadge(resolution: AskResolution): string {
  switch (resolution.kind) {
    case "withdrawn":
      return "Withdrawn";
    case "retracted":
      return "Retracted";
    case "declined":
      return "Declined";
    case "resolved":
      return "Resolved";
    // The route table carries the kind as a plain string; a word this build does not know is
    // shown as the server wrote it rather than as an empty badge.
    default:
      return resolution.kind;
  }
}
