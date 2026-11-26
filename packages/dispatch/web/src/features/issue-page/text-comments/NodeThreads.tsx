import { type ReactNode, type RefObject, useEffect, useLayoutEffect, useRef } from "react";

import type { IssueDetails } from "../../../api/types";
import { useMediaQuery } from "../../shell/useDialog";
import { CONTENTS_PANE_QUERY } from "../contents-frame";
import type { TreeNode } from "../issue-tree";
import { NoTooltips } from "../NoTooltips";
import { authorText } from "../page-structure";
import { MARGIN_WORDS, type Preset } from "../page-words";
import { moveToRow } from "../useTreeKeymap";
import { SelectionComposer, wordsKeyOf } from "./SelectionComposer";
import {
  cardIn,
  TEXT_COMPOSER_ATTRIBUTE,
  type TextComposer,
  type TextComposerRequest,
  takeFocus,
  useSheet,
  useTextComposer,
} from "./TextMargin";
import { TextThreadCard, useThreadCardHost } from "./TextThreadCard";
import type { TextThread } from "./text-threads";
import { useStageIssue, useTextThreads, type ViewThreads } from "./useTextThreads";

/** The attribute of the sheet that holds one node's threads below 1024 px. */
export const TEXT_THREADS_ATTRIBUTE = "data-text-threads";

/** What `f` steps through in a row besides its chips (`useTreeKeymap.ts`' `rowStops`): its links,
 *  then each button out of the tab order. */
const ROW_STOPS = "a[href], button";

/**
 * Takes the sheet's links and buttons out of the tab order while `out` holds, those it is drawn
 * with and those that arrive after, so `f` reaches them as the next stops of the row the sheet
 * stands in (`rowStops` walks a row's links, then its buttons out of the tab order), as it
 * reaches the row's own: the tree is one tab stop (R9). A reply's field stays in the tab order,
 * as the field of a row's open change form does, since no key of the tree reaches a field. A view
 * that no tree's keys reach passes `out` false and leaves them where they are.
 */
function useStopsOutOfTabOrder(box: RefObject<HTMLElement | null>, out: boolean): void {
  useLayoutEffect(() => {
    const element = box.current;
    if (!out || element === null) return;
    const untab = (): void => {
      for (const stop of element.querySelectorAll<HTMLElement>(ROW_STOPS)) {
        if (stop.tabIndex !== -1) stop.tabIndex = -1;
      }
    };
    untab();
    const arrivals = new MutationObserver(untab);
    arrivals.observe(element, { attributeFilter: ["href"], childList: true, subtree: true });
    return () => arrivals.disconnect();
  }, [box, out]);
}

/**
 * The sheet of one node's threads below 1024 px (R3): a `div` with `role="group"` named by the
 * margin's words (`page-words-margin.ts`), as wide as the row, each thread drawn as the column
 * draws it (`TextThreadCard`), in the order of the threads' words, and the composer of R5 where
 * it is open on this node. A row site mounts it inside the node's row after its text, facts and
 * acts, before its children's rows, and outside the element that names the row, so the row's
 * name leaves it out; the row's fold folds its children's rows and never the sheet. From 1024 px
 * the column draws the threads instead, and the sheet draws nothing.
 *
 * `parent` is the tree id of the node the row is drawn under: a node drawn under several parents
 * - a check or a plan task under each requirement it serves - has its threads under its stored
 * parent's row alone, as in the column (R2). `tabStops` is the row's: in a tree whose keys reach a
 * row's controls the sheet's links and buttons are out of the tab order, and `f` reaches them.
 * Each row site mounts it at its part's gate. It reads the node's id alone, so a row that draws a
 * node it holds no tree node of - the Spec view's cell of a decision - hands it that id.
 */
export function NodeThreads({
  node,
  parent,
  tabStops = false,
}: {
  readonly node: Pick<TreeNode, "id">;
  readonly parent?: string;
  readonly tabStops?: boolean;
}): ReactNode {
  const wide = useMediaQuery(CONTENTS_PANE_QUERY);
  if (wide) return null;
  return (
    <NarrowSheet node={node} tabStops={tabStops} {...(parent === undefined ? {} : { parent })} />
  );
}

