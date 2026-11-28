import type { AuthenticatedUser } from "./types";

/** The session the address the page was first opened at names in its `session` query parameter
 *  (`/projects/INFRA/settings?session=<id>`), kept for the page's life; undefined where it named
 *  none. An agent whose credential proves no session of its own - whoami answers it
 *  `session: null` - opens the dashboard on that parameter, so that its writes can name the
 *  session they are made for (`agentSession`). It is read once, as the page loads (`main.tsx`),
 *  since a move inside the app leaves the parameter off the address. */
let pageSession: string | undefined;

/** Keeps the `session` parameter of `search`, the query string of the address the page was first
 *  opened at (`pageSession`); a blank one, or none, keeps nothing. */
export function keepPageSession(search: string): void {
  const named = new URLSearchParams(search).get("session")?.trim() ?? "";
  pageSession = named === "" ? undefined : named;
}

/** The session the writes of `viewer` are made as: for an agent (`kind: "agent"`), the session its
 *  credential proved, else the one the page's first address named (`keepPageSession`); none for a
 *  person, whose cookie names the writer, and none for an agent with neither. A route that records
 *  a session as its writer takes it from a bearer's body (`actor`) when the bearer's credential
 *  proves none, and refuses a bearer that names none `400 ACTOR_KIND`. */
export function agentSession(viewer: AuthenticatedUser | undefined): string | undefined {
  if (viewer?.kind !== "agent") return undefined;
  return viewer.session ?? pageSession;
}
