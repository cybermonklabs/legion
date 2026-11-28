// packages/cipher-client/src/device-login-scopes.ts
//
// The scopes of a Dispatch sign-in, a device login of the scope `dispatch` (`device-login.ts`):
// the scopes a sign-in asks for, held to those its person may approve (contracts'
// `DISPATCH_SIGN_IN_SCOPES`), and the scopes a grant's answer names for one, held to those it
// asked for. `startDeviceLogin` reads the first, and `finishDeviceLogin` and `refreshDeviceLogin`
// the second. Nothing here sends a request: each takes what it checks from its caller.

import { DISPATCH_SIGN_IN_SCOPES, type DispatchSignInScope } from "@legion/contracts";
import type { TokenEndpointResponse } from "openid-client";
import { DISPATCH_LOGIN_SCOPE } from "./wire";

/** `scopes` as a Dispatch sign-in asks for them: sorted and distinct, each one a scope its person
 *  may approve (contracts' `DISPATCH_SIGN_IN_SCOPES`), else refused with a `RangeError`. */
export function signInScopes(scopes: readonly string[]): readonly DispatchSignInScope[] {
  const consentable: readonly string[] = DISPATCH_SIGN_IN_SCOPES;
  const unique = [...new Set(scopes)].sort();
  if (unique.some((scope) => !consentable.includes(scope))) {
    throw new RangeError(
      `a Dispatch sign-in may ask only for these scopes: ${DISPATCH_SIGN_IN_SCOPES.join(", ")}`
    );
  }
  return unique as DispatchSignInScope[];
}

/**
 * The Dispatch scopes a grant's answer, `answered`, names for a sign-in that asked for `asked`, or
 * undefined where it names none such: the answer must name the scope `dispatch` and, as
 * `dispatch_scopes`, distinct scopes, each one asked for, and, where `exact`, every one. The first
 * login takes any of the asked scopes, since its person may tick fewer than it asked for; a
 * refresh takes the same scopes again, since the broker never changes what the person approved.
 * A refresh of a sign-in whose file names none, which an earlier build's refresh left, takes any
 * scope a sign-in may hold, as a first login does, since nothing is left to hold it to.
 * Each scope asked for is one a sign-in may hold ({@link signInScopes}, or the file's own check),
 * so each scope this answers is one too.
 */
export function signInScopesOf(
  answered: TokenEndpointResponse,
  asked: readonly DispatchSignInScope[],
  exact: boolean
): readonly DispatchSignInScope[] | undefined {
  const named = answered["dispatch_scopes"];
  if (answered.scope !== DISPATCH_LOGIN_SCOPE || !Array.isArray(named)) return undefined;
  const askedFor: readonly unknown[] = asked;
  if (new Set(named).size !== named.length || named.some((scope) => !askedFor.includes(scope))) {
    return undefined;
  }
  if (exact && named.length !== asked.length) return undefined;
  return named as DispatchSignInScope[];
}
