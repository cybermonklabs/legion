import { whoAmIQuery } from "../api/queries";
import { useShownQuery } from "../features/writes/shown-query";

/** Whether the signed-in principal is a person (`/auth/whoami`'s `kind: "user"`), whose sign-in
 *  holds the `person` scope that the routes behind these surfaces require: false while the
 *  principal is still loading and for every other principal, so a surface waits for the sign-in
 *  read before it asks and nothing without the scope cycles on a 403. */
export function useIsHuman(): boolean {
  return useShownQuery(whoAmIQuery()).data?.kind === "user";
}
