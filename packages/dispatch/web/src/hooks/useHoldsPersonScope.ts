import { type DefinedUseQueryResult, type UseQueryResult, useQuery } from "@tanstack/react-query";

import { api } from "../api/client";
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
import type { AuthenticatedUser, BroadcastRead, DispatchScope } from "../api/types";

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

/** The gate's rule over the two reads' answers: whether the viewer's credential holds the `person`
 *  scope that the routes behind a person's own lists require. A person signed in to the dashboard
 *  (`/auth/whoami`'s `kind: "user"`) holds it, since their sign-in does, and an agent holds it
 *  where its scopes (`GET /api/v1/whoami`) name it, as a personal token minted with it does; an
 *  answer not yet read holds nothing. The gate's two hooks, `useHoldsPersonScope` and
 *  `usePersonScope`, answer by this rule alone, so who holds the scope is decided here for both. */
function holdsPersonScope(
  kind: AuthenticatedUser["kind"] | undefined,
  scopes: readonly string[] | undefined
): boolean {
  return kind === "user" || (kind === "agent" && scopes?.includes(PERSON_SCOPE) === true);
}

/** Whether the viewer's credential holds the `person` scope, as one of three answers: `held`
 *  where the two reads' answers hold it (`holdsPersonScope`), so exactly where
 *  `useHoldsPersonScope` is true; `pending` while whoami loads, or an agent's scopes do; and
 *  `lacking` otherwise, for an agent whose scopes do not name it and where either read failed. A
 *  surface reads this hook where what it draws tells a wait for the reads apart from a credential
 *  that lacks the scope. */
export function usePersonScope(): "held" | "lacking" | "pending" {
  const { scopes, whoAmI } = useScopeReads();
  const kind = whoAmI.data?.kind;
  if (holdsPersonScope(kind, scopes.data)) return "held";
  if (kind === "agent") return scopes.isPending ? "pending" : "lacking";
  return whoAmI.isPending ? "pending" : "lacking";
}

/** Whether a read says the viewer's credential holds the `person` scope (`holdsPersonScope`):
 *  false while either read loads, so a surface asks only once a read says it is held, and
 *  nothing without it cycles on a 403. It reads the two answers alone, never whether a read is
 *  out, so a surface under it draws again only when an answer changes, not when a read that
 *  answers nothing fails. It and `usePersonScope` are the gate of every read of a route behind
 *  the `person` scope, whatever the surface draws: both answer by `holdsPersonScope` from the
 *  same two reads, so every read asks under one rule. The viewer's kind
 *  (`AuthenticatedUser.kind`) decides only what is drawn for an agent (no Inbox and no Peek),
 *  never what is asked. A key that several surfaces read is read through one hook of its own,
 *  under this one - those below, or one beside its feature, as `useCredentialPending` is - so
 *  none hands another an answer that one would not have asked for. */
export function useHoldsPersonScope(): boolean {
  const { scopes, whoAmI } = useScopeReads();
  return holdsPersonScope(whoAmI.data?.kind, scopes.data);
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

/** What a list of subscribed sessions is of: an issue's topics, by the issue's key, or a project
 *  document's, by the document's id. */
export interface SubscriptionOwner {
  readonly id: string;
  readonly kind: "issue" | "document";
}

/** The sessions subscribed to an owner's topics (`GET /api/v1/issues/{key}/subscribers`, `GET
 *  /api/v1/artifacts/{id}/subscribers`), under the one gate, once there is an owner: the issue's
 *  header, a project document's page and an ask's card read the key here, so an Inbox of cards on
 *  one issue shares one read with its header, and a removal from any of them refreshes all of
 *  them. `staleTime` is the reader's own: an ask's card holds the list fresh for a while. */
export function useSubscribers(
  owner: SubscriptionOwner | undefined,
  options: { readonly staleTime?: number } = {}
) {
  const holds = useHoldsPersonScope();
  return useQuery({
    ...options,
    enabled: owner !== undefined && holds,
    queryKey: ["subscribers", owner?.id],
    queryFn: () => {
      if (owner === undefined) {
        throw new Error("Subscribers query requires an owner.");
      }
      return owner.kind === "issue"
        ? api.getIssueSubscribers(owner.id)
        : api.getArtifactSubscribers(owner.id);
    },
  });
}

/** One broadcast (`GET /api/v1/broadcasts/{id}`), under the one gate: its own page and the Agents
 *  page's row of a broadcast just sent read the key here. It is keyed under `["broadcast"]`, the
 *  prefix every issue-less message event refreshes, so a recipient's delivery receipt or reply
 *  lands without a poll. `sent` is the broadcast as its send answered: the read starts from it,
 *  so it is drawn until a read answers, and for good for a viewer without the scope. */
export function useBroadcast(id: string): UseQueryResult<BroadcastRead>;
export function useBroadcast(id: string, sent: BroadcastRead): DefinedUseQueryResult<BroadcastRead>;
export function useBroadcast(id: string, sent?: BroadcastRead): UseQueryResult<BroadcastRead> {
  const holds = useHoldsPersonScope();
  return useQuery({
    enabled: holds,
    initialData: sent,
    queryFn: () => api.getBroadcast(id),
    queryKey: ["broadcast", id],
  });
}
