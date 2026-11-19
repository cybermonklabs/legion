import { useQuery } from "@tanstack/react-query";
import { type ReactNode, useMemo, useState } from "react";
import { useLocation, useNavigate } from "react-router-dom";

import { primarySpec } from "../../api/issue-cache";
import { issueResultsQuery, whoAmIQuery } from "../../api/queries";
import type { IssueDetails } from "../../api/types";
import { pageLineBorder, pagePaperBg } from "../../theme/classes";
import { useKeymap } from "../shell/keymap";
import { useMediaQuery } from "../shell/useDialog";
import { DecisionsFilterBody } from "./decisions-filter/DecisionsFilterBody";
import { IssueComposer } from "./IssueComposer";
import {
  type ContentsEntry,
  ContentsList,
  contentsOf,
  IssueContentsPane,
  OutlineSheet,
} from "./IssueContents";
import { IssuePresets } from "./IssuePresets";
import { IssueReadMark } from "./IssueReadMark";
import { IssueThreeThings, specSummary } from "./IssueThreeThings";
import { IssueTree, type OutlineRequest } from "./IssueTree";
import type { AddressTarget, IssueAddress } from "./issue-address";
import { pathToTarget, presetPath } from "./issue-address";
import { artifactsBySlug } from "./issue-artifacts";
import { EMPTY_STATE_LINES, type Preset } from "./page-words";
import { PlanFilterBody } from "./plan-filter/PlanFilterBody";
import { filterSignals, isFilter, presetView } from "./presets";
import { SpecFilterBody } from "./spec-filter/SpecFilterBody";
import { useIssueTree } from "./useIssueTree";
import { VerificationFilterBody } from "./verification-filter/VerificationFilterBody";
import { waitingPressOf } from "./waiting";

/** The width the Contents pane stands beside the outline from (R8, R14): 1024 px, in `rem` so
 *  it switches with Tailwind's `lg:` at any default font size. */
export const CONTENTS_PANE_QUERY = "(min-width: 64rem)";

/** The page's inset from the edges of its column, the prototype's: 28 px from the phone's width
 *  up, 14 px below it. The Contents pane takes none, and stands against the rail. */
export const PAGE_INSET = "px-3.5 md:px-7";

/**
 * The issue's body on every address: the filter row and the one tree the page and its filters
 * draw (dispatch://INFRA-1196 R1, R2, R3, R5, R10) - or, where the address opens a document,
 * that document in place of the tree.
 *
 * It is one component because every view reads the same tree: the row's signals count what
 * each view holds, and each view draws what its query left. Building that tree is this page's
 * one model read (`useIssueTree.ts`), so it happens here, once, below the guards `IssuePage`
 * draws for an issue it could not load.
 *
 * With no filter on the outline is the conversation (`IssueTree`), and from 1024 px up the
 * Contents pane stands against the rail with the three sentences and the list of questions and
 * the reader's own threads (R8, R14); below 1024 px both are in the Outline sheet behind the
 * control beside the filters, as the 390 px pictures draw it. Under a filter the pane is not
 * drawn and the filter's own body takes the column (ruling 5 of build-rulings-md): the Spec
 * filter's decided-by columns, the Verification filter's table, the Plan filter's chart and its
 * steps, and the Decisions filter's four parts, each the approved picture of that view.
 *
 * Escape takes the steps of the key table in its order: a caret is left first (the outline's
 * and a document's own), then an opened document is closed (`OpenedDocument.tsx`), then the
 * filter by words is cleared, then every filter is turned off, and then the outline is left
 * (`useTreeKeymap.ts`). The two middle steps are this component's.
 */
