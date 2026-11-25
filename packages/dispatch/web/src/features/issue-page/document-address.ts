import { useQuery } from "@tanstack/react-query";
import { useEffect } from "react";
import { useLocation, useNavigate } from "react-router-dom";

import { artifactBlocksQuery } from "../../api/queries";
import type { Artifact, IssueDetails } from "../../api/types";
import { buildIssuePath, type IssueRoute } from "../refs/routes";
import type { ChipRecord } from "./chip-return";
import { DOCUMENT_OPEN_ATTRIBUTE, DOCUMENTS_LIST_ATTRIBUTE } from "./DocumentsList";
import { type IssueAddress, issueAddress } from "./issue-address";
import { NODE_TYPES } from "./page-words";
import { nodeRowSelectorOf, rowSelectorOf } from "./tree-focus";
import { useIssueDetail } from "./useIssueDetail";
import { treeDocumentIds } from "./useIssueTree";
import { abortOnReaderAct } from "./useNodeEditing";

/** The document an address opens in place of the outline: the route it is drawn at, and the
 *  artifact itself, undefined for a slug the issue does not hold. */
export interface OpenedAddress {
  readonly artifact: Artifact | undefined;
  readonly route: Extract<IssueRoute, { kind: "artifact" }>;
}

/** The location's `outline`, if it carries one: the address Documents' list or a document was
 *  opened from (`DocumentsList.tsx`' rows hand over the list's own). */
function outlineOf(state: unknown): string | undefined {
  if (typeof state !== "object" || state === null || !("outline" in state)) return undefined;
  return typeof state.outline === "string" ? state.outline : undefined;
}

/**
 * Whether the address opens its document in place of the outline (dispatch://INFRA-1196 R3,
 * R20). A document's own address opens the document, with one exception: an address naming a
 * block of it that the outline holds as a node - a typed block of a document the outline is
 * built from (`outlined`), which a citation of a check or a plan task names - is where that block
 * now stands, so it opens the outline at that node. A block no node holds opens the document at
 * the block: a paragraph of the document's own text, and every block of a document the outline is
 * not built from, a typed one included (decision 449: a task of a document of tasks that is no
 * plan). Which blocks of an outlined document are typed is the blocks read the outline is built
 * from, so this asks it under the same key and sends nothing of its own, and none for a document
 * the outline is not built from; until it answers, the address draws the outline it most likely
 * names.
 */
function useOpensDocument(
  address: IssueAddress,
  artifact: Artifact | undefined,
  outlined: boolean
): boolean {
  const block = address.target?.kind === "block" ? address.target.id : undefined;
  const asksBlocks = outlined && address.document !== undefined && block !== undefined;
  const blocks = useQuery({
    ...artifactBlocksQuery(artifact?.id ?? ""),
    enabled: asksBlocks && artifact?.kind === "doc",
  });
  if (address.document === undefined) return false;
  if (!asksBlocks || artifact?.kind !== "doc") return true;
  if (blocks.data === undefined) return blocks.isError;
  const types: readonly string[] = NODE_TYPES;
  return !blocks.data.some((one) => one.id === block && types.includes(one.type));
}

/**
 * The document the address opens in place of the outline (part N's R1, R2, R6), or undefined
 * where it opens none. Only an address with no view on opens one: a document's own address,
 * `/artifacts/<slug>` or `/artifact/<slug>`, with what it carries - `?mark=`, a pinned version, a
 * `#b-` of a block no node holds (`useOpensDocument`). An address that keeps a view's preset draws
 * that view, a block of the spec, `?mark=` or a pinned version beside `/spec` among them
 * (`issueAddress`, part I's rule in `slots/stage.tsx`), and so does the outline's own address. The
 * stage draws the document (`slots/stage.tsx`) and the view row's version control reads it
 * (`view-row-tools.tsx`), each through this one rule.
 */
export function useOpenedDocument(
  issue: IssueDetails,
  route: IssueRoute
): OpenedAddress | undefined {
  const { hash, search } = useLocation();
  const { artifactRoute, primaryArtifact, selectedArtifact } = useIssueDetail(issue, route);
  const address = issueAddress(route, search, hash, primaryArtifact?.slug);
  const documentRoute = address.preset === "conversation" ? artifactRoute : undefined;
  const outlined =
    selectedArtifact !== undefined && treeDocumentIds(issue).includes(selectedArtifact.id);
  const opens = useOpensDocument(address, selectedArtifact, outlined);
  return opens && documentRoute !== undefined
    ? { artifact: selectedArtifact, route: documentRoute }
    : undefined;
}

/** The address Documents' list, or a document opened from it, was opened from: what the
 *  navigation hands over (the location's `outline`, `DocumentsList.tsx`' rows hand over the
 *  list's own), and the issue's own, Conversation, otherwise. Escape on the list returns there
 *  (`escape-order.ts`), and each write that keeps the reader's place - the filter by words, a
 *  version picked - hands it on to the location it writes. */
export function useOutlineAddress(key: string): string {
  return outlineOf(useLocation().state) ?? buildIssuePath({ key, kind: "issue" });
}

/** Documents' list's own address, `/issues/<key>/documents` (part N's R2). */
export function documentsAddress(key: string): string {
  return buildIssuePath({ key, kind: "issue-documents" });
}

/**
 * What takes the focus at the address an opened document's way back returns to, which the
 * location it writes carries in its state as `chip` until the page draws it (`useReturnFocus`):
 * with `row`, the link of `href` in that row - the chip that opened the document, in the row that
 * held it (`ChipRecord`); with no row, the row of Documents' list whose link is `href` - the
 * document's own - and with neither, the list's first row.
 */
