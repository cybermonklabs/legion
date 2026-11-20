/** How the harness's Dispatch servers sign a person in.
 *
 * `DISPATCH_E2E_SIGNIN` unset (or empty) is dev sign-in: each server mounts `/auth/_dev/signin`
 * (`DISPATCH_DEV_SIGNIN=1`), where `e2e/users.ts` and `e2e/api.ts` sign `alice` and `bob` in.
 * `oidc` and `google` are the production sign-in, through `packages/envoy/cmd/dev-oidc-issuer` as
 * the issuer: the harness starts that issuer (`e2e/run-signin-issuer.sh`), and every server takes
 * it as `DISPATCH_SIGNIN_ISSUER` under one `DISPATCH_SIGNING_KEY` with dev sign-in off, so a
 * browser signs in at `/auth/start` and `/auth/callback` as it does in a deployment.
 *
 * `oidc` is a Cognito sign-in pool's shape: the servers admit the pool group
 * `oidcSignIn.group`, and the issuer signs in `oidcSignIn.person`, a member of it, at every
 * authorization. Only `e2e/signin.multiprocess.e2e.ts` runs then, and `oidc` needs
 * `DISPATCH_E2E_PROCESSES=2`: what it proves is that a sign-in started on one process completes on
 * the other.
 *
 * `google` is Google's shape, with the issuer standing in for Google: the servers admit the
 * Workspace domain `googleSignIn.domain` (`DISPATCH_SIGNIN_DOMAIN`, which only
 * `DISPATCH_TEST_SIGNIN_DOMAIN_ISSUER=1` lets a server take from an issuer other than Google's).
 * Each server answers at a second origin, `googleOutsideOrigin` of its canonical one
 * (`DISPATCH_OUTSIDE_ORIGIN`), where passkey sign-in is on (`DISPATCH_SIGNIN_PASSKEY=1`): a person
 * outside the domain whom a project's access list names enrolls a passkey through the link the
 * project's owner makes, and signs in with it there as a restricted member. The issuer signs in
 * nobody of its own: a spec queues each person, with the claims Google gives that account, at the
 * issuer's `/_dev/sign-in-as`. Only `e2e/outside-passkey.e2e.ts` runs then, with one process or
 * two.
 *
 * Any other value is refused naming the allowed ones, as `e2e/harness-processes.ts` refuses its
 * own, and `e2e/run-server.sh` refuses it the same way.
 *
 * Like `e2e/harness-processes.ts`, this module reads only the environment and opens nothing, so
 * every process in `e2e/` may import it.
 */

import { harnessProcessCount } from "./harness-processes";

export type HarnessSignIn = "dev" | "oidc" | "google";

/** Dispatch's confidential client at the dev OIDC issuer, under either mode. */
export const signInClientId = "dispatch-e2e";

/** Dispatch's client at the dev OIDC issuer, the sign-in pool group the servers admit
 *  (`DISPATCH_SIGNIN_GROUP`), and the person the issuer signs in with none queued, a member of
 *  that group (`--sign-in-as`, `--sign-in-group`). */
export const oidcSignIn = {
  clientId: signInClientId,
  group: "dispatch-e2e-people",
  person: "alice@example.com",
} as const;

/** Dispatch's client at the dev OIDC issuer standing in for Google, the Workspace domain the
 *  servers admit (`DISPATCH_SIGNIN_DOMAIN`), and the host of the second origin each server
 *  answers at (`googleOutsideOrigin`). */
export const googleSignIn = {
  clientId: signInClientId,
  domain: "example.com",
  outsideHost: "localhost",
} as const;

/** The second origin a server of `google` answers at, beside its canonical origin `canonical`:
 *  the same scheme and port under `googleSignIn.outsideHost`, the origin `e2e/run-server.sh`
 *  hands the server as `DISPATCH_OUTSIDE_ORIGIN`. A browser reaches both, and each holds a
 *  session of its own, since a cookie is the host's that set it. */
export function googleOutsideOrigin(canonical: string): string {
  const url = new URL(canonical);
  url.hostname = googleSignIn.outsideHost;
  return url.origin;
}

function resolveSignIn(): HarnessSignIn {
  const requested = process.env.DISPATCH_E2E_SIGNIN;
  if (requested === undefined || requested === "") return "dev";
  if (requested !== "oidc" && requested !== "google") {
    throw new Error(
      `DISPATCH_E2E_SIGNIN must be "oidc", "google" or unset, not ${JSON.stringify(requested)}. ` +
        "Leave it unset to sign people in through dev sign-in, set it to oidc, with " +
        "DISPATCH_E2E_PROCESSES=2, to sign in through the dev OIDC issuer as a sign-in pool, or " +
        "set it to google to sign in through that issuer as Google, at two origins."
    );
  }
  if (requested === "oidc" && harnessProcessCount !== 2) {
    throw new Error(
      "DISPATCH_E2E_SIGNIN=oidc needs DISPATCH_E2E_PROCESSES=2: its one spec starts a sign-in on " +
        "Dispatch A and completes it on B. Run `DISPATCH_E2E_SIGNIN=oidc bun run e2e:multiprocess`."
    );
  }
  return requested;
}

export const harnessSignIn = resolveSignIn();
