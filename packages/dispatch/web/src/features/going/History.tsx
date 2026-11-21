import { useQuery } from "@tanstack/react-query";
import type { ReactNode } from "react";

import { goingConversationsQuery } from "../../api/queries";
import type { GoingConversation, GoingRun } from "../../api/types";
import { QueryError } from "../../components/QueryError";
import {
  borderDefault,
  secondaryButtonCompact,
  textMutedOnSurface,
  textPrimaryOnSurface,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { Timestamp } from "../refs/Timestamp";
import { openConversation } from "./going-store";
import { RunUndo } from "./RunUndo";
import { stateOfRecord } from "./run-frames";
import { type RunState, runEnded } from "./run-states";

/** How a run stands, as History lists it. */
const RUN_STATES: Readonly<Record<RunState, string>> = {
  done: "done",
  failed: "failed",
  idle: "",
  stopped_bound: "stopped at a bound",
  stopped_person: "stopped",
  waiting: "a plan waits",
  working: "working",
};

/** One run as History lists it: its words, how it stands, when it started, and Undo where it
 *  changed something. */
function HistoryRun({ run }: { readonly run: GoingRun }): ReactNode {
  const state = stateOfRecord(run.state, run.stop_reason);
  return (
    <li className="space-y-1 py-1" data-run={run.id} data-testid="going-history-run">
      <div className="flex flex-wrap items-baseline gap-x-2 text-xs">
        <span className={textPrimaryOnSurface} data-testid="going-history-words">
          {run.words}
        </span>
        <span className={textMutedOnSurface}>{RUN_STATES[state]}</span>
        <span className={textMutedOnSurface}>
          <Timestamp at={run.created_at} />
        </span>
      </div>
      {runEnded(state) && (run.issues ?? []).length > 0 ? <RunUndo run={run.id} /> : null}
    </li>
  );
}

/** One conversation: its runs, newest first, and Open, which shows it in the bar. */
function HistoryConversation({
  conversation,
  onOpen,
}: {
  readonly conversation: GoingConversation;
  readonly onOpen: () => void;
}): ReactNode {
  return (
    <li
      className={`space-y-1 border-t py-2 first:border-t-0 ${borderDefault}`}
      data-conversation={conversation.id}
      data-testid="going-history-conversation"
    >
      <div className="flex items-center gap-2">
        <span className={`mr-auto text-xs ${textSecondaryOnSurface}`}>
          <Timestamp at={conversation.newest_at} />
        </span>
        <button
          className={secondaryButtonCompact}
          data-testid="going-history-open"
          onClick={() => {
            openConversation(conversation);
            onOpen();
          }}
          type="button"
        >
          Open
        </button>
      </div>
      <ul>
        {(conversation.runs ?? []).map((run) => (
          <HistoryRun key={run.id} run={run} />
        ))}
      </ul>
    </li>
  );
}

/**
 * History (R11): the person's conversations of the last 7 days (`GET /api/v1/me/going-runs`),
 * the newest first, each with its runs, the newest first, as Dispatch keeps them. A run that
 * changed something keeps its Undo for as long as its conversation is listed here; Open shows the
 * conversation in the bar, where a follow-up sees the turns before it.
 */
export function History({ onOpen }: { readonly onOpen: () => void }): ReactNode {
  const conversations = useQuery(goingConversationsQuery());
  if (conversations.isPending) {
    return (
      <p className={`px-3 py-3 text-sm ${textMutedOnSurface}`} role="status">
        Reading History…
      </p>
    );
  }
  if (conversations.isError) {
    return (
      <div className="p-3">
        <QueryError
          message="History could not be read."
          onRetry={() => void conversations.refetch()}
          retrying={conversations.isFetching}
        />
      </div>
    );
  }
  const rows = conversations.data.rows ?? [];
  return (
    <section
      aria-label="History"
      className="max-h-[50vh] min-h-0 overflow-y-auto px-3 py-2"
      data-testid="going-history"
    >
      {rows.length === 0 ? (
        <p className={`text-sm ${textMutedOnSurface}`}>No conversation in the last 7 days.</p>
      ) : (
        <ul>
          {rows.map((conversation) => (
            <HistoryConversation
              conversation={conversation}
              key={conversation.id}
              onOpen={onOpen}
            />
          ))}
        </ul>
      )}
    </section>
  );
}