export interface ReturnFocus {
  readonly href?: string;
  readonly row?: string;
}

/** Where an opened document's way back goes, and what takes the focus there. */
export interface WayBack {
  readonly focus: ReturnFocus;
  readonly to: string;
}

/** "Back to Documents" (part N's R2): Documents' list, with the focus on the document's row,
 *  from wherever the document was opened. */
export function backToDocuments(key: string, slug: string): WayBack {
  return {
    focus: { href: buildIssuePath({ key, kind: "artifact", slug }) },
    to: documentsAddress(key),
  };
}

/**
 * Escape's way back from the document of `slug` (part N's R1, R2, the plan's ST-2): for a
 * document a chip opened (`chip`, the record of the follow that drew this location), the address
 * the chip was pressed at, the focus on the chip once its row is drawn again; for one Documents'
 * list opened (the location's `outline` is the list's address), the list, the focus on the
 * document's row; and for one opened by its own address - a cold load, a link from elsewhere -
 * the list, the focus on its first row.
 */
export function escapeWayBack(
  key: string,
  slug: string,
  outline: string,
  chip: ChipRecord | undefined
): WayBack {
  if (chip !== undefined) return { focus: { href: chip.href, row: chip.row }, to: chip.address };
  if (outline === documentsAddress(key)) return backToDocuments(key, slug);
  return { focus: {}, to: documentsAddress(key) };
}

/** Follows a way back: a push, as a link's follow is, so the browser's Back returns to the
 *  document; the address it opens carries what takes the focus there (`chip`). */
export function useFollowWayBack(): (way: WayBack) => void {
  const navigate = useNavigate();
  return (way) => navigate(way.to, { state: { chip: way.focus } });
}

/** The location's `chip`, if it carries one (`ReturnFocus`). */
function returnFocusOf(state: unknown): ReturnFocus | undefined {
  if (typeof state !== "object" || state === null || !("chip" in state)) return undefined;
  const chip = state.chip;
  if (typeof chip !== "object" || chip === null) return undefined;
  const href = "href" in chip && typeof chip.href === "string" ? chip.href : undefined;
  const row = "row" in chip && typeof chip.row === "string" ? chip.row : undefined;
  return { ...(href === undefined ? {} : { href }), ...(row === undefined ? {} : { row }) };
}

/** The link `focus` names (`ReturnFocus`), once the page draws what holds it: the chip in its
 *  row, else that row itself, whose words no longer draw the chip; the document's row of
 *  Documents' list, else the list's first row, where the list's first page does not draw it,
 *  else the list itself, which holds no row. Null until the row or the list is drawn. */
function returnTarget({ href, row }: ReturnFocus): HTMLElement | null {
  const link = href === undefined ? undefined : `a[href="${CSS.escape(href)}"]`;
  if (row !== undefined) {
    const held =
      document.querySelector<HTMLElement>(rowSelectorOf(row)) ??
      document.querySelector<HTMLElement>(nodeRowSelectorOf(row));
    if (held === null) return null;
    return (link === undefined ? null : held.querySelector<HTMLElement>(link)) ?? held;
  }
  const list = document.querySelector<HTMLElement>(`[${DOCUMENTS_LIST_ATTRIBUTE}]`);
  if (list === null) return null;
  const rows = `[${DOCUMENT_OPEN_ATTRIBUTE}]`;
  return (
    (link === undefined ? null : list.querySelector<HTMLElement>(`${link}${rows}`)) ??
    list.querySelector<HTMLElement>(rows) ??
    list
  );
}

/** How many frames the way back keeps the focus where it put it, against a landing of the
 *  address it returned to that comes after it: about a third of a second. */
const KEPT_FRAMES = 20;

/**
 * Puts the focus where an opened document's way back says (the location's `chip`,
 * `ReturnFocus`), once the page draws it, as an address's own landing does (dispatch://INFRA-1196
 * decision 413): in the commit that draws the row, or Documents' list, which may come after the
 * reads it waits for. The way back is the reader's own Escape or press, so where the address it
 * returned to lands on a row of its own after that - the row that holds the chip, which its
 * address names - the focus goes back to the link for a few frames (`KEPT_FRAMES`), until the
 * reader acts (`abortOnReaderAct`): from their first key, press, wheel or touch the focus is
 * theirs. Each arrival at such a location places it once, Back and Forward to it included.
 */
export function useReturnFocus(): void {
  const { key, state } = useLocation();
  const focus = returnFocusOf(state);
  const href = focus?.href;
  const row = focus?.row;
  const wanted = focus !== undefined;
  // biome-ignore lint/correctness/useExhaustiveDependencies: `key` starts each arrival's placing again
  useEffect(() => {
    if (!wanted) return;
    const acts = abortOnReaderAct();
    let frame = 0;
    const keep = (link: HTMLElement, left: number) => {
      frame = requestAnimationFrame(() => {
        if (acts.signal.aborted || !link.isConnected) return;
        if (document.activeElement !== link) link.focus();
        if (left > 1) keep(link, left - 1);
      });
    };
    const place = (): boolean => {
      const link = returnTarget({ href, row });
      if (link === null) return false;
      link.focus();
      keep(link, KEPT_FRAMES);
      return true;
    };
    const drawn = new MutationObserver(() => {
      if (acts.signal.aborted || place()) drawn.disconnect();
    });
    if (!place()) drawn.observe(document.body, { childList: true, subtree: true });
    return () => {
      drawn.disconnect();
      cancelAnimationFrame(frame);
      acts.abort();
    };
  }, [href, key, row, wanted]);
}
