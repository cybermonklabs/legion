import { DISPATCH_SIGN_IN_SCOPES, type DispatchSignInScope } from "@legion/contracts";

import type { DispatchTokenScope } from "../../api/types";

/** A word a personal token's scopes can hold, as its checkbox reads it: what the reader ticks, and
 *  the line under it saying what a token holding it may do. */
export interface DispatchScopeChoice<Id extends DispatchTokenScope> {
  readonly id: Id;
  readonly label: string;
  readonly allows: string;
}

export const ACT_AS_YOU: DispatchScopeChoice<"person"> = {
  allows: "Reads and writes as you: issues, comments, asks, documents and messages.",
  id: "person",
  label: "Act as you",
};

export const APPROVE: DispatchScopeChoice<"approve"> = {
  allows:
    "Reviews documents, answers approvals and keeps decisions for you, and approves the credentials agents ask you for.",
  id: "approve",
  label: "Approve and control credentials",
};

export const READ_ONLY: DispatchScopeChoice<"read"> = {
  allows: "Only reads: Dispatch refuses it every write, whatever else it holds.",
  id: "read",
  label: "Read only",
};

export const SET_P0: DispatchScopeChoice<"p0"> = {
  allows: "Makes work P0: sets P0, or moves, clears or files work under a P0 issue.",
  id: "p0",
  label: "Set P0",
};

/** What each scope a Dispatch sign-in may ask for is called where a person decides it: the words
 *  Settings gives the same scope on a personal token. Keyed by contracts' own type, so a scope
 *  contracts adds to `DISPATCH_SIGN_IN_SCOPES` fails the build until it has its words here. */
const SIGN_IN_SCOPE_CHOICES: Readonly<
  Record<DispatchSignInScope, DispatchScopeChoice<DispatchSignInScope>>
> = { p0: SET_P0, person: ACT_AS_YOU };

/** The scopes a Dispatch sign-in may ask a person to approve, in contracts' order
 *  (`DISPATCH_SIGN_IN_SCOPES`, the list the broker and the clients read too), as the consent page
 *  draws them. */
export const DISPATCH_SIGN_IN_CHOICES: readonly DispatchScopeChoice<DispatchSignInScope>[] =
  DISPATCH_SIGN_IN_SCOPES.map((scope) => SIGN_IN_SCOPE_CHOICES[scope]);

/** The choice the consent page draws for `scope`, a word a sign-in asked for, or undefined where
 *  this build knows no such scope: a broker newer than this page may take one, and the page then
 *  shows the word as itself. */
export function signInScopeChoice(
  scope: string
): DispatchScopeChoice<DispatchSignInScope> | undefined {
  return DISPATCH_SIGN_IN_CHOICES.find((choice) => choice.id === scope);
}

/** Whether the consent page starts `scope`, a word a sign-in asked for, ticked: every scope it
 *  draws in its own words but `p0`, which the person ticks for that one sign-in themselves
 *  (dispatch://INFRA-5943/ask/3dfbf22b-74af-4e6d-af17-3bf368aac9fa), so no Approve press gives a
 *  session P0 unasked; and no word this build does not know, which the page draws as itself. */
export function signInScopeStartsTicked(scope: string): boolean {
  return scope !== SET_P0.id && signInScopeChoice(scope) !== undefined;
}

/** `scopes`, the words a sign-in asked for or was approved, as a line names them: each known scope
 *  by its Settings label and a word this build does not know as itself, so no scope a sign-in
 *  holds goes unnamed; "no scope" where there is none. */
export function signInScopesText(scopes: readonly string[]): string {
  return scopes.map((scope) => signInScopeChoice(scope)?.label ?? scope).join(", ") || "no scope";
}
