import { useQuery } from "@tanstack/react-query";
import { type ReactNode, useLayoutEffect, useRef, useState } from "react";
import { useLocation, useNavigate } from "react-router-dom";

import { primarySpec } from "../../../api/issue-cache";
import { myRepliesOwnerQuery } from "../../../api/queries";
import type { Message } from "../../../api/types";
import { focusVisibleRing, pageLineBorder, pageMutedText } from "../../../theme/classes";
import { EVENT_FRESH } from "../../refs/reference-target";
import { buildIssuePath } from "../../refs/routes";
import { useKeymap } from "../../shell/keymap";
import { useMediaQuery } from "../../shell/useDialog";
import { issueAddress } from "../issue-address";
import { pageChrome } from "../page-structure";
import { ANSWER_LINES, EMPTY_STATE_LINES, PRESET_NAMES, THREADS_WORDS } from "../page-words";
import type { SlotProps } from "../slot-props";
import { FailedReadLine } from "../slots/failed-read-line";
import { NewThread } from "./NewThread";
import { ThreadList, useThreadLists } from "./ThreadList";
import { OpenedRecord } from "./ThreadRecord";
import { focusedEntry, recordChoice } from "./thread-address";
import { useThreadLookup } from "./thread-lookup";
import { type ThreadRecord, threadRecords } from "./thread-records";

/** From this width up the list and the main column stand side by side (R2): 1024 px. */
export const SIDE_BY_SIDE_QUERY = "(min-width: 64rem)";

/** A record the main column draws, with every record the view holds of its thread. */
interface DrawnRecord {
  readonly record: ThreadRecord;
  readonly thread: readonly ThreadRecord[];
}

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
 * `/threads/<id>` opens the record `<id>` (dispatch://INFRA-5704/spec#b-r-addresses, R12). A row
 * opens it at that address, which goes on the browser's history, so Back returns to the list; with
 * no caret in a box, Escape does too. Where no page the view holds has the record, once All's first
 * pages have answered, R12's lookups read it (`useThreadLookup`), and an id that names no record of
 * the issue draws the page's line for an address it cannot find. A record the address opens, and
 * no row of the choice that is on draws, opens under its own choice (`recordChoice`): Agents for a
 * thread between agents, All otherwise.
 *
 * A write that lands while a record is open is drawn in it with no reload, from the pages read
 * again (R14). A record whose write moves it out of its choice stays open, and its row is drawn
 * where its choice now holds it.
 *
 * It draws no outline, no children's section, none of the issue's three sentences and no version
 * control (R1): only the two columns, in the outline's measure.
 */
export function ThreadsView({ issue, route }: SlotProps): ReactNode {
  const { hash, search, state } = useLocation();
  const navigate = useNavigate();
  const { record } = issueAddress(route, search, hash, primarySpec(issue)?.slug);
  const wide = useMediaQuery(SIDE_BY_SIDE_QUERY);
  const lists = useThreadLists(issue);
  const [newThread, setNewThread] = useState(false);
  // A record the reader opens takes the main column from New thread. Its choice is decided once
  // the view holds it (`placing`, below).
  const [drawnRecord, setDrawnRecord] = useState(record);
  const [placing, setPlacing] = useState(record);
  if (drawnRecord !== record) {
    setDrawnRecord(record);
    setPlacing(record);
    if (record !== undefined) setNewThread(false);
  }
  // The thread New thread just sent, drawn from Dispatch's answer at its address until a page
  // of the list holds it, so it opens with no lookup (R10, R12).
  const [sent, setSent] = useState<Message>();
  const listPath = buildIssuePath({ key: issue.key, kind: "threads" });
  const held =
    record === undefined
      ? undefined
      : (lists.records.get(record) ??
        (sent?.id === record ? threadRecords({ message: sent, replies: null })[0] : undefined));
  // While a route's pages are out, one of them may hold the record, so the lookups wait for it:
  // a write that moves the thread to another route leaves it on no page the view holds until
  // that route's read answers (R14).
  const lookup = useThreadLookup(
    issue.key,
    record,
    held !== undefined ? true : lists.settled && !lists.reading ? false : undefined
  );
  const found = held ?? (lookup.kind === "found" ? lookup.record : undefined);
  // The record as the main column drew it last. Through the moment such a move leaves it on no
  // page, the open record stays drawn as it was, with its boxes and the reader's caret, rather
  // than going and mounting anew once the route it joins answers.
  const lastDrawn = useRef<DrawnRecord | undefined>(undefined);
  const kept =
    found === undefined && lookup.kind === "reading" && lastDrawn.current?.record.id === record
      ? lastDrawn.current
      : undefined;
  const opened = found ?? kept?.record;
  const thread: readonly ThreadRecord[] =
    kept?.thread ??
    (opened === undefined
      ? []
      : held === undefined && lookup.kind === "found"
        ? lookup.thread
        : [...lists.records.values()].filter((one) => one.thread === opened.thread));
  useLayoutEffect(() => {
    lastDrawn.current = opened === undefined ? undefined : { record: opened, thread };
  });
  if (placing !== undefined && opened !== undefined) {
    setPlacing(undefined);
    // A row the reader pressed opens its record under the choice that drew it.
    const drawn = lists.view.rows?.some((one) => one.id === opened.id) === true;
    const under = recordChoice(opened, issue.project);
    if (!drawn && under !== lists.choice) lists.choose(under);
  }
  const open = record !== undefined || newThread;
  // The read mark of an opened record reads the reader's unread threads of the issue once
  // (`useMarkThreadRead`, R9, R15). From the first record opened the view watches that read
  // itself, as long as Threads stays open, so each mark that lands, and each event that moves
  // it, reads it again at once, while a record is open or not, and the next record opened
  // finds it fresh and sends no read of its own.
  const [marking, setMarking] = useState(false);
  if (opened !== undefined && !marking) setMarking(true);
  useQuery({ ...myRepliesOwnerQuery({ issue: issue.key }), ...EVENT_FRESH, enabled: marking });
  useKeymap("issue", [
    {
      // R12: Escape with no caret in a box closes the record and returns to the list. With the
      // caret in a box the record takes the caret out first (`OpenedRecord`), and a menu, a
      // dialog or Details takes the press before the page does.
      id: "threads-close-record",
      keys: "Escape",
      label: THREADS_WORDS.close_record,
      palette: false,
      run: () => navigate(listPath),
      when: () => record !== undefined,
    },
  ]);

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
        opened !== undefined ? (
          // One record at a time: another record opened starts with no box of the last one open.
          <OpenedRecord
            focus={focusedEntry(state)}
            issue={issue}
            key={opened.id}
            record={opened}
            thread={thread}
          />
        ) : lookup.kind === "missing" ? (
          <p className={`text-sm ${pageMutedText}`} {...pageChrome}>
            {ANSWER_LINES.threads_not_found}
          </p>
        ) : lookup.kind === "failed" ? (
          // A read Retry sends makes the lookup reading again, so the line is never drawn busy.
          <FailedReadLine
            message={ANSWER_LINES.threads_unreadable}
            onRetry={lookup.retry}
            retrying={false}
          />
        ) : null
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
