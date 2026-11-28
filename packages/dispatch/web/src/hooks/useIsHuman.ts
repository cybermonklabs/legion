import { useQuery } from "@tanstack/react-query";

import { whoAmIQuery } from "../api/queries";

/** Whether the signed-in principal is a person (`/auth/whoami`'s `kind: "user"`): false while the
 *  principal is still loading and for every other principal. It decides what a surface offers a
 *  person alone, such as the controls of a write the server takes from a person only, and never
 *  whether a read of a route behind the `person` scope is asked, which `useHoldsPersonScope`
 *  alone decides. */
export function useIsHuman(): boolean {
  return useQuery(whoAmIQuery()).data?.kind === "user";
}