/** The sheet below 1024 px, once the page's read of the issue the router is at holds it
 *  (`useStageIssue`): only a width the sheet is drawn at watches the address and the issue's
 *  read. It sends no read of the issue: the page's root holds that read before the stage draws a
 *  row, so a view drawn alone, as its own tests draw it, sends no request for its sheets. */
function NarrowSheet({
  node,
  parent,
  tabStops,
}: {
  readonly node: Pick<TreeNode, "id">;
  readonly parent?: string;
  readonly tabStops: boolean;
}): ReactNode {
  const stage = useStageIssue(false);
  if (stage === undefined) return null;
  return (
    <Sheet
      issue={stage.issue}
      node={node}
      preset={stage.preset}
      tabStops={tabStops}
      {...(parent === undefined ? {} : { parent })}
    />
  );
}

function Sheet({
  issue,
  node,
  parent,
  preset,
  tabStops,
}: {
  readonly issue: IssueDetails;
  readonly node: Pick<TreeNode, "id">;
  readonly parent?: string;
  readonly preset: Preset;
  readonly tabStops: boolean;
}): ReactNode {
  const view = useTextThreads(issue, preset);
  const composer = useTextComposer();
  const held = view.threads.get(node.id);
  const here = parent === undefined || held === undefined || held.parent === parent;
  // The sheet is the node's place for a composer below 1024 px whether it draws a thread or not,
  // as long as the row it stands in is the node's own.
  useSheet(here ? node.id : undefined);
  const threads = here ? (held?.threads ?? []) : [];
  const request = here && composer.request?.nodeId === node.id ? composer.request : undefined;
  if (threads.length === 0 && request === undefined) return null;
  return (
    <SheetBody
      composer={composer}
      issue={issue}
      request={request}
      tabStops={tabStops}
      threads={threads}
      view={view}
    />
  );
}

function SheetBody({
  composer,
  issue,
  request,
  tabStops,
  threads,
  view,
}: {
  readonly composer: TextComposer;
  readonly issue: IssueDetails;
  readonly request: TextComposerRequest | undefined;
  readonly tabStops: boolean;
  readonly threads: readonly TextThread[];
  readonly view: ViewThreads;
}): ReactNode {
  const sheet = useRef<HTMLDivElement>(null);
  const host = useThreadCardHost(issue);
  useStopsOutOfTabOrder(sheet, !tabStops);

  // The root of the thread a send of this part wrote takes the focus once the re-read draws it in
  // the sheet (R5).
  const sent = composer.landed;
  useEffect(() => {
    if (sent === undefined || !("root" in sent)) return;
    const thread = threads.find((one) => one.thread.root.comment.id === sent.root);
    const card = thread === undefined ? null : cardIn(sheet.current, thread.thread.key);
    if (card === null) return;
    takeFocus(card);
    composer.settle();
  });

  // Escape in the composer, and its own Close, close it with its draft kept and give the focus to
  // the row the sheet stands in (R5's table).
  const leaveComposer = () => {
    const row = sheet.current?.closest<HTMLElement>("[data-node-id]") ?? null;
    composer.close();
    if (row !== null) moveToRow(row);
  };

  return (
    // biome-ignore lint/a11y/useSemanticElements: the sheet groups a row's threads, which are no form fields for a fieldset to group.
    <div
      aria-label={MARGIN_WORDS.region}
      className="col-span-full mt-2 flex w-full min-w-0 flex-col gap-2"
      ref={sheet}
      role="group"
      {...{ [TEXT_THREADS_ATTRIBUTE]: "" }}
    >
      <NoTooltips>
        {/* The threads' stored text is the authors' (R4), as the column marks its cards. */}
        <div className="contents" {...authorText}>
          {threads.map((thread) => (
            <TextThreadCard host={host} key={thread.thread.key} thread={thread} />
          ))}
          {request === undefined ? null : (
            <div {...{ [TEXT_COMPOSER_ATTRIBUTE]: "" }}>
              <SelectionComposer
                issue={issue}
                key={wordsKeyOf(request)}
                onLand={composer.land}
                onLeave={leaveComposer}
                view={view}
                words={request}
              />
            </div>
          )}
        </div>
      </NoTooltips>
    </div>
  );
}
