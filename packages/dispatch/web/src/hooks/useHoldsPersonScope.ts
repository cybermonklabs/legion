import { useQuery } from "@tanstack/react-query";

import {
  agentMessagesQuery,
  inboxQuery,
  userAgentStateQuery,
  userStateQuery,
  usersQuery,
  whoAmIQuery,
  whoAmIScopesQuery,
} from "../api/queries";
import { EVENT_FRESH } from "../api/query-refresh";
import type { DispatchScope } from "../api/types";

/** The scope the routes behind a person's own lists require (`OAuthScopes` in the Go routes). */
const PERSON_SCOPE: DispatchScope = "person";

/** Why a surface drawn under the `person` scope shows nothing for a viewer whose credential lacks
 *  it, as the end of its "Could not load" line. */
export const PERSON_SCOPE_LACKING = "this page's credential does not hold the person scope.";

/** The two reads the gate answers from: who is signed in (`/auth/whoami`), and, for an agent
 *  alone, the scopes its credential holds (`GET /api/v1/whoami`). An observer of who is signed
 *  in schedules no read of its own, so both are observed as events keep a read fresh
 *  (`EVENT_FRESH`): neither a mount nor a focus reads either again while it holds an answer (a
 *  focus reads one again only while it is in error), as every other observer of who is signed in
 *  on these surfaces does. A missing answer is still read once, and a check of the sign-in
 *  (`askSignInAgain`) reads it again. */
function useScopeReads() {
  const whoAmI = useQuery({ ...whoAmIQuery(), ...EVENT_FRESH });
  const scopes = useQuery({
    ...whoAmIScopesQuery(),
    ...EVENT_FRESH,
    enabled: whoAmI.data?.kind === "agent",
  });
  return { scopes, whoAmI };
}

/** Whether the viewer's credential holds the `person` scope that the routes behind a person's own
 *  lists require: `held` for a person signed in to the dashboard (`/auth/whoami`'s `kind: "user"`),
 *  whose sign-in holds it, and for an agent whose scopes (`GET /api/v1/whoami`) name it, as a
 *  personal token minted with it does; `lacking` for an agent whose scopes do not name it, and
 *  where either read failed; `pending` while whoami loads, or an agent's scopes do. */
export function usePersonScope(): "held" | "lacking" | "pending" {
  const { scopes, whoAmI } = useScopeReads();
  const kind = whoAmI.data?.kind;
  if (kind === "user") return "held";
  if (kind === "agent") {
    if (scopes.isPending) return "pending";
    return scopes.data?.includes(PERSON_SCOPE) === true ? "held" : "lacking";
  }
  return whoAmI.isPending ? "pending" : "lacking";
}

/** Whether a read says the viewer's credential holds the `person` scope (`usePersonScope`'s
 *  `held`): false while either read loads, so a surface asks only once a read says it is held,
 *  and nothing without it cycles on a 403. It reads the two answers alone, never whether a read
 *  is out, so a surface under it draws again only when an answer changes, not when a read that
 *  answers nothing fails. It is the one gate of every read of a route behind the `person` scope,
 *  whatever the surface draws: the viewer's kind (`AuthenticatedUser.kind`) decides only what is
 *  drawn for an agent (no Inbox and no Peek), never what is asked. A key that several surfaces
 *  read is read through its own hook below, so every surface asks it under the same gate and none
 *  hands another an answer that one would not have asked for. */
export function useHoldsPersonScope(): boolean {
  const { scopes, whoAmI } = useScopeReads();
  const kind = whoAmI.data?.kind;
  return kind === "user" || (kind === "agent" && scopes.data?.includes(PERSON_SCOPE) === true);
}

/** The asks waiting on the viewer (`inboxQuery`), under the one gate: the rail, the Inbox, its
 *  count, a document's margin, the Agents page and a machine's page read the key here. */
export function useInbox() {
  return useQuery({ ...inboxQuery(), enabled: useHoldsPersonScope() });
}

/** The viewer's state of each issue - its pin, its read mark and its dismissed asks
 *  (`userStateQuery`) - under the one gate, once `wanted`: the issue page, the board, the list
 *  and a document's margin read the key here. */
export function useUserState(wanted = true) {
  const holds = useHoldsPersonScope();
  return useQuery({ ...userStateQuery(), enabled: wanted && holds });
}

/** The viewer's state of each session's conversation - its pin, its read marks and its unread
 *  replies (`userAgentStateQuery`) - under the one gate: the shell, the rail, the Agents page, a
 *  machine's page and the live view read the key here. */
export function useUserAgentState() {
  return useQuery({ ...userAgentStateQuery(), enabled: useHoldsPersonScope() });
}

/** Everyone who has signed in (`usersQuery`), under the one gate, once a reader reaches for a
 *  control that lists them (`wanted`): every picker of a person reads the key here. */
export function useUsers(wanted: boolean) {
  const holds = useHoldsPersonScope();
  return useQuery({ ...usersQuery(), enabled: wanted && holds });
}

/** A session's conversation with the viewer (`agentMessagesQuery`), under the one gate: the
 *  Agents page's row and composer, the live view and an Agent thread of the Inbox read the key
 *  here. */
export function useAgentMessages(sessionId: string) {
  return useQuery({ ...agentMessagesQuery(sessionId), enabled: useHoldsPersonScope() });
}
