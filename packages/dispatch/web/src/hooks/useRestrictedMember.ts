import { useQuery } from "@tanstack/react-query";

import { whoAmIQuery } from "../api/queries";

/** Whether the signed-in person is a restricted member (`/auth/whoami`'s `member_kind`), who reads
 *  only the projects that name them (dispatch://INFRA-3862/spec#b-r-cross-project-reads).
 *  Dispatch refuses such a person every read of the deployment's own rows - the priorities,
 *  Delivery, the broadcasts, the credential requests and machine logins, the groups, the delivery
 *  and new-issue settings (403 DEPLOYMENT_READ_REFUSED, `readclass.Deployment` in
 *  `api/read_classes.go`) - so the dashboard asks them for none of those, and draws what each
 *  would have drawn as absent rather than failed.
 *
 *  The shell draws no page until `/auth/whoami` has answered (`AuthGate`), so every page reads
 *  the answer it already holds and a restricted member's first render asks nothing it may not.
 *  An identity not yet read is a full member's, as every person was before member kinds
 *  existed, so a full member's reads are the ones they always were. */
export function useRestrictedMember(): boolean {
  return useQuery(whoAmIQuery()).data?.member_kind === "restricted";
}
