import { useQuery, useQueryClient } from "@tanstack/react-query";

import { api } from "../../api/client";
import type { Subscriber } from "../../api/types";
import { useAgentText } from "../refs/AgentName";
import { type KeyedWriteResult, useKeyedWrite } from "../writes/useKeyedWrite";
import { CONTROL_LABELS } from "./page-words";

/** The sessions subscribed to an issue, and the write that takes one off. The panel's People
 *  section draws both (dispatch://INFRA-1196 R12). */
export interface IssueSubscribers {
  /** Whether `rows.length` is a count of who is subscribed: once a read has answered, what the
   *  last answer said, kept through a later read that failed (decision 98); before any has, it
   *  counts nobody, since the number would be a claim the page cannot make. */
  readonly counted: boolean;
  /** Whether the last read failed; `rows` still holds what an earlier read answered. */
  readonly error: boolean;
  /** The failure itself, so the panel can tell a listener that is refilling from one that
   *  failed (`DetailsErrorLine`). */
  readonly failure: unknown;
  readonly fetching: boolean;
  readonly refetch: () => void;
  readonly rows: readonly Subscriber[];
  readonly unsubscribe: KeyedWriteResult<void, string, unknown>;
}

export function useIssueSubscribers(issueKey: string): IssueSubscribers {
  const queryClient = useQueryClient();
  const agentText = useAgentText();
  const subscribers = useQuery({
    queryKey: ["subscribers", issueKey],
    queryFn: () => api.getIssueSubscribers(issueKey),
  });
  const rows = subscribers.data ?? [];
  const unsubscribe = useKeyedWrite({
    write: (sessionId: string, bounds) => api.unsubscribeIssueSession(issueKey, sessionId, bounds),
    inline: true,
    label: (sessionId) =>
      `${agentText.sessionText(sessionId, rows.find((row) => row.session_id === sessionId)?.title)} ${CONTROL_LABELS.unsubscribed_from} ${issueKey}`,
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["subscribers", issueKey] });
    },
  });
  return {
    counted: subscribers.data !== undefined,
    error: subscribers.isError,
    failure: subscribers.error,
    fetching: subscribers.isFetching,
    refetch: () => {
      void subscribers.refetch();
    },
    rows,
    unsubscribe,
  };
}
