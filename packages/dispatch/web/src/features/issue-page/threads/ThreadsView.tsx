import { useQuery } from "@tanstack/react-query";
import { type ReactNode, useState } from "react";
import { useLocation, useNavigate } from "react-router-dom";

import { primarySpec } from "../../../api/issue-cache";
import { myRepliesOwnerQuery } from "../../../api/queries";
import type { Message } from "../../../api/types";
import { focusVisibleRing, pageLineBorder, pageMutedText } from "../../../theme/classes";
import { EVENT_FRESH } from "../../refs/reference-target";
import { buildIssuePath } from "../../refs/routes";
import { useMediaQuery } from "../../shell/useDialog";
import { issueAddress } from "../issue-address";
import { pageChrome } from "../page-structure";
import { EMPTY_STATE_LINES, PRESET_NAMES, THREADS_WORDS } from "../page-words";
import type { SlotProps } from "../slot-props";
import { NewThread } from "./NewThread";
import { ThreadList, useThreadLists } from "./ThreadList";
import { OpenedRecord } from "./ThreadRecord";
import { threadRecords } from "./thread-records";

/** From this width up the list and the main column stand side by side (R2): 1024 px. */
export const SIDE_BY_SIDE_QUERY = "(min-width: 64rem)";

/**
 * The Threads view (dispatch://INFRA-5704/spec#b-r-view), which the stage draws in the outline's
 * place (`slots/view-threads.tsx`). It is one region named by its control, so a reader finds it as
 * a landmark, and it holds two columns: the list (`data-thread-list`, `ThreadList.tsx`), and the
 * main column (`data-thread-main`), which draws the opened record, New thread, or one line that
 * says how to open or start a thread (R2).
 *
 * From 1024 px up the list stands in a column at the left and the main column beside it. Below
 * 1024 px the list fills the column, and an opened record or New thread stands in its place, with
 * the control that returns to the list above it. The width decides which is drawn, so a record
 * opened at 1440 px stands alone once the reader narrows the window below 1024 px, and beside the
 * list again once they widen it. The list's state is the view's (`useThreadLists`), so the list
 * drawn again holds the same choice and the rows it had shown.
 *
 * It draws no outline, no children's section, none of the issue's three sentences and no version
 * control (R1): only the two columns, in the outline's measure.
 */
export function ThreadsView({ issue, route }: SlotProps): ReactNode {
  const { hash, search } = useLocation();
  const navigate = useNavigate();
  const { record } = issueAddress(route, search, hash, primarySpec(issue)?.slug);
  const wide = useMediaQuery(SIDE_BY_SIDE_QUERY);
  const lists = useThreadLists(issue);
  const [newThread, setNewThread] = useState(false);
  // A record the reader opens takes the main column from New thread.
  const [drawnRecord, setDrawnRecord] = useState(record);
  if (drawnRecord !== record) {
    setDrawnRecord(record);
    if (record !== undefined) setNewThread(false);
  }
  // The thread New thread just sent, drawn from Dispatch's answer at its address until a page
  // of the list holds it, so it opens with no lookup (R10, R12).
  const [sent, setSent] = useState<Message>();
  const listPath = buildIssuePath({ key: issue.key, kind: "threads" });
  const opened =
    record === undefined
      ? undefined
      : (lists.records.get(record) ??
        (sent?.id === record ? threadRecords({ message: sent, replies: null })[0] : undefined));
  const open = record !== undefined || newThread;
  // The read mark of an opened record reads the reader's unread threads of the issue once
  // (`useMarkThreadRead`, R9, R15). From the first record opened the view watches that read
  // itself, as long as Threads stays open, so each mark that lands, and each event that moves
  // it, reads it again at once, while a record is open or not, and the next record opened
  // finds it fresh and sends no read of its own.
  const [marking, setMarking] = useState(false);
  if (opened !== undefined && !marking) setMarking(true);
  useQuery({ ...myRepliesOwnerQuery({ issue: issue.key }), ...EVENT_FRESH, enabled: marking });

  const main = (
    <div className="flex min-w-0 flex-1 flex-col gap-3" data-thread-main="">
      {wide ? null : (
        <button
          className={`self-start rounded-lg border px-3 py-1 text-sm font-medium ${focusVisibleRing} ${pageLineBorder} ${pageMutedText}`}
          onClick={() => {
            setNewThread(false);
            if (record !== undefined) navigate(listPath);
          }}
          type="button"
          {...pageChrome}
        >
          {THREADS_WORDS.back_to_list}
        </button>
      )}
      {record !== undefined ? (
        opened === undefined ? null : (
          // One record at a time: another record opened starts with no box of the last one open.
          <OpenedRecord
            issue={issue}
            key={opened.id}
            record={opened}
            thread={[...lists.records.values()].filter((one) => one.thread === opened.thread)}
          />
        )
      ) : newThread ? (
        <NewThread
          issueKey={issue.key}
          onSent={(message) => {
            setSent(message);
            navigate(buildIssuePath({ key: issue.key, kind: "threads", record: message.id }));
          }}
        />
      ) : (
        <p className={`text-sm ${pageMutedText}`} {...pageChrome}>
          {EMPTY_STATE_LINES.threads_none_open}
        </p>
      )}
    </div>
  );
  return (
    <section
      aria-label={PRESET_NAMES.threads}
      className="mx-auto flex w-full max-w-[1000px] min-w-0 gap-6 pb-6"
    >
      {wide || !open ? (
        <div className={wide ? "w-[340px] min-w-0 shrink-0" : "w-full min-w-0"}>
          <ThreadList
            issue={issue}
            lists={lists}
            onNewThread={() => {
              setNewThread(true);
              if (record !== undefined) navigate(listPath);
            }}
          />
        </div>
      ) : null}
      {wide || open ? main : null}
    </section>
  );
}
