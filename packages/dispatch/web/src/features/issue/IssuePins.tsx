import { type InfiniteData, useQuery, useQueryClient } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { Link } from "react-router-dom";

import { api } from "../../api/client";
import type { DispatchEvent } from "../../api/types";
import { PinButton } from "../../components/PinButton";
import { linkText, pageInkText, pageMutedText, pageQuietText } from "../../theme/classes";
import { useMarginDrawer } from "../margin/margin-slot";
import { AgentParts } from "../refs/AgentName";
import { buildIssuePath } from "../refs/routes";
import { PHONE_VIEWPORT_QUERY, useMediaQuery } from "../shell/useDialog";
import { eventDescription } from "./event-description";
import { firstLineOf } from "./issue-tree";
import { authorText, pageChrome } from "./page-structure";
import { CONTROL_LABELS, NODE_TYPE_NAMES } from "./page-words";
import { writePin } from "./pin-write";
import { conversationItemId, eventItemId, pinnedConversations, pinnedEventIds } from "./pins";

/**
 * The reader's pins, as links to what they pinned (dispatch://INFRA-1196 R14): each pinned ask,
 * comment and message by its first line, a link to its own address, which focuses its row in the
 * outline (R20), with its Unpin beside it. A pin of what the outline draws no row for - a thread
 * agents wrote among themselves - links to its address too, which opens the history at it, and an
 * old pin of a handoff opens the history. Each line of the history the reader pinned
 * (`IssueHistory.tsx`'s Pin, an `event:<id>` pin) is listed under them: who did it and what the
 * line says, with a link that opens the history and its Unpin. The pin of a row itself is the
 * focused row's, in its facts (`FocusedNodeFacts.tsx`).
 *
 * The words come from the reads the outline and the history are drawn from, under their keys, so
 * the list costs no request of its own. Where a read has not answered, has failed, or holds no
 * words for a pin - a handoff, an event older than the history has read - the pin is drawn by
 * the page's word for its kind and never by its id (R17).
 */
export function IssuePins({
  dismissed,
  issueKey,
}: {
  dismissed: readonly string[];
  issueKey: string;
}): ReactNode {
  const queryClient = useQueryClient();
  const drawer = useMarginDrawer();
  const phone = useMediaQuery(PHONE_VIEWPORT_QUERY);
  const pins = pinnedConversations([...dismissed]);
  const eventPins = pinnedEventIds([...dismissed]);
  const reading = pins.length > 0;
  const asks = useQuery({
    enabled: reading,
    queryFn: () => api.listIssueAsks(issueKey, "all"),
    queryKey: ["asks", issueKey],
  });
  const comments = useQuery({
    enabled: reading,
    queryFn: () => api.listComments(issueKey),
    queryKey: ["comments", issueKey],
  });
  const messages = useQuery({
    enabled: reading,
    queryFn: () => api.listIssueMessages(issueKey),
    queryKey: ["issue-messages", issueKey],
  });
  // The history's own log, under its key (`IssueHistory.tsx`), which the history under this list
  // reads: watched here and never asked for, so a pinned line is drawn from the pages it holds.
  const log = useQuery<InfiniteData<DispatchEvent[]>>({
    enabled: false,
    queryKey: ["events", issueKey],
  });
  const events = log.data?.pages.flat() ?? [];
  const wordsOf = (kind: string, id: string): string | undefined => {
    if (kind === "ask") return asks.data?.find((ask) => ask.id === id)?.question;
    if (kind === "comment") return comments.data?.find((comment) => comment.id === id)?.body;
    if (kind !== "message") return undefined;
    return (messages.data ?? [])
      .flatMap((thread) => [thread.message, ...(thread.replies ?? [])])
      .find((message) => message.id === id)?.body;
  };
  // On a phone Details covers the page the link lands on.
  const leave = () => {
    if (phone) drawer?.setOpen(false);
  };
  const link = `dispatch-inline-link underline ${linkText}`;
  return (
    <section aria-label={CONTROL_LABELS.pinned} data-testid="issue-pins">
      <h2 className={`text-sm font-semibold ${pageInkText}`} {...pageChrome}>
        {CONTROL_LABELS.pinned}
      </h2>
      {pins.length + eventPins.length === 0 ? (
        <p className={`text-xs ${pageQuietText}`} {...pageChrome}>
          {CONTROL_LABELS.none}
        </p>
      ) : (
        <ul className="mt-1 flex min-w-0 flex-col gap-0.5">
          {pins.map((pin) => {
            const id = conversationItemId(pin.kind, pin.id);
            const words = wordsOf(pin.kind, pin.id);
            return (
              <li className="flex min-w-0 items-center gap-2 text-sm" data-pin={id} key={id}>
                <Link
                  className={`min-w-0 flex-1 truncate ${link}`}
                  onClick={leave}
                  to={
                    pin.kind === "handoff"
                      ? buildIssuePath({ key: issueKey, kind: "activity" })
                      : buildIssuePath({ id: pin.id, key: issueKey, kind: pin.kind })
                  }
                >
                  {words === undefined ? (
                    <span {...pageChrome}>{NODE_TYPE_NAMES[pin.kind]}</span>
                  ) : (
                    <span {...authorText}>{firstLineOf(words).line}</span>
                  )}
                </Link>
                <PinButton
                  label={CONTROL_LABELS.unpin}
                  onClick={() => writePin(queryClient, issueKey, { id, op: "unpin" })}
                  pinned
                  quiet
                />
              </li>
            );
          })}
          {eventPins.map((eventId) => {
            const id = eventItemId({ id: Number(eventId) });
            const event = events.find((one) => String(one.id) === eventId);
            return (
              <li className="flex min-w-0 items-center gap-2 text-sm" data-pin={id} key={id}>
                {event === undefined ? null : (
                  <span className={`min-w-0 flex-1 truncate ${pageMutedText}`} {...authorText}>
                    <AgentParts parts={[{ actor: event.actor }, " ", ...eventDescription(event)]} />
                  </span>
                )}
                <Link
                  className={`${event === undefined ? "min-w-0 flex-1" : "shrink-0"} ${link}`}
                  onClick={leave}
                  to={buildIssuePath({ key: issueKey, kind: "activity" })}
                  {...pageChrome}
                >
                  {CONTROL_LABELS.history}
                </Link>
                <PinButton
                  label={CONTROL_LABELS.unpin}
                  onClick={() => writePin(queryClient, issueKey, { id, op: "unpin" })}
                  pinned
                  quiet
                />
              </li>
            );
          })}
        </ul>
      )}
    </section>
  );
}
