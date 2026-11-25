import type { ReactNode } from "react";

/**
 * The data attribute the stage carries while the column of comments on text is drawn, which turns
 * on the container steps of Plan's chart (`PlanFilterBody.tsx`) and no other class of the stage
 * (`widths.test.ts`). Built by t6.
 */
export const TEXT_MARGIN_ATTRIBUTE = "data-text-margin";

/** The attribute of the box that holds a composer of this part, in the column or in a node's
 *  sheet. Built by t8. */
export const TEXT_COMPOSER_ATTRIBUTE = "data-text-composer";

/** The words a person selected, as the control hands them to the column to open its composer on
 *  (R5): the node they are in, by its tree id (`TreeNode.id`), and what the server needs to find
 *  them again. */
export interface TextComposerRequest {
  /** The item's block that holds the selection, or null for a node that has one text. */
  readonly block: string | null;
  /** The tree id of the node the words are in. */
  readonly nodeId: string;
  /** How many matches of `quote` start before the selection in the block's (or node's) text. */
  readonly occurrence: number;
  /** The selected words as plain text reads them. */
  readonly quote: string;
}

/** The composer of this part: the request it is open on, or undefined while none is open. */
export interface TextComposer {
  /** Closes the composer, keeping its draft as `MentionComposer` keeps one. */
  readonly close: () => void;
  /** Opens the composer in the column beside the request's node, or in that node's sheet below
   *  1024 px; the column is drawn for it. */
  readonly open: (request: TextComposerRequest) => void;
  readonly request: TextComposerRequest | undefined;
}

/** The composer's one state, held by the provider that `slots/providers.tsx` mounts around the
 *  page (the column and the control share it). Built by t6 (the state and the column) and t8 (the
 *  control that opens it). Answers a composer that is never open until then. */
export function useTextComposer(): TextComposer {
  return NO_COMPOSER;
}

const NO_COMPOSER: TextComposer = { close: () => {}, open: () => {}, request: undefined };

/**
 * The column at the right of the main column that draws each thread on text beside its node
 * (R2): an `aside` with `role="region"` named by the margin's words (`page-words-margin.ts`),
 * drawn from 1024 px while the view draws a node with a thread or a composer is open, 280 px wide
 * and 320 px from 1280 px. It reads the page's one answer of the view's threads from the
 * provider and takes no property. Built by t6; it draws nothing until then.
 */
export function TextMargin(): ReactNode {
  return null;
}
