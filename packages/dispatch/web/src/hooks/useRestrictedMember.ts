import { useQuery } from "@tanstack/react-query";

import { whoAmIQuery } from "../api/queries";
import type { AuthenticatedUser } from "../api/types";

/** Whether `user`, `/auth/whoami`'s answer, is a restricted member (`member_kind`), who reads only
 *  the projects that name them (R4 of the private-projects spec). An identity not yet read
 *  (`undefined`) is a full member's, as every person was before member kinds existed. */
export function isRestrictedMember(user: AuthenticatedUser | undefined): boolean {
  return user?.member_kind === "restricted";
}

/** Whether the signed-in person is a restricted member (`isRestrictedMember`).
 *  Dispatch refuses such a person every read of the deployment's own rows - the priorities, the
 *  Pipeline page, a delivery run's jobs, the broadcasts, the credential requests, the groups, the
 *  delivery and new-issue settings (403 DEPLOYMENT_READ_REFUSED, `readclass.Deployment` in
 *  `api/read_classes.go`) - so the dashboard asks them for none of those, and draws what each
 *  would have drawn as absent rather than failed. Delivery's timeline and measures are not among
 *  them: they answer such a person the pull requests whose issue they see. Nor are the machine
 *  logins: at their browser Dispatch answers them their own and no service's
 *  (`restrictedOwnLoginRoutes`), which the machine-login page lists for them (`MachineLoginsSection`
 *  with `ownOnly`).
 *
 *  The shell draws no page until `/auth/whoami` has answered (`AuthGate`), so every page reads
 *  the answer it already holds and a restricted member's first render asks nothing it may not.
 *  An identity not yet read is a full member's, so a full member's reads are the ones they always
 *  were. */
export function useRestrictedMember(): boolean {
  return isRestrictedMember(useQuery(whoAmIQuery()).data);
}
