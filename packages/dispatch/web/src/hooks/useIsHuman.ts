import { type QueryStatus, useQuery } from "@tanstack/react-query";

import { whoAmIQuery } from "../api/queries";

type FocusRule = (query: { readonly state: { readonly status: QueryStatus } }) => "always" | false;
/** The freshness of a read that events keep fresh: `EVENT_FRESH`'s two options, by shape. */
export type EventFreshness = Readonly<{ refetchOnWindowFocus: FocusRule; staleTime: number }>;

/** Whether the signed-in principal is a person (`/auth/whoami`'s `kind: "user"`), whose sign-in
 *  holds the `person` scope that the routes behind these surfaces require: false while the
 *  principal is still loading and for every other principal, so a surface waits for the sign-in
 *  read before it asks and nothing without the scope cycles on a 403. */
export function useIsHuman(options?: EventFreshness): boolean {
  return useQuery({ ...whoAmIQuery(), ...options }).data?.kind === "user";
}
