import type { RequestCredential } from "./bearer";

/**
 * The variable that holds a CI job's results capability as the job sees it: the secrets broker's
 * proxy placeholder of the run's result grant, which `agent-secrets result-grant -- <command>`
 * gives the command it runs, beside the airlock proxy's settings. The job holds nothing else of
 * Dispatch's: the broker holds the run's results token and the key it is bound to.
 */
export const AIRLOCK_RESULTS_VAR = "DISPATCH_RESULTS_TOKEN";

/**
 * The credential of a write that goes out through the secrets broker's airlock: the placeholder
 * as its one `Authorization` header, `DPoP <placeholder>`, and nothing else. It signs no proof and
 * holds no key: the airlock proxy puts the run's results token in place of the placeholder, and a
 * proof of its own pod's key in place of any `DPoP` header the request carries. Dispatch refuses
 * the placeholder itself, so a write that goes around the proxy authenticates as nothing.
 */
export function airlockCredential(placeholder: string): RequestCredential {
  return {
    scheme: "DPoP",
    headers: () => ({ Authorization: `DPoP ${placeholder}` }),
    secret: () => placeholder,
  };
}
