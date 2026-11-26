import { type ReactNode, useState } from "react";
import { useLocation, useNavigate } from "react-router-dom";

import { primarySpec } from "../../../api/issue-cache";
import {
  focusVisibleRing,
  pageInkText,
  pageLineBorder,
  pageMutedText,
} from "../../../theme/classes";
import { buildIssuePath } from "../../refs/routes";
import { useMediaQuery } from "../../shell/useDialog";
import { issueAddress } from "../issue-address";
import { pageChrome } from "../page-structure";
import { EMPTY_STATE_LINES, PRESET_NAMES, THREADS_WORDS } from "../page-words";
import type { SlotProps } from "../slot-props";
import { ThreadList, useRowNames, useThreadLists } from "./ThreadList";
import { KindWords } from "./ThreadRow";
import type { ThreadRecord } from "./thread-records";
import { threadRow } from "./thread-row";

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
  const listPath = buildIssuePath({ key: issue.key, kind: "threads" });
  const opened = record === undefined ? undefined : lists.records.get(record);
  const open = record !== undefined || newThread;

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
          <OpenedRecord project={issue.project} record={opened} />
        )
      ) : newThread ? null : (
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

/**
 * The record the address opens, in the main column (R2): it carries the record's id
 * (`data-thread-record`) and names its kind, as its row does. Its entries, its boxes and its read
 * mark stand under that name (R9).
 */
function OpenedRecord({
  project,
  record,
}: {
  readonly project: string;
  readonly record: ThreadRecord;
}): ReactNode {
  const names = useRowNames(project);
  return (
    <article className="min-w-0" data-thread-record={record.id}>
      <h2 className={`text-[15px] font-semibold ${pageInkText}`} {...pageChrome}>
        <KindWords text={threadRow(record, names)} />
      </h2>
    </article>
  );
}
