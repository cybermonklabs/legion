import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useRef } from "react";

import { api } from "../../api/client";
import type { Subscriber } from "../../api/types";
import { useAgentText } from "../refs/AgentName";
import { type KeyedWriteResult, useKeyedWrite } from "../writes/useKeyedWrite";
import { CONTROL_LABELS } from "./page-words";

/** The sessions subscribed to an issue, and the write that takes one off. The panel's People
 *  section draws both. */
export interface IssueSubscribers {
  /** Whether `rows.length` is a count of who is subscribed: once a read has answered, what the
   *  last answer said, kept through a later read that failed; before any has, it
   *  counts nobody, since the number would be a claim the page cannot make. */
  readonly counted: boolean;
  /** Whether the read is said as failed, with its Retry: while it stands failed, and while it is
   *  read again after it failed holding no answer, since a fetch of such a read clears its failure
   *  and puts it back to pending as it starts, which would take the line, and its Retry with a
   *  reader's focus, away at the press. `rows` still holds what an earlier read answered. */
  readonly error: boolean;
  /** How many times the read has failed, which keys the failure the line says
   *  (`DetailsErrorLineProps.failure`), so a read that fails again is said again. */
  readonly errorUpdateCount: number;
  /** The last failure itself, held while the read Retry sent again is out, so the panel can tell
   *  a listener that is refilling from one that failed (`DetailsErrorLine`). */
  readonly failure: unknown;
  readonly fetching: boolean;
  /** Sends the read again, or joins the read that is out rather than ending it and sending
   *  another (`cancelRefetch: false`). */
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
  // A fetch of a read that holds no answer clears its failure as it starts, while its line is
  // still drawn: that line goes on saying the failure it said, refilling or not.
  const lastFailure = useRef<unknown>(null);
  if (subscribers.error !== null) lastFailure.current = subscribers.error;
  const unsubscribe = useKeyedWrite({
    write: (sessionId: string, bounds) => api.unsubscribeIssueSession(issueKey, sessionId, bounds),
    method: "DELETE",
    inline: true,
    label: (sessionId) =>
      `${agentText.sessionText(sessionId, rows.find((row) => row.session_id === sessionId)?.title)} ${CONTROL_LABELS.unsubscribed_from} ${issueKey}`,
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["subscribers", issueKey] });
    },
  });
  return {
    counted: subscribers.data !== undefined,
    error: subscribers.isError || (subscribers.isPending && subscribers.errorUpdateCount > 0),
    errorUpdateCount: subscribers.errorUpdateCount,
    failure: subscribers.error ?? lastFailure.current,
    fetching: subscribers.isFetching,
    refetch: () => {
      void subscribers.refetch({ cancelRefetch: false });
    },
    rows,
    unsubscribe,
  };
}
