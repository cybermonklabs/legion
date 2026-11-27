import { useQuery } from "@tanstack/react-query";

import { whoAmIQuery, whoAmIScopesQuery } from "../api/queries";
import { refetchFailedOnFocus } from "../api/query-refresh";
import type { DispatchScope } from "../api/types";

/** The scope the routes behind a person's own lists require (`OAuthScopes` in the Go routes). */
const PERSON_SCOPE: DispatchScope = "person";

/** How this hook observes who is signed in and what their credential holds: an observer of who is
 *  signed in schedules no read of its own, so neither a mount nor a focus reads either again while
 *  it holds an answer (a focus reads one again only while it is in error), as every other
 *  observer of who is signed in on these surfaces does (`EVENT_FRESH`). A missing answer is still
 *  read once, and a check of the sign-in (`askSignInAgain`) reads it again. */
const OBSERVED = {
  refetchOnWindowFocus: refetchFailedOnFocus,
  staleTime: Number.POSITIVE_INFINITY,
} as const;

/** Why a surface drawn under the `person` scope shows nothing for a viewer whose credential lacks
 *  it, as the end of its "Could not load" line. */
export const PERSON_SCOPE_LACKING = "this page's credential does not hold the person scope.";

/** Whether the viewer's credential holds the `person` scope that the routes behind a person's own
 *  lists require: `held` for a person signed in to the dashboard (`/auth/whoami`'s `kind: "user"`),
 *  whose sign-in holds it, and for an agent whose scopes (`GET /api/v1/whoami`) name it, as a
 *  personal token minted with it does; `lacking` for an agent whose scopes do not name it, and
 *  where either read failed; `pending` while whoami loads, or an agent's scopes do. The scopes are
 *  read for an agent alone. */
export function usePersonScope(): "held" | "lacking" | "pending" {
  const whoAmI = useQuery({ ...whoAmIQuery(), ...OBSERVED });
  const kind = whoAmI.data?.kind;
  const scopes = useQuery({ ...whoAmIScopesQuery(), ...OBSERVED, enabled: kind === "agent" });
  if (kind === "user") return "held";
  if (kind === "agent") {
    if (scopes.isPending) return "pending";
    return scopes.data?.includes(PERSON_SCOPE) === true ? "held" : "lacking";
  }
  return whoAmI.isPending ? "pending" : "lacking";
}

/** Whether a read says the viewer's credential holds the `person` scope (`usePersonScope`): false
 *  while either read loads, so a surface drawn for any viewer that holds it asks only once a read
 *  says it is held, and nothing without it cycles on a 403. A surface drawn for a person alone
 *  asks under `useIsHuman` instead. */
export function useHoldsPersonScope(): boolean {
  return usePersonScope() === "held";
}