export function IssueBody({
  address,
  below,
  openedDocument,
  isClosed,
  issue,
  onEditing,
  onLeave,
  toolbar,
}: {
  address: IssueAddress;
  /** What the page draws under the outline, in the reading column (`IssuePresetPanels`). */
  below?: ReactNode;
  /** The document the address opens, drawn in place of the tree (R3), with its version control
   *  beside the filters; undefined where the outline is drawn. */
  openedDocument?: { readonly panel: ReactNode; readonly toolbar: ReactNode };
  /** Whether the issue is closed, which the node menu's upload reads. */
  isClosed: boolean;
  issue: IssueDetails;
  /** The document a row of the tree is being edited in, so the page draws that document's one
   *  editor for the row to take; `undefined` while the reader is editing nothing. */
  onEditing: (artifactId: string | undefined) => void;
  /** Called before a preset's own control leaves this address, so the page can record where
   *  the reader was before the move. */
  onLeave: () => void;
  /** The version control beside the filters while the outline is drawn. */
  toolbar?: ReactNode;
}): ReactNode {
  const navigate = useNavigate();
  const { hash, pathname, search, state: locationState } = useLocation();
  const { blocksRead, root } = useIssueTree(issue);
  // The issue's checks with the results recorded against them, under the key `useIssueTree.ts`
  // already reads them with, so the Verification filter's body costs no request of its own
  // (R20): it is what holds a result's commit, its verdict and its withdrawal, which the tree
  // does not carry (`verification-filter/statement-state.ts`).
  const results = useQuery(issueResultsQuery(issue.key)).data;
  const checks = useMemo(() => results?.checks ?? [], [results]);
  // Who the reader is, which the Contents list's entries are read for: a thread is an entry
  // where the reader began it (`contentsOf`).
  const viewer = useQuery(whoAmIQuery()).data?.login;
  // What each control of the filter row draws after its name: a filter's count of its main kind
  // and what of it asks something of the reader, and for Conversation how many entries the
  // Contents list holds, as the prototype's signal counts its branches. Each follows `?q=`.
  const signals = useMemo(() => {
    const entries = (list: readonly ContentsEntry[]): number =>
      list.reduce((count, entry) => count + 1 + entries(entry.children), 0);
    const conversation = presetView(root, "conversation", address.filter).root;
    return {
      ...filterSignals(root, address.filter),
      conversation: { count: entries(contentsOf(conversation, viewer)) },
    };
  }, [address.filter, root, viewer]);
  // The address's own node is resolved against the whole tree and then kept by the preset's
  // query, so an address that names a node the preset would not have matched still draws it.
  // The target is read by what it says rather than by the value `issueAddress` built, which is
  // a new one on every render: the tree is walked and pruned again only when the address or
  // the tree itself changes, not on every commit the page makes.
  const targetKind = address.target?.kind;
  const targetId = address.target?.id;
  // Which document a block target belongs to, where its address names one: a block id alone
  // names a block of any document of the issue (`pathToTarget`).
  const targetDocument = address.target?.kind === "block" ? address.target.document : undefined;
  const target = useMemo<AddressTarget | undefined>(() => {
    if (targetKind === undefined || targetId === undefined) return undefined;
    if (targetKind === "block") return { document: targetDocument, id: targetId, kind: "block" };
    // These fields are the whole of a target, and this is the one place that says so, so a
    // target holds its value across a render rather than being built afresh.
    return { id: targetId, kind: targetKind } as AddressTarget;
  }, [targetDocument, targetId, targetKind]);
  const reveal = useMemo(() => pathToTarget(root, target), [root, target]);
  const named = useMemo(() => {
    const node = reveal?.at(-1);
    return new Set(node === undefined ? [] : [node.id]);
  }, [reveal]);
  const specSlug = primarySpec(issue)?.slug ?? "";
  // The spec's summary callout is the head's three sentences and no row of the outline (R8).
  const memo = useMemo(() => specSummary(root, specSlug)?.id, [root, specSlug]);
  const view = useMemo(
    () => presetView(root, address.preset, address.filter, named, memo),
    [address.filter, address.preset, memo, named, root]
  );
  const artifacts = useMemo(() => artifactsBySlug(issue), [issue]);
  const drawsDocument = openedDocument !== undefined;
  const shownToolbar = openedDocument === undefined ? toolbar : openedDocument.toolbar;
  // Where the three sentences and the list are drawn: in the Contents pane from 1024 px up, and
  // below that the sentences over the outline and the list behind the Outline control (R8).
  const paned = useMediaQuery(CONTENTS_PANE_QUERY);
  // What the list last asked of the outline, and whether the outline draws one branch at a
  // time: the pane and the sheet share them, so a width that swaps one for the other keeps both.
  const [request, setRequest] = useState<OutlineRequest | undefined>(undefined);
  const [focusBranch, setFocusBranch] = useState(false);
  const ask = (id: string | undefined, branch: boolean) =>
    setRequest((held) => ({ focusBranch: branch, id, seq: (held?.seq ?? 0) + 1 }));
  // What the filter by words holds is `?q=`, which the box writes in place, keeping the rest
  // of the address.
  const setWords = (next: string) => {
    const query = new URLSearchParams(search);
    if (next === "") query.delete("q");
    else query.set("q", next);
    const rest = query.toString();
    navigate(`${pathname}${rest === "" ? "" : `?${rest}`}${hash}`, { replace: true });
  };
  // Whether the page's own Escape is the next step: no document is open, whose close comes
  // first, no menu is open, whose press it is, and the focus is in no control inside a row of
  // the outline, which Escape gives the row back from first (`useTreeKeymap.ts`).
  const escapesHere = () => {
    if (drawsDocument || document.querySelector('[role="menu"]') !== null) return false;
    const active = document.activeElement;
    const row = active?.closest("[data-node-row]");
    return row === null || row === undefined || row === active;
  };
  const filterOn = isFilter(address.preset);
  useKeymap("issue", [
    {
      // The box the filter by words is typed in: Escape empties it, and leaves it once it is
      // empty, as Escape leaves a caret first.
      id: "words-leave",
      inEditable: true,
      keys: "Escape",
      label: "Clear the filter by words, then leave it",
      palette: false,
      run: () => {
        if (address.filter !== "") setWords("");
        else (document.activeElement as HTMLElement | null)?.blur();
      },
      when: () => document.activeElement?.matches('[data-testid="issue-filter"]') === true,
    },
    {
      id: "words-clear",
      keys: "Escape",
      label: "Clear the filter by words",
      palette: false,
      run: () => setWords(""),
      when: () => address.filter !== "" && escapesHere(),
    },
    {
      // Every filter off, which is Conversation's address. A reader on a row of the filter
      // stays on that node in the outline it opens on, so the next Escape leaves the outline.
      id: "filters-off",
      keys: "Escape",
      label: "Turn every filter off",
      palette: false,
      run: () => {
        const on = document.activeElement?.closest<HTMLElement>("[data-node-row]")?.dataset.nodeId;
        onLeave();
        navigate(presetPath(issue.key, "conversation"));
        if (on !== undefined) ask(on, focusBranch);
      },
      when: () => filterOn && escapesHere(),
    },
  ]);
  // The header's count of what waits on the reader turns the Decisions filter on and leaves its
  // press in the history entry (`IssuePropertiesRow.tsx`): the Decisions filter's body takes the
  // press, in its own order, once per press (R13, dispatch://INFRA-3678).
  const waitingPress = waitingPressOf(locationState);
  const list = (close?: () => void) =>
    drawsDocument ? null : (
      <ContentsList
        focusBranch={focusBranch}
        onFocusBranch={(current) => {
          setFocusBranch(!focusBranch);
          ask(current, !focusBranch);
        }}
        onGo={(id) => {
          close?.();
          ask(id, focusBranch);
        }}
        root={view.root}
      />
    );
  // The three sentences, with no filter on: in the Contents pane from 1024 px up, and in the
  // Outline sheet below it (R8, ruling 5). The Spec filter draws them as its first group.
  const head = (
    <IssueThreeThings
      look={paned ? "pane" : "sheet"}
      reading={!blocksRead}
      summary={specSummary(root, specSlug)}
    />
  );
  // What the filter by words leaves of the view: where it holds words and leaves nothing, the
  // view says so in its own line rather than that the issue holds nothing of its kind.
  const emptyLine =
    address.filter === "" ? EMPTY_STATE_LINES[address.preset] : EMPTY_STATE_LINES.no_match;
  const holdsEscape = filterOn || address.filter !== "";
  const body = (() => {
    switch (address.preset) {
      case "spec":
        return (
          <SpecFilterBody
            context={view.context}
            emptyLine={emptyLine}
            holdsEscape={holdsEscape}
            issue={issue}
            reader={viewer}
            reading={!blocksRead}
            request={request}
            reveal={reveal?.at(-1)?.id}
            root={root}
            view={view.root}
          />
        );
      case "verification":
        return (
          <VerificationFilterBody
            checks={checks}
            context={view.context}
            emptyLine={emptyLine}
            view={view.root}
          />
        );
      case "decisions":
        return (
          <DecisionsFilterBody
            artifacts={artifacts}
            emptyLine={emptyLine}
            holdsEscape={holdsEscape}
            isClosed={isClosed}
            issue={issue}
            reader={viewer}
            request={request}
            reveal={reveal?.at(-1)?.id}
            root={root}
            view={view.root}
            waitingPress={waitingPress}
          />
        );
      default: {
        const Outline = address.preset === "plan" ? PlanFilterBody : IssueTree;
        return (
          <Outline
            artifacts={artifacts}
            context={view.context}
            emptyLine={emptyLine}
            filtering={view.filtering}
            focusReveal
            holdsEscape={holdsEscape}
            isClosed={isClosed}
            onEditing={onEditing}
            request={request}
            reveal={reveal}
            root={view.root}
            rootRow={view.rootRow}
          />
        );
      }
    }
  })();

  return (
    <>
      {/* The filter row's rule runs the column's whole width, as the prototype's `#tabs` does,
          and its controls keep the page's inset. */}
      <div
        className={`border-b ${PAGE_INSET} ${pageLineBorder} ${pagePaperBg} max-[760px]:sticky max-[760px]:top-0 max-[760px]:z-[25]`}
      >
        <IssuePresets
          active={address.preset}
          artifacts={issue.artifacts ?? []}
          filter={address.filter}
          isClosed={isClosed}
          issueKey={issue.key}
          nodeMenu={address.nodeMenu}
          onFilterChange={setWords}
          onLeave={onLeave}
          // Below 1024 px the Outline sheet holds what the pane holds with no filter on - the
          // three sentences and the list - and at 760 px and below the row's own small controls
          // as well (`tools`: the node menu, the filter by words and the version control), so
          // the row is one line at every width (ruling 5).
          outline={
            paned
              ? undefined
              : (tools) =>
                  drawsDocument || filterOn ? (
                    tools === undefined ? null : (
                      <OutlineSheet tools={tools} />
                    )
                  ) : (
                    <OutlineSheet brief={head} list={list} tools={tools} />
                  )
          }
          signals={signals}
          toolbar={shownToolbar}
        />
      </div>
      <div className="flex min-w-0 items-start">
        {paned && !filterOn ? <IssueContentsPane brief={head} list={list()} /> : null}
        {/* At least the screen's height, so the box the reader writes in stands at the foot of
            the screen however short the outline is (ruling 5: "fixed to the foot"). */}
        <div className={`flex min-h-dvh min-w-0 flex-1 flex-col pt-2.5 ${PAGE_INSET}`}>
          {drawsDocument ? (
            openedDocument?.panel
          ) : (
            // The view's own measure, in the middle of the column: the outline's 1000 px (the
            // prototype's `#pane`), and under a filter the width its picture draws its body at
            // (`.spec`, `.ver`, `.plan`, `.decs`). On a phone 4 px from the left edge and 10 px
            // from the right, as the prototype's `#reading` is there, so the bullets keep the
            // picture's place.
            <div
              className={`mx-auto flex w-full min-w-0 flex-col max-[760px]:-mr-1 max-[760px]:-ml-2.5 max-[760px]:w-auto ${MEASURES[address.preset]}`}
            >
              {body}
              {/* The end of the conversation, which reads the issue once the reader has seen
                  it (`IssueReadMark.tsx`). */}
              {address.preset === "conversation" ? <IssueReadMark issueKey={issue.key} /> : null}
            </div>
          )}
          {below}
          {/* The one place to write, fixed to the foot of the screen (R16, ruling 5), in the
              outline's measure. It stays mounted while a filter or an opened document hides it,
              so a draft outlives the reader's way through the page. */}
          <div className="sticky bottom-0 z-[15] mx-auto mt-auto w-full max-w-[1000px] min-w-0 max-[760px]:-mr-1 max-[760px]:-ml-2.5 max-[760px]:w-auto">
            <IssueComposer
              documentOpen={drawsDocument}
              filterOn={filterOn}
              isClosed={isClosed}
              issueKey={issue.key}
            />
          </div>
        </div>
      </div>
    </>
  );
}

/** How wide each view draws its body: the outline's measure with no filter on, and under a
 *  filter the width the approved picture draws that filter's body at (ruling 5: "the filter's
 *  body has the whole width"). */
const MEASURES: Readonly<Record<Preset, string>> = {
  conversation: "max-w-[1000px]",
  decisions: "max-w-[960px]",
  plan: "max-w-[1300px]",
  spec: "max-w-[1080px]",
  verification: "max-w-[1180px]",
};
